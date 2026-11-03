package protocol

import "encoding/json"

// PathEvents is the events endpoint of one operation.
func PathEvents(operationID string) string {
	return BasePath + "/agent/operations/" + operationID + "/events"
}

// Command types.
const (
	CommandDeploy   = "deploy"
	CommandRollback = "rollback"
)

// Operation states.
const (
	OpQueued          = "queued"
	OpDispatched      = "dispatched"
	OpAccepted        = "accepted"
	OpRunning         = "running"
	OpVerifying       = "verifying"
	OpSucceeded       = "succeeded"
	OpFailed          = "failed"
	OpPartiallyFailed = "partially_failed"
	OpInterrupted     = "interrupted"
	OpExpired         = "expired"
	OpCancelled       = "cancelled"
)

// Event types.
const (
	EventAccepted    = "accepted"
	EventRejected    = "rejected"
	EventProgress    = "progress"
	EventCompleted   = "completed"
	EventFailed      = "failed"
	EventInterrupted = "interrupted"
)

// Progress steps.
const (
	StepValidating   = "validating"
	StepSnapshotting = "snapshotting"
	StepPulling      = "pulling"
	StepDeploying    = "deploying"
	StepVerifying    = "verifying"
	StepFinalising   = "finalising"
)

// Failure and rejection codes.
const (
	FailUnsupported     = "unsupported"
	FailIneligible      = "ineligible"
	FailUnsafe          = "unsafe"
	FailPull            = "pull_failed"
	FailDeploy          = "deploy_failed"
	FailVerify          = "verify_failed"
	FailDigestMismatch  = "digest_mismatch"
	FailInterrupted     = "interrupted"
	FailInternal        = "internal"
	FailNotAcknowledged = "not_acknowledged"
)

// Per-container outcomes.
const (
	OutcomeUpdated   = "updated"
	OutcomeUnchanged = "unchanged"
	OutcomeFailed    = "failed"
	OutcomeSkipped   = "skipped"
)

// MaxOutputTail is the largest output_tail a failed event carries.
const MaxOutputTail = 64 << 10

// ImageState is an image reference and the digest it must resolve to.
type ImageState struct {
	ImageRef string `json:"image_ref"`
	Digest   string `json:"digest"`
}

// ServiceChange is one container to move to a new image.
type ServiceChange struct {
	ContainerID string     `json:"container_id"`
	IdentityKey string     `json:"identity_key"`
	Current     ImageState `json:"current"`
	Desired     ImageState `json:"desired"`
}

// DeployTarget is one stack of a deploy operation.
type DeployTarget struct {
	StackID          string          `json:"stack_id"`
	StackFingerprint string          `json:"stack_fingerprint"`
	Services         []ServiceChange `json:"services"`
}

// VerifySettings come from the effective policy.
type VerifySettings struct {
	TimeoutSeconds int `json:"timeout_seconds"`
	StableSeconds  int `json:"stable_seconds"`
}

// Scope is the scope an operation was created for.
type Scope struct {
	Kind string `json:"kind"` // stack | group
	ID   string `json:"id"`
}

// DeployPayload is the payload of a deploy command.
type DeployPayload struct {
	OperationID string         `json:"operation_id"`
	Scope       Scope          `json:"scope"`
	IssuedAt    string         `json:"issued_at"`
	ExpiresAt   string         `json:"expires_at"`
	Verify      VerifySettings `json:"verify"`
	Targets     []DeployTarget `json:"targets"`
	SelfUpdate  bool           `json:"self_update,omitempty"`
}

// ContainerOutcome reports one container's result.
type ContainerOutcome struct {
	IdentityKey string `json:"identity_key"`
	StackID     string `json:"stack_id,omitempty"`
	Outcome     string `json:"outcome"`
	NewDigest   string `json:"new_digest,omitempty"`
	DockerID    string `json:"docker_id,omitempty"`
	Message     string `json:"message,omitempty"`
}

// SnapshotRef names the snapshot an operation created.
type SnapshotRef struct {
	SnapshotID string `json:"snapshot_id"`
	StackID    string `json:"stack_id,omitempty"`
	Created    bool   `json:"created"`
}

// Result is the result of a completed or failed operation.
type Result struct {
	Containers []ContainerOutcome `json:"containers"`
	Snapshots  []SnapshotRef      `json:"snapshots,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
	// RolledBack is set when the agent restored the snapshot itself after a
	// failed bot self-update.
	RolledBack bool `json:"rolled_back,omitempty"`
}

// RollbackPayload is the payload of a rollback command.
type RollbackPayload struct {
	OperationID         string         `json:"operation_id"`
	OriginalOperationID string         `json:"original_operation_id"`
	SnapshotIDs         []string       `json:"snapshot_ids"`
	Scope               Scope          `json:"scope"`
	IssuedAt            string         `json:"issued_at"`
	ExpiresAt           string         `json:"expires_at"`
	Verify              VerifySettings `json:"verify"`
	// Targets name the stacks and services the original operation changed,
	// with Desired set to what the rollback must restore.
	Targets []DeployTarget `json:"targets"`
}

// Event is one operation event.
type Event struct {
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	At         string          `json:"at"`
	Step       string          `json:"step,omitempty"`
	Message    string          `json:"message,omitempty"`
	Target     string          `json:"target,omitempty"`
	Code       string          `json:"code,omitempty"`
	Result     *Result         `json:"result,omitempty"`
	OutputTail string          `json:"output_tail,omitempty"`
	Extra      json.RawMessage `json:"extra,omitempty"`
}

// EventsRequest is the body of POST .../events.
type EventsRequest struct {
	Events []Event `json:"events"`
}

// EventsResponse acknowledges events up to AckedSeq.
type EventsResponse struct {
	AckedSeq int `json:"acked_seq"`
}

// IsTerminalEvent reports whether an event ends an operation.
func IsTerminalEvent(t string) bool {
	switch t {
	case EventRejected, EventCompleted, EventFailed, EventInterrupted:
		return true
	}
	return false
}
