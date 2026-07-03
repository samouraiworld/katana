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
