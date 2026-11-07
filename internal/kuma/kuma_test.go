package kuma

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParsePageURL(t *testing.T) {
	tests := []struct {
		in, base, slug string
		ok             bool
	}{
		{"https://kuma.example.com/status/homelab", "https://kuma.example.com", "homelab", true},
		{"http://10.0.0.5:3001/status/media/", "http://10.0.0.5:3001", "media", true},
		{"https://example.com/kuma/status/home", "https://example.com/kuma", "home", true},
		{"https://kuma.example.com/dashboard", "", "", false},
		{"https://kuma.example.com/status/", "", "", false},
		{"https://kuma.example.com/status/a/b", "", "", false},
		{"kuma.example.com/status/home", "", "", false},
	}
	for _, tt := range tests {
		base, slug, err := ParsePageURL(tt.in)
		if (err == nil) != tt.ok || base != tt.base || slug != tt.slug {
			t.Errorf("ParsePageURL(%q) = %q, %q, %v", tt.in, base, slug, err)
		}
	}
}

// The answers follow Uptime Kuma 2.5's status page router.
func TestGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status-page/home":
			io.WriteString(w, `{"config":{"slug":"home","title":"Home"},"incidents":[],"maintenanceList":[],
				"publicGroupList":[
					{"id":1,"name":"Media","weight":1,"monitorList":[{"id":3,"name":"Plex","sendUrl":0,"type":"http"},{"id":4,"name":"Seerr","sendUrl":0,"type":"http"}]},
					{"id":2,"name":"Network","weight":2,"monitorList":[{"id":7,"name":"DNS","sendUrl":0,"type":"dns"},{"id":8,"name":"New","sendUrl":0,"type":"dns"}]}]}`)
		case "/api/status-page/heartbeat/home":
			io.WriteString(w, `{"heartbeatList":{
					"3":[{"status":0,"time":"2026-11-07 10:00:00","msg":"","ping":null},{"status":1,"time":"2026-11-07 10:01:00","msg":"","ping":20}],
					"4":[{"status":1,"time":"2026-11-07 10:00:00","msg":"","ping":5},{"status":0,"time":"2026-11-07 10:01:00","msg":"","ping":null}]},
				"uptimeList":{"3_24":0.9993,"4_24":0.5,"7_24":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL + "/status/home")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Groups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Group{
		{"Media", []Monitor{{"Plex", Up, true, 0.9993, true}, {"Seerr", Down, true, 0.5, true}}},
		{"Network", []Monitor{{"DNS", 0, false, 0, true}, {"New", 0, false, 0, false}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Groups =\n%+v\nwant\n%+v", got, want)
	}

	missing, _ := NewClient(srv.URL + "/status/nope")
	if _, err := missing.Groups(context.Background()); err == nil {
		t.Error("missing page: want error")
	}
}
