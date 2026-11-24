package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Headless HeadlessConfig `toml:"headless"`

	Repo             string `toml:"repo"`
	BeadsDir         string `toml:"beads_dir"`
	BranchPrefix     string `toml:"branch_prefix"`
	BaseBranch       string `toml:"base_branch"`
	WorkerAgent      string `toml:"worker_agent"`
	WorkerBackend    string `toml:"worker_backend"` // empty/herdr (default), or pi-rpc; local workers only
	CoordinatorAgent string `toml:"coordinator_agent"`
	CoordinatorName  string `toml:"coordinator_name"`
	TickSeconds      int    `toml:"tick_seconds"`
	GHSeconds        int    `toml:"gh_seconds"`
	// A pane must have been idle this long before the ticker types into it:
	// on Herdr 0.9.1 a prompt merges with whatever the user has half-typed.
	IdleSeconds int  `toml:"idle_seconds"`
	Nudge       bool `toml:"nudge"`
	// PromptDebounceSeconds is how long a worker approval/question prompt
	// must keep showing before the ticker surfaces it to the inbox; a prompt
	// that clears sooner (a judge transient) never surfaces. 0 disables the
	// debounce: prompts surface on the tick they are first seen.
	PromptDebounceSeconds int `toml:"prompt_debounce_seconds"`
	// ReviewBumpHours is how long an open, non-draft, green, mergeable
	// REVIEW_REQUIRED PR with zero unresolved review threads may stay on the
	// same head SHA without a new human review before the ticker emits one
	// bounded review_bump inbox reminder. 0 disables review_bump reminders.
	ReviewBumpHours int `toml:"review_bump_hours"`
	// Only keys with these team prefixes count as Linear issues; empty accepts
	// any key outside a small denylist.
	LinearPrefixes []string `toml:"linear_prefixes"`
	// LinearWorkspace is the workspace in Linear's URLs (linear.app/<it>/...),
	// for opening an issue from the board when its bead links none.
	LinearWorkspace string `toml:"linear_workspace"`
	// VerifyMinutes is how often the ticker re-checks stale beads for work
	// that is already done; 0 turns it off. Each pass rebuilds only what
	// changed since the last, and each bead in full once a day.
	VerifyMinutes int `toml:"verify_minutes"`
	// StaleDays is how long a bead goes untouched before verify checks it.
	StaleDays int `toml:"stale_days"`
	// NeedsMeParkedDays is how long an agentless, non-critical needs_me bead
	// may go untouched before views group it under Parked / Dormant instead of
	// Active (default 7; 0 disables age-based parking).
	NeedsMeParkedDays *int `toml:"needs_me_parked_days"`
	// VerifyMax caps the GitHub calls a verify pass makes beyond its probes.
	VerifyMax int `toml:"verify_max"`
	// GHMinRemaining skips a verify pass while fewer GitHub GraphQL points
	// than this are left: the hourly limit is shared with every agent.
	GHMinRemaining int `toml:"gh_min_remaining"`
	// HerdrSocket is the herdr server the coordinator runs on. The ticker
	// always talks to it, whichever server's plugin startup launched it.
	HerdrSocket string `toml:"herdr_socket"`

	// Repos are named repositories a bead selects with a repo:<name> label;
	// Repo is the default.
	Repos map[string]string `toml:"repos"`

	// Schedule is how dispatch picks a host when none is given; only
	// "manual" (local unless --host or a host: label) exists so far.
	Schedule string `toml:"schedule"`
	// Hosts are the machines workers run on ([hosts.<name>]); local is
	// implicit.
	Hosts map[string]HostConfig `toml:"hosts"`
	// RepoNeeds are host labels every bead in a repo needs, by repo name.
	RepoNeeds map[string][]string `toml:"repo_needs"`
	// Slot derivation defaults for every host; see Config.slots.
	CoresPerWorker  int `toml:"cores_per_worker"`
	RAMPerWorkerGiB int `toml:"ram_per_worker_gib"`
	ReserveGiB      int `toml:"reserve_gib"`

	// CoordinatorRules are extra lines appended to the coordinator guide's
	// rules, one bullet each.
	CoordinatorRules []string `toml:"coordinator_rules"`

	// Agents holds per-kind settings ([agents.muse]), for workers and the
	// coordinator alike.
	Agents map[string]AgentConfig `toml:"agents"`

	// Autonomy is the dial: how far beads go without TJ; see
	// docs/design/autonomy-dial.md. It also decides what auto_close and
	// auto_resolve did: the ticker closes and resolves at level 3 and above.
	Autonomy AutonomyConfig `toml:"autonomy"`

	// Verifier checks in-scope beads before `quartermaster close` closes
	// them; see accept.go.
	Verifier VerifierConfig `toml:"verifier"`

	// Tap serves signed one-time approval links on the tailnet, so TJ can
	// answer a worker's approval prompt from a phone; see tap.go and
	// docs/approval-taps.md. Off unless [tap] enabled is set.
	Tap TapConfig `toml:"tap"`

	// Watches configures declarative per-bead watches (`quartermaster watch`);
	// see watch.go and docs/watches.md. Command probes fail closed unless
	// [watches] allow_cmd = true is set.
	Watches WatchesConfig `toml:"watches"`

	// Desktop configures event-driven desktop notifications (`notify-send`)
	// delivered by the ticker; see desktop.go and docs/desktop-alerts.md.
	Desktop DesktopConfig `toml:"desktop"`

	// StallRetry configures bounded automatic recovery when a pi worker stops
	// a turn on a transient ACP stall error (stallretry.go).
	StallRetry StallRetryConfig `toml:"stall_retry"`

	// OrphanFinish configures the bounded periodic finisher sweep for agentless
	// merged and completed rolling-out beads (orphanfinish.go).
	OrphanFinish OrphanFinishConfig `toml:"orphan_finish"`

	// InboxRunner configures the opt-in headless inbox runner that drives
	// autonomous coordinator turns for allowlisted inbox events while the
	// interactive coordinator is idle or closed (inboxrunner.go, docs/inbox-runner.md).
	InboxRunner InboxRunnerConfig `toml:"inbox_runner"`
}

// DesktopConfig is [desktop]: controls event-driven desktop notifications
// delivered via notify-send. Omitted booleans default to true (on);
// cooldown_seconds defaults to 60 (0 disables cooldown).
type DesktopConfig struct {
	// Enabled turns ticker desktop notifications on or off (default true).
	Enabled *bool `toml:"enabled"`
	// PRMerged notifies when a tracked PR merges (default true).
	PRMerged *bool `toml:"pr_merged"`
	// PRGreen notifies when an open non-draft PR's checks are green and it is
	// awaiting review (default true).
	PRGreen *bool `toml:"pr_green"`
	// PRReady notifies when an open PR becomes ready to merge (default true).
	PRReady *bool `toml:"pr_ready"`
	// NeedsMe notifies when a bead enters needs_me or a worker hits an
	// approval prompt (default true).
	NeedsMe *bool `toml:"needs_me"`
	// Blocked notifies when a bead enters blocked status (default true).
	Blocked *bool `toml:"blocked"`
	// NeedsMeSummary includes the bead ID and a concise 1-line sanitized
	// summary on needs_me notifications (default true). Set false for fixed
	// generic wording with no bead ID or note summary.
	NeedsMeSummary *bool `toml:"needs_me_summary"`
	// CooldownSeconds is the minimum interval between desktop notifications
	// so bursts coalesce into one alert (default 60; 0 disables cooldown).
	CooldownSeconds *int `toml:"cooldown_seconds"`
}

func desktopBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func (c DesktopConfig) enabled() bool               { return desktopBool(c.Enabled, true) }
func (c DesktopConfig) notifyPRMerged() bool        { return c.enabled() && desktopBool(c.PRMerged, true) }
func (c DesktopConfig) notifyPRGreen() bool         { return c.enabled() && desktopBool(c.PRGreen, true) }
func (c DesktopConfig) notifyPRReady() bool         { return c.enabled() && desktopBool(c.PRReady, true) }
func (c DesktopConfig) notifyNeedsMe() bool         { return c.enabled() && desktopBool(c.NeedsMe, true) }
func (c DesktopConfig) notifyBlocked() bool         { return c.enabled() && desktopBool(c.Blocked, true) }
func (c DesktopConfig) includeNeedsMeSummary() bool { return desktopBool(c.NeedsMeSummary, true) }

func (c DesktopConfig) allowsMilestone(reason string) bool {
	switch reason {
	case "pr_merged":
		return c.notifyPRMerged()
	case "pr_green":
		return c.notifyPRGreen()
	case "pr_ready":
		return c.notifyPRReady()
	default:
		return false
	}
}

func (c DesktopConfig) cooldown() time.Duration {
	if c.CooldownSeconds == nil {
		return time.Minute
	}
	if *c.CooldownSeconds <= 0 {
		return 0
	}
	return time.Duration(*c.CooldownSeconds) * time.Second
}

func (c Config) validateDesktop() error {
	if c.Desktop.CooldownSeconds != nil && *c.Desktop.CooldownSeconds < 0 {
		return fmt.Errorf("[desktop] cooldown_seconds %d is negative; use 0 to disable cooldown", *c.Desktop.CooldownSeconds)
	}
	return nil
}

// TapConfig is [tap]: the approval-tap endpoint the ticker runs. Devices are
// the Tailscale node names (the part before the tailnet domain) allowed to
// tap; a tap from anywhere else, this machine included, is refused. The
// listener binds this machine's Tailscale IPv4 only.
type TapConfig struct {
	Enabled bool     `toml:"enabled"`
	Port    int      `toml:"port"` // 0 takes defaultTapPort
	Devices []string `toml:"devices"`
}

func (c TapConfig) port() int {
	if c.Port == 0 {
		return defaultTapPort
	}
	return c.Port
}

// validateTap fails closed: an enabled tap endpoint with no device allowlist
// would take a tap from any tailnet node holding a link.
func (c Config) validateTap() error {
	t := c.Tap
	if t.Port < 0 || t.Port > 65535 {
		return fmt.Errorf("[tap] port %d is out of range", t.Port)
	}
	if t.Enabled && len(t.Devices) == 0 {
		return errors.New("[tap] enabled needs devices: the Tailscale node names allowed to tap (see `tailscale status`)")
	}
	return nil
}

type AgentConfig struct {
	// Args go to the agent's own CLI on every start, dispatch and resume.
	Args []string `toml:"args"`
	// CoordinatorArgs replace Args when this kind runs the coordinator, such
	// as a different model from the workers'; unset uses Args.
	CoordinatorArgs []string `toml:"coordinator_args"`
	// BypassArgs follow Args when a worker starts under a 9 lease, turning
	// the agent's own permission checks off; unset takes the kind's default,
	// and [] keeps a kind off bypass. The coordinator never gets them.
	BypassArgs []string `toml:"bypass_args"`
	// Env is "NAME=value" pairs exported in the agent's pane before it
	// starts, after BEADS_ACTOR and BEADS_DIR, on every host.
	Env []string `toml:"env"`
	// Ask (pi only) lists the bash commands, as regular expressions, that a
	// worker asks quartermaster about when a guard hook would ask the user,
	// instead of blocking them: the question shows in the inbox and
	// `quartermaster prompt`, and `quartermaster answer` answers it. Empty,
	// the default, asks about nothing. A hook's deny still blocks.
	Ask []string `toml:"ask"`
	// AskTimeout (pi only) is how long a worker waits for that answer before
	// it blocks the command anyway (default 10m).
	AskTimeout string `toml:"ask_timeout"`
	// The pi virtual-model router (pi/router.ts, docs/pi.md): the ordered
	// physical model chains quartermaster/worker-auto and
	// quartermaster/coordinator-auto route through (defaulting to
	// Antigravity Neon XHigh, then Claude, then OpenAI), optional per-model
	// :thinking suffixes (e.g. anthropic/claude-opus-5-5:high), global
	// switch/recover headroom thresholds (percent left in the tightest live
	// window), and optional per-provider threshold/recover overrides under
	// [agents.pi.router_providers.<provider>]. router_worker_model,
	// router_coordinator_model and router_fallback_model remain supported
	// for backward compatibility.
	RouterWorkerModels      []string                        `toml:"router_worker_models"`
	RouterCoordinatorModels []string                        `toml:"router_coordinator_models"`
	RouterWorkerModel       string                          `toml:"router_worker_model"`
	RouterCoordinatorModel  string                          `toml:"router_coordinator_model"`
	RouterFallbackModel     string                          `toml:"router_fallback_model"`
	RouterThreshold         *float64                        `toml:"router_threshold"`
	RouterRecover           *float64                        `toml:"router_recover"`
	RouterProviders         map[string]RouterProviderConfig `toml:"router_providers"`
}

// RouterProviderConfig is [agents.pi.router_providers.<provider>]: optional
// per-provider headroom thresholds for the pi virtual-model router. Omitted
// bounds inherit [agents.pi] router_threshold / router_recover.
type RouterProviderConfig struct {
	Threshold *float64 `toml:"threshold"`
	Recover   *float64 `toml:"recover"`
}

// defaultAgentArgs apply to a kind with no [agents.<kind>] table. Muse's
// sandbox scrubs the environment (BEADS_DIR) and blocks gh's credentials, so
// a sandboxed Muse worker can't use bd or open a PR. Its :auto-review profile
// keeps the approval judge but lets the judge, not a human, decide, so an
// unattended worker doesn't wait on a prompt nobody is watching. agy's Auto
// mode is the same idea (a review judge for commands that would leave its
// sandbox); it is agy's own default, pinned here so a saved /config mode
// such as plan doesn't silently turn a worker into a planner.
var defaultAgentArgs = map[string][]string{
	"agy":  {"--mode", "auto"},
	"muse": {"--disable-sandbox", "--permission-profile", ":auto-review"},
	// pi: --approve trusts the worktree's own .pi resources, since a fresh
	// worktree has no saved trust decision to skip the prompt with. The
	// model is quartermaster's virtual auto model: Antigravity Neon XHigh
	// first, failing over through Claude and OpenAI on low quota or errors
	// (pi/router.ts). A physical --model in [agents.pi] args opts out.
	"pi": {"--approve", "--model", piWorkerVirtualModel},
}

// defaultCoordinatorArgs apply when a kind runs the coordinator and has no
// [agents.<kind>] table.
var defaultCoordinatorArgs = map[string][]string{
	"pi": {"--approve", "--model", piCoordinatorVirtualModel},
}

// defaultBypassArgs are each kind's switch for its own permission checks,
// used only under a 9 lease (docs/design/autonomy-dial.md, D8).
var defaultBypassArgs = map[string][]string{
	"claude": {"--dangerously-skip-permissions"},
	"codex":  {"--dangerously-bypass-approvals-and-sandbox"},
}

// agentEnv is the shell command that exports what bd needs into an agent's
// pane before it starts: BEADS_DIR, so bd finds the shared database from a
// worktree, and BEADS_ACTOR, so the agent acts as its kind. Claude and Codex
// also set these in their own configs; Muse has nowhere to, and its hooks and
// unsandboxed shell inherit the pane's environment.
//
// On a remote host BEADS_DIR stays out: bd there is a shim to the desktop's
// bd-proxy, which pins the database itself.
func (c Config) agentEnv(kind string) string { return c.agentEnvOn(localHost, kind) }

func (c Config) agentEnvOn(host, kind string) string {
	cmd := "export BEADS_ACTOR=" + shellQuote(kind)
	if c.BeadsDir != "" && host == localHost {
		cmd += " BEADS_DIR=" + shellQuote(c.BeadsDir)
	}
	for _, e := range c.Agents[kind].Env {
		k, v, _ := strings.Cut(e, "=")
		cmd += " " + k + "=" + shellQuote(v)
	}
	return cmd
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateAgents checks each [agents.<kind>] env entry is NAME=value with a
// name the shell can export unquoted. BEADS_ACTOR and BEADS_DIR are
// quartermaster's to set: a remote pane must not get BEADS_DIR.
func (c Config) validateAgents() error {
	if c.WorkerBackend != "" && c.WorkerBackend != "herdr" && c.WorkerBackend != rpcBackend {
		return fmt.Errorf("unknown worker_backend %q (herdr or pi-rpc)", c.WorkerBackend)
	}
	if err := c.validatePiAsk(); err != nil {
		return err
	}
	if err := c.validatePiRouter(); err != nil {
		return err
	}
	for kind, a := range c.Agents {
		for _, e := range a.Env {
			k, _, ok := strings.Cut(e, "=")
			if !ok || !envNameRE.MatchString(k) {
				return fmt.Errorf("[agents.%s] env: %q is not NAME=value", kind, e)
			}
			if k == "BEADS_ACTOR" || k == "BEADS_DIR" {
				return fmt.Errorf("[agents.%s] env: quartermaster sets %s itself", kind, k)
			}
		}
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// agentArgs is what a kind starts with: its configured args (else the
// default), its bypass args when bypass, then extra, such as a resume
// subcommand. Configured args go first because codex takes its root options
// before a subcommand.
func (c Config) agentArgs(kind string, bypass bool, extra ...string) []string {
	args := defaultAgentArgs[kind]
	if a, ok := c.Agents[kind]; ok {
		args = a.Args
	}
	args = slices.Clone(args)
	if bypass {
		args = append(args, c.bypassArgs(kind)...)
	}
	return append(args, extra...)
}

// coordinatorArgs is what a kind starts the coordinator with: its
// coordinator_args if set, else its default coordinator args when it has no
// [agents.<kind>] table, else what a worker of that kind starts with. The
// coordinator never gets bypass args.
func (c Config) coordinatorArgs(kind string) []string {
	a, ok := c.Agents[kind]
	switch {
	case ok && a.CoordinatorArgs != nil:
		return slices.Clone(a.CoordinatorArgs)
	case !ok && defaultCoordinatorArgs[kind] != nil:
		return slices.Clone(defaultCoordinatorArgs[kind])
	}
	return c.agentArgs(kind, false)
}

func (c Config) bypassArgs(kind string) []string {
	if a, ok := c.Agents[kind]; ok && a.BypassArgs != nil {
		return a.BypassArgs
	}
	return defaultBypassArgs[kind]
}

// launchBypass reports whether b's worker starts with bypass args: only
// under a 9 lease, the stop file counted, and only on this machine, since the
// ticker can restart a worker only through resume, which is local.
func (c Config) launchBypass(b Bead, host string) bool {
	lvl, _ := c.beadLevel(b, autonomyStopped())
	return lvl == 9 && host == localHost
}

func defaultConfig() Config {
	return Config{
		BranchPrefix:          "tj/",
		BaseBranch:            "main",
		WorkerAgent:           "claude",
		CoordinatorAgent:      "claude",
		CoordinatorName:       toolName,
		TickSeconds:           15,
		GHSeconds:             60,
		IdleSeconds:           60,
		Nudge:                 true,
		PromptDebounceSeconds: 90,
		ReviewBumpHours:       4,
		VerifyMinutes:         60,
		StaleDays:             3,
		VerifyMax:             20,
		GHMinRemaining:        1000,
		Autonomy:              AutonomyConfig{Level: 4, Max: maxStanding, Widespread: 5, Budget: BudgetConfig{Steps: 30, Applies: 3, Dispatches: 5}},
		StallRetry:            StallRetryConfig{MaxRetries: defaultStallMaxRetries, BackoffSeconds: defaultStallBackoffSeconds},
		OrphanFinish: OrphanFinishConfig{
			IntervalSeconds: defaultOrphanFinishIntervalSeconds,
			GraceSeconds:    defaultOrphanFinishGraceSeconds,
			MaxPerPass:      defaultOrphanFinishMaxPerPass,
		},
		InboxRunner: InboxRunnerConfig{
			Enabled:         false,
			IdleSeconds:     defaultInboxRunnerIdleSeconds,
			CooldownSeconds: defaultInboxRunnerCooldownSeconds,
			TimeoutSeconds:  defaultInboxRunnerTimeoutSeconds,
			MaxAttempts:     defaultInboxRunnerMaxAttempts,
			Kinds:           slices.Clone(defaultInboxRunnerKinds),
		},
	}
}

// toolName names the binary, the config and state dirs, the ticker and the
// coordinator. The tool was called flatcircle, kelpie before that, and
// shepherd before that; those names, newest first, are deprecated aliases and
// the places old config and state are migrated from.
const toolName = "quartermaster"

// shortName is toolName's short alias (~/.local/bin/qm, bin/qm): the same
// binary under another name, not a deprecated one.
const shortName = "qm"

var legacyNames = []string{"flatcircle", "kelpie", "shepherd"}

func isLegacyName(name string) bool { return slices.Contains(legacyNames, name) }

// envNames is the variable named after this tool and after each old name, so
// QUARTERMASTER_STATE_DIR, FLATCIRCLE_STATE_DIR, KELPIE_STATE_DIR and
// SHEPHERD_STATE_DIR all work.
func envNames(suffix string) []string {
	out := []string{strings.ToUpper(toolName) + "_" + suffix}
	for _, n := range legacyNames {
		out = append(out, strings.ToUpper(n)+"_"+suffix)
	}
	return out
}

// configBase is $XDG_CONFIG_HOME, else ~/.config, on every platform. herdr keeps
// its config and socket there on macOS too, where os.UserConfigDir would say
// ~/Library/Application Support.
func configBase() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

func stateBase() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

func configDir() string {
	if d := envDir(envNames("CONFIG_DIR")...); d != "" {
		return d
	}
	return pickDir(configBase())
}

func stateDir() string {
	if d := envDir(envNames("STATE_DIR")...); d != "" {
		return d
	}
	return pickDir(stateBase())
}

func envDir(keys ...string) string {
	for _, k := range keys {
		if d := os.Getenv(k); d != "" {
			return d
		}
	}
	return ""
}

// pickDir is base/quartermaster, or the newest legacy dir under base while only
// that one exists (a migration that could not move it).
func pickDir(base string) string {
	dir := filepath.Join(base, toolName)
	if _, err := os.Stat(dir); err != nil {
		for _, n := range legacyNames {
			if fi, err := os.Stat(filepath.Join(base, n)); err == nil && fi.IsDir() {
				return filepath.Join(base, n)
			}
		}
	}
	return dir
}

// migrateDirs moves flatcircle's, kelpie's or shepherd's config and state to
// quartermaster's paths. Directories set through the environment are left
// where they are. Links left by earlier migrations (kelpie -> flatcircle,
// shepherd -> kelpie) keep working through the flatcircle link this one
// leaves.
func migrateDirs() []string {
	var out []string
	for _, d := range []struct{ env, base string }{{"CONFIG_DIR", configBase()}, {"STATE_DIR", stateBase()}} {
		if envDir(envNames(d.env)...) != "" {
			continue
		}
		// On macOS, config used to live under os.UserConfigDir
		// (~/Library/Application Support) before configBase.
		if old, err := os.UserConfigDir(); err == nil && d.env == "CONFIG_DIR" && old != d.base {
			for _, n := range append([]string{toolName}, legacyNames...) {
				out = append(out, migrateDir(filepath.Join(old, n), filepath.Join(d.base, toolName))...)
			}
		}
		for _, n := range legacyNames {
			out = append(out, migrateDir(filepath.Join(d.base, n), filepath.Join(d.base, toolName))...)
		}
	}
	return out
}

// migrateDir renames legacy to dir and leaves a symlink at legacy, so a
// process still using the old path (a running older ticker, an open
// coordinator) keeps reading and writing the same files. A rename keeps every
// file, open ones included; it is idempotent, and does nothing once dir exists
// or when legacy is missing or already a link.
func migrateDir(legacy, dir string) []string {
	fi, err := os.Lstat(legacy)
	if err != nil || !fi.IsDir() {
		return nil
	}
	if _, err := os.Lstat(dir); err == nil {
		return []string{fmt.Sprintf("both %s and %s exist; using %s, left %s alone", dir, legacy, dir, legacy)}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return []string{fmt.Sprintf("could not create %s (%v); still using %s", filepath.Dir(dir), err, legacy)}
	}
	if err := os.Rename(legacy, dir); err != nil {
		return []string{fmt.Sprintf("could not move %s to %s (%v); still using %s", legacy, dir, err, legacy)}
	}
	if err := os.Symlink(dir, legacy); err != nil {
		return []string{fmt.Sprintf("moved %s to %s; no link left behind: %v", legacy, dir, err)}
	}
	return []string{fmt.Sprintf("moved %s to %s (linked from the old path)", legacy, dir)}
}

func loadConfig() (Config, error) {
	cfg := defaultConfig()
	md, err := toml.DecodeFile(filepath.Join(configDir(), "config.toml"), &cfg)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return cfg, err
	}
	cfg.Autonomy.migrate(md.IsDefined("autonomy", "level"))
	if err := cfg.Autonomy.validate(); err != nil {
		return cfg, err
	}
	cfg.Repo, cfg.BeadsDir = expandHome(cfg.Repo), expandHome(cfg.BeadsDir)
	cfg.HerdrSocket = expandHome(cfg.HerdrSocket)
	for name, path := range cfg.Repos {
		cfg.Repos[name] = expandHome(path)
	}
	if err := cfg.validateHosts(); err != nil {
		return cfg, err
	}
	if err := cfg.validateAgents(); err != nil {
		return cfg, err
	}
	if err := cfg.validateTap(); err != nil {
		return cfg, err
	}
	if err := cfg.validateDesktop(); err != nil {
		return cfg, err
	}
	if cfg.Watches.MaxConcurrent < 0 {
		return cfg, fmt.Errorf("[watches] max_concurrent %d is negative", cfg.Watches.MaxConcurrent)
	}
	if cfg.PromptDebounceSeconds < 0 {
		return cfg, fmt.Errorf("prompt_debounce_seconds %d is negative; use 0 to disable the prompt debounce", cfg.PromptDebounceSeconds)
	}
	if cfg.ReviewBumpHours < 0 {
		return cfg, fmt.Errorf("review_bump_hours %d is negative; use 0 to disable review bump reminders", cfg.ReviewBumpHours)
	}
	if cfg.NeedsMeParkedDays != nil && *cfg.NeedsMeParkedDays < 0 {
		return cfg, fmt.Errorf("needs_me_parked_days %d is negative; use 0 to disable age-based parking", *cfg.NeedsMeParkedDays)
	}
	if err := cfg.validateStallRetry(); err != nil {
		return cfg, err
	}
	if err := cfg.validateOrphanFinish(); err != nil {
		return cfg, err
	}
	if err := cfg.validateInboxRunner(); err != nil {
		return cfg, err
	}
	if cfg.Repo == "" {
		return cfg, errNoRepo
	}
	if cfg.BeadsDir == "" {
		cfg.BeadsDir = filepath.Join(cfg.Repo, ".beads")
	}
	// Keybindings and plugin actions run in the herdr server's environment,
	// which has no BEADS_DIR; without this bd silently opens another database.
	if os.Getenv("BEADS_DIR") == "" {
		os.Setenv("BEADS_DIR", cfg.BeadsDir)
	}
	return cfg, nil
}

var errNoRepo = errors.New("set repo in " + filepath.Join(configDir(), "config.toml") + ", or run `quartermaster configure --repo <path>`")

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}

const repoLabel = "repo:"

// repoName is the name a bead's repo: label selects, "" for the default repo,
// and whether that name is configured.
func (c Config) repoName(b Bead) (string, bool) {
	for _, l := range b.Labels {
		if name, ok := strings.CutPrefix(l, repoLabel); ok {
			_, known := c.Repos[name]
			return name, known
		}
	}
	return "", true
}

// repoFor is the repository a bead's worktree, branch and PRs live in. Every
// per-bead repo lookup goes through here so multi-repo support has one seam.
func (c Config) repoFor(b Bead) string {
	if name, ok := c.repoName(b); ok && name != "" {
		return c.Repos[name]
	}
	return c.Repo
}

// allRepos is every repository quartermaster follows, for passes that are not about
// one bead (PR listing, worktree sweeps).
func (c Config) allRepos() []string {
	repos := []string{filepath.Clean(c.Repo)}
	for _, name := range slices.Sorted(maps.Keys(c.Repos)) {
		if p := filepath.Clean(c.Repos[name]); !slices.Contains(repos, p) {
			repos = append(repos, p)
		}
	}
	return repos
}

// coordSocket is the socket of the herdr server the ticker follows: herdr_socket,
// or herdr's default server. Never the caller's HERDR_SOCKET_PATH: every server
// that loads the plugin runs startup, and a ticker on a server without the
// coordinator sees no agents.
func (c Config) coordSocket() string {
	if c.HerdrSocket != "" {
		return c.HerdrSocket
	}
	return defaultHerdrSocket()
}

func (c Config) tick() time.Duration { return time.Duration(max(c.TickSeconds, 5)) * time.Second }
func (c Config) ghEvery() time.Duration {
	return time.Duration(max(c.GHSeconds, 30)) * time.Second
}
func (c Config) verifyEvery() time.Duration {
	return time.Duration(max(c.VerifyMinutes, 0)) * time.Minute
}
func (c Config) staleAge() time.Duration { return time.Duration(max(c.StaleDays, 0)) * 24 * time.Hour }

// needsMeParkedAfter is how long an agentless, non-critical needs_me bead may
// go untouched before views classify it as Parked / Dormant. Unset defaults to
// defaultNeedsMeParkedDays (7d); 0 disables age-based parking.
func (c Config) needsMeParkedAfter() time.Duration {
	if c.NeedsMeParkedDays == nil {
		return defaultNeedsMeParkedDays * 24 * time.Hour
	}
	if *c.NeedsMeParkedDays <= 0 {
		return 0
	}
	return time.Duration(*c.NeedsMeParkedDays) * 24 * time.Hour
}
func (c Config) idle() time.Duration { return time.Duration(c.IdleSeconds) * time.Second }

// promptDebounce is the window a worker prompt must keep showing before it
// surfaces; 0 (or a zero Config that never went through defaultConfig)
// disables it.
func (c Config) promptDebounce() time.Duration {
	if c.PromptDebounceSeconds <= 0 {
		return 0
	}
	return time.Duration(c.PromptDebounceSeconds) * time.Second
}

// promptFlap is how long a prompt may go undetected (agent briefly not
// blocked, or its screen unreadable) without losing its debounce clock or
// its already-surfaced dedup: two ticks (30s at the default tick). A longer
// absence is a cleared prompt; the same prompt showing again after it is a
// new instance with a fresh window.
func (c Config) promptFlap() time.Duration { return 2 * c.tick() }

// reviewBumpAfter is how long a ready_for_review PR head may stay green and
// REVIEW_REQUIRED without a new human review before emitting one review_bump
// reminder; 0 (or a zero Config that never went through defaultConfig)
// disables it.
func (c Config) reviewBumpAfter() time.Duration {
	if c.ReviewBumpHours <= 0 {
		return 0
	}
	return time.Duration(c.ReviewBumpHours) * time.Hour
}

func instructionsPath() string { return filepath.Join(configDir(), "instructions.md") }

func readInstructions() string {
	b, err := os.ReadFile(instructionsPath())
	if err != nil {
		return ""
	}
	return string(b)
}
