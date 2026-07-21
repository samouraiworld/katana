# katana end-to-end on the local devnet

This runs **katana** against the local gno.land devnet in this directory, through
the full flow: detect a bad validator → broadcast an ejection proposal → follow
the GovDAO vote → notify. It reuses the devnet and scenarios documented in
[README.md](README.md).

## Prerequisites

- The devnet is bootstrapped and running (`GNO_REPO_PATH=… ./bootstrap.sh`,
  `docker compose up -d`). See [README.md](README.md).
- A **gnomonitoring** backend pointed at this devnet as `chain=local`
  (RPC `:26658`, indexer `:8546`, gnoweb `:8890`). See README "Pointing
  gnomonitoring at the devnet".
- `gnokey` on PATH and the devnet `dev` keybase at `.gnokey-dev/`
  (passphrase `devpassword123`).

## Flow

1. **Onboard then down a validator** so gnomonitoring records missed blocks /
   CRITICAL alerts for it:

   ```bash
   make scenario1          # add validator4 to the valset via GovDAO
   make scenario2          # stop validator4 → it starts missing blocks
   ```

   Wait until gnomonitoring shows CRITICAL / missed blocks for validator4 on
   `chain=local`.

2. **Configure katana** from the example and run it:

   ```bash
   cp gnoland-test/config.katana.local.yaml.example config.yaml
   # adjust gnokey.home to the absolute path of gnoland-test/.gnokey-dev if needed
   ./katana --config config.yaml
   ```

3. **Expected behaviour**
   - the eval worker finds validator4 eligible and, in `broadcast` mode, submits
     the ejection proposal via `gnokey maketx run` (the built-in `Power=0`
     script through `r/sys/validators/v3`);
   - the on-chain proposal id is captured from the `ProposalCreated` event and
     stored (status `pending`);
   - the vote worker polls the GovDAO render for that id.

4. **Drive the vote** as the sole GovDAO member to test both outcomes:

   ```bash
   # Vote YES then execute → validator ejected → katana emits "accepted"
   gnokey maketx call -pkgpath gno.land/r/gov/dao -func MustVoteOnProposalSimple \
     -args <ID> -args YES -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
     -chainid dev -remote http://localhost:26658 -home "$PWD/gnoland-test/.gnokey-dev" dev
   gnokey maketx call -pkgpath gno.land/r/gov/dao -func ExecuteProposal \
     -args <ID> -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
     -chainid dev -remote http://localhost:26658 -home "$PWD/gnoland-test/.gnokey-dev" dev
   ```

   Voting NO (or letting it expire) exercises the `rejected` path → katana
   suppresses re-proposal for `rejected_backoff` and stops after
   `max_rejections`.

## Known gap — operator vs signing address

gnomonitoring keys missed blocks / alerts by a validator's **signing/consensus
address**, but `NewValoperChange` (the ejection) needs the validator's
**operator address**. On this devnet that is `VALIDATOR4_ADDR` (signing) vs
`VALIDATOR4_OP_ADDR` (operator) in `.devaddrs.mk`.

katana currently uses the metric address as the operator address
(`Target.OperatorAddress` falls back to `Address`). For the e2e to eject the
right validator, katana must resolve **signing → operator** via the valopers
realm (as gnomonitoring's `valoper.go` does). Until that resolution is wired in,
either:

- point the eligible-validator metric at the operator address in your test, or
- set `proposal.submit: run` with a custom `run_script_template` that already
  embeds the correct operator address.

This mapping is the main remaining item to make the broadcast e2e fully
automatic (tracked in `prd.md` §12.5 PP-5).

## Stub gnomonitoring — testing score bands without real chain degradation

katana only ever talks to gnomonitoring via `GET /api/reports/validators`
(`internal/gnomonitoring/client.go`). `stub-gnomonitoring/` serves that exact
JSON contract from a local file instead of a real gnomonitoring backend, so
every score band (`eject`/`lower_vp`/`raise_vp`), the lifecycle gate, and both
`dry_run` values can be exercised deterministically and instantly — no need to
stop containers and wait for real downtime windows. The live valset check
(`gno.IsValidator`) still runs against the real chain, so you need the devnet
up with all 4 validators onboarded:

```bash
make full-reinit-4                 # fresh chain, validator4 onboarded via GovDAO
make stub-scenario NAME=healthy    # render scenarios + activate one (default: healthy)
make stub-up                       # foreground server on :8989 — leave this running
```

In another terminal:

```bash
cp gnoland-test/stub-gnomonitoring/config.katana.stub.yaml.example config.yaml
# adjust gnokey.home to the absolute path of gnoland-test/.gnokey-dev if needed
./katana --config config.yaml
```

Switch scenarios at any time — no restart of `stub-up` needed, katana picks
the change up on its next `poll_interval`:

```bash
make stub-scenario NAME=eject        # validator4: score 15 on current_month (< score_below 30)
make stub-scenario NAME=lower_vp     # validator4: score 45 on current_week (< score_below 60)
make stub-scenario NAME=raise_vp     # validator4: score 90 on current_year (>= score_at_or_above 85)
make stub-scenario NAME=multi_eject  # validator3 AND validator4 both below eject threshold
make stub-scenario NAME=healthy      # everyone healthy, no action expected
```

(`make stub-list` prints the full set.) Each scenario's addresses are
re-rendered from the live devnet (`genesis.json` + `.devaddrs.mk`) every time
you switch, so they stay correct across `full-reinit`/`full-reinit-4` reruns.

Things worth exercising this way, beyond the four scenarios above:

- **`dry_run: true`** — flip it in `config.yaml`, rerun any scenario, and
  confirm katana logs `[DRY-RUN]`, notifies with the script + ready gnokey
  command, records `store.StatusDryRun`, and never calls `gnokey`.
- **Exceptions** — add the target validator's signing address (printed in the
  server logs, or `.devaddrs.mk`'s `VALIDATOR4_ADDR` for validator4) to
  `exceptions:` in `config.yaml` and confirm the eligible action is logged but
  suppressed.
- **Lifecycle gate** — after a proposal is broadcast, switch back to the same
  degraded scenario and confirm the `pending`/`cooldown`/`rejected_backoff`
  suppression logs (drive `accepted`/`rejected` via `make accept-govdao` /
  voting NO, same as the main flow above).
