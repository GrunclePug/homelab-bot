package seerr

import (
	"context"
	"fmt"
	"net/http"
)

// Stats is a summary of Seerr for the status card.
type Stats struct {
	Version         string
	UpdateAvailable bool
	Pending         int // requests waiting for approval
	Processing      int // approved requests not yet available
	OpenIssues      int
}

// Stats reads Seerr's version and its request and issue counts.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var (
		status struct {
			Version         string `json:"version"`
			UpdateAvailable bool   `json:"updateAvailable"`
		}
		requests struct {
			Pending    int `json:"pending"`
			Processing int `json:"processing"`
		}
		issues struct {
			Open int `json:"open"`
		}
	)
	for _, r := range []struct {
		path string
		out  any
	}{
		{"/api/v1/status", &status},
		{"/api/v1/request/count", &requests},
		{"/api/v1/issue/count", &issues},
	} {
		if err := c.do(ctx, http.MethodGet, r.path, nil, r.out); err != nil {
			return Stats{}, fmt.Errorf("seerr: read status: %w", err)
		}
	}
	return Stats{
		Version:         status.Version,
		UpdateAvailable: status.UpdateAvailable,
		Pending:         requests.Pending,
		Processing:      requests.Processing,
		OpenIssues:      issues.Open,
	}, nil
}
