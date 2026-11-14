package receipt

import (
	"context"
	"fmt"
)

// CleanupSource verifies the original immediately before deletion. beforeDelete
// must persist dispatch before any destructive operation. This is best-effort:
// neither local unlink nor Copyparty DELETE excludes a concurrent replacement.
type CleanupSource interface {
	Cleanup(context.Context, string, string, func() error) error
}

type Cleanup struct {
	State, Error, CompletedAt string
	Attempts                  int
}

func (s *Store) Cleanups(ctx context.Context) (map[string]Cleanup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT receipt_sha256,state,last_error,completed_at,attempts FROM receipt_cleanup`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Cleanup{}
	for rows.Next() {
		var hash string
		var c Cleanup
		if err := rows.Scan(&hash, &c.State, &c.Error, &c.CompletedAt, &c.Attempts); err != nil {
			return nil, err
		}
		result[hash] = c
	}
	return result, rows.Err()
}

// ProcessCleanup requires one serial cleanup worker per database, including
// across service processes; it does not coordinate concurrent workers.
// Failed or interrupted attempts retry on the next cycle. An accessible source
// that is already absent completes cleanup; matching files are reverified before
// every deletion attempt. Remote uploads are never invoked.
func (s *Store) ProcessCleanup(ctx context.Context, source CleanupSource) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE receipt_cleanup SET state='failed',last_error='Interrupted cleanup; retrying when eligible' WHERE state='deleting'`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO receipt_cleanup(receipt_sha256)
 SELECT r.sha256 FROM receipt r JOIN receipt_review v ON v.receipt_sha256=r.sha256
 JOIN receipt_attachment a ON a.receipt_sha256=r.sha256 JOIN receipt_archive p ON p.receipt_sha256=r.sha256
 WHERE r.status='extracted' AND v.state='approved' AND a.state='confirmed' AND p.state='confirmed'
 AND a.attachment_id!='' AND a.completed_at!='' AND p.document_id>0 AND p.completed_at!=''
 AND a.tenant_id=v.tenant_id AND a.transaction_id=v.bank_transaction_id
 ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.sha256,r.source FROM receipt r
 JOIN receipt_cleanup c ON c.receipt_sha256=r.sha256 JOIN receipt_review v ON v.receipt_sha256=r.sha256
 JOIN receipt_attachment a ON a.receipt_sha256=r.sha256 JOIN receipt_archive p ON p.receipt_sha256=r.sha256
 WHERE c.state IN ('pending','failed','uncertain') AND r.status='extracted' AND v.state='approved'
 AND a.state='confirmed' AND p.state='confirmed' AND a.attachment_id!='' AND a.completed_at!=''
 AND p.document_id>0 AND p.completed_at!='' AND a.tenant_id=v.tenant_id AND a.transaction_id=v.bank_transaction_id`)
	if err != nil {
		return err
	}
	type job struct{ hash, source string }
	var jobs []job
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.hash, &j.source); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE receipt_cleanup SET attempts=attempts+1 WHERE receipt_sha256=?`, j.hash); err != nil {
			return err
		}
		dispatched := false
		attemptErr := source.Cleanup(ctx, j.source, j.hash, func() error {
			// Normal review changes are already excluded by the attachment lock.
			// Recheck authorization atomically with dispatch as verification may
			// take time, and do not rely on the earlier candidate snapshot.
			result, e := s.db.ExecContext(ctx, `UPDATE receipt_cleanup SET state='deleting',last_error=''
 WHERE receipt_sha256=? AND state IN ('pending','failed','uncertain') AND EXISTS (
 SELECT 1 FROM receipt r JOIN receipt_review v ON v.receipt_sha256=r.sha256
 JOIN receipt_attachment a ON a.receipt_sha256=r.sha256 JOIN receipt_archive p ON p.receipt_sha256=r.sha256
 WHERE r.sha256=? AND r.source=? AND r.status='extracted' AND v.state='approved'
 AND a.state='confirmed' AND p.state='confirmed' AND a.attachment_id!='' AND a.completed_at!=''
 AND p.document_id>0 AND p.completed_at!='' AND a.tenant_id=v.tenant_id AND a.transaction_id=v.bank_transaction_id)`, j.hash, j.hash, j.source)
			if e != nil {
				return e
			}
			n, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return fmt.Errorf("cleanup authorization changed before dispatch")
			}
			dispatched = true
			return nil
		})
		state, message := "confirmed", ""
		if attemptErr != nil || !dispatched {
			state = "failed"
			message = "Cleanup authorization not confirmed; retrying when eligible"
			if attemptErr != nil {
				message = fmt.Sprintf("Cleanup failed; retrying when eligible: %v", attemptErr)
			}
		}
		_, err = s.db.ExecContext(ctx, `UPDATE receipt_cleanup SET state=?,last_error=?,completed_at=CASE WHEN ?='confirmed' THEN strftime('%Y-%m-%dT%H:%M:%SZ','now') ELSE '' END WHERE receipt_sha256=?`, state, message, state, j.hash)
		if err != nil {
			return fmt.Errorf("record cleanup: %w", err)
		}
	}
	return nil
}
