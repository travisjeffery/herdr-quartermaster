package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestOptimisticAnswerFeedbackNeedsMeOptionAndRollback(t *testing.T) {
	qNote := strings.Join([]string{
		"QUESTION: Close superseded PR #40? Recommendation: 1 — already merged.",
		"OPTIONS:",
		"1. Close PR and bead",
		"2. Keep PR open",
		"CLASS: 3 — close superseded PR",
	}, "\n")
	f := &fakeBoard{
		beads: []Bead{
			{ID: "bk-decide", Title: "Close superseded PR", Status: StatusNeedsMe, Priority: 1, Notes: qNote, UpdatedAt: t0},
			{ID: "bk-next", Title: "Second question", Status: StatusNeedsMe, Priority: 2, Notes: qNote, UpdatedAt: t0},
		},
		agents: []Agent{
			{Name: "bk-decide", Kind: "pi", Status: "idle", PaneID: "w:p1"},
		},
	}
	m := testModel(t, f)
	selectBead(t, m, "bk-decide")

	// Step 1: Select option 1 and confirm without running the async command yet.
	press(t, m, "1")
	inFlight := m.key("y")
	if len(inFlight) != 1 {
		t.Fatalf("expected 1 in-flight cmd, got %d", len(inFlight))
	}

	// Row, standard view, compact view, wide split detail pane, and stacked detail
	// pane all immediately reflect the in-flight submission.
	if r := m.sel(); r == nil || r.pending == nil || r.pending.phase != pendingAnswerSending || r.pending.option != 1 {
		t.Fatalf("selected row missing immediate pendingAnswer: %+v", r)
	}
	stdScreen := strings.Join(screenLines(renderBoard(m, 100, 24)), "\n")
	if !strings.Contains(stdScreen, "✓ sending: 1. Close PR and bead") || !strings.Contains(stdScreen, "✓ Submitting: 1. Close PR and bead") {
		t.Fatalf("standard board missing immediate in-flight feedback:\n%s", stdScreen)
	}
	compactScreen := strings.Join(screenLines(renderBoard(m, 60, 22)), "\n")
	if !strings.Contains(compactScreen, "[SENDING]") || !strings.Contains(compactScreen, "✓ sending: 1.") {
		t.Fatalf("compact board missing immediate in-flight feedback:\n%s", compactScreen)
	}
	wideScreen := strings.Join(screenLines(renderBoard(m, 150, 30)), "\n")
	if !strings.Contains(wideScreen, "✓ Submitting: 1. Close PR and bead (saving…)") || !strings.Contains(wideScreen, "✓ 1. Close PR and bead ★ (sending…)") {
		t.Fatalf("wide split detail missing immediate in-flight banner/option highlight:\n%s", wideScreen)
	}
	// Clicking option 1 in the wide right pane must NOT record an option hit while in flight.
	if len(m.hits.optionRows) != 0 {
		t.Fatalf("expected option click targets disabled while answer is in flight, got %v", m.hits.optionRows)
	}
	detText := strings.Join(texts(detailLines(*m.sel(), m.details["bk-decide"], 80)), "\n")
	if !strings.Contains(detText, "✓ Submitting: 1. Close PR and bead (saving…)") || !strings.Contains(detText, "✓ 1. Close PR and bead ★ (sending…)") {
		t.Fatalf("detailLines missing immediate in-flight feedback:\n%s", detText)
	}

	// Duplicate submissions via 1-9 or f are blocked while in flight.
	for _, k := range []string{"1", "2", "f"} {
		if cmds := m.key(k); len(cmds) != 0 || m.ask != nil || !strings.Contains(m.status, "already in flight") {
			t.Fatalf("key %q was not blocked while in flight: cmds=%d ask=%+v status=%q", k, len(cmds), m.ask, m.status)
		}
	}

	// Step 2: Simulate write failure -> cleanly rolls back row & detail with error status.
	f.fail = errors.New("bd note failed: locked")
	drive(t, m, inFlight)
	if !strings.Contains(m.status, "bd note failed: locked") {
		t.Fatalf("expected error in status, got %q", m.status)
	}
	if m.pendingAnswerFor("bk-decide") != nil || m.sel().pending != nil {
		t.Fatalf("pendingAnswer did not roll back on error: %+v", m.pendingAnswers)
	}
	reverted := strings.Join(screenLines(renderBoard(m, 150, 30)), "\n")
	if strings.Contains(reverted, "✓ Submitting:") || strings.Contains(reverted, "✓ sending:") {
		t.Fatalf("in-flight indicator remained after error rollback:\n%s", reverted)
	}

	// Step 3: Retry now succeeds -> verify intermediate "answered" state survives
	// a stale overlapping beadsMsg before clearing on the fresh reload.
	f.fail = nil
	staleLoad := m.loadBeads() // started BEFORE the write finishes
	press(t, m, "1")
	inFlight = m.key("y")
	// Apply only the write doneMsg (before reload finishes).
	done := inFlight[0]()
	reloadCmds := m.apply(done)
	if pa := m.pendingAnswerFor("bk-decide"); pa == nil || pa.phase != pendingAnswerSent {
		t.Fatalf("expected pendingAnswerSent after doneMsg before reload, got %+v", pa)
	}
	sentScreen := strings.Join(screenLines(renderBoard(m, 150, 30)), "\n")
	if !strings.Contains(sentScreen, "✓ answered: 1. Close PR and bead") || !strings.Contains(sentScreen, "✓ Answered: 1. Close PR and bead (updating board…)") {
		t.Fatalf("expected optimistic answered indicator while reload is pending:\n%s", sentScreen)
	}
	// Duplicate submission stays blocked during pendingAnswerSent.
	if cmds := m.key("1"); len(cmds) != 0 || m.ask != nil || !strings.Contains(m.status, "already answered") {
		t.Fatalf("expected duplicate guard during pendingAnswerSent: cmds=%d ask=%+v status=%q", len(cmds), m.ask, m.status)
	}

	// Stale load (seq <= clearAfterSeq) must NOT prematurely clear pendingAnswerSent.
	staleMsg := staleLoad()
	// Make stale snapshot still look like StatusNeedsMe (simulating pre-write snapshot).
	if bm, ok := staleMsg.(beadsMsg); ok {
		for i := range bm.beads {
			if bm.beads[i].ID == "bk-decide" {
				bm.beads[i].Status = StatusNeedsMe
			}
		}
		staleMsg = bm
	}
	freshCmds := m.apply(staleMsg)
	if pa := m.pendingAnswerFor("bk-decide"); pa == nil || pa.phase != pendingAnswerSent {
		t.Fatalf("stale beadsMsg prematurely cleared pendingAnswerSent: %+v", pa)
	}

	// Now let the queued fresh reload complete -> clears pending state and keeps cursor on bk-next!
	drive(t, m, append(reloadCmds, freshCmds...))
	if m.pendingAnswerFor("bk-decide") != nil {
		t.Fatalf("fresh beadsMsg did not clear pendingAnswerSent: %+v", m.pendingAnswers)
	}
	if r := m.sel(); r == nil || r.bead.ID != "bk-next" || r.group != GroupNeedsYou {
		t.Fatalf("after reload moved bk-decide out of needs you, selected %+v, want bk-next", r)
	}
}

func TestOptimisticAnswerFeedbackCustomPromptRunAndDirect(t *testing.T) {
	// 1. Custom free-text needs_me answer (t) shows immediate indicator + rolls back on error.
	qNote := "QUESTION: Which region? Recommendation: 1 — us-east.\nOPTIONS:\n1. us-east\n2. eu-west\nCLASS: 4 — config"
	f := &fakeBoard{
		beads: []Bead{
			{ID: "bk-custom", Title: "Region choice", Status: StatusNeedsMe, Priority: 1, Notes: qNote, UpdatedAt: t0},
			{ID: "bk-prompt", Title: "Approve bash", Status: StatusInProgress, Priority: 1, UpdatedAt: t0},
			{ID: "bk-run", Title: "Refresh SSO", Status: StatusNeedsMe, Priority: 1, Notes: "RUN: aws sso login\nwhy: expired", UpdatedAt: t0},
			{ID: "bk-direct", Title: "Idle worker", Status: StatusInProgress, Priority: 2, UpdatedAt: t0},
		},
		agents: []Agent{
			{Name: "bk-prompt", Kind: "pi", Status: "blocked", PaneID: "w:p1"},
			{Name: "bk-direct", Kind: "claude", Status: "idle", PaneID: "w:p2"},
		},
		prompts: map[string]ApprovalPrompt{},
	}
	p := ApprovalPrompt{
		Lines: []string{"Bash command", "go test ./...", "Allow?"},
		Options: []PromptOption{
			{Number: 1, Label: "Yes", Selected: true},
			{Number: 2, Label: "No"},
		},
		Numbered: true,
	}
	p.Hash = promptHash(p)
	f.prompts["bk-prompt"] = p

	env := wireExtendedFakeEnv(f, f.env(), &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}})
	var runErr error
	origRun := env.completeRun
	env.completeRun = func(id, command, result string) (string, error) {
		if runErr != nil {
			return "", runErr
		}
		return origRun(id, command, result)
	}
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0

	// Custom text in viewAnswer.
	selectBead(t, m, "bk-custom")
	press(t, m, "A", "t")
	typeText(t, m, "use ap-south\r")
	inFlight := m.key("y")
	ansScreen := strings.Join(screenLines(renderBoard(m, 90, 24)), "\n")
	if !strings.Contains(ansScreen, "✓ Submitting: custom: use ap-south (saving…)") || !strings.Contains(ansScreen, "sending…") {
		t.Fatalf("viewAnswer missing in-flight custom answer banner:\n%s", ansScreen)
	}
	if cmds := m.key("t"); len(cmds) != 0 || m.ask != nil || !strings.Contains(m.status, "already in flight") {
		t.Fatalf("duplicate custom answer was not blocked in viewAnswer: ask=%+v status=%q", m.ask, m.status)
	}
	// Error rolls back cleanly while keeping viewAnswer open.
	f.fail = errors.New("write failed")
	drive(t, m, inFlight)
	if m.view != viewAnswer || m.pendingAnswerFor("bk-custom") != nil || !strings.Contains(m.status, "write failed") {
		t.Fatalf("custom answer error rollback failed: view=%v pending=%+v status=%q", m.view, m.pendingAnswers, m.status)
	}
	f.fail = nil
	press(t, m, "q")

	// 2. Approval prompt option (confirmPromptOption) shows immediate banner & option ✓ + rollback on error.
	selectBead(t, m, "bk-prompt")
	press(t, m, "A", "1")
	inFlight = m.key("y")
	promptScreen := strings.Join(screenLines(renderBoard(m, 90, 24)), "\n")
	if !strings.Contains(promptScreen, "✓ Submitting: 1. Yes (saving…)") || !strings.Contains(promptScreen, "✓ 1. Yes (sending…)") {
		t.Fatalf("prompt viewAnswer missing in-flight banner or option checkmark:\n%s", promptScreen)
	}
	if len(m.hits.optionRows) != 0 {
		t.Fatalf("prompt option clicks should be disabled while in flight: %v", m.hits.optionRows)
	}
	// Check narrow/wide no-overflow while in flight.
	for _, w := range []int{42, 60, 80, 140} {
		for row, l := range screenLines(renderBoard(m, w, 24)) {
			if n := utf8.RuneCountInString(l); n > w {
				t.Errorf("viewAnswer in-flight width=%d row %d overflow (%d): %q", w, row, n, l)
			}
		}
	}
	f.fail = errors.New("prompt send failed")
	drive(t, m, inFlight)
	if m.view != viewAnswer || m.pendingAnswerFor("bk-prompt") != nil || !strings.Contains(m.status, "prompt send failed") {
		t.Fatalf("prompt error rollback failed: view=%v pending=%+v status=%q", m.view, m.pendingAnswers, m.status)
	}
	f.fail = nil
	press(t, m, "q")

	// 3. RUN completion shows immediate banner on viewAnswer and board row + rolls back on error.
	selectBead(t, m, "bk-run")
	press(t, m, "A", "d")
	typeText(t, m, "logged in\r")
	inFlight = m.key("y")
	runScreen := strings.Join(screenLines(renderBoard(m, 90, 24)), "\n")
	if !strings.Contains(runScreen, "✓ Submitting: RAN: logged in (saving…)") {
		t.Fatalf("RUN viewAnswer missing in-flight banner:\n%s", runScreen)
	}
	if cmds := m.key("d"); len(cmds) != 0 || m.ask != nil || !strings.Contains(m.status, "already in flight") {
		t.Fatalf("duplicate RUN completion was not blocked: ask=%+v status=%q", m.ask, m.status)
	}
	runErr = errors.New("bd unavailable")
	drive(t, m, inFlight)
	if m.view != viewAnswer || m.pendingAnswerFor("bk-run") != nil || !strings.Contains(m.status, "bd unavailable") {
		t.Fatalf("RUN rollback failed: view=%v pending=%+v status=%q", m.view, m.pendingAnswers, m.status)
	}
	runErr = nil
	press(t, m, "q")

	// 4. Direct worker reply (answer) shows immediate feedback on row & detail and clears on completion.
	selectBead(t, m, "bk-direct")
	press(t, m, "A")
	typeText(t, m, "check logs\r")
	inFlight = m.key("y")
	if r := m.sel(); r == nil || r.pending == nil || !strings.Contains(r.line, "✓ sending: reply: check logs") {
		t.Fatalf("direct reply missing immediate row feedback: %+v", r)
	}
	directDetail := strings.Join(texts(detailLines(*m.sel(), nil, 80)), "\n")
	if !strings.Contains(directDetail, "✓ Submitting: reply: check logs (saving…)") {
		t.Fatalf("direct reply missing detail banner:\n%s", directDetail)
	}
	if cmds := m.key("A"); len(cmds) != 0 || m.ask != nil || !strings.Contains(m.status, "already in flight") {
		t.Fatalf("duplicate direct reply was not blocked: ask=%+v status=%q", m.ask, m.status)
	}
	drive(t, m, inFlight)
	if m.pendingAnswerFor("bk-direct") != nil {
		t.Fatalf("direct reply pending state did not clear on doneMsg: %+v", m.pendingAnswers)
	}
}

func TestOptimisticAnswerFeedbackInDecisionQueueAndLayouts(t *testing.T) {
	f := queueFixtureFleet()
	env := wireExtendedFakeEnv(f, f.env(), &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}})
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0

	press(t, m, "Q")
	for i, it := range m.queue {
		if it.row.bead.ID == "bk-decide" {
			m.queueCur = i
			break
		}
	}
	press(t, m, "1")
	inFlight := m.key("y")
	if len(inFlight) != 1 {
		t.Fatalf("expected 1 in-flight queue cmd, got %d", len(inFlight))
	}

	// Queue card and wide split right pane immediately show SENDING + Submitting banner.
	for _, sz := range []struct{ w, h int }{{42, 20}, {70, 24}, {150, 30}} {
		lines := screenLines(renderBoard(m, sz.w, sz.h))
		if len(lines) != sz.h {
			t.Fatalf("queue %dx%d: %d lines, want %d", sz.w, sz.h, len(lines), sz.h)
		}
		for row, l := range lines {
			if n := utf8.RuneCountInString(l); n > sz.w {
				t.Errorf("queue in-flight %dx%d row %d overflow (%d): %q", sz.w, sz.h, row, n, l)
			}
		}
		screen := strings.Join(lines, "\n")
		if !strings.Contains(screen, "SENDING") && !strings.Contains(screen, "✓ sending:") {
			t.Errorf("queue %dx%d missing in-flight indicator:\n%s", sz.w, sz.h, screen)
		}
	}
	// Duplicate pick in queue is rejected while in flight.
	if cmds := m.key("1"); len(cmds) != 0 || m.ask != nil || !strings.Contains(m.status, "already in flight") {
		t.Fatalf("duplicate queue pick not blocked: ask=%+v status=%q", m.ask, m.status)
	}
	drive(t, m, inFlight)
	if !slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "bk-decide choice=1") }) {
		t.Fatalf("expected answer-needs-me call, got %v", f.calls)
	}
}

func TestOptimisticAnswerCloseDoesNotCloseDifferentAnswerView(t *testing.T) {
	qNote := "QUESTION: Proceed? Recommendation: 1 — yes.\nOPTIONS:\n1. Yes\n2. No\nCLASS: 3 — routine"
	f := &fakeBoard{
		beads: []Bead{
			{ID: "bk-a", Title: "First", Status: StatusNeedsMe, Priority: 1, Notes: qNote, UpdatedAt: t0},
			{ID: "bk-b", Title: "Second", Status: StatusNeedsMe, Priority: 2, Notes: qNote, UpdatedAt: t0},
		},
	}
	m := testModel(t, f)
	selectBead(t, m, "bk-a")
	press(t, m, "A", "1")
	inFlight := m.key("y")

	// User exits bk-a's answer view and opens bk-b's answer view before bk-a finishes.
	press(t, m, "q")
	selectBead(t, m, "bk-b")
	press(t, m, "A")
	if m.view != viewAnswer || m.ans == nil || m.ans.beadID != "bk-b" {
		t.Fatalf("expected bk-b answer view open, got view=%v ans=%+v", m.view, m.ans)
	}

	// Completing bk-a's async work must NOT close bk-b's open answer view.
	drive(t, m, inFlight)
	if m.view != viewAnswer || m.ans == nil || m.ans.beadID != "bk-b" {
		t.Fatalf("bk-a completion closed or switched bk-b answer view: view=%v ans=%+v", m.view, m.ans)
	}
	if fmt.Sprint(m.pendingAnswerFor("bk-a")) != "<nil>" {
		t.Fatalf("bk-a pending answer should be cleared after reload: %+v", m.pendingAnswers)
	}
}
