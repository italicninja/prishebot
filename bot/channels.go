// Per-guild channel allow-lists for slash commands.
//
// Two layers compose with AND:
//   - Global    - a guild-wide allow-list. Empty = every channel is allowed.
//   - Per-module - narrows the global gate for one module. Empty = inherits the global gate.
//
// A command is permitted in a channel only if both gates allow it. Default
// (no entries at either level) is "all channels" - admins opt in to restrict.
package bot

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"sync"
)

// ChannelPermissions is the per-guild channel-allow-list store.
type ChannelPermissions struct {
	file string

	mu    sync.RWMutex
	state map[string]channelState // key: guildID
}

// channelState is the on-disk shape per guild.
type channelState struct {
	Global  []string            `json:"global,omitempty"`
	Modules map[string][]string `json:"modules,omitempty"`
}

// NewChannelPermissions constructs the store and loads any persisted state.
func NewChannelPermissions(file string) *ChannelPermissions {
	cp := &ChannelPermissions{file: file, state: make(map[string]channelState)}
	cp.load()
	return cp
}

// Allowed reports whether moduleName may be invoked in channelID for guildID.
// Empty allow-lists mean "no restriction at that layer".
func (cp *ChannelPermissions) Allowed(guildID, moduleName, channelID string) bool {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	st, ok := cp.state[guildID]
	if !ok {
		return true
	}
	if len(st.Global) > 0 && !slices.Contains(st.Global, channelID) {
		return false
	}
	if list, ok := st.Modules[moduleName]; ok && len(list) > 0 {
		if !slices.Contains(list, channelID) {
			return false
		}
	}
	return true
}

// GetGlobal returns the configured global channel allow-list for a guild.
func (cp *ChannelPermissions) GetGlobal(guildID string) []string {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	src := cp.state[guildID].Global
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// GetModule returns the configured per-module channel allow-list for a guild.
func (cp *ChannelPermissions) GetModule(guildID, moduleName string) []string {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	src := cp.state[guildID].Modules[moduleName]
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// SetGlobal replaces the guild-wide channel allow-list.
// An empty or nil slice clears the entry, returning the gate to "all channels".
func (cp *ChannelPermissions) SetGlobal(guildID string, channelIDs []string) {
	cp.mu.Lock()
	st := cp.state[guildID]
	if len(channelIDs) == 0 {
		st.Global = nil
	} else {
		stored := make([]string, len(channelIDs))
		copy(stored, channelIDs)
		st.Global = stored
	}
	cp.persistGuild(guildID, st)
	cp.mu.Unlock()
	cp.save()
}

// SetModule replaces the per-module channel allow-list for a guild.
// An empty or nil slice clears the entry, returning the gate to inherit-from-global.
func (cp *ChannelPermissions) SetModule(guildID, moduleName string, channelIDs []string) {
	cp.mu.Lock()
	st := cp.state[guildID]
	if st.Modules == nil {
		st.Modules = make(map[string][]string)
	}
	if len(channelIDs) == 0 {
		delete(st.Modules, moduleName)
	} else {
		stored := make([]string, len(channelIDs))
		copy(stored, channelIDs)
		st.Modules[moduleName] = stored
	}
	cp.persistGuild(guildID, st)
	cp.mu.Unlock()
	cp.save()
}

// persistGuild stores the updated state back, deleting the guild key entirely
// when it carries no useful settings so the JSON file stays compact.
// Caller must hold cp.mu in write mode.
func (cp *ChannelPermissions) persistGuild(guildID string, st channelState) {
	if len(st.Global) == 0 && len(st.Modules) == 0 {
		delete(cp.state, guildID)
		return
	}
	if len(st.Modules) == 0 {
		st.Modules = nil
	}
	cp.state[guildID] = st
}

func (cp *ChannelPermissions) save() {
	cp.mu.RLock()
	data, err := json.MarshalIndent(cp.state, "", "  ")
	cp.mu.RUnlock()
	if err != nil {
		log.Printf("[channels] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(cp.file, data, 0600); err != nil {
		log.Printf("[channels] write %s error: %v", cp.file, err)
	}
}

func (cp *ChannelPermissions) load() {
	raw, err := os.ReadFile(cp.file)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[channels] read %s error: %v", cp.file, err)
		}
		return
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if err := json.Unmarshal(raw, &cp.state); err != nil {
		log.Printf("[channels] parse %s error: %v", cp.file, err)
	}
}
