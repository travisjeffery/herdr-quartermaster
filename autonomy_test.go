package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestInstallsSelf(t *testing.T) {
	cfg := defaultConfig()
	cfg.Repo = "~/src/oscilar/backend"
	cfg.Repos = map[string]string{"herdr-quartermaster": "~/src/herdr-quartermaster/", "infra": "~/src/oscilar/infra"}
	cases := []struct {
		labels []string
		want   bool
	}{
		{[]string{"repo:herdr-quartermaster"}, true},
		{[]string{"repo:infra"}, false},
		{nil, false},
		{[]string{"repo:unknown"}, false},
	}
	for _, c := range cases {
		if got := cfg.installsSelf(Bead{Labels: c.labels}); got != c.want {
			t.Errorf("%v: got %v, want %v", c.labels, got, c.want)
		}
	}
	cfg.Repo = "/home/x/herdr-quartermaster"
	if !cfg.installsSelf(Bead{}) {
		t.Error("default repo herdr-quartermaster does not install")
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
		"quartermaster autonomy check backend-ab12 --action install",
		"only for a bead in herdr-quartermaster",
		"quartermaster ticker stop && quartermaster ticker start",
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

func TestStateLineLevel(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("working", 1), nil)
	th.Level = "L4"
	if got := stateLine(th); !strings.HasSuffix(got, " · L4") {
		t.Errorf("state line %q has no level marker", got)
	}
}

func TestLevelMark(t *testing.T) {
	now := time.Now()
	a := AutonomyConfig{Level: 4, Max: maxStanding}
	own := Bead{ID: "a-1", Labels: []string{"autonomy:8"}}
	leased := autonomyState{Leases: map[string]autonomyLease{"a-1": {Level: 8, Until: now.Add(3*time.Hour + time.Minute)}}}
	for _, c := range []struct {
		name string
		s    autonomyState
		b    Bead
		stop bool
		want string
	}{
		{"global", autonomyState{}, Bead{ID: "a-2"}, false, "L4"},
		{"own", autonomyState{}, Bead{ID: "a-2", Labels: []string{LabelSupervised}}, false, "L2"},
		{"lease", leased, own, false, "L8 3h left"},
		{"no lease is capped at max", autonomyState{}, own, false, "L7"},
		{"expired lease", autonomyState{Leases: map[string]autonomyLease{"a-1": {Level: 8, Until: now.Add(-time.Minute)}}}, own, false, "L7"},
		{"stopped", leased, own, true, "L2"},
		{"global lease", autonomyState{Level: new(8), Leases: map[string]autonomyLease{"": {Level: 8, Until: now.Add(90 * time.Minute)}}}, Bead{ID: "a-2"}, false, "L8 1h left"},
	} {
		if got := a.levelMark(c.s, c.b, c.stop, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
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
			if f.bead.Labels = without(f.bead.Labels, l); on {
				f.bead.Labels = append(f.bead.Labels, l)
			}
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

func ptr[T any](v T) *T { return &v }

func TestLevel(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	a := defaultConfig().Autonomy
	b := func(labels ...string) Bead { return Bead{ID: "b-1", Labels: labels} }
	lease := func(key string, n int, left time.Duration) autonomyState {
		return autonomyState{Leases: map[string]autonomyLease{key: {Level: n, Until: now.Add(left)}}}
	}
	cases := []struct {
		name    string
		s       autonomyState
		b       Bead
		stopped bool
		want    int
		why     string
	}{
		{"global from config", autonomyState{}, b(), false, 4, ""},
		{"global from state", autonomyState{Level: ptr(1)}, b(), false, 1, ""},
		{"own above global", autonomyState{}, b("autonomy:6"), false, 6, ""},
		{"own below global", autonomyState{}, b("autonomy:0"), false, 0, ""},
		{"lowest of several", autonomyState{}, b("autonomy:6", "autonomy:3"), false, 3, ""},
		{"bad label ignored", autonomyState{}, b("autonomy:x", "autonomy:12"), false, 4, ""},
		{"autonomous reads as 7", autonomyState{}, b(LabelAutonomous), false, 7, ""},
		{"supervised reads as 2", autonomyState{}, b(LabelSupervised, LabelAutonomous), false, 2, ""},
		{"level label beats legacy", autonomyState{}, b(LabelAutonomous, "autonomy:5"), false, 5, ""},
		{"8 without lease capped at max", autonomyState{}, b("autonomy:8"), false, 7, "max = 7"},
		{"8 under lease", lease("b-1", 8, time.Hour), b("autonomy:8"), false, 8, ""},
		{"9 under an 8 lease", lease("b-1", 8, time.Hour), b("autonomy:9"), false, 8, "its lease"},
		{"expired lease", lease("b-1", 9, -time.Second), b("autonomy:9"), false, 7, "max = 7"},
		{"another bead's lease", lease("b-2", 9, time.Hour), b("autonomy:9"), false, 7, "max"},
		{"global lease", autonomyState{Level: ptr(8), Leases: lease("", 8, time.Hour).Leases}, b(), false, 8, ""},
		{"global lease doesn't lift an own level", lease("", 8, time.Hour), b("autonomy:8"), false, 7, "max"},
		{"stop caps", autonomyState{}, b("autonomy:6"), true, 2, "autonomy is off"},
		{"stop caps a lease", lease("b-1", 9, time.Hour), b("autonomy:9"), true, 2, "autonomy is off"},
		{"stop leaves lower alone", autonomyState{}, b("autonomy:1"), true, 1, ""},
	}
	for _, c := range cases {
		got, why := a.level(c.s, c.b, c.stopped, now)
		if got != c.want || !strings.Contains(why, c.why) || (c.why == "" && why != "") {
			t.Errorf("%s: got %d %q, want %d %q", c.name, got, why, c.want, c.why)
		}
	}
	low := a
	low.Max = 3
	if got, why := low.level(autonomyState{}, b("autonomy:5"), false, now); got != 3 || !strings.Contains(why, "max = 3") {
		t.Errorf("max 3: %d %q", got, why)
	}
}

func TestMigrateAutonomy(t *testing.T) {
	cases := []struct {
		name     string
		a        AutonomyConfig
		levelSet bool
		level    int
		max      int
	}{
		{"defaults", AutonomyConfig{Level: 4, Max: 7}, false, 4, 7},
		{"default = true", AutonomyConfig{Level: 4, Max: 7, Default: ptr(true)}, false, 7, 7},
		{"default = true, level set", AutonomyConfig{Level: 3, Max: 7, Default: ptr(true)}, true, 3, 7},
		{"enabled = false", AutonomyConfig{Level: 4, Max: 7, Enabled: ptr(false)}, false, 4, 2},
		{"merge = false", AutonomyConfig{Level: 4, Max: 7, Merge: ptr(false)}, false, 4, 3},
		{"apply = false", AutonomyConfig{Level: 4, Max: 7, Apply: ptr(false)}, false, 4, 5},
		{"install = false", AutonomyConfig{Level: 4, Max: 7, Install: ptr(false)}, false, 4, 2},
		{"decide = false", AutonomyConfig{Level: 4, Max: 7, Decide: ptr(false)}, false, 4, 1},
		{"switches on", AutonomyConfig{Level: 4, Max: 7, Enabled: ptr(true), Merge: ptr(true), Apply: ptr(true)}, false, 4, 7},
		{"lowest wins", AutonomyConfig{Level: 4, Max: 7, Apply: ptr(false), Merge: ptr(false)}, false, 4, 3},
	}
	for _, c := range cases {
		c.a.migrate(c.levelSet)
		if c.a.Level != c.level || c.a.Max != c.max {
			t.Errorf("%s: level %d max %d, want %d %d", c.name, c.a.Level, c.a.Max, c.level, c.max)
		}
	}
	for _, bad := range []AutonomyConfig{{Level: 8, Max: 7}, {Level: 4, Max: 9}, {Level: -1, Max: 7}, {Level: 4, Max: 7, Prod: []string{"["}}} {
		if bad.validate() == nil {
			t.Errorf("%+v validated", bad)
		}
	}
}

func TestLoadConfigMigratesAutonomy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
	t.Setenv("BEADS_DIR", dir)
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("repo = \"/r\"\n"+s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("[autonomy]\nenabled = true\ndefault = true\nmerge = false\n")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Autonomy.Level != 7 || cfg.Autonomy.Max != 3 {
		t.Errorf("level %d max %d, want 7 3", cfg.Autonomy.Level, cfg.Autonomy.Max)
	}
	write("[autonomy]\ndefault = true\nlevel = 2\n")
	if cfg, err = loadConfig(); err != nil || cfg.Autonomy.Level != 2 {
		t.Errorf("level set beside default: %d, %v", cfg.Autonomy.Level, err)
	}
	write("[autonomy]\nlevel = 9\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "leases") {
		t.Errorf("level 9 in config: %v", err)
	}
	write("")
	if cfg, err = loadConfig(); err != nil || cfg.Autonomy.Level != 4 || cfg.Autonomy.Max != 7 {
		t.Errorf("no [autonomy]: %+v, %v", cfg.Autonomy, err)
	}
}

func TestActionClass(t *testing.T) {
	cfg := defaultConfig()
	cfg.Repo = "/src/app"
	cfg.Repos = map[string]string{"infra": "/src/infra", "herdr-quartermaster": "/src/herdr-quartermaster", "docs": "/src/docs"}
	cfg.Autonomy.Prod = []string{"*prod*", "*-production"}
	cfg.Autonomy.MergeClass = map[string]int{"infra": 7, "docs": 1}
	app, infra, self, docs := Bead{}, labelled("b", "repo:infra"), labelled("b", "repo:herdr-quartermaster"), labelled("b", "repo:docs")
	cases := []struct {
		b      Bead
		action string
		target string
		want   int
	}{
		{app, "push", "", 2},
		{app, "dispatch", "", 3},
		{app, "resolve", "", 3},
		{self, "install", "", 3},
		{app, "merge", "", 4},
		{infra, "merge", "", 7},
		{docs, "merge", "", 4}, // merge_class can't lower a merge
		{app, "decide", "", 4},
		{app, "post", "", 5},
		{app, "apply", "acme/app/staging", 6},
		{app, "deploy", "dev-east", 6},
		{app, "apply", "acme/app/prod", 7},
		{app, "rollout", "api-production", 7},
		{app, "apply", "", 7},
		{app, "delete", "", 8},
		{app, "secret", "", 8},
		{app, "message-external", "", 8},
		{app, "spend", "", 8},
		{app, "approve", "", 8},
		{app, "classifier", "", 9},
		{app, "loosen-settings", "", 9},
	}
	for _, c := range cases {
		got, err := cfg.actionClass(c.b, c.action, c.target)
		if err != nil || got != c.want {
			t.Errorf("%s %q on %v: %d, %v; want %d", c.action, c.target, c.b.Labels, got, err, c.want)
		}
	}
	if _, err := cfg.actionClass(app, "install", ""); !errors.Is(err, errNotAutonomous) {
		t.Errorf("install outside %s: %v", selfRepo, err)
	}
	cfg.Autonomy.Prod = nil
	if got := cfg.Autonomy.targetClass("dev-east"); got != 7 {
		t.Errorf("no prod patterns: dev-east is %d, want 7", got)
	}
}

func TestAutonomyCheck(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	f := &fakeAutonomy{bead: Bead{ID: "b-1", Status: StatusInProgress}}
	check := func(o autonomyOpts) error {
		var out strings.Builder
		return autonomyCmd(cfg, f.env(), []string{"check", "b-1"}, o, &out)
	}
	none := autonomyOpts{Class: -1}
	cls := func(n int) autonomyOpts { return autonomyOpts{Class: n} }
	act := func(a string) autonomyOpts { return autonomyOpts{Class: -1, Action: a} }
	// Global level 4: class 4 passes, 5 doesn't.
	if err := check(cls(4)); err != nil {
		t.Errorf("class 4 at level 4: %v", err)
	}
	if err := check(cls(5)); !errors.Is(err, errNotAutonomous) || !strings.Contains(err.Error(), "level 4") {
		t.Errorf("class 5 at level 4: %v", err)
	}
	if err := check(act("merge")); err != nil {
		t.Errorf("merge at level 4: %v", err)
	}
	if err := check(act("post")); err == nil {
		t.Error("post passed at level 4")
	}
	// A bare check asks for the old autonomous level.
	if err := check(none); err == nil {
		t.Error("bare check passed at level 4")
	}
	f.bead.Labels = []string{LabelAutonomous}
	if err := check(none); err != nil {
		t.Errorf("bare check on autonomous: %v", err)
	}
	if err := check(act("apply")); err != nil {
		t.Errorf("apply on autonomous: %v", err)
	}
	if err := check(act("delete")); err == nil {
		t.Error("delete passed at 7")
	}
	var out strings.Builder
	if err := autonomyCmd(cfg, f.env(), []string{"off"}, none, &out); err != nil {
		t.Fatal(err)
	}
	if err := check(act("dispatch")); err == nil || !strings.Contains(err.Error(), "autonomy is off") {
		t.Errorf("dispatch while off: %v", err)
	}
	if err := check(act("push")); err != nil {
		t.Errorf("push while off: %v", err)
	}
	if err := autonomyCmd(cfg, f.env(), []string{"on"}, none, &out); err != nil {
		t.Fatal(err)
	}
	f.bead.Status = StatusClosed
	if err := check(cls(0)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("closed: %v", err)
	}
	for _, o := range []autonomyOpts{act("deploy-everything"), {Class: 3, Action: "merge"}, cls(10)} {
		if err := check(o); err == nil || errors.Is(err, errNotAutonomous) {
			t.Errorf("%+v: %v, want a usage error", o, err)
		}
	}
}

func TestLeaseFor(t *testing.T) {
	cases := []struct {
		n    int
		d    time.Duration
		want time.Duration
		err  bool
	}{
		{8, 0, leaseDefault, false},
		{8, time.Hour, time.Hour, false},
		{9, 0, 0, true},
		{9, time.Hour, time.Hour, false},
		{7, 0, 0, false},
		{7, time.Hour, 0, true},
		{-1, time.Hour, 0, true},
		{8, -time.Hour, 0, true},
	}
	for _, c := range cases {
		got, err := leaseFor(c.n, c.d)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("leaseFor(%d, %v) = %v, %v", c.n, c.d, got, err)
		}
	}
}

func TestGlobalLevel(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if _, err := setGlobalLevel(cfg, 9, time.Hour, now); err == nil {
		t.Fatal("set 9 globally")
	}
	if msg, err := setGlobalLevel(cfg, 5, 0, now); err != nil || !strings.Contains(msg, "level 5") || !strings.Contains(msg, "advisory") {
		t.Fatalf("%q, %v", msg, err)
	}
	if msg, err := setGlobalLevel(cfg, 8, 0, now); err != nil || !strings.Contains(msg, "level 8 until") {
		t.Fatalf("%q, %v", msg, err)
	}
	s, _ := loadAutonomy()
	if l := s.Leases[""]; l.Level != 8 || !l.Until.Equal(now.Add(leaseDefault)) || l.Prev == nil || *l.Prev != 5 {
		t.Fatalf("global lease %+v", l)
	}
	if got, _ := cfg.Autonomy.level(s, Bead{ID: "b"}, false, now); got != 8 {
		t.Fatalf("bead on the global level: %d", got)
	}
	events, err := expireLeases(cfg, autonomyEnv{}, now.Add(leaseDefault))
	if err != nil || len(events) != 1 || events[0].Kind != EventLeaseExpired {
		t.Fatalf("%+v, %v", events, err)
	}
	if s, _ = loadAutonomy(); s.Level == nil || *s.Level != 5 || len(s.Leases) != 0 {
		t.Fatalf("after expiry: %+v", s)
	}
	if _, err := setGlobalLevel(cfg, -1, 0, now); err != nil {
		t.Fatal(err)
	}
	if s, _ = loadAutonomy(); s.Level != nil {
		t.Fatalf("- left %d", *s.Level)
	}
}

func TestGrantTellsIdleWorker(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeAutonomy{
		bead:  Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"repo:x", LabelSupervised}},
		agent: &Agent{Name: "b-1", PaneID: "w1:p1", Status: "idle"},
	}
	var out strings.Builder
	if err := autonomyCmd(defaultConfig(), f.env(), []string{"grant", "b-1"}, autonomyOpts{Class: -1}, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"label b-1 supervised false",
		"label b-1 autonomy:7 true",
		"note b-1 autonomy level 7 set by TJ",
		"prompt local w1:p1 [quartermaster autonomy: level 7, set by TJ]",
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls\n%q\nwant\n%q", f.calls, want)
	}
	if got := out.String(); got != "b-1 autonomy level 7; told b-1\n" {
		t.Fatalf("message %q", got)
	}
	if q := queuedNotice(t, localHost, "b-1"); q != "" {
		t.Fatalf("queued a notice it delivered: %q", q)
	}
}

func TestLevelQueuesForBusyWorkerAndALowerOneReplacesIt(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now()
	f := &fakeAutonomy{
		bead:  Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"host:server"}},
		agent: &Agent{Name: "b-1", PaneID: "w1:p1", Status: "working"},
	}
	msg, err := setLevel(defaultConfig(), f.env(), "b-1", 7, 0, "TJ", now)
	if err != nil || !strings.Contains(msg, "b-1 is working; queued until it is idle") {
		t.Fatalf("%q, %v", msg, err)
	}
	if !strings.HasPrefix(queuedNotice(t, "server", "b-1"), levelTag+"7") {
		t.Fatal("level not queued on the bead's host")
	}
	f.agent.Status = "blocked"
	if _, err := setLevel(defaultConfig(), f.env(), "b-1", levelSupervised, 0, "TJ", now); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(queuedNotice(t, "server", "b-1"), levelTag+"2") {
		t.Fatal("the lower level did not replace the queued one")
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "prompt") }) {
		t.Fatalf("prompted a busy worker: %q", f.calls)
	}
	if !slices.Equal(f.bead.Labels, []string{"host:server", "autonomy:2"}) {
		t.Fatalf("labels: %q", f.bead.Labels)
	}
	if _, err := setLevel(defaultConfig(), f.env(), "b-1", -1, 0, "TJ", now); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.bead.Labels, []string{"host:server"}) {
		t.Fatalf("- left labels %q", f.bead.Labels)
	}
}

func TestLowerLevelWaitsOutNeedsMe(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeAutonomy{
		bead:  Bead{ID: "b-1", Status: StatusNeedsMe, Labels: []string{LabelAutonomous}},
		agent: &Agent{Name: "b-1", PaneID: "w1:p1", Status: "idle"},
	}
	msg, err := setLevel(defaultConfig(), f.env(), "b-1", 4, 0, "TJ", time.Now())
	if err != nil || !strings.Contains(msg, "queued until it is answered") {
		t.Fatalf("%q, %v", msg, err)
	}
	// A grant to the same idle needs_me worker goes through: it lets the
	// worker take its recommendation.
	if _, err := setLevel(defaultConfig(), f.env(), "b-1", 7, 0, "TJ", time.Now()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "prompt local w1:p1 [quartermaster autonomy: level 7, set by TJ]") {
		t.Fatalf("grant not delivered: %q", f.calls)
	}
}

func TestLevelOnOpenBeadLeavesItToTheBrief(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	f := &fakeAutonomy{bead: Bead{ID: "b-1", Status: StatusOpen}}
	cfg := defaultConfig()
	cfg.Autonomy.Max = 2
	msg, err := setLevel(cfg, f.env(), "b-1", 7, 0, "TJ", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "level 2 (capped by [autonomy] max = 2)") || !strings.Contains(msg, "its brief will carry it") {
		t.Fatalf("message %q", msg)
	}
	if queuedNotice(t, localHost, "b-1") != "" {
		t.Fatal("queued a notice for a bead with no worker")
	}
	if _, err := setLevel(cfg, autonomyEnv{show: func(string) (Bead, error) { return Bead{ID: "b-1", Status: StatusClosed}, nil }}, "b-1", 7, 0, "TJ", time.Now()); err == nil {
		t.Fatal("set a closed bead")
	}
}

func TestBeadLeaseExpires(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	f := &fakeAutonomy{bead: Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:5"}}}
	if _, err := setLevel(cfg, f.env(), "b-1", 9, 0, "TJ", now); err == nil {
		t.Fatal("9 without --for")
	}
	if _, err := setLevel(cfg, f.env(), "b-1", 9, time.Hour, "TJ", now); err != nil {
		t.Fatal(err)
	}
	// Re-leasing keeps the level the first lease would go back to.
	if _, err := setLevel(cfg, f.env(), "b-1", 8, 2*time.Hour, "TJ", now); err != nil {
		t.Fatal(err)
	}
	s, _ := loadAutonomy()
	if got, _ := cfg.Autonomy.level(s, f.bead, false, now.Add(time.Hour)); got != 8 {
		t.Fatalf("under lease: %d", got)
	}
	if got, _ := cfg.Autonomy.level(s, f.bead, false, now.Add(3*time.Hour)); got != 7 {
		t.Fatalf("lapsed, before the ticker: %d", got)
	}
	if events, err := expireLeases(cfg, f.env(), now.Add(time.Hour)); err != nil || len(events) != 0 {
		t.Fatalf("expired early: %+v, %v", events, err)
	}
	events, err := expireLeases(cfg, f.env(), now.Add(2*time.Hour))
	if err != nil || len(events) != 1 || events[0].Bead != "b-1" || !strings.Contains(events[0].Summary, "lease at 8 expired") {
		t.Fatalf("%+v, %v", events, err)
	}
	if !slices.Equal(f.bead.Labels, []string{"autonomy:5"}) {
		t.Fatalf("labels after expiry: %q", f.bead.Labels)
	}
	if !slices.Contains(f.calls, "note b-1 autonomy level 5 set by the ticker (lease expired)") {
		t.Fatalf("calls %q", f.calls)
	}
	if s, _ = loadAutonomy(); len(s.Leases) != 0 {
		t.Fatalf("leases left: %+v", s.Leases)
	}
	// A closed bead's lapsed lease is dropped.
	if _, err := setLevel(cfg, f.env(), "b-1", 8, time.Hour, "TJ", now); err != nil {
		t.Fatal(err)
	}
	f.bead.Status = StatusClosed
	if events, err = expireLeases(cfg, f.env(), now.Add(2*time.Hour)); err != nil || len(events) != 1 || !strings.Contains(events[0].Summary, "lease dropped") {
		t.Fatalf("%+v, %v", events, err)
	}
	if s, _ = loadAutonomy(); len(s.Leases) != 0 {
		t.Fatalf("closed bead's lease left: %+v", s.Leases)
	}
}

func TestAutonomyStatus(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	a := defaultConfig().Autonomy
	s := autonomyState{Leases: map[string]autonomyLease{"a-1": {Level: 9, Until: now.Add(time.Hour), Prev: ptr(5)}}}
	active := []Bead{{ID: "a-1", Labels: []string{"autonomy:9"}}, {ID: "a-2"}, {ID: "a-3", Labels: []string{LabelSupervised}}}
	got := autonomyStatus(a, s, false, active, now)
	for _, want := range []string{"level: 4 (global, from [autonomy] level), max 7\n", "advisory", "lease: a-1 at 9 until", "then 5\n", "a-1 9 (own)\n", "a-2 4\n", "a-3 2 (own)\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}
	got = autonomyStatus(a, s, true, active, now)
	if !strings.Contains(got, "level: 2 (global, from [autonomy] level; capped by autonomy is off") || !strings.Contains(got, "a-1 2 (own) (capped by autonomy is off") {
		t.Errorf("stopped status:\n%s", got)
	}
}

func TestDeliverQueuedNotices(t *testing.T) {
	dir := fakeTools(t)
	if err := queueBrief("b-1", "brief"); err != nil {
		t.Fatal(err)
	}
	for id, text := range map[string]string{"b-1": levelNotice("b-1", 7, "TJ"), "b-2": levelNotice("b-2", 2, "TJ")} {
		if err := writeAtomic(noticePath(localHost, id), []byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	agents := map[string]Agent{
		"b-1": {Name: "b-1", PaneID: "w1:p1", Status: "idle"},
		"b-2": {Name: "b-2", PaneID: "w2:p1", Status: "idle"},
	}
	needsMe := map[string]bool{"b-2": true}
	// b-1 gets its brief first and the grant on a later pass; b-2's lower
	// level waits while it is at needs_me.
	if sent := deliverOutbox(hostsConfig(), newHerdr(), agents, needsMe, nil); !slices.Equal(sent, []string{"b-1"}) {
		t.Fatalf("first pass sent %q", sent)
	}
	if sent := deliverOutbox(hostsConfig(), newHerdr(), agents, needsMe, nil); !slices.Equal(sent, []string{"b-1"}) {
		t.Fatalf("second pass sent %q", sent)
	}
	needsMe["b-2"] = false
	if sent := deliverOutbox(hostsConfig(), newHerdr(), agents, needsMe, nil); !slices.Equal(sent, []string{"b-2"}) {
		t.Fatalf("third pass sent %q", sent)
	}
	herdr := logLines(t, dir, "herdr")
	for _, want := range []string{"agent prompt w1:p1 brief", "agent prompt w1:p1 " + levelTag + "7", "agent prompt w2:p1 " + levelTag + "2"} {
		if !hasLine(herdr, want) {
			t.Errorf("no herdr call %q", want)
		}
	}
}

func TestHeldNotice(t *testing.T) {
	for _, c := range []struct {
		text    string
		needsMe bool
		want    bool
	}{
		{levelNotice("b", 7, "TJ"), true, false},
		{levelNotice("b", 9, "TJ"), true, false},
		{levelNotice("b", 6, "TJ"), true, true},
		{levelNotice("b", 2, "TJ"), false, false},
		{"[quartermaster autonomy: granted by TJ] old", true, true},
	} {
		if got := heldNotice(c.text, c.needsMe); got != c.want {
			t.Errorf("%.40q needs_me=%v: %v", c.text, c.needsMe, got)
		}
	}
}

func TestBriefNamesMidFlightLevel(t *testing.T) {
	b := brief(Bead{ID: "b-1", Title: "T"}, "br", "/p", "", "", false)
	if !strings.Contains(b, "[quartermaster autonomy: level N, set by TJ]") || !strings.Contains(b, "`quartermaster autonomy check b-1`") {
		t.Fatalf("brief does not explain a mid-flight level change:\n%s", b)
	}
}
