package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// resumeArgs continue an agent's most recent conversation in its working
// directory, which is the bead's own worktree.
var resumeArgs = map[string][]string{
	"claude": {"--continue"},
	"codex":  {"resume", "--last"},
	"muse":   {"resume", "--last"},
}

type worktreeLister func(repo string) ([]Worktree, error)

// cachedWorktrees lists each repository once however many beads live in it.
func cachedWorktrees(list worktreeLister) worktreeLister {
	seen := map[string][]Worktree{}
	return func(repo string) ([]Worktree, error) {
		if wts, ok := seen[repo]; ok {
			return wts, nil
		}
		wts, err := list(repo)
		if err == nil {
			seen[repo] = wts
		}
		return wts, err
	}
}

// beadWorktree finds the linked worktree a bead was worked in: on its own
// branch, or on its PR's head when the work moved to another branch.
func beadWorktree(cfg Config, b Bead, prs map[string]PR, wts []Worktree) (Worktree, bool) {
	branches := []string{branchFor(cfg.BranchPrefix, b)}
	if pr, ok := prs[b.ID]; ok && pr.Head != "" {
		branches = append(branches, pr.Head)
	}
	for _, w := range wts {
		if w.Linked && slices.Contains(branches, w.Branch) {
			return w, true
		}
	}
	return Worktree{}, false
}

type Resumable struct {
	Bead     Bead
	Worktree Worktree
}

// orphans splits the beads with no live agent into those with a worktree to
// resume in and those without one.
func orphans(cfg Config, beads []Bead, live map[string]bool, prs map[string]PR, list worktreeLister) ([]Resumable, []Bead, error) {
	var resumable []Resumable
	var bare []Bead
	for _, b := range beads {
		if live[agentName(b.ID)] {
			continue
		}
		wts, err := list(cfg.repoFor(b))
		if err != nil {
			return nil, nil, err
		}
		if w, ok := beadWorktree(cfg, b, prs, wts); ok {
			resumable = append(resumable, Resumable{Bead: b, Worktree: w})
		} else {
			bare = append(bare, b)
		}
	}
	return resumable, bare, nil
}

// staleClaims are the in-progress beads among bare untouched for at least age.
// Blocked and needs_me beads keep their status, and a bead with a PR is
// delivered work to close, not a claim to release.
func staleClaims(bare []Bead, prs map[string]PR, now time.Time, age time.Duration) []Bead {
	var out []Bead
	for _, b := range bare {
		if _, hasPR := prs[b.ID]; !hasPR && b.Status == StatusInProgress && now.Sub(b.UpdatedAt) >= age {
			out = append(out, b)
		}
	}
	return out
}

// staleAge is when a bare claim counts as stale, in context and by default for
// quartermaster stale.
const staleAge = 7 * 24 * time.Hour

func daysSince(t, now time.Time) int { return int(now.Sub(t).Hours() / 24) }

// worktreed is the set of no-agent threads that have a worktree, or nil when
// the worktrees could not be listed.
func worktreed(cfg Config, h Herdr, threads []Thread, prs map[string]PR) map[string]bool {
	var noAgent []Bead
	for _, t := range threads {
		if classify(t) == GroupNoAgent {
			noAgent = append(noAgent, t.Bead)
		}
	}
	res, _, err := orphans(cfg, noAgent, nil, prs, cachedWorktrees(h.Worktrees))
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, r := range res {
		set[r.Bead.ID] = true
	}
	return set
}

// pickResumable keeps the named beads, or all of them when none are named, and
// reports the named beads that cannot be resumed.
func pickResumable(res []Resumable, ids []string) ([]Resumable, []string) {
	if len(ids) == 0 {
		return res, nil
	}
	var picked []Resumable
	var missing []string
	for _, id := range ids {
		i := slices.IndexFunc(res, func(r Resumable) bool { return r.Bead.ID == id })
		if i < 0 {
			missing = append(missing, id)
			continue
		}
		picked = append(picked, res[i])
	}
	return picked, missing
}

// resumeKind prefers an explicit kind, then the kind that claimed the bead
// (dispatch claims as the agent kind), then the configured worker.
func resumeKind(cfg Config, b Bead, kind string) string {
	switch {
	case kind == "" && validKinds[b.Assignee]:
		return b.Assignee
	case kind == "":
		kind = cfg.WorkerAgent
	}
	if kind == "auto" {
		return autoAgent(time.Now())
	}
	return kind
}

func freePane(panes []Pane) (string, bool) {
	for _, p := range panes {
		if p.Agent == "" {
			return p.ID, true
		}
	}
	return "", false
}

// unattended is orphans over the live beads, agents and worktrees.
func unattended(cfg Config, h Herdr, prs map[string]PR) ([]Resumable, []Bead, error) {
	active, err := Beads{}.Active()
	if err != nil {
		return nil, nil, err
	}
	agents, err := h.Agents()
	if err != nil {
		return nil, nil, err
	}
	live := map[string]bool{}
	for _, a := range agents {
		live[a.Name] = true
	}
	return orphans(cfg, active, live, prs, cachedWorktrees(h.Worktrees))
}

func resume(cfg Config, h Herdr, ids []string, kind string) ([]string, error) {
	if kind != "" && !validKinds[kind] {
		return nil, fmt.Errorf("unknown agent kind %q (%s)", kind, kindList())
	}
	res, _, err := unattended(cfg, h, loadState().PRs)
	if err != nil {
		return nil, err
	}
	picked, missing := pickResumable(res, ids)
	var out []string
	var errs []error
	for _, id := range missing {
		errs = append(errs, fmt.Errorf("%s: nothing to resume (not active, already has an agent, or no worktree)", id))
	}
	for _, r := range picked {
		msg, err := resumeOne(cfg, h, r, resumeKind(cfg, r.Bead, kind))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Bead.ID, err))
			continue
		}
		out = append(out, msg)
	}
	if len(picked) == 0 && len(ids) == 0 {
		out = append(out, "nothing to resume")
	}
	return out, errors.Join(errs...)
}

func resumeOne(cfg Config, h Herdr, r Resumable, kind string) (string, error) {
	if !validKinds[kind] {
		return "", fmt.Errorf("unknown agent kind %q (%s)", kind, kindList())
	}
	id, name := r.Bead.ID, agentName(r.Bead.ID)
	var pane string
	if r.Worktree.WorkspaceID == "" {
		opened, err := h.WorktreeOpen(cfg.repoFor(r.Bead), r.Worktree.Branch, name, false)
		if err != nil {
			return "", fmt.Errorf("open worktree %s: %w", r.Worktree.Branch, err)
		}
		pane = opened.PaneID
		_ = h.TabRename(pane[:strings.LastIndex(pane, ":")]+":t1", shortTitle(r.Bead.Title, 34))
	} else {
		panes, err := h.Panes(r.Worktree.WorkspaceID)
		if err != nil {
			return "", err
		}
		var ok bool
		if pane, ok = freePane(panes); !ok {
			return "", fmt.Errorf("workspace %s has no pane without an agent", r.Worktree.WorkspaceID)
		}
	}
	text := fmt.Sprintf("[quartermaster] You were resumed after your agent exited. Run `bd show %s` and continue.", id)
	if err := h.AgentStart(name, kind, pane, cfg.agentArgs(kind, resumeArgs[kind]...)...); err != nil {
		if !strings.Contains(err.Error(), "agent_not_ready") {
			return "", fmt.Errorf("start %s in %s: %w", kind, pane, err)
		}
		if err := queueBrief(name, text); err != nil {
			return "", err
		}
		h.Notify("quartermaster: "+id+" is waiting for you", "Answer its startup prompt in "+pane+"; the resume prompt follows.")
		return fmt.Sprintf("resumed %s on %s (%s); it is at a startup prompt, the prompt is queued", kind, id, pane), nil
	}
	if err := h.Prompt(name, text); err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	return fmt.Sprintf("resumed %s on %s (%s)", kind, id, pane), nil
}

func stale(cfg Config, h Herdr, days int, release bool) ([]string, error) {
	if days < 0 {
		return nil, fmt.Errorf("--days must not be negative")
	}
	prs := loadState().PRs
	_, bare, err := unattended(cfg, h, prs)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	claims := staleClaims(bare, prs, now, time.Duration(days)*24*time.Hour)
	if len(claims) == 0 {
		return []string{fmt.Sprintf("no stale claims untouched %dd+", days)}, nil
	}
	var out []string
	var errs []error
	for _, b := range claims {
		line := fmt.Sprintf("- %s untouched %dd: %s", b.ID, daysSince(b.UpdatedAt, now), shortTitle(b.Title, 70))
		if release {
			note := fmt.Sprintf("released by quartermaster stale: no agent or worktree for %dd", days)
			if err := (Beads{}).Note(b.ID, note); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := (Beads{}).SetStatus(b.ID, StatusOpen); err != nil {
				errs = append(errs, err)
				continue
			}
			line += " (released)"
		}
		out = append(out, line)
	}
	return out, errors.Join(errs...)
}
