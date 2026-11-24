package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func piAgent(status string, seq int64) *Agent {
	return &Agent{
		Name:    "task-1",
		Kind:    "pi",
		Status:  status,
		PaneID:  "w1:p1",
		Seq:     seq,
		Session: AgentSession{Agent: "pi", Kind: "path", Value: "/tmp/task-1.jsonl"},
	}
}

func stallThread(status, agentStatus string, seq int64, turn *PiTurn) Thread {
	th := Thread{
		Bead:  Bead{ID: "task-1", Title: "Recover stalled turn", Status: status},
		Agent: piAgent(agentStatus, seq),
		Turn:  turn,
	}
	return th.derive()
}

func TestIsTransientACPStall(t *testing.T) {
	yes := []*PiTurn{
		{ID: "a1", StopReason: "error", Error: "ACP stalled for 5m with no output"},
		{ID: "a2", StopReason: "error", Error: "ACP stalled for 10m with no output"},
		{ID: "a3", StopReason: "error", Error: "ACP stalled"},
		{ID: "a4", StopReason: "error", Error: "ACP stream stalled without output"},
		{ID: "a5", StopReason: "error", Error: "ACP idle stall"},
		{ID: "a6", StopReason: "error", Error: "agy stalled for 5m with no output"},
		{ID: "a7", StopReason: "error", Error: "Antigravity stalled with no output; the turn was stopped"},
		{ID: "a8", StopReason: "aborted", Error: "ACP stalled for 5m with no output"},
	}
	for _, turn := range yes {
		if !isTransientACPStall(turn) {
			t.Errorf("want transient ACP stall = true for %+v", turn)
		}
	}

	no := []*PiTurn{
		nil,
		{ID: "b1", StopReason: "stop", Error: "ACP stalled for 5m with no output"},
		{ID: "b2", StopReason: "length", Error: ""},
		{ID: "b3", StopReason: "error", Error: ""},
		{ID: "b4", StopReason: "error", Error: "429 rate limited"},
		{ID: "b5", StopReason: "error", Error: "Your authentication token has expired. Please try refreshing it."},
		{ID: "b6", StopReason: "error", Error: "Codex error: The usage limit has been reached"},
		{ID: "b7", StopReason: "aborted", Error: "Request was aborted"},
		{ID: "b8", StopReason: "error", Error: "context length exceeded"},
		{ID: "b9", StopReason: "error", Error: "ACP driver is shut down"},
		{ID: "b10", StopReason: "error", Error: "ACP authentication required"},
	}
	for _, turn := range no {
		if isTransientACPStall(turn) {
			t.Errorf("want transient ACP stall = false for %+v", turn)
		}
	}
}

func TestStallRetryConfig(t *testing.T) {
	def := defaultConfig().StallRetry
	if def.MaxRetries != 2 || def.BackoffSeconds != 30 {
		t.Fatalf("default stall_retry = %+v, want max_retries=2 backoff_seconds=30", def)
	}
	if def.backoff(1) != 30*time.Second || def.backoff(2) != 60*time.Second || def.backoff(3) != 120*time.Second {
		t.Fatalf("unexpected backoff sequence: %v, %v, %v", def.backoff(1), def.backoff(2), def.backoff(3))
	}
	if got := def.backoff(20); got != stallRetryMaxBackoff {
		t.Fatalf("backoff cap = %v, want %v", got, stallRetryMaxBackoff)
	}
	if (Config{}).StallRetry.enabled() {
		t.Fatal("zero Config should have stall_retry disabled")
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
	if cfg, err := loadConfig(); err != nil || cfg.StallRetry.MaxRetries != 2 || cfg.StallRetry.BackoffSeconds != 30 {
		t.Fatalf("absent [stall_retry]: err=%v cfg=%+v", err, cfg.StallRetry)
	}
	write("repo = \"/repo\"\n[stall_retry]\nmax_retries = 0\n")
	if cfg, err := loadConfig(); err != nil || cfg.StallRetry.enabled() {
		t.Fatalf("max_retries = 0 should disable: err=%v cfg=%+v", err, cfg.StallRetry)
	}
	write("repo = \"/repo\"\n[stall_retry]\nmax_retries = 3\nbackoff_seconds = 45\n")
	if cfg, err := loadConfig(); err != nil || cfg.StallRetry.MaxRetries != 3 || cfg.StallRetry.BackoffSeconds != 45 {
		t.Fatalf("custom [stall_retry]: err=%v cfg=%+v", err, cfg.StallRetry)
	}
	write("repo = \"/repo\"\n[stall_retry]\nmax_retries = -1\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "max_retries") {
		t.Fatalf("negative max_retries accepted: %v", err)
	}
	write("repo = \"/repo\"\n[stall_retry]\nbackoff_seconds = -5\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "backoff_seconds") {
		t.Fatalf("negative backoff_seconds accepted: %v", err)
	}
	if !strings.Contains(configTemplate("/repo"), "[stall_retry]") {
		t.Fatal("configTemplate does not mention [stall_retry]")
	}
}

func TestStallRetryLifecycleBackoffAndExhaustion(t *testing.T) {
	cfg := StallRetryConfig{MaxRetries: 2, BackoffSeconds: 30}
	now := t0
	working := stallThread(StatusInProgress, "working", 1, &PiTurn{ID: "a1", StopReason: "stop"})
	prev := snapshot(working, Snapshot{}, now.Add(-15*time.Second))

	// First stall (a2): schedules retry 1/2 after 30s backoff and suppresses raw EventFinished.
	stall1 := stallThread(StatusInProgress, "idle", 2, &PiTurn{ID: "a2", StopReason: "error", Error: "ACP stalled for 5m with no output"})
	p := planStallRetry(cfg, stall1, prev, false, now, stallRetryGuard{})
	if p.Action != stallActionSchedule || !p.SuppressFinished || p.Attempt != 1 || p.Backoff != 30*time.Second || !p.Next.NextAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("first stall plan = %+v", p)
	}
	prev = snapshot(stall1, prev, now)
	applyStallSnapshot(&prev, p.Next)

	// 15s later: still in backoff -> wait.
	p = planStallRetry(cfg, stall1, prev, false, now.Add(15*time.Second), stallRetryGuard{})
	if p.Action != stallActionWait {
		t.Fatalf("mid-backoff plan = %+v, want wait", p)
	}
	prev = snapshot(stall1, prev, now.Add(15*time.Second))
	applyStallSnapshot(&prev, p.Next)

	// 30s later: backoff elapsed -> fire retry 1/2.
	p = planStallRetry(cfg, stall1, prev, false, now.Add(30*time.Second), stallRetryGuard{})
	if p.Action != stallActionRetry || p.Attempt != 1 || p.MaxRetries != 2 {
		t.Fatalf("retry 1 plan = %+v", p)
	}
	if !strings.Contains(p.Prompt, "Auto-retry 1/2") || !strings.Contains(p.Prompt, "a2") {
		t.Fatalf("retry 1 prompt missing details: %q", p.Prompt)
	}
	if !strings.HasPrefix(p.Note, "auto: ACP stall retry 1/2") {
		t.Fatalf("retry 1 note = %q", p.Note)
	}
	// Simulate successful nudge delivery.
	prev = snapshot(stall1, prev, now.Add(30*time.Second))
	applyStallSnapshot(&prev, stallSnapshot{Retries: 1, TurnID: "a2", Error: stall1.Turn.Error})

	// Worker goes working while still showing last completed turn a2: count must stay 1.
	working2 := stallThread(StatusInProgress, "working", 3, &PiTurn{ID: "a2", StopReason: "error", Error: "ACP stalled for 5m with no output"})
	p = planStallRetry(cfg, working2, prev, false, now.Add(45*time.Second), stallRetryGuard{})
	if p.Action != stallActionNone || p.Next.Retries != 1 {
		t.Fatalf("while working after retry 1: %+v", p)
	}
	prev = snapshot(working2, prev, now.Add(45*time.Second))
	applyStallSnapshot(&prev, p.Next)

	// Second stall (a3): backoff doubles to 60s.
	tStall2 := now.Add(345 * time.Second)
	stall2 := stallThread(StatusInProgress, "idle", 4, &PiTurn{ID: "a3", StopReason: "error", Error: "ACP stalled for 5m with no output"})
	p = planStallRetry(cfg, stall2, prev, false, tStall2, stallRetryGuard{})
	if p.Action != stallActionSchedule || !p.SuppressFinished || p.Attempt != 2 || p.Backoff != 60*time.Second || !p.Next.NextAt.Equal(tStall2.Add(60*time.Second)) {
		t.Fatalf("second stall plan = %+v", p)
	}
	prev = snapshot(stall2, prev, tStall2)
	applyStallSnapshot(&prev, p.Next)

	// 30s into second backoff: still waiting (must wait full 60s).
	p = planStallRetry(cfg, stall2, prev, false, tStall2.Add(30*time.Second), stallRetryGuard{})
	if p.Action != stallActionWait {
		t.Fatalf("30s into 60s backoff should wait, got %+v", p)
	}

	// 60s later: fire retry 2/2.
	p = planStallRetry(cfg, stall2, prev, false, tStall2.Add(60*time.Second), stallRetryGuard{})
	if p.Action != stallActionRetry || p.Attempt != 2 || !strings.Contains(p.Prompt, "Auto-retry 2/2") || !strings.Contains(p.Prompt, "a3") {
		t.Fatalf("retry 2 plan = %+v", p)
	}
	prev = snapshot(stall2, prev, tStall2.Add(60*time.Second))
	applyStallSnapshot(&prev, stallSnapshot{Retries: 2, TurnID: "a3", Error: stall2.Turn.Error})

	// Worker works and stalls a third time (a4): cap exhausted -> escalate cleanly.
	working3 := stallThread(StatusInProgress, "working", 5, stall2.Turn)
	prev = snapshot(working3, prev, tStall2.Add(75*time.Second))
	tStall3 := tStall2.Add(375 * time.Second)
	stall3 := stallThread(StatusInProgress, "idle", 6, &PiTurn{ID: "a4", StopReason: "error", Error: "ACP stalled for 5m with no output"})
	p = planStallRetry(cfg, stall3, prev, false, tStall3, stallRetryGuard{})
	if p.Action != stallActionEscalate || !p.SuppressFinished || !p.Notify || p.EscalateEvent == nil {
		t.Fatalf("cap exhaustion plan = %+v", p)
	}
	if p.EscalateEvent.Kind != EventFinished || !strings.Contains(p.EscalateEvent.Summary, "after 2/2 auto-retries") {
		t.Fatalf("exhausted event = %+v", p.EscalateEvent)
	}
	if !p.Next.Escalated || !p.Next.NextAt.IsZero() {
		t.Fatalf("exhausted next state = %+v", p.Next)
	}
	prev = snapshot(stall3, prev, tStall3)
	applyStallSnapshot(&prev, p.Next)

	// Subsequent ticks on the same exhausted turn must not retry or re-escalate.
	p = planStallRetry(cfg, stall3, prev, false, tStall3.Add(5*time.Minute), stallRetryGuard{})
	if p.Action != stallActionNone || p.EscalateEvent != nil {
		t.Fatalf("after exhaustion next tick should be no-op, got %+v", p)
	}
}

func TestStallRetryResetOnHealthyTurn(t *testing.T) {
	cfg := StallRetryConfig{MaxRetries: 2, BackoffSeconds: 30}
	now := t0

	// Starting from a state with 2 exhausted retries on a4, a healthy turn a5 resets everything.
	prev := Snapshot{
		AgentStatus:    "working",
		AgentSeq:       6,
		TurnID:         "a4",
		StallRetries:   2,
		StallTurnID:    "a4",
		StallError:     "ACP stalled for 5m with no output",
		StallEscalated: true,
	}
	healthy := stallThread(StatusInProgress, "idle", 7, &PiTurn{ID: "a5", StopReason: "stop"})
	p := planStallRetry(cfg, healthy, prev, false, now, stallRetryGuard{})
	if p.Action != stallActionReset || p.Next.hasState() {
		t.Fatalf("healthy turn should reset stall state, got %+v", p)
	}
	prev = snapshot(healthy, prev, now)
	applyStallSnapshot(&prev, p.Next)

	// A later stall on a6 starts a fresh episode at attempt 1/2 with 30s backoff.
	working := stallThread(StatusInProgress, "working", 8, healthy.Turn)
	prev = snapshot(working, prev, now.Add(time.Minute))
	stallAgain := stallThread(StatusInProgress, "idle", 9, &PiTurn{ID: "a6", StopReason: "error", Error: "ACP stalled for 5m with no output"})
	p = planStallRetry(cfg, stallAgain, prev, false, now.Add(6*time.Minute), stallRetryGuard{})
	if p.Action != stallActionSchedule || p.Attempt != 1 || p.Backoff != 30*time.Second {
		t.Fatalf("fresh episode after reset should start at attempt 1/2 (30s), got %+v", p)
	}

	// Non-stall error turn (e.g. 429) also clears prior stall state and never auto-retries.
	prevErr := Snapshot{AgentStatus: "working", AgentSeq: 10, TurnID: "a6", StallRetries: 1, StallTurnID: "a6"}
	rateLimited := stallThread(StatusInProgress, "idle", 11, &PiTurn{ID: "a7", StopReason: "error", Error: "429 rate limited"})
	p = planStallRetry(cfg, rateLimited, prevErr, false, now.Add(7*time.Minute), stallRetryGuard{})
	if p.Action != stallActionReset || p.SuppressFinished || p.Next.hasState() {
		t.Fatalf("non-stall error should reset episode without suppressing, got %+v", p)
	}
}

func TestStallRetryNonRetryGuards(t *testing.T) {
	cfg := StallRetryConfig{MaxRetries: 2, BackoffSeconds: 30}
	now := t0
	stallTurn := &PiTurn{ID: "a2", StopReason: "error", Error: "ACP stalled for 5m with no output"}
	prevWorking := Snapshot{AgentStatus: "working", AgentSeq: 1, TurnID: "a1"}

	cases := []struct {
		name  string
		th    Thread
		guard stallRetryGuard
		first bool
	}{
		{"needs_me bead", stallThread(StatusNeedsMe, "idle", 2, stallTurn), stallRetryGuard{}, false},
		{"blocked bead", stallThread(StatusBlocked, "idle", 2, stallTurn), stallRetryGuard{}, false},
		{"closed bead", stallThread(StatusClosed, "idle", 2, stallTurn), stallRetryGuard{}, false},
		{"open bead", stallThread(StatusOpen, "idle", 2, stallTurn), stallRetryGuard{}, false},
		{"held via job HOLD", stallThread(StatusInProgress, "idle", 2, stallTurn), stallRetryGuard{Held: true}, false},
		{"headless owned", stallThread(StatusInProgress, "idle", 2, stallTurn), stallRetryGuard{Headless: true}, false},
		{"relaunch due", stallThread(StatusInProgress, "idle", 2, stallTurn), stallRetryGuard{Relaunching: true}, false},
		{"live pi ask prompt", stallThread(StatusInProgress, "idle", 2, stallTurn), stallRetryGuard{LiveAsk: true}, false},
		{"agent blocked at prompt", stallThread(StatusInProgress, "blocked", 2, stallTurn), stallRetryGuard{}, false},
		{
			"parsed approval prompt attached",
			func() Thread {
				th := stallThread(StatusInProgress, "idle", 2, stallTurn)
				th.Prompt = &ApprovalPrompt{Hash: "deadbeef", Lines: []string{"Bash command"}}
				return th
			}(),
			stallRetryGuard{},
			false,
		},
		{
			"non-pi agent",
			Thread{
				Bead:  Bead{ID: "task-1", Status: StatusInProgress},
				Agent: &Agent{Name: "task-1", Kind: "claude", Status: "idle", Seq: 2},
				Turn:  stallTurn,
			}.derive(),
			stallRetryGuard{},
			false,
		},
		{"first sight baseline", stallThread(StatusInProgress, "idle", 2, stallTurn), stallRetryGuard{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planStallRetry(cfg, tc.th, prevWorking, tc.first, now, tc.guard)
			if p.Action == stallActionSchedule || p.Action == stallActionRetry || p.SuppressFinished {
				t.Fatalf("%s unexpectedly scheduled/retried/suppressed: %+v", tc.name, p)
			}
		})
	}

	// Mid-backoff cancellations: a retry scheduled on tick 1 must never fire if
	// a guard trips before the backoff expires.
	pendingPrev := Snapshot{
		AgentStatus: "idle",
		AgentSeq:    2,
		TurnID:      "a2",
		StallTurnID: "a2",
		StallNextAt: now,
		StallError:  stallTurn.Error,
	}

	t.Run("mid-backoff becomes needs_me cancels without extra finished event", func(t *testing.T) {
		p := planStallRetry(cfg, stallThread(StatusNeedsMe, "idle", 2, stallTurn), pendingPrev, false, now.Add(time.Second), stallRetryGuard{})
		if p.Action != stallActionNone || !p.Next.NextAt.IsZero() || p.EscalateEvent != nil {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("mid-backoff placed on HOLD cancels and surfaces skipped stall once", func(t *testing.T) {
		th := stallThread(StatusInProgress, "idle", 2, stallTurn)
		p := planStallRetry(cfg, th, pendingPrev, false, now.Add(time.Second), stallRetryGuard{Held: true})
		if p.Action != stallActionEscalate || p.EscalateEvent == nil || !strings.Contains(p.EscalateEvent.Summary, "bead is on hold") || !p.Next.Escalated || !p.Next.NextAt.IsZero() {
			t.Fatalf("got %+v", p)
		}
		nextPrev := snapshot(th, pendingPrev, now.Add(time.Second))
		applyStallSnapshot(&nextPrev, p.Next)
		p2 := planStallRetry(cfg, th, nextPrev, false, now.Add(30*time.Second), stallRetryGuard{Held: true})
		if p2.Action != stallActionNone || p2.EscalateEvent != nil {
			t.Fatalf("held escalation duplicated: %+v", p2)
		}
	})

	t.Run("mid-backoff becomes blocked cancels and surfaces skipped stall", func(t *testing.T) {
		p := planStallRetry(cfg, stallThread(StatusBlocked, "idle", 2, stallTurn), pendingPrev, false, now.Add(time.Second), stallRetryGuard{})
		if p.Action != stallActionEscalate || p.EscalateEvent == nil || !strings.Contains(p.EscalateEvent.Summary, "bead status is blocked") || !p.Next.NextAt.IsZero() {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("mid-backoff live ask cancels without retrying", func(t *testing.T) {
		p := planStallRetry(cfg, stallThread(StatusInProgress, "idle", 2, stallTurn), pendingPrev, false, now.Add(time.Second), stallRetryGuard{LiveAsk: true})
		if p.Action == stallActionRetry || !p.Next.NextAt.IsZero() {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("mid-backoff agent resumes working cancels pending timer", func(t *testing.T) {
		p := planStallRetry(cfg, stallThread(StatusInProgress, "working", 3, stallTurn), pendingPrev, false, now.Add(time.Second), stallRetryGuard{})
		if p.Action != stallActionNone || !p.Next.NextAt.IsZero() {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("mid-backoff unreadable session waits safely instead of blind retry", func(t *testing.T) {
		p := planStallRetry(cfg, stallThread(StatusInProgress, "idle", 2, nil), pendingPrev, false, now.Add(time.Second), stallRetryGuard{})
		if p.Action != stallActionWait || p.Next.NextAt.IsZero() {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("mid-backoff config disabled surfaces previously suppressed stall once", func(t *testing.T) {
		disabled := StallRetryConfig{MaxRetries: 0, BackoffSeconds: 30}
		th := stallThread(StatusInProgress, "idle", 2, stallTurn)
		p := planStallRetry(disabled, th, pendingPrev, false, now.Add(time.Second), stallRetryGuard{})
		if p.Action != stallActionEscalate || p.EscalateEvent == nil || !strings.Contains(p.EscalateEvent.Summary, "auto-retry disabled") || !p.Next.Escalated || !p.Next.NextAt.IsZero() {
			t.Fatalf("got %+v", p)
		}
	})
}

func TestStallRetryZeroBackoffAndStateRestart(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := StallRetryConfig{MaxRetries: 2, BackoffSeconds: 0}
	now := t0
	prev := Snapshot{AgentStatus: "working", AgentSeq: 1, TurnID: "a1"}
	stall := stallThread(StatusInProgress, "idle", 2, &PiTurn{ID: "a2", StopReason: "error", Error: "ACP stalled for 5m with no output"})
	p := planStallRetry(cfg, stall, prev, false, now, stallRetryGuard{})
	if p.Action != stallActionRetry || !p.SuppressFinished || p.Attempt != 1 || p.Backoff != 0 {
		t.Fatalf("zero backoff plan = %+v", p)
	}

	// Verify state.json persistence across a ticker restart mid-backoff.
	snap := Snapshot{
		AgentStatus:    "idle",
		AgentSeq:       2,
		TurnID:         "a2",
		StallRetries:   1,
		StallTurnID:    "a3",
		StallNextAt:    now.Add(60 * time.Second),
		StallError:     "ACP stalled for 5m with no output",
		StallEscalated: false,
	}
	if err := saveState(TickerState{Threads: map[string]Snapshot{"task-1": snap}, PRs: map[string]PR{}}); err != nil {
		t.Fatal(err)
	}
	loaded := loadState().Threads["task-1"]
	if loaded.StallRetries != 1 || loaded.StallTurnID != "a3" || !loaded.StallNextAt.Equal(snap.StallNextAt) || loaded.StallError != snap.StallError {
		t.Fatalf("reloaded snapshot lost stall retry fields: %+v", loaded)
	}
}

func TestTickerStallRetryEndToEnd(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("PATH", dir)

	sessionFile := filepath.Join(t.TempDir(), "2026-11-24T10-00-00-000Z_task-1.jsonl")
	appendSession := func(line string) {
		t.Helper()
		f, err := os.OpenFile(sessionFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line + "\n"); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	appendSession(`{"type":"session","version":3,"id":"task-1","timestamp":"2026-11-24T10:00:00.000Z","cwd":"/w"}`)
	appendSession(`{"type":"message","id":"a1","timestamp":"2026-11-24T10:00:02.000Z","message":{"role":"assistant","content":[],"stopReason":"stop"}}`)

	promptFailFile := filepath.Join(dir, "prompt-fail")
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
  if [ -f "`+promptFailFile+`" ]; then
    echo "pane unavailable" >&2
    exit 1
  fi
  echo "$*" >> "$LOG/prompts.log"
  printf '{"result":{}}\n'
  exit 0
fi
printf '{"result":{}}\n'
`)

	setBeads := func(list ...Bead) { setJSONEnv("FIXTURE_BEADS", list) }
	setAgents := func(list ...Agent) { setJSONEnv("FIXTURE_AGENTS", list) }
	promptLines := func() []string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "prompts.log"))
		if err != nil {
			return nil
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}

	now := time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)
	cfg := defaultConfig()
	cfg.Repo = t.TempDir()
	cfg.GHSeconds = 3600
	cfg.VerifyMinutes = 0
	cfg.Nudge = false
	cfg.StallRetry = StallRetryConfig{MaxRetries: 2, BackoffSeconds: 30}

	var logs bytes.Buffer
	tk := newTicker(cfg, log.New(&logs, "", 0))
	tk.now = func() time.Time { return now }
	st := loadState()
	st.LastGH = now // skip gh pass

	makeAgent := func(status string, seq int64) Agent {
		return Agent{
			Name:    "task-1",
			Kind:    "pi",
			Status:  status,
			PaneID:  "w1:p1",
			Seq:     seq,
			Session: AgentSession{Agent: "pi", Kind: "path", Value: sessionFile},
		}
	}

	// Tick 0: baseline working worker.
	setBeads(Bead{ID: "task-1", Title: "Do work", Status: StatusInProgress})
	setAgents(makeAgent("working", 1))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 0: %v", err)
	}

	// Tick 1: worker stops on transient ACP stall (a2). Backoff = 30s -> no prompt, no inbox event yet.
	now = now.Add(15 * time.Second)
	appendSession(`{"type":"message","id":"a2","timestamp":"2026-11-24T14:00:10.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"ACP stalled for 5m with no output"}}`)
	setAgents(makeAgent("idle", 2))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if prompts := promptLines(); len(prompts) != 0 {
		t.Fatalf("prompt fired before backoff elapsed: %v", prompts)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("stall surfaced to inbox while auto-retry scheduled: %+v", items)
	}

	// Tick 2: +15s (still < 30s backoff) -> still waiting.
	now = now.Add(15 * time.Second)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if prompts := promptLines(); len(prompts) != 0 {
		t.Fatalf("prompt fired early: %v", prompts)
	}

	// Tick 3: +15s (30s total) -> retry 1/2 nudged + journaled + bead noted.
	now = now.Add(15 * time.Second)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if prompts := promptLines(); len(prompts) != 1 || !strings.Contains(prompts[0], "Auto-retry 1/2") {
		t.Fatalf("want retry 1/2 prompt, got %v", prompts)
	}
	if st.Threads["task-1"].StallRetries != 1 {
		t.Fatalf("StallRetries = %d, want 1", st.Threads["task-1"].StallRetries)
	}
	bdLog := strings.Join(logLines(t, dir, "bd"), "\n")
	if !strings.Contains(bdLog, "auto: ACP stall retry 1/2") {
		t.Fatalf("missing bead note for retry 1/2: %s", bdLog)
	}
	recs, err := readJournal(time.Time{})
	if err != nil || len(recs) != 1 || recs[0].Kind != journalStallRetry || recs[0].Action != "retry" || !recs[0].Pass {
		t.Fatalf("journal after retry 1 = %+v (err=%v)", recs, err)
	}

	// Tick 4: worker goes working, then stalls again (a3) -> backoff doubles to 60s.
	now = now.Add(15 * time.Second)
	setAgents(makeAgent("working", 3))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 4: %v", err)
	}
	now = now.Add(5 * time.Minute)
	appendSession(`{"type":"message","id":"a3","timestamp":"2026-11-24T14:06:00.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"ACP stalled for 5m with no output"}}`)
	setAgents(makeAgent("idle", 4))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 5: %v", err)
	}
	// +30s: must not fire yet (backoff is 60s).
	now = now.Add(30 * time.Second)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 6: %v", err)
	}
	if prompts := promptLines(); len(prompts) != 1 {
		t.Fatalf("retry 2 fired before 60s backoff: %v", prompts)
	}
	// +30s (60s total): retry 2/2 fires.
	now = now.Add(30 * time.Second)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 7: %v", err)
	}
	if prompts := promptLines(); len(prompts) != 2 || !strings.Contains(prompts[1], "Auto-retry 2/2") {
		t.Fatalf("want retry 2/2 prompt, got %v", prompts)
	}
	if st.Threads["task-1"].StallRetries != 2 {
		t.Fatalf("StallRetries = %d, want 2", st.Threads["task-1"].StallRetries)
	}

	// Third stall (a4): retry cap exhausted -> no 3rd nudge, clean escalation to inbox + journal + note.
	now = now.Add(15 * time.Second)
	setAgents(makeAgent("working", 5))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 8: %v", err)
	}
	now = now.Add(5 * time.Minute)
	appendSession(`{"type":"message","id":"a4","timestamp":"2026-11-24T14:12:00.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"ACP stalled for 5m with no output"}}`)
	setAgents(makeAgent("idle", 6))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 9: %v", err)
	}
	if prompts := promptLines(); len(prompts) != 2 {
		t.Fatalf("exhausted stall should not send a 3rd prompt: %v", prompts)
	}
	items, err := readInbox()
	if err != nil || len(items) != 1 || items[0].Kind != EventFinished || !strings.Contains(items[0].Summary, "after 2/2 auto-retries") {
		t.Fatalf("want 1 exhausted inbox item, got %+v (err=%v)", items, err)
	}
	recs, _ = readJournal(time.Time{})
	if len(recs) != 3 || recs[2].Kind != journalStallRetry || recs[2].Action != "exhausted" || recs[2].Pass {
		t.Fatalf("journal after exhaustion = %+v", recs)
	}
	// Another idle tick must not duplicate the escalation.
	now = now.Add(60 * time.Second)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 10: %v", err)
	}
	if items, _ := readInbox(); len(items) != 1 {
		t.Fatalf("exhaustion duplicated inbox items: %+v", items)
	}

	// Healthy turn (a5) resets counter; durable HOLD prevents retrying a later stall (a6).
	now = now.Add(15 * time.Second)
	setAgents(makeAgent("working", 7))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 11: %v", err)
	}
	now = now.Add(30 * time.Second)
	appendSession(`{"type":"message","id":"a5","timestamp":"2026-11-24T14:14:00.000Z","message":{"role":"assistant","content":[],"stopReason":"stop"}}`)
	setAgents(makeAgent("idle", 8))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 12: %v", err)
	}
	if st.Threads["task-1"].StallRetries != 0 || st.Threads["task-1"].StallEscalated {
		t.Fatalf("healthy turn did not reset snapshot: %+v", st.Threads["task-1"])
	}

	// Place bead on durable HOLD via job store, then stall again (a6): must NOT auto-retry.
	if err := setJobHold(jobHoldRecord{Bead: "task-1", JobID: "job-hold-1", By: "user", Why: "pause", At: now}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	setAgents(makeAgent("working", 9))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 13: %v", err)
	}
	now = now.Add(5 * time.Minute)
	appendSession(`{"type":"message","id":"a6","timestamp":"2026-11-24T14:19:00.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"ACP stalled for 5m with no output"}}`)
	setAgents(makeAgent("idle", 10))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 14: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 15: %v", err)
	}
	if prompts := promptLines(); len(prompts) != 2 {
		t.Fatalf("held bead was nudged: %v", prompts)
	}

	// Clear hold, recover with healthy turn a7, then verify prompt delivery failure
	// is also bounded and escalates cleanly instead of looping forever.
	if _, err := clearJobHold("task-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	appendSession(`{"type":"message","id":"a7","timestamp":"2026-11-24T14:21:00.000Z","message":{"role":"assistant","content":[],"stopReason":"stop"}}`)
	setAgents(makeAgent("idle", 11))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 16: %v", err)
	}
	if err := os.WriteFile(promptFailFile, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	setAgents(makeAgent("working", 12))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 17: %v", err)
	}
	now = now.Add(5 * time.Minute)
	appendSession(`{"type":"message","id":"a8","timestamp":"2026-11-24T14:26:00.000Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"ACP stalled for 5m with no output"}}`)
	setAgents(makeAgent("idle", 13))
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 18: %v", err)
	}
	// Backoff 1 (30s): prompt fails -> counted as attempt 1, rescheduled +60s.
	now = now.Add(30 * time.Second)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 19: %v", err)
	}
	if st.Threads["task-1"].StallRetries != 1 || st.Threads["task-1"].StallEscalated {
		t.Fatalf("after delivery failure 1: %+v", st.Threads["task-1"])
	}
	// Backoff 2 (60s): prompt fails -> reaches cap 2/2 -> escalates and stops.
	now = now.Add(60 * time.Second)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 20: %v", err)
	}
	if st.Threads["task-1"].StallRetries != 2 || !st.Threads["task-1"].StallEscalated || !st.Threads["task-1"].StallNextAt.IsZero() {
		t.Fatalf("after delivery failure 2: %+v", st.Threads["task-1"])
	}
	items, _ = readInbox()
	foundDeliveryEsc := false
	for _, it := range items {
		if it.Kind == EventFinished && strings.Contains(it.Summary, "auto-retry 2/2 failed") {
			foundDeliveryEsc = true
		}
	}
	if !foundDeliveryEsc {
		t.Fatalf("missing delivery-failure exhaustion inbox event: %+v", items)
	}
}
