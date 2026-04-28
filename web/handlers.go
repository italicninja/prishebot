package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/gin-gonic/gin"

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
	rawGuilds, err := fetchAdminGuilds(c.Request.Context(), token, s.oauth)
	if err != nil {
		c.String(http.StatusInternalServerError, "Could not fetch guild list.")
		return
	}

	// 4. Build the session guild list, marking which ones already have the bot.
	botGuilds := make(map[string]bool)
	for _, g := range s.bot.Session().State.Guilds {
		botGuilds[g.ID] = true
	}

	var guilds []Guild
	for _, g := range rawGuilds {
		guilds = append(guilds, Guild{
			ID:         g.ID,
			Name:       g.Name,
			IconURL:    guildIconURL(g.ID, g.Icon),
			BotPresent: botGuilds[g.ID],
		})
	}

	// 5. Create session and set the cookie.
	sess := s.store.Create(user.ID, user.Username, userAvatarURL(user.ID, user.Avatar), token.AccessToken, guilds)
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
	botSet := s.botGuildSet()
	// Refresh BotPresent from live bot state — session data is stale after join/leave.
	guilds := make([]Guild, len(sess.Guilds))
	for i, g := range sess.Guilds {
		guilds[i] = g
		guilds[i].BotPresent = botSet[g.ID]
	}
	if err := s.tmpl.ExecuteTemplate(c.Writer, "dashboard.html", gin.H{
		"User":   sess,
		"Guilds": guilds,
	}); err != nil {
		log.Printf("[web] failed to render dashboard.html: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleServerPage renders the management page for a single server.
// It verifies that the logged-in user actually has admin in that server by
// checking their session guild list — we never trust the URL parameter alone.
func (s *Server) handleServerPage(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	src := findGuild(sess.Guilds, guildID)
	if src == nil {
		c.String(http.StatusForbidden, "You don't have admin access to that server.")
		return
	}
	// Copy and refresh BotPresent from live bot state.
	guild := *src
	guild.BotPresent = s.botGuildSet()[guildID]

	settings := s.bot.GuildModuleSettings(guildID)
	modules := s.bot.Modules()

	type ModuleRow struct {
		Name        string
		Description string
		Enabled     bool
	}
	var rows []ModuleRow
	for name, mod := range modules {
		rows = append(rows, ModuleRow{
			Name:        name,
			Description: mod.Description(),
			Enabled:     settings[name],
		})
	}

	if err := s.tmpl.ExecuteTemplate(c.Writer, "server.html", gin.H{
		"User":     sess,
		"Guild":    &guild,
		"Modules":  rows,
		"ClientID": s.cfg.ClientID,
	}); err != nil {
		log.Printf("[web] failed to render server.html: %v", err)
		c.Status(http.StatusInternalServerError)
	}
}

// handleLeaveServer makes the bot leave the guild, then redirects to the dashboard.
func (s *Server) handleLeaveServer(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if findGuild(sess.Guilds, guildID) == nil {
		c.String(http.StatusForbidden, "Access denied.")
		return
	}

	if err := s.bot.Session().GuildLeave(guildID); err != nil {
		log.Printf("[web] failed to leave guild %s: %v", guildID, err)
		c.String(http.StatusInternalServerError, "Failed to leave server.")
		return
	}

	c.Redirect(http.StatusFound, "/dashboard")
}

// handleUpdateModules processes the module toggle form on the server page.
func (s *Server) handleUpdateModules(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if findGuild(sess.Guilds, guildID) == nil {
		c.String(http.StatusForbidden, "Access denied.")
		return
	}

	if err := c.Request.ParseForm(); err != nil {
		c.String(http.StatusBadRequest, "Invalid form data.")
		return
	}

	// The HTML form sends a checkbox named "module_<name>" for each enabled
	// module. Modules whose checkboxes are unchecked simply don't appear in
	// the form — so any module *not* in the form data should be disabled.
	enabled := make(map[string]bool)
	for key := range c.Request.Form {
		if name, ok := strings.CutPrefix(key, "module_"); ok {
			enabled[name] = true
		}
	}

	// Only iterate known modules so form data can't set arbitrary module names.
	for name := range s.bot.Modules() {
		s.bot.SetModuleEnabled(guildID, name, enabled[name])
	}

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID)
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

// handleRolesPage renders the admin page for managing self-assignable roles.
func (s *Server) handleRolesPage(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	src := findGuild(sess.Guilds, guildID)
	if src == nil {
		c.String(http.StatusForbidden, "You don't have admin access to that server.")
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

	if findGuild(sess.Guilds, guildID) == nil {
		c.String(http.StatusForbidden, "Access denied.")
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

	c.Redirect(http.StatusFound, "/dashboard/server/"+guildID+"/roles")
}

// handleDeleteRole removes a self-assignable role from a guild's config.
// It does NOT delete the Discord role itself.
func (s *Server) handleDeleteRole(c *gin.Context) {
	sess := c.MustGet("session").(*Session)
	guildID := c.Param("id")

	if findGuild(sess.Guilds, guildID) == nil {
		c.String(http.StatusForbidden, "Access denied.")
		return
	}

	roleID := c.Param("roleID")
	if mod, ok := s.bot.Modules()["roles"]; ok {
		if rm, ok := mod.(*roles.Module); ok {
			rm.RemoveRole(guildID, roleID)
		}
	}

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
