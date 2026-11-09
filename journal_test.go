package main

import (
	"errors"
	"io"
	"log"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAutoClassAndWhat(t *testing.T) {
	cases := []struct {
		note, what string
		class      int
	}{
		{"AUTO: [worker L7 C6] applied dev-east — gate ok", "applied dev-east", 6},
		{"AUTO: [coordinator L4 C3] dispatched b-2", "dispatched b-2", 3},
		{"AUTO: merged #1 — approved, green", "merged #1", -1},
	}
	for _, c := range cases {
		if got := autoClass(c.note); got != c.class {
			t.Errorf("autoClass(%q) = %d, want %d", c.note, got, c.class)
		}
		if got := autoWhat(c.note); got != c.what {
			t.Errorf("autoWhat(%q) = %q, want %q", c.note, got, c.what)
		}
	}
}

func TestAutoLines(t *testing.T) {
	steps := []autoStep{
		{"a-1", "AUTO: [worker L4 C2] pushed fix — ci green"},
		{"a-1", "AUTO: [worker L4 C4] merged #3 — approved"},
		{"a-2", "AUTO: [worker L7 C7] applied prod — gate ok, 2 update"},
		{"a-1", "AUTO: [worker L4 C3] resolved worktree"},
		{"a-3", "AUTO: legacy step — evidence"},
	}
	want := []string{
		"a-3 C?: AUTO: legacy step — evidence",
		"a-2 C7: AUTO: applied prod — gate ok, 2 update",
		"a-1 C4: merged #3",
		"a-1: 2 step(s) at class 3 or below",
	}
	if got := autoLines(steps); !slices.Equal(got, want) {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	if got := topClass(steps); got != 7 {
		t.Errorf("topClass = %d", got)
	}
}

func TestBudgetSpent(t *testing.T) {
	b := BudgetConfig{Steps: 3, Applies: 1, Dispatches: 2}
	recs := []journalRecord{
		{Kind: journalAuto, Bead: "a-1", Class: 6},
		{Kind: journalAuto, Bead: "a-1", Class: 2},
		{Kind: journalAuto, Bead: "a-2", Class: 2},
		{Kind: journalCheck, Bead: "x", Action: "dispatch", Pass: true},
		{Kind: journalCheck, Bead: "y", Action: "dispatch", Pass: false},
	}
	if got := budgetSpent(b, recs, "a-1", 2, "push"); got != "" {
		t.Errorf("under budget: %q", got)
	}
	if got := budgetSpent(b, recs, "a-1", 6, "apply"); !strings.Contains(got, "applies = 1") {
		t.Errorf("applies: %q", got)
	}
	if got := budgetSpent(b, append(recs, journalRecord{Kind: journalAuto, Bead: "a-1", Class: 1}), "a-1", 1, ""); !strings.Contains(got, "steps = 3") {
		t.Errorf("steps: %q", got)
	}
	if got := budgetSpent(b, append(recs, journalRecord{Kind: journalCheck, Action: "dispatch", Pass: true}), "z", 3, "dispatch"); !strings.Contains(got, "dispatches = 2") {
		t.Errorf("dispatches: %q", got)
	}
	if got := budgetSpent(BudgetConfig{}, recs, "a-1", 6, "apply"); got != "" {
		t.Errorf("0 is no limit: %q", got)
	}
}

func TestCheckBudgetAndBreaker(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.Autonomy.Budget = BudgetConfig{Applies: 1}
	f := &fakeAutonomy{bead: Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:7"}}}
	check := func(o autonomyOpts) error {
		return autonomyCmd(cfg, f.env(), []string{"check", "b-1"}, o, io.Discard)
	}
	apply := autonomyOpts{Class: -1, Action: "apply", Target: "dev"}
	if err := check(apply); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	now := time.Now()
	if err := appendJournal(journalRecord{At: now, Kind: journalAuto, Bead: "b-1", Class: 7, Text: "AUTO: [worker L7 C7] applied"}); err != nil {
		t.Fatal(err)
	}
	if err := check(apply); !errors.Is(err, errNotAutonomous) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("second apply: %v, want a budget failure", err)
	}
	// A bare check is not journaled, so it neither counts nor resets the run.
	if err := check(autonomyOpts{Class: -1}); err != nil {
		t.Fatalf("bare check at 7: %v", err)
	}
	if err := check(apply); err == nil {
		t.Fatal("third apply passed")
	}
	if err := check(apply); err == nil {
		t.Fatal("fourth apply passed")
	}
	recs, err := readJournal(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, r.Kind)
	}
	if want := []string{"check", "auto", "check", "check", "check", "breaker"}; !slices.Equal(kinds, want) {
		t.Errorf("journal kinds %q, want %q", kinds, want)
	}
	if lvl, why := cfg.beadLevel(f.bead, false); lvl != breakerCap || !strings.Contains(why, "circuit breaker") {
		t.Errorf("after the breaker: level %d (%s)", lvl, why)
	}
	items, err := readInbox()
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Event.Summary, "circuit breaker") {
		t.Errorf("inbox: %+v, %v", items, err)
	}
	// Only TJ lifts it.
	if _, err := setLevel(cfg, f.env(), "b-1", 5, 0, "the ticker (lease expired)", now); err != nil {
		t.Fatal(err)
	}
	if lvl, _ := cfg.beadLevel(f.bead, false); lvl != breakerCap {
		t.Errorf("a non-TJ level change lifted the breaker: %d", lvl)
	}
	if _, err := setLevel(cfg, f.env(), "b-1", 5, 0, "TJ", now); err != nil {
		t.Fatal(err)
	}
	if lvl, _ := cfg.beadLevel(f.bead, false); lvl != 5 {
		t.Errorf("after TJ set 5: %d", lvl)
	}
}

func TestJournalAuto(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	tk := Ticker{cfg: defaultConfig(), herdr: newHerdr(), log: log.New(io.Discard, "", 0)}
	now := time.Now()
	th := Thread{Bead: Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:5"}, Notes: "AUTO: [worker L5 C2] retried — x"}}
	var snap Snapshot
	tk.journalAuto(th, Snapshot{}, false, &snap, now)
	if recs, _ := readJournal(time.Time{}); len(recs) != 0 || snap.Auto != 1 {
		t.Fatalf("first sight journaled %d records, count %d", len(recs), snap.Auto)
	}
	prev := snap
	th.Bead.Notes += "\nAUTO: [worker L5 C2] retried — y\nAUTO: [worker L5 C2] retried — z"
	tk.journalAuto(th, prev, true, &snap, now)
	recs, err := readJournal(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].Kind != journalAuto || recs[0].Class != 2 || recs[2].Kind != journalBreaker {
		t.Errorf("journal: %+v", recs)
	}
	if snap.Auto != 3 {
		t.Errorf("count %d", snap.Auto)
	}
	if lvl, _ := tk.cfg.beadLevel(th.Bead, false); lvl != breakerCap {
		t.Errorf("repeated step: level %d, want %d", lvl, breakerCap)
	}
}

func TestEnforceDispatchBudget(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.Autonomy.Budget = BudgetConfig{Dispatches: 1}
	b := Bead{ID: "b-1", Status: StatusOpen}
	if _, err := cfg.enforce(b, "dispatch", "coordinator"); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if _, err := cfg.enforce(b, "dispatch", "coordinator"); !errors.Is(err, errNotAutonomous) || !strings.Contains(err.Error(), "budget") {
		t.Errorf("second dispatch: %v, want a budget failure", err)
	}
	if _, err := cfg.enforce(b, "dispatch", "TJ"); err != nil {
		t.Errorf("TJ's dispatch: %v", err)
	}
}
