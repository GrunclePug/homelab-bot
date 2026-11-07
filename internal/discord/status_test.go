package discord

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/arr"
	"github.com/grunclepug/homelab-bot/internal/kuma"
	"github.com/grunclepug/homelab-bot/internal/plex"
	"github.com/grunclepug/homelab-bot/internal/qbit"
	"github.com/grunclepug/homelab-bot/internal/seerr"
)

const tib = int64(1) << 40

// healthy is a report with nothing wrong.
func healthy() report {
	return report{
		at:     time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
		plex:   plex.Server{Online: true, Version: "1.43.5.11079-b3b13e506"},
		seerr:  seerr.Stats{Version: "3.5.1", Pending: 2, Processing: 47, OpenIssues: 1},
		sonarr: arr.Stats{Version: "4.0.20", Queue: 227, Disks: []arr.Disk{{Path: "/", FreeSpace: 400 << 30, TotalSpace: 450 << 30}, {Path: "/media", FreeSpace: 6 * tib, TotalSpace: 16 * tib}}},
		radarr: arr.Stats{Version: "6.4.4", Queue: 36},
		qbit:   qbit.Transfer{Version: "v5.3.0", Connection: "connected", DLSpeed: 2663501, UPSpeed: 415549},
		kumaOn: true,
		kuma: []kuma.Group{
			{Name: "Media", Monitors: []kuma.Monitor{{Name: "Plex", Status: kuma.Up, Seen: true, Uptime: 1, UptimeKnown: true}, {Name: "Seerr", Status: kuma.Up, Seen: true, Uptime: 0.9996, UptimeKnown: true}}},
		},
	}
}

func fieldNamed(e discord.Embed, name string) (discord.EmbedField, bool) {
	for _, f := range e.Fields {
		if strings.HasSuffix(f.Name, " "+name) {
			return f, true
		}
	}
	return discord.EmbedField{}, false
}

func TestStatusEmbedHealthy(t *testing.T) {
	e := statusEmbed(healthy())
	if e.Color != colorApproved || e.Description != "🟢 Everything is running." {
		t.Errorf("color %x, description %q", e.Color, e.Description)
	}
	want := map[string][]string{
		"Plex":        {"Online", "v1.43.5.11079"},
		"Seerr":       {"2 to approve, 47 on the way", "1 open issue"},
		"qBittorrent": {"↓ 2.5 MiB/s", "↑ 405.8 KiB/s"},
		"Sonarr":      {"227 items in the queue", "No health warnings"},
		"Radarr":      {"36 items in the queue"},
		"Storage":     {"6.0 TiB free of 16.0 TiB", "62% used"},
		"Media":       {"2 of 2 up, 99.98% over 24h"},
	}
	for name, texts := range want {
		f, ok := fieldNamed(e, name)
		if !ok {
			t.Errorf("no %s field in %+v", name, e.Fields)
			continue
		}
		if !strings.HasPrefix(f.Name, "🟢") {
			t.Errorf("%s: %q, want green", name, f.Name)
		}
		for _, s := range texts {
			if !strings.Contains(f.Value, s) {
				t.Errorf("%s = %q, want %q in it", name, f.Value, s)
			}
		}
	}
	if len(e.Fields) > 25 {
		t.Errorf("%d fields", len(e.Fields))
	}
}

func TestStatusEmbedTrouble(t *testing.T) {
	r := healthy()
	r.plex.Online = false
	r.seerr.UpdateAvailable = true
	r.sonarr.Health = []arr.HealthCheck{
		{Type: "warning", Message: "Indexers unavailable due to failures for more than 6 hours: Example"},
		{Type: "notice", Message: "ignored"},
		{Type: "warning", Message: "Download client unavailable"},
	}
	r.radarrErr = &arr.StatusError{App: "radarr", Method: "GET", Path: "/api/v3/health", Code: 500, Body: "at 10.0.0.5"}
	r.qbit.Connection = "firewalled"
	r.kuma = append(r.kuma, kuma.Group{Name: "Net_work", Monitors: []kuma.Monitor{
		{Name: "DNS", Status: kuma.Down, Seen: true, Uptime: 0.5, UptimeKnown: true},
		{Name: "Router", Status: kuma.Maintenance, Seen: true, Uptime: 1, UptimeKnown: true},
		{Name: "New", Seen: false},
	}})
	e := statusEmbed(r)
	if e.Color != colorTrouble || !strings.HasPrefix(e.Description, "🔴 Needs a look: Plex, Seerr, qBittorrent, Sonarr, Radarr, Net\\_work.") {
		t.Errorf("color %x, description %q", e.Color, e.Description)
	}
	checks := []struct{ name, dot, text string }{
		{"Plex", "🔴", "Offline"},
		{"Seerr", "🟡", "Update available"},
		{"qBittorrent", "🟡", "Firewalled"},
		{"Sonarr", "🟡", "Indexers unavailable due to failures for more than 6 hours: Example (+1 more)"},
		{"Radarr", "🔴", "Radarr answered 500 Internal Server Error"},
		{"Net\\_work", "🔴", "0 of 3 up, 75.00% over 24h\nDown: DNS\nRouter in maintenance\nNew pending"},
	}
	for _, c := range checks {
		f, ok := fieldNamed(e, c.name)
		if !ok || !strings.HasPrefix(f.Name, c.dot) || !strings.Contains(f.Value, c.text) {
			t.Errorf("%s = %q %q, want %s and %q", c.name, f.Name, f.Value, c.dot, c.text)
		}
	}
	for _, f := range e.Fields {
		if strings.Contains(f.Value, "10.0.0.5") {
			t.Errorf("%s shows a host: %q", f.Name, f.Value)
		}
	}
}

func TestStatusStorageWarnings(t *testing.T) {
	for _, tt := range []struct {
		free int64
		dot  string
	}{{2 * tib, "🟢"}, {tib + tib/2, "🟡"}, {tib / 2, "🔴"}} {
		r := healthy()
		r.sonarrErr = errors.New("down")
		r.radarr.Disks = []arr.Disk{{FreeSpace: tt.free, TotalSpace: 16 * tib}}
		if f, ok := fieldNamed(statusEmbed(r), "Storage"); !ok || !strings.HasPrefix(f.Name, tt.dot) {
			t.Errorf("free %d: %q, want %s (from Radarr, as Sonarr is down)", tt.free, f.Name, tt.dot)
		}
	}
	r := healthy()
	r.sonarr.Disks = nil
	if _, ok := fieldNamed(statusEmbed(r), "Storage"); ok {
		t.Error("storage shown with no disks")
	}
}

func TestStatusKuma(t *testing.T) {
	r := healthy()
	r.kumaOn = false
	if _, ok := fieldNamed(statusEmbed(r), "Media"); ok {
		t.Error("Kuma shown when not configured")
	}
	r = healthy()
	r.kumaErr = fmt.Errorf("kuma: Get http://10.0.0.5:3001/api: %w", context.DeadlineExceeded)
	f, ok := fieldNamed(statusEmbed(r), "Uptime Kuma")
	if !ok || !strings.HasPrefix(f.Name, "🔴") || f.Value != "Couldn't read the status page: timed out" {
		t.Errorf("Kuma error: %q %q", f.Name, f.Value)
	}
	r = healthy()
	r.kuma = nil
	for i := range 30 {
		r.kuma = append(r.kuma, kuma.Group{Name: fmt.Sprint("G", i)})
	}
	e := statusEmbed(r)
	if len(e.Fields) > 25 || !strings.Contains(e.Description, "12 more Kuma groups don't fit") {
		t.Errorf("%d fields, description %q", len(e.Fields), e.Description)
	}
}

// A big outage lists every monitor that's down; the card must still fit
// Discord's 6000-character limit.
func TestStatusFitsDiscord(t *testing.T) {
	r := healthy()
	r.kuma = nil
	for g := range 30 {
		grp := kuma.Group{Name: strings.Repeat("group ", 60) + fmt.Sprint(g)}
		for m := range 40 {
			grp.Monitors = append(grp.Monitors, kuma.Monitor{Name: fmt.Sprintf("a long monitor name %d", m), Status: kuma.Down, Seen: true})
		}
		r.kuma = append(r.kuma, grp)
	}
	e := statusEmbed(r)
	n := utf8.RuneCountInString
	total := n(e.Title) + n(e.Description) + n("Refreshed")
	for _, f := range e.Fields {
		total += n(f.Name) + n(f.Value)
		if n(f.Name) > 256 || n(f.Value) > maxFieldValue {
			t.Errorf("field: name %d, value %d", n(f.Name), n(f.Value))
		}
	}
	if total > 6000 || len(e.Fields) > 25 {
		t.Errorf("card is %d characters in %d fields", total, len(e.Fields))
	}
}

func TestStatusLabels(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "i/o timeout", IsTimeout: true}, "Timed out"},
		{errors.New("seerr: read status: GET /api/v1/status: 401 Unauthorized: {}"), "API key rejected"},
		{errors.New("dial tcp 192.0.2.1:5055: connect: connection refused"), "Unreachable"},
	} {
		if got := unreachable(tc.err); got != tc.want {
			t.Errorf("unreachable(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	r := healthy()
	r.plexErr = plex.ErrServerNotFound
	if f, _ := fieldNamed(statusEmbed(r), "Plex"); !strings.HasPrefix(f.Value, "Not on the owner's plex.tv account") {
		t.Errorf("server not found: %q", f.Value)
	}
	// Uptime of monitors Kuma has no figure for is left out, not counted as 0.
	r = healthy()
	r.kuma[0].Monitors = append(r.kuma[0].Monitors, kuma.Monitor{Name: "New", Status: kuma.Up, Seen: true})
	if f, _ := fieldNamed(statusEmbed(r), "Media"); !strings.Contains(f.Value, "3 of 3 up, 99.98% over 24h") {
		t.Errorf("unknown uptime: %q", f.Value)
	}
	// Storage comes from Radarr if Sonarr lists no disks.
	r = healthy()
	r.sonarr.Disks = nil
	r.radarr.Disks = []arr.Disk{{FreeSpace: tib, TotalSpace: 2 * tib}}
	if f, ok := fieldNamed(statusEmbed(r), "Storage"); !ok || !strings.Contains(f.Value, "1.0 TiB free of 2.0 TiB") {
		t.Errorf("storage from Radarr: %q", f.Value)
	}
}

func TestPercent(t *testing.T) {
	for in, want := range map[float64]string{1: "100%", 0.99996: "99.99%", 0.5: "50.00%", 0: "0.00%"} {
		if got := percent(in); got != want {
			t.Errorf("percent(%v) = %q, want %q", in, got, want)
		}
	}
}

type fakeSources struct{ slow bool }

func (f fakeSources) Server(ctx context.Context) (plex.Server, error) {
	if f.slow {
		<-ctx.Done()
		return plex.Server{}, ctx.Err()
	}
	return plex.Server{Online: true}, nil
}
func (fakeSources) Stats(context.Context) (arr.Stats, error) { return arr.Stats{Queue: 3}, nil }
func (fakeSources) Transfer(context.Context) (qbit.Transfer, error) {
	return qbit.Transfer{}, errors.New("qbittorrent: API key rejected: check QBIT_API_KEY")
}

type fakeSeerrStats struct{}

func (fakeSeerrStats) Stats(context.Context) (seerr.Stats, error) {
	return seerr.Stats{Pending: 4}, nil
}

// One slow source shows as timed out without holding up the others.
func TestStatusCollect(t *testing.T) {
	f := fakeSources{slow: true}
	s := StatusSources{Plex: f, Seerr: fakeSeerrStats{}, Sonarr: f, Radarr: f, Qbit: f}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := s.collect(ctx)
	if time.Since(start) > 5*time.Second {
		t.Errorf("collect took %v", time.Since(start))
	}
	if r.kumaOn || r.seerr.Pending != 4 || r.sonarr.Queue != 3 || !errors.Is(r.plexErr, context.DeadlineExceeded) {
		t.Errorf("report = %+v", r)
	}
	e := statusEmbed(r)
	for name, want := range map[string]string{"Plex": "plex.tv: timed out", "qBittorrent": "API key rejected"} {
		if f, _ := fieldNamed(e, name); f.Value != want {
			t.Errorf("%s = %q, want %q", name, f.Value, want)
		}
	}
}

func TestStatusPermissions(t *testing.T) {
	var got []reply
	routingBot().onCommand(commandEvent(t, "status", []snowflake.ID{approverRole}, &got))
	if len(got) != 1 || got[0].text != textStatusOnly {
		t.Errorf("/status: replies = %+v", got)
	}
	got = nil
	routingBot().onComponent(componentEvent(t, idStatusRefresh, member, nil, &got))
	if len(got) != 1 || got[0].text != textStatusOnly {
		t.Errorf("refresh: replies = %+v", got)
	}
}
