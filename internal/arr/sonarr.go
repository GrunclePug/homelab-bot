package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Sonarr is a Sonarr v3 API client.
type Sonarr struct {
	c client
}

// NewSonarr returns a client for the Sonarr at baseURL.
func NewSonarr(baseURL, apiKey string) *Sonarr {
	return &Sonarr{c: newClient("sonarr", baseURL, apiKey)}
}

// Series is a Sonarr series.
type Series struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Year   int    `json:"year"`
	TVDBID int    `json:"tvdbId"`
}

// Episode is a Sonarr episode.
type Episode struct {
	ID            int    `json:"id"`
	SeriesID      int    `json:"seriesId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Title         string `json:"title"`
	HasFile       bool   `json:"hasFile"`
	EpisodeFileID int    `json:"episodeFileId"`
}

// EpisodeFile is a Sonarr episode file.
type EpisodeFile struct {
	ID           int     `json:"id"`
	SeriesID     int     `json:"seriesId"`
	SeasonNumber int     `json:"seasonNumber"`
	RelativePath string  `json:"relativePath"`
	Size         int64   `json:"size"`
	ReleaseGroup string  `json:"releaseGroup"`
	Quality      Quality `json:"quality"`
}

// SeriesByTVDB finds a series by its TVDB ID. ok is false if Sonarr doesn't
// have it.
func (s *Sonarr) SeriesByTVDB(ctx context.Context, tvdbID int) (Series, bool, error) {
	var list []Series
	if err := s.c.do(ctx, http.MethodGet, "/api/v3/series?tvdbId="+strconv.Itoa(tvdbID), nil, &list); err != nil {
		return Series{}, false, fmt.Errorf("sonarr: find series tvdb %d: %w", tvdbID, err)
	}
	for _, se := range list {
		if se.TVDBID == tvdbID {
			return se, true, nil
		}
	}
	return Series{}, false, nil
}

// Episodes lists a series' episodes.
func (s *Sonarr) Episodes(ctx context.Context, seriesID int) ([]Episode, error) {
	var list []Episode
	if err := s.c.do(ctx, http.MethodGet, "/api/v3/episode?seriesId="+strconv.Itoa(seriesID), nil, &list); err != nil {
		return nil, fmt.Errorf("sonarr: list episodes of series %d: %w", seriesID, err)
	}
	return list, nil
}

// EpisodeFiles lists a series' episode files.
func (s *Sonarr) EpisodeFiles(ctx context.Context, seriesID int) ([]EpisodeFile, error) {
	var list []EpisodeFile
	if err := s.c.do(ctx, http.MethodGet, "/api/v3/episodefile?seriesId="+strconv.Itoa(seriesID), nil, &list); err != nil {
		return nil, fmt.Errorf("sonarr: list episode files of series %d: %w", seriesID, err)
	}
	return list, nil
}

// History returns an episode's history, newest first.
func (s *Sonarr) History(ctx context.Context, episodeID int) ([]HistoryRecord, error) {
	q := url.Values{
		"episodeId":     {strconv.Itoa(episodeID)},
		"pageSize":      {strconv.Itoa(historyPageSize)},
		"sortKey":       {"date"},
		"sortDirection": {"descending"},
	}
	var page historyPage
	if err := s.c.do(ctx, http.MethodGet, "/api/v3/history?"+q.Encode(), nil, &page); err != nil {
		return nil, fmt.Errorf("sonarr: history of episode %d: %w", episodeID, err)
	}
	return page.Records, nil
}

// MarkFailed marks a grab failed, which blocklists its release.
func (s *Sonarr) MarkFailed(ctx context.Context, historyID int) error {
	return s.c.markFailed(ctx, historyID)
}

// DeleteEpisodeFile deletes an episode file.
func (s *Sonarr) DeleteEpisodeFile(ctx context.Context, fileID int) error {
	if err := s.c.do(ctx, http.MethodDelete, "/api/v3/episodefile/"+strconv.Itoa(fileID), nil, nil); err != nil {
		return fmt.Errorf("sonarr: delete episode file %d: %w", fileID, err)
	}
	return nil
}

// SearchEpisodes starts a search for the given episodes.
func (s *Sonarr) SearchEpisodes(ctx context.Context, episodeIDs []int) error {
	return s.c.command(ctx, map[string]any{"name": "EpisodeSearch", "episodeIds": episodeIDs})
}
