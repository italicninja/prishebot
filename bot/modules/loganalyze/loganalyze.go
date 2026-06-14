package loganalyze

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/user/discord-bot-skeleton/bot"
)

// Embed colors mirror FFLogs' kill-green / wipe-red convention.
const (
	colorKill = 0x57F287
	colorWipe = 0xED4245
)

// Display caps keep the Discord embed within field/length limits.
const (
	maxDeathLines = 10
	maxCauseLines = 6
	maxDPSLines   = 12
)

// Module implements bot.Module for FFXIV log analysis.
type Module struct {
	client *Client
}

// New builds the log-analysis module. Pass the FFLogs API client credentials
// (FFLOGS_CLIENT_ID / FFLOGS_CLIENT_SECRET); when they are empty the command
// still loads but politely reports that analysis is unavailable.
func New(fflogsClientID, fflogsClientSecret string) *Module {
	return &Module{client: NewClient(fflogsClientID, fflogsClientSecret)}
}

func (m *Module) Name() string           { return "loganalyze" }
func (m *Module) Category() bot.Category { return bot.CategoryFunctional }
func (m *Module) Description() string {
	return "Analyze an FFXIV combat log from FFLogs - deaths, causes of death, and DPS. Paste a report link or code."
}

// Client exposes the FFLogs client to the web dashboard so it can render the
// same analysis as an HTML page.
func (m *Module) Client() *Client { return m.client }

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        "loganalyze",
			Description: "Analyze an FFXIV FFLogs report (deaths, causes, DPS)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "report",
					Description: "FFLogs report link or code (e.g. https://www.fflogs.com/reports/VRjJPTzXk9NDQfpr)",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "fight",
					Description: "Fight/pull number to analyze (defaults to the last kill or longest pull)",
					Required:    false,
				},
			},
		},
	}
}

func (m *Module) HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	var reportInput string
	var fightID int
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "report":
			reportInput = opt.StringValue()
		case "fight":
			fightID = int(opt.IntValue())
		}
	}

	if !m.client.Configured() {
		ephemeral(s, i, "⚠️ Log analysis isn't set up on this bot. An admin needs to set `FFLOGS_CLIENT_ID` and `FFLOGS_CLIENT_SECRET` (create a v2 client at <https://www.fflogs.com/api/clients/>).")
		return
	}

	code, parsedFight := ParseReportInput(reportInput)
	if code == "" {
		ephemeral(s, i, "❌ I couldn't find a report code in that. Paste an FFLogs link like `https://www.fflogs.com/reports/VRjJPTzXk9NDQfpr` or just the code.")
		return
	}
	if fightID == 0 {
		fightID = parsedFight // fall back to a ?fight= in the pasted URL
	}

	// FFLogs calls take a few seconds - defer so we don't blow the 3s window.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		log.Printf("[loganalyze] defer failed: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, analysis, err := m.client.Analyze(ctx, code, fightID)
	if err != nil {
		m.editError(s, i, fmt.Sprintf("❌ %v", err))
		return
	}

	embed := m.buildEmbed(info, analysis)
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
	}); err != nil {
		log.Printf("[loganalyze] response edit failed: %v", err)
	}
}

// buildEmbed renders the analysis as a Discord embed.
func (m *Module) buildEmbed(info *ReportInfo, a *FightAnalysis) *discordgo.MessageEmbed {
	f := a.Fight

	color := colorWipe
	if f.Kill {
		color = colorKill
	}

	var header []string
	if info.Zone != "" {
		header = append(header, "📍 "+info.Zone)
	}
	header = append(header, fmt.Sprintf("⚔️ **%s** · %s · ⏱ %s", f.Name, f.Outcome(), f.Duration()))
	if u := info.StartUnix(); u > 0 {
		header = append(header, fmt.Sprintf("🗓 <t:%d:f>", u))
	}
	if info.Owner != "" {
		header = append(header, "👤 Uploaded by "+info.Owner)
	}

	embed := &discordgo.MessageEmbed{
		Title:       "📊 " + nonEmpty(info.Title, "FFLogs Report"),
		URL:         m.reportURL(info.Code, f.ID),
		Color:       color,
		Description: strings.Join(header, "\n"),
		Footer:      &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Report %s · Fight #%d · Powered by FFLogs", info.Code, f.ID)},
	}

	// ── Deaths ───────────────────────────────────────────────────────────────
	if len(a.Deaths) == 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  "💀 Deaths (0)",
			Value: "No deaths — clean run! 🎉",
		})
	} else {
		var b strings.Builder
		shown := a.Deaths
		if len(shown) > maxDeathLines {
			shown = shown[:maxDeathLines]
		}
		for _, d := range shown {
			job := d.Job
			if job != "" {
				job = " *(" + job + ")*"
			}
			fmt.Fprintf(&b, "`%s` **%s**%s — %s\n", d.TimeStr(), d.Player, job, d.Cause)
		}
		if len(a.Deaths) > maxDeathLines {
			fmt.Fprintf(&b, "…and %d more", len(a.Deaths)-maxDeathLines)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("💀 Deaths (%d)", len(a.Deaths)),
			Value: strings.TrimSpace(b.String()),
		})
	}

	// ── Causes of death ──────────────────────────────────────────────────────
	if len(a.DeathsByCause) > 0 {
		var b strings.Builder
		shown := a.DeathsByCause
		if len(shown) > maxCauseLines {
			shown = shown[:maxCauseLines]
		}
		for _, c := range shown {
			fmt.Fprintf(&b, "**%d×** %s\n", c.Count, c.Cause)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "☠️ Causes of death",
			Value:  strings.TrimSpace(b.String()),
			Inline: true,
		})
	}

	// ── DPS ──────────────────────────────────────────────────────────────────
	if len(a.DPS) > 0 {
		var b strings.Builder
		shown := a.DPS
		if len(shown) > maxDPSLines {
			shown = shown[:maxDPSLines]
		}
		for idx, e := range shown {
			job := e.Job
			if job != "" {
				job = " *(" + job + ")*"
			}
			fmt.Fprintf(&b, "`%2d.` **%s** dps — %s%s\n", idx+1, e.DPSStr(), e.Player, job)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "🔥 DPS ranking",
			Value:  strings.TrimSpace(b.String()),
			Inline: true,
		})
	}

	return embed
}

// reportURL builds a deep link back to the analyzed fight on FFLogs.
func (m *Module) reportURL(code string, fightID int) string {
	if fightID > 0 {
		return fmt.Sprintf("https://www.fflogs.com/reports/%s#fight=%d", code, fightID)
	}
	return "https://www.fflogs.com/reports/" + code
}

func (m *Module) editError(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &msg,
	}); err != nil {
		log.Printf("[loganalyze] error edit failed: %v", err)
	}
}

func (m *Module) OnLoad(_ *discordgo.Session) error {
	if m.client.Configured() {
		log.Println("[loganalyze] module loaded (FFLogs API configured)")
	} else {
		log.Println("[loganalyze] module loaded (FFLogs API NOT configured - set FFLOGS_CLIENT_ID/SECRET)")
	}
	return nil
}

func (m *Module) OnUnload(_ *discordgo.Session) error {
	log.Println("[loganalyze] module unloaded")
	return nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

func ephemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		log.Printf("[loganalyze] ephemeral respond failed: %v", err)
	}
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
