// Package gnomonitoring is a thin HTTP client for the gnomonitoring REST API.
//
// katana does not compute validator metrics itself; it reads a computed
// per-validator health score from a running gnomonitoring instance
// (https://github.com/samouraiworld/gnomonitoring), via
// GET /api/reports/validators?chain=<chain> — a public endpoint listing every
// validator currently in the chain's valset, with score/tier/sub-metrics over
// four rolling/calendar windows.
package gnomonitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Client talks to a gnomonitoring backend.
type Client struct {
	baseURL     string
	token       string
	http        *http.Client
	reportsPath string
}

// New builds a Client from the gnomonitoring section of the config.
func New(cfg config.Gnomonitoring) *Client {
	return &Client{
		baseURL:     cfg.BaseURL,
		token:       cfg.APIToken,
		reportsPath: cfg.ReportsPath,
		http:        &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
	}
}

// PeriodScore is one validator's health metrics over one reporting window
// (last_24h | current_week | current_month | current_year). ProposerReliability
// is nil when the validator's expected proposal count is too low to be a
// meaningful signal (dropped from gnomonitoring's own score in that case).
type PeriodScore struct {
	Score               int      `json:"score"`
	Tier                string   `json:"tier"`
	SignRate            float64  `json:"sign_rate"`
	ProposerReliability *float64 `json:"proposer_reliability"`
	VotingPower         int64    `json:"voting_power"`
	CriticalCount       int      `json:"critical_count"`
	WarningCount        int      `json:"warning_count"`
	IncidentCount       int      `json:"incident_count"`
	IncidentRatePerWeek float64  `json:"incident_rate_per_week"`
	DowntimeBlocks      int64    `json:"downtime_blocks"`
	MissedBlocks        int64    `json:"missed_blocks"`
}

// Report is one validator's entry in gnomonitoring's per-validator health
// report. A validator that has left the valset is excluded entirely by
// gnomonitoring, so every entry here is a live candidate.
type Report struct {
	Addr               string                 `json:"addr"`
	Moniker            string                 `json:"moniker"`
	DaysSinceLastAlert *int                   `json:"days_since_last_alert"`
	Periods            map[string]PeriodScore `json:"periods"`
}

// FetchReports returns the health report for every validator currently in
// chain's valset.
func (c *Client) FetchReports(ctx context.Context, chain string) ([]Report, error) {
	q := url.Values{}
	q.Set("chain", chain)

	var out []Report
	if err := c.getJSON(ctx, c.reportsPath, q, &out); err != nil {
		return nil, fmt.Errorf("fetching validator reports: %w", err)
	}
	return out, nil
}

// getJSON performs a GET against path?query and decodes the JSON body into out.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", endpoint, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", endpoint, err)
	}
	return nil
}
