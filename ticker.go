package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type TickerState struct {
	Threads          map[string]Snapshot   `json:"threads"`
	LastTick         time.Time             `json:"last_tick"`
	LastGH           time.Time             `json:"last_gh"`
	PRs              map[string]PR         `json:"prs"`                  // by bead id, from the last gh pass
	MergedPRs        map[string][]PR       `json:"merged_prs,omitempty"` // each bead's merged PRs, from the last gh pass
	Runs             map[string][]Run      `json:"runs"`                 // by bead id, from the last gh pass
	Noted            map[string]PR         `json:"noted_prs,omitempty"`  // by URL: linked PRs outside the listings, from the last gh pass
	LastNudge        time.Time             `json:"last_nudge"`
	Login            string                `json:"login"`
	CoordReady       time.Time             `json:"coord_ready"`
	CoordSeq         int64                 `json:"coord_seq"`
	GHFailingFor     time.Time             `json:"gh_failing_since"`
	GHFailingIn      []string              `json:"gh_failing_in"` // repos whose PR listing failed
	LastVerify       time.Time             `json:"last_verify"`
	Verified         map[string]VerifyMark `json:"verified"`     // by bead id, from verify passes
	VerifyRepos      map[string]VerifyRepo `json:"verify_repos"` // by repo path
	LastOrphanFinish time.Time             `json:"last_orphan_finish,omitempty"`
	Orphans          map[string]OrphanMark `json:"orphans,omitempty"`
	Blockers         BlockerState          `json:"blockers,omitempty"`
	GHLeft           int                   `json:"gh_left"`    // GraphQL points left, as of GHLeftAt
	GHLeftAt         time.Time             `json:"gh_left_at"` // when a verify pass last read GHLeft
	// Intervals are the running ticker's, which a config edit since it
	// started doesn't change: status measures lateness against these.
	Intervals *Intervals `json:"intervals,omitempty"`
}

type Intervals struct {
	Tick   time.Duration `json:"tick"`
	GH     time.Duration `json:"gh"`
	Verify time.Duration `json:"verify"`
}

func intervalsOf(cfg Config) Intervals {
	return Intervals{Tick: cfg.tick(), GH: cfg.ghEvery(), Verify: cfg.verifyEvery()}
}

func statePath() string { return filepath.Join(stateDir(), "state.json") }

func loadState() TickerState {
	s := TickerState{Threads: map[string]Snapshot{}, PRs: map[string]PR{}}
	b, err := os.ReadFile(statePath())
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Threads == nil {
		s.Threads = map[string]Snapshot{}
	}
	if s.PRs == nil {
		s.PRs = map[string]PR{}
	}
	return s
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func saveState(s TickerState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(statePath(), b)
}

type Ticker struct {
	cfg            Config
	herdr          Herdr
	beads          Beads
	gh             GH
	log            *log.Logger
	side           *sidebar
	watchEnv       *WatchEnv
	jobEnv         *JobEnv
	orphanEnv      *orphanFinishEnv
	blockerEnv     *blockerWakeEnv
	ciRerunEnv     *ciRerunEnv
	inboxRunnerEnv *inboxRunnerEnv
	inboxRunnerCtx context.Context
	inboxRunnerWG  *sync.WaitGroup
	notifyEnv      *notifyEnv
	notifyCtx      context.Context
	notifyWG       *sync.WaitGroup
	now            func() time.Time
}

func (t Ticker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func newTicker(cfg Config, logger *log.Logger) Ticker {
	h := newHerdr().on(cfg.coordSocket())
	return Ticker{cfg: cfg, herdr: h, beads: Beads{}, gh: GH{repo: cfg.Repo}, log: logger, side: newSidebar(h, cfg.tick(), logger)}
}

// gather joins beads, agents and PRs into threads. Only beads a worker could
// be on (in_progress, needs_me, blocked) are threads, while open child beads
// are still counted so parent beads with open children are not classified as
// merged.
func (t Ticker) gather(st *TickerState, now time.Time) ([]Thread, map[string]Agent, error) {
	threads, _, agents, _, _, _, _, err := t.gatherAll(st, now)
	return threads, agents, err
}

func (t Ticker) gatherAll(st *TickerState, now time.Time) ([]Thread, []Bead, map[string]Agent, bool, map[string]PR, map[string]PR, bool, error) {
	all, err := t.beads.NonClosed()
	if err != nil {
		return nil, nil, nil, false, nil, nil, false, err
	}
	children := openChildCounts(all)
	var active []Bead
	for _, b := range all {
		if isActiveStatus(b.Status) {
			active = append(active, b)
		}
	}
	agents, agentsComplete, err := t.herdr.observedAgents()
	if err != nil {
		// Bead truth is available even if worker transport is not. Return
		// those observations for desktop alerts only; do not run ordinary
		// transitions against missing agents or treat them as gone.
		threads := make([]Thread, len(active))
		for i, b := range active {
			threads[i] = Thread{Bead: b, OpenChildren: children[b.ID]}
		}
		return threads, all, nil, false, nil, nil, false, err
	}
	byName := map[string]Agent{}
	for _, a := range agents {
		if a.Name != "" {
			byName[a.Name] = a
		}
	}
	var freshPRs, knownPRs map[string]PR
	ghRefreshed := false
	if now.Sub(st.LastGH) >= t.cfg.ghEvery() {
		freshPRs, knownPRs = t.refreshPRs(st, active)
		t.refreshRuns(st, active)
		st.LastGH = now
		ghRefreshed = true
	}
	threads := make([]Thread, 0, len(active))
	for _, b := range active {
		th := threadForAt(t.cfg, *st, b, byName, now)
		th.OpenChildren = children[b.ID]
		th.AgentUnknown = !agentsComplete && th.Agent == nil
		threads = append(threads, th.derive())
	}
	return threads, all, byName, agentsComplete, freshPRs, knownPRs, ghRefreshed, nil
}

// threadFor joins one bead with its agent, by name, and what the last gh pass
// knows of its PRs and runs.
func threadFor(cfg Config, st TickerState, b Bead, byName map[string]Agent) Thread {
	return threadForAt(cfg, st, b, byName, time.Now())
}

func threadForAt(cfg Config, st TickerState, b Bead, byName map[string]Agent, now time.Time) Thread {
	th := Thread{Bead: b, Linear: linearKey(b, cfg.LinearPrefixes)}
	th.Level = cfg.beadMark(b)
	if a, ok := byName[agentName(b.ID)]; ok {
		th.Agent = &a
	}
	// PRs already holds the canonical selection with detailed evidence. Do
	// not promote a light Noted lookup after a failed detail fetch: it would
	// establish a false zero-review baseline for the next successful pass.
	if pr, ok := st.PRs[b.ID]; ok {
		// The ticker reloads its state every tick. Preserve a successful
		// observation between polls, but never indefinitely (or after a
		// failed refresh, which clears the gate in gatePRs).
		if pr.Gate != nil {
			age := now.Sub(pr.Gate.At)
			if age < 0 || age > 2*cfg.ghEvery() {
				pr.Gate, pr.DetailsFresh = nil, false
			}
		}
		policy := tickerMergePolicy(cfg, b, pr)
		pr.Policy = &policy
		th.PR = &pr
		th.Checks = summarizeChecks(pr.Checks)
		th.Reviews = reviewsNotBy(pr.Reviews, st.Login)
		th.BotReviews = botReviews(pr.Reviews)
	}
	th.Merged = st.MergedPRs[b.ID]
	th.Runs = st.Runs[b.ID]
	th = restoreThreadEvidence(th, st.Threads[b.ID])
	return th.derive()
}

func (t Ticker) refreshPRs(st *TickerState, active []Bead) (map[string]PR, map[string]PR) {
	if st.Login == "" {
		st.Login = t.gh.Login()
	}
	mine := map[string][]PR{}
	st.GHFailingIn = nil
	for _, repo := range t.cfg.allRepos() {
		prs, err := GH{repo: repo}.Mine()
		if err != nil {
			t.log.Printf("gh %s: %v", repo, err)
			st.GHFailingIn = append(st.GHFailingIn, repo)
			continue
		}
		mine[repo] = prs
	}
	if len(st.GHFailingIn) == 0 {
		st.GHFailingFor = time.Time{}
	} else if st.GHFailingFor.IsZero() {
		st.GHFailingFor = time.Now()
	}
	inList := map[string]PR{}
	for _, list := range mine {
		for _, pr := range list {
			inList[pr.URL] = pr
		}
	}
	noted := t.notedPRs(st, active, mine, inList)
	owner := prOwner(t.cfg, active, mine)
	// Each bead costs a gh pr view or two; run them a few at a time so a pass
	// over dozens of beads doesn't take a minute.
	found := make([]*PR, len(active))
	merged := make([][]PR, len(active))
	eachLimit(len(active), ghParallel, func(i int) {
		pr, m, ok := t.beadPR(st, active[i], mine, inList, noted, owner)
		if ok {
			found[i] = &pr
		}
		merged[i] = m
	})
	prs := map[string]PR{}
	st.MergedPRs = map[string][]PR{}
	for i, b := range active {
		if found[i] != nil {
			prs[b.ID] = *found[i]
		}
		if len(merged[i]) > 0 {
			st.MergedPRs[b.ID] = merged[i]
		}
	}
	t.gatePRs(prs)
	st.PRs = prs
	fresh := map[string]PR{}
	mergePRMap(fresh, inList)
	mergePRMap(fresh, noted)
	known := map[string]PR{}
	mergePRMap(known, fresh)
	for _, list := range st.MergedPRs {
		for _, p := range list {
			if p.URL != "" {
				mergePRMap(known, map[string]PR{p.URL: p})
			}
		}
	}
	for _, p := range st.PRs {
		if p.URL != "" {
			mergePRMap(known, map[string]PR{p.URL: p})
		}
	}
	return fresh, known
}

// beadPR is the PR b is delivered through, in full detail when open, and b's
// merged PRs; false when it has none or its detail can't be fetched and
// nothing older stands in.
func (t Ticker) beadPR(st *TickerState, b Bead, mine map[string][]PR, inList, noted map[string]PR, owner func(PR) string) (PR, []PR, bool) {
	own, listed := repoPRs(t.cfg, b, mine)
	if !listed {
		old, ok := st.PRs[b.ID]
		old.Gate, old.DetailsFresh = nil, false // cached details are not merge evidence
		return old, st.MergedPRs[b.ID], ok
	}
	byURL := map[string]PR{}
	for _, u := range NotedPRs(b.Notes) {
		if pr, ok := inList[u]; ok {
			byURL[u] = pr
		} else if pr, ok := noted[u]; ok {
			byURL[u] = pr
		}
	}
	pr, merged, ok := prForBead(b, t.cfg.BranchPrefix, own, byURL, owner)
	if !ok || pr.State != "OPEN" {
		return pr, merged, ok
	}
	full, err := t.gh.View(pr.URL, true)
	if err == nil {
		err = t.gh.MarkBotReviews(&full)
	}
	if err == nil {
		full.DetailsFresh = true
		return full, merged, true
	}
	t.log.Printf("gh pr view %d: %v", pr.Number, err)
	old, ok := st.PRs[b.ID]
	if !ok || old.Number != pr.Number {
		// The list entry has no reviews or checks; recording it would make
		// the next good pass see every existing review as new.
		return PR{}, merged, false
	}
	old.Gate, old.DetailsFresh = nil, false
	return old, merged, true
}

// notedPRs looks up, by URL, the PRs active beads' notes link that no repo
// listing holds: another repository's, or one past the listing's 100.
func (t Ticker) notedPRs(st *TickerState, active []Bead, mine map[string][]PR, inList map[string]PR) map[string]PR {
	st.Noted = nextNoted(t.cfg, st.Noted, active, mine, inList, t.gh.Lookup, t.log.Printf)
	return st.Noted
}

// nextNoted is notedPRs given the last pass's. Beads link dozens of PRs, and a
// gh pr view each every pass spent most of the hourly GraphQL budget, so they
// come in one batched lookup, and a PR already seen merged, which can't
// change, isn't asked for again. When the lookup fails a PR keeps what the
// last pass found.
func nextNoted(cfg Config, prev map[string]PR, active []Bead, mine map[string][]PR, inList map[string]PR, lookup func([]string) (map[string]PR, error), logf func(string, ...any)) map[string]PR {
	known := map[string]PR{}
	var want []string
	for _, b := range active {
		if _, listed := repoPRs(cfg, b, mine); !listed {
			continue
		}
		for _, u := range NotedPRs(b.Notes) {
			if _, ok := inList[u]; ok {
				continue
			}
			if _, ok := known[u]; ok || slices.Contains(want, u) {
				continue
			}
			if pr, ok := prev[u]; ok && pr.State == "MERGED" {
				known[u] = pr
				continue
			}
			want = append(want, u)
		}
	}
	if len(want) == 0 {
		return known
	}
	got, err := lookup(want)
	if err != nil {
		logf("gh linked PRs: %v", err)
	}
	for _, u := range want {
		if pr, ok := got[u]; ok {
			known[u] = pr
		} else if pr, ok := prev[u]; ok && err != nil {
			known[u] = pr
		}
	}
	return known
}

// ghParallel bounds the gh calls a pass makes at once.
const ghParallel = 8

// eachLimit calls f(0..n-1), at most limit at a time, and waits for them.
func eachLimit(n, limit int, f func(int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f(i)
		}()
	}
	wg.Wait()
}

// gatePRs fetches, in one query, the merge gate of every PR that is otherwise
// mergeable or a candidate for ready_for_review. A failed query clears the
// gate; cached approvals or thread counts are not evidence that the current PR
// is still safe to merge or free of unresolved review threads.
func (t Ticker) gatePRs(prs map[string]PR) {
	var want []PR
	for _, pr := range prs {
		c := summarizeChecks(pr.Checks)
		if pr.DetailsFresh && (mergeable(&pr, c) || reviewGateCandidate(&pr, c)) {
			want = append(want, pr)
		}
	}
	if len(want) == 0 {
		applyGates(prs, nil)
		return
	}
	now := t.clock()
	gates, err := t.gh.Gates(want, now)
	if err != nil {
		t.log.Printf("gh merge gates: %v", err)
	}
	applyGates(prs, gates)
}

func applyGates(prs map[string]PR, gates map[string]MergeGate) {
	for id, pr := range prs {
		if g, ok := gates[pr.URL]; ok && pr.DetailsFresh {
			pr.Gate = &g
		} else {
			pr.Gate = nil
		}
		prs[id] = pr
	}
}

// repoPRs is the PR listing of the bead's own repository, or false when
// listing it failed this pass.
func repoPRs(cfg Config, b Bead, mine map[string][]PR) ([]PR, bool) {
	prs, ok := mine[filepath.Clean(cfg.repoFor(b))]
	return prs, ok
}

// prOwner names the active bead whose branch a PR is on, or "". Branch names
// are per repository, so the PR must also be listed in that bead's own
// repository, where the bead itself finds it.
func prOwner(cfg Config, active []Bead, mine map[string][]PR) func(PR) string {
	return func(pr PR) string {
		b, ok := beadForBranch(cfg.BranchPrefix, pr.Head, active)
		if !ok {
			return ""
		}
		own, _ := repoPRs(cfg, b, mine)
		if slices.ContainsFunc(own, func(p PR) bool { return p.URL == pr.URL }) {
			return b.ID
		}
		return ""
	}
}

func rank(g Group, id string) string { return fmt.Sprintf("%d-%s", int(g), id) }

// once runs one pass: sidebar rows, events, prompts to threads, a nudge to the
// coordinator, and closing up beads that left the active set.
func (t Ticker) once(st *TickerState) error {
	now := t.clock()
	threads, allBeads, agents, agentsComplete, freshPRs, knownPRs, ghRefreshed, err := t.gatherAll(st, now)
	if err != nil {
		if threads != nil { // canonical Beads read succeeded; agents are unknown
			if notifyErr := reconcileDesktopWith(threads, nil, t.cfg.Desktop, now, submitDesktopMessage); notifyErr != nil {
				t.log.Printf("desktop notifications: %v", notifyErr)
			}
			t.reconcileNotify(threads, nil, now)
		}
		return err
	}
	// Only a pass that got its data counts: a ticker failing every pass shows
	// as late in `ticker status`.
	st.LastTick = now
	iv := intervalsOf(t.cfg)
	st.Intervals = &iv
	// Worktrees are listed with the PR pass, not every tick.
	if st.LastGH.Equal(now) {
		t.fixNames(st)
	}
	needsMe := map[string]bool{}
	for _, th := range threads {
		needsMe[agentName(th.Bead.ID)] = th.Bead.Status == StatusNeedsMe
	}
	events, err := expireLeases(t.cfg, liveAutonomyEnv(t.cfg, t.herdr), now)
	if err != nil {
		t.log.Printf("autonomy leases: %v", err)
	}
	for _, e := range events {
		t.log.Printf("%s: %s", orDash(e.Bead), e.Summary)
		if err := writeEvent(e); err != nil {
			t.log.Printf("inbox: %v", err)
		}
	}
	relaunch := t.launchMismatch(threads)
	// Remove departed workers' notices before delivery, not just at the end
	// of this pass when leftActive clears their sidebar rows.
	outboxReady := true
	for id := range st.Threads {
		if _, active := needsMe[agentName(id)]; !active {
			if err := clearQueuedOutbox(agentName(id)); err != nil {
				t.log.Printf("outbox %s: %v", id, err)
				outboxReady = false
			}
		}
	}
	// Failed cleanup holds delivery, but must not halt inbox reconciliation
	// or the other ticker work. leftActive keeps failed snapshots for retry.
	if outboxReady {
		for _, name := range deliverOutbox(t.cfg, t.herdr, agents, needsMe, relaunch) {
			t.log.Printf("delivered queued brief to %s", name)
		}
	}
	var rows []sidebarRow
	var alertThreads []Thread
	var desktopMilestones []desktopMilestone
	current := map[string]bool{}
	promptedThisTick := map[string]bool{}
	for i, th := range threads {
		id := th.Bead.ID
		current[id] = true
		prev, seen := st.Threads[id]
		if th.Agent != nil && th.Agent.Status == "blocked" {
			th.Prompt = t.readPrompt(th.Agent)
		}
		if th.Agent != nil && th.Agent.Session.Agent == "pi" && (th.Agent.Backend != rpcBackend || th.Agent.Status == "idle") {
			th.Turn = piLastTurn(th.Agent)
		}
		if seen && wokeAfterNeedsMe(th, prev) {
			if prompted, known := promptedSince(th.Agent, prev.ReadySince); known && !prompted {
				th.Unprompted = true
				t.log.Printf("%s woke at needs_me with no prompt since %s; not a resume", id, prev.ReadySince.Format(time.RFC3339))
			}
		}
		th = th.derive()
		opts := transitionOpts{
			debounce:   t.cfg.promptDebounce(),
			flap:       t.cfg.promptFlap(),
			reviewBump: t.cfg.reviewBumpAfter(),
		}
		out := transitionWith(th, prev, !seen, now, opts)
		if out.Resume {
			if err := t.resume(id); err != nil {
				t.log.Printf("resume %s: %v", id, err)
			} else {
				t.log.Printf("resumed %s: its agent went back to work after needs_me", id)
				th.Bead.Status = StatusInProgress
				th = th.derive()
			}
		}
		threads[i] = th
		snap := snapshotWith(th, prev, now, opts, !seen)
		if out.Resume {
			// transition saw needs_me and withheld ready events. A
			// successful resume must not baseline them away before next pass.
			snap.ReadyKey = ""
			snap.ReviewReadyKey = prev.ReviewReadyKey
			snap.ReviewReadyAt = prev.ReviewReadyAt
			snap.ReviewReadyReviews = prev.ReviewReadyReviews
			snap.ReviewReadyReviewed = prev.ReviewReadyReviewed
			snap.ReviewBumpKey = prev.ReviewBumpKey
		}
		t.journalAuto(th, prev, seen, &snap, now)
		ciRes := t.handleCIRerun(th, prev, !seen, ghRefreshed, hasEventKind(out.Events, EventFailing), &snap, now)
		for _, p := range out.Prompts {
			if ciRes.SuppressFailing && isFailingChecksPrompt(p) {
				continue
			}
			snap.Pending = append(snap.Pending, p)
		}
		if ciRes.FailingPrompt != "" {
			snap.Pending = append(snap.Pending, ciRes.FailingPrompt)
		}
		t.relaunch(th, &snap, relaunch[agentName(id)], now)
		stallGuard := t.stallGuardFor(th, prev, relaunch[agentName(id)] || snap.Relaunch)
		stallPlan := planStallRetry(t.cfg.StallRetry, th, prev, !seen, now, stallGuard)
		var threadEvents []Event
		for _, e := range out.Events {
			if stallPlan.SuppressFinished && e.Kind == EventFinished {
				continue
			}
			if ciRes.SuppressFailing && e.Kind == EventFailing {
				continue
			}
			threadEvents = append(threadEvents, e)
		}
		if ciRes.EmitFailing != nil {
			threadEvents = append(threadEvents, *ciRes.EmitFailing)
		}
		for _, e := range threadEvents {
			// A parsable approval prompt's event carries its tap links, so
			// what the coordinator relays to TJ is tappable as it stands;
			// a needs_me OPTIONS question's event carries its own.
			if e.Kind == EventNeedsYou && th.Prompt != nil {
				e.Summary += t.tapEventSuffix(th)
			} else if e.Kind == EventNeedsYou {
				e.Summary += t.tapQuestionEventSuffix(th)
			}
			if err := writeEvent(e); err != nil {
				t.log.Printf("inbox: %v", err)
			}
		}
		alertThreads = append(alertThreads, th)
		desktopMilestones = append(desktopMilestones, transitionMilestones(th, prev, !seen)...)
		if out.Notify && desktopReason(th) == "" && classify(th) != GroupNeedsYou {
			if err := t.herdr.Notify(fmt.Sprintf("%s: %s", id, classify(th)), shortTitle(th.Bead.Title, 60)); err != nil {
				t.log.Printf("desktop notification submission failed: %v", err)
			}
		}
		pendingDelivered := false
		if th.Agent != nil {
			// A needs_me thread waits on the user; typing into it would look like
			// an answer and resume it. Its prompts wait until it is answered.
			// A worker due a relaunch gets nothing else first (D8).
			if len(snap.Pending) > 0 && !headlessOwned(id) && th.Bead.Status != StatusNeedsMe && !relaunch[agentName(id)] && deliverable(snap, now, t.cfg.idle()) {
				text := strings.Join(snap.Pending, "\n\n")
				if _, err := promptPiAgent(t.herdr, *th.Agent, text); err != nil {
					t.log.Printf("prompt %s: %v", id, err)
				} else {
					t.log.Printf("prompted %s (%d item(s))", id, len(snap.Pending))
					snap.Pending = nil
					snap.ReadySince = time.Time{}
					pendingDelivered = true
					promptedThisTick[id] = true
				}
			}
		}
		if t.applyStallRetry(th, &snap, stallPlan, pendingDelivered, now) {
			promptedThisTick[id] = true
		}
		if th.Agent != nil {
			tokens := map[string]string{"sh_state": stateLine(th), "sh_rank": rank(classify(th), id)}
			rows = append(rows, sidebarRow{Name: id, Pane: th.Agent.PaneID, Tokens: tokens})
		}
		st.Threads[id] = snap
	}
	if err := reconcileInbox(alertThreads, t.beads.Show); err != nil {
		t.log.Printf("inbox reconciliation: %v", err)
	}
	if err := reconcileDesktopWith(alertThreads, desktopMilestones, t.cfg.Desktop, now, submitDesktopMessage); err != nil {
		t.log.Printf("desktop notifications: %v", err)
	}
	t.reconcileNotify(alertThreads, desktopMilestones, now)
	watchOutcomes := t.checkWatches(threads, agents, now)
	if coord, ok := agents[t.cfg.CoordinatorName]; ok {
		inbox, _ := readInbox()
		tokens := map[string]string{"sh_rank": coordinatorRank, "sh_state": coordinatorLine(threads, len(inbox))}
		rows = append(rows, sidebarRow{Name: t.cfg.CoordinatorName, Pane: coord.PaneID, Tokens: tokens})
	}
	// Set before leftActive clears departed panes, so a refresh can't bring
	// their tokens back.
	t.side.set(rows, time.Now())
	for id, prev := range st.Threads {
		if current[id] {
			continue
		}
		t.leftActive(st, id, prev, agents, now)
	}
	if verifyDue(t.cfg, *st, now) {
		t.verify(st, now)
	}
	if orphanFinishDue(t.cfg, *st, now) {
		t.orphanFinish(st, allBeads, agents, agentsComplete, now)
	}
	t.checkBlockers(st, allBeads, threads, agents, agentsComplete, freshPRs, knownPRs, ghRefreshed, promptedThisTick, relaunch, watchOutcomes, now)
	t.advanceWorkflows(st, threads, now)
	t.processJobs(st, threads, agents, now)
	t.nudgeCoordinator(st, agents, now)
	t.triggerInboxRunner(st, agents, now)
	return nil
}

func (t Ticker) reconcileNotify(threads []Thread, milestones []desktopMilestone, now time.Time) {
	var env notifyEnv
	if t.notifyEnv != nil {
		env = *t.notifyEnv
	} else {
		env = liveNotifyEnv(t.cfg, t.log.Printf)
	}
	if env.logf == nil && t.log != nil {
		env.logf = t.log.Printf
	}
	ctx := t.notifyCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := reconcileNotifyWith(ctx, t.cfg, env, threads, milestones, now, t.notifyWG); err != nil {
		t.log.Printf("notifications: %v", err)
	}
}

// launchMismatch is the ready workers, by agent name, whose launch mode no
// longer matches their level: started with their kind's bypass args below 9,
// or without them at 9 (docs/design/autonomy-dial.md, D8). The ticker lists
// only this machine's agents, and remote workers never start with bypass.
func (t Ticker) launchMismatch(threads []Thread) map[string]bool {
	out := map[string]bool{}
	for _, th := range threads {
		a := th.Agent
		// A needs_me worker waits on TJ, and typing /exit into it would read as
		// an answer; it relaunches once answered and idle again.
		if a == nil || !agentReady(a) || th.Bead.Status == StatusNeedsMe {
			continue
		}
		args := t.cfg.bypassArgs(a.Kind)
		if len(args) == 0 {
			continue
		}
		argv, err := t.herdr.ForegroundArgv(a.PaneID)
		if err != nil {
			t.log.Printf("launch mode of %s: %v", th.Bead.ID, err)
			continue
		}
		if launchedWith(argv, args) != t.cfg.launchBypass(th.Bead, localHost) {
			out[a.Name] = true
		}
	}
	return out
}

// launchedWith reports whether any of a pane's foreground processes was
// started with args, in order.
func launchedWith(argv [][]string, args []string) bool {
	for _, a := range argv {
		for i := 0; len(args) > 0 && i+len(args) <= len(a); i++ {
			if slices.Equal(a[i:i+len(args)], args) {
				return true
			}
		}
	}
	return false
}

// relaunch restarts a worker whose launch mode no longer matches its level,
// in two passes: once it has been idle a while it is asked to exit, and on a
// later pass, its agent gone, it is resumed with the right args, keeping the
// conversation.
func (t Ticker) relaunch(th Thread, snap *Snapshot, due bool, now time.Time) {
	id := th.Bead.ID
	switch {
	case th.Agent == nil && snap.Relaunch:
		snap.Relaunch = false
		mode := "without"
		if t.cfg.launchBypass(th.Bead, localHost) {
			mode = "with"
		}
		// The relaunch carries out a level change already made, so a lowered
		// level must not block it.
		if _, err := resume(t.cfg, t.herdr, []string{id}, "", "TJ"); err != nil {
			t.log.Printf("relaunch %s: %v", id, err)
			_ = t.beads.Note(id, "quartermaster could not relaunch the worker "+mode+" bypass args after it exited: "+err.Error()+"; `quartermaster resume "+id+"`")
			return
		}
		t.log.Printf("relaunched %s %s bypass args", id, mode)
		_ = t.beads.Note(id, "quartermaster relaunched the worker "+mode+" its kind's bypass args, to match its autonomy level")
	case due && deliverable(*snap, now, t.cfg.idle()):
		if err := t.herdr.Prompt(th.Agent.PaneID, "/exit"); err != nil {
			t.log.Printf("relaunch %s: exit: %v", id, err)
			return
		}
		t.log.Printf("relaunch %s: asked it to exit", id)
		snap.Relaunch = true
	case th.Agent != nil && !due && agentReady(th.Agent):
		snap.Relaunch = false
	}
}

// readPrompt is the approval prompt an agent shows, nil when there is none or
// it can't be read: a pi worker's ask over its control socket, else what its
// pane shows.
func (t Ticker) readPrompt(a *Agent) *ApprovalPrompt {
	if ask, ok, err := piAskOf(t.herdr.hostName(), a); ok {
		if err != nil {
			t.log.Printf("read %s's ask: %v", a.Name, err)
		}
		if ask != nil {
			p := ask.prompt()
			return &p
		}
		// Blocked on something else, such as an extension dialog: the screen
		// may show it.
	}
	pane := a.PaneID
	screen, err := t.herdr.Read(pane, promptReadLines, callTimeout)
	if err != nil {
		t.log.Printf("read prompt in %s: %v", pane, err)
		return nil
	}
	if p, ok := parsePrompt(screen); ok {
		return &p
	}
	return nil
}

// resume sets a needs_me bead the user answered in its pane back to
// in_progress, noting why so the question stays in its history.
func (t Ticker) resume(id string) error {
	if err := t.beads.SetStatus(id, StatusInProgress); err != nil {
		return err
	}
	return t.beads.Note(id, resumeNote)
}

// leftActive handles a bead that is no longer in_progress/needs_me/blocked:
// usually closed by its thread after the PR merged.
func (t Ticker) leftActive(st *TickerState, id string, prev Snapshot, agents map[string]Agent, now time.Time) {
	if err := clearQueuedOutbox(agentName(id)); err != nil {
		t.log.Printf("outbox %s: %v", id, err)
		return // keep the snapshot so the next pass retries cleanup
	}
	b, err := t.beads.Show(id)
	if err != nil {
		t.log.Printf("show %s: %v", id, err)
		delete(st.Threads, id)
		return
	}
	a, hasAgent := agents[agentName(id)]
	if hasAgent {
		t.herdr.ClearState(a.PaneID)
	}
	if b.Status == StatusClosed {
		summary := fmt.Sprintf("%s closed", id)
		if prev.PRState == "MERGED" {
			summary += fmt.Sprintf(" after PR #%d merged", prev.PRNumber)
		}
		// The coordinator may have resolved it before this pass saw it close.
		if role, rerr := t.cfg.resolveRoleForBead(b, ""); rerr == nil && !role.Worktree {
			if _, ok := hasRoleDir(id); ok {
				summary += fmt.Sprintf("; run `quartermaster resolve %s` to remove its role directory", id)
			} else {
				summary += "; its role directory is already removed"
			}
		} else {
			wt, _, werr := linkedWorktree(t.cfg, t.herdr, b)
			if lvl, _ := t.cfg.beadLevel(b, autonomyStopped()); werr == nil && wt == nil {
				summary += "; its worktree is already removed"
			} else if lvl >= classHousekeeping && prev.PRState == "MERGED" && (!hasAgent || agentReady(&a)) {
				if res, err := resolve(t.cfg, t.herdr, id, false); err != nil {
					summary += "; auto-resolve failed: " + err.Error()
				} else {
					summary += "; " + res
				}
			} else {
				summary += fmt.Sprintf("; run `quartermaster resolve %s` to remove its worktree", id)
			}
		}
		_ = writeEvent(Event{Bead: id, Kind: "closed", Summary: summary, At: now})
	}
	delete(st.Threads, id)
}

func (t Ticker) checkWatches(threads []Thread, agents map[string]Agent, now time.Time) []watchCheckOutcome {
	var env WatchEnv
	if t.watchEnv != nil {
		env = *t.watchEnv
	} else {
		env = liveWatchEnv(t.cfg, t.herdr)
	}
	if env.Now == nil {
		env.Now = func() time.Time { return now }
	}
	if env.Logf == nil {
		env.Logf = t.log.Printf
	}
	outcomes, err := runWatchesPass(t.cfg, env, threads, agents, "", false)
	if err != nil {
		t.log.Printf("watches: %v", err)
		return nil
	}
	for _, o := range outcomes {
		switch o.Action {
		case "triggered":
			t.log.Printf("watch %s (%s) triggered: %s", o.Watch.ID, o.Watch.Bead, o.Watch.Evidence)
		case "expired":
			t.log.Printf("watch %s (%s) expired", o.Watch.ID, o.Watch.Bead)
		case "retired":
			t.log.Printf("watch %s (%s) retired: %s", o.Watch.ID, o.Watch.Bead, o.Watch.Evidence)
		case "error":
			t.log.Printf("watch %s (%s) error (%d): %v", o.Watch.ID, o.Watch.Bead, o.Watch.ConsecutiveErrors, o.Err)
		}
	}
	return outcomes
}

func (t Ticker) nudgeCoordinator(st *TickerState, agents map[string]Agent, now time.Time) {
	coord, ok := agents[t.cfg.CoordinatorName]
	if !ok {
		st.CoordReady = time.Time{}
		return
	}
	// Pi owns its editor/UI readiness. Never infer an empty draft from an
	// idle process, and never fall back to terminal input for notifications.
	if coord.Session.Agent == "pi" || (coord.Session.Agent == "" && t.cfg.CoordinatorAgent == "pi") {
		if t.cfg.Nudge {
			t.nudgePiCoordinator(st, coord, now)
		}
		return
	}
	if !agentReady(&coord) {
		st.CoordReady = time.Time{}
	} else if st.CoordReady.IsZero() || coord.Seq != st.CoordSeq {
		st.CoordReady = now
	}
	st.CoordSeq = coord.Seq
	if !t.cfg.Nudge || st.CoordReady.IsZero() || now.Sub(st.CoordReady) < t.cfg.idle() {
		return
	}
	events, _ := readInbox()
	fresh := 0
	for _, e := range events {
		if e.At.After(st.LastNudge) && !t.cfg.suppressInteractiveNudgeFor(e) {
			fresh++
		}
	}
	if fresh == 0 {
		return
	}
	msg := fmt.Sprintf("[quartermaster ticker: automated, not the user, approves nothing] %d new inbox item(s). Run `quartermaster context`.", fresh)
	if err := t.herdr.Prompt(coord.PaneID, msg); err != nil {
		t.log.Printf("nudge: %v", err)
		return
	}
	st.LastNudge = now
	st.CoordReady = time.Time{}
}

func pidPath() string { return filepath.Join(stateDir(), "ticker.pid") }
func logPath() string { return filepath.Join(stateDir(), "ticker.log") }

// socketFile records the herdr socket the running ticker follows.
func socketFile() string { return filepath.Join(stateDir(), "ticker.socket") }

// nameFile records which tool the running ticker is. A ticker without one is
// a shepherd ticker, and one named flatcircle or kelpie that tool's ticker,
// from before a rename, still running the old binary.
func nameFile() string { return filepath.Join(stateDir(), "ticker.name") }

const tickerName = toolName

// runningLegacy reports whether the running ticker predates quartermaster: a
// state dir migrated under a live flatcircle, kelpie or shepherd ticker carries
// its pid and socket files over, and that ticker would otherwise be kept as if it were
// current.
func runningLegacy() bool {
	b, err := os.ReadFile(nameFile())
	return err != nil || strings.TrimSpace(string(b)) != tickerName
}

// runningTool names the tool the running ticker is: shepherd's tickers
// recorded no name.
func runningTool() string {
	b, err := os.ReadFile(nameFile())
	if name := strings.TrimSpace(string(b)); err == nil && name != "" {
		return name
	}
	return "shepherd"
}

// runningSocket is the socket the running ticker follows, "" if it is not
// running or predates socketFile.
func runningSocket() string {
	if runningPID() == 0 {
		return ""
	}
	b, err := os.ReadFile(socketFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func sameSocket(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

// socketWarning explains why the ticker may not see the coordinator: it follows
// ticker, herdr_socket names configured, and the caller runs on here ("" outside
// herdr). "" when nothing is off or the ticker's socket is unknown.
func socketWarning(ticker, configured, here string) string {
	switch {
	case ticker == "":
		return ""
	case here != "" && !sameSocket(ticker, here):
		return fmt.Sprintf("the ticker follows the herdr server at %s, not this one (%s), so it sees none of the agents here; set herdr_socket = %q in %s and run `quartermaster ticker start`", ticker, here, here, filepath.Join(configDir(), "config.toml"))
	case !sameSocket(ticker, configured):
		return fmt.Sprintf("the ticker follows %s but herdr_socket is %s; run `quartermaster ticker start` to move it", ticker, configured)
	}
	return ""
}

func runningPID() int {
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil || !isTicker(pid) {
		return 0
	}
	return pid
}

// isTicker reports whether pid is a `quartermaster ticker run`, so a stale pid file
// whose pid was reused never gets another process signalled. Without ps it
// trusts the pid file.
func isTicker(pid int) bool {
	if pid == os.Getpid() {
		return true
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		var exit *exec.ExitError
		return !errors.As(err, &exit)
	}
	return tickerArgs(string(out))
}

func tickerArgs(args string) bool {
	return strings.HasSuffix(strings.TrimSpace(args), " ticker run")
}

func tickerRun(cfg Config) error {
	if pid := runningPID(); pid != 0 && pid != os.Getpid() {
		return fmt.Errorf("ticker already running (pid %d)", pid)
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidPath())
	if err := os.WriteFile(socketFile(), []byte(cfg.coordSocket()), 0o644); err != nil {
		return err
	}
	defer os.Remove(socketFile())
	if err := os.WriteFile(nameFile(), []byte(tickerName), 0o644); err != nil {
		return err
	}
	defer os.Remove(nameFile())
	f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	logger := log.New(f, "", log.LstdFlags)
	logger.Printf("ticker %s started, every %s, on %s", version, cfg.tick(), cfg.coordSocket())
	t := newTicker(cfg, logger)
	runnerCtx, cancelRunner := context.WithCancel(context.Background())
	defer cancelRunner()
	var runnerWG sync.WaitGroup
	t.inboxRunnerCtx = runnerCtx
	t.inboxRunnerWG = &runnerWG
	notifyCtx, cancelNotify := context.WithCancel(context.Background())
	defer cancelNotify()
	var notifyWG sync.WaitGroup
	t.notifyCtx = notifyCtx
	t.notifyWG = &notifyWG
	if cfg.Tap.Enabled {
		// The tap endpoint rides the ticker: no new daemon. A bind failure
		// (no tailnet, port taken) leaves the ticker running and taps off.
		if ln, err := startTapServerWithConfig(cfg, livePromptEnv(cfg, t.herdr), liveTapInboxEnv(cfg, t.herdr), logger.Printf); err != nil {
			logger.Printf("tap: %v (approval taps unavailable)", err)
		} else {
			defer ln.Close()
		}
	}
	done := make(chan struct{})
	defer close(done)
	go t.side.run(done)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	timer := time.NewTicker(cfg.tick())
	defer timer.Stop()
	var lastQuota time.Time
	routerCfg := cfg
	for {
		st := loadState()
		if err := t.once(&st); err != nil {
			logger.Printf("tick: %v", err)
		}
		// Keep the pi quota router's extensions, config and snapshot fresh for
		// sessions already running and new launches; a launch writes them too.
		// Reload config.toml on each refresh so router edits apply live without
		// restarting the ticker, keeping the last known-good config on parse error.
		if now := time.Now(); now.Sub(lastQuota) >= time.Minute {
			lastQuota = now
			refreshPiRouter(&routerCfg, loadConfig, now, logger.Printf)
		}
		if err := saveState(st); err != nil {
			logger.Printf("state: %v", err)
		}
		select {
		case <-sig:
			logger.Printf("ticker stopping")
			cancelRunner()
			cancelNotify()
			runnerWG.Wait()
			notifyWG.Wait()
			return nil
		case <-timer.C:
		}
	}
}

// refreshPiRouter keeps the embedded pi extension, router.json and quota.json
// current on the ticker's periodic pass. It reloads config.toml via load before
// writing router.json so configuration edits take effect without restarting the
// ticker; if load fails (for example during an in-progress edit), it logs the
// error, keeps *lastGood intact, and preserves any existing router.json on disk.
func refreshPiRouter(lastGood *Config, load func() (Config, error), now time.Time, logf func(string, ...any)) {
	if _, err := piExtensionFile(); err != nil {
		logf("pi extension: %v", err)
	}
	if fresh, err := load(); err != nil {
		logf("pi router config reload: %v (keeping last known-good config)", err)
		if _, statErr := os.Stat(piRouterConfigPath()); errors.Is(statErr, fs.ErrNotExist) {
			if _, werr := lastGood.writePiRouterConfig(); werr != nil {
				logf("pi router config: %v", werr)
			}
		}
	} else {
		*lastGood = fresh
		if _, err := lastGood.writePiRouterConfig(); err != nil {
			logf("pi router config: %v", err)
		}
	}
	if err := writeQuotaSnapshot(now); err != nil {
		logf("quota snapshot: %v", err)
	}
}

func tickerStart(cfg Config) error {
	if pid := runningPID(); pid != 0 {
		sock := runningSocket()
		switch {
		case runningLegacy():
			fmt.Printf("ticker (pid %d) is %s's, from before the rename; restarting it as %s\n", pid, runningTool(), toolName)
		case sock != "" && sameSocket(sock, cfg.coordSocket()):
			fmt.Printf("ticker already running (pid %d)\n", pid)
			return nil
		default:
			// herdr_socket changed since it started, or an older version started
			// it on whichever server ran startup: follow the configured server.
			if sock == "" {
				sock = "an unknown herdr server"
			}
			fmt.Printf("ticker (pid %d) is on %s, not %s; restarting it\n", pid, sock, cfg.coordSocket())
		}
		if err := tickerStop(); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "ticker", "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Printf("ticker started (pid %d), log %s\n", cmd.Process.Pid, logPath())
	return cmd.Process.Release()
}

func tickerStop() error {
	pid := runningPID()
	if pid == 0 {
		fmt.Println("ticker not running")
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	// A pass can sit in a slow bd, gh or herdr call before it sees SIGTERM.
	for i := 0; i < 300 && runningPID() != 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if runningPID() == pid {
		return fmt.Errorf("ticker (pid %d) did not stop within 30s", pid)
	}
	fmt.Printf("ticker stopped (pid %d)\n", pid)
	return nil
}

func tickerStatus(cfg Config) {
	if pid := runningPID(); pid != 0 {
		sock := runningSocket()
		if sock == "" {
			sock = "unknown herdr server (started by an older version; restart it)"
		}
		fmt.Printf("ticker running (pid %d) on %s, log %s\n", pid, sock, logPath())
		st := loadState()
		fmt.Println(statusAges(st, time.Now()))
		for _, w := range statusWarnings(cfg, st, time.Now()) {
			fmt.Println("warning:", w)
		}
		if runningLegacy() {
			fmt.Printf("warning: it is %s's ticker, from before the rename; run `%s ticker start` to restart it as %s\n", runningTool(), toolName, toolName)
		}
		if w := socketWarning(runningSocket(), cfg.coordSocket(), os.Getenv("HERDR_SOCKET_PATH")); w != "" {
			fmt.Println("warning:", w)
		}
	} else {
		fmt.Println("ticker not running")
	}
}

// Inbox: one JSON file per event; handled ones move to inbox/done.
func inboxDir() string { return filepath.Join(stateDir(), "inbox") }

func writeEvent(e Event) error {
	if e.Clears == nil {
		e.Clears = defaultInboxCondition(e)
	}
	if err := os.MkdirAll(inboxDir(), 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%s-%s.json", e.At.UnixNano(), agentName(e.Bead), e.Kind)
	return rpcSave(filepath.Join(inboxDir(), name), e)
}

type InboxItem struct {
	Event
	File string `json:"-"`
}

func readInbox() ([]InboxItem, error) {
	entries, err := os.ReadDir(inboxDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []InboxItem
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(inboxDir(), e.Name()))
		if err != nil {
			continue
		}
		var it InboxItem
		if json.Unmarshal(b, &it.Event) == nil {
			it.File = e.Name()
			items = append(items, it)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].File < items[j].File })
	return items, nil
}

// inboxDone retains the bulk CLI: all items, or matching bead IDs/filenames.
// Interactive views use inboxDoneFiles instead, with the files they displayed.
func inboxDone(beads []string) (int, error) {
	items, err := readInbox()
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	for _, b := range beads {
		want[b] = true
	}
	var files []string
	for _, it := range items {
		if len(want) == 0 || want[it.Bead] || want[it.File] {
			files = append(files, it.File)
		}
	}
	return inboxDoneFiles(files)
}

func (t Ticker) fixNames(st *TickerState) {
	active, err := t.beads.Active()
	if err != nil {
		return
	}
	// All agents, unnamed ones included: gather only keeps named agents.
	list, err := t.herdr.Agents()
	if err != nil {
		return
	}
	wts := map[string][]Worktree{}
	for _, repo := range t.cfg.allRepos() {
		if list, err := t.herdr.Worktrees(repo); err == nil {
			wts[filepath.Clean(repo)] = list
		}
	}
	coordinator := ""
	for _, a := range list {
		if a.Name == t.cfg.CoordinatorName {
			coordinator = a.WorkspaceID
		}
	}
	for _, r := range nameFixes(t.cfg, active, list, st.PRs, wts) {
		if err := t.herdr.Rename(r.Pane, r.To); err != nil {
			t.log.Printf("rename %s: %v", r.Pane, err)
			continue
		}
		why := "it is the only agent in that bead's worktree"
		if isLegacyName(r.From) && r.To == t.cfg.CoordinatorName {
			why = "the coordinator's name before the rename to " + toolName
			moveQueuedBrief(r.From, r.To)
			for _, a := range list {
				if a.PaneID == r.Pane {
					coordinator = a.WorkspaceID
				}
			}
		}
		t.log.Printf("renamed %s from %q to %s: %s", r.Pane, r.From, r.To, why)
	}
	// A rename (kelpie, flatcircle) left the coordinator's workspace labelled with an old
	// name; it follows the coordinator here.
	if relabelCoordinatorWorkspace(t.herdr, coordinator) {
		t.log.Printf("relabelled the coordinator's workspace %s to %s", coordinator, toolName)
	}
}
