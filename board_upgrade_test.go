package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBoardAnswerApprovalPrompt(t *testing.T) {
	f := testFleet()
	f.agents[0].Status = "blocked"
	p := ApprovalPrompt{
		Lines: []string{"Bash command", "git push origin feat/test", "Allow running this command?"},
		Options: []PromptOption{
			{Number: 1, Label: "Yes", Selected: true},
			{Number: 2, Label: "Yes, don't ask again for this session"},
			{Number: 3, Label: "No"},
		},
		Numbered: true,
	}
	p.Hash = promptHash(p)
	f.prompts = map[string]ApprovalPrompt{"bk-ask": p}

	m := testModel(t, f)
	selectBead(t, m, "bk-ask")
	press(t, m, "A")
	if m.view != viewAnswer || m.ans == nil || m.ans.prompt == nil {
		t.Fatalf("expected viewAnswer with prompt, got view=%v ans=%+v status=%q", m.view, m.ans, m.status)
	}

	screen := strings.Join(screenLines(renderBoard(m, 80, 24)), "\n")
	for _, want := range []string{
		"quartermaster answer · bk-ask · approval prompt · [" + p.Hash + "]",
		"git push origin feat/test",
		"hash " + p.Hash,
		"1. Yes",
		"[class 9]",
		"3. No",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("answer screen missing %q:\n%s", want, screen)
		}
	}
	for _, l := range screenLines(renderBoard(m, 80, 24)) {
		if n := utf8.RuneCountInString(l); n > 80 {
			t.Errorf("viewAnswer overflow (%d runes): %q", n, l)
		}
	}

	// Custom free-text is never offered on approval prompts.
	press(t, m, "t")
	if m.ask != nil {
		t.Fatalf("approval prompt accepted free-text key t: %+v", m.ask)
	}

	// Pick option 2, decline confirmation.
	press(t, m, "2")
	if m.ask == nil || !strings.Contains(m.ask.prompt, p.Hash) || !strings.Contains(m.ask.prompt, "[class 9]") {
		t.Fatalf("expected hash + class 9 confirmation, got %+v", m.ask)
	}
	press(t, m, "n")
	if len(f.calls) != 0 {
		t.Fatalf("declined prompt answer ran: %v", f.calls)
	}

	// Hash mismatch refuses without answering.
	changed := p
	changed.Lines = []string{"Bash command", "rm -rf /tmp/other"}
	changed.Hash = promptHash(changed)
	f.prompts["bk-ask"] = changed
	press(t, m, "1", "y")
	if len(f.calls) != 0 || !strings.Contains(m.status, "prompt changed") || m.view != viewAnswer {
		t.Fatalf("expected hash-mismatch refusal staying in viewAnswer: calls=%v status=%q view=%v", f.calls, m.status, m.view)
	}

	// Reload picks up the new hash; confirming then succeeds and returns to list.
	press(t, m, "r")
	if m.ans == nil || m.ans.prompt == nil || m.ans.prompt.Hash != changed.Hash {
		t.Fatalf("reload did not pick up new prompt hash: %+v", m.ans)
	}
	press(t, m, "3", "y")
	wantCall := fmt.Sprintf("answer-prompt bk-ask 3 --expect %s --by TJ", changed.Hash)
	if !slices.Equal(f.calls, []string{wantCall}) {
		t.Fatalf("calls = %v, want [%s]", f.calls, wantCall)
	}
	if m.view != viewList || m.ans != nil {
		t.Fatalf("expected return to viewList after answer, got view=%v ans=%+v", m.view, m.ans)
	}
}

func TestBoardAnswerApprovalPromptEndToEnd(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	screenPrompt := strings.Join([]string{
		"──────────────────────────────",
		"Bash command",
		"  go test ./...",
		"Do you want to proceed?",
		"❯ 1. Yes",
		"  2. No",
		"Esc to cancel",
	}, "\n")
	parsed, ok := parsePrompt(screenPrompt)
	if !ok {
		t.Fatal("fixture screen did not parse as approval prompt")
	}

	var sentKeys []string
	var notes []string
	curScreen := screenPrompt
	pEnv := promptEnv{
		show: func(id string) (Bead, error) { return Bead{ID: id, Status: StatusInProgress}, nil },
		agent: func(host, name string) (*Agent, error) {
			return &Agent{Name: name, PaneID: "w:p1", Status: "blocked"}, nil
		},
		read: func(host, pane string) (string, error) { return curScreen, nil },
		keys: func(host, pane string, keys ...string) error {
			sentKeys = append(sentKeys, keys...)
			curScreen = "running tests...\n"
			return nil
		},
		note:  func(id, text string) error { notes = append(notes, text); return nil },
		sleep: func(time.Duration) {},
	}

	f := testFleet()
	f.agents[0].Status = "blocked"
	env := f.env()
	env.readPrompt = func(id string) (ApprovalPrompt, error) {
		p, _, _, err := workerPrompt(pEnv, id)
		return p, err
	}
	env.answerPrompt = func(id, choice, expect string) (string, error) {
		return answerPromptVia(pEnv, id, choice, expect, "TJ", -1, "board")
	}

	m := newBoard(Config{}, env, boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	selectBead(t, m, "bk-ask")

	// Open answer view, then mutate the pane before confirming -> must refuse.
	press(t, m, "A")
	curScreen = strings.ReplaceAll(screenPrompt, "go test ./...", "make deploy")
	press(t, m, "1", "y")
	if len(sentKeys) != 0 || !strings.Contains(m.status, "prompt changed") {
		t.Fatalf("expected live hash-mismatch refusal: keys=%v status=%q", sentKeys, m.status)
	}

	// Restore prompt, reload, and confirm option 1.
	curScreen = screenPrompt
	press(t, m, "r", "1", "y")
	if !slices.Equal(sentKeys, []string{"1"}) {
		t.Fatalf("sentKeys = %v, want [1]", sentKeys)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "PROMPT ANSWERED: 1 Yes") || !strings.Contains(notes[0], "["+parsed.Hash+"] by TJ via board") {
		t.Fatalf("unexpected audit note: %v", notes)
	}
	if m.view != viewList {
		t.Fatalf("expected viewList after answering, got %v", m.view)
	}
}

func TestBoardAnswerStructuredNeedsMeEndToEnd(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	questionNote := strings.Join([]string{
		"QUESTION: Roll out to one region first or all regions? Recommendation: 2 — safer first step.",
		"OPTIONS:",
		"1. All regions at once",
		"2. One region first",
		"CLASS: 7 — production rollout",
	}, "\n")
	parsed, ok := parseNeedsMeQuestion(questionNote)
	if !ok {
		t.Fatal("fixture question did not parse")
	}

	beadState := boardBead("bk-ask", StatusNeedsMe, 1)
	beadState.Notes = questionNote
	var noted []string
	var told string
	pEnv := promptEnv{
		show: func(id string) (Bead, error) { return beadState, nil },
		agent: func(host, name string) (*Agent, error) {
			return &Agent{Name: name, PaneID: "w:p1", Status: "idle"}, nil
		},
		note: func(id, text string) error {
			noted = append(noted, text)
			beadState.Notes += "\n" + text
			return nil
		},
		setStatus: func(id, status string) error {
			beadState.Status = status
			return nil
		},
		tell: func(host string, a *Agent, text string) error {
			told = text
			return nil
		},
	}

	f := testFleet()
	f.beads[0] = beadState
	env := f.env()
	env.show = pEnv.show
	env.answerNeedsMe = func(id, choice, text, expect string) (string, error) {
		return answerNeedsMeTextVia(pEnv, id, choice, text, expect, "board")
	}

	m := newBoard(Config{}, env, boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	selectBead(t, m, "bk-ask")
	press(t, m, "A")
	if m.view != viewAnswer || m.ans == nil || m.ans.question == nil || m.ans.question.Hash != parsed.Hash {
		t.Fatalf("expected structured needs_me answer view, got view=%v ans=%+v", m.view, m.ans)
	}

	screen := strings.Join(screenLines(renderBoard(m, 90, 24)), "\n")
	for _, want := range []string{
		"Roll out to one region first or all regions?",
		"CLASS: 7 — production rollout",
		"1. All regions at once",
		"2. One region first",
		"recommended",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("needs_me screen missing %q:\n%s", want, screen)
		}
	}

	// Pick option 2, decline first, then confirm.
	press(t, m, "2", "n")
	if len(noted) != 0 || beadState.Status != StatusNeedsMe {
		t.Fatalf("declined answer mutated bead: notes=%v status=%s", noted, beadState.Status)
	}
	press(t, m, "2", "y")
	if beadState.Status != StatusInProgress {
		t.Fatalf("bead status = %s, want in_progress", beadState.Status)
	}
	if len(noted) != 1 || !strings.Contains(noted[0], "NEEDS_ME ANSWERED: 2 One region first") || !strings.Contains(noted[0], "by TJ via board") {
		t.Fatalf("unexpected note: %v", noted)
	}
	if !strings.Contains(told, "2. One region first") || !strings.Contains(told, "via board") {
		t.Fatalf("worker prompt missing answer: %q", told)
	}
	if m.view != viewList {
		t.Fatalf("expected viewList after answering, got %v", m.view)
	}

	// Custom free-text answer on an agentless structured needs_me bead.
	beadState = boardBead("bk-ask", StatusNeedsMe, 1)
	beadState.Notes = questionNote
	pEnv.agent = func(host, name string) (*Agent, error) { return nil, nil }
	noted, told = nil, ""
	f.beads[0] = beadState
	f.agents = nil
	m = newBoard(Config{}, env, boardData{Beads: slices.Clone(f.beads), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	selectBead(t, m, "bk-ask")
	press(t, m, "A")
	// Mutate the bead's question while the answer view is open -> hash mismatch must refuse.
	beadState.Notes = strings.ReplaceAll(questionNote, "One region first", "Canary only")
	press(t, m, "1", "y")
	if len(noted) != 0 || !strings.Contains(m.status, "question changed") || m.view != viewAnswer {
		t.Fatalf("expected needs_me hash-mismatch refusal: notes=%v status=%q view=%v", noted, m.status, m.view)
	}
	// Reload picks up updated question; custom text then succeeds.
	press(t, m, "r", "t")
	typeText(t, m, "wait for canary metrics first\r")
	press(t, m, "y")
	if beadState.Status != StatusInProgress {
		t.Fatalf("custom text status = %s, want in_progress", beadState.Status)
	}
	if len(noted) < 1 || !strings.Contains(noted[0], "NEEDS_ME ANSWERED: free text") || !strings.Contains(noted[0], "by TJ via board: wait for canary metrics first") {
		t.Fatalf("custom text note = %v", noted)
	}

	// Structured needs_me bead whose worker is currently working refuses A upfront.
	beadState = boardBead("bk-ask", StatusNeedsMe, 1)
	beadState.Notes = questionNote
	f.beads[0] = beadState
	f.agents = []Agent{{Name: "bk-ask", Kind: "claude", Status: "working", PaneID: "w:p1"}}
	m = newBoard(Config{}, env, boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	selectBead(t, m, "bk-ask")
	press(t, m, "A")
	if m.view != viewList || !strings.Contains(m.status, "working; answer once it stops") {
		t.Fatalf("expected working agent refusal upfront: view=%v status=%q", m.view, m.status)
	}
}

func TestBoardDispatchWithRolesAndResumeOverride(t *testing.T) {
	noWT := false
	cfg := defaultConfig()
	cfg.Roles = map[string]RoleConfig{
		"security-audit": {Worktree: &noWT, Agent: "pi", Brief: roleInvestigator},
	}

	f := testFleet()
	f.beads = append(f.beads, boardBead("bk-rev", StatusOpen, 1, "role:reviewer"))
	m := newBoard(cfg, f.env(), boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0

	// Bead with role:reviewer label defaults to reviewer (no worktree).
	selectBead(t, m, "bk-rev")
	press(t, m, "\r")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "(role reviewer)") || !strings.Contains(m.ask.prompt, "no worktree") {
		t.Fatalf("expected role:reviewer default prompt, got %+v", m.ask)
	}
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"dispatch bk-rev  role=reviewer focus=true"}) || !m.quit {
		t.Fatalf("calls = %v quit=%v", f.calls, m.quit)
	}

	// Agent key + r role picker selects a built-in role.
	f.calls = nil
	m = newBoard(cfg, f.env(), boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	selectBead(t, m, "bk-nx1")
	press(t, m, "i", "r")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "3 investigator") || !strings.Contains(m.ask.prompt, "5 security-audit") {
		t.Fatalf("role picker prompt = %+v", m.ask)
	}
	press(t, m, "3", "y")
	if !slices.Equal(f.calls, []string{"dispatch bk-nx1 pi role=investigator focus=true"}) {
		t.Fatalf("calls = %v", f.calls)
	}

	// D opens role picker directly and supports custom [roles.<name>].
	f.calls = nil
	m = newBoard(cfg, f.env(), boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	selectBead(t, m, "bk-nx2")
	press(t, m, "D", "5")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "start pi on bk-nx2 (role security-audit)") {
		t.Fatalf("custom role confirm prompt = %+v", m.ask)
	}
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"dispatch bk-nx2  role=security-audit focus=true"}) {
		t.Fatalf("calls = %v", f.calls)
	}

	// R resume supports overriding the agent kind (e.g. legacy session onto pi).
	f.calls = nil
	m = newBoard(cfg, f.env(), boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	selectBead(t, m, "bk-gone")
	press(t, m, "R", "a", "i")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "resume bk-gone with pi?") {
		t.Fatalf("resume override prompt = %+v", m.ask)
	}
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"resume bk-gone pi"}) {
		t.Fatalf("resume override calls = %v", f.calls)
	}
}

func TestBoardMainMultiSelectBulkActions(t *testing.T) {
	f := testFleet()
	f.beads = append(f.beads, boardBead("bk-gone2", StatusInProgress, 3))
	m := testModel(t, f)

	// Existing space/v detail pane and m (Muse) bindings stay intact.
	selectBead(t, m, "bk-nx1")
	press(t, m, " ")
	if m.view != viewDetail || len(m.marked) != 0 {
		t.Fatalf("space broke detail pane: view=%v marked=%v", m.view, m.marked)
	}
	press(t, m, "\x1b", "v")
	if m.view != viewDetail {
		t.Fatalf("v broke detail pane: view=%v", m.view)
	}
	press(t, m, "\x1b", "m")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "start muse on bk-nx1") {
		t.Fatalf("m broke Muse dispatch: %+v", m.ask)
	}
	press(t, m, "n")

	// ; and M toggle row marks and render within 80 columns.
	selectBead(t, m, "bk-nx1")
	press(t, m, ";", "M")
	if !m.marked["bk-nx1"] || !m.marked["bk-nx2"] || len(m.marked) != 2 {
		t.Fatalf("marks = %v, want bk-nx1 and bk-nx2", m.marked)
	}
	screen := screenLines(renderBoard(m, 80, 24))
	if !strings.Contains(screen[0], "2 marked") {
		t.Fatalf("title lacks mark count: %q", screen[0])
	}
	for _, l := range screen {
		if n := utf8.RuneCountInString(l); n > 80 {
			t.Errorf("marked list overflow (%d runes): %q", n, l)
		}
	}

	// Bulk n toggles next off when all marked already have next.
	press(t, m, "n")
	wantNext := []string{"label bk-nx1 next false", "label bk-nx2 next false"}
	if !slices.Equal(f.calls, wantNext) || len(m.marked) != 0 {
		t.Fatalf("bulk next calls=%v marks=%v", f.calls, m.marked)
	}

	// * toggles section marks on, then off.
	selectBead(t, m, "bk-gone")
	press(t, m, "*")
	if !m.marked["bk-gone"] || !m.marked["bk-gone2"] || len(m.marked) != 2 {
		t.Fatalf("section mark = %v", m.marked)
	}
	press(t, m, "*")
	if len(m.marked) != 0 {
		t.Fatalf("section unmark left %v", m.marked)
	}

	// Bulk R skips beads with running agents and supports agent override.
	f.calls = nil
	selectBead(t, m, "bk-work")
	press(t, m, ";")
	selectBead(t, m, "bk-gone")
	press(t, m, ";", ";") // marks bk-gone and bk-gone2
	press(t, m, "R", "i", "y")
	wantResume := []string{"resume bk-gone pi", "resume bk-gone2 pi"}
	if !slices.Equal(f.calls, wantResume) {
		t.Fatalf("bulk resume calls = %v, want %v", f.calls, wantResume)
	}
	// Unacted running bead stayed marked; clear with Esc without quitting.
	if !m.marked["bk-work"] {
		t.Fatalf("expected unacted bk-work to remain marked: %v", m.marked)
	}
	press(t, m, "\x1b")
	if len(m.marked) != 0 || m.quit {
		t.Fatalf("esc should clear marks without quitting: marked=%v quit=%v", m.marked, m.quit)
	}

	// Bulk C closes all marked beads with one reason + confirm.
	f.calls = nil
	selectBead(t, m, "bk-gone")
	press(t, m, "*")
	press(t, m, "C")
	typeText(t, m, "obsolete\r")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "close bk-gone, bk-gone2") {
		t.Fatalf("bulk close confirm = %+v", m.ask)
	}
	press(t, m, "y")
	wantClose := []string{"close bk-gone obsolete", "close bk-gone2 obsolete"}
	if !slices.Equal(f.calls, wantClose) {
		t.Fatalf("bulk close calls = %v, want %v", f.calls, wantClose)
	}

	// Bulk d resolves marked beads (never --force) with confirmation.
	f.calls = nil
	selectBead(t, m, "bk-gone")
	press(t, m, "*", "d")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "never --force") {
		t.Fatalf("bulk resolve confirm = %+v", m.ask)
	}
	press(t, m, "y")
	wantResolve := []string{"resolve bk-gone", "resolve bk-gone2"}
	if !slices.Equal(f.calls, wantResolve) {
		t.Fatalf("bulk resolve calls = %v, want %v", f.calls, wantResolve)
	}

	// Bulk dispatch (D) starts marked agentless beads with a chosen role without quitting.
	f.calls = nil
	selectBead(t, m, "bk-gone")
	press(t, m, "*", "D", "2", "y")
	wantBulkDispatch := []string{"dispatch bk-gone  role=reviewer focus=false", "dispatch bk-gone2  role=reviewer focus=false"}
	if !slices.Equal(f.calls, wantBulkDispatch) || m.quit {
		t.Fatalf("bulk dispatch calls=%v quit=%v, want %v", f.calls, m.quit, wantBulkDispatch)
	}

	// Filter safety: marked rows hidden by filter are never silently acted on.
	f.calls = nil
	selectBead(t, m, "bk-old")
	press(t, m, ";")
	press(t, m, "/")
	typeText(t, m, "bk-ask\r")
	press(t, m, "C")
	if m.ask != nil || !strings.Contains(m.status, "no marked beads match current filter") {
		t.Fatalf("expected filter-hidden mark refusal, got ask=%+v status=%q", m.ask, m.status)
	}
}

func TestBoardEventsViewAndLiveLoader(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	class7 := 7
	// Verify loadBoardEvents reads real notify/events.json and journal.jsonl.
	if err := appendNotifyFeedEvents([]NotifyEnvelope{
		{
			Version: notifyEnvelopeVersion,
			Event:   "needs_me",
			At:      t0.Add(-2 * time.Minute).UTC().Format(time.RFC3339),
			Title:   "Decision needed",
			Body:    "bk-ask needs decision",
			Items:   []NotifyItem{{Bead: "bk-ask", Reason: "needs_me", Class: &class7}},
		},
		{
			Version: notifyEnvelopeVersion,
			Event:   "pr_merged",
			At:      t0.Add(-1 * time.Minute).UTC().Format(time.RFC3339),
			Title:   "PR merged",
			Body:    "PR #42 merged",
			Items:   []NotifyItem{{Bead: "bk-old", Reason: "pr_merged"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendJournal(journalRecord{
		At:    t0.Add(-30 * time.Second),
		Kind:  journalAuto,
		Bead:  "bk-work",
		Class: 4,
		Text:  "AUTO: [worker L5 C4] merged PR #41 — checks green",
	}); err != nil {
		t.Fatal(err)
	}
	loadedFeed, loadedAuto, err := loadBoardEvents(t0)
	if err != nil {
		t.Fatalf("loadBoardEvents: %v", err)
	}
	if len(loadedFeed) != 2 || len(loadedAuto) != 1 {
		t.Fatalf("loaded feed=%d auto=%d", len(loadedFeed), len(loadedAuto))
	}

	f := testFleet()
	f.inbox = []InboxItem{
		{
			Event: Event{
				Bead:    "bk-ask",
				Kind:    EventNeedsYou,
				Summary: "worker at approval prompt",
				At:      t0.Add(-10 * time.Second),
				Clears:  &InboxCondition{Kind: "prompt", Bead: "bk-ask", Key: "abcd1234"},
			},
			File: "100-bk-ask-needs_you.json",
		},
		{
			Event: Event{
				Bead:    "bk-old",
				Kind:    EventReadyForReview,
				Summary: "PR #55 ready for review",
				At:      t0.Add(-5 * time.Minute),
			},
			File: "050-bk-old-ready_for_review.json",
		},
	}
	f.feed = loadedFeed
	f.journal = loadedAuto

	m := testModel(t, f)
	press(t, m, "E")
	if m.view != viewEvents {
		t.Fatalf("expected viewEvents, got %v", m.view)
	}
	lines := screenLines(renderBoard(m, 90, 24))
	screen := strings.Join(lines, "\n")
	for _, want := range []string{
		"quartermaster events · 2 unread inbox · 3 recent",
		"unread inbox (2)",
		"prompt",
		"ready_for_review",
		"recent events (3)",
		"AUTO",
		"pr_merged",
		"[C7]",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("events screen missing %q:\n%s", want, screen)
		}
	}
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > 90 {
			t.Errorf("events view overflow (%d runes): %q", n, l)
		}
	}

	// i acknowledges the selected unread inbox file (newest first: bk-ask prompt).
	press(t, m, "i")
	if !slices.Equal(f.calls, []string{"dismiss 100-bk-ask-needs_you.json"}) {
		t.Fatalf("ack calls = %v", f.calls)
	}
	if len(m.data.Inbox) != 1 || m.data.Inbox[0].File != "050-bk-old-ready_for_review.json" {
		t.Fatalf("remaining inbox = %+v", m.data.Inbox)
	}

	// Move down into recent events (bk-work AUTO) and verify i refuses non-inbox rows.
	press(t, m, "j")
	if r := m.evSel(); r == nil || r.source != "auto" || r.bead != "bk-work" {
		t.Fatalf("expected AUTO row selected, got %+v", r)
	}
	f.calls = nil
	press(t, m, "i")
	if len(f.calls) != 0 || !strings.Contains(m.status, "only unread inbox") {
		t.Fatalf("non-inbox ack should refuse: calls=%v status=%q", f.calls, m.status)
	}

	// Enter jumps straight to bk-work on the main board (even clearing a kept filter).
	m.filter.query = "bk-old"
	m.rebuild()
	press(t, m, "\r")
	if m.view != viewList || m.sel() == nil || m.sel().bead.ID != "bk-work" || m.filter.query != "" {
		t.Fatalf("jump from events failed: view=%v sel=%+v filter=%q", m.view, m.sel(), m.filter.query)
	}

	// E toggles open and closed cleanly.
	press(t, m, "E", "E")
	if m.view != viewList {
		t.Fatalf("expected second E to return to viewList, got %v", m.view)
	}
}

func TestBoardCreateTopLevelBeadAndAskCoordinator(t *testing.T) {
	cfg := defaultConfig()
	cfg.Repos = map[string]string{
		"app":   "/tmp/app",
		"infra": "/tmp/infra",
	}

	// N works even on an empty board.
	f := &fakeBoard{}
	m := newBoard(cfg, f.env(), boardData{BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0

	press(t, m, "N")
	typeText(t, m, "Audit rollout alerts\r")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "1 app") || !strings.Contains(m.ask.prompt, "2 infra") {
		t.Fatalf("repo picker = %+v", m.ask)
	}
	press(t, m, "2") // infra
	if m.ask == nil || !strings.Contains(m.ask.prompt, "3 investigator") {
		t.Fatalf("role picker = %+v", m.ask)
	}
	press(t, m, "3") // investigator
	if m.ask == nil || !strings.Contains(m.ask.prompt, "[repo:infra, role:investigator]") {
		t.Fatalf("confirm create = %+v", m.ask)
	}
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{`create "Audit rollout alerts" repo=infra role=investigator`}) || m.quit {
		t.Fatalf("create calls = %v quit=%v", f.calls, m.quit)
	}

	// Create + immediate dispatch (d).
	f.calls = nil
	press(t, m, "N")
	typeText(t, m, "Review PR checks\r")
	press(t, m, "\r") // default repo
	press(t, m, "2")  // reviewer
	press(t, m, "d")  // create + dispatch
	want := []string{
		`create "Review PR checks" repo= role=reviewer`,
		"dispatch bk-new2  role=reviewer focus=true",
	}
	if !slices.Equal(f.calls, want) || !m.quit {
		t.Fatalf("create+dispatch calls = %v want=%v quit=%v", f.calls, want, m.quit)
	}

	// @ Ask-coordinator bar: fails cleanly when absent, working, and sends when idle without quitting.
	f.calls = nil
	m = newBoard(cfg, f.env(), boardData{Beads: slices.Clone(f.beads), BeadsAt: t0}, func() time.Time { return t0 })
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	press(t, m, "@")
	typeText(t, m, "summarize open PRs\r")
	if len(f.calls) != 0 || !strings.Contains(m.status, "coordinator is not running") || m.quit {
		t.Fatalf("absent coordinator: calls=%v status=%q quit=%v", f.calls, m.status, m.quit)
	}

	f.agents = []Agent{{Name: cfg.CoordinatorName, Kind: "pi", Status: "working", PaneID: "w:c1"}}
	press(t, m, "@")
	typeText(t, m, "summarize open PRs\r")
	if len(f.calls) != 0 || !strings.Contains(m.status, "is working") || m.quit {
		t.Fatalf("working coordinator: calls=%v status=%q quit=%v", f.calls, m.status, m.quit)
	}

	f.agents[0].Status = "idle"
	press(t, m, "@")
	typeText(t, m, "summarize open PRs\r")
	if !slices.Equal(f.calls, []string{"coord quartermaster summarize open PRs"}) || m.quit || !strings.Contains(m.status, "sent to coordinator") {
		t.Fatalf("idle coordinator: calls=%v status=%q quit=%v", f.calls, m.status, m.quit)
	}
}

func TestInboxRunnerSuppressInteractiveNudges(t *testing.T) {
	// 1. Default config keeps suppress_interactive_nudges = false.
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
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(fmt.Sprintf("repo = %q\n", repoDir)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InboxRunner.SuppressInteractiveNudges {
		t.Fatal("expected suppress_interactive_nudges default false")
	}

	// Opt-in config parses and surfaces in inbox-runner status.
	tomlBody := fmt.Sprintf("repo = %q\n[inbox_runner]\nenabled = true\nsuppress_interactive_nudges = true\n", repoDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(tomlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InboxRunner.Enabled || !cfg.InboxRunner.SuppressInteractiveNudges {
		t.Fatalf("expected enabled+suppress true, got %+v", cfg.InboxRunner)
	}
	rep, err := buildInboxRunnerStatus(cfg, inboxRunnerEnv{
		now:        func() time.Time { return t0 },
		listAgents: func() (map[string]Agent, bool, error) { return nil, false, nil },
		readInbox:  func() ([]InboxItem, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeInboxRunnerStatus(&out, rep, false, t0); err != nil {
		t.Fatal(err)
	}
	if !rep.Config.SuppressInteractiveNudges || !strings.Contains(out.String(), "suppress_nudges=true") {
		t.Fatalf("status output missing suppress_nudges=true:\n%s", out.String())
	}

	// 2. Non-pi coordinator nudgeCoordinator: suppresses allowlisted when enabled, still nudges human-gated.
	toolDir := fakeTools(t)
	now := time.Now().Truncate(time.Second)
	cfg.CoordinatorAgent = "claude"
	cfg.Nudge = true
	cfg.IdleSeconds = 10
	ticker := Ticker{cfg: cfg, herdr: newHerdr(), log: log.New(io.Discard, "", 0)}
	coord := Agent{Name: cfg.CoordinatorName, Kind: "claude", PaneID: "w:c1", Status: "idle", Seq: 1}
	agents := map[string]Agent{coord.Name: coord}

	if err := writeEvent(Event{Bead: "task-1", Kind: EventMerged, Summary: "PR merged", At: now}); err != nil {
		t.Fatal(err)
	}
	st := &TickerState{CoordReady: now.Add(-time.Minute), CoordSeq: 1}
	ticker.nudgeCoordinator(st, agents, now)
	if !st.LastNudge.IsZero() {
		t.Fatal("allowlisted event nudged interactive coordinator despite suppress_interactive_nudges=true")
	}
	if lines := logLines(t, toolDir, "herdr"); strings.TrimSpace(strings.Join(lines, "")) != "" {
		t.Fatalf("unexpected herdr calls for suppressed event: %v", lines)
	}

	// When inbox_runner is disabled, suppress_interactive_nudges does not drop nudges.
	disabledCfg := cfg
	disabledCfg.InboxRunner.Enabled = false
	Ticker{cfg: disabledCfg, herdr: newHerdr(), log: log.New(io.Discard, "", 0)}.nudgeCoordinator(st, agents, now)
	if st.LastNudge.IsZero() {
		t.Fatal("expected nudge when inbox_runner is disabled")
	}

	// Human-gated event (needs_you) still nudges even when enabled + suppressed.
	st2 := &TickerState{CoordReady: now.Add(-time.Minute), CoordSeq: 1, LastNudge: now}
	later := now.Add(time.Minute)
	if err := writeEvent(Event{Bead: "task-2", Kind: EventNeedsYou, Summary: "needs decision", At: later}); err != nil {
		t.Fatal(err)
	}
	ticker.nudgeCoordinator(st2, agents, later)
	if !st2.LastNudge.Equal(later) {
		t.Fatalf("expected needs_you to nudge interactive coordinator: lastNudge=%v", st2.LastNudge)
	}

	// 3. Pi coordinator nudgePiCoordinator: allowlisted-only sends inbox_status, human-gated sends inbox.
	short, err := os.MkdirTemp("", "qm-quiet-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	t.Setenv("QUARTERMASTER_STATE_DIR", short)
	if err := os.MkdirAll(piControlDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(piCoordinatorSocket()+".token", []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", piCoordinatorSocket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	reqs := make(chan piRequest, 4)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req piRequest
			_ = json.Unmarshal(line, &req)
			reqs <- req
			_ = json.NewEncoder(conn).Encode(piResponse{OK: true, Session: req.Session})
			conn.Close()
		}
	}()

	piCfg := cfg
	piCfg.CoordinatorAgent = "pi"
	piTicker := Ticker{cfg: piCfg, herdr: newHerdr(), log: log.New(io.Discard, "", 0)}
	piCoord := Agent{Name: piCfg.CoordinatorName, Kind: "pi", Status: "idle"}
	piCoord.Session.Agent = "pi"
	piCoord.Session.Kind = "path"
	piCoord.Session.Value = filepath.Join(short, "session.jsonl")
	piAgents := map[string]Agent{piCoord.Name: piCoord}

	t1 := now.Add(2 * time.Minute)
	if err := writeEvent(Event{Bead: "task-3", Kind: EventReadyForReview, Summary: "ready", At: t1}); err != nil {
		t.Fatal(err)
	}
	piSt := &TickerState{}
	piTicker.nudgeCoordinator(piSt, piAgents, t1)
	req1 := <-reqs
	if req1.Op != "inbox_status" || req1.Through != "" {
		t.Fatalf("expected inbox_status for suppressed allowlisted event, got %+v", req1)
	}

	t2 := now.Add(3 * time.Minute)
	if err := writeEvent(Event{Bead: "task-4", Kind: EventNeedsYou, Summary: "question", At: t2}); err != nil {
		t.Fatal(err)
	}
	piTicker.nudgeCoordinator(piSt, piAgents, t2)
	req2 := <-reqs
	if req2.Op != "inbox" || req2.Through != fmt.Sprintf("%019d", t2.UnixNano()) {
		t.Fatalf("expected inbox op for needs_you event, got %+v", req2)
	}
}

func TestBoardHelpAndDetailHintsForNewViews(t *testing.T) {
	for _, v := range []boardView{viewList, viewGC, viewEvents, viewAnswer} {
		lines := helpLinesFor(v)
		if len(lines) == 0 {
			t.Fatalf("helpLinesFor(%v) empty", v)
		}
		for _, l := range lines {
			if n := utf8.RuneCountInString(l); n > 90 {
				t.Errorf("help line too wide (%d runes) in view %v: %q", n, v, l)
			}
		}
	}
	mainHelp := strings.Join(helpLinesFor(viewList), "\n")
	for _, want := range []string{"E     live events", "N         create top-level bead", "@  ask coord", "D         start with a [roles.<name>]", "; M       mark/unmark row"} {
		if !strings.Contains(mainHelp, want) {
			t.Errorf("main help missing %q", want)
		}
	}

	// Detail pane hints reflect live approval prompt and structured needs_me answering.
	blockedRow := boardRow{
		bead:  boardBead("bk-b", StatusInProgress, 1),
		agent: &Agent{Name: "bk-b", Kind: "claude", Status: "blocked"},
	}
	blockedDetail := strings.Join(texts(detailLines(blockedRow, &beadDetail{full: &blockedRow.bead, tailDone: true}, 80)), "\n")
	if !strings.Contains(blockedDetail, "A opens the approval prompt") {
		t.Errorf("blocked detail missing prompt hint:\n%s", blockedDetail)
	}

	qBead := boardBead("bk-q", StatusNeedsMe, 1)
	qBead.Notes = "QUESTION: Pick one\nOPTIONS:\n1. Alpha\n2. Beta"
	qRow := boardRow{bead: qBead}
	qDetail := strings.Join(texts(detailLines(qRow, &beadDetail{full: &qBead}, 80)), "\n")
	if !strings.Contains(qDetail, "A opens the structured question") {
		t.Errorf("structured needs_me detail missing question hint:\n%s", qDetail)
	}
}
