package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type wfFixture struct {
	t      *testing.T
	cfg    Config
	store  *fakeCardStore
	runner *fakeRunner
	exec   *executor
	pr     PR
	gate   MergeGate
	hasG   bool
	run    Run
	clock  time.Time
}

func newWFFixture(t *testing.T, bead Bead) *wfFixture {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)
	cfg := defaultConfig()
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Widespread = 5
	store := newFakeCardStore(bead)
	store.now = now
	effectsDir := filepath.Join(stateDir(), "effects")
	if err := os.MkdirAll(effectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{dir: effectsDir, safe: true}
	f := &wfFixture{
		t:      t,
		cfg:    cfg,
		store:  store,
		runner: r,
		clock:  now,
	}
	f.exec = &executor{
		cfg: cfg,
		show: func(id string) (Bead, error) {
			return f.store.env().show(id)
		},
		active: func() ([]Bead, error) {
			var out []Bead
			for _, b := range f.store.beads {
				out = append(out, b)
			}
			return out, nil
		},
		needsMe: func(id, note string) error {
			return errors.Join(f.store.env().note(id, note), f.store.env().setStatus(id, StatusNeedsMe))
		},
		runners: map[string]opRunner{
			"github.pr.merge":     r,
			"github.pr.automerge": r,
			"iac.apply":           r,
		},
		now:   func() time.Time { return f.clock },
		sleep: func(context.Context, time.Duration) {},
		logf:  log.New(io.Discard, "", 0).Printf,
	}
	return f
}

func (f *wfFixture) env() workflowEnv {
	ce := f.store.env()
	return workflowEnv{
		cfg:       f.cfg,
		show:      ce.show,
		note:      ce.note,
		setStatus: ce.setStatus,
		viewPR: func(_ context.Context, _ string) (PR, error) {
			return f.pr, nil
		},
		viewPRGate: func(_ context.Context, _ PR, _ time.Time) (MergeGate, bool, error) {
			return f.gate, f.hasG, nil
		},
		viewRun: func(_ context.Context, _, _ string) (Run, error) {
			return f.run, nil
		},
		execPlan: func(ctx context.Context, digest string) (string, error) {
			return f.exec.execPlan(ctx, digest)
		},
		now: func() time.Time {
			f.store.now = f.clock
			return f.clock
		},
		logf: func(string, ...any) {},
	}
}

func readyPRFixture(now time.Time) (PR, MergeGate) {
	pr := PR{
		Number:         55,
		URL:            "https://github.com/acme/app/pull/55",
		State:          "OPEN",
		HeadSHA:        sha,
		ReviewDecision: "APPROVED",
		MergeState:     "CLEAN",
		DetailsFresh:   true,
		Checks:         []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}},
	}
	var rev Review
	rev.Author.Login = "reviewer"
	rev.State = "APPROVED"
	rev.CanPush = true
	rev.SubmittedAt = now.Add(-10 * time.Minute)
	rev.Commit.OID = sha
	gate := MergeGate{
		Head:    sha,
		Author:  "author",
		Viewer:  "author",
		Reviews: []Review{rev},
		At:      now,
	}
	return pr, gate
}

func TestWorkflowListTemplatesAndCustomValidation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	var out strings.Builder
	if err := workflowCmd(cfg, workflowEnv{cfg: cfg}, []string{"list-templates", "--json"}, nil, &out); err != nil {
		t.Fatalf("list-templates: %v", err)
	}
	var tmpls []workflowTemplateInfo
	if err := json.Unmarshal([]byte(out.String()), &tmpls); err != nil {
		t.Fatalf("decode templates: %v", err)
	}
	names := make([]string, len(tmpls))
	for i, tm := range tmpls {
		names[i] = tm.Name
	}
	if !slices.Equal(names, []string{"pr-merge-verify", "iac-rollout"}) {
		t.Fatalf("templates = %v", names)
	}

	// Strict custom spec validation.
	for _, tc := range []struct {
		name string
		spec string
		want string
	}{
		{"unknown field", `{"bead":"b-1","steps":[{"name":"m","kind":"milestone","phase":"p","summary":"s"}],"extra":1}`, "unknown field"},
		{"unknown step kind", `{"bead":"b-1","steps":[{"name":"sh","kind":"shell"}]}`, "unknown step kind"},
		{"empty steps", `{"bead":"b-1","steps":[]}`, "at least one step"},
		{"duplicate step name", `{"bead":"b-1","steps":[{"name":"a","kind":"milestone","phase":"p","summary":"s"},{"name":"a","kind":"milestone","phase":"p","summary":"s"}]}`, "duplicate step name"},
		{"forward gate_step", `{"bead":"b-1","steps":[{"name":"a","kind":"approval","gate_step":"g"},{"name":"g","kind":"gate_check","preview":"x"}]}`, "earlier gate_check"},
		{"invalid wait_kind", `{"bead":"b-1","steps":[{"name":"w","kind":"wait","wait_kind":"exec"}]}`, "unknown wait_kind"},
		{"invalid plan target", `{"bead":"b-1","steps":[{"name":"p","kind":"plan_step","plan":{"op":"github.pr.merge","target":"bad","pre":{"head_sha":"` + sha + `"}}}]}`, "github:owner/repo#N"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseWorkflowSpec([]byte(tc.spec), ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestWorkflowPRMergeVerifyDialAndApproval(t *testing.T) {
	t.Run("dial auto-approves at L4 and waits on PR and Actions run", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:4"}})
		pr, gate := readyPRFixture(f.clock)
		pr.Checks = []Check{{Name: "test", Status: "IN_PROGRESS"}}
		f.pr, f.gate, f.hasG = pr, gate, true
		f.run = Run{ID: 777, Workflow: "deploy", Status: "in_progress", Repo: "acme/app"}

		var out strings.Builder
		if err := workflowCmd(f.cfg, f.env(), []string{
			"start", "b-1",
			"--template", "pr-merge-verify",
			"--var", "pr_url=" + pr.URL,
			"--var", "head_sha=" + sha,
			"--var", "run_url=https://github.com/acme/app/actions/runs/777",
			"--json",
		}, nil, &out); err != nil {
			t.Fatalf("start: %v", err)
		}
		var st workflowState
		if err := json.Unmarshal([]byte(out.String()), &st); err != nil {
			t.Fatal(err)
		}
		if st.Status != wfStatusWaiting || st.CurrentStep != 1 || f.runner.calls() != 0 {
			t.Fatalf("expected waiting at step 1, got %+v (calls=%d)", st, f.runner.calls())
		}

		// PR checks go green: advance runs wait -> approval(dial) -> merge -> milestone -> wait(run).
		f.pr.Checks = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}
		st, err := advanceWorkflow(context.Background(), f.env(), st.ID)
		if err != nil {
			t.Fatalf("advance 1: %v", err)
		}
		if st.Status != wfStatusWaiting || st.CurrentStep != 5 || f.runner.calls() != 1 {
			t.Fatalf("expected waiting at step 5 (run_completed), got status=%s step=%d calls=%d why=%q", st.Status, st.CurrentStep, f.runner.calls(), st.Why)
		}
		if st.Steps[1].ApprovedBy != "dial" {
			t.Errorf("step 2 approved_by = %q, want dial", st.Steps[1].ApprovedBy)
		}

		// Actions run finishes -> completes and writes verified milestone.
		f.run.Status = "completed"
		f.run.Conclusion = "success"
		st, err = advanceWorkflow(context.Background(), f.env(), st.ID)
		if err != nil {
			t.Fatalf("advance 2: %v", err)
		}
		if st.Status != wfStatusCompleted {
			t.Fatalf("expected completed, got %+v", st)
		}
		ms, err := readMilestones("b-1")
		if err != nil || len(ms) != 2 || ms[0].Phase != "merged" || ms[1].Phase != "verified" {
			t.Fatalf("milestones = %+v, err=%v", ms, err)
		}
	})

	t.Run("below dial emits structured QuestionCard and gates on TJ approval", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-2", Status: StatusInProgress, Labels: []string{"autonomy:3"}})
		pr, gate := readyPRFixture(f.clock)
		f.pr, f.gate, f.hasG = pr, gate, true

		spec, err := buildTemplateSpec(context.Background(), f.env(), "b-2", "pr-merge-verify", map[string]string{
			"pr_url":   pr.URL,
			"head_sha": sha,
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := startWorkflow(context.Background(), f.env(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != wfStatusWaiting || st.CurrentStep != 2 || f.runner.calls() != 0 {
			t.Fatalf("expected waiting at step 2 approval, got %+v", st)
		}
		cardHash := st.Steps[1].CardHash
		if len(cardHash) != 8 {
			t.Fatalf("expected structured QuestionCard hash, got %q", cardHash)
		}
		card, err := readQuestionCard("b-2")
		if err != nil || card.Hash != cardHash || card.Class != 4 {
			t.Fatalf("card = %+v, err=%v", card, err)
		}
		if f.store.beads["b-2"].Status != StatusNeedsMe {
			t.Fatalf("bead status = %s, want needs_me", f.store.beads["b-2"].Status)
		}
		if q, ok := parseNeedsMeQuestion(f.store.beads["b-2"].Notes); !ok || q.Hash != cardHash {
			t.Fatalf("bead note question hash mismatch: ok=%v q=%+v", ok, q)
		}

		// Non-TJ approval is refused.
		if _, err := approveWorkflow(context.Background(), f.env(), st.ID, 2, "coordinator", approvalTTL); !errors.Is(err, errNotAutonomous) {
			t.Fatalf("coordinator approval error = %v, want errNotAutonomous", err)
		}

		// TJ approval executes merge and completes (optional run skipped).
		st, err = approveWorkflow(context.Background(), f.env(), st.ID, 2, "TJ", approvalTTL)
		if err != nil {
			t.Fatalf("TJ approve: %v", err)
		}
		if st.Status != wfStatusCompleted || f.runner.calls() != 1 {
			t.Fatalf("after TJ approve: status=%s calls=%d why=%q", st.Status, f.runner.calls(), st.Why)
		}
	})
}

func TestWorkflowIACRolloutApprovalDigestAndExpiry(t *testing.T) {
	t.Run("stateful replace elevates to C8 CRITICAL, applies on TJ approve, verifies no-drift", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-iac", Status: StatusInProgress, Labels: []string{"autonomy:7"}})
		previewPath := copyPreview(t, "pulumi-replace.json")
		verifyPath := filepath.Join(t.TempDir(), "empty-preview.json")
		if err := os.WriteFile(verifyPath, []byte(`{"steps":[],"changeSummary":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}

		spec, err := buildTemplateSpec(context.Background(), f.env(), "b-iac", "iac-rollout", map[string]string{
			"target":         "pulumi:org/app/staging",
			"preview":        previewPath,
			"expect":         "create-replacement=1,delete=1,delete-replaced=1,replace=1,update=1",
			"verify_preview": verifyPath,
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := startWorkflow(context.Background(), f.env(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != wfStatusWaiting || st.CurrentStep != 2 || st.Steps[1].Class != 8 {
			t.Fatalf("expected waiting at step 2 C8, got %+v", st)
		}
		card, err := readQuestionCard("b-iac")
		if err != nil || card.Class != 8 || !card.Critical || card.Evidence == nil || card.Evidence.IAC == nil {
			t.Fatalf("expected CRITICAL C8 card with IaC evidence, got %+v err=%v", card, err)
		}
		if card.Evidence.IAC.Replace != 1 || card.Evidence.IAC.Delete != 1 {
			t.Errorf("IaC counts = %+v", card.Evidence.IAC)
		}

		// Approve by TJ -> applies -> records applied milestone -> no-drift passes -> verified milestone -> completed.
		st, err = approveWorkflow(context.Background(), f.env(), st.ID, 2, "TJ", approvalTTL)
		if err != nil {
			t.Fatalf("approve iac: %v", err)
		}
		if st.Status != wfStatusCompleted || f.runner.calls() != 1 {
			t.Fatalf("expected completed with 1 apply call, got status=%s calls=%d why=%q", st.Status, f.runner.calls(), st.Why)
		}
		ms, _ := readMilestones("b-iac")
		if len(ms) != 2 || ms[0].Phase != "applied" || ms[1].Phase != "verified" {
			t.Errorf("milestones = %+v", ms)
		}
	})

	t.Run("changed preview invalidates approval both before and after approve", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-chg", Status: StatusInProgress, Labels: []string{"autonomy:4"}})
		previewPath := copyPreview(t, "tofu-update.json")
		verifyPath := filepath.Join(t.TempDir(), "empty.json")
		_ = os.WriteFile(verifyPath, []byte(`{"format_version":"1.2","resource_changes":[]}`), 0o644)

		spec, err := buildTemplateSpec(context.Background(), f.env(), "b-chg", "iac-rollout", map[string]string{
			"target":         "tofu:org/app/staging",
			"preview":        previewPath,
			"expect":         "create=1,update=1",
			"verify_preview": verifyPath,
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := startWorkflow(context.Background(), f.env(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != wfStatusWaiting || st.CurrentStep != 2 {
			t.Fatalf("expected waiting at step 2, got %+v", st)
		}
		// Mutate preview file on disk while waiting for TJ approval.
		if err := os.WriteFile(previewPath, readPlan(t, "tofu-destroy.json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := approveWorkflow(context.Background(), f.env(), st.ID, 2, "TJ", approvalTTL); err == nil || !strings.Contains(err.Error(), "digest changed") {
			t.Fatalf("expected digest changed refusal on approve, got %v", err)
		}
		if f.runner.calls() != 0 {
			t.Fatalf("runner ran %d times after preview mutation", f.runner.calls())
		}

		// Also test preview change AFTER approval (between approval and plan_step via a wait step).
		previewPath2 := copyPreview(t, "tofu-update.json")
		custom := WorkflowSpec{
			Version: 1,
			Bead:    "b-chg",
			Steps: []WorkflowStep{
				{Name: "gate", Kind: wfStepGateCheck, Preview: previewPath2, Target: "org/app/staging", Expect: "create=1,update=1"},
				{Name: "approve", Kind: wfStepApproval, Action: "apply", Target: "org/app/staging", GateStep: "gate"},
				{Name: "hold", Kind: wfStepWait, WaitKind: wfWaitSignal},
				{Name: "apply", Kind: wfStepPlan, Plan: &Step{Op: "iac.apply", Target: "tofu:org/app/staging", Args: map[string]string{"preview": previewPath2, "expect": "create=1,update=1"}}},
			},
		}
		f.store.beads["b-chg"] = Bead{ID: "b-chg", Status: StatusInProgress, Labels: []string{"autonomy:4"}}
		st2, err := startWorkflow(context.Background(), f.env(), custom)
		if err != nil || st2.Status != wfStatusWaiting || st2.CurrentStep != 2 {
			t.Fatalf("start custom: %+v err=%v", st2, err)
		}
		st2, err = approveWorkflow(context.Background(), f.env(), st2.ID, 2, "TJ", approvalTTL)
		if err != nil || st2.Status != wfStatusWaiting || st2.CurrentStep != 3 {
			t.Fatalf("after approve step 2, expected waiting at step 3 signal: %+v err=%v", st2, err)
		}
		// Now mutate preview before releasing step 3!
		if err := os.WriteFile(previewPath2, readPlan(t, "tofu-destroy.json"), 0o644); err != nil {
			t.Fatal(err)
		}
		st2, err = approveWorkflow(context.Background(), f.env(), st2.ID, 3, "TJ", approvalTTL)
		if err != nil {
			t.Fatalf("release signal: %v", err)
		}
		if st2.Status != wfStatusFailed || !strings.Contains(st2.Why, "preview digest changed") || f.runner.calls() != 0 {
			t.Fatalf("expected step 4 failure on changed preview, got status=%s why=%q calls=%d", st2.Status, st2.Why, f.runner.calls())
		}
	})

	t.Run("approval expires before plan_step and requires re-approval", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-exp", Status: StatusInProgress, Labels: []string{"autonomy:4"}})
		previewPath := copyPreview(t, "tofu-update.json")
		custom := WorkflowSpec{
			Version: 1,
			Bead:    "b-exp",
			Steps: []WorkflowStep{
				{Name: "gate", Kind: wfStepGateCheck, Preview: previewPath, Target: "org/app/staging", Expect: "create=1,update=1"},
				{Name: "approve", Kind: wfStepApproval, Action: "apply", Target: "org/app/staging", GateStep: "gate"},
				{Name: "hold", Kind: wfStepWait, WaitKind: wfWaitSignal},
				{Name: "apply", Kind: wfStepPlan, Plan: &Step{Op: "iac.apply", Target: "tofu:org/app/staging", Args: map[string]string{"preview": previewPath, "expect": "create=1,update=1"}}},
			},
		}
		st, err := startWorkflow(context.Background(), f.env(), custom)
		if err != nil || st.CurrentStep != 2 {
			t.Fatalf("start: %+v err=%v", st, err)
		}
		st, err = approveWorkflow(context.Background(), f.env(), st.ID, 2, "TJ", 30*time.Minute)
		if err != nil || st.CurrentStep != 3 {
			t.Fatalf("approve step 2: %+v err=%v", st, err)
		}
		// Advance clock past the 30m approval TTL, then release step 3 signal.
		f.clock = f.clock.Add(45 * time.Minute)
		st, err = approveWorkflow(context.Background(), f.env(), st.ID, 3, "TJ", time.Hour)
		if err != nil {
			t.Fatalf("release signal after expiry: %v", err)
		}
		if st.Status != wfStatusWaiting || st.CurrentStep != 2 || !strings.Contains(st.Why, "expired") || f.runner.calls() != 0 {
			t.Fatalf("expected re-parked on expired approval at step 2, got status=%s step=%d why=%q calls=%d", st.Status, st.CurrentStep, st.Why, f.runner.calls())
		}
		// Fresh TJ approval lets step 4 execute and complete.
		st, err = approveWorkflow(context.Background(), f.env(), st.ID, 2, "TJ", time.Hour)
		if err != nil || st.Status != wfStatusCompleted || f.runner.calls() != 1 {
			t.Fatalf("after fresh approval: status=%s calls=%d why=%q err=%v", st.Status, f.runner.calls(), st.Why, err)
		}
	})

	t.Run("post-apply no-drift check fails when drift remains", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-drift", Status: StatusInProgress, Labels: []string{"autonomy:7"}})
		previewPath := copyPreview(t, "tofu-update.json")
		driftPath := copyPreview(t, "tofu-update.json")
		spec, err := buildTemplateSpec(context.Background(), f.env(), "b-drift", "iac-rollout", map[string]string{
			"target":         "tofu:org/app/staging",
			"preview":        previewPath,
			"expect":         "create=1,update=1",
			"verify_preview": driftPath,
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := startWorkflow(context.Background(), f.env(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != wfStatusFailed || st.CurrentStep != 5 || !strings.Contains(st.Why, "drift detected") {
			t.Fatalf("expected drift failure at step 5, got status=%s step=%d why=%q", st.Status, st.CurrentStep, st.Why)
		}
	})
}

func TestWorkflowApprovalViaStructuredQuestionCardTap(t *testing.T) {
	t.Run("tap option 1 approves and ticker advances workflow", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-tap", Status: StatusInProgress, Labels: []string{"autonomy:3"}})
		pr, gate := readyPRFixture(f.clock)
		f.pr, f.gate, f.hasG = pr, gate, true
		spec, err := buildTemplateSpec(context.Background(), f.env(), "b-tap", "pr-merge-verify", map[string]string{
			"pr_url":   pr.URL,
			"head_sha": sha,
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := startWorkflow(context.Background(), f.env(), spec)
		if err != nil || st.Status != wfStatusWaiting || st.CurrentStep != 2 {
			t.Fatalf("start: %+v err=%v", st, err)
		}
		cardHash := st.Steps[1].CardHash

		// TJ taps option 1 via tapquestion.go answerNeedsMeVia.
		pEnv := promptEnv{
			show:      f.store.env().show,
			agent:     func(string, string) (*Agent, error) { return nil, nil },
			note:      f.store.env().note,
			setStatus: f.store.env().setStatus,
		}
		if _, err := answerNeedsMeVia(pEnv, "b-tap", "1", cardHash, "tap (device phone)"); err != nil {
			t.Fatalf("answerNeedsMeVia: %v", err)
		}
		// Keep clock aligned with the journaled tap answer (time.Now()).
		f.clock = time.Now().UTC()

		advanced := advanceWaitingWorkflows(context.Background(), f.env())
		if advanced != 1 {
			t.Fatalf("expected 1 workflow advanced by ticker, got %d", advanced)
		}
		_, events, err := loadWorkflow(st.ID)
		if err != nil {
			t.Fatal(err)
		}
		after := reconstructWorkflow(spec, events)
		if after.Status != wfStatusCompleted || f.runner.calls() != 1 {
			t.Fatalf("after tap advance: status=%s calls=%d why=%q", after.Status, f.runner.calls(), after.Why)
		}
	})

	t.Run("tap option 2 cancels workflow without executing plan_step", func(t *testing.T) {
		f := newWFFixture(t, Bead{ID: "b-tap2", Status: StatusInProgress, Labels: []string{"autonomy:3"}})
		pr, gate := readyPRFixture(f.clock)
		f.pr, f.gate, f.hasG = pr, gate, true
		spec, err := buildTemplateSpec(context.Background(), f.env(), "b-tap2", "pr-merge-verify", map[string]string{
			"pr_url":   pr.URL,
			"head_sha": sha,
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := startWorkflow(context.Background(), f.env(), spec)
		if err != nil {
			t.Fatal(err)
		}
		pEnv := promptEnv{
			show:      f.store.env().show,
			agent:     func(string, string) (*Agent, error) { return nil, nil },
			note:      f.store.env().note,
			setStatus: f.store.env().setStatus,
		}
		if _, err := answerNeedsMeVia(pEnv, "b-tap2", "2", st.Steps[1].CardHash, "tap (device phone)"); err != nil {
			t.Fatal(err)
		}
		f.clock = time.Now().UTC()
		advanceWaitingWorkflows(context.Background(), f.env())
		_, events, _ := loadWorkflow(st.ID)
		after := reconstructWorkflow(spec, events)
		if after.Status != wfStatusCanceled || f.runner.calls() != 0 {
			t.Fatalf("after cancel tap: status=%s calls=%d", after.Status, f.runner.calls())
		}
	})
}

func TestWorkflowCrashSafeIdempotentResumeAndCancel(t *testing.T) {
	f := newWFFixture(t, Bead{ID: "b-crash", Status: StatusInProgress, Labels: []string{"autonomy:4"}})
	pr, gate := readyPRFixture(f.clock)
	f.pr, f.gate, f.hasG = pr, gate, true

	// Build a workflow, store spec, and simulate a crash mid-plan_step AFTER the
	// underlying executor plan already finished, AND after the next milestone was
	// written before step_ok hit events.jsonl.
	mergeStep := Step{Op: "github.pr.merge", Target: "github:acme/app#55", Pre: map[string]string{"head_sha": sha}}
	spec := WorkflowSpec{
		Version: 1,
		ID:      "wf-crash-01",
		Bead:    "b-crash",
		Created: f.clock,
		Steps: []WorkflowStep{
			{Name: "merge", Kind: wfStepPlan, Plan: &mergeStep},
			{Name: "mark", Kind: wfStepMilestone, Phase: "merged", Status: "ok", Summary: "Merged PR #55"},
		},
	}
	d, err := spec.digest()
	if err != nil {
		t.Fatal(err)
	}
	spec.Digest = d
	if err := storeWorkflowSpec(spec); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkflowEvent(spec, workflowEvent{At: f.clock, Kind: wfEventStarted, Digest: d}); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkflowEvent(spec, workflowEvent{At: f.clock, Kind: wfEventStepStarted, Step: 1, StepName: "merge"}); err != nil {
		t.Fatal(err)
	}

	// Pre-complete the child plan (as if executor finished right before workflow crashed).
	childPlan := Plan{Bead: "b-crash", Actor: "workflow:" + spec.ID, Source: "workflow", Steps: []Step{mergeStep}}
	pd, err := childPlan.digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := storePlan(childPlan, pd); err != nil {
		t.Fatal(err)
	}
	if err := appendPlanEvent(pd, planEvent{At: f.clock, Kind: planDone, Class: 4}); err != nil {
		t.Fatal(err)
	}
	// Pre-write the milestone (as if milestone wrote to disk right before crash).
	if _, err := appendMilestone(f.store.env(), "b-crash", milestoneInput{Phase: "merged", Status: "ok", Summary: "Merged PR #55"}); err != nil {
		t.Fatal(err)
	}

	// Resume via CLI `workflow resume`.
	var out strings.Builder
	if err := workflowCmd(f.cfg, f.env(), []string{"resume", "wf-crash", "--json"}, nil, &out); err != nil {
		t.Fatalf("workflow resume: %v", err)
	}
	var st workflowState
	if err := json.Unmarshal([]byte(out.String()), &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != wfStatusCompleted {
		t.Fatalf("expected completed after resume, got %+v", st)
	}
	if f.runner.calls() != 0 {
		t.Errorf("resume re-executed already-done plan_step (%d calls)", f.runner.calls())
	}
	// Verify milestone wasn't duplicated on crash resume!
	ms, err := readMilestones("b-crash")
	if err != nil || len(ms) != 1 {
		t.Errorf("expected 1 deduplicated milestone, got %d (%+v)", len(ms), ms)
	}

	// Verify workflow cancel stops an active waiting workflow.
	spec2 := WorkflowSpec{
		Version: 1,
		Bead:    "b-crash",
		Steps:   []WorkflowStep{{Name: "wait_sig", Kind: wfStepWait, WaitKind: wfWaitSignal}},
	}
	st2, err := startWorkflow(context.Background(), f.env(), spec2)
	if err != nil || st2.Status != wfStatusWaiting {
		t.Fatalf("start wait_sig: %+v err=%v", st2, err)
	}
	out.Reset()
	if err := workflowCmd(f.cfg, f.env(), []string{"cancel", st2.ID, "--reason", "superseded", "--json"}, nil, &out); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var canceled workflowState
	if err := json.Unmarshal([]byte(out.String()), &canceled); err != nil {
		t.Fatal(err)
	}
	if canceled.Status != wfStatusCanceled || canceled.Why != "superseded" {
		t.Fatalf("unexpected cancel state: %+v", canceled)
	}
}

func TestWorkflowTickerAdvancementRespectsDialAndContext(t *testing.T) {
	f := newWFFixture(t, Bead{ID: "b-tick", Status: StatusInProgress, Labels: []string{"autonomy:4"}})
	f.store.beads["b-wait"] = Bead{ID: "b-wait", Status: StatusInProgress, Labels: []string{"autonomy:4"}}

	// Workflow 1: waiting on a C7 approval (above L4) — ticker must NEVER approve it.
	wf1, err := startWorkflow(context.Background(), f.env(), WorkflowSpec{
		Version: 1,
		Bead:    "b-tick",
		Steps: []WorkflowStep{
			{Name: "prod_gate", Kind: wfStepApproval, Action: "apply", Target: "org/app/prod", Class: 7},
			{Name: "done", Kind: wfStepMilestone, Phase: "done", Status: "ok", Summary: "Done"},
		},
	})
	if err != nil || wf1.Status != wfStatusWaiting {
		t.Fatalf("wf1 start: %+v err=%v", wf1, err)
	}

	// Workflow 2: waiting on an Actions run.
	f.run = Run{ID: 888, Workflow: "release", Status: "in_progress", Repo: "acme/app"}
	wf2, err := startWorkflow(context.Background(), f.env(), WorkflowSpec{
		Version: 1,
		Bead:    "b-wait",
		Steps: []WorkflowStep{
			{Name: "wait_ci", Kind: wfStepWait, WaitKind: wfWaitRunCompleted, RunURL: "https://github.com/acme/app/actions/runs/888"},
			{Name: "done", Kind: wfStepMilestone, Phase: "verified", Status: "ok", Summary: "CI green"},
		},
	})
	if err != nil || wf2.Status != wfStatusWaiting {
		t.Fatalf("wf2 start: %+v err=%v", wf2, err)
	}

	// Context lists both active workflows.
	ctxSec := workflowContextSection()
	if !strings.Contains(ctxSec, wf1.ID) || !strings.Contains(ctxSec, wf2.ID) {
		t.Errorf("workflowContextSection missing active workflows:\n%s", ctxSec)
	}

	// Tick 1: run still in progress -> nothing advances, no duplicate step_waiting events.
	beforeEv1, _ := readWorkflowEvents(wf1.ID)
	if n := advanceWaitingWorkflows(context.Background(), f.env()); n != 0 {
		t.Errorf("tick 1 advanced %d workflows, want 0", n)
	}
	afterEv1, _ := readWorkflowEvents(wf1.ID)
	if len(afterEv1) != len(beforeEv1) {
		t.Errorf("repeated tick appended duplicate waiting events: %d -> %d", len(beforeEv1), len(afterEv1))
	}

	// Tick 2: run succeeds -> wf2 advances to completed; wf1 stays waiting (never auto-approved).
	f.run.Status = "completed"
	f.run.Conclusion = "success"
	if n := advanceWaitingWorkflows(context.Background(), f.env()); n != 1 {
		t.Errorf("tick 2 advanced %d workflows, want 1", n)
	}
	spec1, ev1, _ := loadWorkflow(wf1.ID)
	if st1 := reconstructWorkflow(spec1, ev1); st1.Status != wfStatusWaiting {
		t.Errorf("wf1 should still be waiting (above dial), got %+v", st1)
	}
	spec2, ev2, _ := loadWorkflow(wf2.ID)
	if st2 := reconstructWorkflow(spec2, ev2); st2.Status != wfStatusCompleted {
		t.Errorf("wf2 should be completed, got %+v", st2)
	}

	// Status lookup by bead ID works.
	var out strings.Builder
	if err := workflowCmd(f.cfg, f.env(), []string{"status", "b-tick", "--json"}, nil, &out); err != nil {
		t.Fatalf("status by bead: %v", err)
	}
	var list []workflowState
	if err := json.Unmarshal([]byte(out.String()), &list); err != nil || len(list) != 1 || list[0].ID != wf1.ID {
		t.Fatalf("status by bead = %+v, err=%v", list, err)
	}

	// Context now shows only wf1 (wf2 is completed).
	ctxSec = workflowContextSection()
	if !strings.Contains(ctxSec, wf1.ID) || strings.Contains(ctxSec, wf2.ID) {
		t.Errorf("workflowContextSection after completion:\n%s", ctxSec)
	}
}

func TestWorkflowCustomSpecAlwaysAskBudgetAcceptanceAndTamper(t *testing.T) {
	f := newWFFixture(t, Bead{ID: "b-cust", Status: StatusInProgress, Labels: []string{"autonomy:7"}})
	pr, _ := readyPRFixture(f.clock)
	pr.State = "MERGED"
	f.pr = pr

	// Custom workflow via --file -: wait(pr_merged) -> approval(always_ask) -> verify(acceptance) -> milestone.
	customJSON := `{
  "steps": [
    {"name": "wait_merge", "kind": "wait", "wait_kind": "pr_merged", "pr_url": "` + pr.URL + `", "head_sha": "` + sha + `"},
    {"name": "confirm", "kind": "approval", "class": 2, "always_ask": true, "question": "Confirm rollout verification?"},
    {"name": "accept", "kind": "verify", "verify_kind": "acceptance"},
    {"name": "done", "kind": "milestone", "phase": "verified", "status": "ok", "summary": "Acceptance passed"}
  ]
}`
	env := f.env()
	verifyCalls := 0
	env.runVerify = func(_ context.Context, _ Bead) (verifierResult, error) {
		verifyCalls++
		return verifierResult{
			Verdict:  "pass",
			Criteria: []criterionResult{{Criterion: "rollout healthy", Pass: true, Evidence: "live check ok"}},
		}, nil
	}

	var out strings.Builder
	if err := workflowCmd(f.cfg, env, []string{"start", "b-cust", "--file", "-", "--json"}, strings.NewReader(customJSON), &out); err != nil {
		t.Fatalf("custom start: %v", err)
	}
	var st workflowState
	if err := json.Unmarshal([]byte(out.String()), &st); err != nil {
		t.Fatal(err)
	}
	// Despite bead level 7 >= class 2, always_ask forces TJ approval at step 2.
	if st.Status != wfStatusWaiting || st.CurrentStep != 2 || verifyCalls != 0 {
		t.Fatalf("always_ask did not pause at step 2: %+v verifyCalls=%d", st, verifyCalls)
	}

	// Verify private file permissions on workflows/<id>/spec.json and events.jsonl.
	wfDir, err := workflowDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		path string
		perm os.FileMode
	}{
		{workflowsDir(), 0o700},
		{wfDir, 0o700},
		{filepath.Join(wfDir, "spec.json"), 0o600},
		{filepath.Join(wfDir, "events.jsonl"), 0o600},
	} {
		fi, err := os.Stat(check.path)
		if err != nil {
			t.Fatalf("stat %s: %v", check.path, err)
		}
		if fi.Mode().Perm() != check.perm {
			t.Errorf("%s perm = %o, want %o", check.path, fi.Mode().Perm(), check.perm)
		}
	}

	// Approve step 2 -> runs acceptance verifier -> completes.
	st, err = approveWorkflow(context.Background(), env, st.ID, 2, "TJ", approvalTTL)
	if err != nil || st.Status != wfStatusCompleted || verifyCalls != 1 {
		t.Fatalf("after approve: status=%s verifyCalls=%d why=%q err=%v", st.Status, verifyCalls, st.Why, err)
	}

	// Budget refusal prevents dial auto-approval even when level >= class.
	f.cfg.Autonomy.Budget.Steps = 1
	if err := appendJournal(journalRecord{At: f.clock, Kind: journalAuto, Bead: "b-cust", Class: 2}); err != nil {
		t.Fatal(err)
	}
	f.store.beads["b-cust"] = Bead{ID: "b-cust", Status: StatusInProgress, Labels: []string{"autonomy:7"}}
	stBud, err := startWorkflow(context.Background(), f.env(), WorkflowSpec{
		Version: 1,
		Bead:    "b-cust",
		Steps:   []WorkflowStep{{Name: "gate", Kind: wfStepApproval, Class: 4}},
	})
	if err != nil || stBud.Status != wfStatusWaiting || !strings.Contains(stBud.Why, "budget") {
		t.Fatalf("budget refusal: status=%s why=%q err=%v", stBud.Status, stBud.Why, err)
	}

	// Tampering with spec.json fails loadWorkflow cleanly.
	specPath := filepath.Join(wfDir, "spec.json")
	raw, _ := os.ReadFile(specPath)
	if err := os.WriteFile(specPath, []byte(strings.Replace(string(raw), "Acceptance passed", "Tampered", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadWorkflow(st.ID); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("tampered spec.json was not rejected: %v", err)
	}
}
