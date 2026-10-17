-- +goose Up
CREATE TABLE receipt (
  sha256 TEXT PRIMARY KEY NOT NULL,
  name TEXT NOT NULL,
  source TEXT NOT NULL,
  original BLOB NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'extracted', 'failed')),
  analysis TEXT,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  extracted_at TIMESTAMP,
  CHECK ((status = 'extracted' AND analysis IS NOT NULL AND extracted_at IS NOT NULL)
      OR (status != 'extracted' AND analysis IS NULL AND extracted_at IS NULL))
);

-- +goose Down
DROP TABLE receipt;
