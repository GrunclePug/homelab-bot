// Package plex invites people to a Plex server through plex.tv.
//
// plex.tv's sharing API isn't documented. This follows what python-plexapi
// (MyPlexAccount.inviteFriend) does: list the server's library sections,
// then share all of them with the invited account.
package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultBaseURL is plex.tv.
const DefaultBaseURL = "https://plex.tv"

// clientID identifies the bot to plex.tv.
const clientID = "homelab-bot"

// Section is a library on the server, as plex.tv knows it.
type Section struct {
	ID    int    `xml:"id,attr"`
	Key   int    `xml:"key,attr"`
	Title string `xml:"title,attr"`
	Type  string `xml:"type,attr"`
}

// Client shares a Plex server with new users, using the owner's token.
type Client struct {
	baseURL  string
	token    string
	serverID string
	http     *http.Client
}

// NewClient returns a client for the server with the given machine
// identifier, acting as the owner of token. baseURL is normally
// DefaultBaseURL.
func NewClient(baseURL, token, serverID string) *Client {
	return &Client{
		baseURL:  baseURL,
		token:    token,
		serverID: serverID,
		http:     &http.Client{Timeout: 20 * time.Second},
	}
}

// Sections lists the server's libraries.
func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var mc struct {
		Servers []struct {
			MachineIdentifier string    `xml:"machineIdentifier,attr"`
			Sections          []Section `xml:"Section"`
		} `xml:"Server"`
	}
	body, err := c.do(ctx, http.MethodGet, "/api/servers/"+url.PathEscape(c.serverID), nil)
	if err != nil {
		return nil, fmt.Errorf("plex: list libraries: %w", err)
	}
	if err := xml.Unmarshal(body, &mc); err != nil {
		return nil, fmt.Errorf("plex: list libraries: decode: %w", err)
	}
	if len(mc.Servers) == 0 {
		return nil, fmt.Errorf("plex: server %s not found on this account", c.serverID)
	}
	return mc.Servers[0].Sections, nil
}

// Invite shares every library with a Plex account, given as its email
// address or username. plex.tv sends the invitation.
func (c *Client) Invite(ctx context.Context, who string) error {
	secs, err := c.Sections(ctx)
	if err != nil {
		return err
	}
	if len(secs) == 0 {
		return fmt.Errorf("plex: server %s has no libraries to share", c.serverID)
	}
	ids := make([]int, len(secs))
	for i, s := range secs {
		ids[i] = s.ID
	}
	req := map[string]any{
		"server_id": c.serverID,
		"shared_server": map[string]any{
			"library_section_ids": ids,
			"invited_email":       who,
		},
		"sharing_settings": map[string]string{
			"allowSync":         "0",
			"allowCameraUpload": "0",
			"allowChannels":     "0",
			"filterMovies":      "",
			"filterTelevision":  "",
			"filterMusic":       "",
		},
	}
	if _, err := c.do(ctx, http.MethodPost, "/api/servers/"+url.PathEscape(c.serverID)+"/shared_servers", req); err != nil {
		// The error leaves out who, which may be an email address.
		return fmt.Errorf("plex: invite: %w", err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, in any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("X-Plex-Client-Identifier", clientID)
	req.Header.Set("X-Plex-Product", clientID)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: %s", method, path, errorText(resp.Status, b))
	}
	return b, nil
}

// errorText pulls the message out of a plex.tv error body, which is
// usually XML such as <errors><error code="1001" message="..."/></errors>.
func errorText(status string, body []byte) string {
	var e struct {
		Errors []struct {
			Code    int    `xml:"code,attr"`
			Message string `xml:"message,attr"`
		} `xml:"error"`
	}
	if xml.Unmarshal(body, &e) == nil && len(e.Errors) > 0 && e.Errors[0].Message != "" {
		return status + ": " + e.Errors[0].Message + " (code " + strconv.Itoa(e.Errors[0].Code) + ")"
	}
	if len(body) > 300 {
		body = body[:300]
	}
	return status + ": " + string(bytes.TrimSpace(body))
}
