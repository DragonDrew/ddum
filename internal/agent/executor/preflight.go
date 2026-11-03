package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

const labelComposeConfigHash = "com.docker.compose.config-hash"

// preflight checks that Compose, running inside the agent, sees a stack the
// way it was last started, before anything is changed. `docker compose
// up` builds the service again from the files and the agent's environment, so a
// mount from ~ or ${HOME}, or a variable only the owner's shell had, would come
// back different, and the deploy would still pass verification, because the
// container runs the right image. It also catches a stack Compose cannot load
// from inside the agent, such as an env_file outside the mounted roots.
//
// Compose's own config hash settles most stacks. When it differs, the hash
// cannot say why, and it may just come from another Compose version than the
// one that started the stack, so the mounts and variables are compared directly.
// A difference there refuses the deploy. Anything else is a warning: the
// recreate will apply it, as it would for `docker compose up` by hand.
func (x *Executor) preflight(ctx context.Context, sp stackPlan) ([]string, error) {
	if sp.portainer != nil {
		return nil, nil // Portainer runs Compose itself, with the stack's own Env
	}
	var names []string
	for _, s := range sp.services {
		if !s.fileOnDesired {
			names = append(names, s.change.IdentityKey)
		}
	}
	if len(names) == 0 {
		return nil, nil // retrying a failed deploy: the files no longer describe the containers
	}
	hashes, err := x.composeHashes(ctx, sp.stack.Compose, names)
	if err != nil {
		return nil, err
	}
	running, err := x.projectContainers(ctx, sp.stack.Compose.Project)
	if err != nil {
		return nil, fail(protocol.FailInternal, "list containers: %v", err)
	}
	var differ []string
	for _, n := range names {
		// No label means nothing to compare with, such as a container started
		// long ago, and an empty hash means this Compose has no answer.
		if c, ok := running[n]; ok && c.configHash != "" && hashes[n] != "" && hashes[n] != c.configHash {
			differ = append(differ, n)
		}
	}
	if len(differ) == 0 {
		return nil, nil
	}
	rendered, err := x.composeRendered(ctx, sp.stack.Compose)
	if err != nil {
		return nil, err
	}
	var warnings []string
	for _, n := range differ {
		if changes := renderedChanges(rendered[n], running[n]); len(changes) > 0 {
			return nil, fail(protocol.FailIneligible, "recreating %s here would change it: %s. Nothing was changed. "+
				"This usually means the stack depends on your shell's environment, which the agent does not have. "+
				"Pass what it needs with STACK_ENV_<NAME> on the agent (for example STACK_ENV_HOME), or set it in the stack's .env",
				n, strings.Join(changes, "; "))
		}
		warnings = append(warnings, n+": its Compose configuration has changed since it was started, so this update also applied that change")
	}
	return warnings, nil
}

// composeHashes returns Compose's config hash of the named services.
func (x *Executor) composeHashes(ctx context.Context, ci *protocol.ComposeInfo, names []string) (map[string]string, error) {
	args := append(composeBase(ci), "config", "--hash", strings.Join(names, ","))
	out, err := x.runLimited(ctx, x.composeLimit(), ci.WorkingDir, args...)
	if err != nil {
		return nil, composeLoadError(err, out)
	}
	hashes := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			hashes[f[0]] = f[1]
		}
	}
	return hashes, nil
}

// renderedService is the part of `docker compose config` the preflight reads.
type renderedService struct {
	Environment map[string]*string `json:"environment"`
	Volumes     []struct {
		Type   string `json:"type"`
		Source string `json:"source"`
		Target string `json:"target"`
	} `json:"volumes"`
}

func (x *Executor) composeRendered(ctx context.Context, ci *protocol.ComposeInfo) (map[string]renderedService, error) {
	args := append(composeBase(ci), "config", "--format", "json")
	out, err := x.runLimited(ctx, x.composeLimit(), ci.WorkingDir, args...)
	if err != nil {
		return nil, composeLoadError(err, out)
	}
	var doc struct {
		Services map[string]renderedService `json:"services"`
	}
	// Warnings can come first in the combined output.
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fail(protocol.FailIneligible, "could not read docker compose config: %v. Nothing was changed", err)
	}
	return doc.Services, nil
}

func composeLoadError(err error, out []byte) error {
	se := fail(protocol.FailIneligible, "docker compose cannot load this stack from inside the agent: %v: %s. "+
		"Nothing was changed. Every file the stack uses must be mounted into the agent at the same path", err, lastLine(out))
	se.output = string(out)
	return se
}

// renderedChanges compares a service as Compose would now create it with its
// running container: bind mounts by path, and variables by name only, because
// their values can be secrets and this text ends up in Discord.
func renderedChanges(want renderedService, c projectContainer) []string {
	var changes []string
	mounts := map[string]string{}
	for _, m := range c.mounts {
		if m.Type == "bind" {
			mounts[m.Destination] = m.Source
		}
	}
	for _, v := range want.Volumes {
		if v.Type != "bind" {
			continue
		}
		if got, ok := mounts[v.Target]; !ok {
			changes = append(changes, fmt.Sprintf("%s would be mounted from %s, where it is not now", v.Target, v.Source))
		} else if got != v.Source {
			changes = append(changes, fmt.Sprintf("%s would be mounted from %s instead of %s", v.Target, v.Source, got))
		}
	}
	env := map[string]string{}
	for _, kv := range c.env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	var vars []string
	for k, v := range want.Environment {
		got, ok := env[k]
		if v == nil {
			continue // passed through from the environment: the container's own value is what it had
		}
		if !ok || got != *v {
			vars = append(vars, k)
		}
	}
	sort.Strings(vars)
	if len(vars) > 0 {
		changes = append(changes, "these variables would change: "+strings.Join(vars, ", "))
	}
	return changes
}

// composeLimit is the time allowed for one Compose command.
func (x *Executor) composeLimit() time.Duration {
	if x.ComposeTimeout > 0 {
		return x.ComposeTimeout
	}
	return defaultComposeTimeout
}

// restoreEdits puts edited files back after `compose up` failed, unless a
// container did change, in which case the files describe what now runs and
// Roll back is the way out. It reports whether the files were restored.
func (x *Executor) restoreEdits(ctx context.Context, project string, names []string, before map[string]projectContainer, edited map[string][]byte) bool {
	if len(edited) == 0 {
		return false
	}
	after, err := x.projectContainers(ctx, project)
	if err != nil {
		return false
	}
	for _, n := range names {
		if after[n].id != before[n].id {
			return false
		}
	}
	for path, data := range edited {
		if err := replaceFile(path, data); err != nil {
			x.Log.Warn("could not put a Compose file back", "file", path, "err", err)
			return false
		}
	}
	return true
}
