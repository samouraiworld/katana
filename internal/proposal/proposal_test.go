package proposal

import (
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

func target() Target {
	return Target{
		ChainKey: "local", ChainID: "dev", RPC: "http://localhost:26658",
		Moniker: "bad-node", Address: "g1sign", OperatorAddress: "g1op",
		Reason: "missed 200 blocks",
	}
}

func TestBuildRunMode(t *testing.T) {
	p := config.Proposal{
		Submit:              "run",
		RunScriptTemplate:   config.DefaultEjectScript,
		TitleTemplate:       "Eject {moniker} ({operator_address})",
		DescriptionTemplate: "Reason: {reason}\n\"quoted\"",
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1000000ugnot", GasWanted: 20000000}

	b := Build(target(), p, gk)
	if b.Submit != "run" {
		t.Fatalf("submit = %s, want run", b.Submit)
	}
	// Operator address is used in the script, not the signing address.
	if !strings.Contains(b.Script, `address("g1op")`) {
		t.Errorf("script missing operator address:\n%s", b.Script)
	}
	if !strings.Contains(b.Script, "NewValoperChange") || !strings.Contains(b.Script, ", 0)") {
		t.Errorf("script is not a Power=0 removal:\n%s", b.Script)
	}
	// The description's quotes/newlines must be escaped inside the .gno literal.
	if !strings.Contains(b.Script, `\"quoted\"`) || strings.Contains(b.Script, "Reason: missed 200 blocks\n\"quoted\"") {
		t.Errorf("description not escaped for Go literal:\n%s", b.Script)
	}

	args := b.runArgs(gk, "/tmp/x.gno")
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "maketx run") || !strings.HasSuffix(joined, "dev /tmp/x.gno") {
		t.Errorf("unexpected run args: %v", args)
	}
	// gnokey must read the password from stdin, not a TTY (katana execs it with
	// no controlling terminal); otherwise it fails with "inappropriate ioctl".
	if !strings.Contains(joined, "-insecure-password-stdin") {
		t.Errorf("run args missing -insecure-password-stdin: %v", args)
	}
}

func TestBuildCallMode(t *testing.T) {
	p := config.Proposal{
		Submit:        "call",
		RealmPkgPath:  "gno.land/r/x",
		Function:      "RemoveValidator",
		Args:          []string{"{operator_address}", "{moniker}"},
		TitleTemplate: "t",
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}

	b := Build(target(), p, gk)
	if b.Submit != "call" {
		t.Fatalf("submit = %s, want call", b.Submit)
	}
	if b.Args[0] != "g1op" || b.Args[1] != "bad-node" {
		t.Errorf("args not rendered: %v", b.Args)
	}
	args := b.callArgs(gk)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "maketx call -pkgpath gno.land/r/x -func RemoveValidator") {
		t.Errorf("unexpected call args: %v", args)
	}
}

func TestOperatorFallsBackToAddress(t *testing.T) {
	tg := target()
	tg.OperatorAddress = ""
	b := Build(tg, config.Proposal{Submit: "run", RunScriptTemplate: config.DefaultEjectScript}, config.Gnokey{})
	if !strings.Contains(b.Script, `address("g1sign")`) {
		t.Errorf("expected fallback to signing address:\n%s", b.Script)
	}
}
