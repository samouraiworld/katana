# Score-Driven VP Actions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace katana's binary eject-only model (driven by raw `missed_blocks`/`critical_alerts` thresholds) with three score-driven actions — `eject`, `lower_vp`, `raise_vp` — driven by gnomonitoring's computed per-validator health score, each independently configurable with its own window, threshold, and (for the two VP actions) a dual-capped fixed-step delta.

**Architecture:** `internal/gnomonitoring` fetches one report per validator per cycle (replacing two separate metric fetches). `internal/rules.Evaluate` walks a configurable action priority list and returns at most one `Decision` per validator. `internal/proposal` builds a `ValoperChange` proposal generalized to carry an arbitrary target voting power. `internal/lifecycle`, `internal/store`, and `internal/notify` become action-aware. `internal/daemon/cycle.go` wires it all together, unchanged in overall shape (fetch → evaluate → except → gate → valset-check → build → broadcast/dry-run → notify → record).

**Tech Stack:** Go, `modernc.org/sqlite` (pure-Go SQLite driver), `gopkg.in/yaml.v3`, standard library `net/http`/`net/http/httptest` for fakes.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-20-score-driven-vp-actions-design.md` — read it before starting; this plan implements it verbatim except where a task note below says otherwise.
- This is a **breaking config change**: `conditions.missed_blocks`, `conditions.critical_alerts`, `conditions.require_all`, `gnomonitoring.missed_blocks_path`, `gnomonitoring.alerts_path` are all removed, no back-compat shim.
- **Deviation from the spec's informal wording:** the spec's Config section says `proposal.actions.*` follows "the existing global-with-per-chain-override model (field-by-field merge, like today's `gnokey` block)". The *actual* existing convention for `Proposal` (see `internal/config/config.go:397-400`) is **whole-block replace**, not field merge — `ChainConfig.Proposal *Proposal` fully replaces the global `Proposal` when set, same as `Conditions` did. This plan follows the real existing code convention (whole-block replace) rather than the spec's aside, since inventing field-by-field merge for a nested `map[string]ActionConfig` would require new merge machinery nothing else in the codebase has. A chain overriding `proposal:` must restate every field/action it wants to keep.
- **Addition beyond the spec's literal text:** the spec's Store section only migrates the `proposals` table. The `evaluations` table's `EvalRecord` currently carries `MissedBlocks`/`CriticalAlerts` int fields that have no equivalent once raw metrics are gone. Task 6 replaces them with `Score`/`Tier` (additively — old columns stay in the schema, always written as `0` going forward, new `score`/`tier` columns added). This is necessary for the daemon (Task 8) to compile and keep a meaningful audit trail; flagged here since it isn't literally in the spec.
- Follow existing repo conventions throughout: table-driven tests (see `internal/rules/rules_test.go`, `internal/lifecycle/lifecycle_test.go`), offline/faked I/O (`net/http/httptest` for HTTP, temp-file SQLite, injected clock), `go vet ./...` and `go test ./... -race` clean at every commit.
- Run `go build ./...` after every task's implementation step — this plan touches shared types across 8 packages and a stale signature elsewhere will fail the build even if the local package's tests pass.

---

### Task 1: Config schema — score-driven actions

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces (used by every later task): `config.ActionEject`, `config.ActionLowerVP`, `config.ActionRaiseVP` (string constants); `config.ActionConfig` struct; `config.Proposal.ActionPriority []string`, `.MinVotingPower int64`, `.MaxVotingPowerShare float64`, `.Actions map[string]ActionConfig`; `config.DefaultVPChangeScript` constant; `config.Gnomonitoring.ReportsPath`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go` (new test, plus update `validYAML` — see Step 3 for why both must change together):

```go
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

	if got := rt.Proposal.ActionPriority; len(got) != 3 || got[0] != ActionEject || got[1] != ActionLowerVP || got[2] != ActionRaiseVP {
		t.Fatalf("ActionPriority = %v, want default [eject lower_vp raise_vp]", got)
	}
	if rt.Proposal.MinVotingPower != 100 {
		t.Errorf("MinVotingPower = %d, want default 100", rt.Proposal.MinVotingPower)
	}
	if rt.Proposal.MaxVotingPowerShare != 15 {
		t.Errorf("MaxVotingPowerShare = %v, want default 15", rt.Proposal.MaxVotingPowerShare)
	}

	eject := rt.Proposal.Actions[ActionEject]
	if eject.ScoreBelow != 30 || eject.Window != "current_month" {
		t.Errorf("eject defaults = %+v, want score_below=30 window=current_month", eject)
	}
	if eject.RunScriptTemplate != DefaultEjectScript {
		t.Errorf("eject run_script_template not defaulted to DefaultEjectScript")
	}

	// User set only score_below; window/steps/templates must still be defaulted.
	lower := rt.Proposal.Actions[ActionLowerVP]
	if lower.ScoreBelow != 55 {
		t.Errorf("lower_vp.score_below = %d, want overridden 55", lower.ScoreBelow)
	}
	if lower.Window != "current_week" || lower.VPStepPercentOfSelf != -20 || lower.VPStepPercentOfTotal != -2 {
		t.Errorf("lower_vp defaults not filled: %+v", lower)
	}
	if lower.RunScriptTemplate != DefaultVPChangeScript {
		t.Errorf("lower_vp run_script_template not defaulted to DefaultVPChangeScript")
	}

	raise := rt.Proposal.Actions[ActionRaiseVP]
	if raise.ScoreAtOrAbove != 85 || raise.Window != "current_year" || raise.VPStepPercentOfSelf != 20 {
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
```

Also replace the `conditions:` block in the shared `validYAML` fixture (used by `TestLoadAndRuntime`, `TestDryRunResolution`) since `conditions` no longer exists:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/... -run 'TestActionDefaultsAndOverride|TestActionPriorityRejectsUnknownName|TestActionWindowMustBeAKnownPeriod|TestLoadAndRuntime' -v`
Expected: compile errors (`ActionEject` undefined, `Conditions` field access in `validYAML`-dependent tests, etc.) — this is the expected "fails because it doesn't exist yet" state for a whole-package rewrite; the failure signal here is the compiler, not a test assertion.

- [ ] **Step 3: Implement the config changes**

In `internal/config/config.go`:

1. Replace the `Gnomonitoring` struct (lines 51-59):

```go
// Gnomonitoring points katana at a running gnomonitoring REST API.
type Gnomonitoring struct {
	BaseURL        string `yaml:"base_url"`
	APIToken       string `yaml:"api_token"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	ReportsPath    string `yaml:"reports_path"`
	InfoPath       string `yaml:"info_path"`
	DiscoverChains bool   `yaml:"discover_chains"`
}
```

2. Delete the `Conditions`, `MissedBlocksCond`, `CriticalCond` types and the `CriticalCond.Duration()` method (lines 103-128).

3. Add action name constants and `ActionConfig` right above the `Proposal` struct:

```go
// Score-driven action names: map keys for Proposal.Actions and the only
// valid entries in Proposal.ActionPriority.
const (
	ActionEject   = "eject"
	ActionLowerVP = "lower_vp"
	ActionRaiseVP = "raise_vp"
)

// ActionConfig configures one score-driven action. Only the threshold field
// relevant to that action's fixed semantics is read: eject/lower_vp use
// ScoreBelow (score strictly below triggers), raise_vp uses ScoreAtOrAbove
// (score at or above triggers). VPStepPercentOfSelf/OfTotal only apply to
// lower_vp/raise_vp — see internal/rules for the dual-cap step formula.
type ActionConfig struct {
	ScoreBelow     int `yaml:"score_below"`
	ScoreAtOrAbove int `yaml:"score_at_or_above"`
	// Window is the gnomonitoring reporting period this action reads its
	// score from: last_24h | current_week | current_month | current_year.
	Window                string  `yaml:"window"`
	VPStepPercentOfSelf    float64 `yaml:"vp_step_percent_of_self"`
	VPStepPercentOfTotal   float64 `yaml:"vp_step_percent_of_total"`
	RunScriptTemplate      string  `yaml:"run_script_template"`
	// call-mode fields (eject only; see internal/proposal doc comment):
	RealmPkgPath string   `yaml:"realm_pkg_path"`
	Function     string   `yaml:"function"`
	Args         []string `yaml:"args"`

	TitleTemplate       string `yaml:"title_template"`
	DescriptionTemplate string `yaml:"description_template"`
}
```

4. Replace the `Proposal` struct (lines 130-148):

```go
// Proposal describes the score-driven action model plus the lifecycle
// timers. The timers and VP bounds have global defaults, overridable per
// chain (whole-block replace — see the Runtime doc comment).
type Proposal struct {
	// ActionPriority is the order Evaluate checks actions in; the first
	// action whose threshold matches (on its own configured window) wins.
	// Defaults to [eject, lower_vp, raise_vp].
	ActionPriority []string `yaml:"action_priority"`
	// MinVotingPower is an absolute floor: lower_vp never proposes a target
	// below this. MaxVotingPowerShare is a ceiling expressed as % of the
	// chain's total valset VP (portable across networks): raise_vp never
	// proposes a target above it.
	MinVotingPower      int64   `yaml:"min_voting_power"`
	MaxVotingPowerShare float64 `yaml:"max_voting_power_share"`

	Cooldown        string `yaml:"cooldown"`
	RejectedBackoff string `yaml:"rejected_backoff"`
	MaxRejections   int    `yaml:"max_rejections"`
	// Submit selects the on-chain submission method: "run" (a .gno script via
	// gnokey maketx run) or "call" (a single MsgCall). "call" is eject-only —
	// see validate().
	Submit string `yaml:"submit"`

	Actions map[string]ActionConfig `yaml:"actions"`
}
```

5. Remove `Conditions Conditions` from both `Config` (line 29) and `ChainConfig` (line 204), and from `Runtime` (line 221).

6. In `applyDefaults()`, replace the missed-blocks/alerts path defaults with:

```go
setIfEmpty(&c.Gnomonitoring.ReportsPath, "/api/reports/validators")
```

and replace the flat proposal-template defaults (`RunScriptTemplate`, old `Cooldown`/`RejectedBackoff`/`Submit` already there) with:

```go
setIfEmpty(&c.Proposal.Cooldown, "720h")
setIfEmpty(&c.Proposal.RejectedBackoff, "720h")
setIfEmpty(&c.Proposal.Submit, "run")
if len(c.Proposal.ActionPriority) == 0 {
	c.Proposal.ActionPriority = []string{ActionEject, ActionLowerVP, ActionRaiseVP}
}
if c.Proposal.MinVotingPower == 0 {
	c.Proposal.MinVotingPower = 100
}
if c.Proposal.MaxVotingPowerShare == 0 {
	c.Proposal.MaxVotingPowerShare = 15
}
if c.Proposal.Actions == nil {
	c.Proposal.Actions = map[string]ActionConfig{}
}
c.Proposal.Actions[ActionEject] = mergeActionDefault(c.Proposal.Actions[ActionEject], ActionConfig{
	ScoreBelow: 30, Window: "current_month",
	RunScriptTemplate:   DefaultEjectScript,
	TitleTemplate:       "Eject validator {moniker} ({address}) from the valset",
	DescriptionTemplate: "Automated ejection proposal generated by katana.\nValidator: {moniker} ({address})\nScore: {score}/100 (tier {tier}) over {window}\nReason: {reason}\n",
})
c.Proposal.Actions[ActionLowerVP] = mergeActionDefault(c.Proposal.Actions[ActionLowerVP], ActionConfig{
	ScoreBelow: 60, Window: "current_week",
	VPStepPercentOfSelf: -20, VPStepPercentOfTotal: -2,
	RunScriptTemplate:   DefaultVPChangeScript,
	TitleTemplate:       "Lower voting power of validator {moniker} ({address})",
	DescriptionTemplate: "Automated voting-power reduction generated by katana.\nValidator: {moniker} ({address})\nScore: {score}/100 (tier {tier}) over {window}\nVoting power: {current_power} -> {target_power}\nReason: {reason}\n",
})
c.Proposal.Actions[ActionRaiseVP] = mergeActionDefault(c.Proposal.Actions[ActionRaiseVP], ActionConfig{
	ScoreAtOrAbove: 85, Window: "current_year",
	VPStepPercentOfSelf: 20, VPStepPercentOfTotal: 2,
	RunScriptTemplate:   DefaultVPChangeScript,
	TitleTemplate:       "Raise voting power of validator {moniker} ({address})",
	DescriptionTemplate: "Automated voting-power increase generated by katana.\nValidator: {moniker} ({address})\nScore: {score}/100 (tier {tier}) over {window}\nVoting power: {current_power} -> {target_power}\nReason: {reason}\n",
})
```

7. Add the merge helper near `setIfEmpty`:

```go
// mergeActionDefault fills any zero-valued field of cur from def, so a chain
// or global override that only sets e.g. score_below still gets sane
// defaults for window/steps/templates.
func mergeActionDefault(cur, def ActionConfig) ActionConfig {
	if cur.ScoreBelow == 0 {
		cur.ScoreBelow = def.ScoreBelow
	}
	if cur.ScoreAtOrAbove == 0 {
		cur.ScoreAtOrAbove = def.ScoreAtOrAbove
	}
	if cur.Window == "" {
		cur.Window = def.Window
	}
	if cur.VPStepPercentOfSelf == 0 {
		cur.VPStepPercentOfSelf = def.VPStepPercentOfSelf
	}
	if cur.VPStepPercentOfTotal == 0 {
		cur.VPStepPercentOfTotal = def.VPStepPercentOfTotal
	}
	if cur.RunScriptTemplate == "" {
		cur.RunScriptTemplate = def.RunScriptTemplate
	}
	if cur.RealmPkgPath == "" {
		cur.RealmPkgPath = def.RealmPkgPath
	}
	if cur.Function == "" {
		cur.Function = def.Function
	}
	if len(cur.Args) == 0 {
		cur.Args = def.Args
	}
	if cur.TitleTemplate == "" {
		cur.TitleTemplate = def.TitleTemplate
	}
	if cur.DescriptionTemplate == "" {
		cur.DescriptionTemplate = def.DescriptionTemplate
	}
	return cur
}
```

8. Add `DefaultVPChangeScript` right after `DefaultEjectScript`:

```go
// DefaultVPChangeScript is the gno.land valset voting-power-change script
// used by lower_vp/raise_vp in "run" submission mode. Same crossing-script
// pattern as DefaultEjectScript, but with an arbitrary {target_power}
// instead of a hardcoded 0. Placeholders: {operator_address} {target_power}
// {title} {description}.
const DefaultVPChangeScript = `package main

import (
	"gno.land/r/gov/dao"
	validators "gno.land/r/sys/validators/v3"
)

func main(cur realm) {
	pr := validators.NewValidatorProposalRequest(cross(cur),
		[]validators.ValoperChange{validators.NewValoperChange(address("{operator_address}"), {target_power})},
		"{title}",
		"{description}",
	)
	dao.MustCreateProposal(cross(cur), pr)
}
`
```

9. In `validate()`, remove the loop-free check that no longer applies and add action validation. Replace this block:

```go
	for _, id := range c.EnabledChains() {
		if _, err := c.Runtime(id); err != nil {
			return fmt.Errorf("chain %q: %w", id, err)
		}
	}
	return nil
}
```

with the same loop (unchanged — `Runtime()` itself now validates actions, see next step), so no edit needed here beyond what Step 10 adds inside `Runtime()`.

10. In `Runtime()`, replace the `cond := c.Conditions ...` block (lines 382-390) with action-priority/window/submit validation:

```go
	prop := c.Proposal
	if ch.Proposal != nil {
		prop = *ch.Proposal
	}
	if _, err := parseDur(prop.Cooldown); err != nil {
		return Runtime{}, fmt.Errorf("proposal.cooldown: %w", err)
	}
	if _, err := parseDur(prop.RejectedBackoff); err != nil {
		return Runtime{}, fmt.Errorf("proposal.rejected_backoff: %w", err)
	}
	for _, name := range prop.ActionPriority {
		if name != ActionEject && name != ActionLowerVP && name != ActionRaiseVP {
			return Runtime{}, fmt.Errorf("proposal.action_priority: unknown action %q", name)
		}
	}
	for name, ac := range prop.Actions {
		switch ac.Window {
		case "last_24h", "current_week", "current_month", "current_year":
		default:
			return Runtime{}, fmt.Errorf("proposal.actions.%s.window: %q is not a known gnomonitoring period", name, ac.Window)
		}
		if prop.Submit == "call" && name != ActionEject {
			return Runtime{}, fmt.Errorf("proposal.submit \"call\" is only supported for the eject action; %q must use \"run\"", name)
		}
	}
```

(This replaces the old `prop := c.Proposal; if ch.Proposal != nil { prop = *ch.Proposal }` block that already existed a few lines below — merge the two so `prop` is declared once. Delete the now-duplicate second declaration.)

Remove the now-dead `cond.CriticalAlerts.Enabled` duration-validation block entirely (it referenced the deleted `Conditions` type), and remove `Conditions: cond,` from the `Runtime{...}` struct literal at the end of the function.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS — all tests including the three new ones and the updated `TestLoadAndRuntime`/`TestDryRunResolution` (which used `validYAML`).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): score-driven action model, replacing conditions.*"
```

---

### Task 2: gnomonitoring client — FetchReports

**Files:**
- Modify: `internal/gnomonitoring/client.go`
- Create: `internal/gnomonitoring/client_test.go`

**Interfaces:**
- Consumes: `config.Gnomonitoring` (Task 1: `.ReportsPath` field).
- Produces: `gnomonitoring.Client.FetchReports(ctx, chain string) ([]Report, error)`; `gnomonitoring.Report` struct with `Addr`, `Moniker`, `Periods map[string]PeriodScore`; `gnomonitoring.PeriodScore` with `Score int`, `Tier string`, `SignRate float64`, `ProposerReliability *float64`, `VotingPower int64`, `CriticalCount`, `WarningCount`, `IncidentCount int`, `IncidentRatePerWeek float64`, `DowntimeBlocks`, `MissedBlocks int64`.

- [ ] **Step 1: Write the failing test**

Create `internal/gnomonitoring/client_test.go`:

```go
package gnomonitoring

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

func TestFetchReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/reports/validators" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("chain"); got != "test12" {
			t.Errorf("chain query = %q, want test12", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"addr":"g1ek7","moniker":"gnocore-val-01","days_since_last_alert":3,
			"periods":{"current_month":{"score":91,"tier":"Excellent","sign_rate":100,
			"proposer_reliability":88.0,"voting_power":1000,"critical_count":1,"warning_count":0,
			"incident_count":1,"incident_rate_per_week":0.47,"downtime_blocks":30,"missed_blocks":0},
			"current_week":{"score":95,"tier":"Excellent","sign_rate":100,"proposer_reliability":null,
			"voting_power":1000,"critical_count":0,"warning_count":0,"incident_count":0,
			"incident_rate_per_week":0,"downtime_blocks":0,"missed_blocks":0}}}]`))
	}))
	defer srv.Close()

	c := New(config.Gnomonitoring{BaseURL: srv.URL, ReportsPath: "/api/reports/validators", TimeoutSeconds: 5})
	reports, err := c.FetchReports(context.Background(), "test12")
	if err != nil {
		t.Fatalf("FetchReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Addr != "g1ek7" || reports[0].Moniker != "gnocore-val-01" {
		t.Fatalf("reports = %+v", reports)
	}
	month := reports[0].Periods["current_month"]
	if month.Score != 91 || month.Tier != "Excellent" || month.VotingPower != 1000 {
		t.Errorf("current_month period = %+v", month)
	}
	if month.ProposerReliability == nil || *month.ProposerReliability != 88.0 {
		t.Errorf("current_month proposer_reliability = %v, want 88.0", month.ProposerReliability)
	}
	week := reports[0].Periods["current_week"]
	if week.ProposerReliability != nil {
		t.Errorf("current_week proposer_reliability = %v, want nil (too few samples)", *week.ProposerReliability)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gnomonitoring/... -run TestFetchReports -v`
Expected: FAIL — compile error (`FetchReports`/`Report` undefined).

- [ ] **Step 3: Rewrite the client**

Replace the entire contents of `internal/gnomonitoring/client.go`:

```go
// Package gnomonitoring is a thin HTTP client for the gnomonitoring REST API.
//
// katana does not compute validator metrics itself; it reads a computed
// per-validator health score from a running gnomonitoring instance
// (https://github.com/samouraiworld/gnomonitoring), via
// GET /api/reports/validators?chain=<chain> — a public endpoint listing every
// validator currently in the chain's valset, with score/tier/sub-metrics over
// four rolling/calendar windows.
package gnomonitoring

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Client talks to a gnomonitoring backend.
type Client struct {
	baseURL     string
	token       string
	http        *http.Client
	reportsPath string
}

// New builds a Client from the gnomonitoring section of the config.
func New(cfg config.Gnomonitoring) *Client {
	return &Client{
		baseURL:     cfg.BaseURL,
		token:       cfg.APIToken,
		reportsPath: cfg.ReportsPath,
		http:        &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second},
	}
}

// PeriodScore is one validator's health metrics over one reporting window
// (last_24h | current_week | current_month | current_year). ProposerReliability
// is nil when the validator's expected proposal count is too low to be a
// meaningful signal (dropped from gnomonitoring's own score in that case).
type PeriodScore struct {
	Score               int      `json:"score"`
	Tier                string   `json:"tier"`
	SignRate            float64  `json:"sign_rate"`
	ProposerReliability *float64 `json:"proposer_reliability"`
	VotingPower         int64    `json:"voting_power"`
	CriticalCount       int      `json:"critical_count"`
	WarningCount        int      `json:"warning_count"`
	IncidentCount       int      `json:"incident_count"`
	IncidentRatePerWeek float64  `json:"incident_rate_per_week"`
	DowntimeBlocks      int64    `json:"downtime_blocks"`
	MissedBlocks        int64    `json:"missed_blocks"`
}

// Report is one validator's entry in gnomonitoring's per-validator health
// report. A validator that has left the valset is excluded entirely by
// gnomonitoring, so every entry here is a live candidate.
type Report struct {
	Addr               string                 `json:"addr"`
	Moniker            string                 `json:"moniker"`
	DaysSinceLastAlert *int                   `json:"days_since_last_alert"`
	Periods            map[string]PeriodScore `json:"periods"`
}

// FetchReports returns the health report for every validator currently in
// chain's valset.
func (c *Client) FetchReports(ctx context.Context, chain string) ([]Report, error) {
	q := url.Values{}
	q.Set("chain", chain)

	var out []Report
	if err := c.getJSON(ctx, c.reportsPath, q, &out); err != nil {
		return nil, fmt.Errorf("fetching validator reports: %w", err)
	}
	return out, nil
}

// getJSON performs a GET against path?query and decodes the JSON body into out.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", endpoint, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", endpoint, err)
	}
	return nil
}
```

Note: the bearer token is still attached when `api_token` is configured, even though `/api/reports/validators` is public — reusing the same `getJSON` helper as any future admin-protected endpoint is simpler than branching, and a stray auth header on a public endpoint is harmless.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gnomonitoring/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/gnomonitoring/client.go internal/gnomonitoring/client_test.go
git commit -m "feat(gnomonitoring): FetchReports replaces missed-blocks/alerts calls"
```

---

### Task 3: Rules — score-driven Evaluate

**Files:**
- Modify: `internal/rules/rules.go`
- Modify: `internal/rules/rules_test.go`

**Interfaces:**
- Consumes: `gnomonitoring.Report`/`PeriodScore` (Task 2); `config.ActionConfig`, `config.ActionEject/LowerVP/RaiseVP` (Task 1).
- Produces (used by Tasks 4, 8): `rules.Decision{Address, Moniker, Action string, Score int, Tier, Window string, CurrentPower, TargetPower int64, Reason string}`; `rules.Evaluate(r gnomonitoring.Report, priority []string, actions map[string]config.ActionConfig, minVP int64, maxVPShare float64, totalWindowVP map[string]int64) Decision`; `rules.TotalVotingPower(reports []gnomonitoring.Report, window string) int64`.

- [ ] **Step 1: Write the failing test**

Replace `internal/rules/rules_test.go` entirely:

```go
package rules

import (
	"testing"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnomonitoring"
)

func actions() map[string]config.ActionConfig {
	return map[string]config.ActionConfig{
		config.ActionEject: {ScoreBelow: 30, Window: "current_month"},
		config.ActionLowerVP: {
			ScoreBelow: 60, Window: "current_week",
			VPStepPercentOfSelf: -20, VPStepPercentOfTotal: -2,
		},
		config.ActionRaiseVP: {
			ScoreAtOrAbove: 85, Window: "current_year",
			VPStepPercentOfSelf: 20, VPStepPercentOfTotal: 2,
		},
	}
}

func defaultPriority() []string {
	return []string{config.ActionEject, config.ActionLowerVP, config.ActionRaiseVP}
}

func report(addr string, periods map[string]gnomonitoring.PeriodScore) gnomonitoring.Report {
	return gnomonitoring.Report{Addr: addr, Moniker: "m-" + addr, Periods: periods}
}

func TestEvaluate_ActionThresholds(t *testing.T) {
	tests := []struct {
		name       string
		r          gnomonitoring.Report
		wantAction string
	}{
		{
			name: "score below eject threshold on its window",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 29, VotingPower: 1000},
			}),
			wantAction: config.ActionEject,
		},
		{
			name: "score at eject threshold does not trigger (strict <)",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 30, VotingPower: 1000},
				"current_week":  {Score: 30, VotingPower: 1000}, // also not < 60, no lower_vp either
			}),
			wantAction: "",
		},
		{
			name: "score in the 60-84 stability zone has no action",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 70, VotingPower: 1000},
				"current_week":  {Score: 70, VotingPower: 1000},
				"current_year":  {Score: 70, VotingPower: 1000},
			}),
			wantAction: "",
		},
		{
			name: "score at raise threshold triggers (>=)",
			r: report("g1a", map[string]gnomonitoring.PeriodScore{
				"current_month": {Score: 90, VotingPower: 1000},
				"current_week":  {Score: 90, VotingPower: 1000},
				"current_year":  {Score: 85, VotingPower: 1000},
			}),
			wantAction: config.ActionRaiseVP,
		},
	}
	totalVP := map[string]int64{"current_month": 20000, "current_week": 20000, "current_year": 20000}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.r, defaultPriority(), actions(), 100, 15, totalVP)
			if got.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q (reason: %s)", got.Action, tt.wantAction, got.Reason)
			}
		})
	}
}

func TestEvaluate_PriorityOrderIsConfigurable(t *testing.T) {
	// A validator whose current_month score also happens to clear the
	// eject band AND (independently) whose current_year score clears the
	// raise band — reordering priority changes which one wins.
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_month": {Score: 10, VotingPower: 1000},
		"current_year":  {Score: 90, VotingPower: 1000},
	})
	totalVP := map[string]int64{"current_month": 20000, "current_year": 20000}

	got := Evaluate(r, []string{config.ActionEject, config.ActionRaiseVP}, actions(), 100, 15, totalVP)
	if got.Action != config.ActionEject {
		t.Fatalf("default order: Action = %q, want eject", got.Action)
	}

	got = Evaluate(r, []string{config.ActionRaiseVP, config.ActionEject}, actions(), 100, 15, totalVP)
	if got.Action != config.ActionRaiseVP {
		t.Fatalf("reordered priority: Action = %q, want raise_vp", got.Action)
	}
}

func TestEvaluate_DualCapStep(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}

	// Small validator: self-cap (20% of 100 = 20) is tighter than the
	// network cap (2% of 20000 = 400) — self-cap wins.
	small := report("g1small", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 100},
	})
	got := Evaluate(small, defaultPriority(), actions(), 10, 15, totalVP)
	if got.Action != config.ActionLowerVP || got.TargetPower != 80 {
		t.Fatalf("small validator: Action=%q TargetPower=%d, want lower_vp 80", got.Action, got.TargetPower)
	}

	// Large validator: self-cap (20% of 5000 = 1000) is looser than the
	// network cap (2% of 20000 = 400) — network cap wins.
	large := report("g1large", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 5000},
	})
	got = Evaluate(large, defaultPriority(), actions(), 10, 15, totalVP)
	if got.Action != config.ActionLowerVP || got.TargetPower != 4600 {
		t.Fatalf("large validator: Action=%q TargetPower=%d, want lower_vp 4600", got.Action, got.TargetPower)
	}
}

func TestEvaluate_ClampToMinVotingPower(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 110}, // step would be -20 (self-cap), -> 90, below floor 100
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != config.ActionLowerVP || got.TargetPower != 100 {
		t.Fatalf("Action=%q TargetPower=%d, want lower_vp clamped to floor 100", got.Action, got.TargetPower)
	}
}

func TestEvaluate_NoopClampClearsAction(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 100}, // already at the floor
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != "" {
		t.Fatalf("Action = %q, want \"\" (no-op: already at min_voting_power)", got.Action)
	}
}

func TestEvaluate_MissingWindowDataSkipsAction(t *testing.T) {
	totalVP := map[string]int64{"current_week": 20000}
	// No current_month entry at all -> eject can't be evaluated, falls
	// through to lower_vp on current_week.
	r := report("g1a", map[string]gnomonitoring.PeriodScore{
		"current_week": {Score: 40, VotingPower: 1000},
	})
	got := Evaluate(r, defaultPriority(), actions(), 100, 15, totalVP)
	if got.Action != config.ActionLowerVP {
		t.Fatalf("Action = %q, want lower_vp (eject's window has no data)", got.Action)
	}
}

func TestTotalVotingPower(t *testing.T) {
	reports := []gnomonitoring.Report{
		report("g1a", map[string]gnomonitoring.PeriodScore{"current_week": {VotingPower: 1000}}),
		report("g1b", map[string]gnomonitoring.PeriodScore{"current_week": {VotingPower: 2500}}),
	}
	if got := TotalVotingPower(reports, "current_week"); got != 3500 {
		t.Fatalf("TotalVotingPower = %d, want 3500", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/rules/... -v`
Expected: compile errors (`Evaluate`/`Decision`/`TotalVotingPower` signatures don't exist yet).

- [ ] **Step 3: Rewrite rules.go**

Replace `internal/rules/rules.go` entirely:

```go
// Package rules decides, per validator per cycle, which score-driven action
// (if any) applies: eject, lower_vp, or raise_vp. It is pure logic over a
// gnomonitoring report and the resolved action config — no I/O, no history.
package rules

import (
	"fmt"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/gnomonitoring"
)

// Decision is the outcome of evaluating one validator's report. Action is ""
// when no configured action's threshold matched.
type Decision struct {
	Address, Moniker string
	Action           string // config.ActionEject | ActionLowerVP | ActionRaiseVP | ""
	Score            int
	Tier             string
	Window           string
	CurrentPower     int64
	TargetPower      int64 // 0 for eject
	Reason           string
}

// TotalVotingPower sums every report's voting power for the given window,
// used to clamp lower_vp/raise_vp targets against a % of the chain's total
// valset VP. Missing entries for that window contribute 0.
func TotalVotingPower(reports []gnomonitoring.Report, window string) int64 {
	var total int64
	for _, r := range reports {
		total += r.Periods[window].VotingPower
	}
	return total
}

// Evaluate walks priority in order and returns the first action whose
// threshold matches on its own configured window. totalWindowVP maps a
// window name to that window's total valset voting power (TotalVotingPower),
// needed to clamp lower_vp/raise_vp targets against maxVPShare.
func Evaluate(r gnomonitoring.Report, priority []string, actions map[string]config.ActionConfig,
	minVP int64, maxVPShare float64, totalWindowVP map[string]int64) Decision {

	d := Decision{Address: r.Addr, Moniker: r.Moniker}

	for _, name := range priority {
		ac, ok := actions[name]
		if !ok {
			continue
		}
		period, ok := r.Periods[ac.Window]
		if !ok {
			continue
		}

		switch name {
		case config.ActionEject:
			if period.Score < ac.ScoreBelow {
				d.Action = name
				d.Score, d.Tier, d.Window = period.Score, period.Tier, ac.Window
				d.CurrentPower = period.VotingPower
				d.Reason = reason(period, ac.Window)
				return d
			}

		case config.ActionLowerVP, config.ActionRaiseVP:
			matches := period.Score < ac.ScoreBelow
			if name == config.ActionRaiseVP {
				matches = period.Score >= ac.ScoreAtOrAbove
			}
			if !matches {
				continue
			}
			total := totalWindowVP[ac.Window]
			target := vpTarget(period.VotingPower, ac, minVP, maxVPShare, total)
			if target == period.VotingPower {
				// Clamped to a no-op: already at a bound, nothing to propose.
				return d
			}
			d.Action = name
			d.Score, d.Tier, d.Window = period.Score, period.Tier, ac.Window
			d.CurrentPower = period.VotingPower
			d.TargetPower = target
			d.Reason = reason(period, ac.Window)
			return d
		}
	}
	return d
}

// vpTarget computes the clamped target voting power for a lower_vp/raise_vp
// action: current power adjusted by the smaller-magnitude of two step caps
// (a % of the validator's own current power, and a % of the chain's total
// valset power — see the spec's "VP delta model"), then clamped to
// [minVP, maxVPShare% of total].
func vpTarget(current int64, ac config.ActionConfig, minVP int64, maxVPShare float64, total int64) int64 {
	capSelf := ac.VPStepPercentOfSelf / 100 * float64(current)
	capTotal := ac.VPStepPercentOfTotal / 100 * float64(total)
	step := capSelf
	if absf(capTotal) < absf(capSelf) {
		step = capTotal
	}
	target := current + roundf(step)

	ceiling := int64(maxVPShare / 100 * float64(total))
	if target < minVP {
		target = minVP
	}
	if target > ceiling {
		target = ceiling
	}
	return target
}

func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func roundf(f float64) int64 {
	if f < 0 {
		return int64(f - 0.5)
	}
	return int64(f + 0.5)
}

// reason renders a human-readable explanation embedding the deciding score
// and gnomonitoring's sub-metrics for that window, for use in proposal
// descriptions and notifications. Score stays the only eligibility signal;
// these sub-metrics are display-only context (see the spec).
func reason(p gnomonitoring.PeriodScore, window string) string {
	return fmt.Sprintf("score %d/100 (tier %s) over %s — sign_rate %.0f%%, %.1f incidents/week, %d downtime blocks",
		p.Score, p.Tier, window, p.SignRate, p.IncidentRatePerWeek, p.DowntimeBlocks)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/rules/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/rules/rules.go internal/rules/rules_test.go
git commit -m "feat(rules): score-driven Evaluate with configurable priority and dual-cap VP step"
```

---

### Task 4: Proposal — generalized Target/Build + VP-change script

**Files:**
- Modify: `internal/proposal/proposal.go`
- Modify: `internal/proposal/proposal_test.go`

**Interfaces:**
- Consumes: `config.Proposal.Actions` (Task 1); `rules.Decision` fields (Task 3, via daemon wiring in Task 8 — this task itself only needs the field names, not the type).
- Produces: `proposal.Target` gains `Action, Tier, Window string`, `Score int`, `CurrentPower, TargetPower int64`; `Build(t Target, p config.Proposal, gk config.Gnokey) Built` resolves `p.Actions[t.Action]` internally instead of reading flat `p.TitleTemplate` etc.

- [ ] **Step 1: Write the failing test**

Replace `internal/proposal/proposal_test.go` entirely:

```go
package proposal

import (
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

func target(action string) Target {
	return Target{
		ChainKey: "local", ChainID: "dev", RPC: "http://localhost:26658",
		Moniker: "bad-node", Address: "g1sign", OperatorAddress: "g1op",
		Reason: "score 20/100 (tier Critical) over current_month",
		Action: action, Score: 20, Tier: "Critical", Window: "current_month",
		CurrentPower: 1000, TargetPower: 0,
	}
}

func ejectProposalConfig() config.Proposal {
	return config.Proposal{
		Submit: "run",
		Actions: map[string]config.ActionConfig{
			config.ActionEject: {
				RunScriptTemplate:   config.DefaultEjectScript,
				TitleTemplate:       "Eject {moniker} ({operator_address})",
				DescriptionTemplate: "Reason: {reason}\n\"quoted\"",
			},
		},
	}
}

func TestBuildRunMode(t *testing.T) {
	p := ejectProposalConfig()
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1000000ugnot", GasWanted: 20000000}

	tg := target(config.ActionEject)
	b := Build(tg, p, gk)
	if b.Submit != "run" {
		t.Fatalf("submit = %s, want run", b.Submit)
	}
	if !strings.Contains(b.Script, `address("g1op")`) {
		t.Errorf("script missing operator address:\n%s", b.Script)
	}
	if !strings.Contains(b.Script, "NewValoperChange") || !strings.Contains(b.Script, ", 0)") {
		t.Errorf("script is not a Power=0 removal:\n%s", b.Script)
	}
	if !strings.Contains(b.Script, `\"quoted\"`) || strings.Contains(b.Script, "Reason: score 20/100 (tier Critical) over current_month\n\"quoted\"") {
		t.Errorf("description not escaped for Go literal:\n%s", b.Script)
	}

	args := b.runArgs(gk, "/tmp/x.gno")
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "maketx run") || !strings.HasSuffix(joined, "dev /tmp/x.gno") {
		t.Errorf("unexpected run args: %v", args)
	}
	if !strings.Contains(joined, "-insecure-password-stdin") {
		t.Errorf("run args missing -insecure-password-stdin: %v", args)
	}
}

func TestBuildLowerVPRunMode(t *testing.T) {
	p := config.Proposal{
		Submit: "run",
		Actions: map[string]config.ActionConfig{
			config.ActionLowerVP: {
				RunScriptTemplate:   config.DefaultVPChangeScript,
				TitleTemplate:       "Lower {moniker}",
				DescriptionTemplate: "{current_power} -> {target_power}",
			},
		},
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}

	tg := target(config.ActionLowerVP)
	tg.CurrentPower, tg.TargetPower = 1000, 600
	b := Build(tg, p, gk)

	if !strings.Contains(b.Script, `address("g1op")`) {
		t.Errorf("script missing operator address:\n%s", b.Script)
	}
	if !strings.Contains(b.Script, ", 600)") {
		t.Errorf("script does not carry the target power 600:\n%s", b.Script)
	}
	if strings.Contains(b.Script, ", 0)") {
		t.Errorf("script must not be a Power=0 removal for lower_vp:\n%s", b.Script)
	}
	if !strings.Contains(b.Description, "1000 -> 600") {
		t.Errorf("description missing current->target power: %s", b.Description)
	}
}

func TestBuildRaiseVPRunMode(t *testing.T) {
	p := config.Proposal{
		Submit: "run",
		Actions: map[string]config.ActionConfig{
			config.ActionRaiseVP: {
				RunScriptTemplate:   config.DefaultVPChangeScript,
				TitleTemplate:       "Raise {moniker}",
				DescriptionTemplate: "score={score} tier={tier} window={window}",
			},
		},
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}

	tg := target(config.ActionRaiseVP)
	tg.Score, tg.Tier, tg.Window = 90, "Excellent", "current_year"
	tg.CurrentPower, tg.TargetPower = 1000, 1200
	b := Build(tg, p, gk)

	if !strings.Contains(b.Script, ", 1200)") {
		t.Errorf("script does not carry the target power 1200:\n%s", b.Script)
	}
	if b.Description != "score=90 tier=Excellent window=current_year" {
		t.Errorf("description = %q", b.Description)
	}
}

func TestBuildCallMode(t *testing.T) {
	p := config.Proposal{
		Submit: "call",
		Actions: map[string]config.ActionConfig{
			config.ActionEject: {
				RealmPkgPath:  "gno.land/r/x",
				Function:      "RemoveValidator",
				Args:          []string{"{operator_address}", "{moniker}"},
				TitleTemplate: "t",
			},
		},
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}

	b := Build(target(config.ActionEject), p, gk)
	if b.Submit != "call" {
		t.Fatalf("submit = %s, want call", b.Submit)
	}
	if b.Args[0] != "g1op" || b.Args[1] != "bad-node" {
		t.Errorf("args not rendered: %v", b.Args)
	}
	args := b.callArgs(gk)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "maketx call -pkgpath gno.land/r/x -func RemoveValidator") {
		t.Errorf("unexpected call args: %v", args)
	}
}

func TestCommandRunMode(t *testing.T) {
	p := ejectProposalConfig()
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1000000ugnot", GasWanted: 20000000, Home: "/app/data/keybase", Password: "s3cret"}
	b := Build(target(config.ActionEject), p, gk)

	cmd := b.Command(gk, "eject-bad-node.gno")
	for _, want := range []string{
		"gnokey maketx run", "-remote http://localhost:26658", "-chainid dev",
		"-insecure-password-stdin", "-home /app/data/keybase", "eject-bad-node.gno",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "s3cret") {
		t.Errorf("password must not appear in the command: %s", cmd)
	}
	if !strings.Contains(cmd, `"$KEYBASE_PASSWORD"`) {
		t.Errorf("command should pipe the password from an env var: %s", cmd)
	}
}

func TestOperatorFallsBackToAddress(t *testing.T) {
	tg := target(config.ActionEject)
	tg.OperatorAddress = ""
	b := Build(tg, ejectProposalConfig(), config.Gnokey{})
	if !strings.Contains(b.Script, `address("g1sign")`) {
		t.Errorf("expected fallback to signing address:\n%s", b.Script)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/proposal/... -v`
Expected: compile errors (`Target` has no field `Action`, `config.Proposal` has no field `Actions`, etc.).

- [ ] **Step 3: Implement**

In `internal/proposal/proposal.go`:

1. Replace the `Target` struct:

```go
// Target identifies the validator a proposal is built against and the
// score-driven action to take.
type Target struct {
	ChainKey string
	ChainID  string
	RPC      string
	Moniker  string
	Address  string // consensus/signing address (from the report)
	// OperatorAddress is the valoper operator address the valset proposal
	// must reference. Falls back to Address when empty.
	OperatorAddress string
	Reason          string

	Action       string // config.ActionEject | ActionLowerVP | ActionRaiseVP
	Score        int
	Tier         string
	Window       string
	CurrentPower int64
	TargetPower  int64 // 0 for eject
}
```

2. Replace `Build`:

```go
// Build renders the proposal templates for target.Action's configured
// submission method.
func Build(t Target, p config.Proposal, gk config.Gnokey) Built {
	if t.OperatorAddress == "" {
		t.OperatorAddress = t.Address
	}
	ac := p.Actions[t.Action]
	vars := map[string]string{
		"address":          t.Address,
		"operator_address": t.OperatorAddress,
		"moniker":          t.Moniker,
		"reason":           t.Reason,
		"chain":            t.ChainKey,
		"realm":            ac.RealmPkgPath,
		"score":            strconv.Itoa(t.Score),
		"tier":             t.Tier,
		"window":           t.Window,
		"current_power":    strconv.FormatInt(t.CurrentPower, 10),
		"target_power":     strconv.FormatInt(t.TargetPower, 10),
	}
	title := render(ac.TitleTemplate, vars)
	description := render(ac.DescriptionTemplate, vars)

	b := Built{
		Target:      t,
		Submit:      submitMethod(p),
		Title:       title,
		Description: description,
		RealmPath:   ac.RealmPkgPath,
		Function:    ac.Function,
	}

	if b.Submit == "run" {
		// Inside a .gno script, title/description are embedded in Go string
		// literals, so they must be escaped (quotes/newlines).
		svars := cloneVars(vars)
		svars["title"] = goInner(title)
		svars["description"] = goInner(description)
		b.Script = render(ac.RunScriptTemplate, svars)
	} else {
		vars["title"] = title
		vars["description"] = description
		b.Args = make([]string, len(ac.Args))
		for i, a := range ac.Args {
			b.Args[i] = render(a, vars)
		}
	}
	return b
}
```

`submitMethod` is unchanged (still reads `p.Submit`, global — `call` mode is validated eject-only in `config.Runtime()`, Task 1 Step 3.10, so `Build` doesn't need to re-check it).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/proposal/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/proposal/proposal.go internal/proposal/proposal_test.go
git commit -m "feat(proposal): generalize Build to eject/lower_vp/raise_vp actions"
```

---

### Task 5: Lifecycle — action-aware Gate

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Modify: `internal/lifecycle/lifecycle_test.go`
- Modify: `internal/store/store.go` (add `Action string` to `ProposalRecord` — done here since `lifecycle_test.go` needs it; Task 6 does the rest of the store work)

**Interfaces:**
- Consumes: `store.ProposalRecord.Action` (added in this task's Step 3a); `config.ActionEject/LowerVP/RaiseVP` (Task 1).
- Produces: `lifecycle.Gate` behavior change — accepted `lower_vp`/`raise_vp` falls through to cooldown instead of being terminal.

- [ ] **Step 1: Write the failing test**

Add to `internal/lifecycle/lifecycle_test.go` (new cases, appended to the existing `tests` table in `TestGate` — insert after the `"accepted is terminal (already ejected)"` case):

```go
			{
				name:      "accepted eject stays terminal",
				last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionEject},
				wantAllow: false,
			},
			{
				name:      "accepted lower_vp within cooldown is blocked",
				last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionLowerVP, CreatedAt: now.Add(-1 * time.Hour)},
				wantAllow: false,
			},
			{
				name:      "accepted lower_vp after cooldown is allowed",
				last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionLowerVP, CreatedAt: now.Add(-800 * time.Hour)},
				wantAllow: true,
			},
			{
				name:      "accepted raise_vp after cooldown is allowed",
				last:      &store.ProposalRecord{Status: store.StatusAccepted, Action: config.ActionRaiseVP, CreatedAt: now.Add(-800 * time.Hour)},
				wantAllow: true,
			},
```

Add the `config` import to the test file's import block.

Also add a standalone test confirming rejection count is shared across action
types for the same validator (the spec requires this explicitly): `Gate`
itself never reads `last.Action` in the `StatusRejected` branch, and
`store.RejectionCount`'s query (`WHERE chain=? AND address=? AND status=?`,
Task 6) has no `action` filter either — so a rejected `lower_vp` counts
toward the same budget as a rejected `eject` would. Add:

```go
func TestRejectedCountIsSharedAcrossActionTypes(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	params := Params{Cooldown: 720 * time.Hour, RejectedBackoff: 720 * time.Hour, MaxRejections: 1, Now: now}

	// A lower_vp proposal was rejected once; the cap (1) is already reached,
	// regardless of what action a fresh evaluation would pick this cycle.
	last := &store.ProposalRecord{Status: store.StatusRejected, Action: config.ActionLowerVP, ResolvedAt: now.Add(-800 * time.Hour)}
	got := Gate(last, 1, params)
	if got.Allow {
		t.Fatalf("expected max_rejections to block regardless of action type, got Allow=true (reason: %q)", got.Reason)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/... -v`
Expected: compile error (`store.ProposalRecord` has no field `Action`).

- [ ] **Step 3a: Add `Action` to `store.ProposalRecord`**

In `internal/store/store.go`, add one field to the `ProposalRecord` struct (right after `Moniker`):

```go
	Moniker string
	// Action is the score-driven action this proposal represents: eject |
	// lower_vp | raise_vp. Defaults to "eject" for rows written before this
	// field existed (see the sqlite migration in Task 6).
	Action    string
	TargetPower  int64
	CurrentPower int64
	Reason    string
```

(`TargetPower`/`CurrentPower` are added here too since they're needed by Task 4's `Target` round-trip in Task 8's daemon wiring, and belong next to `Action` for readability — Task 6 wires the SQLite persistence for all three.)

- [ ] **Step 3b: Implement the Gate change**

Replace `internal/lifecycle/lifecycle.go`'s `Gate` function and add a helper:

```go
package lifecycle

import (
	"fmt"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/store"
)

// Params are the resolved lifecycle timers for a chain.
type Params struct {
	Cooldown        time.Duration
	RejectedBackoff time.Duration
	MaxRejections   int // 0 = unlimited
	Now             time.Time
}

// Decision is the gate outcome.
type Decision struct {
	Allow  bool
	Reason string // human-readable suppression reason (empty when Allow)
}

func allow() Decision { return Decision{Allow: true} }
func deny(f string, a ...any) Decision {
	return Decision{Allow: false, Reason: fmt.Sprintf(f, a...)}
}

// Gate decides whether a new proposal may be created for a validator.
//
//	last       — the validator's most recent proposal on this chain, or nil.
//	rejections — how many of its past proposals were rejected by GovDAO.
func Gate(last *store.ProposalRecord, rejections int, p Params) Decision {
	if last == nil {
		return allow()
	}

	switch last.Status {
	case store.StatusAccepted:
		if last.Action != config.ActionLowerVP && last.Action != config.ActionRaiseVP {
			// eject (or a legacy/unset row, which predates this field and
			// was always an eject) — the validator has been ejected.
			return deny("already ejected (proposal accepted)")
		}
		// lower_vp/raise_vp accepted is not terminal: the VP can be
		// adjusted again later once the score moves and cooldown elapses.
		if until := cooldownFloor(last, p); p.Now.Before(until) {
			return deny("cooldown until %s", until.UTC().Format(time.RFC3339))
		}
		return allow()

	case store.StatusPending:
		return deny("a proposal is pending GovDAO vote")

	case store.StatusRejected:
		if p.MaxRejections > 0 && rejections >= p.MaxRejections {
			return deny("max rejections reached (%d)", rejections)
		}
		base := last.ResolvedAt
		if base.IsZero() {
			base = last.CreatedAt
		}
		if until := base.Add(p.RejectedBackoff); p.Now.Before(until) {
			return deny("rejection backoff until %s", until.UTC().Format(time.RFC3339))
		}
		return allow()

	default:
		// Failed broadcast or a proposal not yet promoted to an on-chain status:
		// apply the cooldown floor from the last proposal's creation time so a
		// failing broadcast is not retried every poll interval.
		if until := cooldownFloor(last, p); p.Now.Before(until) {
			return deny("cooldown until %s", until.UTC().Format(time.RFC3339))
		}
		return allow()
	}
}

// cooldownFloor is the earliest time a new proposal may be created given
// last's creation time and the configured cooldown.
func cooldownFloor(last *store.ProposalRecord, p Params) time.Time {
	return last.CreatedAt.Add(p.Cooldown)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lifecycle/... ./internal/store/... -v`
Expected: PASS (the `store` package still compiles — `ProposalRecord` gained fields, nothing removed).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go internal/store/store.go
git commit -m "feat(lifecycle): accepted lower_vp/raise_vp is not terminal like eject"
```

---

### Task 6: Store — proposals + evaluations schema migration

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/sqlite.go`
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `store.ProposalRecord.Action/TargetPower/CurrentPower` (added Task 5).
- Produces: `store.EvalRecord.Score int`, `.Tier string` (replacing `MissedBlocks`/`CriticalAlerts`); `SQLite` persists and round-trips `Action`/`TargetPower`/`CurrentPower` on `proposals`, and `Score`/`Tier` on `evaluations`, via an additive, idempotent migration.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestProposalActionRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1a",
		Moniker: "a", Status: StatusPending, OnchainID: "1",
		Action: "lower_vp", CurrentPower: 1000, TargetPower: 600,
		Args: []string{"g1a"},
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	last, err := st.LastProposal(ctx, "local", "g1a")
	if err != nil || last == nil {
		t.Fatalf("LastProposal: %+v, err %v", last, err)
	}
	if last.ID != id || last.Action != "lower_vp" || last.CurrentPower != 1000 || last.TargetPower != 600 {
		t.Fatalf("round-tripped record = %+v, want action=lower_vp current=1000 target=600", last)
	}
}

func TestEvaluationScoreTierRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.RecordEvaluation(ctx, EvalRecord{
		CreatedAt: time.Now(), Chain: "local", Address: "g1a", Moniker: "a",
		Score: 24, Tier: "Critical", Eligible: true, Reason: "score 24/100",
	}); err != nil {
		t.Fatalf("RecordEvaluation: %v", err)
	}
	// No public read API for evaluations beyond the write path itself; this
	// asserts RecordEvaluation with the new fields doesn't error against the
	// migrated schema (the NOT NULL missed_blocks/critical_alerts columns
	// are the risk: an unmigrated or mis-migrated schema would fail here).
}

// TestMigrationAddsColumnsToExistingDB simulates a pre-existing database that
// predates the action/target_power/current_power columns, and asserts
// opening it through store.New (which runs migrate()) additively adds them
// without touching existing data.
func TestMigrationAddsColumnsToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	const oldSchema = `
CREATE TABLE proposals (
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
);`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatalf("seeding old schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO proposals (created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success)
		 VALUES ('2026-01-01T00:00:00Z', 'local', 'dev', 'g1old', 'old', 'r', 't', '', '', '[]', '', 1)`); err != nil {
		t.Fatalf("seeding old row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}

	st, err := New(config.Storage{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("New (migration): %v", err)
	}
	defer st.Close()

	last, err := st.LastProposal(context.Background(), "local", "g1old")
	if err != nil || last == nil {
		t.Fatalf("LastProposal after migration: %+v, err %v", last, err)
	}
	if last.Action != "eject" {
		t.Fatalf("pre-migration row Action = %q, want default 'eject'", last.Action)
	}
	if last.TargetPower != 0 || last.CurrentPower != 0 {
		t.Fatalf("pre-migration row TargetPower/CurrentPower = %d/%d, want 0/0", last.TargetPower, last.CurrentPower)
	}

	// Migration must be idempotent: opening it again must not error.
	st2, err := New(config.Storage{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("New (second open, idempotency): %v", err)
	}
	st2.Close()
}
```

Add `"database/sql"` to the test file's imports (needed for the raw pre-migration DB setup).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/... -v`
Expected: compile errors (`EvalRecord.Score`/`.Tier` undefined; `ProposalRecord.Action`/`.CurrentPower`/`.TargetPower` not yet persisted — the `RecordProposal`/`RecordEvaluation` calls will compile since Task 5 already added the struct fields, but `TestMigrationAddsColumnsToExistingDB` and the round-trip assertions will fail at runtime since `sqlite.go` doesn't read/write them yet).

- [ ] **Step 3: Implement**

In `internal/store/store.go`, replace `EvalRecord`:

```go
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
```

In `internal/store/sqlite.go`:

1. Replace `migrate()`:

```go
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

	// Additive column migrations: new deployments and upgrades of an
	// existing database both go through this path (a fresh CREATE TABLE
	// above doesn't include these columns either, so there is a single
	// source of truth for their DDL).
	cols := []struct{ table, name, ddl string }{
		{"proposals", "action", `ALTER TABLE proposals ADD COLUMN action TEXT NOT NULL DEFAULT 'eject'`},
		{"proposals", "target_power", `ALTER TABLE proposals ADD COLUMN target_power INTEGER NOT NULL DEFAULT 0`},
		{"proposals", "current_power", `ALTER TABLE proposals ADD COLUMN current_power INTEGER NOT NULL DEFAULT 0`},
		{"evaluations", "score", `ALTER TABLE evaluations ADD COLUMN score INTEGER NOT NULL DEFAULT 0`},
		{"evaluations", "tier", `ALTER TABLE evaluations ADD COLUMN tier TEXT NOT NULL DEFAULT ''`},
	}
	for _, c := range cols {
		if err := s.addColumnIfMissing(c.table, c.name, c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// addColumnIfMissing runs ddl only if table doesn't already have column,
// making repeated calls to migrate() (every daemon startup) idempotent.
func (s *SQLite) addColumnIfMissing(table, column, ddl string) error {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("inspecting %s schema: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := s.db.Exec(ddl); err != nil {
		return fmt.Errorf("adding column %s.%s: %w", table, column, err)
	}
	return nil
}
```

2. Replace `RecordEvaluation`:

```go
func (s *SQLite) RecordEvaluation(ctx context.Context, r EvalRecord) error {
	// missed_blocks/critical_alerts are historical columns from the
	// pre-score model; always 0 going forward, superseded by score/tier.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO evaluations
		 (created_at, chain, address, moniker, missed_blocks, critical_alerts, eligible, excepted, reason, score, tier)
		 VALUES (?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?)`,
		fmtTime(r.CreatedAt), r.Chain, r.Address, r.Moniker,
		boolInt(r.Eligible), boolInt(r.Excepted), r.Reason, r.Score, r.Tier,
	)
	return err
}
```

3. Replace `RecordProposal`:

```go
func (s *SQLite) RecordProposal(ctx context.Context, r ProposalRecord) (int64, error) {
	args, err := json.Marshal(r.Args)
	if err != nil {
		return 0, fmt.Errorf("marshaling proposal args: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO proposals
		 (created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success, onchain_proposal_id, status, resolved_at, action, target_power, current_power)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmtTime(r.CreatedAt), r.Chain, r.ChainID, r.Address, r.Moniker,
		r.Reason, r.Title, r.RealmPath, r.Function, string(args), r.Output,
		boolInt(r.Success), r.OnchainID, r.Status, fmtTime(r.ResolvedAt),
		r.Action, r.TargetPower, r.CurrentPower,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
```

4. Replace `proposalCols` and `scanProposal`:

```go
const proposalCols = `id, created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success, onchain_proposal_id, status, resolved_at, action, target_power, current_power`

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
		&r.Output, &success, &r.OnchainID, &r.Status, &resolvedAt,
		&r.Action, &r.TargetPower, &r.CurrentPower); err != nil {
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
```

Everything else in `sqlite.go` (`LastProposal`, `PendingProposals`, `RejectionCount`, `ListProposals`, `collectProposals`) is unchanged — they all go through `proposalCols`/`scanProposal`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/store/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/sqlite.go internal/store/store_test.go
git commit -m "feat(store): additive migration for action/target_power/current_power, score/tier"
```

---

### Task 7: Notify — per-action messages

**Files:**
- Modify: `internal/notify/notify.go`
- Modify: `internal/notify/notify_test.go`

**Interfaces:**
- Consumes: `config.ActionEject/LowerVP/RaiseVP` (Task 1).
- Produces: `notify.Info` gains `Action string`, `CurrentPower, TargetPower int64`, `VPDeltaPercent float64`; `Message()` generalizes sent/accepted/rejected wording per action, eject wording unchanged.

- [ ] **Step 1: Write the failing test**

Add to `internal/notify/notify_test.go` (check the existing file's `Info{...}` literal style first and match it — the new cases below assume a `TestMessage` table-driven test already exists; if it does, append rows; otherwise add this standalone test):

```go
func TestMessage_LowerVPSent(t *testing.T) {
	msg := Message(EventSent, Info{
		Chain: "test6", Moniker: "bad-node", Address: "g1bad",
		Action: config.ActionLowerVP, CurrentPower: 1000, TargetPower: 600, VPDeltaPercent: -2.0,
	})
	want := "A proposal for validator bad-node (g1bad) on test6 has been sent to lower its voting power from 1000 to 600 (-2.0% of total valset VP)."
	if msg != want {
		t.Errorf("Message = %q, want %q", msg, want)
	}
}

func TestMessage_RaiseVPAccepted(t *testing.T) {
	msg := Message(EventAccepted, Info{
		Chain: "test6", Moniker: "good-node", Address: "g1good",
		Action: config.ActionRaiseVP, CurrentPower: 1000, TargetPower: 1400,
	})
	want := "GovDAO accepted the proposal for validator good-node (g1good) on test6: its voting power has been raised from 1000 to 1400."
	if msg != want {
		t.Errorf("Message = %q, want %q", msg, want)
	}
}

func TestMessage_LowerVPRejected(t *testing.T) {
	msg := Message(EventRejected, Info{
		Chain: "test6", Moniker: "bad-node", Address: "g1bad",
		Action: config.ActionLowerVP, CurrentPower: 1000, RejectedBackoff: 720 * time.Hour,
	})
	want := "GovDAO rejected the proposal for validator bad-node (g1bad) on test6 to lower its voting power: it stays at 1000. A new proposal may be sent after 720h0m0s."
	if msg != want {
		t.Errorf("Message = %q, want %q", msg, want)
	}
}

func TestMessage_EjectUnchanged(t *testing.T) {
	msg := Message(EventSent, Info{Chain: "test6", Moniker: "bad-node", Address: "g1bad", Action: config.ActionEject})
	want := "A proposal for validator bad-node (g1bad) on test6 has been sent to remove it from the valset."
	if msg != want {
		t.Errorf("Message = %q, want %q (eject wording must not change)", msg, want)
	}
}
```

Add `"github.com/samouraiworld/katana/internal/config"` and `"time"` to the test file's imports if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/notify/... -v`
Expected: compile error (`Info` has no field `Action`/`CurrentPower`/`TargetPower`/`VPDeltaPercent`), then assertion failures once it compiles against the old `Message()`.

- [ ] **Step 3: Implement**

In `internal/notify/notify.go`:

1. Add the `config` import (already imported for `config.Notifications`).

2. Extend `Info`:

```go
// Info carries the fields a message may reference.
type Info struct {
	Chain           string
	Moniker         string
	Address         string
	GnowebURL       string
	ProposalID      string
	RejectedBackoff time.Duration
	// Action is the score-driven action this notification is about: eject |
	// lower_vp | raise_vp. CurrentPower/TargetPower are 0 for eject.
	// VPDeltaPercent is the signed % of the chain's total valset VP this
	// move represents (only used by the "sent" wording).
	Action         string
	CurrentPower   int64
	TargetPower    int64
	VPDeltaPercent float64
	// Observe (would_eject) fields: the proposal script/expression, the filename
	// it is attached under, and the ready-to-run gnokey command to broadcast it.
	ProposalCode     string
	ProposalFilename string
	GnokeyCommand    string
}
```

3. Replace `Message()`:

```go
// Message renders the English notification text for an event.
func Message(event Event, in Info) string {
	var msg string
	switch event {
	case EventSent:
		msg = sentText(in)
	case EventAccepted:
		msg = acceptedText(in)
	case EventRejected:
		msg = rejectedText(in)
	case EventWouldEject:
		msg = fmt.Sprintf("Validator %s (%s) on %s qualifies for %s, but dry_run is enabled — nothing was broadcast.",
			in.display(), in.Address, in.Chain, actionNoun(in.Action))
		if in.GnokeyCommand != "" {
			msg += "\nTo broadcast it manually, run:\n```\n" + in.GnokeyCommand + "\n```\nThe proposal script is included with this notification (attached, or inline below if your channel doesn't support attachments)."
		}
		return msg
	default:
		msg = fmt.Sprintf("katana event %q for validator %s (%s) on %s.", event, in.display(), in.Address, in.Chain)
	}
	if link := in.proposalLink(); link != "" {
		msg += " " + link
	}
	return msg
}

func isVPAction(action string) bool {
	return action == config.ActionLowerVP || action == config.ActionRaiseVP
}

func vpVerb(action string, past bool) string {
	if action == config.ActionRaiseVP {
		if past {
			return "raised"
		}
		return "raise"
	}
	if past {
		return "lowered"
	}
	return "lower"
}

func sentText(in Info) string {
	if isVPAction(in.Action) {
		return fmt.Sprintf("A proposal for validator %s (%s) on %s has been sent to %s its voting power from %d to %d (%+.1f%% of total valset VP).",
			in.display(), in.Address, in.Chain, vpVerb(in.Action, false), in.CurrentPower, in.TargetPower, in.VPDeltaPercent)
	}
	return fmt.Sprintf("A proposal for validator %s (%s) on %s has been sent to remove it from the valset.",
		in.display(), in.Address, in.Chain)
}

func acceptedText(in Info) string {
	if isVPAction(in.Action) {
		return fmt.Sprintf("GovDAO accepted the proposal for validator %s (%s) on %s: its voting power has been %s from %d to %d.",
			in.display(), in.Address, in.Chain, vpVerb(in.Action, true), in.CurrentPower, in.TargetPower)
	}
	return fmt.Sprintf("GovDAO accepted the proposal for validator %s (%s) on %s: it has been removed from the valset.",
		in.display(), in.Address, in.Chain)
}

func rejectedText(in Info) string {
	if isVPAction(in.Action) {
		return fmt.Sprintf("GovDAO rejected the proposal for validator %s (%s) on %s to %s its voting power: it stays at %d. A new proposal may be sent after %s.",
			in.display(), in.Address, in.Chain, vpVerb(in.Action, false), in.CurrentPower, in.RejectedBackoff)
	}
	return fmt.Sprintf("GovDAO rejected the proposal for validator %s (%s) on %s: it stays in the valset. A new proposal may be sent after %s.",
		in.display(), in.Address, in.Chain, in.RejectedBackoff)
}

func actionNoun(action string) string {
	switch action {
	case config.ActionLowerVP:
		return "a voting-power reduction"
	case config.ActionRaiseVP:
		return "a voting-power increase"
	default:
		return "ejection"
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/notify/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/notify/notify.go internal/notify/notify_test.go
git commit -m "feat(notify): per-action sent/accepted/rejected wording"
```

---

### Task 8: Daemon — wire the score-driven pipeline

**Files:**
- Modify: `internal/daemon/cycle.go`
- Modify: `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-7 (`gnomonitoring.FetchReports`, `rules.Evaluate`/`TotalVotingPower`, `proposal.Target`/`Build`, `lifecycle.Gate`, `store.EvalRecord`/`ProposalRecord`, `notify.Info`).
- Produces: `evalCycle`/`produce`/`dryRun`/`voteCycle`/`dispatch` updated; no new exported symbols (package-internal wiring).

- [ ] **Step 1: Write the failing test**

Rewrite `internal/daemon/daemon_test.go`'s fixture (`newFixture`) to serve `/api/reports/validators` and use the new config shape. Replace lines 159-201 (the `gnomon := httptest.NewServer(...)` block through the end of the `cfg := &config.Config{...}` literal):

```go
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/reports/validators" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reportsJSON))
	}))
	t.Cleanup(gnomon.Close)

	notifs := &notifRecorder{}
	hook := httptest.NewServer(notifs.handler())
	t.Cleanup(hook.Close)

	cfg := &config.Config{
		PollInterval: "5m",
		Gnomonitoring: config.Gnomonitoring{
			BaseURL: gnomon.URL, TimeoutSeconds: 5, ReportsPath: "/api/reports/validators",
		},
		Gnokey: config.Gnokey{Bin: "gnokey", KeyName: "test-key", GasFee: "1ugnot", GasWanted: 1},
		Proposal: config.Proposal{
			Cooldown: "720h", RejectedBackoff: "720h", Submit: "run",
			ActionPriority:      []string{config.ActionEject, config.ActionLowerVP, config.ActionRaiseVP},
			MinVotingPower:      100,
			MaxVotingPowerShare: 15,
			Actions: map[string]config.ActionConfig{
				config.ActionEject: {
					ScoreBelow: 30, Window: "current_month",
					RunScriptTemplate:   "{operator_address} eject {title} {description}",
					TitleTemplate:       "Eject {moniker} ({address})",
					DescriptionTemplate: "Reason: {reason}",
				},
				config.ActionLowerVP: {
					ScoreBelow: 60, Window: "current_week",
					VPStepPercentOfSelf: -20, VPStepPercentOfTotal: -50, // wide network cap: self-cap decides in this fixture
					RunScriptTemplate:   "{operator_address} lower {target_power} {title} {description}",
					TitleTemplate:       "Lower {moniker} ({address})",
					DescriptionTemplate: "Reason: {reason}",
				},
				config.ActionRaiseVP: {
					ScoreAtOrAbove: 85, Window: "current_year",
					VPStepPercentOfSelf: 20, VPStepPercentOfTotal: 50,
					RunScriptTemplate:   "{operator_address} raise {target_power} {title} {description}",
					TitleTemplate:       "Raise {moniker} ({address})",
					DescriptionTemplate: "Reason: {reason}",
				},
			},
		},
		VoteTracking: config.VoteTracking{VotePollInterval: "5m"},
		Notifications: config.Notifications{
			Enabled: true,
			Discord: config.WebhookChannel{Enabled: true, WebhookURL: hook.URL},
		},
		Storage: config.Storage{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "katana.db")},
		Chains: map[string]*config.ChainConfig{
			"test": {
				RPCEndpoint: rpc.URL, ChainID: "dev",
				GnomonitoringChain: "local", Enabled: true,
			},
		},
		ValopersRealm: "gno.land/r/gnops/valopers",
	}
```

Add a package-level fixture constant right above `newFixture` (this is what makes `g1bad` eligible for `eject`, matching the old fixture's "misses more blocks than the threshold" intent, now expressed as a low score):

```go
// reportsJSON is the default gnomonitoring report fixture: g1bad has a
// critical score on every window (eject-eligible); tests that need a
// lower_vp/raise_vp candidate use mutate to swap this out (see
// TestEvalCycleLowersVotingPower / TestEvalCycleRaisesVotingPower).
const reportsJSON = `[{"addr":"g1bad","moniker":"bad","periods":{
	"current_month":{"score":10,"tier":"Critical","voting_power":1000},
	"current_week":{"score":10,"tier":"Critical","voting_power":1000},
	"current_year":{"score":10,"tier":"Critical","voting_power":1000}
}}]`
```

The fixture's `stubChain.valset`/`operators` (`g1bad`/`g1op`) stay as-is — unaffected by the metrics-source change.

Update the two existing tests that reference dropped config fields / old log text:

```go
func TestDryRunLogsAndNotifiesWithoutBroadcast(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })

	f.d.evalCycle(ctx, f.runtime(t))

	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0 in dry-run mode", got)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "[DRY-RUN]") {
		t.Fatalf("logs missing [DRY-RUN] entry:\n%s", logs)
	}
	for _, want := range []string{"g1bad", "g1op", "bad", "Eject bad (g1bad)", "score 10/100"} {
		if !strings.Contains(logs, want) {
			t.Errorf("dry-run log missing %q:\n%s", want, logs)
		}
	}
	bodies := f.notifs.all()
	if len(bodies) != 1 {
		t.Fatalf("notifications = %d, want 1", len(bodies))
	}
	body := bodies[0]
```

(keep whatever assertions follow `body := bodies[0]` in the existing file — they check the notification payload shape, unrelated to conditions/metrics, and don't need to change.)

Now add three new end-to-end tests after `TestDryRunLogsAndNotifiesWithoutBroadcast`:

```go
func TestEvalCycleLowersVotingPower(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) {
		c.Chains["test"].Enabled = true
	})
	// Override the fixture's default report: score in the lower_vp band
	// (60 > score >= 30... actually strictly below 60 and not below 30).
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"addr":"g1bad","moniker":"bad","periods":{
			"current_month":{"score":50,"tier":"Watch","voting_power":1000},
			"current_week":{"score":50,"tier":"Watch","voting_power":1000},
			"current_year":{"score":50,"tier":"Watch","voting_power":1000}
		}}]`))
	}))
	t.Cleanup(gnomon.Close)
	f.cfg.Gnomonitoring.BaseURL = gnomon.URL

	f.d.evalCycle(ctx, f.runtime(t))

	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1\nlogs:\n%s", got, f.logs.String())
	}
	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 {
		t.Fatalf("proposals = %+v, err %v", props, err)
	}
	p := props[0]
	if p.Action != config.ActionLowerVP {
		t.Fatalf("Action = %q, want lower_vp", p.Action)
	}
	if p.CurrentPower != 1000 || p.TargetPower != 800 {
		t.Fatalf("CurrentPower/TargetPower = %d/%d, want 1000/800 (20%% self-cap)", p.CurrentPower, p.TargetPower)
	}
	built := f.broadcasts.built[0]
	if !strings.Contains(built.Script, "800") {
		t.Errorf("built script missing target power 800: %s", built.Script)
	}
}

func TestEvalCycleRaisesVotingPower(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", nil)
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"addr":"g1bad","moniker":"bad","periods":{
			"current_month":{"score":95,"tier":"Excellent","voting_power":1000},
			"current_week":{"score":95,"tier":"Excellent","voting_power":1000},
			"current_year":{"score":95,"tier":"Excellent","voting_power":1000}
		}}]`))
	}))
	t.Cleanup(gnomon.Close)
	f.cfg.Gnomonitoring.BaseURL = gnomon.URL

	f.d.evalCycle(ctx, f.runtime(t))

	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 {
		t.Fatalf("proposals = %+v, err %v", props, err)
	}
	p := props[0]
	if p.Action != config.ActionRaiseVP || p.TargetPower != 1200 {
		t.Fatalf("Action/TargetPower = %s/%d, want raise_vp/1200", p.Action, p.TargetPower)
	}
}

func TestExceptionsBlockAllThreeActions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		score int
	}{
		{"eject-band", 10},
		{"lower_vp-band", 50},
		{"raise_vp-band", 95},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "dev", func(c *config.Config) {
				c.Exceptions = []string{"g1bad"}
			})
			gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `[{"addr":"g1bad","moniker":"bad","periods":{
					"current_month":{"score":%d,"voting_power":1000},
					"current_week":{"score":%d,"voting_power":1000},
					"current_year":{"score":%d,"voting_power":1000}
				}}]`, tc.score, tc.score, tc.score)
			}))
			t.Cleanup(gnomon.Close)
			f.cfg.Gnomonitoring.BaseURL = gnomon.URL

			f.d.evalCycle(ctx, f.runtime(t))

			if got := f.broadcasts.count(); got != 0 {
				t.Fatalf("broadcasts = %d, want 0 (exception must block every action)", got)
			}
		})
	}
}
```

Add one more test confirming the fixed-step VP model doesn't compound across
consecutive cycles (the spec's core "no compounding" requirement, exercised
end-to-end rather than just at the `rules` unit level):

```go
func TestConsecutiveLowerVPCyclesDoNotCompound(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) {
		// No cooldown floor between cycles, so this test can drive three
		// evaluations back to back and observe three broadcasts.
		c.Proposal.Cooldown = "0s"
	})

	power := int64(1000)
	report := func() string {
		return fmt.Sprintf(`[{"addr":"g1bad","moniker":"bad","periods":{
			"current_month":{"score":50,"voting_power":%d},
			"current_week":{"score":50,"voting_power":%d},
			"current_year":{"score":50,"voting_power":%d}
		}}]`, power, power, power)
	}
	gnomon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(report()))
	}))
	t.Cleanup(gnomon.Close)
	f.cfg.Gnomonitoring.BaseURL = gnomon.URL

	wantTargets := []int64{800, 600, 400} // fixed -20%-of-self step each cycle, off the live current power
	for i, want := range wantTargets {
		f.d.evalCycle(ctx, f.runtime(t))
		props, err := f.st.ListProposals(ctx, 1)
		if err != nil || len(props) != 1 {
			t.Fatalf("cycle %d: proposals = %+v, err %v", i, props, err)
		}
		if props[0].TargetPower != want {
			t.Fatalf("cycle %d: TargetPower = %d, want %d (current power was %d)", i, props[0].TargetPower, want, power)
		}
		power = props[0].TargetPower // simulate the proposal having been accepted on-chain
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/... -v`
Expected: compile errors (`config.Conditions` etc. no longer exist in the fixture) until Step 3 lands.

- [ ] **Step 3: Implement**

Replace `internal/daemon/cycle.go` entirely:

```go
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

	produced := 0
	byAction := map[string]int{}
	for _, report := range reports {
		dec := rules.Evaluate(report, rt.Proposal.ActionPriority, rt.Proposal.Actions,
			rt.Proposal.MinVotingPower, rt.Proposal.MaxVotingPowerShare, totalVP)

		excepted := d.cfg.IsException(rt.Key, report.Addr, report.Moniker)
		_ = d.st.RecordEvaluation(ctx, store.EvalRecord{
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
		if inSet, err := gno.IsValidator(ctx, report.Addr); err != nil {
			d.logger.Printf("[%s][eval] valset check failed for %s: %v (skipping)", rt.Key, report.Addr, err)
			continue
		} else if !inSet {
			d.logger.Printf("[%s][eval] %s not in valset, skipping", rt.Key, report.Addr)
			continue
		}

		d.logger.Printf("[%s][eval] ELIGIBLE %s action=%s: %s", rt.Key, disp(report.Moniker, report.Addr), dec.Action, dec.Reason)
		d.produce(ctx, rt, gno, dec, totalVP[dec.Window])
		produced++
		byAction[dec.Action]++
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

	out, err := d.broadcast(ctx, built, rt.Gnokey)
	rec.Output = out
	rec.Success = err == nil
	if err == nil {
		rec.Status = store.StatusPending
		rec.OnchainID = govdao.ExtractProposalID(out)
	}
	id, recErr := d.st.RecordProposal(ctx, rec)
	if err != nil {
		d.logger.Printf("[%s][eval] broadcast failed for %s: %v", rt.Key, dec.Address, err)
		return
	}
	if recErr != nil {
		// The proposal is already on-chain but we failed to persist it: the
		// lifecycle gate has no record and will re-broadcast an identical
		// proposal next cycle. Surface this loudly rather than silently spam.
		d.logger.Printf("[%s][eval] CRITICAL: broadcast ok (id=%q) but recording it failed for %s: %v — re-broadcast likely next cycle", rt.Key, rec.OnchainID, dec.Address, recErr)
	}
	d.logger.Printf("[%s][eval] BROADCAST ok for %s action=%s (proposal id=%q)", rt.Key, dec.Address, dec.Action, rec.OnchainID)
	d.dispatch(ctx, notify.EventSent, rt, id, nt, rec.OnchainID, vpDeltaPercent, observePayload{})
}

// notifyTarget carries the validator/action identity dispatch needs to build
// a notify.Info, independent of whether the caller is the eval path
// (produce/dryRun, from a rules.Decision) or the vote-watcher (voteCycle,
// from a store.ProposalRecord).
type notifyTarget struct {
	Address, Moniker, Action string
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
		nt := notifyTarget{Address: p.Address, Moniker: p.Moniker, Action: p.Action, CurrentPower: p.CurrentPower, TargetPower: p.TargetPower}
		switch status {
		case govdao.Accepted:
			_ = d.st.UpdateStatus(ctx, p.ID, store.StatusAccepted, d.now())
			d.logger.Printf("[%s][vote] proposal %s ACCEPTED — %s action=%s", rt.Key, p.OnchainID, disp(p.Moniker, p.Address), p.Action)
			d.dispatch(ctx, notify.EventAccepted, rt, p.ID, nt, p.OnchainID, 0, observePayload{})
		case govdao.Rejected:
			_ = d.st.UpdateStatus(ctx, p.ID, store.StatusRejected, d.now())
			d.logger.Printf("[%s][vote] proposal %s REJECTED — %s action=%s", rt.Key, p.OnchainID, disp(p.Moniker, p.Address), p.Action)
			d.dispatch(ctx, notify.EventRejected, rt, p.ID, nt, p.OnchainID, 0, observePayload{})
		default:
			// pending / unknown: leave as pending, retry next tick.
		}
	}
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
```

Note: `store.ProposalRecord` needs `Action`/`CurrentPower`/`TargetPower` (added in Tasks 5-6) to be readable in `voteCycle`'s `pending` loop — already satisfied.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/... -v`
Expected: PASS. Then run the full suite to catch any cross-package regression:

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/cycle.go internal/daemon/daemon_test.go
git commit -m "feat(daemon): wire score-driven eject/lower_vp/raise_vp through the eval cycle"
```

---

### Task 9: Docs — config.yaml.example, README, CLAUDE.md, prd.md

**Files:**
- Modify: `config.yaml.example`
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `prd.md`

**Interfaces:** none (documentation only) — this task has no code to test; "done" means the docs describe the schema Task 1 actually implements, verified by a final read-through against the merged code rather than a test run.

- [ ] **Step 1: Rewrite `config.yaml.example`**

Replace the `gnomonitoring:` block's `missed_blocks_path`/`alerts_path` lines
and the entire `conditions:` block through the end of `proposal:` with:

```yaml
gnomonitoring:
  base_url: "http://localhost:8989"   # env: KATANA_GNOMON_BASE_URL
  api_token: ""                       # env: KATANA_GNOMON_API_TOKEN (unused by reports_path, kept for future admin endpoints)
  timeout_seconds: 10
  reports_path: "/api/reports/validators"   # per-validator computed health score
  info_path: "/info"                  # chain-discovery endpoint (validation)
  discover_chains: true               # validate config chains against /info at startup

# ── Score-driven actions ──────────────────────────────────────────────────────
# katana reads gnomonitoring's computed health score (0-100) instead of raw
# missed-blocks/alert counts. At most one action applies per validator per
# cycle: action_priority decides which, when more than one action's
# threshold matches (on its own configured window).
proposal:
  action_priority: [eject, lower_vp, raise_vp]

  min_voting_power: 100             # absolute floor; protects a small validator
                                     # from being driven to ~0 VP by repeated
                                     # lower_vp instead of a real eject
  max_voting_power_share: 15        # ceiling as % of the chain's total valset VP

  cooldown: 720h                      # min interval between two proposals for the same validator (30d)
  rejected_backoff: 720h              # wait after a GovDAO rejection before re-proposing (30d)
  max_rejections: 2                   # stop after N GovDAO rejections; 0 = unlimited
  submit: run                         # run (default; required for lower_vp/raise_vp) | call (eject only)

  actions:
    # Thresholds default to gnomonitoring's own tier boundaries (Excellent
    # >=85 / Good >=60 / Watch >=30 / Critical <30); 60-84 is an intentional
    # no-action "stability zone".
    eject:
      score_below: 30
      window: current_month           # CONDITION WINDOW: last_24h|current_week|current_month|current_year
      title_template: "Eject validator {moniker} ({address}) from the valset"
      description_template: |
        Automated ejection proposal generated by katana.
        Validator: {moniker} ({address})
        Score: {score}/100 (tier {tier}) over {window}
        Reason: {reason}
    lower_vp:
      score_below: 60
      window: current_week
      # Step = min(vp_step_percent_of_self% of the validator's own current
      # VP, vp_step_percent_of_total% of the chain's total valset VP) —
      # whichever cap is tighter for this validator's size wins. Recomputed
      # from live data every cycle, so repeated triggers don't compound.
      vp_step_percent_of_self: -20
      vp_step_percent_of_total: -2
      title_template: "Lower voting power of validator {moniker} ({address})"
      description_template: |
        Automated voting-power reduction generated by katana.
        Validator: {moniker} ({address})
        Score: {score}/100 (tier {tier}) over {window}
        Voting power: {current_power} -> {target_power}
        Reason: {reason}
    raise_vp:
      score_at_or_above: 85
      window: current_year              # longest window: raise_vp is the most
                                         # governance-sensitive action, requires
                                         # a sustained track record
      vp_step_percent_of_self: 20
      vp_step_percent_of_total: 2
      title_template: "Raise voting power of validator {moniker} ({address})"
      description_template: |
        Automated voting-power increase generated by katana.
        Validator: {moniker} ({address})
        Score: {score}/100 (tier {tier}) over {window}
        Voting power: {current_power} -> {target_power}
        Reason: {reason}
```

No other file changes: the per-chain example blocks (`test6`, `betanet`,
`mainnet`) don't reference `conditions:` or the removed proposal template
fields today, so they need no edits — `action_priority`/VP bounds are
global-only (per this plan's Global Constraints deviation note), and a chain
overriding `proposal:` would restate the whole block per existing
whole-block-replace convention (no new example needed unless a real chain
requires one).

- [ ] **Step 2: Update `README.md`**

Replace the "## Ejection conditions" section (currently ~lines 53-68) with:

```markdown
## Score-driven actions

katana reads a computed per-validator health score from gnomonitoring
(`GET /api/reports/validators`) instead of raw missed-blocks/alert counts.
Three actions are possible, evaluated in `proposal.action_priority` order
(default `eject, lower_vp, raise_vp`) — the first whose threshold matches on
its own configured window wins, so at most one action applies per validator
per cycle:

| Action      | Threshold                          | Effect                                                          |
|-------------|-------------------------------------|-------------------------------------------------------------------|
| `eject`     | `score < score_below`               | Power=0 removal (unchanged from the previous ejection mechanism)  |
| `lower_vp`  | `score < score_below`               | reduce voting power by a fixed step                               |
| `raise_vp`  | `score >= score_at_or_above`        | increase voting power by a fixed step                             |

The default thresholds (30 / 60 / 85) mirror gnomonitoring's own tier
boundaries (Critical / Watch / Good / Excellent); scores 60-84 ("Good") are an
intentional no-action stability zone.

**VP step.** `lower_vp`/`raise_vp` move a validator's voting power by the
smaller-magnitude of two caps: `vp_step_percent_of_self` (a % of the
validator's own current VP — protects small validators from an
oversized single move) and `vp_step_percent_of_total` (a % of the chain's
total valset VP — keeps a large validator's step bounded). Both recomputed
fresh from live data every cycle, so repeated triggers don't compound. The
result is clamped to `[min_voting_power, max_voting_power_share% of total
valset VP]`; a step that would be a no-op at a bound produces no proposal.

Every threshold/window/step is independently configurable per action, and
`proposal.actions.*` can be overridden per chain like the rest of
`proposal:` (whole-block replace).
```

- [ ] **Step 3: Update `CLAUDE.md`**

In the "Architecture" section, replace:

```
- **internal/rules** — pure eligibility. Conditions independently toggleable;
  `require_all` = AND/OR over *enabled* ones; zero enabled → never eligible;
  thresholds are strict `>`.
```

with:

```
- **internal/rules** — pure score-driven decision. `Evaluate` walks
  `proposal.action_priority` (default eject, lower_vp, raise_vp) and returns
  at most one action per validator per cycle, matched against gnomonitoring's
  computed health score on that action's own configured window. `lower_vp`/
  `raise_vp` targets use a dual-cap fixed step (see README "Score-driven
  actions"), clamped to `[min_voting_power, max_voting_power_share]`.
```

In "Key seams", update the "metrics come from gnomonitoring" line to note the single-endpoint change if desired (optional, low-value edit — skip unless time allows). In "Conventions", the "run-mode entrypoint... confirmed end-to-end" bullet is unaffected (eject's script/behavior is unchanged); no edit needed there.

- [ ] **Step 4: Update `prd.md`**

1. Replace §6.4 "Eligibility conditions `[EXISTS]`" (the `FR-4.1`-`FR-4.4` block) with:

```markdown
### 6.4b Score-driven actions `[TO BUILD]`

- **FR-4b.1** katana reads a computed per-validator health score (0-100) from
  gnomonitoring (`GET /api/reports/validators?chain=`) instead of raw
  missed-blocks/CRITICAL-alert counts. Three actions are possible: `eject`
  (Power=0 removal, unchanged from the previous mechanism), `lower_vp`, and
  `raise_vp` (voting-power adjustments). At most one action applies per
  validator per cycle.
- **FR-4b.2** `proposal.action_priority` (default `[eject, lower_vp,
  raise_vp]`) decides which action wins when more than one action's
  threshold would match on its own configured window — reordering is a
  config change, not a code change.
- **FR-4b.3** Each action reads its score on its own configured window
  (`last_24h` | `current_week` | `current_month` | `current_year`) and
  threshold: `eject`/`lower_vp` trigger when `score < score_below`;
  `raise_vp` triggers when `score >= score_at_or_above`. Default thresholds
  (30 / 60 / 85) mirror gnomonitoring's own tier boundaries; scores 60-84 are
  an intentional no-action stability zone.
- **FR-4b.4** `lower_vp`/`raise_vp` compute a target voting power as the
  validator's current power plus a step equal to the smaller-magnitude of
  two caps: `vp_step_percent_of_self` (% of the validator's own current VP)
  and `vp_step_percent_of_total` (% of the chain's total valset VP). Both
  are recomputed from live data every cycle, so consecutive triggers don't
  compound. The result is clamped to `[proposal.min_voting_power,
  proposal.max_voting_power_share% of total valset VP]`; a step that would
  be a no-op at a bound produces no proposal that cycle.
- **FR-4b.5** gnomonitoring's sub-metrics (`sign_rate`, `proposer_reliability`,
  `incident_rate_per_week`, `downtime_blocks`) are never used as eligibility
  conditions — only the score is. They are surfaced as human-readable
  context in the proposal description and notification text.
```

2. Replace §7's example `config.yaml` block's `gnomonitoring:`/`conditions:`/`proposal:` portions with the YAML from this plan's Task 9 Step 1 (identical content — `config.yaml.example` and this PRD section must not drift).

3. Under §14 Decisions ("Resolved — GovDAO vote tracking" or a new subsection after it), add:

```markdown
- **D15 — Score as the sole eligibility signal.** ✅ gnomonitoring's computed
  score (not raw missed-blocks/alert counts, and not its sub-metrics) is the
  only input to `eject`/`lower_vp`/`raise_vp` eligibility — sub-metrics are
  display-only context in proposal/notification text. Rationale: the score
  already blends sign rate, proposer reliability, incident frequency and
  downtime with tunable weights/caps on the gnomonitoring side; re-deriving
  eligibility from the raw sub-metrics in katana would duplicate that tuning
  surface. The VP step for `lower_vp`/`raise_vp` uses two caps (% of the
  validator's own VP, % of the chain's total valset VP) and takes the
  smaller: this avoids both a disproportionate single move for a small
  validator and an unbounded absolute move for a large one, without
  persisting any state across cycles (see
  `docs/superpowers/specs/2026-07-20-score-driven-vp-actions-design.md`).
```

4. Under §15 Milestones, add after M6:

```markdown
7. **M7 — Score-driven actions:** replace `conditions.missed_blocks`/
   `critical_alerts` with gnomonitoring's computed health score;
   `eject`/`lower_vp`/`raise_vp` via a configurable `action_priority`, a
   dual-cap fixed-step VP delta, and `min_voting_power`/
   `max_voting_power_share` bounds.
```

5. Light pass on §6.6 (proposal generation), §6.7 (lifecycle gate), §6.9
   (notifications): find every occurrence of "ejection proposal" that's
   describing the general proposal mechanism (not a sentence specifically
   about the eject action) and change it to "proposal (eject / lower_vp /
   raise_vp)". Do not rewrite the surrounding FR text otherwise — those
   mechanics (broadcast flow, cooldown/backoff state machine, notification
   triggers) are unchanged, only generalized to three action types.

- [ ] **Step 5: Final read-through**

Read the four changed files back and confirm every field name mentioned matches Task 1's actual Go struct tags exactly (`action_priority`, `min_voting_power`, `max_voting_power_share`, `score_below`, `score_at_or_above`, `window`, `vp_step_percent_of_self`, `vp_step_percent_of_total`, `reports_path`) — a docs/code field-name mismatch here is a silent trap for the next operator copying `config.yaml.example`.

- [ ] **Step 6: Commit**

```bash
git add config.yaml.example README.md CLAUDE.md prd.md
git commit -m "docs: describe score-driven eject/lower_vp/raise_vp actions"
```

---

## Final verification (after all 9 tasks)

- [ ] Run `go build ./... && go vet ./... && go test ./... -race` from the repo root — must be fully green.
- [ ] Grep for any remaining reference to the removed `Conditions`/`MissedBlocksCond`/`CriticalCond` types or `missed_blocks_path`/`alerts_path` config keys outside of `docs/` and `prd.md`'s historical narrative: `grep -rn "MissedBlocksCond\|CriticalCond\|missed_blocks_path\|alerts_path" --include=*.go .` should return nothing.
- [ ] Manually diff `config.yaml.example` against a config file that previously worked (if one exists in `deploy/e2e/`) to confirm the migration path is clear for an operator — this plan does not touch `deploy/e2e/*.yaml`; flag to the user that those e2e config fixtures need a manual follow-up pass before the e2e suite (§12.5 in `prd.md`) will run green again, since it's out of this plan's scope (no e2e test task was requested).
