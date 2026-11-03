package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Errors returned by MergeStacks.
var (
	ErrSameStack    = errors.New("a stack cannot be merged into itself")
	ErrStackRetired = errors.New("stack already retired by an earlier merge")
	ErrMergeBusy    = errors.New("an operation is in progress for one of the stacks")
)

// MergeResult describes a completed merge.
type MergeResult struct {
	Survivor Stack  // the older stack, now with the newer one's identity
	Retired  string // the newer stack's UUID, now retired
	Matched  int    // containers re-bound by identity key
	Moved    int    // containers moved over unmatched
}

// MergeStacks merges two stacks for the cases fingerprinting cannot handle,
// renamed Compose projects and host moves. The older UUID
// survives; its fingerprint, host, provider columns, and container bindings
// become those of the newer stack, and the newer row is retired. The
// retired row's fingerprint is rewritten first, so UNIQUE (host_id,
// fingerprint) holds at every step. Policies, groups, snoozes, updates,
// desired states, snapshots, and operations of the newer stack are
// re-pointed to the survivor in the same transaction. Audit events are
// immutable; the stack.merged event records the ID mapping instead.
func (s *Store) MergeStacks(ctx context.Context, aID, bID string, by Actor, at time.Time) (MergeResult, error) {
	var res MergeResult
	if aID == bID {
		return res, ErrSameStack
	}
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res = MergeResult{}
		load := func(id string) (Stack, string, error) {
			st, err := scanStack(tx.QueryRowContext(ctx, `SELECT `+stackColumns+stackFrom+` WHERE s.id = ?`, id))
			if errors.Is(err, sql.ErrNoRows) {
				return st, "", ErrNotFound
			}
			var retired string
			tx.QueryRowContext(ctx, `SELECT COALESCE(retired_at,'') FROM stacks WHERE id = ?`, id).Scan(&retired)
			return st, retired, err
		}
		a, ra, err := load(aID)
		if err != nil {
			return err
		}
		b, rb, err := load(bID)
		if err != nil {
			return err
		}
		if ra != "" || rb != "" {
			return ErrStackRetired
		}
		old, newer := a, b
		if b.CreatedAt.Before(a.CreatedAt) || (b.CreatedAt.Equal(a.CreatedAt) && b.ID < a.ID) {
			old, newer = b, a
		}
		var busy int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM desired_states d JOIN containers c ON c.id = d.container_id
			WHERE d.status = 'pending' AND c.stack_id IN (?, ?)`, old.ID, newer.ID).Scan(&busy); err != nil {
			return err
		}
		if busy > 0 {
			return ErrMergeBusy
		}
		now := FormatTime(at)

		// 1. Retire the newer row, freeing its (host, fingerprint).
		if _, err := tx.ExecContext(ctx, `UPDATE stacks SET fingerprint = fingerprint || '#retired:' || id, retired_at = ?, updated_at = ?
			WHERE id = ?`, now, now, newer.ID); err != nil {
			return err
		}
		// 2. The survivor takes the newer stack's identity and provider data.
		if _, err := tx.ExecContext(ctx, `UPDATE stacks SET (host_id, fingerprint, provider, provider_ref_json, compose_json,
			deploy_supported, deploy_reason, rollback_available, presence, missing_since, updated_at) =
			(SELECT host_id, ?, provider, provider_ref_json, compose_json, deploy_supported, deploy_reason, rollback_available,
			        presence, missing_since, ? FROM stacks WHERE id = ?) WHERE id = ?`,
			newer.Fingerprint, now, newer.ID, old.ID); err != nil {
			return err
		}
		// 3. Containers: re-bind by identity key, or move unmatched ones over.
		mapping := map[string]string{}
		rows, err := tx.QueryContext(ctx, `SELECT n.id, COALESCE(o.id,'') FROM containers n
			LEFT JOIN containers o ON o.stack_id = ? AND o.identity_key = n.identity_key WHERE n.stack_id = ?`, old.ID, newer.ID)
		if err != nil {
			return err
		}
		type pair struct{ newC, oldC string }
		var pairs []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.newC, &p.oldC); err != nil {
				rows.Close()
				return err
			}
			pairs = append(pairs, p)
		}
		rows.Close()
		for _, p := range pairs {
			if p.oldC == "" {
				if _, err := tx.ExecContext(ctx, `UPDATE containers SET stack_id = ?, updated_at = ? WHERE id = ?`, old.ID, now, p.newC); err != nil {
					return err
				}
				res.Moved++
				continue
			}
			mapping[p.newC] = p.oldC
			if err := s.rebindContainer(ctx, tx, p.newC, p.oldC, now); err != nil {
				return err
			}
			res.Matched++
		}
		// 4. Stack-level history.
		if err := movePolicy(ctx, tx, "stack", newer.ID, old.ID); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE updates SET stack_id = ? WHERE stack_id = ?`,
			`UPDATE operations SET scope_id = ? WHERE scope_type = 'stack' AND scope_id = ?`,
			`UPDATE rollback_snapshots SET stack_id = ? WHERE stack_id = ?`,
			`UPDATE approval_offers SET scope_id = ? WHERE scope_type = 'stack' AND scope_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, old.ID, newer.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE updates SET host_id = (SELECT host_id FROM stacks WHERE id = ?) WHERE stack_id = ?`,
			old.ID, old.ID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, by, at, "stack.merged", "stack", old.ID, map[string]any{
			"survivor": old.ID, "retired": newer.ID, "from_fingerprint": old.Fingerprint, "to_fingerprint": newer.Fingerprint,
			"from_host": old.HostID, "to_host": newer.HostID, "containers": mapping,
		}); err != nil {
			return err
		}
		res.Retired = newer.ID
		res.Survivor, err = scanStack(tx.QueryRowContext(ctx, `SELECT `+stackColumns+stackFrom+` WHERE s.id = ?`, old.ID))
		return err
	})
	return res, err
}

// rebindContainer makes the surviving container oldC carry the newer
// container newC's runtime state and history; newC stays on the retired
// stack, missing.
func (s *Store) rebindContainer(ctx context.Context, tx *sql.Tx, newC, oldC, now string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE containers SET (docker_id, docker_name, image_ref, image_repository,
		running_digests_json, platform, runtime_state, health, protected, self_role, ddum_labels_json, notes_json,
		tag_change_json, tracking, untracked_reason, last_check_at, last_check_error, presence, last_seen_at, updated_at) =
		(SELECT docker_id, docker_name, image_ref, image_repository, running_digests_json, platform, runtime_state, health,
		        protected, self_role, ddum_labels_json, notes_json, tag_change_json, tracking, untracked_reason, last_check_at,
		        last_check_error, presence, last_seen_at, ? FROM containers WHERE id = ?) WHERE id = ?`, now, newC, oldC); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE containers SET presence = 'missing', updated_at = ? WHERE id = ?`, now, newC); err != nil {
		return err
	}
	// Updates: move those whose digest the survivor does not already have.
	if _, err := tx.ExecContext(ctx, `UPDATE updates SET container_id = ? WHERE container_id = ?
		AND target_digest NOT IN (SELECT target_digest FROM updates WHERE container_id = ?)`, oldC, newC, oldC); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'superseded' WHERE container_id = ? AND status = 'detected'`, newC); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE desired_states SET container_id = ? WHERE container_id = ? AND status <> 'pending'`, oldC, newC); err != nil {
		return err
	}
	// Group: the survivor keeps its own membership; otherwise it inherits.
	var oldGroup int
	tx.QueryRowContext(ctx, `SELECT count(*) FROM update_group_members WHERE container_id = ?`, oldC).Scan(&oldGroup)
	if oldGroup == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE update_group_members SET container_id = ? WHERE container_id = ?`, oldC, newC); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM update_group_members WHERE container_id = ?`, newC); err != nil {
		return err
	}
	return movePolicy(ctx, tx, "container", newC, oldC)
}

// movePolicy moves a policy row from one scope ID to another, unless the
// target already has one (the survivor's own policy wins).
func movePolicy(ctx context.Context, tx *sql.Tx, level, from, to string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM policies WHERE scope_type = ? AND scope_id = ?`, level, to).Scan(&n); err != nil {
		return err
	}
	q := `UPDATE policies SET scope_id = ? WHERE scope_type = ? AND scope_id = ?`
	args := []any{to, level, from}
	if n > 0 {
		q, args = `DELETE FROM policies WHERE scope_type = ? AND scope_id = ?`, []any{level, from}
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("move %s policy: %w", level, err)
	}
	return nil
}
