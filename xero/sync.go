package xero

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var decimalPattern = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
var xeroDatePattern = regexp.MustCompile(`^/Date\((-?[0-9]+)(?:[+-][0-9]{4})?\)/$`)

// Sync reads spend-money bank transactions and purchase bills. It does not read
// bank statement lines, write accounting data, or perform reconciliation.
func (c *Client) Sync(ctx context.Context) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.access(ctx, false)
	if err != nil {
		return 0, err
	}
	if g.TenantID == "" {
		return 0, ErrSelect
	}
	connections, err := c.connections(ctx)
	if err != nil {
		return 0, err
	}
	found := false
	for _, connection := range connections {
		if connection.ID == g.ConnectionID && connection.TenantID == g.TenantID {
			found = true
		}
	}
	if !found {
		return 0, c.store.reconnect(ctx)
	}

	targets := []Target{}
	for _, kind := range []string{"BankTransactions", "Invoices"} {
		records, err := c.fetchTargets(ctx, g.TenantID, kind)
		if err != nil {
			return 0, err
		}
		targets = append(targets, records...)
	}
	if err := c.store.replaceTargets(ctx, g.TenantID, targets); err != nil {
		return 0, err
	}
	return len(targets), nil
}

func (c *Client) fetchTargets(ctx context.Context, tenant, kind string) ([]Target, error) {
	targets := []Target{}
	seen := map[string]bool{}
	for page := 1; page <= 10000; page++ {
		query := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {"100"}, "order": {map[string]string{
			"BankTransactions": "BankTransactionID ASC", "Invoices": "InvoiceID ASC"}[kind]}}
		// Fetch all types and states so a complete refresh also removes records
		// that have become deleted or voided. Filter locally after pagination.
		var response map[string]json.RawMessage
		if err := c.get(ctx, c.apiURL+"/"+kind+"?"+query.Encode(), tenant, &response); err != nil {
			return nil, err
		}
		var records []remoteTarget
		data, ok := response[kind]
		if !ok || string(data) == "null" || json.Unmarshal(data, &records) != nil {
			return nil, errors.New("invalid Xero transaction page; previous snapshot retained")
		}
		if len(records) == 0 {
			return targets, nil
		}
		for _, record := range records {
			id := record.BankTransactionID
			if kind == "Invoices" {
				id = record.InvoiceID
			}
			if id == "" || seen[id] {
				return nil, errors.New("missing or repeated Xero transaction ID; retry sync")
			}
			seen[id] = true
			if (kind == "BankTransactions" && record.Type != "SPEND") ||
				(kind == "Invoices" && record.Type != "ACCPAY") || record.Status == "DELETED" || record.Status == "VOIDED" {
				continue
			}
			target, err := record.target(tenant, kind, id)
			if err != nil {
				return nil, err
			}
			targets = append(targets, target)
		}
		// Continue until an empty page, rather than assuming that the server
		// honored pageSize. This also works with legacy 100-item pagination.
	}
	return nil, errors.New("Xero pagination limit exceeded; previous snapshot retained")
}

type remoteTarget struct {
	BankTransactionID string
	InvoiceID         string
	Type              string
	Contact           struct {
		ContactID string
		Name      string
	}
	Date           string
	DateString     string
	Total          json.Number
	CurrencyCode   string
	Reference      string
	InvoiceNumber  string
	Status         string
	IsReconciled   *bool
	HasAttachments bool
}

func (r remoteTarget) target(tenant, kind, id string) (Target, error) {
	date := r.DateString
	if date == "" {
		date = r.Date
	}
	if match := xeroDatePattern.FindStringSubmatch(date); match != nil {
		millis, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return Target{}, errors.New("invalid Xero transaction date")
		}
		date = time.UnixMilli(millis).UTC().Format("2006-01-02")
	} else if len(date) >= 10 {
		date = date[:10]
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return Target{}, errors.New("invalid Xero transaction date")
	}
	if !decimalPattern.MatchString(r.Total.String()) || len(r.CurrencyCode) != 3 || strings.ToUpper(r.CurrencyCode) != r.CurrencyCode || r.Status == "" {
		return Target{}, fmt.Errorf("invalid Xero %s amount, currency, or status; previous snapshot retained", kind)
	}
	reconciled := r.IsReconciled
	if kind == "Invoices" {
		reconciled = nil
	}
	return Target{TenantID: tenant, Kind: kind, ID: id, Type: r.Type, ContactID: r.Contact.ContactID,
		Contact: r.Contact.Name, Date: date, Total: r.Total.String(), Currency: r.CurrencyCode,
		Reference: r.Reference, Number: r.InvoiceNumber, Status: r.Status,
		Reconciled: reconciled, HasAttachments: r.HasAttachments}, nil
}
