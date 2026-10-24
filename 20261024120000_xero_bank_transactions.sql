-- +goose Up
-- SPEND and RECEIVE bank transactions from Xero. Records are kept when Xero deletes or voids them.
-- A transaction is eligible for a new receipt link only when status is 'AUTHORISED' and is_reconciled is 1.
CREATE TABLE xero_bank_transaction (
  tenant_id TEXT NOT NULL,
  bank_transaction_id TEXT NOT NULL,
  type TEXT NOT NULL CHECK (type IN ('SPEND', 'RECEIVE')),
  status TEXT NOT NULL,
  is_reconciled INTEGER NOT NULL,
  date TEXT NOT NULL, -- YYYY-MM-DD
  contact_name TEXT NOT NULL DEFAULT '',
  bank_account_name TEXT NOT NULL DEFAULT '',
  reference TEXT NOT NULL DEFAULT '',
  currency_code TEXT NOT NULL,
  total TEXT NOT NULL, -- exact decimal
  has_attachments INTEGER NOT NULL,
  updated_date_utc TIMESTAMP NOT NULL,
  fetched_at TIMESTAMP NOT NULL,
  PRIMARY KEY (tenant_id, bank_transaction_id)
);

-- Refresh progress for each organisation. modified_since is NULL until the first complete refresh.
CREATE TABLE xero_transaction_refresh (
  tenant_id TEXT PRIMARY KEY NOT NULL,
  modified_since TIMESTAMP,
  refreshed_at TIMESTAMP,
  attempted_at TIMESTAMP NOT NULL,
  last_error TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE xero_transaction_refresh;
DROP TABLE xero_bank_transaction;
