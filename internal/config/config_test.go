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
conditions:
  critical_alerts:
    enabled: true
    max: 3
    interval: 168h
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
	if rt.DryRun {
		t.Error("test chain dry_run = true, want default false")
	}
	rt2, err := c.Runtime("betanet")
	if err != nil {
		t.Fatalf("Runtime(betanet): %v", err)
	}
	if !rt2.DryRun {
		t.Error("betanet dry_run = false, want true from per-chain config")
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
	_, err := Load(writeConfig(t, fmt.Sprintf(base, "")))
	if err == nil {
		t.Fatal("expected fail-start with driver none + broadcast-capable chain")
	}
	if !strings.Contains(err.Error(), "sqlite") || !strings.Contains(err.Error(), "armed") {
		t.Fatalf("error should point the operator at sqlite and name the chain, got: %v", err)
	}

	// A dry-run-only config may run without persistence.
	if _, err := Load(writeConfig(t, fmt.Sprintf(base, "dry_run: true"))); err != nil {
		t.Fatalf("dry-run-only config with driver none should load, got: %v", err)
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

func TestProposalEnabledDefaultsTrue(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML)) // validYAML sets no proposal.enabled
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := c.Runtime("test")
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if !rt.Proposal.BroadcastEnabled() {
		t.Fatal("proposal.enabled should default to true when unset")
	}
}

func TestProposalEnabledPerChainOverride(t *testing.T) {
	const yaml = `
gnomonitoring:
  base_url: "http://localhost:8989"
gnokey:
  key_name: "k"
  password: "p"
proposal:
  enabled: true
  cooldown: 720h
  rejected_backoff: 720h
storage:
  driver: sqlite
chains:
  armed:
    rpc_endpoint: "http://x:26657"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
  observe:
    rpc_endpoint: "http://y:26657"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
    proposal:
      enabled: false
      cooldown: 720h
      rejected_backoff: 720h
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	armed, _ := c.Runtime("armed")
	if !armed.Proposal.BroadcastEnabled() {
		t.Error("armed chain should have broadcasting enabled")
	}
	observe, _ := c.Runtime("observe")
	if observe.Proposal.BroadcastEnabled() {
		t.Error("observe chain should have broadcasting disabled")
	}
}
