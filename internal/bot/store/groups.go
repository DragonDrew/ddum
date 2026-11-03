package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
)

// Errors returned by group handling.
var (
	ErrInOtherGroup = errors.New("container is already in another group")
	ErrNotMember    = errors.New("container is not in a group")
)

// Group is an update group.
type Group struct {
	ID          string
	DisplayName string
	Source      string // user | label
	CreatedAt   time.Time
	Members     int
}

func scanGroup(r scanner) (Group, error) {
	var g Group
	var created string
	err := r.Scan(&g.ID, &g.DisplayName, &g.Source, &created, &g.Members)
	g.CreatedAt = parseOpt(created)
	return g, err
}

const groupSelect = `SELECT g.id, g.display_name, g.source, g.created_at,
	(SELECT count(*) FROM update_group_members m WHERE m.group_id = g.id) FROM update_groups g`

// Groups lists every group by name.
func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.r.QueryContext(ctx, groupSelect+` ORDER BY g.display_name COLLATE NOCASE, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup returns one group.
func (s *Store) GetGroup(ctx context.Context, id string) (Group, error) {
	return getGroup(ctx, s.r, id)
}

func getGroup(ctx context.Context, q querier, id string) (Group, error) {
	g, err := scanGroup(q.QueryRowContext(ctx, groupSelect+` WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

// GroupMembers lists a group's containers (present or not).
func (s *Store) GroupMembers(ctx context.Context, groupID string) ([]Container, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+containerColumns+containerFrom+` WHERE gm.group_id = ?
		ORDER BY h.display_name COLLATE NOCASE, s.display_name COLLATE NOCASE, c.display_name COLLATE NOCASE`, groupID)
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

// CreateGroup creates a group.
func (s *Store) CreateGroup(ctx context.Context, name string, by Actor, at time.Time) (Group, error) {
	var g Group
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		id, err := s.createGroup(ctx, tx, name, "user", by, at)
		if err != nil {
			return err
		}
		g, err = getGroup(ctx, tx, id)
		return err
	})
	return g, err
}

func (s *Store) createGroup(ctx context.Context, tx *sql.Tx, name, source string, by Actor, at time.Time) (string, error) {
	id := s.ids.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO update_groups (id, display_name, source, created_at) VALUES (?, ?, ?, ?)`,
		id, name, source, FormatTime(at)); err != nil {
		return "", err
	}
	return id, s.audit(ctx, tx, by, at, "group.created", "group", id, map[string]any{"name": name, "source": source})
}

// RenameGroup changes a group's display name; its UUID never changes.
func (s *Store) RenameGroup(ctx context.Context, id, name string, by Actor, at time.Time) (Group, error) {
	var g Group
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		old, err := getGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE update_groups SET display_name = ? WHERE id = ?`, name, id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, by, at, "group.renamed", "group", id, map[string]any{"from": old.DisplayName, "to": name}); err != nil {
			return err
		}
		g, err = getGroup(ctx, tx, id)
		return err
	})
	return g, err
}

// DeleteGroup removes a group and its memberships. Its policy row goes too.
// Labelled members are suppressed so their label does not recreate it.
func (s *Store) DeleteGroup(ctx context.Context, id string, by Actor, at time.Time) (Group, error) {
	var g Group
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		g, err = getGroup(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE containers SET group_label_suppressed = 1
			WHERE id IN (SELECT container_id FROM update_group_members WHERE group_id = ?)
			AND json_extract(ddum_labels_json, '$."ddum.group"') IS NOT NULL`, id); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM update_group_members WHERE group_id = ?`,
			`DELETE FROM policies WHERE scope_type = 'group' AND scope_id = ?`,
			`UPDATE updates SET group_id = NULL WHERE group_id = ?`,
			`DELETE FROM update_groups WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, by, at, "group.deleted", "group", id, map[string]any{"name": g.DisplayName, "members": g.Members})
	})
	return g, err
}

// AddMember puts a container into a group. A container belongs to at most
// one group.
func (s *Store) AddMember(ctx context.Context, groupID, containerID string, by Actor, at time.Time) error {
	return s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := getGroup(ctx, tx, groupID); err != nil {
			return err
		}
		return s.addMember(ctx, tx, groupID, containerID, false, by, at)
	})
}

func (s *Store) addMember(ctx context.Context, tx *sql.Tx, groupID, containerID string, viaLabel bool, by Actor, at time.Time) error {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT group_id FROM update_group_members WHERE container_id = ?`, containerID).Scan(&current)
	switch {
	case err == nil && current == groupID:
		return nil
	case err == nil:
		return ErrInOtherGroup
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO update_group_members (id, group_id, container_id, created_at) VALUES (?, ?, ?, ?)`,
		s.ids.New(), groupID, containerID, FormatTime(at)); err != nil {
		return err
	}
	if !viaLabel {
		// An explicit Discord edit wins over the label from now on.
		if _, err := tx.ExecContext(ctx, `UPDATE containers SET group_label_suppressed = 0 WHERE id = ?`, containerID); err != nil {
			return err
		}
	}
	return s.audit(ctx, tx, by, at, "group.member_added", "group", groupID,
		map[string]any{"container_id": containerID, "via_label": viaLabel})
}

// RemoveMember takes a container out of its group. If the container has a
// ddum.group label, the label will not put it back.
func (s *Store) RemoveMember(ctx context.Context, containerID string, by Actor, at time.Time) (string, error) {
	var groupID string
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT group_id FROM update_group_members WHERE container_id = ?`, containerID).Scan(&groupID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM update_group_members WHERE container_id = ?`, containerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE containers SET group_label_suppressed = 1 WHERE id = ?`, containerID); err != nil {
			return err
		}
		return s.audit(ctx, tx, by, at, "group.member_removed", "group", groupID, map[string]any{"container_id": containerID})
	})
	return groupID, err
}

// seedGroupLabel applies a container's ddum.group label during inventory
// : the group is created if no group has that name, and the container
// joins only if it is in no group and a user has not removed it before.
func (s *Store) seedGroupLabel(ctx context.Context, tx *sql.Tx, containerID, label string, at time.Time) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil
	}
	var suppressed, inGroup int
	if err := tx.QueryRowContext(ctx, `SELECT group_label_suppressed,
		(SELECT count(*) FROM update_group_members WHERE container_id = c.id) FROM containers c WHERE c.id = ?`,
		containerID).Scan(&suppressed, &inGroup); err != nil {
		return err
	}
	if suppressed == 1 || inGroup > 0 {
		return nil
	}
	system := Actor{Kind: "system", Name: "ddum.group label"}
	var groupID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM update_groups WHERE display_name = ? ORDER BY created_at, id LIMIT 1`, label).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		groupID, err = s.createGroup(ctx, tx, label, "label", system, at)
	}
	if err != nil {
		return fmt.Errorf("seed group %q: %w", label, err)
	}
	return s.addMember(ctx, tx, groupID, containerID, true, system, at)
}

// evaluateGroupTx applies the group rules to the group's current pending
// updates (detected, on visible containers). stale reports that a member has
// an actionable update the offer did not show.
func (s *Store) evaluateGroupTx(ctx context.Context, tx *sql.Tx, groupID string, items []OfferItem,
	policies map[PolicyKey]domain.PolicyFields, at time.Time) (domain.GroupEvaluation, bool, error) {
	offered := map[string]bool{}
	for _, it := range items {
		offered[it.UpdateID] = true
	}
	ups, err := queryUpdates(ctx, tx, UpdateFilter{Statuses: []string{StatusDetected}, Visible: true, GroupID: groupID}, at)
	if err != nil {
		return domain.GroupEvaluation{}, false, err
	}
	var members []domain.MemberFacts
	stale := false
	for _, u := range ups {
		c, err := scanContainer(tx.QueryRowContext(ctx, `SELECT `+containerColumns+containerFrom+` WHERE c.id = ?`, u.ContainerID))
		if err != nil {
			return domain.GroupEvaluation{}, false, err
		}
		m, actionable := MemberFactsFor(u, c, policies)
		members = append(members, m)
		if actionable && !offered[u.ID] {
			stale = true
		}
	}
	return domain.EvaluateGroup(members), stale, nil
}

// MemberFactsFor describes one member update for the group rules. actionable
// is false for ignored and protected members, which groups leave out.
func MemberFactsFor(u Update, c Container, policies map[PolicyKey]domain.PolicyFields) (domain.MemberFacts, bool) {
	eff := domain.ResolvePolicy(Chain(policies, c.HostID, c.StackID, c.GroupID, c.ID), domain.LabelIgnore(c.DdumLabels))
	f := domain.UpdateFacts{Kind: u.Kind, Mode: eff.Mode, Protected: c.Protected,
		DeploySupported: c.StackDeploySupported, DeployReason: c.StackDeployReason,
		HostApproved: c.HostApproval == domain.ApprovalApproved, HostOnline: c.HostConnectivity == domain.Online,
		AutoFailed: u.AutoFailed}
	if c.TagChange != nil {
		f.TagChangeSupported, f.TagChangeReason = c.TagChange.Supported, c.TagChange.Reason
	}
	held := ""
	if state, reason := domain.UpdateState(f); state == domain.StateHeld && !c.Protected {
		held = reason
	}
	m := domain.MemberFacts{Name: c.DisplayName, Mode: eff.Mode, HostID: c.HostID, Snoozed: u.Snoozed(),
		Protected: c.Protected, Held: held}
	return m, eff.Mode != domain.ModeIgnore && !c.Protected
}
