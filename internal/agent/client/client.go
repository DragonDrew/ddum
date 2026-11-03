// Package client is the agent's side of the agent–bot protocol: signed
// requests, verified responses, the poll loop, and inventory submission
// .
package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// APIError is a non-2xx response whose signature verified.
type APIError struct {
	Status     int
	Body       protocol.ErrorBody
	RetryAfter time.Duration // from Retry-After on 429
}

func (e *APIError) Error() string {
	return fmt.Sprintf("bot returned %d: %s", e.Status, e.Body)
}

// Code returns the error code of err if it is an *APIError, else "".
func Code(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Body.Code
	}
	return ""
}

// pinBotKey remembers the bot's key for every later response.
func (h *HTTP) pinBotKey(key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.BotKey != "" {
		return nil
	}
	if h.SaveBotKey != nil {
		if err := h.SaveBotKey(key); err != nil {
			return fmt.Errorf("pin the bot's key: %w", err)
		}
	}
	h.BotKey = key
	return nil
}

// ErrUnsignedResponse means the response failed signature verification and
// was discarded. It is treated as a network failure.
var ErrUnsignedResponse = errors.New("response signature missing or invalid")

// HTTP sends signed requests to the bot.
type HTTP struct {
	BaseURL string // no trailing slash
	Key     []byte // the shared key, which introduces this agent to the bot
	AgentID string
	Client  *http.Client
	Now     func() time.Time
	// AgentKey is this agent's own key; empty means a throwaway one (tests).
	AgentKey ed25519.PrivateKey
	// BotKey is the bot's pinned key. With "", the first response the shared
	// key vouches for pins it, and SaveBotKey (if set) keeps it.
	BotKey     string
	SaveBotKey func(string) error

	mu sync.Mutex
}

// Do sends body as JSON to method path and decodes a 2xx response into out
// (which may be nil). Every response, including errors, must carry a valid
// signature bound to this request; otherwise ErrUnsignedResponse is returned
// and the body is never interpreted.
func (h *HTTP) Do(ctx context.Context, method, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, h.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	h.mu.Lock()
	if h.AgentKey == nil {
		if h.AgentKey, err = protocol.NewKey(); err != nil {
			h.mu.Unlock()
			return err
		}
	}
	agentKey, pinned := h.AgentKey, h.BotKey
	h.mu.Unlock()
	reqSig, err := protocol.SignHTTPRequestV2(req, h.Key, agentKey, h.AgentID, payload, h.Now())
	if err != nil {
		return err
	}

	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(respBody) > protocol.MaxBodyBytes {
		return fmt.Errorf("response larger than %d bytes", protocol.MaxBodyBytes)
	}
	botKey, err := protocol.VerifyHTTPResponseV2(resp, pinned, h.Key, reqSig, respBody)
	if err != nil {
		hint := ""
		if resp.StatusCode == http.StatusUnauthorized {
			hint = "; if this keeps happening, check that AGENT_SHARED_KEY matches the bot's"
		}
		if errors.Is(err, protocol.ErrBotKeyChanged) {
			hint = "; if the bot was reinstalled, delete bot_key from this agent's data volume. Otherwise something else is answering as the bot"
		}
		return fmt.Errorf("%w (HTTP %d): %v%s", ErrUnsignedResponse, resp.StatusCode, err, hint)
	}
	if pinned == "" {
		if err := h.pinBotKey(botKey); err != nil {
			return err
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Status: resp.StatusCode}
		var er protocol.ErrorResponse
		if json.Unmarshal(respBody, &er) == nil {
			ae.Body = er.Error
		}
		if ae.Body.Code == "" {
			ae.Body.Code = http.StatusText(resp.StatusCode)
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ae.RetryAfter = time.Duration(s) * time.Second
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
