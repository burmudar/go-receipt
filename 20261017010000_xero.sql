-- +goose Up
CREATE TABLE xero_auth (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  access_token TEXT NOT NULL,
  refresh_token TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  reconnect_required INTEGER NOT NULL DEFAULT 0,
  connection_id TEXT NOT NULL DEFAULT '',
  tenant_id TEXT NOT NULL DEFAULT '',
  tenant_name TEXT NOT NULL DEFAULT '',
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE xero_target (
  tenant_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('BankTransactions', 'Invoices')),
  xero_id TEXT NOT NULL,
  data TEXT NOT NULL,
  refreshed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (tenant_id, kind, xero_id)
);

CREATE TABLE xero_sync (
  tenant_id TEXT PRIMARY KEY,
  refreshed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- A grant belongs to the configured OAuth client. A credential change requires login.
-- +goose StatementBegin
CREATE TRIGGER xero_client_changed AFTER UPDATE ON oauth_client
WHEN OLD.name = 'Xero' AND (
  NEW.name != OLD.name OR NEW.client_id != OLD.client_id OR
  NEW.client_secret != OLD.client_secret OR NEW.redirect_uri != OLD.redirect_uri
)
BEGIN
  DELETE FROM xero_auth;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER xero_client_deleted AFTER DELETE ON oauth_client
WHEN OLD.name = 'Xero'
BEGIN
  DELETE FROM xero_auth;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER xero_client_deleted;
DROP TRIGGER xero_client_changed;
DROP TABLE xero_sync;
DROP TABLE xero_target;
DROP TABLE xero_auth;
