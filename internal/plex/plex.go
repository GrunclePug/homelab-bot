// Package plex invites people to a Plex server through plex.tv.
//
// plex.tv's sharing API isn't documented. This follows what python-plexapi
// does: inviteFriend lists the server's library sections and shares all of
// them with the invited account; updateFriend with removeSections deletes
// the server's share by its ID.
package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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

// ErrShareUnknown means an invite went out but its share ID couldn't be
// found, so the share can only be removed by hand.
var ErrShareUnknown = errors.New("plex: invite sent, but its share ID is unknown")

// ErrAlreadyShared means the server was already shared with the account,
// so no invite was sent. Invite then returns the existing share's ID.
var ErrAlreadyShared = errors.New("plex: the server is already shared with this account")

// sharedServer is one account's share of the server, as plex.tv lists it.
type sharedServer struct {
	ID       int    `xml:"id,attr"`
	Email    string `xml:"email,attr"`
	Username string `xml:"username,attr"`
}

// Invite shares every library with a Plex account, given as its email
// address or username, and returns the ID of the share it created.
// plex.tv sends the invitation. If the invite went out but the share's ID
// can't be found, it returns an error wrapping ErrShareUnknown. If the
// account already has a share, it returns that share's ID and an error
// wrapping ErrAlreadyShared, and also ErrShareUnknown if it can't be found.
func (c *Client) Invite(ctx context.Context, who string) (int, error) {
	secs, err := c.Sections(ctx)
	if err != nil {
		return 0, err
	}
	if len(secs) == 0 {
		return 0, fmt.Errorf("plex: server %s has no libraries to share", c.serverID)
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
	body, err := c.do(ctx, http.MethodPost, c.sharesPath(), req)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusBadRequest && strings.Contains(strings.ToLower(se.text), "already sharing") {
		id, ferr := c.findShare(ctx, who)
		if ferr != nil || id == 0 {
			return 0, errors.Join(ErrAlreadyShared, ErrShareUnknown)
		}
		return id, ErrAlreadyShared
	}
	if errors.As(err, &se) {
		// The path names the server; plex.tv's own words are enough.
		err = errors.New(se.status + ": " + se.text)
	}
	if err != nil {
		// who may be an email address, and plex.tv's message might repeat
		// it, so it's taken out of the error, which ends up in Discord.
		return 0, fmt.Errorf("plex: invite: %s", redact(err.Error(), who))
	}
	if id := createdShare(body); id > 0 {
		return id, nil
	}
	id, err := c.findShare(ctx, who)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrShareUnknown, redact(err.Error(), who))
	}
	if id == 0 {
		return 0, ErrShareUnknown
	}
	return id, nil
}

func (c *Client) sharesPath() string {
	return "/api/servers/" + url.PathEscape(c.serverID) + "/shared_servers"
}

// createdShare reads the share ID from plex.tv's answer to an invite,
// which is a SharedServer element, on its own or in a MediaContainer.
func createdShare(body []byte) int {
	var one struct {
		XMLName xml.Name
		ID      int            `xml:"id,attr"`
		Shares  []sharedServer `xml:"SharedServer"`
	}
	if xml.Unmarshal(body, &one) != nil {
		return 0
	}
	if one.XMLName.Local == "SharedServer" {
		return one.ID
	}
	if len(one.Shares) == 1 {
		return one.Shares[0].ID
	}
	return 0
}

// findShare looks up the share of the account who, by email address or
// username. It returns 0 if there is none.
func (c *Client) findShare(ctx context.Context, who string) (int, error) {
	body, err := c.do(ctx, http.MethodGet, c.sharesPath(), nil)
	if err != nil {
		return 0, err
	}
	var mc struct {
		Shares []sharedServer `xml:"SharedServer"`
	}
	if err := xml.Unmarshal(body, &mc); err != nil {
		return 0, fmt.Errorf("decode shares: %w", err)
	}
	for _, s := range mc.Shares {
		if strings.EqualFold(s.Email, who) || strings.EqualFold(s.Username, who) {
			return s.ID, nil
		}
	}
	return 0, nil
}

// RemoveShare stops sharing the server through the share with this ID,
// whether or not its invite was accepted. A share that is already gone
// is not an error.
func (c *Client) RemoveShare(ctx context.Context, id int) error {
	if id <= 0 {
		return errors.New("plex: remove share: no share ID")
	}
	_, err := c.do(ctx, http.MethodDelete, c.sharesPath()+"/"+strconv.Itoa(id), nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("plex: remove share %d: %w", id, err)
	}
	return nil
}

// Server is how plex.tv last saw the server.
type Server struct {
	Online  bool
	Version string
}

// Server reports whether plex.tv sees the server online, and its version.
func (c *Client) Server(ctx context.Context) (Server, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/v2/resources", nil)
	if err != nil {
		return Server{}, fmt.Errorf("plex: list resources: %w", err)
	}
	var rs []struct {
		ClientIdentifier string `json:"clientIdentifier"`
		Presence         bool   `json:"presence"`
		ProductVersion   string `json:"productVersion"`
	}
	if err := json.Unmarshal(body, &rs); err != nil {
		return Server{}, fmt.Errorf("plex: list resources: decode: %w", err)
	}
	for _, r := range rs {
		if r.ClientIdentifier == c.serverID {
			return Server{Online: r.Presence, Version: r.ProductVersion}, nil
		}
	}
	return Server{}, fmt.Errorf("plex: server %s not found on this account", c.serverID)
}

// redact replaces every case-insensitive occurrence of who in s.
func redact(s, who string) string {
	if who == "" {
		return s
	}
	return regexp.MustCompile("(?i)"+regexp.QuoteMeta(who)).ReplaceAllLiteralString(s, "the invited account")
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
	// The v2 API answers in XML unless asked for JSON; the older one
	// ignores this and always answers in XML.
	if strings.HasPrefix(path, "/api/v2/") {
		req.Header.Set("Accept", "application/json")
	}
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
		return nil, &statusError{method: method, path: path, code: resp.StatusCode, status: resp.Status, text: errorText(b)}
	}
	return b, nil
}

// statusError is an answer from plex.tv outside 2xx.
type statusError struct {
	method, path string
	code         int
	status       string // such as "400 Bad Request"
	text         string // plex.tv's message
}

func (e *statusError) Error() string {
	return e.method + " " + e.path + ": " + e.status + ": " + e.text
}

// errorText pulls the message out of a plex.tv error body. plex.tv answers
// in two shapes: <errors><error code="1001" message="..."/></errors>, and
// <Response code="400" status="..."/>.
func errorText(body []byte) string {
	var e struct {
		XMLName xml.Name
		Status  string `xml:"status,attr"`
		Errors  []struct {
			Code    int    `xml:"code,attr"`
			Message string `xml:"message,attr"`
		} `xml:"error"`
	}
	if xml.Unmarshal(body, &e) == nil {
		switch {
		case len(e.Errors) > 0 && e.Errors[0].Message != "":
			return e.Errors[0].Message + " (code " + strconv.Itoa(e.Errors[0].Code) + ")"
		case e.XMLName.Local == "Response" && e.Status != "":
			return e.Status
		}
	}
	if len(body) > 300 {
		body = body[:300]
	}
	return string(bytes.TrimSpace(body))
}
