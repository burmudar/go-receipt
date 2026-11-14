package xero

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

type Status string

const (
	StatusNotConnected      Status = "not_connected"
	StatusConnected         Status = "connected"
	StatusReconnectRequired Status = "reconnect_required"
)

var (
	ErrNotConnected      = errors.New("xero is not connected")
	ErrReconnectRequired = errors.New("xero connection must be reconnected")
	ErrInvalidState      = errors.New("xero authorisation request is unknown or expired; start the connection again")
	ErrPendingApprovals  = errors.New("return approved receipts to review before disconnecting the organisation")
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
	log    *slog.Logger
	now    func() time.Time
	mu     sync.Mutex
	// refreshing makes sure that one bank transaction refresh runs at a time.
	refreshing sync.Mutex
}

func NewManager(db *sql.DB, cfg Config) (*Manager, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Manager{db: db, client: c, log: log, now: time.Now}, nil
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
	m.log.Info("xero authorisation started", "redirect_uri", m.client.oauth.RedirectURL, "scopes", strings.Join(Scopes, " "))
	return m.client.authCodeURL(state, verifier), nil
}

// Complete validates the callback state, exchanges the code, and saves the tokens.
// The service uses one organisation, so the first connection must authorise exactly one organisation.
// A reconnect must authorise the organisation that is already connected.
func (m *Manager) Complete(ctx context.Context, state, code string) error {
	if state == "" || code == "" {
		m.log.Warn("xero callback rejected: the state or the code is missing")
		return ErrInvalidState
	}

	var verifier string
	var createdAt time.Time
	err := m.db.QueryRowContext(ctx,
		"DELETE FROM xero_oauth_state WHERE state = ? RETURNING verifier, created_at", state,
	).Scan(&verifier, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		m.log.Warn("xero callback rejected: the state is unknown or was already used")
		return ErrInvalidState
	}
	if err != nil {
		return fmt.Errorf("read xero authorisation request: %w", err)
	}
	if age := m.now().Sub(createdAt); age > stateLifetime {
		m.log.Warn("xero callback rejected: the state expired", "age", age.Round(time.Second), "lifetime", stateLifetime)
		return ErrInvalidState
	}

	token, err := m.client.exchange(ctx, code, verifier)
	if err != nil {
		return fmt.Errorf("exchange xero authorisation code: %w", err)
	}
	m.log.Info("xero authorisation code exchanged",
		"expires_at", token.Expiry.UTC(), "refresh_token", token.RefreshToken != "", "granted_scopes", token.Extra("scope"))
	if token.RefreshToken == "" {
		return fmt.Errorf("xero did not return a refresh token; check that the offline_access scope is granted")
	}
	organisations, err := m.client.organisations(ctx, token.AccessToken)
	if err != nil {
		return err
	}
	m.log.Info("xero organisations authorised", "count", len(organisations), "organisations", organisations)

	m.mu.Lock()
	defer m.mu.Unlock()

	current, err := m.load(ctx)
	if err != nil && !errors.Is(err, ErrNotConnected) {
		return err
	}

	next := tokens{
		accessToken:  token.AccessToken,
		refreshToken: token.RefreshToken,
		expiresAt:    token.Expiry.UTC(),
		status:       StatusConnected,
	}
	switch {
	case current.tenantID != "":
		i := slices.IndexFunc(organisations, func(o Organisation) bool { return o.TenantID == current.tenantID })
		if i < 0 {
			message := fmt.Sprintf("reconnect and authorise the organisation %q; disconnect first to change the organisation", current.tenantName)
			m.log.Warn("xero reconnect rejected: the connected organisation is not authorised", "tenant_id", current.tenantID)
			if err := m.setStatus(ctx, StatusReconnectRequired, message); err != nil {
				return err
			}
			return errors.New(message)
		}
		next.tenantID, next.tenantName = current.tenantID, organisations[i].Name
	case len(organisations) != 1:
		m.log.Warn("xero connection rejected: exactly one organisation must be authorised", "count", len(organisations))
		return fmt.Errorf("xero authorised %d organisations; this service uses exactly one. Disconnect this app from the other organisations in Xero, then connect again", len(organisations))
	default:
		next.tenantID, next.tenantName = organisations[0].TenantID, organisations[0].Name
	}

	if err := m.save(ctx, next); err != nil {
		return err
	}
	m.log.Info("xero connected", "tenant_id", next.tenantID, "tenant_name", next.tenantName, "reconnect", current.tenantID != "")
	return nil
}

// Connection returns the persisted state without contacting Xero.
func (m *Manager) Connection(ctx context.Context) (Connection, error) {
	var c Connection
	var checkedAt sql.NullTime
	err := m.db.QueryRowContext(ctx, `
		SELECT status, tenant_id, tenant_name, expires_at, refreshed_at, checked_at, last_error
		FROM xero_connection WHERE id = 1`,
	).Scan(&c.Status, &c.TenantID, &c.TenantName, &c.ExpiresAt, &c.RefreshedAt, &checkedAt, &c.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{Status: StatusNotConnected}, nil
	}
	if err != nil {
		return Connection{}, fmt.Errorf("read xero connection: %w", err)
	}
	c.CheckedAt = checkedAt.Time
	return c, nil
}

// AccessToken returns a valid access token and the connected tenant ID for Xero API requests.
// The caller must not log or display the token.
func (m *Manager) AccessToken(ctx context.Context) (accessToken, tenantID string, err error) {
	t, err := m.token(ctx)
	if err != nil {
		return "", "", err
	}
	return t.accessToken, t.tenantID, nil
}

// Check refreshes the access token when necessary and confirms that the organisation is still connected.
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
		return m.requireReconnect(ctx, errors.New("the organisation is no longer connected"))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	_, err = m.db.ExecContext(ctx,
		"UPDATE xero_connection SET checked_at = ?, last_error = '' WHERE id = 1 AND status = ?",
		m.now().UTC(), StatusConnected,
	)
	if err != nil {
		return err
	}
	m.log.Info("xero connection checked", "tenant_id", tenantID)
	return nil
}

// Disconnect removes the local connection. The user can then connect a different organisation.
func (m *Manager) Disconnect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	result, err := m.db.ExecContext(ctx, `DELETE FROM xero_connection WHERE id = 1 AND NOT EXISTS
		(SELECT 1 FROM receipt_review WHERE state = 'approved' AND tenant_id = xero_connection.tenant_id)`)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists bool
		if err := m.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM xero_connection WHERE id=1)").Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrPendingApprovals
		}
	}
	m.log.Info("xero disconnected")
	return nil
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
		m.log.Warn("xero token refresh failed; the previous refresh token is kept", "error", err)
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
	m.log.Info("xero access token refreshed", "expires_at", t.expiresAt, "refresh_token_rotated", refreshed.RefreshToken != "")
	return t, nil
}

func (m *Manager) load(ctx context.Context) (tokens, error) {
	var t tokens
	err := m.db.QueryRowContext(ctx, `
		SELECT tenant_id, tenant_name, access_token, refresh_token, expires_at, status
		FROM xero_connection WHERE id = 1`,
	).Scan(&t.tenantID, &t.tenantName, &t.accessToken, &t.refreshToken, &t.expiresAt, &t.status)
	if errors.Is(err, sql.ErrNoRows) {
		return tokens{}, ErrNotConnected
	}
	if err != nil {
		return tokens{}, fmt.Errorf("read xero connection: %w", err)
	}
	return t, nil
}

// save writes the tokens in one statement, so that the rotated refresh token and the new access token are saved together.
func (m *Manager) save(ctx context.Context, t tokens) error {
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
		t.tenantID, t.tenantName, t.accessToken, t.refreshToken, t.expiresAt, t.status, m.now().UTC(),
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
	m.log.Warn("xero connection requires a reconnect", "reason", cause)
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
