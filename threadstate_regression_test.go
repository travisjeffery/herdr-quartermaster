package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDependenciesDoNotReclassifyActiveThreads(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	for _, status := range []string{StatusOpen, StatusInProgress} {
		for _, agentStatus := range []string{"", "idle"} {
			b := Bead{ID: "task-1", Status: status, Dependencies: []Dependency{{DependsOn: "task-2", Type: "blocks"}}}
			blocker := Bead{ID: "task-2", Status: StatusOpen}
			var agents []Agent
			byName := map[string]Agent{}
			st := TickerState{Threads: map[string]Snapshot{}}
			want := GroupNoAgent
			if agentStatus != "" {
				a := Agent{Name: b.ID, Status: agentStatus}
				agents, byName[b.ID] = []Agent{a}, a
				st.Threads[b.ID] = Snapshot{AgentStatus: agentStatus}
				want = GroupIdle
			}
			if status == StatusOpen {
				want = GroupBlocked
			}
			rows := buildRows(Config{}, boardData{Beads: []Bead{b, blocker}, Agents: agents, State: st}, sortPriority)
			for _, r := range rows {
				if r.bead.ID == b.ID && r.group != want {
					t.Fatalf("%s/%s board = %s, want %s", status, agentStatus, r.group, want)
				}
			}
			d, _ := buildDigest(digestInputs{open: []Bead{b, blocker}, agents: byName, ticker: st}, digestState{}, nil, t0)
			if it := item(t, d, b.ID); it.State.group() != want || len(it.Blockers) != 1 {
				t.Fatalf("digest = %+v, want %s with dependency", it, want)
			}
			if status == StatusInProgress {
				th := threadFor(Config{}, st, b, byName)
				if classify(th) != want {
					t.Fatalf("ticker/context = %s, want %s", classify(th), want)
				}
				r := buildReport(t0, nil, []Bead{b, blocker}, nil, st, "")
				if len(r.InFlight) != 1 || r.InFlight[0].Derived.group() != want {
					t.Fatalf("report = %+v, want %s", r.InFlight, want)
				}
			}
		}
	}
}

func TestLightNotedPRCannotEstablishReviewBaseline(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	b := Bead{ID: "task-1", Status: StatusInProgress, Notes: "PR: https://github.com/example/app/pull/7"}
	light := PR{Number: 7, URL: "https://github.com/example/app/pull/7", State: "OPEN"}
	st := TickerState{Noted: map[string]PR{light.URL: light}}
	agents := map[string]Agent{b.ID: {Name: b.ID, Status: "idle", Seq: 1}}
	// The ticker deliberately omitted PRs[id] when the first detail fetch failed.
	th := threadFor(Config{}, st, b, agents)
	if th.PR != nil {
		t.Fatalf("light lookup promoted after failed detail fetch: %+v", th.PR)
	}
	prev := snapshot(th, Snapshot{}, t0)
	full := light
	full.Reviews = []Review{approvalOf("head", t0)}
	full.Reviews[0].Author.Login = "reviewer"
	st.PRs = map[string]PR{b.ID: full}
	th = threadFor(Config{}, st, b, agents)
	out := transition(th, prev, false, t0)
	for _, e := range out.Events {
		if e.Kind == EventReview {
			t.Fatalf("existing feedback announced as new: %+v", out)
		}
	}
}

func TestUnrelatedNoteDoesNotRenotifyWaitingThread(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = "Question: which option?"
	th := thread(b, agent("idle", 1), nil)
	prev := snapshot(th, Snapshot{}, t0)
	for _, note := range []string{"coordinator checked the worktree", "AUTO: [worker L4 C2] verified tests"} {
		th.Bead.Notes += "\n" + note
		if out := transition(th, prev, false, t0); out.Notify || len(out.Events) != 0 {
			t.Fatalf("unrelated note re-notified: %+v", out)
		}
		prev = snapshot(th, prev, t0)
	}
}

func TestDigestShowsCachedPRWhenLookupHasNoObservation(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
		b := Bead{ID: "task-1", Status: StatusInProgress}
		p := PR{Number: 7, URL: "https://github.com/example/app/pull/7", State: state}
		st := TickerState{PRs: map[string]PR{b.ID: p}}
		d, next := buildDigest(digestInputs{open: []Bead{b}, ticker: st, branch: map[string]string{b.ID: p.URL}}, digestState{}, nil, t0)
		it := item(t, d, b.ID)
		if it.PR == nil || it.PR.URL != p.URL || it.State.PR.URL != p.URL || next.PRs[p.URL].State != state {
			t.Fatalf("fallback not shown/remembered: %+v / %+v", it, next.PRs)
		}
		if !strings.Contains(itemDetail(it), "example/app#7") {
			t.Fatalf("PR missing from detail: %s", itemDetail(it))
		}
	}
}

func TestPromptKeepsPendingCommandVisible(t *testing.T) {
	b := bead(StatusNeedsMe)
	b.Notes = "RUN: login"
	th := thread(b, agent("blocked", 1), nil)
	if s := th.derived(); s.Waiting != WaitPrompt || s.Command != "login" || stateLine(th) != "needs you · run command" {
		t.Fatalf("prompt hid pending command: %+v", s)
	}
}

func TestExplicitBlockedOutranksPRAndWorkingAgent(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cases := []struct {
		name  string
		agent *Agent
		pr    *PR
	}{
		{"open PR with idle agent", agent("idle", 1), openPR()},
		{"ready PR with idle agent", agent("idle", 1), readyPR()},
		{"failing PR with idle agent", agent("idle", 1), openPR("build")},
		{"open PR with working agent", agent("working", 1), openPR()},
		{"working agent without PR", agent("working", 1), nil},
		{"merged PR", agent("idle", 1), &PR{Number: 7, State: "MERGED"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := Bead{ID: "task-held", Status: StatusBlocked, UpdatedAt: t0, Notes: "BLOCKED until soak ends."}
			if tc.pr != nil {
				tc.pr.Number = 7
				tc.pr.URL = "https://github.com/example/app/pull/7"
				b.Notes = "PR: " + tc.pr.URL + "\n" + b.Notes
			}
			if tc.agent != nil {
				tc.agent.Name = b.ID
			}
			th := thread(b, tc.agent, tc.pr).derive()
			if s := th.derived(); s.Kind != ThreadBlocked || classify(th) != GroupBlocked || sectionFor(s) != SecBlocked {
				t.Fatalf("explicit blocked misclassified: state=%+v group=%s", s, classify(th))
			}
			in := digestInputs{open: []Bead{b}, agents: map[string]Agent{}, branch: map[string]string{}, prs: map[string]DigestPR{}}
			st := TickerState{PRs: map[string]PR{}, Threads: map[string]Snapshot{}}
			if tc.agent != nil {
				in.agents[b.ID] = *tc.agent
				st.Threads[b.ID] = Snapshot{AgentStatus: tc.agent.Status, AgentSeq: tc.agent.Seq}
			}
			if tc.pr != nil {
				st.PRs[b.ID] = *tc.pr
				in.branch[b.ID] = tc.pr.URL
				in.prs[tc.pr.URL] = digestObservation(*tc.pr)
			}
			in.ticker = st
			d, _ := buildDigest(in, digestState{}, nil, t0)
			it := item(t, d, b.ID)
			if it.Section != SecBlocked {
				t.Fatalf("digest section = %s, want %s", it.Section, SecBlocked)
			}
			detail := itemDetail(it)
			if !strings.Contains(detail, "BLOCKED until soak ends.") {
				t.Fatalf("digest detail hid hold note: %q", detail)
			}
			if tc.pr != nil && !strings.Contains(detail, "example/app#7") {
				t.Fatalf("digest detail hid PR: %q", detail)
			}
		})
	}
}

func TestIgnoreChecksFailClosedRegressions(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.IgnoreChecks = []string{"* (report-only)", "advisory/*"}
	now := time.Now()
	b := Bead{ID: "task-1", Status: StatusInProgress, Notes: "PR: https://github.com/example/app/pull/7"}
	agents := map[string]Agent{b.ID: {Name: b.ID, Status: "idle", Seq: 1}}

	basePR := func(checks ...Check) PR {
		return PR{
			Number:         7,
			URL:            "https://github.com/example/app/pull/7",
			State:          "OPEN",
			HeadSHA:        "sha1",
			MergeState:     "UNSTABLE",
			Mergeable:      "MERGEABLE",
			ReviewDecision: "REVIEW_REQUIRED",
			DetailsFresh:   true,
			Gate:           &MergeGate{Head: "sha1", Author: "owner", Viewer: "owner", OpenThreads: 0, At: now},
			Checks:         checks,
		}
	}

	t.Run("zero remaining non-ignored checks never qualifies as green or ready_for_review", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			check Check
		}{
			{"only ignored failing check", Check{Name: "scan (report-only)", Status: "COMPLETED", Conclusion: "FAILURE"}},
			{"only ignored pending check", Check{Name: "scan (report-only)", Status: "IN_PROGRESS"}},
			{"only ignored passing check", Check{Name: "scan (report-only)", Status: "COMPLETED", Conclusion: "SUCCESS"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st := TickerState{PRs: map[string]PR{b.ID: basePR(tc.check)}}
				th := threadForAt(cfg, st, b, agents, now)
				if th.Checks.Passing != 0 || th.Checks.Pending != 0 || len(th.Checks.Failing) != 0 {
					t.Fatalf("unexpected remaining checks: %+v", th.Checks)
				}
				if readyForReview(th.PR, th.Checks) || reviewGateCandidate(th.PR, th.Checks) {
					t.Fatalf("PR with zero remaining checks marked ready_for_review: %+v", th.Checks)
				}
				if got := greenKey(th); got != "" {
					t.Fatalf("PR with zero remaining checks marked green: %q", got)
				}
			})
		}
	})

	t.Run("mixed real and ignored failure blocks readiness and emits checks_failing for real check only", func(t *testing.T) {
		pr := basePR(
			Check{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
			Check{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
			Check{Name: "scan (report-only)", Status: "COMPLETED", Conclusion: "FAILURE"},
		)
		st := TickerState{PRs: map[string]PR{b.ID: pr}}
		th := threadForAt(cfg, st, b, agents, now)
		if classify(th) != GroupFailing {
			t.Fatalf("group = %s, want GroupFailing", classify(th))
		}
		if !slices.Equal(th.Checks.Failing, []string{"lint"}) || !slices.Equal(th.Checks.IgnoredFailing, []string{"scan (report-only)"}) {
			t.Fatalf("checks = %+v", th.Checks)
		}
		if readyForReview(th.PR, th.Checks) || greenKey(th) != "" {
			t.Fatal("non-ignored failure did not block readiness")
		}
		prev := Snapshot{Group: GroupReview, PRNumber: 7, PRState: "OPEN", ReviewReadyTracked: true}
		out := transition(th, prev, false, now)
		var failingEvs []Event
		for _, e := range out.Events {
			if e.Kind == EventReadyForReview {
				t.Fatalf("unexpected ready_for_review on mixed failure: %+v", out.Events)
			}
			if e.Kind == EventFailing {
				failingEvs = append(failingEvs, e)
			}
		}
		if len(failingEvs) != 1 || !strings.Contains(failingEvs[0].Summary, "lint") || strings.Contains(failingEvs[0].Summary, "report-only") {
			t.Fatalf("unexpected failing events: %+v", failingEvs)
		}
	})

	t.Run("non-ignored pending check still blocks even when advisory check is ignored", func(t *testing.T) {
		pr := basePR(
			Check{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
			Check{Name: "e2e", Status: "IN_PROGRESS"},
			Check{Name: "scan (report-only)", Status: "COMPLETED", Conclusion: "FAILURE"},
		)
		st := TickerState{PRs: map[string]PR{b.ID: pr}}
		th := threadForAt(cfg, st, b, agents, now)
		if th.Checks.Pending != 1 || len(th.Checks.Failing) != 0 {
			t.Fatalf("checks = %+v", th.Checks)
		}
		if readyForReview(th.PR, th.Checks) || greenKey(th) != "" || mergeable(th.PR, th.Checks) {
			t.Fatal("non-ignored pending check failed to block readiness")
		}
	})

	t.Run("ignored pending check does not block readiness when non-ignored checks pass", func(t *testing.T) {
		pr := basePR(
			Check{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
			Check{Name: "advisory/slow", Status: "IN_PROGRESS"},
		)
		st := TickerState{PRs: map[string]PR{b.ID: pr}}
		th := threadForAt(cfg, st, b, agents, now)
		if th.Checks.Pending != 0 || th.Checks.IgnoredPending != 1 || th.Checks.Passing != 1 {
			t.Fatalf("checks = %+v", th.Checks)
		}
		if !readyForReview(th.PR, th.Checks) || greenKey(th) == "" {
			t.Fatalf("ignored pending check blocked readiness: %+v", th.Checks)
		}
	})

	t.Run("ci rerun inspection skips ignored advisory failures across runs and same run", func(t *testing.T) {
		pr := basePR(
			Check{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE", DetailsURL: "https://github.com/acme/app/actions/runs/42/job/101"},
			Check{Name: "Scan (report-only)", Status: "COMPLETED", Conclusion: "FAILURE", DetailsURL: "https://github.com/acme/app/actions/runs/42/job/102"},
			Check{Context: "advisory/external", State: "FAILURE", TargetURL: "https://status.example.com/check/9"},
		)
		patterns, err := cfg.CIRerun.compilePatterns()
		if err != nil {
			t.Fatal(err)
		}
		env := ciRerunEnv{
			runDetail: func(repo, runID string) (RunDetail, error) {
				return RunDetail{
					ID:         42,
					Status:     "completed",
					Conclusion: "failure",
					Jobs: []RunJob{
						{ID: 101, Name: "test", Status: "completed", Conclusion: "failure"},
						{ID: 102, Name: "Scan (report-only)", Status: "completed", Conclusion: "failure"},
					},
				}, nil
			},
			annotations: func(repo string, jobID int64) ([]CheckAnnotation, error) {
				if jobID == 102 {
					return []CheckAnnotation{{Message: "advisory lint violation"}}, nil
				}
				return []CheckAnnotation{{Message: "The self-hosted runner lost communication with the server."}}, nil
			},
		}
		analysis := inspectCIRerunFailureWith(&pr, patterns, cfg.IgnoreChecks, env)
		if !analysis.AllFlake {
			t.Fatalf("ignored advisory failure poisoned flake analysis: %+v", analysis)
		}
		if fpIgnored, fpOnlyReal := ciFailureFingerprintWith(&pr, cfg.IgnoreChecks), ciFailureFingerprint(&PR{Checks: pr.Checks[:1]}); fpIgnored != fpOnlyReal {
			t.Fatalf("fingerprint included ignored check: %q vs %q", fpIgnored, fpOnlyReal)
		}
	})
}
