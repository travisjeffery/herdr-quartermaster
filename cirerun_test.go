package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ciBoolPtr(v bool) *bool { return &v }

func ciCheck(name, conclusion, repo, runID string, jobID int64, completedAt string) Check {
	url := fmt.Sprintf("https://github.com/%s/actions/runs/%s", repo, runID)
	if jobID > 0 {
		url = fmt.Sprintf("%s/job/%d", url, jobID)
	}
	return Check{
		Name:        name,
		Status:      "COMPLETED",
		Conclusion:  conclusion,
		DetailsURL:  url,
		CompletedAt: completedAt,
	}
}

func ciPR(number int, sha string, checks ...Check) *PR {
	return &PR{
		Number:       number,
		Title:        "Fix widget",
		URL:          fmt.Sprintf("https://github.com/acme/app/pull/%d", number),
		Head:         "feat/fix-widget",
		State:        "OPEN",
		HeadSHA:      sha,
		Checks:       checks,
		DetailsFresh: true,
	}
}

func ciThread(b Bead, pr *PR) Thread {
	th := Thread{
		Bead: b,
		PR:   pr,
	}
	if pr != nil {
		th.Checks = summarizeChecks(pr.Checks)
	}
	return th.derive()
}

func TestCIRerunConfig(t *testing.T) {
	def := defaultConfig().CIRerun
	if !def.enabled() {
		t.Fatalf("default [ci_rerun] should be enabled: %+v", def)
	}
	if def.MaxReruns != 2 || def.minLevel() != 3 {
		t.Fatalf("default [ci_rerun] = max_reruns=%d min_level=%d, want 2 and 3", def.MaxReruns, def.minLevel())
	}
	if len(def.effectivePatterns()) == 0 {
		t.Fatal("default [ci_rerun] patterns should be non-empty")
	}
	if (Config{}).CIRerun.enabled() {
		t.Fatal("zero Config should leave [ci_rerun] disabled")
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
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("absent [ci_rerun]: %v", err)
	}
	if !cfg.CIRerun.enabled() || cfg.CIRerun.MaxReruns != 2 || cfg.CIRerun.minLevel() != 3 {
		t.Fatalf("absent [ci_rerun] = %+v", cfg.CIRerun)
	}

	write("repo = \"/repo\"\n[ci_rerun]\nenabled = false\n")
	if cfg, err := loadConfig(); err != nil || cfg.CIRerun.enabled() {
		t.Fatalf("enabled=false should disable: err=%v cfg=%+v", err, cfg.CIRerun)
	}

	write("repo = \"/repo\"\n[ci_rerun]\nmax_reruns = 0\n")
	if cfg, err := loadConfig(); err != nil || cfg.CIRerun.enabled() {
		t.Fatalf("max_reruns=0 should disable: err=%v cfg=%+v", err, cfg.CIRerun)
	}

	write("repo = \"/repo\"\n[ci_rerun]\npatterns = []\n")
	if cfg, err := loadConfig(); err != nil || cfg.CIRerun.enabled() || len(cfg.CIRerun.effectivePatterns()) != 0 {
		t.Fatalf("empty patterns=[] should disable matching: err=%v cfg=%+v", err, cfg.CIRerun)
	}

	write("repo = \"/repo\"\n[ci_rerun]\nmax_reruns = 3\nmin_level = 4\npatterns = [\"(?i)custom infra flake\"]\n")
	if cfg, err := loadConfig(); err != nil || !cfg.CIRerun.enabled() || cfg.CIRerun.MaxReruns != 3 || cfg.CIRerun.minLevel() != 4 || len(cfg.CIRerun.effectivePatterns()) != 1 {
		t.Fatalf("custom [ci_rerun]: err=%v cfg=%+v", err, cfg.CIRerun)
	}

	write("repo = \"/repo\"\n[ci_rerun]\nmin_level = 0\n")
	if cfg, err := loadConfig(); err != nil || cfg.CIRerun.minLevel() != 0 {
		t.Fatalf("min_level=0: err=%v min=%d", err, cfg.CIRerun.minLevel())
	}

	write("repo = \"/repo\"\n[ci_rerun]\nmax_reruns = -1\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "max_reruns") {
		t.Fatalf("negative max_reruns accepted: %v", err)
	}

	write("repo = \"/repo\"\n[ci_rerun]\nmin_level = 10\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "min_level") {
		t.Fatalf("min_level=10 accepted: %v", err)
	}

	write("repo = \"/repo\"\n[ci_rerun]\nmin_level = -1\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "min_level") {
		t.Fatalf("min_level=-1 accepted: %v", err)
	}

	write("repo = \"/repo\"\n[ci_rerun]\npatterns = [\"[invalid\"]\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "pattern") {
		t.Fatalf("invalid regex accepted: %v", err)
	}

	write("repo = \"/repo\"\n[ci_rerun]\npatterns = [\"   \"]\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "empty pattern") {
		t.Fatalf("empty pattern accepted: %v", err)
	}

	if !strings.Contains(configTemplate("/repo"), "[ci_rerun]") {
		t.Fatal("configTemplate does not mention [ci_rerun]")
	}
}

func TestCIRerunDefaultPatterns(t *testing.T) {
	patterns, err := defaultConfig().CIRerun.compilePatterns()
	if err != nil {
		t.Fatalf("compile default patterns: %v", err)
	}

	yes := []string{
		"The self-hosted runner: ci-spot-42 lost communication with the server. Verify the machine is running.",
		"The runner has received a shutdown signal. This can happen when the runner service is stopped.",
		"##[error]Process completed with exit code 137.",
		"command failed with exit status: 137",
		"node was terminated due to spot instance preemption",
		"spot interruption notice received, draining workload",
		"runner was evicted from node pool",
		"worker killed by SIGKILL during setup",
		"startup_failure",
		"failed to acquire a runner within timeout",
		"received HTTP 503 Service Unavailable from registry",
		"upstream returned 502 Bad Gateway",
	}
	for _, s := range yes {
		if _, ok := matchCIRerunPatterns(patterns, s); !ok {
			t.Errorf("want default pattern match for %q", s)
		}
	}

	no := []string{
		"",
		"FAIL: TestAuthMiddleware (0.04s): expected 200, got 500",
		"golangci-lint: unused variable foo (deadcode)",
		"Process completed with exit code 1.",
		"Process completed with exit code 2.",
		"syntax error: unexpected newline, expecting comma or )",
		"AssertionError: expected true to equal false",
		"Node.js 20 is deprecated. Actions target Node.js 20.",
		"connection refused on localhost:8080 in unit test",
	}
	for _, s := range no {
		if m, ok := matchCIRerunPatterns(patterns, s); ok {
			t.Errorf("want NO default pattern match for %q, matched %q", s, m)
		}
	}
}

func TestHeadTailBuffer(t *testing.T) {
	b := &headTailBuffer{headCap: 8, tailCap: 8}
	_, _ = b.Write([]byte("HEAD1234"))
	_, _ = b.Write([]byte("middle-content-dropped"))
	_, _ = b.Write([]byte("TAIL5678"))
	got := b.String()
	if !strings.Contains(got, "HEAD1234") || !strings.Contains(got, "TAIL5678") || strings.Contains(got, "middle") {
		t.Fatalf("unexpected headTailBuffer output: %q", got)
	}
}

func TestCIRerunMatchingFlakeBoundedAndExhaustion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_STATE_DIR", dir)

	cfg := defaultConfig()
	cfg.CIRerun = CIRerunConfig{Enabled: ciBoolPtr(true), MaxReruns: 2, MinLevel: intPtr(3)}
	bead := Bead{ID: "task-1", Title: "Fix widget", Status: StatusInProgress}
	now := t0

	var reruns []string
	var notes []string
	var journalRecs []journalRecord

	env := ciRerunEnv{
		runDetail: func(repo, runID string) (RunDetail, error) {
			return RunDetail{
				ID:           1001,
				Status:       "completed",
				Conclusion:   "failure",
				WorkflowName: "CI",
				Jobs: []RunJob{
					{ID: 5001, Name: "test", Status: "completed", Conclusion: "failure"},
				},
			}, nil
		},
		annotations: func(repo string, jobID int64) ([]CheckAnnotation, error) {
			return []CheckAnnotation{
				{AnnotationLevel: "warning", Message: "Node.js 20 deprecation"},
				{AnnotationLevel: "failure", Message: "The self-hosted runner: spot-1 lost communication with the server."},
			}, nil
		},
		failedLog: func(repo, runID string, jobID int64) (string, error) {
			t.Fatal("failedLog should not be called when failure annotation already matched")
			return "", nil
		},
		rerunFailed: func(repo, runID string) error {
			reruns = append(reruns, repo+"#"+runID)
			return nil
		},
		canRunLevel: func(b Bead) (bool, string) { return true, "" },
		enforce: func(b Bead, at time.Time) (string, int, int, error) {
			return "AUTO: [ticker L4 C3]", 4, 3, nil
		},
		note: func(id, text string) error {
			notes = append(notes, id+": "+text)
			return nil
		},
		journal: func(r journalRecord) error {
			journalRecs = append(journalRecs, r)
			return nil
		},
	}

	// Baseline: checks pending on sha1.
	pendingPR := ciPR(42, "sha1111", Check{Name: "test", Status: "IN_PROGRESS"})
	th0 := ciThread(bead, pendingPR)
	prev := snapshot(th0, Snapshot{}, now)

	// Attempt 1: job 5001 fails with lost runner communication -> rerun 1/2, suppress checks_failing.
	fail1PR := ciPR(42, "sha1111", ciCheck("test", "FAILURE", "acme/app", "1001", 5001, "2026-11-24T12:01:00Z"))
	th1 := ciThread(bead, fail1PR)
	out1 := transition(th1, prev, false, now.Add(time.Minute))
	snap1 := snapshot(th1, prev, now.Add(time.Minute))
	res1 := runCIRerun(cfg, env, th1, prev, false, true, hasEventKind(out1.Events, EventFailing), &snap1, now.Add(time.Minute))
	if !res1.SuppressFailing || res1.EmitFailing != nil || res1.FailingPrompt != "" {
		t.Fatalf("attempt 1 result = %+v, want SuppressFailing only", res1)
	}
	if len(reruns) != 1 || reruns[0] != "acme/app#1001" {
		t.Fatalf("attempt 1 reruns = %v, want [acme/app#1001]", reruns)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "AUTO: [ticker L4 C3]") || !strings.Contains(notes[0], "1/2") || !strings.Contains(notes[0], "run 1001") {
		t.Fatalf("attempt 1 notes = %v", notes)
	}
	if snap1.CIRerun == nil || snap1.CIRerun.Reruns != 1 || !snap1.CIRerun.PendingRerun {
		t.Fatalf("attempt 1 snap.CIRerun = %+v", snap1.CIRerun)
	}
	if snap1.Auto != 1 {
		t.Fatalf("attempt 1 snap.Auto = %d, want 1", snap1.Auto)
	}

	// Cached tick 15s later: same failed job still cached -> keep suppressing, do NOT rerun again.
	out1b := transition(th1, snap1, false, now.Add(75*time.Second))
	snap1b := snapshot(th1, snap1, now.Add(75*time.Second))
	res1b := runCIRerun(cfg, env, th1, snap1, false, false, hasEventKind(out1b.Events, EventFailing), &snap1b, now.Add(75*time.Second))
	if !res1b.SuppressFailing || res1b.EmitFailing != nil {
		t.Fatalf("cached tick during pending rerun = %+v", res1b)
	}
	if len(reruns) != 1 {
		t.Fatalf("cached tick triggered duplicate rerun: %v", reruns)
	}

	// Attempt 2: rerun finished fast and failed AGAIN on new job 5002 (same check name "test"!).
	// Note: transitionWith sees prev.Failing == ["test"] == th2.Checks.Failing, so hadTransitionFailing is false,
	// yet ciFailureFingerprint changed -> must trigger rerun 2/2!
	env.runDetail = func(repo, runID string) (RunDetail, error) {
		return RunDetail{
			ID:           1001,
			Status:       "completed",
			Conclusion:   "failure",
			Attempt:      2,
			WorkflowName: "CI",
			Jobs: []RunJob{
				{ID: 5002, Name: "test", Status: "completed", Conclusion: "failure"},
			},
		}, nil
	}
	fail2PR := ciPR(42, "sha1111", ciCheck("test", "FAILURE", "acme/app", "1001", 5002, "2026-11-24T12:03:00Z"))
	th2 := ciThread(bead, fail2PR)
	out2 := transition(th2, snap1b, false, now.Add(3*time.Minute))
	snap2 := snapshot(th2, snap1b, now.Add(3*time.Minute))
	res2 := runCIRerun(cfg, env, th2, snap1b, false, true, hasEventKind(out2.Events, EventFailing), &snap2, now.Add(3*time.Minute))
	if !res2.SuppressFailing || res2.EmitFailing != nil {
		t.Fatalf("attempt 2 result = %+v, want SuppressFailing", res2)
	}
	if len(reruns) != 2 {
		t.Fatalf("attempt 2 reruns = %v, want 2", reruns)
	}
	if snap2.CIRerun == nil || snap2.CIRerun.Reruns != 2 || !snap2.CIRerun.PendingRerun {
		t.Fatalf("attempt 2 snap.CIRerun = %+v", snap2.CIRerun)
	}

	// Attempt 3: job 5003 fails again -> max_reruns (2) exhausted -> MUST surface checks_failing + worker prompt!
	fail3PR := ciPR(42, "sha1111", ciCheck("test", "FAILURE", "acme/app", "1001", 5003, "2026-11-24T12:05:00Z"))
	th3 := ciThread(bead, fail3PR)
	out3 := transition(th3, snap2, false, now.Add(5*time.Minute))
	snap3 := snapshot(th3, snap2, now.Add(5*time.Minute))
	res3 := runCIRerun(cfg, env, th3, snap2, false, true, hasEventKind(out3.Events, EventFailing), &snap3, now.Add(5*time.Minute))
	if len(reruns) != 2 {
		t.Fatalf("exhausted attempt must not trigger 3rd rerun: %v", reruns)
	}
	if res3.EmitFailing == nil || res3.EmitFailing.Kind != EventFailing || !strings.Contains(res3.EmitFailing.Summary, "2/2 exhausted") {
		t.Fatalf("exhausted result = %+v, want EventFailing noting 2/2 exhausted", res3)
	}
	if !isFailingChecksPrompt(res3.FailingPrompt) {
		t.Fatalf("exhausted prompt = %q, want failing checks prompt", res3.FailingPrompt)
	}
	if snap3.CIRerun == nil || snap3.CIRerun.PendingRerun || snap3.CIRerun.SurfacedFingerprint == "" {
		t.Fatalf("exhausted snap.CIRerun = %+v", snap3.CIRerun)
	}

	// Next tick on same exhausted failure: must NOT duplicate checks_failing.
	out3b := transition(th3, snap3, false, now.Add(6*time.Minute))
	snap3b := snapshot(th3, snap3, now.Add(6*time.Minute))
	res3b := runCIRerun(cfg, env, th3, snap3, false, true, hasEventKind(out3b.Events, EventFailing), &snap3b, now.Add(6*time.Minute))
	if res3b.SuppressFailing || res3b.EmitFailing != nil || res3b.FailingPrompt != "" {
		t.Fatalf("post-exhaustion tick should be no-op, got %+v", res3b)
	}

	// Verify journal recorded both reruns and exhaustion.
	var actions []string
	for _, r := range journalRecs {
		if r.Kind == journalCIRerun {
			actions = append(actions, r.Action)
		}
	}
	if strings.Join(actions, ",") != "rerun,rerun,exhausted" {
		t.Fatalf("ci_rerun journal actions = %v, want [rerun rerun exhausted]", actions)
	}
}

func TestCIRerunMixedFlakeAndRealFailureDoesNotSuppress(t *testing.T) {
	cfg := defaultConfig()
	cfg.CIRerun = CIRerunConfig{Enabled: ciBoolPtr(true), MaxReruns: 2, MinLevel: intPtr(3)}
	bead := Bead{ID: "task-1", Title: "Fix widget", Status: StatusInProgress}
	now := t0

	t.Run("same workflow run with one flake job and one lint failure", func(t *testing.T) {
		rerunCalls := 0
		env := ciRerunEnv{
			runDetail: func(repo, runID string) (RunDetail, error) {
				return RunDetail{
					ID:         2001,
					Status:     "completed",
					Conclusion: "failure",
					Jobs: []RunJob{
						{ID: 6001, Name: "test", Status: "completed", Conclusion: "failure"},
						{ID: 6002, Name: "lint", Status: "completed", Conclusion: "failure"},
					},
				}, nil
			},
			annotations: func(repo string, jobID int64) ([]CheckAnnotation, error) {
				if jobID == 6001 {
					return []CheckAnnotation{{AnnotationLevel: "failure", Message: "Process completed with exit code 137."}}, nil
				}
				return []CheckAnnotation{{AnnotationLevel: "failure", Message: "golangci-lint: unused variable x"}}, nil
			},
			failedLog: func(repo, runID string, jobID int64) (string, error) {
				if jobID == 6002 {
					return "pkg/foo.go:12: unused variable x\nProcess completed with exit code 1.\n", nil
				}
				return "", nil
			},
			rerunFailed: func(repo, runID string) error {
				rerunCalls++
				return nil
			},
			canRunLevel: func(b Bead) (bool, string) { return true, "" },
		}

		prev := snapshot(ciThread(bead, ciPR(10, "sha-mix", Check{Name: "test", Status: "IN_PROGRESS"})), Snapshot{}, now)
		pr := ciPR(10, "sha-mix",
			ciCheck("test", "FAILURE", "acme/app", "2001", 6001, "2026-11-24T12:01:00Z"),
			ciCheck("lint", "FAILURE", "acme/app", "2001", 6002, "2026-11-24T12:01:05Z"),
		)
		th := ciThread(bead, pr)
		out := transition(th, prev, false, now.Add(time.Minute))
		snap := snapshot(th, prev, now.Add(time.Minute))
		res := runCIRerun(cfg, env, th, prev, false, true, hasEventKind(out.Events, EventFailing), &snap, now.Add(time.Minute))

		if rerunCalls != 0 {
			t.Fatalf("mixed failure triggered %d reruns, want 0", rerunCalls)
		}
		if res.SuppressFailing {
			t.Fatal("mixed failure suppressed checks_failing")
		}
		if !hasEventKind(out.Events, EventFailing) {
			t.Fatal("expected transitionWith to emit checks_failing immediately")
		}
	})

	t.Run("two workflow runs where one flakes and one has real test failure", func(t *testing.T) {
		rerunCalls := 0
		env := ciRerunEnv{
			runDetail: func(repo, runID string) (RunDetail, error) {
				if runID == "2001" {
					return RunDetail{ID: 2001, Status: "completed", Conclusion: "failure", Jobs: []RunJob{{ID: 6001, Name: "integration", Status: "completed", Conclusion: "failure"}}}, nil
				}
				return RunDetail{ID: 2002, Status: "completed", Conclusion: "failure", Jobs: []RunJob{{ID: 7001, Name: "unit", Status: "completed", Conclusion: "failure"}}}, nil
			},
			annotations: func(repo string, jobID int64) ([]CheckAnnotation, error) {
				if jobID == 6001 {
					return []CheckAnnotation{{AnnotationLevel: "failure", Message: "spot instance preemption"}}, nil
				}
				return nil, nil
			},
			failedLog: func(repo, runID string, jobID int64) (string, error) {
				return "--- FAIL: TestWidget (0.01s)\n    widget_test.go:18: got 2, want 3\n", nil
			},
			rerunFailed: func(repo, runID string) error {
				rerunCalls++
				return nil
			},
			canRunLevel: func(b Bead) (bool, string) { return true, "" },
		}

		prev := snapshot(ciThread(bead, ciPR(11, "sha-mix2", Check{Name: "unit", Status: "IN_PROGRESS"})), Snapshot{}, now)
		pr := ciPR(11, "sha-mix2",
			ciCheck("integration", "FAILURE", "acme/app", "2001", 6001, "2026-11-24T12:01:00Z"),
			ciCheck("unit", "FAILURE", "acme/app", "2002", 7001, "2026-11-24T12:01:02Z"),
		)
		th := ciThread(bead, pr)
		out := transition(th, prev, false, now.Add(time.Minute))
		snap := snapshot(th, prev, now.Add(time.Minute))
		res := runCIRerun(cfg, env, th, prev, false, true, hasEventKind(out.Events, EventFailing), &snap, now.Add(time.Minute))

		if rerunCalls != 0 {
			t.Fatalf("multi-run mixed failure triggered %d reruns, want 0", rerunCalls)
		}
		if res.SuppressFailing {
			t.Fatal("multi-run mixed failure suppressed checks_failing")
		}
	})

	t.Run("actions flake paired with failing external non-Actions check", func(t *testing.T) {
		rerunCalls := 0
		env := ciRerunEnv{
			runDetail: func(repo, runID string) (RunDetail, error) {
				t.Fatal("should fail closed on external check before inspecting Actions runs")
				return RunDetail{}, nil
			},
			rerunFailed: func(repo, runID string) error {
				rerunCalls++
				return nil
			},
			canRunLevel: func(b Bead) (bool, string) { return true, "" },
		}

		prev := snapshot(ciThread(bead, ciPR(12, "sha-ext", Check{Name: "test", Status: "IN_PROGRESS"})), Snapshot{}, now)
		pr := ciPR(12, "sha-ext",
			ciCheck("test", "FAILURE", "acme/app", "2001", 6001, "2026-11-24T12:01:00Z"),
			Check{Context: "external-ci", State: "FAILURE", TargetURL: "https://ci.example.com/builds/99"},
		)
		th := ciThread(bead, pr)
		out := transition(th, prev, false, now.Add(time.Minute))
		snap := snapshot(th, prev, now.Add(time.Minute))
		res := runCIRerun(cfg, env, th, prev, false, true, hasEventKind(out.Events, EventFailing), &snap, now.Add(time.Minute))

		if rerunCalls != 0 || res.SuppressFailing {
			t.Fatalf("external failure should not rerun or suppress: reruns=%d res=%+v", rerunCalls, res)
		}
	})

	t.Run("real failure after a prior suppressed flake rerun still surfaces checks_failing", func(t *testing.T) {
		rerunCalls := 0
		isFlake := true
		env := ciRerunEnv{
			runDetail: func(repo, runID string) (RunDetail, error) {
				jid := int64(8001)
				if !isFlake {
					jid = 8002
				}
				return RunDetail{ID: 3001, Status: "completed", Conclusion: "failure", Jobs: []RunJob{{ID: jid, Name: "test", Status: "completed", Conclusion: "failure"}}}, nil
			},
			annotations: func(repo string, jobID int64) ([]CheckAnnotation, error) {
				if jobID == 8001 {
					return []CheckAnnotation{{AnnotationLevel: "failure", Message: "Process completed with exit code 137."}}, nil
				}
				return nil, nil
			},
			failedLog: func(repo, runID string, jobID int64) (string, error) {
				return "FAIL: TestRealBug\n", nil
			},
			rerunFailed: func(repo, runID string) error {
				rerunCalls++
				return nil
			},
			canRunLevel: func(b Bead) (bool, string) { return true, "" },
		}

		prev := snapshot(ciThread(bead, ciPR(13, "sha-flip", Check{Name: "test", Status: "IN_PROGRESS"})), Snapshot{}, now)
		// Attempt 1: flake -> rerun & suppress.
		th1 := ciThread(bead, ciPR(13, "sha-flip", ciCheck("test", "FAILURE", "acme/app", "3001", 8001, "2026-11-24T12:01:00Z")))
		out1 := transition(th1, prev, false, now.Add(time.Minute))
		snap1 := snapshot(th1, prev, now.Add(time.Minute))
		res1 := runCIRerun(cfg, env, th1, prev, false, true, hasEventKind(out1.Events, EventFailing), &snap1, now.Add(time.Minute))
		if !res1.SuppressFailing || rerunCalls != 1 {
			t.Fatalf("expected attempt 1 flake rerun: res=%+v reruns=%d", res1, rerunCalls)
		}

		// Attempt 2: same check name "test" now fails with a real bug (job 8002).
		// transitionWith does not emit EventFailing because prev.Failing == ["test"],
		// so runCIRerun must emit EventFailing + prompt itself!
		isFlake = false
		th2 := ciThread(bead, ciPR(13, "sha-flip", ciCheck("test", "FAILURE", "acme/app", "3001", 8002, "2026-11-24T12:03:00Z")))
		out2 := transition(th2, snap1, false, now.Add(3*time.Minute))
		if hasEventKind(out2.Events, EventFailing) {
			t.Fatal("test precondition: transitionWith should not have re-emitted EventFailing for identical check names")
		}
		snap2 := snapshot(th2, snap1, now.Add(3*time.Minute))
		res2 := runCIRerun(cfg, env, th2, snap1, false, true, hasEventKind(out2.Events, EventFailing), &snap2, now.Add(3*time.Minute))
		if rerunCalls != 1 {
			t.Fatalf("real failure triggered a 2nd rerun: %d", rerunCalls)
		}
		if res2.SuppressFailing || res2.EmitFailing == nil || res2.EmitFailing.Kind != EventFailing || !isFailingChecksPrompt(res2.FailingPrompt) {
			t.Fatalf("real failure after suppressed flake must surface checks_failing + prompt: %+v", res2)
		}
	})
}

func TestCIRerunNewHeadSHAResetsCounter(t *testing.T) {
	cfg := defaultConfig()
	cfg.CIRerun = CIRerunConfig{Enabled: ciBoolPtr(true), MaxReruns: 2, MinLevel: intPtr(3)}
	bead := Bead{ID: "task-1", Title: "Fix widget", Status: StatusInProgress}
	now := t0

	rerunCalls := 0
	env := ciRerunEnv{
		runDetail: func(repo, runID string) (RunDetail, error) {
			return RunDetail{
				ID:         4001,
				Status:     "completed",
				Conclusion: "failure",
				Jobs:       []RunJob{{ID: 9001, Name: "test", Status: "completed", Conclusion: "failure"}},
			}, nil
		},
		annotations: func(repo string, jobID int64) ([]CheckAnnotation, error) {
			return []CheckAnnotation{{AnnotationLevel: "failure", Message: "Process completed with exit code 137."}}, nil
		},
		rerunFailed: func(repo, runID string) error {
			rerunCalls++
			return nil
		},
		canRunLevel: func(b Bead) (bool, string) { return true, "" },
	}

	// Exhaust sha1 (already at 2/2 reruns).
	prOld := ciPR(42, "sha1111", ciCheck("test", "FAILURE", "acme/app", "4001", 9001, "2026-11-24T12:01:00Z"))
	prev := snapshot(ciThread(bead, prOld), Snapshot{}, now)
	prev.CIRerun = &CIRerunSnapshot{
		Key:                 ciRerunPRKey(prOld),
		Reruns:              2,
		LastFingerprint:     ciFailureFingerprint(prOld),
		SurfacedFingerprint: ciFailureFingerprint(prOld),
	}

	// Push new commit sha2222: counter MUST reset and allow a fresh rerun 1/2.
	prNew := ciPR(42, "sha2222", ciCheck("test", "FAILURE", "acme/app", "4002", 9002, "2026-11-24T12:10:00Z"))
	thNew := ciThread(bead, prNew)
	out := transition(thNew, prev, false, now.Add(10*time.Minute))
	snap := snapshot(thNew, prev, now.Add(10*time.Minute))
	res := runCIRerun(cfg, env, thNew, prev, false, true, hasEventKind(out.Events, EventFailing), &snap, now.Add(10*time.Minute))

	if !res.SuppressFailing || rerunCalls != 1 {
		t.Fatalf("new head SHA should reset counter and rerun: res=%+v reruns=%d", res, rerunCalls)
	}
	if snap.CIRerun == nil || snap.CIRerun.Key != ciRerunPRKey(prNew) || snap.CIRerun.Reruns != 1 {
		t.Fatalf("snap.CIRerun after new head SHA = %+v", snap.CIRerun)
	}
}

func TestCIRerunAutonomyGating(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_STATE_DIR", dir)

	now := t0
	prPending := ciPR(50, "sha-auth", Check{Name: "test", Status: "IN_PROGRESS"})
	prFail := ciPR(50, "sha-auth", ciCheck("test", "FAILURE", "acme/app", "5001", 9101, "2026-11-24T12:01:00Z"))

	makeEnv := func(cfg Config, reruns *int, inspected *int) ciRerunEnv {
		tk := Ticker{cfg: cfg, log: log.New(bytes.NewBuffer(nil), "", 0)}
		env := tk.liveCIRerunEnv()
		env.runDetail = func(repo, runID string) (RunDetail, error) {
			*inspected++
			return RunDetail{ID: 5001, Status: "completed", Conclusion: "failure", Jobs: []RunJob{{ID: 9101, Name: "test", Status: "completed", Conclusion: "failure"}}}, nil
		}
		env.annotations = func(repo string, jobID int64) ([]CheckAnnotation, error) {
			return []CheckAnnotation{{AnnotationLevel: "failure", Message: "lost communication with the server"}}, nil
		}
		env.rerunFailed = func(repo, runID string) error {
			*reruns++
			return nil
		}
		env.note = func(id, text string) error { return nil }
		return env
	}

	// Case 1: bead with autonomy:2 below default min_level=3 -> must NOT rerun or suppress.
	cfg := defaultConfig()
	beadLow := Bead{ID: "task-low", Title: "Low autonomy", Status: StatusInProgress, Labels: []string{"autonomy:2"}}
	prev := snapshot(ciThread(beadLow, prPending), Snapshot{}, now)
	thLow := ciThread(beadLow, prFail)
	out := transition(thLow, prev, false, now.Add(time.Minute))
	snap := snapshot(thLow, prev, now.Add(time.Minute))
	reruns, inspected := 0, 0
	res := runCIRerun(cfg, makeEnv(cfg, &reruns, &inspected), thLow, prev, false, true, hasEventKind(out.Events, EventFailing), &snap, now.Add(time.Minute))
	if reruns != 0 || inspected != 0 || res.SuppressFailing {
		t.Fatalf("L2 bead below min_level=3 should skip immediately: reruns=%d inspected=%d res=%+v", reruns, inspected, res)
	}

	// Case 2: bead with autonomy:3 meets default min_level=3 -> auto-reruns!
	beadOK := Bead{ID: "task-ok", Title: "L3 autonomy", Status: StatusInProgress, Labels: []string{"autonomy:3"}}
	prevOK := snapshot(ciThread(beadOK, prPending), Snapshot{}, now)
	thOK := ciThread(beadOK, prFail)
	outOK := transition(thOK, prevOK, false, now.Add(time.Minute))
	snapOK := snapshot(thOK, prevOK, now.Add(time.Minute))
	reruns, inspected = 0, 0
	resOK := runCIRerun(cfg, makeEnv(cfg, &reruns, &inspected), thOK, prevOK, false, true, hasEventKind(outOK.Events, EventFailing), &snapOK, now.Add(time.Minute))
	if reruns != 1 || !resOK.SuppressFailing {
		t.Fatalf("L3 bead meeting min_level=3 should rerun: reruns=%d res=%+v", reruns, resOK)
	}

	// Case 3: custom min_level=5 blocks an L4 bead.
	cfgStrict := defaultConfig()
	cfgStrict.CIRerun.MinLevel = intPtr(5)
	beadL4 := Bead{ID: "task-l4", Title: "L4 autonomy", Status: StatusInProgress, Labels: []string{"autonomy:4"}}
	prevL4 := snapshot(ciThread(beadL4, prPending), Snapshot{}, now)
	thL4 := ciThread(beadL4, prFail)
	outL4 := transition(thL4, prevL4, false, now.Add(time.Minute))
	snapL4 := snapshot(thL4, prevL4, now.Add(time.Minute))
	reruns, inspected = 0, 0
	resL4 := runCIRerun(cfgStrict, makeEnv(cfgStrict, &reruns, &inspected), thL4, prevL4, false, true, hasEventKind(outL4.Events, EventFailing), &snapL4, now.Add(time.Minute))
	if reruns != 0 || resL4.SuppressFailing {
		t.Fatalf("L4 bead below min_level=5 should not rerun: reruns=%d res=%+v", reruns, resL4)
	}

	// Case 4: global autonomy-off stop file caps every bead at L2 -> blocks auto-rerun even on L5 bead.
	if err := os.WriteFile(autonomyStopPath(), []byte("stopped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beadL5 := Bead{ID: "task-l5", Title: "L5 autonomy", Status: StatusInProgress, Labels: []string{"autonomy:5"}}
	prevL5 := snapshot(ciThread(beadL5, prPending), Snapshot{}, now)
	thL5 := ciThread(beadL5, prFail)
	outL5 := transition(thL5, prevL5, false, now.Add(time.Minute))
	snapL5 := snapshot(thL5, prevL5, now.Add(time.Minute))
	reruns, inspected = 0, 0
	resL5 := runCIRerun(cfg, makeEnv(cfg, &reruns, &inspected), thL5, prevL5, false, true, hasEventKind(outL5.Events, EventFailing), &snapL5, now.Add(time.Minute))
	if reruns != 0 || resL5.SuppressFailing {
		t.Fatalf("autonomy-off stop file should block rerun: reruns=%d res=%+v", reruns, resL5)
	}
}

func TestCIRerunInProgressRunWaitsAndErrorsSurface(t *testing.T) {
	cfg := defaultConfig()
	cfg.CIRerun = CIRerunConfig{Enabled: ciBoolPtr(true), MaxReruns: 2, MinLevel: intPtr(3)}
	bead := Bead{ID: "task-1", Title: "Fix widget", Status: StatusInProgress}
	now := t0

	// Workflow run still in_progress while job 1 already failed on spot preemption:
	// wait for run completion without burning a rerun attempt, then rerun once completed.
	runStatus := "in_progress"
	rerunCalls := 0
	env := ciRerunEnv{
		runDetail: func(repo, runID string) (RunDetail, error) {
			return RunDetail{
				ID:         6001,
				Status:     runStatus,
				Conclusion: "failure",
				Jobs: []RunJob{
					{ID: 7101, Name: "test", Status: "completed", Conclusion: "failure"},
					{ID: 7102, Name: "build", Status: runStatus, Conclusion: ""},
				},
			}, nil
		},
		annotations: func(repo string, jobID int64) ([]CheckAnnotation, error) {
			return []CheckAnnotation{{AnnotationLevel: "failure", Message: "spot instance preemption"}}, nil
		},
		rerunFailed: func(repo, runID string) error {
			rerunCalls++
			return nil
		},
		canRunLevel: func(b Bead) (bool, string) { return true, "" },
	}

	prev := snapshot(ciThread(bead, ciPR(60, "sha-ip", Check{Name: "test", Status: "IN_PROGRESS"})), Snapshot{}, now)
	prInProg := ciPR(60, "sha-ip",
		ciCheck("test", "FAILURE", "acme/app", "6001", 7101, "2026-11-24T12:01:00Z"),
		Check{Name: "build", Status: "IN_PROGRESS"},
	)
	th1 := ciThread(bead, prInProg)
	out1 := transition(th1, prev, false, now.Add(time.Minute))
	snap1 := snapshot(th1, prev, now.Add(time.Minute))
	res1 := runCIRerun(cfg, env, th1, prev, false, true, hasEventKind(out1.Events, EventFailing), &snap1, now.Add(time.Minute))
	if !res1.SuppressFailing || rerunCalls != 0 || snap1.CIRerun == nil || !snap1.CIRerun.WaitingCompletion || snap1.CIRerun.Reruns != 0 {
		t.Fatalf("in-progress flake should wait without rerunning: res=%+v reruns=%d snap=%+v", res1, rerunCalls, snap1.CIRerun)
	}

	// Build finishes passing, run is now completed -> rerun triggers on next GH refresh!
	runStatus = "completed"
	prDone := ciPR(60, "sha-ip",
		ciCheck("test", "FAILURE", "acme/app", "6001", 7101, "2026-11-24T12:01:00Z"),
		Check{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
	)
	th2 := ciThread(bead, prDone)
	out2 := transition(th2, snap1, false, now.Add(2*time.Minute))
	snap2 := snapshot(th2, snap1, now.Add(2*time.Minute))
	res2 := runCIRerun(cfg, env, th2, snap1, false, true, hasEventKind(out2.Events, EventFailing), &snap2, now.Add(2*time.Minute))
	if !res2.SuppressFailing || rerunCalls != 1 || snap2.CIRerun == nil || snap2.CIRerun.WaitingCompletion || !snap2.CIRerun.PendingRerun || snap2.CIRerun.Reruns != 1 {
		t.Fatalf("completed run should trigger rerun: res=%+v reruns=%d snap=%+v", res2, rerunCalls, snap2.CIRerun)
	}

	// Pending rerun timeout surfaces checks_failing instead of hiding forever.
	snap3 := snapshot(th2, snap2, now.Add(2*time.Minute+ciRerunPendingTimeout))
	res3 := runCIRerun(cfg, env, th2, snap2, false, true, false, &snap3, now.Add(2*time.Minute+ciRerunPendingTimeout))
	if res3.SuppressFailing || res3.EmitFailing == nil || res3.EmitFailing.Kind != EventFailing {
		t.Fatalf("timed-out pending rerun must surface checks_failing: %+v", res3)
	}

	// Rerun command error surfaces checks_failing immediately.
	envErr := env
	envErr.rerunFailed = func(repo, runID string) error { return errors.New("HTTP 422") }
	prErr := ciPR(61, "sha-err", ciCheck("test", "FAILURE", "acme/app", "6002", 7201, "2026-11-24T12:01:00Z"))
	prevErr := snapshot(ciThread(bead, ciPR(61, "sha-err", Check{Name: "test", Status: "IN_PROGRESS"})), Snapshot{}, now)
	thErr := ciThread(bead, prErr)
	outErr := transition(thErr, prevErr, false, now.Add(time.Minute))
	snapErr := snapshot(thErr, prevErr, now.Add(time.Minute))
	resErr := runCIRerun(cfg, envErr, thErr, prevErr, false, true, hasEventKind(outErr.Events, EventFailing), &snapErr, now.Add(time.Minute))
	if resErr.SuppressFailing {
		t.Fatalf("failed gh run rerun must not suppress checks_failing: %+v", resErr)
	}
}

func TestCIRerunGuardsNeedsMeAndHold(t *testing.T) {
	cfg := defaultConfig()
	now := t0
	prPending := ciPR(70, "sha-guard", Check{Name: "test", Status: "IN_PROGRESS"})
	prFail := ciPR(70, "sha-guard", ciCheck("test", "FAILURE", "acme/app", "7001", 7701, "2026-11-24T12:01:00Z"))

	rerunCalls := 0
	env := ciRerunEnv{
		runDetail: func(repo, runID string) (RunDetail, error) {
			t.Fatal("guarded bead must not inspect runs")
			return RunDetail{}, nil
		},
		rerunFailed: func(repo, runID string) error {
			rerunCalls++
			return nil
		},
		canRunLevel: func(b Bead) (bool, string) { return true, "" },
		held:        func(id string) bool { return id == "task-held" },
	}

	for _, b := range []Bead{
		{ID: "task-needs", Title: "Needs me", Status: StatusNeedsMe},
		{ID: "task-held", Title: "Held", Status: StatusInProgress},
	} {
		prev := snapshot(ciThread(b, prPending), Snapshot{}, now)
		th := ciThread(b, prFail)
		out := transition(th, prev, false, now.Add(time.Minute))
		snap := snapshot(th, prev, now.Add(time.Minute))
		res := runCIRerun(cfg, env, th, prev, false, true, hasEventKind(out.Events, EventFailing), &snap, now.Add(time.Minute))
		if rerunCalls != 0 || res.SuppressFailing {
			t.Fatalf("bead %s should not rerun or suppress: reruns=%d res=%+v", b.ID, rerunCalls, res)
		}
	}
}

func TestCIRerunTickerEndToEndAndRestartPersistence(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("PATH", dir)

	setJSONEnv := func(key string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, string(b))
	}
	writeScript := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	writeScript("bd", `echo "$*" >> "$LOG/bd.log"
if [ "$1" = "list" ]; then
  printf '%s\n' "$FIXTURE_BEADS"
  exit 0
fi
exit 0
`)
	writeScript("herdr", `echo "$*" >> "$LOG/herdr.log"
if [ "$1" = "agent" ] && [ "$2" = "list" ]; then
  printf '{"result":{"agents":%s}}\n' "$FIXTURE_AGENTS"
  exit 0
fi
if [ "$1" = "agent" ] && [ "$2" = "prompt" ]; then
  echo "$*" >> "$LOG/prompts.log"
  printf '{"result":{}}\n'
  exit 0
fi
printf '{"result":{}}\n'
`)
	writeScript("gh", `echo "$*" >> "$LOG/gh.log"
if [ "$1" = "api" ] && [ "$2" = "user" ]; then
  echo "alice"
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  printf '%s\n' "$FIXTURE_PR_LIST"
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '%s\n' "$FIXTURE_PR_VIEW"
  exit 0
fi
if [ "$1" = "run" ] && [ "$2" = "view" ]; then
  for arg in "$@"; do
    if [ "$arg" = "--log-failed" ]; then
      printf '%s\n' "$FIXTURE_RUN_LOG"
      exit 0
    fi
  done
  printf '%s\n' "$FIXTURE_RUN_DETAIL"
  exit 0
fi
if [ "$1" = "api" ]; then
  printf '%s\n' "$FIXTURE_ANNOTATIONS"
  exit 0
fi
if [ "$1" = "run" ] && [ "$2" = "rerun" ]; then
  echo "$*" >> "$LOG/reruns.log"
  exit 0
fi
exit 0
`)

	readLines := func(name string) []string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}

	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	cfg := defaultConfig()
	cfg.Repo = t.TempDir()
	cfg.GHSeconds = 30
	cfg.IdleSeconds = 0
	cfg.VerifyMinutes = 0
	cfg.OrphanFinish.IntervalSeconds = 0
	cfg.Nudge = false
	cfg.CIRerun = CIRerunConfig{Enabled: ciBoolPtr(true), MaxReruns: 2, MinLevel: intPtr(3)}

	newTestTicker := func() Ticker {
		var logs bytes.Buffer
		tk := newTicker(cfg, log.New(&logs, "", 0))
		tk.now = func() time.Time { return now }
		return tk
	}

	bead := Bead{
		ID:     "task-1",
		Title:  "Auto-rerun CI flakes",
		Status: StatusInProgress,
		Notes:  "PR: https://github.com/acme/app/pull/88",
	}
	worker := Agent{Name: "task-1", Kind: "claude", Status: "idle", PaneID: "w1:p1", Seq: 1}
	setJSONEnv("FIXTURE_BEADS", []Bead{bead})
	setJSONEnv("FIXTURE_AGENTS", []Agent{worker})
	setJSONEnv("FIXTURE_ANNOTATIONS", []CheckAnnotation{})

	setPR := func(sha string, checks ...Check) {
		pr := PR{
			Number:  88,
			Title:   "Auto-rerun CI flakes",
			URL:     "https://github.com/acme/app/pull/88",
			Head:    "feat/ci-flake",
			State:   "OPEN",
			HeadSHA: sha,
			Checks:  checks,
		}
		setJSONEnv("FIXTURE_PR_LIST", []PR{pr})
		setJSONEnv("FIXTURE_PR_VIEW", pr)
	}

	// Tick 0: baseline with pending checks.
	setPR("aaaa1111", Check{Name: "test", Status: "IN_PROGRESS"})
	tk1 := newTestTicker()
	st := loadState()
	if err := tk1.once(&st); err != nil {
		t.Fatalf("tick 0: %v", err)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}

	// Tick 1: job 9501 fails with exit code 137 in failed log -> triggers `gh run rerun 8801 -R acme/app --failed` (1/2).
	now = now.Add(time.Minute)
	setPR("aaaa1111", ciCheck("test", "FAILURE", "acme/app", "8801", 9501, "2026-11-24T15:00:50Z"))
	setJSONEnv("FIXTURE_RUN_DETAIL", RunDetail{
		ID:           8801,
		Status:       "completed",
		Conclusion:   "failure",
		Attempt:      1,
		WorkflowName: "CI",
		Jobs:         []RunJob{{ID: 9501, Name: "test", Status: "completed", Conclusion: "failure"}},
	})
	t.Setenv("FIXTURE_RUN_LOG", "##[error]Process completed with exit code 137.")
	if err := tk1.once(&st); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}

	if lines := readLines("reruns.log"); len(lines) != 1 || lines[0] != "run rerun 8801 -R acme/app --failed" {
		t.Fatalf("tick 1 reruns.log = %v, want [run rerun 8801 -R acme/app --failed]", lines)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("tick 1 flake should suppress inbox checks_failing, got %+v", items)
	}
	if prompts := readLines("prompts.log"); len(prompts) != 0 {
		t.Fatalf("tick 1 flake should suppress worker failing-checks prompt, got %v", prompts)
	}
	bdLog := strings.Join(readLines("bd.log"), "\n")
	if !strings.Contains(bdLog, "AUTO: [ticker L4 C3] reran failed CI jobs for PR #88") || !strings.Contains(bdLog, "(1/2, run 8801)") {
		t.Fatalf("tick 1 missing AUTO note in bd.log:\n%s", bdLog)
	}

	// Simulate ticker restart: fresh Ticker + reload TickerState from state.json.
	tk2 := newTestTicker()
	st = loadState()
	if st.Threads["task-1"].CIRerun == nil || st.Threads["task-1"].CIRerun.Reruns != 1 || !st.Threads["task-1"].CIRerun.PendingRerun {
		t.Fatalf("persisted state after restart = %+v", st.Threads["task-1"].CIRerun)
	}

	// Tick 2 (after restart, 15s later, cached): same failure still pending -> no duplicate rerun.
	now = now.Add(15 * time.Second)
	if err := tk2.once(&st); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if lines := readLines("reruns.log"); len(lines) != 1 {
		t.Fatalf("post-restart cached tick triggered duplicate rerun: %v", lines)
	}

	// Tick 3 (after restart): rerun goes pending (IN_PROGRESS), counter remains 1.
	now = now.Add(time.Minute)
	setPR("aaaa1111", Check{Name: "test", Status: "IN_PROGRESS"})
	if err := tk2.once(&st); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	if st.Threads["task-1"].CIRerun == nil || st.Threads["task-1"].CIRerun.Reruns != 1 || st.Threads["task-1"].CIRerun.PendingRerun {
		t.Fatalf("after pending tick CIRerun = %+v, want Reruns=1 PendingRerun=false", st.Threads["task-1"].CIRerun)
	}

	// Simulate second restart before attempt 2 fails!
	tk3 := newTestTicker()
	st = loadState()

	// Tick 4: attempt 2 fails with runner shutdown signal -> rerun 2/2.
	now = now.Add(time.Minute)
	setPR("aaaa1111", ciCheck("test", "FAILURE", "acme/app", "8801", 9502, "2026-11-24T15:03:00Z"))
	setJSONEnv("FIXTURE_RUN_DETAIL", RunDetail{
		ID:           8801,
		Status:       "completed",
		Conclusion:   "failure",
		Attempt:      2,
		WorkflowName: "CI",
		Jobs:         []RunJob{{ID: 9502, Name: "test", Status: "completed", Conclusion: "failure"}},
	})
	t.Setenv("FIXTURE_RUN_LOG", "The runner has received a shutdown signal.")
	if err := tk3.once(&st); err != nil {
		t.Fatalf("tick 4: %v", err)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	if lines := readLines("reruns.log"); len(lines) != 2 {
		t.Fatalf("tick 4 reruns.log = %v, want 2", lines)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("tick 4 should still suppress checks_failing, got %+v", items)
	}

	// Tick 5: attempt 3 fails -> cap (2/2) exhausted -> NO 3rd rerun, checks_failing surfaces to inbox + worker prompt!
	now = now.Add(time.Minute)
	setPR("aaaa1111", ciCheck("test", "FAILURE", "acme/app", "8801", 9503, "2026-11-24T15:04:00Z"))
	if err := tk3.once(&st); err != nil {
		t.Fatalf("tick 5: %v", err)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	if lines := readLines("reruns.log"); len(lines) != 2 {
		t.Fatalf("exhausted head triggered 3rd rerun: %v", lines)
	}
	items, err := readInbox()
	if err != nil || len(items) != 1 || items[0].Kind != EventFailing || !strings.Contains(items[0].Summary, "2/2 exhausted") {
		t.Fatalf("tick 5 inbox = %+v (err=%v), want 1 checks_failing exhausted event", items, err)
	}
	if prompts := readLines("prompts.log"); len(prompts) != 1 || !strings.Contains(prompts[0], "PR #88 has failing checks: test") {
		t.Fatalf("tick 5 prompts = %v, want 1 worker failing-checks prompt", prompts)
	}

	// Tick 6: new head SHA pushed -> counter resets and a fresh flake triggers rerun 1/2!
	now = now.Add(time.Minute)
	setPR("bbbb2222", ciCheck("test", "FAILURE", "acme/app", "8802", 9601, "2026-11-24T15:05:00Z"))
	setJSONEnv("FIXTURE_RUN_DETAIL", RunDetail{
		ID:           8802,
		Status:       "completed",
		Conclusion:   "failure",
		Attempt:      1,
		WorkflowName: "CI",
		Jobs:         []RunJob{{ID: 9601, Name: "test", Status: "completed", Conclusion: "failure"}},
	})
	t.Setenv("FIXTURE_RUN_LOG", "Process completed with exit code 137.")
	if err := tk3.once(&st); err != nil {
		t.Fatalf("tick 6: %v", err)
	}
	if lines := readLines("reruns.log"); len(lines) != 3 || lines[2] != "run rerun 8802 -R acme/app --failed" {
		t.Fatalf("new head SHA should trigger rerun for run 8802, got %v", lines)
	}
	if st.Threads["task-1"].CIRerun == nil || st.Threads["task-1"].CIRerun.Reruns != 1 {
		t.Fatalf("new head SHA CIRerun state = %+v, want Reruns=1", st.Threads["task-1"].CIRerun)
	}
}
