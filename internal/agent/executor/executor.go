package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Runner runs the docker CLI with an argument array and no shell, returning combined output.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

// ExecRunner runs the real docker binary.
type ExecRunner struct {
	Binary string // "docker"
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Dir = dir
	cmd.Env = RunnerEnv(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// At the deadline ask docker to stop, which it forwards to the compose
	// plugin. WaitDelay then ends the wait even if a child process still holds
	// the output pipe, which would otherwise block Run for as long as it lives.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	err := cmd.Run()
	return out.Bytes(), err
}

// StackEnvPrefix marks agent variables meant for stacks: STACK_ENV_HOME=/home/me
// reaches Compose as HOME=/home/me.
const StackEnvPrefix = "STACK_ENV_"

// runnerEnvKeep lists what docker itself needs from the agent's environment.
var runnerEnvKeep = map[string]bool{
	"PATH": true, "TMPDIR": true,
	"DOCKER_HOST": true, "DOCKER_CONFIG": true, "DOCKER_API_VERSION": true, "DOCKER_CERT_PATH": true, "DOCKER_TLS_VERIFY": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "no_proxy": true,
}

// RunnerEnv is the environment docker runs with. Compose fills in a stack's
// variables from it, ahead of the stack's own .env, and expands ~ with its
// HOME, so the agent's own environment must not reach it: its secrets would be
// available to stacks, and a variable with the same name as one of a stack's
// would silently change that stack. What a stack needs from outside its
// files comes from STACK_ENV_ variables. HOME stays, so that a stack using ~ is
// caught by the preflight with a clear reason, and not by Compose failing to
// load it. The docker CLI's config stays where it was, even if a stack is given
// its own HOME.
func RunnerEnv(environ []string) []string {
	var out, stack []string
	var home string
	hasConfig := false
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case !ok:
		case k == "HOME":
			home = v
		case runnerEnvKeep[k]:
			hasConfig = hasConfig || k == "DOCKER_CONFIG"
			out = append(out, kv)
		case strings.HasPrefix(k, StackEnvPrefix) && k != StackEnvPrefix:
			stack = append(stack, strings.TrimPrefix(k, StackEnvPrefix)+"="+v)
		}
	}
	if home != "" {
		out = append(out, "HOME="+home)
		if !hasConfig {
			out = append(out, "DOCKER_CONFIG="+home+"/.docker")
		}
	}
	return append(out, stack...) // the last value of a name wins
}

// Executor runs operations one at a time and keeps their events in the
// journal until the bot acknowledges them.
type Executor struct {
	Journal      *Journal
	Engine       docker.Engine
	Runner       Runner
	Files        discovery.FileChecker
	AllowedRoots []string
	DataDir      string
	// Inventory returns the stacks of the latest discovery.
	Inventory func() []protocol.Stack
	Log       *slog.Logger
	Now       func() time.Time
	// Wake asks the poll loop to run soon (to send events and inventory).
	Wake func()
	// VerifyPoll is the verification polling period (default 2 s).
	VerifyPoll time.Duration
	// NewID generates snapshot IDs.
	NewID func() string
	// RetentionCount is SNAPSHOT_RETENTION_COUNT: snapshots kept per stack.
	RetentionCount int
	// Portainer is the Portainer API, when that provider is enabled.
	Portainer PortainerAPI
	// OnPortainerChange is called after the agent changed a Portainer stack
	// (discovery drops its cached copy).
	OnPortainerChange func(stackID int)
	// PullRetryDelays are the waits before each retry of a failed pull
	// (default 5 s, 20 s, 60 s). Registries refuse pulls now and then (rate
	// limits, timeouts); a pull by digest is safe to repeat.
	PullRetryDelays []time.Duration
	// PullTimeout and ComposeTimeout limit one docker pull and one compose up
	// (default 30 and 15 minutes).
	PullTimeout    time.Duration
	ComposeTimeout time.Duration

	// selfRestores are interrupted bot self-updates to restore in Run: the
	// inventory they need does not exist yet when Recover finds them.
	selfRestores []*Entry

	queue         chan string
	current       atomic.Value // string: running operation ID
	needInventory atomic.Bool
	startOnce     sync.Once
}

func (x *Executor) init() {
	x.startOnce.Do(func() {
		x.queue = make(chan string, 16)
		x.current.Store("")
		if x.VerifyPoll <= 0 {
			x.VerifyPoll = 2 * time.Second
		}
	})
}

// Recover turns every operation that was running when the agent stopped into
// interrupted. Call it once at startup, before the first poll.
func (x *Executor) Recover() {
	x.init()
	entries, err := x.Journal.List()
	if err != nil {
		x.Log.Error("read journal", "err", err)
		return
	}
	for _, e := range entries {
		if e.State != JournalRunning {
			continue
		}
		x.Log.Warn("operation was interrupted by an agent restart", "operation_id", e.OperationID)
		var p struct {
			SelfUpdate bool `json:"self_update"`
		}
		json.Unmarshal(e.Payload, &p)
		if p.SelfUpdate && len(e.SnapshotIDs) > 0 {
			// The bot may be down, and only the agent can bring it back.
			x.selfRestores = append(x.selfRestores, e)
			continue
		}
		x.emit(e.OperationID, interruptedEvent(e))
	}
	x.Journal.Prune(x.Now())
	x.dropFileCopies()
}

// interruptedEvent reports an operation cut short by a restart, with its
// snapshots so the bot can offer or start a rollback.
func interruptedEvent(e *Entry) protocol.Event {
	ev := protocol.Event{Type: protocol.EventInterrupted, Code: protocol.FailInterrupted,
		Message: "the agent restarted while this operation was running; check the stack before retrying"}
	if len(e.SnapshotIDs) > 0 {
		var p struct {
			Targets []struct {
				StackID string `json:"stack_id"`
			} `json:"targets"`
		}
		json.Unmarshal(e.Payload, &p)
		ev.Result = &protocol.Result{}
		for i, id := range e.SnapshotIDs {
			ref := protocol.SnapshotRef{SnapshotID: id, Created: true}
			if i < len(p.Targets) {
				ref.StackID = p.Targets[i].StackID
			}
			ev.Result.Snapshots = append(ev.Result.Snapshots, ref)
		}
	}
	return ev
}

// restoreSelfUpdates puts the bot back after a self-update that a restart cut
// short: if its new image does not start, nothing else would. It waits
// for the first discovery, then reports each as interrupted.
func (x *Executor) restoreSelfUpdates(ctx context.Context) {
	for _, e := range x.selfRestores {
		for len(x.Inventory()) == 0 {
			select {
			case <-ctx.Done():
				return // still in the journal as running, so the next start retries
			case <-time.After(x.VerifyPoll):
			}
		}
		x.current.Store(e.OperationID)
		ev := interruptedEvent(e)
		var p protocol.DeployPayload
		json.Unmarshal(e.Payload, &p)
		progress := func(step, msg string) {
			x.Log.Info("restoring the bot", "operation_id", e.OperationID, "step", step, "message", msg)
		}
		if err := x.rollbackLocally(ctx, e.SnapshotIDs[0], p.Verify, progress); err != nil {
			x.Log.Error("could not restore the bot", "operation_id", e.OperationID, "err", err)
			ev.Message += "; the agent's local rollback ALSO failed: " + err.Error()
		} else {
			ev.Result.RolledBack = true
			ev.Message += "; the agent rolled the bot back to its previous image"
		}
		x.emit(e.OperationID, ev)
		x.current.Store("")
		x.needInventory.Store(true)
	}
	x.selfRestores = nil
}

// Run executes queued operations until ctx ends.
func (x *Executor) Run(ctx context.Context) {
	x.init()
	x.restoreSelfUpdates(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-x.queue:
			x.current.Store(id)
			x.execute(ctx, id)
			x.current.Store("")
			x.needInventory.Store(true) // after any deployment
			x.Journal.Prune(x.Now())
			x.wake()
		}
	}
}

// Handle takes the commands of a poll response. A command already in the
// journal is never executed again; its unacknowledged events are re-sent
// by the flush.
func (x *Executor) Handle(cmds []protocol.Command) {
	x.init()
	for _, cmd := range cmds {
		if _, known, err := x.Journal.Get(cmd.OperationID); err != nil || known {
			if err != nil {
				x.Log.Error("journal lookup", "operation_id", cmd.OperationID, "err", err)
			}
			continue
		}
		e := &Entry{OperationID: cmd.OperationID, Type: cmd.Type, State: JournalRunning, NextSeq: 1,
			Payload: cmd.Payload, CreatedAt: x.Now()}
		if err := x.Journal.Create(e); err != nil {
			x.Log.Error("journal create", "operation_id", cmd.OperationID, "err", err)
			continue
		}
		log := x.Log.With("operation_id", cmd.OperationID)
		if code, msg := x.precheck(cmd); code != "" {
			log.Warn("operation rejected", "code", code, "reason", msg)
			x.needInventory.Store(true)
			x.emit(cmd.OperationID, protocol.Event{Type: protocol.EventRejected, Code: code, Message: msg})
			continue
		}
		log.Info("operation accepted", "type", cmd.Type)
		x.emit(cmd.OperationID, protocol.Event{Type: protocol.EventAccepted})
		select {
		case x.queue <- cmd.OperationID:
		default:
			x.emit(cmd.OperationID, protocol.Event{Type: protocol.EventFailed, Code: protocol.FailInternal, Message: "agent queue full"})
		}
	}
}

// precheck rejects commands the agent will not run.
func (x *Executor) precheck(cmd protocol.Command) (code, msg string) {
	if cmd.Type == protocol.CommandRollback {
		var p protocol.RollbackPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return protocol.FailUnsafe, "payload does not parse: " + err.Error()
		}
		if p.OperationID != cmd.OperationID {
			return protocol.FailUnsafe, "payload operation ID does not match the command"
		}
		if exp, err := time.Parse(time.RFC3339, p.ExpiresAt); err == nil && x.Now().After(exp) {
			return protocol.FailIneligible, "the operation expired before it reached the agent"
		}
		if _, err := x.planRollback(p); err != nil {
			var se *stepError
			if errors.As(err, &se) {
				return se.code, se.msg
			}
			return protocol.FailUnsafe, err.Error()
		}
		return "", ""
	}
	if cmd.Type != protocol.CommandDeploy {
		return protocol.FailUnsupported, "unknown operation type " + cmd.Type
	}
	var p protocol.DeployPayload
	if err := json.Unmarshal(cmd.Payload, &p); err != nil {
		return protocol.FailUnsafe, "payload does not parse: " + err.Error()
	}
	if p.OperationID != cmd.OperationID {
		return protocol.FailUnsafe, "payload operation ID does not match the command"
	}
	if exp, err := time.Parse(time.RFC3339, p.ExpiresAt); err == nil && x.Now().After(exp) {
		return protocol.FailIneligible, "the operation expired before it reached the agent"
	}
	if _, err := x.plan(p); err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return se.code, se.msg
		}
		return protocol.FailUnsafe, err.Error()
	}
	return "", ""
}

// Active returns the operations to report in the poll request.
func (x *Executor) Active() []protocol.ActiveOperation {
	x.init()
	if id, _ := x.current.Load().(string); id != "" {
		return []protocol.ActiveOperation{{OperationID: id, State: protocol.OpRunning}}
	}
	return []protocol.ActiveOperation{}
}

// Pending returns unacknowledged events per operation.
func (x *Executor) Pending() map[string][]protocol.Event {
	out := map[string][]protocol.Event{}
	entries, err := x.Journal.List()
	if err != nil {
		return out
	}
	for _, e := range entries {
		if len(e.Unacked) > 0 {
			out[e.OperationID] = append([]protocol.Event(nil), e.Unacked...)
		}
	}
	return out
}

// Acked records the bot's acknowledgement.
func (x *Executor) Acked(id string, seq int) {
	if err := x.Journal.Ack(id, seq); err != nil {
		x.Log.Error("journal ack", "operation_id", id, "err", err)
	}
}

// TakeInventoryRequest reports (once) that an operation finished and a fresh
// inventory should be sent.
func (x *Executor) TakeInventoryRequest() bool {
	return x.needInventory.Swap(false)
}

func (x *Executor) emit(id string, events ...protocol.Event) {
	if _, err := x.Journal.Append(id, x.Now(), events...); err != nil {
		x.Log.Error("journal append", "operation_id", id, "err", err)
	}
	x.wake()
}

func (x *Executor) wake() {
	if x.Wake != nil {
		x.Wake()
	}
}

// stepError is a failure with its protocol code.
type stepError struct {
	code, msg, output string
}

func (e *stepError) Error() string { return e.code + ": " + e.msg }

func fail(code, format string, a ...any) *stepError {
	return &stepError{code: code, msg: fmt.Sprintf(format, a...)}
}

func (x *Executor) execute(ctx context.Context, id string) {
	log := x.Log.With("operation_id", id)
	e, ok, err := x.Journal.Get(id)
	if err != nil || !ok {
		log.Error("operation vanished from the journal", "err", err)
		return
	}
	progress := func(step, msg string) {
		log.Info("operation step", "step", step, "message", msg)
		x.emit(id, protocol.Event{Type: protocol.EventProgress, Step: step, Message: msg})
	}
	var result *protocol.Result
	var selfUpdate bool
	var verify protocol.VerifySettings
	switch e.Type {
	case protocol.CommandRollback:
		var p protocol.RollbackPayload
		if err = json.Unmarshal(e.Payload, &p); err == nil {
			result, err = x.rollback(ctx, p, progress)
		}
	default:
		var p protocol.DeployPayload
		if err = json.Unmarshal(e.Payload, &p); err == nil {
			selfUpdate, verify = p.SelfUpdate, p.Verify
			result, err = x.deploy(ctx, p, progress)
		}
	}
	if err != nil {
		var se *stepError
		if !errors.As(err, &se) {
			se = &stepError{code: protocol.FailInternal, msg: err.Error()}
		}
		values := x.sensitiveValues(ctx)
		se.msg, se.output = maskValues(se.msg, values), maskValues(se.output, values)
		if selfUpdate && result != nil && len(result.Snapshots) > 0 && changedSomething(se.code) {
			// The bot may be down: restore it now, without waiting.
			log.Warn("bot self-update failed; restoring the previous bot", "code", se.code)
			if rerr := x.rollbackLocally(ctx, result.Snapshots[0].SnapshotID, verify, progress); rerr != nil {
				se.msg += "; the agent's local rollback ALSO failed: " + rerr.Error()
			} else {
				result.RolledBack = true
				se.msg += "; the agent rolled the bot back to its previous image"
			}
		}
		log.Error("operation failed", "code", se.code, "reason", se.msg)
		ev := protocol.Event{Type: protocol.EventFailed, Code: se.code, Message: se.msg, Result: result}
		if se.output != "" {
			ev.OutputTail = tail(se.output, protocol.MaxOutputTail)
		}
		x.needInventory.Store(true) // before the event, so the poll that sends it collects afresh
		x.emit(id, ev)
		return
	}
	log.Info("operation completed")
	x.needInventory.Store(true) // before the event, so the poll that sends it collects afresh
	x.emit(id, protocol.Event{Type: protocol.EventCompleted, Result: result})
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Busy reports whether an operation is executing.
func (x *Executor) Busy() bool {
	x.init()
	id, _ := x.current.Load().(string)
	return id != ""
}

// changedSomething reports whether a failure can have left the stack
// changed: pull and digest failures happen before any file or container
// changes.
func changedSomething(code string) bool {
	switch code {
	case protocol.FailDeploy, protocol.FailVerify, protocol.FailInterrupted, protocol.FailInternal:
		return true
	}
	return false
}
