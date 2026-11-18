package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The merge runners carry out github.pr.merge and github.pr.automerge. They
// take neither the worker's word nor the ticker's: before each attempt they
// read the PR's head, review decision, merge state and checks from GitHub,
// and they merge with --match-head-commit, so GitHub refuses a head that moved
// between the check and the act. That compare-and-swap makes them retry-safe,
// and the probe is the PR merged at that head. Auto requests use the same
// gate and direct merge: a deferred native request is not head-bound.

// errUnmet is a precondition that doesn't hold yet, such as checks still
// running: the step fails cleanly, and a later run of the same plan can pass.
var errUnmet = errors.New("not ready")

func init() {
	opRunners["github.pr.merge"] = mergeRunner{gh: ghExec}
	opRunners["github.pr.automerge"] = mergeRunner{auto: true, gh: ghExec}
	opRunners[opOpaque] = opaqueRunner{run: ghExec, tool: toolExec}
}

type mergeRunner struct {
	auto bool
	gh   func(ctx context.Context, dir string, args ...string) ([]byte, error)
	// Installed by the executor, never read from the proposed step.
	policy func(PR) MergePolicy
}

func (mergeRunner) retrySafe() bool { return true }

func (m mergeRunner) view(ctx context.Context, s Step) (PR, string, error) {
	url, err := stepPRURL(s)
	if err != nil {
		return PR{}, "", err
	}
	out, err := m.gh(ctx, "", "pr", "view", url, "--json", detailFields)
	if err != nil {
		return PR{}, url, err
	}
	var pr PR
	if err := json.Unmarshal(out, &pr); err != nil {
		return PR{}, url, fmt.Errorf("gh pr view %s: %w", url, err)
	}
	return pr, url, nil
}

// pre requires the same task policy and live current-head evidence as the
// ticker, for BOTH direct and auto requests. No branch protection is not
// permission, and --auto can merge immediately on unprotected repositories.
func (m mergeRunner) pre(ctx context.Context, s Step) error {
	if method := s.Args["method"]; method != "" && method != "squash" && method != "merge" && method != "rebase" {
		return fmt.Errorf("%w: unsupported merge method %q", errUnmet, method)
	}
	pr, url, err := m.view(ctx, s)
	if err != nil {
		return err
	}
	want := s.Pre["head_sha"]
	switch {
	case pr.State == "MERGED" && pr.HeadSHA == want:
		return nil // do finds it done
	case pr.State != "OPEN":
		return fmt.Errorf("%w: %s is %s", errStale, url, strings.ToLower(pr.State))
	case pr.HeadSHA != want:
		return fmt.Errorf("%w: %s's head is %s, not %s", errStale, url, short(pr.HeadSHA), short(want))
	case pr.IsDraft:
		return fmt.Errorf("%w: %s is a draft", errUnmet, url)
	}
	c := summarizeChecks(pr.Checks)
	if !mergeable(&pr, c) {
		return fmt.Errorf("%w: %s: %s", errUnmet, url, notReady(pr, c))
	}
	// Set the canonical target: never accept a URL or policy in response data.
	pr.URL, pr.Policy, pr.Gate, pr.DetailsFresh = url, nil, nil, true
	if m.policy != nil {
		policy := m.policy(pr)
		pr.Policy = &policy
	}
	q, aliases := gatesQuery([]PR{pr})
	out, err := m.gh(ctx, "", "api", "graphql", "-f", "query="+q)
	if err != nil {
		return err
	}
	gates, err := parseGates(out, aliases, time.Now())
	if err != nil {
		return err
	}
	if gate, ok := gates[url]; ok {
		pr.Gate = &gate
	}
	if pr.Gate != nil && pr.Gate.Head != want {
		return fmt.Errorf("%w: head moved while fetching merge evidence", errStale)
	}
	if !readyToMerge(&pr, c) {
		return fmt.Errorf("%w: %s: %s", errUnmet, url, strings.Join(mergeBlockers(&pr), "; "))
	}
	return nil
}

// notReady says why a PR isn't mergeable yet.
func notReady(pr PR, c Checks) string {
	var why []string
	if d := pr.ReviewDecision; d != "" && d != "APPROVED" {
		why = append(why, "review "+strings.ToLower(strings.ReplaceAll(d, "_", " ")))
	}
	if c.Pending > 0 {
		why = append(why, fmt.Sprintf("%d %s pending", c.Pending, plural(c.Pending, "check")))
	}
	if len(c.Failing) > 0 {
		why = append(why, "failing: "+strings.Join(c.Failing, ", "))
	}
	if ms := pr.MergeState; ms != "" && ms != "CLEAN" && ms != "UNSTABLE" && ms != "HAS_HOOKS" {
		why = append(why, "merge state "+strings.ToLower(ms))
	}
	if len(why) == 0 {
		return "not mergeable"
	}
	return strings.Join(why, "; ")
}

func (m mergeRunner) do(ctx context.Context, s Step, _ string) (string, error) {
	if happened, _, evidence, err := m.probe(ctx, s, ""); err == nil && happened {
		return evidence + " already", nil
	}
	// Re-read evidence immediately before mutation too; a successful pre or
	// a prior auto-merge request is not a durable permission to merge.
	if err := m.pre(ctx, s); err != nil {
		return "", err
	}
	url, err := stepPRURL(s)
	if err != nil {
		return "", err
	}
	args := []string{"pr", "merge", url, "--match-head-commit", s.Pre["head_sha"]}
	if method := s.Args["method"]; method != "" {
		args = append(args, "--"+method)
	}
	// Never arm deferred native auto-merge: it is not bound to this head or
	// to our review/thread policy after a later push. An eligible auto request
	// is executed as an exact-head merge; an ineligible one fails pre above.
	if s.Args["delete_branch"] == "true" {
		args = append(args, "--delete-branch")
	}
	for _, f := range [][2]string{{"subject", "--subject"}, {"body", "--body"}, {"author_email", "--author-email"}} {
		if v := s.Args[f[0]]; v != "" {
			args = append(args, f[1], v)
		}
	}
	if _, err := m.gh(ctx, "", args...); err != nil {
		return "", err
	}
	return fmt.Sprintf("merged %s at %s", url, short(s.Pre["head_sha"])), nil
}

// probe: only a merge at the authorized head proves the effect. A native
// auto-merge request is not head-bound and cannot count as completion.
func (m mergeRunner) probe(ctx context.Context, s Step, _ string) (bool, bool, string, error) {
	pr, url, err := m.view(ctx, s)
	if err != nil {
		return false, false, "", err
	}
	at := pr.HeadSHA == s.Pre["head_sha"]
	switch {
	case at && pr.State == "MERGED":
		return true, true, fmt.Sprintf("merged %s at %s", url, short(pr.HeadSHA)), nil
	}
	return false, true, "", nil
}

// opaqueRunner runs an opaque step a shim lowered: the tool's argv exactly as
// the worker gave it, from its directory. It runs only under an approval of
// the plan's digest, which for class 9 is TJ's. It has no probe, so a crash
// mid-step stops for TJ, and it is never retried.
type opaqueRunner struct {
	run func(ctx context.Context, dir string, args ...string) ([]byte, error)
	// tool runs pulumi and tofu, with the env the shim carried.
	tool func(ctx context.Context, c toolCmd) (toolOut, error)
}

func (opaqueRunner) retrySafe() bool { return false }

func (opaqueRunner) argv(s Step) ([]string, error) {
	var argv []string
	if !slices.Contains(shimTools, s.Args["tool"]) || json.Unmarshal([]byte(s.Args["argv"]), &argv) != nil || len(argv) == 0 {
		return nil, fmt.Errorf("%w: the executor runs only an opaque step a shim lowered (args.tool and args.argv)", errUnmet)
	}
	if s.Args["tool"] == "gh" && len(argv) >= 2 && argv[0] == "pr" && argv[1] == "merge" {
		for _, arg := range argv[2:] {
			name, _, _ := strings.Cut(arg, "=")
			if name == "--admin" {
				return nil, fmt.Errorf("%w: --admin merge is not allowed", errUnmet)
			}
		}
	}
	return argv, nil
}

func (o opaqueRunner) pre(_ context.Context, s Step) error {
	_, err := o.argv(s)
	return err
}

func (o opaqueRunner) do(ctx context.Context, s Step, _ string) (string, error) {
	argv, err := o.argv(s)
	if err != nil {
		return "", err
	}
	var out []byte
	if tool := s.Args["tool"]; tool != "gh" {
		res, terr := o.tool(ctx, toolCmd{Tool: tool, Args: argv, Dir: s.Args["dir"], Env: iacEnvList(Step{Target: tool + ":", Args: s.Args})})
		out, err = res.Stdout, terr
		if err == nil && res.Code != 0 {
			err = fmt.Errorf("%s exited %d: %s", tool, res.Code, tail(string(res.Stderr), 1500))
		}
	} else {
		out, err = o.run(ctx, s.Args["dir"], argv...)
	}
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, errUnknown) {
			err = fmt.Errorf("%w: %w", errUnknown, err)
		}
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if len(text) > 2000 {
		text = text[:2000] + "…"
	}
	return "ran " + s.Text + prefix("\n", text), nil
}

func prefix(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

func (opaqueRunner) probe(context.Context, Step, string) (bool, bool, string, error) {
	return false, false, "", nil
}
