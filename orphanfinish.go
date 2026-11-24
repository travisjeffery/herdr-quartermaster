package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	defaultOrphanFinishIntervalSeconds = 300 // 5m
	defaultOrphanFinishGraceSeconds    = 600 // 10m
	defaultOrphanFinishMaxPerPass      = 5
)

// OrphanFinishConfig is [orphan_finish] in config.toml: bounded periodic
// finisher sweep for agentless merged and completed rolling-out beads.
type OrphanFinishConfig struct {
	// IntervalSeconds is how often the ticker runs the orphan finisher sweep;
	// 0 disables it. Default: 300 (5m).
	IntervalSeconds int `toml:"interval_seconds"`
	// GraceSeconds is how long an agentless merged or completed rolling-out
	// bead must remain untouched before the sweep acts on it. Default: 600 (10m).
	GraceSeconds int `toml:"grace_seconds"`
	// MaxPerPass caps how many orphan beads one sweep pass acts on; 0 disables
	// the sweep. Default: 5.
	MaxPerPass int `toml:"max_per_pass"`
}

func (c OrphanFinishConfig) enabled() bool {
	return c.IntervalSeconds > 0 && c.MaxPerPass > 0
}

func (c OrphanFinishConfig) interval() time.Duration {
	if c.IntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}

func (c OrphanFinishConfig) grace() time.Duration {
	if c.GraceSeconds <= 0 {
		return 0
	}
	return time.Duration(c.GraceSeconds) * time.Second
}

func (c OrphanFinishConfig) maxPerPass() int {
	if c.MaxPerPass <= 0 {
		return 0
	}
	return c.MaxPerPass
}

func (c Config) validateOrphanFinish() error {
	if c.OrphanFinish.IntervalSeconds < 0 {
		return fmt.Errorf("[orphan_finish] interval_seconds %d is negative; use 0 to disable", c.OrphanFinish.IntervalSeconds)
	}
	if c.OrphanFinish.GraceSeconds < 0 {
		return fmt.Errorf("[orphan_finish] grace_seconds %d is negative", c.OrphanFinish.GraceSeconds)
	}
	if c.OrphanFinish.MaxPerPass < 0 {
		return fmt.Errorf("[orphan_finish] max_per_pass %d is negative; use 0 to disable", c.OrphanFinish.MaxPerPass)
	}
	return nil
}

// OrphanMark records the last orphan-finisher action taken for a bead's
// observed state fingerprint so neither notifications nor failed closes loop.
type OrphanMark struct {
	Key    string    `json:"key"`
	Action string    `json:"action"` // "closed", "close_failed", "notified"
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`
}

const (
	orphanActionClosed      = "closed"
	orphanActionCloseFailed = "close_failed"
	orphanActionNotified    = "notified"
)

type orphanKind string

const (
	orphanKindMerged     orphanKind = "merged"
	orphanKindRollingOut orphanKind = "rolling_out"
)

type orphanCandidate struct {
	Bead         Bead
	Kind         orphanKind
	PR           *PR
	Runs         []Run
	CanAutoClose bool
	AutoCloseWhy string
	Key          string
}

// orphanFinishEnv is everything the orphan finisher sweep reads or changes,
// kept injectable for deterministic unit tests.
type orphanFinishEnv struct {
	show          func(id string) (Bead, error)
	agents        func() ([]Agent, bool, error)
	lookupPRs     func(urls []string) (map[string]PR, error)
	runView       func(repo, id string) (Run, error)
	canCloseLevel func(b Bead) bool
	enforce       func(b Bead, action, by string) (string, error)
	closeBead     func(id, reason string) error
	note          func(id, text string) error
	resolve       func(id string) (string, error)
	event         func(Event) error
	journal       func(journalRecord) error
	clearOutbox   func(agent string) error
	held          func(id string) bool
	headless      func(id string) bool
	logf          func(format string, args ...any)
}

func (t Ticker) liveOrphanFinishEnv() orphanFinishEnv {
	return orphanFinishEnv{
		show:      t.beads.Show,
		agents:    t.herdr.observedAgents,
		lookupPRs: t.gh.Lookup,
		runView:   t.gh.RunView,
		canCloseLevel: func(b Bead) bool {
			lvl, _ := t.cfg.beadLevel(b, autonomyStopped())
			return lvl >= classHousekeeping
		},
		enforce: func(b Bead, action, by string) (string, error) {
			return t.cfg.enforce(b, action, by)
		},
		closeBead: func(id, reason string) error {
			_, err := t.beads.run("close", id, "--reason", reason)
			return err
		},
		note: t.beads.Note,
		resolve: func(id string) (string, error) {
			return resolve(t.cfg, t.herdr, id, false)
		},
		event:       writeEvent,
		journal:     appendJournal,
		clearOutbox: clearQueuedOutbox,
		held: func(id string) bool {
			_, ok := isJobHeld(id)
			return ok
		},
		headless: headlessOwned,
		logf:     t.log.Printf,
	}
}

func orphanFinishDue(cfg Config, st TickerState, now time.Time) bool {
	every := cfg.OrphanFinish.interval()
	return cfg.OrphanFinish.enabled() && every > 0 && (st.LastOrphanFinish.IsZero() || now.Sub(st.LastOrphanFinish) >= every)
}

func (t Ticker) orphanFinish(st *TickerState, all []Bead, agents map[string]Agent, agentsComplete bool, now time.Time) {
	var env orphanFinishEnv
	if t.orphanEnv != nil {
		env = *t.orphanEnv
	} else {
		env = t.liveOrphanFinishEnv()
	}
	if env.logf == nil {
		env.logf = t.log.Printf
	}
	for _, line := range orphanFinishPass(t.cfg, env, st, all, agents, agentsComplete, now) {
		env.logf("orphan finish: %s", line)
	}
}

func hasNotedOwnPR(notes string) bool {
	return notedOwnPR.MatchString(notes)
}

func beadOwnsPR(b Bead, pr PR, branchPrefix string) bool {
	return ownsPR(b, prRef{URL: pr.URL, Branch: pr.Head}, branchPrefix)
}

// matchedCompletedRuns matches every URL in NotedRuns(notes) against runs by
// run ID. It returns (matched, true) only when at least one run is noted and
// every noted run is present in runs with Status == "completed".
func matchedCompletedRuns(notes string, runs []Run) ([]Run, bool) {
	noted := NotedRuns(notes)
	if len(noted) == 0 {
		return nil, false
	}
	byID := make(map[string]Run, len(runs))
	for _, r := range runs {
		byID[runKey(r)] = r
	}
	matched := make([]Run, 0, len(noted))
	for _, u := range noted {
		m := runURL.FindStringSubmatch(u)
		if m == nil {
			return nil, false
		}
		r, ok := byID[m[2]]
		if !ok || r.Status != "completed" {
			return nil, false
		}
		matched = append(matched, r)
	}
	return matched, true
}

func orphanStateKey(kind orphanKind, b Bead, pr *PR, runs []Run) string {
	parts := []string{string(kind), b.Status, b.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if b.HasLabel(LabelRollingOut) {
		parts = append(parts, LabelRollingOut)
	}
	if pr != nil {
		parts = append(parts, fmt.Sprintf("pr=%s@%s:%s", pr.URL, pr.State, pr.HeadSHA))
	}
	if len(runs) > 0 {
		rs := make([]string, 0, len(runs))
		for _, r := range runs {
			rs = append(rs, fmt.Sprintf("%d=%s/%s", r.ID, r.Status, r.Conclusion))
		}
		slices.Sort(rs)
		parts = append(parts, "runs="+strings.Join(rs, ","))
	}
	return strings.Join(parts, "|")
}

func orphanPRFor(cfg Config, st *TickerState, noted map[string]PR, b Bead, all []Bead) (PR, bool, bool) {
	owner := func(pr PR) string {
		if pr.Head == "" {
			return ""
		}
		if ob, ok := beadForBranch(cfg.BranchPrefix, pr.Head, all); ok {
			return ob.ID
		}
		return ""
	}
	// If any noted PR belonging to this bead is known to be OPEN, it is not finished.
	for _, u := range NotedPRs(b.Notes) {
		if p, ok := noted[u]; ok && p.State == "OPEN" {
			if oid := owner(p); oid == "" || oid == b.ID {
				return p, true, true
			}
		}
	}
	if pr, ok := st.PRs[b.ID]; ok {
		return pr, true, true
	}
	urls := NotedPRs(b.Notes)
	if len(urls) == 0 {
		return PR{}, false, true
	}
	byURL := map[string]PR{}
	allResolved := true
	for _, u := range urls {
		if p, ok := noted[u]; ok {
			byURL[u] = p
		} else {
			allResolved = false
		}
	}
	pr, merged, ok := prForBead(b, cfg.BranchPrefix, nil, byURL, owner)
	if ok && pr.State == "MERGED" && !beadOwnsPR(b, pr, cfg.BranchPrefix) {
		// Prefer the bead's own latest merged PR if a casually cited PR merged later.
		var own *PR
		for i := range merged {
			m := merged[i]
			if beadOwnsPR(b, m, cfg.BranchPrefix) && (own == nil || m.MergedAt.After(own.MergedAt)) {
				cp := m
				own = &cp
			}
		}
		if own != nil {
			pr = *own
		}
	}
	return pr, ok, allResolved
}

func classifyOrphanCandidate(cfg Config, b Bead, pr PR, hasPR, allPRsResolved bool, runs []Run, now time.Time, grace time.Duration) (orphanCandidate, bool) {
	if b.UpdatedAt.IsZero() || now.Sub(b.UpdatedAt) < grace {
		return orphanCandidate{}, false
	}
	latest := b.UpdatedAt
	if hasPR && !pr.MergedAt.IsZero() && pr.MergedAt.After(latest) {
		latest = pr.MergedAt
	}

	if b.HasLabel(LabelRollingOut) {
		if hasPR && pr.State == "OPEN" {
			return orphanCandidate{}, false
		}
		if len(NotedPRs(b.Notes)) > 0 && (!hasPR || pr.State != "MERGED") {
			return orphanCandidate{}, false
		}
		matched, done := matchedCompletedRuns(b.Notes, runs)
		if !done {
			return orphanCandidate{}, false
		}
		for _, r := range matched {
			if !r.CreatedAt.IsZero() && r.CreatedAt.After(latest) {
				latest = r.CreatedAt
			}
		}
		if now.Sub(latest) < grace {
			return orphanCandidate{}, false
		}
		var prPtr *PR
		if hasPR && pr.State == "MERGED" {
			p := pr
			prPtr = &p
		}
		return orphanCandidate{
			Bead:         b,
			Kind:         orphanKindRollingOut,
			PR:           prPtr,
			Runs:         matched,
			CanAutoClose: false,
			AutoCloseWhy: "labelled " + LabelRollingOut,
			Key:          orphanStateKey(orphanKindRollingOut, b, prPtr, matched),
		}, true
	}

	// Non-rolling-out bead: must be in merged state with no open PRs.
	if !hasPR || pr.State != "MERGED" {
		return orphanCandidate{}, false
	}
	if b.Status == StatusOpen && (!allPRsResolved || !beadOwnsPR(b, pr, cfg.BranchPrefix)) {
		return orphanCandidate{}, false
	}
	if b.Status == StatusInProgress && !beadOwnsPR(b, pr, cfg.BranchPrefix) &&
		citedBefore(b, prRef{URL: pr.URL, State: pr.State, MergedAt: pr.MergedAt, Branch: pr.Head}, cfg.BranchPrefix) {
		return orphanCandidate{}, false
	}

	var matchedRuns []Run
	if len(NotedRuns(b.Notes)) > 0 {
		var done bool
		matchedRuns, done = matchedCompletedRuns(b.Notes, runs)
		if !done {
			return orphanCandidate{}, false
		}
		for _, r := range matchedRuns {
			if !r.CreatedAt.IsZero() && r.CreatedAt.After(latest) {
				latest = r.CreatedAt
			}
		}
	}
	if now.Sub(latest) < grace {
		return orphanCandidate{}, false
	}

	p := pr
	cand := orphanCandidate{
		Bead: b,
		Kind: orphanKindMerged,
		PR:   &p,
		Runs: matchedRuns,
		Key:  orphanStateKey(orphanKindMerged, b, &p, matchedRuns),
	}

	inScope, scopeWhy := verifierScope(b)
	switch {
	case inScope:
		cand.AutoCloseWhy = scopeWhy
	case !beadOwnsPR(b, pr, cfg.BranchPrefix):
		cand.AutoCloseWhy = "merged PR is not noted as this bead's own PR"
	case workRemains(b) != "":
		cand.AutoCloseWhy = workRemains(b)
	case rejections(b) > 0:
		cand.AutoCloseWhy = "bead has a prior close rejection or keep-open note"
	default:
		cand.CanAutoClose = true
	}
	return cand, true
}

func formatOrphanPR(pr *PR) string {
	if pr == nil {
		return ""
	}
	if pr.Number > 0 && pr.URL != "" {
		return fmt.Sprintf("PR #%d (%s)", pr.Number, pr.URL)
	}
	if pr.URL != "" {
		return pr.URL
	}
	if pr.Number > 0 {
		return fmt.Sprintf("PR #%d", pr.Number)
	}
	return "merged PR"
}

func formatOrphanRuns(runs []Run) (text string, anyFailed bool) {
	if len(runs) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(runs))
	for _, r := range runs {
		if runFailed(r) {
			anyFailed = true
		}
		name := r.Workflow
		if name == "" {
			name = "run"
		}
		concl := r.Conclusion
		if concl == "" {
			concl = r.Status
		}
		if r.URL != "" {
			parts = append(parts, fmt.Sprintf("%s #%d %s (%s)", name, r.ID, concl, r.URL))
		} else {
			parts = append(parts, fmt.Sprintf("%s #%d (%s)", name, r.ID, concl))
		}
	}
	return strings.Join(parts, ", "), anyFailed
}

func orphanFinishSummary(c orphanCandidate, extraWhy string) string {
	id := c.Bead.ID
	var details []string
	if c.PR != nil {
		details = append(details, formatOrphanPR(c.PR)+" merged")
	}
	runsText, anyFailed := formatOrphanRuns(c.Runs)
	if runsText != "" {
		if anyFailed {
			details = append(details, "completed runs with failures: "+runsText)
		} else {
			details = append(details, "all noted runs completed: "+runsText)
		}
	}
	why := extraWhy
	if why == "" {
		why = c.AutoCloseWhy
	}
	if why != "" {
		details = append(details, why)
	}

	closeReason := "verified complete"
	if c.PR != nil && c.PR.Number > 0 {
		closeReason = fmt.Sprintf("PR #%d merged and verified", c.PR.Number)
	}
	closeCmd := fmt.Sprintf("quartermaster close %s --reason %q", id, closeReason)
	resumeCmd := fmt.Sprintf("quartermaster resume %s", id)

	switch {
	case c.Kind == orphanKindRollingOut && anyFailed:
		return fmt.Sprintf("orphan rolling-out bead %s has no live agent (%s); run `%s` to investigate or `%s` if superseded",
			id, strings.Join(details, "; "), resumeCmd, closeCmd)
	case c.Kind == orphanKindRollingOut:
		return fmt.Sprintf("orphan rolling-out bead %s has no live agent (%s); run `%s` to verify and close or `%s`",
			id, strings.Join(details, "; "), closeCmd, resumeCmd)
	case workRemains(c.Bead) != "" || rejections(c.Bead) > 0:
		return fmt.Sprintf("orphan merged bead %s has no live agent (%s); run `%s` to finish remaining work or `%s`",
			id, strings.Join(details, "; "), resumeCmd, closeCmd)
	default:
		return fmt.Sprintf("orphan merged bead %s has no live agent (%s); run `%s` or `%s`",
			id, strings.Join(details, "; "), closeCmd, resumeCmd)
	}
}

// orphanFinishPass runs one bounded sweep over non-closed beads, auto-closing
// eligible PR-only agentless beads at autonomy level 3+ and emitting
// deduplicated orphan_finish inbox items for rolling-out, verifier-scoped,
// below-L3, or failed-close beads.
func orphanFinishPass(cfg Config, env orphanFinishEnv, st *TickerState, all []Bead, agents map[string]Agent, agentsComplete bool, now time.Time) []string {
	if !cfg.OrphanFinish.enabled() {
		return nil
	}
	if !agentsComplete {
		return nil
	}
	st.LastOrphanFinish = now
	if st.Orphans == nil {
		st.Orphans = map[string]OrphanMark{}
	}
	nonClosed := make(map[string]bool, len(all))
	for _, b := range all {
		if b.Status != StatusClosed {
			nonClosed[b.ID] = true
		}
	}
	for id := range st.Orphans {
		if !nonClosed[id] {
			delete(st.Orphans, id)
		}
	}

	children := openChildCounts(all)
	live := make(map[string]bool, len(agents))
	for name, a := range agents {
		if name != "" {
			live[name] = true
		} else if a.Name != "" {
			live[a.Name] = true
		}
	}

	grace := cfg.OrphanFinish.grace()
	var eligible []Bead
	for _, b := range all {
		if b.Status != StatusOpen && b.Status != StatusInProgress {
			continue
		}
		if b.Type == "epic" || children[b.ID] > 0 {
			continue
		}
		if live[agentName(b.ID)] {
			continue
		}
		if st.Threads[b.ID].Relaunch {
			continue
		}
		if env.held != nil && env.held(b.ID) {
			continue
		}
		if env.headless != nil && env.headless(b.ID) {
			continue
		}
		if b.HasLabel(LabelStaleRejected) {
			continue
		}
		if b.UpdatedAt.IsZero() || now.Sub(b.UpdatedAt) < grace {
			continue
		}
		eligible = append(eligible, b)
	}
	if len(eligible) == 0 {
		return nil
	}

	// Copy cached noted PRs and batch-lookup any uncached PRs for open
	// candidates that explicitly own a PR or carry rolling-out.
	noted := make(map[string]PR, len(st.Noted))
	for u, p := range st.Noted {
		noted[u] = p
	}
	var wantURLs []string
	for _, b := range eligible {
		if b.Status != StatusOpen {
			continue
		}
		if _, cached := st.PRs[b.ID]; cached {
			continue
		}
		if !b.HasLabel(LabelRollingOut) && !hasNotedOwnPR(b.Notes) {
			continue
		}
		for _, u := range NotedPRs(b.Notes) {
			if p, ok := noted[u]; ok && p.State == "MERGED" {
				continue
			}
			if !slices.Contains(wantURLs, u) {
				wantURLs = append(wantURLs, u)
			}
		}
	}
	if len(wantURLs) > 0 && env.lookupPRs != nil {
		got, err := env.lookupPRs(wantURLs)
		if err != nil && env.logf != nil {
			env.logf("orphan finish: lookup PRs: %v", err)
		}
		for u, p := range got {
			noted[u] = p
			if p.State == "MERGED" {
				if st.Noted == nil {
					st.Noted = map[string]PR{}
				}
				st.Noted[u] = p
			}
		}
	}

	// Refresh runs for open candidates with noted runs (active beads' runs are
	// already refreshed by refreshRuns).
	var needRuns []Bead
	for _, b := range eligible {
		if b.Status != StatusOpen || len(NotedRuns(b.Notes)) == 0 {
			continue
		}
		if b.HasLabel(LabelRollingOut) || hasNotedOwnPR(b.Notes) {
			needRuns = append(needRuns, b)
		}
	}
	if len(needRuns) > 0 && env.runView != nil {
		logf := env.logf
		if logf == nil {
			logf = func(string, ...any) {}
		}
		fetched := nextRuns(st.Runs, needRuns, env.runView, logf)
		if len(fetched) > 0 {
			if st.Runs == nil {
				st.Runs = map[string][]Run{}
			}
			for id, rs := range fetched {
				st.Runs[id] = rs
			}
		}
	}

	var cands []orphanCandidate
	for _, b := range eligible {
		pr, hasPR, allResolved := orphanPRFor(cfg, st, noted, b, all)
		if cand, ok := classifyOrphanCandidate(cfg, b, pr, hasPR, allResolved, st.Runs[b.ID], now, grace); ok {
			cands = append(cands, cand)
		}
	}
	if len(cands) == 0 {
		return nil
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if !cands[i].Bead.UpdatedAt.Equal(cands[j].Bead.UpdatedAt) {
			return cands[i].Bead.UpdatedAt.Before(cands[j].Bead.UpdatedAt)
		}
		return cands[i].Bead.ID < cands[j].Bead.ID
	})

	maxPerPass := cfg.OrphanFinish.maxPerPass()
	acted := 0
	var lines []string
	for _, c := range cands {
		if acted >= maxPerPass {
			break
		}
		id := c.Bead.ID
		prev, seen := st.Orphans[id]
		if seen && prev.Key == c.Key {
			switch prev.Action {
			case orphanActionClosed, orphanActionCloseFailed:
				continue
			case orphanActionNotified:
				if !c.CanAutoClose {
					continue
				}
				if env.canCloseLevel != nil && !env.canCloseLevel(c.Bead) {
					continue
				}
			}
		}

		if c.CanAutoClose {
			// If the bead's current level is already below class 3, run enforce
			// once to record the exact reason and emit orphan_finish without
			// spending an extra bd show / agent re-check.
			if env.canCloseLevel != nil && !env.canCloseLevel(c.Bead) {
				_, enforceErr := "", error(nil)
				if env.enforce != nil {
					_, enforceErr = env.enforce(c.Bead, "close", "ticker")
				}
				if enforceErr != nil {
					if seen && prev.Key == c.Key && prev.Action == orphanActionNotified {
						continue
					}
					summary := orphanFinishSummary(c, "not auto-closed: "+enforceErr.Error())
					if env.event != nil {
						if err := env.event(Event{Bead: id, Kind: EventOrphanFinish, Summary: summary, At: now}); err != nil {
							lines = append(lines, fmt.Sprintf("%s: write orphan_finish event: %v", id, err))
							continue
						}
					}
					st.Orphans[id] = OrphanMark{Key: c.Key, Action: orphanActionNotified, At: now}
					if env.journal != nil {
						_ = env.journal(journalRecord{At: now, Kind: journalOrphanFinish, Bead: id, Class: classHousekeeping, Action: orphanActionNotified, Pass: false, Text: summary})
					}
					lines = append(lines, summary)
					acted++
					continue
				}
			}

			// Re-verify live bead and agent absence immediately before enforcing
			// and mutating so concurrent edits, labels, or resumed workers win.
			targetBead := c.Bead
			if env.show != nil {
				cur, err := env.show(id)
				if err != nil {
					summary := orphanFinishSummary(c, "auto-close failed (re-read bead): "+err.Error())
					st.Orphans[id] = OrphanMark{Key: c.Key, Action: orphanActionCloseFailed, At: now, Error: err.Error()}
					if env.event != nil {
						_ = env.event(Event{Bead: id, Kind: EventOrphanFinish, Summary: summary, At: now})
					}
					if env.journal != nil {
						_ = env.journal(journalRecord{At: now, Kind: journalOrphanFinish, Bead: id, Class: classHousekeeping, Action: orphanActionCloseFailed, Pass: false, Text: err.Error()})
					}
					lines = append(lines, summary)
					acted++
					continue
				}
				if cur.Status == StatusClosed {
					delete(st.Threads, id)
					delete(st.Orphans, id)
					continue
				}
				if cur.Status != c.Bead.Status || !cur.UpdatedAt.Equal(c.Bead.UpdatedAt) || cur.Type == "epic" || cur.HasLabel(LabelStaleRejected) {
					continue
				}
				if inScope, _ := verifierScope(cur); inScope || workRemains(cur) != "" || rejections(cur) > 0 {
					continue
				}
				targetBead = cur
			}
			if env.agents != nil {
				curAgents, complete, err := env.agents()
				if err != nil || !complete {
					continue
				}
				if slices.ContainsFunc(curAgents, func(a Agent) bool { return a.Name == agentName(id) }) {
					continue
				}
			}

			prefix, enforceErr := "", error(nil)
			if env.enforce != nil {
				prefix, enforceErr = env.enforce(targetBead, "close", "ticker")
			}
			if enforceErr != nil {
				if seen && prev.Key == c.Key && prev.Action == orphanActionNotified {
					continue
				}
				summary := orphanFinishSummary(c, "not auto-closed: "+enforceErr.Error())
				if env.event != nil {
					if err := env.event(Event{Bead: id, Kind: EventOrphanFinish, Summary: summary, At: now}); err != nil {
						lines = append(lines, fmt.Sprintf("%s: write orphan_finish event: %v", id, err))
						continue
					}
				}
				st.Orphans[id] = OrphanMark{Key: c.Key, Action: orphanActionNotified, At: now}
				if env.journal != nil {
					_ = env.journal(journalRecord{At: now, Kind: journalOrphanFinish, Bead: id, Class: classHousekeeping, Action: orphanActionNotified, Pass: false, Text: summary})
				}
				lines = append(lines, summary)
				acted++
				continue
			}

			reason := fmt.Sprintf("orphan finisher: %s merged; work ended at its PR with no live agent", formatOrphanPR(c.PR))
			if err := env.closeBead(id, reason); err != nil {
				summary := orphanFinishSummary(c, "auto-close failed: "+err.Error())
				st.Orphans[id] = OrphanMark{Key: c.Key, Action: orphanActionCloseFailed, At: now, Error: err.Error()}
				if env.event != nil {
					_ = env.event(Event{Bead: id, Kind: EventOrphanFinish, Summary: summary, At: now})
				}
				if env.journal != nil {
					_ = env.journal(journalRecord{At: now, Kind: journalOrphanFinish, Bead: id, Class: classHousekeeping, Action: orphanActionCloseFailed, Pass: false, Text: err.Error()})
				}
				lines = append(lines, summary)
				acted++
				continue
			}

			autoNote := fmt.Sprintf("%s orphan finisher closed after %s merged", prefix, formatOrphanPR(c.PR))
			if prefix == "" {
				autoNote = fmt.Sprintf("AUTO: [ticker L%d C%d] orphan finisher closed after %s merged", classHousekeeping, classHousekeeping, formatOrphanPR(c.PR))
			}
			if env.note != nil {
				if err := env.note(id, autoNote); err != nil {
					lines = append(lines, fmt.Sprintf("%s: note: %v", id, err))
				}
			}
			if env.journal != nil {
				_ = env.journal(journalRecord{At: now, Kind: journalAuto, Bead: id, Class: classHousekeeping, Action: "orphan_close", Pass: true, Text: autoNote})
				_ = env.journal(journalRecord{At: now, Kind: journalOrphanFinish, Bead: id, Class: classHousekeeping, Action: orphanActionClosed, Pass: true, Text: reason})
			}
			if env.clearOutbox != nil {
				_ = env.clearOutbox(agentName(id))
			}
			// Remove from active snapshots so leftActive does not emit a duplicate
			// "closed" event on the next tick.
			delete(st.Threads, id)

			summary := fmt.Sprintf("orphan finisher auto-closed %s after %s merged", id, formatOrphanPR(c.PR))
			if env.resolve != nil {
				if res, err := env.resolve(id); err != nil {
					summary += "; auto-resolve skipped: " + err.Error()
				} else if res != "" {
					summary += "; " + res
				}
			}
			st.Orphans[id] = OrphanMark{Key: c.Key, Action: orphanActionClosed, At: now}
			if env.event != nil {
				if err := env.event(Event{Bead: id, Kind: EventAutoClosed, Summary: summary, At: now}); err != nil {
					lines = append(lines, fmt.Sprintf("%s: write auto_closed event: %v", id, err))
				}
			}
			lines = append(lines, summary)
			acted++
			continue
		}

		summary := orphanFinishSummary(c, "")
		if env.event != nil {
			if err := env.event(Event{Bead: id, Kind: EventOrphanFinish, Summary: summary, At: now}); err != nil {
				lines = append(lines, fmt.Sprintf("%s: write orphan_finish event: %v", id, err))
				continue
			}
		}
		st.Orphans[id] = OrphanMark{Key: c.Key, Action: orphanActionNotified, At: now}
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalOrphanFinish, Bead: id, Class: -1, Action: orphanActionNotified, Pass: true, Text: summary})
		}
		lines = append(lines, summary)
		acted++
	}
	return lines
}
