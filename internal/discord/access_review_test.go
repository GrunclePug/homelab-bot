package discord

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
)

func TestCheckPlexAccount(t *testing.T) {
	ok := []string{"ann@example.com", "a.b+plex@mail.example.co.uk", "plexuser", "Plex_User-1"}
	bad := []string{"", "  ", "ann@example", "ann@@example.com", "ann@example.c", "ann @example.com", "@example.com", "ann@.com"}
	for _, s := range ok {
		if p := checkPlexAccount(s); p != "" {
			t.Errorf("checkPlexAccount(%q) = %q, want ok", s, p)
		}
	}
	for _, s := range bad {
		if checkPlexAccount(s) == "" {
			t.Errorf("checkPlexAccount(%q) passed, want a problem", s)
		}
	}
}

func buttonIDs(m discord.MessageCreate) []string { return customIDs(m.Components) }

func TestReviewMessage(t *testing.T) {
	d := draft{UserID: member, Plex: "ann@example.com", Note: "Bob's *friend*"}
	m := reviewMessage(d, "tok", "")
	if m.Flags&discord.MessageFlagEphemeral == 0 {
		t.Error("review is not ephemeral")
	}
	// The member sees their own address in full, to spot a typo.
	for _, want := range []string{"`ann@example.com`", "Is that right?", `Bob's \*friend\*`} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("review lacks %q:\n%s", want, m.Content)
		}
	}
	if ids := buttonIDs(m); !slices.Equal(ids, []string{"access:send:tok", "access:edit:tok"}) {
		t.Errorf("buttons = %v", ids)
	}

	m = reviewMessage(draft{Plex: "ann@example"}, "tok", checkPlexAccount("ann@example"))
	if !strings.Contains(m.Content, "doesn't look like an email") {
		t.Errorf("problem not shown:\n%s", m.Content)
	}
	if ids := buttonIDs(m); !slices.Equal(ids, []string{"access:edit:tok"}) {
		t.Errorf("with a problem, buttons = %v, want only Edit", ids)
	}
}

func TestRequestFormPrefill(t *testing.T) {
	values := func(m discord.ModalCreate) map[string]string {
		got := map[string]string{}
		for _, c := range m.Components {
			if l, ok := c.(discord.LabelComponent); ok {
				if ti, ok := l.Component.(discord.TextInputComponent); ok {
					got[ti.CustomID] = ti.Value
				}
			}
		}
		return got
	}
	blank := requestForm(idAccessForm, draft{})
	if blank.CustomID != idAccessForm || values(blank)[inputPlex] != "" {
		t.Errorf("blank form: id %q, values %v", blank.CustomID, values(blank))
	}
	again := requestForm(idAccessReform, draft{Plex: "ann@example", Note: "hi"})
	if v := values(again); again.CustomID != idAccessReform || v[inputPlex] != "ann@example" || v[inputNote] != "hi" {
		t.Errorf("prefilled form: id %q, values %v", again.CustomID, v)
	}
}

func TestDraftStore(t *testing.T) {
	s := newDraftStore()
	now := s.now()
	s.now = func() time.Time { return now }
	tok := s.put(draft{UserID: member, Plex: "ann"})

	if _, ok := s.get(tok, member+1); ok {
		t.Error("another member read the draft")
	}
	if d, ok := s.get(tok, member); !ok || d.Plex != "ann" {
		t.Fatalf("get = %+v, %v", d, ok)
	}
	if _, ok := s.take(tok, member); !ok {
		t.Fatal("take failed")
	}
	if _, ok := s.take(tok, member); ok {
		t.Error("draft sent twice")
	}
	tok = s.put(draft{UserID: member})
	s.now = func() time.Time { return now.Add(pendingTTL + time.Second) }
	if _, ok := s.get(tok, member); ok {
		t.Error("expired draft still there")
	}
}

func TestDrain(t *testing.T) {
	b := &Bot{}
	if !b.enter() {
		t.Fatal("enter refused before shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Drain(ctx); err == nil {
		t.Error("Drain returned while a handler was running")
	}
	if b.enter() {
		t.Error("enter accepted a handler after shutdown began")
	}
	b.handlers.Done()
	if err := b.Drain(context.Background()); err != nil {
		t.Errorf("Drain after the handler finished: %v", err)
	}
}
