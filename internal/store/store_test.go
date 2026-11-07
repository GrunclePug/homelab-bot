package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPosts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "bot.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.Post(ctx, 1); err != nil || ok {
		t.Fatalf("empty store: ok %v, err %v", ok, err)
	}
	when := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	if err := s.SavePost(ctx, Post{IssueID: 1, ChannelID: "10", MessageID: "20", CreatedAt: when}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePost(ctx, Post{IssueID: 1, ChannelID: "10", MessageID: "21", CreatedAt: when}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.Post(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if p.MessageID != "21" || p.ChannelID != "10" || !p.CreatedAt.Equal(when) {
		t.Errorf("post = %+v", p)
	}
	if err := s.Ping(ctx); err != nil {
		t.Error(err)
	}
	s.Close()

	// Reopening keeps the data and doesn't rerun migrations.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Errorf("user_version = %d, %v", version, err)
	}
	if _, ok, _ := s.Post(ctx, 1); !ok {
		t.Error("post lost after reopen")
	}
}
