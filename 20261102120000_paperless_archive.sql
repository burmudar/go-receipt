-- +goose Up
CREATE TABLE receipt_archive (
 receipt_sha256 TEXT PRIMARY KEY REFERENCES receipt(sha256),
 endpoint TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending',
 checksum TEXT NOT NULL DEFAULT '',
 dispatched INTEGER NOT NULL DEFAULT 0,
 task_id TEXT NOT NULL DEFAULT '',
 document_id INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 completed_at TEXT NOT NULL DEFAULT ''
);
-- +goose Down
DROP TABLE receipt_archive;
