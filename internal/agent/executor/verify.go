package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// projectContainer is one service's container in a Compose project.
type projectContainer struct {
	key          string
	id           string
	imageID      string
	state        string
	health       string
	restartCount int
	protected    bool
	configHash   string
	mounts       []docker.Mount
	env          []string
}

// projectContainers returns each service's lowest-numbered container.
func (x *Executor) projectContainers(ctx context.Context, project string) (map[string]projectContainer, error) {
	list, err := x.Engine.Containers(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]projectContainer{}
	nums := map[string]int{}
	for _, c := range list {
		l := c.Labels
		if l[discovery.LabelComposeProject] != project || strings.EqualFold(l[discovery.LabelComposeOneoff], "true") {
			continue
		}
		if img, err := x.Engine.Image(ctx, c.ImageID); err == nil {
			l = discovery.OwnLabels(l, img.Labels)
			if l[discovery.LabelComposeProject] != project {
				continue // the image claimed this project
			}
		}
		key := l[discovery.LabelComposeService]
		n, err := strconv.Atoi(l[discovery.LabelComposeContainerNumber])
		if err != nil {
			n = 1 << 30
		}
		if prev, ok := nums[key]; ok && prev <= n {
			continue
		}
		nums[key] = n
		out[key] = projectContainer{key: key, id: c.ID, imageID: c.ImageID, state: c.State, health: c.Health,
			restartCount: c.RestartCount, configHash: l[labelComposeConfigHash], mounts: c.Mounts, env: c.Env,
			protected: strings.EqualFold(l[discovery.LabelProtected], "true") || l[discovery.LabelSelf] == protocol.SelfAgent}
	}
	return out, nil
}

// verify waits until every target container runs the approved digest and is
// healthy (or stable without a health check), and every other container that
// was running before still runs. It returns the final containers.
func (x *Executor) verify(ctx context.Context, sp stackPlan, before map[string]projectContainer, v protocol.VerifySettings,
	setOutcome func(key, outcome, msg string)) (map[string]projectContainer, error) {
	timeout := time.Duration(v.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	stable := time.Duration(v.StableSeconds) * time.Second
	start := x.Now()
	deadline := start.Add(timeout)
	realStart := time.Now() // guards against a clock that does not move
	type watch struct {
		since    time.Time
		restarts int
	}
	watches := map[string]*watch{}
	targets := map[string]servicePlan{}
	for _, s := range sp.services {
		targets[s.change.IdentityKey] = s
	}
	var last map[string]projectContainer
	problems := map[string]string{}

	for {
		now, err := x.projectContainers(ctx, sp.stack.Compose.Project)
		if err != nil {
			return last, fail(protocol.FailVerify, "list containers: %v", err)
		}
		last = now
		clear(problems)
		done := 0
		for key, s := range targets {
			c, ok := now[key]
			switch {
			case !ok:
				problems[key] = "container not found"
				continue
			case c.state != "running":
				problems[key] = "container is " + c.state
				delete(watches, key)
				continue
			}
			if s.imageID == "" || c.imageID != s.imageID {
				img, err := x.Engine.Image(ctx, c.imageID)
				if err != nil || !hasDigest(img.RepoDigests, s.repo, s.change.Desired.Digest) {
					problems[key] = "running image is not the approved digest"
					continue
				}
			}
			switch c.health {
			case "unhealthy":
				setOutcome(key, protocol.OutcomeFailed, "unhealthy")
				return now, fail(protocol.FailVerify, "%s is unhealthy", key)
			case "starting":
				problems[key] = "health check still starting"
				continue
			case "healthy":
				done++
				continue
			}
			// No health check: running with no restart for stable seconds.
			w := watches[key]
			if w == nil || w.restarts != c.restartCount {
				w = &watch{since: x.Now(), restarts: c.restartCount}
				watches[key] = w
			}
			if x.Now().Sub(w.since) >= stable {
				done++
			} else {
				problems[key] = fmt.Sprintf("waiting for %s of stable running", stable)
			}
		}
		if done == len(targets) {
			// Non-targets that ran before must still run.
			for key, b := range before {
				if _, isTarget := targets[key]; isTarget || b.state != "running" {
					continue
				}
				if c, ok := now[key]; !ok || c.state != "running" {
					for k := range targets {
						setOutcome(k, protocol.OutcomeUpdated, "")
					}
					return now, fail(protocol.FailVerify, "%s was running before the update and is not running now", key)
				}
			}
			for k := range targets {
				setOutcome(k, protocol.OutcomeUpdated, "")
			}
			return now, nil
		}
		if !x.Now().Before(deadline) || time.Since(realStart) > timeout {
			var parts []string
			for k, p := range problems {
				setOutcome(k, protocol.OutcomeFailed, p)
				parts = append(parts, k+": "+p)
			}
			for k := range targets {
				if _, bad := problems[k]; !bad {
					setOutcome(k, protocol.OutcomeUpdated, "")
				}
			}
			return now, fail(protocol.FailVerify, "not verified after %s: %s", timeout, strings.Join(parts, "; "))
		}
		select {
		case <-ctx.Done():
			return now, fail(protocol.FailInterrupted, "the agent is stopping")
		case <-time.After(x.VerifyPoll):
		}
	}
}
