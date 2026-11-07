// Package discord runs the Discord side of the bot: the gateway session,
// the /queue command, the issue posts and their buttons.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
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

// Bot is the Discord bot.
type Bot struct {
	cfg     Config
	client  *bot.Client
	seerr   *seerr.Client
	replace replace.Service
	qbit    *qbit.Client
	relay   *relay
	access  *access
	pending *pendingStore
	log     *slog.Logger
}

// New builds the bot. It doesn't connect until Start.
func New(cfg Config, st *store.Store, sc *seerr.Client, rs replace.Service, qc *qbit.Client, pc *plex.Client, log *slog.Logger) (*Bot, error) {
	b := &Bot{
		cfg:     cfg,
		seerr:   sc,
		replace: rs,
		qbit:    qc,
		pending: newPendingStore(),
		log:     log,
	}
	none := discord.AllowedMentions{
		Parse: []discord.AllowedMentionType{},
		Roles: []snowflake.ID{},
		Users: []snowflake.ID{},
	}
	client, err := disgo.New(cfg.Token,
		bot.WithLogger(log.With("component", "disgo")),
		bot.WithGatewayConfigOpts(gateway.WithIntents(gateway.IntentGuilds)),
		bot.WithEventManagerConfigOpts(bot.WithAsyncEventsEnabled()),
		bot.WithRestConfigOpts(rest.WithDefaultAllowedMentions(none)),
		bot.WithEventListenerFunc(b.onCommand),
		bot.WithEventListenerFunc(b.onComponent),
		bot.WithEventListenerFunc(b.onModal),
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
	b.access = &access{
		guild:     cfg.GuildID,
		channel:   cfg.AccessChannelID,
		mediaRole: cfg.MediaRoleID,
		seerrURL:  cfg.SeerrPublicURL,
		rest:      client.Rest,
		store:     st,
		plex:      pc,
		log:       log,
	}
	return b, nil
}

// commands are the guild's slash commands.
var commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "queue",
		Description: "Show what's downloading and what's waiting",
	},
	discord.SlashCommandCreate{
		Name:        "access-panel",
		Description: "Post the media server access request panel in this channel",
		// Hidden from members without Manage Server; the bot also checks
		// for the admin role.
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

// HandleSeerr acts on a Seerr webhook payload.
func (b *Bot) HandleSeerr(ctx context.Context, p seerr.Payload) error {
	return b.relay.handle(ctx, p)
}

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
	if !b.inGuild(e.GuildID()) {
		b.ephemeral(e.CreateMessage, "This bot only works in its own server.")
		return
	}
	switch name := e.SlashCommandInteractionData().CommandName(); name {
	case "queue":
		b.queue(e)
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
