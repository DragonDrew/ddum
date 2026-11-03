package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Session connects to Discord with discordgo and implements Gateway.
type Session struct {
	s       *discordgo.Session
	appID   string
	guildID string
	log     *slog.Logger
	router  *Router
	closing atomic.Bool
}

var _ Gateway = (*Session)(nil)

// NewSession prepares a Discord session. Call Start to connect.
func NewSession(token, appID, guildID string, log *slog.Logger) (*Session, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discord session: %w", err)
	}
	// Interactions arrive regardless of intents; Guilds is the only one asked
	// for, and it is not privileged.
	s.Identify.Intents = discordgo.IntentsGuilds
	return &Session{s: s, appID: appID, guildID: guildID, log: log}, nil
}

// Start connects, routes interactions to router, and registers the guild
// commands by bulk overwrite.
func (d *Session) Start(router *Router) error {
	d.router = router
	d.s.AddHandler(d.onInteraction)
	if err := d.s.Open(); err != nil {
		return fmt.Errorf("connect to Discord: %w", err)
	}
	cmds, err := d.s.ApplicationCommandBulkOverwrite(d.appID, d.guildID, Commands())
	if err != nil {
		return fmt.Errorf("register slash commands: %w", err)
	}
	d.log.Info("discord connected; commands registered", "commands", len(cmds), "guild_id", d.guildID)
	return nil
}

// Close stops accepting interactions and disconnects. Only the first
// call has an effect.
func (d *Session) Close() error {
	if !d.closing.CompareAndSwap(false, true) {
		return nil
	}
	return d.s.Close()
}

// Send implements Gateway. Mentions are never pinged.
func (d *Session) Send(ctx context.Context, channelID string, m Message) (string, error) {
	m = fitLimits(m)
	msg, err := d.s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: m.Content, Embeds: m.Embeds, Components: m.Components,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}, discordgo.WithContext(ctx))
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

// Edit implements Gateway.
func (d *Session) Edit(ctx context.Context, channelID, messageID string, m Message) error {
	m = fitLimits(m)
	content := m.Content
	embeds := m.Embeds
	if embeds == nil {
		embeds = []*discordgo.MessageEmbed{}
	}
	components := m.Components
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	_, err := d.s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID: messageID, Channel: channelID, Content: &content, Embeds: &embeds, Components: &components,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}, discordgo.WithContext(ctx))
	return err
}

// EveryoneCanView reports whether @everyone can see channelID.
func (d *Session) EveryoneCanView(ctx context.Context, channelID string) (bool, error) {
	roles, err := d.s.GuildRoles(d.guildID, discordgo.WithContext(ctx))
	if err != nil {
		return false, fmt.Errorf("read guild roles: %w", err)
	}
	ch, err := d.s.Channel(channelID, discordgo.WithContext(ctx))
	if err != nil {
		return false, fmt.Errorf("read channel: %w", err)
	}
	if ch.GuildID != d.guildID {
		return false, errors.New("NOTIFICATION_CHANNEL_ID is not a channel of DISCORD_GUILD_ID")
	}
	var everyone int64
	for _, r := range roles {
		if r.ID == d.guildID { // the @everyone role has the guild's ID
			everyone = r.Permissions
		}
	}
	return EveryoneCanView(d.guildID, everyone, ch.PermissionOverwrites), nil
}

// EveryoneCanView applies the channel's @everyone overwrite to the
// @everyone role's permissions.
func EveryoneCanView(guildID string, everyonePerms int64, overwrites []*discordgo.PermissionOverwrite) bool {
	if everyonePerms&discordgo.PermissionAdministrator != 0 {
		return true
	}
	p := everyonePerms
	for _, o := range overwrites {
		if o.Type == discordgo.PermissionOverwriteTypeRole && o.ID == guildID {
			p &^= o.Deny
			p |= o.Allow
		}
	}
	return p&discordgo.PermissionViewChannel != 0
}

func (d *Session) onInteraction(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	it, ok := toInteraction(ic.Interaction)
	if !ok {
		return
	}
	// discordgo runs handlers on their own goroutines, so a panic here would
	// stop the whole bot, with every agent and check along with it.
	defer func() {
		if p := recover(); p != nil {
			d.log.Error("panic handling an interaction", "panic", p, "kind", it.Kind, "command", it.Command,
				"custom_id", it.CustomID, "stack", string(debug.Stack()))
			s.InteractionRespond(ic.Interaction, toDiscordResponse(ephemeral("Something went wrong. The error has been logged.")))
		}
	}()
	var resp Response
	if d.closing.Load() {
		resp = ephemeral("The bot is restarting. Try again in a minute.")
	} else {
		// Every handler in this version reads or writes the local database
		// only, so it answers well within the 3-second limit without deferring.
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		resp = d.router.Handle(ctx, it)
		cancel()
	}
	if err := s.InteractionRespond(ic.Interaction, toDiscordResponse(resp)); err != nil {
		d.log.Error("respond to interaction", "kind", it.Kind, "command", it.Command, "custom_id", it.CustomID, "err", err)
		return
	}
	if resp.Kind == RespMessage && !resp.Ephemeral && resp.Bind != nil {
		// A new card: look up its message ID so the bot can edit it later.
		msg, err := s.InteractionResponse(ic.Interaction)
		if err != nil {
			d.log.Warn("read card message", "command", it.Command, "err", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		d.router.Delivered(ctx, resp, msg.ChannelID, msg.ID)
		cancel()
	}
}

func toInteraction(i *discordgo.Interaction) (Interaction, bool) {
	it := Interaction{GuildID: i.GuildID, ChannelID: i.ChannelID, Options: map[string]string{}}
	switch {
	case i.Member != nil && i.Member.User != nil:
		it.UserID = i.Member.User.ID
		it.UserName = i.Member.User.Username
		if i.Member.Nick != "" {
			it.UserName = i.Member.Nick
		} else if i.Member.User.GlobalName != "" {
			it.UserName = i.Member.User.GlobalName
		}
	case i.User != nil:
		it.UserID = i.User.ID
		it.UserName = i.User.Username
	}
	if i.Message != nil {
		it.MessageID = i.Message.ID
		it.Ephemeral = i.Message.Flags&discordgo.MessageFlagsEphemeral != 0
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand, discordgo.InteractionApplicationCommandAutocomplete:
		it.Kind = KindCommand
		if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
			it.Kind = KindAutocomplete
		}
		data := i.ApplicationCommandData()
		it.Command = data.Name
		opts := data.Options
		if len(opts) == 1 && opts[0].Type == discordgo.ApplicationCommandOptionSubCommand {
			it.Subcommand = opts[0].Name
			opts = opts[0].Options
		}
		for _, o := range opts {
			it.Options[o.Name] = fmt.Sprint(o.Value)
			if o.Focused {
				it.Focused = o.Name
			}
		}
	case discordgo.InteractionMessageComponent:
		it.Kind = KindComponent
		data := i.MessageComponentData()
		it.CustomID = data.CustomID
		it.Values = data.Values
	default:
		return it, false
	}
	return it, true
}

func toDiscordResponse(r Response) *discordgo.InteractionResponse {
	switch r.Kind {
	case RespChoices:
		choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(r.Choices))
		for _, c := range r.Choices {
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: c.Name, Value: c.Value})
		}
		return &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: choices},
		}
	case RespUpdate:
		return &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: responseData(r),
		}
	}
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: responseData(r),
	}
}

func responseData(r Response) *discordgo.InteractionResponseData {
	m := fitLimits(r.Message)
	d := &discordgo.InteractionResponseData{
		Content:         m.Content,
		Embeds:          m.Embeds,
		Components:      m.Components,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}
	if d.Embeds == nil {
		d.Embeds = []*discordgo.MessageEmbed{}
	}
	if d.Components == nil {
		d.Components = []discordgo.MessageComponent{}
	}
	if r.Ephemeral {
		d.Flags = discordgo.MessageFlagsEphemeral
	}
	return d
}
