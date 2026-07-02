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
	"github.com/samouraiworld/katana/internal/store"
)

// Daemon owns the shared dependencies and drives the per-chain workers.
type Daemon struct {
	cfg      *config.Config
	st       store.Store
	gnomon   *gnomonitoring.Client
	notifier *notify.Notifier
	logger   *log.Logger
	now      func() time.Time
}

// New builds a Daemon from the loaded config and an open store.
func New(cfg *config.Config, st store.Store, logger *log.Logger) *Daemon {
	return &Daemon{
		cfg:      cfg,
		st:       st,
		gnomon:   gnomonitoring.New(cfg.Gnomonitoring),
		notifier: notify.New(cfg.Notifications),
		logger:   logger,
		now:      time.Now,
	}
}

// Run starts every worker and blocks until ctx is cancelled, then waits for the
// workers to stop.
func (d *Daemon) Run(ctx context.Context) error {
	chains := d.cfg.EnabledChains()
	d.logger.Printf("katana daemon starting — chains=%v storage=%s", chains, d.cfg.Storage.Driver)

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
