// Package raid provides FF14-style raid sign-ups backed by button interactions.
// Standard 8-person composition: 2 Tank · 2 Healer · 2 Melee · 1 Ranged · 1 Caster.
// Players click a role button → pick their specific job from a dropdown → get a
// numbered spot in the roster. Bench / Late / Tentative / Absence are tracked
// separately below the main roster in the embed.
package raid

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// ── Composition definitions ───────────────────────────────────────────────────

type slotDef struct {
	role    string
	label   string
	emoji   string
	max     int
	style   discordgo.ButtonStyle
	iconURL string // xivapi role icon used as embed thumbnail
}

var stdComp = []slotDef{
	{"tank",   "Tank",       "🛡️", 2, discordgo.PrimaryButton,   "https://xivapi.com/i/062000/062581.png"},
	{"healer", "Healer",     "💚", 2, discordgo.SuccessButton,   "https://xivapi.com/i/062000/062582.png"},
	{"melee",  "Melee DPS",  "⚔️", 2, discordgo.DangerButton,   "https://xivapi.com/i/062000/062583.png"},
	{"ranged", "Ranged DPS", "🏹", 1, discordgo.SecondaryButton, "https://xivapi.com/i/062000/062584.png"},
	{"caster", "Caster DPS", "🔮", 1, discordgo.PrimaryButton,   "https://xivapi.com/i/062000/062585.png"},
}

type jobDef struct {
	key     string
	name    string
	iconURL string
}

var jobsByRole = map[string][]jobDef{
	"tank": {
		{"paladin",    "Paladin",     "https://xivapi.com/cj/1/paladin.png"},
		{"warrior",    "Warrior",     "https://xivapi.com/cj/1/warrior.png"},
		{"darkknight", "Dark Knight", "https://xivapi.com/cj/1/darkknight.png"},
		{"gunbreaker", "Gunbreaker",  "https://xivapi.com/cj/1/gunbreaker.png"},
	},
	"healer": {
		{"whitemage",   "White Mage",  "https://xivapi.com/cj/1/whitemage.png"},
		{"scholar",     "Scholar",     "https://xivapi.com/cj/1/scholar.png"},
		{"astrologian", "Astrologian", "https://xivapi.com/cj/1/astrologian.png"},
		{"sage",        "Sage",        "https://xivapi.com/cj/1/sage.png"},
	},
	"melee": {
		{"monk",    "Monk",    "https://xivapi.com/cj/1/monk.png"},
		{"dragoon", "Dragoon", "https://xivapi.com/cj/1/dragoon.png"},
		{"ninja",   "Ninja",   "https://xivapi.com/cj/1/ninja.png"},
		{"samurai", "Samurai", "https://xivapi.com/cj/1/samurai.png"},
		{"reaper",  "Reaper",  "https://xivapi.com/cj/1/reaper.png"},
		{"viper",   "Viper",   "https://xivapi.com/cj/1/viper.png"},
	},
	"ranged": {
		{"bard",      "Bard",      "https://xivapi.com/cj/1/bard.png"},
		{"machinist", "Machinist", "https://xivapi.com/cj/1/machinist.png"},
		{"dancer",    "Dancer",    "https://xivapi.com/cj/1/dancer.png"},
	},
	"caster": {
		{"blackmage",   "Black Mage",  "https://xivapi.com/cj/1/blackmage.png"},
		{"summoner",    "Summoner",    "https://xivapi.com/cj/1/summoner.png"},
		{"redmage",     "Red Mage",    "https://xivapi.com/cj/1/redmage.png"},
		{"pictomancer", "Pictomancer", "https://xivapi.com/cj/1/pictomancer.png"},
	},
}

// All jobs flattened — used for bench/tentative job selection.
var allJobs []jobDef

func init() {
	for _, role := range []string{"tank", "healer", "melee", "ranged", "caster"} {
		allJobs = append(allJobs, jobsByRole[role]...)
	}
}

// ── Data types ────────────────────────────────────────────────────────────────

type Signee struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Job         string `json:"job,omitempty"`
	Late        bool   `json:"late,omitempty"`
	Number      int    `json:"number"` // global sign-up order within this raid
}

type Slot struct {
	Role   string  `json:"role"`
	Signee *Signee `json:"signee,omitempty"`
}

// StatusEntry holds bench / tentative / absence registrations outside the main roster.
type StatusEntry struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Job         string `json:"job,omitempty"`
	Type        string `json:"type"`   // "bench" | "tentative" | "absence"
	Number      int    `json:"number"` // global sign-up order
}

type Raid struct {
	ID            string        `json:"id"`
	GuildID       string        `json:"guild_id"`
	ChannelID     string        `json:"channel_id"`
	MessageID     string        `json:"message_id"`
	Title         string        `json:"title"`
	Description   string        `json:"description"`
	UnixTime      int64         `json:"unix_time,omitempty"`
	CreatorID     string        `json:"creator_id"`
	Slots         []Slot        `json:"slots"`
	StatusEntries []StatusEntry `json:"status_entries,omitempty"`
	NextNumber    int           `json:"next_number"`
	Closed        bool          `json:"closed"`
}

func (r *Raid) isFull() bool {
	for _, s := range r.Slots {
		if s.Signee == nil {
			return false
		}
	}
	return true
}

// findUserAnywhere returns true if the user is in the main roster OR any status entry.
func (r *Raid) findUserAnywhere(userID string) bool {
	for _, sl := range r.Slots {
		if sl.Signee != nil && sl.Signee.UserID == userID {
			return true
		}
	}
	for _, se := range r.StatusEntries {
		if se.UserID == userID {
			return true
		}
	}
	return false
}

func (r *Raid) nextNum() int {
	r.NextNumber++
	return r.NextNumber
}

func newSlots() []Slot {
	slots := make([]Slot, 0, 8)
	for _, def := range stdComp {
		for range def.max {
			slots = append(slots, Slot{Role: def.role})
		}
	}
	return slots
}

// ── Module ────────────────────────────────────────────────────────────────────

type Module struct {
	dataFile string
	mu       sync.Mutex
	raids    map[string]*Raid
}

func New(dataFile string) *Module {
	return &Module{dataFile: dataFile, raids: make(map[string]*Raid)}
}

func (m *Module) Name() string        { return "raid" }
func (m *Module) Description() string { return "FF14-style raid sign-ups (2T/2H/2M/1R/1C). Post a sign-up embed and let members claim slots with buttons." }

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "raid",
			Description: "Create and manage FF14 raid sign-ups",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "create",
					Description: "Post a new FF14 raid sign-up (2T/2H/2M/1R/1C = 8 players)",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionString,
							Name:        "title",
							Description: "Raid name (e.g. \"Eden's Promise E12S\")",
							Required:    true,
						},
						{
							Type:        discordgo.ApplicationCommandOptionString,
							Name:        "description",
							Description: "Additional details (optional)",
							Required:    false,
						},
						{
							Type:        discordgo.ApplicationCommandOptionInteger,
							Name:        "date",
							Description: "Scheduled time as a Unix timestamp (e.g. from epochconverter.com)",
							Required:    false,
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "close",
					Description: "Close sign-ups for a raid (creator or server admin only)",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionString,
							Name:        "id",
							Description: "Raid ID shown in the embed footer",
							Required:    true,
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "list",
					Description: "List open raid sign-ups in this server",
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
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		opts := i.ApplicationCommandData().Options
		if len(opts) == 0 {
			return
		}
		switch opts[0].Name {
		case "create":
			m.handleCreate(s, i, opts[0])
		case "close":
			m.handleClose(s, i, opts[0])
		case "list":
			m.handleList(s, i)
		}
	case discordgo.InteractionMessageComponent:
		m.handleComponent(s, i)
	}
}

// ── Slash command handlers ────────────────────────────────────────────────────

func (m *Module) handleCreate(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	var title, description string
	var unixTime int64
	for _, opt := range sub.Options {
		switch opt.Name {
		case "title":
			title = opt.StringValue()
		case "description":
			description = opt.StringValue()
		case "date":
			unixTime = opt.IntValue()
		}
	}

	id := newID()
	raid := &Raid{
		ID:          id,
		GuildID:     i.GuildID,
		ChannelID:   i.ChannelID,
		Title:       title,
		Description: description,
		UnixTime:    unixTime,
		CreatorID:   i.Member.User.ID,
		Slots:       newSlots(),
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{buildEmbed(raid)},
			Components: buildComponents(raid),
		},
	}); err != nil {
		log.Printf("[raid] create respond error: %v", err)
		return
	}

	msg, err := s.InteractionResponse(i.Interaction)
	if err != nil {
		log.Printf("[raid] failed to fetch interaction response: %v", err)
		return
	}
	raid.MessageID = msg.ID

	m.mu.Lock()
	m.raids[id] = raid
	m.mu.Unlock()
	m.save()

	log.Printf("[raid] created raid %s (%s) in guild %s", id, title, i.GuildID)
}

func (m *Module) handleClose(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	if len(sub.Options) == 0 {
		return
	}
	id := strings.TrimSpace(sub.Options[0].StringValue())

	m.mu.Lock()
	raid, ok := m.raids[id]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Raid not found. Check the ID in the embed footer.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "Sign-ups for this raid are already closed.")
		return
	}
	isCreator := raid.CreatorID == i.Member.User.ID
	isAdmin := i.Member.Permissions&discordgo.PermissionManageServer != 0
	if !isCreator && !isAdmin {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Only the raid creator or a server admin can close sign-ups.")
		return
	}
	raid.Closed = true
	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID, messageID, title := raid.ChannelID, raid.MessageID, raid.Title
	m.mu.Unlock()
	m.save()

	s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID,
		Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
	})
	ephemeralRespond(s, i, fmt.Sprintf("🔒 Sign-ups for **%s** are now closed.", title))
}

func (m *Module) handleList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	m.mu.Lock()
	var open []*Raid
	for _, r := range m.raids {
		if r.GuildID == i.GuildID && !r.Closed {
			open = append(open, r)
		}
	}
	m.mu.Unlock()

	if len(open) == 0 {
		ephemeralRespond(s, i, "No open raids right now. Use `/raid create` to post one!")
		return
	}

	var sb strings.Builder
	for _, r := range open {
		filled := 0
		for _, sl := range r.Slots {
			if sl.Signee != nil {
				filled++
			}
		}
		fmt.Fprintf(&sb, "**%s** — %d/8 signed up", r.Title, filled)
		if r.UnixTime != 0 {
			fmt.Fprintf(&sb, "\n📅 <t:%d:D> · 🕐 <t:%d:t> · ⏳ <t:%d:R>", r.UnixTime, r.UnixTime, r.UnixTime)
		}
		fmt.Fprintf(&sb, "\n`ID: %s`\n\n", r.ID)
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{
				Title:       "📋 Open Raid Sign-Ups",
				Description: strings.TrimSpace(sb.String()),
				Color:       0x1E3A5F,
				Footer:      &discordgo.MessageEmbedFooter{Text: "Use /raid close <id> to close a sign-up"},
			}},
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
}

// ── Component routing ─────────────────────────────────────────────────────────
//
// Custom ID format: "raid:<action>:<raidID>[:<extra>]"
//
//   join:<raidID>:<role>       — role button; shows job dropdown
//   select:<raidID>:<role>     — job dropdown for main roster
//   withdraw:<raidID>          — remove from roster / status list
//   bench:<raidID>             — bench button; shows job dropdown
//   selectbench:<raidID>       — job dropdown for bench
//   late:<raidID>              — toggle late status on main-roster slot
//   tentative:<raidID>         — shows job dropdown for tentative
//   selecttent:<raidID>        — job dropdown for tentative
//   absence:<raidID>           — toggle absence entry (no job needed)

func (m *Module) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	parts := strings.SplitN(i.MessageComponentData().CustomID, ":", 4)
	if len(parts) < 3 {
		return
	}
	action, raidID := parts[1], parts[2]

	switch action {
	case "join":
		if len(parts) < 4 {
			return
		}
		m.handleJoin(s, i, raidID, parts[3])
	case "select":
		if len(parts) < 4 {
			return
		}
		m.handleJobSelect(s, i, raidID, parts[3])
	case "withdraw":
		m.handleWithdraw(s, i, raidID)
	case "bench":
		m.handleBench(s, i, raidID)
	case "selectbench":
		m.handleBenchSelect(s, i, raidID)
	case "late":
		m.handleLate(s, i, raidID)
	case "tentative":
		m.handleTentative(s, i, raidID)
	case "selecttent":
		m.handleTentSelect(s, i, raidID)
	case "absence":
		m.handleAbsence(s, i, raidID)
	}
}

// ── Main roster join ──────────────────────────────────────────────────────────

func (m *Module) handleJoin(s *discordgo.Session, i *discordgo.InteractionCreate, raidID, role string) {
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "Sign-ups for this raid are closed.")
		return
	}
	if raid.findUserAnywhere(user.ID) {
		m.mu.Unlock()
		ephemeralRespond(s, i, "You're already registered for this raid. Click **Withdraw** first to change.")
		return
	}
	available := 0
	for _, sl := range raid.Slots {
		if sl.Role == role && sl.Signee == nil {
			available++
		}
	}
	m.mu.Unlock()

	if available == 0 {
		ephemeralRespond(s, i, fmt.Sprintf("All **%s** slots are taken!", roleName(role)))
		return
	}

	showJobSelect(s, i, fmt.Sprintf("raid:select:%s:%s", raidID, role),
		fmt.Sprintf("Choose your **%s** job:", roleName(role)),
		jobsByRole[role])
}

func (m *Module) handleJobSelect(s *discordgo.Session, i *discordgo.InteractionCreate, raidID, role string) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	jobKey := values[0]
	jobLabel := resolveJobName(jobKey)
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		updateEphemeral(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		updateEphemeral(s, i, "Sign-ups for this raid are closed.")
		return
	}
	if raid.findUserAnywhere(user.ID) {
		m.mu.Unlock()
		updateEphemeral(s, i, "You're already registered. Click **Withdraw** first to change.")
		return
	}
	claimed := false
	for idx := range raid.Slots {
		if raid.Slots[idx].Role == role && raid.Slots[idx].Signee == nil {
			raid.Slots[idx].Signee = &Signee{
				UserID:      user.ID,
				DisplayName: displayName(user),
				Job:         jobKey,
				Number:      raid.nextNum(),
			}
			claimed = true
			break
		}
	}
	if !claimed {
		m.mu.Unlock()
		updateEphemeral(s, i, fmt.Sprintf("All **%s** slots were just taken!", roleName(role)))
		return
	}
	if raid.isFull() {
		raid.Closed = true
	}
	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID, messageID, closed := raid.ChannelID, raid.MessageID, raid.Closed
	m.mu.Unlock()
	m.save()

	s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID,
		Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
	})

	msg := fmt.Sprintf("✅ Signed up as **%s**!", jobLabel)
	if closed {
		msg += "\n🔒 The raid is now full — sign-ups are closed."
	}
	updateEphemeral(s, i, msg)
}

// ── Withdraw ──────────────────────────────────────────────────────────────────

func (m *Module) handleWithdraw(s *discordgo.Session, i *discordgo.InteractionCreate, raidID string) {
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "Sign-ups for this raid are closed.")
		return
	}

	removed := false
	for idx := range raid.Slots {
		if raid.Slots[idx].Signee != nil && raid.Slots[idx].Signee.UserID == user.ID {
			raid.Slots[idx].Signee = nil
			removed = true
			break
		}
	}
	if !removed {
		newEntries := raid.StatusEntries[:0]
		for _, se := range raid.StatusEntries {
			if se.UserID == user.ID {
				removed = true
			} else {
				newEntries = append(newEntries, se)
			}
		}
		raid.StatusEntries = newEntries
	}

	if !removed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "You're not registered for this raid.")
		return
	}

	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID, messageID := raid.ChannelID, raid.MessageID
	m.mu.Unlock()
	m.save()

	s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID,
		Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
	})
	ephemeralRespond(s, i, "✅ You've been removed from the raid.")
}

// ── Bench ─────────────────────────────────────────────────────────────────────

func (m *Module) handleBench(s *discordgo.Session, i *discordgo.InteractionCreate, raidID string) {
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "Sign-ups for this raid are closed.")
		return
	}
	if raid.findUserAnywhere(user.ID) {
		m.mu.Unlock()
		ephemeralRespond(s, i, "You're already registered. Click **Withdraw** first to change.")
		return
	}
	m.mu.Unlock()

	showJobSelect(s, i, "raid:selectbench:"+raidID,
		"Choose your job for **Bench**:", allJobs)
}

func (m *Module) handleBenchSelect(s *discordgo.Session, i *discordgo.InteractionCreate, raidID string) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	jobKey := values[0]
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		updateEphemeral(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		updateEphemeral(s, i, "Sign-ups for this raid are closed.")
		return
	}
	if raid.findUserAnywhere(user.ID) {
		m.mu.Unlock()
		updateEphemeral(s, i, "You're already registered. Click **Withdraw** first to change.")
		return
	}
	raid.StatusEntries = append(raid.StatusEntries, StatusEntry{
		UserID:      user.ID,
		DisplayName: displayName(user),
		Job:         jobKey,
		Type:        "bench",
		Number:      raid.nextNum(),
	})
	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID, messageID := raid.ChannelID, raid.MessageID
	m.mu.Unlock()
	m.save()

	s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID,
		Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
	})
	updateEphemeral(s, i, fmt.Sprintf("🪑 Added to bench as **%s**.", resolveJobName(jobKey)))
}

// ── Late ──────────────────────────────────────────────────────────────────────

func (m *Module) handleLate(s *discordgo.Session, i *discordgo.InteractionCreate, raidID string) {
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "Sign-ups for this raid are closed.")
		return
	}

	// Find the user in the main roster and toggle late.
	found := false
	var msg string
	for idx := range raid.Slots {
		if raid.Slots[idx].Signee != nil && raid.Slots[idx].Signee.UserID == user.ID {
			raid.Slots[idx].Signee.Late = !raid.Slots[idx].Signee.Late
			if raid.Slots[idx].Signee.Late {
				msg = "🕐 Marked as **late**. Your spot is held."
			} else {
				msg = "✅ Late status removed."
			}
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		ephemeralRespond(s, i, "You need to sign up for a role first before marking yourself as late.")
		return
	}

	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID, messageID := raid.ChannelID, raid.MessageID
	m.mu.Unlock()
	m.save()

	s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID,
		Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
	})
	ephemeralRespond(s, i, msg)
}

// ── Tentative ─────────────────────────────────────────────────────────────────

func (m *Module) handleTentative(s *discordgo.Session, i *discordgo.InteractionCreate, raidID string) {
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "Sign-ups for this raid are closed.")
		return
	}
	if raid.findUserAnywhere(user.ID) {
		m.mu.Unlock()
		ephemeralRespond(s, i, "You're already registered. Click **Withdraw** first to change.")
		return
	}
	m.mu.Unlock()

	showJobSelect(s, i, "raid:selecttent:"+raidID,
		"Choose your job for **Tentative**:", allJobs)
}

func (m *Module) handleTentSelect(s *discordgo.Session, i *discordgo.InteractionCreate, raidID string) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	jobKey := values[0]
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		updateEphemeral(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		updateEphemeral(s, i, "Sign-ups for this raid are closed.")
		return
	}
	if raid.findUserAnywhere(user.ID) {
		m.mu.Unlock()
		updateEphemeral(s, i, "You're already registered. Click **Withdraw** first to change.")
		return
	}
	raid.StatusEntries = append(raid.StatusEntries, StatusEntry{
		UserID:      user.ID,
		DisplayName: displayName(user),
		Job:         jobKey,
		Type:        "tentative",
		Number:      raid.nextNum(),
	})
	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID, messageID := raid.ChannelID, raid.MessageID
	m.mu.Unlock()
	m.save()

	s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID,
		Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
	})
	updateEphemeral(s, i, fmt.Sprintf("⚖️ Marked as **tentative** as %s.", resolveJobName(jobKey)))
}

// ── Absence ───────────────────────────────────────────────────────────────────

func (m *Module) handleAbsence(s *discordgo.Session, i *discordgo.InteractionCreate, raidID string) {
	user := i.Member.User

	m.mu.Lock()
	raid, ok := m.raids[raidID]
	if !ok || raid.GuildID != i.GuildID {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Raid not found.")
		return
	}
	if raid.Closed {
		m.mu.Unlock()
		ephemeralRespond(s, i, "Sign-ups for this raid are closed.")
		return
	}

	// Toggle: if already marked absent, remove it.
	for idx, se := range raid.StatusEntries {
		if se.UserID == user.ID && se.Type == "absence" {
			raid.StatusEntries = append(raid.StatusEntries[:idx], raid.StatusEntries[idx+1:]...)
			embed := buildEmbed(raid)
			components := buildComponents(raid)
			channelID, messageID := raid.ChannelID, raid.MessageID
			m.mu.Unlock()
			m.save()
			s.ChannelMessageEditComplex(&discordgo.MessageEdit{
				Channel: channelID, ID: messageID,
				Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
			})
			ephemeralRespond(s, i, "✅ Absence removed.")
			return
		}
	}

	// Already signed up as something else — require withdraw first.
	if raid.findUserAnywhere(user.ID) {
		m.mu.Unlock()
		ephemeralRespond(s, i, "You're already registered. Click **Withdraw** first to change.")
		return
	}

	raid.StatusEntries = append(raid.StatusEntries, StatusEntry{
		UserID:      user.ID,
		DisplayName: displayName(user),
		Type:        "absence",
		Number:      raid.nextNum(),
	})
	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID, messageID := raid.ChannelID, raid.MessageID
	m.mu.Unlock()
	m.save()

	s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: channelID, ID: messageID,
		Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
	})
	ephemeralRespond(s, i, "❌ Marked as **absent**.")
}

// ── Embed builder ─────────────────────────────────────────────────────────────

func buildEmbed(raid *Raid) *discordgo.MessageEmbed {
	// ── Description: tagline + date line ────────────────────────────────────
	var descLines []string
	if raid.Description != "" {
		descLines = append(descLines, raid.Description)
	}
	if raid.UnixTime != 0 {
		descLines = append(descLines,
			fmt.Sprintf("📅 <t:%d:D>  ·  🕐 <t:%d:t>  ·  ⏳ <t:%d:R>", raid.UnixTime, raid.UnixTime, raid.UnixTime))
	}

	// ── Per-role roster fields (inline) ─────────────────────────────────────
	filledByRole := map[string]int{}
	for _, sl := range raid.Slots {
		if sl.Signee != nil {
			filledByRole[sl.Role]++
		}
	}

	var fields []*discordgo.MessageEmbedField
	for _, def := range stdComp {
		var lines []string
		for _, sl := range raid.Slots {
			if sl.Role != def.role {
				continue
			}
			if sl.Signee != nil {
				line := fmt.Sprintf("`%d` <@%s>", sl.Signee.Number, sl.Signee.UserID)
				if sl.Signee.Job != "" {
					line += " — " + resolveJobName(sl.Signee.Job)
				}
				if sl.Signee.Late {
					line += " *(late)*"
				}
				lines = append(lines, line)
			} else {
				lines = append(lines, "*(open)*")
			}
		}
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("%s %s (%d/%d)", def.emoji, def.label, filledByRole[def.role], def.max),
			Value:  strings.Join(lines, "\n"),
			Inline: true,
		})
	}

	// ── Status sections (non-inline, only if populated) ──────────────────────
	type statusSection struct {
		typ   string
		label string
	}
	for _, sec := range []statusSection{
		{"bench", "🪑 Bench"},
		{"tentative", "⚖️ Tentative"},
		{"absence", "❌ Absence"},
	} {
		var lines []string
		for _, se := range raid.StatusEntries {
			if se.Type != sec.typ {
				continue
			}
			line := fmt.Sprintf("`%d` <@%s>", se.Number, se.UserID)
			if se.Job != "" {
				line += " — " + resolveJobName(se.Job)
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   fmt.Sprintf("%s (%d)", sec.label, len(lines)),
				Value:  strings.Join(lines, "\n"),
				Inline: false,
			})
		}
	}

	// ── Assemble embed ───────────────────────────────────────────────────────
	title := "📋 " + raid.Title
	if raid.Closed {
		title = "🔒 " + raid.Title
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: strings.Join(descLines, "\n"),
		Color:       0x1E3A5F,
		Fields:      fields,
		Footer:      &discordgo.MessageEmbedFooter{Text: "ID: " + raid.ID},
	}

	// Thumbnail: first role that still needs players.
	if !raid.Closed {
		for _, def := range stdComp {
			if filledByRole[def.role] < def.max {
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: def.iconURL}
				break
			}
		}
	}

	return embed
}

// ── Component builder ─────────────────────────────────────────────────────────

func buildComponents(raid *Raid) []discordgo.MessageComponent {
	filledByRole := map[string]int{}
	for _, sl := range raid.Slots {
		if sl.Signee != nil {
			filledByRole[sl.Role]++
		}
	}

	// Row 1: role join buttons
	joinRow := make([]discordgo.MessageComponent, 0, len(stdComp))
	for _, def := range stdComp {
		joinRow = append(joinRow, discordgo.Button{
			Label:    def.emoji + " " + def.label,
			Style:    def.style,
			CustomID: fmt.Sprintf("raid:join:%s:%s", raid.ID, def.role),
			Disabled: filledByRole[def.role] >= def.max || raid.Closed,
		})
	}

	// Row 2: status buttons
	statusRow := []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "🪑 Bench",
			Style:    discordgo.SecondaryButton,
			CustomID: "raid:bench:" + raid.ID,
			Disabled: raid.Closed,
		},
		discordgo.Button{
			Label:    "🕐 Late",
			Style:    discordgo.SecondaryButton,
			CustomID: "raid:late:" + raid.ID,
			Disabled: raid.Closed,
		},
		discordgo.Button{
			Label:    "⚖️ Tentative",
			Style:    discordgo.SecondaryButton,
			CustomID: "raid:tentative:" + raid.ID,
			Disabled: raid.Closed,
		},
		discordgo.Button{
			Label:    "❌ Absence",
			Style:    discordgo.SecondaryButton,
			CustomID: "raid:absence:" + raid.ID,
			Disabled: raid.Closed,
		},
	}

	// Row 3: withdraw
	withdrawRow := []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "🚪 Withdraw",
			Style:    discordgo.DangerButton,
			CustomID: "raid:withdraw:" + raid.ID,
			Disabled: raid.Closed,
		},
	}

	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: joinRow},
		discordgo.ActionsRow{Components: statusRow},
		discordgo.ActionsRow{Components: withdrawRow},
	}
}

// ── Module lifecycle ──────────────────────────────────────────────────────────

func (m *Module) OnLoad(_ *discordgo.Session) error {
	m.load()
	log.Println("[raid] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[raid] module unloaded")
	return nil
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (m *Module) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := json.MarshalIndent(m.raids, "", "  ")
	if err != nil {
		log.Printf("[raid] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(m.dataFile, data, 0600); err != nil {
		log.Printf("[raid] write %s error: %v", m.dataFile, err)
	}
}

func (m *Module) load() {
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[raid] read %s error: %v", m.dataFile, err)
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := json.Unmarshal(raw, &m.raids); err != nil {
		log.Printf("[raid] parse %s error: %v", m.dataFile, err)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func roleName(role string) string {
	for _, def := range stdComp {
		if def.role == role {
			return def.label
		}
	}
	return role
}

func resolveJobName(key string) string {
	for _, jobs := range jobsByRole {
		for _, j := range jobs {
			if j.key == key {
				return j.name
			}
		}
	}
	return key
}

func displayName(u *discordgo.User) string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// showJobSelect sends an ephemeral job dropdown using the provided jobs list.
func showJobSelect(s *discordgo.Session, i *discordgo.InteractionCreate, customID, prompt string, jobs []jobDef) {
	options := make([]discordgo.SelectMenuOption, 0, len(jobs))
	for _, j := range jobs {
		options = append(options, discordgo.SelectMenuOption{
			Label: j.name,
			Value: j.key,
		})
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: prompt,
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.SelectMenu{
						CustomID:    customID,
						Placeholder: "Select a job…",
						Options:     options,
					},
				}},
			},
		},
	})
}

func ephemeralRespond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func updateEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Components: []discordgo.MessageComponent{},
		},
	})
}

func newID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		log.Printf("[raid] newID rand error: %v", err)
		return "00000000"
	}
	return fmt.Sprintf("%08x", b)
}
