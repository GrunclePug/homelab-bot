// Package qbit reads the download queue and transfer state from
// qBittorrent's Web API.
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

// Client is a qBittorrent Web API client, authenticated with the Web UI's
// API key (qBittorrent 5.2 and later).
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
	var list []Torrent
	if err := c.get(ctx, "/api/v2/torrents/info", &list); err != nil {
		return nil, fmt.Errorf("qbittorrent: list torrents: %w", err)
	}
	return list, nil
}

// Transfer is qBittorrent's overall transfer state.
type Transfer struct {
	Version    string
	Connection string // connected, firewalled or disconnected
	DLSpeed    int64  // bytes per second
	UPSpeed    int64
}

// Transfer reads qBittorrent's version and overall transfer state.
func (c *Client) Transfer(ctx context.Context) (Transfer, error) {
	var (
		version []byte
		info    struct {
			Connection string `json:"connection_status"`
			DLSpeed    int64  `json:"dl_info_speed"`
			UPSpeed    int64  `json:"up_info_speed"`
		}
	)
	if err := c.get(ctx, "/api/v2/app/version", &version); err != nil {
		return Transfer{}, fmt.Errorf("qbittorrent: read version: %w", err)
	}
	if err := c.get(ctx, "/api/v2/transfer/info", &info); err != nil {
		return Transfer{}, fmt.Errorf("qbittorrent: read transfer info: %w", err)
	}
	return Transfer{
		Version:    string(bytes.TrimSpace(version)),
		Connection: info.Connection,
		DLSpeed:    info.DLSpeed,
		UPSpeed:    info.UPSpeed,
	}, nil
}

// get decodes the JSON answer to a GET into out, or stores the raw body
// if out is a *[]byte.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("API key rejected: check QBIT_API_KEY")
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if raw, ok := out.(*[]byte); ok {
		*raw, err = io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
