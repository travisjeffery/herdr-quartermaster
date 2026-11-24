package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func reviewReadyPR(sha string, at time.Time) *PR {
	return &PR{
		Number:         17,
		Title:          "Add queue worker",
		URL:            "https://github.com/acme/app/pull/17",
		Head:           "feat/queue-worker",
		State:          "OPEN",
		IsDraft:        false,
		ReviewDecision: "REVIEW_REQUIRED",
		HeadSHA:        sha,
		MergeState:     "BLOCKED",
		Mergeable:      "MERGEABLE",
		Checks: []Check{
			{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Name: "vet", Status: "COMPLETED", Conclusion: "SUCCESS"},
		},
		Gate: &MergeGate{
			Head:        sha,
			Author:      "owner",
			Viewer:      "owner",
			OpenThreads: 0,
			At:          at,
		},
		DetailsFresh: true,
	}
}

func reviewEventsOfKind(events []Event, kind EventKind) []Event {
	var out []Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestReadyForReviewPredicate(t *testing.T) {
	cases := []struct {
		name string
		edit func(*PR)
		want bool
	}{
		{"green mergeable REVIEW_REQUIRED 0 threads", func(*PR) {}, true},
		{"behind base but mergeable", func(pr *PR) { pr.MergeState = "BEHIND" }, true},
		{"clean merge state", func(pr *PR) { pr.MergeState = "CLEAN" }, true},
		{"empty Mergeable with BLOCKED MergeState", func(pr *PR) { pr.Mergeable = "" }, true},
		{"empty MergeState with MERGEABLE Mergeable", func(pr *PR) { pr.MergeState = "" }, true},
		{"draft PR suppressed", func(pr *PR) { pr.IsDraft = true }, false},
		{"merged PR suppressed", func(pr *PR) { pr.State = "MERGED" }, false},
		{"closed PR suppressed", func(pr *PR) { pr.State = "CLOSED" }, false},
		{"no reported checks suppressed", func(pr *PR) { pr.Checks = nil }, false},
		{"failing check suppressed", func(pr *PR) {
			pr.Checks = append(pr.Checks, Check{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"})
		}, false},
		{"pending check suppressed", func(pr *PR) {
			pr.Checks = append(pr.Checks, Check{Name: "e2e", Status: "IN_PROGRESS"})
		}, false},
		{"conflicting MergeState DIRTY suppressed", func(pr *PR) { pr.MergeState = "DIRTY" }, false},
		{"conflicting Mergeable suppressed", func(pr *PR) { pr.Mergeable = "CONFLICTING" }, false},
		{"unknown MergeState suppressed", func(pr *PR) { pr.MergeState = "UNKNOWN" }, false},
		{"unknown Mergeable suppressed", func(pr *PR) { pr.Mergeable = "UNKNOWN" }, false},
		{"draft MergeState suppressed", func(pr *PR) { pr.MergeState = "DRAFT" }, false},
		{"missing mergeability signal suppressed", func(pr *PR) { pr.MergeState, pr.Mergeable = "", "" }, false},
		{"already approved suppressed", func(pr *PR) { pr.ReviewDecision = "APPROVED" }, false},
		{"changes requested suppressed", func(pr *PR) { pr.ReviewDecision = "CHANGES_REQUESTED" }, false},
		{"no review required suppressed", func(pr *PR) { pr.ReviewDecision = "" }, false},
		{"unresolved review thread suppressed", func(pr *PR) { pr.Gate.OpenThreads = 1 }, false},
		{"paginated review threads fail closed", func(pr *PR) { pr.Gate.MoreThreads = true }, false},
		{"missing gate fails closed", func(pr *PR) { pr.Gate = nil }, false},
		{"stale gate head fails closed", func(pr *PR) { pr.Gate.Head = "oldsha" }, false},
		{"stale details fail closed", func(pr *PR) { pr.DetailsFresh = false }, false},
		{"empty head SHA suppressed", func(pr *PR) { pr.HeadSHA = "" }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := reviewReadyPR("sha1", t0)
			tc.edit(pr)
			got := readyForReview(pr, summarizeChecks(pr.Checks))
			if got != tc.want {
				t.Fatalf("readyForReview = %v, want %v (pr=%+v)", got, tc.want, pr)
			}
			th := thread(Bead{ID: "task-1", Status: StatusInProgress}, agent("idle", 1), pr)
			key := reviewReadyKey(th)
			if tc.want && key != "17@sha1" {
				t.Fatalf("reviewReadyKey = %q, want 17@sha1", key)
			}
			if !tc.want && key != "" {
				t.Fatalf("reviewReadyKey = %q, want empty", key)
			}
		})
	}
}

func TestReadyForReviewTransitionDedupAndNewHead(t *testing.T) {
	opts := transitionOpts{reviewBump: 4 * time.Hour}
	b := Bead{ID: "task-1", Status: StatusInProgress, Title: "Queue worker"}

	// First sight of an already-ready PR baselines without firing.
	ready := reviewReadyPR("sha1", t0)
	thReady := thread(b, agent("idle", 1), ready)
	if o := transitionWith(thReady, Snapshot{}, true, t0, opts); len(o.Events) != 0 {
		t.Fatalf("first sight emitted events: %+v", o.Events)
	}
	base := snapshotWith(thReady, Snapshot{}, t0, opts, true)
	if !base.ReviewReadyTracked || base.ReviewReadyKey != "17@sha1" || !base.ReviewReadyAt.Equal(t0) {
		t.Fatalf("unexpected first-sight baseline: %+v", base)
	}

	// Pre-upgrade snapshot (ReviewReadyTracked=false) baselines without spamming.
	legacy := Snapshot{Group: GroupReview, PRNumber: 17, PRState: "OPEN", ReviewsSplit: true}
	if o := transitionWith(thReady, legacy, false, t0, opts); len(o.Events) != 0 {
		t.Fatalf("pre-upgrade snapshot emitted events: %+v", o.Events)
	}

	// Start with pending checks -> tracked baseline has empty ReviewReadyKey.
	pending := reviewReadyPR("sha1", t0)
	pending.Checks = []Check{{Name: "test", Status: "IN_PROGRESS"}}
	prev := snapshotWith(thread(b, agent("idle", 1), pending), Snapshot{}, t0, opts, true)
	if !prev.ReviewReadyTracked || prev.ReviewReadyKey != "" {
		t.Fatalf("bad pending baseline: %+v", prev)
	}

	// Transition to green + mergeable + REVIEW_REQUIRED + 0 open threads emits once.
	t1 := t0.Add(2 * time.Minute)
	thReady = thread(b, agent("idle", 1), reviewReadyPR("sha1", t1))
	o := transitionWith(thReady, prev, false, t1, opts)
	evts := reviewEventsOfKind(o.Events, EventReadyForReview)
	if len(evts) != 1 {
		t.Fatalf("want 1 ready_for_review event, got %+v", o.Events)
	}
	if o.Notify || len(o.Prompts) != 0 {
		t.Fatalf("ready_for_review should not desktop-notify or prompt worker: %+v", o)
	}
	if !strings.Contains(evts[0].Summary, "PR #17 is ready for review at sha1") || !strings.Contains(evts[0].Summary, ready.URL) {
		t.Fatalf("unexpected summary: %q", evts[0].Summary)
	}
	if evts[0].Clears == nil || evts[0].Clears.Kind != "ready_for_review" || evts[0].Clears.Key != inboxPRKey(thReady) {
		t.Fatalf("unexpected clears condition: %+v", evts[0].Clears)
	}
	prev = snapshotWith(thReady, prev, t1, opts, false)

	// Subsequent ticks on the same head stay quiet.
	t2 := t1.Add(time.Minute)
	if o := transitionWith(thReady, prev, false, t2, opts); len(o.Events) != 0 {
		t.Fatalf("same head re-emitted: %+v", o.Events)
	}
	prev = snapshotWith(thReady, prev, t2, opts, false)

	// Transient observation gap (stale details / UNKNOWN / missing gate) and
	// recovery on the same head neither duplicates ready_for_review nor clears ReviewReadyAt.
	t3 := t2.Add(time.Minute)
	transient := reviewReadyPR("sha1", t3)
	transient.DetailsFresh = false
	transient.Gate = nil
	prev = snapshotWith(thread(b, agent("idle", 1), transient), prev, t3, opts, false)
	if prev.ReviewReadyKey != "17@sha1" || !prev.ReviewReadyAt.Equal(t1) {
		t.Fatalf("transient gap lost tracking: key=%q at=%v", prev.ReviewReadyKey, prev.ReviewReadyAt)
	}
	t4 := t3.Add(time.Minute)
	thReady = thread(b, agent("idle", 1), reviewReadyPR("sha1", t4))
	if o := transitionWith(thReady, prev, false, t4, opts); len(o.Events) != 0 {
		t.Fatalf("recovery after transient gap re-emitted: %+v", o.Events)
	}
	prev = snapshotWith(thReady, prev, t4, opts, false)

	// New head SHA re-emits ready_for_review once.
	t5 := t4.Add(5 * time.Minute)
	thNewHead := thread(b, agent("idle", 1), reviewReadyPR("sha2", t5))
	o = transitionWith(thNewHead, prev, false, t5, opts)
	if evts := reviewEventsOfKind(o.Events, EventReadyForReview); len(evts) != 1 || !strings.Contains(evts[0].Summary, "sha2") {
		t.Fatalf("new head should re-emit ready_for_review, got %+v", o.Events)
	}
	prev = snapshotWith(thNewHead, prev, t5, opts, false)
	if prev.ReviewReadyKey != "17@sha2" || !prev.ReviewReadyAt.Equal(t5) {
		t.Fatalf("new head snapshot wrong: %+v", prev)
	}

	// Replacement PR on the same head SHA also re-emits.
	t6 := t5.Add(time.Minute)
	repl := reviewReadyPR("sha2", t6)
	repl.Number = 18
	repl.URL = "https://github.com/acme/app/pull/18"
	o = transitionWith(thread(b, agent("idle", 1), repl), prev, false, t6, opts)
	if evts := reviewEventsOfKind(o.Events, EventReadyForReview); len(evts) != 1 || !strings.Contains(evts[0].Summary, "PR #18") {
		t.Fatalf("replacement PR should re-emit ready_for_review, got %+v", o.Events)
	}
}

func TestReadyForReviewRespectsNeedsMe(t *testing.T) {
	opts := transitionOpts{reviewBump: 4 * time.Hour}
	pending := reviewReadyPR("sha1", t0)
	pending.Checks = []Check{{Name: "test", Status: "IN_PROGRESS"}}
	prev := snapshotWith(thread(Bead{ID: "task-1", Status: StatusNeedsMe}, agent("idle", 1), pending), Snapshot{}, t0, opts, true)

	// While needs_me, becoming green must not emit ready_for_review or baseline it away.
	t1 := t0.Add(time.Minute)
	held := thread(Bead{ID: "task-1", Status: StatusNeedsMe}, agent("idle", 1), reviewReadyPR("sha1", t1))
	if o := transitionWith(held, prev, false, t1, opts); len(reviewEventsOfKind(o.Events, EventReadyForReview)) != 0 {
		t.Fatalf("emitted ready_for_review while needs_me: %+v", o.Events)
	}
	prev = snapshotWith(held, prev, t1, opts, false)
	if prev.ReviewReadyKey != "" {
		t.Fatalf("baselined ReviewReadyKey %q while needs_me", prev.ReviewReadyKey)
	}

	// Once answered, ready_for_review fires once.
	t2 := t1.Add(time.Minute)
	answered := thread(Bead{ID: "task-1", Status: StatusInProgress}, agent("idle", 2), reviewReadyPR("sha1", t2))
	if o := transitionWith(answered, prev, false, t2, opts); len(reviewEventsOfKind(o.Events, EventReadyForReview)) != 1 {
		t.Fatalf("want ready_for_review once answered, got %+v", o.Events)
	}
	prev = snapshotWith(answered, prev, t2, opts, false)

	// Entering needs_me AFTER ready_for_review fired and returning on the same
	// head must not re-emit ready_for_review.
	t3 := t2.Add(time.Minute)
	prev = snapshotWith(thread(Bead{ID: "task-1", Status: StatusNeedsMe}, agent("idle", 2), reviewReadyPR("sha1", t3)), prev, t3, opts, false)
	t4 := t3.Add(time.Minute)
	if o := transitionWith(thread(Bead{ID: "task-1", Status: StatusInProgress}, agent("idle", 3), reviewReadyPR("sha1", t4)), prev, false, t4, opts); len(reviewEventsOfKind(o.Events, EventReadyForReview)) != 0 {
		t.Fatalf("re-emitted ready_for_review for already-announced head after needs_me: %+v", o.Events)
	}
}

func TestReviewBumpBoundedLoop(t *testing.T) {
	opts := transitionOpts{reviewBump: 4 * time.Hour}
	b := Bead{ID: "task-1", Status: StatusInProgress, Title: "Queue worker"}

	// Baseline pending, then transition to ready_for_review at t1.
	pending := reviewReadyPR("sha1", t0)
	pending.Checks = []Check{{Name: "test", Status: "IN_PROGRESS"}}
	prev := snapshotWith(thread(b, agent("idle", 1), pending), Snapshot{}, t0, opts, true)

	t1 := t0.Add(time.Minute)
	th := thread(b, agent("idle", 1), reviewReadyPR("sha1", t1))
	o := transitionWith(th, prev, false, t1, opts)
	if len(reviewEventsOfKind(o.Events, EventReadyForReview)) != 1 || len(reviewEventsOfKind(o.Events, EventReviewBump)) != 0 {
		t.Fatalf("expected only ready_for_review at t1, got %+v", o.Events)
	}
	prev = snapshotWith(th, prev, t1, opts, false)

	// Just before 4h: no bump yet.
	tBefore := t1.Add(4*time.Hour - time.Minute)
	th = thread(b, agent("idle", 1), reviewReadyPR("sha1", tBefore))
	if o := transitionWith(th, prev, false, tBefore, opts); len(o.Events) != 0 {
		t.Fatalf("bumped early: %+v", o.Events)
	}
	prev = snapshotWith(th, prev, tBefore, opts, false)

	// Bot review (with 0 open threads) does NOT suppress human review bump.
	thBot := thread(b, agent("idle", 1), reviewReadyPR("sha1", tBefore.Add(30*time.Second)))
	thBot.BotReviews = []string{"review-bot"}
	prev = snapshotWith(thBot, prev, tBefore.Add(30*time.Second), opts, false)

	// At 4h: emits exactly ONE review_bump.
	tAt := t1.Add(4 * time.Hour)
	thAt := thread(b, agent("idle", 1), reviewReadyPR("sha1", tAt))
	thAt.BotReviews = []string{"review-bot"}
	o = transitionWith(thAt, prev, false, tAt, opts)
	bumps := reviewEventsOfKind(o.Events, EventReviewBump)
	if len(bumps) != 1 || len(reviewEventsOfKind(o.Events, EventReadyForReview)) != 0 {
		t.Fatalf("want 1 review_bump at 4h, got %+v", o.Events)
	}
	if !strings.Contains(bumps[0].Summary, "PR #17 is still awaiting review at sha1 after 4h") {
		t.Fatalf("unexpected review_bump summary: %q", bumps[0].Summary)
	}
	if bumps[0].Clears == nil || bumps[0].Clears.Kind != "review_bump" || bumps[0].Clears.Key != inboxPRKey(thAt) {
		t.Fatalf("unexpected review_bump clears: %+v", bumps[0].Clears)
	}
	prev = snapshotWith(thAt, prev, tAt, opts, false)
	if prev.ReviewBumpKey != "17@sha1" {
		t.Fatalf("ReviewBumpKey = %q, want 17@sha1", prev.ReviewBumpKey)
	}

	// Never spams again on the same head even hours later.
	for _, delta := range []time.Duration{time.Minute, 4 * time.Hour, 24 * time.Hour} {
		tLater := tAt.Add(delta)
		thLater := thread(b, agent("idle", 1), reviewReadyPR("sha1", tLater))
		thLater.BotReviews = []string{"review-bot"}
		if o := transitionWith(thLater, prev, false, tLater, opts); len(o.Events) != 0 {
			t.Fatalf("re-emitted at +%v: %+v", delta, o.Events)
		}
		prev = snapshotWith(thLater, prev, tLater, opts, false)
	}

	// Pushing a new head SHA re-emits ready_for_review and allows at most one new bump.
	tPush := tAt.Add(25 * time.Hour)
	th2 := thread(b, agent("idle", 1), reviewReadyPR("sha2", tPush))
	o = transitionWith(th2, prev, false, tPush, opts)
	if len(reviewEventsOfKind(o.Events, EventReadyForReview)) != 1 || len(reviewEventsOfKind(o.Events, EventReviewBump)) != 0 {
		t.Fatalf("new head should emit ready_for_review only, got %+v", o.Events)
	}
	prev = snapshotWith(th2, prev, tPush, opts, false)

	tPush4h := tPush.Add(4 * time.Hour)
	th2Later := thread(b, agent("idle", 1), reviewReadyPR("sha2", tPush4h))
	o = transitionWith(th2Later, prev, false, tPush4h, opts)
	if len(reviewEventsOfKind(o.Events, EventReviewBump)) != 1 {
		t.Fatalf("new head should emit 1 review_bump after 4h, got %+v", o.Events)
	}
	prev = snapshotWith(th2Later, prev, tPush4h, opts, false)
	if o := transitionWith(th2Later, prev, false, tPush4h.Add(5*time.Hour), opts); len(o.Events) != 0 {
		t.Fatalf("new head emitted second review_bump: %+v", o.Events)
	}
}

func TestReviewBumpSuppressions(t *testing.T) {
	b := Bead{ID: "task-1", Status: StatusInProgress}
	opts := transitionOpts{reviewBump: 4 * time.Hour}

	startReady := func(at time.Time, initialReviews int) Snapshot {
		pending := reviewReadyPR("sha1", at.Add(-time.Minute))
		pending.Checks = []Check{{Name: "test", Status: "IN_PROGRESS"}}
		thPending := thread(b, agent("idle", 1), pending)
		thPending.Reviews = initialReviews
		prev := snapshotWith(thPending, Snapshot{}, at.Add(-time.Minute), opts, true)
		thReady := thread(b, agent("idle", 1), reviewReadyPR("sha1", at))
		thReady.Reviews = initialReviews
		return snapshotWith(thReady, prev, at, opts, false)
	}

	t.Run("disabled when review_bump_hours is 0", func(t *testing.T) {
		prev := startReady(t0, 0)
		th := thread(b, agent("idle", 1), reviewReadyPR("sha1", t0.Add(10*time.Hour)))
		if o := transitionWith(th, prev, false, t0.Add(10*time.Hour), transitionOpts{reviewBump: 0}); len(o.Events) != 0 {
			t.Fatalf("bump emitted while disabled: %+v", o.Events)
		}
	})

	t.Run("suppressed when a new human review arrives", func(t *testing.T) {
		prev := startReady(t0, 1)
		// Human reviewer leaves a comment (still REVIEW_REQUIRED, 0 open threads).
		tMid := t0.Add(time.Hour)
		thMid := thread(b, agent("idle", 1), reviewReadyPR("sha1", tMid))
		thMid.Reviews = 2
		prev = snapshotWith(thMid, prev, tMid, opts, false)

		tLater := t0.Add(5 * time.Hour)
		thLater := thread(b, agent("idle", 1), reviewReadyPR("sha1", tLater))
		thLater.Reviews = 2
		if o := transitionWith(thLater, prev, false, tLater, opts); len(reviewEventsOfKind(o.Events, EventReviewBump)) != 0 {
			t.Fatalf("bumped despite new human review: %+v", o.Events)
		}
	})

	t.Run("confirmed draft pause resets bump timer without duplicating ready_for_review", func(t *testing.T) {
		prev := startReady(t0, 0)
		// Converted to draft at +10m and stays draft past 4h.
		draft := reviewReadyPR("sha1", t0.Add(10*time.Minute))
		draft.IsDraft = true
		prev = snapshotWith(thread(b, agent("idle", 1), draft), prev, t0.Add(10*time.Minute), opts, false)

		// Un-drafted on same sha1 at +5h: neither ready_for_review nor immediate bump.
		tUndraft := t0.Add(5 * time.Hour)
		thUndraft := thread(b, agent("idle", 1), reviewReadyPR("sha1", tUndraft))
		if o := transitionWith(thUndraft, prev, false, tUndraft, opts); len(o.Events) != 0 {
			t.Fatalf("unexpected events immediately after leaving draft: %+v", o.Events)
		}
		prev = snapshotWith(thUndraft, prev, tUndraft, opts, false)

		// After a full 4h continuously ready since un-drafting, emits 1 bump.
		tFull := tUndraft.Add(4 * time.Hour)
		thFull := thread(b, agent("idle", 1), reviewReadyPR("sha1", tFull))
		if o := transitionWith(thFull, prev, false, tFull, opts); len(reviewEventsOfKind(o.Events, EventReviewBump)) != 1 {
			t.Fatalf("expected 1 review_bump after full 4h window, got %+v", o.Events)
		}
	})

	t.Run("suppressed when PR becomes approved or has open threads", func(t *testing.T) {
		for _, mutate := range []func(*PR){
			func(p *PR) { p.ReviewDecision = "APPROVED" },
			func(p *PR) { p.ReviewDecision = "CHANGES_REQUESTED" },
			func(p *PR) { p.IsDraft = true },
			func(p *PR) { p.Mergeable = "CONFLICTING"; p.MergeState = "DIRTY" },
			func(p *PR) { p.Gate.OpenThreads = 1 },
			func(p *PR) {
				p.Checks = append(p.Checks, Check{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"})
			},
		} {
			prev := startReady(t0, 0)
			pr := reviewReadyPR("sha1", t0.Add(5*time.Hour))
			mutate(pr)
			th := thread(b, agent("idle", 1), pr)
			if o := transitionWith(th, prev, false, t0.Add(5*time.Hour), opts); len(reviewEventsOfKind(o.Events, EventReviewBump)) != 0 {
				t.Fatalf("bumped ineligible PR: %+v", o.Events)
			}
		}
	})
}

func TestReadyForReviewAndBumpPersistAcrossRestartsEndToEnd(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\ncase \"$1\" in list) printf '%s\\n' '[{\"id\":\"task-1\",\"status\":\"in_progress\",\"title\":\"Queue worker\"}]' ;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	newRestartedTicker := func() Ticker {
		cfg := defaultConfig()
		cfg.Repo = t.TempDir()
		cfg.Nudge = false
		cfg.VerifyMinutes = 0
		disabled := false
		cfg.Desktop.Enabled = &disabled
		tk := newTicker(cfg, log.New(io.Discard, "", 0))
		tk.now = func() time.Time { return now }
		return tk
	}

	runTick := func(pr PR) []InboxItem {
		t.Helper()
		tk := newRestartedTicker()
		st := loadState()
		st.LastGH = now // keep PR fixtures controlled without invoking gh
		st.PRs["task-1"] = pr
		if err := tk.once(&st); err != nil {
			t.Fatalf("ticker once at %v: %v", now, err)
		}
		if err := saveState(st); err != nil {
			t.Fatalf("saveState: %v", err)
		}
		items, err := readInbox()
		if err != nil {
			t.Fatalf("readInbox: %v", err)
		}
		return items
	}

	// Tick 1: pending checks baseline.
	pending := *reviewReadyPR("sha1", now)
	pending.Checks = []Check{{Name: "test", Status: "IN_PROGRESS"}}
	if items := runTick(pending); len(items) != 0 {
		t.Fatalf("baseline tick wrote inbox items: %+v", items)
	}

	// Tick 2 (after restart): PR turns green + mergeable + REVIEW_REQUIRED + 0 threads -> emits ready_for_review.
	now = now.Add(5 * time.Minute)
	items := runTick(*reviewReadyPR("sha1", now))
	if len(items) != 1 || items[0].Kind != EventReadyForReview {
		t.Fatalf("want 1 ready_for_review item, got %+v", items)
	}
	if _, err := inboxDone([]string{items[0].File}); err != nil {
		t.Fatal(err)
	}

	// Tick 3 (after restart, +30m): deduplicated across restarts; no bump yet.
	now = now.Add(30 * time.Minute)
	if items := runTick(*reviewReadyPR("sha1", now)); len(items) != 0 {
		t.Fatalf("re-emitted after restart: %+v", items)
	}

	// Tick 4 (after restart, +4h since ready_for_review): emits exactly ONE review_bump.
	now = now.Add(3*time.Hour + 30*time.Minute)
	items = runTick(*reviewReadyPR("sha1", now))
	if len(items) != 1 || items[0].Kind != EventReviewBump {
		t.Fatalf("want 1 review_bump item after 4h, got %+v", items)
	}
	if _, err := inboxDone([]string{items[0].File}); err != nil {
		t.Fatal(err)
	}

	// Tick 5 (after restart, +10h): stays completely silent (bounded to 1 bump).
	now = now.Add(10 * time.Hour)
	if items := runTick(*reviewReadyPR("sha1", now)); len(items) != 0 {
		t.Fatalf("emitted extra event after bump: %+v", items)
	}

	// Tick 6 (after restart, new head sha2): emits fresh ready_for_review.
	now = now.Add(5 * time.Minute)
	items = runTick(*reviewReadyPR("sha2", now))
	if len(items) != 1 || items[0].Kind != EventReadyForReview || !strings.Contains(items[0].Summary, "sha2") {
		t.Fatalf("want fresh ready_for_review for sha2, got %+v", items)
	}
}

func TestGatePRsFetchesForReviewRequiredCandidates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	queryLog := filepath.Join(dir, "gh-query.log")
	t.Setenv("GH_QUERY_LOG", queryLog)

	resp, err := json.Marshal(map[string]any{
		"data": map[string]any{
			"viewer": map[string]string{"login": "owner"},
			"p0": map[string]any{
				"pullRequest": map[string]any{
					"headRefOid":    "sha-review",
					"author":        map[string]string{"login": "owner"},
					"reviews":       map[string]any{"pageInfo": map[string]bool{"hasNextPage": false}, "nodes": []any{}},
					"reviewThreads": map[string]any{"pageInfo": map[string]bool{"hasNextPage": false}, "nodes": []any{}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GH_QUERY_LOG\"\nprintf '%s\\n' '" + string(resp) + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	tk := Ticker{cfg: defaultConfig(), gh: GH{repo: dir}, log: log.New(io.Discard, "", 0), now: func() time.Time { return now }}

	candidate := *reviewReadyPR("sha-review", now)
	candidate.Number = 21
	candidate.URL = "https://github.com/acme/app/pull/21"
	candidate.Gate = nil

	draft := candidate
	draft.Number = 22
	draft.URL = "https://github.com/acme/app/pull/22"
	draft.IsDraft = true

	failing := candidate
	failing.Number = 23
	failing.URL = "https://github.com/acme/app/pull/23"
	failing.Checks = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}

	conflicting := candidate
	conflicting.Number = 24
	conflicting.URL = "https://github.com/acme/app/pull/24"
	conflicting.Mergeable = "CONFLICTING"
	conflicting.MergeState = "DIRTY"

	prs := map[string]PR{
		"task-candidate":   candidate,
		"task-draft":       draft,
		"task-failing":     failing,
		"task-conflicting": conflicting,
	}
	tk.gatePRs(prs)

	raw, err := os.ReadFile(queryLog)
	if err != nil {
		t.Fatalf("expected gh graphql call: %v", err)
	}
	q := string(raw)
	if !strings.Contains(q, "pullRequest(number: 21)") {
		t.Fatalf("query missing review-required candidate #21:\n%s", q)
	}
	for _, skip := range []string{"pullRequest(number: 22)", "pullRequest(number: 23)", "pullRequest(number: 24)"} {
		if strings.Contains(q, skip) {
			t.Fatalf("query should not fetch gate for ineligible %s:\n%s", skip, q)
		}
	}
	g := prs["task-candidate"].Gate
	if g == nil || g.Head != "sha-review" || g.OpenThreads != 0 || !g.At.Equal(now) {
		t.Fatalf("candidate gate not applied: %+v", g)
	}
}

func TestReadyForReviewAndBumpInboxLifecycle(t *testing.T) {
	for _, kind := range []EventKind{EventReadyForReview, EventReviewBump} {
		t.Run(string(kind), func(t *testing.T) {
			cases := []struct {
				name     string
				mutate   func(*Thread)
				obsolete bool
			}{
				{"still ready same head", func(*Thread) {}, false},
				{"transient missing PR", func(th *Thread) { th.PR = nil }, false},
				{"transient unknown merge state", func(th *Thread) { th.PR.MergeState = "UNKNOWN" }, false},
				{"transient missing gate", func(th *Thread) { th.PR.Gate = nil }, false},
				{"PR merged", func(th *Thread) { th.PR.State = "MERGED" }, true},
				{"PR closed", func(th *Thread) { th.PR.State = "CLOSED" }, true},
				{"head moved", func(th *Thread) { th.PR.HeadSHA = "sha2" }, true},
				{"converted to draft", func(th *Thread) { th.PR.IsDraft = true }, true},
				{"approved", func(th *Thread) { th.PR.ReviewDecision = "APPROVED" }, true},
				{"changes requested", func(th *Thread) { th.PR.ReviewDecision = "CHANGES_REQUESTED" }, true},
				{"checks failing", func(th *Thread) { th.Checks.Failing = []string{"test"} }, true},
				{"merge state DIRTY", func(th *Thread) { th.PR.MergeState = "DIRTY" }, true},
				{"mergeable CONFLICTING", func(th *Thread) { th.PR.Mergeable = "CONFLICTING" }, true},
				{"unresolved review thread", func(th *Thread) { th.PR.Gate.OpenThreads = 1 }, true},
				{"more than 100 threads", func(th *Thread) { th.PR.Gate.MoreThreads = true }, true},
				{"bead entered needs_me", func(th *Thread) { th.Bead.Status = StatusNeedsMe }, true},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					th := thread(Bead{ID: "task-1", Status: StatusInProgress}, agent("idle", 1), reviewReadyPR("sha1", t0))
					it := InboxItem{Event: Event{Bead: th.Bead.ID, Kind: kind, Clears: eventCondition(th, kind)}}
					tc.mutate(&th)
					if got := inboxObsolete(it, map[string]Thread{th.Bead.ID: th}); got != tc.obsolete {
						t.Fatalf("obsolete=%v, want %v (clears=%+v)", got, tc.obsolete, it.Clears)
					}
				})
			}
		})
	}
}

func TestReviewBumpHoursConfig(t *testing.T) {
	if def := defaultConfig(); def.ReviewBumpHours != 4 || def.reviewBumpAfter() != 4*time.Hour {
		t.Fatalf("default review_bump_hours = %d (%v), want 4 (4h)", def.ReviewBumpHours, def.reviewBumpAfter())
	}
	if zero := (Config{}); zero.reviewBumpAfter() != 0 {
		t.Fatalf("zero Config reviewBumpAfter = %v, want 0", zero.reviewBumpAfter())
	}

	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("repo = \"/repo\"\n")
	cfg, err := loadConfig()
	if err != nil || cfg.ReviewBumpHours != 4 || cfg.reviewBumpAfter() != 4*time.Hour {
		t.Fatalf("absent review_bump_hours: cfg=%+v err=%v", cfg, err)
	}

	write("repo = \"/repo\"\nreview_bump_hours = 0\n")
	cfg, err = loadConfig()
	if err != nil || cfg.ReviewBumpHours != 0 || cfg.reviewBumpAfter() != 0 {
		t.Fatalf("review_bump_hours=0: cfg=%+v err=%v", cfg, err)
	}

	write("repo = \"/repo\"\nreview_bump_hours = 6\n")
	cfg, err = loadConfig()
	if err != nil || cfg.ReviewBumpHours != 6 || cfg.reviewBumpAfter() != 6*time.Hour {
		t.Fatalf("review_bump_hours=6: cfg=%+v err=%v", cfg, err)
	}

	write("repo = \"/repo\"\nreview_bump_hours = -1\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "review_bump_hours") {
		t.Fatalf("expected error on negative review_bump_hours, got %v", err)
	}

	if !strings.Contains(configTemplate("/repo"), "review_bump_hours") {
		t.Fatal("configTemplate does not mention review_bump_hours")
	}
}

func TestCoordinatorGuideDocumentsReadyForReviewAndBump(t *testing.T) {
	g := coordinatorGuide(Config{Repo: "/r"})
	for _, want := range []string{
		"ready_for_review",
		"review_bump",
		"review_bump_hours",
		"--action post",
		"class 5",
		"team review channel",
		"AUTO: [coordinator L<n> C5]",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("coordinatorGuide missing %q", want)
		}
	}
	// Verify review_bump and ready_for_review events clear cleanly on departed beads.
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	for i, kind := range []EventKind{EventReadyForReview, EventReviewBump} {
		inboxFixture(t, Event{Bead: "departed", Kind: kind, At: t0.Add(time.Duration(i))})
	}
	if err := reconcileInbox(nil, nil); err != nil {
		t.Fatal(err)
	}
	items, err := readInbox()
	if err != nil || len(items) != 0 {
		t.Fatalf("departed review events not archived: %+v, %v", items, err)
	}
	// Verify EventReadyForReview and EventReviewBump are distinct event kinds.
	if slices.Contains([]EventKind{EventReady, EventReview, EventFailing}, EventReadyForReview) || EventReadyForReview == EventReviewBump {
		t.Fatal("event kinds are not distinct")
	}
}
