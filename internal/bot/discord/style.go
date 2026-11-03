package discord

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// The look of every channel message: one icon set, one palette, short
// titles, the host in the author line, details in compact inline fields, and
// IDs only in the footer.

// Icons.
const (
	iconUpdates   = "📦"
	iconHost      = "🖥️"
	iconPending   = "🟡"
	iconAutomatic = "🔵"
	iconSnoozed   = "💤"
	iconHeld      = "⛔"
	iconDeploying = "🚀"
	iconOK        = "✅"
	iconFailed    = "❌"
	iconWarning   = "⚠️"
	iconRollback  = "↩️"
	iconBot       = "🤖"
	iconOffline   = "🔴"
	iconOnline    = "🟢"
	iconClock     = "⏰"
	iconDrift     = "🔀"
	iconExpired   = "⌛"
	iconMaint     = "🛠️"
	iconPublic    = "🔓"
	iconNotes     = "📝"
)

// stateIcon is the icon of a displayed state.
func stateIcon(state string) string {
	switch state {
	case domain.StatePending:
		return iconPending
	case domain.StateAutomatic:
		return iconAutomatic
	case domain.StateSnoozed:
		return iconSnoozed
	case domain.StateHeld:
		return iconHeld
	case domain.StateDeploying:
		return iconDeploying
	}
	return "⚪"
}

// stateLabel is a displayed state as people read it.
func stateLabel(state string) string {
	switch state {
	case domain.StateAutomatic:
		return "Automatic"
	case domain.StateIgnored:
		return "Ignored"
	}
	return state
}

// change renders a tag change as `old` → **`new`**, or the digests when the
// tag stays the same.
func change(currentRef, currentDigest, targetRef, targetDigest string) string {
	from, to := tagOf(currentRef), tagOf(targetRef)
	if from != to {
		return code(from) + " → **" + code(to) + "**"
	}
	return code(from) + " rebuilt · " + code(shortDigest(currentDigest)) + " → **" + code(shortDigest(targetDigest)) + "**"
}

// author puts the host, and optionally the provider, above the title.
func author(host, provider string) *discordgo.MessageEmbedAuthor {
	name := iconHost + " " + host
	if provider != "" {
		name += " · " + providerLabel(provider)
	}
	return &discordgo.MessageEmbedAuthor{Name: truncate(name, 256)}
}

func providerLabel(p string) string {
	switch p {
	case protocol.ProviderCompose:
		return "Compose"
	case protocol.ProviderPortainer:
		return "Portainer"
	case protocol.ProviderDocker:
		return "Docker"
	}
	return p
}

// stamp is an embed timestamp; zero means none.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// footer shows short IDs and similar quiet detail.
func footer(parts ...string) *discordgo.MessageEmbedFooter {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil // Discord rejects an empty footer
	}
	return &discordgo.MessageEmbedFooter{Text: strings.Join(kept, " · ")}
}

// plural renders "1 update" / "3 updates".
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// notice is a one-embed message for system events.
func notice(icon, title, desc string, color int) Message {
	return Message{Embeds: []*discordgo.MessageEmbed{{Title: icon + " " + title, Description: desc, Color: color}}}
}
