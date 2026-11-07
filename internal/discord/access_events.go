package discord

import (
	"context"
	"strconv"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
)

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

	if id == idAccessRequest {
		why, err := b.access.canRequest(ctx, e.User().ID, hasRole(e.Member(), b.cfg.MediaRoleID))
		switch {
		case err != nil:
			b.log.Error("check access request", "err", err)
			b.ephemeral(e.CreateMessage, "Something went wrong, try again later.")
		case why != "":
			b.ephemeral(e.CreateMessage, why)
		default:
			if err := e.Modal(requestForm()); err != nil {
				b.log.Error("open access form", "err", err)
			}
		}
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
	text, err := run(ctx, reqID, e.Member().EffectiveName())
	if err != nil {
		b.log.Error("access decision", "request", reqID, "err", err)
		text = strings.TrimSpace(text + "\nSomething went wrong: " + err.Error())
	}
	b.respondText(ctx, e.Token(), text)
}

// onModal handles the access request form.
func (b *Bot) onModal(e *events.ModalSubmitInteractionCreate) {
	if e.Data.CustomID != idAccessForm || !b.inGuild(e.GuildID()) || e.Member() == nil {
		return
	}
	if err := e.DeferCreateMessage(true); err != nil {
		b.log.Error("defer access form", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	text, err := b.access.submit(ctx, e.User().ID, e.Member().EffectiveName(),
		e.Data.Text(inputPlex), e.Data.Text(inputNote))
	if err != nil {
		b.log.Error("submit access request", "err", err)
		text = "Couldn't send your request right now. Please try again later."
	}
	b.respond(ctx, e.Token(), discord.NewMessageUpdate().WithContent(text))
}
