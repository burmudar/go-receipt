// Package xero maintains the Xero connection. It does not depend on HTTP handlers or CLI commands.
package xero

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
}

var DefaultEndpoints = Endpoints{
	AuthURL:        "https://login.xero.com/identity/connect/authorize",
	TokenURL:       "https://identity.xero.com/connect/token",
	ConnectionsURL: "https://api.xero.com/connections",
}

type Config struct {
	ClientID     string
	ClientSecret string
	// RedirectURL must exactly match a redirect URI registered for the Xero app.
	RedirectURL string
	Endpoints   Endpoints
	// HTTPClient is used for all Xero requests. Set a timeout on it.
	HTTPClient *http.Client
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

// isInvalidGrant reports whether Xero rejected a code or refresh token permanently.
func isInvalidGrant(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	return errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant"
}
