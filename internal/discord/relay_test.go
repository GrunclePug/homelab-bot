package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/replace"
	"github.com/grunclepug/homelab-bot/internal/seerr"
	"github.com/grunclepug/homelab-bot/internal/store"
)

const channel = snowflake.ID(500)

// fakeDiscord stands in for Discord's REST API.
type fakeDiscord struct {
	next    snowflake.ID
	msgs    map[snowflake.ID]*discord.Message
	created []snowflake.ID // in order
	deleted []snowflake.ID
	fail    error
}

func newFakeDiscord() *fakeDiscord {
	return &fakeDiscord{next: 1000, msgs: map[snowflake.ID]*discord.Message{}}
}

func (f *fakeDiscord) GetMessage(ch, id snowflake.ID, _ ...rest.RequestOpt) (*discord.Message, error) {
	m, ok := f.msgs[id]
	if !ok || m.ChannelID != ch {
		return nil, &rest.Error{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: "Unknown Message"}
	}
	c := *m
	c.Embeds = slices.Clone(m.Embeds)
	return &c, nil
}

func (f *fakeDiscord) CreateMessage(ch snowflake.ID, mc discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.next++
	m := &discord.Message{
		ID:               f.next,
		ChannelID:        ch,
		Content:          mc.Content,
		Embeds:           mc.Embeds,
		Components:       mc.Components,
		MessageReference: mc.MessageReference,
	}
	f.msgs[m.ID] = m
	f.created = append(f.created, m.ID)
	return m, nil
}

func (f *fakeDiscord) UpdateMessage(ch, id snowflake.ID, mu discord.MessageUpdate, _ ...rest.RequestOpt) (*discord.Message, error) {
	m, ok := f.msgs[id]
	if !ok || m.ChannelID != ch {
		return nil, errors.New("unknown message")
	}
	if mu.Embeds != nil {
		m.Embeds = *mu.Embeds
	}
	if mu.Components != nil {
		m.Components = *mu.Components
	}
	return m, nil
}

func (f *fakeDiscord) DeleteMessage(ch, id snowflake.ID, _ ...rest.RequestOpt) error {
	if m, ok := f.msgs[id]; !ok || m.ChannelID != ch {
		return errors.New("unknown message")
	}
	delete(f.msgs, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeDiscord) last() *discord.Message {
	return f.msgs[f.created[len(f.created)-1]]
}

// fakeSeerr serves GET /api/v1/issue/{id}. Issue 404 is missing.
func fakeSeerr(t *testing.T) *seerr.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/issue/7":
			io.WriteString(w, `{"id":7,"issueType":2,"status":1,"problemSeason":2,"problemEpisode":3,
				"media":{"mediaType":"tv","tmdbId":1,"tvdbId":2},"createdBy":{"displayName":"ann"},
				"comments":[{"id":1,"message":"No sound"}]}`)
		case "/api/v1/issue/8":
			io.WriteString(w, `{"id":8,"issueType":1,"status":1,"problemSeason":0,"problemEpisode":0,
				"media":{"mediaType":"movie","tmdbId":3},"createdBy":{"displayName":"bob"}}`)
		default:
			http.Error(w, `{"message":"Issue not found."}`, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return seerr.NewClient(srv.URL, "key")
}

func newTestRelay(t *testing.T) (*relay, *fakeDiscord, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fd := newFakeDiscord()
	return &relay{
		channel: channel,
		msg:     fd,
		store:   st,
		issues:  fakeSeerr(t),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fd, st
}

func payload(typ, id string) seerr.Payload {
	return seerr.Payload{
		NotificationType: typ,
		Event:            "New Audio Issue Reported",
		Subject:          "Shooter (2016)",
		Message:          "No sound",
		Image:            "https://image.example/poster.jpg",
		IssueID:          id,
		IssueType:        "AUDIO",
		ReportedBy:       "ann",
	}
}

func field(e discord.Embed, name string) string {
	for _, f := range e.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

func customIDs(cs []discord.LayoutComponent) []string {
	var ids []string
	for _, c := range cs {
		row, ok := c.(discord.ActionRowComponent)
		if !ok {
			continue
		}
		for _, ic := range row.Components {
			ids = append(ids, ic.GetCustomID())
		}
	}
	return ids
}

func TestRelayTest(t *testing.T) {
	r, fd, _ := newTestRelay(t)
	if err := r.handle(context.Background(), seerr.Payload{NotificationType: seerr.NotifyTest, Subject: "Test Notification"}); err != nil {
		t.Fatal(err)
	}
	if got := fd.last().Content; got != testMessage {
		t.Errorf("content = %q", got)
	}
}

func TestRelayLifecycle(t *testing.T) {
	ctx := context.Background()
	r, fd, st := newTestRelay(t)

	// Created: a new post with the issue's details and both buttons.
	if err := r.handle(ctx, payload(seerr.NotifyIssueCreated, "7")); err != nil {
		t.Fatal(err)
	}
	post := fd.last()
	e := post.Embeds[0]
	checks := map[string]string{
		"title":       e.Title,
		"description": e.Description,
		"thumbnail":   e.Thumbnail.URL,
		"footer":      e.Footer.Text,
		"Type":        field(e, "Type"),
		"Reported by": field(e, "Reported by"),
		"Affects":     field(e, "Affects"),
		"Status":      field(e, fieldStatus),
	}
	want := map[string]string{
		"title":       "Shooter (2016)",
		"description": "No sound",
		"thumbnail":   "https://image.example/poster.jpg",
		"footer":      "Seerr issue #7",
		"Type":        "Audio",
		"Reported by": "ann",
		"Affects":     "Season 2, episode 3",
		"Status":      "Open",
	}
	for k, w := range want {
		if checks[k] != w {
			t.Errorf("created %s = %q, want %q", k, checks[k], w)
		}
	}
	if ids := customIDs(post.Components); !slices.Equal(ids, []string{"replace:7", "resolve:7"}) {
		t.Errorf("buttons = %v", ids)
	}
	p, ok, err := st.Post(ctx, 7)
	if err != nil || !ok || p.MessageID != post.ID.String() || p.ChannelID != channel.String() {
		t.Fatalf("stored post = %+v, %v, %v", p, ok, err)
	}

	// Comment: a reply to the post, quoting the comment.
	c := payload(seerr.NotifyIssueComment, "7")
	c.CommentedBy, c.Comment = "bob", "Same here\non S02E03"
	if err := r.handle(ctx, c); err != nil {
		t.Fatal(err)
	}
	reply := fd.last()
	if reply.MessageReference == nil || *reply.MessageReference.MessageID != post.ID {
		t.Errorf("reply reference = %+v", reply.MessageReference)
	}
	if want := "**bob** commented:\n> Same here\n> on S02E03"; reply.Content != want {
		t.Errorf("reply = %q, want %q", reply.Content, want)
	}

	// Resolved: greyed out, no buttons.
	if err := r.handle(ctx, payload(seerr.NotifyIssueResolved, "7")); err != nil {
		t.Fatal(err)
	}
	e = fd.msgs[post.ID].Embeds[0]
	if field(e, fieldStatus) != statusResolved || e.Color != colorResolved || len(fd.msgs[post.ID].Components) != 0 {
		t.Errorf("resolved: status %q, colour %x, components %d", field(e, fieldStatus), e.Color, len(fd.msgs[post.ID].Components))
	}
	if field(e, "Affects") != "Season 2, episode 3" {
		t.Error("resolved post lost its fields")
	}

	// Reopened: open again, buttons back.
	if err := r.handle(ctx, payload(seerr.NotifyIssueReopened, "7")); err != nil {
		t.Fatal(err)
	}
	e = fd.msgs[post.ID].Embeds[0]
	if field(e, fieldStatus) != statusOpen || e.Color != colorOpen {
		t.Errorf("reopened: status %q, colour %x", field(e, fieldStatus), e.Color)
	}
	if ids := customIDs(fd.msgs[post.ID].Components); !slices.Equal(ids, []string{"replace:7", "resolve:7"}) {
		t.Errorf("reopened buttons = %v", ids)
	}

	if n := len(fd.created); n != 2 {
		t.Errorf("messages created = %d, want 2 (post and reply)", n)
	}
}

func TestRelayWithoutPost(t *testing.T) {
	ctx := context.Background()

	// A comment on an issue from before the bot: the post is created first.
	r, fd, _ := newTestRelay(t)
	c := payload(seerr.NotifyIssueComment, "8")
	c.CommentedBy, c.Comment = "bob", "Any news?"
	if err := r.handle(ctx, c); err != nil {
		t.Fatal(err)
	}
	if len(fd.created) != 2 {
		t.Fatalf("created %d messages, want post and reply", len(fd.created))
	}
	post := fd.msgs[fd.created[0]]
	if field(post.Embeds[0], "Affects") != "" {
		t.Error("movie post has an Affects field")
	}
	if ref := fd.last().MessageReference; ref == nil || *ref.MessageID != post.ID {
		t.Error("reply doesn't reference the new post")
	}

	// The post was deleted in Discord: resolving posts a fresh, resolved one.
	delete(fd.msgs, post.ID)
	if err := r.handle(ctx, payload(seerr.NotifyIssueResolved, "8")); err != nil {
		t.Fatal(err)
	}
	fresh := fd.last()
	if fresh.ID == post.ID || field(fresh.Embeds[0], fieldStatus) != statusResolved || len(fresh.Components) != 0 {
		t.Errorf("fresh post: id %v, status %q, components %d", fresh.ID, field(fresh.Embeds[0], fieldStatus), len(fresh.Components))
	}
}

func TestRelayIssueFetchFails(t *testing.T) {
	r, fd, _ := newTestRelay(t)
	if err := r.handle(context.Background(), payload(seerr.NotifyIssueCreated, "404")); err != nil {
		t.Fatal(err)
	}
	e := fd.last().Embeds[0]
	if e.Title != "Shooter (2016)" || field(e, "Type") != "Audio" || field(e, "Affects") != "" {
		t.Errorf("fallback post: %+v", e)
	}
}

func TestRelayCreatedTwice(t *testing.T) {
	ctx := context.Background()
	r, fd, _ := newTestRelay(t)
	for range 2 {
		if err := r.handle(ctx, payload(seerr.NotifyIssueCreated, "7")); err != nil {
			t.Fatal(err)
		}
	}
	if len(fd.created) != 1 {
		t.Errorf("created %d posts for one issue", len(fd.created))
	}
}

func TestRelayErrors(t *testing.T) {
	ctx := context.Background()
	r, fd, _ := newTestRelay(t)
	tests := []struct {
		name string
		p    seerr.Payload
		bad  bool
	}{
		{"unsupported type", payload("MEDIA_PENDING", "7"), true},
		{"no issue id", payload(seerr.NotifyIssueCreated, ""), true},
		{"discord fails", payload(seerr.NotifyIssueCreated, "7"), false},
	}
	fd.fail = errors.New("discord down")
	for _, tt := range tests {
		err := r.handle(ctx, tt.p)
		if err == nil {
			t.Errorf("%s: want error", tt.name)
			continue
		}
		if errors.Is(err, seerr.ErrBadPayload) != tt.bad {
			t.Errorf("%s: bad payload = %v, want %v (%v)", tt.name, !tt.bad, tt.bad, err)
		}
	}
}

func TestPickViewChunks(t *testing.T) {
	plan := seasonPlan(30)
	upd := pickView(plan, "tok")
	rows := *upd.Components
	if len(rows) != 2 {
		t.Fatalf("menus = %d, want 2", len(rows))
	}
	first := rows[0].(discord.ActionRowComponent).Components[0].(discord.StringSelectMenuComponent)
	second := rows[1].(discord.ActionRowComponent).Components[0].(discord.StringSelectMenuComponent)
	if len(first.Options) != 25 || len(second.Options) != 6 {
		t.Errorf("options = %d + %d, want 25 + 6", len(first.Options), len(second.Options))
	}
	if first.Options[0].Value != "all" || first.MaxValues != 25 || second.MaxValues != 6 {
		t.Errorf("first option %q, max values %d/%d", first.Options[0].Value, first.MaxValues, second.MaxValues)
	}
	if first.CustomID == second.CustomID || !strings.HasPrefix(second.CustomID, idPick+"tok:") {
		t.Errorf("custom IDs %q, %q", first.CustomID, second.CustomID)
	}
}

func TestLines(t *testing.T) {
	ls := []string{"aaaa", "bbbb", "cccc", "dddd"}
	if got := lines(ls, 100); got != "aaaa\nbbbb\ncccc\ndddd" {
		t.Errorf("all fit: %q", got)
	}
	got := lines(ls, 25)
	if got != "aaaa\nbbbb\n*and 2 more*" {
		t.Errorf("cut: %q", got)
	}
	if n := len([]rune(got)); n > 25 {
		t.Errorf("cut text is %d runes, limit 25", n)
	}
}

func TestPendingExpiry(t *testing.T) {
	s := newPendingStore()
	now := s.now()
	s.now = func() time.Time { return now }
	tok := s.put(&pendingReplace{IssueID: 1})
	if _, ok := s.get(tok); !ok {
		t.Fatal("fresh flow missing")
	}
	if _, ok := s.take(tok); !ok {
		t.Fatal("take failed")
	}
	if _, ok := s.take(tok); ok {
		t.Error("flow taken twice")
	}
	tok = s.put(&pendingReplace{IssueID: 2})
	s.now = func() time.Time { return now.Add(pendingTTL + time.Second) }
	if _, ok := s.get(tok); ok {
		t.Error("expired flow still there")
	}
}

func seasonPlan(n int) replace.Plan {
	p := replace.Plan{Title: "Show", Scope: replace.ScopeSeason, Season: 1}
	for i := range n {
		p.Candidates = append(p.Candidates, replace.Target{
			Kind: replace.Episode, ItemID: 100 + i, Season: 1, Number: i + 1,
			Label: fmt.Sprintf("S01E%02d · Episode", i+1),
		})
	}
	return p
}
