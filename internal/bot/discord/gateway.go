// Package discord is the bot's only user interface. Handlers
// are written against the small Interaction/Response types and the Gateway
// interface, so they run in tests without Discord.
package discord

import (
	"context"

	"github.com/bwmarrin/discordgo"
)

// Kind is the type of an incoming interaction.
type Kind int

const (
	KindCommand Kind = iota + 1
	KindAutocomplete
	KindComponent
)

// Interaction is a provider-neutral view of one Discord interaction.
type Interaction struct {
	Kind      Kind
	GuildID   string
	ChannelID string
	MessageID string // the message a component belongs to
	// Ephemeral is set when that message is visible only to the user, so the
	// bot cannot edit it later with its own token.
	Ephemeral bool
	UserID    string
	UserName  string

	Command    string            // top-level command, e.g. "host"
	Subcommand string            // e.g. "show"
	Options    map[string]string // option values as strings ("true"/"false" for booleans)
	Focused    string            // autocomplete: the option being typed

	CustomID string   // components
	Values   []string // select menus
}

// ResponseKind says how to answer an interaction.
type ResponseKind int

const (
	RespMessage ResponseKind = iota + 1 // new reply message
	RespUpdate                          // edit the message the component is on
	RespChoices                         // autocomplete choices
)

// Message is the content of a message the bot sends or edits.
type Message struct {
	Content    string
	Embeds     []*discordgo.MessageEmbed
	Components []discordgo.MessageComponent
}

// Choice is one autocomplete choice: a readable name and a UUID value.
type Choice struct {
	Name  string
	Value string
}

// Response answers an interaction.
type Response struct {
	Kind      ResponseKind
	Ephemeral bool
	Message   Message
	Choices   []Choice
	// Private keeps a command reply visible only to the user even in the
	// notification channel: errors and refusals. Other command replies are
	// cards posted in the channel when the command is run there.
	Private bool
	// Bind names what a new public card represents, so the bot can keep
	// editing it (see Router.Delivered).
	Bind *Binding
}

// Binding is what a card represents: an entity type and ID.
type Binding struct {
	Entity string
	ID     string
}

// Gateway sends and edits channel messages with the bot's own token.
type Gateway interface {
	Send(ctx context.Context, channelID string, m Message) (messageID string, err error)
	Edit(ctx context.Context, channelID, messageID string, m Message) error
}
