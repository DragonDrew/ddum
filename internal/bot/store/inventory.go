package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/distribution/reference"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// InventoryResult summarizes an applied inventory.
type InventoryResult struct {
	Host             Host
	StacksCreated    int
	StacksMissing    int
	ContainersAdded  int
	ProviderChanges  int
	ContainersMissed int
	Drifts           []DriftEvent // containers that left their desired state
	// MergeSuggestions pairs new stacks with missing ones that look like the
	// same workload under a new name.
	MergeSuggestions []MergeSuggestion
	// ChangedStacks are the stacks whose open updates this inventory closed or
	// corrected, so their cards can be edited now.
	ChangedStacks []string
}

// ApplyInventory upserts a full inventory snapshot for the agent's host by
// identity and marks absent objects missing. It is
// idempotent. Display names from the snapshot only name new rows, so names
// changed in Discord are kept.
func (s *Store) ApplyInventory(ctx context.Context, agentID string, inv protocol.Inventory, at time.Time) (InventoryResult, error) {
	var res InventoryResult
	now := FormatTime(at)
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res = InventoryResult{}
		h, err := getHost(ctx, tx, `h.agent_id = ?`, agentID)
		if errors.Is(err, ErrNotFound) {
			var n int
			tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE id = ?`, agentID).Scan(&n)
			if n > 0 {
				return ErrHostRemoved // detached by reassign-agent
			}
			return ErrUnknownAgent
		}
		if err != nil {
			return err
		}
		if h.Removed() {
			return ErrHostRemoved
		}
		providers, _ := json.Marshal(nonNilProviders(inv.Providers))
		if _, err := tx.ExecContext(ctx, `UPDATE hosts SET last_inventory_at = ?, inventory_hash = ?, providers_json = ? WHERE id = ?`,
			now, inv.InventoryHash, string(providers), h.ID); err != nil {
			return err
		}
		if discoveryFailed(inv) {
			// The agent could not read Docker, so the snapshot says nothing
			// about which stacks exist; marking them missing would hide them
			// until the next good inventory.
			res.Host, err = getHost(ctx, tx, `h.id = ?`, h.ID)
			return err
		}

		existing, err := loadExisting(ctx, tx, h.ID)
		if err != nil {
			return err
		}
		plan := domain.PlanInventory(existing, inv.Stacks)
		type createdStack struct {
			id    string
			stack protocol.Stack
		}
		var created []createdStack

		for _, ch := range plan.Stacks {
			stackID, err := s.applyStack(ctx, tx, h.ID, ch, now, at)
			if err != nil {
				return err
			}
			if err := s.syncSnapshots(ctx, tx, stackID, ch.Incoming.Rollback.SnapshotIDs); err != nil {
				return err
			}
			if ch.ExistingID == "" {
				res.StacksCreated++
				created = append(created, createdStack{id: stackID, stack: ch.Incoming})
			}
			if ch.ProviderChanged {
				res.ProviderChanges++
			}
			for _, cc := range ch.Containers {
				if err := s.applyContainer(ctx, tx, stackID, cc, now); err != nil {
					return err
				}
				if label := cc.Incoming.DdumLabels["ddum.group"]; label != "" {
					var cid string
					if err := tx.QueryRowContext(ctx, `SELECT id FROM containers WHERE stack_id = ? AND identity_key = ?`,
						stackID, cc.Incoming.IdentityKey).Scan(&cid); err != nil {
						return err
					}
					if err := s.seedGroupLabel(ctx, tx, cid, label, at); err != nil {
						return err
					}
				}
				if cc.ExistingID != "" {
					applied, err := s.markAppliedExternally(ctx, tx, cc.ExistingID, cc.Incoming.RepoDigests, at)
					if err != nil {
						return err
					}
					reconciled, err := s.reconcileUpdates(ctx, tx, cc.ExistingID, cc.Incoming, at)
					if err != nil {
						return err
					}
					if (applied || reconciled) && !slices.Contains(res.ChangedStacks, stackID) {
						res.ChangedStacks = append(res.ChangedStacks, stackID)
					}
					d, err := s.checkDrift(ctx, tx, cc.ExistingID, cc.Incoming, at)
					if err != nil {
						return err
					}
					if d != nil {
						res.Drifts = append(res.Drifts, *d)
					}
				}
				if cc.ExistingID == "" {
					res.ContainersAdded++
				}
			}
			for _, id := range ch.MissingContainerIDs {
				if _, err := tx.ExecContext(ctx, `UPDATE containers SET presence = 'missing', updated_at = ? WHERE id = ?`, now, id); err != nil {
					return err
				}
				res.ContainersMissed++
			}
		}
		for _, id := range plan.MissingStackIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE stacks SET presence = 'missing', missing_since = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
				return err
			}
			r, err := tx.ExecContext(ctx, `UPDATE containers SET presence = 'missing', updated_at = ? WHERE stack_id = ? AND presence = 'present'`, now, id)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			res.ContainersMissed += int(n)
			res.StacksMissing++
		}

		// Suggest merges: a new stack sharing at least 80 % of its service names
		// with a stack that is (or just became) missing. Never automatic.
		nowMissing := map[string]bool{}
		for _, id := range plan.MissingStackIDs {
			nowMissing[id] = true
		}
		for _, c := range created {
			var keys []string
			for _, ct := range c.stack.Containers {
				keys = append(keys, ct.IdentityKey)
			}
			for _, old := range existing {
				if old.Presence != domain.Missing && !nowMissing[old.ID] {
					continue
				}
				var oldKeys []string
				for _, oc := range old.Containers {
					oldKeys = append(oldKeys, oc.IdentityKey)
				}
				if matched, ok := domain.SharesServices(keys, oldKeys, 0.8); ok {
					res.MergeSuggestions = append(res.MergeSuggestions, MergeSuggestion{NewStackID: c.id, MissingStackID: old.ID,
						Matched: matched, Total: len(keys)})
				}
			}
		}
		res.Host, err = getHost(ctx, tx, `h.id = ?`, h.ID)
		return err
	})
	return res, err
}

func loadExisting(ctx context.Context, tx *sql.Tx, hostID string) ([]domain.ExistingStack, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.fingerprint, s.provider, s.presence, COALESCE(c.id,''), COALESCE(c.identity_key,''), COALESCE(c.presence,'')
		FROM stacks s LEFT JOIN containers c ON c.stack_id = s.id
		WHERE s.host_id = ? AND s.retired_at IS NULL
		ORDER BY s.id`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExistingStack
	for rows.Next() {
		var st domain.ExistingStack
		var c domain.ExistingContainer
		if err := rows.Scan(&st.ID, &st.Fingerprint, &st.Provider, &st.Presence, &c.ID, &c.IdentityKey, &c.Presence); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != st.ID {
			out = append(out, st)
		}
		if c.ID != "" {
			last := &out[len(out)-1]
			last.Containers = append(last.Containers, c)
		}
	}
	return out, rows.Err()
}

func (s *Store) applyStack(ctx context.Context, tx *sql.Tx, hostID string, ch domain.StackChange, now string, at time.Time) (string, error) {
	in := ch.Incoming
	providerRef := "{}"
	if len(in.ProviderRef) > 0 && json.Valid(in.ProviderRef) {
		providerRef = string(in.ProviderRef)
	}
	var compose sql.NullString
	if in.Compose != nil {
		b, _ := json.Marshal(in.Compose)
		compose = sql.NullString{String: string(b), Valid: true}
	}
	provider := in.Provider
	switch provider {
	case protocol.ProviderCompose, protocol.ProviderPortainer, protocol.ProviderDocker:
	default:
		provider = protocol.ProviderDocker
	}

	if ch.ExistingID == "" {
		id := s.ids.New()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO stacks (id, host_id, display_name, fingerprint, provider, provider_ref_json, compose_json,
			                    deploy_supported, deploy_reason, rollback_available, presence, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'present', ?, ?)`,
			id, hostID, nameOr(in.DisplayName, in.Fingerprint), in.Fingerprint, provider, providerRef, compose,
			in.Deploy.Supported, nullString(in.Deploy.Reason), in.Rollback.Available, now, now)
		return id, err
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE stacks SET provider = ?, provider_ref_json = ?, compose_json = ?, deploy_supported = ?, deploy_reason = ?,
		                  rollback_available = ?, presence = 'present', missing_since = NULL, updated_at = ?
		WHERE id = ?`,
		provider, providerRef, compose, in.Deploy.Supported, nullString(in.Deploy.Reason), in.Rollback.Available, now, ch.ExistingID)
	if err != nil {
		return "", err
	}
	if ch.ProviderChanged {
		// A provider change never creates a new stack.
		if _, err := s.InsertAudit(ctx, tx, AuditEvent{
			At: at, ActorKind: "agent", Action: "stack.provider_changed", TargetType: "stack", TargetID: ch.ExistingID,
			Details: map[string]any{"from": ch.OldProvider, "to": provider, "fingerprint": in.Fingerprint},
		}); err != nil {
			return "", err
		}
	}
	return ch.ExistingID, nil
}

func (s *Store) applyContainer(ctx context.Context, tx *sql.Tx, stackID string, cc domain.ContainerChange, now string) error {
	in := cc.Incoming
	digests, _ := json.Marshal(nonNil(in.RepoDigests))
	labels, _ := json.Marshal(nonNilMap(in.DdumLabels))
	notes, _ := json.Marshal(nonNil(in.Notes))
	var tagChange sql.NullString
	if in.TagChange != nil {
		b, _ := json.Marshal(in.TagChange)
		tagChange = sql.NullString{String: string(b), Valid: true}
	}
	self := in.Self
	if self != protocol.SelfAgent && self != protocol.SelfBot {
		self = ""
	}
	repo := imageRepository(in.ImageRef)

	if cc.ExistingID == "" {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO containers (id, stack_id, identity_key, display_name, docker_id, docker_name, image_ref,
			                        image_repository, running_digests_json, platform, runtime_state, health, protected,
			                        self_role, ddum_labels_json, notes_json, tag_change_json, presence, last_seen_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'present', ?, ?, ?)`,
			s.ids.New(), stackID, in.IdentityKey, in.IdentityKey, nullString(in.DockerID), nullString(in.DockerName),
			nullString(in.ImageRef), repo, string(digests), nullString(in.Platform), nullString(in.State),
			nullString(in.Health), in.Protected, self, string(labels), string(notes), tagChange, now, now, now)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE containers SET docker_id = ?, docker_name = ?, image_ref = ?, image_repository = ?, running_digests_json = ?,
		                      platform = ?, runtime_state = ?, health = ?, protected = ?, self_role = ?,
		                      ddum_labels_json = ?, notes_json = ?, tag_change_json = ?, presence = 'present', last_seen_at = ?, updated_at = ?
		WHERE id = ?`,
		nullString(in.DockerID), nullString(in.DockerName), nullString(in.ImageRef), repo, string(digests),
		nullString(in.Platform), nullString(in.State), nullString(in.Health), in.Protected, self,
		string(labels), string(notes), tagChange, now, now, cc.ExistingID)
	return err
}

// imageRepository returns the normalized repository of an image reference
// ("nginx:1.27" → "docker.io/library/nginx"), or NULL if it does not parse
// (for example a bare image ID).
func imageRepository(ref string) sql.NullString {
	if ref == "" {
		return sql.NullString{}
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: named.Name(), Valid: true}
}

func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// discoveryFailed reports an inventory the agent sent without reading Docker
// : it has no stacks and every provider is in error.
func discoveryFailed(inv protocol.Inventory) bool {
	if len(inv.Stacks) > 0 || len(inv.Providers) == 0 {
		return false
	}
	for _, p := range inv.Providers {
		if p.Status != "error" {
			return false
		}
	}
	return true
}

func nonNilProviders(p []protocol.ProviderStatus) []protocol.ProviderStatus {
	if p == nil {
		return []protocol.ProviderStatus{}
	}
	return p
}

// MergeSuggestion proposes /stack merge for a renamed workload.
type MergeSuggestion struct {
	NewStackID     string
	MissingStackID string
	Matched        int
	Total          int
}
