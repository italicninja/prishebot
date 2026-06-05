// download_icons fetches all FF14 job and role icons from xivapi.com and saves
// them locally so the bot's web server can serve them without hitting an external
// CDN on every page load.
//
// Usage:
//
//	go run ./scripts/download_icons
//
// Environment variables:
//
//	ICONS_DIR   Destination directory (default: web/static/icons/ffxiv)
//	BASE_URL    Source base URL      (default: https://xivapi.com)
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// icon describes one image to download.
type icon struct {
	// name is the output filename (no directory, no extension suffix needed - it's kept from the URL).
	name string
	// path is the URL path appended to the base URL.
	path string
	// note is shown when the download fails, e.g. "EW job - may be absent from CDN".
	note string
}

// -----------------------------------------------------------------
// Icon manifest
// Role icons: sourced from xivapi's /i/062000/ range.
// Mapping confirmed as of Endwalker patch cycle (062581–062585);
// verify visually if they shift in a future patch:
//   062581 → Tank      (blue shield)
//   062582 → Healer    (green cross)
//   062583 → DPS       (orange generic)
//   062584 → Melee DPS (orange sword)
//   062585 → Ranged DPS (orange bow)
// -----------------------------------------------------------------
var icons = []icon{
	// ── Role icons ──────────────────────────────────────────────────────────────
	{name: "role-tank.png", path: "/i/062000/062581.png"},
	{name: "role-healer.png", path: "/i/062000/062582.png"},
	{name: "role-dps.png", path: "/i/062000/062583.png"},
	{name: "role-melee.png", path: "/i/062000/062584.png"},
	{name: "role-ranged.png", path: "/i/062000/062585.png"},

	// ── Tank jobs ────────────────────────────────────────────────────────────────
	{name: "job-paladin.png", path: "/cj/1/paladin.png"},
	{name: "job-warrior.png", path: "/cj/1/warrior.png"},
	{name: "job-darkknight.png", path: "/cj/1/darkknight.png"},
	{name: "job-gunbreaker.png", path: "/cj/1/gunbreaker.png"},

	// ── Healer jobs ──────────────────────────────────────────────────────────────
	{name: "job-whitemage.png", path: "/cj/1/whitemage.png"},
	{name: "job-scholar.png", path: "/cj/1/scholar.png"},
	{name: "job-astrologian.png", path: "/cj/1/astrologian.png"},
	{name: "job-sage.png", path: "https://beta.xivapi.com/api/1/asset?path=ui/icon/062000/062140_hr1.tex&format=png"},

	// ── Melee DPS jobs ───────────────────────────────────────────────────────────
	{name: "job-monk.png", path: "/cj/1/monk.png"},
	{name: "job-dragoon.png", path: "/cj/1/dragoon.png"},
	{name: "job-ninja.png", path: "/cj/1/ninja.png"},
	{name: "job-samurai.png", path: "/cj/1/samurai.png"},
	{name: "job-reaper.png", path: "https://beta.xivapi.com/api/1/asset?path=ui/icon/062000/062139_hr1.tex&format=png"},
	{name: "job-viper.png", path: "https://beta.xivapi.com/api/1/asset?path=ui/icon/062000/062141_hr1.tex&format=png"},

	// ── Ranged DPS jobs ──────────────────────────────────────────────────────────
	{name: "job-bard.png", path: "/cj/1/bard.png"},
	{name: "job-machinist.png", path: "/cj/1/machinist.png"},
	{name: "job-dancer.png", path: "/cj/1/dancer.png"},

	// ── Caster DPS jobs ──────────────────────────────────────────────────────────
	{name: "job-blackmage.png", path: "/cj/1/blackmage.png"},
	{name: "job-summoner.png", path: "/cj/1/summoner.png"},
	{name: "job-redmage.png", path: "/cj/1/redmage.png"},
	{name: "job-pictomancer.png", path: "https://beta.xivapi.com/api/1/asset?path=ui/icon/062000/062142_hr1.tex&format=png"},
}

func main() {
	destDir := envOrDefault("ICONS_DIR", filepath.Join("web", "static", "icons", "ffxiv"))
	baseURL := envOrDefault("ICONS_SOURCE_URL", "https://xivapi.com")

	if err := os.MkdirAll(destDir, 0755); err != nil {
		log.Fatalf("creating icons dir %s: %v", destDir, err)
	}

	log.Printf("Downloading %d icons to %s (source: %s)", len(icons), destDir, baseURL)

	client := &http.Client{Timeout: 15 * time.Second}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok, bad int
	)

	// Rate-limit to 5 concurrent downloads so we don't hammer the CDN.
	sem := make(chan struct{}, 5)

	for _, ic := range icons {
		wg.Add(1)
		ic := ic
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			dest := filepath.Join(destDir, ic.name)

			// Skip if already downloaded and non-empty.
			if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
				log.Printf("  skip  %-35s (already exists, %d bytes)", ic.name, fi.Size())
				mu.Lock()
				ok++
				mu.Unlock()
				return
			}

			url := ic.path
			if len(url) == 0 || url[0] == '/' {
				url = baseURL + ic.path
			}
			resp, err := client.Get(url)
			if err != nil {
				warn(&mu, &bad, ic, fmt.Sprintf("request failed: %v", err))
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				warn(&mu, &bad, ic, fmt.Sprintf("HTTP %d", resp.StatusCode))
				return
			}

			f, err := os.Create(dest)
			if err != nil {
				warn(&mu, &bad, ic, fmt.Sprintf("create file: %v", err))
				return
			}
			n, err := io.Copy(f, resp.Body)
			f.Close()
			if err != nil {
				warn(&mu, &bad, ic, fmt.Sprintf("write: %v", err))
				return
			}

			log.Printf("  ok    %-35s (%d bytes)", ic.name, n)
			mu.Lock()
			ok++
			mu.Unlock()
		}()
	}

	wg.Wait()

	fmt.Printf("\nDone: %d downloaded, %d failed\n", ok, bad)
	if bad > 0 {
		fmt.Printf("\nFor failed icons, you can:\n")
		fmt.Printf("  1. Check if the source CDN was updated: %s\n", baseURL)
		fmt.Printf("  2. Download manually and place in: %s\n", destDir)
		fmt.Printf("  3. Point ICONS_SOURCE_URL at a mirror that has the files\n")
		os.Exit(1)
	}
}

func warn(mu *sync.Mutex, bad *int, ic icon, reason string) {
	note := ""
	if ic.note != "" {
		note = " [" + ic.note + "]"
	}
	log.Printf("  FAIL  %-35s %s%s", ic.name, reason, note)
	mu.Lock()
	*bad++
	mu.Unlock()
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
