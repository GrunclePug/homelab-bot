package qbit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	torrents := []Torrent{
		{Name: "seeding", State: "uploading", Progress: 1},
		{Name: "stalled seed", State: "stalledUP", Progress: 1},
		{Name: "stopped seed", State: "stoppedUP", Progress: 1},
		{Name: "paused seed", State: "pausedUP", Progress: 1},
		{Name: "done but moving", State: "moving", Progress: 1},
		{Name: "slow", State: "downloading", Progress: 0.5, Size: 2 << 30, DLSpeed: 1 << 20, ETA: 7200},
		{Name: "fast", State: "forcedDL", Progress: 0.999, Size: 1 << 30, DLSpeed: 10 << 20, ETA: 90},
		{Name: "meta", State: "metaDL", ETA: 8640000},
		{Name: "stuck", State: "stalledDL", Progress: 0.25, Size: 700 << 20},
		{Name: "second in line", State: "queuedDL", Size: 3 << 30, Priority: 2},
		{Name: "first in line", State: "queuedDL", Size: 4 << 30, Priority: 1},
		{Name: "stopped", State: "stoppedDL", Progress: 0.1},
		{Name: "broken", State: "missingFiles", Progress: 0.3},
	}
	q := Build(torrents, MaxRows)
	want := strings.Join([]string{
		"__**Downloading**__",
		"**fast**",
		"99% of 1.0 GiB · 10.0 MiB/s · ETA 1m",
		"**slow**",
		"50% of 2.0 GiB · 1.0 MiB/s · ETA 2h 0m",
		"**meta**",
		"0% of 0 B · 0 B/s · ETA unknown",
		"",
		"__**Stalled**__",
		"**stuck**",
		"25% of 700.0 MiB",
		"",
		"__**Queued**__",
		"**first in line**",
		"4.0 GiB",
		"**second in line**",
		"3.0 GiB",
		"",
		"__**Other**__",
		"**stopped**",
		"stopped",
		"**broken**",
		"missing files",
	}, "\n")
	if got := q.Text(); got != want {
		t.Errorf("text:\n%s\n\nwant:\n%s", got, want)
	}
	if got, want := q.Footer(), "3 downloading · 1 stalled · 2 queued · 2 other"; got != want {
		t.Errorf("footer = %q, want %q", got, want)
	}
}

func TestBuildRowLimit(t *testing.T) {
	var ts []Torrent
	for i := range 18 {
		ts = append(ts, Torrent{Name: fmt.Sprintf("dl%02d", i), State: "downloading", ETA: int64(i)})
	}
	for i := range 4 {
		ts = append(ts, Torrent{Name: fmt.Sprintf("q%d", i), State: "queuedDL", Priority: i + 1})
	}
	ts = append(ts, Torrent{Name: "stopped", State: "stoppedDL"})

	q := Build(ts, MaxRows)
	rows := 0
	for _, s := range q.Sections {
		rows += len(s.Rows)
	}
	if rows != MaxRows {
		t.Errorf("rows = %d, want %d", rows, MaxRows)
	}
	text := q.Text()
	for _, want := range []string{"**q1**", "*and 2 more*", "__**Other**__\n*and 1 more*"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "**q2**") {
		t.Errorf("text shows a row past the limit:\n%s", text)
	}
	if got, want := q.Footer(), "18 downloading · 4 queued · 1 other"; got != want {
		t.Errorf("footer = %q, want %q", got, want)
	}
}

func TestBuildEmpty(t *testing.T) {
	q := Build([]Torrent{{Name: "seed", State: "stalledUP", Progress: 1}}, MaxRows)
	if !q.Empty() || q.Text() != "Nothing downloading or queued right now." || q.Footer() != "" {
		t.Errorf("queue = %+v, text %q, footer %q", q, q.Text(), q.Footer())
	}
}

func TestFormat(t *testing.T) {
	bytes := map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 40: "3.0 TiB"}
	for in, want := range bytes {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
	etas := map[int64]string{-1: "unknown", 8640000: "unknown", 42: "42s", 600: "10m", 3661: "1h 1m", 90000: "1d 1h"}
	for in, want := range etas {
		if got := ETA(in); got != want {
			t.Errorf("ETA(%d) = %q, want %q", in, got, want)
		}
	}
	if got := name("a_b*c"); got != `a\_b\*c` {
		t.Errorf("name escape = %q", got)
	}
	if got := []rune(name(strings.Repeat("x", 100))); len(got) != maxName || got[maxName-1] != '…' {
		t.Errorf("long name = %q", string(got))
	}
}

func TestClientLogin(t *testing.T) {
	logins := 0
	sid := "first"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			r.ParseForm()
			if r.Header.Get("Referer") == "" {
				http.Error(w, "csrf", http.StatusUnauthorized)
				return
			}
			if r.PostForm.Get("username") != "admin" || r.PostForm.Get("password") != "pw" {
				fmt.Fprint(w, "Fails.")
				return
			}
			logins++
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/"})
			fmt.Fprint(w, "Ok.")
		case "/api/v2/torrents/info":
			c, err := r.Cookie("SID")
			if err != nil || c.Value != sid {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `[{"hash":"h","name":"x","state":"downloading","progress":0.5,"size":10,"dlspeed":1,"eta":5,"priority":1}]`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	c := NewClient(srv.URL, "admin", "pw")
	ts, err := c.Torrents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].State != "downloading" || ts[0].Progress != 0.5 {
		t.Errorf("torrents = %+v", ts)
	}
	if _, err := c.Torrents(ctx); err != nil || logins != 1 {
		t.Errorf("second call: err %v, logins %d; want reuse of the session", err, logins)
	}

	sid = "second" // the session expires
	if _, err := c.Torrents(ctx); err != nil || logins != 2 {
		t.Errorf("after expiry: err %v, logins %d; want one new login", err, logins)
	}

	bad := NewClient(srv.URL, "admin", "wrong")
	if _, err := bad.Torrents(ctx); err == nil || !strings.Contains(err.Error(), "login failed") {
		t.Errorf("bad password: err = %v", err)
	}
}
