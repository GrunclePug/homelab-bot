// Package plex shares a Plex server through plex.tv.
//
// plex.tv's sharing API isn't documented; this follows python-plexapi's
// inviteFriend, updateFriend and cancelInvite.
package plex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is plex.tv.
const DefaultBaseURL = "https://plex.tv"

const clientID = "homelab-bot"

var (
	// ErrAlreadyShared means the account already had access, accepted or
	// pending, so no invite was sent.
	ErrAlreadyShared = errors.New("plex: the server is already shared with this account")
	// ErrAccountUnknown means the account has access but couldn't be
	// found among the shares and invites.
	ErrAccountUnknown = errors.New("plex: the invited account couldn't be found")
	// ErrServerNotFound means plex.tv doesn't list the server on the
	// owner's account.
	ErrServerNotFound = errors.New("plex: server not found on this account")
)

// Client acts on one Plex server as its owner.
type Client struct {
	baseURL  string
	token    string
	serverID string
	http     *http.Client
}

// NewClient returns a client for the server with machine identifier
// serverID, acting as the owner of token.
func NewClient(baseURL, token, serverID string) *Client {
	return &Client{
		baseURL:  baseURL,
		token:    token,
		serverID: serverID,
		http:     &http.Client{Timeout: 20 * time.Second},
	}
}

// Account identifies whose access a share or invite is: "id:" and a Plex
// account ID, or for an address with no Plex account, "email:" and the
// SHA-256 of the lowercased address. It never holds the address.
type Account string

func accountID(id int) Account { return Account("id:" + strconv.Itoa(id)) }

func accountEmail(email string) Account { return Account("email:" + hashEmail(email)) }

func hashEmail(s string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:])
}

func (a Account) matches(id, email string) bool {
	kind, v, _ := strings.Cut(string(a), ":")
	switch kind {
	case "id":
		return id != "" && id == v
	case "email":
		return email != "" && hashEmail(email) == v
	}
	return false
}

type serverInfo struct {
	Name     string `xml:"name,attr"`
	Sections []struct {
		ID int `xml:"id,attr"`
	} `xml:"Section"`
}

// sharedServer is an accepted share.
type sharedServer struct {
	ID       int    `xml:"id,attr"`
	UserID   int    `xml:"userID,attr"`
	Email    string `xml:"email,attr"`
	Username string `xml:"username,attr"`
}

// invite is a pending invite. Its ID is the invitee's Plex account ID, or
// for an address with no Plex account, the address itself.
type invite struct {
	ID       string         `xml:"id,attr"`
	Email    string         `xml:"email,attr"`
	Username string         `xml:"username,attr"`
	Friend   string         `xml:"friend,attr"`
	Home     string         `xml:"home,attr"`
	Server   string         `xml:"server,attr"`
	Servers  []inviteServer `xml:"Server"`
}

type inviteServer struct {
	Name string `xml:"name,attr"`
}

func (c *Client) server(ctx context.Context) (serverInfo, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/servers/"+url.PathEscape(c.serverID), nil)
	if err != nil {
		return serverInfo{}, fmt.Errorf("plex: get server: %w", err)
	}
	var mc struct {
		Servers []serverInfo `xml:"Server"`
	}
	if err := xml.Unmarshal(body, &mc); err != nil {
		return serverInfo{}, fmt.Errorf("plex: get server: decode: %w", err)
	}
	if len(mc.Servers) == 0 {
		return serverInfo{}, ErrServerNotFound
	}
	return mc.Servers[0], nil
}

// Invite shares every library with an account, given by email address or
// username, and returns the Account for RemoveAccess.
//
// The error wraps ErrAlreadyShared if the account already had access, and
// ErrAccountUnknown, with an empty Account, if it couldn't be found
// afterwards. Either way the account has access.
func (c *Client) Invite(ctx context.Context, who string) (Account, error) {
	srv, err := c.server(ctx)
	if err != nil {
		return "", err
	}
	if len(srv.Sections) == 0 {
		return "", fmt.Errorf("plex: server %s has no libraries to share", c.serverID)
	}
	ids := make([]int, len(srv.Sections))
	for i, s := range srv.Sections {
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
	var already error
	_, err = c.do(ctx, http.MethodPost, c.sharesPath(), req)
	if se, ok := errors.AsType[*statusError](err); ok {
		if se.code != http.StatusBadRequest || !strings.Contains(strings.ToLower(se.text), "already sharing") {
			// who may be an address that plex.tv's message repeats, and the
			// error reaches Discord.
			return "", fmt.Errorf("plex: invite: %s", redact(se.status+": "+se.text, who))
		}
		already = ErrAlreadyShared
	} else if err != nil {
		return "", fmt.Errorf("plex: invite: %s", redact(err.Error(), who))
	}
	acct, err := c.findAccount(ctx, who, srv.Name)
	if err != nil {
		return "", errors.Join(already, fmt.Errorf("%w: %s", ErrAccountUnknown, redact(err.Error(), who)))
	}
	if acct == "" {
		return "", errors.Join(already, ErrAccountUnknown)
	}
	return acct, already
}

func (c *Client) sharesPath() string {
	return "/api/servers/" + url.PathEscape(c.serverID) + "/shared_servers"
}

// findAccount looks who up by email address or username among the
// accepted shares, then the pending invites. It returns "" if not found.
func (c *Client) findAccount(ctx context.Context, who, serverName string) (Account, error) {
	shares, err := c.shares(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range shares {
		if s.UserID > 0 && (strings.EqualFold(s.Email, who) || strings.EqualFold(s.Username, who)) {
			return accountID(s.UserID), nil
		}
	}
	invites, err := c.invites(ctx, serverName)
	if err != nil {
		return "", err
	}
	for _, inv := range invites {
		if inv.ID == "" || !(strings.EqualFold(inv.Email, who) || strings.EqualFold(inv.Username, who) || strings.EqualFold(inv.ID, who)) {
			continue
		}
		if n, err := strconv.Atoi(inv.ID); err == nil && n > 0 {
			return accountID(n), nil
		}
		return accountEmail(inv.ID), nil
	}
	return "", nil
}

func (c *Client) shares(ctx context.Context) ([]sharedServer, error) {
	body, err := c.do(ctx, http.MethodGet, c.sharesPath(), nil)
	if err != nil {
		return nil, err
	}
	var mc struct {
		Shares []sharedServer `xml:"SharedServer"`
	}
	if err := xml.Unmarshal(body, &mc); err != nil {
		return nil, fmt.Errorf("decode shares: %w", err)
	}
	return mc.Shares, nil
}

// invites lists the pending invites to the server, which plex.tv names
// but gives no ID for.
func (c *Client) invites(ctx context.Context, serverName string) ([]invite, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/invites/requested", nil)
	if err != nil {
		return nil, err
	}
	var mc struct {
		Invites []invite `xml:"Invite"`
	}
	if err := xml.Unmarshal(body, &mc); err != nil {
		return nil, fmt.Errorf("decode invites: %w", err)
	}
	var out []invite
	for _, inv := range mc.Invites {
		if inv.Server != "0" && inv.Server != "false" && slices.Contains(inv.Servers, inviteServer{Name: serverName}) {
			out = append(out, inv)
		}
	}
	return out, nil
}

// RemoveAccess deletes acct's share, or cancels their invite if they
// haven't accepted it. removed is false if they had neither.
func (c *Client) RemoveAccess(ctx context.Context, acct Account) (removed bool, err error) {
	if acct == "" {
		return false, errors.New("plex: remove access: no account")
	}
	shares, err := c.shares(ctx)
	if err != nil {
		return false, fmt.Errorf("plex: remove access: %s", brief(err))
	}
	for _, s := range shares {
		// Someone invited by address who has since signed up matches by it.
		if acct.matches(strconv.Itoa(s.UserID), s.Email) {
			return c.remove(ctx, c.sharesPath()+"/"+strconv.Itoa(s.ID), s.Email, s.Username)
		}
	}
	srv, err := c.server(ctx)
	if err != nil {
		return false, fmt.Errorf("plex: remove access: %s", brief(err))
	}
	invites, err := c.invites(ctx, srv.Name)
	if err != nil {
		return false, fmt.Errorf("plex: remove access: %s", brief(err))
	}
	for _, inv := range invites {
		if acct.matches(inv.ID, inv.Email) || acct.matches("", inv.ID) {
			q := url.Values{"friend": {orZero(inv.Friend)}, "home": {orZero(inv.Home)}, "server": {orZero(inv.Server)}}
			return c.remove(ctx, "/api/invites/requested/"+url.PathEscape(inv.ID)+"?"+q.Encode(), inv.ID, inv.Email, inv.Username)
		}
	}
	return false, nil
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// remove sends a DELETE; a 404 counts as not removed. Its errors leave out
// the path and secrets, which may name the account.
func (c *Client) remove(ctx context.Context, path string, secrets ...string) (bool, error) {
	_, err := c.do(ctx, http.MethodDelete, path, nil)
	if se, ok := errors.AsType[*statusError](err); ok {
		if se.code == http.StatusNotFound {
			return false, nil
		}
		text := se.text
		for _, s := range secrets {
			text = redact(text, s)
		}
		return false, fmt.Errorf("plex: remove access: plex.tv answered %s: %s", se.status, text)
	}
	if err != nil {
		return false, fmt.Errorf("plex: remove access: %s", brief(err))
	}
	return true, nil
}

// brief describes a failed request without its URL.
func brief(err error) string {
	if se, ok := errors.AsType[*statusError](err); ok {
		return "plex.tv answered " + se.status
	}
	if te, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](err); errors.Is(err, context.DeadlineExceeded) || (ok && te.Timeout()) {
		return "plex.tv timed out"
	}
	return "plex.tv unreachable"
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
	return Server{}, ErrServerNotFound
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
	// The v2 API answers in XML unless asked for JSON.
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

type statusError struct {
	method, path string
	code         int
	status       string // such as "400 Bad Request"
	text         string // plex.tv's message
}

func (e *statusError) Error() string {
	return e.method + " " + e.path + ": " + e.status + ": " + e.text
}

// errorText reads plex.tv's message from either of its error shapes:
// <errors><error code message/></errors> or <Response code status/>.
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
