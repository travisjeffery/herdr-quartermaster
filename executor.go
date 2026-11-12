package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The executor (docs/design/execution-plan.md, "Execution and durability")
// runs approved plans step by step. Every effect is bracketed in the plan's
// fsynced event log: an intent before it, an outcome after. A crash between
// the two leaves an intent with no outcome, and resume settles it: probe
// whether the effect happened, recheck the preconditions, retry only an op
// that is retry-safe, and otherwise stop for TJ. That is at-least-once with a
// probe, not exactly-once.
//
// It runs as `quartermaster executor run` under a systemd user unit and
// listens on Unix sockets: one control socket, and one per bead that dispatch
// opens. The socket a plan arrives on names its bead. On one Unix user that
// identity is still forgeable; it stops being so when the executor runs as
// its own user (P2).

// An opRunner carries out one op for the executor.
type opRunner interface {
	// retrySafe says a retry can neither apply the effect twice nor apply a
	// stale one, because the provider checks the step's preconditions as it
	// acts (a compare-and-swap such as --match-head-commit).
	retrySafe() bool
	// pre rechecks the step's preconditions against the provider, wrapping
	// errStale when one no longer holds.
	pre(ctx context.Context, s Step) error
	// do performs the step. tag marks the effect so probe can find it. An
	// error wrapping errUnknown means the effect may have happened.
	do(ctx context.Context, s Step, tag string) (evidence string, err error)
	// probe says whether the step's effect happened; known is false when it
	// can't tell.
	probe(ctx context.Context, s Step, tag string) (happened, known bool, evidence string, err error)
}

var (
	errStale   = errors.New("precondition no longer holds")
	errUnknown = errors.New("outcome unknown")
	errBusy    = errors.New("busy")
)

// opRunners are the ops the executor can carry out. An op with no runner
// fails before its intent: it is checked and journaled, never run. Merges and
// applies register theirs with their shims (P1, later PRs).
var opRunners = map[string]opRunner{}

// maxAttempts bounds the tries of one retry-safe step, the first included.
const maxAttempts = 3

// executor runs plans. Its fields are what tests replace.
type executor struct {
	cfg     Config
	show    func(id string) (Bead, error)
	active  func() ([]Bead, error)
	needsMe func(id, note string) error
	runners map[string]opRunner
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration)
	logf    func(format string, a ...any)

	mu    sync.Mutex
	socks map[string]net.Listener
	wg    sync.WaitGroup
}

func newExecutor(cfg Config, logger *log.Logger) *executor {
	bd := Beads{actor: "executor"}
	return &executor{
		cfg:    cfg,
		show:   bd.Show,
		active: bd.Active,
		needsMe: func(id, note string) error {
			return errors.Join(bd.Note(id, note), bd.SetStatus(id, StatusNeedsMe))
		},
		runners: opRunners,
		now:     time.Now,
		sleep:   sleepCtx,
		logf:    logger.Printf,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// backoff is the wait before attempt n+1.
func backoff(n int) time.Duration { return time.Duration(n) * 5 * time.Second }

// stepTag marks step i's effect where the provider can hold it (a comment
// marker, a stack update message), so probe can find it.
func stepTag(digest string, i int) string { return fmt.Sprintf("qm:%s:%d", short(digest), i) }

// stepState is where step i (1-based) stands in events: "" not started, ok,
// failed, or pending (an intent with no outcome, or an unknown one), and how
// many attempts it has had.
func stepState(events []planEvent, i int) (string, int) {
	state, n := "", 0
	for _, e := range events {
		if e.Step != i {
			continue
		}
		switch e.Kind {
		case planIntent:
			state, n = "pending", max(n, e.Attempt)
		case planOutcome:
			if e.Result == resultUnknown {
				state = "pending"
			} else {
				state = e.Result
			}
		}
	}
	return state, n
}

func isEnd(kind string) bool {
	return kind == planDone || kind == planFailed || kind == planNeedsMe || kind == planExpired
}

// lastRun is the latest intent, outcome or end in events.
func lastRun(events []planEvent) (planEvent, bool) {
	for _, e := range slices.Backward(events) {
		if e.Kind == planIntent || e.Kind == planOutcome || isEnd(e.Kind) {
			return e, true
		}
	}
	return planEvent{}, false
}

// unfinished says a run started and neither ended nor stopped: the executor
// went away mid-plan, and resumes it on start.
func unfinished(events []planEvent) bool {
	e, ok := lastRun(events)
	return ok && !isEnd(e.Kind)
}

// planRunState is how a plan's run stands, "" if it never ran.
func planRunState(events []planEvent, digest string) string {
	e, ok := lastRun(events)
	if !ok {
		return ""
	}
	again := fmt.Sprintf("quartermaster plan approve %s --by TJ, then quartermaster plan resume %s", digest, digest)
	switch e.Kind {
	case planDone:
		return "done " + e.At.Format("15:04")
	case planFailed:
		return "failed: " + e.Why
	case planNeedsMe:
		return "stopped for TJ: " + e.Why + "; " + again
	case planExpired:
		return "expired: " + e.Why + "; " + again
	case planIntent:
		return fmt.Sprintf("running step %d (attempt %d), or the executor stopped there: quartermaster plan resume %s", e.Step, e.Attempt, digest)
	}
	return fmt.Sprintf("step %d %s; quartermaster plan resume %s continues it", e.Step, e.Result, digest)
}

// record appends e to digest's log and mirrors it into the journal, so the
// digest and report see what ran.
func (x *executor) record(digest string, p Plan, e planEvent) error {
	e.At = x.now()
	err := appendPlanEvent(digest, e)
	text := fmt.Sprintf("plan %s %s", short(digest), e.Kind)
	if e.Step > 0 {
		text = fmt.Sprintf("plan %s step %d %s", short(digest), e.Step, e.Kind)
	}
	if e.Kind == planIntent {
		text += fmt.Sprintf(" (attempt %d)", e.Attempt)
	}
	if e.Result != "" {
		text += " " + e.Result
	}
	for _, s := range []string{e.Evidence, e.Why} {
		if s != "" {
			text += ": " + s
		}
	}
	rec := journalRecord{At: e.At, Kind: journalExec, Bead: p.Bead, Class: e.Class, Action: "plan", Pass: e.Kind == planDone || e.Result == resultOK, Text: text}
	return errors.Join(err, appendJournal(rec))
}

// end records how a plan ended or stopped and, when it stopped for TJ, puts
// the bead to needs_me with the question.
func (x *executor) end(digest string, p Plan, class int, kind, why string) (string, error) {
	err := x.record(digest, p, planEvent{Kind: kind, Class: class, Why: why})
	if kind == planNeedsMe || kind == planExpired {
		note := fmt.Sprintf("EXEC: plan %s stopped: %s. To go on: quartermaster plan approve %s --by TJ, then quartermaster plan resume %s\nCLASS: %d — plan %s", short(digest), why, digest, digest, class, short(digest))
		if nerr := x.needsMe(p.Bead, note); nerr != nil {
			err = errors.Join(err, fmt.Errorf("needs_me on %s: %w", p.Bead, nerr))
		}
	}
	x.logf("plan %s (%s): %s %s", short(digest), p.Bead, kind, why)
	return kind, err
}

// recheck is what stops plan p before its next step, "" if nothing does: a
// closed bead; no live approval; and for the dial's approval, a level now
// below the plan's class or a spent budget. TJ's approval stands above the
// level until it expires.
func (x *executor) recheck(p Plan, digest string, class int) (kind, why string) {
	now := x.now()
	b, err := x.show(p.Bead)
	if err != nil {
		return planNeedsMe, "can't read " + p.Bead + ": " + err.Error()
	}
	if b.Status == StatusClosed {
		return planFailed, p.Bead + " is closed"
	}
	events, err := readPlanEvents(digest)
	if err != nil {
		return planNeedsMe, "events: " + err.Error()
	}
	a, ok := liveApproval(events, now)
	if !ok {
		return planExpired, "no live approval"
	}
	if a.By == "TJ" {
		return "", ""
	}
	if lvl, capped := x.cfg.beadLevel(b, autonomyStopped()); class > lvl {
		if capped != "" {
			capped = ", capped by " + capped
		}
		return planNeedsMe, fmt.Sprintf("class %d is above %s's level %d now%s", class, b.ID, lvl, capped)
	}
	recs, err := readJournal(now.Add(-budgetWindow))
	if err != nil {
		return planNeedsMe, "journal: " + err.Error()
	}
	if spent := budgetSpent(x.cfg.Autonomy.Budget, recs, b.ID, class, ""); spent != "" {
		return planNeedsMe, spent
	}
	return "", ""
}

// execPlan runs the stored plan digest from where its log leaves off, and
// returns how it ended: done, failed, needs_me or expired, or "" when ctx
// ended it mid-plan (the next start resumes it). One plan per bead runs at a
// time; another gets errBusy.
func (x *executor) execPlan(ctx context.Context, digest string) (string, error) {
	p, digest, err := loadPlan(digest)
	if err != nil {
		return "", err
	}
	unlock, err := lockBead(p.Bead)
	if err != nil {
		return "", err
	}
	defer unlock()
	events, err := readPlanEvents(digest)
	if err != nil {
		return "", err
	}
	if e, ok := lastRun(events); ok && (e.Kind == planDone || e.Kind == planFailed) {
		return e.Kind, nil
	}
	b, err := x.show(p.Bead)
	if err != nil {
		return "", err
	}
	class := 0
	for _, s := range p.Steps {
		c, _ := x.cfg.stepClass(b, s)
		class = max(class, c)
	}
	// Every step needs a runner before the first one runs, so a plan never
	// stops halfway for want of one.
	for i, s := range p.Steps {
		if x.runners[s.Op] != nil {
			continue
		}
		if state, _ := stepState(events, i+1); state == resultOK {
			continue
		}
		if _, started := lastRun(events); !started {
			return x.end(digest, p, class, planFailed, fmt.Sprintf("step %d: the executor can't run %s yet", i+1, s.Op))
		}
		return x.end(digest, p, class, planNeedsMe, fmt.Sprintf("step %d: no runner for %s to carry it on", i+1, s.Op))
	}
	for i, s := range p.Steps {
		kind, err := x.runStep(ctx, digest, p, class, i+1, s, x.runners[s.Op])
		if kind != "" || err != nil {
			return kind, err
		}
	}
	return x.end(digest, p, class, planDone, "")
}

// runStep takes step n to ok, returning "" and nil, or ends the plan and
// returns how. A ctx that ends mid-step leaves it pending and returns "".
func (x *executor) runStep(ctx context.Context, digest string, p Plan, class, n int, s Step, r opRunner) (string, error) {
	tag := stepTag(digest, n)
	outcome := func(attempt int, result, evidence string) error {
		return x.record(digest, p, planEvent{Kind: planOutcome, Class: class, Step: n, Attempt: attempt, Result: result, Evidence: evidence})
	}
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		events, err := readPlanEvents(digest)
		if err != nil {
			return "", err
		}
		state, attempts := stepState(events, n)
		switch state {
		case resultOK:
			return "", nil
		case resultFailed:
			return x.end(digest, p, class, planFailed, fmt.Sprintf("step %d failed", n))
		case "pending":
			// It may have happened: ask the provider before anything else.
			happened, known, evidence, err := r.probe(ctx, s, tag)
			switch {
			case err != nil:
				return x.end(digest, p, class, planNeedsMe, fmt.Sprintf("step %d: probe: %v", n, err))
			case !known:
				return x.end(digest, p, class, planNeedsMe, fmt.Sprintf("step %d may have happened and its probe can't tell", n))
			case happened:
				if err := outcome(attempts, resultOK, "probe: "+evidence); err != nil {
					return "", err
				}
				return "", nil
			case !r.retrySafe():
				return x.end(digest, p, class, planNeedsMe, fmt.Sprintf("step %d didn't happen and %s isn't retry-safe", n, s.Op))
			case attempts >= maxAttempts:
				return x.end(digest, p, class, planNeedsMe, fmt.Sprintf("step %d didn't happen after %d attempts", n, attempts))
			}
			x.sleep(ctx, backoff(attempts))
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
		}
		if kind, why := x.recheck(p, digest, class); kind != "" {
			return x.end(digest, p, class, kind, fmt.Sprintf("before step %d: %s", n, why))
		}
		if err := r.pre(ctx, s); err != nil {
			if errors.Is(err, errStale) {
				return x.end(digest, p, class, planFailed, fmt.Sprintf("step %d: %v", n, err))
			}
			return x.end(digest, p, class, planNeedsMe, fmt.Sprintf("step %d: preconditions: %v", n, err))
		}
		attempt := attempts + 1
		if err := x.record(digest, p, planEvent{Kind: planIntent, Class: class, Step: n, Attempt: attempt}); err != nil {
			// No intent on disk, so no effect: stop before acting.
			return "", err
		}
		evidence, err := r.do(ctx, s, tag)
		switch {
		case err == nil:
			if err := outcome(attempt, resultOK, evidence); err != nil {
				return "", err
			}
			return "", nil
		case ctx.Err() != nil || errors.Is(err, errUnknown):
			if oerr := outcome(attempt, resultUnknown, err.Error()); oerr != nil {
				return "", oerr
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// Loop: the pending branch probes it.
		default:
			if oerr := outcome(attempt, resultFailed, err.Error()); oerr != nil {
				return "", oerr
			}
			return x.end(digest, p, class, planFailed, fmt.Sprintf("step %d: %v", n, err))
		}
	}
}

// resumeAll resumes every plan a run left unfinished.
func (x *executor) resumeAll(ctx context.Context) {
	entries, err := os.ReadDir(plansDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		x.logf("resume: %v", err)
		return
	}
	for _, e := range entries {
		events, err := readPlanEvents(e.Name())
		if err != nil || !unfinished(events) {
			continue
		}
		x.logf("resume: plan %s", short(e.Name()))
		if kind, err := x.execPlan(ctx, e.Name()); err != nil && !errors.Is(err, errBusy) {
			x.logf("resume: plan %s: %v", short(e.Name()), err)
		} else if kind != "" {
			x.logf("resume: plan %s: %s", short(e.Name()), kind)
		}
	}
}

// lockBead takes bead id's run lock, an flock held until unlock. It isn't
// waited for: a second plan on the bead gets errBusy.
func lockBead(id string) (func(), error) {
	if !beadName.MatchString(id) {
		return nil, fmt.Errorf("%w: %q isn't a bead id", errPlan, id)
	}
	dir := filepath.Join(runDir(), "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s already has a plan running", errBusy, id)
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// The socket protocol is one JSON request line and one JSON response line.
type execRequest struct {
	// On a bead's socket.
	Run json.RawMessage `json:"run,omitempty"` // a plan; its bead is the socket's
	// On the control socket.
	Open   string `json:"open,omitempty"`   // a bead to open a socket for (dispatch)
	Close  string `json:"close,omitempty"`  // a bead whose socket to remove (resolve)
	Resume string `json:"resume,omitempty"` // a plan digest (prefix) to continue
	Ping   bool   `json:"ping,omitempty"`
}

type execResponse struct {
	Digest string `json:"digest,omitempty"`
	// State is the plan's end (done, failed, needs_me, expired), waiting when
	// the dial didn't approve it, ok for a control request, error.
	State  string `json:"state"`
	Why    string `json:"why,omitempty"`
	Output string `json:"output,omitempty"`
}

const journalExec = "exec"

func runDir() string              { return filepath.Join(stateDir(), "run") }
func controlSocket() string       { return filepath.Join(runDir(), "executor.sock") }
func beadSocket(id string) string { return filepath.Join(runDir(), "beads", id+".sock") }

// beadName is what a bead id must look like to name a socket or lock file.
var beadName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxSocketPath is the longest path a Unix socket address takes (sun_path,
// less its terminator, on macOS; Linux allows 107).
const maxSocketPath = 103

func listenUnix(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is %d bytes, longer than a Unix socket allows; use a shorter state dir", path, len(path))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// execCall sends req to the socket at path and reads the response.
func execCall(path string, req execRequest, timeout time.Duration) (execResponse, error) {
	var resp execResponse
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return resp, fmt.Errorf("executor: %w", err)
	}
	if resp.State == "error" {
		return resp, errors.New(resp.Why)
	}
	return resp, nil
}

// serve runs the executor until ctx ends: the control socket, a socket for
// every active bead, and the resume of unfinished plans. In-flight steps see
// ctx end and are left pending for the next start.
func (x *executor) serve(ctx context.Context) error {
	if _, err := execCall(controlSocket(), execRequest{Ping: true}, 2*time.Second); err == nil {
		return fmt.Errorf("an executor is already listening on %s", controlSocket())
	}
	ctl, err := listenUnix(controlSocket())
	if err != nil {
		return err
	}
	x.socks = map[string]net.Listener{}
	open := map[string]bool{}
	if beads, err := x.active(); err != nil {
		x.logf("active beads: %v", err)
	} else {
		for _, b := range beads {
			if err := x.open(ctx, b.ID); err != nil {
				x.logf("open %s: %v", b.ID, err)
			}
			open[b.ID+".sock"] = true
		}
	}
	// A socket left by a bead that is no longer active goes.
	if entries, err := os.ReadDir(filepath.Dir(beadSocket("x"))); err == nil {
		for _, e := range entries {
			if !open[e.Name()] {
				_ = os.Remove(filepath.Join(filepath.Dir(beadSocket("x")), e.Name()))
			}
		}
	}
	x.wg.Go(func() { x.resumeAll(ctx) })
	x.wg.Go(func() { x.accept(ctx, ctl, "") })
	x.logf("executor listening on %s", controlSocket())
	<-ctx.Done()
	ctl.Close()
	x.mu.Lock()
	for _, l := range x.socks {
		l.Close()
	}
	x.mu.Unlock()
	x.wg.Wait()
	_ = os.Remove(controlSocket())
	return nil
}

// open listens on bead id's socket, once.
func (x *executor) open(ctx context.Context, id string) error {
	if !beadName.MatchString(id) {
		return fmt.Errorf("%q isn't a bead id", id)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.socks[id]; ok {
		return nil
	}
	l, err := listenUnix(beadSocket(id))
	if err != nil {
		return err
	}
	x.socks[id] = l
	x.wg.Go(func() { x.accept(ctx, l, id) })
	return nil
}

func (x *executor) close(id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if l, ok := x.socks[id]; ok {
		l.Close()
		delete(x.socks, id)
	}
	_ = os.Remove(beadSocket(id))
}

// accept serves l until it closes; bead is the socket's bead, "" for the
// control socket.
func (x *executor) accept(ctx context.Context, l net.Listener, bead string) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		x.wg.Go(func() {
			defer conn.Close()
			var req execRequest
			resp := execResponse{State: "error"}
			if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
				resp.Why = "bad request: " + err.Error()
			} else {
				resp = x.handle(ctx, bead, req)
			}
			_ = json.NewEncoder(conn).Encode(resp)
		})
	}
}

func (x *executor) handle(ctx context.Context, bead string, req execRequest) execResponse {
	fail := func(err error) execResponse { return execResponse{State: "error", Why: err.Error()} }
	if bead != "" {
		if req.Run == nil {
			return fail(errors.New("a bead's socket only takes run"))
		}
		return x.submit(ctx, bead, req.Run)
	}
	switch {
	case req.Ping:
		return execResponse{State: "ok"}
	case req.Open != "":
		if err := x.open(ctx, req.Open); err != nil {
			return fail(err)
		}
		return execResponse{State: "ok", Output: beadSocket(req.Open)}
	case req.Close != "":
		x.close(req.Close)
		return execResponse{State: "ok"}
	case req.Resume != "":
		kind, err := x.execPlan(ctx, req.Resume)
		if err != nil {
			return fail(err)
		}
		_, digest, _ := loadPlan(req.Resume)
		return x.result(digest, kind)
	}
	return fail(errors.New("empty request"))
}

// submit checks a plan that arrived on bead's socket, unless it already has a
// live approval, and runs it.
func (x *executor) submit(ctx context.Context, bead string, raw json.RawMessage) execResponse {
	fail := func(err error) execResponse { return execResponse{State: "error", Why: err.Error()} }
	// The socket names the bead; a plan may leave it out but can't name another.
	var loose map[string]json.RawMessage
	if err := json.Unmarshal(raw, &loose); err != nil {
		return fail(fmt.Errorf("%w: not a plan: %v", errPlan, err))
	}
	var named string
	_ = json.Unmarshal(loose["bead"], &named)
	if named != "" && named != bead {
		return fail(fmt.Errorf("%w: a plan for %s on %s's socket", errPlan, named, bead))
	}
	loose["bead"], _ = json.Marshal(bead)
	data, _ := json.Marshal(loose)
	p, err := parsePlan(data)
	if err != nil {
		return fail(err)
	}
	digest, err := p.digest()
	if err != nil {
		return fail(err)
	}
	events, err := readPlanEvents(digest)
	if err != nil {
		return fail(err)
	}
	var out strings.Builder
	if _, ok := liveApproval(events, x.now()); !ok {
		if err := checkPlanFor(x.cfg, x.show, p, x.now(), &out); err != nil {
			if errors.Is(err, errNotAutonomous) {
				return execResponse{Digest: digest, State: "waiting", Why: err.Error(), Output: out.String()}
			}
			return fail(err)
		}
	}
	kind, err := x.execPlan(ctx, digest)
	if err != nil {
		resp := fail(err)
		resp.Digest = digest
		return resp
	}
	resp := x.result(digest, kind)
	resp.Output = out.String() + resp.Output
	return resp
}

// result is a plan's response after a run that ended as kind.
func (x *executor) result(digest, kind string) execResponse {
	events, _ := readPlanEvents(digest)
	resp := execResponse{Digest: digest, State: kind, Output: planState(events, digest, x.now()) + "\n"}
	if e, ok := lastRun(events); ok {
		resp.Why = e.Why
	}
	if kind == "" {
		resp.State = "stopped"
		resp.Why = "the executor is stopping; it resumes the plan when it starts"
	}
	return resp
}

// executorRun is `quartermaster executor run`: serve until SIGINT or SIGTERM.
func executorRun(cfg Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return newExecutor(cfg, log.New(os.Stderr, "", log.LstdFlags)).serve(ctx)
}

// execOpen asks a running executor for bead id's socket. Without one it does
// nothing: routing through the executor is advisory (S1).
func execOpen(id string) {
	_, _ = execCall(controlSocket(), execRequest{Open: id}, 5*time.Second)
}

func execClose(id string) {
	_, _ = execCall(controlSocket(), execRequest{Close: id}, 5*time.Second)
}

// execEnv is the worker's export of its bead's socket, appended to agentEnv.
func execEnv(id string) string {
	return " QUARTERMASTER_EXEC_SOCKET=" + shellQuote(beadSocket(id))
}

// planRun is `plan run`: send the plan to its bead's socket
// ($QUARTERMASTER_EXEC_SOCKET, else the bead's) and wait for its end.
func planRun(data []byte, w io.Writer) error {
	sock := os.Getenv("QUARTERMASTER_EXEC_SOCKET")
	if sock == "" {
		var p struct{ Bead string }
		if err := json.Unmarshal(data, &p); err != nil || p.Bead == "" {
			return fmt.Errorf("%w: no bead, and no QUARTERMASTER_EXEC_SOCKET", errPlan)
		}
		sock = beadSocket(p.Bead)
	}
	resp, err := execCall(sock, execRequest{Run: data}, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("no executor on %s: is it running (quartermaster executor status)?", sock)
		}
		return err
	}
	return reportRun(resp, w)
}

// planResume is `plan resume`: the executor continues the plan.
func planResume(ref string, w io.Writer) error {
	resp, err := execCall(controlSocket(), execRequest{Resume: ref}, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return errors.New("the executor isn't running: quartermaster executor install, or quartermaster executor run")
		}
		return err
	}
	return reportRun(resp, w)
}

func reportRun(resp execResponse, w io.Writer) error {
	fmt.Fprint(w, resp.Output)
	switch resp.State {
	case planDone:
		return nil
	case "waiting":
		return errors.New(resp.Why)
	}
	return fmt.Errorf("plan %s %s: %s", short(resp.Digest), resp.State, resp.Why)
}

//go:embed systemd/quartermaster-executor.service
var executorUnits embed.FS

const executorService = "quartermaster-executor.service"

// executorCmd is `quartermaster executor run|install|uninstall|status`.
func executorCmd(cfg Config, sub string, w io.Writer) error {
	var lines []string
	var err error
	switch sub {
	case "run":
		return executorRun(cfg)
	case "install":
		lines, err = executorInstall(cfg, userUnitDir(), exec.LookPath, systemctlUser)
	case "uninstall":
		lines, err = executorUninstall(userUnitDir(), systemctlUser)
	case "status":
		if _, err := execCall(controlSocket(), execRequest{Ping: true}, 2*time.Second); err != nil {
			return fmt.Errorf("the executor isn't answering on %s: %v", controlSocket(), err)
		}
		lines = []string{"executor listening on " + controlSocket()}
	default:
		return errors.New("usage: quartermaster executor run|install|uninstall|status")
	}
	fmt.Fprint(w, strings.Join(append(lines, ""), "\n"))
	return err
}

// executorInstall writes the executor's user unit and drop-in into dir and
// starts it.
func executorInstall(cfg Config, dir string, lookPath func(string) (string, error), systemctl func(args ...string) error) ([]string, error) {
	if err := installedBinary(); err != nil {
		return nil, err
	}
	b, err := executorUnits.ReadFile("systemd/" + executorService)
	if err != nil {
		return nil, err
	}
	var lines []string
	if err := writeAtomic(filepath.Join(dir, executorService), b); err != nil {
		return nil, err
	}
	lines = append(lines, "wrote "+filepath.Join(dir, executorService))
	dropIn := filepath.Join(dir, executorService+".d", toolName+".conf")
	if err := writeAtomic(dropIn, []byte(unitDropIn("executor install", cfg.BeadsDir, digestSearchPath(lookPath, miseShims())))); err != nil {
		return lines, err
	}
	lines = append(lines, "wrote "+dropIn)
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", executorService}, {"restart", executorService}} {
		if err := systemctl(args...); err != nil {
			return lines, err
		}
		lines = append(lines, "systemctl --user "+strings.Join(args, " "))
	}
	return lines, nil
}

func executorUninstall(dir string, systemctl func(args ...string) error) ([]string, error) {
	var lines []string
	if err := systemctl("disable", "--now", executorService); err == nil {
		lines = append(lines, "systemctl --user disable --now "+executorService)
	}
	for _, p := range []string{filepath.Join(dir, executorService+".d", toolName+".conf"), filepath.Join(dir, executorService)} {
		if err := os.Remove(p); err == nil {
			lines = append(lines, "removed "+p)
		} else if !os.IsNotExist(err) {
			return lines, err
		}
	}
	_ = os.Remove(filepath.Join(dir, executorService+".d"))
	return lines, systemctl("daemon-reload")
}
