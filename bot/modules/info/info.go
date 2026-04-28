// Package info provides /serverinfo and /botinfo slash commands.
// It shows a slightly more realistic module: one module, multiple commands.
package info

import (
	"fmt"
	"log"
	"os"

	"github.com/bwmarrin/discordgo"
)

const discordBlurple = 0x5865F2

// Module implements bot.Module for server and bot information commands.
type Module struct{}

func New() *Module { return &Module{} }

func (m *Module) Name() string        { return "info" }
func (m *Module) Description() string { return "Provides /serverinfo and /botinfo slash commands." }

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "serverinfo",
			Description: "Display information about this server",
		},
		{
			Name:        "botinfo",
			Description: "Display information about the bot",
		},
	}
}

// HandleInteraction dispatches to the correct handler based on the command name.
// This is the standard pattern when a module owns more than one command.
func (m *Module) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.ApplicationCommandData().Name {
	case "serverinfo":
		m.serverInfo(s, i)
	case "botinfo":
		m.botInfo(s, i)
	}
}

func (m *Module) serverInfo(s *discordgo.Session, i *discordgo.InteractionCreate) {
	guild, err := s.Guild(i.GuildID)
	if err != nil {
		log.Printf("[info] s.Guild(%s) failed: %v", i.GuildID, err)
		respondError(s, i, "Couldn't fetch server info.")
		return
	}

	embed := &discordgo.MessageEmbed{
		Title:       guild.Name,
		Description: fmt.Sprintf("ID: `%s`", guild.ID),
		Color:       discordBlurple,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Owner", Value: fmt.Sprintf("<@%s>", guild.OwnerID), Inline: true},
			{Name: "Members", Value: fmt.Sprintf("%d", guild.MemberCount), Inline: true},
			{Name: "Locale", Value: guild.PreferredLocale, Inline: true},
			{Name: "Channels", Value: fmt.Sprintf("%d", len(guild.Channels)), Inline: true},
			{Name: "Roles", Value: fmt.Sprintf("%d", len(guild.Roles)), Inline: true},
		},
	}
	if guild.Icon != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
			URL: fmt.Sprintf("https://cdn.discordapp.com/icons/%s/%s.png", guild.ID, guild.Icon),
		}
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}},
	}); err != nil {
		log.Printf("[info] failed to respond to /serverinfo: %v", err)
	}
}

func (m *Module) botInfo(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if s.State.User == nil {
		log.Println("[info] s.State.User is nil in botInfo")
		respondError(s, i, "Bot state is not available.")
		return
	}
	self := s.State.User

	embed := &discordgo.MessageEmbed{
		Title: self.Username,
		Color: discordBlurple,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "ID", Value: fmt.Sprintf("`%s`", self.ID), Inline: true},
			{Name: "Servers", Value: fmt.Sprintf("%d", len(s.State.Guilds)), Inline: true},
		},
	}
	if self.Avatar != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
			URL: fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png", self.ID, self.Avatar),
		}
	}
	if env := os.Getenv("RAILWAY_ENVIRONMENT_NAME"); env != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Environment", Value: fmt.Sprintf("`%s`", env), Inline: true,
		})
	}
	if region := os.Getenv("RAILWAY_REPLICA_REGION"); region != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Region", Value: fmt.Sprintf("`%s`", region), Inline: true,
		})
	}
	if svc := os.Getenv("RAILWAY_SERVICE_NAME"); svc != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "Service", Value: fmt.Sprintf("`%s`", svc), Inline: true,
		})
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}},
	}); err != nil {
		log.Printf("[info] failed to respond to /botinfo: %v", err)
	}
}

// respondError sends an ephemeral error message back to the user.
func respondError(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "❌ " + msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[info] failed to send error response: %v", err)
	}
}

func (m *Module) OnLoad(_ *discordgo.Session) error {
	log.Println("[info] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[info] module unloaded")
	return nil
}
