# Consolidate `dry_run` and `proposal.enabled` into a single flag

## Context

Two independent branches landed two overlapping "don't broadcast, tell someone
what would happen" mechanisms:

- `proposal.enabled` (this branch, `feat/proposal-enabled-toggle`, FR-6.6a): a
  global-with-per-chain-override switch. When `false`, katana evaluates
  normally but doesn't sign/broadcast; it sends a `would_eject` notification
  with the ejection script attached and a ready-to-run `gnokey` command, and
  records the attempt with `Status: ""` so the real lifecycle cooldown gate
  applies to it.
- `dry_run` (PR #9, `feat/operational-safety-rails`): a per-chain-only switch
  for rehearsing katana against a live network before arming it. When `true`,
  katana runs the full pipeline and logs/notifies the would-be proposal, but
  records it with a dedicated `store.StatusDryRun` that is excluded from every
  real-lifecycle query — so disabling `dry_run` lets a real proposal go out
  immediately, with no memory of prior rehearsals.

Merging `master` (which carries PR #9) into this branch required resolving a
conflict between the two in `internal/daemon/cycle.go` and
`internal/notify/notify.go`. A subsequent code review of the merged branch
found the two mechanisms are now redundant and inconsistent with each other
(different notification payloads, different store semantics, a validation
helper that only knows about one of them). This spec consolidates them into
one flag.

## Goal

A single `dry_run` config flag (global default + per-chain override, replacing
`proposal.enabled`) that governs whether katana broadcasts:

- `dry_run: false` (default) — sign and broadcast on-chain, as today.
- `dry_run: true` — never signs or broadcasts. Evaluates the full pipeline
  (metrics → conditions → exceptions → lifecycle gate → valset check →
  operator resolution → chain-id assert), then logs the would-be proposal and
  sends a notification carrying the attached ejection script and a
  copy-pasteable `gnokey` command — today's `proposal.enabled: false`
  behavior. The record counts toward the real lifecycle cooldown gate (today's
  `proposal.enabled` behavior, not PR #9's exclusion), so disabling `dry_run`
  does not bypass the cooldown of a proposal that was already rehearsed/shown
  to an operator.

`proposal.enabled` is removed entirely.

## Config (`internal/config`)

- Remove `Proposal.Enabled *bool`, `Proposal.BroadcastEnabled()`, and the
  per-field fallback block in `Runtime()` that resolves it (currently
  config.go:398-408).
- `ChainConfig.DryRun` changes from `bool` to `*bool` (nil = inherit the
  global default). Add `Config.DryRun bool` at the root, alongside
  `poll_interval` / `conditions` / `proposal` / `gnokey`, following the same
  global-with-per-chain-override model documented in CLAUDE.md.
  `Runtime.DryRun` stays a resolved `bool`.
- `BroadcastCapableChains()` (used by the `storage.driver: none` fail-start
  check) resolves the same global+override logic inline — a two-line
  `dryRun := c.DryRun; if ch.DryRun != nil { dryRun = *ch.DryRun }` — rather
  than reading `ch.DryRun` raw. It runs before the per-chain `Runtime()`
  validation loop in `validate()`, so it cannot rely on an already-resolved
  `Runtime`. Once `proposal.enabled` is gone, this function's notion of
  "broadcast-capable" is exact again (it was already blind to
  `proposal.enabled`; removing that flag closes the gap rather than widening
  it).

## Daemon (`internal/daemon/cycle.go`)

- `produce()` keeps exactly one non-broadcast branch, gated on `rt.DryRun`.
  It absorbs today's `proposal.enabled: false` block: build `code` (script or
  call-expression), `filename` (via `ejectFilename`), `command` (via
  `built.Command`), record the proposal, log, and dispatch
  `notify.EventWouldEject` with the populated `observePayload`.
- The dedicated dry-run dedup check in `evalCycle` (the second `lifecycle.Gate`
  call against `d.st.LastDryRun`) is deleted. Because dry-run records no
  longer are excluded from `LastProposal` (see Store below), the existing
  `Gate` call already in `evalCycle` (computed from `LastProposal` /
  `RejectionCount`) provides the same dedup, matching how the observe path
  works today.
- The method stays named `d.dryRun(ctx, rt, built, rec, operator)`, matching
  the config flag name; the old `proposal.enabled` inline block and the old
  `d.dryRun` body are merged into this one method.

## Store (`internal/store`)

- Keep `store.StatusDryRun` as a tag on the row (distinguishes, in the DB, an
  intentional non-broadcast from a genuine broadcast failure — currently both
  collapse to `Success: false, Status: ""` for observe-mode rows, a gap the
  code review flagged). `produce()`'s dry-run branch sets
  `rec.Status = store.StatusDryRun` and `rec.Success = true`.
- `LastProposal`, `RejectionCount`, and `PendingProposals` stop excluding
  `StatusDryRun` rows — they count toward the real lifecycle gate like any
  other attempt. `PendingProposals` and `RejectionCount` already filter on
  `Status = StatusPending` / `StatusRejected` respectively, so no change is
  needed there beyond `LastProposal` dropping its `status != ?` filter.
- Remove `LastDryRun` from the `Store` interface, `SQLite`, and `Noop`
  (superseded by the plain `LastProposal` path).

## Notify (`internal/notify`)

- Remove `EventDryRun`. `EventWouldEject` becomes the single event for the
  non-broadcast case, with its message generalized to fit both origin stories,
  e.g.: `"Validator %s (%s) on %s qualifies for ejection, but dry_run is
  enabled — nothing was broadcast."` (dropping the `proposal.enabled`-specific
  "proposal sending is disabled" wording), still followed by the
  download/command block when `GnokeyCommand != ""`.
- Fix the Slack wording bug found in review: only append "download the
  attached script and run..." when an attachment actually exists for that
  channel; Slack (webhook, no upload) should say the code follows inline
  instead of pointing at a nonexistent download.

## Docs

- README: merge the "Rehearsal / dry-run" section and the `proposal.enabled`
  references in "Safety"/"Notifications" into one description of `dry_run`.
- `config.yaml.example`: replace the `proposal.enabled` comment block with
  `dry_run` (root + per-chain example), matching the new global+override
  shape.
- `prd.md`: remove FR-6.6a (superseded), update the D9 amendment note to
  describe the final single-flag design instead of "a per-chain `dry_run`
  rehearsal switch was later reintroduced" (which now undersells it — it's the
  full replacement for the observe toggle too).

## Tests

- `internal/config/config_test.go`: replace `TestProposalEnabledPerChainOverride`
  and `TestDryRunResolution`/`TestStorageNoneRequiresDryRunOnlyChains` with
  versions exercising the merged global+override `dry_run` resolution.
- `internal/daemon/daemon_test.go`: consolidate `TestDryRunLogsAndNotifiesWithoutBroadcast`
  et al. so the single dry-run path is asserted to (a) never call the
  broadcaster, (b) attach the script/command in its notification, (c) dedup
  across repeated cycles via the normal `Gate`/`LastProposal` path, (d) count
  toward the cooldown when later disabled (i.e. no longer "goes out
  immediately" — this is an intentional behavior change from PR #9's
  standalone `dry_run`, confirmed with the user), (e) still skip cleanly on a
  chain-id mismatch.
- `internal/store/store_test.go`: update `TestDryRunRecordsExcludedFromLifecycle`
  to assert the opposite — dry-run rows now *are* visible to `LastProposal`
  and gate a subsequent real proposal within the cooldown window.
- `internal/notify/notify_test.go`: drop the `EventDryRun` case, extend the
  `EventWouldEject` test for the generalized message and the Slack
  no-attachment wording fix.

## Out of scope

- The chain-id assert (`assertChainID`) and the `storage.driver: none`
  fail-start check's core rationale are unaffected — only
  `BroadcastCapableChains()`'s resolution of "capable" changes as described
  above.
- No change to `proposal.submit` (`run`/`call`), `cooldown`,
  `rejected_backoff`, `max_rejections`, or any other `Proposal` field.
