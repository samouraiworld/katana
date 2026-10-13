// Package daemon is katana's long-running core. For every enabled chain it runs
// two workers on independent tickers: an evaluation worker that finds bad
// validators and creates ejection proposals, and a vote-watcher worker that
// follows the GovDAO outcome of broadcast proposals. It exposes an optional
// /health endpoint and shuts down gracefully on context cancellation.
package daemon

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnomonitoring"
	"github.com/samouraiworld/katana/internal/notify"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
	"github.com/samouraiworld/katana/internal/valopers"
)

// Daemon owns the shared dependencies and drives the per-chain workers.
type Daemon struct {
	cfg      *config.Config
	st       store.Store
	gnomon   *gnomonitoring.Client
	notifier *notify.Notifier
	logger   *log.Logger
	now      func() time.Time
	// broadcast signs and submits a built proposal via gnokey. It is a field so
	// tests can substitute a double; production uses proposal.Built.Broadcast.
	broadcast func(ctx context.Context, b proposal.Built, gk config.Gnokey) (string, error)
	// txMu serializes every gnokey transaction: the eval and vote workers
	// sign with the same key, and two concurrent txs would race on the
	// account sequence (the loser fails CheckTx).
	txMu sync.Mutex
	// outcomeTimeout bounds recording and notifying a broadcast outcome
	// once the worker ctx may be cancelled (see detach).
	outcomeTimeout time.Duration

	execMu   sync.Mutex
	executed map[int64]*executeState // proposal row id → ExecuteOrRejectProposal attempts

	resolversMu sync.Mutex
	resolvers   map[string]*valopers.Resolver // chain key → operator resolver

	guardMu      sync.Mutex
	guardTripped map[string]bool // chain key → mass-eligibility guardrail tripped last cycle
}

// New builds a Daemon from the loaded config and an open store.
func New(cfg *config.Config, st store.Store, logger *log.Logger) *Daemon {
	return &Daemon{
		cfg:            cfg,
		st:             st,
		gnomon:         gnomonitoring.New(cfg.Gnomonitoring),
		notifier:       notify.New(cfg.Notifications),
		logger:         logger,
		now:            time.Now,
		outcomeTimeout: 30 * time.Second,
		broadcast: func(ctx context.Context, b proposal.Built, gk config.Gnokey) (string, error) {
			return b.Broadcast(ctx, gk)
		},
		resolvers: map[string]*valopers.Resolver{},
		executed:  map[int64]*executeState{},
	}
}

// signAndBroadcast runs one gnokey transaction at a time.
func (d *Daemon) signAndBroadcast(ctx context.Context, b proposal.Built, gk config.Gnokey) (string, error) {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	return d.broadcast(ctx, b, gk)
}

// resolverFor lazily builds and caches the operator resolver for a chain.
func (d *Daemon) resolverFor(rt config.Runtime) *valopers.Resolver {
	d.resolversMu.Lock()
	defer d.resolversMu.Unlock()
	if r, ok := d.resolvers[rt.Key]; ok {
		return r
	}
	r := valopers.New(rt.RPCEndpoint, d.cfg.ValopersRealm, 10*time.Minute)
	d.resolvers[rt.Key] = r
	return r
}

// Run starts every worker and blocks until ctx is cancelled, then waits for the
// workers to stop.
func (d *Daemon) Run(ctx context.Context) error {
	chains := d.cfg.EnabledChains()
	d.logger.Printf("katana daemon starting — chains=%v storage=%s", chains, d.cfg.Storage.Driver)
	if w := storageWarning(d.cfg); w != "" {
		d.logger.Print(w)
	}

	var wg sync.WaitGroup

	// Optional health server.
	var srv *http.Server
	if d.cfg.Web.Enabled {
		srv = d.startHealthServer()
	}

	for _, chainKey := range chains {
		rt, err := d.cfg.Runtime(chainKey)
		if err != nil {
			d.logger.Printf("[%s] skipping: %v", chainKey, err)
			continue
		}
		wg.Add(2)
		go func(rt config.Runtime) {
			defer wg.Done()
			d.runWorker(ctx, "eval", rt.Key, rt.PollInterval, func(c context.Context) {
				d.evalCycle(c, rt)
			})
		}(rt)
		go func(rt config.Runtime) {
			defer wg.Done()
			d.runWorker(ctx, "vote", rt.Key, rt.VotePollInterval, func(c context.Context) {
				d.voteCycle(c, rt)
			})
		}(rt)
	}

	<-ctx.Done()
	d.logger.Printf("shutdown requested, stopping workers…")

	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutCtx)
		cancel()
	}

	wg.Wait()
	d.logger.Printf("all workers stopped")
	return nil
}

// storageWarning describes the degradation of running without persistence, or
// returns "" when none applies. Config validation already refuses to start a
// broadcast-capable chain on the Noop store, so this only concerns
// dry-run-only configurations.
func storageWarning(cfg *config.Config) string {
	if cfg.Storage.Driver != "none" {
		return ""
	}
	return "WARNING: storage.driver \"none\" keeps no history: dry-run dedup state is lost every cycle, so the same rehearsal will be re-logged and re-notified on every poll; configure storage.driver: sqlite (required before any chain is armed)"
}

// runWorker runs cycle immediately, then on every tick, until ctx is cancelled.
// A panic or error inside cycle must not stop the loop (cycle handles its own
// errors); the loop only exits on cancellation.
func (d *Daemon) runWorker(ctx context.Context, kind, chainKey string, interval time.Duration, cycle func(context.Context)) {
	d.logger.Printf("[%s][%s] worker started (every %s)", chainKey, kind, interval)
	cycle(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.logger.Printf("[%s][%s] worker stopped", chainKey, kind)
			return
		case <-ticker.C:
			cycle(ctx)
		}
	}
}

// startHealthServer serves GET /health for external liveness monitoring.
func (d *Daemon) startHealthServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: d.cfg.Web.Listen, Handler: mux}
	go func() {
		d.logger.Printf("health server listening on %s (/health)", d.cfg.Web.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			d.logger.Printf("health server error: %v", err)
		}
	}()
	return srv
}
