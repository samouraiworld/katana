package txindexer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
)

const (
	caller = "g1jq93n9pw2qxkq4mks89yenwmzlkgjapktcudek"
	realm  = "gno.land/r/gov/dao"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve answers every POST with status and body, recording the request body.
func serve(t *testing.T, status int, body []byte, got *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s %q, want POST application/json", r.Method, r.Header.Get("Content-Type"))
		}
		if got != nil {
			*got, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProposalsByCallerFixture(t *testing.T) {
	var reqBody []byte
	srv := serve(t, http.StatusOK, fixture(t, "indexer_proposals_by_caller.response.json"), &reqBody)

	txs, err := New(srv.URL, realm, nil).ProposalsByCaller(context.Background(), caller, 7)
	if err != nil {
		t.Fatalf("ProposalsByCaller: %v", err)
	}

	// The request is the verified one: same query text, afterHeight =
	// minHeight-1 (FilterInt has no gte).
	type gqlReq struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	var got, want gqlReq
	if err := json.Unmarshal(reqBody, &got); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if err := json.Unmarshal(fixture(t, "indexer_proposals_by_caller.request.json"), &want); err != nil {
		t.Fatal(err)
	}
	if got.Query != want.Query {
		t.Fatalf("query differs from the verified fixture:\n%s", got.Query)
	}
	if got.Variables["caller"] != caller || got.Variables["afterHeight"] != float64(6) || got.Variables["realm"] != realm {
		t.Fatalf("variables = %v, want caller %s afterHeight 6 realm %s", got.Variables, caller, realm)
	}

	if len(txs) != 3 {
		t.Fatalf("got %d txs, want 3", len(txs))
	}
	wantTx := []struct {
		hash   string
		height int64
		id     string
		file   string
	}{
		{"KKUJRcbwg7obrJEwkJJbvOP3MdwSt30RGFAhBgCkaJE=", 7, "1", "eject1.gno"},
		{"j6iKhwYrUpu1zZw4AEwHRyPvx9HrPqghSZAEUTuAR8Q=", 8, "2", "eject2.gno"},
		{"x/3en1gygPnR103c136yFFJT2Z/E7et3pSUNEb+cpxk=", 9, "3", "eject3.gno"},
	}
	for i, w := range wantTx {
		tx := txs[i]
		if tx.Hash != w.hash || tx.BlockHeight != w.height || tx.ProposalID != w.id {
			t.Fatalf("tx %d = hash %q height %d id %q", i, tx.Hash, tx.BlockHeight, tx.ProposalID)
		}
		if len(tx.Messages) != 1 {
			t.Fatalf("tx %d: %d messages", i, len(tx.Messages))
		}
		m := tx.Messages[0]
		if m.TypeURL != "run" || m.Route != "vm" || m.Caller != caller || len(m.Files) != 1 || m.Files[0].Name != w.file {
			t.Fatalf("tx %d message = %+v", i, m)
		}
		if !strings.HasPrefix(m.Files[0].Body, "package main\n") || !strings.HasSuffix(m.Files[0].Body, "}\n") {
			t.Fatalf("tx %d body not returned verbatim: %q", i, m.Files[0].Body)
		}
	}
}

// The indexer's hash is the same text gnokey prints as TX HASH (base64 of the
// tx's SHA-256), which is what katana stores in proposals.tx_hash.
func TestHashMatchesGnokeyTxHash(t *testing.T) {
	srv := serve(t, http.StatusOK, fixture(t, "indexer_proposals_by_caller.response.json"), nil)
	txs, err := New(srv.URL, realm, nil).ProposalsByCaller(context.Background(), caller, 7)
	if err != nil {
		t.Fatal(err)
	}
	var printed string
	for _, line := range strings.Split(string(fixture(t, "gnokey_run_proposal_ok.stdout")), "\n") {
		if v, ok := strings.CutPrefix(line, "TX HASH:"); ok {
			printed = strings.TrimSpace(v)
		}
	}
	if printed == "" || txs[0].Hash != printed {
		t.Fatalf("indexer hash %q != gnokey TX HASH %q", txs[0].Hash, printed)
	}
}

// A custom GovDAO realm is both the query's event filter and the realm the
// proposal id is read from: the fixture's gno.land/r/gov/dao events then
// carry no id.
func TestProposalsByCallerUsesConfiguredRealm(t *testing.T) {
	const other = "gno.land/r/gov/dao/custom"
	var reqBody []byte
	srv := serve(t, http.StatusOK, fixture(t, "indexer_proposals_by_caller.response.json"), &reqBody)
	txs, err := New(srv.URL, other, nil).ProposalsByCaller(context.Background(), caller, 7)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(reqBody, &got); err != nil {
		t.Fatal(err)
	}
	if got.Variables["realm"] != other {
		t.Fatalf("realm variable = %v, want %s", got.Variables["realm"], other)
	}
	for i, tx := range txs {
		if tx.ProposalID != "" {
			t.Fatalf("tx %d: id %q read from a realm other than %s", i, tx.ProposalID, other)
		}
	}
}

func TestProposalsByCallerEmpty(t *testing.T) {
	for _, body := range []string{
		`{"data":{"getTransactions":null}}`,
		`{"data":{"getTransactions":[]}}`,
	} {
		srv := serve(t, http.StatusOK, []byte(body), nil)
		txs, err := New(srv.URL, realm, nil).ProposalsByCaller(context.Background(), caller, 1)
		if err != nil || len(txs) != 0 {
			t.Fatalf("%s: txs %+v, err %v", body, txs, err)
		}
	}
}

func TestProposalsByCallerErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"graphql errors", http.StatusOK, `{"data":null,"errors":[{"message":"unknown field"}]}`},
		{"graphql errors with partial data", http.StatusOK, `{"data":{"getTransactions":[]},"errors":[{"message":"boom"}]}`},
		{"non-200", http.StatusBadGateway, `bad gateway`},
		{"invalid json", http.StatusOK, `not json`},
		{"no data", http.StatusOK, `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, c.status, []byte(c.body), nil)
			if txs, err := New(srv.URL, realm, nil).ProposalsByCaller(context.Background(), caller, 1); err == nil {
				t.Fatalf("want an error, got txs %+v", txs)
			}
		})
	}

	t.Run("invalid arguments", func(t *testing.T) {
		c := New("http://127.0.0.1:1", realm, nil)
		if _, err := c.ProposalsByCaller(context.Background(), "", 1); err == nil {
			t.Fatal("empty caller: want an error")
		}
		if _, err := c.ProposalsByCaller(context.Background(), caller, 0); err == nil {
			t.Fatal("unknown broadcast height (0): want an error")
		}
	})
}

func TestProposalIDFailsClosed(t *testing.T) {
	body := `{"data":{"getTransactions":[
	 {"hash":"a","block_height":5,"messages":[],"response":{"events":[
	   {"type":"ProposalCreated","pkg_path":"gno.land/r/gov/dao","attrs":[{"key":"id","value":"1"}]},
	   {"type":"ProposalCreated","pkg_path":"gno.land/r/gov/dao","attrs":[{"key":"id","value":"2"}]}]}},
	 {"hash":"b","block_height":6,"messages":[],"response":{"events":[
	   {"type":"ProposalCreated","pkg_path":"gno.land/r/evil","attrs":[{"key":"id","value":"3"}]},{}]}},
	 {"hash":"c","block_height":7,"messages":[],"response":{"events":null}}
	]}}`
	srv := serve(t, http.StatusOK, []byte(body), nil)
	txs, err := New(srv.URL, realm, nil).ProposalsByCaller(context.Background(), caller, 1)
	if err != nil || len(txs) != 3 {
		t.Fatalf("txs %+v, err %v", txs, err)
	}
	for _, tx := range txs {
		if tx.ProposalID != "" {
			t.Fatalf("tx %s: ProposalID = %q, want \"\" (ambiguous or foreign event)", tx.Hash, tx.ProposalID)
		}
	}
}

// fixtureEjectScript is config.DefaultEjectScript as it was when the indexer
// fixture was captured (gno.land mainnet realms, chain/mainnet tag:
// r/sys/validators/v0). Pinned here so the test checks
// the rendering path (proposal.Build: placeholder substitution, Go-literal
// escaping, trailing newline) against real indexer output, independently of
// later template changes.
const fixtureEjectScript = `package main

import (
	"gno.land/r/gov/dao"
	validators "gno.land/r/sys/validators/v0"
)

func main(cur realm) {
	pr := validators.NewValidatorProposalRequest(cross(cur),
		[]validators.ValoperChange{validators.NewValoperChange(address("{operator_address}"), 0)},
		"{title}",
		"{description}",
	)
	dao.MustCreateProposal(cross(cur), pr)
}
`

// What katana submits in run mode (the rendered script written verbatim to
// the temp .gno file) hashes to the same value as the body the indexer
// returns for that tx, whatever the file name.
func TestRunPayloadHashMatchesIndexerBody(t *testing.T) {
	srv := serve(t, http.StatusOK, fixture(t, "indexer_proposals_by_caller.response.json"), nil)
	txs, err := New(srv.URL, realm, nil).ProposalsByCaller(context.Background(), caller, 7)
	if err != nil {
		t.Fatal(err)
	}

	p := config.Proposal{Actions: map[string]config.ActionConfig{
		config.ActionEject: {
			RunScriptTemplate:   fixtureEjectScript,
			TitleTemplate:       "Eject validator {moniker} ({address}) from the valset [fixture 2]",
			DescriptionTemplate: "Automated ejection proposal generated by katana (e2e fixture capture).\nScore: {score}/100 over {window}",
		},
	}}
	b := proposal.Build(proposal.Target{
		Moniker:         "samourai-crew-4",
		Address:         "g155yu85dl2rygemkxcvn3g26nfw0rr5y2dkj6m0",
		OperatorAddress: "g1xqwtx7typgztlwtqzqqungq6lpy5k3a9p8djxp",
		Action:          config.ActionEject,
		Score:           12,
		Window:          "current_week",
	}, p, config.Gnokey{})
	if b.Submit != "run" {
		t.Fatalf("Submit = %q, want run", b.Submit)
	}

	h := RunPayloadHash(b.Script)
	if got := txs[1].PayloadHash(); got != h {
		t.Fatalf("indexer body hash %s != submitted script hash %s\nsubmitted:\n%q\nindexer:\n%q",
			got, h, b.Script, txs[1].Messages[0].Files[0].Body)
	}
	tx, ok := Match(txs, h)
	if !ok || tx.ProposalID != "2" || tx.Hash != "j6iKhwYrUpu1zZw4AEwHRyPvx9HrPqghSZAEUTuAR8Q=" {
		t.Fatalf("Match = %+v, %v", tx, ok)
	}
}

func TestPayloadHashCanonicalization(t *testing.T) {
	body := "package main\n\nfunc main() {}\n"
	if RunPayloadHash(body) == RunPayloadHash(strings.TrimSuffix(body, "\n")) {
		t.Fatal("run hash must be byte-exact (trailing newline)")
	}
	if len(RunPayloadHash(body)) != 64 {
		t.Fatalf("hash %q is not hex SHA-256", RunPayloadHash(body))
	}

	distinct := map[string]string{
		"base":         CallPayloadHash("gno.land/r/x", "F", []string{"a", "b"}),
		"joined args":  CallPayloadHash("gno.land/r/x", "F", []string{"ab"}),
		"shifted args": CallPayloadHash("gno.land/r/x", "F", []string{"a", "", "b"}),
		"moved fn":     CallPayloadHash("gno.land/r/xF", "", []string{"a", "b"}),
		"moved arg":    CallPayloadHash("gno.land/r/x", "Fa", []string{"b"}),
		"no args":      CallPayloadHash("gno.land/r/x", "F", nil),
		"one empty":    CallPayloadHash("gno.land/r/x", "F", []string{""}),
		"run":          RunPayloadHash("gno.land/r/x"),
		"run vs call":  RunPayloadHash(""),
		"empty call":   CallPayloadHash("", "", nil),
	}
	seen := map[string]string{}
	for name, h := range distinct {
		if prev, dup := seen[h]; dup {
			t.Fatalf("%s and %s hash to the same value", name, prev)
		}
		seen[h] = name
	}
	if CallPayloadHash("p", "f", nil) != CallPayloadHash("p", "f", []string{}) {
		t.Fatal("nil and empty args must hash the same (both are no args)")
	}
}

func TestTxPayloadHash(t *testing.T) {
	call := Msg{TypeURL: "call", Route: "vm", PkgPath: "gno.land/r/gov/dao", Func: "F", Args: []string{"1"}}
	run := Msg{TypeURL: "run", Route: "vm", Files: []File{{Name: "katana-123.gno", Body: "package main\n"}}}
	cases := []struct {
		name string
		tx   Tx
		want string
	}{
		{"call", Tx{Messages: []Msg{call}}, CallPayloadHash("gno.land/r/gov/dao", "F", []string{"1"})},
		{"run", Tx{Messages: []Msg{run}}, RunPayloadHash("package main\n")},
		{"run ignores non-gno files", Tx{Messages: []Msg{{TypeURL: "run", Route: "vm", Files: []File{
			{Name: "gnomod.toml", Body: "module = \"x\"\n"}, {Name: "a.gno", Body: "package main\n"}}}}},
			RunPayloadHash("package main\n")},
		{"run with two gno files", Tx{Messages: []Msg{{TypeURL: "run", Files: []File{
			{Name: "a.gno", Body: "package main\n"}, {Name: "b.gno", Body: "package main\n"}}}}}, ""},
		{"run with no file", Tx{Messages: []Msg{{TypeURL: "run"}}}, ""},
		{"two messages", Tx{Messages: []Msg{call, call}}, ""},
		{"no message", Tx{}, ""},
		{"other message", Tx{Messages: []Msg{{TypeURL: "send", Route: "bank"}}}, ""},
	}
	for _, c := range cases {
		if got := c.tx.PayloadHash(); got != c.want {
			t.Fatalf("%s: PayloadHash = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMatch(t *testing.T) {
	mk := func(hash, id, body string) Tx {
		return Tx{Hash: hash, ProposalID: id, Messages: []Msg{{TypeURL: "run", Files: []File{{Name: "x.gno", Body: body}}}}}
	}
	h := RunPayloadHash("mine")

	if tx, ok := Match([]Tx{mk("a", "1", "other"), mk("b", "2", "mine")}, h); !ok || tx.Hash != "b" {
		t.Fatalf("single match = %+v, %v", tx, ok)
	}
	if _, ok := Match([]Tx{mk("a", "1", "mine"), mk("b", "2", "mine")}, h); ok {
		t.Fatal("two matches must fail closed")
	}
	if _, ok := Match([]Tx{mk("a", "", "mine")}, h); ok {
		t.Fatal("a match without a proposal id must not be returned")
	}
	if _, ok := Match([]Tx{mk("a", "1", "other")}, h); ok {
		t.Fatal("no match expected")
	}
	if _, ok := Match([]Tx{{Hash: "a", ProposalID: "1"}}, ""); ok {
		t.Fatal("an empty payload hash (unknown) must never match")
	}
	if _, ok := Match(nil, h); ok {
		t.Fatal("no txs: no match")
	}
}
