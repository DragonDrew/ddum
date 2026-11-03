package discord

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/store"
)

// NotAuthorized is the only thing an unauthorized user is told.
const NotAuthorized = "You are not authorized."

// Router authorizes and dispatches interactions.
type Router struct {
	Store     *store.Store
	Gateway   Gateway
	Log       *slog.Logger
	Now       func() time.Time
	Started   time.Time
	Version   string
	GuildID   string
	ChannelID string // notification channel
	Notify    *Notifier
	// StartCheck starts a registry check in the background and calls done
	// when it ends; it returns ErrCheckBusy if one is running. Empty IDs mean
	// everything.
	StartCheck func(hostID, stackID string, done func(CheckSummary)) error
	// ExpiryMinutes is COMMAND_EXPIRY_MINUTES for operations created here.
	ExpiryMinutes int
	// Location is the time zone schedule times are offered in (TZ).
	Location   *time.Location
	authorized map[string]bool
	checks     *checkCards
}

// NewRouter builds a router that accepts only the given Discord user IDs.
func NewRouter(r Router, authorizedIDs []string) *Router {
	if r.ExpiryMinutes <= 0 {
		r.ExpiryMinutes = 60
	}
	r.checks = &checkCards{byID: map[string]*checkCard{}}
	r.authorized = make(map[string]bool, len(authorizedIDs))
	for _, id := range authorizedIDs {
		r.authorized[id] = true
	}
	return &r
}

// Handle is the single entry point for every interaction. The allowlist
// check runs first for every kind — commands, autocomplete, and components —
// because anyone in the channel can click a button.
func (r *Router) Handle(ctx context.Context, it Interaction) Response {
	if !r.authorized[it.UserID] || (r.GuildID != "" && it.GuildID != r.GuildID) {
		r.Log.Warn("unauthorized interaction denied", "user_id", it.UserID, "user_name", it.UserName,
			"guild_id", it.GuildID, "command", it.Command, "subcommand", it.Subcommand, "custom_id", it.CustomID)
		if it.Kind == KindAutocomplete {
			return Response{Kind: RespChoices}
		}
		return Response{Kind: RespMessage, Ephemeral: true, Private: true, Message: Message{Content: NotAuthorized}}
	}
	switch it.Kind {
	case KindAutocomplete:
		return Response{Kind: RespChoices, Choices: r.autocomplete(ctx, it)}
	case KindComponent:
		return r.visibility(it, r.component(ctx, it))
	case KindCommand:
		return r.visibility(it, r.command(ctx, it))
	}
	return ephemeral("Unsupported interaction.")
}

func (r *Router) command(ctx context.Context, it Interaction) Response {
	switch it.Command + " " + it.Subcommand {
	case "host list":
		return r.hostList(ctx)
	case "host show":
		return r.hostShow(ctx, it)
	case "host approve":
		return r.hostApprove(ctx, it)
	case "host rename":
		return r.hostRename(ctx, it)
	case "host remove":
		return r.hostRemove(ctx, it)
	case "host reassign-agent":
		return r.hostReassign(ctx, it)
	case "stack list":
		return r.stackList(ctx, it)
	case "stack show":
		return r.stackShow(ctx, it)
	case "stack rename":
		return r.stackRename(ctx, it)
	case "stack merge":
		return r.stackMerge(ctx, it)
	case "container list":
		return r.containerList(ctx, it)
	case "container show":
		return r.containerShow(ctx, it)
	case "system status":
		return r.systemStatus(ctx)
	case "update list":
		return r.updateList(ctx, it)
	case "update show":
		return r.updateShow(ctx, it)
	case "update check":
		return r.updateCheck(ctx, it)
	case "update snooze":
		return r.updateSnooze(ctx, it)
	case "update snoozed":
		return r.updateSnoozed(ctx)
	case "update unsnooze":
		return r.updateUnsnooze(ctx, it)
	case "update unsnooze-all":
		return r.updateUnsnoozeAll(ctx)
	case "update approve":
		return r.updateApprove(ctx, it)
	case "history list":
		return r.historyList(ctx, it)
	case "history show":
		return r.historyShow(ctx, it)
	case "group list":
		return r.groupList(ctx)
	case "group show":
		return r.groupShow(ctx, it)
	case "group create":
		return r.groupCreate(ctx, it)
	case "group rename":
		return r.groupRename(ctx, it)
	case "group delete":
		return r.groupDelete(ctx, it)
	case "group add":
		return r.groupAdd(ctx, it)
	case "group remove":
		return r.groupRemove(ctx, it)
	case "rollback list":
		return r.rollbackList(ctx)
	case "rollback run":
		return r.rollbackRun(ctx, it)
	case "rollback clear-hold":
		return r.rollbackClearHold(ctx, it, it.Options[optStack])
	case "policy show":
		return r.policyShow(ctx, it)
	case "policy set":
		return r.policySet(ctx, it)
	case "policy clear":
		return r.policyClear(ctx, it)
	case "maintenance on", "maintenance off", "maintenance status":
		return r.maintenance(ctx, it)
	}
	return ephemeral("Unknown command.")
}

// Component custom_id actions. Format: v1:<action>:<uuid>[:<arg>].
const (
	actHostApprove     = "host_approve"
	actHostReject      = "host_reject"
	actHostRemove      = "host_remove"
	actHostReassign    = "host_reassign"
	actCancel          = "cancel"
	actSnooze          = "snooze"
	actSnoozePick      = "snooze_pick"
	actReview          = "review"
	actReviewPick      = "review_pick"
	actUnsnoozeAll     = "unsnooze_all"
	actApprove         = "approve"
	actOpCancel        = "op_cancel"
	actRollback        = "rollback"
	actRollbackConfirm = "rollback_ok"
	actHoldClear       = "hold_clear"
	actSelfUpdate      = "selfupdate"
	actGroupDelete     = "group_delete"
	actStackMerge      = "stack_merge"
	actMergeAsk        = "merge_ask"
	actBack            = "back"        // show a stack's or group's updates again
	actResultBack      = "result_back" // show an operation's result again
	actSelfBack        = "self_back"   // show the bot update card again
	actHostBack        = "host_back"   // show a registration card again
	actHostPick        = "host_pick"   // menu: open a host's waiting stacks
	actHostView        = "host_view"   // button: the same
	actSchedule        = "schedule"    // ask when to deploy an offer
	actSchedulePick    = "sched_pick"  // menu: deploy the offer then
	actMaintenanceOff  = "maint_off"   // confirmed: end maintenance mode
	actKeyAccept       = "key_accept"  // accept the key an agent now presents
	actKeyReject       = "key_reject"  // keep the pinned key
)

func customID(action string, parts ...string) string {
	return strings.Join(append([]string{"v1", action}, parts...), ":")
}

func (r *Router) component(ctx context.Context, it Interaction) Response {
	parts := strings.Split(it.CustomID, ":")
	if len(parts) < 2 || parts[0] != "v1" {
		return ephemeral("This button is from an older version and no longer works.")
	}
	arg := func(i int) string {
		if len(parts) > i {
			return parts[i]
		}
		return ""
	}
	switch parts[1] {
	case actHostApprove:
		return r.hostApproveButton(ctx, it, arg(2))
	case actHostReject:
		return r.hostRejectButton(ctx, arg(2))
	case actHostBack:
		return r.hostBack(ctx, arg(2))
	case actHostRemove:
		return r.hostRemoveConfirmed(ctx, it, arg(2))
	case actHostReassign:
		return r.hostReassignConfirmed(ctx, it, arg(2), arg(3))
	case actSnooze:
		return r.snoozeButton(ctx, arg(2))
	case actSnoozePick:
		return r.snoozePicked(ctx, it, arg(2))
	case actReview:
		return r.allPending(ctx)
	case actReviewPick:
		if len(it.Values) != 1 {
			return ephemeral("Pick one stack or group.")
		}
		return r.openScope(ctx, it, it.Values[0])
	case actHostPick:
		if len(it.Values) != 1 {
			return ephemeral("Pick one host.")
		}
		return r.hostView(ctx, it.Values[0])
	case actHostView:
		return r.hostView(ctx, arg(2))
	case actSchedule:
		return r.scheduleButton(ctx, arg(2))
	case actSchedulePick:
		return r.schedulePicked(ctx, it, arg(2))
	case actApprove:
		return r.approveButton(ctx, it, arg(2))
	case actRollback:
		return r.rollbackButton(ctx, it, arg(2))
	case actResultBack:
		return r.resultBack(ctx, arg(2))
	case actBack:
		return r.backToScope(ctx, it, arg(2), "")
	case actSelfBack:
		return r.selfBack(ctx, arg(2))
	case actRollbackConfirm:
		return r.rollbackConfirmed(ctx, it, arg(2))
	case actHoldClear:
		return r.rollbackClearHold(ctx, it, arg(2))
	case actMergeAsk:
		return r.stackMerge(ctx, Interaction{Kind: KindComponent, Options: map[string]string{optSource: arg(2), optInto: arg(3)}})
	case actStackMerge:
		return r.stackMergeConfirmed(ctx, it, arg(2), arg(3))
	case actGroupDelete:
		return r.groupDeleteConfirmed(ctx, it, arg(2))
	case actSelfUpdate:
		return r.selfUpdateButton(ctx, it, arg(2))
	case actOpCancel:
		return r.cancelOperation(ctx, it, arg(2))
	case actUnsnoozeAll:
		return r.unsnoozeAllConfirmed(ctx, it)
	case actMaintenanceOff:
		return r.setMaintenance(ctx, it, false)
	case actKeyAccept:
		return r.agentKeyDecision(ctx, it, arg(2), true)
	case actKeyReject:
		return r.agentKeyDecision(ctx, it, arg(2), false)
	case actCancel:
		return cancelledCard()
	}
	return ephemeral("Unknown button.")
}

func (r *Router) actor(it Interaction) store.Actor {
	return store.Actor{Kind: "user", DiscordID: it.UserID, Name: it.UserName}
}

// internalError logs err and gives the user a generic reply.
func (r *Router) internalError(what string, err error) Response {
	r.Log.Error(what, "err", err)
	return ephemeral("Something went wrong (" + what + "). The error has been logged.")
}
