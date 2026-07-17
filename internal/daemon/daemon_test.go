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

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
)

// stubChain serves the minimal gno.land RPC surface an eval cycle touches:
// /status (chain identity), /validators (consensus valset) and /abci_query
// (valopers realm renders for signing→operator resolution).
type stubChain struct {
	network   string            // node_info.network reported by /status
	valset    []string          // consensus signing addresses
	operators map[string]string // operator address → signing address
}

func (s *stubChain) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			fmt.Fprintf(w, `{"result":{"node_info":{"network":%q}}}`, s.network)
		case "/validators":
			entries := make([]string, 0, len(s.valset))
			for _, a := range s.valset {
				entries = append(entries, fmt.Sprintf(`{"address":%q,"voting_power":"1"}`, a))
			}
			fmt.Fprintf(w, `{"result":{"block_height":"1","validators":[%s],"total":"%d"}}`,
				strings.Join(entries, ","), len(s.valset))
		case "/abci_query":
			// vm/qeval render call; the Render argument is hex-encoded in ?data.
			raw, _ := hex.DecodeString(strings.TrimPrefix(r.URL.Query().Get("data"), "0x"))
			cmd := string(raw)
			var md string
			switch {
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

// broadcastRecorder is a test double for the gnokey broadcast step.
type broadcastRecorder struct {
	mu    sync.Mutex
	built []proposal.Built
}

func (b *broadcastRecorder) broadcast(_ context.Context, built proposal.Built, _ config.Gnokey) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.built = append(b.built, built)
	return "broadcast ok", nil
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
}

// newFixture wires a Daemon against stub gnomonitoring/RPC/webhook servers and
// a real SQLite store, with one eligible validator (g1bad, operator g1op) that
// misses more blocks than the threshold.
func newFixture(t *testing.T, network string, mutate func(*config.Config)) *fixture {
	t.Helper()

	chain := &stubChain{
		network:   network,
		valset:    []string{"g1bad"},
		operators: map[string]string{"g1op": "g1bad"},
	}
	rpc := httptest.NewServer(chain.handler())
	t.Cleanup(rpc.Close)

	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/missing_block" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[{"moniker":"bad","addr":"g1bad","missingBlock":500}]`)
	}))
	t.Cleanup(gnomon.Close)

	notifs := &notifRecorder{}
	hook := httptest.NewServer(notifs.handler())
	t.Cleanup(hook.Close)

	cfg := &config.Config{
		PollInterval: "5m",
		Gnomonitoring: config.Gnomonitoring{
			BaseURL: gnomon.URL, TimeoutSeconds: 5,
			MissedBlocksPath: "/missing_block", AlertsPath: "/admin/alerts",
		},
		Gnokey: config.Gnokey{Bin: "gnokey", KeyName: "test-key", GasFee: "1ugnot", GasWanted: 1},
		Conditions: config.Conditions{
			MissedBlocks: config.MissedBlocksCond{Enabled: true, Max: 100, Interval: "current_week"},
		},
		Proposal: config.Proposal{
			Cooldown: "720h", RejectedBackoff: "720h", Submit: "run",
			RunScriptTemplate:   "{operator_address} {title} {description}",
			TitleTemplate:       "Eject {moniker} ({address})",
			DescriptionTemplate: "Reason: {reason}",
		},
		VoteTracking: config.VoteTracking{VotePollInterval: "5m"},
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

	return &fixture{d: d, cfg: cfg, st: st, logs: logs, notifs: notifs, broadcasts: br}
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
			"test": {Enabled: true, DryRun: true},
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
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = true })

	f.d.evalCycle(ctx, f.runtime(t))

	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 in dry-run mode", got)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "[DRY-RUN]") {
		t.Fatalf("logs missing [DRY-RUN] entry:\n%s", logs)
	}
	// The would-be proposal must be logged completely.
	for _, want := range []string{"g1bad", "g1op", "bad", "Eject bad (g1bad)", "missed 500 blocks"} {
		if !strings.Contains(logs, want) {
			t.Errorf("dry-run log missing %q:\n%s", want, logs)
		}
	}
	bodies := f.notifs.all()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "[DRY-RUN]") {
		t.Fatalf("want one DRY-RUN-labeled notification, got %+v", bodies)
	}
	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 || props[0].Status != store.StatusDryRun {
		t.Fatalf("proposals = %+v, err %v; want one dry-run record", props, err)
	}
}

func TestDryRunRepeatedCyclesDoNotSpam(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = true })
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

func TestDryRunRecordDoesNotBlockRealProposal(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = true })

	// Rehearse once, then arm the chain.
	f.d.evalCycle(ctx, f.runtime(t))
	f.cfg.Chains["test"].DryRun = false
	f.d.evalCycle(ctx, f.runtime(t))

	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 after disabling dry_run\nlogs:\n%s", got, f.logs.String())
	}
	props, _ := f.st.ListProposals(ctx, 10)
	if len(props) != 2 || props[0].Status != store.StatusPending || props[1].Status != store.StatusDryRun {
		t.Fatalf("proposals = %+v; want [pending, dry-run]", props)
	}
}
