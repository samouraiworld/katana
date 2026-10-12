package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/store"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time               { return c.t }
func (c *clock) advance(d time.Duration)      { c.t = c.t.Add(d) }
func newClock() *clock                        { return &clock{t: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)} }
func healthOf(h *healthRegistry) bool         { ok, _ := h.snapshot(); return ok }
func workerOf(h *healthRegistry) workerHealth { _, ws := h.snapshot(); return ws[0] }

func TestHealthStartupGrace(t *testing.T) {
	c := newClock()
	h := newHealthRegistry(c.now)
	h.register("test", "eval", time.Minute)

	c.advance(3 * time.Minute)
	if !healthOf(h) {
		t.Fatal("unhealthy inside the startup grace window")
	}
	c.advance(time.Second)
	if healthOf(h) {
		t.Fatal("still healthy after 3 intervals without a successful cycle")
	}
}

func TestHealthStaleAfterSuccess(t *testing.T) {
	c := newClock()
	h := newHealthRegistry(c.now)
	h.register("test", "vote", time.Minute)

	c.advance(2 * time.Minute)
	h.record("test", "vote", nil)
	c.advance(3 * time.Minute)
	h.record("test", "vote", errors.New("rpc down")) // failures do not refresh
	if !healthOf(h) {
		t.Fatal("unhealthy exactly 3 intervals after the last success")
	}
	c.advance(time.Second)
	if healthOf(h) {
		t.Fatal("healthy although the last success is older than 3 intervals")
	}
	if w := workerOf(h); w.LastError != "rpc down" || w.LastFailure.IsZero() {
		t.Fatalf("last failure not reported: %+v", w)
	}

	h.record("test", "vote", nil)
	if !healthOf(h) {
		t.Fatal("a successful cycle must restore health")
	}
}

// One stale worker makes the whole daemon unhealthy.
func TestHealthAnyStaleWorker(t *testing.T) {
	c := newClock()
	h := newHealthRegistry(c.now)
	h.register("a", "eval", time.Minute)
	h.register("b", "eval", time.Hour)

	c.advance(10 * time.Minute)
	h.record("b", "eval", nil)
	if healthOf(h) {
		t.Fatal("healthy although chain a's eval worker is stale")
	}
}

func TestHealthHandler(t *testing.T) {
	c := newClock()
	h := newHealthRegistry(c.now)
	h.register("test", "eval", time.Minute)
	h.record("test", "eval", nil)

	get := func() (int, healthReport) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		var body healthReport
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding %q: %v", rec.Body.String(), err)
		}
		return rec.Code, body
	}

	code, body := get()
	if code != http.StatusOK || body.Status != "ok" || len(body.Workers) != 1 || body.Workers[0].Stale {
		t.Fatalf("healthy: code=%d body=%+v", code, body)
	}

	c.advance(4 * time.Minute)
	code, body = get()
	if code != http.StatusServiceUnavailable || body.Status != "unhealthy" || !body.Workers[0].Stale {
		t.Fatalf("stale: code=%d body=%+v", code, body)
	}
}

// A panicking cycle is recovered: it is logged with its stack, counted as a
// failed cycle, and the worker keeps ticking.
func TestRunWorkerRecoversPanic(t *testing.T) {
	var logs bytes.Buffer
	d := &Daemon{logger: log.New(&logs, "", 0), now: time.Now}
	d.health = newHealthRegistry(d.now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.runWorker(ctx, "eval", "test", 5*time.Millisecond, func(context.Context) error {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			cancel()
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not keep running after a panic")
	}
	if calls.Load() < 2 {
		t.Fatalf("cycle ran %d time(s), want a second run after the panic", calls.Load())
	}
	out := logs.String()
	if !strings.Contains(out, "PANIC") || !strings.Contains(out, "boom") || !strings.Contains(out, "goroutine") {
		t.Fatalf("panic and stack not logged:\n%s", out)
	}
	if w := workerOf(d.health); !strings.Contains(w.LastError, "boom") {
		t.Fatalf("panic not recorded as a failed cycle: %+v", w)
	}
}

// A cycle cut short by shutdown is not recorded as a failure.
func TestRunWorkerIgnoresCancelledCycle(t *testing.T) {
	d := &Daemon{logger: log.New(&bytes.Buffer{}, "", 0), now: time.Now}
	d.health = newHealthRegistry(d.now)

	ctx, cancel := context.WithCancel(context.Background())
	d.runWorker(ctx, "vote", "test", time.Hour, func(context.Context) error {
		cancel()
		return context.Canceled
	})
	if w := workerOf(d.health); !w.LastFailure.IsZero() {
		t.Fatalf("cancelled cycle recorded as a failure: %+v", w)
	}
}

// A successful eval cycle reports nil; an RPC that serves an unreadable valset
// fails it (and proposes nothing).
func TestEvalCycleOutcome(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)
	if err := f.d.evalCycle(ctx, f.runtime(t)); err != nil {
		t.Fatalf("healthy cycle returned %v\nlogs:\n%s", err, f.logs.String())
	}

	f = newFixture(t, "dev", nil)
	f.chain.setPower("g1peer", 0) // RPC serves voting_power "0": unreadable
	if err := f.d.evalCycle(ctx, f.runtime(t)); err == nil {
		t.Fatal("cycle with an unreadable valset returned nil")
	}
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0", got)
	}

	f = newFixture(t, "other-network", nil)
	if err := f.d.evalCycle(ctx, f.runtime(t)); err == nil {
		t.Fatal("cycle on a mismatched chain id returned nil")
	}
}

// The vote cycle fails when it cannot read a tracked proposal's status.
func TestVoteCycleOutcome(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)
	rt := f.runtime(t)
	if err := f.d.voteCycle(ctx, rt); err != nil {
		t.Fatalf("vote cycle with nothing tracked returned %v", err)
	}

	seed(t, f, "7", "", store.StatusPending, time.Minute)
	f.chain.setRender("7", "- **Proposal is open for votes**\n- YES PERCENT: 0%\n- NO PERCENT: 0%\n")
	if err := f.d.voteCycle(ctx, rt); err != nil {
		t.Fatalf("vote cycle on a readable open proposal returned %v\nlogs:\n%s", err, f.logs.String())
	}

	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	rt.RPCEndpoint = down.URL
	if err := f.d.voteCycle(ctx, rt); err == nil {
		t.Fatal("vote cycle with an unreachable RPC returned nil")
	}
}
