package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Errors returned by host operations.
var (
	ErrNotFound        = errors.New("not found")
	ErrHostRemoved     = errors.New("host has been removed")
	ErrUnknownAgent    = errors.New("unknown agent")
	ErrUnsupportedArch = errors.New("unsupported architecture")
	ErrAgentInUse      = errors.New("agent is attached to an approved host")
)

// Actor identifies who made a change, for audit events.
type Actor struct {
	Kind      string // user | policy | agent | system
	DiscordID string
	Name      string
}

// Host is a host joined with its agent.
type Host struct {
	ID              string
	AgentID         string
	DisplayName     string
	Docker          protocol.DockerInfo
	Providers       []protocol.ProviderStatus
	Approval        string
	ApprovedBy      string
	ApprovedAt      time.Time
	Connectivity    string
	LastInventoryAt time.Time // zero if none
	InventoryHash   string
	CreatedAt       time.Time
	RemovedAt       time.Time // zero unless removed

	AgentVersion    string
	ProtocolVersion int
	Capabilities    []string
	PollInterval    int
	AgentKey        string // pinned public key
	PendingAgentKey string // a different key waiting for a decision
	LastSeenAt      time.Time
}

// Removed reports whether the host has been removed.
func (h Host) Removed() bool { return !h.RemovedAt.IsZero() }

const hostColumns = `h.id, COALESCE(h.agent_id,''), h.display_name, h.docker_json, h.providers_json, h.approval,
	COALESCE(h.approved_by_discord_id,''), COALESCE(h.approved_at,''), h.connectivity,
	COALESCE(h.last_inventory_at,''), COALESCE(h.inventory_hash,''), h.created_at, COALESCE(h.removed_at,''),
	COALESCE(a.agent_version,''), COALESCE(a.protocol_version,0), COALESCE(a.capabilities_json,'[]'),
	COALESCE(a.poll_interval_seconds,0), COALESCE(a.last_seen_at,''),
	COALESCE(a.public_key,''), COALESCE(a.pending_public_key,'')`

const hostFrom = ` FROM hosts h LEFT JOIN agents a ON a.id = h.agent_id`

type scanner interface{ Scan(dest ...any) error }

func scanHost(r scanner) (Host, error) {
	var h Host
	var docker, providers, approvedAt, lastInv, created, removed, caps, lastSeen string
	err := r.Scan(&h.ID, &h.AgentID, &h.DisplayName, &docker, &providers, &h.Approval,
		&h.ApprovedBy, &approvedAt, &h.Connectivity, &lastInv, &h.InventoryHash, &created, &removed,
		&h.AgentVersion, &h.ProtocolVersion, &caps, &h.PollInterval, &lastSeen, &h.AgentKey, &h.PendingAgentKey)
	if err != nil {
		return h, err
	}
	json.Unmarshal([]byte(docker), &h.Docker)
	json.Unmarshal([]byte(providers), &h.Providers)
	json.Unmarshal([]byte(caps), &h.Capabilities)
	h.ApprovedAt = parseOpt(approvedAt)
	h.LastInventoryAt = parseOpt(lastInv)
	h.CreatedAt = parseOpt(created)
	h.RemovedAt = parseOpt(removed)
	h.LastSeenAt = parseOpt(lastSeen)
	return h, nil
}

// parseOpt parses a stored timestamp, returning the zero time for "".
func parseOpt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := ParseTime(s)
	return t
}

func optTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: FormatTime(t), Valid: true}
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func getHost(ctx context.Context, q querier, where string, args ...any) (Host, error) {
	h, err := scanHost(q.QueryRowContext(ctx, `SELECT `+hostColumns+hostFrom+` WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// Host returns one host by ID (removed hosts included).
func (s *Store) Host(ctx context.Context, id string) (Host, error) {
	return getHost(ctx, s.r, `h.id = ?`, id)
}

// HostByAgent returns the host currently bound to an agent.
func (s *Store) HostByAgent(ctx context.Context, agentID string) (Host, error) {
	return getHost(ctx, s.r, `h.agent_id = ?`, agentID)
}

// Hosts lists hosts ordered by name. Removed hosts are included only when
// includeRemoved is set.
func (s *Store) Hosts(ctx context.Context, includeRemoved bool) ([]Host, error) {
	q := `SELECT ` + hostColumns + hostFrom
	if !includeRemoved {
		q += ` WHERE h.removed_at IS NULL`
	}
	q += ` ORDER BY h.display_name COLLATE NOCASE, h.id`
	rows, err := s.r.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Poll is what one poll request tells the bot about its agent.
type Poll struct {
	AgentID         string
	AgentVersion    string
	ProtocolVersion int
	Capabilities    []string
	PollInterval    int
	HostName        string
	Docker          protocol.DockerInfo
	// PublicKey is the key to pin, from a request the shared key vouched for.
	// Empty when the agent's key is already pinned, or it is on protocol 1.
	PublicKey string
	At        time.Time
}

// PollResult says what a poll changed.
type PollResult struct {
	Host       Host
	Registered bool // the agent was new; a pending host was created
	Collision  bool // another host already has this display name
	CameOnline bool // the host was offline and is now online
	Removed    bool // the agent's host was removed: answer 403
}

// RecordPoll registers an unknown agent with a pending host, or refreshes a
// known agent and marks its host online.
func (s *Store) RecordPoll(ctx context.Context, p Poll) (PollResult, error) {
	var res PollResult
	caps, _ := json.Marshal(nonNil(p.Capabilities))
	docker, _ := json.Marshal(p.Docker)
	now := FormatTime(p.At)
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res = PollResult{}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE id = ?`, p.AgentID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO agents (id, display_name, agent_version, protocol_version, capabilities_json,
				                    poll_interval_seconds, first_seen_at, last_seen_at, public_key)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.AgentID, p.HostName, p.AgentVersion, p.ProtocolVersion, string(caps), p.PollInterval, now, now, nullString(p.PublicKey)); err != nil {
				return err
			}
			hostID := s.ids.New()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO hosts (id, agent_id, display_name, docker_json, approval, connectivity, created_at)
				VALUES (?, ?, ?, ?, 'pending', 'online', ?)`,
				hostID, p.AgentID, p.HostName, string(docker), now); err != nil {
				return err
			}
			if _, err := s.InsertAudit(ctx, tx, AuditEvent{
				At: p.At, ActorKind: "agent", Action: "host.registered", TargetType: "host", TargetID: hostID,
				Details: map[string]any{"agent_id": p.AgentID, "host_name": p.HostName, "docker": p.Docker},
			}); err != nil {
				return err
			}
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM hosts WHERE display_name = ? AND id <> ? AND removed_at IS NULL`,
				p.HostName, hostID).Scan(&n); err != nil {
				return err
			}
			res.Registered = true
			res.Collision = n > 0
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE agents SET agent_version = ?, protocol_version = ?, capabilities_json = ?,
				                  poll_interval_seconds = ?, last_seen_at = ?
				WHERE id = ?`,
				p.AgentVersion, p.ProtocolVersion, string(caps), p.PollInterval, now, p.AgentID); err != nil {
				return err
			}
			if p.PublicKey != "" {
				r, err := tx.ExecContext(ctx, `UPDATE agents SET public_key = ? WHERE id = ? AND public_key IS NULL`, p.PublicKey, p.AgentID)
				if err != nil {
					return err
				}
				if n, _ := r.RowsAffected(); n > 0 {
					if _, err := s.InsertAudit(ctx, tx, AuditEvent{At: p.At, ActorKind: "agent", Action: "agent.key_pinned", TargetType: "agent",
						TargetID: p.AgentID, Details: map[string]any{"fingerprint": protocol.KeyFingerprint(p.PublicKey)}}); err != nil {
						return err
					}
				}
			}
		}

		h, err := getHost(ctx, tx, `h.agent_id = ?`, p.AgentID)
		if errors.Is(err, ErrNotFound) {
			// The agent was detached by /host reassign-agent.
			res.Removed = true
			return nil
		}
		if err != nil {
			return err
		}
		if h.Removed() {
			res.Removed = true
			res.Host = h
			return nil
		}
		if h.Connectivity == domain.Offline {
			res.CameOnline = true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hosts SET docker_json = ?, connectivity = 'online' WHERE id = ?`,
			string(docker), h.ID); err != nil {
			return err
		}
		res.Host, err = getHost(ctx, tx, `h.id = ?`, h.ID)
		return err
	})
	return res, err
}

// HostsAwaitingInventory returns the online hosts whose last finished operation
// is newer than their last inventory, so what is stored for them may be out of
// date. Operations that never reached the agent changed nothing, and an offline
// host cannot report, so those do not count.
func (s *Store) HostsAwaitingInventory(ctx context.Context) (map[string]bool, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT h.id FROM hosts h WHERE h.removed_at IS NULL AND h.connectivity = 'online' AND EXISTS (
		SELECT 1 FROM operations o WHERE o.host_id = h.id AND o.finished_at IS NOT NULL
			AND o.state IN ('succeeded','failed','partially_failed','interrupted')
			AND o.finished_at > COALESCE(h.last_inventory_at,''))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ApproveHost approves a pending host. It returns
// alreadyApproved=true, and changes nothing, if the host was approved before.
func (s *Store) ApproveHost(ctx context.Context, hostID string, by Actor, at time.Time) (h Host, alreadyApproved bool, err error) {
	err = s.WriteTx(ctx, func(tx *sql.Tx) error {
		h, err = getHost(ctx, tx, `h.id = ?`, hostID)
		if err != nil {
			return err
		}
		if h.Removed() {
			return ErrHostRemoved
		}
		if h.Approval == domain.ApprovalApproved {
			alreadyApproved = true
			return nil
		}
		if !domain.ArchSupported(h.Docker.Arch) {
			return fmt.Errorf("%w %q: only %s hosts can be approved", ErrUnsupportedArch, h.Docker.Arch, domain.SupportedArches)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hosts SET approval = 'approved', approved_by_discord_id = ?, approved_at = ? WHERE id = ?`,
			nullString(by.DiscordID), FormatTime(at), hostID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, by, at, "host.approved", "host", hostID, map[string]any{"agent_id": h.AgentID}); err != nil {
			return err
		}
		h, err = getHost(ctx, tx, `h.id = ?`, hostID)
		return err
	})
	return h, alreadyApproved, err
}

// RemoveHost removes a host (also used by Reject). Its agent
// then receives 403 agent_removed. action is "host.removed" or "host.rejected".
func (s *Store) RemoveHost(ctx context.Context, hostID, action string, by Actor, at time.Time) (h Host, err error) {
	err = s.WriteTx(ctx, func(tx *sql.Tx) error {
		h, err = getHost(ctx, tx, `h.id = ?`, hostID)
		if err != nil {
			return err
		}
		if h.Removed() {
			return ErrHostRemoved
		}
		if _, err := s.releaseOperations(ctx, tx, hostID, by, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hosts SET removed_at = ? WHERE id = ?`, FormatTime(at), hostID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, by, at, action, "host", hostID,
			map[string]any{"agent_id": h.AgentID, "approval": h.Approval}); err != nil {
			return err
		}
		h, err = getHost(ctx, tx, `h.id = ?`, hostID)
		return err
	})
	return h, err
}

// RenameHost changes a host's display name.
func (s *Store) RenameHost(ctx context.Context, hostID, name string, by Actor, at time.Time) (Host, error) {
	var h Host
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		h, err = getHost(ctx, tx, `h.id = ?`, hostID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hosts SET display_name = ? WHERE id = ?`, name, hostID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, by, at, "host.renamed", "host", hostID,
			map[string]any{"from": h.DisplayName, "to": name}); err != nil {
			return err
		}
		h.DisplayName = name
		return nil
	})
	return h, err
}

// ReassignResult describes a completed agent reassignment.
type ReassignResult struct {
	Host        Host   // the host, now bound to the new agent
	OldAgentID  string // the agent that was detached ("" if none)
	RetiredHost string // the new agent's own pending host, removed in favour of Host
	// Released are the old agent's unfinished operations, now ended.
	Released []Operation
}

// ReassignAgent points an existing host at a replacement agent, keeping the host's UUID, approval, stacks, and history. The new
// agent may be one that has never polled (the next poll is then accepted as
// this host), or one that registered its own pending host — that host is
// removed and detached. The old agent is detached and receives 403.
func (s *Store) ReassignAgent(ctx context.Context, hostID, newAgentID string, by Actor, at time.Time) (ReassignResult, error) {
	var res ReassignResult
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res = ReassignResult{}
		h, err := getHost(ctx, tx, `h.id = ?`, hostID)
		if err != nil {
			return err
		}
		if h.Removed() {
			return ErrHostRemoved
		}
		if h.AgentID == newAgentID {
			res.Host = h
			return nil
		}
		res.OldAgentID = h.AgentID

		var agentExists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE id = ?`, newAgentID).Scan(&agentExists); err != nil {
			return err
		}
		if agentExists == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO agents (id, display_name, first_seen_at) VALUES (?, ?, ?)`,
				newAgentID, h.DisplayName, FormatTime(at)); err != nil {
				return err
			}
		}
		other, err := getHost(ctx, tx, `h.agent_id = ?`, newAgentID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		default:
			if other.Approval == domain.ApprovalApproved && !other.Removed() {
				return fmt.Errorf("%w %q; remove that host first", ErrAgentInUse, other.DisplayName)
			}
			removed := optTime(other.RemovedAt)
			if !removed.Valid {
				removed = optTime(at)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE hosts SET agent_id = NULL, removed_at = ? WHERE id = ?`, removed, other.ID); err != nil {
				return err
			}
			res.RetiredHost = other.ID
		}
		if res.Released, err = s.releaseOperations(ctx, tx, hostID, by, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hosts SET agent_id = ? WHERE id = ?`, newAgentID, hostID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, by, at, "host.agent_reassigned", "host", hostID, map[string]any{
			"from_agent_id": res.OldAgentID, "to_agent_id": newAgentID, "retired_host_id": res.RetiredHost,
		}); err != nil {
			return err
		}
		res.Host, err = getHost(ctx, tx, `h.id = ?`, hostID)
		return err
	})
	return res, err
}

// MarkOfflineHosts sets every online host whose agent has not polled within
// domain.OfflineAfter to offline, and returns them.
func (s *Store) MarkOfflineHosts(ctx context.Context, now time.Time) ([]Host, error) {
	var out []Host
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT `+hostColumns+hostFrom+
			` WHERE h.removed_at IS NULL AND h.connectivity = 'online' AND h.agent_id IS NOT NULL`)
		if err != nil {
			return err
		}
		var stale []Host
		for rows.Next() {
			h, err := scanHost(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if !h.LastSeenAt.IsZero() && domain.IsOffline(h.LastSeenAt, now, h.PollInterval) {
				stale = append(stale, h)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, h := range stale {
			if _, err := tx.ExecContext(ctx, `UPDATE hosts SET connectivity = 'offline' WHERE id = ?`, h.ID); err != nil {
				return err
			}
			h.Connectivity = domain.Offline
			out = append(out, h)
		}
		return nil
	})
	return out, err
}

func (s *Store) audit(ctx context.Context, tx *sql.Tx, by Actor, at time.Time, action, targetType, targetID string, details any) error {
	_, err := s.InsertAudit(ctx, tx, AuditEvent{
		At: at, ActorKind: by.Kind, ActorDiscordID: by.DiscordID, ActorName: by.Name,
		Action: action, TargetType: targetType, TargetID: targetID, Details: details,
	})
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
