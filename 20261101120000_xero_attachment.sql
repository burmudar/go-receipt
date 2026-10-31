-- +goose Up
CREATE TABLE receipt_attachment (
 receipt_sha256 TEXT PRIMARY KEY REFERENCES receipt(sha256),
 tenant_id TEXT NOT NULL,
 transaction_id TEXT NOT NULL,
 total TEXT NOT NULL,
 currency TEXT NOT NULL,
 date TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending',
 locked INTEGER NOT NULL DEFAULT 0,
 dispatched INTEGER NOT NULL DEFAULT 0,
 attachment_id TEXT NOT NULL DEFAULT '',
 last_error TEXT NOT NULL DEFAULT '',
 completed_at TEXT NOT NULL DEFAULT ''
);
-- Older approvals have no approval-time transaction snapshot. Ask for fresh approval.
INSERT INTO receipt_review_event(receipt_sha256,action,retention)
 SELECT receipt_sha256,'needs_review','Attachment upgrade requires fresh approval' FROM receipt_review WHERE state='approved';
UPDATE receipt_review SET state='needs_review',tenant_id=NULL,bank_transaction_id=NULL WHERE state='approved';
-- +goose StatementBegin
CREATE TRIGGER attachment_lock BEFORE UPDATE ON receipt_review
WHEN EXISTS(SELECT 1 FROM receipt_attachment WHERE receipt_sha256=OLD.receipt_sha256 AND locked=1)
BEGIN SELECT RAISE(ABORT,'attachment is in progress or confirmed; target changes require manual follow-up'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attachment_approval_insert AFTER INSERT ON receipt_review WHEN NEW.state='approved'
BEGIN
 INSERT OR REPLACE INTO receipt_attachment(receipt_sha256,tenant_id,transaction_id,total,currency,date)
 SELECT NEW.receipt_sha256,tenant_id,bank_transaction_id,total,currency_code,date FROM xero_bank_transaction WHERE tenant_id=NEW.tenant_id AND bank_transaction_id=NEW.bank_transaction_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attachment_approval_update AFTER UPDATE ON receipt_review WHEN NEW.state='approved' AND OLD.state!='approved'
BEGIN
 INSERT OR REPLACE INTO receipt_attachment(receipt_sha256,tenant_id,transaction_id,total,currency,date)
 SELECT NEW.receipt_sha256,tenant_id,bank_transaction_id,total,currency_code,date FROM xero_bank_transaction WHERE tenant_id=NEW.tenant_id AND bank_transaction_id=NEW.bank_transaction_id;
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER attachment_approval_update;
DROP TRIGGER attachment_approval_insert;
DROP TRIGGER attachment_lock;
DROP TABLE receipt_attachment;
