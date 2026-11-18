package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gateJSON is the actual GraphQL connection shape, not gh pr view's truncated
// reviews array. Empty connections and missing connections are different.
func gateJSON(g *MergeGate) []byte {
	if g == nil {
		return []byte(`{"data":{"viewer":{"login":"owner"},"p0":{"pullRequest":null}}}`)
	}
	threads := make([]map[string]bool, g.OpenThreads)
	for i := range threads {
		threads[i] = map[string]bool{"isResolved": false}
	}
	reviews := g.Reviews
	if reviews == nil {
		reviews = []Review{}
	}
	out, _ := json.Marshal(map[string]any{"data": map[string]any{
		"viewer": map[string]string{"login": g.Viewer},
		"p0": map[string]any{"pullRequest": map[string]any{
			"headRefOid": g.Head, "author": map[string]string{"login": g.Author},
			"reviews":       map[string]any{"pageInfo": map[string]bool{"hasNextPage": g.MoreReviews}, "nodes": reviews},
			"reviewThreads": map[string]any{"pageInfo": map[string]bool{"hasNextPage": g.MoreThreads}, "nodes": threads},
		}},
	}})
	return out
}

func TestMergePolicyMatrix(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*PR)
	}{
		{"no approval on unprotected repo", func(p *PR) { p.ReviewDecision = ""; p.Gate.Reviews = nil }},
		{"stale head approval", func(p *PR) { p.Gate.Reviews[0].Commit.OID = "old" }},
		{"expired approval", func(p *PR) { p.Gate.Reviews[0].SubmittedAt = time.Now().Add(-8 * 24 * time.Hour) }},
		{"future approval", func(p *PR) { p.Gate.Reviews[0].SubmittedAt = time.Now().Add(time.Hour) }},
		{"self approval", func(p *PR) { p.Gate.Reviews[0].Author.Login = "OWNER" }},
		{"reviewer lacks write permission", func(p *PR) { p.Gate.Reviews[0].CanPush = false }},
		{"missing reviewer", func(p *PR) { p.Gate.Reviews[0].Author.Login = "" }},
		{"missing date", func(p *PR) { p.Gate.Reviews[0].SubmittedAt = time.Time{} }},
		{"dismissal supersedes approval", func(p *PR) {
			r := p.Gate.Reviews[0]
			r.State = "DISMISSED"
			r.SubmittedAt = time.Now()
			p.Gate.Reviews = append(p.Gate.Reviews, r)
		}},
		{"change request supersedes approval", func(p *PR) {
			r := p.Gate.Reviews[0]
			r.State = "CHANGES_REQUESTED"
			r.SubmittedAt = time.Now()
			p.Gate.Reviews = append(p.Gate.Reviews, r)
		}},
		{"other reviewer requests changes", func(p *PR) {
			r := p.Gate.Reviews[0]
			r.Author.Login = "other"
			r.State = "CHANGES_REQUESTED"
			p.Gate.Reviews = append(p.Gate.Reviews, r)
		}},
		{"incomplete reviews", func(p *PR) { p.Gate.MoreReviews = true }},
		{"open thread", func(p *PR) { p.Gate.OpenThreads = 1 }},
		{"incomplete threads", func(p *PR) { p.Gate.MoreThreads = true }},
		{"missing gate", func(p *PR) { p.Gate = nil }},
		{"wrong owner", func(p *PR) { p.Gate.Author = "other" }},
		{"missing task permission", func(p *PR) { p.Policy = nil }},
		{"denied task permission", func(p *PR) { p.Policy.Allowed = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, auto := range []bool{false, true} {
				p := greenPR()
				tc.edit(&p)
				if !mergeable(&p, summarizeChecks(p.Checks)) {
					t.Fatal("fixture must remain GitHub mergeable")
				}
				if readyToMerge(&p, summarizeChecks(p.Checks)) {
					t.Fatal("ticker accepted unsafe PR")
				}
				g := &fakeGH{pr: p}
				m := mergeRunner{auto: auto, gh: g.run, policy: func(PR) MergePolicy {
					if p.Policy == nil {
						return MergePolicy{}
					}
					return *p.Policy
				}}
				if err := m.pre(context.Background(), mergeStep("github.pr.merge")); err == nil {
					t.Fatal("executor accepted unsafe PR")
				}
				if _, err := m.do(context.Background(), mergeStep("github.pr.merge"), ""); err == nil || len(g.calls) != 0 {
					t.Fatalf("do wrote: %v %v", err, g.calls)
				}
			}
		})
	}
}

func TestMergeGateFailsClosedOnPartialEvidence(t *testing.T) {
	good := string(gateJSON(greenPR().Gate))
	for name, data := range map[string]string{
		"null threads":      strings.Replace(good, `"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}`, `"reviewThreads":null`, 1),
		"missing threads":   strings.Replace(good, `"reviewThreads":`, `"ignoredThreads":`, 1),
		"null page info":    strings.Replace(good, `"pageInfo":{"hasNextPage":false}`, `"pageInfo":null`, 1),
		"missing page flag": strings.ReplaceAll(good, `"hasNextPage":false`, `"unused":false`),
		"null nodes":        strings.Replace(good, `"nodes":[]`, `"nodes":null`, 1),
		"null thread":       strings.Replace(good, `"nodes":[]`, `"nodes":[null]`, 1),
		"missing resolved":  strings.Replace(good, `"nodes":[]`, `"nodes":[{}]`, 1),
		"missing viewer":    strings.Replace(good, `"viewer":`, `"ignoredViewer":`, 1),
		"partial error":     strings.TrimSuffix(good, "}") + `,"errors":[{"path":["p0","pullRequest","reviews"]}]}`,
		"pathless error":    strings.TrimSuffix(good, "}") + `,"errors":[{"message":"partial"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if data == good {
				t.Fatal("fixture edit did not match")
			}
			gates, err := parseGates([]byte(data), map[string]string{"p0": "url"}, time.Now())
			if err == nil && len(gates) != 0 {
				t.Fatalf("partial evidence accepted: %+v", gates)
			}
		})
	}
}

func TestMergeTaskPolicy(t *testing.T) {
	cfg := Config{BranchPrefix: "tj/"}
	b := bead(StatusInProgress)
	p := *readyPR()
	p.URL = "https://github.com/o/r/pull/7"
	if !mergeTaskPolicy(cfg, b, p, true).Allowed {
		t.Fatal("own branch not associated")
	}
	p.Head = "unrelated"
	if mergeTaskPolicy(cfg, b, p, true).Allowed {
		t.Fatal("unrelated task accepted")
	}
	b.Notes = "PR: " + p.URL
	if !mergeTaskPolicy(cfg, b, p, true).Allowed {
		t.Fatal("noted PR not associated")
	}
	if mergeTaskPolicy(cfg, b, p, false).Allowed {
		t.Fatal("no authorization accepted")
	}
	for _, status := range []string{StatusNeedsMe, StatusBlocked, StatusClosed, StatusOpen} {
		b.Status = status
		if mergeTaskPolicy(cfg, b, p, true).Allowed {
			t.Fatalf("held task %s accepted", status)
		}
	}
}

func TestMergeRunnerHeadMovementAndErrors(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprint(auto), func(t *testing.T) {
			g := &fakeGH{pr: greenPR()}
			m := mergeRunner{auto: auto, gh: g.run, policy: func(PR) MergePolicy { return MergePolicy{Allowed: true} }}
			s := mergeStep("github.pr.merge")
			if err := m.pre(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			g.pr.Gate.Head = strings.Repeat("f", 40)
			if _, err := m.do(context.Background(), s, ""); !errors.Is(err, errStale) || len(g.calls) != 0 {
				t.Fatalf("moved gate: %v %v", err, g.calls)
			}
			g.pr = greenPR()
			g.err = errors.New("unsupported or unavailable API")
			if _, err := m.do(context.Background(), s, ""); err == nil || len(g.calls) != 0 {
				t.Fatalf("API error: %v %v", err, g.calls)
			}
		})
	}
}

func TestExecutorEnforcesMergeTaskPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*execFixture, *fakeGH)
		pass bool
	}{
		{"authorized", func(*execFixture, *fakeGH) {}, true},
		{"held", func(f *execFixture, _ *fakeGH) { f.bead.Status = StatusNeedsMe }, false},
		{"blocked", func(f *execFixture, _ *fakeGH) { f.bead.Status = StatusBlocked }, false},
		{"unrelated", func(f *execFixture, _ *fakeGH) { f.bead.Notes = "" }, false},
		{"dial lowered", func(f *execFixture, _ *fakeGH) { f.bead.Labels = []string{"autonomy:3"} }, false},
		{"another author", func(_ *execFixture, g *fakeGH) { g.pr.Gate.Author = "other" }, false},
		{"unprotected unapproved", func(_ *execFixture, g *fakeGH) { g.pr.ReviewDecision = ""; g.pr.Gate.Reviews = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecFixture(t)
			g := &fakeGH{pr: greenPR()}
			f.bead.Notes = "PR: https://github.com/acme/app/pull/55"
			f.x.runners["github.pr.merge"] = mergeRunner{gh: g.run}
			f.check(mergePlan("b-1"))
			tc.edit(f, g)
			kind := f.runPlan()
			if (kind == planDone) != tc.pass || (len(g.calls) == 1) != tc.pass {
				t.Fatalf("kind %s, mutations %v", kind, g.calls)
			}
		})
	}
}

func TestExplicitDigestApprovalResumesOnlyOlderNeedsMe(t *testing.T) {
	for _, tc := range []struct {
		status string
		newer  bool
		pass   bool
	}{
		{StatusNeedsMe, false, true}, {StatusNeedsMe, true, false}, {StatusBlocked, false, false}, {StatusClosed, false, false},
	} {
		t.Run(fmt.Sprintf("%s-newer=%v", tc.status, tc.newer), func(t *testing.T) {
			f := newExecFixture(t)
			g := &fakeGH{pr: greenPR()}
			f.bead.Notes = "PR: https://github.com/acme/app/pull/55"
			f.x.runners["github.pr.merge"] = mergeRunner{gh: g.run}
			f.check(mergePlan("b-1"))
			now := time.Now()
			f.bead.Status, f.bead.UpdatedAt = tc.status, now.Add(-time.Minute)
			f.bead.Labels = []string{"autonomy:3"}
			if tc.newer {
				f.bead.UpdatedAt = now.Add(time.Minute)
			}
			if err := appendPlanEvent(f.digest, planEvent{At: now, Kind: planApproved, By: "TJ", Expires: now.Add(time.Hour), Pass: true}); err != nil {
				t.Fatal(err)
			}
			kind := f.runPlan()
			if (kind == planDone) != tc.pass || (len(g.calls) == 1) != tc.pass {
				t.Fatalf("kind %s, mutations %v", kind, g.calls)
			}
		})
	}
}

func TestMergeObservationSurvivesOrdinaryTickerReloads(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	b := bead(StatusInProgress)
	b.Labels = []string{"autonomy:4"}
	p := greenPR()
	p.URL = "https://github.com/o/r/pull/7"
	st := TickerState{PRs: map[string]PR{b.ID: p}, LastGH: time.Now()}
	roundTrip := func() TickerState {
		data, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var saved TickerState
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	th := threadFor(cfg, roundTrip(), b, nil)
	if readyKey(th) == "" || classify(th) != GroupReady {
		t.Fatalf("saved successful observation lost readiness: %v", mergeBlockers(th.PR))
	}
	prev := Snapshot{Group: GroupReview, PRNumber: 7, PRState: "OPEN", ReviewsSplit: true, Reviews: 1}
	o := transition(th, prev, false, time.Now())
	if len(o.Prompts) != 1 {
		t.Fatalf("ready prompt missing: %+v", o)
	}
	next := threadFor(cfg, roundTrip(), b, nil)
	if got := freshPending(next, o.Prompts); len(got) != 1 {
		t.Fatalf("busy-worker ready prompt lost between polls: %v", got)
	}
	p.AutoMerge = &AutoMerge{Method: "SQUASH"}
	st.PRs[b.ID] = p
	next = threadFor(cfg, roundTrip(), b, nil)
	if unsafeAutoKey(next) != "" {
		t.Fatal("healthy observation oscillated to unsafe after reload")
	}
	p.Gate.At = time.Now().Add(-3 * cfg.ghEvery())
	st.PRs[b.ID] = p
	next = threadFor(cfg, roundTrip(), b, nil)
	if readyKey(next) != "" || next.PR.Gate != nil {
		t.Fatal("expired observation still cleared to merge")
	}
}

func TestMergeShimUnavailableAndAdmin(t *testing.T) {
	for _, sock := range []string{"", "/unavailable.sock"} {
		for _, argv := range [][]string{
			{"gh", "pr", "merge", "55", "--auto", "--squash"},
			{"gh", "pr", "merge", "55", "--squash"},
			{"gh", "api", "-X", "PUT", "repos/acme/app/pulls/55/merge"},
		} {
			f := newRunFixture(t, sock)
			f.callErr = syscall.ECONNREFUSED
			if err := runCmd(argv, f.r); err == nil || len(f.replaced) != 0 {
				t.Fatalf("escaped policy: %v %v", err, f.replaced)
			}
		}
	}
	f := newRunFixture(t, "")
	if err := runCmd([]string{"gh", "pr", "merge", "55", "--disable-auto"}, f.r); err != nil || len(f.replaced) == 0 {
		t.Fatalf("cannot disarm without executor: %v", err)
	}
	g := &fakeGH{pr: greenPR()}
	m := mergeRunner{gh: g.run}
	s := mergeStep("github.pr.merge")
	s.Args["method"] = "admin"
	if err := m.pre(context.Background(), s); !errors.Is(err, errUnmet) {
		t.Fatalf("method admin: %v", err)
	}
	l, _ := fakeLookup()
	s, _, _ = opaqueGH([]string{"pr", "merge", "55", "--admin", "--squash"}, l, "test")
	o := opaqueRunner{run: g.run}
	if _, err := o.do(context.Background(), s, ""); !errors.Is(err, errUnmet) || len(g.calls) != 0 {
		t.Fatalf("opaque admin: %v %v", err, g.calls)
	}
}

func TestUnfreshDetailsCannotRegainGate(t *testing.T) {
	p := *readyPR()
	p.DetailsFresh = false
	if readyToMerge(&p, summarizeChecks(p.Checks)) {
		t.Fatal("persisted details authorized merge")
	}
	prs := map[string]PR{"task": p}
	applyGates(prs, map[string]MergeGate{p.URL: *p.Gate})
	if prs["task"].Gate != nil {
		t.Fatal("refreshed a stale detail from another task sharing the URL")
	}
}

func TestMergeApprovalCommentAndBot(t *testing.T) {
	p := greenPR()
	p.ReviewDecision = "" // no GitHub review requirement, independent floor still met
	p.Gate.Reviews[0].Author.Login = "review-app[bot]"
	r := p.Gate.Reviews[0]
	r.State, r.SubmittedAt = "COMMENTED", time.Now()
	p.Gate.Reviews = append(p.Gate.Reviews, r)
	if !readyToMerge(&p, summarizeChecks(p.Checks)) {
		t.Fatalf("valid independent bot approval rejected: %v", mergeBlockers(&p))
	}
}
