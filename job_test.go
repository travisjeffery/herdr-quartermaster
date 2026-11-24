package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const jobTestSHA = "0123456789abcdef0123456789abcdef01234567"

type jobTestHarness struct {
	t   *testing.T
	cfg Config
	now time.Time

	mu          sync.Mutex
	beads       map[string]Bead
	prs         map[string]PR
	prToBead    map[string]string
	beadToPR    map[string]string
	workers     map[string]*Agent
	prompts     map[string]string // bead -> screen text
	noteCalls   int
	tellCalls   int
	mergeCalls  int
	dispCalls   int
	resumeCalls int
	closeCalls  int
	stopCalls   int
	lastTells   []string
	lastMergeBy string
	onAfterInt  func(jobID string) error
	onAfterEff  func(jobID string) error
}

func newJobTestHarness(t *testing.T, globalLevel int) *jobTestHarness {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.Autonomy.Level = globalLevel
	cfg.Autonomy.Max = 7
	return &jobTestHarness{
		t:        t,
		cfg:      cfg,
		now:      time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC),
		beads:    map[string]Bead{},
		prs:      map[string]PR{},
		prToBead: map[string]string{},
		beadToPR: map[string]string{},
		workers:  map[string]*Agent{},
		prompts:  map[string]string{},
	}
}

func (h *jobTestHarness) env() JobEnv {
	pEnv := promptEnv{
		show: h.show,
		agent: func(_ string, name string) (*Agent, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			for id, a := range h.workers {
				if agentName(id) == name || a.Name == name {
					cp := *a
					return &cp, nil
				}
			}
			return nil, nil
		},
		read: func(_, pane string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			for id, a := range h.workers {
				if a.PaneID == pane {
					return h.prompts[id], nil
				}
			}
			return "", nil
		},
		keys: func(_, pane string, _ ...string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			for id, a := range h.workers {
				if a.PaneID == pane {
					// Clear the prompt once keys are sent.
					h.prompts[id] = ""
					a.Status = "working"
				}
			}
			return nil
		},
		note:      h.note,
		setStatus: h.setStatus,
		tell: func(_ string, _ *Agent, text string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.tellCalls++
			h.lastTells = append(h.lastTells, text)
			return nil
		},
		sleep: func(time.Duration) {},
		level: func(id string) (int, string) {
			b, err := h.show(id)
			if err != nil {
				return 0, err.Error()
			}
			return h.cfg.beadLevel(b, autonomyStopped())
		},
	}

	return JobEnv{
		Cfg:       h.cfg,
		Show:      h.show,
		Note:      h.note,
		SetStatus: h.setStatus,
		Prompt:    pEnv,
		ViewPR: func(_ context.Context, url string) (PR, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			pr, ok := h.prs[url]
			if !ok {
				return PR{}, fmt.Errorf("PR %s not found", url)
			}
			return pr, nil
		},
		PRForBead: func(id string) (PR, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			u, ok := h.beadToPR[id]
			if !ok {
				return PR{}, false
			}
			pr, ok := h.prs[u]
			return pr, ok
		},
		BeadForPR: func(url string) (string, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			id, ok := h.prToBead[url]
			return id, ok
		},
		MergePR: func(_ context.Context, b Bead, pr PR, by string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.mergeCalls++
			h.lastMergeBy = by
			cur := h.prs[pr.URL]
			cur.State = "MERGED"
			h.prs[pr.URL] = cur
			return fmt.Sprintf("merged %s at %s", pr.URL, short(pr.HeadSHA)), nil
		},
		Dispatch: func(b Bead, _ string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.dispCalls++
			cur := h.beads[b.ID]
			cur.Status = StatusInProgress
			h.beads[b.ID] = cur
			h.workers[b.ID] = &Agent{Name: agentName(b.ID), Status: "working", PaneID: "p-" + b.ID}
			return fmt.Sprintf("started pi on %s", b.ID), nil
		},
		Resume: func(b Bead, _ string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.resumeCalls++
			h.workers[b.ID] = &Agent{Name: agentName(b.ID), Status: "idle", PaneID: "p-" + b.ID}
			return fmt.Sprintf("resumed pi on %s", b.ID), nil
		},
		Close: func(b Bead, reason, _ string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.closeCalls++
			cur := h.beads[b.ID]
			cur.Status = StatusClosed
			h.beads[b.ID] = cur
			return fmt.Sprintf("closed %s: %s", b.ID, reason), nil
		},
		StopWorker: func(b Bead) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.stopCalls++
			delete(h.workers, b.ID)
			return fmt.Sprintf("stopped worker for %s; bead stays claimed", b.ID), nil
		},
		FleetStatus: func() (string, error) {
			return "1 needs you · 2 working", nil
		},
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.now
		},
		Logf:          func(string, ...any) {},
		OnAfterIntent: h.onAfterInt,
		OnAfterEffect: h.onAfterEff,
	}
}

func (h *jobTestHarness) show(id string) (Bead, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.beads[id]
	if !ok {
		return Bead{}, fmt.Errorf("bead %s not found", id)
	}
	return b, nil
}

func (h *jobTestHarness) note(id, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noteCalls++
	b := h.beads[id]
	if b.Notes != "" {
		b.Notes += "\n"
	}
	b.Notes += text
	h.beads[id] = b
	return nil
}

func (h *jobTestHarness) setStatus(id, status string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.beads[id]
	b.Status = status
	h.beads[id] = b
	return nil
}

func (h *jobTestHarness) runCLI(args ...string) (string, error) {
	var buf bytes.Buffer
	err := jobCmd(h.cfg, h.env(), args, &buf)
	return buf.String(), err
}

func parseQMStatusBlock(t *testing.T, output string) map[string]string {
	t.Helper()
	re := regexp.MustCompile("(?s)```qm-status\\n(.*?)```")
	m := re.FindStringSubmatch(output)
	if m == nil {
		t.Fatalf("missing ```qm-status block in output:\n%s", output)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(m[1]), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || v == "" {
			t.Fatalf("malformed qm-status line %q", line)
		}
		if strings.ContainsAny(v, " \t\r\n") {
			t.Fatalf("qm-status value for %q contains whitespace: %q", k, v)
		}
		fields[k] = v
	}
	return fields
}

func TestJobRequestParsing(t *testing.T) {
	cases := []struct {
		name     string
		tokens   []string
		key      string
		why      string
		text     string
		wantVerb string
		wantTgt  string
		wantCh   string
		wantFT   string
		wantWhy  string
		wantKey  string
		wantErr  bool
	}{
		{
			name:     "answer option with em-dash why",
			tokens:   []string{"ANSWER", "task-1", "2", "—", "TJ", "tapped", "option", "2"},
			key:      "app-20261124-0001",
			wantVerb: JobVerbAnswer,
			wantTgt:  "task-1",
			wantCh:   "2",
			wantWhy:  "TJ tapped option 2",
			wantKey:  "app-20261124-0001",
		},
		{
			name:     "quoted single string with req tag and free text",
			tokens:   []string{"[req:app-20261124-0002] ANSWER task-2 — Keep   two  spaces verbatim"},
			wantVerb: JobVerbAnswer,
			wantTgt:  "task-2",
			wantFT:   "Keep   two  spaces verbatim",
			wantKey:  "app-20261124-0002",
		},
		{
			name:     "merge by PR URL",
			tokens:   []string{"merge", "https://github.com/acme/app/pull/42"},
			key:      "app-20261124-0003",
			wantVerb: JobVerbMerge,
			wantTgt:  "https://github.com/acme/app/pull/42",
			wantKey:  "app-20261124-0003",
		},
		{
			name:     "fleet status without target",
			tokens:   []string{"STATUS"},
			wantVerb: JobVerbStatus,
		},
		{
			name:     "close with reason",
			tokens:   []string{"CLOSE", "task-3", "—", "verified", "live"},
			wantVerb: JobVerbClose,
			wantTgt:  "task-3",
			wantWhy:  "verified live",
		},
		{
			name:    "close without reason fails",
			tokens:  []string{"CLOSE", "task-3"},
			wantErr: true,
		},
		{
			name:    "bare merge fails",
			tokens:  []string{"MERGE"},
			wantErr: true,
		},
		{
			name:    "unknown verb fails",
			tokens:  []string{"DELETE", "task-1"},
			wantErr: true,
		},
		{
			name:    "req tag conflicts with key flag",
			tokens:  []string{"[req:k-a]", "STATUS"},
			key:     "k-b",
			wantErr: true,
		},
		{
			name:    "free text over 2000 runes fails",
			tokens:  []string{"ANSWER", "task-1"},
			text:    strings.Repeat("x", jobMaxFreeTextRune+1),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, key, err := parseJobRequest(tc.tokens, tc.key, tc.why, tc.text, "", "coordinator", -1)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got req=%+v", req)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.Verb != tc.wantVerb || req.Target != tc.wantTgt || req.Choice != tc.wantCh || req.FreeText != tc.wantFT || req.Why != tc.wantWhy || key != tc.wantKey {
				t.Fatalf("got req=%+v key=%q, want verb=%q target=%q choice=%q ft=%q why=%q key=%q",
					req, key, tc.wantVerb, tc.wantTgt, tc.wantCh, tc.wantFT, tc.wantWhy, tc.wantKey)
			}
		})
	}
}

func TestJobSubmitDurableAndRetrieveAfterTickerRestartNoCoordinator(t *testing.T) {
	h := newJobTestHarness(t, 4)
	h.beads["task-1"] = Bead{
		ID:     "task-1",
		Status: StatusNeedsMe,
		Notes:  "QUESTION: Ship canary first? Recommendation: 2\nOPTIONS:\n1. All at once\n2. Canary first\nCLASS: 2 — safe choice",
	}
	h.workers["task-1"] = &Agent{Name: "task-1", Status: "idle", PaneID: "p-1"}

	// Submit returns durable ID immediately without running any daemon or coordinator session.
	out, err := h.runCLI("submit", "--key", "app-20261124-0017", "--by", "TJ", "--", "ANSWER", "task-1", "2", "—", "TJ tapped option 2")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "job=") || fields[1] != "state=queued" {
		t.Fatalf("unexpected submit output: %q", out)
	}
	jobID := strings.TrimPrefix(fields[0], "job=")
	if !jobIDRE.MatchString(jobID) {
		t.Fatalf("job id is not 64-char hex digest: %q", jobID)
	}

	// Status and list work before any ticker pass, using a 6-char prefix, with no daemon alive.
	statusOut, err := h.runCLI("status", jobID[:6])
	if err != nil {
		t.Fatalf("status before tick failed: %v", err)
	}
	if !strings.Contains(statusOut, "state=queued") || !strings.Contains(statusOut, "key=app-20261124-0017") {
		t.Fatalf("unexpected status output:\n%s", statusOut)
	}
	if _, err := h.runCLI("result", jobID); !errors.Is(err, errJobNotTerminal) {
		t.Fatalf("expected errJobNotTerminal while queued, got %v", err)
	}

	// Simulate ticker #1 running with NO coordinator session alive (agents map empty).
	env1 := h.env()
	ticker1 := Ticker{cfg: h.cfg, log: log.New(io.Discard, "", 0), jobEnv: &env1}
	ticker1.processJobs(&TickerState{}, nil, map[string]Agent{}, h.now)

	// Simulate killing ticker #1 and restarting a fresh ticker #2 (no in-memory state carried over).
	h.now = h.now.Add(2 * time.Minute)
	env2 := h.env()
	ticker2 := Ticker{cfg: h.cfg, log: log.New(io.Discard, "", 0), jobEnv: &env2}
	ticker2.processJobs(&TickerState{}, nil, map[string]Agent{}, h.now)

	// Status & result retrieval work cleanly after ticker restart with no coordinator session.
	var statusJSON JobView
	rawStatus, err := h.runCLI("status", jobID[:8], "--json")
	if err != nil {
		t.Fatalf("status --json after restart failed: %v", err)
	}
	if err := json.Unmarshal([]byte(rawStatus), &statusJSON); err != nil {
		t.Fatalf("unmarshal status JSON: %v", err)
	}
	if statusJSON.State != JobStateDone || statusJSON.Bead != "task-1" || statusJSON.BeadStatus != StatusInProgress {
		t.Fatalf("unexpected status view: %+v", statusJSON)
	}

	resultText, err := h.runCLI("result", jobID[:8])
	if err != nil {
		t.Fatalf("result failed: %v", err)
	}
	if !strings.Contains(resultText, "[req:app-20261124-0017]") || !strings.Contains(resultText, "NEEDS_ME ANSWERED: 2 Canary first") {
		t.Fatalf("result missing correlation tag or audit note:\n%s", resultText)
	}
	qm := parseQMStatusBlock(t, resultText)
	if qm["req"] != "app-20261124-0017" || qm["state"] != "done" || qm["bead"] != "task-1" || qm["bead_status"] != "in_progress" || qm["job"] != jobID {
		t.Fatalf("unexpected qm-status fields: %+v", qm)
	}

	// Verify answer ran strictly once across both ticker instances.
	if h.noteCalls != 1 || h.tellCalls != 1 {
		t.Fatalf("expected 1 note and 1 tell, got note=%d tell=%d", h.noteCalls, h.tellCalls)
	}
}

func TestJobIdempotencyKeyDedupConflictAndConcurrentSubmit(t *testing.T) {
	h := newJobTestHarness(t, 4)
	h.beads["task-1"] = Bead{ID: "task-1", Status: StatusOpen}

	// Concurrent submits with the same idempotency key must all return the exact same job ID.
	const n = 10
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	req := JobRequest{Verb: JobVerbDispatch, Target: "task-1", By: "coordinator", Class: -1}
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			v, err := submitJob(req, "req-concurrent-1", h.now)
			ids[idx] = v.Job
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d submit failed: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d returned different id %s != %s", i, ids[i], ids[0])
		}
	}

	// Execute queued jobs once.
	if got := processPendingJobs(context.Background(), h.env()); got != 1 {
		t.Fatalf("expected 1 progressed job, got %d", got)
	}
	if h.dispCalls != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", h.dispCalls)
	}

	// Re-submitting the same key after completion returns the original done job and never re-runs.
	dup, err := submitJob(req, "req-concurrent-1", h.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("resubmit failed: %v", err)
	}
	if !dup.Deduped || dup.Job != ids[0] || dup.State != JobStateDone {
		t.Fatalf("unexpected deduped view: %+v", dup)
	}
	if got := processPendingJobs(context.Background(), h.env()); got != 0 {
		t.Fatalf("expected 0 progressed jobs after dedup, got %d", got)
	}
	if h.dispCalls != 1 {
		t.Fatalf("dispatch duplicated after resubmit: %d", h.dispCalls)
	}

	// Re-submitting the same key with a DIFFERENT request must return conflict.
	other := JobRequest{Verb: JobVerbStop, Target: "task-1", By: "coordinator", Class: -1}
	if _, err := submitJob(other, "req-concurrent-1", h.now); !errors.Is(err, errJobKeyConflict) {
		t.Fatalf("expected errJobKeyConflict, got %v", err)
	}

	// Crash-window repair: delete keys/<key>.json while job.json remains on disk; resubmit repairs index.
	kp, err := jobKeyPath("req-concurrent-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(kp); err != nil {
		t.Fatal(err)
	}
	repaired, err := submitJob(req, "req-concurrent-1", h.now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("resubmit repair failed: %v", err)
	}
	if !repaired.Deduped || repaired.Job != ids[0] || repaired.State != JobStateDone {
		t.Fatalf("unexpected repaired view: %+v", repaired)
	}
	if _, err := os.Stat(kp); err != nil {
		t.Fatalf("expected key index repaired on disk: %v", err)
	}
}

func TestJobAnswerNeedsMeAtMostOnceAndFreeText(t *testing.T) {
	h := newJobTestHarness(t, 4)
	h.beads["task-1"] = Bead{
		ID:     "task-1",
		Status: StatusNeedsMe,
		Notes:  "QUESTION: Pick rollout mode?\nOPTIONS:\n1. Fast\n2. Safe\nCLASS: 4 — rollout",
	}
	h.workers["task-1"] = &Agent{Name: "task-1", Status: "idle", PaneID: "p-1"}

	// Two distinct jobs (different idempotency keys) racing for the same live question.
	j1, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-1", Choice: "2", By: "TJ", Class: -1}, "key-first", h.now)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-1", Choice: "1", By: "TJ", Class: -1}, "key-second", h.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	processPendingJobs(context.Background(), h.env())

	s1, e1, _ := loadJob(j1.Job)
	v1 := reconstructJob(s1, e1, h.now)
	s2, e2, _ := loadJob(j2.Job)
	v2 := reconstructJob(s2, e2, h.now)

	if v1.State != JobStateDone {
		t.Fatalf("first answer job should be done, got %+v", v1)
	}
	if v2.State != JobStateBlocked || !strings.Contains(v2.Why, "already answered") {
		t.Fatalf("second answer job should be blocked (already answered), got %+v", v2)
	}
	if h.noteCalls != 1 || h.tellCalls != 1 {
		t.Fatalf("expected exactly 1 answer effect, got note=%d tell=%d", h.noteCalls, h.tellCalls)
	}

	// Free-text answer on a needs_me bead WITHOUT an OPTIONS block lands verbatim.
	h.beads["task-2"] = Bead{
		ID:     "task-2",
		Status: StatusNeedsMe,
		Notes:  "QUESTION: Which accounts should be included?",
	}
	verbatim := "Include account-a  and account-b; leave the rest off."
	ftReq, ftKey, err := parseJobRequest([]string{"ANSWER", "task-2", "—", verbatim}, "key-freetext-1", "", "", "", "TJ", -1)
	if err != nil {
		t.Fatal(err)
	}
	jf1, err := submitJob(ftReq, ftKey, h.now)
	if err != nil {
		t.Fatal(err)
	}
	jf2, err := submitJob(ftReq, "key-freetext-2", h.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	processPendingJobs(context.Background(), h.env())

	sf1, ef1, _ := loadJob(jf1.Job)
	vf1 := reconstructJob(sf1, ef1, h.now)
	sf2, ef2, _ := loadJob(jf2.Job)
	vf2 := reconstructJob(sf2, ef2, h.now)
	if vf1.State != JobStateDone || !strings.Contains(vf1.AuditNote, verbatim) {
		t.Fatalf("free-text job failed or altered text: %+v", vf1)
	}
	if vf2.State != JobStateBlocked || !strings.Contains(vf2.Why, "already answered") {
		t.Fatalf("second free-text job should be blocked, got %+v", vf2)
	}
	if h.beads["task-2"].Status != StatusInProgress {
		t.Fatalf("expected task-2 back to in_progress, got %s", h.beads["task-2"].Status)
	}
}

func TestJobConcurrentAnswerRaceAppliesAtMostOnce(t *testing.T) {
	h := newJobTestHarness(t, 4)
	h.beads["task-race"] = Bead{
		ID:     "task-race",
		Status: StatusNeedsMe,
		Notes:  "QUESTION: Proceed?\nOPTIONS:\n1. Yes\n2. No\nCLASS: 2 — test",
	}

	j1, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-race", Choice: "1", By: "TJ", Class: -1}, "race-a", h.now)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-race", Choice: "2", By: "TJ", Class: -1}, "race-b", h.now)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var doneCount, blockedCount atomic.Int32
	for _, id := range []string{j1.Job, j2.Job} {
		wg.Add(1)
		go func(jid string) {
			defer wg.Done()
			v, serr := stepJob(context.Background(), h.env(), jid)
			if serr != nil {
				t.Errorf("stepJob %s: %v", short(jid), serr)
				return
			}
			switch v.State {
			case JobStateDone:
				doneCount.Add(1)
			case JobStateBlocked:
				blockedCount.Add(1)
			}
		}(id)
	}
	wg.Wait()

	if doneCount.Load() != 1 || blockedCount.Load() != 1 {
		t.Fatalf("expected 1 done and 1 blocked under race, got done=%d blocked=%d", doneCount.Load(), blockedCount.Load())
	}
	if h.noteCalls != 1 {
		t.Fatalf("expected exactly 1 note written under concurrent race, got %d", h.noteCalls)
	}
}

func TestJobAnswerApprovalPromptAtMostOnceAndClassifierGuard(t *testing.T) {
	h := newJobTestHarness(t, 4)
	h.beads["task-p"] = Bead{ID: "task-p", Status: StatusInProgress}
	h.workers["task-p"] = &Agent{Name: "task-p", Status: "blocked", PaneID: "p-1"}
	h.prompts["task-p"] = strings.Join([]string{
		"──────────────────────────────",
		"Bash command",
		"git status",
		"❯ 1. Yes",
		"  2. No",
		"Enter to confirm · Esc to cancel",
	}, "\n")

	j1, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-p", Choice: "1", By: "coordinator", Class: 2}, "prompt-1", h.now)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-p", Choice: "1", By: "coordinator", Class: 2}, "prompt-2", h.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	processPendingJobs(context.Background(), h.env())

	s1, e1, _ := loadJob(j1.Job)
	v1 := reconstructJob(s1, e1, h.now)
	s2, e2, _ := loadJob(j2.Job)
	v2 := reconstructJob(s2, e2, h.now)
	if v1.State != JobStateDone {
		t.Fatalf("expected prompt job 1 done, got %+v", v1)
	}
	if v2.State != JobStateBlocked {
		t.Fatalf("expected prompt job 2 blocked, got %+v", v2)
	}

	// Standing permission ("Don't ask again") by coordinator is class 9 -> needs_tj.
	h.workers["task-p"].Status = "blocked"
	h.prompts["task-p"] = strings.Join([]string{
		"──────────────────────────────",
		"Bash command",
		"rm -rf build",
		"❯ 1. Yes, don't ask again for this session",
		"  2. No",
		"Enter to confirm · Esc to cancel",
	}, "\n")
	j3, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-p", Choice: "1", By: "coordinator", Class: 2}, "prompt-standing", h.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	processPendingJobs(context.Background(), h.env())
	s3, e3, _ := loadJob(j3.Job)
	v3 := reconstructJob(s3, e3, h.now)
	if v3.State != JobStateNeedsTJ || v3.Class != 9 {
		t.Fatalf("expected standing-permission prompt to stop in needs_tj class 9, got %+v", v3)
	}
}

func TestJobParkedNeedsMeWaitMultiDayAndAtMostOnceResume(t *testing.T) {
	// Autonomy L2 < merge class 4 -> MERGE job parks on a needs_me question.
	h := newJobTestHarness(t, 2)
	prURL := "https://github.com/acme/app/pull/77"
	h.beads["task-m"] = Bead{ID: "task-m", Status: StatusInProgress}
	h.prs[prURL] = PR{URL: prURL, Number: 77, State: "OPEN", HeadSHA: jobTestSHA}
	h.beadToPR["task-m"] = prURL
	h.prToBead[prURL] = "task-m"

	jm, err := submitJob(JobRequest{Verb: JobVerbMerge, Target: prURL, By: "coordinator", Class: -1}, "merge-req-1", h.now)
	if err != nil {
		t.Fatal(err)
	}

	// Tick 1: job parks on needs_me approval question.
	processPendingJobs(context.Background(), h.env())
	sm, em, _ := loadJob(jm.Job)
	vm := reconstructJob(sm, em, h.now)
	if vm.State != JobStateParked || vm.ParkHash == "" || vm.Bead != "task-m" {
		t.Fatalf("expected parked merge job with park hash, got %+v", vm)
	}
	if h.beads["task-m"].Status != StatusNeedsMe {
		t.Fatalf("expected bead set to needs_me, got %s", h.beads["task-m"].Status)
	}

	// Advance 5 days across multiple ticker passes: job stays parked, never expires silently, and list shows age.
	for d := 1; d <= 5; d++ {
		h.now = h.now.Add(24 * time.Hour)
		processPendingJobs(context.Background(), h.env())
	}
	listOut, err := h.runCLI("list", "--bead", "task-m")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(listOut, "parked") || !strings.Contains(listOut, "5d") {
		t.Fatalf("expected job list to show parked age 5d:\n%s", listOut)
	}
	if h.mergeCalls != 0 {
		t.Fatalf("merge must not run while parked, got %d", h.mergeCalls)
	}

	// A non-TJ note (even if trailing prose mentions "by TJ") must NOT unlock the parked approval.
	_ = h.note("task-m", fmt.Sprintf("NEEDS_ME ANSWERED: 1 Approve [%s] by coordinator via job deadbeef — approved by TJ", vm.ParkHash))
	processPendingJobs(context.Background(), h.env())
	sm, em, _ = loadJob(jm.Job)
	if cur := reconstructJob(sm, em, h.now); cur.State != JobStateParked || h.mergeCalls != 0 {
		t.Fatalf("non-TJ answer must not unlock parked job: state=%s merges=%d", cur.State, h.mergeCalls)
	}

	// Restore clean question note and have TJ answer option 1 via ANSWER job.
	// Note: reset bead notes back to the parked question so ANSWER parses it cleanly.
	b := h.beads["task-m"]
	b.Notes = vm.AuditNote
	h.beads["task-m"] = b

	ja, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-m", Choice: "1", By: "TJ", Class: -1}, "tj-approve-1", h.now)
	if err != nil {
		t.Fatal(err)
	}
	// Single tick runs ANSWER job AND immediately wakes the parked MERGE job!
	processPendingJobs(context.Background(), h.env())

	sa, ea, _ := loadJob(ja.Job)
	va := reconstructJob(sa, ea, h.now)
	sm, em, _ = loadJob(jm.Job)
	vm = reconstructJob(sm, em, h.now)
	if va.State != JobStateDone {
		t.Fatalf("expected TJ answer job done, got %+v", va)
	}
	if vm.State != JobStateDone || vm.PRState != "merged" || vm.Head != jobTestSHA {
		t.Fatalf("expected parked merge job done after TJ approval, got %+v", vm)
	}
	if h.mergeCalls != 1 || h.lastMergeBy != "TJ" {
		t.Fatalf("expected exactly 1 merge as TJ, got calls=%d by=%q", h.mergeCalls, h.lastMergeBy)
	}

	// Additional ticks and duplicate answer job must NEVER re-apply the parked answer.
	jaDup, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-m", Choice: "1", By: "TJ", Class: -1}, "tj-approve-2", h.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	processPendingJobs(context.Background(), h.env())
	processPendingJobs(context.Background(), h.env())
	sd, ed, _ := loadJob(jaDup.Job)
	vd := reconstructJob(sd, ed, h.now)
	if vd.State != JobStateBlocked {
		t.Fatalf("duplicate answer after parked resume should be blocked, got %+v", vd)
	}
	if h.mergeCalls != 1 {
		t.Fatalf("parked merge applied more than once: %d", h.mergeCalls)
	}

	// Test decline (option 2) on a second parked job.
	prURL2 := "https://github.com/acme/app/pull/78"
	h.beads["task-m2"] = Bead{ID: "task-m2", Status: StatusInProgress}
	h.prs[prURL2] = PR{URL: prURL2, Number: 78, State: "OPEN", HeadSHA: jobTestSHA}
	h.beadToPR["task-m2"] = prURL2
	jm2, err := submitJob(JobRequest{Verb: JobVerbMerge, Target: "task-m2", By: "coordinator", Class: -1}, "merge-req-2", h.now)
	if err != nil {
		t.Fatal(err)
	}
	processPendingJobs(context.Background(), h.env())
	sm2, em2, _ := loadJob(jm2.Job)
	vm2 := reconstructJob(sm2, em2, h.now)
	if vm2.State != JobStateParked {
		t.Fatalf("expected second merge job parked, got %+v", vm2)
	}
	// Simulate phone tap recording option 2 in journalNeedsMe.
	_ = appendJournal(journalRecord{
		At:   h.now,
		Kind: journalNeedsMe,
		Bead: "task-m2",
		Text: fmt.Sprintf("NEEDS_ME ANSWERED: 2 Decline [%s] by TJ via tap (device phone)", vm2.ParkHash),
	})
	processPendingJobs(context.Background(), h.env())
	sm2, em2, _ = loadJob(jm2.Job)
	vm2 = reconstructJob(sm2, em2, h.now)
	if vm2.State != JobStateBlocked || !strings.Contains(vm2.Why, "declined") {
		t.Fatalf("expected declined parked job blocked, got %+v", vm2)
	}
	if h.mergeCalls != 1 {
		t.Fatalf("declined merge must never execute, got total mergeCalls=%d", h.mergeCalls)
	}
}

func TestJobCancelBeforeAndAfterEffect(t *testing.T) {
	h := newJobTestHarness(t, 2)
	h.beads["task-c1"] = Bead{ID: "task-c1", Status: StatusOpen}
	h.beads["task-c2"] = Bead{ID: "task-c2", Status: StatusInProgress}

	// 1. Cancel a queued job before execution.
	jq, err := submitJob(JobRequest{Verb: JobVerbDispatch, Target: "task-c1", By: "TJ", Class: -1}, "cancel-q", h.now)
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.runCLI("cancel", jq.Job[:8], "--reason", "changed mind")
	if err != nil {
		t.Fatalf("cancel queued failed: %v", err)
	}
	if !strings.Contains(out, "state=blocked") || !strings.Contains(out, "canceled=true") {
		t.Fatalf("unexpected cancel output: %q", out)
	}
	processPendingJobs(context.Background(), h.env())
	if h.dispCalls != 0 {
		t.Fatalf("canceled queued job still executed!")
	}

	// 2. Cancel a parked job before TJ answers.
	jp, err := submitJob(JobRequest{Verb: JobVerbDispatch, Target: "task-c2", By: "coordinator", Class: -1}, "cancel-p", h.now)
	if err != nil {
		t.Fatal(err)
	}
	processPendingJobs(context.Background(), h.env())
	sp, ep, _ := loadJob(jp.Job)
	vp := reconstructJob(sp, ep, h.now)
	if vp.State != JobStateParked {
		t.Fatalf("expected parked, got %+v", vp)
	}
	if _, err := cancelJob(h.env(), jp.Job, "no longer needed", "TJ"); err != nil {
		t.Fatalf("cancel parked failed: %v", err)
	}
	// Late answer after cancel must not execute the canceled job.
	_ = appendJournal(journalRecord{
		At:   h.now,
		Kind: journalNeedsMe,
		Bead: "task-c2",
		Text: fmt.Sprintf("NEEDS_ME ANSWERED: 1 Approve [%s] by TJ via tap", vp.ParkHash),
	})
	processPendingJobs(context.Background(), h.env())
	if h.dispCalls != 0 {
		t.Fatalf("canceled parked job executed after late answer!")
	}

	// 3. Cannot cancel an already terminal (done) job.
	js, err := submitJob(JobRequest{Verb: JobVerbStatus, Target: "task-c1", By: "coordinator", Class: -1}, "cancel-done", h.now)
	if err != nil {
		t.Fatal(err)
	}
	processPendingJobs(context.Background(), h.env())
	if _, err := cancelJob(h.env(), js.Job, "too late", "TJ"); !errors.Is(err, errJobNotCancel) {
		t.Fatalf("expected errJobNotCancel for done job, got %v", err)
	}
}

func TestJobCrashWindows(t *testing.T) {
	simCrash := errors.New("simulated ticker crash")

	t.Run("crash after job.json before events.jsonl", func(t *testing.T) {
		h := newJobTestHarness(t, 4)
		h.beads["task-w1"] = Bead{ID: "task-w1", Status: StatusInProgress}
		spec := JobSpec{
			Version:   jobSchemaVersion,
			Key:       "crash-no-events",
			Request:   JobRequest{Verb: JobVerbStatus, Target: "task-w1", By: "coordinator", Class: -1},
			CreatedAt: h.now,
		}
		id, err := spec.digest()
		if err != nil {
			t.Fatal(err)
		}
		spec.ID = id
		if err := storeJobSpec(spec); err != nil {
			t.Fatal(err)
		}
		// No events.jsonl yet; ticker restart must pick it up and complete it.
		processPendingJobs(context.Background(), h.env())
		s, evs, err := loadJob(id)
		if err != nil {
			t.Fatal(err)
		}
		if v := reconstructJob(s, evs, h.now); v.State != JobStateDone {
			t.Fatalf("expected done after recovery, got %+v", v)
		}
	})

	t.Run("torn trailing JSONL line ignored safely", func(t *testing.T) {
		h := newJobTestHarness(t, 4)
		h.beads["task-w2"] = Bead{ID: "task-w2", Status: StatusInProgress}
		j, err := submitJob(JobRequest{Verb: JobVerbStatus, Target: "task-w2", By: "coordinator", Class: -1}, "torn-line", h.now)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(jobDir(j.Job), "events.jsonl")
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("{\"at\":\"2026-11-24T15:00:00Z\",\"kind\":\"int")
		_ = f.Close()

		s, evs, err := loadJob(j.Job)
		if err != nil {
			t.Fatalf("loadJob with torn line failed: %v", err)
		}
		if v := reconstructJob(s, evs, h.now); v.State != JobStateQueued {
			t.Fatalf("expected queued despite torn trailing line, got %+v", v)
		}
	})

	t.Run("ANSWER needs_me crashes after effect before outcome settles once", func(t *testing.T) {
		h := newJobTestHarness(t, 4)
		h.beads["task-w3"] = Bead{
			ID:     "task-w3",
			Status: StatusNeedsMe,
			Notes:  "QUESTION: Deploy now?\nOPTIONS:\n1. Yes\n2. No\nCLASS: 2 — test",
		}
		h.workers["task-w3"] = &Agent{Name: "task-w3", Status: "idle", PaneID: "p-w3"}
		h.onAfterEff = func(string) error { return simCrash }

		j, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-w3", Choice: "1", By: "TJ", Class: -1}, "crash-ans-after", h.now)
		if err != nil {
			t.Fatal(err)
		}
		// Ticker #1 applies answer, then crashes before writing outcome/done.
		processPendingJobs(context.Background(), h.env())
		s, evs, _ := loadJob(j.Job)
		mid := reconstructJob(s, evs, h.now)
		if mid.State != JobStateRunning || !mid.intentPending {
			t.Fatalf("expected running+intentPending after simulated crash, got %+v", mid)
		}
		if h.noteCalls != 1 || h.tellCalls != 1 {
			t.Fatalf("expected 1 effect before crash, got note=%d tell=%d", h.noteCalls, h.tellCalls)
		}

		// Ticker #2 restarts (crash hook cleared) and reconciles via probe without re-applying.
		h.onAfterEff = nil
		processPendingJobs(context.Background(), h.env())
		s, evs, _ = loadJob(j.Job)
		after := reconstructJob(s, evs, h.now)
		if after.State != JobStateDone {
			t.Fatalf("expected recovered job done, got %+v", after)
		}
		if h.noteCalls != 1 || h.tellCalls != 1 {
			t.Fatalf("crash recovery duplicated answer effect! note=%d tell=%d", h.noteCalls, h.tellCalls)
		}
	})

	t.Run("ANSWER needs_me crashes after intent before effect never blind retries", func(t *testing.T) {
		h := newJobTestHarness(t, 4)
		h.beads["task-w4"] = Bead{
			ID:     "task-w4",
			Status: StatusNeedsMe,
			Notes:  "QUESTION: Deploy now?\nOPTIONS:\n1. Yes\n2. No\nCLASS: 2 — test",
		}
		h.onAfterInt = func(string) error { return simCrash }

		j, err := submitJob(JobRequest{Verb: JobVerbAnswer, Target: "task-w4", Choice: "1", By: "TJ", Class: -1}, "crash-ans-before", h.now)
		if err != nil {
			t.Fatal(err)
		}
		processPendingJobs(context.Background(), h.env())
		if h.noteCalls != 0 {
			t.Fatalf("expected 0 note calls before crash, got %d", h.noteCalls)
		}

		// Restart ticker: ANSWER is not retry-safe -> stops in needs_tj instead of blind-retrying.
		h.onAfterInt = nil
		processPendingJobs(context.Background(), h.env())
		s, evs, _ := loadJob(j.Job)
		after := reconstructJob(s, evs, h.now)
		if after.State != JobStateNeedsTJ {
			t.Fatalf("expected needs_tj after intent-only crash on ANSWER, got %+v", after)
		}
		if h.noteCalls != 0 {
			t.Fatalf("non-retry-safe ANSWER should not blind-retry, got noteCalls=%d", h.noteCalls)
		}
	})

	t.Run("MERGE crashes after merge before outcome recovers via probe once", func(t *testing.T) {
		h := newJobTestHarness(t, 4)
		prURL := "https://github.com/acme/app/pull/88"
		h.beads["task-w5"] = Bead{ID: "task-w5", Status: StatusInProgress}
		h.prs[prURL] = PR{URL: prURL, Number: 88, State: "OPEN", HeadSHA: jobTestSHA}
		h.beadToPR["task-w5"] = prURL
		h.onAfterEff = func(string) error { return simCrash }

		j, err := submitJob(JobRequest{Verb: JobVerbMerge, Target: "task-w5", By: "coordinator", Class: -1}, "crash-merge-after", h.now)
		if err != nil {
			t.Fatal(err)
		}
		processPendingJobs(context.Background(), h.env())
		if h.mergeCalls != 1 {
			t.Fatalf("expected 1 merge before crash, got %d", h.mergeCalls)
		}
		h.onAfterEff = nil
		processPendingJobs(context.Background(), h.env())
		s, evs, _ := loadJob(j.Job)
		after := reconstructJob(s, evs, h.now)
		if after.State != JobStateDone || after.PRState != "merged" {
			t.Fatalf("expected recovered merge done, got %+v", after)
		}
		if h.mergeCalls != 1 {
			t.Fatalf("merge ran twice on recovery: %d", h.mergeCalls)
		}
	})

	t.Run("MERGE crashes after intent before merge retries safely", func(t *testing.T) {
		h := newJobTestHarness(t, 4)
		prURL := "https://github.com/acme/app/pull/89"
		h.beads["task-w6"] = Bead{ID: "task-w6", Status: StatusInProgress}
		h.prs[prURL] = PR{URL: prURL, Number: 89, State: "OPEN", HeadSHA: jobTestSHA}
		h.beadToPR["task-w6"] = prURL
		h.onAfterInt = func(string) error { return simCrash }

		j, err := submitJob(JobRequest{Verb: JobVerbMerge, Target: "task-w6", By: "coordinator", Class: -1}, "crash-merge-before", h.now)
		if err != nil {
			t.Fatal(err)
		}
		processPendingJobs(context.Background(), h.env())
		if h.mergeCalls != 0 {
			t.Fatalf("expected 0 merge calls before crash, got %d", h.mergeCalls)
		}
		h.onAfterInt = nil
		processPendingJobs(context.Background(), h.env())
		s, evs, _ := loadJob(j.Job)
		after := reconstructJob(s, evs, h.now)
		if after.State != JobStateDone || h.mergeCalls != 1 {
			t.Fatalf("expected retry-safe merge to complete once, state=%s calls=%d", after.State, h.mergeCalls)
		}
	})

	t.Run("parked needs_me resume crashes after effect applies at most once", func(t *testing.T) {
		h := newJobTestHarness(t, 2) // L2 < close class 3 -> parks
		h.beads["task-w7"] = Bead{ID: "task-w7", Status: StatusInProgress}

		j, err := submitJob(JobRequest{Verb: JobVerbClose, Target: "task-w7", Why: "verified", By: "coordinator", Class: -1}, "crash-parked-close", h.now)
		if err != nil {
			t.Fatal(err)
		}
		processPendingJobs(context.Background(), h.env())
		s, evs, _ := loadJob(j.Job)
		parked := reconstructJob(s, evs, h.now)
		if parked.State != JobStateParked {
			t.Fatalf("expected parked, got %+v", parked)
		}

		// TJ answers option 1; crash occurs right after Close succeeded before outcome/done persisted.
		_ = appendJournal(journalRecord{
			At:   h.now,
			Kind: journalNeedsMe,
			Bead: "task-w7",
			Text: fmt.Sprintf("NEEDS_ME ANSWERED: 1 Approve [%s] by TJ via tap", parked.ParkHash),
		})
		h.onAfterEff = func(string) error { return simCrash }
		processPendingJobs(context.Background(), h.env())
		if h.closeCalls != 1 {
			t.Fatalf("expected 1 close before crash, got %d", h.closeCalls)
		}

		// Restart ticker: must settle done via probe without re-applying parked answer or re-closing.
		h.onAfterEff = nil
		processPendingJobs(context.Background(), h.env())
		s, evs, _ = loadJob(j.Job)
		after := reconstructJob(s, evs, h.now)
		if after.State != JobStateDone || after.BeadStatus != StatusClosed {
			t.Fatalf("expected recovered parked close done, got %+v", after)
		}
		if h.closeCalls != 1 {
			t.Fatalf("parked close applied more than once: %d", h.closeCalls)
		}
	})

	t.Run("crash after outcome(ok) before done settles cleanly", func(t *testing.T) {
		h := newJobTestHarness(t, 4)
		h.beads["task-w8"] = Bead{ID: "task-w8", Status: StatusInProgress}
		j, err := submitJob(JobRequest{Verb: JobVerbClose, Target: "task-w8", Why: "shipped", By: "TJ", Class: -1}, "crash-after-outcome", h.now)
		if err != nil {
			t.Fatal(err)
		}
		spec, _, _ := loadJob(j.Job)
		_ = appendJobEvent(spec, jobEvent{At: h.now, Kind: jobEventClaimed, Bead: "task-w8", Class: -1, Level: -1})
		_ = appendJobEvent(spec, jobEvent{At: h.now, Kind: jobEventIntent, Attempt: 1, Bead: "task-w8", Class: 3, Level: -1})
		_ = appendJobEvent(spec, jobEvent{At: h.now, Kind: jobEventOutcome, Attempt: 1, Bead: "task-w8", BeadStatus: StatusClosed, Class: 3, Level: -1, Result: resultOK, Summary: "closed task-w8"})

		processPendingJobs(context.Background(), h.env())
		s, evs, _ := loadJob(j.Job)
		after := reconstructJob(s, evs, h.now)
		if after.State != JobStateDone || h.closeCalls != 0 {
			t.Fatalf("expected done with 0 extra close calls, got state=%s calls=%d", after.State, h.closeCalls)
		}
	})
}

func TestJobAllGateVerbsAndListFiltering(t *testing.T) {
	h := newJobTestHarness(t, 4)
	h.beads["task-v"] = Bead{ID: "task-v", Status: StatusOpen}
	h.beads["task-other"] = Bead{ID: "task-other", Status: StatusInProgress}

	// STATUS (fleet)
	jsFleet, err := submitJob(JobRequest{Verb: JobVerbStatus, By: "coordinator", Class: -1}, "v-status-fleet", h.now)
	if err != nil {
		t.Fatal(err)
	}
	// HOLD task-v
	jHold, err := submitJob(JobRequest{Verb: JobVerbHold, Target: "task-v", Why: "wait on vendor", By: "coordinator", Class: -1}, "v-hold", h.now.Add(1*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// DISPATCH task-v while held -> must be blocked
	jDispBlocked, err := submitJob(JobRequest{Verb: JobVerbDispatch, Target: "task-v", By: "coordinator", Class: -1}, "v-disp-held", h.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// RESUME task-v -> clears hold & resumes
	jResume, err := submitJob(JobRequest{Verb: JobVerbResume, Target: "task-v", By: "coordinator", Class: -1}, "v-resume", h.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// STOP task-v -> stops worker, keeps bead claimed
	jStop, err := submitJob(JobRequest{Verb: JobVerbStop, Target: "task-v", By: "coordinator", Class: -1}, "v-stop", h.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// CLOSE task-v
	jClose, err := submitJob(JobRequest{Verb: JobVerbClose, Target: "task-v", Why: "verified", By: "coordinator", Class: -1}, "v-close", h.now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// STATUS task-other
	jOther, err := submitJob(JobRequest{Verb: JobVerbStatus, Target: "task-other", By: "coordinator", Class: -1}, "v-status-other", h.now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	processPendingJobs(context.Background(), h.env())

	checkState := func(id, want string) JobView {
		t.Helper()
		s, e, err := loadJob(id)
		if err != nil {
			t.Fatalf("loadJob %s: %v", id, err)
		}
		v := reconstructJob(s, e, h.now.Add(10*time.Second))
		if v.State != want {
			t.Fatalf("job %s (%s): got state %s (why=%q), want %s", short(id), v.Verb, v.State, v.Why, want)
		}
		return v
	}

	vf := checkState(jsFleet.Job, JobStateDone)
	if !strings.Contains(vf.Summary, "1 needs you") {
		t.Fatalf("unexpected fleet summary: %q", vf.Summary)
	}
	checkState(jHold.Job, JobStateDone)
	vb := checkState(jDispBlocked.Job, JobStateBlocked)
	if !strings.Contains(vb.Why, "on hold") {
		t.Fatalf("expected hold block reason, got %q", vb.Why)
	}
	checkState(jResume.Job, JobStateDone)
	checkState(jStop.Job, JobStateDone)
	vc := checkState(jClose.Job, JobStateDone)
	if vc.BeadStatus != StatusClosed {
		t.Fatalf("expected closed status, got %q", vc.BeadStatus)
	}
	checkState(jOther.Job, JobStateDone)

	// Verify list ordering (newest first), --bead filter, --state filter, --limit, and --json.
	raw, err := h.runCLI("list", "--bead", "task-v", "--json")
	if err != nil {
		t.Fatalf("list --json failed: %v", err)
	}
	var listed []JobView
	if err := json.Unmarshal([]byte(raw), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(listed) != 5 {
		t.Fatalf("expected 5 jobs for task-v, got %d", len(listed))
	}
	if listed[0].Job != jClose.Job || listed[len(listed)-1].Job != jHold.Job {
		t.Fatalf("expected newest-first order (close first, hold last), got first=%s last=%s", listed[0].Verb, listed[len(listed)-1].Verb)
	}

	rawBlocked, err := h.runCLI("list", "--state", "blocked", "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("list --state blocked failed: %v", err)
	}
	var blocked []JobView
	if err := json.Unmarshal([]byte(rawBlocked), &blocked); err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].Job != jDispBlocked.Job {
		t.Fatalf("unexpected filtered blocked list: %+v", blocked)
	}
}

func TestJobCLIEndToEndNoDaemonOrConfig(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())

	// Capture stdout from main.go run() with no config.toml and no running daemon.
	capture := func(args ...string) (string, error) {
		orig := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		runErr := run(args)
		_ = w.Close()
		os.Stdout = orig
		b, _ := io.ReadAll(r)
		_ = r.Close()
		return string(b), runErr
	}

	out, err := capture("job", "submit", "--key", "cli-no-daemon-1", "--", "STATUS", "task-cli")
	if err != nil {
		t.Fatalf("run job submit failed: %v", err)
	}
	if !strings.Contains(out, "job=") || !strings.Contains(out, "state=queued") {
		t.Fatalf("unexpected submit stdout: %q", out)
	}
	id := strings.TrimPrefix(strings.Fields(strings.TrimSpace(out))[0], "job=")

	stOut, err := capture("job", "status", id[:6])
	if err != nil {
		t.Fatalf("run job status failed: %v", err)
	}
	if !strings.Contains(stOut, "state=queued") || !strings.Contains(stOut, "bead=task-cli") {
		t.Fatalf("unexpected status stdout:\n%s", stOut)
	}

	listOut, err := capture("job", "list")
	if err != nil {
		t.Fatalf("run job list failed: %v", err)
	}
	if !strings.Contains(listOut, id[:12]) || !strings.Contains(listOut, "cli-no-daemon-1") {
		t.Fatalf("unexpected list stdout:\n%s", listOut)
	}
}
