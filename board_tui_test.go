package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func typeKeys(t *testing.T, m *board, s string) {
	t.Helper()
	for _, r := range s {
		press(t, m, string(r))
	}
}

func queueFixtureFleet() *fakeBoard {
	now := t0
	f := &fakeBoard{
		beads: []Bead{
			{
				ID:        "bk-prompt",
				Title:     "Approve deploy command",
				Status:    StatusInProgress,
				Priority:  1,
				UpdatedAt: now.Add(-10 * time.Minute),
			},
			{
				ID:        "bk-crit",
				Title:     "Prod table cleanup",
				Status:    StatusNeedsMe,
				Priority:  0,
				UpdatedAt: now.Add(-30 * time.Minute),
				Notes:     "CRITICAL: Irreversible prod deletion\nQUESTION: Drop legacy table? Recommendation: 2 — keep backup first.\nOPTIONS:\n1. Drop now\n2. Snapshot first, then drop\nCLASS: 8 — prod deletion",
			},
			{
				ID:        "bk-decide",
				Title:     "Close superseded zero-diff PR",
				Status:    StatusNeedsMe,
				Priority:  2,
				UpdatedAt: now.Add(-45 * time.Minute),
				Notes:     "QUESTION: PR #40 has 0 unique changes left; close it? Recommendation: 1 — already landed.\nOPTIONS:\n1. Close PR and bead\n2. Keep PR open\nCLASS: 3 — close superseded PR",
			},
			{
				ID:        "bk-snoozed",
				Title:     "Low urgency cleanup choice",
				Status:    StatusNeedsMe,
				Priority:  3,
				UpdatedAt: now.Add(-2 * time.Hour),
				Notes:     "QUESTION: Archive old docs? Recommendation: 1 — clean up.\nOPTIONS:\n1. Archive\n2. Keep\nCLASS: 3 — docs cleanup",
			},
			{
				ID:        "bk-run",
				Title:     "Refresh local SSO credentials",
				Status:    StatusNeedsMe,
				Priority:  1,
				UpdatedAt: now.Add(-20 * time.Minute),
				Notes:     "RUN: aws sso login --profile dev\nwhy: session token expired",
			},
			{
				ID:        "bk-free",
				Title:     "Pick service display name",
				Status:    StatusNeedsMe,
				Priority:  2,
				UpdatedAt: now.Add(-15 * time.Minute),
				Notes:     "What display name should appear on the settings page?",
			},
			{
				ID:        "bk-work",
				Title:     "Normal working task",
				Status:    StatusInProgress,
				Priority:  2,
				UpdatedAt: now.Add(-5 * time.Minute),
			},
		},
		agents: []Agent{
			{Name: "bk-prompt", Kind: "pi", Status: "blocked", PaneID: "w1:p1"},
			{Name: "bk-work", Kind: "pi", Status: "working", PaneID: "w1:p2"},
		},
		prompts: map[string]ApprovalPrompt{},
	}
	p := ApprovalPrompt{
		Lines: []string{"Bash command", "kubectl delete pod test-1", "Allow?"},
		Options: []PromptOption{
			{Number: 1, Label: "Yes", Selected: true},
			{Number: 2, Label: "No"},
		},
		Numbered: true,
	}
	p.Hash = promptHash(p)
	f.prompts["bk-prompt"] = p
	return f
}

func wireExtendedFakeEnv(f *fakeBoard, env boardEnv, snoozeState *SnoozeState) boardEnv {
	env.snoozes = func() SnoozeState {
		if snoozeState == nil {
			return SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}}
		}
		return *snoozeState
	}
	env.snooze = func(id, dur string) (string, error) {
		d, err := parseSnoozeDuration(dur)
		if err != nil {
			return "", err
		}
		f.calls = append(f.calls, fmt.Sprintf("snooze %s %s", id, dur))
		if snoozeState != nil {
			if snoozeState.Beads == nil {
				snoozeState.Beads = map[string]SnoozeEntry{}
			}
			snoozeState.Beads[id] = SnoozeEntry{Until: t0.Add(d), CreatedAt: t0}
		}
		return fmt.Sprintf("snoozed %s (%s)", id, dur), nil
	}
	env.clearSnooze = func(id string) (string, error) {
		f.calls = append(f.calls, "clear-snooze "+id)
		if snoozeState != nil {
			delete(snoozeState.Beads, id)
		}
		return "cleared snooze on " + id, nil
	}
	env.completeRun = func(id, command, result string) (string, error) {
		f.calls = append(f.calls, fmt.Sprintf("complete-run %s %q -> %q", id, command, result))
		for i := range f.beads {
			if f.beads[i].ID == id {
				f.beads[i].Status = StatusInProgress
				f.beads[i].Notes += "\nRAN: " + command + " → " + result
			}
		}
		return "recorded RAN on " + id + "; bead is in_progress", nil
	}
	return env
}

func TestBoardDecisionQueueOrderingAndSnoozeToggle(t *testing.T) {
	f := queueFixtureFleet()
	sn := &SnoozeState{
		Version: 1,
		Beads: map[string]SnoozeEntry{
			"bk-snoozed": {Until: t0.Add(4 * time.Hour), CreatedAt: t0.Add(-time.Hour)},
			// Snoozing a prompt or CRITICAL question must never hide it.
			"bk-crit":   {Until: t0.Add(4 * time.Hour), CreatedAt: t0.Add(-time.Hour)},
			"bk-prompt": {Until: t0.Add(4 * time.Hour), CreatedAt: t0.Add(-time.Hour)},
		},
	}
	env := wireExtendedFakeEnv(f, f.env(), sn)
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: f.beads, Agents: f.agents, Snooze: *sn}, func() time.Time { return t0 })

	press(t, m, "Q")
	if m.view != viewQueue {
		t.Fatalf("view = %v, want viewQueue", m.view)
	}
	if m.queueSnoozed != 1 {
		t.Fatalf("queueSnoozed = %d, want 1", m.queueSnoozed)
	}
	var gotIDs []string
	for _, it := range m.queue {
		gotIDs = append(gotIDs, it.row.bead.ID)
	}
	wantIDs := []string{"bk-prompt", "bk-crit", "bk-decide", "bk-run", "bk-free"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("queue order = %v, want %v", gotIDs, wantIDs)
	}

	// Pressing H reveals snoozed items at the end of the queue.
	press(t, m, "H")
	if len(m.queue) != 6 || m.queue[5].row.bead.ID != "bk-snoozed" || !m.queue[5].snoozed {
		t.Fatalf("expected bk-snoozed shown after H, got %+v", m.queue)
	}
}

func TestBoardQueueAutoAdvanceAndStepKeys(t *testing.T) {
	f := &fakeBoard{
		beads: []Bead{
			{
				ID:        "bk-q1",
				Title:     "First routine question",
				Status:    StatusNeedsMe,
				Priority:  1,
				UpdatedAt: t0.Add(-30 * time.Minute),
				Notes:     "QUESTION: Close PR #1? Recommendation: 1 — merged.\nOPTIONS:\n1. Close\n2. Keep\nCLASS: 3 — close",
			},
			{
				ID:        "bk-q2",
				Title:     "Second routine question",
				Status:    StatusNeedsMe,
				Priority:  2,
				UpdatedAt: t0.Add(-10 * time.Minute),
				Notes:     "QUESTION: Retry workflow? Recommendation: 2 — wait for main.\nOPTIONS:\n1. Retry now\n2. Wait for main\nCLASS: 4 — workflow",
			},
		},
	}
	env := wireExtendedFakeEnv(f, f.env(), &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}})
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: f.beads}, func() time.Time { return t0 })

	press(t, m, "Q", "\r")
	if m.view != viewAnswer || m.ans == nil || !m.ans.fromQueue || m.ans.beadID != "bk-q1" {
		t.Fatalf("expected queue answer on bk-q1, got view=%v ans=%+v", m.view, m.ans)
	}
	if title := m.titleLine(); !strings.Contains(title, "[1/2]") {
		t.Errorf("answer title missing queue position [1/2]: %q", title)
	}

	// ] steps forward to bk-q2; [ steps back to bk-q1.
	press(t, m, "]")
	if m.ans == nil || m.ans.beadID != "bk-q2" {
		t.Fatalf("] should advance to bk-q2, got %+v", m.ans)
	}
	press(t, m, "[")
	if m.ans == nil || m.ans.beadID != "bk-q1" {
		t.Fatalf("[ should step back to bk-q1, got %+v", m.ans)
	}

	// f follows ★ recommendation 1 on bk-q1 -> auto-advances directly into bk-q2!
	press(t, m, "f", "y")
	if m.view != viewAnswer || m.ans == nil || m.ans.beadID != "bk-q2" {
		t.Fatalf("answering bk-q1 in queue should auto-advance to bk-q2, got view=%v ans=%+v status=%q", m.view, m.ans, m.status)
	}
	// Enter follows ★ recommendation 2 on bk-q2 -> queue hits 0 and returns cleanly to viewQueue!
	press(t, m, "\r", "y")
	if m.view != viewQueue || m.ans != nil || len(m.queue) != 0 {
		t.Fatalf("finishing queue should return to empty viewQueue, got view=%v ans=%+v queue=%d", m.view, m.ans, len(m.queue))
	}
	if !strings.Contains(m.status, "all caught up") {
		t.Errorf("expected caught-up status, got %q", m.status)
	}
	if len(f.calls) != 2 || !strings.Contains(f.calls[0], "bk-q1 choice=1") || !strings.Contains(f.calls[1], "bk-q2 choice=2") {
		t.Fatalf("unexpected answer calls: %v", f.calls)
	}
}

func TestBoardFollowRecommendationAndOptionPlusNote(t *testing.T) {
	f := queueFixtureFleet()
	env := wireExtendedFakeEnv(f, f.env(), &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}})
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: f.beads, Agents: f.agents}, func() time.Time { return t0 })

	selectBead(t, m, "bk-decide")
	// Press f directly on main board, then e (+note), type extra note, submit!
	press(t, m, "f")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "★ 1.") || !strings.Contains(m.ask.prompt, "e +note") {
		t.Fatalf("expected ★ rec confirmation with e +note, got %+v", m.ask)
	}
	press(t, m, "e")
	if m.ask == nil || !m.ask.text {
		t.Fatalf("expected text input for extra note, got %+v", m.ask)
	}
	typeKeys(t, m, "also double-check Linear\r")
	if len(f.calls) != 1 || !strings.Contains(f.calls[0], `bk-decide choice=1 text="also double-check Linear"`) {
		t.Fatalf("expected option+note call, got %v", f.calls)
	}
}

func TestBoardRunCardCopyAndComplete(t *testing.T) {
	f := queueFixtureFleet()
	env := wireExtendedFakeEnv(f, f.env(), &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}})
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: f.beads, Agents: f.agents}, func() time.Time { return t0 })

	selectBead(t, m, "bk-run")
	press(t, m, "A")
	if m.view != viewAnswer || m.ans == nil || m.ans.kind != answerKindRun {
		t.Fatalf("expected answerKindRun for RUN bead, got view=%v ans=%+v", m.view, m.ans)
	}
	screen := strings.Join(screenLines(renderBoard(m, 80, 24)), "\n")
	if !strings.Contains(screen, "aws sso login --profile dev") || !strings.Contains(screen, "session token expired") {
		t.Fatalf("RUN screen missing command or reason:\n%s", screen)
	}

	// Copy command with c.
	press(t, m, "c")
	if !slices.Contains(f.calls, "copy aws sso login --profile dev") {
		t.Fatalf("expected copy call, got %v", f.calls)
	}

	// Complete RUN with d, empty Enter (defaults to "ok"), y to confirm.
	press(t, m, "d", "\r", "y")
	if m.view != viewList {
		t.Fatalf("expected return to viewList after RUN completion, got view=%v status=%q", m.view, m.status)
	}
	if !slices.Contains(f.calls, `complete-run bk-run "aws sso login --profile dev" -> "ok"`) {
		t.Fatalf("expected complete-run call, got %v", f.calls)
	}

	// Directly test completeBoardRun helper end-to-end.
	rb := Bead{ID: "bk-run-2", Status: StatusNeedsMe, Notes: "RUN: tofu apply\nwhy: prod"}
	var notes, told []string
	pEnv := promptEnv{
		show: func(id string) (Bead, error) {
			if id != rb.ID {
				return Bead{}, errors.New("not found")
			}
			return rb, nil
		},
		agent:     func(host, name string) (*Agent, error) { return &Agent{Name: name, Status: "idle"}, nil },
		note:      func(id, text string) error { notes = append(notes, text); return nil },
		setStatus: func(id, s string) error { rb.Status = s; return nil },
		tell:      func(host string, a *Agent, text string) error { told = append(told, text); return nil },
	}
	msg, err := completeBoardRun(pEnv, rb.ID, "tofu apply", "applied 3 updates")
	if err != nil {
		t.Fatalf("completeBoardRun: %v", err)
	}
	if rb.Status != StatusInProgress || len(notes) != 1 || notes[0] != "RAN: tofu apply → applied 3 updates" || len(told) != 1 || !strings.Contains(msg, "prompted worker") {
		t.Fatalf("unexpected completeBoardRun result: msg=%q status=%s notes=%q told=%q", msg, rb.Status, notes, told)
	}
}

func TestBoardSnoozePickerAndClear(t *testing.T) {
	f := queueFixtureFleet()
	sn := &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}}
	env := wireExtendedFakeEnv(f, f.env(), sn)
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: f.beads, Agents: f.agents}, func() time.Time { return t0 })

	selectBead(t, m, "bk-decide")
	press(t, m, "z", "1")
	if !slices.Contains(f.calls, "snooze bk-decide 2h") {
		t.Fatalf("expected 2h snooze call, got %v", f.calls)
	}
	press(t, m, "z", "0")
	if !slices.Contains(f.calls, "clear-snooze bk-decide") {
		t.Fatalf("expected clear-snooze call, got %v", f.calls)
	}
	press(t, m, "z", "t")
	typeKeys(t, m, "12h\r")
	if !slices.Contains(f.calls, "snooze bk-decide 12h") {
		t.Fatalf("expected custom 12h snooze call, got %v", f.calls)
	}
}

func TestBoardResponsiveLayoutsNoOverflow(t *testing.T) {
	f := queueFixtureFleet()
	env := wireExtendedFakeEnv(f, f.env(), &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}})
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: f.beads, Agents: f.agents}, func() time.Time { return t0 })

	sizes := []struct{ w, h int }{
		{42, 18},  // narrow mobile portrait (Moshi)
		{60, 22},  // mobile landscape (Moshi)
		{80, 24},  // classic standard terminal
		{100, 28}, // medium terminal
		{140, 32}, // wide desktop split pane (Ghostty/Rex)
		{180, 40}, // ultra-wide desktop
	}

	for _, sz := range sizes {
		for _, v := range []boardView{viewList, viewQueue, viewDetail, viewEvents, viewHelp} {
			m.view = v
			if v == viewDetail {
				selectBead(t, m, "bk-decide")
			}
			lines := screenLines(renderBoard(m, sz.w, sz.h))
			if len(lines) != sz.h {
				t.Errorf("view=%v %dx%d: got %d lines, want %d", v, sz.w, sz.h, len(lines), sz.h)
			}
			for row, l := range lines {
				if n := utf8.RuneCountInString(l); n > sz.w {
					t.Errorf("view=%v %dx%d row %d overflow (%d > %d): %q", v, sz.w, sz.h, row, n, sz.w, l)
				}
			}
		}
	}

	// Mobile (<80) list shows 2-line card with readable bead title even at 42 cols!
	m.view = viewList
	m.layout = layoutAuto
	selectBead(t, m, "bk-decide")
	mobileScreen := strings.Join(screenLines(renderBoard(m, 42, 20)), "\n")
	if !strings.Contains(mobileScreen, "bk-decide") || !strings.Contains(mobileScreen, "Close superseded") {
		t.Errorf("mobile 42-col compact card should show bead title:\n%s", mobileScreen)
	}

	// Wide desktop (>=130) list renders live split pane with │ divider and decision preview!
	wideScreen := strings.Join(screenLines(renderBoard(m, 150, 30)), "\n")
	if !strings.Contains(wideScreen, "│") || !strings.Contains(wideScreen, "Waiting: needs_me decision") {
		t.Errorf("wide 150-col split pane missing divider or live decision preview:\n%s", wideScreen)
	}

	// L cycles auto -> compact -> standard -> wide -> auto.
	for _, want := range []layoutMode{layoutCompact, layoutStandard, layoutWide, layoutAuto} {
		press(t, m, "L")
		if m.layout != want {
			t.Fatalf("after L: layout = %v, want %v", m.layout, want)
		}
	}
}

func TestSGRMouseParsingAndInteractions(t *testing.T) {
	// splitKeys keeps SGR mouse sequences whole.
	raw := []byte("\x1b[<0;12;5M\x1b[<0;12;5m\x1b[<65;10;8M")
	keys, rest := splitKeys(raw)
	if rest != nil || len(keys) != 3 {
		t.Fatalf("splitKeys SGR mouse = %q rest=%q", keys, rest)
	}
	ev, ok := parseSGRMouse(keys[0])
	if !ok || ev.btn != 0 || ev.col != 11 || ev.row != 4 || ev.release {
		t.Fatalf("parseSGRMouse press = %+v ok=%v", ev, ok)
	}
	rel, ok := parseSGRMouse(keys[1])
	if !ok || !rel.release {
		t.Fatalf("parseSGRMouse release = %+v ok=%v", rel, ok)
	}

	f := queueFixtureFleet()
	env := wireExtendedFakeEnv(f, f.env(), &SnoozeState{Version: 1, Beads: map[string]SnoozeEntry{}})
	m := newBoard(Config{Repo: "/r"}, env, boardData{Beads: f.beads, Agents: f.agents}, func() time.Time { return t0 })

	// Render queue, click on bk-decide row, click again to open viewAnswer,
	// click option 1 row, click y on confirmation bar!
	press(t, m, "Q")
	_ = renderBoard(m, 70, 24)
	var decideRow int
	found := false
	for scrRow, qIdx := range m.hits.rowTargets {
		if qIdx < len(m.queue) && m.queue[qIdx].row.bead.ID == "bk-decide" {
			decideRow = scrRow
			found = true
			break
		}
	}
	if !found {
		t.Fatal("bk-decide not found in queue hit targets")
	}
	// First tap selects bk-decide.
	press(t, m, fmt.Sprintf("\x1b[<0;5;%dM", decideRow+1))
	if it := m.queueSel(); it == nil || it.row.bead.ID != "bk-decide" {
		t.Fatalf("first tap did not select bk-decide: %+v", it)
	}
	// Second tap opens viewAnswer!
	press(t, m, fmt.Sprintf("\x1b[<0;5;%dM", decideRow+1))
	if m.view != viewAnswer || m.ans == nil || m.ans.beadID != "bk-decide" {
		t.Fatalf("second tap did not open viewAnswer: view=%v ans=%+v", m.view, m.ans)
	}
	// Render viewAnswer, tap option 1 row!
	_ = renderBoard(m, 70, 24)
	var opt1Row int
	found = false
	for scrRow, optNum := range m.hits.optionRows {
		if optNum == 1 {
			opt1Row = scrRow
			found = true
			break
		}
	}
	if !found {
		t.Fatal("option 1 row not recorded in viewAnswer hit targets")
	}
	press(t, m, fmt.Sprintf("\x1b[<0;5;%dM", opt1Row+1))
	if m.ask == nil {
		t.Fatal("tapping option 1 did not open confirmation")
	}
	// Render confirmation bar and tap 'y'!
	_ = renderBoard(m, 70, 24)
	ySpan, ok := m.hits.confirmKeys["y"]
	if !ok || m.hits.confirmRow < 0 {
		t.Fatalf("confirm y hit span not recorded: row=%d keys=%+v", m.hits.confirmRow, m.hits.confirmKeys)
	}
	press(t, m, fmt.Sprintf("\x1b[<0;%d;%dM", ySpan[0]+1, m.hits.confirmRow+1))
	if len(f.calls) != 1 || !strings.Contains(f.calls[0], "bk-decide choice=1") {
		t.Fatalf("tapping y did not submit answer: calls=%v", f.calls)
	}
}

func TestOSC52ClipboardAndSSHOpenURL(t *testing.T) {
	seq := osc52Sequence("hello world")
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello world")) + "\x07"
	if seq != want {
		t.Fatalf("osc52Sequence = %q, want %q", seq, want)
	}

	t.Setenv("SSH_TTY", "/dev/pts/1")
	var buf bytes.Buffer
	msg := openBoardURL(&buf, "https://github.com/example/repo/pull/42")
	if msg != "copied URL: https://github.com/example/repo/pull/42" {
		t.Fatalf("openBoardURL over SSH = %q", msg)
	}
	if buf.String() != osc52Sequence("https://github.com/example/repo/pull/42") {
		t.Fatalf("OSC 52 output mismatch: %q", buf.String())
	}
}
