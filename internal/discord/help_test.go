package discord

import (
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// TestHelpShowsOnlyWhatYouCanDo checks each role sees its own section and
// nothing it can't use.
func TestHelpShowsOnlyWhatYouCanDo(t *testing.T) {
	tests := []struct {
		name      string
		r         roles
		want, not []string
	}{
		{"no roles", roles{},
			[]string{"Request access"},
			[]string{"/queue", "/status", "Approve", "Replace", "/access-panel", "Report an Issue"}},
		{"media", roles{media: true},
			[]string{"/queue", "/status", "/help", "Report an Issue"},
			[]string{"Request access", "Approve", "Replace", "/access-panel"}},
		{"approver without media", roles{approver: true},
			[]string{"Request access", "Approve", "Remove access"},
			[]string{"/queue", "Replace", "/access-panel"}},
		{"admin", roles{admin: true},
			[]string{"/queue", "/status", "Replace", "Resolve", "/access-panel"},
			[]string{"Request access", "Approve"}},
		{"everything", roles{admin: true, media: true, approver: true},
			[]string{"/queue", "Approve", "Replace", "/access-panel"},
			[]string{"Request access"}},
	}
	for _, tt := range tests {
		d := helpEmbed(tt.r, 10, 20).Description
		for _, w := range tt.want {
			if !strings.Contains(d, w) {
				t.Errorf("%s: help lacks %q:\n%s", tt.name, w, d)
			}
		}
		for _, n := range tt.not {
			if strings.Contains(d, n) {
				t.Errorf("%s: help shows %q:\n%s", tt.name, n, d)
			}
		}
		if len(d) > maxDescription {
			t.Errorf("%s: help is %d characters, over %d", tt.name, len(d), maxDescription)
		}
	}
}

func TestHelpCommand(t *testing.T) {
	var got []reply
	routingBot().onCommand(commandEvent(t, "help", []snowflake.ID{mediaRole}, &got))
	if len(got) != 1 || got[0].typ != discord.InteractionResponseTypeCreateMessage {
		t.Errorf("replies = %+v, want one message", got)
	}
}
