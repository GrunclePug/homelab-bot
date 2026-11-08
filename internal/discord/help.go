package discord

import (
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

// roles is which of the bot's roles a member has.
type roles struct {
	admin, media, approver bool
}

func (b *Bot) rolesOf(m *discord.ResolvedMember) roles {
	return roles{
		admin:    hasRole(m, b.cfg.AdminRoleID),
		media:    hasRole(m, b.cfg.MediaRoleID),
		approver: hasRole(m, b.cfg.ApproverRoleID),
	}
}

// help shows the member what they can do with the bot, and nothing they
// can't.
func (b *Bot) help(e *events.ApplicationCommandInteractionCreate) {
	m := discord.NewMessageCreate().
		WithEmbeds(helpEmbed(b.rolesOf(e.Member()), b.cfg.SupportChannelID, b.cfg.AccessChannelID)).
		WithEphemeral(true)
	if err := e.CreateMessage(m); err != nil {
		b.log.Error("answer help", "err", err)
	}
}

// helpEmbed lists the commands and buttons a member with r may use.
func helpEmbed(r roles, support, accessChannel snowflake.ID) discord.Embed {
	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }

	if r.media || r.admin {
		add("**Commands**",
			"`/queue`: what's downloading and what's waiting.",
			"`/status`: how the media server and everything behind it is doing.",
			"`/help`: this list.")
	} else {
		add("**Getting access**",
			"Press **Request access** on the access panel and tell us your Plex account. "+
				"An approver will set you up, and I'll DM you when they have.")
	}
	if r.media || r.admin {
		add("", "**Something wrong with a movie or show?**",
			"Open it in Seerr and choose Report an Issue. It shows up in <#"+support.String()+
				"> and you'll see it there as the admins work on it.")
	}
	if r.approver {
		add("", "**Approvers**",
			"In <#"+accessChannel.String()+">, **Approve** sends the member a Plex invite, gives them the media role and DMs them; **Deny** closes the request and tells them.",
			"When a member leaves or loses the media role, I ask there whether to **Remove access** to Plex or **Keep** it.")
	}
	if r.admin || r.approver {
		add("", "**Issues**",
			"On issue posts in <#"+support.String()+">, **Replace** blocklists the release, deletes the file and searches for another; **Resolve** closes the issue in Seerr.")
	}
	if r.admin {
		add("", "**Admins**",
			"`/access-panel` posts the access request panel in the channel you run it in.")
	}
	return discord.NewEmbed().
		WithTitle("What I can do for you").
		WithColor(colorQueue).
		WithDescription(strings.Join(lines, "\n"))
}
