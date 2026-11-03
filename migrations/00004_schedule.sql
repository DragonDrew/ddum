-- Scheduled deploys: an approved operation may wait for a time of day
-- (owner request, ).

-- +goose Up
-- Not dispatched before this time; NULL means as soon as possible.
ALTER TABLE operations ADD COLUMN not_before TEXT;

-- +goose Down
ALTER TABLE operations DROP COLUMN not_before;
