package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestInboxAgentConditionsSurvivePartialDiscovery(t *testing.T) {
	dir := fakeTools(t)
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	agents, complete, err := newHerdr().agentsWithNative([]Agent{{Name: "native-task", Backend: rpcBackend}})
	if err != nil || complete || len(agents) != 1 {
		t.Fatalf("native fallback did not report partial observation: %v %v %v", agents, complete, err)
	}
	th := Thread{Bead: Bead{ID: "unobserved-task", Status: StatusInProgress}, AgentUnknown: !complete}
	for _, kind := range []string{"needs_you", "agent_blocked", "prompt"} {
		it := InboxItem{Event: Event{Bead: th.Bead.ID, Kind: EventNeedsYou,
			Clears: &InboxCondition{Kind: kind, Bead: th.Bead.ID, Key: "approval"}}}
		if inboxObsolete(it, map[string]Thread{th.Bead.ID: th}) {
			t.Fatalf("partial list archived %s", kind)
		}
	}
}

func TestInboxBacklogNoticesWaitForClosedEvidence(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	for i, kind := range []EventKind{EventLikelyStale, EventLeaseExpired, EventBreaker} {
		inboxFixture(t, Event{Bead: "backlog-task", Kind: kind, At: t0.Add(time.Duration(i))})
	}
	for _, status := range []string{"unavailable", StatusOpen, StatusClosed} {
		reads := 0
		err := reconcileInbox(nil, func(id string) (Bead, error) {
			reads++
			if status == "unavailable" {
				return Bead{}, errors.New("offline")
			}
			return Bead{ID: id, Status: status}, nil
		})
		if err != nil || reads != 1 {
			t.Fatalf("status %s: reads=%d err=%v", status, reads, err)
		}
		items, _ := readInbox()
		want := 3
		if status == StatusClosed {
			want = 0
		}
		if len(items) != want {
			t.Fatalf("status %s: unread=%+v", status, items)
		}
	}
}

func TestInboxReadySurvivesGateFlickerAndRearmsAfterDecision(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	th := thread(Bead{ID: "task-one", Status: StatusInProgress}, &Agent{Status: "idle"}, readyPR())
	inboxFixture(t, Event{Bead: th.Bead.ID, Kind: EventReady, At: t0, Clears: eventCondition(th, EventReady)})
	prev := snapshot(th, Snapshot{}, t0)
	th.PR.MergeState = "UNKNOWN"
	if err := reconcileInbox([]Thread{th}, nil); err != nil {
		t.Fatal(err)
	}
	prev = snapshot(th, prev, t0.Add(time.Second))
	th.PR.MergeState = "CLEAN"
	if out := transition(th, prev, false, t0.Add(2*time.Second)); len(out.Events) != 0 {
		t.Fatalf("readiness flicker broke once-per-head dedup: %+v", out)
	}
	items, _ := readInbox()
	if len(items) != 1 {
		t.Fatal("flicker lost ready notice without a replacement")
	}
	th.Bead.Status = StatusNeedsMe
	if err := reconcileInbox([]Thread{th}, nil); err != nil {
		t.Fatal(err)
	}
	prev = snapshot(th, prev, t0.Add(3*time.Second))
	items, _ = readInbox()
	if len(items) != 0 {
		t.Fatal("human decision did not supersede ready notice")
	}
	th.Bead.Status = StatusInProgress
	out := transition(th, prev, false, t0.Add(4*time.Second))
	if !slices.ContainsFunc(out.Events, func(e Event) bool { return e.Kind == EventReady }) {
		t.Fatalf("answer did not rearm ready notice: %+v", out)
	}
}

func TestInboxAgentPromptResolutionRetainsBeadQuestion(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	th := Thread{Bead: Bead{ID: "task-one", Status: StatusNeedsMe, Notes: "Question remains"},
		Agent: &Agent{Status: "blocked"}, Prompt: &ApprovalPrompt{Hash: "approval"}}
	inboxFixture(t, Event{Bead: th.Bead.ID, Kind: EventNeedsYou, At: t0, Clears: eventCondition(th, EventNeedsYou)})
	prev := snapshot(th, Snapshot{}, t0)
	th.Agent.Status, th.Prompt = "idle", nil
	out := transition(th, prev, false, t0.Add(time.Second))
	for _, e := range out.Events {
		if err := writeEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcileInbox([]Thread{th}, nil); err != nil {
		t.Fatal(err)
	}
	items, _ := readInbox()
	if len(items) != 1 || items[0].Clears.Kind != "needs_me" {
		t.Fatalf("agent prompt resolution lost still-pending bead question: %+v", items)
	}
}

func TestInboxArchiveFailureRetainsEvidence(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	it := inboxFixture(t, Event{Bead: "task-one", Kind: EventNeedsYou, At: t0})
	if err := os.WriteFile(filepath.Join(inboxDir(), "done"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reconcileInbox(nil, nil); err == nil {
		t.Fatal("archive failure hidden")
	}
	items, _ := readInbox()
	if len(items) != 1 || items[0].File != it.File {
		t.Fatal("archive failure lost unread evidence")
	}
}

func TestOutboxCleanupFailureDoesNotHaltInboxReconciliation(t *testing.T) {
	tk, st, _ := inboxTickerFixture(t)
	st.Threads["task-one"] = Snapshot{}
	t.Setenv("FAKE_BEAD", `[{"id":"task-one","status":"closed"}]`)
	// A nonempty directory at the notice path is a portable removal failure,
	// even when the test process can override ordinary file permissions.
	if err := writeAtomic(filepath.Join(noticePath(localHost, "task-one"), "fixture"), []byte("keep")); err != nil {
		t.Fatal(err)
	}
	inboxFixture(t, Event{Bead: "task-one", Kind: EventNeedsYou, At: t0})
	if err := tk.once(st); err != nil {
		t.Fatal(err)
	}
	items, _ := readInbox()
	if len(items) != 0 {
		t.Fatal("outbox failure stopped inbox reconciliation")
	}
	if _, ok := st.Threads["task-one"]; !ok {
		t.Fatal("failed cleanup was not retained for retry")
	}
}
