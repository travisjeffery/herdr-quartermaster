package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	changedFiles   map[string][]string
	changedErr     error
	tellErr        map[string]error
	closeErr       map[string]error
	resolveErr     map[string]error
	dispatchErr    error
	enforceErr     error
	enforceFn      func(b Bead, action, by string) (string, error)
	now            time.Time
	nextCreatedSeq int

	created      []BeadCreateOpts
	closed       []struct{ Bead, Reason string }
	resolved     []string
	notes        []string
	statuses     map[string]string
	tells        []struct{ Bead, Text string }
	dispatches   []struct{ Bead, Role, Agent, By string }
	enforceCalls []struct{ Bead, Action, By string }
	events       []Event
	journal      []journalRecord
}

func newFakeReviewEnv(beads ...Bead) *fakeReviewEnv {
	m := make(map[string]Bead, len(beads))
	for _, b := range beads {
		m[b.ID] = b
	}
	return &fakeReviewEnv{
		beads:        m,
		headInfo:     map[string]reviewHeadInfo{},
		changedFiles: map[string][]string{},
		tellErr:      map[string]error{},
		closeErr:     map[string]error{},
		resolveErr:   map[string]error{},
		statuses:     map[string]string{},
		now:          time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
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
		closeBead: func(id, reason string) error {
			if err := f.closeErr[id]; err != nil {
				return err
			}
			f.closed = append(f.closed, struct{ Bead, Reason string }{id, reason})
			if b, ok := f.beads[id]; ok {
				b.Status = StatusClosed
				b.CloseReason = reason
				f.beads[id] = b
			}
			return nil
		},
		resolveReviewer: func(id string) (string, error) {
			if err := f.resolveErr[id]; err != nil {
				return "", err
			}
			f.resolved = append(f.resolved, id)
			return "removed role directory /state/roles/" + id, nil
		},
		resolveHead: func(b Bead) (reviewHeadInfo, error) {
			if info, ok := f.headInfo[b.ID]; ok {
				return info, nil
			}
			return reviewHeadInfo{}, errors.New("no head resolved")
		},
		changedFiles: func(b Bead, info reviewHeadInfo) ([]string, error) {
			if f.changedErr != nil {
				return nil, f.changedErr
			}
			if files, ok := f.changedFiles[info.HeadSHA]; ok {
				return slices.Clone(files), nil
			}
			if files, ok := f.changedFiles[b.ID]; ok {
				return slices.Clone(files), nil
			}
			return nil, nil
		},
		tell: func(id, text string) (string, error) {
			if err := f.tellErr[id]; err != nil {
				return "", err
			}
			f.tells = append(f.tells, struct{ Bead, Text string }{id, text})
			return "sent to " + id, nil
		},
		dispatchReviewer: func(id, role, agent, by string) (string, error) {
			if f.dispatchErr != nil {
				return "", f.dispatchErr
			}
			f.dispatches = append(f.dispatches, struct{ Bead, Role, Agent, By string }{id, role, agent, by})
			return "started reviewer " + id, nil
		},
		enforce: func(b Bead, action, by string) (string, error) {
			f.enforceCalls = append(f.enforceCalls, struct{ Bead, Action, By string }{b.ID, action, by})
			if f.enforceFn != nil {
				return f.enforceFn(b, action, by)
			}
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
	// Passing verdict must auto-close and resolve the spawned sibling reviewer,
	// while leaving the implementer bead untouched.
	if !slices.Equal(v3.ReviewerClosed, []string{"rev-1"}) || !slices.Equal(v3.ReviewerResolved, []string{"rev-1"}) {
		t.Fatalf("expected rev-1 closed & resolved on can_merge, got closed=%v resolved=%v cleanup=%q",
			v3.ReviewerClosed, v3.ReviewerResolved, v3.ReviewerCleanup)
	}
	if f.beads["rev-1"].Status != StatusClosed || !strings.Contains(f.beads["rev-1"].CloseReason, "All blockers resolved and race tests pass") {
		t.Fatalf("rev-1 should be closed with verdict summary, got status=%q reason=%q", f.beads["rev-1"].Status, f.beads["rev-1"].CloseReason)
	}
	if f.beads["task-impl"].Status != StatusInProgress {
		t.Fatalf("implementer bead must never be closed by reviewer cleanup, got status=%q", f.beads["task-impl"].Status)
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

func TestReviewRiskPathSecurityRoleSelectionAndFallback(t *testing.T) {
	noWorktree := false
	secPatterns := []string{
		"**/auth/**",
		"**/iam/**",
		"**/.github/workflows/**",
		"**/migrations/**",
	}

	t.Run("routes matching diff to configured security-review role", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		cfg.Repo = "/src/app"
		cfg.Review.SecurityPaths = secPatterns
		cfg.Roles = map[string]RoleConfig{
			"security-review": {Worktree: &noWorktree, Brief: roleReviewer},
		}

		impl := Bead{ID: "task-sec", Title: "Add OIDC token check", Status: StatusInProgress, Priority: 2, Parent: "epic-sec"}
		f := newFakeReviewEnv(impl)
		f.changedFiles[testHeadSHA1] = []string{"pkg/auth/oidc.go", "pkg/http/server.go"}
		f.tellErr["rev-1"] = errors.New("not running")

		res, err := requestReview(cfg, f.env(), "task-sec", ReviewRequestOpts{
			HeadSHA:       testHeadSHA1,
			SpawnReviewer: true,
			By:            "coordinator",
		})
		if err != nil {
			t.Fatalf("requestReview: %v", err)
		}
		if res.ReviewerRole != "security-review" {
			t.Fatalf("ReviewerRole = %q, want security-review", res.ReviewerRole)
		}
		if !slices.Equal(res.MatchedSecurityPaths, []string{"pkg/auth/oidc.go"}) {
			t.Fatalf("MatchedSecurityPaths = %v, want [pkg/auth/oidc.go]", res.MatchedSecurityPaths)
		}
		if len(f.created) != 1 || !slices.Contains(f.created[0].Labels, "role:security-review") {
			t.Fatalf("created bead labels = %+v, want role:security-review", f.created)
		}
		if len(f.dispatches) != 1 || f.dispatches[0].Role != "security-review" {
			t.Fatalf("dispatches = %+v, want role=security-review", f.dispatches)
		}
		if !strings.Contains(res.Prompt, "Review role: `security-review`") || !strings.Contains(res.Prompt, "pkg/auth/oidc.go") {
			t.Errorf("prompt missing security role context:\n%s", res.Prompt)
		}
		if !strings.Contains(f.created[0].Description, "pkg/auth/oidc.go") {
			t.Errorf("reviewer description missing matched security path:\n%s", f.created[0].Description)
		}
	})

	t.Run("falls back to reviewer when security role is not configured", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		cfg.Repo = "/src/app"
		cfg.Review.SecurityPaths = secPatterns
		// Intentionally omit cfg.Roles["security-review"].

		impl := Bead{ID: "task-fallback", Title: "Update CI workflow", Status: StatusInProgress, Priority: 2}
		f := newFakeReviewEnv(impl)
		f.changedFiles[testHeadSHA1] = []string{".github/workflows/ci.yml"}
		f.tellErr["rev-1"] = errors.New("not running")

		res, err := requestReview(cfg, f.env(), "task-fallback", ReviewRequestOpts{
			HeadSHA:       testHeadSHA1,
			SpawnReviewer: true,
			By:            "coordinator",
		})
		if err != nil {
			t.Fatalf("requestReview: %v", err)
		}
		if res.ReviewerRole != roleReviewer {
			t.Fatalf("ReviewerRole = %q, want fallback %q", res.ReviewerRole, roleReviewer)
		}
		if len(f.created) != 1 || !slices.Contains(f.created[0].Labels, "role:reviewer") {
			t.Fatalf("created bead labels = %+v, want role:reviewer", f.created)
		}
		if len(f.dispatches) != 1 || f.dispatches[0].Role != roleReviewer {
			t.Fatalf("dispatches = %+v, want role=reviewer", f.dispatches)
		}
	})

	t.Run("routes non-matching diff to plain reviewer even when security role exists", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		cfg.Repo = "/src/app"
		cfg.Review.SecurityPaths = secPatterns
		cfg.Review.SecurityRole = "custom-sec"
		cfg.Roles = map[string]RoleConfig{
			"custom-sec": {Worktree: &noWorktree, Brief: roleReviewer},
		}

		impl := Bead{ID: "task-normal", Title: "Tune cache size", Status: StatusInProgress, Priority: 2}
		f := newFakeReviewEnv(impl)
		f.changedFiles[testHeadSHA1] = []string{"pkg/cache/lru.go", "pkg/authz/helper.go"}
		f.tellErr["rev-1"] = errors.New("not running")

		res, err := requestReview(cfg, f.env(), "task-normal", ReviewRequestOpts{
			HeadSHA:       testHeadSHA1,
			SpawnReviewer: true,
			By:            "coordinator",
		})
		if err != nil {
			t.Fatalf("requestReview: %v", err)
		}
		if res.ReviewerRole != roleReviewer || len(res.MatchedSecurityPaths) != 0 {
			t.Fatalf("unexpected non-security routing: role=%q matched=%v", res.ReviewerRole, res.MatchedSecurityPaths)
		}
		if len(f.dispatches) != 1 || f.dispatches[0].Role != roleReviewer {
			t.Fatalf("dispatches = %+v, want role=reviewer", f.dispatches)
		}
	})

	t.Run("round 2 escalates from reviewer to security-review and cleans up both on can_merge", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		cfg.Repo = "/src/app"
		cfg.Review.SecurityPaths = secPatterns
		cfg.Roles = map[string]RoleConfig{
			"security-review": {Worktree: &noWorktree, Brief: roleReviewer},
		}

		impl := Bead{ID: "task-esc", Title: "Add tenant filter", Status: StatusInProgress, Priority: 2}
		f := newFakeReviewEnv(impl)
		f.changedFiles[testHeadSHA1] = []string{"pkg/query/filter.go"}
		f.changedFiles[testHeadSHA2] = []string{"pkg/query/filter.go", "db/migrations/002_tenant.sql"}
		f.tellErr["rev-1"] = errors.New("not running")
		f.tellErr["rev-2"] = errors.New("not running")

		r1, err := requestReview(cfg, f.env(), "task-esc", ReviewRequestOpts{HeadSHA: testHeadSHA1, SpawnReviewer: true})
		if err != nil || r1.SpawnedBead != "rev-1" || r1.ReviewerRole != roleReviewer {
			t.Fatalf("round 1 unexpected: res=%+v err=%v", r1, err)
		}
		if _, err := recordReviewVerdict(f.env(), "task-esc", ReviewVerdictInput{
			Verdict:  "cannot_merge",
			HeadSHA:  testHeadSHA1,
			Summary:  "Missing DB index",
			Findings: []ReviewFinding{{File: "pkg/query/filter.go", Line: 20, Summary: "full table scan"}},
		}); err != nil {
			t.Fatalf("round 1 verdict: %v", err)
		}

		// Round 2 touches migrations/** -> spawns a fresh role:security-review sibling instead of reusing plain rev-1.
		r2, err := requestReview(cfg, f.env(), "task-esc", ReviewRequestOpts{HeadSHA: testHeadSHA2, SpawnReviewer: true})
		if err != nil || r2.SpawnedBead != "rev-2" || r2.ReviewerRole != "security-review" {
			t.Fatalf("round 2 should escalate to security-review rev-2: res=%+v err=%v", r2, err)
		}

		v2, err := recordReviewVerdict(f.env(), "task-esc", ReviewVerdictInput{
			Verdict: "can_merge",
			HeadSHA: testHeadSHA2,
			Summary: "Migration and query verified",
		})
		if err != nil {
			t.Fatalf("round 2 verdict: %v", err)
		}
		if !slices.Equal(v2.ReviewerClosed, []string{"rev-1", "rev-2"}) || !slices.Equal(v2.ReviewerResolved, []string{"rev-1", "rev-2"}) {
			t.Fatalf("expected both rev-1 and rev-2 cleaned up, got closed=%v resolved=%v", v2.ReviewerClosed, v2.ReviewerResolved)
		}
		if f.beads["task-esc"].Status != StatusInProgress {
			t.Fatalf("implementer status = %q, want in_progress", f.beads["task-esc"].Status)
		}
	})
}

func TestReviewGitChangedFilesAndCommitsAheadIntegration(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test User",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial")
	baseSHA := runGit("rev-parse", "HEAD")

	// Fresh branch at base commit has 0 commits ahead of main.
	runGit("checkout", "-b", "topic/auth-update")
	if gitWorktreeHasCommitsAhead(repo, "main", baseSHA) {
		t.Fatal("fresh branch at base commit should report no commits ahead")
	}

	// Commit a change under pkg/auth/ and verify real git diff inspection selects security-review.
	authDir := filepath.Join(repo, "pkg", "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "verify.go"), []byte("package auth\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "pkg/auth/verify.go")
	runGit("commit", "-m", "add auth verifier")
	headSHA := runGit("rev-parse", "HEAD")

	if !gitWorktreeHasCommitsAhead(repo, "main", headSHA) {
		t.Fatal("branch with new commit should report commits ahead")
	}

	files, err := gitChangedFiles(repo, "main", headSHA)
	if err != nil {
		t.Fatalf("gitChangedFiles: %v", err)
	}
	if !slices.Equal(files, []string{"pkg/auth/verify.go"}) {
		t.Fatalf("gitChangedFiles = %v, want [pkg/auth/verify.go]", files)
	}

	noWorktree := false
	cfg := defaultConfig()
	cfg.Repo = repo
	cfg.BaseBranch = "main"
	cfg.Review.SecurityPaths = []string{"**/auth/**", "**/iam/**"}
	cfg.Roles = map[string]RoleConfig{
		"security-review": {Worktree: &noWorktree, Brief: roleReviewer},
	}

	impl := Bead{ID: "task-git", Title: "Verify tokens", Status: StatusInProgress, Priority: 2}
	f := newFakeReviewEnv(impl)
	f.headInfo["task-git"] = reviewHeadInfo{HeadSHA: headSHA, Workdir: repo, Branch: "topic/auth-update"}
	f.tellErr["rev-1"] = errors.New("not running")
	env := f.env()
	env.changedFiles = func(b Bead, info reviewHeadInfo) ([]string, error) {
		return liveChangedFiles(cfg, b, info)
	}

	res, err := requestReview(cfg, env, "task-git", ReviewRequestOpts{SpawnReviewer: true, By: "ticker"})
	if err != nil {
		t.Fatalf("requestReview with real git diff: %v", err)
	}
	if res.ReviewerRole != "security-review" || !slices.Equal(res.MatchedSecurityPaths, []string{"pkg/auth/verify.go"}) {
		t.Fatalf("unexpected real-git routing: role=%q matched=%v", res.ReviewerRole, res.MatchedSecurityPaths)
	}
}

func TestReviewAutoTriggerOnTickerAndFallbackGates(t *testing.T) {
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	t.Run("auto-triggers pre-PR and round-2 heads with autonomy + budget + dedup", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		cfg.Autonomy.Level = 5
		cfg.Autonomy.Max = 7

		impl := Bead{ID: "task-auto", Title: "Add parser", Status: StatusInProgress, Labels: []string{reviewRequiredLabel}}
		f := newFakeReviewEnv(impl)
		f.tellErr["rev-1"] = errors.New("not running")
		f.enforceFn = func(b Bead, action, by string) (string, error) {
			return cfg.enforce(b, action, by)
		}

		curHead := testHeadSHA1
		var tickerEvents []Event
		trigEnv := reviewLoopTriggerEnv{
			resolveHead: func(th Thread) (string, bool) { return curHead, true },
			autoRequest: func(b Bead, headSHA, prURL string) (ReviewRequestResult, error) {
				return requestReview(cfg, f.env(), b.ID, ReviewRequestOpts{
					HeadSHA:       headSHA,
					PRURL:         prURL,
					SpawnReviewer: true,
					By:            "ticker",
				})
			},
			event: func(e Event) error { tickerEvents = append(tickerEvents, e); return nil },
		}

		threads := []Thread{{Bead: impl, Agent: &Agent{Name: "task-auto", Status: "idle"}}}
		lines1 := checkReviewLoopsPass(cfg, trigEnv, threads, now)
		if len(lines1) != 1 || !strings.Contains(lines1[0], "auto-triggered:") {
			t.Fatalf("expected auto-triggered line on round 1, got %v", lines1)
		}
		if len(tickerEvents) != 0 {
			t.Fatalf("successful auto-trigger should not emit redundant review_needed inbox event, got %+v", tickerEvents)
		}
		if len(f.created) != 1 || len(f.dispatches) != 1 {
			t.Fatalf("expected 1 spawned & dispatched reviewer, got created=%d dispatches=%d", len(f.created), len(f.dispatches))
		}

		// Head-SHA dedup: repeated ticks on sha1 do nothing.
		linesDup := checkReviewLoopsPass(cfg, trigEnv, threads, now.Add(time.Minute))
		if len(linesDup) != 0 || len(f.created) != 1 || len(f.dispatches) != 1 {
			t.Fatalf("duplicate tick on same head should be no-op: lines=%v created=%d", linesDup, len(f.created))
		}

		// Round 1 cannot_merge, then new open-PR head sha2 -> auto-triggers round 2/2.
		if _, err := recordReviewVerdict(f.env(), "task-auto", ReviewVerdictInput{
			Verdict:  "cannot_merge",
			HeadSHA:  testHeadSHA1,
			Summary:  "Edge case panic",
			Findings: []ReviewFinding{{File: "parser.go", Line: 14, Summary: "nil deref on empty input"}},
		}); err != nil {
			t.Fatalf("round 1 verdict: %v", err)
		}
		delete(f.tellErr, "rev-1") // reviewer is now live
		curHead = testHeadSHA2
		threads[0].PR = &PR{State: "OPEN", HeadSHA: testHeadSHA2, URL: "https://github.com/example/repo/pull/42"}

		lines2 := checkReviewLoopsPass(cfg, trigEnv, threads, now.Add(2*time.Minute))
		if len(lines2) != 1 || !strings.Contains(lines2[0], "round 2/2") {
			t.Fatalf("expected round 2/2 auto-trigger on sha2, got %v", lines2)
		}
		st, ok, err := loadReviewLoop("task-auto")
		if err != nil || !ok || st.CurrentRound != 2 || st.State != reviewStatePending || st.Rounds[1].PRURL != "https://github.com/example/repo/pull/42" {
			t.Fatalf("unexpected round 2 state: ok=%v err=%v st=%+v", ok, err, st)
		}
	})

	t.Run("fails closed below class 3 or over dispatch budget and falls back to one review_needed event", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		cfg.Autonomy.Level = 5
		cfg.Autonomy.Max = 7

		lowBead := Bead{ID: "task-low", Title: "Low autonomy bead", Status: StatusInProgress, Labels: []string{reviewRequiredLabel, "autonomy:2"}}
		f := newFakeReviewEnv(lowBead)
		f.enforceFn = func(b Bead, action, by string) (string, error) {
			return cfg.enforce(b, action, by)
		}

		var tickerEvents []Event
		trigEnv := reviewLoopTriggerEnv{
			resolveHead: func(th Thread) (string, bool) { return testHeadSHA1, true },
			autoRequest: func(b Bead, headSHA, prURL string) (ReviewRequestResult, error) {
				return requestReview(cfg, f.env(), b.ID, ReviewRequestOpts{
					HeadSHA:       headSHA,
					PRURL:         prURL,
					SpawnReviewer: true,
					By:            "ticker",
				})
			},
			event: func(e Event) error { tickerEvents = append(tickerEvents, e); return nil },
		}
		threads := []Thread{{Bead: lowBead, Agent: &Agent{Name: "task-low", Status: "idle"}}}
		checkReviewLoopsPass(cfg, trigEnv, threads, now)
		checkReviewLoopsPass(cfg, trigEnv, threads, now.Add(time.Minute))
		if len(f.created) != 0 || len(f.dispatches) != 0 {
			t.Fatalf("autonomy:2 bead must not spawn reviewer, got created=%d dispatches=%d", len(f.created), len(f.dispatches))
		}
		if len(tickerEvents) != 1 || tickerEvents[0].Kind != EventReviewNeeded || !strings.Contains(tickerEvents[0].Summary, "auto-trigger skipped") {
			t.Fatalf("expected 1 fallback review_needed event with skip reason, got %+v", tickerEvents)
		}

		// Exhaust global dispatch budget and verify L5 bead also fails closed without spawning.
		cfg.Autonomy.Budget.Dispatches = 1
		if err := appendJournal(journalRecord{At: time.Now(), Kind: journalCheck, Bead: "other", Class: 3, Action: "dispatch", Pass: true}); err != nil {
			t.Fatal(err)
		}
		budgetBead := Bead{ID: "task-budget", Title: "Budgeted bead", Status: StatusInProgress, Labels: []string{reviewRequiredLabel}}
		f.beads[budgetBead.ID] = budgetBead
		threads2 := []Thread{{Bead: budgetBead, Agent: &Agent{Name: "task-budget", Status: "idle"}}}
		checkReviewLoopsPass(cfg, trigEnv, threads2, now.Add(2*time.Minute))
		if len(f.created) != 0 || len(f.dispatches) != 0 {
			t.Fatalf("budget-exhausted bead must not spawn reviewer, got created=%d", len(f.created))
		}
		if len(tickerEvents) != 2 || !strings.Contains(tickerEvents[1].Summary, "budget:") {
			t.Fatalf("expected budget fallback review_needed event, got %+v", tickerEvents)
		}
	})

	t.Run("respects auto_trigger = false and held/headless guards", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		off := false
		cfg := defaultConfig()
		cfg.Review.AutoTrigger = &off

		impl := Bead{ID: "task-off", Title: "Manual review only", Status: StatusInProgress, Labels: []string{reviewRequiredLabel}}
		autoCalls := 0
		var events []Event
		trigEnv := reviewLoopTriggerEnv{
			resolveHead: func(th Thread) (string, bool) { return testHeadSHA1, true },
			autoRequest: func(b Bead, headSHA, prURL string) (ReviewRequestResult, error) {
				autoCalls++
				return ReviewRequestResult{}, nil
			},
			event: func(e Event) error { events = append(events, e); return nil },
		}
		checkReviewLoopsPass(cfg, trigEnv, []Thread{{Bead: impl}}, now)
		if autoCalls != 0 || len(events) != 1 || events[0].Kind != EventReviewNeeded {
			t.Fatalf("auto_trigger=false should emit review_needed without autoRequest: calls=%d events=%+v", autoCalls, events)
		}
	})
}

func TestReviewCanMergeCleanupGuardsAndDeferredResolve(t *testing.T) {
	t.Run("manual non-spawned reviewer is not auto-closed", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		impl := Bead{ID: "task-m", Title: "Manual reviewer test", Status: StatusInProgress}
		manualRev := Bead{ID: "rev-manual", Title: "Human-managed reviewer", Status: StatusInProgress, Labels: []string{"role:reviewer"}}
		f := newFakeReviewEnv(impl, manualRev)

		if _, err := requestReview(cfg, f.env(), "task-m", ReviewRequestOpts{HeadSHA: testHeadSHA1, ReviewerBead: "rev-manual"}); err != nil {
			t.Fatalf("requestReview: %v", err)
		}
		res, err := recordReviewVerdict(f.env(), "task-m", ReviewVerdictInput{
			Verdict: "can_merge",
			HeadSHA: testHeadSHA1,
			Summary: "Looks good",
		})
		if err != nil {
			t.Fatalf("recordReviewVerdict: %v", err)
		}
		if len(res.ReviewerClosed) != 0 || len(f.closed) != 0 || len(f.resolved) != 0 {
			t.Fatalf("manual non-spawned reviewer should not be auto-closed/resolved: res=%+v closed=%v resolved=%v", res, f.closed, f.resolved)
		}
	})

	t.Run("never closes or resolves implementer bead even on corrupted state", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		impl := Bead{ID: "task-safe", Title: "Must stay open", Status: StatusInProgress}
		otherImpl := Bead{ID: "task-other-impl", Title: "Another implementer", Status: StatusInProgress}
		f := newFakeReviewEnv(impl, otherImpl)

		if err := saveReviewLoop(ReviewLoopState{
			Version:          reviewLoopSchemaVersion,
			Bead:             "task-safe",
			ReviewerBead:     "task-safe",
			ReviewerSpawned:  true,
			SpawnedReviewers: []string{"task-safe", "task-other-impl"},
			MaxRounds:        2,
			State:            reviewStatePending,
			CurrentRound:     1,
			LastHeadSHA:      testHeadSHA1,
			Rounds:           []ReviewRound{{Round: 1, HeadSHA: testHeadSHA1}},
		}); err != nil {
			t.Fatal(err)
		}

		res, err := recordReviewVerdict(f.env(), "task-safe", ReviewVerdictInput{
			Verdict: "can_merge",
			HeadSHA: testHeadSHA1,
			Summary: "Verified",
		})
		if err != nil {
			t.Fatalf("recordReviewVerdict: %v", err)
		}
		if len(f.closed) != 0 || len(f.resolved) != 0 || len(res.ReviewerClosed) != 0 {
			t.Fatalf("must never close or resolve implementer beads: closed=%v resolved=%v", f.closed, f.resolved)
		}
		if f.beads["task-safe"].Status != StatusInProgress || f.beads["task-other-impl"].Status != StatusInProgress {
			t.Fatalf("implementer statuses modified: %+v", f.beads)
		}
	})

	t.Run("defers resolve when reviewer agent is still working and finishes on next ticker pass", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		impl := Bead{ID: "task-def", Title: "Deferred resolve", Status: StatusInProgress}
		f := newFakeReviewEnv(impl)
		f.tellErr["rev-1"] = errors.New("not running")

		if _, err := requestReview(cfg, f.env(), "task-def", ReviewRequestOpts{HeadSHA: testHeadSHA1, SpawnReviewer: true}); err != nil {
			t.Fatalf("requestReview: %v", err)
		}
		// Simulate the reviewer agent invoking `quartermaster review verdict` mid-turn.
		f.resolveErr["rev-1"] = errors.New("rev-1's agent is still working")

		res, err := recordReviewVerdict(f.env(), "task-def", ReviewVerdictInput{
			Verdict: "can_merge",
			HeadSHA: testHeadSHA1,
			Summary: "Clean diff and passing tests",
		})
		if err != nil {
			t.Fatalf("recordReviewVerdict should succeed even when resolve defers: %v", err)
		}
		if !slices.Equal(res.ReviewerClosed, []string{"rev-1"}) || len(res.ReviewerResolved) != 0 {
			t.Fatalf("expected rev-1 closed and resolve deferred: closed=%v resolved=%v", res.ReviewerClosed, res.ReviewerResolved)
		}
		if !slices.Equal(res.State.PendingResolveBeads, []string{"rev-1"}) {
			t.Fatalf("PendingResolveBeads = %v, want [rev-1]", res.State.PendingResolveBeads)
		}

		// Reviewer finishes its turn -> next ticker pass resolves rev-1 and clears PendingResolveBeads.
		delete(f.resolveErr, "rev-1")
		trigEnv := reviewLoopTriggerEnv{
			show:            f.env().show,
			resolveReviewer: f.env().resolveReviewer,
		}
		lines := checkReviewLoopsPass(cfg, trigEnv, nil, f.now.Add(15*time.Second))
		if len(lines) != 1 || !strings.Contains(lines[0], "resolved finished reviewer rev-1") {
			t.Fatalf("expected deferred resolve on next tick, got %v", lines)
		}
		if !slices.Equal(f.resolved, []string{"rev-1"}) {
			t.Fatalf("resolved = %v, want [rev-1]", f.resolved)
		}
		st, ok, err := loadReviewLoop("task-def")
		if err != nil || !ok || len(st.PendingResolveBeads) != 0 {
			t.Fatalf("PendingResolveBeads should be empty after deferred resolve: ok=%v err=%v pending=%v", ok, err, st.PendingResolveBeads)
		}
	})

	t.Run("skips cleanup when reviewer bead is below class 3 without failing verdict", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
		cfg := defaultConfig()
		cfg.Autonomy.Level = 5
		impl := Bead{ID: "task-lowrev", Title: "Low autonomy reviewer cleanup", Status: StatusInProgress}
		f := newFakeReviewEnv(impl)
		f.tellErr["rev-1"] = errors.New("not running")

		if _, err := requestReview(cfg, f.env(), "task-lowrev", ReviewRequestOpts{HeadSHA: testHeadSHA1, SpawnReviewer: true}); err != nil {
			t.Fatalf("requestReview: %v", err)
		}
		// Lower spawned reviewer bead to autonomy:2 before verdict.
		rb := f.beads["rev-1"]
		rb.Labels = append(rb.Labels, "autonomy:2")
		f.beads["rev-1"] = rb
		f.enforceFn = func(b Bead, action, by string) (string, error) {
			return cfg.enforce(b, action, by)
		}

		res, err := recordReviewVerdict(f.env(), "task-lowrev", ReviewVerdictInput{
			Verdict: "can_merge",
			HeadSHA: testHeadSHA1,
			Summary: "Approved",
		})
		if err != nil {
			t.Fatalf("verdict should succeed even when cleanup is skipped: %v", err)
		}
		if len(res.ReviewerClosed) != 0 || len(res.ReviewerResolved) != 0 || !strings.Contains(res.ReviewerCleanup, "cleanup skipped") {
			t.Fatalf("expected cleanup skipped below class 3, got res=%+v", res)
		}
		if f.beads["rev-1"].Status == StatusClosed {
			t.Fatal("rev-1 at autonomy:2 should remain open")
		}
	})
}
