package receipt_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"os"
	"strings"
	"testing"
)

func TestCleanupRestartAfterRemovalBeforeConfirmation(t *testing.T) {
	conn, store, hash, path := cleanupFixture(t)
	ctx := context.Background()
	if _, err := conn.Exec(`CREATE TRIGGER fail_cleanup_confirmation BEFORE UPDATE ON receipt_cleanup WHEN NEW.state='confirmed' BEGIN SELECT RAISE(ABORT,'simulated commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessCleanup(ctx, intake.LocalSource{}); err == nil {
		t.Fatal("expected persistence failure")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("test did not reach removal")
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`DROP TRIGGER fail_cleanup_confirmation`); err != nil {
		t.Fatal(err)
	}
	if err := receipt.NewStore(conn).ProcessCleanup(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatal("restart removed replacement", err)
	}
	states, err := store.Cleanups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states[hash].State != "failed" || states[hash].CompletedAt != "" {
		t.Fatal(states[hash])
	}
}

func TestCleanupRetriesWebDAVDeletionAfterPermissionRestored(t *testing.T) {
	conn, store, hash, _ := cleanupFixture(t)
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			fmt.Fprint(w, "original")
		case http.MethodDelete:
			deletes++
			if deletes == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	if _, err := conn.Exec(`UPDATE receipt SET source=? WHERE sha256=?`, server.URL+"/inbox/receipt.pdf", hash); err != nil {
		t.Fatal(err)
	}
	source, err := intake.NewWebDAVInput(server.URL, "user", "password", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.ProcessCleanup(ctx, source); err != nil {
		t.Fatal(err)
	}
	states, err := store.Cleanups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states[hash].State != "failed" || !strings.Contains(states[hash].Error, "HTTP 403") {
		t.Fatalf("permission error: %+v", states[hash])
	}
	if err := store.ProcessCleanup(ctx, source); err != nil {
		t.Fatal(err)
	}
	states, err = store.Cleanups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states[hash].State != "confirmed" || deletes != 2 {
		t.Fatalf("retry: %+v; deletions=%d", states[hash], deletes)
	}
}

func TestCleanupRetriesPreviouslyUncertainFile(t *testing.T) {
	conn, store, hash, path := cleanupFixture(t)
	if _, err := conn.Exec(`INSERT INTO receipt_cleanup(receipt_sha256,state,last_error) VALUES (?,'uncertain','old error')`, hash); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessCleanup(context.Background(), intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	states, err := store.Cleanups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if states[hash].State != "confirmed" {
		t.Fatalf("cleanup: %+v", states[hash])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file not removed: %v", err)
	}
}

func TestCleanupRestartConfirmsWhenSourceIsAbsent(t *testing.T) {
	conn, store, hash, path := cleanupFixture(t)
	ctx := context.Background()
	if _, err := conn.Exec(`INSERT INTO receipt_cleanup(receipt_sha256,state) VALUES (?,'deleting')`, hash); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessCleanup(ctx, intake.LocalSource{}); err != nil {
		t.Fatal(err)
	}
	states, err := store.Cleanups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states[hash].State != "confirmed" || states[hash].CompletedAt == "" {
		t.Fatal("missing source should complete cleanup", states[hash])
	}
}
