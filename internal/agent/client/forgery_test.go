package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/httpapi"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// network is someone on the path between an agent and the real bot. It does
// not hold the shared key. These are attacks that protocol 1 allowed.
type network struct {
	t   *testing.T
	bot http.Handler
	// tamper changes a genuine response before the agent sees it.
	tamper func(*http.Response)
	// junk sends a junk request carrying the agent's signatures instead.
	junk bool
}

func (n network) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	out := httptest.NewRequest(r.Method, r.URL.RequestURI(), strings.NewReader(string(body)))
	out.Header = r.Header.Clone()
	if n.junk {
		out = httptest.NewRequest(r.Method, r.URL.RequestURI(), strings.NewReader("{}"))
		out.Header.Set(protocol.HeaderSignature, r.Header.Get(protocol.HeaderSignature))
		out.Header.Set(protocol.HeaderAgentSignature, r.Header.Get(protocol.HeaderAgentSignature))
		out.Header.Set(protocol.HeaderAgentID, r.Header.Get(protocol.HeaderAgentID))
		out.Header.Set(protocol.HeaderAgentKey, r.Header.Get(protocol.HeaderAgentKey))
		out.Header.Set(protocol.HeaderProtocol, "2")
		out.Header.Set(protocol.HeaderTimestamp, "1")
		out.Header.Set(protocol.HeaderNonce, "x")
	}
	rec := httptest.NewRecorder()
	n.bot.ServeHTTP(rec, out)
	resp := rec.Result()
	if n.tamper != nil {
		n.tamper(resp)
	}
	return resp, nil
}

func realBot(t *testing.T) http.Handler {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ddum.db"), ids.V7{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return httpapi.New(db, nil, key, slog.New(slog.DiscardHandler), time.Now).Handler()
}

func attacked(t *testing.T, n network) *HTTP {
	n.t, n.bot = t, realBot(t)
	return &HTTP{BaseURL: "http://bot.invalid", Key: key, AgentID: agentID, Client: &http.Client{Transport: n}, Now: time.Now}
}

func poll(h *HTTP) error {
	var resp protocol.PollResponse
	return h.Do(context.Background(), http.MethodPost, protocol.PathPoll, protocol.PollRequest{ProtocolVersion: protocol.Version, AgentID: agentID, HostName: "h"}, &resp)
}

// The bot used to sign its refusal of a request it could not verify, using
// whatever signature that request carried.
func TestARefusalIsNotSignedForAnUnverifiedRequest(t *testing.T) {
	err := poll(attacked(t, network{junk: true}))
	if !errors.Is(err, ErrUnsignedResponse) {
		t.Fatalf("the agent accepted the bot's refusal of someone else's request: %v", err)
	}
}

func TestAChangedStatusIsDetected(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusForbidden} {
		err := poll(attacked(t, network{tamper: func(r *http.Response) { r.StatusCode = status }}))
		if !errors.Is(err, ErrUnsignedResponse) {
			t.Fatalf("status %d was accepted: %v", status, err)
		}
	}
}

func TestAnAddedRetryAfterIsDetected(t *testing.T) {
	err := poll(attacked(t, network{tamper: func(r *http.Response) { r.Header.Set("Retry-After", "604800") }}))
	if !errors.Is(err, ErrUnsignedResponse) {
		t.Fatalf("an added Retry-After was accepted: %v", err)
	}
}

func TestABotKeyOtherThanThePinnedOneIsRefused(t *testing.T) {
	h := attacked(t, network{})
	other, err := protocol.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	h.BotKey = protocol.EncodePublicKey(other)
	if err := poll(h); !errors.Is(err, ErrUnsignedResponse) || !strings.Contains(err.Error(), "bot_key") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheBotKeyIsPinnedOnFirstContact(t *testing.T) {
	h := attacked(t, network{})
	var saved string
	h.SaveBotKey = func(k string) error { saved = k; return nil }
	if err := poll(h); err != nil {
		t.Fatal(err)
	}
	if saved == "" || h.BotKey != saved {
		t.Fatalf("saved=%q pinned=%q", saved, h.BotKey)
	}
}

func TestAGenuineRetryAfterIsCapped(t *testing.T) {
	l := &Loop{Log: slog.New(slog.DiscardHandler), PollInterval: 30 * time.Second}
	if got := l.failed("poll", &APIError{Status: http.StatusTooManyRequests, RetryAfter: 7 * 24 * time.Hour}); got != RemovedPollInterval {
		t.Fatalf("wait = %s", got)
	}
}
