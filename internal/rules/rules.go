// Package rules decides, per validator per cycle, which score-driven action
// (if any) applies: eject, lower_vp, or raise_vp. It is pure logic over a
// gnomonitoring report and the resolved action config — no I/O, no history.
package rules

import (
	"fmt"
	"strings"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnoland"
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

// Valset is the live consensus valset read from the chain RPC: the
// authoritative voting power for VP-change targets. gnomonitoring's per-window
// voting power is display data only and never feeds a computation.
type Valset struct {
	Power map[string]int64 // lower-cased address -> current voting power
	Total int64            // sum of Power
}

// NewValset indexes RPC validators by lower-cased address.
func NewValset(vals []gnoland.Validator) Valset {
	v := Valset{Power: make(map[string]int64, len(vals))}
	for _, val := range vals {
		v.Power[strings.ToLower(val.Address)] = val.VotingPower
		v.Total += val.VotingPower
	}
	return v
}

// Evaluate walks priority in order and returns the first action whose
// threshold matches on its own configured window. Current and total voting
// power come from the live valset (vs); a validator absent from it gets no
// lower_vp/raise_vp decision, since it has no voting power to change.
func Evaluate(r gnomonitoring.Report, priority []string, actions map[string]config.ActionConfig,
	minVP int64, maxVPShare float64, vs Valset) Decision {

	d := Decision{Address: r.Addr, Moniker: r.Moniker}
	current, inValset := vs.Power[strings.ToLower(r.Addr)]

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
			if period.Score < *ac.ScoreBelow {
				d.Action = name
				d.Score, d.Tier, d.Window = period.Score, period.Tier, ac.Window
				d.CurrentPower = current
				d.Reason = reason(period, ac.Window)
				return d
			}

		case config.ActionLowerVP, config.ActionRaiseVP:
			// ScoreBelow/ScoreAtOrAbove are only guaranteed set for the field
			// each action actually uses (see ActionConfig doc comment) — the
			// other one may be nil, so only dereference the relevant field.
			var matches bool
			if name == config.ActionRaiseVP {
				matches = period.Score >= *ac.ScoreAtOrAbove
			} else {
				matches = period.Score < *ac.ScoreBelow
			}
			if !matches || !inValset {
				continue
			}
			target := vpTarget(current, ac, minVP, maxVPShare, vs.Total)
			if target == current {
				// Clamped to a no-op: already at a bound, nothing to propose
				// for this action — try the next one in priority.
				continue
			}
			d.Action = name
			d.Score, d.Tier, d.Window = period.Score, period.Tier, ac.Window
			d.CurrentPower = current
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
//
// The floor/ceiling clamp can itself push the target back across current in
// the wrong direction — e.g. raise_vp on a validator already above the
// maxVPShare ceiling clamps target down below current, which would read as a
// "raise" whose target is lower than where it started. When that happens the
// action is not meaningful (the validator is already past the relevant
// bound), so the target collapses to current, which Evaluate's
// target == period.VotingPower check then treats as a no-op.
func vpTarget(current int64, ac config.ActionConfig, minVP int64, maxVPShare float64, total int64) int64 {
	capSelf := *ac.VPStepPercentOfSelf / 100 * float64(current)
	capTotal := *ac.VPStepPercentOfTotal / 100 * float64(total)
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
		if target >= current {
			// Clamping to the floor pushed the target back up to or past
			// current: the validator is already at/under the floor, so a
			// further reduction is not meaningful — no-op.
			target = current
		}
	case step > 0:
		ceiling := int64(maxVPShare / 100 * float64(total))
		if target > ceiling {
			target = ceiling
		}
		if target <= current {
			// Clamping to the ceiling pushed the target back down to or past
			// current: the validator is already at/over the share ceiling, so
			// a further increase is not meaningful — no-op.
			target = current
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
