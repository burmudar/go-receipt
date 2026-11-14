package xero

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/burmudar/go-receipt/receipt"
)

// Suggestion is a heuristic, not an approval or a probability.
type Suggestion struct {
	Transaction BankTransaction
	Confidence  string
	Reasons     []string
	score       int
}

// SuggestedBankTransactions considers all eligible transactions in the connected
// organisation within seven calendar days, not just the first search page.
// An exact amount and currency are required; merchant and date rank the results.
func (m *Manager) SuggestedBankTransactions(ctx context.Context, a *receipt.Analysis) ([]Suggestion, error) {
	if a == nil || a.Validate() != nil {
		return nil, nil
	}
	date := a.Date.Format(time.DateOnly)
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, `SELECT t.bank_transaction_id,t.type,t.status,t.is_reconciled,t.date,t.contact_name,t.bank_account_name,t.reference,t.currency_code,t.total,t.has_attachments,t.updated_date_utc
 FROM xero_bank_transaction t JOIN xero_connection c ON c.id=1 AND c.tenant_id=t.tenant_id AND c.status='connected'
 WHERE t.status='AUTHORISED' AND t.is_reconciled=1 AND t.currency_code=? AND t.date BETWEEN ? AND ?`, a.Currency, day.AddDate(0, 0, -7).Format(time.DateOnly), day.AddDate(0, 0, 7).Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("find suggested transactions: %w", err)
	}
	defer rows.Close()
	var result []Suggestion
	for rows.Next() {
		var t BankTransaction
		if err := rows.Scan(&t.ID, &t.Type, &t.Status, &t.Reconciled, &t.Date, &t.Contact, &t.BankAccount, &t.Reference, &t.Currency, &t.Total, &t.HasAttachments, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if s, ok := matchTransaction(a, t); ok {
			result = append(result, s)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].score != result[j].score {
			return result[i].score > result[j].score
		}
		if result[i].Transaction.Date != result[j].Transaction.Date {
			return result[i].Transaction.Date > result[j].Transaction.Date
		}
		return result[i].Transaction.ID < result[j].Transaction.ID
	})
	return result, nil
}

func matchTransaction(a *receipt.Analysis, t BankTransaction) (Suggestion, bool) {
	amount, ok := new(big.Rat).SetString(a.Total)
	if !ok {
		return Suggestion{}, false
	}
	total, ok := new(big.Rat).SetString(t.Total)
	if !ok || amount.Cmp(total) != 0 || a.Currency != t.Currency || !t.Eligible() {
		return Suggestion{}, false
	}
	day, err := time.Parse(time.DateOnly, a.Date.Format(time.DateOnly))
	if err != nil {
		return Suggestion{}, false
	}
	transactionDay, err := time.Parse(time.DateOnly, t.Date)
	if err != nil {
		return Suggestion{}, false
	}
	days := int(transactionDay.Sub(day).Hours() / 24)
	if days < 0 {
		days = -days
	}
	if days > 7 {
		return Suggestion{}, false
	}
	s := Suggestion{Transaction: t, Confidence: "Low", Reasons: []string{"Exact amount and currency"}, score: 10}
	if days == 0 {
		s.score += 7
		s.Reasons = append(s.Reasons, "Same date")
	} else {
		s.score += 7 - days
		s.Reasons = append(s.Reasons, fmt.Sprintf("Date differs by %d day(s)", days))
	}
	merchant, contact := normalizeMerchant(a.Merchant), normalizeMerchant(t.Contact)
	if merchant != "" && merchant == contact {
		s.score += 7
		s.Reasons = append(s.Reasons, "Merchant matches contact")
	} else {
		s.Reasons = append(s.Reasons, "Merchant does not exactly match contact")
	}
	if s.score >= 24 {
		s.Confidence = "High"
	} else if s.score >= 17 {
		s.Confidence = "Medium"
	}
	return s, true
}

func normalizeMerchant(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }), " ")
}

// ReceiptLinks returns current approved links in the connected organisation.
func (m *Manager) ReceiptLinks(ctx context.Context) (map[string][]ReceiptLink, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT v.bank_transaction_id,r.sha256,r.name FROM receipt_review v JOIN receipt r ON r.sha256=v.receipt_sha256 JOIN xero_connection c ON c.id=1 AND c.status='connected' AND c.tenant_id=v.tenant_id WHERE v.state='approved' ORDER BY r.name,r.sha256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make(map[string][]ReceiptLink)
	for rows.Next() {
		var id string
		var link ReceiptLink
		if err := rows.Scan(&id, &link.Hash, &link.Name); err != nil {
			return nil, err
		}
		links[id] = append(links[id], link)
	}
	return links, rows.Err()
}

type ReceiptLink struct{ Hash, Name string }
