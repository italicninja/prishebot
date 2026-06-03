// Package raid provides FF14-style raid sign-ups backed by button interactions.
// Standard 8-person composition: 2 Tank · 2 Healer · 2 Melee · 1 Ranged · 1 Caster.
//
// On startup, the module registers the five official FF14 role icons as Discord
// application emojis (idempotent: reuses any already uploaded). Those emojis are
// then used in role buttons and embed field headers instead of generic Unicode.
package raid

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/bot"
)

// ── Composition definitions ───────────────────────────────────────────────────

type slotDef struct {
	role      string
	label     string
	fallback  string // Unicode emoji shown when application emoji is unavailable
	max       int
	style     discordgo.ButtonStyle
	iconURL   string // xivapi.com role icon, downloaded and uploaded as app emoji
	emojiName string // Discord emoji name (must be alphanumeric + underscores)
}

var stdComp = []slotDef{
	{"tank",   "Tank",       "🛡️", 2, discordgo.PrimaryButton,   "https://xivapi.com/i/062000/062581.png", "prs_tank"},
	{"healer", "Healer",     "💚", 2, discordgo.SuccessButton,   "https://xivapi.com/i/062000/062582.png", "prs_healer"},
	{"melee",  "Melee DPS",  "⚔️", 2, discordgo.DangerButton,   "https://xivapi.com/i/062000/062583.png", "prs_melee"},
	{"ranged", "Ranged DPS", "🏹", 1, discordgo.SecondaryButton, "https://xivapi.com/i/062000/062584.png", "prs_ranged"},
	{"caster", "Caster DPS", "🔮", 1, discordgo.PrimaryButton,   "https://xivapi.com/i/062000/062585.png", "prs_caster"},
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
	Number      int    `json:"number"`
}

type Slot struct {
	Role   string  `json:"role"`
	Signee *Signee `json:"signee,omitempty"`
}

type StatusEntry struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Job         string `json:"job,omitempty"`
	Type        string `json:"type"`   // "bench" | "tentative" | "absence"
	Number      int    `json:"number"`
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

// hasStarted reports whether the scheduled event time has been reached.
// Raids without a scheduled time never "start" by the clock.
func (r *Raid) hasStarted() bool {
	return r.UnixTime > 0 && time.Now().Unix() >= r.UnixTime
}

func (r *Raid) isFull() bool {
	for _, s := range r.Slots {
		if s.Signee == nil {
			return false
		}
	}
	return true
}

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

// popMainSlot removes the user from the main roster and returns their Signee,
// or nil if they were not in the main roster.
func (r *Raid) popMainSlot(userID string) *Signee {
	for idx, sl := range r.Slots {
		if sl.Signee != nil && sl.Signee.UserID == userID {
			s := sl.Signee
			r.Slots[idx].Signee = nil
			return s
		}
	}
	return nil
}

// findStatusEntry returns a pointer to the user's StatusEntry, or nil.
func (r *Raid) findStatusEntry(userID string) *StatusEntry {
	for i := range r.StatusEntries {
		if r.StatusEntries[i].UserID == userID {
			return &r.StatusEntries[i]
		}
	}
	return nil
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
	appID    string // Discord application ID (= ClientID), used for application emoji API
	mu       sync.Mutex
	raids    map[string]*Raid

	// pingRoles[guildID] is the role ID admins can opt to ping when posting a
	// new raid. Empty entry = no ping role configured. Stored alongside
	// raids in the same data file for atomic persistence.
	pingRoles map[string]string

	// roleEmoji and jobEmoji are populated synchronously in OnLoad then never
	// modified again, so they are safe to read from interaction handlers without mu.
	roleEmoji map[string]*discordgo.ComponentEmoji // role key -> app emoji
	jobEmoji  map[string]*discordgo.ComponentEmoji // job key -> app emoji

	stopCh chan struct{}
}

// Embed colors. Red replaces the default once the scheduled start time arrives.
const (
	colorOpen    = 0x1E3A5F
	colorStarted = 0xED4245
)

func New(dataFile, appID string) *Module {
	return &Module{
		dataFile:  dataFile,
		appID:     appID,
		raids:     make(map[string]*Raid),
		pingRoles: make(map[string]string),
	}
}

// PingRoleID returns the configured "post-new-raid" ping role for a guild,
// or "" if none has been set.
func (m *Module) PingRoleID(guildID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pingRoles[guildID]
}

// SetPingRoleID stores (or clears, when roleID == "") the ping role for a guild.
func (m *Module) SetPingRoleID(guildID, roleID string) {
	m.mu.Lock()
	if roleID == "" {
		delete(m.pingRoles, guildID)
	} else {
		m.pingRoles[guildID] = roleID
	}
	m.mu.Unlock()
	m.save()
}

func (m *Module) Name() string          { return "raid" }
func (m *Module) Description() string   { return "FF14-style raid sign-ups (2T/2H/2M/1R/1C). Post a sign-up embed and let members claim slots with buttons." }
func (m *Module) Category() bot.Category { return bot.CategoryFunctional }

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

// ── Module lifecycle ──────────────────────────────────────────────────────────

func (m *Module) OnLoad(s *discordgo.Session) error {
	m.load()
	m.ensureEmojis(s)
	m.stopCh = make(chan struct{})
	go m.watchEventTimes(s)
	log.Println("[raid] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	if m.stopCh != nil {
		close(m.stopCh)
		m.stopCh = nil
	}
	log.Println("[raid] module unloaded")
	return nil
}

// watchEventTimes periodically scans persisted raids and auto-closes any whose
// scheduled start time has been reached. Closing flips the embed to red (via
// hasStarted in buildEmbed) and disables every sign-up button.
func (m *Module) watchEventTimes(s *discordgo.Session) {
	// Run an immediate sweep so raids whose time passed while the bot was offline
	// don't have to wait a full tick to be closed.
	m.sweepStartedRaids(s)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sweepStartedRaids(s)
		}
	}
}

func (m *Module) sweepStartedRaids(s *discordgo.Session) {
	type pending struct {
		channelID, messageID, title string
		embed                       *discordgo.MessageEmbed
		components                  []discordgo.MessageComponent
	}
	var todo []pending

	m.mu.Lock()
	for _, r := range m.raids {
		if r.Closed || !r.hasStarted() {
			continue
		}
		r.Closed = true
		todo = append(todo, pending{
			channelID:  r.ChannelID,
			messageID:  r.MessageID,
			title:      r.Title,
			embed:      m.buildEmbed(r),
			components: m.buildComponents(r),
		})
	}
	m.mu.Unlock()

	if len(todo) == 0 {
		return
	}
	m.save()

	for _, p := range todo {
		if p.messageID == "" {
			continue
		}
		if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    p.channelID,
			ID:         p.messageID,
			Embeds:     &[]*discordgo.MessageEmbed{p.embed},
			Components: &p.components,
		}); err != nil {
			log.Printf("[raid] auto-close edit failed for %q: %v", p.title, err)
			continue
		}
		log.Printf("[raid] auto-closed %q at scheduled start time", p.title)
	}
}

// ensureEmojis uploads FF14 role and job icons as Discord application emojis
// if they are not already present. Idempotent: existing emojis are reused.
// Missing uploads (e.g. 404 on newer jobs) are skipped silently; the embed
// falls back to text for any job whose icon could not be uploaded.
func (m *Module) ensureEmojis(s *discordgo.Session) {
	existing, err := s.ApplicationEmojis(m.appID)
	if err != nil {
		log.Printf("[raid] could not list application emojis: %v — icons will fall back to text/Unicode", err)
		return
	}

	byName := make(map[string]*discordgo.Emoji, len(existing))
	for _, e := range existing {
		byName[e.Name] = e
	}

	upload := func(name, iconURL string) *discordgo.ComponentEmoji {
		if e, ok := byName[name]; ok {
			log.Printf("[raid] reusing emoji %s (%s)", name, e.ID)
			return &discordgo.ComponentEmoji{ID: e.ID, Name: e.Name}
		}
		img, err := fetchImage(iconURL)
		if err != nil {
			log.Printf("[raid] skip emoji %s: %v", name, err)
			return nil
		}
		b64 := base64.StdEncoding.EncodeToString(img)
		created, err := s.ApplicationEmojiCreate(m.appID, &discordgo.EmojiParams{
			Name:  name,
			Image: "data:image/png;base64," + b64,
		})
		if err != nil {
			log.Printf("[raid] could not create emoji %s: %v", name, err)
			return nil
		}
		log.Printf("[raid] created emoji %s (%s)", name, created.ID)
		return &discordgo.ComponentEmoji{ID: created.ID, Name: created.Name}
	}

	// Role icons
	roleEmoji := make(map[string]*discordgo.ComponentEmoji, len(stdComp))
	for _, def := range stdComp {
		if e := upload(def.emojiName, def.iconURL); e != nil {
			roleEmoji[def.role] = e
		}
	}
	m.roleEmoji = roleEmoji

	// Job icons — emoji name is "prs_<key>" (e.g. "prs_darkknight")
	jobEmoji := make(map[string]*discordgo.ComponentEmoji, len(allJobs))
	for _, j := range allJobs {
		if e := upload("prs_"+j.key, j.iconURL); e != nil {
			jobEmoji[j.key] = e
		}
	}
	m.jobEmoji = jobEmoji
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
			Embeds:     []*discordgo.MessageEmbed{m.buildEmbed(raid)},
			Components: m.buildComponents(raid),
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
	if raid.CreatorID != i.Member.User.ID && i.Member.Permissions&discordgo.PermissionManageServer == 0 {
		m.mu.Unlock()
		ephemeralRespond(s, i, "❌ Only the raid creator or a server admin can close sign-ups.")
		return
	}
	raid.Closed = true
	embed := m.buildEmbed(raid)
	components := m.buildComponents(raid)
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

	m.showJobSelect(s, i, fmt.Sprintf("raid:select:%s:%s", raidID, role),
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
	embed := m.buildEmbed(raid)
	components := m.buildComponents(raid)
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

	embed := m.buildEmbed(raid)
	components := m.buildComponents(raid)
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
	// Already on bench — nothing to do.
	if se := raid.findStatusEntry(user.ID); se != nil {
		if se.Type == "bench" {
			m.mu.Unlock()
			ephemeralRespond(s, i, "You're already on bench.")
			return
		}
		m.mu.Unlock()
		ephemeralRespond(s, i, fmt.Sprintf("You're already marked as **%s**. Click **Withdraw** first to change.", se.Type))
		return
	}

	// Already in main roster — move to bench, keep job.
	if signee := raid.popMainSlot(user.ID); signee != nil {
		raid.StatusEntries = append(raid.StatusEntries, StatusEntry{
			UserID: signee.UserID, DisplayName: signee.DisplayName,
			Job: signee.Job, Type: "bench", Number: signee.Number,
		})
		embed := m.buildEmbed(raid)
		components := m.buildComponents(raid)
		channelID, messageID := raid.ChannelID, raid.MessageID
		m.mu.Unlock()
		m.save()
		s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel: channelID, ID: messageID,
			Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
		})
		ephemeralRespond(s, i, "🪑 Moved to bench. Your slot is now open for others.")
		return
	}

	// Not registered — show job picker.
	m.mu.Unlock()
	m.showJobSelect(s, i, "raid:selectbench:"+raidID, "Choose your job for **Bench**:", allJobs)
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
		UserID: user.ID, DisplayName: displayName(user),
		Job: jobKey, Type: "bench", Number: raid.nextNum(),
	})
	embed := m.buildEmbed(raid)
	components := m.buildComponents(raid)
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
	embed := m.buildEmbed(raid)
	components := m.buildComponents(raid)
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
	// Already tentative — nothing to do.
	if se := raid.findStatusEntry(user.ID); se != nil {
		if se.Type == "tentative" {
			m.mu.Unlock()
			ephemeralRespond(s, i, "You're already marked as tentative.")
			return
		}
		m.mu.Unlock()
		ephemeralRespond(s, i, fmt.Sprintf("You're already marked as **%s**. Click **Withdraw** first to change.", se.Type))
		return
	}

	// Already in main roster — move to tentative, keep job.
	if signee := raid.popMainSlot(user.ID); signee != nil {
		raid.StatusEntries = append(raid.StatusEntries, StatusEntry{
			UserID: signee.UserID, DisplayName: signee.DisplayName,
			Job: signee.Job, Type: "tentative", Number: signee.Number,
		})
		embed := m.buildEmbed(raid)
		components := m.buildComponents(raid)
		channelID, messageID := raid.ChannelID, raid.MessageID
		m.mu.Unlock()
		m.save()
		s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel: channelID, ID: messageID,
			Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
		})
		ephemeralRespond(s, i, "⚖️ Moved to tentative. Your slot is now open for others.")
		return
	}

	// Not registered — show job picker.
	m.mu.Unlock()
	m.showJobSelect(s, i, "raid:selecttent:"+raidID, "Choose your job for **Tentative**:", allJobs)
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
		UserID: user.ID, DisplayName: displayName(user),
		Job: jobKey, Type: "tentative", Number: raid.nextNum(),
	})
	embed := m.buildEmbed(raid)
	components := m.buildComponents(raid)
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
	// Toggle off if already absent.
	if se := raid.findStatusEntry(user.ID); se != nil && se.Type == "absence" {
		for idx, entry := range raid.StatusEntries {
			if entry.UserID == user.ID {
				raid.StatusEntries = append(raid.StatusEntries[:idx], raid.StatusEntries[idx+1:]...)
				break
			}
		}
		embed := m.buildEmbed(raid)
		components := m.buildComponents(raid)
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

	// Already in a different status — block.
	if se := raid.findStatusEntry(user.ID); se != nil {
		m.mu.Unlock()
		ephemeralRespond(s, i, fmt.Sprintf("You're already marked as **%s**. Click **Withdraw** first to change.", se.Type))
		return
	}

	// Already in main roster — move to absence, keep job.
	if signee := raid.popMainSlot(user.ID); signee != nil {
		raid.StatusEntries = append(raid.StatusEntries, StatusEntry{
			UserID: signee.UserID, DisplayName: signee.DisplayName,
			Job: signee.Job, Type: "absence", Number: signee.Number,
		})
		embed := m.buildEmbed(raid)
		components := m.buildComponents(raid)
		channelID, messageID := raid.ChannelID, raid.MessageID
		m.mu.Unlock()
		m.save()
		s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel: channelID, ID: messageID,
			Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components,
		})
		ephemeralRespond(s, i, "❌ Marked as **absent**. Your slot has been freed.")
		return
	}

	// Not registered at all — add to absence.
	raid.StatusEntries = append(raid.StatusEntries, StatusEntry{
		UserID: user.ID, DisplayName: displayName(user),
		Type: "absence", Number: raid.nextNum(),
	})
	embed := m.buildEmbed(raid)
	components := m.buildComponents(raid)
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

func (m *Module) buildEmbed(raid *Raid) *discordgo.MessageEmbed {
	var descLines []string
	if raid.Description != "" {
		descLines = append(descLines, raid.Description)
	}
	if raid.UnixTime != 0 {
		descLines = append(descLines,
			fmt.Sprintf("📅 <t:%d:D>  ·  🕐 <t:%d:t>  ·  ⏳ <t:%d:R>", raid.UnixTime, raid.UnixTime, raid.UnixTime))
	}

	filledByRole := map[string]int{}
	for _, sl := range raid.Slots {
		if sl.Signee != nil {
			filledByRole[sl.Role]++
		}
	}

	var fields []*discordgo.MessageEmbedField
	for _, def := range stdComp {
		icon := m.emojiText(def.role, def.fallback)
		var lines []string
		for _, sl := range raid.Slots {
			if sl.Role != def.role {
				continue
			}
			if sl.Signee != nil {
				line := fmt.Sprintf("`%d` <@%s>", sl.Signee.Number, sl.Signee.UserID)
				if sl.Signee.Job != "" {
					if e := m.jobEmoji[sl.Signee.Job]; e != nil {
						line += fmt.Sprintf(" <:%s:%s>", e.Name, e.ID)
					} else {
						line += " — " + resolveJobName(sl.Signee.Job)
					}
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
			Name:   fmt.Sprintf("%s %s (%d/%d)", icon, def.label, filledByRole[def.role], def.max),
			Value:  strings.Join(lines, "\n"),
			Inline: true,
		})
	}

	for _, sec := range []struct {
		typ, label string
	}{
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
				if e := m.jobEmoji[se.Job]; e != nil {
					line += fmt.Sprintf(" <:%s:%s>", e.Name, e.ID)
				} else {
					line += " — " + resolveJobName(se.Job)
				}
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

	title := "📋 " + raid.Title
	if raid.Closed {
		title = "🔒 " + raid.Title
	}

	color := colorOpen
	if raid.hasStarted() {
		color = colorStarted
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: strings.Join(descLines, "\n"),
		Color:       color,
		Fields:      fields,
		Footer:      &discordgo.MessageEmbedFooter{Text: "ID: " + raid.ID},
	}
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

func (m *Module) buildComponents(raid *Raid) []discordgo.MessageComponent {
	filledByRole := map[string]int{}
	for _, sl := range raid.Slots {
		if sl.Signee != nil {
			filledByRole[sl.Role]++
		}
	}

	joinRow := make([]discordgo.MessageComponent, 0, len(stdComp))
	for _, def := range stdComp {
		btn := discordgo.Button{
			Label:    def.label,
			Style:    def.style,
			CustomID: fmt.Sprintf("raid:join:%s:%s", raid.ID, def.role),
			Disabled: filledByRole[def.role] >= def.max || raid.Closed,
		}
		if e, ok := m.roleEmoji[def.role]; ok {
			btn.Emoji = e
			// Label is kept so screen readers and hover text still show the role name.
		} else {
			btn.Label = def.fallback + " " + def.label
		}
		joinRow = append(joinRow, btn)
	}

	statusRow := []discordgo.MessageComponent{
		discordgo.Button{Label: "🪑 Bench", Style: discordgo.SecondaryButton, CustomID: "raid:bench:" + raid.ID, Disabled: raid.Closed},
		discordgo.Button{Label: "🕐 Late", Style: discordgo.SecondaryButton, CustomID: "raid:late:" + raid.ID, Disabled: raid.Closed},
		discordgo.Button{Label: "⚖️ Tentative", Style: discordgo.SecondaryButton, CustomID: "raid:tentative:" + raid.ID, Disabled: raid.Closed},
		discordgo.Button{Label: "❌ Absence", Style: discordgo.SecondaryButton, CustomID: "raid:absence:" + raid.ID, Disabled: raid.Closed},
	}

	withdrawRow := []discordgo.MessageComponent{
		discordgo.Button{Label: "🚪 Withdraw", Style: discordgo.DangerButton, CustomID: "raid:withdraw:" + raid.ID, Disabled: raid.Closed},
	}

	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: joinRow},
		discordgo.ActionsRow{Components: statusRow},
		discordgo.ActionsRow{Components: withdrawRow},
	}
}

// ── Persistence ───────────────────────────────────────────────────────────────

// persistedData wraps the on-disk shape. Older files stored only a
// map[string]*Raid at the top level; load() falls back to that shape when
// the wrapped version fails to parse, so a fresh deploy doesn't lose data.
type persistedData struct {
	Raids     map[string]*Raid  `json:"raids"`
	PingRoles map[string]string `json:"ping_roles,omitempty"`
}

func (m *Module) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := json.MarshalIndent(persistedData{
		Raids:     m.raids,
		PingRoles: m.pingRoles,
	}, "", "  ")
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

	// Try the new wrapped shape first.
	var pd persistedData
	if err := json.Unmarshal(raw, &pd); err == nil && pd.Raids != nil {
		m.raids = pd.Raids
		if pd.PingRoles != nil {
			m.pingRoles = pd.PingRoles
		}
		return
	}

	// Fallback: legacy flat map[string]*Raid (no ping_roles key).
	if err := json.Unmarshal(raw, &m.raids); err != nil {
		log.Printf("[raid] parse %s error: %v", m.dataFile, err)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// emojiText returns "<:name:id>" if the role has an application emoji, else fallback.
func (m *Module) emojiText(role, fallback string) string {
	if e, ok := m.roleEmoji[role]; ok && e != nil && e.ID != "" {
		return fmt.Sprintf("<:%s:%s>", e.Name, e.ID)
	}
	return fallback
}

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

func (m *Module) showJobSelect(s *discordgo.Session, i *discordgo.InteractionCreate, customID, prompt string, jobs []jobDef) {
	options := make([]discordgo.SelectMenuOption, 0, len(jobs))
	for _, j := range jobs {
		opt := discordgo.SelectMenuOption{Label: j.name, Value: j.key}
		if e := m.jobEmoji[j.key]; e != nil {
			opt.Emoji = e
		}
		options = append(options, opt)
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: prompt,
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.SelectMenu{CustomID: customID, Placeholder: "Select a job…", Options: options},
				}},
			},
		},
	})
}

func ephemeralRespond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: discordgo.MessageFlagsEphemeral},
	})
}

func updateEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: content, Components: []discordgo.MessageComponent{}},
	})
}

func fetchImage(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func newID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		log.Printf("[raid] newID rand error: %v", err)
		return "00000000"
	}
	return fmt.Sprintf("%08x", b)
}

// ── Web calendar API ──────────────────────────────────────────────────────────

// RaidMember is a read-only view of one participant for the web UI.
type RaidMember struct {
	Number      int
	DisplayName string
	Job         string // job key, e.g. "darkknight"
	JobName     string // human-readable, e.g. "Dark Knight"
	JobIconURL  string // xivapi.com PNG URL
	Late        bool
	Status      string // "" | "bench" | "tentative" | "absence"
}

// RaidView is a read-only snapshot of a raid for the web UI.
type RaidView struct {
	ID       string
	Title    string
	UnixTime int64
	DateStr  string // pre-formatted date, e.g. "Apr 30, 2026 · 20:00 UTC"
	Closed   bool
	Accepted []RaidMember // main roster (filled slots)
	Maybe    []RaidMember // bench + tentative
	Declined []RaidMember // absence
}

// GuildRaids returns snapshots of all raids for a guild, newest first.
func (m *Module) GuildRaids(guildID string) []RaidView {
	m.mu.Lock()
	defer m.mu.Unlock()
	var views []RaidView
	for _, r := range m.raids {
		if r.GuildID == guildID {
			views = append(views, buildRaidView(r))
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].UnixTime != views[j].UnixTime {
			return views[i].UnixTime > views[j].UnixTime
		}
		return views[i].ID > views[j].ID
	})
	return views
}

// CloseRaid closes sign-ups for a raid by ID. Returns false if not found or already closed.
func (m *Module) CloseRaid(guildID, raidID string) bool {
	m.mu.Lock()
	r, ok := m.raids[raidID]
	if !ok || r.GuildID != guildID || r.Closed {
		m.mu.Unlock()
		return false
	}
	r.Closed = true
	m.mu.Unlock()
	m.save()
	return true
}

func buildRaidView(r *Raid) RaidView {
	v := RaidView{ID: r.ID, Title: r.Title, UnixTime: r.UnixTime, Closed: r.Closed}
	if r.UnixTime != 0 {
		v.DateStr = time.Unix(r.UnixTime, 0).UTC().Format("Jan 2, 2006 · 15:04 UTC")
	}
	for _, sl := range r.Slots {
		if sl.Signee == nil {
			continue
		}
		v.Accepted = append(v.Accepted, buildMemberView(sl.Signee.DisplayName, sl.Signee.Job, sl.Signee.Number, sl.Signee.Late, ""))
	}
	for _, se := range r.StatusEntries {
		mem := buildMemberView(se.DisplayName, se.Job, se.Number, false, se.Type)
		switch se.Type {
		case "bench", "tentative":
			v.Maybe = append(v.Maybe, mem)
		case "absence":
			v.Declined = append(v.Declined, mem)
		}
	}
	sort.Slice(v.Accepted, func(i, j int) bool { return v.Accepted[i].Number < v.Accepted[j].Number })
	sort.Slice(v.Maybe, func(i, j int) bool { return v.Maybe[i].Number < v.Maybe[j].Number })
	sort.Slice(v.Declined, func(i, j int) bool { return v.Declined[i].Number < v.Declined[j].Number })
	return v
}

// CreateRaidFromWeb posts a new raid embed to a Discord channel and persists it.
// This is the web-dashboard equivalent of the /raid create slash command.
//
// When pingRole is true AND a per-guild ping role is configured via
// SetPingRoleID, the message content is "<@&roleID>" with the matching
// AllowedMentions.Roles list so the ping actually fires. Without a
// configured role the flag is a no-op (we never invent a role to ping).
func (m *Module) CreateRaidFromWeb(s *discordgo.Session, guildID, channelID, title, description string, unixTime int64, pingRole bool) error {
	id := newID()
	r := &Raid{
		ID:          id,
		GuildID:     guildID,
		ChannelID:   channelID,
		Title:       title,
		Description: description,
		UnixTime:    unixTime,
		Slots:       newSlots(),
	}

	send := &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{m.buildEmbed(r)},
		Components: m.buildComponents(r),
	}
	if pingRole {
		if roleID := m.PingRoleID(guildID); roleID != "" {
			send.Content = "<@&" + roleID + ">"
			// Restrict the ping to exactly this role — Discord's default
			// would otherwise allow every mention in the payload to fire.
			send.AllowedMentions = &discordgo.MessageAllowedMentions{
				Roles: []string{roleID},
			}
		}
	}

	msg, err := s.ChannelMessageSendComplex(channelID, send)
	if err != nil {
		return fmt.Errorf("could not post raid embed to channel: %w", err)
	}
	r.MessageID = msg.ID

	m.mu.Lock()
	m.raids[id] = r
	m.mu.Unlock()
	m.save()

	log.Printf("[raid] created raid %s (%s) via web dashboard in guild %s", id, title, guildID)
	return nil
}

func buildMemberView(displayName, jobKey string, number int, late bool, status string) RaidMember {
	mem := RaidMember{Number: number, DisplayName: displayName, Job: jobKey, Late: late, Status: status}
	if jobKey != "" {
		mem.JobName = resolveJobName(jobKey)
		for _, j := range allJobs {
			if j.key == jobKey {
				mem.JobIconURL = j.iconURL
				break
			}
		}
	}
	return mem
}
