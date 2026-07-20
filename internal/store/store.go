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
	"fmt"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Proposal lifecycle statuses.
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusRejected = "rejected"
	// StatusDryRun tags a proposal record produced by a dry_run chain instead
	// of a real broadcast attempt, so store queries and operators can tell a
	// deliberate non-broadcast apart from a genuine gnokey failure (Success:
	// false, Status: ""). It counts toward the real lifecycle gate exactly
	// like any other record: LastProposal/RejectionCount/PendingProposals do
	// not exclude it, so disabling dry_run does not bypass the cooldown of a
	// proposal that was already shown to an operator.
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
	Status     string    // pending | accepted | rejected ("" until broadcast succeeds)
	ResolvedAt time.Time // zero until accepted/rejected
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
	// RecordProposal persists a proposal and returns its row id.
	RecordProposal(ctx context.Context, r ProposalRecord) (int64, error)
	// SetOnchainID records the GovDAO id captured after a successful broadcast.
	SetOnchainID(ctx context.Context, id int64, onchainID string) error
	// UpdateStatus advances a proposal's lifecycle status.
	UpdateStatus(ctx context.Context, id int64, status string, resolvedAt time.Time) error
	RecordNotification(ctx context.Context, r NotifRecord) error

	// Lifecycle queries.
	// LastProposal returns the validator's most recent proposal record
	// (broadcast or dry-run) on this chain, or nil.
	LastProposal(ctx context.Context, chain, address string) (*ProposalRecord, error)
	PendingProposals(ctx context.Context, chain string) ([]ProposalRecord, error)
	RejectionCount(ctx context.Context, chain, address string) (int, error)

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

func (Noop) RecordEvaluation(context.Context, EvalRecord) error            { return nil }
func (Noop) RecordProposal(context.Context, ProposalRecord) (int64, error) { return 0, nil }
func (Noop) SetOnchainID(context.Context, int64, string) error             { return nil }
func (Noop) UpdateStatus(context.Context, int64, string, time.Time) error  { return nil }
func (Noop) RecordNotification(context.Context, NotifRecord) error         { return nil }
func (Noop) LastProposal(context.Context, string, string) (*ProposalRecord, error) {
	return nil, nil
}
func (Noop) PendingProposals(context.Context, string) ([]ProposalRecord, error) { return nil, nil }
func (Noop) RejectionCount(context.Context, string, string) (int, error)        { return 0, nil }
func (Noop) ListProposals(context.Context, int) ([]ProposalRecord, error)       { return nil, nil }
func (Noop) Close() error                                                       { return nil }
