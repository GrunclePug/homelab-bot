package plex

import (
	"context"
	"encoding/json"
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

	if err := c.Invite(ctx, "friend@example.com"); err != nil {
		t.Fatal(err)
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
	err = bad.Invite(ctx, "friend@example.com")
	if err == nil || !strings.Contains(err.Error(), "User could not be authenticated (code 1001)") {
		t.Errorf("bad token: err = %v", err)
	}

	missing := NewClient(srv.URL, "tok", "nope")
	if err := missing.Invite(ctx, "x"); err == nil {
		t.Error("unknown server: want error")
	}
}
