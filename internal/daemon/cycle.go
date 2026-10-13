package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnoland"
	"github.com/samouraiworld/katana/internal/govdao"
	"github.com/samouraiworld/katana/internal/lifecycle"
	"github.com/samouraiworld/katana/internal/notify"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/rules"
	"github.com/samouraiworld/katana/internal/store"
	"github.com/samouraiworld/katana/internal/txindexer"
)

// evalCycle runs one evaluation pass for a chain: fetch validator reports,
// evaluate every candidate against the score-driven action model, and
// produce a proposal for each eligible, non-suppressed validator.
func (d *Daemon) evalCycle(ctx context.Context, rt config.Runtime) error {
	// Chain-identity assert (also runs on the worker's first cycle at startup):
	// skip the whole cycle before any state is written when the node no longer
	// reports the configured chain id.
	gno := gnoland.New(rt.RPCEndpoint)
	if !d.assertChainID(ctx, gno, rt) {
		return errors.New("chain identity check failed")
	}

	reports, err := d.gnomon.FetchReports(ctx, rt.GnomonitoringChain)
	if err != nil {
		d.logger.Printf("[%s][eval] %v", rt.Key, err)
		return err
	}

	// Fetched once per cycle rather than re-queried per eligible candidate:
	// the score-driven model (raise_vp especially) can make many more
	// validators eligible per cycle than the old missed-blocks/critical-
	// alerts subset did, so a per-candidate valset refetch would multiply
	// RPC calls with the candidate count instead of staying flat per cycle.
	valset, err := gno.Validators(ctx)
	if err != nil {
		d.logger.Printf("[%s][eval] valset fetch failed: %v (skipping cycle)", rt.Key, err)
		return fmt.Errorf("valset fetch: %w", err)
	}
	if len(valset) == 0 {
		d.logger.Printf("[%s][eval] node reports an empty valset (skipping cycle)", rt.Key)
		return errors.New("node reports an empty valset")
	}
	inValset := make(map[string]bool, len(valset))
	for _, v := range valset {
		inValset[strings.ToLower(v.Address)] = true
	}
	// The live valset, not gnomonitoring, is the voting-power source for the
	// VP-change targets: a report's VP is a window aggregate and can be stale.
	vs := rules.NewValset(valset)

	// First pass: decide every report, so the mass-eligibility guardrail
	// sees the whole valset before anything is proposed.
	decs := make([]rules.Decision, len(reports))
	excepted := make([]bool, len(reports))
	evals := make([]store.EvalRecord, 0, len(reports))
	downward := 0
	for i, report := range reports {
		dec := rules.Evaluate(report, rt.Proposal.ActionPriority, rt.Proposal.Actions,
			*rt.Proposal.MinVotingPower, *rt.Proposal.MaxVotingPowerShare, vs)
		decs[i] = dec
		excepted[i] = d.cfg.IsException(rt.Key, report.Addr, report.Moniker)
		evals = append(evals, store.EvalRecord{
			CreatedAt: d.now(), Chain: rt.Key, Address: report.Addr, Moniker: report.Moniker,
			Score: dec.Score, Tier: dec.Tier, Eligible: dec.Action != "", Excepted: excepted[i], Reason: dec.Reason,
		})
		if !excepted[i] && inValset[strings.ToLower(report.Addr)] &&
			(dec.Action == config.ActionEject || dec.Action == config.ActionLowerVP) {
			downward++
		}
	}
	// cycleErr marks the cycle failed for /health while letting it go on:
	// a store error closes the gate for one candidate, not the whole pass.
	var cycleErr error
	if err := d.st.RecordEvaluations(ctx, evals); err != nil {
		d.logger.Printf("[%s][eval] recording evaluations: %v", rt.Key, err)
		cycleErr = fmt.Errorf("recording evaluations: %w", err)
	}

	// A non-excepted validator in the valset but missing from the report
	// cannot be judged: it counts as downward, so a partial report cannot
	// dilute the guardrail below.
	reported := make(map[string]bool, len(reports))
	for _, report := range reports {
		reported[strings.ToLower(report.Addr)] = true
	}
	missing := 0
	for _, v := range valset {
		if !reported[strings.ToLower(v.Address)] && !d.cfg.IsException(rt.Key, v.Address, "") {
			missing++
		}
	}

	// Mass-eligibility guardrail: many validators failing at once points at
	// bad metrics (a gnomonitoring outage, a chain-wide incident) rather than
	// at many bad operators. Propose nothing and alert once per episode.
	if flagged := downward + missing; float64(flagged)*100 > rt.Safety.MaxEligibleShare*float64(len(valset)) {
		d.logger.Printf("[%s][eval] CRITICAL: %d of %d validators qualify for eject/lower_vp or are missing from the report (%d missing), above safety.max_eligible_share=%g%% — proposing nothing this cycle",
			rt.Key, flagged, len(valset), missing, rt.Safety.MaxEligibleShare)
		if d.setGuardTripped(rt.Key, true) {
			d.dispatchGuardrail(ctx, rt, flagged, len(valset))
		}
		return cycleErr // a tripped guardrail is a deliberate skip, not a failure
	}
	d.setGuardTripped(rt.Key, false)

	// Broadcast budget for this cycle. A failed count closes it.
	recent, err := d.st.BroadcastsSince(ctx, rt.Key, d.now().Add(-24*time.Hour))
	if err != nil {
		d.logger.Printf("[%s][eval] counting recent broadcasts: %v (skipping cycle)", rt.Key, err)
		return fmt.Errorf("counting recent broadcasts: %w", err)
	}
	broadcasts := 0
	openEjects := -1 // open ejections of validators still in the valset; -1 = not counted yet

	produced := 0
	byAction := map[string]int{}
	for i, report := range reports {
		dec := decs[i]
		if excepted[i] {
			if dec.Action != "" {
				d.logger.Printf("[%s][eval] SKIP (exception) %s would be eligible for %s: %s", rt.Key, disp(report.Moniker, report.Addr), dec.Action, dec.Reason)
			}
			continue
		}
		if dec.Action == "" {
			continue
		}
		mode := rt.ActionMode(dec.Action)
		if mode == config.ModeOff {
			continue
		}

		// Lifecycle gate: don't re-propose while pending/accepted(eject)/within
		// backoff or cooldown. Dry-run records count here too (LastProposal no
		// longer excludes them), so a rehearsal already dedups across cycles
		// the same way a real proposal would. A failed read closes the gate:
		// without the history, katana cannot tell a fresh candidate from one
		// with a proposal already open.
		last, err := d.st.LastProposal(ctx, rt.Key, report.Addr)
		if err != nil {
			d.logger.Printf("[%s][eval] SUPPRESS %s: reading last proposal: %v", rt.Key, disp(report.Moniker, report.Addr), err)
			cycleErr = fmt.Errorf("reading last proposal: %w", err)
			continue
		}
		rejections, err := d.st.RejectionCount(ctx, rt.Key, report.Addr)
		if err != nil {
			d.logger.Printf("[%s][eval] SUPPRESS %s: reading rejection count: %v", rt.Key, disp(report.Moniker, report.Addr), err)
			cycleErr = fmt.Errorf("reading rejection count: %w", err)
			continue
		}
		if g := lifecycle.Gate(last, rejections, lifecycle.Params{
			Cooldown:        rt.Proposal.CooldownFor(dec.Action),
			RejectedBackoff: rt.Proposal.RejectedBackoffDur(),
			MaxRejections:   rt.Proposal.MaxRejections,
			Now:             d.now(),
		}); !g.Allow {
			d.logger.Printf("[%s][eval] SUPPRESS %s: %s", rt.Key, disp(report.Moniker, report.Addr), g.Reason)
			continue
		}

		// Safety: confirm the validator is in the live consensus valset.
		if !inValset[strings.ToLower(report.Addr)] {
			d.logger.Printf("[%s][eval] %s not in valset, skipping", rt.Key, report.Addr)
			continue
		}

		// Safety: never shrink the valset below min_valset_size, counting
		// open ejections (they may still pass) as already gone.
		if dec.Action == config.ActionEject {
			if openEjects < 0 {
				if openEjects, err = d.openEjections(ctx, rt, inValset); err != nil {
					d.logger.Printf("[%s][eval] SUPPRESS %s: counting open ejections: %v", rt.Key, disp(report.Moniker, report.Addr), err)
					cycleErr = fmt.Errorf("counting open ejections: %w", err)
					openEjects = -1
					continue
				}
			}
			if remaining := len(valset) - openEjects - 1; remaining < rt.Safety.MinValsetSize {
				d.logger.Printf("[%s][eval] SUPPRESS %s: ejecting would leave %d validator(s) (%d in valset, %d open ejection(s)), below safety.min_valset_size=%d",
					rt.Key, disp(report.Moniker, report.Addr), remaining, len(valset), openEjects, rt.Safety.MinValsetSize)
				continue
			}
		}

		// Broadcast caps. Manual actions never broadcast, so they are not capped.
		if mode == config.ModeAuto {
			if broadcasts >= rt.Safety.MaxProposalsPerCycle {
				d.logger.Printf("[%s][eval] SUPPRESS %s: safety.max_proposals_per_cycle=%d reached", rt.Key, disp(report.Moniker, report.Addr), rt.Safety.MaxProposalsPerCycle)
				continue
			}
			if recent+broadcasts >= rt.Safety.MaxProposalsPerDay {
				d.logger.Printf("[%s][eval] SUPPRESS %s: safety.max_proposals_per_day=%d reached (%d in the last 24h)", rt.Key, disp(report.Moniker, report.Addr), rt.Safety.MaxProposalsPerDay, recent+broadcasts)
				continue
			}
		}

		d.logger.Printf("[%s][eval] ELIGIBLE %s action=%s mode=%s: %s", rt.Key, disp(report.Moniker, report.Addr), dec.Action, mode, dec.Reason)
		res, err := d.produce(ctx, rt, gno, dec, vs.Total, mode == config.ModeManual)
		if err != nil {
			cycleErr = err
		}
		if res == skipped {
			continue
		}
		produced++
		byAction[dec.Action]++
		// A definitive broadcast failure created nothing: it uses up neither
		// the budget nor the valset. A manual ejection may still be
		// broadcast by hand, so it counts as open: here for the rest of this
		// cycle, and through openEjections in later ones.
		if res == broadcastOnChain {
			broadcasts++
		}
		if dec.Action == config.ActionEject && res != broadcastFailed {
			openEjects++
		}
	}

	if produced == 0 {
		d.logger.Printf("[%s][eval] no proposal produced (%d candidates)", rt.Key, len(reports))
	} else {
		d.logger.Printf("[%s][eval] produced %d proposal(s) (eject=%d lower_vp=%d raise_vp=%d)",
			rt.Key, produced, byAction[config.ActionEject], byAction[config.ActionLowerVP], byAction[config.ActionRaiseVP])
	}
	return cycleErr
}

// openEjections counts the chain's open ejections of validators still in the
// valset: tracked (pending, unconfirmed, expired) eject proposals, plus eject
// hand-offs (dry-run records) younger than the eject cooldown. An operator may
// still broadcast a hand-off by hand, and until the cooldown ends the gate
// keeps katana from proposing that validator again. Each validator counts once.
func (d *Daemon) openEjections(ctx context.Context, rt config.Runtime, inValset map[string]bool) (int, error) {
	tracked, err := d.st.TrackedProposals(ctx, rt.Key)
	if err != nil {
		return 0, err
	}
	handedOff, err := d.st.DryRunsSince(ctx, rt.Key, d.now().Add(-rt.Proposal.CooldownFor(config.ActionEject)))
	if err != nil {
		return 0, err
	}
	open := map[string]bool{}
	for _, p := range append(tracked, handedOff...) {
		// An unset action is a legacy row, which was always an eject.
		if addr := strings.ToLower(p.Address); (p.Action == config.ActionEject || p.Action == "") && inValset[addr] {
			open[addr] = true
		}
	}
	return len(open), nil
}

// setGuardTripped records whether the chain's mass-eligibility guardrail is
// tripped, and reports whether this call tripped it (was clear before).
func (d *Daemon) setGuardTripped(chain string, tripped bool) bool {
	d.guardMu.Lock()
	defer d.guardMu.Unlock()
	if d.guardTripped == nil {
		d.guardTripped = map[string]bool{}
	}
	was := d.guardTripped[chain]
	d.guardTripped[chain] = tripped
	return tripped && !was
}

// produceResult is what produce did with a proposal.
type produceResult int

const (
	skipped          produceResult = iota // stopped before acting (no operator mapping, chain id mismatch)
	handedOff                             // manual: recorded and notified, not broadcast
	broadcastOnChain                      // committed or ambiguous: may be on-chain
	broadcastFailed                       // definitive failure: nothing on-chain
)

// produce builds a proposal for dec.Action, then either broadcasts it
// (signing via gnokey, recording it, notifying on success) or — when manual
// (dry_run chain, or the action's mode is manual) — logs and notifies the
// complete would-be proposal, with the script and a ready-to-run gnokey
// command attached, without touching the chain. totalVP is the live valset's
// total voting power (RPC /validators) this cycle, used to compute the
// notification's VPDeltaPercent.
func (d *Daemon) produce(ctx context.Context, rt config.Runtime, gno *gnoland.Client, dec rules.Decision, totalVP int64, manual bool) (produceResult, error) {
	// A valset action is keyed by the operator address, but metrics are keyed
	// by the signing address. A missing mapping is fatal for the broadcast (we
	// would act on an unknown operator).
	operator := dec.Address
	if op, ok := d.resolverFor(rt).OperatorFor(ctx, dec.Address); ok {
		operator = op
	} else {
		d.logger.Printf("[%s][eval] no operator mapping for signing address %s; skipping broadcast", rt.Key, dec.Address)
		return skipped, nil
	}

	built := proposal.Build(proposal.Target{
		ChainKey:        rt.Key,
		ChainID:         rt.ChainID,
		RPC:             rt.RPCEndpoint,
		Moniker:         dec.Moniker,
		Address:         dec.Address,
		OperatorAddress: operator,
		Reason:          dec.Reason,
		Action:          dec.Action,
		Score:           dec.Score,
		Tier:            dec.Tier,
		Window:          dec.Window,
		CurrentPower:    dec.CurrentPower,
		TargetPower:     dec.TargetPower,
	}, rt.Proposal, rt.Gnokey)

	rec := store.ProposalRecord{
		CreatedAt: d.now(), Chain: rt.Key, ChainID: rt.ChainID, Address: dec.Address,
		Moniker: dec.Moniker, Reason: dec.Reason, Title: built.Title,
		RealmPath: built.RealmPath, Function: built.Function, Args: built.Args,
		Action: dec.Action, TargetPower: dec.TargetPower, CurrentPower: dec.CurrentPower,
	}

	// Re-assert chain identity immediately before acting: a genesis reset can
	// land mid-cycle, after the check that opened this pass.
	if !d.assertChainID(ctx, gno, rt) {
		return skipped, nil
	}

	vpDeltaPercent := 0.0
	if totalVP > 0 {
		vpDeltaPercent = float64(dec.TargetPower-dec.CurrentPower) / float64(totalVP) * 100
	}
	nt := notifyTarget{Address: dec.Address, Moniker: dec.Moniker, Action: dec.Action, CurrentPower: dec.CurrentPower, TargetPower: dec.TargetPower}

	if manual {
		d.dryRun(ctx, rt, built, rec, operator, nt, vpDeltaPercent)
		return handedOff, nil
	}

	// Recovery data in case gnokey loses the tx hash (see reconcile): the
	// exact payload and the height the tx can only land after. A failed
	// height read only disables that recovery, never the broadcast.
	rec.PayloadHash = payloadHash(built)
	if h, err := gno.LatestHeight(ctx); err != nil {
		d.logger.Printf("[%s][eval] reading chain height before broadcast: %v (an unconfirmed outcome could not be reconciled)", rt.Key, err)
	} else {
		rec.BroadcastHeight = h
	}

	// Record the intent before gnokey runs, as unconfirmed: it closes the
	// gate whatever happens next (crash, failed outcome write), and carries
	// the reconcile data. No durable record, no broadcast.
	rec.Status = store.StatusUnconfirmed
	id, recErr := d.st.RecordProposal(ctx, rec)
	if recErr != nil {
		d.logger.Printf("[%s][eval] NOT broadcasting %s for %s: recording the broadcast intent failed: %v", rt.Key, dec.Action, dec.Address, recErr)
		return skipped, fmt.Errorf("recording broadcast intent: %w", recErr)
	}

	out, err := d.signAndBroadcast(ctx, built, rt.Gnokey)
	// The tx may have landed even if a shutdown interrupted gnokey: record
	// and notify the outcome regardless of cancellation (the intent row
	// already gates a restart, but would lose the hash and the alert).
	ctx, cancel := d.detach(ctx)
	defer cancel()
	tx := proposal.ParseTxOutput(out)
	result := store.ProposalRecord{Output: out, TxHash: tx.TxHash}
	outcome := proposal.Classify(out, err)
	switch outcome {
	case proposal.Committed:
		result.Success = true
		result.Status = store.StatusPending
		result.OnchainID = govdao.ProposalIDFromEvents([]byte(tx.Events), d.cfg.VoteTracking.GovdaoRealm)
	case proposal.Ambiguous:
		// The tx may be on-chain: gate like a pending proposal until the
		// vote worker resolves, reconciles or expires it.
		result.Status = store.StatusUnconfirmed
	default:
		result.Status = "" // definitive failure: nothing on-chain, no gate
	}
	rec.OnchainID, rec.TxHash = result.OnchainID, result.TxHash
	var cycleErr error
	if err := d.st.RecordBroadcastOutcome(ctx, id, result); errors.Is(err, store.ErrBroadcastSuperseded) {
		// The vote worker reconciled the row from the tx-indexer while
		// gnokey ran: its on-chain record is the better one.
		d.logger.Printf("[%s][eval] broadcast %s for %s: row %d was already reconciled by the vote worker, keeping it", rt.Key, outcome, dec.Address, id)
	} else if err != nil {
		// The intent row stays unconfirmed: the gate holds and the vote
		// worker can still reconcile it from the tx-indexer.
		d.logger.Printf("[%s][eval] CRITICAL: broadcast %s for %s but recording the outcome failed: %v — row %d stays unconfirmed (gate closed)", rt.Key, outcome, dec.Address, err, id)
		cycleErr = fmt.Errorf("recording broadcast outcome: %w", err)
	}

	switch outcome {
	case proposal.Committed:
		if rec.OnchainID == "" {
			d.logger.Printf("[%s][eval] broadcast ok for %s but no proposal id in gnokey output (tx %q): resolving it from the tx", rt.Key, dec.Address, rec.TxHash)
		}
		d.logger.Printf("[%s][eval] BROADCAST ok for %s action=%s (proposal id=%q)", rt.Key, dec.Address, dec.Action, rec.OnchainID)
		d.dispatch(ctx, notify.EventSent, rt, id, nt, rec.OnchainID, vpDeltaPercent, observePayload{})
	case proposal.Ambiguous:
		d.logger.Printf("[%s][eval] CRITICAL: broadcast outcome unknown for %s action=%s: %v — recorded as unconfirmed, check the chain", rt.Key, dec.Address, dec.Action, err)
		d.dispatch(ctx, notify.EventUnconfirmed, rt, id, nt, "", vpDeltaPercent, observePayload{})
	default:
		d.logger.Printf("[%s][eval] broadcast failed for %s: %v", rt.Key, dec.Address, err)
		return broadcastFailed, cycleErr
	}
	return broadcastOnChain, cycleErr
}

// notifyTarget carries the validator/action identity dispatch needs to build
// a notify.Info, independent of whether the caller is the eval path
// (produce/dryRun, from a rules.Decision) or the vote-watcher (voteCycle,
// from a store.ProposalRecord).
type notifyTarget struct {
	Address, Moniker, Action  string
	CurrentPower, TargetPower int64
}

// dryRun records, logs, and notifies the proposal a dry_run chain would have
// broadcast: the script/expression, a filename to attach it under, and a
// ready-to-run gnokey command — the same manual-approval hand-off a
// broadcast-capable chain's operator would get if sending failed and they had
// to act by hand. The record carries store.StatusDryRun and counts toward the
// real lifecycle gate like any other proposal record.
func (d *Daemon) dryRun(ctx context.Context, rt config.Runtime, built proposal.Built, rec store.ProposalRecord, operator string, nt notifyTarget, vpDeltaPercent float64) {
	code := built.Script
	if built.Submit == "call" {
		code = fmt.Sprintf("%s.%s(%s)", built.RealmPath, built.Function, strings.Join(built.Args, ", "))
	}
	filename := actionFilename(nt.Action, disp(nt.Moniker, nt.Address))
	command := built.Command(rt.Gnokey, filename)

	rec.Status = store.StatusDryRun
	rec.Success = true
	rec.Output = code
	id, recErr := d.st.RecordProposal(ctx, rec)
	if recErr != nil {
		d.logger.Printf("[%s][eval] recording dry-run proposal failed for %s: %v — expect this rehearsal to repeat next cycle", rt.Key, rec.Address, recErr)
	}
	d.logger.Printf("[%s][eval] WARN [DRY-RUN] would broadcast %s proposal: validator=%q addr=%s operator=%s chain_id=%s conditions=%q title=%q description=%q",
		rt.Key, nt.Action, nt.Moniker, nt.Address, operator, rt.ChainID, rec.Reason, built.Title, built.Description)
	d.dispatch(ctx, notify.EventWouldEject, rt, id, nt, "", vpDeltaPercent, observePayload{code: code, filename: filename, command: command})
}

// assertChainID confirms the node behind rt.RPCEndpoint still reports the
// configured chain_id. After a genesis reset the endpoint stays up but the
// network id changes; broadcasting would only produce signature-failure noise
// (or, with a reused id, transactions on the wrong chain). Fails closed: an
// unreachable /status also skips.
func (d *Daemon) assertChainID(ctx context.Context, gno *gnoland.Client, rt config.Runtime) bool {
	network, err := gno.Network(ctx)
	if err != nil {
		d.logger.Printf("[%s][eval] chain-id check failed: %v — skipping", rt.Key, err)
		return false
	}
	if network != rt.ChainID {
		d.logger.Printf("[%s][eval] ERROR configured chain_id %q but node reports %q — skipping; likely genesis reset (update the chain's chain_id once confirmed)", rt.Key, rt.ChainID, network)
		return false
	}
	return true
}

// detach returns the context a broadcast outcome is recorded and notified
// with: it survives the worker's cancellation (the tx may have landed) but is
// bounded by outcomeTimeout, so a stuck store or webhook cannot hold up
// shutdown forever.
func (d *Daemon) detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d.outcomeTimeout)
}

// executeRetry is the minimum delay between two ExecuteOrRejectProposal
// attempts on the same proposal. A call failing at DeliverTx still burns gas,
// so a call that keeps failing must not be retried on every vote tick.
const executeRetry = time.Hour

// maxExecuteFailures is how many failed ExecuteOrRejectProposal attempts
// katana makes on one proposal before giving up and alerting. The call fails
// for good when the executor panics (e.g. ejecting a validator already gone):
// the realm only turns a returned error into a denial, a panic reverts.
// The count is in memory, so a restart allows another round.
const maxExecuteFailures = 3

// executeState tracks katana's execute attempts on one proposal.
type executeState struct {
	last     time.Time
	failures int
}

// voteCycle follows this chain's tracked proposals: it resolves missing
// proposal ids, executes proposals that reached a supermajority, records
// GovDAO outcomes, and expires proposals left open past pending_timeout.
func (d *Daemon) voteCycle(ctx context.Context, rt config.Runtime) error {
	tracked, err := d.st.TrackedProposals(ctx, rt.Key)
	if err != nil {
		d.logger.Printf("[%s][vote] %v", rt.Key, err)
		return err
	}
	if len(tracked) == 0 {
		return nil
	}
	gd := govdao.New(rt.RPCEndpoint, d.cfg.VoteTracking.GovdaoRealm)
	var cycleErr error
	for _, p := range tracked {
		if err := d.trackProposal(ctx, rt, gd, p); err != nil {
			cycleErr = err
		}
	}
	return cycleErr
}

// trackProposal advances one tracked proposal. It returns an operational
// error for /health: the GovDAO status or a configured tx-indexer could not be
// read, /tx could not be queried, or lifecycle state could not be persisted.
// An unrecognised render is one too, though the row is left untouched. A /tx
// lookup that finds no tx, or a committed tx without a ProposalCreated event,
// is not one (an ambiguous broadcast may simply never have landed).
func (d *Daemon) trackProposal(ctx context.Context, rt config.Runtime, gd *govdao.Client, p store.ProposalRecord) error {
	nt := notifyTarget{Address: p.Address, Moniker: p.Moniker, Action: p.Action, CurrentPower: p.CurrentPower, TargetPower: p.TargetPower}
	var opErr error

	if p.TxHash == "" {
		// Rows recorded before the tx_hash column existed: the hash is in
		// the stored gnokey output.
		p.TxHash = proposal.ParseTxOutput(p.Output).TxHash
	}
	if p.OnchainID == "" && p.TxHash == "" {
		p, opErr = d.reconcile(ctx, rt, p)
	}
	if p.OnchainID == "" && p.TxHash != "" {
		if id, err := gd.TxProposalID(ctx, p.TxHash); err != nil {
			d.logger.Printf("[%s][vote] proposal row %d: resolving id from tx %s: %v", rt.Key, p.ID, p.TxHash, err)
			if !errors.Is(err, govdao.ErrTxNotFound) && !errors.Is(err, govdao.ErrNoProposalCreated) {
				opErr = errors.Join(opErr, fmt.Errorf("resolving proposal id from tx %s: %w", p.TxHash, err))
			}
		} else if err := d.st.SetOnchainID(ctx, p.ID, id); err != nil {
			d.logger.Printf("[%s][vote] proposal row %d: recording id %s: %v", rt.Key, p.ID, id, err)
			opErr = fmt.Errorf("recording proposal id: %w", err)
		} else {
			d.logger.Printf("[%s][vote] proposal row %d: resolved GovDAO id %s from tx %s", rt.Key, p.ID, id, p.TxHash)
			p.OnchainID = id
		}
	}
	if p.OnchainID == "" {
		// Nothing to poll: only the timeout can close this row.
		return errors.Join(opErr, d.expireIfDue(ctx, rt, p, nt))
	}

	prop, err := gd.Proposal(ctx, p.OnchainID)
	if err != nil {
		d.logger.Printf("[%s][vote] status query failed for proposal %s: %v", rt.Key, p.OnchainID, err)
		return errors.Join(opErr, fmt.Errorf("status query for proposal %s: %w", p.OnchainID, err))
	}
	switch prop.Status {
	case govdao.Accepted:
		return errors.Join(opErr, d.resolve(ctx, rt, p, store.StatusAccepted, notify.EventAccepted, nt))
	case govdao.Rejected:
		return errors.Join(opErr, d.resolve(ctx, rt, p, store.StatusRejected, notify.EventRejected, nt))
	case govdao.Pending:
		if rt.ExecuteProposals && prop.Executable() {
			switch d.execute(ctx, rt, p, prop) {
			case proposal.Committed:
				// The call decided the proposal: record the outcome now
				// rather than expiring it first.
				return errors.Join(opErr, d.recordAfterExecute(ctx, rt, gd, p, nt))
			case proposal.Ambiguous:
				// The outcome is on its way; let the next tick read it.
				return opErr
			}
		}
		return errors.Join(opErr, d.expireIfDue(ctx, rt, p, nt))
	default:
		// Unknown is inert: never terminal, never expired, never notified.
		// It still fails the cycle: tracking is broken (e.g. a realm
		// upgrade changed the render) and /health must show it.
		d.logger.Printf("[%s][vote] proposal %s: unrecognised render, keeping it %s", rt.Key, p.OnchainID, p.Status)
		return errors.Join(opErr, fmt.Errorf("chain %s: unrecognised render for proposal %s on %s", rt.Key, p.OnchainID, d.cfg.VoteTracking.GovdaoRealm))
	}
}

// reconcile looks a hashless broadcast (gnokey timed out while sending) up on
// the tx-indexer: a committed tx from katana's key, after the recorded height,
// carrying the exact payload and a ProposalCreated event. On a single match
// the row gets its tx hash and GovDAO id and is tracked like any other; no
// indexer, no match or an ambiguous match leaves it as it was (still gating).
func (d *Daemon) reconcile(ctx context.Context, rt config.Runtime, p store.ProposalRecord) (store.ProposalRecord, error) {
	if rt.TxIndexerURL == "" || p.BroadcastHeight <= 0 || p.PayloadHash == "" {
		return p, nil
	}
	// BroadcastHeight was already committed when the broadcast started: the
	// tx can only land in a later block.
	txs, err := txindexer.New(rt.TxIndexerURL, d.cfg.VoteTracking.GovdaoRealm, nil).ProposalsByCaller(ctx, rt.Gnokey.Address, p.BroadcastHeight+1)
	if err != nil {
		d.logger.Printf("[%s][vote] proposal row %d: tx-indexer lookup: %v", rt.Key, p.ID, err)
		return p, fmt.Errorf("tx-indexer lookup: %w", err)
	}
	tx, ok := txindexer.Match(txs, p.PayloadHash)
	if !ok {
		return p, nil
	}
	if err := d.st.Reconcile(ctx, p.ID, tx.Hash, tx.ProposalID); err != nil {
		d.logger.Printf("[%s][vote] proposal row %d: recording reconciled tx %s: %v", rt.Key, p.ID, tx.Hash, err)
		if errors.Is(err, store.ErrNotReconciled) {
			return p, nil // the row moved on meanwhile (e.g. the eval worker recorded the outcome)
		}
		return p, fmt.Errorf("recording reconciled tx: %w", err)
	}
	d.logger.Printf("[%s][vote] proposal row %d: RECONCILED via tx-indexer — tx %s at height %d, GovDAO id %s", rt.Key, p.ID, tx.Hash, tx.BlockHeight, tx.ProposalID)
	p.TxHash, p.OnchainID = tx.Hash, tx.ProposalID
	if p.Status == store.StatusUnconfirmed {
		p.Status = store.StatusPending
	}
	return p, nil
}

// payloadHash fingerprints exactly what a built proposal submits, the way the
// tx-indexer will show it back (see txindexer.Tx.PayloadHash).
func payloadHash(b proposal.Built) string {
	if b.Submit == "call" {
		return txindexer.CallPayloadHash(b.RealmPath, b.Function, b.Args)
	}
	return txindexer.RunPayloadHash(b.Script)
}

// recordAfterExecute reads the render right after a committed execute call
// and records the outcome it shows.
func (d *Daemon) recordAfterExecute(ctx context.Context, rt config.Runtime, gd *govdao.Client, p store.ProposalRecord, nt notifyTarget) error {
	prop, err := gd.Proposal(ctx, p.OnchainID)
	if err != nil {
		d.logger.Printf("[%s][vote] status query after execute failed for proposal %s: %v", rt.Key, p.OnchainID, err)
		return fmt.Errorf("status query after execute for proposal %s: %w", p.OnchainID, err)
	}
	switch prop.Status {
	case govdao.Accepted:
		return d.resolve(ctx, rt, p, store.StatusAccepted, notify.EventAccepted, nt)
	case govdao.Rejected:
		return d.resolve(ctx, rt, p, store.StatusRejected, notify.EventRejected, nt)
	}
	return nil
}

// resolve records a GovDAO outcome and notifies it. The notification only
// goes out once the status is persisted, so a failed write is retried next
// tick instead of notifying the same outcome repeatedly.
func (d *Daemon) resolve(ctx context.Context, rt config.Runtime, p store.ProposalRecord, status string, event notify.Event, nt notifyTarget) error {
	if err := d.st.UpdateStatus(ctx, p.ID, status, d.now()); err != nil {
		d.logger.Printf("[%s][vote] proposal %s: recording %s: %v", rt.Key, p.OnchainID, status, err)
		return fmt.Errorf("recording %s: %w", status, err)
	}
	d.execMu.Lock()
	delete(d.executed, p.ID)
	d.execMu.Unlock()
	d.logger.Printf("[%s][vote] proposal %s %s — %s action=%s", rt.Key, p.OnchainID, strings.ToUpper(status), disp(p.Moniker, p.Address), p.Action)
	d.dispatch(ctx, event, rt, p.ID, nt, p.OnchainID, 0, observePayload{})
	return nil
}

// expireIfDue marks a still-open proposal expired once pending_timeout has
// passed since it was broadcast. Expired rows stay tracked for a late outcome.
func (d *Daemon) expireIfDue(ctx context.Context, rt config.Runtime, p store.ProposalRecord, nt notifyTarget) error {
	if p.Status == store.StatusExpired || d.now().Sub(p.CreatedAt) < rt.PendingTimeout {
		return nil
	}
	if err := d.st.UpdateStatus(ctx, p.ID, store.StatusExpired, d.now()); err != nil {
		d.logger.Printf("[%s][vote] proposal row %d: recording expiry: %v", rt.Key, p.ID, err)
		return fmt.Errorf("recording expiry: %w", err)
	}
	d.logger.Printf("[%s][vote] proposal %q (row %d) EXPIRED after %s — %s action=%s", rt.Key, p.OnchainID, p.ID, rt.PendingTimeout, disp(p.Moniker, p.Address), p.Action)
	d.dispatch(ctx, notify.EventExpired, rt, p.ID, nt, p.OnchainID, 0, observePayload{})
	return nil
}

// execute calls ExecuteOrRejectProposal on a proposal that reached a
// supermajority, at most once per executeRetry and maxExecuteFailures times,
// and returns the call's outcome (Failed when no call was made).
func (d *Daemon) execute(ctx context.Context, rt config.Runtime, p store.ProposalRecord, prop govdao.Proposal) proposal.Outcome {
	d.execMu.Lock()
	st, ok := d.executed[p.ID]
	if !ok {
		st = &executeState{}
		d.executed[p.ID] = st
	}
	if st.failures >= maxExecuteFailures || (!st.last.IsZero() && d.now().Sub(st.last) < executeRetry) {
		d.execMu.Unlock()
		return proposal.Failed
	}
	st.last = d.now()
	d.execMu.Unlock()

	b := proposal.ExecuteCall(rt.ChainID, rt.RPCEndpoint, d.cfg.VoteTracking.GovdaoRealm, p.OnchainID)
	out, err := d.signAndBroadcast(ctx, b, rt.Gnokey)
	ctx, cancel := d.detach(ctx) // notify even if a shutdown interrupted gnokey
	defer cancel()
	outcome := proposal.Classify(out, err)
	if outcome != proposal.Committed {
		d.logger.Printf("[%s][vote] EXECUTE %s for proposal %s (yes=%.2f%% no=%.2f%%): %v\n%s", rt.Key, outcome, p.OnchainID, prop.YesPercent, prop.NoPercent, err, out)
		d.execMu.Lock()
		st.failures++
		giveUp := st.failures == maxExecuteFailures
		d.execMu.Unlock()
		if giveUp {
			d.logger.Printf("[%s][vote] proposal %s: giving up on execution after %d failed attempts", rt.Key, p.OnchainID, maxExecuteFailures)
			nt := notifyTarget{Address: p.Address, Moniker: p.Moniker, Action: p.Action, CurrentPower: p.CurrentPower, TargetPower: p.TargetPower}
			d.dispatch(ctx, notify.EventExecuteFailed, rt, p.ID, nt, p.OnchainID, 0, observePayload{})
		}
		return outcome
	}
	d.logger.Printf("[%s][vote] EXECUTE ok for proposal %s (yes=%.2f%% no=%.2f%%)", rt.Key, p.OnchainID, prop.YesPercent, prop.NoPercent)
	return outcome
}

// observePayload carries the extra fields a would_eject (observe) notification
// needs: the proposal script, the filename it is attached under, and the
// ready-to-run gnokey command. Zero value = not an observe notification.
type observePayload struct {
	code, filename, command string
}

// dispatch sends a notification and records each channel result.
func (d *Daemon) dispatch(ctx context.Context, event notify.Event, rt config.Runtime, proposalID int64, nt notifyTarget, onchainID string, vpDeltaPercent float64, obs observePayload) {
	if !d.notifier.Enabled() {
		return
	}
	results := d.notifier.Dispatch(ctx, event, notify.Info{
		Chain:            rt.Key,
		Moniker:          nt.Moniker,
		Address:          nt.Address,
		GnowebURL:        rt.GnowebURL,
		ProposalID:       onchainID,
		RejectedBackoff:  rt.Proposal.RejectedBackoffDur(),
		PendingTimeout:   rt.PendingTimeout,
		Action:           nt.Action,
		CurrentPower:     nt.CurrentPower,
		TargetPower:      nt.TargetPower,
		VPDeltaPercent:   vpDeltaPercent,
		ProposalCode:     obs.code,
		ProposalFilename: obs.filename,
		GnokeyCommand:    obs.command,
	})
	d.recordNotifications(ctx, rt, event, nt, proposalID, results)
}

// dispatchGuardrail alerts that the chain's mass-eligibility guardrail
// tripped: eligible of valsetSize validators qualify for eject/lower_vp or
// are missing from the gnomonitoring report.
func (d *Daemon) dispatchGuardrail(ctx context.Context, rt config.Runtime, eligible, valsetSize int) {
	if !d.notifier.Enabled() {
		return
	}
	results := d.notifier.Dispatch(ctx, notify.EventGuardrail, notify.Info{
		Chain:            rt.Key,
		EligibleCount:    eligible,
		ValsetSize:       valsetSize,
		MaxEligibleShare: rt.Safety.MaxEligibleShare,
	})
	d.recordNotifications(ctx, rt, notify.EventGuardrail, notifyTarget{}, 0, results)
}

// recordNotifications logs failed channels and records each channel result.
func (d *Daemon) recordNotifications(ctx context.Context, rt config.Runtime, event notify.Event, nt notifyTarget, proposalID int64, results []notify.Result) {
	for _, r := range results {
		status := "ok"
		if !r.OK {
			status = "error"
			d.logger.Printf("[%s][notify] %s failed: %s", rt.Key, r.Channel, r.Err)
		}
		_ = d.st.RecordNotification(ctx, store.NotifRecord{
			CreatedAt: d.now(), Chain: rt.Key, Address: nt.Address, Moniker: nt.Moniker,
			Channel: r.Channel, Target: r.Target, Event: string(event),
			Status: status, Error: r.Err, ProposalID: proposalID,
		})
	}
}

func disp(moniker, addr string) string {
	if moniker != "" {
		return moniker
	}
	return addr
}

// actionFilename builds a safe .gno filename for the attached proposal
// script, prefixed by the action name, replacing any character outside
// [A-Za-z0-9-_] so it is a valid attachment name.
func actionFilename(action, name string) string {
	var b strings.Builder
	b.WriteString(action)
	b.WriteString("-")
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	b.WriteString(".gno")
	return b.String()
}
