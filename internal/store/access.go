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
	DecidedBy string // the approver, set when they claim the request
	CreatedAt time.Time
	DecidedAt time.Time
	InvitedAt time.Time // zero until the Plex invite has gone out

	// ShareID is the plex.tv share the approval created; 0 if unknown,
	// for example for approvals made before it was recorded.
	ShareID    int
	ShareState string
}

// Share states of an approved request: what is being done about its Plex
// share once the member leaves or loses the media role.
const (
	ShareActive   = ""         // nothing to do
	ShareAsked    = "asked"    // approvers were asked to remove or keep it
	ShareKept     = "kept"     // an approver chose to keep it
	ShareRemoving = "removing" // an approver's Remove click is being acted on
	ShareRemoved  = "removed"
)

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

const requestColumns = `id, user_id, user_name, plex, note, status, channel_id, message_id,
	decided_by, created_at, decided_at, invited_at, plex_share_id, share_state`

type scanner interface{ Scan(dest ...any) error }

func scanRequest(row scanner) (AccessRequest, error) {
	var (
		r                         AccessRequest
		created, decided, invited string
	)
	err := row.Scan(&r.ID, &r.UserID, &r.UserName, &r.Plex, &r.Note, &r.Status, &r.ChannelID, &r.MessageID,
		&r.DecidedBy, &created, &decided, &invited, &r.ShareID, &r.ShareState)
	if err != nil {
		return AccessRequest{}, err
	}
	for _, t := range []struct {
		dst *time.Time
		s   string
	}{{&r.CreatedAt, created}, {&r.DecidedAt, decided}, {&r.InvitedAt, invited}} {
		if t.s == "" {
			continue
		}
		if *t.dst, err = time.Parse(time.RFC3339, t.s); err != nil {
			return AccessRequest{}, fmt.Errorf("parse time %q: %w", t.s, err)
		}
	}
	return r, nil
}

// Request returns a request. ok is false if there is none.
func (s *Store) Request(ctx context.Context, id int) (AccessRequest, bool, error) {
	r, err := scanRequest(s.db.QueryRowContext(ctx,
		"SELECT "+requestColumns+" FROM access_request WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessRequest{}, false, nil
	}
	if err != nil {
		return AccessRequest{}, false, fmt.Errorf("get access request %d: %w", id, err)
	}
	return r, true, nil
}

// Processing lists the requests an approver's click was acting on, for
// example when the bot stopped halfway through.
func (s *Store) Processing(ctx context.Context) ([]AccessRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+requestColumns+" FROM access_request WHERE status = ? ORDER BY id", StatusProcessing)
	if err != nil {
		return nil, fmt.Errorf("list processing access requests: %w", err)
	}
	defer rows.Close()
	var out []AccessRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("list processing access requests: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list processing access requests: %w", err)
	}
	return out, nil
}

// MarkInvited records that the Plex invite for a request went out, and
// the plex.tv share it created, or 0 if that isn't known.
func (s *Store) MarkInvited(ctx context.Context, id, shareID int) error {
	_, err := s.db.ExecContext(ctx, "UPDATE access_request SET invited_at = ?, plex_share_id = ? WHERE id = ?",
		time.Now().UTC().Format(time.RFC3339), shareID, id)
	if err != nil {
		return fmt.Errorf("mark access request %d invited: %w", id, err)
	}
	return nil
}

// Claim moves a request from one status to another, recording by as the
// approver acting on it, and reports whether it was in the from status.
// Approvers claim a pending request before acting on it, so two clicks
// can't both act.
func (s *Store) Claim(ctx context.Context, id int, from, to, by string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET status = ?, decided_by = ? WHERE id = ? AND status = ?", to, by, id, from)
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
// account with plex, normally a masked form: it isn't needed any more, so no
// copy of it is kept on disk either.
func (s *Store) Decide(ctx context.Context, id int, status, decidedBy, plex string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET status = ?, decided_by = ?, decided_at = ?, plex = ? WHERE id = ?",
		status, decidedBy, time.Now().UTC().Format(time.RFC3339), plex, id)
	if err != nil {
		return fmt.Errorf("decide access request %d: %w", id, err)
	}
	// secure_delete clears the old value from the database file, but the
	// WAL still holds the page as it was until a checkpoint. If a reader
	// is busy, SQLite reports that in the result instead of an error and
	// leaves the rest to the next checkpoint.
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("decide access request %d: checkpoint: %w", id, err)
	}
	return nil
}

// Share returns a member's newest approved request with a Plex share that
// hasn't been removed. ok is false if there is none.
func (s *Store) Share(ctx context.Context, userID string) (AccessRequest, bool, error) {
	r, err := scanRequest(s.db.QueryRowContext(ctx,
		"SELECT "+requestColumns+` FROM access_request
		 WHERE user_id = ? AND status = ? AND plex_share_id > 0 AND share_state != ?
		 ORDER BY id DESC LIMIT 1`,
		userID, StatusApproved, ShareRemoved))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessRequest{}, false, nil
	}
	if err != nil {
		return AccessRequest{}, false, fmt.Errorf("get plex share of member %s: %w", userID, err)
	}
	return r, true, nil
}

// SetShareState moves a request's share from one of the states in from to
// to, and reports whether it was in one of them, so two events or clicks
// can't both act on it.
func (s *Store) SetShareState(ctx context.Context, id int, to string, from ...string) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("set share state: no from state")
	}
	args := []any{to, id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET share_state = ? WHERE id = ? AND share_state IN (?"+
			strings.Repeat(", ?", len(from)-1)+")", args...)
	if err != nil {
		return false, fmt.Errorf("set share state of access request %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set share state of access request %d: %w", id, err)
	}
	return n == 1, nil
}

// Removing lists the requests whose share an approver's Remove click was
// acting on, for example when the bot stopped halfway through.
func (s *Store) Removing(ctx context.Context) ([]AccessRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+requestColumns+" FROM access_request WHERE share_state = ? ORDER BY id", ShareRemoving)
	if err != nil {
		return nil, fmt.Errorf("list access requests being removed: %w", err)
	}
	defer rows.Close()
	var out []AccessRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("list access requests being removed: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list access requests being removed: %w", err)
	}
	return out, nil
}
