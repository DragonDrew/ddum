package discord

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bwmarrin/discordgo"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
)

// policyTarget resolves the scope and target options to a policy key, the
// chain IDs for effective-policy display, a label, and ddum.ignore.
type policyTarget struct {
	key         store.PolicyKey
	hostID      string
	stackID     string
	groupID     string
	containerID string
	label       string
	labelIgnore bool
}

func (r *Router) resolvePolicyTarget(ctx context.Context, it Interaction) (policyTarget, *Response) {
	scope, target := it.Options[optScope], it.Options[optTarget]
	switch scope {
	case domain.LevelGlobal:
		return policyTarget{key: store.PolicyKey{Level: domain.LevelGlobal}, label: "global"}, nil
	case domain.LevelHost:
		h, errResp := r.resolveHost(ctx, target)
		if errResp != nil {
			return policyTarget{}, errResp
		}
		return policyTarget{key: store.PolicyKey{Level: scope, ID: h.ID}, hostID: h.ID, label: "host " + h.DisplayName}, nil
	case domain.LevelStack:
		s, errResp := r.resolveStack(ctx, target)
		if errResp != nil {
			return policyTarget{}, errResp
		}
		return policyTarget{key: store.PolicyKey{Level: scope, ID: s.ID}, hostID: s.HostID, stackID: s.ID,
			label: "stack " + s.DisplayName + " on " + s.HostName}, nil
	case domain.LevelGroup:
		g, errResp := r.resolveGroup(ctx, target)
		if errResp != nil {
			return policyTarget{}, errResp
		}
		// Members inherit from their own host and stack first; shown here is
		// global → group.
		return policyTarget{key: store.PolicyKey{Level: scope, ID: g.ID}, groupID: g.ID, label: "group " + g.DisplayName}, nil
	case domain.LevelContainer:
		c, errResp := r.resolveContainer(ctx, target)
		if errResp != nil {
			return policyTarget{}, errResp
		}
		return policyTarget{key: store.PolicyKey{Level: scope, ID: c.ID}, hostID: c.HostID, stackID: c.StackID, groupID: c.GroupID, containerID: c.ID,
			label: "container " + c.DisplayName + " in " + c.StackName, labelIgnore: domain.LabelIgnore(c.DdumLabels)}, nil
	}
	resp := ephemeral("Choose a level: global, host, stack, group, or container.")
	return policyTarget{}, &resp
}

func (r *Router) effectivePolicyMessage(ctx context.Context, t policyTarget, note string) Response {
	all, err := r.Store.Policies(ctx)
	if err != nil {
		return r.internalError("load policies", err)
	}
	e := domain.ResolvePolicy(store.Chain(all, t.hostID, t.stackID, t.groupID, t.containerID), t.labelIgnore)
	row := func(name, value string) *discordgo.MessageEmbedField {
		return field(name, fmt.Sprintf("**%s**\nset at: %s", value, e.Source[name]), true)
	}
	ar := strconv.FormatBool(e.AutoRollback)
	emb := &discordgo.MessageEmbed{
		Title:       "⚙️ Effective policy · " + t.label,
		Description: note,
		Color:       colorInfo,
		Fields: []*discordgo.MessageEmbedField{
			row(domain.FieldMode, e.Mode),
			row(domain.FieldTagTrack, e.TagTrack),
			row(domain.FieldAutoRollback, ar),
			row(domain.FieldVerifyTimeout, strconv.Itoa(e.VerifyTimeout)+" s"),
			row(domain.FieldVerifyStable, strconv.Itoa(e.VerifyStable)+" s"),
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "Each field is inherited from the most specific level that sets it: global → host → stack → group → container."},
	}
	return reply(Message{Embeds: []*discordgo.MessageEmbed{emb}})
}

func (r *Router) policyShow(ctx context.Context, it Interaction) Response {
	t, errResp := r.resolvePolicyTarget(ctx, it)
	if errResp != nil {
		return *errResp
	}
	return r.effectivePolicyMessage(ctx, t, "")
}

func (r *Router) policySet(ctx context.Context, it Interaction) Response {
	t, errResp := r.resolvePolicyTarget(ctx, it)
	if errResp != nil {
		return *errResp
	}
	var f domain.PolicyFields
	if v := it.Options[optMode]; v != "" {
		f.Mode = &v
	}
	if v := it.Options[optTagTrack]; v != "" {
		f.TagTrack = &v
	}
	if v := it.Options[optAutoRollback]; v != "" {
		b := v == "true"
		f.AutoRollback = &b
	}
	for _, o := range []struct {
		name     string
		dst      **int
		min, max int
	}{{optVerifyTimeout, &f.VerifyTimeout, 10, 1800}, {optVerifyStable, &f.VerifyStable, 0, 600}} {
		v := it.Options[o.name]
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < o.min || n > o.max {
			return ephemeral(fmt.Sprintf("%s must be %d–%d.", o.name, o.min, o.max))
		}
		*o.dst = &n
	}
	if f == (domain.PolicyFields{}) {
		return ephemeral("Give at least one field to set: mode, tag_track, auto_rollback, verify_timeout_seconds, or verify_stable_seconds.")
	}
	if err := r.Store.SetPolicy(ctx, t.key, f, r.actor(it), r.Now()); err != nil {
		return r.internalError("set policy", err)
	}
	note := "Saved. It applies from the next registry check."
	if t.labelIgnore {
		note += " This container has the label ddum.ignore=true, which keeps it ignored whatever its policy says."
	}
	return r.effectivePolicyMessage(ctx, t, note)
}

func (r *Router) policyClear(ctx context.Context, it Interaction) Response {
	t, errResp := r.resolvePolicyTarget(ctx, it)
	if errResp != nil {
		return *errResp
	}
	fieldName := it.Options[optField]
	ok, err := r.Store.ClearPolicy(ctx, t.key, fieldName, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("clear policy", err)
	}
	if !ok {
		return ephemeral("Nothing is set at " + t.label + ", so there is nothing to clear.")
	}
	what := "All fields"
	if fieldName != "" {
		what = fieldName
	}
	note := what + " cleared; they now inherit."
	if t.key.Level == domain.LevelGlobal {
		note = what + " reset to the built-in defaults."
	}
	return r.effectivePolicyMessage(ctx, t, note)
}

func (r *Router) maintenance(ctx context.Context, it Interaction) Response {
	switch it.Subcommand {
	case "status":
		on, err := r.Store.Maintenance(ctx)
		if err != nil {
			return r.internalError("read maintenance mode", err)
		}
		if on {
			return replyCard(iconMaint, "Maintenance mode is on", "Update notifications and automatic deploys are paused. Heartbeats, inventory, and registry checks continue.", colorWarn)
		}
		return replyCard(iconMaint, "Maintenance mode is off", "Notifications and automatic deploys run as usual.", colorOK)
	case "on":
		return r.setMaintenance(ctx, it, true)
	case "off":
		// Ending maintenance while operations wait asks first.
		on, err := r.Store.Maintenance(ctx)
		if err != nil {
			return r.internalError("read maintenance mode", err)
		}
		queued, err := r.Store.QueuedOperations(ctx)
		if err != nil {
			return r.internalError("count queued operations", err)
		}
		if on && queued > 0 {
			return reply(Message{Embeds: []*discordgo.MessageEmbed{{
				Title: iconMaint + " Turn maintenance mode off?",
				Description: plural(queued, "operation") + " waiting to run. Update notifications and automatic deploys start again, " +
					"and you get one summary of what is pending.",
				Color: colorWarn,
			}}, Components: buttonRow(
				button("Turn it off", "", discordgo.DangerButton, customID(actMaintenanceOff)),
				button("Cancel", "", discordgo.SecondaryButton, customID(actCancel)))})
		}
		return r.setMaintenance(ctx, it, false)
	}
	return ephemeral("Unknown subcommand.")
}

// setMaintenance changes maintenance mode and announces it.
func (r *Router) setMaintenance(ctx context.Context, it Interaction, on bool) Response {
	changed, err := r.Store.SetMaintenance(ctx, on, r.actor(it), r.Now())
	if err != nil {
		return r.internalError("set maintenance mode", err)
	}
	if !changed {
		state := "off"
		if on {
			state = "on"
		}
		return ephemeral("Maintenance mode is already " + state + ".")
	}
	answer := reply
	if it.Kind == KindComponent {
		answer = updateTo // the confirmation card becomes the announcement
	}
	if it.ChannelID == r.ChannelID {
		// The reply is the announcement; only the summary follows.
		if !on && r.Notify != nil {
			r.Notify.MaintenanceSummary(ctx)
		}
		return answer(maintenanceNotice(on, it.UserID))
	}
	if r.Notify != nil {
		if on {
			r.Notify.enqueue(ctx, func(ctx context.Context) { r.Notify.post(ctx, "maintenance", maintenanceNotice(true, it.UserID)) })
		} else {
			r.Notify.enqueue(ctx, func(ctx context.Context) { r.Notify.post(ctx, "maintenance", maintenanceNotice(false, it.UserID)) })
			r.Notify.MaintenanceSummary(ctx)
		}
	}
	return answer(maintenanceNotice(on, it.UserID))
}
