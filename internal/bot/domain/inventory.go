// Package domain holds the bot's pure business rules. It
// imports no I/O packages.
package domain

import (
	"sort"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// Host approval states.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
)

// Host connectivity.
const (
	Online  = "online"
	Offline = "offline"
)

// Presence of stacks and containers.
const (
	Present = "present"
	Missing = "missing"
)

// SupportedArches are the architectures a host may be approved with, as
// Docker names them.
const SupportedArches = "amd64, arm64"

// ArchSupported reports whether a host of this architecture may be approved.
func ArchSupported(arch string) bool { return arch == "amd64" || arch == "arm64" }

// PlatformRuns reports whether an image built for one of platforms can run
// where platform (an image's os/arch[/variant]) runs. 64-bit ARM has only one
// variant in use, so only 32-bit ARM compares variants. No platforms means the
// registry did not say, which is not a reason to hold an update back.
func PlatformRuns(platform string, platforms []string) bool {
	if platform == "" || len(platforms) == 0 {
		return true
	}
	want := strings.Split(platform, "/")
	for _, p := range platforms {
		got := strings.Split(p, "/")
		if len(want) < 2 || len(got) < 2 || want[0] != got[0] || want[1] != got[1] {
			continue
		}
		if want[1] != "arm" || len(want) < 3 || len(got) < 3 || want[2] == got[2] {
			return true
		}
	}
	return false
}

// OfflineAfter is how long a host may go without polling before it is
// offline: max(90 s, 3 × the agent's reported poll interval).
func OfflineAfter(pollIntervalSeconds int) time.Duration {
	return max(90*time.Second, 3*time.Duration(pollIntervalSeconds)*time.Second)
}

// IsOffline reports whether a host last seen at lastSeen is offline at now.
func IsOffline(lastSeen, now time.Time, pollIntervalSeconds int) bool {
	return now.Sub(lastSeen) > OfflineAfter(pollIntervalSeconds)
}

// ExistingStack is a stored, non-retired stack with its containers.
type ExistingStack struct {
	ID          string
	Fingerprint string
	Provider    string
	Presence    string
	Containers  []ExistingContainer
}

// ExistingContainer is a stored container.
type ExistingContainer struct {
	ID          string
	IdentityKey string
	Presence    string
}

// StackChange applies one incoming stack. ExistingID is empty for a new stack.
type StackChange struct {
	ExistingID      string
	Incoming        protocol.Stack
	OldProvider     string // set when ProviderChanged
	ProviderChanged bool
	Reappeared      bool
	Containers      []ContainerChange
	// MissingContainerIDs are present containers of this stack that the
	// snapshot no longer contains.
	MissingContainerIDs []string
}

// ContainerChange applies one incoming container. ExistingID is empty for a
// new container.
type ContainerChange struct {
	ExistingID string
	Incoming   protocol.Container
	Reappeared bool
}

// InventoryPlan is everything a full inventory snapshot changes.
type InventoryPlan struct {
	Stacks []StackChange
	// MissingStackIDs are present stacks absent from the snapshot. Their
	// present containers become missing too.
	MissingStackIDs []string
}

// PlanInventory matches a full snapshot against stored state by identity —
// stacks by fingerprint, containers by identity key, never by Docker ID
// . Stored objects absent from the snapshot become missing;
// missing objects that reappear become present again. A provider
// change keeps the stack's identity.
func PlanInventory(existing []ExistingStack, incoming []protocol.Stack) InventoryPlan {
	byFP := make(map[string]ExistingStack, len(existing))
	for _, s := range existing {
		byFP[s.Fingerprint] = s
	}
	var plan InventoryPlan
	seen := map[string]bool{}
	for _, in := range incoming {
		if seen[in.Fingerprint] {
			continue // a well-formed snapshot never repeats a fingerprint
		}
		seen[in.Fingerprint] = true
		ch := StackChange{Incoming: in}
		old, ok := byFP[in.Fingerprint]
		var oldContainers map[string]ExistingContainer
		if ok {
			ch.ExistingID = old.ID
			ch.Reappeared = old.Presence == Missing
			if old.Provider != in.Provider {
				ch.ProviderChanged = true
				ch.OldProvider = old.Provider
			}
			oldContainers = make(map[string]ExistingContainer, len(old.Containers))
			for _, c := range old.Containers {
				oldContainers[c.IdentityKey] = c
			}
		}
		seenKeys := map[string]bool{}
		for _, c := range in.Containers {
			if seenKeys[c.IdentityKey] {
				continue
			}
			seenKeys[c.IdentityKey] = true
			cc := ContainerChange{Incoming: c}
			if oc, ok := oldContainers[c.IdentityKey]; ok {
				cc.ExistingID = oc.ID
				cc.Reappeared = oc.Presence == Missing
			}
			ch.Containers = append(ch.Containers, cc)
		}
		for _, oc := range old.Containers {
			if !seenKeys[oc.IdentityKey] && oc.Presence == Present {
				ch.MissingContainerIDs = append(ch.MissingContainerIDs, oc.ID)
			}
		}
		sort.Strings(ch.MissingContainerIDs)
		plan.Stacks = append(plan.Stacks, ch)
	}
	for _, s := range existing {
		if !seen[s.Fingerprint] && s.Presence == Present {
			plan.MissingStackIDs = append(plan.MissingStackIDs, s.ID)
		}
	}
	sort.Strings(plan.MissingStackIDs)
	return plan
}

// SharesServices reports whether newKeys shares at least threshold of its
// service names with oldKeys (the merge-suggestion rule), and how
// many matched.
func SharesServices(newKeys, oldKeys []string, threshold float64) (int, bool) {
	if len(newKeys) == 0 {
		return 0, false
	}
	old := map[string]bool{}
	for _, k := range oldKeys {
		old[k] = true
	}
	n := 0
	for _, k := range newKeys {
		if old[k] {
			n++
		}
	}
	return n, float64(n)/float64(len(newKeys)) >= threshold
}
