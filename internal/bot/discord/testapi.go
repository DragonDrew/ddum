//go:build rig

package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// LoggingFake is the gateway of DISCORD_MODE=fake: a FakeGateway that also
// logs each message, for the end-to-end rig.
type LoggingFake struct {
	FakeGateway
	Log *slog.Logger
}

// Edit implements Gateway.
func (f *LoggingFake) Edit(ctx context.Context, channelID, messageID string, m Message) error {
	err := f.FakeGateway.Edit(ctx, channelID, messageID, m)
	f.Log.Info("fake discord: edit", "message_id", messageID, "text", flatten(m))
	return err
}

// Send implements Gateway.
func (f *LoggingFake) Send(ctx context.Context, channelID string, m Message) (string, error) {
	id, err := f.FakeGateway.Send(ctx, channelID, m)
	f.Log.Info("fake discord: message", "message_id", id, "text", flatten(m))
	return id, err
}

// TestMessage is a sent message as the test API reports it.
type TestMessage struct {
	MessageID string   `json:"message_id"`
	Text      string   `json:"text"`
	CustomIDs []string `json:"custom_ids"`
	Edited    bool     `json:"edited"`
	// Options maps each select menu option's label to its value.
	Options map[string]string `json:"options,omitempty"`
}

// TestResponse is an interaction response as the test API reports it.
type TestResponse struct {
	Kind int `json:"kind"`
	// MessageID is the channel message the response is shown on: the card a
	// button belongs to, or a new public card. Empty for private replies.
	MessageID string   `json:"message_id"`
	Text      string   `json:"text"`
	CustomIDs []string `json:"custom_ids"`
	Values    []string `json:"values"` // select menu option values
}

// TestHandler serves /test/v1/interaction and /test/v1/messages for the
// rig. Requests must be signed with the agent key exactly like agent
// requests. It exists only in DISCORD_MODE=fake.
func TestHandler(r *Router, gw *LoggingFake, key []byte, now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	verify := func(w http.ResponseWriter, req *http.Request) ([]byte, bool) {
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, protocol.MaxBodyBytes))
		if err != nil || protocol.VerifyHTTPRequest(req, key, body, now()) != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return nil, false
		}
		return body, true
	}
	mux.HandleFunc("POST /test/v1/interaction", func(w http.ResponseWriter, req *http.Request) {
		body, ok := verify(w, req)
		if !ok {
			return
		}
		var it Interaction
		if err := json.Unmarshal(body, &it); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := r.Handle(req.Context(), it)
		out := TestResponse{Kind: int(resp.Kind), Text: flatten(resp.Message), CustomIDs: customIDs(resp.Message)}
		// Do what Discord does with the response: a public reply becomes a
		// channel message, and an update edits the card it came from.
		switch {
		case resp.Kind == RespMessage && !resp.Ephemeral:
			id, _ := gw.Send(req.Context(), it.ChannelID, resp.Message)
			out.MessageID = id
			r.Delivered(req.Context(), resp, it.ChannelID, id)
		case resp.Kind == RespUpdate && it.MessageID != "" && !it.Ephemeral:
			gw.Edit(req.Context(), it.ChannelID, it.MessageID, resp.Message)
			out.MessageID = it.MessageID
		}
		for _, c := range resp.Choices {
			out.Values = append(out.Values, c.Value)
		}
		out.Values = append(out.Values, menuValues(resp.Message)...)
		json.NewEncoder(w).Encode(out)
	})
	// The rig cannot wait an hour for a scheduled deploy, and moving the
	// bot's clock would break request signing, so it brings scheduled
	// operations forward to now instead.
	mux.HandleFunc("POST /test/v1/scheduled/start", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := verify(w, req); !ok {
			return
		}
		n, err := r.Store.BringScheduledForward(req.Context(), now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]int64{"started": n})
	})
	mux.HandleFunc("POST /test/v1/messages", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := verify(w, req); !ok {
			return
		}
		gw.mu.Lock()
		latest := map[string]Message{}
		var editedOnly []string // edits of messages sent before a restart
		sent := map[string]bool{}
		for _, s := range gw.Sent {
			sent[s.MessageID] = true
		}
		for _, e := range gw.Edits {
			if _, ok := latest[e.MessageID]; !ok && !sent[e.MessageID] {
				editedOnly = append(editedOnly, e.MessageID)
			}
			latest[e.MessageID] = e.Message
		}
		var out []TestMessage
		for _, id := range editedOnly {
			m := latest[id]
			out = append(out, TestMessage{MessageID: id, Text: flatten(m), CustomIDs: customIDs(m), Edited: true, Options: menuOptions(m)})
		}
		for _, s := range gw.Sent {
			m, edited := s.Message, false
			if e, ok := latest[s.MessageID]; ok {
				m, edited = e, true
			}
			out = append(out, TestMessage{MessageID: s.MessageID, Text: flatten(m), CustomIDs: customIDs(m), Edited: edited, Options: menuOptions(m)})
		}
		gw.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	})
	return mux
}

func flatten(m Message) string {
	s := m.Content
	for _, e := range m.Embeds {
		s += "\n" + e.Title + "\n" + e.Description
		for _, f := range e.Fields {
			s += "\n" + f.Name + ": " + f.Value
		}
		if e.Footer != nil {
			s += "\n" + e.Footer.Text
		}
	}
	return s
}

func customIDs(m Message) []string {
	var out []string
	for _, row := range m.Components {
		if ar, ok := row.(discordgo.ActionsRow); ok {
			for _, c := range ar.Components {
				switch b := c.(type) {
				case discordgo.Button:
					if b.CustomID != "" && !b.Disabled {
						out = append(out, b.CustomID)
					}
				case discordgo.SelectMenu:
					out = append(out, b.CustomID)
				}
			}
		}
	}
	return out
}

func menuOptions(m Message) map[string]string {
	out := map[string]string{}
	for _, row := range m.Components {
		if ar, ok := row.(discordgo.ActionsRow); ok {
			for _, c := range ar.Components {
				if sm, ok := c.(discordgo.SelectMenu); ok {
					for _, o := range sm.Options {
						out[o.Label] = o.Value
					}
				}
			}
		}
	}
	return out
}

func menuValues(m Message) []string {
	var out []string
	for _, row := range m.Components {
		if ar, ok := row.(discordgo.ActionsRow); ok {
			for _, c := range ar.Components {
				if sm, ok := c.(discordgo.SelectMenu); ok {
					for _, o := range sm.Options {
						out = append(out, o.Value)
					}
				}
			}
		}
	}
	return out
}
