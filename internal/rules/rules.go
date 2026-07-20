// Package rules decides, per validator per cycle, which score-driven action
// (if any) applies: eject, lower_vp, or raise_vp. It is pure logic over a
// gnomonitoring report and the resolved action config — no I/O, no history.
package rules

import (
	"fmt"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnomonitoring"
)

// Decision is the outcome of evaluating one validator's report. Action is ""
// when no configured action's threshold matched.
type Decision struct {
	Address, Moniker string
	Action           string // config.ActionEject | ActionLowerVP | ActionRaiseVP | ""
	Score            int
	Tier             string
	Window           string
	CurrentPower     int64
	TargetPower      int64 // 0 for eject
	Reason           string
}

// TotalVotingPower sums every report's voting power for the given window,
// used to clamp lower_vp/raise_vp targets against a % of the chain's total
// valset VP. Missing entries for that window contribute 0.
func TotalVotingPower(reports []gnomonitoring.Report, window string) int64 {
	var total int64
	for _, r := range reports {
		total += r.Periods[window].VotingPower
	}
	return total
}

// Evaluate walks priority in order and returns the first action whose
// threshold matches on its own configured window. totalWindowVP maps a
// window name to that window's total valset voting power (TotalVotingPower),
// needed to clamp lower_vp/raise_vp targets against maxVPShare.
func Evaluate(r gnomonitoring.Report, priority []string, actions map[string]config.ActionConfig,
	minVP int64, maxVPShare float64, totalWindowVP map[string]int64) Decision {

	d := Decision{Address: r.Addr, Moniker: r.Moniker}

	for _, name := range priority {
		ac, ok := actions[name]
		if !ok {
			continue
		}
		period, ok := r.Periods[ac.Window]
		if !ok {
			continue
		}

		switch name {
		case config.ActionEject:
			if period.Score < ac.ScoreBelow {
				d.Action = name
				d.Score, d.Tier, d.Window = period.Score, period.Tier, ac.Window
				d.CurrentPower = period.VotingPower
				d.Reason = reason(period, ac.Window)
				return d
			}

		case config.ActionLowerVP, config.ActionRaiseVP:
			matches := period.Score < ac.ScoreBelow
			if name == config.ActionRaiseVP {
				matches = period.Score >= ac.ScoreAtOrAbove
			}
			if !matches {
				continue
			}
			total := totalWindowVP[ac.Window]
			target := vpTarget(period.VotingPower, ac, minVP, maxVPShare, total)
			if target == period.VotingPower {
				// Clamped to a no-op: already at a bound, nothing to propose.
				return d
			}
			d.Action = name
			d.Score, d.Tier, d.Window = period.Score, period.Tier, ac.Window
			d.CurrentPower = period.VotingPower
			d.TargetPower = target
			d.Reason = reason(period, ac.Window)
			return d
		}
	}
	return d
}

// vpTarget computes the clamped target voting power for a lower_vp/raise_vp
// action: current power adjusted by the smaller-magnitude of two step caps
// (a % of the validator's own current power, and a % of the chain's total
// valset power — see the spec's "VP delta model"), then clamped in the
// direction of the step: a reducing step (lower_vp) is floored at minVP, a
// raising step (raise_vp) is capped at maxVPShare% of total. The other bound
// never applies — e.g. clamping a lower_vp target against the network-share
// ceiling would push it back up and defeat the action.
func vpTarget(current int64, ac config.ActionConfig, minVP int64, maxVPShare float64, total int64) int64 {
	capSelf := ac.VPStepPercentOfSelf / 100 * float64(current)
	capTotal := ac.VPStepPercentOfTotal / 100 * float64(total)
	step := capSelf
	if absf(capTotal) < absf(capSelf) {
		step = capTotal
	}
	target := current + roundf(step)

	switch {
	case step < 0:
		if target < minVP {
			target = minVP
		}
	case step > 0:
		ceiling := int64(maxVPShare / 100 * float64(total))
		if target > ceiling {
			target = ceiling
		}
	}
	return target
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func roundf(f float64) int64 {
	if f < 0 {
		return int64(f - 0.5)
	}
	return int64(f + 0.5)
}

// reason renders a human-readable explanation embedding the deciding score
// and gnomonitoring's sub-metrics for that window, for use in proposal
// descriptions and notifications. Score stays the only eligibility signal;
// these sub-metrics are display-only context (see the spec).
func reason(p gnomonitoring.PeriodScore, window string) string {
	return fmt.Sprintf("score %d/100 (tier %s) over %s — sign_rate %.0f%%, %.1f incidents/week, %d downtime blocks",
		p.Score, p.Tier, window, p.SignRate, p.IncidentRatePerWeek, p.DowntimeBlocks)
}
