package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestQuestionExtractionPrefersRealQuestionOverHousekeeping(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())

	t.Run("structured OPTIONS block survives trailing AUTO handoff and likely_stale rejected", func(t *testing.T) {
		base := "CRITICAL: destructive schema change\nQUESTION: Drop legacy archive table? Recommendation: 2 — keep 7d first\nOPTIONS:\n1. Drop now\n2. Keep 7 days\nCLASS: 8 — one prod delete"
		withHousekeeping := base + "\n" + strings.Join([]string{
			"AUTO: [coordinator L4 C3] handoff task-1; fresh handoff parked, no model turn or approval",
			"AUTO: [coordinator L4 C0] likely_stale rejected — still awaiting decision",
			"likely_stale rejected: keep open until reviewer replies",
			"MILESTONE: [review] info — waiting",
			"PR: https://github.com/example/repo/pull/55",
			"Linear: ENG-42",
			"2026-11-20 coordinator: agent session closed cleanly",
		}, "\n")

		wantQ, ok := parseNeedsMeQuestion(base)
		if !ok {
			t.Fatal("base OPTIONS block did not parse")
		}
		gotQ, ok := parseNeedsMeQuestion(withHousekeeping)
		if !ok {
			t.Fatal("OPTIONS block followed by housekeeping notes failed to parse")
		}
		if gotQ.Hash != wantQ.Hash {
			t.Errorf("question hash drifted across housekeeping notes: got %s, want %s", gotQ.Hash, wantQ.Hash)
		}
		if len(gotQ.Options) != 2 || gotQ.ClassN != 8 {
			t.Errorf("got question = %+v", gotQ)
		}
		if asked(Bead{Status: StatusNeedsMe, Notes: base}) != asked(Bead{Status: StatusNeedsMe, Notes: withHousekeeping}) {
			t.Error("asked() fingerprint changed after housekeeping notes")
		}
		if !isCriticalNeedsMe(Bead{ID: "task-1", Status: StatusNeedsMe, Notes: withHousekeeping}) {
			t.Error("expected CRITICAL to remain detected past housekeeping notes")
		}
		summary := needsMeSummaryNote(withHousekeeping, 200)
		if !strings.Contains(summary, "CRITICAL: destructive schema change") || !strings.Contains(summary, "Drop legacy archive table?") {
			t.Errorf("summary = %q", summary)
		}
		if strings.Contains(summary, "handoff") || strings.Contains(summary, "likely_stale") {
			t.Errorf("summary leaked housekeeping note: %q", summary)
		}
	})

	t.Run("free-text QUESTION and CRITICAL survive handoff and likely_stale notes", func(t *testing.T) {
		notes := strings.Join([]string{
			"investigated cache invalidation",
			"CRITICAL: rotating primary signing key",
			"QUESTION: Confirm maintenance window for key rotation?",
			"CLASS: 8 — secret write",
			"AUTO: [coordinator L4 C3] handoff task-2; fresh handoff parked",
			"likely_stale flag rejected — awaiting human reply",
		}, "\n")
		summary := needsMeSummaryNote(notes, 200)
		if !strings.Contains(summary, "CRITICAL: rotating primary signing key") || !strings.Contains(summary, "QUESTION: Confirm maintenance window for key rotation?") {
			t.Fatalf("summary = %q", summary)
		}
		if strings.Contains(summary, "handoff") || strings.Contains(summary, "likely_stale") {
			t.Fatalf("summary leaked housekeeping: %q", summary)
		}
		if !isCriticalNeedsMe(Bead{ID: "task-2", Status: StatusNeedsMe, Notes: notes}) {
			t.Error("expected CRITICAL to remain true")
		}
	})

	t.Run("RUN command and why survive trailing handoff and CLASS notes", func(t *testing.T) {
		notes := strings.Join([]string{
			"CLASS: 6 — non-prod auth",
			"RUN: aws sso login --profile dev-admin",
			"SSO session expired during preview",
			"AUTO: [coordinator L4 C3] handoff task-3; fresh handoff parked",
			"likely_stale rejected: still waiting for login",
		}, "\n")
		if got := pendingCommand(notes); got != "aws sso login --profile dev-admin" {
			t.Fatalf("pendingCommand = %q, want aws sso login --profile dev-admin", got)
		}
		if got := pendingCommandReason(notes); got != "SSO session expired during preview" {
			t.Fatalf("pendingCommandReason = %q, want SSO session expired during preview", got)
		}

		// Bare RUN without a why line must not surface the handoff note as question:.
		bareRun := "RUN: gh auth login\nAUTO: [coordinator L4 C3] handoff task-3b"
		if got := pendingCommand(bareRun); got != "gh auth login" {
			t.Fatalf("bare pendingCommand = %q", got)
		}
		if got := pendingCommandReason(bareRun); got != "" {
			t.Fatalf("bare pendingCommandReason = %q, want empty", got)
		}
		b := Bead{ID: "task-3b", Status: StatusNeedsMe, Title: "Auth refresh", Notes: bareRun}
		ctxOut := renderContext(Config{Repo: "/r"}, []Thread{thread(b, agent("idle", 1), nil)}, nil, nil, nil, TickerState{}, nil, t0)
		if !strings.Contains(ctxOut, "  run: gh auth login\n") || strings.Contains(ctxOut, "question:") || strings.Contains(ctxOut, "handoff") {
			t.Fatalf("context should show bare run without handoff question:\n%s", ctxOut)
		}
	})

	t.Run("latest directive wins over earlier directive and untagged status notes", func(t *testing.T) {
		notes := strings.Join([]string{
			"QUESTION: First question?",
			"AUTO: [coordinator L4 C3] handoff task-4",
			"QUESTION: Updated question after rebase?",
			"checked PR status, still waiting",
			"AUTO: [coordinator L4 C3] handoff task-4 again",
		}, "\n")
		summary := needsMeSummaryNote(notes, 200)
		if !strings.Contains(summary, "QUESTION: Updated question after rebase?") {
			t.Fatalf("expected latest QUESTION directive, got %q", summary)
		}
	})

	t.Run("answer boundaries retire older CRITICAL, QUESTION, and RUN notes", func(t *testing.T) {
		answered := strings.Join([]string{
			"CRITICAL: old prod delete",
			"QUESTION: Old question?",
			"OPTIONS:",
			"1. Yes",
			"2. No",
			"NEEDS_ME ANSWERED: 2 No to Old question? [12345678] by TJ via tap (device phone)",
			"QUESTION: New routine question?",
			"AUTO: [coordinator L4 C3] handoff task-5",
		}, "\n")
		if isCriticalNeedsMe(Bead{ID: "task-5", Status: StatusNeedsMe, Notes: answered}) {
			t.Error("old CRITICAL before NEEDS_ME ANSWERED leaked into new non-critical question")
		}
		summary := needsMeSummaryNote(answered, 200)
		if summary != "QUESTION: New routine question?" {
			t.Errorf("summary = %q, want QUESTION: New routine question?", summary)
		}

		// Answered RUN followed by housekeeping must stay cleared.
		ranNotes := "RUN: aws sso login\nRAN: logged in ok\nAUTO: [coordinator L4 C3] handoff task-6"
		if got := pendingCommand(ranNotes); got != "" {
			t.Errorf("answered RUN resurfaced past RAN + housekeeping: %q", got)
		}

		// Coordinator self-answer AUTO note acts as an answer boundary.
		coordAns := "QUESTION: Pick flag?\nOPTIONS:\n1. A\n2. B\nAUTO: [coordinator L4 C2] answered \"Pick flag?\": 1 — matches convention"
		if _, ok := parseNeedsMeQuestion(coordAns); ok {
			t.Error("coordinator-answered OPTIONS block should not parse as live")
		}
	})

	t.Run("untagged OPTIONS block preceded by question line preserves question over handoff", func(t *testing.T) {
		notes := "Should we keep the legacy flag?\nOPTIONS:\n1. Keep\n2. Drop\nAUTO: [coordinator L4 C3] handoff task-7"
		summary := needsMeSummaryNote(notes, 200)
		if summary != "Should we keep the legacy flag?" {
			t.Errorf("summary = %q, want question line", summary)
		}
	})

	t.Run("housekeeping-only bead falls back to lastNote for display", func(t *testing.T) {
		notes := "PR: https://github.com/example/repo/pull/1\nAUTO: [coordinator L4 C3] dispatched worker"
		if got := activeQuestionText(notes); got != "" {
			t.Errorf("activeQuestionText = %q, want empty", got)
		}
		if got := needsMeSummaryNote(notes, 200); got != "AUTO: [coordinator L4 C3] dispatched worker" {
			t.Errorf("needsMeSummaryNote fallback = %q", got)
		}
	})
}

func TestClassifyNeedsYouActiveVsParked(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	cfg := defaultConfig()

	old := now.Add(-10 * 24 * time.Hour)
	recent := now.Add(-2 * 24 * time.Hour)
	snoozeActive := SnoozeState{
		Version: 1,
		Beads: map[string]SnoozeEntry{
			"task-snoozed": {
				Until:       now.Add(24 * time.Hour),
				CreatedAt:   now.Add(-time.Hour),
				BeadUpdated: recent,
				Asked:       asked(Bead{Status: StatusNeedsMe, Notes: "QUESTION: Defer me?"}),
			},
			"task-snoozed-crit": {
				Until:       now.Add(24 * time.Hour),
				CreatedAt:   now.Add(-time.Hour),
				BeadUpdated: recent,
			},
			"task-snoozed-prompt": {
				Until:       now.Add(24 * time.Hour),
				CreatedAt:   now.Add(-time.Hour),
				BeadUpdated: recent,
			},
		},
	}

	// Mark a parked native pi-rpc worker so we verify migration-parked RPC workers
	// are NOT treated as live interactive agents or active approval prompts.
	if err := os.MkdirAll(rpcDir("task-rpc-parked"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rpcParkPath("task-rpc-parked"), []byte("parked"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		cfg        Config
		th         Thread
		wantParked bool
		wantReason string
	}{
		{
			name:       "live idle agent stays active even when old",
			cfg:        cfg,
			th:         thread(Bead{ID: "task-live", Status: StatusNeedsMe, UpdatedAt: old, Notes: "QUESTION: Proceed?"}, agent("idle", 1), nil),
			wantParked: false,
		},
		{
			name:       "active approval prompt overrides snooze",
			cfg:        cfg,
			th:         thread(Bead{ID: "task-snoozed-prompt", Status: StatusNeedsMe, UpdatedAt: recent, Notes: "QUESTION: Proceed?"}, agent("blocked", 1), nil),
			wantParked: false,
		},
		{
			name:       "recent agentless needs_me within 7d stays active",
			cfg:        cfg,
			th:         Thread{Bead: Bead{ID: "task-recent", Status: StatusNeedsMe, UpdatedAt: recent, Notes: "QUESTION: Proceed?"}},
			wantParked: false,
		},
		{
			name:       "old agentless non-critical needs_me is dormant parked",
			cfg:        cfg,
			th:         Thread{Bead: Bead{ID: "task-old", Status: StatusNeedsMe, UpdatedAt: old, Notes: "QUESTION: Proceed?"}},
			wantParked: true,
			wantReason: parkedReasonDormant,
		},
		{
			name:       "migration-parked pi-rpc agent older than threshold is dormant parked",
			cfg:        cfg,
			th:         Thread{Bead: Bead{ID: "task-rpc-parked", Status: StatusNeedsMe, UpdatedAt: old, Notes: "QUESTION: Proceed?"}, Agent: &Agent{Name: "task-rpc-parked", Kind: "pi", Backend: rpcBackend, Status: "blocked"}},
			wantParked: true,
			wantReason: parkedReasonDormant,
		},
		{
			name:       "unsnoozed CRITICAL stays active even when old and agentless",
			cfg:        cfg,
			th:         Thread{Bead: Bead{ID: "task-crit", Status: StatusNeedsMe, UpdatedAt: old, Notes: "CRITICAL: prod delete\nQUESTION: Confirm?"}},
			wantParked: false,
		},
		{
			name:       "explicitly snoozed CRITICAL moves to parked",
			cfg:        cfg,
			th:         Thread{Bead: Bead{ID: "task-snoozed-crit", Status: StatusNeedsMe, UpdatedAt: recent, Notes: "CRITICAL: prod delete\nQUESTION: Confirm?"}},
			wantParked: true,
			wantReason: parkedReasonSnoozed,
		},
		{
			name:       "explicitly snoozed bead with idle agent moves to parked",
			cfg:        cfg,
			th:         thread(Bead{ID: "task-snoozed", Status: StatusNeedsMe, UpdatedAt: recent, Notes: "QUESTION: Defer me?"}, agent("idle", 1), nil),
			wantParked: true,
			wantReason: parkedReasonSnoozed,
		},
		{
			name:       "custom needs_me_parked_days = 2 parks 3-day-old agentless bead",
			cfg:        Config{NeedsMeParkedDays: intPtr(2)},
			th:         Thread{Bead: Bead{ID: "task-3d", Status: StatusNeedsMe, UpdatedAt: now.Add(-3 * 24 * time.Hour), Notes: "QUESTION: Proceed?"}},
			wantParked: true,
			wantReason: parkedReasonDormant,
		},
		{
			name:       "needs_me_parked_days = 0 disables age-based parking",
			cfg:        Config{NeedsMeParkedDays: intPtr(0)},
			th:         Thread{Bead: Bead{ID: "task-old-disabled", Status: StatusNeedsMe, UpdatedAt: old, Notes: "QUESTION: Proceed?"}},
			wantParked: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotParked, gotReason, _ := classifyNeedsYou(tc.cfg, tc.th, snoozeActive, now)
			if gotParked != tc.wantParked || gotReason != tc.wantReason {
				t.Errorf("classifyNeedsYou = (%v, %q), want (%v, %q)", gotParked, gotReason, tc.wantParked, tc.wantReason)
			}
		})
	}
}

func TestSnoozeCLIDurationExpiryWakeAndClear(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	curNow := t0

	b := Bead{
		ID:        "task-defer",
		Title:     "Deferred rollout decision",
		Status:    StatusNeedsMe,
		UpdatedAt: t0.Add(-time.Hour),
		Notes:     "QUESTION: Ship next week?\nOPTIONS:\n1. Yes\n2. No",
	}
	env := snoozeEnv{
		show: func(id string) (Bead, error) {
			if id == "task-closed" {
				return Bead{ID: id, Status: StatusClosed}, nil
			}
			if id == b.ID {
				return b, nil
			}
			return Bead{}, errBeadNotFound
		},
		now: func() time.Time { return curNow },
	}

	// Duration parsing validation.
	for raw, want := range map[string]time.Duration{
		"30m":   30 * time.Minute,
		"4h":    4 * time.Hour,
		"1h30m": 90 * time.Minute,
		"3d":    72 * time.Hour,
		"2w":    14 * 24 * time.Hour,
	} {
		got, err := parseSnoozeDuration(raw)
		if err != nil || got != want {
			t.Errorf("parseSnoozeDuration(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, bad := range []string{"", "0h", "-2h", "0d", "-1d", "0w", "tomorrow", "3x"} {
		if _, err := parseSnoozeDuration(bad); err == nil {
			t.Errorf("parseSnoozeDuration(%q) unexpectedly succeeded", bad)
		}
	}

	// Closed bead refused.
	if err := snoozeCmd(defaultConfig(), env, []string{"task-closed", "2d"}, &bytes.Buffer{}); err == nil {
		t.Fatal("snoozing closed bead succeeded")
	}

	// Snooze for 2d with reason.
	var out bytes.Buffer
	if err := snoozeCmd(defaultConfig(), env, []string{"task-defer", "2d", "--reason", "waiting on review"}, &out); err != nil {
		t.Fatalf("snooze failed: %v", err)
	}
	if !strings.Contains(out.String(), "snoozed task-defer until") {
		t.Errorf("unexpected output: %q", out.String())
	}

	// Reload from disk and verify active before expiry.
	st := loadSnoozeState()
	e, ok := st.activeFor(b, curNow.Add(24*time.Hour))
	if !ok || e.Reason != "waiting on review" || !e.Until.Equal(curNow.Add(48*time.Hour)) {
		t.Fatalf("activeFor before expiry = (%+v, %v)", e, ok)
	}

	// Listing shows the active snooze in text and JSON.
	out.Reset()
	if err := snoozeCmd(defaultConfig(), env, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "task-defer") || !strings.Contains(out.String(), "waiting on review") {
		t.Errorf("list text = %q", out.String())
	}
	out.Reset()
	if err := snoozeCmd(defaultConfig(), env, []string{"--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var listed []struct {
		Bead   string    `json:"bead"`
		Until  time.Time `json:"until"`
		Reason string    `json:"reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].Bead != "task-defer" {
		t.Fatalf("list json = %q (err=%v)", out.String(), err)
	}

	// Expiry: at or past Until, snooze is no longer active.
	if _, ok := st.activeFor(b, curNow.Add(48*time.Hour)); ok {
		t.Error("snooze remained active at exact expiry timestamp")
	}
	if _, ok := st.activeFor(b, curNow.Add(49*time.Hour)); ok {
		t.Error("snooze remained active past expiry")
	}

	// Fresh activity wake 1: bead UpdatedAt advances past snoozed BeadUpdated.
	updatedBead := b
	updatedBead.UpdatedAt = curNow.Add(5 * time.Minute)
	if _, ok := st.activeFor(updatedBead, curNow.Add(time.Hour)); ok {
		t.Error("snooze did not wake when bead UpdatedAt advanced")
	}

	// Fresh activity wake 2: new question asked even if UpdatedAt is unchanged.
	newQuestionBead := b
	newQuestionBead.Notes += "\nNEEDS_ME ANSWERED: 1 Yes [11223344] by TJ\nQUESTION: New urgent question?"
	if _, ok := st.activeFor(newQuestionBead, curNow.Add(time.Hour)); ok {
		t.Error("snooze did not wake when active question changed")
	}

	// --clear removes the snooze immediately.
	out.Reset()
	if err := snoozeCmd(defaultConfig(), env, []string{"task-defer", "--clear"}, &out); err != nil {
		t.Fatalf("snooze --clear failed: %v", err)
	}
	if !strings.Contains(out.String(), "cleared snooze on task-defer") {
		t.Errorf("clear output = %q", out.String())
	}
	if _, ok := loadSnoozeState().activeFor(b, curNow.Add(time.Hour)); ok {
		t.Error("snooze still active after --clear")
	}
	// Idempotent second --clear succeeds cleanly.
	out.Reset()
	if err := snoozeCmd(defaultConfig(), env, []string{"task-defer", "--clear"}, &out); err != nil {
		t.Fatalf("second --clear failed: %v", err)
	}
	if !strings.Contains(out.String(), "was not snoozed") {
		t.Errorf("second clear output = %q", out.String())
	}
}

func TestConfigNeedsMeParkedDaysValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
	t.Setenv("QUARTERMASTER_STATE_DIR", dir)

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("repo = \"/tmp/r\"\nneeds_me_parked_days = 14\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("valid needs_me_parked_days failed: %v", err)
	}
	if cfg.needsMeParkedAfter() != 14*24*time.Hour {
		t.Errorf("needsMeParkedAfter = %v, want 14d", cfg.needsMeParkedAfter())
	}

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("repo = \"/tmp/r\"\nneeds_me_parked_days = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil || cfg.needsMeParkedAfter() != 0 {
		t.Fatalf("needs_me_parked_days=0: got %v, err %v", cfg.needsMeParkedAfter(), err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("repo = \"/tmp/r\"\nneeds_me_parked_days = -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "needs_me_parked_days") {
		t.Errorf("negative needs_me_parked_days: err = %v", err)
	}
}

func TestContextSeparatesActiveAndParkedNeedsMeAndPreservesQuestions(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	cfg := Config{Repo: "/r", Autonomy: AutonomyConfig{Level: 4, Max: 7}}

	activeHot := Bead{
		ID:        "app-active",
		Title:     "Hot rollout decision",
		Status:    StatusNeedsMe,
		UpdatedAt: now.Add(-2 * time.Hour),
		Notes:     "QUESTION: Enable canary in us-east?\nOPTIONS:\n1. Yes\n2. Wait\nCLASS: 7\nAUTO: [coordinator L4 C3] handoff app-active",
	}
	activeCrit := Bead{
		ID:        "app-crit",
		Title:     "Old critical decision",
		Status:    StatusNeedsMe,
		UpdatedAt: now.Add(-12 * 24 * time.Hour),
		Notes:     "CRITICAL: prod bucket deletion\nQUESTION: Delete legacy archive?\nAUTO: [coordinator L4 C3] likely_stale rejected",
	}
	parkedDormant := Bead{
		ID:        "app-dormant",
		Title:     "Old dormant thread",
		Status:    StatusNeedsMe,
		UpdatedAt: now.Add(-15 * 24 * time.Hour),
		Notes:     "QUESTION: Should we switch logging format?\nAUTO: [coordinator L4 C3] handoff app-dormant",
	}
	parkedSnoozed := Bead{
		ID:        "app-snoozed",
		Title:     "Deferred thread",
		Status:    StatusNeedsMe,
		UpdatedAt: now.Add(-time.Hour),
		Notes:     "QUESTION: Upgrade client library next sprint?\nlikely_stale rejected: deferred by user",
	}
	workingBead := Bead{ID: "app-work", Title: "Active worker", Status: StatusInProgress}

	if err := saveSnoozeState(SnoozeState{
		Version: 1,
		Beads: map[string]SnoozeEntry{
			"app-snoozed": {Until: now.Add(48 * time.Hour), CreatedAt: now.Add(-10 * time.Minute), BeadUpdated: parkedSnoozed.UpdatedAt},
		},
	}); err != nil {
		t.Fatal(err)
	}

	threads := []Thread{
		{Bead: parkedDormant},
		thread(activeHot, agent("idle", 1), nil),
		{Bead: parkedSnoozed},
		{Bead: activeCrit},
		thread(workingBead, agent("working", 1), nil),
	}

	full := renderContext(cfg, threads, nil, nil, nil, TickerState{}, nil, now)
	for _, want := range []string{
		"4 needs you (2 active, 2 parked)",
		"### Active needs you (2)",
		"### Parked / Dormant needs you (2: 1 snoozed, 1 dormant)",
		"question: Enable canary in us-east?",
		"options: 1 Yes · 2 Wait",
		"question: CRITICAL: prod bucket deletion QUESTION: Delete legacy archive?",
		"question: QUESTION: Should we switch logging format?",
		"question: QUESTION: Upgrade client library next sprint?",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("full context missing %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "handoff app-active") || strings.Contains(full, "handoff app-dormant") {
		t.Errorf("full context leaked handoff housekeeping note as question:\n%s", full)
	}
	if strings.Index(full, "### Active needs you (2)") >= strings.Index(full, "### Parked / Dormant needs you") {
		t.Errorf("active section must appear before parked section:\n%s", full)
	}
	if strings.Index(full, "app-active") >= strings.Index(full, "app-dormant") {
		t.Errorf("active bead must appear before dormant bead:\n%s", full)
	}

	// JSON output exposes active/parked summary counts and per-thread parked metadata.
	rawJSON, err := renderContextWith(cfg, threads, nil, nil, nil, TickerState{}, nil, now, ContextOptions{JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Summary struct {
			NeedsYou       int `json:"needs_you"`
			NeedsYouActive int `json:"needs_you_active"`
			NeedsYouParked int `json:"needs_you_parked"`
		} `json:"summary"`
		Threads []struct {
			Bead         string     `json:"bead"`
			Group        string     `json:"group"`
			Parked       bool       `json:"parked"`
			ParkedReason string     `json:"parked_reason"`
			SnoozedUntil *time.Time `json:"snoozed_until"`
			Question     string     `json:"question"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &doc); err != nil {
		t.Fatalf("invalid context JSON: %v\n%s", err, rawJSON)
	}
	if doc.Summary.NeedsYou != 4 || doc.Summary.NeedsYouActive != 2 || doc.Summary.NeedsYouParked != 2 {
		t.Fatalf("summary = %+v", doc.Summary)
	}
	// First two threads must be the two active needs_you beads, followed by the two parked ones.
	gotOrder := []string{doc.Threads[0].Bead, doc.Threads[1].Bead, doc.Threads[2].Bead, doc.Threads[3].Bead}
	if !slices.Equal(gotOrder, []string{"app-active", "app-crit", "app-dormant", "app-snoozed"}) {
		t.Fatalf("thread order = %v, want active before parked", gotOrder)
	}
	if doc.Threads[0].Parked || doc.Threads[1].Parked || !doc.Threads[2].Parked || !doc.Threads[3].Parked {
		t.Errorf("parked flags = %+v", doc.Threads[:4])
	}
	if !strings.Contains(doc.Threads[2].ParkedReason, "dormant") || !strings.Contains(doc.Threads[3].ParkedReason, "snoozed") || doc.Threads[3].SnoozedUntil == nil {
		t.Errorf("parked reasons = %q, %q (until=%v)", doc.Threads[2].ParkedReason, doc.Threads[3].ParkedReason, doc.Threads[3].SnoozedUntil)
	}
}

func TestDigestSeparatesActiveAndParkedNeedsMeAndPreservesQuestions(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0

	activeBead := Bead{
		ID:        "app-hot",
		Title:     "Active question",
		Status:    StatusNeedsMe,
		Priority:  2,
		UpdatedAt: now.Add(-3 * time.Hour),
		Notes:     "QUESTION: Proceed with regional rollout?\nAUTO: [coordinator L4 C3] handoff app-hot",
	}
	dormantBead := Bead{
		ID:        "app-old",
		Title:     "Dormant P1 question",
		Status:    StatusNeedsMe,
		Priority:  1, // higher priority, but parked -> must sort AFTER active!
		UpdatedAt: now.Add(-10 * 24 * time.Hour),
		Notes:     "QUESTION: Archive old dashboard?\nlikely_stale rejected: keeping around",
	}
	snoozedBead := Bead{
		ID:        "app-snz",
		Title:     "Snoozed P1 question",
		Status:    StatusNeedsMe,
		Priority:  1,
		UpdatedAt: now.Add(-time.Hour),
		Notes:     "QUESTION: Revisit quotas next month?\nAUTO: [coordinator L4 C3] handoff app-snz",
	}

	in := digestInputs{
		cfg: defaultConfig(),
		snoozes: SnoozeState{
			Version: 1,
			Beads: map[string]SnoozeEntry{
				"app-snz": {Until: now.Add(24 * time.Hour), CreatedAt: now.Add(-10 * time.Minute), BeadUpdated: snoozedBead.UpdatedAt},
			},
		},
		open: []Bead{dormantBead, snoozedBead, activeBead},
	}

	d, _ := buildDigest(in, digestState{}, nil, now)
	md := renderDigest(d)
	for _, want := range []string{
		"1 need you (2 parked)",
		"## Needs you — Active (1)",
		"### Parked / Dormant (2: 1 snoozed, 1 dormant)",
		"QUESTION: Proceed with regional rollout?",
		"QUESTION: Archive old dashboard?",
		"QUESTION: Revisit quotas next month?",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("digest markdown missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "handoff app-hot") || strings.Contains(md, "handoff app-snz") {
		t.Errorf("digest markdown leaked handoff housekeeping note:\n%s", md)
	}
	if strings.Index(md, "app-hot") >= strings.Index(md, "### Parked / Dormant") {
		t.Errorf("active item must appear before Parked / Dormant heading:\n%s", md)
	}
	if !d.worthNotifying() {
		t.Error("digest with 1 active needs_me should notify")
	}
	_, body := d.summary(3)
	if !strings.Contains(body, "app-hot") || !strings.Contains(body, "(2 parked / dormant)") || strings.Contains(body, "app-old") {
		t.Errorf("digest summary should list active items + parked count only:\n%s", body)
	}

	// JSON separates needs_you (active) and needs_you_parked (parked).
	raw, err := renderDigestJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	var dj digestJSON
	if err := json.Unmarshal(raw, &dj); err != nil {
		t.Fatalf("invalid digest JSON: %v\n%s", err, raw)
	}
	if dj.Summary.NeedsYou != 3 || dj.Summary.NeedsYouActive != 1 || dj.Summary.NeedsYouParked != 2 {
		t.Errorf("digest JSON summary = %+v", dj.Summary)
	}
	if len(dj.NeedsYou) != 1 || dj.NeedsYou[0].Bead != "app-hot" || dj.NeedsYou[0].Parked {
		t.Errorf("digest JSON needs_you = %+v", dj.NeedsYou)
	}
	if len(dj.NeedsYouParked) != 2 || !dj.NeedsYouParked[0].Parked || !dj.NeedsYouParked[1].Parked {
		t.Errorf("digest JSON needs_you_parked = %+v", dj.NeedsYouParked)
	}

	// When ONLY parked beads remain and nothing changed, digest stays quiet.
	inParkedOnly := in
	inParkedOnly.open = []Bead{dormantBead, snoozedBead}
	dQuiet, _ := buildDigest(inParkedOnly, digestState{}, nil, now)
	if dQuiet.worthNotifying() {
		t.Error("digest with only parked needs_me and no changes should not interrupt")
	}
	if !strings.Contains(dQuiet.quiet(), "nothing active needed you (2 parked)") {
		t.Errorf("quiet line = %q", dQuiet.quiet())
	}
}

func TestTapInboxAndCLIQuestionsSeparateActiveAndParked(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	cfg := defaultConfig()
	cfg.Tap = TapConfig{Enabled: true, Devices: []string{"iphone181"}}

	activeBead := Bead{
		ID:        "task-active",
		Status:    StatusNeedsMe,
		Title:     "Active rollout choice",
		UpdatedAt: now.Add(-time.Hour),
		Notes:     "QUESTION: Ship canary now?\nOPTIONS:\n1. Ship\n2. Wait\nCLASS: 7 — prod\nAUTO: [coordinator L4 C3] handoff task-active",
	}
	critBead := Bead{
		ID:        "task-crit",
		Status:    StatusNeedsMe,
		Title:     "Old critical decision",
		UpdatedAt: now.Add(-14 * 24 * time.Hour),
		Notes:     "CRITICAL: prod delete\nQUESTION: Delete legacy bucket?\nOPTIONS:\n1. Delete\n2. Keep\nCLASS: 8 — prod delete\nlikely_stale rejected: waiting",
	}
	dormantBead := Bead{
		ID:        "task-dormant",
		Status:    StatusNeedsMe,
		Title:     "Dormant question",
		UpdatedAt: now.Add(-12 * 24 * time.Hour),
		Notes:     "QUESTION: Refactor old tool?\nOPTIONS:\n1. Yes\n2. No\nAUTO: [coordinator L4 C3] handoff task-dormant",
	}
	snoozedBead := Bead{
		ID:        "task-snoozed",
		Status:    StatusNeedsMe,
		Title:     "Snoozed question",
		UpdatedAt: now.Add(-30 * time.Minute),
		Notes:     "QUESTION: Upgrade library later?\nOPTIONS:\n1. Now\n2. Later",
	}

	snoozes := SnoozeState{
		Version: 1,
		Beads: map[string]SnoozeEntry{
			"task-snoozed": {Until: now.Add(24 * time.Hour), CreatedAt: now.Add(-5 * time.Minute), BeadUpdated: snoozedBead.UpdatedAt},
		},
	}
	beads := []Bead{activeBead, critBead, dormantBead, snoozedBead}

	s := newTestInboxServer(t, cfg, promptEnv{}, &tapInboxEnv{
		active:     func() ([]Bead, error) { return beads, nil },
		loadSnooze: func() SnoozeState { return snoozes },
	}, "iphone181")
	s.now = func() time.Time { return now }

	// 1. /inbox.json separates active questions vs parked_questions and preserves real OPTIONS questions.
	jsonRec := inboxRequest(s, http.MethodGet, "/inbox.json", "", tapPhoneAddr)
	if jsonRec.Code != http.StatusOK {
		t.Fatalf("GET /inbox.json = %d %q", jsonRec.Code, jsonRec.Body.String())
	}
	var snap inboxSnapshot
	if err := json.Unmarshal(jsonRec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Questions != 2 || snap.Counts.ParkedQuestions != 2 || snap.Counts.Total() != 2 {
		t.Fatalf("counts = %+v, want 2 active and 2 parked", snap.Counts)
	}
	activeIDs := []string{snap.Questions[0].Bead, snap.Questions[1].Bead}
	if !slices.Equal(activeIDs, []string{"task-crit", "task-active"}) {
		t.Fatalf("active questions = %v, want [task-crit task-active]", activeIDs)
	}
	if snap.Questions[1].Kind != "options" || !snap.Questions[1].Tappable || !strings.Contains(snap.Questions[1].Question, "Ship canary now?") {
		t.Errorf("active question lost real OPTIONS past handoff: %+v", snap.Questions[1])
	}
	parkedIDs := []string{snap.ParkedQuestions[0].Bead, snap.ParkedQuestions[1].Bead}
	if !slices.Equal(parkedIDs, []string{"task-dormant", "task-snoozed"}) {
		t.Fatalf("parked questions = %v, want [task-dormant task-snoozed]", parkedIDs)
	}
	if !snap.ParkedQuestions[0].Parked || !strings.Contains(snap.ParkedQuestions[0].ParkedReason, "dormant") || snap.ParkedQuestions[0].Kind != "options" {
		t.Errorf("dormant parked question = %+v", snap.ParkedQuestions[0])
	}
	if !snap.ParkedQuestions[1].Parked || !strings.Contains(snap.ParkedQuestions[1].ParkedReason, "snoozed") || snap.ParkedQuestions[1].SnoozedUntil == nil {
		t.Errorf("snoozed parked question = %+v", snap.ParkedQuestions[1])
	}

	// 2. /inbox HTML renders active section first, collapsed <details id="parked"> with summary count, and no handoff noise.
	htmlRec := inboxRequest(s, http.MethodGet, "/inbox", "", tapPhoneAddr)
	if htmlRec.Code != http.StatusOK {
		t.Fatalf("GET /inbox = %d %q", htmlRec.Code, htmlRec.Body.String())
	}
	html := htmlRec.Body.String()
	for _, want := range []string{
		"Needs-Me Questions &amp; Cards — Active (2)",
		`href="#parked">Parked <span class="n">2</span>`,
		`<details id="parked" class="parked-box">`,
		"Parked / Dormant (2: 1 snoozed, 1 dormant)",
		"Ship canary now?",
		"Refactor old tool?",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("/inbox HTML missing %q", want)
		}
	}
	if strings.Index(html, "task-active") >= strings.Index(html, `<details id="parked"`) {
		t.Error("active question did not render before parked disclosure")
	}
	if strings.Contains(html, "handoff task-active") || strings.Contains(html, "handoff task-dormant") {
		t.Error("/inbox HTML leaked handoff housekeeping note")
	}

	// 3. `quartermaster inbox --questions` excludes parked/snoozed beads and keeps active ones.
	qs, err := inboxQuestions(inboxQuestionsEnv{
		cfg:     cfg,
		active:  func() ([]Bead, error) { return beads, nil },
		snoozes: func() SnoozeState { return snoozes },
		now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	gotCLI := make([]string, len(qs))
	for i, q := range qs {
		gotCLI[i] = q.Bead
	}
	if !slices.Equal(gotCLI, []string{"task-active", "task-crit"}) {
		t.Fatalf("inbox --questions = %v, want only active [task-active task-crit]", gotCLI)
	}
}

func TestCompactContextPacksParkedNeedsMeBeforeActive(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	cfg := Config{Repo: "/r", Autonomy: AutonomyConfig{Level: 4, Max: 7}}

	var threads []Thread
	for i := 0; i < 3; i++ {
		threads = append(threads, thread(Bead{
			ID:        "app-active-" + string(rune('a'+i)),
			Title:     "Active thread",
			Status:    StatusNeedsMe,
			UpdatedAt: now.Add(-time.Hour),
			Notes:     "QUESTION: Proceed now?",
		}, agent("idle", 1), nil))
	}
	// Many dormant parked needs_me beads to push compact context past the 4 KB budget.
	for i := 0; i < 120; i++ {
		id := "app-parked-" + strings.Repeat("x", 10) + "-" + time.Duration(i).String()
		threads = append(threads, Thread{Bead: Bead{
			ID:        id,
			Title:     "Dormant parked thread",
			Status:    StatusNeedsMe,
			UpdatedAt: now.Add(-20 * 24 * time.Hour),
			Notes:     "QUESTION: Old deferred question?",
		}})
	}

	compact, err := renderContextWith(cfg, threads, nil, nil, nil, TickerState{}, nil, now, ContextOptions{Compact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(compact) >= compactContextBudget {
		t.Fatalf("compact context exceeded 4 KB budget: %d bytes", len(compact))
	}
	// Active needs_me threads remain listed individually while parked ones fold first.
	for _, want := range []string{
		"- app-active-a · needs_you",
		"- app-active-b · needs_you",
		"- app-active-c · needs_you",
		"- needs_me parked (120):",
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("compact context missing %q:\n%s", want, compact)
		}
	}
}

func TestReportPreservesRealQuestionOverHousekeeping(t *testing.T) {
	now := t0
	r := buildReport(now.Add(-24*time.Hour), nil, []Bead{
		{
			ID:        "task-rpt",
			Title:     "Retention choice",
			Status:    StatusNeedsMe,
			UpdatedAt: now.Add(-2 * time.Hour),
			Notes:     "QUESTION: Keep 30d or 90d retention?\nAUTO: [coordinator L4 C3] handoff task-rpt\nlikely_stale rejected: awaiting reply",
		},
	}, nil, TickerState{}, "")
	md := renderReport(r, now)
	if !strings.Contains(md, "QUESTION: Keep 30d or 90d retention?") {
		t.Fatalf("report missing real question:\n%s", md)
	}
	if strings.Contains(md, "handoff task-rpt") || strings.Contains(md, "likely_stale") {
		t.Fatalf("report leaked housekeeping note:\n%s", md)
	}
}
