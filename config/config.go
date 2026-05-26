package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	BotToken      string // Discord bot token
	ClientID      string // Discord OAuth2 application client ID
	ClientSecret  string // Discord OAuth2 application client secret
	RedirectURI   string // OAuth2 callback URL (must be registered in Discord dev portal)
	Port          string // HTTP server port
	SecretKey     string // Used to sign session cookies
	SecureCookies bool   // Set true in production (requires HTTPS)

	// StorageDir is the base directory for every runtime data file the bot
	// writes. Set this to a mounted persistent volume in production so role
	// allow-lists, birthdays, raids, etc. survive redeploys. Defaults to
	// /data — matches Railway/Fly conventions; override per-file via the
	// individual *_FILE / *_DIR env vars when you need finer control.
	StorageDir string

	GIFsDir            string // Directory where per-server birthday GIFs are stored
	BirthdayDataFile   string // Path to birthday persistence file
	RolesDataFile      string // Path to roles persistence file
	RaidDataFile       string // Path to raid sign-up persistence file
	CommandPermsFile   string // Path to command role-lock persistence file
	ChannelPermsFile   string // Path to channel allow-list persistence file
	ModeratorRolesFile string // Path to dashboard-moderator role persistence file
	ModuleStateFile    string // Path to per-guild module enable/disable persistence file
	IconsDir           string // Directory where downloaded FF14 icons are stored
	BaseURL            string // Public URL of the web server, used to build icon URLs (e.g. https://mybot.railway.app)

	// BotDescription is the "About Me" text that appears on the bot's Discord
	// profile. Applied via the Application API on every start so it stays in
	// sync with what's deployed — useful for displaying environment hints
	// (e.g. dev vs prod) right on the profile card.
	BotDescription string
}

// Load reads config from a .env file (if present) and then from environment
// variables. Environment variables always win over .env values.
//
// We call log.Fatalf for required variables so the app crashes loudly at
// startup rather than silently misbehaving at runtime.
func Load() *Config {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Only log if the file exists but couldn't be parsed; missing .env is fine.
		if _, statErr := os.Stat(".env"); statErr == nil {
			log.Printf("warning: error loading .env file: %v", err)
		}
	}

	// Resolve the storage base. Default /data matches the standard mount
	// point for a Railway/Fly volume. We create it eagerly so the first
	// write doesn't trip on a missing directory — a failure here is logged
	// but not fatal, because individual *_FILE overrides might point
	// elsewhere and still work.
	storageDir := getOrDefault("STORAGE_DIR", "/data")
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		log.Printf("warning: could not create storage dir %q: %v — per-file writes may fail unless you override each *_FILE env var", storageDir, err)
	}

	// dataPath returns the explicit env override if set, otherwise a path
	// under StorageDir. Keeps the per-file overrides backward-compatible.
	dataPath := func(envKey, filename string) string {
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		return filepath.Join(storageDir, filename)
	}

	return &Config{
		BotToken:      mustGet("DISCORD_BOT_TOKEN"),
		ClientID:      mustGet("DISCORD_CLIENT_ID"),
		ClientSecret:  mustGet("DISCORD_CLIENT_SECRET"),
		RedirectURI:   getOrDefault("DISCORD_REDIRECT_URI", "http://localhost:8080/auth/callback"),
		Port:          getOrDefault("PORT", "8080"),
		SecretKey:     loadSecretKey(),
		SecureCookies: os.Getenv("SECURE_COOKIES") == "true",

		StorageDir:         storageDir,
		GIFsDir:            dataPath("GIFS_DIR", "gifs"),
		BirthdayDataFile:   dataPath("BIRTHDAY_DATA_FILE", "birthdays.json"),
		RolesDataFile:      dataPath("ROLES_DATA_FILE", "roles.json"),
		RaidDataFile:       dataPath("RAID_DATA_FILE", "raids.json"),
		CommandPermsFile:   dataPath("COMMAND_PERMS_FILE", "command-permissions.json"),
		ChannelPermsFile:   dataPath("CHANNEL_PERMS_FILE", "channel-permissions.json"),
		ModeratorRolesFile: dataPath("MODERATOR_ROLES_FILE", "moderator-roles.json"),
		ModuleStateFile:    dataPath("MODULE_STATE_FILE", "module-state.json"),
		IconsDir:           getOrDefault("ICONS_DIR", filepath.Join("web", "static", "icons", "ffxiv")),
		BaseURL:            os.Getenv("BASE_URL"), // empty = icons not served; Discord embeds use emoji only

		BotDescription: getOrDefault("BOT_DESCRIPTION", "I'm currently in development, bear with me :3"),
	}
}

// loadSecretKey returns the SECRET_KEY env var, or generates a random one with
// a loud warning. A random key means sessions won't survive restarts, but it's
// safer than a hardcoded default.
func loadSecretKey() string {
	if key := os.Getenv("SECRET_KEY"); key != "" {
		return key
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate fallback secret key: %v", err)
	}
	key := hex.EncodeToString(b)
	log.Println("WARNING: SECRET_KEY is not set. A random key has been generated — sessions will not survive restarts. Set SECRET_KEY in your environment for production use.")
	return key
}

func mustGet(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %q is not set", key)
	}
	return v
}

func getOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
