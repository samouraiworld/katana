package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "katana.db")
	st, err := New(config.Storage{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSQLiteLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Record a broadcast proposal (pending).
	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Moniker: "bad", Status: StatusPending, OnchainID: "3",
		Args: []string{"g1bad"},
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	// It should show up as pending.
	pending, err := st.PendingProposals(ctx, "local")
	if err != nil || len(pending) != 1 || pending[0].OnchainID != "3" {
		t.Fatalf("PendingProposals = %+v, err %v", pending, err)
	}

	// LastProposal returns it.
	last, err := st.LastProposal(ctx, "local", "g1bad")
	if err != nil || last == nil || last.ID != id {
		t.Fatalf("LastProposal = %+v, err %v", last, err)
	}

	// Reject it → rejection count becomes 1 and it leaves the pending set.
	if err := st.UpdateStatus(ctx, id, StatusRejected, time.Now()); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if n, _ := st.RejectionCount(ctx, "local", "g1bad"); n != 1 {
		t.Fatalf("RejectionCount = %d, want 1", n)
	}
	if p, _ := st.PendingProposals(ctx, "local"); len(p) != 0 {
		t.Fatalf("expected no pending after rejection, got %d", len(p))
	}

	// A different validator has no history.
	if last, _ := st.LastProposal(ctx, "local", "g1good"); last != nil {
		t.Fatalf("expected nil last proposal for unknown validator, got %+v", last)
	}
}

func TestDryRunRecordsExcludedFromLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// A dry-run record is persisted…
	if _, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Moniker: "bad", Status: StatusDryRun, Success: true,
	}); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	// …but is invisible to every real-lifecycle query, so a rehearsal can
	// never block a later real proposal.
	if last, err := st.LastProposal(ctx, "local", "g1bad"); err != nil || last != nil {
		t.Fatalf("LastProposal = %+v, err %v; want nil (dry-run excluded)", last, err)
	}
	if pending, _ := st.PendingProposals(ctx, "local"); len(pending) != 0 {
		t.Fatalf("PendingProposals = %+v; want none", pending)
	}
	if n, _ := st.RejectionCount(ctx, "local", "g1bad"); n != 0 {
		t.Fatalf("RejectionCount = %d, want 0", n)
	}

	// LastDryRun sees it (the rehearsal dedup path).
	dry, err := st.LastDryRun(ctx, "local", "g1bad")
	if err != nil || dry == nil || dry.Status != StatusDryRun {
		t.Fatalf("LastDryRun = %+v, err %v; want the dry-run record", dry, err)
	}

	// A later real proposal becomes LastProposal.
	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Status: StatusPending, Success: true, OnchainID: "9",
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if last, _ := st.LastProposal(ctx, "local", "g1bad"); last == nil || last.ID != id {
		t.Fatalf("LastProposal = %+v, want the real record %d", last, id)
	}
}

func TestNoopStore(t *testing.T) {
	ctx := context.Background()
	st, err := New(config.Storage{Driver: "none"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.RecordProposal(ctx, ProposalRecord{}); err != nil {
		t.Fatalf("Noop RecordProposal: %v", err)
	}
	if last, _ := st.LastProposal(ctx, "c", "a"); last != nil {
		t.Fatalf("Noop LastProposal should be nil")
	}
	if dry, _ := st.LastDryRun(ctx, "c", "a"); dry != nil {
		t.Fatalf("Noop LastDryRun should be nil")
	}
}
