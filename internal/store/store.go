// Package store persists katana's run history: every validator it evaluates,
// every proposal it produces, each proposal's on-chain lifecycle, and every
// notification attempt. It is also the source of truth for the proposal
// lifecycle gate (cooldown, rejection backoff, rejection cap).
//
// The Store interface is the single seam used by the rest of the program. Two
// implementations exist — Noop (persist nothing) and SQLite.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// ErrNotReconciled is returned by Reconcile when the row is not eligible for
// reconciliation (unknown id, wrong status, or a tx hash or GovDAO id already
// recorded). Nothing was written.
var ErrNotReconciled = errors.New("proposal row not reconcilable")

// Proposal lifecycle statuses.
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusRejected = "rejected"
	// StatusExpired marks a proposal still open on-chain after
	// vote_tracking.pending_timeout. GovDAO proposals never expire on-chain
	// and cannot be withdrawn, so the vote worker keeps polling (and
	// executing) it, and the lifecycle gate stays closed until it resolves.
	StatusExpired = "expired"
	// StatusUnconfirmed marks a broadcast whose outcome is unknown: gnokey
	// failed after sending the tx (timeout, dropped connection), so it may or
	// may not be on-chain. It blocks the gate like pending, and keeps
	// blocking once expired.
	StatusUnconfirmed = "unconfirmed"
	// StatusDryRun tags a proposal record produced by a dry_run chain instead
	// of a real broadcast attempt, so store queries and operators can tell a
	// deliberate non-broadcast apart from a genuine gnokey failure (Success:
	// false, Status: ""). It counts toward the real lifecycle gate exactly
	// like any other record: LastProposal/RejectionCount do not exclude it,
	// so disabling dry_run does not bypass the cooldown of a proposal that was
	// already shown to an operator. TrackedProposals excludes it (nothing to
	// vote-poll); DryRunsSince returns it for safety.min_valset_size.
	StatusDryRun = "dry-run"
)

// EvalRecord is one validator evaluated during a cycle.
type EvalRecord struct {
	CreatedAt time.Time
	Chain     string
	Address   string
	Moniker   string
	Score     int
	Tier      string
	Eligible  bool
	Excepted  bool
	Reason    string
}

// ProposalRecord is one proposal katana produced, plus its on-chain lifecycle.
type ProposalRecord struct {
	ID        int64
	CreatedAt time.Time
	Chain     string
	ChainID   string
	Address   string
	Moniker   string
	// Action is the score-driven action this proposal represents: eject |
	// lower_vp | raise_vp. Defaults to "eject" for rows written before this
	// field existed (see the sqlite migration in Task 6).
	Action       string
	TargetPower  int64
	CurrentPower int64
	Reason       string
	Title        string
	RealmPath    string
	Function     string
	Args         []string
	Output       string // gnokey output / error text
	Success      bool
	// Broadcast-only lifecycle fields.
	OnchainID  string    // GovDAO proposal id
	TxHash     string    // base64 tx hash printed by gnokey, if any
	Status     string    // pending | accepted | rejected | expired | unconfirmed ("" for a failed broadcast)
	ResolvedAt time.Time // zero until accepted/rejected/expired
	// Reconciliation inputs for a broadcast whose gnokey output carried no
	// tx hash (see Reconcile): the chain height read just before
	// broadcasting (0 = unknown) and the hex SHA-256 of the exact submitted
	// payload (txindexer.RunPayloadHash / CallPayloadHash; "" = unknown).
	BroadcastHeight int64
	PayloadHash     string
}

// NotifRecord is one notification attempt.
type NotifRecord struct {
	CreatedAt  time.Time
	Chain      string
	Address    string
	Moniker    string
	Channel    string // discord | slack | telegram
	Target     string // webhook host / chat id
	Event      string // sent | accepted | rejected
	Status     string // ok | error
	Error      string
	ProposalID int64
}

// Store persists run history. Implementations must be safe for the sequential
// use katana makes of them.
type Store interface {
	RecordEvaluation(ctx context.Context, r EvalRecord) error
	// RecordEvaluations persists a whole cycle's evaluation records in one
	// batch (a single transaction on SQLite), rather than one commit per
	// validator — an eval cycle records one row per report, not just per
	// eligible candidate.
	RecordEvaluations(ctx context.Context, rs []EvalRecord) error
	// RecordProposal persists a proposal and returns its row id.
	RecordProposal(ctx context.Context, r ProposalRecord) (int64, error)
	// RecordBroadcastOutcome completes a broadcast intent row (recorded as
	// unconfirmed before gnokey ran) with gnokey's outcome: output, success,
	// status, tx hash and GovDAO id. A missing row is an error.
	RecordBroadcastOutcome(ctx context.Context, id int64, outcome ProposalRecord) error
	// SetOnchainID records a GovDAO id resolved after the broadcast (from the
	// tx's events when gnokey's own output did not carry it).
	SetOnchainID(ctx context.Context, id int64, onchainID string) error
	// Reconcile attaches the tx hash and GovDAO id recovered from the
	// tx-indexer to a broadcast that gnokey reported without a tx hash. It
	// only touches a row whose status is unconfirmed or expired and whose
	// tx_hash and onchain id are both empty; otherwise it writes nothing and
	// returns ErrNotReconciled. An unconfirmed row becomes pending. An expired
	// row stays expired: it is past pending_timeout already (going back to
	// pending would re-expire it and notify twice) and the vote worker
	// follows expired rows anyway. txHash and onchainID must be non-empty.
	Reconcile(ctx context.Context, id int64, txHash, onchainID string) error
	// UpdateStatus advances a proposal's lifecycle status.
	UpdateStatus(ctx context.Context, id int64, status string, resolvedAt time.Time) error
	RecordNotification(ctx context.Context, r NotifRecord) error

	// Lifecycle queries.
	// LastProposal returns the validator's most recent proposal record
	// (broadcast or dry-run) on this chain, or nil.
	LastProposal(ctx context.Context, chain, address string) (*ProposalRecord, error)
	// TrackedProposals returns the proposals the vote worker still follows:
	// pending, unconfirmed, and expired (still open on-chain).
	TrackedProposals(ctx context.Context, chain string) ([]ProposalRecord, error)
	// RejectionCount counts the validator's rejected proposals.
	RejectionCount(ctx context.Context, chain, address string) (int, error)
	// BroadcastsSince counts the chain's proposals created at or after since
	// that may be on-chain (pending, unconfirmed, accepted, rejected,
	// expired). Dry-run records and definitive broadcast failures are not
	// counted. Backs the safety.max_proposals_per_day cap.
	BroadcastsSince(ctx context.Context, chain string, since time.Time) (int, error)
	// DryRunsSince returns the chain's dry-run records (manual hand-offs)
	// created at or after since. Backs safety.min_valset_size, which counts a
	// recent eject hand-off as open: an operator may still broadcast it.
	DryRunsSince(ctx context.Context, chain string, since time.Time) ([]ProposalRecord, error)

	ListProposals(ctx context.Context, limit int) ([]ProposalRecord, error)
	Close() error
}

// New builds the Store described by the storage configuration.
func New(cfg config.Storage) (Store, error) {
	switch cfg.Driver {
	case "none":
		return Noop{}, nil
	case "sqlite":
		return newSQLite(cfg.SQLitePath)
	default:
		return nil, fmt.Errorf("unknown storage driver %q", cfg.Driver)
	}
}

// timeLayout is the textual format used to store timestamps in SQLite.
const timeLayout = time.RFC3339

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Noop is a Store that persists nothing. It is the default backend.
type Noop struct{}

func (Noop) RecordEvaluation(context.Context, EvalRecord) error                  { return nil }
func (Noop) RecordEvaluations(context.Context, []EvalRecord) error               { return nil }
func (Noop) RecordProposal(context.Context, ProposalRecord) (int64, error)       { return 0, nil }
func (Noop) RecordBroadcastOutcome(context.Context, int64, ProposalRecord) error { return nil }
func (Noop) SetOnchainID(context.Context, int64, string) error                   { return nil }
func (Noop) UpdateStatus(context.Context, int64, string, time.Time) error        { return nil }
func (Noop) Reconcile(context.Context, int64, string, string) error              { return nil }
func (Noop) RecordNotification(context.Context, NotifRecord) error               { return nil }
func (Noop) LastProposal(context.Context, string, string) (*ProposalRecord, error) {
	return nil, nil
}
func (Noop) TrackedProposals(context.Context, string) ([]ProposalRecord, error) { return nil, nil }
func (Noop) RejectionCount(context.Context, string, string) (int, error)        { return 0, nil }
func (Noop) BroadcastsSince(context.Context, string, time.Time) (int, error)    { return 0, nil }
func (Noop) DryRunsSince(context.Context, string, time.Time) ([]ProposalRecord, error) {
	return nil, nil
}
func (Noop) ListProposals(context.Context, int) ([]ProposalRecord, error) { return nil, nil }
func (Noop) Close() error                                                 { return nil }
