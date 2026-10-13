package daemon

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// staleAfter is how many poll intervals a worker may go without a successful
// cycle before /health reports the daemon unhealthy. The same window is the
// startup grace: a worker that has never succeeded is measured from its start.
const staleAfter = 3

// workerHealth is one worker's liveness record, as served by /health.
type workerHealth struct {
	Chain       string        `json:"chain"`
	Kind        string        `json:"kind"`
	Interval    time.Duration `json:"-"`
	IntervalStr string        `json:"interval"`
	Started     time.Time     `json:"started"`
	LastSuccess time.Time     `json:"last_success,omitzero"`
	LastFailure time.Time     `json:"last_failure,omitzero"`
	LastError   string        `json:"last_error,omitempty"`
	Stale       bool          `json:"stale"`
}

// healthReport is the /health response body.
type healthReport struct {
	Status  string         `json:"status"` // ok | unhealthy
	Workers []workerHealth `json:"workers"`
}

// healthRegistry tracks the last successful cycle of every worker. A cycle is
// successful when it ran to the end with its dependencies (gnomonitoring, the
// chain RPC, the store) answering; a deliberate skip such as a tripped
// guardrail still counts as a success.
type healthRegistry struct {
	now     func() time.Time
	mu      sync.Mutex
	workers map[string]*workerHealth // chain + "/" + kind
}

func newHealthRegistry(now func() time.Time) *healthRegistry {
	return &healthRegistry{now: now, workers: map[string]*workerHealth{}}
}

func (h *healthRegistry) register(chain, kind string, interval time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workers[chain+"/"+kind] = &workerHealth{
		Chain: chain, Kind: kind, Interval: interval, IntervalStr: interval.String(), Started: h.now(),
	}
}

// record stores a cycle outcome; err == nil marks a success.
func (h *healthRegistry) record(chain, kind string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w, ok := h.workers[chain+"/"+kind]
	if !ok {
		return
	}
	if err == nil {
		w.LastSuccess = h.now()
		return
	}
	w.LastFailure = h.now()
	w.LastError = err.Error()
}

// snapshot reports whether every worker succeeded within staleAfter
// intervals, with a copy of each worker's record sorted by chain then kind.
func (h *healthRegistry) snapshot() (bool, []workerHealth) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	healthy := true
	out := make([]workerHealth, 0, len(h.workers))
	for _, w := range h.workers {
		c := *w
		ref := c.LastSuccess
		if ref.IsZero() {
			ref = c.Started
		}
		c.Stale = now.Sub(ref) > staleAfter*c.Interval
		if c.Stale {
			healthy = false
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chain != out[j].Chain {
			return out[i].Chain < out[j].Chain
		}
		return out[i].Kind < out[j].Kind
	})
	return healthy, out
}

// ServeHTTP answers GET /health: 200 when every worker is fresh, 503 when any
// is stale, with the per-worker detail as JSON either way.
func (h *healthRegistry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	healthy, workers := h.snapshot()
	body := healthReport{Status: "ok", Workers: workers}
	code := http.StatusOK
	if !healthy {
		body.Status = "unhealthy"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
