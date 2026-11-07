package discord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/plex"
	"github.com/grunclepug/homelab-bot/internal/store"
)

// When a member the bot onboarded leaves the server or loses the media
// role, approvers are asked whether to remove their Plex share. Nothing
// changes in Plex until one of them presses Remove access.
const (
	idShareRemove = "access:unshare:"
	idShareKeep   = "access:keep:"

	textLeft     = "left the server"
	textLostRole = "no longer has the media role"
)

// memberLeft asks approvers about the Plex share of a member who left,
// unless they have already been asked. A share an approver chose to keep
// is asked about again: leaving is a new reason.
func (a *access) memberLeft(ctx context.Context, userID snowflake.ID) error {
	r, ok, err := a.store.Share(ctx, userID.String())
	if err != nil || !ok {
		return err
	}
	return a.askRemoval(ctx, r, textLeft, store.ShareActive, store.ShareKept)
}

// memberChanged acts on a member's roles changing. Losing the media role
// asks approvers about their Plex share, once. Getting it back clears a
// choice to keep the share, so losing it again asks again.
func (a *access) memberChanged(ctx context.Context, userID snowflake.ID, roles []snowflake.ID) error {
	r, ok, err := a.store.Share(ctx, userID.String())
	if err != nil || !ok {
		return err
	}
	if slices.Contains(roles, a.mediaRole) {
		if r.ShareState == store.ShareKept {
			_, err := a.store.SetShareState(ctx, r.ID, store.ShareActive, store.ShareKept)
			return err
		}
		return nil
	}
	return a.askRemoval(ctx, r, textLostRole, store.ShareActive)
}

// askRemoval posts the question about r's share if its state is one of
// from.
func (a *access) askRemoval(ctx context.Context, r store.AccessRequest, why string, from ...string) error {
	if got, err := a.store.SetShareState(ctx, r.ID, store.ShareAsked, from...); err != nil || !got {
		return err
	}
	m := discord.NewMessageCreate().
		WithEmbeds(shareEmbed(r, why)).
		AddActionRow(shareButtons(r.ID)...)
	if _, err := a.rest.CreateMessage(a.channel, m, rest.WithCtx(ctx)); err != nil {
		// Put the state back, so the next event asks again.
		undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()
		_, uerr := a.store.SetShareState(undo, r.ID, r.ShareState, store.ShareAsked)
		return errors.Join(fmt.Errorf("ask about plex share of access request %d: %w", r.ID, err), uerr)
	}
	a.log.Info("asked approvers about a plex share", "request", r.ID, "reason", why)
	return nil
}

func shareEmbed(r store.AccessRequest, why string) discord.Embed {
	return discord.NewEmbed().
		WithTitle("Remove Plex access?").
		WithColor(colorPending).
		WithDescription("<@"+r.UserID+"> ("+escape(r.UserName)+") "+why+". "+
			"Their Plex access stays until an approver removes it.").
		AddField("Plex account", code(r.Plex), true).
		AddField("Approved", "Request #"+strconv.Itoa(r.ID)+" by "+escape(orDefault(r.DecidedBy, "an approver")), true).
		AddField(fieldStatus, "Waiting for an approver", true)
}

func shareButtons(id int) []discord.InteractiveComponent {
	s := strconv.Itoa(id)
	return []discord.InteractiveComponent{
		discord.NewDangerButton("Remove access", idShareRemove+s),
		discord.NewSecondaryButton("Keep", idShareKeep+s),
	}
}

// removeShare removes the Plex share of request id and updates the
// question post msg. It returns the text to show the approver.
func (a *access) removeShare(ctx context.Context, id int, approver string, msg discord.Message) (string, error) {
	got, err := a.store.SetShareState(ctx, id, store.ShareRemoving, store.ShareAsked)
	if err != nil || !got {
		return textHandled, err
	}
	fin, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	r, ok, err := a.store.Request(fin, id)
	if err == nil && !ok {
		err = fmt.Errorf("access request %d not found", id)
	}
	if err != nil {
		_, uerr := a.store.SetShareState(fin, id, store.ShareAsked, store.ShareRemoving)
		return "", errors.Join(err, uerr)
	}
	removed, err := a.plex.RemoveAccess(ctx, plex.Account(r.PlexAccount))
	if err != nil {
		a.log.Warn("remove plex access failed", "request", id, "err", err)
		if _, uerr := a.store.SetShareState(fin, id, store.ShareAsked, store.ShareRemoving); uerr != nil {
			return "", errors.Join(err, uerr)
		}
		return "Couldn't remove their Plex access, nothing changed. Try again in a minute.\n" + err.Error(), nil
	}
	if _, err := a.store.SetShareState(fin, id, store.ShareRemoved, store.ShareRemoving); err != nil {
		return "", err
	}
	text, status := "Plex access removed.", "Plex access removed by "+escape(approver)
	if !removed {
		text, status = "They no longer had Plex access, so there was nothing to remove.", "Nothing to remove (checked by "+escape(approver)+")"
	}
	return text, a.settleShare(fin, msg, colorResolved, status)
}

// keepShare records that an approver chose to keep the share.
func (a *access) keepShare(ctx context.Context, id int, approver string, msg discord.Message) (string, error) {
	got, err := a.store.SetShareState(ctx, id, store.ShareKept, store.ShareAsked)
	if err != nil || !got {
		return textHandled, err
	}
	return "Kept. Their Plex access stays.", a.settleShare(ctx, msg, colorApproved, "Kept by "+escape(approver))
}

// settleShare shows the outcome on the question post and takes its
// buttons away.
func (a *access) settleShare(ctx context.Context, msg discord.Message, color int, status string) error {
	if len(msg.Embeds) == 0 {
		return nil
	}
	e := cleanEmbed(msg.Embeds[0])
	e.Color = color
	e = setField(e, fieldStatus, status, true)
	upd := discord.NewMessageUpdate().WithEmbeds(e).ClearComponents()
	if _, err := a.rest.UpdateMessage(msg.ChannelID, msg.ID, upd, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("edit plex share post: %w", err)
	}
	return nil
}

// recoverRemoving puts shares whose removal the bot stopped halfway back
// to asked. Their posts kept their buttons, and removing a share that is
// already gone succeeds, so pressing Remove access again finishes it.
func (a *access) recoverRemoving(ctx context.Context) error {
	rs, err := a.store.Removing(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		a.log.Info("reopening plex share removal interrupted by a restart", "request", r.ID)
		_, err := a.store.SetShareState(ctx, r.ID, store.ShareAsked, store.ShareRemoving)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
