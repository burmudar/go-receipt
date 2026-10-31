package xero

import (
	"context"
	"fmt"
	"strings"
)

// SearchBankTransactions searches stored transactions only in the connected organisation.
// LIKE metacharacters are escaped so the user's query is treated literally.
func (m *Manager) SearchBankTransactions(ctx context.Context, query string) ([]BankTransaction, error) {
	q := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(query))
	pattern := "%" + q + "%"
	rows, err := m.db.QueryContext(ctx, `SELECT t.bank_transaction_id,t.type,t.status,t.is_reconciled,t.date,t.contact_name,t.bank_account_name,t.reference,t.currency_code,t.total,t.has_attachments,t.updated_date_utc
 FROM xero_bank_transaction t JOIN xero_connection c ON c.id=1 AND c.tenant_id=t.tenant_id AND c.status='connected'
 WHERE t.contact_name LIKE ? ESCAPE '\' OR t.reference LIKE ? ESCAPE '\' OR t.bank_account_name LIKE ? ESCAPE '\' OR t.date LIKE ? ESCAPE '\' OR t.total LIKE ? ESCAPE '\' OR t.bank_transaction_id LIKE ? ESCAPE '\'
 ORDER BY t.date DESC,t.bank_transaction_id LIMIT 100`, pattern, pattern, pattern, pattern, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("search bank transactions: %w", err)
	}
	defer rows.Close()
	var result []BankTransaction
	for rows.Next() {
		var t BankTransaction
		if err := rows.Scan(&t.ID, &t.Type, &t.Status, &t.Reconciled, &t.Date, &t.Contact, &t.BankAccount, &t.Reference, &t.Currency, &t.Total, &t.HasAttachments, &t.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}
