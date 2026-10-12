// Package gnoland provides the minimal on-chain queries katana needs against a
// gno.land (Tendermint2) RPC node. It uses the node's REST-style RPC endpoints
// directly over HTTP so katana stays free of the heavy gnolang/gno dependency.
package gnoland

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Validator is a single entry of the consensus validator set.
type Validator struct {
	Address     string
	VotingPower int64
}

// Client queries a single gno.land RPC endpoint.
type Client struct {
	rpcURL string
	http   *http.Client
}

// New builds a Client for the given RPC endpoint.
func New(rpcURL string) *Client {
	return &Client{
		rpcURL: strings.TrimRight(rpcURL, "/"),
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

// tmValidatorsResponse models the /validators RPC response.
type tmValidatorsResponse struct {
	Result struct {
		BlockHeight string `json:"block_height"`
		Validators  []struct {
			Address     string `json:"address"`
			VotingPower string `json:"voting_power"`
		} `json:"validators"`
		Total string `json:"total"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

// Validators returns the current consensus validator set, following pagination
// until every entry has been collected.
func (c *Client) Validators(ctx context.Context) ([]Validator, error) {
	const perPage = 100
	var (
		all  []Validator
		page = 1
	)

	for {
		q := url.Values{}
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))

		var body tmValidatorsResponse
		if err := c.getJSON(ctx, "/validators", q, &body); err != nil {
			return nil, err
		}
		if body.Error != nil {
			return nil, fmt.Errorf("RPC error: %s (%s)", body.Error.Message, body.Error.Data)
		}

		// Fail closed: an unreadable entry must never read as a 0 voting
		// power or a smaller valset (both feed the VP targets and the
		// min_valset_size guardrail).
		for _, v := range body.Result.Validators {
			if v.Address == "" {
				return nil, fmt.Errorf("validator with an empty address on page %d", page)
			}
			power, err := strconv.ParseInt(v.VotingPower, 10, 64)
			if err != nil || power <= 0 {
				return nil, fmt.Errorf("validator %s: invalid voting_power %q", v.Address, v.VotingPower)
			}
			all = append(all, Validator{Address: v.Address, VotingPower: power})
		}

		total, err := strconv.Atoi(body.Result.Total)
		if err != nil || total < 0 {
			return nil, fmt.Errorf("invalid valset total %q", body.Result.Total)
		}
		if len(all) >= total {
			break
		}
		if len(body.Result.Validators) == 0 {
			return nil, fmt.Errorf("valset truncated: %d of %d validators returned", len(all), total)
		}
		page++
	}
	return all, nil
}

// IsValidator reports whether address is a member of the current consensus
// validator set.
func (c *Client) IsValidator(ctx context.Context, address string) (bool, error) {
	vals, err := c.Validators(ctx)
	if err != nil {
		return false, err
	}
	for _, v := range vals {
		if strings.EqualFold(v.Address, address) {
			return true, nil
		}
	}
	return false, nil
}

// getJSON performs a GET against the RPC endpoint and decodes the JSON body.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := c.rpcURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
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
