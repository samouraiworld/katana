//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/govdao"
)

// TestLifecycle drives katana against the live gnoland-test devnet and
// asserts the proposals.status column for every vote-tracking outcome.
//
// Each subtest runs its own katana process with a fresh SQLite file, so the
// lifecycle gate of one path never blocks the next. The target is always
// validator4 (the only registered valoper in the valset). Order matters:
// accept ejects validator4, so it runs last and re-onboards it afterwards.
//
// The `dev` key is both katana's signer and the voter here: GovDAO resolves
// on ExecuteOrRejectProposal, which katana calls itself once a side holds
// the supermajority (vote_tracking.execute_proposals).
func TestLifecycle(t *testing.T) {
	requireDevnet(t)
	ensureValidator4(t)

	t.Run("deny", testDeny)
	t.Run("expire_then_late_reject", testExpire)
	t.Run("unconfirmed_without_indexer_expires", testUnconfirmed)
	t.Run("unconfirmed_reconciled_via_indexer", testReconcile)
	t.Run("guardrail_max_eligible_share", testGuardrailEligibleShare)
	t.Run("guardrail_min_valset_size", testGuardrailMinValset)
	t.Run("health", testHealth)
	t.Run("vp_change_executes", testVPChange)
	t.Run("accept", testAccept)
}

func newNonce(t *testing.T) string {
	return fmt.Sprintf("e2e-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
}

// startOnValidator4 starts katana with validator4 scored 10/100 and waits
// for its broadcast to be recorded as pending with a GovDAO id.
func startOnValidator4(t *testing.T, pendingTimeout string) (*katanaProc, proposalRow) {
	t.Helper()
	ensureValidator4(t)
	gm := fakeGnomonitoring(t, dn.addrs["VALIDATOR4_ADDR"], 10)
	k := startKatana(t, katanaOpts{rpc: dn.rpc, gnomon: gm.URL, pendingTimeout: pendingTimeout, nonce: newNonce(t)})
	row := k.waitRow(t, "a pending proposal with a GovDAO id", 90*time.Second, func(r proposalRow) bool {
		return r.Status == "pending" && r.OnchainID != ""
	})
	if !row.Success {
		t.Fatalf("pending row not marked success: %+v", row)
	}
	if p := render(t, row.OnchainID); p.Status != govdao.Pending {
		t.Fatalf("GovDAO #%s render status = %s, want pending", row.OnchainID, p.Status)
	}
	t.Logf("katana filed GovDAO #%s (row %d)", row.OnchainID, row.ID)
	return k, row
}

// deny: 100% NO -> katana calls ExecuteOrRejectProposal -> DENIED -> rejected.
func testDeny(t *testing.T) {
	k, row := startOnValidator4(t, "1h")
	vote(t, row.OnchainID, "NO")
	got := k.waitRow(t, "status rejected", 60*time.Second, func(r proposalRow) bool { return r.Status == "rejected" })
	if got.ResolvedAt == "" {
		t.Errorf("rejected row has no resolved_at: %+v", got)
	}
	if p := render(t, row.OnchainID); p.Status != govdao.Rejected {
		t.Fatalf("on-chain #%s = %s, want rejected", row.OnchainID, p.Status)
	}
	if !inValset(t, dn.addrs["VALIDATOR4_ADDR"]) {
		t.Fatalf("validator4 left the valset after a denied ejection")
	}
}

// expire: no vote for pending_timeout -> expired (once); the row stays
// tracked, so a late 100% NO still resolves it -> rejected.
func testExpire(t *testing.T) {
	k, row := startOnValidator4(t, "20s")
	k.waitRow(t, "status expired", 60*time.Second, func(r proposalRow) bool { return r.Status == "expired" })
	// No duplicate proposal while expired (waitRow fails on a second row).
	time.Sleep(8 * time.Second)
	if rs := k.rows(t); len(rs) != 1 || rs[0].Status != "expired" {
		t.Fatalf("after expiry rows = %+v", rs)
	}
	vote(t, row.OnchainID, "NO")
	k.waitRow(t, "late outcome rejected", 60*time.Second, func(r proposalRow) bool { return r.Status == "rejected" })
}

// unconfirmed: katana's RPC goes through a proxy that drops the
// broadcast_tx_commit response after the node committed the tx. gnokey prints
// no TX HASH in that case, so katana cannot resolve the id from RPC /tx and,
// with no tx_indexer_url configured, cannot reconcile either: the row stays
// unconfirmed with no id, then expires. The tx-indexer query of
// testdata/indexer_proposals_by_caller.graphql finds the proposal anyway.
func testUnconfirmed(t *testing.T) {
	ensureValidator4(t)
	start, err := latestHeight()
	if err != nil {
		t.Fatal(err)
	}
	nonce := newNonce(t)
	gm := fakeGnomonitoring(t, dn.addrs["VALIDATOR4_ADDR"], 10)
	k := startKatana(t, katanaOpts{rpc: dropBroadcastProxy(t), gnomon: gm.URL, pendingTimeout: "20s", nonce: nonce})

	row := k.waitRow(t, "status unconfirmed", 90*time.Second, func(r proposalRow) bool { return r.Status == "unconfirmed" })
	if row.Success || row.OnchainID != "" {
		t.Fatalf("unconfirmed row = %+v, want success=false and no id", row)
	}
	k.waitRow(t, "unconfirmed row expired", 60*time.Second, func(r proposalRow) bool { return r.Status == "expired" })

	// The tx did land: recover it from the indexer by signer + height + nonce.
	var pid string
	waitFor(t, "the indexer to report the proposal", 60*time.Second, func() bool {
		pid = indexerFindProposal(t, dn.addrs["DEV_ADDR"], start-1, nonce)
		return pid != ""
	})
	t.Logf("indexer recovered GovDAO #%s for the unconfirmed broadcast", pid)
	if rs := k.rows(t); rs[0].OnchainID != "" {
		t.Fatalf("row resolved without an indexer: %+v", rs[0])
	}

	// Cleanup: close the orphan proposal so it does not linger open.
	vote(t, pid, "NO")
	if out, err := callDAO("ExecuteOrRejectProposal", pid); err != nil {
		t.Logf("closing orphan #%s: %v\n%s", pid, err, out)
	}
}

// reconcile: same dropped broadcast, but katana has tx_indexer_url and
// gnokey.address: the vote worker finds the committed tx on the tx-indexer
// (signer, height above broadcast_height, payload hash), records its hash and
// GovDAO id, and the row then follows the vote like any pending proposal.
func testReconcile(t *testing.T) {
	ensureValidator4(t)
	start, err := latestHeight()
	if err != nil {
		t.Fatal(err)
	}
	nonce := newNonce(t)
	gm := fakeGnomonitoring(t, dn.addrs["VALIDATOR4_ADDR"], 10)
	k := startKatana(t, katanaOpts{rpc: dropBroadcastProxy(t), gnomon: gm.URL, pendingTimeout: "1h", nonce: nonce, indexer: true})

	row := k.waitRow(t, "the unconfirmed row reconciled via the indexer", 120*time.Second, func(r proposalRow) bool {
		return r.Status == "pending" && r.OnchainID != ""
	})
	if row.TxHash == "" || row.BroadcastHeight < start || row.PayloadHash == "" {
		t.Fatalf("reconciled row = %+v, want tx_hash, broadcast_height >= %d and payload_hash", row, start)
	}
	if !k.logContains("RECONCILED via tx-indexer") {
		t.Fatalf("no RECONCILED log line")
	}
	pid := indexerFindProposal(t, dn.addrs["DEV_ADDR"], start-1, nonce)
	if pid != row.OnchainID {
		t.Fatalf("katana reconciled GovDAO #%s, indexer has #%s", row.OnchainID, pid)
	}
	if p := render(t, pid); p.Status != govdao.Pending {
		t.Fatalf("GovDAO #%s render status = %s, want pending", pid, p.Status)
	}
	t.Logf("katana reconciled row %d to GovDAO #%s (tx %s)", row.ID, pid, row.TxHash)

	// Tracked like any pending proposal from here: 100% NO -> rejected. The
	// ExecuteOrRejectProposal response is dropped too (ambiguous), so the
	// outcome is read from the render on the next tick.
	vote(t, pid, "NO")
	k.waitRow(t, "status rejected", 90*time.Second, func(r proposalRow) bool { return r.Status == "rejected" })
	if p := render(t, pid); p.Status != govdao.Rejected {
		t.Fatalf("on-chain #%s = %s, want rejected", pid, p.Status)
	}
}

// guardrail (max_eligible_share): every validator scores 10/100, so 4 of 4
// qualify for eject, above the default 25% share. katana proposes nothing and
// alerts exactly once for the whole episode.
func testGuardrailEligibleShare(t *testing.T) {
	ensureValidator4(t)
	gm := fakeGnomonitoringFunc(t, func(string) int { return 10 })
	hook := newWebhookSink(t)
	k := startKatana(t, katanaOpts{rpc: dn.rpc, gnomon: gm.URL, pendingTimeout: "1h", nonce: newNonce(t),
		pollInterval: "2s", safety: "min_valset_size: 1\nmax_eligible_share: 25", webhook: hook.srv.URL})

	waitFor(t, "a guardrail notification", 60*time.Second, func() bool {
		return len(k.notificationEvents(t)) > 0
	})
	time.Sleep(10 * time.Second) // several more tripped cycles
	if ev := k.notificationEvents(t); len(ev) != 1 || ev[0] != "guardrail" {
		t.Fatalf("notifications = %v, want exactly one guardrail", ev)
	}
	if posts := hook.posts(); len(posts) != 1 {
		t.Fatalf("webhook got %d posts, want 1: %v", len(posts), posts)
	}
	if rs := k.rows(t); len(rs) != 0 {
		t.Fatalf("guardrail tripped but proposals were recorded: %+v", rs)
	}
	if !k.logContains("above safety.max_eligible_share=25%") {
		t.Fatalf("no max_eligible_share log line")
	}
}

// guardrail (min_valset_size): ejecting validator4 would leave 3 of 4
// validators, below min_valset_size 4: suppressed, nothing proposed.
func testGuardrailMinValset(t *testing.T) {
	ensureValidator4(t)
	gm := fakeGnomonitoring(t, dn.addrs["VALIDATOR4_ADDR"], 10)
	k := startKatana(t, katanaOpts{rpc: dn.rpc, gnomon: gm.URL, pendingTimeout: "1h", nonce: newNonce(t),
		pollInterval: "2s", safety: "min_valset_size: 4"})
	waitFor(t, "the min_valset_size suppression", 60*time.Second, func() bool {
		return k.logContains("below safety.min_valset_size=4")
	})
	time.Sleep(6 * time.Second)
	if rs := k.rows(t); len(rs) != 0 {
		t.Fatalf("min_valset_size breached: %+v", rs)
	}
}

// health: /health is 200 while every worker cycles, then 503 once the eval
// worker has failed for staleAfter (3) poll intervals because gnomonitoring
// is gone.
func testHealth(t *testing.T) {
	gm := fakeGnomonitoring(t, "", 0) // everyone healthy: nothing to propose
	addr := freeAddr(t)
	k := startKatana(t, katanaOpts{rpc: dn.rpc, gnomon: gm.URL, pendingTimeout: "1h", nonce: newNonce(t),
		pollInterval: "1s", webListen: addr})
	var code int
	var body string
	// The JSON /health body (internal/daemon/health.go healthReport).
	type workerHealth struct {
		Kind        string `json:"kind"`
		LastSuccess string `json:"last_success"`
		Stale       bool   `json:"stale"`
	}
	workers := func() map[string]workerHealth {
		var r struct {
			Workers []workerHealth `json:"workers"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("decoding /health body %q: %v", body, err)
		}
		out := map[string]workerHealth{}
		for _, w := range r.Workers {
			out[w.Kind] = w
		}
		return out
	}
	// 200 alone could still be the startup grace: wait for a real success.
	waitFor(t, "/health 200 with a successful eval cycle", 30*time.Second, func() bool {
		code, body = health(addr)
		return code == http.StatusOK && workers()["eval"].LastSuccess != ""
	})
	t.Logf("/health %d %s", code, body)
	gm.Close()
	waitFor(t, "/health 503 after gnomonitoring stopped", 30*time.Second, func() bool {
		code, body = health(addr)
		return code == http.StatusServiceUnavailable
	})
	t.Logf("/health %d %s", code, body)
	if !strings.Contains(body, `"status":"unhealthy"`) {
		t.Fatalf("503 body = %s", body)
	}
	ws := workers()
	if !ws["eval"].Stale {
		t.Fatalf("eval worker not stale: %s", body)
	}
	for kind, w := range ws {
		if w.Stale != (kind == "eval") {
			t.Fatalf("%s worker stale=%v, want only the eval worker stale: %s", kind, w.Stale, body)
		}
	}
	if rs := k.rows(t); len(rs) != 0 {
		t.Fatalf("proposals recorded: %+v", rs)
	}
}

// vp change: raise_vp then lower_vp on validator4 (power 1 of 31), each
// accepted and executed through DefaultVPChangeScript (r/sys/validators/v0
// upsert): RPC /validators shows 1 -> 2 -> 1.
func testVPChange(t *testing.T) {
	ensureValidator4(t)
	v4 := dn.addrs["VALIDATOR4_ADDR"]
	if vp := votingPower(t, v4); vp != 1 {
		t.Fatalf("validator4 voting power = %d, want 1", vp)
	}
	step := func(action, threshold string, score int, wantPower int64, selfPct, totalPct int) {
		t.Helper()
		nonce := newNonce(t) + "-" + action
		gm := fakeGnomonitoringFunc(t, func(addr string) int {
			if addr == v4 {
				return score
			}
			return 80 // neither raise (>= 90) nor lower (< 50)
		})
		actions := fmt.Sprintf(`%s:
  mode: auto
  %s
  window: current_week
  vp_step_percent_of_self: %d
  vp_step_percent_of_total: %d
  title_template: "%s {moniker} {current_power} -> {target_power} [%s]"
  description_template: "katana e2e %s. Score {score}/100 over {window}."`, action, threshold, selfPct, totalPct, action, nonce, nonce)
		k := startKatana(t, katanaOpts{rpc: dn.rpc, gnomon: gm.URL, pendingTimeout: "1h", nonce: nonce,
			actions: actions, actionPriority: "[" + action + "]"})
		row := k.waitRow(t, action+" pending with a GovDAO id", 90*time.Second, func(r proposalRow) bool {
			return r.Status == "pending" && r.OnchainID != ""
		})
		if row.Action != action || row.TargetPower != wantPower {
			t.Fatalf("%s row = %+v, want target power %d", action, row, wantPower)
		}
		vote(t, row.OnchainID, "YES")
		k.waitRow(t, action+" accepted", 60*time.Second, func(r proposalRow) bool { return r.Status == "accepted" })
		waitFor(t, fmt.Sprintf("validator4 power %d on /validators", wantPower), 30*time.Second, func() bool {
			return votingPower(t, v4) == wantPower
		})
		t.Logf("%s GovDAO #%s executed: validator4 power now %d", action, row.OnchainID, wantPower)
		gm.Close()
	}
	step("raise_vp", "score_at_or_above: 90", 99, 2, 100, 10)
	step("lower_vp", "score_below: 50", 40, 1, -50, -10)
}

// accept: 100% YES -> katana calls ExecuteOrRejectProposal -> ACCEPTED ->
// accepted, and validator4 leaves the consensus valset.
func testAccept(t *testing.T) {
	k, row := startOnValidator4(t, "1h")
	vote(t, row.OnchainID, "YES")
	k.waitRow(t, "status accepted", 60*time.Second, func(r proposalRow) bool { return r.Status == "accepted" })
	if p := render(t, row.OnchainID); p.Status != govdao.Accepted {
		t.Fatalf("on-chain #%s = %s, want accepted", row.OnchainID, p.Status)
	}
	v4 := dn.addrs["VALIDATOR4_ADDR"]
	waitFor(t, "validator4 removed from /validators", 30*time.Second, func() bool { return !inValset(t, v4) })
	// Leave the devnet re-runnable.
	t.Cleanup(func() { ensureValidator4(t) })
}

// indexerFindProposal runs the recovery query and returns the GovDAO id of the
// single successful tx signed by caller above afterHeight whose message
// content carries marker. "" when none (yet).
func indexerFindProposal(t *testing.T, caller string, afterHeight int64, marker string) string {
	t.Helper()
	q, err := os.ReadFile(filepath.Join("testdata", "indexer_proposals_by_caller.graphql"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"query":     string(q),
		"variables": map[string]any{"caller": caller, "afterHeight": afterHeight, "realm": realm},
	})
	resp, err := http.Post(dn.indexer, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("indexer: %v", err)
	}
	defer resp.Body.Close()
	var out indexerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("indexer response: %v", err)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("indexer errors: %+v", out.Errors)
	}
	var ids []string
	for _, tx := range out.Data.GetTransactions {
		match := false
		for _, m := range tx.Messages {
			if m.Value.Package != nil {
				for _, f := range m.Value.Package.Files {
					match = match || strings.Contains(f.Body, marker)
				}
			}
			for _, a := range m.Value.Args {
				match = match || strings.Contains(a, marker)
			}
		}
		if match {
			if id := govdao.ProposalIDFromEvents(tx.Response.Events, realm); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > 1 {
		t.Fatalf("indexer: %d proposals carry marker %q: %v", len(ids), marker, ids)
	}
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
