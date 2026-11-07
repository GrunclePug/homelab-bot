package discord

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"

	"github.com/grunclepug/homelab-bot/internal/seerr"
)

// Embed colours.
const (
	colorOpen     = 0xE67E22
	colorResolved = 0x95A5A6
	colorQueue    = 0x3498DB
)

// Embed limits set by Discord.
const (
	maxDescription = 4096
	maxFieldValue  = 1024
	maxLabel       = 100 // select option label and description
)

const (
	footerPrefix   = "Seerr issue #"
	fieldStatus    = "Status"
	fieldReplace   = "Replacement"
	statusOpen     = "Open"
	statusResolved = "Resolved"
)

// Custom ID prefixes for components.
const (
	idReplace = "replace:"
	idResolve = "resolve:"
	idPick    = "pick:"
	idConfirm = "confirm:"
	idCancel  = "cancel:"
)

// issueView is what an issue post shows.
type issueView struct {
	ID          int
	Title       string
	Event       string
	Type        string
	ReportedBy  string
	Where       string // episode or season, for a show
	Description string
	Image       string
}

// newIssueView combines the webhook payload with the issue fetched from
// Seerr, which may be nil.
func newIssueView(id int, p seerr.Payload, is *seerr.Issue) issueView {
	v := issueView{
		ID:          id,
		Title:       orDefault(p.Subject, fmt.Sprintf("Issue %d", id)),
		Event:       p.Event,
		Type:        typeName(p.IssueType),
		ReportedBy:  p.ReportedBy,
		Description: strings.TrimSpace(p.Message),
		Image:       p.Image,
	}
	if is == nil {
		return v
	}
	if v.Type == "" {
		v.Type = issueTypeName(is.IssueType)
	}
	if v.ReportedBy == "" {
		v.ReportedBy = is.CreatedBy.DisplayName
	}
	if v.Description == "" && len(is.Comments) > 0 {
		v.Description = strings.TrimSpace(is.Comments[0].Message)
	}
	if is.Media.MediaType == seerr.MediaTV {
		v.Where = where(is.ProblemSeason, is.ProblemEpisode)
	}
	return v
}

func (v issueView) embed() discord.Embed {
	e := discord.NewEmbed().
		WithTitle(truncate(v.Title, 256)).
		WithColor(colorOpen).
		WithFooterText(footerPrefix + strconv.Itoa(v.ID)).
		WithTimestamp(time.Now())
	if v.Event != "" {
		e = e.WithAuthorName(truncate(v.Event, 256))
	}
	if v.Description != "" {
		e = e.WithDescription(truncate(v.Description, maxDescription))
	}
	if v.Image != "" {
		e = e.WithThumbnail(v.Image)
	}
	if v.Type != "" {
		e = e.AddField("Type", v.Type, true)
	}
	if v.ReportedBy != "" {
		e = e.AddField("Reported by", truncate(v.ReportedBy, maxFieldValue), true)
	}
	if v.Where != "" {
		e = e.AddField("Affects", v.Where, true)
	}
	return e.AddField(fieldStatus, statusOpen, true)
}

func issueButtons(id int) []discord.InteractiveComponent {
	s := strconv.Itoa(id)
	return []discord.InteractiveComponent{
		discord.NewDangerButton("Replace", idReplace+s),
		discord.NewSuccessButton("Resolve", idResolve+s),
	}
}

func setOpen(e discord.Embed) discord.Embed {
	e.Color = colorOpen
	return setField(e, fieldStatus, statusOpen, true)
}

func setResolved(e discord.Embed) discord.Embed {
	e.Color = colorResolved
	return setField(e, fieldStatus, statusResolved, true)
}

// setField replaces the field called name, or adds it at the end.
func setField(e discord.Embed, name, value string, inline bool) discord.Embed {
	value = truncate(value, maxFieldValue)
	for i, f := range e.Fields {
		if f.Name == name {
			e.Fields[i].Value = value
			return e
		}
	}
	return e.AddField(name, value, inline)
}

// cleanEmbed copies the fields of an embed read back from Discord that can
// be sent again, leaving out read-only ones such as proxy URLs.
func cleanEmbed(in discord.Embed) discord.Embed {
	out := discord.Embed{
		Title:       in.Title,
		Description: in.Description,
		URL:         in.URL,
		Timestamp:   in.Timestamp,
		Color:       in.Color,
		Fields:      append([]discord.EmbedField(nil), in.Fields...),
	}
	if in.Footer != nil {
		out.Footer = &discord.EmbedFooter{Text: in.Footer.Text, IconURL: in.Footer.IconURL}
	}
	if in.Thumbnail != nil {
		out.Thumbnail = &discord.EmbedResource{URL: in.Thumbnail.URL}
	}
	if in.Image != nil {
		out.Image = &discord.EmbedResource{URL: in.Image.URL}
	}
	if in.Author != nil {
		out.Author = &discord.EmbedAuthor{Name: in.Author.Name, URL: in.Author.URL, IconURL: in.Author.IconURL}
	}
	return out
}

// typeName turns the webhook's issue_type (VIDEO, AUDIO, ...) into a label.
func typeName(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "VIDEO":
		return "Video"
	case "AUDIO":
		return "Audio"
	case "SUBTITLES":
		return "Subtitles"
	case "OTHER":
		return "Other"
	}
	return ""
}

func issueTypeName(t int) string {
	switch t {
	case seerr.IssueVideo:
		return "Video"
	case seerr.IssueAudio:
		return "Audio"
	case seerr.IssueSubtitles:
		return "Subtitles"
	case seerr.IssueOther:
		return "Other"
	}
	return ""
}

// where describes the part of a show an issue is about. Seerr uses 0 for
// "all" in both numbers.
func where(season, episode int) string {
	switch {
	case season > 0 && episode > 0:
		return fmt.Sprintf("Season %d, episode %d", season, episode)
	case season > 0:
		return fmt.Sprintf("Season %d (all episodes)", season)
	default:
		return "All seasons"
	}
}

// truncate cuts s to at most n runes, ending in an ellipsis if cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// lines joins as many lines as fit in limit runes, ending with an
// "and N more" line when some are left out.
func lines(ls []string, limit int) string {
	var out []string
	used := 0
	for i, l := range ls {
		n := len([]rune(l)) + 1
		reserve := 0 // room for the "and N more" line, unless this is the last
		if left := len(ls) - i - 1; left > 0 {
			reserve = len(fmt.Sprintf("*and %d more*", left))
		}
		if used+n+reserve > limit {
			out = append(out, fmt.Sprintf("*and %d more*", len(ls)-i))
			break
		}
		out = append(out, l)
		used += n
	}
	return strings.Join(out, "\n")
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
