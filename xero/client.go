package xero

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/burmudar/go-receipt/db/oauth"
)

const scopes = "offline_access accounting.banktransactions.read accounting.invoices.read"

// Client serializes grant rotation within one process. Run only one Xero command
// per database at a time. HTTP endpoints are fixed in production.
type Client struct {
	store          *Store
	credentials    oauth.OAuthClientModel
	http           *http.Client
	authorizeURL   string
	tokenURL       string
	connectionsURL string
	apiURL         string
	mu             sync.Mutex
}

func NewClient(store *Store, credentials oauth.OAuthClientModel) *Client {
	return &Client{store: store, credentials: credentials,
		http:           &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		authorizeURL:   "https://login.xero.com/identity/connect/authorize",
		tokenURL:       "https://identity.xero.com/connect/token",
		connectionsURL: "https://api.xero.com/connections", apiURL: "https://api.xero.com/api.xro/2.0"}
}

// RateLimitError returns the server delay without sleeping or retrying early.
// The caller can retry the whole read-only sync after this delay.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("Xero rate limit: retry after %s", e.RetryAfter)
}

func responseError(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := time.Minute
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 && seconds <= 86400*7 {
			delay = time.Duration(seconds) * time.Second
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil && time.Until(at) > 0 {
			delay = time.Until(at)
		}
		return &RateLimitError{RetryAfter: delay}
	}
	if resp.StatusCode == http.StatusForbidden {
		return errors.New("Xero access denied: check organisation permissions and reconnect with the required scopes")
	}
	// Do not include remote response bodies, request URLs, or credentials.
	return fmt.Errorf("Xero request failed (HTTP %d)", resp.StatusCode)
}

func (c *Client) exchange(ctx context.Context, values url.Values, previous grant) (grant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return grant{}, errors.New("invalid Xero token endpoint")
	}
	req.SetBasicAuth(c.credentials.ClientID, c.credentials.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return grant{}, errors.New("Xero token request failed; check network access and retry")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			var failure struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&failure)
			if failure.Error == "invalid_grant" && values.Get("grant_type") == "refresh_token" {
				return grant{}, c.store.reconnect(ctx)
			}
			if failure.Error == "invalid_grant" {
				return grant{}, errors.New("Xero login expired or was rejected; run xero login again")
			}
		}
		return grant{}, responseError(resp)
	}
	var token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
		Type    string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return grant{}, errors.New("invalid Xero token response; retry login or refresh")
	}
	if token.Access == "" || token.Refresh == "" || token.Expires <= 0 || token.Expires > 86400 || !strings.EqualFold(token.Type, "Bearer") {
		return grant{}, errors.New("incomplete Xero token response; retry login or refresh")
	}
	previous.AccessToken, previous.RefreshToken = token.Access, token.Refresh
	previous.ExpiresAt = time.Now().Unix() + token.Expires
	previous.Reconnect = false
	// Persist both rotated tokens before any subsequent API request.
	if err := c.store.save(ctx, previous); err != nil {
		return grant{}, err
	}
	return previous, nil
}

func (c *Client) access(ctx context.Context, force bool) (grant, error) {
	g, err := c.store.load(ctx)
	if err != nil {
		return g, err
	}
	if g.Reconnect {
		return g, ErrReconnect
	}
	if force || g.ExpiresAt <= time.Now().Add(time.Minute).Unix() {
		return c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g.RefreshToken}}, g)
	}
	return g, nil
}

func (c *Client) get(ctx context.Context, endpoint, tenant string, result any) error {
	for attempt := 0; attempt < 2; attempt++ {
		g, err := c.access(ctx, attempt == 1)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return errors.New("invalid Xero API endpoint")
		}
		req.Header.Set("Authorization", "Bearer "+g.AccessToken)
		req.Header.Set("Accept", "application/json")
		if tenant != "" {
			req.Header.Set("xero-tenant-id", tenant)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return errors.New("Xero API request failed; check network access and retry")
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			if attempt == 0 {
				continue
			}
			return c.store.reconnect(ctx)
		}
		if resp.StatusCode != http.StatusOK {
			err := responseError(resp)
			resp.Body.Close()
			return err
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(result)
		resp.Body.Close()
		if err != nil {
			return errors.New("invalid Xero API response; retry sync")
		}
		return nil
	}
	return ErrReconnect
}

func (c *Client) connections(ctx context.Context) ([]Connection, error) {
	var connections []Connection
	if err := c.get(ctx, c.connectionsURL, "", &connections); err != nil {
		return nil, err
	}
	orgs := []Connection{}
	for _, connection := range connections {
		if connection.TenantType == "ORGANISATION" && connection.ID != "" && connection.TenantID != "" {
			orgs = append(orgs, connection)
		}
	}
	return orgs, nil
}

func (c *Client) Connections(ctx context.Context) ([]Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	connections, err := c.connections(ctx)
	if err != nil {
		return nil, err
	}
	g, err := c.store.load(ctx)
	if err != nil {
		return nil, err
	}
	if g.TenantID != "" {
		for _, connection := range connections {
			if connection.TenantID == g.TenantID && connection.ID == g.ConnectionID {
				return connections, nil
			}
		}
		return nil, c.store.reconnect(ctx)
	}
	return connections, nil
}

func (c *Client) Select(ctx context.Context, tenant string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	connections, err := c.connections(ctx)
	if err != nil {
		return err
	}
	for _, connection := range connections {
		if connection.TenantID == tenant {
			g, err := c.store.load(ctx)
			if err != nil {
				return err
			}
			g.ConnectionID, g.TenantID, g.TenantName = connection.ID, connection.TenantID, connection.TenantName
			return c.store.save(ctx, g)
		}
	}
	return errors.New("Xero organisation is not connected; check xero connections or run xero login")
}
