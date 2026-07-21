#!/usr/bin/env bash
# Renders scenarios/*.json.tmpl -> scenarios/*.json, substituting the live
# devnet validator addresses: validator/validator2/validator3 from genesis.json
# (samourai-crew-1/2/3, stable monikers but regenerated addresses on every
# fresh bootstrap) and validator4 from .devaddrs.mk (samourai-crew-4, written
# by bootstrap.sh). Re-run automatically by every stub-* Makefile target, so
# scenarios always match whatever devnet is currently up.
set -euo pipefail
cd "$(dirname "$0")"

GENESIS="../genesis.json"
DEVADDRS="../.devaddrs.mk"

test -f "$GENESIS" || { echo "!! $GENESIS not found — run 'make full-reinit' (or full-reinit-4) first"; exit 1; }
test -f "$DEVADDRS" || { echo "!! $DEVADDRS not found — run 'make full-reinit' (or full-reinit-4) first"; exit 1; }

V1_ADDR=$(jq -r '.validators[] | select(.name=="samourai-crew-1") | .address' "$GENESIS")
V2_ADDR=$(jq -r '.validators[] | select(.name=="samourai-crew-2") | .address' "$GENESIS")
V3_ADDR=$(jq -r '.validators[] | select(.name=="samourai-crew-3") | .address' "$GENESIS")
V4_ADDR=$(grep -E '^VALIDATOR4_ADDR' "$DEVADDRS" | sed 's/.*:= *//')

for addr_var in V1_ADDR:$V1_ADDR V2_ADDR:$V2_ADDR V3_ADDR:$V3_ADDR V4_ADDR:$V4_ADDR; do
  name="${addr_var%%:*}"; val="${addr_var#*:}"
  test -n "$val" || { echo "!! could not resolve $name (empty) — is the devnet bootstrapped?"; exit 1; }
done

cd scenarios
for f in *.json.tmpl; do
  out="${f%.tmpl}"
  sed -e "s/__V1_ADDR__/$V1_ADDR/g" \
      -e "s/__V2_ADDR__/$V2_ADDR/g" \
      -e "s/__V3_ADDR__/$V3_ADDR/g" \
      -e "s/__V4_ADDR__/$V4_ADDR/g" \
      "$f" > "$out"
done
echo "rendered $(ls -1 *.json.tmpl | wc -l) scenario(s) into stub-gnomonitoring/scenarios/"
