package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/user/discord-bot-skeleton/bot"
	"github.com/user/discord-bot-skeleton/bot/modules/birthday"
	"github.com/user/discord-bot-skeleton/bot/modules/info"
	"github.com/user/discord-bot-skeleton/bot/modules/ping"
	"github.com/user/discord-bot-skeleton/bot/modules/raid"
	"github.com/user/discord-bot-skeleton/bot/modules/roles"
	"github.com/user/discord-bot-skeleton/config"
	"github.com/user/discord-bot-skeleton/web"
)

// startTime is captured at process start so "online since" reflects actual
// startup rather than the moment b.Start() opens the WebSocket.
var startTime = time.Now()

func main() {
	cfg := config.Load()

	// ── Bot ──────────────────────────────────────────────────────────────────
	b, err := bot.New(cfg)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}

	// ── Web server ───────────────────────────────────────────────────────────
	// Start the HTTP server first so Railway's healthcheck passes immediately
	// while module loading (which makes several Discord API calls) is still
	// in progress. The /health endpoint responds 200 with no dependencies.
	srv := web.NewServer(cfg, b)
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()
	log.Printf("Web UI listening on :%s", cfg.Port)

	// ── Bot modules ──────────────────────────────────────────────────────────
	// Register built-in modules. To add your own, implement the bot.Module
	// interface and call b.LoadModule(yourmodule.New()) here.
	if err := b.LoadModule(ping.New()); err != nil {
		log.Fatalf("failed to load ping module: %v", err)
	}
	if err := b.LoadModule(info.New(cfg.ClientID, startTime)); err != nil {
		log.Fatalf("failed to load info module: %v", err)
	}
	if err := b.LoadModule(birthday.New(cfg.BirthdayDataFile)); err != nil {
		log.Fatalf("failed to load birthday module: %v", err)
	}
	if err := b.LoadModule(roles.New(cfg.RolesDataFile)); err != nil {
		log.Fatalf("failed to load roles module: %v", err)
	}
	if err := b.LoadModule(raid.New(cfg.RaidDataFile, cfg.ClientID)); err != nil {
		log.Fatalf("failed to load raid module: %v", err)
	}

	if err := b.Start(); err != nil {
		log.Fatalf("failed to start bot: %v", err)
	}
	defer b.Stop()

	log.Printf("Bot and web UI running. Visit http://localhost:%s", cfg.Port)

	// ── Graceful shutdown ────────────────────────────────────────────────────
	// Block until we receive SIGINT (Ctrl+C), SIGTERM (systemd/Docker stop),
	// or the web server exits unexpectedly.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
		log.Println("Shutting down gracefully...")
	case err := <-srvErr:
		log.Fatalf("web server failed: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("web server shutdown error: %v", err)
	}
}
