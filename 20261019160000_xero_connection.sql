-- +goose Up
-- The server derives the OAuth redirect URI from its public base URL.
ALTER TABLE oauth_client DROP COLUMN redirect_uri;

-- Pending authorisation requests. A state is used once and expires after a short time.
CREATE TABLE xero_oauth_state (
  state TEXT PRIMARY KEY NOT NULL,
  verifier TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL
);

-- The single Xero connection. tenant_id is NULL until the user selects an organisation.
CREATE TABLE xero_connection (
  id INTEGER PRIMARY KEY NOT NULL CHECK (id = 1),
  tenant_id TEXT,
  tenant_name TEXT NOT NULL DEFAULT '',
  access_token TEXT NOT NULL,
  refresh_token TEXT NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('select_organisation', 'connected', 'reconnect_required')),
  last_error TEXT NOT NULL DEFAULT '',
  refreshed_at TIMESTAMP NOT NULL,
  checked_at TIMESTAMP,
  CHECK (status != 'connected' OR tenant_id IS NOT NULL)
);

-- +goose Down
DROP TABLE xero_connection;
DROP TABLE xero_oauth_state;
ALTER TABLE oauth_client ADD COLUMN redirect_uri TEXT NOT NULL DEFAULT '';
