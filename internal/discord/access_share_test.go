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
	if r, _, _ := st.Request(ctx, 1); r.PlexAccount != "id:100" {
		t.Fatalf("plex account = %q, want id:100", r.PlexAccount)
	}

	// Role changes that keep the media role ask nothing.
	if err := a.memberChanged(ctx, member, []snowflake.ID{mediaRole}, true, []snowflake.ID{mediaRole, 5}); err != nil || len(questions(fd)) != 0 {
		t.Fatalf("kept role: %v, %d questions", err, len(questions(fd)))
	}

	// Losing it asks once, however many updates follow.
	for range 2 {
		if err := a.memberChanged(ctx, member, []snowflake.ID{mediaRole, 5}, true, []snowflake.ID{5}); err != nil {
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
	if err := a.memberChanged(ctx, member, []snowflake.ID{5}, true, []snowflake.ID{mediaRole}); err != nil || shareState(t, st) != store.ShareActive {
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
	if !slices.Equal(fp.removed, []string{"/api/invites/requested/100"}) || shareState(t, st) != store.ShareRemoved ||
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

	// plex.tv doesn't list the invite: still approved, with a note for
	// the approver.
	fp.unlisted = true
	text, err := a.approve(ctx, 1, "Bob")
	if err != nil || !strings.HasPrefix(text, "Approved.") || !strings.Contains(text, "remove it by hand") {
		t.Fatalf("approve = %q, %v", text, err)
	}
	if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusApproved || r.PlexAccount != "" {
		t.Errorf("status %q, account %q", r.Status, r.PlexAccount)
	}
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 0 {
		t.Errorf("unknown account: %v, %d questions", err, len(questions(fd)))
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

// roleChange is a member update taking roles from old to roles.
func roleChange(t *testing.T, a *access, old, roles []snowflake.ID) {
	t.Helper()
	if err := a.memberChanged(context.Background(), member, old, true, roles); err != nil {
		t.Fatal(err)
	}
}

// The member's own earlier account: they lost the media role, a card was
// posted, and they asked again. The card is retired, the new approval
// tracks the account, and the old card can't remove it.
func TestReapproveRetiresCard(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	approved(t, a, fd) // request 1, a pending invite for account 100
	roleChange(t, a, []snowflake.ID{mediaRole}, nil)
	old := questions(fd)[0]

	text, err := a.submit(ctx, member, "Ann", "ann@example.com", "")
	if err != nil || text != textRequestSent {
		t.Fatalf("submit = %q, %v", text, err)
	}
	text, err = a.approve(ctx, 2, "Bob")
	if err != nil || !strings.Contains(text, "already had Plex access, so no new invite was sent") || strings.Contains(text, "by hand") {
		t.Fatalf("approve = %q, %v", text, err)
	}
	r1, _, _ := st.Request(ctx, 1)
	r2, _, _ := st.Request(ctx, 2)
	if r1.ShareState != store.ShareSuperseded || r2.PlexAccount != "id:100" || !r2.PlexAlready {
		t.Errorf("request 1 %q; request 2 account %q, already %v", r1.ShareState, r2.PlexAccount, r2.PlexAlready)
	}
	if status := field(old.Embeds[0], fieldStatus); status != "Approved again in request #2, so this no longer applies" || len(old.Components) != 0 {
		t.Errorf("old card: status %q, buttons %v", status, customIDs(old.Components))
	}
	if text, _ := a.removeShare(ctx, 1, "Cy", *old); text != textHandled || len(fp.removed) != 0 {
		t.Errorf("old card's remove = %q, removed %v", text, fp.removed)
	}
	dms := fd.dms(member)
	if !strings.Contains(dms[len(dms)-1], "Your Plex account already had access") {
		t.Errorf("welcome DM = %q", dms[len(dms)-1])
	}

	// Even a card that slipped through can't act on the newer approval.
	if ok, _ := st.SetShareState(ctx, 1, store.ShareAsked, store.ShareSuperseded); !ok {
		t.Fatal("reset state")
	}
	if text, err := a.removeShare(ctx, 1, "Cy", *old); err != nil || !strings.HasPrefix(text, "This card is out of date") || len(fp.removed) != 0 {
		t.Errorf("outdated remove = %q, %v, removed %v", text, err, fp.removed)
	}
	if s := shareState(t, st); s != store.ShareAsked {
		t.Errorf("outdated card left in %q", s)
	}

	// The new approval's access is the one asked about, and removed.
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 2 {
		t.Fatalf("leave: %v, %d questions", err, len(questions(fd)))
	}
	if text, err := a.removeShare(ctx, 2, "Bob", *questions(fd)[1]); err != nil || text != "Plex access removed." ||
		!slices.Equal(fp.removed, []string{"/api/invites/requested/100"}) {
		t.Errorf("remove = %q, %v; removed %v", text, err, fp.removed)
	}
}

// An account that already had access, and wasn't this member's before,
// may be someone else's: the member is onboarded, but it isn't tracked.
func TestApproveSomeoneElsesAccount(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name              string
		pending, accepted map[string]int
	}{
		{"pending", map[string]int{"ann@example.com": 90}, nil},
		{"accepted", nil, map[string]int{"ann@example.com": 91}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, fd, fp, st := newTestAccess(t)
			fp.pending, fp.accepted = tc.pending, tc.accepted
			submitted(t, a, fd)
			text, err := a.approve(ctx, 1, "Bob")
			if err != nil || !strings.Contains(text, "already had Plex access, so no new invite was sent") ||
				!strings.Contains(text, "had access before this request, so I won't offer to remove it") {
				t.Fatalf("approve = %q, %v", text, err)
			}
			if r, _, _ := st.Request(ctx, 1); r.Status != store.StatusApproved || r.PlexAccount != "" || !r.PlexAlready {
				t.Errorf("status %q, account %q, already %v", r.Status, r.PlexAccount, r.PlexAlready)
			}
			if !slices.Equal(fd.roles[member], []snowflake.ID{mediaRole}) {
				t.Errorf("roles %v", fd.roles[member])
			}
			if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 0 {
				t.Errorf("leave: %v, %d questions", err, len(questions(fd)))
			}
		})
	}
}

// Only losing the role asks: not a nickname change for a member whose role
// was never added, and not an update overtaken by one giving it back.
func TestRoleChangeOnlyWhenLost(t *testing.T) {
	ctx := context.Background()
	a, fd, _, _ := newTestAccess(t)
	approved(t, a, fd)

	roleChange(t, a, []snowflake.ID{5}, []snowflake.ID{5})
	if len(questions(fd)) != 0 {
		t.Error("asked about a member who never had the role")
	}
	var now []snowflake.ID
	a.currentRoles = func(snowflake.ID) ([]snowflake.ID, bool) { return now, true }
	now = []snowflake.ID{mediaRole}
	roleChange(t, a, []snowflake.ID{mediaRole}, nil)
	if len(questions(fd)) != 0 {
		t.Error("asked although a later update gave the role back")
	}
	// Not seen before: assume they lost it.
	now = nil
	if err := a.memberChanged(ctx, member, nil, false, nil); err != nil || len(questions(fd)) != 1 {
		t.Errorf("unknown old roles: %v, %d questions", err, len(questions(fd)))
	}
}

// Getting the role back retires an open card.
func TestRoleBackRetiresCard(t *testing.T) {
	a, fd, _, st := newTestAccess(t)
	approved(t, a, fd)
	roleChange(t, a, []snowflake.ID{mediaRole}, nil)
	q := questions(fd)[0]
	roleChange(t, a, nil, []snowflake.ID{mediaRole})
	if shareState(t, st) != store.ShareActive || len(q.Components) != 0 ||
		field(q.Embeds[0], fieldStatus) != "They have the media role again, so this no longer applies" {
		t.Errorf("state %q, status %q, buttons %v", shareState(t, st), field(q.Embeds[0], fieldStatus), customIDs(q.Components))
	}
	roleChange(t, a, []snowflake.ID{mediaRole}, nil)
	if len(questions(fd)) != 2 {
		t.Errorf("losing it again: %d questions", len(questions(fd)))
	}
}

// A deleted card is posted again by the next event; one still there isn't.
func TestDeletedCardPostedAgain(t *testing.T) {
	ctx := context.Background()
	a, fd, _, _ := newTestAccess(t)
	approved(t, a, fd)
	if err := a.memberLeft(ctx, member); err != nil {
		t.Fatal(err)
	}
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 1 {
		t.Fatalf("card still there: %v, %d questions", err, len(questions(fd)))
	}
	q := questions(fd)[0]
	if err := fd.DeleteMessage(q.ChannelID, q.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.memberLeft(ctx, member); err != nil || len(questions(fd)) != 1 || questions(fd)[0].ID == q.ID {
		t.Errorf("deleted card: %v, %d questions", err, len(questions(fd)))
	}
}

// Members who left or lost the role while the bot was down are asked
// about at startup.
func TestCheckMissed(t *testing.T) {
	ctx := context.Background()
	a, fd, _, _ := newTestAccess(t)
	approved(t, a, fd) // member, who still has the role
	if err := a.checkMissed(ctx); err != nil || len(questions(fd)) != 0 {
		t.Fatalf("nothing missed: %v, %d questions", err, len(questions(fd)))
	}
	fd.roles[member] = nil
	if err := a.checkMissed(ctx); err != nil || len(questions(fd)) != 1 || !strings.Contains(questions(fd)[0].Embeds[0].Description, textLostRole) {
		t.Fatalf("lost role: %v, %d questions", err, len(questions(fd)))
	}
	// Already asked: not again.
	if err := a.checkMissed(ctx); err != nil || len(questions(fd)) != 1 {
		t.Errorf("second check: %v, %d questions", err, len(questions(fd)))
	}

	a2, fd2, _, _ := newTestAccess(t)
	approved(t, a2, fd2)
	fd2.gone[member] = true
	if err := a2.checkMissed(ctx); err != nil || len(questions(fd2)) != 1 || !strings.Contains(questions(fd2)[0].Embeds[0].Description, textLeft) {
		t.Errorf("left: %v, %d questions", err, len(questions(fd2)))
	}
}

// Access removed in Plex by hand before an approver pressed Remove access.
func TestRemoveNothingLeft(t *testing.T) {
	ctx := context.Background()
	a, fd, fp, st := newTestAccess(t)
	approved(t, a, fd)
	if err := a.memberLeft(ctx, member); err != nil {
		t.Fatal(err)
	}
	fp.pending = nil
	q := questions(fd)[0]
	text, err := a.removeShare(ctx, 1, "Bob", *q)
	if err != nil || !strings.HasPrefix(text, "Couldn't find a share or invite for them on plex.tv") || len(fp.removed) != 0 ||
		shareState(t, st) != store.ShareRemoved || !strings.HasPrefix(field(q.Embeds[0], fieldStatus), "Not found on plex.tv") {
		t.Errorf("remove = %q, %v; removed %v, state %q, status %q", text, err, fp.removed, shareState(t, st), field(q.Embeds[0], fieldStatus))
	}
}
