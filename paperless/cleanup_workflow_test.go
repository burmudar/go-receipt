package paperless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/xero"
	"github.com/pressly/goose/v3"
)

type cleanupWorkflowScanner struct{}

func (cleanupWorkflowScanner) Scan(context.Context, receipt.File) (receipt.Analysis, error) {
	return receipt.Analysis{Merchant: "Corrected shop", Total: "12.30", Currency: "ZAR", Date: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Confidence: 1}, nil
}

// TestCleanupFullWorkflow uses the real import, extraction, review and delivery
// managers. Only the connected account and transaction cache are seeded; neither
// delivery confirmation is fabricated. All files and HTTP endpoints are disposable.
func TestCleanupFullWorkflow(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dir, "workflow.db")
	conn, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close() }()
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, `INSERT INTO xero_connection(id,tenant_id,tenant_name,access_token,refresh_token,expires_at,status,refreshed_at) VALUES(1,'tenant','Home','access','refresh',?,'connected',?)`, time.Now().Add(time.Hour).UTC(), time.Now().UTC())
	mustExec(t, conn, `INSERT INTO xero_bank_transaction(tenant_id,bank_transaction_id,type,status,is_reconciled,date,currency_code,total,has_attachments,updated_date_utc,fetched_at) VALUES('tenant','target','SPEND','AUTHORISED',1,'2026-10-01','ZAR','12.30',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	inbox := filepath.Join(dir, "inbox")
	if err = os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	original := syntheticPDF()
	source := filepath.Join(inbox, "receipt.pdf")
	if err = os.WriteFile(source, original, 0600); err != nil {
		t.Fatal(err)
	}
	store := receipt.NewStore(conn)
	files, err := intake.LocalFiles(ctx, inbox)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	for file, e := range files {
		if e != nil {
			t.Fatal(e)
		}
		var added bool
		hash, added, err = store.Import(ctx, file)
		if err != nil || !added {
			t.Fatal("import", err)
		}
	}
	if hash == "" {
		t.Fatal("receipt not discovered")
	}
	retained := func() {
		t.Helper()
		data, e := os.ReadFile(source)
		if e != nil || string(data) != string(original) {
			t.Fatalf("intake not retained: %v", e)
		}
	}
	cleanup := func() {
		t.Helper()
		if e := store.ProcessCleanup(ctx, intake.LocalSource{}); e != nil {
			t.Fatal(e)
		}
	}
	cleanup()
	retained()
	if _, err = store.Process(ctx, intake.LocalSource{}, cleanupWorkflowScanner{}, false); err != nil {
		t.Fatal(err)
	}
	cleanup()
	retained()
	if err = store.Decide(ctx, hash, "approved", "target"); err != nil {
		t.Fatal(err)
	}
	cleanup()
	retained()
	name := "receipt-" + hash + ".pdf"
	var puts atomic.Int32
	var attachmentExists atomic.Bool
	xeroAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("xero-tenant-id") != "tenant" {
			t.Error("missing Xero credentials")
		}
		attachment := map[string]string{"AttachmentID": "attachment-id", "FileName": name}
		switch {
		case r.Method == http.MethodPut:
			puts.Add(1)
			data, e := io.ReadAll(r.Body)
			if e != nil || string(data) != string(original) {
				t.Error("Xero received wrong original")
			}
			attachmentExists.Store(true)
			json.NewEncoder(w).Encode(map[string]any{"Attachments": []any{attachment}})
		case strings.HasSuffix(r.URL.Path, "/Attachments"):
			attachments := []any{}
			if attachmentExists.Load() {
				attachments = append(attachments, attachment)
			}
			json.NewEncoder(w).Encode(map[string]any{"Attachments": attachments})
		case strings.HasSuffix(r.URL.Path, "/Attachments/attachment-id"):
			w.Write(original)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/BankTransactions/target"):
			fmt.Fprintf(w, `{"BankTransactions":[{"BankTransactionID":"target","Type":"SPEND","Status":"AUTHORISED","IsReconciled":true,"Total":12.30,"CurrencyCode":"ZAR","Date":"/Date(%d+0000)/"}]}`, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
		default:
			t.Errorf("unexpected Xero request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer xeroAPI.Close()
	newXero := func() *xero.Manager {
		t.Helper()
		m, e := xero.NewManager(conn, xero.Config{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://localhost/callback", Endpoints: xero.Endpoints{AccountingURL: xeroAPI.URL}, HTTPClient: xeroAPI.Client()})
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	if err = newXero().ProcessAttachments(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	attachments, err := store.Attachments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attached := attachments[hash]
	if attached.State != "confirmed" || attached.ID != "attachment-id" || attached.CompletedAt == "" || !attached.Locked {
		t.Fatalf("attachment not confirmed: %+v", attached)
	}
	cleanup()
	retained()
	fake := &fakePaperless{t: t, data: original, taskStatus: "STARTED"}
	paperlessAPI := httptest.NewServer(fake)
	defer paperlessAPI.Close()
	cfg := Config{URL: paperlessAPI.URL, Token: "private-token", Tags: []int{7}, Timeout: time.Second}
	if err = manager(t, conn, cfg).Process(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	if a := state(t, conn, hash); a.State != "consuming" || a.TaskID != "task-1" || a.DocumentID != 0 {
		t.Fatalf("premature archive confirmation: %+v", a)
	}
	cleanup()
	retained()
	fake.visible = true
	if err = manager(t, conn, cfg).Process(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	if state(t, conn, hash).State != "consuming" {
		t.Fatal("visible document treated as consumed")
	}
	cleanup()
	retained()
	restart := func() {
		t.Helper()
		if e := conn.Close(); e != nil {
			t.Fatal(e)
		}
		conn, err = db.Open(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		store = receipt.NewStore(conn)
	}
	restart()
	if err = newXero().ProcessAttachments(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	fake.taskStatus = "SUCCESS"
	if err = manager(t, conn, cfg).Process(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	archived := state(t, conn, hash)
	if archived.State != "confirmed" || archived.DocumentID != 42 || archived.CompletedAt == "" {
		t.Fatalf("archive not confirmed: %+v", archived)
	}
	retained()
	// Verification runs after eligibility selection. Review changes attempted
	// during this interval are rejected by the durable attachment target lock.
	lockedSource := reviewLockedCleanup{t: t, store: store, hash: hash}
	if err = store.ProcessCleanup(ctx, lockedSource); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("intake not removed", err)
	}
	cleanups, err := store.Cleanups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completed := cleanups[hash]
	if completed.State != "confirmed" || completed.CompletedAt == "" || completed.Attempts != 1 {
		t.Fatalf("not complete: %+v", completed)
	}
	// A reused path after completion must survive a process/database restart.
	if err = os.WriteFile(source, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	restart()
	if err = newXero().ProcessAttachments(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	if err = manager(t, conn, cfg).Process(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if data, e := os.ReadFile(source); e != nil || string(data) != "replacement" {
		t.Fatal("restart deleted replacement", e)
	}
	after, err := store.Cleanups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after[hash] != completed {
		t.Fatal("cleanup repeated")
	}
	attachments, err = store.Attachments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attachments[hash] != attached || state(t, conn, hash) != archived {
		t.Fatal("remote confirmations changed")
	}
	if puts.Load() != 1 || fake.uploads != 1 || fake.patches != 1 || len(fake.notes) != 1 {
		t.Fatalf("remote operations repeated: Xero=%d Paperless uploads=%d patches=%d notes=%d", puts.Load(), fake.uploads, fake.patches, len(fake.notes))
	}
}

type reviewLockedCleanup struct {
	t     *testing.T
	store *receipt.Store
	hash  string
}

func (s reviewLockedCleanup) Cleanup(ctx context.Context, path, hash string, beforeDelete func() error) error {
	return (intake.LocalSource{}).Cleanup(ctx, path, hash, func() error {
		for _, action := range []string{"needs_review", "ignored", "unmatched"} {
			if err := s.store.Decide(ctx, s.hash, action, ""); !errors.Is(err, receipt.ErrReviewConflict) {
				s.t.Errorf("expected locked review conflict during cleanup for %s: %v", action, err)
			}
		}
		return beforeDelete()
	})
}
