// Package kuma reads an Uptime Kuma status page through the public JSON the
// page itself polls, so it needs no credentials.
package kuma

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Monitor statuses, as Kuma numbers them.
const (
	Down        = 0
	Up          = 1
	Pending     = 2
	Maintenance = 3
)

// Monitor is one monitor on the status page.
type Monitor struct {
	Name   string
	Status int     // the latest heartbeat's status
	Seen   bool    // false if it has no heartbeats yet
	Uptime float64 // over the last 24 hours, from 0 to 1
	// UptimeKnown is false if Kuma gave no 24-hour uptime for it.
	UptimeKnown bool
}

// Group is a section of the status page.
type Group struct {
	Name     string
	Monitors []Monitor
}

// Client reads one status page.
type Client struct {
	baseURL string
	slug    string
	http    *http.Client
}

// NewClient returns a client for the status page at pageURL, such as
// https://kuma.example.com/status/homelab.
func NewClient(pageURL string) (*Client, error) {
	base, slug, err := ParsePageURL(pageURL)
	if err != nil {
		return nil, err
	}
	return &Client{baseURL: base, slug: slug, http: &http.Client{Timeout: 10 * time.Second}}, nil
}

// ParsePageURL splits a status page's address into Kuma's base URL and
// the page's slug.
func ParsePageURL(pageURL string) (base, slug string, err error) {
	u, err := url.Parse(pageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("kuma: %q is not an http or https URL", pageURL)
	}
	prefix, slug, ok := strings.Cut(strings.TrimRight(u.Path, "/"), "/status/")
	if !ok || slug == "" || strings.Contains(slug, "/") {
		return "", "", fmt.Errorf("kuma: %q is not a status page address, which ends in /status/<slug>", pageURL)
	}
	return u.Scheme + "://" + u.Host + prefix, slug, nil
}

// Groups reads the status page's groups and the state of their monitors.
func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	var page struct {
		Groups []struct {
			Name     string `json:"name"`
			Monitors []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"monitorList"`
		} `json:"publicGroupList"`
	}
	var beats struct {
		Heartbeats map[string][]struct {
			Status int `json:"status"`
		} `json:"heartbeatList"`
		Uptime map[string]float64 `json:"uptimeList"`
	}
	slug := url.PathEscape(c.slug)
	if err := c.get(ctx, "/api/status-page/"+slug, &page); err != nil {
		return nil, err
	}
	if err := c.get(ctx, "/api/status-page/heartbeat/"+slug, &beats); err != nil {
		return nil, err
	}
	var out []Group
	for _, g := range page.Groups {
		grp := Group{Name: g.Name}
		for _, m := range g.Monitors {
			id := strconv.Itoa(m.ID)
			mon := Monitor{Name: m.Name}
			mon.Uptime, mon.UptimeKnown = beats.Uptime[id+"_24"]
			// Oldest first, so the last one is the latest.
			if hb := beats.Heartbeats[id]; len(hb) > 0 {
				mon.Status, mon.Seen = hb[len(hb)-1].Status, true
			}
			grp.Monitors = append(grp.Monitors, mon)
		}
		out = append(out, grp)
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("kuma: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kuma: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("kuma: GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("kuma: GET %s: decode: %w", path, err)
	}
	return nil
}
