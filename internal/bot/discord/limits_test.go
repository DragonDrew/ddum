package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// checkLimits fails if Discord would refuse the message.
func checkLimits(t *testing.T, m Message) {
	t.Helper()
	if len(m.Content) > 2000 {
		t.Errorf("content = %d", len(m.Content))
	}
	total := 0
	for _, e := range m.Embeds {
		if len(e.Title) > 256 || len(e.Description) > 4096 || len(e.Fields) > 25 {
			t.Errorf("title %d, description %d, fields %d", len(e.Title), len(e.Description), len(e.Fields))
		}
		total += len(e.Title) + len(e.Description)
		if e.Footer != nil {
			total += len(e.Footer.Text)
		}
		if e.Author != nil {
			total += len(e.Author.Name)
		}
		for _, f := range e.Fields {
			if len(f.Name) > 256 || len(f.Value) > 1024 {
				t.Errorf("field %d, %d", len(f.Name), len(f.Value))
			}
			total += len(f.Name) + len(f.Value)
		}
	}
	if total > 6000 {
		t.Errorf("message total = %d", total)
	}
}

// A failed group deploy with many long failure messages, the largest card
// there is: as built, it is over Discord's limits, and /history show would die
// with "interaction failed" exactly when the output is needed most.
func TestLargestOperationCardFitsDiscord(t *testing.T) {
	op := store.Operation{Kind: protocol.CommandDeploy, State: protocol.OpPartiallyFailed, StackName: strings.Repeat("s", 300), HostName: "h"}
	var svcs []protocol.ServiceChange
	res := &protocol.Result{Warnings: []string{strings.Repeat("w", 2000)}}
	for i := range 80 {
		key := strings.Repeat("k", 30) + string(rune('a'+i%26))
		svcs = append(svcs, protocol.ServiceChange{IdentityKey: key,
			Current: protocol.ImageState{ImageRef: "ghcr.io/x/y:1.0.0"}, Desired: protocol.ImageState{ImageRef: "ghcr.io/x/y:1.1.0"}})
		res.Containers = append(res.Containers, protocol.ContainerOutcome{IdentityKey: key, Outcome: protocol.OutcomeFailed, Message: strings.Repeat("m", 300)})
	}
	op.Payload.Targets = []protocol.DeployTarget{{Services: svcs}}
	op.Outcome = store.OperationOutcome{Code: protocol.FailVerify, Message: strings.Repeat("r", 3000), OutputTail: strings.Repeat("o", 60000), Result: res}
	e := operationEmbed(op)
	e.Description += "\n**Output (end):**\n```\n" + lastBytes(op.Outcome.OutputTail, 1500) + "\n```"
	e.Fields = append(e.Fields, field("Events", strings.Repeat("e", 1024), false))
	checkLimits(t, fitLimits(Message{Embeds: []*discordgo.MessageEmbed{e}}))
}

func TestFitLimits(t *testing.T) {
	t.Run("a long refusal, as for a big stack", func(t *testing.T) {
		long := strings.Repeat("• some-container: the container changed after this update was found\n", 200)
		r := ephemeral(long)
		m := fitLimits(r.Message)
		checkLimits(t, m)
		if !strings.HasPrefix(m.Embeds[0].Description, "• some-container") {
			t.Fatal("the start must survive")
		}
	})
	t.Run("too many fields", func(t *testing.T) {
		e := &discordgo.MessageEmbed{}
		for range 40 {
			e.Fields = append(e.Fields, field("n", "v", false))
		}
		checkLimits(t, fitLimits(Message{Embeds: []*discordgo.MessageEmbed{e}}))
	})
	t.Run("the original is not changed", func(t *testing.T) {
		e := &discordgo.MessageEmbed{Title: strings.Repeat("t", 300)}
		fitLimits(Message{Embeds: []*discordgo.MessageEmbed{e}})
		if len(e.Title) != 300 {
			t.Fatal("cards are reused; fitting one must not change it")
		}
	})
	t.Run("a cut code block is closed", func(t *testing.T) {
		got := fitDescription("intro\n```\n"+strings.Repeat("x", 5000)+"\n```", 4096)
		if len(got) > 4096 || !strings.HasSuffix(got, "\n```") {
			t.Fatalf("len %d, end %q", len(got), got[len(got)-8:])
		}
	})
	t.Run("a small card is left alone", func(t *testing.T) {
		e := &discordgo.MessageEmbed{Title: "t", Description: "d", Fields: []*discordgo.MessageEmbedField{{Name: "n", Value: "v"}}}
		got := fitLimits(Message{Embeds: []*discordgo.MessageEmbed{e}, Content: "c"})
		if got.Content != "c" || got.Embeds[0].Description != "d" || len(got.Embeds[0].Fields) != 1 {
			t.Fatalf("got %+v", got.Embeds[0])
		}
	})
}
