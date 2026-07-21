# e2e stack — gnomonitoring + katana on the local devnet

[`docker-compose.e2e.yml`](../../docker-compose.e2e.yml) runs **Postgres +
gnomonitoring + katana** wired to the local gno.land devnet, so you can watch
katana detect a bad validator, broadcast a real GovDAO ejection proposal, and
follow it through to acceptance/rejection — end-to-end, in-container.

The devnet is a separate layer (it is built locally, not pulled). The three
layers share the devnet's Docker network `gnoland-test_default`.

```
gnoland-test/ (layer 1)          docker-compose.e2e.yml (layer 2)
  validator / 2 / 3      ◄─────    gnomonitoring (chain=local) ──► REST :8989
  tx-indexer, gnoweb               postgres
  (network gnoland-test_default)   katana (broadcast) ──► /health :8990, gnokey
```

katana ships `gnokey` inside its image and imports a signing key from a
mnemonic into an in-container keybase (`/app/data/keybase`) at startup — there
is no host-run path anymore.

**Data volume:** the SQLite schema is broadcast-only (no `mode`/`artifact`
columns). If `e2e-katana-data` was created by a pre-refactor katana, wipe it
(`docker volume rm katana_e2e-katana-data` or similar) rather than reusing it.

## Steps

1. **Start the devnet** (creates the shared network):

   ```bash
   cd gnoland-test
   GNO_REPO_PATH=/path/to/gno ./bootstrap.sh      # one-time
   docker compose up -d
   ```

2. **Create a bad validator** so metrics exist:

   ```bash
   make scenario1        # onboard validator4 via GovDAO
   make scenario2        # stop it → it misses blocks
   ```

3. **Configure katana for broadcast** (from the repo root):

   ```bash
   cp deploy/e2e/katana.config.yaml.example deploy/e2e/katana.config.yaml
   ```

   Edit `deploy/e2e/katana.config.yaml` and set `gnokey.mnemonic` to a
   throwaway devnet test mnemonic (or leave it empty and pass
   `KATANA_GNOKEY_MNEMONIC` via the environment instead). The signing key's
   address **must** be funded in the devnet genesis and be a GovDAO member, or
   the broadcast will succeed but no proposal will be created.

   Export the required secrets/build settings:

   ```bash
   export KATANA_GNOKEY_PASSWORD=<keybase password>
   export KATANA_DISCORD_WEBHOOK=<discord webhook url>   # optional, notifications only
   export GNO_REF=master                                  # pin to the gno revision the devnet was built from
   ```

4. **Start the e2e stack**:

   ```bash
   docker compose -f docker-compose.e2e.yml up --build
   ```

5. **Observe**
   - gnomonitoring ingests `chain=local` and, after a while,
     `curl -s 'http://localhost:8989/api/reports/validators?chain=local' | jq`
     shows validator4's computed health score dropping;
   - katana logs the matched score-driven action, signs and broadcasts a
     GovDAO ejection proposal via the in-container `gnokey`, then polls for
     the vote outcome;
   - `curl -s http://localhost:8990/health` returns `ok`.

## Config

- gnomonitoring: [`gnomonitoring.config.yaml`](gnomonitoring.config.yaml) —
  points at the devnet by service name, Postgres on the `postgres` service.
- katana: `katana.config.yaml` (git-ignored, copied from
  [`katana.config.yaml.example`](katana.config.yaml.example)) — broadcast-only,
  `eject` action only, reading the public `reports_path` (no admin token).

## Going further

- **lower_vp/raise_vp:** add them to the katana config's `proposal.actions`
  and `action_priority` to exercise the VP-change actions alongside `eject`.
- **Pinning gno:** `GNO_REF` must match the gno revision the devnet
  (`gnoland-test/GNO_REPO_PATH`) was built from, or the ejection `.gno` script
  broadcast by `gnokey maketx run` may not match the on-chain realm ABI.

## Notes

- **Order matters:** start the devnet first; the external network must exist.
- **After a devnet reset**, restart gnomonitoring (it caches the last-seen block
  height): `docker compose -f docker-compose.e2e.yml restart gnomonitoring`.
- The gnomonitoring image tag/command may need tweaking — see the commented
  `command:` in the compose file if the container doesn't start the server.
