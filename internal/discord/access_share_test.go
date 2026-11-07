package discord

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/store"
)

// approved files a request for member and approves it.
func approved(t *testing.T, a *access, fd *fakeAccessDiscord) {
	t.Helper()
	submitted(t, a, fd)
	if text, err := a.approve(context.Background(), 1, "Bob"); err != nil || !strings.HasPrefix(text, "Approved.") {
		t.Fatalf("approve = %q, %v", text, err)
	}
}

// shareState returns request 1's share state.
func shareState(t *testing.T, st *store.Store) string {
	t.Helper()
	r, _, err := st.Request(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return r.ShareState
}

// questions returns the Remove access posts made so far.
func questions(fd *fakeAccessDiscord) []*discord.Message {
	var out []*discord.Message
	for _, id := range fd.created {
		if m, ok := fd.msgs[id]; ok && len(m.Embeds) > 0 && m.Embeds[0].Title == "Remove Plex access?" {
			out = append(out, m)
		}
	}
	return out
}

func TestShareLifecycle(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	approved(t, a, fd)
	if r, _, _ := st.Request(ctx, 1); r.ShareID != 100 {
		t.Fatalf("share ID = %d, want 100", r.ShareID)
	}

	// Role changes that keep the media role ask nothing.
	if err := a.memberChanged(ctx, member, []snowflake.ID{mediaRole, 5}); err != nil || len(questions(fd)) != 0 {
		t.Fatalf("kept role: %v, %d questions", err, len(questions(fd)))
	}

	// Losing it asks once, however many updates follow.
	for range 2 {
		if err := a.memberChanged(ctx, member, []snowflake.ID{5}); err != nil {
			t.Fatal(err)
		}
	}
	qs := questions(fd)
	if len(qs) != 1 || shareState(t, st) != store.ShareAsked {
		t.Fatalf("lost role: %d questions, state %q", len(qs), shareState(t, st))
	}
	q := qs[0]
	if q.ChannelID != accessChannel || !strings.Contains(q.Embeds[0].Description, textLostRole) ||
		!slices.Equal(customIDs(q.Components), []string{"access:unshare:1", "access:keep:1"}) {
		t.Errorf("question = %+v, buttons %v", q.Embeds[0], customIDs(q.Components))
	}
	if acct := field(q.Embeds[0], "Plex account"); strings.Contains(acct, "ann@example.com") || !strings.Contains(acct, "a•••@e•••.com") {
		t.Errorf("question shows account %q", acct)
	}

	// Keep settles the post; the role coming back clears the choice.
	if text, err := a.keepShare(ctx, 1, "Bob", *q); err != nil || !strings.HasPrefix(text, "Kept.") {
		t.Fatalf("keep = %q, %v", text, err)
	}
	if field(q.Embeds[0], fieldStatus) != "Kept by Bob" || len(q.Components) != 0 || shareState(t, st) != store.ShareKept {
		t.Errorf("after keep: status %q, buttons %v, state %q", field(q.Embeds[0], fieldStatus), customIDs(q.Components), shareState(t, st))
	}
	if text, _ := a.keepShare(ctx, 1, "Cy", *q); text != textHandled {
		t.Errorf("second keep = %q", text)
	}
	if err := a.memberChanged(ctx, member, []snowflake.ID{mediaRole}); err != nil || shareState(t, st) != store.ShareActive {
		t.Fatalf("role back: %v, state %q", err, shareState(t, st))
	}

	// Leaving asks again.
	if err := a.memberLeft(ctx, member); err != nil {
		t.Fatal(err)
	}
	qs = questions(fd)
	if len(qs) != 2 || !strings.Contains(qs[1].Embeds[0].Description, textLeft) {
		t.Fatalf("left: %d questions", len(qs))
	}
	q = qs[1]

	// A failed removal changes nothing and can be pressed again.
	fp.failRemove = true
	text, err := a.removeShare(ctx, 1, "Bob", *q)
	if err != nil || !strings.HasPrefix(text, "Couldn't remove") || shareState(t, st) != store.ShareAsked || len(q.Components) == 0 {
		t.Fatalf("failed remove = %q, %v, state %q", text, err, shareState(t, st))
	}
	fp.failRemove = false
	if text, err := a.removeShare(ctx, 1, "Bob", *q); err != nil || text != "Plex access removed." {
		t.Fatalf("remove = %q, %v", text, err)
	}
	if !slices.Equal(fp.removed, []int{100}) || shareState(t, st) != store.ShareRemoved ||
		field(q.Embeds[0], fieldStatus) != "Plex access removed by Bob" || len(q.Components) != 0 {
		t.Errorf("after remove: removed %v, state %q, status %q", fp.removed, shareState(t, st), field(q.Embeds[0], fieldStatus))
	}
	if text, _ := a.removeShare(ctx, 1, "Cy", *q); text != textHandled || len(fp.removed) != 1 {
		t.Errorf("second remove = %q, removed %v", text, fp.removed)
	}

	// Nothing more to ask about a removed share.
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 2 {
		t.Errorf("after removal: %v, %d questions", err, len(questions(fd)))
	}
	// A new request is possible again, as the member has no media role.
	if why, err := a.canRequest(ctx, member, false); err != nil || why != "" {
		t.Errorf("canRequest = %q, %v", why, err)
	}
}

// Members never approved, approved without a known share, or approved
// before shares were recorded are left alone.
func TestShareUnknown(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	submitted(t, a, fd)
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 0 {
		t.Errorf("pending request: %v, %d questions", err, len(questions(fd)))
	}

	// plex.tv names no share and doesn't list one: still approved, with
	// a note for the approver.
	fp.noShareID = true
	text, err := a.approve(ctx, 1, "Bob")
	if err != nil || !strings.HasPrefix(text, "Approved.") || !strings.Contains(text, "remove their Plex access by hand") {
		t.Fatalf("approve = %q, %v", text, err)
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusApproved || r.ShareID != 0 {
		t.Errorf("status %q, share %d", r.Status, r.ShareID)
	}
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 0 {
		t.Errorf("unknown share: %v, %d questions", err, len(questions(fd)))
	}

	// As an approval from before shares were recorded looks.
	a2, fd2, _, st2 := newTestAccess(t)
	submitted(t, a2, fd2)
	ok, _ := st2.Claim(ctx, 1, store.StatusPending, store.StatusProcessing, "Bob")
	if !ok || st2.Decide(ctx, 1, store.StatusApproved, "Bob", "a•••@e•••.com") != nil {
		t.Fatal("set up old approval")
	}
	if err := a2.memberLeft(ctx, member); err != nil || len(questions(fd2)) != 0 {
		t.Errorf("old approval: %v, %d questions", err, len(questions(fd2)))
	}
}

func TestShareQuestionPostFails(t *testing.T) {
	ctx := context.Background()
	a, fd, _, st := newTestAccess(t)
	approved(t, a, fd)
	fd.fail = errors.New("discord down")
	if err := a.memberLeft(ctx, member); err == nil {
		t.Fatal("want error")
	}
	if s := shareState(t, st); s != store.ShareActive {
		t.Fatalf("state %q after a failed post; the next event should ask again", s)
	}
	fd.fail = nil
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 1 {
		t.Errorf("retry: %v, %d questions", err, len(questions(fd)))
	}
}

func TestShareRecoverRemoving(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	approved(t, a, fd)
	if err := a.memberLeft(ctx, member); err != nil {
		t.Fatal(err)
	}
	// The bot stopped after claiming the removal.
	if ok, err := st.SetShareState(ctx, 1, store.ShareRemoving, store.ShareAsked); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := a.recoverRemoving(ctx); err != nil || shareState(t, st) != store.ShareAsked {
		t.Fatalf("recover: %v, state %q", err, shareState(t, st))
	}
	if text, err := a.removeShare(ctx, 1, "Bob", *questions(fd)[0]); err != nil || text != "Plex access removed." || len(fp.removed) != 1 {
		t.Errorf("remove after recovery = %q, %v, removed %v", text, err, fp.removed)
	}
}
