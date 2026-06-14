package loganalyze

import "testing"

func TestParseReportInput(t *testing.T) {
	cases := []struct {
		in    string
		code  string
		fight int
	}{
		{"https://www.fflogs.com/reports/VRjJPTzXk9NDQfpr", "VRjJPTzXk9NDQfpr", 0},
		{"https://www.fflogs.com/reports/VRjJPTzXk9NDQfpr#fight=5", "VRjJPTzXk9NDQfpr", 5},
		{"https://www.fflogs.com/reports/VRjJPTzXk9NDQfpr?fight=12&type=damage-done", "VRjJPTzXk9NDQfpr", 12},
		{"VRjJPTzXk9NDQfpr", "VRjJPTzXk9NDQfpr", 0},
		{"  VRjJPTzXk9NDQfpr  ", "VRjJPTzXk9NDQfpr", 0},
		{"https://www.fflogs.com/reports/VRjJPTzXk9NDQfpr#fight=last", "VRjJPTzXk9NDQfpr", 0},
		{"not a link", "", 0},
		{"", "", 0},
	}
	for _, c := range cases {
		code, fight := ParseReportInput(c.in)
		if code != c.code || fight != c.fight {
			t.Errorf("ParseReportInput(%q) = (%q, %d), want (%q, %d)", c.in, code, fight, c.code, c.fight)
		}
	}
}

func TestPrettyJob(t *testing.T) {
	cases := map[string]string{
		"DarkKnight":  "Dark Knight",
		"Warrior":     "Warrior",
		"WhiteMage":   "White Mage",
		"BlackMage":   "Black Mage",
		"Pictomancer": "Pictomancer",
		"":            "",
	}
	for in, want := range cases {
		if got := prettyJob(in); got != want {
			t.Errorf("prettyJob(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizePct(t *testing.T) {
	cases := map[float64]float64{
		27:   27,
		2730: 27.3,
		0:    0,
		-5:   0,
		100:  100,
	}
	for in, want := range cases {
		if got := normalizePct(in); got != want {
			t.Errorf("normalizePct(%v) = %v, want %v", in, got, want)
		}
	}
}
