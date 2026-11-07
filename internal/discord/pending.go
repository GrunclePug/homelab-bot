package discord

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/grunclepug/homelab-bot/internal/replace"
)

// pendingTTL is how long a Replace flow waits for its next click. Discord
// interaction tokens last 15 minutes, so the ephemeral messages can't be
// edited after that anyway.
const pendingTTL = 14 * time.Minute

// pendingReplace is a Replace flow between the button press and the
// confirmation.
type pendingReplace struct {
	IssueID int
	AdminID snowflake.ID
	Plan    replace.Plan
	Chosen  []replace.Target
	expires time.Time
}

// pendingStore keeps Replace flows in memory, keyed by a random token that
// goes into the custom IDs of the flow's components. A restart forgets
// them, and the admin presses Replace again.
type pendingStore struct {
	mu      sync.Mutex
	flows   map[string]*pendingReplace
	running map[int]bool // issues with a replacement in progress
	now     func() time.Time
}

func newPendingStore() *pendingStore {
	return &pendingStore{flows: map[string]*pendingReplace{}, running: map[int]bool{}, now: time.Now}
}

// start marks a replacement for an issue as running. It returns false if
// one already is, so two confirmed flows can't act on the same files.
func (s *pendingStore) start(issueID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[issueID] {
		return false
	}
	s.running[issueID] = true
	return true
}

// finish marks the issue's replacement as done.
func (s *pendingStore) finish(issueID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, issueID)
}

// put stores a flow and returns its token.
func (s *pendingStore) put(p *pendingReplace) string {
	b := make([]byte, 8)
	rand.Read(b) // crypto/rand.Read never returns an error
	token := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, f := range s.flows {
		if now.After(f.expires) {
			delete(s.flows, k)
		}
	}
	p.expires = now.Add(pendingTTL)
	s.flows[token] = p
	return token
}

// get returns a copy of the flow for token, if it hasn't expired.
func (s *pendingStore) get(token string) (pendingReplace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.flows[token]
	if !ok || s.now().After(p.expires) {
		return pendingReplace{}, false
	}
	return *p, true
}

// setChosen records the files picked for a flow.
func (s *pendingStore) setChosen(token string, chosen []replace.Target) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.flows[token]
	if !ok || s.now().After(p.expires) {
		return false
	}
	p.Chosen = chosen
	return true
}

// take removes and returns the flow for token, so a double click can't
// run it twice.
func (s *pendingStore) take(token string) (*pendingReplace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.flows[token]
	delete(s.flows, token)
	if !ok || s.now().After(p.expires) {
		return nil, false
	}
	return p, true
}
