package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// SQLite is a Store backed by a local SQLite database (pure Go, no CGO).
type SQLite struct {
	db *sql.DB
}

func newSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // katana writes sequentially

	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS evaluations (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at      TEXT    NOT NULL,
	chain           TEXT    NOT NULL,
	address         TEXT    NOT NULL,
	moniker         TEXT    NOT NULL,
	missed_blocks   INTEGER NOT NULL,
	critical_alerts INTEGER NOT NULL,
	eligible        INTEGER NOT NULL,
	excepted        INTEGER NOT NULL,
	reason          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_eval_chain_addr ON evaluations(chain, address);

CREATE TABLE IF NOT EXISTS proposals (
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
);
CREATE INDEX IF NOT EXISTS idx_prop_chain_addr ON proposals(chain, address);
CREATE INDEX IF NOT EXISTS idx_prop_status ON proposals(chain, status);

CREATE TABLE IF NOT EXISTS notifications (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at  TEXT    NOT NULL,
	chain       TEXT    NOT NULL,
	address     TEXT    NOT NULL,
	moniker     TEXT    NOT NULL,
	channel     TEXT    NOT NULL,
	target      TEXT    NOT NULL,
	event       TEXT    NOT NULL,
	status      TEXT    NOT NULL,
	error       TEXT    NOT NULL,
	proposal_id INTEGER NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrating sqlite schema: %w", err)
	}
	return nil
}

func (s *SQLite) RecordEvaluation(ctx context.Context, r EvalRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO evaluations
		 (created_at, chain, address, moniker, missed_blocks, critical_alerts, eligible, excepted, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmtTime(r.CreatedAt), r.Chain, r.Address, r.Moniker,
		r.MissedBlocks, r.CriticalAlerts, boolInt(r.Eligible), boolInt(r.Excepted), r.Reason,
	)
	return err
}

func (s *SQLite) RecordProposal(ctx context.Context, r ProposalRecord) (int64, error) {
	args, err := json.Marshal(r.Args)
	if err != nil {
		return 0, fmt.Errorf("marshaling proposal args: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO proposals
		 (created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success, onchain_proposal_id, status, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmtTime(r.CreatedAt), r.Chain, r.ChainID, r.Address, r.Moniker,
		r.Reason, r.Title, r.RealmPath, r.Function, string(args), r.Output,
		boolInt(r.Success), r.OnchainID, r.Status, fmtTime(r.ResolvedAt),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *SQLite) SetOnchainID(ctx context.Context, id int64, onchainID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE proposals SET onchain_proposal_id = ? WHERE id = ?`, onchainID, id)
	return err
}

func (s *SQLite) UpdateStatus(ctx context.Context, id int64, status string, resolvedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE proposals SET status = ?, resolved_at = ? WHERE id = ?`,
		status, fmtTime(resolvedAt), id)
	return err
}

func (s *SQLite) RecordNotification(ctx context.Context, r NotifRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notifications
		 (created_at, chain, address, moniker, channel, target, event, status, error, proposal_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmtTime(r.CreatedAt), r.Chain, r.Address, r.Moniker, r.Channel, r.Target,
		r.Event, r.Status, r.Error, r.ProposalID,
	)
	return err
}

const proposalCols = `id, created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success, onchain_proposal_id, status, resolved_at`

func scanProposal(rows *sql.Rows) (ProposalRecord, error) {
	var (
		r          ProposalRecord
		created    string
		argsJSON   string
		success    int
		resolvedAt string
	)
	if err := rows.Scan(&r.ID, &created, &r.Chain, &r.ChainID, &r.Address, &r.Moniker,
		&r.Reason, &r.Title, &r.RealmPath, &r.Function, &argsJSON,
		&r.Output, &success, &r.OnchainID, &r.Status, &resolvedAt); err != nil {
		return r, err
	}
	r.CreatedAt, _ = parseTime(created)
	if resolvedAt != "" {
		r.ResolvedAt, _ = parseTime(resolvedAt)
	}
	r.Success = success != 0
	_ = json.Unmarshal([]byte(argsJSON), &r.Args)
	return r, nil
}

func (s *SQLite) LastProposal(ctx context.Context, chain, address string) (*ProposalRecord, error) {
	// Dry-run rehearsal records never participate in the real lifecycle gate.
	return s.lastWhere(ctx,
		`chain = ? AND address = ? AND status != ?`, chain, address, StatusDryRun)
}

func (s *SQLite) LastDryRun(ctx context.Context, chain, address string) (*ProposalRecord, error) {
	return s.lastWhere(ctx,
		`chain = ? AND address = ? AND status = ?`, chain, address, StatusDryRun)
}

func (s *SQLite) lastWhere(ctx context.Context, where string, args ...any) (*ProposalRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+proposalCols+` FROM proposals WHERE `+where+` ORDER BY id DESC LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	r, err := scanProposal(rows)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *SQLite) PendingProposals(ctx context.Context, chain string) ([]ProposalRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+proposalCols+` FROM proposals WHERE chain = ? AND status = ? ORDER BY id`,
		chain, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProposals(rows)
}

func (s *SQLite) RejectionCount(ctx context.Context, chain, address string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proposals WHERE chain = ? AND address = ? AND status = ?`,
		chain, address, StatusRejected).Scan(&n)
	return n, err
}

func (s *SQLite) ListProposals(ctx context.Context, limit int) ([]ProposalRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+proposalCols+` FROM proposals ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProposals(rows)
}

func collectProposals(rows *sql.Rows) ([]ProposalRecord, error) {
	var out []ProposalRecord
	for rows.Next() {
		r, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }
