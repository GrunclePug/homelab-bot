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

// When an onboarded member leaves or loses the media role, approvers get a
// card asking whether to remove their Plex access. Nothing changes in Plex
// until one of them presses Remove access.
const (
	idShareRemove = "access:unshare:"
	idShareKeep   = "access:keep:"

	textLeft     = "left the server"
	textLostRole = "no longer has the media role"
)

// memberLeft asks about each account tracked for a member who left. Access
// an approver kept is asked about again: leaving is a new reason.
func (a *access) memberLeft(ctx context.Context, userID snowflake.ID) error {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
	rs, err := a.store.Shares(ctx, userID.String())
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		errs = append(errs, a.askRemoval(ctx, r, textLeft, store.ShareActive, store.ShareKept))
	}
	return errors.Join(errs...)
}

// memberChanged acts on a member's roles changing from old to roles;
// oldKnown is false if the bot hadn't seen the member yet. Losing the media
// role asks about their access, getting it back retires the card.
func (a *access) memberChanged(ctx context.Context, userID snowflake.ID, old []snowflake.ID, oldKnown bool, roles []snowflake.ID) error {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
	// Events run concurrently: skip one a newer change has overtaken.
	hasMedia := slices.Contains(roles, a.mediaRole)
	if now, ok := a.rolesNow(userID); ok && slices.Contains(now, a.mediaRole) != hasMedia {
		return nil
	}
	// Only a change that took the role away counts.
	if !hasMedia && oldKnown && !slices.Contains(old, a.mediaRole) {
		return nil
	}
	rs, err := a.store.Shares(ctx, userID.String())
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rs {
		if hasMedia {
			errs = append(errs, a.roleBack(ctx, r))
		} else {
			errs = append(errs, a.askRemoval(ctx, r, textLostRole, store.ShareActive))
		}
	}
	return errors.Join(errs...)
}

func (a *access) rolesNow(userID snowflake.ID) ([]snowflake.ID, bool) {
	if a.currentRoles == nil {
		return nil, false
	}
	return a.currentRoles(userID)
}

// roleBack settles r for a member who has the media role again. Callers
// hold shareMu.
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

// retireOlder retires the cards of the member's approvals before r. One for
// the same account hands its tracking to r; one for another account stays
// tracked, as that account may still have access.
func (a *access) retireOlder(ctx context.Context, r store.AccessRequest) error {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
	olds, err := a.store.OlderShares(ctx, r.UserID, r.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range olds {
		to := store.ShareActive
		if r.PlexAccount != "" && o.PlexAccount == r.PlexAccount {
			to = store.ShareSuperseded
		}
		// A removal in progress checks for newer approvals itself.
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

// askRemoval posts r's card if its state is one of from, or again if it
// was asked about and the card was deleted. Callers hold shareMu.
func (a *access) askRemoval(ctx context.Context, r store.AccessRequest, why string, from ...string) error {
	moved := false
	switch {
	case r.ShareState == store.ShareAsked:
		if a.cardExists(ctx, r) {
			return nil
		}
		a.log.Info("plex share card was deleted, posting it again", "request", r.ID)
	case slices.Contains(from, r.ShareState):
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
			// So the next event asks again.
			undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
			defer cancel()
			_, uerr := a.store.SetShareState(undo, r.ID, r.ShareState, store.ShareAsked)
			err = errors.Join(err, uerr)
		}
		return err
	}
	if err := a.store.SetSharePost(ctx, r.ID, msg.ChannelID.String(), msg.ID.String()); err != nil {
		// The card still works; it just can't be retired.
		a.log.Error("record plex share card", "request", r.ID, "err", err)
	}
	a.log.Info("asked approvers about a plex share", "request", r.ID, "reason", why)
	return nil
}

// cardExists reports whether r's card is still there. When it can't tell,
// it says yes, so a card is never posted twice.
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
	return !isNotFound(err)
}

// retireCard marks r's card as no longer applying and takes its buttons
// away, if the card is still there.
func (a *access) retireCard(ctx context.Context, r store.AccessRequest, status string) error {
	ch, err1 := snowflake.Parse(r.ShareChannelID)
	mid, err2 := snowflake.Parse(r.ShareMessageID)
	if err1 != nil || err2 != nil {
		return nil
	}
	a.log.Info("retiring plex share card", "request", r.ID, "why", status)
	msg, err := a.rest.GetMessage(ch, mid, rest.WithCtx(ctx))
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get plex share card of access request %d: %w", r.ID, err)
	}
	return a.settleShare(ctx, *msg, colorResolved, status)
}

// checkMissed catches up on tracked members whose leaving or role changes
// the bot missed while it was down.
func (a *access) checkMissed(ctx context.Context) error {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
	rs, err := a.store.TrackedShares(ctx)
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
			errs = append(errs, a.askRemoval(ctx, r, textLeft, store.ShareActive, store.ShareKept))
		case err != nil:
			errs = append(errs, fmt.Errorf("check member of access request %d: %w", r.ID, err))
		case slices.Contains(m.RoleIDs, a.mediaRole):
			errs = append(errs, a.roleBack(ctx, r))
		default:
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

// removeShare removes request id's Plex access and settles its card msg.
// It returns the text for the approver.
func (a *access) removeShare(ctx context.Context, id int, approver string, msg discord.Message) (string, error) {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
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
	// An old card mustn't remove access a newer approval gave.
	if newer, ok, err := a.store.NewerApproval(fin, r.UserID, id, r.PlexAccount); err != nil || ok {
		_, uerr := a.store.SetShareState(fin, id, store.ShareAsked, store.ShareRemoving)
		if err != nil || uerr != nil {
			return "", errors.Join(err, uerr)
		}
		return "This card is out of date: they were approved again in request #" + strconv.Itoa(newer) + ". Nothing changed.", nil
	}
	if userID, err := snowflake.Parse(r.UserID); err == nil {
		if now, ok := a.rolesNow(userID); ok && slices.Contains(now, a.mediaRole) {
			_, uerr := a.store.SetShareState(fin, id, store.ShareActive, store.ShareRemoving)
			if uerr != nil {
				return "", uerr
			}
			return "They have the media role again, so nothing changed.",
				a.settleShare(fin, msg, colorResolved, "They have the media role again, so this no longer applies")
		}
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
		// Removed by hand, or a lookup that missed.
		text = "Couldn't find a share or invite for them on plex.tv. If they still have access, remove it by hand under Manage Library Access in Plex."
		status = "Not found on plex.tv (checked by " + escape(approver) + ")"
	}
	return text, a.settleShare(fin, msg, colorResolved, status)
}

// keepShare records that an approver kept the access.
func (a *access) keepShare(ctx context.Context, id int, approver string, msg discord.Message) (string, error) {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
	got, err := a.store.SetShareState(ctx, id, store.ShareKept, store.ShareAsked)
	if err != nil || !got {
		return textHandled, err
	}
	a.log.Info("plex access kept", "request", id, "by", approver)
	return "Kept. Their Plex access stays.", a.settleShare(ctx, msg, colorApproved, "Kept by "+escape(approver))
}

// settleShare shows the outcome on a card and takes its buttons away.
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

// recoverRemoving reopens removals a restart interrupted. Their cards kept
// their buttons, so pressing Remove access again finishes them.
func (a *access) recoverRemoving(ctx context.Context) error {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
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
