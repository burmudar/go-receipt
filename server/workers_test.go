package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
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

func TestDurableWorkerControlAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	conn, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close() }()
	p, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("http://localhost")
	cfg := Config{DB: conn, Listen: "127.0.0.1:0", PublicURL: public, RequestTimeout: time.Second, XeroTimeout: time.Second, CheckInterval: time.Hour, JobTimeout: 10 * time.Millisecond}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	failure := func(context.Context) error { calls++; return errors.New("secret remote error") }
	if err = s.runJob(ctx, "test", time.Minute, failure); err == nil {
		t.Fatal("expected failure")
	}
	if err = s.runJob(ctx, "test", time.Minute, failure); err != nil || calls != 1 {
		t.Fatalf("backoff: %v calls=%d", err, calls)
	}
	post := func(action string) {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/processing", strings.NewReader("action="+action))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.ServeHTTP(w, r)
		if w.Code != 303 {
			t.Fatalf("control: %d %s", w.Code, w.Body.String())
		}
	}
	post("pause")
	conn.Close()
	conn, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DB = conn
	s, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var persistedNext int64
	if err = conn.QueryRow(`SELECT next_at FROM service_worker WHERE name='test'`).Scan(&persistedNext); err != nil || persistedNext <= time.Now().UnixMilli() {
		t.Fatalf("backoff lost on restart: %d %v", persistedNext, err)
	}
	if _, err = conn.Exec(`UPDATE service_worker SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.runJob(ctx, "test", time.Minute, failure); err != nil || calls != 1 {
		t.Fatal("pause lost on restart")
	}
	paused, workers, err := s.workerStatus(ctx)
	if err != nil || !paused || len(workers) != 1 || workers[0].Failures != 1 {
		t.Fatalf("health: %+v %v", workers, err)
	}
	if strings.Contains(workers[0].LastError, "secret") {
		t.Fatal("remote error leaked")
	}
	post("resume")
	if err = s.runJob(ctx, "test", time.Minute, func(context.Context) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	_, workers, err = s.workerStatus(ctx)
	if err != nil || workers[0].Failures != 0 || workers[0].SucceededAt == 0 {
		t.Fatalf("recovery: %+v %v", workers, err)
	}
	if err = s.runJob(ctx, "timeout", time.Minute, func(c context.Context) error { <-c.Done(); return c.Err() }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	if err = s.runJob(cancelled, "cancel", time.Minute, func(c context.Context) error { cancel(); return c.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if retryDelay(time.Minute, 100) != time.Hour {
		t.Fatal("unbounded backoff")
	}
	// An SQLite snapshot preserves pause state, receipt records and audit decisions.
	post("pause")
	if _, err = conn.Exec(`INSERT INTO receipt(sha256,name,source) VALUES ('hash','receipt','/retained/receipt'); INSERT INTO receipt_review_event(receipt_sha256,action) VALUES ('hash','unmatched')`); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err = conn.Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := db.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var count int
	if err = restored.QueryRow(`SELECT count(*) FROM receipt_review_event WHERE receipt_sha256='hash'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("restore history: %d %v", count, err)
	}
	if err = restored.QueryRow(`SELECT paused FROM service_control`).Scan(&paused); err != nil || !paused {
		t.Fatal("restore must stay paused")
	}
}

func TestAutomaticExtractionRetryBudget(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
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
	file := filepath.Join(t.TempDir(), "bad.pdf")
	if err = os.WriteFile(file, []byte("bad receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	store := receipt.NewStore(conn)
	hash, _, err := store.Import(ctx, receipt.File{Name: "bad.pdf", Source: file, Data: []byte("bad receipt")})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &testScanner{}
	for i := 0; i < 7; i++ {
		if _, err = conn.Exec(`UPDATE receipt SET updated_at=datetime('now','-2 hours')`); err != nil {
			t.Fatal(err)
		}
		store.ProcessAutomatic(ctx, intake.LocalSource{}, scanner)
		// Immediate repeat must not bypass per-receipt backoff.
		store.ProcessAutomatic(ctx, intake.LocalSource{}, scanner)
	}
	if scanner.calls.Load() != 5 {
		t.Fatalf("attempts=%d", scanner.calls.Load())
	}
	records, err := store.List(ctx)
	if err != nil || records[0].Status != receipt.Failed || records[0].SHA256 != hash {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	// Explicit operator retry remains available after exhaustion.
	store.Process(ctx, intake.LocalSource{}, scanner, true)
	if scanner.calls.Load() != 6 {
		t.Fatal("explicit retry blocked")
	}
	// An interrupted fifth attempt must remain eligible, not consume the last
	// failed-result slot and strand the receipt on restart.
	if _, err = conn.Exec(`UPDATE receipt SET attempts=4,updated_at=datetime('now','-2 hours')`); err != nil {
		t.Fatal(err)
	}
	interrupted, cancel := context.WithCancel(ctx)
	_, err = store.ProcessAutomatic(interrupted, intake.LocalSource{}, scanFunc(func(context.Context, receipt.File) (receipt.Analysis, error) {
		cancel()
		return receipt.Analysis{}, context.Canceled
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption: %v", err)
	}
	records, err = store.List(ctx)
	if err != nil || records[0].Status != receipt.Pending || records[0].Attempts != 5 {
		t.Fatalf("interrupted state: %+v %v", records, err)
	}
	if _, err = conn.Exec(`UPDATE receipt SET updated_at=datetime('now','-2 hours')`); err != nil {
		t.Fatal(err)
	}
	_, err = receipt.NewStore(conn).ProcessAutomatic(ctx, intake.LocalSource{}, scanFunc(func(context.Context, receipt.File) (receipt.Analysis, error) {
		return receipt.Analysis{Merchant: "Shop", Total: "12.34", Currency: "USD", Date: time.Now(), Confidence: .9}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	records, err = store.List(ctx)
	if err != nil || records[0].Status != receipt.Extracted {
		t.Fatalf("recovery: %+v %v", records, err)
	}
}

type scanFunc func(context.Context, receipt.File) (receipt.Analysis, error)

func (f scanFunc) Scan(ctx context.Context, file receipt.File) (receipt.Analysis, error) {
	return f(ctx, file)
}
