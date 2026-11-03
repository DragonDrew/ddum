package discord

import "github.com/bwmarrin/discordgo"

// Option names shared by commands and autocomplete.
const (
	optHost           = "host"
	optStack          = "stack"
	optContainer      = "container"
	optName           = "name"
	optAgentID        = "agent_id"
	optIncludeMissing = "include_missing"
	optUpdate         = "update"
	optDuration       = "duration"
	optIncludeSnoozed = "include_snoozed"
	optScope          = "scope"
	optTarget         = "target"
	optMode           = "mode"
	optTagTrack       = "tag_track"
	optAutoRollback   = "auto_rollback"
	optVerifyTimeout  = "verify_timeout_seconds"
	optVerifyStable   = "verify_stable_seconds"
	optField          = "field"
	optOperation      = "operation"
	optGroup          = "group"
	optSource         = "source"
	optInto           = "into"
)

// Commands returns the slash commands registered at startup.
// default_member_permissions "0" hides them from ordinary members;
// the allowlist remains the real authorization check.
func Commands() []*discordgo.ApplicationCommand {
	hidden := int64(0)
	guildOnly := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	cmd := func(name, desc string, subs ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommand {
		return &discordgo.ApplicationCommand{
			Name: name, Description: desc, Options: subs,
			DefaultMemberPermissions: &hidden, Contexts: &guildOnly,
		}
	}
	sub := func(name, desc string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: desc, Options: opts}
	}
	pick := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name,
			Description: desc, Required: required, Autocomplete: true}
	}
	text := func(name, desc string, maxLen int) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name,
			Description: desc, Required: true, MinLength: ptr(1), MaxLength: maxLen}
	}
	flag := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionBoolean, Name: name, Description: desc}
	}

	return []*discordgo.ApplicationCommand{
		cmd("host", "Managed Docker hosts",
			sub("list", "List hosts"),
			sub("show", "Show a host", pick(optHost, "Host", true)),
			sub("approve", "Approve a host that is waiting to register", pick(optHost, "Pending host", true)),
			sub("rename", "Rename a host", pick(optHost, "Host", true), text(optName, "New name", 100)),
			sub("remove", "Remove a host; its agent is refused from then on", pick(optHost, "Host", true)),
			sub("reassign-agent", "Point a host at a replacement agent",
				pick(optHost, "Host", true), text(optAgentID, "UUID of the replacement agent (AGENT_ID)", 36)),
		),
		cmd("stack", "Stacks (Compose projects, Portainer stacks, standalone containers)",
			sub("list", "List stacks", pick(optHost, "Only this host", false), flag(optIncludeMissing, "Include stacks that are no longer seen")),
			sub("show", "Show a stack", pick(optStack, "Stack", true)),
			sub("rename", "Rename a stack", pick(optStack, "Stack", true), text(optName, "New name", 100)),
			sub("merge", "Merge a renamed or moved stack into its old identity (confirms first)",
				pick(optSource, "Stack", true), pick(optInto, "Stack", true)),
		),
		cmd("container", "Containers",
			sub("list", "List containers", pick(optHost, "Only this host", false), pick(optStack, "Only this stack", false),
				flag(optIncludeMissing, "Include containers that are no longer seen")),
			sub("show", "Show a container", pick(optContainer, "Container", true)),
		),
		cmd("system", "System information",
			sub("status", "Bot version, uptime, and agent status"),
		),
		cmd("update", "Detected image updates",
			sub("list", "List pending updates", pick(optHost, "Only this host", false), pick(optStack, "Only this stack", false),
				flag(optIncludeSnoozed, "Include snoozed updates")),
			sub("show", "Show one update", pick(optUpdate, "Update", true)),
			sub("check", "Check registries for updates now", pick(optHost, "Only this host", false), pick(optStack, "Only this stack", false)),
			sub("snooze", "Snooze one update", pick(optUpdate, "Update", true), choices(optDuration, "For how long", true, snoozeChoices...)),
			sub("snoozed", "List snoozed updates"),
			sub("unsnooze", "Remove the snooze from one update", pick(optUpdate, "Snoozed update", true)),
			sub("unsnooze-all", "Remove every snooze"),
			sub("approve", "Approve one update (confirms first)", pick(optUpdate, "Update", true)),
		),
		cmd("group", "Update groups: containers deployed and rolled back together",
			sub("list", "List groups"),
			sub("show", "Show a group and its members", pick(optGroup, "Group", true)),
			sub("create", "Create a group", text(optName, "Name", 100)),
			sub("rename", "Rename a group", pick(optGroup, "Group", true), text(optName, "New name", 100)),
			sub("delete", "Delete a group (confirms first)", pick(optGroup, "Group", true)),
			sub("add", "Add a container to a group", pick(optGroup, "Group", true), pick(optContainer, "Container", true)),
			sub("remove", "Take a container out of its group", pick(optContainer, "Container", true)),
		),
		cmd("rollback", "Roll back a deployment",
			sub("list", "Deployments that can be rolled back, and stacks held after a failed rollback"),
			sub("run", "Roll back a deployment (confirms first)", pick(optOperation, "Deployment", true)),
			sub("clear-hold", "Clear the hold left by a failed rollback", pick(optStack, "Stack", true)),
		),
		cmd("history", "Deployment history",
			sub("list", "Recent operations", pick(optHost, "Only this host", false), pick(optStack, "Only this stack", false)),
			sub("show", "One operation, with its events and output", pick(optOperation, "Operation", true)),
		),
		cmd("policy", "Update policies (global, host, stack, container)",
			sub("show", "Show the effective policy and which level set each field",
				choices(optScope, "Level", true, scopeChoices...), pick(optTarget, "Host, stack, or container (not for global)", false)),
			sub("set", "Set policy fields at one level",
				choices(optScope, "Level", true, scopeChoices...), pick(optTarget, "Host, stack, or container (not for global)", false),
				choices(optMode, "manual, automatic, or ignore", false, "manual", "automatic", "ignore"),
				choices(optTagTrack, "Which version updates to offer", false, "pinned", "same-minor", "same-major", "any"),
				flag(optAutoRollback, "Roll back automatically when a deployment fails"),
				number(optVerifyTimeout, "Seconds to wait for a healthy container (10-1800)", 10, 1800),
				number(optVerifyStable, "Seconds a container must stay up without a health check (0-600)", 0, 600)),
			sub("clear", "Clear policy fields at one level so they inherit again",
				choices(optScope, "Level", true, scopeChoices...), pick(optTarget, "Host, stack, or container (not for global)", false),
				choices(optField, "Only this field (default: all)", false, "mode", "tag_track", "auto_rollback", "verify_timeout_seconds", "verify_stable_seconds")),
		),
		cmd("maintenance", "Maintenance mode: pause update notifications",
			sub("on", "Turn maintenance mode on"),
			sub("off", "Turn maintenance mode off and summarize pending updates"),
			sub("status", "Show whether maintenance mode is on"),
		),
	}
}

var (
	snoozeChoices = []string{"1 day", "1 week", "1 month", "indefinitely"}
	scopeChoices  = []string{"global", "host", "stack", "group", "container"}
)

// choices is a string option limited to fixed values.
func choices(name, desc string, required bool, values ...string) *discordgo.ApplicationCommandOption {
	o := &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name, Description: desc, Required: required}
	for _, v := range values {
		o.Choices = append(o.Choices, &discordgo.ApplicationCommandOptionChoice{Name: v, Value: v})
	}
	return o
}

// number is an integer option with bounds.
func number(name, desc string, minV, maxV float64) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: name, Description: desc,
		MinValue: &minV, MaxValue: maxV}
}

func ptr[T any](v T) *T { return &v }
