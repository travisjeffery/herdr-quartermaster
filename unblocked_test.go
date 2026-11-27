package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeWakeCall struct {
	Bead  string
	Agent Agent
	Text  string
}

type fakeBlockerEnv struct {
	beads        map[string]Bead
	showErr      map[string]error
	prs          map[string]PR
	lookupErr    error
	watches      []Watch
	remoteAgents map[string]*Agent
	level        int
	enforceErr   error
	wakeErr      error
	eventErr     error
	liveAsk      map[string]bool
	held         map[string]bool
	headless     map[string]bool
	parked       map[string]bool

	wakes       []fakeWakeCall
	notes       []string
	events      []Event
	journal     []journalRecord
	showCalls   int
	lookupCalls int
}

func newFakeBlockerEnv(level int, beads ...Bead) *fakeBlockerEnv {
	m := make(map[string]Bead, len(beads))
	for _, b := range beads {
		m[b.ID] = b
	}
	return &fakeBlockerEnv{
		beads:        m,
		showErr:      map[string]error{},
		prs:          map[string]PR{},
		remoteAgents: map[string]*Agent{},
		level:        level,
		liveAsk:      map[string]bool{},
		held:         map[string]bool{},
		headless:     map[string]bool{},
		parked:       map[string]bool{},
	}
}

func (f *fakeBlockerEnv) env() blockerWakeEnv {
	return blockerWakeEnv{
		show: func(id string) (Bead, error) {
			f.showCalls++
			if err := f.showErr[id]; err != nil {
				return Bead{}, err
			}
			b, ok := f.beads[id]
			if !ok {
				return Bead{}, fmt.Errorf("%w: %s", errBeadNotFound, id)
			}
			return b, nil
		},
		note: func(id, text string) error {
			f.notes = append(f.notes, id+": "+text)
			if b, ok := f.beads[id]; ok {
				if b.Notes != "" {
					b.Notes += "\n"
				}
				b.Notes += text
				f.beads[id] = b
			}
			return nil
		},
		lookupPRs: func(urls []string) (map[string]PR, error) {
			f.lookupCalls++
			if f.lookupErr != nil {
				return nil, f.lookupErr
			}
			out := map[string]PR{}
			for _, u := range urls {
				if p, ok := f.prs[u]; ok {
					out[u] = p
				}
			}
			return out, nil
		},
		watches: func() ([]Watch, error) {
			return f.watches, nil
		},
		agentFor: func(b Bead) (*Agent, error) {
			return f.remoteAgents[b.ID], nil
		},
		wake: func(b Bead, a Agent, text string) error {
			if f.wakeErr != nil {
				return f.wakeErr
			}
			f.wakes = append(f.wakes, fakeWakeCall{Bead: b.ID, Agent: a, Text: text})
			return nil
		},
		enforce: func(b Bead, action, by string) (string, error) {
			if f.enforceErr != nil {
				return "", f.enforceErr
			}
			return allow(b.ID, by, action, classHousekeeping, f.level, "")
		},
		event: func(e Event) error {
			if f.eventErr != nil {
				return f.eventErr
			}
			f.events = append(f.events, e)
			return nil
		},
		journal: func(r journalRecord) error {
			f.journal = append(f.journal, r)
			return nil
		},
		liveAsk: func(a *Agent) bool {
			return a != nil && f.liveAsk[a.Name]
		},
		held:     func(id string) bool { return f.held[id] },
		headless: func(id string) bool { return f.headless[id] },
		parked:   func(id string) bool { return f.parked[id] },
		logf:     func(string, ...any) {},
	}
}

func testBlockerConfig() Config {
	cfg := defaultConfig()
	cfg.Repo = "/repo"
	cfg.BranchPrefix = "topic/"
	return cfg
}

func roundTripTickerState(t *testing.T, st TickerState) TickerState {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal TickerState: %v", err)
	}
	var out TickerState
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal TickerState: %v", err)
	}
	return out
}

func TestBlockerWakeupDependencyCloseAndDedupeAcrossRestarts(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0

	blocker := Bead{ID: "task-a", Title: "Ship schema migration", Status: StatusInProgress}
	waiting := Bead{
		ID:           "task-b",
		Title:        "Build API consumer",
		Status:       StatusBlocked,
		Dependencies: []Dependency{{DependsOn: "task-a", Type: "blocks"}},
	}
	f := newFakeBlockerEnv(5, blocker, waiting)
	st := TickerState{
		Threads: map[string]Snapshot{
			"task-b": {AgentStatus: "idle", ReadySince: now.Add(-2 * time.Minute), Auto: 1},
		},
	}
	agents := map[string]Agent{
		"task-b": {Name: "task-b", Status: "idle", PaneID: "w1:p2"},
	}
	threads := []Thread{{Bead: waiting, Agent: &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p2"}}}

	// Tick 1: baseline both non-closed beads -> zero events, zero wakes.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{blocker, waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 0 || len(f.events) != 0 || len(f.notes) != 0 {
		t.Fatalf("baseline should not wake or emit: wakes=%+v events=%+v notes=%+v", f.wakes, f.events, f.notes)
	}
	if !st.Blockers.Initialized {
		t.Fatal("baseline did not mark Blockers.Initialized")
	}

	// Blocker task-a transitions to closed.
	blocker.Status = StatusClosed
	f.beads["task-a"] = blocker

	// Tick 2: task-a is no longer in nonClosed -> wake idle L5 worker once.
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	if len(f.wakes) != 1 {
		t.Fatalf("wakes = %d, want 1 (%+v)", len(f.wakes), f.wakes)
	}
	if f.wakes[0].Bead != "task-b" {
		t.Fatalf("woke %q, want task-b", f.wakes[0].Bead)
	}
	if !strings.Contains(f.wakes[0].Text, blockerWakeHeaderPrefix) || !strings.Contains(f.wakes[0].Text, "task-a") {
		t.Fatalf("wake prompt missing automated header or closed bead ID: %q", f.wakes[0].Text)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "AUTO: [ticker L5 C3]") || !strings.Contains(f.notes[0], "task-a") {
		t.Fatalf("unexpected AUTO note: %+v", f.notes)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || f.events[0].Bead != "task-b" {
		t.Fatalf("unexpected events: %+v", f.events)
	}
	if !strings.Contains(f.events[0].Summary, "task-a") || !strings.Contains(f.events[0].Summary, "woke idle worker") {
		t.Fatalf("unexpected event summary: %q", f.events[0].Summary)
	}
	if !st.Threads["task-b"].ReadySince.IsZero() {
		t.Fatal("ReadySince should be cleared after waking worker")
	}
	if st.Threads["task-b"].Auto != 2 {
		t.Fatalf("Snapshot.Auto = %d, want 2 to prevent double-journaling on next tick", st.Threads["task-b"].Auto)
	}

	// Tick 3: steady state -> no duplicate wake or event.
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 1 || len(f.events) != 1 || len(f.notes) != 1 {
		t.Fatalf("repeat tick duplicated action: wakes=%d events=%d notes=%d", len(f.wakes), len(f.events), len(f.notes))
	}

	// Simulate ticker restart via JSON round-trip of TickerState.
	restarted := roundTripTickerState(t, st)
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &restarted, []Bead{waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 1 || len(f.events) != 1 || len(f.notes) != 1 {
		t.Fatalf("restart duplicated action: wakes=%d events=%d notes=%d", len(f.wakes), len(f.events), len(f.notes))
	}
}

func TestBlockerWakeupReferencedPRMergeFromNotes(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	upstreamURL := "https://github.com/acme/app/pull/42"
	ownURL := "https://github.com/acme/app/pull/99"
	mergeSHA := "a27ceed3186e24e00b1971ae8b63eff5802e3fef"

	waiting := Bead{
		ID:     "task-stack",
		Title:  "Stacked PR waiting on base PR",
		Status: StatusInProgress,
		Notes:  "PR: " + ownURL + "\nStacked on " + upstreamURL,
	}
	f := newFakeBlockerEnv(4, waiting)
	f.prs[upstreamURL] = PR{Number: 42, URL: upstreamURL, State: "OPEN", Head: "feat/base", HeadSHA: "111111111111"}
	f.prs[ownURL] = PR{Number: 99, URL: ownURL, State: "OPEN", Head: "topic/task-stack-stacked", HeadSHA: "222222222222"}

	st := TickerState{Threads: map[string]Snapshot{}}
	agents := map[string]Agent{"task-stack": {Name: "task-stack", Status: "idle", PaneID: "w1:p3"}}
	threads := []Thread{{Bead: waiting, Agent: &Agent{Name: "task-stack", Status: "idle", PaneID: "w1:p3"}}}

	// Verify deliveryNotedPRs excludes the stacked upstream PR and keeps only own PR.
	if got := deliveryNotedPRs(waiting.Notes); len(got) != 1 || got[0] != ownURL {
		t.Fatalf("deliveryNotedPRs = %v, want [%s]", got, ownURL)
	}

	// Tick 1: baseline while upstream PR #42 is OPEN.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 0 || len(f.events) != 0 {
		t.Fatalf("baseline should emit nothing: wakes=%+v events=%+v", f.wakes, f.events)
	}
	if f.lookupCalls != 1 {
		t.Fatalf("baseline GH tick lookupCalls = %d, want 1", f.lookupCalls)
	}

	// Non-GH tick must not call lookupPRs and must keep tracking the open PR.
	now = now.Add(15 * time.Second)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, false, nil, nil, nil, nil, now)
	if f.lookupCalls != 1 {
		t.Fatalf("non-GH tick should not call lookupPRs: lookupCalls = %d, want 1", f.lookupCalls)
	}

	// Upstream PR #42 merges with mergeCommit SHA.
	f.prs[upstreamURL] = PR{
		Number:      42,
		URL:         upstreamURL,
		State:       "MERGED",
		Head:        "feat/base",
		HeadSHA:     "111111111111",
		MergeCommit: &CommitRef{OID: mergeSHA},
		MergedAt:    now.Add(30 * time.Second),
	}

	// Tick 2: referenced upstream PR #42 transitions OPEN -> MERGED.
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	if len(f.wakes) != 1 {
		t.Fatalf("wakes = %d, want 1", len(f.wakes))
	}
	prompt := f.wakes[0].Text
	if !strings.Contains(prompt, upstreamURL) || !strings.Contains(prompt, mergeSHA) {
		t.Fatalf("wake prompt missing merged PR URL or SHA: %q", prompt)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "AUTO: [ticker L4 C3]") || !strings.Contains(f.notes[0], upstreamURL) || !strings.Contains(f.notes[0], mergeSHA) {
		t.Fatalf("unexpected AUTO note: %+v", f.notes)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || !strings.Contains(f.events[0].Summary, upstreamURL) || !strings.Contains(f.events[0].Summary, mergeSHA) {
		t.Fatalf("unexpected events: %+v", f.events)
	}

	// Verify our own AUTO note quoting upstreamURL does NOT pollute deliveryNotedPRs!
	if got := deliveryNotedPRs(f.beads["task-stack"].Notes); len(got) != 1 || got[0] != ownURL {
		t.Fatalf("after AUTO note, deliveryNotedPRs = %v, want [%s]", got, ownURL)
	}

	// Own PR #99 merging must NOT trigger an unblocked event!
	f.prs[ownURL] = PR{Number: 99, URL: ownURL, State: "MERGED", Head: "topic/task-stack-stacked", HeadSHA: "222222222222"}
	now = now.Add(time.Minute)
	restarted := roundTripTickerState(t, st)
	blockerWakeupPass(cfg, f.env(), &restarted, []Bead{f.beads["task-stack"]}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 1 || len(f.events) != 1 {
		t.Fatalf("own PR merge or restart caused spurious unblocked: wakes=%+v events=%+v", f.wakes, f.events)
	}
}

func TestBlockerWakeupReferencedPRMergeFromWatchAndHeadSHAFallback(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	prURLStr := "https://github.com/acme/lib/pull/77"
	headSHA := "deadbeef1234567890abcdef"

	waiting := Bead{ID: "task-watch", Title: "Consume upstream fix", Status: StatusInProgress}
	f := newFakeBlockerEnv(3, waiting)
	f.watches = []Watch{{
		ID:     "w1",
		Bead:   "task-watch",
		Kind:   WatchKindGHPR,
		Status: WatchStatusActive,
		Params: WatchParams{GHPR: &GHPRWatchParams{Repo: "acme/lib", PR: 77, Expect: "merged"}},
	}}
	f.prs[prURLStr] = PR{Number: 77, URL: prURLStr, State: "OPEN", HeadSHA: headSHA}

	st := TickerState{}
	agents := map[string]Agent{"task-watch": {Name: "task-watch", Status: "done", PaneID: "w1:p4"}}
	threads := []Thread{{Bead: waiting, Agent: &Agent{Name: "task-watch", Status: "done", PaneID: "w1:p4"}}}

	// Tick 1: baseline active watch PR as OPEN.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 0 || len(f.events) != 0 {
		t.Fatalf("baseline should not emit: wakes=%+v events=%+v", f.wakes, f.events)
	}

	// Watch triggered (inbox mode) and PR merged with only HeadSHA populated.
	f.watches[0].Status = WatchStatusTriggered
	f.prs[prURLStr] = PR{Number: 77, URL: prURLStr, State: "MERGED", HeadSHA: headSHA}

	// Even on a non-GH tick, watchMergedPRs triggers the lookup for the watched PR.
	now = now.Add(15 * time.Second)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, false, nil, nil, nil, map[string]bool{prURLStr: true}, now)
	if len(f.wakes) != 1 {
		t.Fatalf("wakes = %d, want 1", len(f.wakes))
	}
	if !strings.Contains(f.wakes[0].Text, prURLStr) || !strings.Contains(f.wakes[0].Text, headSHA) {
		t.Fatalf("wake prompt missing watch PR URL or fallback HeadSHA: %q", f.wakes[0].Text)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "AUTO: [ticker L3 C3]") {
		t.Fatalf("unexpected notes: %+v", f.notes)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked {
		t.Fatalf("unexpected events: %+v", f.events)
	}
}

func TestBlockerWakeupDoesNotDoubleWakeWatchWakeTrigger(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	prURLStr := "https://github.com/acme/lib/pull/88"

	waiting := Bead{ID: "task-w", Status: StatusInProgress}
	f := newFakeBlockerEnv(5, waiting)
	f.watches = []Watch{{
		ID:        "w2",
		Bead:      "task-w",
		Kind:      WatchKindGHPR,
		OnTrigger: WatchOnTriggerWake,
		Status:    WatchStatusActive,
		Params:    WatchParams{GHPR: &GHPRWatchParams{Repo: "acme/lib", PR: 88, Expect: "merged"}},
	}}
	f.prs[prURLStr] = PR{Number: 88, URL: prURLStr, State: "OPEN"}
	st := TickerState{}
	agents := map[string]Agent{"task-w": {Name: "task-w", Status: "idle", PaneID: "w1:p5"}}

	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	f.watches[0].Status = WatchStatusTriggered
	f.prs[prURLStr] = PR{Number: 88, URL: prURLStr, State: "MERGED", HeadSHA: "cafebabe"}
	watchWoken := map[string]map[string]bool{"task-w": {prURLStr: true}}

	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, watchWoken, map[string]bool{prURLStr: true}, now)
	if len(f.wakes) != 0 {
		t.Fatalf("should not double-wake worker already woken by gh-pr watch: %+v", f.wakes)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || !strings.Contains(f.events[0].Summary, "already woken by gh-pr watch") {
		t.Fatalf("unexpected events: %+v", f.events)
	}
}

func TestBlockerWakeupNeverWakesWorkingOrNeedsMeOrUnsafeStates(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		agent      *Agent
		prompt     *ApprovalPrompt
		level      int
		liveAsk    bool
		held       bool
		headless   bool
		parked     bool
		relaunch   bool
		prompted   bool
		wantReason string
	}{
		{
			name:       "working agent",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "working", PaneID: "w1:p1"},
			level:      5,
			wantReason: "worker is working",
		},
		{
			name:       "needs_me bead with idle agent",
			status:     StatusNeedsMe,
			agent:      &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"},
			level:      5,
			wantReason: "bead is needs_me",
		},
		{
			name:       "blocked agent at prompt",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "blocked", PaneID: "w1:p1"},
			level:      5,
			wantReason: "approval prompt",
		},
		{
			name:       "idle pi worker with live ask",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"},
			level:      5,
			liveAsk:    true,
			wantReason: "approval prompt",
		},
		{
			name:       "autonomy below class 3",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"},
			level:      2,
			wantReason: "class 3",
		},
		{
			name:       "held bead",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"},
			level:      5,
			held:       true,
			wantReason: "on hold",
		},
		{
			name:       "headless bead",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"},
			level:      5,
			headless:   true,
			wantReason: "headless",
		},
		{
			name:       "parked rpc worker",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Backend: rpcBackend, Status: "idle", PaneID: "rpc:task-b"},
			level:      5,
			parked:     true,
			wantReason: "parked",
		},
		{
			name:       "relaunching worker",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"},
			level:      5,
			relaunch:   true,
			wantReason: "relaunch",
		},
		{
			name:       "already prompted this tick",
			status:     StatusInProgress,
			agent:      &Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"},
			level:      5,
			prompted:   true,
			wantReason: "already prompted this tick",
		},
		{
			name:       "no live worker on active bead",
			status:     StatusInProgress,
			agent:      nil,
			level:      5,
			wantReason: "no worker running",
		},
		{
			name:       "open backlog bead",
			status:     StatusOpen,
			agent:      nil,
			level:      5,
			wantReason: "bead is open",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testBlockerConfig()
			now := t0
			blocker := Bead{ID: "task-a", Status: StatusInProgress}
			waiting := Bead{
				ID:           "task-b",
				Status:       tc.status,
				Dependencies: []Dependency{{DependsOn: "task-a", Type: "blocks"}},
			}
			f := newFakeBlockerEnv(tc.level, blocker, waiting)
			if tc.liveAsk {
				f.liveAsk["task-b"] = true
			}
			if tc.held {
				f.held["task-b"] = true
			}
			if tc.headless {
				f.headless["task-b"] = true
			}
			if tc.parked {
				f.parked["task-b"] = true
			}
			agents := map[string]Agent{}
			var threads []Thread
			if tc.agent != nil {
				agents["task-b"] = *tc.agent
				threads = append(threads, Thread{Bead: waiting, Agent: tc.agent, Prompt: tc.prompt})
			}
			relaunch := map[string]bool{}
			if tc.relaunch {
				relaunch["task-b"] = true
			}
			prompted := map[string]bool{}
			if tc.prompted {
				prompted["task-b"] = true
			}

			st := TickerState{}
			blockerWakeupPass(cfg, f.env(), &st, []Bead{blocker, waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)

			blocker.Status = StatusClosed
			f.beads["task-a"] = blocker
			now = now.Add(time.Minute)
			blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, true, prompted, relaunch, nil, nil, now)

			if len(f.wakes) != 0 {
				t.Fatalf("must never wake in case %q: got %+v", tc.name, f.wakes)
			}
			if len(f.notes) != 0 {
				t.Fatalf("must not write AUTO note when not woken: %+v", f.notes)
			}
			if len(f.events) != 1 || f.events[0].Kind != EventUnblocked {
				t.Fatalf("expected 1 unblocked inbox event, got %+v", f.events)
			}
			if !strings.Contains(f.events[0].Summary, tc.wantReason) {
				t.Fatalf("summary %q does not mention %q", f.events[0].Summary, tc.wantReason)
			}

			// Even if the agent later becomes idle on the next tick, dedupe prevents duplicate wakes/events.
			agents["task-b"] = Agent{Name: "task-b", Status: "idle", PaneID: "w1:p1"}
			now = now.Add(time.Minute)
			restarted := roundTripTickerState(t, st)
			blockerWakeupPass(cfg, f.env(), &restarted, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
			if len(f.wakes) != 0 || len(f.events) != 1 {
				t.Fatalf("dedupe failed on follow-up tick: wakes=%+v events=%+v", f.wakes, f.events)
			}
		})
	}
}

func TestBlockerWakeupLiveReReadPreventsConcurrentNeedsMeWake(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	blocker := Bead{ID: "task-a", Status: StatusInProgress}
	staleWaiting := Bead{
		ID:           "task-b",
		Status:       StatusInProgress,
		Dependencies: []Dependency{{DependsOn: "task-a", Type: "blocks"}},
	}
	f := newFakeBlockerEnv(5, blocker, staleWaiting)
	agents := map[string]Agent{"task-b": {Name: "task-b", Status: "idle", PaneID: "w1:p1"}}
	st := TickerState{}

	blockerWakeupPass(cfg, f.env(), &st, []Bead{blocker, staleWaiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	// task-a closed, AND concurrently task-b flipped to needs_me in bd (though
	// allBeads snapshot still shows in_progress).
	blocker.Status = StatusClosed
	f.beads["task-a"] = blocker
	freshNeedsMe := staleWaiting
	freshNeedsMe.Status = StatusNeedsMe
	f.beads["task-b"] = freshNeedsMe

	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{staleWaiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 0 {
		t.Fatalf("live re-read should have prevented waking concurrent needs_me bead: %+v", f.wakes)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || !strings.Contains(f.events[0].Summary, "bead is needs_me") {
		t.Fatalf("unexpected events: %+v", f.events)
	}
}

func TestBlockerWakeupBaselineIgnoresAlreadyClosedAndHistoricalMergedPR(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	oldURL := "https://github.com/acme/app/pull/5"
	waiting := Bead{
		ID:           "task-b",
		Status:       StatusInProgress,
		Notes:        "Waiting on " + oldURL,
		Dependencies: []Dependency{{DependsOn: "task-old", Type: "blocks"}},
	}
	f := newFakeBlockerEnv(5, waiting, Bead{ID: "task-old", Status: StatusClosed})
	f.prs[oldURL] = PR{Number: 5, URL: oldURL, State: "MERGED", HeadSHA: "1234abcd"}
	agents := map[string]Agent{"task-b": {Name: "task-b", Status: "idle", PaneID: "w1:p1"}}

	st := TickerState{}
	// Tick 1: baseline.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	// Tick 2: steady state — already-closed dep and already-merged PR must never fire.
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	if len(f.wakes) != 0 || len(f.events) != 0 {
		t.Fatalf("historical blockers should never fire: wakes=%+v events=%+v", f.wakes, f.events)
	}
}

func TestBlockerWakeupCoalescesMultipleClearedBlockersAndRetriesTransientErrors(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	prURLStr := "https://github.com/acme/app/pull/10"
	dep1 := Bead{ID: "task-1", Title: "Dep one", Status: StatusInProgress}
	dep2 := Bead{ID: "task-2", Title: "Dep two", Status: StatusInProgress}
	waiting := Bead{
		ID:     "task-b",
		Status: StatusInProgress,
		Notes:  "Blocked on " + prURLStr,
		Dependencies: []Dependency{
			{DependsOn: "task-1", Type: "blocks"},
			{DependsOn: "task-2", Type: "blocks"},
			{DependsOn: "epic-parent", Type: "parent-child"}, // non-blocking edge
		},
	}
	f := newFakeBlockerEnv(5, dep1, dep2, waiting)
	f.prs[prURLStr] = PR{Number: 10, URL: prURLStr, State: "OPEN", HeadSHA: "1111"}
	agents := map[string]Agent{"task-b": {Name: "task-b", Status: "idle", PaneID: "w1:p1"}}
	st := TickerState{}

	// Tick 1: baseline.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{dep1, dep2, waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	// Both dep1 closes and PR #10 merges, while dep2 stays open — but bd show and gh lookup transiently fail!
	dep1.Status = StatusClosed
	f.beads["task-1"] = dep1
	f.prs[prURLStr] = PR{Number: 10, URL: prURLStr, State: "MERGED", MergeCommit: &CommitRef{OID: "beef1234"}}
	f.showErr["task-1"] = errors.New("bd database busy")
	f.lookupErr = errors.New("gh timeout")

	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{dep2, waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 0 || len(f.events) != 0 {
		t.Fatalf("transient errors must not fire or drop tracked blockers: wakes=%+v events=%+v", f.wakes, f.events)
	}

	// Next tick: errors clear -> both dep1 and PR #10 coalesce into ONE wake prompt and ONE event, noting remaining blocker task-2!
	delete(f.showErr, "task-1")
	f.lookupErr = nil
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{dep2, waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	if len(f.wakes) != 1 {
		t.Fatalf("wakes = %d, want 1 coalesced wake", len(f.wakes))
	}
	if !strings.Contains(f.wakes[0].Text, "task-1") || !strings.Contains(f.wakes[0].Text, prURLStr) || !strings.Contains(f.wakes[0].Text, "beef1234") || !strings.Contains(f.wakes[0].Text, "task-2") {
		t.Fatalf("coalesced prompt missing expected details: %q", f.wakes[0].Text)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || !strings.Contains(f.events[0].Summary, "remaining blockers: task-2") {
		t.Fatalf("unexpected coalesced event: %+v", f.events)
	}
}

func TestBlockerWakeupOtherBeadOwnedPRWithoutKeyword(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	prA := "https://github.com/acme/app/pull/50"

	beadA := Bead{ID: "task-a", Status: StatusInProgress, Notes: "PR: " + prA}
	beadB := Bead{ID: "task-b", Status: StatusInProgress, Notes: "see " + prA}
	f := newFakeBlockerEnv(5, beadA, beadB)
	f.prs[prA] = PR{Number: 50, URL: prA, State: "OPEN", Head: "topic/task-a-base", HeadSHA: "aaa111"}
	agents := map[string]Agent{"task-b": {Name: "task-b", Status: "idle", PaneID: "w1:p2"}}
	st := TickerState{}

	// Tick 1: baseline while beadA owns prA and prA is OPEN.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{beadA, beadB}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	// beadA closes and prA merges simultaneously.
	f.prs[prA] = PR{Number: 50, URL: prA, State: "MERGED", Head: "topic/task-a-base", MergeCommit: &CommitRef{OID: "fedcba98"}}
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{beadB}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	if len(f.wakes) != 1 || !strings.Contains(f.wakes[0].Text, prA) || !strings.Contains(f.wakes[0].Text, "fedcba98") {
		t.Fatalf("expected wakeup for other bead's merged PR: %+v", f.wakes)
	}
}

func TestBlockerWakeupInboxReconciliationForOpenBead(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	ev := Event{
		Bead:    "task-open",
		Kind:    EventUnblocked,
		Summary: "task-open unblocked (blocking bead task-a closed); worker not woken (bead is open); run `quartermaster dispatch task-open`",
		At:      t0,
		Clears:  &InboxCondition{Kind: "bead_closed", Bead: "task-open", Key: "dep:task-a"},
	}
	inboxFixture(t, ev)

	// While task-open is still open (not in active threads), reconcileInbox keeps the item.
	show := func(id string) (Bead, error) {
		return Bead{ID: id, Status: StatusOpen}, nil
	}
	if err := reconcileInbox(nil, show); err != nil {
		t.Fatal(err)
	}
	items, err := readInbox()
	if err != nil || len(items) != 1 || items[0].Kind != EventUnblocked {
		t.Fatalf("unblocked event on open bead should stay unread, got %+v (err=%v)", items, err)
	}

	// Once task-open closes, reconcileInbox archives the item automatically.
	showClosed := func(id string) (Bead, error) {
		return Bead{ID: id, Status: StatusClosed}, nil
	}
	if err := reconcileInbox(nil, showClosed); err != nil {
		t.Fatal(err)
	}
	items, err = readInbox()
	if err != nil || len(items) != 0 {
		t.Fatalf("unblocked event should archive once bead closes, got %+v (err=%v)", items, err)
	}
}

func TestBlockerWakeupEndToEndTickerOnceAndRestart(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("PATH", dir)
	stateRoot := t.TempDir()
	t.Setenv("QUARTERMASTER_STATE_DIR", stateRoot)

	// Fake bd supporting list and show via env vars (PATH is restricted to dir).
	bdScript := `#!/bin/sh
case "$1" in
  list) printf '%s\n' "${FIXTURE_LIST:-[]}" ;;
  show)
    case "$2" in
      task-a) printf '%s\n' "$FIXTURE_SHOW_A" ;;
      task-b) printf '%s\n' "$FIXTURE_SHOW_B" ;;
      *) printf '[]\n' ;;
    esac
    ;;
  note) exit 0 ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(bdScript), 0o700); err != nil {
		t.Fatal(err)
	}
	herdrScript := `#!/bin/sh
if [ "$1" = "agent" ] && [ "$2" = "list" ]; then
  printf '{"result":{"agents":[{"name":"task-b","agent":"pi","agent_status":"idle","pane_id":"w1:p2","state_change_seq":1}]}}\n'
  exit 0
fi
printf '{"result":{}}\n'
`
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(herdrScript), 0o700); err != nil {
		t.Fatal(err)
	}

	depA := Bead{ID: "task-a", Title: "Prerequisite", Status: StatusInProgress}
	beadB := Bead{
		ID:           "task-b",
		Title:        "Dependent work",
		Status:       StatusInProgress,
		Dependencies: []Dependency{{DependsOn: "task-a", Type: "blocks"}},
	}
	mustJSON := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Setenv("FIXTURE_LIST", mustJSON([]Bead{depA, beadB}))
	t.Setenv("FIXTURE_SHOW_A", mustJSON([]Bead{depA}))
	t.Setenv("FIXTURE_SHOW_B", mustJSON([]Bead{beadB}))

	now := t0
	cfg := testBlockerConfig()
	cfg.Nudge = false
	cfg.VerifyMinutes = 0
	cfg.OrphanFinish.IntervalSeconds = 0
	cfg.Autonomy.Level = 5

	f := newFakeBlockerEnv(5, depA, beadB)
	env := f.env()
	env.event = writeEvent

	tk := newTicker(cfg, log.New(io.Discard, "", 0))
	tk.now = func() time.Time { return now }
	tk.blockerEnv = &env

	// Tick 1: baseline and persist state.json.
	st := loadState()
	st.LastGH = now.Add(time.Hour)
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := saveState(st); err != nil {
		t.Fatalf("saveState tick 1: %v", err)
	}
	if len(f.wakes) != 0 {
		t.Fatalf("tick 1 baseline should not wake: %+v", f.wakes)
	}

	// Close task-a and simulate a fresh ticker process reloading state.json.
	depA.Status = StatusClosed
	f.beads["task-a"] = depA
	t.Setenv("FIXTURE_LIST", mustJSON([]Bead{beadB}))
	t.Setenv("FIXTURE_SHOW_A", mustJSON([]Bead{depA}))

	now = now.Add(time.Minute)
	st2 := loadState()
	if err := tk.once(&st2); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if err := saveState(st2); err != nil {
		t.Fatalf("saveState tick 2: %v", err)
	}
	if len(f.wakes) != 1 || f.wakes[0].Bead != "task-b" {
		t.Fatalf("tick 2 expected 1 wake for task-b, got %+v", f.wakes)
	}
	items, err := readInbox()
	if err != nil {
		t.Fatal(err)
	}
	unblockedCount := 0
	for _, it := range items {
		if it.Kind == EventUnblocked && it.Bead == "task-b" {
			unblockedCount++
		}
	}
	if unblockedCount != 1 {
		t.Fatalf("expected 1 unblocked inbox event on disk, got %d (%+v)", unblockedCount, items)
	}

	// Tick 3 after another restart: reloaded state.json deduplicates cleanly.
	now = now.Add(time.Minute)
	st3 := loadState()
	if err := tk.once(&st3); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if len(f.wakes) != 1 {
		t.Fatalf("tick 3 after restart should not repeat wake, got %d", len(f.wakes))
	}
	items, err = readInbox()
	if err != nil {
		t.Fatal(err)
	}
	unblockedCount = 0
	for _, it := range items {
		if it.Kind == EventUnblocked && it.Bead == "task-b" {
			unblockedCount++
		}
	}
	if unblockedCount != 1 {
		t.Fatalf("tick 3 after restart duplicated inbox item: count=%d", unblockedCount)
	}
}

func TestBlockerPRParsingEdgeCases(t *testing.T) {
	u1 := "https://github.com/acme/app/pull/1"
	u2 := "https://github.com/acme/app/pull/2"
	u3 := "https://github.com/acme/app/pull/3"

	cases := []struct {
		name     string
		notes    string
		wantDel  []string
		wantRefs []string
	}{
		{
			name:     "own PR only",
			notes:    "PR: " + u1,
			wantDel:  []string{u1},
			wantRefs: nil,
		},
		{
			name:     "own PR plus waiting PR",
			notes:    "PR: " + u1 + "\nWaiting on PR: " + u2,
			wantDel:  []string{u1},
			wantRefs: []string{u2},
		},
		{
			name:     "blocked by PR only",
			notes:    "Blocked by " + u2 + " before rebase",
			wantDel:  nil,
			wantRefs: []string{u2},
		},
		{
			name:     "rebase after PR lands",
			notes:    "Rebase after " + u2 + " lands",
			wantDel:  nil,
			wantRefs: []string{u2},
		},
		{
			name:     "own PR opened after fixing tests is not a blocker",
			notes:    "Opened " + u1 + " after fixing tests\n" + u3 + " needs review",
			wantDel:  []string{u1, u3},
			wantRefs: nil,
		},
		{
			name:     "upstream PR and own second PR",
			notes:    "PR: " + u1 + "\nUpstream PR: " + u2 + "\nlater PR " + u3,
			wantDel:  []string{u1, u3},
			wantRefs: []string{u2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := Bead{ID: "task-x", Status: StatusInProgress, Notes: tc.notes}
			gotDel := deliveryNotedPRs(b.Notes)
			if strings.Join(gotDel, ",") != strings.Join(tc.wantDel, ",") {
				t.Errorf("deliveryNotedPRs = %v, want %v", gotDel, tc.wantDel)
			}
			gotRefs := referencedPRURLs(b, nil, "topic/", nil, nil, nil)
			if strings.Join(gotRefs, ",") != strings.Join(tc.wantRefs, ",") {
				t.Errorf("referencedPRURLs = %v, want %v", gotRefs, tc.wantRefs)
			}
		})
	}
}

func TestBlockerWakeupWakeFailureSurfacesAndDeduplicates(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	blocker := Bead{ID: "task-a", Status: StatusInProgress}
	waiting := Bead{
		ID:           "task-b",
		Status:       StatusInProgress,
		Dependencies: []Dependency{{DependsOn: "task-a", Type: "blocks"}},
	}
	f := newFakeBlockerEnv(5, blocker, waiting)
	f.wakeErr = errors.New("pane gone")
	agents := map[string]Agent{"task-b": {Name: "task-b", Status: "idle", PaneID: "w1:p2"}}
	st := TickerState{}

	blockerWakeupPass(cfg, f.env(), &st, []Bead{blocker, waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	blocker.Status = StatusClosed
	f.beads["task-a"] = blocker
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	if len(f.notes) != 0 {
		t.Fatalf("failed wake must not leave AUTO note: %+v", f.notes)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || !strings.Contains(f.events[0].Summary, "worker wake failed (pane gone)") {
		t.Fatalf("expected wake_failed inbox event, got %+v", f.events)
	}
	if st.Blockers.Beads["task-b"].Cleared["dep:task-a"].Action != blockerActionWakeFailed {
		t.Fatalf("unexpected mark: %+v", st.Blockers.Beads["task-b"].Cleared)
	}

	// Next tick after restart must not retry infinitely.
	f.wakeErr = nil
	restarted := roundTripTickerState(t, st)
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &restarted, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 0 || len(f.events) != 1 {
		t.Fatalf("failed wake should stay deduplicated: wakes=%+v events=%+v", f.wakes, f.events)
	}
}

func TestBlockerLookupParsesMergeCommitAndMergedSHA(t *testing.T) {
	out := []byte(`{"data":{
		"p0":{"pullRequest":{"number":12,"url":"https://github.com/acme/app/pull/12","headRefName":"feat/x","state":"MERGED","headRefOid":"1111aaaa","mergeCommit":{"oid":"9999ffff"}}},
		"p1":{"pullRequest":{"number":13,"url":"https://github.com/acme/app/pull/13","headRefName":"feat/y","state":"MERGED","headRefOid":"2222bbbb","mergeCommit":null}}
	}}`)
	alias := map[string]string{
		"p0": "https://github.com/acme/app/pull/12",
		"p1": "https://github.com/acme/app/pull/13",
	}
	prs := map[string]PR{}
	if err := parseLookup(out, alias, prs); err != nil {
		t.Fatalf("parseLookup: %v", err)
	}
	if got := prs[alias["p0"]].mergedSHA(); got != "9999ffff" {
		t.Fatalf("p0 mergedSHA = %q, want 9999ffff", got)
	}
	if got := prs[alias["p1"]].mergedSHA(); got != "2222bbbb" {
		t.Fatalf("p1 mergedSHA = %q, want 2222bbbb", got)
	}
}

func TestBlockerWakeupReopenedDependencyFiresAgainOnSecondClose(t *testing.T) {
	cfg := testBlockerConfig()
	now := t0
	blocker := Bead{ID: "task-a", Status: StatusInProgress}
	waiting := Bead{
		ID:           "task-b",
		Status:       StatusInProgress,
		Dependencies: []Dependency{{DependsOn: "task-a", Type: "blocks"}},
	}
	f := newFakeBlockerEnv(5, blocker, waiting)
	agents := map[string]Agent{"task-b": {Name: "task-b", Status: "idle", PaneID: "w1:p1"}}
	st := TickerState{}

	// Baseline -> close #1 -> wakes once.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{blocker, waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	blocker.Status = StatusClosed
	f.beads["task-a"] = blocker
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 1 {
		t.Fatalf("close #1 wakes = %d, want 1", len(f.wakes))
	}

	// Reopen task-a -> observed live again.
	blocker.Status = StatusInProgress
	f.beads["task-a"] = blocker
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{blocker, waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 1 {
		t.Fatalf("reopen should not wake: wakes = %d", len(f.wakes))
	}

	// Close #2 -> wakes a second time!
	blocker.Status = StatusClosed
	f.beads["task-a"] = blocker
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, nil, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 2 {
		t.Fatalf("close #2 wakes = %d, want 2", len(f.wakes))
	}
}

func TestChainedRolloutAutoDispatchAutonomyDialAndDedupe(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0

	cfg := testBlockerConfig()
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Dev = []string{"dev*", "latest"}
	cfg.Autonomy.Max = 7

	runCase := func(t *testing.T, target string, level int, agentsComplete bool, extraDep bool, wantDispatched bool, wantSummarySub string) {
		t.Helper()
		c := cfg
		c.Autonomy.Level = level
		impl := Bead{
			ID:     "impl-1",
			Title:  "Implement retry backoff",
			Status: StatusInProgress,
			Notes:  "PR: https://github.com/acme/app/pull/77",
		}
		deps := []Dependency{{DependsOn: "impl-1", Type: "blocks"}}
		var allLive []Bead
		allLive = append(allLive, impl)
		if extraDep {
			dep2 := Bead{ID: "impl-2", Title: "Other prereq", Status: StatusInProgress}
			deps = append(deps, Dependency{DependsOn: "impl-2", Type: "blocks"})
			allLive = append(allLive, dep2)
		}
		roll := Bead{
			ID:           "roll-1",
			Title:        "Rollout retry backoff to " + target,
			Status:       StatusOpen,
			Labels:       []string{"role:rollout", "rollout-target:" + target},
			Dependencies: deps,
		}
		allLive = append(allLive, roll)

		f := newFakeBlockerEnv(level, allLive...)
		var dispatched []string
		env := f.env()
		env.enforceRollout = func(b Bead, tgt, by string) (string, int, error) {
			cls := max(actionClasses["dispatch"], c.Autonomy.targetClass(tgt))
			prefix, err := allow(b.ID, by, fmt.Sprintf("chained rollout dispatch (%s)", tgt), cls, level, "")
			return prefix, cls, err
		}
		env.dispatchRollout = func(b Bead, tgt string) (string, error) {
			dispatched = append(dispatched, b.ID+":"+tgt)
			return "started pi on " + b.ID + " (role rollout)", nil
		}

		st := TickerState{}
		// Tick 1: baseline.
		blockerWakeupPass(c, env, &st, allLive, nil, map[string]Agent{}, agentsComplete, nil, nil, true, nil, nil, nil, nil, now)
		if len(dispatched) != 0 || len(f.events) != 0 {
			t.Fatalf("baseline should not dispatch or emit: dispatched=%v events=%+v", dispatched, f.events)
		}

		// Close impl-1.
		impl.Status = StatusClosed
		f.beads["impl-1"] = impl
		nextLive := []Bead{roll}
		if extraDep {
			nextLive = append(nextLive, f.beads["impl-2"])
		}

		// Tick 2: transition detected.
		t2 := now.Add(time.Minute)
		blockerWakeupPass(c, env, &st, nextLive, nil, map[string]Agent{}, agentsComplete, nil, nil, true, nil, nil, nil, nil, t2)
		if wantDispatched {
			if len(dispatched) != 1 || dispatched[0] != "roll-1:"+target {
				t.Fatalf("dispatched = %v, want [roll-1:%s]", dispatched, target)
			}
			// Upstream PR URL should be noted on the rollout bead alongside AUTO note.
			joinedNotes := strings.Join(f.notes, "\n")
			if !strings.Contains(joinedNotes, "https://github.com/acme/app/pull/77") || !strings.Contains(joinedNotes, "AUTO: [ticker L") {
				t.Fatalf("expected upstream PR and AUTO note on rollout bead, got:\n%s", joinedNotes)
			}
		} else if len(dispatched) != 0 {
			t.Fatalf("unexpected dispatch: %v", dispatched)
		}
		if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || !strings.Contains(f.events[0].Summary, wantSummarySub) {
			t.Fatalf("events = %+v, want 1 unblocked containing %q", f.events, wantSummarySub)
		}

		// Tick 3 + simulated restart: must NEVER duplicate dispatch or inbox event!
		restarted := roundTripTickerState(t, st)
		blockerWakeupPass(c, env, &restarted, nextLive, nil, map[string]Agent{}, agentsComplete, nil, nil, true, nil, nil, nil, nil, t2.Add(time.Minute))
		if wantDispatched && len(dispatched) != 1 {
			t.Fatalf("restart duplicated rollout dispatch: %v", dispatched)
		}
		if len(f.events) != 1 {
			t.Fatalf("restart duplicated unblocked event: %+v", f.events)
		}
	}

	t.Run("dev target auto-dispatches at L5", func(t *testing.T) {
		runCase(t, "dev", 5, true, false, true, "auto-dispatched chained rollout worker (target dev)")
	})
	t.Run("latest dev target auto-dispatches at L5", func(t *testing.T) {
		runCase(t, "latest", 5, true, false, true, "auto-dispatched chained rollout worker (target latest)")
	})
	t.Run("dev target blocked at L4 surfaces unblocked_dispatch", func(t *testing.T) {
		runCase(t, "dev", 4, true, false, false, "class 5")
	})
	t.Run("staging target blocked at L5", func(t *testing.T) {
		runCase(t, "staging", 5, true, false, false, "class 6")
	})
	t.Run("staging target auto-dispatches at L6", func(t *testing.T) {
		runCase(t, "staging", 6, true, false, true, "auto-dispatched chained rollout worker (target staging)")
	})
	t.Run("prod target blocked at L6", func(t *testing.T) {
		runCase(t, "prod", 6, true, false, false, "class 7")
	})
	t.Run("incomplete agent discovery fails closed", func(t *testing.T) {
		runCase(t, "dev", 5, false, false, false, "worker discovery incomplete")
	})
	t.Run("remaining blocking dep prevents premature auto-dispatch", func(t *testing.T) {
		runCase(t, "dev", 5, true, true, false, "remaining blockers: impl-2")
	})
}

func TestUnblockedDispatchInboxClearsOnceDispatched(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	it := InboxItem{
		Event: Event{
			Bead:    "roll-1",
			Kind:    EventUnblocked,
			Summary: "roll-1 unblocked; run quartermaster dispatch roll-1 --role rollout",
			At:      t0,
			Clears:  &InboxCondition{Kind: "unblocked_dispatch", Bead: "roll-1", Key: "dep:impl-1"},
		},
	}
	// Open with no agent: stays unread.
	if inboxObsolete(it, map[string]Thread{"roll-1": {Bead: Bead{ID: "roll-1", Status: StatusOpen}}}) {
		t.Fatal("open unblocked rollout bead should stay unread")
	}
	// Once dispatched (in_progress with agent): auto-clears cleanly.
	if !inboxObsolete(it, map[string]Thread{"roll-1": {Bead: Bead{ID: "roll-1", Status: StatusInProgress}, Agent: &Agent{Name: "roll-1", Status: "working"}}}) {
		t.Fatal("dispatched rollout bead should archive stale unblocked_dispatch event")
	}
	// Closed: also clears.
	if !inboxObsolete(it, map[string]Thread{"roll-1": {Bead: Bead{ID: "roll-1", Status: StatusClosed}}}) {
		t.Fatal("closed rollout bead should archive unblocked_dispatch event")
	}
}

func TestBlockerWakeupResourceLockSkipsAndDispatchesOnClose(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0

	cfg := testBlockerConfig()
	cfg.Autonomy.Level = 5
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Dev = []string{"dev*"}
	cfg.Autonomy.Max = 7

	impl := Bead{
		ID:     "impl-1",
		Title:  "Ship schema migration",
		Status: StatusInProgress,
		Notes:  "PR: https://github.com/acme/app/pull/88",
	}
	holder := Bead{
		ID:     "holder-1",
		Title:  "Upgrade staging cluster",
		Status: StatusInProgress,
		Labels: []string{"lock:staging-cluster"},
	}
	roll := Bead{
		ID:           "roll-1",
		Title:        "Rollout schema migration to dev",
		Status:       StatusOpen,
		Labels:       []string{"role:rollout", "rollout-target:dev", "lock:staging-cluster"},
		Dependencies: []Dependency{{DependsOn: "impl-1", Type: "blocks"}},
	}

	f := newFakeBlockerEnv(5, impl, holder, roll)
	var dispatched []string
	env := f.env()
	env.enforceRollout = func(b Bead, tgt, by string) (string, int, error) {
		cls := max(actionClasses["dispatch"], cfg.Autonomy.targetClass(tgt))
		prefix, err := allow(b.ID, by, fmt.Sprintf("chained rollout dispatch (%s)", tgt), cls, 5, "")
		return prefix, cls, err
	}
	env.dispatchRollout = func(b Bead, tgt string) (string, error) {
		dispatched = append(dispatched, b.ID+":"+tgt)
		return "started pi on " + b.ID, nil
	}

	st := TickerState{}
	// Tick 1: baseline.
	blockerWakeupPass(cfg, env, &st, []Bead{impl, holder, roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 0 || len(f.events) != 0 {
		t.Fatalf("baseline emitted unexpected actions: dispatched=%v events=%+v", dispatched, f.events)
	}

	// Tick 2: impl-1 closes while holder-1 STILL holds lock:staging-cluster.
	impl.Status = StatusClosed
	f.beads["impl-1"] = impl
	now = now.Add(time.Minute)
	lines := blockerWakeupPass(cfg, env, &st, []Bead{holder, roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 0 || len(f.events) != 0 || len(f.notes) != 0 {
		t.Fatalf("locked bead should be skipped cleanly: dispatched=%v events=%+v notes=%+v", dispatched, f.events, f.notes)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "skipped while locked") || !strings.Contains(lines[0], "holder-1") {
		t.Fatalf("expected single lock-skip log line, got %+v", lines)
	}

	// Tick 3: holder-1 still holds lock:staging-cluster -> zero spam, zero extra show calls.
	showsBefore := f.showCalls
	now = now.Add(time.Minute)
	lines = blockerWakeupPass(cfg, env, &st, []Bead{holder, roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 0 || len(f.events) != 0 || len(lines) != 0 || f.showCalls != showsBefore {
		t.Fatalf("repeat tick while locked should emit no spam: dispatched=%v events=%+v lines=%+v shows=%d->%d", dispatched, f.events, lines, showsBefore, f.showCalls)
	}

	// Tick 4 (after ticker restart): holder-1 closes -> roll-1 auto-dispatches immediately.
	holder.Status = StatusClosed
	f.beads["holder-1"] = holder
	restarted := roundTripTickerState(t, st)
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, env, &restarted, []Bead{roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 1 || dispatched[0] != "roll-1:dev" {
		t.Fatalf("expected roll-1:dev auto-dispatch after holder closed, got %v", dispatched)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventUnblocked || !strings.Contains(f.events[0].Summary, "auto-dispatched") {
		t.Fatalf("expected one unblocked auto-dispatch event, got %+v", f.events)
	}
	joinedNotes := strings.Join(f.notes, "\n")
	if !strings.Contains(joinedNotes, "https://github.com/acme/app/pull/88") || !strings.Contains(joinedNotes, "lock:staging-cluster") {
		t.Fatalf("expected deferred upstream PR and lock release in notes:\n%s", joinedNotes)
	}

	// Tick 5 + restart: no duplicate dispatch or event.
	restarted2 := roundTripTickerState(t, restarted)
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, env, &restarted2, []Bead{roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 1 || len(f.events) != 1 {
		t.Fatalf("steady state duplicated actions: dispatched=%v events=%+v", dispatched, f.events)
	}
}

func TestBlockerWakeupResourceLockDispatchesWhenLabelRemoved(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0

	cfg := testBlockerConfig()
	cfg.Autonomy.Level = 5
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Dev = []string{"dev*"}
	cfg.Autonomy.Max = 7

	holder := Bead{
		ID:     "holder-1",
		Title:  "Toggle CI workflow",
		Status: StatusInProgress,
		Labels: []string{"lock:ci-workflow-toggle"},
	}
	roll := Bead{
		ID:     "roll-2",
		Title:  "Rollout CI change to dev",
		Status: StatusOpen,
		Labels: []string{"role:rollout", "rollout-target:dev", "lock:ci-workflow-toggle"},
	}
	f := newFakeBlockerEnv(5, holder, roll)
	var dispatched []string
	env := f.env()
	env.enforceRollout = func(b Bead, tgt, by string) (string, int, error) {
		cls := max(actionClasses["dispatch"], cfg.Autonomy.targetClass(tgt))
		prefix, err := allow(b.ID, by, fmt.Sprintf("chained rollout dispatch (%s)", tgt), cls, 5, "")
		return prefix, cls, err
	}
	env.dispatchRollout = func(b Bead, tgt string) (string, error) {
		dispatched = append(dispatched, b.ID+":"+tgt)
		return "started pi on " + b.ID, nil
	}

	st := TickerState{}
	// Tick 1: baseline while holder-1 holds lock:ci-workflow-toggle.
	blockerWakeupPass(cfg, env, &st, []Bead{holder, roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 0 || len(f.events) != 0 {
		t.Fatalf("baseline should emit nothing: dispatched=%v events=%+v", dispatched, f.events)
	}

	// Tick 2: still held -> silent skip.
	now = now.Add(time.Minute)
	lines := blockerWakeupPass(cfg, env, &st, []Bead{holder, roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 0 || len(f.events) != 0 || len(lines) != 0 {
		t.Fatalf("still-locked tick should be silent: dispatched=%v events=%+v lines=%+v", dispatched, f.events, lines)
	}

	// Tick 3: holder-1 stays in_progress but removes lock:ci-workflow-toggle.
	holder.Labels = nil
	f.beads["holder-1"] = holder
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, env, &st, []Bead{holder, roll}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 1 || dispatched[0] != "roll-2:dev" {
		t.Fatalf("expected roll-2:dev dispatched once lock label removed, got %v", dispatched)
	}
	if len(f.events) != 1 || !strings.Contains(f.events[0].Summary, "lock:ci-workflow-toggle released by holder-1") {
		t.Fatalf("unexpected events: %+v", f.events)
	}
}

func TestBlockerWakeupResourceLockSerializesSameTickDispatches(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0

	cfg := testBlockerConfig()
	cfg.Autonomy.Level = 5
	cfg.Autonomy.Prod = []string{"*prod*"}
	cfg.Autonomy.Dev = []string{"dev*"}
	cfg.Autonomy.Max = 7

	impl := Bead{ID: "impl-1", Title: "Shared prereq", Status: StatusInProgress}
	rollA := Bead{
		ID:           "roll-a",
		Title:        "First staging rollout",
		Status:       StatusOpen,
		Labels:       []string{"role:rollout", "rollout-target:dev", "lock:staging-cluster"},
		Dependencies: []Dependency{{DependsOn: "impl-1", Type: "blocks"}},
	}
	rollB := Bead{
		ID:           "roll-b",
		Title:        "Second staging rollout",
		Status:       StatusOpen,
		Labels:       []string{"role:rollout", "rollout-target:dev", "lock:staging-cluster"},
		Dependencies: []Dependency{{DependsOn: "impl-1", Type: "blocks"}},
	}

	f := newFakeBlockerEnv(5, impl, rollA, rollB)
	var dispatched []string
	env := f.env()
	env.enforceRollout = func(b Bead, tgt, by string) (string, int, error) {
		cls := max(actionClasses["dispatch"], cfg.Autonomy.targetClass(tgt))
		prefix, err := allow(b.ID, by, fmt.Sprintf("chained rollout dispatch (%s)", tgt), cls, 5, "")
		return prefix, cls, err
	}
	env.dispatchRollout = func(b Bead, tgt string) (string, error) {
		dispatched = append(dispatched, b.ID)
		b.Status = StatusInProgress
		f.beads[b.ID] = b
		return "started pi on " + b.ID, nil
	}

	st := TickerState{}
	// Tick 1: baseline.
	blockerWakeupPass(cfg, env, &st, []Bead{impl, rollA, rollB}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)

	// Tick 2: impl-1 closes -> roll-a dispatches; roll-b is immediately serialized and skipped cleanly in the same tick.
	impl.Status = StatusClosed
	f.beads["impl-1"] = impl
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, env, &st, []Bead{rollA, rollB}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 1 || dispatched[0] != "roll-a" {
		t.Fatalf("expected only roll-a dispatched on tick 2, got %v", dispatched)
	}
	if len(f.events) != 1 || f.events[0].Bead != "roll-a" {
		t.Fatalf("expected only roll-a event on tick 2, got %+v", f.events)
	}

	// Tick 3: roll-a is in_progress -> roll-b stays skipped with zero spam.
	now = now.Add(time.Minute)
	lines := blockerWakeupPass(cfg, env, &st, []Bead{f.beads["roll-a"], rollB}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 1 || len(f.events) != 1 || len(lines) != 0 {
		t.Fatalf("expected silent wait while roll-a holds lock: dispatched=%v events=%+v lines=%+v", dispatched, f.events, lines)
	}

	// Tick 4: roll-a closes -> roll-b now dispatches automatically.
	rollAClosed := f.beads["roll-a"]
	rollAClosed.Status = StatusClosed
	f.beads["roll-a"] = rollAClosed
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, env, &st, []Bead{rollB}, nil, map[string]Agent{}, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(dispatched) != 2 || dispatched[1] != "roll-b" {
		t.Fatalf("expected roll-b dispatched after roll-a closed, got %v", dispatched)
	}
}

func TestBlockerWakeupResourceLockMultipleLocksAndBlockedWorker(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := t0
	cfg := testBlockerConfig()

	holderA := Bead{ID: "holder-a", Status: StatusInProgress, Labels: []string{"lock:cluster-a"}}
	holderB := Bead{ID: "holder-b", Status: StatusInProgress, Labels: []string{"lock:cluster-b"}}
	waiting := Bead{
		ID:     "task-blocked",
		Title:  "Run cross-cluster sync",
		Status: StatusBlocked,
		Labels: []string{"lock:cluster-a", "lock:cluster-b"},
	}
	f := newFakeBlockerEnv(5, holderA, holderB, waiting)
	agents := map[string]Agent{"task-blocked": {Name: "task-blocked", Status: "idle", PaneID: "w1:p5"}}
	threads := []Thread{{Bead: waiting, Agent: &Agent{Name: "task-blocked", Status: "idle", PaneID: "w1:p5"}}}
	st := TickerState{}

	// Tick 1: baseline both locks held.
	blockerWakeupPass(cfg, f.env(), &st, []Bead{holderA, holderB, waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)

	// Tick 2: holder-a closes, but holder-b still holds lock:cluster-b -> must NOT wake yet!
	holderA.Status = StatusClosed
	f.beads["holder-a"] = holderA
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{holderB, waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 0 || len(f.events) != 0 {
		t.Fatalf("must not wake while lock:cluster-b is still held: wakes=%+v events=%+v", f.wakes, f.events)
	}

	// Tick 3: holder-b closes -> both locks clear, idle worker woken once!
	holderB.Status = StatusClosed
	f.beads["holder-b"] = holderB
	now = now.Add(time.Minute)
	blockerWakeupPass(cfg, f.env(), &st, []Bead{waiting}, threads, agents, true, nil, nil, true, nil, nil, nil, nil, now)
	if len(f.wakes) != 1 || f.wakes[0].Bead != "task-blocked" || !strings.Contains(f.wakes[0].Text, "lock:cluster-b") {
		t.Fatalf("expected worker wake once final lock cleared, got %+v", f.wakes)
	}
}
