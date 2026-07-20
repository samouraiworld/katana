package lifecycle

import (
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/store"
)

func TestGate(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	params := Params{
		Cooldown:        720 * time.Hour,
		RejectedBackoff: 720 * time.Hour,
		MaxRejections:   2,
		Now:             now,
	}

	tests := []struct {
		name       string
		last       *store.ProposalRecord
		rejections int
		wantAllow  bool
	}{
		{
			name:      "no prior proposal is allowed",
			last:      nil,
			wantAllow: true,
		},
		{
			name:      "accepted is terminal (already ejected)",
			last:      &store.ProposalRecord{Status: store.StatusAccepted},
			wantAllow: false,
		},
		{
			name:      "accepted eject stays terminal",
			last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionEject},
			wantAllow: false,
		},
		{
			name:      "accepted lower_vp within cooldown is blocked",
			last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionLowerVP, CreatedAt: now.Add(-1 * time.Hour)},
			wantAllow: false,
		},
		{
			name:      "accepted lower_vp after cooldown is allowed",
			last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionLowerVP, CreatedAt: now.Add(-800 * time.Hour)},
			wantAllow: true,
		},
		{
			name:      "accepted raise_vp after cooldown is allowed",
			last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionRaiseVP, CreatedAt: now.Add(-800 * time.Hour)},
			wantAllow: true,
		},
		{
			name:      "pending blocks re-proposal",
			last:      &store.ProposalRecord{Status: store.StatusPending},
			wantAllow: false,
		},
		{
			name:       "rejected within backoff is blocked",
			last:       &store.ProposalRecord{Status: store.StatusRejected, ResolvedAt: now.Add(-1 * time.Hour)},
			rejections: 1,
			wantAllow:  false,
		},
		{
			name:       "rejected after backoff is allowed",
			last:       &store.ProposalRecord{Status: store.StatusRejected, ResolvedAt: now.Add(-800 * time.Hour)},
			rejections: 1,
			wantAllow:  true,
		},
		{
			name:       "rejection cap reached blocks even after backoff",
			last:       &store.ProposalRecord{Status: store.StatusRejected, ResolvedAt: now.Add(-800 * time.Hour)},
			rejections: 2,
			wantAllow:  false,
		},
		{
			name:      "empty-status within cooldown is blocked",
			last:      &store.ProposalRecord{Status: "", CreatedAt: now.Add(-1 * time.Hour)},
			wantAllow: false,
		},
		{
			name:      "empty-status after cooldown is allowed",
			last:      &store.ProposalRecord{Status: "", CreatedAt: now.Add(-800 * time.Hour)},
			wantAllow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Gate(tt.last, tt.rejections, params)
			if got.Allow != tt.wantAllow {
				t.Fatalf("Allow = %v, want %v (reason: %q)", got.Allow, tt.wantAllow, got.Reason)
			}
		})
	}
}

func TestRejectedCountIsSharedAcrossActionTypes(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	params := Params{Cooldown: 720 * time.Hour, RejectedBackoff: 720 * time.Hour, MaxRejections: 1, Now: now}

	// A lower_vp proposal was rejected once; the cap (1) is already reached,
	// regardless of what action a fresh evaluation would pick this cycle.
	last := &store.ProposalRecord{Status: store.StatusRejected, Action: config.ActionLowerVP, ResolvedAt: now.Add(-800 * time.Hour)}
	got := Gate(last, 1, params)
	if got.Allow {
		t.Fatalf("expected max_rejections to block regardless of action type, got Allow=true (reason: %q)", got.Reason)
	}
}
