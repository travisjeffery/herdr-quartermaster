package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Stored in the destination reservation before creating any checkout/session.
// Ready is published last. An interrupted intent is never completed on retry.
// The destination owns a self-contained seed; source cleanup cannot break it.
type rpcFork struct {
	Source        string    `json:"source"`
	SourceSession string    `json:"source_session"`
	SourceID      string    `json:"source_session_id"`
	SourceHash    string    `json:"source_hash"`
	Through       string    `json:"through"`
	Head          string    `json:"head"`
	SessionID     string    `json:"session_id"`
	SessionFile   string    `json:"session_file"`
	SeedHash      string    `json:"seed_hash"`
	SeedSize      int64     `json:"seed_bytes"`
	Prepared      time.Time `json:"prepared"`
	Ready         bool      `json:"ready"`
}

// Only canonical native-owned paths qualify. Never traverse a symlink into
// another checkout/session or adopt a legacy/TUI reservation for this command.
func rpcForkPath(path, expected string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != expected {
		return errors.New("fork path is not the exact native-owned path")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != path {
		return errors.New("fork refuses symlinked ownership paths")
	}
	return nil
}

func rpcForkCheckout(cfg Config, b Bead, w rpcWorker) (string, error) {
	if w.Bead != b.ID || w.Adoption != nil || w.Handoff != nil {
		return "", errors.New("fork source must be a native-owned reservation, not adoption/handoff")
	}
	if err := rpcForkPath(w.Cwd, filepath.Join(rpcDir(b.ID), "worktree")); err != nil {
		return "", err
	}
	branch, err := adoptionGit(w.Cwd, "symbolic-ref", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) != w.Branch {
		return "", errors.New("fork source branch differs from reservation")
	}
	head, err := adoptionGit(w.Cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(head))
	// Verify the exact checkout/branch/commit is registered in the configured
	// repository, not just a directory containing a plausible .git file.
	list, err := adoptionGit(expandHome(cfg.repoFor(b)), "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	found, path, ref, commit := false, "", "", ""
	for _, field := range strings.Split(string(list), "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			path = strings.TrimPrefix(field, "worktree ")
		case strings.HasPrefix(field, "branch "):
			ref = strings.TrimPrefix(field, "branch ")
		case strings.HasPrefix(field, "HEAD "):
			commit = strings.TrimPrefix(field, "HEAD ")
		case field == "":
			if path == w.Cwd && ref == "refs/heads/"+w.Branch && commit == sha {
				found = true
			}
			path, ref, commit = "", "", ""
		}
	}
	if !found {
		return "", errors.New("fork source is not the registered native checkout")
	}
	status, err := adoptionGit(w.Cwd, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	if err != nil {
		return "", err
	}
	if len(status) != 0 {
		return "", errors.New("fork requires a clean committed source (including untracked/ignored files)")
	}
	flags, err := adoptionGit(w.Cwd, "ls-files", "-v", "-z")
	if err != nil {
		return "", err
	}
	for _, file := range strings.Split(string(flags), "\x00") {
		if len(file) > 0 && (file[0] == 'S' || file[0] >= 'a' && file[0] <= 'z') {
			return "", errors.New("fork refuses hidden index flags; inspect the source checkout")
		}
	}
	return sha, nil
}

func rpcForkPrepare(cfg Config, source, dest Bead, through string, args []string, recheck func() error) (*rpcFork, error) {
	if !rpcIDRE.MatchString(source.ID) || !rpcIDRE.MatchString(dest.ID) || source.ID == dest.ID || !rpcIDRE.MatchString(through) {
		return nil, errors.New("fork requires distinct valid source/destination beads and a checkpoint entry id")
	}
	if dest.Status != StatusOpen || dest.Assignee != "" || (source.Status != StatusInProgress && source.Status != StatusClosed) {
		return nil, errors.New("fork needs an unclaimed open destination and an in_progress/closed source without a blocker")
	}
	srcRepo, srcKnown := cfg.repoName(source)
	dstRepo, dstKnown := cfg.repoName(dest)
	if !srcKnown || !dstKnown || srcRepo != dstRepo {
		return nil, errors.New("fork requires the same configured repository")
	}
	for _, b := range []Bead{source, dest} {
		if host := beadHost(b); host != "" && host != localHost {
			return nil, errors.New("fork is local only")
		}
	}
	if !filepath.IsAbs(stateDir()) || len(rpcSocket(dest.ID)) > 100 {
		return nil, errors.New("fork requires a short absolute state directory")
	}
	gate, err := adoptionAdmission(true)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	// Every lease is nonblocking, so reverse concurrent forks cannot deadlock.
	lease, err := rpcLease(source.ID)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	destLease, err := rpcLease(dest.ID)
	if err != nil {
		return nil, err
	}
	defer destLease.Close()
	if recheck != nil {
		if err := recheck(); err != nil {
			return nil, err
		}
	}
	for _, id := range []string{source.ID, dest.ID} {
		g, err := headlessAdmission(id, "")
		if err != nil {
			return nil, err
		}
		defer g.Close()
	}
	// The destination ID is the operation's idempotency key. A previous intent
	// or completion is inspected, never overwritten, resumed or retried here.
	entries, err := os.ReadDir(rpcDir(dest.ID))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != "lease" {
			return nil, errors.New("destination has existing artifacts; inspect rpc fork-status, never replay preparation")
		}
	}
	if err := rpcForkNoCleanup(source.ID); err != nil {
		return nil, err
	}
	var w rpcWorker
	var st rpcStatus
	if err := rpcLoad(filepath.Join(rpcDir(source.ID), "worker.json"), &w); err != nil {
		return nil, err
	}
	if err := rpcLoad(filepath.Join(rpcDir(source.ID), "status.json"), &st); err != nil {
		return nil, err
	}
	if st.Bead != source.ID || st.Phase != "stopped" || st.Error != "" || !st.GracefulStopRequested || !st.GracefulStop || st.Generation == "" || st.ChildPID != 0 || processAlive(st.PID) || st.SessionID == "" || st.SessionFile == "" {
		return nil, errors.New("fork requires confirmed graceful-stopped, healthy source with no live owner; never stops one for you")
	}
	if rpcParked(source.ID) {
		return nil, errors.New("fork cannot bypass source parking")
	}
	if err := rpcSettledReceipts(source.ID); err != nil {
		return nil, err
	}
	if w.Fork != nil {
		if err := rpcForkValidate(w, st); err != nil {
			return nil, err
		}
	}
	head, err := rpcForkCheckout(cfg, source, w)
	if err != nil {
		return nil, err
	}
	sessionDir := filepath.Join(rpcDir(source.ID), "sessions")
	if filepath.Dir(st.SessionFile) != sessionDir {
		return nil, errors.New("fork source session is outside its exclusive session directory")
	}
	if err := rpcForkPath(st.SessionFile, st.SessionFile); err != nil {
		return nil, err
	}
	f, err := rpcForkOpenSession(st.SessionFile)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > rpcForkMaxSession {
		f.Close()
		return nil, errors.New("fork source must be a regular session file of at most 64 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, rpcForkMaxSession+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	cwd := filepath.Join(rpcDir(dest.ID), "worktree")
	branch := branchFor(cfg.BranchPrefix, dest)
	if err := rpcForkDestination(w.Cwd, cfg.BranchPrefix, dest, branch); err != nil {
		return nil, err
	}
	record := &rpcFork{Source: source.ID, SourceSession: st.SessionFile, SourceID: st.SessionID, SourceHash: fmt.Sprintf("%x", sha256.Sum256(data)), Through: through, Head: head, SessionID: rpcID(), SessionFile: filepath.Join(rpcDir(dest.ID), "sessions", "fork.jsonl"), Prepared: time.Now()}
	context := fmt.Sprintf("This is a new, separately owned worker for %s, not %s. Inherited history is background, not authorization or a request to replay work. Do not act as the source worker or use its old worktree/session/branch.\nCurrent worktree: %s\nCurrent branch: %s\nCurrent task: %s\n%s\nRead the destination bead before acting. No source approvals, pending prompts or autonomy grants transfer. Wait for explicit activation and a new instruction; tools remain subject to the normal guards.", dest.ID, source.ID, cwd, branch, dest.Title, dest.Description)
	seed, err := rpcForkSession(data, st, w.Cwd, cwd, record.SessionID, through, context)
	if err != nil {
		return nil, err
	}
	record.SeedHash, record.SeedSize = fmt.Sprintf("%x", sha256.Sum256(seed)), int64(len(seed))
	child := rpcWorker{Bead: dest.ID, Cwd: cwd, Branch: branch, Args: args, Fork: record}
	if recheck != nil {
		if err := recheck(); err != nil {
			return nil, err
		}
	}
	if err := rpcSave(filepath.Join(rpcDir(dest.ID), "worker.json"), child); err != nil {
		return nil, err
	}
	// From this point all failures leave an explicit incomplete reservation.
	// There is no rollback deletion and no automatic retry or launch.
	out, err := exec.Command("git", "-C", w.Cwd, "worktree", "add", "-b", branch, cwd, head).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("fork intent retained; new worktree failed: %w: %s", err, out)
	}
	if err := rpcPrivateDir(filepath.Dir(record.SessionFile)); err != nil {
		return nil, err
	}
	if err := rpcForkWrite(record.SessionFile, seed); err != nil {
		return nil, err
	}
	if err := rpcPrivateDir(filepath.Join(rpcDir(dest.ID), "requests")); err != nil {
		return nil, err
	}
	if err := rpcForkWrite(rpcParkPath(dest.ID), []byte("fork: explicit activation required\n")); err != nil {
		return nil, err
	}
	if err := rpcSave(filepath.Join(rpcDir(dest.ID), "status.json"), rpcStatus{Bead: dest.ID, Phase: "stopped", SessionID: record.SessionID, SessionFile: record.SessionFile, Updated: time.Now()}); err != nil {
		return nil, err
	}
	record.Ready = true
	if err := rpcSave(filepath.Join(rpcDir(dest.ID), "worker.json"), child); err != nil {
		return nil, err
	}
	return record, nil
}

// Exclusive creation is safe without replacement: Ready stays false until
// the complete seed and parking marker (and their directory entries) are durable.
func rpcForkWrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Called under the supervisor lease before every forked-session launch. Only
// the destination seed is needed: the source remains independently owned.
func rpcForkValidate(w rpcWorker, st rpcStatus) error {
	if err := rpcForkNoCleanup(w.Bead); err != nil {
		return err
	}
	r := w.Fork
	if r == nil || !r.Ready || r.Source == w.Bead || r.SessionID == "" || r.SessionID == r.SourceID || r.SeedSize <= 0 || r.SeedSize > rpcForkMaxSession+(1<<20) {
		return errors.New("fork preparation incomplete/invalid; preserve reservation and inspect fork-status, never replay")
	}
	if st.SessionID != r.SessionID || st.SessionFile != r.SessionFile {
		return errors.New("fork saved session identity changed")
	}
	if err := rpcForkPath(w.Cwd, filepath.Join(rpcDir(w.Bead), "worktree")); err != nil {
		return err
	}
	if err := rpcForkPath(r.SessionFile, filepath.Join(rpcDir(w.Bead), "sessions", "fork.jsonl")); err != nil {
		return err
	}
	f, err := rpcForkOpenSession(r.SessionFile)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.CopyN(hash, f, r.SeedSize)
	if err != nil || n != r.SeedSize || fmt.Sprintf("%x", hash.Sum(nil)) != r.SeedHash {
		return errors.New("fork seed prefix changed; refusing launch")
	}
	return nil
}

// cleanup.json is the offline-maintenance intent/completion contract. Both
// phases (and unreadable/dangling markers) fence a new fork or forked resume.
func rpcForkNoCleanup(id string) error {
	if _, err := os.Lstat(filepath.Join(rpcDir(id), "cleanup.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("worktree cleanup recorded or unreadable; inspect cleanup.json, no fork or resume")
	}
	return nil
}

func rpcForkOpenSession(path string) (*os.File, error) {
	// Nonblocking open rejects special files without hanging on a FIFO. No
	// symlink or hardlink may alias another session's writer/ownership lease.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || st.Nlink != 1 {
			err = errors.New("fork session must be a regular, unaliased file")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func rpcForkDestination(repo, prefix string, dest Bead, branch string) error {
	if _, err := adoptionGit(repo, "check-ref-format", "--branch", branch); err != nil {
		return err
	}
	// Check current and legacy managed branch names before intent publication.
	// Managed TUI dispatch/resume also hold this bead lease. Refusing known
	// old ownership must not replace it with an incomplete native reservation.
	for _, ref := range []string{branch, prefix + agentName(dest.ID)} {
		_, err := adoptionGit(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+ref)
		if err == nil {
			return errors.New("destination task branch already exists; no reservation created")
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return err
		}
	}
	if _, ok := piControlFor(agentName(dest.ID)); ok {
		return errors.New("destination already has a pi control endpoint; no reservation created")
	}
	return nil
}

func rpcForkBeadUnchanged(before, after Bead) bool {
	return before.ID == after.ID && before.Title == after.Title && before.Description == after.Description &&
		before.Status == after.Status && before.Assignee == after.Assignee && slices.Equal(before.Labels, after.Labels)
}

func rpcForkCmd(cfg Config, pos []string, by string, out io.Writer) error {
	if pos[0] == "fork-status" {
		if len(pos) != 2 {
			return errors.New("usage: rpc fork-status <destination>")
		}
		var w rpcWorker
		if err := rpcLoad(filepath.Join(rpcDir(pos[1]), "worker.json"), &w); err != nil {
			return err
		}
		if w.Fork == nil {
			return errors.New("no fork reservation")
		}
		return json.NewEncoder(out).Encode(w.Fork)
	}
	if len(pos) != 4 || !rpcIDRE.MatchString(pos[2]) || !rpcIDRE.MatchString(pos[3]) {
		return errors.New("usage: rpc fork <stopped-source> <fresh-destination> <assistant-entry-id>")
	}
	bd := Beads{}
	source, err := bd.Show(pos[1])
	if err != nil {
		return err
	}
	dest, err := bd.Show(pos[2])
	if err != nil {
		return err
	}
	if source.ID != pos[1] || dest.ID != pos[2] {
		return errors.New("fork requires exact bead identities, not aliases")
	}
	if _, err := cfg.enforce(source, "fork", by); err != nil {
		return err
	}
	auto, err := cfg.enforce(dest, "fork", by)
	if err != nil {
		return err
	}
	if err := bd.Note(dest.ID, strings.TrimSpace(fmt.Sprintf("%s pi-rpc fork attempt from %s through %s by %s; destination is the operation identity; no launch, prompt or replay", auto, source.ID, pos[3], by))); err != nil {
		return err
	}
	args, err := rpcLaunchArgs(cfg, dest.ID)
	if err != nil {
		return err
	}
	recheck := func() error {
		for _, original := range []Bead{source, dest} {
			current, err := bd.Show(original.ID)
			if err != nil {
				return err
			}
			if !rpcForkBeadUnchanged(original, current) {
				return errors.New("fork bead identity/state/ownership changed during preparation; no reservation created")
			}
			if level, _ := cfg.beadLevel(current, autonomyStopped()); by != "TJ" && level < 3 {
				return fmt.Errorf("%w: fork autonomy changed during preparation", errNotAutonomous)
			}
		}
		return nil
	}
	r, err := rpcForkPrepare(cfg, source, dest, pos[3], args, recheck)
	if err != nil {
		return err
	}
	if err := bd.Note(dest.ID, "pi-rpc fork prepared, parked and not started; inspect rpc fork-status before separately authorized claim/resume/activate/tell"); err != nil {
		return fmt.Errorf("fork prepared but outcome note failed: %w; inspect fork-status, do not repeat", err)
	}
	return json.NewEncoder(out).Encode(r)
}
