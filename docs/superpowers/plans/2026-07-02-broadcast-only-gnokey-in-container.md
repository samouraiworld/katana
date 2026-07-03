# Broadcast-only katana with an embedded gnokey — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the dry-run mode from katana entirely (broadcast-only), ship the `gnokey` binary inside the katana container, and let katana provision a signing key from a config mnemonic so the e2e devnet run produces a real GovDAO proposal.

**Architecture:** katana stays a config-driven daemon. `proposal.Broadcast` becomes the only submission path (`DryRun` deleted). A new leaf package `internal/keybase` imports a mnemonic into an on-disk gnokey keybase at startup (idempotent, optional). The Dockerfile gains a pinned gnokey build stage. The e2e stack switches to broadcast; the host-run broadcast config is deleted.

**Tech Stack:** Go 1.25 (CGO disabled), `modernc.org/sqlite`, `gnokey` (external binary from github.com/gnolang/gno), Docker multi-stage build, gnoland-test devnet.

## Global Constraints

- Language for all identifiers, comments, docs: **English**.
- Binary builds `CGO_ENABLED=0` (pure Go). No new cgo dependency.
- Secrets never required in plaintext YAML: every secret/overridable field has a `KATANA_*` env override in `applyEnvOverrides`.
- Config precedence is unchanged: YAML < env override < defaults, then `validate()`. Use `Runtime(chainID)` for resolved per-chain settings.
- `gnokey` merges **field-by-field** per chain; `conditions`/`proposal` replace the whole block; `exceptions` are unioned.
- The SQLite e2e DB is a throwaway volume: a **fresh schema** is acceptable, no incremental migration.
- After every task: `make vet` and `make test` must pass (Go will not compile with dangling references, so each task must leave the tree building).
- Do NOT sign commits as co-authored; author is the user only.

---

### Task 1: Make `produce()` broadcast-only in the daemon

Remove the dry-run branch so every eligible validator is broadcast. Leaves `config.Runtime.Mode`, `proposal.DryRun`, and the `store` mode/artifact fields in place for now (removed in later tasks) so the tree keeps compiling.

**Files:**
- Modify: `internal/daemon/cycle.go` (`produce`, lines ~99-158; comment line 97)

**Interfaces:**
- Consumes: `proposal.Build`, `Built.Broadcast(ctx, gk)`, `store.RecordProposal`, `d.dispatch`.
- Produces: `produce(ctx, rt, addr, moniker, reason)` unchanged signature; always broadcasts.

- [ ] **Step 1: Rewrite `produce` to always broadcast**

Replace the whole body of `produce` (from the comment above it through the closing brace) with:

```go
// produce builds, signs, and broadcasts an ejection proposal, records it, and
// notifies on a successful broadcast.
func (d *Daemon) produce(ctx context.Context, rt config.Runtime, addr, moniker, reason string) {
	// A valset ejection is keyed by the operator address, but metrics are keyed
	// by the signing address. A missing mapping is fatal for the broadcast (we
	// would eject an unknown operator).
	operator := addr
	if op, ok := d.resolverFor(rt).OperatorFor(ctx, addr); ok {
		operator = op
	} else {
		d.logger.Printf("[%s][eval] no operator mapping for signing address %s; skipping broadcast", rt.Key, addr)
		return
	}

	built := proposal.Build(proposal.Target{
		ChainKey:        rt.Key,
		ChainID:         rt.ChainID,
		RPC:             rt.RPCEndpoint,
		Moniker:         moniker,
		Address:         addr,
		OperatorAddress: operator,
		Reason:          reason,
	}, rt.Proposal, rt.Gnokey)

	rec := store.ProposalRecord{
		CreatedAt: d.now(), Chain: rt.Key, ChainID: rt.ChainID, Address: addr,
		Moniker: moniker, Reason: reason, Title: built.Title,
		RealmPath: built.RealmPath, Function: built.Function, Args: built.Args,
	}

	out, err := built.Broadcast(ctx, rt.Gnokey)
	rec.Output = out
	rec.Success = err == nil
	if err == nil {
		rec.Status = store.StatusPending
		rec.OnchainID = govdao.ExtractProposalID(out)
	}
	id, _ := d.st.RecordProposal(ctx, rec)
	if err != nil {
		d.logger.Printf("[%s][eval] broadcast failed for %s: %v", rt.Key, addr, err)
		return
	}
	d.logger.Printf("[%s][eval] BROADCAST ok for %s (proposal id=%q)", rt.Key, addr, rec.OnchainID)
	d.dispatch(ctx, notify.EventSent, rt, id, addr, moniker, rec.OnchainID)
}
```

- [ ] **Step 2: Build to verify it compiles**

Run: `go build ./...`
Expected: builds (unused `rec.Mode`/`rec.Artifact` fields simply not set; `Runtime.Mode` now unread — still compiles because it is a struct field, not a variable).

- [ ] **Step 3: Run the daemon package tests**

Run: `go test ./internal/daemon/...`
Expected: PASS (behaviour for the broadcast path is unchanged).

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/cycle.go
git commit -m "daemon: make proposal production broadcast-only"
```

---

### Task 2: Delete `DryRun` from the proposal package

**Files:**
- Modify: `internal/proposal/proposal.go` (delete `DryRun`, lines ~137-181; package doc lines 1-2; remove now-unused helpers)
- Modify: `internal/proposal/proposal_test.go` (drop any DryRun test)

**Interfaces:**
- Produces: `proposal` package no longer exposes `Built.DryRun`. `Build`, `Broadcast`, `callArgs`, `runArgs`, `commonArgs` unchanged.

- [ ] **Step 1: Check what references DryRun and the helpers**

Run: `grep -rn "DryRun\|shellJoin\|func safe(" internal cmd`
Expected: after Task 1, `DryRun` is referenced only in `proposal.go` and its test. Note whether `shellJoin`/`safe` are used elsewhere (they are only used by `DryRun`).

- [ ] **Step 2: Delete the `DryRun` method**

Remove the entire `DryRun` method (from its doc comment `// DryRun writes...` through its closing brace).

- [ ] **Step 3: Delete helpers that only `DryRun` used**

Delete `func shellJoin(...)` and `func safe(...)` (only referenced by `DryRun`). Keep `goInner`, `cloneVars`, `render` (used by `Build`). Update the package doc comment lines 1-2 to drop "write it out for manual review (dry-run) or":

```go
// Package proposal builds a valset-ejection GovDAO proposal and signs and
// broadcasts it with gnokey.
```

- [ ] **Step 4: Remove any DryRun test**

In `internal/proposal/proposal_test.go`, delete tests that call `DryRun` or assert on written artifacts. Keep `Build`/`Broadcast`/template tests.

- [ ] **Step 5: Build and test**

Run: `go build ./... && go test ./internal/proposal/...`
Expected: builds; PASS. If the compiler reports `shellJoin`/`safe`/`goInner` unused, remove exactly the unused ones and re-run.

- [ ] **Step 6: Commit**

```bash
git add internal/proposal/
git commit -m "proposal: remove dry-run artifact writer"
```

---

### Task 3: Remove `mode`/`output_dir` from config

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Proposal` no longer has `Mode` or `OutputDir`. `config.Runtime` no longer has `Mode`. `ChainConfig` no longer has `Mode`. Broadcast is implicit and always on.

- [ ] **Step 1: Remove the fields**

- In `Proposal` struct: delete `Mode string \`yaml:"mode"\`` (line 124) and `OutputDir string \`yaml:"output_dir"\`` (line 140).
- In `ChainConfig`: delete `Mode string \`yaml:"mode"\`` (line 190).
- In `Runtime`: delete `Mode string` (line 207).
- In `applyDefaults`: delete `setIfEmpty(&c.Proposal.Mode, "dry-run")` (line 282) and `setIfEmpty(&c.Proposal.OutputDir, "./data/proposals")` (line 287).

- [ ] **Step 2: Replace the mode resolution in `Runtime` with a broadcast-only key check**

Replace the block (current lines 356-362):

```go
	mode := c.Proposal.Mode
	if ch.Mode != "" {
		mode = ch.Mode
	}
	if mode != "dry-run" && mode != "broadcast" {
		return Runtime{}, fmt.Errorf("mode %q is invalid (want \"dry-run\" or \"broadcast\")", mode)
	}
```

with nothing (delete it). Then change the later gnokey key check (current lines 389-391):

```go
	if mode == "broadcast" && gk.KeyName == "" {
		return Runtime{}, fmt.Errorf("gnokey.key_name is required when mode is broadcast")
	}
```

to always require a key (katana can only broadcast now):

```go
	if gk.KeyName == "" {
		return Runtime{}, fmt.Errorf("gnokey.key_name is required")
	}
```

- [ ] **Step 3: Drop `Mode` from the returned `Runtime` literal**

In the `return Runtime{...}` at the end of `Runtime`, delete the `Mode: mode,` line.

- [ ] **Step 4: Update the package doc comment**

Line 8-9 mentions "conditions, proposal, gnokey, mode, poll interval". Change to drop "mode":

```go
// Many blocks (conditions, proposal, gnokey, poll interval) have a global
// default and an optional per-chain override. Use Runtime(chainID) to obtain the
```

- [ ] **Step 5: Fix the config test**

In `internal/config/config_test.go`, the fixture YAML has `mode: dry-run` (line ~24) and an assertion `if rt.Mode != "dry-run"` (lines ~71-72). Remove the `mode:` line from the fixture, add a `gnokey:` block with a `key_name` so `Runtime` validation passes, and replace the mode assertion with a broadcast-relevant one. Concretely, ensure the fixture chain has a resolvable key:

```yaml
gnokey:
  key_name: "test"
```

and delete the mode assertion lines:

```go
	if rt.Mode != "dry-run" {
		t.Errorf("test mode = %s, want dry-run", rt.Mode)
	}
```

- [ ] **Step 6: Build and test**

Run: `go build ./... && go test ./internal/config/...`
Expected: builds; PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config/
git commit -m "config: drop dry-run mode and output_dir (broadcast-only)"
```

---

### Task 4: Remove `Mode`/`Artifact` from the store (fresh schema)

**Files:**
- Modify: `internal/store/store.go` (`ProposalRecord`)
- Modify: `internal/store/sqlite.go` (schema, insert, cols, scan)
- Modify: `internal/store/store_test.go` (drop Mode/Artifact assertions)

**Interfaces:**
- Produces: `store.ProposalRecord` without `Mode` and `Artifact`. The `proposals` table has no `mode`/`artifact` columns.

- [ ] **Step 1: Remove the struct fields**

In `internal/store/store.go`, delete from `ProposalRecord`:

```go
	Mode      string // dry-run | broadcast
```
and
```go
	Artifact  string // dry-run file path
```

Update the comment on `Status` (line 57) from `("" for dry-run)` to `("" until broadcast succeeds)`.

- [ ] **Step 2: Update the SQLite schema**

In `internal/store/sqlite.go` `migrate()`, replace the `proposals` table definition with (no `mode`, no `artifact`):

```sql
CREATE TABLE IF NOT EXISTS proposals (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at          TEXT    NOT NULL,
	chain               TEXT    NOT NULL,
	chain_id            TEXT    NOT NULL,
	address             TEXT    NOT NULL,
	moniker             TEXT    NOT NULL,
	reason              TEXT    NOT NULL,
	title               TEXT    NOT NULL,
	realm_path          TEXT    NOT NULL,
	function            TEXT    NOT NULL,
	args                TEXT    NOT NULL,
	output              TEXT    NOT NULL,
	success             INTEGER NOT NULL,
	onchain_proposal_id TEXT    NOT NULL DEFAULT '',
	status              TEXT    NOT NULL DEFAULT '',
	resolved_at         TEXT    NOT NULL DEFAULT ''
);
```

- [ ] **Step 3: Update the INSERT**

Replace `RecordProposal`'s statement (lines 108-115) with:

```go
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO proposals
		 (created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success, onchain_proposal_id, status, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmtTime(r.CreatedAt), r.Chain, r.ChainID, r.Address, r.Moniker,
		r.Reason, r.Title, r.RealmPath, r.Function, string(args), r.Output,
		boolInt(r.Success), r.OnchainID, r.Status, fmtTime(r.ResolvedAt),
	)
```

- [ ] **Step 4: Update `proposalCols` and `scanProposal`**

Replace `proposalCols` (line 146):

```go
const proposalCols = `id, created_at, chain, chain_id, address, moniker, reason, title, realm_path, function, args, output, success, onchain_proposal_id, status, resolved_at`
```

Replace the `rows.Scan(...)` in `scanProposal` (lines 156-158):

```go
	if err := rows.Scan(&r.ID, &created, &r.Chain, &r.ChainID, &r.Address, &r.Moniker,
		&r.Reason, &r.Title, &r.RealmPath, &r.Function, &argsJSON,
		&r.Output, &success, &r.OnchainID, &r.Status, &resolvedAt); err != nil {
		return r, err
	}
```

- [ ] **Step 5: Fix the store test**

In `internal/store/store_test.go`, remove any `Mode:`/`Artifact:` fields from `ProposalRecord` literals and any assertions reading them.

Run: `grep -n "Mode\|Artifact" internal/store/store_test.go` and edit each hit.

- [ ] **Step 6: Build and test**

Run: `go build ./... && go test ./internal/store/...`
Expected: builds; PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store/
git commit -m "store: drop mode/artifact columns (broadcast-only, fresh schema)"
```

---

### Task 5: Re-document the lifecycle default branch

Behaviour is unchanged; only the comment and a test name are misleading now.

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (comment lines 63-65)
- Modify: `internal/lifecycle/lifecycle_test.go` (case names lines 59, 64)

- [ ] **Step 1: Update the comment**

Replace the `default:` comment (lines 63-65) with:

```go
	default:
		// Failed broadcast or a proposal not yet promoted to an on-chain status:
		// apply the cooldown floor from the last proposal's creation time so a
		// failing broadcast is not retried every poll interval.
```

- [ ] **Step 2: Rename the test cases**

In `internal/lifecycle/lifecycle_test.go`, rename:
- `"dry-run within cooldown is blocked"` → `"empty-status within cooldown is blocked"`
- `"dry-run after cooldown is allowed"` → `"empty-status after cooldown is allowed"`

- [ ] **Step 3: Test**

Run: `go test ./internal/lifecycle/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/lifecycle/
git commit -m "lifecycle: clarify cooldown-floor branch is for failed broadcasts"
```

---

### Task 6: Add `gnokey.mnemonic` to config + env override

**Files:**
- Modify: `internal/config/config.go` (`Gnokey` struct, `mergedWith`, `applyEnvOverrides`)
- Modify: `internal/config/config_test.go` (add an env-override assertion)

**Interfaces:**
- Produces: `config.Gnokey.Mnemonic string`, merged field-by-field; env `KATANA_GNOKEY_MNEMONIC` overrides the global value.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestGnokeyMnemonicEnvOverride(t *testing.T) {
	t.Setenv("KATANA_GNOKEY_MNEMONIC", "word1 word2 word3")
	c := &Config{}
	c.applyEnvOverrides()
	if c.Gnokey.Mnemonic != "word1 word2 word3" {
		t.Fatalf("mnemonic = %q, want the env value", c.Gnokey.Mnemonic)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/config/ -run TestGnokeyMnemonicEnvOverride`
Expected: FAIL to compile — `Mnemonic` is not a field of `Gnokey`.

- [ ] **Step 3: Add the field, merge, and override**

In the `Gnokey` struct add:

```go
	Mnemonic  string `yaml:"mnemonic"`
```

In `mergedWith`, before `return g`, add:

```go
	if o.Mnemonic != "" {
		g.Mnemonic = o.Mnemonic
	}
```

In `applyEnvOverrides`, next to the other gnokey overrides, add:

```go
	setStr(&c.Gnokey.Mnemonic, "KATANA_GNOKEY_MNEMONIC")
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/config/ -run TestGnokeyMnemonicEnvOverride`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "config: add gnokey.mnemonic with KATANA_GNOKEY_MNEMONIC override"
```

---

### Task 7: New `internal/keybase` package to import the mnemonic

Idempotent: no-op when the mnemonic is empty or the key already exists. Exec is injected so it is unit-testable without a real gnokey.

**Files:**
- Create: `internal/keybase/keybase.go`
- Create: `internal/keybase/keybase_test.go`

**Interfaces:**
- Produces:
  - `type Runner func(ctx context.Context, bin, stdin string, args ...string) (string, error)`
  - `func Ensure(ctx context.Context, gk config.Gnokey, run Runner) error`
  - `Ensure` with a nil `run` uses the real exec runner.

- [ ] **Step 1: Write the failing test**

Create `internal/keybase/keybase_test.go`:

```go
package keybase

import (
	"context"
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

func TestEnsureNoMnemonicIsNoop(t *testing.T) {
	called := false
	run := func(ctx context.Context, bin, stdin string, args ...string) (string, error) {
		called = true
		return "", nil
	}
	if err := Ensure(context.Background(), config.Gnokey{KeyName: "dev"}, run); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if called {
		t.Fatal("runner must not be called when mnemonic is empty")
	}
}

func TestEnsureImportsWhenKeyAbsent(t *testing.T) {
	var addStdin string
	var sawAdd bool
	run := func(ctx context.Context, bin, stdin string, args ...string) (string, error) {
		switch args[0] {
		case "list":
			return "no keys\n", nil // key "dev" absent
		case "add":
			sawAdd = true
			addStdin = stdin
			return "", nil
		}
		return "", nil
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", Password: "pw", Mnemonic: "a b c", Home: "/tmp/kb"}
	if err := Ensure(context.Background(), gk, run); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !sawAdd {
		t.Fatal("expected an add invocation")
	}
	if !strings.Contains(addStdin, "a b c") || !strings.Contains(addStdin, "pw") {
		t.Fatalf("add stdin missing mnemonic/password: %q", addStdin)
	}
}

func TestEnsureSkipsWhenKeyPresent(t *testing.T) {
	sawAdd := false
	run := func(ctx context.Context, bin, stdin string, args ...string) (string, error) {
		if args[0] == "list" {
			return "dev (address: g1...)\n", nil // key present
		}
		if args[0] == "add" {
			sawAdd = true
		}
		return "", nil
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", Mnemonic: "a b c"}
	if err := Ensure(context.Background(), gk, run); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if sawAdd {
		t.Fatal("must not re-add an existing key")
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/keybase/`
Expected: FAIL to compile — package does not exist.

- [ ] **Step 3: Implement the package**

Create `internal/keybase/keybase.go`:

```go
// Package keybase provisions the signing key katana broadcasts with. When a
// mnemonic is configured, it imports the derived key into the gnokey keybase at
// startup (idempotent); with no mnemonic it is a no-op and katana relies on a
// pre-provisioned keybase (the production path).
package keybase

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/samouraiworld/katana/internal/config"
)

// Runner executes a gnokey subcommand, optionally feeding stdin, and returns the
// combined output. It is injected so Ensure is testable without a real gnokey.
type Runner func(ctx context.Context, bin, stdin string, args ...string) (string, error)

func execRunner(ctx context.Context, bin, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// Ensure imports gk.Mnemonic into the keybase at gk.Home under gk.KeyName unless
// the key is already present. It is a no-op when gk.Mnemonic is empty.
func Ensure(ctx context.Context, gk config.Gnokey, run Runner) error {
	if strings.TrimSpace(gk.Mnemonic) == "" {
		return nil
	}
	if run == nil {
		run = execRunner
	}
	if gk.KeyName == "" {
		return fmt.Errorf("keybase: gnokey.key_name is required to import a mnemonic")
	}

	if out, err := run(ctx, bin(gk), "", homeArgs(gk, "list")...); err == nil && hasKey(out, gk.KeyName) {
		return nil
	}

	// `gnokey add --recover --insecure-password-stdin <name>` reads the mnemonic
	// then the password (twice) from stdin. Order/flags are validated against the
	// pinned gnokey in Task 11.
	stdin := gk.Mnemonic + "\n" + gk.Password + "\n" + gk.Password + "\n"
	args := homeArgs(gk, "add", "--recover", "--insecure-password-stdin", gk.KeyName)
	if out, err := run(ctx, bin(gk), stdin, args...); err != nil {
		return fmt.Errorf("keybase: importing %q: %v: %s", gk.KeyName, err, out)
	}
	return nil
}

func bin(gk config.Gnokey) string {
	if gk.Bin != "" {
		return gk.Bin
	}
	return "gnokey"
}

// homeArgs prepends the subcommand and appends -home when configured. gnokey
// takes -home as a global flag, so placement after the subcommand is fine.
func homeArgs(gk config.Gnokey, sub ...string) []string {
	args := append([]string{}, sub...)
	if gk.Home != "" {
		args = append(args, "-home", gk.Home)
	}
	return args
}

// hasKey reports whether gnokey's `list` output contains a key named name as a
// whole word (avoids matching a substring of another key or an address).
func hasKey(listOutput, name string) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(listOutput)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/keybase/`
Expected: PASS (all three cases).

- [ ] **Step 5: Commit**

```bash
git add internal/keybase/
git commit -m "keybase: import signing key from mnemonic at startup (idempotent)"
```

---

### Task 8: Provision the keybase in `main.go` at startup

**Files:**
- Modify: `cmd/katana/main.go`

**Interfaces:**
- Consumes: `config.Config.EnabledChains()`, `config.Config.Runtime(id)`, `keybase.Ensure`.

- [ ] **Step 1: Call `keybase.Ensure` for each enabled chain before starting the daemon**

After the store is opened (`defer st.Close()`) and before `daemon.New`, add:

```go
	// Provision signing keys from any configured mnemonic before broadcasting.
	for _, id := range cfg.EnabledChains() {
		rt, err := cfg.Runtime(id)
		if err != nil {
			fatal("chain %q: %v", id, err)
		}
		if err := keybase.Ensure(ctx, rt.Gnokey, nil); err != nil {
			fatal("keybase: %v", err)
		}
	}
```

Add the import `"github.com/samouraiworld/katana/internal/keybase"`. Note this must be placed after `ctx` is created (move the `signal.NotifyContext` block above it, or reference `context.Background()` — prefer moving the ctx creation up so the same cancellable context is used).

- [ ] **Step 2: Update the package doc comment**

Lines 6-8 mention "per-chain run mode (dry-run vs broadcast)". Replace with:

```go
// Behaviour is entirely config-driven (config.yaml); katana always signs and
// broadcasts ejection proposals. The only flag is the config path.
```

- [ ] **Step 3: Build and vet**

Run: `go build ./... && make vet`
Expected: builds; no vet errors.

- [ ] **Step 4: Full test suite**

Run: `make test`
Expected: PASS across all packages.

- [ ] **Step 5: Commit**

```bash
git add cmd/katana/main.go
git commit -m "katana: provision signing keybase from mnemonic at startup"
```

---

### Task 9: Build gnokey into the container image

**Files:**
- Modify: `Dockerfile`

**Interfaces:**
- Produces: `/usr/local/bin/gnokey` present in the runtime image; `GNO_REF` build-arg controls the pinned gno version.

- [ ] **Step 1: Verify the go-install path for gnokey**

Run locally (or in a scratch container):
`go install github.com/gnolang/gno/gno.land/cmd/gnokey@latest` and confirm it produces a `gnokey` binary.
- If the module path differs, capture the correct one from the gno repo layout.
- Fallback if `go install` is not viable: `git clone --depth 1 --branch $GNO_REF https://github.com/gnolang/gno && cd gno/gno.land && go build -o /out/gnokey ./cmd/gnokey`.

Record the working command; use it in Step 2.

- [ ] **Step 2: Add the gnokey build stage and copy it into the runtime image**

Add near the top of `Dockerfile`, after the katana build stage:

```dockerfile
# ── gnokey stage ──────────────────────────────────────────────────────────────
# Pin to the SAME gno revision the target network was built from. For the e2e
# devnet this must match gnoland-test's GNO_REPO_PATH commit; for a real chain,
# use the network's released gno tag.
ARG GNO_REF=master
FROM golang:1.25-alpine AS gnokey
RUN apk add --no-cache git
RUN CGO_ENABLED=0 go install github.com/gnolang/gno/gno.land/cmd/gnokey@${GNO_REF}
```

In the runtime stage, after the katana binary copy, add:

```dockerfile
COPY --from=gnokey /go/bin/gnokey /usr/local/bin/gnokey
```

Update the `mkdir` comment (it says "dry-run proposals"):

```dockerfile
# Data dir for the SQLite database and the gnokey keybase. Created with the right
# ownership so a fresh named volume mounted here is writable by the katana user.
```

- [ ] **Step 3: Build the image**

Run: `docker build --build-arg GNO_REF=<devnet-commit> -t katana:latest .`
Expected: build succeeds; final image contains both binaries.

- [ ] **Step 4: Confirm gnokey is present and runnable**

Run: `docker run --rm --entrypoint gnokey katana:latest --help`
Expected: gnokey usage printed (exit 0).

- [ ] **Step 5: Commit**

```bash
git add Dockerfile
git commit -m "docker: build and ship gnokey in the katana image (GNO_REF pinned)"
```

---

### Task 10: Switch the e2e stack to broadcast

**Files:**
- Modify: `deploy/e2e/katana.config.yaml.example` (committed template — the live `katana.config.yaml` is git-ignored via `/deploy/**/*.config.yaml`)
- Modify: `docker-compose.e2e.yml`
- Delete on disk: `deploy/e2e/katana.broadcast.host.yaml` (untracked + git-ignored via `*.host.yaml` — never committed, so `rm`, not `git rm`)
- Modify: `deploy/e2e/README.md`

**Interfaces:**
- Produces: a committed broadcast **template** (`*.example`) with an empty mnemonic; the operator copies it to the git-ignored `katana.config.yaml` and fills in the test mnemonic (or passes `KATANA_GNOKEY_MNEMONIC`). docker-compose mounts the live file.

- [ ] **Step 1: Rewrite `deploy/e2e/katana.config.yaml.example` as broadcast-only**

Replace its contents with (mnemonic left **empty** in the committed template — the real value goes in the git-ignored live copy or via env):

```yaml
# katana e2e config TEMPLATE — BROADCAST against the local devnet, in-container.
#
# Copy this to `katana.config.yaml` (git-ignored) and set gnokey.mnemonic to your
# throwaway devnet test key, OR leave it empty here and pass the key via the env
# var KATANA_GNOKEY_MNEMONIC. The password comes from KATANA_GNOKEY_PASSWORD.
#
# gnokey is shipped inside the katana image (Dockerfile GNO_REF). katana imports
# the signing key into /app/data/keybase at startup.
#
# The signing key's address MUST be funded in the devnet genesis AND be a GovDAO
# member, otherwise the broadcast succeeds but no proposal is created.

log_level: info
poll_interval: 15s

gnomonitoring:
  base_url: "http://gnomonitoring:8989"
  timeout_seconds: 10

valopers_realm: "gno.land/r/gnops/valopers"

gnokey:
  bin: "gnokey"
  key_name: "katana-e2e"
  home: "/app/data/keybase"
  mnemonic: "<24-word devnet test mnemonic>"   # throwaway; devnet only
  gas_fee: "1000000ugnot"
  gas_wanted: 20000000

conditions:
  require_all: false
  missed_blocks:
    enabled: true
    max: 5
    interval: current_week
  critical_alerts:
    enabled: false

proposal:
  submit: run
  cooldown: 1h
  rejected_backoff: 1h
  max_rejections: 2
  title_template: "Eject validator {moniker} ({operator_address}) from the valset"
  description_template: |
    Automated ejection proposal generated by katana (e2e broadcast).
    Validator: {moniker} ({operator_address})
    Reason: {reason}

vote_tracking:
  vote_poll_interval: 15s
  govdao_realm: "gno.land/r/gov/dao"
  status_source: rpc

notifications:
  enabled: true
  discord:
    enabled: true
    webhook_url: ""            # via env KATANA_DISCORD_WEBHOOK

storage:
  driver: sqlite
  sqlite_path: "/app/data/katana.db"

web:
  enabled: true
  listen: ":8990"

chains:
  local:
    rpc_endpoint: "http://validator:26657"
    gnoweb_url: "http://localhost:8890"
    chain_id: "dev"
    gnomonitoring_chain: "local"
    enabled: true
```

- [ ] **Step 2: Pass secrets + GNO_REF in `docker-compose.e2e.yml`**

In the `katana` service, add a build-arg and env:

```yaml
  katana:
    build:
      context: .
      dockerfile: Dockerfile
      args:
        GNO_REF: ${GNO_REF:-master}
    image: katana:latest
    container_name: katana-e2e
    restart: unless-stopped
    depends_on:
      - gnomonitoring
    environment:
      KATANA_GNOKEY_PASSWORD: ${KATANA_GNOKEY_PASSWORD:?set the keybase password}
      KATANA_DISCORD_WEBHOOK: ${KATANA_DISCORD_WEBHOOK:-}
    volumes:
      - ./deploy/e2e/katana.config.yaml:/app/config.yaml:ro
      - e2e-katana-data:/app/data
    ports:
      - "127.0.0.1:8990:8990"
    command: ["--config", "/app/config.yaml"]
    networks:
      - default
      - gno-devnet
```

- [ ] **Step 3: Delete the redundant host broadcast config and revoke its webhook**

The file is untracked and git-ignored (`*.host.yaml`), so it was never committed — remove it from disk only:

```bash
rm -f deploy/e2e/katana.broadcast.host.yaml
```

Then **revoke the Discord webhook** it contained (rotate it in Discord — it sat in plaintext and was shared, treat it as compromised even though it never entered git history).

- [ ] **Step 4: Update `deploy/e2e/README.md`**

Replace any "dry-run" walkthrough and the host-run instructions with the in-container broadcast flow: set `KATANA_GNOKEY_PASSWORD`, `KATANA_DISCORD_WEBHOOK`, `GNO_REF`, put the test mnemonic in `katana.config.yaml`, then `docker compose -f docker-compose.e2e.yml up --build`.

- [ ] **Step 5: Commit**

```bash
git add deploy/e2e/ docker-compose.e2e.yml
git commit -m "e2e: broadcast-only stack with in-container gnokey keybase"
```

---

### Task 11: Update the product docs

**Files:**
- Modify: `prd.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update `prd.md`**

Remove statements that dry-run is the default/an output mode. Document: katana always broadcasts; the signing key is a gnokey keybase provisioned from a mnemonic (or pre-provisioned in production); gnokey ships in the image pinned by `GNO_REF`.

- [ ] **Step 2: Update `CLAUDE.md`**

- In the eval-worker description, drop "dry-run or broadcast" → "broadcast".
- In the `internal/proposal` bullet, drop `DryRun writes JSON (never signs)`.
- In the Configuration model, remove `mode (dry-run|broadcast)` from the global-with-override list.
- In Conventions, remove "`dry-run` is the safe default; on-chain effects require `mode: broadcast`." and note katana is broadcast-only; gnokey is embedded in the image (GNO_REF).
- Add a line under Architecture noting `internal/keybase` provisions the signing key from a mnemonic at startup (idempotent; no-op for a pre-provisioned keybase).

- [ ] **Step 3: Sanity build/test**

Run: `make build && make test && make vet`
Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add prd.md CLAUDE.md
git commit -m "docs: document broadcast-only model and embedded gnokey"
```

---

### Task 12: End-to-end verification on the devnet

Not a code change — the acceptance gate. Uses the running `gnoland-test/` devnet.

- [ ] **Step 1: Create and provision the test key**

- Generate a devnet mnemonic (e.g. `gnokey add --recover katana-e2e` locally, or reuse the devnet `dev` account's mnemonic).
- Create the git-ignored live config from the template: `cp deploy/e2e/katana.config.yaml.example deploy/e2e/katana.config.yaml`, then set `gnokey.mnemonic` in it (or export `KATANA_GNOKEY_MNEMONIC` instead and leave it empty).
- Fund its address in `gnoland-test/genesis_balances.txt`.
- Make its address a **GovDAO member** (via the devnet's govdao init, e.g. `gnoland-test/init-govdao.gno`).
- Set `GNO_REF` to the devnet's gno commit (from `GNO_REPO_PATH`).

- [ ] **Step 2: Verify the exact `gnokey add --recover` invocation**

Run `docker run --rm --entrypoint gnokey katana:latest add --help` and confirm the flag names (`--recover`, `--insecure-password-stdin`) and stdin order used in `internal/keybase/keybase.go`. If they differ, fix `keybase.go`, re-run `go test ./internal/keybase/`, and commit the correction.

- [ ] **Step 3: Bring the stack up**

Run:
```bash
export KATANA_GNOKEY_PASSWORD='devpassword123'
export KATANA_DISCORD_WEBHOOK='https://discord.com/api/webhooks/...'
export GNO_REF='<devnet-commit>'
docker compose -f docker-compose.e2e.yml up --build
```
Expected logs: katana imports the keybase once (no error), then `[local][eval] worker started`.

- [ ] **Step 4: Trigger an ejection**

Down `validator4` (existing devnet scenario) so gnomonitoring reports missed blocks above the threshold.
Expected within a couple of poll intervals: `[local][eval] ELIGIBLE ...` then `[local][eval] BROADCAST ok for ... (proposal id="...")`.

- [ ] **Step 5: Confirm the on-chain proposal + notification**

- Open the devnet gnoweb `gno.land/r/gov/dao` and confirm the proposal exists.
- Confirm a Discord `EventSent` message was received.
- Watch the vote worker: after voting, `[local][vote] proposal ... ACCEPTED/REJECTED` and the matching notification.

- [ ] **Step 6: Confirm suppression works**

Verify a second eval tick logs `SUPPRESS ...: a proposal is pending GovDAO vote` (not a re-broadcast), proving the lifecycle gate holds on the real status.

---

## Self-Review

**Spec coverage:**
- Section 1 (remove dry-run) → Tasks 1-5. ✓
- Section 2 (gnokey in image) → Task 9. ✓
- Section 3 (mnemonic → import by katana) → Tasks 6-8. ✓
- Section 4 (e2e config + compose) → Task 10. ✓
- Section 5 (verification) → Task 12. ✓
- Fresh schema decision → Task 4. ✓
- Real-chain constraint (optional mnemonic, env secret, GNO_REF build-arg, per-chain gnokey) → Tasks 6/7 (`Ensure` no-ops on empty mnemonic), 8, 9, and existing `mergedWith`. ✓
- Docs (prd.md, CLAUDE.md) → Task 11. ✓

**Placeholder scan:** The only intentional `<...>` tokens are deployment inputs that cannot be known at authoring time: `<devnet-commit>` / `GNO_REF`, `<24-word devnet test mnemonic>`, and the verified go-install path in Task 9 Step 1. Each has an explicit resolution step. No "TBD/handle edge cases/add validation" placeholders remain.

**Type consistency:** `keybase.Ensure(ctx, config.Gnokey, Runner)` and `Runner` signature are identical in Tasks 7 and 8. `ProposalRecord` field removals (Task 4) match the columns removed in the schema/insert/scan and the fields no longer set in `produce` (Task 1). `config.Gnokey.Mnemonic` (Task 6) is consumed via `rt.Gnokey` (Task 8) and `gk.Mnemonic` (Task 7).

## Open items / risks

- **gnokey version skew** vs the devnet — mitigated by `GNO_REF`; fallback is the clone+build path in Task 9 Step 1.
- **`gnokey add --recover` flags** are version-dependent — Task 12 Step 2 validates them against the pinned binary before the real run.
- **Committed Discord webhook** in the deleted host config is compromised — Task 10 Step 3 rotates it.
