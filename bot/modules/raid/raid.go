// Package raid provides FF14-style raid sign-ups backed by button interactions.
// The standard 8-person party composition is: 2 Tank, 2 Healer, 2 Melee DPS,
// 1 Ranged DPS, 1 Caster DPS. Members sign up by clicking role buttons on the
// posted embed, then choosing their specific job from a dropdown.
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

// slotDef describes one role group in the FF14 party composition.
type slotDef struct {
	role    string
	label   string
	emoji   string
	max     int
	style   discordgo.ButtonStyle
	iconURL string // xivapi.com role icon URL used as embed thumbnail
}

// stdComp is the FF14 full-party (8-person) standard composition.
var stdComp = []slotDef{
	{"tank",   "Tank",       "🛡️", 2, discordgo.PrimaryButton,   "https://xivapi.com/i/062000/062581.png"},
	{"healer", "Healer",     "💚", 2, discordgo.SuccessButton,   "https://xivapi.com/i/062000/062582.png"},
	{"melee",  "Melee DPS",  "⚔️", 2, discordgo.DangerButton,   "https://xivapi.com/i/062000/062583.png"},
	{"ranged", "Ranged DPS", "🏹", 1, discordgo.SecondaryButton, "https://xivapi.com/i/062000/062584.png"},
	{"caster", "Caster DPS", "🔮", 1, discordgo.PrimaryButton,   "https://xivapi.com/i/062000/062585.png"},
}

// jobDef is one specific FF14 job within a role.
type jobDef struct {
	key     string // used in custom IDs and JSON
	name    string // display name
	iconURL string // xivapi.com job icon URL (empty = use role icon)
}

// jobsByRole maps each role to the jobs players can choose from.
var jobsByRole = map[string][]jobDef{
	"tank": {
		{"paladin",    "Paladin",    "https://xivapi.com/cj/1/paladin.png"},
		{"warrior",    "Warrior",    "https://xivapi.com/cj/1/warrior.png"},
		{"darkknight", "Dark Knight","https://xivapi.com/cj/1/darkknight.png"},
		{"gunbreaker", "Gunbreaker", "https://xivapi.com/cj/1/gunbreaker.png"},
	},
	"healer": {
		{"whitemage",    "White Mage",   "https://xivapi.com/cj/1/whitemage.png"},
		{"scholar",      "Scholar",      "https://xivapi.com/cj/1/scholar.png"},
		{"astrologian",  "Astrologian",  "https://xivapi.com/cj/1/astrologian.png"},
		{"sage",         "Sage",         "https://xivapi.com/cj/1/sage.png"},
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
		{"blackmage",    "Black Mage",   "https://xivapi.com/cj/1/blackmage.png"},
		{"summoner",     "Summoner",     "https://xivapi.com/cj/1/summoner.png"},
		{"redmage",      "Red Mage",     "https://xivapi.com/cj/1/redmage.png"},
		{"pictomancer",  "Pictomancer",  "https://xivapi.com/cj/1/pictomancer.png"},
	},
}

// Signee is a player who has claimed a slot.
type Signee struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Job         string `json:"job,omitempty"` // jobDef.key, e.g. "paladin"
}

// Slot is one position in the party (e.g. "Tank slot 1").
type Slot struct {
	Role   string  `json:"role"`
	Signee *Signee `json:"signee,omitempty"`
}

// Raid represents one raid sign-up event.
type Raid struct {
	ID          string `json:"id"`
	GuildID     string `json:"guild_id"`
	ChannelID   string `json:"channel_id"`
	MessageID   string `json:"message_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	UnixTime    int64  `json:"unix_time,omitempty"`
	CreatorID   string `json:"creator_id"`
	Slots       []Slot `json:"slots"`
	Closed      bool   `json:"closed"`
}

func (r *Raid) isFull() bool {
	for _, s := range r.Slots {
		if s.Signee == nil {
			return false
		}
	}
	return true
}

// newSlots builds the 8-slot array for a fresh raid in composition order.
func newSlots() []Slot {
	slots := make([]Slot, 0, 8)
	for _, def := range stdComp {
		for range def.max {
			slots = append(slots, Slot{Role: def.role})
		}
	}
	return slots
}

// Module implements bot.Module for raid sign-ups.
type Module struct {
	dataFile string
	mu       sync.Mutex
	raids    map[string]*Raid // key: raidID
}

func New(dataFile string) *Module {
	return &Module{
		dataFile: dataFile,
		raids:    make(map[string]*Raid),
	}
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
							Description: "Raid ID shown in the sign-up embed footer",
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

	embed := buildEmbed(raid)
	components := buildComponents(raid)

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: components,
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
	channelID := raid.ChannelID
	messageID := raid.MessageID
	title := raid.Title
	m.mu.Unlock()
	m.save()

	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    channelID,
		ID:         messageID,
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: &components,
	}); err != nil {
		log.Printf("[raid] failed to update closed raid embed: %v", err)
	}

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
			fmt.Fprintf(&sb, "\n📅 <t:%d:F> (<t:%d:R>)", r.UnixTime, r.UnixTime)
		}
		fmt.Fprintf(&sb, "\n`ID: %s`\n\n", r.ID)
	}

	embed := &discordgo.MessageEmbed{
		Title:       "📋 Open Raid Sign-Ups",
		Description: strings.TrimSpace(sb.String()),
		Color:       0x1E3A5F,
		Footer:      &discordgo.MessageEmbedFooter{Text: "Use /raid close <id> to close a sign-up"},
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[raid] list respond error: %v", err)
	}
}

// ── Component (button / select menu) handlers ─────────────────────────────────

func (m *Module) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Custom ID formats:
	//   "raid:join:<raidID>:<role>"      — role button clicked
	//   "raid:select:<raidID>:<role>"    — job select menu submitted
	//   "raid:withdraw:<raidID>"         — withdraw button clicked
	parts := strings.SplitN(i.MessageComponentData().CustomID, ":", 4)
	if len(parts) < 3 {
		return
	}
	action := parts[1]
	raidID := parts[2]

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
	}
}

// handleJoin validates the slot is available and shows an ephemeral job picker.
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
	for _, sl := range raid.Slots {
		if sl.Signee != nil && sl.Signee.UserID == user.ID {
			m.mu.Unlock()
			ephemeralRespond(s, i, "You're already signed up for this raid. Click 🚪 **Withdraw** first to switch roles.")
			return
		}
	}
	// Count available slots for this role.
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

	// Build the job dropdown for this role.
	jobs := jobsByRole[role]
	options := make([]discordgo.SelectMenuOption, 0, len(jobs))
	for _, j := range jobs {
		options = append(options, discordgo.SelectMenuOption{
			Label: j.name,
			Value: j.key,
		})
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("Choose your **%s** job:", roleName(role)),
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    fmt.Sprintf("raid:select:%s:%s", raidID, role),
							Placeholder: "Select a job…",
							Options:     options,
						},
					},
				},
			},
		},
	}); err != nil {
		log.Printf("[raid] join respond error: %v", err)
	}
}

// handleJobSelect processes the job dropdown selection and claims the slot.
func (m *Module) handleJobSelect(s *discordgo.Session, i *discordgo.InteractionCreate, raidID, role string) {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	jobKey := values[0]

	// Resolve display name for the selected job.
	jobLabel := jobKey
	var jobIconURL string
	for _, j := range jobsByRole[role] {
		if j.key == jobKey {
			jobLabel = j.name
			jobIconURL = j.iconURL
			break
		}
	}
	_ = jobIconURL // available for future use (e.g. author icon)

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
	for _, sl := range raid.Slots {
		if sl.Signee != nil && sl.Signee.UserID == user.ID {
			m.mu.Unlock()
			updateEphemeral(s, i, "You're already signed up. Click 🚪 **Withdraw** first to switch roles.")
			return
		}
	}

	claimed := false
	for idx := range raid.Slots {
		if raid.Slots[idx].Role == role && raid.Slots[idx].Signee == nil {
			displayName := user.GlobalName
			if displayName == "" {
				displayName = user.Username
			}
			raid.Slots[idx].Signee = &Signee{
				UserID:      user.ID,
				DisplayName: displayName,
				Job:         jobKey,
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
	channelID := raid.ChannelID
	messageID := raid.MessageID
	closed := raid.Closed
	m.mu.Unlock()
	m.save()

	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    channelID,
		ID:         messageID,
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: &components,
	}); err != nil {
		log.Printf("[raid] failed to update embed after job select: %v", err)
	}

	msg := fmt.Sprintf("✅ Signed up as **%s**!", jobLabel)
	if closed {
		msg += "\n🔒 The raid is now full — sign-ups are closed."
	}
	updateEphemeral(s, i, msg)
}

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

	found := false
	for idx := range raid.Slots {
		if raid.Slots[idx].Signee != nil && raid.Slots[idx].Signee.UserID == user.ID {
			raid.Slots[idx].Signee = nil
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		ephemeralRespond(s, i, "You're not signed up for this raid.")
		return
	}

	embed := buildEmbed(raid)
	components := buildComponents(raid)
	channelID := raid.ChannelID
	messageID := raid.MessageID
	m.mu.Unlock()
	m.save()

	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    channelID,
		ID:         messageID,
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: &components,
	}); err != nil {
		log.Printf("[raid] failed to update embed after withdraw: %v", err)
	}

	ephemeralRespond(s, i, "✅ You've withdrawn from the raid. Slots are open again!")
}

// ── Embed & component builders ────────────────────────────────────────────────

func buildEmbed(raid *Raid) *discordgo.MessageEmbed {
	filledByRole := map[string]int{}
	for _, sl := range raid.Slots {
		if sl.Signee != nil {
			filledByRole[sl.Role]++
		}
	}

	var fields []*discordgo.MessageEmbedField

	// Date field sits above the roster.
	if raid.UnixTime != 0 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "📅 Date",
			Value:  fmt.Sprintf("<t:%d:F> (<t:%d:R>)", raid.UnixTime, raid.UnixTime),
			Inline: false,
		})
	}

	for _, def := range stdComp {
		var lines []string
		for _, sl := range raid.Slots {
			if sl.Role != def.role {
				continue
			}
			if sl.Signee != nil {
				line := fmt.Sprintf("<@%s>", sl.Signee.UserID)
				if sl.Signee.Job != "" {
					line += " — " + jobName(sl.Signee.Job)
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

	title := "📋 Raid Sign-Up: " + raid.Title
	if raid.Closed {
		title = "🔒 Raid Closed: " + raid.Title
	}

	embed := &discordgo.MessageEmbed{
		Title:  title,
		Color:  0x1E3A5F,
		Fields: fields,
		Footer: &discordgo.MessageEmbedFooter{Text: "ID: " + raid.ID},
	}
	if raid.Description != "" {
		embed.Description = raid.Description
	}

	// Thumbnail: icon of the first role that still needs players.
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

func buildComponents(raid *Raid) []discordgo.MessageComponent {
	filledByRole := map[string]int{}
	for _, sl := range raid.Slots {
		if sl.Signee != nil {
			filledByRole[sl.Role]++
		}
	}

	joinBtns := make([]discordgo.MessageComponent, 0, len(stdComp))
	for _, def := range stdComp {
		isFull := filledByRole[def.role] >= def.max
		joinBtns = append(joinBtns, discordgo.Button{
			Label:    def.emoji + " " + def.label,
			Style:    def.style,
			CustomID: fmt.Sprintf("raid:join:%s:%s", raid.ID, def.role),
			Disabled: isFull || raid.Closed,
		})
	}

	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: joinBtns},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "🚪 Withdraw",
				Style:    discordgo.DangerButton,
				CustomID: fmt.Sprintf("raid:withdraw:%s", raid.ID),
				Disabled: raid.Closed,
			},
		}},
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

// jobName returns the display name for a job key, searching all roles.
func jobName(key string) string {
	for _, jobs := range jobsByRole {
		for _, j := range jobs {
			if j.key == key {
				return j.name
			}
		}
	}
	return key
}

func newID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		log.Printf("[raid] newID rand error: %v", err)
		return "00000000"
	}
	return fmt.Sprintf("%08x", b)
}

func ephemeralRespond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[raid] respond error: %v", err)
	}
}

// updateEphemeral edits the existing ephemeral message in-place (used to
// replace the job select dropdown with a confirmation or error message).
func updateEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Components: []discordgo.MessageComponent{},
		},
	}); err != nil {
		log.Printf("[raid] update ephemeral error: %v", err)
	}
}
