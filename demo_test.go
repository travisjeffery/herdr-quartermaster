package main

import (
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
// layout (boardRows, renderBoard, detailLines) and replays the terminal output
// into SVG, so the pictures can't drift from the board. Nothing is read from
// bd, herdr or GitHub. scripts/demo/screenshots.sh runs it and makes the PNGs.
func TestDemoScreenshots(t *testing.T) {
	dir := os.Getenv("QM_DEMO_DIR")
	if dir == "" {
		t.Skip("set QM_DEMO_DIR to write the demo screenshots (scripts/demo/screenshots.sh)")
	}
	threads, next, ready, st := demoData()
	rows := boardRows(Config{}, st, threads, next, ready)

	const w = 100
	// The list, with the thread that waits on a command selected: its command
	// shows in the status line.
	list := renderBoard(rows, 2, "", boardFilter{}, nil, w, 24)
	writeDemo(t, dir, "board", list, w, 24)

	// The detail pane on the thread with a question for you.
	r := rows[1]
	d := &beadDetail{full: demoFull(r.bead), tail: demoTail, tailDone: true, inbox: demoInbox[:1]}
	detail := renderBoard(rows, 1, "", boardFilter{}, detailLines(r, d, w), w, 46)
	writeDemo(t, dir, "board-detail", detail, w, 46)
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
