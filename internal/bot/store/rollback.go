package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Errors returned by rollback handling.
var (
	ErrSnapshotUnavailable = errors.New("no usable snapshot")
	ErrNotLatest           = errors.New("a later operation changed this stack")
	ErrNotRollbackable     = errors.New("this operation cannot be rolled back")
	ErrChangedOutside      = errors.New("a container changed outside ddum since this deploy")
)

// Snapshot statuses.
const (
	SnapshotAvailable   = "available"
	SnapshotConsumed    = "consumed"
	SnapshotPruned      = "pruned"
	SnapshotUnavailable = "unavailable"
)

// Snapshot is a rollback_snapshots row.
type Snapshot struct {
	ID          string
	OperationID string
	StackID     string
	HostID      string
	Status      string
	CreatedAt   time.Time
}

func holdKey(stackID string) string { return "rollback_hold:" + stackID }

// rollbackHold returns why a stack is held after a failed rollback, or "".
func rollbackHold(ctx context.Context, q querier, stackID string) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value_json FROM system_settings WHERE key = ?`, holdKey(stackID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "a rollback of this stack failed; check the stack, then clear the hold (/rollback list)", nil
}

// RollbackHold reports the operation whose failed rollback holds a stack.
func (s *Store) RollbackHold(ctx context.Context, stackID string) (string, bool, error) {
	var v struct {
		OperationID string `json:"operation_id"`
	}
	found, err := s.Setting(ctx, holdKey(stackID), &v)
	return v.OperationID, found, err
}

// ClearRollbackHold lifts a failed-rollback hold.
func (s *Store) ClearRollbackHold(ctx context.Context, stackID string, by Actor, at time.Time) (bool, error) {
	cleared := false
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM system_settings WHERE key = ?`, holdKey(stackID))
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if cleared = n > 0; cleared {
			return s.audit(ctx, tx, by, at, "rollback.hold_cleared", "stack", stackID, nil)
		}
		return nil
	})
	return cleared, err
}

// afterFinish records snapshots and applies rollback outcomes, inside the
// transaction that finishes the operation.
func (s *Store) afterFinish(ctx context.Context, tx *sql.Tx, op Operation, state string, out OperationOutcome, at time.Time) error {
	now := FormatTime(at)
	if out.Result != nil && op.Kind == protocol.CommandDeploy {
		for _, sn := range out.Result.Snapshots {
			stackID := sn.StackID
			if stackID == "" {
				stackID = op.ScopeID
			}
			status := SnapshotAvailable
			if out.Result.RolledBack {
				status = SnapshotConsumed // the agent already restored it
			}
			summary, _ := json.Marshal(map[string]any{"targets": op.Payload.Targets})
			if _, err := tx.ExecContext(ctx, `INSERT INTO rollback_snapshots (id, operation_id, stack_id, host_id, status, summary_json, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
				sn.SnapshotID, op.ID, stackID, op.HostID, status, string(summary), now); err != nil {
				return err
			}
		}
	}
	if op.Kind != protocol.CommandRollback {
		return nil
	}
	var orig string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(original_operation_id,'') FROM operations WHERE id = ?`, op.ID).Scan(&orig); err != nil {
		return err
	}
	if state == protocol.OpSucceeded {
		// The restored state is the new baseline; the updates may be decided
		// again or snoozed.
		if _, err := tx.ExecContext(ctx, `UPDATE rollback_snapshots SET status = 'consumed' WHERE operation_id = ?`, orig); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'detected', approved_by_kind = NULL, approved_by_discord_id = NULL,
			approved_at = NULL, operation_id = NULL, notified_at = NULL, updated_at = ? WHERE operation_id = ?`, now, orig); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM system_settings WHERE key = ?`, holdKey(op.ScopeID)); err != nil {
			return err
		}
		return s.audit(ctx, tx, Actor{Kind: "agent"}, at, "rollback.completed", "operation", op.ID, map[string]any{"original_operation_id": orig})
	}
	// A rollback that was refused before it changed anything leaves the stack
	// as it was, so there is nothing to check.
	if ChangedNothing(state, out.Code) {
		return s.audit(ctx, tx, Actor{Kind: "agent"}, at, "rollback.refused", "operation", op.ID,
			map[string]any{"original_operation_id": orig, "code": out.Code})
	}
	// A failed rollback holds the stack until a user acts.
	b, _ := json.Marshal(map[string]string{"operation_id": op.ID})
	if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings (key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		holdKey(op.ScopeID), string(b), now); err != nil {
		return err
	}
	return s.audit(ctx, tx, Actor{Kind: "agent"}, at, "rollback.failed", "operation", op.ID,
		map[string]any{"original_operation_id": orig, "state": state})
}

// syncSnapshots applies the snapshot IDs an inventory reports usable for a
// stack: listed ones are available, missing ones unavailable.
func (s *Store) syncSnapshots(ctx context.Context, tx *sql.Tx, stackID string, usable []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, status FROM rollback_snapshots WHERE stack_id = ? AND status IN ('available','unavailable')`, stackID)
	if err != nil {
		return err
	}
	type row struct{ id, status string }
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.status); err != nil {
			rows.Close()
			return err
		}
		rs = append(rs, r)
	}
	rows.Close()
	for _, r := range rs {
		want := SnapshotUnavailable
		if slices.Contains(usable, r.id) {
			want = SnapshotAvailable
		}
		if want != r.status {
			if _, err := tx.ExecContext(ctx, `UPDATE rollback_snapshots SET status = ? WHERE id = ?`, want, r.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Snapshots returns an operation's snapshot records.
func (s *Store) Snapshots(ctx context.Context, opID string) ([]Snapshot, error) {
	return snapshotsOf(ctx, s.r, opID)
}

func snapshotsOf(ctx context.Context, q querier, opID string) ([]Snapshot, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, operation_id, stack_id, host_id, status, created_at FROM rollback_snapshots
		WHERE operation_id = ? ORDER BY created_at, id`, opID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		var created string
		if err := rows.Scan(&sn.ID, &sn.OperationID, &sn.StackID, &sn.HostID, &sn.Status, &created); err != nil {
			return nil, err
		}
		sn.CreatedAt = parseOpt(created)
		out = append(out, sn)
	}
	return out, rows.Err()
}

// rollbackable checks the rollback rules for one finished deploy:
// every snapshot available, and no later deploy or rollback of the scope
// (so restoring cannot knowingly undo later changes).
func rollbackable(ctx context.Context, q querier, op Operation) ([]Snapshot, error) {
	if op.Kind != protocol.CommandDeploy {
		return nil, fmt.Errorf("%w: only deployments are rolled back", ErrNotRollbackable)
	}
	switch op.State {
	case protocol.OpSucceeded, protocol.OpFailed, protocol.OpPartiallyFailed, protocol.OpInterrupted:
	default:
		return nil, fmt.Errorf("%w: the operation is %s", ErrNotRollbackable, op.State)
	}
	snaps, err := snapshotsOf(ctx, q, op.ID)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("%w: the agent took no snapshot", ErrSnapshotUnavailable)
	}
	for _, sn := range snaps {
		if sn.Status != SnapshotAvailable {
			return nil, fmt.Errorf("%w: snapshot %s is %s", ErrSnapshotUnavailable, sn.ID[:8], sn.Status)
		}
	}
	// A later operation on the same scope, or on any stack it touched (a group
	// spans stacks), makes this one no longer the latest.
	scopes := []any{op.ScopeID}
	for _, t := range op.Payload.Targets {
		scopes = append(scopes, t.StackID)
	}
	// A snapshot restores whole files, so a later group operation that touched
	// one of these stacks counts as well, though its scope is the group.
	in := `(?` + strings.Repeat(",?", len(scopes)-1) + `)`
	args := append(append([]any{op.ID, FormatTime(op.CreatedAt)}, scopes...), scopes...)
	var later int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM operations o WHERE o.id <> ? AND o.created_at >= ?
		AND o.state NOT IN ('cancelled','expired') AND (o.scope_id IN `+in+` OR EXISTS (
			SELECT 1 FROM json_each(o.payload_json, '$.targets') t WHERE json_extract(t.value, '$.stack_id') IN `+in+`))`,
		args...).Scan(&later); err != nil {
		return nil, err
	}
	if later > 0 {
		return nil, ErrNotLatest
	}
	if err := checkUnchangedOutside(ctx, q, op); err != nil {
		return nil, err
	}
	return snaps, nil
}

// checkUnchangedOutside refuses a rollback when a container runs neither what
// the deployment put there nor what it replaced: it was changed by hand since,
// and restoring the snapshot would silently undo that.
func checkUnchangedOutside(ctx context.Context, q querier, op Operation) error {
	for _, t := range op.Payload.Targets {
		for _, svc := range t.Services {
			if svc.Current.Digest == "" {
				continue
			}
			var raw string
			err := q.QueryRowContext(ctx, `SELECT running_digests_json FROM containers WHERE id = ?`, svc.ContainerID).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			var running []string
			if json.Unmarshal([]byte(raw), &running) != nil || len(running) == 0 {
				continue
			}
			if !runsDigest(running, svc.Desired.Digest) && !runsDigest(running, svc.Current.Digest) {
				return fmt.Errorf("%w: %s", ErrChangedOutside, svc.IdentityKey)
			}
		}
	}
	return nil
}

// RollbackCandidates lists finished deployments that can be rolled back now.
func (s *Store) RollbackCandidates(ctx context.Context) ([]Operation, error) {
	ops, err := s.Operations(ctx, OperationFilter{Limit: 200})
	if err != nil {
		return nil, err
	}
	var out []Operation
	seen := map[string]bool{}
	for _, op := range ops { // newest first
		if seen[op.ScopeID] || op.State == protocol.OpCancelled || op.State == protocol.OpExpired {
			continue
		}
		seen[op.ScopeID] = true
		if _, err := rollbackable(ctx, s.r, op); err == nil {
			out = append(out, op)
		}
	}
	return out, nil
}

// CanRollBack reports whether an operation can be rolled back now, and why not.
func (s *Store) CanRollBack(ctx context.Context, opID string) error {
	op, err := getOp(ctx, s.r, opID)
	if err != nil {
		return err
	}
	_, err = rollbackable(ctx, s.r, op)
	return err
}

// CreateRollback queues a rollback of a finished deployment.
// The rollback restores exactly the services that operation changed.
func (s *Store) CreateRollback(ctx context.Context, origID string, by Actor, at time.Time, expiryMinutes int) (Operation, error) {
	var out Operation
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		orig, err := getOp(ctx, tx, origID)
		if err != nil {
			return err
		}
		snaps, err := rollbackable(ctx, tx, orig)
		if err != nil {
			return err
		}
		host, err := getHost(ctx, tx, `h.id = ?`, orig.HostID)
		if err != nil {
			return err
		}
		switch {
		case host.Removed() || host.Approval != domain.ApprovalApproved:
			return fmt.Errorf("%w: the host is not approved", ErrNotRollbackable)
		case host.Connectivity != domain.Online:
			return fmt.Errorf("%w: the agent is offline", ErrNotRollbackable)
		case !slices.Contains(host.Capabilities, protocol.CapActionRollback):
			return fmt.Errorf("%w: this agent version cannot roll back", ErrNotRollbackable)
		}
		var targets []protocol.DeployTarget
		for _, t := range orig.Payload.Targets {
			nt := protocol.DeployTarget{StackID: t.StackID, StackFingerprint: t.StackFingerprint}
			for _, svc := range t.Services {
				var busy int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM desired_states WHERE container_id = ? AND status = 'pending'`,
					svc.ContainerID).Scan(&busy); err != nil {
					return err
				}
				if busy > 0 {
					return fmt.Errorf("%w: another operation for %s is in progress", ErrNotRollbackable, svc.IdentityKey)
				}
				nt.Services = append(nt.Services, protocol.ServiceChange{ContainerID: svc.ContainerID, IdentityKey: svc.IdentityKey,
					Current: svc.Desired, Desired: svc.Current})
			}
			targets = append(targets, nt)
		}
		var snapIDs []string
		for _, sn := range snaps {
			snapIDs = append(snapIDs, sn.ID)
		}
		id := s.ids.New()
		expires := at.Add(time.Duration(expiryMinutes) * time.Minute)
		payload := protocol.RollbackPayload{OperationID: id, OriginalOperationID: orig.ID, SnapshotIDs: snapIDs,
			Scope: orig.Payload.Scope, IssuedAt: at.UTC().Format(time.RFC3339), ExpiresAt: expires.UTC().Format(time.RFC3339),
			Verify: orig.Payload.Verify, Targets: targets}
		pj, _ := json.Marshal(payload)
		now := FormatTime(at)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO operations (id, kind, agent_id, host_id, scope_type, scope_id, original_operation_id, payload_json, state,
			                        attempt, expires_at, initiator_kind, initiator_discord_id, created_at, updated_at)
			VALUES (?, 'rollback', ?, ?, ?, ?, ?, ?, 'queued', 0, ?, ?, ?, ?, ?)`,
			id, host.AgentID, host.ID, orig.ScopeType, orig.ScopeID, orig.ID, string(pj), FormatTime(expires),
			by.Kind, nullString(by.DiscordID), now, now); err != nil {
			return err
		}
		for _, t := range targets {
			for _, svc := range t.Services {
				if _, err := tx.ExecContext(ctx, `INSERT INTO desired_states (id, container_id, image_ref, image_digest, status, operation_id, set_at)
					VALUES (?, ?, ?, ?, 'pending', ?, ?)`, s.ids.New(), svc.ContainerID, svc.Desired.ImageRef, svc.Desired.Digest, id, now); err != nil {
					return err
				}
			}
		}
		action := "rollback.requested"
		if by.Kind == "policy" {
			action = "rollback.auto_requested"
		}
		if err := s.audit(ctx, tx, by, at, action, "operation", id, map[string]any{"original_operation_id": orig.ID,
			"snapshots": strings.Join(snapIDs, ",")}); err != nil {
			return err
		}
		out, err = getOp(ctx, tx, id)
		return err
	})
	return out, err
}

// CreateSelfUpdateOffer records an offer for one bot update: it is
// approved alone, by a user, after a confirmation.
func (s *Store) CreateSelfUpdateOffer(ctx context.Context, u Update, at time.Time) (string, error) {
	b, _ := json.Marshal([]OfferItem{{UpdateID: u.ID, TargetDigest: u.TargetDigest}})
	id := s.ids.New()
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO approval_offers (id, scope_type, scope_id, items_json, self_update, created_by_kind, created_at)
			VALUES (?, 'stack', ?, ?, 1, 'command', ?)`, id, u.StackID, string(b), FormatTime(at))
		return err
	})
	return id, err
}
