package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gin-gonic/gin"

	"github.com/user/discord-bot-skeleton/bot"
	"github.com/user/discord-bot-skeleton/bot/modules/birthday"
	"github.com/user/discord-bot-skeleton/bot/modules/raid"
	"github.com/user/discord-bot-skeleton/bot/modules/roles"
)

// ── Public handlers ──────────────────────────────────────────────────────────

func (s *Server) handleHome(c *gin.Context) {
	if sess := s.sessionFromCookie(c); sess != nil {
		c.Redirect(http.StatusFound, "/dashboard")
		return
	}
	if err := s.tmpl.ExecuteTemplate(c.Writer, "login.html", nil); err != nil {
		log.Printf("[web] failed to render login.html: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleLogin kicks off the Discord OAuth2 flow.
//
// We generate a random "state" value, store it in a short-lived cookie, then
// redirect the user to Discord's authorization page. When Discord redirects
// back to /auth/callback, we compare the state value to prevent CSRF attacks:
// a malicious site cannot predict the random state, so their crafted callback
// URL will be rejected.
func (s *Server) handleLogin(c *gin.Context) {
	state := randomState()
	// HttpOnly=true: JavaScript on the page cannot read this cookie, preventing
	// XSS-based theft of the state token.
	c.SetCookie("oauth_state", state, 300, "/", "", s.cfg.SecureCookies, true)
	c.Redirect(http.StatusFound, s.oauth.AuthCodeURL(state))
}

// handleCallback processes the OAuth2 redirect from Discord.
func (s *Server) handleCallback(c *gin.Context) {
	// 1. Verify CSRF state using constant-time comparison to prevent timing attacks.
	stateCookie, err := c.Cookie("oauth_state")
	if err != nil || subtle.ConstantTimeCompare([]byte(stateCookie), []byte(c.Query("state"))) != 1 {
		c.String(http.StatusBadRequest, "Invalid OAuth state — possible CSRF attempt.")
		return
	}

	// 2. Exchange the one-time authorization code for an access token.
	// The code is short-lived (~10 minutes) and single-use. Discord's token
	// endpoint validates our client_secret here, so the exchange is secure
	// even over a network that could see the code.
	code := c.Query("code")
	if code == "" {
		c.String(http.StatusBadRequest, "Missing authorization code.")
		return
	}
	token, err := s.oauth.Exchange(c.Request.Context(), code)
	if err != nil {
		log.Printf("[web] token exchange failed: %v", err)
		c.String(http.StatusInternalServerError, "Token exchange failed. Please try logging in again.")
		return
	}

	// 3. Fetch the user's profile and admin guild list from Discord.
	user, err := fetchCurrentUser(c.Request.Context(), token, s.oauth)
	if err != nil {
		c.String(http.StatusInternalServerError, "Could not fetch user info.")
		return
	}
	rawGuilds, err := fetchUserGuilds(c.Request.Context(), token, s.oauth)
	if err != nil {
		c.String(http.StatusInternalServerError, "Could not fetch guild list.")
		return
	}

	// 4. Derive the visible guild list from the raw list. Stored in session
	// AND on rebuild during every /dashboard render — keeps things working
	// if the bot wasn't connected at OAuth time (Railway healthcheck starts
	// the web server before module load finishes).
	guilds := s.buildVisibleGuilds(rawGuilds, user.ID)

	// 5. Create session and set the cookie.
	sess := s.store.Create(user.ID, user.Username, userAvatarURL(user.ID, user.Avatar), token.AccessToken, rawGuilds, guilds)
	// MaxAge=86400 = 24 hours in seconds
	c.SetCookie("session_id", sess.ID, 86400, "/", "", s.cfg.SecureCookies, true)
	c.Redirect(http.StatusFound, "/dashboard")
}

func (s *Server) handleLogout(c *gin.Context) {
	if id, err := c.Cookie("session_id"); err == nil {
		s.store.Delete(id)
	}
	// Delete cookie by setting MaxAge=-1
	c.SetCookie("session_id", "", -1, "/", "", s.cfg.SecureCookies, true)
	c.Redirect(http.StatusFound, "/")
}

// ── Authenticated handlers ────────────────────────────────────────────────────

func (s *Server) handleDashboard(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	// Recompute from the raw guild list against the *current* bot state.
	// If the bot wasn't ready when the user logged in (Railway boots web
	// first, then modules, then the gateway), the initial Guilds list will
	// have been missing moderator entries and showed admin entries as
	// "Not added"; this catches up as soon as the bot is up.
	if sess.RawGuilds != nil {
		sess.Guilds = s.buildVisibleGuilds(sess.RawGuilds, sess.UserID)
	}
	guilds := sess.Guilds
	sort.SliceStable(guilds, func(i, j int) bool {
		return guilds[i].BotPresent && !guilds[j].BotPresent
	})
	if err := s.tmpl.ExecuteTemplate(c.Writer, "dashboard.html", gin.H{
		"User":   sess,
		"Guilds": guilds,
	}); err != nil {
		log.Printf("[web] failed to render dashboard.html: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleServerPage renders the management page for a single server.
// Open to both admins and moderators. The template hides the Danger Zone
// and the moderator-role editor for non-admins; handleUpdateModules
// re-checks the role before applying admin-only form fields.
func (s *Server) handleServerPage(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	src := requireGuildAccess(c, sess, guildID, false)
	if src == nil {
		return
	}
	// Copy and refresh BotPresent from live bot state.
	guild := *src
	guild.BotPresent = s.botGuildSet()[guildID]

	settings := s.bot.GuildModuleSettings(guildID)
	modules := s.bot.Modules()

	// Group registered command names by owning module.
	cmdsByModule := make(map[string][]string)
	for _, ci := range s.bot.RegisteredCommands() {
		cmdsByModule[ci.Module] = append(cmdsByModule[ci.Module], ci.Name)
	}

	cp := s.bot.CommandPerms()
	configured := cp.GuildSettings(guildID)
	chp := s.bot.ChannelPerms()

	type CommandRow struct {
		Name        string   // top-level command name, e.g. "raid"
		Signatures  []string // pretty signatures for display, e.g. "/raid create <title>"
		SelectedIDs []string // configured role IDs (preserves order)
	}
	type ModuleRow struct {
		Name        string
		Description string
		Enabled     bool
		Commands    []CommandRow
		ChannelIDs  []string // configured per-module channel allow-list
	}
	type CategoryGroup struct {
		Name    string
		Modules []ModuleRow
	}

	rowsByCategory := make(map[bot.Category][]ModuleRow)
	for name, mod := range modules {
		var cmdRows []CommandRow
		for _, c := range mod.Commands() {
			cmdRows = append(cmdRows, CommandRow{
				Name:        c.Name,
				Signatures:  commandSignatures(c),
				SelectedIDs: configured[c.Name],
			})
		}
		row := ModuleRow{
			Name:        name,
			Description: mod.Description(),
			Enabled:     settings[name],
			Commands:    cmdRows,
			ChannelIDs:  chp.GetModule(guildID, name),
		}
		cat := mod.Category()
		if cat == "" {
			cat = bot.CategoryFunctional
		}
		rowsByCategory[cat] = append(rowsByCategory[cat], row)
	}

	var categories []CategoryGroup
	for _, cat := range bot.CategoryOrder {
		mods := rowsByCategory[cat]
		if len(mods) == 0 {
			continue
		}
		sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
		categories = append(categories, CategoryGroup{Name: string(cat), Modules: mods})
	}

	// Available roles for the multi-select. Includes @everyone (its role ID
	// equals the guild ID) so admins can open a command to all members.
	type RoleOption struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		ColorHex string `json:"color"`
	}
	var roleOptions []RoleOption
	if guild.BotPresent {
		discordRoles, err := s.bot.Session().GuildRoles(guildID)
		if err != nil {
			log.Printf("[web] GuildRoles %s: %v", guildID, err)
		} else {
			for _, r := range discordRoles {
				if r.Managed {
					continue
				}
				display := r.Name
				if r.ID == guildID {
					display = "@everyone"
				}
				roleOptions = append(roleOptions, RoleOption{
					ID:       r.ID,
					Name:     display,
					ColorHex: roleColorHex(r.Color),
				})
			}
			sort.SliceStable(roleOptions, func(i, j int) bool {
				if roleOptions[i].ID == guildID {
					return true
				}
				if roleOptions[j].ID == guildID {
					return false
				}
				return strings.ToLower(roleOptions[i].Name) < strings.ToLower(roleOptions[j].Name)
			})
		}
	}
	rolesJSON, _ := json.Marshal(roleOptions)

	// Available text channels for the channel multi-select.
	type ChannelOpt struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var channelOptions []ChannelOpt
	if guild.BotPresent {
		if gchans, err := s.bot.Session().GuildChannels(guildID); err == nil {
			for _, ch := range gchans {
				if ch.Type == discordgo.ChannelTypeGuildText {
					channelOptions = append(channelOptions, ChannelOpt{ID: ch.ID, Name: "#" + ch.Name})
				}
			}
			sort.Slice(channelOptions, func(i, j int) bool {
				return strings.ToLower(channelOptions[i].Name) < strings.ToLower(channelOptions[j].Name)
			})
		} else {
			log.Printf("[web] GuildChannels %s: %v", guildID, err)
		}
	}
	channelsJSON, _ := json.Marshal(channelOptions)

	if err := s.tmpl.ExecuteTemplate(c.Writer, "server.html", gin.H{
		"User":           sess,
		"Guild":          &guild,
		"IsAdmin":        guild.Role == RoleAdmin,
		"IsOwner":        guild.IsOwner,
		"Categories":     categories,
		"GlobalChannels": chp.GetGlobal(guildID),
		"ModeratorRoles": s.bot.ModeratorRoles().Get(guildID),
		"AuditChannelID": s.bot.AuditChannels().Get(guildID),
		"AllChannels":    channelOptions,
		"RolesJSON":      template.JS(rolesJSON),
		"ChannelsJSON":   template.JS(channelsJSON),
		"Saved":          c.Query("saved") == "1",
		"SyncWarn":       c.Query("sync_warn"),
		"Resynced":       c.Query("resync"),
		"ClientID":       s.cfg.ClientID,
	}); err != nil {
		log.Printf("[web] failed to render server.html: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleLeaveServer makes the bot leave the guild, then redirects to the dashboard.
func (s *Server) handleLeaveServer(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, true) == nil {
		return
	}

	// Post the audit log BEFORE leaving — the channel send needs the bot to
	// still be a member of the guild.
	s.postAudit(guildID, sess, "Bot removed from server",
		"The dashboard owner triggered the bot to leave this server. Future settings changes will not be possible until the bot is re-invited.")

	if err := s.bot.Session().GuildLeave(guildID); err != nil {
		log.Printf("[web] failed to leave guild %s: %v", guildID, err)
		c.String(http.StatusInternalServerError, "Failed to leave server.")
		return
	}

	c.Redirect(http.StatusFound, "/dashboard")
}

// handleUpdateModules processes the combined module-and-permissions form on
// the server page. The form posts:
//   - module_<name>            — checkbox per enabled module
//   - roles_<commandName>      — zero or more role IDs per command
func (s *Server) handleUpdateModules(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	g := requireGuildAccess(c, sess, guildID, false)
	if g == nil {
		return
	}

	if err := c.Request.ParseForm(); err != nil {
		c.String(http.StatusBadRequest, "Invalid form data.")
		return
	}

	// ── BEFORE-state snapshot ────────────────────────────────────────────
	// Capture everything that could change before we mutate any store, so we
	// can emit a precise audit-log diff after the save.
	cp := s.bot.CommandPerms()
	chp := s.bot.ChannelPerms()
	known := s.bot.Modules()

	beforeModules := s.bot.GuildModuleSettings(guildID)
	beforeCmdRoles := cp.GuildSettings(guildID)
	beforeChGlobal := chp.GetGlobal(guildID)
	beforeChModule := make(map[string][]string, len(known))
	for name := range known {
		beforeChModule[name] = chp.GetModule(guildID, name)
	}
	var beforeModRoles []string
	if g.Role == RoleAdmin {
		beforeModRoles = s.bot.ModeratorRoles().Get(guildID)
	}
	var beforeAuditChan string
	if g.IsOwner {
		beforeAuditChan = s.bot.AuditChannels().Get(guildID)
	}

	// ── Apply: module toggles ─────────────────────────────────────────────
	enabledForm := make(map[string]bool)
	for key := range c.Request.Form {
		if name, ok := strings.CutPrefix(key, "module_"); ok {
			enabledForm[name] = true
		}
	}
	afterModules := make(map[string]bool, len(known))
	for name := range known {
		afterModules[name] = enabledForm[name] // false when missing
		s.bot.SetModuleEnabled(guildID, name, afterModules[name])
	}

	// ── Apply: per-command role allow-lists + push to Discord ─────────────
	afterCmdRoles := make(map[string][]string)
	var syncErrors, syncUnauthorized int
	for _, ci := range s.bot.RegisteredCommands() {
		var ids []string
		for _, id := range c.Request.PostForm["roles_"+ci.Name] {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		cp.SetRoles(guildID, ci.Name, ids)
		afterCmdRoles[ci.Name] = ids

		cmdID := s.bot.CommandIDByName(ci.Name)
		if cmdID == "" {
			continue
		}
		if err := syncCommandPermissions(c.Request.Context(), sess.AccessToken, s.cfg.ClientID, guildID, cmdID, ids); err != nil {
			log.Printf("[web] sync slash-menu visibility for /%s in %s: %v", ci.Name, guildID, err)
			syncErrors++
			if strings.Contains(err.Error(), "401") {
				syncUnauthorized++
			}
		}
	}

	// ── Apply: channel allow-lists ────────────────────────────────────────
	afterChGlobal := cleanIDs(c.Request.PostForm["channels_global"])
	chp.SetGlobal(guildID, afterChGlobal)
	afterChModule := make(map[string][]string, len(known))
	for name := range known {
		afterChModule[name] = cleanIDs(c.Request.PostForm["channels_module_"+name])
		chp.SetModule(guildID, name, afterChModule[name])
	}

	// ── Apply: moderator roles (admin-only) ───────────────────────────────
	var afterModRoles []string
	if g.Role == RoleAdmin {
		afterModRoles = cleanIDs(c.Request.PostForm["moderator_roles"])
		s.bot.ModeratorRoles().Set(guildID, afterModRoles)
	}

	// ── Apply: audit channel (owner-only) ─────────────────────────────────
	var afterAuditChan string
	if g.IsOwner {
		afterAuditChan = strings.TrimSpace(c.Request.PostForm.Get("audit_channel"))
		s.bot.AuditChannels().Set(guildID, afterAuditChan)
	}

	// ── Audit ─────────────────────────────────────────────────────────────
	diff := buildModuleSaveDiff(moduleSaveDiffInput{
		BeforeModules:    beforeModules,
		AfterModules:     afterModules,
		BeforeCmdRoles:   beforeCmdRoles,
		AfterCmdRoles:    afterCmdRoles,
		BeforeChGlobal:   beforeChGlobal,
		AfterChGlobal:    afterChGlobal,
		BeforeChModule:   beforeChModule,
		AfterChModule:    afterChModule,
		ShowModRoles:     g.Role == RoleAdmin,
		BeforeModRoles:   beforeModRoles,
		AfterModRoles:    afterModRoles,
		ShowAuditChannel: g.IsOwner,
		BeforeAuditChan:  beforeAuditChan,
		AfterAuditChan:   afterAuditChan,
	})
	if diff != "" {
		s.postAudit(guildID, sess, "Server settings updated", diff)
	}

	// Pass any sync warning forward so the dashboard can show a banner. The
	// dashboard config is already saved at this point — we just want the
	// admin to know that Discord-side slash menu visibility may not match
	// what they see in the UI.
	redirect := "/dashboard/server/" + guildID + "?saved=1"
	if syncUnauthorized > 0 {
		redirect += "&sync_warn=unauthorized"
	} else if syncErrors > 0 {
		redirect += "&sync_warn=error"
	}
	c.Redirect(http.StatusFound, redirect)
}

// handleResyncCommandPerms re-pushes the saved role allow-list for every
// registered command to Discord, without touching the local config. Use this
// after re-authenticating (so the session has the
// applications.commands.permissions.update scope) to fix divergence between
// what the dashboard shows and what Discord's slash menu enforces.
//
// Admin-only: same trust boundary as handleUpdateModules — pushing overrides
// to Discord requires ManageGuild and the per-guild config is admin-owned.
func (s *Server) handleResyncCommandPerms(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, true) == nil {
		return
	}

	cp := s.bot.CommandPerms()
	var ok, failed, unauthorized int
	for _, ci := range s.bot.RegisteredCommands() {
		cmdID := s.bot.CommandIDByName(ci.Name)
		if cmdID == "" {
			continue
		}
		ids := cp.GetRoles(guildID, ci.Name)
		if err := syncCommandPermissions(c.Request.Context(), sess.AccessToken, s.cfg.ClientID, guildID, cmdID, ids); err != nil {
			log.Printf("[web] resync /%s in %s: %v", ci.Name, guildID, err)
			failed++
			if strings.Contains(err.Error(), "401") {
				unauthorized++
			}
			continue
		}
		ok++
	}

	s.postAudit(guildID, sess, "Slash-menu permissions resynced",
		"Re-pushed the saved command role allow-lists to Discord — "+strconv.Itoa(ok)+" command(s) succeeded, "+strconv.Itoa(failed)+" failed.")

	redirect := "/dashboard/server/" + guildID + "?resync=" + strconv.Itoa(ok)
	if unauthorized > 0 {
		redirect += "&sync_warn=unauthorized"
	} else if failed > 0 {
		redirect += "&sync_warn=error"
	}
	c.Redirect(http.StatusFound, redirect)
}

// cleanIDs trims and drops empty values from a form-submitted slice of IDs.
func cleanIDs(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// sessionFromCookie looks up the session identified by the "session_id" cookie.
// Returns nil if the cookie is missing or the session has expired.
func (s *Server) sessionFromCookie(c *gin.Context) *Session {
	id, err := c.Cookie("session_id")
	if err != nil {
		return nil
	}
	sess, ok := s.store.Get(id)
	if !ok {
		return nil
	}
	return sess
}

// findGuild searches a slice for a guild with a given ID.
func findGuild(guilds []Guild, id string) *Guild {
	for i := range guilds {
		if guilds[i].ID == id {
			return &guilds[i]
		}
	}
	return nil
}

// requireGuildAccess resolves the session's entry for guildID and applies the
// role gate. Returns nil after writing a 403 if access is denied. When
// adminOnly is true, moderators are also rejected — use it on destructive
// routes (leave-server, module enablement, edit moderator-role list).
func requireGuildAccess(c *gin.Context, sess *Session, guildID string, adminOnly bool) *Guild {
	g := findGuild(sess.Guilds, guildID)
	if g == nil {
		c.String(http.StatusForbidden, "You don't have access to that server.")
		return nil
	}
	if adminOnly && g.Role != RoleAdmin {
		c.String(http.StatusForbidden, "Admin access required.")
		return nil
	}
	return g
}

// botGuildSet returns the set of guild IDs the bot is currently a member of,
// read live from the session state so it's always up to date.
func (s *Server) botGuildSet() map[string]bool {
	guilds := s.bot.Session().State.Guilds
	set := make(map[string]bool, len(guilds))
	for _, g := range guilds {
		set[g.ID] = true
	}
	return set
}

// buildVisibleGuilds turns a raw /users/@me/guilds payload into the dashboard
// guild list, applying the same admin / moderator rules used at login time
// but against live bot state. Called at OAuth callback AND on every
// /dashboard render — recomputing each time keeps the list correct when the
// bot wasn't ready at login or has since reconnected/joined/left a guild.
//
// For non-admin guilds, this issues one GuildMember API call per moderator-
// configured guild the user is in. That's bounded (only guilds where an
// admin has set up moderator roles) and only runs on the /dashboard handler.
func (s *Server) buildVisibleGuilds(raw []discordGuild, userID string) []Guild {
	botGuilds := s.botGuildSet()
	modConfigured := s.bot.ModeratorRoles().GuildsConfigured()

	out := make([]Guild, 0, len(raw))
	for _, g := range raw {
		switch {
		case isAdminGuild(g):
			out = append(out, Guild{
				ID:         g.ID,
				Name:       g.Name,
				IconURL:    guildIconURL(g.ID, g.Icon),
				BotPresent: botGuilds[g.ID],
				Role:       RoleAdmin,
				IsOwner:    g.Owner,
			})
		case botGuilds[g.ID] && modConfigured[g.ID]:
			mem, err := s.bot.Session().GuildMember(g.ID, userID)
			if err != nil {
				log.Printf("[web] GuildMember %s/%s: %v", g.ID, userID, err)
				continue
			}
			if !s.bot.ModeratorRoles().HasAny(g.ID, mem.Roles) {
				continue
			}
			out = append(out, Guild{
				ID:         g.ID,
				Name:       g.Name,
				IconURL:    guildIconURL(g.ID, g.Icon),
				BotPresent: true,
				Role:       RoleModerator,
			})
		}
	}
	return out
}

// handleRolesPage renders the admin page for managing self-assignable roles.
func (s *Server) handleRolesPage(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	src := requireGuildAccess(c, sess, guildID, false)
	if src == nil {
		return
	}
	guild := *src
	guild.BotPresent = s.botGuildSet()[guildID]

	// Fetch all Discord roles for this guild (for the "use existing" dropdown).
	discordRoles, err := s.bot.Session().GuildRoles(guildID)
	if err != nil {
		log.Printf("[web] GuildRoles %s: %v", guildID, err)
		c.String(http.StatusInternalServerError, "Could not fetch guild roles.")
		return
	}

	// Exclude @everyone and bot-managed roles from the picker.
	type DiscordRoleOption struct {
		ID       string
		Name     string
		ColorHex string
	}
	var roleOptions []DiscordRoleOption
	for _, r := range discordRoles {
		if r.Name == "@everyone" || r.Managed {
			continue
		}
		roleOptions = append(roleOptions, DiscordRoleOption{
			ID:       r.ID,
			Name:     r.Name,
			ColorHex: roleColorHex(r.Color),
		})
	}

	// Build view rows for already-configured assignable roles.
	type RoleRow struct {
		RoleID      string
		Name        string
		Description string
		JoinMessage string
		ColorHex    string
	}
	var assignedRows []RoleRow
	if mod, ok := s.bot.Modules()["roles"]; ok {
		if rm, ok := mod.(*roles.Module); ok {
			for _, r := range rm.GuildRoles(guildID) {
				assignedRows = append(assignedRows, RoleRow{
					RoleID:      r.RoleID,
					Name:        r.Name,
					Description: r.Description,
					JoinMessage: r.JoinMessage,
					ColorHex:    roleColorHex(r.Color),
				})
			}
		}
	}

	if err := s.tmpl.ExecuteTemplate(c.Writer, "roles.html", gin.H{
		"User":          sess,
		"Guild":         &guild,
		"AssignedRoles": assignedRows,
		"DiscordRoles":  roleOptions,
		"ClientID":      s.cfg.ClientID,
		"Error":         c.Query("error"),
	}); err != nil {
		log.Printf("[web] roles template error: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleAddRole adds (or creates) a self-assignable role for a guild.
func (s *Server) handleAddRole(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	source := c.PostForm("source") // "existing" or "new"
	description := strings.TrimSpace(c.PostForm("description"))
	joinMsg := strings.TrimSpace(c.PostForm("join_message"))

	var roleID, roleName string
	var color int

	switch source {
	case "new":
		name := strings.TrimSpace(c.PostForm("new_name"))
		if name == "" {
			c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/roles?error=name_required")
			return
		}
		colorInt := parseHTMLColor(c.PostForm("new_color"))
		params := discordRoleParams(name, colorInt)
		created, err := s.bot.Session().GuildRoleCreate(guildID, &params)
		if err != nil {
			log.Printf("[web] GuildRoleCreate %s: %v", guildID, err)
			c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/roles?error=create_failed")
			return
		}
		roleID = created.ID
		roleName = created.Name
		color = colorInt

	default: // "existing"
		roleID = strings.TrimSpace(c.PostForm("role_id"))
		if roleID == "" {
			c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/roles?error=role_required")
			return
		}
		discordRoles, _ := s.bot.Session().GuildRoles(guildID)
		for _, r := range discordRoles {
			if r.ID == roleID {
				roleName = r.Name
				color = r.Color
				break
			}
		}
		if roleName == "" {
			c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/roles?error=role_not_found")
			return
		}
	}

	if mod, ok := s.bot.Modules()["roles"]; ok {
		if rm, ok := mod.(*roles.Module); ok {
			rm.AddRole(guildID, roles.AssignableRole{
				RoleID:      roleID,
				Name:        roleName,
				Description: description,
				JoinMessage: joinMsg,
				Color:       color,
			})
		}
	}

	source_label := "existing"
	if source == "new" {
		source_label = "new role created"
	}
	s.postAudit(guildID, sess, "Self-assignable role added",
		"Added <@&"+roleID+"> **"+roleName+"** to the self-assignable list ("+source_label+").")

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/roles")
}

// handleDeleteRole removes a self-assignable role from a guild's config.
// It does NOT delete the Discord role itself.
func (s *Server) handleDeleteRole(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	roleID := c.Param("roleID")
	// Look up the friendly name BEFORE removing so the audit message can
	// show it instead of the bare ID.
	var removedName string
	if mod, ok := s.bot.Modules()["roles"]; ok {
		if rm, ok := mod.(*roles.Module); ok {
			for _, r := range rm.GuildRoles(guildID) {
				if r.RoleID == roleID {
					removedName = r.Name
					break
				}
			}
			rm.RemoveRole(guildID, roleID)
		}
	}

	suffix := ""
	if removedName != "" {
		suffix = " **" + removedName + "**"
	}
	s.postAudit(guildID, sess, "Self-assignable role removed",
		"Removed <@&"+roleID+">"+suffix+" from the self-assignable list.")

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/roles")
}

// randomState produces a URL-safe random string used as OAuth2 CSRF state.
// crypto/rand (not math/rand) is essential here — math/rand is predictable.
func randomState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// handleBirthdaySettingsPage renders the birthday settings page.
func (s *Server) handleBirthdaySettingsPage(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	src := requireGuildAccess(c, sess, guildID, false)
	if src == nil {
		return
	}
	guild := *src
	guild.BotPresent = s.botGuildSet()[guildID]

	var announcementChannelID string
	gifsEnabled := true

	type BirthdayRow struct {
		UserID    string
		Username  string
		AvatarURL string
		Date      string
	}
	var birthdayRows []BirthdayRow

	type GIFItem struct {
		Filename string
		URL      string
	}
	var gifItems []GIFItem

	if mod, ok := s.bot.Modules()["birthday"]; ok {
		if bm, ok := mod.(*birthday.Module); ok {
			announcementChannelID = bm.AnnouncementChannel(guildID)
			gifsEnabled = bm.GIFsEnabled(guildID)

			// Collect GIFs from the guild's library.
			gifDir := bm.GuildGIFsDir(guildID)
			if des, err := os.ReadDir(gifDir); err == nil {
				for _, de := range des {
					if !de.IsDir() && strings.HasSuffix(strings.ToLower(de.Name()), ".gif") {
						gifItems = append(gifItems, GIFItem{
							Filename: de.Name(),
							URL:      "/gifs/" + guildID + "/" + de.Name(),
						})
					}
				}
			}

			// Filter global entries to members of this guild only.
			for _, e := range bm.AllEntries() {
				mem, err := s.bot.Session().GuildMember(guildID, e.UserID)
				if err != nil {
					continue
				}
				birthdayRows = append(birthdayRows, BirthdayRow{
					UserID:    e.UserID,
					Username:  mem.DisplayName(),
					AvatarURL: memberAvatarURL(guildID, mem),
					Date:      fmt.Sprintf("%s %d", time.Month(e.Month), e.Day),
				})
			}
		}
	}

	// Fetch text channels for the announcement channel picker.
	var channels []ChannelOption
	if guild.BotPresent {
		if gchans, err := s.bot.Session().GuildChannels(guildID); err == nil {
			for _, ch := range gchans {
				if ch.Type == discordgo.ChannelTypeGuildText {
					channels = append(channels, ChannelOption{ID: ch.ID, Name: ch.Name})
				}
			}
		}
	}

	errMsg := ""
	switch c.Query("error") {
	case "notgif":
		errMsg = "Only .gif files are accepted."
	case "toobig":
		errMsg = "File must be under 8 MB."
	case "invalid":
		errMsg = "The file doesn't appear to be a valid GIF."
	case "wish_failed":
		errMsg = "Failed to send the birthday wish — check the bot log. Make sure an announcement channel is configured."
	case "birthday_unavailable":
		errMsg = "The birthday module isn't loaded."
	}

	if err := s.tmpl.ExecuteTemplate(c.Writer, "birthday-settings.html", gin.H{
		"User":                  sess,
		"Guild":                 &guild,
		"Saved":                 c.Query("saved") == "1",
		"Wished":                c.Query("wished") == "1",
		"Error":                 errMsg,
		"BirthdayRows":          birthdayRows,
		"Channels":              channels,
		"AnnouncementChannelID": announcementChannelID,
		"GIFsEnabled":           gifsEnabled,
		"GIFItems":              gifItems,
	}); err != nil {
		log.Printf("[web] birthday-settings template error: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleUpdateBirthdaySettings saves the announcement channel and GIF toggle.
func (s *Server) handleUpdateBirthdaySettings(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	newCh := strings.TrimSpace(c.PostForm("channel_id"))
	newGIFs := c.PostForm("gifs_enabled") == "1"
	var beforeCh string
	var beforeGIFs bool
	if mod, ok := s.bot.Modules()["birthday"]; ok {
		if bm, ok := mod.(*birthday.Module); ok {
			beforeCh = bm.AnnouncementChannel(guildID)
			beforeGIFs = bm.GIFsEnabled(guildID)
			bm.SetAnnouncementChannel(guildID, newCh)
			bm.SetGIFsEnabled(guildID, newGIFs)
		}
	}

	// Emit only the fields that actually changed.
	var lines []string
	if beforeCh != newCh {
		switch {
		case beforeCh == "":
			lines = append(lines, "• Announcement channel: set to <#"+newCh+">")
		case newCh == "":
			lines = append(lines, "• Announcement channel: cleared (was <#"+beforeCh+">)")
		default:
			lines = append(lines, "• Announcement channel: <#"+beforeCh+"> → <#"+newCh+">")
		}
	}
	if beforeGIFs != newGIFs {
		from, to := "disabled", "enabled"
		if beforeGIFs {
			from, to = "enabled", "disabled"
		}
		lines = append(lines, "• GIFs: "+from+" → "+to)
	}
	if len(lines) > 0 {
		s.postAudit(guildID, sess, "Birthday settings updated", strings.Join(lines, "\n"))
	}

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/birthday?saved=1")
}

// handleUploadGIF accepts a multipart GIF upload and stores it in the guild's library.
func (s *Server) handleUploadGIF(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	file, err := c.FormFile("gif")
	if err != nil {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/birthday?error=notgif")
		return
	}

	if !strings.HasSuffix(strings.ToLower(file.Filename), ".gif") {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/birthday?error=notgif")
		return
	}

	const maxSize = 8 << 20 // 8 MB
	if file.Size > maxSize {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/birthday?error=toobig")
		return
	}

	// Validate GIF magic bytes.
	f, err := file.Open()
	if err != nil {
		c.String(http.StatusInternalServerError, "Failed to open upload.")
		return
	}
	header := make([]byte, 6)
	_, err = io.ReadFull(f, header)
	f.Close()
	if err != nil || (string(header) != "GIF89a" && string(header) != "GIF87a") {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/birthday?error=invalid")
		return
	}

	safeName := sanitizeGIFFilename(file.Filename)
	dir := filepath.Join(s.cfg.GIFsDir, guildID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		c.String(http.StatusInternalServerError, "Failed to create directory.")
		return
	}

	if err := c.SaveUploadedFile(file, filepath.Join(dir, safeName)); err != nil {
		c.String(http.StatusInternalServerError, "Failed to save file.")
		return
	}

	s.postAudit(guildID, sess, "Birthday GIF uploaded",
		"Added **"+safeName+"** to the birthday GIF library.")

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/birthday?saved=1")
}

// handleSendBirthdayWish manually fires today's birthday wish for one user.
// Useful when the auto-fire happened with no GIF (e.g. before a config fix)
// and an admin wants to re-send it. Marks the user as wished today so the
// daily check won't double-fire later in the day.
func (s *Server) handleSendBirthdayWish(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	userID := c.Param("userID")
	redirect := "/dashboard/server/" + guildID + "/birthday"

	mod, ok := s.bot.Modules()["birthday"]
	if !ok {
		c.Redirect(http.StatusFound, redirect+"?error=birthday_unavailable")
		return
	}
	bm, ok := mod.(*birthday.Module)
	if !ok {
		c.Redirect(http.StatusFound, redirect+"?error=birthday_unavailable")
		return
	}

	if err := bm.SendBirthdayWish(s.bot.Session(), guildID, userID); err != nil {
		log.Printf("[web] SendBirthdayWish %s/%s: %v", guildID, userID, err)
		c.Redirect(http.StatusFound, redirect+"?error=wish_failed")
		return
	}

	c.Redirect(http.StatusFound, redirect+"?wished=1")
}

// handleDeleteGIF removes a GIF from the guild's library.
func (s *Server) handleDeleteGIF(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	safeName := filepath.Base(c.Param("filename"))
	if !strings.HasSuffix(strings.ToLower(safeName), ".gif") {
		c.String(http.StatusBadRequest, "Invalid filename.")
		return
	}

	_ = os.Remove(filepath.Join(s.cfg.GIFsDir, guildID, safeName))

	s.postAudit(guildID, sess, "Birthday GIF removed",
		"Removed **"+safeName+"** from the birthday GIF library.")

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/birthday?saved=1")
}

// sanitizeGIFFilename strips unsafe characters from an uploaded filename.
func sanitizeGIFFilename(name string) string {
	base := filepath.Base(name)
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if !strings.HasSuffix(strings.ToLower(s), ".gif") {
		s += ".gif"
	}
	return s
}

// ChannelOption is a text channel shown in the create-raid channel picker.
type ChannelOption struct {
	ID   string
	Name string
}

// handleRaidsPage renders the raid calendar page for a guild.
func (s *Server) handleRaidsPage(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	src := requireGuildAccess(c, sess, guildID, false)
	if src == nil {
		return
	}
	guild := *src
	guild.BotPresent = s.botGuildSet()[guildID]

	var raids []raid.RaidView
	var pingRoleID string
	var templates []raid.RaidTemplate
	if mod, ok := s.bot.Modules()["raid"]; ok {
		if rm, ok := mod.(*raid.Module); ok {
			raids = rm.GuildRaids(guildID)
			pingRoleID = rm.PingRoleID(guildID)
			templates = rm.GuildTemplates(guildID)
		}
	}

	// Compact JSON used by the calendar JS to place raid chips on dates.
	type calRaid struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		UnixTime int64  `json:"unixTime"`
		Closed   bool   `json:"closed"`
	}
	calRaids := make([]calRaid, len(raids))
	for i, r := range raids {
		calRaids[i] = calRaid{ID: r.ID, Title: r.Title, UnixTime: r.UnixTime, Closed: r.Closed}
	}
	raidsJSON, _ := json.Marshal(calRaids)

	// Fetch text channels for the create-raid channel picker.
	var channels []ChannelOption
	if guild.BotPresent {
		if gchans, err := s.bot.Session().GuildChannels(guildID); err == nil {
			for _, ch := range gchans {
				if ch.Type == discordgo.ChannelTypeGuildText {
					channels = append(channels, ChannelOption{ID: ch.ID, Name: ch.Name})
				}
			}
		}
	}

	// Role picker for the ping-role admin section. JSON-encoded for the
	// chip multi-select widget (window.PRISHE_ROLES) — same shape used on
	// the server page.
	type RoleOption struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		ColorHex string `json:"color"`
	}
	var roleOptions []RoleOption
	if guild.BotPresent {
		if discordRoles, err := s.bot.Session().GuildRoles(guildID); err == nil {
			for _, r := range discordRoles {
				if r.Managed || r.ID == guildID { // skip integration-managed and @everyone
					continue
				}
				roleOptions = append(roleOptions, RoleOption{
					ID: r.ID, Name: r.Name, ColorHex: roleColorHex(r.Color),
				})
			}
			sort.SliceStable(roleOptions, func(i, j int) bool {
				return strings.ToLower(roleOptions[i].Name) < strings.ToLower(roleOptions[j].Name)
			})
		}
	}
	rolesJSON, _ := json.Marshal(roleOptions)

	if err := s.tmpl.ExecuteTemplate(c.Writer, "raids.html", gin.H{
		"User":       sess,
		"Guild":      &guild,
		"IsAdmin":    guild.Role == RoleAdmin,
		"Raids":      raids,
		"RaidsJSON":  template.JS(raidsJSON),
		"Channels":   channels,
		"RolesJSON":  template.JS(rolesJSON),
		"PingRoleID": pingRoleID, // pre-fills the create-raid multi-select
		"Templates":  templates,
		"PingSaved":  c.Query("ping_saved") == "1",
		"Created":    c.Query("created") == "1",
		"ErrMsg":     c.Query("error"),
	}); err != nil {
		log.Printf("[web] raids template error: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleCloseRaidWeb closes a raid via the web dashboard and redirects back.
func (s *Server) handleCloseRaidWeb(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	raidID := c.Param("raidID")
	if mod, ok := s.bot.Modules()["raid"]; ok {
		if rm, ok := mod.(*raid.Module); ok {
			// Pass the session so the Discord message gets edited to the
			// closed state, not just the local store.
			rm.CloseRaid(s.bot.Session(), guildID, raidID)
		}
	}

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids")
}

// handleCreateRaidWeb creates a raid from the web dashboard and posts its embed
// to the chosen Discord channel.
func (s *Server) handleCreateRaidWeb(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, false) == nil {
		return
	}

	title := strings.TrimSpace(c.PostForm("title"))
	if title == "" {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids?error=title_required")
		return
	}
	description := strings.TrimSpace(c.PostForm("description"))
	channelID := strings.TrimSpace(c.PostForm("channel_id"))
	if channelID == "" {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids?error=channel_required")
		return
	}

	// datetime-local value: "2026-04-30T20:00" — treat as UTC.
	var unixTime int64
	if dt := strings.TrimSpace(c.PostForm("datetime")); dt != "" {
		if t, err := time.Parse("2006-01-02T15:04", dt); err == nil {
			unixTime = t.Unix()
		}
	}

	// Per-raid ping list. The chip multi-select renders one hidden input
	// per selected role; the form may include zero or many of them. Default
	// pre-fill is the guild's configured ping role, but the creator can
	// add or remove freely for this specific raid.
	pingRoleIDs := cleanIDs(c.PostFormArray("ping_roles"))

	// Composition template — "standard" / "" both resolve to the built-in
	// 2T/2H/2M/1R/1C layout inside the module.
	templateID := strings.TrimSpace(c.PostForm("template"))

	if mod, ok := s.bot.Modules()["raid"]; ok {
		if rm, ok := mod.(*raid.Module); ok {
			if err := rm.CreateRaidFromWeb(s.bot.Session(), guildID, channelID, title, description, unixTime, pingRoleIDs, templateID); err != nil {
				log.Printf("[web] CreateRaidFromWeb: %v", err)
				c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids?error=post_failed")
				return
			}
		}
	}

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids?created=1")
}

// handleRaidTemplatesPage renders the admin-only page for managing custom
// raid composition templates. The "standard" template is always shown at the
// top as read-only — it can't be edited or deleted.
func (s *Server) handleRaidTemplatesPage(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	src := requireGuildAccess(c, sess, guildID, true)
	if src == nil {
		return
	}
	guild := *src
	guild.BotPresent = s.botGuildSet()[guildID]

	rm, _ := s.bot.Modules()["raid"].(*raid.Module)
	var templates []raid.RaidTemplate
	if rm != nil {
		templates = rm.GuildTemplates(guildID)
	}

	// JSON payloads consumed by the client-side renderer:
	//   - templates → chip lists on each saved-template card
	//   - kind options → the <select> in the dynamic slot-row form, with
	//     specific jobs grouped under an "optgroup" for legibility.
	templatesJSON, _ := json.Marshal(templates)
	type kindOpt struct {
		Value    string    `json:"value,omitempty"`
		Label    string    `json:"label,omitempty"`
		Group    string    `json:"group,omitempty"`
		Children []kindOpt `json:"children,omitempty"`
	}
	roleLabels := map[string]string{
		"tank":   "🛡️ Tank",
		"healer": "💚 Healer",
		"melee":  "⚔️ Melee DPS",
		"ranged": "🏹 Ranged DPS",
		"caster": "🔮 Caster DPS",
	}
	opts := []kindOpt{
		{Value: "tank", Label: roleLabels["tank"]},
		{Value: "healer", Label: roleLabels["healer"]},
		{Value: "melee", Label: roleLabels["melee"]},
		{Value: "ranged", Label: roleLabels["ranged"]},
		{Value: "caster", Label: roleLabels["caster"]},
		{Value: "dps", Label: "⚔️🏹🔮 Any DPS"},
		{Value: "any", Label: "🌐 Any role"},
	}
	// Specific-job options, grouped by role for the optgroup.
	for _, role := range []string{"tank", "healer", "melee", "ranged", "caster"} {
		g := kindOpt{Group: roleLabels[role] + " jobs"}
		for _, j := range raid.JobsForRole(role) {
			g.Children = append(g.Children, kindOpt{Value: j.Key, Label: j.Name})
		}
		opts = append(opts, g)
	}
	kindOptsJSON, _ := json.Marshal(opts)

	if err := s.tmpl.ExecuteTemplate(c.Writer, "raid-templates.html", gin.H{
		"User":                sess,
		"Guild":               &guild,
		"Templates":           templates,
		"TemplatesJSON":       template.JS(templatesJSON),
		"SlotKindOptionsJSON": template.JS(kindOptsJSON),
		"Saved":               c.Query("saved") == "1",
		"Deleted":             c.Query("deleted") == "1",
		"ErrMsg":              c.Query("error"),
	}); err != nil {
		log.Printf("[web] raid-templates template error: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleCreateRaidTemplate accepts a new custom template from the form.
// Admin-only — same trust level as the rest of the raid settings UI.
func (s *Server) handleCreateRaidTemplate(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, true) == nil {
		return
	}

	rm, ok := s.bot.Modules()["raid"].(*raid.Module)
	if !ok {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids/templates?error=raid_unavailable")
		return
	}

	t := raid.RaidTemplate{
		Name:   strings.TrimSpace(c.PostForm("name")),
		Counts: map[string]int{},
	}
	// The form posts one (kind, count) pair per slot row via parallel arrays
	// named "kinds" and "counts". We zip them by index — the module then
	// drops unknown keys, zero/negative counts, and rejects empty templates.
	kinds := c.PostFormArray("kinds")
	counts := c.PostFormArray("counts")
	for i, k := range kinds {
		k = strings.TrimSpace(k)
		if k == "" || i >= len(counts) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(counts[i]))
		if err != nil || n <= 0 {
			continue
		}
		// Summing accumulates duplicate rows (e.g. two "any DPS" rows = total).
		t.Counts[k] += n
	}
	if err := rm.AddTemplate(guildID, t); err != nil {
		log.Printf("[web] AddTemplate: %v", err)
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids/templates?error="+err.Error())
		return
	}

	total := 0
	for _, n := range t.Counts {
		total += n
	}
	s.postAudit(guildID, sess, "Raid template added",
		"Added **"+t.Name+"** ("+strconv.Itoa(total)+" slots).")

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids/templates?saved=1")
}

// handleDeleteRaidTemplate removes a custom template. Cannot remove the
// built-in standard template; the module returns an error in that case.
func (s *Server) handleDeleteRaidTemplate(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, true) == nil {
		return
	}

	rm, ok := s.bot.Modules()["raid"].(*raid.Module)
	if !ok {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids/templates?error=raid_unavailable")
		return
	}

	tid := c.Param("tid")
	if err := rm.DeleteTemplate(guildID, tid); err != nil {
		log.Printf("[web] DeleteTemplate: %v", err)
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids/templates?error="+err.Error())
		return
	}

	s.postAudit(guildID, sess, "Raid template removed",
		"Removed template `"+tid+"`.")

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids/templates?deleted=1")
}

// handleUpdateRaidPingRole stores the per-guild raid ping role. Admin-only —
// the same trust level as the rest of the role-permission UI.
func (s *Server) handleUpdateRaidPingRole(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if requireGuildAccess(c, sess, guildID, true) == nil {
		return
	}

	rm, ok := s.bot.Modules()["raid"].(*raid.Module)
	if !ok {
		c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids?error=raid_unavailable")
		return
	}

	// Field is wired through the chip multi-select with data-max="1" —
	// c.PostForm returns the empty string when no role is picked, which
	// clears the setting (i.e. disables raid pings for this guild).
	//
	// We use c.PostForm here (Gin's helper) rather than c.Request.PostForm
	// because the latter is nil until ParseForm runs, and silently returning
	// "" from a nil url.Values is how the previous version of this handler
	// always-cleared the role on save.
	before := rm.PingRoleID(guildID)
	after := strings.TrimSpace(c.PostForm("raid_ping_role"))
	rm.SetPingRoleID(guildID, after)

	if before != after {
		var line string
		switch {
		case before == "":
			line = "Set to <@&" + after + ">"
		case after == "":
			line = "Cleared (was <@&" + before + ">) — raid pings disabled"
		default:
			line = "<@&" + before + "> → <@&" + after + ">"
		}
		s.postAudit(guildID, sess, "Raid ping role updated", line)
	}

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/raids?ping_saved=1")
}

// memberAvatarURL returns the best available avatar URL for a guild member.
// Prefers the guild-specific avatar, falls back to the global user avatar,
// and returns an empty string if neither is set.
func memberAvatarURL(guildID string, m *discordgo.Member) string {
	if m.Avatar != "" {
		ext := "png"
		if strings.HasPrefix(m.Avatar, "a_") {
			ext = "gif"
		}
		return fmt.Sprintf("https://cdn.discordapp.com/guilds/%s/users/%s/avatars/%s.%s?size=64", guildID, m.User.ID, m.Avatar, ext)
	}
	if m.User != nil && m.User.Avatar != "" {
		ext := "png"
		if strings.HasPrefix(m.User.Avatar, "a_") {
			ext = "gif"
		}
		return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.%s?size=64", m.User.ID, m.User.Avatar, ext)
	}
	return ""
}

func roleColorHex(c int) string {
	if c == 0 {
		return "#99aab5"
	}
	return fmt.Sprintf("#%06x", c)
}

func parseHTMLColor(s string) int {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return 0
	}
	n, err := strconv.ParseInt(s, 16, 32)
	if err != nil {
		return 0
	}
	return int(n)
}

// discordRoleParams builds a RoleParams value for GuildRoleCreate.
// Returned by value so we can take its address inline in a single expression.
func discordRoleParams(name string, color int) discordgo.RoleParams {
	return discordgo.RoleParams{Name: name, Color: &color}
}

// commandSignatures returns one formatted string per top-level invocation path
// for a slash command, e.g. "/raid create <title> [description]".
// Subcommand groups are flattened so each leaf produces its own line.
func commandSignatures(cmd *discordgo.ApplicationCommand) []string {
	return buildSigs("/"+cmd.Name, cmd.Options)
}

func buildSigs(prefix string, opts []*discordgo.ApplicationCommandOption) []string {
	// If the first option is a subcommand (or group), recurse rather than
	// treating them as positional parameters.
	if len(opts) > 0 &&
		(opts[0].Type == discordgo.ApplicationCommandOptionSubCommand ||
			opts[0].Type == discordgo.ApplicationCommandOptionSubCommandGroup) {
		var out []string
		for _, o := range opts {
			out = append(out, buildSigs(prefix+" "+o.Name, o.Options)...)
		}
		return out
	}

	// Leaf command — append required then optional parameters.
	var sb strings.Builder
	sb.WriteString(prefix)
	for _, o := range opts {
		if o.Required {
			fmt.Fprintf(&sb, " <%s>", o.Name)
		} else {
			fmt.Fprintf(&sb, " [%s]", o.Name)
		}
	}
	return []string{sb.String()}
}

