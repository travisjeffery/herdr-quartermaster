package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDesktopMilestonePRMerged(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

	open := &PR{
		Number:  42,
		Title:   "Add caching layer",
		URL:     "https://github.com/acme/app/pull/42",
		State:   "OPEN",
		HeadSHA: "aaa",
	}
	merged := *open
	merged.State = "MERGED"

	thOpen := thread(Bead{ID: "task-merge", Status: StatusInProgress, Title: "Caching"}, agent("idle", 1), open)
	thMerged := thread(Bead{ID: "task-merge", Status: StatusInProgress, Title: "Caching"}, agent("idle", 1), &merged)

	// First sight of an already-merged PR is baselined and emits no milestone.
	if ms := desktopMilestonesForTransition(thMerged, Snapshot{}, true, DesktopConfig{}); len(ms) != 0 {
		t.Fatalf("first sight emitted milestones: %+v", ms)
	}

	prev := snapshot(thOpen, Snapshot{}, now)
	ms := desktopMilestonesForTransition(thMerged, prev, false, DesktopConfig{})
	if len(ms) != 1 || ms[0].Reason != "pr_merged" {
		t.Fatalf("want 1 pr_merged milestone, got %+v", ms)
	}

	var gotTitle, gotBody string
	calls := 0
	submit := func(title, body string) desktopResult {
		calls++
		gotTitle, gotBody = title, body
		return desktopResult{"submitted", "notifier-exited-zero"}
	}
	if err := reconcileDesktopWith([]Thread{thMerged}, ms, DesktopConfig{}, now, submit); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if gotTitle != "Quartermaster: PR merged" {
		t.Fatalf("title = %q", gotTitle)
	}
	if !strings.Contains(gotBody, "task-merge") || !strings.Contains(gotBody, "acme/app#42 merged") || !strings.Contains(gotBody, "Add caching layer") {
		t.Fatalf("body = %q", gotBody)
	}

	// Re-emitting the exact same milestone key does not re-notify even after cooldown.
	if err := reconcileDesktopWith([]Thread{thMerged}, ms, DesktopConfig{}, now.Add(2*time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("duplicate pr_merged fired: calls=%d", calls)
	}

	// Snapshot after merge stops producing new merge milestones.
	prev = snapshot(thMerged, prev, now)
	if ms2 := desktopMilestonesForTransition(thMerged, prev, false, DesktopConfig{}); len(ms2) != 0 {
		t.Fatalf("already-merged PR emitted again: %+v", ms2)
	}

	// Receipt settles inactive after submission.
	j, err := loadDesktop()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := j.Items[ms[0].Key]
	if !ok || r.Active || r.State != "submitted" || r.Attempts != 1 {
		t.Fatalf("unexpected receipt: %+v (ok=%v)", r, ok)
	}
}

func TestDesktopMilestonePRGreenAndReadyToMerge(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

	// Start with checks pending -> baseline snapshot has GreenTracked=true, GreenKey="".
	pending := &PR{
		Number:         17,
		Title:          "Refactor scheduler",
		URL:            "https://github.com/acme/app/pull/17",
		State:          "OPEN",
		HeadSHA:        "sha1",
		ReviewDecision: "REVIEW_REQUIRED",
		MergeState:     "BLOCKED",
		Checks:         []Check{{Name: "test", Status: "IN_PROGRESS"}},
		DetailsFresh:   true,
	}
	prev := snapshot(thread(Bead{ID: "task-pr", Status: StatusInProgress}, agent("idle", 1), pending), Snapshot{}, now)
	if !prev.GreenTracked || prev.GreenKey != "" {
		t.Fatalf("bad initial snapshot: %+v", prev)
	}

	// Draft PR with passing checks must not emit pr_green.
	draft := *pending
	draft.IsDraft = true
	draft.Checks = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}
	if ms := desktopMilestonesForTransition(thread(Bead{ID: "task-pr", Status: StatusInProgress}, agent("idle", 1), &draft), prev, false, DesktopConfig{}); len(ms) != 0 {
		t.Fatalf("draft PR emitted milestone: %+v", ms)
	}

	// PR with CHANGES_REQUESTED or DIRTY or open threads must not emit pr_green.
	for _, mutate := range []func(*PR){
		func(p *PR) { p.ReviewDecision = "CHANGES_REQUESTED" },
		func(p *PR) { p.MergeState = "DIRTY" },
		func(p *PR) { p.Gate = &MergeGate{Head: "sha1", OpenThreads: 2} },
		func(p *PR) { p.Checks = nil },
		func(p *PR) {
			p.Checks = []Check{
				{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
			}
		},
	} {
		bad := *pending
		bad.Checks = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}
		mutate(&bad)
		if ms := desktopMilestonesForTransition(thread(Bead{ID: "task-pr", Status: StatusInProgress}, agent("idle", 1), &bad), prev, false, DesktopConfig{}); len(ms) != 0 {
			t.Fatalf("ineligible PR emitted milestone: %+v for %+v", ms, bad)
		}
	}

	// Pre-upgrade snapshot (GreenTracked=false) must baseline without firing pr_green.
	green := *pending
	green.Checks = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}
	thGreen := thread(Bead{ID: "task-pr", Status: StatusInProgress}, agent("idle", 1), &green)
	legacyPrev := Snapshot{Group: GroupReview, PRNumber: 17, PRState: "OPEN", ReviewsSplit: true}
	if ms := desktopMilestonesForTransition(thGreen, legacyPrev, false, DesktopConfig{}); len(ms) != 0 {
		t.Fatalf("pre-upgrade snapshot spuriously fired pr_green: %+v", ms)
	}

	// Transition from pending -> green awaiting review fires pr_green once.
	ms := desktopMilestonesForTransition(thGreen, prev, false, DesktopConfig{})
	if len(ms) != 1 || ms[0].Reason != "pr_green" {
		t.Fatalf("want 1 pr_green milestone, got %+v", ms)
	}
	var gotTitle, gotBody string
	submit := func(title, body string) desktopResult {
		gotTitle, gotBody = title, body
		return desktopResult{"submitted", "notifier-exited-zero"}
	}
	if err := reconcileDesktopWith([]Thread{thGreen}, ms, DesktopConfig{}, now, submit); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "Quartermaster: PR green, awaiting review" {
		t.Fatalf("title = %q", gotTitle)
	}
	if !strings.Contains(gotBody, "task-pr: acme/app#17 green, awaiting review") {
		t.Fatalf("body = %q", gotBody)
	}

	// Same head stays quiet on subsequent ticks.
	prev = snapshot(thGreen, prev, now)
	if ms2 := desktopMilestonesForTransition(thGreen, prev, false, DesktopConfig{}); len(ms2) != 0 {
		t.Fatalf("same green head re-fired: %+v", ms2)
	}

	// Reviewer approves -> becomes ready_to_merge -> fires pr_ready (not pr_green).
	ready := readyPR()
	ready.Number = 17
	ready.URL = "https://github.com/acme/app/pull/17"
	ready.HeadSHA = "sha1"
	ready.Gate.Head = "sha1"
	ready.Reviews = []Review{approvalOf("sha1", now)}
	ready.Gate.Reviews = ready.Reviews
	ready.Gate.At = now
	thReady := thread(Bead{ID: "task-pr", Status: StatusInProgress}, agent("idle", 1), ready)
	msReady := desktopMilestonesForTransition(thReady, prev, false, DesktopConfig{})
	if len(msReady) != 1 || msReady[0].Reason != "pr_ready" {
		t.Fatalf("want 1 pr_ready milestone, got %+v", msReady)
	}
	if err := reconcileDesktopWith([]Thread{thReady}, msReady, DesktopConfig{}, now.Add(2*time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "Quartermaster: PR ready to merge" {
		t.Fatalf("ready title = %q", gotTitle)
	}
	if !strings.Contains(gotBody, "task-pr: acme/app#17 ready to merge (approved)") {
		t.Fatalf("ready body = %q", gotBody)
	}

	// When a new head becomes simultaneously green AND ready to merge, only pr_ready fires.
	prev = snapshot(thReady, prev, now.Add(2*time.Minute))
	pushedReady := readyPR()
	pushedReady.Number = 17
	pushedReady.URL = "https://github.com/acme/app/pull/17"
	pushedReady.HeadSHA = "sha2"
	pushedReady.Gate.Head = "sha2"
	pushedReady.Reviews = []Review{approvalOf("sha2", now.Add(3*time.Minute))}
	pushedReady.Gate.Reviews = pushedReady.Reviews
	pushedReady.Gate.At = now.Add(3 * time.Minute)
	pushedReady.AutoMerge = &AutoMerge{Method: "SQUASH"}
	thPushed := thread(Bead{ID: "task-pr", Status: StatusInProgress}, agent("idle", 1), pushedReady)
	msBoth := desktopMilestonesForTransition(thPushed, prev, false, DesktopConfig{})
	if len(msBoth) != 1 || msBoth[0].Reason != "pr_ready" {
		t.Fatalf("simultaneous green+ready should emit only pr_ready, got %+v", msBoth)
	}
	if !strings.Contains(msBoth[0].Summary, "auto-merge on") {
		t.Fatalf("expected auto-merge detail in summary, got %q", msBoth[0].Summary)
	}

	// Personal repo with no required review (ReviewDecision=="", MergeState=="CLEAN", passing checks)
	// emits pr_green with "green, ready for review".
	personalPrev := Snapshot{Group: GroupWorking, PRNumber: 9, PRState: "OPEN", GreenTracked: true}
	personalPR := &PR{
		Number:       9,
		Title:        "Improve notifications",
		URL:          "https://github.com/user/tool/pull/9",
		State:        "OPEN",
		HeadSHA:      "abc999",
		MergeState:   "CLEAN",
		Checks:       []Check{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}},
		DetailsFresh: true,
	}
	msPersonal := desktopMilestonesForTransition(thread(Bead{ID: "task-personal", Status: StatusInProgress}, agent("idle", 1), personalPR), personalPrev, false, DesktopConfig{})
	if len(msPersonal) != 1 || msPersonal[0].Reason != "pr_green" || !strings.Contains(msPersonal[0].Summary, "green, ready for review") {
		t.Fatalf("unexpected personal repo green milestone: %+v", msPersonal)
	}
}

func TestDesktopNeedsMeSummaryAndSanitization(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

	tapToken := "cXxiZWFkLTF8YWJjZGVmMTJ8MXwxNzAwMDAwMDAwfDAxMjM0NTY3ODlhYmNkZWY.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	cases := []struct {
		name      string
		bead      Bead
		wantSub   []string
		forbidSub []string
	}{
		{
			name: "structured QUESTION/OPTIONS with tap link",
			bead: Bead{
				ID:     "task-q1",
				Status: StatusNeedsMe,
				Title:  "Rollout decision",
				Notes: strings.Join([]string{
					"PR: https://github.com/acme/app/pull/5",
					"QUESTION: Ship to staging first or all regions? Recommendation: 1 — staging first. Tap: http://100.64.0.10:8642/tap/" + tapToken,
					"OPTIONS:",
					"1. Staging first",
					"2. All regions",
					"CLASS: 7 — prod rollout",
				}, "\n"),
			},
			wantSub:   []string{"task-q1:", "Ship to staging first or all regions?", "Recommendation: 1"},
			forbidSub: []string{"http://", "100.64.0.10", "/tap/", tapToken, "OPTIONS:"},
		},
		{
			name: "free-text note containing secrets and tokens",
			bead: Bead{
				ID:     "task-sec",
				Status: StatusNeedsMe,
				Title:  "Rotate credentials",
				Notes:  "Auth failed with api_key=sk-proj-supersecretvalue12345678, Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.signaturepart1234567890, ghp_1234567890abcdef123456, xoxb-999-888-secret, AKIAIOSFODNN7EXAMPLE, postgres://admin:topsecret@db.internal/prod, and bare " + tapToken + "; rotate?",
			},
			wantSub: []string{"task-sec:", "Auth failed with", "rotate?"},
			forbidSub: []string{
				"sk-proj-supersecretvalue12345678",
				"eyJhbGciOiJIUzI1NiJ9",
				"signaturepart1234567890",
				"ghp_1234567890abcdef123456",
				"xoxb-999-888-secret",
				"AKIAIOSFODNN7EXAMPLE",
				"topsecret",
				"postgres://",
				tapToken,
			},
		},
		{
			name: "tailnet MagicDNS, tap-links, inbox, and CLI flags scrubbed",
			bead: Bead{
				ID:     "task-tap",
				Status: StatusNeedsMe,
				Title:  "Check deploy",
				Notes:  "Approve deploy? http://myhost.tail1234.ts.net:8642/tap-links/task-tap /inbox/plan/deadbeef --token mySecretToken99 password=\"hunter2\" https://example.com/hook?sig=leakme",
			},
			wantSub:   []string{"task-tap:", "Approve deploy?"},
			forbidSub: []string{"ts.net", "8642", "tap-links", "/inbox/plan", "mySecretToken99", "hunter2", "https://", "leakme"},
		},
		{
			name: "RUN note prefers explanation line and strips secrets",
			bead: Bead{
				ID:     "task-run",
				Status: StatusNeedsMe,
				Title:  "Login required",
				Notes:  "RUN: curl -H 'Authorization: Bearer secret-bearer-val' https://api.example.com/auth\nRefresh interactive SSO login for prod cluster",
			},
			wantSub:   []string{"task-run:", "RUN requested — Refresh interactive SSO login for prod cluster"},
			forbidSub: []string{"secret-bearer-val", "https://", "Authorization"},
		},
		{
			name: "ANSI escapes, newlines, and bidi overrides stripped",
			bead: Bead{
				ID:     "task-ansi",
				Status: StatusNeedsMe,
				Title:  "Clean output",
				Notes:  "\x1b[31mShould we\u202e proceed\nwith\tcleanup?\x1b[0m",
			},
			wantSub:   []string{"task-ansi:", "Should we proceed with cleanup?"},
			forbidSub: []string{"\x1b", "\u202e", "\n", "\t"},
		},
		{
			name: "cookie header redacted without eating next question line",
			bead: Bead{
				ID:     "task-cookie",
				Status: StatusNeedsMe,
				Title:  "Auth issue",
				Notes:  "QUESTION: Cookie: sid=secretcookie123; theme=dark\nShould we rotate session cookies?",
			},
			wantSub:   []string{"task-cookie:", "Should we rotate session cookies?"},
			forbidSub: []string{"secretcookie123", "sid="},
		},
		{
			name: "note that is only a secret falls back to safe title",
			bead: Bead{
				ID:     "task-fallback",
				Status: StatusNeedsMe,
				Title:  "Approve key rotation",
				Notes:  "https://100.64.1.2:8642/tap/" + tapToken,
			},
			wantSub:   []string{"task-fallback:", "Approve key rotation"},
			forbidSub: []string{"http://", "https://", "100.64.1.2", tapToken},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
			var gotTitle, gotBody string
			submit := func(title, body string) desktopResult {
				gotTitle, gotBody = title, body
				return desktopResult{"submitted", "notifier-exited-zero"}
			}
			err := reconcileDesktopWith([]Thread{{Bead: tc.bead}}, nil, DesktopConfig{}, now.Add(time.Duration(i)*time.Minute), submit)
			if err != nil {
				t.Fatal(err)
			}
			if gotTitle != "Quartermaster: needs_me" {
				t.Fatalf("title = %q, want Quartermaster: needs_me", gotTitle)
			}
			if strings.Contains(gotBody, "\n") {
				t.Fatalf("single needs_me body is not 1 line: %q", gotBody)
			}
			for _, want := range tc.wantSub {
				if !strings.Contains(gotBody, want) {
					t.Errorf("body %q missing %q", gotBody, want)
				}
			}
			j, err := loadDesktop()
			if err != nil {
				t.Fatal(err)
			}
			r := j.Items[tc.bead.ID]
			rawJournal, err := os.ReadFile(desktopPath())
			if err != nil {
				t.Fatal(err)
			}
			for _, forbid := range tc.forbidSub {
				if strings.Contains(gotBody, forbid) {
					t.Errorf("body leaked forbidden substring %q: %q", forbid, gotBody)
				}
				if strings.Contains(r.Summary, forbid) {
					t.Errorf("receipt summary leaked forbidden substring %q: %q", forbid, r.Summary)
				}
				if forbid != "\n" && forbid != "\t" && strings.Contains(string(rawJournal), forbid) {
					t.Errorf("notifications.json leaked forbidden substring %q", forbid)
				}
			}
		})
	}
}

func TestDesktopNeedsMeNewQuestionVsBookkeepingAndUpgrade(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

	// Simulate an existing pre-upgrade receipt (PromptHash=="") for an active needs_me bead.
	if err := rpcPrivateDir(desktopDir()); err != nil {
		t.Fatal(err)
	}
	if err := rpcSave(desktopPath(), desktopJournal{
		Version: 1,
		LastTry: now.Add(-10 * time.Minute),
		Items: map[string]desktopReceipt{
			"task-existing": {
				Reason:   "needs_me",
				Detected: now.Add(-time.Hour),
				Active:   true,
				State:    "submitted",
				Attempts: 1,
				LastTry:  now.Add(-time.Hour),
				Result:   "notifier-exited-zero",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	var lastBody string
	submit := func(_, body string) desktopResult {
		calls++
		lastBody = body
		return desktopResult{"submitted", "notifier-exited-zero"}
	}

	// Upgrading adopts the question hash without re-notifying.
	th := Thread{Bead: Bead{ID: "task-existing", Status: StatusNeedsMe, Notes: "QUESTION: First question?"}}
	if err := reconcileDesktopWith([]Thread{th}, nil, DesktopConfig{}, now, submit); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("pre-upgrade needs_me re-notified: calls=%d", calls)
	}

	// Appending a bookkeeping note (AUTO:/PR:) does not trigger a duplicate alert.
	th.Bead.Notes += "\nAUTO: [worker L5 C2] pushed commit\nPR: https://github.com/acme/app/pull/1"
	if err := reconcileDesktopWith([]Thread{th}, nil, DesktopConfig{}, now.Add(2*time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("bookkeeping note triggered duplicate alert: calls=%d", calls)
	}

	// A genuinely new question on the same needs_me bead triggers a fresh notification.
	th.Bead.Notes += "\nQUESTION: Second distinct question?"
	if err := reconcileDesktopWith([]Thread{th}, nil, DesktopConfig{}, now.Add(4*time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(lastBody, "Second distinct question?") {
		t.Fatalf("new question missed: calls=%d body=%q", calls, lastBody)
	}
}

func TestDesktopCoalescingRateLimitingAndMilestoneRetry(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

	type call struct{ title, body string }
	var sent []call
	failNext := false
	submit := func(title, body string) desktopResult {
		if failNext {
			return desktopResult{"failed", "notifier-unavailable"}
		}
		sent = append(sent, call{title, body})
		return desktopResult{"submitted", "notifier-exited-zero"}
	}

	// T=0: first milestone fires immediately.
	m1 := desktopMilestone{Key: desktopMilestoneKey("pr_merged", "task-1", "#1"), Bead: "task-1", Reason: "pr_merged", Summary: "task-1: acme/app#1 merged"}
	if err := reconcileDesktopWith(nil, []desktopMilestone{m1}, DesktopConfig{}, now, submit); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].title != "Quartermaster: PR merged" {
		t.Fatalf("T=0 sent = %+v", sent)
	}

	// T=15s, 30s, 45s (inside 60s cooldown): burst of 3 milestones + needs_me queues without spamming.
	m2 := desktopMilestone{Key: desktopMilestoneKey("pr_merged", "task-2", "#2"), Bead: "task-2", Reason: "pr_merged", Summary: "task-2: acme/app#2 merged"}
	m3 := desktopMilestone{Key: desktopMilestoneKey("pr_green", "task-3", "3@sha"), Bead: "task-3", Reason: "pr_green", Summary: "task-3: acme/app#3 green, awaiting review"}
	needsTh := Thread{Bead: Bead{ID: "task-4", Status: StatusNeedsMe, Notes: "QUESTION: Proceed with cutover?"}}

	if err := reconcileDesktopWith(nil, []desktopMilestone{m2}, DesktopConfig{}, now.Add(15*time.Second), submit); err != nil {
		t.Fatal(err)
	}
	if err := reconcileDesktopWith(nil, []desktopMilestone{m3}, DesktopConfig{}, now.Add(30*time.Second), submit); err != nil {
		t.Fatal(err)
	}
	if err := reconcileDesktopWith([]Thread{needsTh}, nil, DesktopConfig{}, now.Add(45*time.Second), submit); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatalf("cooldown violated during burst: sent=%d", len(sent))
	}

	// T=60s: cooldown expires -> all 3 queued items coalesce into ONE notification even though milestones arg is nil.
	if err := reconcileDesktopWith([]Thread{needsTh}, nil, DesktopConfig{}, now.Add(60*time.Second), submit); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("queued burst did not coalesce on cooldown expiry: sent=%+v", sent)
	}
	if sent[1].title != "Quartermaster: 3 updates (1 needs_me)" {
		t.Fatalf("coalesced title = %q", sent[1].title)
	}
	for _, want := range []string{
		"needs_me task-4: Proceed with cutover?",
		"merged task-2: acme/app#2 merged",
		"green task-3: acme/app#3 green, awaiting review",
	} {
		if !strings.Contains(sent[1].body, want) {
			t.Errorf("coalesced body missing %q:\n%s", want, sent[1].body)
		}
	}
	if strings.Contains(sent[1].body, "\n") {
		t.Errorf("coalesced body should stay single-line: %q", sent[1].body)
	}

	// Failed milestone retries safely on next tick after backoff even when milestones slice is empty.
	failNext = true
	m5 := desktopMilestone{Key: desktopMilestoneKey("pr_ready", "task-5", "5@sha"), Bead: "task-5", Reason: "pr_ready", Summary: "task-5: acme/app#5 ready to merge (approved)"}
	if err := reconcileDesktopWith([]Thread{needsTh}, []desktopMilestone{m5}, DesktopConfig{}, now.Add(2*time.Minute), submit); err == nil {
		t.Fatal("expected notifier failure error")
	}
	failNext = false
	// Before 1m retry backoff: does not retry.
	if err := reconcileDesktopWith([]Thread{needsTh}, nil, DesktopConfig{}, now.Add(2*time.Minute+30*time.Second), submit); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("retried too early: sent=%d", len(sent))
	}
	// At +1m retry backoff: succeeds and settles inactive.
	if err := reconcileDesktopWith([]Thread{needsTh}, nil, DesktopConfig{}, now.Add(3*time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[2].title != "Quartermaster: PR ready to merge" {
		t.Fatalf("failed milestone retry lost: sent=%+v", sent)
	}
	j, _ := loadDesktop()
	if r := j.Items[m5.Key]; r.Active || r.State != "submitted" || r.Attempts != 2 {
		t.Fatalf("bad retried milestone receipt: %+v", r)
	}
}

func TestDesktopBurstTruncationAndUnknownMilestoneNoReplay(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

	var milestones []desktopMilestone
	for i := 1; i <= 7; i++ {
		id := "task-" + string(rune('a'+i-1))
		milestones = append(milestones, desktopMilestone{
			Key:     desktopMilestoneKey("pr_merged", id, id),
			Bead:    id,
			Reason:  "pr_merged",
			Summary: id + ": acme/app#1 merged",
		})
	}
	var gotTitle, gotBody string
	calls := 0
	submit := func(title, body string) desktopResult {
		calls++
		gotTitle, gotBody = title, body
		return desktopResult{"unknown", "notifier-timeout"}
	}
	if err := reconcileDesktopWith(nil, milestones, DesktopConfig{}, now, submit); err == nil {
		t.Fatal("expected timeout error")
	}
	if gotTitle != "Quartermaster: 7 PR updates" {
		t.Fatalf("title = %q", gotTitle)
	}
	parts := strings.Split(gotBody, " | ")
	if len(parts) != desktopMaxBodyLines+1 || !strings.Contains(parts[desktopMaxBodyLines], "+2 more") {
		t.Fatalf("expected %d entries + '+2 more', got:\n%s", desktopMaxBodyLines, gotBody)
	}
	// Ambiguous timeout settles milestones inactive and never replays them.
	for i := 1; i <= 5; i++ {
		if err := reconcileDesktopWith(nil, nil, DesktopConfig{}, now.Add(time.Duration(i)*time.Minute), submit); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("ambiguous milestone replayed: calls=%d", calls)
	}
	j, _ := loadDesktop()
	for _, m := range milestones {
		if r := j.Items[m.Key]; r.Active || r.State != "unknown" {
			t.Fatalf("unknown milestone still active: %+v", r)
		}
	}
}

func TestDesktopConfigTomlAndToggles(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	writeCfg := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("repo = \"/tmp/repo\"\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Defaults: all enabled, 60s cooldown.
	writeCfg("")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("default loadConfig: %v", err)
	}
	if !cfg.Desktop.enabled() || !cfg.Desktop.notifyPRMerged() || !cfg.Desktop.notifyPRGreen() || !cfg.Desktop.notifyPRReady() || !cfg.Desktop.notifyNeedsMe() || !cfg.Desktop.notifyBlocked() || !cfg.Desktop.includeNeedsMeSummary() || cfg.Desktop.cooldown() != time.Minute {
		t.Fatalf("unexpected desktop defaults: %+v", cfg.Desktop)
	}

	// Custom toggles and cooldown.
	writeCfg(`
[desktop]
pr_green = false
needs_me_summary = false
cooldown_seconds = 15
`)
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("custom loadConfig: %v", err)
	}
	if !cfg.Desktop.enabled() || !cfg.Desktop.notifyPRMerged() || cfg.Desktop.notifyPRGreen() || !cfg.Desktop.notifyPRReady() || !cfg.Desktop.notifyNeedsMe() || cfg.Desktop.includeNeedsMeSummary() || cfg.Desktop.cooldown() != 15*time.Second {
		t.Fatalf("unexpected custom desktop config: %+v", cfg.Desktop)
	}

	// needs_me_summary = false falls back to generic fixed wording.
	var gotTitle, gotBody string
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	th := Thread{Bead: Bead{ID: "task-private", Status: StatusNeedsMe, Notes: "QUESTION: Secret detail?"}}
	if err := reconcileDesktopWith([]Thread{th}, nil, cfg.Desktop, now, func(title, body string) desktopResult {
		gotTitle, gotBody = title, body
		return desktopResult{"submitted", "notifier-exited-zero"}
	}); err != nil {
		t.Fatal(err)
	}
	if gotTitle != desktopTitle || gotBody != desktopBody {
		t.Fatalf("needs_me_summary=false leaked detail: title=%q body=%q", gotTitle, gotBody)
	}

	// enabled = false suppresses all desktop notifications.
	writeCfg("\n[desktop]\nenabled = false\n")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("disabled loadConfig: %v", err)
	}
	calls := 0
	if err := reconcileDesktopWith([]Thread{th}, []desktopMilestone{{Key: "milestone:pr_merged:x:1", Bead: "x", Reason: "pr_merged", Summary: "x: #1 merged"}}, cfg.Desktop, now.Add(time.Minute), func(_, _ string) desktopResult {
		calls++
		return desktopResult{"submitted", "notifier-exited-zero"}
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("enabled=false still submitted: calls=%d", calls)
	}

	// Negative cooldown fails closed.
	writeCfg("\n[desktop]\ncooldown_seconds = -5\n")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "cooldown_seconds") {
		t.Fatalf("expected error on negative cooldown_seconds, got %v", err)
	}

	// Zero cooldown disables rate limiting.
	writeCfg("\n[desktop]\ncooldown_seconds = 0\n")
	cfg, err = loadConfig()
	if err != nil || cfg.Desktop.cooldown() != 0 {
		t.Fatalf("cooldown_seconds=0 failed: cfg=%+v err=%v", cfg.Desktop, err)
	}
}

func TestDesktopCommandMessageArgvSafety(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	logfile := filepath.Join(dir, "args")
	t.Setenv("NOTIFY_ARGS", logfile)
	script := filepath.Join(dir, "notify-send")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$NOTIFY_ARGS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	title := "Quartermaster: needs_me"
	body := "--urgency=critical task-1: leading dash summary"
	if got := submitDesktopMessage(title, body); got != (desktopResult{"submitted", "notifier-exited-zero"}) {
		t.Fatalf("submitDesktopMessage: %+v", got)
	}
	args, err := os.ReadFile(logfile)
	if err != nil {
		t.Fatal(err)
	}
	want := "--app-name\nquartermaster\n--\n" + title + "\n" + body + "\n"
	if string(args) != want {
		t.Fatalf("argv = %q, want %q", args, want)
	}
}

func TestDesktopTickerMilestonesEndToEnd(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("PATH", dir)
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	beadsFile := filepath.Join(dir, "beads.json")
	writeBeads := func(beads []Bead) {
		t.Helper()
		b, err := json.Marshal(beads)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(beadsFile, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notifyLog := filepath.Join(dir, "notify-payloads.log")
	t.Setenv("FIXTURE_BEADS_FILE", beadsFile)
	t.Setenv("NOTIFY_PAYLOADS", notifyLog)
	write("bd", "IFS= read -r line < \"$FIXTURE_BEADS_FILE\"\nprintf '%s\\n' \"$line\"\n")
	write("herdr", `case "$*" in
*"agent list"*) printf '{"result":{"agents":[]}}\n' ;;
*) printf '{"result":{}}\n' ;;
esac
`)
	write("notify-send", "printf '%s\\n---\\n' \"$*\" >> \"$NOTIFY_PAYLOADS\"\n")

	now := time.Now()
	cfg := defaultConfig()
	cfg.Repo = t.TempDir()
	zero := 0
	cfg.Desktop.CooldownSeconds = &zero

	var logs bytes.Buffer
	tk := newTicker(cfg, log.New(&logs, "", 0))
	st := loadState()
	st.LastGH = now // keep PR fixtures controlled directly in st.PRs

	// Tick 1: baseline two open PRs with pending checks.
	writeBeads([]Bead{
		{ID: "task-a", Status: StatusInProgress, Title: "Feature A"},
		{ID: "task-b", Status: StatusInProgress, Title: "Feature B"},
	})
	st.PRs["task-a"] = PR{
		Number: 10, Title: "Feature A PR", URL: "https://github.com/acme/app/pull/10",
		State: "OPEN", HeadSHA: "shaA1", ReviewDecision: "REVIEW_REQUIRED", MergeState: "BLOCKED",
		Checks: []Check{{Name: "ci", Status: "IN_PROGRESS"}}, DetailsFresh: true,
	}
	st.PRs["task-b"] = PR{
		Number: 11, Title: "Feature B PR", URL: "https://github.com/acme/app/pull/11",
		State: "OPEN", HeadSHA: "shaB1", ReviewDecision: "APPROVED", MergeState: "CLEAN",
		Checks: []Check{{Name: "ci", Status: "IN_PROGRESS"}}, DetailsFresh: true,
	}
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if _, err := os.Stat(notifyLog); !os.IsNotExist(err) {
		t.Fatalf("baseline tick should not send notifications")
	}

	// Tick 2: task-a PR turns green awaiting review, task-b PR merges, task-c enters needs_me (with tap link in notes).
	st.LastGH = time.Now()
	st.PRs["task-a"] = PR{
		Number: 10, Title: "Feature A PR", URL: "https://github.com/acme/app/pull/10",
		State: "OPEN", HeadSHA: "shaA1", ReviewDecision: "REVIEW_REQUIRED", MergeState: "BLOCKED",
		Checks: []Check{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}}, DetailsFresh: true,
	}
	st.PRs["task-b"] = PR{
		Number: 11, Title: "Feature B PR", URL: "https://github.com/acme/app/pull/11",
		State: "MERGED", HeadSHA: "shaB1",
	}
	writeBeads([]Bead{
		{ID: "task-a", Status: StatusInProgress, Title: "Feature A"},
		{ID: "task-b", Status: StatusInProgress, Title: "Feature B"},
		{ID: "task-c", Status: StatusNeedsMe, Title: "Decision C", Notes: "QUESTION: Ship canary now? Tap: http://100.64.0.9:8642/tap/cXx0YXNrLWN8aGFzaHwyfDE3MDAwMDAwMDB8bm9uY2U.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"},
	})
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	raw, err := os.ReadFile(notifyLog)
	if err != nil {
		t.Fatalf("expected notify-send invocation: %v", err)
	}
	out := string(raw)
	if strings.Count(out, "---\n") != 1 {
		t.Fatalf("expected 1 coalesced notify-send call, got:\n%s", out)
	}
	for _, want := range []string{
		"Quartermaster: 3 updates (1 needs_me)",
		"needs_me task-c: Ship canary now?",
		"merged task-b: acme/app#11 merged — Feature B PR",
		"green task-a: acme/app#10 green, awaiting review — Feature A PR",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in notify-send output:\n%s", want, out)
		}
	}
	for _, forbid := range []string{"http://", "100.64.0.9", "/tap/", "BBBBBBBB"} {
		if strings.Contains(out, forbid) {
			t.Errorf("leaked %q in notify-send output:\n%s", forbid, out)
		}
	}

	// Tick 3: nothing changed -> no duplicate desktop notifications.
	st.LastGH = time.Now()
	if err := tk.once(&st); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	raw2, _ := os.ReadFile(notifyLog)
	if string(raw2) != out {
		t.Fatalf("tick 3 emitted duplicate notification:\n%s", string(raw2))
	}
}
