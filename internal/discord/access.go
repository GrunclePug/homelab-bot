package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/plex"
	"github.com/grunclepug/homelab-bot/internal/store"
)

// Custom IDs for the access request flow.
const (
	idAccessRequest = "access:request"
	idAccessForm    = "access:form"      // opened from the panel
	idAccessReform  = "access:form:edit" // reopened from a review's Edit button
	idAccessApprove = "access:approve:"
	idAccessDeny    = "access:deny:"

	inputPlex = "plex"
	inputNote = "note"
)

// finishTimeout bounds the steps that close a request, which run on their
// own context so a request is never left half-handled.
const finishTimeout = 30 * time.Second

const (
	colorApproved = 0x2ECC71
	colorPending  = 0xF1C40F
)

// accessREST is the part of disgo's REST client the access flow uses.
type accessREST interface {
	messenger
	DeleteMessage(channelID, messageID snowflake.ID, opts ...rest.RequestOpt) error
	AddMemberRole(guildID, userID, roleID snowflake.ID, opts ...rest.RequestOpt) error
	CreateDMChannel(userID snowflake.ID, opts ...rest.RequestOpt) (*discord.DMChannel, error)
	GetMember(guildID, userID snowflake.ID, opts ...rest.RequestOpt) (*discord.Member, error)
}

// plexSharer invites accounts to the Plex server and removes their access.
type plexSharer interface {
	Invite(ctx context.Context, who string) (plex.Account, error)
	RemoveAccess(ctx context.Context, acct plex.Account) (bool, error)
}

type accessStore interface {
	CreateRequest(ctx context.Context, r store.AccessRequest) (int, error)
	DeleteRequest(ctx context.Context, id int) error
	SetRequestPost(ctx context.Context, id int, channelID, messageID string) error
	OpenRequest(ctx context.Context, userID string) (bool, error)
	Request(ctx context.Context, id int) (store.AccessRequest, bool, error)
	Processing(ctx context.Context) ([]store.AccessRequest, error)
	Claim(ctx context.Context, id int, from, to, by string) (bool, error)
	MarkInvited(ctx context.Context, id int, plexAccount string, already bool) error
	Decide(ctx context.Context, id int, status, decidedBy, plex string) error
	Shares(ctx context.Context, userID string) ([]store.AccessRequest, error)
	NewerApproval(ctx context.Context, userID string, id int, plexAccount string) (int, bool, error)
	SetShareState(ctx context.Context, id int, to string, from ...string) (bool, error)
	Removing(ctx context.Context) ([]store.AccessRequest, error)
	SetSharePost(ctx context.Context, id int, channelID, messageID string) error
	HadAccount(ctx context.Context, userID, plexAccount string, before int) (bool, error)
	OlderShares(ctx context.Context, userID string, newer int) ([]store.AccessRequest, error)
	TrackedShares(ctx context.Context) ([]store.AccessRequest, error)
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
	plex      plexSharer
	log       *slog.Logger

	// currentRoles returns a member's roles as the bot last saw them, if
	// it has seen them; nil to skip that check.
	currentRoles func(snowflake.ID) ([]snowflake.ID, bool)

	// shareMu serialises the Plex share cards' state changes: member
	// events and clicks run concurrently.
	shareMu sync.Mutex
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

// requestForm is the modal a member fills in, prefilled from d when they
// come back to edit it. customID says where it was opened from.
func requestForm(customID string, d draft) discord.ModalCreate {
	plexInput := discord.NewShortTextInput(inputPlex).
		WithRequired(true).WithMinLength(2).WithMaxLength(100).
		WithPlaceholder("The account we should invite")
	noteInput := discord.NewParagraphTextInput(inputNote).
		WithRequired(false).WithMaxLength(500).
		WithPlaceholder("Optional, for example who invited you")
	if d.Plex != "" {
		plexInput = plexInput.WithValue(d.Plex)
	}
	if d.Note != "" {
		noteInput = noteInput.WithValue(d.Note)
	}
	return discord.NewModalCreate(customID, "Request media server access").
		AddLabel("Plex email or username", plexInput).
		AddLabel("Anything the approvers should know?", noteInput)
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
	// Undoing a half-made request must work even if ctx has run out, or
	// the member would be left with an open request nobody can act on.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	msg, err := a.rest.CreateMessage(a.channel, m, rest.WithCtx(ctx))
	if err != nil {
		return "", errors.Join(fmt.Errorf("post access request %d: %w", id, err), a.store.DeleteRequest(cleanup, id))
	}
	if err := a.store.SetRequestPost(ctx, id, msg.ChannelID.String(), msg.ID.String()); err != nil {
		// A post the bot can't find again can't be updated, so take both
		// back and let the member try again.
		return "", errors.Join(err,
			a.rest.DeleteMessage(msg.ChannelID, msg.ID, rest.WithCtx(cleanup)),
			a.store.DeleteRequest(cleanup, id))
	}
	return textRequestSent, nil
}

// approve onboards the member: Plex invite, media role, welcome DM. If the
// invite fails nothing else happens: the request is closed as failed and
// the member is asked to check their Plex account and request again. It
// returns the text to show the approver.
func (a *access) approve(ctx context.Context, id int, approver string) (string, error) {
	r, err := a.claim(ctx, id, approver)
	if err != nil || r == nil {
		return textHandled, err
	}
	// However ctx ends, the request mustn't be left in processing, so the
	// steps after the invite get their own time.
	fin, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	userID, err := snowflake.Parse(r.UserID)
	if err != nil {
		return "", errors.Join(fmt.Errorf("access request %d: bad user ID: %w", id, err), a.release(fin, id))
	}

	acct, err := a.plex.Invite(ctx, r.Plex)
	// Neither is a failure: the account has access either way. A member
	// can already have it after losing the media role and asking again.
	already, unknown := errors.Is(err, plex.ErrAlreadyShared), errors.Is(err, plex.ErrAccountUnknown)
	if already || unknown {
		a.log.Info("plex invite", "request", id, "already_shared", already, "account_unknown", unknown, "err", err)
		err = nil
	}
	if err != nil {
		a.log.Warn("plex invite failed", "request", id, "err", err)
		return a.inviteFailed(fin, *r, userID, approver, err)
	}
	var notes []string
	if already && acct != "" {
		// Track only an account this member was approved for before, as
		// after losing the media role and asking again: an account that
		// is someone else's must never be removable through this member.
		own, err := a.store.HadAccount(fin, r.UserID, string(acct), id)
		if err != nil {
			a.log.Error("check earlier plex access", "request", id, "err", err)
		}
		if !own {
			acct = ""
			notes = append(notes, "This Plex account had access before this request, so I won't offer to remove it. Remove it by hand in Plex if needed")
		}
	}
	if unknown {
		notes = append(notes, "Couldn't find their Plex access on plex.tv, so if they leave, remove it by hand")
	}
	if err := a.store.MarkInvited(fin, id, string(acct), already); err != nil {
		// Carry on: the member is invited. Only a restart before the end
		// would now reopen the request instead of finishing it.
		a.log.Error("record plex invite", "request", id, "err", err)
	}
	r.PlexAccount, r.PlexAlready = string(acct), already
	return a.onboard(fin, *r, userID, approver, notes...)
}

// onboard finishes an approval once the account has Plex access: media
// role, welcome DM, then the request is closed as approved. notes are
// added to the list of steps on the post.
func (a *access) onboard(ctx context.Context, r store.AccessRequest, userID snowflake.ID, approver string, notes ...string) (string, error) {
	id := r.ID
	steps := []string{"Plex invite sent to " + code(maskAccount(r.Plex))}
	if r.PlexAlready {
		steps[0] = code(maskAccount(r.Plex)) + " already had Plex access, so no new invite was sent"
	}
	if err := a.rest.AddMemberRole(a.guild, userID, a.mediaRole, rest.WithCtx(ctx), rest.WithReason("Access request approved by "+approver)); err != nil {
		a.log.Warn("add media role failed", "request", id, "err", err)
		steps = append(steps, "Couldn't add the media role, add it by hand ("+err.Error()+")")
	} else {
		steps = append(steps, "Media role added")
	}
	if err := a.dm(ctx, userID, a.welcome(r.PlexAlready)); err != nil {
		a.log.Warn("welcome DM failed", "request", id, "err", err)
		steps = append(steps, "Couldn't DM the welcome message, they may have DMs turned off")
	} else {
		steps = append(steps, "Welcome DM sent")
	}
	steps = append(steps, notes...)

	if err := a.store.Decide(ctx, id, store.StatusApproved, approver, maskAccount(r.Plex)); err != nil {
		return "", err
	}
	// Now that this approval counts, cards about earlier ones no longer
	// apply. This runs after a restart too, through recoverInterrupted.
	if err := a.retireOlder(ctx, r); err != nil {
		a.log.Error("retire earlier plex share cards", "request", id, "err", err)
	}
	body := strings.Join(steps, "\n")
	err := a.updatePost(ctx, r, func(e discord.Embed) discord.Embed {
		e.Color = colorApproved
		e = setField(e, fieldStatus, "Approved by "+escape(approver), true)
		return setField(e, "Onboarding", body, false)
	}, false)
	return "Approved.\n" + body, err
}

// inviteFailed closes a request whose Plex invite failed, so the member
// isn't stuck with an open request, and asks them to try again.
func (a *access) inviteFailed(ctx context.Context, r store.AccessRequest, userID snowflake.ID, approver string, inviteErr error) (string, error) {
	if err := a.store.Decide(ctx, r.ID, store.StatusFailed, approver, maskAccount(r.Plex)); err != nil {
		return "", err
	}
	dmNote := "They were asked by DM to check their Plex account and request again."
	if a.dm(ctx, userID, "The Plex invite for your media server access request didn't go through. "+
		"Check your Plex email address or username, then press Request access again.") != nil {
		dmNote = "Couldn't DM them, they may have DMs turned off. Ask them to check their Plex account and request again."
	}
	err := a.updatePost(ctx, r, func(e discord.Embed) discord.Embed {
		e.Color = colorResolved
		e = setField(e, fieldStatus, "Invite failed (approved by "+escape(approver)+")", true)
		return setField(e, "Problem", truncate(inviteErr.Error(), maxFieldValue)+"\n"+dmNote, false)
	}, false)
	return "The Plex invite failed, so nothing else was done and the request is closed:\n" + inviteErr.Error() + "\n" + dmNote, err
}

// deny closes the request and tells the member. It returns the text to
// show the approver.
func (a *access) deny(ctx context.Context, id int, approver string) (string, error) {
	r, err := a.claim(ctx, id, approver)
	if err != nil || r == nil {
		return textHandled, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := a.store.Decide(ctx, id, store.StatusDenied, approver, maskAccount(r.Plex)); err != nil {
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
		e = setField(e, fieldStatus, "Denied by "+escape(approver), true)
		return setField(e, "Notified", dmNote, false)
	}, false)
	return reply + " " + dmNote, err
}

// claim marks a pending request as being acted on. It returns nil if the
// request isn't pending, for example because another approver got there
// first.
func (a *access) claim(ctx context.Context, id int, approver string) (*store.AccessRequest, error) {
	ok, err := a.store.Claim(ctx, id, store.StatusPending, store.StatusProcessing, approver)
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
	_, err := a.store.Claim(ctx, id, store.StatusProcessing, store.StatusPending, "")
	return err
}

// textInterrupted is noted on a request the bot stopped handling halfway.
const textInterrupted = "The bot restarted before this was handled, so nothing was done. Press Approve or Deny again."

// recoverInterrupted settles the requests left in processing when the bot
// stopped halfway through a click. One whose Plex invite went out is
// onboarded; any other goes back to pending for the approver to try again.
func (a *access) recoverInterrupted(ctx context.Context) error {
	rs, err := a.store.Processing(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		if !r.InvitedAt.IsZero() {
			userID, err := snowflake.Parse(r.UserID)
			if err != nil {
				errs = append(errs, fmt.Errorf("access request %d: bad user ID: %w", r.ID, err))
				continue
			}
			a.log.Info("finishing access request interrupted after its invite", "request", r.ID)
			_, err = a.onboard(ctx, r, userID, orDefault(r.DecidedBy, "an approver"), "Finished after the bot restarted")
			errs = append(errs, err)
			continue
		}
		a.log.Info("reopening access request interrupted before its invite", "request", r.ID)
		if err := a.release(ctx, r.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, a.updatePost(ctx, r, func(e discord.Embed) discord.Embed {
			return setField(e, "Note", textInterrupted, false)
		}, true))
	}
	return errors.Join(errs...)
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

// welcome is the DM an approved member gets. already is whether their
// account had access before, so no new invite was sent.
func (a *access) welcome(already bool) string {
	plexStep := "1. Accept the Plex invite. It's in your email, or under Settings, Manage Library Access, at https://app.plex.tv."
	if already {
		plexStep = "1. Your Plex account already had access. If you haven't accepted its invite yet, it's in your email, or under Settings, Manage Library Access, at https://app.plex.tv."
	}
	seerr := "2. Sign in to Seerr with your Plex account to request movies and shows."
	if a.seerrURL != "" {
		seerr = "2. Sign in to Seerr at " + a.seerrURL + " with your Plex account to request movies and shows."
	}
	return strings.Join([]string{
		"**You're in!** Your request for media server access was approved.",
		plexStep,
		seerr,
		"3. Use `/help` in the server to see what else you can do, such as `/queue` for what's downloading.",
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
		AddField("Plex account", code(maskAccount(r.Plex)), true).
		AddField(fieldStatus, "Waiting for an approver", true)
	if r.Note != "" {
		// Escaped, so a note can't show approvers a disguised link.
		e = e.WithDescription(truncate(escape(r.Note), maxDescription))
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
	`[`, `\[`, `]`, `\]`, `#`, `\#`,
)

// escape stops Discord formatting user-typed text, including masked links
// ([text](url)) and headings.
func escape(s string) string { return markdownEscaper.Replace(s) }

// maskAccount hides most of an email address, so nobody reading a post
// learns it: discord2@example.com becomes d•••@e•••.com. A Plex username
// is shown as it is.
func maskAccount(s string) string {
	local, domain, ok := strings.Cut(s, "@")
	if !ok {
		return s
	}
	masked := maskPart(local) + "@"
	if i := strings.LastIndex(domain, "."); i > 0 {
		return masked + maskPart(domain[:i]) + domain[i:]
	}
	return masked + maskPart(domain)
}

func maskPart(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return "•••"
	}
	return string(r[0]) + "•••"
}

// code shows user-typed text as inline code, so it copies exactly.
func code(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }
