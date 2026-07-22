// Package proposal builds a valset-ejection GovDAO proposal and signs and
// broadcasts it with gnokey.
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
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/samouraiworld/katana/internal/config"
)

// Target identifies the validator a proposal is built against and the
// score-driven action to take.
type Target struct {
	ChainKey string
	ChainID  string
	RPC      string
	Moniker  string
	Address  string // consensus/signing address (from the report)
	// OperatorAddress is the valoper operator address the valset proposal
	// must reference. Falls back to Address when empty.
	OperatorAddress string
	Reason          string

	Action       string // config.ActionEject | ActionLowerVP | ActionRaiseVP
	Score        int
	Tier         string
	Window       string
	CurrentPower int64
	TargetPower  int64 // 0 for eject
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

// Build renders the proposal templates for target.Action's configured
// submission method.
func Build(t Target, p config.Proposal, gk config.Gnokey) Built {
	if t.OperatorAddress == "" {
		t.OperatorAddress = t.Address
	}
	ac := p.Actions[t.Action]
	vars := map[string]string{
		"address":          t.Address,
		"operator_address": t.OperatorAddress,
		"moniker":          t.Moniker,
		"reason":           t.Reason,
		"chain":            t.ChainKey,
		"realm":            ac.RealmPkgPath,
		"score":            strconv.Itoa(t.Score),
		"tier":             t.Tier,
		"window":           t.Window,
		"current_power":    strconv.FormatInt(t.CurrentPower, 10),
		"target_power":     strconv.FormatInt(t.TargetPower, 10),
	}
	title := render(ac.TitleTemplate, vars)
	description := render(ac.DescriptionTemplate, vars)

	b := Built{
		Target:      t,
		Submit:      submitMethod(p),
		Title:       title,
		Description: description,
		RealmPath:   ac.RealmPkgPath,
		Function:    ac.Function,
	}

	if b.Submit == "run" {
		// Inside a .gno script, title/description are embedded in Go string
		// literals, so they must be escaped (quotes/newlines).
		svars := cloneVars(vars)
		svars["title"] = goInner(title)
		svars["description"] = goInner(description)
		b.Script = render(ac.RunScriptTemplate, svars)
	} else {
		vars["title"] = title
		vars["description"] = description
		b.Args = make([]string, len(ac.Args))
		for i, a := range ac.Args {
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
		// Read the keybase password from stdin, not the TTY (katana execs gnokey
		// with no controlling terminal); Broadcast feeds the password on stdin.
		"-insecure-password-stdin",
	}
	if gk.Home != "" {
		args = append(args, "-home", gk.Home)
	}
	return append(args, gk.KeyName)
}

// Command renders a copy-pasteable gnokey command an operator can run to
// broadcast this proposal manually (used in observe-mode notifications).
// scriptName is the local path of the .gno file the operator downloads (run
// mode; ignored in call mode). The keybase password is piped via stdin
// (-insecure-password-stdin), never embedded in the command text.
func (b Built) Command(gk config.Gnokey, scriptName string) string {
	var args []string
	if b.Submit == "run" {
		args = b.runArgs(gk, scriptName)
	} else {
		args = b.callArgs(gk)
	}
	bin := gk.Bin
	if bin == "" {
		bin = "gnokey"
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellArg(a)
	}
	return `printf '%s\n' "$KEYBASE_PASSWORD" | ` + bin + " " + strings.Join(quoted, " ")
}

// shellArg single-quotes a when it contains characters the shell would
// interpret, so the rendered Command is safe to copy-paste.
func shellArg(a string) string {
	if a == "" {
		return "''"
	}
	if strings.ContainsAny(a, " \t\n\"'\\$`(){}[]<>|&;*?#~!") {
		return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return a
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
	// Always provide stdin: -insecure-password-stdin makes gnokey read the
	// password (possibly empty) from stdin rather than prompting on a TTY.
	cmd.Stdin = strings.NewReader(gk.Password + "\n")
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
