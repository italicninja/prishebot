package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"

	"golang.org/x/oauth2"
)

const (
	discordAPI = "https://discord.com/api/v10"

	// permAdministrator is the Discord permission bit for ADMINISTRATOR.
	// Discord permissions are a 64-bit integer bitmask. Each bit represents
	// one permission. Bit 3 (value 0x8) is ADMINISTRATOR, which grants all
	// permissions and bypasses channel overrides.
	permAdministrator int64 = 0x8
)

// DiscordEndpoint is the OAuth2 endpoint for Discord.
// We define this manually rather than importing golang.org/x/oauth2/endpoints
// so the dependency is transparent.
var DiscordEndpoint = oauth2.Endpoint{
	AuthURL:  "https://discord.com/api/oauth2/authorize",
	TokenURL: "https://discord.com/api/oauth2/token",
}

// discordUser mirrors the shape of Discord's GET /users/@me response.
type discordUser struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Discriminator string `json:"discriminator"`
	Avatar        string `json:"avatar"`
}

// discordGuild mirrors the shape of Discord's GET /users/@me/guilds response.
// Note: the "permissions" field is a decimal integer sent as a JSON string.
type discordGuild struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Owner       bool   `json:"owner"`
	Permissions string `json:"permissions"`
}

// fetchCurrentUser calls the Discord API to get the logged-in user's profile.
// The oauth2.Config.Client method returns an http.Client that automatically
// injects the Bearer token on every request and handles token refresh.
func fetchCurrentUser(ctx context.Context, token *oauth2.Token, cfg *oauth2.Config) (*discordUser, error) {
	client := cfg.Client(ctx, token)
	resp, err := client.Get(discordAPI + "/users/@me")
	if err != nil {
		return nil, fmt.Errorf("GET /users/@me: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading user body: %w", err)
	}

	var u discordUser
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("parsing user JSON: %w", err)
	}
	return &u, nil
}

// fetchUserGuilds returns the user's full guild list from Discord.
//
// The partial guild objects include the user's computed permissions for that
// guild, so callers can check the admin bit directly without separate lookups.
func fetchUserGuilds(ctx context.Context, token *oauth2.Token, cfg *oauth2.Config) ([]discordGuild, error) {
	client := cfg.Client(ctx, token)
	resp, err := client.Get(discordAPI + "/users/@me/guilds")
	if err != nil {
		return nil, fmt.Errorf("GET /users/@me/guilds: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading guilds body: %w", err)
	}

	var all []discordGuild
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("parsing guilds JSON: %w", err)
	}
	return all, nil
}

// isAdminGuild reports whether the user is owner or has ADMINISTRATOR in g.
func isAdminGuild(g discordGuild) bool {
	return g.Owner || hasAdminBit(g.Permissions)
}

// hasAdminBit parses a Discord permissions decimal string and checks bit 3.
// We parse to int64 (not uint64) because Go's strconv.ParseInt is convenient
// and Discord's permissions fit within 53 bits, well inside int64 range.
func hasAdminBit(permStr string) bool {
	if permStr == "" {
		return false
	}
	perms, err := strconv.ParseInt(permStr, 10, 64)
	if err != nil {
		log.Printf("[web] hasAdminBit: failed to parse permissions %q: %v", permStr, err)
		return false
	}
	return perms&permAdministrator == permAdministrator
}

// guildIconURL builds the Discord CDN URL for a guild's icon image.
func guildIconURL(guildID, icon string) string {
	if icon == "" {
		return ""
	}
	return fmt.Sprintf("https://cdn.discordapp.com/icons/%s/%s.png?size=64", guildID, icon)
}

// userAvatarURL builds the Discord CDN URL for a user's avatar.
// If the user has no custom avatar, Discord assigns a default based on
// (userID >> 22) % 5, matching the new pomelo username system.
func userAvatarURL(userID, avatar string) string {
	if avatar == "" {
		idx := uint64(0)
		if id, err := strconv.ParseUint(userID, 10, 64); err == nil {
			idx = (id >> 22) % 5
		}
		return fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", idx)
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=64", userID, avatar)
}
