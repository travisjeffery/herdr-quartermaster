package main

// Adoption is deliberately pi-to-pi and local. It never kills a process,
// closes a pane, claims a bead, guesses a session, or sends a model prompt.
// Prepare reserves before an operator stops the old worker. Adopt proves the
// saved owner is gone and the checkout unchanged before starting parked RPC.
import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type rpcAdoption struct {
	Source      Agent           `json:"source"`
	Bead        Bead            `json:"bead_snapshot"`
	PIDs        []int           `json:"old_pids"`
	Owners      []adoptionOwner `json:"old_process_identities"`
	ShellPID    int             `json:"shell_pid"`
	Shell       adoptionOwner   `json:"shell_identity"`
	Digest      string          `json:"checkout_digest"`
	GitStatus   string          `json:"git_status"`
	GuardEnv    string          `json:"guard_environment_hash"`
	SessionID   string          `json:"session_id"`
	SessionFile string          `json:"session_file"`
	SessionHash string          `json:"session_prefix_hash"`
	SessionSize int64           `json:"session_prefix_bytes"`
	Prepared    time.Time       `json:"prepared"`
	Ready       bool            `json:"ready"`     // intent to attempt the handoff
	Completed   bool            `json:"completed"` // exact session verified under the supervisor lease
}

// Shared by every managed legacy delivery; prepare takes it exclusively.
// Nonblocking acquisition refuses contention rather than enqueueing a prompt
// that might land after the old worker has been checkpointed.
func adoptionAdmission(exclusive bool) (*os.File, error) {
	if err := rpcPrivateDir(rpcRoot()); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(rpcRoot(), "admission"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("migration/delivery admission busy; retry after the current operation settles")
	}
	return f, nil
}

func rpcParkPath(id string) string { return filepath.Join(rpcDir(id), "parked") }
func rpcParked(id string) bool {
	_, err := os.Stat(rpcParkPath(id))
	return !errors.Is(err, os.ErrNotExist) // unreadable is not permission to run
}

// Raw Herdr listing is required here: native fallback on a failed Herdr call
// must not be interpreted as proof that the old TUI owner disappeared.
func adoptionAgents(h Herdr) ([]Agent, error) {
	res, err := h.call(callTimeout, "agent", "list")
	if err != nil {
		return nil, err
	}
	var out struct {
		Agents []Agent `json:"agents"`
	}
	err = json.Unmarshal(res, &out)
	return out.Agents, err
}

type adoptionProcess struct {
	PID  int      `json:"pid"`
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
}
type adoptionProcessInfo struct {
	ShellPID int               `json:"shell_pid"`
	Procs    []adoptionProcess `json:"foreground_processes"`
}

func adoptionProcesses(h Herdr, pane string) (adoptionProcessInfo, error) {
	res, err := h.call(callTimeout, "pane", "process-info", "--pane", pane)
	var out struct {
		Info adoptionProcessInfo `json:"process_info"`
	}
	if err == nil {
		err = json.Unmarshal(res, &out)
	}
	return out.Info, err
}

func adoptionGit(cwd string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	// Status must not refresh the index while taking a checkpoint.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w", args, err)
	}
	return out, nil
}

// Include both index and working changes (even when they cancel each other),
// plus untracked bytes, names and modes. No checkout/reset/stash is performed.
func adoptionDigest(cwd string) (string, string, error) {
	branch, err := adoptionGit(cwd, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", "", err
	}
	index, err := adoptionGit(cwd, "ls-files", "--stage", "-z")
	if err != nil {
		return "", "", err
	}
	for _, entry := range strings.Split(string(index), "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return "", "", errors.New("submodule checkouts are unsupported for adoption; preserve worker")
		}
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "%d:", len(index))
	hash.Write(index)
	for _, args := range [][]string{{"rev-parse", "HEAD"}, {"status", "--porcelain=v1", "-z", "--untracked-files=all"}, {"diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD"}, {"diff", "--no-ext-diff", "--no-textconv", "--binary", "--cached"}} {
		b, err := adoptionGit(cwd, args...)
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(hash, "%d:", len(b))
		hash.Write(b)
	}
	names, err := adoptionGit(cwd, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", "", err
	}
	for _, name := range strings.Split(string(names), "\x00") {
		if name == "" {
			continue
		}
		p := filepath.Join(cwd, name)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(hash, "%d:%s:missing:", len(name), name)
			continue
		}
		if err != nil {
			return "", "", err
		}
		fmt.Fprintf(hash, "%d:%s:%s:%d:", len(name), name, fi.Mode(), fi.Size())
		switch {
		case fi.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return "", "", err
			}
			_, err = io.Copy(hash, f)
			f.Close()
			if err != nil {
				return "", "", err
			}
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return "", "", err
			}
			hash.Write([]byte(target))
		default:
			return "", "", fmt.Errorf("unsupported checkout file %s", name)
		}
	}
	return strings.TrimSpace(string(branch)), fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func adoptionSession(path, cwd string) (string, int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()
	var header struct {
		Type    string
		Version int
		ID      string
		Cwd     string
	}
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return "", 0, "", err
	}
	if err = json.Unmarshal(line, &header); err != nil {
		return "", 0, "", err
	}
	if header.Type != "session" || header.Version != 3 || header.ID == "" || header.Cwd != cwd {
		return "", 0, "", errors.New("requires a v3 pi session with the exact worktree cwd")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return "", 0, "", err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	return header.ID, n, fmt.Sprintf("%x", hash.Sum(nil)), err
}
func adoptionPrefix(a *rpcAdoption) error {
	f, err := os.Open(a.SessionFile)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.CopyN(hash, f, a.SessionSize)
	if err != nil || n != a.SessionSize || fmt.Sprintf("%x", hash.Sum(nil)) != a.SessionHash {
		return errors.New("saved session history changed or was truncated")
	}
	return nil
}

// Linux /proc is required for this first bounded adoption path. Capture all
// descendants, including in-flight shell/tool processes, before stopping.
// Do not silently drop an environment-selected agent directory or hook set.
// Hash only configuration selectors, never persist environment credentials.
func adoptionGuardEnv(env []string) string {
	keys := []string{"HOME", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "QUARTERMASTER_PI_BASH_HOOKS"}
	values := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		values[k] = v
	}
	hash := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(hash, "%s=%s\x00", k, values[k])
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

type adoptionOwner struct {
	PID          int    `json:"pid"`
	Start        string `json:"start_ticks"`
	Boot         string `json:"boot_id"`
	Session      int    `json:"process_session"`
	SessionStart string `json:"session_leader_start_ticks,omitempty"`
}

func adoptionIdentity(root string, pid int) (adoptionOwner, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return adoptionOwner{}, err
	}
	end := strings.LastIndex(string(b), ") ")
	if end < 0 {
		return adoptionOwner{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(b)[end+2:])
	if len(fields) < 20 {
		return adoptionOwner{}, errors.New("short process stat")
	}
	if fields[0] == "Z" {
		return adoptionOwner{}, os.ErrNotExist
	}
	sid, err := strconv.Atoi(fields[3])
	if err != nil {
		return adoptionOwner{}, err
	}
	boot, err := os.ReadFile(filepath.Join(root, "sys/kernel/random/boot_id"))
	if err != nil {
		return adoptionOwner{}, err
	}
	return adoptionOwner{PID: pid, Start: fields[19], Boot: strings.TrimSpace(string(boot)), Session: sid}, nil
}

// A descendant may orphan after the checkpoint. Its Linux session and/or
// worktree cwd remain evidence even after its recorded parent has exited.
// This is not containment against processes deliberately detaching from both;
// such unmanaged background jobs are unsupported and must be left intact.
func adoptionRemaining(root string, w rpcWorker, ignore map[int]bool) error {
	sessions := map[int]bool{}
	oldest := uint64(^uint64(0))
	for _, old := range w.Adoption.Owners {
		start, err := strconv.ParseUint(old.Start, 10, 64)
		if err != nil {
			return err
		}
		if start < oldest {
			oldest = start
		}
		live, err := adoptionIdentity(root, old.PID)
		if err == nil && live.Start == old.Start && live.Boot == old.Boot && !ignore[old.PID] {
			return fmt.Errorf("old process %d still alive", old.PID)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		boot, err := os.ReadFile(filepath.Join(root, "sys/kernel/random/boot_id"))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(boot)) == old.Boot {
			leader, e := adoptionIdentity(root, old.Session)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			if errors.Is(e, os.ErrNotExist) || old.SessionStart == "" || leader.Start == old.SessionStart {
				sessions[old.Session] = true
			}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || ignore[pid] {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		stat, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Getuid() {
			continue
		}
		live, err := adoptionIdentity(root, pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		// A process predating this worker cannot be an escaped descendant.
		// Do not inspect unrelated long-lived/nondumpable user services.
		start, err := strconv.ParseUint(live.Start, 10, 64)
		if err != nil {
			return err
		}
		if len(w.Adoption.Owners) > 0 && live.Boot == w.Adoption.Owners[0].Boot && start < oldest && !sessions[live.Session] {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(root, e.Name(), "cwd"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if sessions[live.Session] || cwd == w.Cwd || strings.HasPrefix(cwd, w.Cwd+string(os.PathSeparator)) {
			return fmt.Errorf("process %d remains in old process session/worktree; no adoption", pid)
		}
	}
	return nil
}
func adoptionCallerAncestors() map[int]bool {
	out := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !out[pid]; {
		out[pid] = true
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		end := strings.LastIndex(string(b), ") ")
		if end < 0 {
			break
		}
		fields := strings.Fields(string(b)[end+2:])
		if len(fields) < 2 {
			break
		}
		pid, _ = strconv.Atoi(fields[1])
	}
	return out
}

func adoptionDescendants(pid int) ([]int, error) {
	return adoptionDescendantsIn("/proc", pid)
}
func adoptionDescendantsIn(root string, pid int) ([]int, error) {
	seen := map[int]bool{}
	var visit func(int) error
	visit = func(pid int) error {
		if seen[pid] {
			return nil
		}
		seen[pid] = true
		if len(seen) > 4096 {
			return errors.New("process tree too large for bounded adoption")
		}
		tasks, err := os.ReadDir(filepath.Join(root, strconv.Itoa(pid), "task"))
		if err != nil {
			return err
		}
		for _, task := range tasks {
			b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "task", task.Name(), "children"))
			if err != nil {
				return err
			}
			for _, word := range strings.Fields(string(b)) {
				child, err := strconv.Atoi(word)
				if err != nil {
					return err
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(pid); err != nil {
		return nil, err
	}
	out := make([]int, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out, nil
}

func rpcPrepare(cfg Config, h Herdr, b Bead, pane string) error {
	if beadHost(b) != "" && beadHost(b) != localHost {
		return errors.New("adoption is local only")
	}
	if b.Status != StatusInProgress && b.Status != StatusNeedsMe && b.Status != StatusBlocked {
		return errors.New("only existing active workers may be adopted; never backlog or closed beads")
	}
	if _, ok := cfg.repoName(b); !ok {
		return errors.New("unknown repository label")
	}
	if !filepath.IsAbs(stateDir()) || len(rpcSocket(b.ID)) > 100 {
		return errors.New("requires a short absolute state directory")
	}
	lease, err := rpcLease(b.ID)
	if err != nil {
		return err
	}
	defer lease.Close()
	admission, err := adoptionAdmission(true)
	if err != nil {
		return err
	}
	defer admission.Close()
	if rpcExists(b.ID) {
		return errors.New("reservation already exists; inspect it, do not overwrite")
	}
	agents, err := adoptionAgents(h)
	if err != nil {
		return err
	}
	var source *Agent
	for i := range agents {
		a := &agents[i]
		if a.Name == agentName(b.ID) {
			if source != nil {
				return errors.New("multiple source agents")
			}
			source = a
		}
	}
	if source == nil || source.PaneID != pane || source.Name == cfg.CoordinatorName || source.PaneID == os.Getenv("HERDR_PANE_ID") {
		return errors.New("requires the exact existing worker pane, not the caller or coordinator")
	}
	if source.Kind != "pi" || source.Session.Agent != "pi" || source.Session.Kind != "path" || !agentReady(source) {
		return errors.New("only idle/done pi workers with an exact session path can be adopted; other harness/guard states must be preserved")
	}
	cwd, err := filepath.EvalSymlinks(source.Cwd)
	if err != nil {
		return err
	}
	wts, err := h.Worktrees(cfg.repoFor(b))
	if err != nil {
		return err
	}
	found := false
	for _, wt := range wts {
		if wt.Linked && wt.Path == cwd {
			found = true
		}
	}
	if !found {
		return errors.New("source is not a linked worktree of the bead repository")
	}
	branch, digest, err := adoptionDigest(cwd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(branch, cfg.BranchPrefix+b.ID+"-") {
		return errors.New("branch does not belong to the bead")
	}
	info, err := adoptionProcesses(h, pane)
	if err != nil {
		return err
	}
	if len(info.Procs) != 1 || info.Procs[0].PID <= 0 || info.Procs[0].Cwd != cwd {
		return errors.New("source foreground ownership is ambiguous")
	}
	if len(cfg.Agents["pi"].Env) != 0 {
		return errors.New("adoption of custom agent environments needs explicit review")
	}
	sourceEnv, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", info.Procs[0].PID))
	if err != nil {
		return err
	}
	guardEnv := adoptionGuardEnv(strings.Split(string(sourceEnv), "\x00"))
	if guardEnv != adoptionGuardEnv(os.Environ()) {
		return errors.New("source guard environment differs; preserve worker")
	}
	args, err := rpcLaunchArgs(cfg, b.ID)
	if err != nil {
		return err
	}
	// Exact argv equality apart from transport/session prevents dropping tools,
	// trust, or extension restrictions. Do not translate another harness's flags.
	expected := append(append([]string{"pi"}, args[:len(args)-2]...), piSessionArgs(b.ID)...)
	if !reflect.DeepEqual(info.Procs[0].Argv, expected) {
		return errors.New("source pi argv differs from configured guarded launch; preserve this worker for explicit review")
	}
	c, ok := piControlFor(source.Name)
	if !ok {
		return errors.New("source guard socket unavailable")
	}
	ping, err := c.call(piRequest{Op: "ping"})
	if err != nil || !ping.Idle {
		return errors.New("source guard cannot confirm idle")
	}
	ask, err := c.Ask()
	if err != nil || ask != nil {
		return errors.New("source has an unresolved or unreadable approval")
	}
	pids, err := adoptionDescendants(info.Procs[0].PID)
	if err != nil {
		return err
	}
	sid, size, hash, err := adoptionSession(source.Session.Value, cwd)
	if err != nil {
		return err
	}
	if sid != b.ID {
		return errors.New("session id does not match bead")
	}
	gitStatus, err := adoptionGit(cwd, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	again, err := adoptionDescendants(info.Procs[0].PID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(pids, again) {
		return errors.New("source descendants changed during checkpoint; wait for quiescence")
	}
	ping, err = c.call(piRequest{Op: "ping"})
	if err != nil || !ping.Idle {
		return errors.New("source became busy during checkpoint")
	}
	owners := make([]adoptionOwner, 0, len(pids))
	for _, pid := range pids {
		identity, err := adoptionIdentity("/proc", pid)
		if err != nil {
			return err
		}
		leader, e := adoptionIdentity("/proc", identity.Session)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		identity.SessionStart = leader.Start
		owners = append(owners, identity)
	}
	shell, err := adoptionIdentity("/proc", info.ShellPID)
	if err != nil {
		return err
	}
	a := &rpcAdoption{Shell: shell, Owners: owners, GitStatus: string(gitStatus), GuardEnv: guardEnv, Source: *source, Bead: b, PIDs: pids, ShellPID: info.ShellPID, Digest: digest, SessionID: sid, SessionFile: source.Session.Value, SessionHash: hash, SessionSize: size, Prepared: time.Now()}
	ignore := adoptionCallerAncestors()
	ignore[info.ShellPID] = true
	for _, pid := range pids {
		ignore[pid] = true
	}
	if err := adoptionRemaining("/proc", rpcWorker{Cwd: cwd, Adoption: a}, ignore); err != nil {
		return err
	}
	// The parked marker precedes the atomic reservation. A partial prepare is
	// conservative and retryable; nothing can launch without worker.json.
	if err := rpcSave(rpcParkPath(b.ID), map[string]string{"reason": "transport migration; no task continuation authorized"}); err != nil {
		return err
	}
	return rpcSave(filepath.Join(rpcDir(b.ID), "worker.json"), rpcWorker{Bead: b.ID, Cwd: cwd, Branch: branch, Args: args, Adoption: a})
}

func rpcAdopt(cfg Config, h Herdr, b Bead) error {
	lease, err := rpcLease(b.ID)
	if err != nil {
		return err
	}
	defer lease.Close()
	var w rpcWorker
	if err = rpcLoad(filepath.Join(rpcDir(b.ID), "worker.json"), &w); err != nil {
		return err
	}
	if w.Adoption == nil || w.Adoption.Ready {
		return errors.New("requires a prepared adoption; use rpc resume after adoption")
	}
	if err = adoptionValidate(h, b, w); err != nil {
		return err
	}
	w.Adoption.Ready = true
	if err = rpcSave(filepath.Join(rpcDir(b.ID), "worker.json"), w); err != nil {
		return err
	}
	lease.Close()
	return rpcStart(b.ID)
}

func adoptionValidate(h Herdr, b Bead, w rpcWorker) error {
	if b.Status == StatusClosed || b.Status != w.Adoption.Bead.Status || b.Assignee != w.Adoption.Bead.Assignee {
		return errors.New("bead status/owner changed since prepare; leave parked and inspect")
	}
	if err := adoptionStopped(h, w); err != nil {
		return err
	}
	branch, digest, err := adoptionDigest(w.Cwd)
	if err != nil {
		return err
	}
	if branch != w.Branch || digest != w.Adoption.Digest {
		return errors.New("checkout changed since prepare; leave reservation and files intact")
	}
	if err = adoptionPrefix(w.Adoption); err != nil {
		return err
	}
	if !rpcParked(b.ID) {
		return errors.New("adoption parking barrier missing")
	}
	return nil
}

func adoptionStopped(h Herdr, w rpcWorker) error {
	if w.Adoption.Shell.PID != w.Adoption.ShellPID || w.Adoption.Shell.Start == "" {
		return errors.New("missing original shell identity")
	}
	shell, err := adoptionIdentity("/proc", w.Adoption.ShellPID)
	if err != nil {
		return err
	}
	if shell.Start != w.Adoption.Shell.Start || shell.Boot != w.Adoption.Shell.Boot {
		return errors.New("original shell identity changed; do not retire pane")
	}
	if len(w.Adoption.Owners) > 0 {
		ignore := adoptionCallerAncestors()
		ignore[w.Adoption.ShellPID] = true
		if err := adoptionRemaining("/proc", w, ignore); err != nil {
			return err
		}
	} else {
		for _, pid := range w.Adoption.PIDs {
			if processAlive(pid) {
				return fmt.Errorf("old owner/descendant PID %d still alive; no force stop", pid)
			}
		}
	}
	agents, err := adoptionAgents(h)
	if err != nil {
		return err
	}
	for _, a := range agents {
		// A restored name with no process is not a live worker, but an actual
		// agent sharing the checkout or bead is always a conflicting owner.
		if a.Kind != "" && (a.Name == agentName(w.Bead) || a.Cwd == w.Cwd || a.Session.Value == w.Adoption.SessionFile) {
			return fmt.Errorf("conflicting live agent in %s", a.PaneID)
		}
	}
	info, err := adoptionProcesses(h, w.Adoption.Source.PaneID)
	if err != nil {
		return err
	} // retain the old pane through verification
	if info.ShellPID != w.Adoption.ShellPID || len(info.Procs) != 1 || info.Procs[0].PID != info.ShellPID {
		return errors.New("old pane is not the original idle shell; do not close it")
	}
	return nil
}

// Legacy deliveries must not race a prepared migration. This is separate
// from parking the new supervisor: the old extension predates migration.
func adoptionHeldAgent(a Agent) bool {
	entries, _ := os.ReadDir(rpcRoot())
	for _, e := range entries {
		var w rpcWorker
		if rpcLoad(filepath.Join(rpcDir(e.Name()), "worker.json"), &w) == nil && w.Adoption != nil && (w.Adoption.Source.PaneID == a.PaneID || w.Adoption.Source.Name == a.Name) {
			return true
		}
	}
	return false
}

func rpcAdoptionCmd(cfg Config, h Herdr, pos []string, by string) (string, error) {
	op, id := pos[0], pos[1]
	b, err := (Beads{}).Show(id)
	if err != nil {
		return "", err
	}
	auto, err := cfg.enforce(b, "resume", by)
	if err != nil {
		return "", err
	}
	switch op {
	case "prepare":
		if len(pos) != 3 {
			return "", errors.New("usage: rpc prepare <bead> <exact-pane>")
		}
		err = rpcPrepare(cfg, h, b, pos[2])
	case "adopt":
		if len(pos) != 2 {
			return "", errors.New("usage: rpc adopt <bead>")
		}
		err = rpcAdopt(cfg, h, b)
	default:
		return "", errors.New("unknown adoption operation")
	}
	if err != nil {
		return "", err
	}
	msg := op + " " + id + "; migration parked, no prompt sent; inspect worker.json/status and preserve old pane until verification"
	if auto != "" {
		_ = (Beads{}).Note(id, auto+" "+msg)
	}
	return msg, nil
}
