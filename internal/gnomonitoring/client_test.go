package gnomonitoring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

func TestFetchReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/reports/validators" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("chain"); got != "test12" {
			t.Errorf("chain query = %q, want test12", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"addr":"g1ek7","moniker":"gnocore-val-01","days_since_last_alert":3,
			"periods":{"current_month":{"score":91,"tier":"Excellent","sign_rate":100,
			"proposer_reliability":88.0,"voting_power":1000,"critical_count":1,"warning_count":0,
			"incident_count":1,"incident_rate_per_week":0.47,"downtime_blocks":30,"missed_blocks":0},
			"current_week":{"score":95,"tier":"Excellent","sign_rate":100,"proposer_reliability":null,
			"voting_power":1000,"critical_count":0,"warning_count":0,"incident_count":0,
			"incident_rate_per_week":0,"downtime_blocks":0,"missed_blocks":0}}}]`))
	}))
	defer srv.Close()

	c := New(config.Gnomonitoring{BaseURL: srv.URL, ReportsPath: "/api/reports/validators", TimeoutSeconds: 5})
	reports, err := c.FetchReports(context.Background(), "test12")
	if err != nil {
		t.Fatalf("FetchReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Addr != "g1ek7" || reports[0].Moniker != "gnocore-val-01" {
		t.Fatalf("reports = %+v", reports)
	}
	month := reports[0].Periods["current_month"]
	if month.Score != 91 || month.Tier != "Excellent" || month.VotingPower != 1000 {
		t.Errorf("current_month period = %+v", month)
	}
	if month.ProposerReliability == nil || *month.ProposerReliability != 88.0 {
		t.Errorf("current_month proposer_reliability = %v, want 88.0", month.ProposerReliability)
	}
	week := reports[0].Periods["current_week"]
	if week.ProposerReliability != nil {
		t.Errorf("current_week proposer_reliability = %v, want nil (too few samples)", *week.ProposerReliability)
	}
}
