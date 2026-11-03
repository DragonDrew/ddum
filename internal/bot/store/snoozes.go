package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// SnoozeUpdates snoozes each given update that is still detected, until the
// given time (zero means indefinitely). An existing active snooze is
// replaced. Each change is audited. It returns the IDs snoozed.
func (s *Store) SnoozeUpdates(ctx context.Context, updateIDs []string, until time.Time, by Actor, at time.Time) ([]string, error) {
	var done []string
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		done = nil
		for _, id := range updateIDs {
			var status string
			err := tx.QueryRowContext(ctx, `SELECT status FROM updates WHERE id = ?`, id).Scan(&status)
			if errors.Is(err, sql.ErrNoRows) || status != StatusDetected {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := s.removeActiveSnoozes(ctx, tx, id, by, at); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO snoozes (id, update_id, until, created_by_discord_id, created_at)
				VALUES (?, ?, ?, ?, ?)`, s.ids.New(), id, optTime(until), by.DiscordID, FormatTime(at)); err != nil {
				return err
			}
			details := map[string]any{"until": "indefinite"}
			if !until.IsZero() {
				details["until"] = FormatTime(until)
			}
			if err := s.audit(ctx, tx, by, at, "snooze.created", "update", id, details); err != nil {
				return err
			}
			done = append(done, id)
		}
		return nil
	})
	return done, err
}

// Unsnooze removes the active snooze of one update. It reports whether there
// was one.
func (s *Store) Unsnooze(ctx context.Context, updateID string, by Actor, at time.Time) (bool, error) {
	var n int
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = s.removeActiveSnoozes(ctx, tx, updateID, by, at)
		return err
	})
	return n > 0, err
}

// UnsnoozeAll removes every active snooze and returns how many it removed.
func (s *Store) UnsnoozeAll(ctx context.Context, by Actor, at time.Time) (int, error) {
	total := 0
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		total = 0
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT update_id FROM snoozes
			WHERE removed_at IS NULL AND (until IS NULL OR until > ?)`, FormatTime(at))
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			n, err := s.removeActiveSnoozes(ctx, tx, id, by, at)
			if err != nil {
				return err
			}
			total += n
		}
		return nil
	})
	return total, err
}

func (s *Store) removeActiveSnoozes(ctx context.Context, tx *sql.Tx, updateID string, by Actor, at time.Time) (int, error) {
	r, err := tx.ExecContext(ctx, `UPDATE snoozes SET removed_at = ?, removed_by_discord_id = ?
		WHERE update_id = ? AND removed_at IS NULL AND (until IS NULL OR until > ?)`,
		FormatTime(at), nullString(by.DiscordID), updateID, FormatTime(at))
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		if err := s.audit(ctx, tx, by, at, "snooze.removed", "update", updateID, nil); err != nil {
			return 0, err
		}
	}
	return int(n), nil
}

// ExpiredSnooze is a timed snooze that has run out.
type ExpiredSnooze struct {
	Update Update
	Until  time.Time
}

// TakeExpiredSnoozes returns timed snoozes that expired at or before now and
// have not been announced, marking them announced.
func (s *Store) TakeExpiredSnoozes(ctx context.Context, now time.Time) ([]ExpiredSnooze, error) {
	var out []ExpiredSnooze
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT id, update_id, until FROM snoozes
			WHERE removed_at IS NULL AND until IS NOT NULL AND until <= ? AND expiry_notified_at IS NULL`, FormatTime(now))
		if err != nil {
			return err
		}
		type row struct{ id, update, until string }
		var rs []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.update, &r.until); err != nil {
				rows.Close()
				return err
			}
			rs = append(rs, r)
		}
		rows.Close()
		for _, r := range rs {
			if _, err := tx.ExecContext(ctx, `UPDATE snoozes SET expiry_notified_at = ? WHERE id = ?`, FormatTime(now), r.id); err != nil {
				return err
			}
			u, err := getUpdate(ctx, tx, r.update, now)
			if err != nil {
				return err
			}
			if u.Status == StatusDetected {
				out = append(out, ExpiredSnooze{Update: u, Until: parseOpt(r.until)})
			}
		}
		return nil
	})
	return out, err
}

// OfferItem is one update an offer covers, with the digest shown.
type OfferItem struct {
	UpdateID     string `json:"update_id"`
	TargetDigest string `json:"target_digest"`
}

// Offer is what a message's buttons act on: the exact
// updates and digests the message displayed.
type Offer struct {
	ID        string
	ScopeType string
	ScopeID   string
	Items     []OfferItem
	CreatedAt time.Time
}

// CreateOffer records an offer and returns its ID. createdBy is
// notification, command, or policy.
func (s *Store) CreateOffer(ctx context.Context, scopeType, scopeID string, items []OfferItem, createdBy string, at time.Time) (string, error) {
	b, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	id := s.ids.New()
	err = s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO approval_offers (id, scope_type, scope_id, items_json, created_by_kind, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, scopeType, scopeID, string(b), createdBy, FormatTime(at))
		return err
	})
	return id, err
}

// GetOffer returns an offer by ID.
func (s *Store) GetOffer(ctx context.Context, id string) (Offer, error) {
	var o Offer
	var items, created string
	err := s.r.QueryRowContext(ctx, `SELECT id, scope_type, scope_id, items_json, created_at FROM approval_offers WHERE id = ?`, id).
		Scan(&o.ID, &o.ScopeType, &o.ScopeID, &items, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	o.CreatedAt = parseOpt(created)
	return o, json.Unmarshal([]byte(items), &o.Items)
}
