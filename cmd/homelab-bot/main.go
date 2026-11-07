// Command homelab-bot is a Discord bot for a self-hosted media server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/grunclepug/homelab-bot/internal/arr"
	"github.com/grunclepug/homelab-bot/internal/config"
	"github.com/grunclepug/homelab-bot/internal/discord"
	"github.com/grunclepug/homelab-bot/internal/plex"
	"github.com/grunclepug/homelab-bot/internal/qbit"
	"github.com/grunclepug/homelab-bot/internal/replace"
	"github.com/grunclepug/homelab-bot/internal/seerr"
	"github.com/grunclepug/homelab-bot/internal/store"
)

// Version is set at build time.
var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.Bool("healthcheck", false, "check the running bot's /healthz and exit 0 if healthy")
	flag.Parse()

	switch {
	case *version:
		fmt.Println("homelab-bot", Version)
		return
	case *healthcheck:
		if err := checkHealth(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "homelab-bot:", err)
		os.Exit(1)
	}
}

// checkHealth asks the running bot's listener for /healthz. It runs inside
// the distroless container, which has no curl.
func checkHealth() error {
	// Read .env like the bot does, so a LISTEN_ADDR set only there counts.
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = config.DefaultListenAddr
	}
	u, err := config.HealthURL(addr)
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

func run() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting", "version", Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sc := seerr.NewClient(cfg.SeerrURL, cfg.SeerrAPIKey)
	rs := replace.Service{
		Sonarr: arr.NewSonarr(cfg.SonarrURL, cfg.SonarrAPIKey),
		Radarr: arr.NewRadarr(cfg.RadarrURL, cfg.RadarrAPIKey),
	}
	qc := qbit.NewClient(cfg.QbitURL, cfg.QbitAPIKey)

	bot, err := discord.New(discord.Config{
		Token:            cfg.DiscordToken,
		GuildID:          cfg.GuildID,
		SupportChannelID: cfg.SupportChannelID,
		AdminRoleID:      cfg.AdminRoleID,
		MediaRoleID:      cfg.MediaRoleID,
		AccessChannelID:  cfg.AccessChannelID,
		ApproverRoleID:   cfg.ApproverRoleID,
		SeerrPublicURL:   cfg.SeerrPublicURL,
	}, st, sc, rs, qc, plex.NewClient(plex.DefaultBaseURL, cfg.PlexToken, cfg.PlexServerID), log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("POST /seerr", seerr.Handler(cfg.SeerrWebhookSecret, bot.HandleSeerr, log))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := errors.Join(bot.Ready(), st.Ping(ctx)); err != nil {
			log.Debug("health check failed", "err", err)
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	// Take the port before connecting to Discord, so a clash fails fast.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	if err := bot.Start(ctx); err != nil {
		ln.Close()
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		bot.Close(ctx)
	}()
	log.Info("connected to discord")

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("listen: %w", err)
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// Let work in progress finish: first Seerr webhooks, then Discord
	// clicks such as a Replace, before Discord and the database close.
	// docker-compose.yml gives the container long enough for both.
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shut down listener: %w", err)
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelDrain()
	if err := bot.Drain(drainCtx); err != nil {
		log.Warn("shutting down anyway", "err", err)
	}
	return <-errc
}
