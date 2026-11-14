package server

import (
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/cmd/scancli"
	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

type testScanner struct{ calls atomic.Int32 }

func (s *testScanner) Scan(ctx context.Context, file receipt.File) (receipt.Analysis, error) {
	s.calls.Add(1)
	if file.Name == "bad.pdf" {
		return receipt.Analysis{}, fmt.Errorf("extraction failed")
	}
	return receipt.Analysis{Merchant: "Shop", Total: "12.34", Currency: "USD", Date: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC), Confidence: .9}, nil
}

func TestBackgroundReceiptsAndPages(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	original := []byte("%PDF-1.4\nexample receipt")
	if err := os.WriteFile(filepath.Join(dir, "good.pdf"), original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.pdf"), []byte("%PDF-1.4\nbad"), 0600); err != nil {
		t.Fatal(err)
	}
	scanner := &testScanner{}
	public, _ := url.Parse("https://receipts.home.arpa")
	cfg := Config{DB: conn, Listen: "127.0.0.1:0", PublicURL: public, Password: "password", RequestTimeout: time.Second, XeroTimeout: time.Second, CheckInterval: time.Hour, ReceiptInterval: 20 * time.Millisecond, Source: scancli.LocalSource{}, Scanner: scanner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Discover: func(ctx context.Context) (iter.Seq2[receipt.File, error], error) { return scancli.LocalFiles(ctx, dir) }}
	start := func() (*Server, context.CancelFunc, <-chan error) {
		s, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		return s, cancel, done
	}
	wait := func(count int) []receipt.Record {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			records, err := receipt.NewStore(conn).List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) == count {
				ready := true
				for _, r := range records {
					if r.Status == receipt.Pending {
						ready = false
					}
				}
				if ready {
					return records
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("worker did not finish")
		return nil
	}
	s, cancel, done := start()
	records := wait(2)
	var good, bad receipt.Record
	for _, r := range records {
		if r.Status == receipt.Extracted {
			good = r
		} else {
			bad = r
		}
	}
	if good.Analysis == nil || bad.LastError != "extraction failed" {
		t.Fatalf("unexpected records: %+v", records)
	}
	// Files arriving after startup are picked up on a later pass.
	if err := os.WriteFile(filepath.Join(dir, "new.pdf"), []byte("%PDF-1.4\nnew"), 0600); err != nil {
		t.Fatal(err)
	}
	wait(3)
	request := func(path, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	if w := request("/receipts/"+good.SHA256+"/document", ""); w.Code != http.StatusSeeOther {
		t.Fatalf("document not protected: %d", w.Code)
	}
	login := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader("password=password"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(login, req)
	cookie := login.Result().Cookies()[0].String()
	for path, text := range map[string]string{"/receipts": "extraction failed", "/receipts/" + good.SHA256: "Shop", "/receipts/" + bad.SHA256: "extraction failed", "/receipts/" + good.SHA256 + "/document": string(original)} {
		w := request(path, cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), text) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.pdf"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := request("/receipts/"+good.SHA256+"/document", cookie); w.Code != http.StatusConflict {
		t.Fatalf("changed source: %d", w.Code)
	}
	// Remove the changed file to avoid importing it as another receipt.
	if err := os.Remove(filepath.Join(dir, "good.pdf")); err != nil {
		t.Fatal(err)
	}
	s, cancel, done = start()
	_ = s
	wait(3)
	time.Sleep(80 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if scanner.calls.Load() != 3 {
		t.Fatalf("completed and failed scans repeated: %d", scanner.calls.Load())
	}
}
