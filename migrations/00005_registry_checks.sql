-- Registry tag lists kept across check cycles and restarts; notices for long-failing checks and
-- for agents whose version differs from the bot's.

-- +goose Up
CREATE TABLE registry_tag_lists (
  repository TEXT PRIMARY KEY,                             -- e.g. ghcr.io/immich-app/immich-server
  tags_json TEXT NOT NULL CHECK (json_valid(tags_json)),   -- version tags only, in listing order
  tail_json TEXT NOT NULL CHECK (json_valid(tail_json)),   -- the last two tags as listed, of any kind
  latest_digest TEXT NOT NULL DEFAULT '',                  -- latest's digest when listed; '' when it had none
  listed_at TEXT NOT NULL                                  -- listed, or last extended
);

-- Since when this container's registry checks have failed; NULL once one succeeds.
ALTER TABLE containers ADD COLUMN check_failing_since TEXT;
-- When the notice for the current failure was posted; cleared by the recovery notice.
ALTER TABLE containers ADD COLUMN check_failure_noticed_at TEXT;

-- "<agent version> <bot version>" last reported as a version difference.
ALTER TABLE agents ADD COLUMN version_notice TEXT;

-- +goose Down
ALTER TABLE agents DROP COLUMN version_notice;
ALTER TABLE containers DROP COLUMN check_failure_noticed_at;
ALTER TABLE containers DROP COLUMN check_failing_since;
DROP TABLE registry_tag_lists;
