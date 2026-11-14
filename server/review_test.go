package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/cmd/scancli"
	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

func TestManualReviewWorkflow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	conn, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close() }()
	migrations, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := oauth.NewStore(conn).Create(&oauth.OAuthClientModel{Name: "Xero", ClientID: "id", ClientSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = conn.Exec(`INSERT INTO xero_connection(id,tenant_id,tenant_name,access_token,refresh_token,expires_at,status,refreshed_at) VALUES (1,'tenant-a','Home','access','refresh',?,'connected',?)`, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	addTransaction := func(tenant, id, status string, reconciled bool, contact string) {
		t.Helper()
		_, err := conn.Exec(`INSERT INTO xero_bank_transaction (tenant_id,bank_transaction_id,type,status,is_reconciled,date,contact_name,bank_account_name,reference,currency_code,total,has_attachments,updated_date_utc,fetched_at) VALUES (?,?,'SPEND',?,?,'2025-01-02',?,'Bank','Groceries','USD','12.34',0,?,?)`, tenant, id, status, reconciled, contact, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	addTransaction("tenant-a", "good", "AUTHORISED", true, "Shop")
	addTransaction("tenant-a", "unreconciled", "AUTHORISED", false, "Shop")
	addTransaction("tenant-a", "deleted", "DELETED", true, "Shop")
	addTransaction("tenant-b", "foreign", "AUTHORISED", true, "Shop")
	addTransaction("tenant-a", "percent", "AUTHORISED", true, "100% Foods")
	dir := t.TempDir()
	filename := filepath.Join(dir, "receipt.pdf")
	data := []byte("%PDF-1.4\nreview")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := receipt.NewStore(conn)
	hash, _, err := store.Import(ctx, receipt.File{Name: "receipt.pdf", Source: filename, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Process(ctx, scancli.LocalSource{}, &testScanner{}, false); err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("https://receipts.home.arpa")
	makeServer := func() *Server {
		t.Helper()
		s, err := New(Config{DB: conn, Listen: "127.0.0.1:0", PublicURL: public, Password: "password", RequestTimeout: time.Second, XeroTimeout: time.Second, CheckInterval: time.Hour, Source: scancli.LocalSource{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := makeServer()
	cookie := ""
	request := func(method, path string, form url.Values, origin string) *httptest.ResponseRecorder {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req := httptest.NewRequest(method, path, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		if origin != "" {
			req.Header.Set("Sec-Fetch-Site", origin)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	base := "/receipts/" + hash
	if w := request("POST", base+"/decision", url.Values{"action": {"ignored"}}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated decision: %d", w.Code)
	}
	login := request("POST", "/login", url.Values{"password": {"password"}}, "")
	cookie = login.Result().Cookies()[0].String()
	if w := request("POST", base+"/decision", url.Values{"action": {"ignored"}}, "cross-site"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site decision: %d", w.Code)
	}
	post := func(path string, form url.Values, want int) {
		t.Helper()
		w := request("POST", path, form, "")
		if w.Code != want {
			t.Fatalf("POST %s got %d want %d: %s", path, w.Code, want, w.Body.String())
		}
	}
	decision := func(action, target string, want int) {
		post(base+"/decision", url.Values{"action": {action}, "transaction": {target}}, want)
	}
	correction := url.Values{"merchant": {"Corrected"}, "total": {"12.35"}, "currency": {"USD"}, "date": {"2025-01-03"}}
	post(base+"/correction", correction, http.StatusSeeOther)
	records, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].Analysis.Merchant != "Shop" || records[0].Analysis.Total != "12.34" {
		t.Fatalf("original overwritten: %+v", records[0].Analysis)
	}
	bad := url.Values{"merchant": {""}, "total": {"nan"}, "currency": {"usd"}, "date": {"2025-01-03"}}
	post(base+"/correction", bad, http.StatusBadRequest)
	for _, id := range []string{"foreign", "unreconciled", "deleted", "missing"} {
		decision("approved", id, http.StatusConflict)
	}
	w := request("GET", base+"?q=Shop", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Corrected") || !strings.Contains(w.Body.String(), "Approve selected") || !strings.Contains(w.Body.String(), "Review history") || strings.Contains(w.Body.String(), "value=\"foreign\"") {
		t.Fatalf("search/detail: %d %s", w.Code, w.Body.String())
	}
	w = request("GET", base+"?q=%25", nil, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "100% Foods") || strings.Contains(w.Body.String(), "value=\"good\"") {
		t.Fatalf("literal search: %d %s", w.Code, w.Body.String())
	}
	decision("rejected", "good", http.StatusSeeOther)
	decision("unmatched", "", http.StatusSeeOther)
	decision("approved", "good", http.StatusSeeOther)
	post(base+"/correction", correction, http.StatusConflict)
	post("/xero/disconnect", url.Values{}, http.StatusConflict)
	if w := request("GET", "/receipts?state=approved", nil, ""); !strings.Contains(w.Body.String(), "attachment pending") {
		t.Fatalf("approval not pending: %s", w.Body.String())
	}
	// Reopen the database and service as after a restart.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store = receipt.NewStore(conn)
	s = makeServer()
	cookie = ""
	login = request("POST", "/login", url.Values{"password": {"password"}}, "")
	cookie = login.Result().Cookies()[0].String()
	reviews, err := store.Reviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := reviews[hash]
	if r.State != "approved" || r.TenantID != "tenant-a" || r.TransactionID != "good" || r.Corrected.Merchant != "Corrected" {
		t.Fatalf("review after restart: %+v", r)
	}
	events, err := store.ReviewHistory(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Action != "approved" || events[0].Analysis.Total != "12.35" {
		b, _ := json.Marshal(events)
		t.Fatalf("history: %s", b)
	}
	decision("needs_review", "", http.StatusSeeOther)
	decision("ignored", "", http.StatusSeeOther)
	events, err = store.ReviewHistory(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Retention != "retain_in_intake" {
		t.Fatalf("ignore retention: %+v", events[0])
	}
	if _, err := os.Stat(filename); err != nil {
		t.Fatal("ignored source must remain", err)
	}
	post("/xero/disconnect", url.Values{}, http.StatusSeeOther)
}
