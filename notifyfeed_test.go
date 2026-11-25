package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func tapEventsRequest(s *tapServer, method, target, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestNotifyFeedRingBufferPersistenceTruncationAndPagination(t *testing.T) {
	_, _, stDir := setupNotifyTestEnv(t)

	// Empty state dir: loadNotifyFeed is read-only and creates nothing.
	emptyFeed, err := loadNotifyFeed()
	if err != nil {
		t.Fatalf("loadNotifyFeed empty: %v", err)
	}
	if emptyFeed.LatestSeq != 0 || len(emptyFeed.Events) != 0 {
		t.Fatalf("unexpected empty feed: %+v", emptyFeed)
	}
	if _, err := os.Stat(filepath.Join(stDir, "notify")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadNotifyFeed should not create notify dir, stat err = %v", err)
	}

	// Empty feed queries: since=0 is not truncated; since>0 (cursor ahead of reset state) IS truncated.
	r0 := queryNotifyFeed(emptyFeed, NotifyEventsQuery{Since: 0, Limit: 100})
	if r0.Version != 1 || r0.LatestSeq != 0 || r0.NextSince != 0 || r0.HasMore || r0.Truncated || r0.Events == nil || len(r0.Events) != 0 {
		t.Fatalf("unexpected empty since=0 response: %+v", r0)
	}
	rAheadEmpty := queryNotifyFeed(emptyFeed, NotifyEventsQuery{Since: 10, Limit: 100})
	if !rAheadEmpty.Truncated || rAheadEmpty.NextSince != 0 || rAheadEmpty.LatestSeq != 0 || len(rAheadEmpty.Events) != 0 {
		t.Fatalf("expected truncated=true and next_since=0 for since>0 on empty feed, got %+v", rAheadEmpty)
	}

	// Append 505 events across two batches (simulating restarts/reloads) to test monotonic seq and 500-event eviction.
	t0 := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	kinds := []string{"pr_merged", "pr_green", "pr_ready", "needs_me", "blocked"}
	batch1 := make([]NotifyEnvelope, 0, 300)
	for i := 0; i < 300; i++ {
		k := kinds[i%len(kinds)]
		batch1 = append(batch1, NotifyEnvelope{
			Version: 1,
			Event:   k,
			At:      t0.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			Title:   fmt.Sprintf("Event %d", i+1),
			Body:    fmt.Sprintf("Body %d", i+1),
		})
	}
	if err := appendNotifyFeedEvents(batch1); err != nil {
		t.Fatalf("append batch1: %v", err)
	}

	// Verify 0700 dir and 0600 file permissions.
	dirInfo, err := os.Stat(notifyDir())
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("notify dir mode = %v, err = %v (want 0700)", dirInfo, err)
	}
	fileInfo, err := os.Stat(notifyEventsPath())
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("events.json mode = %v, err = %v (want 0600)", fileInfo, err)
	}

	batch2 := make([]NotifyEnvelope, 0, 205)
	for i := 300; i < 505; i++ {
		k := kinds[i%len(kinds)]
		batch2 = append(batch2, NotifyEnvelope{
			Version: 1,
			Event:   k,
			At:      t0.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			Title:   fmt.Sprintf("Event %d", i+1),
			Body:    fmt.Sprintf("Body %d", i+1),
		})
	}
	if err := appendNotifyFeedEvents(batch2); err != nil {
		t.Fatalf("append batch2: %v", err)
	}

	// Reload from disk: latest_seq == 505, ring buffer retains seq 6..505 (500 events).
	reloaded, err := loadNotifyFeed()
	if err != nil {
		t.Fatalf("reload feed: %v", err)
	}
	if reloaded.LatestSeq != 505 || len(reloaded.Events) != notifyFeedMaxEvents {
		t.Fatalf("reloaded latest_seq=%d len=%d (want 505, %d)", reloaded.LatestSeq, len(reloaded.Events), notifyFeedMaxEvents)
	}
	if reloaded.Events[0].Seq != 6 || reloaded.Events[len(reloaded.Events)-1].Seq != 505 {
		t.Fatalf("retained seq bounds = [%d..%d], want [6..505]", reloaded.Events[0].Seq, reloaded.Events[len(reloaded.Events)-1].Seq)
	}
	for i := 1; i < len(reloaded.Events); i++ {
		if reloaded.Events[i].Seq != reloaded.Events[i-1].Seq+1 {
			t.Fatalf("non-monotonic seq at %d: %d followed by %d", i, reloaded.Events[i-1].Seq, reloaded.Events[i].Seq)
		}
	}

	// Truncation boundary checks (oldestSeq = 6, latestSeq = 505):
	// since=0 -> fresh client, not truncated.
	respFresh := queryNotifyFeed(reloaded, NotifyEventsQuery{Since: 0, Limit: 2})
	if respFresh.Truncated || !respFresh.HasMore || respFresh.NextSince != 7 || len(respFresh.Events) != 2 || respFresh.Events[0].Seq != 6 || respFresh.Events[1].Seq != 7 {
		t.Fatalf("since=0 limit=2: %+v", respFresh)
	}
	// since=4 (< oldestSeq-1 = 5) -> missed seq 5 -> truncated=true.
	respEvicted := queryNotifyFeed(reloaded, NotifyEventsQuery{Since: 4, Limit: 10})
	if !respEvicted.Truncated || !respEvicted.HasMore || respEvicted.NextSince != 15 || respEvicted.Events[0].Seq != 6 {
		t.Fatalf("since=4 (evicted): %+v", respEvicted)
	}
	// since=5 (== oldestSeq-1) -> next needed is seq 6, zero events missed -> truncated=false.
	respBoundary := queryNotifyFeed(reloaded, NotifyEventsQuery{Since: 5, Limit: 10})
	if respBoundary.Truncated || !respBoundary.HasMore || respBoundary.NextSince != 15 || respBoundary.Events[0].Seq != 6 {
		t.Fatalf("since=5 (exact boundary): %+v", respBoundary)
	}
	// since=505 (caught up) -> empty, next_since=505, truncated=false, has_more=false.
	respCaughtUp := queryNotifyFeed(reloaded, NotifyEventsQuery{Since: 505, Limit: 100})
	if respCaughtUp.Truncated || respCaughtUp.HasMore || respCaughtUp.NextSince != 505 || len(respCaughtUp.Events) != 0 {
		t.Fatalf("since=505 (caught up): %+v", respCaughtUp)
	}
	// since=600 (> latest_seq) -> truncated=true, next_since=505 so client resyncs cleanly.
	respAhead := queryNotifyFeed(reloaded, NotifyEventsQuery{Since: 600, Limit: 100})
	if !respAhead.Truncated || respAhead.HasMore || respAhead.NextSince != 505 || respAhead.LatestSeq != 505 || len(respAhead.Events) != 0 {
		t.Fatalf("since=600 (ahead): %+v", respAhead)
	}

	// Event filtering + pagination.
	respFiltered := queryNotifyFeed(reloaded, NotifyEventsQuery{
		Since:  500,
		Limit:  1,
		Events: map[string]bool{"pr_merged": true, "needs_me": true},
	})
	// Seq 501 is index 500 -> kinds[0] = pr_merged; seq 504 is index 503 -> kinds[3] = needs_me.
	if len(respFiltered.Events) != 1 || respFiltered.Events[0].Seq != 501 || respFiltered.Events[0].Event != "pr_merged" || !respFiltered.HasMore || respFiltered.NextSince != 501 {
		t.Fatalf("filtered page 1: %+v", respFiltered)
	}
	respFiltered2 := queryNotifyFeed(reloaded, NotifyEventsQuery{
		Since:  respFiltered.NextSince,
		Limit:  5,
		Events: map[string]bool{"pr_merged": true, "needs_me": true},
	})
	if len(respFiltered2.Events) != 1 || respFiltered2.Events[0].Seq != 504 || respFiltered2.Events[0].Event != "needs_me" || respFiltered2.HasMore || respFiltered2.NextSince != 504 {
		t.Fatalf("filtered page 2: %+v", respFiltered2)
	}
	respFiltered3 := queryNotifyFeed(reloaded, NotifyEventsQuery{
		Since:  respFiltered2.NextSince,
		Limit:  5,
		Events: map[string]bool{"pr_merged": true, "needs_me": true},
	})
	// No matching events after 504 -> next_since advances to latest_seq (505).
	if len(respFiltered3.Events) != 0 || respFiltered3.HasMore || respFiltered3.NextSince != 505 {
		t.Fatalf("filtered page 3: %+v", respFiltered3)
	}
}

func TestNotifyFeedClassExtractionAndSanitization(t *testing.T) {
	cfg, _, _ := setupNotifyTestEnv(t)
	cfg.Desktop.Enabled = boolPtr(false)
	cfg.Tap = TapConfig{Enabled: true, Port: 8642, Devices: []string{"phone"}}
	// No outbound sinks configured!
	now := time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC)

	env := liveNotifyEnv(cfg, nil)
	env.now = func() time.Time { return now }
	env.inboxURL = func(Config) (string, error) {
		return "http://100.64.2.9:8642/inbox", nil
	}

	// Write a structured question card for task-card with class 8 (overriding unclassed note)
	// and a stale answered card for task-stale-card that must NOT bleed its old class.
	cardDir := tapBeadCardDir("task-card")
	if err := os.MkdirAll(cardDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardDir, "question.json"), []byte(`{
		"version": 1,
		"bead": "task-card",
		"question": "Approve production migration?",
		"class": 8,
		"class_reason": "stateful migration"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	staleDir := tapBeadCardDir("task-stale-card")
	if err := os.MkdirAll(staleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "question.json"), []byte(`{
		"version": 1,
		"bead": "task-stale-card",
		"hash": "deadbeef",
		"question": "Old critical question",
		"class": 9
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	threads := []Thread{
		{
			Bead: Bead{
				ID:     "task-opts-c7",
				Title:  "Rollout decision",
				Status: StatusNeedsMe,
				Notes: strings.Join([]string{
					"QUESTION: Ship with Authorization: Bearer secret_token_abc999 sk_live_1234567890abcdef and http://100.64.2.9:8642/tap/YWJjZGVmZ2hpamtsbW5vcA.0123456789abcdef0123456789abcdef0123456789ab?",
					"OPTIONS:",
					"1. Canary first",
					"2. All regions",
					"CLASS: 7 — prod rollout",
				}, "\n"),
			},
		},
		{
			Bead: Bead{
				ID:     "task-c0",
				Title:  "Read-only check",
				Status: StatusNeedsMe,
				Notes:  "QUESTION: Confirm log inspection scope?\nCLASS: 0 — read-only",
			},
		},
		{
			Bead: Bead{
				ID:     "task-run-c8",
				Title:  "Run admin command",
				Status: StatusNeedsMe,
				Notes:  "RUN: aws sso login --profile admin\nNeeds interactive login\nCLASS: 8 — credential refresh",
			},
		},
		{
			Bead: Bead{
				ID:     "task-card",
				Title:  "Structured card bead",
				Status: StatusNeedsMe,
				Notes:  "Waiting for migration review",
			},
		},
		{
			Bead: Bead{
				ID:     "task-stale-card",
				Title:  "New unclassed question after answered class 9",
				Status: StatusNeedsMe,
				Notes: strings.Join([]string{
					"QUESTION: Old critical question",
					"CLASS: 9 — old",
					"NEEDS_ME ANSWERED: 1 Yes [deadbeef] by TJ",
					"Follow-up question with no class",
				}, "\n"),
			},
		},
		{
			Bead: Bead{
				ID:     "task-blk",
				Title:  "Blocked bead",
				Status: StatusBlocked,
			},
		},
	}

	if err := reconcileNotifyWith(context.Background(), cfg, env, threads, nil, now, nil); err != nil {
		t.Fatalf("reconcileNotifyWith: %v", err)
	}

	rawFeed, err := os.ReadFile(notifyEventsPath())
	if err != nil {
		t.Fatalf("read events.json: %v", err)
	}
	rawStr := string(rawFeed)
	for _, secret := range []string{
		"secret_token_abc999",
		"sk_live_1234567890abcdef",
		"/tap/",
		"YWJjZGVmZ2hpamtsbW5vcA",
	} {
		if strings.Contains(rawStr, secret) {
			t.Fatalf("events.json leaked secret %q:\n%s", secret, rawStr)
		}
	}

	feed, err := loadNotifyFeed()
	if err != nil {
		t.Fatalf("loadNotifyFeed: %v", err)
	}
	if len(feed.Events) != 6 {
		t.Fatalf("expected 6 feed events, got %d: %+v", len(feed.Events), feed.Events)
	}

	byBead := map[string]NotifyFeedEvent{}
	for _, ev := range feed.Events {
		if ev.InboxURL != "http://100.64.2.9:8642/inbox" {
			t.Errorf("seq %d inbox_url = %q, want non-secret /inbox URL", ev.Seq, ev.InboxURL)
		}
		if len(ev.Items) != 1 {
			t.Fatalf("seq %d items = %+v, want 1", ev.Seq, ev.Items)
		}
		byBead[ev.Items[0].Bead] = ev
	}

	checkClass := func(bead string, want *int) {
		t.Helper()
		ev, ok := byBead[bead]
		if !ok {
			t.Fatalf("missing feed event for %s", bead)
		}
		got := ev.Items[0].Class
		if want == nil {
			if got != nil {
				t.Errorf("%s: class = %d, want nil", bead, *got)
			}
			// Also confirm "class" key is omitted from JSON when nil.
			b, _ := json.Marshal(ev.Items[0])
			if strings.Contains(string(b), `"class"`) {
				t.Errorf("%s: nil class should be omitted from JSON: %s", bead, string(b))
			}
			return
		}
		if got == nil || *got != *want {
			t.Errorf("%s: class = %v, want %d", bead, got, *want)
			return
		}
		b, _ := json.Marshal(ev.Items[0])
		if !strings.Contains(string(b), fmt.Sprintf(`"class":%d`, *want)) {
			t.Errorf("%s: expected serialized class %d in %s", bead, *want, string(b))
		}
	}

	checkClass("task-opts-c7", intPtr(7))
	checkClass("task-c0", intPtr(0))
	checkClass("task-run-c8", intPtr(8))
	checkClass("task-card", intPtr(8))
	checkClass("task-stale-card", nil)
	checkClass("task-blk", nil)
}

func TestNotifyFeedRecordsTickerAndInboxRunnerWithoutDesktopOrSinks(t *testing.T) {
	// 1. Ticker milestones + blockers recorded with [desktop] enabled = false and zero [notify] sinks.
	dir := fakeTools(t)
	t.Setenv("PATH", dir)
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	beads := []Bead{
		{ID: "task-nm", Title: "Needs review", Status: StatusNeedsMe, Notes: "QUESTION: Proceed?\nOPTIONS:\n1. Yes\n2. No\nCLASS: 7 — prod"},
		{ID: "task-wb", Title: "Worker blocked", Status: StatusInProgress},
		{ID: "task-pr", Title: "PR merged", Status: StatusInProgress},
	}
	bJSON, _ := json.Marshal(beads)
	t.Setenv("FIXTURE_BEADS", string(bJSON))
	write("bd", "printf '%s\\n' \"$FIXTURE_BEADS\"\n")
	write("herdr", `printf '{"result":{"agents":[{"name":"task-wb","agent_status":"blocked","pane_id":"w1:p1"}]}}\n'`)

	cfg := defaultConfig()
	cfg.Desktop.Enabled = boolPtr(false)
	// No webhooks, no commands configured.
	if cfg.Notify.enabled() || cfg.Notify.hasSinks() {
		t.Fatalf("expected no sinks configured")
	}

	now := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	ticker := newTicker(cfg, log.New(&logs, "", 0))
	ticker.now = func() time.Time { return now }

	// Seed previous state so task-pr transitions OPEN -> MERGED on this tick.
	openPR := &PR{Number: 77, Title: "Add feed", URL: "https://github.com/acme/app/pull/77", State: "OPEN", HeadSHA: "abc"}
	mergedPR := *openPR
	mergedPR.State = "MERGED"
	st := loadState()
	st.LastGH = now // skip gh CLI calls
	st.PRs["task-pr"] = mergedPR
	st.MergedPRs = map[string][]PR{"task-pr": {mergedPR}}
	st.Threads["task-pr"] = snapshot(thread(beads[2], nil, openPR), Snapshot{}, now.Add(-time.Minute))

	if err := ticker.once(&st); err != nil {
		t.Fatalf("ticker.once: %v (logs: %s)", err, logs.String())
	}
	if _, err := os.Stat(desktopPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("desktop journal should not be created when [desktop] enabled = false")
	}

	feed, err := loadNotifyFeed()
	if err != nil {
		t.Fatalf("loadNotifyFeed after ticker.once: %v", err)
	}
	if len(feed.Events) != 3 {
		t.Fatalf("expected 3 feed events (needs_me, worker_blocked, pr_merged), got %d: %+v", len(feed.Events), feed.Events)
	}
	gotKinds := []string{feed.Events[0].Event, feed.Events[1].Event, feed.Events[2].Event}
	if !slices.Equal(gotKinds, []string{"needs_me", "worker_blocked", "pr_merged"}) {
		t.Fatalf("feed event order/kinds = %v, want [needs_me worker_blocked pr_merged]", gotKinds)
	}
	if feed.Events[0].Items[0].Class == nil || *feed.Events[0].Items[0].Class != 7 {
		t.Fatalf("expected class=7 on needs_me feed event, got %+v", feed.Events[0].Items)
	}

	// Second identical tick must NOT duplicate any feed events.
	ticker.now = func() time.Time { return now.Add(15 * time.Second) }
	st.LastGH = now.Add(15 * time.Second)
	if err := ticker.once(&st); err != nil {
		t.Fatalf("second ticker.once: %v", err)
	}
	feed2, err := loadNotifyFeed()
	if err != nil {
		t.Fatalf("loadNotifyFeed after second tick: %v", err)
	}
	if feed2.LatestSeq != 3 || len(feed2.Events) != 3 {
		t.Fatalf("duplicate tick appended feed events: latest=%d len=%d", feed2.LatestSeq, len(feed2.Events))
	}

	// 2. Headless Inbox Runner outcomes recorded into the SAME feed even when [desktop] and sinks are off.
	f1 := writeTestInboxEvent(t, Event{Bead: "task-ir1", Kind: EventMerged, Summary: "PR merged", At: now.Add(-5 * time.Minute)})
	curTime := now.Add(time.Minute)
	irEnv := inboxRunnerEnv{
		now: func() time.Time { return curTime },
		listAgents: func() (map[string]Agent, bool, error) {
			return map[string]Agent{}, true, nil
		},
		readInbox:     readInbox,
		headlessOwned: func(string) bool { return false },
		coordDir:      func() string { return filepath.Join(t.TempDir(), "coord") },
		notify: func(_, _ string) desktopResult {
			t.Fatal("desktop notify must not run when [desktop] enabled = false")
			return desktopResult{}
		},
		execCmd: func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
			curTime = curTime.Add(300 * time.Millisecond)
			_, _ = inboxDoneFiles([]string{f1})
			return inboxRunnerExecResult{ExitCode: 0, Output: "handled"}
		},
	}
	if _, err := runInboxRunner(context.Background(), cfg, irEnv, inboxRunnerOpts{Manual: true}); err != nil {
		t.Fatalf("runInboxRunner ok: %v", err)
	}

	// No-op (acked == 0, ok) must NOT append to feed.
	_ = writeTestInboxEvent(t, Event{Bead: "task-ir2", Kind: EventUnblocked, Summary: "unblocked", At: now.Add(-5 * time.Minute)})
	irEnv.execCmd = func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
		curTime = curTime.Add(50 * time.Millisecond)
		return inboxRunnerExecResult{ExitCode: 0, Output: "noop"}
	}
	if _, err := runInboxRunner(context.Background(), cfg, irEnv, inboxRunnerOpts{Manual: true}); err != nil {
		t.Fatalf("runInboxRunner noop: %v", err)
	}

	// Failure MUST append.
	irEnv.execCmd = func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
		curTime = curTime.Add(200 * time.Millisecond)
		return inboxRunnerExecResult{ExitCode: 1, Err: errors.New("exit status 1")}
	}
	if _, err := runInboxRunner(context.Background(), cfg, irEnv, inboxRunnerOpts{Manual: true}); err == nil {
		t.Fatal("expected error from failing inbox runner")
	}

	// Timeout MUST append.
	_ = writeTestInboxEvent(t, Event{Bead: "task-ir3", Kind: EventFailing, Summary: "ci failed", At: now.Add(-5 * time.Minute)})
	irEnv.execCmd = func(_ context.Context, _ Config, _ string, _ []string, _ string) inboxRunnerExecResult {
		curTime = curTime.Add(time.Second)
		return inboxRunnerExecResult{ExitCode: -1, TimedOut: true, Err: errors.New("timed out after 300s")}
	}
	if _, err := runInboxRunner(context.Background(), cfg, irEnv, inboxRunnerOpts{Manual: true}); err == nil {
		t.Fatal("expected timeout error from inbox runner")
	}

	feed3, err := loadNotifyFeed()
	if err != nil {
		t.Fatalf("loadNotifyFeed after inbox runner: %v", err)
	}
	if feed3.LatestSeq != 6 || len(feed3.Events) != 6 {
		t.Fatalf("expected 6 total feed events (3 ticker + 3 inbox_runner), got latest=%d len=%d", feed3.LatestSeq, len(feed3.Events))
	}
	for i, wantStatus := range []string{"ok", "error", "timeout"} {
		ev := feed3.Events[3+i]
		if ev.Seq != uint64(4+i) || ev.Event != "inbox_runner" || ev.Runner == nil || ev.Runner.ExitStatus != wantStatus {
			t.Fatalf("runner event[%d] = %+v, want seq=%d exit_status=%q", i, ev, 4+i, wantStatus)
		}
	}

	// 3. `quartermaster notify test` must NOT pollute the durable feed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg.Notify = NotifyConfig{
		Webhooks: []NotifyWebhookConfig{{Name: "test-sink", URL: srv.URL}},
	}
	var out bytes.Buffer
	if err := notifyCmd(cfg, liveNotifyEnv(cfg, nil), []string{"test"}, &out); err != nil {
		t.Fatalf("notify test: %v", err)
	}
	feed4, err := loadNotifyFeed()
	if err != nil {
		t.Fatalf("loadNotifyFeed after notify test: %v", err)
	}
	if feed4.LatestSeq != 6 || len(feed4.Events) != 6 {
		t.Fatalf("notify test polluted events feed: latest=%d len=%d", feed4.LatestSeq, len(feed4.Events))
	}
}

func TestTapEventsHTTPEndpointAuthValidationReplayAndConcurrency(t *testing.T) {
	testTapOwnIP(t)
	f := &fakePane{screens: []string{screen(t, "claude-idle.txt")}}

	t.Run("fail-closed auth 403 and TAP REFUSED journaled before reading state", func(t *testing.T) {
		s := testTapServer(t, f, "desktop")
		// Write corrupt events.json to prove auth fails BEFORE reading any state.
		if err := rpcPrivateDir(notifyDir()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(notifyEventsPath(), []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}

		// 1. Disallowed tailnet device (including host machine itself).
		rec := tapEventsRequest(s, http.MethodGet, "/events?since=bad", "100.78.70.17:49152")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("disallowed device status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
		errMsg := decodeTapLinksError(t, rec)
		if !strings.Contains(errMsg, "device desktop is not allowed") {
			t.Fatalf("error = %q", errMsg)
		}

		// 2. Non-tailnet IP must be refused without calling whois.
		whoisCalled := false
		s.whois = func(string) (tapIdentity, error) {
			whoisCalled = true
			return tapIdentity{Device: "iphone181"}, nil
		}
		for _, addr := range []string{"127.0.0.1:49152", "192.168.1.20:49152", "invalid"} {
			rec2 := tapEventsRequest(s, http.MethodGet, "/events", addr)
			if rec2.Code != http.StatusForbidden {
				t.Errorf("addr %s status = %d, want 403", addr, rec2.Code)
			}
			_ = decodeTapLinksError(t, rec2)
		}
		if whoisCalled {
			t.Fatal("whois was called for non-tailnet remote addresses")
		}

		// 3. Failed whois fails closed with 403.
		s.whois = func(string) (tapIdentity, error) {
			return tapIdentity{}, errors.New("tailscaled unreachable")
		}
		rec3 := tapEventsRequest(s, http.MethodGet, "/events", tapPhoneAddr)
		if rec3.Code != http.StatusForbidden {
			t.Fatalf("whois error status = %d, want 403", rec3.Code)
		}
		_ = decodeTapLinksError(t, rec3)

		// All refusals journaled as TAP REFUSED:.
		recs, err := readJournal(time.Time{})
		if err != nil {
			t.Fatalf("readJournal: %v", err)
		}
		if len(recs) != 5 {
			t.Fatalf("expected 5 TAP REFUSED journal entries, got %d: %+v", len(recs), recs)
		}
		for _, r := range recs {
			if r.Kind != journalTap || !strings.HasPrefix(r.Text, "TAP REFUSED:") {
				t.Errorf("unexpected journal entry: %+v", r)
			}
		}
	})

	t.Run("GET read-only empty state, 405 on POST, 400 on bad params, cursor replay", func(t *testing.T) {
		s := testTapServer(t, f, "iphone181")

		// Strictly read-only on empty state dir (creates no notify dir or journal).
		recEmpty := tapEventsRequest(s, http.MethodGet, "/events", tapPhoneAddr)
		if recEmpty.Code != http.StatusOK {
			t.Fatalf("GET /events empty = %d %s", recEmpty.Code, recEmpty.Body.String())
		}
		if ct := recEmpty.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if cc := recEmpty.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("Cache-Control = %q", cc)
		}
		if _, err := os.Stat(notifyDir()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("GET /events mutated state dir: stat err = %v", err)
		}
		var emptyResp NotifyEventsResponse
		if err := json.Unmarshal(recEmpty.Body.Bytes(), &emptyResp); err != nil {
			t.Fatalf("unmarshal empty response: %v", err)
		}
		if emptyResp.Version != 1 || emptyResp.LatestSeq != 0 || emptyResp.NextSince != 0 || emptyResp.HasMore || emptyResp.Truncated || len(emptyResp.Events) != 0 {
			t.Fatalf("unexpected empty response: %+v", emptyResp)
		}
		// Ensure "events": [] (not null) in JSON.
		if !strings.Contains(recEmpty.Body.String(), `"events": []`) {
			t.Fatalf("expected non-null empty events array in JSON:\n%s", recEmpty.Body.String())
		}

		// 405 on POST / PUT / DELETE with Allow: GET.
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			recM := tapEventsRequest(s, method, "/events", tapPhoneAddr)
			if recM.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s /events status = %d, want 405", method, recM.Code)
			}
			if allow := recM.Header().Get("Allow"); allow != http.MethodGet {
				t.Errorf("%s Allow = %q, want GET", method, allow)
			}
			_ = decodeTapLinksError(t, recM)
		}

		// 400 JSON errors on malformed query params.
		badQueries := []string{
			"/events?since=-1",
			"/events?since=abc",
			"/events?since=",
			"/events?since=1&since=2",
			"/events?limit=0",
			"/events?limit=-5",
			"/events?limit=abc",
			"/events?limit=",
			"/events?limit=1&limit=2",
			"/events?event=",
			"/events?event=unknown_kind",
			"/events?event=test",
			"/events?event=needs_me,",
			"/events?event=,pr_merged",
			"/events?since=%zz",
		}
		for _, target := range badQueries {
			recBad := tapEventsRequest(s, http.MethodGet, target, tapPhoneAddr)
			if recBad.Code != http.StatusBadRequest {
				t.Errorf("GET %s status = %d, want 400 (%s)", target, recBad.Code, recBad.Body.String())
			}
			_ = decodeTapLinksError(t, recBad)
		}

		// Populate 4 events and replay via HTTP cursor + event filter + limit.
		c7 := 7
		if err := appendNotifyFeedEvents([]NotifyEnvelope{
			{
				Version:  1,
				Event:    "needs_me",
				At:       "2026-11-25T04:00:00Z",
				Title:    "Quartermaster: needs_me",
				Body:     "task-1: Approve prod deploy?",
				InboxURL: "http://100.78.70.17:8642/inbox",
				Items:    []NotifyItem{{Bead: "task-1", Reason: "needs_me", Summary: "Approve prod deploy?", Class: &c7}},
			},
			{
				Version: 1,
				Event:   "pr_merged",
				At:      "2026-11-25T04:01:00Z",
				Title:   "Quartermaster: PR merged",
				Body:    "task-2: acme/app#2 merged",
				Items:   []NotifyItem{{Bead: "task-2", Reason: "pr_merged", Summary: "task-2: acme/app#2 merged", PR: "acme/app#2"}},
			},
			{
				Version: 1,
				Event:   "inbox_runner",
				At:      "2026-11-25T04:02:00Z",
				Title:   "Quartermaster: headless inbox runner",
				Body:    "Handled 1/1 inbox item (merged).",
				Runner:  &NotifyRunnerInfo{ExitStatus: "ok", Acked: 1, Eligible: 1, Kinds: []string{"merged"}, DurationMs: 420},
			},
			{
				Version: 1,
				Event:   "pr_ready",
				At:      "2026-11-25T04:03:00Z",
				Title:   "Quartermaster: PR ready to merge",
				Body:    "task-3: acme/app#3 ready to merge (approved)",
				Items:   []NotifyItem{{Bead: "task-3", Reason: "pr_ready", Summary: "task-3: acme/app#3 ready to merge (approved)", PR: "acme/app#3"}},
			},
		}); err != nil {
			t.Fatalf("appendNotifyFeedEvents: %v", err)
		}

		// Page 1: since=0&limit=2
		rec1 := tapEventsRequest(s, http.MethodGet, "/events?since=0&limit=2", tapPhoneAddr)
		if rec1.Code != http.StatusOK {
			t.Fatalf("page 1 status = %d %s", rec1.Code, rec1.Body.String())
		}
		var p1 NotifyEventsResponse
		if err := json.Unmarshal(rec1.Body.Bytes(), &p1); err != nil {
			t.Fatal(err)
		}
		if p1.LatestSeq != 4 || p1.NextSince != 2 || !p1.HasMore || p1.Truncated || len(p1.Events) != 2 {
			t.Fatalf("page 1 = %+v", p1)
		}
		if p1.Events[0].Seq != 1 || p1.Events[0].Items[0].Class == nil || *p1.Events[0].Items[0].Class != 7 {
			t.Fatalf("page 1 event 0 = %+v", p1.Events[0])
		}

		// Page 2: since=2&limit=2
		rec2 := tapEventsRequest(s, http.MethodGet, "/events?since=2&limit=2", tapPhoneAddr)
		if rec2.Code != http.StatusOK {
			t.Fatalf("page 2 status = %d %s", rec2.Code, rec2.Body.String())
		}
		var p2 NotifyEventsResponse
		if err := json.Unmarshal(rec2.Body.Bytes(), &p2); err != nil {
			t.Fatal(err)
		}
		if p2.LatestSeq != 4 || p2.NextSince != 4 || p2.HasMore || p2.Truncated || len(p2.Events) != 2 || p2.Events[0].Event != "inbox_runner" || p2.Events[1].Event != "pr_ready" {
			t.Fatalf("page 2 = %+v", p2)
		}

		// Event filter: event=needs_me,inbox_runner&limit=999 (capped at 500, no error).
		recF := tapEventsRequest(s, http.MethodGet, "/events?event=needs_me,inbox_runner&limit=999", tapPhoneAddr)
		if recF.Code != http.StatusOK {
			t.Fatalf("filtered status = %d %s", recF.Code, recF.Body.String())
		}
		var pF NotifyEventsResponse
		if err := json.Unmarshal(recF.Body.Bytes(), &pF); err != nil {
			t.Fatal(err)
		}
		if len(pF.Events) != 2 || pF.Events[0].Seq != 1 || pF.Events[1].Seq != 3 || pF.NextSince != 3 || pF.HasMore {
			t.Fatalf("filtered response = %+v", pF)
		}

		// Cursor ahead of latest_seq -> truncated=true, next_since=4.
		recAhead := tapEventsRequest(s, http.MethodGet, "/events?since=99", tapPhoneAddr)
		if recAhead.Code != http.StatusOK {
			t.Fatalf("ahead status = %d %s", recAhead.Code, recAhead.Body.String())
		}
		var pAhead NotifyEventsResponse
		if err := json.Unmarshal(recAhead.Body.Bytes(), &pAhead); err != nil {
			t.Fatal(err)
		}
		if !pAhead.Truncated || pAhead.NextSince != 4 || pAhead.LatestSeq != 4 || len(pAhead.Events) != 0 {
			t.Fatalf("ahead response = %+v", pAhead)
		}

		// Concurrent writers + HTTP readers under -race.
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					_ = appendNotifyFeedEvents([]NotifyEnvelope{{
						Version: 1,
						Event:   "pr_green",
						At:      "2026-11-25T04:10:00Z",
						Title:   fmt.Sprintf("Worker %d event %d", worker, i),
						Body:    "concurrent test",
					}})
				}
			}(w)
		}
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					rec := tapEventsRequest(s, http.MethodGet, "/events?since=0&limit=50", tapPhoneAddr)
					if rec.Code != http.StatusOK {
						t.Errorf("concurrent GET /events = %d %s", rec.Code, rec.Body.String())
						return
					}
					var got NotifyEventsResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
						t.Errorf("concurrent JSON decode error: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()

		finalFeed, err := loadNotifyFeed()
		if err != nil {
			t.Fatalf("final loadNotifyFeed: %v", err)
		}
		if finalFeed.LatestSeq != 24 || len(finalFeed.Events) != 24 {
			t.Fatalf("expected 24 total events after concurrent writes, got latest=%d len=%d", finalFeed.LatestSeq, len(finalFeed.Events))
		}
	})
}

func TestNotifyCLIEventsCommand(t *testing.T) {
	cfg, _, _ := setupNotifyTestEnv(t)
	env := liveNotifyEnv(cfg, nil)

	// Empty feed: both text and --json succeed and stay read-only.
	var out bytes.Buffer
	if err := notifyCmd(cfg, env, []string{"events", "--json"}, &out); err != nil {
		t.Fatalf("notify events --json empty: %v", err)
	}
	var emptyResp NotifyEventsResponse
	if err := json.Unmarshal(out.Bytes(), &emptyResp); err != nil {
		t.Fatalf("unmarshal empty CLI JSON: %v", err)
	}
	if emptyResp.Version != 1 || emptyResp.LatestSeq != 0 || emptyResp.NextSince != 0 || len(emptyResp.Events) != 0 {
		t.Fatalf("unexpected empty CLI JSON: %+v", emptyResp)
	}
	if _, err := os.Stat(notifyDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("notify events --json should not create notify dir, stat err = %v", err)
	}

	c8 := 8
	if err := appendNotifyFeedEvents([]NotifyEnvelope{
		{
			Version: 1,
			Event:   "needs_me",
			At:      "2026-11-25T05:00:00Z",
			Title:   "Quartermaster: needs_me",
			Body:    "task-cli-1: Rotate token?",
			Items:   []NotifyItem{{Bead: "task-cli-1", Reason: "needs_me", Summary: "Rotate token?", Class: &c8}},
		},
		{
			Version: 1,
			Event:   "pr_merged",
			At:      "2026-11-25T05:01:00Z",
			Title:   "Quartermaster: PR merged",
			Body:    "task-cli-2: acme/app#12 merged",
			Items:   []NotifyItem{{Bead: "task-cli-2", Reason: "pr_merged", Summary: "task-cli-2: acme/app#12 merged", PR: "acme/app#12"}},
		},
	}); err != nil {
		t.Fatalf("appendNotifyFeedEvents: %v", err)
	}

	// Text output shows seq, event, class tag, and summary.
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"events"}, &out); err != nil {
		t.Fatalf("notify events text: %v", err)
	}
	text := out.String()
	for _, want := range []string{"latest_seq=2", "#1", "needs_me", "[C8]", "task-cli-1", "#2", "pr_merged", "task-cli-2"} {
		if !strings.Contains(text, want) {
			t.Errorf("text output missing %q:\n%s", want, text)
		}
	}

	// JSON output with --since, --limit, and --event.
	out.Reset()
	if err := notifyCmd(cfg, env, []string{"events", "--since", "1", "--limit", "10", "--event", "pr_merged", "--json"}, &out); err != nil {
		t.Fatalf("notify events filtered --json: %v", err)
	}
	var got NotifyEventsResponse
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal filtered CLI JSON: %v", err)
	}
	if got.LatestSeq != 2 || got.NextSince != 2 || got.HasMore || got.Truncated || len(got.Events) != 1 || got.Events[0].Seq != 2 {
		t.Fatalf("unexpected filtered CLI JSON: %+v", got)
	}

	// Invalid CLI flags fail cleanly.
	for _, args := range [][]string{
		{"events", "--limit", "0"},
		{"events", "--limit", "-3"},
		{"events", "--since", "-1"},
		{"events", "--event", "bogus"},
		{"events", "--event", "test"},
		{"events", "extra-arg"},
	} {
		out.Reset()
		if err := notifyCmd(cfg, env, args, &out); err == nil {
			t.Errorf("notify %v unexpectedly succeeded", args)
		}
	}
}
