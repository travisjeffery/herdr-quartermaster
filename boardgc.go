package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// gcItem is one thing the tidy view offers to clean up, the evidence for it,
// and which of the bulk actions apply. A bead appears once, its findings merged.
type gcItem struct {
	bead     Bead
	kinds    []string // stale claim, likely stale, auto-closed, leftover
	evidence []string
	// agent is a live agent still named for the bead, which enter focuses.
	agent string
	// keep is why its worktree must stay; resolve is refused while it is set.
	keep string

	canRelease bool // a stale claim: back to open
	canClose   bool // not closed yet: close with a reason
	canResolve bool // closed with a linked worktree: quartermaster resolve
	canDismiss bool // an unhandled inbox item: mark it done
}

// gcInputs is everything the tidy view is built from. closed is bd list
// --brief, which keeps close_reason; candidates are the leftover worktrees
// classifySweep found, already inspected.
type gcInputs struct {
	active     []Bead
	closed     []Bead
	agents     []Agent
	prs        map[string]PR
	inbox      []InboxItem
	bare       []Bead // active beads with no agent and no worktree
	candidates []sweepCandidate
}

// gcItems builds the tidy view's list: stale claims first, then beads flagged
// likely stale, then closed beads still holding a worktree or an agent.
func gcItems(prefix string, in gcInputs, now time.Time) []gcItem {
	byID := map[string]*gcItem{}
	var order []string
	item := func(b Bead) *gcItem {
		if it, ok := byID[b.ID]; ok {
			return it
		}
		it := &gcItem{bead: b, canClose: b.Status != StatusClosed}
		byID[b.ID] = it
		order = append(order, b.ID)
		return it
	}
	agentFor := map[string]Agent{}
	for _, a := range in.agents {
		agentFor[a.Name] = a
	}

	for _, b := range staleClaims(in.bare, in.prs, now, staleAge) {
		it := item(b)
		it.kinds = append(it.kinds, "stale claim")
		it.canRelease = true
		ev := fmt.Sprintf("in_progress, untouched %dd (since %s); no agent, no worktree, no PR", daysSince(b.UpdatedAt, now), b.UpdatedAt.Local().Format("Jan 2"))
		if b.Assignee != "" {
			ev += "; claimed by " + b.Assignee
		}
		it.evidence = append(it.evidence, ev)
		if latest, _ := notesSplit(b.Notes); latest != "" {
			it.evidence = append(it.evidence, "last note: "+latest)
		}
	}

	active := map[string]Bead{}
	for _, b := range in.active {
		active[b.ID] = b
	}
	closed := map[string]Bead{}
	for _, b := range in.closed {
		closed[b.ID] = b
	}
	// The latest unhandled item of each kind per bead.
	latest := map[string]InboxItem{}
	for _, it := range in.inbox {
		if it.Kind != EventLikelyStale && it.Kind != EventAutoClosed {
			continue
		}
		k := it.Bead + "\x00" + string(it.Kind)
		if prev, ok := latest[k]; !ok || it.At.After(prev.At) {
			latest[k] = it
		}
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ev := latest[k]
		b, ok := active[ev.Bead]
		if !ok {
			b, ok = closed[ev.Bead]
		}
		if !ok {
			b = Bead{ID: ev.Bead, Status: StatusClosed}
		}
		if ev.Kind == EventLikelyStale && b.Status == StatusClosed {
			// Closed since it was flagged: nothing left to decide but the inbox item.
			it := item(b)
			it.kinds = append(it.kinds, "likely stale, now closed")
			it.evidence = append(it.evidence, ev.Summary)
			it.canDismiss = true
			continue
		}
		it := item(b)
		it.canDismiss = true
		if ev.Kind == EventLikelyStale {
			it.kinds = append(it.kinds, "likely stale")
			it.evidence = append(it.evidence, fmt.Sprintf("flagged %s: %s", ev.At.Local().Format("Jan 2 15:04"), strings.TrimPrefix(ev.Summary, "likely stale: ")))
		} else {
			it.kinds = append(it.kinds, "auto-closed")
			it.evidence = append(it.evidence, fmt.Sprintf("closed by verify %s: %s", ev.At.Local().Format("Jan 2 15:04"), ev.Summary))
		}
	}

	for _, c := range in.candidates {
		b, ok := beadForBranch(prefix, c.Branch, in.closed)
		if !ok {
			continue // merged or prunable but not a closed bead's: quartermaster sweep's job
		}
		it := item(b)
		it.kinds = append(it.kinds, "leftover worktree")
		ev := fmt.Sprintf("closed %s ago", strings.TrimSuffix(ago(b.ClosedAt, now), " ago"))
		if b.CloseReason != "" {
			ev += ": " + b.CloseReason
		}
		it.evidence = append(it.evidence, ev, fmt.Sprintf("worktree %s (%s)", tildePath(c.Path), c.Branch))
		it.keep = keepReason(c)
		if it.keep != "" {
			it.evidence = append(it.evidence, "keep: "+it.keep)
		} else {
			it.canResolve = true
		}
	}
	for _, b := range in.closed {
		a, ok := agentFor[agentName(b.ID)]
		if !ok {
			continue
		}
		it := item(b)
		it.agent = a.Name
		if !containsKind(it.kinds, "leftover worktree") {
			it.kinds = append(it.kinds, "leftover agent")
		}
		it.evidence = append(it.evidence, fmt.Sprintf("agent %s (%s %s) still open on a closed bead", a.Name, a.Kind, a.Status))
		if a.Status == "working" {
			it.canResolve = false
			if it.keep == "" {
				it.keep = "agent working"
			}
		}
	}

	out := make([]gcItem, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

func containsKind(kinds []string, k string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// gcSearch is what '/' matches in the tidy view.
func (it gcItem) search() string {
	return searchText(it.bead.ID, it.bead.Title, strings.Join(it.kinds, " "), strings.Join(it.evidence, " "))
}

// loadGC gathers the tidy view's inputs: closed beads, worktrees and their git
// state. It is slow (bd, git per worktree) and runs off the UI.
func loadGC(cfg Config, h Herdr, data boardData) ([]gcItem, error) {
	closed, err := Beads{}.list("--status", StatusClosed, "--brief")
	if err != nil {
		return nil, err
	}
	var active []Bead
	live := map[string]bool{}
	for _, a := range data.Agents {
		live[a.Name] = true
	}
	for _, b := range data.Beads {
		if b.Status != StatusOpen {
			active = append(active, b)
		}
	}
	list := cachedWorktrees(h.Worktrees)
	_, bare, err := orphans(cfg, active, live, data.State.PRs, list)
	if err != nil {
		return nil, err
	}
	env := liveSweepEnv(h)
	var cands []sweepCandidate
	for _, repo := range cfg.allRepos() {
		wts, err := list(repo)
		if err != nil {
			return nil, err
		}
		// Only closed beads' worktrees: a merged or prunable one with no
		// closed bead is quartermaster sweep's.
		for _, c := range classifySweep(repo, cfg.BranchPrefix, wts, nil, closed) {
			if !c.Prunable || len(c.Reasons) > 1 {
				cands = append(cands, c)
			}
		}
	}
	// A git status and rev-list each; dozens of worktrees in a big repo take
	// seconds one at a time.
	eachLimit(len(cands), ghParallel, func(i int) { inspectSweep(&cands[i], env, data.Agents) })
	return gcItems(cfg.BranchPrefix, gcInputs{active: active, closed: closed, agents: data.Agents, prs: data.State.PRs,
		inbox: data.Inbox, bare: bare, candidates: cands}, time.Now()), nil
}
