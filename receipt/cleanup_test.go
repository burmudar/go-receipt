package receipt_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cleanupFixture(t *testing.T) (*sql.DB, *receipt.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	p, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "receipt.pdf")
	if err = os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	store := receipt.NewStore(conn)
	hash, _, err := store.Import(ctx, receipt.File{Name: "receipt.pdf", Source: path, Data: []byte("original")})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO xero_bank_transaction(tenant_id,bank_transaction_id,type,status,is_reconciled,date,currency_code,total,has_attachments,updated_date_utc,fetched_at) VALUES ('tenant','target','SPEND','AUTHORISED',1,'2026-01-01','USD','1',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	exec(`UPDATE receipt SET status='extracted',analysis='{}',extracted_at=CURRENT_TIMESTAMP WHERE sha256=?`, hash)
	exec(`INSERT INTO receipt_review(receipt_sha256,state,tenant_id,bank_transaction_id) VALUES (?,'approved','tenant','target')`, hash)
	exec(`UPDATE receipt_attachment SET state='confirmed',attachment_id='attachment',completed_at='now' WHERE receipt_sha256=?`, hash)
	exec(`INSERT INTO receipt_archive(receipt_sha256,endpoint,state,document_id,completed_at) VALUES (?,'test','confirmed',42,'now')`, hash)
	return conn, store, hash, path
}

type uncertainSource struct{ calls int }

func (s *uncertainSource) Cleanup(ctx context.Context, path, hash string, dispatch func() error) error {
	s.calls++
	if err := dispatch(); err != nil {
		return err
	}
	return errors.New("lost reply")
}
func TestCleanupLifecycle(t *testing.T) {
	for _, scenario := range []string{"success", "mismatch", "missing", "ignored", "unmatched", "pending", "unattached", "unarchived", "accepted-only", "attachment-no-id", "archive-no-document", "attachment-no-time", "archive-no-time", "uncertain", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			conn, store, hash, path := cleanupFixture(t)
			ctx := context.Background()
			var source receipt.CleanupSource = intake.LocalSource{}
			exec := func(q string, args ...any) {
				if _, err := conn.Exec(q, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "mismatch":
				os.WriteFile(path, []byte("replacement"), 0600)
			case "missing":
				os.Remove(path)
			case "ignored", "unmatched":
				exec(`UPDATE receipt_review SET state=? WHERE receipt_sha256=?`, scenario, hash)
			case "pending":
				exec(`UPDATE receipt SET status='pending',analysis=NULL,extracted_at=NULL WHERE sha256=?`, hash)
			case "unattached":
				exec(`UPDATE receipt_attachment SET state='pending' WHERE receipt_sha256=?`, hash)
			case "unarchived":
				exec(`UPDATE receipt_archive SET state='pending' WHERE receipt_sha256=?`, hash)
			case "attachment-no-id":
				exec(`UPDATE receipt_attachment SET attachment_id='' WHERE receipt_sha256=?`, hash)
			case "archive-no-document":
				exec(`UPDATE receipt_archive SET document_id=0 WHERE receipt_sha256=?`, hash)
			case "attachment-no-time":
				exec(`UPDATE receipt_attachment SET completed_at='' WHERE receipt_sha256=?`, hash)
			case "archive-no-time":
				exec(`UPDATE receipt_archive SET completed_at='' WHERE receipt_sha256=?`, hash)
			case "accepted-only":
				exec(`UPDATE receipt_archive SET state='consuming',document_id=0,completed_at='' WHERE receipt_sha256=?`, hash)
			case "uncertain":
				source = &uncertainSource{}
			case "restart":
				exec(`INSERT INTO receipt_cleanup(receipt_sha256,state) VALUES (?,'deleting')`, hash)
			}
			for i := 0; i < 2; i++ {
				if err := receipt.NewStore(conn).ProcessCleanup(ctx, source); err != nil {
					t.Fatal(err)
				}
			}
			states, err := store.Cleanups(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := states[hash]
			want := ""
			switch scenario {
			case "success", "missing", "restart":
				want = "confirmed"
			case "mismatch", "uncertain":
				want = "failed"
			}
			if got.State != want {
				t.Fatalf("state %q want %q", got.State, want)
			}
			if scenario == "success" || scenario == "missing" || scenario == "restart" {
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("not removed")
				}
				if got.Attempts != 1 || got.CompletedAt == "" {
					t.Fatal(got)
				}
			} else if scenario != "missing" {
				if _, err = os.Stat(path); err != nil {
					t.Fatal("source lost", err)
				}
			}
			if s, ok := source.(*uncertainSource); ok && s.calls != 2 {
				t.Fatal("failed deletion must be retried")
			}
			if scenario == "mismatch" {
				if !strings.Contains(got.Error, "intake content changed") {
					t.Fatalf("mismatch error must be visible: %q", got.Error)
				}
				os.WriteFile(path, []byte("original"), 0600)
				if err = store.ProcessCleanup(ctx, source); err != nil {
					t.Fatal(err)
				}
				states, _ = store.Cleanups(ctx)
				if states[hash].State != "confirmed" {
					t.Fatal("verification failure not retried")
				}
			}
			var a, p string
			conn.QueryRow(`SELECT state FROM receipt_attachment WHERE receipt_sha256=?`, hash).Scan(&a)
			conn.QueryRow(`SELECT state FROM receipt_archive WHERE receipt_sha256=?`, hash).Scan(&p)
			if scenario != "unattached" && a != "confirmed" {
				t.Fatal("attachment altered")
			}
			if scenario != "unarchived" && scenario != "accepted-only" && p != "confirmed" {
				t.Fatal("archive altered")
			}
		})
	}
}
