// Package arr holds small clients for the Sonarr and Radarr v3 APIs.
package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// History event types as the v3 API names them. Their numbers differ
// between Sonarr and Radarr, so the clients match on names.
const (
	EventGrabbed  = "grabbed"
	EventImported = "downloadFolderImported"
)

// Quality is a file's quality.
type Quality struct {
	Quality struct {
		Name string `json:"name"`
	} `json:"quality"`
}

// HistoryRecord is one history entry for an episode or movie.
type HistoryRecord struct {
	ID          int       `json:"id"`
	EventType   string    `json:"eventType"`
	Date        time.Time `json:"date"`
	DownloadID  string    `json:"downloadId"`
	SourceTitle string    `json:"sourceTitle"`
}

type historyPage struct {
	Records []HistoryRecord `json:"records"`
}

// historyPageSize is enough to cover one episode's or movie's history.
const historyPageSize = 100

// ReleaseGrab picks the grab behind the current file from history sorted
// newest first: the one matching the newest import's download ID, or else
// the newest grab. ok is false if there is none, as after a manual import.
func ReleaseGrab(records []HistoryRecord) (HistoryRecord, bool) {
	for _, r := range records {
		if r.EventType != EventImported || r.DownloadID == "" {
			continue
		}
		for _, g := range records {
			if g.EventType == EventGrabbed && g.DownloadID == r.DownloadID {
				return g, true
			}
		}
		break
	}
	for _, g := range records {
		if g.EventType == EventGrabbed {
			return g, true
		}
	}
	return HistoryRecord{}, false
}

// StatusError is an answer from Sonarr or Radarr outside 2xx.
type StatusError struct {
	App    string
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Code, http.StatusText(e.Code), e.Body)
}

// UnreachableError is a request that got no answer. Its message holds the
// app's URL, so it belongs in logs, not Discord.
type UnreachableError struct {
	App string
	Err error
}

func (e *UnreachableError) Error() string { return e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// IsNotFound reports whether err is a 404 from Sonarr or Radarr.
func IsNotFound(err error) bool {
	se, ok := errors.AsType[*StatusError](err)
	return ok && se.Code == http.StatusNotFound
}

// Brief describes err in a few words, without URLs, hosts or bodies.
func Brief(err error) string {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return fmt.Sprintf("%s answered %d %s", appName(se.App), se.Code, http.StatusText(se.Code))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if ue, ok := errors.AsType[*UnreachableError](err); ok {
		return appName(ue.App) + " unreachable"
	}
	return "failed"
}

func appName(app string) string {
	switch app {
	case "sonarr":
		return "Sonarr"
	case "radarr":
		return "Radarr"
	}
	return app
}

// client is the HTTP plumbing Sonarr and Radarr share.
type client struct {
	name    string
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(name, baseURL, apiKey string) client {
	return client{
		name:    name,
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// markFailed marks a grab failed, which blocklists its release.
func (c client) markFailed(ctx context.Context, historyID int) error {
	if err := c.do(ctx, http.MethodPost, "/api/v3/history/failed/"+strconv.Itoa(historyID), nil, nil); err != nil {
		return fmt.Errorf("%s: mark history %d failed: %w", c.name, historyID, err)
	}
	return nil
}

// command starts a command such as a search.
func (c client) command(ctx context.Context, body any) error {
	if err := c.do(ctx, http.MethodPost, "/api/v3/command", body, nil); err != nil {
		return fmt.Errorf("%s: start command: %w", c.name, err)
	}
	return nil
}

func (c client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &UnreachableError{App: c.name, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{App: c.name, Method: method, Path: path, Code: resp.StatusCode, Body: string(bytes.TrimSpace(msg))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
