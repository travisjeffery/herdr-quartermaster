package main

import (
	"strings"
	"testing"
	"time"
)

func TestContextStaleNeedsAgeAndNoPR(t *testing.T) {
	now := t0
	fresh := Bead{ID: "b-fresh", Status: StatusInProgress, UpdatedAt: now.Add(-2 * time.Hour)}
	old := Bead{ID: "b-old", Status: StatusInProgress, UpdatedAt: now.Add(-10 * 24 * time.Hour)}
	oldPR := Bead{ID: "b-oldpr", Status: StatusInProgress, UpdatedAt: now.Add(-10 * 24 * time.Hour)}
	threads := []Thread{{Bead: fresh}, {Bead: old}, {Bead: oldPR}}
	st := TickerState{PRs: map[string]PR{"b-oldpr": {Number: 9, State: "MERGED"}}}
	out := renderContext(Config{Repo: "/r"}, threads, nil, nil, nil, st, map[string]bool{}, now)
	if !strings.Contains(out, "- stale claims (1): b-old ") {
		t.Fatalf("only the old bead without a PR is stale:\n%s", out)
	}
	if !strings.Contains(out, "- claimed, no agent (2): b-fresh, b-oldpr") {
		t.Fatalf("fresh and PR-carrying claims are not stale:\n%s", out)
	}
}

func TestLinearTitleCheckWaitsForTitle(t *testing.T) {
	th := Thread{Bead: Bead{ID: "b1", Status: StatusInProgress}, Linear: "ENG-1", PR: &PR{Number: 3, State: "OPEN"}}
	for _, m := range linearMoves([]Thread{th}) {
		if strings.Contains(m, "title lacks") {
			t.Fatalf("flagged a PR whose title is not fetched yet: %v", m)
		}
	}
}

func TestCoordinatorGuideAppendsConfiguredRules(t *testing.T) {
	base := coordinatorGuide(Config{Repo: "/r"})
	got := coordinatorGuide(Config{Repo: "/r", CoordinatorRules: []string{"Rule one.", "  ", "Rule two."}})
	if want := base + "- Rule one.\n- Rule two.\n"; got != want {
		t.Errorf("guide tail:\n%s", got[len(got)-200:])
	}
}
