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
