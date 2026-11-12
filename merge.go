package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The merge runners carry out github.pr.merge and github.pr.automerge. They
// take neither the worker's word nor the ticker's: before each attempt they
// read the PR's head, review decision, merge state and checks from GitHub,
// and they merge with --match-head-commit, so GitHub refuses a head that moved
// between the check and the act. That compare-and-swap makes them retry-safe,
// and the probe is the PR merged (or set to auto-merge) at that head.

// errUnmet is a precondition that doesn't hold yet, such as checks still
// running: the step fails cleanly, and a later run of the same plan can pass.
var errUnmet = errors.New("not ready")

func init() {
	opRunners["github.pr.merge"] = mergeRunner{gh: ghExec}
	opRunners["github.pr.automerge"] = mergeRunner{auto: true, gh: ghExec}
	opRunners[opOpaque] = opaqueRunner{run: ghExec}
}

type mergeRunner struct {
	auto bool
	gh   func(ctx context.Context, dir string, args ...string) ([]byte, error)
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

// pre reads the PR from GitHub. Its head must still be the plan's. A merge
// also needs what branch protection needs: approved (or no review required),
// checks done and passing, a clean merge state. Auto-merge waits for those
// itself, so it needs only an open PR that isn't a draft.
func (m mergeRunner) pre(ctx context.Context, s Step) error {
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
	case m.auto:
		return nil
	}
	if c := summarizeChecks(pr.Checks); !mergeable(&pr, c) {
		return fmt.Errorf("%w: %s: %s; gh pr merge --auto merges it when they pass", errUnmet, url, notReady(pr, c))
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
	url, err := stepPRURL(s)
	if err != nil {
		return "", err
	}
	args := []string{"pr", "merge", url, "--match-head-commit", s.Pre["head_sha"]}
	if method := s.Args["method"]; method != "" {
		args = append(args, "--"+method)
	}
	if m.auto {
		args = append(args, "--auto")
	}
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
	if m.auto {
		return fmt.Sprintf("auto-merge on %s at %s", url, short(s.Pre["head_sha"])), nil
	}
	return fmt.Sprintf("merged %s at %s", url, short(s.Pre["head_sha"])), nil
}

// probe: the PR merged at the step's head, or for auto-merge, auto-merge on
// at that head (or already merged there).
func (m mergeRunner) probe(ctx context.Context, s Step, _ string) (bool, bool, string, error) {
	pr, url, err := m.view(ctx, s)
	if err != nil {
		return false, false, "", err
	}
	at := pr.HeadSHA == s.Pre["head_sha"]
	switch {
	case at && pr.State == "MERGED":
		return true, true, fmt.Sprintf("merged %s at %s", url, short(pr.HeadSHA)), nil
	case at && m.auto && pr.State == "OPEN" && pr.AutoMerge != nil:
		return true, true, fmt.Sprintf("auto-merge on %s at %s", url, short(pr.HeadSHA)), nil
	}
	return false, true, "", nil
}

// opaqueRunner runs an opaque step a shim lowered: the tool's argv exactly as
// the worker gave it, from its directory. It runs only under an approval of
// the plan's digest, which for class 9 is TJ's. It has no probe, so a crash
// mid-step stops for TJ, and it is never retried.
type opaqueRunner struct {
	run func(ctx context.Context, dir string, args ...string) ([]byte, error)
}

func (opaqueRunner) retrySafe() bool { return false }

func (opaqueRunner) argv(s Step) ([]string, error) {
	var argv []string
	if s.Args["tool"] != "gh" || json.Unmarshal([]byte(s.Args["argv"]), &argv) != nil || len(argv) == 0 {
		return nil, fmt.Errorf("%w: the executor runs only an opaque gh step a shim lowered (args.tool and args.argv)", errUnmet)
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
	out, err := o.run(ctx, s.Args["dir"], argv...)
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
