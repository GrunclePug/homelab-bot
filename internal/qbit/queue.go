package qbit

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// MaxRows is the most torrent rows a queue shows across all sections.
const MaxRows = 20

// etaInfinite is the ETA qBittorrent reports when it can't estimate one.
const etaInfinite = 8640000

// maxName caps a torrent name in a row, in runes.
const maxName = 70

// Section is one group of rows in the queue.
type Section struct {
	Title string
	Rows  []string
	More  int // torrents in this section left out by the row limit
}

// Queue is the download queue, ready to show.
type Queue struct {
	Sections []Section
	counts   []count
}

type count struct {
	label string
	n     int
}

type group int

const (
	groupNone group = iota // finished, or not shown
	groupDownloading
	groupStalled
	groupQueued
	groupOther
)

// classify puts a torrent in its section. qBittorrent 5 renamed the
// paused* states to stopped*; both are handled. Anything finished, which
// covers every *UP state, is left out.
func classify(t Torrent) group {
	if strings.HasSuffix(t.State, "UP") || t.Progress >= 1 {
		return groupNone
	}
	switch t.State {
	case "downloading", "forcedDL", "metaDL", "forcedMetaDL":
		return groupDownloading
	case "stalledDL":
		return groupStalled
	case "queuedDL":
		return groupQueued
	default:
		// pausedDL, stoppedDL, checkingDL, checkingResumeData, moving,
		// allocating, error, missingFiles, unknown.
		return groupOther
	}
}

// Build sorts torrents into the queue's sections, with at most maxRows
// rows in total.
func Build(torrents []Torrent, maxRows int) Queue {
	var dl, stalled, queued, other []Torrent
	for _, t := range torrents {
		switch classify(t) {
		case groupDownloading:
			dl = append(dl, t)
		case groupStalled:
			stalled = append(stalled, t)
		case groupQueued:
			queued = append(queued, t)
		case groupOther:
			other = append(other, t)
		}
	}
	slices.SortStableFunc(dl, func(a, b Torrent) int { return cmp.Compare(etaKey(a), etaKey(b)) })
	slices.SortStableFunc(queued, func(a, b Torrent) int { return cmp.Compare(queueKey(a), queueKey(b)) })

	var q Queue
	left := maxRows
	add := func(title string, ts []Torrent, row func(Torrent) string) {
		q.counts = append(q.counts, count{strings.ToLower(title), len(ts)})
		if len(ts) == 0 {
			return
		}
		s := Section{Title: title}
		for _, t := range ts {
			if left == 0 {
				s.More++
				continue
			}
			s.Rows = append(s.Rows, row(t))
			left--
		}
		q.Sections = append(q.Sections, s)
	}
	add("Downloading", dl, func(t Torrent) string {
		return fmt.Sprintf("**%s**\n%s of %s · %s/s · ETA %s",
			name(t.Name), percent(t.Progress), Bytes(t.Size), Bytes(t.DLSpeed), ETA(t.ETA))
	})
	add("Stalled", stalled, func(t Torrent) string {
		return fmt.Sprintf("**%s**\n%s of %s", name(t.Name), percent(t.Progress), Bytes(t.Size))
	})
	add("Queued", queued, func(t Torrent) string {
		return fmt.Sprintf("**%s**\n%s", name(t.Name), Bytes(t.Size))
	})
	add("Other", other, func(t Torrent) string {
		return fmt.Sprintf("**%s**\n%s", name(t.Name), stateName(t.State))
	})
	return q
}

// Empty reports whether nothing is downloading or waiting.
func (q Queue) Empty() bool { return len(q.Sections) == 0 }

// Text renders the sections as Discord markdown.
func (q Queue) Text() string {
	if q.Empty() {
		return "Nothing downloading or queued right now."
	}
	var b strings.Builder
	for i, s := range q.Sections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("__**" + s.Title + "**__")
		for _, r := range s.Rows {
			b.WriteString("\n" + r)
		}
		if s.More > 0 {
			fmt.Fprintf(&b, "\n*and %d more*", s.More)
		}
	}
	return b.String()
}

// Footer gives the totals, for example "3 downloading · 5 queued".
func (q Queue) Footer() string {
	var parts []string
	for _, c := range q.counts {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.label))
		}
	}
	return strings.Join(parts, " · ")
}

func etaKey(t Torrent) int64 {
	if t.ETA < 0 || t.ETA >= etaInfinite {
		return etaInfinite
	}
	return t.ETA
}

// queueKey orders by queue position; -1 (no queueing) goes last.
func queueKey(t Torrent) int {
	if t.Priority <= 0 {
		return int(^uint(0) >> 1)
	}
	return t.Priority
}

func percent(p float64) string {
	// Floor, so an unfinished torrent never shows 100%.
	return fmt.Sprintf("%d%%", int(p*100))
}

// Bytes formats a byte count in binary units, as qBittorrent shows them.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// ETA formats seconds left as a short duration.
func ETA(sec int64) string {
	switch {
	case sec < 0 || sec >= etaInfinite:
		return "unknown"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh %dm", sec/3600, sec%3600/60)
	default:
		return fmt.Sprintf("%dd %dh", sec/86400, sec%86400/3600)
	}
}

func stateName(s string) string {
	switch s {
	case "pausedDL":
		return "paused"
	case "stoppedDL":
		return "stopped"
	case "checkingDL":
		return "checking"
	case "checkingResumeData":
		return "checking resume data"
	case "moving":
		return "moving"
	case "allocating":
		return "allocating disk space"
	case "error":
		return "error"
	case "missingFiles":
		return "missing files"
	default:
		return s
	}
}

var markdown = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, "`", "\\`", `~`, `\~`, `|`, `\|`, `>`, `\>`, `#`, `\#`,
)

// name shortens a torrent name and escapes Discord markdown in it.
func name(s string) string {
	if r := []rune(s); len(r) > maxName {
		s = string(r[:maxName-1]) + "…"
	}
	return markdown.Replace(s)
}
