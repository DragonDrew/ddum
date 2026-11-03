package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Backoff and interval limits.
const (
	MaxBackoff          = 60 * time.Second
	RemovedPollInterval = 300 * time.Second
	// HeartbeatFile in DATA_DIR holds the unix time by which the loop must
	// run again; `ddum-agent healthcheck` reads it.
	HeartbeatFile = "heartbeat"
	// heartbeatGrace is added to each expected wake-up so a slow poll is not
	// reported unhealthy.
	heartbeatGrace = 60 * time.Second
)

// Collector produces inventory snapshots.
type Collector interface {
	Collect(ctx context.Context) (discovery.Snapshot, error)
}

// Operations is the agent's executor as the poll loop sees it.
type Operations interface {
	Active() []protocol.ActiveOperation
	Handle(cmds []protocol.Command)
	Pending() map[string][]protocol.Event
	Acked(id string, seq int)
	TakeInventoryRequest() bool
}

// Loop is the agent's poll loop.
type Loop struct {
	HTTP              *HTTP
	Collector         Collector
	Log               *slog.Logger
	AgentVersion      string
	HostName          string
	PollInterval      time.Duration
	InventoryInterval time.Duration
	Capabilities      []string
	DataDir           string // heartbeat location; empty disables it
	Now               func() time.Time
	Jitter            func(time.Duration) time.Duration // nil: ±10 %
	// Ops executes operations; nil means none (commands are ignored).
	Ops Operations
	// Wake makes Run start the next step early (new events to send).
	Wake chan struct{}

	mu     sync.Mutex
	stacks []protocol.Stack

	failures      int
	lastInventory time.Time
	sentHash      string
	needInventory bool
	started       bool
}

// Run polls until ctx is cancelled.
func (l *Loop) Run(ctx context.Context) error {
	for {
		wait := l.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		l.writeHeartbeat(wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		case <-l.Wake:
			t.Stop()
		}
	}
}

// Step runs one iteration: collect, poll, submit inventory if needed. It
// returns how long to wait before the next iteration.
func (l *Loop) Step(ctx context.Context) time.Duration {
	if !l.started {
		l.started = true
		l.needInventory = true // always submit at startup
	}
	snap, err := l.Collector.Collect(ctx)
	if err != nil {
		l.Log.Warn("docker discovery failed; reporting provider error", "err", err)
	} else {
		l.mu.Lock()
		l.stacks = snap.Stacks
		l.mu.Unlock()
	}
	active := []protocol.ActiveOperation{}
	if l.Ops != nil {
		active = l.Ops.Active()
	}

	req := protocol.PollRequest{
		ProtocolVersion:     protocol.Version,
		AgentID:             l.HTTP.AgentID,
		AgentVersion:        l.AgentVersion,
		HostName:            l.HostName,
		PollIntervalSeconds: int(l.PollInterval / time.Second),
		Capabilities:        l.Capabilities,
		Docker:              snap.Docker,
		InventoryHash:       snap.Hash,
		ActiveOperations:    active,
	}
	var resp protocol.PollResponse
	if err := l.HTTP.Do(ctx, http.MethodPost, protocol.PathPoll, req, &resp); err != nil {
		return l.failed("poll", err)
	}

	if resp.InventoryRequested {
		l.needInventory = true
	}
	if l.InventoryInterval > 0 && l.Now().Sub(l.lastInventory) >= l.InventoryInterval {
		l.needInventory = true
	}
	if l.Ops != nil {
		l.Ops.Handle(resp.Commands)
		if err := l.flushEvents(ctx); err != nil {
			return l.failed("events", err)
		}
		if l.Ops.TakeInventoryRequest() {
			l.needInventory = true // after a deployment
			// snap may predate the deployment. Sent now, after the bot has
			// recorded its result, it would look like the container drifted back.
			if fresh, err := l.Collector.Collect(ctx); err == nil {
				snap = fresh
				l.mu.Lock()
				l.stacks = snap.Stacks
				l.mu.Unlock()
			}
		}
	} else if len(resp.Commands) > 0 {
		l.Log.Warn("ignoring commands: no executor", "count", len(resp.Commands))
	}
	if l.needInventory {
		if err := l.sendInventory(ctx, snap); err != nil {
			return l.failed("inventory", err)
		}
	}
	l.failures = 0
	return l.jitter(l.PollInterval)
}

func (l *Loop) sendInventory(ctx context.Context, snap discovery.Snapshot) error {
	inv := protocol.Inventory{
		ProtocolVersion: protocol.Version,
		AgentID:         l.HTTP.AgentID,
		CollectedAt:     snap.At.UTC().Format(time.RFC3339),
		InventoryHash:   snap.Hash,
		Providers:       snap.Providers,
		Stacks:          snap.Stacks,
	}
	if err := l.HTTP.Do(ctx, http.MethodPut, protocol.PathInventory, inv, nil); err != nil {
		return err
	}
	l.needInventory = false
	l.lastInventory = l.Now()
	if snap.Hash != l.sentHash {
		l.Log.Info("inventory submitted", "stacks", len(snap.Stacks), "hash", snap.Hash)
	}
	l.sentHash = snap.Hash
	return nil
}

// failed logs a request failure and returns the wait before the next try.
func (l *Loop) failed(what string, err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Body.Code {
		case protocol.CodeAgentRemoved:
			// Keep polling slowly until reconfigured.
			l.Log.Error("this agent has been removed in Discord; polling at the maximum interval", "request", what)
			return RemovedPollInterval
		case protocol.CodeAgentKeyChanged:
			l.Log.Error("the bot has a different key pinned for this agent; accept the new key in Discord", "request", what)
			return MaxBackoff
		case protocol.CodeClockSkew:
			l.Log.Error("bot rejected the request: this host's clock is wrong; fix NTP on this host",
				"request", what, "server_time", ae.Body.ServerTime, "local_time", l.Now().UTC().Format(time.RFC3339))
		case protocol.CodeProtocolUnsupported:
			l.Log.Error("bot does not support this agent's protocol version; upgrade the agent or bot",
				"request", what, "agent_protocol", protocol.Version, "bot_supports", ae.Body.Supported)
			return RemovedPollInterval
		}
		if ae.Status == http.StatusTooManyRequests && ae.RetryAfter > 0 {
			wait := min(ae.RetryAfter, RemovedPollInterval)
			l.Log.Warn("bot asked the agent to slow down", "retry_after", ae.RetryAfter, "waiting", wait)
			return wait
		}
	}
	l.failures++
	wait := Backoff(l.PollInterval, l.failures)
	l.Log.Warn("request to bot failed", "request", what, "err", err, "retry_in", wait.String())
	return wait
}

// Backoff is the wait after n consecutive failures (n ≥ 1): the poll interval
// doubled per failure, capped at 60 s — or at the poll interval itself when
// that is longer, so a failure never makes the agent poll faster.
func Backoff(interval time.Duration, n int) time.Duration {
	limit := max(MaxBackoff, interval)
	d := interval
	for i := 0; i < n && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func (l *Loop) jitter(d time.Duration) time.Duration {
	if l.Jitter != nil {
		return l.Jitter(d)
	}
	// ±10 %.
	f := 0.9 + rand.Float64()*0.2
	return time.Duration(float64(d) * f)
}

func (l *Loop) writeHeartbeat(wait time.Duration) {
	if l.DataDir == "" {
		return
	}
	deadline := l.Now().Add(wait + heartbeatGrace).Unix()
	p := filepath.Join(l.DataDir, HeartbeatFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(deadline, 10)+"\n"), 0o640); err == nil {
		err = os.Rename(tmp, p)
		if err != nil {
			l.Log.Warn("write heartbeat", "err", err)
		}
	}
}

// CheckHeartbeat returns nil if the loop's last heartbeat in dataDir has not
// expired at now.
func CheckHeartbeat(dataDir string, now time.Time) error {
	b, err := os.ReadFile(filepath.Join(dataDir, HeartbeatFile))
	if err != nil {
		return fmt.Errorf("no heartbeat yet: %w", err)
	}
	deadline, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fmt.Errorf("corrupt heartbeat file: %w", err)
	}
	if now.Unix() > deadline {
		return fmt.Errorf("poll loop stalled: last heartbeat expired %s ago", now.Sub(time.Unix(deadline, 0)).Round(time.Second))
	}
	return nil
}

// LatestStacks returns the stacks of the most recent successful discovery,
// which the executor validates commands against.
func (l *Loop) LatestStacks() []protocol.Stack {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stacks
}

// flushEvents sends every unacknowledged event and records the bot's acks.
// Unacknowledged events stay in the journal and are re-sent next time
// . An operation the bot no longer knows (404) is dropped.
func (l *Loop) flushEvents(ctx context.Context) error {
	for id, events := range l.Ops.Pending() {
		var resp protocol.EventsResponse
		err := l.HTTP.Do(ctx, http.MethodPost, protocol.PathEvents(id), protocol.EventsRequest{Events: events}, &resp)
		var ae *APIError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
			l.Log.Warn("bot does not know this operation; dropping its events", "operation_id", id)
			l.Ops.Acked(id, events[len(events)-1].Seq)
		case err != nil:
			return err
		default:
			l.Ops.Acked(id, resp.AckedSeq)
		}
	}
	return nil
}
