package xero

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/burmudar/go-receipt/receipt"
)

// Conservative decimal 10 MB cap; see docs/xero-api.md.
const MaxAttachmentBytes = 10_000_000

var errAttachmentTargetMissing = errors.New("Xero attachment target not found")

type attachmentJob struct {
	hash, source, tenant, target, total, currency, date, name string
	dispatched                                                bool
}
type attachmentJSON struct{ AttachmentID, FileName string }

// ProcessAttachments resumes approved, incomplete work. One service/Manager per DB.
func (m *Manager) ProcessAttachments(ctx context.Context, source receipt.Source) error {
	m.attaching.Lock()
	defer m.attaching.Unlock()
	rows, err := m.db.QueryContext(ctx, `SELECT a.receipt_sha256,r.source,a.tenant_id,a.transaction_id,a.total,a.currency,a.date,a.dispatched,r.name FROM receipt_attachment a JOIN receipt r ON r.sha256=a.receipt_sha256 JOIN receipt_review v ON v.receipt_sha256=a.receipt_sha256 WHERE v.state='approved' AND a.state!='confirmed'`)
	if err != nil {
		return err
	}
	var jobs []attachmentJob
	for rows.Next() {
		var j attachmentJob
		if err = rows.Scan(&j.hash, &j.source, &j.tenant, &j.target, &j.total, &j.currency, &j.date, &j.dispatched, &j.name); err != nil {
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
	var failures []error
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Atomic claim fences review edits before any remote work.
		result, e := m.db.ExecContext(ctx, `UPDATE receipt_attachment SET locked=1,state='pending' WHERE receipt_sha256=? AND total=? AND currency=? AND date=? AND EXISTS(SELECT 1 FROM receipt_review WHERE receipt_sha256=? AND state='approved' AND tenant_id=? AND bank_transaction_id=?)`, j.hash, j.total, j.currency, j.date, j.hash, j.tenant, j.target)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			continue
		}
		e = m.attach(ctx, source, j)
		if e != nil {
			// Once dispatched, even an error may mean Xero accepted it. Never unlock that target.
			_, saveErr := m.db.ExecContext(context.WithoutCancel(ctx), `UPDATE receipt_attachment SET state='failed',last_error=?,locked=dispatched WHERE receipt_sha256=? AND state!='confirmed'`, e.Error(), j.hash)
			failures = append(failures, errors.Join(e, saveErr))
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) attachmentRequest(ctx context.Context, j attachmentJob, method, path string, data []byte, key string) ([]byte, error) {
	token, tenant, err := m.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	if tenant != j.tenant {
		return nil, fmt.Errorf("approved organisation is not connected")
	}
	req, err := http.NewRequestWithContext(ctx, method, m.client.accounting+"/BankTransactions/"+url.PathEscape(j.target)+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("xero-tenant-id", tenant)
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	client := *m.client.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Xero attachment request failed; retry will check remote attachments")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errAttachmentTargetMissing
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Xero attachment request returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, MaxAttachmentBytes+1))
}

func (m *Manager) attach(ctx context.Context, source receipt.Source, j attachmentJob) error {
	// The name and key depend only on immutable receipt/tenant/target identity.
	digest := sha256.Sum256([]byte(j.hash + ":" + j.tenant + ":" + j.target))
	key := hex.EncodeToString(digest[:])
	ext := strings.ToLower(filepath.Ext(j.name))
	switch ext {
	case ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return fmt.Errorf("unsupported attachment filename extension")
	}
	name := "receipt-" + j.hash + ext
	confirm := func(id string) error {
		_, err := m.db.ExecContext(ctx, `UPDATE receipt_attachment SET state='confirmed',locked=1,attachment_id=?,completed_at=CURRENT_TIMESTAMP,last_error='' WHERE receipt_sha256=?`, id, j.hash)
		return err
	}
	// Always inspect remote state before PUT, especially after an uncertain response/restart.
	body, err := m.attachmentRequest(ctx, j, http.MethodGet, "/Attachments", nil, "")
	if errors.Is(err, errAttachmentTargetMissing) {
		return m.attachmentNeedsReview(ctx, j)
	}
	if err != nil {
		return err
	}
	var list struct{ Attachments []attachmentJSON }
	if err = json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("invalid Xero attachment list")
	}
	for _, a := range list.Attachments {
		if a.FileName == name {
			// A matching name alone is not proof: verify remote content before confirming.
			remote, e := m.attachmentRequest(ctx, j, http.MethodGet, "/Attachments/"+url.PathEscape(a.AttachmentID), nil, "")
			if e != nil {
				return e
			}
			if (receipt.File{Data: remote}).SHA256() != j.hash {
				return fmt.Errorf("remote attachment name collision; manual follow-up required")
			}
			return confirm(a.AttachmentID)
		}
	}
	if source == nil {
		return fmt.Errorf("receipt source is not configured")
	}
	data, err := source.Read(ctx, j.source)
	if err != nil {
		return fmt.Errorf("receipt original unavailable; restore the source and retry")
	}
	if len(data) > MaxAttachmentBytes {
		return fmt.Errorf("receipt exceeds the 10 MB attachment limit")
	}
	if (receipt.File{Data: data}).SHA256() != j.hash {
		return fmt.Errorf("receipt original has changed")
	}
	body, err = m.attachmentRequest(ctx, j, http.MethodGet, "", nil, "")
	if errors.Is(err, errAttachmentTargetMissing) {
		return m.attachmentNeedsReview(ctx, j)
	}
	if err != nil {
		return err
	}
	var target struct{ BankTransactions []bankTransactionJSON }
	if json.Unmarshal(body, &target) != nil {
		return fmt.Errorf("invalid Xero bank transaction response")
	}
	valid := false
	if len(target.BankTransactions) == 1 {
		t := target.BankTransactions[0]
		d, e := parseXeroTime(t.Date)
		a, aok := new(big.Rat).SetString(t.Total.String())
		b, bok := new(big.Rat).SetString(j.total)
		valid = e == nil && t.BankTransactionID == j.target && (t.Type == "SPEND" || t.Type == "RECEIVE") && t.Status == "AUTHORISED" && t.IsReconciled && t.CurrencyCode == j.currency && d.Format("2006-01-02") == j.date && aok && bok && a.Cmp(b) == 0
	}
	if !valid {
		return m.attachmentNeedsReview(ctx, j)
	}
	// Persist uncertainty BEFORE sending bytes. Crash recovery can never change the target.
	if _, err = m.db.ExecContext(ctx, `UPDATE receipt_attachment SET dispatched=1 WHERE receipt_sha256=?`, j.hash); err != nil {
		return err
	}
	body, err = m.attachmentRequest(ctx, j, http.MethodPut, "/Attachments/"+url.PathEscape(name), data, key)
	if err != nil {
		return err
	}
	var uploaded struct{ Attachments []attachmentJSON }
	if json.Unmarshal(body, &uploaded) != nil {
		return fmt.Errorf("uncertain upload response; retry will check remote attachments")
	}
	for _, a := range uploaded.Attachments {
		if a.FileName == name && a.AttachmentID != "" {
			return confirm(a.AttachmentID)
		}
	}
	return fmt.Errorf("upload confirmation missing; retry will check remote attachments")
}

func (m *Manager) attachmentNeedsReview(ctx context.Context, j attachmentJob) error {
	if j.dispatched {
		return fmt.Errorf("target changed after an uncertain upload; manual follow-up required")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE receipt_attachment SET locked=0 WHERE receipt_sha256=?`, j.hash); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE receipt_review SET state='needs_review',updated_at=CURRENT_TIMESTAMP WHERE receipt_sha256=?`, j.hash); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO receipt_review_event(receipt_sha256,action,tenant_id,bank_transaction_id,retention) VALUES (?,'needs_review',?,?,'Xero target changed or is no longer eligible')`, j.hash, j.tenant, j.target); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return fmt.Errorf("Xero target changed or is no longer eligible; returned to review")
}
