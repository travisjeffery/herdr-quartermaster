package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeBoard is bd and herdr for the board: beads and agents to list, and a
// log of every call that would change something.
type fakeBoard struct {
	beads  []Bead
	agents []Agent
	gc     []gcItem
	calls  []string
	// fail makes every write fail.
	fail error
	// listed counts bead loads.
	listed int
}

func (f *fakeBoard) env() boardEnv {
	rec := func(format string, args ...any) error {
		f.calls = append(f.calls, fmt.Sprintf(format, args...))
		return f.fail
	}
	// edit is a write bd took: what the next load returns.
	edit := func(id string, do func(*Bead)) {
		if f.fail != nil {
			return
		}
		for i := range f.beads {
			if f.beads[i].ID == id {
				do(&f.beads[i])
			}
		}
	}
	return boardEnv{
		beads: func() ([]Bead, error) {
			f.listed++
			return slices.Clone(f.beads), nil
		},
		agents: func() ([]Agent, error) { return slices.Clone(f.agents), nil },
		local:  func() (TickerState, []InboxItem) { return TickerState{}, nil },
		show: func(id string) (Bead, error) {
			for _, b := range f.beads {
				if b.ID == id {
					return b, nil
				}
			}
			return Bead{}, errors.New("no bead " + id)
		},
		read: func(string) (string, error) { return "screen", nil },
		gc:   func(boardData) ([]gcItem, error) { return f.gc, nil },

		note: func(id, text string) error {
			edit(id, func(b *Bead) { b.Notes += "\n" + text })
			return rec("note %s %s", id, text)
		},
		setStatus: func(id, s string) error {
			edit(id, func(b *Bead) { b.Status = s })
			return rec("status %s %s", id, s)
		},
		setLabel: func(id, l string, on bool) error {
			edit(id, func(b *Bead) {
				b.Labels = without(b.Labels, l)
				if on {
					b.Labels = append(b.Labels, l)
				}
			})
			return rec("label %s %s %v", id, l, on)
		},
		setPriority: func(id string, p int) error {
			edit(id, func(b *Bead) { b.Priority = p })
			return rec("priority %s %d", id, p)
		},
		close: func(id, reason string) error { return rec("close %s %s", id, reason) },
		child: func(parent Bead, title string) (string, error) {
			return parent.ID + ".1", rec("child %s %s", parent.ID, title)
		},
		prompt:   func(a, text string) error { return rec("prompt %s %s", a, text) },
		dispatch: func(id, kind string) (string, error) { return "started", rec("dispatch %s %s", id, kind) },
		resume:   func(id string) (string, error) { return "resumed", rec("resume %s", id) },
		resolve:  func(id string) (string, error) { return "removed", rec("resolve %s", id) },
		release:  func(b Bead, days int) error { return rec("release %s %d", b.ID, days) },
		autonomy: func(id string, on bool) (string, error) {
			add, drop := LabelAutonomous, LabelSupervised
			if !on {
				add, drop = drop, add
			}
			edit(id, func(b *Bead) { b.Labels = append(without(b.Labels, drop), add) })
			return "autonomy set", rec("autonomy %s %v", id, on)
		},
		focus:     func(a string) error { return rec("focus %s", a) },
		inboxDone: func(id string) error { return rec("dismiss %s", id) },
		copy:      func(text string) error { return rec("copy %s", text) },
		open:      func(u string) string { rec("open %s", u); return "opened " + u },
	}
}

func boardBead(id, status string, p int, labels ...string) Bead {
	return Bead{ID: id, Title: "Title of " + id, Status: status, Priority: p, Type: "task", Labels: labels, UpdatedAt: t0}
}

// testModel is a board over f's beads and agents, as of t0.
func testModel(t *testing.T, f *fakeBoard) *board {
	t.Helper()
	if f == nil {
		f = &fakeBoard{}
	}
	m := newBoard(Config{}, f.env(), boardData{Beads: slices.Clone(f.beads), Agents: slices.Clone(f.agents), BeadsAt: t0}, func() time.Time { return t0 })
	// Nothing is due: tests start each refresh themselves.
	m.agentsAt, m.localAt, m.beadsTried = t0, t0, t0
	return m
}

// drive runs cmds to completion, feeding what each returns back in, as the
// loop does, but in order and on this goroutine.
func drive(t *testing.T, m *board, cmds []cmd) {
	t.Helper()
	for i := 0; len(cmds) > 0; i++ {
		if i > 50 {
			t.Fatal("commands never settle")
		}
		c := cmds[0]
		cmds = cmds[1:]
		if c != nil {
			cmds = append(cmds, m.apply(c())...)
		}
	}
}

// press types each key into the board and runs what it starts.
func press(t *testing.T, m *board, keys ...string) {
	t.Helper()
	for _, k := range keys {
		drive(t, m, m.key(k))
	}
}

func typeText(t *testing.T, m *board, s string) {
	t.Helper()
	keys, _ := splitKeys([]byte(s))
	press(t, m, keys...)
}

// selectBead moves the selection onto id.
func selectBead(t *testing.T, m *board, id string) {
	t.Helper()
	for i, r := range m.rows {
		if r.header == "" && r.bead.ID == id {
			m.cur = i
			return
		}
	}
	t.Fatalf("%s not on the board", id)
}

func sections(rows []boardRow) []string {
	var out []string
	for _, r := range rows {
		if r.header != "" {
			out = append(out, r.header)
		} else {
			out[len(out)-1] += " " + r.bead.ID
		}
	}
	return out
}

func testFleet() *fakeBoard {
	return &fakeBoard{
		beads: []Bead{
			boardBead("bk-ask", StatusNeedsMe, 1),
			boardBead("bk-work", StatusInProgress, 2),
			boardBead("bk-idle", StatusInProgress, 2),
			boardBead("bk-gone", StatusInProgress, 3),
			boardBead("bk-wait", StatusBlocked, 2),
			boardBead("bk-nx2", StatusOpen, 2, LabelNext),
			boardBead("bk-nx1", StatusOpen, 1, LabelNext),
			{ID: "bk-dep", Title: "Waits", Status: StatusOpen, Priority: 2, Type: "bug", UpdatedAt: t0,
				Dependencies: []Dependency{{DependsOn: "bk-wait", Type: "blocks"}, {DependsOn: "bk-done", Type: "blocks"}, {DependsOn: "bk-nx1", Type: "parent-child"}}},
			boardBead("bk-old", StatusOpen, 3),
		},
		agents: []Agent{
			{Name: "bk-ask", Kind: "claude", Status: "idle", PaneID: "w:p1"},
			{Name: "bk-work", Kind: "claude", Status: "working", PaneID: "w:p2"},
			{Name: "bk-idle", Kind: "codex", Status: "idle", PaneID: "w:p3"},
		},
	}
}

func TestBuildRowsShowsEveryOpenBeadBySection(t *testing.T) {
	m := testModel(t, testFleet())
	got := sections(m.all)
	want := []string{
		"needs you bk-ask",
		"idle bk-idle",
		"working bk-work",
		"blocked bk-wait",
		"no agent bk-gone",
		"next bk-nx1 bk-nx2", // P1 first
		"backlog bk-dep bk-old",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sections =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range m.all {
		if r.bead.ID == "bk-dep" && r.line != "bug · waits on bk-wait" {
			t.Errorf("dep line = %q: want only its open blocker", r.line)
		}
		if r.header == "next" && r.count != 2 {
			t.Errorf("next header count = %d", r.count)
		}
	}
}

func TestSortModes(t *testing.T) {
	f := &fakeBoard{beads: []Bead{boardBead("bk-a", StatusOpen, 3), boardBead("bk-b", StatusOpen, 1), boardBead("bk-c", StatusOpen, 2)}}
	f.beads[0].UpdatedAt = t0.Add(time.Hour) // newest
	f.beads[1].UpdatedAt = t0.Add(-time.Hour)
	m := testModel(t, f)
	for _, want := range []string{"backlog bk-b bk-c bk-a", "backlog bk-a bk-c bk-b", "backlog bk-b bk-c bk-a"} {
		if got := sections(m.all)[0]; got != want {
			t.Errorf("by %s = %q, want %q", m.sort, got, want)
		}
		press(t, m, "s")
	}
	// The cycle comes back round to priority.
	if m.sort != sortPriority {
		t.Fatalf("sort = %s", m.sort)
	}
}

func TestFilterMatchesLabelsAndStatus(t *testing.T) {
	f := testFleet()
	f.beads[3].Labels = []string{"repo:infra"}
	m := testModel(t, f)
	press(t, m, "/")
	typeText(t, m, "infra\r")
	if got := sections(m.rows); !slices.Equal(got, []string{"no agent bk-gone"}) {
		t.Fatalf("label filter = %v", got)
	}
	press(t, m, "\x1b")
	press(t, m, "/")
	typeText(t, m, "needs_me\r")
	if got := sections(m.rows); !slices.Equal(got, []string{"needs you bk-ask"}) {
		t.Fatalf("status filter = %v", got)
	}
}

func TestJumpToBead(t *testing.T) {
	m := testModel(t, testFleet())
	press(t, m, "/")
	typeText(t, m, "next\r")
	press(t, m, ":")
	typeText(t, m, "old\r")
	if r := m.sel(); r == nil || r.bead.ID != "bk-old" || m.filter.query != "" {
		t.Fatalf("jump: selected %v, filter %q; want bk-old with the filter cleared", r, m.filter.query)
	}
	press(t, m, ":")
	typeText(t, m, "nope\r")
	if !strings.Contains(m.status, "no open bead matches nope") {
		t.Fatalf("status = %q", m.status)
	}
}

func TestSectionJumps(t *testing.T) {
	m := testModel(t, testFleet())
	press(t, m, "]", "]")
	if r := m.sel(); r.bead.ID != "bk-work" {
		t.Fatalf("after ]] on %s, want bk-work", r.bead.ID)
	}
	press(t, m, "[")
	if r := m.sel(); r.bead.ID != "bk-idle" {
		t.Fatalf("after [ on %s, want bk-idle", r.bead.ID)
	}
	press(t, m, "G")
	if r := m.sel(); r.bead.ID != "bk-old" {
		t.Fatalf("G on %s", r.bead.ID)
	}
	press(t, m, "]")
	if r := m.sel(); r.bead.ID != "bk-old" {
		t.Fatalf("] past the end moved to %s", r.bead.ID)
	}
}

func TestFirstPaintFromCacheThenLoad(t *testing.T) {
	f := testFleet()
	cache := filepath.Join(t.TempDir(), "board.json")
	if err := saveBoardCache(cache, f.beads[:2], t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := t0
	m := startBoard(Config{}, f.env(), cache, func() time.Time { return now })
	if f.listed != 0 {
		t.Fatal("the first paint waited on bd")
	}
	if got := sections(m.all); len(got) != 2 {
		t.Fatalf("cached board = %v", got)
	}
	if title := m.titleLine(); !strings.Contains(title, "beads 1h ago") {
		t.Fatalf("title = %q: want the cache's age", title)
	}
	cmds := m.tick()
	if title := m.titleLine(); !strings.Contains(title, "refreshing…") {
		t.Fatalf("title while loading = %q", title)
	}
	drive(t, m, cmds)
	if f.listed != 1 || len(boardCountsAll(m)) != 9 {
		t.Fatalf("after the load: %d lists, %d beads", f.listed, len(boardCountsAll(m)))
	}
	// Nothing is due again until beadsEvery passes.
	now = now.Add(beadsEvery / 2)
	drive(t, m, m.tick())
	if f.listed != 1 {
		t.Fatal("beads reloaded before they were due")
	}
}

func boardCountsAll(m *board) []string {
	var out []string
	for _, r := range m.all {
		if r.header == "" {
			out = append(out, r.bead.ID)
		}
	}
	return out
}

func TestAddNote(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)
	selectBead(t, m, "bk-gone")
	press(t, m, "a")
	typeText(t, m, "looked: it is waiting on the vendor\r")
	if !slices.Equal(f.calls, []string{"note bk-gone looked: it is waiting on the vendor"}) {
		t.Fatalf("calls = %v", f.calls)
	}
	if m.status != "noted on bk-gone" || f.listed != 1 {
		t.Fatalf("status %q, %d reloads", m.status, f.listed)
	}
	// Esc and an empty line both cancel.
	press(t, m, "a")
	typeText(t, m, "draft\x1b")
	press(t, m, "a", "\r")
	if len(f.calls) != 1 {
		t.Fatalf("cancelled notes were sent: %v", f.calls)
	}
}

func TestNoteShowsBeforeReload(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)
	selectBead(t, m, "bk-gone")
	press(t, m, "a")
	cmds := m.key("x") // still typing
	typeText(t, m, "y")
	cmds = m.key("\r")
	if latest, _ := notesSplit(m.sel().bead.Notes); latest != "xy" {
		t.Fatalf("latest note before bd answers = %q", latest)
	}
	drive(t, m, cmds)
}

func TestAnswerOnlyAnIdleWorker(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)

	selectBead(t, m, "bk-work")
	press(t, m, "A")
	if m.ask != nil || !strings.Contains(m.status, "working; answer once it stops") {
		t.Fatalf("working agent: ask %v status %q", m.ask, m.status)
	}
	selectBead(t, m, "bk-gone")
	press(t, m, "A")
	if m.ask != nil || !strings.Contains(m.status, "no agent") {
		t.Fatalf("no agent: status %q", m.status)
	}

	// An approval prompt is never answered from the board.
	f.agents[0].Status = "blocked"
	m.data.Agents = slices.Clone(f.agents)
	m.rebuild()
	selectBead(t, m, "bk-ask")
	press(t, m, "A")
	if m.ask != nil || !strings.Contains(m.status, "approval prompt") {
		t.Fatalf("blocked agent: status %q", m.status)
	}

	f.agents[0].Status = "idle"
	m.data.Agents = slices.Clone(f.agents)
	m.rebuild()
	selectBead(t, m, "bk-ask")
	press(t, m, "A")
	typeText(t, m, "per key, option a\r")
	if m.ask == nil || !strings.Contains(m.ask.prompt, `send to bk-ask: "per key, option a"?`) {
		t.Fatalf("no confirmation: %+v", m.ask)
	}
	press(t, m, "n")
	if len(f.calls) != 0 {
		t.Fatalf("declined answer was sent: %v", f.calls)
	}
	press(t, m, "A")
	typeText(t, m, "per key\r")
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"prompt bk-ask per key"}) || m.status != "sent to bk-ask" {
		t.Fatalf("calls %v status %q", f.calls, m.status)
	}

	// The agent hit an approval prompt while the answer was typed.
	press(t, m, "A")
	typeText(t, m, "and another thing\r")
	f.agents[0].Status = "blocked"
	press(t, m, "y")
	if len(f.calls) != 1 || !strings.Contains(m.status, "not sent") {
		t.Fatalf("answered a blocked agent: calls %v status %q", f.calls, m.status)
	}
}

func TestDispatchAndResumeConfirm(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)
	selectBead(t, m, "bk-nx1")
	press(t, m, "\r")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "start an agent on bk-nx1") {
		t.Fatalf("enter on a bead with no agent: %+v", m.ask)
	}
	press(t, m, "n")
	press(t, m, "x", "y")
	if !slices.Equal(f.calls, []string{"dispatch bk-nx1 codex"}) || !m.quit {
		t.Fatalf("calls %v quit %v", f.calls, m.quit)
	}

	m = testModel(t, f)
	f.calls = nil
	selectBead(t, m, "bk-idle")
	press(t, m, "R")
	if m.ask != nil || !strings.Contains(m.status, "already has an agent") {
		t.Fatalf("resume with an agent: %q", m.status)
	}
	selectBead(t, m, "bk-gone")
	press(t, m, "R", "y")
	if !slices.Equal(f.calls, []string{"resume bk-gone"}) {
		t.Fatalf("calls %v", f.calls)
	}
	// Enter on a bead with an agent focuses it, without asking.
	selectBead(t, m, "bk-idle")
	press(t, m, "\r")
	if f.calls[len(f.calls)-1] != "focus bk-idle" || !m.quit {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestBeadEdits(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)
	selectBead(t, m, "bk-old")
	press(t, m, "n")
	if r := m.sel(); r.group != sectionNext {
		t.Fatalf("n left bk-old in %s", sectionName(r.group))
	}
	press(t, m, "S", "b")
	press(t, m, "S", "b") // already blocked: nothing to do
	press(t, m, "S", "z") // not a status: cancelled
	press(t, m, "P", "0")
	press(t, m, "+")
	typeText(t, m, "Write the runbook\r")
	press(t, m, "y")
	press(t, m, "C")
	typeText(t, m, "duplicate of bk-dep\r")
	press(t, m, "y")
	want := []string{
		"label bk-old next true",
		"status bk-old blocked",
		"priority bk-old 0",
		"child bk-old Write the runbook",
		"close bk-old duplicate of bk-dep",
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls =\n%s\nwant\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestActionErrorsShow(t *testing.T) {
	f := testFleet()
	f.fail = errors.New("bd note: database locked")
	m := testModel(t, f)
	press(t, m, "a")
	typeText(t, m, "hi\r")
	if !strings.Contains(m.status, "database locked") {
		t.Fatalf("status = %q", m.status)
	}
}

func TestReloadAfterAnActionWaitsForTheRunningLoad(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)
	running := m.loadBeads()
	press(t, m, "n") // its reload must not be lost while one is in flight
	if !m.reloadAgain {
		t.Fatal("no reload queued behind the running one")
	}
	drive(t, m, []cmd{running})
	if f.listed != 2 {
		t.Fatalf("%d loads, want the running one and one after the write", f.listed)
	}
}

func TestDetailPaneLoadsOnce(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)
	selectBead(t, m, "bk-ask")
	press(t, m, " ")
	d := m.details["bk-ask"]
	if m.view != viewDetail || d == nil || d.full == nil || !d.tailDone || d.tail[0] != "screen" {
		t.Fatalf("detail = %+v", d)
	}
	if cmds := m.tick(); len(cmds) != 0 {
		t.Fatalf("detail looked up again: %d commands", len(cmds))
	}
	press(t, m, "\x1b")
	if m.view != viewList || m.quit {
		t.Fatal("esc in the pane should close the pane only")
	}
}

func TestRenderFitsEightyColumns(t *testing.T) {
	f := testFleet()
	for i := range 40 {
		f.beads = append(f.beads, boardBead(fmt.Sprintf("backend-x%02d", i), StatusOpen, i%5))
		f.beads[len(f.beads)-1].Title = strings.Repeat("a long title ", 8)
		f.beads[len(f.beads)-1].Notes = strings.Repeat("a long note ", 20)
	}
	m := testModel(t, f)
	for _, view := range []boardView{viewList, viewDetail, viewHelp} {
		m.view = view
		lines := screenLines(renderBoard(m, 80, 24))
		if len(lines) != 24 {
			t.Fatalf("view %d: %d lines", view, len(lines))
		}
		for _, l := range lines {
			if n := utf8.RuneCountInString(l); n > 80 {
				t.Errorf("view %d: %d-rune line %q", view, n, l)
			}
		}
	}
	m.view = viewList
	lines := screenLines(renderBoard(m, 80, 24))
	if !strings.HasPrefix(lines[0], "quartermaster · 49 open · 1 need you · by priority") {
		t.Errorf("title = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "needs you (1)") || !strings.Contains(lines[2], "bk-ask") || !strings.Contains(lines[2], "P1 ○") {
		t.Errorf("first rows:\n%s\n%s", lines[1], lines[2])
	}
}

func TestGCItems(t *testing.T) {
	now := t0
	stale := boardBead("bk-stale", StatusInProgress, 2)
	stale.UpdatedAt = now.Add(-10 * 24 * time.Hour)
	stale.Assignee = "claude"
	stale.Notes = "started\nhalfway"
	fresh := boardBead("bk-fresh", StatusInProgress, 2)
	withPR := boardBead("bk-pr", StatusInProgress, 2)
	withPR.UpdatedAt = stale.UpdatedAt
	flagged := boardBead("bk-flag", StatusOpen, 2)
	done := boardBead("bk-done", StatusClosed, 2)
	done.ClosedAt = now.Add(-2 * 24 * time.Hour)
	done.CloseReason = "shipped in #9"
	dirty := boardBead("bk-dirty", StatusClosed, 2)
	ghost := boardBead("bk-ghost", StatusClosed, 2)
	in := gcInputs{
		active: []Bead{stale, fresh, withPR, flagged},
		closed: []Bead{done, dirty, ghost},
		agents: []Agent{{Name: "bk-ghost", Kind: "claude", Status: "idle"}},
		prs:    map[string]PR{"bk-pr": {Number: 3}},
		bare:   []Bead{stale, fresh, withPR},
		inbox: []InboxItem{
			{Event: Event{Bead: "bk-flag", Kind: EventLikelyStale, Summary: "likely stale: (b) PR #4 merged", At: now.Add(-time.Hour)}},
			{Event: Event{Bead: "bk-flag", Kind: EventLikelyStale, Summary: "older flag", At: now.Add(-2 * time.Hour)}},
			{Event: Event{Bead: "bk-done", Kind: EventAutoClosed, Summary: "closed as already done: (a) INF-1 done", At: now.Add(-time.Hour)}},
			{Event: Event{Bead: "bk-fresh", Kind: EventNeedsYou, Summary: "not a tidy item"}},
		},
		candidates: []sweepCandidate{
			{Path: "/w/bk-done", Branch: "tj/bk-done-title-of-bk-done", Reasons: []string{"bk-done closed"}},
			{Path: "/w/bk-dirty", Branch: "tj/bk-dirty", Dirty: true},
			{Path: "/w/other", Branch: "tj/unrelated", Merged: true},
		},
	}
	items := gcItems("tj/", in, now)
	byID := map[string]gcItem{}
	var order []string
	for _, it := range items {
		byID[it.bead.ID] = it
		order = append(order, it.bead.ID)
	}
	if want := []string{"bk-stale", "bk-done", "bk-flag", "bk-dirty", "bk-ghost"}; !slices.Equal(order, want) {
		t.Fatalf("items = %v, want %v", order, want)
	}
	s := byID["bk-stale"]
	if !s.canRelease || !s.canClose || s.canResolve || !strings.Contains(s.evidence[0], "untouched 10d") ||
		!strings.Contains(s.evidence[0], "claimed by claude") || s.evidence[1] != "last note: halfway" {
		t.Errorf("stale claim = %+v", s)
	}
	fl := byID["bk-flag"]
	if !fl.canClose || !fl.canDismiss || len(fl.evidence) != 1 || !strings.Contains(fl.evidence[0], "PR #4 merged") {
		t.Errorf("likely stale = %+v: want the latest flag only", fl)
	}
	d := byID["bk-done"]
	if d.canClose || !d.canDismiss || !d.canResolve || !slices.Equal(d.kinds, []string{"auto-closed", "leftover worktree"}) {
		t.Errorf("auto-closed with a worktree = %+v", d)
	}
	if !strings.Contains(strings.Join(d.evidence, "\n"), "closed 2d ago: shipped in #9\nworktree /w/bk-done (tj/bk-done-title-of-bk-done)") {
		t.Errorf("leftover evidence = %q", d.evidence)
	}
	if dd := byID["bk-dirty"]; dd.canResolve || dd.keep != "dirty" {
		t.Errorf("dirty worktree = %+v: want kept", dd)
	}
	if g := byID["bk-ghost"]; g.agent != "bk-ghost" || g.canResolve || !slices.Equal(g.kinds, []string{"leftover agent"}) {
		t.Errorf("leftover agent = %+v", g)
	}
}

func TestGCBulkActions(t *testing.T) {
	f := testFleet()
	f.gc = []gcItem{
		{bead: boardBead("bk-1", StatusInProgress, 2), canRelease: true, canClose: true},
		{bead: boardBead("bk-2", StatusOpen, 2), canClose: true, canDismiss: true},
		{bead: boardBead("bk-3", StatusClosed, 2), canResolve: true, canDismiss: true},
		{bead: boardBead("bk-4", StatusClosed, 2), keep: "dirty"},
	}
	m := testModel(t, f)
	press(t, m, "T")
	if m.view != viewGC || len(m.gc) != 4 {
		t.Fatalf("tidy view: %d items", len(m.gc))
	}
	screen := strings.Join(screenLines(renderBoard(m, 80, 30)), "\n")
	if !strings.Contains(screen, "[ ] bk-1") || !strings.Contains(screen, "C close · u release") {
		t.Fatalf("tidy screen:\n%s", screen)
	}

	// Release applies to the marked stale claim only, after a confirmation.
	press(t, m, "*", "u")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "release bk-1 back to open") {
		t.Fatalf("release ask = %+v", m.ask)
	}
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"release bk-1 0"}) {
		t.Fatalf("calls %v", f.calls)
	}
	// What was acted on is unmarked; the rest stay marked.
	if m.gcMarked["bk-1"] || !m.gcMarked["bk-2"] {
		t.Fatalf("marks = %v", m.gcMarked)
	}
	f.calls = nil
	press(t, m, "C")
	typeText(t, m, "already done\r")
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"close bk-2 already done"}) {
		t.Fatalf("close calls %v", f.calls)
	}
	f.calls = nil
	press(t, m, "*", "*", "d", "y")
	if !slices.Equal(f.calls, []string{"resolve bk-3"}) {
		t.Fatalf("resolve calls %v: never the kept worktree", f.calls)
	}
	// With nothing marked, actions apply to the selected item.
	f.calls = nil
	m.gcMarked = map[string]bool{}
	m.gcCur = 3
	press(t, m, "d")
	if m.ask != nil || !strings.Contains(m.status, "nothing to resolve") {
		t.Fatalf("resolving a kept worktree: ask %+v status %q", m.ask, m.status)
	}
	press(t, m, "\x1b")
	if m.view != viewList {
		t.Fatal("esc should leave the tidy view")
	}
}

func TestAutonomyToggleConfirms(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeBoard{beads: []Bead{boardBead("bk-a", StatusInProgress, 2, LabelSupervised)}}
	m := testModel(t, f)
	m.cfg.Autonomy = defaultConfig().Autonomy
	m.rebuild()
	selectBead(t, m, "bk-a")
	press(t, m, "U")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "grant autonomy to bk-a") {
		t.Fatalf("no grant confirmation: %+v", m.ask)
	}
	press(t, m, "n")
	if len(f.calls) != 0 {
		t.Fatalf("declined grant ran: %v", f.calls)
	}
	press(t, m, "U")
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"autonomy bk-a true"}) {
		t.Fatalf("calls %v", f.calls)
	}
	if r := m.sel(); r == nil || !strings.Contains(r.line, "auto") {
		t.Fatalf("no auto marker after the grant: %+v", r)
	}
	press(t, m, "U")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "revoke autonomy on bk-a") {
		t.Fatalf("no revoke confirmation: %+v", m.ask)
	}
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"autonomy bk-a true", "autonomy bk-a false"}) {
		t.Fatalf("calls %v", f.calls)
	}
	if r := m.sel(); r == nil || strings.Contains(r.line, "auto") {
		t.Fatalf("auto marker after the revoke: %+v", r)
	}
}
