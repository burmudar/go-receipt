package receipt_test

import (
	"context"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"os"
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
	if states[hash].State != "uncertain" || states[hash].CompletedAt != "" {
		t.Fatal(states[hash])
	}
}

func TestCleanupRestartDoesNotInferCompletionFromAbsence(t *testing.T) {
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
	if states[hash].State != "uncertain" {
		t.Fatal("absence treated as confirmation")
	}
}
