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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Windows are the reporting windows gnomonitoring always reports for every
// validator (it fills a window the validator has no data for with a clean
// score rather than omitting it).
var Windows = []string{"last_24h", "current_week", "current_month", "current_year"}

// ErrMalformedReport is wrapped by FetchReports when the response does not
// have the expected shape. katana fails closed on it: a missing or null score
// must never decode to 0, which would read as "eject".
var ErrMalformedReport = errors.New("malformed gnomonitoring report")

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
// chain's valset. It fails closed: any malformed entry rejects the whole
// response with an error wrapping ErrMalformedReport (entries are never
// dropped, since callers compute valset-wide voting power shares).
func (c *Client) FetchReports(ctx context.Context, chain string) ([]Report, error) {
	q := url.Values{}
	q.Set("chain", chain)

	var raw []*rawReport
	if err := c.getJSON(ctx, c.reportsPath, q, &raw); err != nil {
		return nil, fmt.Errorf("fetching validator reports: %w", err)
	}
	out, err := convertReports(raw)
	if err != nil {
		return nil, fmt.Errorf("fetching validator reports: %w", err)
	}
	return out, nil
}

// rawPeriod mirrors PeriodScore with pointer fields for the values katana
// acts on, so a missing or null one is told apart from an explicit 0.
type rawPeriod struct {
	Score               *int     `json:"score"`
	Tier                string   `json:"tier"`
	SignRate            float64  `json:"sign_rate"`
	ProposerReliability *float64 `json:"proposer_reliability"`
	VotingPower         *int64   `json:"voting_power"`
	CriticalCount       int      `json:"critical_count"`
	WarningCount        int      `json:"warning_count"`
	IncidentCount       int      `json:"incident_count"`
	IncidentRatePerWeek float64  `json:"incident_rate_per_week"`
	DowntimeBlocks      int64    `json:"downtime_blocks"`
	MissedBlocks        int64    `json:"missed_blocks"`
}

type rawReport struct {
	Addr               string                `json:"addr"`
	Moniker            string                `json:"moniker"`
	DaysSinceLastAlert *int                  `json:"days_since_last_alert"`
	Periods            map[string]*rawPeriod `json:"periods"`
}

// convertReports validates the decoded response and converts it to Reports.
func convertReports(raw []*rawReport) ([]Report, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: response is null, want a JSON array", ErrMalformedReport)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: no validators in response", ErrMalformedReport)
	}

	seen := make(map[string]bool, len(raw))
	out := make([]Report, 0, len(raw))
	for i, r := range raw {
		if r == nil {
			return nil, fmt.Errorf("%w: entry %d is null", ErrMalformedReport, i)
		}
		if strings.TrimSpace(r.Addr) == "" {
			return nil, fmt.Errorf("%w: entry %d has no addr", ErrMalformedReport, i)
		}
		key := strings.ToLower(r.Addr)
		if seen[key] {
			return nil, fmt.Errorf("%w: duplicate addr %s", ErrMalformedReport, r.Addr)
		}
		seen[key] = true
		for _, w := range Windows {
			if _, ok := r.Periods[w]; !ok {
				return nil, fmt.Errorf("%w: %s: missing window %s", ErrMalformedReport, r.Addr, w)
			}
		}

		rep := Report{
			Addr:               r.Addr,
			Moniker:            r.Moniker,
			DaysSinceLastAlert: r.DaysSinceLastAlert,
			Periods:            make(map[string]PeriodScore, len(r.Periods)),
		}
		// Every window present is validated, not only Windows: a converted
		// PeriodScore with a defaulted score would be just as dangerous.
		for w, p := range r.Periods {
			ps, err := convertPeriod(p)
			if err != nil {
				return nil, fmt.Errorf("%w: %s: window %s: %v", ErrMalformedReport, r.Addr, w, err)
			}
			rep.Periods[w] = ps
		}
		out = append(out, rep)
	}
	return out, nil
}

// convertPeriod checks the fields katana acts on (score, voting_power) are
// present and in range; the others stay lenient.
func convertPeriod(p *rawPeriod) (PeriodScore, error) {
	switch {
	case p == nil:
		return PeriodScore{}, errors.New("period is null")
	case p.Score == nil:
		return PeriodScore{}, errors.New("missing score")
	case *p.Score < 0 || *p.Score > 100:
		return PeriodScore{}, fmt.Errorf("score %d out of [0,100]", *p.Score)
	case p.VotingPower == nil:
		return PeriodScore{}, errors.New("missing voting_power")
	case *p.VotingPower < 0:
		return PeriodScore{}, fmt.Errorf("negative voting_power %d", *p.VotingPower)
	}
	return PeriodScore{
		Score:               *p.Score,
		Tier:                p.Tier,
		SignRate:            p.SignRate,
		ProposerReliability: p.ProposerReliability,
		VotingPower:         *p.VotingPower,
		CriticalCount:       p.CriticalCount,
		WarningCount:        p.WarningCount,
		IncidentCount:       p.IncidentCount,
		IncidentRatePerWeek: p.IncidentRatePerWeek,
		DowntimeBlocks:      p.DowntimeBlocks,
		MissedBlocks:        p.MissedBlocks,
	}, nil
}

// getJSON performs a GET against path?query and decodes the JSON body into out.
// A body that does not decode into out wraps ErrMalformedReport.
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
		return fmt.Errorf("%w: decoding response from %s: %w", ErrMalformedReport, endpoint, err)
	}
	return nil
}
