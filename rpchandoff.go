package main

// Fresh handoff is an explicit change of harness, not transcript conversion or
// relaxed pi adoption. It uses configured pi guards, retains the old transcript,
// and checkpoints only the named source process tree/sessions (not every host
// process's cwd). The operator must exclude independently detached writers.
import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type rpcHandoff struct {
	Checkpoint rpcAdoption             `json:"checkpoint"`
	Context    string                  `json:"context"`
	Mapping    *handoffCheckoutMapping `json:"checkout_mapping,omitempty"`
}

// A per-handoff operator attestation, not a global branch alias or force flag.
// Pin both the checkout and the exact source incarnation before preparation.
// All other admission, exit, file, transcript and parking checks still apply.
type handoffCheckoutMapping struct {
	Head        string `json:"head"`
	Ref         string `json:"ref"` // full refs/heads/... or HEAD for detached
	SourcePID   int    `json:"source_pid"`
	SourceStart string `json:"source_start_ticks"`
	SourceBoot  string `json:"source_boot_id"`
	Evidence    string `json:"evidence"`
}

func handoffCheckout(cwd string, mapping *handoffCheckoutMapping) (string, string, error) {
	if mapping == nil {
		return handoffDigest(cwd) // default and strict native adoption stay unchanged
	}
	if strings.TrimSpace(mapping.Evidence) == "" || len(mapping.Evidence) > 32<<10 || mapping.SourcePID <= 0 || mapping.SourceStart == "" || mapping.SourceBoot == "" {
		return "", "", errors.New("checkout mapping requires source identity and 1..32KiB ownership evidence")
	}
	if mapping.Ref != "HEAD" && !strings.HasPrefix(mapping.Ref, "refs/heads/") {
		return "", "", errors.New("checkout mapping requires a full branch ref or HEAD for detached")
	}
	ref, err := adoptionGit(cwd, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return "", "", err // only symbolic-ref's documented detached status
		}
		ref = []byte("HEAD")
	}
	head, err := adoptionGit(cwd, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", err
	}
	if mapping.Ref != strings.TrimSpace(string(ref)) || mapping.Head != strings.TrimSpace(string(head)) {
		return "", "", errors.New("checkout mapping ref/HEAD does not match; preserve source and inspect")
	}
	digest, err := checkoutDigest(cwd, 16)
	return strings.TrimPrefix(mapping.Ref, "refs/heads/"), digest, err
}

type handoffInput struct {
	Cwd        string                  `json:"cwd"`
	Transcript string                  `json:"transcript"`
	Context    string                  `json:"context"` // progress, pending question verbatim, next authorized step
	Mapping    *handoffCheckoutMapping `json:"checkout_mapping,omitempty"`
}

// Pi 1.5 buffers a new session until its first assistant response. Seed a
// NEW documented v3 session with only the authored handoff custom message so
// parking survives a restart without a model turn. Never translate old entries,
// fake an assistant response, or overwrite an existing native session.
func handoffSeed(w rpcWorker) (string, error) {
	dir := filepath.Join(rpcDir(w.Bead), "sessions")
	if err := rpcPrivateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "handoff.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	enc := json.NewEncoder(f)
	if err := enc.Encode(map[string]any{"type": "session", "version": 3, "id": w.Bead, "timestamp": now, "cwd": w.Cwd}); err != nil {
		return "", err
	}
	if err := enc.Encode(map[string]any{"type": "custom_message", "id": rpcID(), "parentId": nil, "timestamp": now, "customType": "quartermaster.handoff", "content": w.Handoff.Context, "display": true}); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer d.Close()
	return path, d.Sync()
}

func handoffPrepare(cfg Config, h Herdr, b Bead, pane, input string) error {
	if beadHost(b) != "" && beadHost(b) != localHost {
		return errors.New("fresh handoff is local only")
	}
	if b.Status != StatusOpen && b.Status != StatusInProgress && b.Status != StatusBlocked && b.Status != StatusNeedsMe {
		return errors.New("handoff requires an existing active task; retire completed tasks without restarting")
	}
	if _, ok := cfg.repoName(b); !ok {
		return errors.New("unknown repository label")
	}
	if !filepath.IsAbs(stateDir()) || len(rpcSocket(b.ID)) > 100 {
		return errors.New("requires a short absolute state directory")
	}
	var in handoffInput
	if err := rpcLoad(input, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Context) == "" || len(in.Context) > 128<<10 || !filepath.IsAbs(in.Cwd) || !filepath.IsAbs(in.Transcript) {
		return errors.New("handoff needs absolute cwd/transcript and 1..128KiB context including unresolved questions")
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
		return errors.New("reservation exists; inspect, never overwrite")
	}
	agents, err := adoptionAgents(h)
	if err != nil {
		return err
	}
	var source *Agent
	for i := range agents {
		if agents[i].Name == agentName(b.ID) {
			if source != nil {
				return errors.New("multiple source workers")
			}
			source = &agents[i]
		}
	}
	if source == nil || source.PaneID != pane || source.Name == cfg.CoordinatorName || pane == os.Getenv("HERDR_PANE_ID") {
		return errors.New("requires exact source worker, excluding caller and coordinator")
	}
	if source.Kind != "claude" && source.Kind != "codex" && source.Kind != "muse" {
		return errors.New("fresh handoff supports claude/codex/muse; pi workers retain the strict adoption path")
	}
	if !agentReady(source) && source.Status != "blocked" {
		return errors.New("source must be idle/done or parked at an unanswered question")
	}
	cwd, err := filepath.EvalSymlinks(in.Cwd)
	if err != nil {
		return err
	}
	if rpcReservedPath(cwd) {
		return errors.New("worktree already reserved")
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
		return errors.New("handoff cwd must be a linked worktree of the bead repository")
	}
	branch, digest, err := handoffCheckout(cwd, in.Mapping)
	if err != nil {
		return err
	}
	if in.Mapping == nil && !onBeadBranch(cfg.BranchPrefix, branch, b) {
		return errors.New("worktree branch does not belong to bead")
	}
	for _, a := range agents {
		if a.Kind != "" && a.PaneID != pane && a.Cwd == cwd {
			return errors.New("another live agent occupies the worktree")
		}
	}
	info, err := adoptionProcesses(h, pane)
	if err != nil {
		return err
	}
	ownerPID, pids, err := handoffSourceTree(info)
	if err != nil {
		return err
	}
	// The input is an operator-reviewed handoff, not permission to pick an
	// unrelated transcript. Known session IDs must match the retained filename.
	if source.Session.Value != "" && source.Session.Kind == "id" && !strings.Contains(filepath.Base(in.Transcript), source.Session.Value) {
		return errors.New("transcript does not name the source session")
	}
	transcript, err := os.ReadFile(in.Transcript)
	if err != nil {
		return err
	}
	if len(transcript) == 0 {
		return errors.New("empty original transcript")
	}
	a := rpcAdoption{Source: *source, Bead: b, PIDs: pids, ShellPID: info.ShellPID, Digest: digest, SessionFile: in.Transcript, SessionSize: int64(len(transcript)), SessionHash: fmt.Sprintf("%x", sha256.Sum256(transcript)), Prepared: time.Now()}
	for _, pid := range pids {
		owner, err := adoptionIdentity("/proc", pid)
		if err != nil {
			return err
		}
		leader, err := adoptionIdentity("/proc", owner.Session)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		owner.SessionStart = leader.Start
		if in.Mapping != nil && pid == ownerPID {
			m := in.Mapping
			if m.SourcePID != owner.PID || m.SourceStart != owner.Start || m.SourceBoot != owner.Boot {
				return errors.New("checkout mapping source identity changed; preserve source and inspect")
			}
		}
		a.Owners = append(a.Owners, owner)
	}
	a.Shell, err = adoptionIdentity("/proc", info.ShellPID)
	if err != nil {
		return err
	}
	args, err := rpcLaunchArgs(cfg, b.ID)
	if err != nil {
		return err
	}
	a.GuardEnv = adoptionGuardEnv(append(os.Environ(), cfg.Agents["pi"].Env...))
	status, err := adoptionGit(cwd, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	a.GitStatus = string(status)
	again, err := adoptionProcesses(h, pane)
	if err != nil {
		return err
	}
	descendants, err := adoptionDescendants(ownerPID)
	if err != nil {
		return err
	}
	live, err := adoptionAgents(h)
	if err != nil {
		return err
	}
	same := false
	for _, current := range live {
		if current.PaneID == pane && current.Name == source.Name && current.Kind == source.Kind && current.Session == source.Session && current.Seq == source.Seq && current.Status == source.Status {
			same = true
		}
	}
	if !same || !reflect.DeepEqual(info, again) || !reflect.DeepEqual(pids, descendants) {
		return errors.New("source changed during checkpoint; wait for quiescence")
	}
	context := fmt.Sprintf("Fresh harness handoff for %s: %s\nOriginal %s transcript (read-only history): %s\nWorktree: %s\nBranch: %s\nStatus: %s; original assignee: %s\n\nThis is task context, not approval. Preserve unresolved questions; migration authorizes no task action. Existing configured pi guards apply; no cross-harness equivalence is claimed.\n\n%s\n\nTask description:\n%s\n\nWorking notes at handoff:\n%s", b.ID, b.Title, source.Kind, in.Transcript, cwd, branch, b.Status, b.Assignee, in.Context, b.Description, b.Notes)
	if in.Mapping != nil {
		// Check the pinned ref/HEAD again after the source census, before any
		// reservation. Mapping is not permission to adopt a moving checkout.
		currentBranch, currentDigest, err := handoffCheckout(cwd, in.Mapping)
		if err != nil {
			return err
		}
		if currentBranch != branch || currentDigest != digest {
			return errors.New("mapped checkout changed during checkpoint")
		}
		context += fmt.Sprintf("\n\nExplicit operator checkout mapping (not task approval):\nRef: %s\nHEAD: %s\nOwnership evidence:\n%s", in.Mapping.Ref, in.Mapping.Head, in.Mapping.Evidence)
	}
	if len(context) > 512<<10 {
		return errors.New("handoff context exceeds 512KiB; prepare a bounded summary")
	}
	if err := rpcSave(rpcParkPath(b.ID), map[string]string{"reason": "fresh harness handoff; no continuation or approval"}); err != nil {
		return err
	}
	return rpcSave(filepath.Join(rpcDir(b.ID), "worker.json"), rpcWorker{Bead: b.ID, Cwd: cwd, Branch: branch, Args: args, Handoff: &rpcHandoff{Checkpoint: a, Context: context, Mapping: in.Mapping}})
}

// Some harnesses place their MCP children in the foreground process group.
// Select the unique ancestor covering the entire group, not its first entry.
func handoffSourceTree(info adoptionProcessInfo) (int, []int, error) {
	root := 0
	var pids []int
	for _, p := range info.Procs {
		if p.PID <= 0 || p.PID == info.ShellPID {
			return 0, nil, errors.New("source is not a live agent")
		}
		tree, err := adoptionDescendants(p.PID)
		if err != nil {
			return 0, nil, err
		}
		members := map[int]bool{}
		for _, pid := range tree {
			members[pid] = true
		}
		covers := true
		for _, foreground := range info.Procs {
			if !members[foreground.PID] {
				covers = false
			}
		}
		if covers {
			if root != 0 {
				return 0, nil, errors.New("multiple source roots")
			}
			root, pids = p.PID, tree
		}
	}
	if root == 0 {
		return 0, nil, errors.New("ambiguous source foreground process tree")
	}
	return root, pids, nil
}

// Exact recorded identities plus surviving source-session members. Deliberately
// no host-wide cwd census: this is the explicit fresh-handoff scope, not proof
// that no independent process anywhere can write this checkout.
func handoffRemaining(root string, a *rpcAdoption) error {
	if len(a.Owners) == 0 {
		return errors.New("missing source process identities")
	}
	sessions := map[int]bool{}
	for _, old := range a.Owners {
		live, err := adoptionIdentity(root, old.PID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && live.Start == old.Start && live.Boot == old.Boot {
			return fmt.Errorf("source process %d still alive", old.PID)
		}
		boot, err := os.ReadFile(filepath.Join(root, "sys/kernel/random/boot_id"))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(boot)) != old.Boot {
			return errors.New("boot changed since source checkpoint")
		}
		leader, err := adoptionIdentity(root, old.Session)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if errors.Is(err, os.ErrNotExist) || old.SessionStart == "" || leader.Start == old.SessionStart {
			sessions[old.Session] = true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == a.ShellPID {
			continue
		}
		fi, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
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
		if sessions[live.Session] {
			return fmt.Errorf("source-session process %d remains; preserve it", pid)
		}
	}
	return nil
}

func handoffValidate(h Herdr, b Bead, w rpcWorker) error {
	a := &w.Handoff.Checkpoint
	if b.Status == StatusClosed || b.Status != a.Bead.Status || b.Assignee != a.Bead.Assignee {
		return errors.New("bead status/owner changed; preserve reservation and inspect")
	}
	if err := handoffRemaining("/proc", a); err != nil {
		return err
	}
	// Reuse pane/Herdr identity checks without invoking strict pi cwd census.
	shellCheck := *a
	shellCheck.Owners = nil
	shellCheck.PIDs = nil
	paneWorker := w
	paneWorker.Adoption = &shellCheck
	if err := adoptionStopped(h, paneWorker); err != nil {
		return err
	}
	branch, digest, err := handoffCheckout(w.Cwd, w.Handoff.Mapping)
	if err != nil {
		return err
	}
	if branch != w.Branch || digest != a.Digest {
		return errors.New("worktree changed since handoff checkpoint")
	}
	if err := adoptionPrefix(a); err != nil {
		return err
	}
	if !rpcParked(w.Bead) {
		return errors.New("handoff parking barrier missing")
	}
	return nil
}

func handoffStart(cfg Config, h Herdr, b Bead) error {
	lease, err := rpcLease(b.ID)
	if err != nil {
		return err
	}
	defer lease.Close()
	var w rpcWorker
	if err := rpcLoad(filepath.Join(rpcDir(b.ID), "worker.json"), &w); err != nil {
		return err
	}
	if w.Handoff == nil || w.Handoff.Checkpoint.Ready {
		return errors.New("requires prepared fresh handoff; use rpc resume after launch")
	}
	if err := handoffValidate(h, b, w); err != nil {
		return err
	}
	w.Handoff.Checkpoint.Ready = true
	if err := rpcSave(filepath.Join(rpcDir(b.ID), "worker.json"), w); err != nil {
		return err
	}
	lease.Close()
	return rpcStart(b.ID)
}

func rpcHandoffCmd(cfg Config, h Herdr, pos []string, by string) (string, error) {
	b, err := (Beads{}).Show(pos[1])
	if err != nil {
		return "", err
	}
	auto, err := cfg.enforce(b, "resume", by)
	if err != nil {
		return "", err
	}
	switch {
	case pos[0] == "handoff-prepare" && len(pos) == 4:
		err = handoffPrepare(cfg, h, b, pos[2], pos[3])
	case pos[0] == "handoff" && len(pos) == 2:
		err = handoffStart(cfg, h, b)
	default:
		return "", errors.New("usage: rpc handoff-prepare <bead> <exact-pane> <handoff.json> | rpc handoff <bead>")
	}
	if err != nil {
		return "", err
	}
	msg := pos[0] + " " + b.ID + "; fresh handoff parked, no model turn or approval; retain original pane until verified"
	if auto != "" {
		_ = (Beads{}).Note(b.ID, auto+" "+msg)
	}
	return msg, nil
}
