#!/usr/bin/env bash
# katana-e2e.sh - run katana's opt-in end-to-end suite (../e2e, build tag
# `e2e`) against this devnet. katana runs as a host process built from the
# working tree and signs with the devnet `dev` key through the host `gnokey`.
#
# usage: ./katana-e2e.sh [--reinit] [--down] [-- <extra go test args>]
#   --reinit  wipe the chain first (make reinit; keeps keys/addresses)
#   --down    stop the devnet containers when done
#   e.g.      ./katana-e2e.sh -- -run 'TestLifecycle/deny'
#
# Prerequisites: docker, go, curl, jq, a gnokey built from the gno.land
# mainnet tag (chain/mainnet) first on PATH (or KATANA_E2E_GNOKEY), and a
# one-time `GNO_REPO_PATH=/path/to/gno-mainnet ./bootstrap.sh`. The suite
# itself initialises GovDAO and onboards validator4 (the eject target) when
# they are missing.
#
# Paths covered (asserted on katana's SQLite proposals table, notifications,
# /health and RPC /validators):
#   deny                                -> rejected
#   expire_then_late_reject             -> expired, then rejected
#   unconfirmed_without_indexer_expires -> unconfirmed, then expired
#   unconfirmed_reconciled_via_indexer  -> unconfirmed, reconciled, then rejected
#   guardrail_max_eligible_share        -> no proposal, one guardrail notification
#   guardrail_min_valset_size           -> ejection suppressed
#   health                              -> /health 200, then 503
#   vp_change_executes                  -> validator4 power 1 -> 2 -> 1
#   accept                              -> accepted (validator4 leaves the valset)
# Fixture-only parser checks (no devnet): go test -tags e2e ../e2e -run Fixture
set -euo pipefail

cd "$(dirname "$0")"
export COMPOSE_PROJECT_NAME=gnoland-test

REINIT=0
DOWN=0
EXTRA=()
while [ $# -gt 0 ]; do
  case "$1" in
    --reinit) REINIT=1 ;;
    --down) DOWN=1 ;;
    --) shift; EXTRA=("$@"); break ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

for bin in docker go gnokey curl jq; do
  command -v "$bin" >/dev/null || { echo "missing dependency: $bin" >&2; exit 1; }
done
[ -f .devaddrs.mk ] && [ -d .gnokey-dev ] || {
  echo "devnet not bootstrapped: run GNO_REPO_PATH=/path/to/gno ./bootstrap.sh" >&2; exit 1; }

if [ "$DOWN" = 1 ]; then
  trap 'docker compose --profile phase2 down' EXIT
fi

if [ "$REINIT" = 1 ]; then
  make reinit
elif ! curl -sf http://localhost:26658/status >/dev/null; then
  make up
fi
make wait-chain

cd ..
go test -tags e2e ./e2e -count=1 -v -timeout 40m "${EXTRA[@]}"
