package xero

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	// maxRateLimitWait is the longest Retry-After that a refresh waits for. A longer wait, for example
	// for the daily limit, stops the refresh. The next refresh continues from the last complete refresh.
	maxRateLimitWait    = time.Minute
	maxRateLimitRetries = 3
	// listLimit is the number of transactions that BankTransactions returns.
	listLimit = 500
)

var ErrRefreshRunning = errors.New("a bank transaction refresh is already running")

// BankTransaction is a stored SPEND or RECEIVE bank transaction.
type BankTransaction struct {
	ID             string
	Type           string
	Status         string
	Reconciled     bool
	Date           string // YYYY-MM-DD
	Contact        string
	BankAccount    string
	Reference      string
	Currency       string
	Total          string // exact decimal
	HasAttachments bool
	UpdatedAt      time.Time
}

// Eligible reports whether a new receipt link can target the transaction.
func (t BankTransaction) Eligible() bool {
	return t.Status == "AUTHORISED" && t.Reconciled
}

// BankTransactionSummary describes the stored transactions of the connected organisation.
type BankTransactionSummary struct {
	Stored      int
	Eligible    int
	RefreshedAt time.Time // last complete refresh
	AttemptedAt time.Time // last refresh, complete or not
	LastError   string
}

// RefreshBankTransactions fetches the bank transactions that changed since the last complete refresh,
// and stores the SPEND and RECEIVE records. The first refresh fetches all records.
// Each page is saved when it arrives. The start time of the next refresh is saved only after all pages
// are saved, so a refresh that fails does not lose a change.
func (m *Manager) RefreshBankTransactions(ctx context.Context) error {
	if !m.refreshing.TryLock() {
		return ErrRefreshRunning
	}
	defer m.refreshing.Unlock()

	_, tenantID, err := m.AccessToken(ctx)
	if err != nil {
		return err
	}
	attempted := m.now().UTC()
	next, err := m.fetchBankTransactions(ctx, tenantID)

	// Record the result also when the request was cancelled.
	ctx = context.WithoutCancel(ctx)
	if err != nil {
		_, saveErr := m.db.ExecContext(ctx, `
			INSERT INTO xero_transaction_refresh (tenant_id, attempted_at, last_error) VALUES (?, ?, ?)
			ON CONFLICT (tenant_id) DO UPDATE SET attempted_at = excluded.attempted_at, last_error = excluded.last_error`,
			tenantID, attempted, err.Error(),
		)
		return errors.Join(err, saveErr)
	}
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO xero_transaction_refresh (tenant_id, modified_since, refreshed_at, attempted_at, last_error) VALUES (?, ?, ?, ?, '')
		ON CONFLICT (tenant_id) DO UPDATE SET
		modified_since = excluded.modified_since,
		refreshed_at = excluded.refreshed_at,
		attempted_at = excluded.attempted_at,
		last_error = ''`,
		tenantID, next.UTC(), m.now().UTC(), attempted,
	)
	if err != nil {
		return fmt.Errorf("save xero transaction refresh: %w", err)
	}
	return nil
}

// fetchBankTransactions stores all pages and returns the time from which the next refresh must start.
func (m *Manager) fetchBankTransactions(ctx context.Context, tenantID string) (time.Time, error) {
	var since sql.NullTime
	err := m.db.QueryRowContext(ctx,
		"SELECT modified_since FROM xero_transaction_refresh WHERE tenant_id = ?", tenantID,
	).Scan(&since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("read xero transaction refresh: %w", err)
	}

	// Use the Xero clock, so that a wrong local clock cannot skip changes. Use the local clock only
	// if Xero does not send a Date header.
	next := m.now()
	received, stored := 0, 0
	statuses := map[string]int{}
	page := 1
	for ; ; page++ {
		p, err := m.bankTransactionPage(ctx, tenantID, since.Time, page)
		if err != nil {
			return time.Time{}, err
		}
		if page == 1 && !p.serverTime.IsZero() {
			next = p.serverTime
		}
		n, err := m.storeBankTransactions(ctx, tenantID, p.transactions)
		if err != nil {
			return time.Time{}, err
		}
		received, stored = received+len(p.transactions), stored+n
		for _, t := range p.transactions {
			statuses[t.Type+" "+t.Status]++
		}
		if len(p.transactions) < pageSize {
			break
		}
	}
	m.log.Info("xero bank transactions refreshed",
		"full", !since.Valid, "modified_since", since.Time, "pages", page, "received", received, "stored", stored, "types_and_statuses", statuses)
	return next, nil
}

// bankTransactionPage fetches one page. It waits and tries again after a short rate-limit response.
func (m *Manager) bankTransactionPage(ctx context.Context, tenantID string, since time.Time, page int) (bankTransactionPage, error) {
	for attempt := 1; ; attempt++ {
		accessToken, current, err := m.AccessToken(ctx)
		if err != nil {
			return bankTransactionPage{}, err
		}
		if current != tenantID {
			return bankTransactionPage{}, errors.New("the xero organisation changed during the refresh")
		}
		p, err := m.client.bankTransactions(ctx, accessToken, tenantID, since, page)
		if errors.Is(err, errUnauthorized) {
			return bankTransactionPage{}, m.requireReconnect(ctx, err)
		}
		var limit *rateLimitError
		if !errors.As(err, &limit) || limit.retryAfter > maxRateLimitWait || attempt > maxRateLimitRetries {
			return p, err
		}
		m.log.Warn("xero rate limit reached; waiting before the next request",
			"limit", limit.problem, "retry_after", limit.retryAfter, "page", page, "attempt", attempt)
		timer := time.NewTimer(limit.retryAfter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return bankTransactionPage{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// storeBankTransactions saves the SPEND and RECEIVE records of one page in one transaction.
// A record that already exists is updated, so deleted and voided records keep their row.
func (m *Manager) storeBankTransactions(ctx context.Context, tenantID string, records []bankTransactionJSON) (int, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := m.now().UTC()
	stored := 0
	for _, r := range records {
		if r.Type != "SPEND" && r.Type != "RECEIVE" {
			continue
		}
		if r.BankTransactionID == "" || r.Total == "" {
			return 0, fmt.Errorf("xero bank transaction %q has no ID or total", r.BankTransactionID)
		}
		date, err := parseXeroTime(r.Date)
		if err != nil {
			return 0, fmt.Errorf("xero bank transaction %s: %w", r.BankTransactionID, err)
		}
		updated, err := parseXeroTime(r.UpdatedDateUTC)
		if err != nil {
			return 0, fmt.Errorf("xero bank transaction %s: %w", r.BankTransactionID, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO xero_bank_transaction (
				tenant_id, bank_transaction_id, type, status, is_reconciled, date, contact_name, bank_account_name,
				reference, currency_code, total, has_attachments, updated_date_utc, fetched_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, bank_transaction_id) DO UPDATE SET
			type = excluded.type,
			status = excluded.status,
			is_reconciled = excluded.is_reconciled,
			date = excluded.date,
			contact_name = excluded.contact_name,
			bank_account_name = excluded.bank_account_name,
			reference = excluded.reference,
			currency_code = excluded.currency_code,
			total = excluded.total,
			has_attachments = excluded.has_attachments,
			updated_date_utc = excluded.updated_date_utc,
			fetched_at = excluded.fetched_at`,
			tenantID, r.BankTransactionID, r.Type, r.Status, r.IsReconciled, date.Format(time.DateOnly), r.Contact.Name, r.BankAccount.Name,
			r.Reference, r.CurrencyCode, r.Total.String(), r.HasAttachments, updated, now,
		)
		if err != nil {
			return 0, fmt.Errorf("save xero bank transaction %s: %w", r.BankTransactionID, err)
		}
		stored++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("save xero bank transactions: %w", err)
	}
	return stored, nil
}

// BankTransactionSummary returns the counts and refresh state for the connected organisation.
func (m *Manager) BankTransactionSummary(ctx context.Context) (BankTransactionSummary, error) {
	var s BankTransactionSummary
	var refreshed, attempted sql.NullTime
	err := m.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM xero_bank_transaction t WHERE t.tenant_id = c.tenant_id),
		  (SELECT COUNT(*) FROM xero_bank_transaction t
		   WHERE t.tenant_id = c.tenant_id AND t.status = 'AUTHORISED' AND t.is_reconciled),
		  r.refreshed_at, r.attempted_at, COALESCE(r.last_error, '')
		FROM xero_connection c LEFT JOIN xero_transaction_refresh r ON r.tenant_id = c.tenant_id
		WHERE c.id = 1`,
	).Scan(&s.Stored, &s.Eligible, &refreshed, &attempted, &s.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return BankTransactionSummary{}, nil
	}
	if err != nil {
		return BankTransactionSummary{}, fmt.Errorf("read xero bank transaction summary: %w", err)
	}
	s.RefreshedAt, s.AttemptedAt = refreshed.Time, attempted.Time
	return s, nil
}

// BankTransactions returns the latest stored transactions of the connected organisation, newest first.
func (m *Manager) BankTransactions(ctx context.Context) ([]BankTransaction, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.bank_transaction_id, t.type, t.status, t.is_reconciled, t.date, t.contact_name, t.bank_account_name,
		       t.reference, t.currency_code, t.total, t.has_attachments, t.updated_date_utc
		FROM xero_bank_transaction t JOIN xero_connection c ON c.id = 1 AND t.tenant_id = c.tenant_id
		ORDER BY t.date DESC, t.bank_transaction_id
		LIMIT ?`, listLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("read xero bank transactions: %w", err)
	}
	defer rows.Close()

	var transactions []BankTransaction
	for rows.Next() {
		var t BankTransaction
		if err := rows.Scan(&t.ID, &t.Type, &t.Status, &t.Reconciled, &t.Date, &t.Contact, &t.BankAccount,
			&t.Reference, &t.Currency, &t.Total, &t.HasAttachments, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read xero bank transaction: %w", err)
		}
		transactions = append(transactions, t)
	}
	return transactions, rows.Err()
}
