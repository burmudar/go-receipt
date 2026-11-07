-- +goose Up
CREATE TABLE receipt_cleanup (
 receipt_sha256 TEXT PRIMARY KEY REFERENCES receipt(sha256),
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','deleting','failed','uncertain','confirmed')),
 attempts INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 completed_at TEXT NOT NULL DEFAULT ''
);
-- +goose Down
DROP TABLE receipt_cleanup;
