# Local Gno.land devnet

A 3-validator Gno.land devnet, run entirely with Docker, used to exercise every
gnomonitoring feature (block participation tracking, alerts, GovDAO watcher,
Prometheus metrics, Telegram bots) end-to-end before merging changes to
production — in particular for validating the
[feat/migrate-sqlite-to-postgres](../) branch.

It ships:

- **validator / validator2 / validator3** — genesis validators
  (`samourai-crew-1/2/3`, voting power 10/10/10)
- **validator4** — a 4th identity (`samourai-crew-4`) reserved for the
  Phase 2 "new validator via GovDAO" scenario, not part of the genesis
  validator set, started on demand via the `phase2` compose profile
- **tx-indexer** — GraphQL transaction/block indexer
  (`http://localhost:8546/graphql/query`)
- **gnoweb** — block explorer UI (`http://localhost:8890`)

## Target: gno.land mainnet

The devnet is built from the gno.land **mainnet** tag, `chain/mainnet`
(gnolang/gno commit `9c8eb132e483`, v1.2.0) — the code katana targets in
production. The realms it relies on only exist under these paths there:

| Realm | mainnet tag | (test13, for reference) |
|---|---|---|
| GovDAO proxy | `gno.land/r/gov/dao` | same |
| GovDAO implementation | `gno.land/r/gov/dao/impl/v0` | `r/gov/dao/v3/impl` |
| GovDAO bootstrap | `gno.land/r/gov/dao/init/v0` | `r/gov/dao/v3/init` |
| GovDAO members | `gno.land/r/gov/dao/memberstore/v0` | `r/gov/dao/v3/memberstore` |
| Valset changes | `gno.land/r/sys/validators/v0` | `r/sys/validators/v3` |
| Valoper profiles | `gno.land/r/gnops/valopers` (+ `/proposal`) | same |

A plain upstream checkout of the tag is enough (the earlier devnet was built
from a fork, `gno-validator-tools/gno@2c817cec4`; nothing in bootstrap depends
on it any more). Differences that matter on this devnet:

- `valopers.Register` takes no fee: the fee is a sysparam that defaults to 0,
  and sending coins anyway fails the tx (`coins were sent but the called
  function never read them`). The targets below no longer pass `-send`.
- Valset changes go through `r/sys/params` (`valset:proposed`), applied by the
  chain at end-block: a change shows up on RPC `/validators` a block or two
  after the executing tx.
- Use a `gnokey` built from the same tag (the scenario targets call `gnokey`
  from `PATH`):

  ```bash
  cd /path/to/gno-mainnet && go build -o /some/bin/gnokey ./gno.land/cmd/gnokey
  export PATH=/some/bin:$PATH
  ```

## Prerequisites

- Docker + Docker Compose
- A checkout of [gno](https://github.com/gnolang/gno) at the `chain/mainnet`
  tag (used to build the `gnoland`/`gnogenesis`/`gnokey`/`gnoweb` images and
  to deploy the `examples/gno.land` package tree into genesis), e.g.
  `git -C /path/to/gno worktree add --detach ../gno-mainnet chain/mainnet`
- A `gnokey` built from that checkout, first on `PATH` (see above)

## One-time setup

```bash
GNO_REPO_PATH=/path/to/gno-mainnet ./bootstrap.sh
```

This builds the `gno-validator:mainnet`, `gno-contribs:mainnet`,
`gno-gnokey:mainnet` and `gno-gnoweb:mainnet` images, generates node secrets and P2P identities for all
4 nodes, the `dev`/`v4op` gnokey accounts, the shared `config.toml`, the `.env`
file (P2P seeds), the rendered `init-govdao.gno`/`propose-validator4.gno`
scripts, and `genesis.json` (3 validators + the full `examples/gno.land`
package tree + balances + `auth.unrestricted_addrs`).

**Nothing generated is committed to git** — keys, genesis, `.env`, the
`.gnokey-dev/` keybase, `.devaddrs.mk` and the rendered `*.gno` are all
gitignored. Every fresh setup regenerates them, so the validator / `dev` /
`v4op` **addresses change between setups**. That's intentional and safe: the
genesis, `.env`, the rendered GovDAO scripts and `.devaddrs.mk` are all derived
from the same freshly generated keys, so they always stay consistent.

The script is **idempotent**: re-running it skips any step whose output already
exists. To wipe everything and regenerate from scratch (new keys + addresses),
run `make clean-all` then `make full-reinit`.

## Running the devnet

```bash
docker compose up -d        # start validator, validator2, validator3, tx-indexer, gnoweb
docker compose down          # stop everything (keeps chain state)
```

To also start the 4th identity for the Phase 2 GovDAO scenario:

```bash
docker compose --profile phase2 up -d validator4
```

Useful endpoints once running:

| Service            | URL                                            |
|--------------------|------------------------------------------------|
| validator RPC      | `http://localhost:26658`                       |
| validator2 RPC     | `http://localhost:26659`                       |
| validator3 RPC     | `http://localhost:26660`                       |
| validator4 RPC     | `http://localhost:26661` (phase2 profile only) |
| tx-indexer GraphQL | `http://localhost:8546/graphql/query`          |
| gnoweb             | `http://localhost:8890`                        |

Check progress:

```bash
curl -s http://localhost:26658/status | jq '.result.sync_info'
```

## Resetting the chain

Two levels of reset, both wrapped by the `Makefile`:

- **`make reinit`** — purges the `dev`-chain rows from the gnomonitoring DB,
  then runs `./reinit-chain.sh`: wipes each node's `config`, `db`, `wal` and
  `genesis.json` and resets `priv_validator_state.json`, while **keeping** the
  node identities (`secrets/`) and the `dev`/`v4op` keybase — so all addresses
  stay the same. Fast; use it between test scenarios.
- **`make clean-all`** — nukes *everything* generated (keys, keybase, genesis,
  config, `.env`, `.devaddrs.mk`, rendered `*.gno`). The next `make full-reinit`
  regenerates fresh keys and **new addresses**.

```bash
make reinit        # keep identities, restart chain from existing genesis
# or, full regeneration:
make clean-all
make full-reinit   # bootstrap (new keys+genesis) + purge dev DB + start
```

**`full-reinit` alone does not bring up validator4.** It's `phase2`
profile-gated (see the service list above) and not a valoper until
`scenario1` registers + onboards it via GovDAO. Use **`make full-reinit-4`**
(= `full-reinit` + `scenario1`) to get a fresh chain with all 4 validators
live and in the valset in one command.

> **⚠️ Restart the backend after any reset.** `gnomonitoring-backend` keeps the
> last-seen block height per chain in memory. When you reset the chain under a
> running backend, it waits for the new chain to climb back past the old height
> before ingesting — so `daily_participations` for `dev` stays empty until you
> restart it:
>
> ```bash
> docker restart gnomonitoring-backend
> ```
>
> This also reconnects the **GovDAO websocket**, which is what fires "New
> Proposal" alerts (see the GovDAO note under Scenario 1).

## Local dev accounts

`bootstrap.sh` generates two throwaway gnokey accounts into `.gnokey-dev/`
(gitignored, **regenerated with new addresses on every fresh setup**). Both are
funded in genesis; `dev` is also set as `auth.unrestricted_addrs` so it can
register valopers or submit GovDAO proposals without paying realm storage
deposits.

| Key    | Role                                     |
|--------|------------------------------------------|
| `dev`  | GovDAO member, proposal submitter        |
| `v4op` | validator4 operator (must sign Register) |

- **Passphrase** (both accounts): `devpassword123`
- The current addresses are written to `.devaddrs.mk` after each setup
  (`DEV_ADDR`, `VALIDATOR4_OP_ADDR`, `VALIDATOR4_ADDR`, `VALIDATOR4_PUB`) and
  can also be listed from the keybase:

  ```bash
  cat .devaddrs.mk
  gnokey list -home "$PWD/.gnokey-dev"
  ```

Use them with the local `gnokey` binary (a plain `docker run` of
`gno-gnokey:mainnet` can't reach `localhost:26658` unless the container joins the
devnet's docker network):

```bash
gnokey query bank/balances/$(awk '/DEV_ADDR/{print $3}' .devaddrs.mk) \
  -home "$PWD/.gnokey-dev" -remote http://localhost:26658
```

## Pointing gnomonitoring at the devnet

There are two ways to run gnomonitoring against this devnet, depending on
whether the backend itself runs on the host or in a container.

### Option A — gnomonitoring running on the host (`go run` / `go test`)

Copy the `chains.local` entry from
[`config.devnet.yaml.example`](config.devnet.yaml.example) into the `chains:`
section of `backend/config.yaml`, then either set `default_chain: "local"`
or query the API/Telegram bot with `?chain=local`. The devnet's RPC/GraphQL/
gnoweb ports are published on `127.0.0.1`, so `localhost:26658` etc. are
reachable directly from the host.

### Option B — dockerized gnomonitoring backend (`backend/docker-compose.yml`)

The `gnomonitoring-backend` container can't reach `127.0.0.1:<port>` on the
host, so it needs to join the devnet's Docker network and address the devnet
services by their compose service name instead of `localhost`.

1. **Start the devnet first** (so the `gnoland-test_default` network exists):

   ```bash
   cd gnoland-test && docker compose up -d
   ```

2. `backend/docker-compose.yml` already attaches the `detect-proposal`
   service to the external `gnoland-test_default` network (in addition to
   its own `default` network for Postgres).

3. Configure `backend/config_docker.yaml` (gitignored — copy from
   `backend/config.yaml.template` if it doesn't exist yet) with:

   ```yaml
   default_chain: "local"
   chains:
     local:
       rpc_endpoints:
         - "http://validator:26657"
       graphqls:
         - "http://tx-indexer:8546/graphql/query"
       gnowebs:
         - "http://gnoweb:8888"
       enabled: true
   ```

4. Start the backend:

   ```bash
   cd backend && docker compose up -d --build
   ```

5. Check it picked up the devnet:

   ```bash
   docker logs gnomonitoring-backend --tail=20
   curl -s http://localhost:8989/api/validators?chain=local | jq
   ```

**Troubleshooting:** if `gnomonitoring-backend` logs
`lookup postgres on 127.0.0.11:53: no such host`, the `gnomonitoring-postgres`
container was started without joining the compose network (e.g. left over
from a previous run). Fix with:

```bash
docker compose up -d --force-recreate postgres
docker restart gnomonitoring-backend
```

The `pgdata` volume is untouched, so no data is lost.

## Test scenarios

These scenarios exercise gnomonitoring end-to-end against this devnet (block
participation tracking, alerts, GovDAO watcher, Prometheus metrics). They
assume gnomonitoring is running against `chain=local` as described above and
the chain is healthy (see "Resetting the chain" if not).

Genesis validator set and voting power: samourai-crew-1/2/3 = 10/10/10 (total
30, `generate-genesis.sh` hardcodes `--power 10` for all three). A strict
majority above 2/3 of 30 is 20, so no 2 of the 3 genesis validators reach
supermajority without the third (see Scenario 3).

A `Makefile` in this directory wraps the chain-lifecycle and scenario commands
— run `make help` for the full list. Available scenarios:

| Target | What it exercises | How to assert |
|--------|-------------------|---------------|
| `make scenario1` | New validator via GovDAO (onboard validator4) | `v0.IsValidator` = true, `voting_power` 0→1 |
| `make scenario2` / `scenario2-restart` | Validator downtime: WARNING→CRITICAL→RESOLVED (stops validator4) | `alert_logs` rows for `chain='dev'`, `addr=VALIDATOR4_ADDR` |
| `make scenario3` / `scenario3-restart` | Chain halt → stagnation alert (stops validator2+3) | `alert_logs` `addr='all'`, log `Blockchain stuck`, height frozen |
| `make scenario4` / `scenario4-restart` | Total RPC outage (stops ALL nodes) → **log-only** alert | `grep 'Error when querying latest block height'` (no DB row) |
| `make scenario5` | GovDAO proposal voted **NO** (fresh chain) | live "New Proposal" alert + render shows `NO PERCENT: 100%` |
| `make scenario-mute` | Resend dedup (keeps validator4 down) | exactly one `alert_logs` row/level + `dedup: skipping` log |

`make scenario2` stops **validator4**, so run `make scenario1` first to put it
in the valset. The detailed walkthrough below documents what `make scenario1`
does under the hood.

> **GovDAO alerts (scenario1 / scenario5):** the "New Proposal" alert only fires
> for proposals seen **live over the websocket**, so the backend must already be
> watching `dev` when the proposal is created (reset → `docker restart
> gnomonitoring-backend` → create proposal). On a fresh devnet the alert is
> delivered via the **GovDAO Telegram bot** only (no `webhook_gov_daos` row for
> `dev`). For **scenario5**, voting NO does **not** reject the proposal
> immediately — it stays "open for votes" (`NO PERCENT: 100%`) until its voting
> deadline elapses. Note that gnomonitoring currently emits **no alert for a
> REJECTED/expired proposal and never updates `govdaos.status` to `REJECTED`**:
> `CheckProposalStatus` only handles the `ACCEPTED` transition (see issue #112).
> So scenario5's only observable is the "New Proposal" creation alert + the
> on-chain `NO PERCENT: 100%` render.

### Scenario 1 — New validator via GovDAO

Exercises GovDAO proposal detection/alerts (`internal/govdao`), moniker
resolution via `valopers.Render()`, and the appearance of a 4th validator in
`daily_participations` / `gnoland_chain_active_validators`.

> **TL;DR:** just run **`make scenario1`** — it performs every step below using
> the live addresses from `.devaddrs.mk`. The manual walkthrough is kept for
> reference. **All `g1...`/`gpub1...` values shown below are examples**: keys are
> regenerated on every setup, so use your own from `.devaddrs.mk` /
> `gnokey list -home "$PWD/.gnokey-dev"` (the `Makefile` does this automatically).

#### What you need

- `docker compose --profile phase2 up -d validator4` (or `make up-validator4`)
- The `dev` gnokey account (see "Local dev accounts" above) — funded in genesis
  and the only `auth.unrestricted_addrs` entry.
- The `v4op` gnokey account — validator4's operator key; `Register()` requires
  the caller to be the registered operator address (anti-squat guard).

Both accounts live in `.gnokey-dev/` with passphrase `devpassword123`. All
`gnokey` commands below use the **local `gnokey` binary** with
`-home "$PWD/.gnokey-dev"` (built from the mainnet tag, see "Target" above).
The dockerized `gno-gnokey:mainnet` image does *not*
work for these commands out of the box: a plain `docker run` (without
`--network`) can't reach `localhost:26658`, since the validator's RPC port is
only published on the host.

#### Steps

1. **Initialize GovDAO** (one-time on this devnet — genesis deploys the
   GovDAO v0 packages, and `r/gov/dao/loader/v0`'s automatic `init()` sets up
   empty T1/T2/T3 tiers with **0 members** and leaves `AllowedDAOs` empty, so
   no proposal could ever reach the 66.66% supermajority quorum until a
   member is seeded):

   `InitWithUsers(cur realm, addrs ...address)` takes a variadic `address`
   list, which `maketx call` (`MsgCall`) cannot pass, so it is called from a
   small `maketx run` (`MsgRun`) script. The import alias must *not* be
   `init` (reserved for the `func init()` builtin); `init-govdao.gno` (rendered by `bootstrap.sh` from
   `init-govdao.gno.tmpl`, with your live `dev` address injected) uses
   `govdaoinit` instead:

   ```go
   // init-govdao.gno (rendered from init-govdao.gno.tmpl; <DEV_ADDR> is your
   // live `dev` address, substituted by bootstrap.sh)
   package main

   import (
       govdaoinit "gno.land/r/gov/dao/init/v0"
   )

   func main(cur realm) {
       govdaoinit.InitWithUsers(cross(cur), address("<DEV_ADDR>"))
   }
   ```

   ```bash
   gnokey maketx run \
     -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
     -chainid dev -remote http://localhost:26658 \
     -home "$PWD/.gnokey-dev" dev init-govdao.gno
   ```

   This only works because the genesis `chain_id` is `"dev"`
   (`InitWithUsers` calls `assertIsDevChain()`). It resets the memberstore and
   makes `dev` the sole GovDAO "T1" member — any proposal `dev` votes YES on
   reaches the 66.66% supermajority alone — and locks `AllowedDAOs` to
   `gno.land/r/gov/dao/impl/v0` (which is how `make scenario1` detects an
   already-initialised chain).

2. **Get validator4's bech32 pubkey** from its secrets:

   ```bash
   docker run --rm --entrypoint gnoland \
     -v "$PWD/validator4/gnoland-data:/gnoroot/gnoland-data" \
     gno-validator:mainnet secrets get -data-dir /gnoroot/gnoland-data/secrets validator_key
   ```

   This returns validator4's address + pubkey (example — yours will differ;
   they are also written to `.devaddrs.mk` as `VALIDATOR4_ADDR` /
   `VALIDATOR4_PUB`):
   - address: `g132qwl9...`
   - pub_key: `gpub1pgg...`

3. **Register validator4 as a valoper** (no fee on the mainnet tag: the
   register fee sysparam defaults to 0 and a `-send` would fail the tx):

   ```bash
   gnokey maketx call \
     -pkgpath gno.land/r/gnops/valopers -func Register \
     -args "samourai-crew-4" \
     -args "Phase 2 test validator for gnomonitoring" \
     -args "on-prem" \
     -args "$(awk '/VALIDATOR4_OP_ADDR/{print $3}' .devaddrs.mk)" \
     -args "$(awk '/VALIDATOR4_PUB/{print $3}' .devaddrs.mk)" \
     -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
     -chainid dev -remote http://localhost:26658 \
     -home "$PWD/.gnokey-dev" v4op   # Register must be signed by the operator key
   ```

   (The 4th arg is the **operator** address `VALIDATOR4_OP_ADDR` = the `v4op`
   key, which must also be the signer; the 5th is validator4's BFT pubkey.)

4. **Create the GovDAO proposal** to add validator4 to the valset. This
   chains two realm calls (`proposal.NewValidatorProposalRequest` →
   `dao.MustCreateProposal`), so it needs `maketx run` with a small script:

   ```go
   // propose-validator4.gno (rendered from propose-validator4.gno.tmpl;
   // <V4OP_ADDR> is your live `v4op` operator address)
   package main

   import (
       "gno.land/r/gov/dao"
       "gno.land/r/gnops/valopers/proposal"
   )

   func main(cur realm) {
       pr := proposal.NewValidatorProposalRequest(cross(cur), address("<V4OP_ADDR>"))
       dao.MustCreateProposal(cross(cur), pr)
   }
   ```

   `propose-validator4.gno` is rendered by `bootstrap.sh` from
   `propose-validator4.gno.tmpl` (with your live `v4op` address), so:

   ```bash
   gnokey maketx run \
     -gas-fee 1000000ugnot -gas-wanted 100000000 -broadcast \
     -chainid dev -remote http://localhost:26658 \
     -home "$PWD/.gnokey-dev" dev propose-validator4.gno
   ```

   This creates proposal `#0` (first proposal on this devnet, assuming GovDAO
   was just reinitialized in step 1).

5. **Vote YES** (as the sole T1 member, 100% ≥ 66.66% supermajority):

   ```bash
   gnokey maketx call \
     -pkgpath gno.land/r/gov/dao -func MustVoteOnProposalSimple \
     -args 0 -args YES \
     -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
     -chainid dev -remote http://localhost:26658 \
     -home "$PWD/.gnokey-dev" dev
   ```

6. **Execute the proposal** — adds validator4 (power 1) through
   `r/sys/validators/v0` (emits `ValidatorAdded`; the chain applies the new
   valset from `r/sys/params` at end-block):

   ```bash
   gnokey maketx call \
     -pkgpath gno.land/r/gov/dao -func ExecuteProposal \
     -args 0 \
     -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
     -chainid dev -remote http://localhost:26658 \
     -home "$PWD/.gnokey-dev" dev
   ```

7. **Verify validator4 joined the valset and started signing**:

   ```bash
   gnokey query vm/qeval \
     -data "gno.land/r/sys/validators/v0.IsValidator(\"$(awk '/VALIDATOR4_ADDR/{print $3}' .devaddrs.mk)\")" \
     -remote http://localhost:26658
   curl -s http://localhost:26661/status | jq '.result.validator_info'
   ```

#### What to check in gnomonitoring

- a GovDAO "New Proposal" alert fired when the proposal was created (step 4) —
  `internal/govdao`
- `gnoland_chain_active_validators{chain="local"}` increments from 3 to 4
- validator4's address (`VALIDATOR4_ADDR` in `.devaddrs.mk`) appears in
  `daily_participations` for `chain="local"`
- the moniker for that address resolves to `samourai-crew-4` (from
  `valopers.Render()` via the MonikerMap refresh)

> **GovDAO alert delivery — important timing.** The "New Proposal" alert is only
> sent for proposals seen **live over the GovDAO websocket** (`who == "socket"`
> in `ProcessProposal`). A proposal created while the backend isn't live-watching
> `dev` (e.g. right after a chain reset, before you restarted the backend) is
> picked up by the startup catch-up scan → inserted into the `govdaos` table
> **without** an alert. So the correct order is: reset chain → **restart the
> backend** (websocket reconnects) → *then* create the proposal.
>
> Delivery channels: the alert goes to every `webhook_gov_daos` row (Discord/
> Slack) **and** to the GovDAO Telegram bot (`token_telegram_govdao`) for all
> subscribed chats. On a fresh devnet there is usually **no `webhook_gov_daos`
> row for `dev`**, so the alert only reaches Telegram — check the GovDAO bot, not
> a Discord/Slack channel. Either way the proposal is tracked in `govdaos`.

---

### Scenario 2 — Validator downtime / recovery alerts

Exercises the WARNING → CRITICAL → RESOLVED alert flow and webhook delivery
(`metric.go`, `alert_logs`).

This targets **validator4** (run `make scenario1` first so it's in the valset
with voting power 1). Its power is tiny, so stopping it lowers its own
participation rate **without halting the chain** — the clean way to trigger a
per-validator missed-block alert. (Stopping a genesis validator with power
10/11 instead risks halting consensus; that's Scenario 3.)

1. Stop validator4:

   ```bash
   make scenario2          # docker compose stop validator4
   ```

2. Wait until validator4 has missed ≥5 blocks → **WARNING**, then ≥30 →
   **CRITICAL**.

3. Restart it:

   ```bash
   make scenario2-restart  # docker compose start validator4
   ```

4. Once validator4 participates again at `end_height + 1`, expect a **RESOLVED**
   alert.

5. Check `alert_logs` for the WARNING/CRITICAL/RESOLVED rows:

   ```bash
   docker exec gnomonitoring-postgres psql -U gnomonitoring -d gnomonitoring \
     -c "SELECT level, start_height, end_height FROM alert_logs \
         WHERE chain_id='dev' AND addr='$(awk '/VALIDATOR4_ADDR/{print $3}' .devaddrs.mk)' ORDER BY id;"
   ```

---

### Scenario 3 — Halt the chain (insufficient voting power)

Exercises gnomonitoring's behavior when the chain stops producing blocks
entirely — the global stagnation/RPC-error alert path described in
`CLAUDE.md` under "Known Limitations".

With voting power 10/10/10 (total 30), more than 2/3 is 20. Stopping **any
2** of the 3 genesis validators leaves at most 10/30 ≈ 33%, well below the
66.67% supermajority CometBFT needs to commit blocks — the chain halts
deterministically:

```bash
docker compose stop validator2 validator3   # leaves only validator (power 10/31)
```

#### What to observe

- `gnoland_chain_current_height{chain="local"}` stops increasing
- the global stagnation/RPC-error alert fires
- no new `daily_participations` rows are inserted for any validator while
  halted

#### Recovery

```bash
make scenario3-restart   # docker compose start validator2 validator3
```

**Known issue:** restarting a stopped node can crash-loop with:

```text
panic: Cannot make VoteSet for height == 0, doesn't make sense.
```

This happens when `priv_validator_state.json` (the last *attempted* vote) ends
up one height ahead of the node's committed blockstore/state height. `bootstrap.sh`
now resets that state file on every run (so a fresh `full-reinit` no longer
hits it), but if a running chain gets stuck and `scenario3-restart` doesn't
recover it within a minute or two, do a reset:

```bash
make reinit        # wipe chain state, keep identities, restart
# or, if it's still wedged:
make clean-all && make full-reinit
```

This wipes chain state while keeping (reinit) or regenerating (clean-all) the
node identities. Acceptable for a devnet — there is no real data to preserve.

## Testing katana

katana always runs in Docker here — there is no supported "run `./katana`
locally" path. From the repo root, one command runs every stub score band
(healthy/eject/lower_vp/raise_vp/multi_eject) followed by the real-broadcast
path against this devnet:

```bash
make launch-test-katana
```

It reuses this devnet's `dev` key (no manual mnemonic/`add-key` step needed),
resets katana's own SQLite history before *each* scenario (several scenarios
target the same devnet validator, so a fresh lifecycle state per scenario is
what lets each one actually broadcast instead of finding that validator
already gate-blocked by the previous scenario), and streams katana's logs
live — Ctrl-C stops everything it started. Add `DRY_RUN=true` to rehearse
(evaluate + notify, never broadcast) instead.

For a single scenario, a single phase, or any manual/debugging step — config
reference, `add-key`, switching scenarios by hand, the real-broadcast
walkthrough — see **[`../deploy/e2e/README.md`](../deploy/e2e/README.md)**,
the single source of truth for testing katana.

### Go end-to-end suite (`../e2e`, build tag `e2e`)

`./katana-e2e.sh` runs katana as a host process (built from the working tree)
against this devnet and asserts its SQLite `proposals` table, notifications and
`/health`, plus the on-chain effect on RPC `/validators`:

| Subtest | Asserts |
|---|---|
| `deny` | 100% NO → katana calls `ExecuteOrRejectProposal` → `rejected` |
| `expire_then_late_reject` | `expired` after `pending_timeout`, still tracked → late NO → `rejected` |
| `unconfirmed_without_indexer_expires` | broadcast response dropped by a proxy, no `tx_indexer_url` → `unconfirmed`, then `expired` |
| `unconfirmed_reconciled_via_indexer` | same drop, with `tx_indexer_url` + `gnokey.address` → reconciled (tx hash + GovDAO id) → follows the vote → `rejected` |
| `guardrail_max_eligible_share` | 4/4 validators failing → nothing proposed, exactly one `guardrail` notification |
| `guardrail_min_valset_size` | ejecting validator4 would leave 3 < 4 → suppressed |
| `health` | `/health` 200, then 503 once gnomonitoring is gone for 3 poll intervals |
| `vp_change_executes` | `raise_vp` then `lower_vp` accepted + executed → validator4 power 1 → 2 → 1 |
| `accept` | 100% YES → `accepted`, `ValidatorRemoved`, validator4 leaves `/validators` (re-onboarded afterwards) |

```bash
export PATH=/path/to/mainnet-gnokey-dir:$PATH   # gnokey built from chain/mainnet
./katana-e2e.sh --reinit                        # fresh chain, then the whole suite
./katana-e2e.sh -- -run 'TestLifecycle/health'  # one subtest
go test -tags e2e ../e2e -run Fixture           # parsers vs captured artefacts, no devnet
```

The suite itself initialises GovDAO and onboards validator4 when needed. The
artefacts under `../e2e/testdata` (and their copies in
`../internal/{govdao,proposal,txindexer}/testdata`) were captured on this
devnet built from the mainnet tag; see the provenance list in
`../e2e/fixtures_test.go`.
