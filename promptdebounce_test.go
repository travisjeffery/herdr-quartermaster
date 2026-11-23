package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	debounceWindow = 90 * time.Second
	debounceFlap   = 30 * time.Second // two ticks at the 15s default
)

func blockedPromptThread(hash string) Thread {
	th := thread(bead(StatusInProgress), agent("blocked", 1), nil)
	if hash != "" {
		th.Prompt = &ApprovalPrompt{
			Lines:    []string{"Bash command", "make thing"},
			Options:  []PromptOption{{Number: 1, Label: "Yes", Selected: true}, {Number: 2, Label: "No"}},
			Numbered: true,
			Hash:     hash,
		}
	}
	return th
}

func needsYouEvents(o Outcome) []Event {
	var out []Event
	for _, e := range o.Events {
		if e.Kind == EventNeedsYou {
			out = append(out, e)
		}
	}
	return out
}

// debounceRun drives transitionDebounced/snapshotDebounced the way the
// ticker's once pass does, one tick at a time.
type debounceRun struct {
	prev     Snapshot
	surfaced []Event
}

func newDebounceRun() *debounceRun {
	working := thread(bead(StatusInProgress), agent("working", 1), nil)
	return &debounceRun{prev: snapshotDebounced(working, Snapshot{}, t0.Add(-15*time.Second), debounceWindow, debounceFlap, false)}
}

func (r *debounceRun) tick(th Thread, at time.Time) Outcome {
	o := transitionDebounced(th, r.prev, false, at, debounceWindow, debounceFlap)
	r.surfaced = append(r.surfaced, needsYouEvents(o)...)
	r.prev = snapshotDebounced(th, r.prev, at, debounceWindow, debounceFlap, false)
	return o
}

func (r *debounceRun) tickEvery(th Thread, from, to time.Time) {
	for at := from; !at.After(to); at = at.Add(15 * time.Second) {
		r.tick(th, at)
	}
}

func TestPromptDebounceTransientNeverSurfaces(t *testing.T) {
	r := newDebounceRun()
	r.tickEvery(blockedPromptThread("aaaa1111"), t0, t0.Add(75*time.Second))
	// The judge answers it before the window expires.
	r.tickEvery(thread(bead(StatusInProgress), agent("working", 2), nil), t0.Add(90*time.Second), t0.Add(180*time.Second))
	if len(r.surfaced) != 0 {
		t.Fatalf("a prompt cleared inside the window surfaced: %+v", r.surfaced)
	}
	if r.prev.PromptPending != "" || r.prev.PromptHash != "" {
		t.Fatalf("cleared prompt still tracked: %+v", r.prev)
	}
}

func TestPromptDebouncePersistentSurfacesOnceAtExpiry(t *testing.T) {
	r := newDebounceRun()
	th := blockedPromptThread("aaaa1111")
	r.tickEvery(th, t0, t0.Add(75*time.Second))
	if len(r.surfaced) != 0 {
		t.Fatalf("surfaced before expiry: %+v", r.surfaced)
	}
	if r.prev.PromptPending != "aaaa1111" || !r.prev.PromptSince.Equal(t0) {
		t.Fatalf("pending not tracked from first sight: %+v", r.prev)
	}
	o := r.tick(th, t0.Add(90*time.Second))
	if !o.Notify || len(r.surfaced) != 1 || !strings.Contains(r.surfaced[0].Summary, "[aaaa1111]") {
		t.Fatalf("want one notified event at expiry, got %+v / %+v", o, r.surfaced)
	}
	if r.prev.PromptHash != "aaaa1111" || r.prev.PromptPending != "" {
		t.Fatalf("surfaced prompt still pending: %+v", r.prev)
	}
	r.tickEvery(th, t0.Add(105*time.Second), t0.Add(180*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("surfaced more than once: %+v", r.surfaced)
	}
}

func TestPromptDebounceHashChangeStartsFreshWindow(t *testing.T) {
	r := newDebounceRun()
	r.tickEvery(blockedPromptThread("aaaa1111"), t0, t0.Add(45*time.Second))
	b := blockedPromptThread("bbbb2222")
	r.tickEvery(b, t0.Add(60*time.Second), t0.Add(135*time.Second))
	if len(r.surfaced) != 0 {
		t.Fatalf("the new prompt rode the old prompt's window: %+v", r.surfaced)
	}
	if !r.prev.PromptSince.Equal(t0.Add(60 * time.Second)) {
		t.Fatalf("fresh window should start at the hash change: %+v", r.prev.PromptSince)
	}
	r.tick(b, t0.Add(150*time.Second))
	if len(r.surfaced) != 1 || !strings.Contains(r.surfaced[0].Summary, "[bbbb2222]") {
		t.Fatalf("want the new prompt at its own expiry, got %+v", r.surfaced)
	}
}

func TestPromptDebounceFlapToleranceKeepsClock(t *testing.T) {
	a := blockedPromptThread("aaaa1111")
	for _, flapThread := range []Thread{
		thread(bead(StatusInProgress), agent("working", 2), nil), // briefly not blocked
		blockedPromptThread(""),                                  // blocked, screen unreadable
	} {
		r := newDebounceRun()
		r.tickEvery(a, t0, t0.Add(15*time.Second))
		r.tick(flapThread, t0.Add(30*time.Second))
		r.tickEvery(a, t0.Add(45*time.Second), t0.Add(75*time.Second))
		if len(r.surfaced) != 0 {
			t.Fatalf("surfaced early after a flap: %+v", r.surfaced)
		}
		r.tick(a, t0.Add(90*time.Second))
		if len(r.surfaced) != 1 {
			t.Fatalf("a one-tick flap reset the clock: %+v (pending %+v)", r.surfaced, r.prev)
		}
	}

	// Undetected past the tolerance it counts as cleared: the same hash
	// showing again is a new instance with a fresh window.
	r := newDebounceRun()
	r.tickEvery(a, t0, t0.Add(15*time.Second))
	r.tickEvery(thread(bead(StatusInProgress), agent("working", 2), nil), t0.Add(30*time.Second), t0.Add(60*time.Second))
	r.tickEvery(a, t0.Add(75*time.Second), t0.Add(150*time.Second))
	if len(r.surfaced) != 0 {
		t.Fatalf("a prompt gone past the flap tolerance kept its clock: %+v", r.surfaced)
	}
	r.tick(a, t0.Add(165*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("want surfacing at the fresh window's expiry, got %+v", r.surfaced)
	}
}

func TestPromptDebounceSurfacedPromptFlapDoesNotRepeat(t *testing.T) {
	r := newDebounceRun()
	a := blockedPromptThread("aaaa1111")
	r.tickEvery(a, t0, t0.Add(90*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("setup: %+v", r.surfaced)
	}
	r.tick(thread(bead(StatusInProgress), agent("working", 2), nil), t0.Add(105*time.Second))
	r.tickEvery(a, t0.Add(120*time.Second), t0.Add(180*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("the same prompt flapping back in re-surfaced: %+v", r.surfaced)
	}
}

func TestPromptDebounceRestartMidWindow(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	roundTrip := func(r *debounceRun) {
		t.Helper()
		st := TickerState{Threads: map[string]Snapshot{"backend-ab12": r.prev}, PRs: map[string]PR{}}
		if err := saveState(st); err != nil {
			t.Fatal(err)
		}
		r.prev = loadState().Threads["backend-ab12"]
	}
	r := newDebounceRun()
	a := blockedPromptThread("aaaa1111")
	r.tickEvery(a, t0, t0.Add(30*time.Second))
	roundTrip(r)
	if r.prev.PromptPending != "aaaa1111" || !r.prev.PromptSince.Equal(t0) {
		t.Fatalf("restart lost the pending prompt: %+v", r.prev)
	}
	r.tickEvery(a, t0.Add(45*time.Second), t0.Add(75*time.Second))
	if len(r.surfaced) != 0 {
		t.Fatalf("restart surfaced the prompt early: %+v", r.surfaced)
	}
	r.tick(a, t0.Add(90*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("restart dropped the prompt at expiry: %+v", r.surfaced)
	}
	roundTrip(r)
	r.tickEvery(a, t0.Add(105*time.Second), t0.Add(150*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("restart re-surfaced an announced prompt: %+v", r.surfaced)
	}
}

func TestPromptDebounceDisabledMatchesToday(t *testing.T) {
	working := thread(bead(StatusInProgress), agent("working", 1), nil)
	oldPrev := snapshot(working, Snapshot{}, t0.Add(-15*time.Second))
	newPrev := snapshotDebounced(working, Snapshot{}, t0.Add(-15*time.Second), 0, debounceFlap, false)
	steps := []Thread{
		blockedPromptThread("aaaa1111"),
		blockedPromptThread("aaaa1111"),
		blockedPromptThread("bbbb2222"),
		blockedPromptThread(""),
		thread(bead(StatusInProgress), agent("working", 2), nil),
		blockedPromptThread(""),
	}
	for i, th := range steps {
		at := t0.Add(time.Duration(i*15) * time.Second)
		oldOut := transition(th, oldPrev, false, at)
		newOut := transitionDebounced(th, newPrev, false, at, 0, debounceFlap)
		var oldEv, newEv []string
		for _, e := range oldOut.Events {
			oldEv = append(oldEv, string(e.Kind)+": "+e.Summary)
		}
		for _, e := range newOut.Events {
			newEv = append(newEv, string(e.Kind)+": "+e.Summary)
		}
		if strings.Join(oldEv, "\n") != strings.Join(newEv, "\n") || oldOut.Notify != newOut.Notify {
			t.Fatalf("step %d: disabled debounce changed behavior\nold: %q notify %v\nnew: %q notify %v", i, oldEv, oldOut.Notify, newEv, newOut.Notify)
		}
		oldPrev = snapshot(th, oldPrev, at)
		newPrev = snapshotDebounced(th, newPrev, at, 0, debounceFlap, false)
		if oldPrev.PromptHash != newPrev.PromptHash || newPrev.PromptPending != "" {
			t.Fatalf("step %d: disabled snapshot differs: %q vs %q pending %q", i, oldPrev.PromptHash, newPrev.PromptHash, newPrev.PromptPending)
		}
	}
}

func TestPromptDebounceUnparsedPrompt(t *testing.T) {
	r := newDebounceRun()
	th := blockedPromptThread("")
	r.tickEvery(th, t0, t0.Add(75*time.Second))
	if len(r.surfaced) != 0 || r.prev.PromptPending != promptUnparsedKey {
		t.Fatalf("unparsed prompt not debounced: %+v / %+v", r.surfaced, r.prev)
	}
	r.tick(th, t0.Add(90*time.Second))
	if len(r.surfaced) != 1 || r.surfaced[0].Summary != "backend-ab12 is waiting at an approval or question prompt" {
		t.Fatalf("want the generic prompt event at expiry, got %+v", r.surfaced)
	}
	r.tick(th, t0.Add(105*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("unparsed prompt re-surfaced: %+v", r.surfaced)
	}
}

func TestPromptDebounceUnparsedBecomingParsedKeepsClock(t *testing.T) {
	r := newDebounceRun()
	r.tickEvery(blockedPromptThread(""), t0, t0.Add(15*time.Second))
	a := blockedPromptThread("aaaa1111")
	r.tickEvery(a, t0.Add(30*time.Second), t0.Add(75*time.Second))
	if r.prev.PromptPending != "aaaa1111" || !r.prev.PromptSince.Equal(t0) {
		t.Fatalf("becoming parseable reset the clock: %+v", r.prev)
	}
	r.tick(a, t0.Add(90*time.Second))
	if len(r.surfaced) != 1 || !strings.Contains(r.surfaced[0].Summary, "[aaaa1111]") {
		t.Fatalf("want the parsed event at the original expiry, got %+v", r.surfaced)
	}
}

func TestPromptDebounceLegacyUnparsedSnapshotNotResurfaced(t *testing.T) {
	// A pre-debounce snapshot: in needs-you, blocked, and no hash or
	// pending fields — its generic prompt already reached the inbox.
	r := &debounceRun{prev: Snapshot{Group: GroupNeedsYou, AgentStatus: "blocked", AgentSeq: 1}}
	r.tickEvery(blockedPromptThread(""), t0, t0.Add(180*time.Second))
	if len(r.surfaced) != 0 {
		t.Fatalf("upgrade re-announced an unparsed prompt: %+v", r.surfaced)
	}
	if r.prev.PromptHash != promptUnparsedKey {
		t.Fatalf("legacy prompt not adopted as surfaced: %+v", r.prev)
	}
}

func TestPromptDebounceFirstSightBaselinesAsBefore(t *testing.T) {
	a := blockedPromptThread("aaaa1111")
	r := &debounceRun{prev: snapshotDebounced(a, Snapshot{}, t0, debounceWindow, debounceFlap, true)}
	r.tickEvery(a, t0.Add(15*time.Second), t0.Add(180*time.Second))
	if len(r.surfaced) != 0 {
		t.Fatalf("a prompt showing at first sight became news: %+v", r.surfaced)
	}
}

// A regression guard by name: the debounce holds worker prompt inbox events
// only; everything else the ticker surfaces keeps its current timing.
func TestPromptDebounceLeavesNonPromptEventsAlone(t *testing.T) {
	// A needs_me question surfaces at once.
	prev := Snapshot{Group: GroupWorking, AgentStatus: "working"}
	question := thread(bead(StatusNeedsMe), agent("idle", 1), nil)
	if o := transitionDebounced(question, prev, false, t0, debounceWindow, debounceFlap); !o.Notify || len(needsYouEvents(o)) != 1 {
		t.Fatalf("needs_me question was debounced: %+v", o)
	}
	// A prompt on a needs_me bead is the question surfacing: also at once.
	atPrompt := blockedPromptThread("aaaa1111")
	atPrompt.Bead.Status = StatusNeedsMe
	if o := transitionDebounced(atPrompt, prev, false, t0, debounceWindow, debounceFlap); len(needsYouEvents(o)) != 1 {
		t.Fatalf("a needs_me prompt was debounced: %+v", o)
	}
	// Failing checks prompt the worker and reach the inbox at once.
	th := thread(bead(StatusInProgress), agent("idle", 1), openPR("build"))
	o := transitionDebounced(th, Snapshot{Group: GroupReview, PRNumber: 7, PRState: "OPEN"}, false, t0, debounceWindow, debounceFlap)
	if len(o.Events) != 1 || o.Events[0].Kind != EventFailing || len(o.Prompts) != 1 {
		t.Fatalf("checks_failing was debounced: %+v", o)
	}
	// A merge lands at once even on the tick a prompt starts pending, and
	// that prompt still surfaces at its own expiry.
	open := thread(bead(StatusInProgress), agent("working", 1), &PR{Number: 7, State: "OPEN", URL: "https://github.com/o/a/pull/7"})
	r := &debounceRun{prev: snapshotDebounced(open, Snapshot{}, t0.Add(-15*time.Second), debounceWindow, debounceFlap, false)}
	merged := blockedPromptThread("aaaa1111")
	merged.PR = &PR{Number: 7, State: "MERGED", URL: "https://github.com/o/a/pull/7"}
	o = r.tick(merged, t0)
	kinds := map[EventKind]int{}
	for _, e := range o.Events {
		kinds[e.Kind]++
	}
	if kinds[EventMerged] != 1 || kinds[EventNeedsYou] != 0 {
		t.Fatalf("merge should land now and the prompt at expiry: %+v", o.Events)
	}
	r.tickEvery(merged, t0.Add(15*time.Second), t0.Add(90*time.Second))
	if len(r.surfaced) != 1 {
		t.Fatalf("prompt after a merge did not surface at expiry: %+v", r.surfaced)
	}
}

func TestPromptDebounceConfig(t *testing.T) {
	if got := defaultConfig().promptDebounce(); got != 90*time.Second {
		t.Fatalf("default debounce = %v, want 90s", got)
	}
	if got := defaultConfig().promptFlap(); got != 30*time.Second {
		t.Fatalf("default flap = %v, want two ticks (30s)", got)
	}
	if got := (Config{}).promptDebounce(); got != 0 {
		t.Fatalf("zero config debounce = %v, want disabled", got)
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
	if cfg, err := loadConfig(); err != nil || cfg.promptDebounce() != 90*time.Second {
		t.Fatalf("absent knob: %v / %v", err, cfg.promptDebounce())
	}
	write("repo = \"/repo\"\nprompt_debounce_seconds = 0\n")
	if cfg, err := loadConfig(); err != nil || cfg.promptDebounce() != 0 {
		t.Fatalf("zero knob should disable: %v / %v", err, cfg.promptDebounce())
	}
	write("repo = \"/repo\"\nprompt_debounce_seconds = -1\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "prompt_debounce_seconds") {
		t.Fatalf("negative knob accepted: %v", err)
	}
	if !strings.Contains(configTemplate("/repo"), "prompt_debounce_seconds") {
		t.Fatal("the fresh config template does not name the knob")
	}
}
