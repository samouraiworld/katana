# Consolidate dry_run and proposal.enabled Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the two overlapping non-broadcast mechanisms (`proposal.enabled` and PR #9's `dry_run`) with a single `dry_run` flag (global default + per-chain override) that never broadcasts, sends the rich manual-approval notification (script attached + gnokey command), and counts toward the real lifecycle cooldown gate.

**Architecture:** No new packages. `internal/config` grows a root `DryRun bool` + changes `ChainConfig.DryRun` to `*bool` (mirroring the existing global-with-per-chain-override pattern) and loses `Proposal.Enabled`/`BroadcastEnabled()`. `internal/store` keeps the `StatusDryRun` tag but stops excluding it from lifecycle queries, and drops `LastDryRun`. `internal/notify` drops `EventDryRun`, keeping `EventWouldEject` as the one non-broadcast event, with a wording fix. `internal/daemon/cycle.go`'s `produce()` collapses to a single `rt.DryRun` branch that absorbs the old observe behavior.

**Tech Stack:** Go 1.23+, `modernc.org/sqlite`, standard `testing` + `httptest`.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-19-consolidate-dry-run-design.md` — follow it exactly; this plan is its task breakdown.
- Go's whole-module compilation means `go build ./...` (and `go vet ./...`) for the **whole repo** will fail between Task 3 and Task 4 — `internal/daemon` won't compile until `cycle.go` is updated in Task 4, because Tasks 1–3 remove symbols (`store.LastDryRun`, `notify.EventDryRun`, `config.Proposal.BroadcastEnabled`) that `internal/daemon/cycle.go` still references. This is expected and correct for a staged refactor of a shared interface — verify each of Tasks 1–3 with a **package-scoped** test command (`go test ./internal/<pkg>/...`), not the whole module. Task 4 is what makes the whole module build again; Task 6 does the final whole-repo verification.
- Every commit message is written by the implementer, not signed/attributed to any AI tool (existing repo convention — check recent `git log` if unsure).
- All identifiers, comments and docs stay in English (per `CLAUDE.md`).

---

## Task 1: Store — keep the dry-run tag, stop excluding it from the lifecycle gate

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/sqlite.go`
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `Store.LastProposal(ctx, chain, address) (*ProposalRecord, error)` now returns the most recent record **regardless of status** (including `StatusDryRun`). `Store.LastDryRun` no longer exists — any caller must be updated to just use `LastProposal`. `store.StatusDryRun` constant is unchanged (`"dry-run"`).

- [ ] **Step 1: Update store_test.go to the new expected behavior**

Replace `TestDryRunRecordsExcludedFromLifecycle` (currently at store_test.go:66-107) with:

```go
func TestDryRunRecordCountsTowardLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// A dry-run record is persisted…
	id, err := st.RecordProposal(ctx, ProposalRecord{
		CreatedAt: time.Now(), Chain: "local", ChainID: "dev", Address: "g1bad",
		Moniker: "bad", Status: StatusDryRun, Success: true,
	})
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	// …and is visible to LastProposal, so it counts toward the real lifecycle
	// gate exactly like a broadcast attempt would (disabling dry_run later
	// does not bypass the cooldown of a proposal already shown to an
	// operator).
	last, err := st.LastProposal(ctx, "local", "g1bad")
	if err != nil || last == nil || last.ID != id || last.Status != StatusDryRun {
		t.Fatalf("LastProposal = %+v, err %v; want the dry-run record", last, err)
	}

	// It is not pending and not a rejection, so it doesn't show up in those
	// queries — same as any other non-pending, non-rejected record.
	if pending, _ := st.PendingProposals(ctx, "local"); len(pending) != 0 {
		t.Fatalf("PendingProposals = %+v; want none", pending)
	}
	if n, _ := st.RejectionCount(ctx, "local", "g1bad"); n != 0 {
		t.Fatalf("RejectionCount = %d, want 0", n)
	}
}
```

Also remove the `LastDryRun` assertion from `TestNoopStore` (store_test.go:121-123):

```go
	if dry, _ := st.LastDryRun(ctx, "c", "a"); dry != nil {
		t.Fatalf("Noop LastDryRun should be nil")
	}
```

Delete those 3 lines entirely (the closing `}` of the test stays).

- [ ] **Step 2: Run the test to confirm it fails to compile**

Run: `go test ./internal/store/... -run TestDryRunRecordCountsTowardLifecycle -v`
Expected: FAIL — compile error, `last.Status != StatusDryRun` line is fine, but the still-present `LastProposal` implementation excludes dry-run rows, so the new assertion `last.ID != id` will fail at runtime with `last == nil`. (If `LastDryRun` is still referenced anywhere else in this file, the compiler will also complain — there should be none left after Step 1.)

- [ ] **Step 3: Update store.go**

In `internal/store/store.go`, replace the `StatusDryRun` comment (lines 23-28):

```go
	// StatusDryRun marks a rehearsal record written by a dry_run chain instead
	// of a broadcast. Dry-run records are excluded from every real-lifecycle
	// query (LastProposal, PendingProposals, RejectionCount) so a rehearsal can
	// never block a real proposal once dry_run is disabled; they are consulted
	// only through LastDryRun to dedup rehearsal logs/notifications.
	StatusDryRun = "dry-run"
```

with:

```go
	// StatusDryRun tags a proposal record produced by a dry_run chain instead
	// of a real broadcast attempt, so store queries and operators can tell a
	// deliberate non-broadcast apart from a genuine gnokey failure (Success:
	// false, Status: ""). It counts toward the real lifecycle gate exactly
	// like any other record: LastProposal/RejectionCount/PendingProposals do
	// not exclude it, so disabling dry_run does not bypass the cooldown of a
	// proposal that was already shown to an operator.
	StatusDryRun = "dry-run"
```

Replace the `Store` interface's lifecycle-queries block (lines 91-99):

```go
	// Lifecycle queries.
	// LastProposal returns the validator's most recent real proposal; dry-run
	// records are excluded so a rehearsal never gates a real proposal.
	LastProposal(ctx context.Context, chain, address string) (*ProposalRecord, error)
	// LastDryRun returns the validator's most recent dry-run record, used to
	// dedup rehearsal logs/notifications across cycles.
	LastDryRun(ctx context.Context, chain, address string) (*ProposalRecord, error)
	PendingProposals(ctx context.Context, chain string) ([]ProposalRecord, error)
	RejectionCount(ctx context.Context, chain, address string) (int, error)
```

with:

```go
	// Lifecycle queries.
	// LastProposal returns the validator's most recent proposal record
	// (broadcast or dry-run) on this chain, or nil.
	LastProposal(ctx context.Context, chain, address string) (*ProposalRecord, error)
	PendingProposals(ctx context.Context, chain string) ([]ProposalRecord, error)
	RejectionCount(ctx context.Context, chain, address string) (int, error)
```

Remove the `Noop.LastDryRun` method (lines 147-149):

```go
func (Noop) LastDryRun(context.Context, string, string) (*ProposalRecord, error) {
	return nil, nil
}
```

Delete those 3 lines entirely.

- [ ] **Step 4: Update sqlite.go**

In `internal/store/sqlite.go`, replace `LastProposal`, `LastDryRun`, and `lastWhere` (lines 168-194):

```go
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
```

with (single query, no `WHERE`-clause parameter — `LastDryRun` had exactly one caller, which no longer exists after Task 4, so the indirection is no longer worth keeping):

```go
func (s *SQLite) LastProposal(ctx context.Context, chain, address string) (*ProposalRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+proposalCols+` FROM proposals WHERE chain = ? AND address = ? ORDER BY id DESC LIMIT 1`,
		chain, address)
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
```

- [ ] **Step 5: Run the store tests**

Run: `go test ./internal/store/... -v`
Expected: PASS — all tests including `TestDryRunRecordCountsTowardLifecycle`, `TestSQLiteLifecycle`, `TestNoopStore`.

(`go build ./...` for the whole repo will still fail — `internal/daemon/cycle.go` still calls `d.st.LastDryRun`. That's expected per Global Constraints; do not try to fix it here.)

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/sqlite.go internal/store/store_test.go
git commit -m "refactor(store): dry-run records count toward the real lifecycle gate"
```

---

## Task 2: Notify — drop EventDryRun, fix the Slack attachment wording

**Files:**
- Modify: `internal/notify/notify.go`
- Modify: `internal/notify/notify_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `notify.EventWouldEject` is the only non-broadcast event (unchanged constant value `"would_eject"`). `notify.EventDryRun` no longer exists. `Message(EventWouldEject, in)` wording changes (see Step 3) — daemon code in Task 4 must match the new substrings when asserting on notification bodies.

- [ ] **Step 1: Update notify_test.go to the new expected wording**

In `internal/notify/notify_test.go`, remove the dry-run block from `TestMessage` (lines 37-42):

```go
	// Dry-run messages are unmistakably labeled and say nothing went on-chain.
	dry := Message(EventDryRun, in)
	if !strings.HasPrefix(dry, "[DRY-RUN]") || !strings.Contains(dry, "Sam (g1abc)") ||
		!strings.Contains(dry, "nothing was broadcast") {
		t.Errorf("unexpected dry-run message: %q", dry)
	}

```

Delete those 6 lines entirely (keep the blank line before `// Moniker falls back...`).

Replace `TestMessageWouldEjectIncludesCommand` (lines 79-95) with:

```go
func TestMessageWouldEjectIncludesCommand(t *testing.T) {
	msg := Message(EventWouldEject, Info{
		Chain:         "local",
		Moniker:       "val4",
		Address:       "g1abc",
		GnokeyCommand: "printf … | gnokey maketx run … ./eject-val4.gno",
	})
	if !strings.Contains(msg, "val4") || !strings.Contains(msg, "g1abc") {
		t.Errorf("missing validator identity: %q", msg)
	}
	if !strings.Contains(msg, "dry_run is enabled") {
		t.Errorf("would_eject message should reference dry_run: %q", msg)
	}
	if !strings.Contains(msg, "gnokey maketx run") {
		t.Errorf("would_eject message should embed the gnokey command: %q", msg)
	}
	// The wording must not unconditionally claim a file was attached — Slack
	// webhooks can't upload, and fall back to an inline block instead.
	if !strings.Contains(msg, "attached, or inline") {
		t.Errorf("would_eject message should describe both delivery paths: %q", msg)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/notify/... -run 'TestMessage$|TestMessageWouldEjectIncludesCommand' -v`
Expected: FAIL — `TestMessage` won't even compile (`EventDryRun` still referenced elsewhere in the file is fine, but the test file no longer using it is fine too; the real failure is `TestMessageWouldEjectIncludesCommand` asserting on wording the current `Message()` doesn't produce yet).

- [ ] **Step 3: Update notify.go**

In `internal/notify/notify.go`, replace the `Event` constants block (lines 26-34):

```go
const (
	EventSent       Event = "sent"
	EventAccepted   Event = "accepted"
	EventRejected   Event = "rejected"
	EventWouldEject Event = "would_eject"
	// EventDryRun announces a proposal that WOULD have been sent by a dry_run
	// chain; nothing was signed or broadcast.
	EventDryRun Event = "dry-run"
)
```

with:

```go
const (
	EventSent       Event = "sent"
	EventAccepted   Event = "accepted"
	EventRejected   Event = "rejected"
	// EventWouldEject announces a proposal a dry_run chain would have sent;
	// nothing was signed or broadcast.
	EventWouldEject Event = "would_eject"
)
```

Replace the `EventWouldEject`/`EventDryRun` cases in `Message()` (lines 79-88):

```go
	case EventWouldEject:
		msg = fmt.Sprintf("Validator %s (%s) on %s qualifies for ejection, but proposal sending is disabled — no on-chain proposal was submitted.",
			in.display(), in.Address, in.Chain)
		if in.GnokeyCommand != "" {
			msg += "\nTo broadcast it manually, download the attached script and run:\n```\n" + in.GnokeyCommand + "\n```"
		}
		return msg
	case EventDryRun:
		msg = fmt.Sprintf("[DRY-RUN] katana would send a proposal to remove validator %s (%s) on %s from the valset — dry_run is enabled, nothing was broadcast.",
			in.display(), in.Address, in.Chain)
```

with:

```go
	case EventWouldEject:
		msg = fmt.Sprintf("Validator %s (%s) on %s qualifies for ejection, but dry_run is enabled — nothing was broadcast.",
			in.display(), in.Address, in.Chain)
		if in.GnokeyCommand != "" {
			msg += "\nTo broadcast it manually, run:\n```\n" + in.GnokeyCommand + "\n```\nThe ejection script is included with this notification (attached, or inline below if your channel doesn't support attachments)."
		}
		return msg
```

(The `attach` detection in `Dispatch()`, `attach := event == EventWouldEject && in.ProposalCode != "" && in.ProposalFilename != ""`, needs no change — it already checks `EventWouldEject`.)

- [ ] **Step 4: Run the notify tests**

Run: `go test ./internal/notify/... -v`
Expected: PASS — all tests including `TestMessage`, `TestMessageWouldEjectIncludesCommand`, `TestDispatchWouldEjectAttachesFile`.

- [ ] **Step 5: Commit**

```bash
git add internal/notify/notify.go internal/notify/notify_test.go
git commit -m "refactor(notify): drop EventDryRun, fix Slack attachment wording"
```

---

## Task 3: Config — single dry_run flag (global + per-chain override), remove proposal.enabled

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `Config.DryRun bool` (root, `yaml:"dry_run"`, global default). `ChainConfig.DryRun *bool` (was `bool`; nil = inherit the global default). `Runtime.DryRun bool` (resolved — same field name/type as before, now sourced from the global+override resolution instead of the raw per-chain field). `Proposal.Enabled` and `Proposal.BroadcastEnabled()` no longer exist — any caller must be updated to use `Runtime.DryRun` instead. `Config.BroadcastCapableChains() []string` keeps its exact signature but now resolves dry_run's global+override internally.

- [ ] **Step 1: Update config_test.go to the new expected behavior**

In `internal/config/config_test.go`, delete `TestProposalEnabledDefaultsTrue` (lines 206-218) and `TestProposalEnabledPerChainOverride` (lines 220-261) entirely — both lines ranges, including their blank line separators.

Replace `TestDryRunResolution` (lines 97-116) with:

```go
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
		t.Error("test chain dry_run = true, want default false (no global dry_run set)")
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
```

(`TestStorageNoneRequiresDryRunOnlyChains` and the `validYAML` constant at the top of the file need no changes — `dry_run: true` in YAML unmarshals into a `*bool` field exactly like it did into a `bool` field.)

- [ ] **Step 2: Run the tests to confirm the new one fails**

Run: `go test ./internal/config/... -run 'TestDryRunResolution|TestDryRunGlobalDefaultAndOverride' -v`
Expected: FAIL — `TestDryRunGlobalDefaultAndOverride` fails because `Config` has no `DryRun` field yet (compile error: `dry_run` YAML key is simply ignored today, so `inherits.DryRun` reads `ch.DryRun` which is `false`, the zero value, because `ChainConfig.DryRun` is still per-chain-only — the assertion `!inherits.DryRun` fails at runtime, not compile time, since extra YAML keys are ignored by default).

- [ ] **Step 3: Update config.go — struct fields**

Add the root `DryRun` field. In `internal/config/config.go`, replace the `Config` struct's `Proposal`/`VoteTracking` lines (lines 30-31):

```go
	Proposal      Proposal      `yaml:"proposal"`
	VoteTracking  VoteTracking  `yaml:"vote_tracking"`
```

with:

```go
	Proposal Proposal `yaml:"proposal"`
	// DryRun is the global default for whether chains broadcast: false
	// (default) broadcasts; true means every enabled chain rehearses instead
	// (logs + notifies, never signs/broadcasts), unless overridden per chain
	// via ChainConfig.DryRun.
	DryRun       bool         `yaml:"dry_run"`
	VoteTracking VoteTracking `yaml:"vote_tracking"`
```

Remove `Proposal.Enabled` and `BroadcastEnabled()`. Replace the `Proposal` struct and the method right after it (lines 125-153):

```go
type Proposal struct {
	// Enabled toggles on-chain broadcasting for this scope. nil/absent means
	// enabled (the broadcast-first default). false = observe-only: katana still
	// evaluates and notifies, but does not sign/broadcast the proposal on-chain.
	Enabled         *bool  `yaml:"enabled"`
	Cooldown        string `yaml:"cooldown"`
	RejectedBackoff string `yaml:"rejected_backoff"`
	MaxRejections   int    `yaml:"max_rejections"`
	// Submit selects the on-chain submission method: "run" (a .gno script via
	// gnokey maketx run — required for gno.land valset ejection) or "call" (a
	// single MsgCall to RealmPkgPath.Function with Args).
	Submit            string `yaml:"submit"`
	RunScriptTemplate string `yaml:"run_script_template"`
	// call-mode fields:
	RealmPkgPath string   `yaml:"realm_pkg_path"`
	Function     string   `yaml:"function"`
	Args         []string `yaml:"args"`

	TitleTemplate       string `yaml:"title_template"`
	DescriptionTemplate string `yaml:"description_template"`
}

// BroadcastEnabled reports whether katana should sign & broadcast proposals for
// this scope. Absent (nil) means enabled — the broadcast-first default.
func (p Proposal) BroadcastEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}
```

with:

```go
type Proposal struct {
	Cooldown        string `yaml:"cooldown"`
	RejectedBackoff string `yaml:"rejected_backoff"`
	MaxRejections   int    `yaml:"max_rejections"`
	// Submit selects the on-chain submission method: "run" (a .gno script via
	// gnokey maketx run — required for gno.land valset ejection) or "call" (a
	// single MsgCall to RealmPkgPath.Function with Args).
	Submit            string `yaml:"submit"`
	RunScriptTemplate string `yaml:"run_script_template"`
	// call-mode fields:
	RealmPkgPath string   `yaml:"realm_pkg_path"`
	Function     string   `yaml:"function"`
	Args         []string `yaml:"args"`

	TitleTemplate       string `yaml:"title_template"`
	DescriptionTemplate string `yaml:"description_template"`
}
```

Change `ChainConfig.DryRun` to a pointer. Replace (lines 200-204):

```go
	// DryRun runs the full evaluation pipeline for this chain but never signs
	// or broadcasts: the would-be proposal is logged and notified instead.
	// Meant for rehearsing katana against a live network before arming it.
	DryRun       bool        `yaml:"dry_run"`
```

with:

```go
	// DryRun overrides the global dry_run default for this chain (nil =
	// inherit the global value). true runs the full evaluation pipeline but
	// never signs or broadcasts: the would-be proposal is logged and notified
	// (with the ejection script and a ready-to-run gnokey command attached)
	// instead. Meant both for rehearsing katana on a new network and as a
	// standing manual-approval mode.
	DryRun       *bool       `yaml:"dry_run"`
```

- [ ] **Step 4: Update config.go — Runtime() resolution**

Replace the broadcast-resolution block in `Runtime()` (lines 394-408):

```go
	prop := c.Proposal
	if ch.Proposal != nil {
		prop = *ch.Proposal
	}
	// Resolve broadcasting: default true, set by global proposal.enabled, then
	// overridden by the per-chain proposal.enabled when present. After this,
	// prop.Enabled is always non-nil (BroadcastEnabled reads it directly).
	broadcast := true
	if c.Proposal.Enabled != nil {
		broadcast = *c.Proposal.Enabled
	}
	if ch.Proposal != nil && ch.Proposal.Enabled != nil {
		broadcast = *ch.Proposal.Enabled
	}
	prop.Enabled = &broadcast
	if _, err := parseDur(prop.Cooldown); err != nil {
		return Runtime{}, fmt.Errorf("proposal.cooldown: %w", err)
	}
```

with:

```go
	dryRun := c.DryRun
	if ch.DryRun != nil {
		dryRun = *ch.DryRun
	}

	prop := c.Proposal
	if ch.Proposal != nil {
		prop = *ch.Proposal
	}
	if _, err := parseDur(prop.Cooldown); err != nil {
		return Runtime{}, fmt.Errorf("proposal.cooldown: %w", err)
	}
```

Update the `Runtime{...}` literal's `DryRun` field (line 430):

```go
		DryRun:             ch.DryRun,
```

with:

```go
		DryRun:             dryRun,
```

- [ ] **Step 5: Update config.go — BroadcastCapableChains()**

Replace (lines 457-468):

```go
// BroadcastCapableChains returns the IDs of all enabled chains that may sign
// and broadcast (i.e. not in dry_run), sorted alphabetically.
func (c *Config) BroadcastCapableChains() []string {
	ids := make([]string, 0, len(c.Chains))
	for id, ch := range c.Chains {
		if ch.Enabled && !ch.DryRun {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
```

with:

```go
// BroadcastCapableChains returns the IDs of all enabled chains that may sign
// and broadcast (i.e. not in dry_run, resolving the global default and any
// per-chain override), sorted alphabetically.
func (c *Config) BroadcastCapableChains() []string {
	ids := make([]string, 0, len(c.Chains))
	for id, ch := range c.Chains {
		dryRun := c.DryRun
		if ch.DryRun != nil {
			dryRun = *ch.DryRun
		}
		if ch.Enabled && !dryRun {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
```

- [ ] **Step 6: Run the config tests**

Run: `go test ./internal/config/... -v`
Expected: PASS — all tests including `TestDryRunResolution`, `TestDryRunGlobalDefaultAndOverride`, `TestStorageNoneRequiresDryRunOnlyChains`, `TestLoadAndRuntime`, `TestExceptionsUnion`, `TestValidationErrors`.

(`go build ./...` for the whole repo will still fail — `internal/daemon/cycle.go` calls `rt.Proposal.BroadcastEnabled()`, which no longer exists. Expected per Global Constraints.)

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "refactor(config): single dry_run flag, global default + per-chain override"
```

---

## Task 4: Daemon — merge produce()'s two non-broadcast branches into one

**Files:**
- Modify: `internal/daemon/cycle.go`
- Modify: `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: `store.StatusDryRun`, `Store.LastProposal` (Task 1); `notify.EventWouldEject` (Task 2); `config.Runtime.DryRun bool`, `config.ChainConfig.DryRun *bool` (Task 3).
- Produces: `Daemon.produce(ctx, rt, gno, addr, moniker, reason)` — unchanged signature. `Daemon.dryRun(ctx, rt, built, rec, operator, moniker, addr)` — new signature (was `(ctx, rt, built, rec, operator)`); this is the only non-broadcast path now, called for every `rt.DryRun == true` chain regardless of what used to be `proposal.enabled`.

- [ ] **Step 1: Update daemon_test.go**

In `internal/daemon/daemon_test.go`, add `"time"` to the import block (currently missing):

```go
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
)
```

becomes:

```go
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/proposal"
	"github.com/samouraiworld/katana/internal/store"
)
```

Add a `boolPtr` helper right after the imports:

```go
func boolPtr(b bool) *bool { return &b }
```

In `TestStorageWarning` (line 231), change:

```go
			"test": {Enabled: true, DryRun: true},
```

to:

```go
			"test": {Enabled: true, DryRun: boolPtr(true)},
```

In `TestDryRunLogsAndNotifiesWithoutBroadcast`, `TestDryRunRepeatedCyclesDoNotSpam`, and `TestChainIDMismatchSkipsDryRunToo`, change every occurrence of:

```go
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = true })
```

(and the `"test999"` variant in `TestChainIDMismatchSkipsDryRunToo`) to use `boolPtr(true)`:

```go
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })
```

```go
	f := newFixture(t, "test999", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })
```

Replace `TestDryRunLogsAndNotifiesWithoutBroadcast` (lines 263-290) with:

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
	// The would-be proposal must be logged completely.
	for _, want := range []string{"g1bad", "g1op", "bad", "Eject bad (g1bad)", "missed 500 blocks"} {
		if !strings.Contains(logs, want) {
			t.Errorf("dry-run log missing %q:\n%s", want, logs)
		}
	}
	bodies := f.notifs.all()
	if len(bodies) != 1 {
		t.Fatalf("notifications = %d, want 1", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, "dry_run is enabled") {
		t.Errorf("dry-run notification should say dry_run is enabled:\n%s", body)
	}
	// Like the old observe mode, a dry_run notification attaches the ejection
	// script and a ready-to-run gnokey command.
	if !strings.Contains(body, "eject-bad.gno") {
		t.Errorf("dry-run notification missing the attached script filename:\n%s", body)
	}
	if !strings.Contains(body, "gnokey maketx run") {
		t.Errorf("dry-run notification missing the gnokey command:\n%s", body)
	}
	props, err := f.st.ListProposals(ctx, 10)
	if err != nil || len(props) != 1 || props[0].Status != store.StatusDryRun {
		t.Fatalf("proposals = %+v, err %v; want one dry-run record", props, err)
	}
}
```

Replace `TestDryRunRecordDoesNotBlockRealProposal` (lines 354-370) with:

```go
func TestDryRunRecordCountsTowardCooldown(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, "dev", func(c *config.Config) { c.Chains["test"].DryRun = boolPtr(true) })

	// Rehearse once, then arm the chain immediately.
	f.d.evalCycle(ctx, f.runtime(t))
	f.cfg.Chains["test"].DryRun = boolPtr(false)
	f.d.evalCycle(ctx, f.runtime(t))

	// The dry-run record counts toward the real lifecycle gate exactly like a
	// broadcast attempt would: the cooldown (720h in the fixture) is still in
	// effect immediately after, so disabling dry_run does not by itself
	// produce a real broadcast.
	if got := f.broadcasts.count(); got != 0 {
		t.Fatalf("broadcasts = %d, want 0: the cooldown from the dry-run record should still apply\nlogs:\n%s", got, f.logs.String())
	}
	props, _ := f.st.ListProposals(ctx, 10)
	if len(props) != 1 || props[0].Status != store.StatusDryRun {
		t.Fatalf("proposals = %+v; want only the original dry-run record (no new one produced yet)", props)
	}

	// Once the cooldown has elapsed, the real proposal goes out normally.
	f.d.now = func() time.Time { return time.Now().Add(800 * time.Hour) }
	f.d.evalCycle(ctx, f.runtime(t))
	if got := f.broadcasts.count(); got != 1 {
		t.Fatalf("broadcasts = %d, want 1 after the cooldown elapses\nlogs:\n%s", got, f.logs.String())
	}
}
```

- [ ] **Step 2: Run the daemon package tests to confirm they fail to compile**

Run: `go vet ./internal/daemon/...`
Expected: FAIL — `cycle.go` still calls `d.st.LastDryRun` (removed in Task 1), `notify.EventDryRun` (removed in Task 2), and `rt.Proposal.BroadcastEnabled()` (removed in Task 3).

- [ ] **Step 3: Rewrite cycle.go's evalCycle dedup block**

In `internal/daemon/cycle.go`, replace the lifecycle-gate block inside `evalCycle` (currently around lines 64-96, from the `// Lifecycle gate:` comment through the `// Safety:` comment):

```go
		// Lifecycle gate: don't re-propose while pending/accepted/within backoff.
		params := lifecycle.Params{
			Cooldown:        rt.Proposal.CooldownDur(),
			RejectedBackoff: rt.Proposal.RejectedBackoffDur(),
			MaxRejections:   rt.Proposal.MaxRejections,
			Now:             d.now(),
		}
		last, _ := d.st.LastProposal(ctx, rt.Key, addr)
		rejections, _ := d.st.RejectionCount(ctx, rt.Key, addr)
		if dec := lifecycle.Gate(last, rejections, params); !dec.Allow {
			d.logger.Printf("[%s][eval] SUPPRESS %s: %s", rt.Key, disp(moniker, addr), dec.Reason)
			continue
		}

		// Dry-run dedup: a rehearsal recorded within the cooldown window (via
		// Gate's default branch) suppresses another one, so dry-run cycles do
		// not re-log/re-notify the same would-be proposal every poll.
		if rt.DryRun {
			lastDry, _ := d.st.LastDryRun(ctx, rt.Key, addr)
			if dec := lifecycle.Gate(lastDry, 0, params); !dec.Allow {
				d.logger.Printf("[%s][eval] SUPPRESS dry-run for %s: %s", rt.Key, disp(moniker, addr), dec.Reason)
				continue
			}
		}

		// Safety: confirm the validator is in the live consensus valset.
```

with:

```go
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
```

- [ ] **Step 4: Rewrite produce() and dryRun()**

Replace the whole block from `produce`'s doc comment through the end of `dryRun` (currently lines 115-217: `// produce builds ...` through the closing `}` of `dryRun`) with:

```go
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
```

Check that `internal/daemon/cycle.go` still needs the `ejectFilename` function (defined further down in the same file, unchanged) and the `observePayload` type (unchanged) — both stay as-is, just referenced from `dryRun` now instead of an inline block in `produce`.

- [ ] **Step 5: Run the full build and daemon tests**

Run: `go build ./... && go vet ./... && go test ./internal/daemon/... -v`
Expected: PASS — the whole module builds again. All tests including `TestEvalCycleBroadcasts`, `TestDryRunLogsAndNotifiesWithoutBroadcast`, `TestDryRunRepeatedCyclesDoNotSpam`, `TestChainIDMismatchSkipsCycle`, `TestChainIDMismatchSkipsDryRunToo`, `TestDryRunRecordCountsTowardCooldown`, `TestStorageWarning`.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/cycle.go internal/daemon/daemon_test.go
git commit -m "refactor(daemon): merge dry_run and observe into one non-broadcast path"
```

---

## Task 5: Docs — README, config.yaml.example, deploy/e2e example, prd.md

**Files:**
- Modify: `README.md`
- Modify: `config.yaml.example`
- Modify: `deploy/e2e/katana.config.yaml.example`
- Modify: `prd.md`
- (Not committed — gitignored local file, see Step 5) `deploy/e2e/katana.config.yaml`

**Interfaces:** none — documentation only.

- [ ] **Step 1: Update README.md**

Replace the "Rehearsal / dry-run" section (currently lines 97-112):

```markdown
## Rehearsal / dry-run

Set `dry_run: true` on a chain to rehearse katana against a live network
before arming it. The eval worker runs the **full pipeline** — metrics,
conditions, exceptions, lifecycle gate, valset membership, operator
resolution — but instead of signing and broadcasting it:

- logs the complete would-be proposal (validator, operator, triggering
  conditions, title, description) with a `[DRY-RUN]` prefix;
- sends the normal notification, clearly labeled `[DRY-RUN]`;
- records a `dry-run` row in the proposal history so the same rehearsal is not
  re-logged/re-notified on every poll (the `proposal.cooldown` window applies).

Dry-run records are **excluded from the real lifecycle gate**: disabling
`dry_run` later lets a real proposal go out immediately. `dry_run` defaults to
`false` — a plain enabled chain is broadcast-armed.
```

with:

```markdown
## Rehearsal / dry-run

Set `dry_run: true` — globally (the default for every chain) or per chain,
overriding the global default — to run katana against a live network without
ever signing or broadcasting. The eval worker runs the **full pipeline** —
metrics, conditions, exceptions, lifecycle gate, valset membership, operator
resolution — but instead of signing and broadcasting it:

- logs the complete would-be proposal (validator, operator, triggering
  conditions, title, description) with a `[DRY-RUN]` prefix;
- sends the normal notification with the ejection script attached
  (Discord/Telegram; Slack falls back to inline code) and a ready-to-run
  `gnokey` command (password piped, never embedded), so an operator can
  review and broadcast it by hand;
- records a `dry-run` row in the proposal history so the same rehearsal is not
  re-logged/re-notified on every poll (the `proposal.cooldown` window
  applies).

Dry-run records count toward the **real lifecycle gate** exactly like a
broadcast attempt would: disabling `dry_run` does not bypass the cooldown of a
proposal that was already rehearsed and shown to an operator. Use `dry_run`
both to onboard a new chain and as a standing manual-approval mode. It
defaults to `false` — a plain enabled chain is broadcast-armed.
```

In the "Safety" section, replace the first bullet (currently lines 225-230):

```markdown
- katana broadcasts by default: every eligible validator on a chain without
  `dry_run: true` results in a signed, on-chain GovDAO proposal — rehearse
  with `dry_run` before arming a new chain. `gnokey` is embedded in the container image
  (pinned by the `GNO_REF` build-arg); the signing key is provisioned from a
  mnemonic at startup (`gnokey.mnemonic` / `KATANA_GNOKEY_MNEMONIC`) or from a
  pre-provisioned keybase.
```

with:

```markdown
- katana broadcasts by default: every eligible validator on a chain without
  `dry_run: true` results in a signed, on-chain GovDAO proposal — use
  `dry_run` to rehearse a new chain or as a standing manual-approval mode.
  `gnokey` is embedded in the container image
  (pinned by the `GNO_REF` build-arg); the signing key is provisioned from a
  mnemonic at startup (`gnokey.mnemonic` / `KATANA_GNOKEY_MNEMONIC`) or from a
  pre-provisioned keybase.
```

- [ ] **Step 2: Update config.yaml.example**

Add a root-level `dry_run` example. Replace (currently lines 17-19):

```yaml
# Daemon cadence: how often each chain worker re-evaluates its validators.
# This is the RUN cadence — NOT a condition window (see conditions.* below).
poll_interval: 5m
```

with:

```yaml
# Daemon cadence: how often each chain worker re-evaluates its validators.
# This is the RUN cadence — NOT a condition window (see conditions.* below).
poll_interval: 5m

# dry_run: true rehearses every chain (or one chain, if set under chains.<name>)
# without ever signing or broadcasting — see "Rehearsal / dry-run" in README.md.
# Overridable per chain via chains.<name>.dry_run. Defaults to false.
dry_run: false
```

Remove the `proposal.enabled` field and its comment. Replace (currently lines 69-75):

```yaml
proposal:
  # enabled: true broadcasts on-chain (default). Set false for observe-only:
  # katana still evaluates and NOTIFIES but does not sign/broadcast. The observe
  # notification attaches the ejection .gno script (Discord/Telegram) and gives
  # the ready-to-run gnokey command, so an operator can broadcast it by hand. Not
  # a dry-run/artifact mode. Overridable per chain via chains.<name>.proposal.enabled.
  enabled: true
  cooldown: 720h                      # min interval between two proposals for the same validator (30d)
```

with:

```yaml
proposal:
  cooldown: 720h                      # min interval between two proposals for the same validator (30d)
```

Update the per-chain `dry_run` comment in the "Chains" section. Replace (currently lines 146-148):

```yaml
# dry_run             = rehearsal mode (default false): run the full pipeline
#                       but log + notify the would-be proposal instead of
#                       signing/broadcasting. Use it to onboard a new chain.
```

with:

```yaml
# dry_run             = per-chain override of the root dry_run default: run
#                       the full pipeline but log + notify the would-be
#                       proposal (script attached + gnokey command) instead of
#                       signing/broadcasting. Use it to onboard a new chain or
#                       as a standing manual-approval mode.
```

- [ ] **Step 3: Update deploy/e2e/katana.config.yaml.example**

Add root `dry_run`. Replace (currently lines 13-14):

```yaml
log_level: info
poll_interval: 15s
```

with:

```yaml
log_level: info
poll_interval: 15s
dry_run: false          # true = observe-only: evaluate + notify (attach .gno script + gnokey command), no broadcast
```

Remove `proposal.enabled`. Replace (currently line 39-41):

```yaml
proposal:
  enabled: true          # false = observe-only: evaluate + notify (attach .gno script + gnokey command), no broadcast
  submit: run
```

with:

```yaml
proposal:
  submit: run
```

- [ ] **Step 4: Update prd.md**

Merge FR-6.6 and FR-6.6a. Replace (currently lines 175-186):

```markdown
- **FR-6.6** katana broadcasts on-chain by default. There is no dry-run/artifact
  mode and no CLI flag to select behaviour. *(decision D9 — dry-run removed.)*
  `[CHANGE]`
- **FR-6.6a** `proposal.enabled` (default `true`, overridable per chain) is an
  **observe toggle**: when `false`, katana still evaluates candidates and sends a
  `would_eject` notification, records the proposal row so the lifecycle cooldown
  gate prevents re-notification, but does **not** sign/broadcast on-chain. The
  notification is a **manual-approval hand-off**: it attaches the ejection script
  as a downloadable `.gno` file (Discord/Telegram; Slack falls back to inline
  code) and includes the ready-to-run `gnokey` command (password piped, never
  embedded), so an operator can review and broadcast it themselves. This is an
  arm/observe switch for safe rollout, not a return of dry-run.
```

with:

```markdown
- **FR-6.6** katana broadcasts on-chain by default. `dry_run` (default `false`,
  global with per-chain override) is the sole toggle for the opposite: when
  `true`, katana still evaluates candidates and sends a `would_eject`
  notification, and records the proposal row (`status: dry-run`) so the real
  lifecycle cooldown gate prevents re-notification and does not reset once
  `dry_run` is disabled. The notification is a **manual-approval hand-off**: it
  attaches the ejection script as a downloadable `.gno` file (Discord/Telegram;
  Slack falls back to inline code) and includes the ready-to-run `gnokey`
  command (password piped, never embedded), so an operator can review and
  broadcast it themselves. Serves both onboarding rehearsal and a standing
  manual-approval mode. *(decision D9 — see amendment below.)* `[CHANGE]`
```

Update the D9 amendment. Replace (currently lines 626-633):

```markdown
- **D9 — Run mode.** ✅ **Superseded: katana is broadcast-only.** There is no
  `proposal.mode` / dry-run setting; every eligible proposal is signed and
  submitted on-chain via gnokey. The signing key is either provisioned from a
  configured mnemonic at startup (`internal/keybase`) or already present in a
  pre-provisioned keybase in production.
  *Amended: a per-chain `dry_run` rehearsal switch was later reintroduced for
  onboarding katana on live networks (see README "Rehearsal / dry-run");
  broadcast remains the default.*
```

with:

```markdown
- **D9 — Run mode.** ✅ **Superseded: katana is broadcast-only.** There is no
  `proposal.mode` setting; every eligible proposal is signed and submitted
  on-chain via gnokey unless `dry_run` says otherwise. The signing key is
  either provisioned from a configured mnemonic at startup
  (`internal/keybase`) or already present in a pre-provisioned keybase in
  production.
  *Amended: a `dry_run` toggle (global default + per-chain override) was later
  reintroduced, covering both onboarding rehearsal and a standing
  manual-approval mode (see README "Rehearsal / dry-run"); broadcast remains
  the default.*
```

- [ ] **Step 5: Update the local (gitignored) e2e config if present**

Check whether `deploy/e2e/katana.config.yaml` exists on disk (it is gitignored, so it may or may not be present depending on the local checkout):

Run: `test -f deploy/e2e/katana.config.yaml && echo present || echo absent`

If `present`, apply the same two edits as Step 3 to this file too (add root `dry_run: false` near `poll_interval: 15s`; remove `enabled: false` — note this file currently has `enabled: false` with comment `# OBSERVE smoke: evaluate + notify with the proposal code, NO broadcast`, so set `dry_run: true` at the root instead of `false`, to preserve its current observe-smoke-test behavior). Do **not** `git add` this file — it stays untracked.

- [ ] **Step 6: Verify no stale references remain**

Run: `grep -rn "proposal\.enabled\|BroadcastEnabled" README.md config.yaml.example deploy/e2e/katana.config.yaml.example prd.md`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add README.md config.yaml.example deploy/e2e/katana.config.yaml.example prd.md
git commit -m "docs: describe the consolidated dry_run flag"
```

---

## Task 6: Final whole-repo verification

**Files:** none (verification only).

**Interfaces:** none.

- [ ] **Step 1: Full build, vet, format check**

Run: `go build ./... && go vet ./... && gofmt -l .`
Expected: no errors, no output from `gofmt -l .` (nothing needs reformatting).

- [ ] **Step 2: Full test suite with race detector**

Run: `go test ./... -race`
Expected: `ok` for every package (`cmd/katana` reports `[no test files]`, that's fine).

- [ ] **Step 3: Confirm no leftover references to removed symbols**

Run: `grep -rn "LastDryRun\|EventDryRun\|BroadcastEnabled\|Proposal.Enabled" --include=*.go .`
Expected: no output.

- [ ] **Step 4: Update the design spec's status (optional but recommended)**

If the implementer wants a record that this plan was completed, append a one-line note to the top of `docs/superpowers/specs/2026-07-19-consolidate-dry-run-design.md` (e.g. `> Implemented — see docs/superpowers/plans/2026-07-19-consolidate-dry-run.md`) and commit it.

- [ ] **Step 5: Final commit (if Step 4 was done)**

```bash
git add docs/superpowers/specs/2026-07-19-consolidate-dry-run-design.md
git commit -m "docs: mark dry_run consolidation spec as implemented"
```
