package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
)

func boolPtr(b bool) *bool          { return &b }
func intPtr(i int) *int             { return &i }
func int64Ptr(i int64) *int64       { return &i }
func float64Ptr(f float64) *float64 { return &f }

// stubChain serves the minimal gno.land RPC surface an eval cycle touches:
// /status (chain identity), /validators (consensus valset) and /abci_query
// (valopers realm renders for signing→operator resolution).
type stubChain struct {
	network   string            // node_info.network reported by /status
	valset    []string          // consensus signing addresses
	powers    map[string]int64  // signing address → voting power (default 1000)
	operators map[string]string // operator address → signing address

	mu      sync.Mutex
	renders map[string]string // GovDAO proposal id → realm render
	txs     map[string]string // hex tx hash → DeliverTx events JSON
}

func (s *stubChain) setPower(addr string, power int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.powers == nil {
		s.powers = map[string]int64{}
	}
	s.powers[addr] = power
}

func (s *stubChain) setRender(id, render string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renders == nil {
		s.renders = map[string]string{}
	}
	s.renders[id] = render
}

func (s *stubChain) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			fmt.Fprintf(w, `{"result":{"node_info":{"network":%q},"sync_info":{"latest_block_height":"42"}}}`, s.network)
		case "/validators":
			entries := make([]string, 0, len(s.valset))
			for _, a := range s.valset {
				s.mu.Lock()
				power, ok := s.powers[a]
				s.mu.Unlock()
				if !ok {
					power = 1000
				}
				entries = append(entries, fmt.Sprintf(`{"address":%q,"voting_power":"%d"}`, a, power))
			}
			fmt.Fprintf(w, `{"result":{"block_height":"1","validators":[%s]}}`, strings.Join(entries, ","))
		case "/abci_query":
			// vm/qeval render call; the Render argument is hex-encoded in ?data.
			raw, _ := hex.DecodeString(strings.TrimPrefix(r.URL.Query().Get("data"), "0x"))
			cmd := string(raw)
			var md string
			switch {
			case strings.HasPrefix(cmd, "gno.land/r/gov/dao:"):
				s.mu.Lock()
				md = s.renders[strings.TrimPrefix(cmd, "gno.land/r/gov/dao:")]
				s.mu.Unlock()
			case strings.Contains(cmd, "?page=1"):
				for op := range s.operators {
					md += fmt.Sprintf("- [Val](/r/gnops/valopers:%s)\n", op)
				}
			case strings.Contains(cmd, "?page="):
				// further pages: empty
			default:
				for op, signing := range s.operators {
					if strings.Contains(cmd, op) {
						md = "Signing Address: " + signing
					}
				}
			}
			fmt.Fprintf(w, `{"result":{"response":{"ResponseBase":{"Data":%q}}}}`,
				base64.StdEncoding.EncodeToString([]byte(md)))
		case "/tx":
			s.mu.Lock()
			events, ok := s.txs[strings.TrimPrefix(r.URL.Query().Get("hash"), "0x")]
			s.mu.Unlock()
			if !ok {
				fmt.Fprint(w, `{"error":{"code":-32603,"message":"Internal error","data":"tx not found"}}`)
				return
			}
			fmt.Fprintf(w, `{"result":{"tx_result":{"ResponseBase":{"Events":%s}}}}`, events)
		default:
			http.NotFound(w, r)
		}
	}
}

// notifRecorder captures webhook deliveries.
type notifRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (n *notifRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.bodies = append(n.bodies, string(body))
		n.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func (n *notifRecorder) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.bodies...)
}

// gnokeyOK renders gnokey's output for a successful broadcast that created
// GovDAO proposal id (gno.land/pkg/keyscli PrintTxSuccess).
func gnokeyOK(id string) string {
	return "\nOK!\nGAS WANTED: 1\nGAS USED:   1\nHEIGHT:     42\n" +
		`EVENTS:     [{"type":"ProposalCreated","attrs":[{"key":"id","value":"` + id + `"}],"pkg_path":"gno.land/r/gov/dao"}]` +
		"\nINFO:       \nTX HASH:    q2Vh\n"
}

// broadcastRecorder is a test double for the gnokey broadcast step. By
// default every broadcast succeeds and creates proposal "7"; tests swap out
// and err to simulate gnokey failures.
type broadcastRecorder struct {
	mu    sync.Mutex
	built []proposal.Built
	out   string
	err   error
	// onCall, when set, runs on every broadcast (e.g. to make the chain
	// reflect an executed proposal).
	onCall func(proposal.Built)
}

func (b *broadcastRecorder) broadcast(_ context.Context, built proposal.Built, _ config.Gnokey) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.built = append(b.built, built)
	if b.onCall != nil {
		b.onCall(built)
	}
	if b.out == "" && b.err == nil {
		return gnokeyOK("7"), nil
	}
	return b.out, b.err
}

func (b *broadcastRecorder) set(out string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.out, b.err = out, err
}

func (b *broadcastRecorder) calls() []proposal.Built {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]proposal.Built(nil), b.built...)
}

func (b *broadcastRecorder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.built)
}

// recStore counts writes on top of a real Store, so tests can assert that a
// skipped cycle wrote no state at all.
type recStore struct {
	store.Store
	mu    sync.Mutex
	evals int
}

func (r *recStore) RecordEvaluation(ctx context.Context, rec store.EvalRecord) error {
	r.mu.Lock()
	r.evals++
	r.mu.Unlock()
	return r.Store.RecordEvaluation(ctx, rec)
}

func (r *recStore) evalCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evals
}

type fixture struct {
	d          *Daemon
	cfg        *config.Config
	st         *recStore
	logs       *bytes.Buffer
	notifs     *notifRecorder
	broadcasts *broadcastRecorder
	chain      *stubChain
}

// reportsJSON is the default gnomonitoring report fixture: g1bad has a
// critical score on every window (eject-eligible); tests that need a
// lower_vp/raise_vp candidate override the gnomon server to swap this out
// (see TestEvalCycleLowersVotingPower / TestEvalCycleRaisesVotingPower).
const reportsJSON = `[{"addr":"g1bad","moniker":"bad","periods":{
	"current_month":{"score":10,"tier":"Critical","voting_power":1000},
	"last_24h":{"score":100,"tier":"Excellent","voting_power":1000},
	"current_week":{"score":10,"tier":"Critical","voting_power":1000},
	"current_year":{"score":10,"tier":"Critical","voting_power":1000}
}}]`

// newFixture wires a Daemon against stub gnomonitoring/RPC/webhook servers and
// a real SQLite store, with one eligible validator (g1bad, operator g1op)
// whose default report (reportsJSON) scores low enough for the eject action.
func newFixture(t *testing.T, network string, mutate func(*config.Config)) *fixture {
	t.Helper()

	chain := &stubChain{
		network:   network,
		valset:    []string{"g1bad", "g1peer"}, // g1peer: ejecting g1bad leaves one validator
		operators: map[string]string{"g1op": "g1bad"},
	}
	rpc := httptest.NewServer(chain.handler())
	t.Cleanup(rpc.Close)

	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/reports/validators" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reportsJSON))
	}))
	t.Cleanup(gnomon.Close)

	notifs := &notifRecorder{}
	hook := httptest.NewServer(notifs.handler())
	t.Cleanup(hook.Close)

	cfg := &config.Config{
		PollInterval: "5m",
		// Armed, with guardrails loose enough to stay out of the way of
		// single-validator tests; guardrails_test.go tightens them.
		DryRun: boolPtr(false),
		Safety: config.Safety{
			MaxProposalsPerCycle: intPtr(100), MaxProposalsPerDay: intPtr(100),
			MaxEligibleShare: float64Ptr(100), MinValsetSize: intPtr(1),
		},
		Gnomonitoring: config.Gnomonitoring{
			BaseURL: gnomon.URL, TimeoutSeconds: 5, ReportsPath: "/api/reports/validators",
		},
		Gnokey: config.Gnokey{Bin: "gnokey", KeyName: "test-key", GasFee: "1ugnot", GasWanted: 1},
		Proposal: config.Proposal{
			Cooldown: "720h", RejectedBackoff: "720h", Submit: "run",
			ActionPriority: []string{config.ActionEject, config.ActionLowerVP, config.ActionRaiseVP},
			MinVotingPower: int64Ptr(100),
			// Realistic ceiling, matching the config default. The raise_vp
			// fixture (TestEvalCycleRaisesVotingPower) reports a second,
			// larger validator alongside g1bad so g1bad's share of total
			// valset VP stays well under this ceiling and a genuine
			// (non-clamped) raise occurs.
			MaxVotingPowerShare: float64Ptr(15),
			Actions: map[string]config.ActionConfig{
				config.ActionEject: {
					Mode:       config.ModeAuto,
					ScoreBelow: intPtr(30), Window: "current_month",
					RunScriptTemplate:   "{operator_address} eject {title} {description}",
					TitleTemplate:       "Eject {moniker} ({address})",
					DescriptionTemplate: "Reason: {reason}",
				},
				config.ActionLowerVP: {
					Mode:       config.ModeAuto,
					ScoreBelow: intPtr(60), Window: "current_week",
					VPStepPercentOfSelf: float64Ptr(-20), VPStepPercentOfTotal: float64Ptr(-50), // wide network cap: self-cap decides in this fixture
					RunScriptTemplate:   "{operator_address} lower {target_power} {title} {description}",
					TitleTemplate:       "Lower {moniker} ({address})",
					DescriptionTemplate: "Reason: {reason}",
				},
				config.ActionRaiseVP: {
					Mode:           config.ModeAuto,
					ScoreAtOrAbove: intPtr(85), Window: "current_year",
					VPStepPercentOfSelf: float64Ptr(20), VPStepPercentOfTotal: float64Ptr(50),
					RunScriptTemplate:   "{operator_address} raise {target_power} {title} {description}",
					TitleTemplate:       "Raise {moniker} ({address})",
					DescriptionTemplate: "Reason: {reason}",
				},
			},
		},
		VoteTracking: config.VoteTracking{
			VotePollInterval: "5m", GovdaoRealm: "gno.land/r/gov/dao", PendingTimeout: "336h",
		},
		Notifications: config.Notifications{
			Enabled: true,
			Discord: config.WebhookChannel{Enabled: true, WebhookURL: hook.URL},
		},
		Storage: config.Storage{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "katana.db")},
		Chains: map[string]*config.ChainConfig{
			"test": {
				RPCEndpoint: rpc.URL, ChainID: "dev",
				GnomonitoringChain: "local", Enabled: true,
			},
		},
		ValopersRealm: "gno.land/r/gnops/valopers",
	}
	if mutate != nil {
		mutate(cfg)
	}

	base, err := store.New(cfg.Storage)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { base.Close() })
	st := &recStore{Store: base}

	logs := &bytes.Buffer{}
	d := New(cfg, st, log.New(logs, "", 0))
	br := &broadcastRecorder{}
	d.broadcast = br.broadcast

	return &fixture{d: d, cfg: cfg, st: st, logs: logs, notifs: notifs, broadcasts: br, chain: chain}
}

func (f *fixture) runtime(t *testing.T) config.Runtime {
	t.Helper()
	rt, err := f.cfg.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	return rt
}

func TestStorageWarning(t *testing.T) {
	cfg := &config.Config{
		Storage: config.Storage{Driver: "none"},
		Chains: map[string]*config.ChainConfig{
			"test": {Enabled: true, DryRun: boolPtr(true)},
		},
	}
	w := storageWarning(cfg)
	if w == "" || !strings.Contains(w, "sqlite") {
		t.Fatalf("want a warning pointing at sqlite for driver none + dry-run chains, got %q", w)
	}

	cfg.Storage.Driver = "sqlite"
	if w := storageWarning(cfg); w != "" {
		t.Fatalf("no warning expected with sqlite storage, got %q", w)
	}
}

func TestEvalCycleBroadcasts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)

	f.d.evalCycle(ctx, f.runtime(t))

	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1\nlogs:\n%s", got, f.logs.String())
	}
	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 || props[0].Status != store.StatusPending {
		t.Fatalf("proposals = %+v, err %v; want one pending", props, err)
	}
	if n := len(f.notifs.all()); n != 1 {
		t.Fatalf("notifications = %d, want 1", n)
	}
}

func TestDryRunLogsAndNotifiesWithoutBroadcast(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })

	f.d.evalCycle(ctx, f.runtime(t))

	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 in dry-run mode", got)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "[DRY-RUN]") {
		t.Fatalf("logs missing [DRY-RUN] entry:\n%s", logs)
	}
	// The would-be proposal must be logged completely.
	for _, want := range []string{"g1bad", "g1op", "bad", "Eject bad (g1bad)", "score 10/100"} {
		if !strings.Contains(logs, want) {
			t.Errorf("dry-run log missing %q:\n%s", want, logs)
		}
	}
	bodies := f.notifs.all()
	if len(bodies) != 1 {
		t.Fatalf("notifications = %d, want 1", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, "manual approval") {
		t.Errorf("dry-run notification should say it needs manual approval:\n%s", body)
	}
	// Like the old observe mode, a dry_run notification attaches the ejection
	// script and a ready-to-run gnokey command.
	if !strings.Contains(body, "eject-bad.gno") {
		t.Errorf("dry-run notification missing the attached script filename:\n%s", body)
	}
	if !strings.Contains(body, "gnokey maketx run") {
		t.Errorf("dry-run notification missing the gnokey command:\n%s", body)
	}
	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 || props[0].Status != store.StatusDryRun {
		t.Fatalf("proposals = %+v, err %v; want one dry-run record", props, err)
	}
}

func TestEvalCycleLowersVotingPower(t *testing.T) {
	ctx := context.Background()
	// Override the fixture's default report: score in the lower_vp band
	// (60 > score >= 30... actually strictly below 60 and not below 30).
	// The gnomon server must exist, and its URL must be wired in via the
	// mutate callback, BEFORE newFixture constructs the Daemon: New() reads
	// config.Gnomonitoring by value to build d.gnomon, so assigning
	// f.cfg.Gnomonitoring.BaseURL after the fact (as newFixture's own default
	// gnomon setup might suggest) would have no effect on the already-built
	// client.
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"addr":"g1bad","moniker":"bad","periods":{
			"current_month":{"score":50,"tier":"Watch","voting_power":1000},
			"last_24h":{"score":100,"tier":"Excellent","voting_power":1000},
			"current_week":{"score":50,"tier":"Watch","voting_power":1000},
			"current_year":{"score":50,"tier":"Watch","voting_power":1000}
		}}]`))
	}))
	t.Cleanup(gnomon.Close)
	f := newFixture(t, "dev", func(c *config.Config) {
		c.Chains["test"].Enabled = true
		c.Gnomonitoring.BaseURL = gnomon.URL
	})

	f.d.evalCycle(ctx, f.runtime(t))

	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1\nlogs:\n%s", got, f.logs.String())
	}
	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 {
		t.Fatalf("proposals = %+v, err %v", props, err)
	}
	p := props[0]
	if p.Action != config.ActionLowerVP {
		t.Fatalf("Action = %q, want lower_vp", p.Action)
	}
	if p.CurrentPower != 1000 || p.TargetPower != 800 {
		t.Fatalf("CurrentPower/TargetPower = %d/%d, want 1000/800 (20%% self-cap)", p.CurrentPower, p.TargetPower)
	}
	built := f.broadcasts.built[0]
	if !strings.Contains(built.Script, "800") {
		t.Errorf("built script missing target power 800: %s", built.Script)
	}
}

func TestEvalCycleRaisesVotingPower(t *testing.T) {
	ctx := context.Background()
	// g1bad (1000 VP, raise-eligible) is reported alongside a second, much
	// larger validator (g1big, 19000 VP, neutral score) so the total valset
	// VP is a realistic 20000: g1bad's 5% share stays well under the 15%
	// max_voting_power_share ceiling (proposal.MaxVotingPowerShare in
	// newFixture), and a genuine, non-clamped raise occurs. g1big's score
	// (70, on every window) sits in the 60-84 no-action stability zone so it
	// never itself becomes eligible for eject/lower_vp/raise_vp.
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"addr":"g1bad","moniker":"bad","periods":{
			"current_month":{"score":95,"tier":"Excellent","voting_power":1000},
			"last_24h":{"score":100,"tier":"Excellent","voting_power":1000},
			"current_week":{"score":95,"tier":"Excellent","voting_power":1000},
			"current_year":{"score":95,"tier":"Excellent","voting_power":1000}
		}},{"addr":"g1big","moniker":"big","periods":{
			"current_month":{"score":70,"tier":"Stable","voting_power":19000},
			"last_24h":{"score":100,"tier":"Excellent","voting_power":1000},
			"current_week":{"score":70,"tier":"Stable","voting_power":19000},
			"current_year":{"score":70,"tier":"Stable","voting_power":19000}
		}}]`))
	}))
	t.Cleanup(gnomon.Close)
	f := newFixture(t, "dev", func(c *config.Config) {
		c.Gnomonitoring.BaseURL = gnomon.URL
	})
	// Voting power comes from the RPC valset, not the reports above: g1peer
	// carries the 19000 that makes the live total 20000.
	f.chain.setPower("g1peer", 19000)

	f.d.evalCycle(ctx, f.runtime(t))

	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 {
		t.Fatalf("proposals = %+v, err %v", props, err)
	}
	p := props[0]
	if p.Action != config.ActionRaiseVP || p.TargetPower != 1200 {
		t.Fatalf("Action/TargetPower = %s/%d, want raise_vp/1200", p.Action, p.TargetPower)
	}
}

func TestExceptionsBlockAllThreeActions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		score int
	}{
		{"eject-band", 10},
		{"lower_vp-band", 50},
		{"raise_vp-band", 95},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `[{"addr":"g1bad","moniker":"bad","periods":{
					"current_month":{"score":%d,"voting_power":1000},
					"last_24h":{"score":100,"tier":"Excellent","voting_power":1000},
					"current_week":{"score":%d,"voting_power":1000},
					"current_year":{"score":%d,"voting_power":1000}
				}}]`, tc.score, tc.score, tc.score)
			}))
			t.Cleanup(gnomon.Close)
			f := newFixture(t, "dev", func(c *config.Config) {
				c.Exceptions = []string{"g1bad"}
				c.Gnomonitoring.BaseURL = gnomon.URL
			})

			f.d.evalCycle(ctx, f.runtime(t))

			if got := f.broadcasts.count(); got != 0 {
				t.Fatalf("broadcasts = %d, want 0 (exception must block every action)", got)
			}
		})
	}
}

// TestConsecutiveLowerVPCyclesDoNotCompound confirms the fixed-step VP model
// doesn't compound the WRONG way across consecutive cycles: rules.vpTarget
// (Task 3) computes each cycle's step as 20% of that cycle's live current
// power, not a fixed absolute amount off the original power. So repeated
// -20%-of-self cycles decay geometrically (1000 -> 800 -> 640 -> 512), not
// arithmetically (1000 -> 800 -> 600 -> 400) — the "no compounding" property
// the spec asks for is that a *single* proposal's target is a fixed step
// off the power at propose time, not that the target sequence across
// accepted proposals is a flat arithmetic decrement.
func TestConsecutiveLowerVPCyclesDoNotCompound(t *testing.T) {
	ctx := context.Background()

	// gnomonitoring keeps reporting a stale 1000: only the live RPC valset
	// moves, and it alone must drive each step.
	power := int64(1000)
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"addr":"g1bad","moniker":"bad","periods":{
			"current_month":{"score":50,"voting_power":1000},
			"last_24h":{"score":100,"tier":"Excellent","voting_power":1000},
			"current_week":{"score":50,"voting_power":1000},
			"current_year":{"score":50,"voting_power":1000}
		}}]`))
	}))
	t.Cleanup(gnomon.Close)
	f := newFixture(t, "dev", func(c *config.Config) {
		// No cooldown floor between cycles, so this test can drive three
		// evaluations back to back and observe three broadcasts.
		c.Proposal.Cooldown = "0s"
		c.Gnomonitoring.BaseURL = gnomon.URL
	})

	wantTargets := []int64{800, 640, 512} // 20%-of-current-power step each cycle, off the live current power
	for i, want := range wantTargets {
		f.d.evalCycle(ctx, f.runtime(t))
		props, err := f.st.ListProposals(ctx, 1)
		if err != nil || len(props) != 1 {
			t.Fatalf("cycle %d: proposals = %+v, err %v", i, props, err)
		}
		if props[0].TargetPower != want {
			t.Fatalf("cycle %d: TargetPower = %d, want %d (current power was %d)", i, props[0].TargetPower, want, power)
		}
		// A still-pending proposal blocks the lifecycle gate unconditionally
		// (see lifecycle.Gate's StatusPending case), regardless of cooldown.
		// Actually simulate the accepted-on-chain outcome the next cycle
		// needs, so the gate's lower_vp/raise_vp non-terminal path (Task 5)
		// is what's exercised instead of just re-observing cycle 1's row.
		if err := f.st.UpdateStatus(ctx, props[0].ID, store.StatusAccepted, f.d.now()); err != nil {
			t.Fatalf("cycle %d: UpdateStatus: %v", i, err)
		}
		power = props[0].TargetPower // simulate the proposal having been accepted on-chain
		f.chain.setPower("g1bad", power)
	}
}

func TestDryRunRepeatedCyclesDoNotSpam(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })
	rt := f.runtime(t)

	f.d.evalCycle(ctx, rt)
	f.d.evalCycle(ctx, rt)
	f.d.evalCycle(ctx, rt)

	if n := len(f.notifs.all()); n != 1 {
		t.Fatalf("notifications = %d, want 1 (dedup across cycles)", n)
	}
	props, _ := f.st.ListProposals(ctx, 10)
	if len(props) != 1 {
		t.Fatalf("proposal records = %d, want 1 (dedup across cycles)", len(props))
	}
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0", got)
	}
}

func TestChainIDMismatchSkipsCycle(t *testing.T) {
	ctx := context.Background()
	// The node reports a different network id than the configured chain_id
	// ("dev") — the situation after a testnet genesis reset.
	f := newFixture(t, "test999", nil)

	f.d.evalCycle(ctx, f.runtime(t))

	logs := f.logs.String()
	if !strings.Contains(logs, `configured chain_id "dev" but node reports "test999"`) {
		t.Fatalf("logs missing the chain-id mismatch error:\n%s", logs)
	}
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 on chain-id mismatch", got)
	}
	if n := len(f.notifs.all()); n != 0 {
		t.Fatalf("notifications = %d, want 0 on chain-id mismatch", n)
	}
	// The cycle is skipped cleanly: no state of any kind is written.
	if n := f.st.evalCount(); n != 0 {
		t.Fatalf("evaluation records = %d, want 0 (cycle must be skipped before state writes)", n)
	}
	if props, _ := f.st.ListProposals(ctx, 10); len(props) != 0 {
		t.Fatalf("proposals = %+v, want none", props)
	}
}

func TestChainIDMismatchSkipsDryRunToo(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "test999", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })

	f.d.evalCycle(ctx, f.runtime(t))

	if strings.Contains(f.logs.String(), "[DRY-RUN] would broadcast") {
		t.Fatalf("dry-run proposal logged despite chain-id mismatch:\n%s", f.logs.String())
	}
	if n := len(f.notifs.all()); n != 0 {
		t.Fatalf("notifications = %d, want 0 on chain-id mismatch", n)
	}
}

func TestDryRunRecordCountsTowardCooldown(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })

	// Rehearse once, then arm the chain immediately.
	f.d.evalCycle(ctx, f.runtime(t))
	f.cfg.Chains["test"].DryRun = boolPtr(false)
	f.d.evalCycle(ctx, f.runtime(t))

	// The dry-run record counts toward the real lifecycle gate exactly like a
	// broadcast attempt would: the cooldown (720h in the fixture) is still in
	// effect immediately after, so disabling dry_run does not by itself
	// produce a real broadcast.
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0: the cooldown from the dry-run record should still apply\nlogs:\n%s", got, f.logs.String())
	}
	props, _ := f.st.ListProposals(ctx, 10)
	if len(props) != 1 || props[0].Status != store.StatusDryRun {
		t.Fatalf("proposals = %+v; want only the original dry-run record (no new one produced yet)", props)
	}

	// Once the cooldown has elapsed, the real proposal goes out normally.
	f.d.now = func() time.Time { return time.Now().Add(800 * time.Hour) }
	f.d.evalCycle(ctx, f.runtime(t))
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 after the cooldown elapses\nlogs:\n%s", got, f.logs.String())
	}
}
