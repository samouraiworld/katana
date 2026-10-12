package gnomonitoring

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

// serveReports starts a gnomonitoring stub serving body on the reports path
// and returns a Client pointed at it.
func serveReports(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/reports/validators" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("chain"); got != "test12" {
			t.Errorf("chain query = %q, want test12", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(config.Gnomonitoring{BaseURL: srv.URL, ReportsPath: "/api/reports/validators", TimeoutSeconds: 5})
}

func TestFetchReports(t *testing.T) {
	c := serveReports(t, `[{"addr":"g1ek7","moniker":"gnocore-val-01","days_since_last_alert":3,
		"periods":{"current_month":{"score":91,"tier":"Excellent","sign_rate":100,
		"proposer_reliability":88.0,"voting_power":1000,"critical_count":1,"warning_count":0,
		"incident_count":1,"incident_rate_per_week":0.47,"downtime_blocks":30,"missed_blocks":0},
		"current_week":{"score":95,"tier":"Excellent","sign_rate":100,"proposer_reliability":null,
		"voting_power":1000,"critical_count":0,"warning_count":0,"incident_count":0,
		"incident_rate_per_week":0,"downtime_blocks":0,"missed_blocks":0},
		"last_24h":{"score":0,"tier":"Critical","voting_power":1000},
		"current_year":{"score":100,"tier":"Excellent","voting_power":0}}}]`)

	reports, err := c.FetchReports(context.Background(), "test12")
	if err != nil {
		t.Fatalf("FetchReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Addr != "g1ek7" || reports[0].Moniker != "gnocore-val-01" {
		t.Fatalf("reports = %+v", reports)
	}
	if d := reports[0].DaysSinceLastAlert; d == nil || *d != 3 {
		t.Errorf("days_since_last_alert = %v, want 3", d)
	}
	month := reports[0].Periods["current_month"]
	if month.Score != 91 || month.Tier != "Excellent" || month.VotingPower != 1000 ||
		month.SignRate != 100 || month.CriticalCount != 1 || month.IncidentCount != 1 ||
		month.IncidentRatePerWeek != 0.47 || month.DowntimeBlocks != 30 {
		t.Errorf("current_month period = %+v", month)
	}
	if month.ProposerReliability == nil || *month.ProposerReliability != 88.0 {
		t.Errorf("current_month proposer_reliability = %v, want 88.0", month.ProposerReliability)
	}
	week := reports[0].Periods["current_week"]
	if week.ProposerReliability != nil {
		t.Errorf("current_week proposer_reliability = %v, want nil (too few samples)", *week.ProposerReliability)
	}
	// An explicit score of 0 is a real (worst) score, not a missing one.
	if day := reports[0].Periods["last_24h"]; day.Score != 0 || day.Tier != "Critical" || day.VotingPower != 1000 {
		t.Errorf("last_24h period = %+v, want explicit score 0", day)
	}
	// An explicit voting_power of 0 is accepted as-is.
	if year := reports[0].Periods["current_year"]; year.Score != 100 || year.VotingPower != 0 {
		t.Errorf("current_year period = %+v", year)
	}
}

// validEntry returns one well-formed report entry, as a generic JSON value the
// malformed cases below mutate.
func validEntry(addr string) map[string]any {
	periods := map[string]any{}
	for _, w := range Windows {
		periods[w] = map[string]any{"score": 80, "tier": "Good", "voting_power": 10}
	}
	return map[string]any{"addr": addr, "moniker": "m-" + addr, "periods": periods}
}

func period(e map[string]any, window string) map[string]any {
	return e["periods"].(map[string]any)[window].(map[string]any)
}

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFetchReportsMalformed(t *testing.T) {
	// mutate builds a two-entry response, applies f to the second entry and
	// encodes it.
	mutate := func(f func(e map[string]any)) func(t *testing.T) string {
		return func(t *testing.T) string {
			second := validEntry("g1bbb")
			f(second)
			return encode(t, []any{validEntry("g1aaa"), second})
		}
	}
	raw := func(s string) func(*testing.T) string { return func(*testing.T) string { return s } }

	tests := []struct {
		name    string
		body    func(t *testing.T) string
		wantMsg []string // substrings the error must name
	}{
		{"object instead of array", raw(`{"addr":"g1aaa"}`), nil},
		{"null body", raw(`null`), nil},
		{"invalid json", raw(`[{"addr":`), nil},
		{"empty array", raw(`[]`), nil},
		{"null entry", raw(`[null]`), nil},
		{"missing addr", mutate(func(e map[string]any) { delete(e, "addr") }), nil},
		{"empty addr", mutate(func(e map[string]any) { e["addr"] = "  " }), nil},
		{"duplicate addr (case-insensitive)", mutate(func(e map[string]any) { e["addr"] = "G1AAA" }), []string{"G1AAA"}},
		{"missing periods", mutate(func(e map[string]any) { delete(e, "periods") }), []string{"g1bbb"}},
		{"null periods", mutate(func(e map[string]any) { e["periods"] = nil }), []string{"g1bbb"}},
		{"missing window", mutate(func(e map[string]any) {
			delete(e["periods"].(map[string]any), "current_month")
		}), []string{"g1bbb", "current_month"}},
		{"null window", mutate(func(e map[string]any) {
			e["periods"].(map[string]any)["last_24h"] = nil
		}), []string{"g1bbb", "last_24h"}},
		{"missing score", mutate(func(e map[string]any) {
			delete(period(e, "current_week"), "score")
		}), []string{"g1bbb", "current_week", "score"}},
		{"null score", mutate(func(e map[string]any) {
			period(e, "current_week")["score"] = nil
		}), []string{"g1bbb", "current_week", "score"}},
		{"negative score", mutate(func(e map[string]any) {
			period(e, "current_year")["score"] = -1
		}), []string{"g1bbb", "current_year", "score"}},
		{"score above 100", mutate(func(e map[string]any) {
			period(e, "current_year")["score"] = 101
		}), []string{"g1bbb", "current_year", "score"}},
		{"missing voting_power", mutate(func(e map[string]any) {
			delete(period(e, "last_24h"), "voting_power")
		}), []string{"g1bbb", "last_24h", "voting_power"}},
		{"null voting_power", mutate(func(e map[string]any) {
			period(e, "last_24h")["voting_power"] = nil
		}), []string{"g1bbb", "last_24h", "voting_power"}},
		{"negative voting_power", mutate(func(e map[string]any) {
			period(e, "current_month")["voting_power"] = -5
		}), []string{"g1bbb", "current_month", "voting_power"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := serveReports(t, tt.body(t))
			reports, err := c.FetchReports(context.Background(), "test12")
			if !errors.Is(err, ErrMalformedReport) {
				t.Fatalf("err = %v, want ErrMalformedReport", err)
			}
			if reports != nil {
				t.Errorf("reports = %+v, want nil (one bad entry rejects the whole response)", reports)
			}
			for _, s := range tt.wantMsg {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("err = %q, want it to name %q", err, s)
				}
			}
		})
	}
}

func TestFetchReportsHTTPErrorIsNotMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(config.Gnomonitoring{BaseURL: srv.URL, ReportsPath: "/api/reports/validators", TimeoutSeconds: 5})
	_, err := c.FetchReports(context.Background(), "test12")
	if err == nil || errors.Is(err, ErrMalformedReport) {
		t.Fatalf("err = %v, want a non-nil transport error that is not ErrMalformedReport", err)
	}
}
