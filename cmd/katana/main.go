// Command katana is a continuously running backend that watches gno.land
// validators across every enabled chain and, for any that fail the configured
// health conditions, produces a GovDAO proposal to eject it from the valset —
// then follows the vote outcome and notifies on the result.
//
// Behaviour is entirely config-driven (config.yaml); per-chain run mode
// (dry-run vs broadcast) is set there, not via CLI flags. The only flag is the
// config path.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/samouraiworld/katana/internal/config"
	"github.com/samouraiworld/katana/internal/daemon"
	"github.com/samouraiworld/katana/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("config: %v", err)
	}

	if cfg.Web.Enabled {
		logger.Printf("health endpoint enabled on %s", cfg.Web.Listen)
	}

	st, err := store.New(cfg.Storage)
	if err != nil {
		fatal("storage: %v", err)
	}
	defer st.Close()

	// Cancel the root context on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := daemon.New(cfg, st, logger)
	if err := d.Run(ctx); err != nil {
		fatal("daemon: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "katana: "+format+"\n", args...)
	os.Exit(1)
}
