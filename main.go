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
	"github.com/user/discord-bot-skeleton/config"
	"github.com/user/discord-bot-skeleton/web"
)

func main() {
	cfg := config.Load()

	// ── Bot ──────────────────────────────────────────────────────────────────
	b, err := bot.New(cfg)
	if err != nil {
		log.Fatalf("failed to create bot: %v", err)
	}

	// Register built-in modules. To add your own, implement the bot.Module
	// interface and call b.LoadModule(yourmodule.New()) here.
	if err := b.LoadModule(ping.New()); err != nil {
		log.Fatalf("failed to load ping module: %v", err)
	}
	if err := b.LoadModule(info.New()); err != nil {
		log.Fatalf("failed to load info module: %v", err)
	}
	if err := b.LoadModule(birthday.New(cfg.TenorAPIKey, cfg.BirthdayDataFile)); err != nil {
		log.Fatalf("failed to load birthday module: %v", err)
	}

	if err := b.Start(); err != nil {
		log.Fatalf("failed to start bot: %v", err)
	}
	defer b.Stop()

	// ── Web server ───────────────────────────────────────────────────────────
	srv := web.NewServer(cfg, b)
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

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
