package main

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAcceptanceCriteria(t *testing.T) {
	desc := `Do the thing.

## Acceptance

- The flag is on in prod
  for every tenant.
* Dashboard shows it
1. Runbook updated

## Notes
- not a criterion
`
	got := acceptanceCriteria(desc)
	want := []string{"The flag is on in prod for every tenant.", "Dashboard shows it", "Runbook updated"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := acceptanceCriteria("## Problem\n- x\n"); got != nil {
		t.Fatalf("no Acceptance section: got %q", got)
	}
	// A ### subheading stays inside the section.
	if got := acceptanceCriteria("## Acceptance:\n### Must\n- a\n"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %q", got)
	}
}

func TestVerifierScope(t *testing.T) {
	pr := "PR: https://github.com/o/r/pull/7"
	run := "run https://github.com/o/r/actions/runs/123"
	for _, c := range []struct {
		name   string
		labels []string
		notes  string
		want   bool
	}{
		{"PR only", nil, pr, false},
		{"no PR", nil, "audit written up", true},
		{"rolling out", []string{LabelRollingOut}, pr, true},
		{"runs noted, label already removed", nil, pr + "\n" + run, true},
	} {
		got, why := verifierScope(Bead{Labels: c.labels, Notes: c.notes})
		if got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, why, c.want)
		}
	}
}

func TestParseVerdict(t *testing.T) {
	out := "checked things\nVERDICT {\"verdict\":\"fail\"}\nmore\nVERDICT {\"verdict\":\"pass\",\"criteria\":[{\"criterion\":\"a\",\"pass\":true,\"evidence\":\"e\"}]}\n"
	r, err := parseVerdict(out)
	if err != nil || !r.passed() {
		t.Fatalf("the last VERDICT line counts: %+v %v", r, err)
	}
	if _, err := parseVerdict("no verdict here"); err == nil {
		t.Fatal("want an error without a VERDICT line")
	}
	// "pass" with a failing criterion, or none, is a fail.
	for _, r := range []verifierResult{
		{Verdict: "pass", Criteria: []criterionResult{{Pass: true}, {Pass: false}}},
		{Verdict: "pass"},
	} {
		if r.passed() {
			t.Fatalf("%+v should not pass", r)
		}
	}
}

// fakeClose is an in-memory closeEnv: one bead, a journal, and a verifier
// that returns outs in turn.
type fakeClose struct {
	bead    Bead
	notes   []string
	closed  string
	status  string
	labels  map[string]bool
	journal []journalRecord
	outs    []string
	runErr  error
	prompts []string
	denied  bool
}

func (f *fakeClose) env() closeEnv {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return closeEnv{
		show:      func(string) (Bead, error) { return f.bead, nil },
		close:     func(_, reason string) error { f.closed = reason; return nil },
		note:      func(_, text string) error { f.notes = append(f.notes, text); return nil },
		setStatus: func(_, s string) error { f.status = s; return nil },
		setLabel: func(_, l string, on bool) error {
			if f.labels == nil {
				f.labels = map[string]bool{}
			}
			f.labels[l] = on
			return nil
		},
		enforce: func(_ Bead, by string) (string, error) {
			if f.denied && by != "TJ" {
				return "", errNotAutonomous
			}
			if by == "TJ" {
				return "", nil
			}
			return "AUTO: [" + by + " L4 C3]", nil
		},
		verify: func(_, prompt string) (string, error) {
			f.prompts = append(f.prompts, prompt)
			if f.runErr != nil {
				return "", f.runErr
			}
			out := f.outs[0]
			f.outs = f.outs[1:]
			return out, nil
		},
		journal: func(time.Time) ([]journalRecord, error) { return f.journal, nil },
		record:  func(r journalRecord) error { f.journal = append(f.journal, r); return nil },
		now:     func() time.Time { return now },
	}
}

const (
	passOut = `VERDICT {"verdict":"pass","criteria":[{"criterion":"run green","pass":true,"evidence":"run 123 succeeded"}]}`
	failOut = `VERDICT {"verdict":"fail","criteria":[{"criterion":"run green","pass":false,"evidence":"run 123 failed"}]}`
)

func noPRBead() Bead {
	return Bead{ID: "backend-ab12", Title: "Audit the thing", Status: StatusInProgress,
		Description: "Audit it.\n\n## Acceptance\n- run green\n", Notes: "SECRET-CONCLUSION: it is all done"}
}

func hasNote(notes []string, prefix string) bool {
	return slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, prefix) })
}

func TestClosePass(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), outs: []string{passOut}}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "audited", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.closed != "audited" || !hasNote(f.notes, "VERIFY: pass [verifier]") || !hasNote(f.notes, "AUTO: [worker L4 C3] close") {
		t.Fatalf("closed=%q notes=%q", f.closed, f.notes)
	}
	if len(f.journal) != 1 || f.journal[0].Kind != journalVerify || !f.journal[0].Pass {
		t.Fatalf("journal: %+v", f.journal)
	}
	// Fresh context: the verifier sees the criteria, not the worker's notes.
	p := f.prompts[0]
	if !strings.Contains(p, "1. run green") || strings.Contains(p, "SECRET-CONCLUSION") {
		t.Fatalf("prompt: %s", p)
	}
}

func TestCloseFailReturnsToWorkerThenEscalates(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), outs: []string{failOut, failOut}}
	var out strings.Builder
	err := closeBead(defaultConfig(), f.env(), "backend-ab12", "audited", "worker", &out)
	if !errors.Is(err, errVerifyFailed) || f.closed != "" || f.status != "" {
		t.Fatalf("round 1: err=%v closed=%q status=%q", err, f.closed, f.status)
	}
	if !strings.Contains(out.String(), "run green — run 123 failed") || !hasNote(f.notes, "VERIFY: fail [verifier] round 1/2") {
		t.Fatalf("round 1 should hand back the failing criteria: %s %q", out.String(), f.notes)
	}
	err = closeBead(defaultConfig(), f.env(), "backend-ab12", "audited", "worker", io.Discard)
	if !errors.Is(err, errVerifyFailed) || f.closed != "" || f.status != StatusNeedsMe {
		t.Fatalf("round 2: err=%v closed=%q status=%q", err, f.closed, f.status)
	}
	last := f.notes[len(f.notes)-1]
	if !strings.Contains(last, "escalated to TJ") || !strings.Contains(last, "Worker's close reason: audited") || !strings.Contains(last, "CLASS: 3") {
		t.Fatalf("escalation note: %s", last)
	}
}

func TestCloseFailThenPassResetsRounds(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), outs: []string{failOut, passOut}}
	_ = closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard)
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err != nil || f.closed != "r" {
		t.Fatalf("err=%v closed=%q", err, f.closed)
	}
	if fails, _ := verifyRounds(f.journal, "backend-ab12", time.Time{}); fails != 0 {
		t.Fatalf("a pass resets the rounds, got %d", fails)
	}
}

func TestCloseRetryBoundConfigurable(t *testing.T) {
	cfg := defaultConfig()
	cfg.Verifier.MaxRounds = 1
	f := &fakeClose{bead: noPRBead(), outs: []string{failOut}}
	if err := closeBead(cfg, f.env(), "backend-ab12", "r", "worker", io.Discard); !errors.Is(err, errVerifyFailed) || f.status != StatusNeedsMe {
		t.Fatalf("err=%v status=%q", err, f.status)
	}
}

func TestCloseOutOfScopeSkipsVerifier(t *testing.T) {
	b := noPRBead()
	b.Notes = "PR: https://github.com/o/r/pull/7"
	f := &fakeClose{bead: b}
	if err := closeBead(defaultConfig(), f.env(), b.ID, "merged", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.closed != "merged" || len(f.prompts) != 0 || len(f.journal) != 0 || hasNote(f.notes, "VERIFY:") {
		t.Fatalf("a PR-only bead closes as before: closed=%q prompts=%d notes=%q", f.closed, len(f.prompts), f.notes)
	}
}

func TestCloseWithoutAcceptanceSection(t *testing.T) {
	b := noPRBead()
	b.Description = "Investigate why the thing is slow and write it up."
	out := `VERDICT {"verdict":"pass","derived":true,"criteria":[{"criterion":"write-up exists","pass":true,"evidence":"docs/x.md"}]}`
	f := &fakeClose{bead: b, outs: []string{out}}
	if err := closeBead(defaultConfig(), f.env(), b.ID, "written up", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.prompts[0], "Derive 2-4 checkable criteria") {
		t.Fatalf("prompt should ask to derive criteria: %s", f.prompts[0])
	}
	if !slices.ContainsFunc(f.notes, func(n string) bool {
		return strings.HasPrefix(n, "VERIFY: pass") && strings.Contains(n, "criteria derived from the description")
	}) {
		t.Fatalf("notes: %q", f.notes)
	}
}

func TestCloseRollingOutRemovesLabel(t *testing.T) {
	b := noPRBead()
	b.Labels = []string{LabelRollingOut}
	b.Notes = "PR: https://github.com/o/r/pull/7"
	f := &fakeClose{bead: b, outs: []string{passOut}}
	if err := closeBead(defaultConfig(), f.env(), b.ID, "rolled out", "worker", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 1 || f.labels[LabelRollingOut] || f.closed == "" {
		t.Fatalf("prompts=%d labels=%v closed=%q", len(f.prompts), f.labels, f.closed)
	}
	if !strings.Contains(f.prompts[0], "PR: https://github.com/o/r/pull/7") {
		t.Fatalf("PR URL is a pointer for the verifier: %s", f.prompts[0])
	}
}

func TestCloseByTJSkipsVerifier(t *testing.T) {
	f := &fakeClose{bead: noPRBead()}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "TJ checked", "TJ", io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.closed == "" || len(f.prompts) != 0 || !hasNote(f.notes, "VERIFY: skipped [TJ]") {
		t.Fatalf("closed=%q prompts=%d notes=%q", f.closed, len(f.prompts), f.notes)
	}
}

func TestCloseRefusals(t *testing.T) {
	f := &fakeClose{bead: noPRBead(), denied: true}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); !errors.Is(err, errNotAutonomous) || len(f.prompts) != 0 {
		t.Fatalf("below the dial: err=%v", err)
	}
	if err := closeBead(defaultConfig(), (&fakeClose{bead: noPRBead()}).env(), "backend-ab12", " ", "worker", io.Discard); err == nil {
		t.Fatal("want an error without a reason")
	}
	// A verifier that fails to run spends no round and closes nothing.
	f = &fakeClose{bead: noPRBead(), runErr: errors.New("claude: not found")}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err == nil || f.closed != "" || len(f.journal) != 0 {
		t.Fatalf("err=%v closed=%q journal=%v", err, f.closed, f.journal)
	}
}

func TestCloseReusesFreshPass(t *testing.T) {
	f := &fakeClose{bead: noPRBead()}
	now := f.env().now()
	f.journal = []journalRecord{{At: now.Add(-10 * time.Minute), Kind: journalVerify, Bead: "backend-ab12", Pass: true}}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err != nil || len(f.prompts) != 0 {
		t.Fatalf("a fresh pass needs no rerun: err=%v prompts=%d", err, len(f.prompts))
	}
	// An old one does.
	f = &fakeClose{bead: noPRBead(), outs: []string{passOut}}
	f.journal = []journalRecord{{At: now.Add(-2 * time.Hour), Kind: journalVerify, Bead: "backend-ab12", Pass: true}}
	if err := closeBead(defaultConfig(), f.env(), "backend-ab12", "r", "worker", io.Discard); err != nil || len(f.prompts) != 1 {
		t.Fatalf("a stale pass reruns: err=%v prompts=%d", err, len(f.prompts))
	}
}

func TestBriefsDescribeAcceptanceAndVerifier(t *testing.T) {
	b := Bead{ID: "backend-ab12", Title: "t"}
	for name, text := range map[string]string{
		"brief": brief(b, "tj/x", "/w", "", ""),
		"guide": coordinatorGuide(defaultConfig()),
	} {
		for _, want := range []string{"## Acceptance", "quartermaster close", "read-only verifier", "needs_me", "rolling-out"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(text, "`bd close ") {
			t.Errorf("%s still tells agents to bd close", name)
		}
	}
}
