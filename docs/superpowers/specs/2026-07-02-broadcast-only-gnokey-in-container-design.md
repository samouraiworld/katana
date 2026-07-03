# Design — Broadcast-only katana with an embedded gnokey

Date: 2026-07-02
Status: approved design (implementation plan pending)

## Goal

katana must **only** broadcast ejection proposals — the dry-run mode is removed
entirely from the code. The `gnokey` binary is shipped **inside the katana
container image**, and for testing against the `gnoland-test/` devnet a dedicated
test key is provisioned into an in-container keybase from a mnemonic declared in
katana's config.

The product must keep working for a **real chain**: nothing devnet-specific is
hardcoded, key material is never required in plaintext config, and the signing
key can be a pre-provisioned keybase rather than an imported mnemonic.

## Non-goals

- No migration of existing SQLite databases. The e2e `katana.db` lives in a
  throwaway volume and is recreated from a fresh schema.
- No change to the rules / lifecycle / govdao / notify logic beyond what the
  dry-run removal strictly requires.
- No new submission method (`run`/`call` stay as-is).

## Decisions (locked)

1. **Remove dry-run completely from the code** — katana can only broadcast.
2. **gnokey built from gno source** in a Docker build stage, pinned via a
   `GNO_REF` build-arg (devnet commit for e2e, release tag for a real chain).
3. **Test key delivered as a mnemonic**, imported into an in-container keybase
   **by katana itself** (Go, at startup) — no shell entrypoint. Import is
   **optional**: empty mnemonic ⇒ use the existing keybase under `gnokey.home`.
4. **Fresh SQLite schema** — no incremental migration.

## Section 1 — Remove dry-run from the code

| File | Change |
|---|---|
| `internal/config/config.go` | Remove `Proposal.Mode` and `Proposal.OutputDir`; remove the `mode: dry-run` default (`setIfEmpty`), the `mode` validation, and `Runtime.Mode`. |
| `internal/proposal/proposal.go` | Delete the `DryRun` method and any helper it alone used (e.g. `shellJoin`, `safe` if now unused). Keep `Build`, `Broadcast`, `callArgs`, `runArgs`, `commonArgs`. |
| `internal/daemon/cycle.go` | `produce()` always broadcasts: drop the `rt.Mode == "broadcast"` guard and the dry-run branch; keep operator resolution, record, and notify. |
| `internal/lifecycle/lifecycle.go` | **Keep** the `default` branch (it now throttles *failed broadcasts* whose status is empty against re-spam every tick) but rewrite its comment: "dry-run" → "failed broadcast / no on-chain status yet". |
| `internal/store/store.go` | Remove `ProposalRecord.Mode` and `.Artifact` and their SQLite columns. Regenerate the schema fresh. |
| `internal/config/config_test.go`, `internal/lifecycle/lifecycle_test.go` | Adapt cases that referenced `mode: dry-run`; the cooldown-floor tests keep their meaning (now "failed/pending-less broadcast"). |
| `cmd/katana/main.go` | Update the header comment that mentions dry-run vs broadcast. |
| `prd.md`, `CLAUDE.md` | Drop "dry-run is the safe default"; document broadcast-only and the embedded-gnokey model. |

**Behavioural note (important):** on a successful broadcast the proposal row is
`StatusPending`; on a **failed** broadcast the row is recorded with an empty
status (existing behaviour at `cycle.go` `RecordProposal` before the error
check). The lifecycle `default` branch (cooldown floor from `CreatedAt`) is what
prevents a failing broadcast from being retried every `poll_interval`. This is
why the branch must stay — it is no longer about dry-run.

## Section 2 — gnokey inside the katana image

Add a gnokey build stage to `Dockerfile`, pinned by a build-arg:

```dockerfile
ARG GNO_REF=<pinned>            # devnet commit for e2e; release tag for prod
FROM golang:1.25-alpine AS gnokey
RUN apk add --no-cache git
RUN go install github.com/gnolang/gno/gno.land/cmd/gnokey@${GNO_REF}
# → /go/bin/gnokey
```

Final stage: `COPY --from=gnokey /go/bin/gnokey /usr/local/bin/gnokey`.

- The exact go-install module path (`github.com/gnolang/gno/gno.land/cmd/gnokey`)
  is verified as the first implementation step; fallback is building from a
  cloned `$GNO_REPO_PATH` via gno's own `--target gnokey` (the same target
  `gnoland-test/bootstrap.sh:59` already uses), which guarantees a devnet match.
- `GNO_REF` for the e2e image must equal the gno commit the devnet was built
  from (same `GNO_REPO_PATH`); a version skew can break tx encoding / chainid
  handling.
- The runtime stage keeps `ca-certificates`; gnokey needs no extra runtime deps.

## Section 3 — Test key: mnemonic in config → import by katana at startup

New config surface under `gnokey`:

- `mnemonic` (string, **optional**) — BIP39 phrase for the signing key.
- Env override `KATANA_GNOKEY_MNEMONIC` added to the `applyEnvOverrides`
  allowlist (so a real chain can pass it as a secret, never as plaintext YAML).
- Reuses existing `gnokey.key_name`, `gnokey.home`, `gnokey.password`.

Startup import (in Go, before `daemon.Run`):

```
if gnokey.mnemonic != "":
    if key_name not present in keybase(home):
        gnokey add --recover --insecure-password-stdin <key_name>   # feed mnemonic + password
    # else: key already imported (idempotent) → no-op
# if mnemonic == "": use the existing keybase at home as-is (production path)
```

- The exact non-interactive `gnokey add --recover` invocation (flag names, stdin
  order of mnemonic/password) is validated against the pinned gnokey during
  implementation and covered by the e2e run.
- `home` in-container = `/app/data/keybase` (persisted in the `e2e-katana-data`
  volume, writable by uid 10001 `katana`).
- Import lives next to config load / store open in the daemon boot path so it has
  one config source and is unit-testable with a fake exec.

**Real-chain path:** operators leave `mnemonic` empty and mount / provision a
keybase at `home`, passing only `KATANA_GNOKEY_PASSWORD`. Per-chain `gnokey`
overrides (already field-by-field merged) let a multi-chain deployment use a
distinct signer per network.

## Section 4 — e2e config + compose

- Rewrite `deploy/e2e/katana.config.yaml` as broadcast-only: a `gnokey` block
  (`bin: gnokey`, `key_name`, `home: /app/data/keybase`, `mnemonic: <test key>`),
  Discord `notifications`, `chains.local` unchanged. No `mode`, no `output_dir`.
- `docker-compose.e2e.yml`: katana service passes `KATANA_GNOKEY_PASSWORD` (and
  optionally `KATANA_GNOKEY_MNEMONIC`) via `environment`; add the `GNO_REF`
  build-arg. The `e2e-katana-data` volume now also holds the keybase.
- `deploy/e2e/katana.broadcast.host.yaml` becomes redundant (its whole reason was
  "gnokey lives on the host") → remove it, and revoke the Discord webhook that
  was committed in it.

### Devnet prerequisites (blocking — a broadcast that "succeeds" still yields no proposal without these)

- The test key's address must be **funded** in the devnet genesis
  (`gnoland-test/genesis_balances.txt`) so it can pay gas.
- The test key must be a **GovDAO member** (only members can create proposals);
  otherwise `dao.MustCreateProposal` in the run script fails on-chain.
- `GNO_REF` must match the devnet's gno commit.

These are provisioning steps, done once when the test key is created.

## Section 5 — End-to-end verification

1. `make test` — offline unit suite passes with dry-run removed (proves the
   removal did not break rules/lifecycle/config/store).
2. `make vet`.
3. `docker compose -f docker-compose.e2e.yml up --build` with the devnet already
   up; confirm katana logs the keybase import once, then normal eval ticks.
4. Down `validator4` (existing scenario) → gnomonitoring reports missed blocks →
   katana logs `BROADCAST ok ... proposal id="..."`.
5. Confirm the proposal exists on-chain (gnoweb `govdao_realm`) and a Discord
   `EventSent` notification was received.
6. `voteCycle` tracks the outcome; accept/reject updates lifecycle + notifies.

## Risks / open items

- **gnokey version skew** vs the devnet — mitigated by `GNO_REF`; the fallback is
  copying from gno's own `--target gnokey` build.
- **`gnokey add --recover` non-interactive flags** may differ across gno
  versions — validated during implementation against the pinned binary.
- **Fresh schema** means any existing e2e data is discarded — acceptable (test
  volume), called out so nobody expects history to survive.
