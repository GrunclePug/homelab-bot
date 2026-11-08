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

	// PlexAccount is whose Plex access the approval gave, as a
	// plex.Account; "" if unknown or not tracked.
	PlexAccount string
	ShareState  string
	// The "Remove Plex access?" card, if one was posted.
	ShareChannelID string
	ShareMessageID string
	// PlexAlready means the account had access before the approval.
	PlexAlready bool
}

// Share states: what is being done about an approval's Plex access once
// the member leaves or loses the media role.
const (
	ShareActive   = ""         // nothing to do
	ShareAsked    = "asked"    // approvers were asked to remove or keep it
	ShareKept     = "kept"     // an approver chose to keep it
	ShareRemoving = "removing" // an approver's Remove click is being acted on
	ShareRemoved  = "removed"
	// ShareSuperseded hands tracking to a newer approval of the account.
	ShareSuperseded = "superseded"
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
	decided_by, created_at, decided_at, invited_at, plex_account, share_state,
	share_channel_id, share_message_id, plex_already`

type scanner interface{ Scan(dest ...any) error }

func scanRequest(row scanner) (AccessRequest, error) {
	var (
		r                         AccessRequest
		created, decided, invited string
	)
	err := row.Scan(&r.ID, &r.UserID, &r.UserName, &r.Plex, &r.Note, &r.Status, &r.ChannelID, &r.MessageID,
		&r.DecidedBy, &created, &decided, &invited, &r.PlexAccount, &r.ShareState,
		&r.ShareChannelID, &r.ShareMessageID, &r.PlexAlready)
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

// Processing lists the requests an approver's click is acting on.
func (s *Store) Processing(ctx context.Context) ([]AccessRequest, error) {
	return s.listRequests(ctx, "processing access requests", " WHERE status = ? ORDER BY id", StatusProcessing)
}

// MarkInvited records that the account has Plex access, new or already
// had, and plexAccount, its plex.Account or "" if not tracked.
func (s *Store) MarkInvited(ctx context.Context, id int, plexAccount string, already bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE access_request SET invited_at = ?, plex_account = ?, plex_already = ? WHERE id = ?",
		time.Now().UTC().Format(time.RFC3339), plexAccount, already, id)
	if err != nil {
		return fmt.Errorf("mark access request %d invited: %w", id, err)
	}
	return nil
}

// Claim moves a request from status from to to, recording by as its
// approver, and reports whether it was in from, so two clicks can't both
// act.
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

// Decide closes a request as approved, denied or failed, replacing the Plex
// account with plex, normally masked, so no copy is kept on disk.
func (s *Store) Decide(ctx context.Context, id int, status, decidedBy, plex string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET status = ?, decided_by = ?, decided_at = ?, plex = ? WHERE id = ?",
		status, decidedBy, time.Now().UTC().Format(time.RFC3339), plex, id)
	if err != nil {
		return fmt.Errorf("decide access request %d: %w", id, err)
	}
	// secure_delete clears the old value from the database file, but the
	// WAL keeps it until a checkpoint.
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("decide access request %d: checkpoint: %w", id, err)
	}
	return nil
}

// Shares lists a member's approvals whose Plex access is still tracked,
// newest first: there is one per account they were approved for.
func (s *Store) Shares(ctx context.Context, userID string) ([]AccessRequest, error) {
	return s.listRequests(ctx, "plex shares of member "+userID,
		` WHERE user_id = ? AND status = ? AND plex_account != '' AND share_state NOT IN (?, ?) ORDER BY id DESC`,
		userID, StatusApproved, ShareRemoved, ShareSuperseded)
}

// NewerApproval returns the member's newest approval after request id that
// may cover the same access: one for plexAccount, or one for an account
// that already had access but isn't known.
func (s *Store) NewerApproval(ctx context.Context, userID string, id int, plexAccount string) (newer int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM access_request
		 WHERE user_id = ? AND id > ? AND status = ? AND (plex_account = ? OR (plex_account = '' AND plex_already = 1))
		 ORDER BY id DESC LIMIT 1`,
		userID, id, StatusApproved, plexAccount).Scan(&newer)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("check newer approvals of member %s: %w", userID, err)
	}
	return newer, true, nil
}

// SetShareState moves a request's share state from one of from to to, and
// reports whether it was in one, so two events or clicks can't both act.
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

// Removing lists the requests whose removal is in progress.
func (s *Store) Removing(ctx context.Context) ([]AccessRequest, error) {
	return s.listRequests(ctx, "access requests being removed", " WHERE share_state = ? ORDER BY id", ShareRemoving)
}

// SetSharePost records the "Remove Plex access?" card of a request.
func (s *Store) SetSharePost(ctx context.Context, id int, channelID, messageID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE access_request SET share_channel_id = ?, share_message_id = ? WHERE id = ?", channelID, messageID, id)
	if err != nil {
		return fmt.Errorf("set plex share post of access request %d: %w", id, err)
	}
	return nil
}

// HadAccount reports whether one of the member's requests before request
// before was approved for plexAccount.
func (s *Store) HadAccount(ctx context.Context, userID, plexAccount string, before int) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM access_request
		 WHERE user_id = ? AND plex_account = ? AND status = ? AND id < ?`,
		userID, plexAccount, StatusApproved, before).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check earlier plex access of member %s: %w", userID, err)
	}
	return n > 0, nil
}

// OlderShares lists the member's approved requests before request newer
// whose Plex access is still tracked, newest first.
func (s *Store) OlderShares(ctx context.Context, userID string, newer int) ([]AccessRequest, error) {
	return s.listRequests(ctx, "older plex shares",
		` WHERE user_id = ? AND id < ? AND status = ? AND plex_account != '' AND share_state NOT IN (?, ?) ORDER BY id DESC`,
		userID, newer, StatusApproved, ShareRemoved, ShareSuperseded)
}

// TrackedShares lists every approval whose Plex access is still tracked.
func (s *Store) TrackedShares(ctx context.Context) ([]AccessRequest, error) {
	return s.listRequests(ctx, "tracked plex shares",
		` WHERE status = ? AND plex_account != '' AND share_state IN (?, ?, ?) ORDER BY id`,
		StatusApproved, ShareActive, ShareAsked, ShareKept)
}

func (s *Store) listRequests(ctx context.Context, what, where string, args ...any) ([]AccessRequest, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+requestColumns+" FROM access_request"+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", what, err)
	}
	defer rows.Close()
	var out []AccessRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", what, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", what, err)
	}
	return out, nil
}
