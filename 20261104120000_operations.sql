-- +goose Up
CREATE TABLE service_control (
  id INTEGER PRIMARY KEY CHECK (id=1),
  paused INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0,1))
);
INSERT INTO service_control(id) VALUES (1);
CREATE TABLE service_worker (
  name TEXT PRIMARY KEY,
  attempted_at INTEGER NOT NULL DEFAULT 0,
  succeeded_at INTEGER NOT NULL DEFAULT 0,
  next_at INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'waiting',
  last_error TEXT NOT NULL DEFAULT ''
);
ALTER TABLE receipt_review_event ADD COLUMN approval_method TEXT NOT NULL DEFAULT '';
ALTER TABLE receipt_review_event ADD COLUMN approval_policy TEXT NOT NULL DEFAULT '';
UPDATE receipt_review_event SET approval_method='manual', approval_policy='manual-v1' WHERE action='approved';
-- +goose Down
ALTER TABLE receipt_review_event DROP COLUMN approval_policy;
ALTER TABLE receipt_review_event DROP COLUMN approval_method;
DROP TABLE service_worker;
DROP TABLE service_control;
