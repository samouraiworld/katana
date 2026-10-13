# Pin the compose project name so the devnet network is always
# "gnoland-test_default", regardless of the directory name. The gnomonitoring
# backend (backend/docker-compose.yml) attaches to that network by name, so a
# stable project name prevents DNS breakage ("no such host: validator") when
# this folder gets copied or renamed.
export COMPOSE_PROJECT_NAME := gnoland-test

DEV_HOME := $(CURDIR)/.gnokey-dev
DEV_PASS := devpassword123
RPC      := http://localhost:26658

# Live addresses (VALIDATOR4_ADDR/PUB, VALIDATOR4_OP_ADDR, DEV_ADDR) are no longer
# hardcoded: keys are regenerated on every fresh setup, so bootstrap.sh writes
# them to .devaddrs.mk (gitignored) and we include them here. The leading "-"
# means make won't fail if it's missing (run 'make full-reinit' to generate it).
-include .devaddrs.mk

# gnomonitoring backend Postgres - used to purge the local "dev" chain data on
# every reinit, so validator metrics / participation don't carry stale block
# heights from the previous chain instance.
PG_CONTAINER := gnomonitoring-postgres
PG_USER      := gnomonitoring
PG_DB        := gnomonitoring
DEV_CHAIN    := dev

.PHONY: help up up-validator4 ensure-bootstrap down reinit regen-genesis full-reinit full-reinit-4 wait-chain clean-db clean-all status logs \
        stop-val1 stop-val2 stop-val3 stop-val4 \
        start-val1 start-val2 start-val3 start-val4 \
        scenario1 scenario2 scenario2-restart scenario3 scenario3-restart \
        scenario4 scenario4-restart scenario5 scenario-mute add-key accept-govdao \
        stub-render stub-list stub-scenario stub-up

help:
	@echo "Chain lifecycle:"
	@echo "  make up                - start validator/validator2/validator3 + tx-indexer + gnoweb"
	@echo "  make up-validator4     - start validator4 (phase2 profile, needed for scenario1)"
	@echo "  make down              - stop and remove all containers"
	@echo "  make reinit            - wipe chain state + purge dev-chain DB rows, restart from existing genesis.json"
	@echo "  make regen-genesis     - delete + regenerate genesis.json (after editing generate-genesis.sh)"
	@echo "  make full-reinit       - regen-genesis + reinit + up (use after changing powers/params)"
	@echo "  make full-reinit-4     - full-reinit + validator4 onboarded via GovDAO (scenario1)"
	@echo "  make clean-db          - purge local dev-chain rows from the gnomonitoring DB (keeps Telegram subs)"
	@echo "  make clean-all         - nuke ALL generated state (keys/genesis/config) -> full regen on next full-reinit"
	@echo "  make status            - docker compose ps + height/catching_up per node"
	@echo "  make logs              - follow logs of all containers"
	@echo ""
	@echo "Per-validator control:"
	@echo "  make stop-val1..4      - docker compose stop validator[N]"
	@echo "  make start-val1..4     - docker compose start validator[N]"
	@echo ""
	@echo "Test scenarios (see README.md for details):"
	@echo "  make scenario1         - GovDAO: init DAO, register & onboard validator4"
	@echo "                           (targets proposal #0 - run 'make reinit' first"
	@echo "                           if this isn't the first proposal on the chain)"
	@echo "  make scenario2         - stop validator4 (downtime: WARNING then CRITICAL)"
	@echo "                           (run scenario1 first so validator4 is in the valset)"
	@echo "  make scenario2-restart - restart validator4 (expect RESOLVED)"
	@echo "  make scenario3         - stop validator2+validator3 (halt the chain -> stagnation)"
	@echo "  make scenario3-restart - restart validator2+validator3"
	@echo "  make scenario4         - stop ALL validators (RPC-error alert, LOG-ONLY)"
	@echo "  make scenario4-restart - restart all validators"
	@echo "  make scenario5         - GovDAO proposal voted NO (rejected-status path; fresh chain)"
	@echo "  make scenario-mute     - keep validator4 down to exercise the resend dedup"
	@echo ""
	@echo "katana signing key (broadcast e2e):"
	@echo "  make add-key NAME=<key> [MNEMONIC=\"24 words\"]"
	@echo "                         - import a key + fund it from dev + add it as a GovDAO"
	@echo "                           member (no genesis reinit). Put the printed MNEMONIC"
	@echo "                           into katana's gnokey.mnemonic."
	@echo ""
	@echo "Stub gnomonitoring (fake scores, no real chain degradation needed):"
	@echo "  make stub-list          - list available scenarios"
	@echo "  make stub-scenario NAME=<healthy|eject|lower_vp|raise_vp|multi_eject>"
	@echo "                           - render + activate a scenario (picked up on katana's"
	@echo "                             next poll_interval, server does not need a restart)"
	@echo "  make stub-up             - run the stub server (foreground) on :8989, serving"
	@echo "                             stub-gnomonitoring/scenarios/current.json. Point"
	@echo "                             katana's gnomonitoring.base_url at it (see"
	@echo "                             stub-gnomonitoring/config.katana.stub.yaml.example)."

# add-key: provision a katana-usable signing key on the LIVE devnet, WITHOUT a
# genesis reinit. Creating a GovDAO proposal requires membership (r/gov/dao
# PreCreateProposal panics for non-members), so this: (1) imports a key from a
# mnemonic — generated if MNEMONIC is not passed — into the dev keybase; (2)
# funds it from dev; (3) adds it as a T1 GovDAO member through an AddMember
# proposal that dev creates, votes YES on, and executes. (This devnet's GovDAO
# is initialized with only the T1 tier, so impl.AddMember — which targets T3 —
# is unusable; the proposal route via impl.NewAddMemberRequest is.)
# Put the printed MNEMONIC into katana's gnokey.mnemonic and set gnokey.key_name
# to NAME: katana imports it into its own keybase at startup (internal/keybase),
# so no keybase volume mount is needed. Requires the devnet running (make up).
#
#   make add-key NAME=katana                 # generate a fresh mnemonic + print it
#   make add-key NAME=katana MNEMONIC="..."  # use a mnemonic you supply
add-key:
	@test -n "$(NAME)" || { echo "usage: make add-key NAME=<key> [MNEMONIC=\"24 words\"]"; exit 1; }
	@set -e; \
	MN="$(MNEMONIC)"; \
	if gnokey list -home "$(DEV_HOME)" 2>/dev/null | grep -q " $(NAME) ("; then \
	  echo ">>> [1/3] key '$(NAME)' already in the keybase — skipping import (reusing it)"; \
	else \
	  if [ -z "$$MN" ]; then \
	    MN=$$(gnokey generate); \
	    echo ">>> generated mnemonic for '$(NAME)' — SAVE THIS (katana gnokey.mnemonic):"; \
	    echo "    $$MN"; \
	  fi; \
	  echo ">>> [1/3] importing key '$(NAME)' into $(DEV_HOME)"; \
	  printf '%s\n%s\n%s\n' "$$MN" "$(DEV_PASS)" "$(DEV_PASS)" \
	    | gnokey add --recover --insecure-password-stdin -home "$(DEV_HOME)" "$(NAME)"; \
	fi; \
	NEWADDR=$$(gnokey list -home "$(DEV_HOME)" | sed -n 's/.* $(NAME) (.*addr: \(g1[a-z0-9]*\).*/\1/p' | head -1); \
	test -n "$$NEWADDR" || { echo "!! could not resolve address for '$(NAME)'"; exit 1; }; \
	echo ">>> [2/3] funding $$NEWADDR from dev"; \
	echo "$(DEV_PASS)" | gnokey maketx send -send 100000000ugnot -to "$$NEWADDR" \
	  -gas-fee 1000000ugnot -gas-wanted 5000000 -broadcast -chainid dev -remote $(RPC) \
	  -home "$(DEV_HOME)" -insecure-password-stdin dev; \
	echo ">>> [3/3] ensuring $$NEWADDR is a GovDAO member"; \
	if gnokey query vm/qrender -remote $(RPC) -data 'gno.land/r/gov/dao/memberstore/v0:members' 2>/dev/null | grep -q "$$NEWADDR"; then \
	  echo "    already a GovDAO member — skipping"; \
	else \
	  echo "    proposing $$NEWADDR as a T1 member (dev creates the proposal, votes YES, executes)"; \
	  SCRIPTDIR=$$(mktemp -d); SCRIPT="$$SCRIPTDIR/addmember.gno"; \
	  printf 'package main\n\nimport (\n\t"gno.land/r/gov/dao"\n\t"gno.land/r/gov/dao/impl/v0"\n)\n\nfunc main(cur realm) {\n\tpr := impl.NewAddMemberRequest(cross(cur), address("%s"), "T1", "katana e2e signer")\n\tdao.MustCreateProposal(cross(cur), pr)\n}\n' "$$NEWADDR" > "$$SCRIPT"; \
	  OUT=$$(echo "$(DEV_PASS)" | gnokey maketx run -gas-fee 1000000ugnot -gas-wanted 100000000 -broadcast -chainid dev -remote $(RPC) -home "$(DEV_HOME)" -insecure-password-stdin dev "$$SCRIPT" 2>&1) || true; \
	  rm -rf "$$SCRIPTDIR"; \
	  echo "$$OUT" | grep -q '"type":"ProposalCreated"' || { echo "!! add-member proposal failed:"; echo "$$OUT" | tail -20; exit 1; }; \
	  PID=$$(echo "$$OUT" | grep -oE '"key":"id","value":"[0-9]+"' | grep -oE '[0-9]+' | head -1); \
	  echo "    proposal #$$PID created — voting YES with every known member + executing"; \
	  for M in dev katana; do \
	    echo "$(DEV_PASS)" | gnokey maketx call -pkgpath gno.land/r/gov/dao -func MustVoteOnProposalSimple -args "$$PID" -args YES -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast -chainid dev -remote $(RPC) -home "$(DEV_HOME)" -insecure-password-stdin $$M \
	      || echo "    ($$M vote skipped: not in keybase, already voted, or not a member yet)"; \
	  done; \
	  echo "$(DEV_PASS)" | gnokey maketx call -pkgpath gno.land/r/gov/dao -func ExecuteProposal -args "$$PID" -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast -chainid dev -remote $(RPC) -home "$(DEV_HOME)" -insecure-password-stdin dev; \
	fi; \
	echo ">>> done: '$(NAME)' = $$NEWADDR is funded + GovDAO member."; \
	echo ">>> in katana config: gnokey.key_name=$(NAME), gnokey.mnemonic=<the phrase above>."

# accept-govdao: drive a GovDAO proposal to acceptance on the devnet by voting
# YES with every T1 member in the dev keybase (dev + katana), then executing it.
# katana only PROPOSES in production (it never votes); this test-harness step is
# what confirms a proposal's end-to-end effect — e.g. that an accepted
# validator-ejection removes the validator from the consensus valset.
#
# BOTH members must vote YES: a single YES is only 2/3 (66.66%), which does NOT
# exceed the supermajority, so ExecuteProposal would fail with "didn't reach
# supermajority yet". Each vote is best-effort (a member that already voted is
# skipped); ExecuteProposal is strict and fails loud if the proposal is still not
# accepted (e.g. new GovDAO members added without their YES). Requires the devnet
# running and .gnokey-dev (bootstrap.sh / add-key).
#   make accept-govdao ID=2
accept-govdao:
	@test -n "$(ID)" || { echo "usage: make accept-govdao ID=<proposal-id>"; exit 1; }
	@set -e; \
	for M in dev katana; do \
	  echo ">>> voting YES on proposal #$(ID) as $$M"; \
	  echo "$(DEV_PASS)" | gnokey maketx call -pkgpath gno.land/r/gov/dao -func MustVoteOnProposalSimple \
	    -args "$(ID)" -args YES -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
	    -chainid $(DEV_CHAIN) -remote $(RPC) -home "$(DEV_HOME)" -insecure-password-stdin $$M \
	    || echo "    ($$M vote skipped: already voted or not a member)"; \
	done; \
	echo ">>> executing proposal #$(ID) as dev"; \
	echo "$(DEV_PASS)" | gnokey maketx call -pkgpath gno.land/r/gov/dao -func ExecuteProposal \
	  -args "$(ID)" -gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
	  -chainid $(DEV_CHAIN) -remote $(RPC) -home "$(DEV_HOME)" -insecure-password-stdin dev; \
	echo ">>> done: proposal #$(ID) accepted + executed."

# Guard: the generated .env (P2P seeds) and genesis.json are required for the
# chain to form. docker-compose has no default for SEEDS_*, so a missing .env
# would start validators with empty peers and the chain would silently never
# produce blocks. Bootstrap (idempotent) if either is missing.
ensure-bootstrap:
	@if [ ! -f .env ] || [ ! -f genesis.json ]; then \
		echo "ℹ️  .env/genesis.json missing — running bootstrap.sh first..."; \
		./bootstrap.sh; \
	fi

up: ensure-bootstrap
	docker compose up -d

up-validator4: ensure-bootstrap
	docker compose --profile phase2 up -d validator4

down:
	docker compose down

reinit: clean-db
	./reinit-chain.sh
	docker compose up -d

regen-genesis:
	rm -f genesis.json
	./bootstrap.sh

full-reinit: regen-genesis clean-db
	./reinit-chain.sh
	docker compose up -d

# wait-chain: block until the chain's RPC is up and has mined at least one
# block. 'docker compose up -d' returns as soon as containers are Started, but
# gnoland needs a few seconds to open its RPC port and produce the first
# block — calling gnokey (scenario1's GovDAO init) before that fails with
# "connection reset by peer", not a retryable/idempotent error.
wait-chain:
	@printf ">>> waiting for chain RPC to be ready"; \
	for i in $$(seq 1 60); do \
		h=$$(curl -s http://localhost:26658/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // empty' 2>/dev/null); \
		if [ -n "$$h" ] && [ "$$h" -ge 1 ] 2>/dev/null; then echo " ready (height $$h)"; exit 0; fi; \
		printf "."; sleep 1; \
	done; \
	echo " timeout after 60s — check 'docker compose logs validator'"; exit 1

# full-reinit-4: full-reinit alone does NOT bring validator4 up (it's phase2
# profile-gated, see up-validator4/docker-compose.yml) or onboard it as a
# valoper. This chains full-reinit -> wait-chain -> scenario1 (which itself
# depends on up-validator4) so a single command gets you a fresh chain with
# all 4 validators live and in the valset.
full-reinit-4: full-reinit wait-chain scenario1

# Purge the local "dev" chain rows from the gnomonitoring backend DB. Chain data
# only (participation, monikers, govdao proposals, aggregates, alert logs).
# Telegram subscriptions are intentionally KEPT so alerts are still delivered on
# the next test run. No-op (with a notice) if the Postgres container isn't up.
clean-db:
	@if docker ps --format '{{.Names}}' | grep -qx '$(PG_CONTAINER)'; then \
		echo "🧹 Purging chain '$(DEV_CHAIN)' data from DB '$(PG_DB)' (keeping Telegram subs)..."; \
		docker exec $(PG_CONTAINER) psql -U $(PG_USER) -d $(PG_DB) \
			-c "DELETE FROM daily_participations WHERE chain_id='$(DEV_CHAIN)';" \
			-c "DELETE FROM addr_monikers WHERE chain_id='$(DEV_CHAIN)';" \
			-c "DELETE FROM govdaos WHERE chain_id='$(DEV_CHAIN)';" \
			-c "DELETE FROM daily_participation_agregas WHERE chain_id='$(DEV_CHAIN)';" \
			-c "DELETE FROM alert_logs WHERE chain_id='$(DEV_CHAIN)';" \
		|| echo "⚠️  DB purge failed (continuing with chain reinit anyway)"; \
	else \
		echo "ℹ️  $(PG_CONTAINER) not running — skipping DB purge."; \
	fi

# Nuke ALL generated devnet state so the next 'make full-reinit' regenerates
# keys, accounts, genesis and config from scratch (fresh addresses everywhere).
# None of this is committed (see .gitignore). Uses a throwaway alpine container
# to delete files that docker may have created as root.
clean-all: clean-db
	docker compose --profile phase2 down -v 2>/dev/null || true
	@echo "🧨 Removing all generated devnet artifacts (keys, genesis, config, keybase)..."
	docker run --rm -v "$(CURDIR):/work" -w /work alpine sh -c \
		'rm -rf validator/gnoland-data validator2/gnoland-data validator3/gnoland-data validator4/gnoland-data \
		        validator/genesis.json validator2/genesis.json validator3/genesis.json validator4/genesis.json \
		        validator/config.toml validator2/config.toml validator3/config.toml validator4/config.toml \
		        .gnokey-dev genesis.json genesis_balances.txt config.toml .env .devaddrs.mk \
		        init-govdao.gno propose-validator4.gno'
	@echo "✅ Clean. Run 'make full-reinit' to regenerate everything from scratch."

status:
	docker compose ps
	@for port in 26658 26659 26660 26661; do \
		echo "--- localhost:$$port ---"; \
		curl -s http://localhost:$$port/status | jq -r '.result.sync_info | {latest_block_height, catching_up}' 2>/dev/null || echo "unreachable"; \
	done

logs:
	docker compose logs -f

stop-val1:
	docker compose stop validator
stop-val2:
	docker compose stop validator2
stop-val3:
	docker compose stop validator3
stop-val4:
	docker compose stop validator4

start-val1:
	docker compose start validator
start-val2:
	docker compose start validator2
start-val3:
	docker compose start validator3
start-val4:
	docker compose start validator4

scenario1: up-validator4
	@echo ">>> Step 1: initialize GovDAO (InitWithUsers)"
	@if gnokey query vm/qeval -remote $(RPC) \
		-data 'gno.land/r/gov/dao.AllowedDAOs()' 2>/dev/null | grep -q 'gno.land/r/gov/dao/impl/v0'; then \
		echo "    GovDAO already initialized on this chain (AllowedDAOs locked to impl/v0) - skipping."; \
		echo "    (InitWithUsers is one-shot per chain lifetime; run 'make reinit' first for a clean re-init)"; \
	else \
		echo "$(DEV_PASS)" | gnokey maketx run \
			-gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
			-chainid dev -remote $(RPC) \
			-home "$(DEV_HOME)" -insecure-password-stdin \
			dev init-govdao.gno; \
	fi
	@echo ">>> Step 2: validator4 pubkey (from committed secrets)"
	@echo "    address: $(VALIDATOR4_ADDR)"
	@echo "    pub_key: $(VALIDATOR4_PUB)"
	@echo ">>> Step 3: register validator4 as a valoper (signed by v4op operator account)"
	echo "$(DEV_PASS)" | gnokey maketx call \
		-pkgpath gno.land/r/gnops/valopers -func Register \
		-args "samourai-crew-4" \
		-args "Phase 2 test validator for gnomonitoring" \
		-args "on-prem" \
		-args $(VALIDATOR4_OP_ADDR) \
		-args $(VALIDATOR4_PUB) \
		-gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
		-chainid dev -remote $(RPC) \
		-home "$(DEV_HOME)" -insecure-password-stdin v4op
	@echo ">>> Step 4: create GovDAO proposal (expects #0)"
	echo "$(DEV_PASS)" | gnokey maketx run \
		-gas-fee 1000000ugnot -gas-wanted 100000000 -broadcast \
		-chainid dev -remote $(RPC) \
		-home "$(DEV_HOME)" -insecure-password-stdin \
		dev propose-validator4.gno
	@echo ">>> Step 5: vote YES on proposal #0"
	echo "$(DEV_PASS)" | gnokey maketx call \
		-pkgpath gno.land/r/gov/dao -func MustVoteOnProposalSimple \
		-args 0 -args YES \
		-gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
		-chainid dev -remote $(RPC) \
		-home "$(DEV_HOME)" -insecure-password-stdin dev
	@echo ">>> Step 6: execute proposal #0"
	echo "$(DEV_PASS)" | gnokey maketx call \
		-pkgpath gno.land/r/gov/dao -func ExecuteProposal \
		-args 0 \
		-gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
		-chainid dev -remote $(RPC) \
		-home "$(DEV_HOME)" -insecure-password-stdin dev
	@echo ">>> Step 7: verify validator4 joined the valset"
	gnokey query vm/qeval \
		-data 'gno.land/r/sys/validators/v0.IsValidator("$(VALIDATOR4_ADDR)")' \
		-remote $(RPC)
	curl -s http://localhost:26661/status | jq '.result.validator_info'

scenario2:
	docker compose stop validator4
	@echo "validator4 stopped. Watch for a WARNING alert (>=5 missed blocks),"
	@echo "then CRITICAL (>=30 missed blocks)."

scenario2-restart:
	docker compose start validator4
	@echo "validator4 restarted. Expect a RESOLVED alert once it participates again."

scenario3:
	docker compose stop validator2 validator3
	@echo "validator2+validator3 stopped (11/31 power left, below the 2/3 supermajority)."
	@echo "Watch gnoland_chain_current_height (should stop increasing) and the"
	@echo "global stagnation/RPC-error alert."

scenario3-restart:
	docker compose start validator2 validator3
	@echo "If the chain doesn't resume producing blocks within ~1-2 min, run 'make reinit'."

# scenario4: total RPC outage. Distinct from scenario3 (stagnation): there the
# chain is reachable but stuck; here EVERY RPC endpoint is down, so the backend's
# fallback client can't reach the node at all. This alert is LOG-ONLY: it does
# NOT write to alert_logs and does NOT hit webhooks (see gnovalidator_realtime.go).
scenario4:
	docker compose --profile phase2 stop validator validator2 validator3 validator4
	@echo "All validators stopped -> every RPC endpoint is unreachable."
	@echo "Expect a LOG-ONLY alert (no alert_logs row, no webhook):"
	@echo "  docker logs gnomonitoring-backend | grep 'Error when querying latest block height'"
	@echo "It fires once, then is suppressed for the RPC-error cooldown (~10 min)."

scenario4-restart:
	docker compose --profile phase2 start validator validator2 validator3 validator4
	@echo "Validators restarted. The monitor loop should resume fetching heights."

# scenario5: GovDAO rejection path. Mirrors scenario1 but votes NO and never
# executes, so the proposal is rejected instead of onboarding validator4. This
# exercises the GovDAO watcher's CheckProposalStatus (status -> rejected/expired).
# Run on a FRESH chain (targets proposal #0): 'make reinit && make scenario5'.
scenario5: up-validator4
	@echo ">>> Rejected-proposal scenario (FRESH chain expected: targets proposal #0)"
	@echo ">>> Step 1: initialize GovDAO (skipped if already initialized)"
	@if gnokey query vm/qeval -remote $(RPC) \
		-data 'gno.land/r/gov/dao.AllowedDAOs()' 2>/dev/null | grep -q 'gno.land/r/gov/dao/impl/v0'; then \
		echo "    GovDAO already initialized - skipping."; \
	else \
		echo "$(DEV_PASS)" | gnokey maketx run \
			-gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
			-chainid dev -remote $(RPC) \
			-home "$(DEV_HOME)" -insecure-password-stdin \
			dev init-govdao.gno; \
	fi
	@echo ">>> Step 2: register validator4 as a valoper (signed by v4op)"
	echo "$(DEV_PASS)" | gnokey maketx call \
		-pkgpath gno.land/r/gnops/valopers -func Register \
		-args "samourai-crew-4" \
		-args "Phase 2 test validator for gnomonitoring" \
		-args "on-prem" \
		-args $(VALIDATOR4_OP_ADDR) \
		-args $(VALIDATOR4_PUB) \
		-gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
		-chainid dev -remote $(RPC) \
		-home "$(DEV_HOME)" -insecure-password-stdin v4op
	@echo ">>> Step 3: create GovDAO proposal (expects #0)"
	echo "$(DEV_PASS)" | gnokey maketx run \
		-gas-fee 1000000ugnot -gas-wanted 100000000 -broadcast \
		-chainid dev -remote $(RPC) \
		-home "$(DEV_HOME)" -insecure-password-stdin \
		dev propose-validator4.gno
	@echo ">>> Step 4: vote NO on proposal #0 (rejection path - NOT executed)"
	echo "$(DEV_PASS)" | gnokey maketx call \
		-pkgpath gno.land/r/gov/dao -func MustVoteOnProposalSimple \
		-args 0 -args NO \
		-gas-fee 1000000ugnot -gas-wanted 50000000 -broadcast \
		-chainid dev -remote $(RPC) \
		-home "$(DEV_HOME)" -insecure-password-stdin dev
	@echo ">>> Done. Expect the GovDAO watcher to track it and mark it rejected/expired:"
	@echo "  docker exec gnomonitoring-postgres psql -U gnomonitoring -d gnomonitoring \\"
	@echo "    -c \"SELECT id, status FROM govdaos WHERE chain_id='dev';\""

# scenario-mute: NOT a separate chain action - it asserts the resend DEDUP
# (CLAUDE.md's "4 repeats -> MUTED" is outdated; the real rule is one alert per
# level per validator within the resend window: WARNING=6h, CRITICAL=24h, unless
# a RESOLVED was sent). Keep validator4 down across several alert-check cycles.
scenario-mute:
	docker compose stop validator4
	@echo "validator4 stopped. The FIRST WARNING/CRITICAL is written to alert_logs;"
	@echo "every later check within the resend window is SKIPPED (no new row)."
	@echo "Assert exactly ONE row per level:"
	@echo "  docker exec gnomonitoring-postgres psql -U gnomonitoring -d gnomonitoring \\"
	@echo "    -c \"SELECT level, count(*) FROM alert_logs WHERE chain_id='dev' AND addr='$(VALIDATOR4_ADDR)' GROUP BY level;\""
	@echo "And confirm the dedup log line appears:"
	@echo "  docker logs gnomonitoring-backend | grep 'dedup: skipping'"
	@echo "Restart with: make scenario2-restart"

# --- Stub gnomonitoring ------------------------------------------------------
# katana only ever talks to gnomonitoring via GET /api/reports/validators
# (see internal/gnomonitoring/client.go). This stub serves that exact JSON
# contract from a local file, so every score band (eject/lower_vp/raise_vp),
# every lifecycle-gate/exception edge case, and both dry_run values can be
# exercised deterministically and instantly, without waiting on real chain
# degradation (stopping containers, downtime windows, etc). It still needs
# the real validators to exist and be in the live valset (katana's safety
# check queries /validators over RPC) — so run this against a devnet started
# with 'make full-reinit-4', not standalone.
STUB_DIR  := stub-gnomonitoring
STUB_ADDR := :8989

stub-render:
	@$(STUB_DIR)/render-scenarios.sh

stub-list: stub-render
	@ls $(STUB_DIR)/scenarios/*.json.tmpl | xargs -n1 basename | sed 's/\.json\.tmpl$$//'

# make stub-scenario NAME=eject
stub-scenario: stub-render
	@test -n "$(NAME)" || { echo "usage: make stub-scenario NAME=<name> (see 'make stub-list')"; exit 1; }
	@test -f "$(STUB_DIR)/scenarios/$(NAME).json" || { echo "!! unknown scenario '$(NAME)' (see 'make stub-list')"; exit 1; }
	@cp "$(STUB_DIR)/scenarios/$(NAME).json" "$(STUB_DIR)/scenarios/current.json"
	@echo ">>> active scenario: $(NAME) — katana picks it up on its next poll_interval, no restart needed"

# Foreground server (like 'make logs'): run in its own terminal, switch
# scenarios from another one with 'make stub-scenario NAME=...'.
stub-up:
	@test -f "$(STUB_DIR)/scenarios/current.json" || $(MAKE) stub-scenario NAME=healthy
	cd $(STUB_DIR) && go run . -listen $(STUB_ADDR) -file scenarios/current.json
