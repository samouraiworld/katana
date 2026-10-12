package store

import (
	"context"
	"database/sql"
	"errors"
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
	if last.BroadcastHeight != 0 || last.PayloadHash != "" {
		t.Fatalf("pre-migration row BroadcastHeight/PayloadHash = %d/%q, want 0/\"\"", last.BroadcastHeight, last.PayloadHash)
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
// on-chain, so the vote worker keeps polling it; it is not a rejection. An
// unconfirmed broadcast is tracked until it resolves or expires.
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
	if n, err := st.RejectionCount(ctx, "local", "g1bad"); err != nil || n != 1 {
		t.Fatalf("RejectionCount = %d, %v; want 1 (expired is not a rejection)", n, err)
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

func TestBroadcastsSince(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	rec := func(chain string, at time.Time, status string) {
		id, err := st.RecordProposal(ctx, ProposalRecord{
			CreatedAt: at, Chain: chain, ChainID: "dev", Address: "g1x", Status: StatusPending,
		})
		if err != nil {
			t.Fatalf("RecordProposal: %v", err)
		}
		if status != StatusPending {
			if err := st.UpdateStatus(ctx, id, status, at); err != nil {
				t.Fatalf("UpdateStatus: %v", err)
			}
		}
	}
	recent := now.Add(-time.Hour)
	// Counted: every status a broadcast that may be on-chain can carry.
	rec("local", recent, StatusPending)
	rec("local", recent, StatusUnconfirmed)
	rec("local", recent, StatusAccepted)
	rec("local", recent, StatusRejected)
	rec("local", recent, StatusExpired)
	// Not counted: dry-run, definitive failure, too old, other chain.
	rec("local", recent, StatusDryRun)
	rec("local", recent, "")
	rec("local", now.Add(-25*time.Hour), StatusPending)
	rec("other", recent, StatusPending)

	n, err := st.BroadcastsSince(ctx, "local", now.Add(-24*time.Hour))
	if err != nil || n != 5 {
		t.Fatalf("BroadcastsSince = %d, %v; want 5", n, err)
	}
	if n, err := (Noop{}).BroadcastsSince(ctx, "local", now); err != nil || n != 0 {
		t.Fatalf("Noop.BroadcastsSince = %d, %v; want 0, nil", n, err)
	}
}

func TestBroadcastHeightAndPayloadHashRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const hash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Status: StatusUnconfirmed, BroadcastHeight: 42, PayloadHash: hash,
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	check := func(where string, r ProposalRecord) {
		t.Helper()
		if r.ID != id || r.BroadcastHeight != 42 || r.PayloadHash != hash {
			t.Fatalf("%s: ID/BroadcastHeight/PayloadHash = %d/%d/%q", where, r.ID, r.BroadcastHeight, r.PayloadHash)
		}
	}
	last, err := st.LastProposal(ctx, "local", "g1bad")
	if err != nil || last == nil {
		t.Fatalf("LastProposal = %+v, %v", last, err)
	}
	check("LastProposal", *last)
	tracked, err := st.TrackedProposals(ctx, "local")
	if err != nil || len(tracked) != 1 {
		t.Fatalf("TrackedProposals = %+v, %v", tracked, err)
	}
	check("TrackedProposals", tracked[0])
	list, err := st.ListProposals(ctx, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProposals = %+v, %v", list, err)
	}
	check("ListProposals", list[0])
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	record := func(status, txHash, onchainID string) int64 {
		t.Helper()
		id, err := st.RecordProposal(ctx, ProposalRecord{
			CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
			Status: status, TxHash: txHash, OnchainID: onchainID,
		})
		if err != nil {
			t.Fatalf("RecordProposal: %v", err)
		}
		return id
	}
	get := func(id int64) ProposalRecord {
		t.Helper()
		list, err := st.ListProposals(ctx, 100)
		if err != nil {
			t.Fatalf("ListProposals: %v", err)
		}
		for _, r := range list {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("row %d not found", id)
		return ProposalRecord{}
	}

	t.Run("unconfirmed becomes pending", func(t *testing.T) {
		id := record(StatusUnconfirmed, "", "")
		if err := st.Reconcile(ctx, id, "q7lHmFF/yLsDJx8hdMiYn+raUZ/0PLMGWdxaspP+qjE=", "7"); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		r := get(id)
		if r.Status != StatusPending || r.TxHash != "q7lHmFF/yLsDJx8hdMiYn+raUZ/0PLMGWdxaspP+qjE=" || r.OnchainID != "7" {
			t.Fatalf("after Reconcile = status %q, hash %q, id %q", r.Status, r.TxHash, r.OnchainID)
		}
		// A second reconciliation is a no-op: the row now has a tx hash.
		if err := st.Reconcile(ctx, id, "other", "8"); !errors.Is(err, ErrNotReconciled) {
			t.Fatalf("second Reconcile err = %v, want ErrNotReconciled", err)
		}
		if r := get(id); r.TxHash != "q7lHmFF/yLsDJx8hdMiYn+raUZ/0PLMGWdxaspP+qjE=" || r.OnchainID != "7" {
			t.Fatalf("second Reconcile overwrote the row: %+v", r)
		}
	})

	t.Run("expired keeps its status", func(t *testing.T) {
		id := record(StatusExpired, "", "")
		if err := st.Reconcile(ctx, id, "hash", "9"); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		r := get(id)
		if r.Status != StatusExpired || r.TxHash != "hash" || r.OnchainID != "9" {
			t.Fatalf("after Reconcile = status %q, hash %q, id %q", r.Status, r.TxHash, r.OnchainID)
		}
	})

	refused := []struct {
		name                      string
		status, txHash, onchainID string
	}{
		{"pending", StatusPending, "", ""},
		{"accepted", StatusAccepted, "", ""},
		{"rejected", StatusRejected, "", ""},
		{"dry-run", StatusDryRun, "", ""},
		{"failed broadcast", "", "", ""},
		{"unconfirmed with a tx hash", StatusUnconfirmed, "known", ""},
		{"expired with a tx hash", StatusExpired, "known", ""},
		{"unconfirmed with an id", StatusUnconfirmed, "", "3"},
	}
	for _, c := range refused {
		t.Run("refuses "+c.name, func(t *testing.T) {
			id := record(c.status, c.txHash, c.onchainID)
			if err := st.Reconcile(ctx, id, "hash", "9"); !errors.Is(err, ErrNotReconciled) {
				t.Fatalf("Reconcile err = %v, want ErrNotReconciled", err)
			}
			if r := get(id); r.Status != c.status || r.TxHash != c.txHash || r.OnchainID != c.onchainID {
				t.Fatalf("refused Reconcile modified the row: %+v", r)
			}
		})
	}

	t.Run("unknown row", func(t *testing.T) {
		if err := st.Reconcile(ctx, 9999, "hash", "9"); !errors.Is(err, ErrNotReconciled) {
			t.Fatalf("Reconcile err = %v, want ErrNotReconciled", err)
		}
	})

	t.Run("empty inputs", func(t *testing.T) {
		id := record(StatusUnconfirmed, "", "")
		if err := st.Reconcile(ctx, id, "", "9"); err == nil || errors.Is(err, ErrNotReconciled) {
			t.Fatalf("empty tx hash: err = %v, want a validation error", err)
		}
		if err := st.Reconcile(ctx, id, "hash", ""); err == nil || errors.Is(err, ErrNotReconciled) {
			t.Fatalf("empty id: err = %v, want a validation error", err)
		}
	})
}

// A broadcast is recorded as an unconfirmed intent first, then completed with
// gnokey's outcome.
func TestRecordBroadcastOutcome(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Status: StatusUnconfirmed, BroadcastHeight: 9, PayloadHash: "abc",
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if err := st.RecordBroadcastOutcome(ctx, id, ProposalRecord{
		Output: "OK!", Success: true, Status: StatusPending, TxHash: "h=", OnchainID: "4",
	}); err != nil {
		t.Fatalf("RecordBroadcastOutcome: %v", err)
	}
	last, err := st.LastProposal(ctx, "local", "g1bad")
	if err != nil || last == nil {
		t.Fatalf("LastProposal: %+v, %v", last, err)
	}
	if last.Output != "OK!" || !last.Success || last.Status != StatusPending || last.TxHash != "h=" ||
		last.OnchainID != "4" || last.BroadcastHeight != 9 || last.PayloadHash != "abc" {
		t.Fatalf("row = %+v", last)
	}

	if err := st.RecordBroadcastOutcome(ctx, id+100, ProposalRecord{Status: StatusPending}); err == nil {
		t.Fatal("updating a missing row must fail")
	}
}
