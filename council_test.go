package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCouncilEnv struct {
	mu             sync.Mutex
	beads          map[string]Bead
	closeErr       map[string]error
	resolveErr     map[string]error
	dispatchErr    map[string]error
	createErr      error
	enforceErr     error
	enforceFn      func(b Bead, action, by string) (string, error)
	shuffle        func(n int, swap func(i, j int))
	now            time.Time
	nextCreatedSeq int

	created      []BeadCreateOpts
	createdIDs   []string
	closed       []struct{ Bead, Reason string }
	resolved     []string
	notes        []struct{ Bead, Text string }
	dispatches   []struct{ Bead, Role, By string }
	enforceCalls []struct{ Bead, Action, By string }
	events       []Event
	journal      []journalRecord
	logs         []string
}

func newFakeCouncilEnv(beads ...Bead) *fakeCouncilEnv {
	m := make(map[string]Bead, len(beads))
	for _, b := range beads {
		m[b.ID] = b
	}
	return &fakeCouncilEnv{
		beads:       m,
		closeErr:    map[string]error{},
		resolveErr:  map[string]error{},
		dispatchErr: map[string]error{},
		now:         time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC),
		// Identity shuffle by default for deterministic assertions unless a
		// test explicitly exercises non-trivial shuffling.
		shuffle: func(n int, swap func(i, j int)) {},
	}
}

func (f *fakeCouncilEnv) env() councilEnv {
	return councilEnv{
		show: func(id string) (Bead, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			b, ok := f.beads[id]
			if !ok {
				return Bead{}, fmt.Errorf("%w: %s", errBeadNotFound, id)
			}
			return b, nil
		},
		note: func(id, text string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.notes = append(f.notes, struct{ Bead, Text string }{id, text})
			if b, ok := f.beads[id]; ok {
				if b.Notes != "" {
					b.Notes += "\n"
				}
				b.Notes += text
				f.beads[id] = b
			}
			return nil
		},
		createBead: func(opts BeadCreateOpts) (Bead, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.createErr != nil {
				return Bead{}, f.createErr
			}
			f.nextCreatedSeq++
			id := fmt.Sprintf("helper-%d", f.nextCreatedSeq)
			f.created = append(f.created, opts)
			f.createdIDs = append(f.createdIDs, id)
			b := Bead{
				ID:          id,
				Title:       opts.Title,
				Type:        opts.Type,
				Priority:    opts.Priority,
				Parent:      opts.Parent,
				Labels:      slices.Clone(opts.Labels),
				Description: opts.Description,
				Status:      StatusOpen,
			}
			f.beads[id] = b
			return b, nil
		},
		closeBead: func(id, reason string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.closeErr[id]; err != nil {
				return err
			}
			f.closed = append(f.closed, struct{ Bead, Reason string }{id, reason})
			if b, ok := f.beads[id]; ok {
				b.Status = StatusClosed
				b.CloseReason = reason
				f.beads[id] = b
			}
			return nil
		},
		resolveHelper: func(id string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.resolveErr[id]; err != nil {
				return "", err
			}
			f.resolved = append(f.resolved, id)
			return "removed role directory /state/roles/" + id, nil
		},
		dispatchHelper: func(id, role, by string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.dispatchErr[role]; err != nil {
				return "", err
			}
			if err := f.dispatchErr[id]; err != nil {
				return "", err
			}
			f.dispatches = append(f.dispatches, struct{ Bead, Role, By string }{id, role, by})
			if b, ok := f.beads[id]; ok {
				b.Status = StatusInProgress
				f.beads[id] = b
			}
			return "started " + id, nil
		},
		enforce: func(b Bead, action, by string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.enforceCalls = append(f.enforceCalls, struct{ Bead, Action, By string }{b.ID, action, by})
			if f.enforceFn != nil {
				return f.enforceFn(b, action, by)
			}
			if f.enforceErr != nil {
				return "", f.enforceErr
			}
			if by == "TJ" {
				return "", nil
			}
			return fmt.Sprintf("AUTO: [%s L4 C3]", by), nil
		},
		event: func(e Event) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.events = append(f.events, e)
			return nil
		},
		journal: func(r journalRecord) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.journal = append(f.journal, r)
			return nil
		},
		now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
		shuffle: func(n int, swap func(i, j int)) {
			if f.shuffle != nil {
				f.shuffle(n, swap)
			}
		},
		logf: func(format string, args ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.logs = append(f.logs, fmt.Sprintf(format, args...))
		},
	}
}

func testCouncilConfig() Config {
	cfg := defaultConfig()
	cfg.Repo = "/tmp/example-repo"
	cfg.Repos = map[string]string{
		"demo": "/tmp/demo-repo",
	}
	boolFalse := false
	cfg.Roles = map[string]RoleConfig{
		"planner-a": {
			Worktree:     &boolFalse,
			Agent:        "pi",
			Model:        "provider/model-alpha-1",
			Brief:        roleInvestigator,
			ExcludeTools: []string{"edit", "write"},
		},
		"planner-b": {
			Worktree:     &boolFalse,
			Agent:        "pi",
			Model:        "provider/model-beta-2",
			Brief:        roleInvestigator,
			ExcludeTools: []string{"edit", "write"},
		},
		"planner-c": {
			Worktree:     &boolFalse,
			Agent:        "pi",
			Model:        "provider/model-gamma-3",
			Brief:        roleInvestigator,
			ExcludeTools: []string{"edit", "write"},
		},
		"planner-chair": {
			Worktree:     &boolFalse,
			Agent:        "pi",
			Model:        "provider/model-chair-9",
			Brief:        roleInvestigator,
			ExcludeTools: []string{"edit", "write"},
		},
		"custom-worktree-role": {
			// Intentionally leaves Worktree unset (defaults to true for custom roles)
			// to verify council helper beads still force worktree = false.
			Agent: "pi",
			Model: "provider/model-custom",
		},
	}
	cfg.Councils = map[string]CouncilConfig{
		"default": {
			Members:        []string{"planner-a", "planner-b", "planner-c"},
			Chair:          "planner-chair",
			MinQuorum:      2,
			TimeoutMinutes: 20,
		},
		"pair": {
			Members: []string{"planner-a", "planner-b"},
			Chair:   "planner-chair",
		},
	}
	return cfg
}

func TestCouncilStartFanOutAutonomyAndDuplicateRefusal(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := testCouncilConfig()
	target := Bead{
		ID:          "task-100",
		Title:       "Design distributed rate limiter",
		Description: "Need a multi-region token bucket design.\n\n## Acceptance\n- Sub-5ms p99",
		Status:      StatusOpen,
		Priority:    2,
		Parent:      "epic-10",
		Labels:      []string{"repo:demo"},
	}

	t.Run("class 3 autonomy denial blocks non-TJ start", func(t *testing.T) {
		f := newFakeCouncilEnv(target)
		f.enforceErr = fmt.Errorf("%w: dispatch (class 3) is above task-100's level 2", errNotAutonomous)
		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "pair", By: "coordinator"}); !errors.Is(err, errNotAutonomous) {
			t.Fatalf("startCouncil err = %v, want errNotAutonomous", err)
		}
		if len(f.created) != 0 {
			t.Fatalf("created %d helper beads on denied start, want 0", len(f.created))
		}
	})

	t.Run("spawns isolated sibling helper beads and refuses duplicate active council", func(t *testing.T) {
		f := newFakeCouncilEnv(target)
		// Reverse order shuffle to prove anonymous labels are decoupled from member order.
		f.shuffle = func(n int, swap func(i, j int)) {
			for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
				swap(i, j)
			}
		}

		res, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "pair", By: "coordinator"})
		if err != nil {
			t.Fatalf("startCouncil failed: %v", err)
		}
		if res.State.Phase != councilPhaseFanout {
			t.Fatalf("Phase = %q, want %q", res.State.Phase, councilPhaseFanout)
		}
		if !res.State.Anonymize || !res.State.AutoCleanup {
			t.Fatalf("expected Anonymize=true and AutoCleanup=true, got %+v", res.State)
		}
		if res.State.MinQuorum != 2 || res.State.TimeoutMinutes != 20 {
			t.Fatalf("quorum/timeout = %d/%d, want 2/20", res.State.MinQuorum, res.State.TimeoutMinutes)
		}
		if len(f.created) != 2 || len(f.dispatches) != 2 {
			t.Fatalf("created=%d dispatches=%d, want 2/2", len(f.created), len(f.dispatches))
		}
		// Reversed shuffle assigns Plan B to planner-a and Plan A to planner-b.
		if res.State.Members[0].Label != "Plan B" || res.State.Members[1].Label != "Plan A" {
			t.Fatalf("unexpected shuffled labels: %+v", res.State.Members)
		}

		for i, c := range f.created {
			if c.Parent != "epic-10" {
				t.Errorf("member %d Parent = %q, want sibling epic-10 (never target bead)", i, c.Parent)
			}
			if c.Parent == target.ID {
				t.Errorf("member %d must never be parented under target bead %s", i, target.ID)
			}
			wantRole := res.State.Members[i].Role
			for _, lbl := range []string{roleLabel + wantRole, councilLabelPrefix + target.ID, councilRoleLabel + councilRoleMember, repoLabel + "demo"} {
				if !slices.Contains(c.Labels, lbl) {
					t.Errorf("member %d labels %v missing %q", i, c.Labels, lbl)
				}
			}
			for _, wantText := range []string{
				"Architecture & Trade-offs",
				"Hidden Risks / Failure Modes / Blind Spots",
				"Proposed File-Disjoint Bead DAG",
				"## Boundaries",
				"## Acceptance",
				"Open Decisions / Unknowns",
				"zero cross-talk",
				"quartermaster council submit " + target.ID + " --member " + wantRole + " --file -",
			} {
				if !strings.Contains(c.Description, wantText) {
					t.Errorf("member %d brief missing %q", i, wantText)
				}
			}
		}

		// Duplicate active council is refused.
		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "pair", By: "TJ"}); err == nil || !strings.Contains(err.Error(), "already active") {
			t.Fatalf("duplicate start err = %v, want already active", err)
		}
	})

	t.Run("council helper beads force worktree=false and read-only brief even for custom roles", func(t *testing.T) {
		helper := Bead{
			ID:     "helper-custom",
			Title:  "Council plan",
			Labels: []string{"role:custom-worktree-role", "council:task-100", "council-role:member"},
		}
		eff, err := cfg.resolveRoleForBead(helper, "")
		if err != nil {
			t.Fatalf("resolveRoleForBead failed: %v", err)
		}
		if eff.Worktree {
			t.Fatal("expected council helper bead to force Worktree=false")
		}
		prompt := briefForRole(helper, eff, "", "/state/roles/helper-custom", "/tmp/demo-repo", "", "", "")
		if !strings.Contains(prompt, "strictly read-only investigator") {
			t.Fatalf("expected investigator brief for council helper, got:\n%s", prompt)
		}
		if strings.Contains(prompt, "gh pr merge") {
			t.Fatalf("council helper brief must not include PR-authoring/auto-merge boilerplate")
		}
	})
}

func TestCouncilEndToEndStrictAnonymizationAttributionAndAutoCleanup(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := testCouncilConfig()
	target := Bead{
		ID:          "task-200",
		Title:       "Migrate session store to atomic files",
		Description: "Replace non-atomic session writes with temp+rename.",
		Status:      StatusOpen,
		Priority:    1,
		Parent:      "epic-20",
		Labels:      []string{"repo:demo"},
	}
	f := newFakeCouncilEnv(target)
	// Swap indices so Plan A -> planner-b and Plan B -> planner-a.
	f.shuffle = func(n int, swap func(i, j int)) {
		if n == 2 {
			swap(0, 1)
		}
	}

	// Start council via CLI with --panel pair --by TJ.
	var out bytes.Buffer
	if err := councilCmd(cfg, f.env(), []string{"start", target.ID, "--panel", "pair", "--by", "TJ", "--json"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("council start CLI failed: %v", err)
	}
	var startRes CouncilStartResult
	if err := json.Unmarshal(out.Bytes(), &startRes); err != nil {
		t.Fatalf("decode start JSON: %v", err)
	}
	member1Helper := startRes.State.Members[0].HelperBead
	member2Helper := startRes.State.Members[1].HelperBead

	// Submit member 1 via stdin (--file -), member 2 via explicit file path.
	planAFile := filepath.Join(t.TempDir(), "plan-b-input.md")
	if err := os.WriteFile(planAFile, []byte("## Architecture\nUse fsynced temp files in dir.\n\n## Unique Catch\nSync parent directory fd after rename."), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-a", "--file", "-"}, strings.NewReader("## Architecture\nUse temp file + os.Rename in state dir.\n"), &out); err != nil {
		t.Fatalf("member planner-a stdin submit failed: %v", err)
	}
	out.Reset()
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-b", "--file", planAFile}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("member planner-b file submit failed: %v", err)
	}

	// Empty submission must be rejected.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-a", "--file", "-"}, strings.NewReader("   \n\t  "), &out); err == nil {
		t.Fatal("expected error for empty submission")
	}

	// Ticker fan-in runs once all members submit (even before deadline).
	lines := reconcileCouncilsPass(cfg, f.env(), f.now.Add(2*time.Minute))
	if len(lines) != 1 || !strings.Contains(lines[0], "fanned in") {
		t.Fatalf("reconcileCouncilsPass lines = %v, want 1 fan-in line", lines)
	}

	st, ok, err := loadCouncil(target.ID)
	if err != nil || !ok {
		t.Fatalf("loadCouncil failed: ok=%v err=%v", ok, err)
	}
	if st.Phase != councilPhaseSynthesizing {
		t.Fatalf("Phase = %q, want %q", st.Phase, councilPhaseSynthesizing)
	}
	if st.Chair.HelperBead == "" || st.Chair.Status != councilRunRunning {
		t.Fatalf("unexpected Chair state: %+v", st.Chair)
	}

	// Repeated ticker pass while synthesizing must be an idempotent no-op.
	if dupLines := reconcileCouncilsPass(cfg, f.env(), f.now.Add(3*time.Minute)); len(dupLines) != 0 {
		t.Fatalf("expected idempotent ticker pass while synthesizing, got %v", dupLines)
	}
	if len(f.created) != 3 {
		t.Fatalf("expected exactly 3 created beads (2 members + 1 chair), got %d", len(f.created))
	}

	// STRICT ANONYMIZATION LEAK CHECK on Chair bead and all Chair input files.
	chairCreate := f.created[2]
	if chairCreate.Parent != "epic-20" {
		t.Fatalf("chair Parent = %q, want epic-20", chairCreate.Parent)
	}
	for _, lbl := range []string{"role:planner-chair", "council:" + target.ID, "council-role:chair", "repo:demo"} {
		if !slices.Contains(chairCreate.Labels, lbl) {
			t.Fatalf("chair labels %v missing %q", chairCreate.Labels, lbl)
		}
	}

	forbiddenLeaks := []string{
		"planner-a",
		"planner-b",
		"model-alpha-1",
		"model-beta-2",
		member1Helper,
		member2Helper,
	}
	for _, forbidden := range forbiddenLeaks {
		if strings.Contains(chairCreate.Title, forbidden) {
			t.Errorf("ANONYMIZATION LEAK: chair title %q contains %q", chairCreate.Title, forbidden)
		}
		if strings.Contains(chairCreate.Description, forbidden) {
			t.Errorf("ANONYMIZATION LEAK: chair description contains %q", forbidden)
		}
	}
	for _, wantMandate := range []string{
		"Independently Fact-Check Against the Live Repo",
		"Compare Consensus vs Genuine Divergences",
		"Highlight Unique Catches",
		"Synthesize the Best Merged Canonical Plan + File-Disjoint Bead DAG",
		"quartermaster council submit " + target.ID + " --chair --file -",
		"Plan A",
		"Plan B",
	} {
		if !strings.Contains(chairCreate.Description, wantMandate) {
			t.Errorf("chair description missing %q", wantMandate)
		}
	}

	// Check assembled Chair input files under chair/: filenames and contents must leak nothing.
	chairFiles, err := filepath.Glob(filepath.Join(stateDir(), "councils", target.ID, "chair", "*.md"))
	if err != nil || len(chairFiles) != 2 {
		t.Fatalf("expected 2 chair input files, got %v (err=%v)", chairFiles, err)
	}
	for _, p := range chairFiles {
		base := filepath.Base(p)
		if base != "plan-a.md" && base != "plan-b.md" {
			t.Errorf("unexpected chair input filename %q", base)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range forbiddenLeaks {
			if strings.Contains(p, forbidden) || strings.Contains(string(raw), forbidden) {
				t.Errorf("ANONYMIZATION LEAK in %s: found %q", p, forbidden)
			}
		}
	}
	// Verify Plan A actually holds planner-b's content (shuffled).
	planAContent, err := os.ReadFile(filepath.Join(stateDir(), "councils", target.ID, "chair", "plan-a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(planAContent), "Sync parent directory fd after rename") {
		t.Fatalf("expected shuffled Plan A to hold planner-b's submission, got:\n%s", string(planAContent))
	}

	// Late member submit once synthesizing must be refused.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-a", "--file", "-"}, strings.NewReader("late edit"), &out); err == nil {
		t.Fatal("expected late member submit during synthesizing phase to fail")
	}

	// Simulate mid-turn Chair submission where Chair's resolve returns "still working"
	// while member helpers resolve immediately.
	chairHelper := st.Chair.HelperBead
	f.resolveErr[chairHelper] = fmt.Errorf("%s's agent is still working", chairHelper)

	synthBody := "# Canonical Merged Plan\n\n## Fact-Check\nVerified atomic writes.\n\n## Unique Catches\n- Plan A caught parent-directory fsync.\n"
	out.Reset()
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--chair", "--file", "-", "--json"}, strings.NewReader(synthBody), &out); err != nil {
		t.Fatalf("chair submit failed: %v", err)
	}
	var submitRes CouncilSubmitResult
	if err := json.Unmarshal(out.Bytes(), &submitRes); err != nil {
		t.Fatalf("decode chair submit JSON: %v", err)
	}
	if submitRes.State.Phase != councilPhaseDone {
		t.Fatalf("Phase = %q, want %q", submitRes.State.Phase, councilPhaseDone)
	}

	// Verify post-scoring Attribution Table was appended to synthesis.md.
	finalSynth, err := os.ReadFile(submitRes.ArtifactPath)
	if err != nil {
		t.Fatalf("read synthesis.md: %v", err)
	}
	synthStr := string(finalSynth)
	for _, want := range []string{
		"# Canonical Merged Plan",
		"## Attribution Table",
		"| Plan A | `planner-b` | `provider/model-beta-2` | submitted |",
		"| Plan B | `planner-a` | `provider/model-alpha-1` | submitted |",
		"Chair: `planner-chair` (model: `provider/model-chair-9`",
	} {
		if !strings.Contains(synthStr, want) {
			t.Errorf("synthesis.md missing %q\nfull content:\n%s", want, synthStr)
		}
	}

	// Verify target bead was noted with summary + synthesis path, and NEVER closed/resolved.
	targetAfter := f.beads[target.ID]
	if targetAfter.Status != StatusOpen {
		t.Fatalf("target bead status mutated to %q, want open", targetAfter.Status)
	}
	if !strings.Contains(targetAfter.Notes, "COUNCIL DONE:") || !strings.Contains(targetAfter.Notes, submitRes.ArtifactPath) {
		t.Fatalf("target bead missing COUNCIL DONE note with synthesis path:\n%s", targetAfter.Notes)
	}
	for _, c := range f.closed {
		if c.Bead == target.ID {
			t.Fatalf("target bead %s must never be closed by auto_cleanup", target.ID)
		}
	}
	for _, r := range f.resolved {
		if r == target.ID {
			t.Fatalf("target bead %s must never be resolved by auto_cleanup", target.ID)
		}
	}

	// Verify council_done inbox event emitted with bead_closed clear condition.
	if len(f.events) != 1 || f.events[0].Kind != EventCouncilDone || f.events[0].Bead != target.ID {
		t.Fatalf("events = %+v, want 1 council_done event for %s", f.events, target.ID)
	}
	if f.events[0].Clears == nil || f.events[0].Clears.Kind != "bead_closed" {
		t.Fatalf("council_done Clears = %+v, want bead_closed", f.events[0].Clears)
	}
	// Verify inboxObsolete keeps council_done alive while target bead is still open, and clears once closed.
	item := InboxItem{Event: f.events[0]}
	if inboxObsolete(item, map[string]Thread{target.ID: {Bead: targetAfter}}) {
		t.Fatal("council_done should stay active while target bead is open")
	}
	closedTarget := targetAfter
	closedTarget.Status = StatusClosed
	if !inboxObsolete(item, map[string]Thread{target.ID: {Bead: closedTarget}}) {
		t.Fatal("council_done should clear once target bead is closed")
	}

	// Verify all 3 helpers were closed, member helpers resolved immediately,
	// and mid-turn Chair helper was queued in PendingResolveBeads.
	if len(f.closed) != 3 {
		t.Fatalf("expected 3 closed helpers, got %+v", f.closed)
	}
	if len(f.resolved) != 2 {
		t.Fatalf("expected 2 immediate resolves (members), got %v", f.resolved)
	}
	if !slices.Equal(submitRes.State.PendingResolveBeads, []string{chairHelper}) {
		t.Fatalf("PendingResolveBeads = %v, want [%s]", submitRes.State.PendingResolveBeads, chairHelper)
	}

	// While chair is still working, ticker keeps chairHelper queued.
	if drain1 := reconcileCouncilsPass(cfg, f.env(), f.now.Add(5*time.Minute)); len(drain1) != 0 {
		t.Fatalf("expected no resolve while chair still working, got %v", drain1)
	}
	// Once chair goes idle, ticker drains PendingResolveBeads without duplicating council_done.
	delete(f.resolveErr, chairHelper)
	drain2 := reconcileCouncilsPass(cfg, f.env(), f.now.Add(6*time.Minute))
	if len(drain2) != 1 || !strings.Contains(drain2[0], chairHelper) {
		t.Fatalf("expected chairHelper deferred resolve drain, got %v", drain2)
	}
	finalSt, ok, err := loadCouncil(target.ID)
	if err != nil || !ok {
		t.Fatalf("loadCouncil after drain failed: %v", err)
	}
	if len(finalSt.PendingResolveBeads) != 0 {
		t.Fatalf("PendingResolveBeads not drained: %v", finalSt.PendingResolveBeads)
	}
	if len(f.events) != 1 {
		t.Fatalf("expected no duplicate council_done events on subsequent ticks, got %d", len(f.events))
	}
	// Verify councilContextLine renders compact summary.
	if line := councilContextLine(target.ID); !strings.Contains(line, "done 2/2") {
		t.Fatalf("councilContextLine = %q, want substring 'done 2/2'", line)
	}
}

func TestCouncilQuorumTimeoutFanInAndInsufficientQuorumFailure(t *testing.T) {
	t.Run("partial quorum waits before deadline then fans in at deadline", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := testCouncilConfig() // default panel: 3 members, min_quorum=2, timeout=20m
		target := Bead{ID: "task-300", Title: "Add canary probe", Status: StatusOpen, Priority: 2}
		f := newFakeCouncilEnv(target)

		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "default", By: "TJ"}); err != nil {
			t.Fatalf("startCouncil failed: %v", err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-a", Content: "Plan from first"}); err != nil {
			t.Fatal(err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-c", Content: "Plan from third"}); err != nil {
			t.Fatal(err)
		}

		// Before deadline: 2/3 submitted, 1 still running -> must stay in fanout.
		if lines := reconcileCouncilsPass(cfg, f.env(), f.now.Add(10*time.Minute)); len(lines) != 0 {
			t.Fatalf("unexpected early fan-in before deadline: %v", lines)
		}
		st, _, _ := loadCouncil(target.ID)
		if st.Phase != councilPhaseFanout {
			t.Fatalf("Phase = %q, want fanout before deadline", st.Phase)
		}

		// At deadline: 2 >= min_quorum(2) -> marks planner-b timed_out and spawns Chair with 2 plans.
		lines := reconcileCouncilsPass(cfg, f.env(), f.now.Add(20*time.Minute))
		if len(lines) != 1 || !strings.Contains(lines[0], "2/3 plans") {
			t.Fatalf("expected quorum fan-in at deadline, got %v", lines)
		}
		st, _, _ = loadCouncil(target.ID)
		if st.Phase != councilPhaseSynthesizing {
			t.Fatalf("Phase = %q, want synthesizing", st.Phase)
		}
		if st.Members[1].Status != councilRunTimedOut {
			t.Fatalf("unsubmitted member status = %q, want timed_out", st.Members[1].Status)
		}
		// Only 2 chair input files written (Plan A and Plan C); Plan B absent.
		chairFiles, err := filepath.Glob(filepath.Join(stateDir(), "councils", target.ID, "chair", "*.md"))
		if err != nil || len(chairFiles) != 2 {
			t.Fatalf("expected 2 chair input files for partial quorum, got %v", chairFiles)
		}
	})

	t.Run("insufficient quorum at deadline marks failed and emits council_failed", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := testCouncilConfig() // default panel: 3 members, min_quorum=2, timeout=20m
		target := Bead{ID: "task-301", Title: "Add shard rebalancer", Status: StatusOpen, Priority: 2}
		f := newFakeCouncilEnv(target)

		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "default", By: "TJ"}); err != nil {
			t.Fatalf("startCouncil failed: %v", err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-a", Content: "Only one plan"}); err != nil {
			t.Fatal(err)
		}

		lines := reconcileCouncilsPass(cfg, f.env(), f.now.Add(21*time.Minute))
		if len(lines) != 1 || !strings.Contains(lines[0], "failed") {
			t.Fatalf("expected failure line at deadline, got %v", lines)
		}
		st, _, _ := loadCouncil(target.ID)
		if st.Phase != councilPhaseFailed {
			t.Fatalf("Phase = %q, want failed", st.Phase)
		}
		if !strings.Contains(st.FailureReason, "quorum not met") {
			t.Fatalf("FailureReason = %q, want quorum not met", st.FailureReason)
		}
		if st.Members[1].Status != councilRunTimedOut || st.Members[2].Status != councilRunTimedOut {
			t.Fatalf("expected unsubmitted members timed_out, got %+v", st.Members)
		}
		if len(f.events) != 1 || f.events[0].Kind != EventCouncilFailed || f.events[0].Bead != target.ID {
			t.Fatalf("events = %+v, want 1 council_failed event", f.events)
		}
		if !strings.Contains(f.beads[target.ID].Notes, "COUNCIL FAILED:") {
			t.Fatalf("target bead missing COUNCIL FAILED note: %s", f.beads[target.ID].Notes)
		}
		// A failed council does NOT block starting a fresh council on the same bead.
		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "pair", By: "TJ"}); err != nil {
			t.Fatalf("restart after failed council should succeed: %v", err)
		}
	})
}

func TestCouncilNonAnonymizedAutoCleanupFalseCancelAndStatus(t *testing.T) {
	t.Run("anonymize=false exposes roles and auto_cleanup=false keeps helper beads open", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := testCouncilConfig()
		target := Bead{ID: "task-400", Title: "Tune cache eviction", Status: StatusOpen, Priority: 2}
		f := newFakeCouncilEnv(target)

		var out bytes.Buffer
		err := councilCmd(cfg, f.env(), []string{
			"start", target.ID,
			"--members", "planner-a,planner-b",
			"--chair", "planner-chair",
			"--anonymize=false",
			"--auto-cleanup=false",
			"--timeout", "15m",
			"--by", "TJ",
		}, strings.NewReader(""), &out)
		if err != nil {
			t.Fatalf("ad-hoc start failed: %v", err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-a", Content: "LRU plan"}); err != nil {
			t.Fatal(err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-b", Content: "TinyLFU plan"}); err != nil {
			t.Fatal(err)
		}
		reconcileCouncilsPass(cfg, f.env(), f.now.Add(time.Minute))

		// Chair description and files should include role names when anonymize=false.
		chairCreate := f.created[2]
		if !strings.Contains(chairCreate.Description, "role: `planner-a`") || !strings.Contains(chairCreate.Description, "role: `planner-b`") {
			t.Fatalf("expected role names in non-anonymized chair description:\n%s", chairCreate.Description)
		}
		planA, err := os.ReadFile(filepath.Join(stateDir(), "councils", target.ID, "chair", "plan-a.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(planA), "role: planner-a") {
			t.Fatalf("expected role header in non-anonymized plan-a.md, got:\n%s", string(planA))
		}

		// Submit chair synthesis -> helpers stay open when auto_cleanup=false.
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Chair: true, Content: "Pick TinyLFU."}); err != nil {
			t.Fatal(err)
		}
		if len(f.closed) != 0 || len(f.resolved) != 0 {
			t.Fatalf("expected zero cleanup with auto_cleanup=false, got closed=%v resolved=%v", f.closed, f.resolved)
		}

		// Status CLI (single + list, text + json).
		out.Reset()
		if err := councilCmd(cfg, f.env(), []string{"status", target.ID}, strings.NewReader(""), &out); err != nil {
			t.Fatalf("status single text failed: %v", err)
		}
		if !strings.Contains(out.String(), "phase:     done") || !strings.Contains(out.String(), "anonymize: false") {
			t.Fatalf("unexpected status text:\n%s", out.String())
		}
		out.Reset()
		if err := councilCmd(cfg, f.env(), []string{"status", "--json"}, strings.NewReader(""), &out); err != nil {
			t.Fatalf("status list json failed: %v", err)
		}
		var list []CouncilState
		if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 1 || list[0].Bead != target.ID {
			t.Fatalf("unexpected status list JSON: %s (err=%v)", out.String(), err)
		}
		out.Reset()
		if err := councilCmd(cfg, f.env(), []string{"status"}, strings.NewReader(""), &out); err != nil {
			t.Fatalf("status list text failed: %v", err)
		}
		if !strings.Contains(out.String(), target.ID) || !strings.Contains(out.String(), "BEAD") {
			t.Fatalf("unexpected status table:\n%s", out.String())
		}
	})

	t.Run("cancel closes and resolves active helpers without touching target bead", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := testCouncilConfig()
		target := Bead{ID: "task-401", Title: "Add audit log exporter", Status: StatusOpen, Priority: 2}
		f := newFakeCouncilEnv(target)

		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "pair", By: "TJ"}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := councilCmd(cfg, f.env(), []string{"cancel", target.ID, "--by", "coordinator", "--json"}, strings.NewReader(""), &out); err != nil {
			t.Fatalf("council cancel failed: %v", err)
		}
		var cancelRes CouncilCancelResult
		if err := json.Unmarshal(out.Bytes(), &cancelRes); err != nil {
			t.Fatalf("decode cancel JSON: %v", err)
		}
		if cancelRes.State.Phase != councilPhaseCanceled {
			t.Fatalf("Phase = %q, want canceled", cancelRes.State.Phase)
		}
		if len(cancelRes.HelpersClosed) != 2 || len(cancelRes.HelpersResolve) != 2 {
			t.Fatalf("expected 2 closed and resolved helpers, got closed=%v resolved=%v", cancelRes.HelpersClosed, cancelRes.HelpersResolve)
		}
		if f.beads[target.ID].Status != StatusOpen {
			t.Fatalf("target bead status = %q, want open", f.beads[target.ID].Status)
		}
		// Canceling an already canceled council fails closed.
		if _, err := cancelCouncil(f.env(), target.ID, "TJ"); err == nil || !strings.Contains(err.Error(), "not active") {
			t.Fatalf("double cancel err = %v, want not active", err)
		}
	})
}

func TestCouncilConcurrentSubmissionsRace(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := testCouncilConfig()
	target := Bead{ID: "task-500", Title: "Concurrent submission test", Status: StatusOpen, Priority: 2}
	f := newFakeCouncilEnv(target)

	if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "default", By: "TJ"}); err != nil {
		t.Fatalf("startCouncil failed: %v", err)
	}

	members := []string{"planner-a", "planner-b", "planner-c"}
	var wg sync.WaitGroup
	errs := make(chan error, len(members))
	for _, role := range members {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			_, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{
				Member:  r,
				Content: fmt.Sprintf("# Plan from %s\n\nDetailed architecture and bead DAG.", r),
			})
			errs <- err
		}(role)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent submit failed: %v", err)
		}
	}

	st, ok, err := loadCouncil(target.ID)
	if err != nil || !ok {
		t.Fatalf("loadCouncil failed: %v", err)
	}
	if st.submittedCount() != 3 {
		t.Fatalf("submittedCount = %d, want 3 after concurrent submissions", st.submittedCount())
	}
	for _, m := range st.Members {
		if m.Status != councilRunSubmitted || m.ArtifactPath == "" {
			t.Errorf("member %s not cleanly submitted: %+v", m.Role, m)
		}
	}

	// Now fan in and verify all 3 chair input files exist cleanly.
	lines := reconcileCouncilsPass(cfg, f.env(), f.now.Add(time.Minute))
	if len(lines) != 1 || !strings.Contains(lines[0], "3/3 plans") {
		t.Fatalf("expected 3/3 fan-in after concurrent submits, got %v", lines)
	}
}

func TestCouncilSubmitValidationAndEdgeCases(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := testCouncilConfig()
	target := Bead{ID: "task-600", Title: "Submit validation checks", Status: StatusOpen, Priority: 2}
	f := newFakeCouncilEnv(target)

	var out bytes.Buffer
	// Submitting before council starts fails closed.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-a"}, strings.NewReader("plan"), &out); err == nil || !strings.Contains(err.Error(), "no council found") {
		t.Fatalf("submit nonexistent err = %v, want no council found", err)
	}

	if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "pair", By: "TJ"}); err != nil {
		t.Fatal(err)
	}

	// Omitted --file defaults cleanly to stdin.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-a"}, strings.NewReader("# Plan via default stdin"), &out); err != nil {
		t.Fatalf("omitted --file stdin submit failed: %v", err)
	}

	// Explicit empty --file fails closed.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-b", "--file", ""}, strings.NewReader("plan"), &out); err == nil || !strings.Contains(err.Error(), "--file") {
		t.Fatalf("empty --file err = %v, want --file error", err)
	}

	// Neither --member nor --chair fails closed.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID}, strings.NewReader("plan"), &out); err == nil {
		t.Fatal("expected error when neither --member nor --chair is set")
	}

	// Both --member and --chair fails closed.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-a", "--chair"}, strings.NewReader("plan"), &out); err == nil {
		t.Fatal("expected error when both --member and --chair are set")
	}

	// Unknown member fails closed.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--member", "planner-c"}, strings.NewReader("plan"), &out); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("unknown member err = %v, want not a member", err)
	}

	// Chair submit during fanout phase fails closed.
	if err := councilCmd(cfg, f.env(), []string{"submit", target.ID, "--chair"}, strings.NewReader("too early"), &out); err == nil || !strings.Contains(err.Error(), "must be synthesizing") {
		t.Fatalf("early chair submit err = %v, want phase error", err)
	}
}

func TestCouncilStartPartialDispatchFailureAndChairMemberOverlap(t *testing.T) {
	t.Run("fails closed and cleans up when started members fall below min_quorum", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := testCouncilConfig() // pair: 2 members, min_quorum=2
		target := Bead{ID: "task-610", Title: "Strict quorum dispatch", Status: StatusOpen, Priority: 2}
		f := newFakeCouncilEnv(target)
		f.dispatchErr["planner-b"] = errors.New("backend unavailable")

		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "pair", By: "TJ"}); err == nil || !strings.Contains(err.Error(), "min_quorum=2") {
			t.Fatalf("startCouncil err = %v, want min_quorum failure", err)
		}
		// Both created helpers should have been cleaned up; target bead untouched.
		if len(f.closed) != 2 {
			t.Fatalf("expected cleanup of created helpers on failed start, closed=%+v", f.closed)
		}
		if f.beads[target.ID].Status != StatusOpen {
			t.Fatalf("target bead status = %q, want open", f.beads[target.ID].Status)
		}
	})

	t.Run("degrades gracefully when remaining members satisfy min_quorum", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := testCouncilConfig() // default: 3 members, min_quorum=2
		target := Bead{ID: "task-611", Title: "Quorum tolerant dispatch", Status: StatusOpen, Priority: 2}
		f := newFakeCouncilEnv(target)
		f.dispatchErr["planner-c"] = errors.New("temporary launch failure")

		res, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{Panel: "default", By: "TJ"})
		if err != nil {
			t.Fatalf("expected degraded start to succeed with 2/3 >= min_quorum(2): %v", err)
		}
		if len(res.Warnings) != 1 {
			t.Fatalf("expected 1 warning, got %v", res.Warnings)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-a", Content: "Plan A body"}); err != nil {
			t.Fatal(err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-b", Content: "Plan B body"}); err != nil {
			t.Fatal(err)
		}
		// No remaining active members and 2 >= min_quorum(2) -> fans in immediately even before deadline.
		lines := reconcileCouncilsPass(cfg, f.env(), f.now.Add(1*time.Minute))
		if len(lines) != 1 || !strings.Contains(lines[0], "2/3 plans") {
			t.Fatalf("expected immediate fan-in when remaining viable members finish, got %v", lines)
		}
	})

	t.Run("chair also participating as member preserves strict blind chair inputs", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := testCouncilConfig()
		target := Bead{ID: "task-612", Title: "Self-chair blind test", Status: StatusOpen, Priority: 2}
		f := newFakeCouncilEnv(target)

		if _, err := startCouncil(cfg, f.env(), target.ID, CouncilStartOpts{
			Members: []string{"planner-a", "planner-b"},
			Chair:   "planner-a",
			By:      "TJ",
		}); err != nil {
			t.Fatalf("startCouncil failed: %v", err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-a", Content: "Approach one"}); err != nil {
			t.Fatal(err)
		}
		if _, err := submitCouncilPlan(cfg, f.env(), target.ID, CouncilSubmitOpts{Member: "planner-b", Content: "Approach two"}); err != nil {
			t.Fatal(err)
		}
		reconcileCouncilsPass(cfg, f.env(), f.now.Add(time.Minute))

		chairCreate := f.created[2]
		for _, leak := range []string{"planner-a", "planner-b", "model-alpha-1", "model-beta-2", "helper-1", "helper-2"} {
			if strings.Contains(chairCreate.Title, leak) || strings.Contains(chairCreate.Description, leak) {
				t.Errorf("blind chair leaked %q into title/description", leak)
			}
		}
	})
}

func TestCouncilCleanupSafetyAndContextIntegration(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := testCouncilConfig()
	target := Bead{ID: "task-700", Title: "Safety and context integration", Status: StatusInProgress, Priority: 2}
	unrelated := Bead{ID: "task-999", Title: "Unrelated implementer bead", Status: StatusClosed, Priority: 2}
	f := newFakeCouncilEnv(target, unrelated)

	// Write a council state whose PendingResolveBeads was tampered to include
	// both the target bead and an unrelated closed bead; drain must skip both.
	st := CouncilState{
		Version:             councilSchemaVersion,
		Bead:                target.ID,
		Panel:               "pair",
		Phase:               councilPhaseFanout,
		Anonymize:           true,
		AutoCleanup:         true,
		MinQuorum:           2,
		TimeoutMinutes:      20,
		StartedAt:           f.now,
		Deadline:            f.now.Add(20 * time.Minute),
		UpdatedAt:           f.now,
		Members:             []CouncilMemberRun{{Role: "planner-a", Label: "Plan A", Status: councilRunRunning}, {Role: "planner-b", Label: "Plan B", Status: councilRunSubmitted}},
		Chair:               CouncilChairRun{Role: "planner-chair", Status: councilRunPending},
		PendingResolveBeads: []string{target.ID, unrelated.ID},
	}
	if err := saveCouncil(st); err != nil {
		t.Fatal(err)
	}

	drainPendingCouncilResolves(f.env(), f.now.Add(time.Minute))
	if len(f.resolved) != 0 {
		t.Fatalf("drain resolved non-council beads %v, want none", f.resolved)
	}
	after, _, _ := loadCouncil(target.ID)
	if len(after.PendingResolveBeads) != 0 {
		t.Fatalf("expected invalid pending entries dropped, got %v", after.PendingResolveBeads)
	}

	// Verify quartermaster context renders council status in text and JSON.
	threads := []Thread{{Bead: target, Agent: &Agent{Name: target.ID, Kind: "pi", Status: "idle"}}}
	textOut, err := renderContextWith(cfg, threads, nil, nil, nil, TickerState{}, nil, f.now, ContextOptions{})
	if err != nil {
		t.Fatalf("renderContextWith text failed: %v", err)
	}
	if !strings.Contains(textOut, "council: fanout 1/2 [pair]") {
		t.Fatalf("context text missing council line:\n%s", textOut)
	}
	jsonOut, err := renderContextWith(cfg, threads, nil, nil, nil, TickerState{}, nil, f.now, ContextOptions{JSON: true})
	if err != nil {
		t.Fatalf("renderContextWith json failed: %v", err)
	}
	var doc struct {
		Threads []struct {
			Bead    string `json:"bead"`
			Council string `json:"council"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil || len(doc.Threads) != 1 || !strings.Contains(doc.Threads[0].Council, "fanout 1/2") {
		t.Fatalf("unexpected context JSON: %s (err=%v)", jsonOut, err)
	}
}
