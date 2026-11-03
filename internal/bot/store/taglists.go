package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// TagList is a repository's tag list kept across check cycles and restarts
// .
type TagList struct {
	Tags   []string  // version tags, in listing order; nothing else is selectable
	Tail   []string  // the last two tags as listed, of any kind
	Latest string    // digest of latest when listed; "" when it had none
	At     time.Time // when listed, or last extended
}

// TagList returns the kept tag list of repo.
func (s *Store) TagList(ctx context.Context, repo string) (TagList, bool, error) {
	var l TagList
	var tags, tail, at string
	err := s.r.QueryRowContext(ctx, `SELECT tags_json, tail_json, latest_digest, listed_at
		FROM registry_tag_lists WHERE repository = ?`, repo).Scan(&tags, &tail, &l.Latest, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return TagList{}, false, nil
	}
	if err != nil {
		return TagList{}, false, err
	}
	if err := json.Unmarshal([]byte(tags), &l.Tags); err != nil {
		return TagList{}, false, err
	}
	if err := json.Unmarshal([]byte(tail), &l.Tail); err != nil {
		return TagList{}, false, err
	}
	if l.At, err = ParseTime(at); err != nil {
		return TagList{}, false, err
	}
	return l, true, nil
}

// SetTagList keeps l as the tag list of repo.
func (s *Store) SetTagList(ctx context.Context, repo string, l TagList) error {
	tags, err := json.Marshal(nonNil(l.Tags))
	if err != nil {
		return err
	}
	tail, err := json.Marshal(nonNil(l.Tail))
	if err != nil {
		return err
	}
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO registry_tag_lists (repository, tags_json, tail_json, latest_digest, listed_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (repository) DO UPDATE SET tags_json = excluded.tags_json, tail_json = excluded.tail_json,
				latest_digest = excluded.latest_digest, listed_at = excluded.listed_at`,
			repo, string(tags), string(tail), l.Latest, FormatTime(l.At))
		return err
	})
}

// PruneTagLists deletes tag lists last listed or extended before cutoff, so
// repositories no longer in use are dropped.
func (s *Store) PruneTagLists(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM registry_tag_lists WHERE listed_at < ?`, FormatTime(cutoff))
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	return n, err
}
