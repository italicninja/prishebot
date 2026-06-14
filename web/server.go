package web

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"

	"github.com/user/discord-bot-skeleton/bot"
	"github.com/user/discord-bot-skeleton/config"
)

// Server bundles the HTTP server, its dependencies, and route configuration.
type Server struct {
	cfg     *config.Config
	bot     *bot.Bot
	store   *SessionStore
	oauth   *oauth2.Config
	tmpl    *template.Template
	r       *gin.Engine
	httpSrv *http.Server
}

// NewServer constructs and wires up the web server.
func NewServer(cfg *config.Config, b *bot.Bot) *Server {
	// Configure the Discord OAuth2 client.
	// Scopes:
	//   "identify"                                  - read username, avatar, discriminator
	//   "guilds"                                    - list the user's servers (and their permissions)
	//   "applications.commands.permissions.update"  - push per-guild slash-command
	//      visibility overrides on behalf of the logged-in admin, so the
	//      dashboard's role allow-list also controls who sees the command in
	//      Discord's slash menu (not just runtime enforcement)
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURI,
		Scopes:       []string{"identify", "guilds", "applications.commands.permissions.update"},
		Endpoint:     DiscordEndpoint,
	}

	srv := &Server{
		cfg:   cfg,
		bot:   b,
		store: NewSessionStore(),
		oauth: oauthCfg,
	}

	// Parse all HTML templates at startup from the embedded filesystem.
	//
	// template.Must panics if parsing fails. This is intentional: a broken
	// template is a programming error, and we want to catch it immediately on
	// start rather than serve a broken page to the first user who triggers it.
	srv.tmpl = template.Must(
		template.ParseFS(webFS, "templates/*.html"),
	)

	srv.r = srv.routes()
	return srv
}

// routes registers all HTTP routes and returns the configured Gin engine.
func (s *Server) routes() *gin.Engine {
	r := gin.Default()

	// Serve static assets from the embedded filesystem.
	staticFS, err := fs.Sub(webFS, "static")
	if err != nil {
		panic("failed to sub embedded static FS: " + err.Error())
	}
	r.StaticFS("/static", http.FS(staticFS))

	// Serve downloaded FF14 icons from the on-disk icons directory.
	// This avoids cross-origin loads to xivapi.com on every page view.
	// The directory is populated by running: go run ./scripts/download_icons
	if s.cfg.IconsDir != "" {
		r.Static("/icons", s.cfg.IconsDir)
	}

	// Serve per-server birthday GIFs so Discord can load them in embeds.
	if s.cfg.GIFsDir != "" {
		r.Static("/gifs", s.cfg.GIFsDir)
	}

	// ── Health ──────────────────────────────────────────────────────────────
	// Railway (and any load balancer) polls this path. It must respond 200
	// immediately - do not gate it behind module load or Discord connectivity.
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	// ── Public ──────────────────────────────────────────────────────────────
	r.GET("/", s.handleHome)
	r.GET("/login", s.handleLogin)
	r.GET("/auth/callback", s.handleCallback)
	r.GET("/logout", s.handleLogout)

	// ── Protected - requires a valid session ─────────────────────────────────
	// gin.RouterGroup lets us apply the requireAuth middleware to a set of
	// routes without repeating it on every handler.
	dash := r.Group("/dashboard")
	dash.Use(s.requireAuth)
	{
		dash.GET("", s.handleDashboard)
		dash.GET("/server/:id", s.handleServerPage)
		dash.POST("/server/:id/modules", s.handleUpdateModules)
		dash.POST("/server/:id/resync", s.handleResyncCommandPerms)
		dash.POST("/server/:id/leave", s.handleLeaveServer)
		dash.GET("/server/:id/roles", s.handleRolesPage)
		dash.POST("/server/:id/roles", s.handleAddRole)
		dash.POST("/server/:id/roles/:roleID/delete", s.handleDeleteRole)
		dash.GET("/server/:id/birthday", s.handleBirthdaySettingsPage)
		dash.POST("/server/:id/birthday", s.handleUpdateBirthdaySettings)
		dash.POST("/server/:id/birthday/gifs", s.handleUploadGIF)
		dash.POST("/server/:id/birthday/gifs/:filename/delete", s.handleDeleteGIF)
		dash.POST("/server/:id/birthday/wish/:userID", s.handleSendBirthdayWish)
		dash.GET("/server/:id/raids", s.handleRaidsPage)
		dash.POST("/server/:id/raids/create", s.handleCreateRaidWeb)
		dash.POST("/server/:id/raids/ping-role", s.handleUpdateRaidPingRole)
		dash.GET("/server/:id/raids/templates", s.handleRaidTemplatesPage)
		dash.POST("/server/:id/raids/templates", s.handleCreateRaidTemplate)
		dash.POST("/server/:id/raids/templates/:tid/delete", s.handleDeleteRaidTemplate)
		dash.POST("/server/:id/raids/:raidID/close", s.handleCloseRaidWeb)
		dash.POST("/server/:id/raids/:raidID/delete", s.handleDeleteRaidWeb)
		dash.GET("/server/:id/loganalyze", s.handleLogAnalyzePage)
	}

	return r
}

// Start runs the HTTP server. This call blocks until the server is shut down.
func (s *Server) Start() error {
	s.httpSrv = &http.Server{
		Addr:    ":" + s.cfg.Port,
		Handler: s.r,
	}
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully drains active connections within the given context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}
