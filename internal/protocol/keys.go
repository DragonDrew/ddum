package protocol

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Protocol 2 gives every agent and the bot a key of their own. The
// shared key only introduces an agent: once the bot has pinned an agent's public
// key, that agent's requests stand on its own signature, and holding the shared
// key no longer lets anyone pose as it. The agent pins the bot's key the same
// way, so holding the shared key does not let anyone forge the bot's answers.

// Headers added by protocol 2.
const (
	HeaderNonce          = "X-DDUM-Nonce"
	HeaderAgentKey       = "X-DDUM-Agent-Key"
	HeaderAgentSignature = "X-DDUM-Agent-Signature"
	HeaderBotKey         = "X-DDUM-Bot-Key"
	HeaderBotSignature   = "X-DDUM-Bot-Signature"
)

// ErrBotKeyChanged means a response came from a key other than the pinned one.
var ErrBotKeyChanged = errors.New("the bot's key is not the one this agent pinned")

// NewKey generates an Ed25519 private key.
func NewKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// EncodePublicKey is the text form of a public key, as sent and stored.
func EncodePublicKey(priv ed25519.PrivateKey) string {
	return base64.RawStdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// DecodePublicKey parses the text form of a public key.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key: %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// KeyFingerprint is the short form of a public key that people compare.
func KeyFingerprint(publicKey string) string {
	sum := sha256.Sum256([]byte(publicKey))
	h := hex.EncodeToString(sum[:8])
	return h[:4] + ":" + h[4:8] + ":" + h[8:12] + ":" + h[12:16]
}

// CanonicalRequestV2 builds the string a protocol 2 request's signatures cover.
func CanonicalRequestV2(method, pathWithQuery string, timestamp int64, agentID, nonce string, body []byte) string {
	return "DDUM2\n" +
		strings.ToUpper(method) + "\n" +
		pathWithQuery + "\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		agentID + "\n" +
		nonce + "\n" +
		bodyHash(body)
}

// SignHTTPRequestV2 sets the protocol 2 headers on req. It returns the agent's
// signature, which the response must be bound to.
func SignHTTPRequestV2(req *http.Request, shared []byte, priv ed25519.PrivateKey, agentID string, body []byte, now time.Time) (string, error) {
	var n [16]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	nonce := hex.EncodeToString(n[:])
	ts := now.Unix()
	canonical := CanonicalRequestV2(req.Method, req.URL.RequestURI(), ts, agentID, nonce, body)
	sig := base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, []byte(canonical)))
	req.Header.Set(HeaderAgentID, agentID)
	req.Header.Set(HeaderProtocol, strconv.Itoa(Version))
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, mac(shared, canonical))
	req.Header.Set(HeaderAgentKey, EncodePublicKey(priv))
	req.Header.Set(HeaderAgentSignature, sig)
	return sig, nil
}

func requestCanonicalV2(req *http.Request, body []byte) (string, error) {
	ts, err := strconv.ParseInt(req.Header.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return "", ErrBadTimestamp
	}
	return CanonicalRequestV2(req.Method, req.RequestURI, ts, req.Header.Get(HeaderAgentID), req.Header.Get(HeaderNonce), body), nil
}

// VerifySharedV2 checks a protocol 2 request's shared-key signature.
func VerifySharedV2(req *http.Request, shared, body []byte) error {
	canonical, err := requestCanonicalV2(req, body)
	if err != nil {
		return err
	}
	if !equalHex(mac(shared, canonical), req.Header.Get(HeaderSignature)) {
		return ErrBadSignature
	}
	return nil
}

// VerifyAgentV2 checks a protocol 2 request's signature against publicKey.
func VerifyAgentV2(req *http.Request, publicKey string, body []byte) error {
	pub, err := DecodePublicKey(publicKey)
	if err != nil {
		return err
	}
	canonical, err := requestCanonicalV2(req, body)
	if err != nil {
		return err
	}
	sig, err := base64.RawStdEncoding.DecodeString(req.Header.Get(HeaderAgentSignature))
	if err != nil || !ed25519.Verify(pub, []byte(canonical), sig) {
		return ErrBadSignature
	}
	return nil
}

// CheckTimestamp returns a *ClockSkewError when a verified request's timestamp
// is outside MaxClockSkew.
func CheckTimestamp(req *http.Request, now time.Time) error {
	ts, err := strconv.ParseInt(req.Header.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	skew := time.Unix(ts, 0).Sub(now)
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return &ClockSkewError{Skew: skew}
	}
	return nil
}

// CanonicalResponseV2 builds the string a protocol 2 response's signatures
// cover. Unlike protocol 1 it covers the status and Retry-After, which the agent
// acts on, and the bot's key, which it pins.
func CanonicalResponseV2(agentSignature string, status int, retryAfter string, timestamp int64, botKey string, body []byte) string {
	return "DDUM2-RESP\n" +
		agentSignature + "\n" +
		strconv.Itoa(status) + "\n" +
		retryAfter + "\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		botKey + "\n" +
		bodyHash(body)
}

// SignHTTPResponseV2 sets the response headers, Retry-After included if it is
// already set. Call it before WriteHeader, with the status and body to be sent.
func SignHTTPResponseV2(h http.Header, priv ed25519.PrivateKey, shared []byte, agentSignature string, status int, body []byte, now time.Time) {
	ts := now.Unix()
	botKey := EncodePublicKey(priv)
	canonical := CanonicalResponseV2(agentSignature, status, h.Get("Retry-After"), ts, botKey, body)
	h.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	h.Set(HeaderBotKey, botKey)
	h.Set(HeaderBotSignature, base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, []byte(canonical))))
	// Lets an agent with no pinned key trust the bot's key on first contact.
	h.Set(HeaderSignature, mac(shared, canonical))
}

// VerifyHTTPResponseV2 verifies a response against the pinned bot key. With no
// key pinned yet, the shared-key signature vouches for the one offered. It
// returns the bot's key.
func VerifyHTTPResponseV2(resp *http.Response, pinned string, shared []byte, agentSignature string, body []byte) (string, error) {
	h := resp.Header
	offered, sigText, tsText := h.Get(HeaderBotKey), h.Get(HeaderBotSignature), h.Get(HeaderTimestamp)
	if offered == "" || sigText == "" || tsText == "" {
		return "", ErrMissingHeaders
	}
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil {
		return "", ErrBadTimestamp
	}
	canonical := CanonicalResponseV2(agentSignature, resp.StatusCode, h.Get("Retry-After"), ts, offered, body)
	if pinned != "" && offered != pinned {
		return "", ErrBotKeyChanged
	}
	if pinned == "" && !hmac.Equal([]byte(mac(shared, canonical)), []byte(strings.ToLower(h.Get(HeaderSignature)))) {
		return "", ErrBadSignature
	}
	pub, err := DecodePublicKey(offered)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawStdEncoding.DecodeString(sigText)
	if err != nil || !ed25519.Verify(pub, []byte(canonical), sig) {
		return "", ErrBadSignature
	}
	return offered, nil
}
