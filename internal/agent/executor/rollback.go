package executor

import (
	"context"

	"github.com/distribution/reference"

	"github.com/DragonDrew/ddum/internal/agent/discovery"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// rollbackPlan is one snapshot to restore onto its stack.
type rollbackPlan struct {
	meta  *snapshotMeta
	stack protocol.Stack
}

// planRollback validates a rollback against the snapshots and the latest
// inventory, applying the same safety rules as a deploy.
func (x *Executor) planRollback(p protocol.RollbackPayload) ([]rollbackPlan, error) {
	if len(p.SnapshotIDs) == 0 {
		return nil, fail(protocol.FailUnsafe, "the rollback names no snapshot")
	}
	inv := x.Inventory()
	var out []rollbackPlan
	for _, id := range p.SnapshotIDs {
		m, err := x.loadSnapshot(id)
		if err != nil {
			return nil, fail(protocol.FailIneligible, "snapshot %s is not on this agent", id)
		}
		if m.OperationID != p.OriginalOperationID {
			return nil, fail(protocol.FailUnsafe, "snapshot %s belongs to another operation", id)
		}
		if !x.usable(context.Background(), m) {
			return nil, fail(protocol.FailIneligible, "snapshot %s is no longer usable (a pinned image or saved file is gone)", id)
		}
		var st *protocol.Stack
		for i := range inv {
			if inv[i].Fingerprint == m.StackFingerprint {
				st = &inv[i]
			}
		}
		if st == nil || st.Compose == nil {
			return nil, fail(protocol.FailUnsafe, "stack %s is not in this agent's inventory", m.StackFingerprint)
		}
		if m.PortainerStackID == 0 {
			if _, err := x.composeRestoreEdits(m, *st); err != nil {
				return nil, err
			}
		}
		for _, s := range m.Services {
			found := false
			for _, c := range st.Containers {
				if c.IdentityKey == s.IdentityKey {
					found = true
					if c.Protected {
						return nil, fail(protocol.FailUnsafe, "service %s is protected", s.IdentityKey)
					}
				}
			}
			if !found {
				return nil, fail(protocol.FailUnsafe, "service %s is not in stack %s", s.IdentityKey, m.StackFingerprint)
			}
			if _, err := reference.ParseNormalizedNamed(s.ImageRef); err != nil {
				return nil, fail(protocol.FailUnsafe, "the saved image of %s does not parse", s.IdentityKey)
			}
		}
		out = append(out, rollbackPlan{meta: m, stack: *st})
	}
	return out, nil
}

// rollback restores the snapshots of a rollback operation.
func (x *Executor) rollback(ctx context.Context, p protocol.RollbackPayload, progress func(step, msg string)) (*protocol.Result, error) {
	progress(protocol.StepValidating, "checking the snapshots")
	plans, err := x.planRollback(p)
	if err != nil {
		return nil, err
	}
	res := &protocol.Result{}
	for _, rp := range plans {
		outcomes, err := x.restore(ctx, rp, p.Verify, progress)
		res.Containers = append(res.Containers, outcomes...)
		if err != nil {
			return res, err
		}
	}
	progress(protocol.StepFinalising, "done")
	return res, nil
}

// composeRestoreEdits works out the tag edits that put a Compose stack's files
// back, after checking them against the allowed roots.
func (x *Executor) composeRestoreEdits(m *snapshotMeta, st protocol.Stack) ([]edit, error) {
	files, envFiles, err := x.readSources(st.Compose)
	if err != nil {
		return nil, err
	}
	edits, err := restoreEdits(m, st, files, envFiles)
	if err != nil {
		return nil, err
	}
	for _, e := range edits {
		if err := x.checkPath(e.file); err != nil {
			return nil, err
		}
	}
	return edits, nil
}

// restoreEdits works out how to put back the tags a deployment changed, and
// nothing else, so anything the owner changed since the deploy stays. A
// service whose files already name the old tag needs no edit: a digest update
// never changed its tag, and a failed deploy may already have been put back. A
// tag that is neither the old nor the deployed one is someone's own edit, so it
// is refused rather than overwritten.
func restoreEdits(m *snapshotMeta, st protocol.Stack, files, envFiles []discovery.SourceFile) ([]edit, error) {
	classes, locs, err := discovery.LocateTagSources(files, envFiles)
	if err != nil {
		return nil, fail(protocol.FailIneligible, "read Compose files: %v", err)
	}
	var edits []edit
	for _, s := range m.Services {
		n, err := reference.ParseNormalizedNamed(s.ImageRef)
		if err != nil {
			return nil, fail(protocol.FailUnsafe, "the saved image of %s does not parse", s.IdentityKey)
		}
		want := tagOf(n)
		loc, ok := locs[s.IdentityKey]
		if !ok {
			return nil, fail(protocol.FailIneligible, "%s is no longer in the stack's files, so its tag cannot be put back", s.IdentityKey)
		}
		if loc.Tag == "" || loc.Tag == want {
			continue
		}
		running := ""
		for _, c := range st.Containers {
			if c.IdentityKey == s.IdentityKey {
				if r, err := reference.ParseNormalizedNamed(c.ImageRef); err == nil {
					running = tagOf(r)
				}
			}
		}
		if s.NewTag != "" && loc.Tag != s.NewTag && loc.Tag != running {
			return nil, fail(protocol.FailIneligible, "the files name %s for %s, which this deployment did not put there; undo that edit or update the stack by hand",
				loc.Tag, s.IdentityKey)
		}
		tc := classes[s.IdentityKey]
		if !tc.Supported {
			return nil, fail(protocol.FailIneligible, "the tag of %s cannot be put back here: %s", s.IdentityKey, tc.Reason)
		}
		if tc.Variable != "" {
			edits = append(edits, edit{file: loc.VarFile, variable: tc.Variable, newValue: want})
			continue
		}
		value := withTag(loc.Value, want)
		if v, err := reference.ParseNormalizedNamed(value); err != nil || v.Name() != n.Name() {
			return nil, fail(protocol.FailUnsafe, "the image: value of %s names another repository", s.IdentityKey)
		}
		edits = append(edits, edit{file: loc.File, line: loc.Line, column: loc.Column, oldValue: loc.Value, newValue: value})
	}
	return edits, nil
}

// restore puts a stack back as the snapshot recorded it: the tags in its files,
// the image tags (from the pins), and the containers, then verifies the digests.
func (x *Executor) restore(ctx context.Context, rp rollbackPlan, v protocol.VerifySettings, progress func(step, msg string)) ([]protocol.ContainerOutcome, error) {
	m := rp.meta
	var outcomes []protocol.ContainerOutcome
	set := func(key, outcome, msg string) {
		for i := range outcomes {
			if outcomes[i].IdentityKey == key {
				outcomes[i].Outcome, outcomes[i].Message = outcome, msg
			}
		}
	}
	sp := stackPlan{stack: rp.stack}
	var names []string
	for _, s := range m.Services {
		outcomes = append(outcomes, protocol.ContainerOutcome{IdentityKey: s.IdentityKey, StackID: m.StackID, Outcome: protocol.OutcomeSkipped})
		n, _ := reference.ParseNormalizedNamed(s.ImageRef)
		sp.services = append(sp.services, servicePlan{repo: n.Name(), imageID: s.ImageID,
			change: protocol.ServiceChange{IdentityKey: s.IdentityKey, ContainerID: s.ContainerID,
				Desired: protocol.ImageState{ImageRef: s.ImageRef, Digest: s.Digest}}})
		names = append(names, s.IdentityKey)
	}

	progress(protocol.StepDeploying, "restoring the tags and images of "+rp.stack.DisplayName)
	var pp *portainerPlan
	if m.PortainerStackID != 0 {
		var err error
		if pp, err = x.planPortainerRestore(ctx, m, rp.stack); err != nil {
			return outcomes, err
		}
	} else {
		edits, err := x.composeRestoreEdits(m, rp.stack)
		if err != nil {
			return outcomes, err
		}
		for _, e := range edits {
			if err := applyEdit(e); err != nil {
				return outcomes, fail(protocol.FailDeploy, "put back the tag in %s: %v", e.file, err)
			}
		}
	}
	for _, s := range m.Services {
		if err := x.Engine.Tag(ctx, s.Pin, s.ImageRef); err != nil {
			set(s.IdentityKey, protocol.OutcomeFailed, "retag failed")
			return outcomes, fail(protocol.FailDeploy, "tag %s as %s: %v", s.Pin, s.ImageRef, err)
		}
		// Removing an image's last tag also drops its repo@digest record. Pulling
		// by digest restores it (only the manifest moves: the layers are local),
		// so update detection keeps working. Best effort.
		if n, err := reference.ParseNormalizedNamed(s.ImageRef); err == nil && s.Digest != "" {
			if out, err := x.pull(ctx, n.Name()+"@"+s.Digest); err != nil {
				x.Log.Warn("could not restore the digest record", "image", s.ImageRef, "err", err, "output", tail(string(out), 300))
			}
		}
	}
	before, _ := x.projectContainers(ctx, rp.stack.Compose.Project)
	if pp != nil {
		if err := x.applyPortainer(ctx, pp); err != nil {
			for _, n := range names {
				set(n, protocol.OutcomeFailed, "Portainer redeploy failed")
			}
			return outcomes, err
		}
	} else if out, err := x.composeUp(ctx, rp.stack.Compose, names); err != nil {
		for _, n := range names {
			set(n, protocol.OutcomeFailed, "docker compose up failed")
		}
		se := fail(protocol.FailDeploy, "docker compose up: %v", err)
		se.output = string(out)
		return outcomes, se
	}
	progress(protocol.StepVerifying, "waiting for the restored containers")
	after, err := x.verify(ctx, sp, before, v, set)
	for i := range outcomes {
		if outcomes[i].Outcome == protocol.OutcomeUpdated {
			outcomes[i].NewDigest = m.Services[i].Digest
			if a, ok := after[outcomes[i].IdentityKey]; ok {
				outcomes[i].DockerID = a.id
			}
		}
	}
	return outcomes, err
}

// rollbackLocally restores a snapshot without the bot, after a failed bot
// self-update.
func (x *Executor) rollbackLocally(ctx context.Context, snapshotID string, v protocol.VerifySettings, progress func(step, msg string)) error {
	m, err := x.loadSnapshot(snapshotID)
	if err != nil {
		return err
	}
	var st *protocol.Stack
	for _, s := range x.Inventory() {
		if s.Fingerprint == m.StackFingerprint {
			s := s
			st = &s
		}
	}
	if st == nil || st.Compose == nil {
		return fail(protocol.FailInternal, "stack %s is gone", m.StackFingerprint)
	}
	_, err = x.restore(ctx, rollbackPlan{meta: m, stack: *st}, v, progress)
	return err
}
