package seerr

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// Notification types as Seerr's webhook agent names them.
const (
	NotifyTest          = "TEST_NOTIFICATION"
	NotifyIssueCreated  = "ISSUE_CREATED"
	NotifyIssueComment  = "ISSUE_COMMENT"
	NotifyIssueResolved = "ISSUE_RESOLVED"
	NotifyIssueReopened = "ISSUE_REOPENED"
)

// Payload is the body Seerr's webhook sends with the README's template.
// Every value is a string, empty when unset.
type Payload struct {
	Event            string `json:"event"`
	NotificationType string `json:"notification_type"`
	Subject          string `json:"subject"`
	Message          string `json:"message"`
	Image            string `json:"image"`
	IssueID          string `json:"issue_id"`
	IssueType        string `json:"issue_type"`
	IssueStatus      string `json:"issue_status"`
	ReportedBy       string `json:"reported_by"`
	CommentedBy      string `json:"commented_by"`
	Comment          string `json:"comment"`
	MediaType        string `json:"media_type"`
	TMDBID           string `json:"tmdb_id"`
	TVDBID           string `json:"tvdb_id"`
}

// Issue returns the payload's issue ID.
func (p Payload) Issue() (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(p.IssueID))
	if err != nil || id <= 0 {
		return 0, errors.New("payload has no valid issue_id")
	}
	return id, nil
}

// ErrBadPayload marks a payload the receiver can't act on, answered with
// 400 rather than 500.
var ErrBadPayload = errors.New("bad payload")

// maxBody caps the webhook body. Seerr's payloads are a few kilobytes.
const maxBody = 1 << 20

// Handler returns the POST /seerr handler: it checks the Authorization
// header against secret and passes the payload to receive. Seerr doesn't
// retry.
func Handler(secret string, receive func(context.Context, Payload) error, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			log.Warn("seerr webhook: wrong or missing Authorization header", "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var p Payload
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&p); err != nil {
			log.Warn("seerr webhook: undecodable body", "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		log.Debug("seerr webhook", "type", p.NotificationType, "issue", p.IssueID)
		if err := receive(r.Context(), p); err != nil {
			log.Error("seerr webhook", "type", p.NotificationType, "issue", p.IssueID, "err", err)
			if errors.Is(err, ErrBadPayload) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
