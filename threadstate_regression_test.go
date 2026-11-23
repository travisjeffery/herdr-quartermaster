package main

import (
	"strings"
	"testing"
)

func TestDependenciesDoNotReclassifyActiveThreads(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	for _, status := range []string{StatusOpen, StatusInProgress} {
		for _, agentStatus := range []string{"", "idle"} {
			b := Bead{ID: "task-1", Status: status, Dependencies: []Dependency{{DependsOn: "task-2", Type: "blocks"}}}
			blocker := Bead{ID: "task-2", Status: StatusOpen}
			var agents []Agent
			byName := map[string]Agent{}
			st := TickerState{Threads: map[string]Snapshot{}}
			want := GroupNoAgent
			if agentStatus != "" {
				a := Agent{Name: b.ID, Status: agentStatus}
				agents, byName[b.ID] = []Agent{a}, a
				st.Threads[b.ID] = Snapshot{AgentStatus: agentStatus}
				want = GroupIdle
			}
			if status == StatusOpen {
				want = GroupBlocked
			}
			rows := buildRows(Config{}, boardData{Beads: []Bead{b, blocker}, Agents: agents, State: st}, sortPriority)
			for _, r := range rows {
				if r.bead.ID == b.ID && r.group != want {
					t.Fatalf("%s/%s board = %s, want %s", status, agentStatus, r.group, want)
				}
			}
			d, _ := buildDigest(digestInputs{open: []Bead{b, blocker}, agents: byName, ticker: st}, digestState{}, nil, t0)
			if it := item(t, d, b.ID); it.State.group() != want || len(it.Blockers) != 1 {
				t.Fatalf("digest = %+v, want %s with dependency", it, want)
			}
			if status == StatusInProgress {
				th := threadFor(Config{}, st, b, byName)
				if classify(th) != want {
					t.Fatalf("ticker/context = %s, want %s", classify(th), want)
				}
				r := buildReport(t0, nil, []Bead{b, blocker}, nil, st, "")
				if len(r.InFlight) != 1 || r.InFlight[0].Derived.group() != want {
					t.Fatalf("report = %+v, want %s", r.InFlight, want)
				}
			}
		}
	}
}

func TestLightNotedPRCannotEstablishReviewBaseline(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	b := Bead{ID: "task-1", Status: StatusInProgress, Notes: "PR: https://github.com/example/app/pull/7"}
	light := PR{Number: 7, URL: "https://github.com/example/app/pull/7", State: "OPEN"}
	st := TickerState{Noted: map[string]PR{light.URL: light}}
	agents := map[string]Agent{b.ID: {Name: b.ID, Status: "idle", Seq: 1}}
	// The ticker deliberately omitted PRs[id] when the first detail fetch failed.
	th := threadFor(Config{}, st, b, agents)
	if th.PR != nil {
		t.Fatalf("light lookup promoted after failed detail fetch: %+v", th.PR)
	}
	prev := snapshot(th, Snapshot{}, t0)
	full := light
	full.Reviews = []Review{approvalOf("head", t0)}
	full.Reviews[0].Author.Login = "reviewer"
	st.PRs = map[string]PR{b.ID: full}
	th = threadFor(Config{}, st, b, agents)
	out := transition(th, prev, false, t0)
	for _, e := range out.Events {
		if e.Kind == EventReview {
			t.Fatalf("existing feedback announced as new: %+v", out)
		}
	}
}

func TestUnrelatedNoteDoesNotRenotifyWaitingThread(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = "Question: which option?"
	th := thread(b, agent("idle", 1), nil)
	prev := snapshot(th, Snapshot{}, t0)
	for _, note := range []string{"coordinator checked the worktree", "AUTO: [worker L4 C2] verified tests"} {
		th.Bead.Notes += "\n" + note
		if out := transition(th, prev, false, t0); out.Notify || len(out.Events) != 0 {
			t.Fatalf("unrelated note re-notified: %+v", out)
		}
		prev = snapshot(th, prev, t0)
	}
}

func TestDigestShowsCachedPRWhenLookupHasNoObservation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
		b := Bead{ID: "task-1", Status: StatusInProgress}
		p := PR{Number: 7, URL: "https://github.com/example/app/pull/7", State: state}
		st := TickerState{PRs: map[string]PR{b.ID: p}}
		d, next := buildDigest(digestInputs{open: []Bead{b}, ticker: st, branch: map[string]string{b.ID: p.URL}}, digestState{}, nil, t0)
		it := item(t, d, b.ID)
		if it.PR == nil || it.PR.URL != p.URL || it.State.PR.URL != p.URL || next.PRs[p.URL].State != state {
			t.Fatalf("fallback not shown/remembered: %+v / %+v", it, next.PRs)
		}
		if !strings.Contains(itemDetail(it), "example/app#7") {
			t.Fatalf("PR missing from detail: %s", itemDetail(it))
		}
	}
}

func TestPromptKeepsPendingCommandVisible(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = "RUN: login"
	th := thread(b, agent("blocked", 1), nil)
	if s := th.derived(); s.Waiting != WaitPrompt || s.Command != "login" || stateLine(th) != "needs you · run command" {
		t.Fatalf("prompt hid pending command: %+v", s)
	}
}

func TestExplicitBlockedOutranksPRAndWorkingAgent(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cases := []struct {
		name  string
		agent *Agent
		pr    *PR
	}{
		{"open PR with idle agent", agent("idle", 1), openPR()},
		{"ready PR with idle agent", agent("idle", 1), readyPR()},
		{"failing PR with idle agent", agent("idle", 1), openPR("build")},
		{"open PR with working agent", agent("working", 1), openPR()},
		{"working agent without PR", agent("working", 1), nil},
		{"merged PR", agent("idle", 1), &PR{Number: 7, State: "MERGED"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := Bead{ID: "task-held", Status: StatusBlocked, UpdatedAt: t0, Notes: "BLOCKED until soak ends."}
			if tc.pr != nil {
				tc.pr.Number = 7
				tc.pr.URL = "https://github.com/example/app/pull/7"
				b.Notes = "PR: " + tc.pr.URL + "\n" + b.Notes
			}
			if tc.agent != nil {
				tc.agent.Name = b.ID
			}
			th := thread(b, tc.agent, tc.pr).derive()
			if s := th.derived(); s.Kind != ThreadBlocked || classify(th) != GroupBlocked || sectionFor(s) != SecBlocked {
				t.Fatalf("explicit blocked misclassified: state=%+v group=%s", s, classify(th))
			}
			in := digestInputs{open: []Bead{b}, agents: map[string]Agent{}, branch: map[string]string{}, prs: map[string]DigestPR{}}
			st := TickerState{PRs: map[string]PR{}, Threads: map[string]Snapshot{}}
			if tc.agent != nil {
				in.agents[b.ID] = *tc.agent
				st.Threads[b.ID] = Snapshot{AgentStatus: tc.agent.Status, AgentSeq: tc.agent.Seq}
			}
			if tc.pr != nil {
				st.PRs[b.ID] = *tc.pr
				in.branch[b.ID] = tc.pr.URL
				in.prs[tc.pr.URL] = digestObservation(*tc.pr)
			}
			in.ticker = st
			d, _ := buildDigest(in, digestState{}, nil, t0)
			it := item(t, d, b.ID)
			if it.Section != SecBlocked {
				t.Fatalf("digest section = %s, want %s", it.Section, SecBlocked)
			}
			detail := itemDetail(it)
			if !strings.Contains(detail, "BLOCKED until soak ends.") {
				t.Fatalf("digest detail hid hold note: %q", detail)
			}
			if tc.pr != nil && !strings.Contains(detail, "example/app#7") {
				t.Fatalf("digest detail hid PR: %q", detail)
			}
		})
	}
}
