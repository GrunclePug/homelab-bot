package discord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"

	"github.com/grunclepug/homelab-bot/internal/qbit"
	"github.com/grunclepug/homelab-bot/internal/replace"
)

// Time limits for the work behind an interaction. Discord lets a deferred
// response be edited for 15 minutes.
const (
	lookupTimeout  = 1 * time.Minute
	replaceTimeout = 5 * time.Minute
)

const (
	textAdminsOnly = "Only admins can do that."
	textIssueRoles = "Only admins and approvers can do that."
	textMediaOnly  = "You need the media role to use /queue."
	textExpired    = "This has expired. Press Replace on the issue post again."
)

// A message holds up to maxMenus select menus, one per action row, each
// with up to maxMenuOptions options.
const (
	maxMenus       = 5
	maxMenuOptions = 25
)

func (b *Bot) onComponent(e *events.ComponentInteractionCreate) {
	if !b.enter() {
		b.ephemeral(e.CreateMessage, textRestarting)
		return
	}
	defer b.handlers.Done()
	if !b.inGuild(e.GuildID()) {
		b.ephemeral(e.CreateMessage, "This bot only works in its own server.")
		return
	}
	id := e.Data.CustomID()
	if strings.HasPrefix(id, "access:") {
		b.accessComponent(e, id)
		return
	}
	if id == idStatusRefresh {
		b.statusRefreshed(e)
		return
	}
	// The issue buttons: Replace, Resolve and the Replace flow's own.
	if !hasRole(e.Member(), b.cfg.AdminRoleID, b.cfg.ApproverRoleID) {
		b.ephemeral(e.CreateMessage, textIssueRoles)
		return
	}
	switch {
	case strings.HasPrefix(id, idReplace):
		b.replacePressed(e, strings.TrimPrefix(id, idReplace))
	case strings.HasPrefix(id, idResolve):
		b.resolvePressed(e, strings.TrimPrefix(id, idResolve))
	case strings.HasPrefix(id, idPick):
		token, _, _ := strings.Cut(strings.TrimPrefix(id, idPick), ":")
		b.picked(e, token)
	case strings.HasPrefix(id, idConfirm):
		b.confirmed(e, strings.TrimPrefix(id, idConfirm))
	case strings.HasPrefix(id, idCancel):
		b.cancelled(e, strings.TrimPrefix(id, idCancel))
	default:
		b.log.Warn("unknown component", "custom_id", id)
		b.ephemeral(e.CreateMessage, "Unknown button.")
	}
}

// respond edits the deferred response of an interaction.
func (b *Bot) respond(ctx context.Context, appToken string, upd discord.MessageUpdate) {
	if _, err := b.client.Rest.UpdateInteractionResponse(b.client.ApplicationID, appToken, upd, rest.WithCtx(ctx)); err != nil {
		b.log.Error("edit interaction response", "err", err)
	}
}

func (b *Bot) respondText(ctx context.Context, appToken, text string) {
	b.respond(ctx, appToken, discord.NewMessageUpdate().WithContent(truncate(text, 2000)).ClearComponents().ClearEmbeds())
}

// problem describes an error for the admin and logs anything unexpected.
func (b *Bot) problem(what string, err error) string {
	if ue, ok := errors.AsType[*replace.UserError](err); ok {
		return ue.Msg
	}
	b.log.Error(what, "err", err)
	return "Something went wrong: " + err.Error()
}

func (b *Bot) replacePressed(e *events.ComponentInteractionCreate, arg string) {
	issueID, err := strconv.Atoi(arg)
	if err != nil {
		b.ephemeral(e.CreateMessage, "This button has a bad issue ID.")
		return
	}
	if err := e.DeferCreateMessage(true); err != nil {
		b.log.Error("defer replace", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()

	is, err := b.seerr.Issue(ctx, issueID)
	if err != nil {
		b.respondText(ctx, e.Token(), b.problem("replace: fetch issue", err))
		return
	}
	plan, err := b.replace.Lookup(ctx, is)
	if err != nil {
		b.respondText(ctx, e.Token(), b.problem("replace: look up files", err))
		return
	}
	p := &pendingReplace{IssueID: issueID, AdminID: e.User().ID, Plan: plan}
	if !plan.NeedsPick() {
		p.Chosen = plan.Candidates
	}
	token := b.pending.put(p)
	if plan.NeedsPick() {
		b.respond(ctx, e.Token(), pickView(plan, token))
		return
	}
	b.respond(ctx, e.Token(), confirmView(plan.Title, p.Chosen, token))
}

func (b *Bot) picked(e *events.ComponentInteractionCreate, token string) {
	p, ok := b.pending.get(token)
	if !ok {
		b.updateText(e, textExpired)
		return
	}
	chosen := replace.Select(p.Plan.Candidates, e.StringSelectMenuInteractionData().Values)
	if len(chosen) == 0 || !b.pending.setChosen(token, chosen) {
		b.updateText(e, textExpired)
		return
	}
	if err := e.UpdateMessage(confirmView(p.Plan.Title, chosen, token)); err != nil {
		b.log.Error("show replace confirmation", "err", err)
	}
}

func (b *Bot) cancelled(e *events.ComponentInteractionCreate, token string) {
	b.pending.take(token)
	b.updateText(e, "Cancelled. Nothing was changed.")
}

// updateText replaces the message a component is on with plain text.
func (b *Bot) updateText(e *events.ComponentInteractionCreate, text string) {
	upd := discord.NewMessageUpdate().WithContent(text).ClearComponents().ClearEmbeds()
	if err := e.UpdateMessage(upd); err != nil {
		b.log.Error("update message", "err", err)
	}
}

func (b *Bot) confirmed(e *events.ComponentInteractionCreate, token string) {
	p, ok := b.pending.take(token)
	if !ok || len(p.Chosen) == 0 {
		b.updateText(e, textExpired)
		return
	}
	if !b.pending.start(p.IssueID) {
		b.updateText(e, "A replacement for this issue is already running. Wait for it to finish, then press Replace again if needed.")
		return
	}
	defer b.pending.finish(p.IssueID)
	b.updateText(e, fmt.Sprintf("Replacing %s…", plural(fileCount(p.Chosen), "file")))

	ctx, cancel := context.WithTimeout(context.Background(), replaceTimeout)
	defer cancel()
	b.answer(e.Token(), b.finishReplace(ctx, p, e.Member().EffectiveName()))
}

// finishReplace runs a confirmed replacement and reports it on the issue
// post and in Seerr. It returns the text for the admin.
func (b *Bot) finishReplace(ctx context.Context, p *pendingReplace, admin string) string {
	results := b.replace.Run(ctx, p.Chosen)

	summaries := make([]string, len(results))
	var started []string
	for i, r := range results {
		summaries[i] = "• " + r.Summary()
		if r.Deleted() {
			started = append(started, r.Target.Label)
		}
		// The summaries give short reasons; the details go to the log.
		if err := r.Err(); err != nil {
			b.log.Warn("replace step failed", "issue", p.IssueID, "target", r.Target.Label, "err", err)
		}
	}

	var notes []string
	if err := b.reportOnPost(ctx, p.IssueID, admin, summaries); err != nil {
		notes = append(notes, b.problem("replace: update issue post", err))
	}
	if len(started) > 0 {
		msg := fmt.Sprintf("Replacement started by %s from Discord: the current file was deleted and a new one is being searched for (%s).",
			admin, strings.Join(started, ", "))
		if err := b.seerr.Comment(ctx, p.IssueID, truncate(msg, 2000)); err != nil {
			notes = append(notes, "Couldn't comment on the Seerr issue: "+b.problem("replace: comment on issue", err))
		}
	}

	head := "**Replacement started.** Resolve the issue once the new file is in."
	if len(started) == 0 {
		head = "**Nothing was replaced.**"
	}
	text := head + "\n" + lines(summaries, 1500)
	if len(notes) > 0 {
		text += "\n\n" + strings.Join(notes, "\n")
	}
	return text
}

// fileCount counts the distinct files among targets.
func fileCount(targets []replace.Target) int {
	files := map[int]bool{}
	for _, t := range targets {
		files[t.FileID] = true
	}
	return len(files)
}

// reportOnPost adds the replacement's results to the issue post.
func (b *Bot) reportOnPost(ctx context.Context, issueID int, admin string, summaries []string) error {
	b.relay.mu.Lock()
	defer b.relay.mu.Unlock()
	msg, err := b.relay.existing(ctx, issueID)
	if err != nil {
		return err
	}
	if msg == nil || len(msg.Embeds) == 0 {
		return fmt.Errorf("no post recorded for issue %d", issueID)
	}
	head := "Replacement started by " + escape(admin)
	body := head + "\n" + lines(summaries, maxFieldValue-len([]rune(head))-1)
	open := true
	for _, f := range msg.Embeds[0].Fields {
		if f.Name == fieldStatus && f.Value == statusResolved {
			open = false
		}
	}
	return b.relay.edit(ctx, msg, func(e discord.Embed) discord.Embed {
		return setField(e, fieldReplace, body, false)
	}, open)
}

func (b *Bot) resolvePressed(e *events.ComponentInteractionCreate, arg string) {
	issueID, err := strconv.Atoi(arg)
	if err != nil {
		b.ephemeral(e.CreateMessage, "This button has a bad issue ID.")
		return
	}
	if err := e.DeferCreateMessage(true); err != nil {
		b.log.Error("defer resolve", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	if err := b.seerr.Resolve(ctx, issueID); err != nil {
		b.respondText(ctx, e.Token(), b.problem("resolve issue", err))
		return
	}
	b.respondText(ctx, e.Token(), "Resolved in Seerr. The post updates when Seerr confirms it.")
}

func (b *Bot) queue(e *events.ApplicationCommandInteractionCreate) {
	if !hasRole(e.Member(), b.cfg.MediaRoleID, b.cfg.AdminRoleID) {
		b.ephemeral(e.CreateMessage, textMediaOnly)
		return
	}
	if err := e.DeferCreateMessage(true); err != nil {
		b.log.Error("defer queue", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ts, err := b.qbit.Torrents(ctx)
	if err != nil {
		b.log.Error("queue", "err", err)
		b.respondText(ctx, e.Token(), "Couldn't reach qBittorrent right now.")
		return
	}
	b.respond(ctx, e.Token(), discord.NewMessageUpdate().WithEmbeds(queueEmbed(qbit.Build(ts, qbit.MaxRows))))
}

func queueEmbed(q qbit.Queue) discord.Embed {
	e := discord.NewEmbed().
		WithTitle("Download queue").
		WithColor(colorQueue).
		WithDescription(truncate(q.Text(), maxDescription))
	if f := q.Footer(); f != "" {
		e = e.WithFooterText(f)
	}
	return e
}

// pickView asks the admin which files of a season or show to replace.
func pickView(p replace.Plan, token string) discord.MessageUpdate {
	var opts []discord.StringSelectMenuOption
	var prompt string
	switch p.Scope {
	case replace.ScopeShow:
		seasons := replace.Seasons(p.Candidates)
		prompt = fmt.Sprintf("**%s**: the issue is about the whole show. Pick the seasons to replace.", p.Title)
		opts = append(opts, discord.NewStringSelectMenuOption("All of them", replace.PickAll).
			WithDescription(fmt.Sprintf("%s across %s", plural(len(p.Candidates), "episode"), plural(len(seasons), "season"))))
		for _, s := range seasons {
			label := "Season " + strconv.Itoa(s.Number)
			if s.Number == 0 {
				label = "Specials"
			}
			opts = append(opts, discord.NewStringSelectMenuOption(label, replace.PickSeason(s.Number)).
				WithDescription(plural(s.Files, "episode")+" with files"))
		}
	default:
		prompt = fmt.Sprintf("**%s**: the issue is about season %d. Pick the episodes to replace.", p.Title, p.Season)
		opts = append(opts, discord.NewStringSelectMenuOption("All of them", replace.PickAll).
			WithDescription(plural(len(p.Candidates), "episode")+" in season "+strconv.Itoa(p.Season)))
		for _, t := range p.Candidates {
			opts = append(opts, discord.NewStringSelectMenuOption(truncate(t.Label, maxLabel), replace.PickItem(t)).
				WithDescription(truncate(fileInfo(t), maxLabel)))
		}
	}

	var rows []discord.LayoutComponent
	for chunk := range slices.Chunk(opts, maxMenuOptions) {
		if len(rows) == maxMenus {
			break
		}
		menu := discord.NewStringSelectMenu(idPick+token+":"+strconv.Itoa(len(rows)), "Choose…", chunk...).
			WithMinValues(1).
			WithMaxValues(len(chunk))
		rows = append(rows, discord.NewActionRow(menu))
	}
	if len(rows) > 1 {
		prompt += "\nEach menu is a separate pick: choose from one menu, or use \"All of them\"."
	}
	if shown := maxMenus * maxMenuOptions; len(opts) > shown {
		prompt += fmt.Sprintf("\nOnly the first %d choices fit here; use \"All of them\" for the rest.", shown-1)
	}
	return discord.NewMessageUpdate().WithContent(prompt).WithComponents(rows...)
}

// confirmView lists the files about to be replaced, with Replace and
// Cancel buttons.
func confirmView(title string, chosen []replace.Target, token string) discord.MessageUpdate {
	ls := make([]string, len(chosen))
	shared := replace.SharedFile(chosen)
	for i, t := range chosen {
		if shared[i] != "" {
			ls[i] = "• **" + t.Label + "**: same file as " + shared[i]
			continue
		}
		ls[i] = "• **" + t.Label + "**: " + fileInfo(t)
	}
	e := discord.NewEmbed().
		WithTitle(truncate("Replace "+plural(fileCount(chosen), "file")+" of "+title+"?", 256)).
		WithColor(colorOpen).
		WithDescription(lines(ls, maxDescription)).
		WithFooterText("Each release is blocklisted, the file deleted and a new one searched for.")
	return discord.NewMessageUpdate().
		WithContent("").
		WithEmbeds(e).
		WithComponents(discord.NewActionRow(
			discord.NewDangerButton("Replace", idConfirm+token),
			discord.NewSecondaryButton("Cancel", idCancel+token),
		))
}

// fileInfo describes a file: quality, release group and size.
func fileInfo(t replace.Target) string {
	var parts []string
	if t.Quality != "" {
		parts = append(parts, t.Quality)
	}
	if t.Group != "" {
		parts = append(parts, t.Group)
	}
	if t.Size > 0 {
		parts = append(parts, qbit.Bytes(t.Size))
	}
	if len(parts) == 0 {
		return "no file details"
	}
	return strings.Join(parts, " · ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
