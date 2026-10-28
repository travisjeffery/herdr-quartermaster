package main

import (
	"flag"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func bead(status string) Bead {
	return Bead{ID: "backend-ab12", Title: "Fix the thing", Status: status}
}
func agent(status string, seq int64) *Agent {
	return &Agent{Name: "backend-ab12", Kind: "claude", Status: status, PaneID: "w1:p1", Seq: seq}
}
func openPR(failing ...string) *PR {
	pr := &PR{Number: 7, State: "OPEN", Head: "tj/backend-ab12-fix-the-thing"}
	for _, f := range failing {
		pr.Checks = append(pr.Checks, Check{Name: f, Status: "COMPLETED", Conclusion: "FAILURE"})
	}
	return pr
}
func thread(b Bead, a *Agent, pr *PR) Thread {
	t := Thread{Bead: b, Agent: a, PR: pr}
	if pr != nil {
		t.Checks = summarizeChecks(pr.Checks)
	}
	return t
}

func TestClassifyPrecedence(t *testing.T) {
	cases := []struct {
		name string
		th   Thread
		want Group
	}{
		{"needs_me beats a failing PR", thread(bead(StatusNeedsMe), agent("idle", 1), openPR("build")), GroupNeedsYou},
		{"blocked agent needs you", thread(bead(StatusInProgress), agent("blocked", 1), nil), GroupNeedsYou},
		{"merged beats review", thread(bead(StatusInProgress), agent("idle", 1), &PR{Number: 7, State: "MERGED"}), GroupMerged},
		{"failing checks while working", thread(bead(StatusInProgress), agent("working", 1), openPR("build")), GroupFailing},
		{"working with green PR", thread(bead(StatusInProgress), agent("working", 1), openPR()), GroupWorking},
		{"idle with open PR is review", thread(bead(StatusInProgress), agent("done", 1), openPR()), GroupReview},
		{"dependency blocked", thread(bead(StatusBlocked), nil, nil), GroupBlocked},
		{"idle without PR", thread(bead(StatusInProgress), agent("idle", 1), nil), GroupIdle},
		{"unknown agent state is idle, not missing", thread(bead(StatusInProgress), agent("unknown", 1), nil), GroupIdle},
		{"claimed but nobody on it", thread(bead(StatusInProgress), nil, nil), GroupNoAgent},
	}
	for _, c := range cases {
		if got := classify(c.th); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestStateLine(t *testing.T) {
	if got := stateLine(thread(bead(StatusInProgress), agent("working", 1), openPR("lint", "build"))); got != "checks failing · PR #7 (build, lint)" {
		t.Errorf("got %q", got)
	}
	pr := openPR()
	pr.ReviewDecision = "CHANGES_REQUESTED"
	if got := stateLine(thread(bead(StatusInProgress), agent("idle", 1), pr)); got != "review · PR #7 changes requested" {
		t.Errorf("got %q", got)
	}
}

func TestFirstSightIsNotAChange(t *testing.T) {
	o := transition(thread(bead(StatusNeedsMe), agent("idle", 1), openPR("build")), Snapshot{}, true, t0)
	if len(o.Events) != 0 || len(o.Prompts) != 0 || o.Notify {
		t.Fatalf("first sight produced %+v", o)
	}
}

func TestFailingChecksPromptOncePerSet(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("idle", 1), openPR("build"))
	prev := Snapshot{Group: GroupReview, PRNumber: 7, PRState: "OPEN"}
	o := transition(th, prev, false, t0)
	if len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "build") {
		t.Fatalf("want one prompt naming the check, got %+v", o.Prompts)
	}
	prev = snapshot(th, prev, t0)
	if o := transition(th, prev, false, t0.Add(time.Minute)); len(o.Prompts) != 0 {
		t.Fatalf("same failing set re-prompted: %+v", o.Prompts)
	}
	th2 := thread(bead(StatusInProgress), agent("idle", 1), openPR("build", "test"))
	if o := transition(th2, prev, false, t0.Add(2*time.Minute)); len(o.Prompts) != 1 {
		t.Fatalf("a new failure should prompt again: %+v", o.Prompts)
	}
}

func TestNewReviewPromptsOnlyOnSamePR(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("idle", 1), openPR())
	th.Reviews = 2
	if o := transition(th, Snapshot{PRNumber: 0, Reviews: 0}, false, t0); len(o.Prompts) != 0 {
		t.Fatalf("reviews on a newly found PR are not new: %+v", o.Prompts)
	}
	if o := transition(th, Snapshot{PRNumber: 7, PRState: "OPEN", Reviews: 1, ReviewsSplit: true}, false, t0); len(o.Prompts) != 1 {
		t.Fatalf("want a review prompt, got %+v", o.Prompts)
	}
}

func TestMergedAsksWorkerToClose(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("idle", 1), &PR{Number: 7, State: "MERGED"})
	o := transition(th, Snapshot{PRNumber: 7, PRState: "OPEN"}, false, t0)
	if !o.Notify || len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "bd close backend-ab12") {
		t.Fatalf("got %+v", o)
	}
	if o := transition(th, Snapshot{PRNumber: 7, PRState: "MERGED"}, false, t0); len(o.Prompts) != 0 {
		t.Fatalf("merge re-announced: %+v", o)
	}
}

func TestNeedsYouNotifiesOnEntryOnly(t *testing.T) {
	th := thread(bead(StatusNeedsMe), agent("idle", 1), nil)
	if o := transition(th, Snapshot{Group: GroupWorking, AgentStatus: "working"}, false, t0); !o.Notify {
		t.Fatal("entering needs you should notify")
	}
	if o := transition(th, Snapshot{Group: GroupNeedsYou}, false, t0); o.Notify || len(o.Events) != 0 {
		t.Fatalf("staying in needs you re-notified: %+v", o)
	}
}

func TestFinishedEvenWhenTheTurnFitBetweenTicks(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("done", 9), nil)
	o := transition(th, Snapshot{Group: GroupIdle, AgentStatus: "idle", AgentSeq: 5}, false, t0)
	if len(o.Events) != 1 || o.Events[0].Kind != EventFinished {
		t.Fatalf("want a finished event, got %+v", o.Events)
	}
	if o := transition(th, Snapshot{Group: GroupIdle, AgentStatus: "done", AgentSeq: 9}, false, t0); len(o.Events) != 0 {
		t.Fatalf("an untouched idle agent did not finish anything: %+v", o.Events)
	}
}

func TestReadySinceTracksUntouchedIdle(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("idle", 5), nil)
	s1 := snapshot(th, Snapshot{AgentStatus: "working", AgentSeq: 4}, t0)
	if !s1.ReadySince.Equal(t0) {
		t.Fatalf("becoming idle starts the clock: %v", s1.ReadySince)
	}
	s2 := snapshot(th, s1, t0.Add(30*time.Second))
	if !s2.ReadySince.Equal(t0) {
		t.Fatalf("unchanged idle keeps the clock: %v", s2.ReadySince)
	}
	if deliverable(s2, t0.Add(30*time.Second), time.Minute) {
		t.Fatal("deliverable before the idle window")
	}
	if !deliverable(s2, t0.Add(61*time.Second), time.Minute) {
		t.Fatal("not deliverable after the idle window")
	}
	touched := thread(bead(StatusInProgress), agent("idle", 6), nil)
	if s3 := snapshot(touched, s2, t0.Add(90*time.Second)); !s3.ReadySince.Equal(t0.Add(90 * time.Second)) {
		t.Fatalf("a state change (user typing, new turn) restarts the clock: %v", s3.ReadySince)
	}
	if s4 := snapshot(thread(bead(StatusInProgress), agent("working", 7), nil), s2, t0); !s4.ReadySince.IsZero() {
		t.Fatal("a working agent is never ready")
	}
}

func TestBranchFor(t *testing.T) {
	cases := map[string]Bead{
		"tj/backend-ab12-fix-the-thing":           {ID: "backend-ab12", Title: "Fix the thing"},
		"tj/backend-qw12-3-raise-the-worker-pool": {ID: "backend-qw12.3", Title: "Raise the worker pool timeout on every node"},
		"tj/backend-zz99":                         {ID: "backend-zz99", Title: "!!!"},
	}
	for want, b := range cases {
		if got := branchFor("tj/", b); got != want {
			t.Errorf("branchFor(%q) = %q, want %q", b.Title, got, want)
		}
	}
}

func TestPRForBead(t *testing.T) {
	b := Bead{ID: "backend-ab12", Notes: "PR: https://github.com/acme/app/pull/1\nlater PR https://github.com/acme/infra/pull/2"}
	mine := []PR{{Number: 9, Head: "tj/backend-ab12x-other"}, {Number: 3, Head: "tj/backend-ab12-fix"}}
	if pr, _, ok := prForBead(b, "tj/", mine, nil, nil); !ok || pr.Number != 3 {
		t.Fatalf("own branch should win and not match a longer id: %+v", pr)
	}
	byURL := map[string]PR{
		"https://github.com/acme/app/pull/1":   {Number: 1},
		"https://github.com/acme/infra/pull/2": {Number: 2},
	}
	if pr, _, ok := prForBead(b, "tj/", nil, byURL, nil); !ok || pr.Number != 2 {
		t.Fatalf("latest noted PR should win: %+v", pr)
	}
}

func TestPRForBeadSkipsChildBranches(t *testing.T) {
	mine := []PR{
		{Number: 2, Head: "tj/backend-qkvu-2-inf-1247-radar-rules"},
		{Number: 10, Head: "tj/backend-qkvu-10-follow-up"},
		{Number: 1, Head: "tj/backend-qkvu-quartermaster-thing"},
		{Number: 71, Head: "tj/backend-cw9f-7-1-nested"},
		{Number: 7, Head: "tj/backend-cw9f-7-sweep"},
	}
	cases := []struct {
		id    string
		title string
		want  int
	}{
		{"backend-qkvu", "quartermaster thing", 1},
		{"backend-qkvu.2", "inf-1247 radar rules", 2},
		{"backend-qkvu.10", "follow up", 10},
		{"backend-cw9f.7", "sweep", 7},
		{"backend-cw9f.7.1", "nested", 71},
	}
	for _, c := range cases {
		pr, _, ok := prForBead(Bead{ID: c.id, Title: c.title}, "tj/", mine, nil, nil)
		if !ok || pr.Number != c.want {
			t.Errorf("%s: got #%d (%v), want #%d", c.id, pr.Number, ok, c.want)
		}
	}
	// A parent with no PR of its own matches none of its children's.
	if pr, _, ok := prForBead(Bead{ID: "backend-qkvu"}, "tj/", mine[:2], nil, nil); ok {
		t.Errorf("parent matched child PR #%d", pr.Number)
	}
	// The exact branch quartermaster names for a bead counts even when its slug
	// starts with digits.
	b := Bead{ID: "backend-qkvu", Title: "2 things"}
	if pr, _, ok := prForBead(b, "tj/", []PR{{Number: 5, Head: "tj/backend-qkvu-2-things"}}, nil, nil); !ok || pr.Number != 5 {
		t.Errorf("exact branch: got #%d (%v)", pr.Number, ok)
	}
}

func TestPRForBeadLeavesAnotherBeadsBranch(t *testing.T) {
	owner := Bead{ID: "backend-qkvu"}
	linker := Bead{ID: "backend-72s4", Notes: "see https://github.com/o/backend/pull/14776"}
	byURL := map[string]PR{"https://github.com/o/backend/pull/14776": {Number: 14776, State: "OPEN", Head: "tj/backend-qkvu-radar"}}
	ownedBy := func(id string) func(PR) string { return func(PR) string { return id } }
	if pr, _, ok := prForBead(linker, "tj/", nil, byURL, ownedBy(owner.ID)); ok {
		t.Errorf("72s4 took #%d from the bead whose branch it is on", pr.Number)
	}
	// With no other owner (inactive, or the PR is in another repo), the link stands.
	if _, _, ok := prForBead(linker, "tj/", nil, byURL, ownedBy("")); !ok {
		t.Error("noted PR nobody else owns should still count")
	}
}

func TestPRForBeadPrefersOpenThenLatestMerge(t *testing.T) {
	b := Bead{ID: "backend-wlri", Notes: "PR: https://github.com/o/a/pull/273\nPR: https://github.com/o/b/pull/151"}
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	byURL := map[string]PR{
		"https://github.com/o/a/pull/273": {Number: 273, URL: "https://github.com/o/a/pull/273", State: "OPEN"},
		"https://github.com/o/b/pull/151": {Number: 151, URL: "https://github.com/o/b/pull/151", State: "MERGED", MergedAt: at(1)},
	}
	pr, merged, _ := prForBead(b, "tj/", nil, byURL, nil)
	if pr.Number != 273 {
		t.Fatalf("open PR should win over the later-noted merged one, got #%d", pr.Number)
	}
	if len(merged) != 1 || merged[0].Number != 151 {
		t.Fatalf("merged = %+v, want #151", merged)
	}
	byURL["https://github.com/o/a/pull/273"] = PR{Number: 273, State: "MERGED", MergedAt: at(2)}
	if pr, _, _ := prForBead(b, "tj/", nil, byURL, nil); pr.Number != 273 {
		t.Fatalf("last merge should win, got #%d", pr.Number)
	}
	byURL["https://github.com/o/b/pull/151"] = PR{Number: 151, State: "CLOSED"}
	if pr, _, _ := prForBead(b, "tj/", nil, byURL, nil); pr.Number != 273 {
		t.Fatalf("merged should win over closed, got #%d", pr.Number)
	}
}

func TestMergedOncePerPR(t *testing.T) {
	a := &PR{Number: 151, URL: "https://github.com/o/b/pull/151", State: "MERGED"}
	b := &PR{Number: 273, URL: "https://github.com/o/a/pull/273", State: "OPEN"}
	prev := snapshot(thread(bead(StatusInProgress), agent("idle", 1), a), Snapshot{PRNumber: 151, PRState: "OPEN"}, t0)
	// The pick moves to the open PR and back: the first merge is not news.
	prev = snapshot(thread(bead(StatusInProgress), agent("idle", 1), b), prev, t0)
	th := thread(bead(StatusInProgress), agent("idle", 1), a)
	if o := transition(th, prev, false, t0); len(o.Events) != 0 || len(o.Prompts) != 0 {
		t.Fatalf("#151 merge re-announced: %+v", o)
	}
	b.State = "MERGED"
	th = thread(bead(StatusInProgress), agent("idle", 1), b)
	if o := transition(th, prev, false, t0); len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "PR #273 merged") {
		t.Fatalf("#273 merge not announced: %+v", o)
	}
}

func TestMergedBehindAnotherOpenPR(t *testing.T) {
	a := PR{Number: 1, URL: "https://github.com/o/a/pull/1", State: "OPEN"}
	b := PR{Number: 2, URL: "https://github.com/o/a/pull/2", State: "OPEN"}
	prev := snapshot(thread(bead(StatusInProgress), agent("idle", 1), &a), Snapshot{}, t0)
	// #1 merges, but #2 is still open and becomes the pick.
	a.State = "MERGED"
	th := thread(bead(StatusInProgress), agent("idle", 1), &b)
	th.Merged = []PR{a}
	o := transition(th, prev, false, t0)
	if len(o.Events) != 1 || o.Events[0].Summary != "PR #1 merged" {
		t.Fatalf("merge behind an open PR not announced: %+v", o.Events)
	}
	if len(o.Prompts) != 0 {
		t.Fatalf("asked to close with #2 still open: %+v", o.Prompts)
	}
	prev = snapshot(th, prev, t0)
	// #2 merges later and wins the pick; #1 is not announced again.
	b.State = "MERGED"
	th = thread(bead(StatusInProgress), agent("idle", 1), &b)
	th.Merged = []PR{a, b}
	o = transition(th, prev, false, t0)
	if len(o.Events) != 1 || o.Events[0].Summary != "PR #2 merged" || len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "bd close") {
		t.Fatalf("got %+v", o)
	}
}

func TestMergedLegacySnapshotByNumberOnlyOnce(t *testing.T) {
	// After the upgrade pass, a same-numbered PR from another repo is news.
	prev := snapshot(thread(bead(StatusInProgress), agent("idle", 1), &PR{Number: 42, URL: "https://github.com/o/a/pull/42", State: "MERGED"}), Snapshot{PRNumber: 42, PRState: "MERGED"}, t0)
	th := thread(bead(StatusInProgress), agent("idle", 1), &PR{Number: 42, URL: "https://github.com/o/b/pull/42", State: "MERGED"})
	if o := transition(th, prev, false, t0); len(o.Events) != 1 {
		t.Fatalf("o/b#42 merge suppressed by o/a#42: %+v", o)
	}
}

func TestSummarizeChecks(t *testing.T) {
	s := summarizeChecks([]Check{
		{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Name: "e2e", Status: "IN_PROGRESS"},
		{Context: "codecov", State: "ERROR"},
		{Name: "skip", Status: "COMPLETED", Conclusion: "SKIPPED"},
	})
	if !slices.Equal(s.Failing, []string{"codecov", "lint"}) || s.Pending != 1 || s.Passing != 2 {
		t.Fatalf("got %+v", s)
	}
}

func TestInterspersedFlags(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	kind := fs.String("agent", "", "")
	pos, err := interspersed(fs, []string{"backend-ab12", "--agent", "codex"})
	if err != nil || !slices.Equal(pos, []string{"backend-ab12"}) || *kind != "codex" {
		t.Fatalf("pos=%v kind=%q err=%v", pos, *kind, err)
	}
	// A bool flag after the bead id, as in resolve <bead> --force.
	force := fs.Bool("force", false, "")
	pos, err = interspersed(fs, []string{"backend-ab12", "--force"})
	if err != nil || !slices.Equal(pos, []string{"backend-ab12"}) || !*force {
		t.Fatalf("pos=%v force=%v err=%v", pos, *force, err)
	}
}

func TestParseOpened(t *testing.T) {
	o, err := parseOpened([]byte(`{"workspace":{"workspace_id":"w9","worktree":{"checkout_path":"/w/x"}},"root_pane":{"pane_id":"w9:p1"}}`))
	if err != nil || o.PaneID != "w9:p1" || o.Path != "/w/x" || o.WorkspaceID != "w9" {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := parseOpened([]byte(`{}`)); err == nil {
		t.Fatal("missing pane must be an error")
	}
}

func TestNotedRuns(t *testing.T) {
	notes := strings.Join([]string{
		"deploy: https://github.com/acme/app/actions/runs/1",
		"canary https://github.com/acme/app/actions/runs/2/job/77",
		"provision https://github.com/acme/infra/actions/runs/3/attempts/2",
		"rollout https://github.com/acme/app/actions/runs/4",
		"retried https://github.com/acme/app/actions/runs/2/attempts/2",
	}, "\n")
	want := []string{
		"https://github.com/acme/infra/actions/runs/3",
		"https://github.com/acme/app/actions/runs/4",
		"https://github.com/acme/app/actions/runs/2",
	}
	if got := NotedRuns(notes); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := NotedRuns("PR: https://github.com/acme/app/pull/1"); len(got) != 0 {
		t.Fatalf("a PR link is not a run: %v", got)
	}
}

func deployRun(status, conclusion string) Run {
	return Run{ID: 42, Status: status, Conclusion: conclusion, Workflow: "Deploy", URL: "https://github.com/acme/app/actions/runs/42", Repo: "acme/app"}
}

func runThread(r Run) Thread {
	th := thread(bead(StatusInProgress), agent("idle", 1), &PR{Number: 7, State: "MERGED"})
	th.Runs = []Run{r}
	return th
}

func TestRunFirstSightIsNotAChange(t *testing.T) {
	th := runThread(deployRun("completed", "failure"))
	if o := transition(th, Snapshot{PRNumber: 7, PRState: "MERGED"}, false, t0); len(o.Events) != 0 || len(o.Prompts) != 0 || o.Notify {
		t.Fatalf("a newly noted run produced %+v", o)
	}
}

func TestRunSucceededPromptsNextStep(t *testing.T) {
	running := runThread(deployRun("in_progress", ""))
	prev := snapshot(running, Snapshot{PRNumber: 7, PRState: "MERGED"}, t0)
	done := runThread(deployRun("completed", "success"))
	o := transition(done, prev, false, t0)
	if len(o.Events) != 1 || o.Events[0].Kind != EventRunSucceeded || o.Notify {
		t.Fatalf("want one run_succeeded event and no notification, got %+v", o)
	}
	want := "[quartermaster: automated, not the user] Deploy run https://github.com/acme/app/actions/runs/42 succeeded. Continue with the next step of the rollout."
	if len(o.Prompts) != 1 || o.Prompts[0] != want {
		t.Fatalf("got prompts %q", o.Prompts)
	}
	if o := transition(done, snapshot(done, prev, t0), false, t0.Add(time.Minute)); len(o.Events) != 0 || len(o.Prompts) != 0 {
		t.Fatalf("a finished run was re-announced: %+v", o)
	}
}

func TestRunFailedNotifiesAndPromptsInvestigation(t *testing.T) {
	for _, conclusion := range []string{"failure", "cancelled", "timed_out"} {
		prev := snapshot(runThread(deployRun("queued", "")), Snapshot{PRNumber: 7, PRState: "MERGED"}, t0)
		o := transition(runThread(deployRun("completed", conclusion)), prev, false, t0)
		if len(o.Events) != 1 || o.Events[0].Kind != EventRunFailed || !o.Notify {
			t.Fatalf("%s: want a notified run_failed event, got %+v", conclusion, o)
		}
		if len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "`gh run view 42 -R acme/app --log-failed`") {
			t.Fatalf("%s: got prompts %q", conclusion, o.Prompts)
		}
	}
}

func TestStateLineShowsRunningWorkflow(t *testing.T) {
	th := runThread(deployRun("in_progress", ""))
	if got := stateLine(th); got != "merged · PR #7 · Deploy running" {
		t.Errorf("got %q", got)
	}
	if got := stateLine(runThread(deployRun("completed", "success"))); got != "merged · PR #7" {
		t.Errorf("a finished run is not running: %q", got)
	}
}

func TestRollingOutAfterMerge(t *testing.T) {
	b := bead(StatusInProgress)
	b.Labels = []string{LabelRollingOut}
	th := thread(b, agent("idle", 1), &PR{Number: 7, State: "MERGED"})
	if got := classify(th); got != GroupRollingOut {
		t.Fatalf("got %s, want %s", got, GroupRollingOut)
	}
	if got := classify(thread(b, agent("idle", 1), openPR())); got != GroupReview {
		t.Fatalf("an unmerged rolling-out bead is still in review, got %s", got)
	}
	if !(GroupReview < GroupRollingOut && GroupRollingOut < GroupFailing) {
		t.Fatal("rolling out sorts right after review")
	}
	o := transition(th, Snapshot{PRNumber: 7, PRState: "OPEN"}, false, t0)
	if len(o.Prompts) != 1 {
		t.Fatalf("want one merge prompt, got %q", o.Prompts)
	}
	p := o.Prompts[0]
	if !strings.Contains(p, "Continue the rollout") || !strings.Contains(p, "bd label remove backend-ab12 rolling-out") ||
		!strings.Contains(p, "bd close backend-ab12") || strings.Contains(p, "and stop") {
		t.Fatalf("merge prompt should continue the rollout, not close now: %q", p)
	}
}

func TestBotAndHumanReviews(t *testing.T) {
	prev := Snapshot{PRNumber: 7, PRState: "OPEN", Reviews: 1, BotReviews: 1, ReviewsSplit: true}
	bot := thread(bead(StatusInProgress), agent("idle", 1), openPR())
	bot.Reviews, bot.BotReviews = 1, []string{"chatgpt-codex-connector", "coderabbitai"}
	o := transition(bot, prev, false, t0)
	if o.Notify || len(o.Events) != 1 || o.Events[0].Summary != "PR #7 has a new review from coderabbitai" {
		t.Fatalf("a bot review is an unnotified event naming the bot, got %+v", o)
	}
	if len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "new review from coderabbitai") {
		t.Fatalf("got prompts %q", o.Prompts)
	}
	human := thread(bead(StatusInProgress), agent("idle", 1), openPR())
	human.Reviews, human.BotReviews = 2, []string{"chatgpt-codex-connector"}
	o = transition(human, prev, false, t0)
	if !o.Notify || len(o.Events) != 1 || o.Events[0].Summary != "PR #7 has new human review feedback" {
		t.Fatalf("a human review notifies, got %+v", o)
	}
	if len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "new human review feedback") {
		t.Fatalf("got prompts %q", o.Prompts)
	}
}

func TestBotReviewsSplitFromHumans(t *testing.T) {
	reviews := []Review{{State: "APPROVED"}, {State: "COMMENTED", Bot: true}, {State: "COMMENTED"}, {State: "COMMENTED"}}
	reviews[0].Author.Login = "alice"
	reviews[1].Author.Login = "chatgpt-codex-connector"
	reviews[2].Author.Login = "travisjeffery"
	reviews[3].Author.Login = "renovate[bot]"
	if n := reviewsNotBy(reviews, "travisjeffery"); n != 1 {
		t.Errorf("human reviews: got %d, want 1", n)
	}
	if got := botReviews(reviews); !slices.Equal(got, []string{"chatgpt-codex-connector", "renovate[bot]"}) {
		t.Errorf("bot reviews: got %v", got)
	}
}

func TestFirstPassAfterUpgradeOnlySetsReviewBaseline(t *testing.T) {
	// Before bots were told apart, 2 humans + 1 bot were saved as Reviews=3.
	// Now there are 3 humans and 1 bot: nothing is attributed on this pass.
	th := thread(bead(StatusInProgress), agent("idle", 1), openPR())
	th.Reviews, th.BotReviews = 3, []string{"codex-connector"}
	legacy := Snapshot{PRNumber: 7, PRState: "OPEN", Reviews: 3}
	if o := transition(th, legacy, false, t0); len(o.Prompts) != 0 || o.Notify {
		t.Fatalf("a legacy snapshot produced review prompts: %+v", o)
	}
	next := snapshot(th, legacy, t0)
	th.Reviews = 4
	if o := transition(th, next, false, t0); len(o.Prompts) != 1 || !o.Notify {
		t.Fatalf("a human review after the baseline should prompt and notify: %+v", o)
	}
}

func TestCoordinatorSortsFirstAndSummarises(t *testing.T) {
	for _, g := range []Group{GroupNeedsYou, GroupNoAgent} {
		if r := rank(g, "backend-ab12"); !(coordinatorRank < r) {
			t.Fatalf("coordinator rank %q must sort before thread rank %q", coordinatorRank, r)
		}
	}
	threads := []Thread{
		thread(bead(StatusNeedsMe), agent("idle", 1), nil),
		thread(bead(StatusNeedsMe), agent("idle", 1), nil),
		thread(bead(StatusInProgress), agent("working", 1), nil),
	}
	if got := coordinatorLine(threads, 3); got != "coordinator · 2 need you · 1 working · 3 inbox" {
		t.Fatalf("got %q", got)
	}
	if got := coordinatorLine(nil, 0); got != "coordinator" {
		t.Fatalf("got %q", got)
	}
}

func readyPR() *PR {
	pr := openPR()
	pr.ReviewDecision, pr.MergeState, pr.HeadSHA = "APPROVED", "CLEAN", "aaa"
	pr.Checks = []Check{{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"}}
	pr.Reviews = []Review{approvalOf("aaa", t0.Add(-time.Hour))}
	pr.Gate = &MergeGate{Head: "aaa", At: t0}
	return pr
}

func approvalOf(sha string, at time.Time) Review {
	r := Review{State: "APPROVED", SubmittedAt: at}
	r.Commit.OID = sha
	return r
}

func TestReadyToMerge(t *testing.T) {
	cases := []struct {
		name string
		edit func(*PR)
		want bool
	}{
		{"approved and clean", func(*PR) {}, true},
		{"no review required", func(pr *PR) { pr.ReviewDecision = "" }, true},
		{"only optional checks failing", func(pr *PR) {
			pr.MergeState = "UNSTABLE"
			pr.Checks = append(pr.Checks, Check{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"})
		}, true},
		{"required check pending or failing", func(pr *PR) { pr.MergeState = "BLOCKED" }, false},
		{"review required", func(pr *PR) { pr.ReviewDecision = "REVIEW_REQUIRED" }, false},
		{"changes requested", func(pr *PR) { pr.ReviewDecision = "CHANGES_REQUESTED" }, false},
		{"draft", func(pr *PR) { pr.IsDraft = true }, false},
		{"behind base", func(pr *PR) { pr.MergeState = "BEHIND" }, false},
		{"conflicts", func(pr *PR) { pr.MergeState = "DIRTY" }, false},
		{"merge state not computed yet", func(pr *PR) { pr.MergeState = "UNKNOWN" }, false},
		{"checks still running", func(pr *PR) {
			pr.MergeState = "UNSTABLE"
			pr.Checks = append(pr.Checks, Check{Name: "e2e", Status: "IN_PROGRESS"})
		}, false},
		{"merged", func(pr *PR) { pr.State = "MERGED" }, false},
		{"open review thread", func(pr *PR) { pr.Gate.OpenThreads = 1 }, false},
		{"review threads past the first page", func(pr *PR) { pr.Gate.MoreThreads = true }, false},
		// A pending approval of an older commit submitted after the push.
		{"approval of an older commit", func(pr *PR) { pr.Reviews[0].Commit.OID = "old" }, false},
		{"approval older than 7 days", func(pr *PR) { pr.Reviews[0].SubmittedAt = t0.Add(-8 * 24 * time.Hour) }, false},
		{"a fresh approval beside a stale one", func(pr *PR) {
			pr.Reviews = append([]Review{approvalOf("old", t0.Add(-30*24*time.Hour))}, pr.Reviews...)
		}, true},
		{"commented after the head is no approval", func(pr *PR) { pr.Reviews[0].State = "COMMENTED" }, false},
		{"no review required needs no approval", func(pr *PR) { pr.ReviewDecision, pr.Reviews = "", nil }, true},
		{"no review required still needs threads resolved", func(pr *PR) {
			pr.ReviewDecision, pr.Reviews, pr.Gate.OpenThreads = "", nil, 2
		}, false},
		{"gate not fetched", func(pr *PR) { pr.Gate = nil }, false},
		{"gate from another head", func(pr *PR) { pr.Gate.Head = "old" }, false},
	}
	for _, c := range cases {
		pr := readyPR()
		c.edit(pr)
		if got := readyToMerge(pr, summarizeChecks(pr.Checks)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadyToMergeClassifiesAboveOptionalFailures(t *testing.T) {
	pr := readyPR()
	pr.MergeState = "UNSTABLE"
	pr.Checks = append(pr.Checks, Check{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"})
	th := thread(bead(StatusInProgress), agent("idle", 1), pr)
	if g := classify(th); g != GroupReady {
		t.Fatalf("got %s, want %s", g, GroupReady)
	}
	if got := stateLine(th); got != "ready to merge · PR #7 approved" {
		t.Errorf("got %q", got)
	}
}

// backend#14388 on 2026-10-01: reviewDecision APPROVED from a 09-23 approval
// that predates the 10-01 push, with three unresolved Codex threads.
func TestApprovedButBlocked(t *testing.T) {
	pr := readyPR()
	pr.Number = 14388
	pr.Reviews = []Review{approvalOf("903ed41", time.Date(2026, 9, 23, 4, 50, 32, 0, time.UTC))}
	pr.Gate = &MergeGate{Head: "aaa", OpenThreads: 3, At: time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)}
	th := thread(bead(StatusInProgress), agent("idle", 1), pr)
	if g := classify(th); g == GroupReady {
		t.Fatalf("classified %s", g)
	}
	if got, want := stateLine(th), "review · PR #14388 approved but blocked: 3 open threads, stale approval"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	prev := Snapshot{Group: GroupReview, PRNumber: 14388, PRState: "OPEN", ReviewsSplit: true}
	if o := transition(th, prev, false, t0); len(o.Events) != 0 || len(o.Prompts) != 0 {
		t.Fatalf("announced a blocked PR: %+v", o)
	}
	if k := readyKey(th); k != "" {
		t.Fatalf("readyKey = %q", k)
	}
}

func TestParseGates(t *testing.T) {
	out := []byte(`{"data":{
		"p0":{"pullRequest":{"headRefOid":"8b47",
			"reviewThreads":{"pageInfo":{"hasNextPage":true},"nodes":[{"isResolved":false,"isOutdated":false},{"isResolved":true,"isOutdated":false},{"isResolved":false,"isOutdated":true}]}}},
		"p1":{"pullRequest":null}}}`)
	alias := map[string]string{"p0": "https://github.com/o/r/pull/1", "p1": "https://github.com/o/r/pull/2", "p2": "https://github.com/o/r/pull/3"}
	gates, err := parseGates(out, alias, t0)
	if err != nil {
		t.Fatal(err)
	}
	want := MergeGate{Head: "8b47", OpenThreads: 1, MoreThreads: true, At: t0}
	if len(gates) != 1 || gates["https://github.com/o/r/pull/1"] != want {
		t.Fatalf("got %+v", gates)
	}
}

// An approval cached across a failed gate query still ages out.
func TestApplyGatesAgesCachedApproval(t *testing.T) {
	pr := *readyPR()
	old := map[string]PR{"b": pr}
	now := t0.Add(7 * 24 * time.Hour)
	prs := map[string]PR{"b": pr}
	applyGates(old, prs, nil, true, now)
	if g := prs["b"].Gate; g == nil || !g.At.Equal(now) {
		t.Fatalf("gate = %+v", g)
	}
	if got := prs["b"]; readyToMerge(&got, summarizeChecks(got.Checks)) {
		t.Fatal("a cached approval past 7 days is still ready")
	}
	pushed := pr
	pushed.HeadSHA = "bbb"
	prs = map[string]PR{"b": pushed}
	if applyGates(old, prs, nil, true, now); prs["b"].Gate != nil {
		t.Fatal("kept a gate from another head")
	}
	prs = map[string]PR{"b": pr}
	if applyGates(old, prs, nil, false, now); prs["b"].Gate != nil {
		t.Fatal("kept a gate the query no longer returns")
	}
}

func TestReadyToMergeOncePerHead(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("idle", 1), readyPR())
	prev := Snapshot{Group: GroupReview, PRNumber: 7, PRState: "OPEN", ReviewsSplit: true}
	o := transition(th, prev, false, t0)
	if len(o.Events) != 1 || o.Events[0].Kind != EventReady || !o.Notify {
		t.Fatalf("want one notifying ready_to_merge event, got %+v", o)
	}
	if len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "PR #7 is ready to merge at aaa: approved") || !strings.Contains(o.Prompts[0], "--match-head-commit aaa") {
		t.Fatalf("want a ready prompt, got %+v", o.Prompts)
	}
	prev = snapshot(th, prev, t0)
	if o := transition(th, prev, false, t0.Add(time.Minute)); len(o.Events) != 0 || len(o.Prompts) != 0 {
		t.Fatalf("same head re-announced: %+v", o)
	}
	// Losing readiness and getting it back on the same head stays quiet.
	blocked := readyPR()
	blocked.MergeState = "BLOCKED"
	prev = snapshot(thread(bead(StatusInProgress), agent("idle", 1), blocked), prev, t0.Add(2*time.Minute))
	if o := transition(th, prev, false, t0.Add(3*time.Minute)); len(o.Events) != 0 {
		t.Fatalf("same head re-announced after a blip: %+v", o)
	}
	pushed := readyPR()
	pushed.HeadSHA, pushed.Gate.Head = "bbb", "bbb"
	pushed.Reviews = []Review{approvalOf("bbb", t0)}
	if o := transition(thread(bead(StatusInProgress), agent("idle", 1), pushed), prev, false, t0.Add(4*time.Minute)); len(o.Events) != 1 || o.Events[0].Kind != EventReady {
		t.Fatalf("a new head ready to merge should announce again: %+v", o)
	}
}

func TestReadyToMergeFirstSightIsBaseline(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("idle", 1), readyPR())
	if o := transition(th, Snapshot{}, true, t0); len(o.Events) != 0 {
		t.Fatalf("first sight produced %+v", o)
	}
	if s := snapshot(th, Snapshot{}, t0); s.ReadyKey != "7@aaa" {
		t.Fatalf("baseline ReadyKey = %q", s.ReadyKey)
	}
}

func TestReadyToMergeReplacementPRSameHead(t *testing.T) {
	prev := snapshot(thread(bead(StatusInProgress), agent("idle", 1), readyPR()), Snapshot{}, t0)
	pr := readyPR()
	pr.Number = 8
	if o := transition(thread(bead(StatusInProgress), agent("idle", 1), pr), prev, false, t0.Add(time.Minute)); len(o.Events) != 1 || o.Events[0].Kind != EventReady {
		t.Fatalf("a replacement PR on the same head should announce: %+v", o)
	}
}

func TestQueuedReadyPromptDroppedWhenHeadMoves(t *testing.T) {
	th := thread(bead(StatusInProgress), agent("working", 1), readyPR())
	prev := Snapshot{Group: GroupWorking, AgentStatus: "working", PRNumber: 7, PRState: "OPEN", ReviewsSplit: true}
	o := transition(th, prev, false, t0)
	prev = snapshot(th, prev, t0)
	prev.Pending = append(prev.Pending, o.Prompts...)
	if s := snapshot(th, prev, t0.Add(time.Minute)); len(s.Pending) != 1 {
		t.Fatalf("a still-ready head should keep its prompt: %+v", s.Pending)
	}
	pushed := readyPR()
	pushed.HeadSHA, pushed.MergeState = "bbb", "BLOCKED"
	pushed.Checks = append(pushed.Checks, Check{Name: "e2e", Status: "IN_PROGRESS"})
	// One queued by the kelpie ticker, before the rename, is dropped too.
	prev.Pending = append(prev.Pending, strings.Replace(o.Prompts[0], "[quartermaster:", "[kelpie:", 1), "other prompt")
	s := snapshot(thread(bead(StatusInProgress), agent("working", 1), pushed), prev, t0.Add(2*time.Minute))
	if !slices.Equal(s.Pending, []string{"other prompt"}) {
		t.Fatalf("stale ready prompt kept: %+v", s.Pending)
	}
}

func TestNextRunsStopsFollowingFinishedRuns(t *testing.T) {
	const base = "https://github.com/acme/app/actions/runs/"
	b := Bead{ID: "backend-ab12", Notes: "deploy " + base + "1\ncanary " + base + "2\nretry " + base + "3"}
	other := Bead{ID: "backend-cd34", Notes: "new " + base + "4"}
	prev := map[string][]Run{b.ID: {
		{ID: 1, Status: "completed", Conclusion: "success", Workflow: "Deploy", Repo: "acme/app"},
		{ID: 2, Status: "in_progress", Workflow: "Canary", Repo: "acme/app"},
		{ID: 3, Status: "completed", Conclusion: "failure", Workflow: "Provision", Repo: "acme/app"},
	}}
	var mu sync.Mutex
	var viewed []string
	view := func(repo, id string) (Run, error) {
		mu.Lock()
		viewed = append(viewed, id)
		mu.Unlock()
		if id == "3" {
			return Run{ID: 3, Status: "in_progress", Workflow: "Provision", Repo: repo}, nil
		}
		return Run{}, fmt.Errorf("gh run view: exit status 1: HTTP 404: Not Found")
	}
	all := nextRuns(prev, []Bead{b, other}, view, func(string, ...any) {})
	got := append(all[b.ID], all[other.ID]...)
	slices.Sort(viewed)
	if !slices.Equal(viewed, []string{"2", "3", "4"}) {
		t.Fatalf("viewed %v; a succeeded run must not be polled, a failed one must (rerun)", viewed)
	}
	if len(got) != 4 || got[0].Conclusion != "success" || got[2].Status != "in_progress" {
		t.Fatalf("got %+v", got)
	}
	for _, r := range []Run{got[1], got[3]} {
		if r.Status != "completed" || r.Conclusion != runGone {
			t.Fatalf("a 404 run must be recorded gone, got %+v", r)
		}
	}
	if got[1].Workflow != "Canary" || got[3].ID != 4 || got[3].URL != base+"4" {
		t.Fatalf("gone runs keep what was known: %+v %+v", got[1], got[3])
	}
	viewed = nil
	nextRuns(all, []Bead{b, other}, view, func(string, ...any) {})
	if !slices.Equal(viewed, []string{"3"}) {
		t.Fatalf("gone runs must not be polled again, viewed %v", viewed)
	}
	o := transition(runThread(got[1]), snapshot(runThread(prev[b.ID][1]), Snapshot{PRNumber: 7, PRState: "MERGED"}, t0), false, t0)
	if len(o.Events) != 0 || len(o.Prompts) != 0 || o.Notify {
		t.Fatalf("a run going from running to gone must not prompt, got %+v", o)
	}
}

// A batched lookup answers what it can: an alias whose repository is gone is
// null beside the others' data.
func TestParseLookupKeepsPartialAnswer(t *testing.T) {
	out := []byte(`{"data":{
		"p0":{"pullRequest":{"number":7,"url":"https://github.com/o/r/pull/7","headRefName":"tj/x","state":"MERGED","mergedAt":"2026-10-01T00:00:00Z"}},
		"p1":null},
		"errors":[{"type":"NOT_FOUND","path":["p1"]}]}`)
	alias := map[string]string{"p0": "https://github.com/o/r/pull/7", "p1": "https://github.com/o/gone/pull/1"}
	prs := map[string]PR{}
	if err := parseLookup(out, alias, prs); err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[alias["p0"]].State != "MERGED" || prs[alias["p0"]].MergedAt.IsZero() {
		t.Fatalf("got %+v", prs)
	}
}

func TestNextNotedLooksUpOnlyWhatCanChange(t *testing.T) {
	cfg := Config{Repo: "/src/app"}
	const gh = "https://github.com/o/"
	b := Bead{ID: "backend-ab12", Notes: "PR: " + gh + "app/pull/1\nPR: " + gh + "other/pull/2\nPR: " + gh + "other/pull/3\nagain " + gh + "other/pull/3"}
	c := Bead{ID: "backend-cd34", Notes: "PR: " + gh + "other/pull/3\nPR: " + gh + "other/pull/4"}
	mine := map[string][]PR{"/src/app": {{URL: gh + "app/pull/1", State: "OPEN"}}}
	inList := map[string]PR{gh + "app/pull/1": mine["/src/app"][0]}
	prev := map[string]PR{
		gh + "other/pull/2": {URL: gh + "other/pull/2", State: "MERGED"},
		gh + "other/pull/4": {URL: gh + "other/pull/4", State: "OPEN"},
		gh + "other/pull/9": {URL: gh + "other/pull/9", State: "MERGED"},
	}
	var calls [][]string
	lookup := func(urls []string) (map[string]PR, error) {
		calls = append(calls, urls)
		return map[string]PR{gh + "other/pull/3": {URL: gh + "other/pull/3", State: "OPEN"}}, nil
	}
	got := nextNoted(cfg, prev, []Bead{b, c}, mine, inList, lookup, func(string, ...any) {})
	if len(calls) != 1 || !slices.Equal(calls[0], []string{gh + "other/pull/3", gh + "other/pull/4"}) {
		t.Fatalf("lookups %v; want one batch of the PRs not listed and not merged", calls)
	}
	if got[gh+"other/pull/2"].State != "MERGED" || got[gh+"other/pull/3"].State != "OPEN" {
		t.Fatalf("got %+v", got)
	}
	if _, ok := got[gh+"other/pull/4"]; ok {
		t.Fatal("a PR the lookup no longer resolves must be dropped")
	}
	if _, ok := got[gh+"other/pull/9"]; ok || len(got) != 2 {
		t.Fatalf("PRs no bead links any more must be dropped, got %+v", got)
	}

	// A failed lookup keeps what the last pass found.
	failing := func([]string) (map[string]PR, error) { return nil, fmt.Errorf("rate limited") }
	got = nextNoted(cfg, prev, []Bead{c}, mine, inList, failing, func(string, ...any) {})
	if got[gh+"other/pull/4"].State != "OPEN" {
		t.Fatalf("got %+v", got)
	}

	// A bead whose repo listing failed this pass looks nothing up.
	calls = nil
	nextNoted(cfg, prev, []Bead{b}, map[string][]PR{}, nil, lookup, func(string, ...any) {})
	if len(calls) != 0 {
		t.Fatalf("lookups %v", calls)
	}
}
