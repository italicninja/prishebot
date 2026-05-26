// Diff helpers used to render explicit, change-by-change audit log embeds
// from the dashboard "Save changes" form.
//
// Output format is a Discord-flavoured markdown string ready to drop into an
// embed description: bold section headers, bullet lines, role and channel
// references rendered with Discord's <@&id> / <#id> syntax so they render as
// clickable mentions. Mentions are rendered safely — the audit message
// suppresses the ping with allowed_mentions (see audit.go).
package web

import (
	"sort"
	"strings"
)

// diffStringSet returns the IDs present in after that weren't in before, and
// vice versa. Order is by sort, so output is deterministic across saves.
func diffStringSet(before, after []string) (added, removed []string) {
	bset := make(map[string]struct{}, len(before))
	for _, b := range before {
		bset[b] = struct{}{}
	}
	aset := make(map[string]struct{}, len(after))
	for _, a := range after {
		aset[a] = struct{}{}
	}
	for a := range aset {
		if _, ok := bset[a]; !ok {
			added = append(added, a)
		}
	}
	for b := range bset {
		if _, ok := aset[b]; !ok {
			removed = append(removed, b)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// roleMentions formats role IDs as Discord role mentions joined by ", ".
func roleMentions(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "<@&" + id + ">"
	}
	return strings.Join(out, ", ")
}

// channelMentions formats channel IDs as Discord channel mentions joined by ", ".
func channelMentions(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "<#" + id + ">"
	}
	return strings.Join(out, ", ")
}

// addRemoveLine builds a "+ added X, − removed Y" segment for a single
// command/section, given pre-formatted mention strings. Returns empty when
// both sides are empty.
func addRemoveLine(added, removed string) string {
	parts := make([]string, 0, 2)
	if added != "" {
		parts = append(parts, "added "+added)
	}
	if removed != "" {
		parts = append(parts, "removed "+removed)
	}
	return strings.Join(parts, ", ")
}

// moduleSaveDiffInput packages all the before/after snapshots needed to
// describe a server-settings save. Keeping it in one struct makes the
// handler call site short and the diff builder easy to unit-test later.
type moduleSaveDiffInput struct {
	BeforeModules     map[string]bool     // map[moduleName]enabled
	AfterModules      map[string]bool     // same shape, computed from form
	BeforeCmdRoles    map[string][]string // map[commandName]roleIDs
	AfterCmdRoles     map[string][]string
	BeforeChGlobal    []string
	AfterChGlobal     []string
	BeforeChModule    map[string][]string // map[moduleName]channelIDs
	AfterChModule     map[string][]string
	ShowModRoles      bool // admin submitted — moderator_roles is in scope
	BeforeModRoles    []string
	AfterModRoles     []string
	ShowAuditChannel  bool // owner submitted — audit_channel is in scope
	BeforeAuditChan   string
	AfterAuditChan    string
}

// buildModuleSaveDiff returns a markdown description of every change made by
// a handleUpdateModules submission, or "" if nothing actually changed. We
// skip the audit post entirely in the no-op case so admins clicking "Save"
// to bail out of an edit don't spam the channel.
func buildModuleSaveDiff(in moduleSaveDiffInput) string {
	var sections []string

	// ── Modules: per-module enabled flip ───────────────────────────────────
	var moduleLines []string
	moduleNames := sortedKeys(in.AfterModules)
	for _, name := range moduleNames {
		before := in.BeforeModules[name]
		after := in.AfterModules[name]
		if before == after {
			continue
		}
		from := "disabled"
		to := "enabled"
		if before {
			from, to = "enabled", "disabled"
		}
		moduleLines = append(moduleLines, "• `"+name+"`: "+from+" → "+to)
	}
	if len(moduleLines) > 0 {
		sections = append(sections, "**Modules**\n"+strings.Join(moduleLines, "\n"))
	}

	// ── Per-command role allow-lists ──────────────────────────────────────
	var cmdLines []string
	cmdNames := mergedSortedKeys(in.BeforeCmdRoles, in.AfterCmdRoles)
	for _, cmd := range cmdNames {
		added, removed := diffStringSet(in.BeforeCmdRoles[cmd], in.AfterCmdRoles[cmd])
		if len(added) == 0 && len(removed) == 0 {
			continue
		}
		seg := addRemoveLine(roleMentions(added), roleMentions(removed))
		cmdLines = append(cmdLines, "• `/"+cmd+"`: "+seg)
	}
	if len(cmdLines) > 0 {
		sections = append(sections, "**Command roles**\n"+strings.Join(cmdLines, "\n"))
	}

	// ── Global channel allow-list ─────────────────────────────────────────
	if added, removed := diffStringSet(in.BeforeChGlobal, in.AfterChGlobal); len(added)+len(removed) > 0 {
		seg := addRemoveLine(channelMentions(added), channelMentions(removed))
		sections = append(sections, "**Channels — global**\n• "+seg)
	}

	// ── Per-module channel allow-lists ────────────────────────────────────
	var modChLines []string
	for _, name := range sortedKeys(in.AfterChModule) {
		added, removed := diffStringSet(in.BeforeChModule[name], in.AfterChModule[name])
		if len(added) == 0 && len(removed) == 0 {
			continue
		}
		seg := addRemoveLine(channelMentions(added), channelMentions(removed))
		modChLines = append(modChLines, "• `"+name+"`: "+seg)
	}
	if len(modChLines) > 0 {
		sections = append(sections, "**Channels — per module**\n"+strings.Join(modChLines, "\n"))
	}

	// ── Dashboard moderators (admin-scoped field) ─────────────────────────
	if in.ShowModRoles {
		if added, removed := diffStringSet(in.BeforeModRoles, in.AfterModRoles); len(added)+len(removed) > 0 {
			seg := addRemoveLine(roleMentions(added), roleMentions(removed))
			sections = append(sections, "**Dashboard moderators**\n• "+seg)
		}
	}

	// ── Audit channel (owner-scoped field) ────────────────────────────────
	if in.ShowAuditChannel && in.BeforeAuditChan != in.AfterAuditChan {
		var line string
		switch {
		case in.BeforeAuditChan == "":
			line = "set to <#" + in.AfterAuditChan + ">"
		case in.AfterAuditChan == "":
			line = "cleared (was <#" + in.BeforeAuditChan + ">) — auditing now disabled"
		default:
			line = "<#" + in.BeforeAuditChan + "> → <#" + in.AfterAuditChan + ">"
		}
		sections = append(sections, "**Audit channel**\n• "+line)
	}

	return strings.Join(sections, "\n\n")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mergedSortedKeys returns the union of keys from two maps, sorted.
// Used so the diff covers commands that existed only before or only after.
func mergedSortedKeys[V any](a, b map[string]V) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
