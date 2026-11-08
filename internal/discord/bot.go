// Package discord runs the bot's Discord side: commands, issue posts,
// access requests and their buttons.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/plex"
	"github.com/grunclepug/homelab-bot/internal/qbit"
	"github.com/grunclepug/homelab-bot/internal/replace"
	"github.com/grunclepug/homelab-bot/internal/seerr"
	"github.com/grunclepug/homelab-bot/internal/store"
)

// Config is what the bot needs from the configuration.
type Config struct {
	Token            string
	GuildID          snowflake.ID
	SupportChannelID snowflake.ID
	AdminRoleID      snowflake.ID
	MediaRoleID      snowflake.ID
	AccessChannelID  snowflake.ID
	ApproverRoleID   snowflake.ID
	SeerrPublicURL   string
}

// seerrAPI is the part of the Seerr client the buttons use.
type seerrAPI interface {
	Issue(ctx context.Context, id int) (seerr.Issue, error)
	Comment(ctx context.Context, id int, message string) error
	Resolve(ctx context.Context, id int) error
}

// Bot is the Discord bot.
type Bot struct {
	cfg     Config
	client  *bot.Client
	seerr   seerrAPI
	replace replace.Service
	qbit    *qbit.Client
	relay   *relay
	access  *access
	sources StatusSources
	drafts  *draftStore
	pending *pendingStore
	log     *slog.Logger

	// Running handlers, so shutdown can let them finish: disgo runs each
	// in its own goroutine and doesn't wait for them.
	mu       sync.Mutex
	closing  bool
	handlers sync.WaitGroup
}

// textRestarting answers a click that comes in while the bot shuts down.
const textRestarting = "The bot is restarting. Try again in a minute."

// enter registers a starting handler, which calls b.handlers.Done when it
// returns. Once shutdown has begun it returns false, and the handler
// should only answer textRestarting.
func (b *Bot) enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return false
	}
	b.handlers.Add(1)
	return true
}

// Drain stops taking new interactions and waits until the running ones
// have finished, or ctx ends. Call it before Close.
func (b *Bot) Drain(ctx context.Context) error {
	b.mu.Lock()
	b.closing = true
	b.mu.Unlock()
	done := make(chan struct{})
	// If ctx ends first, this goroutine is left waiting as the process exits.
	go func() {
		b.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("interaction handlers still running: %w", ctx.Err())
	}
}

// New builds the bot. It doesn't connect until Start.
func New(cfg Config, st *store.Store, sc *seerr.Client, rs replace.Service, qc *qbit.Client, pc *plex.Client, sources StatusSources, log *slog.Logger) (*Bot, error) {
	b := &Bot{
		cfg:     cfg,
		sources: sources,
		seerr:   sc,
		replace: rs,
		qbit:    qc,
		pending: newPendingStore(),
		drafts:  newDraftStore(),
		log:     log,
	}
	none := discord.AllowedMentions{
		Parse: []discord.AllowedMentionType{},
		Roles: []snowflake.ID{},
		Users: []snowflake.ID{},
	}
	client, err := disgo.New(cfg.Token,
		// At debug level disgo logs REST bodies, which hold members' email
		// addresses and interaction tokens.
		bot.WithLogger(slog.New(atLeast{Handler: log.Handler(), min: slog.LevelInfo}).With("component", "disgo")),
		// Guild members (privileged) brings leaves and role changes; the
		// member cache, off by default, gives updates the roles from
		// before, and the largest threshold fills it on connect.
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildMembers),
			gateway.WithLargeThreshold(250),
		),
		bot.WithCacheConfigOpts(cache.WithCaches(cache.FlagMembers)),
		bot.WithEventManagerConfigOpts(bot.WithAsyncEventsEnabled()),
		bot.WithRestConfigOpts(rest.WithDefaultAllowedMentions(none)),
		bot.WithEventListenerFunc(b.onCommand),
		bot.WithEventListenerFunc(b.onComponent),
		bot.WithEventListenerFunc(b.onModal),
		bot.WithEventListenerFunc(b.onMemberLeave),
		bot.WithEventListenerFunc(b.onMemberUpdate),
	)
	if err != nil {
		return nil, fmt.Errorf("create discord client: %w", err)
	}
	b.client = client
	b.relay = &relay{
		channel: cfg.SupportChannelID,
		msg:     client.Rest,
		store:   st,
		issues:  sc,
		log:     log,
	}
	currentRoles := func(userID snowflake.ID) ([]snowflake.ID, bool) {
		m, ok := client.Caches.Member(cfg.GuildID, userID)
		return m.RoleIDs, ok
	}
	b.access = &access{
		currentRoles: currentRoles,
		guild:        cfg.GuildID,
		channel:      cfg.AccessChannelID,
		mediaRole:    cfg.MediaRoleID,
		seerrURL:     cfg.SeerrPublicURL,
		rest:         client.Rest,
		store:        st,
		plex:         pc,
		log:          log,
	}
	return b, nil
}

// atLeast passes on only records at min or above.
type atLeast struct {
	slog.Handler
	min slog.Level
}

func (h atLeast) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h atLeast) WithAttrs(attrs []slog.Attr) slog.Handler {
	return atLeast{Handler: h.Handler.WithAttrs(attrs), min: h.min}
}

func (h atLeast) WithGroup(name string) slog.Handler {
	return atLeast{Handler: h.Handler.WithGroup(name), min: h.min}
}

// commands are the guild's slash commands.
var commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "help",
		Description: "Show what you can do with this bot",
	},
	discord.SlashCommandCreate{
		Name:        "queue",
		Description: "Show what's downloading and what's waiting",
	},
	discord.SlashCommandCreate{
		Name:        "status",
		Description: "Show how the media server and everything behind it is doing",
	},
	discord.SlashCommandCreate{
		Name:        "access-panel",
		Description: "Post the media server access request panel in this channel",
		// Hidden without Manage Server; the bot checks the admin role too.
		DefaultMemberPermissions: omit.NewPtr(discord.PermissionManageGuild),
	},
}

// Start registers the slash commands and connects to the gateway.
func (b *Bot) Start(ctx context.Context) error {
	if _, err := b.client.Rest.SetGuildCommands(b.client.ApplicationID, b.cfg.GuildID, commands, rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	if err := b.client.OpenGateway(ctx); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	// Not fatal: the requests can be fixed by hand.
	if err := b.access.recoverInterrupted(ctx); err != nil {
		b.log.Error("recover interrupted access requests", "err", err)
	}
	if err := b.access.recoverRemoving(ctx); err != nil {
		b.log.Error("recover interrupted plex share removals", "err", err)
	}
	if err := b.access.checkMissed(ctx); err != nil {
		b.log.Error("check members missed while the bot was down", "err", err)
	}
	return nil
}

// Close disconnects from Discord.
func (b *Bot) Close(ctx context.Context) {
	b.client.Close(ctx)
}

// Ready reports whether the gateway session is up.
func (b *Bot) Ready() error {
	if b.client.Gateway == nil {
		return errors.New("no gateway")
	}
	if s := b.client.Gateway.Status(); s != gateway.StatusReady {
		return fmt.Errorf("gateway %s", s)
	}
	return nil
}

// HandleSeerr acts on a Seerr webhook payload. It carries on if Seerr
// hangs up, or a post made but not yet recorded would be made again.
func (b *Bot) HandleSeerr(ctx context.Context, p seerr.Payload) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayTimeout)
	defer cancel()
	return b.relay.handle(ctx, p)
}

// relayTimeout bounds the handling of one Seerr event.
const relayTimeout = 30 * time.Second

// inGuild reports whether an interaction comes from the bot's own server.
func (b *Bot) inGuild(guildID *snowflake.ID) bool {
	return guildID != nil && *guildID == b.cfg.GuildID
}

// hasRole reports whether a member has any of the roles.
func hasRole(m *discord.ResolvedMember, roles ...snowflake.ID) bool {
	if m == nil {
		return false
	}
	for _, r := range roles {
		if slices.Contains(m.RoleIDs, r) {
			return true
		}
	}
	return false
}

func (b *Bot) onCommand(e *events.ApplicationCommandInteractionCreate) {
	if !b.enter() {
		b.ephemeral(e.CreateMessage, textRestarting)
		return
	}
	defer b.handlers.Done()
	if !b.inGuild(e.GuildID()) {
		b.ephemeral(e.CreateMessage, "This bot only works in its own server.")
		return
	}
	// Other kinds of command, such as context menu entries, aren't slash
	// commands.
	data, ok := e.Data.(discord.SlashCommandInteractionData)
	if !ok {
		b.log.Warn("unsupported command type", "name", e.Data.CommandName())
		b.ephemeral(e.CreateMessage, "Unknown command.")
		return
	}
	switch name := data.CommandName(); name {
	case "help":
		b.help(e)
	case "queue":
		b.queue(e)
	case "status":
		b.status(e)
	case "access-panel":
		b.accessPanel(e)
	default:
		b.log.Warn("unknown command", "name", name)
		b.ephemeral(e.CreateMessage, "Unknown command.")
	}
}

// ephemeral answers an interaction with a message only its user sees.
func (b *Bot) ephemeral(create func(discord.MessageCreate, ...rest.RequestOpt) error, text string) {
	if err := create(discord.NewMessageCreate().WithContent(text).WithEphemeral(true)); err != nil {
		b.log.Error("answer interaction", "err", err)
	}
}

// onMemberLeave asks approvers about the Plex access of a member who left.
func (b *Bot) onMemberLeave(e *events.GuildMemberLeave) {
	if e.GuildID != b.cfg.GuildID || !b.enter() {
		return
	}
	defer b.handlers.Done()
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	if err := b.access.memberLeft(ctx, e.User.ID); err != nil {
		b.log.Error("member left", "err", err)
	}
}

// onMemberUpdate acts on a member's roles changing.
func (b *Bot) onMemberUpdate(e *events.GuildMemberUpdate) {
	if e.GuildID != b.cfg.GuildID || !b.enter() {
		return
	}
	defer b.handlers.Done()
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	// OldMember is empty if the member wasn't cached yet.
	oldKnown := e.OldMember.User.ID != 0
	if err := b.access.memberChanged(ctx, e.Member.User.ID, e.OldMember.RoleIDs, oldKnown, e.Member.RoleIDs); err != nil {
		b.log.Error("member changed", "err", err)
	}
}
