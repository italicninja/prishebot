# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Prishe is a modular Discord bot for FFXIV-flavored communities. It provides raid sign-ups, birthdays, role management, and announcements, with a browser-based admin dashboard. Written in Go using discordgo and Gin.

## Commands

```bash
# Run locally
go run .

# Build
go build -o prishe .

# Download FF14 icons (one-time setup)
go run ./scripts/download_icons

# Docker
docker build -t prishe .
docker run -e DISCORD_BOT_TOKEN=... -e DISCORD_CLIENT_ID=... -p 8080:8080 prishe
```

There is no test suite. Manual testing against a real Discord bot is the primary validation path.

## Architecture

Two processes run concurrently from `main.go`:
- **Discord bot** (`bot/`) — WebSocket gateway, module routing, slash commands
- **Web dashboard** (`web/`) — Gin HTTP server, Discord OAuth2, server-side HTML templates

The web server starts *before* bot module load intentionally so Railway's healthcheck passes early (`/health`).

### Module System

Every feature is a self-contained module implementing `bot.Module` ([bot/module.go](bot/module.go)):

```go
type Module interface {
    Name() string
    Description() string
    Category() Category
    Commands() []*discordgo.ApplicationCommand
    HandleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate)
    OnLoad(s *discordgo.Session) error
    OnUnload(s *discordgo.Session) error
}
```

Modules are registered in `main.go` via `b.LoadModule(...)`. The bot routes slash commands via a `cmdOwners` map (module name → module), and routes button/select/modal interactions by matching the custom ID prefix (e.g. `raid:join:...` → raid module).

To add a new module: create `bot/modules/yourname/yourname.go`, implement the interface, and add `b.LoadModule(yourname.New())` in `main.go`.

### Permission Layers

Three independent checks gate every command:
1. Per-guild module enabled/disabled toggle (`bot/module_state.go`)
2. Per-command role allow-list (`bot/permissions.go`) — defaults to admin-only
3. Per-module channel allow-list (`bot/channels.go`) — defaults to all channels

### Persistence

All runtime state is flat JSON files. Location defaults to `/data/` (Railway volume) and can be overridden with per-file env vars (`BIRTHDAY_DATA_FILE`, etc.). There is no database.

### Web Dashboard Auth

OAuth2 flow with CSRF state tokens stored in HttpOnly cookies. Access control:
- Discord admins see all their servers
- Users holding a designated "moderator role" see only those servers
- Owner-only actions (leave server, edit moderator list) are enforced in handlers

Sessions are in-memory with 24-hour expiry; there is no shared session store.

## Configuration

Copy `.env.example` to `.env`. Required vars:

| Var | Purpose |
|---|---|
| `DISCORD_BOT_TOKEN` | Bot token from Developer Portal |
| `DISCORD_CLIENT_ID` | OAuth2 app client ID |
| `DISCORD_CLIENT_SECRET` | OAuth2 app client secret |
| `DISCORD_REDIRECT_URI` | OAuth2 callback URL |

Key optional vars: `PORT` (default 8080), `STORAGE_DIR` (default `/data`), `SECURE_COOKIES` (set `true` in prod), `ICONS_DIR` + `BASE_URL` (FF14 job icons), `BOT_DESCRIPTION` (synced to Discord profile on start).

## Workflow

After every commit and push, run `railway logs` from the project root and check for errors before reporting the task complete.

## Key Files

- [main.go](main.go) — entry point, module registration order
- [bot/bot.go](bot/bot.go) — `Bot` struct, interaction routing, module lifecycle
- [bot/module.go](bot/module.go) — module interface definition
- [bot/modules/ping/ping.go](bot/modules/ping/ping.go) — minimal reference module
- [web/server.go](web/server.go) — route definitions
- [web/handlers.go](web/handlers.go) — HTTP handler implementations
