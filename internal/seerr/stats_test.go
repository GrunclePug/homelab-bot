package seerr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStats(t *testing.T) {
	answers := map[string]string{
		"/api/v1/status":        `{"version":"3.5.1","commitTag":"v3.5.1","updateAvailable":true,"commitsBehind":0,"restartRequired":false}`,
		"/api/v1/request/count": `{"total":51,"movie":41,"tv":10,"pending":2,"approved":48,"declined":0,"processing":47,"available":0,"completed":3}`,
		"/api/v1/issue/count":   `{"total":4,"video":1,"audio":1,"subtitles":1,"others":1,"open":3,"closed":1}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := answers[r.URL.Path]
		if !ok || r.Header.Get("X-Api-Key") != "key" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		io.WriteString(w, a)
	}))
	defer srv.Close()
	s, err := NewClient(srv.URL, "key").Stats(context.Background())
	want := Stats{Version: "3.5.1", UpdateAvailable: true, Pending: 2, Processing: 47, OpenIssues: 3}
	if err != nil || s != want {
		t.Errorf("Stats = %+v, %v; want %+v", s, err, want)
	}
	if _, err := NewClient(srv.URL, "bad").Stats(context.Background()); err == nil {
		t.Error("bad key: want error")
	}
}
