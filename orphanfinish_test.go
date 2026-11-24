package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeOrphanEnv struct {
	beads      map[string]Bead
	agents     []Agent
	complete   bool
	notedPRs   map[string]PR
	runs       map[string]Run
	level      int
	enforceErr error
	closeErr   error
	resolveMsg string
	resolveErr error
	held       map[string]bool
	headless   map[string]bool

	closed    []string
	notes     []string
	resolved  []string
	events    []Event
	journal   []journalRecord
	cleared   []string
	showCalls int
	prLookups int
}

func newFakeOrphanEnv(level int, beads ...Bead) *fakeOrphanEnv {
	m := make(map[string]Bead, len(beads))
	for _, b := range beads {
		m[b.ID] = b
	}
	return &fakeOrphanEnv{
		beads:      m,
		complete:   true,
		notedPRs:   map[string]PR{},
		runs:       map[string]Run{},
		level:      level,
		resolveMsg: "removed worktree /tmp/wt; deleted merged branch topic/task",
		held:       map[string]bool{},
		headless:   map[string]bool{},
	}
}

func (f *fakeOrphanEnv) env() orphanFinishEnv {
	return orphanFinishEnv{
		show: func(id string) (Bead, error) {
			f.showCalls++
			b, ok := f.beads[id]
			if !ok {
				return Bead{}, fmt.Errorf("%w: %s", errBeadNotFound, id)
			}
			return b, nil
		},
		agents: func() ([]Agent, bool, error) {
			return f.agents, f.complete, nil
		},
		lookupPRs: func(urls []string) (map[string]PR, error) {
			f.prLookups++
			out := map[string]PR{}
			for _, u := range urls {
				if p, ok := f.notedPRs[u]; ok {
					out[u] = p
				}
			}
			return out, nil
		},
		runView: func(repo, id string) (Run, error) {
			if r, ok := f.runs[id]; ok {
				return r, nil
			}
			return Run{}, fmt.Errorf("run %s not found", id)
		},
		canCloseLevel: func(b Bead) bool {
			return f.level >= classHousekeeping
		},
		enforce: func(b Bead, action, by string) (string, error) {
			if f.enforceErr != nil {
				return "", f.enforceErr
			}
			return allow(b.ID, by, action, classHousekeeping, f.level, "")
		},
		closeBead: func(id, reason string) error {
			if f.closeErr != nil {
				return f.closeErr
			}
			f.closed = append(f.closed, id+": "+reason)
			if b, ok := f.beads[id]; ok {
				b.Status = StatusClosed
				f.beads[id] = b
			}
			return nil
		},
		note: func(id, text string) error {
			f.notes = append(f.notes, id+": "+text)
			if b, ok := f.beads[id]; ok {
				if b.Notes != "" {
					b.Notes += "\n"
				}
				b.Notes += text
				f.beads[id] = b
			}
			return nil
		},
		resolve: func(id string) (string, error) {
			f.resolved = append(f.resolved, id)
			if f.resolveErr != nil {
				return "", f.resolveErr
			}
			return f.resolveMsg, nil
		},
		event: func(e Event) error {
			f.events = append(f.events, e)
			return nil
		},
		journal: func(r journalRecord) error {
			f.journal = append(f.journal, r)
			return nil
		},
		clearOutbox: func(agent string) error {
			f.cleared = append(f.cleared, agent)
			return nil
		},
		held:     func(id string) bool { return f.held[id] },
		headless: func(id string) bool { return f.headless[id] },
		logf:     func(string, ...any) {},
	}
}

func TestOrphanFinishConfig(t *testing.T) {
	def := defaultConfig().OrphanFinish
	if !def.enabled() || def.interval() != 5*time.Minute || def.grace() != 10*time.Minute || def.maxPerPass() != 5 {
		t.Fatalf("default orphan_finish = %+v, want 5m/10m/5", def)
	}
	if (Config{}).OrphanFinish.enabled() {
		t.Fatal("zero Config should have orphan_finish disabled")
	}

	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("repo = \"/repo\"\n")
	if cfg, err := loadConfig(); err != nil || cfg.OrphanFinish.IntervalSeconds != 300 || cfg.OrphanFinish.GraceSeconds != 600 || cfg.OrphanFinish.MaxPerPass != 5 {
		t.Fatalf("absent [orphan_finish]: err=%v cfg=%+v", err, cfg.OrphanFinish)
	}
	write("repo = \"/repo\"\n[orphan_finish]\ninterval_seconds = 0\n")
	if cfg, err := loadConfig(); err != nil || cfg.OrphanFinish.enabled() {
		t.Fatalf("interval_seconds = 0 should disable: err=%v cfg=%+v", err, cfg.OrphanFinish)
	}
	write("repo = \"/repo\"\n[orphan_finish]\ninterval_seconds = 120\ngrace_seconds = 900\nmax_per_pass = 3\n")
	if cfg, err := loadConfig(); err != nil || cfg.OrphanFinish.interval() != 2*time.Minute || cfg.OrphanFinish.grace() != 15*time.Minute || cfg.OrphanFinish.maxPerPass() != 3 {
		t.Fatalf("custom [orphan_finish]: err=%v cfg=%+v", err, cfg.OrphanFinish)
	}
	for name, body := range map[string]string{
		"interval_seconds": "repo = \"/repo\"\n[orphan_finish]\ninterval_seconds = -1\n",
		"grace_seconds":    "repo = \"/repo\"\n[orphan_finish]\ngrace_seconds = -1\n",
		"max_per_pass":     "repo = \"/repo\"\n[orphan_finish]\nmax_per_pass = -1\n",
	} {
		write(body)
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("negative %s accepted: %v", name, err)
		}
	}
	if !strings.Contains(configTemplate("/repo"), "[orphan_finish]") {
		t.Fatal("configTemplate does not mention [orphan_finish]")
	}
}

func TestParentWithOpenChildrenExcludedFromMergedAcrossViews(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	pr := PR{
		Number:   42,
		Title:    "ENG-500: initial groundwork",
		URL:      "https://github.com/acme/app/pull/42",
		State:    "MERGED",
		Head:     "topic/epic-1-groundwork",
		MergedAt: now.Add(-30 * time.Minute),
	}
	epicParent := Bead{
		ID:        "epic-1",
		Title:     "ENG-500: Platform rollout epic",
		Type:      "epic",
		Status:    StatusInProgress,
		Notes:     "PR: " + pr.URL,
		UpdatedAt: now.Add(-30 * time.Minute),
	}
	openChild := Bead{
		ID:        "epic-1.1",
		Title:     "Remaining child work",
		Type:      "task",
		Status:    StatusOpen,
		Parent:    "epic-1",
		UpdatedAt: now.Add(-20 * time.Minute),
	}
	taskParent := Bead{
		ID:        "task-parent",
		Title:     "Parent task with dependency child",
		Type:      "task",
		Status:    StatusInProgress,
		Notes:     "PR: https://github.com/acme/app/pull/43",
		UpdatedAt: now.Add(-30 * time.Minute),
	}
	depChild := Bead{
		ID:           "task-parent.1",
		Title:        "Child linked via parent-child dependency",
		Type:         "task",
		Status:       StatusInProgress,
		Dependencies: []Dependency{{DependsOn: "task-parent", Type: "parent-child"}},
		UpdatedAt:    now.Add(-15 * time.Minute),
	}
	closedChild := Bead{
		ID:        "epic-1.0",
		Title:     "Finished child",
		Type:      "task",
		Status:    StatusClosed,
		Parent:    "epic-1",
		UpdatedAt: now.Add(-40 * time.Minute),
	}

	counts := openChildCounts([]Bead{epicParent, openChild, taskParent, depChild, closedChild})
	if counts["epic-1"] != 1 || counts["task-parent"] != 1 {
		t.Fatalf("openChildCounts = %+v, want epic-1=1, task-parent=1", counts)
	}

	cfg := defaultConfig()
	cfg.Repo = "/repo"
	cfg.BranchPrefix = "topic/"
	pr2 := PR{Number: 43, URL: "https://github.com/acme/app/pull/43", State: "MERGED", MergedAt: now.Add(-30 * time.Minute)}
	st := TickerState{
		PRs: map[string]PR{
			"epic-1":      pr,
			"task-parent": pr2,
		},
	}

	// Direct ThreadState derivation: parent with open children is NOT ThreadMerged.
	epicTh := threadFor(cfg, st, epicParent, nil)
	epicTh.OpenChildren = 1
	epicTh = epicTh.derive()
	if epicTh.derived().Kind == ThreadMerged || classify(epicTh) == GroupMerged {
		t.Fatalf("epic with open child classified as merged: %+v (%s)", epicTh.derived(), classify(epicTh))
	}
	if classify(epicTh) != GroupNoAgent {
		t.Fatalf("agentless parent with open child = %s, want no agent", classify(epicTh))
	}

	// With a working or idle agent, it derives to working / idle, not merged.
	epicWorking := epicTh
	epicWorking.Agent = &Agent{Name: "epic-1", Status: "working", Seq: 2}
	epicWorking = epicWorking.derive()
	if classify(epicWorking) != GroupWorking {
		t.Fatalf("working parent with open child = %s, want working", classify(epicWorking))
	}
	epicIdle := epicTh
	epicIdle.Agent = &Agent{Name: "epic-1", Status: "idle", Seq: 2}
	epicIdle = epicIdle.derive()
	if classify(epicIdle) != GroupIdle {
		t.Fatalf("idle parent with open child = %s, want idle", classify(epicIdle))
	}

	// Merge transition announces PR merge but does not prompt to close while open children remain.
	out := transition(epicIdle, Snapshot{PRNumber: 42, PRState: "OPEN"}, false, now)
	if !slices.ContainsFunc(out.Events, func(e Event) bool { return e.Kind == EventMerged }) {
		t.Fatalf("expected EventMerged announcement, got %+v", out.Events)
	}
	if len(out.Prompts) != 0 {
		t.Fatalf("parent with open children should not be prompted to close: %+v", out.Prompts)
	}

	// linearMoves does not suggest moving parent's Linear issue to Done while children are open.
	if moves := linearMoves([]Thread{epicTh}); len(moves) != 0 {
		t.Fatalf("parent with open children suggested linear move: %v", moves)
	}

	// Context excludes parent from merged even when child arrives in ready/next list.
	rawEpicTh := threadFor(cfg, st, epicParent, nil) // OpenChildren intentionally unset
	rawTaskParentTh := threadFor(cfg, st, taskParent, nil)
	rawDepChildTh := threadFor(cfg, st, depChild, nil)
	ctxOut := renderContext(cfg, []Thread{rawEpicTh, rawTaskParentTh, rawDepChildTh}, nil, []Bead{openChild}, nil, st, map[string]bool{}, now)
	if strings.Contains(ctxOut, "merged · PR #42") || strings.Contains(ctxOut, "merged · PR #43") || strings.Contains(ctxOut, "1 merged") || strings.Contains(ctxOut, "2 merged") {
		t.Fatalf("context classified parent with open children as merged:\n%s", ctxOut)
	}
	if strings.Contains(ctxOut, "ENG-500 → Done") {
		t.Fatalf("context suggested Linear Done for parent with open children:\n%s", ctxOut)
	}

	// Board excludes parent with open children from merged section.
	rows := buildRows(cfg, boardData{
		Beads: []Bead{epicParent, openChild, taskParent, depChild},
		State: st,
	}, sortPriority)
	for _, r := range rows {
		if r.header == "" && (r.bead.ID == "epic-1" || r.bead.ID == "task-parent") && r.group == GroupMerged {
			t.Fatalf("board placed parent %s in merged group", r.bead.ID)
		}
	}

	// Digest excludes parent with open children from SecMerged.
	d, _ := buildDigest(digestInputs{
		cfg:    cfg,
		open:   []Bead{epicParent, openChild, taskParent, depChild},
		ticker: st,
		branch: map[string]string{"epic-1": pr.URL, "task-parent": pr2.URL},
		prs: map[string]DigestPR{
			pr.URL:  digestObservation(pr),
			pr2.URL: digestObservation(pr2),
		},
	}, digestState{}, nil, now)
	for _, id := range []string{"epic-1", "task-parent"} {
		if it := item(t, d, id); it.Section == SecMerged {
			t.Fatalf("digest placed parent %s in SecMerged", id)
		}
	}

	// Report excludes parent with open children from ThreadMerged.
	rep := buildReportFor(cfg, now.Add(-time.Hour), nil, []Bead{epicParent, openChild, taskParent, depChild}, nil, st)
	for _, inf := range rep.InFlight {
		if (inf.Bead.ID == "epic-1" || inf.Bead.ID == "task-parent") && inf.Derived.Kind == ThreadMerged {
			t.Fatalf("report classified parent %s as ThreadMerged: %+v", inf.Bead.ID, inf)
		}
	}

	// Once all children are closed, a non-epic parent CAN classify as merged.
	closedParentTh := threadFor(cfg, st, taskParent, nil)
	closedParentTh.OpenChildren = 0
	closedParentTh = closedParentTh.derive()
	if classify(closedParentTh) != GroupMerged {
		t.Fatalf("non-epic parent with 0 open children = %s, want merged", classify(closedParentTh))
	}
}

func TestOrphanSweepExcludesParentsWithOpenChildrenAndEpics(t *testing.T) {
	now := t0
	cfg := defaultConfig()
	cfg.BranchPrefix = "topic/"

	epicNoChildren := Bead{
		ID:        "epic-empty",
		Title:     "Standalone epic",
		Type:      "epic",
		Status:    StatusInProgress,
		Notes:     "PR: https://github.com/acme/app/pull/10",
		UpdatedAt: now.Add(-30 * time.Minute),
	}
	parentTask := Bead{
		ID:        "task-parent",
		Title:     "Parent task",
		Type:      "task",
		Status:    StatusInProgress,
		Notes:     "PR: https://github.com/acme/app/pull/11",
		UpdatedAt: now.Add(-30 * time.Minute),
	}
	openChild := Bead{
		ID:        "task-parent.1",
		Title:     "Open follow-up child",
		Type:      "task",
		Status:    StatusOpen,
		Parent:    "task-parent",
		UpdatedAt: now.Add(-25 * time.Minute),
	}
	parentViaDep := Bead{
		ID:        "task-dep-parent",
		Title:     "Parent via dep",
		Type:      "task",
		Status:    StatusInProgress,
		Notes:     "PR: https://github.com/acme/app/pull/12",
		UpdatedAt: now.Add(-30 * time.Minute),
	}
	depChild := Bead{
		ID:           "task-dep-child",
		Title:        "Open dep child",
		Type:         "task",
		Status:       StatusOpen,
		Dependencies: []Dependency{{DependsOn: "task-dep-parent", Type: "parent-child"}},
		UpdatedAt:    now.Add(-25 * time.Minute),
	}

	all := []Bead{epicNoChildren, parentTask, openChild, parentViaDep, depChild}
	f := newFakeOrphanEnv(5, all...)
	st := TickerState{
		PRs: map[string]PR{
			"epic-empty":      {Number: 10, URL: "https://github.com/acme/app/pull/10", State: "MERGED", MergedAt: now.Add(-30 * time.Minute)},
			"task-parent":     {Number: 11, URL: "https://github.com/acme/app/pull/11", State: "MERGED", MergedAt: now.Add(-30 * time.Minute)},
			"task-dep-parent": {Number: 12, URL: "https://github.com/acme/app/pull/12", State: "MERGED", MergedAt: now.Add(-30 * time.Minute)},
		},
	}

	orphanFinishPass(cfg, f.env(), &st, all, nil, true, now)
	if len(f.closed) != 0 || len(f.events) != 0 || len(f.resolved) != 0 {
		t.Fatalf("epic/parent with open children swept: closed=%v events=%+v resolved=%v", f.closed, f.events, f.resolved)
	}
}

func TestOrphanSweepMergedAutoCloseAtL3AndSurfaceBelowL3(t *testing.T) {
	now := t0
	cfg := defaultConfig()
	cfg.BranchPrefix = "topic/"
	prURL := "https://github.com/acme/app/pull/77"
	pr := PR{Number: 77, URL: prURL, Head: "topic/task-1-cleanup", State: "MERGED", HeadSHA: "abc123", MergedAt: now.Add(-20 * time.Minute)}
	b := Bead{
		ID:        "task-1",
		Title:     "Clean up unused flag",
		Type:      "task",
		Status:    StatusInProgress,
		Notes:     "PR: " + prURL,
		UpdatedAt: now.Add(-20 * time.Minute),
	}

	t.Run("L3+ auto-closes, resolves clean worktree, emits auto_closed once", func(t *testing.T) {
		f := newFakeOrphanEnv(3, b)
		st := TickerState{
			PRs:     map[string]PR{"task-1": pr},
			Threads: map[string]Snapshot{"task-1": {Group: GroupMerged, PRNumber: 77, PRState: "MERGED"}},
		}
		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now)
		if len(f.closed) != 1 || !strings.Contains(f.closed[0], "PR #77") {
			t.Fatalf("closed = %v, want task-1 closed citing PR #77", f.closed)
		}
		if !slices.Equal(f.resolved, []string{"task-1"}) {
			t.Fatalf("resolved = %v, want [task-1]", f.resolved)
		}
		if len(f.notes) != 1 || !strings.Contains(f.notes[0], "AUTO: [ticker L3 C3]") {
			t.Fatalf("notes = %v, want AUTO: [ticker L3 C3]", f.notes)
		}
		if len(f.events) != 1 || f.events[0].Kind != EventAutoClosed || !strings.Contains(f.events[0].Summary, "removed worktree") {
			t.Fatalf("events = %+v, want one auto_closed with resolve summary", f.events)
		}
		if _, stillTracked := st.Threads["task-1"]; stillTracked {
			t.Fatal("st.Threads[task-1] should be deleted on auto-close so leftActive does not emit a duplicate closed event")
		}
		if !slices.Equal(f.cleared, []string{"task-1"}) {
			t.Fatalf("cleared outbox = %v, want [task-1]", f.cleared)
		}
		if mark := st.Orphans["task-1"]; mark.Action != orphanActionClosed {
			t.Fatalf("mark = %+v, want action=closed", mark)
		}

		// Re-running with the same stale slice before bd catches up must not duplicate.
		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now.Add(6*time.Minute))
		if len(f.closed) != 1 || len(f.events) != 1 {
			t.Fatalf("second sweep duplicated action: closed=%v events=%+v", f.closed, f.events)
		}
	})

	t.Run("below L3 surfaces deduplicated orphan_finish and auto-closes once raised to L3", func(t *testing.T) {
		f := newFakeOrphanEnv(2, b)
		st := TickerState{PRs: map[string]PR{"task-1": pr}}

		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now)
		if len(f.closed) != 0 || len(f.resolved) != 0 {
			t.Fatalf("should not close or resolve below L3: closed=%v resolved=%v", f.closed, f.resolved)
		}
		if len(f.events) != 1 || f.events[0].Kind != EventOrphanFinish {
			t.Fatalf("events = %+v, want one orphan_finish", f.events)
		}
		sum := f.events[0].Summary
		for _, want := range []string{"task-1", "PR #77", prURL, "quartermaster close task-1"} {
			if !strings.Contains(sum, want) {
				t.Errorf("orphan_finish summary missing %q: %s", want, sum)
			}
		}

		// Second pass still at L2: strictly deduplicated.
		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now.Add(6*time.Minute))
		if len(f.events) != 1 || len(f.closed) != 0 {
			t.Fatalf("second L2 pass duplicated event: events=%+v closed=%v", f.events, f.closed)
		}

		// Once autonomy is raised to L3+, the next pass auto-closes cleanly.
		f.level = 3
		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now.Add(12*time.Minute))
		if len(f.closed) != 1 || len(f.events) != 2 || f.events[1].Kind != EventAutoClosed {
			t.Fatalf("expected auto-close after raising level to 3: closed=%v events=%+v", f.closed, f.events)
		}
	})

	t.Run("failed close is deduplicated and never loops infinitely", func(t *testing.T) {
		f := newFakeOrphanEnv(4, b)
		f.closeErr = errors.New("bd database locked")
		st := TickerState{PRs: map[string]PR{"task-1": pr}}

		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now)
		if len(f.closed) != 0 || len(f.resolved) != 0 {
			t.Fatalf("unexpected close/resolve on close error: closed=%v resolved=%v", f.closed, f.resolved)
		}
		if len(f.events) != 1 || f.events[0].Kind != EventOrphanFinish || !strings.Contains(f.events[0].Summary, "bd database locked") {
			t.Fatalf("events = %+v, want one orphan_finish mentioning close error", f.events)
		}
		if mark := st.Orphans["task-1"]; mark.Action != orphanActionCloseFailed || mark.Error == "" {
			t.Fatalf("mark = %+v, want close_failed", mark)
		}

		// Subsequent sweeps with same state must NOT retry close or emit more events.
		showsBefore := f.showCalls
		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now.Add(6*time.Minute))
		orphanFinishPass(cfg, f.env(), &st, []Bead{b}, nil, true, now.Add(12*time.Minute))
		if len(f.events) != 1 || f.showCalls != showsBefore {
			t.Fatalf("failed close looped: events=%d showCalls=%d->%d", len(f.events), showsBefore, f.showCalls)
		}
	})
}

func TestOrphanSweepOpenBeadWithMergedPR(t *testing.T) {
	now := t0
	cfg := defaultConfig()
	cfg.BranchPrefix = "topic/"
	prURL := "https://github.com/acme/app/pull/88"
	openOwn := Bead{
		ID:        "task-open",
		Title:     "Fix parser edge case",
		Type:      "task",
		Status:    StatusOpen,
		Notes:     "PR: " + prURL,
		UpdatedAt: now.Add(-20 * time.Minute),
	}
	openCited := Bead{
		ID:        "task-cited",
		Title:     "Follow-up after another PR",
		Type:      "task",
		Status:    StatusOpen,
		Notes:     "Follow-up to https://github.com/acme/app/pull/89",
		UpdatedAt: now.Add(-20 * time.Minute),
	}
	f := newFakeOrphanEnv(4, openOwn, openCited)
	f.notedPRs[prURL] = PR{Number: 88, URL: prURL, State: "MERGED", Head: "topic/task-open-fix", MergedAt: now.Add(-15 * time.Minute)}
	f.notedPRs["https://github.com/acme/app/pull/89"] = PR{Number: 89, URL: "https://github.com/acme/app/pull/89", State: "MERGED", MergedAt: now.Add(-30 * time.Minute)}

	st := TickerState{}
	orphanFinishPass(cfg, f.env(), &st, []Bead{openOwn, openCited}, nil, true, now)
	if len(f.closed) != 1 || !strings.HasPrefix(f.closed[0], "task-open:") {
		t.Fatalf("closed = %v, want only task-open closed", f.closed)
	}
	if len(f.events) != 1 || f.events[0].Bead != "task-open" || f.events[0].Kind != EventAutoClosed {
		t.Fatalf("events = %+v, want only task-open auto_closed", f.events)
	}
}

func TestOrphanSweepRollingOutCompletedRunsAndGuards(t *testing.T) {
	now := t0
	cfg := defaultConfig()
	cfg.BranchPrefix = "topic/"
	prURL := "https://github.com/acme/app/pull/91"
	run1URL := "https://github.com/acme/app/actions/runs/5001"
	run2URL := "https://github.com/acme/app/actions/runs/5002"

	rollingDone := Bead{
		ID:        "task-roll-done",
		Title:     "Roll out config update",
		Type:      "task",
		Status:    StatusInProgress,
		Labels:    []string{LabelRollingOut},
		Notes:     "PR: " + prURL + "\nDeploy: " + run1URL + "\nVerify: " + run2URL,
		UpdatedAt: now.Add(-25 * time.Minute),
	}
	rollingInProgress := Bead{
		ID:        "task-roll-running",
		Title:     "Still deploying",
		Type:      "task",
		Status:    StatusInProgress,
		Labels:    []string{LabelRollingOut},
		Notes:     "PR: " + prURL + "\nDeploy: https://github.com/acme/app/actions/runs/5003",
		UpdatedAt: now.Add(-25 * time.Minute),
	}
	rollingNoRuns := Bead{
		ID:        "task-roll-noruns",
		Title:     "Manual soak rollout",
		Type:      "task",
		Status:    StatusInProgress,
		Labels:    []string{LabelRollingOut},
		Notes:     "PR: " + prURL,
		UpdatedAt: now.Add(-25 * time.Minute),
	}
	rollingFailed := Bead{
		ID:        "task-roll-failed",
		Title:     "Deploy failed",
		Type:      "task",
		Status:    StatusInProgress,
		Labels:    []string{LabelRollingOut},
		Notes:     "PR: " + prURL + "\nDeploy: https://github.com/acme/app/actions/runs/5004",
		UpdatedAt: now.Add(-25 * time.Minute),
	}

	all := []Bead{rollingDone, rollingInProgress, rollingNoRuns, rollingFailed}
	f := newFakeOrphanEnv(5, all...)
	mergedPR := PR{Number: 91, URL: prURL, State: "MERGED", MergedAt: now.Add(-30 * time.Minute)}
	st := TickerState{
		PRs: map[string]PR{
			"task-roll-done":    mergedPR,
			"task-roll-running": mergedPR,
			"task-roll-noruns":  mergedPR,
			"task-roll-failed":  mergedPR,
		},
		Runs: map[string][]Run{
			"task-roll-done": {
				{ID: 5001, Workflow: "Deploy", Status: "completed", Conclusion: "success", URL: run1URL, CreatedAt: now.Add(-20 * time.Minute)},
				{ID: 5002, Workflow: "Smoke", Status: "completed", Conclusion: "success", URL: run2URL, CreatedAt: now.Add(-15 * time.Minute)},
			},
			"task-roll-running": {
				{ID: 5003, Workflow: "Deploy", Status: "in_progress", URL: "https://github.com/acme/app/actions/runs/5003", CreatedAt: now.Add(-20 * time.Minute)},
			},
			"task-roll-failed": {
				{ID: 5004, Workflow: "Deploy", Status: "completed", Conclusion: "failure", URL: "https://github.com/acme/app/actions/runs/5004", CreatedAt: now.Add(-20 * time.Minute)},
			},
		},
	}

	orphanFinishPass(cfg, f.env(), &st, all, nil, true, now)
	if len(f.closed) != 0 {
		t.Fatalf("rolling-out beads must never auto-close without verifier: %v", f.closed)
	}
	if len(f.events) != 2 {
		t.Fatalf("events = %+v, want exactly 2 orphan_finish events (done + failed)", f.events)
	}
	byBead := map[string]Event{}
	for _, ev := range f.events {
		if ev.Kind != EventOrphanFinish {
			t.Fatalf("unexpected event kind %s: %+v", ev.Kind, ev)
		}
		byBead[ev.Bead] = ev
	}
	doneEv, ok := byBead["task-roll-done"]
	if !ok {
		t.Fatalf("missing orphan_finish for task-roll-done: %+v", f.events)
	}
	for _, want := range []string{"task-roll-done", "PR #91", "Deploy #5001", "Smoke #5002", "quartermaster close task-roll-done", "quartermaster resume task-roll-done"} {
		if !strings.Contains(doneEv.Summary, want) {
			t.Errorf("task-roll-done summary missing %q: %s", want, doneEv.Summary)
		}
	}
	failEv, ok := byBead["task-roll-failed"]
	if !ok {
		t.Fatalf("missing orphan_finish for task-roll-failed: %+v", f.events)
	}
	for _, want := range []string{"task-roll-failed", "Deploy #5004", "failure", "quartermaster resume task-roll-failed"} {
		if !strings.Contains(failEv.Summary, want) {
			t.Errorf("task-roll-failed summary missing %q: %s", want, failEv.Summary)
		}
	}

	// Second sweep: deduplicated without infinite retries.
	orphanFinishPass(cfg, f.env(), &st, all, nil, true, now.Add(6*time.Minute))
	if len(f.events) != 2 {
		t.Fatalf("rolling-out sweep duplicated notifications: %+v", f.events)
	}
}

func TestOrphanSweepSkipsLiveAgentsGraceWindowAndRespectsMaxPerPass(t *testing.T) {
	now := t0
	cfg := defaultConfig()
	cfg.BranchPrefix = "topic/"
	cfg.OrphanFinish.MaxPerPass = 2

	mkMerged := func(id string, age time.Duration) (Bead, PR) {
		u := fmt.Sprintf("https://github.com/acme/app/pull/%s", id[len(id)-1:])
		return Bead{
			ID:        id,
			Title:     "Small fix " + id,
			Type:      "task",
			Status:    StatusInProgress,
			Notes:     "PR: " + u,
			UpdatedAt: now.Add(-age),
		}, PR{Number: 1, URL: u, State: "MERGED", MergedAt: now.Add(-age)}
	}

	liveBead, livePR := mkMerged("task-1", 30*time.Minute)
	freshBead, freshPR := mkMerged("task-2", 5*time.Minute) // under 10m grace
	recentMergeBead, recentMergePR := mkMerged("task-3", 30*time.Minute)
	recentMergePR.MergedAt = now.Add(-3 * time.Minute) // PR merged only 3m ago
	b4, p4 := mkMerged("task-4", 40*time.Minute)
	b5, p5 := mkMerged("task-5", 35*time.Minute)
	b6, p6 := mkMerged("task-6", 20*time.Minute)
	heldBead, heldPR := mkMerged("task-7", 45*time.Minute)
	notesRemainBead, notesRemainPR := mkMerged("task-8", 42*time.Minute)
	notesRemainBead.Notes += "\nRemaining: update dashboard"

	all := []Bead{liveBead, freshBead, recentMergeBead, b4, b5, b6, heldBead, notesRemainBead}
	f := newFakeOrphanEnv(5, all...)
	f.held["task-7"] = true
	agents := map[string]Agent{"task-1": {Name: "task-1", Status: "idle"}}
	st := TickerState{
		PRs: map[string]PR{
			"task-1": livePR,
			"task-2": freshPR,
			"task-3": recentMergePR,
			"task-4": p4,
			"task-5": p5,
			"task-6": p6,
			"task-7": heldPR,
			"task-8": notesRemainPR,
		},
	}

	// Incomplete agent discovery skips the entire sweep without advancing LastOrphanFinish.
	orphanFinishPass(cfg, f.env(), &st, all, agents, false, now)
	if len(f.closed) != 0 || len(f.events) != 0 || !st.LastOrphanFinish.IsZero() {
		t.Fatalf("incomplete agent discovery must skip sweep: closed=%v events=%+v last=%v", f.closed, f.events, st.LastOrphanFinish)
	}

	// Complete pass 1: acts on at most MaxPerPass=2 oldest eligible beads (task-8 notify + task-4 close).
	orphanFinishPass(cfg, f.env(), &st, all, agents, true, now)
	if len(f.events) != 2 {
		t.Fatalf("pass 1 events = %+v, want 2 bounded by max_per_pass", f.events)
	}
	// task-8 (42m old, work remains) must be orphan_finish, NOT auto-closed.
	if f.events[0].Bead != "task-8" || f.events[0].Kind != EventOrphanFinish {
		t.Fatalf("expected task-8 orphan_finish first, got %+v", f.events[0])
	}
	// task-4 (40m old) auto-closed.
	if f.events[1].Bead != "task-4" || f.events[1].Kind != EventAutoClosed {
		t.Fatalf("expected task-4 auto_closed second, got %+v", f.events[1])
	}
	// Live agent, grace-window, recent-merge, and held beads were all skipped.
	for _, ev := range f.events {
		if slices.Contains([]string{"task-1", "task-2", "task-3", "task-7"}, ev.Bead) {
			t.Fatalf("guarded bead %s should not have been acted on: %+v", ev.Bead, ev)
		}
	}

	// Pass 2 picks up the remaining eligible beads (task-5 and task-6) without repeating task-8 or task-4.
	orphanFinishPass(cfg, f.env(), &st, all, agents, true, now.Add(6*time.Minute))
	if len(f.events) != 4 {
		t.Fatalf("pass 2 events = %+v, want 4 total", f.events)
	}
	if f.events[2].Bead != "task-5" || f.events[2].Kind != EventAutoClosed || f.events[3].Bead != "task-6" || f.events[3].Kind != EventAutoClosed {
		t.Fatalf("pass 2 acted on unexpected beads: %+v", f.events[2:])
	}
}

func TestOrphanFinishInboxReconciliation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	ev := Event{Bead: "task-1", Kind: EventOrphanFinish, Summary: "orphan merged bead task-1", At: t0}
	it := inboxFixture(t, ev)
	if it.Clears == nil || it.Clears.Kind != "orphan_finish" {
		t.Fatalf("orphan_finish clears condition = %+v, want kind=orphan_finish", it.Clears)
	}

	// Still open & agentless -> stays unread.
	if err := reconcileInbox(nil, func(id string) (Bead, error) {
		return Bead{ID: id, Status: StatusOpen}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 1 {
		t.Fatalf("open agentless orphan_finish should stay unread, got %+v", items)
	}

	// Still in_progress & agentless -> stays unread.
	if err := reconcileInbox([]Thread{{Bead: Bead{ID: "task-1", Status: StatusInProgress}}}, nil); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 1 {
		t.Fatalf("in_progress agentless orphan_finish should stay unread, got %+v", items)
	}

	// Once a live agent picks it up -> auto-archived.
	if err := reconcileInbox([]Thread{{Bead: Bead{ID: "task-1", Status: StatusInProgress}, Agent: &Agent{Name: "task-1", Status: "working"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("orphan_finish should archive once a live agent starts, got %+v", items)
	}

	// Also auto-archives when the bead closes.
	inboxFixture(t, Event{Bead: "task-2", Kind: EventOrphanFinish, Summary: "orphan task-2", At: t0.Add(time.Second)})
	if err := reconcileInbox(nil, func(id string) (Bead, error) {
		return Bead{ID: id, Status: StatusClosed}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("orphan_finish should archive once closed, got %+v", items)
	}
}

func TestTickerOnceOrphanFinisherEndToEnd(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("PATH", dir)

	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	prMerged := PR{Number: 101, URL: "https://github.com/acme/app/pull/101", Head: "topic/task-done-fix", State: "MERGED", MergedAt: now.Add(-20 * time.Minute)}
	prEpic := PR{Number: 102, URL: "https://github.com/acme/app/pull/102", Head: "topic/epic-1-part1", State: "MERGED", MergedAt: now.Add(-20 * time.Minute)}
	runURL := "https://github.com/acme/app/actions/runs/9001"
	prRoll := PR{Number: 103, URL: "https://github.com/acme/app/pull/103", Head: "topic/task-roll-deploy", State: "MERGED", MergedAt: now.Add(-25 * time.Minute)}

	beads := []Bead{
		{ID: "task-done", Title: "Simple fix", Type: "task", Status: StatusInProgress, Notes: "PR: " + prMerged.URL, UpdatedAt: now.Add(-20 * time.Minute)},
		{ID: "epic-1", Title: "Parent epic", Type: "epic", Status: StatusInProgress, Notes: "PR: " + prEpic.URL, UpdatedAt: now.Add(-20 * time.Minute)},
		{ID: "epic-1.1", Title: "Open child", Type: "task", Status: StatusOpen, Parent: "epic-1", UpdatedAt: now.Add(-15 * time.Minute)},
		{ID: "task-roll", Title: "Deploy rollout", Type: "task", Status: StatusInProgress, Labels: []string{LabelRollingOut}, Notes: "PR: " + prRoll.URL + "\nRun: " + runURL, UpdatedAt: now.Add(-20 * time.Minute)},
	}
	rawBeads, err := json.Marshal(beads)
	if err != nil {
		t.Fatal(err)
	}
	beadsFile := filepath.Join(dir, "beads.json")
	if err := os.WriteFile(beadsFile, rawBeads, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_BEADS_FILE", beadsFile)
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\nif [ \"$1\" = list ]; then IFS= read -r line < \"$FIXTURE_BEADS_FILE\"; printf '%s\\n' \"$line\"; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	cfg.Repo = t.TempDir()
	cfg.BranchPrefix = "topic/"
	cfg.Nudge, cfg.VerifyMinutes = false, 0
	cfg.Autonomy.Level = 4
	f := newFakeOrphanEnv(4, beads...)
	fEnv := f.env()
	fEnv.event = writeEvent

	var logs bytes.Buffer
	tk := newTicker(cfg, log.New(&logs, "", 0))
	tk.orphanEnv = &fEnv
	tk.now = func() time.Time { return now }

	st := loadState()
	st.LastGH = now
	st.PRs["task-done"] = prMerged
	st.PRs["epic-1"] = prEpic
	st.PRs["task-roll"] = prRoll
	st.Runs = map[string][]Run{
		"task-roll": {{ID: 9001, Workflow: "Deploy", Status: "completed", Conclusion: "success", URL: runURL, CreatedAt: now.Add(-15 * time.Minute)}},
	}

	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// epic-1 must NOT be classified as merged in st.Threads.
	if snap := st.Threads["epic-1"]; snap.Group == GroupMerged {
		t.Fatalf("epic-1 with open child classified as GroupMerged in ticker snapshot: %+v", snap)
	}
	// task-done auto-closed & resolved; task-roll surfaced as orphan_finish; epic-1 untouched.
	if len(f.closed) != 1 || !strings.HasPrefix(f.closed[0], "task-done:") {
		t.Fatalf("closed = %v, want only task-done", f.closed)
	}
	items, err := readInbox()
	if err != nil {
		t.Fatal(err)
	}
	var gotAutoClosed, gotOrphanFinish bool
	for _, it := range items {
		if it.Bead == "task-done" && it.Kind == EventAutoClosed {
			gotAutoClosed = true
		}
		if it.Bead == "task-roll" && it.Kind == EventOrphanFinish {
			gotOrphanFinish = true
		}
		if it.Bead == "epic-1" && (it.Kind == EventAutoClosed || it.Kind == EventOrphanFinish) {
			t.Fatalf("epic-1 should not appear in orphan sweep events: %+v", it)
		}
	}
	if !gotAutoClosed || !gotOrphanFinish {
		t.Fatalf("inbox items = %+v, want auto_closed for task-done and orphan_finish for task-roll", items)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	reloaded := loadState()
	if reloaded.LastOrphanFinish.IsZero() || reloaded.Orphans["task-roll"].Action != orphanActionNotified {
		t.Fatalf("reloaded state lost orphan finisher marks: %+v", reloaded)
	}

	// Next tick before interval elapses does not re-run; after interval it stays deduplicated.
	tk.now = func() time.Time { return now.Add(6 * time.Minute) }
	if err := tk.once(&reloaded); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	items2, _ := readInbox()
	orphanCount := 0
	for _, it := range items2 {
		if it.Bead == "task-roll" && it.Kind == EventOrphanFinish {
			orphanCount++
		}
	}
	if orphanCount != 1 {
		t.Fatalf("expected 1 deduplicated orphan_finish item for task-roll, got %d: %+v", orphanCount, items2)
	}
}

func TestOrphanSweepReopenedBeadAndNotedRunsWithoutRollingOutLabel(t *testing.T) {
	now := t0
	cfg := defaultConfig()
	cfg.BranchPrefix = "topic/"
	prURL := "https://github.com/acme/app/pull/120"
	runURL := "https://github.com/acme/app/actions/runs/7701"

	// A bead previously auto-closed by the orphan finisher and reopened must NOT be auto-closed again.
	reopened := Bead{
		ID:        "task-reopened",
		Title:     "Reopened task",
		Type:      "task",
		Status:    StatusInProgress,
		Notes:     "PR: " + prURL + "\nAUTO: [ticker L4 C3] orphan finisher closed after PR #120 (" + prURL + ") merged",
		UpdatedAt: now.Add(-20 * time.Minute),
	}
	// A non-rolling-out bead with a noted post-merge run still in progress waits; once completed it surfaces orphan_finish (never auto-closes).
	withRun := Bead{
		ID:        "task-with-run",
		Title:     "Post-merge job",
		Type:      "task",
		Status:    StatusInProgress,
		Notes:     "PR: " + prURL + "\nRun: " + runURL,
		UpdatedAt: now.Add(-20 * time.Minute),
	}
	// An open rolling-out bead fetches its PR and run on demand and caches them.
	openRoll := Bead{
		ID:        "task-open-roll",
		Title:     "Open rolling-out task",
		Type:      "task",
		Status:    StatusOpen,
		Labels:    []string{LabelRollingOut},
		Notes:     "PR: " + prURL + "\nRun: https://github.com/acme/app/actions/runs/7702",
		UpdatedAt: now.Add(-20 * time.Minute),
	}

	all := []Bead{reopened, withRun, openRoll}
	f := newFakeOrphanEnv(5, all...)
	mergedPR := PR{Number: 120, URL: prURL, State: "MERGED", MergedAt: now.Add(-25 * time.Minute)}
	f.notedPRs[prURL] = mergedPR
	f.runs["7702"] = Run{ID: 7702, Workflow: "Canary", Status: "completed", Conclusion: "success", URL: "https://github.com/acme/app/actions/runs/7702", CreatedAt: now.Add(-15 * time.Minute)}
	st := TickerState{
		PRs: map[string]PR{
			"task-reopened": mergedPR,
			"task-with-run": mergedPR,
		},
		Runs: map[string][]Run{
			"task-with-run": {{ID: 7701, Workflow: "PostMerge", Status: "in_progress", URL: runURL, CreatedAt: now.Add(-15 * time.Minute)}},
		},
	}

	orphanFinishPass(cfg, f.env(), &st, all, nil, true, now)
	if len(f.closed) != 0 {
		t.Fatalf("none of these beads should auto-close: %v", f.closed)
	}
	if len(f.events) != 2 {
		t.Fatalf("events = %+v, want 2 (task-reopened + task-open-roll; task-with-run still running)", f.events)
	}
	for _, ev := range f.events {
		if ev.Kind != EventOrphanFinish {
			t.Fatalf("expected orphan_finish, got %+v", ev)
		}
		if ev.Bead == "task-with-run" {
			t.Fatalf("task-with-run should wait while run is in_progress: %+v", ev)
		}
	}
	if f.prLookups != 1 {
		t.Fatalf("prLookups = %d, want 1", f.prLookups)
	}

	// Now task-with-run's run finishes -> surfaces orphan_finish once; openRoll PR is cached so no extra PR lookup.
	st.Runs["task-with-run"] = []Run{{ID: 7701, Workflow: "PostMerge", Status: "completed", Conclusion: "success", URL: runURL, CreatedAt: now.Add(-15 * time.Minute)}}
	orphanFinishPass(cfg, f.env(), &st, all, nil, true, now.Add(6*time.Minute))
	if len(f.closed) != 0 {
		t.Fatalf("verifier-scoped bead should not auto-close: %v", f.closed)
	}
	if len(f.events) != 3 || f.events[2].Bead != "task-with-run" || f.events[2].Kind != EventOrphanFinish {
		t.Fatalf("expected orphan_finish for task-with-run after run completed, got %+v", f.events)
	}
	if f.prLookups != 1 {
		t.Fatalf("merged PR should stay cached in st.Noted, prLookups = %d", f.prLookups)
	}
}
