package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/user/discord-bot-skeleton/bot/modules/loganalyze"
)

// renderLogAnalyze executes the loganalyze.html template (plus its partials) with
// the given data and returns the output, failing on any execution error.
func renderLogAnalyze(t *testing.T, data map[string]any) string {
	t.Helper()
	tmpl, err := template.ParseFS(webFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "loganalyze.html", data); err != nil {
		t.Fatalf("execute loganalyze.html: %v", err)
	}
	return buf.String()
}

func sampleReport() *loganalyze.ReportInfo {
	return &loganalyze.ReportInfo{
		Code: "VRjJPTzXk9NDQfpr", Title: "Test Report", Owner: "Tester", Zone: "Test Zone",
		StartTime: 1700000000000,
		Fights: []loganalyze.FightInfo{
			{ID: 1, Name: "Dancing Mad", Kill: false, BossPct: 51.3, StartTime: 0, EndTime: 118000},
			{ID: 2, Name: "Dancing Mad", Kill: false, BossPct: 18.9, StartTime: 120000, EndTime: 300000},
			{ID: 3, Name: "Dancing Mad", Kill: true, BossPct: 0, StartTime: 310000, EndTime: 600000},
		},
	}
}

func sampleBase(report *loganalyze.ReportInfo, sel int, analysis *loganalyze.Analysis) map[string]any {
	return map[string]any{
		"User":          &Session{Username: "tester", AvatarURL: "/x.png"},
		"Guild":         &Guild{ID: "g1", Name: "Guild", IconURL: ""},
		"IsAdmin":       true,
		"Configured":    true,
		"RawInput":      report.Code,
		"SelectedFight": sel,
		"Report":        report,
		"Analysis":      analysis,
	}
}

// assertClean fails if the rendered output shows html/template's "unsafe value"
// sentinel (ZgotmplZ), which is what a rejected CSS/URL interpolation becomes.
func assertClean(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, "ZgotmplZ") {
		t.Fatalf("output contains ZgotmplZ — an interpolation was rejected by the sanitizer (likely the bar width style)")
	}
}

func TestLogAnalyzeRenderWholeReport(t *testing.T) {
	report := sampleReport()
	a := &loganalyze.Analysis{
		WholeReport: true, Scope: "Whole report", DurationMS: 600000, TotalDeaths: 6,
		Deaths: []loganalyze.DeathInfo{
			{Player: "Oki", Job: "Monk", FightID: 1, FightName: "Dancing Mad", TimeMS: 90000, Cause: "The River of Light"},
		},
		DeathsByCause: []loganalyze.CauseCount{
			{Cause: "The River of Light", Count: 4, BarPct: 100},
			{Cause: "Spelldriver", Count: 2, BarPct: 50},
		},
		DeathsByPlayer: []loganalyze.PlayerDeaths{
			{Player: "Oki Dokizeme", Job: "Monk", Count: 4, BarPct: 100},
			{Player: "Metra Truzi", Job: "Warrior", Count: 2, BarPct: 50},
		},
		DPS: []loganalyze.DPSEntry{
			{Rank: 1, Player: "Somnus", Job: "Samurai", Total: 204263249, DPS: 42900, BarPct: 100},
			{Rank: 2, Player: "Harasho", Job: "Summoner", Total: 166715567, DPS: 35000, BarPct: 82},
		},
		TotalDowns: 74,
		DamageDowns: []loganalyze.DownEntry{
			{Player: "Metra Truzi", Job: "Warrior", Count: 41, BarPct: 100},
			{Player: "Bruiser Forever", Job: "Paladin", Count: 33, BarPct: 80},
		},
		DeathsByPull: []loganalyze.PullBreakdown{
			{FightID: 1, FightName: "Dancing Mad", Outcome: "Wipe · 51.3%", Kill: false,
				Deaths: []loganalyze.DeathInfo{{Player: "Oki", Job: "Monk", FightID: 1, FightName: "Dancing Mad", TimeMS: 90000, Cause: "Blizzard III Blowout"}},
				Downs:  []loganalyze.DownInPull{{Player: "Oki Dokizeme", Count: 1}}},
			{FightID: 3, FightName: "Dancing Mad", Outcome: "Wipe · 61.3%", Kill: false,
				Deaths: []loganalyze.DeathInfo{{Player: "Metra Truzi", Job: "Warrior", FightID: 3, FightName: "Dancing Mad", TimeMS: 346000, Cause: "All Things Ending"}},
				Downs:  []loganalyze.DownInPull{{Player: "Metra Truzi", Count: 10}}},
		},
	}
	out := renderLogAnalyze(t, sampleBase(report, 0, a))
	assertClean(t, out)

	for _, want := range []string{
		"Whole-report summary",  // analysis header
		`class="page la-page"`,  // wide full-width shell
		`class="la-grid"`,       // metric cards tiled in the dashboard grid
		`class="la-stat-strip"`, // stat strip element present
		`class="la-prog-svg"`,   // progression chart element present (>=2 fights)
		"width:100%",            // top bar fully filled (BarPct=100)
		"The River of Light",    // long mechanic name
		"DPS ranking",
		"Damage Down",     // damage-down section
		"Metra Truzi",     // a damage-down recipient
		`class="la-pull"`, // deaths-by-pull disclosure present
		"Deaths by pull",
		"×10",             // a per-pull damage-down chip count
		"10× Damage Down", // per-pull summary uses the true application total…
		"1 death ",        // …and correct singular pluralization (not "1 deaths")
	} {
		if !strings.Contains(out, want) {
			t.Errorf("whole-report output missing %q", want)
		}
	}
	// No leftover decorative emoji in the page chrome.
	for _, emoji := range []string{"📊", "💀", "🔥", "⚔️", "📍", "🗓", "👤", "⏱", "🎉"} {
		if strings.Contains(out, emoji) {
			t.Errorf("whole-report output still contains emoji %q", emoji)
		}
	}
}

func TestLogAnalyzeRenderSingleFight(t *testing.T) {
	report := sampleReport()
	fight := &report.Fights[0]
	a := &loganalyze.Analysis{
		WholeReport: false, Fight: fight, Scope: fight.Name, DurationMS: fight.DurationMS(), TotalDeaths: 1,
		Deaths: []loganalyze.DeathInfo{
			{Player: "Oki", Job: "Monk", FightID: 1, FightName: "Dancing Mad", TimeMS: 90000, Cause: "Gravitational Explosion"},
		},
		DeathsByCause: []loganalyze.CauseCount{{Cause: "Gravitational Explosion", Count: 1, BarPct: 100}},
		DPS:           []loganalyze.DPSEntry{{Rank: 1, Player: "Somnus", Job: "Samurai", Total: 100, DPS: 47500, BarPct: 100}},
		TotalDowns:    2,
		DamageDowns:   []loganalyze.DownEntry{{Player: "Oki Dokizeme", Job: "Monk", Count: 2, BarPct: 100}},
	}
	out := renderLogAnalyze(t, sampleBase(report, 1, a))
	assertClean(t, out)

	if !strings.Contains(out, "Gravitational Explosion") {
		t.Error("single-fight output missing the death cause")
	}
	if !strings.Contains(out, "Damage Down") {
		t.Error("single-fight view should still show the Damage Down section")
	}
	// The progression chart, stat strip, and per-pull list are whole-report only.
	if strings.Contains(out, `class="la-prog-svg"`) {
		t.Error("single-fight view should not render the progression chart")
	}
	if strings.Contains(out, `class="la-stat-strip"`) {
		t.Error("single-fight view should not render the stat strip")
	}
	if strings.Contains(out, `class="la-pull"`) {
		t.Error("single-fight view should not render the deaths-by-pull list")
	}
}

func TestLogAnalyzeRenderEdgeCases(t *testing.T) {
	// Single pull → no progression chart; zero deaths; no DPS data.
	report := &loganalyze.ReportInfo{
		Code: "ABC", Title: "Solo", Fights: []loganalyze.FightInfo{{ID: 1, Name: "Boss", Kill: true}},
	}
	a := &loganalyze.Analysis{WholeReport: true, Scope: "Whole report", TotalDeaths: 0}
	out := renderLogAnalyze(t, sampleBase(report, 0, a))
	assertClean(t, out)
	if strings.Contains(out, `class="la-prog-svg"`) {
		t.Error("single-pull report should not render the progression chart (HasProgression false)")
	}
	if !strings.Contains(out, "No deaths recorded.") {
		t.Error("zero-death output should show the empty deaths message")
	}
	if !strings.Contains(out, "No damage data available.") {
		t.Error("no-DPS output should show the empty DPS message")
	}

	// Report present but Analysis failed (nil) — overview still renders.
	data := sampleBase(report, 0, nil)
	data["Analysis"] = nil
	out2 := renderLogAnalyze(t, data)
	assertClean(t, out2)
	if !strings.Contains(out2, "Solo") {
		t.Error("overview should render even when Analysis is nil")
	}
}
