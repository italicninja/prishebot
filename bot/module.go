package bot

import "github.com/bwmarrin/discordgo"

// Category groups modules in the dashboard and the /modules command.
type Category string

// CategoryOrder is the canonical display order for Category values.
// Renderers should iterate this slice rather than ranging a map so the
// grouping is stable across requests.
var CategoryOrder = []Category{CategoryFunctional, CategoryFun}

const (
	// CategoryFunctional groups modules that provide bot administration,
	// server management, or general utility (ping, info, roles, etc.).
	CategoryFunctional Category = "Functional"
	// CategoryFun groups modules that exist for entertainment or social
	// flair (birthday, raid sign-ups, meow, etc.).
	CategoryFun Category = "Fun"
)

// Module is the interface every bot module must implement.
//
// A module is a self-contained feature bundle — it owns its own slash commands
// and event handling logic. The bot core manages loading/unloading modules,
// registering their commands with Discord's API, and routing interactions to
// the right module automatically.
//
// To create a new module:
//  1. Create a new package under bot/modules/yourmodule/
//  2. Define a struct that implements all methods below
//  3. Export a New() constructor
//  4. Call b.LoadModule(yourmodule.New()) in main.go
type Module interface {
	// Name returns a unique, lowercase identifier (e.g. "moderation", "music").
	// This key is used in the registry and in the web UI.
	Name() string

	// Description returns a short human-readable summary shown in the web UI.
	Description() string

	// Category groups the module in the dashboard and /modules listings.
	// Return either CategoryFunctional or CategoryFun.
	Category() Category

	// Commands returns the slash command definitions this module registers.
	// These are sent to Discord's API at load time.
	// Return an empty slice if the module only reacts to events.
	Commands() []*discordgo.ApplicationCommand

	// HandleInteraction is called whenever one of this module's slash commands
	// is invoked in Discord. Check i.ApplicationCommandData().Name to branch
	// between commands if the module owns more than one.
	HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate)

	// OnLoad is called once when the module is registered with the bot.
	// Spin up background goroutines, open connections, etc. here.
	OnLoad(s *discordgo.Session) error

	// OnUnload is called when the module is removed (or the bot shuts down).
	// Stop goroutines, close connections, and release resources here.
	OnUnload(s *discordgo.Session) error
}

// MessageHandler is an optional add-on a Module can implement to react to
// plain (non-interaction) messages. The bot core dispatches MessageCreate
// events to every loaded Module that satisfies this interface, after
// applying the per-guild module-enable toggle and channel allow-list.
//
// Modules that implement this need the application to be running with the
// IntentsGuildMessages and the privileged IntentsMessageContent intents,
// and the latter must be granted in the Discord developer portal.
type MessageHandler interface {
	HandleMessage(s *discordgo.Session, m *discordgo.MessageCreate)
}
