-- +goose Up
-- Review state is separate from extraction and never implies a completed upload.
CREATE TABLE receipt_review (
  receipt_sha256 TEXT PRIMARY KEY NOT NULL REFERENCES receipt(sha256),
  state TEXT NOT NULL DEFAULT 'needs_review' CHECK (state IN ('needs_review', 'unmatched', 'approved', 'rejected', 'ignored')),
  corrected_analysis TEXT,
  tenant_id TEXT,
  bank_transaction_id TEXT,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CHECK ((tenant_id IS NULL AND bank_transaction_id IS NULL) OR (tenant_id IS NOT NULL AND bank_transaction_id IS NOT NULL)),
  CHECK (state != 'approved' OR tenant_id IS NOT NULL),
  FOREIGN KEY (tenant_id, bank_transaction_id) REFERENCES xero_bank_transaction(tenant_id, bank_transaction_id)
);

-- Append-only history includes the reviewed values and the target, when applicable.
CREATE TABLE receipt_review_event (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  receipt_sha256 TEXT NOT NULL REFERENCES receipt(sha256),
  action TEXT NOT NULL CHECK (action IN ('corrected', 'approved', 'rejected', 'ignored', 'unmatched', 'needs_review')),
  analysis TEXT,
  tenant_id TEXT,
  bank_transaction_id TEXT,
  retention TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX receipt_review_event_receipt ON receipt_review_event(receipt_sha256, id);

-- +goose Down
DROP TABLE receipt_review_event;
DROP TABLE receipt_review;
