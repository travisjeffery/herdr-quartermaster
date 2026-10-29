package main

import (
	"strings"
	"testing"
	"time"
)

func autoMergePR() *PR {
	pr := openPR()
	pr.URL = "https://github.com/o/r/pull/7"
	pr.AutoMerge = &AutoMerge{Method: "SQUASH"}
	return pr
}

func TestBriefTurnsOnAutoMerge(t *testing.T) {
	s := brief(bead(StatusInProgress), "tj/x", "/w/x", "", "", false)
	for _, want := range []string{"`gh pr merge <url> --auto --squash`", "Leave it off on drafts", "`gh pr merge <url> --disable-auto`", "note that on the bead and carry on"} {
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

func TestBriefStartsWithBdPrime(t *testing.T) {
	s := brief(bead(StatusInProgress), "tj/x", "/w/x", "", "", false)
	prime, show := strings.Index(s, "`bd prime`"), strings.Index(s, "`bd show ")
	if prime < 0 || show < 0 || prime > show {
		t.Fatalf("the brief should ask for bd prime before bd show:\n%s", s)
	}
}
