package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
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
	if len(notifs) != 1 || !strings.Contains(notifs[0], "expired") {
		t.Fatalf("notifications = %v, want one expired", notifs)
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
