package discord

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/scheduler"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/buildinfo"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

func (r *Router) resolveStack(ctx context.Context, value string) (store.Stack, *Response) {
	if id, err := ids.Parse(value); err == nil {
		if s, err := r.Store.Stack(ctx, id); err == nil {
			return s, nil
		}
	}
	stacks, err := r.Store.Stacks(ctx, store.StackFilter{IncludeMissing: true})
	if err != nil {
		resp := r.internalError("list stacks", err)
		return store.Stack{}, &resp
	}
	var match []store.Stack
	for _, s := range stacks {
		if strings.EqualFold(s.DisplayName, value) {
			match = append(match, s)
		}
	}
	if len(match) == 1 {
		return match[0], nil
	}
	resp := ephemeral("No single stack matches " + code(value) + ". Pick one from the list as you type.")
	return store.Stack{}, &resp
}

func (r *Router) resolveContainer(ctx context.Context, value string) (store.Container, *Response) {
	if id, err := ids.Parse(value); err == nil {
		if c, err := r.Store.Container(ctx, id); err == nil {
			return c, nil
		}
	}
	resp := ephemeral("No container matches " + code(value) + ". Pick one from the list as you type.")
	return store.Container{}, &resp
}

func (r *Router) stackList(ctx context.Context, it Interaction) Response {
	f := store.StackFilter{IncludeMissing: it.Options[optIncludeMissing] == "true"}
	title := "Stacks"
	if v := it.Options[optHost]; v != "" {
		h, errResp := r.resolveHost(ctx, v)
		if errResp != nil {
			return *errResp
		}
		f.HostID = h.ID
		title += " on " + h.DisplayName
	}
	stacks, err := r.Store.Stacks(ctx, f)
	if err != nil {
		return r.internalError("list stacks", err)
	}
	if len(stacks) == 0 {
		return replyCard(iconUpdates, "No stacks found", "Stacks appear once an approved host's agent reports them.", colorMuted)
	}
	var ls []string
	for _, s := range stacks {
		l := fmt.Sprintf("**%s** · %s · %s · %d containers", escape(s.DisplayName), escape(s.HostName), s.Provider, s.ContainerCount)
		if s.Presence == domain.Missing {
			l += " · *missing since " + relTime(s.MissingSince) + "*"
		}
		ls = append(ls, l)
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{{
		Title:       fmt.Sprintf("%s %s · %d", iconUpdates, title, len(stacks)),
		Description: lines(ls, maxDescription, "Narrow it with the host option."),
		Color:       colorInfo,
	}}})
}

func (r *Router) stackShow(ctx context.Context, it Interaction) Response {
	s, errResp := r.resolveStack(ctx, it.Options[optStack])
	if errResp != nil {
		return *errResp
	}
	h, err := r.Store.Host(ctx, s.HostID)
	if err != nil {
		return r.internalError("load host", err)
	}
	containers, err := r.Store.Containers(ctx, store.ContainerFilter{StackID: s.ID, IncludeMissing: s.Presence == domain.Missing})
	if err != nil {
		return r.internalError("list containers", err)
	}
	var cl []string
	for _, c := range containers {
		cl = append(cl, containerLine(c))
	}
	deploy := "supported"
	if !s.DeploySupport {
		deploy = "not supported: " + s.DeployReason
	}
	e := &discordgo.MessageEmbed{
		Title:       iconUpdates + " " + escape(s.DisplayName),
		Description: fmt.Sprintf("on **%s** · %s\n%s", escape(s.HostName), hostState(h), inventoryAge(h)),
		Color:       colorInfo,
		Fields: []*discordgo.MessageEmbedField{
			field("Fingerprint", code(s.Fingerprint), true),
			field("Provider", s.Provider, true),
			field("Deployment", deploy, false),
		},
	}
	if s.Compose != nil {
		files := strings.Join(s.Compose.ConfigFiles, "\n")
		if s.Compose.EnvironmentFile != "" {
			files += "\nenv: " + s.Compose.EnvironmentFile
		}
		e.Fields = append(e.Fields, field("Compose files", files+"\ndir: "+s.Compose.WorkingDir, false))
	}
	if s.Presence == domain.Missing {
		e.Color = colorMuted
		e.Fields = append(e.Fields, field("Missing", "Not seen since "+relTime(s.MissingSince), false))
	}
	e.Fields = append(e.Fields, field(fmt.Sprintf("Containers (%d)", len(containers)),
		lines(cl, maxFieldValue, "Use /container list."), false))
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}})
}

func containerLine(c store.Container) string {
	l := fmt.Sprintf("**%s** · %s · %s", escape(c.DisplayName), code(c.ImageRef), c.RuntimeState)
	if c.Health != "" && c.Health != "none" {
		l += " (" + c.Health + ")"
	}
	if c.Protected {
		l += " · protected"
	}
	if c.Self != "" {
		l += " · ddum " + c.Self
	}
	if c.Presence == domain.Missing {
		l += " · *missing*"
	}
	return l
}

func (r *Router) stackRename(ctx context.Context, it Interaction) Response {
	s, errResp := r.resolveStack(ctx, it.Options[optStack])
	if errResp != nil {
		return *errResp
	}
	name := strings.TrimSpace(it.Options[optName])
	if name == "" {
		return ephemeral("The name cannot be empty.")
	}
	if _, err := r.Store.RenameStack(ctx, s.ID, name, r.actor(it), r.Now()); err != nil {
		return r.internalError("rename stack", err)
	}
	return replyCard(iconUpdates, "Stack renamed", fmt.Sprintf("**%s** is now **%s**.", escape(s.DisplayName), escape(name)), colorOK)
}

func (r *Router) containerList(ctx context.Context, it Interaction) Response {
	f := store.ContainerFilter{IncludeMissing: it.Options[optIncludeMissing] == "true"}
	title := "Containers"
	if v := it.Options[optHost]; v != "" {
		h, errResp := r.resolveHost(ctx, v)
		if errResp != nil {
			return *errResp
		}
		f.HostID = h.ID
		title += " on " + h.DisplayName
	}
	if v := it.Options[optStack]; v != "" {
		s, errResp := r.resolveStack(ctx, v)
		if errResp != nil {
			return *errResp
		}
		f.StackID = s.ID
		title += " in " + s.DisplayName
	}
	cs, err := r.Store.Containers(ctx, f)
	if err != nil {
		return r.internalError("list containers", err)
	}
	if len(cs) == 0 {
		return replyCard(iconUpdates, "No containers found", "", colorMuted)
	}
	var ls []string
	for _, c := range cs {
		ls = append(ls, containerLine(c)+" · "+escape(c.StackName)+" @ "+escape(c.HostName))
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{{
		Title:       fmt.Sprintf("🧱 %s · %d", title, len(cs)),
		Description: lines(ls, maxDescription, "Narrow it with the host or stack option."),
		Color:       colorInfo,
	}}})
}

func (r *Router) containerShow(ctx context.Context, it Interaction) Response {
	c, errResp := r.resolveContainer(ctx, it.Options[optContainer])
	if errResp != nil {
		return *errResp
	}
	h, err := r.Store.Host(ctx, c.HostID)
	if err != nil {
		return r.internalError("load host", err)
	}
	var digests []string
	for _, d := range c.RunningDigests {
		digests = append(digests, code(d))
	}
	var labels []string
	for k, v := range c.DdumLabels {
		labels = append(labels, k+"="+v)
	}
	sort.Strings(labels)
	state := c.RuntimeState
	if c.Health != "" && c.Health != "none" {
		state += " (" + c.Health + ")"
	}
	e := &discordgo.MessageEmbed{
		Title:       "🧱 " + escape(c.DisplayName),
		Description: fmt.Sprintf("in **%s** on **%s**\n%s", escape(c.StackName), escape(c.HostName), inventoryAge(h)),
		Color:       colorInfo,
		Fields: []*discordgo.MessageEmbedField{
			field("Identity key", code(c.IdentityKey), true),
			field("Docker name", code(c.DockerName), true),
			field("Docker ID", code(shortID(c.DockerID)), true),
			field("Image", code(c.ImageRef), false),
			field("Repository", code(c.ImageRepository), true),
			field("Platform", c.Platform, true),
			field("State", state, true),
			field("Running digests", strings.Join(digests, "\n"), false),
			field("Protected", yesNo(c.Protected), true),
			field("ddum role", c.Self, true),
			field("ddum labels", code(strings.Join(labels, ", ")), false),
		},
	}
	if policies, err := r.Store.Policies(ctx); err == nil {
		eff := domain.ResolvePolicy(store.Chain(policies, c.HostID, c.StackID, c.GroupID, c.ID), domain.LabelIgnore(c.DdumLabels))
		e.Fields = append(e.Fields, field("Effective policy", fmt.Sprintf("mode **%s** (%s) · tag_track **%s** (%s)",
			eff.Mode, eff.Source[domain.FieldMode], eff.TagTrack, eff.Source[domain.FieldTagTrack]), false))
	}
	tracking := c.Tracking
	if c.LastCheckAt.IsZero() {
		tracking = "not checked yet"
	} else if c.UntrackedReason != "" {
		tracking += ": " + c.UntrackedReason
	}
	check := relTime(c.LastCheckAt)
	if c.LastCheckError != "" {
		check += "\nfailed: " + c.LastCheckError
	}
	e.Fields = append(e.Fields, field("Tracking", tracking, true), field("Last registry check", check, true))
	if c.NewerOutsideTrack != "" {
		e.Fields = append(e.Fields, field("Newer, outside tag_track", code(c.NewerOutsideTrack)+" exists. To be offered it, change tag_track with /policy set, or update by hand.", false))
	}
	if tc := c.TagChange; tc != nil {
		src := "not supported: " + tc.Reason
		if tc.Supported {
			src = tc.Source
			if tc.Variable != "" {
				src += " (" + tc.Variable + ")"
			}
		}
		e.Fields = append(e.Fields, field("Tag changes", src, false))
	}
	if len(c.Notes) > 0 {
		e.Fields = append(e.Fields, field("Notes", strings.Join(c.Notes, ", "), false))
	}
	if c.Presence == domain.Missing {
		e.Color = colorMuted
		e.Fields = append(e.Fields, field("Missing", "Last seen "+relTime(c.LastSeenAt), false))
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{e}})
}

func (r *Router) systemStatus(ctx context.Context) Response {
	hosts, err := r.Store.Hosts(ctx, false)
	if err != nil {
		return r.internalError("list hosts", err)
	}
	var online, offline, pending int
	for _, h := range hosts {
		switch {
		case h.Approval == domain.ApprovalPending:
			pending++
		case h.Connectivity == domain.Online:
			online++
		default:
			offline++
		}
	}
	maint, err := r.Store.Maintenance(ctx)
	if err != nil {
		return r.internalError("read maintenance setting", err)
	}
	botKey := "not created yet"
	if priv, err := r.Store.BotSigningKey(ctx); err == nil {
		botKey = code(protocol.KeyFingerprint(protocol.EncodePublicKey(priv)))
	}
	lastCheck := "never"
	if last, found, err := scheduler.LastCheck(ctx, r.Store); err == nil && found {
		lastCheck = fmt.Sprintf("%s: %d images checked, %d new updates, %d errors",
			relTime(last.Finished), last.Checked, last.NewUpdates, last.Errors)
		if last.RateLimits > 0 {
			lastCheck += fmt.Sprintf(" (%d rate limited)", last.RateLimits)
		}
		requests := 0
		for _, n := range last.Requests {
			requests += n
		}
		if requests > 0 {
			lastCheck += fmt.Sprintf(", %d registry requests", requests)
		}
	}
	version := r.Version
	if version == "" {
		version = buildinfo.Version
	}
	uptime := r.Now().Sub(r.Started).Round(time.Second)
	return reply(Message{Embeds: []*discordgo.MessageEmbed{{
		Title: iconBot + " ddum status",
		Color: colorInfo,
		Fields: []*discordgo.MessageEmbedField{
			field("Bot version", version, true),
			field("Uptime", uptime.String(), true),
			field("Hosts", fmt.Sprintf("%d online · %d offline · %d pending approval", online, offline, pending), false),
			field("Last registry check", lastCheck, false),
			field("Maintenance mode", map[bool]string{true: "on", false: "off"}[maint], true),
			field("Bot key", botKey, true),
		},
	}}})
}

// autocomplete returns at most 25 choices whose names contain what the user
// typed. Values are UUIDs; names are human-readable.
func (r *Router) autocomplete(ctx context.Context, it Interaction) []Choice {
	typed := strings.ToLower(strings.TrimSpace(it.Options[it.Focused]))
	var out []Choice
	add := func(name, value string) bool {
		if typed != "" && !strings.Contains(strings.ToLower(name), typed) {
			return true
		}
		out = append(out, Choice{Name: truncate(name, maxChoiceName), Value: value})
		return len(out) < maxChoices
	}
	switch it.Focused {
	case optTarget:
		// /policy target: the list depends on the chosen level.
		switch level := it.Options[optScope]; level {
		case optHost, optStack, optContainer, optGroup:
			inner := it
			inner.Options = map[string]string{level: it.Options[optTarget]}
			inner.Focused, inner.Command, inner.Subcommand = level, "", ""
			return r.autocomplete(ctx, inner)
		}
		return nil
	case optGroup:
		groups, err := r.Store.Groups(ctx)
		if err != nil {
			r.Log.Error("autocomplete groups", "err", err)
			return nil
		}
		for _, g := range groups {
			if !add(fmt.Sprintf("%s · %d members", g.DisplayName, g.Members), g.ID) {
				break
			}
		}
	case optOperation:
		ops, err := r.Store.Operations(ctx, store.OperationFilter{Limit: 50})
		if err != nil {
			r.Log.Error("autocomplete operations", "err", err)
			return nil
		}
		for _, op := range ops {
			if !add(opLabel(op), op.ID) {
				break
			}
		}
	case optUpdate:
		pending, err := loadLines(ctx, r.Store, store.UpdateFilter{}, r.Now())
		if err != nil {
			r.Log.Error("autocomplete updates", "err", err)
			return nil
		}
		onlySnoozed := it.Subcommand == "unsnooze"
		for _, l := range pending {
			if onlySnoozed != l.U.Snoozed() && (onlySnoozed || it.Subcommand == "snooze") {
				continue
			}
			name := fmt.Sprintf("%s · %s · %s: %s → %s", l.C.DisplayName, l.U.StackName, l.U.HostName,
				tagOf(l.U.CurrentRef), tagOf(l.U.TargetRef))
			if !add(name, l.U.ID) {
				break
			}
		}
	case optHost:
		hosts, err := r.Store.Hosts(ctx, false)
		if err != nil {
			r.Log.Error("autocomplete hosts", "err", err)
			return nil
		}
		for _, h := range hosts {
			if it.Command == "host" && it.Subcommand == "approve" && h.Approval != domain.ApprovalPending {
				continue
			}
			label := h.DisplayName
			if h.Approval == domain.ApprovalPending {
				label += " (pending)"
			}
			if !add(label+" · "+shortID(h.AgentID), h.ID) {
				break
			}
		}
	case optSource, optInto:
		// Merging is mostly about missing stacks, so they are offered too.
		stacks, err := r.Store.Stacks(ctx, store.StackFilter{IncludeMissing: true})
		if err != nil {
			return nil
		}
		for _, s := range stacks {
			label := s.DisplayName + " · " + s.HostName
			if s.Presence == "missing" {
				label += " (missing)"
			}
			if !add(label, s.ID) {
				break
			}
		}
	case optStack:
		f := store.StackFilter{}
		if hv := it.Options[optHost]; hv != "" {
			if _, err := ids.Parse(hv); err == nil {
				f.HostID = hv
			}
		}
		stacks, err := r.Store.Stacks(ctx, f)
		if err != nil {
			r.Log.Error("autocomplete stacks", "err", err)
			return nil
		}
		for _, s := range stacks {
			if !add(s.DisplayName+" · "+s.HostName, s.ID) {
				break
			}
		}
	case optContainer:
		cs, err := r.Store.Containers(ctx, store.ContainerFilter{})
		if err != nil {
			r.Log.Error("autocomplete containers", "err", err)
			return nil
		}
		for _, c := range cs {
			if !add(c.DisplayName+" · "+c.StackName+" · "+c.HostName, c.ID) {
				break
			}
		}
	}
	return out
}
