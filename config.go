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
	Repo             string `toml:"repo"`
	BeadsDir         string `toml:"beads_dir"`
	BranchPrefix     string `toml:"branch_prefix"`
	BaseBranch       string `toml:"base_branch"`
	WorkerAgent      string `toml:"worker_agent"`
	CoordinatorAgent string `toml:"coordinator_agent"`
	CoordinatorName  string `toml:"coordinator_name"`
	TickSeconds      int    `toml:"tick_seconds"`
	GHSeconds        int    `toml:"gh_seconds"`
	// A pane must have been idle this long before the ticker types into it:
	// on Herdr 0.9.1 a prompt merges with whatever the user has half-typed.
	IdleSeconds int  `toml:"idle_seconds"`
	Nudge       bool `toml:"nudge"`
	// Remove a bead's worktree once its PR merged, the bead is closed and its
	// agent is idle. Off by default: removal closes the workspace.
	AutoResolve bool `toml:"auto_resolve"`
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
	// AutoClose lets verify close a bead on strong evidence; off, it only
	// flags it to the coordinator.
	AutoClose bool `toml:"auto_close"`
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
}

type AgentConfig struct {
	// Args go to the agent's own CLI on every start, dispatch and resume.
	Args []string `toml:"args"`
	// Env is "NAME=value" pairs exported in the agent's pane before it
	// starts, after BEADS_ACTOR and BEADS_DIR, on every host.
	Env []string `toml:"env"`
}

// defaultAgentArgs apply to a kind with no [agents.<kind>] table. Muse's
// sandbox scrubs the environment (BEADS_DIR) and blocks gh's credentials, so
// a sandboxed Muse worker can't use bd or open a PR. Its :auto-review profile
// keeps the approval judge but lets the judge, not a human, decide, so an
// unattended worker doesn't wait on a prompt nobody is watching.
var defaultAgentArgs = map[string][]string{
	"muse": {"--disable-sandbox", "--permission-profile", ":auto-review"},
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
// default), then extra, such as a resume subcommand. Configured args go first
// because codex takes its root options before a subcommand.
func (c Config) agentArgs(kind string, extra ...string) []string {
	args := defaultAgentArgs[kind]
	if a, ok := c.Agents[kind]; ok {
		args = a.Args
	}
	return append(slices.Clone(args), extra...)
}

func defaultConfig() Config {
	return Config{
		BranchPrefix:     "tj/",
		BaseBranch:       "main",
		WorkerAgent:      "claude",
		CoordinatorAgent: "claude",
		CoordinatorName:  toolName,
		TickSeconds:      15,
		GHSeconds:        60,
		IdleSeconds:      60,
		Nudge:            true,
		VerifyMinutes:    60,
		StaleDays:        3,
		VerifyMax:        20,
		GHMinRemaining:   1000,
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
	_, err := toml.DecodeFile(filepath.Join(configDir(), "config.toml"), &cfg)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
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
func (c Config) idle() time.Duration     { return time.Duration(c.IdleSeconds) * time.Second }

func instructionsPath() string { return filepath.Join(configDir(), "instructions.md") }

func readInstructions() string {
	b, err := os.ReadFile(instructionsPath())
	if err != nil {
		return ""
	}
	return string(b)
}
