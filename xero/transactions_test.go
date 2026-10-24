package xero

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/pressly/goose/v3"
)

type fakeTransaction struct {
	typ, status string
	reconciled  bool
	total       string // JSON number
	updated     time.Time
}

// fakeAccounting serves GET /BankTransactions like the Xero Accounting API.
type fakeAccounting struct {
	mu           sync.Mutex
	now          time.Time // sent in the Date header
	transactions map[string]fakeTransaction
	// limits are returned, in order, instead of the next pages. Zero means a normal response.
	limits []int // Retry-After seconds
	// modifiedSince records the If-Modified-Since header of each request.
	modifiedSince []string
}

func (f *fakeAccounting) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path != "/BankTransactions" || r.Header.Get("Authorization") != "Bearer access" ||
		r.Header.Get("xero-tenant-id") != "tenant-a" || r.Header.Get("Accept") != "application/json" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.modifiedSince = append(f.modifiedSince, r.Header.Get("If-Modified-Since"))
	w.Header().Set("Date", f.now.Format(http.TimeFormat))
	if len(f.limits) > 0 {
		retry := f.limits[0]
		f.limits = f.limits[1:]
		if retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			w.Header().Set("X-Rate-Limit-Problem", "test")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}

	var since time.Time
	if v := r.Header.Get("If-Modified-Since"); v != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, v); err != nil {
			http.Error(w, "bad If-Modified-Since", http.StatusBadRequest)
			return
		}
	}
	var ids []string
	for id, t := range f.transactions {
		if !t.updated.Before(since) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	start, end := min((page-1)*size, len(ids)), min(page*size, len(ids))

	records := []map[string]any{}
	for _, id := range ids[start:end] {
		t := f.transactions[id]
		records = append(records, map[string]any{
			"BankTransactionID": id, "Type": t.typ, "Status": t.status, "IsReconciled": t.reconciled,
			"Date": "/Date(1759190400000+0000)/", "CurrencyCode": "ZAR", "Total": json.RawMessage(t.total),
			"UpdatedDateUTC": fmt.Sprintf("/Date(%d+0000)/", t.updated.UnixMilli()),
			"Contact":        map[string]string{"Name": "Shop"}, "BankAccount": map[string]string{"Name": "Cheque"},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"BankTransactions": records})
}

func TestRefreshBankTransactions(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO xero_connection (id, tenant_id, tenant_name, access_token, refresh_token, expires_at, status, refreshed_at)
		VALUES (1, 'tenant-a', 'Home Ltd', 'access', 'refresh', ?, 'connected', ?)`,
		time.Now().Add(time.Hour).UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	fake := &fakeAccounting{now: t0, transactions: map[string]fakeTransaction{
		"transfer": {typ: "SPEND-TRANSFER", status: "AUTHORISED", reconciled: true, total: "5", updated: t0.Add(-time.Hour)},
		"receive":  {typ: "RECEIVE", status: "AUTHORISED", total: "9007199254740993.01", updated: t0.Add(-time.Hour)},
	}}
	for i := range 150 {
		fake.transactions[fmt.Sprintf("spend-%03d", i)] = fakeTransaction{
			typ: "SPEND", status: "AUTHORISED", reconciled: true, total: "12.50", updated: t0.Add(-time.Hour)}
	}
	api := httptest.NewServer(fake)
	defer api.Close()

	m, err := NewManager(conn, Config{
		ClientID: "id", ClientSecret: "secret", RedirectURL: "http://localhost/callback",
		Endpoints: Endpoints{AuthURL: api.URL, TokenURL: api.URL, ConnectionsURL: api.URL, AccountingURL: api.URL},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := func() BankTransactionSummary {
		t.Helper()
		s, err := m.BankTransactionSummary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	stored := func() map[string]BankTransaction {
		t.Helper()
		list, err := m.BankTransactions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]BankTransaction{}
		for _, tx := range list {
			byID[tx.ID] = tx
		}
		return byID
	}

	// The daily limit stops the first refresh on page 2. Page 1 stays stored, and the next refresh is still a full refresh.
	fake.limits = []int{0, 3600}
	if err := m.RefreshBankTransactions(ctx); err == nil {
		t.Fatal("expected the daily rate limit to stop the refresh")
	}
	if s := summary(); s.Stored != 100 || !s.RefreshedAt.IsZero() || s.LastError == "" {
		t.Fatalf("summary after the stopped refresh: %+v", s)
	}

	fake.now = t0.Add(time.Minute)
	fake.modifiedSince = nil
	if err := m.RefreshBankTransactions(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.modifiedSince, []string{"", ""}) {
		t.Fatalf("a refresh after a stopped first refresh must fetch all pages without If-Modified-Since: %q", fake.modifiedSince)
	}
	all := stored()
	if s := summary(); s.Stored != 151 || s.Eligible != 150 || s.RefreshedAt.IsZero() || s.LastError != "" {
		t.Fatalf("summary after the full refresh: %+v", s)
	}
	if _, ok := all["transfer"]; ok {
		t.Fatal("a transfer was stored")
	}
	if r := all["receive"]; r.Total != "9007199254740993.01" || r.Date != "2025-09-30" || r.Eligible() {
		t.Fatalf("receive transaction: %+v", r)
	}

	// Changes in Xero after the full refresh. The daily limit stops the next refresh, and then
	// a short rate limit delays the refresh after it.
	changed := fake.now.Add(time.Hour)
	fake.transactions["spend-005"] = fakeTransaction{typ: "SPEND", status: "VOIDED", reconciled: true, total: "12.50", updated: changed}
	fake.transactions["receive"] = fakeTransaction{typ: "RECEIVE", status: "AUTHORISED", reconciled: true, total: "9007199254740993.01", updated: changed}
	fake.transactions["spend-new"] = fakeTransaction{typ: "SPEND", status: "AUTHORISED", reconciled: true, total: "99.99", updated: changed}
	fake.now = changed.Add(time.Hour)
	fake.modifiedSince = nil
	fake.limits = []int{3600}
	if err := m.RefreshBankTransactions(ctx); err == nil {
		t.Fatal("expected the daily rate limit to stop the refresh")
	}
	fake.now = changed.Add(2 * time.Hour)
	fake.limits = []int{1}
	if err := m.RefreshBankTransactions(ctx); err != nil {
		t.Fatal(err)
	}
	// Both refreshes start at the Xero time of the last complete refresh, so the stopped refresh loses no change.
	want := t0.Add(time.Minute).Format(time.RFC3339)
	if !slices.Equal(fake.modifiedSince, []string{want, want, want}) {
		t.Fatalf("If-Modified-Since: got %q, want %q for the stopped request, the rate-limited request, and its retry", fake.modifiedSince, want)
	}
	all = stored()
	if s := summary(); s.Stored != 152 || s.Eligible != 151 {
		t.Fatalf("summary after the incremental refresh: %+v", s)
	}
	if v := all["spend-005"]; v.Status != "VOIDED" || v.Eligible() {
		t.Fatalf("voided transaction: %+v", v)
	}
	if !all["receive"].Eligible() || all["spend-new"].Total != "99.99" {
		t.Fatalf("changed transactions: %+v %+v", all["receive"], all["spend-new"])
	}
}
