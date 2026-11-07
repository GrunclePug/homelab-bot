// Package replace replaces bad media files: it works out which file a Seerr
// issue is about, then blocklists the release behind it, deletes it and
// searches for a new one.
package replace

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/grunclepug/homelab-bot/internal/arr"
	"github.com/grunclepug/homelab-bot/internal/seerr"
)

// Sonarr is the part of the Sonarr client the flow uses.
type Sonarr interface {
	SeriesByTVDB(ctx context.Context, tvdbID int) (arr.Series, bool, error)
	Episodes(ctx context.Context, seriesID int) ([]arr.Episode, error)
	EpisodeFiles(ctx context.Context, seriesID int) ([]arr.EpisodeFile, error)
	History(ctx context.Context, episodeID int) ([]arr.HistoryRecord, error)
	MarkFailed(ctx context.Context, historyID int) error
	DeleteEpisodeFile(ctx context.Context, fileID int) error
	SearchEpisodes(ctx context.Context, episodeIDs []int) error
}

// Radarr is the part of the Radarr client the flow uses.
type Radarr interface {
	MovieByTMDB(ctx context.Context, tmdbID int) (arr.Movie, bool, error)
	History(ctx context.Context, movieID int) ([]arr.HistoryRecord, error)
	MarkFailed(ctx context.Context, historyID int) error
	DeleteMovieFile(ctx context.Context, fileID int) error
	SearchMovies(ctx context.Context, movieIDs []int) error
}

// Service runs the flow against Sonarr and Radarr.
type Service struct {
	Sonarr Sonarr
	Radarr Radarr
}

// Kind says which app a target lives in.
type Kind int

// Target kinds.
const (
	Movie Kind = iota
	Episode
)

// Target is one file to replace.
type Target struct {
	Kind    Kind
	ItemID  int // Sonarr episode ID or Radarr movie ID
	FileID  int
	Season  int
	Number  int // episode number
	Label   string
	Quality string
	Group   string
	Size    int64
}

// Scope says how much of a show an issue covers.
type Scope int

// Issue scopes.
const (
	ScopeOne    Scope = iota // a movie or a single episode
	ScopeSeason              // every episode of one season
	ScopeShow                // the whole show
)

// Plan is what a Replace press found: the title and the candidate files.
// For a season or show, the admin picks among the candidates.
type Plan struct {
	Title      string
	Scope      Scope
	Season     int // for ScopeSeason
	Candidates []Target
}

// NeedsPick reports whether the admin has to choose among the candidates.
func (p Plan) NeedsPick() bool {
	return p.Scope != ScopeOne && len(p.Candidates) > 1
}

// UserError is a problem worth showing to the admin as it is, such as a
// title missing from Sonarr.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error {
	return &UserError{Msg: fmt.Sprintf(format, a...)}
}

// Lookup works out the files a Seerr issue is about.
func (s Service) Lookup(ctx context.Context, is seerr.Issue) (Plan, error) {
	switch is.Media.MediaType {
	case seerr.MediaMovie:
		return s.lookupMovie(ctx, is)
	case seerr.MediaTV:
		return s.lookupShow(ctx, is)
	default:
		return Plan{}, userErr("Seerr issue %d has unknown media type %q.", is.ID, is.Media.MediaType)
	}
}

func (s Service) lookupMovie(ctx context.Context, is seerr.Issue) (Plan, error) {
	if is.Media.TMDBID == 0 {
		return Plan{}, userErr("Seerr has no TMDB ID for this movie.")
	}
	m, ok, err := s.Radarr.MovieByTMDB(ctx, is.Media.TMDBID)
	if err != nil {
		return Plan{}, err
	}
	if !ok {
		return Plan{}, userErr("Radarr doesn't have this movie (TMDB %d).", is.Media.TMDBID)
	}
	title := withYear(m.Title, m.Year)
	if !m.HasFile || m.MovieFile == nil {
		return Plan{}, userErr("Radarr has no file for %s.", title)
	}
	f := m.MovieFile
	return Plan{
		Title: title,
		Scope: ScopeOne,
		Candidates: []Target{{
			Kind:    Movie,
			ItemID:  m.ID,
			FileID:  f.ID,
			Label:   title,
			Quality: f.Quality.Quality.Name,
			Group:   f.ReleaseGroup,
			Size:    f.Size,
		}},
	}, nil
}

func (s Service) lookupShow(ctx context.Context, is seerr.Issue) (Plan, error) {
	if is.Media.TVDBID == 0 {
		return Plan{}, userErr("Seerr has no TVDB ID for this show.")
	}
	se, ok, err := s.Sonarr.SeriesByTVDB(ctx, is.Media.TVDBID)
	if err != nil {
		return Plan{}, err
	}
	if !ok {
		return Plan{}, userErr("Sonarr doesn't have this show (TVDB %d).", is.Media.TVDBID)
	}
	title := withYear(se.Title, se.Year)
	eps, err := s.Sonarr.Episodes(ctx, se.ID)
	if err != nil {
		return Plan{}, err
	}
	files, err := s.Sonarr.EpisodeFiles(ctx, se.ID)
	if err != nil {
		return Plan{}, err
	}
	byID := make(map[int]arr.EpisodeFile, len(files))
	for _, f := range files {
		byID[f.ID] = f
	}

	var all []Target // every episode with a file
	for _, e := range eps {
		if !e.HasFile || e.EpisodeFileID == 0 {
			continue
		}
		f := byID[e.EpisodeFileID]
		all = append(all, Target{
			Kind:    Episode,
			ItemID:  e.ID,
			FileID:  e.EpisodeFileID,
			Season:  e.SeasonNumber,
			Number:  e.EpisodeNumber,
			Label:   EpisodeCode(e.SeasonNumber, e.EpisodeNumber) + " · " + e.Title,
			Quality: f.Quality.Quality.Name,
			Group:   f.ReleaseGroup,
			Size:    f.Size,
		})
	}
	slices.SortFunc(all, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Number, b.Number))
	})
	var picked []Target
	for _, t := range all {
		if is.ProblemSeason > 0 && t.Season != is.ProblemSeason {
			continue
		}
		if is.ProblemEpisode > 0 && t.Number != is.ProblemEpisode {
			continue
		}
		picked = append(picked, t)
	}
	// Deleting a file that holds several episodes, such as S01E01E02,
	// removes all of them, so they're all replaced together.
	cands := withSharedFiles(all, picked)

	p := Plan{Title: title, Candidates: cands}
	switch {
	case is.ProblemSeason > 0 && is.ProblemEpisode > 0:
		p.Scope = ScopeOne
		if len(cands) == 0 {
			return Plan{}, userErr("Sonarr has no file for %s %s.", title, EpisodeCode(is.ProblemSeason, is.ProblemEpisode))
		}
	case is.ProblemSeason > 0:
		p.Scope, p.Season = ScopeSeason, is.ProblemSeason
		if len(cands) == 0 {
			return Plan{}, userErr("Sonarr has no files for %s season %d.", title, is.ProblemSeason)
		}
	default:
		p.Scope = ScopeShow
		if len(cands) == 0 {
			return Plan{}, userErr("Sonarr has no files for %s.", title)
		}
	}
	return p, nil
}

// Values a pick menu sends back.
const (
	PickAll       = "all"
	pickSeasonPfx = "s:"
	pickItemPfx   = "e:"
)

// PickSeason and PickItem return menu values for one season or one target.
func PickSeason(season int) string { return pickSeasonPfx + strconv.Itoa(season) }
func PickItem(t Target) string     { return pickItemPfx + strconv.Itoa(t.ItemID) }

// Select returns the candidates the picked menu values cover, in candidate
// order and without duplicates.
func Select(cands []Target, values []string) []Target {
	all := false
	seasons := map[int]bool{}
	items := map[int]bool{}
	for _, v := range values {
		switch {
		case v == PickAll:
			all = true
		case strings.HasPrefix(v, pickSeasonPfx):
			if n, err := strconv.Atoi(v[len(pickSeasonPfx):]); err == nil {
				seasons[n] = true
			}
		case strings.HasPrefix(v, pickItemPfx):
			if n, err := strconv.Atoi(v[len(pickItemPfx):]); err == nil {
				items[n] = true
			}
		}
	}
	var out []Target
	for _, t := range cands {
		if all || seasons[t.Season] || items[t.ItemID] {
			out = append(out, t)
		}
	}
	return withSharedFiles(cands, out)
}

// withSharedFiles returns chosen plus every target in all that shares a
// file with one of them, in the order of all.
func withSharedFiles(all, chosen []Target) []Target {
	files := map[int]bool{}
	items := map[int]bool{}
	for _, t := range chosen {
		if t.FileID != 0 {
			files[t.FileID] = true
		}
		items[t.ItemID] = true
	}
	var out []Target
	for _, t := range all {
		if items[t.ItemID] || files[t.FileID] {
			out = append(out, t)
		}
	}
	return out
}

// SharedFile says, for each target, which earlier target it shares a file
// with: that target's label, or "" for the first target of each file.
func SharedFile(targets []Target) []string {
	first := map[int]string{}
	out := make([]string, len(targets))
	for i, t := range targets {
		if label, ok := first[t.FileID]; ok {
			out[i] = label
			continue
		}
		first[t.FileID] = t.Label
	}
	return out
}

// Season is one season among the candidates, with its number of files.
type Season struct {
	Number int
	Files  int
}

// Seasons groups candidates by season, in order.
func Seasons(cands []Target) []Season {
	var out []Season
	for _, t := range cands {
		if n := len(out); n > 0 && out[n-1].Number == t.Season {
			out[n-1].Files++
			continue
		}
		out = append(out, Season{Number: t.Season, Files: 1})
	}
	return out
}

// Result is what happened to one target.
type Result struct {
	Target Target

	// Blocklisted is the release marked failed, if any. BlocklistNote
	// says why nothing was blocklisted, or that the release was already
	// blocklisted for an earlier target in this run.
	Blocklisted   string
	BlocklistNote string
	BlocklistErr  error

	DeleteErr   error
	AlreadyGone bool   // the file was gone before the delete, for example by an earlier replace
	SameFileAs  string // label of the earlier target whose file this one shared
	SearchErr   error  // set when the search for this target failed
	Searched    bool
}

// Deleted reports whether the file was deleted.
func (r Result) Deleted() bool { return r.DeleteErr == nil }

// Run replaces each target: blocklist, then delete, for every file, then
// one search for every file that was deleted. A failed blocklist doesn't
// stop the delete; a failed delete means no search for that file.
//
// Targets sharing a file are handled once, with the first of them: the
// file is blocklisted and deleted once, and every episode it held is
// searched for.
func (s Service) Run(ctx context.Context, targets []Target) []Result {
	results := make([]Result, len(targets))
	marked := map[string]bool{} // download IDs already marked failed
	byFile := map[int]int{}     // file ID → index of the result that handled it
	var search []int
	for i, t := range targets {
		r := Result{Target: t}
		if j, ok := byFile[t.FileID]; ok {
			first := results[j]
			r.SameFileAs = first.Target.Label
			r.DeleteErr, r.AlreadyGone = first.DeleteErr, first.AlreadyGone
		} else {
			byFile[t.FileID] = i
			s.blocklist(ctx, t, &r, marked)
			r.DeleteErr = s.delete(ctx, t)
			if arr.IsNotFound(r.DeleteErr) {
				r.DeleteErr, r.AlreadyGone = nil, true
			}
		}
		if r.Deleted() {
			search = append(search, t.ItemID)
		}
		results[i] = r
	}
	if len(search) == 0 {
		return results
	}
	var err error
	switch targets[0].Kind {
	case Movie:
		err = s.Radarr.SearchMovies(ctx, search)
	case Episode:
		err = s.Sonarr.SearchEpisodes(ctx, search)
	}
	for i := range results {
		if results[i].Deleted() {
			results[i].Searched = err == nil
			results[i].SearchErr = err
		}
	}
	return results
}

func (s Service) blocklist(ctx context.Context, t Target, r *Result, marked map[string]bool) {
	var (
		hist []arr.HistoryRecord
		err  error
	)
	switch t.Kind {
	case Movie:
		hist, err = s.Radarr.History(ctx, t.ItemID)
	case Episode:
		hist, err = s.Sonarr.History(ctx, t.ItemID)
	}
	if err != nil {
		r.BlocklistErr = err
		return
	}
	g, ok := arr.ReleaseGrab(hist)
	if !ok {
		r.BlocklistNote = "no grab in history"
		return
	}
	if g.DownloadID != "" && marked[g.DownloadID] {
		r.BlocklistNote = "same release as above, already blocklisted"
		return
	}
	switch t.Kind {
	case Movie:
		err = s.Radarr.MarkFailed(ctx, g.ID)
	case Episode:
		err = s.Sonarr.MarkFailed(ctx, g.ID)
	}
	if err != nil {
		r.BlocklistErr = err
		return
	}
	r.Blocklisted = g.SourceTitle
	if g.DownloadID != "" {
		marked[g.DownloadID] = true
	}
}

func (s Service) delete(ctx context.Context, t Target) error {
	switch t.Kind {
	case Movie:
		return s.Radarr.DeleteMovieFile(ctx, t.FileID)
	case Episode:
		return s.Sonarr.DeleteEpisodeFile(ctx, t.FileID)
	}
	return errors.New("unknown target kind")
}

// Summary describes one result in a line, for the issue post.
// Summary describes one result in a line, for the issue post. Errors are
// given in a few words (arr.Brief); log Err for the details.
func (r Result) Summary() string {
	var parts []string
	switch {
	case r.SameFileAs != "":
		parts = append(parts, "same file as "+r.SameFileAs)
	case r.Blocklisted != "":
		parts = append(parts, "blocklisted "+r.Blocklisted)
	case r.BlocklistErr != nil:
		parts = append(parts, "not blocklisted ("+arr.Brief(r.BlocklistErr)+")")
	case strings.HasPrefix(r.BlocklistNote, "same release"):
		parts = append(parts, r.BlocklistNote)
	default:
		parts = append(parts, "not blocklisted ("+r.BlocklistNote+")")
	}
	switch {
	case r.DeleteErr != nil:
		parts = append(parts, "delete failed ("+arr.Brief(r.DeleteErr)+")", "not searched")
	case r.AlreadyGone:
		parts = append(parts, "file was already gone")
	default:
		parts = append(parts, "deleted")
	}
	if r.Deleted() {
		if r.Searched {
			parts = append(parts, "search started")
		} else if r.SearchErr != nil {
			parts = append(parts, "search failed ("+arr.Brief(r.SearchErr)+")")
		}
	}
	return r.Target.Label + ": " + strings.Join(parts, " · ")
}

// Err joins the errors behind a result, with full details, for the log.
// Errors shared from an earlier target of the same file aren't repeated.
func (r Result) Err() error {
	if r.SameFileAs != "" {
		return r.SearchErr
	}
	return errors.Join(r.BlocklistErr, r.DeleteErr, r.SearchErr)
}

// EpisodeCode formats an episode as S01E02.
func EpisodeCode(season, episode int) string {
	return fmt.Sprintf("S%02dE%02d", season, episode)
}

func withYear(title string, year int) string {
	if year > 0 {
		return fmt.Sprintf("%s (%d)", title, year)
	}
	return title
}
