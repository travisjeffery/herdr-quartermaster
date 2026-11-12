package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Acceptance and the verifier (INF-2607): the worker that does a bead
// shouldn't also be the one that says it is done. A PR already gets an
// independent check (review, required checks), so a bead whose work ends at a
// merged PR closes as it always has. Two kinds of bead don't: one with steps
// after the merge (labelled rolling-out, or with Actions runs noted), and one
// with no PR at all (an audit, a design, an investigation). Those close only
// through `quartermaster close`, which runs a read-only verifier with a fresh
// context against the bead's "## Acceptance" criteria and records its verdict
// in the journal. The journal record, not the VERIFY: note, is what close
// trusts: a note is one `bd note` away for the worker being checked.

// acceptHeading starts the criteria section of a bead's description.
const acceptHeading = "## Acceptance"

// VerifierConfig is [verifier] in config.toml.
type VerifierConfig struct {
	// Command runs the verifier: the prompt goes to its stdin, the verdict
	// comes back on stdout. Unset uses defaultVerifierCommand.
	Command []string `toml:"command"`
	// MaxRounds is how many failed verdicts in a row send the bead to TJ
	// (needs_me) instead of back to its worker; 0 means 2.
	MaxRounds int `toml:"max_rounds"`
	// Minutes bounds one run; 0 means 15.
	Minutes int `toml:"minutes"`
}

// defaultVerifierCommand is a headless Claude that can read and nothing else:
// no settings files (whose allow rules may permit writes), no MCP servers, a
// read-only tool set, and dontAsk, which denies anything the allowlist doesn't
// name instead of stopping at a prompt nobody is watching.
var defaultVerifierCommand = []string{
	"claude", "-p",
	"--setting-sources", "",
	"--strict-mcp-config",
	"--no-session-persistence",
	"--permission-mode", "dontAsk",
	"--tools", "Read,Grep,Glob,Bash,WebFetch",
	"--allowedTools", strings.Join([]string{
		"Read", "Grep", "Glob", "WebFetch",
		"Bash(gh pr view:*)", "Bash(gh pr checks:*)", "Bash(gh pr diff:*)",
		"Bash(gh run view:*)", "Bash(gh run list:*)", "Bash(gh release view:*)",
		"Bash(git log:*)", "Bash(git show:*)", "Bash(git diff:*)", "Bash(git status:*)",
		"Bash(git rev-parse:*)", "Bash(git ls-files:*)", "Bash(git ls-tree:*)", "Bash(git cat-file:*)",
		"Bash(git branch --list:*)", "Bash(git merge-base:*)", "Bash(git grep:*)", "Bash(gh pr list:*)", "Bash(gh issue view:*)",
		"Bash(bd show:*)", "Bash(bd list:*)", "Bash(bd search:*)",
		"Bash(ls:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(grep:*)",
		"Bash(kubectl get:*)", "Bash(kubectl describe:*)",
	}, ","),
}

func (v VerifierConfig) command() []string {
	if len(v.Command) > 0 {
		return v.Command
	}
	return defaultVerifierCommand
}

func (v VerifierConfig) maxRounds() int {
	if v.MaxRounds > 0 {
		return v.MaxRounds
	}
	return 2
}

func (v VerifierConfig) timeout() time.Duration {
	if v.Minutes > 0 {
		return time.Duration(v.Minutes) * time.Minute
	}
	return 15 * time.Minute
}

// acceptanceCriteria are the bullet items under the description's
// "## Acceptance" heading, up to the next heading of level 2 or above. A
// bullet's continuation lines join it. None means the bead predates criteria.
func acceptanceCriteria(desc string) []string {
	var out []string
	in := false
	for line := range strings.SplitSeq(desc, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "###") {
			in = strings.EqualFold(strings.TrimRight(t, " :"), acceptHeading)
			continue
		}
		if !in || t == "" {
			continue
		}
		if item, ok := bullet(t); ok {
			out = append(out, item)
		} else if len(out) > 0 {
			out[len(out)-1] += " " + t
		}
	}
	return out
}

// bullet is a list item's text: "- x", "* x", "• x" or "1. x".
func bullet(t string) (string, bool) {
	for _, p := range []string{"- [ ] ", "- [x] ", "- ", "* ", "• "} {
		if s, ok := strings.CutPrefix(t, p); ok {
			return strings.TrimSpace(s), true
		}
	}
	if i := strings.IndexAny(t, ".)"); i > 0 && i <= 3 && strings.Trim(t[:i], "0123456789") == "" && len(t) > i+1 && t[i+1] == ' ' {
		return strings.TrimSpace(t[i+2:]), true
	}
	return "", false
}

// verifierScope says whether closing b needs a verifier pass, and why. Its
// work ends at a merged PR, and so needs none, when its notes link a PR and
// nothing follows the merge: no rolling-out label and no Actions run noted.
// The runs count because a worker told to remove the label when the rollout
// ends may do so before it closes.
func verifierScope(b Bead) (bool, string) {
	switch {
	case b.HasLabel(LabelRollingOut):
		return true, "labelled " + LabelRollingOut
	case len(NotedRuns(b.Notes)) > 0:
		return true, "Actions runs noted after the work"
	case len(NotedPRs(b.Notes)) == 0:
		return true, "no PR noted"
	}
	return false, "work ends at its PR"
}

// verifierPrompt is everything the verifier sees: the task, the criteria and
// where to look. It leaves out the worker's notes and close reason, which are
// the conclusion being checked; only the URLs in them and the results of
// commands TJ ran (RAN: notes) go in, as pointers.
func verifierPrompt(cfg Config, b Bead) string {
	var s strings.Builder
	fmt.Fprintf(&s, "You are an independent verifier for bead %s: %s\n", b.ID, b.Title)
	s.WriteString(`Another agent did this work and wants to close it. Decide, from live evidence only, whether each acceptance criterion holds now. You are read-only: never edit files, run anything that changes state, push, comment or close. Check, don't trust: a claim in a description or PR body is not evidence until you confirm it (the PR is merged, the run succeeded, the file says so on the default branch, the resource exists).
Your shell runs only an allowlist of read-only commands (gh pr/run view, git log/show/diff/ls-files/rev-parse, bd show, kubectl get, ...), one plain command per call: no cd (you are already in the repository), no ;, &&, pipes, loops or redirections, which are refused. Use Read, Grep and Glob for files.

`)
	crit := acceptanceCriteria(b.Description)
	if len(crit) == 0 {
		s.WriteString("The bead has no \"" + acceptHeading + "\" section. Derive 2-4 checkable criteria from the description below, check those, and set \"derived\": true.\n\n")
	} else {
		s.WriteString("Acceptance criteria:\n")
		for i, c := range crit {
			fmt.Fprintf(&s, "%d. %s\n", i+1, c)
		}
		s.WriteString("\n")
	}
	s.WriteString("Description:\n")
	s.WriteString(strings.TrimSpace(b.Description))
	s.WriteString("\n\nWhere to look:\n")
	fmt.Fprintf(&s, "- Repository: %s (your working directory); the work's branch is %s, and %s is the default branch.\n", expandHome(cfg.repoFor(b)), branchFor(cfg.BranchPrefix, b), cfg.BaseBranch)
	fmt.Fprintf(&s, "- `bd show %s` for the bead's labels and status (ignore its notes: they are the claim under test).\n", b.ID)
	for _, u := range NotedPRs(b.Notes) {
		fmt.Fprintf(&s, "- PR: %s\n", u)
	}
	for _, u := range NotedRuns(b.Notes) {
		fmt.Fprintf(&s, "- Actions run: %s\n", u)
	}
	for l := range strings.SplitSeq(b.Notes, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "RAN:") {
			fmt.Fprintf(&s, "- Reported by TJ: %s\n", l)
		}
	}
	s.WriteString(`
End your reply with exactly one line, the last line, starting "VERDICT " and followed by JSON on the same line:
VERDICT {"verdict":"pass"|"fail","derived":true|false,"criteria":[{"criterion":"...","pass":true|false,"evidence":"what you checked and saw, with URLs or paths"}]}
"pass" only if every criterion passed. A criterion you could not check fails, with what blocked you as its evidence.
`)
	return s.String()
}

type criterionResult struct {
	Criterion string `json:"criterion"`
	Pass      bool   `json:"pass"`
	Evidence  string `json:"evidence"`
}

type verifierResult struct {
	Verdict  string            `json:"verdict"`
	Derived  bool              `json:"derived"`
	Criteria []criterionResult `json:"criteria"`
}

// passed holds the verdict to its own criteria: a "pass" with a failing or
// no criterion is a fail.
func (r verifierResult) passed() bool {
	if r.Verdict != "pass" || len(r.Criteria) == 0 {
		return false
	}
	for _, c := range r.Criteria {
		if !c.Pass {
			return false
		}
	}
	return true
}

// failing is the criteria that failed, one line each.
func (r verifierResult) failing() []string {
	var out []string
	for _, c := range r.Criteria {
		if !c.Pass {
			out = append(out, fmt.Sprintf("✗ %s — %s", c.Criterion, c.Evidence))
		}
	}
	if len(out) == 0 && !r.passed() {
		out = append(out, "✗ the verifier returned no passing criteria")
	}
	return out
}

// summary is the verdict for a VERIFY: note: every criterion with its evidence.
func (r verifierResult) summary() string {
	var parts []string
	if r.Derived {
		parts = append(parts, "no "+acceptHeading+" section; criteria derived from the description")
	}
	for _, c := range r.Criteria {
		mark := "✓"
		if !c.Pass {
			mark = "✗"
		}
		parts = append(parts, fmt.Sprintf("%s %s — %s", mark, c.Criterion, c.Evidence))
	}
	return strings.Join(parts, "; ")
}

// parseVerdict reads the last VERDICT line of the verifier's output.
func parseVerdict(out string) (verifierResult, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "VERDICT ")
		if !ok {
			continue
		}
		var r verifierResult
		if err := json.Unmarshal([]byte(rest), &r); err != nil {
			return r, fmt.Errorf("verifier's VERDICT line is not JSON: %w", err)
		}
		return r, nil
	}
	return verifierResult{}, errors.New("verifier gave no VERDICT line")
}

// closeEnv is everything `quartermaster close` reads or changes.
type closeEnv struct {
	show      func(id string) (Bead, error)
	close     func(id, reason string) error
	note      func(id, text string) error
	setStatus func(id, status string) error
	setLabel  func(id, label string, on bool) error
	// enforce is the dial's check for closing on someone's behalf.
	enforce func(b Bead, by string) (string, error)
	// verify runs the verifier on prompt in dir and returns its stdout.
	verify  func(dir, prompt string) (string, error)
	journal func(since time.Time) ([]journalRecord, error)
	record  func(journalRecord) error
	now     func() time.Time
}

func liveCloseEnv(cfg Config) closeEnv {
	bd := Beads{}
	return closeEnv{
		show:      bd.Show,
		close:     func(id, reason string) error { _, err := bd.run("close", id, "--reason", reason); return err },
		note:      bd.Note,
		setStatus: bd.SetStatus,
		setLabel:  bd.SetLabel,
		enforce:   func(b Bead, by string) (string, error) { return cfg.enforce(b, "close", by) },
		verify:    func(dir, prompt string) (string, error) { return runVerifier(cfg.Verifier, dir, prompt) },
		journal:   readJournal,
		record:    appendJournal,
		now:       time.Now,
	}
}

// runVerifier runs the configured command in dir with prompt on stdin.
func runVerifier(v VerifierConfig, dir, prompt string) (string, error) {
	argv := v.command()
	ctx, cancel := context.WithTimeout(context.Background(), v.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	// The verifier is its own actor, never the worker's.
	cmd.Env = append(os.Environ(), "BEADS_ACTOR=verifier")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("verifier %s: %w: %s", argv[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// passFresh is how long a recorded pass lets a later close skip the verifier,
// so a close that failed for another reason can be retried without a rerun.
const passFresh = time.Hour

var errVerifyFailed = errors.New("verifier failed the bead")

// verifyRounds is the bead's verify records since its last pass: the failed
// rounds in a row, and whether the latest record is a pass at or after since.
func verifyRounds(recs []journalRecord, id string, since time.Time) (fails int, fresh bool) {
	for _, r := range recs {
		if r.Kind != journalVerify || r.Bead != id {
			continue
		}
		if r.Pass {
			fails, fresh = 0, !r.At.Before(since)
		} else {
			fails, fresh = fails+1, false
		}
	}
	return fails, fresh
}

// closeBead is `quartermaster close <bead> --reason R [--by WHO]`, the close
// path that keeps a worker from certifying its own in-scope bead. by is who
// asked: TJ closes as the evaluator and skips the verifier; anyone else needs
// the dial to allow closing (class 3) and, for an in-scope bead, a verifier
// pass. A fail goes back to the caller with the failing criteria; maxRounds
// fails in a row send the bead to TJ.
func closeBead(cfg Config, env closeEnv, id, reason, by string, w io.Writer) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("close needs --reason \"<what shipped and how it was verified>\"")
	}
	b, err := env.show(id)
	if err != nil {
		return err
	}
	if b.Status == StatusClosed {
		return fmt.Errorf("%s is already closed", id)
	}
	prefix, err := env.enforce(b, by)
	if err != nil {
		return err
	}
	inScope, why := verifierScope(b)
	note := func(text string) {
		if err := env.note(id, text); err != nil {
			fmt.Fprintf(w, "note %s: %v\n", id, err)
		}
	}
	switch {
	case by == "TJ":
		if inScope {
			note("VERIFY: skipped [TJ] TJ closed it as the evaluator (" + why + ")")
		}
	case !inScope:
		fmt.Fprintf(w, "%s: no verifier (%s)\n", id, why)
	default:
		if err := verify(cfg, env, b, reason, why, note, w); err != nil {
			return err
		}
	}
	if b.HasLabel(LabelRollingOut) {
		if err := env.setLabel(id, LabelRollingOut, false); err != nil {
			return err
		}
	}
	if err := env.close(id, reason); err != nil {
		return err
	}
	if prefix != "" {
		note(prefix + " close — " + reason)
	}
	fmt.Fprintf(w, "closed %s\n", id)
	return nil
}

// verify runs the verifier on an in-scope bead unless a fresh pass is on
// record, and records the verdict. It returns nil only on a pass.
func verify(cfg Config, env closeEnv, b Bead, reason, why string, note func(string), w io.Writer) error {
	now := env.now()
	recs, err := env.journal(time.Time{})
	if err != nil {
		return err
	}
	fails, fresh := verifyRounds(recs, b.ID, now.Add(-passFresh))
	if fresh {
		fmt.Fprintf(w, "%s: verifier passed within the last %s\n", b.ID, passFresh)
		return nil
	}
	fmt.Fprintf(w, "%s: running the verifier (%s)...\n", b.ID, why)
	out, runErr := env.verify(expandHome(cfg.repoFor(b)), verifierPrompt(cfg, b))
	res, err := parseVerdict(out)
	if runErr != nil {
		// A verifier that didn't run says nothing about the work: no round
		// is spent, and the caller sees why.
		return fmt.Errorf("%s: %w", b.ID, runErr)
	}
	if err != nil {
		res = verifierResult{Verdict: "fail", Criteria: []criterionResult{{Criterion: "the verifier returns a verdict", Evidence: err.Error()}}}
	}
	pass := res.passed()
	if err := env.record(journalRecord{At: now, Kind: journalVerify, Bead: b.ID, Class: -1, Action: "close", Pass: pass, Text: res.summary()}); err != nil {
		return err
	}
	if pass {
		note("VERIFY: pass [verifier] " + res.summary())
		fmt.Fprintf(w, "%s: verifier passed: %s\n", b.ID, res.summary())
		return nil
	}
	fails++
	limit := cfg.Verifier.maxRounds()
	failing := strings.Join(res.failing(), "\n")
	if fails < limit {
		note(fmt.Sprintf("VERIFY: fail [verifier] round %d/%d: %s", fails, limit, res.summary()))
		fmt.Fprintf(w, "%s: verifier failed (round %d of %d). The bead stays open; fix these and run close again:\n%s\n", b.ID, fails, limit, failing)
		return errVerifyFailed
	}
	note(fmt.Sprintf("VERIFY: fail [verifier] round %d/%d, escalated to TJ. Verifier: %s\nWorker's close reason: %s\nRecommendation: TJ reads both and either closes with `quartermaster close %s --by TJ --reason \"...\"` or sends the worker back with what is missing.\nCLASS: 3 — close %s over the verifier's fail", fails, limit, res.summary(), reason, b.ID, b.ID))
	if err := env.setStatus(b.ID, StatusNeedsMe); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: verifier failed %d rounds in a row; the bead is needs_me for TJ:\n%s\n", b.ID, fails, failing)
	return errVerifyFailed
}
