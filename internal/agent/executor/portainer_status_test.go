package executor

import (
	"testing"

	"github.com/DragonDrew/ddum/internal/agent/portainer"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// The containers can look right while Portainer says the stack is
// stopped.
func TestInactivePortainerStackFailsVerification(t *testing.T) {
	const file = "services:\n  web:\n    image: nginx:1.27\n"
	h, fp := portainerHarness(t, file, nil, false)
	fp.stack.Status = portainer.StatusInactive
	evs := h.run(command(ids.V7{}.New(), webChange("1.27", dg("b"))))
	if got := last(evs); got.Type != protocol.EventFailed || got.Code != protocol.FailVerify {
		t.Fatalf("events = %s; last = %+v", types(evs), got)
	}
}
