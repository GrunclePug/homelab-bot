package arr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStats(t *testing.T) {
	answers := map[string]string{
		"/api/v3/system/status": `{"appName":"Sonarr","version":"4.0.20.1234","startupPath":"/app"}`,
		"/api/v3/health":        `[{"source":"IndexerLongTermStatusCheck","type":"warning","message":"Indexers unavailable due to failures for more than 6 hours: Example","wikiUrl":"https://wiki.example"}]`,
		"/api/v3/queue/status":  `{"totalCount":227,"count":227,"unknownCount":0,"errors":false,"warnings":false}`,
		"/api/v3/diskspace":     `[{"path":"/","label":"","freeSpace":100,"totalSpace":200},{"path":"/media","label":"","freeSpace":6000,"totalSpace":18000}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := answers[r.URL.Path]
		if !ok || r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != "key" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, a)
	}))
	defer srv.Close()

	s, err := NewSonarr(srv.URL, "key").Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != "4.0.20.1234" || s.Queue != 227 || len(s.Health) != 1 || s.Health[0].Type != "warning" ||
		len(s.Disks) != 2 || s.Disks[1].FreeSpace != 6000 || s.Disks[1].TotalSpace != 18000 {
		t.Errorf("stats = %+v", s)
	}

	delete(answers, "/api/v3/health")
	if _, err := NewRadarr(srv.URL, "key").Stats(context.Background()); err == nil || Brief(err) != "Radarr answered 404 Not Found" {
		t.Errorf("missing endpoint: err = %v (%s)", err, Brief(err))
	}
}
