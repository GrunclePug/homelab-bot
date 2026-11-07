package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Radarr is a Radarr v3 API client.
type Radarr struct {
	c client
}

// NewRadarr returns a client for the Radarr at baseURL.
func NewRadarr(baseURL, apiKey string) *Radarr {
	return &Radarr{c: newClient("radarr", baseURL, apiKey)}
}

// Movie is a Radarr movie.
type Movie struct {
	ID        int        `json:"id"`
	Title     string     `json:"title"`
	Year      int        `json:"year"`
	TMDBID    int        `json:"tmdbId"`
	HasFile   bool       `json:"hasFile"`
	MovieFile *MovieFile `json:"movieFile"`
}

// MovieFile is a Radarr movie file.
type MovieFile struct {
	ID           int     `json:"id"`
	MovieID      int     `json:"movieId"`
	RelativePath string  `json:"relativePath"`
	Size         int64   `json:"size"`
	ReleaseGroup string  `json:"releaseGroup"`
	Quality      Quality `json:"quality"`
}

// MovieByTMDB finds a movie by its TMDB ID. ok is false if Radarr doesn't
// have it.
func (r *Radarr) MovieByTMDB(ctx context.Context, tmdbID int) (Movie, bool, error) {
	var list []Movie
	if err := r.c.do(ctx, http.MethodGet, "/api/v3/movie?tmdbId="+strconv.Itoa(tmdbID), nil, &list); err != nil {
		return Movie{}, false, fmt.Errorf("radarr: find movie tmdb %d: %w", tmdbID, err)
	}
	for _, m := range list {
		if m.TMDBID == tmdbID {
			return m, true, nil
		}
	}
	return Movie{}, false, nil
}

// History returns a movie's history, newest first.
func (r *Radarr) History(ctx context.Context, movieID int) ([]HistoryRecord, error) {
	q := url.Values{
		"movieIds":      {strconv.Itoa(movieID)},
		"pageSize":      {strconv.Itoa(historyPageSize)},
		"sortKey":       {"date"},
		"sortDirection": {"descending"},
	}
	var page historyPage
	if err := r.c.do(ctx, http.MethodGet, "/api/v3/history?"+q.Encode(), nil, &page); err != nil {
		return nil, fmt.Errorf("radarr: history of movie %d: %w", movieID, err)
	}
	return page.Records, nil
}

// MarkFailed marks a grab failed, which blocklists its release.
func (r *Radarr) MarkFailed(ctx context.Context, historyID int) error {
	return r.c.markFailed(ctx, historyID)
}

// DeleteMovieFile deletes a movie file.
func (r *Radarr) DeleteMovieFile(ctx context.Context, fileID int) error {
	if err := r.c.do(ctx, http.MethodDelete, "/api/v3/moviefile/"+strconv.Itoa(fileID), nil, nil); err != nil {
		return fmt.Errorf("radarr: delete movie file %d: %w", fileID, err)
	}
	return nil
}

// SearchMovies starts a search for the given movies.
func (r *Radarr) SearchMovies(ctx context.Context, movieIDs []int) error {
	return r.c.command(ctx, map[string]any{"name": "MoviesSearch", "movieIds": movieIDs})
}
