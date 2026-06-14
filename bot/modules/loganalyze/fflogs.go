// Package loganalyze provides FFXIV combat-log analysis backed by the FFLogs
// (fflogs.com) v2 GraphQL API. It powers the /loganalyze slash command and the
// dashboard's log-analysis page: paste an FFLogs report link (or just the
// report code) and get a breakdown of deaths, death causes, and DPS.
//
// This file holds the FFLogs API client and the analysis logic. The Discord
// and web layers consume the structured ReportInfo / FightAnalysis results so
// the formatting concerns stay out of here.
package loganalyze

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2/clientcredentials"
)

const (
	fflogsTokenURL = "https://www.fflogs.com/oauth/token"
	fflogsAPIURL   = "https://www.fflogs.com/api/v2/client"
)

// Client talks to the FFLogs v2 GraphQL API using OAuth2 client-credentials.
// The underlying *http.Client (from golang.org/x/oauth2/clientcredentials)
// fetches and transparently refreshes the bearer token, so we never manage it
// by hand. A zero-value Client (no credentials) is considered unconfigured and
// every call returns errNotConfigured.
type Client struct {
	httpClient *http.Client
	configured bool
}

// errNotConfigured is returned by every call when FFLogs credentials are absent.
var errNotConfigured = fmt.Errorf("FFLogs API is not configured (set FFLOGS_CLIENT_ID and FFLOGS_CLIENT_SECRET)")

// NewClient builds an FFLogs client. When either credential is empty the client
// reports Configured() == false and refuses calls with a friendly error, so the
// rest of the bot keeps working without FFLogs set up.
func NewClient(clientID, clientSecret string) *Client {
	clientID = strings.TrimSpace(clientID)
	clientSecret = strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return &Client{configured: false}
	}
	cc := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     fflogsTokenURL,
	}
	hc := cc.Client(context.Background())
	hc.Timeout = 25 * time.Second
	return &Client{httpClient: hc, configured: true}
}

// Configured reports whether usable FFLogs credentials were supplied.
func (c *Client) Configured() bool { return c.configured }

// ── Public result types ─────────────────────────────────────────────────────

// ReportInfo is the high-level overview of an FFLogs report plus its fight list.
type ReportInfo struct {
	Code      string
	Title     string
	Owner     string
	Zone      string
	StartTime int64 // ms epoch (UTC) of report start
	Fights    []FightInfo
}

// StartUnix returns the report start as a Unix second timestamp (for Discord
// <t:...> tags), or 0 when unknown.
func (r *ReportInfo) StartUnix() int64 {
	if r.StartTime == 0 {
		return 0
	}
	return r.StartTime / 1000
}

// StartDateStr is a pre-formatted UTC date for the dashboard, or "" when the
// report start is unknown.
func (r *ReportInfo) StartDateStr() string {
	if r.StartTime == 0 {
		return ""
	}
	return time.Unix(r.StartTime/1000, 0).UTC().Format("Jan 2, 2006 · 15:04 UTC")
}

// FightInfo summarises a single pull/fight within a report.
type FightInfo struct {
	ID         int
	Name       string
	Kill       bool
	Difficulty int
	BossPct    float64 // boss HP % at end (wipes only); 0 on a kill
	StartTime  int64   // ms relative to report start
	EndTime    int64   // ms relative to report start
}

// DurationMS returns the fight length in milliseconds.
func (f FightInfo) DurationMS() int64 { return f.EndTime - f.StartTime }

// Duration returns a friendly "M:SS" fight length.
func (f FightInfo) Duration() string { return formatDuration(f.DurationMS()) }

// Outcome is "Kill" or "Wipe (NN%)" for display.
func (f FightInfo) Outcome() string {
	if f.Kill {
		return "Kill"
	}
	if f.BossPct > 0 {
		return fmt.Sprintf("Wipe · %.1f%%", f.BossPct)
	}
	return "Wipe"
}

// FightAnalysis is the deaths + DPS breakdown for one chosen fight.
type FightAnalysis struct {
	Fight         FightInfo
	TotalTimeMS   int64
	Deaths        []DeathInfo  // chronological
	DeathsByCause []CauseCount // grouped by killing ability, most common first
	DPS           []DPSEntry   // highest DPS first
}

// DeathInfo is one player death within a fight.
type DeathInfo struct {
	Player string
	Job    string // pretty-printed, e.g. "Dark Knight"
	TimeMS int64  // ms into the fight
	Cause  string // killing ability, or "Unknown"
}

// TimeStr renders the death time as "M:SS" into the fight.
func (d DeathInfo) TimeStr() string { return formatDuration(d.TimeMS) }

// CauseCount is a death-cause tally for the "causes of death" breakdown.
type CauseCount struct {
	Cause string
	Count int
}

// DPSEntry is one player's damage contribution within a fight.
type DPSEntry struct {
	Rank   int // 1-based position in the ranking
	Player string
	Job    string // pretty-printed
	Total  int64  // total damage dealt
	DPS    float64
}

// DPSStr renders DPS as a compact "12.3k" style string.
func (e DPSEntry) DPSStr() string { return formatThousands(e.DPS) }

// ── Report-code parsing ─────────────────────────────────────────────────────

// reportPathRe pulls the 16-ish char report code out of a /reports/<code> URL.
var reportPathRe = regexp.MustCompile(`reports/([A-Za-z0-9]{8,32})`)

// bareCodeRe matches a standalone report code (no surrounding URL).
var bareCodeRe = regexp.MustCompile(`^[A-Za-z0-9]{8,32}$`)

// fightParamRe extracts a numeric fight id from a ?fight= / #fight= fragment.
var fightParamRe = regexp.MustCompile(`fight=(\d+)`)

// ParseReportInput accepts a full FFLogs URL, a URL with a fight selector, or a
// bare report code, and returns the report code plus an optional fight id
// (0 when none was specified, or when "last" was requested). It is tolerant of
// trailing query strings and fragments.
func ParseReportInput(input string) (code string, fightID int) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", 0
	}

	if m := fightParamRe.FindStringSubmatch(input); m != nil {
		fmt.Sscanf(m[1], "%d", &fightID)
	}

	if m := reportPathRe.FindStringSubmatch(input); m != nil {
		return m[1], fightID
	}

	// No /reports/ segment: strip any query/fragment and test for a bare code.
	candidate := input
	if i := strings.IndexAny(candidate, "?#"); i >= 0 {
		candidate = candidate[:i]
	}
	candidate = strings.Trim(candidate, "/")
	if i := strings.LastIndex(candidate, "/"); i >= 0 {
		candidate = candidate[i+1:]
	}
	if bareCodeRe.MatchString(candidate) {
		return candidate, fightID
	}
	return "", fightID
}

// ── GraphQL plumbing ────────────────────────────────────────────────────────

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLError struct {
	Message string `json:"message"`
}

// query runs a GraphQL request and unmarshals the "data" payload into out.
func (c *Client) query(ctx context.Context, q string, vars map[string]any, out any) error {
	if !c.configured {
		return errNotConfigured
	}

	body, err := json.Marshal(graphQLRequest{Query: q, Variables: vars})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fflogsAPIURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact FFLogs: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("FFLogs rejected the credentials (check FFLOGS_CLIENT_ID / FFLOGS_CLIENT_SECRET)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FFLogs returned HTTP %d", resp.StatusCode)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("FFLogs: %s", envelope.Errors[0].Message)
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// ── Queries ─────────────────────────────────────────────────────────────────

const overviewQuery = `
query ($code: String!) {
  reportData {
    report(code: $code) {
      title
      startTime
      owner { name }
      zone { name }
      fights {
        id
        name
        kill
        difficulty
        bossPercentage
        fightPercentage
        startTime
        endTime
      }
    }
  }
}`

const tablesQuery = `
query ($code: String!, $fightIDs: [Int]) {
  reportData {
    report(code: $code) {
      deaths: table(dataType: Deaths, fightIDs: $fightIDs)
      damage: table(dataType: DamageDone, fightIDs: $fightIDs)
    }
  }
}`

// FetchReport returns the report overview (title, zone, owner, fight list).
func (c *Client) FetchReport(ctx context.Context, code string) (*ReportInfo, error) {
	var resp struct {
		ReportData struct {
			Report *struct {
				Title     string `json:"title"`
				StartTime int64  `json:"startTime"`
				Owner     *struct {
					Name string `json:"name"`
				} `json:"owner"`
				Zone *struct {
					Name string `json:"name"`
				} `json:"zone"`
				Fights []struct {
					ID              int     `json:"id"`
					Name            string  `json:"name"`
					Kill            *bool   `json:"kill"`
					Difficulty      *int    `json:"difficulty"`
					BossPercentage  float64 `json:"bossPercentage"`
					FightPercentage float64 `json:"fightPercentage"`
					StartTime       int64   `json:"startTime"`
					EndTime         int64   `json:"endTime"`
				} `json:"fights"`
			} `json:"report"`
		} `json:"reportData"`
	}
	if err := c.query(ctx, overviewQuery, map[string]any{"code": code}, &resp); err != nil {
		return nil, err
	}
	r := resp.ReportData.Report
	if r == nil {
		return nil, fmt.Errorf("report %q not found (it may be private or the code is wrong)", code)
	}

	info := &ReportInfo{
		Code:      code,
		Title:     r.Title,
		StartTime: r.StartTime,
	}
	if r.Owner != nil {
		info.Owner = r.Owner.Name
	}
	if r.Zone != nil {
		info.Zone = r.Zone.Name
	}
	for _, f := range r.Fights {
		fi := FightInfo{
			ID:        f.ID,
			Name:      f.Name,
			StartTime: f.StartTime,
			EndTime:   f.EndTime,
			BossPct:   normalizePct(f.BossPercentage),
		}
		if f.Kill != nil {
			fi.Kill = *f.Kill
		}
		if f.Difficulty != nil {
			fi.Difficulty = *f.Difficulty
		}
		info.Fights = append(info.Fights, fi)
	}
	return info, nil
}

// Analyze fetches the report overview, picks a fight to analyze, then fetches
// and parses that fight's death and DPS tables. When fightID <= 0 it defaults
// to the most recent boss kill, falling back to the longest pull. It returns
// the full report (so callers can list every fight) alongside the chosen
// fight's analysis.
func (c *Client) Analyze(ctx context.Context, code string, fightID int) (*ReportInfo, *FightAnalysis, error) {
	info, err := c.FetchReport(ctx, code)
	if err != nil {
		return nil, nil, err
	}
	if len(info.Fights) == 0 {
		return info, nil, fmt.Errorf("this report has no fights to analyze")
	}

	fight := chooseFight(info.Fights, fightID)
	if fight == nil {
		return info, nil, fmt.Errorf("fight %d not found in this report", fightID)
	}

	var resp struct {
		ReportData struct {
			Report struct {
				Deaths json.RawMessage `json:"deaths"`
				Damage json.RawMessage `json:"damage"`
			} `json:"report"`
		} `json:"reportData"`
	}
	vars := map[string]any{"code": code, "fightIDs": []int{fight.ID}}
	if err := c.query(ctx, tablesQuery, vars, &resp); err != nil {
		return info, nil, err
	}

	analysis := &FightAnalysis{Fight: *fight}
	analysis.Deaths, analysis.DeathsByCause = parseDeaths(resp.ReportData.Report.Deaths, fight.StartTime)
	analysis.DPS, analysis.TotalTimeMS = parseDamage(resp.ReportData.Report.Damage, fight.DurationMS())
	return info, analysis, nil
}

// chooseFight selects which fight to analyze. An explicit id wins; otherwise we
// prefer the most recent kill, then fall back to the longest pull (the most
// representative attempt when there's no clear).
func chooseFight(fights []FightInfo, fightID int) *FightInfo {
	if fightID > 0 {
		for i := range fights {
			if fights[i].ID == fightID {
				return &fights[i]
			}
		}
		return nil
	}
	var best *FightInfo
	// Most recent kill (highest id among kills).
	for i := range fights {
		if fights[i].Kill {
			if best == nil || fights[i].ID > best.ID {
				best = &fights[i]
			}
		}
	}
	if best != nil {
		return best
	}
	// No kill: longest pull.
	for i := range fights {
		if best == nil || fights[i].DurationMS() > best.DurationMS() {
			best = &fights[i]
		}
	}
	return best
}

// ── Table parsing ───────────────────────────────────────────────────────────

// tableEnvelope is the common { "data": { "entries": [...] } } shape the
// FFLogs `table` scalar returns for both Deaths and DamageDone.
type tableEnvelope struct {
	Data struct {
		Entries   []tableEntry `json:"entries"`
		TotalTime int64        `json:"totalTime"`
	} `json:"data"`
}

type tableEntry struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // job/class, e.g. "DarkKnight"
	Total      int64  `json:"total"`
	ActiveTime int64  `json:"activeTime"`
	DeathTime  int64  `json:"deathTime"` // ms, report-relative (Deaths only)
	Ability    *struct {
		Name string `json:"name"`
	} `json:"ability"` // killing blow (Deaths only)
}

// parseDeaths converts the Deaths table into a chronological death list and a
// grouped cause tally. fightStart is the fight's report-relative start in ms,
// used to convert absolute death times into "time into fight".
func parseDeaths(raw json.RawMessage, fightStart int64) ([]DeathInfo, []CauseCount) {
	if len(raw) == 0 {
		return nil, nil
	}
	var env tableEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil
	}

	deaths := make([]DeathInfo, 0, len(env.Data.Entries))
	causes := map[string]int{}
	for _, e := range env.Data.Entries {
		cause := "Unknown"
		if e.Ability != nil && strings.TrimSpace(e.Ability.Name) != "" {
			cause = e.Ability.Name
		}
		rel := max(e.DeathTime-fightStart, 0)
		deaths = append(deaths, DeathInfo{
			Player: e.Name,
			Job:    prettyJob(e.Type),
			TimeMS: rel,
			Cause:  cause,
		})
		causes[cause]++
	}

	sort.SliceStable(deaths, func(i, j int) bool { return deaths[i].TimeMS < deaths[j].TimeMS })

	grouped := make([]CauseCount, 0, len(causes))
	for cause, n := range causes {
		grouped = append(grouped, CauseCount{Cause: cause, Count: n})
	}
	sort.SliceStable(grouped, func(i, j int) bool {
		if grouped[i].Count != grouped[j].Count {
			return grouped[i].Count > grouped[j].Count
		}
		return grouped[i].Cause < grouped[j].Cause
	})
	return deaths, grouped
}

// parseDamage converts the DamageDone table into a DPS ranking. The per-second
// figure uses the encounter duration (table totalTime, falling back to the
// fight length) as the denominator so every player is compared over the same
// window.
func parseDamage(raw json.RawMessage, fightDurationMS int64) ([]DPSEntry, int64) {
	if len(raw) == 0 {
		return nil, 0
	}
	var env tableEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0
	}

	durationMS := env.Data.TotalTime
	if durationMS <= 0 {
		durationMS = fightDurationMS
	}
	seconds := float64(durationMS) / 1000.0

	entries := make([]DPSEntry, 0, len(env.Data.Entries))
	for _, e := range env.Data.Entries {
		if e.Total <= 0 {
			continue
		}
		dps := 0.0
		if seconds > 0 {
			dps = float64(e.Total) / seconds
		}
		entries = append(entries, DPSEntry{
			Player: e.Name,
			Job:    prettyJob(e.Type),
			Total:  e.Total,
			DPS:    dps,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].DPS > entries[j].DPS })
	for i := range entries {
		entries[i].Rank = i + 1
	}
	return entries, durationMS
}

// ── Formatting helpers ──────────────────────────────────────────────────────

// normalizePct keeps a percentage in the 0-100 display range. FFLogs has
// historically returned these as either a plain percent or a hundredths value,
// so anything above 100 is scaled down defensively.
func normalizePct(v float64) float64 {
	if v > 100 {
		return v / 100
	}
	return max(v, 0)
}

// prettyJob turns FFLogs' camelCase class names ("DarkKnight") into spaced
// display names ("Dark Knight"). Unknown/empty values pass through unchanged.
func prettyJob(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := runes[i-1]
			if prev >= 'a' && prev <= 'z' {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// formatDuration renders a millisecond span as "M:SS".
func formatDuration(ms int64) string {
	total := max(ms, 0) / 1000
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// formatThousands renders a DPS value compactly: "1.23m", "12.3k", or "842".
func formatThousands(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fm", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}
