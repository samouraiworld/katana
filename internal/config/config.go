// Package config loads and validates katana's configuration.
//
// The entire runtime configuration — including every value that would
// traditionally be passed through environment variables — lives in a single
// config.yaml file. Environment variables are supported only as optional
// overrides for secrets and per-deployment values (see applyEnvOverrides).
//
// Many blocks (conditions, proposal, gnokey, poll interval) have a global
// default and an optional per-chain override. Use Runtime(chainID) to obtain the
// fully-resolved settings for a chain rather than reading the raw fields.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration document (config.yaml).
type Config struct {
	LogLevel      string        `yaml:"log_level"`
	PollInterval  string        `yaml:"poll_interval"` // daemon cadence (global default)
	Gnomonitoring Gnomonitoring `yaml:"gnomonitoring"`
	Gnokey        Gnokey        `yaml:"gnokey"` // global proposing wallet
	Proposal      Proposal      `yaml:"proposal"`
	// DryRun is the global default for whether chains broadcast: false
	// (default) broadcasts; true means every enabled chain rehearses instead
	// (logs + notifies, never signs/broadcasts), unless overridden per chain
	// via ChainConfig.DryRun.
	DryRun        bool          `yaml:"dry_run"`
	VoteTracking  VoteTracking  `yaml:"vote_tracking"`
	Notifications Notifications `yaml:"notifications"`
	// Exceptions lists validators that must never be subscribed to an ejection
	// proposal, matched by address or moniker. This global list is unioned with
	// each chain's own Exceptions.
	Exceptions []string                `yaml:"exceptions"`
	Storage    Storage                 `yaml:"storage"`
	Web        Web                     `yaml:"web"`
	Chains     map[string]*ChainConfig `yaml:"chains"`
	// ValopersRealm is the realm used to resolve a validator's operator address
	// from its signing address (needed for broadcast ejection).
	ValopersRealm string `yaml:"valopers_realm"`
}

// Gnomonitoring points katana at a running gnomonitoring REST API.
type Gnomonitoring struct {
	BaseURL        string `yaml:"base_url"`
	APIToken       string `yaml:"api_token"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	ReportsPath    string `yaml:"reports_path"`
	InfoPath       string `yaml:"info_path"`
	DiscoverChains bool   `yaml:"discover_chains"`
}

// Gnokey is a proposing wallet. Used only on the broadcast path. The global
// block is the default; a chain may override any subset of fields.
type Gnokey struct {
	Bin       string `yaml:"bin"`
	KeyName   string `yaml:"key_name"`
	Address   string `yaml:"address"`
	Home      string `yaml:"home"`
	Password  string `yaml:"password"`
	GasFee    string `yaml:"gas_fee"`
	GasWanted int64  `yaml:"gas_wanted"`
	Mnemonic  string `yaml:"mnemonic"`
}

// mergedWith returns g with any non-zero field of o applied on top.
func (g Gnokey) mergedWith(o Gnokey) Gnokey {
	if o.Bin != "" {
		g.Bin = o.Bin
	}
	if o.KeyName != "" {
		g.KeyName = o.KeyName
	}
	if o.Address != "" {
		g.Address = o.Address
	}
	if o.Home != "" {
		g.Home = o.Home
	}
	if o.Password != "" {
		g.Password = o.Password
	}
	if o.GasFee != "" {
		g.GasFee = o.GasFee
	}
	if o.GasWanted != 0 {
		g.GasWanted = o.GasWanted
	}
	if o.Mnemonic != "" {
		g.Mnemonic = o.Mnemonic
	}
	return g
}

// Score-driven action names: map keys for Proposal.Actions and the only
// valid entries in Proposal.ActionPriority.
const (
	ActionEject   = "eject"
	ActionLowerVP = "lower_vp"
	ActionRaiseVP = "raise_vp"
)

// ActionConfig configures one score-driven action. Only the threshold field
// relevant to that action's fixed semantics is read: eject/lower_vp use
// ScoreBelow (score strictly below triggers), raise_vp uses ScoreAtOrAbove
// (score at or above triggers). VPStepPercentOfSelf/OfTotal only apply to
// lower_vp/raise_vp — see internal/rules for the dual-cap step formula.
type ActionConfig struct {
	ScoreBelow     int `yaml:"score_below"`
	ScoreAtOrAbove int `yaml:"score_at_or_above"`
	// Window is the gnomonitoring reporting period this action reads its
	// score from: last_24h | current_week | current_month | current_year.
	Window               string  `yaml:"window"`
	VPStepPercentOfSelf  float64 `yaml:"vp_step_percent_of_self"`
	VPStepPercentOfTotal float64 `yaml:"vp_step_percent_of_total"`
	RunScriptTemplate    string  `yaml:"run_script_template"`
	// call-mode fields (eject only; see internal/proposal doc comment):
	RealmPkgPath string   `yaml:"realm_pkg_path"`
	Function     string   `yaml:"function"`
	Args         []string `yaml:"args"`

	TitleTemplate       string `yaml:"title_template"`
	DescriptionTemplate string `yaml:"description_template"`
}

// Proposal describes the score-driven action model plus the lifecycle
// timers. The timers and VP bounds have global defaults, overridable per
// chain (whole-block replace — see the Runtime doc comment).
type Proposal struct {
	// ActionPriority is the order Evaluate checks actions in; the first
	// action whose threshold matches (on its own configured window) wins.
	// Defaults to [eject, lower_vp, raise_vp].
	ActionPriority []string `yaml:"action_priority"`
	// MinVotingPower is an absolute floor: lower_vp never proposes a target
	// below this. MaxVotingPowerShare is a ceiling expressed as % of the
	// chain's total valset VP (portable across networks): raise_vp never
	// proposes a target above it.
	MinVotingPower      int64   `yaml:"min_voting_power"`
	MaxVotingPowerShare float64 `yaml:"max_voting_power_share"`

	Cooldown        string `yaml:"cooldown"`
	RejectedBackoff string `yaml:"rejected_backoff"`
	MaxRejections   int    `yaml:"max_rejections"`
	// Submit selects the on-chain submission method: "run" (a .gno script via
	// gnokey maketx run) or "call" (a single MsgCall). "call" is eject-only —
	// see validate().
	Submit string `yaml:"submit"`

	Actions map[string]ActionConfig `yaml:"actions"`
}

// VoteTracking configures the vote-watcher worker.
type VoteTracking struct {
	VotePollInterval string `yaml:"vote_poll_interval"`
	GovdaoRealm      string `yaml:"govdao_realm"`
	StatusSource     string `yaml:"status_source"` // rpc | graphql
}

// Notifications configures outbound channels. Sent only after a successful
// on-chain broadcast (and on accepted/rejected outcomes).
type Notifications struct {
	Enabled  bool            `yaml:"enabled"`
	Discord  WebhookChannel  `yaml:"discord"`
	Slack    WebhookChannel  `yaml:"slack"`
	Telegram TelegramChannel `yaml:"telegram"`
}

type WebhookChannel struct {
	Enabled    bool   `yaml:"enabled"`
	WebhookURL string `yaml:"webhook_url"`
}

type TelegramChannel struct {
	Enabled  bool   `yaml:"enabled"`
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

// Storage configures run-history persistence.
type Storage struct {
	Driver     string `yaml:"driver"` // none | sqlite
	SQLitePath string `yaml:"sqlite_path"`
}

// Web reserves configuration for the health/UI HTTP server.
type Web struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// ChainConfig describes a single gno.land network with optional overrides.
type ChainConfig struct {
	RPCEndpoint        string `yaml:"rpc_endpoint"`
	GnowebURL          string `yaml:"gnoweb_url"`
	ChainID            string `yaml:"chain_id"`
	GnomonitoringChain string `yaml:"gnomonitoring_chain"`
	Enabled            bool   `yaml:"enabled"`
	// DryRun overrides the global dry_run default for this chain (nil =
	// inherit the global value). true runs the full evaluation pipeline but
	// never signs or broadcasts: the would-be proposal is logged and notified
	// (with the ejection script and a ready-to-run gnokey command attached)
	// instead. Meant both for rehearsing katana on a new network and as a
	// standing manual-approval mode.
	DryRun       *bool     `yaml:"dry_run"`
	PollInterval string    `yaml:"poll_interval"`
	Proposal     *Proposal `yaml:"proposal"`
	Gnokey       *Gnokey   `yaml:"gnokey"`
	Exceptions   []string  `yaml:"exceptions"`
}

// Runtime is the fully-resolved settings for one chain, global defaults merged
// with per-chain overrides.
type Runtime struct {
	Key                string
	RPCEndpoint        string
	GnowebURL          string
	ChainID            string
	GnomonitoringChain string
	DryRun             bool
	PollInterval       time.Duration
	VotePollInterval   time.Duration
	Proposal           Proposal
	Gnokey             Gnokey
}

// DefaultEjectScript is the gno.land valset-ejection script used in "run"
// submission mode. It routes through r/sys/validators/v3 with Power=0 (removal)
// and creates the GovDAO proposal. Placeholders: {operator_address} {title}
// {description}. Direct MsgCall is unsupported (the v3 builder is non-crossing),
// so a crossing script is required.
//
// CONFIRMED end-to-end on the gnoland-test devnet (PP-5): a proposal built from
// this script, once accepted and executed, emits ValidatorRemoved from
// r/sys/validators/v3 and drops the operator from the consensus valset (RPC
// /validators). Confirmed via `make accept-govdao` in gnoland-test (see issue #2).
const DefaultEjectScript = `package main

import (
	"gno.land/r/gov/dao"
	validators "gno.land/r/sys/validators/v3"
)

func main(cur realm) {
	pr := validators.NewValidatorProposalRequest(cross(cur),
		[]validators.ValoperChange{validators.NewValoperChange(address("{operator_address}"), 0)},
		"{title}",
		"{description}",
	)
	dao.MustCreateProposal(cross(cur), pr)
}
`

// DefaultVPChangeScript is the gno.land valset voting-power-change script
// used by lower_vp/raise_vp in "run" submission mode. Same crossing-script
// pattern as DefaultEjectScript, but with an arbitrary {target_power}
// instead of a hardcoded 0. Placeholders: {operator_address} {target_power}
// {title} {description}.
const DefaultVPChangeScript = `package main

import (
	"gno.land/r/gov/dao"
	validators "gno.land/r/sys/validators/v3"
)

func main(cur realm) {
	pr := validators.NewValidatorProposalRequest(cross(cur),
		[]validators.ValoperChange{validators.NewValoperChange(address("{operator_address}"), {target_power})},
		"{title}",
		"{description}",
	)
	dao.MustCreateProposal(cross(cur), pr)
}
`

// Load reads config.yaml, applies env overrides + defaults, then validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}
	c.applyEnvOverrides()
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyEnvOverrides() {
	setStr(&c.LogLevel, "KATANA_LOG_LEVEL")
	setStr(&c.Gnomonitoring.BaseURL, "KATANA_GNOMON_BASE_URL")
	setStr(&c.Gnomonitoring.APIToken, "KATANA_GNOMON_API_TOKEN")
	setStr(&c.Gnokey.Bin, "KATANA_GNOKEY_BIN")
	setStr(&c.Gnokey.KeyName, "KATANA_GNOKEY_KEY")
	setStr(&c.Gnokey.Address, "KATANA_GNOKEY_ADDRESS")
	setStr(&c.Gnokey.Home, "KATANA_GNOKEY_HOME")
	setStr(&c.Gnokey.Password, "KATANA_GNOKEY_PASSWORD")
	setStr(&c.Gnokey.Mnemonic, "KATANA_GNOKEY_MNEMONIC")
	setStr(&c.Notifications.Discord.WebhookURL, "KATANA_DISCORD_WEBHOOK")
	setStr(&c.Notifications.Slack.WebhookURL, "KATANA_SLACK_WEBHOOK")
	setStr(&c.Notifications.Telegram.BotToken, "KATANA_TELEGRAM_TOKEN")
	setStr(&c.Notifications.Telegram.ChatID, "KATANA_TELEGRAM_CHAT_ID")
}

func (c *Config) applyDefaults() {
	setIfEmpty(&c.LogLevel, "info")
	setIfEmpty(&c.PollInterval, "5m")
	if c.Gnomonitoring.TimeoutSeconds == 0 {
		c.Gnomonitoring.TimeoutSeconds = 10
	}
	setIfEmpty(&c.Gnomonitoring.ReportsPath, "/api/reports/validators")
	setIfEmpty(&c.Gnomonitoring.InfoPath, "/info")
	setIfEmpty(&c.Gnokey.Bin, "gnokey")
	setIfEmpty(&c.Gnokey.GasFee, "1000000ugnot")
	if c.Gnokey.GasWanted == 0 {
		c.Gnokey.GasWanted = 20_000_000
	}
	setIfEmpty(&c.Proposal.Cooldown, "720h")
	setIfEmpty(&c.Proposal.RejectedBackoff, "720h")
	setIfEmpty(&c.Proposal.Submit, "run")
	if len(c.Proposal.ActionPriority) == 0 {
		c.Proposal.ActionPriority = []string{ActionEject, ActionLowerVP, ActionRaiseVP}
	}
	if c.Proposal.MinVotingPower == 0 {
		c.Proposal.MinVotingPower = 100
	}
	if c.Proposal.MaxVotingPowerShare == 0 {
		c.Proposal.MaxVotingPowerShare = 15
	}
	if c.Proposal.Actions == nil {
		c.Proposal.Actions = map[string]ActionConfig{}
	}
	c.Proposal.Actions[ActionEject] = mergeActionDefault(c.Proposal.Actions[ActionEject], ActionConfig{
		ScoreBelow: 30, Window: "current_month",
		RunScriptTemplate:   DefaultEjectScript,
		TitleTemplate:       "Eject validator {moniker} ({address}) from the valset",
		DescriptionTemplate: "Automated ejection proposal generated by katana.\nValidator: {moniker} ({address})\nScore: {score}/100 (tier {tier}) over {window}\nReason: {reason}\n",
	})
	c.Proposal.Actions[ActionLowerVP] = mergeActionDefault(c.Proposal.Actions[ActionLowerVP], ActionConfig{
		ScoreBelow: 60, Window: "current_week",
		VPStepPercentOfSelf: -20, VPStepPercentOfTotal: -2,
		RunScriptTemplate:   DefaultVPChangeScript,
		TitleTemplate:       "Lower voting power of validator {moniker} ({address})",
		DescriptionTemplate: "Automated voting-power reduction generated by katana.\nValidator: {moniker} ({address})\nScore: {score}/100 (tier {tier}) over {window}\nVoting power: {current_power} -> {target_power}\nReason: {reason}\n",
	})
	c.Proposal.Actions[ActionRaiseVP] = mergeActionDefault(c.Proposal.Actions[ActionRaiseVP], ActionConfig{
		ScoreAtOrAbove: 85, Window: "current_year",
		VPStepPercentOfSelf: 20, VPStepPercentOfTotal: 2,
		RunScriptTemplate:   DefaultVPChangeScript,
		TitleTemplate:       "Raise voting power of validator {moniker} ({address})",
		DescriptionTemplate: "Automated voting-power increase generated by katana.\nValidator: {moniker} ({address})\nScore: {score}/100 (tier {tier}) over {window}\nVoting power: {current_power} -> {target_power}\nReason: {reason}\n",
	})
	setIfEmpty(&c.VoteTracking.VotePollInterval, "5m")
	setIfEmpty(&c.VoteTracking.GovdaoRealm, "gno.land/r/gov/dao")
	setIfEmpty(&c.VoteTracking.StatusSource, "rpc")
	setIfEmpty(&c.Storage.Driver, "none")
	setIfEmpty(&c.Storage.SQLitePath, "./data/katana.db")
	setIfEmpty(&c.Web.Listen, ":8990")
	setIfEmpty(&c.ValopersRealm, "gno.land/r/gnops/valopers")
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Gnomonitoring.BaseURL) == "" {
		return fmt.Errorf("gnomonitoring.base_url is required")
	}
	if len(c.Chains) == 0 {
		return fmt.Errorf("no chains defined under 'chains:'")
	}
	if len(c.EnabledChains()) == 0 {
		return fmt.Errorf("no chain is enabled")
	}
	switch c.Storage.Driver {
	case "none", "sqlite":
	default:
		return fmt.Errorf("storage.driver %q is invalid (want \"none\" or \"sqlite\")", c.Storage.Driver)
	}
	// The Noop store keeps no lifecycle history, so a broadcast-capable chain
	// running on it would re-broadcast the same ejection every cycle. Refuse to
	// start rather than spam GovDAO. Dry-run-only configs may still run without
	// persistence (with degraded rehearsal dedup — warned at startup).
	if c.Storage.Driver == "none" {
		if armed := c.BroadcastCapableChains(); len(armed) > 0 {
			return fmt.Errorf("storage.driver %q (the default) cannot be used while chains %v are broadcast-capable: without persistence the lifecycle gate keeps no history and katana would re-broadcast the same ejection every cycle; set storage.driver: sqlite (or dry_run: true on every enabled chain)", c.Storage.Driver, armed)
		}
	}
	if _, err := parseDur(c.PollInterval); err != nil {
		return fmt.Errorf("poll_interval: %w", err)
	}
	if _, err := parseDur(c.VoteTracking.VotePollInterval); err != nil {
		return fmt.Errorf("vote_tracking.vote_poll_interval: %w", err)
	}
	for _, id := range c.EnabledChains() {
		if _, err := c.Runtime(id); err != nil {
			return fmt.Errorf("chain %q: %w", id, err)
		}
	}
	return nil
}

// Runtime resolves the effective settings for a chain (global merged with the
// per-chain overrides), validating durations and required fields.
func (c *Config) Runtime(chainID string) (Runtime, error) {
	ch, ok := c.Chains[chainID]
	if !ok {
		return Runtime{}, fmt.Errorf("unknown chain")
	}
	if ch.RPCEndpoint == "" {
		return Runtime{}, fmt.Errorf("rpc_endpoint is required")
	}
	if ch.ChainID == "" {
		return Runtime{}, fmt.Errorf("chain_id is required")
	}
	if ch.GnomonitoringChain == "" {
		return Runtime{}, fmt.Errorf("gnomonitoring_chain is required")
	}

	pollStr := c.PollInterval
	if ch.PollInterval != "" {
		pollStr = ch.PollInterval
	}
	poll, err := parseDur(pollStr)
	if err != nil {
		return Runtime{}, fmt.Errorf("poll_interval: %w", err)
	}
	votePoll, err := parseDur(c.VoteTracking.VotePollInterval)
	if err != nil {
		return Runtime{}, fmt.Errorf("vote_poll_interval: %w", err)
	}

	dryRun := c.DryRun
	if ch.DryRun != nil {
		dryRun = *ch.DryRun
	}

	prop := c.Proposal
	if ch.Proposal != nil {
		prop = *ch.Proposal
	}
	if _, err := parseDur(prop.Cooldown); err != nil {
		return Runtime{}, fmt.Errorf("proposal.cooldown: %w", err)
	}
	if _, err := parseDur(prop.RejectedBackoff); err != nil {
		return Runtime{}, fmt.Errorf("proposal.rejected_backoff: %w", err)
	}
	for name, ac := range prop.Actions {
		switch ac.Window {
		case "last_24h", "current_week", "current_month", "current_year":
		default:
			return Runtime{}, fmt.Errorf("proposal.actions.%s.window: %q is not a known gnomonitoring period", name, ac.Window)
		}
	}
	for _, name := range prop.ActionPriority {
		if name != ActionEject && name != ActionLowerVP && name != ActionRaiseVP {
			return Runtime{}, fmt.Errorf("proposal.action_priority: unknown action %q", name)
		}
		if _, ok := prop.Actions[name]; !ok {
			continue
		}
		if prop.Submit == "call" && name != ActionEject {
			return Runtime{}, fmt.Errorf("proposal.submit \"call\" is only supported for the eject action; %q must use \"run\"", name)
		}
	}

	gk := c.Gnokey
	if ch.Gnokey != nil {
		gk = gk.mergedWith(*ch.Gnokey)
	}
	if gk.KeyName == "" {
		return Runtime{}, fmt.Errorf("gnokey.key_name is required")
	}

	return Runtime{
		Key:                chainID,
		RPCEndpoint:        ch.RPCEndpoint,
		GnowebURL:          ch.GnowebURL,
		ChainID:            ch.ChainID,
		GnomonitoringChain: ch.GnomonitoringChain,
		DryRun:             dryRun,
		PollInterval:       poll,
		VotePollInterval:   votePoll,
		Proposal:           prop,
		Gnokey:             gk,
	}, nil
}

// Cooldown parses the resolved cooldown duration.
func (p Proposal) CooldownDur() time.Duration { d, _ := parseDur(p.Cooldown); return d }

// RejectedBackoffDur parses the resolved rejection backoff duration.
func (p Proposal) RejectedBackoffDur() time.Duration { d, _ := parseDur(p.RejectedBackoff); return d }

// EnabledChains returns the IDs of all enabled chains, sorted alphabetically.
func (c *Config) EnabledChains() []string {
	ids := make([]string, 0, len(c.Chains))
	for id, ch := range c.Chains {
		if ch.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// BroadcastCapableChains returns the IDs of all enabled chains that may sign
// and broadcast (i.e. not in dry_run, resolving the global default and any
// per-chain override), sorted alphabetically.
func (c *Config) BroadcastCapableChains() []string {
	ids := make([]string, 0, len(c.Chains))
	for id, ch := range c.Chains {
		dryRun := c.DryRun
		if ch.DryRun != nil {
			dryRun = *ch.DryRun
		}
		if ch.Enabled && !dryRun {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// IsException reports whether a validator is protected on the given chain.
func (c *Config) IsException(chainID, addr, moniker string) bool {
	if matchException(c.Exceptions, addr, moniker) {
		return true
	}
	if ch, ok := c.Chains[chainID]; ok {
		return matchException(ch.Exceptions, addr, moniker)
	}
	return false
}

func matchException(list []string, addr, moniker string) bool {
	for _, e := range list {
		if e == "" {
			continue
		}
		if strings.EqualFold(e, addr) || (moniker != "" && e == moniker) {
			return true
		}
	}
	return false
}

func parseDur(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	return time.ParseDuration(s)
}

func setStr(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		*dst = v
	}
}

func setIfEmpty(dst *string, def string) {
	if *dst == "" {
		*dst = def
	}
}

// mergeActionDefault fills any zero-valued field of cur from def, so a chain
// or global override that only sets e.g. score_below still gets sane
// defaults for window/steps/templates.
func mergeActionDefault(cur, def ActionConfig) ActionConfig {
	if cur.ScoreBelow == 0 {
		cur.ScoreBelow = def.ScoreBelow
	}
	if cur.ScoreAtOrAbove == 0 {
		cur.ScoreAtOrAbove = def.ScoreAtOrAbove
	}
	if cur.Window == "" {
		cur.Window = def.Window
	}
	if cur.VPStepPercentOfSelf == 0 {
		cur.VPStepPercentOfSelf = def.VPStepPercentOfSelf
	}
	if cur.VPStepPercentOfTotal == 0 {
		cur.VPStepPercentOfTotal = def.VPStepPercentOfTotal
	}
	if cur.RunScriptTemplate == "" {
		cur.RunScriptTemplate = def.RunScriptTemplate
	}
	if cur.RealmPkgPath == "" {
		cur.RealmPkgPath = def.RealmPkgPath
	}
	if cur.Function == "" {
		cur.Function = def.Function
	}
	if len(cur.Args) == 0 {
		cur.Args = def.Args
	}
	if cur.TitleTemplate == "" {
		cur.TitleTemplate = def.TitleTemplate
	}
	if cur.DescriptionTemplate == "" {
		cur.DescriptionTemplate = def.DescriptionTemplate
	}
	return cur
}
