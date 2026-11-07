// Package store keeps the bot's state in SQLite: which Discord message
// belongs to which Seerr issue.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the SQLite database.
type Store struct {
	db *sql.DB
}

// Post is the Discord message posted for a Seerr issue.
type Post struct {
	IssueID   int
	ChannelID string
	MessageID string
	CreatedAt time.Time
}

// Open opens the database at path, creating the file and its directory if
// needed, and applies any pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping checks that the database can be reached.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// migrate applies each embedded migration numbered above the database's
// user_version, in order, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	slices.Sort(names)
	for _, name := range names {
		base := filepath.Base(name)
		num, _, _ := strings.Cut(base, "_")
		version, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", base)
		}
		if version <= current {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", base, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", base, err)
		}
		// PRAGMA takes no parameters; version is an integer parsed above.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("set schema version %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", base, err)
		}
		current = version
	}
	return nil
}

// Post returns the post recorded for an issue. ok is false if there is none.
func (s *Store) Post(ctx context.Context, issueID int) (p Post, ok bool, err error) {
	var created string
	err = s.db.QueryRowContext(ctx,
		"SELECT issue_id, channel_id, message_id, created_at FROM issue_post WHERE issue_id = ?",
		issueID).Scan(&p.IssueID, &p.ChannelID, &p.MessageID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Post{}, false, nil
	}
	if err != nil {
		return Post{}, false, fmt.Errorf("get post for issue %d: %w", issueID, err)
	}
	p.CreatedAt, err = time.Parse(time.RFC3339, created)
	if err != nil {
		return Post{}, false, fmt.Errorf("get post for issue %d: parse created_at: %w", issueID, err)
	}
	return p, true, nil
}

// SavePost records the post for an issue, replacing any earlier one.
func (s *Store) SavePost(ctx context.Context, p Post) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO issue_post (issue_id, channel_id, message_id, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (issue_id) DO UPDATE SET channel_id = excluded.channel_id,
		     message_id = excluded.message_id, created_at = excluded.created_at`,
		p.IssueID, p.ChannelID, p.MessageID, p.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save post for issue %d: %w", p.IssueID, err)
	}
	return nil
}
