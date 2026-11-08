package discord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"

	"github.com/grunclepug/homelab-bot/internal/arr"
	"github.com/grunclepug/homelab-bot/internal/kuma"
	"github.com/grunclepug/homelab-bot/internal/plex"
	"github.com/grunclepug/homelab-bot/internal/qbit"
	"github.com/grunclepug/homelab-bot/internal/seerr"
)

// StatusSources are what /status reads. A nil Kuma leaves the monitors out.
type StatusSources struct {
	Plex interface {
		Server(context.Context) (plex.Server, error)
	}
	Seerr interface {
		Stats(context.Context) (seerr.Stats, error)
	}
	Sonarr interface {
		Stats(context.Context) (arr.Stats, error)
	}
	Radarr interface {
		Stats(context.Context) (arr.Stats, error)
	}
	Qbit interface {
		Transfer(context.Context) (qbit.Transfer, error)
	}
	Kuma interface {
		Groups(context.Context) ([]kuma.Group, error)
	}
}

const (
	idStatusRefresh = "status:refresh"

	textStatusOnly = "You need the media role to use /status."

	// statusTimeout bounds each source, so one slow app can't hold up the
	// card.
	statusTimeout = 10 * time.Second

	colorTrouble = 0xE74C3C

	// maxKumaGroups leaves room for the apps within Discord's 25 fields.
	maxKumaGroups = 18
)

// report is everything the status card shows, read at one moment.
type report struct {
	at time.Time

	plex      plex.Server
	plexErr   error
	seerr     seerr.Stats
	seerrErr  error
	sonarr    arr.Stats
	sonarrErr error
	radarr    arr.Stats
	radarrErr error
	qbit      qbit.Transfer
	qbitErr   error

	kumaOn  bool
	kuma    []kuma.Group
	kumaErr error
}

// collect asks every source at once.
func (s StatusSources) collect(ctx context.Context) report {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	r := report{at: time.Now(), kumaOn: s.Kuma != nil}
	var wg sync.WaitGroup
	wg.Go(func() { r.plex, r.plexErr = s.Plex.Server(ctx) })
	wg.Go(func() { r.seerr, r.seerrErr = s.Seerr.Stats(ctx) })
	wg.Go(func() { r.sonarr, r.sonarrErr = s.Sonarr.Stats(ctx) })
	wg.Go(func() { r.radarr, r.radarrErr = s.Radarr.Stats(ctx) })
	wg.Go(func() { r.qbit, r.qbitErr = s.Qbit.Transfer(ctx) })
	if r.kumaOn {
		wg.Go(func() { r.kuma, r.kumaErr = s.Kuma.Groups(ctx) })
	}
	wg.Wait()
	return r
}

// errs joins what couldn't be read, for the log: the errors name hosts, so
// the card doesn't show them.
func (r report) errs() error {
	return errors.Join(r.plexErr, r.seerrErr, r.sonarrErr, r.radarrErr, r.qbitErr, r.kumaErr)
}

// level is how a part of the stack is doing.
type level int

const (
	levelOK level = iota
	levelWarn
	levelDown
)

func (l level) dot() string {
	return [...]string{"🟢", "🟡", "🔴"}[l]
}

// part is one field of the status card.
type part struct {
	name  string
	level level
	text  string
}

func (p part) field(maxName, maxValue int) discord.EmbedField {
	return discord.EmbedField{
		Name:   truncate(p.level.dot()+" "+p.name, maxName),
		Value:  truncate(p.text, maxValue),
		Inline: new(true),
	}
}

// Discord's embed limits, in characters.
const (
	maxFieldName  = 256
	maxEmbedTotal = 6000
)

// unreachable describes a failed read without naming hosts.
func unreachable(err error) string {
	if _, ok := errors.AsType[*arr.StatusError](err); ok {
		return arr.Brief(err)
	}
	if te, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](err); errors.Is(err, context.DeadlineExceeded) || (ok && te.Timeout()) {
		return "Timed out"
	}
	// qBittorrent says so; Seerr's errors carry the HTTP status.
	msg := err.Error()
	if strings.Contains(msg, "API key rejected") || strings.Contains(msg, "401 Unauthorized") || strings.Contains(msg, "403 Forbidden") {
		return "API key rejected"
	}
	return "Unreachable"
}

// shortVersion drops build suffixes: 1.43.5.11079-b3b13e506 is 1.43.5.11079.
func shortVersion(v string) string {
	v, _, _ = strings.Cut(v, "-")
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

func plexPart(r report) part {
	p := part{name: "Plex"}
	switch {
	case errors.Is(r.plexErr, plex.ErrServerNotFound):
		p.level, p.text = levelDown, "Not on the owner's plex.tv account: check PLEX_SERVER_ID"
	case r.plexErr != nil:
		p.level, p.text = levelDown, "plex.tv: "+strings.ToLower(unreachable(r.plexErr))
	case !r.plex.Online:
		p.level, p.text = levelDown, "Offline, as plex.tv sees it"
	default:
		p.text = "Online\n" + shortVersion(r.plex.Version)
	}
	return p
}

func seerrPart(r report) part {
	p := part{name: "Seerr"}
	if r.seerrErr != nil {
		p.level, p.text = levelDown, unreachable(r.seerrErr)
		return p
	}
	s := r.seerr
	p.text = fmt.Sprintf("%d to approve, %d on the way\n%s", s.Pending, s.Processing, plural(s.OpenIssues, "open issue"))
	if s.UpdateAvailable {
		p.level, p.text = levelWarn, p.text+"\nUpdate available"
	}
	return p
}

func arrPart(name string, s arr.Stats, err error) part {
	p := part{name: name}
	if err != nil {
		p.level, p.text = levelDown, unreachable(err)
		return p
	}
	lines := []string{plural(s.Queue, "item") + " in the queue"}
	var problems []arr.HealthCheck
	for _, h := range s.Health {
		switch h.Type {
		case "error":
			p.level = levelDown
		case "warning":
			p.level = max(p.level, levelWarn)
		default:
			continue
		}
		problems = append(problems, h)
	}
	if len(problems) == 0 {
		lines = append(lines, "No health warnings")
	} else {
		more := ""
		if len(problems) > 1 {
			more = fmt.Sprintf(" (+%d more)", len(problems)-1)
		}
		lines = append(lines, truncate(problems[0].Message, 150)+more)
	}
	p.text = strings.Join(lines, "\n")
	return p
}

func qbitPart(r report) part {
	p := part{name: "qBittorrent"}
	if r.qbitErr != nil {
		p.level, p.text = levelDown, unreachable(r.qbitErr)
		return p
	}
	t := r.qbit
	p.text = fmt.Sprintf("↓ %s/s  ↑ %s/s\n%s", qbit.Bytes(t.DLSpeed), qbit.Bytes(t.UPSpeed), shortVersion(t.Version))
	switch t.Connection {
	case "connected":
	case "firewalled":
		p.level, p.text = levelWarn, p.text+"\nFirewalled: peers can't connect in"
	default:
		p.level, p.text = levelDown, p.text+"\nNo connection to peers"
	}
	return p
}

// storagePart shows the largest volume Sonarr sees, or Radarr if Sonarr
// shows none: that is where the media lives.
func storagePart(r report) (part, bool) {
	disks := r.sonarr.Disks
	if r.sonarrErr != nil || len(disks) == 0 {
		disks = r.radarr.Disks
	}
	if len(disks) == 0 {
		return part{}, false
	}
	d := slices.MaxFunc(disks, func(a, b arr.Disk) int { return cmp.Compare(a.TotalSpace, b.TotalSpace) })
	if d.TotalSpace <= 0 {
		return part{}, false
	}
	p := part{name: "Storage"}
	free := float64(d.FreeSpace) / float64(d.TotalSpace)
	switch {
	case free < 0.05:
		p.level = levelDown
	case free < 0.10:
		p.level = levelWarn
	}
	p.text = fmt.Sprintf("%s free of %s\n%.0f%% used", qbit.Bytes(d.FreeSpace), qbit.Bytes(d.TotalSpace), (1-free)*100)
	return p, true
}

// kumaParts summarises each status page group; hidden is how many groups
// didn't fit on the card.
func kumaParts(r report) (parts []part, hidden int) {
	if !r.kumaOn {
		return nil, 0
	}
	if r.kumaErr != nil {
		return []part{{name: "Uptime Kuma", level: levelDown, text: "Couldn't read the status page: " + strings.ToLower(unreachable(r.kumaErr))}}, 0
	}
	var out []part
	for i, g := range r.kuma {
		if i == maxKumaGroups {
			hidden = len(r.kuma) - i
			break
		}
		p := part{name: escape(g.Name)}
		var up, timed int
		var uptime float64
		var down, other []string
		for _, m := range g.Monitors {
			if m.UptimeKnown {
				uptime += m.Uptime
				timed++
			}
			switch {
			case !m.Seen || m.Status == kuma.Pending:
				p.level = max(p.level, levelWarn)
				other = append(other, escape(m.Name)+" pending")
			case m.Status == kuma.Up:
				up++
			case m.Status == kuma.Maintenance:
				p.level = max(p.level, levelWarn)
				other = append(other, escape(m.Name)+" in maintenance")
			default:
				p.level = levelDown
				down = append(down, escape(m.Name))
			}
		}
		lines := []string{fmt.Sprintf("%d of %d up", up, len(g.Monitors))}
		if timed > 0 {
			lines[0] += fmt.Sprintf(", %s over 24h", percent(uptime/float64(timed)))
		}
		if len(down) > 0 {
			lines = append(lines, "Down: "+strings.Join(down, ", "))
		}
		lines = append(lines, other...)
		p.text = strings.Join(lines, "\n")
		out = append(out, p)
	}
	return out, hidden
}

// percent shows an uptime ratio without rounding 99.96% up to 100%.
func percent(ratio float64) string {
	if ratio >= 1 {
		return "100%"
	}
	return fmt.Sprintf("%.2f%%", float64(int(ratio*10000))/100)
}

// statusEmbed is the status card.
func statusEmbed(r report) discord.Embed {
	parts := []part{plexPart(r), seerrPart(r), qbitPart(r),
		arrPart("Sonarr", r.sonarr, r.sonarrErr), arrPart("Radarr", r.radarr, r.radarrErr)}
	if s, ok := storagePart(r); ok {
		parts = append(parts, s)
	}
	kparts, hidden := kumaParts(r)
	parts = append(parts, kparts...)

	worst := levelOK
	var trouble []string
	for _, p := range parts {
		worst = max(worst, p.level)
		if p.level != levelOK {
			trouble = append(trouble, p.name)
		}
	}
	summary := "Everything is running."
	if len(trouble) > 0 {
		summary = "Needs a look: " + strings.Join(trouble, ", ") + "."
	}
	if hidden > 0 {
		summary += fmt.Sprintf(" %d more Kuma groups don't fit on this card.", hidden)
	}
	e := discord.NewEmbed().
		WithTitle("Media server status").
		WithDescription(truncate(worst.dot()+" "+summary, maxFieldValue)).
		WithColor([...]int{colorApproved, colorPending, colorTrouble}[worst]).
		WithTimestamp(r.at).
		WithFooterText("Refreshed")
	// In a big outage every group lists what's down: shorten the fields
	// until the card fits, as Discord refuses a larger one.
	n := utf8.RuneCountInString
	for maxValue := maxFieldValue; ; maxValue /= 2 {
		fields := make([]discord.EmbedField, len(parts))
		total := n(e.Title) + n(e.Description) + n("Refreshed")
		for i, p := range parts {
			fields[i] = p.field(min(maxFieldName, maxValue/4), maxValue)
			total += n(fields[i].Name) + n(fields[i].Value)
		}
		if total <= maxEmbedTotal || maxValue < 64 {
			e.Fields = fields
			return e
		}
	}
}

func statusButtons() discord.ActionRowComponent {
	return discord.NewActionRow(discord.NewSecondaryButton("Refresh", idStatusRefresh))
}

// status answers /status with the card, which only its user sees.
func (b *Bot) status(e *events.ApplicationCommandInteractionCreate) {
	if !hasRole(e.Member(), b.cfg.MediaRoleID, b.cfg.AdminRoleID) {
		b.ephemeral(e.CreateMessage, textStatusOnly)
		return
	}
	if err := e.DeferCreateMessage(true); err != nil {
		b.log.Error("defer status", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	b.respond(ctx, e.Token(), b.statusUpdate(ctx))
}

// statusRefreshed redraws the card in place.
func (b *Bot) statusRefreshed(e *events.ComponentInteractionCreate) {
	if !hasRole(e.Member(), b.cfg.MediaRoleID, b.cfg.AdminRoleID) {
		b.ephemeral(e.CreateMessage, textStatusOnly)
		return
	}
	if err := e.DeferUpdateMessage(); err != nil {
		b.log.Error("defer status refresh", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	b.respond(ctx, e.Token(), b.statusUpdate(ctx))
}

func (b *Bot) statusUpdate(ctx context.Context) discord.MessageUpdate {
	r := b.sources.collect(ctx)
	if err := r.errs(); err != nil {
		b.log.Warn("status: some sources failed", "err", err)
	}
	return discord.NewMessageUpdate().WithEmbeds(statusEmbed(r)).WithComponents(statusButtons())
}
