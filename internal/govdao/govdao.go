// Package govdao reads GovDAO proposal state from a gno.land node and extracts
// the on-chain proposal id emitted when a proposal is created.
//
// It mirrors gnomonitoring's approach (backend/internal/govdao): the id comes
// from the `ProposalCreated` event (attr key "id"); the status is derived from
// the realm render (`vm/qrender` on `<realm>:<id>`). All access is over plain
// HTTP RPC so katana stays free of the heavy gnolang/gno dependency.
package govdao

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Katana lifecycle status values (mirror store.Status*).
const (
	Accepted = "accepted"
	Pending  = "pending"
	Rejected = "rejected"
	Unknown  = "unknown"
)

// Client queries a single gno.land RPC endpoint for GovDAO state.
type Client struct {
	rpcURL string
	realm  string
	http   *http.Client
}

// New builds a Client for the given RPC endpoint and GovDAO realm path.
func New(rpcURL, realm string) *Client {
	return &Client{
		rpcURL: strings.TrimRight(rpcURL, "/"),
		realm:  realm,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

type abciQueryResponse struct {
	Result struct {
		Response struct {
			Value string `json:"value"` // base64 of the rendered bytes
			Log   string `json:"log"`
		} `json:"response"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Status renders the proposal and maps it to a katana lifecycle status.
//
// Mapping is render-string based and realm-version dependent. An unrecognised
// render returns Unknown, which the caller must treat as still-pending — it must
// never be promoted to Rejected (see PRD FR-VT.2b).
func (c *Client) Status(ctx context.Context, proposalID string) (string, error) {
	data := fmt.Sprintf("%s:%s", c.realm, proposalID)

	q := url.Values{}
	q.Set("path", `"vm/qrender"`)
	q.Set("data", "0x"+hex.EncodeToString([]byte(data)))

	var body abciQueryResponse
	if err := c.getJSON(ctx, "/abci_query", q, &body); err != nil {
		return Unknown, err
	}
	if body.Error != nil {
		return Unknown, fmt.Errorf("RPC error: %s", body.Error.Message)
	}
	raw, err := base64.StdEncoding.DecodeString(body.Result.Response.Value)
	if err != nil {
		return Unknown, fmt.Errorf("decoding render: %w", err)
	}
	return classify(string(raw)), nil
}

// classify maps a rendered GovDAO proposal page to a lifecycle status.
func classify(render string) string {
	switch {
	case strings.Contains(render, "ACCEPTED"):
		return Accepted
	case strings.Contains(render, "REJECTED"):
		return Rejected
	case strings.Contains(render, "ACTIVE"),
		strings.Contains(render, "PROPOSED"),
		strings.Contains(render, "Vote YES"):
		return Pending
	default:
		return Unknown
	}
}

// proposalCreatedID matches an on-chain `ProposalCreated` event and captures the
// numeric id attribute from gnokey/RPC broadcast output.
var proposalCreatedID = regexp.MustCompile(`ProposalCreated[\s\S]{0,400}?\bid\b\D{0,10}(\d+)`)

// ExtractProposalID best-effort parses the GovDAO proposal id from the textual
// output of a broadcast (gnokey). Returns "" if not found; the caller should
// then fall back to the GraphQL indexer.
func ExtractProposalID(broadcastOutput string) string {
	// First try a structured events array if gnokey printed JSON.
	if id := extractFromJSON(broadcastOutput); id != "" {
		return id
	}
	if m := proposalCreatedID.FindStringSubmatch(broadcastOutput); len(m) == 2 {
		return m[1]
	}
	return ""
}

func extractFromJSON(out string) string {
	type attr struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	type event struct {
		Type  string `json:"type"`
		Attrs []attr `json:"attrs"`
	}
	// Scan every '[' … ']' slice and try to decode an events array.
	for i := 0; i < len(out); i++ {
		if out[i] != '[' {
			continue
		}
		for j := len(out); j > i; j-- {
			if out[j-1] != ']' {
				continue
			}
			var evs []event
			if err := json.Unmarshal([]byte(out[i:j]), &evs); err == nil {
				for _, e := range evs {
					if e.Type != "ProposalCreated" {
						continue
					}
					for _, a := range e.Attrs {
						if a.Key == "id" {
							return a.Value
						}
					}
				}
			}
			break
		}
	}
	return ""
}

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
