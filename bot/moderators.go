// Per-guild moderator role lists for the web dashboard.
//
// An admin opens the server page and designates one or more roles as
// "dashboard moderator" roles. Any member who holds at least one of those
// roles can sign in and manage the day-to-day pages (raids, birthdays, the
// self-assignable role list) for that server. Destructive actions — removing
// the bot, changing module enablement, editing the moderator list itself —
// stay restricted to Discord-level admins.
package bot

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"sync"
)

// ModeratorRoles stores the per-guild dashboard-moderator role allow-list.
// A missing or empty entry means "no moderators — admins only".
type ModeratorRoles struct {
	file string

	mu sync.RWMutex
	// roles[guildID] = []roleID
	roles map[string][]string
}

// NewModeratorRoles constructs the store and loads any persisted state.
// A read error other than "file not found" is logged but not fatal — we'd
// rather start with an empty state than refuse to boot.
func NewModeratorRoles(file string) *ModeratorRoles {
	mr := &ModeratorRoles{file: file, roles: make(map[string][]string)}
	mr.load()
	return mr
}

// Get returns the configured moderator role IDs for a guild, or nil if none.
func (mr *ModeratorRoles) Get(guildID string) []string {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	src := mr.roles[guildID]
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// Set replaces the moderator role allow-list for a guild.
// An empty or nil slice clears the entry.
func (mr *ModeratorRoles) Set(guildID string, roleIDs []string) {
	mr.mu.Lock()
	if len(roleIDs) == 0 {
		delete(mr.roles, guildID)
	} else {
		stored := make([]string, len(roleIDs))
		copy(stored, roleIDs)
		mr.roles[guildID] = stored
	}
	mr.mu.Unlock()
	mr.save()
}

// HasAny reports whether any of memberRoleIDs is configured as a moderator
// role for guildID. Used at login to decide whether to surface a guild on
// the dashboard for a non-admin user.
func (mr *ModeratorRoles) HasAny(guildID string, memberRoleIDs []string) bool {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	allowed := mr.roles[guildID]
	if len(allowed) == 0 {
		return false
	}
	for _, rid := range memberRoleIDs {
		if slices.Contains(allowed, rid) {
			return true
		}
	}
	return false
}

// GuildsConfigured returns the set of guild IDs that have at least one
// moderator role configured. Used at login to narrow the GuildMember lookups
// to only the guilds where moderator access could possibly apply.
func (mr *ModeratorRoles) GuildsConfigured() map[string]bool {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	out := make(map[string]bool, len(mr.roles))
	for gid, list := range mr.roles {
		if len(list) > 0 {
			out[gid] = true
		}
	}
	return out
}

func (mr *ModeratorRoles) save() {
	mr.mu.RLock()
	data, err := json.MarshalIndent(mr.roles, "", "  ")
	mr.mu.RUnlock()
	if err != nil {
		log.Printf("[moderators] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(mr.file, data, 0600); err != nil {
		log.Printf("[moderators] write %s error: %v", mr.file, err)
	}
}

func (mr *ModeratorRoles) load() {
	raw, err := os.ReadFile(mr.file)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[moderators] read %s error: %v", mr.file, err)
		}
		return
	}
	mr.mu.Lock()
	defer mr.mu.Unlock()
	if err := json.Unmarshal(raw, &mr.roles); err != nil {
		log.Printf("[moderators] parse %s error: %v", mr.file, err)
	}
}
