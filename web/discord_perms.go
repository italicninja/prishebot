// Push Discord Application Command Permissions v2 overrides on behalf of the
// logged-in admin, so the role allow-list the admin set in the dashboard also
// controls who sees the command in Discord's slash menu - not just whether
// the bot will accept the invocation at runtime.
//
// Why a user token and not the bot token? Discord requires a *user* OAuth2
// token with the "applications.commands.permissions.update" scope for this
// endpoint, and the user must hold ManageGuild in the target guild. The
// dashboard already restricts the "save permissions" form to admins, who
// trivially satisfy that.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Discord application-command permission override type. We only ever push
// role-targeted overrides; user (2) and channel (3) overrides are unused.
const cmdPermTypeRole = 1

// cmdPermEntry is one allow/deny override pushed to Discord.
type cmdPermEntry struct {
	ID         string `json:"id"`
	Type       int    `json:"type"`
	Permission bool   `json:"permission"`
}

// cmdPermBody is the PUT body shape for the Discord permissions endpoint.
type cmdPermBody struct {
	Permissions []cmdPermEntry `json:"permissions"`
}

// syncCommandPermissions pushes the dashboard's role allow-list to Discord
// so the slash-menu visibility matches.
//
// Mapping rules:
//   - If allowedRoleIDs is empty, all overrides are cleared. Visibility then
//     falls back to the command's default_member_permissions (ManageGuild),
//     i.e. admins only.
//   - Otherwise each role ID is sent as an allow override. If the allow-list
//     contains the guild ID (== the @everyone role ID), that one entry opens
//     the command to all members on top of the admin-by-default rule.
//
// userToken is the OAuth2 bearer token of the admin saving the form. The
// session must have been authorized with the
// "applications.commands.permissions.update" scope - older sessions created
// before that scope was added will fail with 401 here. We surface that as a
// distinct error so callers can log a hint without panicking.
//
// 429 (rate-limited) responses are retried automatically, honouring Discord's
// retry_after hint, up to syncMaxRetries times. Saving a server's full
// permission set involves one PUT per command, which is enough to trip the
// per-route bucket; the retry keeps the whole batch reliable without making
// callers worry about pacing.
func syncCommandPermissions(ctx context.Context, userToken, appID, guildID, commandID string, allowedRoleIDs []string) error {
	if userToken == "" || appID == "" || guildID == "" || commandID == "" {
		return fmt.Errorf("syncCommandPermissions: missing required field")
	}

	body := cmdPermBody{Permissions: make([]cmdPermEntry, 0, len(allowedRoleIDs))}
	// Deduplicate role IDs while preserving order - Discord rejects duplicates.
	seen := make(map[string]struct{}, len(allowedRoleIDs))
	for _, id := range allowedRoleIDs {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		body.Permissions = append(body.Permissions, cmdPermEntry{
			ID:         id,
			Type:       cmdPermTypeRole,
			Permission: true,
		})
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal permissions: %w", err)
	}

	url := fmt.Sprintf("%s/applications/%s/guilds/%s/commands/%s/permissions",
		discordAPI, appID, guildID, commandID)

	client := &http.Client{Timeout: 10 * time.Second}

	for attempt := 0; attempt <= syncMaxRetries; attempt++ {
		// Build the request fresh each attempt - the body Reader is consumed on send.
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+userToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("PUT command permissions: %w", err)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			return nil
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests && attempt < syncMaxRetries {
			wait := parseRetryAfter(respBody)
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("discord 401 - admin's session lacks applications.commands.permissions.update scope (have them log out and back in): %s", respBody)
		}
		return fmt.Errorf("discord %d: %s", resp.StatusCode, respBody)
	}

	return fmt.Errorf("discord 429: gave up after %d retries", syncMaxRetries)
}

// syncMaxRetries caps how many times we'll back off on a 429 for a single
// command. Three is plenty even for a full guild resync: Discord's
// retry_after is typically 2–5 seconds and rate limits clear quickly.
const syncMaxRetries = 3

// parseRetryAfter extracts the retry_after hint (in seconds) from Discord's
// 429 body. Clamped to [200ms, 30s] so a bad response can't stall the whole
// request, and falls back to a sensible default if the body is missing/junk.
func parseRetryAfter(body []byte) time.Duration {
	var parsed struct {
		RetryAfter float64 `json:"retry_after"`
	}
	_ = json.Unmarshal(body, &parsed)
	wait := time.Duration(parsed.RetryAfter * float64(time.Second))
	if wait < 200*time.Millisecond {
		wait = 2 * time.Second
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}
