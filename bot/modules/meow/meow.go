// Package meow replies "meow" to any guild message whose entire text is the
// word "meow" (case-insensitive, surrounding whitespace ignored).
//
// Requires the bot to be running with the IntentsGuildMessages and the
// privileged IntentsMessageContent intents. The latter must be enabled in
// the Discord developer portal — without it Discord delivers messages with
// empty content and this module silently does nothing.
package meow

import (
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/bot"
)

// Module implements bot.Module and bot.MessageHandler.
type Module struct{}

func New() *Module { return &Module{} }

func (m *Module) Name() string { return "meow" }
func (m *Module) Description() string {
	return "Replies \"meow\" whenever a member's entire message is just \"meow\"."
}
func (m *Module) Category() bot.Category { return bot.CategoryFun }

func (m *Module) Commands() []*discordgo.ApplicationCommand { return nil }

func (m *Module) HandleInteraction(_ *discordgo.Session, _ *discordgo.InteractionCreate) {
	// No slash commands; nothing to route.
}

func (m *Module) OnLoad(_ *discordgo.Session) error {
	log.Println("[meow] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[meow] module unloaded")
	return nil
}

// HandleMessage is dispatched by the bot core for every guild message after
// the module-enable and channel-allow gates pass. We only reply when the
// trimmed message text is exactly "meow" — no leading prose, no trailing
// punctuation, no embeds-with-text dressed up as a meow.
func (m *Module) HandleMessage(s *discordgo.Session, msg *discordgo.MessageCreate) {
	if msg.GuildID == "" {
		// Skip DMs. The dashboard's per-guild settings can't gate them, and a
		// follow-up that wants DM replies should be an opt-in feature.
		return
	}
	if !strings.EqualFold(strings.TrimSpace(msg.Content), "meow") {
		return
	}

	if _, err := s.ChannelMessageSendComplex(msg.ChannelID, &discordgo.MessageSend{
		Content:   "meow",
		Reference: msg.Reference(),
		// Suppress the auto-mention that Discord otherwise adds when replying.
		AllowedMentions: &discordgo.MessageAllowedMentions{RepliedUser: false},
	}); err != nil {
		log.Printf("[meow] reply in %s failed: %v", msg.ChannelID, err)
	}
}
