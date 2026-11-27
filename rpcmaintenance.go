package main

// Maintenance is explicit and offline. Never unlink the lease/reservation or
// touch adopted/TUI checkouts, saved sessions, receipts, parking or inboxes.
import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type RPCRetentionConfig struct {
	EventBytes int64 `toml:"event_bytes"` // 0 disables retention; no scheduled work
}

const rpcRetentionGap = "quartermaster_retention_gap"
const rpcRetentionReserve = 512

type rpcLogGap struct {
	Type    string `json:"type"`
	Bytes   int64  `json:"removed_bytes"`
	Records int64  `json:"removed_records"`
}

type rpcMaintenanceReport struct {
	Operation string `json:"operation"`
	Bead      string `json:"bead"`
	Applied   bool   `json:"applied"`
	Change    bool   `json:"change"` // would change (preview), or did change (apply)
	Before    int64  `json:"before_bytes,omitempty"`
	After     int64  `json:"after_bytes,omitempty"`
	Removed   int64  `json:"removed_records,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
	Head      string `json:"head,omitempty"`
	Message   string `json:"message"`
}

type rpcCleanupRecord struct {
	Bead  string    `json:"bead"`
	Cwd   string    `json:"cwd"`
	Repo  string    `json:"repo"`
	Head  string    `json:"head"`
	Phase string    `json:"phase"` // intent or removed; both fence resume
	At    time.Time `json:"at"`
}

func rpcCleanupPath(id string) string { return filepath.Join(rpcDir(id), "cleanup.json") }

func rpcCleanupBarrier(id string) error {
	if _, err := os.Lstat(rpcCleanupPath(id)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("worktree cleanup recorded or unreadable; inspect cleanup.json and restore the recorded checkout before explicitly clearing the cleanup checkpoint; no resume attempted")
	}
	return nil
}

// Require canonical paths: neither a symlinked root nor a symlinked artifact
// can turn maintenance into removal of another worker's evidence.
func rpcMaintenancePath(path string, dir bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("noncanonical maintenance path: %s", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != path {
		return fmt.Errorf("symlinked maintenance path: %s", path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if dir && !fi.IsDir() || !dir && !fi.Mode().IsRegular() {
		return fmt.Errorf("unexpected maintenance artifact: %s", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !dir && ok && st.Nlink != 1 {
		return fmt.Errorf("hard-linked maintenance artifact: %s", path)
	}
	return nil
}

func rpcMaintenanceLock(id string) (*os.File, rpcWorker, error) {
	var w rpcWorker
	if !rpcIDRE.MatchString(id) {
		return nil, w, errors.New("invalid bead id")
	}
	for _, name := range []string{"worker.json", "lease", "status.json"} {
		if err := rpcMaintenancePath(filepath.Join(rpcDir(id), name), false); err != nil {
			return nil, w, err
		}
	}
	// Open the existing inode, never create or replace it (also in dry runs).
	lease, err := os.OpenFile(filepath.Join(rpcDir(id), "lease"), os.O_RDWR, 0)
	if err != nil {
		return nil, w, err
	}
	fail := func(err error) (*os.File, rpcWorker, error) { lease.Close(); return nil, w, err }
	if err = syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(errors.New("live session lease; maintenance refused"))
	}
	if err = rpcLoad(filepath.Join(rpcDir(id), "worker.json"), &w); err != nil {
		return fail(err)
	}
	if w.Bead != id || w.Adoption != nil || w.Handoff != nil || w.Cwd != filepath.Join(rpcDir(id), "worktree") || w.Branch == "" {
		return fail(errors.New("maintenance requires a native-dispatch reservation; adopted/handoff/TUI/worktree-free paths are preserved"))
	}
	if w.Fork != nil && !w.Fork.Ready {
		return fail(errors.New("incomplete fork reservation; preserve worker and inspect fork-status"))
	}
	var st rpcStatus
	if err = rpcLoad(filepath.Join(rpcDir(id), "status.json"), &st); err != nil {
		return fail(err)
	}
	if st.Bead != id || st.Phase != "stopped" || st.Error != "" || st.ChildPID != 0 || processAlive(st.PID) {
		return fail(errors.New("no confirmed stopped owner; preserve worker and inspect status"))
	}
	if w.Fork != nil && (st.SessionID != w.Fork.SessionID || st.SessionFile != w.Fork.SessionFile) {
		return fail(errors.New("fork session identity mismatch; preserve worker and inspect fork-status"))
	}
	if st.SessionID == "" || !inside(st.SessionFile, filepath.Join(rpcDir(id), "sessions")) {
		return fail(errors.New("saved native session identity unavailable"))
	}
	if err = rpcMaintenancePath(st.SessionFile, false); err != nil {
		return fail(err)
	}
	if err = rpcMaintenanceSession(st, w.Cwd); err != nil {
		return fail(err)
	}
	if err = rpcMaintenanceReceipts(id); err != nil {
		return fail(err)
	}
	return lease, w, nil
}

func rpcMaintenanceSession(st rpcStatus, cwd string) error {
	f, err := os.Open(st.SessionFile)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 64<<10).ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("saved session header: %w", err)
	}
	var header struct {
		Type    string
		Version int
		ID      string
		Cwd     string
	}
	if json.Unmarshal(line, &header) != nil || header.Type != "session" || header.Version != 3 || header.ID != st.SessionID || header.Cwd != cwd {
		return errors.New("saved session header does not match reservation/status")
	}
	return nil
}

func rpcMaintenanceReceipts(id string) error {
	dir := filepath.Join(rpcDir(id), "requests")
	if err := rpcMaintenancePath(dir, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			return errors.New("unexpected request artifact; preserve reconciliation evidence")
		}
		path := filepath.Join(dir, e.Name())
		if err := rpcMaintenancePath(path, false); err != nil {
			return err
		}
		var r rpcReceipt
		if err := rpcLoad(path, &r); err != nil {
			return err
		}
		if (r.State != "accepted" && r.State != "rejected") || r.ID+".json" != e.Name() || r.Hash == "" || r.Response.Type != "response" || r.Response.ID != r.ID || r.Response.Command == "" || (r.State == "accepted") != r.Response.Success {
			return errors.New("incomplete request receipt; preserve reconciliation evidence")
		}
	}
	return nil
}

// Responses and extension dialogs/errors are always protected, even if a
// future protocol adds a response kind this version doesn't understand.
func rpcProtectedEvent(typ string) bool {
	return typ == "response" || typ == "extension_ui_request" || typ == "extension_error"
}

func rpcWalkRetention(f *os.File, fn func([]byte, string) error) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	err := readRPCLog(f, func(raw []byte) error {
		var record struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &record) != nil || record.Type == "" {
			return errors.New("malformed event log; preserve for investigation")
		}
		return fn(raw, record.Type)
	})
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err // partial records are not permission to discard evidence
}

// Two streaming passes, bounded by one protocol record. An atomic replacement
// includes the gap record, so a crash cannot publish pruned data without its gap.
// The caller holds the worker lease throughout both passes and publication.
func rpcRetainEvents(id string, limit int64, apply bool, recheck func() error) (rpcMaintenanceReport, error) {
	r := rpcMaintenanceReport{Operation: "retain", Bead: id, Message: "sessions, receipts, diagnostics and protected response/dialog events retained"}
	if limit < 1024 {
		return r, errors.New("event retention disabled or invalid; set [rpc_retention] event_bytes or --max-bytes to at least 1024")
	}
	path := filepath.Join(rpcDir(id), "events.jsonl")
	if err := rpcMaintenancePath(path, false); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return r, err
	}
	r.Before, r.After = fi.Size(), fi.Size()
	var protected, disposable int64
	gap := rpcLogGap{Type: rpcRetentionGap}
	err = rpcWalkRetention(f, func(raw []byte, typ string) error {
		switch {
		case typ == rpcRetentionGap:
			var old rpcLogGap
			if json.Unmarshal(raw, &old) != nil || old.Bytes < 0 || old.Records < 0 {
				return errors.New("invalid retention checkpoint")
			}
			gap.Bytes += old.Bytes
			gap.Records += old.Records
		case rpcProtectedEvent(typ):
			protected += int64(len(raw) + 1)
		default:
			disposable += int64(len(raw) + 1)
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	if r.Before <= limit {
		return r, nil
	}
	budget := limit - protected - rpcRetentionReserve
	if budget < 0 {
		return r, errors.New("protected response/dialog evidence exceeds retention budget; increase it, no evidence removed")
	}
	var tmp *os.File
	if apply {
		tmp, err = os.CreateTemp(filepath.Dir(path), ".retention-*")
		if err != nil {
			return r, err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		// Reserve a fixed-width first line, filled before publication.
		if _, err = tmp.Write(bytes.Repeat([]byte(" "), rpcRetentionReserve)); err != nil {
			return r, err
		}
	}
	r.After = rpcRetentionReserve
	err = rpcWalkRetention(f, func(raw []byte, typ string) error {
		if typ == rpcRetentionGap {
			return nil
		}
		n := int64(len(raw) + 1)
		keep := rpcProtectedEvent(typ)
		if !keep {
			keep = disposable <= budget
			disposable -= n
		}
		if !keep {
			gap.Bytes += n
			gap.Records++
			r.Removed++
			return nil
		}
		r.After += n
		if tmp != nil {
			_, err := tmp.Write(append(raw, '\n'))
			return err
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	r.Change = true
	if tmp == nil {
		return r, nil
	}
	line, err := json.Marshal(gap)
	if err != nil || len(line) >= rpcRetentionReserve {
		return r, errors.New("retention checkpoint exceeds reserved size")
	}
	line = append(line, bytes.Repeat([]byte(" "), rpcRetentionReserve-1-len(line))...)
	line = append(line, '\n')
	if _, err = tmp.WriteAt(line, 0); err != nil {
		return r, err
	}
	if err = tmp.Sync(); err != nil {
		return r, err
	}
	if err = tmp.Close(); err != nil {
		return r, err
	}
	if recheck != nil {
		if err = recheck(); err != nil {
			return r, err
		}
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return r, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return r, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return r, err
	}
	r.Applied = true
	return r, nil
}

// Exact local ancestry, or a merged PR at this exact head whose merge commit
// is in the configured base. A branch-name-only merged PR is not sufficient.
func rpcCleanupMerged(repo, branch, head, base string) error {
	if base == "" || strings.HasPrefix(base, "-") {
		return errors.New("invalid configured base branch; no cleanup")
	}
	if _, err := gitRun(repo, "--no-optional-locks", "merge-base", "--is-ancestor", head, base); err == nil {
		return nil
	}
	out, err := (GH{repo: repo}).run("pr", "list", "--state", "merged", "--head", branch, "--limit", "100", "--json", "headRefOid,mergeCommit")
	if err != nil {
		return err
	}
	var prs []struct {
		Head  string `json:"headRefOid"`
		Merge struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err = json.Unmarshal(out, &prs); err != nil {
		return err
	}
	for _, pr := range prs {
		if pr.Head == head && pr.Merge.OID != "" {
			if _, err := gitRun(repo, "--no-optional-locks", "merge-base", "--is-ancestor", pr.Merge.OID, base); err == nil {
				return nil
			}
		}
	}
	return errors.New("worktree head not proven merged into configured base; fetch/review separately, no cleanup")
}

// Helpers are injectable for crash-window tests, not configurable command hooks.
type rpcCleanupEnv struct {
	git     func(string, ...string) (string, error)
	merged  func(string, string, string, string) error
	recheck func() error
}

func rpcRemoveWorktree(cfg Config, b Bead, w rpcWorker, apply bool, env rpcCleanupEnv) (rpcMaintenanceReport, error) {
	r := rpcMaintenanceReport{Operation: "cleanup", Bead: b.ID, Worktree: w.Cwd, Message: "branch, reservation, saved sessions, receipts and logs retained"}
	if b.Status != StatusClosed {
		return r, errors.New("active bead; cleanup requires a closed bead (no force override)")
	}
	if _, ok := cfg.repoName(b); !ok {
		return r, errors.New("unknown repository label")
	}
	repo := cfg.repoFor(b)
	if err := rpcMaintenancePath(repo, true); err != nil {
		return r, err
	}
	checkpoint := rpcCleanupPath(b.ID)
	var saved rpcCleanupRecord
	if _, lerr := os.Lstat(checkpoint); lerr == nil {
		if err := rpcMaintenancePath(checkpoint, false); err != nil {
			return r, err
		}
		if err := rpcLoad(checkpoint, &saved); err != nil {
			return r, err
		}
		if saved.Bead != b.ID || saved.Cwd != w.Cwd || saved.Repo != repo || saved.Head == "" || (saved.Phase != "intent" && saved.Phase != "removed") {
			return r, errors.New("cleanup checkpoint mismatch")
		}
	} else if !errors.Is(lerr, os.ErrNotExist) {
		return r, lerr
	}
	_, statErr := os.Lstat(w.Cwd)
	if errors.Is(statErr, os.ErrNotExist) && saved.Head != "" {
		// Git may have finished before the completion checkpoint was saved.
		// Never prune other registrations or infer success from absence alone.
		listed, err := env.git(repo, "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return r, err
		}
		for _, field := range strings.Split(listed, "\x00") {
			if field == "worktree "+w.Cwd {
				return r, errors.New("checkout absent but still registered; interrupted git removal requires inspection")
			}
		}
		r.Head = saved.Head
		r.Message += "; removal already completed"
		r.Change = saved.Phase != "removed"
		if apply && r.Change {
			saved.Phase = "removed"
			if err = rpcSave(checkpoint, saved); err != nil {
				return r, err
			}
			r.Applied = true
		}
		return r, nil
	}
	if statErr != nil {
		return r, statErr
	}
	if saved.Phase == "removed" {
		return r, errors.New("worktree exists after completed cleanup; inspect restored checkout before changing checkpoint")
	}
	if err := rpcMaintenancePath(w.Cwd, true); err != nil {
		return r, err
	}
	if err := rpcMaintenancePath(filepath.Join(w.Cwd, ".git"), false); err != nil {
		return r, errors.New("not a native linked checkout")
	}
	git := func(dir string, args ...string) (string, error) {
		s, e := env.git(dir, append([]string{"--no-optional-locks"}, args...)...)
		return strings.TrimSpace(s), e
	}
	common, err := git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return r, err
	}
	other, err := git(w.Cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || other != common {
		return r, errors.New("worktree repository mismatch")
	}
	top, err := git(w.Cwd, "rev-parse", "--show-toplevel")
	if err != nil || top != w.Cwd {
		return r, errors.New("worktree top-level mismatch")
	}
	listed, err := env.git(repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return r, err
	}
	registered := false
	for _, entry := range strings.Split(listed, "\x00\x00") {
		fields := strings.Split(entry, "\x00")
		if len(fields) == 0 || fields[0] != "worktree "+w.Cwd {
			continue
		}
		registered = true
		for _, field := range fields {
			if field == "locked" || strings.HasPrefix(field, "locked ") || field == "prunable" || strings.HasPrefix(field, "prunable ") {
				return r, errors.New("locked or prunable checkout; inspect before cleanup")
			}
		}
	}
	if !registered {
		return r, errors.New("native checkout is not registered")
	}
	branch, err := git(w.Cwd, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != w.Branch {
		return r, errors.New("worktree branch changed")
	}
	cleanCheckout := func(msg string) error {
		status, err := git(w.Cwd, "status", "--porcelain=v1", "--untracked-files=all", "--ignored", "--ignore-submodules=none")
		if err != nil {
			return err
		}
		if status != "" {
			return errors.New(msg)
		}
		flags, err := env.git(w.Cwd, "--no-optional-locks", "ls-files", "-v", "-z")
		if err != nil {
			return err
		}
		for _, file := range strings.Split(flags, "\x00") {
			if len(file) > 0 && (file[0] == 'S' || file[0] >= 'a' && file[0] <= 'z') {
				return errors.New("hidden git index flags; preserve checkout")
			}
		}
		return nil
	}
	if err = cleanCheckout("dirty/untracked/ignored worktree files; preserve checkout"); err != nil {
		return r, err
	}
	head, err := git(w.Cwd, "rev-parse", "HEAD")
	if err != nil {
		return r, err
	}
	if saved.Head != "" && saved.Head != head {
		return r, errors.New("worktree head changed since interrupted cleanup")
	}
	if err = env.merged(repo, branch, head, cfg.BaseBranch); err != nil {
		return r, err
	}
	r.Head = head
	r.Change = true
	if !apply {
		return r, nil
	}
	saved = rpcCleanupRecord{Bead: b.ID, Cwd: w.Cwd, Repo: repo, Head: head, Phase: "intent", At: time.Now().UTC()}
	if err = rpcSave(checkpoint, saved); err != nil {
		return r, err
	}
	// The merge lookup can be slow. Revalidate after it and the intent write,
	// immediately before removal. This is not a lock against arbitrary editors
	// or detached processes: operators must stop those writers separately.
	if env.recheck != nil {
		if err = env.recheck(); err != nil {
			return r, err
		}
	}
	if err = cleanCheckout("worktree changed during cleanup; intent retained, no removal"); err != nil {
		return r, err
	}
	selected, err := git(w.Cwd, "rev-parse", "HEAD")
	if err != nil || selected != head {
		return r, errors.New("worktree head changed during cleanup")
	}
	selected, err = git(w.Cwd, "symbolic-ref", "--short", "HEAD")
	if err != nil || selected != branch {
		return r, errors.New("worktree branch changed during cleanup")
	}
	if _, err = env.git(repo, "worktree", "remove", "--", w.Cwd); err != nil {
		return r, err
	}
	saved.Phase = "removed"
	if err = rpcSave(checkpoint, saved); err != nil {
		return r, err
	}
	r.Applied = true
	return r, nil
}

func rpcMaintenanceCmd(cfg Config, pos []string, by string, apply bool, limit int64, out io.Writer) error {
	if len(pos) != 2 || (pos[0] != "retain" && pos[0] != "cleanup") {
		return errors.New("usage: rpc retain|cleanup <bead> [--yes] [--max-bytes N]")
	}
	lease, w, err := rpcMaintenanceLock(pos[1])
	if err != nil {
		return err
	}
	defer lease.Close()
	b, err := (Beads{}).Show(pos[1])
	if err != nil {
		return err
	}
	if b.ID != pos[1] {
		return errors.New("bead identity mismatch")
	}
	if b.Status != StatusClosed {
		return errors.New("active bead; maintenance requires a closed bead (no force override)")
	}
	if pos[0] == "cleanup" && limit != 0 {
		return errors.New("--max-bytes is only for rpc retain")
	}
	if limit == 0 {
		limit = cfg.RPCRetention.EventBytes
	}
	recheck := func() error {
		latest, err := (Beads{}).Show(b.ID)
		if err != nil {
			return err
		}
		if _, ok := cfg.repoName(latest); !ok || latest.ID != b.ID || latest.Status != StatusClosed || cfg.repoFor(latest) != cfg.repoFor(b) {
			return errors.New("bead changed during maintenance; no removal")
		}
		if apply && by != "TJ" {
			if lvl, _ := cfg.beadLevel(latest, autonomyStopped()); lvl < 3 {
				return fmt.Errorf("%w: maintenance autonomy changed during operation", errNotAutonomous)
			}
		}
		b = latest
		return nil
	}
	compute := func(apply bool) (rpcMaintenanceReport, error) {
		if pos[0] == "retain" {
			return rpcRetainEvents(b.ID, limit, apply, recheck)
		}
		return rpcRemoveWorktree(cfg, b, w, apply, rpcCleanupEnv{git: gitRun, merged: rpcCleanupMerged, recheck: recheck})
	}
	// Refusals and no-ops are not actions. Preview under the same lease before
	// charging the autonomy gate; note an outcome only after a real change.
	report, err := compute(false)
	if err != nil {
		return err
	}
	if apply && report.Change {
		if err = recheck(); err != nil {
			return err
		}
		auto, err := cfg.enforce(b, "resolve", by)
		if err != nil {
			return err
		}
		report, err = compute(true)
		if err != nil {
			return err
		}
		if auto != "" && report.Applied {
			if err = (Beads{}).Note(b.ID, auto+" completed offline RPC "+pos[0]+"; sessions/receipts/reservation retained"); err != nil {
				return fmt.Errorf("maintenance completed but outcome note failed: %w", err)
			}
		}
	}
	return json.NewEncoder(out).Encode(report)
}
