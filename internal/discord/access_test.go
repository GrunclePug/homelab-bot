package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
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
// is set. Each invite creates a share, numbered from 100.
type fakePlexTV struct {
	mu         sync.Mutex
	invited    []string
	removed    []int
	failInvite bool
	failRemove bool
	noShareID  bool // invites name no share, and none is listed
	echo       bool // the failure message repeats the invited address
}

func (f *fakePlexTV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /api/servers/srv":
		io.WriteString(w, `<MediaContainer><Server machineIdentifier="srv"><Section id="1" key="1" title="Movies"/></Server></MediaContainer>`)
	case "POST /api/servers/srv/shared_servers":
		var body struct {
			Shared struct {
				Email string `json:"invited_email"`
			} `json:"shared_server"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if f.failInvite {
			msg := "Unknown user"
			if f.echo {
				// plex.tv might name the account, in any case.
				msg = "Unknown user " + strings.ToUpper(body.Shared.Email)
			}
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `<errors><error code="1000" message="`+msg+`"/></errors>`)
			return
		}
		f.invited = append(f.invited, body.Shared.Email)
		if f.noShareID {
			io.WriteString(w, `<SharedServer/>`)
			return
		}
		fmt.Fprintf(w, `<SharedServer id="%d"/>`, 99+len(f.invited))
	default:
		if id, ok := strings.CutPrefix(r.URL.Path, "/api/servers/srv/shared_servers/"); ok && r.Method == http.MethodDelete {
			if f.failRemove {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			n, _ := strconv.Atoi(id)
			f.removed = append(f.removed, n)
			return
		}
		http.NotFound(w, r)
	}
}

// faultyStore makes chosen store calls fail.
type faultyStore struct {
	*store.Store
	failDecide, failSetPost bool
}

func (s *faultyStore) Decide(ctx context.Context, id int, status, by, plex string) error {
	if s.failDecide {
		return errors.New("disk I/O error")
	}
	return s.Store.Decide(ctx, id, status, by, plex)
}

func (s *faultyStore) SetRequestPost(ctx context.Context, id int, ch, msg string) error {
	if s.failSetPost {
		return errors.New("disk I/O error")
	}
	return s.Store.SetRequestPost(ctx, id, ch, msg)
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
		store:     &faultyStore{Store: st},
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
	if field(e, "Plex account") != "`a•••@e•••.com`" || field(e, "Member") != "<@42> (Ann)" {
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
	want := "Plex invite sent to `a•••@e•••.com`\nMedia role added\nWelcome DM sent"
	if text != "Approved.\n"+want {
		t.Errorf("reply = %q", text)
	}
	m := fd.msgs[post.ID]
	e := m.Embeds[0]
	if field(e, fieldStatus) != "Approved by Bob" || field(e, "Onboarding") != want || e.Color != colorApproved || len(m.Components) != 0 {
		t.Errorf("post after approval: %+v, components %d", e, len(m.Components))
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusApproved || r.DecidedBy != "Bob" || r.Plex != "a•••@e•••.com" {
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
	if !strings.Contains(text, "Unknown user") || !strings.Contains(text, "request is closed") {
		t.Errorf("reply = %q", text)
	}
	if len(fd.roles[member]) != 0 {
		t.Error("role given although the invite failed")
	}
	if dms := fd.dms(member); len(dms) != 1 || !strings.Contains(dms[0], "press Request access again") {
		t.Errorf("DMs = %q", dms)
	}
	m := fd.msgs[post.ID]
	e := m.Embeds[0]
	if field(e, fieldStatus) != "Invite failed (approved by Bob)" || !strings.Contains(field(e, "Problem"), "Unknown user") || len(m.Components) != 0 {
		t.Errorf("post after failed invite: %+v, components %d", e.Fields, len(m.Components))
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusFailed {
		t.Errorf("status = %q, want failed", r.Status)
	}
	if text, _ := a.approve(ctx, 1, "Bob"); text != textHandled {
		t.Errorf("approve after failure = %q", text)
	}

	// The member isn't stuck: they can ask again, and this time it works.
	if why, _ := a.canRequest(ctx, member, false); why != "" {
		t.Fatalf("after a failed invite: %q", why)
	}
	fp.failInvite = false
	if text, err := a.submit(ctx, member, "Ann", "ann@example.com", ""); err != nil || text != textRequestSent {
		t.Fatalf("new request = %q, %v", text, err)
	}
	if text, err := a.approve(ctx, 2, "Bob"); err != nil || !strings.HasPrefix(text, "Approved.") {
		t.Errorf("second approval = %q, %v", text, err)
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

func TestMaskAccount(t *testing.T) {
	tests := map[string]string{
		"discord2@exampledom.com": "d•••@e•••.com",
		"a@b.co.uk":               "a•••@b•••.uk",
		"x@localhost":             "x•••@l•••",
		"@example.com":            "•••@e•••.com",
		"plexuser":                "plexuser",
	}
	for in, want := range tests {
		if got := maskAccount(in); got != want {
			t.Errorf("maskAccount(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAccessNoEmailLeaks checks that the full address appears in no post,
// reply or DM, whichever way a request goes.
func TestAccessNoEmailLeaks(t *testing.T) {
	ctx := context.Background()
	const email = "ann@example.com"
	for _, path := range []string{"approve", "fail then approve", "deny"} {
		a, fd, fp, st := newTestAccess(t)
		submitted(t, a, fd)
		var replies []string
		last := 1
		if path == "fail then approve" {
			fp.failInvite, fp.echo = true, true
			text, _ := a.approve(ctx, 1, "Bob")
			replies = append(replies, text)
			fp.failInvite = false
			submitted(t, a, fd)
			last = 2
		}
		var text string
		if path == "deny" {
			text, _ = a.deny(ctx, last, "Bob")
		} else {
			text, _ = a.approve(ctx, last, "Bob")
		}
		replies = append(replies, text)

		for _, m := range fd.msgs {
			b, _ := json.Marshal(m)
			if strings.Contains(strings.ToLower(string(b)), email) {
				t.Errorf("message %s shows the address: %s", m.ID, b)
			}
		}
		for _, r := range replies {
			if strings.Contains(strings.ToLower(r), email) {
				t.Errorf("reply shows the address: %q", r)
			}
		}
		for id := 1; id <= last; id++ {
			if r, _, _ := st.Request(ctx, id); strings.Contains(r.Plex, email) {
				t.Errorf("request %d keeps the address: %q", id, r.Plex)
			}
		}
		if want := map[bool][]string{true: nil, false: {email}}[path == "deny"]; !slices.Equal(fp.invited, want) {
			t.Errorf("%s: invited = %v, want %v", path, fp.invited, want)
		}
	}
}

func TestAccessRecoverInterrupted(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	before := submitted(t, a, fd) // request 1: stopped before the invite
	a.submit(ctx, member+1, "Cy", "cy@example.com", "")
	after := fd.last() // request 2: stopped after the invite

	for id, by := range map[int]string{1: "Bob", 2: "Bob"} {
		if ok, err := st.Claim(ctx, id, store.StatusPending, store.StatusProcessing, by); !ok || err != nil {
			t.Fatalf("claim %d: %v, %v", id, ok, err)
		}
	}
	if err := st.MarkInvited(ctx, 2, 0); err != nil {
		t.Fatal(err)
	}

	if err := a.recoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	r1, _, _ := st.Request(ctx, 1)
	if r1.Status != store.StatusPending || field(fd.msgs[before.ID].Embeds[0], "Note") != textInterrupted ||
		len(customIDs(fd.msgs[before.ID].Components)) != 2 {
		t.Errorf("not invited: status %q, post %+v", r1.Status, fd.msgs[before.ID].Embeds[0].Fields)
	}
	r2, _, _ := st.Request(ctx, 2)
	if r2.Status != store.StatusApproved || !slices.Equal(fd.roles[member+1], []snowflake.ID{mediaRole}) {
		t.Errorf("invited: status %q, roles %v", r2.Status, fd.roles[member+1])
	}
	if e := fd.msgs[after.ID].Embeds[0]; field(e, fieldStatus) != "Approved by Bob" ||
		!strings.Contains(field(e, "Onboarding"), "Finished after the bot restarted") {
		t.Errorf("invited post: %+v", e.Fields)
	}
	if len(fp.invited) != 0 {
		t.Errorf("recovery sent invites again: %v", fp.invited)
	}
	// The reopened request works as normal.
	if text, err := a.approve(ctx, 1, "Bob"); err != nil || !strings.HasPrefix(text, "Approved.") {
		t.Errorf("approve after recovery = %q, %v", text, err)
	}
}

// cancelAfterInvite ends the click's context as soon as the invite has
// gone out, as a slow plex.tv answer would.
type cancelAfterInvite struct {
	plexSharer
	cancel func()
}

func (c cancelAfterInvite) Invite(ctx context.Context, who string) (int, error) {
	defer c.cancel()
	return c.plexSharer.Invite(ctx, who)
}

func TestAccessApproveOutlivesContext(t *testing.T) {
	a, fd, _, st := newTestAccess(t)
	submitted(t, a, fd)
	ctx, cancel := context.WithCancel(context.Background())
	a.plex = cancelAfterInvite{plexSharer: a.plex, cancel: cancel}

	text, err := a.approve(ctx, 1, "Bob")
	if err != nil || !strings.HasPrefix(text, "Approved.") {
		t.Fatalf("approve = %q, %v", text, err)
	}
	if r, _, _ := st.Request(context.Background(), 1); r.Status != store.StatusApproved || r.InvitedAt.IsZero() {
		t.Errorf("status %q, invited %v: the closing steps should still run", r.Status, r.InvitedAt)
	}
	if !slices.Equal(fd.roles[member], []snowflake.ID{mediaRole}) {
		t.Errorf("roles = %v", fd.roles[member])
	}
}

func TestAccessDecideFails(t *testing.T) {
	ctx := context.Background()
	a, fd, _, st := newTestAccess(t)
	submitted(t, a, fd)
	a.store.(*faultyStore).failDecide = true

	if _, err := a.approve(ctx, 1, "Bob"); err == nil {
		t.Fatal("want an error when the approval can't be saved")
	}
	// The invite went out, so a restart finishes the job.
	a.store.(*faultyStore).failDecide = false
	if err := a.recoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusApproved {
		t.Errorf("after recovery: status %q", r.Status)
	}
}

func TestAccessSubmitSavePostFails(t *testing.T) {
	ctx := context.Background()
	a, fd, _, st := newTestAccess(t)
	a.store.(*faultyStore).failSetPost = true
	if _, err := a.submit(ctx, member, "Ann", "ann@example.com", ""); err == nil {
		t.Fatal("want error")
	}
	if len(fd.deleted) != 1 || len(fd.msgs) != 0 {
		t.Errorf("post not taken back: deleted %v, left %d", fd.deleted, len(fd.msgs))
	}
	if open, _ := st.OpenRequest(ctx, member.String()); open {
		t.Error("member left with an open request")
	}
}

func TestAccessApproveDenyRace(t *testing.T) {
	ctx := context.Background()
	for range 20 {
		a, fd, fp, st := newTestAccess(t)
		submitted(t, a, fd)
		var wg sync.WaitGroup
		texts := make([]string, 2)
		for i, run := range []func(context.Context, int, string) (string, error){a.approve, a.deny} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				texts[i], _ = run(ctx, 1, "Bob")
			}()
		}
		wg.Wait()
		handled := 0
		for _, tx := range texts {
			if tx == textHandled {
				handled++
			}
		}
		r, _, _ := st.Request(ctx, 1)
		won := map[string]bool{store.StatusApproved: len(fp.invited) == 1, store.StatusDenied: len(fp.invited) == 0}
		if handled != 1 || !won[r.Status] {
			t.Fatalf("replies %q, status %q, invites %d: exactly one click should act", texts, r.Status, len(fp.invited))
		}
	}
}
