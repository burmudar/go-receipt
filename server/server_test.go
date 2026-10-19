package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/burmudar/go-receipt/xero"
	"github.com/pressly/goose/v3"
)

// fakeXero issues tokens, rotates refresh tokens, and lists connections like the Xero identity and API servers.
type fakeXero struct {
	mu            sync.Mutex
	challenge     string
	refreshTokens map[string]int // refresh token -> generation of the next token pair
	accessToken   string
	revoked       bool
}

func (f *fakeXero) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/token":
		if id, secret, _ := r.BasicAuth(); id != "client-id" || secret != "client-secret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		var generation int
		switch r.PostFormValue("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
			if r.PostFormValue("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
				writeInvalidGrant(w)
				return
			}
			generation = 1
		case "refresh_token":
			next, ok := f.refreshTokens[r.PostFormValue("refresh_token")]
			if !ok || f.revoked {
				writeInvalidGrant(w)
				return
			}
			// Xero rotates refresh tokens: the previous token stops working.
			delete(f.refreshTokens, r.PostFormValue("refresh_token"))
			generation = next
		}
		refresh := "refresh-token-" + string(rune('0'+generation))
		f.refreshTokens[refresh] = generation + 1
		f.accessToken = "access-token-" + string(rune('0'+generation))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": f.accessToken, "refresh_token": refresh, "expires_in": 1800, "token_type": "Bearer",
		})
	case "/connections":
		if r.Header.Get("Authorization") != "Bearer "+f.accessToken || f.revoked {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[
			{"tenantId":"tenant-a","tenantType":"ORGANISATION","tenantName":"Home Ltd"},
			{"tenantId":"tenant-b","tenantType":"ORGANISATION","tenantName":"Other Ltd"},
			{"tenantId":"practice","tenantType":"PRACTICE","tenantName":"Practice"}]`)
	default:
		http.NotFound(w, r)
	}
}

func writeInvalidGrant(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	io.WriteString(w, `{"error":"invalid_grant"}`)
}

// Connect Xero through the browser flow, refresh after a restart, and require a reconnect after revocation.
func TestXeroConnectionThroughBrowser(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "app.db")
	conn, err := db.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := oauth.NewStore(conn).Create(&oauth.OAuthClientModel{Name: "Xero", ClientID: "client-id", ClientSecret: "client-secret"}); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	fake := &fakeXero{refreshTokens: map[string]int{}}
	xeroServer := httptest.NewServer(fake)
	defer xeroServer.Close()

	public, _ := url.Parse("https://receipts.home.arpa")
	cfg := Config{
		Listen:         "127.0.0.1:0",
		PublicURL:      public,
		RequestTimeout: time.Minute,
		XeroTimeout:    5 * time.Second,
		CheckInterval:  time.Hour,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		XeroEndpoints: xero.Endpoints{
			AuthURL:        xeroServer.URL + "/authorize",
			TokenURL:       xeroServer.URL + "/token",
			ConnectionsURL: xeroServer.URL + "/connections",
		},
	}

	// start opens a new database connection and server, as after a process restart.
	start := func() (*Server, func()) {
		t.Helper()
		conn, err := db.Open(database)
		if err != nil {
			t.Fatal(err)
		}
		c := cfg
		c.DB, c.Password = conn, "correct horse"
		srv, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		return srv, func() { conn.Close() }
	}

	cfg.DB = nil
	if _, err := New(cfg); err == nil {
		t.Fatal("expected a password to be required behind a home-network public URL")
	}

	var bodies strings.Builder
	var cookie string
	do := func(srv *Server, method, target string, form url.Values, headers ...string) *http.Response {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req := httptest.NewRequest(method, target, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		bodies.WriteString(rec.Body.String())
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie {
				cookie = c.Name + "=" + c.Value
			}
		}
		return rec.Result()
	}
	expect := func(resp *http.Response, status int, location string) {
		t.Helper()
		if resp.StatusCode != status || (location != "" && resp.Header.Get("Location") != location) {
			t.Fatalf("got %d %q, want %d %q", resp.StatusCode, resp.Header.Get("Location"), status, location)
		}
	}
	statusPage := func(srv *Server) string {
		t.Helper()
		resp := do(srv, http.MethodGet, "/", nil)
		expect(resp, http.StatusOK, "")
		page, _ := io.ReadAll(resp.Body)
		return string(page)
	}

	srv, stop := start()
	expect(do(srv, http.MethodGet, "/", nil), http.StatusSeeOther, "/login")
	expect(do(srv, http.MethodPost, "/xero/connect", url.Values{}), http.StatusUnauthorized, "")
	expect(do(srv, http.MethodPost, "/login", url.Values{"password": {"correct horse"}}), http.StatusSeeOther, "/")
	expect(do(srv, http.MethodPost, "/xero/connect", url.Values{}, "Sec-Fetch-Site", "cross-site"), http.StatusForbidden, "")

	resp := do(srv, http.MethodPost, "/xero/connect", url.Values{})
	expect(resp, http.StatusSeeOther, "")
	authorize, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	if query.Get("redirect_uri") != "https://receipts.home.arpa/xero/callback" ||
		query.Get("scope") != strings.Join(xero.Scopes, " ") || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected authorisation request: %s", authorize)
	}
	fake.challenge = query.Get("code_challenge")

	expect(do(srv, http.MethodGet, "/xero/callback?state=forged&code=good-code", nil), http.StatusBadRequest, "")
	callback := "/xero/callback?" + url.Values{"state": {query.Get("state")}, "code": {"good-code"}}.Encode()
	expect(do(srv, http.MethodGet, callback, nil), http.StatusSeeOther, "/xero/organisations")
	expect(do(srv, http.MethodGet, callback, nil), http.StatusBadRequest, "") // A state is used once.

	resp = do(srv, http.MethodGet, "/xero/organisations", nil)
	expect(resp, http.StatusOK, "")
	if page, _ := io.ReadAll(resp.Body); !strings.Contains(string(page), "Other Ltd") || strings.Contains(string(page), "Practice") {
		t.Fatalf("organisation page does not list only organisations:\n%s", page)
	}
	expect(do(srv, http.MethodPost, "/xero/organisations", url.Values{"tenant_id": {"tenant-a"}}), http.StatusSeeOther, "/")
	if page := statusPage(srv); !strings.Contains(page, "Connected") || !strings.Contains(page, "Home Ltd") {
		t.Fatalf("status page does not show the connection:\n%s", page)
	}
	stop()

	// Restart with an expired access token. The check refreshes and saves the rotated refresh token.
	expire := func() {
		t.Helper()
		conn, err := db.Open(database)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Exec("UPDATE xero_connection SET expires_at = ?", time.Now().Add(-time.Minute).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	srv, stop = start()
	cookie = "" // The session key changes on restart.
	expect(do(srv, http.MethodPost, "/login", url.Values{"password": {"correct horse"}}), http.StatusSeeOther, "/")
	expect(do(srv, http.MethodPost, "/xero/check", url.Values{}), http.StatusSeeOther, "/")
	connection, err := srv.xero.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Status != xero.StatusConnected || connection.CheckedAt.IsZero() || connection.LastError != "" {
		t.Fatalf("connection after refresh: %+v", connection)
	}
	var stored string
	if err := srv.cfg.DB.QueryRow("SELECT refresh_token FROM xero_connection").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.refreshTokens[stored]; !ok || stored != "refresh-token-2" {
		t.Fatalf("stored refresh token %q is not the rotated token", stored)
	}
	stop()

	// Revoke the connection in Xero. The next refresh requires a reconnect, and the browser shows it.
	fake.revoked = true
	expire()
	srv, stop = start()
	defer stop()
	cookie = ""
	expect(do(srv, http.MethodPost, "/login", url.Values{"password": {"correct horse"}}), http.StatusSeeOther, "/")
	expect(do(srv, http.MethodPost, "/xero/check", url.Values{}), http.StatusSeeOther, "/")
	if page := statusPage(srv); !strings.Contains(page, "Reconnect required") || !strings.Contains(page, "Reconnect to Xero") {
		t.Fatalf("status page does not require a reconnect:\n%s", page)
	}

	for _, secret := range []string{"client-secret", "access-token-", "refresh-token-"} {
		if strings.Contains(bodies.String(), secret) {
			t.Fatalf("a response contains %q", secret)
		}
	}
}
