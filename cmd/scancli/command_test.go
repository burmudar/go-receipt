package scancli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

// Exercise the public CLI with real SQLite and a controlled extraction endpoint.
// Each invocation opens a new connection, as it would after a process restart.
func TestScanPersistsAndRetriesWithoutSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database := filepath.Join(dir, "receipts.db")
	inbox := filepath.Join(dir, "inbox")
	if err := os.Mkdir(inbox, 0700); err != nil {
		t.Fatal(err)
	}

	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}

	originals := map[string][]byte{
		"receipt.png": png,
		"receipt.pdf": []byte("%PDF-1.4\ncontrolled receipt\n%%EOF"),
	}
	for name, data := range originals {
		if err := os.WriteFile(filepath.Join(inbox, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}

	conn, err := db.Open(database)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	var images atomic.Int32
	var pdfs atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []struct {
				Content []struct {
					Type     string `json:"type"`
					ImageURL string `json:"image_url"`
					FileData string `json:"file_data"`
					FileID   string `json:"file_id"`
				} `json:"content"`
			} `json:"input"`
			Text struct {
				Format struct {
					Schema struct {
						Properties map[string]struct {
							Type string `json:"type"`
						} `json:"properties"`
					} `json:"schema"`
				} `json:"format"`
			} `json:"text"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", 400)
			return
		}

		properties := body.Text.Format.Schema.Properties
		if properties["total"].Type != "string" || properties["confidence"].Type != "number" {
			t.Error("request must use an exact total and the confidence field")
		}

		if len(body.Input) != 1 || len(body.Input[0].Content) != 1 {
			t.Error("expected one receipt input")
			http.Error(w, "invalid input", 400)
			return
		}

		content := body.Input[0].Content[0]
		currency := "ZAR"
		switch content.Type {
		case "input_image":
			images.Add(1)
			if !strings.HasPrefix(content.ImageURL, "data:image/png;base64,") || content.FileID != "" {
				t.Error("image input must use a data URL without a file ID")
			}
		case "input_file":
			if !strings.HasPrefix(content.FileData, "data:application/pdf;base64,") {
				t.Error("PDF input must use a PDF data URL")
			}
			if pdfs.Add(1) == 1 {
				currency = "invalid"
			}
		default:
			t.Errorf("unexpected input type %q", content.Type)
		}

		analysis := fmt.Sprintf(`{
			"merchant": "Shop",
			"total": "9007199254740993.01",
			"currency": %q,
			"date": "2026-10-01T00:00:00Z",
			"confidence": 0.95
		}`, currency)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_test",
			"object": "response",
			"status": "completed",
			"output": []any{
				map[string]any{
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": analysis,
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	t.Setenv("OPENAI_BASE_URL", server.URL+"/")
	t.Setenv("OPENAI_API_KEY", "controlled-test-key")

	run := func(args ...string) (string, error) {
		t.Helper()

		cmd := Cmd()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(append([]string{"--db", database}, args...))

		err := cmd.ExecuteContext(ctx)
		return output.String(), err
	}

	list := func() map[string]receipt.Record {
		t.Helper()

		output, err := run("list")
		if err != nil {
			t.Fatal(err)
		}

		var records []receipt.Record
		if err := json.Unmarshal([]byte(output), &records); err != nil {
			t.Fatal(err)
		}
		if len(records) != 2 {
			t.Fatalf("got %d receipts, want 2", len(records))
		}

		byName := map[string]receipt.Record{}
		for _, r := range records {
			byName[r.Name] = r
		}

		return byName
	}

	if output, err := run("--dir", inbox, "--import-only"); err != nil {
		t.Fatalf("import: %s: %v", output, err)
	}
	for _, r := range list() {
		if r.Status != receipt.Pending || r.Attempts != 0 || r.CreatedAt.IsZero() || r.Source == "" {
			t.Fatalf("import did not persist a pending receipt: %+v", r)
		}
	}
	if images.Load()+pdfs.Load() != 0 {
		t.Fatal("import-only contacted extraction endpoint")
	}

	if _, err := run("--dir", inbox); err == nil {
		t.Fatal("invalid extraction should fail the command")
	}
	records := list()
	if r := records["receipt.pdf"]; r.Status != receipt.Failed ||
		r.LastError == "" || r.Attempts != 1 || r.Analysis != nil {
		t.Fatalf("invalid extraction was not saved as a failure: %+v", r)
	}
	if r := records["receipt.png"]; r.Status != receipt.Extracted ||
		r.Analysis == nil || r.Analysis.Total != "9007199254740993.01" || r.ExtractedAt == nil {
		t.Fatalf("successful extraction was not preserved exactly: %+v", r)
	}

	if output, err := run("--dir", inbox); err != nil {
		t.Fatalf("duplicate scan: %s: %v", output, err)
	}
	if images.Load() != 1 || pdfs.Load() != 1 {
		t.Fatal("duplicate scan repeated extraction without an explicit retry")
	}

	if err := os.RemoveAll(inbox); err != nil {
		t.Fatal(err)
	}
	if output, err := run("retry"); err != nil {
		t.Fatalf("retry without source: %s: %v", output, err)
	}
	if images.Load() != 1 || pdfs.Load() != 2 {
		t.Fatal("retry must extract only the failed receipt")
	}

	conn, err = db.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	store := receipt.NewStore(conn)
	for name, r := range list() {
		if r.SHA256 != records[name].SHA256 ||
			r.Status != receipt.Extracted ||
			r.LastError != "" ||
			r.ExtractedAt == nil ||
			r.Analysis.Total != "9007199254740993.01" {
			t.Fatalf("retry did not preserve a successful extraction: %+v", r)
		}

		file, err := store.Original(ctx, r.SHA256)
		if err != nil || !bytes.Equal(file.Data, originals[name]) {
			t.Fatalf("original not preserved for %s: %v", name, err)
		}

		// A second transport uses the same hash identity and must not reset state.
		file.Source = "https://example.invalid/inbox/" + name
		if _, added, err := store.Import(ctx, file); err != nil || added {
			t.Fatalf("same content from another source was not deduplicated: added=%v, err=%v", added, err)
		}
	}
}
