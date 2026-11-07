// Package config loads and validates the bot's settings from environment
// variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"github.com/disgoorg/snowflake/v2"
)

// Config holds every setting the bot needs.
type Config struct {
	DiscordToken     string
	GuildID          snowflake.ID
	SupportChannelID snowflake.ID
	AdminRoleID      snowflake.ID
	MediaRoleID      snowflake.ID

	SeerrURL           string
	SeerrAPIKey        string
	SeerrWebhookSecret string

	SonarrURL    string
	SonarrAPIKey string
	RadarrURL    string
	RadarrAPIKey string

	QbitURL      string
	QbitUsername string
	QbitPassword string

	ListenAddr string
	DBPath     string
	LogLevel   slog.Level
}

// Defaults for the optional settings.
const (
	DefaultListenAddr = ":8080"
	DefaultDBPath     = "data/homelab-bot.db"
)

// Load reads the configuration through lookup, which is normally
// os.LookupEnv. It reports every missing or invalid variable at once.
func Load(lookup func(string) (string, bool)) (Config, error) {
	var (
		c       Config
		missing []string
		invalid []string
	)
	get := func(name string) string {
		v, ok := lookup(name)
		v = strings.TrimSpace(v)
		if !ok || v == "" {
			missing = append(missing, name)
		}
		return v
	}
	optional := func(name, def string) string {
		if v, ok := lookup(name); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	id := func(name string) snowflake.ID {
		v := get(name)
		if v == "" {
			return 0
		}
		sf, err := snowflake.Parse(v)
		if err != nil || sf == 0 {
			invalid = append(invalid, name+" must be a Discord ID")
		}
		return sf
	}
	baseURL := func(name string) string {
		v := get(name)
		if v == "" {
			return ""
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			invalid = append(invalid, name+" must be an http or https URL")
		}
		return strings.TrimRight(v, "/")
	}

	c.DiscordToken = get("DISCORD_TOKEN")
	c.GuildID = id("DISCORD_GUILD_ID")
	c.SupportChannelID = id("DISCORD_SUPPORT_CHANNEL_ID")
	c.AdminRoleID = id("DISCORD_ADMIN_ROLE_ID")
	c.MediaRoleID = id("DISCORD_MEDIA_ROLE_ID")

	c.SeerrURL = baseURL("SEERR_URL")
	c.SeerrAPIKey = get("SEERR_API_KEY")
	c.SeerrWebhookSecret = get("SEERR_WEBHOOK_SECRET")

	c.SonarrURL = baseURL("SONARR_URL")
	c.SonarrAPIKey = get("SONARR_API_KEY")
	c.RadarrURL = baseURL("RADARR_URL")
	c.RadarrAPIKey = get("RADARR_API_KEY")

	c.QbitURL = baseURL("QBIT_URL")
	c.QbitUsername = get("QBIT_USERNAME")
	c.QbitPassword = get("QBIT_PASSWORD")

	c.ListenAddr = optional("LISTEN_ADDR", DefaultListenAddr)
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		invalid = append(invalid, "LISTEN_ADDR must be host:port, for example :8080")
	}
	c.DBPath = optional("DB_PATH", DefaultDBPath)

	level := optional("LOG_LEVEL", "info")
	if err := c.LogLevel.UnmarshalText([]byte(level)); err != nil {
		invalid = append(invalid, "LOG_LEVEL must be debug, info, warn or error")
	}

	var errs []error
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("missing %s", strings.Join(missing, ", ")))
	}
	for _, s := range invalid {
		errs = append(errs, errors.New(s))
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return c, nil
}

// HealthURL returns the /healthz URL on the loopback interface for a
// listener address such as ":8080".
func HealthURL(listenAddr string) (string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse listen address: %w", err)
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz", nil
}
