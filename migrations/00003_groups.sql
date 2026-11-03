-- update groups.

-- +goose Up
-- Set when a user removes a ddum.group-labelled container from its group, so
-- the label never adds it back.
ALTER TABLE containers ADD COLUMN group_label_suppressed INTEGER NOT NULL DEFAULT 0;
CREATE INDEX update_group_members_group ON update_group_members(group_id);

-- +goose Down
DROP INDEX update_group_members_group;
ALTER TABLE containers DROP COLUMN group_label_suppressed;
