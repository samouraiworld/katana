package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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
	pending, err := st.TrackedProposals(ctx, "local")
	if err != nil || len(pending) != 1 || pending[0].OnchainID != "3" {
		t.Fatalf("TrackedProposals = %+v, err %v", pending, err)
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
	if p, _ := st.TrackedProposals(ctx, "local"); len(p) != 0 {
		t.Fatalf("expected no pending after rejection, got %d", len(p))
	}

	// A different validator has no history.
	if last, _ := st.LastProposal(ctx, "local", "g1good"); last != nil {
		t.Fatalf("expected nil last proposal for unknown validator, got %+v", last)
	}
}

func TestDryRunRecordCountsTowardLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// A dry-run record is persisted…
	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Moniker: "bad", Status: StatusDryRun, Success: true,
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	// …and is visible to LastProposal, so it counts toward the real lifecycle
	// gate exactly like a broadcast attempt would (disabling dry_run later
	// does not bypass the cooldown of a proposal already shown to an
	// operator).
	last, err := st.LastProposal(ctx, "local", "g1bad")
	if err != nil || last == nil || last.ID != id || last.Status != StatusDryRun {
		t.Fatalf("LastProposal = %+v, err %v; want the dry-run record", last, err)
	}

	// It is not pending and not a rejection, so it doesn't show up in those
	// queries — same as any other non-pending, non-rejected record.
	if pending, _ := st.TrackedProposals(ctx, "local"); len(pending) != 0 {
		t.Fatalf("TrackedProposals = %+v; want none", pending)
	}
	if n, _ := st.RejectionCount(ctx, "local", "g1bad"); n != 0 {
		t.Fatalf("RejectionCount = %d, want 0", n)
	}
}

func TestProposalActionRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1a",
		Moniker: "a", Status: StatusPending, OnchainID: "1",
		Action: "lower_vp", CurrentPower: 1000, TargetPower: 600,
		Args: []string{"g1a"},
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	last, err := st.LastProposal(ctx, "local", "g1a")
	if err != nil || last == nil {
		t.Fatalf("LastProposal: %+v, err %v", last, err)
	}
	if last.ID != id || last.Action != "lower_vp" || last.CurrentPower != 1000 || last.TargetPower != 600 {
		t.Fatalf("round-tripped record = %+v, want action=lower_vp current=1000 target=600", last)
	}
}

func TestEvaluationScoreTierRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.RecordEvaluation(ctx, EvalRecord{
		CreatedAt: time.Now(), Chain: "local", Address: "g1a", Moniker: "a",
		Score: 24, Tier: "Critical", Eligible: true, Reason: "score 24/100",
	}); err != nil {
		t.Fatalf("RecordEvaluation: %v", err)
	}
	// No public read API for evaluations beyond the write path itself; this
	// asserts RecordEvaluation with the new fields doesn't error against the
	// migrated schema (the NOT NULL missed_blocks/critical_alerts columns
	// are the risk: an unmigrated or mis-migrated schema would fail here).
}

// TestMigrationAddsColumnsToExistingDB simulates a pre-existing database that
// predates the action/target_power/current_power columns, and asserts
// opening it through store.New (which runs migrate()) additively adds them
// without touching existing data.
func TestMigrationAddsColumnsToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	const oldSchema = `
CREATE TABLE proposals (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at          TEXT    NOT NULL,
	chain               TEXT    NOT NULL,
	chain_id            TEXT    NOT NULL,
	address             TEXT    NOT NULL,
	moniker             TEXT    NOT NULL,
	reason              TEXT    NOT NULL,
	title               TEXT    NOT NULL,
	realm_path          TEXT    NOT NULL,
	function            TEXT    NOT NULL,
	args                TEXT    NOT NULL,
	output              TEXT    NOT NULL,
	success             INTEGER NOT NULL,
	onchain_proposal_id TEXT    NOT NULL DEFAULT '',
	status              TEXT    NOT NULL DEFAULT '',
	resolved_at         TEXT    NOT NULL DEFAULT ''
);`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatalf("seeding old schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO proposals (created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success)
		 VALUES ('2026-01-01T00:00:00Z', 'local', 'dev', 'g1old', 'old', 'r', 't', '', '', '[]', '', 1)`); err != nil {
		t.Fatalf("seeding old row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}

	st, err := New(config.Storage{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("New (migration): %v", err)
	}
	defer st.Close()

	last, err := st.LastProposal(context.Background(), "local", "g1old")
	if err != nil || last == nil {
		t.Fatalf("LastProposal after migration: %+v, err %v", last, err)
	}
	if last.Action != "eject" {
		t.Fatalf("pre-migration row Action = %q, want default 'eject'", last.Action)
	}
	if last.TargetPower != 0 || last.CurrentPower != 0 {
		t.Fatalf("pre-migration row TargetPower/CurrentPower = %d/%d, want 0/0", last.TargetPower, last.CurrentPower)
	}

	// Migration must be idempotent: opening it again must not error.
	st2, err := New(config.Storage{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("New (second open, idempotency): %v", err)
	}
	st2.Close()
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
}

// TestTrackedProposalsAndRejectionCount: an expired proposal is still open
// on-chain, so the vote worker keeps polling it, and the gate counts it like a
// rejection. An unconfirmed broadcast is tracked until it resolves or expires.
func TestTrackedProposalsAndRejectionCount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	rec := func(onchainID, status string) int64 {
		id, err := st.RecordProposal(ctx, ProposalRecord{
			CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
			Status: StatusPending, OnchainID: onchainID, Success: true,
		})
		if err != nil {
			t.Fatalf("RecordProposal: %v", err)
		}
		if status != StatusPending {
			if err := st.UpdateStatus(ctx, id, status, time.Now()); err != nil {
				t.Fatalf("UpdateStatus: %v", err)
			}
		}
		return id
	}
	rec("1", StatusPending)
	rec("2", StatusExpired)
	rec("3", StatusRejected)
	rec("4", StatusAccepted)
	rec("5", StatusUnconfirmed)

	tracked, err := st.TrackedProposals(ctx, "local")
	if err != nil {
		t.Fatalf("TrackedProposals: %v", err)
	}
	var ids []string
	for _, p := range tracked {
		ids = append(ids, p.OnchainID)
	}
	if strings.Join(ids, ",") != "1,2,5" {
		t.Fatalf("TrackedProposals ids = %v, want [1 2 5]", ids)
	}
	if n, err := st.RejectionCount(ctx, "local", "g1bad"); err != nil || n != 2 {
		t.Fatalf("RejectionCount = %d, %v; want 2 (rejected + expired)", n, err)
	}
}

func TestTxHashAndOnchainIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Status: StatusPending, TxHash: "q2Vh0z3D+Jm2nSx0Zl1Q0Q==", Success: true,
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if err := st.SetOnchainID(ctx, id, "12"); err != nil {
		t.Fatalf("SetOnchainID: %v", err)
	}
	last, err := st.LastProposal(ctx, "local", "g1bad")
	if err != nil || last == nil {
		t.Fatalf("LastProposal = %+v, %v", last, err)
	}
	if last.TxHash != "q2Vh0z3D+Jm2nSx0Zl1Q0Q==" || last.OnchainID != "12" {
		t.Fatalf("TxHash, OnchainID = %q, %q", last.TxHash, last.OnchainID)
	}
}
