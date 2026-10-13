//go:build e2e

// Package e2e is katana's opt-in end-to-end suite (PRD §12.5, PP-6). It is
// guarded by the `e2e` build tag so `go test ./...` never builds it.
//
// Two layers:
//
//   - fixtures_test.go (this file): katana's parsers run against REAL
//     artefacts captured from the gnoland-test devnet (testdata/). No network.
//     go test -tags e2e ./e2e -run Fixture
//   - lifecycle_test.go: katana runs as a process against the live devnet and
//     the suite asserts the proposals.status column (rejected, expired,
//     unconfirmed, accepted). See ../gnoland-test/katana-e2e.sh.
//
// Fixture provenance (gnoland-test devnet built from the gno.land mainnet tag
// chain/mainnet = 9c8eb132e483, 2026-10-12; gnokey and chain image
// gno-validator:mainnet built from that same commit):
//
//	gnokey_run_proposal_ok.{stdout,stderr,exitcode}  maketx run of katana's DefaultEjectScript (GovDAO #1)
//	gnokey_run_proposal_ok_combined.txt              same, 2>&1 as katana captures it (GovDAO #2)
//	gnokey_call_execute_or_reject_{accept,deny}.txt  ExecuteOrRejectProposal after a 100% YES / NO vote
//	gnokey_call_execute_or_reject_executor_panic.txt ExecuteOrRejectProposal whose executor panics (validator already removed)
//	gnokey_call_fail_simulation.txt                  call that fails in simulation (default -simulate test)
//	gnokey_call_fail_delivertx.txt                   same call with -simulate skip: committed, DeliverTx failed
//	gnokey_call_fail_checktx_unknown_address.txt     unfunded signer with -simulate skip: CheckTx failed
//	gnokey_run_fail_simulation.txt                   eject script on a non-valoper address (simulation)
//	gnokey_run_fail_broadcast_transport.txt          broadcast_tx_commit response dropped by a proxy (tx WAS committed, GovDAO #3)
//	rpc_tx_proposal_created.json                     RPC /tx?hash=0x.. of GovDAO #1's tx
//	rpc_tx_delivertx_failed.json                     RPC /tx?hash=0x.. of the DeliverTx-failed tx
//	rpc_validators.json                              RPC /validators with the 4 validators (no pagination, no total)
//	abci_qrender_open.json                           raw abci_query vm/qrender response for GovDAO #1 while open
//	render_{open,open_yes_supermajority,accepted,open_no_supermajority,denied}.md  decoded renders
//	indexer_proposals_by_caller.{graphql,request.json,response.json}  tx-indexer query (see lifecycle_test.go)
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/gnoland"
	"github.com/samouraiworld/katana/internal/govdao"
	"github.com/samouraiworld/katana/internal/proposal"
)

const realm = "gno.land/r/gov/dao"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(b)
}

// stripExit drops the "exit=<n>" trailer the capture appended.
func stripExit(s string) string {
	if i := strings.LastIndex(s, "\nexit="); i >= 0 {
		return s[:i+1]
	}
	return s
}

func TestFixtureBroadcastSuccess(t *testing.T) {
	cases := []struct {
		file, wantID string
		wantHeight   int64
	}{
		{"gnokey_run_proposal_ok.stdout", "1", 7},
		{"gnokey_run_proposal_ok_combined.txt", "2", 8},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			out := fixture(t, c.file)
			r := proposal.ParseTxOutput(out)
			if !r.OK || r.Height != c.wantHeight || r.TxHash == "" || r.Events == "" {
				t.Fatalf("ParseTxOutput = %+v", r)
			}
			if got := govdao.ProposalIDFromEvents([]byte(r.Events), realm); got != c.wantID {
				t.Fatalf("ProposalIDFromEvents = %q, want %q", got, c.wantID)
			}
			if o := proposal.Classify(out, nil); o != proposal.Committed {
				t.Fatalf("Classify = %s", o)
			}
		})
	}
}

func TestFixtureExecuteOrRejectOutput(t *testing.T) {
	for _, f := range []string{"gnokey_call_execute_or_reject_accept.txt", "gnokey_call_execute_or_reject_deny.txt"} {
		r := proposal.ParseTxOutput(fixture(t, f))
		if !r.OK || r.Height == 0 || r.TxHash == "" {
			t.Errorf("%s: ParseTxOutput = %+v", f, r)
		}
		// A MsgCall result line ("(true bool)" / "(false bool)") precedes OK!;
		// the deny call returns false yet the tx succeeds.
	}
}

// gnokey exits non-zero on every failure below; Broadcast turns that into an
// *exec.ExitError-wrapping error, which is what Classify sees.
var errExit = errors.New("gnokey: exit status 1")

func TestFixtureBroadcastFailures(t *testing.T) {
	cases := []struct {
		file        string
		want        proposal.Outcome
		wantTxHash  bool
		wantHeight  bool
		description string
	}{
		{"gnokey_call_fail_simulation.txt", proposal.Failed, false, false, "simulation: TX HASH line printed but empty, HEIGHT 0"},
		// The mainnet gnokey prints GAS/HEIGHT/EVENTS for a DeliverTx failure
		// too: the tx IS in a block (real height, empty events), never OK!.
		{"gnokey_call_fail_delivertx.txt", proposal.Failed, true, true, "DeliverTx failed: committed with a hash and a height, no OK!"},
		{"gnokey_call_fail_checktx_unknown_address.txt", proposal.Failed, false, false, "CheckTx failed: no TX HASH line at all"},
		{"gnokey_run_fail_simulation.txt", proposal.Failed, false, false, "run script panics in simulation"},
		{"gnokey_call_execute_or_reject_executor_panic.txt", proposal.Failed, false, false, "executor panic reverts ExecuteOrRejectProposal"},
		{"gnokey_run_fail_broadcast_transport.txt", proposal.Ambiguous, false, false, "response dropped after send: no TX HASH line"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			out := stripExit(fixture(t, c.file))
			r := proposal.ParseTxOutput(out)
			if r.OK || (r.Height != 0) != c.wantHeight {
				t.Errorf("ParseTxOutput = %+v, want !OK and wantHeight=%v (%s)", r, c.wantHeight, c.description)
			}
			if (r.TxHash != "") != c.wantTxHash {
				t.Errorf("TxHash = %q, wantTxHash=%v (%s)", r.TxHash, c.wantTxHash, c.description)
			}
			if o := proposal.Classify(out, errExit); o != c.want {
				t.Errorf("Classify = %s, want %s (%s)", o, c.want, c.description)
			}
		})
	}
}

func serveFile(t *testing.T, name string) *httptest.Server {
	t.Helper()
	body := fixture(t, name)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFixtureRPCTx(t *testing.T) {
	ctx := context.Background()

	srv := serveFile(t, "rpc_tx_proposal_created.json")
	id, err := govdao.New(srv.URL, realm).TxProposalID(ctx, "KKUJRcbwg7obrJEwkJJbvOP3MdwSt30RGFAhBgCkaJE=")
	if err != nil || id != "1" {
		t.Fatalf("TxProposalID = %q, %v; want 1", id, err)
	}

	// A committed-but-failed tx carries "Events": null and an Error: no id.
	srv = serveFile(t, "rpc_tx_delivertx_failed.json")
	if id, err := govdao.New(srv.URL, realm).TxProposalID(ctx, "fw09HZp+zWGO4NpT0vIdf8ItRKlMBn8q4vr1nwT+KGk="); !errors.Is(err, govdao.ErrNoProposalCreated) {
		t.Fatalf("TxProposalID on a failed tx = %q, %v; want ErrNoProposalCreated", id, err)
	}
}

func TestFixtureRenders(t *testing.T) {
	cases := []struct {
		file       string
		status     string
		yes, no    float64
		executable bool
	}{
		{"render_open.md", govdao.Pending, 0, 0, false},
		{"render_open_yes_supermajority.md", govdao.Pending, 100, 0, true},
		{"render_accepted.md", govdao.Accepted, 100, 0, false},
		{"render_open_no_supermajority.md", govdao.Pending, 0, 100, true},
		{"render_denied.md", govdao.Rejected, 0, 100, false},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			p := govdao.ParseRender(fixture(t, c.file))
			if p.Status != c.status || !p.HasTally || p.YesPercent != c.yes || p.NoPercent != c.no {
				t.Fatalf("ParseRender = %+v", p)
			}
			if p.Executable() != c.executable {
				t.Fatalf("Executable = %v, want %v", p.Executable(), c.executable)
			}
		})
	}

	srv := serveFile(t, "abci_qrender_open.json")
	p, err := govdao.New(srv.URL, realm).Proposal(context.Background(), "1")
	if err != nil || p.Status != govdao.Pending {
		t.Fatalf("Proposal over abci_query fixture = %+v, %v", p, err)
	}
}

// indexerTx is the subset of the tx-indexer getTransactions response the
// recovery query returns (see testdata/indexer_proposals_by_caller.graphql).
type indexerTx struct {
	Hash        string `json:"hash"`
	BlockHeight int64  `json:"block_height"`
	Messages    []struct {
		TypeURL string `json:"typeUrl"`
		Route   string `json:"route"`
		Value   struct {
			Caller  string `json:"caller"`
			PkgPath string `json:"pkg_path"`
			Func    string `json:"func"`
			Args    []string
			Package *struct {
				Files []struct {
					Name string `json:"name"`
					Body string `json:"body"`
				} `json:"files"`
			} `json:"package"`
		} `json:"value"`
	} `json:"messages"`
	Response struct {
		// Non-GnoEvent entries (storage deposits) come back as {}.
		Events json.RawMessage `json:"events"`
	} `json:"response"`
}

type indexerResponse struct {
	Data struct {
		// null (not []) when nothing matches.
		GetTransactions []indexerTx `json:"getTransactions"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func TestFixtureIndexerResponse(t *testing.T) {
	var resp indexerResponse
	if err := json.Unmarshal([]byte(fixture(t, "indexer_proposals_by_caller.response.json")), &resp); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tx := range resp.Data.GetTransactions {
		if len(tx.Messages) != 1 || tx.Messages[0].TypeURL != "run" || tx.Messages[0].Value.Package == nil {
			t.Fatalf("unexpected message shape: %+v", tx.Messages)
		}
		if !strings.Contains(tx.Messages[0].Value.Package.Files[0].Body, "NewValoperChange") {
			t.Fatalf("MsgRun body does not carry the eject script")
		}
		// The indexer events array uses the same type/attrs/pkg_path keys as
		// gnokey's EVENTS line, so katana's parser applies unchanged.
		ids = append(ids, govdao.ProposalIDFromEvents(tx.Response.Events, realm))
	}
	if strings.Join(ids, ",") != "1,2,3" {
		t.Fatalf("ids = %v, want [1 2 3]", ids)
	}
}

// tm2's /validators has no pagination and no total: the whole valset comes
// back in one response.
func TestFixtureRPCValidators(t *testing.T) {
	srv := serveFile(t, "rpc_validators.json")
	vs, err := gnoland.New(srv.URL).Validators(context.Background())
	if err != nil {
		t.Fatalf("Validators: %v", err)
	}
	if len(vs) != 4 {
		t.Fatalf("got %d validators, want 4: %+v", len(vs), vs)
	}
	var total int64
	for _, v := range vs {
		if !strings.HasPrefix(v.Address, "g1") || v.VotingPower <= 0 {
			t.Fatalf("validator %+v", v)
		}
		total += v.VotingPower
	}
	if total != 31 {
		t.Fatalf("total voting power = %d, want 31 (10+10+10+1)", total)
	}
}
