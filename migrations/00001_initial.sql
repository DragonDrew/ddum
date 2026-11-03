-- Initial schema. IDs are UUID text; timestamps are RFC 3339 UTC text.

-- +goose Up

CREATE TABLE agents (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  agent_version TEXT,
  protocol_version INTEGER,
  capabilities_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(capabilities_json)),
  poll_interval_seconds INTEGER,
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT
);

CREATE TABLE hosts (
  id TEXT PRIMARY KEY,
  agent_id TEXT UNIQUE REFERENCES agents(id),   -- NULL after the host is retired by /host reassign-agent
  display_name TEXT NOT NULL,
  docker_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(docker_json)),
  providers_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(providers_json)),  -- provider statuses from the last inventory
  approval TEXT NOT NULL DEFAULT 'pending' CHECK (approval IN ('pending','approved')),
  approved_by_discord_id TEXT,
  approved_at TEXT,
  connectivity TEXT NOT NULL DEFAULT 'online' CHECK (connectivity IN ('online','offline')),
  last_inventory_at TEXT,
  inventory_hash TEXT,
  created_at TEXT NOT NULL,
  removed_at TEXT
);

CREATE TABLE stacks (
  id TEXT PRIMARY KEY,
  host_id TEXT NOT NULL REFERENCES hosts(id),
  display_name TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  provider TEXT NOT NULL CHECK (provider IN ('portainer','compose','docker')),
  provider_ref_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(provider_ref_json)),
  compose_json TEXT CHECK (compose_json IS NULL OR json_valid(compose_json)),
  deploy_supported INTEGER NOT NULL DEFAULT 0,
  deploy_reason TEXT,
  rollback_available INTEGER NOT NULL DEFAULT 0,
  presence TEXT NOT NULL DEFAULT 'present' CHECK (presence IN ('present','missing')),
  missing_since TEXT,
  retired_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (host_id, fingerprint)
);

CREATE TABLE containers (
  id TEXT PRIMARY KEY,
  stack_id TEXT NOT NULL REFERENCES stacks(id),
  identity_key TEXT NOT NULL,
  display_name TEXT NOT NULL,
  docker_id TEXT,
  docker_name TEXT,
  image_ref TEXT,
  image_repository TEXT,
  running_digests_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(running_digests_json)),
  platform TEXT,
  tracking TEXT NOT NULL DEFAULT 'digest' CHECK (tracking IN ('digest','version','untracked')),
  untracked_reason TEXT,
  runtime_state TEXT,
  health TEXT,
  protected INTEGER NOT NULL DEFAULT 0,
  self_role TEXT NOT NULL DEFAULT '' CHECK (self_role IN ('','agent','bot')),  -- ddum.self label
  ddum_labels_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(ddum_labels_json)),
  notes_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(notes_json)),
  drifted INTEGER NOT NULL DEFAULT 0,
  presence TEXT NOT NULL DEFAULT 'present' CHECK (presence IN ('present','missing')),
  last_seen_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (stack_id, identity_key)
);

CREATE TABLE update_groups (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'user' CHECK (source IN ('user','label')),
  created_at TEXT NOT NULL
);

CREATE TABLE update_group_members (
  id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL REFERENCES update_groups(id),
  container_id TEXT NOT NULL UNIQUE REFERENCES containers(id),
  created_at TEXT NOT NULL
);

CREATE TABLE policies (
  id TEXT PRIMARY KEY,
  scope_type TEXT NOT NULL CHECK (scope_type IN ('global','host','stack','group','container')),
  scope_id TEXT,                               -- NULL only for global
  mode TEXT CHECK (mode IN ('manual','automatic','ignore')),
  tag_track TEXT CHECK (tag_track IN ('pinned','same-minor','same-major','any')),
  auto_rollback INTEGER,
  verify_timeout_seconds INTEGER,
  verify_stable_seconds INTEGER,
  updated_at TEXT NOT NULL,
  UNIQUE (scope_type, scope_id),
  CHECK ((scope_type = 'global') = (scope_id IS NULL))
);
CREATE UNIQUE INDEX policies_one_global ON policies(scope_type) WHERE scope_type = 'global';

CREATE TABLE operations (                      -- command queue and deployment/rollback operation in one
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('deploy','rollback')),
  agent_id TEXT NOT NULL REFERENCES agents(id),
  host_id TEXT NOT NULL REFERENCES hosts(id),
  scope_type TEXT NOT NULL CHECK (scope_type IN ('stack','group')),
  scope_id TEXT NOT NULL,
  original_operation_id TEXT REFERENCES operations(id),  -- rollbacks
  payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
  state TEXT NOT NULL CHECK (state IN ('queued','dispatched','accepted','running','verifying',
        'succeeded','failed','partially_failed','interrupted','expired','cancelled')),
  attempt INTEGER NOT NULL DEFAULT 0,
  lease_expires_at TEXT,
  expires_at TEXT NOT NULL,
  initiator_kind TEXT NOT NULL CHECK (initiator_kind IN ('user','policy')),
  initiator_discord_id TEXT,
  result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  finished_at TEXT
);

CREATE TABLE operation_events (
  id TEXT PRIMARY KEY,
  operation_id TEXT NOT NULL REFERENCES operations(id),
  seq INTEGER NOT NULL,
  type TEXT NOT NULL,
  step TEXT,
  message TEXT,
  payload_json TEXT CHECK (payload_json IS NULL OR json_valid(payload_json)),
  received_at TEXT NOT NULL,
  UNIQUE (operation_id, seq)
);

CREATE TABLE updates (
  id TEXT PRIMARY KEY,
  container_id TEXT NOT NULL REFERENCES containers(id),
  stack_id TEXT NOT NULL REFERENCES stacks(id),
  host_id TEXT NOT NULL REFERENCES hosts(id),
  group_id TEXT REFERENCES update_groups(id),
  kind TEXT NOT NULL CHECK (kind IN ('digest','version')),
  current_image_ref TEXT NOT NULL,
  current_digest TEXT,
  target_image_ref TEXT NOT NULL,
  target_digest TEXT NOT NULL,
  target_version TEXT,
  release_url TEXT,
  status TEXT NOT NULL CHECK (status IN ('detected','approved','deploying','applied','failed','superseded')),
  approved_by_kind TEXT CHECK (approved_by_kind IN ('user','policy')),
  approved_by_discord_id TEXT,
  approved_at TEXT,
  operation_id TEXT REFERENCES operations(id),
  detected_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (container_id, target_digest)
);

CREATE TABLE approval_offers (                 -- what an Approve button approves
  id TEXT PRIMARY KEY,                         -- UUID carried in the button custom_id
  scope_type TEXT NOT NULL CHECK (scope_type IN ('stack','group')),
  scope_id TEXT NOT NULL,
  items_json TEXT NOT NULL CHECK (json_valid(items_json)),  -- [{"update_id":"…","target_digest":"sha256:…"}]
  self_update INTEGER NOT NULL DEFAULT 0,
  created_by_kind TEXT NOT NULL CHECK (created_by_kind IN ('notification','command','policy')),
  created_at TEXT NOT NULL,
  consumed_at TEXT,                            -- set when it produced an operation
  operation_id TEXT REFERENCES operations(id)
);

CREATE TABLE snoozes (
  id TEXT PRIMARY KEY,
  update_id TEXT NOT NULL REFERENCES updates(id),
  until TEXT,                                  -- NULL = indefinite
  created_by_discord_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  removed_at TEXT,
  removed_by_discord_id TEXT,
  expiry_notified_at TEXT
);

CREATE TABLE desired_states (
  id TEXT PRIMARY KEY,
  container_id TEXT NOT NULL REFERENCES containers(id),
  image_ref TEXT NOT NULL,
  image_digest TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','reached','failed','superseded')),
  operation_id TEXT REFERENCES operations(id),
  set_at TEXT NOT NULL,
  resolved_at TEXT
);

CREATE TABLE rollback_snapshots (
  id TEXT PRIMARY KEY,                         -- same UUID the agent uses
  operation_id TEXT NOT NULL REFERENCES operations(id),
  stack_id TEXT NOT NULL REFERENCES stacks(id),
  host_id TEXT NOT NULL REFERENCES hosts(id),
  status TEXT NOT NULL CHECK (status IN ('available','consumed','pruned','unavailable')),
  summary_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(summary_json)),
  created_at TEXT NOT NULL
);

CREATE TABLE audit_events (
  id TEXT PRIMARY KEY,
  at TEXT NOT NULL,
  actor_kind TEXT NOT NULL CHECK (actor_kind IN ('user','policy','agent','system')),
  actor_discord_id TEXT,
  actor_name TEXT,
  action TEXT NOT NULL,                        -- e.g. 'update.approved', 'policy.set', 'stack.merged'
  target_type TEXT,
  target_id TEXT,
  details_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(details_json)),
  result TEXT NOT NULL DEFAULT 'ok'
);

CREATE TABLE discord_messages (
  id TEXT PRIMARY KEY,
  entity_type TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  message_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE system_settings (
  key TEXT PRIMARY KEY,
  value_json TEXT NOT NULL CHECK (json_valid(value_json)),
  updated_at TEXT NOT NULL
);

-- Indexes.
CREATE INDEX operations_host_state ON operations(host_id, state);
CREATE INDEX operations_state_lease ON operations(state, lease_expires_at);
CREATE INDEX updates_status ON updates(status);
CREATE INDEX updates_container ON updates(container_id);
CREATE INDEX snoozes_update ON snoozes(update_id);
CREATE INDEX audit_events_target ON audit_events(target_type, target_id, at);
CREATE INDEX desired_states_container_status ON desired_states(container_id, status);
CREATE UNIQUE INDEX desired_states_one_pending ON desired_states(container_id) WHERE status = 'pending';
CREATE INDEX discord_messages_entity ON discord_messages(entity_type, entity_id);

-- Immutability: audit and operation events cannot be changed or deleted by
-- application code. operation_events rows may be deleted by the retention job.
-- +goose StatementBegin
CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
  BEGIN SELECT RAISE(ABORT, 'audit_events is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
  BEGIN SELECT RAISE(ABORT, 'audit_events is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER operation_events_no_update BEFORE UPDATE ON operation_events
  BEGIN SELECT RAISE(ABORT, 'operation_events is immutable'); END;
-- +goose StatementEnd

-- Seed the global policy. auto_rollback stays NULL: it resolves from mode.
INSERT INTO policies (id, scope_type, scope_id, mode, tag_track, auto_rollback,
                      verify_timeout_seconds, verify_stable_seconds, updated_at)
VALUES ('00000000-0000-7000-8000-000000000001', 'global', NULL, 'manual', 'same-major', NULL,
        120, 30, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

-- +goose Down
-- Down migrations exist only for development; production never runs them.
DROP TABLE system_settings;
DROP TABLE discord_messages;
DROP TABLE audit_events;
DROP TABLE rollback_snapshots;
DROP TABLE desired_states;
DROP TABLE snoozes;
DROP TABLE approval_offers;
DROP TABLE updates;
DROP TABLE operation_events;
DROP TABLE operations;
DROP TABLE policies;
DROP TABLE update_group_members;
DROP TABLE update_groups;
DROP TABLE containers;
DROP TABLE stacks;
DROP TABLE hosts;
DROP TABLE agents;
