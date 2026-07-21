# e2e stack — katana on the local devnet, always via Docker

[`docker-compose.e2e.yml`](../../docker-compose.e2e.yml) runs **katana** wired
to the local gno.land devnet. There is a single tool (Docker Compose) and two
variants of the same command, depending on what you're testing:

| Testing... | Command | Backend |
|------------|---------|---------|
| Real broadcast / on-chain effect (non-regression) | `docker compose -f docker-compose.e2e.yml up --build` | real gnomonitoring + Postgres, real chain degradation (`make scenario2`) |
| katana's own logic: eject / lower_vp / raise_vp / dry_run / lifecycle gate / exceptions | `docker compose -f docker-compose.e2e.yml up --build --no-deps katana` | [`stub-gnomonitoring`](../../gnoland-test/stub-gnomonitoring/) on the host, fake deterministic scores |

`--no-deps` skips starting `gnomonitoring`/`postgres` (declared as a
`depends_on` for convenience in the first variant); katana then reaches the
host-run stub via `http://host.docker.internal:8989` (wired in the compose
file via `extra_hosts`).

The devnet itself is a separate layer, built and run from `gnoland-test/`
(not part of this compose file), which creates the external network
`gnoland-test_default` that `katana` joins either way.

```text
gnoland-test/ (layer 1, always)          docker-compose.e2e.yml (layer 2)
  validator / 2 / 3 / 4                    katana ──► /health :8990, gnokey
  (network gnoland-test_default)           + gnomonitoring+postgres (real path)
                                            OR host-run stub-gnomonitoring (fast path)
```

katana ships `gnokey` inside its image and imports a signing key from a
mnemonic into an in-container keybase (`/app/data/keybase`) at startup — there
is no host-run katana path anymore.

**Data volume:** the SQLite schema is broadcast-only (no `mode`/`artifact`
columns). If `e2e-katana-data` was created by a pre-refactor katana, wipe it
(`docker volume rm katana_e2e-katana-data` or similar) rather than reusing it.

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

## Setup (once)

1. **Start the devnet** with all 4 validators onboarded (creates the shared
   network + validator4):

   ```bash
   cd gnoland-test
   GNO_REPO_PATH=/path/to/gno ./bootstrap.sh      # one-time
   make full-reinit-4
   ```

2. **Provision katana's signing key** on the live devnet — required after
   *every* `full-reinit`/`full-reinit-4`, since a fresh genesis only funds
   `validator1-4`/`dev`/`v4op`, not this key:

   ```bash
   make add-key NAME=katana-e2e MNEMONIC="<your throwaway devnet mnemonic>"
   ```

   This funds the address from `dev` and adds it as a GovDAO T1 member (safe
   to re-run; skips steps already done). Without this, broadcasts fail with
   `unknown address error` and no proposal is ever created — no error is
   visible in Discord because katana only notifies on a *successful*
   broadcast.

3. **Configure katana**, from the repo root:

   - Real-broadcast path: `cp deploy/e2e/katana.config.yaml.example deploy/e2e/katana.config.yaml`
   - Stub path: `cp deploy/e2e/katana.config.stub.yaml.example deploy/e2e/katana.config.yaml`

   Both share the same `gnokey.mnemonic`/`key_name`/`password` slot — put the
   mnemonic from step 2 there directly (config-first: the file is git-ignored
   and this is a throwaway devnet key, so a plaintext password is fine). The
   `environment:` block in `docker-compose.e2e.yml` is commented out by
   default — only uncomment it and export `KATANA_GNOKEY_MNEMONIC`/
   `KATANA_GNOKEY_PASSWORD` if you'd rather not put secrets in the config file.

## Real broadcast path

```bash
cd gnoland-test && make scenario2        # stop validator4 -> real degradation
cd ..
export GNO_REF=master                    # must match GNO_REPO_PATH's gno revision
docker compose -f docker-compose.e2e.yml up --build
```

Watch it detect the score drop, broadcast a real ejection proposal, and poll
for the vote outcome:

```bash
curl -s 'http://localhost:8989/api/reports/validators?chain=local' | jq
docker logs -f katana-e2e
cd gnoland-test && make accept-govdao ID=<id>   # vote YES + execute
```

## Stub path (recommended for day-to-day testing)

```bash
cd gnoland-test
make stub-scenario NAME=eject     # or lower_vp / raise_vp / multi_eject / healthy
make stub-up                      # foreground, leave running — this is the host service katana talks to

# in another terminal, from the repo root:
docker compose -f docker-compose.e2e.yml up --build --no-deps katana
```

Switch scenarios any time with `make stub-scenario NAME=...` (no restart of
either side needed — katana picks it up on its next `poll_interval`). Flip
`dry_run: true` in `deploy/e2e/katana.config.yaml` and restart the `katana`
container to compare the rehearsal path (logs `[DRY-RUN]`, notifies with the
script + gnokey command, never calls `gnokey`) against a real broadcast.

```bash
docker logs -f katana-e2e
```

## Config

- `deploy/e2e/katana.config.yaml.example` — real gnomonitoring, `eject` only.
- `deploy/e2e/katana.config.stub.yaml.example` — stub gnomonitoring, all three
  actions (`eject`/`lower_vp`/`raise_vp`), short `cooldown`/`rejected_backoff`
  for fast iteration.
- `deploy/e2e/gnomonitoring.config.yaml` — only used by the real path; points
  at the devnet by service name, Postgres on the `postgres` service.

## Notes

- **Order matters:** start the devnet first; the external network must exist.
- **After a devnet reset**, re-run `make add-key` (step 2 above) — the fresh
  genesis doesn't carry the signing key forward — and, on the real path,
  restart gnomonitoring (it caches the last-seen block height):
  `docker compose -f docker-compose.e2e.yml restart gnomonitoring`.
- **Pinning gno:** `GNO_REF` must match the gno revision the devnet
  (`gnoland-test/GNO_REPO_PATH`) was built from, or the ejection `.gno` script
  broadcast by `gnokey maketx run` may not match the on-chain realm ABI.
- A broadcast **failure still counts as "last proposal"** for the lifecycle
  gate (avoids retry storms), so a mistake (e.g. an unprovisioned key) blocks
  re-testing the same validator for a full `cooldown`. Lower `cooldown` in
  `katana.config.yaml` while iterating, or delete the stale row from the
  SQLite `proposals` table if you need an immediate retry.
