package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func initTestGitWorktree(t *testing.T) (repoDir, wtDir, branch, headSHA string) {
	t.Helper()
	base := t.TempDir()
	repoDir = filepath.Join(base, "repo")
	wtDir = filepath.Join(base, "wt-task-1")
	branch = "topic/task-1-long-task"

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test Author",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test Author",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", "app.go")
	runGit(repoDir, "commit", "-m", "initial commit")
	runGit(repoDir, "worktree", "add", "-b", branch, wtDir, "main")

	if err := os.WriteFile(filepath.Join(wtDir, "app.go"), []byte("package app\n// committed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(wtDir, "add", "app.go")
	runGit(wtDir, "commit", "-m", "implement first pass")
	headSHA = runGit(wtDir, "rev-parse", "HEAD")

	// Leave both modified tracked and untracked files to prove shift preserves git state.
	if err := os.WriteFile(filepath.Join(wtDir, "app.go"), []byte("package app\n// uncommitted edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "scratch_wip.txt"), []byte("wip notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repoDir, wtDir, branch, headSHA
}

type fakeShiftHarness struct {
	bead        Bead
	agents      []Agent
	worktrees   []Worktree
	workspaces  []Workspace
	panes       map[string][]Pane
	pr          *PR
	review      *ReviewLoopState
	milestones  []MilestoneRecord
	level       int
	liveAsk     bool
	callerPane  string
	startErr    error
	exitOnQuit  bool
	closeLeaves bool

	notes        []string
	prompts      []string
	closedPanes  []string
	splitCalls   []string
	openedWT     []string
	createdWS    []string
	runCmds      []string
	startedCalls []struct {
		Name, Kind, Pane string
		Args             []string
	}
	journal []journalRecord
	now     time.Time
}

func (f *fakeShiftHarness) env(cfg Config) shiftEnv {
	return shiftEnv{
		show: func(id string) (Bead, error) {
			if f.bead.ID == id {
				return f.bead, nil
			}
			return Bead{}, errBeadNotFound
		},
		note: func(id, text string) error {
			f.notes = append(f.notes, text)
			if f.bead.Notes != "" {
				f.bead.Notes += "\n"
			}
			f.bead.Notes += text
			return nil
		},
		enforce: func(b Bead, by string) (string, error) {
			return allow(b.ID, by, "shift", classHousekeeping, f.level, "")
		},
		agents: func(host string) ([]Agent, error) {
			return slices.Clone(f.agents), nil
		},
		worktrees: func(host, repo string) ([]Worktree, error) {
			return slices.Clone(f.worktrees), nil
		},
		workspaces: func(host string) ([]Workspace, error) {
			return slices.Clone(f.workspaces), nil
		},
		panes: func(host, wsID string) ([]Pane, error) {
			return slices.Clone(f.panes[wsID]), nil
		},
		promptPane: func(host, target, text string) error {
			f.prompts = append(f.prompts, target+": "+text)
			if f.exitOnQuit && (text == "/quit" || text == "/exit") {
				f.agents = slices.DeleteFunc(f.agents, func(a Agent) bool { return a.PaneID == target })
			}
			return nil
		},
		closePane: func(host, paneID string) error {
			f.closedPanes = append(f.closedPanes, paneID)
			if !f.closeLeaves {
				f.agents = slices.DeleteFunc(f.agents, func(a Agent) bool { return a.PaneID == paneID })
				for wsID, ps := range f.panes {
					f.panes[wsID] = slices.DeleteFunc(ps, func(p Pane) bool { return p.ID == paneID })
				}
			}
			return nil
		},
		splitPane: func(host, paneID, cwd string, focus bool) (string, error) {
			f.splitCalls = append(f.splitCalls, paneID+"@"+cwd)
			return "w9:p2", nil
		},
		openWorktree: func(host, repo, branch, label string, focus bool) (Opened, error) {
			f.openedWT = append(f.openedWT, branch)
			return Opened{WorkspaceID: "w10", PaneID: "w10:p1", Path: f.worktrees[0].Path}, nil
		},
		createWorkspace: func(host, cwd, label string, focus bool) (Opened, error) {
			f.createdWS = append(f.createdWS, cwd)
			return Opened{WorkspaceID: "w11", PaneID: "w11:p1", Path: cwd}, nil
		},
		renameTab: func(host, tabID, label string) error { return nil },
		runPane: func(host, paneID, cmd string) error {
			f.runCmds = append(f.runCmds, paneID+": "+cmd)
			return nil
		},
		startAgent: func(host, name, kind, paneID string, args ...string) error {
			f.startedCalls = append(f.startedCalls, struct {
				Name, Kind, Pane string
				Args             []string
			}{Name: name, Kind: kind, Pane: paneID, Args: slices.Clone(args)})
			if f.startErr != nil {
				return f.startErr
			}
			f.agents = append(f.agents, Agent{Name: name, Kind: kind, Status: "idle", PaneID: paneID})
			return nil
		},
		notify: func(title, body string) error { return nil },
		liveAsk: func(host string, a *Agent) bool {
			return f.liveAsk
		},
		piLaunch: func(host string, args []string, role, id string) ([]string, error) {
			return append(slices.Clone(args), "--quartermaster-control", "/tmp/ctrl.sock", "--session-id", id), nil
		},
		inspectGit: func(host, dir string) (shiftGitInfo, error) {
			return liveInspectShiftGit(cfg, host, dir)
		},
		loadPR: func(id string) (PR, bool) {
			if f.pr == nil {
				return PR{}, false
			}
			return *f.pr, true
		},
		loadReview: func(id string) (ReviewLoopState, bool, error) {
			if f.review == nil {
				return ReviewLoopState{}, false, nil
			}
			return *f.review, true, nil
		},
		loadMilestones: func(id string) ([]MilestoneRecord, error) {
			return slices.Clone(f.milestones), nil
		},
		journal: func(r journalRecord) error {
			f.journal = append(f.journal, r)
			return nil
		},
		now:          func() time.Time { return f.now },
		sleep:        func(d time.Duration) { f.now = f.now.Add(d) },
		stopWait:     50 * time.Millisecond,
		stopPoll:     10 * time.Millisecond,
		callerPaneID: f.callerPane,
	}
}

func TestShiftWorktreeHandoffArtifactNoteFreshSessionAndGitPreserved(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())
	repoDir, wtDir, branch, headSHA := initTestGitWorktree(t)

	cfg := defaultConfig()
	cfg.Repo = repoDir
	cfg.BranchPrefix = "topic/"
	cfg.WorkerAgent = "pi"

	f := &fakeShiftHarness{
		bead: Bead{
			ID:          "task-1",
			Title:       "Long multi-round task",
			Status:      StatusInProgress,
			Assignee:    "pi",
			Description: "Build feature cleanly.\n\n## Acceptance\n- Unit tests pass\n- Race detector passes\n",
			Notes:       "PR: https://github.com/acme/app/pull/55\nInvestigated race in worker loop",
		},
		agents: []Agent{{
			Name:        "task-1",
			Kind:        "pi",
			Status:      "done",
			PaneID:      "w9:p1",
			WorkspaceID: "w9",
			Cwd:         wtDir,
			Session:     AgentSession{Agent: "pi", Kind: "path", Value: "/sessions/old_task-1.jsonl"},
		}},
		worktrees: []Worktree{{
			Branch:      branch,
			Path:        wtDir,
			Linked:      true,
			WorkspaceID: "w9",
		}},
		panes: map[string][]Pane{"w9": {{ID: "w9:p1", Agent: "task-1", Cwd: wtDir}}},
		pr: &PR{
			Number:         55,
			URL:            "https://github.com/acme/app/pull/55",
			State:          "OPEN",
			ReviewDecision: "CHANGES_REQUESTED",
			Head:           branch,
			HeadSHA:        headSHA,
		},
		review: &ReviewLoopState{
			Version:      1,
			Bead:         "task-1",
			State:        "changes_requested",
			CurrentRound: 1,
			MaxRounds:    2,
			LastHeadSHA:  headSHA,
			ReviewerBead: "task-rev-1",
			Rounds: []ReviewRound{{
				Round:   1,
				HeadSHA: headSHA,
				Verdict: "cannot_merge",
				Summary: "Race on shared map",
				Findings: []ReviewFinding{{
					File:        "app.go",
					Line:        2,
					Summary:     "unsynchronized write",
					Consequence: "panics under -race",
				}},
			}},
		},
		milestones: []MilestoneRecord{{
			Version: 1,
			At:      t0,
			Bead:    "task-1",
			Phase:   "pr_opened",
			Status:  "ok",
			Summary: "Opened PR #55",
			Evidence: &MilestoneEvidence{
				PRURL: "https://github.com/acme/app/pull/55",
				SHA:   headSHA,
			},
		}},
		level:      5,
		exitOnQuit: true,
		now:        t0.Add(time.Hour),
	}

	var buf bytes.Buffer
	err := shiftCmd(cfg, f.env(cfg), []string{
		"task-1",
		"--reason", "context bloat after review round 1",
		"--next", "fix unsynchronized map write and re-run race tests",
		"--by", "coordinator",
		"--json",
	}, &buf)
	if err != nil {
		t.Fatalf("shiftCmd: %v", err)
	}

	var res ShiftResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal ShiftResult: %v\n%s", err, buf.String())
	}

	// Verify durable handoff artifact path, permissions (0700 dir / 0600 file), and schema.
	wantPath := filepath.Join(stateDir(), "handoffs", "task-1.json")
	if res.ArtifactPath != wantPath {
		t.Fatalf("ArtifactPath = %q, want %q", res.ArtifactPath, wantPath)
	}
	dirInfo, err := os.Stat(filepath.Dir(wantPath))
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("handoffs dir mode = %v, err=%v, want dir 0700", dirInfo, err)
	}
	fileInfo, err := os.Stat(wantPath)
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("handoff file mode = %v, err=%v, want 0600", fileInfo, err)
	}

	loaded, ok, err := loadShiftHandoff("task-1")
	if err != nil || !ok {
		t.Fatalf("loadShiftHandoff: ok=%v err=%v", ok, err)
	}
	if loaded.ShiftNumber != 1 || loaded.HeadSHA != headSHA || loaded.Branch != branch || loaded.Workdir != wtDir {
		t.Fatalf("unexpected git/shift metadata: %+v", loaded)
	}
	if !loaded.Dirty || loaded.DirtyCount != 2 {
		t.Fatalf("expected Dirty=true DirtyCount=2, got %+v", loaded)
	}
	if loaded.PreviousAgent != "pi" || loaded.PreviousPaneID != "w9:p1" || loaded.PreviousSession != "/sessions/old_task-1.jsonl" {
		t.Fatalf("unexpected previous agent fields: %+v", loaded)
	}
	if loaded.NextAgent != "pi" || loaded.NextSessionID != "task-1-shift-1" || loaded.NextPaneID != "w10:p1" {
		t.Fatalf("unexpected next agent fields: %+v", loaded)
	}
	if loaded.Review == nil || loaded.Review.Status != "changes_requested" || loaded.Review.LastVerdict != "cannot_merge" || len(loaded.Review.Findings) != 1 {
		t.Fatalf("unexpected review summary: %+v", loaded.Review)
	}
	if len(loaded.Milestones) != 1 || loaded.Milestones[0].Phase != "pr_opened" {
		t.Fatalf("unexpected milestones: %+v", loaded.Milestones)
	}
	if !slices.Equal(loaded.Acceptance, []string{"Unit tests pass", "Race detector passes"}) {
		t.Fatalf("unexpected acceptance: %+v", loaded.Acceptance)
	}

	// Verify HANDOFF: note and AUTO: note were recorded.
	if len(f.notes) != 2 {
		t.Fatalf("expected 2 notes (HANDOFF + AUTO), got %d: %+v", len(f.notes), f.notes)
	}
	for _, must := range []string{"HANDOFF: shift #1", wtDir, short(headSHA), "dirty=2 file(s)", "PR #55", "review=changes_requested (1/2)", "milestones=1", "acceptance=2 item(s)", "context bloat", "fix unsynchronized map write", wantPath} {
		if !strings.Contains(f.notes[0], must) {
			t.Errorf("HANDOFF note missing %q:\n%s", must, f.notes[0])
		}
	}
	if !strings.HasPrefix(f.notes[1], "AUTO: [coordinator L5 C3] shifted task-1") {
		t.Errorf("unexpected AUTO note: %q", f.notes[1])
	}

	// Verify old worker was gracefully asked to /quit and its pane was closed (never deleting worktree).
	if len(f.prompts) < 2 || f.prompts[0] != "w9:p1: /quit" {
		t.Fatalf("expected graceful /quit prompt first, got %+v", f.prompts)
	}
	if !slices.Equal(f.closedPanes, []string{"w9:p1"}) {
		t.Fatalf("closedPanes = %v, want [w9:p1]", f.closedPanes)
	}
	if !slices.Equal(f.openedWT, []string{branch}) {
		t.Fatalf("openedWT = %v, want [%s]", f.openedWT, branch)
	}

	// Verify fresh-context pi worker got --session-id task-1-shift-1 (NOT task-1) and no --continue.
	if len(f.startedCalls) != 1 {
		t.Fatalf("startedCalls = %d, want 1", len(f.startedCalls))
	}
	sc := f.startedCalls[0]
	if sc.Name != "task-1" || sc.Kind != "pi" || sc.Pane != "w10:p1" {
		t.Fatalf("unexpected startAgent call: %+v", sc)
	}
	joinedArgs := strings.Join(sc.Args, " ")
	if !strings.Contains(joinedArgs, "--session-id task-1-shift-1") || strings.Contains(joinedArgs, "--continue") {
		t.Fatalf("pi fresh session args wrong: %q", joinedArgs)
	}

	// Verify shift brief sent to fresh worker contains handoff context.
	briefPrompt := f.prompts[len(f.prompts)-1]
	for _, must := range []string{
		"## Shift Handoff (Shift #1 — Fresh Context)",
		wantPath,
		headSHA,
		"scratch_wip.txt",
		"unsynchronized write",
		"Opened PR #55",
		"Unit tests pass",
		"fix unsynchronized map write",
	} {
		if !strings.Contains(briefPrompt, must) {
			t.Errorf("shift brief missing %q:\n%s", must, briefPrompt)
		}
	}

	// Verify git state on disk (HEAD, modified file, untracked file) is 100% intact.
	afterGit, err := liveInspectShiftGit(cfg, localHost, wtDir)
	if err != nil {
		t.Fatalf("inspect git after shift: %v", err)
	}
	if afterGit.HeadSHA != headSHA || afterGit.Branch != branch || !afterGit.Dirty || afterGit.DirtyCount != 2 {
		t.Fatalf("git state altered by shift: %+v", afterGit)
	}
	gotApp, err := os.ReadFile(filepath.Join(wtDir, "app.go"))
	if err != nil || !strings.Contains(string(gotApp), "uncommitted edit") {
		t.Fatalf("modified file lost: %q, err=%v", string(gotApp), err)
	}
	if _, err := os.Stat(filepath.Join(wtDir, "scratch_wip.txt")); err != nil {
		t.Fatalf("untracked file lost: %v", err)
	}

	// Verify journal entry recorded.
	if len(f.journal) != 1 || f.journal[0].Kind != journalShift || f.journal[0].Class != classHousekeeping || !f.journal[0].Pass {
		t.Fatalf("unexpected journal records: %+v", f.journal)
	}

	// Run a second shift: ShiftNumber increments to 2 and shift #1 moves into PriorHandoffs.
	f.now = f.now.Add(30 * time.Minute)
	buf.Reset()
	if err := shiftCmd(cfg, f.env(cfg), []string{"task-1", "--reason", "second handoff", "--by", "TJ", "--json"}, &buf); err != nil {
		t.Fatalf("second shift: %v", err)
	}
	loaded2, ok, err := loadShiftHandoff("task-1")
	if err != nil || !ok {
		t.Fatalf("load second handoff: ok=%v err=%v", ok, err)
	}
	if loaded2.ShiftNumber != 2 || loaded2.NextSessionID != "task-1-shift-2" || len(loaded2.PriorHandoffs) != 1 || loaded2.PriorHandoffs[0].ShiftNumber != 1 {
		t.Fatalf("unexpected second handoff: %+v", loaded2)
	}

	// Verify `quartermaster shift show task-1 --json`.
	buf.Reset()
	if err := shiftCmd(cfg, f.env(cfg), []string{"show", "task-1", "--json"}, &buf); err != nil {
		t.Fatalf("shift show: %v", err)
	}
	var shown ShiftHandoff
	if err := json.Unmarshal(buf.Bytes(), &shown); err != nil || shown.ShiftNumber != 2 {
		t.Fatalf("shift show JSON mismatch: err=%v shown=%+v", err, shown)
	}
}

func TestShiftRoleDirWithoutWorktree(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())

	rdir, err := ensureRoleDir("task-rev")
	if err != nil {
		t.Fatal(err)
	}
	noteFile := filepath.Join(rdir, "review-notes.md")
	if err := os.WriteFile(noteFile, []byte("checked files 1-10\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	cfg.Repo = "/src/main-repo"
	cfg.Roles = map[string]RoleConfig{
		roleReviewer: {
			Agent:    "claude",
			Model:    "claude-opus-5-5",
			Thinking: "high",
		},
	}

	wtCalled := false
	f := &fakeShiftHarness{
		bead: Bead{
			ID:       "task-rev",
			Title:    "Review gateway changes",
			Status:   StatusInProgress,
			Assignee: "claude",
			Labels:   []string{"role:reviewer"},
		},
		agents: []Agent{{
			Name:        "task-rev",
			Kind:        "claude",
			Status:      "idle",
			PaneID:      "w5:p1",
			WorkspaceID: "w5",
			Cwd:         rdir,
		}},
		panes:      map[string][]Pane{"w5": {{ID: "w5:p1", Agent: "task-rev", Cwd: rdir}}},
		level:      4,
		exitOnQuit: true,
		now:        t0,
	}
	env := f.env(cfg)
	env.worktrees = func(host, repo string) ([]Worktree, error) {
		wtCalled = true
		return nil, nil
	}

	res, err := shiftWorker(cfg, env, "task-rev", ShiftOpts{Reason: "fresh review pass", By: "coordinator"})
	if err != nil {
		t.Fatalf("shiftWorker role dir: %v", err)
	}
	if wtCalled {
		t.Fatal("worktree=false shift must not query git worktrees")
	}
	if res.Handoff.Worktree || res.Handoff.Workdir != rdir || res.Handoff.Role != roleReviewer {
		t.Fatalf("unexpected role handoff: %+v", res.Handoff)
	}
	// Claude gets /exit (not /quit), fresh launch without --continue, and role model/effort args.
	if len(f.prompts) < 2 || f.prompts[0] != "w5:p1: /exit" {
		t.Fatalf("expected /exit for claude, got %+v", f.prompts)
	}
	if len(f.startedCalls) != 1 || slices.Contains(f.startedCalls[0].Args, "--continue") {
		t.Fatalf("claude fresh shift must not pass --continue: %+v", f.startedCalls)
	}
	if !slices.Contains(f.startedCalls[0].Args, "--model") || !slices.Contains(f.startedCalls[0].Args, "claude-opus-5-5") {
		t.Fatalf("role model override missing: %+v", f.startedCalls[0].Args)
	}
	if !slices.Equal(f.createdWS, []string{rdir}) {
		t.Fatalf("createdWS = %v, want [%s]", f.createdWS, rdir)
	}
	// Existing file inside <state>/roles/task-rev is untouched.
	if b, err := os.ReadFile(noteFile); err != nil || string(b) != "checked files 1-10\n" {
		t.Fatalf("role dir contents altered: %q err=%v", string(b), err)
	}
}

func TestShiftRefusesActiveTurnBlockedAndLowAutonomy(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())

	rdir, err := ensureRoleDir("task-guard")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()

	baseHarness := func(status string, level int) *fakeShiftHarness {
		return &fakeShiftHarness{
			bead: Bead{
				ID:       "task-guard",
				Title:    "Investigate latency",
				Status:   StatusInProgress,
				Assignee: "codex",
				Labels:   []string{"role:investigator"},
			},
			agents: []Agent{{
				Name:        "task-guard",
				Kind:        "codex",
				Status:      status,
				PaneID:      "w6:p1",
				WorkspaceID: "w6",
				Cwd:         rdir,
			}},
			panes: map[string][]Pane{"w6": {{ID: "w6:p1", Agent: "task-guard", Cwd: rdir}}},
			level: level,
			now:   t0,
		}
	}

	// 1. Mid-turn working agent refused without --force, zero side effects.
	f1 := baseHarness("working", 5)
	if _, err := shiftWorker(cfg, f1.env(cfg), "task-guard", ShiftOpts{By: "coordinator"}); !errors.Is(err, errShiftActiveTurn) {
		t.Fatalf("working agent without --force: err=%v, want errShiftActiveTurn", err)
	}
	if len(f1.closedPanes) != 0 || len(f1.notes) != 0 {
		t.Fatalf("refused shift had side effects: closed=%v notes=%v", f1.closedPanes, f1.notes)
	}
	if _, ok, _ := loadShiftHandoff("task-guard"); ok {
		t.Fatal("refused shift must not write handoff artifact")
	}

	// 2. Blocked agent refused without --force.
	f2 := baseHarness("blocked", 5)
	if _, err := shiftWorker(cfg, f2.env(cfg), "task-guard", ShiftOpts{By: "coordinator"}); !errors.Is(err, errShiftActiveTurn) {
		t.Fatalf("blocked agent without --force: err=%v, want errShiftActiveTurn", err)
	}

	// 3. Active control-socket approval ask refused without --force.
	f3 := baseHarness("idle", 5)
	f3.liveAsk = true
	if _, err := shiftWorker(cfg, f3.env(cfg), "task-guard", ShiftOpts{By: "coordinator"}); !errors.Is(err, errShiftActiveTurn) {
		t.Fatalf("live ask without --force: err=%v, want errShiftActiveTurn", err)
	}

	// 4. Caller's own active pane refused without --force.
	f4 := baseHarness("idle", 5)
	f4.callerPane = "w6:p1"
	if _, err := shiftWorker(cfg, f4.env(cfg), "task-guard", ShiftOpts{By: "coordinator"}); !errors.Is(err, errShiftActiveTurn) {
		t.Fatalf("caller pane without --force: err=%v, want errShiftActiveTurn", err)
	}

	// 5. Class 3 autonomy enforced: Level 2 denied for coordinator with zero side effects.
	f5 := baseHarness("idle", 2)
	if _, err := shiftWorker(cfg, f5.env(cfg), "task-guard", ShiftOpts{By: "coordinator"}); !errors.Is(err, errNotAutonomous) || !strings.Contains(err.Error(), "class 3") {
		t.Fatalf("L2 coordinator shift: err=%v, want errNotAutonomous class 3", err)
	}
	if len(f5.closedPanes) != 0 || len(f5.notes) != 0 {
		t.Fatalf("denied autonomy shift had side effects: closed=%v notes=%v", f5.closedPanes, f5.notes)
	}
	if _, ok, _ := loadShiftHandoff("task-guard"); ok {
		t.Fatal("denied autonomy shift must not write handoff artifact")
	}

	// 6. --by TJ allowed at Level 2 with no AUTO note.
	if _, err := shiftWorker(cfg, f5.env(cfg), "task-guard", ShiftOpts{By: "TJ"}); err != nil {
		t.Fatalf("TJ shift at L2 failed: %v", err)
	}
	if len(f5.notes) != 1 || !strings.HasPrefix(f5.notes[0], "HANDOFF:") {
		t.Fatalf("TJ shift notes = %+v, want only HANDOFF note", f5.notes)
	}

	// 7. --force shifts an actively working agent without typing /exit mid-turn.
	f6 := baseHarness("working", 5)
	if _, err := shiftWorker(cfg, f6.env(cfg), "task-guard", ShiftOpts{Force: true, By: "coordinator"}); err != nil {
		t.Fatalf("forced shift failed: %v", err)
	}
	if len(f6.closedPanes) != 1 || f6.closedPanes[0] != "w6:p1" {
		t.Fatalf("forced shift did not close pane: %v", f6.closedPanes)
	}
	if len(f6.prompts) != 1 || strings.Contains(f6.prompts[0], "/exit") {
		t.Fatalf("forced mid-turn shift should not send /exit before closing pane: %+v", f6.prompts)
	}
	if len(f6.startedCalls) != 1 || slices.Contains(f6.startedCalls[0].Args, "resume") {
		t.Fatalf("codex fresh shift must not pass resume --last: %+v", f6.startedCalls)
	}
}

func TestShiftExitedWorkerSplitPaneQueuedBriefAndValidation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())
	repoDir, wtDir, branch, _ := initTestGitWorktree(t)

	cfg := defaultConfig()
	cfg.Repo = repoDir
	cfg.BranchPrefix = "topic/"

	// Validation fail-closed cases.
	fVal := &fakeShiftHarness{
		bead:  Bead{ID: "task-closed", Status: StatusClosed},
		level: 5,
		now:   t0,
	}
	if _, err := shiftWorker(cfg, fVal.env(cfg), "../escape", ShiftOpts{}); !errors.Is(err, errShift) {
		t.Fatalf("invalid bead id should fail closed: %v", err)
	}
	if _, err := shiftWorker(cfg, fVal.env(cfg), "task-closed", ShiftOpts{}); !errors.Is(err, errShift) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed bead should fail closed: %v", err)
	}

	// Exited worker (no live agent) with an existing free pane reuses pane and queues brief on agent_not_ready.
	fExited := &fakeShiftHarness{
		bead: Bead{
			ID:       "task-1",
			Title:    "Resume cleanly",
			Status:   StatusInProgress,
			Assignee: "claude",
		},
		worktrees: []Worktree{{Branch: branch, Path: wtDir, Linked: true, WorkspaceID: "w9"}},
		panes:     map[string][]Pane{"w9": {{ID: "w9:p1", Agent: "", Cwd: wtDir}}},
		startErr:  errors.New("agent_not_ready: folder trust prompt"),
		level:     5,
		now:       t0,
	}
	res, err := shiftWorker(cfg, fExited.env(cfg), "task-1", ShiftOpts{By: "coordinator"})
	if err != nil {
		t.Fatalf("shift exited worker: %v", err)
	}
	if !res.QueuedBrief || res.PaneID != "w9:p1" {
		t.Fatalf("expected QueuedBrief=true on reused w9:p1, got %+v", res)
	}
	if len(fExited.runCmds) != 1 || !strings.Contains(fExited.runCmds[0], "cd "+shellQuote(wtDir)) {
		t.Fatalf("reused pane should cd to workdir first: %+v", fExited.runCmds)
	}
	outboxData, err := os.ReadFile(filepath.Join(outboxDir(), "task-1.md"))
	if err != nil || !strings.Contains(string(outboxData), "## Shift Handoff") {
		t.Fatalf("queued shift brief missing from outbox: err=%v", err)
	}

	// Workspace with a second manual pane splits instead of hijacking the manual pane.
	fSplit := &fakeShiftHarness{
		bead: Bead{
			ID:       "task-1",
			Title:    "Split beside manual pane",
			Status:   StatusInProgress,
			Assignee: "claude",
		},
		agents: []Agent{{
			Name:        "task-1",
			Kind:        "claude",
			Status:      "idle",
			PaneID:      "w9:p1",
			WorkspaceID: "w9",
			Cwd:         wtDir,
		}},
		worktrees:  []Worktree{{Branch: branch, Path: wtDir, Linked: true, WorkspaceID: "w9"}},
		panes:      map[string][]Pane{"w9": {{ID: "w9:p1", Agent: "task-1", Cwd: wtDir}, {ID: "w9:p9", Agent: "", Cwd: wtDir}}},
		level:      5,
		exitOnQuit: true,
		now:        t0.Add(time.Minute),
	}
	resSplit, err := shiftWorker(cfg, fSplit.env(cfg), "task-1", ShiftOpts{By: "TJ"})
	if err != nil {
		t.Fatalf("shift with sibling pane: %v", err)
	}
	if resSplit.PaneID != "w9:p2" || len(fSplit.splitCalls) != 1 || fSplit.splitCalls[0] != "w9:p9@"+wtDir {
		t.Fatalf("expected split from remaining pane w9:p9, got pane=%q splits=%v", resSplit.PaneID, fSplit.splitCalls)
	}
}
