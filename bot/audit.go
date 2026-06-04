// Per-guild audit-log channel - set by the server owner from the dashboard
// to receive notifications when anyone makes a configuration change via the
// web UI. Empty / missing means "auditing disabled" for that guild.
package bot

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// AuditChannels stores the per-guild audit channel mapping.
type AuditChannels struct {
	file string

	mu       sync.RWMutex
	channels map[string]string // guildID -> channelID
}

// NewAuditChannels constructs the store and loads any persisted state.
func NewAuditChannels(file string) *AuditChannels {
	ac := &AuditChannels{file: file, channels: make(map[string]string)}
	ac.load()
	return ac
}

// Get returns the configured channel ID for a guild, or "" if unset.
func (ac *AuditChannels) Get(guildID string) string {
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	return ac.channels[guildID]
}

// Set replaces the audit channel for a guild. Pass "" to disable auditing.
func (ac *AuditChannels) Set(guildID, channelID string) {
	ac.mu.Lock()
	if channelID == "" {
		delete(ac.channels, guildID)
	} else {
		ac.channels[guildID] = channelID
	}
	ac.mu.Unlock()
	ac.save()
}

func (ac *AuditChannels) save() {
	ac.mu.RLock()
	data, err := json.MarshalIndent(ac.channels, "", "  ")
	ac.mu.RUnlock()
	if err != nil {
		log.Printf("[audit] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(ac.file, data, 0600); err != nil {
		log.Printf("[audit] write %s error: %v", ac.file, err)
	}
}

func (ac *AuditChannels) load() {
	raw, err := os.ReadFile(ac.file)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[audit] read %s error: %v", ac.file, err)
		}
		return
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if err := json.Unmarshal(raw, &ac.channels); err != nil {
		log.Printf("[audit] parse %s error: %v", ac.file, err)
	}
}
