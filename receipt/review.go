package receipt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Review is the current manual decision, separate from extraction and delivery.
type Review struct {
	State         string
	Corrected     *Analysis
	TenantID      string
	TransactionID string
}

type ReviewEvent struct {
	ID            int64
	Action        string
	Analysis      *Analysis
	TenantID      string
	TransactionID string
	Retention     string
	CreatedAt     string
}

var ErrReviewConflict = errors.New("review conflict")

// Reviews returns persisted decisions indexed by receipt hash; absent rows need review.
func (s *Store) Reviews(ctx context.Context) (map[string]Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT receipt_sha256,state,corrected_analysis,COALESCE(tenant_id,''),COALESCE(bank_transaction_id,'') FROM receipt_review`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Review{}
	for rows.Next() {
		var hash string
		var r Review
		var data sql.NullString
		if err := rows.Scan(&hash, &r.State, &data, &r.TenantID, &r.TransactionID); err != nil {
			return nil, err
		}
		if data.Valid {
			if err := json.Unmarshal([]byte(data.String), &r.Corrected); err != nil {
				return nil, err
			}
		}
		result[hash] = r
	}
	return result, rows.Err()
}

func (s *Store) ReviewHistory(ctx context.Context, hash string) ([]ReviewEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,action,analysis,COALESCE(tenant_id,''),COALESCE(bank_transaction_id,''),retention,created_at FROM receipt_review_event WHERE receipt_sha256=? ORDER BY id DESC`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []ReviewEvent
	for rows.Next() {
		var e ReviewEvent
		var analysis sql.NullString
		if err := rows.Scan(&e.ID, &e.Action, &analysis, &e.TenantID, &e.TransactionID, &e.Retention, &e.CreatedAt); err != nil {
			return nil, err
		}
		if analysis.Valid {
			if err := json.Unmarshal([]byte(analysis.String), &e.Analysis); err != nil {
				return nil, err
			}
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Correct saves a validated correction without changing the original extraction.
func (s *Store) Correct(ctx context.Context, hash string, analysis Analysis) error {
	if err := analysis.Validate(); err != nil {
		return fmt.Errorf("invalid correction: %w", err)
	}
	data, err := json.Marshal(analysis)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, state string
	err = tx.QueryRowContext(ctx, `SELECT r.status,COALESCE(v.state,'needs_review') FROM receipt r LEFT JOIN receipt_review v ON v.receipt_sha256=r.sha256 WHERE r.sha256=?`, hash).Scan(&status, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: receipt not found", ErrReviewConflict)
	}
	if err != nil {
		return err
	}
	if status != Extracted || state == "approved" {
		return fmt.Errorf("%w: extraction must be complete and approval cleared before editing", ErrReviewConflict)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO receipt_review(receipt_sha256,state,corrected_analysis) VALUES (?, 'needs_review', ?) ON CONFLICT(receipt_sha256) DO UPDATE SET corrected_analysis=excluded.corrected_analysis,state='needs_review',tenant_id=NULL,bank_transaction_id=NULL,updated_at=CURRENT_TIMESTAMP`, hash, string(data))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO receipt_review_event(receipt_sha256,action,analysis) VALUES (?,'corrected',?)`, hash, string(data))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Decide atomically validates the current organisation and target and records the decision.
// Ignored receipts are retained in the intake; the retention choice is recorded in the event.
func (s *Store) Decide(ctx context.Context, hash, action, target string) error {
	switch action {
	case "approved", "rejected", "ignored", "unmatched", "needs_review":
	default:
		return fmt.Errorf("%w: unknown decision", ErrReviewConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, state string
	var original, corrected sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT r.status, r.analysis, COALESCE(v.state,'needs_review'), v.corrected_analysis FROM receipt r LEFT JOIN receipt_review v ON v.receipt_sha256=r.sha256 WHERE r.sha256=?`, hash).Scan(&status, &original, &state, &corrected)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: receipt not found", ErrReviewConflict)
	}
	if err != nil {
		return err
	}
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT locked FROM receipt_attachment WHERE receipt_sha256=?),0)`, hash).Scan(&locked); err != nil {
		return err
	}
	if locked {
		return fmt.Errorf("%w: attachment started or confirmed; target changes require manual follow-up in Xero", ErrReviewConflict)
	}
	if status != Extracted {
		return fmt.Errorf("%w: extraction must be complete before review", ErrReviewConflict)
	}
	if state == "approved" && action != "needs_review" {
		return fmt.Errorf("%w: return the approval to review first", ErrReviewConflict)
	}
	var tenant, transaction any
	if action == "approved" || action == "rejected" {
		if target == "" {
			return fmt.Errorf("%w: select a transaction", ErrReviewConflict)
		}
		var tenantID, transactionID, status string
		var reconciled bool
		err = tx.QueryRowContext(ctx, `SELECT t.tenant_id,t.bank_transaction_id,t.status,t.is_reconciled FROM xero_bank_transaction t JOIN xero_connection c ON c.id=1 AND c.tenant_id=t.tenant_id AND c.status='connected' WHERE t.bank_transaction_id=?`, target).Scan(&tenantID, &transactionID, &status, &reconciled)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: transaction not found in the connected organisation", ErrReviewConflict)
		}
		if err != nil {
			return err
		}
		if action == "approved" && (status != "AUTHORISED" || !reconciled) {
			return fmt.Errorf("%w: transaction must be reconciled and authorised", ErrReviewConflict)
		}
		tenant, transaction = tenantID, transactionID
	}
	var analysis any
	if corrected.Valid {
		analysis = corrected.String
	} else if original.Valid {
		analysis = original.String
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO receipt_review(receipt_sha256,state,tenant_id,bank_transaction_id) VALUES (?,?,?,?) ON CONFLICT(receipt_sha256) DO UPDATE SET state=excluded.state,tenant_id=excluded.tenant_id,bank_transaction_id=excluded.bank_transaction_id,updated_at=CURRENT_TIMESTAMP`, hash, action, tenant, transaction)
	if err != nil {
		return err
	}
	if action != "approved" {
		if _, err = tx.ExecContext(ctx, `UPDATE receipt_attachment SET state='awaiting_approval' WHERE receipt_sha256=? AND locked=0`, hash); err != nil {
			return err
		}
	}
	retention := ""
	if action == "ignored" {
		retention = "retain_in_intake"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO receipt_review_event(receipt_sha256,action,analysis,tenant_id,bank_transaction_id,retention) VALUES (?,?,?,?,?,?)`, hash, action, analysis, tenant, transaction, retention)
	if err != nil {
		return err
	}
	return tx.Commit()
}
