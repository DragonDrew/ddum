-- +goose Up
-- When an automatic deployment of this update failed. The policy engine does
-- not deploy the same digest again; a user may still approve it.
ALTER TABLE updates ADD COLUMN auto_failed_at TEXT;

-- +goose Down
ALTER TABLE updates DROP COLUMN auto_failed_at;
