package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/store"
)

// Custom IDs for the access request flow.
const (
	idAccessRequest = "access:request"
	idAccessForm    = "access:form"
	idAccessApprove = "access:approve:"
	idAccessDeny    = "access:deny:"

	inputPlex = "plex"
	inputNote = "note"
)

const (
	colorApproved = 0x2ECC71
	colorPending  = 0xF1C40F
)

// accessREST is the part of disgo's REST client the access flow uses.
type accessREST interface {
	messenger
	AddMemberRole(guildID, userID, roleID snowflake.ID, opts ...rest.RequestOpt) error
	CreateDMChannel(userID snowflake.ID, opts ...rest.RequestOpt) (*discord.DMChannel, error)
}

type inviter interface {
	Invite(ctx context.Context, who string) error
}

type accessStore interface {
	CreateRequest(ctx context.Context, r store.AccessRequest) (int, error)
	DeleteRequest(ctx context.Context, id int) error
	SetRequestPost(ctx context.Context, id int, channelID, messageID string) error
	OpenRequest(ctx context.Context, userID string) (bool, error)
	Request(ctx context.Context, id int) (store.AccessRequest, bool, error)
	Claim(ctx context.Context, id int, from, to string) (bool, error)
	Decide(ctx context.Context, id int, status, decidedBy string) error
}

// access runs access requests: members ask, approvers approve or deny,
// and an approval onboards the member.
type access struct {
	guild     snowflake.ID
	channel   snowflake.ID // where requests are posted
	mediaRole snowflake.ID
	seerrURL  string // public Seerr URL for the welcome DM; may be empty
	rest      accessREST
	store     accessStore
	plex      inviter
	log       *slog.Logger
}

// Texts members see.
const (
	textHasAccess     = "You already have access to the media server."
	textOpenRequest   = "You already have a request waiting for an approver."
	textRequestSent   = "Request sent. You'll get a DM from me once an approver has looked at it."
	textApproversOnly = "Only approvers can do that."
	textHandled       = "This request has already been handled."
)

// panelMessage is the message /access-panel posts.
func panelMessage() discord.MessageCreate {
	e := discord.NewEmbed().
		WithTitle("Media server access").
		WithDescription("Want to watch on Plex and request movies and shows? " +
			"Press the button, tell us your Plex account, and an approver will set you up.").
		WithColor(colorQueue)
	return discord.NewMessageCreate().
		WithEmbeds(e).
		AddActionRow(discord.NewPrimaryButton("Request access", idAccessRequest))
}

// requestForm is the modal a member fills in.
func requestForm() discord.ModalCreate {
	return discord.NewModalCreate(idAccessForm, "Request media server access").
		AddLabel("Plex email or username", discord.NewShortTextInput(inputPlex).
			WithRequired(true).WithMinLength(2).WithMaxLength(100).
			WithPlaceholder("The account we should invite")).
		AddLabel("Anything the approvers should know?", discord.NewParagraphTextInput(inputNote).
			WithRequired(false).WithMaxLength(500).
			WithPlaceholder("Optional, for example who invited you"))
}

// canRequest says whether a member may open the request form, and if not,
// why. hasMedia is whether they already have the media role.
func (a *access) canRequest(ctx context.Context, userID snowflake.ID, hasMedia bool) (string, error) {
	if hasMedia {
		return textHasAccess, nil
	}
	open, err := a.store.OpenRequest(ctx, userID.String())
	if err != nil {
		return "", err
	}
	if open {
		return textOpenRequest, nil
	}
	return "", nil
}

// submit records a request and posts it for the approvers. It returns the
// text to show the member.
func (a *access) submit(ctx context.Context, userID snowflake.ID, userName, plexAccount, note string) (string, error) {
	plexAccount = strings.TrimSpace(plexAccount)
	note = strings.TrimSpace(note)
	r := store.AccessRequest{UserID: userID.String(), UserName: userName, Plex: plexAccount, Note: note}
	id, err := a.store.CreateRequest(ctx, r)
	if errors.Is(err, store.ErrOpenRequest) {
		return textOpenRequest, nil
	}
	if err != nil {
		return "", err
	}
	r.ID = id
	m := discord.NewMessageCreate().
		WithEmbeds(requestEmbed(r)).
		AddActionRow(requestButtons(id)...)
	msg, err := a.rest.CreateMessage(a.channel, m, rest.WithCtx(ctx))
	if err != nil {
		// Without a post nobody can act on it, so drop it and let the
		// member try again.
		return "", errors.Join(fmt.Errorf("post access request %d: %w", id, err), a.store.DeleteRequest(ctx, id))
	}
	if err := a.store.SetRequestPost(ctx, id, msg.ChannelID.String(), msg.ID.String()); err != nil {
		return "", err
	}
	return textRequestSent, nil
}

// approve onboards the member: Plex invite, media role, welcome DM. If the
// invite fails nothing else happens and the request stays pending, so it
// can be tried again. It returns the text to show the approver.
func (a *access) approve(ctx context.Context, id int, approver string) (string, error) {
	r, err := a.claim(ctx, id)
	if err != nil || r == nil {
		return textHandled, err
	}
	userID, err := snowflake.Parse(r.UserID)
	if err != nil {
		return "", errors.Join(fmt.Errorf("access request %d: bad user ID: %w", id, err), a.release(ctx, id))
	}

	if err := a.plex.Invite(ctx, r.Plex); err != nil {
		a.log.Warn("plex invite failed", "request", id, "err", err)
		note := fmt.Sprintf("%s tried to approve: the Plex invite failed (%v). Check the Plex account and try again.", approver, err)
		return "The Plex invite failed, so nothing else was done and the request is still open:\n" + err.Error(),
			errors.Join(a.release(ctx, id), a.updatePost(ctx, *r, func(e discord.Embed) discord.Embed {
				return setField(e, "Last attempt", note, false)
			}, true))
	}

	steps := []string{"Plex invite sent to " + code(r.Plex)}
	if err := a.rest.AddMemberRole(a.guild, userID, a.mediaRole, rest.WithCtx(ctx), rest.WithReason("Access request approved by "+approver)); err != nil {
		a.log.Warn("add media role failed", "request", id, "err", err)
		steps = append(steps, "Couldn't add the media role, add it by hand ("+err.Error()+")")
	} else {
		steps = append(steps, "Media role added")
	}
	if err := a.dm(ctx, userID, a.welcome()); err != nil {
		a.log.Warn("welcome DM failed", "request", id, "err", err)
		steps = append(steps, "Couldn't DM the welcome message, they may have DMs turned off")
	} else {
		steps = append(steps, "Welcome DM sent")
	}

	if err := a.store.Decide(ctx, id, store.StatusApproved, approver); err != nil {
		return "", err
	}
	body := strings.Join(steps, "\n")
	err = a.updatePost(ctx, *r, func(e discord.Embed) discord.Embed {
		e.Color = colorApproved
		e = setField(e, fieldStatus, "Approved by "+approver, true)
		return setField(e, "Onboarding", body, false)
	}, false)
	return "Approved.\n" + body, err
}

// deny closes the request and tells the member. It returns the text to
// show the approver.
func (a *access) deny(ctx context.Context, id int, approver string) (string, error) {
	r, err := a.claim(ctx, id)
	if err != nil || r == nil {
		return textHandled, err
	}
	if err := a.store.Decide(ctx, id, store.StatusDenied, approver); err != nil {
		return "", err
	}
	reply := "Denied."
	dmNote := "They were told by DM."
	if userID, err := snowflake.Parse(r.UserID); err != nil || a.dm(ctx, userID,
		"Your request for access to the media server was declined. Ask an admin if you think that's a mistake.") != nil {
		dmNote = "Couldn't DM them, they may have DMs turned off."
	}
	err = a.updatePost(ctx, *r, func(e discord.Embed) discord.Embed {
		e.Color = colorResolved
		e = setField(e, fieldStatus, "Denied by "+approver, true)
		return setField(e, "Notified", dmNote, false)
	}, false)
	return reply + " " + dmNote, err
}

// claim marks a pending request as being acted on. It returns nil if the
// request isn't pending, for example because another approver got there
// first.
func (a *access) claim(ctx context.Context, id int) (*store.AccessRequest, error) {
	ok, err := a.store.Claim(ctx, id, store.StatusPending, store.StatusProcessing)
	if err != nil || !ok {
		return nil, err
	}
	r, found, err := a.store.Request(ctx, id)
	if err != nil {
		return nil, errors.Join(err, a.release(ctx, id))
	}
	if !found {
		return nil, nil
	}
	return &r, nil
}

// release puts a claimed request back to pending.
func (a *access) release(ctx context.Context, id int) error {
	_, err := a.store.Claim(ctx, id, store.StatusProcessing, store.StatusPending)
	return err
}

func (a *access) dm(ctx context.Context, userID snowflake.ID, text string) error {
	ch, err := a.rest.CreateDMChannel(userID, rest.WithCtx(ctx))
	if err != nil {
		return fmt.Errorf("open DM: %w", err)
	}
	if _, err := a.rest.CreateMessage(ch.ID(), discord.NewMessageCreate().WithContent(text), rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("send DM: %w", err)
	}
	return nil
}

func (a *access) welcome() string {
	seerr := "2. Sign in to Seerr with your Plex account to request movies and shows."
	if a.seerrURL != "" {
		seerr = "2. Sign in to Seerr at " + a.seerrURL + " with your Plex account to request movies and shows."
	}
	return strings.Join([]string{
		"**You're in!** Your request for media server access was approved.",
		"1. Accept the Plex invite. It's in your email, or under Settings, Manage Library Access, at https://app.plex.tv.",
		seerr,
		"3. Use `/queue` in the server to see what's downloading.",
	}, "\n")
}

// updatePost edits a request's approval post. A missing post is logged,
// not an error: the request itself is already settled.
func (a *access) updatePost(ctx context.Context, r store.AccessRequest, change func(discord.Embed) discord.Embed, buttons bool) error {
	ch, err1 := snowflake.Parse(r.ChannelID)
	mid, err2 := snowflake.Parse(r.MessageID)
	if err1 != nil || err2 != nil {
		a.log.Warn("access request has no post to update", "request", r.ID)
		return nil
	}
	msg, err := a.rest.GetMessage(ch, mid, rest.WithCtx(ctx))
	if isNotFound(err) {
		a.log.Warn("access request post was deleted", "request", r.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get post for access request %d: %w", r.ID, err)
	}
	e := requestEmbed(r)
	if len(msg.Embeds) > 0 {
		e = cleanEmbed(msg.Embeds[0])
	}
	upd := discord.NewMessageUpdate().WithEmbeds(change(e))
	if buttons {
		upd = upd.WithComponents(discord.NewActionRow(requestButtons(r.ID)...))
	} else {
		upd = upd.ClearComponents()
	}
	if _, err := a.rest.UpdateMessage(ch, mid, upd, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("edit post for access request %d: %w", r.ID, err)
	}
	return nil
}

func requestEmbed(r store.AccessRequest) discord.Embed {
	e := discord.NewEmbed().
		WithTitle("Access request #"+strconv.Itoa(r.ID)).
		WithColor(colorPending).
		WithTimestamp(time.Now()).
		AddField("Member", "<@"+r.UserID+"> ("+escape(r.UserName)+")", true).
		AddField("Plex account", code(r.Plex), true).
		AddField(fieldStatus, "Waiting for an approver", true)
	if r.Note != "" {
		e = e.WithDescription(truncate(r.Note, maxDescription))
	}
	return e
}

func requestButtons(id int) []discord.InteractiveComponent {
	s := strconv.Itoa(id)
	return []discord.InteractiveComponent{
		discord.NewSuccessButton("Approve", idAccessApprove+s),
		discord.NewDangerButton("Deny", idAccessDeny+s),
	}
}

var markdownEscaper = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, "`", "\\`", `~`, `\~`, `|`, `\|`, `>`, `\>`,
)

// escape stops Discord formatting user-typed text.
func escape(s string) string { return markdownEscaper.Replace(s) }

// code shows user-typed text as inline code, so it copies exactly.
func code(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }
