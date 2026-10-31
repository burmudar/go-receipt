package paperless

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

type fileSource struct{}

func (fileSource) Read(_ context.Context, path string) ([]byte, error) { return os.ReadFile(path) }

type fakePaperless struct {
	t                                               *testing.T
	data                                            []byte
	uploads, patches                                int
	visible, uncertain, wrongOriginal, patchFailure bool
	taskStatus                                      string
	notes                                           []map[string]string
}

func (f *fakePaperless) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Token private-token" {
		f.t.Error("missing authentication")
	}
	switch r.URL.Path {
	case "/api/documents/":
		if got := r.URL.Query().Get("checksum__iexact"); got != fmt.Sprintf("%x", md5.Sum(f.data)) {
			f.t.Errorf("checksum %s", got)
		}
		if f.visible {
			io.WriteString(w, `{"count":1,"results":[{"id":42}]}`)
		} else {
			io.WriteString(w, `{"count":0,"results":[]}`)
		}
	case "/api/documents/post_document/":
		f.uploads++
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			f.t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("document")
		if err != nil {
			f.t.Error(err)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if string(data) != string(f.data) {
			f.t.Error("wrong uploaded original")
		}
		if r.FormValue("title") != "Corrected shop — 12.30 ZAR" || r.FormValue("created") != "2026-10-01" || r.FormValue("tags") != "7" {
			f.t.Errorf("incorrect metadata: %v", r.MultipartForm.Value)
		}
		if f.uncertain {
			io.WriteString(w, `invalid response`)
		} else {
			io.WriteString(w, `"task-1"`)
		}
	case "/api/tasks/":
		if r.URL.Query().Get("task_id") != "task-1" {
			f.t.Error("wrong task")
		}
		json.NewEncoder(w).Encode([]map[string]any{{"task_id": "task-1", "status": f.taskStatus, "related_document": "42"}})
	case "/api/documents/42/download/":
		if r.URL.Query().Get("original") != "true" {
			f.t.Error("not original download")
		}
		if f.wrongOriginal {
			io.WriteString(w, "wrong bytes")
		} else {
			w.Write(f.data)
		}
	case "/api/documents/42/":
		if r.Method == "PATCH" {
			f.patches++
			if f.patchFailure {
				http.Error(w, "private-token", 500)
				return
			}
			var metadata struct {
				Title string
				Tags  []int
			}
			json.NewDecoder(r.Body).Decode(&metadata)
			if metadata.Title != "Corrected shop — 12.30 ZAR" || len(metadata.Tags) != 2 {
				f.t.Errorf("bad patch %+v", metadata)
			}
		}
		io.WriteString(w, `{"tags":[99]}`)
	case "/api/documents/42/notes/":
		if r.Method == "POST" {
			var note map[string]string
			json.NewDecoder(r.Body).Decode(&note)
			if !strings.Contains(note["note"], "Bank transaction: target") {
				f.t.Error("missing transaction reference")
			}
			f.notes = append(f.notes, note)
		}
		if f.notes == nil {
			io.WriteString(w, `[]`)
		} else {
			json.NewEncoder(w).Encode(f.notes)
		}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}
func setup(t *testing.T) (*sql.DB, string, string, *fakePaperless, Config) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")
	conn, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	data := syntheticPDF()
	source := filepath.Join(dir, "receipt.pdf")
	if err = os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	file := receipt.File{Name: "receipt.pdf", Source: source, Data: data}
	store := receipt.NewStore(conn)
	if _, _, err = store.Import(ctx, file); err != nil {
		t.Fatal(err)
	}
	original := receipt.Analysis{Merchant: "Original shop", Total: "10", Currency: "ZAR", Date: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Confidence: 1}
	analysis, _ := json.Marshal(original)
	mustExec(t, conn, `UPDATE receipt SET status='extracted',extracted_at=CURRENT_TIMESTAMP,analysis=? WHERE sha256=?`, string(analysis), file.SHA256())
	original.Merchant = "Corrected shop"
	original.Total = "12.30"
	analysis, _ = json.Marshal(original)
	mustExec(t, conn, `INSERT INTO receipt_review(receipt_sha256,state,corrected_analysis) VALUES (?,'needs_review',?)`, file.SHA256(), string(analysis))
	mustExec(t, conn, `INSERT INTO receipt_attachment(receipt_sha256,tenant_id,transaction_id,total,currency,date,state,locked,attachment_id) VALUES (?,'tenant','target','12.30','ZAR','2026-10-01','confirmed',1,'xero-attachment')`, file.SHA256())
	fake := &fakePaperless{t: t, data: data, taskStatus: "STARTED"}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return conn, path, file.SHA256(), fake, Config{URL: srv.URL, Token: "private-token", Tags: []int{7}, Timeout: time.Second}
}
func mustExec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func state(t *testing.T, conn *sql.DB, hash string) receipt.Archive {
	t.Helper()
	a, err := receipt.NewStore(conn).Archives(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a[hash]
}
func manager(t *testing.T, conn *sql.DB, cfg Config) *Manager {
	t.Helper()
	m, err := New(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestArchiveConsumptionAndRestart(t *testing.T) {
	conn, path, hash, f, cfg := setup(t)
	ctx := context.Background()
	m := manager(t, conn, cfg)
	if err := m.Process(ctx, fileSource{}); err != nil {
		t.Fatal(err)
	}
	if a := state(t, conn, hash); a.State != "consuming" || a.TaskID != "task-1" || a.DocumentID != 0 {
		t.Fatalf("acceptance confirmed prematurely: %+v", a)
	}
	f.visible = true // A document may appear before its task finishes.
	if err := m.Process(ctx, fileSource{}); err != nil {
		t.Fatal(err)
	}
	if state(t, conn, hash).State != "consuming" {
		t.Fatal("confirmed an active task")
	}
	f.visible = false
	conn.Close()
	conn, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	m = manager(t, conn, cfg)
	f.taskStatus = "SUCCESS"
	if err = m.Process(ctx, fileSource{}); err != nil {
		t.Fatal(err)
	}
	if a := state(t, conn, hash); a.State != "confirmed" || a.DocumentID != 42 || a.CompletedAt == "" {
		t.Fatalf("not archived: %+v", a)
	}
	if err = m.Process(ctx, fileSource{}); err != nil {
		t.Fatal(err)
	}
	if f.uploads != 1 || f.patches != 1 || len(f.notes) != 1 {
		t.Fatalf("repeated side effects: %+v", f)
	}
	var source, attachment string
	conn.QueryRow(`SELECT source FROM receipt WHERE sha256=?`, hash).Scan(&source)
	if _, err = os.Stat(source); err != nil {
		t.Fatal("original removed", err)
	}
	conn.QueryRow(`SELECT attachment_id FROM receipt_attachment WHERE receipt_sha256=?`, hash).Scan(&attachment)
	if attachment != "xero-attachment" {
		t.Fatal("attachment changed")
	}
}
func TestUncertainUploadNeverResubmitted(t *testing.T) {
	conn, _, hash, f, cfg := setup(t)
	ctx := context.Background()
	f.uncertain = true
	if err := manager(t, conn, cfg).Process(ctx, fileSource{}); err == nil {
		t.Fatal("expected uncertain upload")
	}
	if err := manager(t, conn, cfg).Process(ctx, fileSource{}); err == nil {
		t.Fatal("expected blocked resubmission")
	}
	if f.uploads != 1 {
		t.Fatal("duplicate upload")
	}
	f.visible = true
	f.patchFailure = true
	if err := manager(t, conn, cfg).Process(ctx, nil); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("expected sanitized metadata failure", err)
	}
	f.patchFailure = false
	if err := manager(t, conn, cfg).Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if state(t, conn, hash).State != "confirmed" || f.uploads != 1 {
		t.Fatal("did not recover independently of intake")
	}
}
func TestArchiveSafeguards(t *testing.T) {
	for _, scenario := range []string{"not-attached", "changed", "missing", "checksum-collision", "task-failed", "endpoint-changed"} {
		t.Run(scenario, func(t *testing.T) {
			conn, _, hash, f, cfg := setup(t)
			ctx := context.Background()
			m := manager(t, conn, cfg)
			var source string
			conn.QueryRow(`SELECT source FROM receipt WHERE sha256=?`, hash).Scan(&source)
			switch scenario {
			case "not-attached":
				mustExec(t, conn, `UPDATE receipt_attachment SET state='pending'`)
			case "changed":
				os.WriteFile(source, []byte("changed"), 0600)
			case "missing":
				os.Remove(source)
			case "checksum-collision":
				f.visible = true
				f.wrongOriginal = true
			case "task-failed":
				if err := m.Process(ctx, fileSource{}); err != nil {
					t.Fatal(err)
				}
				f.taskStatus = "FAILURE"
			case "endpoint-changed":
				if err := m.Process(ctx, fileSource{}); err != nil {
					t.Fatal(err)
				}
				mustExec(t, conn, `UPDATE receipt_archive SET endpoint='https://old.example'`)
			}
			err := m.Process(ctx, fileSource{})
			if scenario == "not-attached" {
				if err != nil || state(t, conn, hash).State != "" {
					t.Fatal("archived before attachment", err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if state(t, conn, hash).State == "confirmed" {
				t.Fatal("unsafe confirmation")
			}
			expected := 0
			if scenario == "task-failed" || scenario == "endpoint-changed" {
				expected = 1
			}
			if f.uploads != expected {
				t.Fatalf("uploads %d", f.uploads)
			}
		})
	}
}
func TestRejectRedirectAndInvalidConfiguration(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer origin.Close()
	cfg := Config{URL: origin.URL, Token: "private-token", Tags: []int{1}, Timeout: time.Second}
	m := manager(t, nil, cfg)
	if _, err := m.request(context.Background(), "GET", "documents/", "", nil); err == nil || leaked {
		t.Fatal("followed redirect")
	}
	for _, u := range []string{"https://user:secret@host", "https://host?secret=token", "file:///tmp/a"} {
		cfg.URL = u
		if _, err := New(nil, cfg); err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
}
