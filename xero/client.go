// Package xero maintains the Xero connection and the stored bank transactions.
// It does not depend on HTTP handlers or CLI commands.
package xero

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Scopes for a Xero app created on or after 2 March 2026. See docs/xero-api.md.
var Scopes = []string{"offline_access", "accounting.banktransactions.read", "accounting.attachments"}

// Endpoints are the Xero URLs. Tests replace them with a controlled server.
type Endpoints struct {
	AuthURL        string
	TokenURL       string
	ConnectionsURL string
	AccountingURL  string
}

var DefaultEndpoints = Endpoints{
	AuthURL:        "https://login.xero.com/identity/connect/authorize",
	TokenURL:       "https://identity.xero.com/connect/token",
	ConnectionsURL: "https://api.xero.com/connections",
	AccountingURL:  "https://api.xero.com/api.xro/2.0",
}

type Config struct {
	ClientID     string
	ClientSecret string
	// RedirectURL must exactly match a redirect URI registered for the Xero app.
	RedirectURL string
	Endpoints   Endpoints
	// HTTPClient is used for all Xero requests. Set a timeout on it.
	HTTPClient *http.Client
	// Logger records the steps of the connection flow. It never receives tokens, codes, or the client secret.
	Logger *slog.Logger
}

// Organisation is a Xero tenant of type ORGANISATION that the user authorised.
type Organisation struct {
	TenantID string `json:"tenantId"`
	Name     string `json:"tenantName"`
}

// errUnauthorized means that Xero rejected the access token.
var errUnauthorized = errors.New("xero rejected the access token")

type client struct {
	oauth       oauth2.Config
	http        *http.Client
	connections string
	accounting  string
}

func newClient(cfg Config) (*client, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("xero client credentials are not set; run \"rcpt oauth set\"")
	}
	if cfg.RedirectURL == "" {
		return nil, fmt.Errorf("xero redirect URL is not set")
	}
	endpoints := cfg.Endpoints
	if endpoints == (Endpoints{}) {
		endpoints = DefaultEndpoints
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &client{
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   endpoints.AuthURL,
				TokenURL:  endpoints.TokenURL,
				AuthStyle: oauth2.AuthStyleInHeader,
			},
		},
		http:        httpClient,
		connections: endpoints.ConnectionsURL,
		accounting:  endpoints.AccountingURL,
	}, nil
}

func (c *client) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, c.http)
}

func (c *client) authCodeURL(state, verifier string) string {
	return c.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

func (c *client) exchange(ctx context.Context, code, verifier string) (*oauth2.Token, error) {
	return c.oauth.Exchange(c.context(ctx), code, oauth2.VerifierOption(verifier))
}

// refresh exchanges the refresh token. Xero rotates refresh tokens, so the caller must save the result.
func (c *client) refresh(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	expired := &oauth2.Token{RefreshToken: refreshToken, Expiry: time.Unix(1, 0)}
	return c.oauth.TokenSource(c.context(ctx), expired).Token()
}

func (c *client) organisations(ctx context.Context, accessToken string) ([]Organisation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.connections, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list xero connections: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list xero connections: unexpected status %s", resp.Status)
	}

	var tenants []struct {
		Organisation
		TenantType string `json:"tenantType"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tenants); err != nil {
		return nil, fmt.Errorf("decode xero connections: %w", err)
	}

	organisations := make([]Organisation, 0, len(tenants))
	for _, tenant := range tenants {
		if tenant.TenantType == "ORGANISATION" {
			organisations = append(organisations, tenant.Organisation)
		}
	}
	return organisations, nil
}

// pageSize is the largest page that Xero returns for bank transactions.
const pageSize = 100

// bankTransactionJSON is one record of GET /BankTransactions.
type bankTransactionJSON struct {
	BankTransactionID string
	Type              string
	Status            string
	IsReconciled      bool
	Date              string // /Date(milliseconds+0000)/
	Reference         string
	CurrencyCode      string
	Total             json.Number // Keeps the exact decimal.
	HasAttachments    bool
	UpdatedDateUTC    string // /Date(milliseconds+0000)/
	Contact           struct{ Name string }
	BankAccount       struct{ Name string }
}

type bankTransactionPage struct {
	transactions []bankTransactionJSON
	// serverTime is the Xero clock when Xero sent the page. It is zero if the Date header is missing.
	serverTime time.Time
}

// rateLimitError means that Xero returned HTTP 429.
type rateLimitError struct {
	retryAfter time.Duration
	problem    string // minute, day, concurrent, or appminute
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("xero rate limit reached (%s limit); retry after %s", e.problem, e.retryAfter)
}

// bankTransactions returns one page of bank transactions of all types.
// The order by ID is stable when records change during a refresh, so paging cannot skip a record.
func (c *client) bankTransactions(ctx context.Context, accessToken, tenantID string, modifiedSince time.Time, page int) (bankTransactionPage, error) {
	query := url.Values{
		"page":     {strconv.Itoa(page)},
		"pageSize": {strconv.Itoa(pageSize)},
		"order":    {"BankTransactionID ASC"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.accounting+"/BankTransactions?"+query.Encode(), nil)
	if err != nil {
		return bankTransactionPage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("xero-tenant-id", tenantID)
	if !modifiedSince.IsZero() {
		req.Header.Set("If-Modified-Since", modifiedSince.UTC().Format(time.RFC3339))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return bankTransactionPage{}, fmt.Errorf("list xero bank transactions: %w", err)
	}
	defer resp.Body.Close()

	result := bankTransactionPage{}
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		result.serverTime = t
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return result, nil
	case http.StatusUnauthorized:
		return bankTransactionPage{}, errUnauthorized
	case http.StatusTooManyRequests:
		limit := &rateLimitError{retryAfter: time.Minute, problem: resp.Header.Get("X-Rate-Limit-Problem")}
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			limit.retryAfter = time.Duration(seconds) * time.Second
		}
		return bankTransactionPage{}, limit
	default:
		// Xero error bodies describe the problem, for example a missing scope. They do not contain tokens.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return bankTransactionPage{}, fmt.Errorf("list xero bank transactions: unexpected status %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	var body struct{ BankTransactions []bankTransactionJSON }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return bankTransactionPage{}, fmt.Errorf("decode xero bank transactions: %w", err)
	}
	result.transactions = body.BankTransactions
	return result, nil
}

// parseXeroTime parses the Xero JSON date format, for example /Date(1476316800000+0000)/.
// The number is milliseconds since the Unix epoch in UTC. The offset does not change the instant.
func parseXeroTime(s string) (time.Time, error) {
	inner, hasPrefix := strings.CutPrefix(s, "/Date(")
	inner, hasSuffix := strings.CutSuffix(inner, ")/")
	if !hasPrefix || !hasSuffix || inner == "" {
		return time.Time{}, fmt.Errorf("unexpected xero date %q", s)
	}
	if i := strings.IndexAny(inner[1:], "+-"); i >= 0 {
		inner = inner[:i+1]
	}
	ms, err := strconv.ParseInt(inner, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("unexpected xero date %q", s)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// isInvalidGrant reports whether Xero rejected a code or refresh token permanently.
func isInvalidGrant(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	return errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant"
}
