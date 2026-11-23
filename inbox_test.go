package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func inboxFixture(t *testing.T, e Event) InboxItem {
	t.Helper()
	if err := writeEvent(e); err != nil {
		t.Fatal(err)
	}
	items, err := readInbox()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.At.Equal(e.At) && it.Bead == e.Bead && it.Kind == e.Kind {
			return it
		}
	}
	t.Fatal("event not written")
	return InboxItem{}
}

func inboxTickerFixture(t *testing.T) (Ticker, *TickerState, string) {
	t.Helper()
	dir := fakeTools(t)
	// Even if a code path is accidentally added, no real external executable
	// or notifier can be used by these ticker passes.
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\ncase \"$1\" in list) printf '%s\\n' \"${FIXTURE_ACTIVE:-[]}\" ;; show) printf '%s\\n' \"$FAKE_BEAD\" ;; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Nudge, cfg.VerifyMinutes = false, 0
	tk := newTicker(cfg, log.New(io.Discard, "", 0))
	st := loadState()
	st.LastGH = time.Now().Add(time.Hour) // all GitHub inputs are fixture data
	return tk, &st, dir
}

func TestInboxTickerArchivesAnsweredNeedsYou(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			tk, st, _ := inboxTickerFixture(t)
			t.Setenv("FIXTURE_ACTIVE", `[{"id":"task-one","status":"in_progress"}]`)
			th := Thread{Bead: Bead{ID: "task-one", Status: StatusNeedsMe, Notes: "Question?"}}
			it := inboxFixture(t, Event{Bead: th.Bead.ID, Kind: EventNeedsYou, At: t0, Clears: eventCondition(th, EventNeedsYou)})
			path := filepath.Join(inboxDir(), it.File)
			if legacy {
				it.Clears = nil
				if err := rpcSave(path, it.Event); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			st.Threads[th.Bead.ID] = Snapshot{NeedsMe: true, Group: GroupNeedsYou}
			if err := tk.once(st); err != nil {
				t.Fatal(err)
			}
			items, err := readInbox()
			if err != nil || len(items) != 0 {
				t.Fatalf("answered notice still unread: %+v, %v", items, err)
			}
			after, err := os.ReadFile(filepath.Join(inboxDir(), "done", it.File))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("archive lost original evidence: %v", err)
			}
			if !coordinatorEventExists(fmt.Sprintf("%019d", t0.UnixNano()), t0) {
				t.Fatal("archival broke coordinator acknowledgement reconciliation")
			}
			if err := tk.once(st); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInboxConditionLifecycle(t *testing.T) {
	base := Thread{Bead: Bead{ID: "task-one", Status: StatusInProgress}, Agent: &Agent{Status: "idle"}}
	cases := []struct {
		name          string
		kind          EventKind
		before, after func(*Thread)
		obsolete      bool
	}{
		{"question answered", EventNeedsYou, func(th *Thread) { th.Bead.Status = StatusNeedsMe }, func(th *Thread) { th.Bead.Status = StatusInProgress }, true},
		{"same question waking", EventNeedsYou, func(th *Thread) { th.Bead.Status = StatusNeedsMe }, func(th *Thread) { th.Agent.Status = "working"; th.Unprompted = true }, false},
		{"question replaced", EventNeedsYou, func(th *Thread) { th.Bead.Status = StatusNeedsMe; th.Bead.Notes = "first" }, func(th *Thread) { th.Bead.Notes = "second" }, true},
		{"prompt replaced", EventNeedsYou, func(th *Thread) { th.Agent.Status = "blocked"; th.Prompt = &ApprovalPrompt{Hash: "first"} }, func(th *Thread) { th.Prompt = &ApprovalPrompt{Hash: "second"} }, true},
		{"prompt unreadable", EventNeedsYou, func(th *Thread) { th.Agent.Status = "blocked"; th.Prompt = &ApprovalPrompt{Hash: "first"} }, func(th *Thread) { th.Prompt = nil }, false},
		{"prompt answered", EventNeedsYou, func(th *Thread) { th.Agent.Status = "blocked"; th.Prompt = &ApprovalPrompt{Hash: "first"} }, func(th *Thread) { th.Agent.Status = "working"; th.Prompt = nil }, true},
		{"blocked agent answered", EventNeedsYou, func(th *Thread) { th.Agent.Status = "blocked" }, func(th *Thread) { th.Agent.Status = "working" }, true},
		{"agent resumed", EventAgentGone, func(th *Thread) { th.Agent = nil }, func(th *Thread) { th.Agent = &Agent{Status: "working"} }, true},
		{"agent still absent", EventAgentGone, func(th *Thread) { th.Agent = nil }, func(th *Thread) {}, false},
		{"checks recovered", EventFailing, func(th *Thread) { th.PR = openPR("test"); th.Checks.Failing = []string{"test"} }, func(th *Thread) { th.Checks.Failing = nil }, true},
		{"checks same head", EventFailing, func(th *Thread) { th.PR = openPR("test"); th.Checks.Failing = []string{"test"} }, func(th *Thread) {}, false},
		{"checks head replaced", EventFailing, func(th *Thread) { th.PR = openPR("test"); th.Checks.Failing = []string{"test"} }, func(th *Thread) { th.PR.HeadSHA = "new" }, true},
		{"checks unknown", EventFailing, func(th *Thread) { th.PR = openPR("test"); th.Checks.Failing = []string{"test"} }, func(th *Thread) { th.PR = nil }, false},
		{"ready merged", EventReady, func(th *Thread) { th.PR = readyPR() }, func(th *Thread) { th.PR.State = "MERGED" }, true},
		{"ready transient gate", EventReady, func(th *Thread) { th.PR = readyPR() }, func(th *Thread) { th.PR.MergeState = "UNKNOWN" }, false},
		{"ready same head", EventReady, func(th *Thread) { th.PR = readyPR() }, func(th *Thread) {}, false},
		{"ready waiting on user", EventReady, func(th *Thread) { th.PR = readyPR() }, func(th *Thread) { th.Bead.Status = StatusNeedsMe }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			th := base
			th.Agent = &Agent{Status: "idle"}
			tc.before(&th)
			it := InboxItem{Event: Event{Bead: th.Bead.ID, Kind: tc.kind, Clears: eventCondition(th, tc.kind)}}
			tc.after(&th)
			if got := inboxObsolete(it, map[string]Thread{th.Bead.ID: th}); got != tc.obsolete {
				t.Fatalf("obsolete=%v want %v, condition=%+v", got, tc.obsolete, it.Clears)
			}
		})
	}
}

func TestInboxSupersededConditionsProduceReplacementEvents(t *testing.T) {
	for _, kind := range []EventKind{EventNeedsYou, EventFailing} {
		t.Run(string(kind), func(t *testing.T) {
			th := thread(bead(StatusInProgress), agent("idle", 1), openPR("test"))
			th.PR.HeadSHA = "old"
			if kind == EventNeedsYou {
				th.Bead.Status, th.Bead.Notes, th.PR = StatusNeedsMe, "first question", nil
			}
			prev := snapshot(th, Snapshot{}, t0)
			if kind == EventNeedsYou {
				th.Bead.Notes = "second question"
			} else {
				th.PR.HeadSHA = "new"
			}
			out := transition(th, prev, false, t0.Add(time.Second))
			if !slices.ContainsFunc(out.Events, func(e Event) bool {
				return e.Kind == kind && e.Clears != nil && e.Clears.Key == eventCondition(th, kind).Key
			}) {
				t.Fatalf("replacement condition not announced: %+v", out.Events)
			}
		})
	}
}

func TestInboxDepartureAndGlobalDismissal(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	for i, kind := range []EventKind{EventNeedsYou, EventReview, EventMerged, EventReady, EventFinished, EventLikelyStale, EventLeaseExpired, "closed", EventAutoClosed} {
		inboxFixture(t, Event{Bead: "departed", Kind: kind, At: t0.Add(time.Duration(i))})
	}
	global := inboxFixture(t, Event{Kind: EventLeaseExpired, At: t0.Add(time.Minute)})
	if err := reconcileInbox(nil, func(id string) (Bead, error) { return Bead{ID: id, Status: StatusClosed}, nil }); err != nil {
		t.Fatal(err)
	}
	items, _ := readInbox()
	if len(items) != 3 {
		t.Fatalf("want only closed/auto-closed/global notices, got %+v", items)
	}
	if n, err := inboxDone([]string{global.File}); err != nil || n != 1 {
		t.Fatalf("global notice exact dismissal: %d, %v", n, err)
	}
	items, _ = readInbox()
	if len(items) != 2 {
		t.Fatalf("global dismissal also handled unrelated items: %+v", items)
	}
	if n, err := inboxDoneFiles(nil); err != nil || n != 0 {
		t.Fatalf("empty exact-file selection handled items: %d, %v", n, err)
	}
	if _, err := inboxDoneFiles([]string{"../state.json"}); err == nil {
		t.Fatal("accepted a path instead of a filename")
	}
}

func TestInboxRetainsHeadlessOwnedEvidence(t *testing.T) {
	f := newHeadlessFixture(t)
	it := inboxFixture(t, Event{Bead: f.b.ID, Kind: EventNeedsYou, At: f.now})
	if err := reconcileInbox(nil, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := inboxDoneFiles([]string{it.File}); err != nil || n != 0 {
		t.Fatalf("dismissed verifier-owned event: %d, %v", n, err)
	}
	items, _ := readInbox()
	if len(items) != 1 {
		t.Fatal("general expiry consumed verifier evidence")
	}
}

func TestInboxRetainedWhenGatherFails(t *testing.T) {
	tk, st, dir := inboxTickerFixture(t)
	it := inboxFixture(t, Event{Bead: "task-one", Kind: EventNeedsYou, At: t0})
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := tk.once(st); err == nil {
		t.Fatal("gather failure hidden")
	}
	items, _ := readInbox()
	if len(items) != 1 || items[0].File != it.File {
		t.Fatal("failed observation cleared inbox")
	}
}

func TestLeftActiveClearsOnlyDepartedOutbox(t *testing.T) {
	fakeTools(t)
	t.Setenv("FAKE_BEAD", `[{"id":"task-one","status":"closed"}]`)
	for _, host := range []string{localHost, "server", "removed-host"} {
		for _, id := range []string{"task-one", "task-two"} {
			if err := queueBriefOn(host, id, "brief"); err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(noticePath(host, id), []byte("level notice")); err != nil {
				t.Fatal(err)
			}
		}
	}
	tk := newTicker(hostsConfig(), log.New(io.Discard, "", 0))
	st := loadState()
	st.Threads["task-one"] = Snapshot{}
	tk.leftActive(&st, "task-one", Snapshot{}, nil, t0)
	for _, host := range []string{localHost, "server", "removed-host"} {
		for _, suffix := range []string{".md", ".autonomy"} {
			if _, err := os.Stat(filepath.Join(outboxDirOn(host), "task-one"+suffix)); !os.IsNotExist(err) {
				t.Fatalf("departed queue retained: %s %s, %v", host, suffix, err)
			}
			if _, err := os.Stat(filepath.Join(outboxDirOn(host), "task-two"+suffix)); err != nil {
				t.Fatalf("other agent queue removed: %v", err)
			}
		}
	}
}

func TestTickerDropsDepartedOutboxBeforeDelivery(t *testing.T) {
	tk, st, dir := inboxTickerFixture(t)
	t.Setenv("FAKE_BEAD", `[{"id":"task-one","status":"closed"}]`)
	t.Setenv("FAKE_LOCAL_AGENTS", `[{"name":"task-one","agent_status":"idle","pane_id":"w1:p1"}]`)
	if err := queueBrief("task-one", "obsolete brief"); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(noticePath(localHost, "task-one"), []byte("obsolete notice")); err != nil {
		t.Fatal(err)
	}
	st.Threads["task-one"] = Snapshot{}
	if err := tk.once(st); err != nil {
		t.Fatal(err)
	}
	for _, line := range logLines(t, dir, "herdr") {
		if strings.Contains(line, "prompt") || strings.Contains(line, "obsolete") {
			t.Fatalf("departed worker received stale outbox: %s", line)
		}
	}
}

func TestBoardTidyDismissesOnlyDisplayedInboxFiles(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	b := Bead{ID: "task-one", Status: StatusInProgress}
	inboxFixture(t, Event{Bead: b.ID, Kind: EventLikelyStale, At: t0.Add(-time.Minute)})
	shown := inboxFixture(t, Event{Bead: b.ID, Kind: EventLikelyStale, At: t0})
	inboxFixture(t, Event{Bead: b.ID, Kind: EventNeedsYou, At: t0.Add(time.Second)})
	items, _ := readInbox()
	f := &fakeBoard{gc: gcItems("tj/", gcInputs{active: []Bead{b}, inbox: items}, t0)}
	if len(f.gc) != 1 || !slices.Equal(f.gc[0].inboxFiles, []string{shown.File}) {
		t.Fatalf("row did not capture exact displayed event: %+v", f.gc)
	}
	m := testModel(t, f)
	m.env.inboxDone = func(files []string) error { _, err := inboxDoneFiles(files); return err }
	press(t, m, "T")
	// A newer event arrives while the board still shows the previous flag.
	inboxFixture(t, Event{Bead: b.ID, Kind: EventLikelyStale, At: t0.Add(time.Minute)})
	press(t, m, "i")
	items, _ = readInbox()
	if len(items) != 3 || slices.ContainsFunc(items, func(it InboxItem) bool { return it.File == shown.File }) {
		t.Fatalf("dismissal did not preserve unseen events: %+v", items)
	}
}
