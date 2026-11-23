package main

import (
	"strings"
	"testing"
	"time"
)

func autoMergePR() *PR {
	pr := openPR()
	pr.URL = "https://github.com/o/r/pull/7"
	pr.ReviewDecision = "REVIEW_REQUIRED"
	pr.AutoMerge = &AutoMerge{Method: "SQUASH"}
	return pr
}

func TestBriefTurnsOnAutoMerge(t *testing.T) {
	s := brief(bead(StatusInProgress), "tj/x", "/w/x", "", "")
	for _, want := range []string{"`gh pr merge <url> --auto --squash`", "Leave auto-merge off on drafts", "`gh pr merge <url> --disable-auto`", "note that on the bead and carry on"} {
		if !strings.Contains(s, want) {
			t.Fatalf("brief lacks %q:\n%s", want, s)
		}
	}
}

func TestAutoMergeTurnedOffPromptsOnce(t *testing.T) {
	prev := snapshot(thread(bead(StatusInProgress), agent("idle", 1), autoMergePR()), Snapshot{}, t0)
	if prev.AutoMerge != "https://github.com/o/r/pull/7" {
		t.Fatalf("AutoMerge = %q", prev.AutoMerge)
	}
	off := autoMergePR()
	off.AutoMerge = nil
	th := thread(bead(StatusInProgress), agent("idle", 1), off)
	o := transition(th, prev, false, t0.Add(time.Minute))
	if len(o.Events) != 1 || o.Events[0].Kind != EventAutoMergeOff {
		t.Fatalf("want one auto_merge_off event, got %+v", o.Events)
	}
	if len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "`gh pr merge https://github.com/o/r/pull/7 --auto --squash`") {
		t.Fatalf("want a re-enable prompt, got %+v", o.Prompts)
	}
	prev = snapshot(th, prev, t0.Add(time.Minute))
	if o := transition(th, prev, false, t0.Add(2*time.Minute)); len(o.Events) != 0 || len(o.Prompts) != 0 {
		t.Fatalf("re-announced: %+v", o)
	}
}

func TestAutoMergeOffQuiet(t *testing.T) {
	prev := snapshot(thread(bead(StatusInProgress), agent("idle", 1), autoMergePR()), Snapshot{}, t0)
	cases := []struct {
		name   string
		status string
		edit   func(*PR)
	}{
		{"still on", StatusInProgress, func(*PR) {}},
		{"marked draft", StatusInProgress, func(pr *PR) { pr.AutoMerge, pr.IsDraft = nil, true }},
		{"waiting on a human decision", StatusNeedsMe, func(pr *PR) { pr.AutoMerge = nil }},
		{"merged", StatusInProgress, func(pr *PR) { pr.AutoMerge, pr.State = nil, "MERGED" }},
		{"another PR", StatusInProgress, func(pr *PR) { pr.AutoMerge, pr.Number, pr.URL = nil, 8, "https://github.com/o/r/pull/8" }},
		{"no review required", StatusInProgress, func(pr *PR) { pr.AutoMerge, pr.ReviewDecision = nil, "" }},
	}
	for _, c := range cases {
		pr := autoMergePR()
		c.edit(pr)
		o := transition(thread(bead(c.status), agent("idle", 1), pr), prev, false, t0.Add(time.Minute))
		for _, e := range o.Events {
			if e.Kind == EventAutoMergeOff {
				t.Errorf("%s: got %+v", c.name, e)
			}
		}
	}
	// A PR never seen with auto-merge on, as after upgrading, stays quiet.
	off := autoMergePR()
	off.AutoMerge = nil
	if o := transition(thread(bead(StatusInProgress), agent("idle", 1), off), Snapshot{PRNumber: 7, PRState: "OPEN"}, false, t0); len(o.Events) != 0 {
		t.Fatalf("got %+v", o)
	}
}

func TestReadyPromptWithAutoMerge(t *testing.T) {
	pr := readyPR()
	pr.AutoMerge = &AutoMerge{Method: "SQUASH"}
	th := thread(bead(StatusInProgress), agent("idle", 1), pr)
	o := transition(th, Snapshot{Group: GroupReview, PRNumber: 7, PRState: "OPEN", ReviewsSplit: true}, false, t0)
	if len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "Auto-merge is on, so GitHub merges it; don't merge it yourself.") || strings.Contains(o.Prompts[0], "--match-head-commit") {
		t.Fatalf("got %+v", o.Prompts)
	}
	// It is still a ready prompt, dropped like one when the head moves.
	if got := freshPending(th, o.Prompts); len(got) != 1 {
		t.Fatalf("a still-ready head dropped its prompt: %+v", got)
	}
	pushed := thread(bead(StatusInProgress), agent("idle", 1), readyPR())
	pushed.PR.HeadSHA = "bbb"
	if got := freshPending(pushed, o.Prompts); len(got) != 0 {
		t.Fatalf("kept a ready prompt for an old head: %+v", got)
	}
}

func TestUnsafeNativeAutoMergePolicyBAndInboxClear(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	pr := readyPR()
	pr.URL = "https://github.com/o/r/pull/7"
	pr.AutoMerge = &AutoMerge{Method: "SQUASH"}
	pr.ReviewDecision, pr.Gate.Reviews = "", nil
	th := thread(bead(StatusInProgress), agent("idle", 1), pr)

	// First sight is a baseline: no warning on first pass.
	if o := transition(th, Snapshot{}, true, t0); len(o.Events) != 0 || o.Notify || len(o.Prompts) != 0 {
		t.Fatalf("first pass must not warn: %+v", o)
	}
	// Already-armed PRs (for example from a prior snapshot before upgrade, or
	// after a push) must not fire unsafe_auto_merge either.
	already := Snapshot{PRNumber: 7, PRState: "OPEN", AutoMerge: pr.URL}
	if o := transition(th, already, false, t0); len(o.Events) != 0 || o.Notify {
		t.Fatalf("already-armed PR must not warn: %+v", o)
	}
	// Native auto-merge on a base branch requiring review is allowed under policy B.
	reviewReq := readyPR()
	reviewReq.URL = pr.URL
	reviewReq.ReviewDecision, reviewReq.Gate.Reviews = "REVIEW_REQUIRED", nil
	reviewReq.AutoMerge = &AutoMerge{Method: "SQUASH"}
	if o := transition(thread(bead(StatusInProgress), agent("idle", 1), reviewReq), Snapshot{PRNumber: 7, PRState: "OPEN"}, false, t0); len(o.Events) != 0 || o.Notify {
		t.Fatalf("review-required PR must not warn: %+v", o)
	}

	// Newly arming native auto-merge on an unprotected PR warns via ev() with
	// a PR@head clear condition.
	unarmed := Snapshot{PRNumber: 7, PRState: "OPEN"}
	o := transition(th, unarmed, false, t0)
	if len(o.Events) != 1 || o.Events[0].Kind != EventUnsafeAutoMerge || !o.Notify || len(o.Prompts) != 1 || !strings.Contains(o.Prompts[0], "--disable-auto") {
		t.Fatalf("expected newly-armed unsafe warning: %+v", o)
	}
	ev := o.Events[0]
	if ev.Clears == nil || ev.Clears.Kind != "unsafe_auto_merge" || ev.Clears.Key != inboxPRKey(th) {
		t.Fatalf("missing PR@head clear condition: %+v", ev.Clears)
	}
	if err := writeEvent(ev); err != nil {
		t.Fatal(err)
	}
	// Same state does not re-warn; moving head while still armed does not spam.
	prev := snapshot(th, unarmed, t0)
	if next := transition(th, prev, false, t0); len(next.Events) != 0 {
		t.Fatalf("duplicate warning: %+v", next)
	}
	// Missing PR observation retains the inbox notice.
	noPR := th
	noPR.PR = nil
	if err := reconcileInbox([]Thread{noPR}, nil); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 1 {
		t.Fatalf("missing PR observation archived warning: %+v", items)
	}
	// Turning auto-merge off drops the queued disarm prompt and archives the notice.
	pr.AutoMerge = nil
	if pending := freshPending(th, o.Prompts); len(pending) != 0 {
		t.Fatalf("stale disarm prompt: %v", pending)
	}
	if err := reconcileInbox([]Thread{th}, nil); err != nil {
		t.Fatal(err)
	}
	if items, _ := readInbox(); len(items) != 0 {
		t.Fatalf("disarmed warning not archived: %+v", items)
	}
}

func TestBriefStartsWithBdPrime(t *testing.T) {
	s := brief(bead(StatusInProgress), "tj/x", "/w/x", "", "")
	prime, show := strings.Index(s, "`bd prime`"), strings.Index(s, "`bd show ")
	if prime < 0 || show < 0 || prime > show {
		t.Fatalf("the brief should ask for bd prime before bd show:\n%s", s)
	}
}
