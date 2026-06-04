// Package info provides /serverinfo, /botinfo, and /modules slash commands.
//
// The bot's "About Me" description is owned by Bot.applyBotDescription
// (configured via BOT_DESCRIPTION). This module used to also push stats into
// the description on load, but that endpoint required an owner token and
// always failed for bots - the call was removed in favour of the single
// canonical updater.
package info

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/bot"
)

const discordBlurple = 0x5865F2

// ModuleListing is one row in the /modules response.
type ModuleListing struct {
	Name        string
	Description string
	Category    bot.Category
	Commands    []string // top-level command names, with leading slash
	Enabled     bool     // is the module enabled for the guild being queried
}

// ListModulesFunc returns the full set of loaded modules and their per-guild
// enable state. Called at command-run time, so a closure that references the
// live bot is fine even when this module is loaded before others.
type ListModulesFunc func(guildID string) []ModuleListing

// CommandHelp is a single command's metadata as exposed to /help - enough
// to render signatures and a description, without dragging the full
// discordgo.ApplicationCommand into the info module's surface.
type CommandHelp struct {
	Module      string
	Category    bot.Category
	Name        string
	Description string
	Options     []*discordgo.ApplicationCommandOption
	Enabled     bool // is the owning module enabled in the guild being queried
}

// AllCommandsFunc returns every command registered by every module, with
// the enabled flag reflecting the guild's per-module settings. Like
// ListModulesFunc, this is called at request time so a closure capturing
// the bot sees the live state regardless of module load order.
type AllCommandsFunc func(guildID string) []CommandHelp

// Module implements bot.Module for server and bot information commands.
type Module struct {
	appID       string
	startTime   time.Time
	listModules ListModulesFunc
	allCommands AllCommandsFunc
}

// New creates the info module. appID is the Discord application / client ID;
// startTime is when the process started (used in both status and bio);
// listModules supplies the data shown by /modules at command-run time;
// allCommands supplies the data shown by /help.
func New(appID string, startTime time.Time, listModules ListModulesFunc, allCommands AllCommandsFunc) *Module {
	return &Module{
		appID:       appID,
		startTime:   startTime,
		listModules: listModules,
		allCommands: allCommands,
	}
}

func (m *Module) Name() string { return "info" }
func (m *Module) Description() string {
	return "Provides /serverinfo, /botinfo, and /modules slash commands."
}
func (m *Module) Category() bot.Category { return bot.CategoryFunctional }

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	// help is on every-member access by default - clear the admin-only
	// default that bot.LoadModule would otherwise stamp onto it.
	allMembers := int64(discordgo.PermissionViewChannel)
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
		{
			Name:                     "help",
			Description:              "Show every available command with usage examples (only you see the reply)",
			DefaultMemberPermissions: &allMembers,
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
	case "help":
		m.helpList(s, i)
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
// The response is ephemeral - this is admin-facing config, not a public reply.
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

	// Bucket enabled modules by category; track disabled ones for the footer.
	byCategory := make(map[bot.Category][]ModuleListing)
	enabledCount := 0
	var disabled []string
	for _, ml := range listings {
		if !ml.Enabled {
			disabled = append(disabled, ml.Name)
			continue
		}
		cat := ml.Category
		if cat == "" {
			cat = bot.CategoryFunctional
		}
		byCategory[cat] = append(byCategory[cat], ml)
		enabledCount++
	}

	categoryEmoji := map[bot.Category]string{
		bot.CategoryFunctional: "🛠️",
		bot.CategoryFun:        "🎉",
	}

	var fields []*discordgo.MessageEmbedField
	for _, cat := range bot.CategoryOrder {
		mods := byCategory[cat]
		if len(mods) == 0 {
			continue
		}
		var lines []string
		for _, ml := range mods {
			line := "**" + ml.Name + "** - " + ml.Description
			if len(ml.Commands) > 0 {
				cmds := make([]string, len(ml.Commands))
				for idx, c := range ml.Commands {
					cmds[idx] = "`" + c + "`"
				}
				line += "\n" + strings.Join(cmds, " ")
			}
			lines = append(lines, line)
		}
		emoji := categoryEmoji[cat]
		if emoji == "" {
			emoji = "📦"
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("%s %s · %d", emoji, cat, len(mods)),
			Value: strings.Join(lines, "\n\n"),
		})
	}

	embed := &discordgo.MessageEmbed{
		Title: "Active modules",
		Color: discordBlurple,
	}
	if enabledCount == 0 {
		embed.Description = "_No modules are currently enabled in this server._"
	} else {
		embed.Description = fmt.Sprintf("**%d** module%s enabled in this server.",
			enabledCount, pluralS(enabledCount))
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

// helpList responds with an ephemeral embed listing every command available
// to the user in this guild, grouped by module category and showing the
// signature (subcommand paths + parameter placeholders) for each. Commands
// from disabled modules are omitted but called out in the footer.
func (m *Module) helpList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID == "" {
		respondError(s, i, "This command only works in a server.")
		return
	}
	if m.allCommands == nil {
		respondError(s, i, "Command listing isn't wired up.")
		return
	}

	all := m.allCommands(i.GuildID)

	// Group enabled commands by category, then by module name. Track which
	// modules are disabled so we can list them in the footer.
	type cmdByModule struct {
		Name string
		Cmds []CommandHelp
	}
	byCategory := map[bot.Category]map[string]*cmdByModule{}
	disabledModules := map[string]struct{}{}
	for _, ch := range all {
		if !ch.Enabled {
			disabledModules[ch.Module] = struct{}{}
			continue
		}
		cat := ch.Category
		if cat == "" {
			cat = bot.CategoryFunctional
		}
		if byCategory[cat] == nil {
			byCategory[cat] = map[string]*cmdByModule{}
		}
		bucket := byCategory[cat][ch.Module]
		if bucket == nil {
			bucket = &cmdByModule{Name: ch.Module}
			byCategory[cat][ch.Module] = bucket
		}
		bucket.Cmds = append(bucket.Cmds, ch)
	}

	categoryEmoji := map[bot.Category]string{
		bot.CategoryFunctional: "🛠️",
		bot.CategoryFun:        "🎉",
	}

	var fields []*discordgo.MessageEmbedField
	for _, cat := range bot.CategoryOrder {
		mods := byCategory[cat]
		if len(mods) == 0 {
			continue
		}
		// Render module sub-lists in sorted order for stable output.
		modNames := make([]string, 0, len(mods))
		for name := range mods {
			modNames = append(modNames, name)
		}
		sort.Strings(modNames)

		var blocks []string
		for _, name := range modNames {
			bucket := mods[name]
			sort.Slice(bucket.Cmds, func(a, b int) bool {
				return bucket.Cmds[a].Name < bucket.Cmds[b].Name
			})
			var lines []string
			lines = append(lines, "**"+name+"**")
			for _, ch := range bucket.Cmds {
				for _, sig := range buildSigs("/"+ch.Name, ch.Options) {
					lines = append(lines, "`"+sig+"`")
				}
				if ch.Description != "" {
					lines = append(lines, "  ↳ "+ch.Description)
				}
			}
			blocks = append(blocks, strings.Join(lines, "\n"))
		}

		emoji := categoryEmoji[cat]
		if emoji == "" {
			emoji = "📦"
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("%s %s", emoji, cat),
			Value: strings.Join(blocks, "\n\n"),
		})
	}

	embed := &discordgo.MessageEmbed{
		Title:       "Prishe - Available commands",
		Description: "Pick a command from the slash menu, or type it directly. Parameters: `<required>` / `[optional]`.",
		Color:       discordBlurple,
		Fields:      fields,
	}
	if len(fields) == 0 {
		embed.Description = "_No commands are available right now - every module is disabled in this server._"
	}
	if len(disabledModules) > 0 {
		names := make([]string, 0, len(disabledModules))
		for n := range disabledModules {
			names = append(names, n)
		}
		sort.Strings(names)
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: "Commands from disabled modules hidden: " + strings.Join(names, ", "),
		}
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[info] failed to respond to /help: %v", err)
	}
}

// buildSigs flattens a slash command into one signature string per leaf path.
// Mirrors web/handlers.go's buildSigs - kept duplicated here to keep the info
// module standalone from the web package.
func buildSigs(prefix string, opts []*discordgo.ApplicationCommandOption) []string {
	if len(opts) > 0 &&
		(opts[0].Type == discordgo.ApplicationCommandOptionSubCommand ||
			opts[0].Type == discordgo.ApplicationCommandOptionSubCommandGroup) {
		var out []string
		for _, o := range opts {
			out = append(out, buildSigs(prefix+" "+o.Name, o.Options)...)
		}
		return out
	}
	var sb strings.Builder
	sb.WriteString(prefix)
	for _, o := range opts {
		if o.Required {
			fmt.Fprintf(&sb, " <%s>", o.Name)
		} else {
			fmt.Fprintf(&sb, " [%s]", o.Name)
		}
	}
	return []string{sb.String()}
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
