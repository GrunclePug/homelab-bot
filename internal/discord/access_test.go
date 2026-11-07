package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/plex"
	"github.com/grunclepug/homelab-bot/internal/store"
)

const (
	accessChannel = snowflake.ID(600)
	mediaRole     = snowflake.ID(700)
	member        = snowflake.ID(42)
)

// fakeAccessDiscord adds roles and DMs to fakeDiscord.
type fakeAccessDiscord struct {
	*fakeDiscord
	roles   map[snowflake.ID][]snowflake.ID
	roleErr error
	dmErr   error
}

func (f *fakeAccessDiscord) AddMemberRole(_, userID, roleID snowflake.ID, _ ...rest.RequestOpt) error {
	if f.roleErr != nil {
		return f.roleErr
	}
	f.roles[userID] = append(f.roles[userID], roleID)
	return nil
}

// dmChannel is the fake DM channel for a user.
func dmChannel(userID snowflake.ID) snowflake.ID { return 9000 + userID }

func (f *fakeAccessDiscord) CreateDMChannel(userID snowflake.ID, _ ...rest.RequestOpt) (*discord.DMChannel, error) {
	if f.dmErr != nil {
		return nil, f.dmErr
	}
	var ch discord.DMChannel
	if err := json.Unmarshal([]byte(`{"id":"`+dmChannel(userID).String()+`","type":1}`), &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

// dms returns the messages sent to a user's DM channel.
func (f *fakeAccessDiscord) dms(userID snowflake.ID) []string {
	var out []string
	for _, id := range f.created {
		if m := f.msgs[id]; m.ChannelID == dmChannel(userID) {
			out = append(out, m.Content)
		}
	}
	return out
}

// fakePlexTV is plex.tv with one server; invites fail while failInvite
// is set.
type fakePlexTV struct {
	mu         sync.Mutex
	invited    []string
	failInvite bool
}

func (f *fakePlexTV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /api/servers/srv":
		io.WriteString(w, `<MediaContainer><Server machineIdentifier="srv"><Section id="1" key="1" title="Movies"/></Server></MediaContainer>`)
	case "POST /api/servers/srv/shared_servers":
		if f.failInvite {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `<errors><error code="1000" message="Unknown user"/></errors>`)
			return
		}
		var body struct {
			Shared struct {
				Email string `json:"invited_email"`
			} `json:"shared_server"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.invited = append(f.invited, body.Shared.Email)
		io.WriteString(w, `<SharedServer/>`)
	default:
		http.NotFound(w, r)
	}
}

func newTestAccess(t *testing.T) (*access, *fakeAccessDiscord, *fakePlexTV, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fp := &fakePlexTV{}
	srv := httptest.NewServer(fp)
	t.Cleanup(srv.Close)
	fd := &fakeAccessDiscord{fakeDiscord: newFakeDiscord(), roles: map[snowflake.ID][]snowflake.ID{}}
	return &access{
		guild:     1,
		channel:   accessChannel,
		mediaRole: mediaRole,
		seerrURL:  "https://seerr.example",
		rest:      fd,
		store:     st,
		plex:      plex.NewClient(srv.URL, "tok", "srv"),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fd, fp, st
}

// submitted files a request and returns its post.
func submitted(t *testing.T, a *access, fd *fakeAccessDiscord) *discord.Message {
	t.Helper()
	text, err := a.submit(context.Background(), member, "Ann", "  ann@example.com ", "Bob's friend")
	if err != nil || text != textRequestSent {
		t.Fatalf("submit = %q, %v", text, err)
	}
	return fd.last()
}

func TestAccessSubmit(t *testing.T) {
	ctx := context.Background()
	a, fd, _, st := newTestAccess(t)

	if why, _ := a.canRequest(ctx, member, true); why != textHasAccess {
		t.Errorf("member with media role: %q", why)
	}
	if why, err := a.canRequest(ctx, member, false); why != "" || err != nil {
		t.Errorf("new member: %q, %v", why, err)
	}

	post := submitted(t, a, fd)
	e := post.Embeds[0]
	if post.ChannelID != accessChannel || e.Title != "Access request #1" || e.Description != "Bob's friend" {
		t.Errorf("post: channel %v, title %q, description %q", post.ChannelID, e.Title, e.Description)
	}
	if field(e, "Plex account") != "`ann@example.com`" || field(e, "Member") != "<@42> (Ann)" {
		t.Errorf("fields: %+v", e.Fields)
	}
	if ids := customIDs(post.Components); !slices.Equal(ids, []string{"access:approve:1", "access:deny:1"}) {
		t.Errorf("buttons = %v", ids)
	}
	r, _, _ := st.Request(ctx, 1)
	if r.MessageID != post.ID.String() || r.Status != store.StatusPending {
		t.Errorf("stored = %+v", r)
	}

	if why, _ := a.canRequest(ctx, member, false); why != textOpenRequest {
		t.Errorf("second request: %q", why)
	}
	if text, err := a.submit(ctx, member, "Ann", "again", ""); text != textOpenRequest || err != nil {
		t.Errorf("second submit = %q, %v", text, err)
	}
}

func TestAccessSubmitPostFails(t *testing.T) {
	ctx := context.Background()
	a, fd, _, st := newTestAccess(t)
	fd.fail = errors.New("discord down")
	if _, err := a.submit(ctx, member, "Ann", "ann", ""); err == nil {
		t.Fatal("want error")
	}
	if open, _ := st.OpenRequest(ctx, member.String()); open {
		t.Error("request kept although nobody can see it")
	}
}

func TestAccessApprove(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	post := submitted(t, a, fd)

	text, err := a.approve(ctx, 1, "Bob")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fp.invited, []string{"ann@example.com"}) {
		t.Errorf("invited = %v", fp.invited)
	}
	if !slices.Equal(fd.roles[member], []snowflake.ID{mediaRole}) {
		t.Errorf("roles = %v", fd.roles[member])
	}
	dms := fd.dms(member)
	if len(dms) != 1 || !strings.Contains(dms[0], "https://seerr.example") || !strings.Contains(dms[0], "Accept the Plex invite") {
		t.Errorf("DMs = %q", dms)
	}
	want := "Plex invite sent to `ann@example.com`\nMedia role added\nWelcome DM sent"
	if text != "Approved.\n"+want {
		t.Errorf("reply = %q", text)
	}
	m := fd.msgs[post.ID]
	e := m.Embeds[0]
	if field(e, fieldStatus) != "Approved by Bob" || field(e, "Onboarding") != want || e.Color != colorApproved || len(m.Components) != 0 {
		t.Errorf("post after approval: %+v, components %d", e, len(m.Components))
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusApproved || r.DecidedBy != "Bob" {
		t.Errorf("stored = %+v", r)
	}

	// A second click, or a deny after the approval, does nothing.
	for _, run := range []func(context.Context, int, string) (string, error){a.approve, a.deny} {
		if text, err := run(ctx, 1, "Cy"); text != textHandled || err != nil {
			t.Errorf("after approval: %q, %v", text, err)
		}
	}
	if len(fp.invited) != 1 {
		t.Errorf("invited %d times", len(fp.invited))
	}
}

func TestAccessApproveInviteFails(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	post := submitted(t, a, fd)
	fp.failInvite = true

	text, err := a.approve(ctx, 1, "Bob")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Unknown user") || !strings.Contains(text, "still open") {
		t.Errorf("reply = %q", text)
	}
	if len(fd.roles[member]) != 0 || len(fd.dms(member)) != 0 {
		t.Error("role or DM given although the invite failed")
	}
	m := fd.msgs[post.ID]
	if !strings.Contains(field(m.Embeds[0], "Last attempt"), "Bob tried to approve") || len(customIDs(m.Components)) != 2 {
		t.Errorf("post after failed invite: %+v", m.Embeds[0].Fields)
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusPending {
		t.Errorf("status = %q, want pending", r.Status)
	}

	// Fixed on Plex's side: the retry goes through.
	fp.failInvite = false
	if text, err := a.approve(ctx, 1, "Bob"); err != nil || !strings.HasPrefix(text, "Approved.") {
		t.Errorf("retry = %q, %v", text, err)
	}
}

func TestAccessApprovePartial(t *testing.T) {
	ctx := context.Background()
	a, fd, _, st := newTestAccess(t)
	submitted(t, a, fd)
	fd.roleErr = errors.New("missing permissions")
	fd.dmErr = errors.New("cannot send messages to this user")

	text, err := a.approve(ctx, 1, "Bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plex invite sent", "Couldn't add the media role", "missing permissions", "Couldn't DM"} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusApproved {
		t.Errorf("status = %q: the invite went out, so the request is approved", r.Status)
	}
}

func TestAccessDeny(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	post := submitted(t, a, fd)

	text, err := a.deny(ctx, 1, "Bob")
	if err != nil {
		t.Fatal(err)
	}
	if text != "Denied. They were told by DM." {
		t.Errorf("reply = %q", text)
	}
	if dms := fd.dms(member); len(dms) != 1 || !strings.Contains(dms[0], "declined") {
		t.Errorf("DMs = %q", dms)
	}
	m := fd.msgs[post.ID]
	if field(m.Embeds[0], fieldStatus) != "Denied by Bob" || len(m.Components) != 0 {
		t.Errorf("post after denial: %+v", m.Embeds[0].Fields)
	}
	if len(fp.invited) != 0 || len(fd.roles[member]) != 0 {
		t.Error("denied member was onboarded")
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusDenied {
		t.Errorf("status = %q", r.Status)
	}
	// They may ask again later.
	if why, _ := a.canRequest(ctx, member, false); why != "" {
		t.Errorf("after denial: %q", why)
	}
}
