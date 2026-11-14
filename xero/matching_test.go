package xero

import (
	"testing"
	"time"

	"github.com/burmudar/go-receipt/receipt"
)

func TestMatchTransaction(t *testing.T) {
	a := &receipt.Analysis{Merchant: "The Shop", Total: "12.340", Currency: "USD", Date: time.Date(2025, 1, 2, 23, 0, 0, 0, time.FixedZone("local", 3600)), Confidence: .8}
	base := BankTransaction{ID: "id", Status: "AUTHORISED", Reconciled: true, Date: "2025-01-02", Contact: "THE SHOP", Currency: "USD", Total: "12.34"}
	high, ok := matchTransaction(a, base)
	if !ok || high.Confidence != "High" {
		t.Fatalf("exact match: %+v %v", high, ok)
	}
	near := base
	near.Date = "2025-01-05"
	near.Contact = "Different"
	low, ok := matchTransaction(a, near)
	if !ok || low.score >= high.score || low.Confidence != "Low" {
		t.Fatalf("near match: %+v %v", low, ok)
	}
	for name, change := range map[string]func(*BankTransaction){
		"currency":     func(t *BankTransaction) { t.Currency = "EUR" },
		"amount":       func(t *BankTransaction) { t.Total = "12.341" },
		"unreconciled": func(t *BankTransaction) { t.Reconciled = false },
		"status":       func(t *BankTransaction) { t.Status = "DELETED" },
		"date":         func(t *BankTransaction) { t.Date = "2025-01-10" },
		"invalid date": func(t *BankTransaction) { t.Date = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			change(&candidate)
			if s, ok := matchTransaction(a, candidate); ok {
				t.Fatalf("unexpected match: %+v", s)
			}
		})
	}
}
