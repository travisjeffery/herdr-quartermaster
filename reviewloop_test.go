package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	testHeadSHA1 = "1111111111111111111111111111111111111111"
	testHeadSHA2 = "2222222222222222222222222222222222222222"
	testHeadSHA3 = "3333333333333333333333333333333333333333"
)

type fakeReviewEnv struct {
	beads          map[string]Bead
	headInfo       map[string]reviewHeadInfo
	tellErr        map[string]error
	dispatchErr    error
	enforceErr     error
	now            time.Time
	nextCreatedSeq int

	created    []BeadCreateOpts
	notes      []string
	statuses   map[string]string
	tells      []struct{ Bead, Text string }
	dispatches []struct{ Bead, Agent, By string }
	events     []Event
	journal    []journalRecord
}

func newFakeReviewEnv(beads ...Bead) *fakeReviewEnv {
	m := make(map[string]Bead, len(beads))
	for _, b := range beads {
		m[b.ID] = b
	}
	return &fakeReviewEnv{
		beads:    m,
		headInfo: map[string]reviewHeadInfo{},
		tellErr:  map[string]error{},
		statuses: map[string]string{},
		now:      time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeReviewEnv) env() reviewLoopEnv {
	return reviewLoopEnv{
		show: func(id string) (Bead, error) {
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
		setStatus: func(id, status string) error {
			f.statuses[id] = status
			if b, ok := f.beads[id]; ok {
				b.Status = status
				f.beads[id] = b
			}
			return nil
		},
		createBead: func(opts BeadCreateOpts) (Bead, error) {
			f.nextCreatedSeq++
			f.created = append(f.created, opts)
			id := fmt.Sprintf("rev-%d", f.nextCreatedSeq)
			created := Bead{
				ID:          id,
				Title:       opts.Title,
				Type:        opts.Type,
				Priority:    opts.Priority,
				Parent:      opts.Parent,
				Labels:      opts.Labels,
				Description: opts.Description,
				Status:      StatusOpen,
			}
			f.beads[id] = created
			return created, nil
		},
		resolveHead: func(b Bead) (reviewHeadInfo, error) {
			if info, ok := f.headInfo[b.ID]; ok {
				return info, nil
			}
			return reviewHeadInfo{}, errors.New("no head resolved")
		},
		tell: func(id, text string) (string, error) {
			if err := f.tellErr[id]; err != nil {
				return "", err
			}
			f.tells = append(f.tells, struct{ Bead, Text string }{id, text})
			return "sent to " + id, nil
		},
		dispatchReviewer: func(id, agent, by string) (string, error) {
			if f.dispatchErr != nil {
				return "", f.dispatchErr
			}
			f.dispatches = append(f.dispatches, struct{ Bead, Agent, By string }{id, agent, by})
			return "started reviewer " + id, nil
		},
		enforce: func(b Bead, action, by string) (string, error) {
			if f.enforceErr != nil {
				return "", f.enforceErr
			}
			return "AUTO: [" + by + " L5 C3]", nil
		},
		event: func(e Event) error {
			f.events = append(f.events, e)
			return nil
		},
		journal: func(r journalRecord) error {
			f.journal = append(f.journal, r)
			return nil
		},
		now: func() time.Time { return f.now },
	}
}

func TestReviewPingPongLifecycleHeadDedupAndEscalation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.Repo = "/src/app"
	cfg.Repos = map[string]string{"svc": "/src/svc"}

	impl := Bead{
		ID:          "task-impl",
		Title:       "Add rate limiter middleware",
		Status:      StatusInProgress,
		Priority:    2,
		Parent:      "epic-1",
		Labels:      []string{"repo:svc"},
		Description: "Design: token bucket per tenant.\n\n## Acceptance\n- Enforces burst limit\n- Returns 429 with Retry-After",
	}
	f := newFakeReviewEnv(impl)
	f.headInfo["task-impl"] = reviewHeadInfo{
		HeadSHA: testHeadSHA1,
		Branch:  "topic/rate-limiter",
		Workdir: "/wt/task-impl",
		PRURL:   "https://github.com/acme/svc/pull/12",
	}
	// Newly spawned reviewer has no running agent yet, so tell fails and dispatch runs.
	f.tellErr["rev-1"] = errors.New("rev-1 has no worker running")

	// Round 1 request with --spawn creates a worktree-free sibling reviewer bead (never parented under impl).
	res1, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{
		SpawnReviewer: true,
		MaxRounds:     2,
		By:            "coordinator",
	})
	if err != nil {
		t.Fatalf("round 1 requestReview: %v", err)
	}
	if res1.Deduplicated || res1.SpawnedBead != "rev-1" || res1.State.CurrentRound != 1 || res1.State.State != reviewStatePending {
		t.Fatalf("unexpected round 1 result: %+v", res1)
	}
	if len(f.created) != 1 {
		t.Fatalf("expected 1 created reviewer bead, got %d", len(f.created))
	}
	if f.created[0].Parent != "epic-1" {
		t.Errorf("reviewer bead parent = %q, want sibling parent epic-1 (never task-impl)", f.created[0].Parent)
	}
	if !strings.Contains(strings.Join(f.created[0].Labels, ","), "role:reviewer") || !strings.Contains(strings.Join(f.created[0].Labels, ","), "repo:svc") {
		t.Errorf("reviewer bead labels = %v, want role:reviewer and repo:svc", f.created[0].Labels)
	}
	if !strings.Contains(res1.Prompt, "Enforces burst limit") || !strings.Contains(res1.Prompt, "quartermaster review verdict task-impl --head "+testHeadSHA1) {
		t.Errorf("round 1 prompt missing acceptance criteria or verdict command:\n%s", res1.Prompt)
	}
	if len(f.dispatches) != 1 || f.dispatches[0].Bead != "rev-1" {
		t.Fatalf("expected reviewer dispatch for rev-1, got %+v", f.dispatches)
	}

	// Head-SHA deduplication: repeating request for the exact same pending head is a no-op.
	dup1, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{HeadSHA: testHeadSHA1, SpawnReviewer: true})
	if err != nil {
		t.Fatalf("duplicate pending request failed: %v", err)
	}
	if !dup1.Deduplicated || dup1.State.CurrentRound != 1 || len(f.created) != 1 || len(f.dispatches) != 1 {
		t.Fatalf("duplicate pending request should not increment round or dispatch: %+v", dup1)
	}

	// Stale verdict head SHA must fail closed.
	if _, err := recordReviewVerdict(f.env(), "task-impl", ReviewVerdictInput{
		Verdict:  "cannot_merge",
		HeadSHA:  testHeadSHA2,
		Summary:  "wrong sha",
		Findings: []ReviewFinding{{File: "limiter.go", Line: 10, Summary: "bug"}},
	}); err == nil || !strings.Contains(err.Error(), "stale verdict head") {
		t.Fatalf("expected stale verdict head error, got %v", err)
	}

	// cannot_merge without concrete blocking finding must fail closed.
	if _, err := recordReviewVerdict(f.env(), "task-impl", ReviewVerdictInput{
		Verdict: "cannot merge",
		HeadSHA: testHeadSHA1,
		Summary: "needs polish",
	}); err == nil || !strings.Contains(err.Error(), "blocking --finding") {
		t.Fatalf("expected missing finding error, got %v", err)
	}

	// Round 1 cannot_merge verdict routes structured findings to implementer via tell.
	f.now = f.now.Add(2 * time.Minute)
	v1, err := recordReviewVerdict(f.env(), "task-impl", ReviewVerdictInput{
		Verdict: "cannot merge",
		HeadSHA: testHeadSHA1,
		Summary: "Missing Retry-After header and map race",
		Findings: []ReviewFinding{
			{File: "limiter.go", Line: 42, Summary: "unsynchronized bucket map write", Consequence: "panics under concurrent requests"},
			{File: "limiter.go", Line: 78, Summary: "429 omits Retry-After", Consequence: "violates acceptance criterion"},
		},
		Later: []string{"rename helper var"},
	})
	if err != nil {
		t.Fatalf("round 1 verdict: %v", err)
	}
	if v1.Escalated || !v1.Routed || v1.State.State != reviewStateChanges {
		t.Fatalf("unexpected round 1 verdict result: %+v", v1)
	}
	if len(f.tells) != 1 || f.tells[0].Bead != "task-impl" ||
		!strings.Contains(f.tells[0].Text, "limiter.go:42: unsynchronized bucket map write — panics under concurrent requests") ||
		!strings.Contains(f.tells[0].Text, "round 2/2") {
		t.Fatalf("unexpected implementer tell feedback: %+v", f.tells)
	}
	if f.statuses["task-impl"] == StatusNeedsMe {
		t.Fatal("round 1 under cap should not escalate to needs_me")
	}

	// Head-SHA deduplication: cannot request round 2 on the exact same unfixed head SHA1.
	if _, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{HeadSHA: testHeadSHA1}); err == nil || !strings.Contains(err.Error(), "already received cannot_merge") {
		t.Fatalf("expected same-head rejection after cannot_merge, got %v", err)
	}

	// Implementer commits head SHA2; reviewer rev-1 is now running, so tell delivers round 2 prompt.
	delete(f.tellErr, "rev-1")
	f.now = f.now.Add(5 * time.Minute)
	res2, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{HeadSHA: testHeadSHA2})
	if err != nil {
		t.Fatalf("round 2 requestReview: %v", err)
	}
	if res2.State.CurrentRound != 2 || res2.State.State != reviewStatePending || res2.State.ReviewerBead != "rev-1" {
		t.Fatalf("unexpected round 2 state: %+v", res2)
	}
	if len(f.tells) != 2 || f.tells[1].Bead != "rev-1" || !strings.Contains(f.tells[1].Text, "Prior round 1/2") {
		t.Fatalf("round 2 should tell existing reviewer rev-1 with prior findings: %+v", f.tells)
	}

	// Round 2 still cannot_merge -> cap (2/2) reached -> escalates cleanly to needs_me without telling implementer.
	f.now = f.now.Add(3 * time.Minute)
	v2, err := recordReviewVerdict(f.env(), "task-impl", ReviewVerdictInput{
		Verdict: "cannot_merge",
		HeadSHA: testHeadSHA2,
		Summary: "Burst refill still allows negative tokens",
		Findings: []ReviewFinding{
			{File: "limiter.go", Line: 51, Summary: "clock jump produces negative token balance", Consequence: "permanently blocks tenant"},
		},
	})
	if err != nil {
		t.Fatalf("round 2 verdict: %v", err)
	}
	if !v2.Escalated || v2.Routed || v2.State.State != reviewStateEscalated {
		t.Fatalf("round 2 cap should escalate without routing: %+v", v2)
	}
	if f.statuses["task-impl"] != StatusNeedsMe {
		t.Fatalf("bead status = %q, want needs_me", f.statuses["task-impl"])
	}
	if len(f.tells) != 2 {
		t.Fatalf("escalation must not tell implementer (got %d tells)", len(f.tells))
	}
	// Structured QUESTION/OPTIONS/CLASS note must be parseable for approval taps.
	q, ok := parseNeedsMeQuestion(f.beads["task-impl"].Notes)
	if !ok || q.ClassN != 3 || q.recommendation() == nil || *q.recommendation() != 1 {
		t.Fatalf("escalation note should produce parseable QUESTION/OPTIONS/CLASS(3) rec=1: ok=%v q=%+v\nnotes:\n%s", ok, q, f.beads["task-impl"].Notes)
	}

	// Further requests while escalated fail closed; automated callers cannot --reset.
	if _, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{HeadSHA: testHeadSHA3, By: "coordinator"}); err == nil || !strings.Contains(err.Error(), "escalated") {
		t.Fatalf("expected escalated refusal, got %v", err)
	}
	if _, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{HeadSHA: testHeadSHA3, Reset: true, By: "coordinator"}); err == nil || !strings.Contains(err.Error(), "--by TJ") {
		t.Fatalf("automated --reset must be refused, got %v", err)
	}

	// TJ can explicitly --reset --by TJ and pass on head SHA3.
	f.beads["task-impl"] = func() Bead { b := f.beads["task-impl"]; b.Status = StatusInProgress; return b }()
	res3, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{HeadSHA: testHeadSHA3, Reset: true, By: "TJ"})
	if err != nil || res3.State.CurrentRound != 1 || res3.State.State != reviewStatePending {
		t.Fatalf("TJ reset failed: res=%+v err=%v", res3, err)
	}
	v3, err := recordReviewVerdict(f.env(), "task-impl", ReviewVerdictInput{
		Verdict: "can merge",
		HeadSHA: testHeadSHA3,
		Summary: "All blockers resolved and race tests pass",
	})
	if err != nil || v3.State.State != reviewStateApproved || v3.State.PassedHeadSHA != testHeadSHA3 || !v3.Routed {
		t.Fatalf("round 3 can_merge failed: res=%+v err=%v", v3, err)
	}

	// Repeating request on already-passed head deduplicates cleanly.
	dupPass, err := requestReview(cfg, f.env(), "task-impl", ReviewRequestOpts{HeadSHA: testHeadSHA3})
	if err != nil || !dupPass.Deduplicated {
		t.Fatalf("already-passed head should deduplicate: res=%+v err=%v", dupPass, err)
	}
}

func TestReviewLoopTickerNewHeadTriggerAndTellFailureFallback(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	now := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)

	impl := Bead{ID: "task-pp", Title: "Refactor cache", Status: StatusInProgress, Labels: []string{reviewRequiredLabel}}
	f := newFakeReviewEnv(impl)
	f.tellErr["task-pp"] = errors.New("task-pp has no worker running")

	// Ticker detects first clean head on review:required bead -> emits exactly one review_needed event.
	curHead := testHeadSHA1
	var tickerEvents []Event
	trigEnv := reviewLoopTriggerEnv{
		resolveHead: func(th Thread) (string, bool) { return curHead, true },
		event:       func(e Event) error { tickerEvents = append(tickerEvents, e); return nil },
	}
	threads := []Thread{{Bead: impl, Agent: &Agent{Name: "task-pp", Status: "idle"}}}
	checkReviewLoopsPass(cfg, trigEnv, threads, now)
	checkReviewLoopsPass(cfg, trigEnv, threads, now.Add(time.Minute))
	if len(tickerEvents) != 1 || tickerEvents[0].Kind != EventReviewNeeded || tickerEvents[0].Clears.Key != testHeadSHA1 {
		t.Fatalf("expected 1 deduplicated review_needed event for sha1, got %+v", tickerEvents)
	}

	// Request + cannot_merge verdict while implementer is offline falls back to review_feedback inbox event.
	if _, err := requestReview(cfg, f.env(), "task-pp", ReviewRequestOpts{HeadSHA: testHeadSHA1}); err != nil {
		t.Fatalf("request sha1: %v", err)
	}
	// Inbox condition for review_needed on sha1 is now obsolete once requested.
	if !inboxObsolete(InboxItem{Event: tickerEvents[0]}, map[string]Thread{"task-pp": threads[0]}) {
		t.Fatal("review_needed inbox item should become obsolete once review request starts for that head")
	}

	v1, err := recordReviewVerdict(f.env(), "task-pp", ReviewVerdictInput{
		Verdict:  "cannot_merge",
		HeadSHA:  testHeadSHA1,
		Summary:  "TTL bug",
		Findings: []ReviewFinding{{File: "cache.go", Line: 19, Summary: "zero TTL never expires"}},
	})
	if err != nil || v1.Routed {
		t.Fatalf("offline verdict should record without failing: v1=%+v err=%v", v1, err)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventReviewFeedback {
		t.Fatalf("expected review_feedback fallback event, got %+v", f.events)
	}

	// Same head SHA1 must not trigger another review_needed.
	checkReviewLoopsPass(cfg, trigEnv, threads, now.Add(2*time.Minute))
	if len(tickerEvents) != 1 {
		t.Fatalf("same head sha1 should not emit second review_needed: %+v", tickerEvents)
	}

	// Pushing new head SHA2 emits exactly one review_needed for round 2/2.
	curHead = testHeadSHA2
	checkReviewLoopsPass(cfg, trigEnv, threads, now.Add(3*time.Minute))
	checkReviewLoopsPass(cfg, trigEnv, threads, now.Add(4*time.Minute))
	if len(tickerEvents) != 2 || tickerEvents[1].Clears.Key != testHeadSHA2 || !strings.Contains(tickerEvents[1].Summary, "round 2/2") {
		t.Fatalf("expected 1 review_needed for sha2 round 2/2, got %+v", tickerEvents)
	}
}

func TestReviewCLIAndContextRendering(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	impl := Bead{ID: "task-cli", Title: "Add metrics exporter", Status: StatusInProgress}
	f := newFakeReviewEnv(impl)

	// Request via CLI (--json).
	var buf bytes.Buffer
	if err := reviewCmd(cfg, f.env(), []string{"request", "task-cli", "--head", testHeadSHA1, "--max-rounds", "3", "--json"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("reviewCmd request: %v", err)
	}
	var reqRes ReviewRequestResult
	if err := json.Unmarshal(buf.Bytes(), &reqRes); err != nil || reqRes.State.CurrentRound != 1 || reqRes.State.MaxRounds != 3 {
		t.Fatalf("unexpected request JSON: %v\n%s", err, buf.String())
	}

	// Verdict via JSON stdin (--file -).
	payload := `{"verdict":"cannot_merge","head_sha":"` + testHeadSHA1 + `","summary":"Missing timeout","findings":[{"file":"metrics.go","line":12,"summary":"no HTTP timeout","consequence":"hangs worker"}]}`
	buf.Reset()
	if err := reviewCmd(cfg, f.env(), []string{"verdict", "task-cli", "--file", "-", "--json"}, strings.NewReader(payload), &buf); err != nil {
		t.Fatalf("reviewCmd verdict --file -: %v", err)
	}
	var vRes ReviewVerdictResult
	if err := json.Unmarshal(buf.Bytes(), &vRes); err != nil || vRes.Verdict != reviewVerdictCannotMerge || !vRes.Routed {
		t.Fatalf("unexpected verdict JSON: %v\n%s", err, buf.String())
	}

	// Status CLI (single bead + list).
	buf.Reset()
	if err := reviewCmd(cfg, f.env(), []string{"status", "task-cli"}, strings.NewReader(""), &buf); err != nil {
		t.Fatalf("reviewCmd status: %v", err)
	}
	if !strings.Contains(buf.String(), "changes_requested") || !strings.Contains(buf.String(), "metrics.go:12") {
		t.Errorf("status text missing details:\n%s", buf.String())
	}

	// Context rendering surfaces review_loop in both text and JSON.
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	threads := []Thread{{Bead: impl, Agent: &Agent{Name: "task-cli", Kind: "pi", Status: "idle"}}}
	txt := renderContext(cfg, threads, nil, nil, nil, TickerState{}, nil, now)
	if !strings.Contains(txt, "review: round 1/3 changes_requested @1111111") {
		t.Errorf("context text missing review loop line:\n%s", txt)
	}
	jsonOut, err := renderContextWith(cfg, threads, nil, nil, nil, TickerState{}, nil, now, ContextOptions{JSON: true})
	if err != nil {
		t.Fatalf("context JSON: %v", err)
	}
	var doc struct {
		Threads []struct {
			Bead       string `json:"bead"`
			ReviewLoop string `json:"review_loop"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil || len(doc.Threads) != 1 || !strings.Contains(doc.Threads[0].ReviewLoop, "changes_requested") {
		t.Fatalf("context JSON missing review_loop: %v %+v", err, doc.Threads)
	}
}
