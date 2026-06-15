// Package loganalyze provides FFXIV combat-log analysis backed by the FFLogs
// (fflogs.com) v2 GraphQL API. It powers the /loganalyze slash command and the
// dashboard's log-analysis page: paste an FFLogs report link (or just the
// report code) and get a breakdown of deaths, death causes, and DPS.
//
// This file holds the FFLogs API client and the analysis logic. The Discord
// and web layers consume the structured ReportInfo / Analysis results so the
// formatting concerns stay out of here.
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

// Kills counts how many fights in the report ended in a kill.
func (r *ReportInfo) Kills() int {
	n := 0
	for _, f := range r.Fights {
		if f.Kill {
			n++
		}
	}
	return n
}

// Wipes counts how many fights ended without a kill.
func (r *ReportInfo) Wipes() int { return len(r.Fights) - r.Kills() }

// BestPull returns the most successful attempt: the first kill if any, else the
// wipe that pushed the boss to the lowest HP %. Returns nil for an empty report.
func (r *ReportInfo) BestPull() *FightInfo {
	var best *FightInfo
	for i := range r.Fights {
		f := &r.Fights[i]
		if f.Kill {
			return f
		}
		// Among wipes, lowest boss HP % wins. Treat 0 (unknown) as "no progress".
		if f.BossPct <= 0 {
			continue
		}
		if best == nil || f.BossPct < best.BossPct {
			best = f
		}
	}
	return best
}

// ── Pull-progression chart ──────────────────────────────────────────────────
//
// The chart is an inline SVG column chart, one column per pull, drawn in a fixed
// progChartW × progChartH viewBox. Every coordinate is precomputed here as an
// integer so the template (which has no arithmetic) only interpolates numbers.
// A column's filled (bottom) portion shows how far the boss was pushed
// (100 − BossPct, or 100 for a kill); the top portion is the boss HP that
// remained. Taller filled portion = more progress.
const (
	progChartW = 720
	progChartH = 180
	progPadTop = 16
	progPadBot = 16
	progPlotH  = progChartH - progPadTop - progPadBot // plot area height (148)
	// ProgAxisY is the y of the chart baseline (bottom of the plot). Exposed for
	// the template's axis line so it stays in sync with the constants here.
	ProgAxisY = progPadTop + progPlotH // 164
)

// ChartDims carries the progression chart's viewBox size and baseline y so the
// template renders the SVG without hardcoding magic numbers.
type ChartDims struct{ W, H, AxisY int }

// ProgChartDims returns the fixed geometry of the progression chart viewBox.
func (r *ReportInfo) ProgChartDims() ChartDims {
	return ChartDims{W: progChartW, H: progChartH, AxisY: ProgAxisY}
}

// PullBar is one precomputed column of the progression chart. All fields are
// integer pixel coordinates within the progChartW × progChartH viewBox.
type PullBar struct {
	X, BarW    int    // column x position and width
	HpY, HpH   int    // "boss HP remaining" block (top, drawn in surface-2)
	BarY, BarH int    // "boss HP pushed" block (bottom, accent or green)
	Kill       bool   // true → kill column (green, full height)
	Label      string // hover tooltip, e.g. "Pull 3 · Wipe · 51.3%"
}

// progressOf returns how far a pull pushed the boss, 0-100 (100 for a kill).
// An unknown boss % (<= 0 on a wipe, e.g. an aborted pull) counts as no
// measurable progress — matching BestPull/BestLineY, which skip the same case —
// so it renders as a flat empty column rather than a misleading full-height one.
func progressOf(f FightInfo) float64 {
	if f.Kill {
		return 100
	}
	if f.BossPct <= 0 {
		return 0
	}
	return max(min(100-f.BossPct, 100), 0)
}

// HasProgression reports whether the progression chart is worth drawing. A
// single pull carries no trend, so it is hidden.
func (r *ReportInfo) HasProgression() bool { return len(r.Fights) >= 2 }

// ProgressionChart precomputes one PullBar per pull for the SVG column chart.
func (r *ReportInfo) ProgressionChart() []PullBar {
	n := len(r.Fights)
	if n == 0 {
		return nil
	}
	colW := max(progChartW/n, 1)
	gap := colW / 5
	barW := max(colW-gap, 1)

	out := make([]PullBar, 0, n)
	for i, f := range r.Fights {
		barH := int(progressOf(f)/100*float64(progPlotH) + 0.5)
		out = append(out, PullBar{
			X:     i*colW + gap/2,
			BarW:  barW,
			HpY:   progPadTop,
			HpH:   progPlotH - barH,
			BarY:  progPadTop + progPlotH - barH,
			BarH:  barH,
			Kill:  f.Kill,
			Label: fmt.Sprintf("Pull %d · %s", f.ID, f.Outcome()),
		})
	}
	return out
}

// BestLineY returns the y coordinate for the dashed "best pull" baseline, or 0
// when there is no meaningful best pull (so the template omits the line).
func (r *ReportInfo) BestLineY() int {
	bp := r.BestPull()
	if bp == nil || (!bp.Kill && bp.BossPct <= 0) {
		return 0
	}
	barH := int(progressOf(*bp)/100*float64(progPlotH) + 0.5)
	return progPadTop + progPlotH - barH
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

// Outcome is "Kill" or "Wipe · NN%" for display.
func (f FightInfo) Outcome() string {
	if f.Kill {
		return "Kill"
	}
	if f.BossPct > 0 {
		return fmt.Sprintf("Wipe · %.1f%%", f.BossPct)
	}
	return "Wipe"
}

// Analysis is the deaths + DPS breakdown for either a single fight or the whole
// report (the default). For a single fight, Deaths is the per-death list; for a
// whole-report summary, the per-cause and per-player rollups are the focus.
type Analysis struct {
	WholeReport bool       // true when this summarises the whole report
	Fight       *FightInfo // set only for a single-fight analysis
	Scope       string     // human label, e.g. "Whole report" or the fight name
	DurationMS  int64      // analysed combat time (fight length, or summed active time)

	TotalDeaths    int
	Deaths         []DeathInfo    // chronological
	DeathsByCause  []CauseCount   // grouped by killing mechanic, most common first
	DeathsByPlayer []PlayerDeaths // grouped by player, most deaths first

	DPS []DPSEntry // highest DPS first
}

// DurationStr renders the analysed combat time as "M:SS".
func (a *Analysis) DurationStr() string { return formatDuration(a.DurationMS) }

// DeathInfo is one player death.
type DeathInfo struct {
	Player    string
	Job       string // pretty-printed, e.g. "Dark Knight"
	FightID   int
	FightName string
	TimeMS    int64  // ms into the fight it occurred in
	Cause     string // killing mechanic, or "Unknown"
}

// TimeStr renders the death time as "M:SS" into its fight.
func (d DeathInfo) TimeStr() string { return formatDuration(d.TimeMS) }

// CauseCount is a death-cause tally for the "causes of death" breakdown.
type CauseCount struct {
	Cause  string
	Count  int
	BarPct int // 0-100, relative to the most common cause (for bar charts)
}

// PlayerDeaths is a per-player death tally.
type PlayerDeaths struct {
	Player string
	Job    string
	Count  int
	BarPct int // 0-100, relative to the player with the most deaths
}

// DPSEntry is one player's damage contribution.
type DPSEntry struct {
	Rank   int // 1-based position in the ranking
	Player string
	Job    string // pretty-printed
	Total  int64  // total damage dealt
	DPS    float64
	BarPct int // 0-100, relative to the top DPS (for bar charts)
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

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
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

// tablesQuery fetches the Deaths and DamageDone tables together. $fightIDs is
// optional: omitted (null) it covers the whole report, otherwise just the
// listed fight(s).
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
					ID             int     `json:"id"`
					Name           string  `json:"name"`
					Kill           *bool   `json:"kill"`
					Difficulty     *int    `json:"difficulty"`
					BossPercentage float64 `json:"bossPercentage"`
					StartTime      int64   `json:"startTime"`
					EndTime        int64   `json:"endTime"`
				} `json:"fights"`
			} `json:"report"`
		} `json:"reportData"`
	}
	if err := c.query(ctx, overviewQuery, map[string]any{"code": code}, &resp); err != nil {
		return nil, err
	}
	r := resp.ReportData.Report
	if r == nil {
		return nil, fmt.Errorf("report %q not found (it may be private, or the code is wrong)", code)
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

// Analyze fetches the report overview and a deaths/DPS breakdown. When fightID
// is > 0, the breakdown is scoped to that fight; otherwise it summarises the
// whole report (every pull combined) - this is the default. It returns the full
// report (so callers can list every fight) alongside the analysis.
func (c *Client) Analyze(ctx context.Context, code string, fightID int) (*ReportInfo, *Analysis, error) {
	info, err := c.FetchReport(ctx, code)
	if err != nil {
		return nil, nil, err
	}
	if len(info.Fights) == 0 {
		return info, nil, fmt.Errorf("this report has no fights to analyze")
	}

	// Per-fight start time and name, used to convert report-relative death
	// timestamps into "time into the fight" and to label deaths.
	startByID := make(map[int]int64, len(info.Fights))
	nameByID := make(map[int]string, len(info.Fights))
	for _, f := range info.Fights {
		startByID[f.ID] = f.StartTime
		nameByID[f.ID] = f.Name
	}

	a := &Analysis{}
	vars := map[string]any{"code": code}
	if fightID > 0 {
		f := findFight(info.Fights, fightID)
		if f == nil {
			return info, nil, fmt.Errorf("fight %d not found in this report", fightID)
		}
		a.Fight = f
		a.Scope = f.Name
		a.DurationMS = f.DurationMS()
		vars["fightIDs"] = []int{f.ID}
	} else {
		a.WholeReport = true
		a.Scope = "Whole report"
		// The table query rejects an absent fight list, so pass every fight id
		// explicitly to cover the whole report.
		ids := make([]int, 0, len(info.Fights))
		for _, f := range info.Fights {
			ids = append(ids, f.ID)
		}
		vars["fightIDs"] = ids
	}

	var resp struct {
		ReportData struct {
			Report struct {
				Deaths json.RawMessage `json:"deaths"`
				Damage json.RawMessage `json:"damage"`
			} `json:"report"`
		} `json:"reportData"`
	}
	if err := c.query(ctx, tablesQuery, vars, &resp); err != nil {
		return info, nil, err
	}

	a.Deaths = parseDeaths(resp.ReportData.Report.Deaths, startByID, nameByID)
	a.TotalDeaths = len(a.Deaths)
	a.DeathsByCause = groupByCause(a.Deaths)
	a.DeathsByPlayer = groupByPlayer(a.Deaths)

	dps, totalTime := parseDamage(resp.ReportData.Report.Damage, a.DurationMS)
	a.DPS = dps
	if a.WholeReport && totalTime > 0 {
		a.DurationMS = totalTime // summed active combat time across pulls
	}
	return info, a, nil
}

func findFight(fights []FightInfo, id int) *FightInfo {
	for i := range fights {
		if fights[i].ID == id {
			return &fights[i]
		}
	}
	return nil
}

// ── Table parsing ───────────────────────────────────────────────────────────

// tableEnvelope is the { "data": { "entries": [...] } } shape the FFLogs `table`
// scalar returns for both Deaths and DamageDone.
type tableEnvelope struct {
	Data struct {
		Entries   []tableEntry `json:"entries"`
		TotalTime int64        `json:"totalTime"`
	} `json:"data"`
}

// tableEntry covers fields from both the Deaths and DamageDone tables. Numeric
// fields are float64 because FFLogs may return large values in scientific
// notation, which fails to unmarshal into an integer type.
type tableEntry struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`      // job/class, e.g. "DarkKnight"
	Total     float64 `json:"total"`     // DamageDone: total damage dealt
	Timestamp int64   `json:"timestamp"` // Deaths: report-relative time of death (ms)
	Fight     int     `json:"fight"`     // Deaths: the fight the death occurred in
	// Deaths: breakdown of damage taken in the death window. The ability that
	// dealt the most is treated as the killing mechanic.
	Damage *struct {
		Abilities []struct {
			Name  string  `json:"name"`
			Total float64 `json:"total"`
		} `json:"abilities"`
	} `json:"damage"`
}

// killingMechanic returns the name of the ability that dealt the most damage in
// the player's death window, or "Unknown" when none is available.
func (e tableEntry) killingMechanic() string {
	if e.Damage == nil || len(e.Damage.Abilities) == 0 {
		return "Unknown"
	}
	best := e.Damage.Abilities[0]
	for _, ab := range e.Damage.Abilities[1:] {
		if ab.Total > best.Total {
			best = ab
		}
	}
	if strings.TrimSpace(best.Name) == "" {
		return "Unknown"
	}
	return best.Name
}

// parseDeaths converts the Deaths table into a chronological death list. Death
// times are made relative to the fight they occurred in via startByID.
func parseDeaths(raw json.RawMessage, startByID map[int]int64, nameByID map[int]string) []DeathInfo {
	if len(raw) == 0 {
		return nil
	}
	var env tableEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}

	deaths := make([]DeathInfo, 0, len(env.Data.Entries))
	for _, e := range env.Data.Entries {
		rel := max(e.Timestamp-startByID[e.Fight], 0)
		deaths = append(deaths, DeathInfo{
			Player:    e.Name,
			Job:       prettyJob(e.Type),
			FightID:   e.Fight,
			FightName: nameByID[e.Fight],
			TimeMS:    rel,
			Cause:     e.killingMechanic(),
		})
	}
	sort.SliceStable(deaths, func(i, j int) bool {
		if deaths[i].FightID != deaths[j].FightID {
			return deaths[i].FightID < deaths[j].FightID
		}
		return deaths[i].TimeMS < deaths[j].TimeMS
	})
	return deaths
}

// groupByCause tallies deaths by killing mechanic, most common first.
func groupByCause(deaths []DeathInfo) []CauseCount {
	counts := map[string]int{}
	for _, d := range deaths {
		counts[d.Cause]++
	}
	out := make([]CauseCount, 0, len(counts))
	for cause, n := range counts {
		out = append(out, CauseCount{Cause: cause, Count: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Cause < out[j].Cause
	})
	if len(out) > 0 && out[0].Count > 0 {
		maxC := out[0].Count // sorted desc, so [0] is the largest
		for i := range out {
			out[i].BarPct = out[i].Count * 100 / maxC
		}
	}
	return out
}

// groupByPlayer tallies deaths by player, most deaths first.
func groupByPlayer(deaths []DeathInfo) []PlayerDeaths {
	idx := map[string]int{}
	var out []PlayerDeaths
	for _, d := range deaths {
		if i, ok := idx[d.Player]; ok {
			out[i].Count++
			continue
		}
		idx[d.Player] = len(out)
		out = append(out, PlayerDeaths{Player: d.Player, Job: d.Job, Count: 1})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Player < out[j].Player
	})
	if len(out) > 0 && out[0].Count > 0 {
		maxP := out[0].Count // sorted desc, so [0] is the largest
		for i := range out {
			out[i].BarPct = out[i].Count * 100 / maxP
		}
	}
	return out
}

// parseDamage converts the DamageDone table into a DPS ranking. The per-second
// figure uses the encounter duration (table totalTime, falling back to the
// supplied duration) as the denominator so every player is compared over the
// same window.
func parseDamage(raw json.RawMessage, fallbackMS int64) ([]DPSEntry, int64) {
	if len(raw) == 0 {
		return nil, 0
	}
	var env tableEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0
	}

	durationMS := env.Data.TotalTime
	if durationMS <= 0 {
		durationMS = fallbackMS
	}
	seconds := float64(durationMS) / 1000.0

	entries := make([]DPSEntry, 0, len(env.Data.Entries))
	for _, e := range env.Data.Entries {
		if e.Total <= 0 {
			continue
		}
		dps := 0.0
		if seconds > 0 {
			dps = e.Total / seconds
		}
		entries = append(entries, DPSEntry{
			Player: e.Name,
			Job:    prettyJob(e.Type),
			Total:  int64(e.Total),
			DPS:    dps,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].DPS > entries[j].DPS })
	maxDPS := 0.0
	if len(entries) > 0 {
		maxDPS = entries[0].DPS // sorted desc, so [0] is the top DPS
	}
	for i := range entries {
		entries[i].Rank = i + 1
		if maxDPS > 0 {
			entries[i].BarPct = int(entries[i].DPS/maxDPS*100 + 0.5)
		}
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
