package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
)

// HostOffline posts one notice when a host stops polling.
func (n *Notifier) HostOffline(ctx context.Context, h store.Host) {
	n.enqueue(ctx, func(ctx context.Context) {
		m := notice(iconOffline, escape(h.DisplayName)+" is offline",
			fmt.Sprintf("No contact since %s. Its updates are held until it is back; its inventory is shown with its age.", relTime(h.LastSeenAt)),
			colorProblem)
		m.Embeds[0].Timestamp = stamp(n.Now())
		n.post(ctx, "host offline", m)
	})
}

// AgentKeyChanged asks whether to accept the key an agent now presents.
func (n *Notifier) AgentKeyChanged(ctx context.Context, h store.Host, fingerprint string) {
	n.enqueue(ctx, func(ctx context.Context) {
		n.post(ctx, "agent key changed", agentKeyMessage(h, fingerprint))
	})
}

// HostOnline posts one notice when an offline host polls again.
func (n *Notifier) HostOnline(ctx context.Context, h store.Host) {
	n.enqueue(ctx, func(ctx context.Context) {
		m := notice(iconOnline, escape(h.DisplayName)+" is back online", "", colorOK)
		m.Embeds[0].Timestamp = stamp(n.Now())
		n.post(ctx, "host online", m)
	})
}

// ClockSkew reports a validly signed request rejected for its timestamp
// . The caller rate-limits it.
func (n *Notifier) ClockSkew(ctx context.Context, agentID, hostName string, skew time.Duration) {
	name := hostName
	if name == "" {
		name = "an agent"
	}
	direction := "ahead of"
	if skew < 0 {
		direction, skew = "behind", -skew
	}
	n.enqueue(ctx, func(ctx context.Context) {
		m := notice(iconClock, "Clock problem on "+escape(name),
			fmt.Sprintf("Its clock is **%s %s** the bot's, so its requests are refused (5 minutes is the limit).\n"+
				"Fix the time on that host, for example by enabling NTP.", skew.Round(time.Second), direction), colorProblem)
		m.Embeds[0].Footer = footer("Shown at most once an hour")
		n.post(ctx, "clock skew", m)
	})
}

// ChecksFailing posts one notice listing containers whose registry checks
// have failed for a day without a break. Each run of failures is
// reported once.
func (n *Notifier) ChecksFailing(ctx context.Context, cs []store.Container) {
	list := append([]store.Container(nil), cs...)
	n.enqueue(ctx, func(ctx context.Context) {
		var ls []string
		auth := false
		for _, c := range list {
			ls = append(ls, fmt.Sprintf("**%s** · %s · %s, first failed %s\n%s", escape(c.DisplayName), escape(c.StackName),
				escape(c.HostName), relTime(c.CheckFailingSince), code(truncate(c.LastCheckError, 300))))
			e := strings.ToLower(c.LastCheckError)
			auth = auth || strings.Contains(e, "denied") || strings.Contains(e, "unauthorized")
		}
		desc := "ddum keeps checking them and posts again when they work.\n\n" + lines(ls, 3600, "")
		if auth {
			desc += "\n\nA registry refused ddum's login. For ghcr.io and lscr.io images, check that `GHCR_TOKEN` " +
				"has not expired and still has `read:packages`."
		}
		m := notice(iconWarning, "Registry checks failing · "+plural(len(list), "image"),
			desc, colorProblem)
		m.Embeds[0].Footer = footer("Failing for a day or more", "reported once")
		n.post(ctx, "checks failing", m)
	})
}

// ChecksRecovered posts one notice listing containers reported by
// ChecksFailing whose checks work again.
func (n *Notifier) ChecksRecovered(ctx context.Context, cs []store.Container) {
	list := append([]store.Container(nil), cs...)
	n.enqueue(ctx, func(ctx context.Context) {
		var ls []string
		for _, c := range list {
			ls = append(ls, fmt.Sprintf("**%s** · %s · %s", escape(c.DisplayName), escape(c.StackName), escape(c.HostName)))
		}
		n.post(ctx, "checks recovered", notice(iconOK,
			"Registry checks working again · "+plural(len(list), "image"),
			lines(ls, 3600, ""), colorOK))
	})
}

// AgentVersionDiffers posts that a host's agent runs a version other than
// the bot's. Agents are protected, so ddum never updates them.
func (n *Notifier) AgentVersionDiffers(ctx context.Context, h store.Host, botVersion string) {
	n.enqueue(ctx, func(ctx context.Context) {
		desc := fmt.Sprintf("Its agent runs %s; the bot runs %s.\n", code(h.AgentVersion), code(botVersion))
		if agentOlder(h.AgentVersion, botVersion) {
			desc += fmt.Sprintf("ddum does not update agents. Set the `ddum-agent` image tag to %s in that host's stack, "+
				"for example in Portainer, and redeploy it.", code(botVersion))
		} else {
			desc += "The agent is newer than the bot. Update the bot to match."
		}
		m := notice(iconHost, "Agent version differs · "+escape(h.DisplayName), desc, colorWarn)
		m.Embeds[0].Footer = footer("Reported once per version")
		n.post(ctx, "agent version", m)
	})
}

// agentOlder reports whether agent is an older version than bot.
func agentOlder(agent, bot string) bool {
	a, okA := domain.ParseVersionTag(agent)
	b, okB := domain.ParseVersionTag(bot)
	return !okA || !okB || domain.CompareVersions(a, b) < 0
}

// OperationsExpired posts one notice per expired operation.
func (n *Notifier) OperationsExpired(ctx context.Context, ops []store.Operation) {
	list := append([]store.Operation(nil), ops...)
	n.enqueue(ctx, func(ctx context.Context) {
		for _, op := range list {
			m := notice(iconExpired, "Deploy expired · "+escape(op.StackName),
				"The agent never picked it up (is it offline?). Its updates are pending again.", colorMuted)
			m.Embeds[0].Author = author(op.HostName, "")
			n.post(ctx, "operation expired", m)
			n.refreshStack(ctx, op.ScopeID)
		}
	})
}

// Drifted posts one notice per container that left its desired state.
func (n *Notifier) Drifted(ctx context.Context, drifts []store.DriftEvent) {
	list := append([]store.DriftEvent(nil), drifts...)
	n.enqueue(ctx, func(ctx context.Context) {
		for _, d := range list {
			m := notice(iconDrift, escape(d.Container.DisplayName)+" changed outside ddum",
				fmt.Sprintf("**%s** now runs %s instead of %s, which ddum last deployed.\nddum accepts the new state; this is reported once.",
					escape(d.Container.StackName), code(shortDigest(d.Observed)), code(shortDigest(d.Expected))), colorWarn)
			m.Embeds[0].Author = author(d.Container.HostName, "")
			n.post(ctx, "drift", m)
		}
	})
}

// AutoQueued posts that the policy engine queued an operation.
func (n *Notifier) AutoQueued(ctx context.Context, res store.ApproveResult) {
	n.enqueue(ctx, func(ctx context.Context) {
		m := queuedCard(res.Operation, "Starts on the agent's next poll.")
		m.Embeds[0].Title = iconAutomatic + " Automatic update queued · " + escape(res.Operation.StackName)
		n.showOperation(ctx, res.Operation, m)
		n.refreshStack(ctx, res.Operation.ScopeID)
	})
}

// RollbackQueued posts that a rollback was queued automatically.
func (n *Notifier) RollbackQueued(ctx context.Context, op store.Operation, auto bool) {
	n.enqueue(ctx, func(ctx context.Context) {
		when := "Starts on the agent's next poll."
		if auto {
			when = "The deploy failed and auto_rollback is on."
		}
		n.showOperation(ctx, op, queuedCard(op, when))
		n.refreshStack(ctx, op.ScopeID)
	})
}

// SnoozeExpired posts one notice per expired snooze.
func (n *Notifier) SnoozeExpired(ctx context.Context, expired []store.ExpiredSnooze) {
	list := append([]store.ExpiredSnooze(nil), expired...)
	n.enqueue(ctx, func(ctx context.Context) {
		stacks := map[string]bool{}
		for _, e := range list {
			u := e.Update
			m := notice(iconClock, "Snooze ended · "+escape(u.ContainerName),
				fmt.Sprintf("**%s** in %s is pending again: %s", escape(u.ContainerName), escape(u.StackName),
					change(u.CurrentRef, u.CurrentDigest, u.TargetRef, u.TargetDigest)), colorWarn)
			m.Embeds[0].Author = author(u.HostName, "")
			n.post(ctx, "snooze expired", m)
			stacks[u.StackID] = true
		}
		for id := range stacks {
			n.refreshStack(ctx, id)
		}
	})
}

// maintenanceNotice announces maintenance mode changes.
func maintenanceNotice(on bool, userID string) Message {
	if on {
		return notice(iconMaint, "Maintenance mode on",
			"Turned on by <@"+userID+">. Update notifications and automatic deploys are paused; checks continue and manual approvals still work.",
			colorWarn)
	}
	return notice(iconMaint, "Maintenance mode off", "Turned off by <@"+userID+">. Notifications and automatic deploys resume.", colorOK)
}

// selfUpdateMessage is the separate message for a bot update.
func selfUpdateMessage(l updateLine, offerID string) Message {
	e := &discordgo.MessageEmbed{
		Title:       iconBot + " ddum bot update available",
		Author:      author(l.U.HostName, ""),
		Description: change(l.U.CurrentRef, l.U.CurrentDigest, l.U.TargetRef, l.U.TargetDigest),
		Color:       colorWarn,
		Timestamp:   stamp(l.U.DetectedAt),
		Footer:      footer("Never automatic", "approving restarts the bot"),
	}
	if l.Reason != "" {
		e.Fields = append(e.Fields, field(iconHeld+" Held", l.Reason, false))
	}
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, Emoji: &discordgo.ComponentEmoji{Name: "✅"},
			CustomID: customID(actSelfUpdate, offerID), Disabled: l.Reason != ""},
	}}
	if l.U.ReleaseURL != "" {
		row.Components = append(row.Components, discordgo.Button{Label: "Release notes", Style: discordgo.LinkButton, URL: l.U.ReleaseURL})
	}
	return Message{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{row}}
}
