// Package proposal builds a valset-ejection GovDAO proposal and either writes it
// out for manual review (dry-run) or signs and broadcasts it with gnokey.
//
// Two submission methods are supported (proposal.submit):
//
//   - "run": broadcast a rendered .gno script via `gnokey maketx run`. This is
//     how gno.land valset changes actually work — v3's proposal builder is
//     non-crossing and cannot be reached by a direct MsgCall, so proposers route
//     through a small crossing script (NewValidatorProposalRequest with Power=0
//     to eject, then dao.MustCreateProposal).
//   - "call": broadcast a single `gnokey maketx call` (MsgCall) to a realm
//     function. Kept for networks/realms that expose a one-call entrypoint.
package proposal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samouraiworld/katana/internal/config"
)

// Target identifies the validator an ejection proposal is built against.
type Target struct {
	ChainKey string
	ChainID  string
	RPC      string
	Moniker  string
	Address  string // consensus/signing address (from metrics)
	// OperatorAddress is the valoper operator address the valset proposal must
	// reference. Falls back to Address when empty.
	OperatorAddress string
	Reason          string
}

// Built is a fully rendered proposal.
type Built struct {
	Target      Target
	Submit      string // run | call
	Title       string
	Description string
	RealmPath   string
	Function    string
	Args        []string // call mode
	Script      string   // run mode (.gno source)
}

// Build renders the proposal templates for the configured submission method.
func Build(t Target, p config.Proposal, gk config.Gnokey) Built {
	if t.OperatorAddress == "" {
		t.OperatorAddress = t.Address
	}
	vars := map[string]string{
		"address":          t.Address,
		"operator_address": t.OperatorAddress,
		"moniker":          t.Moniker,
		"reason":           t.Reason,
		"chain":            t.ChainKey,
		"realm":            p.RealmPkgPath,
	}
	title := render(p.TitleTemplate, vars)
	description := render(p.DescriptionTemplate, vars)

	b := Built{
		Target:      t,
		Submit:      submitMethod(p),
		Title:       title,
		Description: description,
		RealmPath:   p.RealmPkgPath,
		Function:    p.Function,
	}

	if b.Submit == "run" {
		// Inside a .gno script, title/description are embedded in Go string
		// literals, so they must be escaped (quotes/newlines).
		svars := cloneVars(vars)
		svars["title"] = goInner(title)
		svars["description"] = goInner(description)
		b.Script = render(p.RunScriptTemplate, svars)
	} else {
		vars["title"] = title
		vars["description"] = description
		b.Args = make([]string, len(p.Args))
		for i, a := range p.Args {
			b.Args[i] = render(a, vars)
		}
	}
	return b
}

func submitMethod(p config.Proposal) string {
	if p.Submit == "call" {
		return "call"
	}
	return "run"
}

// callArgs builds the `gnokey maketx call ...` argument vector.
func (b Built) callArgs(gk config.Gnokey) []string {
	args := []string{"maketx", "call", "-pkgpath", b.RealmPath, "-func", b.Function}
	for _, a := range b.Args {
		args = append(args, "-args", a)
	}
	return append(args, b.commonArgs(gk)...)
}

// runArgs builds the `gnokey maketx run ... <script>` argument vector.
func (b Built) runArgs(gk config.Gnokey, scriptPath string) []string {
	args := []string{"maketx", "run"}
	args = append(args, b.commonArgs(gk)...)
	return append(args, scriptPath)
}

// commonArgs are the flags shared by both maketx subcommands, ending with the
// key name (the positional signer arg, before any run script path).
func (b Built) commonArgs(gk config.Gnokey) []string {
	args := []string{
		"-gas-fee", gk.GasFee,
		"-gas-wanted", strconv.FormatInt(gk.GasWanted, 10),
		"-broadcast",
		"-chainid", b.Target.ChainID,
		"-remote", b.Target.RPC,
	}
	if gk.Home != "" {
		args = append(args, "-home", gk.Home)
	}
	return append(args, gk.KeyName)
}

// DryRun writes the proposal artifact (and, in run mode, the .gno script) into
// the output directory and returns the artifact path.
func (b Built) DryRun(outputDir string, gk config.Gnokey) (string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", fmt.Errorf("creating output dir: %w", err)
	}
	base := fmt.Sprintf("proposal-%s-%s-%d", b.Target.ChainKey, safe(b.Target.Address), time.Now().Unix())

	var gnokeyArgs []string
	artifact := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"chain":        b.Target.ChainKey,
		"chain_id":     b.Target.ChainID,
		"validator":    map[string]string{"moniker": b.Target.Moniker, "address": b.Target.Address, "operator": b.Target.OperatorAddress},
		"reason":       b.Target.Reason,
		"title":        b.Title,
		"description":  b.Description,
		"submit":       b.Submit,
	}

	if b.Submit == "run" {
		scriptPath := filepath.Join(outputDir, base+".gno")
		if err := os.WriteFile(scriptPath, []byte(b.Script), 0o644); err != nil {
			return "", fmt.Errorf("writing script: %w", err)
		}
		gnokeyArgs = b.runArgs(gk, scriptPath)
		artifact["script"] = scriptPath
	} else {
		gnokeyArgs = b.callArgs(gk)
		artifact["realm"] = b.RealmPath
		artifact["function"] = b.Function
		artifact["args"] = b.Args
	}
	artifact["gnokey_command"] = "gnokey " + shellJoin(gnokeyArgs)

	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling proposal: %w", err)
	}
	path := filepath.Join(outputDir, base+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("writing proposal: %w", err)
	}
	return path, nil
}

// Broadcast signs and submits the proposal via gnokey. In run mode it writes the
// script to a temp file first. The combined gnokey output is returned.
func (b Built) Broadcast(ctx context.Context, gk config.Gnokey) (string, error) {
	if gk.KeyName == "" {
		return "", fmt.Errorf("gnokey.key_name is required to broadcast")
	}

	var args []string
	if b.Submit == "run" {
		f, err := os.CreateTemp("", "katana-*.gno")
		if err != nil {
			return "", fmt.Errorf("temp script: %w", err)
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(b.Script); err != nil {
			f.Close()
			return "", fmt.Errorf("writing temp script: %w", err)
		}
		f.Close()
		args = b.runArgs(gk, f.Name())
	} else {
		args = b.callArgs(gk)
	}

	cmd := exec.CommandContext(ctx, gk.Bin, args...)
	if gk.Password != "" {
		cmd.Stdin = strings.NewReader(gk.Password + "\n")
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("gnokey: %w", err)
	}
	return out.String(), nil
}

// render replaces {key} placeholders in tmpl with values from vars.
func render(tmpl string, vars map[string]string) string {
	out := tmpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}

func cloneVars(v map[string]string) map[string]string {
	out := make(map[string]string, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out
}

// goInner returns s escaped for embedding inside a Go double-quoted string
// literal (without the surrounding quotes).
func goInner(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			parts[i] = strconv.Quote(a)
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

func safe(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
	if len(s) > 16 {
		s = s[:16]
	}
	return s
}
