package main

import (
	"fmt"
	"os"
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
	if err := autonomyCmd(cfg, autonomyEnv{}, []string{"off"}, "", &out); err != nil {
		t.Fatal(err)
	}
	if !autonomyStopped() {
		t.Fatal("not stopped after off")
	}
	if err := autonomyCmd(cfg, autonomyEnv{}, []string{"on"}, "", &out); err != nil {
		t.Fatal(err)
	}
	if autonomyStopped() {
		t.Fatal("stopped after on")
	}
	if err := autonomyCmd(cfg, autonomyEnv{}, []string{"on"}, "", &out); err != nil {
		t.Fatalf("on twice: %v", err)
	}
	if err := autonomyCmd(cfg, autonomyEnv{}, []string{"check", "x"}, "deploy", &out); err == nil || !strings.Contains(err.Error(), "--action") {
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

// fakeAutonomy is bd and herdr for setAutonomy: one bead, its agent, and a
// log of every write.
type fakeAutonomy struct {
	bead  Bead
	agent *Agent
	calls []string
}

func (f *fakeAutonomy) env() autonomyEnv {
	return autonomyEnv{
		show: func(string) (Bead, error) { return f.bead, nil },
		setLabel: func(id, l string, on bool) error {
			f.calls = append(f.calls, fmt.Sprintf("label %s %s %v", id, l, on))
			return nil
		},
		note: func(id, text string) error {
			f.calls = append(f.calls, "note "+id+" "+text)
			return nil
		},
		agent: func(host, name string) (*Agent, error) { return f.agent, nil },
		prompt: func(host, pane, text string) error {
			f.calls = append(f.calls, "prompt "+host+" "+pane+" "+strings.SplitN(text, "]", 2)[0]+"]")
			return nil
		},
	}
}

func queuedNotice(t *testing.T, host, agent string) string {
	t.Helper()
	b, err := os.ReadFile(noticePath(host, agent))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestGrantTellsIdleWorker(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeAutonomy{
		bead:  Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{LabelSupervised}},
		agent: &Agent{Name: "b-1", PaneID: "w1:p1", Status: "idle"},
	}
	msg, err := setAutonomy(defaultConfig(), f.env(), "b-1", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"label b-1 supervised false",
		"label b-1 autonomous true",
		"note b-1 AUTO: autonomy granted by TJ (`quartermaster autonomy grant`) mid-flight",
		"prompt local w1:p1 " + grantTag,
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls\n%q\nwant\n%q", f.calls, want)
	}
	if msg != "autonomy granted on b-1; told b-1" {
		t.Fatalf("message %q", msg)
	}
	if q := queuedNotice(t, localHost, "b-1"); q != "" {
		t.Fatalf("queued a notice it delivered: %q", q)
	}
}

func TestGrantQueuesForBusyWorkerAndRevokeReplacesIt(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeAutonomy{
		bead:  Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"host:server"}},
		agent: &Agent{Name: "b-1", PaneID: "w1:p1", Status: "working"},
	}
	msg, err := setAutonomy(defaultConfig(), f.env(), "b-1", true)
	if err != nil || !strings.Contains(msg, "b-1 is working; queued until it is idle") {
		t.Fatalf("%q, %v", msg, err)
	}
	if !strings.HasPrefix(queuedNotice(t, "server", "b-1"), grantTag) {
		t.Fatal("grant not queued on the bead's host")
	}
	f.bead.Labels = append(f.bead.Labels, LabelAutonomous)
	f.agent.Status = "blocked"
	if _, err := setAutonomy(defaultConfig(), f.env(), "b-1", false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(queuedNotice(t, "server", "b-1"), revokeTag) {
		t.Fatal("revoke did not replace the queued grant")
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "prompt") }) {
		t.Fatalf("prompted a busy worker: %q", f.calls)
	}
	if !slices.Contains(f.calls, "label b-1 autonomous false") || !slices.Contains(f.calls, "label b-1 supervised true") {
		t.Fatalf("labels: %q", f.calls)
	}
}

func TestRevokeWaitsOutNeedsMe(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeAutonomy{
		bead:  Bead{ID: "b-1", Status: StatusNeedsMe, Labels: []string{LabelAutonomous}},
		agent: &Agent{Name: "b-1", PaneID: "w1:p1", Status: "idle"},
	}
	msg, err := setAutonomy(defaultConfig(), f.env(), "b-1", false)
	if err != nil || !strings.Contains(msg, "queued until it is answered") {
		t.Fatalf("%q, %v", msg, err)
	}
	// A grant to the same idle needs_me worker goes through: it lets the
	// worker take its recommendation.
	if _, err := setAutonomy(defaultConfig(), f.env(), "b-1", true); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "prompt local w1:p1 "+grantTag) {
		t.Fatalf("grant not delivered: %q", f.calls)
	}
}

func TestGrantOpenBeadLeavesItToTheBrief(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeAutonomy{bead: Bead{ID: "b-1", Status: StatusOpen}}
	cfg := defaultConfig()
	cfg.Autonomy.Enabled = false
	msg, err := setAutonomy(cfg, f.env(), "b-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "not autonomous yet: [autonomy] enabled = false") || !strings.Contains(msg, "its brief will carry it") {
		t.Fatalf("message %q", msg)
	}
	if queuedNotice(t, localHost, "b-1") != "" {
		t.Fatal("queued a notice for a bead with no worker")
	}
	if _, err := setAutonomy(cfg, autonomyEnv{show: func(string) (Bead, error) { return Bead{ID: "b-1", Status: StatusClosed}, nil }}, "b-1", true); err == nil {
		t.Fatal("granted a closed bead")
	}
}

func TestDeliverQueuedNotices(t *testing.T) {
	dir := fakeTools(t)
	if err := queueBrief("b-1", "brief"); err != nil {
		t.Fatal(err)
	}
	for id, text := range map[string]string{"b-1": grantNotice("b-1"), "b-2": revokeNotice("b-2")} {
		if err := writeAtomic(noticePath(localHost, id), []byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	agents := map[string]Agent{
		"b-1": {Name: "b-1", PaneID: "w1:p1", Status: "idle"},
		"b-2": {Name: "b-2", PaneID: "w2:p1", Status: "idle"},
	}
	needsMe := map[string]bool{"b-2": true}
	// b-1 gets its brief first and the grant on a later pass; b-2's revoke
	// waits while it is at needs_me.
	if sent := deliverOutbox(hostsConfig(), newHerdr(), agents, needsMe); !slices.Equal(sent, []string{"b-1"}) {
		t.Fatalf("first pass sent %q", sent)
	}
	if sent := deliverOutbox(hostsConfig(), newHerdr(), agents, needsMe); !slices.Equal(sent, []string{"b-1"}) {
		t.Fatalf("second pass sent %q", sent)
	}
	needsMe["b-2"] = false
	if sent := deliverOutbox(hostsConfig(), newHerdr(), agents, needsMe); !slices.Equal(sent, []string{"b-2"}) {
		t.Fatalf("third pass sent %q", sent)
	}
	herdr := logLines(t, dir, "herdr")
	for _, want := range []string{"agent prompt w1:p1 brief", "agent prompt w1:p1 " + grantTag, "agent prompt w2:p1 " + revokeTag} {
		if !hasLine(herdr, want) {
			t.Errorf("no herdr call %q", want)
		}
	}
}

func TestBriefNamesMidFlightGrant(t *testing.T) {
	b := brief(Bead{ID: "b-1", Title: "T"}, "br", "/p", "", "", false)
	if !strings.Contains(b, grantTag) || !strings.Contains(b, "`quartermaster autonomy check b-1`") {
		t.Fatalf("brief does not explain a mid-flight grant:\n%s", b)
	}
}
