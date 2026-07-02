// Package lifecycle implements the per-(chain, validator) gate that decides
// whether katana may create a new ejection proposal, given the validator's last
// proposal and its rejection history. It is pure logic: the current time is
// injected so cooldown and rejection-backoff windows are deterministically
// testable.
package lifecycle

import (
	"fmt"
	"time"

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
		return deny("already ejected (proposal accepted)")

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
		// Dry-run (no on-chain status) or any other state: apply the cooldown
		// floor from the last proposal's creation time.
		if until := last.CreatedAt.Add(p.Cooldown); p.Now.Before(until) {
			return deny("cooldown until %s", until.UTC().Format(time.RFC3339))
		}
		return allow()
	}
}
