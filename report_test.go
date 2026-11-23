package main

import (
	"slices"
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	est := time.FixedZone("EST", -5*3600)
	now := time.Date(2026, 9, 25, 9, 30, 0, 0, est)
	cases := map[string]time.Time{
		"":           now.Add(-24 * time.Hour),
		"48h":        now.Add(-48 * time.Hour),
		"7d":         time.Date(2026, 9, 18, 9, 30, 0, 0, est),
		"2026-09-20": time.Date(2026, 9, 20, 0, 0, 0, 0, est),
	}
	for in, want := range cases {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"0h", "-3d", "yesterday", "2026-9-20", "3w"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) accepted", bad)
		}
	}
}

func TestBuildReportGroupsBeadsAndPRs(t *testing.T) {
	since := t0.Add(-24 * time.Hour)
	closed := []Bead{
		{ID: "backend-late", Status: StatusClosed, ClosedAt: t0.Add(-time.Hour), Notes: "PR: https://github.com/acme/app/pull/1"},
		{ID: "backend-early", Status: StatusClosed, ClosedAt: t0.Add(-2 * time.Hour)},
		{ID: "backend-old", Status: StatusClosed, ClosedAt: since.Add(-time.Minute), Notes: "https://github.com/acme/app/pull/9"},
	}
	active := []Bead{
		{ID: "backend-ask", Status: StatusNeedsMe},
		{ID: "backend-wip", Status: StatusInProgress},
	}
	merged := []MergedPR{
		{URL: "https://github.com/acme/app/pull/1", MergedAt: t0.Add(-90 * time.Minute)},
		{URL: "https://github.com/acme/app/pull/2", Head: "tj/backend-early-fix", MergedAt: t0.Add(-3 * time.Hour)},
		{URL: "https://github.com/acme/app/pull/3", Head: "tj/backend-wip", MergedAt: t0.Add(-4 * time.Hour)},
		{URL: "https://github.com/acme/app/pull/4", Head: "tj/backend-early2", MergedAt: t0.Add(-5 * time.Hour)},
		{URL: "https://github.com/acme/app/pull/4", Head: "tj/backend-early2", MergedAt: t0.Add(-5 * time.Hour)},
		{URL: "https://github.com/acme/app/pull/5", Head: "tj/hotfix", MergedAt: since.Add(-time.Hour)},
		{URL: "https://github.com/acme/app/pull/6", Head: "tj/eng-1102", MergedAt: t0.Add(-6 * time.Hour)},
		{URL: "https://github.com/acme/app/pull/9", MergedAt: t0.Add(-7 * time.Hour)},
	}
	st := TickerState{PRs: map[string]PR{
		"backend-ask": {Number: 6, URL: "https://github.com/acme/app/pull/6", State: "MERGED"},
		"backend-wip": {Number: 8, URL: "https://github.com/acme/app/pull/8", State: "OPEN"},
	}}

	r := buildReport(since, closed, active, merged, st, "tj/")

	var shipped []string
	for _, s := range r.Shipped {
		shipped = append(shipped, s.Bead.ID)
	}
	if !slices.Equal(shipped, []string{"backend-early", "backend-late"}) {
		t.Fatalf("shipped = %v, want in-window beads oldest first", shipped)
	}
	if !slices.Equal(r.Shipped[0].PRs, []string{"https://github.com/acme/app/pull/2"}) {
		t.Errorf("a PR on the bead's branch belongs to it: %v", r.Shipped[0].PRs)
	}
	if !slices.Equal(r.Shipped[1].PRs, []string{"https://github.com/acme/app/pull/1"}) {
		t.Errorf("a PR the notes link belongs to the bead: %v", r.Shipped[1].PRs)
	}
	var unbeaded []string
	for _, pr := range r.Unbeaded {
		unbeaded = append(unbeaded, pr.URL)
	}
	// #3 is on an active bead's branch, #6 is the ticker's PR for one and #5
	// merged before the window. #4's branch only shares a prefix with a bead id,
	// and #9's bead closed before the window.
	if !slices.Equal(unbeaded, []string{"https://github.com/acme/app/pull/9", "https://github.com/acme/app/pull/4"}) {
		t.Errorf("unbeaded = %v", unbeaded)
	}
	if len(r.InFlight) != 1 || r.InFlight[0].Bead.ID != "backend-wip" {
		t.Errorf("in flight = %+v, want only the bead with an open PR", r.InFlight)
	}
	if len(r.NeedsYou) != 1 || r.NeedsYou[0].ID != "backend-ask" {
		t.Errorf("needs you = %+v", r.NeedsYou)
	}
}

func TestInFlightStateLineFromTickerSnapshot(t *testing.T) {
	pr := PR{Number: 8, URL: "https://github.com/acme/app/pull/8", State: "OPEN", ReviewDecision: "APPROVED"}
	active := []Bead{{ID: "backend-seen", Status: StatusInProgress}, {ID: "backend-unseen", Status: StatusInProgress}}
	st := TickerState{
		PRs:     map[string]PR{"backend-seen": pr, "backend-unseen": pr},
		Threads: map[string]Snapshot{"backend-seen": {AgentStatus: "idle"}},
	}
	r := buildReport(t0, nil, active, nil, st, "tj/")
	if r.InFlight[0].State != "review · PR #8 approved" || r.InFlight[1].State != "no agent · PR #8 approved" {
		t.Fatalf("got %q and %q", r.InFlight[0].State, r.InFlight[1].State)
	}
}

func TestRenderReport(t *testing.T) {
	r := Report{
		Since: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC),
		Shipped: []Shipped{{
			Bead: Bead{ID: "backend-ab12", Title: "Fix the thing", CloseReason: "Fixed it.\nVerified in prod."},
			PRs:  []string{"https://github.com/acme/app/pull/1"},
		}},
		Unbeaded: []MergedPR{{Title: "Bump deps", URL: "https://github.com/acme/app/pull/2"}},
		NeedsYou: []Bead{{ID: "backend-cd34", Title: "Pick a region", Notes: "started\nQuestion: us-east-2 or us-west-2?"}},
		Warnings: []string{"merged PRs in /r: gh pr: boom"},
	}
	want := "# Report: 2026-09-24 09:00 to 2026-09-25 09:00 UTC\n" +
		"\n> WARNING: merged PRs in /r: gh pr: boom\n" +
		"\n## Shipped (1)\n\n" +
		"- **Fix the thing** (`backend-ab12`): Fixed it. Verified in prod.\n" +
		"  - https://github.com/acme/app/pull/1\n" +
		"\n## Merged without a bead (1)\n\n" +
		"- Bump deps: https://github.com/acme/app/pull/2\n" +
		"\n## In flight (0)\n\n_None._\n" +
		"\n## Needs you (1)\n\n" +
		"- **Pick a region** (`backend-cd34`): Question: us-east-2 or us-west-2?\n"
	if got := renderReport(r, time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
