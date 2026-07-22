// Command stub-gnomonitoring is a throwaway HTTP server standing in for a
// real gnomonitoring backend when testing katana against the gnoland-test
// devnet. katana only ever reads GET /api/reports/validators (see
// internal/gnomonitoring/client.go) — this serves that exact JSON contract
// from a local file, re-read on every request, so switching scenarios (see
// ../Makefile's stub-scenario target) takes effect on katana's next poll
// without restarting this server.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/samouraiworld/katana/internal/gnomonitoring"
)

func main() {
	listen := flag.String("listen", ":8989", "listen address")
	file := flag.String("file", "scenarios/current.json", "JSON file to serve as the report payload (re-read on every request)")
	token := flag.String("token", "", "if set, require this exact value as the Authorization: Bearer token")
	flag.Parse()

	http.HandleFunc("/api/reports/validators", func(w http.ResponseWriter, r *http.Request) {
		if *token != "" && r.Header.Get("Authorization") != "Bearer "+*token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		data, err := os.ReadFile(*file)
		if err != nil {
			log.Printf("read %s: %v", *file, err)
			http.Error(w, "scenario file not found: "+*file, http.StatusInternalServerError)
			return
		}

		// Round-trip through katana's own Report type so a malformed scenario
		// file fails loudly here instead of confusing katana's client.
		var reports []gnomonitoring.Report
		if err := json.Unmarshal(data, &reports); err != nil {
			log.Printf("parse %s: %v", *file, err)
			http.Error(w, "invalid scenario JSON: "+err.Error(), http.StatusInternalServerError)
			return
		}

		log.Printf("GET /api/reports/validators?chain=%s -> %s (%d validator(s))",
			r.URL.Query().Get("chain"), *file, len(reports))
		for _, rep := range reports {
			p := rep.Periods
			log.Printf("  %-20s %-44s 24h=%-3d week=%-3d month=%-3d year=%-3d",
				rep.Moniker, rep.Addr,
				p["last_24h"].Score, p["current_week"].Score, p["current_month"].Score, p["current_year"].Score)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(reports); err != nil {
			log.Printf("encode response: %v", err)
		}
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("stub-gnomonitoring listening on %s, serving %s (switch with: make stub-scenario NAME=<name>)", *listen, *file)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
