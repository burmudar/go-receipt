package xero

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

type Status string

const (
	StatusNotConnected       Status = "not_connected"
	StatusSelectOrganisation Status = "select_organisation"
	StatusConnected          Status = "connected"
	StatusReconnectRequired  Status = "reconnect_required"
)

var (
	ErrNotConnected      = errors.New("xero is not connected")
	ErrReconnectRequired = errors.New("xero connection must be reconnected")
	ErrInvalidState      = errors.New("xero authorisation request is unknown or expired; start the connection again")
)

const (
	stateLifetime = 10 * time.Minute
	// Refresh before expiry so that a request does not start with an access token that expires during the request.
	refreshMargin = 2 * time.Minute
)

// Connection is the persisted connection state. It does not contain tokens.
type Connection struct {
	Status      Status
	TenantID    string
	TenantName  string
	ExpiresAt   time.Time
	RefreshedAt time.Time
	CheckedAt   time.Time
	LastError   string
}

type tokens struct {
	tenantID     string
	tenantName   string
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	status       Status
}

// Manager owns the single Xero connection. All token refreshes go through one Manager,
// so that concurrent callers cannot use an old refresh token after Xero rotates it.
// Use only one Manager for each database.
type Manager struct {
	db     *sql.DB
	client *client
	now    func() time.Time
	mu     sync.Mutex
}

func NewManager(db *sql.DB, cfg Config) (*Manager, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Manager{db: db, client: c, now: time.Now}, nil
}

// Start records a new authorisation request and returns the Xero URL for the browser.
func (m *Manager) Start(ctx context.Context) (string, error) {
	state, err := randomString()
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()
	now := m.now().UTC()

	if _, err := m.db.ExecContext(ctx, "DELETE FROM xero_oauth_state WHERE created_at < ?", now.Add(-stateLifetime)); err != nil {
		return "", fmt.Errorf("remove expired xero authorisation requests: %w", err)
	}
	if _, err := m.db.ExecContext(ctx,
		"INSERT INTO xero_oauth_state (state, verifier, created_at) VALUES (?, ?, ?)",
		state, verifier, now,
	); err != nil {
		return "", fmt.Errorf("save xero authorisation request: %w", err)
	}
	return m.client.authCodeURL(state, verifier), nil
}

// Complete validates the callback state, exchanges the code, and saves the tokens.
// If the user authorises exactly one organisation and no organisation is selected, it is selected.
// A reconnect must authorise the organisation that is already selected.
func (m *Manager) Complete(ctx context.Context, state, code string) (Status, error) {
	if state == "" || code == "" {
		return "", ErrInvalidState
	}

	var verifier string
	var createdAt time.Time
	err := m.db.QueryRowContext(ctx,
		"DELETE FROM xero_oauth_state WHERE state = ? RETURNING verifier, created_at", state,
	).Scan(&verifier, &createdAt)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && m.now().Sub(createdAt) > stateLifetime) {
		return "", ErrInvalidState
	}
	if err != nil {
		return "", fmt.Errorf("read xero authorisation request: %w", err)
	}

	token, err := m.client.exchange(ctx, code, verifier)
	if err != nil {
		return "", fmt.Errorf("exchange xero authorisation code: %w", err)
	}
	if token.RefreshToken == "" {
		return "", fmt.Errorf("xero did not return a refresh token; check that the offline_access scope is granted")
	}
	organisations, err := m.client.organisations(ctx, token.AccessToken)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	current, err := m.load(ctx)
	if err != nil && !errors.Is(err, ErrNotConnected) {
		return "", err
	}

	next := tokens{
		accessToken:  token.AccessToken,
		refreshToken: token.RefreshToken,
		expiresAt:    token.Expiry.UTC(),
		status:       StatusSelectOrganisation,
	}
	switch {
	case current.tenantID != "":
		i := slices.IndexFunc(organisations, func(o Organisation) bool { return o.TenantID == current.tenantID })
		if i < 0 {
			message := fmt.Sprintf("reconnect and authorise the organisation %q; disconnect first to change the organisation", current.tenantName)
			if err := m.setStatus(ctx, StatusReconnectRequired, message); err != nil {
				return "", err
			}
			return StatusReconnectRequired, errors.New(message)
		}
		next.tenantID, next.tenantName, next.status = current.tenantID, organisations[i].Name, StatusConnected
	case len(organisations) == 0:
		return "", fmt.Errorf("no xero organisation was authorised")
	case len(organisations) == 1:
		next.tenantID, next.tenantName, next.status = organisations[0].TenantID, organisations[0].Name, StatusConnected
	}

	if err := m.save(ctx, next); err != nil {
		return "", err
	}
	return next.status, nil
}

// Organisations lists the organisations that the current authorisation can access.
func (m *Manager) Organisations(ctx context.Context) ([]Organisation, error) {
	t, err := m.token(ctx)
	if err != nil {
		return nil, err
	}
	organisations, err := m.client.organisations(ctx, t.accessToken)
	if errors.Is(err, errUnauthorized) {
		return nil, m.requireReconnect(ctx, err)
	}
	return organisations, err
}

// Select saves the organisation. It is allowed only before an organisation is selected.
func (m *Manager) Select(ctx context.Context, tenantID string) error {
	organisations, err := m.Organisations(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(organisations, func(o Organisation) bool { return o.TenantID == tenantID })
	if i < 0 {
		return fmt.Errorf("the organisation is not authorised for this connection")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	result, err := m.db.ExecContext(ctx, `
		UPDATE xero_connection SET tenant_id = ?, tenant_name = ?, status = ?, last_error = ''
		WHERE id = 1 AND status = ?`,
		tenantID, organisations[i].Name, StatusConnected, StatusSelectOrganisation,
	)
	if err != nil {
		return fmt.Errorf("save xero organisation: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("an organisation is already selected; disconnect first to change the organisation")
	}
	return nil
}

// Connection returns the persisted state without contacting Xero.
func (m *Manager) Connection(ctx context.Context) (Connection, error) {
	var c Connection
	var tenantID sql.NullString
	var checkedAt sql.NullTime
	err := m.db.QueryRowContext(ctx, `
		SELECT status, tenant_id, tenant_name, expires_at, refreshed_at, checked_at, last_error
		FROM xero_connection WHERE id = 1`,
	).Scan(&c.Status, &tenantID, &c.TenantName, &c.ExpiresAt, &c.RefreshedAt, &checkedAt, &c.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{Status: StatusNotConnected}, nil
	}
	if err != nil {
		return Connection{}, fmt.Errorf("read xero connection: %w", err)
	}
	c.TenantID, c.CheckedAt = tenantID.String, checkedAt.Time
	return c, nil
}

// AccessToken returns a valid access token and the selected tenant ID for Xero API requests.
// The caller must not log or display the token.
func (m *Manager) AccessToken(ctx context.Context) (accessToken, tenantID string, err error) {
	t, err := m.token(ctx)
	if err != nil {
		return "", "", err
	}
	if t.status != StatusConnected {
		return "", "", fmt.Errorf("select a xero organisation first")
	}
	return t.accessToken, t.tenantID, nil
}

// Check refreshes the access token when necessary and confirms that the selected organisation is still connected.
// Run it at least once in each 60-day period, because an unused refresh token expires after 60 days.
func (m *Manager) Check(ctx context.Context) error {
	accessToken, tenantID, err := m.AccessToken(ctx)
	if err != nil {
		return err
	}
	organisations, err := m.client.organisations(ctx, accessToken)
	if errors.Is(err, errUnauthorized) {
		return m.requireReconnect(ctx, err)
	}
	if err != nil {
		return m.recordError(ctx, err)
	}
	if !slices.ContainsFunc(organisations, func(o Organisation) bool { return o.TenantID == tenantID }) {
		return m.requireReconnect(ctx, errors.New("the selected organisation is no longer connected"))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	_, err = m.db.ExecContext(ctx,
		"UPDATE xero_connection SET checked_at = ?, last_error = '' WHERE id = 1 AND status = ?",
		m.now().UTC(), StatusConnected,
	)
	return err
}

// Disconnect removes the local connection. The user can then connect a different organisation.
func (m *Manager) Disconnect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.db.ExecContext(ctx, "DELETE FROM xero_connection WHERE id = 1")
	return err
}

// token returns the stored tokens and refreshes them when the access token expires soon.
// Only this method refreshes tokens. The mutex makes sure that one refresh runs at a time.
func (m *Manager) token(ctx context.Context) (tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, err := m.load(ctx)
	if err != nil {
		return tokens{}, err
	}
	if t.status == StatusReconnectRequired {
		return tokens{}, ErrReconnectRequired
	}
	if m.now().Add(refreshMargin).Before(t.expiresAt) {
		return t, nil
	}

	refreshed, err := m.client.refresh(ctx, t.refreshToken)
	if isInvalidGrant(err) {
		return tokens{}, m.setReconnect(ctx, err)
	}
	if err != nil {
		// Keep the previous refresh token. Xero accepts it again for 30 minutes if the response was lost.
		if saveErr := m.setError(ctx, fmt.Sprintf("refresh xero access token: %v", err)); saveErr != nil {
			return tokens{}, errors.Join(err, saveErr)
		}
		return tokens{}, fmt.Errorf("refresh xero access token: %w", err)
	}

	t.accessToken, t.expiresAt = refreshed.AccessToken, refreshed.Expiry.UTC()
	if refreshed.RefreshToken != "" {
		t.refreshToken = refreshed.RefreshToken
	}
	if err := m.save(ctx, t); err != nil {
		return tokens{}, err
	}
	return t, nil
}

func (m *Manager) load(ctx context.Context) (tokens, error) {
	var t tokens
	var tenantID sql.NullString
	err := m.db.QueryRowContext(ctx, `
		SELECT tenant_id, tenant_name, access_token, refresh_token, expires_at, status
		FROM xero_connection WHERE id = 1`,
	).Scan(&tenantID, &t.tenantName, &t.accessToken, &t.refreshToken, &t.expiresAt, &t.status)
	if errors.Is(err, sql.ErrNoRows) {
		return tokens{}, ErrNotConnected
	}
	if err != nil {
		return tokens{}, fmt.Errorf("read xero connection: %w", err)
	}
	t.tenantID = tenantID.String
	return t, nil
}

// save writes the tokens in one statement, so that the rotated refresh token and the new access token are saved together.
func (m *Manager) save(ctx context.Context, t tokens) error {
	var tenantID any
	if t.tenantID != "" {
		tenantID = t.tenantID
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO xero_connection (id, tenant_id, tenant_name, access_token, refresh_token, expires_at, status, last_error, refreshed_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, '', ?)
		ON CONFLICT (id) DO UPDATE SET
		tenant_id = excluded.tenant_id,
		tenant_name = excluded.tenant_name,
		access_token = excluded.access_token,
		refresh_token = excluded.refresh_token,
		expires_at = excluded.expires_at,
		status = excluded.status,
		last_error = '',
		refreshed_at = excluded.refreshed_at`,
		tenantID, t.tenantName, t.accessToken, t.refreshToken, t.expiresAt, t.status, m.now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("save xero connection: %w", err)
	}
	return nil
}

func (m *Manager) requireReconnect(ctx context.Context, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setReconnect(ctx, cause)
}

func (m *Manager) setReconnect(ctx context.Context, cause error) error {
	if err := m.setStatus(ctx, StatusReconnectRequired, cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("%w: %v", ErrReconnectRequired, cause)
}

func (m *Manager) recordError(ctx context.Context, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.setError(ctx, cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (m *Manager) setStatus(ctx context.Context, status Status, message string) error {
	_, err := m.db.ExecContext(ctx,
		"UPDATE xero_connection SET status = ?, last_error = ? WHERE id = 1", status, message)
	if err != nil {
		return fmt.Errorf("save xero connection status: %w", err)
	}
	return nil
}

func (m *Manager) setError(ctx context.Context, message string) error {
	_, err := m.db.ExecContext(ctx, "UPDATE xero_connection SET last_error = ? WHERE id = 1", message)
	if err != nil {
		return fmt.Errorf("save xero connection error: %w", err)
	}
	return nil
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
