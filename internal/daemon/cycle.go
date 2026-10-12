package daemon

import (
	"context"
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
)

// reportWindows are the four periods gnomonitoring always includes in a
// report; totals are precomputed for all of them once per cycle so any
// action's configured window has its total valset VP ready, with no extra
// API call.
var reportWindows = []string{"last_24h", "current_week", "current_month", "current_year"}

// evalCycle runs one evaluation pass for a chain: fetch validator reports,
// evaluate every candidate against the score-driven action model, and
// produce a proposal for each eligible, non-suppressed validator.
func (d *Daemon) evalCycle(ctx context.Context, rt config.Runtime) {
	// Chain-identity assert (also runs on the worker's first cycle at startup):
	// skip the whole cycle before any state is written when the node no longer
	// reports the configured chain id.
	gno := gnoland.New(rt.RPCEndpoint)
	if !d.assertChainID(ctx, gno, rt) {
		return
	}

	reports, err := d.gnomon.FetchReports(ctx, rt.GnomonitoringChain)
	if err != nil {
		d.logger.Printf("[%s][eval] %v", rt.Key, err)
		return
	}

	totalVP := map[string]int64{}
	for _, w := range reportWindows {
		totalVP[w] = rules.TotalVotingPower(reports, w)
	}

	// Fetched once per cycle rather than re-queried per eligible candidate:
	// the score-driven model (raise_vp especially) can make many more
	// validators eligible per cycle than the old missed-blocks/critical-
	// alerts subset did, so a per-candidate valset refetch would multiply
	// RPC calls with the candidate count instead of staying flat per cycle.
	valset, err := gno.Validators(ctx)
	if err != nil {
		d.logger.Printf("[%s][eval] valset fetch failed: %v (skipping cycle)", rt.Key, err)
		return
	}
	inValset := make(map[string]bool, len(valset))
	for _, v := range valset {
		inValset[strings.ToLower(v.Address)] = true
	}

	produced := 0
	byAction := map[string]int{}
	evals := make([]store.EvalRecord, 0, len(reports))
	for _, report := range reports {
		dec := rules.Evaluate(report, rt.Proposal.ActionPriority, rt.Proposal.Actions,
			*rt.Proposal.MinVotingPower, *rt.Proposal.MaxVotingPowerShare, totalVP)

		excepted := d.cfg.IsException(rt.Key, report.Addr, report.Moniker)
		evals = append(evals, store.EvalRecord{
			CreatedAt: d.now(), Chain: rt.Key, Address: report.Addr, Moniker: report.Moniker,
			Score: dec.Score, Tier: dec.Tier, Eligible: dec.Action != "", Excepted: excepted, Reason: dec.Reason,
		})

		if excepted {
			if dec.Action != "" {
				d.logger.Printf("[%s][eval] SKIP (exception) %s would be eligible for %s: %s", rt.Key, disp(report.Moniker, report.Addr), dec.Action, dec.Reason)
			}
			continue
		}
		if dec.Action == "" {
			continue
		}

		// Lifecycle gate: don't re-propose while pending/accepted(eject)/within
		// backoff or cooldown. Dry-run records count here too (LastProposal no
		// longer excludes them), so a rehearsal already dedups across cycles
		// the same way a real proposal would.
		last, _ := d.st.LastProposal(ctx, rt.Key, report.Addr)
		rejections, _ := d.st.RejectionCount(ctx, rt.Key, report.Addr)
		if g := lifecycle.Gate(last, rejections, lifecycle.Params{
			Cooldown:        rt.Proposal.CooldownDur(),
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

		d.logger.Printf("[%s][eval] ELIGIBLE %s action=%s: %s", rt.Key, disp(report.Moniker, report.Addr), dec.Action, dec.Reason)
		d.produce(ctx, rt, gno, dec, totalVP[dec.Window])
		produced++
		byAction[dec.Action]++
	}

	if err := d.st.RecordEvaluations(ctx, evals); err != nil {
		d.logger.Printf("[%s][eval] recording evaluations: %v", rt.Key, err)
	}

	if produced == 0 {
		d.logger.Printf("[%s][eval] no proposal produced (%d candidates)", rt.Key, len(reports))
	} else {
		d.logger.Printf("[%s][eval] produced %d proposal(s) (eject=%d lower_vp=%d raise_vp=%d)",
			rt.Key, produced, byAction[config.ActionEject], byAction[config.ActionLowerVP], byAction[config.ActionRaiseVP])
	}
}

// produce builds a proposal for dec.Action, then either broadcasts it
// (signing via gnokey, recording it, notifying on success) or — on a
// dry_run chain — logs and notifies the complete would-be proposal, with
// the script and a ready-to-run gnokey command attached, without touching
// the chain. totalWindowVP is dec.Window's total valset VP this cycle, used
// to compute the notification's VPDeltaPercent.
func (d *Daemon) produce(ctx context.Context, rt config.Runtime, gno *gnoland.Client, dec rules.Decision, totalWindowVP int64) {
	// A valset action is keyed by the operator address, but metrics are keyed
	// by the signing address. A missing mapping is fatal for the broadcast (we
	// would act on an unknown operator).
	operator := dec.Address
	if op, ok := d.resolverFor(rt).OperatorFor(ctx, dec.Address); ok {
		operator = op
	} else {
		d.logger.Printf("[%s][eval] no operator mapping for signing address %s; skipping broadcast", rt.Key, dec.Address)
		return
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
		return
	}

	vpDeltaPercent := 0.0
	if totalWindowVP > 0 {
		vpDeltaPercent = float64(dec.TargetPower-dec.CurrentPower) / float64(totalWindowVP) * 100
	}
	nt := notifyTarget{Address: dec.Address, Moniker: dec.Moniker, Action: dec.Action, CurrentPower: dec.CurrentPower, TargetPower: dec.TargetPower}

	if rt.DryRun {
		d.dryRun(ctx, rt, built, rec, operator, nt, vpDeltaPercent)
		return
	}

	out, err := d.signAndBroadcast(ctx, built, rt.Gnokey)
	tx := proposal.ParseTxOutput(out)
	rec.Output = out
	rec.TxHash = tx.TxHash
	outcome := proposal.Classify(out, err)
	switch outcome {
	case proposal.Committed:
		rec.Success = true
		rec.Status = store.StatusPending
		rec.OnchainID = govdao.ProposalIDFromEvents([]byte(tx.Events), d.cfg.VoteTracking.GovdaoRealm)
	case proposal.Ambiguous:
		// The tx may be on-chain: gate like a pending proposal until the
		// vote worker resolves or expires it.
		rec.Status = store.StatusUnconfirmed
	}
	id, recErr := d.st.RecordProposal(ctx, rec)
	if recErr != nil {
		// The proposal may be on-chain but we failed to persist it: the
		// lifecycle gate has no record and will re-broadcast an identical
		// proposal next cycle. Surface this loudly rather than silently spam.
		d.logger.Printf("[%s][eval] CRITICAL: broadcast %s (id=%q) but recording it failed for %s: %v — re-broadcast likely next cycle", rt.Key, outcome, rec.OnchainID, dec.Address, recErr)
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
	}
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

// executeRetry is the minimum delay between two ExecuteOrRejectProposal
// attempts on the same proposal. A failed call still burns gas, so a call that
// keeps failing must not be retried on every vote tick.
const executeRetry = time.Hour

// voteCycle follows this chain's tracked proposals: it resolves missing
// proposal ids, executes proposals that reached a supermajority, records
// GovDAO outcomes, and expires proposals left open past pending_timeout.
func (d *Daemon) voteCycle(ctx context.Context, rt config.Runtime) {
	tracked, err := d.st.TrackedProposals(ctx, rt.Key)
	if err != nil {
		d.logger.Printf("[%s][vote] %v", rt.Key, err)
		return
	}
	if len(tracked) == 0 {
		return
	}
	gd := govdao.New(rt.RPCEndpoint, d.cfg.VoteTracking.GovdaoRealm)
	for _, p := range tracked {
		d.trackProposal(ctx, rt, gd, p)
	}
}

func (d *Daemon) trackProposal(ctx context.Context, rt config.Runtime, gd *govdao.Client, p store.ProposalRecord) {
	nt := notifyTarget{Address: p.Address, Moniker: p.Moniker, Action: p.Action, CurrentPower: p.CurrentPower, TargetPower: p.TargetPower}

	if p.OnchainID == "" && p.TxHash != "" {
		if id, err := gd.TxProposalID(ctx, p.TxHash); err != nil {
			d.logger.Printf("[%s][vote] proposal row %d: resolving id from tx %s: %v", rt.Key, p.ID, p.TxHash, err)
		} else if err := d.st.SetOnchainID(ctx, p.ID, id); err != nil {
			d.logger.Printf("[%s][vote] proposal row %d: recording id %s: %v", rt.Key, p.ID, id, err)
		} else {
			d.logger.Printf("[%s][vote] proposal row %d: resolved GovDAO id %s from tx %s", rt.Key, p.ID, id, p.TxHash)
			p.OnchainID = id
		}
	}
	if p.OnchainID == "" {
		// Nothing to poll: only the timeout can close this row.
		d.expireIfDue(ctx, rt, p, nt)
		return
	}

	prop, err := gd.Proposal(ctx, p.OnchainID)
	if err != nil {
		d.logger.Printf("[%s][vote] status query failed for proposal %s: %v", rt.Key, p.OnchainID, err)
		return
	}
	switch prop.Status {
	case govdao.Accepted:
		d.resolve(ctx, rt, p, store.StatusAccepted, notify.EventAccepted, nt)
	case govdao.Rejected:
		d.resolve(ctx, rt, p, store.StatusRejected, notify.EventRejected, nt)
	case govdao.Pending:
		if rt.ExecuteProposals && prop.Executable() {
			d.execute(ctx, rt, p, prop)
		}
		d.expireIfDue(ctx, rt, p, nt)
	default:
		// Unknown is inert: never terminal, never expired, never notified.
		d.logger.Printf("[%s][vote] proposal %s: unrecognised render, keeping it %s", rt.Key, p.OnchainID, p.Status)
	}
}

// resolve records a GovDAO outcome and notifies it. The notification only
// goes out once the status is persisted, so a failed write is retried next
// tick instead of notifying the same outcome repeatedly.
func (d *Daemon) resolve(ctx context.Context, rt config.Runtime, p store.ProposalRecord, status string, event notify.Event, nt notifyTarget) {
	if err := d.st.UpdateStatus(ctx, p.ID, status, d.now()); err != nil {
		d.logger.Printf("[%s][vote] proposal %s: recording %s: %v", rt.Key, p.OnchainID, status, err)
		return
	}
	d.execMu.Lock()
	delete(d.executed, p.ID)
	d.execMu.Unlock()
	d.logger.Printf("[%s][vote] proposal %s %s — %s action=%s", rt.Key, p.OnchainID, strings.ToUpper(status), disp(p.Moniker, p.Address), p.Action)
	d.dispatch(ctx, event, rt, p.ID, nt, p.OnchainID, 0, observePayload{})
}

// expireIfDue marks a still-open proposal expired once pending_timeout has
// passed since it was broadcast. Expired rows stay tracked for a late outcome.
func (d *Daemon) expireIfDue(ctx context.Context, rt config.Runtime, p store.ProposalRecord, nt notifyTarget) {
	if p.Status == store.StatusExpired || d.now().Sub(p.CreatedAt) < rt.PendingTimeout {
		return
	}
	if err := d.st.UpdateStatus(ctx, p.ID, store.StatusExpired, d.now()); err != nil {
		d.logger.Printf("[%s][vote] proposal row %d: recording expiry: %v", rt.Key, p.ID, err)
		return
	}
	d.logger.Printf("[%s][vote] proposal %q (row %d) EXPIRED after %s — %s action=%s", rt.Key, p.OnchainID, p.ID, rt.PendingTimeout, disp(p.Moniker, p.Address), p.Action)
	d.dispatch(ctx, notify.EventExpired, rt, p.ID, nt, p.OnchainID, 0, observePayload{})
}

// execute calls ExecuteOrRejectProposal on a proposal that reached a
// supermajority, at most once per executeRetry. The outcome is read from the
// render on a later tick, like any other vote.
func (d *Daemon) execute(ctx context.Context, rt config.Runtime, p store.ProposalRecord, prop govdao.Proposal) {
	d.execMu.Lock()
	if last, ok := d.executed[p.ID]; ok && d.now().Sub(last) < executeRetry {
		d.execMu.Unlock()
		return
	}
	d.executed[p.ID] = d.now()
	d.execMu.Unlock()

	b := proposal.ExecuteCall(rt.ChainID, rt.RPCEndpoint, d.cfg.VoteTracking.GovdaoRealm, p.OnchainID)
	out, err := d.signAndBroadcast(ctx, b, rt.Gnokey)
	if outcome := proposal.Classify(out, err); outcome != proposal.Committed {
		d.logger.Printf("[%s][vote] EXECUTE %s for proposal %s (yes=%.2f%% no=%.2f%%): %v\n%s", rt.Key, outcome, p.OnchainID, prop.YesPercent, prop.NoPercent, err, out)
		return
	}
	d.logger.Printf("[%s][vote] EXECUTE ok for proposal %s (yes=%.2f%% no=%.2f%%)", rt.Key, p.OnchainID, prop.YesPercent, prop.NoPercent)
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
