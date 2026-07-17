# katana

**katana** is a continuously running backend that watches
[gno.land](https://gno.land) validators and, for any that fail configurable
health conditions, produces a GovDAO proposal to eject it from the valset — then
follows the vote outcome and notifies on the result.

It reads validator health from a running
[gnomonitoring](https://github.com/samouraiworld/gnomonitoring) instance, is
multi-chain (testnet, betanet, mainnet, …), and is driven entirely by a single
`config.yaml`.

> See [prd.md](prd.md) for the full product specification.

---

## How it works

katana is a **daemon**. For every enabled chain it runs two workers on
independent tickers:

```
                         ┌──────────── katana daemon ────────────┐
 gnomonitoring API ──────┤ eval-worker(chain)  every poll_interval │
   /missing_block        │   evaluate → gate → proposal → notify   │
   /admin/alerts         │                                          │
 gno.land RPC ───────────┤ vote-worker(chain)  every vote_poll_...  │
   /validators, GovDAO   │   poll GovDAO status → accepted/rejected │
                         └──────────────────────────────────────────┘
        SIGINT/SIGTERM → graceful shutdown
```

**Evaluation worker** (every `poll_interval`):
1. Pull missed blocks and CRITICAL alert counts per validator from gnomonitoring.
2. Evaluate each against the configured conditions.
3. Skip exceptions and validators blocked by the lifecycle gate (pending /
   accepted / cooldown / rejection backoff).
4. Confirm the target is in the live consensus valset.
5. Sign & broadcast via `gnokey`, capture the on-chain proposal id, and send a
   notification — or, on a `dry_run: true` chain, log + notify the would-be
   proposal without broadcasting (see [Rehearsal / dry-run](#rehearsal--dry-run)).

**Vote worker** (every `vote_poll_interval`): polls the GovDAO status of each
pending proposal katana broadcast, and on resolution notifies + updates state:
- **accepted** → validator removed from the valset;
- **rejected** → validator kept; re-proposal suppressed for `rejected_backoff`,
  and stopped entirely after `max_rejections`.

## Ejection conditions

A validator becomes eligible when the **enabled** conditions hold, combined with
`require_all` (`true` = AND, `false` = OR):

| Condition        | Triggers when …                                                     |
|------------------|---------------------------------------------------------------------|
| `missed_blocks`  | missed blocks over `interval` is strictly greater than `max`        |
| `critical_alerts`| CRITICAL alerts over `interval` is strictly greater than `max`      |

- `missed_blocks.interval` — gnomonitoring period: `current_week`,
  `current_month`, `current_year`, `all_time`.
- `critical_alerts.interval` — Go duration: `24h`, `168h` (7d), …

> **Two different "interval" notions.** `poll_interval` is how often katana
> *runs*; `conditions.*.interval` is the *window over which a metric is counted*.

All thresholds, windows, wallet and timers are configurable in
`config.yaml` and overridable per chain.

## Exceptions

Validators under `exceptions:` are **never** proposed, matched by address
(recommended) or moniker. The global list is unioned with each chain's own list.

## Lifecycle & anti-spam

Because katana runs continuously, a per-`(chain, validator)` state machine
(backed by SQLite) prevents duplicate proposals:

- a **pending** proposal blocks new ones until the vote resolves;
- **accepted** is terminal (the validator is gone);
- **rejected** suppresses re-proposal for `proposal.rejected_backoff`, and stops
  after `proposal.max_rejections` (default `2`);
- `proposal.cooldown` (default `720h` = 30d) is a floor between any two proposals
  for the same validator.

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

## Notifications

On a **successful broadcast**, and on **accepted** / **rejected** outcomes,
katana notifies every enabled channel (Discord, Slack, Telegram). Example:

> A proposal for validator `Sam` (`g1abc…`) on `betanet` has been sent to remove
> it from the valset.

Nothing is sent on errors.

## Requirements

- Go 1.23+
- A reachable gnomonitoring backend.
- The `gnokey` binary and a keybase (or mnemonic) holding the proposing key —
  katana always signs and broadcasts.

## Build & run

```sh
git clone https://github.com/samouraiworld/katana
cd katana
make build
cp config.yaml.example config.yaml   # then edit it
./katana --config config.yaml        # runs until Ctrl-C / SIGTERM
```

The only flag is `--config`. Chains, wallet, notifications and timers all live
in `config.yaml`.

### Docker

```sh
docker compose up --build -d     # long-running; restart: unless-stopped
docker compose logs -f katana
```

The SQLite database persists in the `katana-data` volume; point
`storage.sqlite_path` under `/app/data`.

## Configuration

Everything is in `config.yaml` (see [`config.yaml.example`](config.yaml.example)).
Secrets/per-deployment values can be overridden from the environment:

| Env var                    | Overrides                       |
|----------------------------|---------------------------------|
| `KATANA_LOG_LEVEL`         | `log_level`                     |
| `KATANA_GNOMON_BASE_URL`   | `gnomonitoring.base_url`        |
| `KATANA_GNOMON_API_TOKEN`  | `gnomonitoring.api_token`       |
| `KATANA_GNOKEY_BIN`        | `gnokey.bin`                    |
| `KATANA_GNOKEY_KEY`        | `gnokey.key_name`               |
| `KATANA_GNOKEY_ADDRESS`    | `gnokey.address`                |
| `KATANA_GNOKEY_HOME`       | `gnokey.home`                   |
| `KATANA_GNOKEY_PASSWORD`   | `gnokey.password`               |
| `KATANA_GNOKEY_MNEMONIC`   | `gnokey.mnemonic`               |
| `KATANA_DISCORD_WEBHOOK`   | `notifications.discord.webhook_url` |
| `KATANA_SLACK_WEBHOOK`     | `notifications.slack.webhook_url`   |
| `KATANA_TELEGRAM_TOKEN`    | `notifications.telegram.bot_token`  |
| `KATANA_TELEGRAM_CHAT_ID`  | `notifications.telegram.chat_id`    |

> **Ejection realm.** `proposal.realm_pkg_path` / `function` / `args` are
> configurable because the valset-ejection entrypoint differs between networks.
> Verify them against each chain before broadcasting — the local devnet
> (below) is the place to confirm them.

## Health

Enable `web:` in `config.yaml` to expose `GET /health` (200 = alive) for
liveness monitoring / Grafana alerting.

## Testing

```sh
make test            # unit tests (offline, no chain/network)
go test ./... -race
```

An opt-in end-to-end / preprod suite runs katana against a local gno.land devnet
under [`gnoland-test/`](gnoland-test/) (copied and adapted from gnomonitoring):
3 validators + tx-indexer + gnoweb + GovDAO, with scenarios to onboard/down a
validator so katana can be exercised through the full detect → broadcast → vote
flow. See [`gnoland-test/README.md`](gnoland-test/README.md).

For a wired-up stack (Postgres + gnomonitoring + katana) against that devnet, use
[`docker-compose.e2e.yml`](docker-compose.e2e.yml) — see
[`deploy/e2e/README.md`](deploy/e2e/README.md).

> **Data volumes are broadcast-only.** The SQLite schema no longer has the old
> dry-run/artifact columns; a `katana-data` (or `e2e-katana-data`) volume
> created before this refactor must be wiped, not reused.

## Project layout

```
cmd/katana/            daemon entrypoint (signals, config, store, run)
internal/config/       config.yaml loading, validation, per-chain resolution
internal/daemon/       per-chain eval + vote workers, health server
internal/gnomonitoring/ REST client for validator metrics
internal/gnoland/      gno.land RPC (consensus valset safety check)
internal/govdao/       GovDAO proposal id extraction + status render
internal/rules/        eligibility evaluation
internal/lifecycle/    proposal state-machine gate (cooldown/backoff/cap)
internal/proposal/     proposal rendering + gnokey broadcast
internal/notify/       Discord / Slack / Telegram senders
internal/store/        SQLite history (evaluations, proposals, notifications)
gnoland-test/          local devnet for preprod/e2e
```

## Safety

- katana broadcasts by default: every eligible validator on a chain without
  `dry_run: true` results in a signed, on-chain GovDAO proposal — rehearse
  with `dry_run` before arming a new chain. `gnokey` is embedded in the container image
  (pinned by the `GNO_REF` build-arg); the signing key is provisioned from a
  mnemonic at startup (`gnokey.mnemonic` / `KATANA_GNOKEY_MNEMONIC`) or from a
  pre-provisioned keybase.
- The lifecycle gate prevents proposal spam in the continuous loop.
- katana verifies valset membership before proposing.
- No key material is stored by katana outside the keybase; signing is
  delegated to `gnokey`.
- `config.yaml`, `data/` and `*.db` are git-ignored.

## License

See [LICENSE](LICENSE).
