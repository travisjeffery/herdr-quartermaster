package main

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// localHost is the machine the coordinator runs on. It is always in the
// registry; [hosts.local] only overrides its defaults.
const localHost = "local"

// HostConfig is one [hosts.<name>] table: a machine workers can run on.
type HostConfig struct {
	// Machine is the herdr machine label (herdr machine add --label <it>).
	// Empty on local.
	Machine string `toml:"machine"`
	// SSH is the target for work outside herdr (hostExec, probes, bootstrap).
	SSH    string `toml:"ssh"`
	Cores  int    `toml:"cores"`
	RAMGiB int    `toml:"ram_gib"`
	// Slots caps concurrent workers; 0 derives it from cores and RAM.
	Slots           int      `toml:"slots"`
	CoresPerWorker  int      `toml:"cores_per_worker"`
	RAMPerWorkerGiB int      `toml:"ram_per_worker_gib"`
	ReserveGiB      int      `toml:"reserve_gib"`
	Labels          []string `toml:"labels"`
	// Repos maps a repo name to its path on the host, ~ meaning the host
	// user's home. A repo missing here is never dispatched to the host.
	Repos     map[string]string `toml:"repos"`
	Worktrees string            `toml:"worktrees"`
	Image     string            `toml:"image"`
	// Runtime runs the worker container: podman or docker.
	Runtime string `toml:"runtime"`
	// Container is the worker container's name, qm-worker by default.
	Container string `toml:"container"`
	// Enabled defaults to true; false keeps the host in config but out of use.
	Enabled *bool `toml:"enabled"`
}

const (
	scheduleManual = "manual"
	scheduleAuto   = "auto"
	hostLabel      = "host:"
	needsLabel     = "needs:"
)

var hostNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// validateHosts checks the registry once at load, so a typo fails loudly
// instead of sending a worker nowhere.
func (c Config) validateHosts() error {
	switch c.Schedule {
	case "", scheduleManual:
	case scheduleAuto:
		return fmt.Errorf(`schedule = "auto" is not implemented yet; use "manual"`)
	default:
		return fmt.Errorf("schedule = %q: want %q or %q", c.Schedule, scheduleManual, scheduleAuto)
	}
	for _, name := range slices.Sorted(maps.Keys(c.Hosts)) {
		h := c.Hosts[name]
		if !hostNameRE.MatchString(name) {
			return fmt.Errorf("hosts.%s: a host name is lowercase letters, digits and dashes", name)
		}
		if name == localHost {
			if h.Machine != "" || h.SSH != "" {
				return fmt.Errorf("hosts.local: local takes no machine or ssh")
			}
			continue
		}
		if h.Machine == "" || h.SSH == "" {
			return fmt.Errorf("hosts.%s: set machine (the herdr machine label) and ssh", name)
		}
		switch h.Runtime {
		case "", "podman", "docker":
		default:
			return fmt.Errorf("hosts.%s: runtime = %q: want podman or docker", name, h.Runtime)
		}
		for repo, path := range h.Repos {
			if !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "~/") {
				return fmt.Errorf("hosts.%s: repos.%s = %q: herdr needs an absolute or ~/ path on the host", name, repo, path)
			}
		}
	}
	return nil
}

// host is the named host's config, with local always present.
func (c Config) host(name string) (HostConfig, bool) {
	h, ok := c.Hosts[name]
	if name == localHost {
		return h, true
	}
	return h, ok
}

// hostNames is every registered host, local first.
func (c Config) hostNames() []string {
	names := []string{localHost}
	for _, n := range slices.Sorted(maps.Keys(c.Hosts)) {
		if n != localHost {
			names = append(names, n)
		}
	}
	return names
}

func (h HostConfig) enabled() bool { return h.Enabled == nil || *h.Enabled }

func (h HostConfig) container() string {
	if h.Container != "" {
		return h.Container
	}
	return "qm-worker"
}

func (h HostConfig) runtime() string {
	if h.Runtime != "" {
		return h.Runtime
	}
	return "podman"
}

// slots is the cap on concurrent workers: the configured value, else
// min(cores / cores_per_worker, (RAM - reserve) / ram_per_worker), never
// below one. Unset knobs fall back to the global ones, then to 4 cores and
// 4 GiB a worker with 3 GiB reserved (8 on local, which is also a desktop).
// Cores and RAM default to this machine's on local.
func (c Config) slots(name string) int {
	h, _ := c.host(name)
	if h.Slots > 0 {
		return h.Slots
	}
	pick := func(vals ...int) int {
		for _, v := range vals {
			if v > 0 {
				return v
			}
		}
		return 0
	}
	reserve := 3
	cores, ram := h.Cores, h.RAMGiB
	if name == localHost {
		reserve = 8
		cores = pick(cores, runtime.NumCPU())
		ram = pick(ram, memTotalGiB())
	}
	cpw := pick(h.CoresPerWorker, c.CoresPerWorker, 4)
	rpw := pick(h.RAMPerWorkerGiB, c.RAMPerWorkerGiB, 4)
	reserve = pick(h.ReserveGiB, c.ReserveGiB, reserve)
	if cores == 0 || ram == 0 {
		return 1
	}
	return max(1, min(cores/cpw, (ram-reserve)/rpw))
}

// memTotalGiB is this machine's RAM, 0 where /proc/meminfo is missing.
func memTotalGiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if rest, ok := strings.CutPrefix(s.Text(), "MemTotal:"); ok {
			kb, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
			return kb / (1024 * 1024)
		}
	}
	return 0
}

// herdrOn is h aimed at the named host's herdr server: local's own, or a
// remote one through herdr's machine forwarding. Agents it lists carry the
// host's name.
func (c Config) herdrOn(h Herdr, name string) Herdr {
	hc, _ := c.host(name)
	h = h.onMachine(hc.Machine)
	h.host = name
	return h
}

// repoKey names a bead's repo in a host's repos table: its repo: label, or
// the default repo's directory name.
func (c Config) repoKey(b Bead) string {
	if name, ok := c.repoName(b); ok && name != "" {
		return name
	}
	return filepath.Base(filepath.Clean(c.Repo))
}

// repoOn is the bead's repo path on a host. Local uses the coordinator's
// paths; a remote host must list the repo.
func (c Config) repoOn(name string, b Bead) (string, bool) {
	if name == localHost {
		return c.repoFor(b), true
	}
	hc, _ := c.host(name)
	p, ok := hc.Repos[c.repoKey(b)]
	return p, ok
}

// beadHost is the host a bead's host: label names, "" when it has none.
func beadHost(b Bead) string {
	for _, l := range b.Labels {
		if h, ok := strings.CutPrefix(l, hostLabel); ok {
			return h
		}
	}
	return ""
}

// requiredLabels is every host label a bead needs: its needs:<label>
// labels and its repo's repo_needs.
func (c Config) requiredLabels(b Bead) []string {
	var out []string
	for _, l := range b.Labels {
		if n, ok := strings.CutPrefix(l, needsLabel); ok && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, n := range c.RepoNeeds[c.repoKey(b)] {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// hostFor picks the host a dispatch runs on and says why another would not
// do. Scheduling is manual: --host, else the bead's sticky host: label, else
// local. Constraints (enabled, repo present, labels) apply to whichever host
// is picked; force skips the label check only.
func (c Config) hostFor(b Bead, explicit string, force bool) (string, error) {
	name := explicit
	if name == "" {
		name = beadHost(b)
	}
	if name == "" {
		name = localHost
	}
	hc, ok := c.host(name)
	if !ok {
		return "", fmt.Errorf("no host %q (hosts: %s)", name, strings.Join(c.hostNames(), ", "))
	}
	if !hc.enabled() {
		return "", fmt.Errorf("%s: disabled", name)
	}
	if _, ok := c.repoOn(name, b); !ok {
		return "", fmt.Errorf("%s: no repo %s (add it to hosts.%s.repos)", name, c.repoKey(b), name)
	}
	// local carries every label unless its table lists them.
	if !force && (name != localHost || hc.Labels != nil) {
		for _, need := range c.requiredLabels(b) {
			if !slices.Contains(hc.Labels, need) {
				return "", fmt.Errorf("%s: lacks label %s, which %s needs (--force to dispatch anyway)", name, need, b.ID)
			}
		}
	}
	return name, nil
}

// setHostLabel records on the bead where its worker runs, dropping any other
// host: label. A bead going to local without one gets none.
func setHostLabel(bd Beads, b Bead, name string) error {
	had := false
	for _, l := range b.Labels {
		h, ok := strings.CutPrefix(l, hostLabel)
		if !ok {
			continue
		}
		had = true
		if h != name {
			if err := bd.SetLabel(b.ID, l, false); err != nil {
				return err
			}
		}
	}
	if (name == localHost && !had) || b.HasLabel(hostLabel+name) {
		return nil
	}
	return bd.SetLabel(b.ID, hostLabel+name, true)
}

// hostExec is a command run on a host: directly on local, else through ssh
// into the host's worker container, where repos and worktrees live. Over
// ssh every argument is quoted for the host's shell, except that a leading
// ~/ is left for it to expand: the container mounts the same $HOME.
func (c Config) hostExec(name string, args ...string) *exec.Cmd {
	if name == localHost {
		return exec.Command(args[0], args[1:]...)
	}
	hc, _ := c.host(name)
	remote := []string{hc.runtime(), "exec", "-i", shellQuote(hc.container())}
	for _, a := range args {
		remote = append(remote, quoteHome(a))
	}
	return exec.Command("ssh", "-o", "BatchMode=yes", hc.SSH, strings.Join(remote, " "))
}

func quoteHome(s string) string {
	if rest, ok := strings.CutPrefix(s, "~/"); ok {
		return "~/" + shellQuote(rest)
	}
	return shellQuote(s)
}

// hostList is `quartermaster host list`: the registry as dispatch sees it.
func hostList(c Config) string {
	var s strings.Builder
	for _, name := range c.hostNames() {
		hc, _ := c.host(name)
		machine := hc.Machine
		if machine == "" {
			machine = "-"
		}
		state := "enabled"
		if !hc.enabled() {
			state = "disabled"
		}
		labels := "all"
		if name != localHost || hc.Labels != nil {
			labels = strings.Join(hc.Labels, ",")
		}
		repos := "all"
		if name != localHost {
			repos = strings.Join(slices.Sorted(maps.Keys(hc.Repos)), ",")
		}
		fmt.Fprintf(&s, "%-10s machine=%s slots=%d %s labels=%s repos=%s\n", name, machine, c.slots(name), state, labels, repos)
	}
	return s.String()
}
