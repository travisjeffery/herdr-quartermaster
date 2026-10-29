package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAutonomous(t *testing.T) {
	on := defaultConfig().Autonomy
	auto := Bead{ID: "backend-ab12", Status: StatusInProgress, Labels: []string{LabelAutonomous}}
	plain := Bead{ID: "backend-ab12", Status: StatusInProgress}
	off := on
	off.Enabled = false
	noMerge := on
	noMerge.Merge = false
	byDefault := on
	byDefault.Default = true
	cases := []struct {
		name    string
		a       AutonomyConfig
		b       Bead
		stopped bool
		action  string
		want    bool
		why     string
	}{
		{"labeled", on, auto, false, "", true, ""},
		{"labeled merge", on, auto, false, "merge", true, ""},
		{"unlabeled", on, plain, false, "", false, "not labeled autonomous"},
		{"stop file", on, auto, true, "", false, "autonomy is off"},
		{"config off", off, auto, false, "", false, "enabled = false"},
		{"step off", noMerge, auto, false, "merge", false, "merge = false"},
		{"other step still on", noMerge, auto, false, "apply", true, ""},
		{"default on", byDefault, plain, false, "", true, ""},
		{"supervised beats default", byDefault, Bead{ID: "x", Labels: []string{LabelSupervised}}, false, "", false, "supervised"},
		{"supervised beats label", on, Bead{ID: "x", Labels: []string{LabelAutonomous, LabelSupervised}}, false, "", false, "supervised"},
		{"closed", on, Bead{ID: "x", Status: StatusClosed, Labels: []string{LabelAutonomous}}, false, "", false, "closed"},
	}
	for _, c := range cases {
		got, why := autonomous(c.a, c.b, c.stopped, c.action)
		if got != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s: got %v %q, want %v containing %q", c.name, got, why, c.want, c.why)
		}
	}
}

func TestAutonomyStopFile(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	var out strings.Builder
	if autonomyStopped() {
		t.Fatal("stopped before off")
	}
	if err := autonomyCmd(cfg, []string{"off"}, "", &out); err != nil {
		t.Fatal(err)
	}
	if !autonomyStopped() {
		t.Fatal("not stopped after off")
	}
	if err := autonomyCmd(cfg, []string{"on"}, "", &out); err != nil {
		t.Fatal(err)
	}
	if autonomyStopped() {
		t.Fatal("stopped after on")
	}
	if err := autonomyCmd(cfg, []string{"on"}, "", &out); err != nil {
		t.Fatalf("on twice: %v", err)
	}
	if err := autonomyCmd(cfg, []string{"check", "x"}, "deploy", &out); err == nil || !strings.Contains(err.Error(), "--action") {
		t.Fatalf("unknown action: %v", err)
	}
}

func TestAutonomyStatus(t *testing.T) {
	a := defaultConfig().Autonomy
	a.Apply = false
	active := []Bead{{ID: "a-1", Labels: []string{LabelAutonomous}}, {ID: "a-2"}}
	got := autonomyStatus(a, false, active)
	for _, want := range []string{"autonomy: on", "steps: decide, merge\n", "autonomous now: a-1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
	if got := autonomyStatus(a, true, active); !strings.Contains(got, "autonomy: off") || !strings.Contains(got, "autonomous now: -") {
		t.Errorf("stopped status:\n%s", got)
	}
}

func TestAutoNotes(t *testing.T) {
	notes := "PR: https://x/pull/1\nAUTO: chose option 2 — recommended\nauto: agent resumed after needs_me\n  AUTO: merged #1 — approved, green\n"
	want := []string{"AUTO: chose option 2 — recommended", "AUTO: merged #1 — approved, green"}
	if got := autoNotes(notes); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBriefGrant(t *testing.T) {
	b := bead(StatusInProgress)
	if s := brief(b, "tj/x", "/w/x", "", "", false); strings.Contains(s, "Autonomous mode") {
		t.Error("grant in a supervised brief")
	}
	s := brief(b, "tj/x", "/w/x", "", "", true)
	for _, want := range []string{
		"quartermaster autonomy check backend-ab12 --action decide",
		"quartermaster autonomy check backend-ab12 --action merge",
		"quartermaster autonomy check backend-ab12 --action apply",
		"quartermaster gate plan plan.json",
		"--match-head-commit",
		"Never approve a PR",
		"sneaky-approve",
		"CRITICAL: <category>",
		"treat it as critical",
		"Never answer Claude Code permission prompts",
		`AUTO: <what you did>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("grant missing %q", want)
		}
	}
}

func TestStateLineAuto(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("working", 1), nil)
	th.Auto = true
	if got := stateLine(th); !strings.HasSuffix(got, " · auto") {
		t.Errorf("state line %q has no auto marker", got)
	}
}

func TestAutoChanges(t *testing.T) {
	b := Bead{ID: "a-1", Notes: "AUTO: one\nAUTO: two\nAUTO: three"}
	if got := autoChanges(digestState{}, []Bead{b}); got != nil {
		t.Errorf("first digest: %q", got)
	}
	prev := digestState{At: time.Now(), Auto: map[string]int{"a-1": 1}}
	want := []string{"a-1 AUTO: two", "a-1 AUTO: three"}
	if got := autoChanges(prev, []Bead{b}); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := autoChanges(digestState{At: time.Now(), Auto: map[string]int{"a-1": 3}}, []Bead{b}); got != nil {
		t.Errorf("nothing new: %q", got)
	}
}

func TestReportAuto(t *testing.T) {
	since := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	active := []Bead{
		{ID: "a-1", Title: "Roll out", Status: StatusInProgress, UpdatedAt: since.Add(time.Hour), Notes: "AUTO: applied stack x — 2 update, gate ok"},
		{ID: "a-2", Title: "Old", Status: StatusInProgress, UpdatedAt: since.Add(-time.Hour), Notes: "AUTO: earlier"},
		{ID: "a-3", Title: "Supervised", Status: StatusInProgress, UpdatedAt: since.Add(time.Hour)},
	}
	r := buildReport(since, nil, active, nil, TickerState{}, "tj/")
	if len(r.Auto) != 1 || r.Auto[0].Bead.ID != "a-1" {
		t.Fatalf("auto: %+v", r.Auto)
	}
	out := renderReport(r, since.Add(2*time.Hour))
	if !strings.Contains(out, "## Done without you (1)") || !strings.Contains(out, "  - AUTO: applied stack x — 2 update, gate ok") {
		t.Errorf("report:\n%s", out)
	}
	if out := renderReport(buildReport(since, nil, active[2:], nil, TickerState{}, "tj/"), since); strings.Contains(out, "Done without you") {
		t.Error("empty section rendered")
	}
}
