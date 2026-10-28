package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/travisjeffery/herdr-quartermaster/internal/ansisvg"
)

// TestDemoScreenshots draws the board for the README from made-up data: a
// small open-source notes app, its beads, agents and PRs. It runs the real
// layout (buildRows, renderBoard, detailLines) and replays the terminal output
// into SVG, so the pictures can't drift from the board. Nothing is read from
// bd, herdr or GitHub. scripts/demo/screenshots.sh runs it and makes the PNGs.
func TestDemoScreenshots(t *testing.T) {
	dir := os.Getenv("QM_DEMO_DIR")
	if dir == "" {
		t.Skip("set QM_DEMO_DIR to write the demo screenshots (scripts/demo/screenshots.sh)")
	}
	m := demoBoard()
	const w = 100
	// The list, with the thread that waits on a command selected: its command
	// shows in the status line.
	selectBead(t, m, "notes-m3d")
	writeDemo(t, dir, "board", renderBoard(m, w, 30), w, 30)

	// The detail pane on the thread with a question for you.
	selectBead(t, m, "notes-7qk")
	m.view = viewDetail
	r := m.sel()
	m.details[r.bead.ID] = demoDetail(r.bead)
	writeDemo(t, dir, "board-detail", renderBoard(m, w, 46), w, 46)

	// The tidy view, with one stale claim marked.
	m.view = viewList
	press(t, m, "T")
	press(t, m, " ")
	writeDemo(t, dir, "board-tidy", renderBoard(m, w, 30), w, 30)
}

// TestDemoRecording is the README's board recording: the real board, driven
// by keys over the made-up data, one frame per step. screenshots.sh turns
// the frames into a GIF.
func TestDemoRecording(t *testing.T) {
	dir := os.Getenv("QM_DEMO_DIR")
	if dir == "" {
		t.Skip("set QM_DEMO_DIR to write the demo recording (scripts/demo/screenshots.sh)")
	}
	frames := filepath.Join(dir, "board-frames")
	if err := os.MkdirAll(frames, 0o755); err != nil {
		t.Fatal(err)
	}
	const w, h = 100, 30
	m := demoBoard()
	n := 0
	frame := func() {
		n++
		writeDemo(t, frames, fmt.Sprintf("%02d", n), renderBoard(m, w, h), w, h)
	}
	keys := func(ks ...string) {
		for _, k := range ks {
			press(t, m, k)
			if r := m.sel(); r != nil && m.view == viewDetail {
				if d, ok := m.details[r.bead.ID]; ok && d.full != nil && d.full.Description == "" {
					m.details[r.bead.ID] = demoDetail(r.bead)
				}
			}
			frame()
		}
	}
	frame()
	keys("j", "j", "j")
	typed := func(chunks ...string) {
		for _, c := range chunks {
			typeText(t, m, c)
			frame()
		}
	}
	keys("/")
	typed("r", "a", "t", "e")
	keys("\r", " ", "J")
	keys("\x1b", "A")
	typed("per", " key,", " 100/min,", " anonymous", " calls share", " one bucket")
	keys("\r", "y")
	keys("\x1b", "T", " ", "u")
	keys("\x1b", "\x1b")
}

// demoDetail is the detail pane's slow lookups, finished.
func demoDetail(b Bead) *beadDetail {
	var inbox []InboxItem
	if b.ID == demoInbox[0].Bead {
		inbox = demoInbox[:1]
	}
	return &beadDetail{full: demoFull(b), tail: demoTail, tailDone: true, inbox: inbox}
}

// demoBoard is the board over the demo's beads, agents and PRs, as bd, herdr
// and the ticker's last pass would have them.
func demoBoard() *board {
	threads, next, ready, st := demoData()
	data := boardData{BeadsAt: demoAt.Add(-12 * time.Second), State: st}
	data.State.Runs = map[string][]Run{}
	for _, th := range threads {
		data.Beads = append(data.Beads, th.Bead)
		if th.Agent != nil {
			data.Agents = append(data.Agents, *th.Agent)
		}
		if th.PR != nil {
			pr := *th.PR
			pr.Checks = demoChecks(th.Checks)
			data.State.PRs[th.Bead.ID] = pr
		}
		data.State.Runs[th.Bead.ID] = th.Runs
	}
	data.Beads = append(append(data.Beads, next...), ready...)
	// Last activity, spread the way a working afternoon spreads it.
	ages := []time.Duration{4 * time.Minute, 22 * time.Minute, 3 * time.Hour, time.Hour, 26 * time.Hour, 50 * time.Minute,
		2 * time.Hour, 40 * time.Second, 6 * time.Minute, 5 * time.Hour, 4 * 24 * time.Hour, 2 * 24 * time.Hour, 6 * 24 * time.Hour,
		9 * 24 * time.Hour, 3 * 7 * 24 * time.Hour, 5 * 7 * 24 * time.Hour}
	for i := range data.Beads {
		data.Beads[i].UpdatedAt = demoAt.Add(-ages[i%len(ages)])
	}
	f := &fakeBoard{beads: data.Beads, agents: data.Agents, gc: demoTidy(data)}
	env := f.env()
	env.local = func() (TickerState, []InboxItem) { return data.State, nil }
	m := newBoard(Config{LinearPrefixes: []string{"APP"}}, env, data, func() time.Time { return demoAt })
	m.agentsAt, m.localAt, m.beadsTried = demoAt, demoAt, demoAt
	return m
}

// demoTidy is what the tidy view finds in the demo: a claim nobody has
// touched in ten days, a bead verify thinks is done, and a closed bead's
// worktree left behind.
func demoTidy(data boardData) []gcItem {
	stale := Bead{ID: "notes-f2h", Title: "Spike: full-text search with SQLite FTS5", Status: StatusInProgress, Priority: 3,
		Assignee: "claude", UpdatedAt: demoAt.Add(-10 * 24 * time.Hour), Notes: "FTS5 works; ranking needs bm25 weights tuned"}
	flagged := Bead{ID: "notes-y4k", Title: "Retry failed webhook deliveries", Status: StatusOpen, Priority: 2, UpdatedAt: demoAt.Add(-9 * 24 * time.Hour)}
	closed := Bead{ID: "notes-a1c", Title: "Upgrade the Markdown renderer", Status: StatusClosed, ClosedAt: demoAt.Add(-3 * time.Hour),
		CloseReason: "PR #40 merged; previews render the same on 200 sample notes"}
	return gcItems("tj/", gcInputs{
		active: []Bead{stale, flagged},
		closed: []Bead{closed},
		bare:   []Bead{stale},
		inbox: []InboxItem{{Event: Event{Bead: flagged.ID, Kind: EventLikelyStale, At: demoAt.Add(-2 * time.Hour),
			Summary: "likely stale: (b) linked PR #38 merged: Retry webhooks with backoff"}}},
		candidates: []sweepCandidate{{Path: "/home/me/worktrees/notes/tj-notes-a1c-upgrade-the-markdown", Branch: "tj/notes-a1c-upgrade-the-markdown"}},
	}, demoAt)
}

// demoChecks is a check rollup that summarizes to c.
func demoChecks(c Checks) []Check {
	var out []Check
	for _, name := range c.Failing {
		out = append(out, Check{Name: name, Status: "COMPLETED", Conclusion: "FAILURE"})
	}
	for i := range c.Pending {
		out = append(out, Check{Name: "pending-" + strconv.Itoa(i), Status: "IN_PROGRESS"})
	}
	for i := range c.Passing {
		out = append(out, Check{Name: "ok-" + strconv.Itoa(i), Status: "COMPLETED", Conclusion: "SUCCESS"})
	}
	return out
}

func writeDemo(t *testing.T, dir, name, screen string, w, h int) {
	t.Helper()
	svg := ansisvg.SVG(ansisvg.Replay(screen, w, h), "quartermaster board", ansisvg.TokyoNight)
	if err := os.WriteFile(filepath.Join(dir, name+".svg"), []byte(svg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// demoAt is the made-up afternoon the demo happens on.
var demoAt = time.Date(2026, 3, 12, 14, 0, 0, 0, time.Local)

const demoRepo = "https://github.com/acme/notes/pull/"

func demoPR(n int, title, state string) *PR {
	return &PR{Number: n, Title: title, URL: demoRepo + strconv.Itoa(n), State: state, HeadSHA: "c0ffee" + strconv.Itoa(n)}
}

func demoAgent(id, kind, status string) *Agent {
	return &Agent{Name: id, Kind: kind, Status: status}
}

func demoData() (threads []Thread, next, ready []Bead, st TickerState) {
	bead := func(id, title, status string, labels ...string) Bead {
		return Bead{ID: id, Title: title, Status: status, Priority: 2, Type: "task", Labels: labels, UpdatedAt: demoAt}
	}

	question := bead("notes-7qk", "Add rate limiting to the API", StatusNeedsMe)
	question.Priority = 1
	question.Notes = strings.Join([]string{
		"Linear: APP-14",
		"Token bucket in internal/ratelimit with tests; not wired into the router yet.",
		"Draft PR: " + demoRepo + "50",
		"Question: limit per API key or per client IP? (a) per key, 100 req/min, " +
			"anonymous calls share one bucket (b) per IP, 60 req/min (c) both, whichever trips first.",
	}, "\n")
	draft := demoPR(50, "APP-14: Add rate limiting to the API", "OPEN")
	draft.IsDraft = true
	draft.MergeState = "CLEAN"
	draft.Checks = []Check{{Name: "test", Conclusion: "SUCCESS", Status: "COMPLETED"}, {Name: "lint", Conclusion: "SUCCESS", Status: "COMPLETED"}}

	rotate := bead("notes-m3d", "Rotate the staging database password", StatusNeedsMe)
	rotate.Notes = "RUN: make rotate-db-password ENV=staging\n" +
		"It needs the vault login only you have; the app picks the new secret up on its next deploy."

	ready2 := demoPR(45, "APP-11: Upgrade the Postgres driver to v5", "OPEN")
	ready2.ReviewDecision = "APPROVED"
	ready2.MergeState = "CLEAN"
	ready2.Gate = &MergeGate{Head: ready2.HeadSHA, At: demoAt}
	ready2.Reviews = []Review{{State: "APPROVED", SubmittedAt: demoAt.Add(-time.Hour)}}
	ready2.Reviews[0].Commit.OID = ready2.HeadSHA

	review := demoPR(47, "Write the v2 migration guide", "OPEN")
	review.ReviewDecision = "REVIEW_REQUIRED"
	review.MergeState = "BLOCKED"

	failing := demoPR(48, "APP-17: Dark mode for the settings page", "OPEN")
	failing.MergeState = "UNSTABLE"

	running := demoPR(49, "Cache rendered Markdown previews", "OPEN")
	running.IsDraft = true

	threads = []Thread{
		{Bead: question, Agent: demoAgent(question.ID, "claude", "idle"), PR: draft, Checks: Checks{Passing: 2}, Linear: "APP-14"},
		{Bead: rotate, Agent: demoAgent(rotate.ID, "codex", "idle")},
		{Bead: bead("notes-2fz", "Fix the flaky login test", StatusInProgress), Agent: demoAgent("notes-2fz", "claude", "idle"),
			PR: demoPR(42, "APP-9: Fix the flaky login test", "MERGED"), Linear: "APP-9"},
		{Bead: bead("notes-9wc", "Upgrade the Postgres driver to v5", StatusInProgress), Agent: demoAgent("notes-9wc", "claude", "idle"),
			PR: ready2, Checks: Checks{Passing: 6}, Linear: "APP-11"},
		{Bead: bead("notes-r8t", "Write the v2 migration guide", StatusInProgress), Agent: demoAgent("notes-r8t", "muse", "idle"),
			PR: review, Checks: Checks{Passing: 3}},
		{Bead: bead("notes-x5n", "Move image uploads to object storage", StatusInProgress, LabelRollingOut),
			Agent: demoAgent("notes-x5n", "claude", "idle"), PR: demoPR(39, "Move image uploads to object storage", "MERGED"),
			Runs: []Run{{Workflow: "Deploy production", Status: "in_progress"}}},
		{Bead: bead("notes-h6p", "Dark mode for the settings page", StatusInProgress), Agent: demoAgent("notes-h6p", "codex", "idle"),
			PR: failing, Checks: Checks{Failing: []string{"e2e (firefox)"}, Passing: 5}, Linear: "APP-17"},
		{Bead: bead("notes-c4j", "Paginate the search endpoint", StatusInProgress), Agent: demoAgent("notes-c4j", "claude", "working")},
		{Bead: bead("notes-u2e", "Cache rendered Markdown previews", StatusInProgress), Agent: demoAgent("notes-u2e", "muse", "working"),
			PR: running, Checks: Checks{Pending: 4, Passing: 2}},
		{Bead: bead("notes-b7v", "Translate the onboarding emails", StatusInProgress), Agent: demoAgent("notes-b7v", "codex", "idle")},
		{Bead: bead("notes-k9s", "Single sign-on for the admin panel", StatusBlocked)},
	}

	next = []Bead{
		{ID: "notes-d1f", Title: "CSV export for reports", Status: StatusOpen, Priority: 1, Type: "feature", Labels: []string{LabelNext}},
		{ID: "notes-p5g", Title: "Invoices show the wrong time zone", Status: StatusOpen, Priority: 2, Type: "bug", Labels: []string{LabelNext}},
	}
	ready = []Bead{
		{ID: "notes-w8m", Title: "Webhooks for new comments", Status: StatusOpen, Priority: 2, Type: "feature"},
		{ID: "notes-e3r", Title: "Remove feature flags that shipped", Status: StatusOpen, Priority: 3, Type: "chore"},
		{ID: "notes-s6y", Title: "Document the release process", Status: StatusOpen, Priority: 3, Type: "task"},
	}
	st = TickerState{PRs: map[string]PR{question.ID: *draft}}
	return threads, next, ready, st
}

// demoFull is the bead as bd show has it, with its description.
func demoFull(b Bead) *Bead {
	b.Description = "One noisy client can slow the API down for everyone. Add a limiter in front of /api " +
		"that answers 429 with Retry-After, and document the limits."
	b.Labels = []string{"api"}
	return &b
}

// demoTail is the end of the worker's screen.
var demoTail = []string{
	"● The limiter is in internal/ratelimit (token bucket, 14 tests), and the draft",
	"  PR is up. Before I wire it into the router I need one decision from you:",
	"  should the limit be per API key, per client IP, or both? Noted on the bead.",
	"❯",
}

var demoInbox = []InboxItem{
	{Event: Event{Bead: "notes-7qk", Kind: EventNeedsYou, At: demoAt.Add(-4 * time.Minute),
		Summary: "notes-7qk needs you: limit per API key or per client IP?"}},
}
