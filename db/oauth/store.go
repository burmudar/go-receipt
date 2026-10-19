package oauth

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/burmudar/go-receipt/db"
)

var _ db.Store[OAuthClientModel] = (*OAuthStore)(nil)

type OAuthClientModel struct {
	ID           int
	Name         string
	ClientID     string
	ClientSecret string `json:"-"`
	RedirectURI  string
	CreatedAt    time.Time
}

// OAuthStore persists OAuth clients. Get, Delete, and Update return
// sql.ErrNoRows when the requested client does not exist.
type OAuthStore struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *OAuthStore {
	return &OAuthStore{db: db}
}

const clientColumns = "id, name, client_id, client_secret, redirect_uri, created_at"

// Create inserts a client and populates its database-generated ID and CreatedAt.
func (m *OAuthStore) Create(v *OAuthClientModel) error {
	if v == nil {
		return fmt.Errorf("oauth client cannot be nil")
	}
	var id int
	var createdAt time.Time
	err := m.db.QueryRow(`
		INSERT INTO oauth_client (name, client_id, client_secret, redirect_uri)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
		client_id = excluded.client_id,
		client_secret = excluded.client_secret,
		redirect_uri = excluded.redirect_uri,
		created_at = CURRENT_TIMESTAMP
		RETURNING id, created_at
		`,
		v.Name, v.ClientID, v.ClientSecret, v.RedirectURI,
	).Scan(&id, &createdAt)
	if err != nil {
		return err
	}
	v.ID, v.CreatedAt = id, createdAt
	return nil
}

func (m *OAuthStore) Get(id int) (*OAuthClientModel, error) {
	return scanClient(m.db.QueryRow("SELECT "+clientColumns+" FROM oauth_client WHERE id = ?", id))
}

func (m *OAuthStore) List() ([]*OAuthClientModel, error) {
	rows, err := m.db.Query("SELECT " + clientColumns + " FROM oauth_client ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	clients := make([]*OAuthClientModel, 0)
	for rows.Next() {
		client, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return clients, nil
}

func (m *OAuthStore) Delete(id int) (*OAuthClientModel, error) {
	return scanClient(m.db.QueryRow("DELETE FROM oauth_client WHERE id = ? RETURNING "+clientColumns, id))
}

func (m *OAuthStore) Update(v *OAuthClientModel) error {
	if v == nil {
		return fmt.Errorf("oauth client cannot be nil")
	}
	var createdAt time.Time
	err := m.db.QueryRow(`
		UPDATE oauth_client SET name = ?, client_id = ?, client_secret = ?, redirect_uri = ?
		WHERE id = ? RETURNING created_at`,
		v.Name, v.ClientID, v.ClientSecret, v.RedirectURI, v.ID,
	).Scan(&createdAt)
	if err != nil {
		return err
	}
	v.CreatedAt = createdAt
	return nil
}

func scanClient(row interface{ Scan(...any) error }) (*OAuthClientModel, error) {
	v := new(OAuthClientModel)
	if err := row.Scan(&v.ID, &v.Name, &v.ClientID, &v.ClientSecret, &v.RedirectURI, &v.CreatedAt); err != nil {
		return nil, err
	}
	return v, nil
}
