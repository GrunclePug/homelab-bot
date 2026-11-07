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

// Client is a qBittorrent Web API client. It authenticates with the Web
// UI's API key, which qBittorrent 5.2 and later accept as a Bearer token.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient returns a client for the qBittorrent Web UI at baseURL.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Torrents lists every torrent.
func (c *Client) Torrents(ctx context.Context) ([]Torrent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v2/torrents/info", nil)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: list torrents: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: list torrents: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, errors.New("qbittorrent: API key rejected: check QBIT_API_KEY")
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("qbittorrent: list torrents: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var list []Torrent
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("qbittorrent: decode torrents: %w", err)
	}
	return list, nil
}
