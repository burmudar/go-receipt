-- +goose Up
-- Manual data uses the existing validated-analysis workflow, but remains distinguishable
-- from machine extraction. Review history retains the failure that preceded entry.
ALTER TABLE receipt ADD COLUMN analysis_source TEXT NOT NULL DEFAULT 'extracted' CHECK (analysis_source IN ('extracted', 'manual'));
ALTER TABLE receipt_review_event ADD COLUMN detail TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE receipt_review_event DROP COLUMN detail;
ALTER TABLE receipt DROP COLUMN analysis_source;
