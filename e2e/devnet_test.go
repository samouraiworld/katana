//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/samouraiworld/katana/internal/gnomonitoring"
	"github.com/samouraiworld/katana/internal/govdao"
)

// Environment knobs (all optional).
//
//	KATANA_E2E_RPC         devnet RPC            (default http://localhost:26658)
//	KATANA_E2E_INDEXER     tx-indexer GraphQL    (default http://localhost:8546/graphql/query)
//	KATANA_E2E_DEVNET_DIR  gnoland-test dir      (default ../gnoland-test)
//	KATANA_E2E_GNOKEY      gnokey binary         (default gnokey on PATH; must be
//	                       built from the same gno commit as the devnet, i.e.
//	                       the chain/mainnet tag)
const (
	chainKey  = "local"
	chainID   = "dev"
	devKey    = "dev"
	v4opKey   = "v4op"
	devPass   = "devpassword123"
	gasFee    = "1000000ugnot"
	chainName = "local"
)

type devnet struct {
	rpc, indexer, dir, home, gnokey string
	addrs                           map[string]string // from .devaddrs.mk
}

var (
	katanaBin string
	dn        devnet
)

func TestMain(m *testing.M) {
	dir := env("KATANA_E2E_DEVNET_DIR", filepath.Join("..", "gnoland-test"))
	abs, _ := filepath.Abs(dir)
	dn = devnet{
		rpc:     strings.TrimRight(env("KATANA_E2E_RPC", "http://localhost:26658"), "/"),
		indexer: env("KATANA_E2E_INDEXER", "http://localhost:8546/graphql/query"),
		dir:     abs,
		home:    filepath.Join(abs, ".gnokey-dev"),
		gnokey:  env("KATANA_E2E_GNOKEY", "gnokey"),
	}
	dn.addrs, _ = readDevAddrs(filepath.Join(abs, ".devaddrs.mk"))

	tmp, err := os.MkdirTemp("", "katana-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	katanaBin = filepath.Join(tmp, "katana")
	build := exec.Command("go", "build", "-o", katanaBin, "../cmd/katana")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building katana: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// readDevAddrs parses bootstrap.sh's `NAME := value` lines.
func readDevAddrs(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":=")
		if ok && !strings.HasPrefix(strings.TrimSpace(k), "#") {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m, sc.Err()
}

// requireDevnet fails fast with an actionable message when the devnet is
// not reachable or not bootstrapped.
func requireDevnet(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(dn.gnokey); err != nil {
		t.Fatalf("gnokey not found (%v): install it or set KATANA_E2E_GNOKEY", err)
	}
	for _, k := range []string{"DEV_ADDR", "VALIDATOR4_ADDR", "VALIDATOR4_OP_ADDR"} {
		if dn.addrs[k] == "" {
			t.Fatalf("%s missing from %s/.devaddrs.mk: run gnoland-test/bootstrap.sh", k, dn.dir)
		}
	}
	if _, err := latestHeight(); err != nil {
		t.Fatalf("devnet RPC %s unreachable (%v): run gnoland-test/katana-e2e.sh or `make up wait-chain` in gnoland-test", dn.rpc, err)
	}
}

// --- RPC helpers -----------------------------------------------------------

func getJSON(u string, out any) error {
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func latestHeight() (int64, error) {
	var st struct {
		Result struct {
			SyncInfo struct {
				Height string `json:"latest_block_height"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if err := getJSON(dn.rpc+"/status", &st); err != nil {
		return 0, err
	}
	var h int64
	_, err := fmt.Sscan(st.Result.SyncInfo.Height, &h)
	return h, err
}

type validator struct {
	Address     string `json:"address"`
	VotingPower string `json:"voting_power"`
}

func validators(t *testing.T) []validator {
	t.Helper()
	var v struct {
		Result struct {
			Validators []validator `json:"validators"`
		} `json:"result"`
	}
	if err := getJSON(dn.rpc+"/validators", &v); err != nil {
		t.Fatalf("RPC /validators: %v", err)
	}
	return v.Result.Validators
}

func inValset(t *testing.T, addr string) bool {
	for _, v := range validators(t) {
		if v.Address == addr {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func render(t *testing.T, pid string) govdao.Proposal {
	t.Helper()
	p, err := govdao.New(dn.rpc, realm).Proposal(context.Background(), pid)
	if err != nil {
		t.Fatalf("render of proposal %s: %v", pid, err)
	}
	return p
}

// --- gnokey helpers ----------------------------------------------------------

// gnokeyTx runs a `gnokey maketx` subcommand signed by key and returns the
// combined output. A keybase-lock error (katana's own gnokey holding the
// leveldb lock) is retried.
func gnokeyTx(key string, args ...string) (string, error) {
	full := append([]string{"maketx"}, args...)
	full = append(full, "-gas-fee", gasFee, "-gas-wanted", "100000000", "-broadcast",
		"-chainid", chainID, "-remote", dn.rpc, "-insecure-password-stdin", "-home", dn.home, key)
	return gnokeyRun(full, "")
}

func gnokeyRun(args []string, extra string) (string, error) {
	if extra != "" {
		args = append(args, extra)
	}
	var out string
	var err error
	for i := 0; i < 5; i++ {
		cmd := exec.Command(dn.gnokey, args...)
		cmd.Stdin = strings.NewReader(devPass + "\n")
		var b bytes.Buffer
		cmd.Stdout, cmd.Stderr = &b, &b
		err = cmd.Run()
		out = b.String()
		if err == nil || !strings.Contains(out, "resource temporarily unavailable") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	return out, err
}

func gnokeyRunScript(t *testing.T, key, script string) (string, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "script.gno")
	if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	full := []string{"maketx", "run", "-gas-fee", gasFee, "-gas-wanted", "100000000", "-broadcast",
		"-chainid", chainID, "-remote", dn.rpc, "-insecure-password-stdin", "-home", dn.home, key}
	return gnokeyRun(full, f)
}

func callDAO(fn string, args ...string) (string, error) {
	a := []string{"call", "-pkgpath", realm, "-func", fn}
	for _, x := range args {
		a = append(a, "-args", x)
	}
	return gnokeyTx(devKey, a...)
}

// vote casts option with every GovDAO member in the dev keybase: dev always,
// plus `katana` when `make add-key NAME=katana` made it a member (with two
// members a single vote is only 50%, short of the 66.66% supermajority).
func vote(t *testing.T, pid, option string) {
	t.Helper()
	out, err := gnokeyTx(devKey, "call", "-pkgpath", realm, "-func", "MustVoteOnProposalSimple", "-args", pid, "-args", option)
	if err != nil {
		t.Fatalf("dev vote %s on #%s: %v\n%s", option, pid, err, out)
	}
	_, _ = gnokeyTx("katana", "call", "-pkgpath", realm, "-func", "MustVoteOnProposalSimple", "-args", pid, "-args", option)
}

// ensureValidator4 makes sure GovDAO is initialised and validator4 is a
// registered valoper in the consensus valset, so katana has an ejectable
// target whose operator address the valopers realm resolves. Idempotent.
func ensureValidator4(t *testing.T) {
	t.Helper()
	v4 := dn.addrs["VALIDATOR4_ADDR"]
	if inValset(t, v4) {
		return
	}
	t.Logf("validator4 %s not in valset: onboarding it via GovDAO", v4)

	out, _ := gnokeyRun([]string{"query", "vm/qeval", "-remote", dn.rpc, "-data", realm + ".AllowedDAOs()"}, "")
	if !strings.Contains(out, "gno.land/r/gov/dao/impl/v0") {
		if out, err := gnokeyRunScript(t, devKey, mustRead(t, filepath.Join(dn.dir, "init-govdao.gno"))); err != nil {
			t.Fatalf("GovDAO init: %v\n%s", err, out)
		}
	}

	// Register is a no-op failure when v4op is already a valoper. The
	// registration fee is a sysparam that defaults to 0 on the mainnet tag:
	// sending coins anyway fails ("never read the send-envelope").
	_, _ = gnokeyTx(v4opKey, "call", "-pkgpath", "gno.land/r/gnops/valopers", "-func", "Register",
		"-args", "samourai-crew-4", "-args", "Phase 2 test validator for gnomonitoring", "-args", "on-prem",
		"-args", dn.addrs["VALIDATOR4_OP_ADDR"], "-args", dn.addrs["VALIDATOR4_PUB"])

	script := fmt.Sprintf(`package main

import (
	"gno.land/r/gov/dao"
	"gno.land/r/gnops/valopers/proposal"
)

func main(cur realm) {
	pr := proposal.NewValidatorProposalRequest(cross(cur), address(%q))
	dao.MustCreateProposal(cross(cur), pr)
}
`, dn.addrs["VALIDATOR4_OP_ADDR"])
	out, err := gnokeyRunScript(t, devKey, script)
	if err != nil {
		t.Fatalf("propose validator4: %v\n%s", err, out)
	}
	pid := proposalIDFromOutput(out)
	if pid == "" {
		t.Fatalf("no ProposalCreated id in:\n%s", out)
	}
	vote(t, pid, "YES")
	if out, err := callDAO("ExecuteProposal", pid); err != nil {
		t.Fatalf("execute #%s: %v\n%s", pid, err, out)
	}
	waitFor(t, "validator4 in /validators", 60*time.Second, func() bool { return inValset(t, v4) })
}

func proposalIDFromOutput(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "EVENTS:"); ok {
			return govdao.ProposalIDFromEvents([]byte(strings.TrimSpace(v)), realm)
		}
	}
	return ""
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- fake gnomonitoring ----------------------------------------------------

// fakeGnomonitoring serves /api/reports/validators for the live valset:
// target gets score `low` on every window, everyone else 95.
func fakeGnomonitoring(t *testing.T, target string, low int) *httptest.Server {
	t.Helper()
	return fakeGnomonitoringFunc(t, func(addr string) int {
		if addr == target {
			return low
		}
		return 95
	})
}

// fakeGnomonitoringFunc serves /api/reports/validators for the live valset,
// scoring every validator with score(address) on every window.
func fakeGnomonitoringFunc(t *testing.T, score func(addr string) int) *httptest.Server {
	t.Helper()
	v4 := dn.addrs["VALIDATOR4_ADDR"]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/reports/validators" {
			http.NotFound(w, r)
			return
		}
		// No t.Fatal here: this runs on the server goroutine.
		var vs struct {
			Result struct {
				Validators []validator `json:"validators"`
			} `json:"result"`
		}
		if err := getJSON(dn.rpc+"/validators", &vs); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var reports []gnomonitoring.Report
		for i, v := range vs.Result.Validators {
			score, tier, moniker := score(v.Address), "Healthy", fmt.Sprintf("samourai-crew-%d", i+1)
			if v.Address == v4 {
				moniker = "samourai-crew-4"
			}
			if score < 50 {
				tier = "Critical"
			}
			var vp int64
			fmt.Sscan(v.VotingPower, &vp)
			p := gnomonitoring.PeriodScore{Score: score, Tier: tier, SignRate: float64(score) / 100, VotingPower: vp}
			reports = append(reports, gnomonitoring.Report{
				Addr: v.Address, Moniker: moniker,
				Periods: map[string]gnomonitoring.PeriodScore{
					"last_24h": p, "current_week": p, "current_month": p, "current_year": p,
				},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reports)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- RPC proxy dropping broadcast responses ---------------------------------

// dropBroadcastProxy forwards every RPC request to the devnet but closes the
// connection instead of returning the response of broadcast_tx_* calls, after
// the node has processed them. gnokey then fails with "broadcasting bytes"
// although the tx is committed: the ambiguous case katana records as
// `unconfirmed`. Same behaviour as the capture tool e2e/tools/dropproxy.py.
func dropBroadcastProxy(t *testing.T) string {
	t.Helper()
	up, _ := url.Parse(dn.rpc)
	rp := httputil.NewSingleHostReverseProxy(up)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if !bytes.Contains(body, []byte("broadcast_tx")) && !strings.Contains(r.URL.Path, "broadcast_tx") {
			rp.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		rp.ServeHTTP(rec, r)
		mu.Lock()
		t.Logf("dropproxy: dropped broadcast response (upstream status %d)", rec.Code)
		mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.(*net.TCPConn).SetLinger(0)
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// --- katana process ----------------------------------------------------------

type katanaOpts struct {
	rpc            string // katana's rpc_endpoint (devnet or the drop proxy)
	gnomon         string
	pendingTimeout string
	nonce          string // unique marker put in the proposal title

	pollInterval string // default 3s
	// safety is the YAML body of the global safety block (default
	// "min_valset_size: 1": validator4 is the only ejectable target of a
	// 4-validator devnet, so the default floor of 4 would block it).
	safety string
	// actions is the YAML body of proposal.actions and actionPriority its
	// order (default: eject only, auto, score_below 30).
	actions        string
	actionPriority string
	indexer        bool   // set tx_indexer_url + gnokey.address (reconciliation)
	webhook        string // Discord webhook URL; "" disables notifications
	webListen      string // health server address; "" disables it
}

func (o katanaOpts) withDefaults() katanaOpts {
	if o.pollInterval == "" {
		o.pollInterval = "3s"
	}
	if o.safety == "" {
		o.safety = "min_valset_size: 1"
	}
	if o.actions == "" {
		o.actions = fmt.Sprintf(`eject:
  mode: auto
  score_below: 30
  window: current_week
  title_template: "Eject validator {moniker} ({address}) [%s]"
  description_template: "katana e2e %s. Score {score}/100 over {window}. Reason: {reason}"`, o.nonce, o.nonce)
		o.actionPriority = "[eject]"
	}
	return o
}

// indent prefixes every line of block with n spaces.
func indent(block string, n int) string {
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(strings.TrimSpace(block), "\n", "\n"+pad)
}

type katanaProc struct {
	cmd    *exec.Cmd
	dbPath string
	log    string
	db     *sql.DB
}

func startKatana(t *testing.T, o katanaOpts) *katanaProc {
	t.Helper()
	o = o.withDefaults()
	dir := t.TempDir()
	notifications := "notifications:\n  enabled: false\n"
	if o.webhook != "" {
		notifications = fmt.Sprintf("notifications:\n  enabled: true\n  discord:\n    enabled: true\n    webhook_url: %q\n", o.webhook)
	}
	web := "web:\n  enabled: false\n"
	if o.webListen != "" {
		web = fmt.Sprintf("web:\n  enabled: true\n  listen: %q\n", o.webListen)
	}
	indexer, address := "", ""
	if o.indexer {
		indexer = fmt.Sprintf("    tx_indexer_url: %q\n", dn.indexer)
		address = fmt.Sprintf("  address: %q\n", dn.addrs["DEV_ADDR"])
	}
	cfg := fmt.Sprintf(`log_level: debug
poll_interval: %s
dry_run: false
safety:
%s
gnomonitoring:
  base_url: %q
  reports_path: "/api/reports/validators"
gnokey:
  bin: %q
  key_name: %q
%s  home: %q
  password: %q
  gas_fee: %q
  gas_wanted: 100000000
proposal:
  action_priority: %s
  min_voting_power: 1
  max_voting_power_share: 100
  cooldown: 1h
  rejected_backoff: 1h
  max_rejections: 2
  submit: run
  actions:
%s
vote_tracking:
  vote_poll_interval: 2s
  govdao_realm: %q
  status_source: rpc
  execute_proposals: true
  pending_timeout: %s
%sstorage:
  driver: sqlite
  sqlite_path: %q
%schains:
  %s:
    rpc_endpoint: %q
    gnoweb_url: "http://localhost:8890"
    chain_id: %q
    gnomonitoring_chain: %q
%s    enabled: true
`, o.pollInterval, indent(o.safety, 2), o.gnomon, dn.gnokey, devKey, address, dn.home, devPass, gasFee,
		o.actionPriority, indent(o.actions, 4), realm, o.pendingTimeout, notifications,
		filepath.Join(dir, "katana.db"), web, chainKey, o.rpc, chainID, chainName, indexer)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "katana.log")
	lf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(katanaBin, "--config", cfgPath)
	cmd.Stdout, cmd.Stderr = lf, lf
	// Clear KATANA_* overrides so the test config is authoritative.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KATANA_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	k := &katanaProc{cmd: cmd, dbPath: filepath.Join(dir, "katana.db"), log: logPath}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
		}
		lf.Close()
		if k.db != nil {
			k.db.Close()
		}
		if t.Failed() {
			t.Logf("katana log (%s):\n%s", logPath, tail(logPath, 60))
		}
	})
	return k
}

func tail(path string, n int) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type proposalRow struct {
	ID              int64
	Status          string
	OnchainID       string
	Success         bool
	CreatedAt       string
	ResolvedAt      string
	TxHash          string
	BroadcastHeight int64
	PayloadHash     string
	Action          string
	TargetPower     int64
}

// rows reads katana's proposals table (read-only, tolerant of the daemon
// writing concurrently and of the table not existing yet).
func (k *katanaProc) rows(t *testing.T) []proposalRow {
	t.Helper()
	if k.db == nil {
		if _, err := os.Stat(k.dbPath); err != nil {
			return nil
		}
		db, err := sql.Open("sqlite", "file:"+k.dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		k.db = db
	}
	rs, err := k.db.Query(`SELECT id, status, onchain_proposal_id, success, created_at, resolved_at,
		tx_hash, broadcast_height, payload_hash, action, target_power
		FROM proposals WHERE chain = ? ORDER BY id`, chainKey)
	if err != nil {
		return nil // schema not created yet
	}
	defer rs.Close()
	var out []proposalRow
	for rs.Next() {
		var r proposalRow
		if err := rs.Scan(&r.ID, &r.Status, &r.OnchainID, &r.Success, &r.CreatedAt, &r.ResolvedAt,
			&r.TxHash, &r.BroadcastHeight, &r.PayloadHash, &r.Action, &r.TargetPower); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// waitRow waits until katana has exactly one proposal row matching cond and
// returns it. More than one row means the lifecycle gate let a duplicate
// through, which fails the test.
func (k *katanaProc) waitRow(t *testing.T, what string, timeout time.Duration, cond func(proposalRow) bool) proposalRow {
	t.Helper()
	var got proposalRow
	waitFor(t, what, timeout, func() bool {
		rs := k.rows(t)
		if len(rs) > 1 {
			t.Fatalf("lifecycle gate let %d proposal rows through: %+v", len(rs), rs)
		}
		if len(rs) == 1 && cond(rs[0]) {
			got = rs[0]
			return true
		}
		return false
	})
	return got
}

// notificationEvents returns the event column of katana's notifications
// table, in order (nil while the table does not exist yet).
func (k *katanaProc) notificationEvents(t *testing.T) []string {
	t.Helper()
	k.rows(t) // opens the db
	if k.db == nil {
		return nil
	}
	rs, err := k.db.Query(`SELECT event FROM notifications WHERE chain = ? ORDER BY id`, chainKey)
	if err != nil {
		return nil
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var e string
		if err := rs.Scan(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// logContains reports whether katana's log has a line containing s.
func (k *katanaProc) logContains(s string) bool {
	b, _ := os.ReadFile(k.log)
	return bytes.Contains(b, []byte(s))
}

// webhookSink is a fake Discord webhook counting the posts it receives.
type webhookSink struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	w := &webhookSink{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.bodies = append(w.bodies, string(b))
		w.mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *webhookSink) posts() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.bodies...)
}

// freeAddr returns a loopback address with a currently free port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// health GETs katana's /health and returns the status code and body.
func health(addr string) (int, string) {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// votingPower returns addr's voting power in RPC /validators (0 when absent).
func votingPower(t *testing.T, addr string) int64 {
	for _, v := range validators(t) {
		if v.Address == addr {
			var vp int64
			fmt.Sscan(v.VotingPower, &vp)
			return vp
		}
	}
	return 0
}
