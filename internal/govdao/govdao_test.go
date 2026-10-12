package govdao

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestStatusParsesResponseBaseData guards the RPC-parsing seam: gno's abci_query
// nests the render under result.response.ResponseBase.Data (base64), not a flat
// "value" field. Reading the wrong field yields an empty render → ParseRender →
// Unknown, which silently keeps every proposal pending forever.
func TestStatusParsesResponseBaseData(t *testing.T) {
	render := "## Prop #1\n### Stats\n- **PROPOSAL HAS BEEN ACCEPTED**\n"
	data := base64.StdEncoding.EncodeToString([]byte(render))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"response":{"ResponseBase":{"Data":"` + data + `","Error":null}}}}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "gno.land/r/gov/dao").Proposal(context.Background(), "1")
	if err != nil {
		t.Fatalf("Proposal: %v", err)
	}
	if got.Status != Accepted {
		t.Fatalf("Status = %q, want %q", got.Status, Accepted)
	}
}

// TestParseRender runs the parser over real GovDAO proposal pages rendered by
// r/gov/dao/impl/v0 at the gno.land mainnet tag (chain/mainnet, 9c8eb132e483).
// The fixtures in testdata/render are verbatim gno.land/r/gov/dao:<id> renders
// produced by the realm itself (see testdata/gen for the generating filetests);
// they are byte-identical to the earlier test13 (r/gov/dao/v3/impl) renders.
func TestParseRender(t *testing.T) {
	tests := []struct {
		fixture  string
		status   string
		yes, no  float64
		hasTally bool
	}{
		{"accepted", Accepted, 100, 0, true},
		{"denied", Rejected, 0, 100, true},
		// Denied on execution failure: YES won, the executor errored, and the
		// realm appended a REASON line right under the marker.
		{"denied_reason", Rejected, 100, 0, true},
		// Title and description mention ACCEPTED/REJECTED and "Vote YES"; the
		// action bar renders vote links on every page. None of it is a status.
		{"open_desc_accepted", Pending, 0, 0, true},
		{"open_supermajority_yes", Pending, 100, 0, true},
		{"open_two_thirds_yes", Pending, 66.66666666666666, 0, true},
		{"open_split", Pending, 33.33333333333333, 33.33333333333333, true},
		// The description forges an accepted marker line. Two marker lines are
		// ambiguous: the parser must refuse to pick one.
		{"open_forged_marker", Unknown, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "render", tt.fixture+".md"))
			if err != nil {
				t.Fatal(err)
			}
			got := ParseRender(string(raw))
			if got.Status != tt.status {
				t.Errorf("Status = %q, want %q", got.Status, tt.status)
			}
			if got.HasTally != tt.hasTally || got.YesPercent != tt.yes || got.NoPercent != tt.no {
				t.Errorf("tally = (%v, %v, %v), want (%v, %v, %v)",
					got.HasTally, got.YesPercent, got.NoPercent, tt.hasTally, tt.yes, tt.no)
			}
		})
	}
}

func TestParseRenderUnrecognised(t *testing.T) {
	for _, render := range []string{
		"",
		"# Proposal not found\n\nProposal 9 does not exist.",
		"Status: ACCEPTED",
		"Status: REJECTED",
		// Marker text embedded in a longer line is not a marker line.
		"Validator x - **PROPOSAL HAS BEEN ACCEPTED** by moniker",
	} {
		if got := ParseRender(render); got.Status != Unknown {
			t.Errorf("ParseRender(%q).Status = %q, want %q", render, got.Status, Unknown)
		}
	}
}

func TestParseRenderAmbiguousTally(t *testing.T) {
	render := "### Stats\n- **Proposal is open for votes**\n" +
		"- YES PERCENT: 100%\n- YES PERCENT: 0%\n- NO PERCENT: 0%\n"
	got := ParseRender(render)
	if got.Status != Pending {
		t.Fatalf("Status = %q, want %q", got.Status, Pending)
	}
	if got.HasTally {
		t.Fatalf("HasTally = true for duplicated YES lines, want false")
	}
}

func TestExecutable(t *testing.T) {
	tests := []struct {
		name string
		p    Proposal
		want bool
	}{
		{"yes supermajority", Proposal{Status: Pending, YesPercent: 66.66666666666666, HasTally: true}, true},
		{"no supermajority", Proposal{Status: Pending, NoPercent: 70, HasTally: true}, true},
		{"below threshold", Proposal{Status: Pending, YesPercent: 66.6, NoPercent: 33.4, HasTally: true}, false},
		{"already accepted", Proposal{Status: Accepted, YesPercent: 100, HasTally: true}, false},
		{"already denied", Proposal{Status: Rejected, NoPercent: 100, HasTally: true}, false},
		{"unknown", Proposal{Status: Unknown, YesPercent: 100, HasTally: true}, false},
		{"no tally", Proposal{Status: Pending}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Executable(); got != tt.want {
				t.Errorf("Executable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProposalIDFromEvents(t *testing.T) {
	const realm = "gno.land/r/gov/dao"
	tests := []struct {
		name   string
		events string
		want   string
	}{
		{
			name:   "gnokey EVENTS line",
			events: `[{"type":"ProposalCreated","attrs":[{"key":"id","value":"7"}],"pkg_path":"gno.land/r/gov/dao"}]`,
			want:   "7",
		},
		{
			name:   "amino JSON from RPC /tx",
			events: `[{"@type":"/tm.GnoEvent","type":"StorageDeposit","attrs":[]},{"@type":"/tm.GnoEvent","type":"ProposalCreated","attrs":[{"key":"id","value":"12"}],"pkg_path":"gno.land/r/gov/dao"}]`,
			want:   "12",
		},
		{
			name:   "storage events carry no type",
			events: `[{"bytes_delta":120,"fee_delta":{"denom":"ugnot","amount":12000},"pkg_path":"gno.land/r/gov/dao"},{"type":"ProposalCreated","attrs":[{"key":"id","value":"3"}],"pkg_path":"gno.land/r/gov/dao"}]`,
			want:   "3",
		},
		{
			name:   "event from another realm is ignored",
			events: `[{"type":"ProposalCreated","attrs":[{"key":"id","value":"9"}],"pkg_path":"gno.land/r/evil"}]`,
			want:   "",
		},
		{
			name:   "two proposals in one tx is ambiguous",
			events: `[{"type":"ProposalCreated","attrs":[{"key":"id","value":"1"}],"pkg_path":"gno.land/r/gov/dao"},{"type":"ProposalCreated","attrs":[{"key":"id","value":"2"}],"pkg_path":"gno.land/r/gov/dao"}]`,
			want:   "",
		},
		{
			name:   "non-numeric id is rejected",
			events: `[{"type":"ProposalCreated","attrs":[{"key":"id","value":"7; rm"}],"pkg_path":"gno.land/r/gov/dao"}]`,
			want:   "",
		},
		{name: "empty", events: `[]`, want: ""},
		{name: "not json", events: `ProposalCreated id 42`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProposalIDFromEvents([]byte(tt.events), realm); got != tt.want {
				t.Errorf("ProposalIDFromEvents() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTxProposalID(t *testing.T) {
	// tx hash "q2Vh" base64 = 0xab6561
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"","result":{"hash":"q2Vh","height":"42","index":0,` +
			`"tx_result":{"ResponseBase":{"Error":null,"Data":null,"Events":[` +
			`{"@type":"/tm.GnoEvent","type":"ProposalCreated","attrs":[{"key":"id","value":"5"}],"pkg_path":"gno.land/r/gov/dao"}` +
			`],"Log":"","Info":""},"GasWanted":"1","GasUsed":"1"},"tx":"AAAA"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "gno.land/r/gov/dao")
	id, err := c.TxProposalID(context.Background(), "q2Vh")
	if err != nil {
		t.Fatalf("TxProposalID: %v", err)
	}
	if id != "5" {
		t.Fatalf("id = %q, want 5", id)
	}
	if gotQuery != "/tx?hash=0xab6561" {
		t.Fatalf("query = %q, want /tx?hash=0xab6561", gotQuery)
	}
}

func TestTxProposalIDNotCommitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"","error":{"code":-32603,"message":"Internal error","data":"tx (AB6561) not found"}}`))
	}))
	defer srv.Close()

	id, err := New(srv.URL, "gno.land/r/gov/dao").TxProposalID(context.Background(), "q2Vh")
	if err == nil || id != "" {
		t.Fatalf("TxProposalID = %q, %v; want an error", id, err)
	}
}
