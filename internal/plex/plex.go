// Package plex invites people to a Plex server through plex.tv.
//
// plex.tv's sharing API isn't documented. This follows what python-plexapi
// does: inviteFriend lists the server's library sections and shares all of
// them with the invited account; updateFriend with removeSections deletes
// an accepted share by its ID, and cancelInvite cancels a pending invite.
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
	srv, err := c.server(ctx)
	return srv.Sections, err
}

// serverInfo is the server as plex.tv knows it.
type serverInfo struct {
	MachineIdentifier string    `xml:"machineIdentifier,attr"`
	Name              string    `xml:"name,attr"`
	Sections          []Section `xml:"Section"`
}

func (c *Client) server(ctx context.Context) (serverInfo, error) {
	var mc struct {
		Servers []serverInfo `xml:"Server"`
	}
	body, err := c.do(ctx, http.MethodGet, "/api/servers/"+url.PathEscape(c.serverID), nil)
	if err != nil {
		return serverInfo{}, fmt.Errorf("plex: get server: %w", err)
	}
	if err := xml.Unmarshal(body, &mc); err != nil {
		return serverInfo{}, fmt.Errorf("plex: get server: decode: %w", err)
	}
	if len(mc.Servers) == 0 {
		return serverInfo{}, fmt.Errorf("plex: server %s not found on this account", c.serverID)
	}
	return mc.Servers[0], nil
}

// ErrAccountUnknown means the account was invited, or already had access,
// but couldn't be found among the shares and invites, so its access can
// only be removed by hand.
var ErrAccountUnknown = errors.New("plex: the invited account couldn't be found")

// Account identifies who a share or invite is for, so RemoveAccess can
// find it later: "id:" and the Plex account ID, or, for an invite to an
// email address with no Plex account yet, "email:" and the SHA-256 of the
// lowercased address. It never holds the address itself.
type Account string

func accountID(id int) Account { return Account("id:" + strconv.Itoa(id)) }

func accountEmail(email string) Account { return Account("email:" + hashEmail(email)) }

func hashEmail(s string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:])
}

// matches reports whether the account is the one with this Plex ID or
// email address; either may be empty.
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

// ErrAlreadyShared means the server was already shared with the account,
// as an accepted share or a pending invite, so no invite was sent.
var ErrAlreadyShared = errors.New("plex: the server is already shared with this account")

// sharedServer is an accepted share of the server, as
// /api/servers/{machineId}/shared_servers lists it.
type sharedServer struct {
	ID       int    `xml:"id,attr"`
	UserID   int    `xml:"userID,attr"`
	Email    string `xml:"email,attr"`
	Username string `xml:"username,attr"`
}

// invite is an invite the owner sent that hasn't been accepted, as
// /api/invites/requested lists it. Its ID is the invitee's Plex account ID,
// or, for an address with no Plex account, the address itself.
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

// Invite shares every library with a Plex account, given as its email
// address or username, and returns the Account that RemoveAccess takes.
// plex.tv sends the invitation.
//
// If the account already has access, accepted or still pending, no invite
// is sent and the error wraps ErrAlreadyShared. If the account can't be
// found, the Account is empty and the error wraps ErrAccountUnknown.
// Either way the account has access.
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
	var se *statusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusBadRequest && strings.Contains(strings.ToLower(se.text), "already sharing"):
		already = ErrAlreadyShared
	case errors.As(err, &se):
		// The path names the server; plex.tv's own words are enough.
		// who may be an email address, and plex.tv's message might repeat
		// it, so it's taken out of the error, which ends up in Discord.
		return "", fmt.Errorf("plex: invite: %s", redact(se.status+": "+se.text, who))
	case err != nil:
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

// findAccount looks up who, by email address or username, among the
// server's accepted shares and then the pending invites to the server
// named serverName. It returns "" if they are in neither.
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

// invites lists the pending invites that include the server named
// serverName. plex.tv names an invite's servers but gives no IDs.
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
		// plex.tv writes flags as 1 or 0, but accept true and false too.
		if inv.Server != "0" && inv.Server != "false" && slices.Contains(inv.Servers, inviteServer{Name: serverName}) {
			out = append(out, inv)
		}
	}
	return out, nil
}

// RemoveAccess stops sharing the server with acct: it deletes their share
// if they accepted the invite, or cancels the invite if they haven't.
// removed is false if they had neither.
func (c *Client) RemoveAccess(ctx context.Context, acct Account) (removed bool, err error) {
	if acct == "" {
		return false, errors.New("plex: remove access: no account")
	}
	shares, err := c.shares(ctx)
	if err != nil {
		return false, fmt.Errorf("plex: remove access: %s", brief(err))
	}
	for _, s := range shares {
		// Someone invited by address and since signed up matches by it.
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
			// As python-plexapi's cancelInvite: the invite's own flags.
			q := url.Values{"friend": {orZero(inv.Friend)}, "home": {orZero(inv.Home)}, "server": {orZero(inv.Server)}}
			// The ID may be the invitee's email address, so it stays out
			// of any error, as do the address and username themselves.
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

// remove sends a DELETE; something already gone counts as not removed.
// Its errors name neither the path nor any of secrets: the account's
// address, username or ID, which plex.tv's message might repeat.
func (c *Client) remove(ctx context.Context, path string, secrets ...string) (bool, error) {
	_, err := c.do(ctx, http.MethodDelete, path, nil)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.code == http.StatusNotFound:
		return false, nil
	case errors.As(err, &se):
		text := se.text
		for _, s := range secrets {
			text = redact(text, s)
		}
		return false, fmt.Errorf("plex: remove access: plex.tv answered %s: %s", se.status, text)
	case err != nil:
		return false, fmt.Errorf("plex: remove access: %s", brief(err))
	}
	return true, nil
}

// brief describes a failed plex.tv request without its URL, which names
// the server and, for some invites, the invitee's address.
func brief(err error) string {
	var se *statusError
	switch {
	case errors.As(err, &se):
		return "plex.tv answered " + se.status
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "plex.tv timed out"
	default:
		return "plex.tv unreachable"
	}
}

func isTimeout(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// ErrServerNotFound means plex.tv doesn't list the server on the owner's
// account.
var ErrServerNotFound = errors.New("plex: server not found on this account")

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
