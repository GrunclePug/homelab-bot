package discord

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
)

// decisionTimeout bounds an Approve or Deny click.
const decisionTimeout = 2 * time.Minute

// answer edits an interaction's deferred reply on a fresh context, so a
// slow step before it can't stop the member or approver being told.
func (b *Bot) answer(token, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b.respondText(ctx, token, text)
}

// accessPanel posts the request panel in the channel the command was run in.
func (b *Bot) accessPanel(e *events.ApplicationCommandInteractionCreate) {
	if !hasRole(e.Member(), b.cfg.AdminRoleID) {
		b.ephemeral(e.CreateMessage, textAdminsOnly)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	if _, err := b.client.Rest.CreateMessage(e.Channel().ID(), panelMessage(), rest.WithCtx(ctx)); err != nil {
		b.log.Error("post access panel", "err", err)
		b.ephemeral(e.CreateMessage, "Couldn't post the panel here: "+err.Error())
		return
	}
	b.ephemeral(e.CreateMessage, "Access panel posted. Pin it so new members find it.")
}

// accessComponent handles the panel's button and the approval buttons.
func (b *Bot) accessComponent(e *events.ComponentInteractionCreate, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()

	switch {
	case id == idAccessRequest:
		why, err := b.access.canRequest(ctx, e.User().ID, hasRole(e.Member(), b.cfg.MediaRoleID))
		switch {
		case err != nil:
			b.log.Error("check access request", "err", err)
			b.ephemeral(e.CreateMessage, "Something went wrong, try again later.")
		case why != "":
			b.ephemeral(e.CreateMessage, why)
		default:
			if err := e.Modal(requestForm(idAccessForm, draft{})); err != nil {
				b.log.Error("open access form", "err", err)
			}
		}
		return

	case strings.HasPrefix(id, idAccessEdit):
		d, ok := b.drafts.get(strings.TrimPrefix(id, idAccessEdit), e.User().ID)
		if !ok {
			b.updateText(e, textDraftExpired)
			return
		}
		if err := e.Modal(requestForm(idAccessReform, d)); err != nil {
			b.log.Error("reopen access form", "err", err)
		}
		return

	case strings.HasPrefix(id, idAccessSend):
		d, ok := b.drafts.take(strings.TrimPrefix(id, idAccessSend), e.User().ID)
		if !ok {
			b.updateText(e, textDraftExpired)
			return
		}
		if hasRole(e.Member(), b.cfg.MediaRoleID) {
			b.updateText(e, textHasAccess)
			return
		}
		if err := e.DeferUpdateMessage(); err != nil {
			b.log.Error("defer access send", "err", err)
			return
		}
		text, err := b.access.submit(ctx, d.UserID, e.Member().EffectiveName(), d.Plex, d.Note)
		if err != nil {
			b.log.Error("submit access request", "err", err)
			text = "Couldn't send your request right now. Please try again later."
		}
		b.answer(e.Token(), text)
		return
	}

	if !hasRole(e.Member(), b.cfg.ApproverRoleID) {
		b.ephemeral(e.CreateMessage, textApproversOnly)
		return
	}
	var (
		run func(context.Context, int, string) (string, error)
		arg string
	)
	switch {
	case strings.HasPrefix(id, idAccessApprove):
		run, arg = b.access.approve, strings.TrimPrefix(id, idAccessApprove)
	case strings.HasPrefix(id, idAccessDeny):
		run, arg = b.access.deny, strings.TrimPrefix(id, idAccessDeny)
	default:
		b.ephemeral(e.CreateMessage, "Unknown button.")
		return
	}
	reqID, err := strconv.Atoi(arg)
	if err != nil {
		b.ephemeral(e.CreateMessage, "This button has a bad request ID.")
		return
	}
	if err := e.DeferCreateMessage(true); err != nil {
		b.log.Error("defer access decision", "err", err)
		return
	}
	// A Plex invite alone may take 40 seconds; the deferred reply can be
	// edited for 15 minutes.
	dctx, dcancel := context.WithTimeout(context.Background(), decisionTimeout)
	defer dcancel()
	text, err := run(dctx, reqID, e.Member().EffectiveName())
	if err != nil {
		b.log.Error("access decision", "request", reqID, "err", err)
		text = strings.TrimSpace(text + "\nSomething went wrong: " + err.Error())
	}
	b.answer(e.Token(), text)
}

// onModal handles the access request form.
func (b *Bot) onModal(e *events.ModalSubmitInteractionCreate) {
	if !b.enter() {
		b.ephemeral(e.CreateMessage, textRestarting)
		return
	}
	defer b.handlers.Done()
	fromEdit := e.Data.CustomID == idAccessReform
	if (e.Data.CustomID != idAccessForm && !fromEdit) || !b.inGuild(e.GuildID()) || e.Member() == nil {
		return
	}
	// Nothing is sent yet: the member first checks what they typed.
	d := draft{
		UserID: e.User().ID,
		Plex:   strings.TrimSpace(e.Data.Text(inputPlex)),
		Note:   strings.TrimSpace(e.Data.Text(inputNote)),
	}
	token := b.drafts.put(d)
	review := reviewMessage(d, token, checkPlexAccount(d.Plex))
	// A form reopened from a review's Edit button replaces that private
	// review. One opened from the public panel gets a new private message:
	// updating its message would overwrite the panel.
	var err error
	if fromEdit {
		err = e.UpdateMessage(discord.NewMessageUpdate().
			WithContent(review.Content).
			WithComponents(review.Components...))
	} else {
		err = e.CreateMessage(review)
	}
	if err != nil {
		b.log.Error("show access review", "err", err)
	}
}
