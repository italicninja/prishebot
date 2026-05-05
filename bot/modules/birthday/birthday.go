// Package birthday wishes users a happy birthday with a random GIF chosen from
// the per-server GIF library uploaded via the web dashboard.
// Birthdays are stored globally per user; announcements fire per-server in
// whichever channel the admin has configured.
package birthday

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// entry holds one user's birthday. LastWished is keyed by guildID so the same
// birthday can be celebrated independently in every server the user shares with
// the bot, without re-firing on the same day.
type entry struct {
	Month      int               `json:"month"`
	Day        int               `json:"day"`
	LastWished map[string]string `json:"last_wished"` // guildID → "2006-01-02"
}

// guildConfig stores per-guild settings.
type guildConfig struct {
	ChannelID   string `json:"channel_id"`
	DisableGIFs bool   `json:"disable_gifs,omitempty"`
}

// legacyEntry is the v1/v2 on-disk entry shape, used only during migration.
type legacyEntry struct {
	Month      int    `json:"month"`
	Day        int    `json:"day"`
	ChannelID  string `json:"channel_id"`
	LastWished string `json:"last_wished"`
}

// persistedData is the v3 on-disk format.
type persistedData struct {
	Entries map[string]entry       `json:"entries"`       // key: userID (global)
	Configs map[string]guildConfig `json:"guild_configs"` // key: guildID
}

// Module implements bot.Module for birthday tracking.
type Module struct {
	dataFile string
	gifsDir  string
	baseURL  string
	mu       sync.Mutex
	entries  map[string]entry       // key: userID
	configs  map[string]guildConfig // key: guildID
	stop     chan struct{}
}

func New(dataFile, gifsDir, baseURL string) *Module {
	return &Module{
		dataFile: dataFile,
		gifsDir:  gifsDir,
		baseURL:  strings.TrimRight(baseURL, "/"),
		entries:  make(map[string]entry),
		configs:  make(map[string]guildConfig),
		stop:     make(chan struct{}),
	}
}

func (m *Module) Name() string        { return "birthday" }
func (m *Module) Description() string { return "Remembers birthdays and wishes users on their special day with a GIF." }

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	minMonth, maxMonth := 1.0, 12.0
	minDay, maxDay := 1.0, 31.0
	return []*discordgo.ApplicationCommand{
		{
			Name:        "birthday",
			Description: "Manage your birthday",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "set",
					Description: "Register your birthday so Prishe can celebrate it",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionInteger,
							Name:        "month",
							Description: "Month number (1 = January … 12 = December)",
							Required:    true,
							MinValue:    &minMonth,
							MaxValue:    maxMonth,
						},
						{
							Type:        discordgo.ApplicationCommandOptionInteger,
							Name:        "day",
							Description: "Day of the month (1–31)",
							Required:    true,
							MinValue:    &minDay,
							MaxValue:    maxDay,
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "check",
					Description: "See your registered birthday",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "remove",
					Description: "Remove your registered birthday",
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
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		return
	}
	switch opts[0].Name {
	case "set":
		m.handleSet(s, i, opts[0])
	case "check":
		m.handleCheck(s, i)
	case "remove":
		m.handleRemove(s, i)
	}
}

func (m *Module) handleSet(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	var month, day int
	for _, opt := range sub.Options {
		switch opt.Name {
		case "month":
			month = int(opt.IntValue())
		case "day":
			day = int(opt.IntValue())
		}
	}

	// Validate using a leap year so Feb 29 is accepted.
	probe := time.Date(2024, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if int(probe.Month()) != month || probe.Day() != day {
		ephemeralRespond(s, i, "❌ That isn't a valid date.")
		return
	}

	userID := i.Member.User.ID
	m.mu.Lock()
	e := m.entries[userID]
	e.Month = month
	e.Day = day
	m.entries[userID] = e
	m.mu.Unlock()
	m.save()

	ephemeralRespond(s, i, fmt.Sprintf("🎂 Got it! I'll wish you a happy birthday on **%s %d**.", time.Month(month), day))
}

func (m *Module) handleCheck(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := i.Member.User.ID
	m.mu.Lock()
	e, ok := m.entries[userID]
	m.mu.Unlock()

	if !ok {
		ephemeralRespond(s, i, "You haven't registered a birthday yet. Use `/birthday set` to add one.")
		return
	}
	ephemeralRespond(s, i, fmt.Sprintf("🎂 Your registered birthday is **%s %d**.", time.Month(e.Month), e.Day))
}

func (m *Module) handleRemove(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := i.Member.User.ID
	m.mu.Lock()
	_, ok := m.entries[userID]
	delete(m.entries, userID)
	m.mu.Unlock()

	if !ok {
		ephemeralRespond(s, i, "You don't have a birthday registered.")
		return
	}
	m.save()
	ephemeralRespond(s, i, "✅ Your birthday has been removed.")
}

func (m *Module) OnLoad(s *discordgo.Session) error {
	m.load()
	go m.run(s)
	log.Println("[birthday] module loaded")
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	close(m.stop)
	log.Println("[birthday] module unloaded")
	return nil
}

// run checks birthdays immediately (catches up if the bot was offline), then
// sleeps until 30 seconds past each midnight UTC before checking again.
func (m *Module) run(s *discordgo.Session) {
	m.checkBirthdays(s)
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 30, 0, time.UTC)
		select {
		case <-time.After(time.Until(next)):
			m.checkBirthdays(s)
		case <-m.stop:
			return
		}
	}
}

func (m *Module) checkBirthdays(s *discordgo.Session) {
	today := time.Now().UTC()
	dateStr := today.Format("2006-01-02")

	type birthdayUser struct {
		userID string
		e      entry
	}
	type guildTarget struct {
		guildID   string
		channelID string
	}

	m.mu.Lock()
	var users []birthdayUser
	for userID, e := range m.entries {
		if e.Month == int(today.Month()) && e.Day == today.Day() {
			users = append(users, birthdayUser{userID, e})
		}
	}
	var targets []guildTarget
	for guildID, cfg := range m.configs {
		if cfg.ChannelID != "" {
			targets = append(targets, guildTarget{guildID, cfg.ChannelID})
		}
	}
	m.mu.Unlock()

	for _, u := range users {
		for _, t := range targets {
			// Skip if already wished in this guild today.
			m.mu.Lock()
			alreadyWished := m.entries[u.userID].LastWished[t.guildID] == dateStr
			m.mu.Unlock()
			if alreadyWished {
				continue
			}

			// Only announce if the user is actually a member of this guild.
			if _, err := s.GuildMember(t.guildID, u.userID); err != nil {
				continue
			}

			gifURL := m.pickGIF(t.guildID)

			embed := &discordgo.MessageEmbed{
				Description: fmt.Sprintf("🎉 Happy Birthday <@%s>! Wishing you an amazing day! 🎂🥳", u.userID),
				Color:       0xff69b4,
			}
			if gifURL != "" {
				embed.Image = &discordgo.MessageEmbedImage{URL: gifURL}
			}

			if _, err := s.ChannelMessageSendEmbed(t.channelID, embed); err != nil {
				log.Printf("[birthday] failed to send to channel %s: %v", t.channelID, err)
				continue
			}

			// Mark as wished for this guild immediately so a restart won't re-send.
			m.mu.Lock()
			if e, ok := m.entries[u.userID]; ok {
				if e.LastWished == nil {
					e.LastWished = make(map[string]string)
				}
				e.LastWished[t.guildID] = dateStr
				m.entries[u.userID] = e
				m.saveUnlocked()
			}
			m.mu.Unlock()
		}
	}
}

// pickGIF returns a URL to a randomly chosen GIF from the guild's local library.
// Returns "" if GIFs are disabled, the library is empty, or no baseURL is set.
func (m *Module) pickGIF(guildID string) string {
	if m.baseURL == "" || m.gifsDir == "" {
		return ""
	}

	m.mu.Lock()
	disabled := m.configs[guildID].DisableGIFs
	m.mu.Unlock()
	if disabled {
		return ""
	}

	dir := filepath.Join(m.gifsDir, guildID)
	des, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var names []string
	for _, de := range des {
		if !de.IsDir() && strings.HasSuffix(strings.ToLower(de.Name()), ".gif") {
			names = append(names, de.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}

	name := names[rand.IntN(len(names))]
	return m.baseURL + "/gifs/" + guildID + "/" + name
}

// ── Public API for the web layer ──────────────────────────────────────────────

// Entry is one user's birthday, exposed to the web layer.
type Entry struct {
	UserID string
	Month  int
	Day    int
}

// AllEntries returns every registered birthday, sorted by month then day.
func (m *Module) AllEntries() []Entry {
	m.mu.Lock()
	out := make([]Entry, 0, len(m.entries))
	for userID, e := range m.entries {
		out = append(out, Entry{UserID: userID, Month: e.Month, Day: e.Day})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Month != out[j].Month {
			return out[i].Month < out[j].Month
		}
		return out[i].Day < out[j].Day
	})
	return out
}

// AnnouncementChannel returns the configured announcement channel for a guild.
func (m *Module) AnnouncementChannel(guildID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configs[guildID].ChannelID
}

// SetAnnouncementChannel stores (or clears) the announcement channel for a guild.
func (m *Module) SetAnnouncementChannel(guildID, channelID string) {
	m.mu.Lock()
	cfg := m.configs[guildID]
	cfg.ChannelID = channelID
	m.configs[guildID] = cfg
	m.mu.Unlock()
	m.save()
}

// GIFsEnabled reports whether GIFs are enabled for the guild (default: true).
func (m *Module) GIFsEnabled(guildID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.configs[guildID].DisableGIFs
}

// SetGIFsEnabled enables or disables GIF sending for a guild.
func (m *Module) SetGIFsEnabled(guildID string, enabled bool) {
	m.mu.Lock()
	cfg := m.configs[guildID]
	cfg.DisableGIFs = !enabled
	m.configs[guildID] = cfg
	m.mu.Unlock()
	m.save()
}

// GuildGIFsDir returns the filesystem path where this guild's GIFs are stored.
func (m *Module) GuildGIFsDir(guildID string) string {
	return filepath.Join(m.gifsDir, guildID)
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (m *Module) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveUnlocked()
}

func (m *Module) saveUnlocked() {
	data, err := json.MarshalIndent(persistedData{
		Entries: m.entries,
		Configs: m.configs,
	}, "", "  ")
	if err != nil {
		log.Printf("[birthday] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(m.dataFile, data, 0600); err != nil {
		log.Printf("[birthday] write %s error: %v", m.dataFile, err)
	}
}

func (m *Module) load() {
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[birthday] read %s error: %v", m.dataFile, err)
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// v3: entries keyed by plain userID (no ":" in any key).
	var pd persistedData
	if json.Unmarshal(raw, &pd) == nil && pd.Entries != nil {
		isV3 := true
		for key := range pd.Entries {
			if strings.Contains(key, ":") {
				isV3 = false
				break
			}
		}
		if isV3 {
			m.entries = pd.Entries
			if pd.Configs != nil {
				m.configs = pd.Configs
			}
			return
		}
	}

	// v2 / v1 migration: entries keyed by "guildID:userID".
	type legacyData struct {
		Entries map[string]legacyEntry `json:"entries"`
		Configs map[string]guildConfig `json:"guild_configs"`
	}

	var legacy legacyData
	if json.Unmarshal(raw, &legacy) == nil && legacy.Entries != nil {
		m.migrateFromLegacy(legacy.Entries, legacy.Configs)
		return
	}

	var flat map[string]legacyEntry
	if err := json.Unmarshal(raw, &flat); err != nil {
		log.Printf("[birthday] parse %s error: %v", m.dataFile, err)
		return
	}
	m.migrateFromLegacy(flat, nil)
}

func (m *Module) migrateFromLegacy(entries map[string]legacyEntry, configs map[string]guildConfig) {
	for key, e := range entries {
		guildID, userID, ok := strings.Cut(key, ":")
		if !ok {
			continue
		}
		if _, exists := m.entries[userID]; !exists {
			m.entries[userID] = entry{Month: e.Month, Day: e.Day}
		}
		if e.ChannelID != "" {
			cfg := m.configs[guildID]
			if cfg.ChannelID == "" {
				cfg.ChannelID = e.ChannelID
				m.configs[guildID] = cfg
			}
		}
	}
	for guildID, cfg := range configs {
		existing := m.configs[guildID]
		if existing.ChannelID == "" {
			existing.ChannelID = cfg.ChannelID
		}
		m.configs[guildID] = existing
	}
	log.Printf("[birthday] migrated %d birthday entries to v3 format", len(m.entries))
}

func ephemeralRespond(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[birthday] respond error: %v", err)
	}
}
