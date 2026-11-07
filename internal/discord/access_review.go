package discord

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// The member checks what they typed before the request is sent: the form
// leads to a review only they can see, with Send request and Edit buttons.
const (
	idAccessSend = "access:send:"
	idAccessEdit = "access:edit:"

	textDraftExpired = "This has expired. Press Request access again."
)

// draft is a filled-in request form waiting for the member to send it.
type draft struct {
	UserID  snowflake.ID
	Plex    string
	Note    string
	expires time.Time
}

// draftStore keeps drafts in memory, keyed by a random token in the review
// buttons' custom IDs. A restart forgets them; the member starts again.
type draftStore struct {
	mu     sync.Mutex
	drafts map[string]draft
	now    func() time.Time
}

func newDraftStore() *draftStore {
	return &draftStore{drafts: map[string]draft{}, now: time.Now}
}

// put stores a draft and returns its token.
func (s *draftStore) put(d draft) string {
	b := make([]byte, 8)
	rand.Read(b) // crypto/rand.Read never returns an error
	token := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, old := range s.drafts {
		if now.After(old.expires) {
			delete(s.drafts, k)
		}
	}
	d.expires = now.Add(pendingTTL)
	s.drafts[token] = d
	return token
}

// get returns the member's draft for token, if it hasn't expired.
func (s *draftStore) get(token string, userID snowflake.ID) (draft, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[token]
	if !ok || d.UserID != userID || s.now().After(d.expires) {
		return draft{}, false
	}
	return d, true
}

// take removes and returns the member's draft, so it is sent only once,
// even if they click twice.
func (s *draftStore) take(token string, userID snowflake.ID) (draft, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[token]
	if !ok || d.UserID != userID || s.now().After(d.expires) {
		return draft{}, false
	}
	delete(s.drafts, token)
	return d, true
}

var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s.]{2,}$`)

// checkPlexAccount returns a problem with what the member typed, or "".
// Anything with an @ must look like an email address; anything else is
// taken as a Plex username.
func checkPlexAccount(s string) string {
	switch {
	case strings.TrimSpace(s) == "":
		return "Please enter your Plex email address or username."
	case strings.Contains(s, "@") && !emailPattern.MatchString(s):
		return "That doesn't look like an email address. Check it and try again."
	}
	return ""
}

// reviewMessage shows the member what they typed, in full, since only they
// see it, and asks them to send it or edit it. problem, if set, explains
// why it can't be sent yet, and only Edit is offered.
func reviewMessage(d draft, token, problem string) discord.MessageCreate {
	var b strings.Builder
	if problem != "" {
		b.WriteString(problem + "\n\nYou entered: " + code(d.Plex))
	} else {
		b.WriteString("An approver will be asked to invite this Plex account:\n" + code(d.Plex) +
			"\n\nIs that right? If the email address is wrong, the invite goes to someone else.")
	}
	if d.Note != "" {
		b.WriteString("\n\nYour note: " + escape(truncate(d.Note, 500)))
	}
	buttons := []discord.InteractiveComponent{discord.NewSecondaryButton("Edit", idAccessEdit+token)}
	if problem == "" {
		buttons = append([]discord.InteractiveComponent{discord.NewSuccessButton("Send request", idAccessSend+token)}, buttons...)
	}
	return discord.NewMessageCreate().
		WithContent(b.String()).
		WithEphemeral(true).
		AddActionRow(buttons...)
}
