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
