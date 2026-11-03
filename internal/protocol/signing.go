package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxClockSkew is the largest accepted difference between a request's
// timestamp and the bot's clock.
const MaxClockSkew = 300 * time.Second

// MinSharedKeyLength is the minimum AGENT_SHARED_KEY length in characters.
const MinSharedKeyLength = 32

var (
	// ErrMissingHeaders means one or more signing headers are absent.
	ErrMissingHeaders = errors.New("missing signature headers")
	// ErrBadSignature means the signature does not match the message.
	ErrBadSignature = errors.New("bad signature")
	// ErrBadTimestamp means the timestamp header is not an integer.
	ErrBadTimestamp = errors.New("malformed timestamp")
)

// ClockSkewError is returned when the signature is valid but the timestamp is
// outside MaxClockSkew. Because it is only returned after the signature
// verifies, the caller may trust the agent ID header and Skew.
type ClockSkewError struct {
	Skew time.Duration // request time minus server time
}

func (e *ClockSkewError) Error() string {
	return fmt.Sprintf("clock skew of %s exceeds %s", e.Skew.Round(time.Second), MaxClockSkew)
}

// CanonicalRequest builds the string that a request signature covers.
// pathWithQuery is the request-target exactly as sent on the wire
// (http.Request.RequestURI on the server, URL.RequestURI() on the client).
func CanonicalRequest(method, pathWithQuery string, timestamp int64, agentID string, body []byte) string {
	return "DDUM1\n" +
		strings.ToUpper(method) + "\n" +
		pathWithQuery + "\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		agentID + "\n" +
		bodyHash(body)
}

// SignRequest returns the hex signature for a request.
func SignRequest(key []byte, method, pathWithQuery string, timestamp int64, agentID string, body []byte) string {
	return mac(key, CanonicalRequest(method, pathWithQuery, timestamp, agentID, body))
}

// VerifyRequest checks a request's signature and then its timestamp.
// It returns nil, ErrMissingHeaders, ErrBadTimestamp, ErrBadSignature, or a
// *ClockSkewError (only when the signature itself is valid).
func VerifyRequest(key []byte, method, pathWithQuery, agentID, timestampHeader, signatureHeader string, body []byte, now time.Time) error {
	if agentID == "" || timestampHeader == "" || signatureHeader == "" {
		return ErrMissingHeaders
	}
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	want := SignRequest(key, method, pathWithQuery, ts, agentID, body)
	if !equalHex(want, signatureHeader) {
		return ErrBadSignature
	}
	skew := time.Unix(ts, 0).Sub(now)
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return &ClockSkewError{Skew: skew}
	}
	return nil
}

// CanonicalResponse builds the string that a response signature covers. It is
// bound to the request signature so an old response cannot be replayed.
func CanonicalResponse(requestSignature string, timestamp int64, body []byte) string {
	return "DDUM1-RESP\n" +
		strings.ToLower(requestSignature) + "\n" +
		strconv.FormatInt(timestamp, 10) + "\n" +
		bodyHash(body)
}

// SignResponse returns the hex signature for a response to the request that
// carried requestSignature.
func SignResponse(key []byte, requestSignature string, timestamp int64, body []byte) string {
	return mac(key, CanonicalResponse(requestSignature, timestamp, body))
}

// VerifyResponse checks a response signature against the request it answers.
func VerifyResponse(key []byte, requestSignature, timestampHeader, signatureHeader string, body []byte) error {
	if timestampHeader == "" || signatureHeader == "" {
		return ErrMissingHeaders
	}
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	if !equalHex(SignResponse(key, requestSignature, ts, body), signatureHeader) {
		return ErrBadSignature
	}
	return nil
}

// SignHTTPRequest sets the protocol headers on req. body must be the exact
// bytes that will be sent. It returns the request signature, which the caller
// needs to verify the response.
func SignHTTPRequest(req *http.Request, key []byte, agentID string, body []byte, now time.Time) string {
	ts := now.Unix()
	sig := SignRequest(key, req.Method, req.URL.RequestURI(), ts, agentID, body)
	req.Header.Set(HeaderAgentID, agentID)
	req.Header.Set(HeaderProtocol, "1") // this is protocol 1's signature; see keys.go
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, sig)
	return sig
}

// VerifyHTTPRequest verifies the protocol headers of an incoming request.
// body must be the exact bytes read from req.Body.
func VerifyHTTPRequest(req *http.Request, key []byte, body []byte, now time.Time) error {
	return VerifyRequest(key, req.Method, req.RequestURI,
		req.Header.Get(HeaderAgentID),
		req.Header.Get(HeaderTimestamp),
		req.Header.Get(HeaderSignature),
		body, now)
}

// SignHTTPResponse sets the response signing headers. Call it before
// WriteHeader, with the exact body that will be written.
func SignHTTPResponse(h http.Header, key []byte, requestSignature string, body []byte, now time.Time) {
	ts := now.Unix()
	h.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	h.Set(HeaderSignature, SignResponse(key, requestSignature, ts, body))
}

// VerifyHTTPResponse verifies a response's signing headers. body must be the
// exact bytes read from the response.
func VerifyHTTPResponse(resp *http.Response, key []byte, requestSignature string, body []byte) error {
	return VerifyResponse(key, requestSignature,
		resp.Header.Get(HeaderTimestamp),
		resp.Header.Get(HeaderSignature),
		body)
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func mac(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// equalHex compares two hex signatures in constant time. A malformed header
// simply fails to match.
func equalHex(want, got string) bool {
	w, err := hex.DecodeString(want)
	if err != nil {
		return false
	}
	g, err := hex.DecodeString(strings.ToLower(got))
	if err != nil {
		return false
	}
	return hmac.Equal(w, g)
}
