// Package birthday wishes users a happy birthday with a random GIF fetched
// from Tenor using the built-in public key — no configuration required.
// Admins can customise the GIF search query per guild via the web dashboard.
package birthday

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	// tenorAPIKey is Tenor's public example key — no sign-up required.
	tenorAPIKey = "LIVDSRZULELA"
	// defaultGIFQuery is used when a guild hasn't set a custom query.
	defaultGIFQuery = "anime happy birthday"
)

// entry holds one user's birthday and the channel where it should be announced.
type entry struct {
	Month      int    `json:"month"`
	Day        int    `json:"day"`
	ChannelID  string `json:"channel_id"`
	LastWished string `json:"last_wished"` // "2006-01-02" — prevents re-sending on restart
}

// guildConfig stores per-guild settings for the birthday module.
type guildConfig struct {
	GIFQuery string `json:"gif_query"`
}

// persistedData is the on-disk format (v2).
// v1 was a flat map[string]entry; load() migrates it automatically.
type persistedData struct {
	Entries map[string]entry       `json:"entries"`
	Configs map[string]guildConfig `json:"guild_configs"`
}

// Module implements bot.Module for birthday tracking.
type Module struct {
	dataFile   string
	httpClient *http.Client
	mu         sync.Mutex
	entries    map[string]entry       // key: "guildID:userID"
	configs    map[string]guildConfig // key: guildID
	stop       chan struct{}
}

func New(dataFile string) *Module {
	return &Module{
		dataFile:   dataFile,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		entries:    make(map[string]entry),
		configs:    make(map[string]guildConfig),
		stop:       make(chan struct{}),
	}
}

func (m *Module) Name() string        { return "birthday" }
func (m *Module) Description() string { return "Remembers birthdays and wishes users on their special day with an anime GIF." }

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

	key := i.GuildID + ":" + i.Member.User.ID
	m.mu.Lock()
	m.entries[key] = entry{Month: month, Day: day, ChannelID: i.ChannelID}
	m.mu.Unlock()
	m.save()

	ephemeralRespond(s, i, fmt.Sprintf("🎂 Got it! I'll wish you a happy birthday on **%s %d**.", time.Month(month), day))
}

func (m *Module) handleCheck(s *discordgo.Session, i *discordgo.InteractionCreate) {
	key := i.GuildID + ":" + i.Member.User.ID
	m.mu.Lock()
	e, ok := m.entries[key]
	m.mu.Unlock()

	if !ok {
		ephemeralRespond(s, i, "You haven't registered a birthday yet. Use `/birthday set` to add one.")
		return
	}
	ephemeralRespond(s, i, fmt.Sprintf("🎂 Your registered birthday is **%s %d**.", time.Month(e.Month), e.Day))
}

func (m *Module) handleRemove(s *discordgo.Session, i *discordgo.InteractionCreate) {
	key := i.GuildID + ":" + i.Member.User.ID
	m.mu.Lock()
	_, ok := m.entries[key]
	delete(m.entries, key)
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

	type wish struct {
		key       string
		guildID   string
		userID    string
		channelID string
	}

	m.mu.Lock()
	var wishes []wish
	for key, e := range m.entries {
		if e.Month != int(today.Month()) || e.Day != today.Day() || e.LastWished == dateStr {
			continue
		}
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		wishes = append(wishes, wish{key: key, guildID: parts[0], userID: parts[1], channelID: e.ChannelID})
	}
	m.mu.Unlock()

	for _, w := range wishes {
		m.mu.Lock()
		query := m.gifQuery(w.guildID)
		m.mu.Unlock()

		gifURL := m.fetchGIF(query) // HTTP call — intentionally outside the mutex

		embed := &discordgo.MessageEmbed{
			Description: fmt.Sprintf("🎉 Happy Birthday <@%s>! Wishing you an amazing day! 🎂🥳", w.userID),
			Color:       0xff69b4,
		}
		if gifURL != "" {
			embed.Image = &discordgo.MessageEmbedImage{URL: gifURL}
		}

		if _, err := s.ChannelMessageSendEmbed(w.channelID, embed); err != nil {
			log.Printf("[birthday] failed to send birthday message to channel %s: %v", w.channelID, err)
			continue
		}

		// Persist immediately so a restart doesn't re-send the same birthday.
		m.mu.Lock()
		if e, ok := m.entries[w.key]; ok {
			e.LastWished = dateStr
			m.entries[w.key] = e
			m.saveUnlocked()
		}
		m.mu.Unlock()
	}
}

// fetchGIF returns a random GIF URL from Tenor for the given query.
func (m *Module) fetchGIF(query string) string {
	apiURL := fmt.Sprintf(
		"https://tenor.googleapis.com/v2/search?q=%s&key=%s&limit=20&media_filter=gif&random=true",
		url.QueryEscape(query),
		tenorAPIKey,
	)
	resp, err := m.httpClient.Get(apiURL)
	if err != nil {
		log.Printf("[birthday] tenor API error: %v", err)
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		Results []struct {
			MediaFormats map[string]struct {
				URL string `json:"url"`
			} `json:"media_formats"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || len(result.Results) == 0 {
		log.Printf("[birthday] tenor parse error or empty results: %v", err)
		return ""
	}

	item := result.Results[rand.IntN(len(result.Results))]
	if gif, ok := item.MediaFormats["gif"]; ok {
		return gif.URL
	}
	return ""
}

// ── Public API for the web layer ──────────────────────────────────────────────

// GuildEntry is one user's birthday, exposed to the web layer.
type GuildEntry struct {
	UserID string
	Month  int
	Day    int
}

// GuildEntries returns all registered birthdays for a guild, sorted by month then day.
func (m *Module) GuildEntries(guildID string) []GuildEntry {
	prefix := guildID + ":"
	m.mu.Lock()
	var out []GuildEntry
	for key, e := range m.entries {
		if userID, ok := strings.CutPrefix(key, prefix); ok {
			out = append(out, GuildEntry{UserID: userID, Month: e.Month, Day: e.Day})
		}
	}
	m.mu.Unlock()
	// Sort by month then day so the list reads like a calendar.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Month != out[j].Month {
			return out[i].Month < out[j].Month
		}
		return out[i].Day < out[j].Day
	})
	return out
}

// GIFQuery returns the configured search query for a guild, or the default.
func (m *Module) GIFQuery(guildID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gifQuery(guildID)
}

// SetGIFQuery stores a custom GIF search query for a guild.
func (m *Module) SetGIFQuery(guildID, query string) {
	m.mu.Lock()
	cfg := m.configs[guildID]
	cfg.GIFQuery = strings.TrimSpace(query)
	m.configs[guildID] = cfg
	m.mu.Unlock()
	m.save()
}

// DefaultGIFQuery returns the built-in default query string, used as placeholder text.
func DefaultGIFQuery() string { return defaultGIFQuery }

// ── Helpers ───────────────────────────────────────────────────────────────────

// gifQuery returns the effective GIF query for a guild. Must hold m.mu.
func (m *Module) gifQuery(guildID string) string {
	if cfg, ok := m.configs[guildID]; ok && cfg.GIFQuery != "" {
		return cfg.GIFQuery
	}
	return defaultGIFQuery
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

	// Try new format first (has an "entries" key at the top level).
	var pd persistedData
	if json.Unmarshal(raw, &pd) == nil && pd.Entries != nil {
		m.entries = pd.Entries
		if pd.Configs != nil {
			m.configs = pd.Configs
		}
		return
	}

	// Fall back to v1 flat format: map["guildID:userID"]entry.
	var old map[string]entry
	if err := json.Unmarshal(raw, &old); err != nil {
		log.Printf("[birthday] parse %s error: %v", m.dataFile, err)
		return
	}
	m.entries = old
	log.Printf("[birthday] migrated %d birthday entries from v1 format", len(old))
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
