package proposal

import (
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

func target(action string) Target {
	return Target{
		ChainKey: "local", ChainID: "dev", RPC: "http://localhost:26658",
		Moniker: "bad-node", Address: "g1sign", OperatorAddress: "g1op",
		Reason: "score 20/100 (tier Critical) over current_month",
		Action: action, Score: 20, Tier: "Critical", Window: "current_month",
		CurrentPower: 1000, TargetPower: 0,
	}
}

func ejectProposalConfig() config.Proposal {
	return config.Proposal{
		Submit: "run",
		Actions: map[string]config.ActionConfig{
			config.ActionEject: {
				RunScriptTemplate:   config.DefaultEjectScript,
				TitleTemplate:       "Eject {moniker} ({operator_address})",
				DescriptionTemplate: "Reason: {reason}\n\"quoted\"",
			},
		},
	}
}

func TestBuildRunMode(t *testing.T) {
	p := ejectProposalConfig()
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1000000ugnot", GasWanted: 20000000}

	tg := target(config.ActionEject)
	b := Build(tg, p, gk)
	if b.Submit != "run" {
		t.Fatalf("submit = %s, want run", b.Submit)
	}
	if !strings.Contains(b.Script, `address("g1op")`) {
		t.Errorf("script missing operator address:\n%s", b.Script)
	}
	if !strings.Contains(b.Script, "NewValoperChange") || !strings.Contains(b.Script, ", 0)") {
		t.Errorf("script is not a Power=0 removal:\n%s", b.Script)
	}
	if !strings.Contains(b.Script, `\"quoted\"`) || strings.Contains(b.Script, "Reason: score 20/100 (tier Critical) over current_month\n\"quoted\"") {
		t.Errorf("description not escaped for Go literal:\n%s", b.Script)
	}

	args := b.runArgs(gk, "/tmp/x.gno")
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "maketx run") || !strings.HasSuffix(joined, "dev /tmp/x.gno") {
		t.Errorf("unexpected run args: %v", args)
	}
	if !strings.Contains(joined, "-insecure-password-stdin") {
		t.Errorf("run args missing -insecure-password-stdin: %v", args)
	}
}

func TestBuildLowerVPRunMode(t *testing.T) {
	p := config.Proposal{
		Submit: "run",
		Actions: map[string]config.ActionConfig{
			config.ActionLowerVP: {
				RunScriptTemplate:   config.DefaultVPChangeScript,
				TitleTemplate:       "Lower {moniker}",
				DescriptionTemplate: "{current_power} -> {target_power}",
			},
		},
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}

	tg := target(config.ActionLowerVP)
	tg.CurrentPower, tg.TargetPower = 1000, 600
	b := Build(tg, p, gk)

	if !strings.Contains(b.Script, `address("g1op")`) {
		t.Errorf("script missing operator address:\n%s", b.Script)
	}
	if !strings.Contains(b.Script, ", 600)") {
		t.Errorf("script does not carry the target power 600:\n%s", b.Script)
	}
	if strings.Contains(b.Script, ", 0)") {
		t.Errorf("script must not be a Power=0 removal for lower_vp:\n%s", b.Script)
	}
	if !strings.Contains(b.Description, "1000 -> 600") {
		t.Errorf("description missing current->target power: %s", b.Description)
	}
}

func TestBuildRaiseVPRunMode(t *testing.T) {
	p := config.Proposal{
		Submit: "run",
		Actions: map[string]config.ActionConfig{
			config.ActionRaiseVP: {
				RunScriptTemplate:   config.DefaultVPChangeScript,
				TitleTemplate:       "Raise {moniker}",
				DescriptionTemplate: "score={score} tier={tier} window={window}",
			},
		},
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}

	tg := target(config.ActionRaiseVP)
	tg.Score, tg.Tier, tg.Window = 90, "Excellent", "current_year"
	tg.CurrentPower, tg.TargetPower = 1000, 1200
	b := Build(tg, p, gk)

	if !strings.Contains(b.Script, ", 1200)") {
		t.Errorf("script does not carry the target power 1200:\n%s", b.Script)
	}
	if b.Description != "score=90 tier=Excellent window=current_year" {
		t.Errorf("description = %q", b.Description)
	}
}

func TestBuildCallMode(t *testing.T) {
	p := config.Proposal{
		Submit: "call",
		Actions: map[string]config.ActionConfig{
			config.ActionEject: {
				RealmPkgPath:  "gno.land/r/x",
				Function:      "RemoveValidator",
				Args:          []string{"{operator_address}", "{moniker}"},
				TitleTemplate: "t",
			},
		},
	}
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}

	b := Build(target(config.ActionEject), p, gk)
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

func TestCommandRunMode(t *testing.T) {
	p := ejectProposalConfig()
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1000000ugnot", GasWanted: 20000000, Home: "/app/data/keybase", Password: "s3cret"}
	b := Build(target(config.ActionEject), p, gk)

	cmd := b.Command(gk, "eject-bad-node.gno")
	for _, want := range []string{
		"gnokey maketx run", "-remote http://localhost:26658", "-chainid dev",
		"-insecure-password-stdin", "-home /app/data/keybase", "eject-bad-node.gno",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "s3cret") {
		t.Errorf("password must not appear in the command: %s", cmd)
	}
	if !strings.Contains(cmd, `"$KEYBASE_PASSWORD"`) {
		t.Errorf("command should pipe the password from an env var: %s", cmd)
	}
}

func TestOperatorFallsBackToAddress(t *testing.T) {
	tg := target(config.ActionEject)
	tg.OperatorAddress = ""
	b := Build(tg, ejectProposalConfig(), config.Gnokey{})
	if !strings.Contains(b.Script, `address("g1sign")`) {
		t.Errorf("expected fallback to signing address:\n%s", b.Script)
	}
}
