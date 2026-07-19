# katana — Product Requirements Document (PRD)
idée # retirer tu voting power systeme de freeze 
Status: **draft for validation** · Owner: Samourai · Last updated: 2026-07-02

This document defines the target behaviour of katana. It is the single source of
truth for scope and design. **No code is written from this document until it is
validated.** Sections tagged `[EXISTS]` are already implemented; `[TO BUILD]`
are not yet implemented; `[CHANGE]` modify existing behaviour.

---

## 1. Summary

katana is a **continuously running backend service** that watches gno.land
validators and automatically produces (and optionally submits) GovDAO proposals
to eject validators that fail configurable health conditions.

It reads validator health from a running
[gnomonitoring](https://github.com/samouraiworld/gnomonitoring) instance, polls
it on a regular cadence, evaluates each validator against thresholds defined in
`config.yaml`, and — when a validator qualifies — creates an ejection proposal,
records it, and sends a notification to Discord / Telegram / Slack.

## 2. Goals

- **G1** — Run as a long-lived backend (daemon), not a one-shot CLI. `[CHANGE]`
- **G2** — Poll the gnomonitoring API on a configurable cadence to obtain, per
  validator, the number of missed blocks and CRITICAL alerts. `[EXISTS]` (poll data) / `[TO BUILD]` (the recurring loop)
- **G3** — Decide eligibility from fully configurable conditions (thresholds +
  time windows + combination logic) in `config.yaml`. `[EXISTS]`
- **G4** — Support multiple networks (testnet / betanet / mainnet …), each
  independently enabled/disabled from `config.yaml`. `[EXISTS]`
- **G5** — Protect specific validators via an exceptions list. `[EXISTS]`
- **G6** — Produce and broadcast a proposal on-chain via gnokey for every
  eligible validator. `[EXISTS]`
- **G7** — Send a notification (Discord, Telegram, Slack) whenever a proposal is
  produced/submitted. `[TO BUILD]`
- **G8** — Never spam duplicate proposals for the same validator (dedup /
  cooldown). `[TO BUILD]`
- **G9** — Persist history (evaluations + proposals) to SQLite. `[EXISTS]`
- **G10** — Be deployable as a container that stays up (`restart: unless-stopped`),
  with graceful shutdown. `[CHANGE]`

## 3. Non-goals (for this iteration)

- Web UI / dashboard. The store schema is prepared for it, but the server is out
  of scope here.
- Computing validator metrics from raw chain data inside katana (gnomonitoring
  remains the metrics source).
- Implementing signing logic inside katana. Signing is delegated to the
  embedded `gnokey` binary + keybase; katana only provisions the keybase entry
  from a configured mnemonic at startup (§6.6b), it never signs itself.
- Voting on proposals or tracking their outcome after submission.

## 4. Users

- **Validator-set operators / GovDAO members** who want under-performing
  validators surfaced and proposed for ejection automatically, fully automated
  end-to-end (katana always broadcasts on-chain).

## 5. System overview

katana runs as a service. On start it launches, **per enabled chain, two
workers**: an **evaluation worker** (finds bad validators and creates proposals)
and a **vote-watcher worker** (follows the GovDAO outcome of proposals katana
submitted). Each loops on its own ticker.

```
                    ┌──────────────────────────── katana service ────────────────────────────┐
 gnomonitoring API ─┼─► eval-worker(chain N) ─► evaluate ─► lifecycle gate ─► proposal ─► notify ─► store
   /missing_block   │                            (rules)   (state machine)   (broadcast)   (sent)
   /admin/alerts    │
 gno.land RPC ──────┼─► (valset safety check)
   /validators      │
 GovDAO realm ──────┼─► vote-worker(chain N) ─► poll proposal status ─► on ACCEPTED / REJECTED:
   (proposal state) │                                                    notify + update state + store
                    └─────────────────────────────────────────────────────────────────────────┘
        SIGINT/SIGTERM ─► graceful shutdown (stop tickers, flush, close store)
```

**Evaluation worker** cycle (every `poll_interval`):
1. Query gnomonitoring for missed blocks (window = condition interval).
2. Query gnomonitoring for CRITICAL alert counts (window = condition interval).
3. Build the candidate set (union of observed addresses).
4. For each candidate: evaluate conditions → check exceptions → **lifecycle gate**
   (skip if a proposal is pending / accepted / within cooldown or rejection
   backoff) → valset safety check → build proposal → broadcast via gnokey →
   notify *proposal sent* → persist evaluation + proposal.
5. Sleep until next tick.

**Vote-watcher worker** cycle (every `vote_poll_interval`):
1. For each of katana's proposals still `pending`, query the GovDAO realm for its
   status.
2. On **accepted/executed** (validator ejected): notify *proposal accepted*, mark
   `accepted`.
3. On **rejected** (GovDAO keeps the validator): notify *proposal rejected*, mark
   `rejected`, and start the rejection backoff (re-propose allowed only after
   `rejected_backoff`).
4. Persist every state change.

## 6. Functional requirements

### 6.1 Continuous operation `[CHANGE]`
- **FR-1.1** katana starts and runs until it receives SIGINT/SIGTERM.
- **FR-1.2** It monitors every **enabled** chain concurrently, each with its own
  loop, so a slow/unreachable chain does not block the others.
- **FR-1.3** Poll cadence is `poll_interval`, configurable globally and
  overridable per chain. Distinct from the condition **windows** (see §7 note).
- **FR-1.4** A failed cycle (e.g. gnomonitoring unreachable) is logged and the
  worker retries on the next tick; it never crashes the whole service.
- **FR-1.5** Graceful shutdown: stop tickers, finish any in-flight cycle up to a
  bounded timeout, close the store.
- **FR-1.6** **No one-shot / `--once` mode.** katana is always a permanent
  backend. *(decision D4 — confirmed: backend must run continuously.)* `[CHANGE]`

### 6.2 Chain selection `[EXISTS]`
- **FR-2.1** Each chain in `config.yaml` has `enabled: true|false`. Only enabled
  chains are monitored.
- **FR-2.2** Each chain declares its RPC endpoint, its signing `chain_id`, and
  its `gnomonitoring_chain` key.

### 6.2b Chain discovery from gnomonitoring `[TO BUILD]`

- **FR-2b.1** katana can fetch the list of chains gnomonitoring exposes via
  `GET /info`, which returns `enabled_chains` plus, per chain, its
  `rpc_endpoints`, `graphqls` and `gnowebs`. **Confirmed available** in the
  current gnomonitoring backend (public endpoint, no admin token).
- **FR-2b.2** Discovery is used to:
  (a) **validate** that every `gnomonitoring_chain` in `config.yaml` actually
  exists on the gnomonitoring side, warning on mismatch at startup; and
  (b) optionally **auto-populate** a chain's `rpc_endpoint` / gnoweb URL from
  gnomonitoring instead of hardcoding it. *(decision D8)*
- **FR-2b.3** katana never signs against an auto-discovered chain unless that
  chain is explicitly `enabled` in `config.yaml` (discovery informs, config
  authorises). Safety guardrail so a new gnomonitoring chain can't silently
  trigger on-chain actions.
- **FR-2b.4** If `/info` is unreachable, katana falls back to the chains defined
  in `config.yaml` and logs a warning; discovery is best-effort.

### 6.3 Metrics ingestion `[EXISTS]`
- **FR-3.1** Missed blocks from `GET /missing_block?chain=&period=`.
- **FR-3.2** CRITICAL alert counts from `GET /admin/alerts?chain=&level=CRITICAL&from=`
  (admin-protected → `gnomonitoring.api_token`).

### 6.4 Eligibility conditions `[EXISTS]`
- **FR-4.1** `missed_blocks`: eligible when missed blocks over its window exceed
  `max`.
- **FR-4.2** `critical_alerts`: eligible when CRITICAL alerts over its window
  exceed `max`.
- **FR-4.3** Each condition is independently toggleable; `require_all` combines
  enabled conditions as AND (true) or OR (false).
- **FR-4.4** Conditions can be overridden per chain.

### 6.5 Exceptions `[EXISTS]`
- **FR-5.1** Validators in `exceptions` are never proposed. Matched by address
  (case-insensitive) or moniker. Global list ∪ per-chain list.

### 6.6 Proposal generation & submission `[EXISTS]`
- **FR-6.1** katana **always broadcasts**: build the proposal, then sign &
  submit it on-chain via gnokey. There is no dry-run mode and no artifact-only
  output — every eligible, non-suppressed validator results in a real
  broadcast attempt.
- **FR-6.3** Before proposing, confirm the target is in the live consensus valset
  (skippable with `--skip-valset-check`).
- **FR-6.4** Realm path / function / args are configurable (per chain).
- **FR-6.5** `config.yaml` holds the **proposing wallet**: its address and its
  password (plus the keybase key name/home used by gnokey). `[CHANGE]`
  - `gnokey.address` — the `g1...` address that submits proposals. Used to (a)
    label proposals/notifications with the author, and (b) optionally
    cross-check that gnokey signs with the expected key.
  - `gnokey.password` — unlocks the key for signing. **Secret**: overridable via
    `KATANA_GNOKEY_PASSWORD`; strongly recommended to keep it out of the file in
    production.
  - The wallet is used on every cycle: katana has no other run path.
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
- **FR-6.6b** The signing key is provisioned into the gnokey keybase from a
  configured **mnemonic** (`gnokey.mnemonic`, env `KATANA_GNOKEY_MNEMONIC`) at
  startup: `internal/keybase.Ensure` imports the derived key under
  `gnokey.key_name` if it is not already present, and is a no-op otherwise
  (idempotent). In production the keybase is typically **pre-provisioned**
  (mounted volume / already-imported key) and no mnemonic is configured, so
  `Ensure` is a no-op and katana relies on the existing key. `gnokey` itself
  ships **inside the katana container image**, built from
  `github.com/gnolang/gno/gno.land/cmd/gnokey` and pinned by the `GNO_REF`
  build-arg (see `Dockerfile`), so no separate gnokey install is required at
  deploy time.
- **FR-6.7** **Per-chain wallet.** The proposing wallet (address, key name,
  password, home, gas) is defined globally under `gnokey:` and can be
  **overridden per chain**. Rationale: a key is only a GovDAO member on the
  network(s) where it was granted membership, and gas is paid in each chain's
  own token, so a single shared wallet is often not viable across testnet /
  betanet / mainnet. When a chain has no override, it uses the global wallet.
  *(decision D10 — proposed: global default + per-chain override.)* `[CHANGE]`

### 6.7 Proposal lifecycle gate (dedup) `[TO BUILD]`

katana tracks a per-`(chain, validator)` state machine, derived from the
`proposals` history, and consults it before creating any new proposal:

```
        none ──propose──► pending ──accepted──► accepted (terminal; validator ejected)
         ▲                   │
         │                   └──rejected──► rejected ──(rejected_backoff elapsed)──► none
         └──(cooldown elapsed, still eligible)──────────────────────────────────────┘
```

- **FR-7.1** If a proposal for the validator is **pending** (vote in progress),
  never create another one.
- **FR-7.2** If the last proposal was **accepted**, the validator has been
  ejected → do not re-propose (and the valset safety check blocks it anyway).
- **FR-7.3** If the last proposal was **rejected** (GovDAO kept the validator),
  do not re-propose until `rejected_backoff` has elapsed; then, if still eligible,
  propose again. *(decision D1 / D11)*
- **FR-7.4** `cooldown` is a global minimum interval between two proposals for
  the same validator, applied in all cases as a floor. Both `cooldown` and
  `rejected_backoff` are configurable (`proposal.cooldown`,
  `proposal.rejected_backoff`), default `720h` (30d). *(decision D1 — confirmed:
  configurable.)*
- **FR-7.5** Suppressed validators are still evaluated and recorded, logged with
  the reason (`pending` / `cooldown` / `rejection backoff`), but emit no proposal.
- **FR-7.6** Cap on repeated rejections (`proposal.max_rejections`, per-chain
  overridable): after N GovDAO rejections for the same validator, stop
  auto-proposing and require manual intervention, to avoid fighting a standing
  GovDAO decision. **Default `2`**; `0` = unlimited. *(decision D12 — confirmed.)*

### 6.8 Vote tracking `[TO BUILD]`

- **FR-VT.1** For every proposal katana **broadcasts**, it captures the resulting
  **on-chain GovDAO proposal id** from the `ProposalCreated` event emitted by
  `gno.land/r/gov/dao` (attr `key: "id"`). *(decision D13 — resolved.)* The id is
  read from the broadcast tx response events; if unavailable there (e.g. gnokey
  CLI broadcast without event capture), fall back to querying the GraphQL indexer
  for that tx (same query gnomonitoring uses — see references). Reference impl:
  gnomonitoring `ExtractGovDAOIDs` / `ProcessProposal`.
- **FR-VT.2** The vote-watcher worker polls each pending proposal's status on
  `vote_poll_interval` via RPC `vm/qrender` on `gno.land/r/gov/dao:<id>` and maps
  the rendered status to katana states: `ACCEPTED → accepted`,
  `ACTIVE` / vote-in-progress `→ pending`, otherwise `→ rejected`. *(decision D14
  — resolved: RPC render, mirroring gnomonitoring `ExtractProposalRender`; the
  GraphQL indexer remains an optional fallback for id/discovery.)*
- **FR-VT.2b** Status parsing is render-string-based and **realm-version
  dependent** (some realms don't expose the same accessor). Parsing must be
  resilient: an unrecognised/`UNKNOWN` render keeps the proposal `pending` and
  retries next tick — it must never be mis-classified as `rejected`.
- **FR-VT.3** On resolution, katana notifies (FR-8) and updates the state machine
  (FR-7).
- **FR-VT.4** On **accepted**, katana may additionally confirm the validator has
  actually left the consensus valset (RPC `/validators`) before declaring it
  removed.
- **FR-VT.5** Vote tracking always applies: every produced proposal is a real
  on-chain broadcast, so every one has a GovDAO id to follow.

### 6.9 Notifications `[TO BUILD]`

Notifications never fire on a failed/errored broadcast; they follow the on-chain
lifecycle. Three events, all in English (decision D3):

- **FR-8.1** *Proposal sent* — after a **successful broadcast**:
  > `A proposal for validator <moniker> (<address>) on <chain> has been sent to remove it from the valset.`
- **FR-8.2** *Proposal accepted* — when GovDAO accepts (validator ejected):
  > `GovDAO accepted the proposal for validator <moniker> (<address>) on <chain>: it has been removed from the valset.`
- **FR-8.3** *Proposal rejected* — when GovDAO votes to keep the validator:
  > `GovDAO rejected the proposal for validator <moniker> (<address>) on <chain>: it stays in the valset. A new proposal may be sent after <rejected_backoff>.`
- **FR-8.4** Sent to every enabled channel among Discord, Telegram, Slack
  (webhook URL for Discord/Slack; bot token + chat id for Telegram). A gnoweb
  link to the proposal is appended when the chain's `gnoweb_url` is known.
- **FR-8.5** Best-effort: a failed notification is logged, never blocks nor
  crashes a worker; short timeout.

### 6.10 Persistence `[EXISTS + extend]`

- **FR-9.1** `storage.driver: none | sqlite`. SQLite stores `evaluations` and
  `proposals`.
- **FR-9.2** The `proposals` table is extended with the on-chain proposal id and
  lifecycle status (see §8); it is the source of truth for the lifecycle gate
  (FR-7) and the future web UI.
- **FR-9.3** A `notifications` table records every notification attempt
  (channel, target, event, status, error) for audit/history. *(decision D5 —
  confirmed.)* `[TO BUILD]`

## 7. Configuration (target `config.yaml`)

> New/changed keys vs. today are marked. Everything else is unchanged from the
> current `config.yaml.example`.

```yaml
log_level: info

# [NEW] Daemon poll cadence. How often each chain worker re-evaluates.
# This is the RUN cadence — NOT the condition window (see note below).
poll_interval: 5m

gnomonitoring:
  base_url: "http://localhost:8989"     # repo: https://github.com/samouraiworld/gnomonitoring
  api_token: ""
  timeout_seconds: 10
  missed_blocks_path: "/missing_block"
  alerts_path: "/admin/alerts"
  info_path: "/info"                    # [NEW] chain discovery endpoint
  discover_chains: true                 # [NEW] validate config against /info only; URLs stay in chains.* (D8)

# [NEW] Global proposing wallet. Overridable per chain (see chains.* below).
# gnokey ships inside the katana image, pinned by the Dockerfile's GNO_REF
# build-arg — no separate install needed at deploy time.
gnokey:
  bin: "gnokey"
  key_name: ""                          # keybase key name
  address: ""                           # [NEW] proposing wallet address g1... (env: KATANA_GNOKEY_ADDRESS)
  home: ""
  password: ""                          # [NEW-emphasis] secret (env: KATANA_GNOKEY_PASSWORD)
  mnemonic: ""                          # [NEW] provisions the key at startup if absent (env: KATANA_GNOKEY_MNEMONIC);
                                         # leave empty when the keybase is already pre-provisioned (production)
  gas_fee: "1000000ugnot"
  gas_wanted: 20000000

conditions:
  require_all: false
  missed_blocks:
    enabled: true
    max: 100
    interval: current_week        # CONDITION WINDOW (gnomonitoring period)
  critical_alerts:
    enabled: true
    max: 3
    interval: 168h                 # CONDITION WINDOW (rolling duration)

exceptions:
  - "g1protected..."

# [NEW] Proposal creation + lifecycle. katana is broadcast-only: every produced
# proposal is signed and submitted on-chain via gnokey (no dry-run/mode field).
proposal:
  cooldown: 720h                   # [NEW] min interval between two proposals for the same validator (D1)
  rejected_backoff: 720h           # [NEW] wait after a GovDAO rejection before re-proposing (D11, per-chain overridable)
  max_rejections: 2                # [NEW] stop after N GovDAO rejections; 0 = unlimited (D12, per-chain overridable)
  realm_pkg_path: "gno.land/r/gnoland/valopers_proposal"
  function: "ProposeRemoveValidator"
  args: ["{address}"]
  title_template: "Eject validator {moniker} ({address}) from the valset"
  description_template: |
    Automated ejection proposal generated by katana.
    Validator: {moniker} ({address})
    Reason: {reason}

# [NEW] Vote tracking — follows the GovDAO outcome of broadcast proposals.
vote_tracking:
  vote_poll_interval: 5m           # how often to poll GovDAO for pending proposals' status
  govdao_realm: "gno.land/r/gov/dao"   # realm that emits ProposalCreated + exposes status
  status_source: rpc               # rpc (vm/qrender, default) | graphql (indexer fallback) (D14)

# [NEW] Notifications — sent only after a SUCCESSFUL on-chain broadcast (D2).
notifications:
  enabled: true
  discord:
    enabled: false
    webhook_url: ""                # env: KATANA_DISCORD_WEBHOOK
  slack:
    enabled: false
    webhook_url: ""                # env: KATANA_SLACK_WEBHOOK
  telegram:
    enabled: false
    bot_token: ""                  # env: KATANA_TELEGRAM_TOKEN
    chat_id: ""                    # env: KATANA_TELEGRAM_CHAT_ID

storage:
  driver: sqlite
  sqlite_path: "./data/katana.db"

web:                               # reserved, not served yet
  enabled: false
  listen: ":8990"

chains:
  test6:
    rpc_endpoint: "https://rpc.test6.testnets.gno.land"
    gnoweb_url: "https://test6.testnets.gno.land"   # [NEW] for proposal links in notifs (D8)
    chain_id: "test6"
    gnomonitoring_chain: "test6"
    enabled: true
    # poll_interval: 10m           # [NEW] optional per-chain override (D7)
  betanet:
    rpc_endpoint: "https://rpc.betanet.testnets.gno.land"
    gnoweb_url: "https://betanet.testnets.gno.land"
    chain_id: "betanet"
    gnomonitoring_chain: "gnoland1"
    enabled: true
    poll_interval: 2m              # [NEW] per-chain cadence override (D7)
    # [NEW] optional per-chain wallet override (decision D10).
    # Any field omitted falls back to the global gnokey block.
    gnokey:
      key_name: "katana-betanet"
      address: "g1betanetproposer..."
      password: ""                 # env: KATANA_GNOKEY_PASSWORD_BETANET
      gas_fee: "1000000ugnot"
  mainnet:
    rpc_endpoint: "https://rpc.gno.land"
    gnoweb_url: "https://gno.land"
    chain_id: "gno.land"
    gnomonitoring_chain: "mainnet"
    enabled: false
    poll_interval: 10m
```

> **Two different "interval" notions — do not confuse:**
> - `poll_interval` = **how often katana runs** a cycle (the daemon cadence).
> - `conditions.*.interval` = **the time window over which a metric is counted**
>   to decide eligibility (e.g. "3 CRITICAL alerts over the last 168h").

## 8. Data model (SQLite) `[EXISTS + extend]`

- `evaluations(id, created_at, chain, address, moniker, missed_blocks,
  critical_alerts, eligible, excepted, reason)` — one row per candidate per cycle.
- `proposals(id, created_at, chain, chain_id, address, moniker, reason,
  title, realm_path, function, args, output, success,
  onchain_proposal_id, status, resolved_at)` — one row per produced (broadcast)
  proposal. `[EXTEND]` `onchain_proposal_id` (GovDAO id, captured after every
  successful broadcast), `status`
  (`pending` | `accepted` | `rejected`), `resolved_at`. Source of truth for the
  lifecycle gate (cooldown, rejection backoff, rejection count) per
  `(chain, address)`.
- `[TO BUILD]` a `notifications` table (id, created_at, chain, address, moniker,
  channel, target, event, status, error, proposal_id) for auditability.
  `event` ∈ `sent` | `accepted` | `rejected`. *(decision D5 — confirmed.)*

## 9. Operational requirements

- **OR-1** Container stays up (`restart: unless-stopped`); the process itself is
  the loop, so `KATANA_INTERVAL`/entrypoint sleep loop is **removed**. `[CHANGE]`
- **OR-2** `data/` (SQLite + proposals) persists in a volume.
- **OR-3** Structured, leveled logs (`log_level`).
- **OR-4** Expose a `/health` endpoint on `web.listen` reporting liveness, so an
  external monitor (Grafana) can alert if the service goes down. `/metrics` is
  **not** required for now. *(decision D6 — confirmed: health only.)* `[TO BUILD]`

## 10. Security & safety

- katana is **broadcast-only**: there is no dry-run/preview mode, so every
  eligible validator that clears the lifecycle gate and valset check is
  actually proposed on-chain.
- No key material stored by katana; gnokey owns signing. The signing key
  itself lives in the gnokey keybase — either provisioned from a mnemonic
  katana passes to `gnokey add` at startup, or already present in a
  pre-provisioned keybase in production (katana never keeps the mnemonic or
  private key beyond that one-time import).
- `config.yaml`, `data/`, `*.db` are git-ignored; secrets overridable via env.
- Valset safety check before every proposal.
- Cooldown prevents accidental proposal spam in a continuous loop.

## 11. Observability

- Per-cycle summary log per chain: candidates evaluated, eligible, excepted,
  suppressed-by-cooldown, proposals produced, notifications sent.
- Every evaluation and proposal persisted for audit.

## 12. Testing & QA `[TO BUILD]`

**Principle:** every package ships with unit tests; no feature (existing or new)
is "done" without them. The current code has **zero tests** — this is a gap to
backfill. All tests must run **fully offline**: no live gnomonitoring, gno.land
RPC, GovDAO, gnokey, or network access.

### 12.1 Strategy

- **Table-driven** unit tests for pure logic.
- All external I/O sits behind interfaces and is **faked** in tests:
  - HTTP (gnomonitoring, RPC `/validators`, GovDAO `vm/qrender`, notification
    webhooks) via `net/http/httptest` servers returning canned payloads.
  - SQLite via a temp-file / `:memory:` database.
  - gnokey via an injectable **command runner** (fake), never a real binary.
- **Deterministic time:** inject a clock (`now func() time.Time`) so cooldown,
  rejection backoff and alert windows are testable without sleeping.
- Run with `go test ./... -race`, plus `go vet ./...`, in CI.
- **Coverage target:** ≥ 80% on logic packages (config, rules, lifecycle,
  proposal, store); best-effort on thin I/O adapters.

### 12.2 Per-package test matrix (TR)

- **TR-config** — load / defaults / validate; env-override precedence; per-chain
  overrides (conditions, proposal, gnokey, poll_interval); exceptions union
  + address/moniker matching; rejects invalid values (bad `storage.driver`, bad
  durations, missing required fields on an enabled chain, unknown `default`).
- **TR-rules** — each condition on/off; strict `>` boundary cases; `require_all`
  AND vs OR; zero enabled conditions → not eligible; reason strings.
- **TR-lifecycle** — state machine `none→pending→accepted`,
  `→rejected→backoff→none`; `cooldown` floor; `max_rejections` cap; a `pending`
  proposal blocks re-proposal; **UNKNOWN status stays `pending`, never
  `rejected`** (FR-VT.2b).
- **TR-gnomonitoring** — parse `missed_block`; group CRITICAL alerts per address;
  bearer-token header sent; non-200 → error; `from`/period formatting.
- **TR-gnoland** — parse `/validators`, pagination, `IsValidator`
  case-insensitive; RPC error handling.
- **TR-votetrack** — extract proposal id from `ProposalCreated` event attrs; map
  `vm/qrender` output to `accepted` / `pending` / `rejected` incl. unknown.
- **TR-proposal** — template placeholder rendering; gnokey argument-vector
  assembly; `Broadcast` via fake runner (success, failure, password on stdin) +
  on-chain id capture.
- **TR-notifications** — English message formatting per event (sent / accepted /
  rejected); per-channel dispatch (Discord / Slack / Telegram) via httptest;
  best-effort (a failure never propagates); emitted only on successful broadcast.
- **TR-store** — migrations idempotent; record evaluations / proposals /
  notifications; lifecycle queries (latest proposal + rejection count per
  `(chain, address)`).
- **TR-daemon** — a single worker cycle end-to-end with all deps faked; graceful
  shutdown stops tickers; a failing cycle does not crash the service; per-chain
  isolation (one chain's failure does not affect others).
- **TR-health** — `/health` returns 200 while the service is alive.

### 12.3 Test infrastructure to introduce

- A `Clock` seam (`now()`), injected everywhere time drives behaviour.
- A `CommandRunner` seam for gnokey (real `exec` vs fake in tests).
- HTTP test servers / fixtures for each external API.
- The `store.Store` interface already exists and enables faking persistence.

### 12.4 Layering

- **Unit tests** (§12.1–12.3) — the default; offline, fast, run on every change.
- **Preprod / end-to-end** (§12.5) — an opt-in suite run against a local gno.land
  devnet, kept out of the default `go test ./...` run (build tag / env guard) so
  CI stays hermetic.

### 12.5 Preprod / end-to-end validation `[TO BUILD]`

The final validation runs katana against a **real local gno.land devnet**,
reusing gnomonitoring's ready-made 3-validator devnet at
`gnomonitoring/gnoland-test/` (Docker; validators + tx-indexer + gnoweb, GovDAO
v3 with a funded `dev` account that is the sole T1 member and can reach the
supermajority alone). This is where katana is exercised as it will run in prod.

- **PP-1 Environment.** Bring up the devnet (`bootstrap.sh` with `GNO_REPO_PATH`,
  then `docker compose up -d`) plus a gnomonitoring backend pointed at the devnet
  as `chain=local` (RPC `:26658`, indexer `:8546`, gnoweb `:8890`). Endpoints and
  the `dev` proposing key (passphrase `devpassword123`, addresses in
  `.devaddrs.mk`, regenerated per setup) come from gnoland-test.
- **PP-2 Seed a bad validator.** Use the devnet scenarios to create the condition
  katana reacts to: onboard `validator4` via GovDAO (`make scenario1`), then stop
  it (`make scenario2`) so gnomonitoring records missed blocks / CRITICAL alerts
  for it under `chain=local`.
- **PP-3 katana config.** A `local` chain: `rpc_endpoint: http://localhost:26658`,
  `chain_id: dev`, `gnomonitoring_chain: local`, `gnoweb_url: http://localhost:8890`,
  wallet = the `dev` gnokey account (katana always broadcasts, so no run-mode
  setting is needed), conditions tuned so `validator4` becomes eligible quickly.
- **PP-4 Assert the full flow.** katana detects `validator4` eligible → broadcasts
  the ejection proposal → captures the `ProposalCreated` id → the vote-watcher
  follows it. Then, driving the `dev` GovDAO vote:
  - vote **YES** → proposal accepted → validator ejected from the valset →
    *accepted* notification; state → `accepted`.
  - vote **NO** (or let it expire) → rejected → *rejected* notification; state →
    `rejected`; re-proposal suppressed until `rejected_backoff`; stop after
    `max_rejections`.
- **PP-5 Ejection mechanism — RESOLVED against gno.land source.** Confirmed by
  reading `examples/gno.land/r/sys/validators/v3/proposal.gno`:
  - Ejection = a `ValoperChange` with **Power=0** applied via
    `r/sys/validators/v3.NewValidatorProposalRequest(changes, title, desc)` then
    `dao.MustCreateProposal(...)`.
  - That v3 builder is **NON-CROSSING**: *"Direct MsgCall is unsupported;
    proposers route through a crossing script."* So katana must broadcast a
    **`gnokey maketx run` script** (MsgRun), **not** a `maketx call` (MsgCall).
    → katana gained a `proposal.submit: run|call` mode; `run` (default) uses the
    built-in `DefaultEjectScript`.
  - **Confirmed end-to-end on the gnoland-test devnet (2026-07-07).** A katana
    `run`-mode proposal (GovDAO #2, "Eject validator samourai-crew-4") was driven
    to acceptance via `make accept-govdao ID=2`; execution emitted
    `ValidatorRemoved` (op `g1ps3…`, signingAddr `g1g646…`) from
    `r/sys/validators/v3`, and RPC `/validators` dropped from 4 to 3 validators
    (the target removed). The run-mode entrypoint is now pinned/confirmed.
  - It needs the validator's **operator address**, whereas gnomonitoring keys
    metrics by the **signing/consensus address**. **Remaining gap:** katana must
    resolve signing → operator via the valopers realm (as gnomonitoring's
    `valoper.go` does) before the broadcast e2e is fully automatic.
    `Target.OperatorAddress` currently falls back to the metric address.
- **PP-6 Isolation.** The e2e suite is opt-in (build tag `e2e` or an env guard)
  and never part of the hermetic unit run.

## 13. Gap analysis — what changes vs. today

| Area | Today | Target |
|------|-------|--------|
| Execution | One-shot CLI; loop delegated to Docker `KATANA_INTERVAL`/cron | Native daemon, per-chain workers, `poll_interval` in config |
| Run mode | `--broadcast` CLI flag (opt-in) | Always broadcast, no flag/mode — the daemon signs & submits every eligible proposal |
| Notifications | None | Discord / Telegram / Slack on **sent / accepted / rejected** |
| Dedup | None (would re-propose each cycle) | Lifecycle state machine (pending/accepted/rejected + cooldown/backoff) |
| Vote tracking | None | Vote-watcher worker follows GovDAO outcome per proposal |
| Docker | Entrypoint sleep loop | Plain long-running process, `restart: unless-stopped` |
| Wallet | Single global gnokey | Global + **per-chain override** |
| Chain discovery | None | Validate config against gnomonitoring `/info` |
| Health | None | `/health` endpoint for Grafana |
| Tests | None (zero) | Unit tests for every package, offline, ≥80% on logic |
| Preprod / e2e | None | Opt-in e2e against gnoland-test local devnet |
| Chain enable | `enabled:` per chain | Unchanged |
| Conditions / windows | In config, per chain | Unchanged |
| Metrics source | gnomonitoring API | Unchanged |
| Proposal build/broadcast | gnokey, dry-run or broadcast | gnokey, **broadcast-only**; gnokey embedded in the image (`GNO_REF`), key provisioned from a mnemonic at startup |
| Storage | SQLite history | Unchanged (+ used for cooldown, + notifications table) |

## 14. Decisions

### Resolved

- **D2 — Notification trigger.** ✅ Notify **only** after a proposal is
  successfully broadcast on-chain (no dry-run, no error notifications). (FR-8.1)
- **D3 — Message content.** ✅ English, includes moniker + address + chain, e.g.
  *"A proposal for validator `<moniker>` (`<address>`) has been sent to remove it
  from the valset."* Optional gnoweb link. (FR-8.3)
- **D4 — `--once` mode.** ✅ **Removed.** katana runs permanently as a backend.
  (FR-1.6)
- **D5 — `notifications` audit table.** ✅ Yes, keep a history. (FR-9.3)
- **D6 — Health endpoint.** ✅ `/health` only (no `/metrics`), for Grafana
  liveness alerting. (OR-4)
- **D1 — Cooldown.** ✅ Configurable (`proposal.cooldown`), default `720h` (30d).
- **D7 — Per-chain `poll_interval`.** ✅ Global default, overridable per chain.
- **D8 — Chain discovery.** ✅ RPC + gnoweb URLs live in `chains.*` in config;
  `/info` is used only to **validate** them at startup (warn on mismatch), not to
  populate. (`gnoweb_url` added per chain.)
- **D9 — Run mode.** ✅ **Superseded: katana is broadcast-only.** There is no
  `proposal.mode` / dry-run setting; every eligible proposal is signed and
  submitted on-chain via gnokey. The signing key is either provisioned from a
  configured mnemonic at startup (`internal/keybase`) or already present in a
  pre-provisioned keybase in production.
  *Amended: a per-chain `dry_run` rehearsal switch was later reintroduced for
  onboarding katana on live networks (see README "Rehearsal / dry-run");
  broadcast remains the default.*
- **D10 — Per-chain wallet.** ✅ Global `gnokey` + optional per-chain override.

### Resolved — GovDAO vote tracking

All decisions are now settled; the PRD is ready to implement.

- **D11 — Rejection backoff.** ✅ Configurable `proposal.rejected_backoff`,
  default `720h` (30d), per-chain overridable. **Must be documented in README +
  `config.yaml.example`.**
- **D12 — Rejection cap.** ✅ Configurable `proposal.max_rejections`, per-chain
  overridable, **default `2`** (stop after 2 rejections); `0` = unlimited.
- **D13 — Proposal-ID mapping.** ✅ Resolved via the `ProposalCreated` event
  (`id` attr) emitted by `gno.land/r/gov/dao`, read from the broadcast tx events;
  indexer fallback by tx hash. (Confirmed by gnomonitoring `ExtractGovDAOIDs`.)
- **D14 — Vote status source.** ✅ RPC `vm/qrender` on `gno.land/r/gov/dao:<id>`
  (mirrors gnomonitoring `ExtractProposalRender`); GraphQL indexer kept as
  optional fallback. Note: render parsing is realm-version dependent and must
  keep unknown states as `pending` (FR-VT.2b).

## 15. Milestones (once validated)

1. **M1 — Daemon core:** per-chain workers, `poll_interval`, graceful shutdown;
   remove Docker sleep loop; katana broadcasts every proposal (no run-mode
   setting).
2. **M2 — Cooldown:** dedup from SQLite history.
3. **M3 — Vote tracking + lifecycle:** capture on-chain proposal id, vote-watcher
   worker, lifecycle state machine (pending/accepted/rejected, cooldown +
   rejection backoff), extended `proposals` table.
4. **M4 — Notifications:** Discord + Slack + Telegram senders for sent / accepted
   / rejected events, config + env overrides, best-effort dispatch,
   `notifications` table.
5. **M5 — Ops polish:** `/health` endpoint, chain discovery/validation (`/info`),
   per-chain wallet, richer per-cycle logging, docs (README + CLAUDE.md) update.
6. **M6 — Preprod / e2e:** opt-in end-to-end suite against the `gnoland-test`
   devnet (§12.5); confirm and pin the ejection realm/function/args (PP-5); full
   detect → broadcast → vote → accepted/rejected flow.

> **Tests are not a separate milestone.** Per §12, each milestone ships with the
> unit tests for the code it introduces, and M1 additionally backfills tests for
> the already-existing packages (config, rules, proposal, store, clients). The
> end-to-end suite (§12.5) is delivered in M6.

---

### Appendix A — Glossary

- **Candidate** — a validator observed with any missed-block or alert metric in a
  cycle; the set evaluated against conditions.
- **Condition window (`interval`)** — the period over which a metric is counted.
- **Poll interval (`poll_interval`)** — how often a chain worker runs a cycle.
- **Cooldown** — minimum time before re-proposing the same validator.
- **Rejection backoff** — time to wait after a GovDAO rejection before
  re-proposing the same validator.
- **Lifecycle state** — per-`(chain, validator)` status: none / pending /
  accepted / rejected, driving whether a new proposal may be created.
- **Vote watcher** — the worker that polls GovDAO for the outcome of katana's
  broadcast proposals.
- **Broadcast** — sign and submit the proposal on-chain via gnokey; katana's
  only mode of operation (no dry-run/preview mode exists).
- **Keybase provisioning** — importing the signing key derived from a
  configured mnemonic into the gnokey keybase at startup (`internal/keybase`);
  idempotent, and a no-op when the keybase is already pre-provisioned.

### Appendix B — References

- **gnomonitoring** (metrics source): <https://github.com/samouraiworld/gnomonitoring>
- gnomonitoring endpoints katana consumes:
  - `GET /missing_block?chain=&period=` — missed blocks per validator.
  - `GET /admin/alerts?chain=&level=CRITICAL&from=` — CRITICAL alerts (admin, needs token).
  - `GET /info` — **chain discovery**: returns `enabled_chains` and, per chain,
    `rpc_endpoints` / `graphqls` / `gnowebs`. Public, no token. **Confirmed
    available** in the current backend.
- GovDAO vote tracking reference impl (to mirror in katana):
  `backend/internal/govdao/govdao.go` — `ProposalCreated` event id extraction
  (`ExtractGovDAOIDs`, `ProcessProposal`) and status render
  (`ExtractProposalRender`, RPC `vm/qrender` on `gno.land/r/gov/dao:<id>`).
- **Local devnet for preprod/e2e** (§12.5): `gnomonitoring/gnoland-test/` —
  3-validator Docker gno.land devnet + tx-indexer + gnoweb, GovDAO v3 with a
  funded `dev` proposer key. RPC `:26658`, indexer `:8546/graphql/query`, gnoweb
  `:8890`. Scenarios (`make scenario1/2`) onboard/down a validator; addresses in
  `.devaddrs.mk`. Ejection realm to confirm against
  `gno.land/r/gnops/valopers/proposal`.
- **gnokey** (signing/broadcast): <https://docs.gno.land>
- gno.land RPC endpoint katana consumes:
  - `GET /validators` — current consensus valset (safety check).
