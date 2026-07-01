# katana

**katana** builds GovDAO proposals to eject under-performing validators from a
[gno.land](https://gno.land) valset.

It reads validator health metrics from a running
[gnomonitoring](https://github.com/samouraiworld/gnomonitoring) instance,
evaluates each validator against a set of fully configurable conditions, and —
for any validator that meets them — produces a proposal to remove it from the
consensus validator set.

katana is multi-chain (testnet, betanet, mainnet, …) and is driven entirely by a
single `config.yaml`.

---

## How it works

```
gnomonitoring REST API ─┐
  /missing_block         │        ┌─ dry-run  → ./proposals/*.json  (default)
  /admin/alerts          ├─ katana ┤
gno.land RPC ────────────┘        └─ broadcast → gnokey maketx call (--broadcast)
  /validators (safety check)
```

1. **Collect metrics** — for each chain, katana asks gnomonitoring for the number
   of missed blocks and CRITICAL alerts per validator over the configured
   windows.
2. **Evaluate conditions** — each validator is checked against the enabled
   conditions (see below).
3. **Safety check** — before producing a proposal, katana confirms the validator
   is actually in the current consensus valset (via the node's `/validators`
   RPC). Skippable with `--skip-valset-check`.
4. **Produce the proposal**
   - **Dry-run (default):** writes a JSON artifact containing the proposal and
     the exact `gnokey` command that would submit it. Nothing is signed or sent.
   - **Broadcast (`--broadcast`):** signs and submits the proposal on-chain via
     the `gnokey` binary.

## Ejection conditions

A validator becomes eligible for an ejection proposal when the **enabled**
conditions are satisfied. Both conditions are independently toggleable and
combined with `require_all` (`true` = AND, `false` = OR).

| Condition        | Triggers when …                                                        |
|------------------|------------------------------------------------------------------------|
| `missed_blocks`  | missed blocks over `interval` is **strictly greater** than `max`       |
| `critical_alerts`| CRITICAL alerts over `interval` is **strictly greater** than `max`     |

- `missed_blocks.interval` is a gnomonitoring reporting window:
  `current_week`, `current_month`, `current_year`, `all_time`.
- `critical_alerts.interval` is a Go duration: `24h`, `168h` (7d), `720h` (30d), …

All thresholds, windows and the combination mode live in `config.yaml` and can
be overridden per chain.

## Requirements

- Go 1.23+
- A reachable [gnomonitoring](https://github.com/samouraiworld/gnomonitoring)
  backend (source of validator metrics).
- For `--broadcast` only: the [`gnokey`](https://docs.gno.land) binary and a
  keybase holding the proposing key.

## Install & build

```sh
git clone https://github.com/samouraiworld/katana
cd katana
make build          # produces ./katana
```

## Configuration

Copy the example and edit it:

```sh
cp config.yaml.example config.yaml
```

Everything is configured in `config.yaml`, including values usually passed as
environment variables. A few secrets/per-deployment values can be overridden
from the environment (each such field is annotated with `env:` in the example):

| Env var                   | Overrides                     |
|---------------------------|-------------------------------|
| `KATANA_LOG_LEVEL`        | `log_level`                   |
| `KATANA_DEFAULT_CHAIN`    | `default_chain`               |
| `KATANA_GNOMON_BASE_URL`  | `gnomonitoring.base_url`      |
| `KATANA_GNOMON_API_TOKEN` | `gnomonitoring.api_token`     |
| `KATANA_GNOKEY_BIN`       | `gnokey.bin`                  |
| `KATANA_GNOKEY_KEY`       | `gnokey.key_name`             |
| `KATANA_GNOKEY_HOME`      | `gnokey.home`                 |
| `KATANA_GNOKEY_PASSWORD`  | `gnokey.password`             |

See [`config.yaml.example`](config.yaml.example) for the full annotated schema.

> **Note on the proposal target.** The realm path, function and argument
> templates under `proposal:` are configurable because they differ between
> networks. Verify them against each chain before enabling `--broadcast`.

## Usage

```sh
# Dry-run against the default chain — writes proposals to ./proposals/
./katana --config config.yaml

# A specific chain
./katana --chain betanet

# Every enabled chain
./katana --all-chains

# Restrict to one validator (by address or moniker)
./katana --validator g1abc...xyz

# Sign and submit on-chain (requires gnokey + a configured key)
./katana --chain betanet --broadcast
```

### Flags

| Flag                  | Description                                                        |
|-----------------------|--------------------------------------------------------------------|
| `--config`            | Path to `config.yaml` (default `config.yaml`).                     |
| `--chain`             | Chain key to evaluate (defaults to `default_chain`).              |
| `--all-chains`        | Evaluate every enabled chain.                                      |
| `--validator`         | Restrict evaluation to a single validator address or moniker.     |
| `--reason`            | Override the reason recorded on generated proposals.              |
| `--broadcast`         | Sign and submit on-chain via gnokey (default: dry-run).           |
| `--skip-valset-check` | Do not verify the target is in the current consensus valset.      |

## Project layout

```
cmd/katana/            CLI entrypoint and orchestration
internal/config/       config.yaml loading, validation, env overrides
internal/gnomonitoring/ REST client for validator metrics
internal/gnoland/      gno.land RPC queries (consensus valset)
internal/rules/        eligibility evaluation
internal/proposal/     proposal rendering, dry-run output, gnokey broadcast
```

## Safety

- **Dry-run is the default.** Nothing is signed or broadcast unless
  `--broadcast` is passed explicitly.
- katana verifies the target is in the live valset before proposing, unless
  `--skip-valset-check` is set.
- `config.yaml` and the generated `proposals/` directory are git-ignored, as
  they may contain secrets or sensitive operational data.

## License

See [LICENSE](LICENSE).
