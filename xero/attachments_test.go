package xero

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

type attachmentScanner struct{}

func (attachmentScanner) Scan(context.Context, receipt.File) (receipt.Analysis, error) {
	return receipt.Analysis{Merchant: "Shop", Total: "12.00", Currency: "ZAR", Date: time.Date(2025, 9, 30, 0, 0, 0, 0, time.UTC), Confidence: 1}, nil
}

func TestAttachmentWorkflow(t *testing.T) {
	for _, scenario := range []string{"success", "lost response", "retry upload", "changed total", "changed currency", "changed date", "deleted", "voided", "unreconciled", "transfer", "missing", "changed source", "oversize", "collision", "rate limit", "not found"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			conn, err := db.Open(filepath.Join(dir, "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			p, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = p.Up(ctx); err != nil {
				t.Fatal(err)
			}
			exec := func(q string, args ...any) {
				t.Helper()
				if _, e := conn.Exec(q, args...); e != nil {
					t.Fatal(e)
				}
			}
			exec(`INSERT INTO xero_connection(id,tenant_id,tenant_name,access_token,refresh_token,expires_at,status,refreshed_at) VALUES(1,'tenant-a','Home','access','refresh',?,'connected',?)`, time.Now().Add(time.Hour).UTC(), time.Now().UTC())
			exec(`INSERT INTO xero_bank_transaction(tenant_id,bank_transaction_id,type,status,is_reconciled,date,currency_code,total,has_attachments,updated_date_utc,fetched_at) VALUES('tenant-a','target','SPEND','AUTHORISED',1,'2025-09-30','ZAR','12.00',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
			data := []byte("%PDF-1.4 test receipt")
			if scenario == "oversize" {
				data = []byte(strings.Repeat("x", MaxAttachmentBytes+1))
			}
			path := filepath.Join(dir, "receipt.pdf")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			store := receipt.NewStore(conn)
			hash, _, err := store.Import(ctx, receipt.File{Name: "receipt.pdf", Source: path, Data: data})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Process(ctx, intake.LocalSource{}, attachmentScanner{}, false); err != nil {
				t.Fatal(err)
			}
			if err = store.Decide(ctx, hash, "approved", "target"); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing" {
				os.Remove(path)
			}
			if scenario == "changed source" {
				os.WriteFile(path, []byte("replacement"), 0600)
			}
			name := "receipt-" + hash + ".pdf"
			puts := 0
			exists := scenario == "collision"
			keys := []string{}
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("xero-tenant-id") != "tenant-a" {
					t.Error("missing credentials or tenant")
				}
				if scenario == "not found" {
					w.WriteHeader(404)
					return
				}
				if scenario == "rate limit" {
					w.WriteHeader(429)
					return
				}
				switch {
				case r.Method == "PUT":
					puts++
					keys = append(keys, r.Header.Get("Idempotency-Key"))
					if scenario == "retry upload" && puts == 1 {
						w.WriteHeader(503)
						return
					}
					exists = true
					if e := store.Decide(ctx, hash, "needs_review", ""); e == nil {
						t.Error("target unlocked during upload")
					}
					if scenario == "lost response" {
						w.Write([]byte("broken JSON"))
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"Attachments": []attachmentJSON{{"attachment-id", name}}})
				case strings.HasSuffix(r.URL.Path, "/Attachments"):
					a := []attachmentJSON{}
					if exists {
						a = append(a, attachmentJSON{"attachment-id", name})
					}
					json.NewEncoder(w).Encode(map[string]any{"Attachments": a})
				case strings.HasSuffix(r.URL.Path, "/Attachments/attachment-id"):
					if scenario == "collision" {
						w.Write([]byte("different receipt"))
					} else {
						w.Write(data)
					}
				default:
					total, currency, date, status, typ, reconciled := "12.0", "ZAR", "/Date(1759190400000+0000)/", "AUTHORISED", "SPEND", true
					switch scenario {
					case "changed total":
						total = "13"
					case "changed currency":
						currency = "USD"
					case "changed date":
						date = "/Date(1759276800000+0000)/"
					case "deleted":
						status = "DELETED"
					case "voided":
						status = "VOIDED"
					case "unreconciled":
						reconciled = false
					case "transfer":
						typ = "SPEND-TRANSFER"
					}
					fmt.Fprintf(w, `{"BankTransactions":[{"BankTransactionID":"target","Type":%q,"Status":%q,"IsReconciled":%t,"Total":%s,"CurrencyCode":%q,"Date":%q}]}`, typ, status, reconciled, total, currency, date)
				}
			}))
			defer fake.Close()
			newManager := func() *Manager {
				m, e := NewManager(conn, Config{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://localhost/callback", Endpoints: Endpoints{AccountingURL: fake.URL}, HTTPClient: fake.Client()})
				if e != nil {
					t.Fatal(e)
				}
				return m
			}
			m := newManager()
			err = m.ProcessAttachments(ctx, intake.LocalSource{})
			success := scenario == "success" || scenario == "lost response" || scenario == "retry upload"
			if scenario == "success" && err != nil {
				t.Fatal(err)
			}
			if scenario != "success" && err == nil {
				t.Fatal("expected failure")
			}
			if success {
				// New Manager simulates restart and must recover uncertainty without a second PUT.
				if err = newManager().ProcessAttachments(ctx, intake.LocalSource{}); err != nil {
					t.Fatal(err)
				}
				a, e := store.Attachments(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if a[hash].State != "confirmed" || a[hash].ID != "attachment-id" || a[hash].CompletedAt == "" {
					t.Fatalf("missing durable confirmation: %+v", a)
				}
				wantPuts := 1
				if scenario == "retry upload" {
					wantPuts = 2
					if len(keys) != 2 || keys[0] != keys[1] {
						t.Fatalf("unstable retry keys: %v", keys)
					}
				}
				if puts != wantPuts || len(keys[0]) != 64 {
					t.Fatalf("puts=%d keys=%v", puts, keys)
				}
				if e = store.Decide(ctx, hash, "needs_review", ""); e == nil {
					t.Fatal("confirmed target unlocked")
				}
			} else {
				if puts != 0 {
					t.Fatalf("unsafe upload: %d", puts)
				}
			}
			if strings.HasPrefix(scenario, "changed ") && scenario != "changed source" || scenario == "deleted" || scenario == "voided" || scenario == "unreconciled" || scenario == "transfer" || scenario == "not found" {
				reviews, e := store.Reviews(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if reviews[hash].State != "needs_review" {
					t.Fatal("target not returned to review")
				}
			}
			if scenario != "missing" {
				if _, e := os.Stat(path); e != nil {
					t.Fatal("intake removed", e)
				}
			}
		})
	}
}
