// Persistence for per-guild module enable/disable state.
//
// guildSettings lives on Bot because the hot path (IsModuleEnabled, called on
// every interaction) reads it under the same RWMutex that guards the module
// registry. Save/load helpers are kept here so bot.go stays focused on
// coordination, not I/O.
package bot

import (
	"encoding/json"
	"log"
	"os"
)

// loadModuleState reads persisted per-guild module state from disk into
// b.guildSettings. Called once at construction; safe to call before any
// other goroutines hold b.mu.
func (b *Bot) loadModuleState() {
	if b.cfg.ModuleStateFile == "" {
		return
	}
	raw, err := os.ReadFile(b.cfg.ModuleStateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[module-state] read %s: %v", b.cfg.ModuleStateFile, err)
		}
		return
	}
	var state map[string]map[string]bool
	if err := json.Unmarshal(raw, &state); err != nil {
		log.Printf("[module-state] parse %s: %v", b.cfg.ModuleStateFile, err)
		return
	}
	if state != nil {
		b.guildSettings = state
	}
}

// saveModuleState writes b.guildSettings to disk. Safe to call without
// holding b.mu — it takes the read lock internally.
func (b *Bot) saveModuleState() {
	if b.cfg.ModuleStateFile == "" {
		return
	}
	b.mu.RLock()
	data, err := json.MarshalIndent(b.guildSettings, "", "  ")
	b.mu.RUnlock()
	if err != nil {
		log.Printf("[module-state] marshal: %v", err)
		return
	}
	if err := os.WriteFile(b.cfg.ModuleStateFile, data, 0600); err != nil {
		log.Printf("[module-state] write %s: %v", b.cfg.ModuleStateFile, err)
	}
}
