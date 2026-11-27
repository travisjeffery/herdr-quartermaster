package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Clean-context shift handoffs (`quartermaster shift <bead>`).
//
// Long multi-round worker sessions accumulate noisy transcripts and context
// bloat ("context anxiety"). A shift replaces an at-rest worker pane with a
// fresh-context worker on the exact same worktree or role directory without
// touching git state (committed or uncommitted changes, branch, or worktree).
//
// Each shift writes a durable structured handoff artifact to
// `<state>/handoffs/<bead>.json` (atomic 0600 inside a 0700 directory),
// mirrors a concise `HANDOFF:` note on the bead, enforces Class 3 autonomy
// (matching dispatch/resume), cleanly stops the current at-rest worker pane
// (refusing mid-turn workers unless `--force`), and briefs the fresh-context
// replacement with the handoff artifact.

const (
	shiftSchemaVersion  = 1
	journalShift        = "shift"
	shiftStopWait       = 5 * time.Second
	shiftStopPoll       = 250 * time.Millisecond
	shiftMaxDirtyFiles  = 50
	shiftMaxCommits     = 5
	shiftMaxMilestones  = 8
	shiftMaxRecentNotes = 10
	shiftMaxHistory     = 10
)

var (
	errShift           = errors.New("shift")
	errShiftActiveTurn = errors.New("worker is actively mid-turn")
)

// ShiftOpts configures `quartermaster shift <bead>`.
type ShiftOpts struct {
	Reason   string
	NextStep string
	Agent    string
	By       string
	Force    bool
	Focus    bool
	JSON     bool
}

// ShiftPRSummary captures the bead's PR status at handoff time.
type ShiftPRSummary struct {
	Number         int    `json:"number,omitempty"`
	URL            string `json:"url"`
	State          string `json:"state,omitempty"`
	ReviewDecision string `json:"review_decision,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
}

// ShiftReviewSummary captures the bead's bounded review-loop state at handoff time.
type ShiftReviewSummary struct {
	Status       string   `json:"status,omitempty"`
	CurrentRound int      `json:"current_round,omitempty"`
	MaxRounds    int      `json:"max_rounds,omitempty"`
	LastHeadSHA  string   `json:"last_head_sha,omitempty"`
	LastVerdict  string   `json:"last_verdict,omitempty"`
	ReviewerBead string   `json:"reviewer_bead,omitempty"`
	LastSummary  string   `json:"last_summary,omitempty"`
	Findings     []string `json:"findings,omitempty"`
}

// ShiftMilestoneEntry is one recent structured milestone included in the handoff.
type ShiftMilestoneEntry struct {
	At      time.Time `json:"at"`
	Phase   string    `json:"phase"`
	Status  string    `json:"status"`
	Summary string    `json:"summary"`
	PRURL   string    `json:"pr_url,omitempty"`
	SHA     string    `json:"sha,omitempty"`
}

// ShiftHistoryEntry records a compact summary of an earlier shift on the same bead.
type ShiftHistoryEntry struct {
	ShiftNumber   int       `json:"shift_number"`
	CreatedAt     time.Time `json:"created_at"`
	By            string    `json:"by"`
	Reason        string    `json:"reason,omitempty"`
	HeadSHA       string    `json:"head_sha,omitempty"`
	PreviousAgent string    `json:"previous_agent,omitempty"`
	NextAgent     string    `json:"next_agent,omitempty"`
}

// ShiftHandoff is the durable structured handoff artifact stored at
// <state>/handoffs/<bead>.json (mode 0600).
type ShiftHandoff struct {
	Version         int                   `json:"version"`
	Bead            string                `json:"bead"`
	Title           string                `json:"title"`
	Role            string                `json:"role"`
	ShiftNumber     int                   `json:"shift_number"`
	CreatedAt       time.Time             `json:"created_at"`
	By              string                `json:"by"`
	Reason          string                `json:"reason,omitempty"`
	NextStep        string                `json:"next_step,omitempty"`
	Host            string                `json:"host,omitempty"`
	Worktree        bool                  `json:"worktree"`
	Workdir         string                `json:"workdir"`
	RepoRef         string                `json:"repo_ref,omitempty"`
	Branch          string                `json:"branch,omitempty"`
	HeadSHA         string                `json:"head_sha,omitempty"`
	Dirty           bool                  `json:"dirty"`
	DirtyCount      int                   `json:"dirty_count,omitempty"`
	DirtyFiles      []string              `json:"dirty_files,omitempty"`
	RecentCommits   []string              `json:"recent_commits,omitempty"`
	PreviousAgent   string                `json:"previous_agent,omitempty"`
	PreviousPaneID  string                `json:"previous_pane_id,omitempty"`
	PreviousSession string                `json:"previous_session,omitempty"`
	NextAgent       string                `json:"next_agent,omitempty"`
	NextPaneID      string                `json:"next_pane_id,omitempty"`
	NextSessionID   string                `json:"next_session_id,omitempty"`
	PR              *ShiftPRSummary       `json:"pr,omitempty"`
	Review          *ShiftReviewSummary   `json:"review,omitempty"`
	Milestones      []ShiftMilestoneEntry `json:"milestones,omitempty"`
	Acceptance      []string              `json:"acceptance,omitempty"`
	RecentNotes     []string              `json:"recent_notes,omitempty"`
	PriorHandoffs   []ShiftHistoryEntry   `json:"prior_handoffs,omitempty"`
}

// ShiftResult is returned by shiftWorker and emitted by `quartermaster shift --json`.
type ShiftResult struct {
	Handoff      ShiftHandoff `json:"handoff"`
	ArtifactPath string       `json:"artifact_path"`
	Note         string       `json:"note"`
	AgentKind    string       `json:"agent_kind"`
	PaneID       string       `json:"pane_id"`
	Host         string       `json:"host"`
	QueuedBrief  bool         `json:"queued_brief,omitempty"`
	Message      string       `json:"message"`
}

type shiftGitInfo struct {
	Branch        string
	HeadSHA       string
	Dirty         bool
	DirtyCount    int
	DirtyFiles    []string
	RecentCommits []string
}

type shiftEnv struct {
	show            func(id string) (Bead, error)
	note            func(id, text string) error
	enforce         func(b Bead, by string) (string, error)
	agents          func(host string) ([]Agent, error)
	worktrees       func(host, repo string) ([]Worktree, error)
	workspaces      func(host string) ([]Workspace, error)
	panes           func(host, workspaceID string) ([]Pane, error)
	promptPane      func(host, target, text string) error
	closePane       func(host, paneID string) error
	splitPane       func(host, paneID, cwd string, focus bool) (string, error)
	openWorktree    func(host, repo, branch, label string, focus bool) (Opened, error)
	createWorkspace func(host, cwd, label string, focus bool) (Opened, error)
	renameTab       func(host, tabID, label string) error
	runPane         func(host, paneID, cmd string) error
	startAgent      func(host, name, kind, paneID string, args ...string) error
	notify          func(title, body string) error
	liveAsk         func(host string, a *Agent) bool
	piLaunch        func(host string, args []string, role, id string) ([]string, error)
	inspectGit      func(host, dir string) (shiftGitInfo, error)
	loadPR          func(id string) (PR, bool)
	loadReview      func(id string) (ReviewLoopState, bool, error)
	loadMilestones  func(id string) ([]MilestoneRecord, error)
	journal         func(journalRecord) error
	now             func() time.Time
	sleep           func(time.Duration)
	stopWait        time.Duration
	stopPoll        time.Duration
	callerPaneID    string
}

// PaneClose closes one Herdr terminal pane without removing any git worktree or
// branch on disk.
func (h Herdr) PaneClose(pane string) error {
	_, err := h.call(callTimeout, "pane", "close", pane)
	return err
}

// PaneSplit creates a new pane beside pane rooted at cwd and returns its pane ID.
func (h Herdr) PaneSplit(pane, cwd string, focus bool) (string, error) {
	args := []string{"pane", "split", "--pane", pane, "--direction", "right"}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	args = append(args, focusFlag(focus))
	res, err := h.call(callTimeout, args...)
	if err != nil {
		return "", err
	}
	var out struct {
		Pane struct {
			ID string `json:"pane_id"`
		} `json:"pane"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	if out.Pane.ID == "" {
		return "", errors.New("herdr pane split returned no pane_id")
	}
	return out.Pane.ID, nil
}

func liveShiftEnv(cfg Config, h Herdr) shiftEnv {
	hh := h.on(cfg.coordSocket())
	bd := Beads{}
	return shiftEnv{
		show: bd.Show,
		note: bd.Note,
		enforce: func(b Bead, by string) (string, error) {
			prefix, _, err := cfg.enforceAtClass(b, "shift", "shift", classHousekeeping, by)
			return prefix, err
		},
		agents: func(host string) ([]Agent, error) {
			return cfg.herdrOn(hh, host).Agents()
		},
		worktrees: func(host, repo string) ([]Worktree, error) {
			return cfg.herdrOn(hh, host).Worktrees(repo)
		},
		workspaces: func(host string) ([]Workspace, error) {
			return cfg.herdrOn(hh, host).Workspaces()
		},
		panes: func(host, workspaceID string) ([]Pane, error) {
			return cfg.herdrOn(hh, host).Panes(workspaceID)
		},
		promptPane: func(host, target, text string) error {
			return cfg.herdrOn(hh, host).Prompt(target, text)
		},
		closePane: func(host, paneID string) error {
			return cfg.herdrOn(hh, host).PaneClose(paneID)
		},
		splitPane: func(host, paneID, cwd string, focus bool) (string, error) {
			return cfg.herdrOn(hh, host).PaneSplit(paneID, cwd, focus)
		},
		openWorktree: func(host, repo, branch, label string, focus bool) (Opened, error) {
			return cfg.herdrOn(hh, host).WorktreeOpen(repo, branch, label, focus)
		},
		createWorkspace: func(host, cwd, label string, focus bool) (Opened, error) {
			return cfg.herdrOn(hh, host).WorkspaceCreate(cwd, label, focus)
		},
		renameTab: func(host, tabID, label string) error {
			return cfg.herdrOn(hh, host).TabRename(tabID, label)
		},
		runPane: func(host, paneID, cmd string) error {
			return cfg.herdrOn(hh, host).Run(paneID, cmd)
		},
		startAgent: func(host, name, kind, paneID string, args ...string) error {
			return cfg.herdrOn(hh, host).AgentStart(name, kind, paneID, args...)
		},
		notify: hh.Notify,
		liveAsk: func(host string, a *Agent) bool {
			ask, ok, err := piAskOf(host, a)
			return ok && err == nil && ask != nil
		},
		piLaunch: cfg.piLaunch,
		inspectGit: func(host, dir string) (shiftGitInfo, error) {
			return liveInspectShiftGit(cfg, host, dir)
		},
		loadPR: func(id string) (PR, bool) {
			pr, ok := loadState().PRs[id]
			return pr, ok
		},
		loadReview:     loadReviewLoop,
		loadMilestones: readMilestones,
		journal:        appendJournal,
		now:            time.Now,
		sleep:          time.Sleep,
		stopWait:       shiftStopWait,
		stopPoll:       shiftStopPoll,
		callerPaneID:   os.Getenv("HERDR_PANE_ID"),
	}
}

func (e shiftEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func handoffsDir() string { return filepath.Join(stateDir(), "handoffs") }

func handoffPath(id string) (string, error) {
	if !beadName.MatchString(id) {
		return "", fmt.Errorf("%w: invalid bead id %q", errShift, id)
	}
	return filepath.Join(handoffsDir(), id+".json"), nil
}

func lockShift(id string) (func(), error) {
	if !beadName.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid bead id %q", errShift, id)
	}
	dir := filepath.Join(runDir(), "locks")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "shift-"+id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: shift for %s is already in progress", errBusy, id)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func loadShiftHandoff(id string) (ShiftHandoff, bool, error) {
	p, err := handoffPath(id)
	if err != nil {
		return ShiftHandoff{}, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ShiftHandoff{}, false, nil
	}
	if err != nil {
		return ShiftHandoff{}, false, err
	}
	var h ShiftHandoff
	if err := json.Unmarshal(b, &h); err != nil {
		return ShiftHandoff{}, false, fmt.Errorf("%w: parse %s: %v", errShift, p, err)
	}
	if h.Version == 0 {
		h.Version = shiftSchemaVersion
	}
	return h, true, nil
}

func saveShiftHandoff(h ShiftHandoff) (string, error) {
	p, err := handoffPath(h.Bead)
	if err != nil {
		return "", err
	}
	if h.Version == 0 {
		h.Version = shiftSchemaVersion
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(p)
	if err := ensurePrivateDir(dir); err != nil {
		return "", err
	}
	if err := writeAtomicMode(p, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return p, nil
}

func liveInspectShiftGit(cfg Config, host, dir string) (shiftGitInfo, error) {
	runGit := func(args ...string) ([]byte, error) {
		argv := append([]string{"-C", dir}, args...)
		if host != "" && host != localHost {
			return cfg.hostExec(host, append([]string{"git"}, argv...)...).Output()
		}
		return exec.Command("git", argv...).Output()
	}
	var info shiftGitInfo
	if out, err := runGit("rev-parse", "HEAD"); err == nil {
		info.HeadSHA = strings.TrimSpace(string(out))
	} else {
		return info, fmt.Errorf("git rev-parse HEAD in %s: %w", dir, err)
	}
	if out, err := runGit("rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if br := strings.TrimSpace(string(out)); br != "" && br != "HEAD" {
			info.Branch = br
		}
	}
	if out, err := runGit("status", "--porcelain=v1"); err == nil {
		for _, raw := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			info.Dirty = true
			info.DirtyCount++
			if len(info.DirtyFiles) < shiftMaxDirtyFiles {
				info.DirtyFiles = append(info.DirtyFiles, raw)
			}
		}
	} else {
		return info, fmt.Errorf("git status in %s: %w", dir, err)
	}
	if out, err := runGit("log", fmt.Sprintf("-n%d", shiftMaxCommits), "--oneline"); err == nil {
		for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line := strings.TrimSpace(raw); line != "" {
				info.RecentCommits = append(info.RecentCommits, line)
			}
		}
	}
	return info, nil
}

// shiftExitCommand returns the interactive slash command that exits the given
// worker CLI cleanly without killing its parent pane abruptly.
func shiftExitCommand(kind string) string {
	if kind == "pi" {
		return "/quit"
	}
	return "/exit"
}

// freshPiShiftSessionID returns a fresh pi session id for shiftNum that does
// not collide with the bead's original session or an already-existing shift
// session file on disk.
func freshPiShiftSessionID(beadID string, shiftNum int, now time.Time) string {
	cand := fmt.Sprintf("%s-shift-%d", beadID, shiftNum)
	if piSessionByID(cand) == "" {
		return cand
	}
	return fmt.Sprintf("%s-%s", cand, now.UTC().Format("20060102t150405"))
}

func extractRecentShiftNotes(notes string, maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	var lines []string
	for _, raw := range strings.Split(notes, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "HANDOFF:") {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

func summarizeShiftReview(st ReviewLoopState) *ShiftReviewSummary {
	s := &ShiftReviewSummary{
		Status:       st.State,
		CurrentRound: st.CurrentRound,
		MaxRounds:    st.MaxRounds,
		LastHeadSHA:  st.LastHeadSHA,
		ReviewerBead: st.ReviewerBead,
	}
	if len(st.Rounds) > 0 {
		last := st.Rounds[len(st.Rounds)-1]
		s.LastVerdict = last.Verdict
		s.LastSummary = last.Summary
		for _, f := range last.Findings {
			s.Findings = append(s.Findings, f.String())
		}
	}
	return s
}

func formatHandoffNote(h ShiftHandoff, artifactPath string) string {
	var parts []string
	agentPart := h.NextAgent
	if h.PreviousAgent != "" && h.NextAgent != "" && h.PreviousAgent != h.NextAgent {
		agentPart = h.PreviousAgent + " -> " + h.NextAgent
	} else if agentPart == "" {
		agentPart = h.PreviousAgent
	}
	head := fmt.Sprintf("HANDOFF: shift #%d", h.ShiftNumber)
	if agentPart != "" {
		head += fmt.Sprintf(" (%s)", agentPart)
	}
	parts = append(parts, head)

	var loc []string
	if h.Workdir != "" {
		loc = append(loc, "path="+h.Workdir)
	}
	if h.Branch != "" {
		loc = append(loc, "branch="+h.Branch)
	}
	if h.HeadSHA != "" {
		loc = append(loc, "head="+short(h.HeadSHA))
	}
	if h.Worktree {
		if h.Dirty {
			loc = append(loc, fmt.Sprintf("dirty=%d file(s)", h.DirtyCount))
		} else {
			loc = append(loc, "clean")
		}
	} else {
		loc = append(loc, "role="+h.Role)
	}
	parts = append(parts, strings.Join(loc, " "))

	if h.PR != nil {
		prStr := h.PR.URL
		if h.PR.Number > 0 {
			prStr = fmt.Sprintf("PR #%d", h.PR.Number)
		}
		if h.PR.State != "" {
			prStr += " " + h.PR.State
		}
		if h.PR.ReviewDecision != "" {
			prStr += " (" + h.PR.ReviewDecision + ")"
		}
		parts = append(parts, prStr)
	}
	if h.Review != nil && h.Review.Status != "" {
		revStr := fmt.Sprintf("review=%s (%d/%d)", h.Review.Status, h.Review.CurrentRound, h.Review.MaxRounds)
		if h.Review.LastVerdict != "" {
			revStr += " " + h.Review.LastVerdict
		}
		parts = append(parts, revStr)
	}
	if len(h.Milestones) > 0 {
		last := h.Milestones[len(h.Milestones)-1]
		parts = append(parts, fmt.Sprintf("milestones=%d (latest: %s/%s: %s)", len(h.Milestones), last.Phase, last.Status, shortTitle(last.Summary, 60)))
	}
	if len(h.Acceptance) > 0 {
		parts = append(parts, fmt.Sprintf("acceptance=%d item(s)", len(h.Acceptance)))
	}
	if h.Reason != "" {
		parts = append(parts, "reason: "+shortTitle(h.Reason, 80))
	}
	if h.NextStep != "" {
		parts = append(parts, "next: "+shortTitle(h.NextStep, 80))
	}
	if artifactPath != "" {
		parts = append(parts, "artifact="+artifactPath)
	}
	return strings.Join(parts, " | ")
}

func buildShiftBrief(b Bead, role EffectiveRole, h ShiftHandoff, artifactPath, linear, globalInstr, roleInstr string) string {
	base := briefForRole(b, role, h.Branch, h.Workdir, h.RepoRef, linear, globalInstr, roleInstr)
	var s strings.Builder
	s.WriteString(base)
	fmt.Fprintf(&s, "\n## Shift Handoff (Shift #%d — Fresh Context)\n\n", h.ShiftNumber)
	s.WriteString("You are starting a fresh-context shift replacing an earlier worker session on this exact workspace. ")
	s.WriteString("This handoff is task context, not a new approval; verify files, git state, and `bd show " + b.ID + "` before acting.\n\n")
	fmt.Fprintf(&s, "- **Handoff artifact:** `%s`\n", artifactPath)
	fmt.Fprintf(&s, "- **Working directory:** `%s`", h.Workdir)
	if h.Worktree {
		s.WriteString(" (existing git worktree preserved intact — do NOT discard committed or uncommitted changes)\n")
		if h.Branch != "" {
			fmt.Fprintf(&s, "- **Branch:** `%s`\n", h.Branch)
		}
		if h.HeadSHA != "" {
			fmt.Fprintf(&s, "- **HEAD SHA:** `%s`\n", h.HeadSHA)
		}
		if h.Dirty {
			fmt.Fprintf(&s, "- **Working tree:** dirty (%d modified/untracked path(s); inspect `git status` and `git diff` first)\n", h.DirtyCount)
		} else {
			s.WriteString("- **Working tree:** clean\n")
		}
	} else {
		fmt.Fprintf(&s, " (isolated role directory for role `%s` preserved intact)\n", h.Role)
	}
	if h.Reason != "" {
		fmt.Fprintf(&s, "- **Shift reason:** %s\n", h.Reason)
	}
	if h.NextStep != "" {
		fmt.Fprintf(&s, "- **Requested next step:** %s\n", h.NextStep)
	}
	if len(h.DirtyFiles) > 0 {
		s.WriteString("\n### Uncommitted paths at handoff\n")
		for _, f := range h.DirtyFiles {
			fmt.Fprintf(&s, "- `%s`\n", f)
		}
		if h.DirtyCount > len(h.DirtyFiles) {
			fmt.Fprintf(&s, "- ... and %d more path(s)\n", h.DirtyCount-len(h.DirtyFiles))
		}
	}
	if len(h.RecentCommits) > 0 {
		s.WriteString("\n### Recent commits\n")
		for _, c := range h.RecentCommits {
			fmt.Fprintf(&s, "- %s\n", c)
		}
	}
	if h.PR != nil {
		s.WriteString("\n### Pull request\n")
		fmt.Fprintf(&s, "- %s", h.PR.URL)
		if h.PR.State != "" {
			fmt.Fprintf(&s, " (%s", h.PR.State)
			if h.PR.ReviewDecision != "" {
				fmt.Fprintf(&s, ", %s", h.PR.ReviewDecision)
			}
			s.WriteString(")")
		}
		if h.PR.HeadSHA != "" {
			fmt.Fprintf(&s, " head=%s", short(h.PR.HeadSHA))
		}
		s.WriteString("\n")
	}
	if h.Review != nil && h.Review.Status != "" {
		s.WriteString("\n### Review loop state\n")
		fmt.Fprintf(&s, "- Status: %s (round %d/%d", h.Review.Status, h.Review.CurrentRound, h.Review.MaxRounds)
		if h.Review.LastVerdict != "" {
			fmt.Fprintf(&s, ", last verdict: %s", h.Review.LastVerdict)
		}
		if h.Review.LastHeadSHA != "" {
			fmt.Fprintf(&s, ", head: %s", short(h.Review.LastHeadSHA))
		}
		s.WriteString(")\n")
		if h.Review.LastSummary != "" {
			fmt.Fprintf(&s, "- Latest summary: %s\n", h.Review.LastSummary)
		}
		for _, f := range h.Review.Findings {
			fmt.Fprintf(&s, "- Blocking finding: %s\n", f)
		}
	}
	if len(h.Milestones) > 0 {
		s.WriteString("\n### Recent milestones\n")
		for _, m := range h.Milestones {
			fmt.Fprintf(&s, "- [%s] %s (%s): %s\n", m.At.UTC().Format("2006-01-02 15:04Z"), m.Phase, m.Status, m.Summary)
		}
	}
	if len(h.Acceptance) > 0 {
		s.WriteString("\n### Acceptance checklist\n")
		for _, a := range h.Acceptance {
			fmt.Fprintf(&s, "- [ ] %s\n", a)
		}
	}
	if len(h.RecentNotes) > 0 {
		s.WriteString("\n### Recent bead notes\n")
		for _, n := range h.RecentNotes {
			fmt.Fprintf(&s, "- %s\n", shortTitle(n, 200))
		}
	}
	return s.String()
}

// shiftWorker performs a clean-context shift handoff on bead id: validates
// preconditions and at-rest status, enforces Class 3 autonomy, writes a durable
// handoff artifact under <state>/handoffs/<bead>.json (0600) plus a concise
// HANDOFF: note, stops the existing worker pane without touching git state,
// and spawns a fresh-context worker on the exact same worktree or role dir.
func shiftWorker(cfg Config, env shiftEnv, id string, opts ShiftOpts) (ShiftResult, error) {
	id = strings.TrimSpace(id)
	if !beadName.MatchString(id) {
		return ShiftResult{}, fmt.Errorf("%w: invalid bead id %q", errShift, id)
	}
	if rpcExists(id) {
		return ShiftResult{}, fmt.Errorf("%w: %s belongs to pi-rpc; quartermaster shift supports Herdr pane workers (use quartermaster rpc for native RPC workers)", errShift, id)
	}
	if opts.Agent != "" && opts.Agent != "auto" && !validKinds[opts.Agent] {
		return ShiftResult{}, fmt.Errorf("%w: unknown agent kind %q (%s)", errShift, opts.Agent, kindList("auto"))
	}

	unlock, err := lockShift(id)
	if err != nil {
		return ShiftResult{}, err
	}
	defer unlock()

	if env.show == nil {
		return ShiftResult{}, fmt.Errorf("%w: bead lookup unavailable", errShift)
	}
	b, err := env.show(id)
	if err != nil {
		return ShiftResult{}, err
	}
	if b.Status == StatusClosed {
		return ShiftResult{}, fmt.Errorf("%w: %s is closed", errShift, id)
	}
	role, err := cfg.resolveRoleForBead(b, "")
	if err != nil {
		return ShiftResult{}, err
	}

	host := beadHost(b)
	if host == "" {
		host = localHost
	}
	if !role.Worktree && host != localHost {
		return ShiftResult{}, fmt.Errorf("%w: role %q has worktree = false and runs on %s host only", errShift, role.Name, localHost)
	}

	name := agentName(id)
	var curAgent *Agent
	if env.agents != nil {
		list, err := env.agents(host)
		if err != nil {
			return ShiftResult{}, fmt.Errorf("%w: list agents%s: %v", errShift, onHost(host), err)
		}
		for i := range list {
			if list[i].Name == name {
				cp := list[i]
				curAgent = &cp
				break
			}
		}
	}

	// Guard against interrupting an active mid-turn or blocked worker unless --force.
	if curAgent != nil && !opts.Force {
		if curAgent.PaneID != "" && env.callerPaneID != "" && curAgent.PaneID == env.callerPaneID {
			return ShiftResult{}, fmt.Errorf("%w: refusing to shift the caller's own active pane %s for %s (pass --force to override)", errShiftActiveTurn, curAgent.PaneID, id)
		}
		if !agentReady(curAgent) {
			return ShiftResult{}, fmt.Errorf("%w: %s worker %s (%s) is currently %s; wait until idle/done or pass --force", errShiftActiveTurn, id, curAgent.Name, curAgent.PaneID, curAgent.Status)
		}
		if env.liveAsk != nil && env.liveAsk(host, curAgent) {
			return ShiftResult{}, fmt.Errorf("%w: %s worker %s (%s) is waiting on an active approval prompt; resolve it or pass --force", errShiftActiveTurn, id, curAgent.Name, curAgent.PaneID)
		}
	}

	// Resolve existing worktree or role directory strictly read-only.
	var (
		workdir       string
		branch        string
		repo          string
		repoRef       string
		wtWorkspaceID string
		gitInfo       shiftGitInfo
	)
	var knownPR PR
	hasPR := false
	if env.loadPR != nil {
		knownPR, hasPR = env.loadPR(id)
	}
	prsMap := map[string]PR{}
	if hasPR {
		prsMap[id] = knownPR
	}

	if role.Worktree {
		var ok bool
		repo, ok = cfg.repoOn(host, b)
		if !ok || repo == "" {
			return ShiftResult{}, fmt.Errorf("%w: repo for %s is not configured%s", errShift, id, onHost(host))
		}
		if env.worktrees == nil {
			return ShiftResult{}, fmt.Errorf("%w: worktree discovery unavailable", errShift)
		}
		wts, err := env.worktrees(host, repo)
		if err != nil {
			return ShiftResult{}, fmt.Errorf("%w: list worktrees for %s%s: %v", errShift, id, onHost(host), err)
		}
		wt, found := beadWorktree(cfg, b, prsMap, wts)
		if !found {
			for _, w := range wts {
				if w.Linked && onBeadBranch(cfg.BranchPrefix, w.Branch, b) {
					wt, found = w, true
					break
				}
			}
		}
		if !found && curAgent != nil && curAgent.Cwd != "" {
			cleanCwd := filepath.Clean(curAgent.Cwd)
			for _, w := range wts {
				if w.Linked && filepath.Clean(w.Path) == cleanCwd {
					wt, found = w, true
					break
				}
			}
		}
		if !found {
			return ShiftResult{}, fmt.Errorf("%w: no existing linked worktree for %s%s; use `quartermaster dispatch %s`", errShift, id, onHost(host), id)
		}
		if filepath.Clean(wt.Path) == filepath.Clean(repo) {
			return ShiftResult{}, fmt.Errorf("%w: refusing to shift in main checkout %s", errShift, wt.Path)
		}
		workdir = wt.Path
		branch = wt.Branch
		wtWorkspaceID = wt.WorkspaceID
		if env.inspectGit != nil {
			gitInfo, err = env.inspectGit(host, workdir)
			if err != nil {
				return ShiftResult{}, fmt.Errorf("%w: inspect git state in %s: %v", errShift, workdir, err)
			}
			if gitInfo.Branch != "" {
				branch = gitInfo.Branch
			}
		}
	} else {
		if dir, ok := hasRoleDir(id); ok {
			workdir = dir
		} else if curAgent != nil && curAgent.Cwd != "" {
			expected, derr := roleDir(id)
			if derr == nil && filepath.Clean(curAgent.Cwd) == filepath.Clean(expected) {
				workdir = curAgent.Cwd
			}
		}
		if workdir == "" {
			return ShiftResult{}, fmt.Errorf("%w: no existing role directory for %s; use `quartermaster dispatch %s --role %s`", errShift, id, id, role.Name)
		}
		if rn, ok := cfg.repoName(b); ok {
			if rn != "" {
				repoRef = cfg.Repos[rn]
			} else {
				repoRef = cfg.Repo
			}
		}
	}

	// Determine incoming fresh-context agent kind and validate launch args
	// BEFORE stopping the old worker or mutating any state.
	kind := opts.Agent
	if kind == "" && curAgent != nil && validKinds[curAgent.Kind] {
		kind = curAgent.Kind
	}
	if kind == "" {
		kind = resumeKind(cfg, b, "")
	}
	if kind == "auto" {
		kind = autoAgent(env.clock())
	}
	if !validKinds[kind] {
		return ShiftResult{}, fmt.Errorf("%w: unknown agent kind %q (%s)", errShift, kind, kindList("auto"))
	}
	if kind == "pi" && role.Thinking != "" && !isValidPiThinkingLevel(role.Thinking) {
		return ShiftResult{}, fmt.Errorf("role %q: invalid pi thinking level %q (want %s)", role.Name, role.Thinking, strings.Join(piThinkingLevels, ", "))
	}
	roleInstr, err := loadRoleInstructions(role)
	if err != nil {
		return ShiftResult{}, err
	}

	now := env.clock().UTC()
	prevHandoff, hasPrev, err := loadShiftHandoff(id)
	if err != nil {
		return ShiftResult{}, err
	}
	shiftNumber := 1
	var history []ShiftHistoryEntry
	if hasPrev {
		shiftNumber = prevHandoff.ShiftNumber + 1
		history = append(history, prevHandoff.PriorHandoffs...)
		history = append(history, ShiftHistoryEntry{
			ShiftNumber:   prevHandoff.ShiftNumber,
			CreatedAt:     prevHandoff.CreatedAt,
			By:            prevHandoff.By,
			Reason:        prevHandoff.Reason,
			HeadSHA:       prevHandoff.HeadSHA,
			PreviousAgent: prevHandoff.PreviousAgent,
			NextAgent:     prevHandoff.NextAgent,
		})
		if len(history) > shiftMaxHistory {
			history = history[len(history)-shiftMaxHistory:]
		}
	}

	// Build fresh-context worker args (deliberately omitting resumeArgs).
	args := cfg.workerArgs(kind, cfg.launchBypass(b, host), role)
	nextSessionID := ""
	if kind == "pi" {
		if env.piLaunch != nil {
			if args, err = env.piLaunch(host, args, "worker", id); err != nil {
				return ShiftResult{}, err
			}
		}
		nextSessionID = freshPiShiftSessionID(id, shiftNumber, now)
		args = replaceFlagValue(args, []string{"--session-id"}, []string{"--session-id", nextSessionID})
	}

	// Enforce Class 3 autonomy BEFORE writing artifacts, adding notes, or
	// stopping the live worker.
	by := strings.TrimSpace(opts.By)
	if by == "" {
		by = "coordinator"
	}
	var autoPrefix string
	if env.enforce != nil {
		autoPrefix, err = env.enforce(b, by)
		if err != nil {
			return ShiftResult{}, err
		}
	}

	// Assemble durable handoff artifact.
	handoff := ShiftHandoff{
		Version:       shiftSchemaVersion,
		Bead:          b.ID,
		Title:         b.Title,
		Role:          role.Name,
		ShiftNumber:   shiftNumber,
		CreatedAt:     now,
		By:            by,
		Reason:        strings.TrimSpace(opts.Reason),
		NextStep:      strings.TrimSpace(opts.NextStep),
		Host:          host,
		Worktree:      role.Worktree,
		Workdir:       workdir,
		RepoRef:       repoRef,
		Branch:        branch,
		HeadSHA:       gitInfo.HeadSHA,
		Dirty:         gitInfo.Dirty,
		DirtyCount:    gitInfo.DirtyCount,
		DirtyFiles:    gitInfo.DirtyFiles,
		RecentCommits: gitInfo.RecentCommits,
		NextAgent:     kind,
		NextSessionID: nextSessionID,
		Acceptance:    acceptanceCriteria(b.Description),
		RecentNotes:   extractRecentShiftNotes(b.Notes, shiftMaxRecentNotes),
		PriorHandoffs: history,
	}
	if curAgent != nil {
		handoff.PreviousAgent = curAgent.Kind
		handoff.PreviousPaneID = curAgent.PaneID
		handoff.PreviousSession = curAgent.Session.Value
	}
	if hasPR && knownPR.URL != "" {
		handoff.PR = &ShiftPRSummary{
			Number:         knownPR.Number,
			URL:            knownPR.URL,
			State:          knownPR.State,
			ReviewDecision: knownPR.ReviewDecision,
			HeadSHA:        knownPR.HeadSHA,
		}
	} else if noted := deliveryNotedPRs(b.Notes); len(noted) > 0 {
		handoff.PR = &ShiftPRSummary{URL: noted[len(noted)-1]}
	}
	if env.loadReview != nil {
		if rloop, ok, rerr := env.loadReview(id); rerr == nil && ok {
			handoff.Review = summarizeShiftReview(rloop)
		}
	}
	if env.loadMilestones != nil {
		if ms, merr := env.loadMilestones(id); merr == nil && len(ms) > 0 {
			if len(ms) > shiftMaxMilestones {
				ms = ms[len(ms)-shiftMaxMilestones:]
			}
			for _, m := range ms {
				entry := ShiftMilestoneEntry{
					At:      m.At,
					Phase:   m.Phase,
					Status:  m.Status,
					Summary: m.Summary,
				}
				if m.Evidence != nil {
					entry.PRURL = m.Evidence.PRURL
					entry.SHA = m.Evidence.SHA
				}
				handoff.Milestones = append(handoff.Milestones, entry)
			}
		}
	}

	artifactPath, err := saveShiftHandoff(handoff)
	if err != nil {
		return ShiftResult{}, fmt.Errorf("%w: save handoff artifact: %v", errShift, err)
	}

	handoffNote := formatHandoffNote(handoff, artifactPath)
	if env.note != nil {
		if err := env.note(id, handoffNote); err != nil {
			return ShiftResult{}, fmt.Errorf("%w: record HANDOFF note on %s: %v", errShift, id, err)
		}
		if autoPrefix != "" {
			_ = env.note(id, fmt.Sprintf("%s shifted %s (shift #%d, %s) — %s", autoPrefix, id, shiftNumber, kind, artifactPath))
		}
	}

	// Clear any stale queued outbox briefs/notices for this worker so old
	// prompts never land on the fresh-context replacement.
	_ = clearQueuedOutbox(name)
	_ = os.Remove(noticePath(host, name))

	// Cleanly stop the current worker pane (never removing the worktree or branch).
	prevWorkspaceID := wtWorkspaceID
	if curAgent != nil {
		if curAgent.WorkspaceID != "" {
			prevWorkspaceID = curAgent.WorkspaceID
		}
		if err := stopShiftWorkerPane(env, host, *curAgent); err != nil {
			return ShiftResult{}, fmt.Errorf("%w: stop worker %s (%s): %v", errShift, curAgent.Name, curAgent.PaneID, err)
		}
	}

	// Obtain a fresh pane on the exact same worktree or role directory.
	pane, reusedExistingPane, err := openShiftPane(env, host, role, repo, branch, workdir, name, prevWorkspaceID, curAgent != nil, opts.Focus)
	if err != nil {
		return ShiftResult{}, fmt.Errorf("%w: open fresh pane for %s: %v", errShift, id, err)
	}
	if env.renameTab != nil {
		if idx := strings.LastIndex(pane, ":"); idx > 0 {
			_ = env.renameTab(host, pane[:idx]+":t1", shortTitle(b.Title, 34))
		}
	}

	envCmd := cfg.agentEnvOn(host, kind) + " QUARTERMASTER_BEAD=" + shellQuote(id)
	if role.Name != roleImplementer {
		envCmd += " QUARTERMASTER_ROLE=" + shellQuote(role.Name)
	}
	if host == localHost {
		envCmd += execEnv(id)
	}
	if reusedExistingPane {
		envCmd = "cd " + shellQuote(workdir) + " && " + envCmd
	}
	if env.runPane != nil {
		_ = env.runPane(host, pane, envCmd)
	}

	briefText := buildShiftBrief(b, role, handoff, artifactPath, linearKey(b, cfg.LinearPrefixes), readInstructions(), roleInstr)

	if env.startAgent == nil {
		return ShiftResult{}, fmt.Errorf("%w: agent launcher unavailable", errShift)
	}
	queuedBrief := false
	where := pane + onHost(host)
	if err := env.startAgent(host, name, kind, pane, args...); err != nil {
		if !strings.Contains(err.Error(), "agent_not_ready") {
			return ShiftResult{}, fmt.Errorf("%w: start fresh %s in %s: %v", errShift, kind, where, err)
		}
		if qerr := queueBriefOn(host, name, briefText); qerr != nil {
			return ShiftResult{}, fmt.Errorf("%w: queue shift brief: %v", errShift, qerr)
		}
		if env.notify != nil {
			_ = env.notify("quartermaster: "+id+" is waiting for you", "Answer its startup prompt in "+where+"; the shift handoff brief follows.")
		}
		queuedBrief = true
	} else {
		if env.promptPane == nil {
			return ShiftResult{}, fmt.Errorf("%w: prompt delivery unavailable", errShift)
		}
		if err := env.promptPane(host, name, briefText); err != nil {
			return ShiftResult{}, fmt.Errorf("%w: send shift brief to %s: %v", errShift, name, err)
		}
	}

	handoff.NextPaneID = pane
	if _, serr := saveShiftHandoff(handoff); serr != nil {
		return ShiftResult{}, fmt.Errorf("%w: update handoff artifact with pane %s: %v", errShift, pane, serr)
	}

	msg := fmt.Sprintf("shifted %s to fresh %s (%s, shift #%d, artifact %s)", id, kind, where, shiftNumber, artifactPath)
	if queuedBrief {
		msg += "; at a startup prompt, shift brief queued"
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalShift,
			Bead:   id,
			Class:  classHousekeeping,
			Action: "shift",
			Pass:   true,
			Text:   msg,
		})
	}
	return ShiftResult{
		Handoff:      handoff,
		ArtifactPath: artifactPath,
		Note:         handoffNote,
		AgentKind:    kind,
		PaneID:       pane,
		Host:         host,
		QueuedBrief:  queuedBrief,
		Message:      msg,
	}, nil
}

// stopShiftWorkerPane cleanly stops the current worker pane without touching
// the worktree or branch. When the worker is at rest it first sends the kind's
// graceful exit command and waits briefly for the agent to exit cleanly, then
// closes the old pane.
func stopShiftWorkerPane(env shiftEnv, host string, cur Agent) error {
	agentStillRunning := func() bool {
		if env.agents == nil {
			return false
		}
		list, err := env.agents(host)
		if err != nil {
			return true
		}
		for _, a := range list {
			if a.Name == cur.Name && (cur.PaneID == "" || a.PaneID == cur.PaneID) {
				return true
			}
		}
		return false
	}

	waitStopped := func() {
		wait := env.stopWait
		poll := env.stopPoll
		if poll <= 0 {
			poll = shiftStopPoll
		}
		if wait <= 0 || env.agents == nil || env.sleep == nil {
			return
		}
		deadline := env.clock().Add(wait)
		maxSteps := max(1, int(wait/poll)+1)
		for step := 0; step < maxSteps && agentStillRunning(); step++ {
			if !env.clock().Before(deadline) {
				break
			}
			env.sleep(poll)
		}
	}

	if agentReady(&cur) && env.promptPane != nil && cur.PaneID != "" {
		_ = env.promptPane(host, cur.PaneID, shiftExitCommand(cur.Kind))
		waitStopped()
	}

	if cur.PaneID != "" && env.closePane != nil {
		if err := env.closePane(host, cur.PaneID); err != nil {
			// If the pane already closed on graceful exit and the agent is gone,
			// proceed cleanly; otherwise fail closed.
			if agentStillRunning() {
				return err
			}
		}
	}

	// Verify the old agent is gone so AgentStart never collides on agent name.
	if env.agents != nil && agentStillRunning() {
		waitStopped()
		if agentStillRunning() {
			return fmt.Errorf("agent %s (%s) still running after stop", cur.Name, cur.PaneID)
		}
	}
	return nil
}

// openShiftPane selects or opens a clean pane rooted at workdir for the fresh
// shift worker. It never calls WorktreeCreate or modifies git state.
func openShiftPane(
	env shiftEnv,
	host string,
	role EffectiveRole,
	repo, branch, workdir, name, prevWorkspaceID string,
	stoppedOldPane bool,
	focus bool,
) (paneID string, reusedExisting bool, err error) {
	wsID := prevWorkspaceID
	if !role.Worktree && wsID == "" && env.workspaces != nil {
		if wss, werr := env.workspaces(host); werr == nil {
			for _, ws := range wss {
				if !ws.HasWorktree && ws.Label == name && ws.ID != "" {
					wsID = ws.ID
					break
				}
			}
		}
	}
	if wsID != "" && env.panes != nil {
		if panes, perr := env.panes(host, wsID); perr == nil && len(panes) > 0 {
			if !stoppedOldPane {
				if p, ok := freePane(panes); ok {
					return p, true, nil
				}
			}
			if env.splitPane != nil {
				p, serr := env.splitPane(host, panes[0].ID, workdir, focus)
				if serr == nil && p != "" {
					return p, false, nil
				}
			}
		}
	}
	if role.Worktree && branch != "" {
		if env.openWorktree == nil {
			return "", false, errors.New("worktree opener unavailable")
		}
		opened, err := env.openWorktree(host, repo, branch, name, focus)
		if err != nil {
			return "", false, err
		}
		return opened.PaneID, false, nil
	}
	if env.createWorkspace == nil {
		return "", false, errors.New("workspace creator unavailable")
	}
	opened, err := env.createWorkspace(host, workdir, name, focus)
	if err != nil {
		return "", false, err
	}
	return opened.PaneID, false, nil
}

const shiftUsage = `usage:
  quartermaster shift <bead> [--reason R] [--next STEP] [--agent K] [--force] [--focus] [--by WHO] [--json]
  quartermaster shift show <bead> [--json]`

func shiftCmd(cfg Config, env shiftEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("shift", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason := fs.String("reason", "", "why this shift handoff is happening")
	nextStep := fs.String("next", "", "explicit next step for the fresh-context worker")
	agent := fs.String("agent", "", "agent kind override: claude, codex, muse, pi, agy or auto")
	by := fs.String("by", "coordinator", "who decided (coordinator or TJ)")
	force := fs.Bool("force", false, "shift even if the worker is actively mid-turn or blocked")
	focus := fs.Bool("focus", false, "focus the fresh worker workspace/pane")
	asJSON := fs.Bool("json", false, "emit JSON output")

	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 2 && (pos[0] == "show" || pos[0] == "status") {
		h, ok, err := loadShiftHandoff(pos[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no handoff artifact recorded for %s", errShift, pos[1])
		}
		if *asJSON {
			return writePrettyJSON(out, h)
		}
		p, _ := handoffPath(pos[1])
		_, err = fmt.Fprintln(out, formatHandoffNote(h, p))
		return err
	}
	if len(pos) != 1 {
		return errors.New(shiftUsage)
	}
	res, err := shiftWorker(cfg, env, pos[0], ShiftOpts{
		Reason:   *reason,
		NextStep: *nextStep,
		Agent:    *agent,
		By:       *by,
		Force:    *force,
		Focus:    *focus,
		JSON:     *asJSON,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return writePrettyJSON(out, res)
	}
	_, err = fmt.Fprintln(out, res.Message)
	return err
}
