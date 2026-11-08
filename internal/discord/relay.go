package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/seerr"
	"github.com/grunclepug/homelab-bot/internal/store"
)

// messenger is the part of disgo's REST client the relay uses.
type messenger interface {
	GetMessage(channelID, messageID snowflake.ID, opts ...rest.RequestOpt) (*discord.Message, error)
	CreateMessage(channelID snowflake.ID, m discord.MessageCreate, opts ...rest.RequestOpt) (*discord.Message, error)
	UpdateMessage(channelID, messageID snowflake.ID, m discord.MessageUpdate, opts ...rest.RequestOpt) (*discord.Message, error)
}

type postStore interface {
	Post(ctx context.Context, issueID int) (store.Post, bool, error)
	SavePost(ctx context.Context, p store.Post) error
}

type issueFetcher interface {
	Issue(ctx context.Context, id int) (seerr.Issue, error)
}

// relay turns Seerr issue events into posts in the support channel.
type relay struct {
	channel snowflake.ID
	msg     messenger
	store   postStore
	issues  issueFetcher
	log     *slog.Logger

	// mu serialises events, so a comment can't race the post its issue is
	// still creating.
	mu sync.Mutex
}

// testMessage is posted for Seerr's Test button.
const testMessage = "Seerr test notification received."

// handle acts on one webhook payload.
func (r *relay) handle(ctx context.Context, p seerr.Payload) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p.NotificationType == seerr.NotifyTest {
		_, err := r.msg.CreateMessage(r.channel, discord.NewMessageCreate().WithContent(testMessage), rest.WithCtx(ctx))
		if err != nil {
			return fmt.Errorf("post test message: %w", err)
		}
		return nil
	}

	switch p.NotificationType {
	case seerr.NotifyIssueCreated, seerr.NotifyIssueComment, seerr.NotifyIssueResolved, seerr.NotifyIssueReopened:
	default:
		return fmt.Errorf("%w: unsupported notification_type %q", seerr.ErrBadPayload, p.NotificationType)
	}
	id, err := p.Issue()
	if err != nil {
		return fmt.Errorf("%w: %v", seerr.ErrBadPayload, err)
	}

	// Only the issue itself has the season and episode; the post goes up
	// without them if it can't be fetched.
	is, err := r.issues.Issue(ctx, id)
	var fetched *seerr.Issue
	if err != nil {
		r.log.Warn("fetch issue for post; posting without episode details", "issue", id, "err", err)
	} else {
		fetched = &is
	}
	view := newIssueView(id, p, fetched)

	switch p.NotificationType {
	case seerr.NotifyIssueCreated:
		msg, err := r.existing(ctx, id)
		if err != nil {
			return err
		}
		if msg != nil {
			// Seen before: bring the post up to date rather than post twice.
			return r.edit(ctx, msg, setOpen, true)
		}
		_, err = r.create(ctx, view)
		return err

	case seerr.NotifyIssueComment:
		msg, err := r.ensure(ctx, view)
		if err != nil {
			return err
		}
		return r.reply(ctx, msg, p)

	case seerr.NotifyIssueResolved:
		msg, err := r.ensure(ctx, view)
		if err != nil {
			return err
		}
		return r.edit(ctx, msg, setResolved, false)

	default: // reopened
		msg, err := r.ensure(ctx, view)
		if err != nil {
			return err
		}
		return r.edit(ctx, msg, setOpen, true)
	}
}

// existing returns the post recorded for an issue, or nil if there is none
// or it has been deleted from Discord.
func (r *relay) existing(ctx context.Context, issueID int) (*discord.Message, error) {
	p, ok, err := r.store.Post(ctx, issueID)
	if err != nil || !ok {
		return nil, err
	}
	ch, err1 := snowflake.Parse(p.ChannelID)
	mid, err2 := snowflake.Parse(p.MessageID)
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("stored post for issue %d: %w", issueID, err)
	}
	msg, err := r.msg.GetMessage(ch, mid, rest.WithCtx(ctx))
	if isNotFound(err) {
		r.log.Info("issue post was deleted from Discord; posting a new one", "issue", issueID)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get post for issue %d: %w", issueID, err)
	}
	return msg, nil
}

// ensure returns the issue's post, creating it if the issue has none, as
// for one filed before the bot was set up.
func (r *relay) ensure(ctx context.Context, v issueView) (*discord.Message, error) {
	msg, err := r.existing(ctx, v.ID)
	if err != nil || msg != nil {
		return msg, err
	}
	return r.create(ctx, v)
}

func (r *relay) create(ctx context.Context, v issueView) (*discord.Message, error) {
	m := discord.NewMessageCreate().
		WithEmbeds(v.embed()).
		AddActionRow(issueButtons(v.ID)...)
	msg, err := r.msg.CreateMessage(r.channel, m, rest.WithCtx(ctx))
	if err != nil {
		return nil, fmt.Errorf("post issue %d: %w", v.ID, err)
	}
	err = r.store.SavePost(ctx, store.Post{
		IssueID:   v.ID,
		ChannelID: msg.ChannelID.String(),
		MessageID: msg.ID.String(),
		CreatedAt: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// edit rewrites a post's embed with change, and shows or removes the
// issue buttons.
func (r *relay) edit(ctx context.Context, msg *discord.Message, change func(discord.Embed) discord.Embed, buttons bool) error {
	if len(msg.Embeds) == 0 {
		return fmt.Errorf("post %s has no embed", msg.ID)
	}
	upd := discord.NewMessageUpdate().WithEmbeds(change(cleanEmbed(msg.Embeds[0])))
	if id, ok := issueIDFromEmbed(msg.Embeds[0]); ok && buttons {
		upd = upd.WithComponents(discord.NewActionRow(issueButtons(id)...))
	} else {
		upd = upd.ClearComponents()
	}
	if _, err := r.msg.UpdateMessage(msg.ChannelID, msg.ID, upd, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("edit post %s: %w", msg.ID, err)
	}
	return nil
}

func (r *relay) reply(ctx context.Context, msg *discord.Message, p seerr.Payload) error {
	who := escape(orDefault(p.CommentedBy, "Someone"))
	text := strings.TrimSpace(p.Comment)
	if text == "" {
		text = "(empty comment)"
	}
	content := truncate(fmt.Sprintf("**%s** commented:\n%s", who, quote(text)), 2000)
	m := discord.NewMessageCreate().
		WithContent(content).
		WithMessageReference(&discord.MessageReference{MessageID: &msg.ID, ChannelID: &msg.ChannelID})
	if _, err := r.msg.CreateMessage(msg.ChannelID, m, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("reply to post %s: %w", msg.ID, err)
	}
	return nil
}

// isNotFound reports whether a REST error is a 404.
func isNotFound(err error) bool {
	re, ok := errors.AsType[*rest.Error](err)
	return ok && re.Response != nil && re.Response.StatusCode == http.StatusNotFound
}

// issueIDFromEmbed reads the issue ID back from a post's footer.
func issueIDFromEmbed(e discord.Embed) (int, bool) {
	if e.Footer == nil {
		return 0, false
	}
	s, ok := strings.CutPrefix(e.Footer.Text, footerPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(s)
	return id, err == nil
}

func quote(s string) string {
	return "> " + strings.ReplaceAll(s, "\n", "\n> ")
}
