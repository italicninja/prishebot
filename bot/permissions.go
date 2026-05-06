// Per-guild role-based access control for slash commands.
//
// By default every command is admin-only (Administrator or Manage Server).
// Admins use the web dashboard to grant access to specific roles per command.
// The @everyone role (whose ID equals the guild ID) opens a command to all members.
//
// CommandPermissions is the source of truth at runtime. The Discord-level
// DefaultMemberPermissions = ManageGuild is set at command registration so
// non-admins don't see the commands in the slash menu, but enforcement is
// always done server-side here.
package bot

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// CommandPermissions stores per-guild allow-lists keyed by top-level command name.
// A missing or empty allow-list means "admin-only".
type CommandPermissions struct {
	file string

	mu sync.RWMutex
	// roles[guildID][commandName] = []roleID
	roles map[string]map[string][]string
}

// NewCommandPermissions constructs the store and loads any persisted state.
// A read error other than "file not found" is logged but not fatal — we'd
// rather start with an empty (admin-only) state than refuse to boot.
func NewCommandPermissions(file string) *CommandPermissions {
	cp := &CommandPermissions{file: file, roles: make(map[string]map[string][]string)}
	cp.load()
	return cp
}

// Allowed reports whether a guild member may invoke a command.
// Members with Administrator or Manage Server permission always pass — this
// guarantees an admin can never accidentally lock themselves out.
func (cp *CommandPermissions) Allowed(guildID, cmdName string, member *discordgo.Member) bool {
	if member == nil {
		return false
	}
	if member.Permissions&discordgo.PermissionAdministrator != 0 ||
		member.Permissions&discordgo.PermissionManageGuild != 0 {
		return true
	}

	cp.mu.RLock()
	defer cp.mu.RUnlock()

	guildMap, ok := cp.roles[guildID]
	if !ok {
		return false
	}
	allowed, ok := guildMap[cmdName]
	if !ok || len(allowed) == 0 {
		return false
	}

	// @everyone (its role ID equals the guild ID) opens the command to all members.
	if slices.Contains(allowed, guildID) {
		return true
	}

	have := make(map[string]struct{}, len(member.Roles))
	for _, r := range member.Roles {
		have[r] = struct{}{}
	}
	for _, rid := range allowed {
		if _, ok := have[rid]; ok {
			return true
		}
	}
	return false
}

// GetRoles returns the configured role IDs for a command in a guild, or nil if none.
func (cp *CommandPermissions) GetRoles(guildID, cmdName string) []string {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	if cp.roles[guildID] == nil {
		return nil
	}
	src := cp.roles[guildID][cmdName]
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// SetRoles replaces the allow-list for a command in a guild.
// An empty or nil slice clears the entry, returning the command to admin-only.
func (cp *CommandPermissions) SetRoles(guildID, cmdName string, roleIDs []string) {
	cp.mu.Lock()
	if cp.roles[guildID] == nil {
		cp.roles[guildID] = make(map[string][]string)
	}
	if len(roleIDs) == 0 {
		delete(cp.roles[guildID], cmdName)
		if len(cp.roles[guildID]) == 0 {
			delete(cp.roles, guildID)
		}
	} else {
		stored := make([]string, len(roleIDs))
		copy(stored, roleIDs)
		cp.roles[guildID][cmdName] = stored
	}
	cp.mu.Unlock()
	cp.save()
}

// GuildSettings returns a copy of all command -> role-IDs entries for a guild.
func (cp *CommandPermissions) GuildSettings(guildID string) map[string][]string {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	src := cp.roles[guildID]
	out := make(map[string][]string, len(src))
	for cmd, ids := range src {
		cp := make([]string, len(ids))
		copy(cp, ids)
		out[cmd] = cp
	}
	return out
}

func (cp *CommandPermissions) save() {
	cp.mu.RLock()
	data, err := json.MarshalIndent(cp.roles, "", "  ")
	cp.mu.RUnlock()
	if err != nil {
		log.Printf("[permissions] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(cp.file, data, 0600); err != nil {
		log.Printf("[permissions] write %s error: %v", cp.file, err)
	}
}

func (cp *CommandPermissions) load() {
	raw, err := os.ReadFile(cp.file)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[permissions] read %s error: %v", cp.file, err)
		}
		return
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if err := json.Unmarshal(raw, &cp.roles); err != nil {
		log.Printf("[permissions] parse %s error: %v", cp.file, err)
	}
}
