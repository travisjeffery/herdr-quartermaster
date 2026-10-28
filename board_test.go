package main

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func testBoard() []boardRow {
	th := thread(bead(StatusInProgress), agent("working", 1), openPR())
	th.PR.URL = "https://github.com/acme/widgets/pull/7"
	th.Linear = "INF-42"
	r := boardRow{bead: th.Bead, agent: th.Agent, line: stateLine(th)}
	r.search = searchText("threads", th.Bead.ID, th.Bead.Title, th.Agent.Name, r.line, "#7 "+th.PR.URL, th.Linear)
	next := Bead{ID: "backend-cd34", Title: "Rotate the keys", Priority: 1, Type: "task"}
	ready := Bead{ID: "backend-ef56", Title: "Tidy the docs", Priority: 3, Type: "chore"}
	return []boardRow{
		{header: "threads"},
		r,
		{header: "next"},
		{bead: next, search: searchText("next", next.ID, next.Title, "P1 task")},
		{header: "ready"},
		{bead: ready, search: searchText("ready", ready.ID, ready.Title, "P3 chore")},
	}
}

func shown(rows []boardRow) []string {
	var out []string
	for _, r := range rows {
		if r.header != "" {
			out = append(out, "["+r.header+"]")
		} else {
			out = append(out, r.bead.ID)
		}
	}
	return out
}

func TestFilterRows(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{"[threads]", "backend-ab12", "[next]", "backend-cd34", "[ready]", "backend-ef56"}},
		{"ROTATE", []string{"[next]", "backend-cd34"}},     // title, any case
		{"ef56", []string{"[ready]", "backend-ef56"}},      // id
		{"inf-42", []string{"[threads]", "backend-ab12"}},  // Linear key
		{"#7", []string{"[threads]", "backend-ab12"}},      // PR number
		{"widgets", []string{"[threads]", "backend-ab12"}}, // PR repo
		{"working", []string{"[threads]", "backend-ab12"}}, // group
		{"ready", []string{"[ready]", "backend-ef56"}},     // section
		{"the next", []string{"[next]", "backend-cd34"}},   // every word, anywhere
		{"nomatch", nil},
	}
	for _, c := range cases {
		if got := shown(filterRows(testBoard(), c.query)); !slices.Equal(got, c.want) {
			t.Errorf("filter %q = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestFilteredSelectionSkipsHeaders(t *testing.T) {
	rows := filterRows(testBoard(), "tidy")
	if cur := nextSelectable(rows, 0, 1); cur != 1 || rows[cur].bead.ID != "backend-ef56" {
		t.Fatalf("selection = %d, want the only match", cur)
	}
	if cur := nextSelectable(filterRows(testBoard(), "nomatch"), 0, 1); cur != -1 {
		t.Fatalf("selection on an empty board = %d, want -1", cur)
	}
}

func TestBoardFilterKeys(t *testing.T) {
	f := boardFilter{typing: true}
	for _, k := range []string{"a", "b", "É", "\x1b[A", "\x7f"} {
		f.key(k)
	}
	if f.query != "ab" || !f.typing {
		t.Fatalf("after typing = %+v, want query ab still typing", f)
	}
	f.key("\r")
	if f.query != "ab" || f.typing {
		t.Fatalf("enter = %+v, want query kept and typing off", f)
	}
	f = boardFilter{query: "ab", typing: true}
	if !f.key("\x1b") || f.query != "" || f.typing {
		t.Fatalf("esc = %+v, want cleared", f)
	}
	f = boardFilter{query: "a", typing: true}
	if f.key("\x7f"); f.key("\x7f") {
		t.Fatal("backspace on an empty query reported a change")
	}
}

func TestSplitKeys(t *testing.T) {
	keys, rest := splitKeys([]byte("/wid\x1b[Aé\r\x1b\x7f"))
	want := []string{"/", "w", "i", "d", "\x1b[A", "é", "\r", "\x1b", "\x7f"}
	if !slices.Equal(keys, want) || rest != nil {
		t.Fatalf("splitKeys = %q rest %q, want %q", keys, rest, want)
	}
	// A rune split across reads waits for its tail.
	e := []byte("é")
	keys, rest = splitKeys(append([]byte("a"), e[0]))
	if !slices.Equal(keys, []string{"a"}) || string(rest) != string(e[:1]) {
		t.Fatalf("split rune: keys %q rest %q", keys, rest)
	}
	if keys, _ = splitKeys(append(rest, e[1])); !slices.Equal(keys, []string{"é"}) {
		t.Fatalf("rejoined rune: %q", keys)
	}
}

func TestPastedFilterIsTypedAndKept(t *testing.T) {
	f := boardFilter{typing: true}
	keys, _ := splitKeys([]byte("widgets\r"))
	for _, k := range keys {
		f.key(k)
	}
	if f.query != "widgets" || f.typing {
		t.Fatalf("pasted filter = %+v, want widgets kept", f)
	}
}

// testDetailRow is the first thread of testBoard as loadBoard builds it from a
// ticker state: a picked PR with checks, a merged one, and a noted PR the
// ticker never looked up.
func testDetailRow() (boardRow, *beadDetail) {
	r := testBoard()[1]
	r.bead.Notes = "started\nPR: https://github.com/acme/widgets/pull/7\nRUN: make deploy\nwhy: needs prod creds\nPR: https://github.com/acme/other/pull/3"
	r.run = "make deploy"
	r.linear = "INF-42"
	st := TickerState{
		PRs: map[string]PR{r.bead.ID: {Number: 7, Title: "INF-42: Fix the thing", URL: "https://github.com/acme/widgets/pull/7", State: "OPEN", IsDraft: true,
			MergeState: "BLOCKED", ReviewDecision: "REVIEW_REQUIRED",
			Checks: []Check{{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"}, {Name: "test", Status: "IN_PROGRESS"}, {Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"}}}},
		MergedPRs: map[string][]PR{r.bead.ID: {{Number: 5, URL: "https://github.com/acme/widgets/pull/5", State: "MERGED", Title: "First half"}}},
	}
	r.prs = linkedPRs(st, r.bead)
	r.moves = []string{"INF-42 → In Review (PR #7 open)"}
	full := r.bead
	full.Description = strings.Repeat("A long description line that wraps. ", 40)
	full.Parent = "backend-ab"
	full.Labels = []string{"repo:widgets"}
	d := &beadDetail{
		full:     &full,
		tail:     tailLines("output one\n─────\n> prompt\n\n\n", detailTailLines),
		tailDone: true,
		inbox:    []InboxItem{{Event: Event{Bead: r.bead.ID, Kind: EventFailing, Summary: "PR #7 checks failing: lint", At: t0}}},
	}
	return r, d
}

func texts(lines []styledLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.text)
	}
	return out
}

func TestLinkedPRs(t *testing.T) {
	r, _ := testDetailRow()
	var got []string
	for _, pr := range r.prs {
		got = append(got, prSummary(pr))
	}
	want := []string{
		"acme/widgets#7 · open · draft · blocked · review required · checks 1 failing (lint), 1 running, 1 passed",
		"acme/widgets#5 · merged",
		"acme/other#3 · not looked up yet", // noted, never looked up: still openable
	}
	if !slices.Equal(got, want) {
		t.Fatalf("linked PRs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A noted PR the ticker did look up comes from noted_prs.
	st := TickerState{Noted: map[string]PR{"https://github.com/acme/other/pull/3": {Number: 3, URL: "https://github.com/acme/other/pull/3", State: "CLOSED"}}}
	if prs := linkedPRs(st, r.bead); len(prs) != 2 || prs[1].State != "CLOSED" {
		t.Fatalf("noted lookup = %+v", prs)
	}
}

func TestDetailLines(t *testing.T) {
	r, d := testDetailRow()
	got := strings.Join(texts(detailLines(r, d, 80)), "\n")
	for _, want := range []string{
		"backend-ab12 · in_progress · P0 · labels repo:widgets · parent backend-ab",
		"Fix the thing",
		"Description\n  A long description",
		"  … 10 more lines (b shows all)",
		"Latest note\n  PR: https://github.com/acme/other/pull/3",
		"History (4 earlier notes, newest first)\n  why: needs prod creds\n  RUN: make deploy\n",
		"  run: make deploy (p copies)",
		"PRs\n  acme/widgets#7 · open · draft",
		"    INF-42: Fix the thing",
		"Linear\n  INF-42\n  expected: INF-42 → In Review (PR #7 open)",
		"Agent backend-ab12 · claude working\n  output one\n  > prompt\n",
		"Inbox (1)\n  " + t0.Local().Format("15:04") + " [checks_failing] PR #7 checks failing: lint",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "A long description line"); n == 0 {
		t.Fatal("no description")
	}
}

func TestDetailLinesLoading(t *testing.T) {
	r, _ := testDetailRow()
	got := strings.Join(texts(detailLines(r, nil, 80)), "\n")
	for _, want := range []string{"Description\n  loading…", "claude working\n  reading…"} {
		if !strings.Contains(got, want) {
			t.Errorf("loading detail lacks %q:\n%s", want, got)
		}
	}
	// A working agent whose read failed shows nothing rather than an error.
	got = strings.Join(texts(detailLines(r, &beadDetail{tailDone: true}, 80)), "\n")
	if !strings.HasSuffix(got, "claude working") {
		t.Errorf("failed read of a working agent:\n%s", got)
	}
}

func TestDetailLinesNarrow(t *testing.T) {
	r, d := testDetailRow()
	for _, w := range []int{20, 33} {
		for _, l := range detailLines(r, d, w) {
			// The agent's screen is cut at draw time, not wrapped.
			if n := utf8.RuneCountInString(l.text); n > w && !strings.HasPrefix(l.text, "  output") {
				t.Errorf("width %d: %d-rune line %q", w, n, l.text)
			}
		}
	}
}

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func TestRenderBoardWithDetail(t *testing.T) {
	r, d := testDetailRow()
	m := testModel(t, nil)
	m.rows = testBoard()
	m.rows[1] = r
	m.cur = 1
	m.view = viewDetail
	m.details[r.bead.ID] = d
	const w, hgt = 60, 18
	lines := screenLines(renderBoard(m, w, hgt))
	if len(lines) != hgt {
		t.Fatalf("%d lines, want %d", len(lines), hgt)
	}
	// Title, then the list (a third of the body), then the rule and the pane.
	if !strings.HasPrefix(lines[1], "threads") || !strings.Contains(lines[2], "backend-ab12") || !strings.HasPrefix(lines[5], "───") {
		t.Fatalf("list then rule, got:\n%s", strings.Join(lines[:6], "\n"))
	}
	screen := strings.Join(lines, "\n")
	if !strings.Contains(screen, "… more below (J scrolls, b shows the bead in full)") {
		t.Error("an overflowing pane doesn't say it was cut")
	}
	if !strings.Contains(screen, "J/K scroll") {
		t.Error("no detail key hint")
	}
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > w {
			t.Errorf("%d-rune line %q", n, l)
		}
	}
	// Without a pane, the list keeps the whole body.
	m.view = viewList
	if plain := renderBoard(m, w, hgt); strings.Contains(plain, "───") {
		t.Error("rule drawn without a pane")
	}
}

// screenLines is a rendered screen as plain lines.
func screenLines(screen string) []string {
	return strings.Split(ansiSeq.ReplaceAllString(screen, ""), "\r\n")
}

func TestWaitingNotes(t *testing.T) {
	cases := []struct {
		notes []string
		want  int
	}{
		{nil, 0},
		{[]string{"one"}, 1},
		{[]string{"a", "b", "c"}, 2},
		{[]string{"Question: per key or per IP? (a) key (b) IP", "progress", "more progress", "pending your a/b"}, 4},
		{[]string{"old?", "x", "y", "z", "w"}, 2}, // past four notes back: just the last two
		{[]string{"RUN: make deploy", "why: prod creds"}, 2},
	}
	for _, c := range cases {
		if got := waitingNotes(c.notes); got != c.want {
			t.Errorf("waitingNotes(%q) = %d, want %d", c.notes, got, c.want)
		}
	}
}

func TestTailLines(t *testing.T) {
	screen := "one\ntwo  \n╭────╮\n│    │\nthree\n  ⏵⏵ auto mode on\n\n\n"
	if got := tailLines(screen, 3); !slices.Equal(got, []string{"two", "three", "  ⏵⏵ auto mode on"}) {
		t.Fatalf("tailLines = %q", got)
	}
	if got := tailLines("", 3); len(got) != 0 {
		t.Fatalf("empty screen = %q", got)
	}
}

func TestLinearURL(t *testing.T) {
	notes := "Linear: https://linear.app/acme/issue/INF-9/other\nLinear: https://linear.app/acme/issue/INF-42/fix-the-thing"
	if got := linearURL("INF-42", notes, "fallback"); got != "https://linear.app/acme/issue/INF-42" {
		t.Errorf("noted link = %q", got)
	}
	if got := linearURL("INF-7", notes, "acme"); got != "https://linear.app/acme/issue/INF-7" {
		t.Errorf("built link = %q", got)
	}
	if got := linearURL("INF-7", "", ""); got != "" {
		t.Errorf("no workspace = %q", got)
	}
}

func TestWrap(t *testing.T) {
	got := wrap("the quick brown fox\n\nhttps://example.com/long", 10)
	want := []string{"the quick", "brown fox", "", "https://ex", "ample.com/", "long"}
	if !slices.Equal(got, want) {
		t.Fatalf("wrap = %q, want %q", got, want)
	}
}
