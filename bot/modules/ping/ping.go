// Package ping is the simplest possible module — it responds to /ping with
// "Pong!". Use it as a copy-paste template when building new modules.
package ping

import (
	"fmt"
	"log"
	"os"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/bot"
)

// Module implements bot.Module for the ping feature.
type Module struct{}

// New returns a ready-to-use PingModule.
func New() *Module { return &Module{} }

func (m *Module) Name() string          { return "ping" }
func (m *Module) Description() string   { return "Responds to /ping with Pong! Useful for checking bot latency." }
func (m *Module) Category() bot.Category { return bot.CategoryFunctional }

// Commands declares the slash commands this module owns.
func (m *Module) Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "ping",
			Description: "Check if the bot is alive",
		},
	}
}

// HandleInteraction responds to the /ping command.
func (m *Module) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	latency := s.HeartbeatLatency().Milliseconds()
	msg := fmt.Sprintf("🏓 Pong! `%dms`", latency)
	if region := os.Getenv("RAILWAY_REPLICA_REGION"); region != "" {
		msg += fmt.Sprintf(" · `%s`", region)
	} else if region = os.Getenv("RAILWAY_REGION"); region != "" {
		msg += fmt.Sprintf(" · `%s`", region)
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
		},
	}); err != nil {
		log.Printf("[ping] failed to respond to /ping: %v", err)
	}
}

func (m *Module) OnLoad(_ *discordgo.Session) error {
	log.Println("[ping] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[ping] module unloaded")
	return nil
}
