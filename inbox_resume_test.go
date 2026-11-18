package main

import (
	"slices"
	"testing"
	"time"
)

func TestInboxReadyAfterTickerResumesAnsweredWorker(t *testing.T) {
	tk, st, _ := inboxTickerFixture(t)
	t.Setenv("FIXTURE_ACTIVE", `[{"id":"task-one","status":"needs_me"}]`)
	t.Setenv("FAKE_LOCAL_AGENTS", `[{"name":"task-one","agent_status":"working","pane_id":"w1:p1","state_change_seq":2}]`)
	th := thread(Bead{ID: "task-one", Status: StatusNeedsMe}, &Agent{Status: "idle", Seq: 1}, readyPR())
	st.PRs[th.Bead.ID] = *th.PR
	st.Threads[th.Bead.ID] = snapshot(th, Snapshot{}, t0)
	if err := tk.once(st); err != nil {
		t.Fatal(err)
	}
	if snap := st.Threads[th.Bead.ID]; snap.NeedsMe || snap.ReadyKey != "" {
		t.Fatalf("ticker resume prematurely baselined ready notice: %+v", snap)
	}
	// The first pass persisted in_progress; the next observation returns it.
	t.Setenv("FIXTURE_ACTIVE", `[{"id":"task-one","status":"in_progress"}]`)
	if err := tk.once(st); err != nil {
		t.Fatal(err)
	}
	items, _ := readInbox()
	if !slices.ContainsFunc(items, func(it InboxItem) bool { return it.Kind == EventReady }) {
		t.Fatalf("ticker-resumed worker lost ready notice: %+v", items)
	}
}

func TestInboxPromptRoundTripDoesNotRepeatExistingQuestion(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	th := Thread{Bead: Bead{ID: "task-one", Status: StatusNeedsMe, Notes: "Question"},
		Agent: &Agent{Status: "idle", Seq: 1}}
	inboxFixture(t, Event{Bead: th.Bead.ID, Kind: EventNeedsYou, At: t0, Clears: eventCondition(th, EventNeedsYou)})
	prev := snapshot(th, Snapshot{}, t0)
	th.Agent.Status, th.Prompt = "blocked", &ApprovalPrompt{Hash: "approval"}
	out := transition(th, prev, false, t0.Add(time.Second))
	for _, e := range out.Events {
		if err := writeEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	prev = snapshot(th, prev, t0.Add(time.Second))
	th.Agent.Status, th.Prompt = "idle", nil
	out = transition(th, prev, false, t0.Add(2*time.Second))
	if slices.ContainsFunc(out.Events, func(e Event) bool { return e.Kind == EventNeedsYou }) || out.Notify {
		t.Fatalf("prompt round-trip duplicated existing question notice: %+v", out)
	}
	if err := reconcileInbox([]Thread{th}, nil); err != nil {
		t.Fatal(err)
	}
	items, _ := readInbox()
	if len(items) != 1 || items[0].Clears.Kind != "needs_me" {
		t.Fatalf("lost original pending question: %+v", items)
	}
}
