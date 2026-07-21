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
STUB_DIR="$GNOLAND_TEST_DIR/stub-gnomonitoring"
STUB_BIN_DIR="$(mktemp -d)"
STUB_BIN="$STUB_BIN_DIR/stub-gnomonitoring"

STUB_PID=""

cleanup() {
  if [ -n "$STUB_PID" ] && kill -0 "$STUB_PID" 2>/dev/null; then
    echo ">>> stopping stub-gnomonitoring (pid $STUB_PID)"
    kill "$STUB_PID" 2>/dev/null || true
    wait "$STUB_PID" 2>/dev/null || true
  fi
  rm -rf "$STUB_BIN_DIR"
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
if [ -z "${KATANA_DISCORD_WEBHOOK:-}" ]; then
  echo "!! KATANA_DISCORD_WEBHOOK is not set - both configs enable Discord" >&2
  echo "!! notifications, but with an empty webhook_url they'll silently" >&2
  echo "!! no-op. export KATANA_DISCORD_WEBHOOK=<url> before running this" >&2
  echo "!! if you want to actually receive them." >&2
fi
if command -v lsof >/dev/null 2>&1 && lsof -i :8989 -sTCP:LISTEN >/dev/null 2>&1; then
  echo "!! something is already listening on :8989 (stub-gnomonitoring's port)," >&2
  echo "!! likely an orphaned process from a previous interrupted run." >&2
  echo "!! find it with 'lsof -i :8989', kill its PID, then re-run." >&2
  exit 1
fi
echo ">>> stopping any leftover real gnomonitoring/postgres from a previous run"
"${COMPOSE[@]}" stop gnomonitoring postgres 2>/dev/null || true

echo "=== [2/5] fresh chain (full-reinit-4) ==="
make -C "$GNOLAND_TEST_DIR" full-reinit-4

echo "=== [3/5] phase A - stub (every score band) ==="
cp deploy/e2e/katana.config.stub.yaml.example deploy/e2e/katana.config.yaml

echo ">>> provisioning katana's data volume with the devnet 'dev' keybase and resetting katana's own history"
# Reuses the katana service's own volume mount (/app/data -> e2e-katana-data)
# instead of a raw 'docker run -v e2e-katana-data:/data': compose has no
# 'name:' override for that volume, so it actually resolves to the
# project-prefixed 'katana_e2e-katana-data' (confirmed via 'docker compose
# config') - a bare 'docker run' referencing the short name would silently
# write to a different, empty volume. '--user root' overrides the image's
# default 'katana' user (uid 10001, see Dockerfile) so the copy/chown can
# run; 'chown -R katana:katana' (by name - that user already exists in the
# image) leaves the copy owned by the same user the daemon runs as.
# The trailing 'rm -f /app/data/*.db' wipes katana's own SQLite history
# (proposals/lifecycle state) in that same volume, since 'e2e-katana-data' is
# a named volume that outlives 'docker compose down' and 'full-reinit-4'
# (which only resets the chain) - without it, stale 'pending' rows from a
# prior run's chain incarnation would permanently block re-proposing via
# internal/lifecycle.Gate. This makes every invocation start from a clean
# slate.
"${COMPOSE[@]}" run --rm --build --no-deps --user root --entrypoint sh \
  -v "$GNOLAND_TEST_DIR/.gnokey-dev:/src:ro" \
  katana -c 'rm -rf /app/data/dev-keybase && cp -r /src /app/data/dev-keybase && chown -R katana:katana /app/data/dev-keybase && rm -f /app/data/*.db'

echo ">>> building stub-gnomonitoring"
# Built once into a temp binary and exec'd directly, rather than 'go run .':
# 'go run' compiles and then forks a child process for the actual binary, so
# killing the tracked PID kills only the 'go run' wrapper and leaves the real
# server (still holding :8989) orphaned - confirmed in practice, it survived
# a full Phase A teardown and made Phase B's real gnomonitoring fail to bind
# the same port. Running the built binary directly means STUB_PID IS the
# server, so 'kill "$STUB_PID"' actually frees the port.
( cd "$STUB_DIR" && go build -o "$STUB_BIN" . )

"$STUB_BIN" -listen :8989 -file "$STUB_DIR/scenarios/current.json" &
STUB_PID=$!
# Wait for the stub server to bind before katana starts polling it.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  curl -s --connect-timeout 1 http://localhost:8989/ -o /dev/null && break
  sleep 0.5
done

"${COMPOSE[@]}" up -d --build --no-deps katana

for scenario in "${STUB_SCENARIOS[@]}"; do
  echo "--- stub scenario: $scenario ---"
  make -C "$GNOLAND_TEST_DIR" stub-scenario NAME="$scenario"
  # Reset katana's own lifecycle history and restart it before watching this
  # scenario. Every stub scenario's "notable" score lands on the same devnet
  # validator (samourai-crew-4) - without this, only the first
  # action-triggering scenario in the loop would ever actually broadcast:
  # every later one would find crew-4 already gate-blocked by a still-
  # 'pending' proposal from the previous scenario and just suppress
  # (confirmed in practice - eject fired, then lower_vp/raise_vp never did).
  "${COMPOSE[@]}" rm -f -s katana
  "${COMPOSE[@]}" run --rm --no-deps --user root --entrypoint sh katana -c 'rm -f /app/data/*.db'
  "${COMPOSE[@]}" up -d --no-deps katana
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
