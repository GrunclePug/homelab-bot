package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAccessRequests(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.CreateRequest(ctx, AccessRequest{UserID: "1", UserName: "ann", Plex: "ann@example.com", Note: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRequest(ctx, AccessRequest{UserID: "1", UserName: "ann", Plex: "x"}); !errors.Is(err, ErrOpenRequest) {
		t.Errorf("second open request: err = %v, want ErrOpenRequest", err)
	}
	if open, _ := s.OpenRequest(ctx, "1"); !open {
		t.Error("OpenRequest = false")
	}
	if err := s.SetRequestPost(ctx, id, "10", "20"); err != nil {
		t.Fatal(err)
	}

	// Only one of two clicks gets the request.
	first, err1 := s.Claim(ctx, id, StatusPending, StatusProcessing, "bob")
	second, err2 := s.Claim(ctx, id, StatusPending, StatusProcessing, "cy")
	if err := errors.Join(err1, err2); err != nil || !first || second {
		t.Fatalf("claims = %v, %v (%v)", first, second, err)
	}
	if _, err := s.CreateRequest(ctx, AccessRequest{UserID: "1", Plex: "x"}); !errors.Is(err, ErrOpenRequest) {
		t.Error("a processing request should still count as open")
	}
	ps, err := s.Processing(ctx)
	if err != nil || len(ps) != 1 || ps[0].ID != id || ps[0].DecidedBy != "bob" || !ps[0].InvitedAt.IsZero() {
		t.Fatalf("Processing = %+v, %v", ps, err)
	}
	if err := s.MarkInvited(ctx, id, "id:77", false); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := s.Request(ctx, id); r.InvitedAt.IsZero() || r.PlexAccount != "id:77" {
		t.Errorf("InvitedAt %v, PlexAccount %q", r.InvitedAt, r.PlexAccount)
	}

	if err := s.Decide(ctx, id, StatusDenied, "bob", "a•••@e•••.com"); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.Request(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Request: %v, %v", ok, err)
	}
	if r.Status != StatusDenied || r.DecidedBy != "bob" || r.DecidedAt.IsZero() || r.MessageID != "20" || r.Plex != "a•••@e•••.com" {
		t.Errorf("request = %+v", r)
	}
	if open, _ := s.OpenRequest(ctx, "1"); open {
		t.Error("closed request still open")
	}

	// After a denial the member may ask again.
	id2, err := s.CreateRequest(ctx, AccessRequest{UserID: "1", UserName: "ann", Plex: "ann2"})
	if err != nil || id2 == id {
		t.Fatalf("new request after denial: %d, %v", id2, err)
	}
	if err := s.DeleteRequest(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Request(ctx, id2); ok {
		t.Error("deleted request still there")
	}
}

// TestScrubLeavesNoCopy checks that once a request is decided, its email
// address is in neither the database file nor its WAL.
func TestScrubLeavesNoCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const email = "scrub-me@example.com"
	id, err := s.CreateRequest(ctx, AccessRequest{UserID: "1", UserName: "ann", Plex: email})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, id, StatusApproved, "bob", "s•••@e•••.com"); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(email)) {
			t.Errorf("%s still holds the address", filepath.Base(f))
		}
	}
}
