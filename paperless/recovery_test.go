package paperless

import (
	"context"
	"strings"
	"testing"
)

func TestRejectedUploadCanRetry(t *testing.T) {
	conn, _, hash, f, cfg := setup(t)
	f.uploadStatus = 400
	m := manager(t, conn, cfg)
	ctx := context.Background()
	if err := m.Process(ctx, fileSource{}); err == nil {
		t.Fatal("expected rejected upload")
	}
	var dispatched bool
	conn.QueryRow("SELECT dispatched FROM receipt_archive WHERE receipt_sha256=?", hash).Scan(&dispatched)
	if dispatched {
		t.Fatal("definitive HTTP 400 rejection incorrectly marked uncertain")
	}
	a := state(t, conn, hash)
	if !strings.Contains(a.Error, "tags") || strings.Contains(a.Error, "private-token") {
		t.Fatal("missing safe validation detail", a.Error)
	}
	f.uploadStatus = 0
	if err := m.Process(ctx, fileSource{}); err != nil {
		t.Fatal(err)
	}
	if state(t, conn, hash).TaskID != "task-1" || f.uploads != 2 {
		t.Fatal("rejected upload did not resume")
	}
}
func TestUncertainUploadPreservesOriginalFailure(t *testing.T) {
	conn, _, hash, f, cfg := setup(t)
	f.uploadStatus = 500
	m := manager(t, conn, cfg)
	ctx := context.Background()
	if err := m.Process(ctx, fileSource{}); err == nil {
		t.Fatal("expected failure")
	}
	original := state(t, conn, hash).Error
	for range 2 {
		if err := m.Process(ctx, fileSource{}); err == nil {
			t.Fatal("expected blocked retry")
		}
	}
	if state(t, conn, hash).Error != original {
		t.Fatal("recovery overwrote the original upload error")
	}
	if f.uploads != 1 {
		t.Fatal("uncertain upload repeated")
	}
}
