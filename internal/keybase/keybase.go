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
