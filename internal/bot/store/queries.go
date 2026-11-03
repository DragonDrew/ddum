package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// Stack is a stored stack with its host's name.
type Stack struct {
	ID             string
	HostID         string
	HostName       string
	DisplayName    string
	Fingerprint    string
	Provider       string
	Compose        *protocol.ComposeInfo
	DeploySupport  bool
	DeployReason   string
	RollbackAvail  bool
	Presence       string
	MissingSince   time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ContainerCount int
}

// Container is a stored container with its stack and host names.
type Container struct {
	ID              string
	StackID         string
	StackName       string
	HostID          string
	HostName        string
	IdentityKey     string
	DisplayName     string
	DockerID        string
	DockerName      string
	ImageRef        string
	ImageRepository string
	RunningDigests  []string
	Platform        string
	RuntimeState    string
	Health          string
	Protected       bool
	Self            string
	DdumLabels      map[string]string
	Notes           []string
	Presence        string
	LastSeenAt      time.Time

	TagChange       *protocol.TagChange // nil until classified
	Tracking        string
	UntrackedReason string
	LastCheckAt     time.Time
	LastCheckError  string
	// CheckEverOK: a registry check of this container has succeeded before.
	CheckEverOK bool
	// NewerOutsideTrack is the newest version its tag_track leaves out.
	NewerOutsideTrack string
	CheckFailingSince time.Time // zero unless checks are failing

	// From the host and stack, for displayed state.
	HostApproval         string
	HostConnectivity     string
	StackDeploySupported bool
	StackDeployReason    string
	StackPresence        string

	GroupID   string // "" when the container is in no group
	GroupName string
}

// StackFilter narrows Stacks. Empty fields do not filter.
type StackFilter struct {
	HostID         string
	IncludeMissing bool
}

const stackColumns = `s.id, s.host_id, h.display_name, s.display_name, s.fingerprint, s.provider,
	COALESCE(s.compose_json,''), s.deploy_supported, COALESCE(s.deploy_reason,''), s.rollback_available,
	s.presence, COALESCE(s.missing_since,''), s.created_at, s.updated_at,
	(SELECT count(*) FROM containers c WHERE c.stack_id = s.id AND c.presence = 'present')`

const stackFrom = ` FROM stacks s JOIN hosts h ON h.id = s.host_id`

func scanStack(r scanner) (Stack, error) {
	var st Stack
	var compose, missing, created, updated string
	err := r.Scan(&st.ID, &st.HostID, &st.HostName, &st.DisplayName, &st.Fingerprint, &st.Provider,
		&compose, &st.DeploySupport, &st.DeployReason, &st.RollbackAvail,
		&st.Presence, &missing, &created, &updated, &st.ContainerCount)
	if err != nil {
		return st, err
	}
	if compose != "" {
		st.Compose = &protocol.ComposeInfo{}
		json.Unmarshal([]byte(compose), st.Compose)
	}
	st.MissingSince = parseOpt(missing)
	st.CreatedAt = parseOpt(created)
	st.UpdatedAt = parseOpt(updated)
	return st, nil
}

// Stacks lists non-retired stacks on non-removed hosts, ordered by host and
// stack name. Missing stacks are hidden unless requested.
func (s *Store) Stacks(ctx context.Context, f StackFilter) ([]Stack, error) {
	q := `SELECT ` + stackColumns + stackFrom + ` WHERE s.retired_at IS NULL AND h.removed_at IS NULL`
	var args []any
	if f.HostID != "" {
		q += ` AND s.host_id = ?`
		args = append(args, f.HostID)
	}
	if !f.IncludeMissing {
		q += ` AND s.presence = 'present'`
	}
	q += ` ORDER BY h.display_name COLLATE NOCASE, s.display_name COLLATE NOCASE, s.id`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stack
	for rows.Next() {
		st, err := scanStack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Stack returns one stack by ID.
func (s *Store) Stack(ctx context.Context, id string) (Stack, error) {
	st, err := scanStack(s.r.QueryRowContext(ctx, `SELECT `+stackColumns+stackFrom+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// RenameStack changes a stack's display name.
func (s *Store) RenameStack(ctx context.Context, stackID, name string, by Actor, at time.Time) (Stack, error) {
	var st Stack
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		st, err = scanStack(tx.QueryRowContext(ctx, `SELECT `+stackColumns+stackFrom+` WHERE s.id = ?`, stackID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE stacks SET display_name = ?, updated_at = ? WHERE id = ?`,
			name, FormatTime(at), stackID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, by, at, "stack.renamed", "stack", stackID,
			map[string]any{"from": st.DisplayName, "to": name}); err != nil {
			return err
		}
		st.DisplayName = name
		return nil
	})
	return st, err
}

// ContainerFilter narrows Containers. Empty fields do not filter.
type ContainerFilter struct {
	HostID         string
	StackID        string
	IncludeMissing bool
}

const containerColumns = `c.id, c.stack_id, s.display_name, s.host_id, h.display_name, c.identity_key, c.display_name,
	COALESCE(c.docker_id,''), COALESCE(c.docker_name,''), COALESCE(c.image_ref,''), COALESCE(c.image_repository,''),
	c.running_digests_json, COALESCE(c.platform,''), COALESCE(c.runtime_state,''), COALESCE(c.health,''),
	c.protected, c.self_role, c.ddum_labels_json, c.notes_json, c.presence, COALESCE(c.last_seen_at,''),
	COALESCE(c.tag_change_json,''), c.tracking, COALESCE(c.untracked_reason,''), COALESCE(c.last_check_at,''),
	COALESCE(c.last_check_error,''), h.approval, h.connectivity, s.deploy_supported, COALESCE(s.deploy_reason,''), s.presence,
	COALESCE(gm.group_id,''), COALESCE(g.display_name,''), COALESCE(c.check_failing_since,''),
	c.check_ok_at IS NOT NULL, COALESCE(c.newer_outside_track,'')`

const containerFrom = ` FROM containers c JOIN stacks s ON s.id = c.stack_id JOIN hosts h ON h.id = s.host_id
	LEFT JOIN update_group_members gm ON gm.container_id = c.id LEFT JOIN update_groups g ON g.id = gm.group_id`

func scanContainer(r scanner) (Container, error) {
	var c Container
	var digests, labels, notes, lastSeen, tagChange, lastCheck, failingSince string
	err := r.Scan(&c.ID, &c.StackID, &c.StackName, &c.HostID, &c.HostName, &c.IdentityKey, &c.DisplayName,
		&c.DockerID, &c.DockerName, &c.ImageRef, &c.ImageRepository,
		&digests, &c.Platform, &c.RuntimeState, &c.Health,
		&c.Protected, &c.Self, &labels, &notes, &c.Presence, &lastSeen,
		&tagChange, &c.Tracking, &c.UntrackedReason, &lastCheck,
		&c.LastCheckError, &c.HostApproval, &c.HostConnectivity, &c.StackDeploySupported, &c.StackDeployReason, &c.StackPresence, &c.GroupID, &c.GroupName,
		&failingSince, &c.CheckEverOK, &c.NewerOutsideTrack)
	if err != nil {
		return c, err
	}
	json.Unmarshal([]byte(digests), &c.RunningDigests)
	json.Unmarshal([]byte(labels), &c.DdumLabels)
	json.Unmarshal([]byte(notes), &c.Notes)
	c.LastSeenAt = parseOpt(lastSeen)
	c.LastCheckAt = parseOpt(lastCheck)
	c.CheckFailingSince = parseOpt(failingSince)
	if tagChange != "" {
		c.TagChange = &protocol.TagChange{}
		json.Unmarshal([]byte(tagChange), c.TagChange)
	}
	return c, nil
}

// Containers lists containers of non-retired stacks on non-removed hosts.
func (s *Store) Containers(ctx context.Context, f ContainerFilter) ([]Container, error) {
	q := `SELECT ` + containerColumns + containerFrom + ` WHERE s.retired_at IS NULL AND h.removed_at IS NULL`
	var args []any
	if f.HostID != "" {
		q += ` AND s.host_id = ?`
		args = append(args, f.HostID)
	}
	if f.StackID != "" {
		q += ` AND c.stack_id = ?`
		args = append(args, f.StackID)
	}
	if !f.IncludeMissing {
		q += ` AND c.presence = 'present'`
	}
	q += ` ORDER BY h.display_name COLLATE NOCASE, s.display_name COLLATE NOCASE, c.display_name COLLATE NOCASE, c.id`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Container
	for rows.Next() {
		c, err := scanContainer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Container returns one container by ID.
func (s *Store) Container(ctx context.Context, id string) (Container, error) {
	c, err := scanContainer(s.r.QueryRowContext(ctx, `SELECT `+containerColumns+containerFrom+` WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// Setting reads a system_settings value into v. found is false if unset.
func (s *Store) Setting(ctx context.Context, key string, v any) (found bool, err error) {
	var raw string
	err = s.r.QueryRowContext(ctx, `SELECT value_json FROM system_settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

// SetSetting writes a system_settings value.
func (s *Store) SetSetting(ctx context.Context, key string, v any, at time.Time) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings (key, value_json, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
			key, string(b), FormatTime(at))
		return err
	})
}

// DiscordMessage is a stored reference to a message the bot posted.
type DiscordMessage struct {
	ID         string
	EntityType string
	EntityID   string
	ChannelID  string
	MessageID  string
}

// AddDiscordMessage records a posted message so it can be edited later
// .
func (s *Store) AddDiscordMessage(ctx context.Context, entityType, entityID, channelID, messageID string, at time.Time) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO discord_messages (id, entity_type, entity_id, channel_id, message_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, s.ids.New(), entityType, entityID, channelID, messageID, FormatTime(at))
		return err
	})
}

// DiscordMessages returns the messages recorded for an entity, oldest first.
func (s *Store) DiscordMessages(ctx context.Context, entityType, entityID string) ([]DiscordMessage, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT id, entity_type, entity_id, channel_id, message_id FROM discord_messages
		WHERE entity_type = ? AND entity_id = ? ORDER BY created_at, id`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscordMessage
	for rows.Next() {
		var m DiscordMessage
		if err := rows.Scan(&m.ID, &m.EntityType, &m.EntityID, &m.ChannelID, &m.MessageID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// BindDiscordMessage makes a message represent entityType/entityID only: any
// earlier binding of the same message is dropped, so a card that turns into
// something else (an update card becoming its operation) stops being edited
// as the old thing.
func (s *Store) BindDiscordMessage(ctx context.Context, entityType, entityID, channelID, messageID string, at time.Time) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM discord_messages WHERE channel_id = ? AND message_id = ?`,
			channelID, messageID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO discord_messages (id, entity_type, entity_id, channel_id, message_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, s.ids.New(), entityType, entityID, channelID, messageID, FormatTime(at))
		return err
	})
}

// DiscordMessageAt returns what a message represents, or ErrNotFound.
func (s *Store) DiscordMessageAt(ctx context.Context, channelID, messageID string) (DiscordMessage, error) {
	var m DiscordMessage
	err := s.r.QueryRowContext(ctx, `
		SELECT id, entity_type, entity_id, channel_id, message_id FROM discord_messages
		WHERE channel_id = ? AND message_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, channelID, messageID).
		Scan(&m.ID, &m.EntityType, &m.EntityID, &m.ChannelID, &m.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}
