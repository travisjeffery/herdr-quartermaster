package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

var vNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// x0w5 is backend-x0w5 as it stood open: a P1 bug fixed in backend#14190
// (INF-994) two weeks before anyone noticed.
func x0w5() Bead {
	return Bead{
		ID:     "backend-x0w5",
		Title:  "aws-security-lake preview fails: GHA role denied organizations:DescribeOrganization",
		Status: StatusOpen,
		Type:   "bug",
		Description: "Every PR preview fails on the aws-security-lake stack:\n\n" +
			"  invoking aws:organizations/getOrganization: operation error Organizations: DescribeOrganization\n\n" +
			"The getOrganization lookup in securitylake.go feeds an EventBridge rule's eventPattern.",
		CreatedAt: time.Date(2026, 9, 17, 19, 25, 15, 0, time.UTC),
		UpdatedAt: time.Date(2026, 9, 17, 19, 25, 15, 0, time.UTC),
	}
}

const pr14190 = "https://github.com/Oscilar/backend/pull/14190"

type fakeVerify struct {
	beads   []Bead
	closedB []Bead
	left    int
	issues  map[string]linearIssue
	linErr  error
	prs     map[string]prRef
	found   []prRef // every bead's search returns these
	merged  []prRef // every repo's search for newly merged PRs returns these
	head    string
	// code is what each rev holds, for grep.
	code   map[string]string
	runs   map[string]Run
	later  *Run
	agents []Agent
	// reshow, if set, is what show returns once the pass has listed the beads.
	reshow  func(Bead) Bead
	shown   int
	calls   map[string]int
	closedN []string
	notes   []string
	events  []Event
}

func newFake(beads ...Bead) *fakeVerify {
	return &fakeVerify{beads: beads, left: 5000, head: "main2", prs: map[string]prRef{}, issues: map[string]linearIssue{},
		code: map[string]string{"then": "", "main2": ""}, runs: map[string]Run{}, calls: map[string]int{}}
}

func (f *fakeVerify) env() verifyEnv {
	return verifyEnv{
		beads:  func() ([]Bead, error) { return f.beads, nil },
		closed: func() ([]Bead, error) { return f.closedB, nil },
		show: func(id string) (Bead, error) {
			f.calls["show"]++
			for _, b := range f.beads {
				if b.ID == id {
					f.shown++
					if f.reshow != nil && f.shown > 1 {
						return f.reshow(b), nil
					}
					return b, nil
				}
			}
			return Bead{}, errors.New("no bead " + id)
		},
		agents:      func() ([]Agent, error) { return f.agents, nil },
		graphqlLeft: func() (int, error) { return f.left, nil },
		linear: func(keys []string) (map[string]linearIssue, error) {
			f.calls["linear"]++
			return f.issues, f.linErr
		},
		github: func(urls []string, searches map[string]string) (map[string]prRef, map[string][]prRef, error) {
			f.calls["github"]++
			for a := range searches {
				if strings.HasPrefix(a, "r") {
					f.calls["repoSearch"]++
				} else {
					f.calls["search"]++
				}
			}
			prs := map[string]prRef{}
			for _, u := range urls {
				if pr, ok := f.prs[u]; ok {
					prs[u] = pr
				}
			}
			found := map[string][]prRef{}
			for a := range searches {
				if strings.HasPrefix(a, "r") {
					found[a] = f.merged
				} else {
					found[a] = f.found
				}
			}
			return prs, found, nil
		},
		slug: func(string) string { return "Oscilar/backend" },
		head: func(string) (string, error) { return f.head, nil },
		revAt: func(string, time.Time) (string, error) {
			return "then", nil
		},
		grep: func(repo, rev, needle string, paths []string) (bool, error) {
			f.calls["grep"]++
			return strings.Contains(f.code[rev], needle), nil
		},
		runView: func(url string) (Run, error) {
			f.calls["runView"]++
			return f.runs[url], nil
		},
		laterPass: func(r Run) (Run, bool, error) {
			f.calls["laterPass"]++
			if f.later != nil {
				return *f.later, true, nil
			}
			return Run{}, false, nil
		},
		close: func(id, reason string) error {
			f.closedN = append(f.closedN, id+": "+reason)
			return nil
		},
		note: func(id, text string) error {
			f.notes = append(f.notes, id+": "+text)
			return nil
		},
		event: func(e Event) error {
			f.events = append(f.events, e)
			return nil
		},
	}
}

// vcfg is the default config, with the global level just below the one that
// auto-closes unless autoClose.
func vcfg(autoClose bool) Config {
	c := defaultConfig()
	c.Autonomy.Level = classHousekeeping
	if !autoClose {
		c.Autonomy.Level--
	}
	return c
}

func pass(t *testing.T, f *fakeVerify, cfg Config, st *TickerState, now time.Time) []string {
	t.Helper()
	lines, err := verifyPass(cfg, f.env(), st, nil, true, now)
	if err != nil {
		t.Fatalf("verifyPass: %v", err)
	}
	return lines
}

func withNote(b Bead, note string) Bead {
	b.Notes = note
	return b
}

func TestVerifyLinearDoneClosesWithAutoClose(t *testing.T) {
	f := newFake(withNote(x0w5(), "Linear: INF-994"))
	f.issues["INF-994"] = linearIssue{Key: "INF-994", URL: "https://linear.app/oscilar/issue/INF-994", State: "completed", Name: "Done", Updated: "u1"}
	st := TickerState{}
	pass(t, f, vcfg(true), &st, vNow)
	if len(f.closedN) != 1 || !strings.Contains(f.closedN[0], "INF-994 is Done") {
		t.Fatalf("closed = %v, want backend-x0w5 closed citing INF-994", f.closedN)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventAutoClosed {
		t.Fatalf("events = %+v, want one auto_closed", f.events)
	}
	if len(f.notes) != 1 || !strings.HasPrefix(f.notes[0], "backend-x0w5: AUTO: [verify L3 C3] closed as already done") {
		t.Fatalf("notes = %q, want the close noted AUTO:", f.notes)
	}
}

func TestVerifyFlagOnlyBelowLevel3(t *testing.T) {
	f := newFake(withNote(x0w5(), "Linear: INF-994"))
	f.issues["INF-994"] = linearIssue{Key: "INF-994", State: "completed", Name: "Done"}
	st := TickerState{}
	lines := pass(t, f, vcfg(false), &st, vNow)
	if len(f.closedN) != 0 {
		t.Fatalf("closed %v at level 2", f.closedN)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventLikelyStale || !strings.Contains(f.events[0].Summary, "level is below 3") {
		t.Fatalf("events = %+v, want one likely_stale saying the level is below 3", f.events)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "backend-x0w5 flag") {
		t.Fatalf("lines = %q", lines)
	}
}

func TestVerifyMergedPRs(t *testing.T) {
	cases := []struct {
		name  string
		prs   map[string]prRef
		close bool
	}{
		{"merged", map[string]prRef{pr14190: {URL: pr14190, State: "MERGED"}}, true},
		{"one still open", map[string]prRef{pr14190: {URL: pr14190, State: "MERGED"}, pr14190 + "1": {State: "OPEN"}}, false},
		{"closed unmerged", map[string]prRef{pr14190: {URL: pr14190, State: "CLOSED"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := x0w5()
			b.Type = "task"
			note := "PR: " + pr14190
			if len(c.prs) > 1 {
				note += "\nfollow-up PR: " + pr14190 + "1"
			}
			f := newFake(withNote(b, note))
			f.prs = c.prs
			st := TickerState{}
			pass(t, f, vcfg(true), &st, vNow)
			if got := len(f.closedN) == 1; got != c.close {
				t.Fatalf("closed = %v, want close %v", f.closedN, c.close)
			}
		})
	}
}

func TestVerifyMentionsOnlyFlag(t *testing.T) {
	f := newFake(withNote(x0w5(), "Linear: INF-994"))
	f.found = []prRef{
		{URL: pr14190, State: "MERGED", MergedAt: time.Date(2026, 9, 17, 19, 23, 0, 0, time.UTC).Add(time.Hour)},
		{URL: "https://github.com/Oscilar/backend/pull/1", State: "MERGED", MergedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	st := TickerState{}
	pass(t, f, vcfg(true), &st, vNow)
	if len(f.closedN) != 0 {
		t.Fatalf("closed on a mention alone: %v", f.closedN)
	}
	if len(f.events) != 1 || !strings.Contains(f.events[0].Summary, pr14190) || strings.Contains(f.events[0].Summary, "/pull/1,") {
		t.Fatalf("events = %+v, want a flag citing only #14190", f.events)
	}
}

func TestVerifyCodeGone(t *testing.T) {
	failed := "https://github.com/Oscilar/backend/actions/runs/35263051104"
	setup := func(later bool) *fakeVerify {
		f := newFake(withNote(x0w5(), "failing run: "+failed))
		f.code["then"] = "organizations.getOrganization DescribeOrganization"
		f.code["main2"] = "siemOrganizationID"
		f.runs[failed] = Run{URL: failed, Workflow: "Preview", Conclusion: "failure", CreatedAt: x0w5().CreatedAt}
		if later {
			f.later = &Run{URL: "https://github.com/Oscilar/backend/actions/runs/36000000000", Conclusion: "success"}
		}
		return f
	}
	t.Run("gone but no later pass flags", func(t *testing.T) {
		f := setup(false)
		st := TickerState{}
		pass(t, f, vcfg(true), &st, vNow)
		if len(f.closedN) != 0 || len(f.events) != 1 || !strings.Contains(f.events[0].Summary, "no longer on main") {
			t.Fatalf("closed %v events %+v, want one flag", f.closedN, f.events)
		}
	})
	t.Run("gone and the workflow passed later closes", func(t *testing.T) {
		f := setup(true)
		st := TickerState{}
		pass(t, f, vcfg(true), &st, vNow)
		if len(f.closedN) != 1 || !strings.Contains(f.closedN[0], "Preview passed") {
			t.Fatalf("closed = %v", f.closedN)
		}
	})
	t.Run("still on main is no evidence", func(t *testing.T) {
		f := setup(true)
		f.code["main2"] = "DescribeOrganization"
		st := TickerState{}
		pass(t, f, vcfg(true), &st, vNow)
		if len(f.closedN) != 0 || len(f.events) != 0 {
			t.Fatalf("closed %v events %+v, want nothing", f.closedN, f.events)
		}
	})
}

func TestVerifyDuplicateFlags(t *testing.T) {
	dup := x0w5()
	dup.ID, dup.Status, dup.CloseReason = "backend-aaaa", StatusClosed, "fixed in backend#14190"
	f := newFake(x0w5())
	f.closedB = []Bead{dup}
	st := TickerState{}
	pass(t, f, vcfg(true), &st, vNow)
	if len(f.closedN) != 0 || len(f.events) != 1 || !strings.Contains(f.events[0].Summary, "backend-aaaa closed") {
		t.Fatalf("closed %v events %+v, want a duplicate flag", f.closedN, f.events)
	}
}

func TestVerifyNeverClosesGuardedBeads(t *testing.T) {
	for name, mod := range map[string]func(*Bead){
		"rolling-out": func(b *Bead) { b.Labels = []string{LabelRollingOut} },
		"needs_me":    func(b *Bead) { b.Status = StatusNeedsMe },
		"epic":        func(b *Bead) { b.Type = "epic" },
	} {
		t.Run(name, func(t *testing.T) {
			b := withNote(x0w5(), "Linear: INF-994")
			mod(&b)
			f := newFake(b)
			f.issues["INF-994"] = linearIssue{Key: "INF-994", State: "completed", Name: "Done"}
			st := TickerState{}
			pass(t, f, vcfg(true), &st, vNow)
			if len(f.closedN) != 0 || len(f.events) != 1 || f.events[0].Kind != EventLikelyStale {
				t.Fatalf("closed %v events %+v, want a flag only", f.closedN, f.events)
			}
		})
	}
}

func TestVerifySkipsLiveAndFreshBeads(t *testing.T) {
	live := x0w5()
	fresh := x0w5()
	fresh.ID, fresh.UpdatedAt = "backend-fres", vNow.Add(-time.Hour)
	f := newFake(live, fresh)
	f.agents = []Agent{{Name: "backend-x0w5"}}
	st := TickerState{}
	lines := pass(t, f, vcfg(true), &st, vNow)
	if f.calls["github"] != 0 || len(st.Verified) != 0 || !slices.Equal(lines, []string{"verify: no stale beads"}) {
		t.Fatalf("lines %q calls %v, want nothing checked", lines, f.calls)
	}
}

func TestVerifyIsIncremental(t *testing.T) {
	f := newFake(withNote(x0w5(), "Linear: INF-994\nPR: "+pr14190))
	f.issues["INF-994"] = linearIssue{Key: "INF-994", State: "started", Name: "In Progress", Updated: "u1"}
	f.prs[pr14190] = prRef{URL: pr14190, State: "OPEN", Head: "aaa"}
	cfg := vcfg(true)
	st := TickerState{}
	pass(t, f, cfg, &st, vNow)
	if f.calls["search"] != 1 || f.calls["show"] != 1 {
		t.Fatalf("first pass calls = %v, want one search and one show", f.calls)
	}

	// Nothing changed an hour later: only the probes run.
	f.calls = map[string]int{}
	lines := pass(t, f, cfg, &st, vNow.Add(time.Hour))
	if f.calls["search"] != 0 || f.calls["show"] != 0 || f.calls["grep"] != 0 || f.calls["github"] != 1 {
		t.Fatalf("unchanged pass calls = %v, want only the probe query", f.calls)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "nothing changed") {
		t.Fatalf("lines = %q", lines)
	}

	// The PR's head moved: re-evaluated from the probe, no search.
	f.calls = map[string]int{}
	f.prs[pr14190] = prRef{URL: pr14190, State: "OPEN", Head: "bbb"}
	pass(t, f, cfg, &st, vNow.Add(2*time.Hour))
	if f.calls["search"] != 0 || st.Verified["backend-x0w5"].PRs != pr14190+"=OPEN@bbb" {
		t.Fatalf("PR-moved pass calls = %v, mark %+v", f.calls, st.Verified["backend-x0w5"])
	}

	// main moved: one search for the repo's newly merged PRs, matched
	// locally; the one naming INF-994 becomes evidence.
	f.calls = map[string]int{}
	f.head = "main3"
	f.merged = []prRef{
		{URL: pr14190 + "9", Title: "INF-994: Read the SIEM org id from config", State: "MERGED", MergedAt: vNow},
		{URL: "https://github.com/Oscilar/backend/pull/2", Title: "INF-9940: unrelated", State: "MERGED", MergedAt: vNow},
	}
	pass(t, f, cfg, &st, vNow.Add(3*time.Hour))
	if f.calls["search"] != 0 || f.calls["repoSearch"] != 1 || f.calls["show"] != 0 || f.calls["github"] != 1 {
		t.Fatalf("main-moved pass calls = %v, want one repo search in the probe query", f.calls)
	}
	if refs := st.Verified["backend-x0w5"].Refs; !slices.Equal(refs, []string{pr14190 + "9"}) {
		t.Fatalf("refs = %q, want only #141909", refs)
	}
	f.merged = nil

	// Linear moved to Done: closes, without another search.
	f.calls = map[string]int{}
	f.issues["INF-994"] = linearIssue{Key: "INF-994", State: "completed", Name: "Done", Updated: "u2"}
	pass(t, f, cfg, &st, vNow.Add(4*time.Hour))
	if f.calls["search"] != 0 || len(f.closedN) != 1 {
		t.Fatalf("Linear-moved pass calls = %v closed %v", f.calls, f.closedN)
	}
}

func TestVerifyFullRecheckDaily(t *testing.T) {
	f := newFake(x0w5())
	st := TickerState{}
	pass(t, f, vcfg(false), &st, vNow)
	f.calls = map[string]int{}
	pass(t, f, vcfg(false), &st, vNow.Add(23*time.Hour))
	if f.calls["show"] != 0 {
		t.Fatalf("re-checked in full within a day: %v", f.calls)
	}
	pass(t, f, vcfg(false), &st, vNow.Add(25*time.Hour))
	if f.calls["show"] != 1 || f.calls["search"] != 1 {
		t.Fatalf("no full re-check after a day: %v", f.calls)
	}
}

func TestVerifyDoesNotReflagUnchangedVerdict(t *testing.T) {
	f := newFake(withNote(x0w5(), "Linear: INF-994"))
	f.found = []prRef{{URL: pr14190, State: "MERGED", MergedAt: vNow.Add(-time.Hour * 300)}}
	f.merged = []prRef{{URL: pr14190, Title: "INF-994", State: "MERGED", MergedAt: vNow}}
	st := TickerState{}
	pass(t, f, vcfg(false), &st, vNow)
	f.head = "main3"
	pass(t, f, vcfg(false), &st, vNow.Add(time.Hour))
	pass(t, f, vcfg(false), &st, vNow.Add(25*time.Hour))
	if len(f.events) != 1 {
		t.Fatalf("events = %+v, want the flag written once", f.events)
	}
}

func TestVerifyCapDefersGitHubWork(t *testing.T) {
	a, b, c := x0w5(), x0w5(), x0w5()
	b.ID, c.ID = "backend-bbbb", "backend-cccc"
	f := newFake(a, b, c)
	cfg := vcfg(false)
	cfg.VerifyMax = 2
	st := TickerState{}
	lines := pass(t, f, cfg, &st, vNow)
	if f.calls["search"] != 2 || len(st.Verified) != 2 {
		t.Fatalf("calls %v, %d checked, want 2 of 3", f.calls, len(st.Verified))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "deferred") {
		t.Fatalf("lines = %q, want one deferred", lines)
	}
	// The deferred bead goes first next pass.
	f.calls = map[string]int{}
	pass(t, f, cfg, &st, vNow.Add(time.Hour))
	if len(st.Verified) != 3 || f.calls["search"] != 1 {
		t.Fatalf("second pass calls %v, %d checked", f.calls, len(st.Verified))
	}
}

func TestVerifySkipsOnLowBudget(t *testing.T) {
	f := newFake(x0w5())
	f.left = 999
	st := TickerState{}
	_, err := verifyPass(vcfg(true), f.env(), &st, nil, true, vNow)
	if !errors.Is(err, errBudget) || f.calls["github"] != 0 {
		t.Fatalf("err %v calls %v, want a budget skip with no calls", err, f.calls)
	}
	// ticker status reads the reading to say verify is paused.
	if st.GHLeft != 999 || !st.GHLeftAt.Equal(vNow) {
		t.Fatalf("recorded %d at %s, want 999 at %s", st.GHLeft, st.GHLeftAt, vNow)
	}
	dry := TickerState{}
	verifyPass(vcfg(true), f.env(), &dry, nil, false, vNow)
	if !dry.GHLeftAt.IsZero() {
		t.Fatal("a dry run must not record the budget")
	}
}

func TestVerifyDryRunChangesNothing(t *testing.T) {
	f := newFake(withNote(x0w5(), "Linear: INF-994"))
	f.issues["INF-994"] = linearIssue{Key: "INF-994", State: "completed", Name: "Done"}
	st := TickerState{}
	lines, err := verifyPass(vcfg(true), f.env(), &st, nil, false, vNow)
	if err != nil || len(f.closedN) != 0 || len(f.events) != 0 || st.Verified != nil {
		t.Fatalf("dry run acted: err %v closed %v events %v state %v", err, f.closedN, f.events, st.Verified)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "backend-x0w5 close") {
		t.Fatalf("lines = %q, want the close it would make", lines)
	}
}

func TestVerifyLinearUnavailable(t *testing.T) {
	f := newFake(withNote(x0w5(), "Linear: INF-994"))
	f.issues, f.linErr = nil, errors.New("no API key")
	st := TickerState{}
	lines := pass(t, f, vcfg(true), &st, vNow)
	if !strings.Contains(lines[0], "Linear unavailable") {
		t.Fatalf("lines = %q", lines)
	}
}

func TestNeedlesFromX0w5(t *testing.T) {
	words, files := needles(x0w5().Title + "\n" + x0w5().Description)
	for _, w := range []string{"DescribeOrganization", "getOrganization"} {
		if !slices.Contains(words, w) {
			t.Errorf("needles %q lack %s", words, w)
		}
	}
	if !slices.Equal(files, []string{"securitylake.go"}) || !slices.Equal(pathspecs(files), []string{"**/securitylake.go"}) {
		t.Errorf("files = %q", files)
	}
}

func TestRepoSlug(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:Oscilar/backend.git":                       "Oscilar/backend",
		"https://github.com/travisjeffery/herdr-quartermaster":     "travisjeffery/herdr-quartermaster",
		"https://github.com/travisjeffery/herdr-quartermaster.git": "travisjeffery/herdr-quartermaster",
		"/local/path": "",
	} {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifyGraphQLAndParse(t *testing.T) {
	q, alias := verifyGraphQL([]string{pr14190, pr14190}, map[string]string{"s0": `repo:Oscilar/backend is:pr is:merged "INF-994"`})
	if strings.Count(q, "pullRequest(number: 14190)") != 1 || !strings.Contains(q, `s0: search(query: "repo:Oscilar/backend is:pr is:merged \"INF-994\""`) {
		t.Fatalf("query = %s", q)
	}
	out := `{"data":{"p0":{"pullRequest":{"url":"` + pr14190 + `","number":14190,"state":"MERGED","headRefOid":"abc"}},"s0":{"nodes":[{"url":"` + pr14190 + `","state":"MERGED"},{}]}}}`
	prs, found, err := parseVerify([]byte(out), alias)
	if err != nil || prs[pr14190].Head != "abc" || len(found["s0"]) != 1 {
		t.Fatalf("prs %+v found %+v err %v", prs, found, err)
	}
}

func TestParseLinear(t *testing.T) {
	out := `{"data":{"i0":{"identifier":"INF-994","url":"u","updatedAt":"2026-09-17T19:30:00Z","state":{"name":"Done","type":"completed"}},"i1":null}}`
	issues, err := parseLinear([]byte(out))
	if err != nil || issues["INF-994"].State != "completed" || issues["INF-994"].Updated == "" || len(issues) != 1 {
		t.Fatalf("issues %+v err %v", issues, err)
	}
}

func TestVerifySearchQuery(t *testing.T) {
	got := verifySearch("Oscilar/backend", x0w5(), "INF-994")
	want := `repo:Oscilar/backend is:pr is:merged "INF-994" OR "backend-x0w5" merged:>=2026-09-17`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestVerifyNeverClosesBeadWithOpenChildren(t *testing.T) {
	parent := withNote(x0w5(), "PR: "+pr14190)
	parent.Type = "task"
	child := Bead{ID: "backend-x0w5.1", Title: "Follow-up", Status: StatusOpen, Parent: "backend-x0w5", UpdatedAt: vNow}
	f := newFake(parent, child)
	f.prs[pr14190] = prRef{URL: pr14190, State: "MERGED"}
	st := TickerState{}
	pass(t, f, vcfg(true), &st, vNow)
	if len(f.closedN) != 0 || len(f.events) != 1 || !strings.Contains(f.events[0].Summary, "1 open child") {
		t.Fatalf("closed %v events %+v, want a flag naming the open child", f.closedN, f.events)
	}
}

func TestVerifyBusyMainStaysCheap(t *testing.T) {
	// A bug whose code is still on main: every pass main moves, and only a
	// free grep at the new head runs, with no GitHub calls beyond the probe.
	f := newFake(x0w5())
	f.code["then"] = "DescribeOrganization"
	f.code["main2"] = "DescribeOrganization"
	cfg := vcfg(false)
	st := TickerState{}
	pass(t, f, cfg, &st, vNow)
	for i, head := range []string{"main3", "main4", "main5"} {
		f.calls = map[string]int{}
		f.head = head
		f.code[head] = "DescribeOrganization"
		pass(t, f, cfg, &st, vNow.Add(time.Duration(i+1)*time.Hour))
		if f.calls["search"] != 0 || f.calls["github"] != 1 || f.calls["grep"] != 1 || f.calls["show"] != 0 {
			t.Fatalf("pass at %s calls = %v, want the probe and one grep", head, f.calls)
		}
	}
}

func TestMentions(t *testing.T) {
	pr := prRef{Title: "INF-994: fix", Body: "closes backend-x0w5."}
	if !mentions(pr, "backend-x0w5", "") || !mentions(pr, "", "INF-994") {
		t.Fatal("missed a mention")
	}
	if mentions(prRef{Title: "INF-9941 and backend-x0w5x"}, "backend-x0w5", "INF-994") {
		t.Fatal("matched inside a longer token")
	}
}

func TestWorkRemainsWeakensMergedPRs(t *testing.T) {
	prs := map[string]prRef{pr14190: {URL: pr14190, State: "MERGED"}}
	for name, b := range map[string]Bead{
		"rollout title":   {Title: "Roll out PR 13821 Temporal secondary protection", Notes: "PR: " + pr14190},
		"remaining note":  {Title: "Set connection_termination", Notes: "PR: " + pr14190 + " merged.\nRemaining clusters are still on the old behaviour."},
		"follow-up after": {Title: "Fix it", Notes: "PR: " + pr14190 + "\nfollow-up: wire the alarm"},
	} {
		ev := prEvidence(b, []string{pr14190}, prs)
		if len(ev) != 1 || ev[0].Strong {
			t.Errorf("%s: evidence %+v, want weak", name, ev)
		}
	}
	// Remaining work noted before the PR that delivered it doesn't count.
	b := Bead{Title: "Fix it", Notes: "still to do: the fix\nPR: " + pr14190 + " merged, verified."}
	if ev := prEvidence(b, []string{pr14190}, prs); len(ev) != 1 || !ev[0].Strong {
		t.Errorf("evidence %+v, want strong", ev)
	}
}

func TestDuplicateIgnoresFamilySharingAKey(t *testing.T) {
	b := Bead{ID: "backend-cw9f.12", Title: "c-p0p3o7: find what sends the SRV lookups", Parent: "backend-cw9f", Notes: "Linear: INF-1140"}
	sib := Bead{ID: "backend-cw9f.11", Title: "INF-1140: capture CoreDNS NXDOMAIN names", Parent: "backend-cw9f", Status: StatusClosed}
	if ev := duplicateEvidence(b, "INF-1140", []Bead{sib}, nil); len(ev) != 0 {
		t.Fatalf("sibling sharing INF-1140 is a duplicate: %+v", ev)
	}
	other := sib
	other.ID, other.Parent = "backend-zzzz", ""
	if ev := duplicateEvidence(b, "INF-1140", []Bead{other}, nil); len(ev) != 1 {
		t.Fatalf("unrelated closed bead with INF-1140 is not a duplicate: %+v", ev)
	}
}

func TestClosedUnmergedPRKeepsEvidenceWeak(t *testing.T) {
	prs := map[string]prRef{pr14190: {URL: pr14190, State: "MERGED"}, pr14190 + "1": {URL: pr14190 + "1", State: "CLOSED"}}
	ev := prEvidence(Bead{Title: "Fix it"}, []string{pr14190, pr14190 + "1"}, prs)
	if len(ev) != 1 || ev[0].Strong || !strings.Contains(ev[0].Detail, "closed unmerged") {
		t.Fatalf("evidence %+v, want weak naming the closed PR", ev)
	}
}

func TestVerifyRevalidatesBeforeClosing(t *testing.T) {
	for name, change := range map[string]func(*fakeVerify){
		"noted":   func(f *fakeVerify) { f.reshow = func(b Bead) Bead { b.UpdatedAt = vNow; return b } },
		"claimed": func(f *fakeVerify) { f.reshow = func(b Bead) Bead { b.Status = StatusInProgress; return b } },
		"agent": func(f *fakeVerify) {
			f.reshow = func(b Bead) Bead { f.agents = []Agent{{Name: "backend-x0w5"}}; return b }
		},
		"new child": func(f *fakeVerify) {
			f.reshow = func(b Bead) Bead { f.beads = append(f.beads, Bead{ID: "backend-x0w5.1", Parent: b.ID}); return b }
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(withNote(x0w5(), "Linear: INF-994"))
			f.issues["INF-994"] = linearIssue{Key: "INF-994", State: "completed", Name: "Done"}
			change(f)
			st := TickerState{}
			lines := pass(t, f, vcfg(true), &st, vNow)
			if len(f.closedN) != 0 || len(f.events) != 0 {
				t.Fatalf("closed %v events %v after the bead changed", f.closedN, f.events)
			}
			if !strings.Contains(strings.Join(lines, "\n"), "not closed") {
				t.Fatalf("lines = %q", lines)
			}
		})
	}
}

func TestVerifyUsesLatestFailedRun(t *testing.T) {
	a := "https://github.com/Oscilar/backend/actions/runs/1"
	b := "https://github.com/Oscilar/backend/actions/runs/2"
	c := "https://github.com/Oscilar/backend/actions/runs/3"
	f := newFake(withNote(x0w5(), "runs: "+a+" "+b+" "+c))
	f.code["then"], f.code["main2"] = "DescribeOrganization", ""
	day := x0w5().CreatedAt
	f.runs[a] = Run{URL: a, Workflow: "Preview", Conclusion: "failure", CreatedAt: day}
	f.runs[b] = Run{URL: b, Workflow: "Preview", Conclusion: "success", CreatedAt: day.Add(time.Hour)}
	f.runs[c] = Run{URL: c, Workflow: "Preview", Conclusion: "failure", CreatedAt: day.Add(2 * time.Hour)}
	var asked []string
	env := f.env()
	env.laterPass = func(r Run) (Run, bool, error) {
		asked = append(asked, r.URL)
		return Run{}, false, nil
	}
	st := TickerState{}
	if _, err := verifyPass(vcfg(true), env, &st, nil, true, vNow); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{c}) || len(f.closedN) != 0 {
		t.Fatalf("asked for a pass after %q, closed %v; want only the latest failure %s", asked, f.closedN, c)
	}
}

func TestVerifyReflagsEvidenceThatReturns(t *testing.T) {
	f := newFake(withNote(x0w5(), "PR: "+pr14190))
	f.prs[pr14190] = prRef{URL: pr14190, State: "MERGED", Head: "a"}
	st := TickerState{}
	pass(t, f, vcfg(false), &st, vNow)
	f.prs[pr14190] = prRef{URL: pr14190, State: "OPEN", Head: "b"}
	pass(t, f, vcfg(false), &st, vNow.Add(time.Hour))
	f.prs[pr14190] = prRef{URL: pr14190, State: "MERGED", Head: "c"}
	pass(t, f, vcfg(false), &st, vNow.Add(2*time.Hour))
	if len(f.events) != 2 {
		t.Fatalf("events = %+v, want the flag again once the evidence returned", f.events)
	}
}

func TestVerifyChargesEveryRunLookup(t *testing.T) {
	w := verifyWork{b: withNote(x0w5(), "https://github.com/o/r/actions/runs/1 https://github.com/o/r/actions/runs/2 https://github.com/o/r/actions/runs/3"), full: true, run: true}
	if got := w.cost(); got != 5 {
		t.Fatalf("cost = %d, want 5: a search, three run views and a run list", got)
	}
}
