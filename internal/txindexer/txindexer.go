// Package txindexer queries a gno tx-indexer (GraphQL) to recover proposals
// katana broadcast without learning their outcome.
//
// When gnokey fails after sending a tx (`broadcasting bytes: ...`, an RPC
// timeout), the tx may have landed but katana has neither a tx hash nor a
// GovDAO id, and the row stays `unconfirmed`. ProposalsByCaller lists the
// signer's successful txs that created a GovDAO proposal since the broadcast
// height; Match picks the one whose payload hashes to what katana submitted
// (RunPayloadHash / CallPayloadHash, recorded at broadcast time).
package txindexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/samouraiworld/katana/internal/govdao"
)

// GovDAORealm is the realm whose ProposalCreated event carries the id.
const GovDAORealm = "gno.land/r/gov/dao"

// proposalsByCallerQuery is the query verified against the gnoland-test
// devnet tx-indexer (copy of e2e/testdata/indexer_proposals_by_caller.graphql).
//
//go:embed proposals_by_caller.graphql
var proposalsByCallerQuery string

// maxResponseBytes caps the indexer response read into memory.
const maxResponseBytes = 16 << 20

// Client talks to a tx-indexer GraphQL endpoint (e.g.
// http://indexer:8546/graphql/query).
type Client struct {
	url  string
	http *http.Client
}

// New returns a Client for the GraphQL endpoint url. A nil httpClient gets a
// 15s timeout.
func New(url string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{url: url, http: httpClient}
}

// File is one file of a MsgRun package, as submitted.
type File struct {
	Name string
	Body string
}

// Msg is one tx message. Run fields are set for a MsgRun, call fields for a
// MsgCall; other message types only carry TypeURL/Route.
type Msg struct {
	TypeURL string // "run" | "call" | ...
	Route   string // "vm"
	Caller  string

	Files []File // MsgRun package files

	PkgPath string   // MsgCall
	Func    string   // MsgCall
	Args    []string // MsgCall
}

// Tx is one indexed transaction returned by ProposalsByCaller.
type Tx struct {
	// Hash is the base64 (std encoding) SHA-256 of the tx bytes: the same
	// text gnokey prints as `TX HASH:` and katana stores in tx_hash, so it
	// is stored as is and resolves with govdao's RPC /tx lookup.
	Hash        string
	BlockHeight int64
	Index       int
	Messages    []Msg
	// ProposalID is the id attr of the tx's single ProposalCreated event
	// from GovDAORealm; "" when there is none or more than one
	// (govdao.ProposalIDFromEvents semantics).
	ProposalID string
}

// Wire types of the getTransactions response.
type gqlTx struct {
	Hash        string `json:"hash"`
	BlockHeight int64  `json:"block_height"`
	Index       int    `json:"index"`
	Messages    []struct {
		TypeURL string `json:"typeUrl"`
		Route   string `json:"route"`
		Value   struct {
			Caller  string `json:"caller"`
			Package *struct {
				Files []struct {
					Name string `json:"name"`
					Body string `json:"body"`
				} `json:"files"`
			} `json:"package"`
			PkgPath string   `json:"pkg_path"`
			Func    string   `json:"func"`
			Args    []string `json:"args"`
		} `json:"value"`
	} `json:"messages"`
	Response struct {
		// Non-GnoEvent entries (storage deposits) come back as {}.
		Events json.RawMessage `json:"events"`
	} `json:"response"`
}

type gqlResponse struct {
	Data *struct {
		// null (not []) when nothing matches.
		GetTransactions []gqlTx `json:"getTransactions"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ProposalsByCaller returns the successful txs signed by caller at a block
// height >= minHeight that emitted a ProposalCreated event from GovDAORealm,
// in indexer order. An empty result is (nil, nil). Any GraphQL error, even
// alongside partial data, is returned as an error. minHeight must be >= 1:
// an unknown broadcast height (0) would scan the signer's whole history.
func (c *Client) ProposalsByCaller(ctx context.Context, caller string, minHeight int64) ([]Tx, error) {
	if caller == "" {
		return nil, errors.New("txindexer: empty caller")
	}
	if minHeight < 1 {
		return nil, fmt.Errorf("txindexer: min height %d: broadcast height unknown", minHeight)
	}
	reqBody, err := json.Marshal(map[string]any{
		"query": proposalsByCallerQuery,
		// FilterInt has only eq/gt/lt: "> minHeight-1" is ">= minHeight".
		"variables": map[string]any{"caller": caller, "afterHeight": minHeight - 1},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("txindexer: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("txindexer: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("txindexer: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("txindexer: HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var out gqlResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("txindexer: decoding response: %w", err)
	}
	if len(out.Errors) > 0 {
		msgs := make([]string, len(out.Errors))
		for i, e := range out.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("txindexer: graphql errors: %s", strings.Join(msgs, "; "))
	}
	if out.Data == nil {
		return nil, errors.New("txindexer: response has no data")
	}

	var txs []Tx
	for _, g := range out.Data.GetTransactions {
		tx := Tx{Hash: g.Hash, BlockHeight: g.BlockHeight, Index: g.Index}
		for _, m := range g.Messages {
			msg := Msg{
				TypeURL: m.TypeURL, Route: m.Route, Caller: m.Value.Caller,
				PkgPath: m.Value.PkgPath, Func: m.Value.Func, Args: m.Value.Args,
			}
			if m.Value.Package != nil {
				for _, f := range m.Value.Package.Files {
					msg.Files = append(msg.Files, File{Name: f.Name, Body: f.Body})
				}
			}
			tx.Messages = append(tx.Messages, msg)
		}
		tx.ProposalID = govdao.ProposalIDFromEvents(g.Response.Events, GovDAORealm)
		txs = append(txs, tx)
	}
	return txs, nil
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// Payload hashing. The canonical payload is what katana hands gnokey, field
// by field, each field length-prefixed (8-byte big-endian length, then the
// bytes) after a mode tag, so no two distinct payloads share an encoding:
//
//	run:  "katana/run/v1",  body
//	call: "katana/call/v1", pkg_path, func, args[0], ..., args[n-1]
//
// The run body is the script exactly as written to the temp .gno file
// (Built.Script, byte for byte, trailing newline included); the file name is
// not part of the payload (katana's is a random temp name). The hash is the
// lowercase hex SHA-256 of that encoding.

// RunPayloadHash hashes a run-mode submission: the single .gno file body
// katana writes, exactly as submitted (proposal.Built.Script).
func RunPayloadHash(body string) string {
	return hashFields("katana/run/v1", body)
}

// CallPayloadHash hashes a call-mode submission: the MsgCall pkg_path, func
// and args (proposal.Built.RealmPath, Function, Args). nil and empty args
// hash the same.
func CallPayloadHash(pkgPath, fn string, args []string) string {
	return hashFields("katana/call/v1", append([]string{pkgPath, fn}, args...)...)
}

func hashFields(tag string, fields ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, f := range append([]string{tag}, fields...) {
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PayloadHash returns the canonical payload hash of a single-message tx: a
// MsgRun whose package holds exactly one .gno file (other files, such as a
// gnomod.toml a newer gnokey may add, are ignored) or a MsgCall. It returns
// "" for anything else, which never matches: katana submits exactly one
// message per proposal tx.
func (t Tx) PayloadHash() string {
	if len(t.Messages) != 1 {
		return ""
	}
	m := t.Messages[0]
	switch m.TypeURL {
	case "run":
		var bodies []string
		for _, f := range m.Files {
			if strings.HasSuffix(f.Name, ".gno") {
				bodies = append(bodies, f.Body)
			}
		}
		if len(bodies) != 1 {
			return ""
		}
		return RunPayloadHash(bodies[0])
	case "call":
		return CallPayloadHash(m.PkgPath, m.Func, m.Args)
	default:
		return ""
	}
}

// Match returns the single tx in txs that carries a GovDAO proposal id and
// whose payload hashes to payloadHash. An empty payloadHash (unknown, e.g. a
// row written before payload hashes were recorded) never matches.
//
// It fails closed: if two or more txs match it returns false. That means the
// same payload was submitted twice since the broadcast height (e.g. an
// identical re-proposal after a rejection, or a manual resubmission of the
// rendered command), and the hash alone cannot tell which tx belongs to the
// row; attaching the wrong id would track another proposal's vote. Callers
// should drop txs whose hash is already recorded on another row first.
func Match(txs []Tx, payloadHash string) (Tx, bool) {
	if payloadHash == "" {
		return Tx{}, false
	}
	var found []Tx
	for _, tx := range txs {
		if tx.ProposalID != "" && tx.PayloadHash() == payloadHash {
			found = append(found, tx)
		}
	}
	if len(found) != 1 {
		return Tx{}, false
	}
	return found[0], true
}
