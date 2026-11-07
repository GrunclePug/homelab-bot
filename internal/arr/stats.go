package arr

import (
	"context"
	"fmt"
	"net/http"
)

// HealthCheck is a problem Sonarr or Radarr reports about itself, such as
// an indexer that keeps failing.
type HealthCheck struct {
	Source  string `json:"source"`
	Type    string `json:"type"` // ok, notice, warning or error
	Message string `json:"message"`
}

// Disk is a volume the app can see.
type Disk struct {
	Path       string `json:"path"`
	FreeSpace  int64  `json:"freeSpace"`
	TotalSpace int64  `json:"totalSpace"`
}

// Stats is a summary of the app for the status card.
type Stats struct {
	Version string
	Health  []HealthCheck
	Queue   int // items in the download queue
	Disks   []Disk
}

// stats reads the app's version, health, queue size and disks.
func (c client) stats(ctx context.Context) (Stats, error) {
	var (
		s      Stats
		status struct {
			Version string `json:"version"`
		}
		queue struct {
			TotalCount int `json:"totalCount"`
		}
	)
	for _, r := range []struct {
		path string
		out  any
	}{
		{"/api/v3/system/status", &status},
		{"/api/v3/health", &s.Health},
		{"/api/v3/queue/status", &queue},
		{"/api/v3/diskspace", &s.Disks},
	} {
		if err := c.do(ctx, http.MethodGet, r.path, nil, r.out); err != nil {
			return Stats{}, fmt.Errorf("%s: read status: %w", c.name, err)
		}
	}
	s.Version = status.Version
	s.Queue = queue.TotalCount
	return s, nil
}

// Stats summarises Sonarr for the status card.
func (s *Sonarr) Stats(ctx context.Context) (Stats, error) { return s.c.stats(ctx) }

// Stats summarises Radarr for the status card.
func (r *Radarr) Stats(ctx context.Context) (Stats, error) { return r.c.stats(ctx) }
