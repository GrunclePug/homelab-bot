package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Access request statuses. A request is open while pending or processing.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing" // an approver's click is being acted on
	StatusApproved   = "approved"
	StatusDenied     = "denied"
	StatusFailed     = "failed" // the Plex invite failed; the member may ask again
)

// ErrOpenRequest means the member already has an open access request.
var ErrOpenRequest = errors.New("member already has an open access request")

// AccessRequest is a member's request for access to the media server.
type AccessRequest struct {
	ID        int
	UserID    string
	UserName  string
	Plex      string // Plex email or username
	Note      string
	Status    string
	ChannelID string
	MessageID string
	DecidedBy string
	CreatedAt time.Time
	DecidedAt time.Time
}

// CreateRequest records a new pending request and returns its ID. It
// returns ErrOpenRequest if the member already has one open.
func (s *Store) CreateRequest(ctx context.Context, r AccessRequest) (int, error) {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO access_request (user_id, user_name, plex, note, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.UserID, r.UserName, r.Plex, r.Note, StatusPending, r.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, ErrOpenRequest
		}
		return 0, fmt.Errorf("create access request: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create access request: %w", err)
	}
	return int(id), nil
}

// DeleteRequest removes a request, for one whose post couldn't be made.
func (s *Store) DeleteRequest(ctx context.Context, id int) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM access_request WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete access request %d: %w", id, err)
	}
	return nil
}

// SetRequestPost records the approval post for a request.
func (s *Store) SetRequestPost(ctx context.Context, id int, channelID, messageID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET channel_id = ?, message_id = ? WHERE id = ?", channelID, messageID, id)
	if err != nil {
		return fmt.Errorf("set post for access request %d: %w", id, err)
	}
	return nil
}

// OpenRequest reports whether a member has an open request.
func (s *Store) OpenRequest(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM access_request WHERE user_id = ? AND status IN (?, ?)",
		userID, StatusPending, StatusProcessing).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check open access requests: %w", err)
	}
	return n > 0, nil
}

// Request returns a request. ok is false if there is none.
func (s *Store) Request(ctx context.Context, id int) (r AccessRequest, ok bool, err error) {
	var created, decided string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, user_id, user_name, plex, note, status, channel_id, message_id, decided_by, created_at, decided_at
		 FROM access_request WHERE id = ?`, id).
		Scan(&r.ID, &r.UserID, &r.UserName, &r.Plex, &r.Note, &r.Status, &r.ChannelID, &r.MessageID,
			&r.DecidedBy, &created, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessRequest{}, false, nil
	}
	if err != nil {
		return AccessRequest{}, false, fmt.Errorf("get access request %d: %w", id, err)
	}
	if r.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
		return AccessRequest{}, false, fmt.Errorf("get access request %d: parse created_at: %w", id, err)
	}
	if decided != "" {
		if r.DecidedAt, err = time.Parse(time.RFC3339, decided); err != nil {
			return AccessRequest{}, false, fmt.Errorf("get access request %d: parse decided_at: %w", id, err)
		}
	}
	return r, true, nil
}

// Claim moves a request from one status to another, and reports whether
// it was in the from status. Approvers claim a pending request before
// acting on it, so two clicks can't both act.
func (s *Store) Claim(ctx context.Context, id int, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET status = ? WHERE id = ? AND status = ?", to, id, from)
	if err != nil {
		return false, fmt.Errorf("claim access request %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim access request %d: %w", id, err)
	}
	return n == 1, nil
}

// Decide closes a request as approved, denied or failed, and replaces the Plex
// account with plex, normally a masked form: it isn't needed any more.
func (s *Store) Decide(ctx context.Context, id int, status, decidedBy, plex string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET status = ?, decided_by = ?, decided_at = ?, plex = ? WHERE id = ?",
		status, decidedBy, time.Now().UTC().Format(time.RFC3339), plex, id)
	if err != nil {
		return fmt.Errorf("decide access request %d: %w", id, err)
	}
	return nil
}
