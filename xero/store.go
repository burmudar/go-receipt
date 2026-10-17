package xero

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrReconnect = errors.New("Xero reconnect required: run xero login, then xero select")
var ErrSelect = errors.New("no Xero organisation selected: run xero connections, then xero select --tenant-id ID")

// Tokens are deliberately excluded from JSON output.
type grant struct {
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	ExpiresAt    int64
	Reconnect    bool
	ConnectionID string
	TenantID     string
	TenantName   string
}

type Connection struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenantId"`
	TenantName string `json:"tenantName"`
	TenantType string `json:"tenantType"`
}

// Target uses decimal text for money. Reconciled is only the reported Xero state;
// it is never inferred from an attachment or approval.
type Target struct {
	TenantID       string `json:"tenant_id"`
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	Type           string `json:"type"`
	ContactID      string `json:"contact_id"`
	Contact        string `json:"contact"`
	Date           string `json:"date"`
	Total          string `json:"total"`
	Currency       string `json:"currency"`
	Reference      string `json:"reference"`
	Number         string `json:"number,omitempty"`
	Status         string `json:"status"`
	Reconciled     *bool  `json:"is_reconciled,omitempty"`
	HasAttachments bool   `json:"has_attachments"`
}

type Status struct {
	Connected         bool       `json:"connected"`
	ReconnectRequired bool       `json:"reconnect_required"`
	TenantID          string     `json:"tenant_id,omitempty"`
	TenantName        string     `json:"tenant_name,omitempty"`
	ExpiresAt         *time.Time `json:"access_token_expires_at,omitempty"`
	LastSync          *time.Time `json:"last_sync,omitempty"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) load(ctx context.Context) (grant, error) {
	var g grant
	err := s.db.QueryRowContext(ctx, `SELECT access_token, refresh_token, expires_at,
		reconnect_required, connection_id, tenant_id, tenant_name FROM xero_auth WHERE id = 1`).Scan(
		&g.AccessToken, &g.RefreshToken, &g.ExpiresAt, &g.Reconnect, &g.ConnectionID, &g.TenantID, &g.TenantName)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrReconnect
	}
	return g, err
}

func (s *Store) save(ctx context.Context, g grant) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO xero_auth
		(id, access_token, refresh_token, expires_at, reconnect_required, connection_id, tenant_id, tenant_name)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET access_token=excluded.access_token,
		refresh_token=excluded.refresh_token, expires_at=excluded.expires_at,
		reconnect_required=excluded.reconnect_required, connection_id=excluded.connection_id,
		tenant_id=excluded.tenant_id, tenant_name=excluded.tenant_name, updated_at=CURRENT_TIMESTAMP`,
		g.AccessToken, g.RefreshToken, g.ExpiresAt, g.Reconnect, g.ConnectionID, g.TenantID, g.TenantName)
	return err
}

func (s *Store) reconnect(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE xero_auth SET reconnect_required=1, updated_at=CURRENT_TIMESTAMP WHERE id=1`)
	if err != nil {
		return err
	}
	return ErrReconnect
}

// Status reports local state only. Connections and Sync check remote access.
func (s *Store) Status(ctx context.Context) (Status, error) {
	g, err := s.load(ctx)
	if errors.Is(err, ErrReconnect) {
		return Status{ReconnectRequired: true}, nil
	}
	if err != nil {
		return Status{}, err
	}
	expires := time.Unix(g.ExpiresAt, 0).UTC()
	status := Status{Connected: !g.Reconnect, ReconnectRequired: g.Reconnect,
		TenantID: g.TenantID, TenantName: g.TenantName, ExpiresAt: &expires}
	var synced time.Time
	err = s.db.QueryRowContext(ctx, `SELECT refreshed_at FROM xero_sync WHERE tenant_id=?`, g.TenantID).Scan(&synced)
	if err == nil {
		status.LastSync = &synced
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Status{}, err
	}
	return status, nil
}

// replaceTargets publishes only a complete snapshot. A failed page leaves the
// previous snapshot and its timestamp intact. Tenant keys prevent cross-org matches.
func (s *Store) replaceTargets(ctx context.Context, tenant string, targets []Target) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM xero_target WHERE tenant_id=?`, tenant); err != nil {
		return err
	}
	for _, target := range targets {
		data, err := json.Marshal(target)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO xero_target (tenant_id, kind, xero_id, data) VALUES (?, ?, ?, ?)`,
			tenant, target.Kind, target.ID, string(data)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO xero_sync (tenant_id) VALUES (?)
		ON CONFLICT(tenant_id) DO UPDATE SET refreshed_at=CURRENT_TIMESTAMP`, tenant); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Targets(ctx context.Context) ([]Target, error) {
	g, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if g.Reconnect {
		return nil, ErrReconnect
	}
	if g.TenantID == "" {
		return nil, ErrSelect
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM xero_target WHERE tenant_id=? ORDER BY kind, xero_id`, g.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []Target{}
	for rows.Next() {
		var data string
		var target Target
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &target); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}
