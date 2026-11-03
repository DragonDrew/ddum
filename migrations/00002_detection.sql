-- update detection and notifications.

-- +goose Up
ALTER TABLE containers ADD COLUMN tag_change_json TEXT CHECK (tag_change_json IS NULL OR json_valid(tag_change_json));
ALTER TABLE containers ADD COLUMN last_check_at TEXT;      -- last registry check of this container
ALTER TABLE containers ADD COLUMN last_check_error TEXT;   -- why the last check failed, if it did
ALTER TABLE updates ADD COLUMN notified_at TEXT;           -- NULL until a notification covered it (cleared on reopen)
CREATE INDEX updates_unnotified ON updates(status) WHERE notified_at IS NULL;

-- +goose Down
DROP INDEX updates_unnotified;
ALTER TABLE updates DROP COLUMN notified_at;
ALTER TABLE containers DROP COLUMN last_check_error;
ALTER TABLE containers DROP COLUMN last_check_at;
ALTER TABLE containers DROP COLUMN tag_change_json;
