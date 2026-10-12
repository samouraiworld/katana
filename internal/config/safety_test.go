package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// guardrailYAML builds a minimal valid config with one enabled chain "test".
// global is inserted at the root, chain inside chains.test (both already
// indented by the caller where needed).
func guardrailYAML(global, chain string) string {
	return `
gnomonitoring: {base_url: "x"}
gnokey: {key_name: "k"}
storage: {driver: sqlite}
` + global + `
chains:
  test:
    rpc_endpoint: "http://x"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
` + chain
}

func loadRuntime(t *testing.T, body string) Runtime {
	t.Helper()
	c, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	return rt
}

func TestSafetyResolution(t *testing.T) {
	cases := []struct {
		name   string
		global string
		chain  string
		want   SafetyRuntime
	}{
		{
			name: "defaults",
			want: SafetyRuntime{MaxProposalsPerCycle: 1, MaxProposalsPerDay: 3, MaxEligibleShare: 25, MinValsetSize: 4},
		},
		{
			name:   "global override",
			global: "safety: {max_proposals_per_cycle: 2, max_proposals_per_day: 10, max_eligible_share: 40, min_valset_size: 7}",
			want:   SafetyRuntime{MaxProposalsPerCycle: 2, MaxProposalsPerDay: 10, MaxEligibleShare: 40, MinValsetSize: 7},
		},
		{
			name:   "per-chain field-by-field merge",
			global: "safety: {max_proposals_per_cycle: 2, max_eligible_share: 40}",
			chain:  "    safety: {max_proposals_per_day: 5, max_eligible_share: 10}\n",
			// per_cycle from global, per_day + share from chain, min_valset from default.
			want: SafetyRuntime{MaxProposalsPerCycle: 2, MaxProposalsPerDay: 5, MaxEligibleShare: 10, MinValsetSize: 4},
		},
		{
			name:  "per-chain only",
			chain: "    safety: {min_valset_size: 10}\n",
			want:  SafetyRuntime{MaxProposalsPerCycle: 1, MaxProposalsPerDay: 3, MaxEligibleShare: 25, MinValsetSize: 10},
		},
		{
			name:   "share upper bound inclusive",
			global: "safety: {max_eligible_share: 100}",
			want:   SafetyRuntime{MaxProposalsPerCycle: 1, MaxProposalsPerDay: 3, MaxEligibleShare: 100, MinValsetSize: 4},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := loadRuntime(t, guardrailYAML(tc.global, tc.chain))
			if rt.Safety != tc.want {
				t.Errorf("Safety = %+v, want %+v", rt.Safety, tc.want)
			}
		})
	}
}

func TestSafetyRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name   string
		global string
		chain  string
		field  string
	}{
		{name: "per_cycle zero", global: "safety: {max_proposals_per_cycle: 0}", field: "max_proposals_per_cycle"},
		{name: "per_cycle negative", global: "safety: {max_proposals_per_cycle: -1}", field: "max_proposals_per_cycle"},
		{name: "per_day zero", global: "safety: {max_proposals_per_day: 0}", field: "max_proposals_per_day"},
		{name: "share zero", global: "safety: {max_eligible_share: 0}", field: "max_eligible_share"},
		{name: "share negative", global: "safety: {max_eligible_share: -5}", field: "max_eligible_share"},
		{name: "share above 100", global: "safety: {max_eligible_share: 100.5}", field: "max_eligible_share"},
		// NaN fails every comparison: it would silently disable the guardrail.
		{name: "share NaN", global: "safety: {max_eligible_share: .nan}", field: "max_eligible_share"},
		{name: "share +Inf", global: "safety: {max_eligible_share: .inf}", field: "max_eligible_share"},
		{name: "min_valset zero", global: "safety: {min_valset_size: 0}", field: "min_valset_size"},
		{name: "per-chain per_day zero", chain: "    safety: {max_proposals_per_day: 0}\n", field: "max_proposals_per_day"},
		// An invalid global value must fail even when the chain overrides it.
		{name: "global invalid shadowed by chain", global: "safety: {min_valset_size: -1}", chain: "    safety: {min_valset_size: 5}\n", field: "min_valset_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, guardrailYAML(tc.global, tc.chain)))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q should name %s", err, tc.field)
			}
		})
	}
}

func TestDryRunDefaultsToTrue(t *testing.T) {
	cases := []struct {
		name   string
		global string
		chain  string
		want   bool
	}{
		{name: "unset everywhere", want: true},
		{name: "global false", global: "dry_run: false", want: false},
		{name: "global unset, chain false", chain: "    dry_run: false\n", want: false},
		{name: "global false, chain true", global: "dry_run: false", chain: "    dry_run: true\n", want: true},
		{name: "global true, chain false", global: "dry_run: true", chain: "    dry_run: false\n", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeConfig(t, guardrailYAML(tc.global, tc.chain)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			rt, err := c.Runtime("test")
			if err != nil {
				t.Fatalf("Runtime: %v", err)
			}
			if rt.DryRun != tc.want {
				t.Errorf("DryRun = %v, want %v", rt.DryRun, tc.want)
			}
			wantArmed := []string{}
			if !tc.want {
				wantArmed = []string{"test"}
			}
			if got := c.BroadcastCapableChains(); !reflect.DeepEqual(got, wantArmed) {
				t.Errorf("BroadcastCapableChains = %v, want %v", got, wantArmed)
			}
		})
	}
}

// A Config built in code (never through Load) must resolve an unset global
// dry_run the same way: nil means rehearse.
func TestDryRunNilGlobalOnUnloadedConfig(t *testing.T) {
	c := &Config{
		Gnokey: Gnokey{KeyName: "k"}, PollInterval: "5m",
		VoteTracking: VoteTracking{VotePollInterval: "5m", PendingTimeout: "1h"},
		Proposal:     Proposal{Cooldown: "1h", RejectedBackoff: "1h"},
		Chains:       map[string]*ChainConfig{"test": {RPCEndpoint: "r", ChainID: "d", GnomonitoringChain: "l", Enabled: true}},
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if !rt.DryRun {
		t.Error("DryRun = false, want true for a nil global dry_run")
	}
	if got := c.BroadcastCapableChains(); len(got) != 0 {
		t.Errorf("BroadcastCapableChains = %v, want none", got)
	}
}

func TestActionModeDefaults(t *testing.T) {
	rt := loadRuntime(t, guardrailYAML("", ""))
	want := map[string]string{ActionEject: ModeManual, ActionLowerVP: ModeAuto, ActionRaiseVP: ModeOff}
	for action, mode := range want {
		if got := rt.Proposal.Actions[action].Mode; got != mode {
			t.Errorf("%s mode = %q, want default %q", action, got, mode)
		}
	}
}

func TestActionModeMatrix(t *testing.T) {
	cases := []struct {
		dryRun bool
		mode   string
		want   string
	}{
		{false, ModeAuto, ModeAuto},
		{false, ModeManual, ModeManual},
		{false, ModeOff, ModeOff},
		{true, ModeAuto, ModeManual},
		{true, ModeManual, ModeManual},
		{true, ModeOff, ModeOff},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/dry_run="+map[bool]string{true: "true", false: "false"}[tc.dryRun], func(t *testing.T) {
			rt := Runtime{DryRun: tc.dryRun, Proposal: Proposal{Actions: map[string]ActionConfig{ActionEject: {Mode: tc.mode}}}}
			if got := rt.ActionMode(ActionEject); got != tc.want {
				t.Errorf("ActionMode = %q, want %q", got, tc.want)
			}
		})
	}
	// An action with no configured entry is never armed.
	if got := (Runtime{}).ActionMode(ActionLowerVP); got != ModeOff {
		t.Errorf("ActionMode(unconfigured) = %q, want %q", got, ModeOff)
	}
}

func TestActionModeFromLoadedConfig(t *testing.T) {
	armed := loadRuntime(t, guardrailYAML("dry_run: false\nproposal:\n  actions:\n    eject: {mode: auto}", ""))
	if got := armed.ActionMode(ActionEject); got != ModeAuto {
		t.Errorf("armed eject = %q, want auto", got)
	}
	if got := armed.ActionMode(ActionLowerVP); got != ModeAuto {
		t.Errorf("armed lower_vp = %q, want auto", got)
	}
	if got := armed.ActionMode(ActionRaiseVP); got != ModeOff {
		t.Errorf("armed raise_vp = %q, want off", got)
	}
	// Default config: dry-run, so even an auto action is manual.
	def := loadRuntime(t, guardrailYAML("", ""))
	if got := def.ActionMode(ActionLowerVP); got != ModeManual {
		t.Errorf("dry-run lower_vp = %q, want manual", got)
	}
}

func TestActionModeRejectsInvalidValue(t *testing.T) {
	for _, body := range []string{
		guardrailYAML("proposal:\n  actions:\n    eject: {mode: yolo}", ""),
		guardrailYAML("", "    proposal:\n      cooldown: 1h\n      rejected_backoff: 1h\n      actions:\n        raise_vp: {mode: Auto}\n"),
	} {
		_, err := Load(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Errorf("expected a mode validation error, got %v", err)
		}
	}
}

func TestActionPriorityFiltersOffActions(t *testing.T) {
	cases := []struct {
		name   string
		global string
		want   []string
	}{
		{name: "defaults drop raise_vp", want: []string{ActionEject, ActionLowerVP}},
		{
			name:   "raise_vp armed",
			global: "proposal:\n  actions:\n    raise_vp: {mode: auto}",
			want:   []string{ActionEject, ActionLowerVP, ActionRaiseVP},
		},
		{
			name:   "custom order preserved",
			global: "proposal:\n  action_priority: [lower_vp, raise_vp, eject]\n  actions:\n    raise_vp: {mode: manual}",
			want:   []string{ActionLowerVP, ActionRaiseVP, ActionEject},
		},
		{
			name: "all off",
			// Unquoted off must decode as the string "off", not a YAML 1.1 bool.
			global: "proposal:\n  actions:\n    eject: {mode: \"off\"}\n    lower_vp: {mode: off}",
			want:   []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeConfig(t, guardrailYAML(tc.global, "")))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			rt, err := c.Runtime("test")
			if err != nil {
				t.Fatalf("Runtime: %v", err)
			}
			if got := rt.Proposal.ActionPriority; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ActionPriority = %v, want %v", got, tc.want)
			}
			// Filtering must not leak back into the global config.
			if len(c.Proposal.ActionPriority) != 3 {
				t.Errorf("global ActionPriority mutated: %v", c.Proposal.ActionPriority)
			}
		})
	}
}

func TestCallSubmitIgnoresOffActions(t *testing.T) {
	// lower_vp off: only eject remains, so call submission is accepted.
	ok := guardrailYAML("proposal:\n  submit: call\n  actions:\n    lower_vp: {mode: \"off\"}", "")
	if _, err := Load(writeConfig(t, ok)); err != nil {
		t.Fatalf("submit: call with lower_vp off should load, got %v", err)
	}
	// Default modes (lower_vp auto) keep the eject-only call-mode invariant.
	bad := guardrailYAML("proposal:\n  submit: call", "")
	if _, err := Load(writeConfig(t, bad)); err == nil {
		t.Fatal("expected an error: submit: call with lower_vp armed")
	}
	// An unknown name is still rejected even though it has no mode.
	unknown := guardrailYAML("proposal:\n  action_priority: [eject, nope]", "")
	if _, err := Load(writeConfig(t, unknown)); err == nil {
		t.Fatal("expected an error for an unknown action_priority entry")
	}
}

func TestCooldownFor(t *testing.T) {
	rt := loadRuntime(t, guardrailYAML("proposal:\n  cooldown: 240h\n  actions:\n    lower_vp: {cooldown: 48h}", ""))
	cases := map[string]time.Duration{
		ActionEject:   240 * time.Hour,  // unset: falls back to proposal.cooldown
		ActionLowerVP: 48 * time.Hour,   // explicit
		ActionRaiseVP: 2160 * time.Hour, // built-in 90d default
		"unknown":     240 * time.Hour,
	}
	// An explicitly empty cooldown replaces the built-in default with the
	// proposal.cooldown fallback.
	rt2 := loadRuntime(t, guardrailYAML("proposal:\n  cooldown: 240h\n  actions:\n    raise_vp: {cooldown: \"\"}", ""))
	if got := rt2.Proposal.CooldownFor(ActionRaiseVP); got != 240*time.Hour {
		t.Errorf("raise_vp with cooldown \"\": CooldownFor = %s, want proposal.cooldown 240h", got)
	}
	for action, want := range cases {
		if got := rt.Proposal.CooldownFor(action); got != want {
			t.Errorf("CooldownFor(%s) = %s, want %s", action, got, want)
		}
	}
}

func TestActionCooldownMustParse(t *testing.T) {
	for _, v := range []string{"soon", "-1h"} {
		body := guardrailYAML("proposal:\n  actions:\n    eject: {cooldown: \""+v+"\"}", "")
		_, err := Load(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), "cooldown") {
			t.Errorf("cooldown %q: expected a cooldown validation error, got %v", v, err)
		}
	}
}

// A per-chain proposal block is a whole-block replace that bypasses
// applyDefaults; it must still get the mode/cooldown defaults.
func TestPerChainProposalOverrideGetsModeAndCooldownDefaults(t *testing.T) {
	chain := "    proposal:\n      cooldown: 100h\n      rejected_backoff: 100h\n      actions:\n        lower_vp: {score_below: 50}\n"
	rt := loadRuntime(t, guardrailYAML("", chain))
	want := map[string]string{ActionEject: ModeManual, ActionLowerVP: ModeAuto, ActionRaiseVP: ModeOff}
	for action, mode := range want {
		if got := rt.Proposal.Actions[action].Mode; got != mode {
			t.Errorf("%s mode = %q, want %q", action, got, mode)
		}
	}
	if got := rt.Proposal.CooldownFor(ActionRaiseVP); got != 2160*time.Hour {
		t.Errorf("raise_vp cooldown = %s, want 2160h", got)
	}
	if got := rt.Proposal.CooldownFor(ActionEject); got != 100*time.Hour {
		t.Errorf("eject cooldown = %s, want chain proposal.cooldown 100h", got)
	}
	if got := rt.Proposal.ActionPriority; !reflect.DeepEqual(got, []string{ActionEject, ActionLowerVP}) {
		t.Errorf("ActionPriority = %v, want [eject lower_vp]", got)
	}
}
