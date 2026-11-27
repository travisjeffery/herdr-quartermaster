package main

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Automatic blocker-cleared wakeup for waiting beads and workers.
//
// Across ticks, the ticker tracks two blocker sources for every non-closed bead:
//   - "blocks" bead dependencies (`bd dep`) that were last seen non-closed;
//   - upstream/waiting PR URLs explicitly referenced in a bead's notes or
//     attached `gh-pr` watches that were last seen non-MERGED.
//
// When a tracked blocking bead transitions to closed or a referenced PR
// transitions to MERGED, the ticker emits a deduplicated `unblocked` event.
// If the unblocked bead has a live idle worker (at rest, not `needs_me`, not
// held/headless/parked/relaunching) and its autonomy level reaches Class 3,
// the ticker wakes the worker once with the exact cleared blocker evidence and
// records an `AUTO: [ticker ...]` note; otherwise it surfaces `unblocked` to
// the coordinator inbox. Deduplication state rides in TickerState (state.json)
// so ticker restarts never repeat nudges.

const (
	blockerActionWoken          = "woken"
	blockerActionDispatched     = "dispatched"
	blockerActionDispatchFailed = "dispatch_failed"
	blockerActionNotified       = "notified"
	blockerActionWakeFailed     = "wake_failed"

	blockerWakeHeaderPrefix = "[" + toolName + ": automated, not the user, approves nothing]"
)

// BlockerState persists observed blockers and deduplicated wakeup/notification
// marks across ticks and ticker restarts inside TickerState (state.json).
type BlockerState struct {
	// Initialized distinguishes a fresh or pre-upgrade state (baseline only,
	// no transitions emitted) from a steady-state ticker.
	Initialized bool `json:"initialized,omitempty"`
	// LiveBeads are non-closed bead IDs seen on the previous pass, allowing
	// transitions to be detected even when a dependent bead was added right
	// before its blocker closed between ticks.
	LiveBeads []string `json:"live_beads,omitempty"`
	// OpenPRs are non-MERGED PR URLs seen on the previous pass across tracked
	// PR listings and references.
	OpenPRs []string `json:"open_prs,omitempty"`
	// Beads maps each non-closed bead ID to its tracked blockers and cleared
	// dedupe marks.
	Beads map[string]BeadBlockerState `json:"beads,omitempty"`
}

// BeadBlockerState holds one bead's currently tracked blockers and already
// handled cleared blockers.
type BeadBlockerState struct {
	LiveDeps []string               `json:"live_deps,omitempty"`
	OpenPRs  []string               `json:"open_prs,omitempty"`
	Cleared  map[string]BlockerMark `json:"cleared,omitempty"`
}

// BlockerMark records the deduplicated action taken for one cleared blocker.
type BlockerMark struct {
	Action string    `json:"action"` // "woken", "notified", "wake_failed"
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

type clearedBlockerKind string

const (
	clearedBlockerDep clearedBlockerKind = "dep"
	clearedBlockerPR  clearedBlockerKind = "pr"
)

type clearedBlocker struct {
	Kind        clearedBlockerKind
	Key         string // "dep:<id>" or "pr:<url>"
	BeadID      string
	BeadTitle   string
	UpstreamPRs []string
	PR          PR
}

func depBlockerKey(id string) string { return "dep:" + id }
func prBlockerKey(u string) string   { return "pr:" + u }

func ghPRURL(repo string, num int) string {
	if repo == "" || num <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, num)
}

var (
	// ownPRLineRE matches a line declaring a bead's own delivery PR ("PR: <url>").
	ownPRLineRE = regexp.MustCompile(`(?i)^(?:[-*]\s+)?PR:\s*(https://github\.com/[\w.-]+/[\w.-]+/pull/\d+)`)
	// blockerBeforePRRE matches blocker/upstream phrasing before a PR URL on a note line.
	blockerBeforePRRE = regexp.MustCompile(`(?i)(?:\b(?:block(?:ed|er|ing|s)?\s+(?:on|by)|wait(?:ing|s)?\s+(?:on|for)|upstream(?:\s+pr)?|depends?\s+on|dependency|stacked\s+on|base\s+pr|parent\s+pr|waiting\s+pr|referenced\s+pr|prereq(?:uisite)?(?:\s+pr)?|unblocked\s+(?:by|when|after)|blocker)\b|\b(?:after|until|once|requires?)(?:\s+pr)?(?:\s*[:(\[])?$)`)
	// blockerAfterPRRE matches merge-wait phrasing right after a PR URL on a note line.
	blockerAfterPRRE = regexp.MustCompile(`(?i)^(?:\s*[,;)\]]\s*)?(?:(?:to|must|should|needs?\s+to)\s+)?(?:merge[sd]?|land[sd]?|is\s+merged)\b`)
)

func isBlockerPRLine(line string, urlStart, urlEnd int) bool {
	if ownPRLineRE.MatchString(line) {
		return false
	}
	before := strings.TrimSpace(line[:urlStart])
	after := strings.TrimSpace(line[urlEnd:])
	return blockerBeforePRRE.MatchString(before) || blockerAfterPRRE.MatchString(after)
}

func isBlockerWakeAutoLine(line string) bool {
	return strings.HasPrefix(line, "AUTO:") && strings.Contains(line, "after blocker cleared:")
}

// ownDeclaredPRURLs returns PR URLs explicitly declared as the bead's own PR
// via a leading "PR: <url>" line.
func ownDeclaredPRURLs(notes string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(notes, "\n") {
		line := strings.TrimSpace(raw)
		if m := ownPRLineRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// deliveryNotedPRs returns noted PR URLs that may represent the bead's own
// delivery PR, excluding URLs that appear only on explicit blocker/upstream
// lines or blocker-wakeup AUTO notes.
func deliveryNotedPRs(notes string) []string {
	nonBlocker := map[string]bool{}
	var ordered []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(notes, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || isBlockerWakeAutoLine(line) {
			continue
		}
		locs := prURL.FindAllStringIndex(line, -1)
		for _, loc := range locs {
			u := line[loc[0]:loc[1]]
			if !seen[u] {
				seen[u] = true
				ordered = append(ordered, u)
			}
			if !isBlockerPRLine(line, loc[0], loc[1]) {
				nonBlocker[u] = true
			}
		}
	}
	var out []string
	for _, u := range ordered {
		if nonBlocker[u] {
			out = append(out, u)
		}
	}
	return out
}

// referencedPRURLs collects upstream/waiting PR URLs explicitly referenced in
// b's notes or attached gh-pr watches, excluding b's own delivery PRs.
func referencedPRURLs(b Bead, watches []Watch, branchPrefix string, otherOwned map[string]bool, prevOpen []string, prs map[string]PR) []string {
	own := ownDeclaredPRURLs(b.Notes)
	isOwn := func(u string) bool {
		if own[u] {
			return true
		}
		if pr, ok := prs[u]; ok && pr.Head != "" && onBeadBranch(branchPrefix, pr.Head, b) {
			return true
		}
		return false
	}

	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u == "" || seen[u] || isOwn(u) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}

	// 1. Attached gh-pr watches (active or already triggered).
	for _, w := range watches {
		if w.Bead != b.ID || w.Kind != WatchKindGHPR || w.Params.GHPR == nil {
			continue
		}
		if w.Status != WatchStatusActive && w.Status != WatchStatusTriggered {
			continue
		}
		add(ghPRURL(w.Params.GHPR.Repo, w.Params.GHPR.PR))
	}

	// 2. Non-bookkeeping note lines referencing upstream/waiting/other-bead PRs.
	prevOpenSet := make(map[string]bool, len(prevOpen))
	for _, u := range prevOpen {
		prevOpenSet[u] = true
	}
	for _, raw := range strings.Split(b.Notes, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || isBookkeepingNote(line) || ownPRLineRE.MatchString(line) {
			continue
		}
		for _, loc := range prURL.FindAllStringIndex(line, -1) {
			u := line[loc[0]:loc[1]]
			if isBlockerPRLine(line, loc[0], loc[1]) || otherOwned[u] || prevOpenSet[u] || b.Status == StatusBlocked {
				add(u)
			}
		}
	}

	return out
}

// otherOwnedPRs builds, for each PR URL, the set of bead IDs that own it via a
// leading "PR:" note or a matching bead branch.
func prOwnersByURL(all []Bead, branchPrefix string, prs map[string]PR) map[string]map[string]bool {
	owners := map[string]map[string]bool{}
	mark := func(u, id string) {
		if u == "" || id == "" {
			return
		}
		if owners[u] == nil {
			owners[u] = map[string]bool{}
		}
		owners[u][id] = true
	}
	for _, b := range all {
		for u := range ownDeclaredPRURLs(b.Notes) {
			mark(u, b.ID)
		}
	}
	for u, pr := range prs {
		if pr.Head == "" {
			continue
		}
		if ob, ok := beadForBranch(branchPrefix, pr.Head, all); ok {
			mark(u, ob.ID)
		}
	}
	return owners
}

func ownedByAnotherBead(owners map[string]map[string]bool, selfID string) map[string]bool {
	out := map[string]bool{}
	for u, ids := range owners {
		for id := range ids {
			if id != selfID {
				out[u] = true
				break
			}
		}
	}
	return out
}

// mergePRMap merges src into dst, preferring MERGED over other states and
// preserving richer commit SHA metadata when both entries share a state.
func mergePRMap(dst, src map[string]PR) {
	for u, p := range src {
		if u == "" {
			u = p.URL
		}
		if u == "" {
			continue
		}
		if p.URL == "" {
			p.URL = u
		}
		cur, ok := dst[u]
		if !ok {
			dst[u] = p
			continue
		}
		if cur.State != "MERGED" && p.State == "MERGED" {
			dst[u] = p
			continue
		}
		if cur.State == "MERGED" && p.State != "MERGED" {
			continue
		}
		// Same status bucket: fill in missing metadata.
		if p.mergedSHA() != "" && cur.mergedSHA() == "" {
			dst[u] = p
			continue
		}
		if p.MergeCommit != nil && p.MergeCommit.OID != "" && (cur.MergeCommit == nil || cur.MergeCommit.OID == "") {
			dst[u] = p
			continue
		}
		if p.Number != 0 && cur.Number == 0 {
			dst[u] = p
		}
	}
}

func formatClearedBlocker(c clearedBlocker) string {
	switch c.Kind {
	case clearedBlockerDep:
		if c.BeadTitle != "" {
			return fmt.Sprintf("blocking bead %s (%s) closed", c.BeadID, shortTitle(c.BeadTitle, 60))
		}
		return fmt.Sprintf("blocking bead %s closed", c.BeadID)
	case clearedBlockerPR:
		sha := c.PR.mergedSHA()
		var meta []string
		if c.PR.URL != "" {
			meta = append(meta, c.PR.URL)
		}
		if sha != "" {
			meta = append(meta, "sha "+sha)
		}
		if c.PR.Number > 0 {
			if len(meta) > 0 {
				return fmt.Sprintf("referenced PR #%d (%s) merged", c.PR.Number, strings.Join(meta, ", "))
			}
			return fmt.Sprintf("referenced PR #%d merged", c.PR.Number)
		}
		if len(meta) > 0 {
			return fmt.Sprintf("referenced PR (%s) merged", strings.Join(meta, ", "))
		}
		return "referenced PR merged"
	default:
		return c.Key
	}
}

func formatClearedBlockers(cs []clearedBlocker) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = formatClearedBlocker(c)
	}
	return strings.Join(parts, "; ")
}

func blockerWakePrompt(b Bead, cleared []clearedBlocker, remainingDeps []string) string {
	desc := formatClearedBlockers(cleared)
	msg := fmt.Sprintf("%s Blocker cleared for %s: %s.", blockerWakeHeaderPrefix, b.ID, desc)
	if len(remainingDeps) > 0 {
		msg += fmt.Sprintf(" Remaining blocking dependencies: %s.", strings.Join(remainingDeps, ", "))
	}
	if b.Status == StatusBlocked {
		msg += fmt.Sprintf(" Inspect the bead (`bd show %s`), update its status if ready, and continue your work.", b.ID)
	} else {
		msg += fmt.Sprintf(" Inspect the bead (`bd show %s`) and continue your work.", b.ID)
	}
	return msg
}

func blockerInboxSummary(b Bead, cleared []clearedBlocker, remainingDeps []string, action, skipReason string) string {
	desc := formatClearedBlockers(cleared)
	s := fmt.Sprintf("%s unblocked (%s)", b.ID, desc)
	if len(remainingDeps) > 0 {
		s += fmt.Sprintf("; remaining blockers: %s", strings.Join(remainingDeps, ", "))
	}
	switch action {
	case blockerActionWoken:
		if skipReason != "" {
			s += "; " + skipReason
		} else {
			s += "; woke idle worker"
		}
	case blockerActionDispatched:
		if skipReason != "" {
			s += "; auto-dispatched chained rollout worker (" + skipReason + ")"
		} else {
			s += "; auto-dispatched chained rollout worker"
		}
	case blockerActionDispatchFailed:
		s += fmt.Sprintf("; chained rollout auto-dispatch failed (%s); run `quartermaster dispatch %s --role rollout`", skipReason, b.ID)
	case blockerActionWakeFailed:
		s += fmt.Sprintf("; worker wake failed (%s)", skipReason)
	default:
		if skipReason != "" {
			s += fmt.Sprintf("; worker not woken (%s)", skipReason)
		}
		switch {
		case b.Status == StatusOpen && len(remainingDeps) == 0:
			if target, err := rolloutTarget(b); err == nil && target != "" && b.HasLabel(roleLabel+roleRollout) {
				s += fmt.Sprintf("; run `quartermaster dispatch %s --role rollout`", b.ID)
			} else {
				s += fmt.Sprintf("; run `quartermaster dispatch %s`", b.ID)
			}
		case (b.Status == StatusInProgress || b.Status == StatusBlocked) && skipReason == "no worker running":
			s += fmt.Sprintf("; run `quartermaster resume %s`", b.ID)
		}
	}
	return s
}

func blockerDedupeKey(cs []clearedBlocker) string {
	keys := make([]string, len(cs))
	for i, c := range cs {
		keys[i] = c.Key
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// blockerWakeEnv injects external dependencies for blocker-cleared wakeup so
// unit tests run deterministically without live bd, gh, or herdr calls.
type blockerWakeEnv struct {
	show            func(id string) (Bead, error)
	note            func(id, text string) error
	lookupPRs       func(urls []string) (map[string]PR, error)
	watches         func() ([]Watch, error)
	agentFor        func(b Bead) (*Agent, error)
	wake            func(b Bead, a Agent, text string) error
	enforce         func(b Bead, action, by string) (string, error)
	enforceRollout  func(b Bead, target, by string) (string, int, error)
	dispatchRollout func(b Bead, target string) (string, error)
	event           func(Event) error
	journal         func(journalRecord) error
	liveAsk         func(a *Agent) bool
	held            func(id string) bool
	headless        func(id string) bool
	parked          func(id string) bool
	logf            func(format string, args ...any)
}

func (t Ticker) liveBlockerWakeEnv() blockerWakeEnv {
	return blockerWakeEnv{
		show:      t.beads.Show,
		note:      t.beads.Note,
		lookupPRs: t.gh.Lookup,
		watches:   loadWatches,
		agentFor: func(b Bead) (*Agent, error) {
			host := beadHost(b)
			if host == "" {
				host = localHost
			}
			return agentOn(t.cfg, t.herdr)(host, agentName(b.ID))
		},
		wake: func(b Bead, a Agent, text string) error {
			host := beadHost(b)
			if host == "" || host == localHost {
				_, err := promptPiAgent(t.herdr, a, text)
				return err
			}
			_, err := tellCmd(t.cfg, t.herdr, func(id string) (Bead, error) {
				if id == b.ID {
					return b, nil
				}
				return t.beads.Show(id)
			}, b.ID, text)
			return err
		},
		enforce: func(b Bead, action, by string) (string, error) {
			return t.cfg.enforce(b, action, by)
		},
		enforceRollout: func(b Bead, target, by string) (string, int, error) {
			return t.cfg.enforceRolloutDispatch(b, target, by)
		},
		dispatchRollout: func(b Bead, _ string) (string, error) {
			// Autonomy dial and budget were already enforced at the target's
			// rollout class immediately prior via enforceRollout.
			return dispatch(t.cfg, t.herdr, b.ID, DispatchOpts{Role: roleRollout, By: "TJ"})
		},
		event:   writeEvent,
		journal: appendJournal,
		liveAsk: func(a *Agent) bool {
			ask, ok, err := piAskOf(t.herdr.hostName(), a)
			return ok && err == nil && ask != nil
		},
		held: func(id string) bool {
			_, ok := isJobHeld(id)
			return ok
		},
		headless: headlessOwned,
		parked:   rpcParked,
		logf:     t.log.Printf,
	}
}

func (t Ticker) checkBlockers(
	st *TickerState,
	all []Bead,
	threads []Thread,
	agents map[string]Agent,
	agentsComplete bool,
	freshPRs, knownPRs map[string]PR,
	ghRefreshed bool,
	promptedThisTick, relaunch map[string]bool,
	watchOutcomes []watchCheckOutcome,
	now time.Time,
) {
	var env blockerWakeEnv
	if t.blockerEnv != nil {
		env = *t.blockerEnv
	} else {
		env = t.liveBlockerWakeEnv()
	}
	if env.logf == nil {
		env.logf = t.log.Printf
	}
	watchWokenPRs := map[string]map[string]bool{}
	watchMergedPRs := map[string]bool{}
	for _, o := range watchOutcomes {
		if o.Action != "triggered" || o.Watch.Kind != WatchKindGHPR || o.Watch.Params.GHPR == nil {
			continue
		}
		u := ghPRURL(o.Watch.Params.GHPR.Repo, o.Watch.Params.GHPR.PR)
		if u == "" {
			continue
		}
		if strings.EqualFold(o.Watch.Params.GHPR.Expect, "merged") {
			watchMergedPRs[u] = true
		}
		if o.WakeResult == "woken" {
			if watchWokenPRs[o.Watch.Bead] == nil {
				watchWokenPRs[o.Watch.Bead] = map[string]bool{}
			}
			watchWokenPRs[o.Watch.Bead][u] = true
		}
	}
	for _, line := range blockerWakeupPass(
		t.cfg, env, st, all, threads, agents, agentsComplete,
		freshPRs, knownPRs, ghRefreshed, promptedThisTick, relaunch,
		watchWokenPRs, watchMergedPRs, now,
	) {
		env.logf("blocker wakeup: %s", line)
	}
}

// blockerWakeupPass tracks dependency and referenced-PR blocker transitions
// across ticks, wakes eligible idle non-needs_me workers at autonomy L3+, and
// surfaces unblocked events to the coordinator inbox.
func blockerWakeupPass(
	cfg Config,
	env blockerWakeEnv,
	st *TickerState,
	all []Bead,
	threads []Thread,
	agents map[string]Agent,
	agentsComplete bool,
	freshPRs, knownPRs map[string]PR,
	ghRefreshed bool,
	promptedThisTick, relaunch map[string]bool,
	watchWokenPRs map[string]map[string]bool,
	watchMergedPRs map[string]bool,
	now time.Time,
) []string {
	logf := env.logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if st.Blockers.Beads == nil {
		st.Blockers.Beads = map[string]BeadBlockerState{}
	}

	nonClosed := make(map[string]Bead, len(all))
	liveIDs := make([]string, 0, len(all))
	for _, b := range all {
		if b.Status == StatusClosed {
			continue
		}
		nonClosed[b.ID] = b
		liveIDs = append(liveIDs, b.ID)
	}
	slices.Sort(liveIDs)

	threadByID := make(map[string]Thread, len(threads))
	for _, th := range threads {
		threadByID[th.Bead.ID] = th
	}

	// Build merged view of all PRs already known on this pass.
	prs := map[string]PR{}
	mergePRMap(prs, st.Noted)
	for _, list := range st.MergedPRs {
		for _, p := range list {
			if p.URL != "" {
				mergePRMap(prs, map[string]PR{p.URL: p})
			}
		}
	}
	for _, p := range st.PRs {
		if p.URL != "" {
			mergePRMap(prs, map[string]PR{p.URL: p})
		}
	}
	mergePRMap(prs, knownPRs)
	mergePRMap(prs, freshPRs)

	// Load declarative watches if available.
	var allWatches []Watch
	if env.watches != nil {
		ws, err := env.watches()
		if err != nil {
			logf("blocker wakeup: load watches: %v", err)
		} else {
			allWatches = ws
		}
	}
	watchesByBead := map[string][]Watch{}
	for _, w := range allWatches {
		if w.Kind == WatchKindGHPR && (w.Status == WatchStatusActive || w.Status == WatchStatusTriggered) {
			watchesByBead[w.Bead] = append(watchesByBead[w.Bead], w)
		}
	}

	owners := prOwnersByURL(all, cfg.BranchPrefix, prs)

	// Collect referenced PR URLs per non-closed bead and batch-lookup any URLs
	// that are missing or need refresh on a GH tick (or just triggered a watch).
	refsByBead := make(map[string][]string, len(nonClosed))
	var wantLookup []string
	wantSeen := map[string]bool{}
	for _, id := range liveIDs {
		b := nonClosed[id]
		prev := st.Blockers.Beads[id]
		otherOwned := ownedByAnotherBead(owners, id)
		refs := referencedPRURLs(b, watchesByBead[id], cfg.BranchPrefix, otherOwned, prev.OpenPRs, prs)
		refsByBead[id] = refs
		for _, u := range refs {
			if wantSeen[u] {
				continue
			}
			cur, hasCur := prs[u]
			if hasCur && cur.State == "MERGED" {
				continue
			}
			if _, isFresh := freshPRs[u]; isFresh {
				continue
			}
			if ghRefreshed || watchMergedPRs[u] {
				wantSeen[u] = true
				wantLookup = append(wantLookup, u)
			}
		}
	}
	if len(wantLookup) > 0 && env.lookupPRs != nil {
		slices.Sort(wantLookup)
		got, err := env.lookupPRs(wantLookup)
		if err != nil {
			logf("blocker wakeup: lookup PRs: %v", err)
		}
		if len(got) > 0 {
			mergePRMap(prs, got)
			for u, p := range got {
				if p.State == "MERGED" {
					if st.Noted == nil {
						st.Noted = map[string]PR{}
					}
					st.Noted[u] = p
				}
			}
			// Recompute owners and refs if newly discovered PR head branches
			// clarify bead ownership.
			owners = prOwnersByURL(all, cfg.BranchPrefix, prs)
			for _, id := range liveIDs {
				b := nonClosed[id]
				prev := st.Blockers.Beads[id]
				otherOwned := ownedByAnotherBead(owners, id)
				refsByBead[id] = referencedPRURLs(b, watchesByBead[id], cfg.BranchPrefix, otherOwned, prev.OpenPRs, prs)
			}
		}
	}

	// Build global sets from previous tick for between-tick race detection.
	prevGlobalLive := make(map[string]bool, len(st.Blockers.LiveBeads))
	for _, id := range st.Blockers.LiveBeads {
		prevGlobalLive[id] = true
	}
	prevGlobalOpenPRs := make(map[string]bool, len(st.Blockers.OpenPRs))
	for _, u := range st.Blockers.OpenPRs {
		prevGlobalOpenPRs[u] = true
	}

	// First pass / pre-upgrade state: baseline only, never emit events.
	if !st.Blockers.Initialized {
		nextBeads := make(map[string]BeadBlockerState, len(nonClosed))
		globalOpenSet := map[string]bool{}
		for u, pr := range prs {
			if pr.State != "" && pr.State != "MERGED" {
				globalOpenSet[u] = true
			}
		}
		for _, id := range liveIDs {
			b := nonClosed[id]
			var liveDeps []string
			for _, d := range b.Dependencies {
				if d.Type == "blocks" && d.DependsOn != "" {
					if _, ok := nonClosed[d.DependsOn]; ok && !slices.Contains(liveDeps, d.DependsOn) {
						liveDeps = append(liveDeps, d.DependsOn)
					}
				}
			}
			slices.Sort(liveDeps)
			var openPRs []string
			for _, u := range refsByBead[id] {
				if pr, ok := prs[u]; ok && pr.State != "" && pr.State != "MERGED" {
					openPRs = append(openPRs, u)
					globalOpenSet[u] = true
				}
			}
			slices.Sort(openPRs)
			if len(liveDeps) > 0 || len(openPRs) > 0 {
				nextBeads[id] = BeadBlockerState{LiveDeps: liveDeps, OpenPRs: openPRs}
			}
		}
		st.Blockers = BlockerState{
			Initialized: true,
			LiveBeads:   liveIDs,
			OpenPRs:     sortedKeys(globalOpenSet),
			Beads:       nextBeads,
		}
		return nil
	}

	// Cache bd show calls for disappearing dependency beads across this pass.
	depShowCache := map[string]Bead{}
	depShowErr := map[string]error{}
	showDep := func(id string) (Bead, error) {
		if b, ok := depShowCache[id]; ok {
			return b, nil
		}
		if err, ok := depShowErr[id]; ok {
			return Bead{}, err
		}
		if env.show == nil {
			err := fmt.Errorf("show unavailable for %s", id)
			depShowErr[id] = err
			return Bead{}, err
		}
		b, err := env.show(id)
		if err != nil {
			depShowErr[id] = err
			return Bead{}, err
		}
		depShowCache[id] = b
		return b, nil
	}

	globalOpenSet := map[string]bool{}
	for u, pr := range prs {
		if pr.State != "" && pr.State != "MERGED" {
			globalOpenSet[u] = true
		}
	}

	nextBeads := make(map[string]BeadBlockerState, len(nonClosed))
	var lines []string

	for _, id := range liveIDs {
		b := nonClosed[id]
		prev := st.Blockers.Beads[id]
		clearedMarks := map[string]BlockerMark{}
		for k, m := range prev.Cleared {
			clearedMarks[k] = m
		}
		prevLiveDeps := make(map[string]bool, len(prev.LiveDeps))
		for _, dep := range prev.LiveDeps {
			prevLiveDeps[dep] = true
		}
		prevOpenPRs := make(map[string]bool, len(prev.OpenPRs))
		for _, u := range prev.OpenPRs {
			prevOpenPRs[u] = true
		}

		var nextLiveDeps []string
		var remainingDeps []string
		var newlyCleared []clearedBlocker
		seenDeps := map[string]bool{}
		for _, d := range b.Dependencies {
			if d.Type != "blocks" || d.DependsOn == "" || seenDeps[d.DependsOn] {
				continue
			}
			seenDeps[d.DependsOn] = true
			depID := d.DependsOn
			if _, isLive := nonClosed[depID]; isLive {
				nextLiveDeps = append(nextLiveDeps, depID)
				remainingDeps = append(remainingDeps, depID)
				// If a previously closed dependency was reopened, allow a future
				// close transition to notify again.
				delete(clearedMarks, depBlockerKey(depID))
				continue
			}
			// Dependency is not in nonClosed. Check if it transitioned from live.
			key := depBlockerKey(depID)
			if _, alreadyCleared := clearedMarks[key]; alreadyCleared {
				continue
			}
			if !prevLiveDeps[depID] && !prevGlobalLive[depID] {
				continue
			}
			depBead, err := showDep(depID)
			if err != nil {
				// Transient bd error: keep tracking so the next tick retries.
				logf("blocker wakeup: show dependency %s for %s: %v", depID, id, err)
				nextLiveDeps = append(nextLiveDeps, depID)
				continue
			}
			if depBead.Status == StatusClosed {
				newlyCleared = append(newlyCleared, clearedBlocker{
					Kind:        clearedBlockerDep,
					Key:         key,
					BeadID:      depID,
					BeadTitle:   depBead.Title,
					UpstreamPRs: deliveryNotedPRs(depBead.Notes),
				})
			} else if depBead.Status != "" && depBead.Status != "deferred" {
				// Still active/open despite list skew: retain for retry.
				nextLiveDeps = append(nextLiveDeps, depID)
			}
		}
		slices.Sort(nextLiveDeps)
		slices.Sort(remainingDeps)

		var nextOpenPRs []string
		for _, u := range refsByBead[id] {
			key := prBlockerKey(u)
			if _, alreadyCleared := clearedMarks[key]; alreadyCleared {
				continue
			}
			pr, hasPR := prs[u]
			if !hasPR {
				// Preserve previously tracked open PR across transient GH failures.
				if prevOpenPRs[u] {
					nextOpenPRs = append(nextOpenPRs, u)
					globalOpenSet[u] = true
				}
				continue
			}
			if pr.State == "MERGED" {
				if prevOpenPRs[u] || prevGlobalOpenPRs[u] {
					if pr.URL == "" {
						pr.URL = u
					}
					newlyCleared = append(newlyCleared, clearedBlocker{
						Kind: clearedBlockerPR,
						Key:  key,
						PR:   pr,
					})
				}
				continue
			}
			if pr.State != "" {
				nextOpenPRs = append(nextOpenPRs, u)
				globalOpenSet[u] = true
			}
		}
		slices.Sort(nextOpenPRs)

		if len(newlyCleared) > 0 {
			sort.SliceStable(newlyCleared, func(i, j int) bool {
				return newlyCleared[i].Key < newlyCleared[j].Key
			})
			action, detail, line := handleClearedBlockers(
				cfg, env, st, b, threadByID[id], agents, agentsComplete,
				promptedThisTick, relaunch, watchWokenPRs[id],
				newlyCleared, remainingDeps, len(nextOpenPRs) > 0, now,
			)
			if line != "" {
				lines = append(lines, line)
			}
			if action != "" {
				for _, c := range newlyCleared {
					clearedMarks[c.Key] = BlockerMark{Action: action, At: now, Detail: detail}
				}
			} else {
				// Event write failed before any wake occurred; keep blockers
				// pending so the next tick retries cleanly.
				for _, c := range newlyCleared {
					switch c.Kind {
					case clearedBlockerDep:
						if !slices.Contains(nextLiveDeps, c.BeadID) {
							nextLiveDeps = append(nextLiveDeps, c.BeadID)
						}
					case clearedBlockerPR:
						if !slices.Contains(nextOpenPRs, c.PR.URL) {
							nextOpenPRs = append(nextOpenPRs, c.PR.URL)
							globalOpenSet[c.PR.URL] = true
						}
					}
				}
				slices.Sort(nextLiveDeps)
				slices.Sort(nextOpenPRs)
			}
		}

		if len(nextLiveDeps) > 0 || len(nextOpenPRs) > 0 || len(clearedMarks) > 0 {
			nextBeads[id] = BeadBlockerState{
				LiveDeps: nextLiveDeps,
				OpenPRs:  nextOpenPRs,
				Cleared:  clearedMarks,
			}
		}
	}

	st.Blockers = BlockerState{
		Initialized: true,
		LiveBeads:   liveIDs,
		OpenPRs:     sortedKeys(globalOpenSet),
		Beads:       nextBeads,
	}
	return lines
}

// handleClearedBlockers evaluates wakeup eligibility for bead b's newly cleared
// blockers, wakes an eligible idle non-needs_me worker at L3+, and records the
// deduplicated unblocked event, note, and journal entry. Returns action="" only
// when no wake occurred and the inbox event write failed (allowing retry).
func handleClearedBlockers(
	cfg Config,
	env blockerWakeEnv,
	st *TickerState,
	b Bead,
	th Thread,
	agents map[string]Agent,
	agentsComplete bool,
	promptedThisTick, relaunch map[string]bool,
	watchWoken map[string]bool,
	cleared []clearedBlocker,
	remainingDeps []string,
	hasRemainingOpenPRs bool,
	now time.Time,
) (action, detail, line string) {
	id := b.ID
	desc := formatClearedBlockers(cleared)
	dedupeKey := blockerDedupeKey(cleared)

	// If every cleared blocker in this batch was a gh-pr watch that already
	// woke this worker on the exact same tick, emit unblocked without sending
	// a second prompt.
	if len(watchWoken) > 0 && allClearedWokenByWatch(cleared, watchWoken) {
		summary := blockerInboxSummary(b, cleared, remainingDeps, blockerActionWoken, "worker already woken by gh-pr watch")
		if env.event != nil {
			if err := env.event(Event{
				Bead:    id,
				Kind:    EventUnblocked,
				Summary: summary,
				At:      now,
				Clears:  &InboxCondition{Kind: "bead_closed", Bead: id, Key: dedupeKey},
			}); err != nil {
				return "", "", fmt.Sprintf("%s: write unblocked event: %v", id, err)
			}
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalUnblocked, Bead: id, Class: classHousekeeping, Action: blockerActionWoken, Pass: true, Text: summary})
		}
		return blockerActionWoken, "woken by gh-pr watch", summary
	}

	// First-class chained rollout auto-dispatch: when an open role:rollout bead
	// with rollout-target:<target> clears its final blocker, auto-dispatch it
	// if within the target's autonomy dial class (e.g. C5 configured dev at L5).
	if b.Status == StatusOpen && len(remainingDeps) == 0 && !hasRemainingOpenPRs {
		if target, ok, err := isChainedRolloutBead(cfg, b); err == nil && ok {
			return handleUnblockedChainedRollout(
				cfg, env, b, target, agents, agentsComplete, relaunch,
				cleared, desc, dedupeKey, now,
			)
		}
	}

	agent, skipReason := checkBlockerWakeEligibility(env, st, b, th, agents, agentsComplete, promptedThisTick, relaunch)
	if skipReason == "" && env.show != nil {
		// Re-read live bead state right before enforcing and waking so a
		// concurrent flip to needs_me or closed is never woken.
		cur, err := env.show(id)
		if err != nil {
			skipReason = "re-read bead failed: " + err.Error()
		} else {
			switch cur.Status {
			case StatusNeedsMe:
				skipReason = "bead is needs_me"
			case StatusClosed:
				skipReason = "bead is closed"
			case StatusOpen:
				skipReason = "bead is open"
			default:
				b = cur
			}
		}
	}

	var autoPrefix string
	if skipReason == "" && env.enforce != nil {
		prefix, err := env.enforce(b, "wake", "ticker")
		if err != nil {
			skipReason = err.Error()
		} else {
			autoPrefix = prefix
		}
	}

	if skipReason != "" {
		summary := blockerInboxSummary(b, cleared, remainingDeps, blockerActionNotified, skipReason)
		clearsKind := "bead_closed"
		if b.Status == StatusOpen && len(remainingDeps) == 0 {
			clearsKind = "unblocked_dispatch"
		}
		if env.event != nil {
			if err := env.event(Event{
				Bead:    id,
				Kind:    EventUnblocked,
				Summary: summary,
				At:      now,
				Clears:  &InboxCondition{Kind: clearsKind, Bead: id, Key: dedupeKey},
			}); err != nil {
				return "", "", fmt.Sprintf("%s: write unblocked event: %v", id, err)
			}
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalUnblocked, Bead: id, Class: classHousekeeping, Action: blockerActionNotified, Pass: false, Text: summary})
		}
		return blockerActionNotified, skipReason, summary
	}

	// Eligible idle non-needs_me worker at L3+: wake once with exact cleared blocker(s).
	prompt := blockerWakePrompt(b, cleared, remainingDeps)
	if err := env.wake(b, *agent, prompt); err != nil {
		summary := blockerInboxSummary(b, cleared, remainingDeps, blockerActionWakeFailed, err.Error())
		if env.event != nil {
			_ = env.event(Event{
				Bead:    id,
				Kind:    EventUnblocked,
				Summary: summary,
				At:      now,
				Clears:  &InboxCondition{Kind: "bead_closed", Bead: id, Key: dedupeKey},
			})
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalUnblocked, Bead: id, Class: classHousekeeping, Action: blockerActionWakeFailed, Pass: false, Text: summary})
		}
		return blockerActionWakeFailed, err.Error(), summary
	}

	if autoPrefix == "" {
		autoPrefix = fmt.Sprintf("AUTO: [ticker L%d C%d]", classHousekeeping, classHousekeeping)
	}
	autoNote := fmt.Sprintf("%s woke idle worker after blocker cleared: %s", autoPrefix, desc)
	noteOK := false
	if env.note != nil {
		if err := env.note(id, autoNote); err != nil {
			if env.logf != nil {
				env.logf("blocker wakeup: note %s: %v", id, err)
			}
		} else {
			noteOK = true
		}
	}
	if snap, ok := st.Threads[id]; ok {
		snap.ReadySince = time.Time{}
		if noteOK {
			snap.Auto++
		}
		st.Threads[id] = snap
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{At: now, Kind: journalAuto, Bead: id, Class: classHousekeeping, Action: "blocker_wake", Pass: true, Text: autoNote})
		_ = env.journal(journalRecord{At: now, Kind: journalUnblocked, Bead: id, Class: classHousekeeping, Action: blockerActionWoken, Pass: true, Text: desc})
	}
	summary := blockerInboxSummary(b, cleared, remainingDeps, blockerActionWoken, "")
	if env.event != nil {
		if err := env.event(Event{
			Bead:    id,
			Kind:    EventUnblocked,
			Summary: summary,
			At:      now,
			Clears:  &InboxCondition{Kind: "bead_closed", Bead: id, Key: dedupeKey},
		}); err != nil && env.logf != nil {
			env.logf("blocker wakeup: write unblocked event %s: %v", id, err)
		}
	}
	return blockerActionWoken, desc, summary
}

func allClearedWokenByWatch(cleared []clearedBlocker, watchWoken map[string]bool) bool {
	if len(cleared) == 0 {
		return false
	}
	for _, c := range cleared {
		if c.Kind != clearedBlockerPR || !watchWoken[c.PR.URL] {
			return false
		}
	}
	return true
}

func checkBlockerWakeEligibility(
	env blockerWakeEnv,
	st *TickerState,
	b Bead,
	th Thread,
	agents map[string]Agent,
	agentsComplete bool,
	promptedThisTick, relaunch map[string]bool,
) (*Agent, string) {
	id := b.ID
	switch b.Status {
	case StatusNeedsMe:
		return nil, "bead is needs_me"
	case StatusClosed:
		return nil, "bead is closed"
	case StatusOpen:
		return nil, "bead is open"
	}
	if env.held != nil && env.held(id) {
		return nil, "bead is on hold"
	}
	if env.headless != nil && env.headless(id) {
		return nil, "headless-owned bead"
	}
	if relaunch[agentName(id)] || st.Threads[id].Relaunch {
		return nil, "worker is scheduled for relaunch"
	}
	if promptedThisTick[id] {
		return nil, "worker already prompted this tick"
	}

	var a *Agent
	host := beadHost(b)
	if agents != nil && (host == "" || host == localHost) {
		if found, ok := agents[agentName(id)]; ok {
			cp := found
			a = &cp
		}
	}
	if a == nil && host != "" && host != localHost && env.agentFor != nil {
		found, err := env.agentFor(b)
		if err != nil {
			return nil, "worker lookup failed: " + err.Error()
		}
		a = found
	}
	if a == nil {
		if !agentsComplete && (host == "" || host == localHost) {
			return nil, "worker discovery incomplete"
		}
		return nil, "no worker running"
	}
	if a.Backend == rpcBackend && env.parked != nil && env.parked(id) {
		return nil, "worker is parked"
	}
	if a.Status == "blocked" || th.Prompt != nil || (th.Bead.ID == id && classify(th) == GroupNeedsYou) {
		return nil, "worker is at an approval prompt or needs_you"
	}
	if !agentReady(a) {
		return nil, "worker is " + a.Status
	}
	if env.liveAsk != nil && env.liveAsk(a) {
		return nil, "worker is at an approval prompt"
	}
	if env.wake == nil {
		return nil, "worker wake unavailable"
	}
	return a, ""
}

// handleUnblockedChainedRollout evaluates and performs autonomy-dial-gated
// auto-dispatch for an open role:rollout bead carrying rollout-target:<target>
// once all of its blockers have cleared.
func handleUnblockedChainedRollout(
	cfg Config,
	env blockerWakeEnv,
	b Bead,
	target string,
	agents map[string]Agent,
	agentsComplete bool,
	relaunch map[string]bool,
	cleared []clearedBlocker,
	desc, dedupeKey string,
	now time.Time,
) (action, detail, line string) {
	id := b.ID
	targetCls := max(actionClasses["dispatch"], cfg.Autonomy.targetClass(target))
	skipReason := ""

	switch {
	case env.held != nil && env.held(id):
		skipReason = "bead is on hold"
	case env.headless != nil && env.headless(id):
		skipReason = "headless-owned bead"
	case relaunch[agentName(id)]:
		skipReason = "worker is scheduled for relaunch"
	case !agentsComplete:
		skipReason = "worker discovery incomplete"
	case agents != nil && agents[agentName(id)].Name != "":
		skipReason = "worker already running"
	case env.dispatchRollout == nil:
		skipReason = "auto-dispatch unavailable"
	}

	if skipReason == "" && env.show != nil {
		cur, err := env.show(id)
		if err != nil {
			skipReason = "re-read bead failed: " + err.Error()
		} else if cur.Status != StatusOpen {
			skipReason = "bead is " + cur.Status
		} else if t2, ok, rerr := isChainedRolloutBead(cfg, cur); rerr != nil || !ok || t2 != target {
			skipReason = "rollout target or role changed"
		} else {
			b = cur
		}
	}

	var autoPrefix string
	cls := targetCls
	if skipReason == "" {
		if env.enforceRollout != nil {
			prefix, c, err := env.enforceRollout(b, target, "ticker")
			if err != nil {
				skipReason = err.Error()
			} else {
				autoPrefix = prefix
				if c > 0 {
					cls = c
				}
			}
		} else {
			lvl, why := cfg.beadLevel(b, autonomyStopped())
			prefix, err := allow(b.ID, "ticker", fmt.Sprintf("chained rollout dispatch (%s)", target), targetCls, lvl, why)
			if err != nil {
				skipReason = err.Error()
			} else {
				autoPrefix = prefix
			}
		}
	}

	if skipReason != "" {
		summary := blockerInboxSummary(b, cleared, nil, blockerActionNotified, skipReason)
		if env.event != nil {
			if err := env.event(Event{
				Bead:    id,
				Kind:    EventUnblocked,
				Summary: summary,
				At:      now,
				Clears:  &InboxCondition{Kind: "unblocked_dispatch", Bead: id, Key: dedupeKey},
			}); err != nil {
				return "", "", fmt.Sprintf("%s: write unblocked event: %v", id, err)
			}
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalUnblocked, Bead: id, Class: cls, Action: blockerActionNotified, Pass: false, Text: summary})
		}
		return blockerActionNotified, skipReason, summary
	}

	if env.note != nil {
		upstreamNote := fmt.Sprintf("Upstream blocker cleared: %s (target=%s)", desc, target)
		var prs []string
		for _, c := range cleared {
			for _, u := range c.UpstreamPRs {
				if !slices.Contains(prs, u) {
					prs = append(prs, u)
				}
			}
		}
		if len(prs) > 0 {
			upstreamNote += "; upstream PR(s): " + strings.Join(prs, ", ")
		}
		_ = env.note(id, upstreamNote)
	}
	dispatchMsg, err := env.dispatchRollout(b, target)
	if err != nil {
		summary := blockerInboxSummary(b, cleared, nil, blockerActionDispatchFailed, err.Error())
		if env.event != nil {
			_ = env.event(Event{
				Bead:    id,
				Kind:    EventUnblocked,
				Summary: summary,
				At:      now,
				Clears:  &InboxCondition{Kind: "unblocked_dispatch", Bead: id, Key: dedupeKey},
			})
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalUnblocked, Bead: id, Class: cls, Action: blockerActionDispatchFailed, Pass: false, Text: summary})
		}
		return blockerActionDispatchFailed, err.Error(), summary
	}

	if autoPrefix == "" {
		autoPrefix = fmt.Sprintf("AUTO: [ticker L%d C%d]", cls, cls)
	}
	autoNote := fmt.Sprintf("%s auto-dispatched chained rollout bead %s for target %s after blocker cleared: %s", autoPrefix, id, target, desc)
	if dispatchMsg != "" {
		autoNote += " — " + shortTitle(dispatchMsg, 100)
	}
	if env.note != nil {
		if nerr := env.note(id, autoNote); nerr != nil && env.logf != nil {
			env.logf("blocker wakeup: note %s: %v", id, nerr)
		}
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{At: now, Kind: journalAuto, Bead: id, Class: cls, Action: "rollout_dispatch", Pass: true, Text: autoNote})
		_ = env.journal(journalRecord{At: now, Kind: journalUnblocked, Bead: id, Class: cls, Action: blockerActionDispatched, Pass: true, Text: desc})
	}
	summary := blockerInboxSummary(b, cleared, nil, blockerActionDispatched, "target "+target)
	if env.event != nil {
		if err := env.event(Event{
			Bead:    id,
			Kind:    EventUnblocked,
			Summary: summary,
			At:      now,
			Clears:  &InboxCondition{Kind: "bead_closed", Bead: id, Key: dedupeKey},
		}); err != nil && env.logf != nil {
			env.logf("blocker wakeup: write unblocked event %s: %v", id, err)
		}
	}
	return blockerActionDispatched, "target " + target, summary
}
