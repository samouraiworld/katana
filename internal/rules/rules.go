// Package rules evaluates whether a validator meets the configured conditions to
// be subscribed to an ejection proposal.
package rules

import (
	"fmt"

	"github.com/samouraiworld/katana/internal/config"
)

// Metrics is the per-validator input to an evaluation.
type Metrics struct {
	Moniker        string
	Address        string
	MissedBlocks   int
	CriticalAlerts int
}

// Evaluation is the outcome of applying the conditions to a validator's metrics.
type Evaluation struct {
	Metrics
	MissedBlocksTriggered   bool
	CriticalAlertsTriggered bool
	// Eligible is true when the enabled conditions, combined per RequireAll,
	// are satisfied.
	Eligible bool
	// Reasons contains a human-readable explanation for each triggered
	// condition, suitable for embedding in a proposal description.
	Reasons []string
}

// Reason returns the triggered reasons joined into a single sentence, or a
// generic fallback when none were recorded.
func (e Evaluation) Reason() string {
	if len(e.Reasons) == 0 {
		return "no condition triggered"
	}
	out := e.Reasons[0]
	for _, r := range e.Reasons[1:] {
		out += "; " + r
	}
	return out
}

// Evaluate applies conditions to a validator's metrics.
func Evaluate(m Metrics, cond config.Conditions) Evaluation {
	e := Evaluation{Metrics: m}

	// Track enabled conditions and how many of them triggered so RequireAll can
	// be applied correctly (an "all" over zero enabled conditions is never
	// eligible).
	enabled := 0
	triggered := 0

	if cond.MissedBlocks.Enabled {
		enabled++
		if m.MissedBlocks > cond.MissedBlocks.Max {
			e.MissedBlocksTriggered = true
			triggered++
			e.Reasons = append(e.Reasons, fmt.Sprintf(
				"missed %d blocks over %s (threshold %d)",
				m.MissedBlocks, cond.MissedBlocks.Interval, cond.MissedBlocks.Max))
		}
	}

	if cond.CriticalAlerts.Enabled {
		enabled++
		if m.CriticalAlerts > cond.CriticalAlerts.Max {
			e.CriticalAlertsTriggered = true
			triggered++
			e.Reasons = append(e.Reasons, fmt.Sprintf(
				"raised %d CRITICAL alerts over %s (threshold %d)",
				m.CriticalAlerts, cond.CriticalAlerts.Interval, cond.CriticalAlerts.Max))
		}
	}

	switch {
	case enabled == 0:
		e.Eligible = false
	case cond.RequireAll:
		e.Eligible = triggered == enabled
	default:
		e.Eligible = triggered > 0
	}
	return e
}
