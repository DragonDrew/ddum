package executor

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

func TestRunnerEnvKeepsTheAgentsOwnVariablesFromStacks(t *testing.T) {
	got := RunnerEnv([]string{"PATH=/bin", "HOME=/root", "HOSTNAME=abc", "TZ=Australia/Sydney", "LOG_LEVEL=debug",
		"AGENT_SHARED_KEY=secret", "PORTAINER_TOKEN=secret", "GHCR_TOKEN=secret", "DOCKER_HOST=unix:///x",
		"STACK_ENV_HOME=/home/me", "STACK_ENV_PUID=1000"})
	want := []string{"PATH=/bin", "DOCKER_HOST=unix:///x", "HOME=/root", "DOCKER_CONFIG=/root/.docker", "HOME=/home/me", "PUID=1000"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := RunnerEnv([]string{"HOME=/root", "DOCKER_CONFIG=/tmp/login"}); !slices.Equal(got, []string{"DOCKER_CONFIG=/tmp/login", "HOME=/root"}) {
		t.Fatalf("an explicit DOCKER_CONFIG must stay: %v", got)
	}
}

// labelRunning records what Compose recorded when the owner started the stack.
func (h *harness) labelRunning(hash string, mounts []docker.Mount, env []string) {
	h.engine.Update(func(l []docker.Container, imgs map[string]docker.Image) ([]docker.Container, map[string]docker.Image) {
		for i := range l {
			l[i].Labels[labelComposeConfigHash] = hash
			l[i].Mounts, l[i].Env = mounts, env
		}
		return l, imgs
	})
}

// ran reports whether a docker command with this argument was run.
func (h *harness) ran(arg string) bool {
	for _, c := range h.runner.calls {
		if slices.Contains(c, arg) {
			return true
		}
	}
	return false
}

func TestPreflightPassesWhenTheHashMatches(t *testing.T) {
	h := newHarness(t, "services:\n  web:\n    image: nginx:1.27\n", "")
	h.labelRunning("abc", nil, nil)
	h.runner.hashes = map[string]string{"web": "abc"}
	if ev := last(h.run(command(ids.V7{}.New(), webChange("1.28", dg("b"))))); ev.Type != protocol.EventCompleted {
		t.Fatalf("%+v", ev)
	}
}

func TestPreflightRefusesAMountThatWouldMove(t *testing.T) {
	const compose = "services:\n  web:\n    image: nginx:1.27\n"
	h := newHarness(t, compose, "")
	h.labelRunning("owner", []docker.Mount{{Type: "bind", Source: "/home/me/data", Destination: "/data"}}, []string{"PUID=1000", "SECRET=old"})
	h.runner.hashes = map[string]string{"web": "agent"}
	h.runner.rendered = `{"services":{"web":{"environment":{"PUID":"","SECRET":"new"},"volumes":[{"type":"bind","source":"/root/data","target":"/data"}]}}}`
	ev := last(h.run(command(ids.V7{}.New(), webChange("1.28", dg("b")))))
	if ev.Code != protocol.FailIneligible {
		t.Fatalf("%+v", ev)
	}
	for _, want := range []string{"/data would be mounted from /root/data instead of /home/me/data", "PUID, SECRET", "STACK_ENV_"} {
		if !strings.Contains(ev.Message, want) {
			t.Errorf("message lacks %q: %s", want, ev.Message)
		}
	}
	if strings.Contains(ev.Message, "new") || strings.Contains(ev.Message, "old") {
		t.Errorf("a variable's value reached the message: %s", ev.Message)
	}
	data, _ := os.ReadFile(h.dir + "/compose.yaml")
	if string(data) != compose {
		t.Errorf("the file changed:\n%s", data)
	}
	if h.ran("up") {
		t.Error("compose up ran")
	}
	if h.ran("pull") {
		t.Error("the pull ran before the check")
	}
}

// A hash from another Compose version says nothing about mounts or variables.
func TestPreflightOnlyWarnsWhenNothingThatMattersDiffers(t *testing.T) {
	h := newHarness(t, "services:\n  web:\n    image: nginx:1.27\n", "")
	h.labelRunning("old-compose", []docker.Mount{{Type: "bind", Source: "/srv/data", Destination: "/data"}}, []string{"PUID=1000", "PATH=/bin"})
	h.runner.hashes = map[string]string{"web": "new-compose"}
	h.runner.rendered = `WARN[0000] something
{"services":{"web":{"environment":{"PUID":"1000","PASSED":null},"volumes":[{"type":"bind","source":"/srv/data","target":"/data"},{"type":"volume","source":"db","target":"/db"}]}}}`
	ev := last(h.run(command(ids.V7{}.New(), webChange("1.28", dg("b")))))
	if ev.Type != protocol.EventCompleted {
		t.Fatalf("%+v", ev)
	}
	if ev.Result == nil || len(ev.Result.Warnings) != 1 || !strings.Contains(ev.Result.Warnings[0], "web") {
		t.Fatalf("no warning: %+v", ev.Result)
	}
}

func TestPreflightRefusesAStackComposeCannotLoad(t *testing.T) {
	const compose = "services:\n  web:\n    image: nginx:1.27\n"
	h := newHarness(t, compose, "")
	h.runner.configErr = errors.New("exit status 1")
	ev := last(h.run(command(ids.V7{}.New(), webChange("1.28", dg("b")))))
	if ev.Code != protocol.FailIneligible || !strings.Contains(ev.Message, "env file /elsewhere/app.env not found") {
		t.Fatalf("%+v", ev)
	}
	if data, _ := os.ReadFile(h.dir + "/compose.yaml"); string(data) != compose {
		t.Errorf("the file changed:\n%s", data)
	}
}

func TestFailureOutputHidesEnvFileValues(t *testing.T) {
	h := newHarness(t, "services:\n  web:\n    image: nginx:1.27\n", "API_KEY=hunter2-long-secret\nSHORT=abc\nWEB_TAG=1.27.3\n")
	h.stacks[0].Compose.WorkingDir = h.dir
	h.runner.upErr = errors.New("exit status 1")
	h.runner.upOutput = "invalid value hunter2-long-secret for abc, tag 1.27.3"
	ev := last(h.run(command(ids.V7{}.New(), webChange("1.28", dg("b")))))
	for _, s := range []string{ev.Message, ev.OutputTail} {
		if strings.Contains(s, "hunter2") || !strings.Contains(s, "***") || !strings.Contains(s, "1.27.3") {
			t.Fatalf("%q", s)
		}
	}
}

func TestFailedComposeUpPutsTheFilesBack(t *testing.T) {
	const compose = "services:\n  web:\n    image: nginx:1.27\n"
	h := newHarness(t, compose, "")
	h.runner.upErr = errors.New("exit status 1")
	ev := last(h.run(command(ids.V7{}.New(), webChange("1.28", dg("b")))))
	if ev.Code != protocol.FailDeploy || !strings.Contains(ev.Message, "put back") || !strings.Contains(ev.Message, "service web: error") {
		t.Fatalf("%+v", ev)
	}
	if data, _ := os.ReadFile(h.dir + "/compose.yaml"); string(data) != compose {
		t.Errorf("the file was not put back:\n%s", data)
	}
}
