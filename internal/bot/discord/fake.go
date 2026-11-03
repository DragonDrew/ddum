package discord

import (
	"context"
	"fmt"
	"sync"
)

// FakeGateway records messages instead of sending them.
type FakeGateway struct {
	mu     sync.Mutex
	n      int
	Sent   []SentMessage
	Edits  []SentMessage
	SendFn func(channelID string, m Message) error // optional failure injection
	// Prefix makes message IDs unique across restarts (the rig).
	Prefix string
}

// SentMessage is one recorded send or edit.
type SentMessage struct {
	ChannelID string
	MessageID string
	Message   Message
}

var _ Gateway = (*FakeGateway)(nil)

// Send implements Gateway.
func (f *FakeGateway) Send(_ context.Context, channelID string, m Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SendFn != nil {
		if err := f.SendFn(channelID, m); err != nil {
			return "", err
		}
	}
	f.n++
	id := fmt.Sprintf("%smsg-%d", f.Prefix, f.n)
	f.Sent = append(f.Sent, SentMessage{channelID, id, m})
	return id, nil
}

// Edit implements Gateway.
func (f *FakeGateway) Edit(_ context.Context, channelID, messageID string, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Edits = append(f.Edits, SentMessage{channelID, messageID, m})
	return nil
}

// SentCount returns the number of messages sent so far.
func (f *FakeGateway) SentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Sent)
}

// Last returns the most recently sent message.
func (f *FakeGateway) Last() SentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Sent) == 0 {
		return SentMessage{}
	}
	return f.Sent[len(f.Sent)-1]
}
