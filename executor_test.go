package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner stands in for a provider: an effect is a file named by the
// step's tag in dir, so a probe can find it after a crash.
type fakeRunner struct {
	dir     string
	safe    bool
	preErr  error
	doErr   error // returned after the effect is made, unless skip
	skip    bool  // make no effect
	unknown bool  // probe can't tell
	mu      sync.Mutex
	dos     int
}

func (f *fakeRunner) retrySafe() bool                 { return f.safe }
func (f *fakeRunner) pre(context.Context, Step) error { return f.preErr }
func (f *fakeRunner) effect(tag string) string {
	return filepath.Join(f.dir, strings.ReplaceAll(tag, ":", "_"))
}
func (f *fakeRunner) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.dos }
func (f *fakeRunner) do(_ context.Context, _ Step, tag string) (string, error) {
	f.mu.Lock()
	f.dos++
	f.mu.Unlock()
	if !f.skip {
		if err := os.WriteFile(f.effect(tag), nil, 0o644); err != nil {
			return "", err
		}
	}
	return "did " + tag, f.doErr
}
func (f *fakeRunner) probe(_ context.Context, _ Step, tag string) (bool, bool, string, error) {
	if f.unknown {
		return false, false, "", nil
	}
	_, err := os.Stat(f.effect(tag))
	return err == nil, true, "found " + tag, nil
}

type execFixture struct {
	t      *testing.T
	x      *executor
	r      *fakeRunner
	bead   Bead
	asked  []string
	digest string
}

// newExecFixture is an executor over a fresh state dir with bead b-1 at
// autonomy level 4, where a merge (class 4) is approved by the dial.
func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	if os.Getenv("QM_CRASH_HELPER") == "" && os.Getenv("QM_KEEP_STATE") == "" {
		t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	}
	f := &execFixture{t: t, bead: Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:4"}}}
	f.r = &fakeRunner{dir: filepath.Join(stateDir(), "effects"), safe: true}
	if err := os.MkdirAll(f.r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.x = &executor{
		cfg: defaultConfig(),
		show: func(id string) (Bead, error) {
			if id != f.bead.ID {
				return Bead{}, errors.New("no bead " + id)
			}
			return f.bead, nil
		},
		active:  func() ([]Bead, error) { return []Bead{f.bead}, nil },
		needsMe: func(id, note string) error { f.asked = append(f.asked, id+": "+note); return nil },
		runners: map[string]opRunner{"github.pr.merge": f.r},
		now:     time.Now,
		sleep:   func(context.Context, time.Duration) {},
		logf:    log.New(io.Discard, "", 0).Printf,
	}
	return f
}

// twoMerges is a plan of two merges on bead.
func twoMerges(bead string) string {
	step := func(n int) string {
		return fmt.Sprintf(`{"op":"github.pr.merge","target":"github:acme/app#%d","pre":{"head_sha":"%s"}}`, n, sha)
	}
	return `{"bead":"` + bead + `","steps":[` + step(55) + `,` + step(56) + `]}`
}

// check stores and approves plan by the dial, as plan check does.
func (f *execFixture) check(plan string) string {
	f.t.Helper()
	p := mustParse(f.t, plan)
	if err := checkPlanFor(f.x.cfg, f.x.show, p, time.Now(), io.Discard); err != nil {
		f.t.Fatal(err)
	}
	f.digest = mustDigest(f.t, p)
	return f.digest
}

func (f *execFixture) events() []planEvent {
	f.t.Helper()
	ev, err := readPlanEvents(f.digest)
	if err != nil {
		f.t.Fatal(err)
	}
	return ev
}

// kinds is the run's events as "intent 1/1", "outcome 1 ok", "done".
func (f *execFixture) kinds() []string {
	var out []string
	for _, e := range f.events() {
		switch e.Kind {
		case planIntent:
			out = append(out, fmt.Sprintf("intent %d/%d", e.Step, e.Attempt))
		case planOutcome:
			out = append(out, fmt.Sprintf("outcome %d %s", e.Step, e.Result))
		case planChecked, planApproved:
		default:
			out = append(out, e.Kind)
		}
	}
	return out
}

func (f *execFixture) runPlan() string {
	f.t.Helper()
	kind, err := f.x.execPlan(context.Background(), f.digest)
	if err != nil {
		f.t.Fatal(err)
	}
	return kind
}

func TestExecutorRunsPlan(t *testing.T) {
	f := newExecFixture(t)
	f.check(twoMerges("b-1"))
	if kind := f.runPlan(); kind != planDone {
		t.Fatalf("ended %s: %v", kind, f.kinds())
	}
	want := []string{"intent 1/1", "outcome 1 ok", "intent 2/1", "outcome 2 ok", "done"}
	if got := f.kinds(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	// The run is mirrored into the journal, so the digest and report see it.
	recs, _ := readJournal(time.Time{})
	var mirrored []string
	for _, r := range recs {
		if r.Kind == journalExec {
			mirrored = append(mirrored, r.Text)
		}
	}
	if len(mirrored) != 5 || !strings.Contains(mirrored[0], "step 1 intent (attempt 1)") || !strings.HasSuffix(mirrored[4], "done") {
		t.Errorf("journal: %q", mirrored)
	}
	// A finished plan doesn't run again.
	if kind := f.runPlan(); kind != planDone || f.r.calls() != 2 {
		t.Errorf("again: %s after %d calls", kind, f.r.calls())
	}
	if s := planRunState(f.events(), f.digest); !strings.HasPrefix(s, "done") {
		t.Errorf("state %q", s)
	}
}

// TestExecutorCrashHelper is the executor process the crash test kills mid-
// step: its runner makes the effect (or not) and exits before the outcome
// is written, as a crash or kill -9 would.
func TestExecutorCrashHelper(t *testing.T) {
	if os.Getenv("QM_CRASH_HELPER") == "" {
		t.Skip("run by TestExecutorCrashMidStep")
	}
	// TestMain clears the state dir variables; the parent's comes separately.
	t.Setenv("QUARTERMASTER_STATE_DIR", os.Getenv("QM_CRASH_STATE"))
	f := newExecFixture(t)
	f.digest = os.Getenv("QM_CRASH_DIGEST")
	f.x.runners["github.pr.merge"] = crashRunner{f.r, os.Getenv("QM_CRASH_EFFECT") == "1"}
	_, err := f.x.execPlan(context.Background(), f.digest)
	t.Fatalf("the runner should have exited the process: %v", err)
}

type crashRunner struct {
	*fakeRunner
	effect bool
}

func (c crashRunner) do(_ context.Context, _ Step, tag string) (string, error) {
	if c.effect {
		_ = os.WriteFile(c.fakeRunner.effect(tag), nil, 0o644)
	}
	os.Exit(3)
	return "", nil
}

// crash runs a real executor process on f's plan and lets it die in step 1.
func (f *execFixture) crash(effect bool) {
	f.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecutorCrashHelper$")
	cmd.Env = append(os.Environ(), "QM_CRASH_HELPER=1", "QM_CRASH_DIGEST="+f.digest, "QM_CRASH_STATE="+stateDir())
	if effect {
		cmd.Env = append(cmd.Env, "QM_CRASH_EFFECT=1")
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		f.t.Fatalf("helper: %v\n%s", err, out)
	}
	// An intent on disk and no outcome: what the crash left.
	if got := f.kinds(); !slices.Equal(got, []string{"intent 1/1"}) {
		f.t.Fatalf("after the crash: %v", got)
	}
	if !unfinished(f.events()) {
		f.t.Fatal("a crashed run is unfinished")
	}
}

func TestExecutorCrashMidStep(t *testing.T) {
	t.Run("effect happened: probe settles it, no second effect", func(t *testing.T) {
		f := newExecFixture(t)
		f.check(twoMerges("b-1"))
		f.crash(true)
		f.x.resumeAll(context.Background())
		want := []string{"intent 1/1", "outcome 1 ok", "intent 2/1", "outcome 2 ok", "done"}
		if got := f.kinds(); !slices.Equal(got, want) {
			t.Errorf("events %v, want %v", got, want)
		}
		if f.r.calls() != 1 {
			t.Errorf("resume ran %d steps, want only step 2", f.r.calls())
		}
		if e := f.events()[3]; !strings.HasPrefix(e.Evidence, "probe: ") {
			t.Errorf("step 1's outcome came from %q, not the probe", e.Evidence)
		}
	})
	t.Run("effect didn't happen, retry-safe: retried", func(t *testing.T) {
		f := newExecFixture(t)
		f.check(twoMerges("b-1"))
		f.crash(false)
		f.x.resumeAll(context.Background())
		want := []string{"intent 1/1", "intent 1/2", "outcome 1 ok", "intent 2/1", "outcome 2 ok", "done"}
		if got := f.kinds(); !slices.Equal(got, want) {
			t.Errorf("events %v, want %v", got, want)
		}
	})
	t.Run("effect didn't happen, not retry-safe: needs_me", func(t *testing.T) {
		f := newExecFixture(t)
		f.r.safe = false
		f.check(twoMerges("b-1"))
		f.crash(false)
		f.x.resumeAll(context.Background())
		if got := f.kinds(); !slices.Equal(got, []string{"intent 1/1", "needs_me"}) {
			t.Errorf("events %v", got)
		}
		if len(f.asked) != 1 || !strings.Contains(f.asked[0], "isn't retry-safe") || !strings.Contains(f.asked[0], "CLASS: 4") {
			t.Errorf("asked %q", f.asked)
		}
		if f.r.calls() != 0 {
			t.Error("a step that isn't retry-safe ran again")
		}
	})
	t.Run("probe can't tell: needs_me", func(t *testing.T) {
		f := newExecFixture(t)
		f.r.unknown = true
		f.check(twoMerges("b-1"))
		f.crash(true)
		f.x.resumeAll(context.Background())
		if got := f.kinds(); !slices.Equal(got, []string{"intent 1/1", "needs_me"}) || f.r.calls() != 0 {
			t.Errorf("events %v after %d calls", got, f.r.calls())
		}
	})
	t.Run("precondition moved: failed, not retried", func(t *testing.T) {
		f := newExecFixture(t)
		f.check(twoMerges("b-1"))
		f.crash(false)
		f.r.preErr = fmt.Errorf("%w: head is now abc", errStale)
		f.x.resumeAll(context.Background())
		if got := f.kinds(); !slices.Equal(got, []string{"intent 1/1", "failed"}) || f.r.calls() != 0 {
			t.Errorf("events %v after %d calls", got, f.r.calls())
		}
		if s := planRunState(f.events(), f.digest); !strings.Contains(s, "head is now abc") {
			t.Errorf("state %q", s)
		}
	})
}

func TestExecutorUnknownOutcome(t *testing.T) {
	// The call returned unknown, but the effect is there: the probe says ok.
	f := newExecFixture(t)
	f.r.doErr = fmt.Errorf("%w: connection reset", errUnknown)
	f.check(mergePlan("b-1"))
	if kind := f.runPlan(); kind != planDone {
		t.Fatalf("ended %s: %v", kind, f.kinds())
	}
	if got := f.kinds(); !slices.Equal(got, []string{"intent 1/1", "outcome 1 unknown", "outcome 1 ok", "done"}) {
		t.Errorf("events %v", got)
	}
	// Unknown and never there: retried up to the limit, then TJ.
	f = newExecFixture(t)
	f.r.doErr, f.r.skip = fmt.Errorf("%w: timeout", errUnknown), true
	f.check(mergePlan("b-1"))
	if kind := f.runPlan(); kind != planNeedsMe || f.r.calls() != maxAttempts {
		t.Errorf("ended %s after %d calls: %v", kind, f.r.calls(), f.kinds())
	}
	// A plain failure ends the plan.
	f = newExecFixture(t)
	f.r.doErr, f.r.skip = errors.New("405 not mergeable"), true
	f.check(mergePlan("b-1"))
	if kind := f.runPlan(); kind != planFailed || f.r.calls() != 1 {
		t.Errorf("ended %s after %d calls", kind, f.r.calls())
	}
}

func TestExecutorRechecksBeforeEachStep(t *testing.T) {
	// The level drops after step 1: step 2 doesn't run.
	f := newExecFixture(t)
	f.check(twoMerges("b-1"))
	f.x.runners["github.pr.merge"] = &lowerAfter{fakeRunner: f.r, f: f}
	if kind := f.runPlan(); kind != planNeedsMe {
		t.Fatalf("ended %s: %v", kind, f.kinds())
	}
	if got := f.kinds(); !slices.Equal(got, []string{"intent 1/1", "outcome 1 ok", "needs_me"}) {
		t.Errorf("events %v", got)
	}
	if len(f.asked) != 1 || !strings.Contains(f.asked[0], "above b-1's level 3") {
		t.Errorf("asked %q", f.asked)
	}
	// TJ approves it: it goes on from step 2, above the level.
	out, err := (&planCLI{t: t, cfg: f.x.cfg, bead: f.bead}).run("TJ", 0, "approve", f.digest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "quartermaster plan resume "+f.digest) {
		t.Errorf("approve doesn't say how to continue:\n%s", out)
	}
	if kind := f.runPlan(); kind != planDone || f.r.calls() != 2 {
		t.Errorf("after TJ: %s, %d calls: %v", kind, f.r.calls(), f.kinds())
	}

	// A closed bead stops it.
	f = newExecFixture(t)
	f.check(mergePlan("b-1"))
	f.bead.Status = StatusClosed
	if kind := f.runPlan(); kind != planFailed || f.r.calls() != 0 {
		t.Errorf("closed: %s", kind)
	}

	// An expired approval stops it.
	f = newExecFixture(t)
	f.check(mergePlan("b-1"))
	f.x.now = func() time.Time { return time.Now().Add(2 * approvalTTL) }
	if kind := f.runPlan(); kind != planExpired || f.r.calls() != 0 {
		t.Errorf("expired: %s", kind)
	}
	if s := planRunState(f.events(), f.digest); !strings.Contains(s, "plan resume "+f.digest) {
		t.Errorf("state %q", s)
	}
}

// lowerAfter lowers the bead's level to 3 once a step has run.
type lowerAfter struct {
	*fakeRunner
	f *execFixture
}

func (l *lowerAfter) do(ctx context.Context, s Step, tag string) (string, error) {
	ev, err := l.fakeRunner.do(ctx, s, tag)
	l.f.bead.Labels = []string{"autonomy:3"}
	return ev, err
}

func TestExecutorNeedsRunnerForEveryStep(t *testing.T) {
	f := newExecFixture(t)
	f.x.runners = map[string]opRunner{}
	f.check(mergePlan("b-1"))
	if kind := f.runPlan(); kind != planFailed {
		t.Fatalf("ended %s", kind)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"failed"}) {
		t.Errorf("a step with no runner wrote %v; want no intent", got)
	}
}

func TestExecutorOnePlanPerBead(t *testing.T) {
	f := newExecFixture(t)
	f.check(mergePlan("b-1"))
	unlock, err := lockBead("b-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.x.execPlan(context.Background(), f.digest); !errors.Is(err, errBusy) {
		t.Errorf("second plan on the bead: %v", err)
	}
	unlock()
	if kind := f.runPlan(); kind != planDone {
		t.Errorf("after unlock: %s", kind)
	}
	if _, err := lockBead("../x"); err == nil {
		t.Error("a bead id with a path in it took a lock")
	}
}

// shortStateDir is a state dir short enough for socket paths.
func shortStateDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "qm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("QUARTERMASTER_STATE_DIR", dir)
	t.Setenv("QM_KEEP_STATE", "1")
	if len(beadSocket("b-1")) > maxSocketPath {
		t.Skipf("temp dir %s is too long for a socket path", dir)
	}
}

func TestExecutorSockets(t *testing.T) {
	shortStateDir(t)
	f := newExecFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.x.serve(ctx) }()
	waitFor(t, func() bool {
		_, err := execCall(controlSocket(), execRequest{Ping: true}, time.Second)
		return err == nil
	})

	// A second executor refuses to start.
	if err := newExecFixture(t).x.serve(context.Background()); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("second executor: %v", err)
	}
	// The active bead's socket is open: a plan on it runs, and its bead may
	// be left out.
	var out strings.Builder
	t.Setenv("QUARTERMASTER_EXEC_SOCKET", beadSocket("b-1"))
	plan := strings.Replace(mergePlan("b-1"), `"bead": "b-1", `, "", 1)
	if err := planRun([]byte(plan), &out); err != nil {
		t.Fatalf("plan run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "approved by the dial") || !strings.Contains(out.String(), "done") {
		t.Errorf("output:\n%s", out.String())
	}
	// The socket names the bead: a plan for another is refused.
	if err := planRun([]byte(mergePlan("b-2")), io.Discard); err == nil || !strings.Contains(err.Error(), "on b-1's socket") {
		t.Errorf("plan for b-2 on b-1's socket: %v", err)
	}
	// Above the level: waiting for TJ, nothing run.
	f.bead.Labels = []string{"autonomy:3"}
	calls := f.r.calls()
	if err := planRun([]byte(twoMerges("b-1")), io.Discard); err == nil || !strings.Contains(err.Error(), "above b-1's level 3") || f.r.calls() != calls {
		t.Errorf("above the level: %v", err)
	}
	// TJ approves; plan resume runs it through the control socket.
	f.digest = mustDigest(t, mustParse(t, twoMerges("b-1")))
	if _, err := (&planCLI{t: t, cfg: f.x.cfg, bead: f.bead}).run("TJ", 0, "approve", f.digest); err != nil {
		t.Fatal(err)
	}
	if err := planResume(f.digest[:12], io.Discard); err != nil {
		t.Errorf("resume: %v", err)
	}
	if f.r.calls() != calls+2 {
		t.Errorf("resume ran %d steps", f.r.calls()-calls)
	}
	// dispatch opens a socket for a new bead; resolve removes it.
	execOpen("b-9")
	if _, err := os.Stat(beadSocket("b-9")); err != nil {
		t.Errorf("open: %v", err)
	}
	// dispatch exports the socket and puts the shims first on PATH.
	env := execEnv("b-9")
	if !strings.Contains(env, "QUARTERMASTER_EXEC_SOCKET="+shellQuote(beadSocket("b-9"))) || !strings.Contains(env, " PATH="+shellQuote(shimDir())+`:"$PATH"`) {
		t.Errorf("execEnv = %q", env)
	}
	if !isShim(filepath.Join(shimDir(), "gh")) {
		t.Error("execEnv wrote no gh shim")
	}
	execClose("b-9")
	if _, err := os.Stat(beadSocket("b-9")); !os.IsNotExist(err) {
		t.Errorf("close left %v", err)
	}
	if _, err := execCall(controlSocket(), execRequest{Open: "../evil"}, time.Second); err == nil {
		t.Error("opened a socket for a path")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(controlSocket()); !os.IsNotExist(err) {
		t.Error("the control socket outlived the executor")
	}
	// With no executor, a worker gets neither the socket nor the shims.
	if env := execEnv("b-9"); env != "" {
		t.Errorf("execEnv with no executor = %q", env)
	}
}

// A step running when the executor stops is left unknown, and the next start
// settles it.
func TestExecutorStopMidStep(t *testing.T) {
	f := newExecFixture(t)
	f.check(mergePlan("b-1"))
	ctx, cancel := context.WithCancel(context.Background())
	f.x.runners["github.pr.merge"] = cancelling{f.r, cancel}
	kind, err := f.x.execPlan(ctx, f.digest)
	if kind != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped: %q, %v", kind, err)
	}
	if got := f.kinds(); !slices.Equal(got, []string{"intent 1/1", "outcome 1 unknown"}) || !unfinished(f.events()) {
		t.Fatalf("events %v", got)
	}
	f.x.runners["github.pr.merge"] = f.r
	f.x.resumeAll(context.Background())
	if got := f.kinds(); got[len(got)-1] != planDone || f.r.calls() != 1 {
		t.Errorf("resumed: %v after %d calls", got, f.r.calls())
	}
}

type cancelling struct {
	*fakeRunner
	cancel func()
}

func (c cancelling) do(ctx context.Context, s Step, tag string) (string, error) {
	ev, _ := c.fakeRunner.do(ctx, s, tag)
	c.cancel()
	return ev, ctx.Err()
}

func TestExecutorInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	var calls []string
	systemctl := func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }
	lookPath := func(tool string) (string, error) { return "/opt/bin/" + tool, nil }
	cfg := defaultConfig()
	cfg.BeadsDir = "/home/me/.beads"
	if _, err := executorInstall(cfg, dir, lookPath, systemctl); err == nil {
		t.Error("installed without ~/.local/bin/quartermaster")
	}
	bin := filepath.Join(home, ".local", "bin", toolName)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := executorInstall(cfg, dir, lookPath, systemctl); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(filepath.Join(dir, executorService))
	drop, _ := os.ReadFile(filepath.Join(dir, executorService+".d", toolName+".conf"))
	if !strings.Contains(string(unit), "ExecStart=%h/.local/bin/quartermaster executor run") || !strings.Contains(string(drop), "BEADS_DIR=/home/me/.beads") || !strings.Contains(string(drop), "executor install") {
		t.Errorf("unit:\n%s\ndrop-in:\n%s", unit, drop)
	}
	if !slices.Equal(calls, []string{"daemon-reload", "enable --now " + executorService, "restart " + executorService}) {
		t.Errorf("systemctl %q", calls)
	}
	calls = nil
	if _, err := executorUninstall(dir, systemctl); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, executorService)); !os.IsNotExist(err) {
		t.Error("uninstall left the unit")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}
