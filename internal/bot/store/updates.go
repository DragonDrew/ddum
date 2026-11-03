package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Update statuses.
const (
	StatusDetected   = "detected"
	StatusApproved   = "approved"
	StatusDeploying  = "deploying"
	StatusApplied    = "applied"
	StatusFailed     = "failed"
	StatusSuperseded = "superseded"
)

// Update is an update record joined with its container, stack, host, and
// active snooze.
type Update struct {
	ID            string
	ContainerID   string
	StackID       string
	HostID        string
	Kind          string // digest | version
	CurrentRef    string
	CurrentDigest string
	TargetRef     string
	TargetDigest  string
	TargetVersion string
	ReleaseURL    string
	Status        string
	DetectedAt    time.Time
	UpdatedAt     time.Time
	NotifiedAt    time.Time
	OperationID   string
	// ScheduledFor is when its approved, scheduled operation starts.
	ScheduledFor time.Time
	// AutoFailed reports that an automatic deployment of this update failed,
	// so the policy engine leaves it to a user.
	AutoFailed bool
	// UnverifiedOrigin reports that the running image is not in the repository
	// it names, so it may be a local build this update would replace.
	UnverifiedOrigin bool

	ContainerName string
	StackName     string
	HostName      string

	SnoozeID    string    // active snooze, if any
	SnoozeUntil time.Time // zero with SnoozeID set means indefinite
}

// Snoozed reports whether the update has an active snooze.
func (u Update) Snoozed() bool { return u.SnoozeID != "" }

// Detection is one result of the detection algorithm.
type Detection struct {
	ContainerID   string
	StackID       string
	HostID        string
	Kind          string
	CurrentRef    string
	CurrentDigest string
	TargetRef     string // repo:tag@digest
	TargetDigest  string
	TargetVersion string
}

// DetectionResult says what recording a detection changed.
type DetectionResult struct {
	Update     Update
	New        bool // created, or reopened: needs a notification
	Reopened   bool
	Superseded int // older unapproved updates replaced
}

const updateColumns = `u.id, u.container_id, u.stack_id, u.host_id, u.kind, u.current_image_ref, COALESCE(u.current_digest,''),
	u.target_image_ref, u.target_digest, COALESCE(u.target_version,''), COALESCE(u.release_url,''), u.status,
	u.detected_at, u.updated_at, COALESCE(u.notified_at,''),
	c.display_name, s.display_name, h.display_name,
	COALESCE(sn.id,''), COALESCE(sn.until,''), COALESCE(u.operation_id,''),
	COALESCE((SELECT not_before FROM operations WHERE id = u.operation_id AND state = 'queued'),''),
	u.auto_failed_at IS NOT NULL, u.unverified_origin_at IS NOT NULL`

// updateFrom joins the active snooze; the query's first argument is now.
const updateFrom = ` FROM updates u
	JOIN containers c ON c.id = u.container_id
	JOIN stacks s ON s.id = u.stack_id
	JOIN hosts h ON h.id = u.host_id
	LEFT JOIN snoozes sn ON sn.id = (
		SELECT id FROM snoozes x WHERE x.update_id = u.id AND x.removed_at IS NULL AND (x.until IS NULL OR x.until > ?)
		ORDER BY x.created_at DESC LIMIT 1)`

func scanUpdate(r scanner) (Update, error) {
	var u Update
	var detected, updated, notified, until, scheduled string
	err := r.Scan(&u.ID, &u.ContainerID, &u.StackID, &u.HostID, &u.Kind, &u.CurrentRef, &u.CurrentDigest,
		&u.TargetRef, &u.TargetDigest, &u.TargetVersion, &u.ReleaseURL, &u.Status,
		&detected, &updated, &notified, &u.ContainerName, &u.StackName, &u.HostName, &u.SnoozeID, &until, &u.OperationID, &scheduled, &u.AutoFailed, &u.UnverifiedOrigin)
	u.DetectedAt, u.UpdatedAt, u.NotifiedAt, u.SnoozeUntil = parseOpt(detected), parseOpt(updated), parseOpt(notified), parseOpt(until)
	u.ScheduledFor = parseOpt(scheduled)
	return u, err
}

// UpdateFilter narrows Updates. Empty fields do not filter.
type UpdateFilter struct {
	Statuses    []string
	HostID      string
	StackID     string
	ContainerID string
	GroupID     string // members of this group (current membership)
	NoGroup     bool   // only containers in no group (stack scopes)
	Unnotified  bool   // notified_at IS NULL
	// Visible restricts to present containers of present, non-retired
	// stacks on approved, non-removed hosts.
	Visible bool
}

// Updates lists update records, oldest detection first. now decides which
// snoozes are active.
func (s *Store) Updates(ctx context.Context, f UpdateFilter, now time.Time) ([]Update, error) {
	return queryUpdates(ctx, s.r, f, now)
}

func queryUpdates(ctx context.Context, q querier, f UpdateFilter, now time.Time) ([]Update, error) {
	query := `SELECT ` + updateColumns + updateFrom + ` WHERE 1=1`
	args := []any{FormatTime(now)}
	if len(f.Statuses) > 0 {
		query += ` AND u.status IN (?` + strings.Repeat(",?", len(f.Statuses)-1) + `)`
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	for _, c := range []struct{ col, v string }{{"u.host_id", f.HostID}, {"u.stack_id", f.StackID}, {"u.container_id", f.ContainerID}} {
		if c.v != "" {
			query += ` AND ` + c.col + ` = ?`
			args = append(args, c.v)
		}
	}
	if f.GroupID != "" {
		query += ` AND u.container_id IN (SELECT container_id FROM update_group_members WHERE group_id = ?)`
		args = append(args, f.GroupID)
	}
	if f.NoGroup {
		query += ` AND u.container_id NOT IN (SELECT container_id FROM update_group_members)`
	}
	if f.Unnotified {
		query += ` AND u.notified_at IS NULL`
	}
	if f.Visible {
		query += ` AND c.presence = 'present' AND s.presence = 'present' AND s.retired_at IS NULL
			AND h.removed_at IS NULL AND h.approval = 'approved'`
	}
	query += ` ORDER BY h.display_name COLLATE NOCASE, s.display_name COLLATE NOCASE, c.display_name COLLATE NOCASE, u.detected_at, u.id`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		u, err := scanUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUpdate returns one update by ID.
func (s *Store) GetUpdate(ctx context.Context, id string, now time.Time) (Update, error) {
	return getUpdate(ctx, s.r, id, now)
}

func getUpdate(ctx context.Context, q querier, id string, now time.Time) (Update, error) {
	u, err := scanUpdate(q.QueryRowContext(ctx, `SELECT `+updateColumns+updateFrom+` WHERE u.id = ?`, FormatTime(now), id))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// RecordDetection creates or refreshes the update keyed by (container,
// target digest):
//   - an existing open row (detected, approved, deploying) is refreshed;
//   - a terminal row (applied, failed, superseded) is reopened to detected,
//     with approval cleared, flagged for a new notification, and audited;
//   - otherwise a new detected row is created.
//
// Any other detected (never approved) update of the container is superseded.
func (s *Store) RecordDetection(ctx context.Context, d Detection, at time.Time) (DetectionResult, error) {
	var res DetectionResult
	now := FormatTime(at)
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res = DetectionResult{}
		var id, status string
		err := tx.QueryRowContext(ctx, `SELECT id, status FROM updates WHERE container_id = ? AND target_digest = ?`,
			d.ContainerID, d.TargetDigest).Scan(&id, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id = s.ids.New()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO updates (id, container_id, stack_id, host_id, kind, current_image_ref, current_digest,
				                     target_image_ref, target_digest, target_version, status, detected_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'detected', ?, ?)`,
				id, d.ContainerID, d.StackID, d.HostID, d.Kind, d.CurrentRef, nullString(d.CurrentDigest),
				d.TargetRef, d.TargetDigest, nullString(d.TargetVersion), now, now); err != nil {
				return err
			}
			res.New = true
		case err != nil:
			return err
		case status == StatusApplied || status == StatusFailed || status == StatusSuperseded:
			if _, err := tx.ExecContext(ctx, `
				UPDATE updates SET status = 'detected', kind = ?, current_image_ref = ?, current_digest = ?,
				       target_image_ref = ?, target_version = ?, detected_at = ?, updated_at = ?, notified_at = NULL,
				       approved_by_kind = NULL, approved_by_discord_id = NULL, approved_at = NULL, operation_id = NULL
				WHERE id = ?`,
				d.Kind, d.CurrentRef, nullString(d.CurrentDigest), d.TargetRef, nullString(d.TargetVersion), now, now, id); err != nil {
				return err
			}
			if _, err := s.InsertAudit(ctx, tx, AuditEvent{At: at, ActorKind: "system", Action: "update.reopened",
				TargetType: "update", TargetID: id, Details: map[string]any{"previous_status": status, "target": d.TargetRef}}); err != nil {
				return err
			}
			res.New, res.Reopened = true, true
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE updates SET current_image_ref = ?, current_digest = ? WHERE id = ?`,
				d.CurrentRef, nullString(d.CurrentDigest), id); err != nil {
				return err
			}
		}
		// A newer detection supersedes older unapproved ones.
		r, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'superseded', updated_at = ?
			WHERE container_id = ? AND status = 'detected' AND id <> ?`, now, d.ContainerID, id)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		res.Superseded = int(n)
		res.Update, err = getUpdate(ctx, tx, id, at)
		return err
	})
	return res, err
}

// SetReleaseURL stores the release link found for an update.
func (s *Store) SetReleaseURL(ctx context.Context, id, url string) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE updates SET release_url = ? WHERE id = ?`, nullString(url), id)
		return err
	})
}

// MarkNotified records that a notification covered these updates.
func (s *Store) MarkNotified(ctx context.Context, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE updates SET notified_at = ? WHERE id = ? AND notified_at IS NULL`,
				FormatTime(at), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetTracking stores the outcome of checking one container.
func (s *Store) SetTracking(ctx context.Context, containerID, mode, reason, checkErr string, at time.Time) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		// A failure keeps the time the current run of failures began.
		if checkErr == "" && mode != domain.TrackingUntracked {
			if _, err := tx.ExecContext(ctx, `UPDATE containers SET check_ok_at = ? WHERE id = ?`, FormatTime(at), containerID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE containers SET tracking = ?1, untracked_reason = ?2, last_check_at = ?3, last_check_error = ?4,
				check_failing_since = CASE WHEN ?4 IS NULL THEN NULL ELSE COALESCE(check_failing_since, ?3) END
			WHERE id = ?5`, mode, nullString(reason), FormatTime(at), nullString(checkErr), containerID)
		return err
	})
}

// SetNewerOutsideTrack records the newest version a container's tag_track
// leaves out, or clears it.
func (s *Store) SetNewerOutsideTrack(ctx context.Context, containerID, tag string) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE containers SET newer_outside_track = ? WHERE id = ? AND COALESCE(newer_outside_track,'') <> ?`,
			nullString(tag), containerID, tag)
		return err
	})
}

// MarkUnverifiedOrigin records that an update would replace an image its
// repository does not hold, perhaps a local build. Like a failed automatic
// deploy, it then waits for a person, whatever the policy says.
func (s *Store) MarkUnverifiedOrigin(ctx context.Context, updateID string, at time.Time) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE updates SET unverified_origin_at = ?1, auto_failed_at = ?1 WHERE id = ?2`, FormatTime(at), updateID)
		return err
	})
}

// ImagePlatforms returns the stored platforms of repo@digest.
func (s *Store) ImagePlatforms(ctx context.Context, repo, digest string) ([]string, bool, error) {
	var raw string
	err := s.r.QueryRowContext(ctx, `SELECT platforms_json FROM image_platforms WHERE repository = ? AND digest = ?`, repo, digest).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out []string
	return out, true, json.Unmarshal([]byte(raw), &out)
}

// SetImagePlatforms stores the platforms of repo@digest.
func (s *Store) SetImagePlatforms(ctx context.Context, repo, digest string, platforms []string, at time.Time) error {
	b, err := json.Marshal(platforms)
	if err != nil {
		return err
	}
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO image_platforms (repository, digest, platforms_json, checked_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (repository, digest) DO UPDATE SET platforms_json = excluded.platforms_json, checked_at = excluded.checked_at`,
			repo, digest, string(b), FormatTime(at))
		return err
	})
}

// PruneImagePlatforms forgets platforms not looked at since cutoff.
func (s *Store) PruneImagePlatforms(ctx context.Context, cutoff time.Time) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM image_platforms WHERE checked_at < ?`, FormatTime(cutoff))
		return err
	})
}

// SupersedeMissingStacks supersedes the open, unapproved updates of stacks
// that have been missing since before cutoff (STACK_MISSING_GRACE_HOURS).
func (s *Store) SupersedeMissingStacks(ctx context.Context, cutoff, at time.Time) (int, error) {
	var n int64
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'superseded', updated_at = ?
			WHERE status = 'detected' AND stack_id IN (
				SELECT id FROM stacks WHERE presence = 'missing' AND missing_since < ?)`, FormatTime(at), FormatTime(cutoff))
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	return int(n), err
}

// SupersedeCurrent supersedes the open, unapproved updates of a container that
// its registry check now finds current. A manual update that got there first
// leaves them behind, and nothing else would clear them (the check records
// nothing for a current container). An update the container is still behind
// is detected again, and reopened, by the check that finds it.
func (s *Store) SupersedeCurrent(ctx context.Context, containerID string, at time.Time) (int, error) {
	var n int
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		n = 0
		ids, err := idsWhere(ctx, tx, `SELECT id FROM updates WHERE container_id = ? AND status = 'detected'`, containerID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.supersedeOvertaken(ctx, tx, id, "the registry check needs no update for this container", at); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// supersedeOvertaken closes one detected update that no longer applies,
// because the container moved past it outside ddum.
func (s *Store) supersedeOvertaken(ctx context.Context, tx *sql.Tx, id, why string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'superseded', updated_at = ? WHERE id = ?`, FormatTime(at), id); err != nil {
		return err
	}
	_, err := s.InsertAudit(ctx, tx, AuditEvent{At: at, ActorKind: "system", Action: "update.overtaken",
		TargetType: "update", TargetID: id, Details: map[string]any{"why": why}})
	return err
}

// reconcileUpdates brings a container's detected updates in line with what it
// runs after a change outside ddum, so the owner sees it within one poll and
// cannot approve an update recorded against an older state. It reports
// whether it changed anything. An update it cannot judge, a digest update
// whose tag is unchanged, is left to the next check: only the registry can say
// which digest is newer, and until then approving it is refused.
func (s *Store) reconcileUpdates(ctx context.Context, tx *sql.Tx, containerID string, in protocol.Container, at time.Time) (bool, error) {
	repo := imageRepository(in.ImageRef)
	running := domain.RunningDigests(repo.String, in.RepoDigests)
	if len(running) == 0 {
		return false, nil // a local image or a lost record: nothing to compare
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, target_image_ref, COALESCE(target_version,''), COALESCE(current_digest,'')
		FROM updates WHERE container_id = ? AND status = 'detected'`, containerID)
	if err != nil {
		return false, err
	}
	type row struct{ id, kind, target, version, current string }
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.kind, &r.target, &r.version, &r.current); err != nil {
			rows.Close()
			return false, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	tag := imageTag(in.ImageRef)
	if tag == "" {
		tag = "latest"
	}
	changed := false
	for _, r := range rs {
		if r.current == "" || slices.Contains(running, r.current) {
			continue // it still runs what the update was recorded against
		}
		switch {
		case !strings.HasPrefix(r.target, repo.String+":"):
			err = s.supersedeOvertaken(ctx, tx, r.id, "the container now runs another repository", at)
		case r.kind == "version" && !domain.IsUpgrade(tag, r.version):
			err = s.supersedeOvertaken(ctx, tx, r.id, "the container now runs "+tag, at)
		case r.kind == "version":
			_, err = tx.ExecContext(ctx, `UPDATE updates SET current_image_ref = ?, current_digest = ?, updated_at = ? WHERE id = ?`,
				in.ImageRef, running[0], FormatTime(at), r.id)
		case imageTag(r.target) != tag:
			err = s.supersedeOvertaken(ctx, tx, r.id, "the container now runs "+tag, at)
		default:
			continue
		}
		if err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// markAppliedExternally closes detected updates whose target digest a
// container now runs because of an outside change.
func (s *Store) markAppliedExternally(ctx context.Context, tx *sql.Tx, containerID string, runningDigests []string, at time.Time) (bool, error) {
	if len(runningDigests) == 0 {
		return false, nil
	}
	digests := map[string]bool{}
	for _, rd := range runningDigests {
		if i := strings.LastIndex(rd, "@"); i >= 0 {
			rd = rd[i+1:]
		}
		digests[rd] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, target_digest, target_image_ref FROM updates WHERE container_id = ? AND status = 'detected'`, containerID)
	if err != nil {
		return false, err
	}
	type hit struct{ id, ref string }
	var hits []hit
	for rows.Next() {
		var id, d, ref string
		if err := rows.Scan(&id, &d, &ref); err != nil {
			rows.Close()
			return false, err
		}
		if digests[d] {
			hits = append(hits, hit{id, ref})
		}
	}
	rows.Close()
	for _, h := range hits {
		if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'applied', updated_at = ? WHERE id = ?`, FormatTime(at), h.id); err != nil {
			return false, err
		}
		if _, err := s.InsertAudit(ctx, tx, AuditEvent{At: at, ActorKind: "system", Action: "update.applied_externally",
			TargetType: "update", TargetID: h.id, Details: map[string]any{"target": h.ref}}); err != nil {
			return false, err
		}
	}
	return len(hits) > 0, nil
}

// StacksWithUpdateChanges returns the stacks whose update records changed
// status (or were created) at or after since, so their messages can be edited.
func (s *Store) StacksWithUpdateChanges(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT DISTINCT stack_id FROM updates WHERE updated_at >= ? ORDER BY stack_id`, FormatTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
