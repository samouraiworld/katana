// Package lifecycle implements the per-(chain, validator) gate that decides
// whether katana may create a new ejection proposal, given the validator's last
// proposal and its rejection history. It is pure logic: the current time is
// injected so cooldown and rejection-backoff windows are deterministically
// testable.
package lifecycle

import (
	"fmt"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/store"
)

// Params are the resolved lifecycle timers for a chain.
type Params struct {
	Cooldown        time.Duration
	RejectedBackoff time.Duration
	MaxRejections   int // 0 = unlimited
	Now             time.Time
}

// Decision is the gate outcome.
type Decision struct {
	Allow  bool
	Reason string // human-readable suppression reason (empty when Allow)
}

func allow() Decision { return Decision{Allow: true} }
func deny(f string, a ...any) Decision {
	return Decision{Allow: false, Reason: fmt.Sprintf(f, a...)}
}

// Gate decides whether a new proposal may be created for a validator.
//
//	last       — the validator's most recent proposal on this chain, or nil.
//	rejections — how many of its past proposals were rejected by GovDAO.
func Gate(last *store.ProposalRecord, rejections int, p Params) Decision {
	if last == nil {
		return allow()
	}

	switch last.Status {
	case store.StatusAccepted:
		if last.Action != config.ActionLowerVP && last.Action != config.ActionRaiseVP {
			// eject (or a legacy/unset row, which predates this field and
			// was always an eject) — the validator has been ejected.
			return deny("already ejected (proposal accepted)")
		}
		// lower_vp/raise_vp accepted is not terminal: the VP can be
		// adjusted again later once the score moves and cooldown elapses.
		// Anchor on ResolvedAt (falling back to CreatedAt), like the
		// rejected branch below: anchoring on CreatedAt would let a vote
		// that outlives the cooldown window skip the cooldown entirely the
		// moment it resolves.
		base := last.ResolvedAt
		if base.IsZero() {
			base = last.CreatedAt
		}
		if until := base.Add(p.Cooldown); p.Now.Before(until) {
			return deny("cooldown until %s", until.UTC().Format(time.RFC3339))
		}
		return allow()

	case store.StatusPending:
		return deny("a proposal is pending GovDAO vote")

	case store.StatusRejected:
		if p.MaxRejections > 0 && rejections >= p.MaxRejections {
			return deny("max rejections reached (%d)", rejections)
		}
		base := last.ResolvedAt
		if base.IsZero() {
			base = last.CreatedAt
		}
		if until := base.Add(p.RejectedBackoff); p.Now.Before(until) {
			return deny("rejection backoff until %s", until.UTC().Format(time.RFC3339))
		}
		return allow()

	default:
		// Failed broadcast or a proposal not yet promoted to an on-chain status:
		// apply the cooldown floor from the last proposal's creation time so a
		// failing broadcast is not retried every poll interval.
		if until := cooldownFloor(last, p); p.Now.Before(until) {
			return deny("cooldown until %s", until.UTC().Format(time.RFC3339))
		}
		return allow()
	}
}

// cooldownFloor is the earliest time a new proposal may be created given
// last's creation time and the configured cooldown.
func cooldownFloor(last *store.ProposalRecord, p Params) time.Time {
	return last.CreatedAt.Add(p.Cooldown)
}
