package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
)

// GlobalPolicyID is the seeded global policy row.
const GlobalPolicyID = "00000000-0000-7000-8000-000000000001"

// Built-in global values that "clear" restores, since the global level must
// always define mode, tag_track, and the verify timings.
var globalDefaults = map[string]any{
	domain.FieldMode: domain.ModeManual, domain.FieldTagTrack: domain.TrackSameMajor,
	domain.FieldAutoRollback: nil, domain.FieldVerifyTimeout: 120, domain.FieldVerifyStable: 30,
}

var policyColumn = map[string]string{
	domain.FieldMode: "mode", domain.FieldTagTrack: "tag_track", domain.FieldAutoRollback: "auto_rollback",
	domain.FieldVerifyTimeout: "verify_timeout_seconds", domain.FieldVerifyStable: "verify_stable_seconds",
}

// PolicyKey identifies one policy row.
type PolicyKey struct {
	Level string // global | host | stack | group | container
	ID    string // empty for global
}

// Policies returns every policy row by key.
func (s *Store) Policies(ctx context.Context) (map[PolicyKey]domain.PolicyFields, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT scope_type, COALESCE(scope_id,''), mode, tag_track, auto_rollback,
		verify_timeout_seconds, verify_stable_seconds FROM policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPolicies(rows)
}

func scanPolicies(rows *sql.Rows) (map[PolicyKey]domain.PolicyFields, error) {
	out := map[PolicyKey]domain.PolicyFields{}
	for rows.Next() {
		var k PolicyKey
		var mode, track sql.NullString
		var ar, vt, vs sql.NullInt64
		if err := rows.Scan(&k.Level, &k.ID, &mode, &track, &ar, &vt, &vs); err != nil {
			return nil, err
		}
		var f domain.PolicyFields
		if mode.Valid {
			f.Mode = &mode.String
		}
		if track.Valid {
			f.TagTrack = &track.String
		}
		if ar.Valid {
			b := ar.Int64 != 0
			f.AutoRollback = &b
		}
		if vt.Valid {
			v := int(vt.Int64)
			f.VerifyTimeout = &v
		}
		if vs.Valid {
			v := int(vs.Int64)
			f.VerifyStable = &v
		}
		out[k] = f
	}
	return out, rows.Err()
}

// Chain builds the policy chain for a scope from all policy rows: global,
// then host, stack, group, and container when their IDs are given.
func Chain(all map[PolicyKey]domain.PolicyFields, hostID, stackID, groupID, containerID string) []domain.PolicyLevel {
	chain := []domain.PolicyLevel{{Level: domain.LevelGlobal, Fields: all[PolicyKey{Level: domain.LevelGlobal}]}}
	for _, l := range []struct{ level, id string }{
		{domain.LevelHost, hostID}, {domain.LevelStack, stackID}, {domain.LevelGroup, groupID}, {domain.LevelContainer, containerID},
	} {
		if l.id == "" {
			continue
		}
		chain = append(chain, domain.PolicyLevel{Level: l.level, Fields: all[PolicyKey{l.level, l.id}]})
	}
	return chain
}

// SetPolicy sets the given (non-nil) fields at one level, leaving the rest
// unchanged, and audits the change.
func (s *Store) SetPolicy(ctx context.Context, key PolicyKey, f domain.PolicyFields, by Actor, at time.Time) error {
	changes := map[string]any{}
	if f.Mode != nil {
		changes[domain.FieldMode] = *f.Mode
	}
	if f.TagTrack != nil {
		changes[domain.FieldTagTrack] = *f.TagTrack
	}
	if f.AutoRollback != nil {
		changes[domain.FieldAutoRollback] = *f.AutoRollback
	}
	if f.VerifyTimeout != nil {
		changes[domain.FieldVerifyTimeout] = *f.VerifyTimeout
	}
	if f.VerifyStable != nil {
		changes[domain.FieldVerifyStable] = *f.VerifyStable
	}
	if len(changes) == 0 {
		return errors.New("no policy fields given")
	}
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		id, err := s.ensurePolicyRow(ctx, tx, key, at)
		if err != nil {
			return err
		}
		for field, v := range changes {
			if b, ok := v.(bool); ok {
				v = boolInt(b)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE policies SET %s = ?, updated_at = ? WHERE id = ?`, policyColumn[field]),
				v, FormatTime(at), id); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, by, at, "policy.set", key.Level, policyTarget(key),
			map[string]any{"level": key.Level, "set": changes})
	})
}

// ClearPolicy unsets one field (or every field when field is "") at a level,
// so it inherits again. At the global level it restores the built-in default.
// It reports whether anything was set before.
func (s *Store) ClearPolicy(ctx context.Context, key PolicyKey, field string, by Actor, at time.Time) (bool, error) {
	fields := []string{domain.FieldMode, domain.FieldTagTrack, domain.FieldAutoRollback, domain.FieldVerifyTimeout, domain.FieldVerifyStable}
	if field != "" {
		if _, ok := policyColumn[field]; !ok {
			return false, fmt.Errorf("unknown policy field %q", field)
		}
		fields = []string{field}
	}
	all, err := s.Policies(ctx)
	if err != nil {
		return false, err
	}
	if _, exists := all[key]; !exists {
		return false, nil
	}
	err = s.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, f := range fields {
			var v any
			if key.Level == domain.LevelGlobal {
				v = globalDefaults[f]
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE policies SET %s = ?, updated_at = ? WHERE %s`,
				policyColumn[f], policyWhere(key)), append([]any{v, FormatTime(at)}, policyArgs(key)...)...); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, by, at, "policy.cleared", key.Level, policyTarget(key),
			map[string]any{"level": key.Level, "fields": fields})
	})
	return err == nil, err
}

func (s *Store) ensurePolicyRow(ctx context.Context, tx *sql.Tx, key PolicyKey, at time.Time) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM policies WHERE `+policyWhere(key), policyArgs(key)...).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = s.ids.New()
	_, err = tx.ExecContext(ctx, `INSERT INTO policies (id, scope_type, scope_id, updated_at) VALUES (?, ?, ?, ?)`,
		id, key.Level, nullString(key.ID), FormatTime(at))
	return id, err
}

func policyWhere(k PolicyKey) string {
	if k.Level == domain.LevelGlobal {
		return `scope_type = 'global'`
	}
	return `scope_type = ? AND scope_id = ?`
}

func policyArgs(k PolicyKey) []any {
	if k.Level == domain.LevelGlobal {
		return nil
	}
	return []any{k.Level, k.ID}
}

func policyTarget(k PolicyKey) string {
	if k.Level == domain.LevelGlobal {
		return GlobalPolicyID
	}
	return k.ID
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// maintenanceKey is the system_settings key for maintenance mode.
const maintenanceKey = "maintenance"

// Maintenance reports whether maintenance mode is on.
func (s *Store) Maintenance(ctx context.Context) (bool, error) {
	var v struct {
		On bool `json:"on"`
	}
	_, err := s.Setting(ctx, maintenanceKey, &v)
	return v.On, err
}

// SetMaintenance turns maintenance mode on or off and audits the change. It
// reports whether the state changed.
func (s *Store) SetMaintenance(ctx context.Context, on bool, by Actor, at time.Time) (bool, error) {
	changed := false
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value_json FROM system_settings WHERE key = ?`, maintenanceKey).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var cur struct {
			On bool `json:"on"`
		}
		json.Unmarshal([]byte(raw), &cur)
		if cur.On == on {
			return nil
		}
		changed = true
		b, _ := json.Marshal(struct {
			On bool `json:"on"`
		}{on})
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings (key, value_json, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
			maintenanceKey, string(b), FormatTime(at)); err != nil {
			return err
		}
		action := "maintenance.disabled"
		if on {
			action = "maintenance.enabled"
		}
		return s.audit(ctx, tx, by, at, action, "system", "maintenance", nil)
	})
	return changed, err
}
