-- Public beta: local builds told from registry images, images a host
-- cannot run left out of its updates, newer versions outside a
-- container's tag_track, per-agent keys.

-- +goose Up
-- When this container's registry check last succeeded; NULL if it never has.
ALTER TABLE containers ADD COLUMN check_ok_at TEXT;
UPDATE containers SET check_ok_at = last_check_at WHERE last_check_at IS NOT NULL AND last_check_error IS NULL;

-- When a new update found that the running image is not in the repository it
-- names. It may be a local build, so a person has to look.
ALTER TABLE updates ADD COLUMN unverified_origin_at TEXT;

-- The platforms of an image in a registry. A digest never changes what it
-- points at, so this is kept rather than fetched again.
CREATE TABLE image_platforms (
  repository TEXT NOT NULL,
  digest TEXT NOT NULL,
  platforms_json TEXT NOT NULL CHECK (json_valid(platforms_json)),
  checked_at TEXT NOT NULL,
  PRIMARY KEY (repository, digest)
);

-- The newest version outside this container's tag_track, if any.
ALTER TABLE containers ADD COLUMN newer_outside_track TEXT;

-- The agent's own key: pinned when first seen, and a different one that
-- a person has to accept.
ALTER TABLE agents ADD COLUMN public_key TEXT;
ALTER TABLE agents ADD COLUMN pending_public_key TEXT;

-- +goose Down
ALTER TABLE agents DROP COLUMN pending_public_key;
ALTER TABLE agents DROP COLUMN public_key;
ALTER TABLE containers DROP COLUMN newer_outside_track;
DROP TABLE image_platforms;
ALTER TABLE updates DROP COLUMN unverified_origin_at;
ALTER TABLE containers DROP COLUMN check_ok_at;
