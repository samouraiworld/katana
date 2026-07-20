package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnoland"
	"github.com/samouraiworld/katana/internal/govdao"
	"github.com/samouraiworld/katana/internal/lifecycle"
	"github.com/samouraiworld/katana/internal/notify"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/rules"
	"github.com/samouraiworld/katana/internal/store"
)

// evalCycle runs one evaluation pass for a chain: fetch metrics, evaluate every
// candidate, and produce a proposal for each eligible, non-suppressed validator.
func (d *Daemon) evalCycle(ctx context.Context, rt config.Runtime) {
	// Chain-identity assert (also runs on the worker's first cycle at startup):
	// skip the whole cycle before any state is written when the node no longer
	// reports the configured chain id.
	gno := gnoland.New(rt.RPCEndpoint)
	if !d.assertChainID(ctx, gno, rt) {
		return
	}

	cond := rt.Conditions
	missed, monikers, critical, err := d.fetchMetrics(ctx, rt)
	if err != nil {
		d.logger.Printf("[%s][eval] %v", rt.Key, err)
		return
	}

	candidates := map[string]struct{}{}
	for a := range missed {
		candidates[a] = struct{}{}
	}
	for a := range critical {
		candidates[a] = struct{}{}
	}

	produced := 0
	for addr := range candidates {
		moniker := monikers[addr]
		eval := rules.Evaluate(rules.Metrics{
			Moniker:        moniker,
			Address:        addr,
			MissedBlocks:   missed[addr],
			CriticalAlerts: critical[addr],
		}, cond)

		excepted := d.cfg.IsException(rt.Key, addr, moniker)
		_ = d.st.RecordEvaluation(ctx, store.EvalRecord{
			CreatedAt: d.now(), Chain: rt.Key, Address: addr, Moniker: moniker,
			MissedBlocks: missed[addr], CriticalAlerts: critical[addr],
			Eligible: eval.Eligible, Excepted: excepted, Reason: eval.Reason(),
		})

		if excepted {
			if eval.Eligible {
				d.logger.Printf("[%s][eval] SKIP (exception) %s would be eligible: %s", rt.Key, disp(moniker, addr), eval.Reason())
			}
			continue
		}
		if !eval.Eligible {
			continue
		}

		// Lifecycle gate: don't re-propose while pending/accepted/within
		// backoff. Dry-run records count here too (LastProposal no longer
		// excludes them), so a rehearsal already dedups across cycles the
		// same way a real proposal would, and its cooldown carries over once
		// dry_run is disabled.
		last, _ := d.st.LastProposal(ctx, rt.Key, addr)
		rejections, _ := d.st.RejectionCount(ctx, rt.Key, addr)
		if dec := lifecycle.Gate(last, rejections, lifecycle.Params{
			Cooldown:        rt.Proposal.CooldownDur(),
			RejectedBackoff: rt.Proposal.RejectedBackoffDur(),
			MaxRejections:   rt.Proposal.MaxRejections,
			Now:             d.now(),
		}); !dec.Allow {
			d.logger.Printf("[%s][eval] SUPPRESS %s: %s", rt.Key, disp(moniker, addr), dec.Reason)
			continue
		}

		// Safety: confirm the validator is in the live consensus valset.
		if inSet, err := gno.IsValidator(ctx, addr); err != nil {
			d.logger.Printf("[%s][eval] valset check failed for %s: %v (skipping)", rt.Key, addr, err)
			continue
		} else if !inSet {
			d.logger.Printf("[%s][eval] %s not in valset, skipping", rt.Key, addr)
			continue
		}

		d.logger.Printf("[%s][eval] ELIGIBLE %s: %s", rt.Key, disp(moniker, addr), eval.Reason())
		d.produce(ctx, rt, gno, addr, moniker, eval.Reason())
		produced++
	}

	if produced == 0 {
		d.logger.Printf("[%s][eval] no proposal produced (%d candidates)", rt.Key, len(candidates))
	}
}

// produce builds an ejection proposal, then either broadcasts it (signing via
// gnokey, recording it, notifying on success) or — on a dry_run chain — logs
// and notifies the complete would-be proposal, with the ejection script and a
// ready-to-run gnokey command attached, without touching the chain.
func (d *Daemon) produce(ctx context.Context, rt config.Runtime, gno *gnoland.Client, addr, moniker, reason string) {
	// A valset ejection is keyed by the operator address, but metrics are keyed
	// by the signing address. A missing mapping is fatal for the broadcast (we
	// would eject an unknown operator).
	operator := addr
	if op, ok := d.resolverFor(rt).OperatorFor(ctx, addr); ok {
		operator = op
	} else {
		d.logger.Printf("[%s][eval] no operator mapping for signing address %s; skipping broadcast", rt.Key, addr)
		return
	}

	built := proposal.Build(proposal.Target{
		ChainKey:        rt.Key,
		ChainID:         rt.ChainID,
		RPC:             rt.RPCEndpoint,
		Moniker:         moniker,
		Address:         addr,
		OperatorAddress: operator,
		Reason:          reason,
	}, rt.Proposal, rt.Gnokey)

	rec := store.ProposalRecord{
		CreatedAt: d.now(), Chain: rt.Key, ChainID: rt.ChainID, Address: addr,
		Moniker: moniker, Reason: reason, Title: built.Title,
		RealmPath: built.RealmPath, Function: built.Function, Args: built.Args,
	}

	// Re-assert chain identity immediately before acting: a genesis reset can
	// land mid-cycle, after the check that opened this pass.
	if !d.assertChainID(ctx, gno, rt) {
		return
	}

	if rt.DryRun {
		d.dryRun(ctx, rt, built, rec, operator, moniker, addr)
		return
	}

	out, err := d.broadcast(ctx, built, rt.Gnokey)
	rec.Output = out
	rec.Success = err == nil
	if err == nil {
		rec.Status = store.StatusPending
		rec.OnchainID = govdao.ExtractProposalID(out)
	}
	id, recErr := d.st.RecordProposal(ctx, rec)
	if err != nil {
		d.logger.Printf("[%s][eval] broadcast failed for %s: %v", rt.Key, addr, err)
		return
	}
	if recErr != nil {
		// The proposal is already on-chain but we failed to persist it: the
		// lifecycle gate has no record and will re-broadcast an identical
		// ejection next cycle. Surface this loudly rather than silently spam.
		d.logger.Printf("[%s][eval] CRITICAL: broadcast ok (id=%q) but recording it failed for %s: %v — re-broadcast likely next cycle", rt.Key, rec.OnchainID, addr, recErr)
	}
	d.logger.Printf("[%s][eval] BROADCAST ok for %s (proposal id=%q)", rt.Key, addr, rec.OnchainID)
	d.dispatch(ctx, notify.EventSent, rt, id, addr, moniker, rec.OnchainID, observePayload{})
}

// dryRun records, logs, and notifies the proposal a dry_run chain would have
// broadcast: the ejection script/expression, a filename to attach it under,
// and a ready-to-run gnokey command — the same manual-approval hand-off a
// broadcast-capable chain's operator would get if sending failed and they had
// to act by hand. The record carries store.StatusDryRun and counts toward the
// real lifecycle gate like any other proposal record.
func (d *Daemon) dryRun(ctx context.Context, rt config.Runtime, built proposal.Built, rec store.ProposalRecord, operator, moniker, addr string) {
	code := built.Script
	if built.Submit == "call" {
		code = fmt.Sprintf("%s.%s(%s)", built.RealmPath, built.Function, strings.Join(built.Args, ", "))
	}
	filename := ejectFilename(disp(moniker, addr))
	command := built.Command(rt.Gnokey, filename)

	rec.Status = store.StatusDryRun
	rec.Success = true
	rec.Output = code
	id, recErr := d.st.RecordProposal(ctx, rec)
	if recErr != nil {
		d.logger.Printf("[%s][eval] recording dry-run proposal failed for %s: %v — expect this rehearsal to repeat next cycle", rt.Key, rec.Address, recErr)
	}
	d.logger.Printf("[%s][eval] WARN [DRY-RUN] would broadcast valset-ejection proposal: validator=%q addr=%s operator=%s chain_id=%s conditions=%q title=%q description=%q",
		rt.Key, moniker, addr, operator, rt.ChainID, rec.Reason, built.Title, built.Description)
	d.dispatch(ctx, notify.EventWouldEject, rt, id, addr, moniker, "", observePayload{code: code, filename: filename, command: command})
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

// voteCycle polls the GovDAO outcome of this chain's pending proposals.
func (d *Daemon) voteCycle(ctx context.Context, rt config.Runtime) {
	pending, err := d.st.PendingProposals(ctx, rt.Key)
	if err != nil {
		d.logger.Printf("[%s][vote] %v", rt.Key, err)
		return
	}
	if len(pending) == 0 {
		return
	}
	gd := govdao.New(rt.RPCEndpoint, d.cfg.VoteTracking.GovdaoRealm)

	for _, p := range pending {
		if p.OnchainID == "" {
			d.logger.Printf("[%s][vote] proposal row %d has no on-chain id, cannot track", rt.Key, p.ID)
			continue
		}
		status, err := gd.Status(ctx, p.OnchainID)
		if err != nil {
			d.logger.Printf("[%s][vote] status query failed for proposal %s: %v", rt.Key, p.OnchainID, err)
			continue
		}
		switch status {
		case govdao.Accepted:
			_ = d.st.UpdateStatus(ctx, p.ID, store.StatusAccepted, d.now())
			d.logger.Printf("[%s][vote] proposal %s ACCEPTED — %s removed from valset", rt.Key, p.OnchainID, disp(p.Moniker, p.Address))
			d.dispatch(ctx, notify.EventAccepted, rt, p.ID, p.Address, p.Moniker, p.OnchainID, observePayload{})
		case govdao.Rejected:
			_ = d.st.UpdateStatus(ctx, p.ID, store.StatusRejected, d.now())
			d.logger.Printf("[%s][vote] proposal %s REJECTED — %s kept in valset", rt.Key, p.OnchainID, disp(p.Moniker, p.Address))
			d.dispatch(ctx, notify.EventRejected, rt, p.ID, p.Address, p.Moniker, p.OnchainID, observePayload{})
		default:
			// pending / unknown: leave as pending, retry next tick.
		}
	}
}

// fetchMetrics pulls missed blocks and CRITICAL alert counts for a chain.
func (d *Daemon) fetchMetrics(ctx context.Context, rt config.Runtime) (missed map[string]int, monikers map[string]string, critical map[string]int, err error) {
	cond := rt.Conditions
	missed = map[string]int{}
	monikers = map[string]string{}
	critical = map[string]int{}

	if cond.MissedBlocks.Enabled {
		rows, e := d.gnomon.MissedBlocks(ctx, rt.GnomonitoringChain, cond.MissedBlocks.Interval)
		if e != nil {
			return nil, nil, nil, e
		}
		for _, r := range rows {
			missed[r.Addr] = r.MissingBlock
			monikers[r.Addr] = r.Moniker
		}
	}
	if cond.CriticalAlerts.Enabled {
		dur, _ := cond.CriticalAlerts.Duration()
		counts, e := d.gnomon.CriticalAlertCounts(ctx, rt.GnomonitoringChain, d.now().Add(-dur))
		if e != nil {
			return nil, nil, nil, e
		}
		critical = counts
	}
	return missed, monikers, critical, nil
}

// dispatch sends a notification and records each channel result.
// observePayload carries the extra fields a would_eject (observe) notification
// needs: the proposal script, the filename it is attached under, and the
// ready-to-run gnokey command. Zero value = not an observe notification.
type observePayload struct {
	code, filename, command string
}

func (d *Daemon) dispatch(ctx context.Context, event notify.Event, rt config.Runtime, proposalID int64, addr, moniker, onchainID string, obs observePayload) {
	if !d.notifier.Enabled() {
		return
	}
	results := d.notifier.Dispatch(ctx, event, notify.Info{
		Chain:            rt.Key,
		Moniker:          moniker,
		Address:          addr,
		GnowebURL:        rt.GnowebURL,
		ProposalID:       onchainID,
		RejectedBackoff:  rt.Proposal.RejectedBackoffDur(),
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
			CreatedAt: d.now(), Chain: rt.Key, Address: addr, Moniker: moniker,
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

// ejectFilename builds a safe .gno filename for the attached ejection script,
// replacing any character outside [A-Za-z0-9-_] so it is a valid attachment name.
func ejectFilename(name string) string {
	var b strings.Builder
	b.WriteString("eject-")
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
