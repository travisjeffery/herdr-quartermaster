package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Execution plans (docs/design/execution-plan.md): a plan is a list of typed
// operations a worker proposes, checked against the dial before anything
// runs. This file is the IR, its digest and the check; the executor that runs
// approved plans comes later. Approval, by the dial or by TJ, binds to the
// plan's digest and expires, so a new head or a new preview needs a new one.

// A Plan is what a worker proposes and quartermaster checks and approves.
type Plan struct {
	Bead   string `json:"bead"`             // the bead it serves; the level comes from it
	Actor  string `json:"actor,omitempty"`  // worker or coordinator, and its pane
	Source string `json:"source,omitempty"` // how it arrived: shim, mcp, hook, cli
	Steps  []Step `json:"steps"`
}

type Step struct {
	Op     string            `json:"op"`               // a name from the op table: "github.pr.merge"
	Target string            `json:"target,omitempty"` // canonical: "github:owner/repo#55", "pulumi:org/proj/stack"
	Args   map[string]string `json:"args,omitempty"`   // op-specific, already expanded
	Pre    map[string]string `json:"pre,omitempty"`    // must still hold when the step runs: head_sha, plan_digest, rev
	Text   string            `json:"text,omitempty"`   // for an opaque step, the exact argv or tool call
	// Class is the class the proposer states. It can raise the step's class,
	// never lower it.
	Class int `json:"class,omitempty"`
}

// opSpec is one row of the op table.
type opSpec struct {
	action string   // the dial action it is, for actionClass
	pre    []string // preconditions the step must carry
}

const opOpaque = "opaque"

// ops is the op table. An op not in it is opaque, class 9.
var ops = map[string]opSpec{
	"github.pr.merge":     {action: "merge", pre: []string{"head_sha"}},
	"github.pr.automerge": {action: "merge", pre: []string{"head_sha"}},
	"iac.apply":           {action: "apply"}, // pre.rev, from the shim; the executor binds the preview's digests
	opOpaque:              {},
}

// approvalTTL is how long an approval lasts without --for (E6).
const approvalTTL = time.Hour

var (
	githubTarget = regexp.MustCompile(`^github:([\w.-]+)/([\w.-]+)#([1-9][0-9]*)$`)
	fullSHA      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	errPlan      = errors.New("plan")
)

// parsePlan reads a plan, checks its shape and normalizes it: an iac.apply
// with a preview gets the preview's digest as pre.plan_digest, so approving
// the plan approves that preview and no other. A plan_digest already there
// must match the file.
func parsePlan(data []byte) (Plan, error) {
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("%w: not a plan: %v", errPlan, err)
	}
	if p.Bead == "" {
		return p, fmt.Errorf("%w: no bead", errPlan)
	}
	if len(p.Steps) == 0 {
		return p, fmt.Errorf("%w: no steps", errPlan)
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		bad := func(format string, a ...any) error {
			return fmt.Errorf("%w: step %d (%s): %s", errPlan, i+1, s.Op, fmt.Sprintf(format, a...))
		}
		if s.Op == "" {
			return p, bad("no op")
		}
		if s.Class < 0 || s.Class > 9 {
			return p, bad("class must be 0-9")
		}
		spec, known := ops[s.Op]
		if !known {
			continue // opaque: classified 9, not refused
		}
		for _, k := range spec.pre {
			if s.Pre[k] == "" {
				return p, bad("needs pre.%s", k)
			}
		}
		switch s.Op {
		case "github.pr.merge", "github.pr.automerge":
			if !githubTarget.MatchString(s.Target) {
				return p, bad("target must be github:owner/repo#N, not %q", s.Target)
			}
			if !fullSHA.MatchString(s.Pre["head_sha"]) {
				return p, bad("pre.head_sha must be a full 40-character commit SHA")
			}
		case "iac.apply":
			if tool, stack, _ := strings.Cut(s.Target, ":"); (tool != "pulumi" && tool != "tofu") || stack == "" {
				return p, bad("target must be pulumi:<stack> or tofu:<name>, not %q", s.Target)
			}
			if rev := s.Pre["rev"]; rev != "" && !fullSHA.MatchString(rev) {
				return p, bad("pre.rev must be a full 40-character commit SHA")
			}
			if path := s.Args["preview"]; path != "" {
				b, err := os.ReadFile(path)
				if err != nil {
					return p, bad("preview: %v", err)
				}
				d := sha256Hex(b)
				if have := s.Pre["plan_digest"]; have != "" && have != d {
					return p, bad("pre.plan_digest %s doesn't match the preview, which is %s", short(have), short(d))
				}
				if s.Pre == nil {
					s.Pre = map[string]string{}
				}
				s.Pre["plan_digest"] = d
			}
		case opOpaque:
			if s.Text == "" {
				return p, bad("an opaque step needs text: the exact argv or tool call")
			}
		}
	}
	return p, nil
}

// canonical is p as JSON with every object's keys sorted and no HTML
// escaping, so the digest depends only on what the plan says.
func (p Plan) canonical() ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// digest is the SHA-256 of p's canonical JSON, in hex.
func (p Plan) digest() (string, error) {
	b, err := p.canonical()
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// short is a digest as status lines show it.
func short(d string) string { return d[:min(12, len(d))] }

// stepClass is one step's class on bead b and why: the highest of the op
// table's (through actionClass and targetClass), gate's classification of an
// apply's preview, and the class the step states.
func (c Config) stepClass(b Bead, s Step) (int, string) {
	class, why := c.opClass(b, s)
	if s.Class > class {
		class, why = s.Class, why+fmt.Sprintf("; raised to %d as stated", s.Class)
	}
	return class, why
}

func (c Config) opClass(b Bead, s Step) (int, string) {
	switch s.Op {
	case "github.pr.merge", "github.pr.automerge":
		class, _ := c.actionClass(b, "merge", "")
		m := githubTarget.FindStringSubmatch(s.Target)
		if mc := c.Autonomy.MergeClass[m[2]]; mc > class {
			class = mc
		}
		return class, "merge at " + short(s.Pre["head_sha"])
	case "iac.apply":
		tool, stack, _ := strings.Cut(s.Target, ":")
		path := s.Args["preview"]
		if path == "" {
			return 9, "apply with no preview, so nothing to classify"
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return 9, "preview: " + err.Error()
		}
		pc, err := checkPlan(data)
		if err != nil {
			return 9, "preview: " + err.Error()
		}
		if pc.Format != tool {
			pc.Problems = append(pc.Problems, fmt.Sprintf("the preview is %s output, the target %s", pc.Format, tool))
		}
		e := s.Args["expect"]
		if e != "" {
			want, err := parseExpect(e)
			if err != nil {
				return 9, "expect: " + err.Error()
			}
			pc.expect(want)
		}
		// The selectors the step carries (AWS_PROFILE, KUBECONFIG, ...) choose
		// what it reaches too, so a production pattern matching one of them
		// makes it production.
		var env map[string]string
		_ = json.Unmarshal([]byte(s.Args["env"]), &env)
		maps.DeleteFunc(env, func(k, _ string) bool { return !slices.Contains(iacEnv, k) })
		where := stack
		for _, k := range sortedKeys(env) {
			where += "/" + env[k]
		}
		pc.classify(c.Autonomy, where)
		pc.Target = stack
		why := "apply on " + targetWord(pc) + ", preview " + counts(pc)
		if len(env) > 0 {
			why += " (with " + strings.Join(func() []string {
				var kv []string
				for _, k := range sortedKeys(env) {
					kv = append(kv, k+"="+env[k])
				}
				return kv
			}(), ", ") + ")"
		}
		if len(pc.Problems) > 0 {
			// gate gives a plan with problems no class: it stops for TJ.
			return 9, why + "; " + strings.Join(pc.Problems, "; ")
		}
		if e == "" {
			// The dial's rule: a step missing a precondition, here the
			// change noted beforehand, is one class up.
			return min(9, pc.Class+1), why + "; no expected counts, so one class up"
		}
		return pc.Class, why + ", as expected"
	case opOpaque:
		return 9, "opaque"
	}
	return 9, "unknown op, so opaque"
}

// counts is a preview's changes, as "create 1, update 2".
func counts(pc PlanCheck) string {
	var out []string
	for _, op := range sortedKeys(pc.Counts) {
		if !slices.Contains(planQuiet, op) {
			out = append(out, fmt.Sprintf("%s %d", op, pc.Counts[op]))
		}
	}
	if len(out) == 0 {
		return "no changes"
	}
	return strings.Join(out, ", ")
}

// planEvent is one line of a plan's events.jsonl. checked and approved come
// from plan check and approve; the executor (executor.go) adds intent and
// outcome around every step, then one of the ends.
type planEvent struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"`
	Class int       `json:"class"`
	Level int       `json:"level"`
	Pass  bool      `json:"pass,omitempty"`
	// Why is what failed a check, or why a plan ended as it did.
	Why     string    `json:"why,omitempty"`
	By      string    `json:"by,omitempty"` // dial or TJ
	Expires time.Time `json:"expires,omitzero"`
	// Step (1-based) and Attempt are an intent's or an outcome's.
	Step    int `json:"step,omitempty"`
	Attempt int `json:"attempt,omitempty"`
	// Result is an outcome's: ok, failed or unknown (it may have happened).
	Result   string `json:"result,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

const (
	planChecked  = "checked"
	planApproved = "approved"
	planIntent   = "intent"
	planOutcome  = "outcome"
	// planRerun starts a new run of a plan whose last run failed: a shim
	// resubmits the same merge once its checks pass. Only the events after
	// the latest rerun are the run's.
	planRerun = "rerun"
	// The ends. done and failed are final; a plan stopped at needs_me or
	// expired continues with plan resume once TJ approves it.
	planDone    = "done"
	planFailed  = "failed"
	planNeedsMe = "needs_me"
	planExpired = "expired"
)

const (
	resultOK      = "ok"
	resultFailed  = "failed"
	resultUnknown = "unknown"
)

func plansDir() string             { return filepath.Join(stateDir(), "plans") }
func planDir(digest string) string { return filepath.Join(plansDir(), digest) }

// storePlan writes p under its digest. The file is immutable: the digest
// names its content, so one already there is left alone.
func storePlan(p Plan, digest string) error {
	path := filepath.Join(planDir(digest), "plan.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// appendPlanEvent adds e to digest's event log and syncs it, so an approval
// that was reported survives a crash.
func appendPlanEvent(digest string, e planEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(planDir(digest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(planDir(digest), "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func readPlanEvents(digest string) ([]planEvent, error) {
	b, err := os.ReadFile(filepath.Join(planDir(digest), "events.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []planEvent
	for _, line := range bytes.Split(b, []byte("\n")) {
		var e planEvent
		if json.Unmarshal(line, &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// loadPlan finds a stored plan by its digest or a unique prefix of 8 or more.
func loadPlan(ref string) (Plan, string, error) {
	if len(ref) < 8 {
		return Plan{}, "", fmt.Errorf("%w: give at least 8 characters of the digest", errPlan)
	}
	entries, err := os.ReadDir(plansDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Plan{}, "", err
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ref) {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		return Plan{}, "", fmt.Errorf("%w: no checked plan %s; run quartermaster plan check first", errPlan, ref)
	case 1:
	default:
		return Plan{}, "", fmt.Errorf("%w: %s names %d plans; give more of the digest", errPlan, ref, len(found))
	}
	b, err := os.ReadFile(filepath.Join(planDir(found[0]), "plan.json"))
	if err != nil {
		return Plan{}, "", err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return Plan{}, "", err
	}
	// The directory name is only a claim; the content is the plan.
	if d, err := p.digest(); err != nil || d != found[0] {
		return Plan{}, "", fmt.Errorf("%w: %s/plan.json doesn't hash to its name", errPlan, short(found[0]))
	}
	return p, found[0], nil
}

// liveApproval is the live approval in events at now, if any: the latest one that
// hasn't expired.
func liveApproval(events []planEvent, now time.Time) (planEvent, bool) {
	for _, e := range slices.Backward(events) {
		if e.Kind == planApproved && now.Before(e.Expires) {
			return e, true
		}
	}
	return planEvent{}, false
}

const planUsage = "usage: quartermaster plan check <file|-> | approve <digest> --by TJ [--for D] | status <digest> | run <file|-> | resume <digest>"

// planCmd is `quartermaster plan`. check classifies a plan, stores it under
// its digest and approves it by the dial when its class is at most the
// bead's level and the budgets allow; otherwise it exits nonzero, naming the
// digest TJ approves. approve is TJ's. status shows a plan and its approval.
func planCmd(cfg Config, show func(string) (Bead, error), pos []string, by string, ttl time.Duration, stdin io.Reader, w io.Writer) error {
	if len(pos) != 2 {
		return errors.New(planUsage)
	}
	if ttl <= 0 {
		ttl = approvalTTL
	}
	now := time.Now()
	switch pos[0] {
	case "check":
		data, err := readPlanArg(pos[1], stdin)
		if err != nil {
			return err
		}
		p, err := parsePlan(data)
		if err != nil {
			return err
		}
		return checkPlanFor(cfg, show, p, now, w)
	case "approve":
		if by != "TJ" {
			return fmt.Errorf("%w: plan approve is TJ's (--by TJ); the dial approves with plan check", errNotAutonomous)
		}
		p, digest, err := loadPlan(pos[1])
		if err != nil {
			return err
		}
		class, _ := classifyPlan(cfg, show, p)
		e := planEvent{At: now, Kind: planApproved, Class: class, Level: -1, Pass: true, By: "TJ", Expires: now.Add(ttl)}
		if err := appendPlanEvent(digest, e); err != nil {
			return err
		}
		fmt.Fprintf(w, "plan %s (%s, class %d) approved by TJ until %s\n", short(digest), p.Bead, class, e.Expires.Format("15:04"))
		if events, _ := readPlanEvents(digest); unfinished(events) || slices.ContainsFunc(events, func(e planEvent) bool { return e.Kind == planNeedsMe || e.Kind == planExpired }) {
			if s := planRunState(events, digest); !strings.HasPrefix(s, "done") && !strings.HasPrefix(s, "failed") {
				fmt.Fprintf(w, "its run stopped; continue it: quartermaster plan resume %s\n", digest)
			}
		}
		return appendJournal(journalRecord{At: now, Kind: journalCheck, Bead: p.Bead, Class: class, Action: "plan", Pass: true, Text: "plan " + short(digest) + " approved by TJ"})
	case "run":
		data, err := readPlanArg(pos[1], stdin)
		if err != nil {
			return err
		}
		return planRun(data, w)
	case "resume":
		return planResume(pos[1], w)
	case "status":
		p, digest, err := loadPlan(pos[1])
		if err != nil {
			return err
		}
		events, err := readPlanEvents(digest)
		if err != nil {
			return err
		}
		b, err := show(p.Bead)
		if err != nil {
			return err
		}
		fmt.Fprint(w, renderPlan(cfg, b, p, digest))
		fmt.Fprintln(w, planState(events, digest, now))
		return nil
	}
	return errors.New(planUsage)
}

func readPlanArg(arg string, stdin io.Reader) ([]byte, error) {
	if arg == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(arg)
}

// classifyPlan is p's class, the highest of its steps', on its bead.
func classifyPlan(cfg Config, show func(string) (Bead, error), p Plan) (int, error) {
	b, err := show(p.Bead)
	if err != nil {
		return 9, err
	}
	class := 0
	for _, s := range p.Steps {
		c, _ := cfg.stepClass(b, s)
		class = max(class, c)
	}
	return class, nil
}

// checkPlanFor is plan check on a parsed plan.
func checkPlanFor(cfg Config, show func(string) (Bead, error), p Plan, now time.Time, w io.Writer) error {
	digest, err := p.digest()
	if err != nil {
		return err
	}
	b, err := show(p.Bead)
	if err != nil {
		return err
	}
	class, _ := classifyPlan(cfg, func(string) (Bead, error) { return b, nil }, p)
	lvl, why := cfg.beadLevel(b, autonomyStopped())
	var fail error
	switch {
	case b.Status == StatusClosed:
		fail = fmt.Errorf("%w: %s is closed", errNotAutonomous, b.ID)
	case class > lvl:
		if why != "" {
			why = ", capped by " + why
		}
		fail = fmt.Errorf("%w: plan %s is class %d, above %s's level %d%s", errNotAutonomous, short(digest), class, b.ID, lvl, why)
	}
	recs, err := readJournal(now.Add(-budgetWindow))
	if err != nil {
		return err
	}
	if spent := budgetSpent(cfg.Autonomy.Budget, recs, b.ID, class, ""); fail == nil && spent != "" {
		fail = fmt.Errorf("%w: plan %s: %s", errNotAutonomous, short(digest), spent)
	}
	if err := storePlan(p, digest); err != nil {
		return err
	}
	e := planEvent{At: now, Kind: planChecked, Class: class, Level: lvl, Pass: fail == nil}
	if fail != nil {
		e.Why = fail.Error()
	}
	errs := []error{appendPlanEvent(digest, e)}
	if fail == nil {
		errs = append(errs, appendPlanEvent(digest, planEvent{At: now, Kind: planApproved, Class: class, Level: lvl, Pass: true, By: "dial", Expires: now.Add(approvalTTL)}))
	}
	rec := journalRecord{At: now, Kind: journalCheck, Bead: b.ID, Class: class, Action: "plan", Pass: fail == nil, Text: "plan " + short(digest)}
	if fail != nil {
		rec.Text = fail.Error()
	}
	errs = append(errs, appendJournal(rec))
	if err := errors.Join(errs...); err != nil {
		return errors.Join(fail, err)
	}
	fmt.Fprint(w, renderPlan(cfg, b, p, digest))
	if fail != nil {
		if failedRun(append(recs, rec), b.ID) && lvl > breakerCap {
			fail = errors.Join(fail, tripBreaker(b.ID, fmt.Sprintf("%d failed checks in a row", breakerRun), now))
		}
		fmt.Fprintf(w, "waiting for TJ: quartermaster plan approve %s --by TJ\n", digest)
		return fail
	}
	fmt.Fprintf(w, "approved by the dial (level %d) until %s; advisory\n", lvl, now.Add(approvalTTL).Format("15:04"))
	return nil
}

// renderPlan is p, step by step with each step's class and why.
func renderPlan(cfg Config, b Bead, p Plan, digest string) string {
	var sb strings.Builder
	top := 0
	var lines []string
	for i, s := range p.Steps {
		c, why := cfg.stepClass(b, s)
		top = max(top, c)
		what := s.Target
		if s.Op == opOpaque || s.Text != "" && what == "" {
			what = strconv.Quote(s.Text)
		}
		lines = append(lines, fmt.Sprintf("  %d. %s %s: class %d (%s)", i+1, s.Op, what, c, why))
	}
	fmt.Fprintf(&sb, "plan %s for %s: class %d, %d step(s)\n", short(digest), p.Bead, top, len(p.Steps))
	for _, l := range lines {
		sb.WriteString(l + "\n")
	}
	fmt.Fprintf(&sb, "digest %s\n", digest)
	return sb.String()
}

// planState is where a plan stands at now: how its run ended or where it
// is, else where its approval stands.
func planState(events []planEvent, digest string, now time.Time) string {
	if s := planRunState(events, digest); s != "" {
		return s
	}
	if a, ok := liveApproval(events, now); ok {
		return fmt.Sprintf("approved by %s until %s", a.By, a.Expires.Format("15:04"))
	}
	for _, e := range slices.Backward(events) {
		switch e.Kind {
		case planApproved:
			return fmt.Sprintf("expired: approved by %s until %s; check it again, or quartermaster plan approve %s --by TJ", e.By, e.Expires.Format("15:04"), digest)
		case planChecked:
			return fmt.Sprintf("waiting for TJ (%s): quartermaster plan approve %s --by TJ", e.Why, digest)
		}
	}
	return "not checked: quartermaster plan check <file>"
}
