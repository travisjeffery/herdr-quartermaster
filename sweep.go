package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type sweepCandidate struct {
	Repo        string
	Path        string
	Branch      string
	WorkspaceID string
	Reasons     []string
	Merged      bool
	Prunable    bool
	Dirty       bool
	Unpushed    int
	Agent       string
	CheckErr    string
	ActiveBead  string
}

// sweepEnv is everything sweep reads or changes, so the decision and the
// removal order are testable without git, gh, bd or herdr.
type sweepEnv struct {
	worktrees       func(repo string) ([]Worktree, error)
	mergedHeads     func(repo string) (map[string]bool, error)
	closedBeads     func() ([]Bead, error)
	activeBeads     func() ([]Bead, error)
	prHeads         func() map[string]string
	agents          func() ([]Agent, error)
	exists          func(path string) bool
	git             func(dir string, args ...string) (string, error)
	removeWorkspace func(id string) error
}

func liveSweepEnv(h Herdr) sweepEnv {
	return sweepEnv{
		worktrees:   h.Worktrees,
		mergedHeads: func(repo string) (map[string]bool, error) { return GH{repo: repo}.MergedHeads() },
		closedBeads: func() ([]Bead, error) { return Beads{}.list("--status", StatusClosed) },
		activeBeads: Beads{}.Active,
		prHeads: func() map[string]string {
			heads := map[string]string{}
			for id, pr := range loadState().PRs {
				heads[pr.Head] = id
			}
			return heads
		},
		agents: h.Agents,
		exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		git:             gitRun,
		removeWorkspace: func(id string) error { return h.WorktreeRemove(id, false) },
	}
}

// MergedHeads is the head branch of every merged PR of the user's, in one
// call: only headRefName, since heavy fields across 300 PRs time out.
func (g GH) MergedHeads() (map[string]bool, error) {
	out, err := g.run("pr", "list", "--author", "@me", "--state", "merged", "--limit", "300", "--json", "headRefName")
	if err != nil {
		return nil, err
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	heads := make(map[string]bool, len(prs))
	for _, pr := range prs {
		heads[pr.Head] = true
	}
	return heads, nil
}

func gitRun(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// beadForBranch finds the bead a quartermaster branch was cut for: one whose own
// name the branch is before one it merely starts with, then the most specific.
// A parent titled "2 things" owns tj/backend-x-2-things although its child
// backend-x.2 would also match it.
func beadForBranch(prefix, branch string, beads []Bead) (Bead, bool) {
	var best Bead
	bestScore := 0
	for _, b := range beads {
		if !onBeadBranch(prefix, branch, b) {
			continue
		}
		score := 1
		if branch == prefix+agentName(b.ID) || branch == branchFor(prefix, b) {
			score = 2
		}
		if score > bestScore || score == bestScore && len(b.ID) > len(best.ID) {
			best, bestScore = b, score
		}
	}
	return best, bestScore > 0
}

// classifySweep picks the linked worktrees that are finished with: merged PR,
// closed bead, or gone from disk. The main checkout is never a candidate.
func classifySweep(repo, prefix string, wts []Worktree, merged map[string]bool, closed []Bead) []sweepCandidate {
	var out []sweepCandidate
	for _, w := range wts {
		if !w.Linked || filepath.Clean(w.Path) == filepath.Clean(repo) {
			continue
		}
		c := sweepCandidate{Repo: repo, Path: w.Path, Branch: w.Branch, WorkspaceID: w.WorkspaceID, Prunable: w.Prunable}
		if w.Branch != "" && merged[w.Branch] {
			c.Merged = true
			c.Reasons = append(c.Reasons, "merged")
		}
		if b, ok := beadForBranch(prefix, w.Branch, closed); ok {
			c.Reasons = append(c.Reasons, b.ID+" closed")
		}
		if w.Prunable {
			c.Reasons = append(c.Reasons, "prunable")
		}
		if len(c.Reasons) > 0 {
			out = append(out, c)
		}
	}
	return out
}

func inside(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func inspectSweep(c *sweepCandidate, env sweepEnv, agents []Agent) {
	for _, a := range agents {
		if a.Cwd != "" && inside(a.Cwd, c.Path) {
			c.Agent = a.Name
			break
		}
	}
	if c.Prunable && !env.exists(c.Path) {
		return
	}
	status, err := env.git(c.Path, "status", "--porcelain")
	if err != nil {
		c.CheckErr = err.Error()
		return
	}
	c.Dirty = strings.TrimSpace(status) != ""
	count, err := env.git(c.Path, "rev-list", "HEAD", "--not", "--remotes", "--count")
	if err != nil {
		c.CheckErr = err.Error()
		return
	}
	if c.Unpushed, err = strconv.Atoi(strings.TrimSpace(count)); err != nil {
		c.CheckErr = "rev-list: " + err.Error()
	}
}

// keepReason is why a candidate must stay; empty means it is safe to remove.
func keepReason(c sweepCandidate) string {
	var why []string
	if c.CheckErr != "" {
		why = append(why, "could not check: "+c.CheckErr)
	}
	if c.Dirty {
		why = append(why, "dirty")
	}
	if c.Unpushed > 0 {
		why = append(why, fmt.Sprintf("%d unpushed", c.Unpushed))
	}
	if c.Agent != "" {
		why = append(why, "agent "+c.Agent+" in it")
	}
	if c.ActiveBead != "" {
		why = append(why, "bead "+c.ActiveBead+" still active")
	}
	return strings.Join(why, ", ")
}

func removeSwept(c sweepCandidate, env sweepEnv) (string, error) {
	if c.WorkspaceID != "" {
		if err := env.removeWorkspace(c.WorkspaceID); err != nil {
			return "", err
		}
	} else if _, err := env.git(c.Repo, "worktree", "remove", c.Path); err != nil {
		return "", err
	}
	msg := "removed " + tildePath(c.Path)
	if c.Merged {
		if _, err := env.git(c.Repo, "branch", "-D", c.Branch); err == nil {
			msg += ", deleted merged branch " + c.Branch
		} else {
			msg += ", branch " + c.Branch + " not deleted: " + err.Error()
		}
	}
	return msg, nil
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && inside(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// sweep reports, and with yes removes, finished linked worktrees across every
// repo. Without yes it changes nothing.
func sweep(cfg Config, env sweepEnv, yes bool, w io.Writer) error {
	closed, err := env.closedBeads()
	if err != nil {
		return err
	}
	agents, err := env.agents()
	if err != nil {
		return err
	}
	// A merged, clean worktree can still belong to live work (a rollout parked
	// on a deploy, an agent that exited): never sweep an active bead's worktree.
	active, err := env.activeBeads()
	if err != nil {
		return err
	}
	var heads map[string]string
	if env.prHeads != nil {
		heads = env.prHeads()
	}
	var cands []sweepCandidate
	for _, repo := range cfg.allRepos() {
		wts, err := env.worktrees(repo)
		if err != nil {
			return err
		}
		merged, err := env.mergedHeads(repo)
		if err != nil {
			return err
		}
		cands = append(cands, classifySweep(repo, cfg.BranchPrefix, wts, merged, closed)...)
	}
	for i := range cands {
		inspectSweep(&cands[i], env, agents)
		cands[i].ActiveBead = activeBeadFor(cfg.BranchPrefix, cands[i].Branch, active, heads)
	}
	if len(cands) == 0 {
		fmt.Fprintln(w, "nothing to sweep")
		return nil
	}

	removable := 0
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tBRANCH\tREASON\tDIRTY\tUNPUSHED\tAGENT\tWORKSPACE\tSWEEP")
	for _, c := range cands {
		action := "remove"
		if why := keepReason(c); why != "" {
			action = "keep: " + why
		} else {
			removable++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", tildePath(c.Path), orDash(c.Branch), strings.Join(c.Reasons, ", "),
			yesNo(c.Dirty), c.Unpushed, orDash(c.Agent), orDash(c.WorkspaceID), action)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d candidate(s): %d removable, %d kept\n", len(cands), removable, len(cands)-removable)
	if !yes {
		if removable > 0 {
			fmt.Fprintln(w, "dry run: pass --yes to remove the removable ones")
		}
		return nil
	}

	fmt.Fprintln(w)
	failed := 0
	for _, c := range cands {
		if why := keepReason(c); why != "" {
			fmt.Fprintf(w, "kept %s: %s\n", tildePath(c.Path), why)
			continue
		}
		msg, err := removeSwept(c, env)
		if err != nil {
			failed++
			fmt.Fprintf(w, "failed %s: %v\n", tildePath(c.Path), err)
			continue
		}
		fmt.Fprintln(w, msg)
	}
	if failed > 0 {
		return fmt.Errorf("%d removal(s) failed", failed)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// activeBeadFor names the active bead a branch belongs to: by quartermaster's
// branch naming or by the PR head the ticker recorded for it.
func activeBeadFor(prefix, branch string, active []Bead, heads map[string]string) string {
	if b, ok := beadForBranch(prefix, branch, active); ok {
		return b.ID
	}
	if id, ok := heads[branch]; ok && branch != "" {
		for _, b := range active {
			if b.ID == id {
				return id
			}
		}
	}
	return ""
}
