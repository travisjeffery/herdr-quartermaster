package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func setupInboxRunnerTestEnv(t *testing.T) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	stDir := filepath.Join(dir, "state")
	repoDir := filepath.Join(dir, "repo")
	for _, d := range []string{cfgDir, stDir, repoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", stDir)
	cfg := defaultConfig()
	cfg.Repo = repoDir
	cfg.BeadsDir = filepath.Join(repoDir, ".beads")
	cfg.CoordinatorAgent = "pi"
	cfg.InboxRunner.Enabled = true
	return cfg, dir
}

func writeTestInboxEvent(t *testing.T, e Event) string {
	t.Helper()
	if err := writeEvent(e); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	return fmt.Sprintf("%d-%s-%s.json", e.At.UnixNano(), agentName(e.Bead), e.Kind)
}

func TestInboxRunnerConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	stDir := filepath.Join(dir, "state")
	repoDir := filepath.Join(dir, "repo")
	for _, d := range []string{cfgDir, stDir, repoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", stDir)

	writeCfg := func(body string) (Config, error) {
		full := fmt.Sprintf("repo = %q\n%s\n", repoDir, body)
		if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(full), 0o644); err != nil {
			t.Fatal(err)
		}
		return loadConfig()
	}

	// Default config: opt-in disabled, expected defaults.
	cfg, err := writeCfg("")
	if err != nil {
		t.Fatalf("default loadConfig: %v", err)
	}
	if cfg.InboxRunner.Enabled {
		t.Fatal("expected [inbox_runner] enabled default false")
	}
	if cfg.InboxRunner.IdleSeconds != 120 || cfg.InboxRunner.CooldownSeconds != 120 ||
		cfg.InboxRunner.TimeoutSeconds != 300 || cfg.InboxRunner.MaxAttempts != 2 {
		t.Fatalf("unexpected defaults: %+v", cfg.InboxRunner)
	}
	if !cfg.InboxRunner.notifyEnabled() {
		t.Fatal("expected notify default true")
	}
	wantKinds := []string{"ready_for_review", "review_bump", "orphan_finish", "merged", "closed", "unblocked", "checks_failing", "likely_stale"}
	if !slices.Equal(cfg.InboxRunner.Kinds, wantKinds) {
		t.Fatalf("default kinds = %v, want %v", cfg.InboxRunner.Kinds, wantKinds)
	}

	// Valid custom config.
	cfg, err = writeCfg(`
[inbox_runner]
enabled = true
idle_seconds = 60
cooldown_seconds = 90
timeout_seconds = 180
max_attempts = 3
kinds = ["ready_for_review", "merged"]
command = ["echo", "ok"]
notify = false
`)
	if err != nil {
		t.Fatalf("valid custom config failed: %v", err)
	}
	if !cfg.InboxRunner.Enabled || cfg.InboxRunner.notifyEnabled() || cfg.InboxRunner.MaxAttempts != 3 {
		t.Fatalf("unexpected custom config: %+v", cfg.InboxRunner)
	}

	// Invalid configs must fail closed.
	invalid := []struct {
		name string
		toml string
		want string
	}{
		{"negative idle", "[inbox_runner]\nidle_seconds = -1", "idle_seconds"},
		{"negative cooldown", "[inbox_runner]\ncooldown_seconds = -5", "cooldown_seconds"},
		{"zero timeout", "[inbox_runner]\ntimeout_seconds = 0", "timeout_seconds"},
		{"zero max_attempts", "[inbox_runner]\nmax_attempts = 0", "max_attempts"},
		{"empty kinds", "[inbox_runner]\nkinds = []", "must not be empty"},
		{"forbid needs_you", "[inbox_runner]\nkinds = [\"needs_you\"]", "human-gated"},
		{"forbid prompt", "[inbox_runner]\nkinds = [\"prompt\"]", "human-gated"},
		{"forbid breaker_tripped", "[inbox_runner]\nkinds = [\"breaker_tripped\"]", "human-gated"},
		{"forbid lease_expired", "[inbox_runner]\nkinds = [\"lease_expired\"]", "human-gated"},
		{"forbid unsafe_auto_merge", "[inbox_runner]\nkinds = [\"unsafe_auto_merge\"]", "human-gated"},
		{"unknown kind", "[inbox_runner]\nkinds = [\"totally_unknown\"]", "unknown kind"},
		{"empty command", "[inbox_runner]\ncommand = [\"\"]", "executable"},
	}
	for _, tc := range invalid {
		if _, err := writeCfg(tc.toml); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

func TestInboxRunnerAllowlistFiltering(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Command = []string{"true"}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-5 * time.Minute)
	fresh := now.Add(-30 * time.Second)

	// Human-gated items must never trigger a run.
	writeTestInboxEvent(t, Event{Bead: "task-1", Kind: EventNeedsYou, Summary: "needs decision", At: old})
	writeTestInboxEvent(t, Event{
		Bead:    "task-2",
		Kind:    EventNeedsYou,
		Summary: "approval prompt",
		At:      old.Add(time.Second),
		Clears:  &InboxCondition{Kind: "prompt", Bead: "task-2", Key: "abcd1234"},
	})
	writeTestInboxEvent(t, Event{Bead: "task-3", Kind: EventBreaker, Summary: "breaker tripped", At: old.Add(2 * time.Second)})
	writeTestInboxEvent(t, Event{Bead: "task-4", Kind: EventKind("prompt"), Summary: "direct prompt kind", At: old.Add(3 * time.Second)})

	// Defense in depth: even if Kinds were tampered with in memory, human-gated kinds are rejected.
	tampered := cfg
	tampered.InboxRunner.Kinds = append(slices.Clone(cfg.InboxRunner.Kinds), "needs_you", "prompt", "breaker_tripped")

	launched := false
	env := inboxRunnerEnv{
		now:           func() time.Time { return now },
		listAgents:    func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      coordinatorHome,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			launched = true
			return inboxRunnerExecResult{ExitCode: 0}
		},
		notify: func(_, _ string) desktopResult { return desktopResult{"submitted", "ok"} },
	}

	res, err := runInboxRunner(context.Background(), tampered, env, inboxRunnerOpts{Manual: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if launched || res.Ran || !res.Skipped || res.SkipReason != "no_eligible_items" {
		t.Fatalf("human-gated items should skip cleanly: launched=%v res=%+v", launched, res)
	}

	// Add an eligible aged item, a too-new item, and a headless-enrolled item.
	eligibleFile := writeTestInboxEvent(t, Event{Bead: "task-5", Kind: EventReadyForReview, Summary: "PR #42 ready for review", At: old.Add(4 * time.Second)})
	freshFile := writeTestInboxEvent(t, Event{Bead: "task-6", Kind: EventMerged, Summary: "PR #43 merged", At: fresh})
	enrolledFile := writeTestInboxEvent(t, Event{Bead: "task-7", Kind: EventMerged, Summary: "PR #44 merged", At: old.Add(5 * time.Second)})
	env.headlessOwned = func(id string) bool { return id == "task-7" }

	var gotPrompt string
	env.execCmd = func(_ context.Context, _ Config, _ string, _ []string, prompt string) inboxRunnerExecResult {
		launched = true
		gotPrompt = prompt
		// Simulate runner acking the eligible file.
		_, _ = inboxDoneFiles([]string{eligibleFile})
		return inboxRunnerExecResult{ExitCode: 0, Output: "handled ready_for_review"}
	}
	var notifiedTitle, notifiedBody string
	env.notify = func(title, body string) desktopResult {
		notifiedTitle, notifiedBody = title, body
		return desktopResult{"submitted", "ok"}
	}

	// Ticker mode: only aged eligibleFile runs; freshFile waits; enrolledFile and human-gated excluded.
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil {
		t.Fatalf("eligible run failed: %v", err)
	}
	if !launched || !res.Ran || !slices.Equal(res.Eligible, []string{eligibleFile}) || res.Acked != 1 {
		t.Fatalf("unexpected run result: %+v", res)
	}
	if !strings.Contains(gotPrompt, eligibleFile) ||
		strings.Contains(gotPrompt, freshFile) ||
		strings.Contains(gotPrompt, enrolledFile) ||
		strings.Contains(gotPrompt, "task-1") ||
		!strings.Contains(gotPrompt, "AUTO: [inbox-runner") ||
		!strings.Contains(gotPrompt, "quartermaster inbox done") ||
		!strings.Contains(gotPrompt, "needs_you") {
		t.Fatalf("prompt missing required guardrails or leaked excluded files:\n%s", gotPrompt)
	}
	if !strings.Contains(notifiedTitle, "headless inbox runner") || !strings.Contains(notifiedBody, "Handled 1/1") {
		t.Fatalf("unexpected desktop notify: title=%q body=%q", notifiedTitle, notifiedBody)
	}

	// Manual mode ignores item age: freshFile runs immediately.
	launched = false
	env.execCmd = func(_ context.Context, _ Config, _ string, _ []string, prompt string) inboxRunnerExecResult {
		launched = true
		gotPrompt = prompt
		return inboxRunnerExecResult{ExitCode: 0}
	}
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true})
	if err != nil {
		t.Fatalf("manual run failed: %v", err)
	}
	if !launched || !res.Ran || !slices.Equal(res.Eligible, []string{freshFile}) {
		t.Fatalf("manual run should immediately include fresh item: %+v", res)
	}
}

func TestInboxRunnerInteractiveCoordinatorBusyRecentAndAFK(t *testing.T) {
	cfg, dir := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Command = []string{"true"}
	cfg.InboxRunner.IdleSeconds = 120
	now := time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)
	writeTestInboxEvent(t, Event{Bead: "task-10", Kind: EventOrphanFinish, Summary: "orphan bead", At: now.Add(-10 * time.Minute)})

	piSess := filepath.Join(dir, "coord-pi.jsonl")
	writePiSess := func(lines ...string) {
		if err := os.WriteFile(piSess, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	launched := 0
	var curAgents map[string]Agent
	sockPresent := true
	var listErr error
	env := inboxRunnerEnv{
		now: func() time.Time { return now },
		listAgents: func() (map[string]Agent, bool, error) {
			return curAgents, sockPresent, listErr
		},
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		tickerState:   func() TickerState { return TickerState{} },
		coordDir:      coordinatorHome,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			launched++
			return inboxRunnerExecResult{ExitCode: 0}
		},
		notify: func(_, _ string) desktopResult { return desktopResult{"submitted", "ok"} },
	}

	// 1. Coordinator working -> skip.
	curAgents = map[string]Agent{
		cfg.CoordinatorName: {Name: cfg.CoordinatorName, Kind: "pi", Status: "working", PaneID: "w1:p1", Seq: 10},
	}
	res, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !res.Skipped || !strings.HasPrefix(res.SkipReason, "busy:") {
		t.Fatalf("working coordinator must skip: res=%+v err=%v", res, err)
	}

	// 2. Coordinator blocked -> skip.
	curAgents[cfg.CoordinatorName] = Agent{Name: cfg.CoordinatorName, Kind: "pi", Status: "blocked", PaneID: "w1:p1", Seq: 11}
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !res.Skipped || !strings.HasPrefix(res.SkipReason, "busy:") {
		t.Fatalf("blocked coordinator must skip: res=%+v err=%v", res, err)
	}

	// 3. Coordinator idle, but recent human input 30s ago in pi session -> skip.
	writePiSess(
		fmt.Sprintf(`{"type":"custom","customType":"quartermaster.input","timestamp":%q,"data":{"source":"interactive"}}`, now.Add(-30*time.Second).Format(time.RFC3339)),
	)
	curAgents[cfg.CoordinatorName] = Agent{
		Name: cfg.CoordinatorName, Kind: "pi", Status: "idle", PaneID: "w1:p1", Seq: 12,
		Session: AgentSession{Agent: "pi", Kind: "path", Value: piSess},
	}
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !res.Skipped || !strings.HasPrefix(res.SkipReason, "recent_activity:") {
		t.Fatalf("recent human input must skip: res=%+v err=%v", res, err)
	}

	// 4. Herdr socket present but agent list errors -> fail closed (skip).
	listErr = errors.New("socket timeout")
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !res.Skipped || !strings.HasPrefix(res.SkipReason, "unknown:") {
		t.Fatalf("unreachable live herdr must fail closed: res=%+v err=%v", res, err)
	}
	listErr = nil

	// 5. Coordinator idle with OLD human input (10m ago) and ONLY a recent passive
	// quartermaster.inbox custom_message (10s ago) -> must PROCEED once idle baseline passes!
	// Clear state so Seq transition from step 3 doesn't hold it back.
	_ = os.Remove(inboxRunnerStatePath())
	writePiSess(
		fmt.Sprintf(`{"type":"custom","customType":"quartermaster.input","timestamp":%q,"data":{"source":"interactive"}}`, now.Add(-10*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"type":"custom_message","customType":"quartermaster.inbox","timestamp":%q,"details":{"session":%q,"through":"0000000000000000001"}}`, now.Add(-10*time.Second).Format(time.RFC3339), piSess),
	)
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || !res.Ran || launched != 1 {
		t.Fatalf("idle coordinator with only passive inbox notice should proceed: launched=%d res=%+v err=%v", launched, res, err)
	}

	// 6. Coordinator closed / AFK (no coordinator agent, or herdr absent) -> proceeds.
	_ = os.Remove(inboxRunnerStatePath())
	curAgents = nil
	sockPresent = false
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || !res.Ran || launched != 2 {
		t.Fatalf("AFK mode without coordinator must proceed: launched=%d res=%+v err=%v", launched, res, err)
	}
}

func TestInboxRunnerSingleFlightFlockAndFileModes(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Command = []string{"true"}
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	writeTestInboxEvent(t, Event{Bead: "task-20", Kind: EventMerged, Summary: "merged", At: now.Add(-5 * time.Minute)})

	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32

	env := inboxRunnerEnv{
		now:           func() time.Time { return now },
		listAgents:    func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      coordinatorHome,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			runs.Add(1)
			close(started)
			<-release
			return inboxRunnerExecResult{ExitCode: 0}
		},
		notify: func(_, _ string) desktopResult { return desktopResult{"submitted", "ok"} },
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var firstRes inboxRunnerResult
	var firstErr error
	go func() {
		defer wg.Done()
		firstRes, firstErr = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true})
	}()

	<-started
	if !probeInboxRunnerLocked() {
		t.Fatal("expected lock probe to report running while first run holds flock")
	}

	// Overlapping second invocation must skip immediately without running.
	secondRes, secondErr := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true})
	if secondErr != nil {
		t.Fatalf("second run unexpected error: %v", secondErr)
	}
	if secondRes.Ran || !secondRes.Skipped || !strings.HasPrefix(secondRes.SkipReason, "runner_busy:") {
		t.Fatalf("overlapping run should skip with runner_busy: %+v", secondRes)
	}

	close(release)
	wg.Wait()
	if firstErr != nil || !firstRes.Ran || runs.Load() != 1 {
		t.Fatalf("first run failed: runs=%d res=%+v err=%v", runs.Load(), firstRes, firstErr)
	}
	if probeInboxRunnerLocked() {
		t.Fatal("lock should be released after first run finishes")
	}

	// Verify 0600 permissions on lock and state file.
	for _, p := range []string{inboxRunnerLockPath(), inboxRunnerStatePath()} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", p, mode)
		}
	}
}

func TestInboxRunnerCooldownMaxAttemptsAndPruning(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Command = []string{"true"}
	cfg.InboxRunner.CooldownSeconds = 120
	cfg.InboxRunner.MaxAttempts = 2

	cur := time.Date(2026, 11, 24, 16, 0, 0, 0, time.UTC)
	itemFile := writeTestInboxEvent(t, Event{Bead: "task-30", Kind: EventUnblocked, Summary: "unblocked", At: cur.Add(-10 * time.Minute)})

	runs := 0
	env := inboxRunnerEnv{
		now:           func() time.Time { return cur },
		listAgents:    func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      coordinatorHome,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			runs++
			// Deliberately do NOT ack itemFile to test cooldown + max_attempts cap.
			return inboxRunnerExecResult{ExitCode: 0}
		},
		notify: func(_, _ string) desktopResult { return desktopResult{"submitted", "ok"} },
	}

	// Attempt 1 runs.
	res, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || !res.Ran || runs != 1 {
		t.Fatalf("attempt 1 failed: runs=%d res=%+v err=%v", runs, res, err)
	}

	// 60s later: blocked by persisted cooldown (simulating fresh load across restart).
	cur = cur.Add(60 * time.Second)
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !res.Skipped || !strings.HasPrefix(res.SkipReason, "cooldown") {
		t.Fatalf("within cooldown should skip: res=%+v err=%v", res, err)
	}

	// 121s after attempt 1: cooldown elapsed -> Attempt 2 runs.
	cur = cur.Add(61 * time.Second)
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || !res.Ran || runs != 2 {
		t.Fatalf("attempt 2 failed: runs=%d res=%+v err=%v", runs, res, err)
	}

	// Check persisted attempt count is 2.
	st := loadInboxRunnerState()
	if st.Attempts[itemFile].Count != 2 {
		t.Fatalf("persisted attempt count = %d, want 2", st.Attempts[itemFile].Count)
	}

	// 3 minutes later: cooldown elapsed, but max_attempts (2) caps the un-acked file!
	cur = cur.Add(3 * time.Minute)
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !res.Skipped || res.SkipReason != "no_eligible_items" || runs != 2 {
		t.Fatalf("max_attempts must stop infinite retries: runs=%d res=%+v err=%v", runs, res, err)
	}

	// Archive the file from inbox -> next evaluation prunes it from persisted state.
	if n, err := inboxDoneFiles([]string{itemFile}); err != nil || n != 1 {
		t.Fatalf("archive failed: n=%d err=%v", n, err)
	}
	cur = cur.Add(3 * time.Minute)
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran {
		t.Fatalf("post-archive check: res=%+v err=%v", res, err)
	}
	st = loadInboxRunnerState()
	if len(st.Attempts) != 0 {
		t.Fatalf("archived file should be pruned from attempts, got %+v", st.Attempts)
	}
}

func TestInboxRunnerTimeoutKillsProcessGroupAndUnsetsHerdrEnv(t *testing.T) {
	cfg, dir := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.TimeoutSeconds = 1

	// Ensure HERDR_ENV / HERDR_PANE_ID in parent are stripped in child.
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p9")

	pidFile := filepath.Join(dir, "pids.txt")
	envFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "hang.sh")
	scriptBody := fmt.Sprintf(`#!/bin/sh
printf 'HERDR_ENV=%%s\nHERDR_PANE_ID=%%s\nQUARTERMASTER_INBOX_RUNNER=%%s\n' "${HERDR_ENV:-}" "${HERDR_PANE_ID:-}" "${QUARTERMASTER_INBOX_RUNNER:-}" > %q
sleep 30 &
child=$!
printf '%%s %%s\n' "$$" "$child" > %q
wait
`, envFile, pidFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.InboxRunner.Command = []string{"sh", script}

	now := time.Now()
	writeTestInboxEvent(t, Event{Bead: "task-40", Kind: EventFailing, Summary: "checks failing", At: now.Add(-5 * time.Minute)})

	var notifiedTitle string
	env := inboxRunnerEnv{
		now:           time.Now,
		listAgents:    func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      coordinatorHome,
		execCmd:       execInboxRunnerCommand,
		notify: func(title, _ string) desktopResult {
			notifiedTitle = title
			return desktopResult{"submitted", "ok"}
		},
	}

	res, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: true})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !res.Ran || res.ExitStatus != "timeout" {
		t.Fatalf("expected timeout status, got res=%+v err=%v", res, err)
	}
	if !strings.Contains(notifiedTitle, "timed out") {
		t.Fatalf("expected timeout desktop notify, got %q", notifiedTitle)
	}

	// Verify HERDR_ENV and HERDR_PANE_ID were unset in child.
	envBytes, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env capture: %v", err)
	}
	envText := string(envBytes)
	if !strings.Contains(envText, "HERDR_ENV=\n") || !strings.Contains(envText, "HERDR_PANE_ID=\n") || !strings.Contains(envText, "QUARTERMASTER_INBOX_RUNNER=1\n") {
		t.Fatalf("unexpected child env:\n%s", envText)
	}

	// Verify both child and grandchild processes were killed by process-group SIGKILL.
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid capture: %v", err)
	}
	fields := strings.Fields(string(pidBytes))
	if len(fields) != 2 {
		t.Fatalf("expected 2 pids, got %q", string(pidBytes))
	}
	for _, f := range fields {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("parse pid %q: %v", f, err)
		}
		dead := false
		for range 30 {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				dead = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !dead {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("process %d survived process-group timeout kill", pid)
		}
	}

	st := loadInboxRunnerState()
	if st.LastExitStatus != "timeout" || !strings.Contains(st.LastError, "timed out") {
		t.Fatalf("persisted timeout state mismatch: %+v", st)
	}
}

func TestInboxRunnerCommandResolutionAndSanitization(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	now := time.Date(2026, 11, 24, 17, 0, 0, 0, time.UTC)

	// Default pi command strips interactive-only flags, preserves model, adds --no-approve --print --no-session.
	cfg.CoordinatorAgent = "pi"
	cfg.Agents = map[string]AgentConfig{
		"pi": {
			CoordinatorArgs: []string{
				"--approve",
				"--model", "quartermaster/coordinator-auto",
				"--tui-mode", "regular",
				"--session-id", "interactive-1",
				"--quartermaster-coordinator", "/tmp/coord.sock",
				"--quartermaster-control", "/tmp/worker.sock",
				"--continue",
			},
		},
	}
	argv, err := inboxRunnerCommand(cfg, false, now)
	if err != nil {
		t.Fatalf("pi command: %v", err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"pi --no-approve --print --no-session", "--model quartermaster/coordinator-auto", "--quartermaster-router"} {
		if !strings.Contains(joined, want) {
			t.Errorf("pi argv %q missing %q", joined, want)
		}
	}
	for _, forbid := range []string{" --approve", "--tui-mode", "--session-id", "--quartermaster-coordinator", "--quartermaster-control", "--continue"} {
		if strings.Contains(joined, forbid) {
			t.Errorf("pi argv %q must strip %q", joined, forbid)
		}
	}

	// Claude default strips --resume <id> and adds --print.
	cfg.CoordinatorAgent = "claude"
	cfg.Agents["claude"] = AgentConfig{CoordinatorArgs: []string{"--resume", "sess-123", "--model", "claude-fable-5-2"}}
	argv, err = inboxRunnerCommand(cfg, false, now)
	if err != nil {
		t.Fatalf("claude command: %v", err)
	}
	wantClaude := []string{"claude", "--print", "--model", "claude-fable-5-2"}
	if !slices.Equal(argv, wantClaude) {
		t.Fatalf("claude argv = %v, want %v", argv, wantClaude)
	}

	// Sanitization redacts secrets and tap URLs.
	dirty := "failed token=sk_live_abcdefgh1234567890 https://100.64.0.1:8642/tap/abc.def \x1b[31merror\x1b[0m"
	clean := sanitizeInboxRunnerText(dirty, 200)
	if strings.Contains(clean, "sk_live_") || strings.Contains(clean, "100.64.0.1") || strings.Contains(clean, "\x1b") {
		t.Fatalf("sanitization leaked secret/url/ansi: %q", clean)
	}
}

func TestInboxRunnerCLIDryRunAndStatusJSON(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Command = []string{"echo", "ok"}
	now := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)

	elig := writeTestInboxEvent(t, Event{Bead: "task-50", Kind: EventReviewBump, Summary: "PR #50 waiting", At: now.Add(-10 * time.Minute)})
	needs := writeTestInboxEvent(t, Event{Bead: "task-51", Kind: EventNeedsYou, Summary: "needs human", At: now.Add(-10 * time.Minute)})

	execCalls := 0
	env := inboxRunnerEnv{
		now:           func() time.Time { return now },
		listAgents:    func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      coordinatorHome,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			execCalls++
			return inboxRunnerExecResult{ExitCode: 0}
		},
		notify: func(_, _ string) desktopResult { return desktopResult{"submitted", "ok"} },
	}

	// dry-run must not execute or write inbox-runner.json.
	var dryOut bytes.Buffer
	if err := inboxRunnerCmd(cfg, env, []string{"run", "--dry-run"}, &dryOut); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	if execCalls != 0 {
		t.Fatalf("dry-run executed command %d times", execCalls)
	}
	if _, err := os.Stat(inboxRunnerStatePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run must not create state file, stat err=%v", err)
	}
	dryText := dryOut.String()
	if !strings.Contains(dryText, "would run on 1 eligible") || !strings.Contains(dryText, elig) || !strings.Contains(dryText, needs) {
		t.Fatalf("unexpected dry-run output:\n%s", dryText)
	}

	// status --json parses cleanly and matches structured fields.
	var jsonOut bytes.Buffer
	if err := inboxRunnerCmd(cfg, env, []string{"status", "--json"}, &jsonOut); err != nil {
		t.Fatalf("status --json failed: %v", err)
	}
	var rep InboxRunnerStatusReport
	if err := json.Unmarshal(jsonOut.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal status JSON: %v\n%s", err, jsonOut.String())
	}
	if !rep.Enabled || rep.Running || rep.Coordinator.State != "absent" ||
		rep.Inbox.Total != 2 || rep.Inbox.EligibleNow != 1 || rep.Inbox.Excluded != 1 ||
		rep.Config.MaxAttempts != 2 {
		t.Fatalf("unexpected status JSON report: %+v", rep)
	}

	// Plain-text status renders cleanly.
	var textOut bytes.Buffer
	if err := inboxRunnerCmd(cfg, env, []string{"status"}, &textOut); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if !strings.Contains(textOut.String(), "inbox-runner: enabled") || !strings.Contains(textOut.String(), "2 total") {
		t.Fatalf("unexpected text status:\n%s", textOut.String())
	}
}

func TestInboxRunnerTickerNonBlockingIntegration(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Enabled = true
	cfg.InboxRunner.Command = []string{"true"}
	now := time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)

	elig := writeTestInboxEvent(t, Event{Bead: "task-60", Kind: EventLikelyStale, Summary: "stale bead", At: now.Add(-5 * time.Minute)})

	started := make(chan struct{})
	release := make(chan struct{})
	var launches atomic.Int32
	var notified atomic.Int32

	env := inboxRunnerEnv{
		now:           func() time.Time { return now },
		listAgents:    func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      coordinatorHome,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			launches.Add(1)
			close(started)
			<-release
			_, _ = inboxDoneFiles([]string{elig})
			return inboxRunnerExecResult{ExitCode: 0, Output: "acked stale bead"}
		},
		notify: func(_, _ string) desktopResult {
			notified.Add(1)
			return desktopResult{"submitted", "ok"}
		},
	}

	var wg sync.WaitGroup
	var logBuf bytes.Buffer
	tk := Ticker{
		cfg:            cfg,
		log:            log.New(&logBuf, "", 0),
		inboxRunnerEnv: &env,
		inboxRunnerCtx: context.Background(),
		inboxRunnerWG:  &wg,
	}

	st := TickerState{Threads: map[string]Snapshot{}, PRs: map[string]PR{}}
	// First trigger must return immediately (non-blocking) while runner blocks on release.
	t0 := time.Now()
	tk.triggerInboxRunner(&st, map[string]Agent{}, now)
	if elapsed := time.Since(t0); elapsed > 500*time.Millisecond {
		t.Fatalf("triggerInboxRunner blocked for %v", elapsed)
	}
	<-started

	// Second tick while runner is active must not launch a second concurrent run.
	tk.triggerInboxRunner(&st, map[string]Agent{}, now.Add(15*time.Second))
	if launches.Load() != 1 {
		t.Fatalf("expected single-flight launch count 1, got %d", launches.Load())
	}

	close(release)
	wg.Wait()

	if launches.Load() != 1 || notified.Load() != 1 {
		t.Fatalf("expected 1 launch and 1 desktop notification, got launches=%d notified=%d", launches.Load(), notified.Load())
	}
	persisted := loadInboxRunnerState()
	if persisted.LastExitStatus != "ok" || persisted.LastAcked != 1 || len(persisted.Attempts) != 0 {
		t.Fatalf("unexpected persisted state after async ticker run: %+v", persisted)
	}
}

func TestInboxRunnerClaudeRecentActivityAndSeqReset(t *testing.T) {
	cfg, dir := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Command = []string{"true"}
	cfg.InboxRunner.IdleSeconds = 120
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	writeTestInboxEvent(t, Event{Bead: "task-70", Kind: EventMerged, Summary: "merged PR", At: now.Add(-10 * time.Minute)})

	// Create a fake Claude transcript with recent human input.
	claudeDir := filepath.Join(dir, "claude-home")
	projDir := filepath.Join(claudeDir, "projects", "demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	sessPath := filepath.Join(projDir, "sess-abc.jsonl")
	recentLine := fmt.Sprintf(`{"type":"user","isMeta":false,"timestamp":%q,"origin":{"kind":"human"}}`+"\n", now.Add(-45*time.Second).Format(time.RFC3339))
	if err := os.WriteFile(sessPath, []byte(recentLine), 0o600); err != nil {
		t.Fatal(err)
	}

	cur := now
	coord := Agent{
		Name: cfg.CoordinatorName, Kind: "claude", Status: "idle", PaneID: "w1:p2", Seq: 5,
		Session: AgentSession{Agent: "claude", Kind: "id", Value: "sess-abc"},
	}
	runs := 0
	env := inboxRunnerEnv{
		now:           func() time.Time { return cur },
		listAgents:    func() (map[string]Agent, bool, error) { return map[string]Agent{cfg.CoordinatorName: coord}, true, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		tickerState:   func() TickerState { return TickerState{} },
		coordDir:      coordinatorHome,
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			runs++
			return inboxRunnerExecResult{ExitCode: 0}
		},
		notify: func(_, _ string) desktopResult { return desktopResult{"submitted", "ok"} },
	}

	// Recent Claude human prompt must skip.
	res, err := runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !strings.HasPrefix(res.SkipReason, "recent_activity:") {
		t.Fatalf("recent claude prompt must skip: res=%+v err=%v", res, err)
	}

	// Once human prompt is older than idle_seconds, but Herdr Seq bumps, idle clock resets and skips.
	cur = cur.Add(3 * time.Minute)
	coord.Seq = 6
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || res.Ran || !strings.HasPrefix(res.SkipReason, "recent_activity:") {
		t.Fatalf("herdr seq change should reset idle clock and skip: res=%+v err=%v", res, err)
	}

	// After idle_seconds pass with unchanged Seq, runner proceeds.
	cur = cur.Add(125 * time.Second)
	res, err = runInboxRunner(context.Background(), cfg, env, inboxRunnerOpts{Manual: false})
	if err != nil || !res.Ran || runs != 1 {
		t.Fatalf("after idle_seconds with unchanged seq should proceed: runs=%d res=%+v err=%v", runs, res, err)
	}
}

func TestInboxRunnerFailureRecordingAndDisabledTickerNoop(t *testing.T) {
	cfg, _ := setupInboxRunnerTestEnv(t)
	cfg.InboxRunner.Enabled = false
	cfg.InboxRunner.Command = []string{"sh", "-c", "echo 'secret=sk_live_abcdefgh1234567890' >&2; exit 7"}
	now := time.Now()
	itemFile := writeTestInboxEvent(t, Event{Bead: "task-80", Kind: "closed", Summary: "closed", At: now.Add(-5 * time.Minute)})

	// Ticker with Enabled=false must be a strict no-op.
	called := false
	var wg sync.WaitGroup
	tk := Ticker{
		cfg: cfg,
		log: log.New(new(bytes.Buffer), "", 0),
		inboxRunnerEnv: &inboxRunnerEnv{
			now:        time.Now,
			listAgents: func() (map[string]Agent, bool, error) { called = true; return nil, false, nil },
		},
		inboxRunnerWG: &wg,
	}
	st := TickerState{}
	tk.triggerInboxRunner(&st, map[string]Agent{}, now)
	wg.Wait()
	if called {
		t.Fatal("disabled inbox_runner must not evaluate or run from ticker")
	}

	// Manual run executes failing command, sanitizes output, records exit code 7 and notifies failure.
	var notifyTitle, notifyBody string
	env := inboxRunnerEnv{
		now:           time.Now,
		listAgents:    func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      coordinatorHome,
		execCmd:       execInboxRunnerCommand,
		notify: func(title, body string) desktopResult {
			notifyTitle, notifyBody = title, body
			return desktopResult{"submitted", "ok"}
		},
	}
	var out bytes.Buffer
	err := inboxRunnerCmd(cfg, env, []string{"run"}, &out)
	if err == nil {
		t.Fatal("expected nonzero exit error")
	}
	if !strings.Contains(notifyTitle, "failed") || strings.Contains(notifyBody, "sk_live_") {
		t.Fatalf("failure notification must be privacy-safe: title=%q body=%q", notifyTitle, notifyBody)
	}
	persisted := loadInboxRunnerState()
	if persisted.LastExitStatus != "error" || persisted.LastExitCode == nil || *persisted.LastExitCode != 7 {
		t.Fatalf("unexpected failed state: %+v", persisted)
	}
	if strings.Contains(persisted.LastOutput, "sk_live_") || strings.Contains(persisted.LastError, "sk_live_") {
		t.Fatalf("persisted diagnostics leaked secret: output=%q err=%q", persisted.LastOutput, persisted.LastError)
	}
	if persisted.Attempts[itemFile].Count != 1 || persisted.Attempts[itemFile].LastError == "" {
		t.Fatalf("expected failed attempt recorded for %s: %+v", itemFile, persisted.Attempts)
	}
}
