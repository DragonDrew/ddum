package client

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// stepCollector returns a new snapshot on each collection.
type stepCollector struct{ n int }

func (s *stepCollector) Collect(context.Context) (discovery.Snapshot, error) {
	s.n++
	return discovery.Snapshot{Hash: "sha256:" + strconv.Itoa(s.n), At: t0, Stacks: []protocol.Stack{}}, nil
}

type finishedOps struct{}

func (finishedOps) Active() []protocol.ActiveOperation   { return nil }
func (finishedOps) Handle([]protocol.Command)            {}
func (finishedOps) Pending() map[string][]protocol.Event { return nil }
func (finishedOps) Acked(string, int)                    {}
func (finishedOps) TakeInventoryRequest() bool           { return true }

// The inventory after an operation must show its result: this poll's snapshot
// may predate it, and would look like the container drifted back.
func TestInventoryAfterAnOperationIsFresh(t *testing.T) {
	var sent []string
	bot := &fakeBot{handle: func(path string, body []byte) (int, any) {
		if path == protocol.PathInventory {
			var inv protocol.Inventory
			json.Unmarshal(body, &inv)
			sent = append(sent, inv.InventoryHash)
		}
		return 200, protocol.PollResponse{}
	}}
	l, _ := newLoop(t, bot)
	l.Collector = &stepCollector{}
	l.Ops = finishedOps{}
	l.Step(context.Background())
	if len(sent) != 1 || sent[0] != "sha256:2" {
		t.Fatalf("inventories sent = %v, want the one collected after the operation", sent)
	}
}
