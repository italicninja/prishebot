// Package info provides /serverinfo and /botinfo slash commands.
// On load it also pushes all bot stats into the application's "About Me"
// description via PATCH /applications/@me so they appear on Prishe's
// Discord profile card.
package info

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const discordBlurple = 0x5865F2

// ModuleListing is one row in the /modules response, exposed to the bot
// loader via a closure so this package doesn't have to import bot/.
type ModuleListing struct {
	Name        string
	Description string
	Commands    []string // top-level command names, with leading slash
	Enabled     bool     // is the module enabled for the guild being queried
}

// ListModulesFunc returns the full set of loaded modules and their per-guild
// enable state. Called at command-run time, so a closure that references the
// live bot is fine even when this module is loaded before others.
type ListModulesFunc func(guildID string) []ModuleListing

// Module implements bot.Module for server and bot information commands.
type Module struct {
	appID       string
	startTime   time.Time
	listModules ListModulesFunc
}

// New creates the info module. appID is the Discord application / client ID;
// startTime is when the process started (used in both status and bio);
// listModules supplies the data shown by /modules at command-run time.
func New(appID string, startTime time.Time, listModules ListModulesFunc) *Module {
	return &Module{appID: appID, startTime: startTime, listModules: listModules}
}

func (m *Module) Name() string { return "info" }
func (m *Module) Description() string {
	return "Provides /serverinfo, /botinfo, and /modules slash commands."
}

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
		{
			Name:        "modules",
			Description: "List the modules currently active on this server",
		},
	}
}

// HandleInteraction dispatches to the correct handler based on the command name.
func (m *Module) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.ApplicationCommandData().Name {
	case "serverinfo":
		m.serverInfo(s, i)
	case "botinfo":
		m.botInfo(s, i)
	case "modules":
		m.modulesList(s, i)
	}
}

func (m *Module) OnLoad(s *discordgo.Session) error {
	// Update the application description (shown as "About Me" on Prishe's
	// Discord profile) with the key stats. This is a plain HTTP call so it
	// works before the WebSocket is opened.
	m.updateBio(s)
	log.Println("[info] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[info] module unloaded")
	return nil
}

// updateBio pushes stats into the application description via the Discord API.
// Errors are logged but not fatal — the bio is cosmetic.
func (m *Module) updateBio(s *discordgo.Session) {
	var lines []string
	lines = append(lines, "Online since "+m.startTime.UTC().Format("01/02/06 15:04:05 UTC"))
	lines = append(lines, "ID: "+m.appID)
	if env := os.Getenv("RAILWAY_ENVIRONMENT_NAME"); env != "" {
		lines = append(lines, "Environment: "+env)
	}
	if region := os.Getenv("RAILWAY_REPLICA_REGION"); region != "" {
		lines = append(lines, "Region: "+region)
	}
	if svc := os.Getenv("RAILWAY_SERVICE_NAME"); svc != "" {
		lines = append(lines, "Service: "+svc)
	}

	type appPatch struct {
		Description string `json:"description"`
	}
	body, _ := json.Marshal(appPatch{Description: strings.Join(lines, "\n")})

	endpoint := discordgo.EndpointApplication(m.appID)
	if _, err := s.RequestWithBucketID("PATCH", endpoint, body, endpoint); err != nil {
		log.Printf("[info] could not update application description: %v", err)
	} else {
		log.Println("[info] updated application description (bio)")
	}
}

// ── Slash commands ─────────────────────────────────────────────────────────

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
			{Name: "Owner",    Value: fmt.Sprintf("<@%s>", guild.OwnerID),        Inline: true},
			{Name: "Members",  Value: fmt.Sprintf("%d", guild.MemberCount),       Inline: true},
			{Name: "Locale",   Value: guild.PreferredLocale,                       Inline: true},
			{Name: "Channels", Value: fmt.Sprintf("%d", len(guild.Channels)),     Inline: true},
			{Name: "Roles",    Value: fmt.Sprintf("%d", len(guild.Roles)),        Inline: true},
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

	// Build description: all the stats that also live in the bio.
	var desc strings.Builder
	fmt.Fprintf(&desc, "Online since `%s`\n", m.startTime.UTC().Format("01/02/06 15:04:05 UTC"))
	fmt.Fprintf(&desc, "Active in **%d** server(s)\n", len(s.State.Guilds))
	fmt.Fprintf(&desc, "ID: `%s`\n", self.ID)
	if env := os.Getenv("RAILWAY_ENVIRONMENT_NAME"); env != "" {
		fmt.Fprintf(&desc, "Environment: `%s`\n", env)
	}
	if region := os.Getenv("RAILWAY_REPLICA_REGION"); region != "" {
		fmt.Fprintf(&desc, "Region: `%s`\n", region)
	}
	if svc := os.Getenv("RAILWAY_SERVICE_NAME"); svc != "" {
		fmt.Fprintf(&desc, "Service: `%s`", svc)
	}

	embed := &discordgo.MessageEmbed{
		Title:       self.Username,
		Description: strings.TrimRight(desc.String(), "\n"),
		Color:       discordBlurple,
	}
	if self.Avatar != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{
			URL: fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png", self.ID, self.Avatar),
		}
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}},
	}); err != nil {
		log.Printf("[info] failed to respond to /botinfo: %v", err)
	}
}

// modulesList responds with an embed listing the currently active modules in
// the guild, plus a footer note for any that are loaded but disabled.
// The response is ephemeral — this is admin-facing config, not a public reply.
func (m *Module) modulesList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID == "" {
		respondError(s, i, "This command only works in a server.")
		return
	}
	if m.listModules == nil {
		respondError(s, i, "Module listing isn't wired up.")
		return
	}

	listings := m.listModules(i.GuildID)

	var fields []*discordgo.MessageEmbedField
	var disabled []string
	for _, ml := range listings {
		if !ml.Enabled {
			disabled = append(disabled, ml.Name)
			continue
		}
		var lines []string
		if ml.Description != "" {
			lines = append(lines, ml.Description)
		}
		if len(ml.Commands) > 0 {
			cmds := make([]string, len(ml.Commands))
			for idx, c := range ml.Commands {
				cmds[idx] = "`" + c + "`"
			}
			lines = append(lines, "Commands: "+strings.Join(cmds, ", "))
		} else {
			lines = append(lines, "_No slash commands._")
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  "✅ " + ml.Name,
			Value: strings.Join(lines, "\n"),
		})
	}

	embed := &discordgo.MessageEmbed{
		Title: "Active modules",
		Color: discordBlurple,
	}
	if len(fields) == 0 {
		embed.Description = "_No modules are currently enabled in this server._"
	} else {
		embed.Description = fmt.Sprintf("**%d** module%s enabled in this server.",
			len(fields), pluralS(len(fields)))
		embed.Fields = fields
	}
	if len(disabled) > 0 {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: "Disabled: " + strings.Join(disabled, ", "),
		}
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[info] failed to respond to /modules: %v", err)
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
