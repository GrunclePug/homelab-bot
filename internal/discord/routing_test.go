package discord

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

const (
	guild        = snowflake.ID(1000000000000000001)
	adminRole    = snowflake.ID(100)
	approverRole = snowflake.ID(200)
)

// reply is one response an event handler gave.
type reply struct {
	typ  discord.InteractionResponseType
	text string
}

func recorder(got *[]reply) events.InteractionResponderFunc {
	return func(typ discord.InteractionResponseType, data discord.InteractionResponseData, _ ...rest.RequestOpt) error {
		r := reply{typ: typ}
		switch d := data.(type) {
		case discord.MessageCreate:
			r.text = d.Content
		case discord.MessageUpdate:
			if d.Content != nil {
				r.text = *d.Content
			}
		}
		*got = append(*got, r)
		return nil
	}
}

func routingBot() *Bot {
	return &Bot{
		cfg:     Config{GuildID: guild, AdminRoleID: adminRole, ApproverRoleID: approverRole, MediaRoleID: mediaRole},
		pending: newPendingStore(),
		drafts:  newDraftStore(),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func memberJSON(user snowflake.ID, roles []snowflake.ID) string {
	rs, _ := json.Marshal(roles)
	return fmt.Sprintf(`{"user":{"id":"%s","username":"u"},"roles":%s,"joined_at":"2026-01-01T00:00:00Z"}`, user, rs)
}

func componentEvent(t *testing.T, customID string, user snowflake.ID, roles []snowflake.ID, got *[]reply) *events.ComponentInteractionCreate {
	t.Helper()
	data := fmt.Sprintf(`{"custom_id":%q,"component_type":2}`, customID)
	if strings.HasPrefix(customID, idPick) {
		data = fmt.Sprintf(`{"custom_id":%q,"component_type":3,"values":["all"]}`, customID)
	}
	raw := fmt.Sprintf(`{"id":"1","application_id":"2","type":3,"token":"tok","version":1,
		"guild_id":"%s","channel_id":"3","member":%s,"data":%s,
		"message":{"id":"5","channel_id":"3","type":0,"content":"","author":{"id":"2","username":"bot"},"timestamp":"2026-01-01T00:00:00Z"}}`,
		guild, memberJSON(user, roles), data)
	var ci discord.ComponentInteraction
	if err := json.Unmarshal([]byte(raw), &ci); err != nil {
		t.Fatalf("build interaction %s: %v", customID, err)
	}
	return &events.ComponentInteractionCreate{GenericEvent: events.NewGenericEvent(nil, 0, 0), ComponentInteraction: ci, Respond: recorder(got)}
}

func commandEvent(t *testing.T, name string, roles []snowflake.ID, got *[]reply) *events.ApplicationCommandInteractionCreate {
	t.Helper()
	raw := fmt.Sprintf(`{"id":"1","application_id":"2","type":2,"token":"tok","version":1,
		"guild_id":"%s","channel_id":"3","member":%s,"data":{"id":"9","name":%q,"type":1}}`,
		guild, memberJSON(member, roles), name)
	var ai discord.ApplicationCommandInteraction
	if err := json.Unmarshal([]byte(raw), &ai); err != nil {
		t.Fatalf("build command %s: %v", name, err)
	}
	return &events.ApplicationCommandInteractionCreate{GenericEvent: events.NewGenericEvent(nil, 0, 0), ApplicationCommandInteraction: ai, Respond: recorder(got)}
}

// TestComponentPermissions checks that every privileged button refuses a
// member without the role it needs, before doing anything else.
func TestComponentPermissions(t *testing.T) {
	adminOnly := []string{"replace:7", "resolve:7", "pick:tok:0", "confirm:tok", "cancel:tok"}
	approverOnly := []string{"access:approve:1", "access:deny:1"}
	tests := []struct {
		name  string
		roles []snowflake.ID
		ids   []string
		want  string
	}{
		{"no roles, admin buttons", nil, adminOnly, textAdminsOnly},
		{"approver, admin buttons", []snowflake.ID{approverRole, mediaRole}, adminOnly, textAdminsOnly},
		{"no roles, approver buttons", nil, approverOnly, textApproversOnly},
		{"admin, approver buttons", []snowflake.ID{adminRole, mediaRole}, approverOnly, textApproversOnly},
	}
	for _, tt := range tests {
		for _, id := range tt.ids {
			t.Run(tt.name+"/"+id, func(t *testing.T) {
				var got []reply
				routingBot().onComponent(componentEvent(t, id, member, tt.roles, &got))
				if len(got) != 1 || got[0].typ != discord.InteractionResponseTypeCreateMessage || got[0].text != tt.want {
					t.Errorf("replies = %+v, want one %q", got, tt.want)
				}
			})
		}
	}
}

// TestDraftOwner checks that only the member who filled in a form can send
// or edit it.
func TestDraftOwner(t *testing.T) {
	for _, prefix := range []string{idAccessSend, idAccessEdit} {
		b := routingBot()
		tok := b.drafts.put(draft{UserID: member, Plex: "ann@example.com"})
		var got []reply
		b.onComponent(componentEvent(t, prefix+tok, member+1, []snowflake.ID{adminRole, approverRole}, &got))
		if len(got) != 1 || got[0].typ != discord.InteractionResponseTypeUpdateMessage || got[0].text != textDraftExpired {
			t.Errorf("%s by another member: replies = %+v", prefix, got)
		}
		if _, ok := b.drafts.get(tok, member); !ok {
			t.Errorf("%s by another member used up the draft", prefix)
		}

		got = nil
		b.onComponent(componentEvent(t, idAccessEdit+tok, member, nil, &got))
		if len(got) != 1 || got[0].typ != discord.InteractionResponseTypeModal {
			t.Errorf("edit by the owner: replies = %+v, want the form", got)
		}
	}
}

func TestCommandPermissions(t *testing.T) {
	tests := []struct {
		name  string
		roles []snowflake.ID
		want  string
	}{
		{"queue", nil, textMediaOnly},
		{"queue", []snowflake.ID{approverRole}, textMediaOnly},
		{"access-panel", []snowflake.ID{mediaRole, approverRole}, textAdminsOnly},
		{"something-else", []snowflake.ID{adminRole}, "Unknown command."},
	}
	for _, tt := range tests {
		var got []reply
		routingBot().onCommand(commandEvent(t, tt.name, tt.roles, &got))
		if len(got) != 1 || got[0].text != tt.want {
			t.Errorf("/%s with roles %v: replies = %+v, want %q", tt.name, tt.roles, got, tt.want)
		}
	}
}

func TestRestartingRefusesClicks(t *testing.T) {
	b := routingBot()
	b.closing = true
	var got []reply
	b.onComponent(componentEvent(t, "replace:7", member, []snowflake.ID{adminRole}, &got))
	if len(got) != 1 || got[0].text != textRestarting {
		t.Errorf("replies = %+v", got)
	}
}
