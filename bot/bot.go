package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/config"
)

// Bot is the central Discord bot. It owns the discordgo session and coordinates
// all loaded modules.
type Bot struct {
	session *discordgo.Session
	cfg     *config.Config

	// mu guards all maps below. We use RWMutex because reads (interaction
	// routing) happen far more often than writes (module load/unload).
	mu sync.RWMutex

	// modules holds every loaded module, keyed by Module.Name().
	modules map[string]Module

	// cmdOwners maps a slash command name → the module that owns it.
	// This lets handleInteraction route in O(1) without searching all modules.
	cmdOwners map[string]Module

	// guildSettings tracks per-guild enable/disable state for each module.
	// Structure: map[guildID]map[moduleName]bool
	// A missing entry means "use the default", which is enabled.
	guildSettings map[string]map[string]bool

	// registeredCmds holds the ApplicationCommand objects returned by Discord
	// when commands are created. We need these IDs to delete commands on unload.
	registeredCmds map[string][]*discordgo.ApplicationCommand

	// commandPerms enforces per-guild role-based access control on slash commands.
	// All commands default to admin-only; admins use the web UI to grant roles.
	commandPerms *CommandPermissions

	// channelPerms enforces per-guild channel allow-lists on slash commands.
	// Empty allow-lists at every layer mean "all channels" — the default.
	channelPerms *ChannelPermissions

	// moderatorRoles tracks which Discord roles grant non-admin users access
	// to the web dashboard. Edited from the server page by admins.
	moderatorRoles *ModeratorRoles

	// auditChannels maps a guild ID to the channel that should receive an
	// embed every time a config change is made via the dashboard. Owner-
	// configured; empty entries disable auditing for that guild.
	auditChannels *AuditChannels

	startTime time.Time
}

// New creates a Bot and opens the underlying discordgo session.
// The session is not yet connected; call Start() to open the WebSocket.
func New(cfg *config.Config) (*Bot, error) {
	session, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		return nil, fmt.Errorf("creating discord session: %w", err)
	}

	// Intents declare which Gateway events Discord will send us.
	// Only request what you need — more intents mean more events and more
	// potential for rate limiting. Modules that need additional intents
	// (e.g. voice, DMs) should document that requirement.
	//
	// IntentsGuilds       — required for slash command routing.
	// IntentsGuildMessages — required for MessageCreate events (e.g. the meow module).
	// IntentsMessageContent — required to read the text of those messages.
	//   This is a PRIVILEGED intent: it must be enabled in the Discord developer
	//   portal for any bot in 100+ servers, and is recommended for everyone.
	//   Without it, message-content fields arrive empty and message-listening
	//   modules silently do nothing.
	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent

	b := &Bot{
		session:        session,
		cfg:            cfg,
		modules:        make(map[string]Module),
		cmdOwners:      make(map[string]Module),
		guildSettings:  make(map[string]map[string]bool),
		registeredCmds: make(map[string][]*discordgo.ApplicationCommand),
		commandPerms:   NewCommandPermissions(cfg.CommandPermsFile),
		channelPerms:   NewChannelPermissions(cfg.ChannelPermsFile),
		moderatorRoles: NewModeratorRoles(cfg.ModeratorRolesFile),
		auditChannels:  NewAuditChannels(cfg.AuditChannelsFile),
	}

	// Restore persisted per-guild module enable/disable state. Loaded before
	// any other goroutines exist so no lock is needed.
	b.loadModuleState()

	// Register the single interaction handler. discordgo calls this for every
	// slash command and component interaction. We then route internally.
	session.AddHandler(b.handleInteraction)

	// Register the single message-event handler. We dispatch internally to
	// any loaded module that implements bot.MessageHandler, after applying
	// the per-guild module-enable toggle and channel allow-list.
	session.AddHandler(b.handleMessage)

	return b, nil
}

// Start opens the WebSocket connection to Discord.
func (b *Bot) Start() error {
	b.startTime = time.Now()
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("opening discord session: %w", err)
	}
	if u := b.session.State.User; u != nil {
		log.Printf("[bot] connected as %s", u.Username)
	} else {
		log.Println("[bot] connected")
	}
	b.setOnlinePresence()
	b.applyBotDescription()
	return nil
}

// applyBotDescription pushes BotDescription (env BOT_DESCRIPTION) to the
// bot's Discord application profile so the "About Me" stays in sync with
// what's deployed. Failures are logged but non-fatal — the bot is fully
// usable without an updated profile description.
//
// Why not session.ApplicationUpdate? That helper hits PATCH /applications/{id},
// which Discord rejects for bot tokens with 403 "Bots cannot use this endpoint".
// The bot-accessible endpoint is PATCH /applications/@me, which discordgo
// v0.29 doesn't wrap — so we make a direct HTTP call with bot auth.
func (b *Bot) applyBotDescription() {
	if b.cfg.BotDescription == "" {
		return
	}

	payload, err := json.Marshal(struct {
		Description string `json:"description"`
	}{Description: b.cfg.BotDescription})
	if err != nil {
		log.Printf("[bot] marshal description: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPatch, "https://discord.com/api/v10/applications/@me", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[bot] build description request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bot "+b.cfg.BotToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[bot] update application description: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("[bot] update application description: HTTP %d: %s", resp.StatusCode, body)
		return
	}
	log.Println("[bot] updated application description (about me)")
}

// setOnlinePresence sets a static "online since MM/DD/YY HH:MM:SS" activity.
func (b *Bot) setOnlinePresence() {
	since := b.startTime.UTC().Format("01/02/06 15:04:05")
	if err := b.session.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status: "online",
		Activities: []*discordgo.Activity{{
			Name: "online since " + since,
			Type: discordgo.ActivityTypeCustom,
		}},
	}); err != nil {
		log.Printf("[bot] failed to set presence: %v", err)
	}
}

// StartTime returns the time at which Start() was called.
func (b *Bot) StartTime() time.Time { return b.startTime }

// Stop cleanly unloads all modules and closes the Discord connection.
func (b *Bot) Stop() {
	b.mu.Lock()
	for _, m := range b.modules {
		if err := m.OnUnload(b.session); err != nil {
			log.Printf("[bot] error unloading module %q: %v", m.Name(), err)
		}
	}
	b.mu.Unlock()
	b.session.Close()
}

// LoadModule registers a module with the bot and creates its slash commands.
//
// Slash commands are registered globally (guildID = ""). Global commands take
// up to 1 hour to appear in every server. During development you can speed
// this up by passing your test guild's ID instead of "" in the
// ApplicationCommandCreate calls below.
func (b *Bot) LoadModule(m Module) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	name := m.Name()
	if _, exists := b.modules[name]; exists {
		return fmt.Errorf("module %q is already loaded", name)
	}

	if err := m.OnLoad(b.session); err != nil {
		return fmt.Errorf("module %q OnLoad: %w", name, err)
	}

	var registered []*discordgo.ApplicationCommand
	// Every command is admin-only at the Discord level by default. Server-side
	// enforcement in handleInteraction is still authoritative; this just hides
	// commands from non-admins in the slash menu unless a guild admin grants
	// access via Discord's own integrations UI.
	adminOnly := int64(discordgo.PermissionManageGuild)
	for _, cmd := range m.Commands() {
		if cmd.DefaultMemberPermissions == nil {
			cmd.DefaultMemberPermissions = &adminOnly
		}
		created, err := b.session.ApplicationCommandCreate(b.cfg.ClientID, "", cmd)
		if err != nil {
			// Roll back already-created commands before returning the error.
			for _, c := range registered {
				_ = b.session.ApplicationCommandDelete(b.cfg.ClientID, "", c.ID)
			}
			return fmt.Errorf("registering command %q for module %q: %w", cmd.Name, name, err)
		}
		b.cmdOwners[cmd.Name] = m
		registered = append(registered, created)
		log.Printf("[bot] registered command /%s (module: %s)", cmd.Name, name)
	}

	b.modules[name] = m
	b.registeredCmds[name] = registered
	log.Printf("[bot] loaded module: %s", name)
	return nil
}

// UnloadModule removes a module and deletes its slash commands from Discord.
func (b *Bot) UnloadModule(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	m, exists := b.modules[name]
	if !exists {
		return fmt.Errorf("module %q is not loaded", name)
	}

	for _, cmd := range b.registeredCmds[name] {
		if err := b.session.ApplicationCommandDelete(b.cfg.ClientID, "", cmd.ID); err != nil {
			log.Printf("[bot] warning: failed to delete command %q: %v", cmd.Name, err)
		}
		delete(b.cmdOwners, cmd.Name)
	}

	if err := m.OnUnload(b.session); err != nil {
		log.Printf("[bot] error in OnUnload for module %q: %v", name, err)
	}

	delete(b.modules, name)
	delete(b.registeredCmds, name)
	log.Printf("[bot] unloaded module: %s", name)
	return nil
}

// Modules returns a snapshot of the currently loaded modules.
// We return a copy so callers can't mutate the internal map directly.
func (b *Bot) Modules() map[string]Module {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]Module, len(b.modules))
	for k, v := range b.modules {
		out[k] = v
	}
	return out
}

// Session exposes the underlying discordgo session (e.g. for the web layer to
// check which guilds the bot is in).
func (b *Bot) Session() *discordgo.Session {
	return b.session
}

// SetModuleEnabled enables or disables a module for a specific guild.
// The web UI calls this when an admin toggles a module on the server page.
// State is persisted to disk so toggles survive restarts.
func (b *Bot) SetModuleEnabled(guildID, moduleName string, enabled bool) {
	b.mu.Lock()
	if b.guildSettings[guildID] == nil {
		b.guildSettings[guildID] = make(map[string]bool)
	}
	b.guildSettings[guildID][moduleName] = enabled
	b.mu.Unlock()
	b.saveModuleState()
}

// IsModuleEnabled reports whether a module is enabled for a guild.
// Defaults to true when no setting has been saved yet.
func (b *Bot) IsModuleEnabled(guildID, moduleName string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if guild, ok := b.guildSettings[guildID]; ok {
		if enabled, ok := guild[moduleName]; ok {
			return enabled
		}
	}
	return true // default: all modules are on
}

// GuildModuleSettings returns a map of moduleName → enabled for a guild.
// Any module without an explicit setting is shown as enabled (the default).
func (b *Bot) GuildModuleSettings(guildID string) map[string]bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make(map[string]bool, len(b.modules))
	for name := range b.modules {
		out[name] = true // default
	}
	for name, enabled := range b.guildSettings[guildID] {
		out[name] = enabled
	}
	return out
}

// handleInteraction is the single entry point for all Discord interactions.
// It routes slash commands by command name and button/select interactions by
// the module-name prefix in the custom ID (e.g. "raid:join:..." → "raid" module).
func (b *Bot) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	var m Module
	var ok bool

	switch i.Type {
	case discordgo.InteractionApplicationCommand, discordgo.InteractionApplicationCommandAutocomplete:
		cmdName := i.ApplicationCommandData().Name
		b.mu.RLock()
		m, ok = b.cmdOwners[cmdName]
		b.mu.RUnlock()
		if !ok {
			log.Printf("[bot] received unknown command: /%s", cmdName)
			return
		}

	case discordgo.InteractionMessageComponent:
		// Custom IDs are prefixed with the owning module's name: "modulename:..."
		customID := i.MessageComponentData().CustomID
		prefix, _, _ := strings.Cut(customID, ":")
		b.mu.RLock()
		m, ok = b.modules[prefix]
		b.mu.RUnlock()
		if !ok {
			return
		}

	case discordgo.InteractionModalSubmit:
		// Same convention as components — module name is the customID prefix.
		// Lets a module flow from /command -> modal -> submit without us
		// needing per-module routing code here.
		customID := i.ModalSubmitData().CustomID
		prefix, _, _ := strings.Cut(customID, ":")
		b.mu.RLock()
		m, ok = b.modules[prefix]
		b.mu.RUnlock()
		if !ok {
			return
		}

	default:
		return
	}

	// Guild-specific enable/disable check. DM interactions have no GuildID.
	if i.GuildID != "" && !b.IsModuleEnabled(i.GuildID, m.Name()) {
		// Autocomplete and component interactions can't receive a plain message — drop silently.
		// Modal submits CAN respond, but a "module just got disabled" mid-flow
		// is rare enough to keep behaviour consistent with components.
		if i.Type == discordgo.InteractionApplicationCommandAutocomplete ||
			i.Type == discordgo.InteractionMessageComponent ||
			i.Type == discordgo.InteractionModalSubmit {
			return
		}
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ The **" + m.Name() + "** module is disabled on this server.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		}); err != nil {
			log.Printf("[bot] failed to send disabled-module response: %v", err)
		}
		return
	}

	// Per-guild role-lock gate. Applies to slash command invocations only —
	// component clicks (e.g. raid Join buttons) and autocomplete fall through
	// so a role-locked /raid create still lets regular members sign up.
	if i.Type == discordgo.InteractionApplicationCommand && i.GuildID != "" {
		cmdName := i.ApplicationCommandData().Name
		if !b.commandPerms.Allowed(i.GuildID, cmdName, i.Member) {
			if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "🔒 You don't have permission to use **/" + cmdName + "** on this server. A server admin can grant access on the dashboard.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			}); err != nil {
				log.Printf("[bot] failed to send permission-denied response: %v", err)
			}
			return
		}
	}

	// Per-guild channel allow-list gate (global + per-module, AND'd).
	// Slash commands only; components and autocomplete fall through.
	if i.Type == discordgo.InteractionApplicationCommand && i.GuildID != "" && i.ChannelID != "" {
		if !b.channelPerms.Allowed(i.GuildID, m.Name(), i.ChannelID) {
			if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "🚫 Prishe doesn't accept commands in this channel. A server admin can change this on the dashboard.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			}); err != nil {
				log.Printf("[bot] failed to send channel-denied response: %v", err)
			}
			return
		}
	}

	m.HandleInteraction(s, i)
}

// handleMessage dispatches MessageCreate events to every loaded module that
// implements MessageHandler, applying the per-guild module-enable toggle and
// channel allow-list. Bots' own messages are dropped early to avoid loops.
func (b *Bot) handleMessage(s *discordgo.Session, msg *discordgo.MessageCreate) {
	if msg.Author == nil || msg.Author.Bot {
		return
	}
	// Snapshot the modules under the lock so dispatch happens lock-free.
	b.mu.RLock()
	listeners := make([]Module, 0, len(b.modules))
	for _, m := range b.modules {
		if _, ok := m.(MessageHandler); ok {
			listeners = append(listeners, m)
		}
	}
	b.mu.RUnlock()

	for _, m := range listeners {
		if msg.GuildID != "" {
			if !b.IsModuleEnabled(msg.GuildID, m.Name()) {
				continue
			}
			if !b.channelPerms.Allowed(msg.GuildID, m.Name(), msg.ChannelID) {
				continue
			}
		}
		m.(MessageHandler).HandleMessage(s, msg)
	}
}

// CommandPerms exposes the role-lock store so the web dashboard can read
// and update per-guild allow-lists.
func (b *Bot) CommandPerms() *CommandPermissions { return b.commandPerms }

// ChannelPerms exposes the channel-allow-list store so the web dashboard can
// read and update per-guild settings.
func (b *Bot) ChannelPerms() *ChannelPermissions { return b.channelPerms }

// ModeratorRoles exposes the dashboard-moderator role store so the web layer
// can read it (at login, to expand the guild list) and write it (from the
// admin UI on the server page).
func (b *Bot) ModeratorRoles() *ModeratorRoles { return b.moderatorRoles }

// AuditChannels exposes the audit-channel store so the web layer can read it
// (when deciding whether to post an audit embed) and write it (from the
// owner UI on the server page).
func (b *Bot) AuditChannels() *AuditChannels { return b.auditChannels }

// CommandInfo describes one registered top-level slash command for the web UI.
type CommandInfo struct {
	Name   string
	Module string
}

// CommandIDByName returns the Discord-issued command ID for a top-level
// slash command, or "" if no command by that name is registered. The same ID
// is used in every guild because commands are registered globally.
func (b *Bot) CommandIDByName(name string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, cmds := range b.registeredCmds {
		for _, c := range cmds {
			if c.Name == name {
				return c.ID
			}
		}
	}
	return ""
}

// RegisteredCommands returns every command currently registered with Discord,
// sorted by command name. Used to render the role-lock UI.
func (b *Bot) RegisteredCommands() []CommandInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]CommandInfo, 0, len(b.cmdOwners))
	for name, m := range b.cmdOwners {
		out = append(out, CommandInfo{Name: name, Module: m.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
