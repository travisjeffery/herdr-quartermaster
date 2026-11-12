package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeLookup resolves every PR to acme/app with head sha, and the current
// repository to acme/app.
func fakeLookup() (ghLookup, *[]string) {
	var asked []string
	return ghLookup{
		pr: func(sel, repo string) (ghPR, error) {
			asked = append(asked, sel+"|"+repo)
			if sel == "missing" {
				return ghPR{}, errors.New("no pull requests found")
			}
			n := 55
			if m := prURLRE.FindStringSubmatch(sel); m != nil {
				fmt.Sscan(m[3], &n)
			}
			return ghPR{Owner: "acme", Repo: "app", Number: n, Head: sha}, nil
		},
		repo:     func() (string, error) { return "acme/app", nil },
		readFile: func(p string) ([]byte, error) { return []byte("from " + p), nil },
		stdin:    strings.NewReader("from stdin"),
		dir:      "/work",
	}, &asked
}

func TestLowerGH(t *testing.T) {
	other := strings.Repeat("f", 40)
	cases := []struct {
		argv    string
		op      string // "" passes through
		target  string
		head    string
		args    map[string]string
		wantErr string
	}{
		{argv: "pr merge 55 --squash --match-head-commit " + other, op: "github.pr.merge", target: "github:acme/app#55", head: other, args: map[string]string{"method": "squash"}},
		{argv: "pr merge https://github.com/acme/app/pull/7 --auto --squash", op: "github.pr.automerge", target: "github:acme/app#7", head: sha, args: map[string]string{"method": "squash"}},
		{argv: "pr merge --merge --match-head-commit=0123456 -d --subject=Ship -R acme/app", op: "github.pr.merge", target: "github:acme/app#55", head: sha, args: map[string]string{"method": "merge", "delete_branch": "true", "subject": "Ship"}},
		{argv: "pr merge 55 -s --body-file -", op: "github.pr.merge", head: sha, args: map[string]string{"method": "squash", "body": "from stdin"}},
		{argv: "pr merge 55 -s -F msg.txt", op: "github.pr.merge", head: sha, args: map[string]string{"method": "squash", "body": "from /work/msg.txt"}},
		{argv: "pr merge 55 --squash --admin", op: opOpaque},
		{argv: "pr merge 55 56 --squash", op: opOpaque},
		{argv: "pr merge 55 --disable-auto"},
		{argv: "pr merge 55 --squash --rebase", wantErr: "only one of"},
		{argv: "pr merge missing --squash", wantErr: "which pull request"},
		{argv: "pr view 55"},
		{argv: "pr create --fill"},
		{argv: "issue comment 3 --body hi"},
		{argv: "api -X PUT repos/acme/app/pulls/9/merge -f sha=" + other + " -f merge_method=squash -f commit_title=T", op: "github.pr.merge", target: "github:acme/app#9", head: other, args: map[string]string{"method": "squash", "subject": "T"}},
		{argv: "api --method=put /repos/{owner}/{repo}/pulls/9/merge", op: "github.pr.merge", target: "github:acme/app#9", head: sha},
		{argv: "api -X PUT repos/acme/app/pulls/9/merge --input body.json", op: opOpaque},
		{argv: "api -X PUT repos/acme/app/pulls/9/merge -F sha=@sha.txt", op: opOpaque},
		{argv: "api -X PUT repos/acme/app/pulls/9/merge -f admin=true", op: opOpaque},
		{argv: "api -XPUT repos/acme/app/pulls/9/merge -fmerge_method=rebase", op: "github.pr.merge", target: "github:acme/app#9", head: sha, args: map[string]string{"method": "rebase"}},
		{argv: "api repos/acme/app/pulls/9/merge"},
		{argv: "api repos/acme/app/pulls/9"},
		{argv: "api -X PATCH repos/acme/app/pulls/9 -f title=x"},
		{argv: "api graphql -f query=mutation{mergePullRequest(input:{pullRequestId:\"x\"}){clientMutationId}}", op: opOpaque},
		{argv: "api graphql -f query={viewer{login}}"},
	}
	for _, c := range cases {
		t.Run(c.argv, func(t *testing.T) {
			l, _ := fakeLookup()
			args := strings.Fields(c.argv)
			s, ok, err := lowerGH(args, l)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.op == "" {
				if ok {
					t.Fatalf("lowered to %+v; want it passed through", s)
				}
				return
			}
			if !ok || s.Op != c.op {
				t.Fatalf("lowered to %v %+v, want %s", ok, s, c.op)
			}
			if c.op == opOpaque {
				var argv []string
				if err := json.Unmarshal([]byte(s.Args["argv"]), &argv); err != nil || strings.Join(argv, " ") != c.argv || s.Args["tool"] != "gh" || s.Args["dir"] != "/work" {
					t.Errorf("opaque args %v", s.Args)
				}
				if !strings.HasPrefix(s.Text, "gh ") {
					t.Errorf("text %q", s.Text)
				}
				p := Plan{Bead: "b-1", Steps: []Step{s}}
				if _, err := parsePlan(mustJSON(t, p)); err != nil {
					t.Errorf("an opaque step doesn't parse: %v", err)
				}
				return
			}
			if c.target != "" && s.Target != c.target {
				t.Errorf("target %s, want %s", s.Target, c.target)
			}
			if s.Pre["head_sha"] != c.head {
				t.Errorf("head %s, want %s", s.Pre["head_sha"], c.head)
			}
			if fmt.Sprint(s.Args) != fmt.Sprint(c.args) && !(len(s.Args) == 0 && len(c.args) == 0) {
				t.Errorf("args %v, want %v", s.Args, c.args)
			}
			// What the shim lowers is a plan the executor takes.
			if _, err := parsePlan(mustJSON(t, Plan{Bead: "b-1", Steps: []Step{s}})); err != nil {
				t.Errorf("lowered step doesn't parse: %v", err)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeTool puts an executable tool in a fresh directory and returns it.
func fakeTool(t *testing.T, tool, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestShimsAndRealBinary(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	shims, err := writeShims("/opt/qm's/quartermaster")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(shims, "gh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `exec '/opt/qm'\''s/quartermaster' run -- gh "$@"`) {
		t.Errorf("shim:\n%s", b)
	}
	if fi, _ := os.Stat(filepath.Join(shims, "gh")); fi.Mode()&0o111 == 0 {
		t.Error("the shim isn't executable")
	}
	// A copy of the shim elsewhere on PATH is skipped too.
	copied := t.TempDir()
	if err := os.WriteFile(filepath.Join(copied, "gh"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	real := fakeTool(t, "gh", "echo real")
	t.Setenv("PATH", strings.Join([]string{shims, copied, real}, string(os.PathListSeparator)))
	if got, err := realBinary("gh"); err != nil || got != filepath.Join(real, "gh") {
		t.Errorf("realBinary = %s, %v", got, err)
	}
	t.Setenv("PATH", shims)
	if _, err := realBinary("gh"); err == nil {
		t.Error("found the shim as the real gh")
	}
}

type runFixture struct {
	r        shimRun
	replaced []string
	sent     []Plan
	resp     execResponse
	callErr  error
	out, err strings.Builder
}

func newRunFixture(t *testing.T, sock string) *runFixture {
	t.Helper()
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("PATH", fakeTool(t, "gh", "exit 0"))
	f := &runFixture{resp: execResponse{State: planDone, Digest: "abc", Output: "step 1: merged\ndone 10:00\n"}}
	f.r = shimRun{
		lookup:  func(string) ghLookup { l, _ := fakeLookup(); return l },
		replace: func(path string, argv []string) error { f.replaced = append([]string{path}, argv...); return nil },
		call: func(sock string, req execRequest) (execResponse, error) {
			var p Plan
			if err := json.Unmarshal(req.Run, &p); err != nil {
				t.Fatal(err)
			}
			f.sent = append(f.sent, p)
			return f.resp, f.callErr
		},
		getenv: func(k string) string {
			switch k {
			case "QUARTERMASTER_EXEC_SOCKET":
				return sock
			case "HERDR_PANE_ID":
				return "w1:p1"
			}
			return ""
		},
		dir:    "/work",
		stdout: &f.out,
		stderr: &f.err,
	}
	return f
}

func TestRunCmd(t *testing.T) {
	merge := []string{"--", "gh", "pr", "merge", "55", "--squash"}

	// No socket: not a routed worker, so the real gh runs.
	f := newRunFixture(t, "")
	if err := runCmd(merge, f.r); err != nil || len(f.sent) != 0 || len(f.replaced) == 0 || !strings.HasSuffix(f.replaced[0], "/gh") || strings.Join(f.replaced[1:], " ") != "gh pr merge 55 --squash" {
		t.Errorf("no socket: err %v, sent %d, replaced %v", err, len(f.sent), f.replaced)
	}

	// A subcommand outside the table runs the real gh.
	f = newRunFixture(t, "/run/b-1.sock")
	if err := runCmd([]string{"gh", "pr", "view", "55"}, f.r); err != nil || len(f.sent) != 0 || len(f.replaced) == 0 {
		t.Errorf("pr view: err %v, sent %d, replaced %v", err, len(f.sent), f.replaced)
	}

	// A merge goes to the executor as a one-step plan.
	f = newRunFixture(t, "/run/b-1.sock")
	if err := runCmd(merge, f.r); err != nil {
		t.Fatal(err)
	}
	if len(f.replaced) != 0 || len(f.sent) != 1 {
		t.Fatalf("replaced %v, sent %d", f.replaced, len(f.sent))
	}
	p := f.sent[0]
	if p.Source != "shim" || p.Actor != "worker w1:p1" || p.Bead != "" || len(p.Steps) != 1 || p.Steps[0].Op != "github.pr.merge" || p.Steps[0].Pre["head_sha"] != sha {
		t.Errorf("plan %+v", p)
	}
	if !strings.Contains(f.out.String(), "step 1: merged") {
		t.Errorf("output %q", f.out.String())
	}

	// Waiting for TJ: nonzero, naming the digest.
	f = newRunFixture(t, "/run/b-1.sock")
	f.resp = execResponse{State: "waiting", Digest: "d1g", Why: "not autonomous: class 9"}
	if err := runCmd(merge, f.r); err == nil || !strings.Contains(f.out.String(), "quartermaster plan status d1g") {
		t.Errorf("waiting: err %v, out %q", err, f.out.String())
	}

	// No executor on the socket: advisory, so the real gh runs, with a warning.
	f = newRunFixture(t, "/run/b-1.sock")
	f.callErr = fmt.Errorf("dial: %w", syscall.ECONNREFUSED)
	if err := runCmd(merge, f.r); err != nil || len(f.replaced) == 0 || !strings.Contains(f.err.String(), "no executor") {
		t.Errorf("no executor: err %v, replaced %v, stderr %q", err, f.replaced, f.err.String())
	}

	// A lowering error stops before anything runs.
	f = newRunFixture(t, "/run/b-1.sock")
	if err := runCmd([]string{"gh", "pr", "merge", "missing", "--squash"}, f.r); err == nil || len(f.replaced)+len(f.sent) != 0 {
		t.Errorf("unresolvable PR: err %v", err)
	}
}

// fakeGH answers gh pr view with pr and records every other call.
type fakeGH struct {
	pr    PR
	calls [][]string
	err   error
}

func (g *fakeGH) run(_ context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) > 1 && args[0] == "pr" && args[1] == "view" {
		return json.Marshal(g.pr)
	}
	g.calls = append(g.calls, append([]string{"dir=" + dir}, args...))
	return []byte("ok"), g.err
}

func mergeStep(op string) Step {
	return Step{Op: op, Target: "github:acme/app#55", Pre: map[string]string{"head_sha": sha}, Args: map[string]string{"method": "squash", "subject": "Ship it"}}
}

func greenPR() PR {
	return PR{State: "OPEN", HeadSHA: sha, ReviewDecision: "APPROVED", MergeState: "CLEAN", Checks: []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}
}

func TestMergeRunner(t *testing.T) {
	ctx := context.Background()
	g := &fakeGH{pr: greenPR()}
	m := mergeRunner{gh: g.run}
	s := mergeStep("github.pr.merge")
	if err := m.pre(ctx, s); err != nil {
		t.Fatalf("green PR: %v", err)
	}
	ev, err := m.do(ctx, s, "qm:x:1")
	if err != nil {
		t.Fatal(err)
	}
	want := "dir= pr merge https://github.com/acme/app/pull/55 --match-head-commit " + sha + " --squash --subject Ship it"
	if len(g.calls) != 1 || strings.Join(g.calls[0], " ") != want {
		t.Errorf("merged with %v\nwant %s", g.calls, want)
	}
	if !strings.Contains(ev, "merged https://github.com/acme/app/pull/55 at 0123456789ab") {
		t.Errorf("evidence %q", ev)
	}

	// The executor reads review and checks itself.
	for name, mutate := range map[string]func(*PR){
		"pending":  func(p *PR) { p.Checks = append(p.Checks, Check{Name: "slow", Status: "IN_PROGRESS"}) },
		"review":   func(p *PR) { p.ReviewDecision = "REVIEW_REQUIRED"; p.MergeState = "BLOCKED" },
		"failing":  func(p *PR) { p.Checks[0].Conclusion = "FAILURE"; p.MergeState = "BLOCKED" },
		"draft":    func(p *PR) { p.IsDraft = true },
		"conflict": func(p *PR) { p.MergeState = "DIRTY" },
	} {
		g.pr = greenPR()
		mutate(&g.pr)
		if err := m.pre(ctx, s); !errors.Is(err, errUnmet) {
			t.Errorf("%s: pre = %v, want errUnmet", name, err)
		}
	}
	// A moved head or a closed PR is stale.
	g.pr = greenPR()
	g.pr.HeadSHA = strings.Repeat("e", 40)
	if err := m.pre(ctx, s); !errors.Is(err, errStale) {
		t.Errorf("moved head: %v", err)
	}
	g.pr = greenPR()
	g.pr.State = "CLOSED"
	if err := m.pre(ctx, s); !errors.Is(err, errStale) {
		t.Errorf("closed: %v", err)
	}

	// Probe: merged at the head happened; merged at another head didn't.
	g.pr = greenPR()
	g.pr.State = "MERGED"
	if happened, known, _, err := m.probe(ctx, s, ""); !happened || !known || err != nil {
		t.Errorf("probe merged: %v %v %v", happened, known, err)
	}
	g.calls = nil
	if ev, err := m.do(ctx, s, ""); err != nil || len(g.calls) != 0 || !strings.HasSuffix(ev, "already") {
		t.Errorf("do on a merged PR: %q %v, calls %v", ev, err, g.calls)
	}
	g.pr.HeadSHA = strings.Repeat("e", 40)
	if happened, known, _, _ := m.probe(ctx, s, ""); happened || !known {
		t.Error("merged at another head counted")
	}

	// Auto-merge: no review or checks needed yet; on at the head is done.
	a := mergeRunner{auto: true, gh: g.run}
	as := mergeStep("github.pr.automerge")
	g.pr = greenPR()
	g.pr.ReviewDecision, g.pr.MergeState = "REVIEW_REQUIRED", "BLOCKED"
	if err := a.pre(ctx, as); err != nil {
		t.Errorf("auto-merge pre: %v", err)
	}
	g.calls = nil
	if _, err := a.do(ctx, as, ""); err != nil || !strings.Contains(strings.Join(g.calls[0], " "), "--squash --auto") {
		t.Errorf("auto-merge do: %v %v", err, g.calls)
	}
	g.pr.AutoMerge = &AutoMerge{Method: "SQUASH"}
	if happened, _, ev, _ := a.probe(ctx, as, ""); !happened || !strings.Contains(ev, "auto-merge on") {
		t.Errorf("auto-merge probe: %v %q", happened, ev)
	}
	if !m.retrySafe() || !a.retrySafe() {
		t.Error("merges carry --match-head-commit, so they're retry-safe")
	}
}

func TestOpaqueRunner(t *testing.T) {
	ctx := context.Background()
	g := &fakeGH{}
	o := opaqueRunner{run: g.run}
	l, _ := fakeLookup()
	s, _, _ := opaqueGH([]string{"pr", "merge", "55", "--admin", "--squash"}, l, "test")
	if err := o.pre(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := o.do(ctx, s, ""); err != nil || strings.Join(g.calls[0], " ") != "dir=/work pr merge 55 --admin --squash" {
		t.Errorf("ran %v, %v", g.calls, err)
	}
	if o.retrySafe() {
		t.Error("opaque is never retried")
	}
	if _, known, _, _ := o.probe(ctx, s, ""); known {
		t.Error("opaque has no probe")
	}
	if err := o.pre(ctx, Step{Op: opOpaque, Text: "kubectl delete ns prod"}); !errors.Is(err, errUnmet) {
		t.Errorf("an opaque step with no argv: %v", err)
	}
}

// A plan whose run failed on a precondition runs again when it is submitted
// again: a merge retried once its checks pass.
func TestExecutorRerunsFailedPlan(t *testing.T) {
	f := newExecFixture(t)
	f.r.preErr = fmt.Errorf("%w: 1 check pending", errUnmet)
	plan := []byte(mergePlan("b-1"))
	resp := f.x.submit(context.Background(), "b-1", plan)
	if resp.State != planFailed || f.r.calls() != 0 {
		t.Fatalf("first run: %+v, %d calls", resp, f.r.calls())
	}
	f.digest = resp.Digest
	if len(f.asked) != 0 {
		t.Errorf("a pending check asked TJ: %v", f.asked)
	}
	f.r.preErr = nil
	resp = f.x.submit(context.Background(), "b-1", plan)
	if resp.State != planDone || f.r.calls() != 1 {
		t.Fatalf("second run: %+v, %d calls", resp, f.r.calls())
	}
	if !strings.Contains(resp.Output, "step 1: did qm:") {
		t.Errorf("output lacks the step's evidence: %q", resp.Output)
	}
	if got := strings.Join(f.kinds(), " "); !strings.Contains(got, "failed rerun") || !strings.HasSuffix(got, "done") {
		t.Errorf("events %s", got)
	}
	// A done plan stays done.
	if resp = f.x.submit(context.Background(), "b-1", plan); resp.State != planDone || f.r.calls() != 1 {
		t.Errorf("third run: %+v, %d calls", resp, f.r.calls())
	}
}
