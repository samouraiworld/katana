package rules

import (
	"testing"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnomonitoring"
)

func actions() map[string]config.ActionConfig {
	return map[string]config.ActionConfig{
		config.ActionEject: {ScoreBelow: 30, Window: "current_month"},
		config.ActionLowerVP: {
			ScoreBelow: 60, Window: "current_week",
			VPStepPercentOfSelf: -20, VPStepPercentOfTotal: -2,
		},
		config.ActionRaiseVP: {
			ScoreAtOrAbove: 85, Window: "current_year",
			VPStepPercentOfSelf: 20, VPStepPercentOfTotal: 2,
		},
	}
}

func defaultPriority() []string {
	return []string{config.ActionEject, config.ActionLowerVP, config.ActionRaiseVP}
}

func report(addr string, periods map[string]gnomonitoring.PeriodScore) gnomonitoring.Report {
	return gnomonitoring.Report{Addr: addr, Moniker: "m-" + addr, Periods: periods}
}

func TestEvaluate_ActionThresholds(t *testing.T) {
	tests := []struct {
		name       string
		r          gnomonitoring.Report
		wantAction string
	}{
		{
			name: "score below eject threshold on its window",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 29, VotingPower: 1000},
			}),
			wantAction: config.ActionEject,
		},
		{
			name: "score at eject threshold does not trigger (strict <)",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 30, VotingPower: 1000},
				"current_week":  {Score: 60, VotingPower: 1000}, // also not < 60, no lower_vp either
			}),
			wantAction: "",
		},
		{
			name: "score in the 60-84 stability zone has no action",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 70, VotingPower: 1000},
				"current_week":  {Score: 70, VotingPower: 1000},
				"current_year":  {Score: 70, VotingPower: 1000},
			}),
			wantAction: "",
		},
		{
			name: "score at raise threshold triggers (>=)",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 90, VotingPower: 1000},
				"current_week":  {Score: 90, VotingPower: 1000},
				"current_year":  {Score: 85, VotingPower: 1000},
			}),
			wantAction: config.ActionRaiseVP,
		},
	}
	totalVP := map[string]int64{"current_month": 20000, "current_week": 20000, "current_year": 20000}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.r, defaultPriority(), actions(), 100, 15, totalVP)
			if got.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q (reason: %s)", got.Action, tt.wantAction, got.Reason)
			}
		})
	}
}

func TestEvaluate_PriorityOrderIsConfigurable(t *testing.T) {
	// A validator whose current_month score also happens to clear the
	// eject band AND (independently) whose current_year score clears the
	// raise band — reordering priority changes which one wins.
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_month": {Score: 10, VotingPower: 1000},
		"current_year":  {Score: 90, VotingPower: 1000},
	})
	totalVP := map[string]int64{"current_month": 20000, "current_year": 20000}

	got := Evaluate(r, []string{config.ActionEject, config.ActionRaiseVP}, actions(), 100, 15, totalVP)
	if got.Action != config.ActionEject {
		t.Fatalf("default order: Action = %q, want eject", got.Action)
	}

	got = Evaluate(r, []string{config.ActionRaiseVP, config.ActionEject}, actions(), 100, 15, totalVP)
	if got.Action != config.ActionRaiseVP {
		t.Fatalf("reordered priority: Action = %q, want raise_vp", got.Action)
	}
}

func TestEvaluate_DualCapStep(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}

	// Small validator: self-cap (20% of 100 = 20) is tighter than the
	// network cap (2% of 20000 = 400) — self-cap wins.
	small := report("g1small", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 100},
	})
	got := Evaluate(small, defaultPriority(), actions(), 10, 15, totalVP)
	if got.Action != config.ActionLowerVP || got.TargetPower != 80 {
		t.Fatalf("small validator: Action=%q TargetPower=%d, want lower_vp 80", got.Action, got.TargetPower)
	}

	// Large validator: self-cap (20% of 5000 = 1000) is looser than the
	// network cap (2% of 20000 = 400) — network cap wins.
	large := report("g1large", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 5000},
	})
	got = Evaluate(large, defaultPriority(), actions(), 10, 15, totalVP)
	if got.Action != config.ActionLowerVP || got.TargetPower != 4600 {
		t.Fatalf("large validator: Action=%q TargetPower=%d, want lower_vp 4600", got.Action, got.TargetPower)
	}
}

func TestEvaluate_ClampToMinVotingPower(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 110}, // step would be -20 (self-cap), -> 90, below floor 100
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != config.ActionLowerVP || got.TargetPower != 100 {
		t.Fatalf("Action=%q TargetPower=%d, want lower_vp clamped to floor 100", got.Action, got.TargetPower)
	}
}

func TestEvaluate_NoopClampClearsAction(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 100}, // already at the floor
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != "" {
		t.Fatalf("Action = %q, want \"\" (no-op: already at min_voting_power)", got.Action)
	}
}

// TestEvaluate_RaiseAboveCeilingIsNoop guards against vpTarget's floor/ceiling
// clamp reversing the action's direction: a raise_vp-eligible validator whose
// current power is already above max_voting_power_share% of total must not
// produce a "raise_vp" decision whose TargetPower is actually lower than
// CurrentPower.
func TestEvaluate_RaiseAboveCeilingIsNoop(t *testing.T) {
	totalVP := map[string]int64{"current_year": 10000}
	// current=5000 is 50% of total(10000), already well above the 15%
	// ceiling (1500). The raise step would nominally target current+step,
	// but clamping to the ceiling (1500) would push the target below
	// current(5000) — must collapse to a no-op instead.
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_year": {Score: 90, VotingPower: 5000},
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != "" {
		t.Fatalf("Action = %q, want \"\" (no-op: already above max_voting_power_share ceiling); TargetPower=%d CurrentPower=%d",
			got.Action, got.TargetPower, got.CurrentPower)
	}
}

// TestEvaluate_LowerBelowFloorIsNoop is the mirror case: a lower_vp-eligible
// validator whose current power is already below min_voting_power must not
// produce a "lower_vp" decision whose TargetPower is actually higher than
// CurrentPower.
func TestEvaluate_LowerBelowFloorIsNoop(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}
	// current=50 is already below minVP=100. The lower_vp step would
	// nominally target current-step, but clamping to the floor (100) would
	// push the target above current(50) — must collapse to a no-op instead.
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 50},
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != "" {
		t.Fatalf("Action = %q, want \"\" (no-op: already below min_voting_power floor); TargetPower=%d CurrentPower=%d",
			got.Action, got.TargetPower, got.CurrentPower)
	}
}

func TestEvaluate_MissingWindowDataSkipsAction(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}
	// No current_month entry at all -> eject can't be evaluated, falls
	// through to lower_vp on current_week.
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 1000},
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != config.ActionLowerVP {
		t.Fatalf("Action = %q, want lower_vp (eject's window has no data)", got.Action)
	}
}

func TestTotalVotingPower(t *testing.T) {
	reports := []gnomonitoring.Report{
		report("g1a", map[string]gnomonitoring.PeriodScore{"current_week": {VotingPower: 1000}}),
		report("g1b", map[string]gnomonitoring.PeriodScore{"current_week": {VotingPower: 2500}}),
	}
	if got := TotalVotingPower(reports, "current_week"); got != 3500 {
		t.Fatalf("TotalVotingPower = %d, want 3500", got)
	}
}
