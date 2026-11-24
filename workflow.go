package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Durable code-defined rollout workflows built on top of plan.go / executor.go
// and cards.go.
//
// Each workflow run persists under <state dir>/workflows/<id>/:
//   - spec.json: immutable workflow spec, template name, vars, version and canonical SHA-256 digest
//   - events.jsonl: append-only fsynced transition log mirrored to journal.jsonl
//
// Every step kind is strictly typed data (no arbitrary shell evaluation):
//   plan_step, gate_check, approval, wait, milestone, verify.
//
// All approval and execution gates enforce the autonomy dial, budgets,
// digest-bound approval expiry, and preview-change invalidation.

const workflowSchemaVersion = 1

const (
	wfStepPlan      = "plan_step"
	wfStepGateCheck = "gate_check"
	wfStepApproval  = "approval"
	wfStepWait      = "wait"
	wfStepMilestone = "milestone"
	wfStepVerify    = "verify"
)

const (
	wfWaitPRReady      = "pr_ready"
	wfWaitPRMerged     = "pr_merged"
	wfWaitRunCompleted = "run_completed"
	wfWaitSignal       = "signal"
)

const (
	wfVerifyNoDrift    = "no_drift"
	wfVerifyAcceptance = "acceptance"
)

const (
	wfEventStarted      = "started"
	wfEventStepStarted  = "step_started"
	wfEventStepWaiting  = "step_waiting"
	wfEventStepApproved = "step_approved"
	wfEventStepOK       = "step_ok"
	wfEventStepFailed   = "step_failed"
	wfEventCompleted    = "completed"
	wfEventCanceled     = "canceled"
)

const (
	wfStatusPending   = "pending"
	wfStatusRunning   = "running"
	wfStatusWaiting   = "waiting"
	wfStatusCompleted = "completed"
	wfStatusFailed    = "failed"
	wfStatusCanceled  = "canceled"
)

var errWorkflow = errors.New("workflow")

type WorkflowSpec struct {
	Version  int               `json:"version"`
	ID       string            `json:"id,omitempty"`
	Bead     string            `json:"bead"`
	Template string            `json:"template,omitempty"`
	Vars     map[string]string `json:"vars,omitempty"`
	Created  time.Time         `json:"created_at,omitzero"`
	Digest   string            `json:"digest,omitempty"`
	Steps    []WorkflowStep    `json:"steps"`
}

type WorkflowStep struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Class int    `json:"class,omitempty"`

	// plan_step
	Plan *Step `json:"plan,omitempty"`

	// gate_check and verify(no_drift)
	Preview string `json:"preview,omitempty"`
	Target  string `json:"target,omitempty"`
	Expect  string `json:"expect,omitempty"`

	// approval
	Action      string `json:"action,omitempty"`
	AlwaysAsk   bool   `json:"always_ask,omitempty"`
	Question    string `json:"question,omitempty"`
	ClassReason string `json:"class_reason,omitempty"`
	GateStep    string `json:"gate_step,omitempty"`
	For         string `json:"for,omitempty"`

	// wait
	WaitKind string `json:"wait_kind,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	RunURL   string `json:"run_url,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Optional bool   `json:"optional,omitempty"`

	// milestone
	Phase   string `json:"phase,omitempty"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary,omitempty"`

	// verify
	VerifyKind string `json:"verify_kind,omitempty"`
}

type workflowEvent struct {
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	Step       int       `json:"step,omitempty"`
	StepName   string    `json:"step_name,omitempty"`
	Class      int       `json:"class,omitempty"`
	Level      int       `json:"level,omitempty"`
	By         string    `json:"by,omitempty"`
	Digest     string    `json:"digest,omitempty"`
	PlanDigest string    `json:"plan_digest,omitempty"`
	CardHash   string    `json:"card_hash,omitempty"`
	Expires    time.Time `json:"expires,omitzero"`
	Summary    string    `json:"summary,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	Why        string    `json:"why,omitempty"`
}

type wfStepState struct {
	Index          int       `json:"index"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	Class          int       `json:"class"`
	Digest         string    `json:"digest,omitempty"`
	PlanDigest     string    `json:"plan_digest,omitempty"`
	CardHash       string    `json:"card_hash,omitempty"`
	StartedAt      time.Time `json:"started_at,omitzero"`
	ApprovedBy     string    `json:"approved_by,omitempty"`
	ApprovedAt     time.Time `json:"approved_at,omitzero"`
	ApprovalExp    time.Time `json:"approval_expires,omitzero"`
	ApprovalDigest string    `json:"approval_digest,omitempty"`
	Evidence       string    `json:"evidence,omitempty"`
	Why            string    `json:"why,omitempty"`
}

type workflowState struct {
	ID          string        `json:"id"`
	Bead        string        `json:"bead"`
	Template    string        `json:"template,omitempty"`
	Digest      string        `json:"digest"`
	Status      string        `json:"status"`
	CurrentStep int           `json:"current_step"`
	Why         string        `json:"why,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	Steps       []wfStepState `json:"steps"`
}

type workflowTemplateInfo struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	RequiredVars []string `json:"required_vars"`
	OptionalVars []string `json:"optional_vars,omitempty"`
	Steps        []string `json:"steps"`
}

var workflowTemplates = []workflowTemplateInfo{
	{
		Name:         "pr-merge-verify",
		Description:  "Wait for PR readiness, gate merge approval against the autonomy dial, merge through the checked executor, record merge milestone, wait for an optional post-merge Actions run, and record verified milestone.",
		RequiredVars: []string{"pr_url"},
		OptionalVars: []string{"head_sha", "method", "run_url"},
		Steps:        []string{"wait(pr_ready)", "approval(merge)", "plan_step(github.pr.merge)", "milestone(merged)", "wait(run_completed, optional)", "milestone(verified)"},
	},
	{
		Name:         "iac-rollout",
		Description:  "Gate-check an IaC preview against expected counts, bind approval to the preview digest and computed class, apply through the checked executor, record applied milestone, run a post-apply no-drift check, and record verified milestone.",
		RequiredVars: []string{"target", "preview"},
		OptionalVars: []string{"expect", "rev", "repo", "dir", "stack", "workdir", "planfile", "cas", "verify_preview"},
		Steps:        []string{"gate_check(preview)", "approval(apply)", "plan_step(iac.apply)", "milestone(applied)", "verify(no_drift)", "milestone(verified)"},
	},
}

type workflowEnv struct {
	cfg            Config
	show           func(id string) (Bead, error)
	note           func(id, text string) error
	setStatus      func(id, status string) error
	viewPR         func(ctx context.Context, url string) (PR, error)
	viewPRGate     func(ctx context.Context, pr PR, now time.Time) (MergeGate, bool, error)
	viewRun        func(ctx context.Context, repo, id string) (Run, error)
	execPlan       func(ctx context.Context, digest string) (string, error)
	refreshPreview func(ctx context.Context, step WorkflowStep) ([]byte, error)
	runVerify      func(ctx context.Context, b Bead) (verifierResult, error)
	now            func() time.Time
	logf           func(format string, a ...any)
}

func liveWorkflowEnv(cfg Config) workflowEnv {
	bd := Beads{actor: "workflow"}
	gh := GH{repo: cfg.Repo}
	return workflowEnv{
		cfg:       cfg,
		show:      bd.Show,
		note:      bd.Note,
		setStatus: bd.SetStatus,
		viewPR: func(_ context.Context, url string) (PR, error) {
			pr, err := gh.View(url, true)
			if err == nil {
				pr.DetailsFresh = true
			}
			return pr, err
		},
		viewPRGate: func(_ context.Context, pr PR, now time.Time) (MergeGate, bool, error) {
			gates, err := gh.Gates([]PR{pr}, now)
			if err != nil {
				return MergeGate{}, false, err
			}
			g, ok := gates[pr.URL]
			return g, ok, nil
		},
		viewRun: func(_ context.Context, repo, id string) (Run, error) {
			return gh.RunView(repo, id)
		},
		execPlan: func(ctx context.Context, digest string) (string, error) {
			if _, err := execCall(controlSocket(), execRequest{Ping: true}, time.Second); err == nil {
				resp, err := execCall(controlSocket(), execRequest{Resume: digest}, 0)
				if err != nil {
					return "", err
				}
				return resp.State, nil
			}
			x := newExecutor(cfg, log.New(io.Discard, "", 0))
			return x.execPlan(ctx, digest)
		},
		runVerify: func(ctx context.Context, b Bead) (verifierResult, error) {
			out, err := runVerifierContext(ctx, cfg.Verifier, expandHome(cfg.repoFor(b)), verifierPrompt(cfg, b))
			if err != nil {
				return verifierResult{}, err
			}
			return parseVerdict(out)
		},
		now:  time.Now,
		logf: func(string, ...any) {},
	}
}

func (e workflowEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e workflowEnv) cardEnv() cardEnv {
	return cardEnv{
		show:      e.show,
		note:      e.note,
		setStatus: e.setStatus,
		now:       e.now,
	}
}

func workflowsDir() string { return filepath.Join(stateDir(), "workflows") }

func workflowDir(id string) (string, error) {
	if !beadName.MatchString(id) {
		return "", fmt.Errorf("%w: invalid workflow id %q", errWorkflow, id)
	}
	return filepath.Join(workflowsDir(), id), nil
}

func lockWorkflow(id string) (func(), error) {
	if !beadName.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid workflow id %q", errWorkflow, id)
	}
	dir := filepath.Join(runDir(), "locks")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "wf-"+id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: workflow %s is already advancing", errBusy, id)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func newWorkflowID(now time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("wf-%s-%s", now.UTC().Format("20060102-150405"), hex.EncodeToString(b))
}

func canonicalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(raw); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// digest hashes the immutable definition of spec (excluding runtime ID/Created/Digest).
func (s WorkflowSpec) digest() (string, error) {
	body := struct {
		Version  int               `json:"version"`
		Bead     string            `json:"bead"`
		Template string            `json:"template,omitempty"`
		Vars     map[string]string `json:"vars,omitempty"`
		Steps    []WorkflowStep    `json:"steps"`
	}{
		Version:  s.Version,
		Bead:     s.Bead,
		Template: s.Template,
		Vars:     s.Vars,
		Steps:    s.Steps,
	}
	b, err := canonicalJSON(body)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

func parseWorkflowSpec(data []byte, beadOverride string) (WorkflowSpec, error) {
	var spec WorkflowSpec
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("%w: invalid spec JSON: %v", errWorkflow, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return spec, fmt.Errorf("%w: unexpected trailing data in spec JSON", errWorkflow)
	}
	if spec.Version == 0 {
		spec.Version = workflowSchemaVersion
	}
	if spec.Version != workflowSchemaVersion {
		return spec, fmt.Errorf("%w: unsupported workflow version %d (want %d)", errWorkflow, spec.Version, workflowSchemaVersion)
	}
	if beadOverride != "" {
		if spec.Bead != "" && spec.Bead != beadOverride {
			return spec, fmt.Errorf("%w: spec bead %q does not match %q", errWorkflow, spec.Bead, beadOverride)
		}
		spec.Bead = beadOverride
	}
	if err := validateWorkflowSpec(&spec); err != nil {
		return spec, err
	}
	return spec, nil
}

func validateWorkflowSpec(spec *WorkflowSpec) error {
	if !beadName.MatchString(spec.Bead) {
		return fmt.Errorf("%w: invalid or missing bead id %q", errWorkflow, spec.Bead)
	}
	if len(spec.Steps) == 0 {
		return fmt.Errorf("%w: workflow must define at least one step", errWorkflow)
	}
	seenNames := map[string]int{}
	for i := range spec.Steps {
		s := &spec.Steps[i]
		if strings.TrimSpace(s.Name) == "" {
			s.Name = fmt.Sprintf("step-%d", i+1)
		} else {
			s.Name = strings.TrimSpace(s.Name)
		}
		if prev, dup := seenNames[s.Name]; dup {
			return fmt.Errorf("%w: duplicate step name %q (steps %d and %d)", errWorkflow, s.Name, prev, i+1)
		}
		seenNames[s.Name] = i + 1
		if s.Class < 0 || s.Class > 9 {
			return fmt.Errorf("%w: step %d (%s): class %d must be 0-9", errWorkflow, i+1, s.Name, s.Class)
		}
		bad := func(format string, a ...any) error {
			return fmt.Errorf("%w: step %d (%s): %s", errWorkflow, i+1, s.Name, fmt.Sprintf(format, a...))
		}
		switch s.Kind {
		case wfStepPlan:
			if s.Plan == nil {
				return bad("plan_step requires plan")
			}
			if s.Plan.Op == "" {
				return bad("plan_step requires plan.op")
			}
			if s.Plan.Class < 0 || s.Plan.Class > 9 {
				return bad("plan_step plan.class must be 0-9")
			}
			switch s.Plan.Op {
			case "github.pr.merge", "github.pr.automerge":
				if !githubTarget.MatchString(s.Plan.Target) {
					return bad("target must be github:owner/repo#N, not %q", s.Plan.Target)
				}
				if !fullSHA.MatchString(s.Plan.Pre["head_sha"]) {
					return bad("needs 40-char pre.head_sha")
				}
			case "iac.apply":
				if tool, stack, _ := strings.Cut(s.Plan.Target, ":"); (tool != "pulumi" && tool != "tofu") || stack == "" {
					return bad("target must be pulumi:<stack> or tofu:<name>, not %q", s.Plan.Target)
				}
				if rev := s.Plan.Pre["rev"]; rev != "" && !fullSHA.MatchString(rev) {
					return bad("pre.rev must be a 40-char commit SHA")
				}
				if e := s.Plan.Args["expect"]; e != "" {
					if _, err := parseExpect(e); err != nil {
						return bad("invalid args.expect: %v", err)
					}
				}
			case opOpaque:
				if s.Plan.Text == "" {
					return bad("opaque plan_step requires text")
				}
			}
		case wfStepGateCheck:
			if strings.TrimSpace(s.Preview) == "" {
				return bad("gate_check requires preview")
			}
			if s.Expect != "" {
				if _, err := parseExpect(s.Expect); err != nil {
					return bad("invalid expect: %v", err)
				}
			}
		case wfStepApproval:
			if s.Action != "" {
				if _, ok := actionClasses[s.Action]; !ok {
					return bad("unknown approval action %q", s.Action)
				}
			}
			if s.For != "" {
				d, err := time.ParseDuration(s.For)
				if err != nil || d <= 0 {
					return bad("invalid for duration %q", s.For)
				}
			}
			if s.GateStep != "" {
				idx, ok := seenNames[s.GateStep]
				if !ok || idx >= i+1 || spec.Steps[idx-1].Kind != wfStepGateCheck {
					return bad("gate_step %q must name an earlier gate_check step", s.GateStep)
				}
			}
		case wfStepWait:
			switch s.WaitKind {
			case wfWaitPRReady, wfWaitPRMerged:
				if s.PRURL == "" && !s.Optional {
					return bad("%s wait requires pr_url", s.WaitKind)
				}
				if s.PRURL != "" && !prURLRE.MatchString(s.PRURL) {
					return bad("invalid pr_url %q", s.PRURL)
				}
				if s.HeadSHA != "" && !fullSHA.MatchString(s.HeadSHA) {
					return bad("head_sha must be a 40-char commit SHA")
				}
			case wfWaitRunCompleted:
				if s.RunURL == "" && !s.Optional {
					return bad("run_completed wait requires run_url (or set optional: true)")
				}
				if s.RunURL != "" && !runURL.MatchString(s.RunURL) {
					return bad("invalid run_url %q", s.RunURL)
				}
			case wfWaitSignal:
			default:
				return bad("unknown wait_kind %q (want pr_ready, pr_merged, run_completed or signal)", s.WaitKind)
			}
		case wfStepMilestone:
			if strings.TrimSpace(s.Phase) == "" {
				return bad("milestone requires phase")
			}
			if strings.TrimSpace(s.Summary) == "" {
				return bad("milestone requires summary")
			}
			if s.Status != "" && !validMilestoneStatuses[strings.ToLower(strings.TrimSpace(s.Status))] {
				return bad("invalid milestone status %q", s.Status)
			}
		case wfStepVerify:
			if s.VerifyKind == "" {
				if s.Preview != "" {
					s.VerifyKind = wfVerifyNoDrift
				} else {
					s.VerifyKind = wfVerifyAcceptance
				}
			}
			if s.VerifyKind != wfVerifyNoDrift && s.VerifyKind != wfVerifyAcceptance {
				return bad("unknown verify_kind %q (want no_drift or acceptance)", s.VerifyKind)
			}
		default:
			return bad("unknown step kind %q (want plan_step, gate_check, approval, wait, milestone or verify)", s.Kind)
		}
	}
	return nil
}

// buildTemplateSpec instantiates one of the built-in code-defined rollout
// workflow templates (pr-merge-verify or iac-rollout).
func buildTemplateSpec(ctx context.Context, env workflowEnv, bead, template string, vars map[string]string) (WorkflowSpec, error) {
	v := maps.Clone(vars)
	if v == nil {
		v = map[string]string{}
	}
	switch template {
	case "pr-merge-verify":
		prU := strings.TrimSpace(v["pr_url"])
		if prU == "" {
			return WorkflowSpec{}, fmt.Errorf("%w: pr-merge-verify requires --var pr_url=https://github.com/owner/repo/pull/N", errWorkflow)
		}
		parsedPR, err := parsePRURL(prU, "")
		if err != nil {
			return WorkflowSpec{}, fmt.Errorf("%w: %v", errWorkflow, err)
		}
		headSHA := strings.TrimSpace(v["head_sha"])
		if headSHA == "" && env.viewPR != nil {
			if pr, err := env.viewPR(ctx, prU); err == nil && fullSHA.MatchString(pr.HeadSHA) {
				headSHA = pr.HeadSHA
				v["head_sha"] = headSHA
			}
		}
		if !fullSHA.MatchString(headSHA) {
			return WorkflowSpec{}, fmt.Errorf("%w: pr-merge-verify requires a 40-char --var head_sha=<sha> (or resolvable PR head)", errWorkflow)
		}
		method := strings.TrimSpace(v["method"])
		if method == "" {
			method = "squash"
			v["method"] = method
		}
		if method != "squash" && method != "merge" && method != "rebase" {
			return WorkflowSpec{}, fmt.Errorf("%w: unsupported merge method %q", errWorkflow, method)
		}
		runU := strings.TrimSpace(v["run_url"])
		mergeStep := Step{
			Op:     "github.pr.merge",
			Target: parsedPR.target(),
			Args:   map[string]string{"method": method},
			Pre:    map[string]string{"head_sha": headSHA},
		}
		spec := WorkflowSpec{
			Version:  workflowSchemaVersion,
			Bead:     bead,
			Template: template,
			Vars:     v,
			Steps: []WorkflowStep{
				{
					Name:     "wait_pr_ready",
					Kind:     wfStepWait,
					WaitKind: wfWaitPRReady,
					PRURL:    prU,
					HeadSHA:  headSHA,
				},
				{
					Name:        "approve_merge",
					Kind:        wfStepApproval,
					Action:      "merge",
					Class:       4,
					Question:    fmt.Sprintf("Approve merging %s at %s?", prU, short(headSHA)),
					ClassReason: "merge to default branch",
				},
				{
					Name: "merge_pr",
					Kind: wfStepPlan,
					Plan: &mergeStep,
				},
				{
					Name:    "milestone_merged",
					Kind:    wfStepMilestone,
					Phase:   "merged",
					Status:  "ok",
					Summary: fmt.Sprintf("Merged %s at %s", prU, short(headSHA)),
				},
				{
					Name:     "wait_run_completed",
					Kind:     wfStepWait,
					WaitKind: wfWaitRunCompleted,
					RunURL:   runU,
					Optional: true,
				},
				{
					Name:    "milestone_verified",
					Kind:    wfStepMilestone,
					Phase:   "verified",
					Status:  "ok",
					Summary: fmt.Sprintf("Verified rollout for %s", prU),
				},
			},
		}
		if err := validateWorkflowSpec(&spec); err != nil {
			return WorkflowSpec{}, err
		}
		return spec, nil

	case "iac-rollout":
		target := strings.TrimSpace(v["target"])
		preview := strings.TrimSpace(v["preview"])
		if target == "" {
			return WorkflowSpec{}, fmt.Errorf("%w: iac-rollout requires --var target=pulumi:<stack>|tofu:<name>", errWorkflow)
		}
		if preview == "" {
			return WorkflowSpec{}, fmt.Errorf("%w: iac-rollout requires --var preview=<path>", errWorkflow)
		}
		tool, stack, ok := strings.Cut(target, ":")
		if !ok || (tool != "pulumi" && tool != "tofu") || stack == "" {
			return WorkflowSpec{}, fmt.Errorf("%w: target must be pulumi:<stack> or tofu:<name>, not %q", errWorkflow, target)
		}
		expect := strings.TrimSpace(v["expect"])
		args := map[string]string{"preview": preview}
		if expect != "" {
			args["expect"] = expect
		}
		for _, k := range []string{"repo", "dir", "stack", "workdir", "planfile", "cas", "env"} {
			if val := strings.TrimSpace(v[k]); val != "" {
				args[k] = val
			}
		}
		if tool == "pulumi" && args["stack"] == "" {
			args["stack"] = stack
		}
		pre := map[string]string{}
		if rev := strings.TrimSpace(v["rev"]); rev != "" {
			pre["rev"] = rev
		}
		if pfd := strings.TrimSpace(v["planfile_digest"]); pfd != "" {
			pre["planfile_digest"] = pfd
		}
		applyStep := Step{
			Op:     "iac.apply",
			Target: target,
			Args:   args,
			Pre:    pre,
		}
		verifyPreview := strings.TrimSpace(v["verify_preview"])
		spec := WorkflowSpec{
			Version:  workflowSchemaVersion,
			Bead:     bead,
			Template: template,
			Vars:     v,
			Steps: []WorkflowStep{
				{
					Name:    "gate_preview",
					Kind:    wfStepGateCheck,
					Preview: preview,
					Target:  stack,
					Expect:  expect,
				},
				{
					Name:        "approve_apply",
					Kind:        wfStepApproval,
					Action:      "apply",
					Target:      stack,
					GateStep:    "gate_preview",
					Question:    fmt.Sprintf("Approve IaC apply on %s?", target),
					ClassReason: "IaC apply on " + target,
				},
				{
					Name: "apply_iac",
					Kind: wfStepPlan,
					Plan: &applyStep,
				},
				{
					Name:    "milestone_applied",
					Kind:    wfStepMilestone,
					Phase:   "applied",
					Status:  "ok",
					Summary: fmt.Sprintf("Applied %s", target),
				},
				{
					Name:       "verify_no_drift",
					Kind:       wfStepVerify,
					VerifyKind: wfVerifyNoDrift,
					Preview:    verifyPreview,
					Target:     stack,
				},
				{
					Name:    "milestone_verified",
					Kind:    wfStepMilestone,
					Phase:   "verified",
					Status:  "ok",
					Summary: fmt.Sprintf("Verified no drift on %s", target),
				},
			},
		}
		if err := validateWorkflowSpec(&spec); err != nil {
			return WorkflowSpec{}, err
		}
		return spec, nil

	default:
		return WorkflowSpec{}, fmt.Errorf("%w: unknown template %q (run `quartermaster workflow list-templates`)", errWorkflow, template)
	}
}

func storeWorkflowSpec(spec WorkflowSpec) error {
	dir, err := workflowDir(spec.ID)
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "spec.json")
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("%w: workflow %s already exists", errWorkflow, spec.ID)
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicPrivate(p, append(b, '\n'))
}

func appendWorkflowEvent(spec WorkflowSpec, e workflowEvent) error {
	dir, err := workflowDir(spec.ID)
	if err != nil {
		return err
	}
	if err := appendPrivateJSONL(filepath.Join(dir, "events.jsonl"), e); err != nil {
		return err
	}
	text := fmt.Sprintf("workflow %s %s", spec.ID, e.Kind)
	if e.Step > 0 {
		text = fmt.Sprintf("workflow %s step %d (%s) %s", spec.ID, e.Step, e.StepName, e.Kind)
	}
	for _, s := range []string{e.Evidence, e.Why} {
		if s != "" {
			text += ": " + shortTitle(s, 140)
		}
	}
	cls := e.Class
	if cls == 0 && e.Kind != wfEventStepApproved && e.Kind != wfEventStepWaiting && e.Kind != wfEventStepOK {
		cls = -1
	}
	rec := journalRecord{
		At:     e.At,
		Kind:   journalWorkflow,
		Bead:   spec.Bead,
		Class:  cls,
		Action: "workflow",
		Pass:   e.Kind == wfEventCompleted || e.Kind == wfEventStepOK || e.Kind == wfEventStepApproved,
		Text:   text,
	}
	return appendJournal(rec)
}

func readWorkflowEvents(id string) ([]workflowEvent, error) {
	dir, err := workflowDir(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []workflowEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e workflowEvent
		if json.Unmarshal(line, &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func loadWorkflow(ref string) (WorkflowSpec, []workflowEvent, error) {
	if strings.TrimSpace(ref) == "" {
		return WorkflowSpec{}, nil, fmt.Errorf("%w: workflow id is required", errWorkflow)
	}
	entries, err := os.ReadDir(workflowsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return WorkflowSpec{}, nil, fmt.Errorf("%w: no workflow %s", errWorkflow, ref)
	}
	if err != nil {
		return WorkflowSpec{}, nil, err
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() || !beadName.MatchString(e.Name()) {
			continue
		}
		if e.Name() == ref {
			found = []string{e.Name()}
			break
		}
		if len(ref) >= 4 && strings.HasPrefix(e.Name(), ref) {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		return WorkflowSpec{}, nil, fmt.Errorf("%w: no workflow %s", errWorkflow, ref)
	case 1:
	default:
		return WorkflowSpec{}, nil, fmt.Errorf("%w: %s matches %d workflows; give a longer prefix", errWorkflow, ref, len(found))
	}
	dir, err := workflowDir(found[0])
	if err != nil {
		return WorkflowSpec{}, nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "spec.json"))
	if err != nil {
		return WorkflowSpec{}, nil, err
	}
	var spec WorkflowSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return WorkflowSpec{}, nil, fmt.Errorf("%w: %s/spec.json: %v", errWorkflow, found[0], err)
	}
	if spec.ID != found[0] {
		return WorkflowSpec{}, nil, fmt.Errorf("%w: %s/spec.json id mismatch (%s)", errWorkflow, found[0], spec.ID)
	}
	wantDigest, err := spec.digest()
	if err != nil || wantDigest != spec.Digest {
		return WorkflowSpec{}, nil, fmt.Errorf("%w: %s/spec.json digest mismatch", errWorkflow, found[0])
	}
	events, err := readWorkflowEvents(spec.ID)
	if err != nil {
		return WorkflowSpec{}, nil, err
	}
	return spec, events, nil
}

func listWorkflowSpecs() ([]WorkflowSpec, error) {
	entries, err := os.ReadDir(workflowsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var specs []WorkflowSpec
	for _, e := range entries {
		if !e.IsDir() || !beadName.MatchString(e.Name()) {
			continue
		}
		spec, _, err := loadWorkflow(e.Name())
		if err != nil {
			continue
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool {
		if !specs[i].Created.Equal(specs[j].Created) {
			return specs[i].Created.After(specs[j].Created)
		}
		return specs[i].ID > specs[j].ID
	})
	return specs, nil
}

// reconstructWorkflow derives the current workflow state purely from spec and
// the fsynced events.jsonl log.
func reconstructWorkflow(spec WorkflowSpec, events []workflowEvent) workflowState {
	st := workflowState{
		ID:        spec.ID,
		Bead:      spec.Bead,
		Template:  spec.Template,
		Digest:    spec.Digest,
		Status:    wfStatusPending,
		CreatedAt: spec.Created,
		UpdatedAt: spec.Created,
		Steps:     make([]wfStepState, len(spec.Steps)),
	}
	for i, s := range spec.Steps {
		st.Steps[i] = wfStepState{
			Index:  i + 1,
			Name:   s.Name,
			Kind:   s.Kind,
			Status: wfStatusPending,
			Class:  s.Class,
		}
	}
	for _, e := range events {
		if !e.At.IsZero() {
			st.UpdatedAt = e.At
		}
		var ss *wfStepState
		if e.Step >= 1 && e.Step <= len(st.Steps) {
			ss = &st.Steps[e.Step-1]
		}
		switch e.Kind {
		case wfEventStarted:
			st.Status = wfStatusRunning
		case wfEventStepStarted:
			if ss != nil {
				ss.Status = wfStatusRunning
				ss.StartedAt = e.At
				st.CurrentStep = e.Step
			}
			st.Status = wfStatusRunning
			st.Why = ""
		case wfEventStepWaiting:
			if ss != nil {
				ss.Status = wfStatusWaiting
				if e.Class > ss.Class {
					ss.Class = e.Class
				}
				if e.Digest != "" {
					ss.Digest = e.Digest
				}
				if e.PlanDigest != "" {
					ss.PlanDigest = e.PlanDigest
				}
				if e.CardHash != "" {
					ss.CardHash = e.CardHash
				}
				ss.Why = e.Why
				st.CurrentStep = e.Step
			}
			st.Status = wfStatusWaiting
			st.Why = e.Why
		case wfEventStepApproved:
			if ss != nil {
				ss.ApprovedBy = e.By
				ss.ApprovedAt = e.At
				ss.ApprovalExp = e.Expires
				ss.ApprovalDigest = e.Digest
				if e.Class > ss.Class {
					ss.Class = e.Class
				}
				if ss.Status == wfStatusWaiting {
					ss.Status = "approved"
					ss.Why = ""
				}
			}
			if st.Status == wfStatusWaiting {
				st.Status = wfStatusRunning
				st.Why = ""
			}
		case wfEventStepOK:
			if ss != nil {
				ss.Status = "ok"
				if e.Class > ss.Class {
					ss.Class = e.Class
				}
				if e.Digest != "" {
					ss.Digest = e.Digest
				}
				if e.PlanDigest != "" {
					ss.PlanDigest = e.PlanDigest
				}
				ss.Evidence = e.Evidence
				ss.Why = ""
			}
			st.Status = wfStatusRunning
			st.Why = ""
		case wfEventStepFailed:
			if ss != nil {
				ss.Status = wfStatusFailed
				if e.Class > ss.Class {
					ss.Class = e.Class
				}
				if e.Digest != "" {
					ss.Digest = e.Digest
				}
				if e.PlanDigest != "" {
					ss.PlanDigest = e.PlanDigest
				}
				ss.Why = e.Why
				st.CurrentStep = e.Step
			}
			st.Status = wfStatusFailed
			st.Why = e.Why
		case wfEventCompleted:
			st.Status = wfStatusCompleted
			st.CurrentStep = 0
			st.Why = ""
		case wfEventCanceled:
			st.Status = wfStatusCanceled
			st.CurrentStep = 0
			st.Why = e.Why
		}
	}
	return st
}

// startWorkflow validates, persists spec.json + started event, and advances
// through any immediately executable steps until parked or finished.
func startWorkflow(ctx context.Context, env workflowEnv, spec WorkflowSpec) (workflowState, error) {
	if env.show != nil {
		b, err := env.show(spec.Bead)
		if err != nil {
			return workflowState{}, err
		}
		if b.Status == StatusClosed {
			return workflowState{}, fmt.Errorf("%w: %s is closed", errWorkflow, spec.Bead)
		}
	}
	if err := validateWorkflowSpec(&spec); err != nil {
		return workflowState{}, err
	}
	now := env.clock().UTC()
	spec.Version = workflowSchemaVersion
	spec.Created = now
	if spec.ID == "" {
		spec.ID = newWorkflowID(now)
	}
	d, err := spec.digest()
	if err != nil {
		return workflowState{}, err
	}
	spec.Digest = d
	if err := storeWorkflowSpec(spec); err != nil {
		return workflowState{}, err
	}
	if err := appendWorkflowEvent(spec, workflowEvent{
		At:     now,
		Kind:   wfEventStarted,
		Digest: spec.Digest,
	}); err != nil {
		return workflowState{}, err
	}
	return advanceWorkflow(ctx, env, spec.ID)
}

// cancelWorkflow marks an active workflow as canceled.
func cancelWorkflow(env workflowEnv, ref, reason, by string) (workflowState, error) {
	spec, events, err := loadWorkflow(ref)
	if err != nil {
		return workflowState{}, err
	}
	unlock, err := lockWorkflow(spec.ID)
	if err != nil {
		return workflowState{}, err
	}
	defer unlock()
	events, err = readWorkflowEvents(spec.ID)
	if err != nil {
		return workflowState{}, err
	}
	st := reconstructWorkflow(spec, events)
	if st.Status == wfStatusCompleted || st.Status == wfStatusCanceled || st.Status == wfStatusFailed {
		return st, fmt.Errorf("%w: %s is already %s", errWorkflow, spec.ID, st.Status)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "canceled"
	}
	now := env.clock().UTC()
	if err := appendWorkflowEvent(spec, workflowEvent{
		At:   now,
		Kind: wfEventCanceled,
		By:   by,
		Why:  reason,
	}); err != nil {
		return st, err
	}
	if env.note != nil {
		_ = env.note(spec.Bead, fmt.Sprintf("WORKFLOW CANCELED [%s]: %s", spec.ID, oneLine(reason)))
	}
	events, err = readWorkflowEvents(spec.ID)
	if err != nil {
		return st, err
	}
	return reconstructWorkflow(spec, events), nil
}

// approveWorkflow records a digest-bound TJ approval on an approval (or signal)
// step and advances the workflow.
func approveWorkflow(ctx context.Context, env workflowEnv, ref string, stepNum int, by string, ttl time.Duration) (workflowState, error) {
	if by != "TJ" {
		return workflowState{}, fmt.Errorf("%w: workflow approve is TJ's (--by TJ); the dial evaluates during advance", errNotAutonomous)
	}
	if ttl <= 0 {
		ttl = approvalTTL
	}
	spec, _, err := loadWorkflow(ref)
	if err != nil {
		return workflowState{}, err
	}
	unlock, err := lockWorkflow(spec.ID)
	if err != nil {
		return workflowState{}, err
	}
	events, err := readWorkflowEvents(spec.ID)
	if err != nil {
		unlock()
		return workflowState{}, err
	}
	st := reconstructWorkflow(spec, events)
	if st.Status == wfStatusCompleted || st.Status == wfStatusCanceled || st.Status == wfStatusFailed {
		unlock()
		return st, fmt.Errorf("%w: %s is %s", errWorkflow, spec.ID, st.Status)
	}
	if stepNum < 1 || stepNum > len(spec.Steps) {
		unlock()
		return st, fmt.Errorf("%w: step %d out of range 1-%d", errWorkflow, stepNum, len(spec.Steps))
	}
	step := spec.Steps[stepNum-1]
	if step.Kind != wfStepApproval && !(step.Kind == wfStepWait && step.WaitKind == wfWaitSignal) {
		unlock()
		return st, fmt.Errorf("%w: step %d (%s) is %s, not an approval or signal step", errWorkflow, stepNum, step.Name, step.Kind)
	}
	var b Bead
	if env.show != nil {
		b, err = env.show(spec.Bead)
		if err != nil {
			unlock()
			return st, err
		}
		if b.Status == StatusClosed {
			unlock()
			return st, fmt.Errorf("%w: %s is closed", errWorkflow, spec.Bead)
		}
	}
	now := env.clock().UTC()
	if cur := st.Steps[stepNum-1]; cur.Status == "ok" && now.Before(cur.ApprovalExp) {
		unlock()
		return st, fmt.Errorf("%w: step %d (%s) is already approved and completed", errWorkflow, stepNum, step.Name)
	}
	effClass := step.Class
	boundDigest := ""
	if step.Kind == wfStepApproval {
		ac, err := resolveApprovalContext(env, spec, st, stepNum-1, b)
		if err != nil {
			unlock()
			return st, err
		}
		effClass = ac.class
		boundDigest = ac.digest
	} else {
		boundDigest = sha256Hex([]byte(spec.Digest + ":" + step.Name))
	}
	if err := appendWorkflowEvent(spec, workflowEvent{
		At:       now,
		Kind:     wfEventStepApproved,
		Step:     stepNum,
		StepName: step.Name,
		Class:    effClass,
		Level:    -1,
		By:       "TJ",
		Digest:   boundDigest,
		Expires:  now.Add(ttl),
		Evidence: fmt.Sprintf("approved by TJ until %s", now.Add(ttl).Format("15:04")),
	}); err != nil {
		unlock()
		return st, err
	}
	if env.note != nil {
		_ = env.note(spec.Bead, fmt.Sprintf("WORKFLOW APPROVED [%s] step %d (%s) digest %s by TJ until %s", spec.ID, stepNum, step.Name, short(boundDigest), now.Add(ttl).Format("15:04")))
	}
	if b.Status == StatusNeedsMe && env.setStatus != nil {
		_ = env.setStatus(spec.Bead, StatusInProgress)
	}
	unlock()
	return advanceWorkflow(ctx, env, spec.ID)
}

type approvalContext struct {
	class     int
	digest    string
	evidence  *CardEvidence
	gateIdx   int // -1 if none
	planIdx   int // -1 if none
	classNote string
}

func findPrecedingGateStep(spec WorkflowSpec, idx int, gateName string) int {
	if gateName != "" {
		for i := idx - 1; i >= 0; i-- {
			if spec.Steps[i].Name == gateName && spec.Steps[i].Kind == wfStepGateCheck {
				return i
			}
		}
		return -1
	}
	for i := idx - 1; i >= 0; i-- {
		if spec.Steps[i].Kind == wfStepGateCheck {
			return i
		}
	}
	return -1
}

func findPrecedingApprovalStep(spec WorkflowSpec, idx int) int {
	for i := idx - 1; i >= 0; i-- {
		if spec.Steps[i].Kind == wfStepApproval {
			return i
		}
	}
	return -1
}

func findFollowingPlanStep(spec WorkflowSpec, idx int) int {
	for i := idx + 1; i < len(spec.Steps); i++ {
		if spec.Steps[i].Kind == wfStepPlan {
			return i
		}
		if spec.Steps[i].Kind == wfStepApproval {
			break
		}
	}
	return -1
}

func resolveApprovalContext(env workflowEnv, spec WorkflowSpec, st workflowState, idx int, b Bead) (approvalContext, error) {
	step := spec.Steps[idx]
	res := approvalContext{
		class:   step.Class,
		gateIdx: -1,
		planIdx: -1,
	}
	if step.Action != "" {
		if c, err := env.cfg.actionClass(b, step.Action, step.Target, ""); err == nil {
			res.class = max(res.class, c)
		}
	}
	ev := &CardEvidence{}
	hasEv := false
	if prU := spec.Vars["pr_url"]; prU != "" {
		ev.PRURL = prU
		hasEv = true
	}
	if runU := spec.Vars["run_url"]; runU != "" {
		ev.RunURLs = []string{runU}
		hasEv = true
	}

	gateIdx := findPrecedingGateStep(spec, idx, step.GateStep)
	if gateIdx >= 0 {
		res.gateIdx = gateIdx
		gateStep := spec.Steps[gateIdx]
		gateSt := st.Steps[gateIdx]
		if gateSt.Status != "ok" || gateSt.Digest == "" {
			return res, fmt.Errorf("%w: approval step %d depends on gate_check step %d (%s) which has not passed", errWorkflow, idx+1, gateIdx+1, gateStep.Name)
		}
		iacEv, curDigest, curClass, pc, err := cardIACFromPreview(env.cfg.Autonomy, gateStep.Preview, gateStep.Target, gateStep.Expect)
		if err != nil {
			return res, fmt.Errorf("%w: re-reading preview for approval: %v", errWorkflow, err)
		}
		if curDigest != gateSt.Digest {
			return res, fmt.Errorf("%w: preview digest changed (%s != %s) since gate_check", errWorkflow, short(curDigest), short(gateSt.Digest))
		}
		if len(pc.Problems) > 0 {
			return res, fmt.Errorf("%w: preview gate failed: %s", errWorkflow, strings.Join(pc.Problems, "; "))
		}
		if gateStep.Expect == "" {
			curClass = min(9, curClass+1)
		}
		res.class = max(res.class, max(gateSt.Class, curClass))
		res.digest = curDigest
		res.classNote = fmt.Sprintf("%s (%s)", targetWord(pc), counts(pc))
		ev.IAC = iacEv
		ev.PlanDigest = curDigest
		hasEv = true
	}

	planIdx := findFollowingPlanStep(spec, idx)
	if planIdx >= 0 {
		res.planIdx = planIdx
		ps := cloneStep(*spec.Steps[planIdx].Plan)
		c, _ := env.cfg.stepClass(b, ps)
		// For iac.apply with an already-checked gate_check, use gate class floor
		// rather than forcing 9 when no preview path is set on plan yet.
		if ps.Op != "iac.apply" || ps.Args["preview"] != "" {
			res.class = max(res.class, c)
		}
		if res.digest == "" {
			p := Plan{Bead: spec.Bead, Actor: "workflow:" + spec.ID, Source: "workflow", Steps: []Step{ps}}
			if raw, merr := json.Marshal(p); merr == nil {
				if norm, perr := parsePlan(raw); perr == nil {
					p = norm
				}
			}
			if d, err := p.digest(); err == nil {
				res.digest = d
			}
		}
		if ev.PlanDigest == "" && res.digest != "" {
			ev.PlanDigest = res.digest
			hasEv = true
		}
		if (ps.Op == "github.pr.merge" || ps.Op == "github.pr.automerge") && ev.PRURL == "" {
			if u, err := stepPRURL(ps); err == nil {
				ev.PRURL = u
				hasEv = true
			}
		}
	}

	if res.digest == "" {
		res.digest = sha256Hex([]byte(spec.Digest + ":" + step.Name))
	}
	if hasEv {
		res.evidence = ev
	}
	return res, nil
}

func cloneStep(s Step) Step {
	cp := s
	if s.Args != nil {
		cp.Args = maps.Clone(s.Args)
	}
	if s.Pre != nil {
		cp.Pre = maps.Clone(s.Pre)
	}
	return cp
}

// tapAnswerForCard searches journal.jsonl for a TJ tap answer matching cardHash.
// Returns choice (1 or 2), timestamp, text, and ok.
func tapAnswerForCard(bead, cardHash string) (int, time.Time, string, bool) {
	if cardHash == "" {
		return 0, time.Time{}, "", false
	}
	recs, err := readJournal(time.Time{})
	if err != nil {
		return 0, time.Time{}, "", false
	}
	needle := "[" + cardHash + "]"
	for _, r := range slices.Backward(recs) {
		if r.Kind != journalNeedsMe || r.Bead != bead {
			continue
		}
		if !strings.Contains(r.Text, needle) || !strings.Contains(r.Text, "by TJ") {
			continue
		}
		rest, ok := strings.CutPrefix(r.Text, "NEEDS_ME ANSWERED: ")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(rest, "1 "):
			return 1, r.At, r.Text, true
		case strings.HasPrefix(rest, "2 "):
			return 2, r.At, r.Text, true
		}
	}
	return 0, time.Time{}, "", false
}

// advanceWorkflow replays events.jsonl and idempotently executes every ready
// step until the workflow completes, fails, cancels, or parks in waiting.
func advanceWorkflow(ctx context.Context, env workflowEnv, ref string) (workflowState, error) {
	spec, _, err := loadWorkflow(ref)
	if err != nil {
		return workflowState{}, err
	}
	unlock, err := lockWorkflow(spec.ID)
	if err != nil {
		return workflowState{}, err
	}
	defer unlock()

	events, err := readWorkflowEvents(spec.ID)
	if err != nil {
		return workflowState{}, err
	}
	st := reconstructWorkflow(spec, events)
	if st.Status == wfStatusCompleted || st.Status == wfStatusFailed || st.Status == wfStatusCanceled {
		return st, nil
	}
	record := func(e workflowEvent) error {
		if e.At.IsZero() {
			e.At = env.clock().UTC()
		}
		if err := appendWorkflowEvent(spec, e); err != nil {
			return err
		}
		events = append(events, e)
		st = reconstructWorkflow(spec, events)
		return nil
	}
	failStep := func(stepNum int, name string, class int, digest, planDigest, why string) (workflowState, error) {
		if err := record(workflowEvent{
			Kind:       wfEventStepFailed,
			Step:       stepNum,
			StepName:   name,
			Class:      class,
			Digest:     digest,
			PlanDigest: planDigest,
			Why:        why,
		}); err != nil {
			return st, err
		}
		return st, nil
	}
	waitStep := func(stepNum int, name string, class, level int, digest, planDigest, cardHash, why string) (workflowState, error) {
		cur := st.Steps[stepNum-1]
		if cur.Status == wfStatusWaiting && cur.Why == why && cur.Digest == digest && cur.CardHash == cardHash && cur.PlanDigest == planDigest {
			return st, nil
		}
		if err := record(workflowEvent{
			Kind:       wfEventStepWaiting,
			Step:       stepNum,
			StepName:   name,
			Class:      class,
			Level:      level,
			Digest:     digest,
			PlanDigest: planDigest,
			CardHash:   cardHash,
			Why:        why,
		}); err != nil {
			return st, err
		}
		return st, nil
	}

	var b Bead
	if env.show != nil {
		b, err = env.show(spec.Bead)
		if err != nil {
			return st, err
		}
		if b.Status == StatusClosed {
			cur := max(1, st.CurrentStep)
			if cur > len(spec.Steps) {
				cur = 1
			}
			return failStep(cur, spec.Steps[cur-1].Name, 0, "", "", spec.Bead+" is closed")
		}
	} else {
		b = Bead{ID: spec.Bead, Status: StatusInProgress}
	}

	for i := 0; i < len(spec.Steps); i++ {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		if st.Steps[i].Status == "ok" {
			continue
		}
		n := i + 1
		step := spec.Steps[i]
		now := env.clock().UTC()

		if st.Steps[i].Status == wfStatusPending {
			if err := record(workflowEvent{
				Kind:     wfEventStepStarted,
				Step:     n,
				StepName: step.Name,
				Class:    step.Class,
			}); err != nil {
				return st, err
			}
		}

		switch step.Kind {
		case wfStepGateCheck:
			data, err := os.ReadFile(step.Preview)
			if err != nil {
				return failStep(n, step.Name, 9, "", "", "preview: "+err.Error())
			}
			digest := sha256Hex(data)
			pc, err := checkPlan(data)
			if err != nil {
				return failStep(n, step.Name, 9, digest, "", "preview: "+err.Error())
			}
			if step.Expect != "" {
				want, err := parseExpect(step.Expect)
				if err != nil {
					return failStep(n, step.Name, 9, digest, "", err.Error())
				}
				pc.expect(want)
			}
			pc.classify(env.cfg.Autonomy, step.Target)
			if len(pc.Problems) > 0 {
				return failStep(n, step.Name, 9, digest, "", strings.Join(pc.Problems, "; "))
			}
			computed := pc.Class
			if step.Expect == "" {
				computed = min(9, computed+1)
			}
			effClass := max(step.Class, computed)
			if err := record(workflowEvent{
				Kind:     wfEventStepOK,
				Step:     n,
				StepName: step.Name,
				Class:    effClass,
				Digest:   digest,
				Evidence: fmt.Sprintf("%s %s: %s (class %d)", pc.Format, targetWord(pc), counts(pc), effClass),
			}); err != nil {
				return st, err
			}

		case wfStepApproval:
			ac, err := resolveApprovalContext(env, spec, st, i, b)
			if err != nil {
				return failStep(n, step.Name, max(step.Class, 9), "", "", err.Error())
			}
			ttl := approvalTTL
			if step.For != "" {
				if d, derr := time.ParseDuration(step.For); derr == nil && d > 0 {
					ttl = d
				}
			}
			ss := st.Steps[i]
			// 1. Check existing recorded approval on this step.
			if ss.ApprovedBy != "" {
				if ss.ApprovalDigest != ac.digest {
					return failStep(n, step.Name, ac.class, ac.digest, "", fmt.Sprintf("approval invalidated: bound digest changed (%s != %s)", short(ac.digest), short(ss.ApprovalDigest)))
				}
				if now.Before(ss.ApprovalExp) {
					if ss.ApprovedBy == "TJ" {
						if err := record(workflowEvent{
							Kind:     wfEventStepOK,
							Step:     n,
							StepName: step.Name,
							Class:    ac.class,
							By:       "TJ",
							Digest:   ac.digest,
							Expires:  ss.ApprovalExp,
							Evidence: "approved by TJ (digest " + short(ac.digest) + ")",
						}); err != nil {
							return st, err
						}
						continue
					}
					if ss.ApprovedBy == "dial" && !step.AlwaysAsk {
						lvl, _ := env.cfg.beadLevel(b, autonomyStopped())
						recs, _ := readJournal(now.Add(-budgetWindow))
						spent := budgetSpent(env.cfg.Autonomy.Budget, recs, b.ID, ac.class, step.Action)
						if ac.class <= lvl && spent == "" {
							if err := record(workflowEvent{
								Kind:     wfEventStepOK,
								Step:     n,
								StepName: step.Name,
								Class:    ac.class,
								Level:    lvl,
								By:       "dial",
								Digest:   ac.digest,
								Expires:  ss.ApprovalExp,
								Evidence: fmt.Sprintf("approved by dial (L%d >= C%d)", lvl, ac.class),
							}); err != nil {
								return st, err
							}
							continue
						}
					}
				}
			}

			// 2. Check if TJ answered the emitted structured QuestionCard via tap/inbox.
			if ss.CardHash != "" {
				if choice, answeredAt, text, ok := tapAnswerForCard(b.ID, ss.CardHash); ok {
					if ss.Digest != "" && ss.Digest != ac.digest {
						return failStep(n, step.Name, ac.class, ac.digest, "", fmt.Sprintf("preview changed while waiting for card answer (%s != %s)", short(ac.digest), short(ss.Digest)))
					}
					if choice == 2 {
						if err := record(workflowEvent{
							Kind: wfEventCanceled,
							By:   "TJ",
							Why:  "canceled by TJ via question card [" + ss.CardHash + "]",
						}); err != nil {
							return st, err
						}
						return st, nil
					}
					if choice == 1 && now.Before(answeredAt.Add(ttl)) {
						exp := answeredAt.Add(ttl)
						if err := record(workflowEvent{
							Kind:     wfEventStepApproved,
							Step:     n,
							StepName: step.Name,
							Class:    ac.class,
							Level:    -1,
							By:       "TJ",
							Digest:   ac.digest,
							Expires:  exp,
							Evidence: text,
						}); err != nil {
							return st, err
						}
						if err := record(workflowEvent{
							Kind:     wfEventStepOK,
							Step:     n,
							StepName: step.Name,
							Class:    ac.class,
							By:       "TJ",
							Digest:   ac.digest,
							Expires:  exp,
							Evidence: "approved by TJ via question card [" + ss.CardHash + "]",
						}); err != nil {
							return st, err
						}
						continue
					}
				}
			}

			// 3. Evaluate the autonomy dial & rolling budgets.
			lvl, capped := env.cfg.beadLevel(b, autonomyStopped())
			recs, err := readJournal(now.Add(-budgetWindow))
			if err != nil {
				return st, err
			}
			spent := budgetSpent(env.cfg.Autonomy.Budget, recs, b.ID, ac.class, step.Action)
			if !step.AlwaysAsk && ac.class <= lvl && spent == "" {
				exp := now.Add(ttl)
				if err := record(workflowEvent{
					Kind:     wfEventStepApproved,
					Step:     n,
					StepName: step.Name,
					Class:    ac.class,
					Level:    lvl,
					By:       "dial",
					Digest:   ac.digest,
					Expires:  exp,
					Evidence: fmt.Sprintf("approved by dial (level %d) until %s", lvl, exp.Format("15:04")),
				}); err != nil {
					return st, err
				}
				autoNote := fmt.Sprintf("AUTO: [worker L%d C%d] workflow %s step %d (%s) approved — digest %s", lvl, ac.class, spec.ID, n, step.Name, short(ac.digest))
				if env.note != nil {
					_ = env.note(b.ID, autoNote)
				}
				_ = appendJournal(journalRecord{At: now, Kind: journalAuto, Bead: b.ID, Class: ac.class, Action: step.Action, Pass: true, Text: autoNote})
				if err := record(workflowEvent{
					Kind:     wfEventStepOK,
					Step:     n,
					StepName: step.Name,
					Class:    ac.class,
					Level:    lvl,
					By:       "dial",
					Digest:   ac.digest,
					Expires:  exp,
					Evidence: fmt.Sprintf("approved by dial (L%d >= C%d, digest %s)", lvl, ac.class, short(ac.digest)),
				}); err != nil {
					return st, err
				}
				continue
			}

			// 4. Requires TJ approval: emit a structured QuestionCard bound to digest.
			var why string
			switch {
			case step.AlwaysAsk:
				why = fmt.Sprintf("step %d (%s) requires explicit TJ approval (always_ask)", n, step.Name)
			case ac.class > lvl:
				capSuffix := ""
				if capped != "" {
					capSuffix = ", capped by " + capped
				}
				why = fmt.Sprintf("class %d is above %s's level %d%s", ac.class, b.ID, lvl, capSuffix)
			default:
				why = spent
			}
			// If already waiting on this exact digest with a live card, stay parked idempotently.
			if ss.Status == wfStatusWaiting && ss.Digest == ac.digest && ss.CardHash != "" {
				if existingCard, cerr := readQuestionCard(b.ID); cerr == nil && existingCard.Hash == ss.CardHash {
					if live, _ := questionCardLive(existingCard, b); live {
						return st, nil
					}
				}
			}
			qText := step.Question
			if qText == "" {
				qText = fmt.Sprintf("Approve workflow %s step %d (%s)?", spec.ID, n, step.Name)
			}
			clsReason := step.ClassReason
			if clsReason == "" {
				clsReason = ac.classNote
			}
			if clsReason == "" {
				clsReason = why
			}
			opt1Class := ac.class
			opt2Class := 0
			cardIn := questionCardInput{
				Question:    qText,
				Details:     fmt.Sprintf("Workflow %s step %d (%s), bound digest %s. Approve with `quartermaster workflow approve %s --step %d --by TJ` or tap option 1.", spec.ID, n, step.Name, short(ac.digest), spec.ID, n),
				Critical:    ac.class >= 8,
				Class:       &ac.class,
				ClassReason: clsReason,
				Options: []CardOption{
					{Number: 1, Label: fmt.Sprintf("Approve and continue (%s)", step.Name), Description: "Approves digest " + short(ac.digest), Class: &opt1Class},
					{Number: 2, Label: "Cancel workflow", Description: "Cancels workflow " + spec.ID, Class: &opt2Class},
				},
				Evidence: ac.evidence,
			}
			if ac.class < 8 {
				cardIn.Recommendation = &CardRecommendation{Option: 1, Rationale: "pre-checks passed; bound to digest " + short(ac.digest)}
			}
			card, cerr := writeQuestionCard(env.cfg, env.cardEnv(), b.ID, cardIn, "", "", "")
			if cerr != nil {
				return st, fmt.Errorf("workflow %s: emit approval card: %w", spec.ID, cerr)
			}
			// Re-read bead if available so subsequent checks see needs_me.
			if env.show != nil {
				if updated, uerr := env.show(b.ID); uerr == nil {
					b = updated
				}
			}
			return waitStep(n, step.Name, ac.class, lvl, ac.digest, "", card.Hash, why)

		case wfStepPlan:
			ps := cloneStep(*step.Plan)
			if ps.Pre == nil {
				ps.Pre = map[string]string{}
			}
			if ps.Args == nil {
				ps.Args = map[string]string{}
			}

			// Verify any preceding gate_check preview still hashes identically.
			gateIdx := findPrecedingGateStep(spec, i, "")
			gateDigest := ""
			gateClass := 0
			if gateIdx >= 0 {
				gateStep := spec.Steps[gateIdx]
				gateSt := st.Steps[gateIdx]
				if gateSt.Status != "ok" || gateSt.Digest == "" {
					return failStep(n, step.Name, 9, "", "", fmt.Sprintf("preceding gate_check step %d has not passed", gateIdx+1))
				}
				data, err := os.ReadFile(gateStep.Preview)
				if err != nil {
					return failStep(n, step.Name, 9, "", "", "preview missing before apply: "+err.Error())
				}
				curDigest := sha256Hex(data)
				if curDigest != gateSt.Digest {
					return failStep(n, step.Name, 9, curDigest, "", fmt.Sprintf("preview digest changed (%s != %s) after gate_check/approval; refusing execution", short(curDigest), short(gateSt.Digest)))
				}
				gateDigest = curDigest
				gateClass = gateSt.Class
				if ps.Op == "iac.apply" {
					if ps.Args["preview"] == "" {
						ps.Args["preview"] = gateStep.Preview
					}
					ps.Pre["plan_digest"] = curDigest
				}
			}

			p := Plan{
				Bead:   spec.Bead,
				Actor:  "workflow:" + spec.ID,
				Source: "workflow",
				Steps:  []Step{ps},
			}
			rawPlan, err := json.Marshal(p)
			if err != nil {
				return failStep(n, step.Name, 9, "", "", err.Error())
			}
			parsedPlan, err := parsePlan(rawPlan)
			if err != nil {
				return failStep(n, step.Name, 9, "", "", err.Error())
			}
			planDigest, err := parsedPlan.digest()
			if err != nil {
				return failStep(n, step.Name, 9, "", "", err.Error())
			}
			planClass, _ := classifyPlan(env.cfg, func(string) (Bead, error) { return b, nil }, parsedPlan)
			effClass := max(max(planClass, step.Class), gateClass)

			// Crash-safe idempotency: if the child plan already finished, settle step_ok without re-running.
			existingPlanEvents, err := readPlanEvents(planDigest)
			if err != nil {
				return st, err
			}
			if last, ok := lastRun(existingPlanEvents); ok && last.Kind == planDone {
				if err := record(workflowEvent{
					Kind:       wfEventStepOK,
					Step:       n,
					StepName:   step.Name,
					Class:      effClass,
					PlanDigest: planDigest,
					Evidence:   "plan " + short(planDigest) + " already done",
				}); err != nil {
					return st, err
				}
				continue
			}

			// Recheck preceding approval step (digest binding, expiry, dial/budget).
			appIdx := findPrecedingApprovalStep(spec, i)
			var wfApp *wfStepState
			if appIdx >= 0 {
				as := st.Steps[appIdx]
				if as.ApprovedBy == "" {
					return failStep(n, step.Name, effClass, "", planDigest, fmt.Sprintf("approval step %d (%s) has no approval", appIdx+1, spec.Steps[appIdx].Name))
				}
				expectedDigest := planDigest
				if gateDigest != "" {
					expectedDigest = gateDigest
				}
				if as.ApprovalDigest != expectedDigest && as.ApprovalDigest != planDigest {
					return failStep(n, step.Name, effClass, expectedDigest, planDigest, fmt.Sprintf("approval digest mismatch (%s != %s)", short(as.ApprovalDigest), short(expectedDigest)))
				}
				if !now.Before(as.ApprovalExp) {
					// Expired approval: re-park approval step for fresh approval.
					expWhy := fmt.Sprintf("approval expired at %s; re-approve with `quartermaster workflow approve %s --step %d --by TJ`", as.ApprovalExp.Format("15:04"), spec.ID, appIdx+1)
					return waitStep(appIdx+1, spec.Steps[appIdx].Name, max(effClass, as.Class), -1, expectedDigest, planDigest, "", expWhy)
				}
				if as.ApprovedBy == "dial" {
					lvl, capped := env.cfg.beadLevel(b, autonomyStopped())
					recs, rerr := readJournal(now.Add(-budgetWindow))
					if rerr != nil {
						return st, rerr
					}
					spent := budgetSpent(env.cfg.Autonomy.Budget, recs, b.ID, effClass, spec.Steps[appIdx].Action)
					if effClass > lvl || spent != "" {
						why := spent
						if effClass > lvl {
							why = fmt.Sprintf("class %d now exceeds %s's level %d", effClass, b.ID, lvl)
							if capped != "" {
								why += ", capped by " + capped
							}
						}
						return waitStep(appIdx+1, spec.Steps[appIdx].Name, effClass, lvl, expectedDigest, planDigest, "", why)
					}
				}
				wfApp = &as
			}

			if err := storePlan(parsedPlan, planDigest); err != nil {
				return st, err
			}
			// Propagate live authorization to the child plan so executor.go enforces it.
			if _, ok := liveApproval(existingPlanEvents, now); !ok {
				if wfApp != nil && wfApp.ApprovedBy == "TJ" {
					if err := appendPlanEvent(planDigest, planEvent{
						At:    now,
						Kind:  planChecked,
						Class: effClass,
						Level: -1,
						Pass:  true,
					}); err != nil {
						return st, err
					}
					if err := appendPlanEvent(planDigest, planEvent{
						At:      now,
						Kind:    planApproved,
						Class:   effClass,
						Level:   -1,
						Pass:    true,
						By:      "TJ",
						Expires: wfApp.ApprovalExp,
					}); err != nil {
						return st, err
					}
				} else {
					if err := checkPlanFor(env.cfg, func(string) (Bead, error) { return b, nil }, parsedPlan, now, io.Discard); err != nil {
						if errors.Is(err, errNotAutonomous) {
							return waitStep(n, step.Name, effClass, -1, planDigest, planDigest, "", err.Error())
						}
						return failStep(n, step.Name, effClass, "", planDigest, err.Error())
					}
				}
			}

			if env.execPlan == nil {
				return failStep(n, step.Name, effClass, "", planDigest, "no plan executor configured")
			}
			kind, err := env.execPlan(ctx, planDigest)
			if err != nil {
				if errors.Is(err, errBusy) {
					return st, err
				}
				return failStep(n, step.Name, effClass, "", planDigest, err.Error())
			}
			afterEvents, _ := readPlanEvents(planDigest)
			var evidenceParts []string
			for _, pe := range currentRun(afterEvents) {
				if pe.Kind == planOutcome && pe.Result == resultOK && pe.Evidence != "" {
					evidenceParts = append(evidenceParts, pe.Evidence)
				}
			}
			lastEv, _ := lastRun(afterEvents)
			switch kind {
			case planDone:
				evText := strings.Join(evidenceParts, "; ")
				if evText == "" {
					evText = "plan " + short(planDigest) + " done"
				}
				if err := record(workflowEvent{
					Kind:       wfEventStepOK,
					Step:       n,
					StepName:   step.Name,
					Class:      effClass,
					PlanDigest: planDigest,
					Evidence:   evText,
				}); err != nil {
					return st, err
				}
			case planNeedsMe, planExpired:
				why := lastEv.Why
				if why == "" {
					why = "plan " + short(planDigest) + " " + kind
				}
				return waitStep(n, step.Name, effClass, -1, planDigest, planDigest, "", why)
			default:
				why := lastEv.Why
				if why == "" {
					why = "plan " + short(planDigest) + " " + kind
				}
				return failStep(n, step.Name, effClass, "", planDigest, why)
			}

		case wfStepWait:
			done, failedWhy, waitWhy, digest, evText := evaluateWaitStep(ctx, env, step, st.Steps[i], now)
			switch {
			case failedWhy != "":
				return failStep(n, step.Name, step.Class, digest, "", failedWhy)
			case done:
				if err := record(workflowEvent{
					Kind:     wfEventStepOK,
					Step:     n,
					StepName: step.Name,
					Class:    step.Class,
					Digest:   digest,
					Evidence: evText,
				}); err != nil {
					return st, err
				}
			default:
				return waitStep(n, step.Name, step.Class, -1, digest, "", "", waitWhy)
			}

		case wfStepMilestone:
			// Crash-safe idempotency: skip appending if this milestone already exists after step_started.
			already := false
			if existing, merr := readMilestones(spec.Bead); merr == nil {
				startedAt := st.Steps[i].StartedAt
				for _, m := range existing {
					if m.Phase == oneLine(step.Phase) && m.Summary == oneLine(step.Summary) && !m.At.Before(startedAt) {
						already = true
						break
					}
				}
			}
			if !already {
				mev := milestoneEvidenceForWorkflow(spec, st)
				if _, err := appendMilestone(env.cardEnv(), spec.Bead, milestoneInput{
					Phase:    step.Phase,
					Status:   step.Status,
					Summary:  step.Summary,
					Evidence: mev,
				}); err != nil {
					return failStep(n, step.Name, step.Class, "", "", err.Error())
				}
			}
			if err := record(workflowEvent{
				Kind:     wfEventStepOK,
				Step:     n,
				StepName: step.Name,
				Class:    step.Class,
				Evidence: fmt.Sprintf("milestone [%s]: %s", step.Phase, step.Summary),
			}); err != nil {
				return st, err
			}

		case wfStepVerify:
			vk := step.VerifyKind
			if vk == "" {
				if step.Preview != "" {
					vk = wfVerifyNoDrift
				} else {
					vk = wfVerifyAcceptance
				}
			}
			switch vk {
			case wfVerifyNoDrift:
				var data []byte
				var err error
				switch {
				case step.Preview != "":
					data, err = os.ReadFile(step.Preview)
				case env.refreshPreview != nil:
					data, err = env.refreshPreview(ctx, step)
				default:
					return failStep(n, step.Name, step.Class, "", "", "no post-apply preview configured for no-drift check")
				}
				if err != nil {
					return failStep(n, step.Name, step.Class, "", "", "no-drift preview: "+err.Error())
				}
				pc, err := checkPlan(data)
				if err != nil {
					return failStep(n, step.Name, step.Class, sha256Hex(data), "", "no-drift preview: "+err.Error())
				}
				pc.expect(map[string]int{})
				pc.classify(env.cfg.Autonomy, step.Target)
				if !pc.ok() {
					return failStep(n, step.Name, max(step.Class, pc.Class), sha256Hex(data), "", "post-apply drift detected: "+counts(pc))
				}
				if err := record(workflowEvent{
					Kind:     wfEventStepOK,
					Step:     n,
					StepName: step.Name,
					Class:    step.Class,
					Digest:   sha256Hex(data),
					Evidence: fmt.Sprintf("no-drift verified (%s: %s)", pc.Format, counts(pc)),
				}); err != nil {
					return st, err
				}
			case wfVerifyAcceptance:
				recs, _ := readJournal(time.Time{})
				if _, fresh := verifyRounds(recs, spec.Bead, now.Add(-passFresh)); fresh {
					if err := record(workflowEvent{
						Kind:     wfEventStepOK,
						Step:     n,
						StepName: step.Name,
						Class:    step.Class,
						Evidence: "fresh acceptance verifier pass in journal",
					}); err != nil {
						return st, err
					}
					continue
				}
				if env.runVerify == nil {
					return waitStep(n, step.Name, step.Class, -1, "", "", "", "waiting for acceptance verifier pass")
				}
				res, err := env.runVerify(ctx, b)
				if err != nil {
					return failStep(n, step.Name, step.Class, "", "", "verifier: "+err.Error())
				}
				_ = appendJournal(journalRecord{
					At:     now,
					Kind:   journalVerify,
					Bead:   b.ID,
					Class:  -1,
					Action: "verify",
					Pass:   res.passed(),
					Text:   res.summary(),
				})
				if !res.passed() {
					return failStep(n, step.Name, step.Class, "", "", strings.Join(res.failing(), "; "))
				}
				if err := record(workflowEvent{
					Kind:     wfEventStepOK,
					Step:     n,
					StepName: step.Name,
					Class:    step.Class,
					Evidence: res.summary(),
				}); err != nil {
					return st, err
				}
			}
		}
	}

	if err := record(workflowEvent{
		Kind:     wfEventCompleted,
		Digest:   spec.Digest,
		Evidence: fmt.Sprintf("%d step(s) completed", len(spec.Steps)),
	}); err != nil {
		return st, err
	}
	return st, nil
}

func evaluateWaitStep(ctx context.Context, env workflowEnv, step WorkflowStep, ss wfStepState, now time.Time) (done bool, failedWhy, waitWhy, digest, evText string) {
	switch step.WaitKind {
	case wfWaitPRReady:
		if step.PRURL == "" && step.Optional {
			return true, "", "", "", "skipped (no PR URL)"
		}
		if env.viewPR == nil {
			return false, "", "waiting for PR provider", "", ""
		}
		pr, err := env.viewPR(ctx, step.PRURL)
		if err != nil {
			return false, "", "pr lookup failed: " + err.Error(), "", ""
		}
		if pr.State == "CLOSED" {
			return false, fmt.Sprintf("PR %s is closed", step.PRURL), "", pr.HeadSHA, ""
		}
		if step.HeadSHA != "" && pr.HeadSHA != step.HeadSHA {
			return false, fmt.Sprintf("PR head moved to %s (expected %s)", short(pr.HeadSHA), short(step.HeadSHA)), "", pr.HeadSHA, ""
		}
		if pr.State == "MERGED" {
			return true, "", "", pr.HeadSHA, fmt.Sprintf("PR #%d already merged at %s", pr.Number, short(pr.HeadSHA))
		}
		if pr.IsDraft {
			return false, "", "PR is a draft", pr.HeadSHA, ""
		}
		c := summarizeChecks(pr.Checks)
		if c.Pending > 0 {
			return false, "", fmt.Sprintf("waiting for %d pending %s", c.Pending, plural(c.Pending, "check")), pr.HeadSHA, ""
		}
		if len(c.Failing) > 0 {
			return false, "", "checks failing: " + strings.Join(c.Failing, ", "), pr.HeadSHA, ""
		}
		if !mergeable(&pr, c) {
			return false, "", notReady(pr, c), pr.HeadSHA, ""
		}
		if env.viewPRGate != nil {
			g, ok, err := env.viewPRGate(ctx, pr, now)
			if err != nil {
				return false, "", "review gate lookup failed: " + err.Error(), pr.HeadSHA, ""
			}
			if !ok {
				return false, "", "waiting for current-head review evidence", pr.HeadSHA, ""
			}
			if g.Head != pr.HeadSHA {
				return false, "", "review evidence head mismatch", pr.HeadSHA, ""
			}
			if g.OpenThreads > 0 {
				return false, "", fmt.Sprintf("%d open %s", g.OpenThreads, plural(g.OpenThreads, "thread")), pr.HeadSHA, ""
			}
			if g.MoreThreads || g.MoreReviews || g.ReviewProblem != "" {
				return false, "", "incomplete review/thread evidence", pr.HeadSHA, ""
			}
			for _, r := range latestOpinions(g.Reviews) {
				if r.State == "CHANGES_REQUESTED" {
					return false, "", "changes requested by " + r.Author.Login, pr.HeadSHA, ""
				}
			}
			if !freshApproval(g.Reviews, &g) {
				return false, "", "waiting for current-head approval", pr.HeadSHA, ""
			}
		}
		return true, "", "", pr.HeadSHA, fmt.Sprintf("PR #%d ready at %s", pr.Number, short(pr.HeadSHA))

	case wfWaitPRMerged:
		if step.PRURL == "" && step.Optional {
			return true, "", "", "", "skipped (no PR URL)"
		}
		if env.viewPR == nil {
			return false, "", "waiting for PR provider", "", ""
		}
		pr, err := env.viewPR(ctx, step.PRURL)
		if err != nil {
			return false, "", "pr lookup failed: " + err.Error(), "", ""
		}
		if pr.State == "MERGED" {
			if step.HeadSHA != "" && pr.HeadSHA != step.HeadSHA {
				return false, fmt.Sprintf("PR merged at %s, expected %s", short(pr.HeadSHA), short(step.HeadSHA)), "", pr.HeadSHA, ""
			}
			return true, "", "", pr.HeadSHA, fmt.Sprintf("PR #%d merged at %s", pr.Number, short(pr.HeadSHA))
		}
		if pr.State == "CLOSED" {
			return false, fmt.Sprintf("PR %s closed without merging", step.PRURL), "", pr.HeadSHA, ""
		}
		return false, "", fmt.Sprintf("waiting for PR #%d to merge", pr.Number), pr.HeadSHA, ""

	case wfWaitRunCompleted:
		if step.RunURL == "" && step.Optional {
			return true, "", "", "", "skipped (no run URL)"
		}
		m := runURL.FindStringSubmatch(step.RunURL)
		if m == nil {
			return false, fmt.Sprintf("invalid run URL %q", step.RunURL), "", "", ""
		}
		if env.viewRun == nil {
			return false, "", "waiting for Actions run provider", "", ""
		}
		r, err := env.viewRun(ctx, m[1], m[2])
		if err != nil {
			return false, "", "run lookup failed: " + err.Error(), "", ""
		}
		if r.Status == "completed" {
			if r.Conclusion == "success" {
				return true, "", "", "", fmt.Sprintf("run %d (%s) succeeded", r.ID, orDash(r.Workflow))
			}
			return false, fmt.Sprintf("run %d ended %s", r.ID, orDash(r.Conclusion)), "", "", ""
		}
		return false, "", fmt.Sprintf("waiting for run %s (%s)", m[2], orDash(r.Status)), "", ""

	case wfWaitSignal:
		if ss.ApprovedBy != "" {
			return true, "", "", ss.ApprovalDigest, "signal received from " + ss.ApprovedBy
		}
		return false, "", "waiting for external signal", "", ""
	}
	return false, "unknown wait_kind " + step.WaitKind, "", "", ""
}

func milestoneEvidenceForWorkflow(spec WorkflowSpec, st workflowState) *MilestoneEvidence {
	ev := &MilestoneEvidence{}
	has := false
	if u := spec.Vars["pr_url"]; u != "" {
		ev.PRURL = u
		has = true
	}
	if u := spec.Vars["run_url"]; u != "" {
		ev.RunURL = u
		has = true
	}
	if s := spec.Vars["head_sha"]; s != "" {
		ev.SHA = s
		has = true
	} else if s := spec.Vars["rev"]; s != "" {
		ev.SHA = s
		has = true
	}
	for _, ss := range slices.Backward(st.Steps) {
		if ss.PlanDigest != "" {
			ev.PlanDigest = ss.PlanDigest
			has = true
			break
		}
		if ss.Digest != "" && ev.PlanDigest == "" {
			ev.PlanDigest = ss.Digest
			has = true
		}
	}
	if !has {
		return nil
	}
	return ev
}

// Ticker integration: cleanly, bounded and non-blocking advancement of waiting
// workflows whose wait or approval conditions are now satisfied.
const workflowTickerTimeout = 15 * time.Second

func (t Ticker) advanceWorkflows(st *TickerState, threads []Thread, now time.Time) {
	env := t.workflowEnv(st, threads, now)
	ctx, cancel := context.WithTimeout(context.Background(), workflowTickerTimeout)
	defer cancel()
	advanceWaitingWorkflows(ctx, env)
}

func (t Ticker) workflowEnv(st *TickerState, threads []Thread, now time.Time) workflowEnv {
	byBead := make(map[string]Bead, len(threads))
	prByURL := map[string]PR{}
	runByKey := map[string]Run{}
	for _, th := range threads {
		byBead[th.Bead.ID] = th.Bead
		if th.PR != nil && th.PR.URL != "" {
			prByURL[th.PR.URL] = *th.PR
		}
		for _, m := range th.Merged {
			if m.URL != "" {
				prByURL[m.URL] = m
			}
		}
		for _, r := range th.Runs {
			runByKey[r.Repo+"#"+runKey(r)] = r
		}
	}
	if st != nil {
		for u, pr := range st.Noted {
			if _, ok := prByURL[u]; !ok {
				prByURL[u] = pr
			}
		}
	}
	base := liveWorkflowEnv(t.cfg)
	base.now = func() time.Time { return now }
	base.logf = t.log.Printf
	base.show = func(id string) (Bead, error) {
		if b, ok := byBead[id]; ok {
			return b, nil
		}
		return t.beads.Show(id)
	}
	base.viewPR = func(_ context.Context, url string) (PR, error) {
		if pr, ok := prByURL[url]; ok && (pr.State == "MERGED" || pr.State == "CLOSED" || pr.DetailsFresh) {
			return pr, nil
		}
		pr, err := t.gh.View(url, true)
		if err == nil {
			pr.DetailsFresh = true
		}
		return pr, err
	}
	base.viewPRGate = func(_ context.Context, pr PR, at time.Time) (MergeGate, bool, error) {
		if cached, ok := prByURL[pr.URL]; ok && cached.Gate != nil && cached.Gate.Head == pr.HeadSHA {
			return *cached.Gate, true, nil
		}
		gates, err := t.gh.Gates([]PR{pr}, at)
		if err != nil {
			return MergeGate{}, false, err
		}
		g, ok := gates[pr.URL]
		return g, ok, nil
	}
	base.viewRun = func(_ context.Context, repo, id string) (Run, error) {
		if r, ok := runByKey[repo+"#"+id]; ok && r.Status == "completed" {
			return r, nil
		}
		return t.gh.RunView(repo, id)
	}
	return base
}

// advanceWaitingWorkflows walks active workflows and advances each waiting or
// unfinished workflow within ctx. It never approves anything above the dial.
func advanceWaitingWorkflows(ctx context.Context, env workflowEnv) int {
	specs, err := listWorkflowSpecs()
	if err != nil || len(specs) == 0 {
		return 0
	}
	advanced := 0
	for _, spec := range specs {
		if ctx.Err() != nil {
			break
		}
		events, err := readWorkflowEvents(spec.ID)
		if err != nil {
			continue
		}
		before := reconstructWorkflow(spec, events)
		if before.Status != wfStatusWaiting && before.Status != wfStatusRunning {
			continue
		}
		after, err := advanceWorkflow(ctx, env, spec.ID)
		if err != nil {
			if !errors.Is(err, errBusy) && env.logf != nil {
				env.logf("workflow %s (%s): %v", spec.ID, spec.Bead, err)
			}
			continue
		}
		if after.Status != before.Status || after.CurrentStep != before.CurrentStep {
			advanced++
			if env.logf != nil {
				env.logf("workflow %s (%s): %s -> %s", spec.ID, spec.Bead, before.Status, after.Status)
			}
		}
	}
	return advanced
}

// workflowContextSection renders an active-workflows section for `quartermaster context`
// when at least one non-terminal workflow exists.
func workflowContextSection() string {
	specs, err := listWorkflowSpecs()
	if err != nil || len(specs) == 0 {
		return ""
	}
	var lines []string
	for _, spec := range specs {
		events, err := readWorkflowEvents(spec.ID)
		if err != nil {
			continue
		}
		st := reconstructWorkflow(spec, events)
		if st.Status == wfStatusCompleted || st.Status == wfStatusCanceled || st.Status == wfStatusFailed {
			continue
		}
		stepInfo := ""
		if st.CurrentStep >= 1 && st.CurrentStep <= len(st.Steps) {
			ss := st.Steps[st.CurrentStep-1]
			stepInfo = fmt.Sprintf("step %d/%d (%s)", st.CurrentStep, len(st.Steps), ss.Name)
		}
		line := fmt.Sprintf("- %s · %s · %s", st.ID, st.Bead, st.Status)
		if stepInfo != "" {
			line += " · " + stepInfo
		}
		if st.Why != "" {
			line += " — " + shortTitle(st.Why, 100)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n## Workflows (`quartermaster workflow status`)\n" + strings.Join(lines, "\n") + "\n"
}

const workflowUsage = "usage: quartermaster workflow list-templates [--json] | start <bead> (--template NAME [--var k=v ...] | --file <spec.json|->) [--json] | status [<id|bead>] [--json] | advance|resume <id> [--json] | approve <id> --step N --by TJ [--for 1h] [--json] | cancel <id> [--reason R] [--json]"

func workflowCmd(cfg Config, env workflowEnv, args []string, stdin io.Reader, w io.Writer) error {
	if len(args) == 0 {
		return errors.New(workflowUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list-templates":
		return workflowListTemplatesCmd(rest, w)
	case "start":
		return workflowStartCmd(env, rest, stdin, w)
	case "status":
		return workflowStatusCmd(rest, w)
	case "advance", "resume":
		return workflowAdvanceCmd(env, rest, w)
	case "approve":
		return workflowApproveCmd(env, rest, w)
	case "cancel":
		return workflowCancelCmd(env, rest, w)
	default:
		return errors.New(workflowUsage)
	}
}

func workflowListTemplatesCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("workflow list-templates", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: quartermaster workflow list-templates [--json]")
	}
	if *jsonOut {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(workflowTemplates)
	}
	for _, t := range workflowTemplates {
		fmt.Fprintf(w, "%s\n  %s\n  required vars: %s\n", t.Name, t.Description, strings.Join(t.RequiredVars, ", "))
		if len(t.OptionalVars) > 0 {
			fmt.Fprintf(w, "  optional vars: %s\n", strings.Join(t.OptionalVars, ", "))
		}
		fmt.Fprintf(w, "  steps: %s\n", strings.Join(t.Steps, " -> "))
	}
	return nil
}

func workflowStartCmd(env workflowEnv, args []string, stdin io.Reader, w io.Writer) error {
	fs := flag.NewFlagSet("workflow start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tmplFlag := fs.String("template", "", "built-in template name (pr-merge-verify or iac-rollout)")
	fileFlag := fs.String("file", "", "declarative JSON workflow spec file (or - for stdin)")
	var varFlags stringSliceFlag
	fs.Var(&varFlags, "var", "repeatable template variable k=v")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster workflow start <bead> (--template NAME [--var k=v ...] | --file <spec.json|->)")
	}
	bead := pos[0]
	if (*tmplFlag == "") == (*fileFlag == "") {
		return errors.New("workflow start requires exactly one of --template or --file")
	}
	ctx := context.Background()
	var spec WorkflowSpec
	if *tmplFlag != "" {
		vars := map[string]string{}
		for _, kv := range varFlags {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return fmt.Errorf("--var %q: want k=v", kv)
			}
			vars[strings.TrimSpace(k)] = v
		}
		spec, err = buildTemplateSpec(ctx, env, bead, *tmplFlag, vars)
		if err != nil {
			return err
		}
	} else {
		if len(varFlags) > 0 {
			return errors.New("--var applies only with --template")
		}
		data, err := readPlanArg(*fileFlag, stdin)
		if err != nil {
			return err
		}
		spec, err = parseWorkflowSpec(data, bead)
		if err != nil {
			return err
		}
	}
	st, err := startWorkflow(ctx, env, spec)
	if err != nil {
		return err
	}
	return writeWorkflowStatus(st, *jsonOut, w)
}

func workflowStatusCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("workflow status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: quartermaster workflow status [<id|bead>] [--json]")
	}
	if len(pos) == 1 {
		ref := pos[0]
		if spec, events, err := loadWorkflow(ref); err == nil {
			return writeWorkflowStatus(reconstructWorkflow(spec, events), *jsonOut, w)
		}
		// Fall back to filtering by bead ID.
		specs, err := listWorkflowSpecs()
		if err != nil {
			return err
		}
		var matches []workflowState
		for _, spec := range specs {
			if spec.Bead == ref {
				events, _ := readWorkflowEvents(spec.ID)
				matches = append(matches, reconstructWorkflow(spec, events))
			}
		}
		if len(matches) == 0 {
			return fmt.Errorf("%w: no workflow or bead %s", errWorkflow, ref)
		}
		return writeWorkflowList(matches, *jsonOut, w)
	}
	specs, err := listWorkflowSpecs()
	if err != nil {
		return err
	}
	states := make([]workflowState, 0, len(specs))
	for _, spec := range specs {
		events, _ := readWorkflowEvents(spec.ID)
		states = append(states, reconstructWorkflow(spec, events))
	}
	return writeWorkflowList(states, *jsonOut, w)
}

func workflowAdvanceCmd(env workflowEnv, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("workflow advance", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster workflow advance|resume <id> [--json]")
	}
	st, err := advanceWorkflow(context.Background(), env, pos[0])
	if err != nil {
		return err
	}
	return writeWorkflowStatus(st, *jsonOut, w)
}

func workflowApproveCmd(env workflowEnv, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("workflow approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stepFlag := fs.Int("step", 0, "1-based step number to approve")
	byFlag := fs.String("by", "", "approver (must be TJ)")
	forFlag := fs.Duration("for", approvalTTL, "approval duration (default 1h)")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *stepFlag <= 0 {
		return errors.New("usage: quartermaster workflow approve <id> --step <n> --by TJ [--for 1h]")
	}
	st, err := approveWorkflow(context.Background(), env, pos[0], *stepFlag, *byFlag, *forFlag)
	if err != nil {
		return err
	}
	return writeWorkflowStatus(st, *jsonOut, w)
}

func workflowCancelCmd(env workflowEnv, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("workflow cancel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reasonFlag := fs.String("reason", "", "cancellation reason")
	byFlag := fs.String("by", "worker", "who canceled")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: quartermaster workflow cancel <id> [--reason \"...\"]")
	}
	st, err := cancelWorkflow(env, pos[0], *reasonFlag, *byFlag)
	if err != nil {
		return err
	}
	return writeWorkflowStatus(st, *jsonOut, w)
}

func writeWorkflowStatus(st workflowState, asJSON bool, w io.Writer) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	tmpl := ""
	if st.Template != "" {
		tmpl = " (" + st.Template + ")"
	}
	fmt.Fprintf(w, "workflow %s for %s%s: %s [digest %s]\n", st.ID, st.Bead, tmpl, st.Status, short(st.Digest))
	for _, s := range st.Steps {
		line := fmt.Sprintf("  %d. [%s] %s (%s)", s.Index, s.Status, s.Name, s.Kind)
		if s.Class > 0 {
			line += fmt.Sprintf(" C%d", s.Class)
		}
		if s.Digest != "" {
			line += " digest=" + short(s.Digest)
		}
		if s.CardHash != "" {
			line += " card=" + s.CardHash
		}
		if s.ApprovedBy != "" {
			line += fmt.Sprintf(" approved=%s", s.ApprovedBy)
		}
		if s.Evidence != "" {
			line += " — " + s.Evidence
		}
		if s.Why != "" {
			line += " — " + s.Why
		}
		fmt.Fprintln(w, line)
	}
	if st.Why != "" {
		fmt.Fprintf(w, "reason: %s\n", st.Why)
	}
	return nil
}

func writeWorkflowList(states []workflowState, asJSON bool, w io.Writer) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(states)
	}
	if len(states) == 0 {
		fmt.Fprintln(w, "(no workflows)")
		return nil
	}
	for _, st := range states {
		line := fmt.Sprintf("- %s · %s · %s", st.ID, st.Bead, st.Status)
		if st.Template != "" {
			line += " (" + st.Template + ")"
		}
		if st.CurrentStep > 0 && st.CurrentStep <= len(st.Steps) {
			line += fmt.Sprintf(" · step %d/%d (%s)", st.CurrentStep, len(st.Steps), st.Steps[st.CurrentStep-1].Name)
		}
		if st.Why != "" {
			line += " — " + shortTitle(st.Why, 80)
		}
		fmt.Fprintln(w, line)
	}
	return nil
}
