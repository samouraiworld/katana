// Package gnomonitoring is a thin HTTP client for the gnomonitoring REST API.
//
// katana does not compute validator metrics itself; it reads them from a running
// gnomonitoring instance (https://github.com/samouraiworld/gnomonitoring). Two
// signals are consumed:
//
//   - missed blocks per validator over a reporting window (/missing_block)
//   - CRITICAL alerts per validator over a rolling window (/admin/alerts)
package gnomonitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Client talks to a gnomonitoring backend.
type Client struct {
	baseURL    string
	token      string
	http       *http.Client
	missedPath string
	alertsPath string
}

// New builds a Client from the gnomonitoring section of the config.
func New(cfg config.Gnomonitoring) *Client {
	return &Client{
		baseURL:    cfg.BaseURL,
		token:      cfg.APIToken,
		missedPath: cfg.MissedBlocksPath,
		alertsPath: cfg.AlertsPath,
		http:       &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
	}
}

// MissedBlock mirrors gnomonitoring's MissingBlockMetrics JSON payload.
type MissedBlock struct {
	Moniker      string `json:"moniker"`
	Addr         string `json:"addr"`
	MissingBlock int    `json:"missingBlock"`
}

// Alert mirrors the subset of gnomonitoring's AlertLog JSON payload katana uses.
type Alert struct {
	ChainID string    `json:"chain_id"`
	Addr    string    `json:"addr"`
	Moniker string    `json:"moniker"`
	Level   string    `json:"level"`
	Msg     string    `json:"msg"`
	SentAt  time.Time `json:"sent_at"`
}

// MissedBlocks returns, per validator, the number of blocks missed on chain
// during the given gnomonitoring period (e.g. "current_week").
func (c *Client) MissedBlocks(ctx context.Context, chain, period string) ([]MissedBlock, error) {
	q := url.Values{}
	q.Set("chain", chain)
	q.Set("period", period)

	var out []MissedBlock
	if err := c.getJSON(ctx, c.missedPath, q, &out); err != nil {
		return nil, fmt.Errorf("fetching missed blocks: %w", err)
	}
	return out, nil
}

// CriticalAlertCounts returns the number of CRITICAL alerts per validator address
// on chain since the given time.
//
// The alerts endpoint is admin-protected in gnomonitoring; set
// gnomonitoring.api_token in config.yaml (or KATANA_GNOMON_API_TOKEN) so the
// request carries a bearer token.
func (c *Client) CriticalAlertCounts(ctx context.Context, chain string, since time.Time) (map[string]int, error) {
	q := url.Values{}
	q.Set("chain", chain)
	q.Set("level", "CRITICAL")
	q.Set("from", since.UTC().Format(time.RFC3339))
	q.Set("limit", strconv.Itoa(10000))

	var alerts []Alert
	if err := c.getJSON(ctx, c.alertsPath, q, &alerts); err != nil {
		return nil, fmt.Errorf("fetching critical alerts: %w", err)
	}

	counts := make(map[string]int, len(alerts))
	for _, a := range alerts {
		counts[a.Addr]++
	}
	return counts, nil
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
