package proposal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/samouraiworld/katana/internal/config"
)

// The outputs below follow gno.land/pkg/keyscli PrintTxSuccess /
// PrintTxMetrics and tm2's maketx error wrapping (see
// .claude/docs/katana-hardening.md for the source references).
const (
	gnokeyOK = "\nOK!\n" +
		"GAS WANTED: 50000000\n" +
		"GAS USED:   12345678\n" +
		"HEIGHT:     4242\n" +
		"EVENTS:     [{\"type\":\"ProposalCreated\",\"attrs\":[{\"key\":\"id\",\"value\":\"7\"}],\"pkg_path\":\"gno.land/r/gov/dao\"}]\n" +
		"INFO:       \n" +
		"TX HASH:    q2Vh0z3D+Jm2nSx0Zl1Q0QsXy7c1yQ8l3v0Hn4c2Gx8=\n"

	// DeliverTx failed: on-chain (height, hash) but no OK! and no events.
	gnokeyDeliverFailed = "GAS WANTED: 50000000\n" +
		"GAS USED:   812345\n" +
		"HEIGHT:     4243\n" +
		"EVENTS:     []\n" +
		"INFO:       \n" +
		"TX HASH:    Zm9vYmFyYmF6cXV4Zm9vYmFyYmF6cXV4Zm9vYmFyYmE=\n" +
		"deliver transaction failed: log:unauthorized\n"

	// Default -simulate test failed: nothing was sent.
	gnokeySimFailed = "GAS WANTED: 50000000\n" +
		"GAS USED:   0\n" +
		"HEIGHT:     0\n" +
		"EVENTS:     []\n" +
		"INFO:       \n" +
		"TX HASH:    \n" +
		"deliver transaction failed: log:out of gas\n"

	gnokeyCheckFailed = "check transaction failed: log:insufficient funds\n"

	gnokeyBroadcastTimeout = "broadcast tx: broadcasting bytes: unable to call RPC method broadcast_tx_commit: request timeout\n"
)

func TestParseTxOutput(t *testing.T) {
	r := ParseTxOutput(gnokeyOK)
	if !r.OK || r.Height != 4242 || r.TxHash != "q2Vh0z3D+Jm2nSx0Zl1Q0QsXy7c1yQ8l3v0Hn4c2Gx8=" {
		t.Fatalf("ParseTxOutput(ok) = %+v", r)
	}
	if r.Events == "" || r.Events[0] != '[' {
		t.Fatalf("Events = %q, want the JSON array", r.Events)
	}

	r = ParseTxOutput(gnokeyDeliverFailed)
	if r.OK || r.Height != 4243 || r.TxHash == "" {
		t.Fatalf("ParseTxOutput(deliver failed) = %+v", r)
	}

	r = ParseTxOutput(gnokeySimFailed)
	if r.OK || r.Height != 0 || r.TxHash != "" {
		t.Fatalf("ParseTxOutput(sim failed) = %+v", r)
	}
}

func TestClassify(t *testing.T) {
	gnokeyErr := errors.New("gnokey: exit status 1")
	tests := []struct {
		name string
		out  string
		err  error
		want Outcome
	}{
		{"success", gnokeyOK, nil, Committed},
		{"deliver failed is on-chain but created nothing", gnokeyDeliverFailed, gnokeyErr, Failed},
		{"simulation failed sent nothing", gnokeySimFailed, gnokeyErr, Failed},
		{"check tx failed sent nothing", gnokeyCheckFailed, gnokeyErr, Failed},
		{"failed before gnokey ran", "", errors.New("gnokey.key_name is required to broadcast"), Failed},
		{"broadcast transport error may have landed", gnokeyBroadcastTimeout, gnokeyErr, Ambiguous},
		{"interrupted mid-run may have landed", "", fmt.Errorf("gnokey interrupted: %w", context.Canceled), Ambiguous},
		{"deadline mid-run may have landed", "", fmt.Errorf("gnokey interrupted: %w", context.DeadlineExceeded), Ambiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.out, tt.err); got != tt.want {
				t.Errorf("Classify() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecuteCallArgs(t *testing.T) {
	b := ExecuteCall("dev", "http://rpc:26657", "gno.land/r/gov/dao", "7")
	gk := config.Gnokey{Bin: "gnokey", KeyName: "dev", GasFee: "1ugnot", GasWanted: 1}
	got := b.Command(gk, "")
	want := "maketx call -pkgpath gno.land/r/gov/dao -func ExecuteOrRejectProposal -args 7"
	if !strings.Contains(got, want) || !strings.Contains(got, "-chainid dev") || !strings.Contains(got, "-remote http://rpc:26657") {
		t.Fatalf("Command = %q", got)
	}
}
