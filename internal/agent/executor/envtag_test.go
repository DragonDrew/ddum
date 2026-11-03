package executor

import (
	"encoding/json"
	"testing"

	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Comments and quotes in the env file are not part of the tag: reading them as
// such would refuse the deploy as a pending file edit.
func TestEnvFileValueIsReadLikeCompose(t *testing.T) {
	const compose = "services:\n  web:\n    image: nginx:${TAG}\n  db:\n    image: postgres:16\n"
	for _, env := range []string{"TAG=1.27\n", "TAG=1.27 # pinned for now\n", "TAG=\"1.27\"\n", "TAG='1.27' # quoted\n", "export TAG=1.27\n"} {
		t.Run(env, func(t *testing.T) {
			h := newHarness(t, compose, env)
			var p protocol.DeployPayload
			json.Unmarshal(command(ids.V7{}.New(), webChange("1.28", dg("b"))).Payload, &p)
			if _, err := h.x.plan(p); err != nil {
				t.Fatalf("plan: %v", err)
			}
		})
	}
	t.Run("a real pending edit is still noticed", func(t *testing.T) {
		h := newHarness(t, compose, "TAG=1.29\n")
		var p protocol.DeployPayload
		json.Unmarshal(command(ids.V7{}.New(), webChange("1.28", dg("b"))).Payload, &p)
		if _, err := h.x.plan(p); err == nil {
			t.Fatal("expected a refusal")
		}
	})
}
