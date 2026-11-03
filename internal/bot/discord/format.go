package discord

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
)

// Discord limits.
const (
	maxDescription = 4096
	maxFieldValue  = 1024
	maxFields      = 25
	maxChoices     = 25
	maxChoiceName  = 100
)

// More Discord limits. A message over any of them is refused outright, which
// would leave a click with "interaction failed" and no explanation.
const (
	maxContent    = 2000
	maxTitle      = 256
	maxFieldName  = 256
	maxFooter     = 2048
	maxAuthorName = 256
	maxEmbedTotal = 6000
)

// fitLimits returns m cut to Discord's limits. The cards aim to stay
// within them, but held reasons, names, and command output have no upper bound
// of their own. Sizes count bytes, which is never less than Discord's count.
func fitLimits(m Message) Message {
	m.Content = truncate(m.Content, maxContent)
	if m.Embeds == nil {
		return m
	}
	budget := maxEmbedTotal
	embeds := make([]*discordgo.MessageEmbed, 0, len(m.Embeds))
	for _, orig := range m.Embeds {
		e := *orig
		e.Title = truncate(e.Title, maxTitle)
		if e.Footer != nil {
			f := *e.Footer
			f.Text = truncate(f.Text, maxFooter)
			e.Footer = &f
		}
		if e.Author != nil {
			a := *e.Author
			a.Name = truncate(a.Name, maxAuthorName)
			e.Author = &a
		}
		size := len(e.Title)
		if e.Footer != nil {
			size += len(e.Footer.Text)
		}
		if e.Author != nil {
			size += len(e.Author.Name)
		}
		fields := make([]*discordgo.MessageEmbedField, 0, len(e.Fields))
		for _, of := range e.Fields {
			if len(fields) == maxFields {
				break
			}
			f := *of
			f.Name, f.Value = truncate(f.Name, maxFieldName), truncate(f.Value, maxFieldValue)
			if size+len(f.Name)+len(f.Value) > budget {
				break // the fields carry the details; the description comes first
			}
			size += len(f.Name) + len(f.Value)
			fields = append(fields, &f)
		}
		e.Fields = fields
		e.Description = fitDescription(e.Description, min(maxDescription, max(budget-size, 0)))
		budget = max(budget-size-len(e.Description), 0)
		embeds = append(embeds, &e)
	}
	m.Embeds = embeds
	return m
}

// fitDescription cuts a description to n bytes and closes a code block the cut
// left open, which would otherwise swallow the rest of the card.
func fitDescription(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const fence = "\n```"
	if n < len(fence) {
		return truncate(s, n)
	}
	cut := truncate(s, n-len(fence))
	if strings.Count(cut, "```")%2 == 1 {
		return cut + fence
	}
	return truncate(s, n)
}

// Embed colours.
const (
	colorInfo    = 0x5865F2
	colorOK      = 0x57F287
	colorWarn    = 0xFEE75C
	colorProblem = 0xED4245
	colorMuted   = 0x99AAB5
)

// relTime renders a timestamp the viewer's client shows as "5 minutes ago",
// in their own time zone. Zero renders as "never".
func relTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return "<t:" + strconv.FormatInt(t.Unix(), 10) + ":R>"
}

// shortDigest shortens "repo@sha256:abcdef…" or "sha256:abcdef…" to 12 hex characters.
func shortDigest(d string) string {
	if i := strings.LastIndex(d, "@"); i >= 0 {
		d = d[i+1:]
	}
	if strings.HasPrefix(d, "sha256:") && len(d) > 19 {
		return d[:19]
	}
	return d
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// truncate cuts s to at most n bytes on a rune boundary, adding "…".
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// lines joins lines into at most limit bytes, ending with a pointer to the
// rest when some do not fit.
func lines(ls []string, limit int, more string) string {
	var b strings.Builder
	for i, l := range ls {
		tail := ""
		if i < len(ls)-1 {
			tail = fmt.Sprintf("\n…and %d more. %s", len(ls)-i-1, more)
		}
		if b.Len()+len(l)+1+len(tail) > limit {
			b.WriteString(fmt.Sprintf("…and %d more. %s", len(ls)-i, more))
			return b.String()
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func field(name, value string, inline bool) *discordgo.MessageEmbedField {
	if value == "" {
		value = "—"
	}
	return &discordgo.MessageEmbedField{Name: name, Value: truncate(value, maxFieldValue), Inline: inline}
}

func code(s string) string {
	if s == "" {
		return "—"
	}
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

// escape neutralizes Markdown in user- or host-supplied names.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "'", "|", `\|`, ">", `\>`, "@", "@\u200b")
	return r.Replace(s)
}

// hostState is a short status like "approved · online".
func hostState(h store.Host) string {
	if h.Removed() {
		return "removed"
	}
	s := h.Approval + " · " + h.Connectivity
	if h.Approval == domain.ApprovalPending {
		s = "**pending approval** · " + h.Connectivity
	}
	if h.Connectivity == domain.Offline {
		s += " (last seen " + relTime(h.LastSeenAt) + ")"
	}
	return s
}

func hostColor(h store.Host) int {
	switch {
	case h.Removed():
		return colorMuted
	case h.Approval == domain.ApprovalPending:
		return colorWarn
	case h.Connectivity == domain.Offline:
		return colorProblem
	}
	return colorOK
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// inventoryAge is the age indicator that must accompany any inventory view
// .
func inventoryAge(h store.Host) string {
	s := "Inventory from " + relTime(h.LastInventoryAt)
	if h.Connectivity == domain.Offline {
		s += " — host offline, data may be stale"
	}
	return s
}
