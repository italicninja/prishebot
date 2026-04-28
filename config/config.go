package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"

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
	BirthdayDataFile string // Path to birthday persistence file (default: birthdays.json)
	RolesDataFile    string // Path to roles persistence file (default: roles.json)
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

	return &Config{
		BotToken:      mustGet("DISCORD_BOT_TOKEN"),
		ClientID:      mustGet("DISCORD_CLIENT_ID"),
		ClientSecret:  mustGet("DISCORD_CLIENT_SECRET"),
		RedirectURI:   getOrDefault("DISCORD_REDIRECT_URI", "http://localhost:8080/auth/callback"),
		Port:          getOrDefault("PORT", "8080"),
		SecretKey:     loadSecretKey(),
		SecureCookies: os.Getenv("SECURE_COOKIES") == "true",
		BirthdayDataFile: getOrDefault("BIRTHDAY_DATA_FILE", "birthdays.json"),
		RolesDataFile:    getOrDefault("ROLES_DATA_FILE", "roles.json"),
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
