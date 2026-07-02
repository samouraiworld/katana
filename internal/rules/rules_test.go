package rules

import (
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

func mb(max int) config.MissedBlocksCond {
	return config.MissedBlocksCond{Enabled: true, Max: max, Interval: "current_week"}
}
func ca(max int) config.CriticalCond {
	return config.CriticalCond{Enabled: true, Max: max, Interval: "168h"}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name         string
		cond         config.Conditions
		metrics      Metrics
		wantEligible bool
	}{
		{
			name:         "no condition enabled is never eligible",
			cond:         config.Conditions{},
			metrics:      Metrics{MissedBlocks: 999, CriticalAlerts: 999},
			wantEligible: false,
		},
		{
			name:         "missed blocks strictly above max triggers",
			cond:         config.Conditions{MissedBlocks: mb(100)},
			metrics:      Metrics{MissedBlocks: 101},
			wantEligible: true,
		},
		{
			name:         "missed blocks equal to max does not trigger",
			cond:         config.Conditions{MissedBlocks: mb(100)},
			metrics:      Metrics{MissedBlocks: 100},
			wantEligible: false,
		},
		{
			name:         "OR: one of two triggers is enough",
			cond:         config.Conditions{RequireAll: false, MissedBlocks: mb(100), CriticalAlerts: ca(3)},
			metrics:      Metrics{MissedBlocks: 0, CriticalAlerts: 4},
			wantEligible: true,
		},
		{
			name:         "AND: one trigger is not enough",
			cond:         config.Conditions{RequireAll: true, MissedBlocks: mb(100), CriticalAlerts: ca(3)},
			metrics:      Metrics{MissedBlocks: 200, CriticalAlerts: 1},
			wantEligible: false,
		},
		{
			name:         "AND: both triggers is eligible",
			cond:         config.Conditions{RequireAll: true, MissedBlocks: mb(100), CriticalAlerts: ca(3)},
			metrics:      Metrics{MissedBlocks: 200, CriticalAlerts: 5},
			wantEligible: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.metrics, tt.cond)
			if got.Eligible != tt.wantEligible {
				t.Fatalf("Eligible = %v, want %v (reasons: %v)", got.Eligible, tt.wantEligible, got.Reasons)
			}
		})
	}
}
