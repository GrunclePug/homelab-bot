package replace

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/grunclepug/homelab-bot/internal/arr"
	"github.com/grunclepug/homelab-bot/internal/seerr"
)

// fakeArr is a fake Sonarr or Radarr. GET responses come from get, keyed
// by path and query; every other request is recorded and answered 200,
// unless its "METHOD path" is in fail.
type fakeArr struct {
	mu       sync.Mutex
	get      map[string]string
	fail     map[string]bool // answered 500
	failCode map[string]int  // answered with this status
	calls    []string
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		body, ok := f.get[key]
		if !ok {
			http.Error(w, "not found: "+key, http.StatusNotFound)
			return
		}
		io.WriteString(w, body)
		return
	}
	b, _ := io.ReadAll(r.Body)
	call := r.Method + " " + r.URL.Path
	if len(b) > 0 {
		call += " " + string(b)
	}
	f.calls = append(f.calls, call)
	if f.fail[r.Method+" "+r.URL.Path] {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	if code := f.failCode[r.Method+" "+r.URL.Path]; code != 0 {
		http.Error(w, http.StatusText(code), code)
		return
	}
	io.WriteString(w, "{}")
}

func historyKey(param, id string) string {
	return "/api/v3/history?" + param + "=" + id + "&pageSize=100&sortDirection=descending&sortKey=date"
}

const seriesJSON = `[{"id":112,"title":"Shooter","year":2016,"tvdbId":311900}]`

const episodesJSON = `[
	{"id":101,"seriesId":112,"seasonNumber":1,"episodeNumber":1,"title":"Pilot","hasFile":true,"episodeFileId":501},
	{"id":102,"seriesId":112,"seasonNumber":1,"episodeNumber":2,"title":"Two","hasFile":true,"episodeFileId":502},
	{"id":103,"seriesId":112,"seasonNumber":1,"episodeNumber":3,"title":"Three","hasFile":false,"episodeFileId":0},
	{"id":201,"seriesId":112,"seasonNumber":2,"episodeNumber":1,"title":"Back","hasFile":true,"episodeFileId":601}
]`

const filesJSON = `[
	{"id":501,"seriesId":112,"seasonNumber":1,"size":1073741824,"releaseGroup":"GRP","quality":{"quality":{"name":"WEBDL-1080p"}}},
	{"id":502,"seriesId":112,"seasonNumber":1,"size":1073741824,"releaseGroup":"GRP","quality":{"quality":{"name":"WEBDL-1080p"}}},
	{"id":601,"seriesId":112,"seasonNumber":2,"size":2147483648,"releaseGroup":"","quality":{"quality":{"name":"HDTV-720p"}}}
]`

func newSonarr(t *testing.T) (*fakeArr, *arr.Sonarr) {
	t.Helper()
	f := &fakeArr{get: map[string]string{
		"/api/v3/series?tvdbId=311900":     seriesJSON,
		"/api/v3/series?tvdbId=1":          `[]`,
		"/api/v3/episode?seriesId=112":     episodesJSON,
		"/api/v3/episodefile?seriesId=112": filesJSON,
		// Episodes 101 and 102 came from the same season pack; the newer
		// grab of 101 never imported.
		historyKey("episodeId", "101"): `{"records":[
			{"id":9,"eventType":"grabbed","downloadId":"NEW","sourceTitle":"Show.S01E01.Upgrade"},
			{"id":8,"eventType":"downloadFolderImported","downloadId":"PACK"},
			{"id":7,"eventType":"grabbed","downloadId":"PACK","sourceTitle":"Show.S01.Pack"}]}`,
		historyKey("episodeId", "102"): `{"records":[
			{"id":11,"eventType":"downloadFolderImported","downloadId":"PACK"},
			{"id":10,"eventType":"grabbed","downloadId":"PACK","sourceTitle":"Show.S01.Pack"}]}`,
		historyKey("episodeId", "201"): `{"records":[]}`,
	}, fail: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, arr.NewSonarr(srv.URL, "key")
}

func newRadarr(t *testing.T) (*fakeArr, *arr.Radarr) {
	t.Helper()
	f := &fakeArr{get: map[string]string{
		"/api/v3/movie?tmdbId=7485": `[{"id":491,"title":"Shooter","year":2007,"tmdbId":7485,"hasFile":true,
			"movieFile":{"id":496,"movieId":491,"size":3846261673,"releaseGroup":"CoSmo","quality":{"quality":{"name":"Bluray-1080p"}}}}]`,
		"/api/v3/movie?tmdbId=1": `[{"id":5,"title":"Empty","year":2001,"tmdbId":1,"hasFile":false}]`,
		historyKey("movieIds", "491"): `{"records":[
			{"id":508,"eventType":"downloadFolderImported","downloadId":"E9"},
			{"id":507,"eventType":"grabbed","downloadId":"E9","sourceTitle":"Shooter.2007.BluRay"},
			{"id":506,"eventType":"downloadFailed","downloadId":"5F"},
			{"id":505,"eventType":"grabbed","downloadId":"5F","sourceTitle":"Shooter.2007.Bad"}]}`,
	}, fail: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, arr.NewRadarr(srv.URL, "key")
}

func tvIssue(season, episode int) seerr.Issue {
	return seerr.Issue{ID: 1, ProblemSeason: season, ProblemEpisode: episode,
		Media: seerr.Media{MediaType: seerr.MediaTV, TMDBID: 66857, TVDBID: 311900}}
}

func TestLookup(t *testing.T) {
	_, so := newSonarr(t)
	_, ra := newRadarr(t)
	s := Service{Sonarr: so, Radarr: ra}

	tests := []struct {
		name      string
		issue     seerr.Issue
		wantErr   string
		wantScope Scope
		wantPick  bool
		wantItems []int
	}{
		{name: "movie", issue: seerr.Issue{Media: seerr.Media{MediaType: "movie", TMDBID: 7485}},
			wantScope: ScopeOne, wantItems: []int{491}},
		{name: "movie without file", issue: seerr.Issue{Media: seerr.Media{MediaType: "movie", TMDBID: 1}},
			wantErr: "Radarr has no file for Empty (2001)."},
		{name: "movie not in radarr", issue: seerr.Issue{Media: seerr.Media{MediaType: "movie", TMDBID: 2}},
			wantErr: "radarr: find movie tmdb 2"},
		{name: "episode", issue: tvIssue(1, 2), wantScope: ScopeOne, wantItems: []int{102}},
		{name: "episode without file", issue: tvIssue(1, 3), wantErr: "Sonarr has no file for Shooter (2016) S01E03."},
		{name: "season", issue: tvIssue(1, 0), wantScope: ScopeSeason, wantPick: true, wantItems: []int{101, 102}},
		{name: "season with one file", issue: tvIssue(2, 0), wantScope: ScopeSeason, wantPick: false, wantItems: []int{201}},
		{name: "whole show", issue: tvIssue(0, 0), wantScope: ScopeShow, wantPick: true, wantItems: []int{101, 102, 201}},
		{name: "show not in sonarr", issue: seerr.Issue{Media: seerr.Media{MediaType: "tv", TVDBID: 1}},
			wantErr: "Sonarr doesn't have this show (TVDB 1)."},
		{name: "show without tvdb", issue: seerr.Issue{Media: seerr.Media{MediaType: "tv"}},
			wantErr: "Seerr has no TVDB ID for this show."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := s.Lookup(context.Background(), tt.issue)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Scope != tt.wantScope || p.NeedsPick() != tt.wantPick {
				t.Errorf("scope = %d, pick = %v; want %d, %v", p.Scope, p.NeedsPick(), tt.wantScope, tt.wantPick)
			}
			var items []int
			for _, c := range p.Candidates {
				items = append(items, c.ItemID)
			}
			if !slices.Equal(items, tt.wantItems) {
				t.Errorf("items = %v, want %v", items, tt.wantItems)
			}
		})
	}

	p, _ := s.Lookup(context.Background(), tvIssue(1, 1))
	got := p.Candidates[0]
	if got.Label != "S01E01 · Pilot" || got.FileID != 501 || got.Quality != "WEBDL-1080p" || got.Group != "GRP" || got.Size != 1<<30 {
		t.Errorf("target = %+v", got)
	}
	var ue *UserError
	if _, err := s.Lookup(context.Background(), tvIssue(1, 3)); !errors.As(err, &ue) {
		t.Errorf("missing file: want a UserError, got %T", err)
	}
}

func TestSelect(t *testing.T) {
	cands := []Target{
		{ItemID: 101, FileID: 1, Season: 1}, {ItemID: 102, FileID: 2, Season: 1},
		{ItemID: 201, FileID: 3, Season: 2}, {ItemID: 301, FileID: 4, Season: 3},
	}
	tests := []struct {
		values []string
		want   []int
	}{
		{[]string{PickAll}, []int{101, 102, 201, 301}},
		{[]string{PickSeason(1)}, []int{101, 102}},
		{[]string{PickSeason(3), PickSeason(1)}, []int{101, 102, 301}},
		{[]string{PickItem(cands[2])}, []int{201}},
		{[]string{PickItem(cands[0]), PickSeason(1)}, []int{101, 102}},
		{[]string{"e:999", "junk"}, nil},
	}
	for _, tt := range tests {
		var got []int
		for _, c := range Select(cands, tt.values) {
			got = append(got, c.ItemID)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("Select(%v) = %v, want %v", tt.values, got, tt.want)
		}
	}
	if s := Seasons(cands); !slices.Equal(s, []Season{{1, 2}, {2, 1}, {3, 1}}) {
		t.Errorf("Seasons = %v", s)
	}
}

func episodeTargets(t *testing.T, s Service, issue seerr.Issue) []Target {
	t.Helper()
	p, err := s.Lookup(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	return p.Candidates
}

func TestRunEpisodes(t *testing.T) {
	tests := []struct {
		name      string
		fail      []string
		wantCalls []string
		wantLines []string
	}{
		{
			name: "all good",
			// The grab behind episode 101's file is the season pack (7),
			// not the newer grab that never imported (9). Episode 102 is
			// in the same pack, so it isn't marked failed twice.
			wantCalls: []string{
				"POST /api/v3/history/failed/7",
				"DELETE /api/v3/episodefile/501",
				"DELETE /api/v3/episodefile/502",
				"DELETE /api/v3/episodefile/601",
				`POST /api/v3/command {"episodeIds":[101,102,201],"name":"EpisodeSearch"}`,
			},
			wantLines: []string{
				"S01E01 · Pilot: blocklisted Show.S01.Pack · deleted · search started",
				"S01E02 · Two: same release as above, already blocklisted · deleted · search started",
				"S02E01 · Back: not blocklisted (no grab in history) · deleted · search started",
			},
		},
		{
			name: "blocklist fails, delete and search go on",
			fail: []string{"POST /api/v3/history/failed/7"},
			wantCalls: []string{
				"POST /api/v3/history/failed/7",
				"DELETE /api/v3/episodefile/501",
				"POST /api/v3/history/failed/10",
				"DELETE /api/v3/episodefile/502",
				"DELETE /api/v3/episodefile/601",
				`POST /api/v3/command {"episodeIds":[101,102,201],"name":"EpisodeSearch"}`,
			},
		},
		{
			name: "delete fails, that file isn't searched",
			fail: []string{"DELETE /api/v3/episodefile/502"},
			wantCalls: []string{
				"POST /api/v3/history/failed/7",
				"DELETE /api/v3/episodefile/501",
				"DELETE /api/v3/episodefile/502",
				"DELETE /api/v3/episodefile/601",
				`POST /api/v3/command {"episodeIds":[101,201],"name":"EpisodeSearch"}`,
			},
		},
		{
			name: "every delete fails, no search",
			fail: []string{"DELETE /api/v3/episodefile/501", "DELETE /api/v3/episodefile/502", "DELETE /api/v3/episodefile/601"},
			wantCalls: []string{
				"POST /api/v3/history/failed/7",
				"DELETE /api/v3/episodefile/501",
				"DELETE /api/v3/episodefile/502",
				"DELETE /api/v3/episodefile/601",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, so := newSonarr(t)
			for _, c := range tt.fail {
				f.fail[c] = true
			}
			s := Service{Sonarr: so}
			results := s.Run(context.Background(), episodeTargets(t, s, tvIssue(0, 0)))
			if !slices.Equal(f.calls, tt.wantCalls) {
				t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(tt.wantCalls, "\n"))
			}
			if tt.wantLines != nil {
				var got []string
				for _, r := range results {
					got = append(got, r.Summary())
				}
				if !slices.Equal(got, tt.wantLines) {
					t.Errorf("summaries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.wantLines, "\n"))
				}
			}
		})
	}
}

func TestRunEpisodeFailureSummaries(t *testing.T) {
	f, so := newSonarr(t)
	f.fail["POST /api/v3/history/failed/7"] = true
	f.fail["DELETE /api/v3/episodefile/501"] = true
	s := Service{Sonarr: so}
	targets := episodeTargets(t, s, tvIssue(1, 1))
	r := s.Run(context.Background(), targets)[0]
	if r.Deleted() || r.Searched || r.BlocklistErr == nil {
		t.Fatalf("result = %+v", r)
	}
	got := r.Summary()
	for _, want := range []string{"not blocklisted (", "delete failed (", "not searched"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
}

func TestRunSearchFails(t *testing.T) {
	f, so := newSonarr(t)
	f.fail["POST /api/v3/command"] = true
	s := Service{Sonarr: so}
	r := s.Run(context.Background(), episodeTargets(t, s, tvIssue(2, 1)))[0]
	if !r.Deleted() || r.Searched || r.SearchErr == nil {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Summary(), "deleted · search failed (") {
		t.Errorf("summary = %q", r.Summary())
	}
}

func TestRunMovie(t *testing.T) {
	f, ra := newRadarr(t)
	s := Service{Radarr: ra}
	p, err := s.Lookup(context.Background(), seerr.Issue{Media: seerr.Media{MediaType: "movie", TMDBID: 7485}})
	if err != nil {
		t.Fatal(err)
	}
	r := s.Run(context.Background(), p.Candidates)[0]
	want := []string{
		"POST /api/v3/history/failed/507",
		"DELETE /api/v3/moviefile/496",
		`POST /api/v3/command {"movieIds":[491],"name":"MoviesSearch"}`,
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := r.Summary(); got != "Shooter (2007): blocklisted Shooter.2007.BluRay · deleted · search started" {
		t.Errorf("summary = %q", got)
	}
}

// newDoubleSonarr is a fake Sonarr whose season 4 has one file holding
// S04E01 and S04E02.
func newDoubleSonarr(t *testing.T) (*fakeArr, *arr.Sonarr, string) {
	t.Helper()
	f := &fakeArr{get: map[string]string{
		"/api/v3/series?tvdbId=311900": seriesJSON,
		"/api/v3/episode?seriesId=112": `[
			{"id":401,"seriesId":112,"seasonNumber":4,"episodeNumber":1,"title":"One","hasFile":true,"episodeFileId":701},
			{"id":402,"seriesId":112,"seasonNumber":4,"episodeNumber":2,"title":"Two","hasFile":true,"episodeFileId":701},
			{"id":403,"seriesId":112,"seasonNumber":4,"episodeNumber":3,"title":"Three","hasFile":true,"episodeFileId":702}]`,
		"/api/v3/episodefile?seriesId=112": `[{"id":701,"size":1,"quality":{"quality":{"name":"WEBDL-1080p"}}},{"id":702,"size":1}]`,
		historyKey("episodeId", "401"):     `{"records":[{"id":5,"eventType":"grabbed","downloadId":"D","sourceTitle":"Show.S04E01E02"}]}`,
		historyKey("episodeId", "403"):     `{"records":[]}`,
	}, fail: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, arr.NewSonarr(srv.URL, "key"), srv.URL
}

func items(ts []Target) []int {
	var out []int
	for _, t := range ts {
		out = append(out, t.ItemID)
	}
	return out
}

func TestSharedFile(t *testing.T) {
	ctx := context.Background()
	f, so, _ := newDoubleSonarr(t)
	s := Service{Sonarr: so}

	// An issue about S04E01 alone also takes in S04E02: deleting the file
	// removes both.
	p, err := s.Lookup(ctx, tvIssue(4, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := items(p.Candidates); !slices.Equal(got, []int{401, 402}) {
		t.Errorf("episode issue candidates = %v, want both episodes of the file", got)
	}
	if SharedFile(p.Candidates)[1] != "S04E01 · One" {
		t.Errorf("SharedFile = %q", SharedFile(p.Candidates))
	}

	// Picking one episode of a season picks its partner too.
	season, _ := s.Lookup(ctx, tvIssue(4, 0))
	if got := items(Select(season.Candidates, []string{"e:402"})); !slices.Equal(got, []int{401, 402}) {
		t.Errorf("Select(e:402) = %v", got)
	}

	results := s.Run(ctx, p.Candidates)
	want := []string{
		"POST /api/v3/history/failed/5",
		"DELETE /api/v3/episodefile/701",
		`POST /api/v3/command {"episodeIds":[401,402],"name":"EpisodeSearch"}`,
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if got := results[1].Summary(); got != "S04E02 · Two: same file as S04E01 · One · deleted · search started" {
		t.Errorf("summary = %q", got)
	}
}

func TestRunAlreadyGone(t *testing.T) {
	f, so, _ := newDoubleSonarr(t)
	f.failCode = map[string]int{"DELETE /api/v3/episodefile/702": http.StatusNotFound}
	s := Service{Sonarr: so}
	p, _ := s.Lookup(context.Background(), tvIssue(4, 3))
	r := s.Run(context.Background(), p.Candidates)[0]
	if !r.Deleted() || !r.AlreadyGone || !r.Searched {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Summary(), "file was already gone · search started") {
		t.Errorf("summary = %q", r.Summary())
	}
}

func TestSummaryHidesHosts(t *testing.T) {
	ctx := context.Background()
	f, so, url := newDoubleSonarr(t)
	f.fail["DELETE /api/v3/episodefile/702"] = true
	s := Service{Sonarr: so}
	p, _ := s.Lookup(ctx, tvIssue(4, 3))
	r := s.Run(ctx, p.Candidates)[0]
	if got := r.Summary(); !strings.Contains(got, "delete failed (Sonarr answered 500 Internal Server Error)") {
		t.Errorf("summary = %q", got)
	}
	if r.Err() == nil || !strings.Contains(r.Err().Error(), "boom") {
		t.Errorf("Err = %v, want the details for the log", r.Err())
	}

	// Sonarr down: the URL stays out of the summary.
	down := arr.NewSonarr("http://127.0.0.1:1", "key")
	r = Service{Sonarr: down}.Run(ctx, p.Candidates)[0]
	got := r.Summary()
	for _, bad := range []string{"http", "127.0.0.1", url} {
		if strings.Contains(got, bad) {
			t.Errorf("summary shows %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "Sonarr unreachable") {
		t.Errorf("summary = %q", got)
	}
}
