package plex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const serverXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer friendlyName="myPlex" size="1">
  <Server name="media" machineIdentifier="abc123">
    <Section id="901" key="1" type="movie" title="Movies"/>
    <Section id="902" key="2" type="show" title="TV"/>
  </Server>
</MediaContainer>`

func TestInvite(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" || r.Header.Get("X-Plex-Client-Identifier") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `<errors><error code="1001" message="User could not be authenticated" status="401"/></errors>`)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/servers/abc123":
			io.WriteString(w, serverXML)
		case "POST /api/servers/abc123/shared_servers":
			json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `<SharedServer id="1"/>`)
		case "GET /api/servers/abc123/shared_servers":
			io.WriteString(w, `<MediaContainer size="0"/>`)
		case "GET /api/invites/requested":
			io.WriteString(w, `<MediaContainer size="1"><Invite id="5001" email="friend@example.com" username="" friend="0" home="0" server="1"><Server name="media" numLibraries="2"/></Invite></MediaContainer>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	c := NewClient(srv.URL, "tok", "abc123")
	secs, err := c.Sections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 2 || secs[0].ID != 901 || secs[1].Title != "TV" {
		t.Errorf("sections = %+v", secs)
	}

	id, err := c.Invite(ctx, "friend@example.com")
	if err != nil || id != "id:5001" {
		t.Fatalf("Invite = %q, %v; want account 5001", id, err)
	}
	shared := got["shared_server"].(map[string]any)
	var ids []float64
	for _, v := range shared["library_section_ids"].([]any) {
		ids = append(ids, v.(float64))
	}
	if got["server_id"] != "abc123" || shared["invited_email"] != "friend@example.com" || !slices.Equal(ids, []float64{901, 902}) {
		t.Errorf("invite body = %v", got)
	}

	bad := NewClient(srv.URL, "wrong", "abc123")
	_, err = bad.Invite(ctx, "friend@example.com")
	if err == nil || !strings.Contains(err.Error(), "User could not be authenticated (code 1001)") {
		t.Errorf("bad token: err = %v", err)
	}

	missing := NewClient(srv.URL, "tok", "nope")
	if _, err := missing.Invite(ctx, "x"); err == nil {
		t.Error("unknown server: want error")
	}
}

func TestInviteRedactsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, serverXML)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `<errors><error code="1000" message="No user Friend@Example.com, or friend@example.com"/></errors>`)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok", "abc123").Invite(context.Background(), "friend@example.com")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(strings.ToLower(err.Error()), "friend@example.com") {
		t.Errorf("error names the account: %v", err)
	}
	if !strings.Contains(err.Error(), "No user the invited account, or the invited account") {
		t.Errorf("error = %v", err)
	}
}

func TestServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/resources" || r.Header.Get("Accept") != "application/json" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `[{"name":"phone","provides":"client","clientIdentifier":"x","presence":false},
			{"name":"media","provides":"server","clientIdentifier":"abc123","presence":true,"productVersion":"1.43.5"}]`)
	}))
	defer srv.Close()
	s, err := NewClient(srv.URL, "tok", "abc123").Server(context.Background())
	if err != nil || !s.Online || s.Version != "1.43.5" {
		t.Errorf("Server = %+v, %v", s, err)
	}
	if _, err := NewClient(srv.URL, "tok", "other").Server(context.Background()); err == nil {
		t.Error("unknown server: want error")
	}
}

// Other plex.tv errors in the <Response> shape read as plex.tv's words,
// without the request path, which names the server.
func TestInviteResponseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, serverXML)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Response code="422" status="Invalid email address"/>`)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok", "abc123").Invite(context.Background(), "friend@example.com")
	if err == nil || err.Error() != "plex: invite: 422 Unprocessable Entity: Invalid email address" {
		t.Errorf("err = %v", err)
	}
}

// plexTV is plex.tv with one server named "media", its accepted shares and
// its pending invites, as plex.tv answered on 2026-11-07. Each request is
// recorded.
type plexTV struct {
	shares, invites string
	alreadyShared   bool
	failLists       bool
	requests        []string
}

func (p *plexTV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.requests = append(p.requests, r.Method+" "+r.URL.RequestURI())
	if p.failLists && r.Method == http.MethodGet && r.URL.Path != "/api/servers/abc123" {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/servers/abc123":
		io.WriteString(w, serverXML)
	case "POST /api/servers/abc123/shared_servers":
		if p.alreadyShared {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<Response code="400" status="You're already sharing this server with the invited account. Please edit your existing share."/>`)
			return
		}
		io.WriteString(w, `<SharedServer/>`)
	case "GET /api/servers/abc123/shared_servers":
		io.WriteString(w, `<MediaContainer>`+p.shares+`</MediaContainer>`)
	case "GET /api/invites/requested":
		io.WriteString(w, `<MediaContainer>`+p.invites+`</MediaContainer>`)
	case "DELETE /api/servers/abc123/shared_servers/71", "DELETE /api/servers/abc123/shared_servers/73",
		"DELETE /api/invites/requested/6002", "DELETE /api/invites/requested/new@example.com":
	default:
		http.NotFound(w, r)
	}
}

const (
	acceptedShare = `<SharedServer id="71" userID="6001" username="Friendly" email="Friend@Example.com"/>
		<SharedServer id="72" userID="6009" username="other" email="other@example.com"/>`
	// An invite to an address with no Plex account has the address as its ID.
	pendingInvite = `<Invite id="6002" email="pending@example.com" username="pend" friend="0" home="0" server="1"><Server name="media" numLibraries="2"/></Invite>
		<Invite id="new@example.com" email="new@example.com" friend="0" home="0" server="1"><Server name="media" numLibraries="2"/></Invite>
		<Invite id="6003" email="elsewhere@example.com" username="" friend="0" home="0" server="1"><Server name="another server" numLibraries="1"/></Invite>`
	// new@example.com after signing up and accepting.
	signedUpShare = `<SharedServer id="73" userID="6010" username="newbie" email="New@Example.com"/>`
)

// Invite finds the account's ID among accepted shares or pending invites,
// whether it just invited them or they already had access.
func TestInviteAccountID(t *testing.T) {
	cases := []struct {
		name, who     string
		alreadyShared bool
		failLists     bool
		want          Account
		already       bool
		unknown       bool
	}{
		{"new invite, pending", "pending@example.com", false, false, "id:6002", false, false},
		{"new invite by username", "pend", false, false, "id:6002", false, false},
		{"new invite, no Plex account", "New@example.com", false, false, accountEmail("new@example.com"), false, false},
		{"already shared, accepted, by email", "friend@example.com", true, false, "id:6001", true, false},
		{"already shared, accepted, by username", "friendly", true, false, "id:6001", true, false},
		{"already shared, pending", "Pending@Example.com", true, false, "id:6002", true, false},
		{"invite to another server doesn't count", "elsewhere@example.com", false, false, "", false, true},
		{"not listed anywhere", "nobody@example.com", false, false, "", false, true},
		{"already shared, lists fail", "friend@example.com", true, true, "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &plexTV{shares: acceptedShare, invites: pendingInvite, alreadyShared: tc.alreadyShared, failLists: tc.failLists}
			srv := httptest.NewServer(p)
			defer srv.Close()
			id, err := NewClient(srv.URL, "tok", "abc123").Invite(context.Background(), tc.who)
			if id != tc.want || errors.Is(err, ErrAlreadyShared) != tc.already || errors.Is(err, ErrAccountUnknown) != tc.unknown ||
				(err != nil && !tc.already && !tc.unknown) {
				t.Errorf("Invite = %q, %v; want %q, already %v, unknown %v", id, err, tc.want, tc.already, tc.unknown)
			}
			if err != nil && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.who)) {
				t.Errorf("error names the account: %v", err)
			}
			if strings.Contains(strings.ToLower(string(id)), "example") {
				t.Errorf("account %q holds the address", id)
			}
		})
	}
}

func TestRemoveAccess(t *testing.T) {
	cases := []struct {
		name    string
		shares  string
		account Account
		removed bool
		deleted string
	}{
		{"accepted share", acceptedShare, "id:6001", true, "DELETE /api/servers/abc123/shared_servers/71"},
		{"pending invite", acceptedShare, "id:6002", true, "DELETE /api/invites/requested/6002?friend=0&home=0&server=1"},
		{"pending invite, no Plex account", acceptedShare, accountEmail("new@example.com"), true,
			"DELETE /api/invites/requested/new@example.com?friend=0&home=0&server=1"},
		{"signed up and accepted since", acceptedShare + signedUpShare, accountEmail("new@example.com"), true,
			"DELETE /api/servers/abc123/shared_servers/73"},
		{"invite to another server", acceptedShare, "id:6003", false, ""},
		{"no access left", acceptedShare, "id:7000", false, ""},
		{"unknown kind", acceptedShare, "6001", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &plexTV{shares: tc.shares, invites: pendingInvite}
			srv := httptest.NewServer(p)
			defer srv.Close()
			removed, err := NewClient(srv.URL, "tok", "abc123").RemoveAccess(context.Background(), tc.account)
			var deletes []string
			for _, r := range p.requests {
				if strings.HasPrefix(r, "DELETE ") {
					deletes = append(deletes, r)
				}
			}
			var want []string
			if tc.deleted != "" {
				want = []string{tc.deleted}
			}
			if err != nil || removed != tc.removed || !slices.Equal(deletes, want) {
				t.Errorf("RemoveAccess = %v, %v; deletes %v, want %v %v", removed, err, deletes, tc.removed, want)
			}
		})
	}

	p := &plexTV{failLists: true}
	srv := httptest.NewServer(p)
	defer srv.Close()
	c := NewClient(srv.URL, "tok", "abc123")
	if _, err := c.RemoveAccess(context.Background(), "id:6001"); err == nil {
		t.Error("lists fail: want error")
	}
	if _, err := c.RemoveAccess(context.Background(), ""); err == nil {
		t.Error("no ID: want error")
	}
}
