package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseContextSections(t *testing.T) {
	all, err := parseContextSections(nil, false)
	if err != nil {
		t.Fatalf("default sections: %v", err)
	}
	for _, sec := range contextSectionOrder {
		if !all.has(sec) {
			t.Errorf("default sections missing %s", sec)
		}
	}

	got, err := parseContextSections([]string{"inbox, threads", "ready", "inbox"}, true)
	if err != nil {
		t.Fatalf("valid sections: %v", err)
	}
	for _, sec := range contextSectionOrder {
		want := sec == ctxInbox || sec == ctxThreads || sec == ctxReady
		if got.has(sec) != want {
			t.Errorf("section %s = %v, want %v", sec, got.has(sec), want)
		}
	}

	if _, err := parseContextSections([]string{"inbox,bogus"}, true); err == nil {
		t.Fatal("unknown section succeeded, want error")
	} else {
		for _, want := range []string{"bogus", "summary,autonomy,inbox,threads,linear,next,ready"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err.Error(), want)
			}
		}
	}

	if _, err := parseContextSections([]string{" , "}, true); err == nil {
		t.Fatal("empty --section succeeded, want error")
	} else if !strings.Contains(err.Error(), "summary,autonomy,inbox,threads,linear,next,ready") {
		t.Errorf("empty error %q lacks valid list", err.Error())
	}
}

func TestContextCLISectionFlagValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
	t.Setenv("QUARTERMASTER_STATE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("repo = \"/tmp/r\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"context", "--section", "bogus"})
	if err == nil {
		t.Fatal("run(context --section bogus) succeeded, want error")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "summary,autonomy,inbox,threads,linear,next,ready") {
		t.Errorf("unexpected error: %v", err)
	}
}

func contextFixture(t *testing.T) (Config, []Thread, []Bead, []Bead, []InboxItem, TickerState, map[string]bool) {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := Config{Repo: "/r", Autonomy: AutonomyConfig{Level: 4, Max: 7}}
	if err := saveAutonomy(autonomyState{
		Leases: map[string]autonomyLease{"app-ask": {Level: 8, Until: t0.Add(2 * time.Hour)}},
	}); err != nil {
		t.Fatal(err)
	}

	askBead := Bead{
		ID:     "app-ask",
		Title:  "Pick rollout strategy for regional deployment",
		Status: StatusNeedsMe,
		Notes:  "QUESTION: Roll out one region first?\nOPTIONS:\n1. One region\n2. All regions\nCLASS: 7",
	}
	revBead := Bead{ID: "app-rev", Title: "Raise client timeout", Status: StatusInProgress}
	revPR := &PR{Number: 42, State: "OPEN", Title: "ENG-101: Raise client timeout", URL: "https://github.com/acme/app/pull/42"}
	revThread := thread(revBead, agent("idle", 1), revPR)
	revThread.Linear = "ENG-101"

	noAgentBead := Bead{ID: "app-wt", Title: "Resumable task", Status: StatusInProgress, UpdatedAt: t0.Add(-time.Hour)}
	threads := []Thread{
		thread(askBead, agent("idle", 1), nil),
		revThread,
		{Bead: noAgentBead},
	}
	next := []Bead{{ID: "app-next", Priority: 1, Type: "task", Status: StatusOpen, Title: "Next priority task"}}
	ready := []Bead{
		next[0],
		{ID: "app-r1", Priority: 2, Type: "task", Status: StatusOpen, Title: "Ready backlog task"},
	}
	inbox := []InboxItem{
		{
			Event: Event{Bead: "app-wt", Kind: EventAgentGone, Summary: "worker exited", At: t0.Add(-5 * time.Minute)},
			File:  "100-app-wt-agent_gone.json",
		},
	}
	return cfg, threads, next, ready, inbox, TickerState{}, map[string]bool{"app-wt": true}
}

func presentTextSections(out string) []string {
	var secs []string
	lines := strings.Split(out, "\n")
	if len(lines) > 0 && (strings.HasPrefix(lines[0], "# quartermaster context") || (lines[0] != "" && !strings.HasPrefix(lines[0], "## "))) {
		secs = append(secs, "summary")
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "## Autonomy"):
			secs = append(secs, "autonomy")
		case strings.HasPrefix(l, "## Inbox"):
			secs = append(secs, "inbox")
		case strings.HasPrefix(l, "## Threads"):
			secs = append(secs, "threads")
		case strings.HasPrefix(l, "## Linear"):
			secs = append(secs, "linear")
		case strings.HasPrefix(l, "## Selected next"):
			secs = append(secs, "next")
		case strings.HasPrefix(l, "## Ready"):
			secs = append(secs, "ready")
		}
	}
	return secs
}

func jsonTopKeys(t *testing.T, raw string) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	var keys []string
	for _, sec := range contextSectionOrder {
		if _, ok := m[string(sec)]; ok {
			keys = append(keys, string(sec))
		}
	}
	if len(keys) != len(m) {
		t.Fatalf("unexpected extra JSON keys: got %v", m)
	}
	return keys
}

func TestContextSectionSelectionTextAndJSONParity(t *testing.T) {
	cfg, threads, next, ready, inbox, st, wt := contextFixture(t)

	cases := []struct {
		name     string
		specs    []string
		explicit bool
		compact  bool
		wantSecs []string
	}{
		{"all default", nil, false, false, []string{"summary", "autonomy", "inbox", "threads", "linear", "next", "ready"}},
		{"inbox and threads", []string{"inbox,threads"}, true, false, []string{"inbox", "threads"}},
		{"inbox and threads compact", []string{"inbox,threads"}, true, true, []string{"inbox", "threads"}},
		{"summary only", []string{"summary"}, true, false, []string{"summary"}},
		{"autonomy only", []string{"autonomy"}, true, false, []string{"autonomy"}},
		{"next and ready", []string{"next", "ready"}, true, false, []string{"next", "ready"}},
		{"compact all", nil, false, true, []string{"summary", "autonomy", "inbox", "threads", "linear", "next", "ready"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secs, err := parseContextSections(tc.specs, tc.explicit)
			if err != nil {
				t.Fatal(err)
			}
			txt, err := renderContextWith(cfg, threads, next, ready, inbox, st, wt, t0, ContextOptions{
				Compact:  tc.compact,
				Sections: secs,
			})
			if err != nil {
				t.Fatal(err)
			}
			js, err := renderContextWith(cfg, threads, next, ready, inbox, st, wt, t0, ContextOptions{
				Compact:  tc.compact,
				Sections: secs,
				JSON:     true,
			})
			if err != nil {
				t.Fatal(err)
			}
			gotText := presentTextSections(txt)
			gotJSON := jsonTopKeys(t, js)
			if !slices.Equal(gotText, tc.wantSecs) {
				t.Errorf("text sections = %v, want %v\n%s", gotText, tc.wantSecs, txt)
			}
			if !slices.Equal(gotJSON, tc.wantSecs) {
				t.Errorf("json sections = %v, want %v\n%s", gotJSON, tc.wantSecs, js)
			}
		})
	}

	// Empty linear: omitted by default in both text and JSON, included in both
	// when explicitly requested.
	noLinearThreads := []Thread{threads[0]}
	for _, explicit := range []bool{false, true} {
		var specs []string
		want := []string{"summary", "autonomy", "inbox", "threads", "next", "ready"}
		if explicit {
			specs = []string{"linear"}
			want = []string{"linear"}
		}
		secs, err := parseContextSections(specs, explicit)
		if err != nil {
			t.Fatal(err)
		}
		txt, err := renderContextWith(cfg, noLinearThreads, next, ready, inbox, st, wt, t0, ContextOptions{Sections: secs})
		if err != nil {
			t.Fatal(err)
		}
		js, err := renderContextWith(cfg, noLinearThreads, next, ready, inbox, st, wt, t0, ContextOptions{Sections: secs, JSON: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := presentTextSections(txt); !slices.Equal(got, want) {
			t.Errorf("explicit=%v text sections = %v, want %v", explicit, got, want)
		}
		if got := jsonTopKeys(t, js); !slices.Equal(got, want) {
			t.Errorf("explicit=%v json sections = %v, want %v", explicit, got, want)
		}
	}
}

func TestContextJSONSpecShape(t *testing.T) {
	cfg, threads, next, ready, inbox, st, wt := contextFixture(t)
	secs, err := parseContextSections([]string{"summary,inbox,threads"}, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := renderContextWith(cfg, threads, next, ready, inbox, st, wt, t0, ContextOptions{
		Compact:  true,
		Sections: secs,
		JSON:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Summary map[string]int `json:"summary"`
		Inbox   []struct {
			Bead    string    `json:"bead"`
			Kind    EventKind `json:"kind"`
			Summary string    `json:"summary"`
			At      time.Time `json:"at"`
			File    string    `json:"file"`
		} `json:"inbox"`
		Threads []map[string]any `json:"threads"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if parsed.Summary["needs_you"] != 1 || parsed.Summary["review"] != 1 || parsed.Summary["no_agent"] != 1 {
		t.Errorf("summary = %v", parsed.Summary)
	}
	if len(parsed.Inbox) != 1 || parsed.Inbox[0].Bead != "app-wt" || parsed.Inbox[0].Kind != EventAgentGone || parsed.Inbox[0].At.IsZero() {
		t.Errorf("inbox = %+v", parsed.Inbox)
	}
	if parsed.Inbox[0].Summary != "" || parsed.Inbox[0].File != "" {
		t.Errorf("compact inbox should omit summary and file: %+v", parsed.Inbox[0])
	}
	if len(parsed.Threads) != 3 {
		t.Fatalf("threads len = %d, want 3 (needs_you, review, folded no_agent): %v", len(parsed.Threads), parsed.Threads)
	}
	ask := parsed.Threads[0]
	if ask["bead"] != "app-ask" || ask["group"] != "needs_you" || ask["status"] != StatusNeedsMe {
		t.Errorf("ask thread = %v", ask)
	}
	if prVal, hasPR := ask["pr"]; !hasPR || prVal != nil {
		t.Errorf("ask thread pr key: present=%v val=%v, want explicit null", hasPR, prVal)
	}
	if _, hasTitle := ask["title"]; hasTitle {
		t.Errorf("compact thread should omit title: %v", ask)
	}
	rev := parsed.Threads[1]
	prObj, ok := rev["pr"].(map[string]any)
	if !ok || prObj["number"] != float64(42) {
		t.Errorf("review thread pr = %v, want {number: 42}", rev["pr"])
	}
	folded := parsed.Threads[2]
	if folded["group"] != "no_agent" || folded["folded"] != true || folded["count"] != float64(1) {
		t.Errorf("folded no_agent = %v", folded)
	}
}

func largeFleetFixture(t *testing.T, needsMeCount, otherCount int) (Config, []Thread, []Bead, []Bead, []InboxItem, []string) {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := Config{Repo: "/r", Autonomy: AutonomyConfig{Level: 4, Max: 7}}

	var threads []Thread
	var needsMeIDs []string
	for i := range needsMeCount {
		id := fmt.Sprintf("app-need-%03d", i)
		needsMeIDs = append(needsMeIDs, id)
		b := Bead{
			ID:     id,
			Title:  fmt.Sprintf("Decision required on rollout step %d with a verbose descriptive title", i),
			Status: StatusNeedsMe,
			Notes:  fmt.Sprintf("QUESTION: Should we proceed with rollout phase %d across all active clusters?\nOPTIONS:\n1. Proceed\n2. Pause\nCLASS: 7", i),
		}
		// Include one needs_me bead whose agent is working to verify it is still never dropped.
		st := "idle"
		if i == needsMeCount-1 && needsMeCount > 1 {
			st = "working"
		}
		var p *PR
		if i%2 == 0 {
			p = &PR{Number: 1000 + i, State: "OPEN", Title: fmt.Sprintf("PR %d", i), URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", 1000+i)}
		}
		threads = append(threads, thread(b, agent(st, 1), p))
	}

	kinds := []struct {
		name string
		mk   func(i int) Thread
	}{
		{"merged", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-mrg-%03d", i), Title: "Merged change awaiting cleanup and verification", Status: StatusInProgress}
			th := thread(b, agent("idle", 1), &PR{Number: 2000 + i, State: "MERGED", URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", 2000+i)})
			th.Linear = fmt.Sprintf("ENG-%d", 2000+i)
			return th
		}},
		{"review", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-rev-%03d", i), Title: "Open pull request awaiting review from maintainers", Status: StatusInProgress}
			th := thread(b, agent("idle", 1), &PR{Number: 3000 + i, State: "OPEN", Title: "Review item", URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", 3000+i)})
			th.Linear = fmt.Sprintf("ENG-%d", 3000+i)
			return th
		}},
		{"rolling", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-rol-%03d", i), Title: "Rolling out deployment across environments", Status: StatusInProgress, Labels: []string{LabelRollingOut}}
			return thread(b, agent("idle", 1), &PR{Number: 4000 + i, State: "MERGED", URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", 4000+i)})
		}},
		{"failing", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-fai-%03d", i), Title: "Fix failing integration checks on branch", Status: StatusInProgress}
			return thread(b, agent("idle", 1), &PR{Number: 5000 + i, State: "OPEN", URL: fmt.Sprintf("https://github.com/acme/app/pull/%d", 5000+i), Checks: []Check{{Name: "test", Conclusion: "FAILURE"}}})
		}},
		{"idle", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-idl-%03d", i), Title: "Idle worker awaiting next instructions", Status: StatusInProgress}
			return thread(b, agent("idle", 1), nil)
		}},
		{"working", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-wrk-%03d", i), Title: "Actively working on refactoring subsystem", Status: StatusInProgress}
			return thread(b, agent("working", 1), nil)
		}},
		{"blocked", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-blk-%03d", i), Title: "Blocked on upstream dependency release", Status: StatusBlocked}
			return thread(b, nil, nil)
		}},
		{"no_agent", func(i int) Thread {
			b := Bead{ID: fmt.Sprintf("app-nag-%03d", i), Title: "Claimed bead with no live worker process", Status: StatusInProgress}
			return Thread{Bead: b}
		}},
	}
	for i := range otherCount {
		threads = append(threads, kinds[i%len(kinds)].mk(i))
	}

	var next, ready []Bead
	for i := range 8 {
		next = append(next, Bead{ID: fmt.Sprintf("app-nxt-%02d", i), Priority: 1, Type: "task", Status: StatusOpen, Title: "Selected next work item with a long descriptive title"})
	}
	for i := range 15 {
		ready = append(ready, Bead{ID: fmt.Sprintf("app-rdy-%02d", i), Priority: 2, Type: "task", Status: StatusOpen, Title: "Ready backlog work item with a long descriptive title"})
	}
	var inbox []InboxItem
	for i := range 25 {
		kind := EventAgentGone
		if i%5 == 0 {
			kind = EventNeedsYou
		}
		inbox = append(inbox, InboxItem{
			Event: Event{
				Bead:    fmt.Sprintf("app-ibx-%02d", i),
				Kind:    kind,
				Summary: "Detailed inbox notification summary text that explains what happened on the worker thread",
				At:      t0.Add(time.Duration(i) * time.Minute),
			},
			File: fmt.Sprintf("%d-app-ibx-%02d.json", i, i),
		})
	}
	return cfg, threads, next, ready, inbox, needsMeIDs
}

func jsonNamedBeads(t *testing.T, raw string) map[string]bool {
	t.Helper()
	var doc struct {
		Threads []struct {
			Bead  string   `json:"bead"`
			Beads []string `json:"beads"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	out := map[string]bool{}
	for _, th := range doc.Threads {
		if th.Bead != "" {
			out[th.Bead] = true
		}
		for _, id := range th.Beads {
			out[id] = true
		}
	}
	return out
}

func TestContextCompactLargeFixtureBudgetAndNeedsMe(t *testing.T) {
	for _, tc := range []struct {
		name         string
		needsMeCount int
		otherCount   int
	}{
		{"165 threads with 15 needs_me", 15, 150},
		{"160 threads with 60 needs_me", 60, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, threads, next, ready, inbox, needsMeIDs := largeFleetFixture(t, tc.needsMeCount, tc.otherCount)
			full := renderContext(cfg, threads, next, ready, inbox, TickerState{}, nil, t0)
			if len(full) < 20000 {
				t.Fatalf("expected large full context, got only %d bytes", len(full))
			}

			compactText, err := renderContextWith(cfg, threads, next, ready, inbox, TickerState{}, nil, t0, ContextOptions{Compact: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(compactText) >= compactContextBudget {
				t.Errorf("compact text size = %d bytes, want < %d\n%s", len(compactText), compactContextBudget, compactText)
			}
			for _, id := range needsMeIDs {
				if !strings.Contains(compactText, id) {
					t.Errorf("compact text dropped needs_me bead %s", id)
				}
			}
			if strings.Contains(compactText, "Decision required on rollout step") || strings.Contains(compactText, "Should we proceed") {
				t.Errorf("compact text leaked titles or question bodies:\n%s", compactText)
			}

			compactJSON, err := renderContextWith(cfg, threads, next, ready, inbox, TickerState{}, nil, t0, ContextOptions{Compact: true, JSON: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(compactJSON) >= compactContextBudget {
				t.Errorf("compact json size = %d bytes, want < %d\n%s", len(compactJSON), compactContextBudget, compactJSON)
			}
			named := jsonNamedBeads(t, compactJSON)
			for _, id := range needsMeIDs {
				if !named[id] {
					t.Errorf("compact json dropped needs_me bead %s", id)
				}
			}
			if tc.needsMeCount <= 20 {
				var doc struct {
					Threads []struct {
						Bead   string `json:"bead"`
						Status string `json:"status"`
						Folded bool   `json:"folded"`
					} `json:"threads"`
				}
				if err := json.Unmarshal([]byte(compactJSON), &doc); err != nil {
					t.Fatal(err)
				}
				individual := 0
				for _, th := range doc.Threads {
					if !th.Folded && th.Status == StatusNeedsMe && th.Bead != "" {
						individual++
					}
				}
				if individual != tc.needsMeCount {
					t.Errorf("individual needs_me JSON entries = %d, want %d", individual, tc.needsMeCount)
				}
			}
			if gotT, gotJ := presentTextSections(compactText), jsonTopKeys(t, compactJSON); !slices.Equal(gotT, gotJ) {
				t.Errorf("compact section mismatch: text=%v json=%v", gotT, gotJ)
			}
		})
	}
}

func TestContextCompactTruncationOrder(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := Config{Repo: "/r", Autonomy: AutonomyConfig{Level: 4, Max: 7}}

	// Stage 1: slightly over 4KB with ready + non-live threads + inbox.
	// Ready must be truncated while non-live threads and inbox survive.
	var threads1 []Thread
	for i := range 90 {
		threads1 = append(threads1, thread(Bead{ID: fmt.Sprintf("app-idl-%03d", i), Status: StatusInProgress}, agent("idle", 1), nil))
	}
	for i := range 10 {
		threads1 = append(threads1, thread(Bead{ID: fmt.Sprintf("app-blk-%03d", i), Status: StatusBlocked}, nil, nil))
	}
	var ready1 []Bead
	for i := range 15 {
		ready1 = append(ready1, Bead{ID: fmt.Sprintf("app-rdy-%02d", i), Priority: 2, Type: "task", Status: StatusOpen})
	}
	inbox1 := []InboxItem{
		{Event: Event{Bead: "app-ibx-keep", Kind: EventAgentGone, At: t0}},
	}
	// Verify initial size is just over 4KB before truncation and drops below 4KB after ready alone.
	doc1 := buildContextDoc(cfg, threads1, nil, ready1, inbox1, TickerState{}, nil, t0, ContextOptions{Compact: true})
	// Adjust idle count dynamically if needed so ready alone puts us over budget.
	for len(renderContextText(&doc1)) < compactContextBudget {
		n := len(threads1)
		threads1 = append(threads1, thread(Bead{ID: fmt.Sprintf("app-idl-%03d", n), Status: StatusInProgress}, agent("idle", 1), nil))
		doc1 = buildContextDoc(cfg, threads1, nil, ready1, inbox1, TickerState{}, nil, t0, ContextOptions{Compact: true})
	}
	// Trim one ready item's worth if we overshot so dropping ready alone fits.
	out1, err := renderContextWith(cfg, threads1, nil, ready1, inbox1, TickerState{}, nil, t0, ContextOptions{Compact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out1) >= compactContextBudget {
		t.Fatalf("stage 1 output = %d bytes, want < %d", len(out1), compactContextBudget)
	}
	if strings.Contains(out1, "app-rdy-00") || !strings.Contains(out1, "(15 omitted)") {
		t.Errorf("expected ready list truncated first:\n%s", out1)
	}
	if !strings.Contains(out1, "app-blk-000") || !strings.Contains(out1, "app-ibx-keep") {
		t.Errorf("expected non-live threads and inbox preserved when ready truncation suffices:\n%s", out1)
	}

	// Stage 2: over 4KB even without ready; non-live groups fold before inbox is touched.
	var threads2 []Thread
	threads2 = append(threads2, thread(Bead{ID: "app-need-001", Status: StatusNeedsMe}, agent("idle", 1), nil))
	for i := range 80 {
		threads2 = append(threads2, thread(Bead{ID: fmt.Sprintf("app-blk-%03d", i), Status: StatusBlocked}, nil, nil))
	}
	for i := range 80 {
		threads2 = append(threads2, thread(Bead{ID: fmt.Sprintf("app-idl-%03d", i), Status: StatusInProgress}, agent("idle", 1), nil))
	}
	var inbox2 []InboxItem
	for i := range 10 {
		inbox2 = append(inbox2, InboxItem{Event: Event{Bead: fmt.Sprintf("app-ibx-%02d", i), Kind: EventAgentGone, At: t0.Add(time.Duration(i) * time.Minute)}})
	}
	out2, err := renderContextWith(cfg, threads2, nil, ready1, inbox2, TickerState{}, nil, t0, ContextOptions{Compact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out2) >= compactContextBudget {
		t.Fatalf("stage 2 output = %d bytes, want < %d", len(out2), compactContextBudget)
	}
	if !strings.Contains(out2, "- blocked: 80") {
		t.Errorf("expected blocked group folded to count:\n%s", out2)
	}
	for i := range 10 {
		if !strings.Contains(out2, fmt.Sprintf("app-ibx-%02d", i)) {
			t.Errorf("inbox item %d truncated before non-live thread groups folded:\n%s", i, out2)
		}
	}
	if !strings.Contains(out2, "app-need-001") {
		t.Errorf("needs_me bead dropped:\n%s", out2)
	}

	// Stage 4: huge inbox after non-live threads fold -> inbox is trimmed, preferring needs_you.
	var inbox3 []InboxItem
	for i := range 180 {
		kind := EventAgentGone
		if i >= 175 {
			kind = EventNeedsYou
		}
		inbox3 = append(inbox3, InboxItem{Event: Event{Bead: fmt.Sprintf("app-ibx-%03d", i), Kind: kind, At: t0.Add(time.Duration(i) * time.Minute)}})
	}
	out3, err := renderContextWith(cfg, threads2, nil, ready1, inbox3, TickerState{}, nil, t0, ContextOptions{Compact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out3) >= compactContextBudget {
		t.Fatalf("stage 3 output = %d bytes, want < %d", len(out3), compactContextBudget)
	}
	if !strings.Contains(out3, "app-need-001") {
		t.Errorf("needs_me bead dropped under inbox pressure:\n%s", out3)
	}
	if !strings.Contains(out3, "omitted)") {
		t.Errorf("expected inbox omitted footer:\n%s", out3)
	}
	for i := 175; i < 180; i++ {
		if !strings.Contains(out3, fmt.Sprintf("app-ibx-%03d", i)) {
			t.Errorf("expected needs_you inbox item app-ibx-%03d kept over older agent_gone items", i)
		}
	}
}
