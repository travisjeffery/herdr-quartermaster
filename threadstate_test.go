package main

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// This is the single exhaustive precedence matrix for non-closed beads. Each
// row lists outcomes for: no PR, OPEN, ready OPEN, failing OPEN, MERGED, CLOSED.
// Every row runs both with and without rolling-out and through every view.
func TestThreadStateMatrixAndViews(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	rows := []struct{ bead, agent, want string }{
		{StatusInProgress, "", "N N R F M N"},
		{StatusInProgress, "working", "W W R F M W"},
		{StatusInProgress, "idle", "I D R F M I"},
		{StatusInProgress, "done", "I D R F M I"},
		{StatusInProgress, "blocked", "P P P P P P"},
		{StatusInProgress, "unknown", "I I R F M I"},
		{StatusInProgress, "background", "W W R F M W"},
		{StatusNeedsMe, "", "U U U U U U"},
		{StatusNeedsMe, "working", "W W R F M W"},
		{StatusNeedsMe, "idle", "U U U U U U"},
		{StatusNeedsMe, "done", "U U U U U U"},
		{StatusNeedsMe, "blocked", "P P P P P P"},
		{StatusNeedsMe, "unknown", "U U U U U U"},
		{StatusNeedsMe, "background", "U U U U U U"},
		{StatusBlocked, "", "B B B B B B"},
		{StatusBlocked, "working", "B B B B B B"},
		{StatusBlocked, "idle", "B B B B B B"},
		{StatusBlocked, "done", "B B B B B B"},
		{StatusBlocked, "blocked", "P P P P P P"},
		{StatusBlocked, "unknown", "B B B B B B"},
		{StatusBlocked, "background", "B B B B B B"},
		{StatusOpen, "", "Q Q Q Q Q Q"},
		{StatusOpen, "working", "W W R F M W"},
		{StatusOpen, "idle", "Q Q Q Q Q Q"},
		{StatusOpen, "done", "Q Q Q Q Q Q"},
		{StatusOpen, "blocked", "P P P P P P"},
		{StatusOpen, "unknown", "Q Q Q Q Q Q"},
		{StatusOpen, "background", "W W R F M W"},
	}
	types := map[string]struct {
		kind    ThreadKind
		gate    DeliveryGate
		waiting WaitOn
		group   Group
		section Section
	}{
		"N": {kind: ThreadNoAgent, group: GroupNoAgent, section: SecInProgress},
		"W": {kind: ThreadWorking, group: GroupWorking, section: SecInProgress},
		"I": {kind: ThreadIdle, group: GroupIdle, section: SecInProgress},
		"D": {kind: ThreadDelivering, gate: GateReview, group: GroupReview, section: SecWaiting},
		"R": {kind: ThreadDelivering, gate: GateReady, group: GroupReady, section: SecWaiting},
		"F": {kind: ThreadDelivering, gate: GateFailing, group: GroupFailing, section: SecWaiting},
		"M": {kind: ThreadMerged, group: GroupMerged, section: SecMerged},
		"B": {kind: ThreadBlocked, group: GroupBlocked, section: SecBlocked},
		"Q": {kind: ThreadQueued, group: sectionBacklog, section: SecBacklog},
		"P": {kind: ThreadWaiting, waiting: WaitPrompt, group: GroupNeedsYou, section: SecNeedsYou},
		"U": {kind: ThreadWaiting, waiting: WaitUser, group: GroupNeedsYou, section: SecNeedsYou},
	}
	for _, row := range rows {
		for col, code := range strings.Fields(row.want) {
			for _, rolling := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/pr%d/rollout=%v", row.bead, row.agent, col, rolling), func(t *testing.T) {
					b := Bead{ID: "task-1", Status: row.bead, UpdatedAt: t0}
					if rolling {
						b.Labels = []string{LabelRollingOut}
					}
					prs := []*PR{nil, openPR(), readyPR(), openPR("build"), {State: "MERGED"}, {State: "CLOSED"}}
					p := prs[col]
					if p != nil {
						p.Number, p.URL = 7, "https://github.com/example/app/pull/7"
						b.Notes = "PR: " + p.URL
					}
					var a *Agent
					if row.agent != "" {
						a = &Agent{Name: b.ID, Status: row.agent, Seq: 4}
						if row.agent == "background" {
							a.Status = "working"
						}
					}
					th := thread(b, a, p)
					th.Unprompted = row.agent == "background" && b.Status == StatusNeedsMe
					th = th.derive()
					want := types[code]
					if code == "M" && rolling {
						want.group, want.section = GroupRollingOut, SecRollingOut
					}
					s := th.derived()
					if s.Kind != want.kind || s.Gate != want.gate || s.Waiting != want.waiting {
						t.Fatalf("state = %+v, want %+v", s, want)
					}
					if classify(th) != want.group || sectionFor(s) != want.section {
						t.Fatalf("adapters: group=%v section=%v; want %+v", classify(th), sectionFor(s), want)
					}
					assertThreadViews(t, th, want.group, want.section)
				})
			}
		}
	}
}

func assertThreadViews(t *testing.T, th Thread, group Group, section Section) {
	t.Helper()
	b := th.Bead
	st := TickerState{PRs: map[string]PR{}, Threads: map[string]Snapshot{}}
	var agents []Agent
	byName := map[string]Agent{}
	snap := Snapshot{Unprompted: th.Unprompted, Asked: asked(b)}
	if th.Agent != nil {
		agents = append(agents, *th.Agent)
		byName[th.Agent.Name] = *th.Agent
		snap.AgentStatus, snap.AgentSeq = th.Agent.Status, th.Agent.Seq
	}
	st.Threads[b.ID] = snap
	in := digestInputs{open: []Bead{b}, agents: byName, branch: map[string]string{}, prs: map[string]DigestPR{}}
	if th.PR != nil {
		st.PRs[b.ID] = *th.PR
		in.branch[b.ID], in.prs[th.PR.URL] = th.PR.URL, digestObservation(*th.PR)
	}
	in.ticker = st
	rows := buildRows(Config{}, boardData{Beads: []Bead{b}, Agents: agents, State: st}, sortPriority)
	if len(rows) != 2 || rows[1].group != group || rows[1].state.Kind != th.derived().Kind {
		t.Fatalf("board disagrees: %+v, want %s", rows, group)
	}
	// A slow bd show must not overwrite the row's state with stale status.
	stale := b
	stale.Status = StatusNeedsMe
	detail := strings.Join(texts(detailLines(rows[1], &beadDetail{full: &stale}, 100)), "\n")
	if strings.Contains(detail, "\nWaiting on you\n") != (group == GroupNeedsYou) {
		t.Fatalf("detail disagrees: %s", detail)
	}
	d, _ := buildDigest(in, digestState{}, nil, t0)
	it := item(t, d, b.ID)
	if it.Section != section || it.State.Kind != th.derived().Kind || it.State.Waiting != th.derived().Waiting {
		t.Fatalf("digest disagrees: %+v, want %+v", it, th.derived())
	}
	r := buildReport(t0, nil, []Bead{b}, nil, st, "topic/")
	if (len(r.NeedsYou) == 1) != (group == GroupNeedsYou) {
		t.Fatalf("report needs-you disagrees: %+v", r)
	}
	if group != GroupNeedsYou && th.derived().Kind != ThreadQueued {
		if len(r.InFlight) != 1 || r.InFlight[0].Derived.Kind != th.derived().Kind || r.InFlight[0].State != stateLine(th) {
			t.Fatalf("report state disagrees: %+v, want %s", r.InFlight, stateLine(th))
		}
	}
	if th.derived().Kind != ThreadQueued {
		out := renderContext(Config{}, []Thread{th}, nil, nil, nil, st, nil, t0)
		if group == GroupNoAgent {
			if !strings.Contains(out, "1 no agent") || !strings.Contains(out, "claimed, no agent (1)") {
				t.Fatalf("context summary disagrees: %s", out)
			}
		} else if !strings.Contains(out, stateLine(th)) {
			t.Fatalf("context lacks shared state %q", stateLine(th))
		}
	}
}

func TestThreadWaitingReasonsAndDisplayAge(t *testing.T) {
	th := thread(bead(StatusNeedsMe), agent("idle", 1), nil)
	th.Bead.Notes = "RUN: login\nwhy: interactive"
	th.Bead.UpdatedAt = t0.Add(-staleNeedsYou)
	s := th.derived()
	if s.Waiting != WaitCommand || s.Command != "login" || s.stale(t0) || !s.stale(t0.Add(time.Second)) {
		t.Fatalf("command/age = %+v", s)
	}
	th.Unprompted, th.Agent.Status = true, "working"
	if stateLine(th) != "needs you · run command" {
		t.Fatalf("background work hid question: %s", stateLine(th))
	}
	th.Unprompted = false
	if th.derived().Waiting != "" || runCommand(th) != "" {
		t.Fatal("answered command is still offered")
	}
	for _, status := range []string{StatusOpen, StatusInProgress, StatusBlocked} {
		th.Bead.Status = status
		if th.derived().Waiting != "" {
			t.Fatal("historical RUN became a new question")
		}
	}
	if (ThreadState{Kind: ThreadWaiting}).stale(t0) {
		t.Fatal("missing timestamp is not evidence of staleness")
	}
}

func TestBackgroundWakeSurvivesViewsUntilObservationChanges(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = "Question: proceed?"
	th := thread(b, agent("working", 3), nil)
	th.Unprompted = true
	prev := snapshot(th, Snapshot{Asked: asked(b)}, t0)
	st := TickerState{Threads: map[string]Snapshot{b.ID: prev}}
	for _, tc := range []struct {
		name  string
		bead  Bead
		agent Agent
		want  bool
	}{
		{"same observation", b, *th.Agent, true},
		{"new turn", b, *agent("working", 4), false},
		{"settled", b, *agent("idle", 4), false},
		{"answered status", bead(StatusInProgress), *th.Agent, false},
		{"new question", bead(StatusNeedsMe), *th.Agent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := threadFor(Config{}, st, tc.bead, map[string]Agent{agentName(b.ID): tc.agent})
			if got.Unprompted != tc.want {
				t.Fatalf("Unprompted=%v, want %v", got.Unprompted, tc.want)
			}
		})
	}
}

func TestOnePRSelectorAcrossAdapters(t *testing.T) {
	b := Bead{ID: "task-1", Status: StatusInProgress, Notes: "PR: https://github.com/example/app/pull/2\nPR: https://github.com/example/app/pull/3"}
	own := *readyPR()
	own.Number, own.URL, own.Head = 1, "https://github.com/example/app/pull/1", "topic/task-1-work"
	older := PR{Number: 2, URL: "https://github.com/example/app/pull/2", State: "MERGED", MergedAt: t0.Add(-time.Hour)}
	newer := PR{Number: 3, URL: "https://github.com/example/app/pull/3", State: "MERGED", MergedAt: t0}
	for _, tc := range []struct {
		name  string
		state string
		want  int
	}{{"own open wins", "OPEN", 1}, {"newest merge wins", "MERGED", 3}, {"closed loses to merged", "CLOSED", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			own.State = tc.state
			byURL := map[string]PR{older.URL: older, newer.URL: newer}
			selected, merged, ok := prForBead(b, "topic/", []PR{own}, byURL, nil)
			if !ok || selected.Number != tc.want {
				t.Fatalf("ticker pick %+v", selected)
			}
			st := TickerState{PRs: map[string]PR{b.ID: selected}, MergedPRs: map[string][]PR{b.ID: merged}, Noted: byURL}
			cached := threadFor(Config{}, st, b, nil)
			if !reflect.DeepEqual(cached.PR, &selected) || !reflect.DeepEqual(cached.Merged, merged) {
				t.Fatalf("cached picker lost metadata/order: %+v vs %+v", cached, selected)
			}
			light := map[string]DigestPR{}
			for _, p := range []PR{own, older, newer} {
				light[p.URL] = digestObservation(p)
			}
			pick, count := pickPR(beadPRURLs(b, map[string]string{b.ID: own.URL}), light)
			if pick == nil || pick.Number != tc.want || count != boolInt(tc.state == "OPEN") {
				t.Fatalf("digest pick %+v (%d)", pick, count)
			}
		})
	}
	// Closed and duplicate links use the same ranking/count, not a fourth picker.
	closed := PR{URL: own.URL, State: "CLOSED"}
	s := selectPR([]PR{closed, closed})
	if s.PR == nil || s.PR.State != "CLOSED" || s.Open != 0 {
		t.Fatalf("closed selection %+v", s)
	}
	if s := selectPR([]PR{*readyPR(), *openPR()}); s.Open != 2 {
		t.Fatalf("URL-less fixture PRs must stay distinct: %+v", s)
	}
	// Another thread's PR in notes is just a reference, not delivery.
	if p, _, ok := prForBead(b, "topic/", nil, map[string]PR{newer.URL: newer}, func(PR) string { return "task-2" }); ok {
		t.Fatalf("another thread's delivery selected: %+v", p)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestPRSelectionKeepsSourceOrderAndLatestMerge(t *testing.T) {
	prs := []PR{{Number: 3, State: "OPEN", URL: "3"}, {Number: 1, State: "OPEN", URL: "1"}, {Number: 2, State: "MERGED", URL: "2", MergedAt: t0}}
	s := selectPR(append(prs, prs[0]))
	if s.PR.Number != 3 || s.Open != 2 || len(s.Merged) != 1 || s.Merged[0].Number != 2 {
		t.Fatalf("selection %+v", s)
	}
	if !slices.EqualFunc(prs, []PR{{Number: 3}, {Number: 1}, {Number: 2}}, func(a, b PR) bool { return a.Number == b.Number }) {
		t.Fatal("selector reordered caller input")
	}
}
