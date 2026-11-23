package main

import (
	"slices"
	"testing"
	"time"
)

func TestInboxReadyAfterTickerResumesAnsweredWorker(t *testing.T) {
	tk, st, _ := inboxTickerFixture(t)
	now := time.Now()
	pr := readyPR()
	pr.URL = "https://github.com/example/app/pull/7"
	pr.Head = "tj/task-one-work"
	pr.Gate.At = now
	pr.Gate.Reviews[0].SubmittedAt = now.Add(-time.Hour)
	t.Setenv("FIXTURE_ACTIVE", `[{"id":"task-one","status":"needs_me","notes":"PR: https://github.com/example/app/pull/7"}]`)
	t.Setenv("FAKE_LOCAL_AGENTS", `[{"name":"task-one","agent_status":"working","pane_id":"w1:p1","state_change_seq":2}]`)
	th := thread(Bead{ID: "task-one", Status: StatusNeedsMe, Notes: "PR: " + pr.URL}, &Agent{Status: "idle", Seq: 1}, pr)
	st.PRs[th.Bead.ID] = *th.PR
	st.Threads[th.Bead.ID] = snapshot(th, Snapshot{}, t0)
	if err := tk.once(st); err != nil {
		t.Fatal(err)
	}
	if snap := st.Threads[th.Bead.ID]; snap.NeedsMe || snap.ReadyKey != "" {
		t.Fatalf("ticker resume prematurely baselined ready notice: %+v", snap)
	}
	// The first pass persisted in_progress; the next observation returns it.
	t.Setenv("FIXTURE_ACTIVE", `[{"id":"task-one","status":"in_progress","notes":"PR: https://github.com/example/app/pull/7"}]`)
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

func TestInboxUnrelatedNotesPreserveWaitingQuestionAcrossViews(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cases := []struct {
		name        string
		initial     string
		replacement string
	}{
		{
			name:        "free text question",
			initial:     "Question: which option?",
			replacement: "Question: which region first?",
		},
		{
			name:        "structured options question",
			initial:     "CRITICAL: gate open\nQUESTION: Pick a rollout order?\nOPTIONS:\n1. All at once\n2. Canary first\nCLASS: 7 — prod",
			replacement: "QUESTION: Proceed with phase 2?\nOPTIONS:\n1. Yes\n2. Hold\nCLASS: 7 — prod",
		},
		{
			name:        "pending run command",
			initial:     "RUN: aws sso login --profile admin\nwhy: expired",
			replacement: "RUN: gh auth login",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
			th := Thread{
				Bead:  Bead{ID: "task-one", Status: StatusNeedsMe, Notes: tc.initial},
				Agent: &Agent{Name: "task-one", Status: "idle", Seq: 1},
			}.derive()
			orig := inboxFixture(t, Event{Bead: th.Bead.ID, Kind: EventNeedsYou, At: t0, Clears: eventCondition(th, EventNeedsYou)})
			prev := snapshot(th, Snapshot{}, t0)
			for i, note := range []string{
				"coordinator checked the worktree",
				"AUTO: [worker L4 C2] verified tests; existing question still waiting?",
				"AUTO: [coordinator L4 C3] reconciled notice",
				"PR: https://github.com/example/app/pull/7",
			} {
				th.Bead.Notes += "\n" + note
				th = th.derive()
				if out := transition(th, prev, false, t0.Add(time.Duration(i+1)*time.Second)); out.Notify || len(out.Events) != 0 {
					t.Fatalf("note %q re-notified: %+v", note, out)
				}
				if err := reconcileInbox([]Thread{th}, nil); err != nil {
					t.Fatal(err)
				}
				items, _ := readInbox()
				if len(items) != 1 || items[0].File != orig.File {
					t.Fatalf("note %q archived original waiting notice: %+v", note, items)
				}
				prev = snapshot(th, prev, t0.Add(time.Duration(i+1)*time.Second))
			}
			// A genuine new question supersedes the old notice and emits a replacement.
			th.Bead.Notes += "\n" + tc.replacement
			th = th.derive()
			out := transition(th, prev, false, t0.Add(time.Minute))
			if !out.Notify || !slices.ContainsFunc(out.Events, func(e Event) bool {
				return e.Kind == EventNeedsYou && e.Clears != nil && e.Clears.Kind == "needs_me" && e.Clears.Key == asked(th.Bead)
			}) {
				t.Fatalf("new question did not emit replacement: %+v", out)
			}
			for _, e := range out.Events {
				if err := writeEvent(e); err != nil {
					t.Fatal(err)
				}
			}
			if err := reconcileInbox([]Thread{th}, nil); err != nil {
				t.Fatal(err)
			}
			items, _ := readInbox()
			if len(items) != 1 || items[0].File == orig.File || items[0].Clears.Key != asked(th.Bead) {
				t.Fatalf("expected only replacement notice, got %+v", items)
			}
		})
	}
}
