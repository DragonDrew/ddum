package store

import (
	"context"
	"database/sql"
	"time"
)

// TakeFailingChecks returns the containers whose registry checks have failed
// without a break since before cutoff and have not been reported yet, and
// marks them reported. Only containers that are still checked count:
// present, in a present stack, on an approved host.
func (s *Store) TakeFailingChecks(ctx context.Context, cutoff, at time.Time) ([]Container, error) {
	return s.takeContainers(ctx, ` AND c.presence = 'present' AND s.presence = 'present' AND h.approval = 'approved'
		AND c.check_failure_noticed_at IS NULL AND c.check_failing_since < ?`, []any{FormatTime(cutoff)},
		`UPDATE containers SET check_failure_noticed_at = ? WHERE id = ?`, FormatTime(at))
}

// TakeRecoveredChecks returns the containers reported by TakeFailingChecks
// whose checks have stopped failing since, and clears their report.
func (s *Store) TakeRecoveredChecks(ctx context.Context) ([]Container, error) {
	return s.takeContainers(ctx, ` AND c.check_failure_noticed_at IS NOT NULL AND c.check_failing_since IS NULL`, nil,
		`UPDATE containers SET check_failure_noticed_at = ? WHERE id = ?`, nil)
}

// takeContainers selects containers matching where (after the usual
// retired-stack and removed-host filter) and runs update on each, with
// value and the container ID, in one transaction.
func (s *Store) takeContainers(ctx context.Context, where string, args []any, update string, value any) ([]Container, error) {
	var out []Container
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT `+containerColumns+containerFrom+
			` WHERE s.retired_at IS NULL AND h.removed_at IS NULL`+where+
			` ORDER BY h.display_name COLLATE NOCASE, s.display_name COLLATE NOCASE, c.display_name COLLATE NOCASE, c.id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			c, err := scanContainer(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range out {
			if _, err := tx.ExecContext(ctx, update, value, c.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// TakeAgentVersionNotices returns the approved hosts whose agent, seen since
// seenSince, reports a version other than botVersion that has not been
// reported for this bot version yet, and marks them reported.
// seenSince keeps out versions recorded before the bot started, such as an
// agent restarted with the bot whose first poll is still to come.
func (s *Store) TakeAgentVersionNotices(ctx context.Context, botVersion string, seenSince time.Time) ([]Host, error) {
	var out []Host
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT `+hostColumns+hostFrom+`
			WHERE h.removed_at IS NULL AND h.approval = 'approved' AND COALESCE(a.agent_version,'') NOT IN ('', ?1)
				AND a.last_seen_at >= ?2 AND COALESCE(a.version_notice,'') != a.agent_version || ' ' || ?1
			ORDER BY h.display_name COLLATE NOCASE, h.id`, botVersion, FormatTime(seenSince))
		if err != nil {
			return err
		}
		for rows.Next() {
			h, err := scanHost(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, h := range out {
			if _, err := tx.ExecContext(ctx, `UPDATE agents SET version_notice = ? WHERE id = ?`,
				h.AgentVersion+" "+botVersion, h.AgentID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
