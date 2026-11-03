package discord

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Embeds have no tables, so lists are rendered as aligned columns in a code
// block, which keeps them lined up on desktop and phones alike. Cells are
// plain text (no Markdown or emoji, which would break the alignment), and
// columns are cut to fixed widths so a row fits a phone screen.

// column is one table column: its heading, its widest cell, and whether
// it is right-aligned (numbers).
type column struct {
	head  string
	max   int
	right bool
}

// table renders rows under cols in a code block of at most limit
// characters, ending with "… and N more" when rows had to be left out.
func table(cols []column, rows [][]string, limit int) string {
	widths := make([]int, len(cols))
	cut := make([][]string, len(rows))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c.head)
	}
	for r, row := range rows {
		cut[r] = make([]string, len(cols))
		for i := range cols {
			cell := ""
			if i < len(row) {
				cell = cellText(row[i], cols[i].max)
			}
			cut[r][i] = cell
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	line := func(cells []string) string {
		var b strings.Builder
		for i, cell := range cells {
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell))
			if cols[i].right {
				b.WriteString(pad + cell)
			} else {
				b.WriteString(cell + pad)
			}
			if i < len(cells)-1 {
				b.WriteString("  ")
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	heads := make([]string, len(cols))
	for i, c := range cols {
		heads[i] = c.head
	}
	out := []string{line(heads)}
	used := len("```\n") + len(out[0]) + len("\n```") + 24 // room for the "more" line
	shown := 0
	for _, row := range cut {
		l := line(row)
		if used+len(l)+1 > limit {
			break
		}
		out = append(out, l)
		used += len(l) + 1
		shown++
	}
	if shown < len(rows) {
		out = append(out, fmt.Sprintf("… and %d more", len(rows)-shown))
	}
	return "```\n" + strings.Join(out, "\n") + "\n```"
}

// linkText makes s safe as the text of a Markdown link.
func linkText(s string) string {
	return strings.NewReplacer("[", "(", "]", ")", "\n", " ").Replace(s)
}

// cellText makes s safe and short for a table cell.
func cellText(s string, max int) string {
	s = strings.NewReplacer("`", "'", "\n", " ").Replace(s)
	if max > 0 && utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = string(r[:max-1]) + "…"
	}
	return s
}

// stateWord is a displayed state as one short word for a table.
func stateWord(l updateLine) string {
	switch {
	case !l.U.ScheduledFor.IsZero():
		return "Scheduled"
	case l.U.Snoozed():
		return "Snoozed"
	case l.State == domain.StateAutomatic:
		return "Auto"
	case l.State == domain.StatePending:
		return "Pending"
	}
	return stateLabel(l.State)
}

// headlineWord is a scope's headline as one short word.
func headlineWord(state string) string {
	switch state {
	case domain.StateAutomatic:
		return "Auto"
	case domain.StatePending:
		return "Pending"
	}
	return stateLabel(state)
}

// fromTo is a change as two table cells: the tags, or "new build" when the
// tag stays the same.
func fromTo(currentRef, targetRef string) (string, string) {
	from, to := tagOf(currentRef), tagOf(targetRef)
	if from == to {
		return from, "new build"
	}
	return from, to
}

var updateColumns = []column{{head: "CONTAINER", max: 16}, {head: "FROM", max: 10}, {head: "TO", max: 10}, {head: "STATE", max: 9}}

// updatesTable lists a scope's updates.
func updatesTable(v scopeView, limit int) string {
	var rows [][]string
	for _, l := range v.Lines {
		name := l.C.DisplayName
		if v.Kind == "group" {
			name += " (" + l.U.StackName + ")"
		}
		from, to := fromTo(l.U.CurrentRef, l.U.TargetRef)
		rows = append(rows, []string{name, from, to, stateWord(l)})
	}
	states := map[string]bool{}
	for _, r := range rows {
		states[r[3]] = true
	}
	if len(states) == 1 {
		for i := range rows {
			rows[i] = rows[i][:3]
		}
		return table(updateColumns[:3], rows, limit)
	}
	return table(updateColumns, rows, limit)
}

// lineNotes are the details a table cannot hold, one line each: why
// something is held, until when it is snoozed, when it deploys, and
// non-default policies.
func lineNotes(v scopeView) []string {
	var out []string
	for _, l := range v.Lines {
		name := "**" + escape(l.C.DisplayName) + "**"
		switch {
		case l.Reason != "":
			out = append(out, iconHeld+" "+name+" · "+l.Reason)
		case !l.U.ScheduledFor.IsZero():
			out = append(out, iconClock+" "+name+" deploys "+discordTime(l.U.ScheduledFor))
		case l.U.Snoozed():
			until := "indefinitely"
			if !l.U.SnoozeUntil.IsZero() {
				until = "until " + relTime(l.U.SnoozeUntil)
			}
			out = append(out, iconSnoozed+" "+name+" snoozed "+until)
		}
		if l.U.UnverifiedOrigin {
			out = append(out, iconWarning+" "+name+"'s running image is not in this repository. If it is a local build, approving replaces it with the registry's image.")
		} else if l.U.AutoFailed && l.Reason == "" {
			out = append(out, iconWarning+" "+name+" failed its automatic deploy, so it waits for approval. A newer release deploys automatically.")
		} else if src := l.Policy.Source[domain.FieldMode]; l.Policy.Mode == domain.ModeAutomatic && src != domain.SourceDefault && src != domain.LevelGlobal {
			out = append(out, iconAutomatic+" "+name+" updates automatically ("+src+" policy)")
		}
		if l.C.NewerOutsideTrack != "" {
			out = append(out, iconNotes+" "+name+": "+code(l.C.NewerOutsideTrack)+" also exists, outside this container's tag_track")
		}
	}
	return out
}

// releaseLinks lists release notes as links: "📝 web · db".
func releaseLinks(v scopeView) string {
	seen := map[string]bool{}
	var links []string
	for _, l := range v.Lines {
		if l.U.ReleaseURL == "" || seen[l.U.ReleaseURL] {
			continue
		}
		seen[l.U.ReleaseURL] = true
		links = append(links, "["+linkText(l.C.DisplayName)+"]("+l.U.ReleaseURL+")")
	}
	if len(links) == 0 {
		return ""
	}
	return iconNotes + " Release notes: " + strings.Join(links, " · ")
}

var scopeColumns = []column{{head: "STACK / GROUP", max: 18}, {head: "#", right: true}, {head: "STATE", max: 9}}

// scopesTable lists stacks and groups with their pending counts.
func scopesTable(views []scopeView, limit int) string {
	var rows [][]string
	for _, v := range views {
		rows = append(rows, []string{v.Name(), fmt.Sprint(len(v.Lines)), headlineWord(v.Headline)})
	}
	return table(scopeColumns, rows, limit)
}

// outcomeWord is a container's result in an operation.
func outcomeWord(op store.Operation, key string) string {
	if op.Outcome.Result != nil {
		for _, c := range op.Outcome.Result.Containers {
			if c.IdentityKey == key {
				switch c.Outcome {
				case protocol.OutcomeUpdated:
					return "OK"
				case protocol.OutcomeFailed:
					return "Failed"
				case protocol.OutcomeSkipped:
					return "Skipped"
				}
				return c.Outcome
			}
		}
	}
	if op.Terminal() {
		return "-"
	}
	if !op.NotBefore.IsZero() && op.State == protocol.OpQueued {
		return "Scheduled"
	}
	return "Queued"
}

var opColumnsTable = []column{{head: "CONTAINER", max: 16}, {head: "FROM", max: 10}, {head: "TO", max: 10}, {head: "RESULT", max: 9}}

// operationTable lists an operation's containers with their result.
func operationTable(op store.Operation, limit int) string {
	var rows [][]string
	for _, t := range op.Payload.Targets {
		for _, s := range t.Services {
			from, to := fromTo(s.Current.ImageRef, s.Desired.ImageRef)
			rows = append(rows, []string{s.IdentityKey, from, to, outcomeWord(op, s.IdentityKey)})
		}
	}
	return table(opColumnsTable, rows, limit)
}

// opIcon is an operation's state as one icon, for lists.
func opIcon(op store.Operation) string {
	switch {
	case op.Terminal():
		return strings.SplitN(operationTitle(op), " ", 2)[0]
	case op.State == protocol.OpQueued && !op.NotBefore.IsZero():
		return iconClock
	case op.State == protocol.OpQueued:
		return "⏳"
	}
	return iconDeploying
}
