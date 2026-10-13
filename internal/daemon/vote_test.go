package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
	"github.com/samouraiworld/katana/internal/txindexer"
)

// render loads a real GovDAO v3 proposal page from the govdao fixtures.
func render(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "govdao", "testdata", "render", name+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// seed records a broadcast proposal for g1bad created age ago.
func seed(t *testing.T, f *fixture, onchainID, txHash, status string, age time.Duration) int64 {
	t.Helper()
	id, err := f.st.RecordProposal(context.Background(), store.ProposalRecord{
		CreatedAt: f.d.now().Add(-age), Chain: "test", ChainID: "dev",
		Address: "g1bad", Moniker: "bad", Action: config.ActionEject,
		Success: status != store.StatusUnconfirmed, Status: status,
		OnchainID: onchainID, TxHash: txHash,
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	return id
}

func lastStatus(t *testing.T, f *fixture) store.ProposalRecord {
	t.Helper()
	last, err := f.st.LastProposal(context.Background(), "test", "g1bad")
	if err != nil || last == nil {
		t.Fatalf("LastProposal = %+v, %v", last, err)
	}
	return *last
}

func executeCalls(f *fixture) []proposal.Built {
	var out []proposal.Built
	for _, b := range f.broadcasts.calls() {
		if b.Function == proposal.ExecuteFunc {
			out = append(out, b)
		}
	}
	return out
}

func TestVoteCycleTerminalOutcomes(t *testing.T) {
	tests := []struct {
		fixture string
		want    string
	}{
		{"accepted", store.StatusAccepted},
		{"denied", store.StatusRejected},
		{"denied_reason", store.StatusRejected},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, "dev", nil)
			seed(t, f, "7", "", store.StatusPending, time.Hour)
			f.chain.setRender("7", render(t, tt.fixture))

			f.d.voteCycle(ctx, f.runtime(t))
			if got := lastStatus(t, f).Status; got != tt.want {
				t.Fatalf("status = %q, want %q\nlogs:\n%s", got, tt.want, f.logs.String())
			}
			if n := len(f.notifs.all()); n != 1 {
				t.Fatalf("notifications = %d, want 1", n)
			}

			// A resolved proposal is no longer tracked: no repeat notification.
			f.d.voteCycle(ctx, f.runtime(t))
			if n := len(f.notifs.all()); n != 1 {
				t.Fatalf("notifications after second cycle = %d, want 1", n)
			}
			if n := len(executeCalls(f)); n != 0 {
				t.Fatalf("execute calls = %d, want 0 on a decided proposal", n)
			}
		})
	}
}

// Real pages that are not a decided outcome: vote links and words like
// ACCEPTED in the description must not resolve the proposal.
func TestVoteCycleOpenOrUnknownStaysPending(t *testing.T) {
	for _, fixture := range []string{"open_desc_accepted", "open_split", "open_forged_marker"} {
		t.Run(fixture, func(t *testing.T) {
			f := newFixture(t, "dev", nil)
			seed(t, f, "7", "", store.StatusPending, time.Hour)
			f.chain.setRender("7", render(t, fixture))

			f.d.voteCycle(context.Background(), f.runtime(t))
			if got := lastStatus(t, f).Status; got != store.StatusPending {
				t.Fatalf("status = %q, want pending", got)
			}
			if n := len(f.notifs.all()); n != 0 {
				t.Fatalf("notifications = %d, want 0", n)
			}
			if n := len(executeCalls(f)); n != 0 {
				t.Fatalf("execute calls = %d, want 0 without a supermajority", n)
			}
		})
	}
}

func TestVoteCycleExecutesOnSupermajority(t *testing.T) {
	for _, fixture := range []string{"open_supermajority_yes", "open_two_thirds_yes"} {
		t.Run(fixture, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, "dev", nil)
			seed(t, f, "7", "", store.StatusPending, time.Hour)
			f.chain.setRender("7", render(t, fixture))

			f.d.voteCycle(ctx, f.runtime(t))
			calls := executeCalls(f)
			if len(calls) != 1 {
				t.Fatalf("execute calls = %d, want 1\nlogs:\n%s", len(calls), f.logs.String())
			}
			if c := calls[0]; c.RealmPath != "gno.land/r/gov/dao" || len(c.Args) != 1 || c.Args[0] != "7" || c.Target.ChainID != "dev" {
				t.Fatalf("execute call = %+v", c)
			}
			if got := lastStatus(t, f).Status; got != store.StatusPending {
				t.Fatalf("status = %q, want pending until the render shows the outcome", got)
			}

			// Still open on the next tick (tx not yet reflected): no retry
			// inside the backoff window, so a failing call does not burn gas
			// every vote_poll_interval.
			f.d.voteCycle(ctx, f.runtime(t))
			if n := len(executeCalls(f)); n != 1 {
				t.Fatalf("execute calls after second cycle = %d, want 1", n)
			}

			// Retry once the backoff has elapsed.
			f.d.now = func() time.Time { return time.Now().Add(2 * executeRetry) }
			f.d.voteCycle(ctx, f.runtime(t))
			if n := len(executeCalls(f)); n != 2 {
				t.Fatalf("execute calls after backoff = %d, want 2", n)
			}
		})
	}
}

func TestVoteCycleExecuteDisabled(t *testing.T) {
	f := newFixture(t, "dev", func(c *config.Config) {
		c.Chains["test"].ExecuteProposals = boolPtr(false)
	})
	seed(t, f, "7", "", store.StatusPending, time.Hour)
	f.chain.setRender("7", render(t, "open_supermajority_yes"))

	f.d.voteCycle(context.Background(), f.runtime(t))
	if n := len(executeCalls(f)); n != 0 {
		t.Fatalf("execute calls = %d, want 0 with execute_proposals: false", n)
	}
}

// A dry_run chain signs nothing, ExecuteOrRejectProposal included.
func TestVoteCycleDryRunNeverExecutes(t *testing.T) {
	f := newFixture(t, "dev", func(c *config.Config) {
		c.Chains["test"].DryRun = boolPtr(true)
		c.Chains["test"].ExecuteProposals = boolPtr(true)
	})
	seed(t, f, "7", "", store.StatusPending, time.Hour)
	f.chain.setRender("7", render(t, "open_supermajority_yes"))

	f.d.voteCycle(context.Background(), f.runtime(t))
	if n := f.broadcasts.count(); n != 0 {
		t.Fatalf("broadcasts = %d, want 0 on a dry_run chain", n)
	}
	if got := lastStatus(t, f).Status; got != store.StatusPending {
		t.Fatalf("status = %q, want pending", got)
	}
}

func TestVoteCycleExpiresAndKeepsWatching(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)
	seed(t, f, "7", "", store.StatusPending, 15*24*time.Hour)
	f.chain.setRender("7", render(t, "open_split"))

	f.d.voteCycle(ctx, f.runtime(t))
	last := lastStatus(t, f)
	if last.Status != store.StatusExpired || last.ResolvedAt.IsZero() {
		t.Fatalf("status = %q resolved_at = %v, want expired with a resolution time", last.Status, last.ResolvedAt)
	}
	notifs := f.notifs.all()
	if len(notifs) != 1 || !strings.Contains(notifs[0], "expired") || !strings.Contains(notifs[0], "336h0m0s") {
		t.Fatalf("notifications = %v, want one expired citing the 336h pending_timeout", notifs)
	}

	// Expired once: no repeat notification while it stays open.
	f.d.voteCycle(ctx, f.runtime(t))
	if n := len(f.notifs.all()); n != 1 {
		t.Fatalf("notifications = %d, want 1", n)
	}

	// The proposal is still open on-chain: a late outcome is picked up.
	f.chain.setRender("7", render(t, "accepted"))
	f.d.voteCycle(ctx, f.runtime(t))
	if got := lastStatus(t, f).Status; got != store.StatusAccepted {
		t.Fatalf("status = %q, want accepted after a late outcome", got)
	}
	if n := len(f.notifs.all()); n != 2 {
		t.Fatalf("notifications = %d, want 2", n)
	}
}

// An overdue proposal that katana executes is recorded with its outcome right
// away, never as expired first (that would notify two contradictory events).
func TestVoteCycleExecutedOverdueProposalIsNotExpired(t *testing.T) {
	f := newFixture(t, "dev", nil)
	seed(t, f, "7", "", store.StatusPending, 15*24*time.Hour)
	f.chain.setRender("7", render(t, "open_supermajority_yes"))
	f.broadcasts.onCall = func(b proposal.Built) {
		if b.Function == proposal.ExecuteFunc {
			f.chain.setRender("7", render(t, "accepted"))
		}
	}

	f.d.voteCycle(context.Background(), f.runtime(t))
	if got := lastStatus(t, f).Status; got != store.StatusAccepted {
		t.Fatalf("status = %q, want accepted\nlogs:\n%s", got, f.logs.String())
	}
	notifs := f.notifs.all()
	if len(notifs) != 1 || strings.Contains(notifs[0], "expired") {
		t.Fatalf("notifications = %v, want one accepted", notifs)
	}
}

// An execute call whose outcome is unknown may still decide the proposal: an
// overdue row is left for the next tick to read, not expired first.
func TestVoteCycleAmbiguousExecuteIsNotExpired(t *testing.T) {
	f := newFixture(t, "dev", nil)
	seed(t, f, "7", "", store.StatusPending, 15*24*time.Hour)
	f.chain.setRender("7", render(t, "open_supermajority_yes"))
	f.broadcasts.set("broadcast tx: broadcasting bytes: request timeout", errors.New("gnokey: exit status 1"))

	f.d.voteCycle(context.Background(), f.runtime(t))
	if n := len(executeCalls(f)); n != 1 {
		t.Fatalf("execute calls = %d, want 1", n)
	}
	if got := lastStatus(t, f).Status; got != store.StatusPending {
		t.Fatalf("status = %q, want pending\nlogs:\n%s", got, f.logs.String())
	}
	if notifs := f.notifs.all(); len(notifs) != 0 {
		t.Fatalf("notifications = %v, want none", notifs)
	}
}

// failUpdateStore fails every status write.
type failUpdateStore struct{ store.Store }

func (failUpdateStore) UpdateStatus(context.Context, int64, string, time.Time) error {
	return errors.New("disk full")
}

// An outcome is notified only once persisted: a failing write retries next
// tick without notifying the same outcome every time.
func TestVoteCycleUnpersistedOutcomeIsNotNotified(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)
	seed(t, f, "7", "", store.StatusPending, time.Hour)
	f.chain.setRender("7", render(t, "accepted"))
	f.d.st = failUpdateStore{Store: f.st}

	for i := 0; i < 2; i++ {
		f.d.voteCycle(ctx, f.runtime(t))
	}
	if notifs := f.notifs.all(); len(notifs) != 0 {
		t.Fatalf("notifications = %v, want none\nlogs:\n%s", notifs, f.logs.String())
	}
	if got := lastStatus(t, f).Status; got != store.StatusPending {
		t.Fatalf("status = %q, want pending", got)
	}
}

// A row recorded before the tx_hash column existed still carries the hash in
// its stored gnokey output: it is used to resolve the missing id.
func TestVoteCycleResolvesIDFromStoredOutput(t *testing.T) {
	f := newFixture(t, "dev", nil)
	id, err := f.st.RecordProposal(context.Background(), store.ProposalRecord{
		CreatedAt: f.d.now().Add(-time.Hour), Chain: "test", ChainID: "dev",
		Address: "g1bad", Moniker: "bad", Action: config.ActionEject,
		Success: true, Status: store.StatusPending,
		Output: "\nOK!\nHEIGHT:     42\nEVENTS:     []\nTX HASH:    q2Vh\n",
	})
	if err != nil || id == 0 {
		t.Fatalf("RecordProposal: %v", err)
	}
	f.chain.txs = map[string]string{
		"ab6561": `[{"type":"ProposalCreated","attrs":[{"key":"id","value":"9"}],"pkg_path":"gno.land/r/gov/dao"}]`,
	}
	f.chain.setRender("9", render(t, "accepted"))

	f.d.voteCycle(context.Background(), f.runtime(t))
	if last := lastStatus(t, f); last.OnchainID != "9" || last.Status != store.StatusAccepted {
		t.Fatalf("onchain id = %q status = %q, want 9 accepted", last.OnchainID, last.Status)
	}
}

// An unreadable render past the timeout keeps the row pending: unknown is
// inert, never terminal.
func TestVoteCycleUnknownNeverExpires(t *testing.T) {
	f := newFixture(t, "dev", nil)
	seed(t, f, "7", "", store.StatusPending, 15*24*time.Hour)
	f.chain.setRender("7", render(t, "open_forged_marker"))

	f.d.voteCycle(context.Background(), f.runtime(t))
	if got := lastStatus(t, f).Status; got != store.StatusPending {
		t.Fatalf("status = %q, want pending", got)
	}
	if n := len(f.notifs.all()); n != 0 {
		t.Fatalf("notifications = %d, want 0", n)
	}
}

func TestVoteCycleResolvesMissingIDFromTxHash(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)
	seed(t, f, "", "q2Vh", store.StatusPending, time.Hour)
	f.chain.txs = map[string]string{
		"ab6561": `[{"@type":"/tm.GnoEvent","type":"ProposalCreated","attrs":[{"key":"id","value":"9"}],"pkg_path":"gno.land/r/gov/dao"}]`,
	}
	f.chain.setRender("9", render(t, "denied"))

	f.d.voteCycle(ctx, f.runtime(t))
	last := lastStatus(t, f)
	if last.OnchainID != "9" || last.Status != store.StatusRejected {
		t.Fatalf("onchain id = %q status = %q, want 9 rejected\nlogs:\n%s", last.OnchainID, last.Status, f.logs.String())
	}
}

func TestVoteCycleExpiresUnconfirmed(t *testing.T) {
	f := newFixture(t, "dev", nil)
	seed(t, f, "", "", store.StatusUnconfirmed, 15*24*time.Hour)

	f.d.voteCycle(context.Background(), f.runtime(t))
	if got := lastStatus(t, f).Status; got != store.StatusExpired {
		t.Fatalf("status = %q, want expired", got)
	}
}

func TestEvalCycleRecordsBroadcastOutcome(t *testing.T) {
	gnokeyErr := errors.New("gnokey: exit status 1")
	tests := []struct {
		name       string
		out        string
		err        error
		wantStatus string
		wantID     string
		wantNotif  string // substring of the single notification, "" = none
	}{
		{"committed", gnokeyOK("12"), nil, store.StatusPending, "12", "submitted"},
		{"ambiguous transport error", "broadcast tx: broadcasting bytes: request timeout", gnokeyErr, store.StatusUnconfirmed, "", "unconfirmed"},
		{"check tx failed", "check transaction failed: log:insufficient funds", gnokeyErr, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, "dev", nil)
			f.broadcasts.set(tt.out, tt.err)

			f.d.evalCycle(ctx, f.runtime(t))
			last := lastStatus(t, f)
			if last.Status != tt.wantStatus || last.OnchainID != tt.wantID {
				t.Fatalf("status = %q id = %q, want %q %q\nlogs:\n%s", last.Status, last.OnchainID, tt.wantStatus, tt.wantID, f.logs.String())
			}
			notifs := f.notifs.all()
			if tt.wantNotif == "" {
				if len(notifs) != 0 {
					t.Fatalf("notifications = %v, want none", notifs)
				}
			} else if len(notifs) != 1 || !strings.Contains(notifs[0], tt.wantNotif) {
				t.Fatalf("notifications = %v, want one %q", notifs, tt.wantNotif)
			}

			// An unconfirmed broadcast blocks the next cycle like a pending one.
			if tt.wantStatus == store.StatusUnconfirmed {
				f.d.evalCycle(ctx, f.runtime(t))
				if n := len(f.broadcasts.calls()); n != 1 {
					t.Fatalf("broadcasts = %d, want 1 (unconfirmed must gate)", n)
				}
			}
		})
	}
}

// ExecuteOrRejectProposal only rejects when the executor returns an error; a
// panicking executor (eject of a validator already gone) reverts the call and
// leaves the proposal open at a supermajority. katana gives up after
// maxExecuteFailures attempts and alerts once instead of retrying forever.
func TestVoteCycleGivesUpOnFailingExecute(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)
	seed(t, f, "7", "", store.StatusPending, time.Hour)
	f.chain.setRender("7", render(t, "open_supermajority_yes"))
	f.broadcasts.set("TX HASH:    \npanic: validator does not exist: g1bad\n", errors.New("gnokey: exit status 1"))

	start := time.Now()
	for i := 0; i < maxExecuteFailures+3; i++ {
		at := start.Add(time.Duration(i) * 2 * executeRetry)
		f.d.now = func() time.Time { return at }
		f.d.voteCycle(ctx, f.runtime(t))
	}
	if n := len(executeCalls(f)); n != maxExecuteFailures {
		t.Fatalf("execute calls = %d, want %d", n, maxExecuteFailures)
	}
	notifs := f.notifs.all()
	if len(notifs) != 1 || !strings.Contains(notifs[0], "execution failed") {
		t.Fatalf("notifications = %v, want one execution-failed alert", notifs)
	}
	if got := lastStatus(t, f).Status; got != store.StatusPending {
		t.Fatalf("status = %q, want pending (the proposal is still open)", got)
	}
}

// indexerServer serves the real tx-indexer response fixture and returns the
// tx it contains, as the txindexer client parses it.
func indexerServer(t *testing.T) (string, txindexer.Tx) {
	t.Helper()
	body, err := os.ReadFile("../txindexer/testdata/indexer_proposals_by_caller.response.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	txs, err := txindexer.New(srv.URL, "gno.land/r/gov/dao", nil).ProposalsByCaller(context.Background(), "g1katana", 1)
	if err != nil || len(txs) == 0 {
		t.Fatalf("fixture: %d txs, err %v", len(txs), err)
	}
	return srv.URL, txs[0]
}

func seedHashless(t *testing.T, f *fixture, status, payloadHash string) int64 {
	t.Helper()
	id, err := f.st.RecordProposal(context.Background(), store.ProposalRecord{
		CreatedAt: f.d.now().Add(-time.Minute), Chain: "test", ChainID: "dev",
		Address: "g1bad", Moniker: "bad", Action: config.ActionEject, Status: status,
		BroadcastHeight: 10, PayloadHash: payloadHash,
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	return id
}

// A hashless unconfirmed broadcast is matched on the tx-indexer by payload
// hash, and gets its tx hash and GovDAO id back.
func TestVoteReconcilesHashlessUnconfirmed(t *testing.T) {
	url, tx := indexerServer(t)
	f := newFixture(t, "dev", nil)
	rt := f.runtime(t)
	rt.TxIndexerURL = url
	rt.Gnokey.Address = "g1katana"
	seedHashless(t, f, store.StatusUnconfirmed, tx.PayloadHash())
	f.chain.setRender(tx.ProposalID, "- **Proposal is open for votes**\n- YES PERCENT: 0%\n- NO PERCENT: 0%\n")

	if err := f.d.voteCycle(context.Background(), rt); err != nil {
		t.Fatalf("voteCycle: %v\nlogs:\n%s", err, f.logs.String())
	}
	got := lastStatus(t, f)
	if got.TxHash != tx.Hash || got.OnchainID != tx.ProposalID || got.Status != store.StatusPending {
		t.Fatalf("row = %+v, want tx %s id %s pending\nlogs:\n%s", got, tx.Hash, tx.ProposalID, f.logs.String())
	}
}

// Reconcile asks the indexer for the configured GovDAO realm, not a
// hard-coded one.
func TestVoteReconcileUsesConfiguredRealm(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"data":{"getTransactions":null}}`))
	}))
	t.Cleanup(srv.Close)
	f := newFixture(t, "dev", nil)
	f.d.cfg.VoteTracking.GovdaoRealm = "gno.land/r/gov/dao/custom"
	rt := f.runtime(t)
	rt.TxIndexerURL, rt.Gnokey.Address = srv.URL, "g1katana"
	seedHashless(t, f, store.StatusUnconfirmed, "deadbeef")

	if err := f.d.voteCycle(context.Background(), rt); err != nil {
		t.Fatalf("voteCycle: %v", err)
	}
	if !strings.Contains(string(reqBody), `"realm":"gno.land/r/gov/dao/custom"`) {
		t.Fatalf("indexer request does not use the configured realm: %s", reqBody)
	}
}

// No match (or no indexer configured): the row stays unconfirmed and hashless.
func TestVoteReconcileNoMatch(t *testing.T) {
	url, _ := indexerServer(t)
	for name, indexer := range map[string]string{"no match": url, "no indexer": ""} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "dev", nil)
			rt := f.runtime(t)
			rt.TxIndexerURL = indexer
			rt.Gnokey.Address = "g1katana"
			seedHashless(t, f, store.StatusUnconfirmed, "deadbeef")

			f.d.voteCycle(context.Background(), rt)
			if got := lastStatus(t, f); got.TxHash != "" || got.OnchainID != "" || got.Status != store.StatusUnconfirmed {
				t.Fatalf("row = %+v, want untouched", got)
			}
		})
	}
}

// A broadcast records what reconcile needs should gnokey lose the tx hash.
func TestBroadcastRecordsReconcileData(t *testing.T) {
	f := newFixture(t, "dev", nil)
	f.d.evalCycle(context.Background(), f.runtime(t))

	calls := f.broadcasts.calls()
	if len(calls) != 1 {
		t.Fatalf("broadcasts = %d, want 1\nlogs:\n%s", len(calls), f.logs.String())
	}
	got := lastStatus(t, f)
	if got.BroadcastHeight != 42 || got.PayloadHash == "" || got.PayloadHash != txindexer.RunPayloadHash(calls[0].Script) {
		t.Fatalf("row BroadcastHeight=%d PayloadHash=%q, want 42 and the script's hash", got.BroadcastHeight, got.PayloadHash)
	}
}

// A shutdown signal landing while gnokey runs interrupts it after the tx may
// have been sent: the outcome must still be recorded and alerted, or the
// restarted daemon would re-propose with no record of the first attempt.
func TestEvalCycleRecordsInterruptedBroadcast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, "dev", nil)
	f.broadcasts.onCall = func(proposal.Built) { cancel() }
	f.broadcasts.set("", fmt.Errorf("gnokey interrupted: %w", context.Canceled))

	f.d.evalCycle(ctx, f.runtime(t))
	if got := lastStatus(t, f).Status; got != store.StatusUnconfirmed {
		t.Fatalf("status = %q, want unconfirmed\nlogs:\n%s", got, f.logs.String())
	}
	notifs := f.notifs.all()
	if len(notifs) != 1 || !strings.Contains(notifs[0], "unconfirmed") {
		t.Fatalf("notifications = %v, want one unconfirmed", notifs)
	}
}

// The last allowed execute attempt being interrupted by a shutdown still
// raises the give-up alert.
func TestVoteCycleAlertsOnInterruptedLastExecute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, "dev", nil)
	seed(t, f, "7", "", store.StatusPending, time.Hour)
	f.chain.setRender("7", render(t, "open_supermajority_yes"))
	f.broadcasts.set("", fmt.Errorf("gnokey interrupted: %w", context.Canceled))

	start := time.Now()
	for i := 0; i < maxExecuteFailures; i++ {
		if i == maxExecuteFailures-1 {
			f.broadcasts.onCall = func(proposal.Built) { cancel() }
		}
		at := start.Add(time.Duration(i) * 2 * executeRetry)
		f.d.now = func() time.Time { return at }
		f.d.voteCycle(ctx, f.runtime(t))
	}
	if n := len(executeCalls(f)); n != maxExecuteFailures {
		t.Fatalf("execute calls = %d, want %d\nlogs:\n%s", n, maxExecuteFailures, f.logs.String())
	}
	notifs := f.notifs.all()
	if len(notifs) != 1 || !strings.Contains(notifs[0], "execution failed") {
		t.Fatalf("notifications = %v, want one execution-failed alert", notifs)
	}
}
