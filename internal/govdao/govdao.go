// Package govdao reads GovDAO proposal state from a gno.land node and extracts
// the on-chain proposal id emitted when a proposal is created.
//
// It mirrors gnomonitoring's approach (backend/internal/govdao): the id comes
// from the `ProposalCreated` event (attr key "id"); the status is derived from
// the realm render (`vm/qrender` on `<realm>:<id>`). When gnokey's output does
// not carry the event, the id is read back from the committed tx (RPC /tx). All access is over plain
// HTTP RPC so katana stays free of the heavy gnolang/gno dependency.
package govdao

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
			// gno's RPC abci_query nests the result under ResponseBase.Data
			// (base64 of the rendered bytes), not a flat "value" field.
			ResponseBase struct {
				Data  string `json:"Data"`
				Error *struct {
					Message string `json:"message"`
				} `json:"Error"`
			} `json:"ResponseBase"`
		} `json:"response"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Supermajority mirrors the GovDAO implementation's law.Supermajority
// (gno.land/r/gov/dao/impl/v0 on the chain/mainnet tag, r/gov/dao/v3/impl on
// test13; 66.66 on both): the YES or NO share a proposal needs before an
// ExecuteProposal call resolves it.
const Supermajority = 66.66

// Marker lines emitted by the GovDAO implementation's proposalStatus.String()
// (r/gov/dao/impl/v0 on mainnet, r/gov/dao/v3/impl on test13; identical lines,
// both also emit an "- ABSTAIN PERCENT: " line that katana ignores) on a
// single-proposal page. Exactly one of them is present per render.
const (
	markerAccepted = "- **PROPOSAL HAS BEEN ACCEPTED**"
	markerDenied   = "- **PROPOSAL HAS BEEN DENIED**"
	markerOpen     = "- **Proposal is open for votes**"

	tallyYes = "- YES PERCENT: "
	tallyNo  = "- NO PERCENT: "
)

// Proposal is what katana reads from a single-proposal render.
type Proposal struct {
	Status string
	// YesPercent and NoPercent are the realm's vote tallies, only meaningful
	// when HasTally is true.
	YesPercent, NoPercent float64
	HasTally              bool
}

// Executable reports whether an ExecuteProposal call would resolve the
// proposal: it is still open and one side holds a supermajority. Below that
// threshold the realm panics and the call only burns gas.
func (p Proposal) Executable() bool {
	return p.Status == Pending && p.HasTally &&
		(p.YesPercent >= Supermajority || p.NoPercent >= Supermajority)
}

// Proposal renders the proposal and parses its status and vote tallies.
//
// An unrecognised render yields Unknown, which the caller must treat as
// still-pending — it must never be promoted to Rejected (see PRD FR-VT.2b).
func (c *Client) Proposal(ctx context.Context, proposalID string) (Proposal, error) {
	data := fmt.Sprintf("%s:%s", c.realm, proposalID)

	q := url.Values{}
	q.Set("path", `"vm/qrender"`)
	q.Set("data", "0x"+hex.EncodeToString([]byte(data)))

	unknown := Proposal{Status: Unknown}
	var body abciQueryResponse
	if err := c.getJSON(ctx, "/abci_query", q, &body); err != nil {
		return unknown, err
	}
	if body.Error != nil {
		return unknown, fmt.Errorf("RPC error: %s", body.Error.Message)
	}
	if e := body.Result.Response.ResponseBase.Error; e != nil {
		return unknown, fmt.Errorf("abci error: %s", e.Message)
	}
	raw, err := base64.StdEncoding.DecodeString(body.Result.Response.ResponseBase.Data)
	if err != nil {
		return unknown, fmt.Errorf("decoding render: %w", err)
	}
	return ParseRender(string(raw)), nil
}

// ParseRender reads the status and vote tallies out of a GovDAO
// single-proposal page (r/gov/dao/impl/v0 on mainnet, v3/impl on test13).
// A proposal created by a previous implementation renders a "not available"
// page with no marker: that yields Unknown, like any unrecognised render.
//
// Only whole marker lines count. Anything looser is unsafe: the page embeds
// the proposal's own title and description (which may say "ACCEPTED", or carry
// a validator-controlled moniker), and an action bar with "Vote YES"/"Vote NO"
// links that the realm renders on every page, decided or not.
//
// The description is not escaped, so it can also forge a whole marker line.
// The realm writes exactly one marker per page: a render with any other count
// is ambiguous and yields Unknown rather than a guess. The same rule applies to
// the tally lines.
func ParseRender(render string) Proposal {
	var statuses []string
	var yes, no []string
	for _, line := range strings.Split(render, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == markerAccepted:
			statuses = append(statuses, Accepted)
		case line == markerDenied:
			statuses = append(statuses, Rejected)
		case line == markerOpen:
			statuses = append(statuses, Pending)
		case strings.HasPrefix(line, tallyYes):
			yes = append(yes, line[len(tallyYes):])
		case strings.HasPrefix(line, tallyNo):
			no = append(no, line[len(tallyNo):])
		}
	}

	p := Proposal{Status: Unknown}
	if len(statuses) == 1 {
		p.Status = statuses[0]
	}
	if len(yes) == 1 && len(no) == 1 {
		y, yerr := parsePercent(yes[0])
		n, nerr := parsePercent(no[0])
		if yerr == nil && nerr == nil {
			p.YesPercent, p.NoPercent, p.HasTally = y, n, true
		}
	}
	return p
}

// parsePercent parses a realm tally value such as "66.66666666666666%".
func parsePercent(s string) (float64, error) {
	v, ok := strings.CutSuffix(s, "%")
	if !ok {
		return 0, fmt.Errorf("tally %q: missing %%", s)
	}
	return strconv.ParseFloat(v, 64)
}

// ProposalIDFromEvents returns the GovDAO proposal id carried by a tx's
// events: the "id" attribute of the single ProposalCreated event emitted by
// realm. It accepts both gnokey's EVENTS line and the amino JSON of RPC /tx.
//
// It returns "" unless exactly one such event with a numeric id is present:
// katana creates one proposal per tx, so anything else is not a tx it can
// attribute safely.
func ProposalIDFromEvents(events []byte, realm string) string {
	var evs []struct {
		Type  string `json:"type"`
		Attrs []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"attrs"`
		PkgPath string `json:"pkg_path"`
	}
	if err := json.Unmarshal(events, &evs); err != nil {
		return ""
	}
	var ids []string
	for _, e := range evs {
		if e.Type != "ProposalCreated" || e.PkgPath != realm {
			continue
		}
		for _, a := range e.Attrs {
			if a.Key == "id" {
				ids = append(ids, a.Value)
			}
		}
	}
	if len(ids) != 1 {
		return ""
	}
	if _, err := strconv.ParseUint(ids[0], 10, 64); err != nil {
		return ""
	}
	return ids[0]
}

type txResponse struct {
	Result *struct {
		TxResult json.RawMessage `json:"tx_result"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

// ErrTxNotFound: the node has no committed tx for the hash (not included yet,
// or never). An expected state for an ambiguous broadcast, not an RPC failure.
var ErrTxNotFound = errors.New("tx not found")

// ErrNoProposalCreated: the tx is committed but carries no single
// ProposalCreated event from the realm (e.g. it failed in DeliverTx).
var ErrNoProposalCreated = errors.New("no single ProposalCreated event")

// isTxNotFound matches the RPC error data of a missing tx: tm2's
// state.NoTxResultForHashError on the chain/mainnet tag ("Could not find tx
// result for hash #<HEX>"), and the older "tx (<HEX>) not found".
func isTxNotFound(data string) bool {
	return strings.HasPrefix(data, "Could not find tx result for hash") ||
		(strings.HasPrefix(data, "tx (") && strings.HasSuffix(data, ") not found"))
}

// TxProposalID looks a committed tx up by its base64 hash (as gnokey prints
// it) via RPC /tx and returns the proposal id from its events. A tx that is
// not committed yet is ErrTxNotFound, a committed one without the event is
// ErrNoProposalCreated; any other error is an RPC or response failure.
func (c *Client) TxProposalID(ctx context.Context, txHash string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(txHash)
	if err != nil {
		return "", fmt.Errorf("decoding tx hash %q: %w", txHash, err)
	}
	q := url.Values{}
	q.Set("hash", "0x"+hex.EncodeToString(raw))

	var body txResponse
	if err := c.getJSON(ctx, "/tx", q, &body); err != nil {
		return "", err
	}
	if body.Error != nil {
		if isTxNotFound(body.Error.Data) {
			return "", fmt.Errorf("tx %s: %w", txHash, ErrTxNotFound)
		}
		return "", fmt.Errorf("RPC error: %s %s", body.Error.Message, body.Error.Data)
	}
	if body.Result == nil {
		return "", fmt.Errorf("tx %s: empty result", txHash)
	}
	events := findEvents(body.Result.TxResult)
	if events == nil {
		return "", fmt.Errorf("tx %s: no events in result", txHash)
	}
	id := ProposalIDFromEvents(events, c.realm)
	if id == "" {
		return "", fmt.Errorf("tx %s: %w from %s", txHash, ErrNoProposalCreated, c.realm)
	}
	return id, nil
}

// findEvents returns the first "events" array (any letter case) in a
// DeliverTx result. The amino JSON nests it under ResponseBase; searching
// keeps katana independent of that layout.
func findEvents(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	for k, v := range obj {
		if strings.EqualFold(k, "events") && len(v) > 0 && v[0] == '[' {
			return v
		}
	}
	for _, v := range obj {
		if len(v) > 0 && v[0] == '{' {
			if ev := findEvents(v); ev != nil {
				return ev
			}
		}
	}
	return nil
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
