package config

import (
	"fmt"
	"math"
)

// Safety holds the anti-runaway guardrails bounding how many proposals katana
// may produce. The global block is the default; a chain may override any
// subset of fields (field-by-field merge, like Gnokey). Pointer fields so an
// explicit value is distinguishable from "unset": an explicit 0 or negative
// value is a validation error, never "disabled" — the guardrails cannot be
// turned off, only loosened.
type Safety struct {
	// MaxProposalsPerCycle caps the proposals broadcast per chain per eval
	// cycle (default 1).
	MaxProposalsPerCycle *int `yaml:"max_proposals_per_cycle"`
	// MaxProposalsPerDay caps the proposals broadcast per chain over a rolling
	// 24h window (default 3).
	MaxProposalsPerDay *int `yaml:"max_proposals_per_day"`
	// MaxEligibleShare aborts the whole cycle when more than this percentage
	// of the valset is eligible for eject/lower_vp at once (default 25): a
	// mass-failure signal more likely caused by a monitoring outage than by
	// genuinely bad validators.
	MaxEligibleShare *float64 `yaml:"max_eligible_share"`
	// MinValsetSize forbids any ejection that would leave fewer validators
	// than this in the valset (default 4).
	MinValsetSize *int `yaml:"min_valset_size"`
}

// SafetyRuntime is the fully-resolved, validated Safety for one chain.
type SafetyRuntime struct {
	MaxProposalsPerCycle int
	MaxProposalsPerDay   int
	MaxEligibleShare     float64
	MinValsetSize        int
}

// defaultSafety is the built-in guardrail baseline every chain starts from.
var defaultSafety = SafetyRuntime{
	MaxProposalsPerCycle: 1,
	MaxProposalsPerDay:   3,
	MaxEligibleShare:     25,
	MinValsetSize:        4,
}

// validate checks every explicitly set field of s; prefix names the block in
// errors (e.g. "safety" or "chains.<name>.safety").
func (s Safety) validate(prefix string) error {
	if s.MaxProposalsPerCycle != nil && *s.MaxProposalsPerCycle < 1 {
		return fmt.Errorf("%s.max_proposals_per_cycle must be >= 1, got %d", prefix, *s.MaxProposalsPerCycle)
	}
	if s.MaxProposalsPerDay != nil && *s.MaxProposalsPerDay < 1 {
		return fmt.Errorf("%s.max_proposals_per_day must be >= 1, got %d", prefix, *s.MaxProposalsPerDay)
	}
	// NaN fails every comparison, so it is rejected explicitly: it would
	// otherwise pass here and silently disable the guardrail.
	if s.MaxEligibleShare != nil && (math.IsNaN(*s.MaxEligibleShare) || *s.MaxEligibleShare <= 0 || *s.MaxEligibleShare > 100) {
		return fmt.Errorf("%s.max_eligible_share must be in (0, 100], got %v", prefix, *s.MaxEligibleShare)
	}
	if s.MinValsetSize != nil && *s.MinValsetSize < 1 {
		return fmt.Errorf("%s.min_valset_size must be >= 1, got %d", prefix, *s.MinValsetSize)
	}
	return nil
}

// applyTo returns r with any set field of s applied on top.
func (s Safety) applyTo(r SafetyRuntime) SafetyRuntime {
	if s.MaxProposalsPerCycle != nil {
		r.MaxProposalsPerCycle = *s.MaxProposalsPerCycle
	}
	if s.MaxProposalsPerDay != nil {
		r.MaxProposalsPerDay = *s.MaxProposalsPerDay
	}
	if s.MaxEligibleShare != nil {
		r.MaxEligibleShare = *s.MaxEligibleShare
	}
	if s.MinValsetSize != nil {
		r.MinValsetSize = *s.MinValsetSize
	}
	return r
}

// resolveSafety merges built-in defaults < global < per-chain (field by
// field), validating both layers. The global layer is validated even when the
// chain overrides every field, so a bad global value never hides behind one
// chain's override.
func resolveSafety(global Safety, chainKey string, chain *Safety) (SafetyRuntime, error) {
	if err := global.validate("safety"); err != nil {
		return SafetyRuntime{}, err
	}
	r := global.applyTo(defaultSafety)
	if chain != nil {
		if err := chain.validate("chains." + chainKey + ".safety"); err != nil {
			return SafetyRuntime{}, err
		}
		r = chain.applyTo(r)
	}
	return r, nil
}
