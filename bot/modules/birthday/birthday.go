// Package birthday wishes users a happy birthday and optionally attaches a
// random anime GIF fetched from the Tenor API (requires TENOR_API_KEY).
package birthday

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// entry holds one user's birthday and the channel where it should be announced.
type entry struct {
	Month      int    `json:"month"`
	Day        int    `json:"day"`
	ChannelID  string `json:"channel_id"`
	LastWished string `json:"last_wished"` // "2006-01-02" — prevents re-sending on restart
}

// Module implements bot.Module for birthday tracking.
type Module struct {
	tenorKey   string
	dataFile   string
	httpClient *http.Client
	mu         sync.Mutex
	data       map[string]entry // key: "guildID:userID"
	stop       chan struct{}
}

func New(tenorKey, dataFile string) *Module {
	return &Module{
		tenorKey:   tenorKey,
		dataFile:   dataFile,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		data:       make(map[string]entry),
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
	m.data[key] = entry{Month: month, Day: day, ChannelID: i.ChannelID}
	m.mu.Unlock()
	m.save()

	ephemeralRespond(s, i, fmt.Sprintf("🎂 Got it! I'll wish you a happy birthday on **%s %d**.", time.Month(month), day))
}

func (m *Module) handleCheck(s *discordgo.Session, i *discordgo.InteractionCreate) {
	key := i.GuildID + ":" + i.Member.User.ID
	m.mu.Lock()
	e, ok := m.data[key]
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
	_, ok := m.data[key]
	delete(m.data, key)
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
		userID    string
		channelID string
	}

	m.mu.Lock()
	var wishes []wish
	for key, e := range m.data {
		if e.Month != int(today.Month()) || e.Day != today.Day() || e.LastWished == dateStr {
			continue
		}
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		wishes = append(wishes, wish{key: key, userID: parts[1], channelID: e.ChannelID})
	}
	m.mu.Unlock()

	for _, w := range wishes {
		gifURL := m.fetchGIF() // HTTP call — intentionally outside the mutex

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
		if e, ok := m.data[w.key]; ok {
			e.LastWished = dateStr
			m.data[w.key] = e
			m.saveUnlocked()
		}
		m.mu.Unlock()
	}
}

// fetchGIF returns a random anime birthday GIF URL from Tenor, or "" if no
// API key is configured or the request fails.
func (m *Module) fetchGIF() string {
	if m.tenorKey == "" {
		return ""
	}
	apiURL := fmt.Sprintf(
		"https://tenor.googleapis.com/v2/search?q=%s&key=%s&limit=20&media_filter=gif&random=true",
		url.QueryEscape("anime happy birthday"),
		m.tenorKey,
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

func (m *Module) save() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveUnlocked()
}

func (m *Module) saveUnlocked() {
	data, err := json.MarshalIndent(m.data, "", "  ")
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
	if err := json.Unmarshal(raw, &m.data); err != nil {
		log.Printf("[birthday] parse %s error: %v", m.dataFile, err)
	}
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
