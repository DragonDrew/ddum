package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/protocol"
)

var (
	key     = []byte("0123456789abcdef0123456789abcdef")
	agentID = "0195f2c4-0000-7000-8000-000000000001"
	t0      = time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
)

// fakeBot answers like the bot: it signs every response, unless unsigned
// is set. handle returns status and body for each request.
type fakeBot struct {
	mu       sync.Mutex
	unsigned bool
	handle   func(path string, body []byte) (int, any)
	calls    []string
	once     sync.Once
	botKey   ed25519.PrivateKey
}

func (f *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	unsigned := f.unsigned
	f.mu.Unlock()
	status, v := f.handle(r.URL.Path, body)
	out, _ := json.Marshal(v)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "17")
	}
	if !unsigned {
		f.once.Do(func() { f.botKey, _ = protocol.NewKey() })
		protocol.SignHTTPResponseV2(w.Header(), f.botKey, key, r.Header.Get(protocol.HeaderAgentSignature), status, out, time.Now())
	}
	w.WriteHeader(status)
	w.Write(out)
}

type staticCollector struct{ snap discovery.Snapshot }

func (s staticCollector) Collect(context.Context) (discovery.Snapshot, error) { return s.snap, nil }

func newLoop(t *testing.T, bot *fakeBot) (*Loop, *time.Time) {
	t.Helper()
	srv := httptest.NewServer(bot)
	t.Cleanup(srv.Close)
	now := t0
	l := &Loop{
		HTTP:              &HTTP{BaseURL: srv.URL, Key: key, AgentID: agentID, Client: srv.Client(), Now: func() time.Time { return now }},
		Collector:         staticCollector{discovery.Snapshot{Hash: "sha256:abc", At: t0, Stacks: []protocol.Stack{}}},
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		HostName:          "host-3",
		PollInterval:      30 * time.Second,
		InventoryInterval: 300 * time.Second,
		Now:               func() time.Time { return now },
		Jitter:            func(d time.Duration) time.Duration { return d },
	}
	return l, &now
}

func okPoll(requested bool) func(string, []byte) (int, any) {
	return func(path string, _ []byte) (int, any) {
		if path == protocol.PathPoll {
			return 200, protocol.PollResponse{InventoryRequested: requested}
		}
		return 200, map[string]any{}
	}
}

func TestInventorySubmissionSchedule(t *testing.T) {
	bot := &fakeBot{handle: okPoll(false)}
	l, now := newLoop(t, bot)

	if w := l.Step(context.Background()); w != 30*time.Second {
		t.Fatalf("wait = %s", w)
	}
	// Startup always submits inventory.
	if got := bot.calls; len(got) != 2 || got[1] != "PUT "+protocol.PathInventory {
		t.Fatalf("startup calls = %v, want poll then inventory", got)
	}
	// Not requested, interval not elapsed: poll only.
	*now = now.Add(30 * time.Second)
	l.Step(context.Background())
	if len(bot.calls) != 3 {
		t.Fatalf("calls = %v, want one more poll only", bot.calls)
	}
	// INVENTORY_INTERVAL elapsed: submit again.
	*now = now.Add(300 * time.Second)
	l.Step(context.Background())
	if len(bot.calls) != 5 {
		t.Fatalf("calls = %v, want poll + inventory after the interval", bot.calls)
	}
	// Requested by the bot: submit.
	bot.handle = okPoll(true)
	l.Step(context.Background())
	if len(bot.calls) != 7 {
		t.Fatalf("calls = %v, want poll + inventory when requested", bot.calls)
	}
}

func TestUnsignedResponseIsAFailure(t *testing.T) {
	bot := &fakeBot{unsigned: true, handle: okPoll(true)}
	l, _ := newLoop(t, bot)
	if w := l.Step(context.Background()); w != 60*time.Second {
		t.Fatalf("wait = %s, want backoff 60s", w)
	}
	if len(bot.calls) != 1 {
		t.Fatalf("an unsigned poll response must not trigger inventory: %v", bot.calls)
	}
	err := l.HTTP.Do(context.Background(), "POST", protocol.PathPoll, struct{}{}, nil)
	if !errors.Is(err, ErrUnsignedResponse) {
		t.Fatalf("err = %v, want ErrUnsignedResponse", err)
	}
}

func TestResponseSignedWithOtherKeyRejected(t *testing.T) {
	bot := &fakeBot{handle: okPoll(false)}
	l, _ := newLoop(t, bot)
	l.HTTP.Key = []byte("ffffffffffffffffffffffffffffffff")
	err := l.HTTP.Do(context.Background(), "POST", protocol.PathPoll, struct{}{}, nil)
	if !errors.Is(err, ErrUnsignedResponse) {
		t.Fatalf("err = %v, want ErrUnsignedResponse", err)
	}
}

func TestErrorResponses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		want   time.Duration
	}{
		{"removed", 403, protocol.CodeAgentRemoved, RemovedPollInterval},
		{"protocol", 426, protocol.CodeProtocolUnsupported, RemovedPollInterval},
		{"rate limited", 429, protocol.CodeTooManyRequests, 17 * time.Second},
		{"clock skew backs off", 401, protocol.CodeClockSkew, 60 * time.Second},
		{"server error backs off", 500, protocol.CodeInternal, 60 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bot := &fakeBot{handle: func(string, []byte) (int, any) {
				return c.status, protocol.ErrorResponse{Error: protocol.ErrorBody{Code: c.code}}
			}}
			l, _ := newLoop(t, bot)
			if w := l.Step(context.Background()); w != c.want {
				t.Fatalf("wait = %s, want %s", w, c.want)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		interval time.Duration
		n        int
		want     time.Duration
	}{
		{3 * time.Second, 1, 6 * time.Second},
		{3 * time.Second, 2, 12 * time.Second},
		{3 * time.Second, 5, 60 * time.Second},
		{3 * time.Second, 50, 60 * time.Second},
		{30 * time.Second, 1, 60 * time.Second},
		{300 * time.Second, 3, 300 * time.Second}, // never faster than the interval
	}
	for _, c := range cases {
		if got := Backoff(c.interval, c.n); got != c.want {
			t.Errorf("Backoff(%s, %d) = %s, want %s", c.interval, c.n, got, c.want)
		}
	}
}

func TestBackoffResetsOnSuccess(t *testing.T) {
	fail := true
	bot := &fakeBot{handle: func(p string, b []byte) (int, any) {
		if fail {
			return 500, protocol.ErrorResponse{Error: protocol.ErrorBody{Code: "internal"}}
		}
		return okPoll(false)(p, b)
	}}
	l, _ := newLoop(t, bot)
	l.PollInterval = 3 * time.Second
	l.Step(context.Background())
	if w := l.Step(context.Background()); w != 12*time.Second {
		t.Fatalf("second failure wait = %s, want 12s", w)
	}
	fail = false
	if w := l.Step(context.Background()); w != 3*time.Second {
		t.Fatalf("after success wait = %s, want the poll interval", w)
	}
}

func TestHeartbeat(t *testing.T) {
	dir := t.TempDir()
	if err := CheckHeartbeat(dir, t0); err == nil {
		t.Fatal("missing heartbeat must be unhealthy")
	}
	l := &Loop{DataDir: dir, Now: func() time.Time { return t0 }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	l.writeHeartbeat(30 * time.Second)
	if err := CheckHeartbeat(dir, t0.Add(89*time.Second)); err != nil {
		t.Fatalf("fresh heartbeat unhealthy: %v", err)
	}
	if err := CheckHeartbeat(dir, t0.Add(91*time.Second)); err == nil {
		t.Fatal("expired heartbeat must be unhealthy")
	}
}
