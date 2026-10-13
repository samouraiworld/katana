package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
)

// failStore makes chosen lifecycle queries fail on top of a real Store.
type failStore struct {
	store.Store
	lastErr, rejErr, sinceErr, trackedErr, dryRunsErr error
}

func (f *failStore) LastProposal(ctx context.Context, chain, addr string) (*store.ProposalRecord, error) {
	if f.lastErr != nil {
		return nil, f.lastErr
	}
	return f.Store.LastProposal(ctx, chain, addr)
}

func (f *failStore) RejectionCount(ctx context.Context, chain, addr string) (int, error) {
	if f.rejErr != nil {
		return 0, f.rejErr
	}
	return f.Store.RejectionCount(ctx, chain, addr)
}

func (f *failStore) BroadcastsSince(ctx context.Context, chain string, since time.Time) (int, error) {
	if f.sinceErr != nil {
		return 0, f.sinceErr
	}
	return f.Store.BroadcastsSince(ctx, chain, since)
}

func (f *failStore) TrackedProposals(ctx context.Context, chain string) ([]store.ProposalRecord, error) {
	if f.trackedErr != nil {
		return nil, f.trackedErr
	}
	return f.Store.TrackedProposals(ctx, chain)
}

func (f *failStore) DryRunsSince(ctx context.Context, chain string, since time.Time) ([]store.ProposalRecord, error) {
	if f.dryRunsErr != nil {
		return nil, f.dryRunsErr
	}
	return f.Store.DryRunsSince(ctx, chain, since)
}

// A store error on any lifecycle query must close the gate, never open it.
func TestEvalCycleFailsClosedOnStoreErrors(t *testing.T) {
	boom := errors.New("database is locked")
	for name, fs := range map[string]*failStore{
		"LastProposal":    {lastErr: boom},
		"RejectionCount":  {rejErr: boom},
		"BroadcastsSince": {sinceErr: boom},
		// Read while counting open ejections for min_valset_size.
		"TrackedProposals": {trackedErr: boom},
		"DryRunsSince":     {dryRunsErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "dev", nil)
			fs.Store = f.st
			f.d.st = fs

			f.d.evalCycle(context.Background(), f.runtime(t))

			if got := f.broadcasts.count(); got != 0 {
				t.Fatalf("broadcasts = %d, want 0 on a %s error\nlogs:\n%s", got, name, f.logs.String())
			}
			if n := len(f.notifs.all()); n != 0 {
				t.Fatalf("notifications = %d, want 0", n)
			}
			if !strings.Contains(f.logs.String(), "database is locked") {
				t.Fatalf("the store error must be logged\nlogs:\n%s", f.logs.String())
			}
		})
	}
}

// valReport renders one gnomonitoring report entry with the same score and
// voting power on all four windows.
func valReport(addr string, score int) string {
	p := fmt.Sprintf(`{"score":%d,"tier":"T","voting_power":1000}`, score)
	return fmt.Sprintf(`{"addr":%q,"moniker":%q,"periods":{"last_24h":%s,"current_week":%s,"current_month":%s,"current_year":%s}}`,
		addr, addr, p, p, p, p)
}

// newValsetFixture builds a fixture whose valset holds bad eject-eligible
// validators (score 10, g1bad0..) and healthy no-action ones (score 70,
// g1ok0..). Every bad validator has an operator mapping. Eject is armed
// (mode auto) unless mutate says otherwise.
func newValsetFixture(t *testing.T, bad, healthy int, mutate func(*config.Config)) *fixture {
	return newMixedFixture(t, bad, 0, healthy, mutate)
}

// newMixedFixture is newValsetFixture plus watch lower_vp-eligible
// validators (score 50, g1watch0..), listed after the bad ones.
func newMixedFixture(t *testing.T, bad, watch, healthy int, mutate func(*config.Config)) *fixture {
	t.Helper()
	var reports, valset []string
	operators := map[string]string{}
	for i := 0; i < bad; i++ {
		addr := fmt.Sprintf("g1bad%d", i)
		reports = append(reports, valReport(addr, 10))
		valset = append(valset, addr)
		operators[fmt.Sprintf("g1op%d", i)] = addr
	}
	for i := 0; i < watch; i++ {
		addr := fmt.Sprintf("g1watch%d", i)
		reports = append(reports, valReport(addr, 50))
		valset = append(valset, addr)
		operators[fmt.Sprintf("g1wop%d", i)] = addr
	}
	for i := 0; i < healthy; i++ {
		addr := fmt.Sprintf("g1ok%d", i)
		reports = append(reports, valReport(addr, 70))
		valset = append(valset, addr)
	}
	body := "[" + strings.Join(reports, ",") + "]"
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(gnomon.Close)

	f := newFixture(t, "dev", func(c *config.Config) {
		c.Gnomonitoring.BaseURL = gnomon.URL
		if mutate != nil {
			mutate(c)
		}
	})
	f.chain.valset = valset
	f.chain.operators = operators
	return f
}

// strict returns the runtime with the confirmed default guardrails.
func strict(rt config.Runtime) config.Runtime {
	rt.Safety = config.SafetyRuntime{MaxProposalsPerCycle: 1, MaxProposalsPerDay: 3, MaxEligibleShare: 25, MinValsetSize: 4}
	return rt
}

func TestMaxProposalsPerCycle(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 2, 8, nil)
	rt := strict(f.runtime(t))

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts after cycle 1 = %d, want 1 (per-cycle cap)\nlogs:\n%s", got, f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "max_proposals_per_cycle") {
		t.Fatalf("the cap must be logged\nlogs:\n%s", f.logs.String())
	}
	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 2 {
		t.Fatalf("broadcasts after cycle 2 = %d, want 2 (the second validator's turn)", got)
	}
}

func TestMaxProposalsPerDay(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 1, 9, nil)
	rt := strict(f.runtime(t))
	now := f.d.now()
	for i, at := range []time.Time{now.Add(-time.Hour), now.Add(-2 * time.Hour), now.Add(-23 * time.Hour)} {
		if _, err := f.st.RecordProposal(ctx, store.ProposalRecord{
			CreatedAt: at, Chain: "test", ChainID: "dev", Address: fmt.Sprintf("g1other%d", i),
			Status: store.StatusPending, Success: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 (3 broadcasts in the last 24h)\nlogs:\n%s", got, f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "max_proposals_per_day") {
		t.Fatalf("the cap must be logged\nlogs:\n%s", f.logs.String())
	}

	// Once the oldest leaves the 24h window, one slot frees up.
	f.d.now = func() time.Time { return now.Add(2 * time.Hour) }
	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 once a slot frees up\nlogs:\n%s", got, f.logs.String())
	}
}

func TestMassEligibilityAbortsCycle(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 3, 7, nil) // 30% eligible > 25%
	rt := strict(f.runtime(t))

	f.d.evalCycle(ctx, rt)
	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 (mass eligibility)\nlogs:\n%s", got, f.logs.String())
	}
	if props, _ := f.st.ListProposals(ctx, 10); len(props) != 0 {
		t.Fatalf("proposals = %+v, want none", props)
	}
	if n := len(f.notifs.all()); n != 1 {
		t.Fatalf("notifications = %d, want exactly 1 alert across both cycles", n)
	}
	if !strings.Contains(f.notifs.all()[0], "3 of 10") {
		t.Fatalf("alert should give the eligible count: %s", f.notifs.all()[0])
	}

	// Under the threshold, proposals resume; tripping again re-alerts.
	rt.Safety.MaxEligibleShare = 50
	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 once under the threshold\nlogs:\n%s", got, f.logs.String())
	}
	rt.Safety.MaxEligibleShare = 25
	f.d.evalCycle(ctx, rt)
	if n := len(f.notifs.all()); n != 3 { // alert, sent, alert
		t.Fatalf("notifications = %d, want 3 (re-alert after recovery)", n)
	}
}

func TestMinValsetSize(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 1, 3, nil) // 4 validators, ejecting one leaves 3
	rt := strict(f.runtime(t))

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 (valset would drop below 4)\nlogs:\n%s", got, f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "min_valset_size") {
		t.Fatalf("the guardrail must be logged\nlogs:\n%s", f.logs.String())
	}
}

// Open ejections of validators still in the valset count as already gone.
func TestMinValsetSizeCountsOpenEjections(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 1, 4, nil) // 5 validators
	rt := strict(f.runtime(t))
	if _, err := f.st.RecordProposal(ctx, store.ProposalRecord{
		CreatedAt: f.d.now().Add(-48 * time.Hour), Chain: "test", ChainID: "dev", Address: "g1ok0",
		Action: config.ActionEject, Status: store.StatusPending, Success: true,
	}); err != nil {
		t.Fatal(err)
	}

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 (5 - 1 open ejection - 1 = 3 < 4)\nlogs:\n%s", got, f.logs.String())
	}
}

func TestEmptyValsetProposesNothing(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 1, 9, nil)
	f.chain.valset = nil

	f.d.evalCycle(ctx, strict(f.runtime(t)))
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 on an empty valset", got)
	}
	if !strings.Contains(f.logs.String(), "empty valset") {
		t.Fatalf("the empty valset must be logged\nlogs:\n%s", f.logs.String())
	}
}

// A manual action never broadcasts on an armed chain: it is notified with
// its script like a dry run, and does not use up the broadcast caps.
func TestManualActionModeDoesNotBroadcast(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 2, 8, func(c *config.Config) {
		ac := c.Proposal.Actions[config.ActionEject]
		ac.Mode = config.ModeManual
		c.Proposal.Actions[config.ActionEject] = ac
	})
	rt := strict(f.runtime(t))

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 for a manual action\nlogs:\n%s", got, f.logs.String())
	}
	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 2 {
		t.Fatalf("proposals = %+v, err %v; want 2 dry-run records (manual does not use the cap)", props, err)
	}
	for _, p := range props {
		if p.Status != store.StatusDryRun {
			t.Fatalf("status = %q, want dry-run", p.Status)
		}
	}
}

// A manual proposal does not use up the broadcast budget of an automatic
// one in the same cycle.
func TestManualActionDoesNotConsumeCycleCap(t *testing.T) {
	ctx := context.Background()
	f := newMixedFixture(t, 1, 1, 8, func(c *config.Config) {
		ac := c.Proposal.Actions[config.ActionEject]
		ac.Mode = config.ModeManual
		c.Proposal.Actions[config.ActionEject] = ac
	})

	f.d.evalCycle(ctx, strict(f.runtime(t)))
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 (the lower_vp, after the manual eject)\nlogs:\n%s", got, f.logs.String())
	}
	if title := f.broadcasts.calls()[0].Title; !strings.HasPrefix(title, "Lower") {
		t.Fatalf("broadcast title = %q, want the lower_vp proposal", title)
	}
}

// A definitive broadcast failure creates no proposal, so it must not use up
// the cycle's broadcast budget nor count as an open ejection.
func TestFailedBroadcastDoesNotConsumeCaps(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 2, 8, nil)
	f.broadcasts.onCall = func(proposal.Built) {
		// Runs under the recorder's lock: first call fails at CheckTx, the
		// second commits.
		if len(f.broadcasts.built) == 1 {
			f.broadcasts.out, f.broadcasts.err = "check transaction failed: log:insufficient funds", errors.New("gnokey: exit status 1")
		} else {
			f.broadcasts.out, f.broadcasts.err = "", nil
		}
	}

	f.d.evalCycle(ctx, strict(f.runtime(t)))
	if got := f.broadcasts.count(); got != 2 {
		t.Fatalf("broadcast attempts = %d, want 2 (the failure frees its slot)\nlogs:\n%s", got, f.logs.String())
	}
}

// Same for min_valset_size: a failed ejection does not shrink the valset.
func TestFailedEjectionDoesNotCountAsOpen(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 2, 3, nil) // 5 validators; one ejection leaves 4
	f.broadcasts.onCall = func(proposal.Built) {
		if len(f.broadcasts.built) == 1 {
			f.broadcasts.out, f.broadcasts.err = "check transaction failed: log:insufficient funds", errors.New("gnokey: exit status 1")
		} else {
			f.broadcasts.out, f.broadcasts.err = "", nil
		}
	}
	rt := strict(f.runtime(t))
	rt.Safety.MaxProposalsPerCycle = 2
	rt.Safety.MaxEligibleShare = 50

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 2 {
		t.Fatalf("broadcast attempts = %d, want 2 (failed ejection is not open)\nlogs:\n%s", got, f.logs.String())
	}
}

// manualEject sets eject to mode manual (the dry-run hand-off).
func manualEject(c *config.Config) {
	ac := c.Proposal.Actions[config.ActionEject]
	ac.Mode = config.ModeManual
	c.Proposal.Actions[config.ActionEject] = ac
}

// A manual ejection handed off in an earlier cycle may still be broadcast by
// hand: it keeps counting as open across cycles, so min_valset_size cannot be
// bypassed one hand-off per cycle.
func TestMinValsetSizeCountsHandOffsAcrossCycles(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 2, 3, manualEject) // 5 validators; one ejection leaves 4
	rt := strict(f.runtime(t))
	rt.Safety.MaxEligibleShare = 50

	now := f.d.now()
	f.d.evalCycle(ctx, rt)
	f.d.now = func() time.Time { return now.Add(time.Hour) }
	f.d.evalCycle(ctx, rt)

	props, err := f.st.ListProposals(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].Address != "g1bad0" {
		t.Fatalf("proposals = %+v, want only g1bad0 handed off (g1bad1 would leave 3 < 4)\nlogs:\n%s", props, f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "min_valset_size") {
		t.Fatalf("the guardrail must be logged\nlogs:\n%s", f.logs.String())
	}
}

// A hand-off older than the eject cooldown no longer counts as open: the gate
// would let katana propose that validator again.
func TestMinValsetSizeIgnoresStaleHandOffs(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 1, 4, nil) // 5 validators
	rt := strict(f.runtime(t))
	if _, err := f.st.RecordProposal(ctx, store.ProposalRecord{
		CreatedAt: f.d.now().Add(-rt.Proposal.CooldownFor(config.ActionEject) - time.Hour), Chain: "test", ChainID: "dev",
		Address: "g1ok0", Action: config.ActionEject, Status: store.StatusDryRun, Success: true,
	}); err != nil {
		t.Fatal(err)
	}

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 (stale hand-off is not open)\nlogs:\n%s", got, f.logs.String())
	}
}

// Validators in the valset but missing from the gnomonitoring report cannot
// be judged: the mass-eligibility guardrail counts them against arming, so a
// partial report cannot dilute the share.
func TestMassEligibilityCountsValidatorsMissingFromReport(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		missing    []string
		exceptions []string
		upper      bool // valset spells the healthy addresses in upper case
		wantTrip   bool
	}{
		// 1 eligible + 2 missing = 3 of 10 > 25%.
		"partial report trips": {missing: []string{"g1gone0", "g1gone1"}, wantTrip: true},
		// An excepted validator is never proposed: 1 + 1 = 2 of 10.
		"excepted missing does not count": {missing: []string{"g1gone0", "g1gone1"}, exceptions: []string{"g1gone1"}},
		// A full report: 1 of 10, whatever the address case.
		"full report": {upper: true},
	} {
		t.Run(name, func(t *testing.T) {
			healthy := 9 - len(tc.missing)
			f := newValsetFixture(t, 1, healthy, func(c *config.Config) { c.Exceptions = tc.exceptions })
			if tc.upper {
				for i := range f.chain.valset {
					if strings.HasPrefix(f.chain.valset[i], "g1ok") {
						f.chain.valset[i] = strings.ToUpper(f.chain.valset[i])
					}
				}
			}
			f.chain.valset = append(f.chain.valset, tc.missing...)

			f.d.evalCycle(ctx, strict(f.runtime(t)))
			want := 1
			if tc.wantTrip {
				want = 0
			}
			if got := f.broadcasts.count(); got != want {
				t.Fatalf("broadcasts = %d, want %d\nlogs:\n%s", got, want, f.logs.String())
			}
			if tripped := strings.Contains(f.logs.String(), "max_eligible_share"); tripped != tc.wantTrip {
				t.Fatalf("guardrail tripped = %v, want %v\nlogs:\n%s", tripped, tc.wantTrip, f.logs.String())
			}
			if tc.wantTrip {
				if props, _ := f.st.ListProposals(ctx, 10); len(props) != 0 {
					t.Fatalf("proposals = %+v, want none", props)
				}
				if !strings.Contains(f.notifs.all()[0], "3 of 10") {
					t.Fatalf("alert should count the missing validators: %s", f.notifs.all()[0])
				}
			}
		})
	}
}

// The gate applies the candidate action's own cooldown, not proposal.cooldown.
func TestGateUsesActionCooldown(t *testing.T) {
	ctx := context.Background()
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// g1big keeps g1bad's VP share low enough for a real raise.
		big := strings.ReplaceAll(valReport("g1big", 70), "1000", "19000")
		w.Write([]byte("[" + valReport("g1bad", 95) + "," + big + "]"))
	}))
	t.Cleanup(gnomon.Close)
	raiseCooldown := "2160h"
	f := newFixture(t, "dev", func(c *config.Config) {
		c.Gnomonitoring.BaseURL = gnomon.URL
		ac := c.Proposal.Actions[config.ActionRaiseVP]
		ac.Cooldown = &raiseCooldown
		c.Proposal.Actions[config.ActionRaiseVP] = ac
	})
	f.chain.valset = []string{"g1bad", "g1big"}

	// Older than proposal.cooldown (720h), newer than raise_vp's (2160h).
	at := f.d.now().Add(-1000 * time.Hour)
	id, err := f.st.RecordProposal(ctx, store.ProposalRecord{
		CreatedAt: at, Chain: "test", ChainID: "dev", Address: "g1bad",
		Action: config.ActionRaiseVP, Status: store.StatusPending, Success: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateStatus(ctx, id, store.StatusAccepted, at); err != nil {
		t.Fatal(err)
	}

	f.d.evalCycle(ctx, f.runtime(t))
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 (raise_vp cooldown not over)\nlogs:\n%s", got, f.logs.String())
	}

	// Once the raise_vp cooldown is over, the raise goes out.
	now := f.d.now()
	f.d.now = func() time.Time { return now.Add(1200 * time.Hour) }
	f.d.evalCycle(ctx, f.runtime(t))
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 after the raise_vp cooldown\nlogs:\n%s", got, f.logs.String())
	}
}

// The daily cap counts this cycle's broadcasts on top of the stored ones.
func TestMaxProposalsPerDayCountsThisCycle(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 3, 9, nil) // 3 of 12 = 25%, not above
	rt := strict(f.runtime(t))
	rt.Safety.MaxProposalsPerCycle = 3
	rt.Safety.MaxProposalsPerDay = 3
	for i := 0; i < 2; i++ {
		if _, err := f.st.RecordProposal(ctx, store.ProposalRecord{
			CreatedAt: f.d.now().Add(-time.Hour), Chain: "test", ChainID: "dev", Address: fmt.Sprintf("g1other%d", i),
			Status: store.StatusPending, Success: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 (2 in the last 24h, daily cap 3)\nlogs:\n%s", got, f.logs.String())
	}
}

// Only non-excepted, in-valset validators eligible for eject/lower_vp count
// toward max_eligible_share.
func TestMassEligibilityFilters(t *testing.T) {
	ctx := context.Background()
	var reports []string
	for _, addr := range []string{"g1bad0", "g1exc0", "g1exc1", "g1far0", "g1far1"} {
		reports = append(reports, valReport(addr, 10))
	}
	for _, addr := range []string{"g1hi0", "g1hi1"} {
		reports = append(reports, valReport(addr, 95))
	}
	for i := 0; i < 5; i++ {
		reports = append(reports, valReport(fmt.Sprintf("g1ok%d", i), 70))
	}
	body := "[" + strings.Join(reports, ",") + "]"
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(gnomon.Close)
	f := newValsetFixture(t, 1, 5, func(c *config.Config) {
		c.Gnomonitoring.BaseURL = gnomon.URL
		c.Exceptions = []string{"g1exc0", "g1exc1"}
	})
	// 10 validators, 1 counted (g1bad0): excepted (g1exc*), out-of-valset
	// (g1far*) and raise_vp-eligible (g1hi*) ones would each add 2 (30%).
	f.chain.valset = append(f.chain.valset, "g1exc0", "g1exc1", "g1hi0", "g1hi1")

	f.d.evalCycle(ctx, strict(f.runtime(t)))
	if strings.Contains(f.logs.String(), "max_eligible_share") {
		t.Fatalf("guardrail tripped on 1 of 10\nlogs:\n%s", f.logs.String())
	}
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1\nlogs:\n%s", got, f.logs.String())
	}
}

// An ambiguous broadcast may be on-chain: it uses up the cycle's budget.
func TestAmbiguousBroadcastConsumesCaps(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 2, 8, nil)
	f.broadcasts.onCall = func(proposal.Built) {
		if len(f.broadcasts.built) == 1 {
			f.broadcasts.out, f.broadcasts.err = "broadcast tx: broadcasting bytes: request timeout", errors.New("gnokey: exit status 1")
		} else {
			f.broadcasts.out, f.broadcasts.err = "", nil
		}
	}

	f.d.evalCycle(ctx, strict(f.runtime(t)))
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcast attempts = %d, want 1 (the ambiguous one keeps its slot)\nlogs:\n%s", got, f.logs.String())
	}
}

// And it counts as an open ejection for min_valset_size.
func TestAmbiguousEjectionCountsAsOpen(t *testing.T) {
	ctx := context.Background()
	f := newValsetFixture(t, 2, 3, nil) // 5 validators; one ejection leaves 4
	f.broadcasts.onCall = func(proposal.Built) {
		if len(f.broadcasts.built) == 1 {
			f.broadcasts.out, f.broadcasts.err = "broadcast tx: broadcasting bytes: request timeout", errors.New("gnokey: exit status 1")
		} else {
			f.broadcasts.out, f.broadcasts.err = "", nil
		}
	}
	rt := strict(f.runtime(t))
	rt.Safety.MaxProposalsPerCycle = 2
	rt.Safety.MaxEligibleShare = 50

	f.d.evalCycle(ctx, rt)
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcast attempts = %d, want 1 (ambiguous ejection is open)\nlogs:\n%s", got, f.logs.String())
	}
}
