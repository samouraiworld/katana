// Package config loads and validates katana's configuration.
//
// The entire runtime configuration — including every value that would
// traditionally be passed through environment variables — lives in a single
// config.yaml file. Environment variables are supported only as optional
// overrides for secrets and per-deployment values (see applyEnvOverrides).
//
// Many blocks (conditions, proposal, gnokey, mode, poll interval) have a global
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
	Conditions    Conditions    `yaml:"conditions"`
	Proposal      Proposal      `yaml:"proposal"`
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
	BaseURL          string `yaml:"base_url"`
	APIToken         string `yaml:"api_token"`
	TimeoutSeconds   int    `yaml:"timeout_seconds"`
	MissedBlocksPath string `yaml:"missed_blocks_path"`
	AlertsPath       string `yaml:"alerts_path"`
	InfoPath         string `yaml:"info_path"`
	DiscoverChains   bool   `yaml:"discover_chains"`
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
	return g
}

// Conditions describes when a validator becomes eligible for an ejection
// proposal. RequireAll combines enabled conditions as AND (true) or OR (false).
type Conditions struct {
	RequireAll     bool             `yaml:"require_all"`
	MissedBlocks   MissedBlocksCond `yaml:"missed_blocks"`
	CriticalAlerts CriticalCond     `yaml:"critical_alerts"`
}

// MissedBlocksCond triggers when missed blocks over Interval exceed Max.
type MissedBlocksCond struct {
	Enabled  bool   `yaml:"enabled"`
	Max      int    `yaml:"max"`
	Interval string `yaml:"interval"` // gnomonitoring period keyword
}

// CriticalCond triggers when CRITICAL alerts over Interval exceed Max.
type CriticalCond struct {
	Enabled  bool   `yaml:"enabled"`
	Max      int    `yaml:"max"`
	Interval string `yaml:"interval"` // Go duration
}

// Duration parses the critical-alert interval as a time.Duration.
func (c CriticalCond) Duration() (time.Duration, error) {
	return time.ParseDuration(c.Interval)
}

// Proposal describes the on-chain GovDAO ejection entrypoint plus the lifecycle
// timers. Mode and the timers have global defaults, overridable per chain.
type Proposal struct {
	Mode            string `yaml:"mode"` // dry-run | broadcast
	Cooldown        string `yaml:"cooldown"`
	RejectedBackoff string `yaml:"rejected_backoff"`
	MaxRejections   int    `yaml:"max_rejections"`
	// Submit selects the on-chain submission method: "run" (a .gno script via
	// gnokey maketx run — required for gno.land valset ejection) or "call" (a
	// single MsgCall to RealmPkgPath.Function with Args).
	Submit            string `yaml:"submit"`
	RunScriptTemplate string `yaml:"run_script_template"`
	// call-mode fields:
	RealmPkgPath string   `yaml:"realm_pkg_path"`
	Function     string   `yaml:"function"`
	Args         []string `yaml:"args"`

	TitleTemplate       string `yaml:"title_template"`
	DescriptionTemplate string `yaml:"description_template"`
	OutputDir           string `yaml:"output_dir"`
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
	RPCEndpoint        string      `yaml:"rpc_endpoint"`
	GnowebURL          string      `yaml:"gnoweb_url"`
	ChainID            string      `yaml:"chain_id"`
	GnomonitoringChain string      `yaml:"gnomonitoring_chain"`
	Enabled            bool        `yaml:"enabled"`
	PollInterval       string      `yaml:"poll_interval"`
	Mode               string      `yaml:"mode"`
	Conditions         *Conditions `yaml:"conditions"`
	Proposal           *Proposal   `yaml:"proposal"`
	Gnokey             *Gnokey     `yaml:"gnokey"`
	Exceptions         []string    `yaml:"exceptions"`
}

// Runtime is the fully-resolved settings for one chain, global defaults merged
// with per-chain overrides.
type Runtime struct {
	Key                string
	RPCEndpoint        string
	GnowebURL          string
	ChainID            string
	GnomonitoringChain string
	PollInterval       time.Duration
	VotePollInterval   time.Duration
	Mode               string
	Conditions         Conditions
	Proposal           Proposal
	Gnokey             Gnokey
}

// DefaultEjectScript is the gno.land valset-ejection script used in "run"
// submission mode. It routes through r/sys/validators/v3 with Power=0 (removal)
// and creates the GovDAO proposal. Placeholders: {operator_address} {title}
// {description}. Confirmed against gno.land examples (r/sys/validators/v3 —
// direct MsgCall is unsupported, so a crossing script is required).
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
	setIfEmpty(&c.Gnomonitoring.MissedBlocksPath, "/missing_block")
	setIfEmpty(&c.Gnomonitoring.AlertsPath, "/admin/alerts")
	setIfEmpty(&c.Gnomonitoring.InfoPath, "/info")
	setIfEmpty(&c.Gnokey.Bin, "gnokey")
	setIfEmpty(&c.Gnokey.GasFee, "1000000ugnot")
	if c.Gnokey.GasWanted == 0 {
		c.Gnokey.GasWanted = 20_000_000
	}
	setIfEmpty(&c.Proposal.Mode, "dry-run")
	setIfEmpty(&c.Proposal.Cooldown, "720h")
	setIfEmpty(&c.Proposal.RejectedBackoff, "720h")
	setIfEmpty(&c.Proposal.Submit, "run")
	setIfEmpty(&c.Proposal.RunScriptTemplate, DefaultEjectScript)
	setIfEmpty(&c.Proposal.OutputDir, "./data/proposals")
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
// per-chain overrides), validating durations, mode, and required fields.
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

	mode := c.Proposal.Mode
	if ch.Mode != "" {
		mode = ch.Mode
	}
	if mode != "dry-run" && mode != "broadcast" {
		return Runtime{}, fmt.Errorf("mode %q is invalid (want \"dry-run\" or \"broadcast\")", mode)
	}

	cond := c.Conditions
	if ch.Conditions != nil {
		cond = *ch.Conditions
	}
	if cond.CriticalAlerts.Enabled {
		if _, err := cond.CriticalAlerts.Duration(); err != nil {
			return Runtime{}, fmt.Errorf("critical_alerts.interval: %w", err)
		}
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

	gk := c.Gnokey
	if ch.Gnokey != nil {
		gk = gk.mergedWith(*ch.Gnokey)
	}
	if mode == "broadcast" && gk.KeyName == "" {
		return Runtime{}, fmt.Errorf("gnokey.key_name is required when mode is broadcast")
	}

	return Runtime{
		Key:                chainID,
		RPCEndpoint:        ch.RPCEndpoint,
		GnowebURL:          ch.GnowebURL,
		ChainID:            ch.ChainID,
		GnomonitoringChain: ch.GnomonitoringChain,
		PollInterval:       poll,
		VotePollInterval:   votePoll,
		Mode:               mode,
		Conditions:         cond,
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
