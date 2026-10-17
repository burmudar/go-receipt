package xero

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/cmd/oauthcli"
	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/pressly/goose/v3"
)

type loginOutput struct{ url chan string }

func (w loginOutput) Write(p []byte) (int, error) {
	w.url <- strings.TrimSpace(strings.SplitN(string(p), "\n", 2)[1])
	return len(p), nil
}

// One workflow regression uses real SQLite, a browser callback, and controlled
// Xero endpoints. It does not establish compatibility with a live organisation.
func TestConnectionSurvivesRestartAndSyncFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	database := filepath.Join(t.TempDir(), "app.db")
	conn, err := db.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close() }()
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	redirect, _ := url.Parse("http://" + listener.Addr().String() + "/callback")
	credentials := oauth.OAuthClientModel{Name: "Xero", ClientID: "client", ClientSecret: "private-client-secret", RedirectURI: redirect.String()}
	if err := oauth.NewStore(conn).Create(&credentials); err != nil {
		t.Fatal(err)
	}

	var exchanges, refreshes, bankPages atomic.Int32
	var rateLimited, invalidGrant, revoked, rejectAccess atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			id, secret, ok := r.BasicAuth()
			if !ok || id != "client" || secret != "private-client-secret" {
				t.Error("missing token client authentication")
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				exchanges.Add(1)
				if r.Form.Get("code") != "private-code" || r.Form.Get("redirect_uri") != redirect.String() {
					t.Error("incorrect code exchange")
				}
			case "refresh_token":
				previous := refreshes.Add(1) - 1
				if r.Form.Get("refresh_token") != fmt.Sprintf("private-refresh-%d", previous) {
					t.Error("rotated refresh token was not used")
				}
				if invalidGrant.Load() {
					w.WriteHeader(400)
					io.WriteString(w, `{"error":"invalid_grant","error_description":"private-remote-error"}`)
					return
				}
			default:
				t.Error("unexpected grant type")
			}
			fmt.Fprintf(w, `{"access_token":"private-access-%d","refresh_token":"private-refresh-%d","expires_in":1800,"token_type":"Bearer"}`, refreshes.Load(), refreshes.Load())
			return
		}
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer private-access-%d", refreshes.Load()) {
			t.Error("incorrect access token")
		}
		if rejectAccess.Swap(false) {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/connections" {
			if revoked.Load() {
				io.WriteString(w, `[]`)
				return
			}
			io.WriteString(w, `[{"id":"connection-a","tenantId":"tenant-a","tenantName":"Organisation A","tenantType":"ORGANISATION"},
				{"id":"connection-b","tenantId":"tenant-b","tenantName":"Organisation B","tenantType":"ORGANISATION"}]`)
			return
		}
		if r.Header.Get("xero-tenant-id") != "tenant-a" {
			t.Error("wrong selected tenant")
		}
		if r.URL.Query().Get("pageSize") != "100" {
			t.Error("missing page size")
		}
		page := r.URL.Query().Get("page")
		switch r.URL.Path {
		case "/BankTransactions":
			bankPages.Add(1)
			switch page {
			case "1":
				io.WriteString(w, `{"BankTransactions":[{"BankTransactionID":"bank-1","Type":"SPEND","Contact":{"ContactID":"contact-1","Name":"Shop"},"Date":"/Date(1790812800000+0000)/","Total":9007199254740993.01,"CurrencyCode":"ZAR","Reference":"Card","Status":"AUTHORISED","IsReconciled":false,"HasAttachments":true}]}`)
			case "2":
				if rateLimited.Load() {
					w.Header().Set("Retry-After", "75")
					w.WriteHeader(429)
					io.WriteString(w, `private-remote-error`)
					return
				}
				io.WriteString(w, `{"BankTransactions":[{"BankTransactionID":"bank-2","Type":"SPEND","DateString":"2026-10-02T00:00:00","Total":12.30,"CurrencyCode":"ZAR","Status":"AUTHORISED"},{"BankTransactionID":"receive-1","Type":"RECEIVE"}]}`)
			case "3":
				io.WriteString(w, `{"BankTransactions":[]}`)
			default:
				t.Error("unexpected bank page")
				w.WriteHeader(500)
			}
		case "/Invoices":
			if page == "1" {
				io.WriteString(w, `{"Invoices":[{"InvoiceID":"bill-1","Type":"ACCPAY","DateString":"2026-10-03T00:00:00","Total":99.99,"CurrencyCode":"ZAR","InvoiceNumber":"INV-1","Status":"PAID"},{"InvoiceID":"void-1","Type":"ACCPAY","Status":"VOIDED"},{"InvoiceID":"sale-1","Type":"ACCREC"}]}`)
			} else {
				io.WriteString(w, `{"Invoices":[]}`)
			}
		default:
			t.Error("unexpected API path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	newClient := func() *Client {
		c := NewClient(NewStore(conn), credentials)
		c.http = server.Client()
		c.tokenURL, c.connectionsURL, c.apiURL = server.URL+"/token", server.URL+"/connections", server.URL
		return c
	}
	client := newClient()
	output := loginOutput{url: make(chan string, 1)}
	loginDone := make(chan error, 1)
	go func() { loginDone <- client.login(ctx, listener, redirect, output) }()
	var authURL string
	select {
	case authURL = <-output.url:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	auth, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := auth.Query().Get("state")
	if len(state) < 32 || auth.Query().Get("scope") != scopes {
		t.Fatal("missing state or read-only scopes")
	}
	callback := func(state string) string {
		t.Helper()
		u := *redirect
		u.RawQuery = url.Values{"state": {state}, "code": {"private-code"}}.Encode()
		resp, err := http.Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "private-") {
			t.Fatal("callback leaked a secret")
		}
		return string(body)
	}
	callback("wrong-state")
	if exchanges.Load() != 0 {
		t.Fatal("invalid state reached token endpoint")
	}
	callback(state)
	if err := <-loginDone; err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 1 {
		t.Fatal("login did not exchange one code")
	}
	if err := client.Select(ctx, "missing"); err == nil {
		t.Fatal("selected an unconnected tenant")
	}
	if err := client.Select(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if count, err := client.Sync(ctx); err != nil || count != 3 {
		t.Fatalf("sync: count=%d err=%v", count, err)
	}
	if bankPages.Load() != 3 {
		t.Fatal("did not traverse all bank pages")
	}

	// Expire the token, close the database, and create a new client. Refresh must
	// retain the tenant and save the new token pair before fetching transactions.
	if _, err := conn.Exec(`UPDATE xero_auth SET expires_at=0`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = db.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	client = newClient()
	if count, err := client.Sync(ctx); err != nil || count != 3 {
		t.Fatalf("sync after restart: count=%d err=%v", count, err)
	}
	g, err := client.store.load(ctx)
	if err != nil || g.RefreshToken != "private-refresh-1" || g.ConnectionID != "connection-a" {
		t.Fatal("rotated grant or selection was not persisted")
	}
	targets, err := client.store.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || targets[0].Total != "9007199254740993.01" || targets[0].Date != "2026-10-01" || targets[0].Reconciled == nil || *targets[0].Reconciled || targets[1].Total != "12.30" || targets[2].Number != "INV-1" {
		t.Fatalf("incorrect target data: %+v", targets)
	}

	before, err := client.store.Status(ctx)
	if err != nil || before.LastSync == nil {
		t.Fatal("missing sync status")
	}
	rateLimited.Store(true)
	_, err = client.Sync(ctx)
	var limit *RateLimitError
	if !errors.As(err, &limit) || limit.RetryAfter != 75*time.Second {
		t.Fatalf("rate limit not surfaced: %v", err)
	}
	after, err := client.store.Status(ctx)
	if err != nil || !after.LastSync.Equal(*before.LastSync) {
		t.Fatal("failed sync changed timestamp")
	}
	retained, err := client.store.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldJSON, _ := json.Marshal(targets)
	newJSON, _ := json.Marshal(retained)
	if !bytes.Equal(oldJSON, newJSON) {
		t.Fatal("partial sync replaced the previous snapshot")
	}
	rateLimited.Store(false)

	if err := client.Select(ctx, "tenant-b"); err != nil {
		t.Fatal(err)
	}
	if targets, err := client.store.Targets(ctx); err != nil || len(targets) != 0 {
		t.Fatal("targets leaked across tenants")
	}
	if err := client.Select(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	rejectAccess.Store(true)
	if _, err := client.Connections(ctx); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 2 {
		t.Fatal("401 did not refresh once")
	}

	// Missing remote connection and expired refresh grants both persist a clear
	// reconnect requirement instead of exposing raw upstream errors.
	revoked.Store(true)
	if _, err := client.Sync(ctx); !errors.Is(err, ErrReconnect) {
		t.Fatalf("revoked connection: %v", err)
	}
	status, err := client.store.Status(ctx)
	if err != nil || !status.ReconnectRequired || status.Connected {
		t.Fatal("reconnect status was not saved")
	}
	revoked.Store(false)
	g, err = client.store.load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g.Reconnect, g.ExpiresAt = false, 0
	if err := client.store.save(ctx, g); err != nil {
		t.Fatal(err)
	}
	invalidGrant.Store(true)
	if _, err := client.Connections(ctx); !errors.Is(err, ErrReconnect) {
		t.Fatalf("expired refresh grant: %v", err)
	}

	// Public output must not include the OAuth client secret or either token.
	command := oauthcli.Cmd()
	var public bytes.Buffer
	command.SetOut(&public)
	command.SetArgs([]string{"--db", database, "get", "--name", "Xero"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = client.store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(&public).Encode(status); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(public.String(), "private-") {
		t.Fatal("public output contains credentials")
	}
	credentials.ClientSecret = "replacement-secret"
	if err := oauth.NewStore(conn).Create(&credentials); err != nil {
		t.Fatal(err)
	}
	if _, err := client.store.load(ctx); !errors.Is(err, ErrReconnect) {
		t.Fatal("credential change retained the previous grant")
	}
}
