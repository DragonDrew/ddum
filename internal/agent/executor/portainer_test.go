package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/agent/portainer"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// fakePortainer keeps one stack and "deploys" it by writing its file and Env
// into the harness directory and running the fake compose for every service.
type fakePortainer struct {
	h       *harness
	stack   portainer.Stack
	content string
	updates int
	gitRuns int
}

func (f *fakePortainer) Stack(context.Context, int) (portainer.Stack, error) { return f.stack, nil }
func (f *fakePortainer) StackFile(context.Context, int) (string, error)      { return f.content, nil }

func (f *fakePortainer) UpdateFileStack(_ context.Context, id int, content string, env []portainer.Pair) error {
	f.updates++
	f.content, f.stack.Env = content, env
	return f.redeploy()
}

func (f *fakePortainer) RedeployGitStack(_ context.Context, s portainer.Stack) error {
	f.gitRuns++
	return f.redeploy()
}

func (f *fakePortainer) redeploy() error {
	os.WriteFile(filepath.Join(f.h.dir, "compose.yaml"), []byte(f.content), 0o644)
	var lines []string
	for _, e := range f.stack.Env {
		lines = append(lines, e.Name+"="+e.Value)
	}
	os.WriteFile(filepath.Join(f.h.dir, ".env"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	_, err := f.h.runner.Run(context.Background(), f.h.dir, "compose", "up", "-d", "--no-deps", "--pull", "never", "web", "db")
	return err
}

// portainerHarness turns the harness stack into Portainer stack 7.
func portainerHarness(t *testing.T, content string, env []portainer.Pair, git bool) (*harness, *fakePortainer) {
	t.Helper()
	h := newHarness(t, content, "")
	fp := &fakePortainer{h: h, content: content, stack: portainer.Stack{ID: 7, Name: "app", Type: portainer.TypeCompose, EndpointID: 2, Env: env}}
	if git {
		fp.stack.GitConfig = &portainer.GitConfig{URL: "https://example.org/app.git", ReferenceName: "refs/heads/main"}
	}
	h.x.Portainer = fp
	h.stacks[0].Provider = protocol.ProviderPortainer
	h.stacks[0].ProviderRef, _ = json.Marshal(map[string]any{"portainer_stack_id": 7})
	fp.redeploy() // make the files match the "Portainer" state
	return h, fp
}

func TestPortainerLiteralTagChangeAndRollback(t *testing.T) {
	env := []portainer.Pair{{Name: "UNRELATED", Value: "keep"}}
	h, fp := portainerHarness(t, literalCompose, env, false)
	op := ids.V7{}.New()
	evs := h.run(command(op, webChange("1.28", dg("b"))))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("deploy: %s %+v", types(evs), last(evs))
	}
	if fp.updates != 1 || !strings.Contains(fp.content, "nginx:1.28   # web server") {
		t.Fatalf("stack file = %q (updates %d)", fp.content, fp.updates)
	}
	if len(fp.stack.Env) != 1 || fp.stack.Env[0].Value != "keep" {
		t.Fatalf("the complete Env list must be sent back: %+v", fp.stack.Env)
	}
	for _, c := range h.runner.calls {
		if c[1] == "compose" && c[0] != h.dir {
			t.Fatal("a Portainer stack must not be deployed with the agent's own compose")
		}
	}
	snap := last(evs).Result.Snapshots[0].SnapshotID
	evs = h.run(rollbackCommand(ids.V7{}.New(), op, snap))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("rollback: %s %+v", types(evs), last(evs))
	}
	if fp.content != literalCompose || fp.updates != 2 {
		t.Fatalf("rollback must restore the stack file through the API: %q", fp.content)
	}
}

// A rollback sends back the stack as it is now, with only the tag put back.
func TestPortainerRollbackKeepsLaterChanges(t *testing.T) {
	content := "services:\n  web:\n    image: nginx:${WEB_TAG}\n  db:\n    image: postgres:16\n"
	h, fp := portainerHarness(t, content, []portainer.Pair{{Name: "WEB_TAG", Value: "1.27"}, {Name: "API_KEY", Value: "original"}}, false)
	op := ids.V7{}.New()
	evs := h.run(command(op, webChange("1.28", dg("b"))))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("deploy: %s %+v", types(evs), last(evs))
	}
	fp.stack.Env[1].Value = "rotated"
	fp.content += "# added later\n"
	evs = h.run(rollbackCommand(ids.V7{}.New(), op, last(evs).Result.Snapshots[0].SnapshotID))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("rollback: %s %+v", types(evs), last(evs))
	}
	if fp.stack.Env[0].Value != "1.27" || fp.stack.Env[1].Value != "rotated" || !strings.Contains(fp.content, "# added later") {
		t.Fatalf("env=%+v content=%q", fp.stack.Env, fp.content)
	}
}

func TestPortainerEnvTagSource(t *testing.T) {
	content := "services:\n  web:\n    image: nginx:${WEB_TAG}\n  db:\n    image: postgres:16\n"
	h, fp := portainerHarness(t, content, []portainer.Pair{{Name: "WEB_TAG", Value: "1.27"}, {Name: "OTHER", Value: "x"}}, false)
	evs := h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))
	if last(evs).Type != protocol.EventCompleted {
		t.Fatalf("deploy: %s %+v", types(evs), last(evs))
	}
	if fp.content != content || fp.stack.Env[0].Value != "1.28" || fp.stack.Env[1].Value != "x" {
		t.Fatalf("only the Env entry may change: content=%q env=%+v", fp.content, fp.stack.Env)
	}
}

func TestPortainerGitStack(t *testing.T) {
	h, fp := portainerHarness(t, literalCompose, nil, true)
	// A same-tag digest update is a plain git redeploy.
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	if last(evs).Type != protocol.EventCompleted || fp.gitRuns != 1 || fp.updates != 0 {
		t.Fatalf("git digest update: %s gitRuns=%d updates=%d %+v", types(evs), fp.gitRuns, fp.updates, last(evs))
	}
	// A tag change cannot be applied to a Git-based stack.
	evs = h.run(command(ids.V7{}.New(), webChange("1.28", dg("c"))))
	if evs[0].Type != protocol.EventRejected || !strings.Contains(evs[0].Message, "git_managed") {
		t.Fatalf("git tag change: %+v", evs)
	}
}

func TestPortainerProviderDisabled(t *testing.T) {
	h, _ := portainerHarness(t, literalCompose, nil, false)
	h.x.Portainer = nil
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	if evs[0].Type != protocol.EventRejected || evs[0].Code != protocol.FailUnsupported {
		t.Fatalf("events = %+v", evs)
	}
}
