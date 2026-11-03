// Package protocol holds the wire contract shared by the bot and the agent:
// version constants, header names, the error envelope, and request/response
// signing. It must not import anything else from this repo.
package protocol

import "fmt"

// Protocol versions.
const (
	Version             = 2
	MinSupportedVersion = 1 // only approved agents with no key yet
	MaxSupportedVersion = 2
)

// BasePath is the prefix of every agent-facing endpoint.
const BasePath = "/v1"

// HTTP header names.
const (
	HeaderAgentID   = "X-DDUM-Agent-ID"
	HeaderProtocol  = "X-DDUM-Protocol"
	HeaderTimestamp = "X-DDUM-Timestamp"
	HeaderSignature = "X-DDUM-Signature"
)

// Request body limits.
const (
	MaxInventoryBodyBytes = 8 << 20
	MaxBodyBytes          = 1 << 20
)

// Error codes carried in ErrorBody.Code.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeBadSignature        = "bad_signature"
	CodeClockSkew           = "clock_skew"
	CodeAgentRemoved        = "agent_removed"
	CodeAgentKeyChanged     = "agent_key_changed"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodeTooLarge            = "too_large"
	CodeProtocolUnsupported = "protocol_unsupported"
	CodeTooManyRequests     = "too_many_requests"
	CodeInternal            = "internal"
)

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the machine-readable code and a human message. Optional
// fields are set only for the codes that use them.
type ErrorBody struct {
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
	Supported  []int  `json:"supported,omitempty"`   // protocol_unsupported: [min, max]
	ServerTime string `json:"server_time,omitempty"` // clock_skew: RFC 3339 UTC
}

func (e ErrorBody) String() string {
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// SupportsVersion reports whether the bot accepts protocol version v.
func SupportsVersion(v int) bool {
	return v >= MinSupportedVersion && v <= MaxSupportedVersion
}
