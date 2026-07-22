# One-shot `make launch-test-katana`

## Context

Testing katana's e2e stack today requires juggling two separate procedures
(`gnoland-test/README.md`, `deploy/e2e/README.md`), several terminals, and a
manual mnemonic copy-paste:

- **Stub path** (day-to-day, `deploy/e2e/README.md` "Stub path"): bootstrap the
  devnet, `make full-reinit-4`, `make add-key NAME=katana-e2e` (generates a
  mnemonic you must paste into `deploy/e2e/katana.config.yaml`), `make
  stub-scenario NAME=...` + `make stub-up` in one foreground terminal, then
  `docker compose -f docker-compose.e2e.yml up --build --no-deps katana` in a
  second terminal. Switching score bands means going back to the first
  terminal.
- **Real broadcast path** (non-regression, "Real broadcast path"): same
  bootstrap, `make scenario2` to stop validator4, then `docker compose -f
  docker-compose.e2e.yml up --build` (real `gnomonitoring` + Postgres), then
  `make accept-govdao ID=<id>` in a second terminal once a proposal appears.

Both paths already work and are documented; the friction is entirely
orchestration (multiple terminals, a manual key-provisioning step, remembering
which config example to copy where). This spec adds one command that runs
every scenario of both paths back to back, without touching what each
scenario actually does.

## Goal

`make launch-test-katana` at the repo root: one command, one terminal, runs
**every** stub score-band scenario followed by the real-broadcast scenario,
using the existing devnet and compose files as-is except for the signing-key
simplification below. Blocks in the foreground with live katana logs
(Ctrl-C to abort); does not attempt automatic pass/fail assertions — the
operator reads the logs, exactly as they do today.

Existing fine-grained targets (`stub-up`, `scenario2`, `stub-scenario`,
`accept-govdao`, `add-key`, ...) are untouched and keep working for anyone
who wants to drive a single step by hand or debug a failure from
`launch-test-katana`.

## Signing key: reuse `dev` instead of provisioning `katana-e2e`

Today's biggest manual step is `make add-key NAME=katana-e2e [MNEMONIC=...]`
(generates or accepts a mnemonic, funds it, adds it as a GovDAO T1 member),
followed by pasting that mnemonic into `deploy/e2e/katana.config.yaml`'s
`gnokey.mnemonic`.

`bootstrap.sh` already provisions a `dev` account that is funded, the sole
GovDAO T1 member, and lives in `gnoland-test/.gnokey-dev` — everything
`add-key` builds from scratch for `katana-e2e`. `launch-test-katana` instead
copies that keybase into katana's own data volume before starting it, and
points `gnokey` at the copy:

- **Not a live bind-mount.** The `katana` container runs as a non-root user
  (`katana`, uid 10001 — see `Dockerfile`); `gnoland-test/.gnokey-dev` is
  owned by the host user. gnokey's keybase needs write access (internal
  locking) even to list/sign, so a read-only mount would break broadcasting,
  and a writable mount would let the container's uid 10001 write into a
  host-owned directory also used by host-run `gnokey`/`make` commands
  (`scenario1`, `accept-govdao`, ...), corrupting its ownership.
  `docker-compose.e2e.yml` needs **no change**.
  - Instead, before bringing `katana` up, `launch-test-katana` copies
    `gnoland-test/.gnokey-dev` into the `katana` service's own
    `e2e-katana-data` volume via `docker compose run --user root --entrypoint
    sh --no-deps katana -c '...'` — reusing the service's already-declared
    volume mount (`/app/data`) rather than a raw `docker run -v
    e2e-katana-data:/data`, which would silently target the wrong volume:
    compose has no `name:` override for `e2e-katana-data`, so it actually
    resolves to the project-prefixed `katana_e2e-katana-data` (confirmed via
    `docker compose config`). `--user root` overrides the image's default
    `katana` user (uid 10001, see `Dockerfile`) so the copy/`chown` can run;
    `chown -R katana:katana` (by name — the image already defines that user)
    leaves the copy owned by the same user the daemon runs as. This runs on
    every invocation (cheap, idempotent) so it always reflects whatever
    `.gnokey-dev` currently holds.
- points `gnokey.key_name: dev`, `gnokey.home: /app/data/dev-keybase`
  (the copy's path inside the volume), `gnokey.mnemonic: ""` in both e2e
  config examples.

`internal/keybase.Ensure` is already a documented no-op when `mnemonic` is
empty (the "pre-provisioned" path) — the key is expected to already exist at
`gnokey.home`, which the copy step now guarantees. This removes `add-key` and
the mnemonic copy-paste from the one-shot flow entirely. It blurs "GovDAO
admin" and "katana operator" into the same devnet-only, throwaway key;
acceptable for disposable e2e, and `add-key` remains available for anyone who
wants a distinct `katana-e2e` identity.

## Flow

```
make launch-test-katana
│
├─ 1. Preflight
│     - gnoland-test/.env and genesis.json must exist, else fail with a
│       pointer to `GNO_REPO_PATH=... ./bootstrap.sh` (no auto-bootstrap).
│     - if the real-broadcast services from a previous run are still up,
│       stop them: `docker compose -f docker-compose.e2e.yml stop
│       gnomonitoring postgres` — guarantees the stub phase talks to
│       stub-gnomonitoring only, never a stale real backend.
│
├─ 2. Fresh chain
│     make -C gnoland-test full-reinit-4
│
├─ 3. Phase A — stub (all score bands, katana's own logic)
│     - cp deploy/e2e/katana.config.stub.yaml.example →
│       deploy/e2e/katana.config.yaml
│     - provision katana's data volume with the `dev` keybase:
│         docker compose -f docker-compose.e2e.yml run --rm --build \
│           --no-deps --user root --entrypoint sh \
│           -v gnoland-test/.gnokey-dev:/src:ro katana -c \
│           'rm -rf /app/data/dev-keybase && cp -r /src /app/data/dev-keybase \
│            && chown -R katana:katana /app/data/dev-keybase'
│     - start stub-gnomonitoring in the background (`go run .` in
│       gnoland-test/stub-gnomonitoring, PID tracked for cleanup)
│     - docker compose -f docker-compose.e2e.yml up -d --build --no-deps katana
│     - for each scenario in healthy, eject, lower_vp, raise_vp, multi_eject:
│         make -C gnoland-test stub-scenario NAME=<scenario>
│         stream katana's logs live for ~2 poll_intervals (~30s) so the
│         operator sees that scenario's evaluation, then move on
│     - stop the katana container and the stub server
│
├─ 4. Phase B — real broadcast (non-regression)
│     - cp deploy/e2e/katana.config.yaml.example → deploy/e2e/katana.config.yaml
│     - make -C gnoland-test scenario2          # stop validator4, real degradation
│     - docker compose -f docker-compose.e2e.yml up --build
│       (gnomonitoring + postgres + katana; no -d — this is the foreground tail)
│     - print the `make -C gnoland-test accept-govdao ID=<id>` reminder once,
│       before attaching, so the operator can run it from a second terminal
│       while phase B's logs stream
│
└─ Ctrl-C at any point: trap cleans up (kill stub server if still running,
     `docker compose -f docker-compose.e2e.yml down`).
```

Phase A never votes or executes a proposal, so validator4 stays in the valset
throughout — sequencing all five stub scenarios is non-destructive and safe to
run every time. Phase B starts from the same still-fresh chain (no second
`full-reinit-4`); `scenario2`'s validator4 downtime is independent of whatever
proposals Phase A created.

## Files touched

- **`Makefile`** (repo root) — new `launch-test-katana` target, implemented as
  a shell recipe (preflight, the keybase copy step, the two phases, the
  cleanup trap). May be split into `launch-test-katana-stub` /
  `launch-test-katana-real` sub-targets that `launch-test-katana` chains, so
  a stub-only or real-only run stays available as a one-liner too.
- **`docker-compose.e2e.yml`** — no change (see "Signing key" above for why a
  mount was rejected).
- **`deploy/e2e/katana.config.stub.yaml.example`**,
  **`deploy/e2e/katana.config.yaml.example`** — `gnokey.key_name: dev`,
  `gnokey.home: /app/data/dev-keybase`, `gnokey.mnemonic: ""`; drop the
  `add-key`/mnemonic comment block.
- **`deploy/e2e/README.md`** — add a "one-shot: `make launch-test-katana`"
  section at the top; keep the existing step-by-step procedures below for
  debugging a phase in isolation.
- **`gnoland-test/README.md`** — collapse the "Testing katana" section (today:
  two commands across two terminals, a manual `add-key`/mnemonic step) down to
  `make launch-test-katana` plus a pointer to `deploy/e2e/README.md` for
  anything manual/debugging.
- **`gnoland-test/Makefile`** — no functional change. `full-reinit-4`,
  `scenario2`, `stub-scenario`, `accept-govdao` are reused as-is; `add-key`
  stays available but is no longer part of this flow.

## Out of scope

- No automatic pass/fail assertions (DB queries, log greps, polling loops) —
  confirmed with the user: orchestration + live logs only, same verification
  model as today.
- No change to `gnoland-test/README.md`'s gnomonitoring-focused scenarios
  (`scenario1`/`scenario3`/`scenario4`/`scenario5`/`scenario-mute`) — those
  exercise gnomonitoring itself, not katana, and stay manual.
- No change to `internal/keybase.Ensure`, `internal/proposal`, or any Go code
  — this is entirely Makefile/compose/config orchestration.
- `add-key` and a dedicated `katana-e2e` identity are not removed, only no
  longer required for `launch-test-katana`.
