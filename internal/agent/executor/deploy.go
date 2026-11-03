package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/portainer"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// servicePlan is one container to move.
type servicePlan struct {
	change     protocol.ServiceChange
	container  protocol.Container
	repo       string // normalized repository
	currentTag string
	desiredTag string
	pullRef    string // repo@digest
	// imageID, when set, is the exact local image a restore must run (the
	// pinned image); it proves the digest even if Docker forgot RepoDigests.
	imageID string
	tagRef  string // repo:desiredTag
	edit    *edit  // nil when the tag does not change
	// fileOnDesired means the file already names the desired tag, after a
	// failed deploy, so the file no longer describes the running container.
	fileOnDesired bool
}

// edit is one in-place change of a tag source.
type edit struct {
	file     string
	variable string // env file edit when set
	line     int
	column   int
	oldValue string
	newValue string
}

// stackPlan is one target stack.
type stackPlan struct {
	stack    protocol.Stack
	services []servicePlan
	// portainer is set for Portainer stacks, which change through the API.
	portainer *portainerPlan
}

// plan validates a deploy against the latest inventory and the agent-side
// safety rules, and works out every pull, tag, and edit.
func (x *Executor) plan(p protocol.DeployPayload) ([]stackPlan, error) {
	if len(p.Targets) == 0 {
		return nil, fail(protocol.FailUnsafe, "the operation has no targets")
	}
	if p.SelfUpdate && (len(p.Targets) != 1 || len(p.Targets[0].Services) != 1) {
		return nil, fail(protocol.FailUnsafe, "a self-update must contain only the bot")
	}
	inv := x.Inventory()
	var out []stackPlan
	for _, t := range p.Targets {
		// Rule 1: only stacks and containers this agent discovered itself.
		var st *protocol.Stack
		for i := range inv {
			if inv[i].Fingerprint == t.StackFingerprint {
				st = &inv[i]
			}
		}
		if st == nil {
			return nil, fail(protocol.FailUnsafe, "stack %s is not in this agent's inventory", t.StackFingerprint)
		}
		isPortainer := st.Provider == protocol.ProviderPortainer
		if st.Provider != protocol.ProviderCompose && !isPortainer {
			return nil, fail(protocol.FailUnsupported, "stack %s is managed by %s, which this agent cannot deploy", st.Fingerprint, st.Provider)
		}
		if !st.Deploy.Supported || st.Compose == nil {
			return nil, fail(protocol.FailIneligible, "stack %s cannot be deployed: %s", st.Fingerprint, st.Deploy.Reason)
		}
		sp := stackPlan{stack: *st}
		var classes map[string]protocol.TagChange
		var locs map[string]discovery.TagLocation
		if !isPortainer {
			files, envFiles, err := x.readSources(st.Compose)
			if err != nil {
				return nil, err
			}
			classes, locs, err = discovery.LocateTagSources(files, envFiles)
			if err != nil {
				return nil, fail(protocol.FailIneligible, "read Compose files: %v", err)
			}
		}
		if len(t.Services) == 0 {
			return nil, fail(protocol.FailUnsafe, "target %s has no services", t.StackFingerprint)
		}
		for _, svc := range t.Services {
			var c *protocol.Container
			for i := range st.Containers {
				if st.Containers[i].IdentityKey == svc.IdentityKey {
					c = &st.Containers[i]
				}
			}
			if c == nil {
				return nil, fail(protocol.FailUnsafe, "service %s is not in stack %s", svc.IdentityKey, st.Fingerprint)
			}
			// Rule 6: never stop or recreate a protected container.
			if c.Protected {
				return nil, fail(protocol.FailUnsafe, "service %s is protected", svc.IdentityKey)
			}
			// The bot is updated only by a self-update operation that contains
			// nothing else.
			if (c.Self == protocol.SelfBot) != p.SelfUpdate {
				return nil, fail(protocol.FailUnsafe, "service %s: the ddum bot is updated only by a self-update operation, alone", svc.IdentityKey)
			}
			if slices.Contains(c.Notes, protocol.NoteReplicasUnsupported) {
				return nil, fail(protocol.FailUnsupported, "service %s runs several replicas", svc.IdentityKey)
			}
			s, err := planService(svc, *c)
			if err != nil {
				return nil, err
			}
			// A file changed without `docker compose up` holds someone's pending
			// edit: redeploying would apply or overwrite it. A file already on
			// the desired tag is a failed deploy being retried.
			if loc, ok := locs[svc.IdentityKey]; ok && !isPortainer && loc.Tag != "" && loc.Tag != s.currentTag && loc.Tag != s.desiredTag {
				return nil, fail(protocol.FailIneligible, "the Compose file names %s for %s, but the container runs %s; run docker compose up or undo the edit, then check again",
					loc.Tag, svc.IdentityKey, s.currentTag)
			}
			if loc, ok := locs[svc.IdentityKey]; ok && !isPortainer && loc.Tag == s.desiredTag && s.currentTag != s.desiredTag {
				s.fileOnDesired = true
			}
			if s.currentTag != s.desiredTag && !isPortainer {
				tc, ok := classes[svc.IdentityKey]
				if !ok || !tc.Supported {
					reason := tc.Reason
					if !ok {
						reason = "service not found in the Compose files"
					}
					return nil, fail(protocol.FailIneligible, "the tag of %s cannot be changed here: %s", svc.IdentityKey, reason)
				}
				loc := locs[svc.IdentityKey]
				if tc.Source == discovery.TagSourceEnvFile {
					s.edit = &edit{file: loc.VarFile, variable: tc.Variable, newValue: s.desiredTag}
				} else {
					s.edit = &edit{file: loc.File, line: loc.Line, column: loc.Column, oldValue: loc.Value,
						newValue: withTag(loc.Value, s.desiredTag)}
					if n, err := reference.ParseNormalizedNamed(s.edit.newValue); err != nil || n.Name() != s.repo {
						return nil, fail(protocol.FailUnsafe, "the image: value of %s names another repository", svc.IdentityKey)
					}
				}
				// Rule 4: edited files resolve under COMPOSE_ALLOWED_ROOTS.
				if err := x.checkPath(s.edit.file); err != nil {
					return nil, err
				}
			}
			sp.services = append(sp.services, s)
		}
		if isPortainer {
			if err := x.planPortainer(context.Background(), &sp, t); err != nil {
				return nil, err
			}
		}
		out = append(out, sp)
	}
	return out, nil
}

// planService applies rules 2 and 3: same repository, digest-pinned target.
func planService(svc protocol.ServiceChange, c protocol.Container) (servicePlan, error) {
	s := servicePlan{change: svc, container: c}
	cur, err := reference.ParseNormalizedNamed(c.ImageRef)
	if err != nil {
		return s, fail(protocol.FailUnsafe, "configured image of %s does not parse", svc.IdentityKey)
	}
	des, err := reference.ParseNormalizedNamed(svc.Desired.ImageRef + "@" + svc.Desired.Digest)
	if err != nil {
		return s, fail(protocol.FailUnsafe, "desired image of %s is not a valid reference with a digest", svc.IdentityKey)
	}
	if _, ok := des.(reference.Canonical); !ok {
		return s, fail(protocol.FailUnsafe, "desired image of %s has no digest", svc.IdentityKey)
	}
	if cur.Name() != des.Name() {
		return s, fail(protocol.FailUnsafe, "%s would change repository from %s to %s", svc.IdentityKey, cur.Name(), des.Name())
	}
	// The approval was for a change from the state the bot recorded. A container
	// changed by hand since then must not be moved, perhaps back to an older
	// release. No digest for this repository says nothing: Docker can lose the
	// record of a running image when its tag moves. A rollback does not come
	// through here.
	if svc.Current.Digest != "" && hasAnyDigest(c.RepoDigests, cur.Name()) && !hasDigest(c.RepoDigests, cur.Name(), svc.Current.Digest) {
		return s, fail(protocol.FailIneligible, "%s changed after this update was approved: it no longer runs %s", svc.IdentityKey, svc.Current.Digest)
	}
	s.repo = cur.Name()
	s.currentTag = tagOf(cur)
	s.desiredTag = tagOf(des)
	s.pullRef = s.repo + "@" + svc.Desired.Digest
	s.tagRef = s.repo + ":" + s.desiredTag
	return s, nil
}

func tagOf(n reference.Named) string {
	if t, ok := n.(reference.Tagged); ok {
		return t.Tag()
	}
	return "latest"
}

// withTag replaces the tag of an image: value, keeping the repository text
// exactly as written ("nginx:1.27" → "nginx:1.28", "nginx" → "nginx:1.28").
func withTag(value, tag string) string {
	slash := strings.LastIndex(value, "/")
	if i := strings.LastIndex(value, ":"); i > slash {
		value = value[:i]
	}
	return value + ":" + tag
}

func (x *Executor) readSources(ci *protocol.ComposeInfo) ([]discovery.SourceFile, []discovery.SourceFile, error) {
	var files, envFiles []discovery.SourceFile
	for _, p := range ci.ConfigFiles {
		if err := x.checkPath(p); err != nil {
			return nil, nil, err
		}
		data, err := x.Files.ReadFile(p)
		if err != nil {
			return nil, nil, fail(protocol.FailIneligible, "read %s: %v", p, err)
		}
		files = append(files, discovery.SourceFile{Path: p, Data: data})
	}
	for _, p := range discovery.EnvFilePaths(ci) {
		if _, err := x.Files.Resolve(p); err != nil {
			continue // the default .env is optional
		}
		if err := x.checkPath(p); err != nil {
			return nil, nil, err
		}
		data, err := x.Files.ReadFile(p)
		if err != nil {
			return nil, nil, fail(protocol.FailIneligible, "read %s: %v", p, err)
		}
		envFiles = append(envFiles, discovery.SourceFile{Path: p, Data: data})
	}
	return files, envFiles, nil
}

// checkPath enforces safety rule 4 after symlink resolution.
func (x *Executor) checkPath(p string) error {
	resolved, err := x.Files.Resolve(p)
	if err != nil {
		return fail(protocol.FailIneligible, "%s is not readable inside the agent", p)
	}
	for _, r := range x.AllowedRoots {
		if resolved == r || strings.HasPrefix(resolved, strings.TrimSuffix(r, "/")+"/") {
			return nil
		}
	}
	return fail(protocol.FailUnsafe, "%s resolves to %s, outside COMPOSE_ALLOWED_ROOTS", p, resolved)
}

// deploy runs the provider-neutral steps for Compose stacks. The result lists per-container outcomes, also on failure.
func (x *Executor) deploy(ctx context.Context, p protocol.DeployPayload, progress func(step, msg string)) (*protocol.Result, error) {
	progress(protocol.StepValidating, "checking the operation against this host")
	plans, err := x.plan(p) // files may have changed since the command was accepted
	if err != nil {
		return nil, err
	}
	res := &protocol.Result{}
	for i, sp := range plans {
		for _, s := range sp.services {
			res.Containers = append(res.Containers, protocol.ContainerOutcome{IdentityKey: s.change.IdentityKey,
				StackID: p.Targets[i].StackID, Outcome: protocol.OutcomeSkipped})
		}
	}
	// Before anything changes in any stack, so a group stays all or nothing.
	for _, sp := range plans {
		warnings, err := x.preflight(ctx, sp)
		if err != nil {
			return nil, err
		}
		res.Warnings = append(res.Warnings, warnings...)
	}

	for i, sp := range plans {
		// Identity keys are unique per stack only, and a group spans stacks.
		setOutcome := func(key, outcome, msg string) {
			for j := range res.Containers {
				if res.Containers[j].StackID == p.Targets[i].StackID && res.Containers[j].IdentityKey == key {
					res.Containers[j].Outcome, res.Containers[j].Message = outcome, msg
				}
			}
		}
		progress(protocol.StepSnapshotting, "pinning the current images of "+sp.stack.DisplayName)
		snapID, err := x.snapshot(ctx, p.OperationID, p.Targets[i].StackID, sp)
		if err != nil {
			return res, fail(protocol.FailInternal, "snapshot: %v", err)
		}
		res.Snapshots = append(res.Snapshots, protocol.SnapshotRef{SnapshotID: snapID, StackID: p.Targets[i].StackID, Created: true})
		x.Journal.Update(p.OperationID, func(e *Entry) { e.SnapshotIDs = append(e.SnapshotIDs, snapID) })

		// Pull, tag, and check the digest before recreating anything.
		for _, s := range sp.services {
			progress(protocol.StepPulling, "pulling "+s.pullRef)
			if out, err := x.pull(ctx, s.pullRef); err != nil {
				setOutcome(s.change.IdentityKey, protocol.OutcomeFailed, "pull failed")
				se := fail(protocol.FailPull, "pull %s: %v", s.pullRef, err)
				se.output = string(out)
				return res, se
			}
			if err := x.Engine.Tag(ctx, s.pullRef, s.tagRef); err != nil {
				return res, fail(protocol.FailPull, "tag %s as %s: %v", s.pullRef, s.tagRef, err)
			}
			img, err := x.Engine.Image(ctx, s.tagRef)
			if err != nil || !hasDigest(img.RepoDigests, s.repo, s.change.Desired.Digest) {
				setOutcome(s.change.IdentityKey, protocol.OutcomeFailed, "digest mismatch")
				return res, fail(protocol.FailDigestMismatch, "%s does not resolve to the approved digest %s; nothing was recreated",
					s.tagRef, s.change.Desired.Digest)
			}
		}

		progress(protocol.StepDeploying, "updating "+sp.stack.DisplayName)
		edited := map[string][]byte{} // each edited file as it was
		for _, s := range sp.services {
			if s.edit == nil {
				continue
			}
			if _, seen := edited[s.edit.file]; !seen {
				data, err := os.ReadFile(s.edit.file)
				if err != nil {
					return res, fail(protocol.FailDeploy, "read %s: %v", s.edit.file, err)
				}
				edited[s.edit.file] = data
			}
			if err := applyEdit(*s.edit); err != nil {
				return res, fail(protocol.FailDeploy, "edit the tag of %s: %v", s.change.IdentityKey, err)
			}
		}

		before, _ := x.projectContainers(ctx, sp.stack.Compose.Project)
		var names []string
		for _, s := range sp.services {
			names = append(names, s.change.IdentityKey)
		}
		if sp.portainer != nil {
			// Portainer redeploys the stack itself; Compose only recreates the
			// services whose image changed.
			if err := x.applyPortainer(ctx, sp.portainer); err != nil {
				for _, n := range names {
					setOutcome(n, protocol.OutcomeFailed, "Portainer redeploy failed")
				}
				return res, err
			}
		} else if out, err := x.composeUp(ctx, sp.stack.Compose, names); err != nil {
			for _, n := range names {
				setOutcome(n, protocol.OutcomeFailed, "docker compose up failed")
			}
			se := fail(protocol.FailDeploy, "docker compose up: %v: %s", err, lastLine(out))
			se.output = string(out)
			if x.restoreEdits(ctx, sp.stack.Compose.Project, names, before, edited) {
				se.msg += "; no container changed, so the Compose files were put back"
			}
			return res, se
		}

		progress(protocol.StepVerifying, "waiting for the containers to be running and healthy")
		after, err := x.verify(ctx, sp, before, p.Verify, setOutcome)
		for _, c := range before {
			if c.protected {
				if a, ok := after[c.key]; ok && a.id != c.id {
					res.Warnings = append(res.Warnings, "protected container "+c.key+" was recreated by Compose")
				}
			}
		}
		if err != nil {
			return res, err
		}
		// A stack Portainer can no longer read is not held against
		// a deploy whose containers just passed.
		if sp.portainer != nil {
			if ps, err := x.Portainer.Stack(ctx, sp.portainer.stack.ID); err == nil && ps.Status == portainer.StatusInactive {
				return res, fail(protocol.FailVerify, "Portainer reports stack %s as inactive after the update", ps.Name)
			}
		}
		for j := range res.Containers {
			if res.Containers[j].StackID != p.Targets[i].StackID {
				continue
			}
			if a, ok := after[res.Containers[j].IdentityKey]; ok && res.Containers[j].Outcome == protocol.OutcomeUpdated {
				res.Containers[j].DockerID = a.id
				res.Containers[j].NewDigest = findDigest(sp, res.Containers[j].IdentityKey)
			}
		}
	}
	progress(protocol.StepFinalising, "done")
	return res, nil
}

func findDigest(sp stackPlan, key string) string {
	for _, s := range sp.services {
		if s.change.IdentityKey == key {
			return s.change.Desired.Digest
		}
	}
	return ""
}

// composeBase is `docker compose` pointed at a project. The default .env is
// read by Compose itself; labelled env files are passed explicitly.
func composeBase(ci *protocol.ComposeInfo) []string {
	args := []string{"compose", "--project-name", ci.Project, "--project-directory", ci.WorkingDir}
	for _, f := range ci.ConfigFiles {
		args = append(args, "-f", f)
	}
	for _, e := range splitList(ci.EnvironmentFile) {
		args = append(args, "--env-file", e)
	}
	return args
}

// composeArgs builds `docker compose ... up -d --no-deps --pull never <svc>...`
// .
func composeArgs(ci *protocol.ComposeInfo, services []string) []string {
	args := append(composeBase(ci), "up", "-d", "--no-deps", "--pull", "never")
	return append(args, services...)
}

// lastLine is the last non-empty line of command output, short enough for a
// result card; the whole output stays in the event's output tail.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return truncateRunes(strings.TrimSpace(lines[len(lines)-1]), 300)
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// hasDigest reports whether repoDigests holds repo@digest.
func hasDigest(repoDigests []string, repo, digest string) bool {
	for _, rd := range repoDigests {
		n, err := reference.ParseNormalizedNamed(rd)
		if err != nil {
			continue
		}
		if c, ok := n.(reference.Canonical); ok && n.Name() == repo && c.Digest().String() == digest {
			return true
		}
	}
	return false
}

// hasAnyDigest reports whether repoDigests holds a digest of repo.
func hasAnyDigest(repoDigests []string, repo string) bool {
	for _, rd := range repoDigests {
		if n, err := reference.ParseNormalizedNamed(rd); err == nil && n.Name() == repo {
			return true
		}
	}
	return false
}

// Limits on one docker command. A command that hangs would otherwise block
// the host for good: an accepted operation cannot be cancelled, and the host
// runs one operation at a time.
const (
	defaultPullTimeout    = 30 * time.Minute
	defaultComposeTimeout = 15 * time.Minute
)

// runLimited runs a docker command for at most limit, saying so if it hits it.
func (x *Executor) runLimited(ctx context.Context, limit time.Duration, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := x.Runner.Run(ctx, dir, args...)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("gave up after %s: %w", limit, err)
	}
	return out, err
}

// composeUp recreates the named services.
func (x *Executor) composeUp(ctx context.Context, ci *protocol.ComposeInfo, names []string) ([]byte, error) {
	return x.runLimited(ctx, x.composeLimit(), ci.WorkingDir, composeArgs(ci, names)...)
}

// pull runs docker pull, retrying after the PullRetryDelays when it fails.
// It returns the last attempt's output and error.
func (x *Executor) pull(ctx context.Context, ref string) ([]byte, error) {
	delays := x.PullRetryDelays
	if delays == nil {
		delays = []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}
	}
	limit := x.PullTimeout
	if limit <= 0 {
		limit = defaultPullTimeout
	}
	out, err := x.runLimited(ctx, limit, "", "pull", ref)
	for _, d := range delays {
		if err == nil {
			return out, nil
		}
		x.Log.Warn("pull failed; retrying", "image", ref, "in", d, "err", err, "output", tail(string(out), 300))
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(d):
		}
		out, err = x.runLimited(ctx, limit, "", "pull", ref)
	}
	return out, err
}
