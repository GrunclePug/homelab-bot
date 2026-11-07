// Package qbit reads the download queue from qBittorrent's Web API.
package qbit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Torrent is the part of /api/v2/torrents/info the queue uses.
type Torrent struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Size     int64   `json:"size"`
	DLSpeed  int64   `json:"dlspeed"`
	ETA      int64   `json:"eta"`
	Priority int     `json:"priority"`
}

// Client is a qBittorrent Web API client. It logs in on first use and
// again whenever the session has expired.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// NewClient returns a client for the qBittorrent Web UI at baseURL.
func NewClient(baseURL, username, password string) *Client {
	jar, _ := cookiejar.New(nil) // cookiejar.New never returns an error
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		http:     &http.Client{Timeout: 15 * time.Second, Jar: jar},
	}
}

// errForbidden means the session cookie is missing or expired.
var errForbidden = errors.New("forbidden")

// Torrents lists every torrent.
func (c *Client) Torrents(ctx context.Context) ([]Torrent, error) {
	list, err := c.torrents(ctx)
	if errors.Is(err, errForbidden) {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		list, err = c.torrents(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: list torrents: %w", err)
	}
	return list, nil
}

func (c *Client) torrents(ctx context.Context) ([]Torrent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/torrents/info", nil)
	if err != nil {
		return nil, err
	}
	c.setReferer(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, errForbidden
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var list []Torrent
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decode torrents: %w", err)
	}
	return list, nil
}

func (c *Client) login(ctx context.Context) error {
	form := url.Values{"username": {c.username}, "password": {c.password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/auth/login",
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("qbittorrent: login: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.setReferer(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("qbittorrent: login: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return errors.New("qbittorrent: login refused: this IP is banned after too many failed logins")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("qbittorrent: login: %s", resp.Status)
	case strings.TrimSpace(string(body)) != "Ok.":
		return errors.New("qbittorrent: login failed: check QBIT_USERNAME and QBIT_PASSWORD")
	}
	return nil
}

// setReferer satisfies qBittorrent's CSRF check, which wants Referer or
// Origin to match the host being called.
func (c *Client) setReferer(req *http.Request) {
	req.Header.Set("Referer", c.baseURL)
}
