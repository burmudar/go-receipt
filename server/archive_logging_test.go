package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/burmudar/go-receipt/paperless"
	"log/slog"
	"strings"
	"testing"
)

func TestArchiveErrorsLogSeparately(t *testing.T) {
	var out bytes.Buffer
	s := &Server{cfg: Config{Logger: slog.New(slog.NewJSONHandler(&out, nil))}}
	s.logArchiveError(&paperless.BatchError{Failures: []paperless.ReceiptFailure{
		{SHA256: "first", Err: errors.New("Paperless returned HTTP 400; invalid fields: tags")},
		{SHA256: "second", Err: errors.New("uncertain upload\nlocal failure")},
	}})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two log records: %s", out.String())
	}
	for i, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["receipt_sha256"] != []string{"first", "second"}[i] || entry["msg"] != "receipt archive failed" {
			t.Fatal(entry)
		}
		if strings.Contains(entry["error"].(string), "\n") {
			t.Fatal("multiline error")
		}
	}
}
