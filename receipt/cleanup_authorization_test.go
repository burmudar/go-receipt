package receipt_test

import (
	"context"
	"os"
	"testing"

	"github.com/burmudar/go-receipt/intake"
)

type authorizationChangeSource struct{ change func() }

func (s authorizationChangeSource) Cleanup(ctx context.Context, path, hash string, beforeDelete func() error) error {
	return (intake.LocalSource{}).Cleanup(ctx, path, hash, func() error { s.change(); return beforeDelete() })
}

// Real attachment delivery locks review changes. These deliberately mutable
// fixtures additionally prove dispatch does not trust a stale candidate snapshot.
func TestCleanupRechecksAuthorizationAtDispatch(t *testing.T) {
	for _, scenario := range []struct{ name, query string }{
		{"review", `UPDATE receipt_review SET state='ignored' WHERE receipt_sha256=?`},
		{"attachment", `UPDATE receipt_attachment SET state='failed' WHERE receipt_sha256=?`},
		{"archive", `UPDATE receipt_archive SET state='consuming' WHERE receipt_sha256=?`},
		{"attachment-reference", `UPDATE receipt_attachment SET attachment_id='' WHERE receipt_sha256=?`},
		{"archive-reference", `UPDATE receipt_archive SET document_id=0 WHERE receipt_sha256=?`},
		{"target", `UPDATE receipt_attachment SET transaction_id='other' WHERE receipt_sha256=?`},
		{"source", `UPDATE receipt SET source='changed' WHERE sha256=?`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			conn, store, hash, path := cleanupFixture(t)
			called := false
			source := authorizationChangeSource{change: func() {
				called = true
				if _, err := conn.Exec(scenario.query, hash); err != nil {
					t.Fatal(err)
				}
			}}
			if err := store.ProcessCleanup(context.Background(), source); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("test did not reach pre-dispatch authorization change")
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
				t.Fatal("unauthorized deletion", err)
			}
			states, err := store.Cleanups(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if states[hash].State != "failed" || states[hash].CompletedAt != "" {
				t.Fatalf("dispatch not refused: %+v", states[hash])
			}
		})
	}
}
