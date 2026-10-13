package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
gnomonitoring:
  base_url: "http://localhost:8989"
poll_interval: 5m
gnokey:
  key_name: "global-key"
  password: "global-pass"
  gas_fee: "1000000ugnot"
proposal:
  cooldown: 720h
  rejected_backoff: 240h
exceptions:
  - "g1protected"
storage:
  driver: sqlite
chains:
  test:
    rpc_endpoint: "http://localhost:26658"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
  betanet:
    rpc_endpoint: "http://rpc.betanet"
    chain_id: "betanet"
    gnomonitoring_chain: "gnoland1"
    enabled: true
    poll_interval: 2m
    dry_run: true
    gnokey:
      key_name: "betanet-key"
    exceptions:
      - "g1betanetonly"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestActionDefaultsAndOverride(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "http://localhost:8989"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  actions:
    lower_vp:
      score_below: 55
chains:
  test:
    rpc_endpoint: "http://x"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
    dry_run: true
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}

	// raise_vp defaults to mode off, so Runtime filters it out of the priority.
	if got := rt.Proposal.ActionPriority; len(got) != 2 || got[0] != ActionEject || got[1] != ActionLowerVP {
		t.Fatalf("ActionPriority = %v, want default [eject lower_vp] (raise_vp off)", got)
	}
	if *rt.Proposal.MinVotingPower != 100 {
		t.Errorf("MinVotingPower = %d, want default 100", *rt.Proposal.MinVotingPower)
	}
	if *rt.Proposal.MaxVotingPowerShare != 15 {
		t.Errorf("MaxVotingPowerShare = %v, want default 15", *rt.Proposal.MaxVotingPowerShare)
	}

	eject := rt.Proposal.Actions[ActionEject]
	if *eject.ScoreBelow != 30 || eject.Window != "current_month" {
		t.Errorf("eject defaults = %+v, want score_below=30 window=current_month", eject)
	}
	if eject.RunScriptTemplate != DefaultEjectScript {
		t.Errorf("eject run_script_template not defaulted to DefaultEjectScript")
	}

	// User set only score_below; window/steps/templates must still be defaulted.
	lower := rt.Proposal.Actions[ActionLowerVP]
	if *lower.ScoreBelow != 55 {
		t.Errorf("lower_vp.score_below = %d, want overridden 55", *lower.ScoreBelow)
	}
	if lower.Window != "current_week" || *lower.VPStepPercentOfSelf != -20 || *lower.VPStepPercentOfTotal != -2 {
		t.Errorf("lower_vp defaults not filled: %+v", lower)
	}
	if lower.RunScriptTemplate != DefaultVPChangeScript {
		t.Errorf("lower_vp run_script_template not defaulted to DefaultVPChangeScript")
	}

	raise := rt.Proposal.Actions[ActionRaiseVP]
	if *raise.ScoreAtOrAbove != 85 || raise.Window != "current_year" || *raise.VPStepPercentOfSelf != 20 {
		t.Errorf("raise_vp defaults = %+v", raise)
	}
}

func TestActionPriorityRejectsUnknownName(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  action_priority: [eject, not_a_real_action]
chains:
  test: {rpc_endpoint: "http://x", chain_id: "dev", gnomonitoring_chain: "local", enabled: true, dry_run: true}
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Fatal("expected an error for an unknown action_priority entry")
	}
}

func TestActionWindowMustBeAKnownPeriod(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  actions:
    eject: {window: "not_a_period"}
chains:
  test: {rpc_endpoint: "http://x", chain_id: "dev", gnomonitoring_chain: "local", enabled: true, dry_run: true}
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Fatal("expected an error for an invalid action window")
	}
}

// TestVPStepSignMustMatchActionDirection guards against a config typo (a
// missing minus sign) silently flipping lower_vp into a raise, or vice versa:
// vpTarget picks the smaller-magnitude of the two step caps with no sign
// check of its own, so Runtime() must reject a mismatched sign up front.
func TestVPStepSignMustMatchActionDirection(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  actions:
    lower_vp: {vp_step_percent_of_self: -20, vp_step_percent_of_total: 2}
chains:
  test: {rpc_endpoint: "http://x", chain_id: "dev", gnomonitoring_chain: "local", enabled: true, dry_run: true}
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Fatal("expected an error for lower_vp with a positive vp_step_percent_of_total")
	}
}

// TestExplicitZeroActionFieldIsNotOverwrittenByDefault guards against the
// zero-value/unset ambiguity: an operator setting vp_step_percent_of_total:
// 0 (meaning "no network-share cap, self-cap only") must not have it
// silently replaced by the built-in default.
func TestExplicitZeroActionFieldIsNotOverwrittenByDefault(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  actions:
    lower_vp: {vp_step_percent_of_self: -20, vp_step_percent_of_total: 0}
chains:
  test: {rpc_endpoint: "http://x", chain_id: "dev", gnomonitoring_chain: "local", enabled: true, dry_run: true}
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	lower := rt.Proposal.Actions[ActionLowerVP]
	if *lower.VPStepPercentOfTotal != 0 {
		t.Errorf("vp_step_percent_of_total = %v, want explicit 0 preserved (not defaulted to -2)", *lower.VPStepPercentOfTotal)
	}
}

// TestCallSubmitEjectOnlyAllowedWithEjectOnlyPriority proves the fix for the
// bug where the call-mode validation walked the fully-defaulted prop.Actions
// map (always containing eject, lower_vp and raise_vp) instead of
// action_priority, so submit: call was rejected even when action_priority
// only listed eject.
func TestCallSubmitEjectOnlyAllowedWithEjectOnlyPriority(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  submit: call
  action_priority: [eject]
chains:
  test: {rpc_endpoint: "http://x", chain_id: "dev", gnomonitoring_chain: "local", enabled: true, dry_run: true}
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := c.Runtime("test"); err != nil {
		t.Fatalf("Runtime: %v, want submit: call with action_priority: [eject] to be accepted", err)
	}
}

// TestCallSubmitStillRejectedWhenNonEjectActionInPriority keeps the
// eject-only-call-mode invariant: if action_priority names an action other
// than eject, submit: call must still be rejected.
func TestCallSubmitStillRejectedWhenNonEjectActionInPriority(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  submit: call
  action_priority: [eject, lower_vp]
chains:
  test: {rpc_endpoint: "http://x", chain_id: "dev", gnomonitoring_chain: "local", enabled: true, dry_run: true}
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Fatal("expected an error: submit: call with a non-eject action in action_priority")
	}
}

// TestActionWindowValidatedEvenWhenNotInActionPriority guards against
// over-scoping the window check to action_priority when the call-mode fix
// (TestCallSubmitEjectOnlyAllowedWithEjectOnlyPriority) was applied: window
// validation must still cover every action configured under proposal.actions,
// not just the ones currently listed in action_priority. Here action_priority
// is [eject] only, but raise_vp carries a bogus window and must still fail
// Load/Runtime.
func TestActionWindowValidatedEvenWhenNotInActionPriority(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
proposal:
  action_priority: [eject]
  actions:
    raise_vp: {window: "bogus"}
chains:
  test: {rpc_endpoint: "http://x", chain_id: "dev", gnomonitoring_chain: "local", enabled: true, dry_run: true}
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Fatal("expected an error: raise_vp.window is invalid even though raise_vp is not in action_priority")
	}
}

// TestCallSubmitRejectedForPerChainOverrideWithNoActionsBlock guards against
// a dead-code guard that used to skip the submit: call / non-eject check
// whenever prop.Actions had no entry for the action_priority name. A
// per-chain proposal override is a whole-block replace (bypasses
// applyDefaults), so a chain config that sets submit/action_priority without
// a nested actions: block leaves Actions nil/empty; the call-mode check must
// still apply based on action_priority alone.
func TestCallSubmitRejectedForPerChainOverrideWithNoActionsBlock(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
chains:
  test:
    rpc_endpoint: "http://x"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
    dry_run: true
    proposal:
      submit: call
      action_priority: [lower_vp]
      cooldown: 720h
      rejected_backoff: 720h
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Fatal("expected an error: per-chain submit: call with lower_vp in action_priority and no actions: block must still be rejected")
	}
}

// TestPerChainProposalOverrideStillGetsActionDefaults guards against a
// per-chain proposal: override silently disabling every action: since the
// override is a whole-block replace that bypasses applyDefaults, a chain
// that overrides proposal: to tweak just one field (here cooldown) must
// still resolve the same built-in action_priority/actions/VP-bound defaults
// the global config would have gotten — not an empty ActionPriority (zero
// proposals, ever, with no error) and zeroed MinVotingPower/MaxVotingPowerShare.
func TestPerChainProposalOverrideStillGetsActionDefaults(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
chains:
  test:
    rpc_endpoint: "http://x"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
    dry_run: true
    proposal:
      cooldown: 240h
      rejected_backoff: 240h
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}

	// raise_vp defaults to mode off, so Runtime filters it out of the priority.
	if got := rt.Proposal.ActionPriority; len(got) != 2 || got[0] != ActionEject || got[1] != ActionLowerVP {
		t.Fatalf("ActionPriority = %v, want default [eject lower_vp] (raise_vp off)", got)
	}
	if *rt.Proposal.MinVotingPower != 100 {
		t.Errorf("MinVotingPower = %d, want default 100", *rt.Proposal.MinVotingPower)
	}
	if *rt.Proposal.MaxVotingPowerShare != 15 {
		t.Errorf("MaxVotingPowerShare = %v, want default 15", *rt.Proposal.MaxVotingPowerShare)
	}
	for _, name := range []string{ActionEject, ActionLowerVP, ActionRaiseVP} {
		ac, ok := rt.Proposal.Actions[name]
		if !ok || ac.Window == "" {
			t.Errorf("Actions[%q] = %+v (ok=%v), want a defaulted entry", name, ac, ok)
		}
	}
	// The override's own field must still take effect (whole-block replace
	// preserved for what the chain DOES set).
	if rt.Proposal.CooldownDur() != 240*time.Hour {
		t.Errorf("Cooldown = %s, want overridden 240h", rt.Proposal.CooldownDur())
	}
}

func TestLoadAndRuntime(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Global chain uses global defaults.
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime(test): %v", err)
	}
	if rt.PollInterval != 5*time.Minute {
		t.Errorf("test poll_interval = %s, want 5m", rt.PollInterval)
	}
	if rt.Gnokey.KeyName != "global-key" {
		t.Errorf("test key = %s, want global-key", rt.Gnokey.KeyName)
	}

	// Per-chain overrides + gnokey field merge (password inherited from global).
	rt2, err := c.Runtime("betanet")
	if err != nil {
		t.Fatalf("Runtime(betanet): %v", err)
	}
	if rt2.PollInterval != 2*time.Minute {
		t.Errorf("betanet poll_interval = %s, want 2m", rt2.PollInterval)
	}
	if rt2.Gnokey.KeyName != "betanet-key" {
		t.Errorf("betanet key = %s, want betanet-key", rt2.Gnokey.KeyName)
	}
	if rt2.Gnokey.Password != "global-pass" {
		t.Errorf("betanet password = %s, want inherited global-pass", rt2.Gnokey.Password)
	}
	if rt2.Proposal.RejectedBackoffDur() != 240*time.Hour {
		t.Errorf("betanet backoff = %s, want 240h", rt2.Proposal.RejectedBackoffDur())
	}
}

func TestDryRunResolution(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime(test): %v", err)
	}
	if !rt.DryRun {
		t.Error("test chain dry_run = false, want default true (no global dry_run set)")
	}
	rt2, err := c.Runtime("betanet")
	if err != nil {
		t.Fatalf("Runtime(betanet): %v", err)
	}
	if !rt2.DryRun {
		t.Error("betanet dry_run = false, want true from per-chain override")
	}
}

func TestDryRunGlobalDefaultAndOverride(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
dry_run: true
storage: {driver: sqlite}
chains:
  inherits:
    rpc_endpoint: "r1"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
  overridden:
    rpc_endpoint: "r2"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
    dry_run: false
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inherits, err := c.Runtime("inherits")
	if err != nil {
		t.Fatalf("Runtime(inherits): %v", err)
	}
	if !inherits.DryRun {
		t.Error("inherits chain should inherit the global dry_run: true")
	}
	overridden, err := c.Runtime("overridden")
	if err != nil {
		t.Fatalf("Runtime(overridden): %v", err)
	}
	if overridden.DryRun {
		t.Error("overridden chain should keep its own dry_run: false despite the global default")
	}
}

func TestExceptionsUnion(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	// Global exception applies to every chain.
	if !c.IsException("test", "g1protected", "") {
		t.Error("global exception not applied to test chain")
	}
	// Per-chain exception applies only to its chain.
	if !c.IsException("betanet", "g1betanetonly", "") {
		t.Error("per-chain exception not applied to betanet")
	}
	if c.IsException("test", "g1betanetonly", "") {
		t.Error("betanet-only exception leaked to test chain")
	}
	// Moniker match: the exception entry equals the moniker (not the address).
	if !c.IsException("test", "g1other", "g1protected") {
		t.Error("expected moniker match against exception entry")
	}
}

func TestGnokeyMnemonicEnvOverride(t *testing.T) {
	t.Setenv("KATANA_GNOKEY_MNEMONIC", "word1 word2 word3")
	c := &Config{}
	c.applyEnvOverrides()
	if c.Gnokey.Mnemonic != "word1 word2 word3" {
		t.Fatalf("mnemonic = %q, want the env value", c.Gnokey.Mnemonic)
	}
}

func TestStorageNoneRequiresDryRunOnlyChains(t *testing.T) {
	const base = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: none}
chains:
  armed:
    rpc_endpoint: "r"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
    %s
`
	// A broadcast-capable chain must not run on the Noop store: without
	// lifecycle history it would re-broadcast the same ejection every cycle.
	_, err := Load(writeConfig(t, fmt.Sprintf(base, "dry_run: false")))
	if err == nil {
		t.Fatal("expected fail-start with driver none + broadcast-capable chain")
	}
	if !strings.Contains(err.Error(), "sqlite") || !strings.Contains(err.Error(), "armed") {
		t.Fatalf("error should point the operator at sqlite and name the chain, got: %v", err)
	}

	// A dry-run-only config may run without persistence, including one that
	// relies on the dry_run default (unset = true).
	for _, line := range []string{"dry_run: true", ""} {
		if _, err := Load(writeConfig(t, fmt.Sprintf(base, line))); err != nil {
			t.Fatalf("dry-run-only config (%q) with driver none should load, got: %v", line, err)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"missing gnokey key_name": `
gnomonitoring: {base_url: "x"}
chains:
  c: {rpc_endpoint: "r", chain_id: "d", gnomonitoring_chain: "l", enabled: true}
`,
		"no enabled chain": `
gnomonitoring: {base_url: "x"}
chains:
  c: {rpc_endpoint: "r", chain_id: "d", gnomonitoring_chain: "l", enabled: false}
`,
		"bad poll_interval": `
gnomonitoring: {base_url: "x"}
poll_interval: "notaduration"
chains:
  c: {rpc_endpoint: "r", chain_id: "d", gnomonitoring_chain: "l", enabled: true}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatalf("expected error for %q, got nil", name)
			}
		})
	}
}

func TestVoteTrackingDefaults(t *testing.T) {
	// Armed: a dry_run chain never executes, whatever execute_proposals says.
	c, err := Load(writeConfig(t, "dry_run: false\n"+validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if !rt.ExecuteProposals {
		t.Error("execute_proposals should default to true")
	}
	if rt.PendingTimeout != 14*24*time.Hour {
		t.Errorf("PendingTimeout = %v, want 336h", rt.PendingTimeout)
	}
}

func TestVoteTrackingPerChainOverride(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: sqlite}
dry_run: false
vote_tracking:
  execute_proposals: false
  pending_timeout: 72h
chains:
  inherits:
    rpc_endpoint: "r1"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
  overridden:
    rpc_endpoint: "r2"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
    execute_proposals: true
    pending_timeout: 24h
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inherits, err := c.Runtime("inherits")
	if err != nil {
		t.Fatalf("Runtime(inherits): %v", err)
	}
	if inherits.ExecuteProposals || inherits.PendingTimeout != 72*time.Hour {
		t.Errorf("inherits = (%v, %v), want (false, 72h)", inherits.ExecuteProposals, inherits.PendingTimeout)
	}
	overridden, err := c.Runtime("overridden")
	if err != nil {
		t.Fatalf("Runtime(overridden): %v", err)
	}
	if !overridden.ExecuteProposals || overridden.PendingTimeout != 24*time.Hour {
		t.Errorf("overridden = (%v, %v), want (true, 24h)", overridden.ExecuteProposals, overridden.PendingTimeout)
	}
}

// dry_run means nothing is signed: it also disables ExecuteOrRejectProposal,
// whatever execute_proposals says, globally or per chain.
func TestDryRunDisablesExecuteProposals(t *testing.T) {
	const yaml = `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: sqlite}
dry_run: true
vote_tracking:
  execute_proposals: true
chains:
  inherits:
    rpc_endpoint: "r1"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
  explicit:
    rpc_endpoint: "r2"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
    execute_proposals: true
  armed:
    rpc_endpoint: "r3"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
    dry_run: false
  armed_disabled:
    rpc_endpoint: "r4"
    chain_id: "d"
    gnomonitoring_chain: "l"
    enabled: true
    dry_run: false
    execute_proposals: false
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for chain, want := range map[string]bool{
		"inherits": false, "explicit": false, "armed": true, "armed_disabled": false,
	} {
		rt, err := c.Runtime(chain)
		if err != nil {
			t.Fatalf("Runtime(%s): %v", chain, err)
		}
		if rt.ExecuteProposals != want {
			t.Errorf("%s: ExecuteProposals = %v, want %v (dry_run %v)", chain, rt.ExecuteProposals, want, rt.DryRun)
		}
	}
}

func TestPendingTimeoutMustBePositive(t *testing.T) {
	for _, v := range []string{"0s", "-1h", "soon"} {
		yaml := strings.Replace(validYAML, "storage:", "vote_tracking:\n  pending_timeout: "+v+"\nstorage:", 1)
		c, err := Load(writeConfig(t, yaml))
		if err == nil {
			_, err = c.Runtime("test")
		}
		if err == nil {
			t.Errorf("pending_timeout %q: expected an error", v)
		}
	}
}

// TestDefaultScriptsTargetMainnetRealms pins the default run scripts to the
// realm paths of the gno.land chain/mainnet tag (9c8eb132e483), where
// r/sys/validators/v3 was renamed to r/sys/validators/v0. A test13 operator
// overrides run_script_template instead of relying on these defaults.
func TestDefaultScriptsTargetMainnetRealms(t *testing.T) {
	for name, script := range map[string]string{
		"DefaultEjectScript":    DefaultEjectScript,
		"DefaultVPChangeScript": DefaultVPChangeScript,
	} {
		if !strings.Contains(script, `validators "gno.land/r/sys/validators/v0"`) {
			t.Errorf("%s does not import gno.land/r/sys/validators/v0", name)
		}
		if strings.Contains(script, "validators/v3") {
			t.Errorf("%s still references the test13 realm r/sys/validators/v3", name)
		}
		if !strings.Contains(script, `"gno.land/r/gov/dao"`) {
			t.Errorf("%s does not import the gno.land/r/gov/dao proxy", name)
		}
		if !strings.Contains(script, "dao.MustCreateProposal(cross(cur), pr)") {
			t.Errorf("%s does not create the proposal through dao.MustCreateProposal", name)
		}
	}
	if !strings.Contains(DefaultEjectScript, `NewValoperChange(address("{operator_address}"), 0)`) {
		t.Errorf("DefaultEjectScript must remove the operator with Power=0")
	}
	if !strings.Contains(DefaultVPChangeScript, `NewValoperChange(address("{operator_address}"), {target_power})`) {
		t.Errorf("DefaultVPChangeScript must upsert {target_power} in a single change")
	}
}

func TestTxIndexerURL(t *testing.T) {
	const base = `
gnomonitoring: {base_url: "http://localhost:8989"}
storage: {driver: sqlite}
gnokey: {key_name: "k"%s}
chains:
  test:
    rpc_endpoint: "http://localhost:26658"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true%s
`
	load := func(gnokey, chain string) (Runtime, error) {
		t.Helper()
		c, err := Load(writeConfig(t, fmt.Sprintf(base, gnokey, chain)))
		if err != nil {
			return Runtime{}, err
		}
		return c.Runtime("test")
	}

	rt, err := load("", "")
	if err != nil || rt.TxIndexerURL != "" {
		t.Fatalf("unset: rt.TxIndexerURL=%q err=%v, want empty", rt.TxIndexerURL, err)
	}

	rt, err = load(`, address: "g1katana"`, "\n    tx_indexer_url: \"http://indexer:8546/graphql/query\"")
	if err != nil || rt.TxIndexerURL != "http://indexer:8546/graphql/query" {
		t.Fatalf("set: rt.TxIndexerURL=%q err=%v", rt.TxIndexerURL, err)
	}

	// Reconciliation queries the indexer by signer: without the key's
	// address it cannot run, so refuse the config rather than silently skip.
	if _, err := load("", "\n    tx_indexer_url: \"http://indexer:8546/graphql/query\""); err == nil ||
		!strings.Contains(err.Error(), "gnokey.address") {
		t.Fatalf("tx_indexer_url without gnokey.address: err=%v, want a gnokey.address error", err)
	}
}
