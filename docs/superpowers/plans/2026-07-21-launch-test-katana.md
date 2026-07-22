# One-shot `make launch-test-katana` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A single `make launch-test-katana` (repo root) that runs every stub
score-band scenario and the real-broadcast scenario against the
`gnoland-test/` devnet, in one foreground command, reusing the devnet's `dev`
signing key instead of a manually-provisioned one.

**Architecture:** All orchestration logic lives in one new shell script,
`scripts/launch-test-katana.sh`, invoked by a thin `Makefile` target. It
shells out to existing, unmodified `gnoland-test/Makefile` targets
(`full-reinit-4`, `stub-scenario`, `scenario2`) and `docker compose -f
docker-compose.e2e.yml`. The only other changes are: the two `deploy/e2e/*
.yaml.example` files (point `gnokey` at the reused `dev` key instead of a
dedicated `katana-e2e` mnemonic) and two README sections that document the new
one-shot path.

**Tech Stack:** Bash (`set -euo pipefail`), `make`, `docker compose`, existing
Go `stub-gnomonitoring` server (`gnoland-test/stub-gnomonitoring`, unmodified).

## Global Constraints

- No Go code changes — this plan touches only shell, Make, YAML config
  examples, and Markdown docs (per the spec's "Out of scope").
- No automated pass/fail assertions in the script — orchestration + live logs
  only, matching how every existing `gnoland-test/` scenario is verified
  today (confirmed with the user during brainstorming).
- Existing fine-grained targets (`stub-up`, `scenario2`, `stub-scenario`,
  `accept-govdao`, `add-key`, ...) must keep working unmodified.
- This repo has no CI harness for shell scripts and no way to spin up a real
  gno devnet in this environment (`gnoland-test/bootstrap.sh` needs a
  `GNO_REPO_PATH` checkout of [gnolang/gno](https://github.com/gnolang/gno)
  and Docker).
  Every task's verification step is therefore split into (a) static checks
  you *can* run right now (`bash -n`, `docker compose config`, `grep`), and
  (b) the exact commands to run against a real devnet later to confirm
  runtime behavior — do (a) as part of each task, do (b) once at the end
  (Task 5).

---

## File Structure

| File | Responsibility |
|---|---|
| `scripts/launch-test-katana.sh` (new) | All orchestration: preflight, chain reset, keybase provisioning, stub-phase loop, real-broadcast phase, cleanup trap. |
| `Makefile` (root, modified) | One new `.PHONY` target, `launch-test-katana`, that execs the script. |
| `deploy/e2e/katana.config.stub.yaml.example` (modified) | `gnokey` block points at the reused `dev` key; top comment updated. |
| `deploy/e2e/katana.config.yaml.example` (modified) | Same. |
| `deploy/e2e/README.md` (modified) | New "one-shot" section at the top. |
| `gnoland-test/README.md` (modified) | "Testing katana" section collapsed to point at the one-shot command. |

---

## Task 1: Point the e2e config examples at the reused `dev` key

**Files:**
- Modify: `deploy/e2e/katana.config.stub.yaml.example`
- Modify: `deploy/e2e/katana.config.yaml.example`

**Interfaces:**
- Produces: both files' `gnokey.key_name` = `"dev"`, `gnokey.home` =
  `"/app/data/dev-keybase"`, `gnokey.mnemonic` = `""` — Task 2's script relies
  on exactly this path (`/app/data/dev-keybase`) when it copies
  `gnoland-test/.gnokey-dev` into the `e2e-katana-data` volume.

- [ ] **Step 1: Update the top comment block in the stub config example**

In `deploy/e2e/katana.config.stub.yaml.example`, replace lines 14-21 (the
`IMPORTANT: every 'make full-reinit'...` block) with:

```yaml
# Signing key: reuses the devnet's pre-provisioned 'dev' account (funded, sole
# GovDAO T1 member from bootstrap.sh) instead of a dedicated key. 'make
# launch-test-katana' copies gnoland-test/.gnokey-dev into katana's data
# volume (as /app/data/dev-keybase) before starting the container, so
# gnokey.mnemonic below stays empty and internal/keybase.Ensure is a no-op.
# Running the stub path by hand instead of via launch-test-katana? Do that
# copy yourself, or fall back to a dedicated key with
# 'make -C gnoland-test add-key NAME=katana-e2e'.
```

- [ ] **Step 2: Update the `gnokey` block in the stub config example**

Replace:

```yaml
gnokey:
  bin: "gnokey"
  key_name: "katana-e2e"
  home: "/app/data/keybase"
  password: "devpassword123"                   # devnet-only throwaway keybase passphrase
  mnemonic: "<24-word devnet test mnemonic>"   # throwaway; devnet only
  gas_fee: "1000000ugnot"
  gas_wanted: 100000000   # lower_vp/raise_vp scripts need more than eject alone (~26M used observed)
```

with:

```yaml
gnokey:
  bin: "gnokey"
  key_name: "dev"
  home: "/app/data/dev-keybase"
  password: "devpassword123"                   # devnet-only throwaway keybase passphrase, set by bootstrap.sh
  mnemonic: ""                                  # empty: reuses the pre-provisioned 'dev' keybase (see above)
  gas_fee: "1000000ugnot"
  gas_wanted: 100000000   # lower_vp/raise_vp scripts need more than eject alone (~26M used observed)
```

- [ ] **Step 3: Same two edits in the real-broadcast config example**

In `deploy/e2e/katana.config.yaml.example`, replace the top comment (lines
1-15, the block explaining "Copy this to `katana.config.yaml`... set
gnokey.mnemonic/password...") with:

```yaml
# katana e2e config TEMPLATE — BROADCAST against the local devnet, in-container.
#
# Copy this to `katana.config.yaml` (git-ignored). Signing key: reuses the
# devnet's pre-provisioned 'dev' account (funded, sole GovDAO T1 member from
# bootstrap.sh) instead of a dedicated key — 'make launch-test-katana' copies
# gnoland-test/.gnokey-dev into katana's data volume (as
# /app/data/dev-keybase) before starting the container. Running this by hand
# instead? Do that copy yourself, or fall back to a dedicated key with
# 'make -C gnoland-test add-key NAME=katana-e2e' and set gnokey.mnemonic below
# (or leave it empty and uncomment the 'environment:' block in
# ../../docker-compose.e2e.yml to pass KATANA_GNOKEY_MNEMONIC instead).
#
# gnokey is shipped inside the katana image (Dockerfile GNO_REF). katana imports
# the signing key into /app/data/keybase at startup.
#
# The signing key's address MUST be funded in the devnet genesis AND be a GovDAO
# member, otherwise the broadcast succeeds but no proposal is created — see
# 'make add-key' in gnoland-test/Makefile.
```

and replace its `gnokey` block:

```yaml
gnokey:
  bin: "gnokey"
  key_name: "katana-e2e"
  home: "/app/data/keybase"
  password: "devpassword123"                   # devnet-only throwaway keybase passphrase
  mnemonic: "<24-word devnet test mnemonic>"   # throwaway; devnet only
  gas_fee: "1000000ugnot"
  gas_wanted: 100000000   # headroom for maketx run script creation (eject alone fits in less, but this covers it)
```

with:

```yaml
gnokey:
  bin: "gnokey"
  key_name: "dev"
  home: "/app/data/dev-keybase"
  password: "devpassword123"                   # devnet-only throwaway keybase passphrase, set by bootstrap.sh
  mnemonic: ""                                  # empty: reuses the pre-provisioned 'dev' keybase (see above)
  gas_fee: "1000000ugnot"
  gas_wanted: 100000000   # headroom for maketx run script creation (eject alone fits in less, but this covers it)
```

- [ ] **Step 4: Verify both files are still valid YAML**

Run: `python3 -c "import yaml,sys; [yaml.safe_load(open(f)) for f in ['deploy/e2e/katana.config.stub.yaml.example','deploy/e2e/katana.config.yaml.example']]; print('ok')"`
Expected: `ok` (if `python3`/`pyyaml` isn't available, `docker run --rm -v "$PWD":/w -w /w python:3-alpine sh -c "pip install -q pyyaml && python -c \"...\""` works too — either way, confirm no YAML syntax error).

- [ ] **Step 5: Commit**

```bash
git add deploy/e2e/katana.config.stub.yaml.example deploy/e2e/katana.config.yaml.example
git commit -m "docs(e2e): point config examples at the reused devnet 'dev' key"
```

---

## Task 2: `scripts/launch-test-katana.sh` + root Makefile target

**Files:**
- Create: `scripts/launch-test-katana.sh`
- Modify: `Makefile` (root)

**Interfaces:**
- Consumes: `deploy/e2e/katana.config.stub.yaml.example` and
  `deploy/e2e/katana.config.yaml.example` from Task 1 (specifically the
  `/app/data/dev-keybase` path baked into both).
- Consumes: unmodified `gnoland-test/Makefile` targets `full-reinit-4`,
  `stub-scenario`, `scenario2`, and `stub-gnomonitoring/main.go`'s `-listen`/
  `-file` flags (already used by the `stub-up` target: `go run . -listen
  :8989 -file scenarios/current.json`).
- Consumes: `docker-compose.e2e.yml`'s `katana` service definition, via
  `docker compose run`/`up`/`rm` (never a raw `docker run`/`docker volume`,
  since the `e2e-katana-data` volume's real, project-prefixed name isn't
  known ahead of time — see the provisioning step's comment in the script).
  `--no-deps` starts only `katana`; a plain `up` also starts
  `gnomonitoring`+`postgres`.
- Produces: `make launch-test-katana` — the only new public interface.

- [ ] **Step 1: Create the `scripts/` directory and write the script**

```bash
mkdir -p scripts
```

Create `scripts/launch-test-katana.sh`:

```bash
#!/usr/bin/env bash
# One-shot e2e run: fresh devnet -> every stub score-band scenario -> the
# real-broadcast scenario. See
# docs/superpowers/specs/2026-07-21-launch-test-katana-design.md.
#
# Reuses the devnet's pre-provisioned 'dev' signing key (see Task 1 in the
# plan / the config examples) instead of a dedicated katana-e2e key: no
# manual mnemonic/add-key step needed.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

GNOLAND_TEST_DIR="$ROOT_DIR/gnoland-test"
COMPOSE=(docker compose -f docker-compose.e2e.yml)
STUB_SCENARIOS=(healthy eject lower_vp raise_vp multi_eject)
STUB_WATCH_SECONDS=30   # ~2x the stub config's poll_interval (15s)

STUB_PID=""

cleanup() {
  if [ -n "$STUB_PID" ] && kill -0 "$STUB_PID" 2>/dev/null; then
    echo ">>> stopping stub-gnomonitoring (pid $STUB_PID)"
    kill "$STUB_PID" 2>/dev/null || true
    wait "$STUB_PID" 2>/dev/null || true
  fi
  echo ">>> docker compose down"
  "${COMPOSE[@]}" down
}
trap cleanup EXIT INT TERM

echo "=== [1/5] preflight ==="
if [ ! -f "$GNOLAND_TEST_DIR/.env" ] || [ ! -f "$GNOLAND_TEST_DIR/genesis.json" ]; then
  echo "!! gnoland-test/.env or genesis.json missing." >&2
  echo "!! Run this first: cd gnoland-test && GNO_REPO_PATH=/path/to/gno ./bootstrap.sh" >&2
  exit 1
fi
echo ">>> stopping any leftover real gnomonitoring/postgres from a previous run"
"${COMPOSE[@]}" stop gnomonitoring postgres 2>/dev/null || true

echo "=== [2/5] fresh chain (full-reinit-4) ==="
make -C "$GNOLAND_TEST_DIR" full-reinit-4

echo "=== [3/5] phase A - stub (every score band) ==="
cp deploy/e2e/katana.config.stub.yaml.example deploy/e2e/katana.config.yaml

echo ">>> provisioning katana's data volume with the devnet 'dev' keybase"
# Reuses the katana service's own volume mount (/app/data -> e2e-katana-data)
# instead of a raw 'docker run -v e2e-katana-data:/data': compose has no
# 'name:' override for that volume, so it actually resolves to the
# project-prefixed 'katana_e2e-katana-data' (confirmed via 'docker compose
# config') - a bare 'docker run' referencing the short name would silently
# write to a different, empty volume. '--user root' overrides the image's
# default 'katana' user (uid 10001, see Dockerfile) so the copy/chown can
# run; 'chown -R katana:katana' (by name - that user already exists in the
# image) leaves the copy owned by the same user the daemon runs as.
"${COMPOSE[@]}" run --rm --build --no-deps --user root --entrypoint sh \
  -v "$GNOLAND_TEST_DIR/.gnokey-dev:/src:ro" \
  katana -c 'rm -rf /app/data/dev-keybase && cp -r /src /app/data/dev-keybase && chown -R katana:katana /app/data/dev-keybase'

( cd "$GNOLAND_TEST_DIR/stub-gnomonitoring" && exec go run . -listen :8989 -file scenarios/current.json ) &
STUB_PID=$!
sleep 1   # let the stub server bind before katana starts polling it

"${COMPOSE[@]}" up -d --build --no-deps katana

for scenario in "${STUB_SCENARIOS[@]}"; do
  echo "--- stub scenario: $scenario ---"
  make -C "$GNOLAND_TEST_DIR" stub-scenario NAME="$scenario"
  since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  timeout "${STUB_WATCH_SECONDS}s" "${COMPOSE[@]}" logs -f --since "$since" katana || true
done

echo ">>> stub phase done - tearing down the stub katana container + server"
"${COMPOSE[@]}" rm -f -s katana
kill "$STUB_PID" 2>/dev/null || true
wait "$STUB_PID" 2>/dev/null || true
STUB_PID=""

echo "=== [4/5] phase B - real broadcast (non-regression) ==="
cp deploy/e2e/katana.config.yaml.example deploy/e2e/katana.config.yaml
make -C "$GNOLAND_TEST_DIR" scenario2

echo ">>> once a proposal appears in the logs below, from another terminal run:"
echo "    make -C gnoland-test accept-govdao ID=<id>"
echo "=== [5/5] streaming katana + gnomonitoring logs (Ctrl-C to stop) ==="
"${COMPOSE[@]}" up --build
```

- [ ] **Step 2: Make it executable**

```bash
chmod +x scripts/launch-test-katana.sh
```

- [ ] **Step 3: Add the Makefile target**

In `Makefile` (root), change:

```make
.PHONY: build run test tidy fmt vet clean
```

to:

```make
.PHONY: build run test tidy fmt vet clean launch-test-katana
```

and append at the end of the file:

```make

launch-test-katana:
	./scripts/launch-test-katana.sh
```

- [ ] **Step 4: Static verification (no devnet needed)**

Run: `bash -n scripts/launch-test-katana.sh`
Expected: no output (syntax OK).

Run: `shellcheck scripts/launch-test-katana.sh` (if `shellcheck` is
installed; skip otherwise — it's not a hard requirement, just a good sanity
check).
Expected: no errors (warnings about `cd` without `||` in a subshell, if any,
are fine here since `set -e` is active in a `( ... )` subshell context and
the script already checks paths in preflight).

Run: `make -n launch-test-katana`
Expected:
```
./scripts/launch-test-katana.sh
```

Run: `docker compose -f docker-compose.e2e.yml config >/dev/null && echo ok`
Expected: `ok` (confirms `docker-compose.e2e.yml` — untouched by this plan —
is still valid; also catches accidental typos if this task's diff is later
rebased against a compose change).

- [ ] **Step 5: Commit**

```bash
git add scripts/launch-test-katana.sh Makefile
git commit -m "feat(e2e): add one-shot make launch-test-katana"
```

---

## Task 3: Document the one-shot path in `deploy/e2e/README.md`

**Files:**
- Modify: `deploy/e2e/README.md`

- [ ] **Step 1: Insert a new section right after the intro table (after line 26, before "## Setup (once)")**

Insert:

```markdown
## One-shot: `make launch-test-katana`

From the repo root, one command runs every stub score band
(healthy/eject/lower_vp/raise_vp/multi_eject) followed by the real-broadcast
scenario, against a freshly reset devnet:

```bash
cd gnoland-test && GNO_REPO_PATH=/path/to/gno ./bootstrap.sh   # one-time
cd ..
make launch-test-katana
```

It reuses the devnet's pre-provisioned `dev` key (no manual mnemonic /
`add-key` step), resets the chain (`full-reinit-4`), streams katana's logs
live for each stub scenario, then switches to the real-broadcast path
(`scenario2` + real `gnomonitoring`+Postgres) and stays attached until you
Ctrl-C. It does not assert pass/fail — read the logs, the same way you would
running each step by hand.

The rest of this document covers each phase manually: useful when debugging
one scenario in isolation, or when you want a dedicated `katana-e2e` signing
key instead of reusing `dev`.
```

- [ ] **Step 2: Verify the anchor link still resolves**

Run: `grep -n "^## " deploy/e2e/README.md`
Expected: `## One-shot: \`make launch-test-katana\`` appears once, directly
after the intro table's section and before `## Setup (once)`; no other
heading text changed.

- [ ] **Step 3: Commit**

```bash
git add deploy/e2e/README.md
git commit -m "docs(e2e): document the one-shot make launch-test-katana path"
```

---

## Task 4: Simplify `gnoland-test/README.md`'s "Testing katana" section

**Files:**
- Modify: `gnoland-test/README.md`

- [ ] **Step 1: Replace the "## Testing katana" section**

Replace the entire section (from `## Testing katana` to the end of the file)
with:

```markdown
## Testing katana

katana always runs in Docker here — there is no supported "run `./katana`
locally" path. From the repo root, one command runs every stub score band
(eject/lower_vp/raise_vp/multi_eject/healthy) followed by the real-broadcast
path against this devnet:

```bash
make launch-test-katana
```

It reuses this devnet's `dev` key (no manual mnemonic/`add-key` step needed)
and streams katana's logs live — Ctrl-C stops everything it started.

For a single scenario, a single phase, or any manual/debugging step — config
reference, `add-key`, switching scenarios by hand, the real-broadcast
walkthrough — see **[`../deploy/e2e/README.md`](../deploy/e2e/README.md)**,
the single source of truth for testing katana.
```

- [ ] **Step 2: Verify the file still ends cleanly and the cross-link resolves**

Run: `tail -20 gnoland-test/README.md`
Expected: ends with the new "Testing katana" section above, no leftover
stray content from the old version.

Run: `test -f deploy/e2e/README.md && echo ok`
Expected: `ok` (the relative link `../deploy/e2e/README.md` from
`gnoland-test/README.md` resolves).

- [ ] **Step 3: Commit**

```bash
git add gnoland-test/README.md
git commit -m "docs(gnoland-test): simplify Testing katana section around launch-test-katana"
```

---

## Task 5: Manual acceptance run (requires a real devnet checkout)

This plan's changes can't be exercised by an automated test suite — they
orchestrate Docker/gno/gnokey. Run this once, by hand, against a real
`GNO_REPO_PATH` checkout, before considering the feature done.

**Files:** none (verification only).

- [ ] **Step 1: Fresh bootstrap (skip if already done)**

```bash
cd gnoland-test && GNO_REPO_PATH=/path/to/gno ./bootstrap.sh && cd ..
```

- [ ] **Step 2: Run the one-shot command**

```bash
make launch-test-katana
```

Expected, in order:
- preflight passes (no missing `.env`/`genesis.json` error)
- `full-reinit-4` completes, ends with validator4 confirmed in the valset
- the keybase-copy `docker compose run` exits 0
- katana's stub-phase container comes up; for each of the 5 scenarios, ~30s
  of live katana logs print, showing evaluation activity (an `eject`/
  `lower_vp`/`raise_vp` broadcast log line for the non-`healthy` scenarios)
- the stub phase tears down (`docker compose rm -f -s katana`,
  stub-gnomonitoring process killed — confirm with `pgrep -fl
  'stub-gnomonitoring'` printing nothing)
- `scenario2` stops validator4
- the real stack (`gnomonitoring`+`postgres`+`katana`) comes up and logs
  stream in the foreground

- [ ] **Step 3: Confirm the real-broadcast path reaches an ejection proposal**

Watch the streamed logs for a `ProposalCreated`/broadcast log line, note its
`id`, then from a second terminal:

```bash
make -C gnoland-test accept-govdao ID=<id>
```

Expected: `>>> done: proposal #<id> accepted + executed.` and validator4 no
longer in the valset (`curl -s http://localhost:26661/status | jq
'.result.validator_info'` — connection refused/absent, since `scenario2`
stopped it and the ejection also removed it from `r/sys/validators/v3`).

- [ ] **Step 4: Confirm Ctrl-C cleans up**

Press Ctrl-C on the still-running `make launch-test-katana`.
Expected: `>>> docker compose down` runs, `docker compose -f
docker-compose.e2e.yml ps` afterwards shows no running containers from this
stack, and `gnoland-test`'s own containers (`validator`, `validator2`, ...)
are untouched (still running — they're a separate compose project).

- [ ] **Step 5: Record the result**

No commit needed for this task (verification only) — note any deviation from
"Expected" above back into the plan/spec if something needs a follow-up fix.
