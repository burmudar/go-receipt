package server

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

type davFixture struct {
	mu      sync.Mutex
	files   map[string][]byte
	offline atomic.Bool
}

func (d *davFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.offline.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != "fixture-user" || pass != "fixture-password" {
		w.Header().Set("WWW-Authenticate", `Basic realm="receipts"`)
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.Method {
	case "OPTIONS":
		w.Header().Set("DAV", "1, 2")
	case "PROPFIND":
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:">
			<d:response><d:href>/dav/inbox/</d:href><d:propstat>
			<d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
			<d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
		for name, data := range d.files {
			u := url.URL{Path: "/dav/inbox/" + name}
			fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat>
				<d:prop><d:resourcetype/><d:getcontentlength>%d</d:getcontentlength></d:prop>
				<d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
				u.EscapedPath(), len(data))
		}
		fmt.Fprint(w, `</d:multistatus>`)
	case "GET":
		name := strings.TrimPrefix(r.URL.Path, "/dav/inbox/")
		data, ok := d.files[name]
		if !ok || !strings.HasPrefix(r.URL.Path, "/dav/inbox/") {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
func (d *davFixture) set(name string, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[name] = data
}

func TestWebDAVBackgroundAndPreview(t *testing.T) {
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
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	original := []byte("%PDF-1.4\nremote original")
	duplicate := []byte("%PDF-1.4\nlocal copy")
	dav := &davFixture{files: map[string][]byte{"remote 10%.pdf": original, "duplicate.pdf": duplicate}}
	remote := httptest.NewServer(dav)
	defer remote.Close()
	web, err := intake.NewWebDAVInput(remote.URL+"/dav/", "fixture-user", "fixture-password", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local.pdf"), duplicate, 0600); err != nil {
		t.Fatal(err)
	}
	scanner := &testScanner{}
	var logs bytes.Buffer
	public, _ := url.Parse("https://receipts.home.arpa")
	cfg := Config{
		DB:              conn,
		Listen:          "127.0.0.1:0",
		PublicURL:       public,
		Password:        "password",
		RequestTimeout:  time.Second,
		XeroTimeout:     time.Second,
		CheckInterval:   time.Hour,
		ReceiptInterval: 20 * time.Millisecond,
		Source:          intake.MixedSource{WebDAV: web},
		Scanner:         scanner,
		Logger:          slog.New(slog.NewTextHandler(&logs, nil)),
		Discover: func(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
			return intake.DiscoverSources(ctx, dir, web)
		},
	}
	start := func(c Config) (*Server, context.CancelFunc, <-chan error) {
		t.Helper()
		s, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		return s, cancel, done
	}
	waitFor := func(f func([]receipt.Record) bool) []receipt.Record {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			records, err := receipt.NewStore(conn).List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if f(records) {
				return records
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for receipt worker")
		return nil
	}
	s, cancel, done := start(cfg)
	records := waitFor(func(rs []receipt.Record) bool {
		if len(rs) != 2 {
			return false
		}
		for _, r := range rs {
			if r.Status != receipt.Extracted {
				return false
			}
		}
		return true
	})
	var remoteRecord receipt.Record
	for _, r := range records {
		if r.Name == "remote 10%.pdf" {
			remoteRecord = r
		}
	}
	if remoteRecord.SHA256 == "" || scanner.calls.Load() != 2 {
		t.Fatalf("unexpected records: %+v; calls %d", records, scanner.calls.Load())
	}
	// A second copy and periodic rescans must not repeat extraction.
	time.Sleep(70 * time.Millisecond)
	if scanner.calls.Load() != 2 {
		t.Fatalf("duplicate extraction: %d", scanner.calls.Load())
	}
	request := func(path, cookie string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	doc := "/receipts/" + remoteRecord.SHA256 + "/document"
	if w := request(doc, ""); w.Code != http.StatusSeeOther {
		t.Fatalf("document unprotected: %d", w.Code)
	}
	login := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader("password=password"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(login, req)
	cookie := login.Result().Cookies()[0].String()
	if w := request(doc, cookie); w.Code != 200 || w.Body.String() != string(original) || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	// A temporary outage leaves new pending records eligible for automatic recovery.
	dav.offline.Store(true)
	late := []byte("%PDF-1.4\nlate")
	dav.set("late.pdf", late)
	lateURL := remote.URL + "/dav/inbox/late.pdf"
	lateHash, _, err := receipt.NewStore(conn).Import(ctx, receipt.File{Name: "late.pdf", Source: lateURL, Data: late})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(func(rs []receipt.Record) bool {
		for _, r := range rs {
			if r.SHA256 == lateHash {
				return r.Status == receipt.Pending && r.Attempts > 0
			}
		}
		return false
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(request("/", cookie).Body.String(), "Receipt import or processing encountered errors") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(request("/", cookie).Body.String(), "Receipt import or processing encountered errors") {
		t.Fatal("outage not visible")
	}
	dav.offline.Store(false)
	waitFor(func(rs []receipt.Record) bool {
		for _, r := range rs {
			if r.SHA256 == lateHash {
				return r.Status == receipt.Extracted
			}
		}
		return false
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Preview verifies bytes even after the original is replaced or removed.
	dav.set("remote 10%.pdf", []byte("%PDF-1.4\nreplacement"))
	if w := request(doc, cookie); w.Code != http.StatusConflict {
		t.Fatalf("changed preview: %d", w.Code)
	}
	dav.mu.Lock()
	delete(dav.files, "remote 10%.pdf")
	dav.mu.Unlock()
	if w := request(doc, cookie); w.Code != http.StatusNotFound {
		t.Fatalf("missing preview: %d", w.Code)
	}
	// When WebDAV is not configured, existing remote records remain pending.
	dav.set("remote 10%.pdf", original)
	pending := []byte("%PDF-1.4\nunconfigured")
	dav.set("unconfigured.pdf", pending)
	pendingHash, _, err := receipt.NewStore(conn).Import(ctx, receipt.File{
		Name: "unconfigured.pdf", Source: remote.URL + "/dav/inbox/unconfigured.pdf", Data: pending,
	})
	if err != nil {
		t.Fatal(err)
	}
	localCfg := cfg
	localCfg.Source = intake.MixedSource{}
	localCfg.Discover = func(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
		return intake.DiscoverSources(ctx, dir, nil)
	}
	s, cancel, done = start(localCfg)
	waitFor(func(rs []receipt.Record) bool {
		for _, r := range rs {
			if r.SHA256 == pendingHash {
				return r.Status == receipt.Pending && r.Attempts > 0
			}
		}
		return false
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if scanner.calls.Load() != 3 {
		t.Fatalf("unconfigured source was scanned: %d", scanner.calls.Load())
	}
	// Reconfiguring the endpoint resumes pending work; extracted receipts are not repeated.
	s, cancel, done = start(cfg)
	waitFor(func(rs []receipt.Record) bool {
		for _, r := range rs {
			if r.SHA256 == pendingHash {
				return r.Status == receipt.Extracted
			}
		}
		return false
	})
	time.Sleep(120 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if scanner.calls.Load() != 4 {
		t.Fatalf("restart repeated extraction: %d", scanner.calls.Load())
	}
	if strings.Contains(logs.String(), "fixture-password") {
		t.Fatal("WebDAV password leaked in logs")
	}
}

func TestUnconfiguredWebDAVStaysPending(t *testing.T) {
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
	store := receipt.NewStore(conn)
	hash, _, err := store.Import(ctx, receipt.File{Name: "remote.pdf", Source: "https://example.org/dav/inbox/remote.pdf", Data: []byte("%PDF-1.4\nremote")})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &testScanner{}
	_, err = store.Process(ctx, intake.MixedSource{}, scanner, false)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected configuration error: %v", err)
	}
	records, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].SHA256 != hash || records[0].Status != receipt.Pending || scanner.calls.Load() != 0 {
		t.Fatalf("unconfigured source changed: %+v", records)
	}
}
