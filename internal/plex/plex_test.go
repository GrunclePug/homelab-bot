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
	if err != nil || id != 1 {
		t.Fatalf("Invite = %d, %v; want share 1", id, err)
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

// plex.tv's answer to an invite may hold the new share on its own, in a
// MediaContainer, or not at all; then the share is looked up by account.
func TestInviteShareID(t *testing.T) {
	const list = `<MediaContainer size="2">
  <SharedServer id="70" username="other" email="other@example.com"/>
  <SharedServer id="71" username="Friendly" email="Friend@Example.com"/>
</MediaContainer>`
	cases := []struct {
		name, answer string
		listCode     int
		who          string
		want         int
		unknown      bool
	}{
		{"element", `<SharedServer id="55" invitedEmail="friend@example.com"/>`, 200, "friend@example.com", 55, false},
		{"container", `<MediaContainer size="1"><SharedServer id="56"/></MediaContainer>`, 200, "friend@example.com", 56, false},
		{"lookup by email", ``, 200, "friend@example.com", 71, false},
		{"lookup by username", `<MediaContainer size="0"/>`, 200, "friendly", 71, false},
		{"not listed", ``, 200, "nobody@example.com", 0, true},
		{"list fails", ``, 500, "friend@example.com", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/servers/abc123":
					io.WriteString(w, serverXML)
				case "POST /api/servers/abc123/shared_servers":
					io.WriteString(w, tc.answer)
				case "GET /api/servers/abc123/shared_servers":
					w.WriteHeader(tc.listCode)
					io.WriteString(w, list)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			id, err := NewClient(srv.URL, "tok", "abc123").Invite(context.Background(), tc.who)
			if id != tc.want || errors.Is(err, ErrShareUnknown) != tc.unknown || (err != nil && !tc.unknown) {
				t.Errorf("Invite = %d, %v; want %d, unknown %v", id, err, tc.want, tc.unknown)
			}
			if err != nil && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.who)) {
				t.Errorf("error names the account: %v", err)
			}
		})
	}
}

func TestRemoveShare(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		deleted = append(deleted, r.URL.Path)
		switch r.URL.Path {
		case "/api/servers/abc123/shared_servers/71":
		case "/api/servers/abc123/shared_servers/72":
			http.NotFound(w, r)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := NewClient(srv.URL, "tok", "abc123")
	if err := c.RemoveShare(ctx, 71); err != nil {
		t.Errorf("remove: %v", err)
	}
	if err := c.RemoveShare(ctx, 72); err != nil {
		t.Errorf("already gone: %v", err)
	}
	if err := c.RemoveShare(ctx, 73); err == nil {
		t.Error("server error: want error")
	}
	if err := c.RemoveShare(ctx, 0); err == nil {
		t.Error("no ID: want error")
	}
	if want := []string{"/api/servers/abc123/shared_servers/71", "/api/servers/abc123/shared_servers/72", "/api/servers/abc123/shared_servers/73"}; !slices.Equal(deleted, want) {
		t.Errorf("deleted %v, want %v", deleted, want)
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

// plex.tv refuses to share the server twice with one account; Invite then
// finds the share the account already has.
func TestInviteAlreadyShared(t *testing.T) {
	for _, tc := range []struct {
		name, list string
		want       int
		unknown    bool
	}{
		{"share found", `<MediaContainer><SharedServer id="71" username="friendly" email="Friend@Example.com"/></MediaContainer>`, 71, false},
		{"share not listed", `<MediaContainer size="0"/>`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/servers/abc123":
					io.WriteString(w, serverXML)
				case "POST /api/servers/abc123/shared_servers":
					// As plex.tv answered on 2026-11-07.
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<Response code="400" status="You're already sharing this server with the invited account. Please edit your existing share."/>`)
				case "GET /api/servers/abc123/shared_servers":
					io.WriteString(w, tc.list)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			id, err := NewClient(srv.URL, "tok", "abc123").Invite(context.Background(), "friend@example.com")
			if id != tc.want || !errors.Is(err, ErrAlreadyShared) || errors.Is(err, ErrShareUnknown) != tc.unknown {
				t.Errorf("Invite = %d, %v; want %d, already shared, unknown %v", id, err, tc.want, tc.unknown)
			}
		})
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
