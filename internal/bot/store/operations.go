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

// Errors returned by approval and operation handling.
var (
	ErrStaleOffer       = errors.New("offer is stale")
	ErrAlreadyApproved  = errors.New("offer already approved")
	ErrNothingToApprove = errors.New("nothing in this offer can be approved")
	ErrHeld             = errors.New("scope is held")
	ErrNotCancellable   = errors.New("operation can no longer be cancelled")
)

// MaxDispatchAttempts is how often an unacknowledged command is re-dispatched
// before it fails with not_acknowledged.
const MaxDispatchAttempts = 3

// Operation is a stored operation.
type Operation struct {
	ID                 string
	Kind               string
	AgentID            string
	HostID             string
	ScopeType          string
	ScopeID            string
	Payload            protocol.DeployPayload
	PayloadJSON        string
	State              string
	Attempt            int
	LeaseExpiresAt     time.Time
	ExpiresAt          time.Time
	InitiatorKind      string
	InitiatorDiscordID string
	Outcome            OperationOutcome
	CreatedAt          time.Time
	UpdatedAt          time.Time
	FinishedAt         time.Time
	// NotBefore is when a scheduled operation may start; zero means now.
	NotBefore time.Time

	HostName  string
	StackName string // for stack scopes
}

// Terminal reports whether the operation has finished.
func (o Operation) Terminal() bool {
	switch o.State {
	case protocol.OpSucceeded, protocol.OpFailed, protocol.OpPartiallyFailed, protocol.OpInterrupted,
		protocol.OpExpired, protocol.OpCancelled:
		return true
	}
	return false
}

// OperationOutcome is stored in result_json.
type OperationOutcome struct {
	Code       string           `json:"code,omitempty"`
	Message    string           `json:"message,omitempty"`
	OutputTail string           `json:"output_tail,omitempty"`
	Result     *protocol.Result `json:"result,omitempty"`
}

const opColumns = `o.id, o.kind, o.agent_id, o.host_id, o.scope_type, o.scope_id, o.payload_json, o.state, o.attempt,
	COALESCE(o.lease_expires_at,''), o.expires_at, o.initiator_kind, COALESCE(o.initiator_discord_id,''),
	COALESCE(o.result_json,''), o.created_at, o.updated_at, COALESCE(o.finished_at,''), COALESCE(o.not_before,''),
	h.display_name, COALESCE((SELECT display_name FROM stacks WHERE id = o.scope_id),
		(SELECT 'group ' || display_name FROM update_groups WHERE id = o.scope_id), '')`

const opFrom = ` FROM operations o JOIN hosts h ON h.id = o.host_id`

func scanOp(r scanner) (Operation, error) {
	var o Operation
	var lease, expires, result, created, updated, finished, notBefore string
	err := r.Scan(&o.ID, &o.Kind, &o.AgentID, &o.HostID, &o.ScopeType, &o.ScopeID, &o.PayloadJSON, &o.State, &o.Attempt,
		&lease, &expires, &o.InitiatorKind, &o.InitiatorDiscordID, &result, &created, &updated, &finished, &notBefore,
		&o.HostName, &o.StackName)
	if err != nil {
		return o, err
	}
	json.Unmarshal([]byte(o.PayloadJSON), &o.Payload)
	if result != "" {
		json.Unmarshal([]byte(result), &o.Outcome)
	}
	o.LeaseExpiresAt, o.ExpiresAt = parseOpt(lease), parseOpt(expires)
	o.CreatedAt, o.UpdatedAt, o.FinishedAt = parseOpt(created), parseOpt(updated), parseOpt(finished)
	o.NotBefore = parseOpt(notBefore)
	return o, nil
}

func getOp(ctx context.Context, q querier, id string) (Operation, error) {
	o, err := scanOp(q.QueryRowContext(ctx, `SELECT `+opColumns+opFrom+` WHERE o.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}

// GetOperation returns one operation.
func (s *Store) GetOperation(ctx context.Context, id string) (Operation, error) {
	return getOp(ctx, s.r, id)
}

// OperationFilter narrows Operations.
type OperationFilter struct {
	HostID  string
	ScopeID string
	Limit   int
}

// Operations lists operations, newest first.
func (s *Store) Operations(ctx context.Context, f OperationFilter) ([]Operation, error) {
	q := `SELECT ` + opColumns + opFrom + ` WHERE 1=1`
	var args []any
	if f.HostID != "" {
		q += ` AND o.host_id = ?`
		args = append(args, f.HostID)
	}
	if f.ScopeID != "" {
		q += ` AND o.scope_id = ?`
		args = append(args, f.ScopeID)
	}
	if f.Limit <= 0 {
		f.Limit = 25
	}
	q += fmt.Sprintf(` ORDER BY o.created_at DESC, o.id DESC LIMIT %d`, f.Limit)
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OperationEvent is a stored event.
type OperationEvent struct {
	Seq        int
	Type       string
	Step       string
	Message    string
	ReceivedAt time.Time
}

// OperationEvents lists an operation's events in order.
func (s *Store) OperationEvents(ctx context.Context, opID string) ([]OperationEvent, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT seq, type, COALESCE(step,''), COALESCE(message,''), received_at
		FROM operation_events WHERE operation_id = ? ORDER BY seq`, opID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperationEvent
	for rows.Next() {
		var e OperationEvent
		var at string
		if err := rows.Scan(&e.Seq, &e.Type, &e.Step, &e.Message, &at); err != nil {
			return nil, err
		}
		e.ReceivedAt = parseOpt(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ApproveRequest asks to approve an offer.
type ApproveRequest struct {
	OfferID       string
	By            Actor // Kind "user" or "policy"
	At            time.Time
	ExpiryMinutes int // COMMAND_EXPIRY_MINUTES
	// NotBefore schedules the operation: it is not dispatched before then,
	// and its expiry counts from then. Zero means as soon as possible.
	NotBefore time.Time
}

// Skipped is an offered update that was left out, and why.
type Skipped struct {
	Update Update
	Reason string
}

// ApproveResult describes a created operation.
type ApproveResult struct {
	Operation     Operation
	Approved      []Update
	Skipped       []Skipped
	QueuePosition int // operations ahead of it on the host (0: next)
}

// AlreadyApprovedError says who approved an offer first.
type AlreadyApprovedError struct {
	OperationID string
	ByDiscordID string
	ByKind      string
}

func (e *AlreadyApprovedError) Error() string { return ErrAlreadyApproved.Error() }
func (e *AlreadyApprovedError) Unwrap() error { return ErrAlreadyApproved }

// NothingError lists why nothing could be approved.
type NothingError struct{ Skipped []Skipped }

func (e *NothingError) Error() string { return ErrNothingToApprove.Error() }
func (e *NothingError) Unwrap() error { return ErrNothingToApprove }

// Approve turns an offer into one operation, re-checking everything inside a
// single transaction:
//   - an offer approves at most once; a second click gets AlreadyApprovedError;
//   - an offer is stale if any of its updates is no longer detected at the
//     offered digest (ErrStaleOffer); nothing changes;
//   - only actionable updates are approved: for a user, members whose
//     effective mode is manual; for the policy engine, automatic. Snoozed,
//     ignored, protected, held, and busy members are skipped (stack scope);
//   - the host must be approved, online, and advertise action:deploy.
func (s *Store) Approve(ctx context.Context, req ApproveRequest) (ApproveResult, error) {
	var res ApproveResult
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res = ApproveResult{}
		offer, err := getOfferTx(ctx, tx, req.OfferID)
		if err != nil {
			return err
		}
		if offer.consumedOp != "" {
			op, _ := getOp(ctx, tx, offer.consumedOp)
			return &AlreadyApprovedError{OperationID: offer.consumedOp, ByDiscordID: op.InitiatorDiscordID, ByKind: op.InitiatorKind}
		}
		isGroup := offer.ScopeType == "group"
		if offer.ScopeType != "stack" && !isGroup {
			return fmt.Errorf("unsupported scope %q", offer.ScopeType)
		}

		var updates []Update
		for _, it := range offer.Items {
			u, err := getUpdate(ctx, tx, it.UpdateID, req.At)
			if errors.Is(err, ErrNotFound) {
				return ErrStaleOffer
			}
			if err != nil {
				return err
			}
			if u.Status != StatusDetected || u.TargetDigest != it.TargetDigest {
				return ErrStaleOffer
			}
			updates = append(updates, u)
		}
		if len(updates) == 0 {
			return &NothingError{}
		}

		host, err := getHost(ctx, tx, `h.id = ?`, updates[0].HostID)
		if err != nil {
			return err
		}
		policies, err := policiesTx(ctx, tx)
		if err != nil {
			return err
		}
		wantMode := domain.ModeManual
		if req.By.Kind == "policy" {
			wantMode = domain.ModeAutomatic
		}
		hold, err := rollbackHold(ctx, tx, offer.ScopeID)
		if err != nil {
			return err
		}
		// Group scope: the offer must show every actionable
		// member update, and the coupling rules decide for the whole group.
		groupHeld := ""
		if isGroup {
			ev, stale, err := s.evaluateGroupTx(ctx, tx, offer.ScopeID, offer.Items, policies, req.At)
			if err != nil {
				return err
			}
			if stale {
				return ErrStaleOffer
			}
			switch {
			case req.By.Kind == "policy" && !ev.AutoDeploy:
				groupHeld = "the group is not eligible for automatic deployment"
				if ev.Held != "" {
					groupHeld += " (" + ev.Held + ")"
				} else if ev.Snoozed {
					groupHeld += " (a member update is snoozed)"
				}
			case req.By.Kind != "policy" && !ev.ManualApprovalAllowed():
				groupHeld = ev.Held
			}
		}
		hostReason := ""
		switch {
		case host.Removed():
			hostReason = "host removed"
		case host.Approval != domain.ApprovalApproved:
			hostReason = "host pending approval"
		case host.Connectivity != domain.Online:
			hostReason = "agent offline"
		case !slices.Contains(host.Capabilities, protocol.CapActionDeploy):
			hostReason = "this agent version cannot deploy (no action:deploy capability)"
		}

		verify := protocol.VerifySettings{}
		var chosen []Update
		var services []protocol.ServiceChange
		targets := map[string]*protocol.DeployTarget{}
		var targetOrder []string
		memberWaits := false // a group member cannot go now, so the group waits
		for _, u := range updates {
			c, err := scanContainer(tx.QueryRowContext(ctx, `SELECT `+containerColumns+containerFrom+` WHERE c.id = ?`, u.ContainerID))
			if err != nil {
				return err
			}
			eff := domain.ResolvePolicy(Chain(policies, c.HostID, c.StackID, c.GroupID, c.ID), domain.LabelIgnore(c.DdumLabels))
			f := domain.UpdateFacts{Kind: u.Kind, Mode: eff.Mode, Snoozed: u.Snoozed(), Protected: c.Protected,
				DeploySupported: c.StackDeploySupported, DeployReason: c.StackDeployReason,
				HostApproved: c.HostApproval == domain.ApprovalApproved, HostOnline: c.HostConnectivity == domain.Online,
				AutoFailed: u.AutoFailed}
			if c.TagChange != nil {
				f.TagChangeSupported, f.TagChangeReason = c.TagChange.Supported, c.TagChange.Reason
			}
			state, reason := domain.UpdateState(f)
			skip := ""
			switch {
			case hostReason != "":
				skip = hostReason
			case c.Presence != domain.Present || c.StackPresence != domain.Present:
				skip = "the container no longer exists"
			case req.By.Kind == "policy" && u.AutoFailed:
				skip = "its automatic deployment failed, so it waits for a user"
			case state == domain.StateIgnored:
				skip = "ignored by policy"
			case isGroup && c.GroupID != offer.ScopeID:
				return ErrStaleOffer // membership changed since the offer
			case !isGroup && c.GroupID != "":
				skip = "in group " + c.GroupName + "; it is approved with its group"
			case isGroup && c.Protected:
				skip = reason // protected members are left out of the group
			case isGroup && groupHeld != "":
				skip = groupHeld
			case !isGroup && state == domain.StateHeld:
				skip = reason
			case !isGroup && state == domain.StateSnoozed && !(offer.createdBy == "command" && len(offer.Items) == 1):
				// A stack approval leaves snoozed updates out; /update approve of
				// that one update approves it and removes the snooze. A
				// group approval includes them and removes the snooze.
				skip = "snoozed"
			case !isGroup && eff.Mode != wantMode && !(req.By.Kind == "user" && u.AutoFailed):
				skip = "policy is " + eff.Mode
			case (c.Self == protocol.SelfBot) != offer.selfUpdate:
				// The bot is updated alone, from its own message, by a user.
				skip = "the ddum bot is updated from its own message"
			case offer.selfUpdate && req.By.Kind != "user":
				skip = "the bot is never updated automatically"
			case hold != "":
				skip = hold
			case u.Kind == "version" && !slices.Contains(host.Capabilities, protocol.CapTagChangeEdit):
				skip = "this agent cannot edit image tags"
			case u.CurrentDigest != "" && len(c.RunningDigests) > 0 && !runsDigest(c.RunningDigests, u.CurrentDigest):
				// The container changed after this update was found, so the offer
				// describes a state it no longer has. No running digest at all
				// says nothing: Docker can lose the record of a running image
				// when its tag moves.
				skip = "the container changed after this update was found; the next check finds it again"
			case u.Kind == "version" && domain.IsDowngrade(imageTag(c.ImageRef), u.TargetVersion):
				skip = "it would be an older release than the one running"
			}
			if skip == "" {
				var busy int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM desired_states WHERE container_id = ? AND status = 'pending'`,
					c.ID).Scan(&busy); err != nil {
					return err
				}
				if busy > 0 {
					skip = "another operation for this container is in progress"
				}
			}
			if skip != "" {
				res.Skipped = append(res.Skipped, Skipped{Update: u, Reason: skip})
				// Ignored and protected members and the bot are left out of a
				// group; anything else would deploy only part of it.
				if isGroup && state != domain.StateIgnored && !c.Protected && c.Self != protocol.SelfBot {
					memberWaits = true
				}
				continue
			}
			t := targets[u.StackID]
			if t == nil {
				st, err := scanStack(tx.QueryRowContext(ctx, `SELECT `+stackColumns+stackFrom+` WHERE s.id = ?`, u.StackID))
				if err != nil {
					return err
				}
				t = &protocol.DeployTarget{StackID: st.ID, StackFingerprint: st.Fingerprint}
				targets[u.StackID] = t
				targetOrder = append(targetOrder, u.StackID)
			}
			verify.TimeoutSeconds = max(verify.TimeoutSeconds, eff.VerifyTimeout)
			verify.StableSeconds = max(verify.StableSeconds, eff.VerifyStable)
			chosen = append(chosen, u)
			svc := protocol.ServiceChange{
				ContainerID: c.ID, IdentityKey: c.IdentityKey,
				Current: protocol.ImageState{ImageRef: u.CurrentRef, Digest: u.CurrentDigest},
				Desired: protocol.ImageState{ImageRef: desiredRef(u.TargetRef), Digest: u.TargetDigest},
			}
			services = append(services, svc)
			t.Services = append(t.Services, svc)
		}
		if memberWaits {
			for _, u := range chosen {
				res.Skipped = append(res.Skipped, Skipped{Update: u, Reason: "the group deploys together, and another member cannot go now"})
			}
			chosen = nil
		}
		if len(chosen) == 0 {
			if req.By.Kind == "user" {
				if err := s.audit(ctx, tx, req.By, req.At, "update.approval_refused", offer.ScopeType, offer.ScopeID,
					map[string]any{"reasons": skipReasons(res.Skipped)}); err != nil {
					return err
				}
			}
			return &NothingError{Skipped: res.Skipped}
		}

		opID := s.ids.New()
		start := req.At
		var notBefore any
		if req.NotBefore.After(req.At) {
			start, notBefore = req.NotBefore, FormatTime(req.NotBefore)
		}
		expires := start.Add(time.Duration(req.ExpiryMinutes) * time.Minute)
		payload := protocol.DeployPayload{
			OperationID: opID, Scope: protocol.Scope{Kind: offer.ScopeType, ID: offer.ScopeID},
			IssuedAt: req.At.UTC().Format(time.RFC3339), ExpiresAt: expires.UTC().Format(time.RFC3339),
			Verify:     verify,
			Targets:    deployTargets(targets, targetOrder),
			SelfUpdate: offer.selfUpdate,
		}
		pj, _ := json.Marshal(payload)
		now := FormatTime(req.At)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO operations (id, kind, agent_id, host_id, scope_type, scope_id, payload_json, state, attempt,
			                        expires_at, initiator_kind, initiator_discord_id, created_at, updated_at, not_before)
			VALUES (?, 'deploy', ?, ?, ?, ?, ?, 'queued', 0, ?, ?, ?, ?, ?, ?)`,
			opID, host.AgentID, host.ID, offer.ScopeType, offer.ScopeID, string(pj), FormatTime(expires), req.By.Kind,
			nullString(req.By.DiscordID), now, now, notBefore); err != nil {
			return err
		}
		for i, u := range chosen {
			if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'approved', approved_by_kind = ?, approved_by_discord_id = ?,
				approved_at = ?, operation_id = ?, updated_at = ? WHERE id = ?`,
				req.By.Kind, nullString(req.By.DiscordID), now, opID, now, u.ID); err != nil {
				return err
			}
			// Approving a snoozed update removes its snooze.
			if _, err := tx.ExecContext(ctx, `UPDATE snoozes SET removed_at = ?, removed_by_discord_id = ?
				WHERE update_id = ? AND removed_at IS NULL`, now, nullString(req.By.DiscordID), u.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO desired_states (id, container_id, image_ref, image_digest, status, operation_id, set_at)
				VALUES (?, ?, ?, ?, 'pending', ?, ?)`, s.ids.New(), u.ContainerID, services[i].Desired.ImageRef, u.TargetDigest, opID, now); err != nil {
				return err
			}
			action := "update.approved"
			if req.By.Kind == "policy" {
				action = "update.auto_approved"
			}
			if err := s.audit(ctx, tx, req.By, req.At, action, "update", u.ID,
				map[string]any{"target": u.TargetRef, "operation_id": opID}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE approval_offers SET consumed_at = ?, operation_id = ? WHERE id = ?`,
			now, opID, offer.ID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, req.By, req.At, "operation.created", "operation", opID,
			map[string]any{"scope": offer.ScopeType, "scope_id": offer.ScopeID, "updates": len(chosen), "not_before": notBefore}); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE host_id = ? AND id <> ?
			AND state IN ('queued','dispatched','accepted','running','verifying')
			AND (not_before IS NULL OR not_before <= ?)`, host.ID, opID, FormatTime(start)).Scan(&res.QueuePosition); err != nil {
			return err
		}
		res.Approved = chosen
		res.Operation, err = getOp(ctx, tx, opID)
		return err
	})
	return res, err
}

// desiredRef drops the digest from "repo:tag@digest": the agent pins the
// digest itself; the reference is what the Compose file must name.
func desiredRef(target string) string {
	if i := strings.LastIndex(target, "@"); i >= 0 {
		return target[:i]
	}
	return target
}

// imageTag returns the tag of "repo:tag" or "repo:tag@digest", or "" if the
// reference has none.
func imageTag(ref string) string {
	ref = desiredRef(ref)
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") { // a port in the registry host, not a tag
		return ""
	}
	return ref[i+1:]
}

// runsDigest reports whether one of the running digests, "repo@digest" or a
// bare digest, is digest.
func runsDigest(running []string, digest string) bool {
	for _, r := range running {
		if i := strings.LastIndex(r, "@"); i >= 0 {
			r = r[i+1:]
		}
		if r == digest {
			return true
		}
	}
	return false
}

func skipReasons(sk []Skipped) []string {
	var out []string
	for _, s := range sk {
		out = append(out, s.Update.ContainerName+": "+s.Reason)
	}
	return out
}

type offerRow struct {
	Offer
	consumedOp string
	createdBy  string
	selfUpdate bool
}

func getOfferTx(ctx context.Context, tx *sql.Tx, id string) (offerRow, error) {
	var o offerRow
	var items, created string
	err := tx.QueryRowContext(ctx, `SELECT id, scope_type, scope_id, items_json, created_at, COALESCE(operation_id,''), created_by_kind, self_update
		FROM approval_offers WHERE id = ?`, id).Scan(&o.ID, &o.ScopeType, &o.ScopeID, &items, &created, &o.consumedOp, &o.createdBy, &o.selfUpdate)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	o.CreatedAt = parseOpt(created)
	return o, json.Unmarshal([]byte(items), &o.Items)
}

func policiesTx(ctx context.Context, tx *sql.Tx) (map[PolicyKey]domain.PolicyFields, error) {
	rows, err := tx.QueryContext(ctx, `SELECT scope_type, COALESCE(scope_id,''), mode, tag_track, auto_rollback,
		verify_timeout_seconds, verify_stable_seconds FROM policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPolicies(rows)
}

// DispatchCommands returns the command for an agent's poll: the host's dispatched-but-unacknowledged operation
// again, or else the oldest queued one, which becomes dispatched with a new
// lease. A host with an accepted or running operation gets nothing.
func (s *Store) DispatchCommands(ctx context.Context, hostID string, leaseSeconds int, at time.Time) ([]Operation, error) {
	var out []Operation
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		out = nil
		var activeID, activeState string
		err := tx.QueryRowContext(ctx, `SELECT id, state FROM operations WHERE host_id = ?
			AND state IN ('dispatched','accepted','running','verifying') ORDER BY created_at LIMIT 1`, hostID).Scan(&activeID, &activeState)
		switch {
		case err == nil && activeState == protocol.OpDispatched:
			op, err := getOp(ctx, tx, activeID)
			if err != nil {
				return err
			}
			out = []Operation{op}
			return nil
		case err == nil:
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		var id string
		// A scheduled operation waits for its time without holding up the
		// ones behind it.
		err = tx.QueryRowContext(ctx, `SELECT id FROM operations WHERE host_id = ? AND state = 'queued' AND expires_at > ?
			AND (not_before IS NULL OR not_before <= ?)
			ORDER BY created_at, id LIMIT 1`, hostID, FormatTime(at), FormatTime(at)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		lease := at.Add(time.Duration(leaseSeconds) * time.Second)
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = 'dispatched', attempt = attempt + 1, lease_expires_at = ?,
			updated_at = ? WHERE id = ?`, FormatTime(lease), FormatTime(at), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'deploying', updated_at = ? WHERE operation_id = ? AND status = 'approved'`,
			FormatTime(at), id); err != nil {
			return err
		}
		op, err := getOp(ctx, tx, id)
		if err != nil {
			return err
		}
		out = []Operation{op}
		return nil
	})
	return out, err
}

// BringScheduledForward makes every waiting scheduled operation due now.
// Only the end-to-end rig's test API uses it.
func (s *Store) BringScheduledForward(ctx context.Context, at time.Time) (int64, error) {
	var n int64
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE operations SET not_before = ?, updated_at = ?
			WHERE state = 'queued' AND not_before > ?`, FormatTime(at), FormatTime(at), FormatTime(at))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// SweepResult lists what a sweep changed.
type SweepResult struct {
	Requeued []Operation
	Failed   []Operation // not acknowledged after MaxDispatchAttempts
	Expired  []Operation
}

// SweepOperations applies lease expiry and command expiry.
func (s *Store) SweepOperations(ctx context.Context, at time.Time) (SweepResult, error) {
	var res SweepResult
	now := FormatTime(at)
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		res = SweepResult{}
		ids, err := idsWhere(ctx, tx, `SELECT id FROM operations WHERE state = 'dispatched' AND lease_expires_at < ?`, now)
		if err != nil {
			return err
		}
		for _, id := range ids {
			op, err := getOp(ctx, tx, id)
			if err != nil {
				return err
			}
			if op.Attempt < MaxDispatchAttempts {
				if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = 'queued', lease_expires_at = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'approved', updated_at = ? WHERE operation_id = ? AND status = 'deploying'`, now, id); err != nil {
					return err
				}
				op.State = protocol.OpQueued
				res.Requeued = append(res.Requeued, op)
				continue
			}
			op, err = s.finish(ctx, tx, op, protocol.OpFailed, OperationOutcome{Code: protocol.FailNotAcknowledged,
				Message: fmt.Sprintf("the agent did not acknowledge the command after %d attempts", op.Attempt)}, at)
			if err != nil {
				return err
			}
			res.Failed = append(res.Failed, op)
		}

		ids, err = idsWhere(ctx, tx, `SELECT id FROM operations WHERE state = 'queued' AND expires_at <= ?`, now)
		if err != nil {
			return err
		}
		for _, id := range ids {
			op, err := getOp(ctx, tx, id)
			if err != nil {
				return err
			}
			op, err = s.unwind(ctx, tx, op, protocol.OpExpired, Actor{Kind: "system"}, at)
			if err != nil {
				return err
			}
			res.Expired = append(res.Expired, op)
		}
		return nil
	})
	return res, err
}

// CancelOperation cancels a queued or dispatched operation.
func (s *Store) CancelOperation(ctx context.Context, id string, by Actor, at time.Time) (Operation, error) {
	var op Operation
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		op, err = getOp(ctx, tx, id)
		if err != nil {
			return err
		}
		if op.State != protocol.OpQueued && op.State != protocol.OpDispatched {
			return ErrNotCancellable
		}
		op, err = s.unwind(ctx, tx, op, protocol.OpCancelled, by, at)
		return err
	})
	return op, err
}

// unwind ends an operation that never started (expired or cancelled): its
// updates return to detected and its pending desired states are superseded.
func (s *Store) unwind(ctx context.Context, tx *sql.Tx, op Operation, state string, by Actor, at time.Time) (Operation, error) {
	now := FormatTime(at)
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, lease_expires_at = NULL, updated_at = ?, finished_at = ? WHERE id = ?`,
		state, now, now, op.ID); err != nil {
		return op, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = 'detected', approved_by_kind = NULL, approved_by_discord_id = NULL,
		approved_at = NULL, operation_id = NULL, updated_at = ? WHERE operation_id = ? AND status IN ('approved','deploying')`, now, op.ID); err != nil {
		return op, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE desired_states SET status = 'superseded', resolved_at = ? WHERE operation_id = ? AND status = 'pending'`,
		now, op.ID); err != nil {
		return op, err
	}
	if err := s.audit(ctx, tx, by, at, "operation."+state, "operation", op.ID, nil); err != nil {
		return op, err
	}
	return getOp(ctx, tx, op.ID)
}

// finish ends an operation that ran (or was refused): updates whose container
// was updated become applied and their desired state reached; the rest fail.
func (s *Store) finish(ctx context.Context, tx *sql.Tx, op Operation, state string, out OperationOutcome, at time.Time) (Operation, error) {
	now := FormatTime(at)
	rj, _ := json.Marshal(out)
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, result_json = ?, lease_expires_at = NULL, updated_at = ?, finished_at = ?
		WHERE id = ?`, state, string(rj), now, now, op.ID); err != nil {
		return op, err
	}
	// Identity keys are unique per stack only, and a group spans stacks. A
	// rollback's outcomes and older agents' may have no stack ID.
	type outcomeKey struct{ stack, key string }
	updated := map[outcomeKey]bool{}
	if out.Result != nil {
		for _, c := range out.Result.Containers {
			if c.Outcome == protocol.OutcomeUpdated || c.Outcome == protocol.OutcomeUnchanged {
				updated[outcomeKey{c.StackID, c.IdentityKey}] = true
			}
		}
	}
	for _, t := range op.Payload.Targets {
		for _, svc := range t.Services {
			ok := updated[outcomeKey{t.StackID, svc.IdentityKey}] || updated[outcomeKey{"", svc.IdentityKey}]
			ok = ok && state != protocol.OpFailed && state != protocol.OpInterrupted
			upd, ds := StatusFailed, "failed"
			if ok {
				upd, ds = StatusApplied, "reached"
			}
			if !ok && op.Kind == protocol.CommandDeploy && op.InitiatorKind == "policy" && !ChangedNothing(state, out.Code) {
				// The policy engine must not deploy this digest again: a rollback
				// returns the update to detected, which would start it over.
				if _, err := tx.ExecContext(ctx, `UPDATE updates SET auto_failed_at = ? WHERE operation_id = ? AND container_id = ?`,
					now, op.ID, svc.ContainerID); err != nil {
					return op, err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE updates SET status = ?, updated_at = ? WHERE operation_id = ? AND container_id = ?
				AND status IN ('approved','deploying')`, upd, now, op.ID, svc.ContainerID); err != nil {
				return op, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE desired_states SET status = ?, resolved_at = ? WHERE operation_id = ? AND container_id = ?
				AND status = 'pending'`, ds, now, op.ID, svc.ContainerID); err != nil {
				return op, err
			}
		}
	}
	if err := s.audit(ctx, tx, Actor{Kind: "agent"}, at, "deployment.finished", "operation", op.ID,
		map[string]any{"state": state, "code": out.Code, "kind": op.Kind}); err != nil {
		return op, err
	}
	if err := s.afterFinish(ctx, tx, op, state, out, at); err != nil {
		return op, err
	}
	return getOp(ctx, tx, op.ID)
}

// ChangedNothing reports whether an operation failed before it changed any
// file or container: the agent never saw it or refused it, or the new image
// could not be pulled or was not the approved one. Such a failure
// has nothing to roll back, and the next check may simply try again.
func ChangedNothing(state, code string) bool {
	if state != protocol.OpFailed {
		return false // another stack of the operation did change
	}
	switch code {
	case protocol.FailNotAcknowledged, protocol.FailUnsupported, protocol.FailIneligible, protocol.FailUnsafe,
		protocol.FailPull, protocol.FailDigestMismatch:
		return true
	}
	return false
}

// releaseOperations ends a host's unfinished operations when its agent goes
// away for good: queued and dispatched ones return their updates to pending,
// and the rest can no longer report an end, which would block the host (and
// any /stack merge) for ever.
func (s *Store) releaseOperations(ctx context.Context, tx *sql.Tx, hostID string, by Actor, at time.Time) ([]Operation, error) {
	ids, err := idsWhere(ctx, tx, `SELECT id FROM operations WHERE host_id = ?
		AND state IN ('queued','dispatched','accepted','running','verifying') ORDER BY created_at`, hostID)
	if err != nil {
		return nil, err
	}
	var out []Operation
	for _, id := range ids {
		op, err := getOp(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if op.State == protocol.OpQueued || op.State == protocol.OpDispatched {
			op, err = s.unwind(ctx, tx, op, protocol.OpCancelled, by, at)
		} else {
			op, err = s.finish(ctx, tx, op, protocol.OpInterrupted, OperationOutcome{Code: protocol.FailInterrupted,
				Message: "the agent was replaced or removed while this operation ran; check the stack"}, at)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

// InterruptLostOperation ends an operation its agent no longer knows about
// (it lost its data volume, so nothing will ever report the end). It returns
// false if the operation has ended in the meantime.
func (s *Store) InterruptLostOperation(ctx context.Context, id string, at time.Time) (Operation, bool, error) {
	var op Operation
	lost := false
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		lost = false
		op, err = getOp(ctx, tx, id)
		if err != nil {
			return err
		}
		switch op.State {
		case protocol.OpAccepted, protocol.OpRunning, protocol.OpVerifying:
		default:
			return nil
		}
		op, err = s.finish(ctx, tx, op, protocol.OpInterrupted, OperationOutcome{Code: protocol.FailInterrupted,
			Message: "the agent is running but no longer knows this operation (was its data volume replaced?); check the stack"}, at)
		lost = err == nil
		return err
	})
	return op, lost, err
}

// QueuedOperations counts the operations no agent has started yet.
func (s *Store) QueuedOperations(ctx context.Context) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE state IN ('queued','dispatched')`).Scan(&n)
	return n, err
}

// RunningOperationIDs returns the operations the host's agent has accepted
// and not yet finished.
func (s *Store) RunningOperationIDs(ctx context.Context, hostID string) ([]string, error) {
	return idsWhere(ctx, s.r, `SELECT id FROM operations WHERE host_id = ?
		AND state IN ('accepted','running','verifying') ORDER BY created_at`, hostID)
}

// Transition is a state change caused by events.
type Transition struct {
	Operation Operation
	From      string
	Finished  bool
}

// RecordEvents stores an agent's events for one of its operations,
// de-duplicated on (operation, seq), and applies the operation state machine.
// It returns the highest seq up to which every event is stored (acked_seq).
func (s *Store) RecordEvents(ctx context.Context, agentID, opID string, events []protocol.Event, at time.Time) (int, *Transition, error) {
	var acked int
	var tr *Transition
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		acked, tr = 0, nil
		op, err := getOp(ctx, tx, opID)
		if err != nil {
			return err
		}
		if op.AgentID != agentID {
			return ErrNotFound // never reveal other agents' operations
		}
		from := op.State
		slices.SortFunc(events, func(a, b protocol.Event) int { return a.Seq - b.Seq })
		for _, e := range events {
			if e.Seq < 1 {
				continue
			}
			payload, _ := json.Marshal(e)
			r, err := tx.ExecContext(ctx, `INSERT INTO operation_events (id, operation_id, seq, type, step, message, payload_json, received_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (operation_id, seq) DO NOTHING`,
				s.ids.New(), opID, e.Seq, e.Type, nullString(e.Step), nullString(truncateText(e.Message, 4096)), string(payload), FormatTime(at))
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				continue // duplicate delivery
			}
			op, err = s.applyEvent(ctx, tx, op, e, at)
			if err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, `
			WITH RECURSIVE c(n) AS (SELECT 1 WHERE EXISTS (SELECT 1 FROM operation_events WHERE operation_id = ?1 AND seq = 1)
				UNION ALL SELECT n + 1 FROM c WHERE EXISTS (SELECT 1 FROM operation_events WHERE operation_id = ?1 AND seq = n + 1))
			SELECT COALESCE(MAX(n), 0) FROM c`, opID).Scan(&acked); err != nil {
			return err
		}
		if op.State != from {
			tr = &Transition{Operation: op, From: from, Finished: op.Terminal()}
		}
		return nil
	})
	return acked, tr, err
}

func (s *Store) applyEvent(ctx context.Context, tx *sql.Tx, op Operation, e protocol.Event, at time.Time) (Operation, error) {
	now := FormatTime(at)
	set := func(state string) (Operation, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, updated_at = ? WHERE id = ?`, state, now, op.ID); err != nil {
			return op, err
		}
		op.State = state
		return op, nil
	}
	if op.Terminal() && op.State != protocol.OpExpired && op.State != protocol.OpCancelled {
		return op, nil // events after the end are only stored
	}
	switch e.Type {
	case protocol.EventAccepted:
		if op.State == protocol.OpExpired || op.State == protocol.OpCancelled {
			// The agent is running it anyway; reality wins.
			if _, err := tx.ExecContext(ctx, `UPDATE operations SET finished_at = NULL WHERE id = ?`, op.ID); err != nil {
				return op, err
			}
		}
		if err := s.audit(ctx, tx, Actor{Kind: "agent"}, at, "deployment.started", "operation", op.ID, nil); err != nil {
			return op, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET lease_expires_at = NULL WHERE id = ?`, op.ID); err != nil {
			return op, err
		}
		return set(protocol.OpAccepted)
	case protocol.EventProgress:
		switch {
		case e.Step == protocol.StepVerifying:
			return set(protocol.OpVerifying)
		case op.State == protocol.OpAccepted || op.State == protocol.OpDispatched || op.State == protocol.OpQueued:
			return set(protocol.OpRunning)
		}
		return op, nil
	case protocol.EventRejected:
		return s.finish(ctx, tx, op, protocol.OpFailed, OperationOutcome{Code: e.Code, Message: e.Message}, at)
	case protocol.EventCompleted:
		state := protocol.OpSucceeded
		if e.Result != nil {
			for _, c := range e.Result.Containers {
				if c.Outcome == protocol.OutcomeFailed {
					state = protocol.OpPartiallyFailed
				}
			}
		}
		return s.finish(ctx, tx, op, state, OperationOutcome{Message: e.Message, Result: e.Result}, at)
	case protocol.EventFailed:
		state := protocol.OpFailed
		if e.Result != nil {
			for _, c := range e.Result.Containers {
				if c.Outcome == protocol.OutcomeUpdated {
					state = protocol.OpPartiallyFailed
				}
			}
		}
		return s.finish(ctx, tx, op, state, OperationOutcome{Code: e.Code, Message: e.Message,
			OutputTail: truncateText(e.OutputTail, protocol.MaxOutputTail), Result: e.Result}, at)
	case protocol.EventInterrupted:
		return s.finish(ctx, tx, op, protocol.OpInterrupted, OperationOutcome{Code: protocol.FailInterrupted,
			Message: "the agent restarted while the operation was running", Result: e.Result}, at)
	}
	return op, nil
}

// ActiveOperationIDs returns the host's non-terminal operation IDs.
func (s *Store) ActiveOperationIDs(ctx context.Context, hostID string) ([]string, error) {
	return idsWhere(ctx, s.r, `SELECT id FROM operations WHERE host_id = ?
		AND state IN ('queued','dispatched','accepted','running','verifying') ORDER BY created_at`, hostID)
}

func idsWhere(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:] // keep the tail: the end of the output matters most
}

// DriftEvent is a container whose running digest left its desired state.
type DriftEvent struct {
	Container Container
	Expected  string
	Observed  string
}

// checkDrift compares a container's running digests with its latest reached
// desired state, when no operation is pending for it.
// On drift it sets the badge, audits, and re-bases desired state to what is
// running, so the drift is reported once.
func (s *Store) checkDrift(ctx context.Context, tx *sql.Tx, containerID string, in protocol.Container, at time.Time) (*DriftEvent, error) {
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM desired_states WHERE container_id = ? AND status = 'pending'`,
		containerID).Scan(&pending); err != nil || pending > 0 {
		return nil, err
	}
	var want string
	err := tx.QueryRowContext(ctx, `SELECT image_digest FROM desired_states WHERE container_id = ? AND status = 'reached'
		ORDER BY set_at DESC, id DESC LIMIT 1`, containerID).Scan(&want)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // no baseline until ddum has deployed it
	}
	if err != nil {
		return nil, err
	}
	running := domain.RunningDigests(imageRepository(in.ImageRef).String, in.RepoDigests)
	if len(running) == 0 {
		return nil, nil
	}
	if slices.Contains(running, want) {
		_, err := tx.ExecContext(ctx, `UPDATE containers SET drifted = 0 WHERE id = ? AND drifted = 1`, containerID)
		return nil, err
	}
	now := FormatTime(at)
	if _, err := tx.ExecContext(ctx, `UPDATE containers SET drifted = 1 WHERE id = ?`, containerID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO desired_states (id, container_id, image_ref, image_digest, status, set_at, resolved_at)
		VALUES (?, ?, ?, ?, 'reached', ?, ?)`, s.ids.New(), containerID, in.ImageRef, running[0], now, now); err != nil {
		return nil, err
	}
	if err := s.audit(ctx, tx, Actor{Kind: "system"}, at, "container.drifted", "container", containerID,
		map[string]any{"expected": want, "observed": running[0]}); err != nil {
		return nil, err
	}
	c, err := scanContainer(tx.QueryRowContext(ctx, `SELECT `+containerColumns+containerFrom+` WHERE c.id = ?`, containerID))
	if err != nil {
		return nil, err
	}
	return &DriftEvent{Container: c, Expected: want, Observed: running[0]}, nil
}

func deployTargets(m map[string]*protocol.DeployTarget, order []string) []protocol.DeployTarget {
	out := make([]protocol.DeployTarget, 0, len(order))
	for _, id := range order {
		out = append(out, *m[id])
	}
	return out
}
