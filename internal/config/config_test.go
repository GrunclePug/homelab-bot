package config

import (
	"log/slog"
	"strings"
	"testing"
)

func full() map[string]string {
	return map[string]string{
		"DISCORD_TOKEN":              "token",
		"DISCORD_GUILD_ID":           "100000000000000001",
		"DISCORD_SUPPORT_CHANNEL_ID": "100000000000000002",
		"DISCORD_ADMIN_ROLE_ID":      "100000000000000003",
		"DISCORD_MEDIA_ROLE_ID":      "100000000000000004",
		"SEERR_URL":                  "http://seerr.example:5055/",
		"SEERR_API_KEY":              "k1",
		"SEERR_WEBHOOK_SECRET":       "secret",
		"SONARR_URL":                 "http://sonarr.example:8989",
		"SONARR_API_KEY":             "k2",
		"RADARR_URL":                 "http://radarr.example:7878",
		"RADARR_API_KEY":             "k3",
		"QBIT_URL":                   "http://qbit.example:8080",
		"QBIT_USERNAME":              "admin",
		"QBIT_PASSWORD":              "pw",
		"DISCORD_ACCESS_CHANNEL_ID":  "100000000000000005",
		"DISCORD_APPROVER_ROLE_ID":   "100000000000000006",
		"PLEX_TOKEN":                 "plex-token",
		"PLEX_SERVER_ID":             "abc123",
	}
}

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(lookup(full()))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.DBPath != "data/homelab-bot.db" || c.LogLevel != slog.LevelInfo {
		t.Errorf("defaults: %q %q %v", c.ListenAddr, c.DBPath, c.LogLevel)
	}
	if c.SeerrURL != "http://seerr.example:5055" {
		t.Errorf("trailing slash kept: %q", c.SeerrURL)
	}
	if c.GuildID.String() != "100000000000000001" {
		t.Errorf("guild = %s", c.GuildID)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		set  map[string]string
		drop []string
		want []string
	}{
		{name: "missing", drop: []string{"DISCORD_TOKEN", "QBIT_PASSWORD"}, want: []string{"missing DISCORD_TOKEN, QBIT_PASSWORD"}},
		{name: "blank counts as missing", set: map[string]string{"SEERR_API_KEY": "  "}, want: []string{"missing SEERR_API_KEY"}},
		{name: "bad id", set: map[string]string{"DISCORD_GUILD_ID": "abc"}, want: []string{"DISCORD_GUILD_ID must be a Discord ID"}},
		{name: "bad url", set: map[string]string{"SONARR_URL": "sonarr:8989"}, want: []string{"SONARR_URL must be an http or https URL"}},
		{name: "bad level", set: map[string]string{"LOG_LEVEL": "loud"}, want: []string{"LOG_LEVEL must be"}},
		{name: "bad public url", set: map[string]string{"SEERR_PUBLIC_URL": "seerr.example"}, want: []string{"SEERR_PUBLIC_URL must be"}},
		{name: "missing plex", drop: []string{"PLEX_TOKEN"}, want: []string{"missing PLEX_TOKEN"}},
		{name: "bad listen", set: map[string]string{"LISTEN_ADDR": "8080"}, want: []string{"LISTEN_ADDR must be host:port"}},
		{name: "several", drop: []string{"RADARR_API_KEY"}, set: map[string]string{"QBIT_URL": "ftp://x"},
			want: []string{"missing RADARR_API_KEY", "QBIT_URL must be"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := full()
			for _, k := range tt.drop {
				delete(env, k)
			}
			for k, v := range tt.set {
				env[k] = v
			}
			_, err := Load(lookup(env))
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{":8080": "http://127.0.0.1:8080/healthz", "0.0.0.0:9000": "http://127.0.0.1:9000/healthz"} {
		got, err := HealthURL(in)
		if err != nil || got != want {
			t.Errorf("HealthURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := HealthURL("8080"); err == nil {
		t.Error("HealthURL(8080): want error")
	}
}
