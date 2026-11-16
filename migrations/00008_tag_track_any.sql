-- The global tag_track default is now 'any': every newer version is
-- offered, including a new major version. same-major and same-minor are
-- policies a person sets. Only the global row changes; host, stack, group
-- and container rows are left as they are.

-- +goose Up
UPDATE policies SET tag_track = 'any' WHERE scope_type = 'global' AND tag_track = 'same-major';

-- +goose Down
UPDATE policies SET tag_track = 'same-major' WHERE scope_type = 'global' AND tag_track = 'any';
