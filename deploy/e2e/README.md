# e2e stack — gnomonitoring + katana on the local devnet

[`docker-compose.e2e.yml`](../../docker-compose.e2e.yml) runs **Postgres +
gnomonitoring + katana** wired to the local gno.land devnet, so you can watch
katana detect a bad validator and produce an ejection proposal end-to-end.

The devnet is a separate layer (it is built locally, not pulled). The three
layers share the devnet's Docker network `gnoland-test_default`.

```
gnoland-test/ (layer 1)          docker-compose.e2e.yml (layer 2)
  validator / 2 / 3      ◄─────    gnomonitoring (chain=local) ──► REST :8989
  tx-indexer, gnoweb               postgres
  (network gnoland-test_default)   katana (dry-run) ──► /health :8990, proposals
```

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

3. **Start the e2e stack** (from the repo root):

   ```bash
   docker compose -f docker-compose.e2e.yml up --build
   ```

4. **Observe**
   - gnomonitoring ingests `chain=local` and, after a while,
     `curl -s 'http://localhost:8989/missing_block?chain=local&period=current_week' | jq`
     lists validator4's missed blocks;
   - katana logs `ELIGIBLE …` and writes a dry-run proposal into the
     `e2e-katana-data` volume (`/app/data/proposals`), whose JSON contains the
     exact `gnokey maketx run` command and the ejection `.gno` script;
   - `curl -s http://localhost:8990/health` returns `ok`.

## Config

- gnomonitoring: [`gnomonitoring.config.yaml`](gnomonitoring.config.yaml) —
  points at the devnet by service name, Postgres on the `postgres` service.
- katana: [`katana.config.yaml`](katana.config.yaml) — dry-run, `missed_blocks`
  only (public endpoint, no admin token).

## Going further

- **critical_alerts:** enable it in the katana config and set
  `gnomonitoring.api_token` (Clerk) — the alerts endpoint is admin-protected.
- **Real broadcast + GovDAO vote:** the katana image has no `gnokey`, so the
  in-container run stays dry-run. To exercise the on-chain path, run katana **on
  the host** with the devnet keybase — see
  [`../../gnoland-test/KATANA-E2E.md`](../../gnoland-test/KATANA-E2E.md).

## Notes

- **Order matters:** start the devnet first; the external network must exist.
- **After a devnet reset**, restart gnomonitoring (it caches the last-seen block
  height): `docker compose -f docker-compose.e2e.yml restart gnomonitoring`.
- The gnomonitoring image tag/command may need tweaking — see the commented
  `command:` in the compose file if the container doesn't start the server.
