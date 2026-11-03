package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Endpoint paths.
const (
	PathPoll      = BasePath + "/agent/poll"
	PathInventory = BasePath + "/agent/inventory"
)

// Capability strings.
const (
	CapProviderCompose   = "provider:compose"
	CapProviderPortainer = "provider:portainer"
	CapProviderDocker    = "provider:docker"
	CapActionDeploy      = "action:deploy"
	CapActionRollback    = "action:rollback"
	CapTagChangeEdit     = "feature:tag_change_edit"
)

// Provider types.
const (
	ProviderPortainer = "portainer"
	ProviderCompose   = "compose"
	ProviderDocker    = "docker"
)

// Values of Container.Self.
const (
	SelfAgent = "agent"
	SelfBot   = "bot"
)

// Container notes.
const (
	NoteReplicasUnsupported = "replicas_unsupported"
)

// Deploy.Reason values set during discovery.
const (
	ReasonStandaloneManual        = "standalone_manual"
	ReasonPortainerNotEnabled     = "portainer provider not enabled"
	ReasonReplicasUnsupported     = "replicas_unsupported"
	ReasonComposeFilesUnavailable = "compose_files_unavailable"
)

// DockerInfo describes the agent's Docker engine.
type DockerInfo struct {
	EngineVersion string `json:"engine_version"`
	APIVersion    string `json:"api_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	// ImageIDIsDigest: the containerd image store, where even a local build has
	// a digest, so a digest does not show that an image came from a registry.
	ImageIDIsDigest bool `json:"image_id_is_digest,omitempty"`
}

// ActiveOperation is an operation the agent is currently executing.
type ActiveOperation struct {
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
}

// PollRequest is the body of POST /v1/agent/poll.
type PollRequest struct {
	ProtocolVersion     int               `json:"protocol_version"`
	AgentID             string            `json:"agent_id"`
	AgentVersion        string            `json:"agent_version"`
	HostName            string            `json:"host_name"`
	PollIntervalSeconds int               `json:"poll_interval_seconds"`
	Capabilities        []string          `json:"capabilities"`
	Docker              DockerInfo        `json:"docker"`
	InventoryHash       string            `json:"inventory_hash"`
	ActiveOperations    []ActiveOperation `json:"active_operations"`
}

// Command is one queued operation handed to the agent.
type Command struct {
	OperationID    string          `json:"operation_id"`
	Type           string          `json:"type"`
	LeaseExpiresAt string          `json:"lease_expires_at"`
	Payload        json.RawMessage `json:"payload"`
}

// PollResponse is the body of a successful poll.
type PollResponse struct {
	ServerTime         string    `json:"server_time"`
	InventoryRequested bool      `json:"inventory_requested"`
	Commands           []Command `json:"commands"`
}

// Inventory is the body of PUT /v1/agent/inventory.
type Inventory struct {
	ProtocolVersion int              `json:"protocol_version"`
	AgentID         string           `json:"agent_id"`
	CollectedAt     string           `json:"collected_at"`
	InventoryHash   string           `json:"inventory_hash"`
	Providers       []ProviderStatus `json:"providers"`
	Stacks          []Stack          `json:"stacks"`
}

// ProviderStatus reports whether a provider could be queried.
type ProviderStatus struct {
	Type   string `json:"type"`
	Status string `json:"status"` // ok | error
	Detail string `json:"detail"`
}

// Stack is one discovered logical workload.
type Stack struct {
	Fingerprint string          `json:"fingerprint"`
	DisplayName string          `json:"display_name"`
	Provider    string          `json:"provider"`
	ProviderRef json.RawMessage `json:"provider_ref,omitempty"`
	Compose     *ComposeInfo    `json:"compose,omitempty"`
	Deploy      Support         `json:"deploy"`
	Rollback    RollbackInfo    `json:"rollback"`
	Containers  []Container     `json:"containers"`
}

// ComposeInfo is the Compose metadata read from container labels.
type ComposeInfo struct {
	Project         string   `json:"project"`
	ConfigFiles     []string `json:"config_files"`
	WorkingDir      string   `json:"working_dir"`
	EnvironmentFile string   `json:"environment_file,omitempty"`
	FilesAccessible bool     `json:"files_accessible"`
}

// Support says whether an action is supported and, if not, why.
type Support struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// RollbackInfo reports snapshot availability.
type RollbackInfo struct {
	Available  bool   `json:"available"`
	SnapshotID string `json:"snapshot_id,omitempty"` // the newest usable snapshot
	// SnapshotIDs lists every usable snapshot of the stack.
	SnapshotIDs []string `json:"snapshot_ids,omitempty"`
}

// TagChange is the tag-source classification.
type TagChange struct {
	Supported bool   `json:"supported"`
	Source    string `json:"source"`
	Variable  string `json:"variable,omitempty"`
	Reason    string `json:"reason"`
}

// Container is one discovered container (one service for Compose stacks).
type Container struct {
	IdentityKey  string            `json:"identity_key"`
	DockerID     string            `json:"docker_id"`
	DockerName   string            `json:"docker_name"`
	ImageRef     string            `json:"image_ref"`
	ImageID      string            `json:"image_id"`
	RepoDigests  []string          `json:"repo_digests"`
	Platform     string            `json:"platform"`
	State        string            `json:"state"`
	Health       string            `json:"health"`
	RestartCount int               `json:"restart_count"`
	CreatedAt    string            `json:"created_at"`
	DdumLabels   map[string]string `json:"ddum_labels"`
	Protected    bool              `json:"protected"`
	Self         string            `json:"self"`
	TagChange    *TagChange        `json:"tag_change,omitempty"`
	Notes        []string          `json:"notes"`
}

// InventoryHash returns "sha256:<hex>" over the canonical JSON of providers
// and stacks, excluding collected_at and the volatile per-container fields
// restart_count and created_at. Stacks, containers, and slices
// are sorted first so the hash does not depend on discovery order.
func InventoryHash(providers []ProviderStatus, stacks []Stack) string {
	ps := append([]ProviderStatus(nil), providers...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Type < ps[j].Type })

	ss := make([]Stack, len(stacks))
	for i, s := range stacks {
		s.Containers = append([]Container(nil), s.Containers...)
		for j := range s.Containers {
			c := &s.Containers[j]
			c.RestartCount = 0
			c.CreatedAt = ""
			c.RepoDigests = sortedCopy(c.RepoDigests)
			c.Notes = sortedCopy(c.Notes)
		}
		sort.Slice(s.Containers, func(a, b int) bool { return s.Containers[a].IdentityKey < s.Containers[b].IdentityKey })
		ss[i] = s
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].Fingerprint < ss[j].Fingerprint })

	// encoding/json writes struct fields in declaration order and map keys
	// sorted, so this encoding is canonical for a given set of types.
	b, err := json.Marshal(struct {
		Providers []ProviderStatus `json:"providers"`
		Stacks    []Stack          `json:"stacks"`
	}{ps, ss})
	if err != nil {
		// Only unsupported types fail to marshal; these types have none.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortedCopy(s []string) []string {
	if s == nil {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
