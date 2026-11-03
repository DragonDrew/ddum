package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// post signs one poll with the given keys.
func (r *rig) post(agentID string, shared []byte, priv ed25519.PrivateKey) (*http.Response, []byte) {
	r.t.Helper()
	body, _ := json.Marshal(protocol.PollRequest{ProtocolVersion: protocol.Version, AgentID: agentID, HostName: "h"})
	req, _ := http.NewRequest("POST", r.srv.URL+protocol.PathPoll, bytes.NewReader(body))
	if _, err := protocol.SignHTTPRequestV2(req, shared, priv, agentID, body, r.clock.Now()); err != nil {
		r.t.Fatal(err)
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	k, err := protocol.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestTheFirstKeyIsPinnedAndTheSharedKeyNoLongerHelps(t *testing.T) {
	r := newRig(t)
	real := newKey(t)
	if resp, body := r.post(agentA, key, real); resp.StatusCode != http.StatusOK {
		t.Fatalf("first poll: %d %s", resp.StatusCode, body)
	}
	h, _ := r.db.HostByAgent(context.Background(), agentA)
	if h.AgentKey != protocol.EncodePublicKey(real) {
		t.Fatalf("key not pinned: %q", h.AgentKey)
	}
	r.db.ApproveHost(context.Background(), h.ID, owner, r.clock.Now())

	// Without the shared key, its own signature is enough.
	if resp, body := r.post(agentA, []byte("not-the-shared-key-not-the-shared-key"), real); resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned agent refused: %d %s", resp.StatusCode, body)
	}
	// Holding the shared key does not let anyone pose as it.
	impostor := newKey(t)
	resp, body := r.post(agentA, key, impostor)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), protocol.CodeAgentKeyChanged) {
		t.Fatalf("impostor: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "accept") || strings.Contains(string(body), `"commands"`) {
		t.Fatalf("impostor got: %s", body)
	}
	if len(r.notes.keyChanged) != 1 {
		t.Fatalf("notices = %v", r.notes.keyChanged)
	}
	// Asked once per key.
	r.post(agentA, key, impostor)
	if len(r.notes.keyChanged) != 1 {
		t.Fatalf("asked again: %v", r.notes.keyChanged)
	}
	// A person accepts: the new key works, and the old one stops.
	if _, err := r.db.AcceptAgentKey(context.Background(), h.ID, owner, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if resp, body := r.post(agentA, key, impostor); resp.StatusCode != http.StatusOK {
		t.Fatalf("accepted key: %d %s", resp.StatusCode, body)
	}
	if resp, _ := r.post(agentA, []byte("not-the-shared-key-not-the-shared-key"), real); resp.StatusCode == http.StatusOK {
		t.Fatal("the old key still works")
	}
}

func TestAWrongSharedKeyCannotRegister(t *testing.T) {
	r := newRig(t)
	resp, _ := r.post(agentA, []byte("not-the-shared-key-not-the-shared-key"), newKey(t))
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get(protocol.HeaderBotSignature) != "" {
		t.Fatalf("status %d, signed %q", resp.StatusCode, resp.Header.Get(protocol.HeaderBotSignature))
	}
	if hosts, _ := r.db.Hosts(context.Background(), false); len(hosts) != 0 {
		t.Fatalf("hosts = %d", len(hosts))
	}
}

func TestAReplayedRequestIsRefused(t *testing.T) {
	r := newRig(t)
	body, _ := json.Marshal(protocol.PollRequest{ProtocolVersion: protocol.Version, AgentID: agentA, HostName: "h"})
	req, _ := http.NewRequest("POST", r.srv.URL+protocol.PathPoll, bytes.NewReader(body))
	if _, err := protocol.SignHTTPRequestV2(req, key, newKey(t), agentA, body, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
	send := func() int {
		again, _ := http.NewRequest("POST", r.srv.URL+protocol.PathPoll, bytes.NewReader(body))
		again.Header = req.Header.Clone()
		resp, err := r.srv.Client().Do(again)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := send(); got != http.StatusOK {
		t.Fatalf("first: %d", got)
	}
	if got := send(); got != http.StatusUnauthorized {
		t.Fatalf("replay: %d", got)
	}
}

// legacyPoll is an agent still on protocol 1.
func (r *rig) legacyPoll(agentID string) (*http.Response, []byte) {
	r.t.Helper()
	body, _ := json.Marshal(protocol.PollRequest{ProtocolVersion: 1, AgentID: agentID, HostName: "old"})
	req, _ := http.NewRequest("POST", r.srv.URL+protocol.PathPoll, bytes.NewReader(body))
	protocol.SignHTTPRequest(req, key, agentID, body, r.clock.Now())
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if err := protocol.VerifyHTTPResponse(resp, key, req.Header.Get(protocol.HeaderSignature), out); err != nil {
		r.t.Fatalf("not signed the way protocol 1 expects (status %d): %v", resp.StatusCode, err)
	}
	return resp, out
}

func TestProtocolOneIsOnlyForApprovedAgentsWithoutAKey(t *testing.T) {
	r := newRig(t)
	// A new agent cannot register on it.
	if resp, body := r.legacyPoll(agentA); resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("new agent: %d %s", resp.StatusCode, body)
	}
	// An approved agent from before this release keeps working.
	ctx := context.Background()
	res, err := r.db.RecordPoll(ctx, store.Poll{AgentID: agentB, HostName: "old", PollInterval: 30,
		Docker: protocol.DockerInfo{Arch: "amd64"}, At: r.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	r.db.ApproveHost(ctx, res.Host.ID, owner, r.clock.Now())
	if resp, body := r.legacyPoll(agentB); resp.StatusCode != http.StatusOK {
		t.Fatalf("existing agent: %d %s", resp.StatusCode, body)
	}
	// Once upgraded, its key is pinned, with no question asked, and it cannot go back.
	k := newKey(t)
	if resp, body := r.post(agentB, key, k); resp.StatusCode != http.StatusOK {
		t.Fatalf("upgraded agent: %d %s", resp.StatusCode, body)
	}
	if h, _ := r.db.Host(ctx, res.Host.ID); h.AgentKey != protocol.EncodePublicKey(k) || len(r.notes.keyChanged) != 0 {
		t.Fatalf("key=%q notices=%v", h.AgentKey, r.notes.keyChanged)
	}
	if resp, _ := r.legacyPoll(agentB); resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("back on protocol 1: %d", resp.StatusCode)
	}
}

func TestTheBotKeyStaysTheSameAcrossARestart(t *testing.T) {
	r := newRig(t)
	resp, _ := r.post(agentA, key, newKey(t))
	first := resp.Header.Get(protocol.HeaderBotKey)
	r.restart()
	r.clock.Add(time.Second)
	resp, _ = r.post(agentB, key, newKey(t))
	if first == "" || resp.Header.Get(protocol.HeaderBotKey) != first {
		t.Fatalf("%q then %q", first, resp.Header.Get(protocol.HeaderBotKey))
	}
}
