// Package roles lets server admins define self-assignable roles. Users pick
// them up (or drop them) via /role join and /role leave.
package roles

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/bot"
)

// AssignableRole is one role admins have made available for self-assignment.
type AssignableRole struct {
	RoleID      string `json:"role_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	JoinMessage string `json:"join_message"`
	Color       int    `json:"color"`
}

type guildConfig struct {
	Roles map[string]AssignableRole `json:"roles"` // key: roleID
}

// Module implements bot.Module for self-assignable roles.
type Module struct {
	dataFile string
	mu       sync.Mutex
	data     map[string]*guildConfig // key: guildID
}

func New(dataFile string) *Module {
	return &Module{
		dataFile: dataFile,
		data:     make(map[string]*guildConfig),
	}
}

func (m *Module) Name() string          { return "roles" }
func (m *Module) Description() string   { return "Lets admins define self-assignable roles; users join or leave them with /role." }
func (m *Module) Category() bot.Category { return bot.CategoryFunctional }

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "role",
			Description: "Join or leave a self-assignable role on this server",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "list",
					Description: "See all self-assignable roles on this server",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "join",
					Description: "Assign yourself a role",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:         discordgo.ApplicationCommandOptionString,
							Name:         "role",
							Description:  "Which role to join",
							Required:     true,
							Autocomplete: true,
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "leave",
					Description: "Remove a role from yourself",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:         discordgo.ApplicationCommandOptionString,
							Name:         "role",
							Description:  "Which role to leave",
							Required:     true,
							Autocomplete: true,
						},
					},
				},
			},
		},
	}
}

func (m *Module) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.GuildID == "" || i.Member == nil {
		ephemeralRespond(s, i, "This command can only be used in a server.")
		return
	}

	if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
		m.handleAutocomplete(s, i)
		return
	}

	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		return
	}
	switch opts[0].Name {
	case "list":
		m.handleList(s, i)
	case "join":
		m.handleJoin(s, i, opts[0])
	case "leave":
		m.handleLeave(s, i, opts[0])
	}
}

func (m *Module) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		return
	}
	sub := opts[0]

	var query string
	for _, opt := range sub.Options {
		if opt.Focused {
			query = strings.ToLower(opt.StringValue())
			break
		}
	}

	m.mu.Lock()
	cfg := m.getOrCreate(i.GuildID)
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, r := range cfg.Roles {
		if query == "" || strings.Contains(strings.ToLower(r.Name), query) {
			choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
				Name:  r.Name,
				Value: r.RoleID,
			})
		}
		if len(choices) == 25 {
			break
		}
	}
	m.mu.Unlock()

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
		log.Printf("[roles] autocomplete respond error: %v", err)
	}
}

func (m *Module) handleList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	m.mu.Lock()
	cfg := m.getOrCreate(i.GuildID)
	list := make([]AssignableRole, 0, len(cfg.Roles))
	for _, r := range cfg.Roles {
		list = append(list, r)
	}
	m.mu.Unlock()

	if len(list) == 0 {
		ephemeralRespond(s, i, "No self-assignable roles have been set up on this server yet.")
		return
	}

	var sb strings.Builder
	for _, r := range list {
		sb.WriteString(fmt.Sprintf("<@&%s>", r.RoleID))
		if r.Description != "" {
			sb.WriteString(fmt.Sprintf(" — %s", r.Description))
		}
		sb.WriteByte('\n')
	}

	embed := &discordgo.MessageEmbed{
		Title:       "Self-Assignable Roles",
		Description: sb.String(),
		Color:       0x5865F2,
		Footer:      &discordgo.MessageEmbedFooter{Text: "Use /role join <role> to sign up"},
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}},
	}); err != nil {
		log.Printf("[roles] list respond error: %v", err)
	}
}

func (m *Module) handleJoin(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	if len(sub.Options) == 0 {
		return
	}
	roleID := sub.Options[0].StringValue()

	m.mu.Lock()
	r, ok := m.getOrCreate(i.GuildID).Roles[roleID]
	m.mu.Unlock()

	if !ok {
		ephemeralRespond(s, i, "That role isn't available for self-assignment.")
		return
	}

	for _, id := range i.Member.Roles {
		if id == roleID {
			ephemeralRespond(s, i, fmt.Sprintf("You already have the **%s** role.", r.Name))
			return
		}
	}

	if err := s.GuildMemberRoleAdd(i.GuildID, i.Member.User.ID, roleID); err != nil {
		log.Printf("[roles] GuildMemberRoleAdd %s/%s: %v", i.GuildID, roleID, err)
		ephemeralRespond(s, i, "❌ Couldn't assign that role. Make sure I have **Manage Roles** permission and my role is above the target role.")
		return
	}

	msg := fmt.Sprintf("✅ You now have the **%s** role!", r.Name)
	if r.JoinMessage != "" {
		msg = r.JoinMessage
	}
	ephemeralRespond(s, i, msg)
}

func (m *Module) handleLeave(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	if len(sub.Options) == 0 {
		return
	}
	roleID := sub.Options[0].StringValue()

	m.mu.Lock()
	r, ok := m.getOrCreate(i.GuildID).Roles[roleID]
	m.mu.Unlock()

	if !ok {
		ephemeralRespond(s, i, "That role isn't available for self-assignment.")
		return
	}

	has := false
	for _, id := range i.Member.Roles {
		if id == roleID {
			has = true
			break
		}
	}
	if !has {
		ephemeralRespond(s, i, fmt.Sprintf("You don't have the **%s** role.", r.Name))
		return
	}

	if err := s.GuildMemberRoleRemove(i.GuildID, i.Member.User.ID, roleID); err != nil {
		log.Printf("[roles] GuildMemberRoleRemove %s/%s: %v", i.GuildID, roleID, err)
		ephemeralRespond(s, i, "❌ Couldn't remove that role.")
		return
	}

	ephemeralRespond(s, i, fmt.Sprintf("✅ Removed the **%s** role.", r.Name))
}

// ── Public API for the web layer ──────────────────────────────────────────────

func (m *Module) GuildRoles(guildID string) []AssignableRole {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.getOrCreate(guildID)
	out := make([]AssignableRole, 0, len(cfg.Roles))
	for _, r := range cfg.Roles {
		out = append(out, r)
	}
	return out
}

func (m *Module) AddRole(guildID string, role AssignableRole) {
	m.mu.Lock()
	m.getOrCreate(guildID).Roles[role.RoleID] = role
	m.mu.Unlock()
	m.save()
}

func (m *Module) RemoveRole(guildID, roleID string) {
	m.mu.Lock()
	delete(m.getOrCreate(guildID).Roles, roleID)
	m.mu.Unlock()
	m.save()
}

// ── Module lifecycle ──────────────────────────────────────────────────────────

func (m *Module) OnLoad(_ *discordgo.Session) error {
	m.load()
	log.Println("[roles] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[roles] module unloaded")
	return nil
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (m *Module) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		log.Printf("[roles] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(m.dataFile, data, 0600); err != nil {
		log.Printf("[roles] write %s error: %v", m.dataFile, err)
	}
}

func (m *Module) load() {
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[roles] read %s error: %v", m.dataFile, err)
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := json.Unmarshal(raw, &m.data); err != nil {
		log.Printf("[roles] parse %s error: %v", m.dataFile, err)
	}
}

// getOrCreate returns (or initialises) the config for a guild. Must hold m.mu.
func (m *Module) getOrCreate(guildID string) *guildConfig {
	if cfg, ok := m.data[guildID]; ok {
		return cfg
	}
	cfg := &guildConfig{Roles: make(map[string]AssignableRole)}
	m.data[guildID] = cfg
	return cfg
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func ephemeralRespond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[roles] respond error: %v", err)
	}
}
