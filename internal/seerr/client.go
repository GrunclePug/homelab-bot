// Package seerr talks to Seerr's API and receives its webhook.
package seerr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Issue types and statuses as Seerr stores them.
const (
	IssueVideo     = 1
	IssueAudio     = 2
	IssueSubtitles = 3
	IssueOther     = 4

	StatusOpen     = 1
	StatusResolved = 2
)

// Media types.
const (
	MediaMovie = "movie"
	MediaTV    = "tv"
)

// Issue is the part of GET /api/v1/issue/{id} the bot uses.
type Issue struct {
	ID             int       `json:"id"`
	IssueType      int       `json:"issueType"`
	Status         int       `json:"status"`
	ProblemSeason  int       `json:"problemSeason"`
	ProblemEpisode int       `json:"problemEpisode"`
	Media          Media     `json:"media"`
	CreatedBy      User      `json:"createdBy"`
	Comments       []Comment `json:"comments"`
}

// Media is the media an issue is about.
type Media struct {
	MediaType string `json:"mediaType"`
	TMDBID    int    `json:"tmdbId"`
	TVDBID    int    `json:"tvdbId"`
}

// User is a Seerr user.
type User struct {
	DisplayName string `json:"displayName"`
}

// Comment is a comment on an issue.
type Comment struct {
	ID      int    `json:"id"`
	Message string `json:"message"`
	User    User   `json:"user"`
}

// Client is a Seerr API client.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient returns a client for the Seerr at baseURL.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Issue fetches an issue.
func (c *Client) Issue(ctx context.Context, id int) (Issue, error) {
	var is Issue
	if err := c.do(ctx, http.MethodGet, "/api/v1/issue/"+strconv.Itoa(id), nil, &is); err != nil {
		return Issue{}, fmt.Errorf("get seerr issue %d: %w", id, err)
	}
	return is, nil
}

// Comment adds a comment to an issue, posted as the API key's user.
func (c *Client) Comment(ctx context.Context, id int, message string) error {
	body := map[string]string{"message": message}
	if err := c.do(ctx, http.MethodPost, "/api/v1/issue/"+strconv.Itoa(id)+"/comment", body, nil); err != nil {
		return fmt.Errorf("comment on seerr issue %d: %w", id, err)
	}
	return nil
}

// Resolve marks an issue resolved.
func (c *Client) Resolve(ctx context.Context, id int) error {
	if err := c.do(ctx, http.MethodPost, "/api/v1/issue/"+strconv.Itoa(id)+"/resolved", nil, nil); err != nil {
		return fmt.Errorf("resolve seerr issue %d: %w", id, err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
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
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
