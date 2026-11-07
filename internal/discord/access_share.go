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

// memberLeft asks approvers about the Plex access of a member who left,
// unless they have already been asked and the card is still there. Access
// an approver chose to keep is asked about again: leaving is a new reason.
func (a *access) memberLeft(ctx context.Context, userID snowflake.ID) error {
	r, ok, err := a.store.Share(ctx, userID.String())
	if err != nil || !ok {
		return err
	}
	return a.askRemoval(ctx, r, textLeft, store.ShareActive, store.ShareKept)
}

// memberChanged acts on a member's roles changing from old (oldKnown is
// false if the bot hadn't seen the member yet) to roles. Losing the media
// role asks approvers about their Plex access, once. Getting it back
// retires an open card and clears a choice to keep it, so losing it again
// asks again.
func (a *access) memberChanged(ctx context.Context, userID snowflake.ID, old []snowflake.ID, oldKnown bool, roles []snowflake.ID) error {
	r, ok, err := a.store.Share(ctx, userID.String())
	if err != nil || !ok {
		return err
	}
	if slices.Contains(roles, a.mediaRole) {
		return a.roleBack(ctx, r)
	}
	// Only a change that took the role away counts: a member whose role
	// was never added, say, may just have changed their nickname.
	if oldKnown && !slices.Contains(old, a.mediaRole) {
		return nil
	}
	// Events are handled concurrently, so a later one may already have
	// given the role back.
	if a.currentRoles != nil {
		if now, ok := a.currentRoles(userID); ok && slices.Contains(now, a.mediaRole) {
			return nil
		}
	}
	return a.askRemoval(ctx, r, textLostRole, store.ShareActive)
}

// roleBack settles r's access for a member who has the media role again.
func (a *access) roleBack(ctx context.Context, r store.AccessRequest) error {
	switch r.ShareState {
	case store.ShareKept:
		_, err := a.store.SetShareState(ctx, r.ID, store.ShareActive, store.ShareKept)
		return err
	case store.ShareAsked:
		got, err := a.store.SetShareState(ctx, r.ID, store.ShareActive, store.ShareAsked)
		if err != nil || !got {
			return err
		}
		return a.retireCard(ctx, r, "They have the media role again, so this no longer applies")
	}
	return nil
}

// retireOlder settles the member's earlier approvals once r, a newer one,
// is approved: their open cards no longer apply. If r tracks the access,
// it takes over from them; if not, they stay tracked, as the member's
// earlier account may still have access.
func (a *access) retireOlder(ctx context.Context, r store.AccessRequest) error {
	olds, err := a.store.OlderShares(ctx, r.UserID, r.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range olds {
		to := store.ShareActive
		if r.PlexAccount != "" {
			to = store.ShareSuperseded
		}
		// A removal being carried out is left to finish.
		if o.ShareState == store.ShareRemoving || o.ShareState == to {
			continue
		}
		got, err := a.store.SetShareState(ctx, o.ID, to, o.ShareState)
		if err != nil || !got {
			errs = append(errs, err)
			continue
		}
		if o.ShareState == store.ShareAsked {
			errs = append(errs, a.retireCard(ctx, o, "Approved again in request #"+strconv.Itoa(r.ID)+", so this no longer applies"))
		}
	}
	return errors.Join(errs...)
}

// askRemoval posts the card about r's access if its state is one of from.
// If approvers were already asked, it posts the card again only if someone
// deleted it.
func (a *access) askRemoval(ctx context.Context, r store.AccessRequest, why string, from ...string) error {
	moved := false
	switch {
	case r.ShareState == store.ShareAsked:
		if a.cardExists(ctx, r) {
			return nil
		}
		a.log.Info("plex share card was deleted, posting it again", "request", r.ID)
	case slices.Contains(from, r.ShareState):
		// From exactly the state read, so an undo restores the right one.
		got, err := a.store.SetShareState(ctx, r.ID, store.ShareAsked, r.ShareState)
		if err != nil || !got {
			return err
		}
		moved = true
	default:
		return nil
	}
	m := discord.NewMessageCreate().
		WithEmbeds(shareEmbed(r, why)).
		AddActionRow(shareButtons(r.ID)...)
	msg, err := a.rest.CreateMessage(a.channel, m, rest.WithCtx(ctx))
	if err != nil {
		err = fmt.Errorf("ask about plex share of access request %d: %w", r.ID, err)
		if moved {
			// Put the state back, so the next event asks again.
			undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
			defer cancel()
			_, uerr := a.store.SetShareState(undo, r.ID, r.ShareState, store.ShareAsked)
			err = errors.Join(err, uerr)
		}
		return err
	}
	if err := a.store.SetSharePost(ctx, r.ID, msg.ChannelID.String(), msg.ID.String()); err != nil {
		// The card works without it; it just can't be retired or found
		// again.
		a.log.Error("record plex share card", "request", r.ID, "err", err)
	}
	a.log.Info("asked approvers about a plex share", "request", r.ID, "reason", why)
	return nil
}

// cardExists reports whether r's card is still there. When that can't be
// told, it says yes, so a card is never posted twice.
func (a *access) cardExists(ctx context.Context, r store.AccessRequest) bool {
	ch, err1 := snowflake.Parse(r.ShareChannelID)
	mid, err2 := snowflake.Parse(r.ShareMessageID)
	if err1 != nil || err2 != nil {
		return true
	}
	_, err := a.rest.GetMessage(ch, mid, rest.WithCtx(ctx))
	if err != nil && !isNotFound(err) {
		a.log.Warn("check plex share card", "request", r.ID, "err", err)
	}
	return err == nil || !isNotFound(err)
}

// retireCard shows on r's card that it no longer applies and takes its
// buttons away. A card that is gone, or wasn't recorded, needs nothing.
func (a *access) retireCard(ctx context.Context, r store.AccessRequest, status string) error {
	a.log.Info("retiring plex share card", "request", r.ID, "why", status)
	ch, err1 := snowflake.Parse(r.ShareChannelID)
	mid, err2 := snowflake.Parse(r.ShareMessageID)
	if err1 != nil || err2 != nil {
		return nil
	}
	msg, err := a.rest.GetMessage(ch, mid, rest.WithCtx(ctx))
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get plex share card of access request %d: %w", r.ID, err)
	}
	return a.settleShare(ctx, *msg, colorResolved, status)
}

// checkMissed asks about members who left or lost the media role while
// the bot wasn't running, or was shutting down.
func (a *access) checkMissed(ctx context.Context) error {
	rs, err := a.store.ActiveShares(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		userID, err := snowflake.Parse(r.UserID)
		if err != nil {
			errs = append(errs, fmt.Errorf("access request %d: bad user ID: %w", r.ID, err))
			continue
		}
		m, err := a.rest.GetMember(a.guild, userID, rest.WithCtx(ctx))
		switch {
		case isNotFound(err):
			errs = append(errs, a.askRemoval(ctx, r, textLeft, store.ShareActive))
		case err != nil:
			errs = append(errs, fmt.Errorf("check member of access request %d: %w", r.ID, err))
		case !slices.Contains(m.RoleIDs, a.mediaRole):
			errs = append(errs, a.askRemoval(ctx, r, textLostRole, store.ShareActive))
		}
	}
	return errors.Join(errs...)
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
	// A card left over from before the member was approved again must
	// not remove the access that approval gave.
	if latest, ok, err := a.store.Share(fin, r.UserID); err != nil || (ok && latest.ID != id) {
		_, uerr := a.store.SetShareState(fin, id, store.ShareAsked, store.ShareRemoving)
		if err != nil || uerr != nil {
			return "", errors.Join(err, uerr)
		}
		return "This card is out of date: they were approved again in request #" + strconv.Itoa(latest.ID) + ". Nothing changed.", nil
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
	a.log.Info("plex access removal", "request", id, "by", approver, "found", removed)
	text, status := "Plex access removed.", "Plex access removed by "+escape(approver)
	if !removed {
		// Removed by hand already, or a lookup that missed: say which
		// it might be, rather than claim there is nothing left.
		text = "Couldn't find a share or invite for them on plex.tv. If they still have access, remove it by hand under Manage Library Access in Plex."
		status = "Not found on plex.tv (checked by " + escape(approver) + ")"
	}
	return text, a.settleShare(fin, msg, colorResolved, status)
}

// keepShare records that an approver chose to keep the share.
func (a *access) keepShare(ctx context.Context, id int, approver string, msg discord.Message) (string, error) {
	got, err := a.store.SetShareState(ctx, id, store.ShareKept, store.ShareAsked)
	if err != nil || !got {
		return textHandled, err
	}
	a.log.Info("plex access kept", "request", id, "by", approver)
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
