package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/client"
	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/bot/httpapi"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

var (
	key    = []byte("0123456789abcdef0123456789abcdef")
	agentA = "0195f2c4-0000-7000-8000-00000000000a"
	agentB = "0195f2c4-0000-7000-8000-00000000000b"
	owner  = store.Actor{Kind: "user", DiscordID: "100000000000000010", Name: "owner"}
	quiet  = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// clock is a settable time source shared by the bot and the agents.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.Set(time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC))
	return c
}
func (c *clock) Now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) Set(t time.Time)     { c.ns.Store(t.UnixNano()) }
func (c *clock) Add(d time.Duration) { c.Set(c.Now().Add(d)) }

// notices records Notifier calls.
type notices struct {
	mu         sync.Mutex
	registered []store.Host
	online     []store.Host
	skew       []string
	finished   []store.Operation
	keyChanged []string
}

func (n *notices) AgentKeyChanged(_ context.Context, _ store.Host, fingerprint string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.keyChanged = append(n.keyChanged, fingerprint)
}

func (n *notices) HostRegistered(_ context.Context, h store.Host, _ bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.registered = append(n.registered, h)
}
func (n *notices) HostOnline(_ context.Context, h store.Host) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.online = append(n.online, h)
}
func (n *notices) ClockSkew(_ context.Context, agentID, _ string, _ time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.skew = append(n.skew, agentID)
}

type rig struct {
	t      *testing.T
	dbPath string
	db     *store.Store
	srv    *httptest.Server
	clock  *clock
	notes  *notices
	keys   map[string]ed25519.PrivateKey // rawPoll's agents
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, dbPath: filepath.Join(t.TempDir(), "ddum.db"), clock: newClock(), notes: &notices{}}
	r.start()
	return r
}

// start opens the database and serves the API; restart simulates a bot restart.
func (r *rig) start() {
	db, err := store.Open(context.Background(), r.dbPath, ids.V7{})
	if err != nil {
		r.t.Fatal(err)
	}
	r.db = db
	api := httpapi.New(db, r.notes, key, quiet, r.clock.Now)
	if r.srv == nil {
		r.srv = httptest.NewServer(api.Handler())
		r.t.Cleanup(func() { r.srv.Close(); r.db.Close() })
	} else {
		r.srv.Config.Handler = api.Handler()
	}
}

func (r *rig) restart() {
	r.db.Close()
	r.start()
}

// agent builds a real agent poll loop against the rig, over a fake Docker.
func (r *rig) agent(id, name string, fake *docker.Fake) *client.Loop {
	return &client.Loop{
		HTTP: &client.HTTP{BaseURL: r.srv.URL, Key: key, AgentID: id, Client: r.srv.Client(), Now: r.clock.Now},
		Collector: &discovery.Collector{Engine: fake, Now: r.clock.Now,
			Options: discovery.Options{EnabledProviders: []string{"compose", "docker"}}},
		Log: quiet, AgentVersion: "test", HostName: name,
		PollInterval: 30 * time.Second, InventoryInterval: 300 * time.Second,
		Capabilities: []string{protocol.CapProviderCompose, protocol.CapProviderDocker},
		Now:          r.clock.Now,
		Jitter:       func(d time.Duration) time.Duration { return d },
	}
}

func fakeDocker(containers ...docker.Container) *docker.Fake {
	f := &docker.Fake{EngineInfo: protocol.DockerInfo{EngineVersion: "29.6.0", APIVersion: "1.55", OS: "linux", Arch: "amd64"}}
	imgs := map[string]docker.Image{}
	for _, c := range containers {
		imgs[c.ImageID] = docker.Image{ID: c.ImageID, RepoDigests: []string{"nginx@sha256:aaa"}, OS: "linux", Arch: "amd64"}
	}
	f.Set(containers, imgs)
	return f
}

func composeContainer(dockerID, project, service string) docker.Container {
	return docker.Container{ID: dockerID, Name: project + "-" + service + "-1", ImageRef: "nginx:1.27", ImageID: "sha256:img",
		State: "running", Health: "none", Labels: map[string]string{
			discovery.LabelComposeProject: project, discovery.LabelComposeService: service,
			discovery.LabelComposeContainerNumber: "1",
		}}
}

// rawPoll sends a hand-built, signed poll so tests can control headers.
func (r *rig) rawPoll(agentID string, sigTime time.Time, mutate func(*http.Request)) (*http.Response, []byte) {
	r.t.Helper()
	body, _ := json.Marshal(protocol.PollRequest{ProtocolVersion: protocol.Version, AgentID: agentID, HostName: "raw"})
	req, _ := http.NewRequest("POST", r.srv.URL+protocol.PathPoll, bytes.NewReader(body))
	if r.keys == nil {
		r.keys = map[string]ed25519.PrivateKey{}
	}
	priv, ok := r.keys[agentID]
	if !ok {
		var err error
		if priv, err = protocol.NewKey(); err != nil {
			r.t.Fatal(err)
		}
		r.keys[agentID] = priv
	}
	if _, err := protocol.SignHTTPRequestV2(req, key, priv, agentID, body, sigTime); err != nil {
		r.t.Fatal(err)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(out), protocol.CodeClockSkew) {
		if _, err := protocol.VerifyHTTPResponseV2(resp, "", key, req.Header.Get(protocol.HeaderAgentSignature), out); err != nil {
			r.t.Fatalf("bot response not signed (status %d): %v", resp.StatusCode, err)
		}
	}
	return resp, out
}

func TestTwoAgentsRegisterAsPendingWithinOnePoll(t *testing.T) {
	r := newRig(t)
	a := r.agent(agentA, "host-3", fakeDocker(composeContainer("d1", "app", "web")))
	b := r.agent(agentB, "host-4", fakeDocker())
	if w := a.Step(context.Background()); w != 30*time.Second {
		t.Fatalf("agent A wait = %s (poll failed?)", w)
	}
	if w := b.Step(context.Background()); w != 30*time.Second {
		t.Fatalf("agent B wait = %s (poll failed?)", w)
	}
	hosts, _ := r.db.Hosts(context.Background(), false)
	if len(hosts) != 2 || len(r.notes.registered) != 2 {
		t.Fatalf("hosts=%d notices=%d, want 2 and 2", len(hosts), len(r.notes.registered))
	}
	for _, h := range hosts {
		if h.Approval != "pending" {
			t.Errorf("%s approval = %s, want pending", h.DisplayName, h.Approval)
		}
	}
	// A's inventory was submitted at startup and stored although pending.
	ha, _ := r.db.HostByAgent(context.Background(), agentA)
	if st, _ := r.db.Stacks(context.Background(), store.StackFilter{HostID: ha.ID}); len(st) != 1 {
		t.Fatalf("pending host inventory not stored: %+v", st)
	}

	// Approve: the host is active; the next poll still registers nothing new.
	if _, _, err := r.db.ApproveHost(context.Background(), ha.ID, owner, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r.clock.Add(30 * time.Second)
	a.Step(context.Background())
	h, _ := r.db.Host(context.Background(), ha.ID)
	if h.Approval != "approved" || len(r.notes.registered) != 2 {
		t.Fatalf("after approve: %+v notices=%d", h, len(r.notes.registered))
	}
}

func TestRejectedHostReceives403(t *testing.T) {
	r := newRig(t)
	a := r.agent(agentA, "rogue", fakeDocker())
	a.Step(context.Background())
	h, _ := r.db.HostByAgent(context.Background(), agentA)
	if _, err := r.db.RemoveHost(context.Background(), h.ID, "host.rejected", owner, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if w := a.Step(context.Background()); w != client.RemovedPollInterval {
		t.Fatalf("wait after reject = %s, want %s", w, client.RemovedPollInterval)
	}
	r.keys = map[string]ed25519.PrivateKey{agentA: a.HTTP.AgentKey}
	resp, body := r.rawPoll(agentA, r.clock.Now(), nil)
	if resp.StatusCode != http.StatusForbidden || !bytes.Contains(body, []byte(protocol.CodeAgentRemoved)) {
		t.Fatalf("status=%d body=%s, want 403 agent_removed", resp.StatusCode, body)
	}
}

func TestClockSkewProducesOneNotice(t *testing.T) {
	r := newRig(t)
	skewed := r.clock.Now().Add(-10 * time.Minute)
	for i := 0; i < 3; i++ {
		resp, body := r.rawPoll(agentA, skewed, nil)
		if resp.StatusCode != http.StatusUnauthorized || !bytes.Contains(body, []byte(protocol.CodeClockSkew)) ||
			!bytes.Contains(body, []byte("server_time")) {
			t.Fatalf("status=%d body=%s, want 401 clock_skew with server_time", resp.StatusCode, body)
		}
	}
	if len(r.notes.skew) != 1 {
		t.Fatalf("skew notices = %d, want 1", len(r.notes.skew))
	}
	// A badly signed request with a bad clock is not trusted: no notice.
	r.rawPoll(agentB, skewed, func(req *http.Request) { req.Header.Set(protocol.HeaderSignature, "00") })
	if len(r.notes.skew) != 1 {
		t.Fatal("an invalid signature must never produce a skew notice")
	}
	// After an hour, one more notice.
	r.clock.Add(time.Hour)
	r.rawPoll(agentA, r.clock.Now().Add(-10*time.Minute), nil)
	if len(r.notes.skew) != 2 {
		t.Fatalf("skew notices after an hour = %d, want 2", len(r.notes.skew))
	}
	// Nothing was registered by any of this.
	if hosts, _ := r.db.Hosts(context.Background(), true); len(hosts) != 0 {
		t.Fatalf("skewed requests registered hosts: %+v", hosts)
	}
}

func TestBadSignatureAndProtocol(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		name   string
		mutate func(*http.Request)
		status int
		code   string
	}{
		{"tampered signature", func(q *http.Request) {
			q.Header.Set(protocol.HeaderSignature, "ab"+q.Header.Get(protocol.HeaderSignature)[2:])
		}, 401, protocol.CodeBadSignature},
		{"missing headers", func(q *http.Request) { q.Header.Del(protocol.HeaderTimestamp) }, 401, protocol.CodeBadSignature},
		{"other agent's header", func(q *http.Request) { q.Header.Set(protocol.HeaderAgentID, agentB) }, 401, protocol.CodeBadSignature},
		{"protocol 3", func(q *http.Request) { q.Header.Set(protocol.HeaderProtocol, "3") }, 426, protocol.CodeProtocolUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := r.rawPoll(agentA, r.clock.Now(), c.mutate)
			if resp.StatusCode != c.status || !bytes.Contains(body, []byte(c.code)) {
				t.Fatalf("status=%d body=%s, want %d %s", resp.StatusCode, body, c.status, c.code)
			}
		})
	}
	resp, body := r.rawPoll(agentA, r.clock.Now(), func(q *http.Request) { q.Header.Set(protocol.HeaderProtocol, "3") })
	if !bytes.Contains(body, []byte(`"supported":[1,2]`)) || resp.StatusCode != 426 {
		t.Fatalf("426 body = %s", body)
	}
}

func TestBodyAgentIDMustMatchHeader(t *testing.T) {
	r := newRig(t)
	body, _ := json.Marshal(protocol.PollRequest{ProtocolVersion: protocol.Version, AgentID: agentB})
	req, _ := http.NewRequest("POST", r.srv.URL+protocol.PathPoll, bytes.NewReader(body))
	priv, err := protocol.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.SignHTTPRequestV2(req, key, priv, agentA, body, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestInventoryTooLarge(t *testing.T) {
	r := newRig(t)
	body := bytes.Repeat([]byte(" "), protocol.MaxInventoryBodyBytes+1)
	req, _ := http.NewRequest("PUT", r.srv.URL+protocol.PathInventory, bytes.NewReader(body))
	protocol.SignHTTPRequest(req, key, agentA, body, r.clock.Now())
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestIdentitySurvivesRecreateRelabelAndBotRestart(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	fake := fakeDocker(composeContainer("d1", "app", "web"), composeContainer("d2", "app", "db"))
	a := r.agent(agentA, "host-3", fake)
	a.Step(ctx)
	h, _ := r.db.HostByAgent(ctx, agentA)
	before, _ := r.db.Containers(ctx, store.ContainerFilter{HostID: h.ID})

	// Recreate the web container (new Docker ID).
	web := composeContainer("d1-new", "app", "web")
	fake.Set([]docker.Container{web, composeContainer("d2", "app", "db")}, fake.Images)
	r.clock.Add(30 * time.Second)
	a.Step(ctx) // the hash changed, so the bot requests inventory...
	r.clock.Add(30 * time.Second)
	a.Step(ctx) // ...and the agent submits it on the next step.

	// Bot restart.
	r.restart()
	// Provider relabeling: the stack's files move under Portainer's directory.
	for i, c := range fake.List {
		c.Labels = copyLabels(c.Labels)
		c.Labels[discovery.LabelComposeConfigFiles] = "/data/compose/3/docker-compose.yml"
		c.Labels[discovery.LabelComposeWorkingDir] = "/data/compose/3"
		fake.List[i] = c
	}
	for i := 0; i < 2; i++ {
		r.clock.Add(30 * time.Second)
		a.Step(ctx)
	}

	after, _ := r.db.Containers(ctx, store.ContainerFilter{HostID: h.ID})
	if len(after) != 2 {
		t.Fatalf("containers after = %+v", after)
	}
	byKey := map[string]store.Container{}
	for _, c := range after {
		byKey[c.IdentityKey] = c
	}
	for _, b := range before {
		if byKey[b.IdentityKey].ID != b.ID || byKey[b.IdentityKey].StackID != b.StackID {
			t.Errorf("%s: UUID %s/%s → %s/%s", b.IdentityKey, b.StackID, b.ID, byKey[b.IdentityKey].StackID, byKey[b.IdentityKey].ID)
		}
	}
	if byKey["web"].DockerID != "d1-new" {
		t.Errorf("docker ID not refreshed: %s", byKey["web"].DockerID)
	}
	st, _ := r.db.Stack(ctx, before[0].StackID)
	if st.Provider != "portainer" {
		t.Errorf("provider = %s, want portainer after relabel", st.Provider)
	}
}

func TestInventoryRequestedOnlyWhenHashDiffers(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.agent(agentA, "h", fakeDocker(composeContainer("d1", "app", "web")))
	a.Step(ctx) // startup: poll + inventory

	snap, _ := a.Collector.Collect(ctx)
	body, _ := json.Marshal(protocol.PollRequest{ProtocolVersion: protocol.Version, AgentID: agentA, HostName: "h", InventoryHash: snap.Hash})
	var resp protocol.PollResponse
	if err := a.HTTP.Do(ctx, "POST", protocol.PathPoll, json.RawMessage(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.InventoryRequested {
		t.Fatal("same hash: inventory must not be requested")
	}
	if len(resp.Commands) != 0 {
		t.Fatal("no commands expected")
	}
	body, _ = json.Marshal(protocol.PollRequest{ProtocolVersion: protocol.Version, AgentID: agentA, HostName: "h", InventoryHash: "sha256:other"})
	a.HTTP.Do(ctx, "POST", protocol.PathPoll, json.RawMessage(body), &resp)
	if !resp.InventoryRequested {
		t.Fatal("different hash: inventory must be requested")
	}
}

func TestUnknownAgentInventoryConflict(t *testing.T) {
	r := newRig(t)
	h := &client.HTTP{BaseURL: r.srv.URL, Key: key, AgentID: agentA, Client: r.srv.Client(), Now: r.clock.Now}
	err := h.Do(context.Background(), "PUT", protocol.PathInventory, protocol.Inventory{ProtocolVersion: protocol.Version, AgentID: agentA, InventoryHash: "sha256:x"}, nil)
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("err = %v, want 409", err)
	}
}

func TestHostComesBackOnline(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.agent(agentA, "h", fakeDocker())
	a.Step(ctx)
	r.clock.Add(5 * time.Minute)
	if got, _ := r.db.MarkOfflineHosts(ctx, r.clock.Now()); len(got) != 1 {
		t.Fatalf("offline = %d, want 1", len(got))
	}
	a.Step(ctx)
	if len(r.notes.online) != 1 {
		t.Fatalf("online notices = %d, want 1", len(r.notes.online))
	}
}

func copyLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (n *notices) OperationFinished(_ context.Context, op store.Operation) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.finished = append(n.finished, op)
}
func (n *notices) Drifted(context.Context, []store.DriftEvent)             {}
func (n *notices) MergeSuggested(context.Context, []store.MergeSuggestion) {}
func (n *notices) RefreshStacks(context.Context, []string)                 {}
