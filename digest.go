package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Section is where a bead sits in the digest. The order is the digest's order:
// what needs the user first, the backlog last.
type Section int

const (
	SecNeedsYou Section = iota
	SecWaiting
	SecBlocked
	SecInProgress
	SecReady
	SecBacklog
	SecMerged
	SecRollingOut
)

var sectionNames = map[Section]string{
	SecNeedsYou:   "Needs you",
	SecWaiting:    "Waiting on review/merge",
	SecBlocked:    "Blocked",
	SecInProgress: "In progress",
	SecReady:      "Ready / next",
	SecBacklog:    "Backlog",
	SecMerged:     "Merged / closing up",
	SecRollingOut: "Rolling out",
}

func (s Section) String() string { return sectionNames[s] }

// How long a bead may go without an update before the digest calls it stale.
const (
	staleNeedsYou   = 4 * time.Hour
	staleInProgress = 24 * time.Hour
	staleBlocked    = 7 * 24 * time.Hour
)

// backlogTop is how many backlog beads the digest lists; the rest are a count.
const backlogTop = 5

// notedPRCap caps the PRs looked up per bead: the latest ones its notes link.
const notedPRCap = 5

// DigestPR is a PR as the digest needs it: one GraphQL query answers all of
// them, so it carries the check rollup rather than every check.
type DigestPR struct {
	Number         int       `json:"number"`
	URL            string    `json:"url"`
	State          string    `json:"state"`
	IsDraft        bool      `json:"isDraft,omitempty"`
	ReviewDecision string    `json:"reviewDecision,omitempty"`
	HeadSHA        string    `json:"headRefOid,omitempty"`
	MergeState     string    `json:"mergeStateStatus,omitempty"`
	Mergeable      string    `json:"mergeable,omitempty"` // MERGEABLE, CONFLICTING or UNKNOWN
	MergedAt       time.Time `json:"mergedAt,omitzero"`
	Checks         string    `json:"checks,omitempty"` // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED or ""
}

// DigestItem is one bead's line in the digest.
type DigestItem struct {
	State        ThreadState
	Bead         Bead
	Section      Section
	PR           *DigestPR
	OpenPRs      int
	Blockers     []string // non-closed beads blocking this one, "id (status)"
	Agent        string   // its herdr agent's status, "" without one
	Linear       string
	Stale        bool
	Parked       bool
	ParkedReason string
	parkedKind   string
	SnoozedUntil time.Time
}

type Digest struct {
	At       time.Time
	Sections map[Section][]DigestItem
	Changes  []string
	Warnings []string
	Total    int
	// Auto is the AUTO: steps taken since the last digest; Changes lists them
	// grouped by class.
	Auto []autoStep
}

// digestState is what the digest remembers between runs: each bead's section,
// to tell what moved, and each PR, to tell what went green, got approved or
// merged. A merged PR never changes again, so it is not looked up again.
type digestState struct {
	At    time.Time           `json:"at"`
	Beads map[string]Section  `json:"beads"`
	PRs   map[string]DigestPR `json:"prs"`
	// Auto is how many AUTO: notes each open bead had, to tell which steps
	// its worker took without TJ since.
	Auto map[string]int `json:"auto,omitempty"`
}

func digestPath() string      { return filepath.Join(stateDir(), "digest.md") }
func digestStatePath() string { return filepath.Join(stateDir(), "digest-state.json") }

func loadDigestState() digestState {
	var s digestState
	if b, err := os.ReadFile(digestStatePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveDigestState(s digestState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(digestStatePath(), b)
}

// digestInputs is everything a digest is built from, read once per run.
type digestInputs struct {
	ticker  TickerState
	snoozes SnoozeState
	cfg     Config
	open    []Bead // every bead that isn't closed
	closed  []Bead // beads closed since the last digest
	agents  map[string]Agent
	prs     map[string]DigestPR // by URL
	branch  map[string]string   // bead id → the PR URL the ticker found on its branch

	asked, queries int // PRs looked up on GitHub, and the GraphQL queries that took
}

// digestEnv is the outside world, so a test can build a digest from fixtures.
type digestEnv struct {
	cfg     Config
	open    func() ([]Bead, error)
	closed  func(since time.Time) ([]Bead, error)
	agents  func() ([]Agent, error)
	ticker  func() TickerState
	snoozes func() SnoozeState
	lookup  func(urls []string) (map[string]DigestPR, int, error)
}

func liveDigestEnv(cfg Config) digestEnv {
	return digestEnv{
		cfg:  cfg,
		open: func() ([]Bead, error) { return Beads{}.list() },
		closed: func(since time.Time) ([]Bead, error) {
			return Beads{}.list("--status", StatusClosed, "--closed-after", since.UTC().Format(time.RFC3339))
		},
		agents:  newHerdr().on(cfg.coordSocket()).Agents,
		ticker:  loadState,
		snoozes: loadSnoozeState,
		lookup:  GH{repo: cfg.Repo}.DigestPRs,
	}
}

// gatherDigest reads beads, agents and the ticker's state at once, then asks
// GitHub about the PRs that active beads link and that aren't already known to
// have merged or closed.
func gatherDigest(env digestEnv, prev digestState) (digestInputs, []string, error) {
	var in digestInputs
	var warnings []string
	var openErr, closedErr, agentErr error
	var agents []Agent
	var st TickerState
	var wg sync.WaitGroup
	wg.Go(func() { in.open, openErr = env.open() })
	if !prev.At.IsZero() {
		wg.Go(func() { in.closed, closedErr = env.closed(prev.At) })
	}
	wg.Go(func() { agents, agentErr = env.agents() })
	wg.Go(func() { st = env.ticker() })
	wg.Wait()
	in.ticker, in.cfg = st, env.cfg
	if env.snoozes != nil {
		in.snoozes = env.snoozes()
	} else {
		in.snoozes = loadSnoozeState()
	}
	if openErr != nil {
		return in, nil, openErr
	}
	if closedErr != nil {
		warnings = append(warnings, "closed beads: "+closedErr.Error())
	}
	if agentErr != nil {
		warnings = append(warnings, "herdr agents: "+agentErr.Error())
	}
	in.agents = map[string]Agent{}
	for _, a := range agents {
		if a.Name != "" {
			in.agents[a.Name] = a
		}
	}
	in.branch = map[string]string{}
	for id, pr := range st.PRs {
		if pr.URL != "" {
			in.branch[id] = pr.URL
		}
	}

	in.prs = map[string]DigestPR{}
	var ask []string
	for _, u := range digestPRURLs(in.open, in.branch) {
		// A merged or closed PR is done changing; a reopened one is rare
		// enough to wait for the ticker.
		if pr, ok := prev.PRs[u]; ok && (pr.State == "MERGED" || pr.State == "CLOSED") {
			in.prs[u] = pr
			continue
		}
		ask = append(ask, u)
	}
	if len(ask) > 0 {
		got, queries, err := env.lookup(ask)
		in.asked, in.queries = len(ask), queries
		if err != nil {
			warnings = append(warnings, "GitHub: "+err.Error()+"; PR states are from the last digest")
			for _, u := range ask {
				if pr, ok := prev.PRs[u]; ok {
					in.prs[u] = pr
				}
			}
		}
		for u, pr := range got {
			in.prs[u] = pr
		}
	}
	return in, warnings, nil
}

// beadPRURLs are the PRs a bead may be delivered through, best guess first:
// the one on its branch, then the ones its notes link, latest first.
func beadPRURLs(b Bead, branch map[string]string) []string {
	var out []string
	if u := branch[b.ID]; u != "" {
		out = append(out, u)
	}
	noted := NotedPRs(b.Notes)
	for i := len(noted) - 1; i >= 0 && len(noted)-i <= notedPRCap; i-- {
		out = append(out, noted[i])
	}
	return dedupe(out)
}

// digestPRURLs are the PRs worth asking GitHub about: only an active bead's
// PR changes which section it is in.
func digestPRURLs(beads []Bead, branch map[string]string) []string {
	var out []string
	for _, b := range beads {
		if b.Status == StatusInProgress || b.Status == StatusNeedsMe || b.Status == StatusBlocked {
			out = append(out, beadPRURLs(b, branch)...)
		}
	}
	return dedupe(out)
}

// pickPR adapts the digest's light observations to the common selector.
func pickPR(urls []string, prs map[string]DigestPR) (*DigestPR, int) {
	var candidates []PR
	for _, u := range urls {
		if pr, ok := prs[u]; ok {
			candidates = append(candidates, pr.observation(PR{}))
		}
	}
	selected := selectPR(candidates)
	if selected.PR == nil {
		return nil, selected.Open
	}
	pr := prs[selected.PR.URL]
	return &pr, selected.Open
}

// digestObservation renders a full ticker observation in the digest, including
// when its separate GitHub lookup is unavailable.
func digestObservation(p PR) DigestPR {
	return digestObservationWith(p, nil)
}

func digestObservationWith(p PR, ignore []string) DigestPR {
	d := DigestPR{Number: p.Number, URL: p.URL, State: p.State, MergedAt: p.MergedAt,
		IsDraft: p.IsDraft, HeadSHA: p.HeadSHA, MergeState: p.MergeState, ReviewDecision: p.ReviewDecision}
	c := summarizeChecksWith(p.Checks, ignore)
	switch {
	case len(c.Failing) > 0:
		d.Checks = "FAILURE"
	case c.Pending > 0:
		d.Checks = "PENDING"
	case c.Passing > 0:
		d.Checks = "SUCCESS"
	}
	return d
}

// observation refreshes light fields without inventing merge eligibility.
// Detailed policy/review evidence is usable only for the same known head.
func (p DigestPR) observation(cached PR) PR {
	pr := PR{}
	if p.HeadSHA != "" && p.HeadSHA == cached.HeadSHA && p.URL == cached.URL {
		pr = cached
	}
	pr.Number, pr.URL, pr.State = p.Number, p.URL, p.State
	pr.IsDraft, pr.ReviewDecision, pr.MergedAt = p.IsDraft, p.ReviewDecision, p.MergedAt
	pr.HeadSHA, pr.MergeState = p.HeadSHA, p.MergeState
	pr.Checks = nil
	if p.Checks != "" {
		pr.Checks = []Check{{Name: "checks", State: p.Checks}}
	}
	return pr
}

// sectionFor is a coarser presentation of the same state as the sidebar.
func sectionFor(s ThreadState) Section {
	switch s.Kind {
	case ThreadWaiting:
		return SecNeedsYou
	case ThreadDelivering:
		return SecWaiting
	case ThreadMerged:
		if s.RollingOut {
			return SecRollingOut
		}
		return SecMerged
	case ThreadBlocked:
		return SecBlocked
	case ThreadQueued:
		if s.Next {
			return SecReady
		}
		return SecBacklog
	default:
		return SecInProgress
	}
}

// buildDigest sorts every open bead into its section and notes what changed
// since prev. It returns the state to remember for the next run.
func buildDigest(in digestInputs, prev digestState, prefixes []string, now time.Time) (Digest, digestState) {
	d := Digest{At: now, Sections: map[Section][]DigestItem{}, Total: len(in.open)}
	byID := map[string]Bead{}
	for _, b := range in.open {
		byID[b.ID] = b
	}
	children := openChildCounts(in.open)
	next := digestState{At: now, Beads: map[string]Section{}, PRs: map[string]DigestPR{}, Auto: map[string]int{}}
	prOwner := map[string]string{}
	for _, b := range in.open {
		var blockers []string
		for _, dep := range b.Dependencies {
			if blk, ok := byID[dep.DependsOn]; ok && dep.Type == "blocks" {
				blockers = append(blockers, fmt.Sprintf("%s (%s)", blk.ID, blk.Status))
			}
		}
		th := threadFor(in.cfg, in.ticker, b, in.agents)
		th.Blockers = blockers
		th.OpenChildren = children[b.ID]
		urls := beadPRURLs(b, in.branch)
		// References to another thread's selected branch are not delivery.
		urls = slices.DeleteFunc(urls, func(u string) bool {
			for id, branch := range in.branch {
				if id != b.ID && branch == u {
					return true
				}
			}
			return false
		})
		var pr *DigestPR
		var open int
		if b.Status != StatusOpen || th.Agent != nil && (th.Agent.Status == "working" || th.Agent.Status == "blocked") {
			pr, open = pickPR(urls, in.prs)
			for _, u := range urls {
				if p, ok := in.prs[u]; ok {
					next.PRs[u] = p
					if _, taken := prOwner[u]; !taken {
						prOwner[u] = b.ID
					}
				}
			}
		}
		if pr == nil && th.PR != nil && b.Status != StatusOpen {
			fallback := digestObservationWith(*th.PR, in.cfg.IgnoreChecks)
			pr = &fallback
			if fallback.State == "OPEN" {
				open = 1
			}
			if fallback.URL != "" {
				next.PRs[fallback.URL], prOwner[fallback.URL] = fallback, b.ID
			}
		}
		if pr != nil {
			cached := PR{}
			if th.PR != nil {
				cached = *th.PR
			}
			observation := pr.observation(cached)
			th.PR, th.Checks = &observation, summarizeChecks(observation.Checks)
		}
		th = th.derive()
		state := th.derived()
		s := sectionFor(state)
		next.Beads[b.ID] = s
		if n := len(autoNotes(b.Notes)); n > 0 {
			next.Auto[b.ID] = n
		}
		item := DigestItem{
			State: state, Bead: b, Section: s, PR: pr, OpenPRs: open, Blockers: blockers, Agent: state.Agent,
			Linear: linearKey(b, prefixes), Stale: state.stale(now),
		}
		if s == SecNeedsYou {
			if parked, kind, until := classifyNeedsYou(in.cfg, th, in.snoozes, now); parked {
				item.Parked = true
				item.parkedKind = kind
				item.SnoozedUntil = until
				item.ParkedReason = formatParkedReason(kind, until, b.UpdatedAt, now)
			}
		}
		d.Sections[s] = append(d.Sections[s], item)
	}
	for s, items := range d.Sections {
		sort.SliceStable(items, func(i, j int) bool {
			if s == SecNeedsYou && items[i].Parked != items[j].Parked {
				return !items[i].Parked
			}
			a, b := items[i].Bead, items[j].Bead
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if s == SecBacklog || s == SecReady {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.UpdatedAt.Before(b.UpdatedAt)
		})
	}
	d.Changes = digestChanges(prev, next, byID, prOwner, in.closed)
	d.Auto = autoChanges(prev, append(slices.Clone(in.open), in.closed...))
	d.Changes = append(d.Changes, autoLines(d.Auto)...)
	return d, next
}

// autoChanges are the AUTO: notes added since the last digest, so every step
// a worker took without TJ is in front of them once. Notes have no
// timestamps: what is new is what lies past the last count. The first digest
// has nothing to compare with, so it reports nothing.
func autoChanges(prev digestState, beads []Bead) []autoStep {
	if prev.At.IsZero() {
		return nil
	}
	var out []autoStep
	for _, b := range beads {
		notes := autoNotes(b.Notes)
		if seen := prev.Auto[b.ID]; seen < len(notes) {
			for _, n := range notes[seen:] {
				out = append(out, autoStep{Bead: b.ID, Note: n})
			}
		}
	}
	return out
}

// digestChanges is what moved since the last digest. The first digest has
// nothing to compare with, so it reports nothing.
func digestChanges(prev, next digestState, byID map[string]Bead, prOwner map[string]string, closed []Bead) []string {
	if prev.At.IsZero() {
		return nil
	}
	var out []string
	label := func(id string) string { return fmt.Sprintf("%s %s", id, shortTitle(byID[id].Title, 60)) }
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range next.Beads {
			if !yield(id) {
				return
			}
		}
	})
	for _, id := range ids {
		s, was := next.Beads[id], prev.Beads[id]
		_, seen := prev.Beads[id]
		switch {
		case s == SecNeedsYou && (!seen || was != SecNeedsYou):
			out = append(out, "needs you: "+label(id))
		case s == SecBlocked && (!seen || was != SecBlocked):
			out = append(out, "blocked: "+label(id))
		}
	}
	urls := slices.Sorted(func(yield func(string) bool) {
		for u := range next.PRs {
			if !yield(u) {
				return
			}
		}
	})
	for _, u := range urls {
		pr, ok := next.PRs[u]
		old, known := prev.PRs[u]
		if !ok || !known {
			continue
		}
		ref := pr.name() + " (" + prOwner[u] + ")"
		switch {
		case pr.State == "MERGED" && old.State != "MERGED":
			out = append(out, "merged: "+ref)
		case pr.State == "OPEN":
			if pr.Checks == "SUCCESS" && old.Checks != "SUCCESS" {
				out = append(out, "checks green: "+ref)
			}
			if pr.ReviewDecision == "APPROVED" && old.ReviewDecision != "APPROVED" {
				out = append(out, "approved: "+ref)
			}
		}
	}
	sort.SliceStable(closed, func(i, j int) bool { return closed[i].ClosedAt.Before(closed[j].ClosedAt) })
	for _, b := range closed {
		line := fmt.Sprintf("closed: %s %s", b.ID, shortTitle(b.Title, 60))
		if r := oneLine(b.CloseReason); r != "" {
			line += " — " + shortTitle(r, 80)
		}
		out = append(out, line)
	}
	return out
}

func (pr DigestPR) name() string { return prName(PR{Number: pr.Number, URL: pr.URL}) }

func checksWord(state string) string {
	switch state {
	case "SUCCESS":
		return "checks green"
	case "FAILURE", "ERROR":
		return "checks failing"
	case "PENDING", "EXPECTED":
		return "checks running"
	}
	return "no checks"
}

func reviewWord(decision string) string {
	switch decision {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes requested"
	case "REVIEW_REQUIRED":
		return "review required"
	}
	return "no review required"
}

// prLine is a PR's state in a few words.
func prLine(pr *DigestPR, open int) string {
	switch pr.State {
	case "MERGED":
		return pr.name() + " merged"
	case "CLOSED":
		return pr.name() + " closed"
	}
	parts := []string{pr.name()}
	if pr.IsDraft {
		parts = append(parts, "draft")
	}
	if pr.Mergeable == "CONFLICTING" {
		parts = append(parts, "conflicts")
	}
	parts = append(parts, checksWord(pr.Checks), reviewWord(pr.ReviewDecision))
	if open > 1 {
		parts = append(parts, fmt.Sprintf("+%d more open", open-1))
	}
	return strings.Join(parts, " · ")
}

// itemLine is a bead's first line: id, priority, title, assignee, age, Linear key.
func itemLine(it DigestItem, now time.Time) string {
	b := it.Bead
	parts := []string{fmt.Sprintf("**%s** P%d %s", b.ID, b.Priority, shortTitle(b.Title, 90))}
	if b.Assignee != "" {
		parts = append(parts, "@"+b.Assignee)
	}
	age := "updated " + ago(b.UpdatedAt, now)
	if it.Stale {
		age = "STALE since " + ago(b.UpdatedAt, now)
	}
	parts = append(parts, age)
	if it.Parked && it.ParkedReason != "" {
		parts = append(parts, it.ParkedReason)
	}
	if it.Linear != "" {
		parts = append(parts, it.Linear)
	}
	return strings.Join(parts, " · ")
}

// itemDetail is the second line: why the bead is where it is.
func itemDetail(it DigestItem) string {
	b := it.Bead
	var parts []string
	switch it.State.Kind {
	case ThreadWaiting:
		switch it.State.Waiting {
		case WaitPrompt:
			parts = append(parts, "agent stopped at a prompt")
		case WaitCommand:
			parts = append(parts, "RUN: "+shortTitle(it.State.Command, 200))
		default:
			parts = append(parts, needsMeSummaryNote(b.Notes, 240))
		}
	case ThreadBlocked:
		if len(it.Blockers) > 0 {
			parts = append(parts, "blocked by "+strings.Join(it.Blockers, ", "))
		}
		if b.Status == StatusBlocked {
			parts = append(parts, lastNote(b.Notes, 200))
		}
	case ThreadWorking, ThreadIdle, ThreadNoAgent, ThreadMerged:
		if it.Agent != "" {
			parts = append(parts, "agent "+it.Agent)
		} else {
			parts = append(parts, "no agent")
		}
		if it.State.RollingOut {
			parts = append(parts, "rolling out")
		}
	}
	if it.PR != nil {
		parts = append(parts, prLine(it.PR, it.OpenPRs))
	}
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func (d Digest) count(s Section) int { return len(d.Sections[s]) }

func (d Digest) needsYouSplit() (active, parked []DigestItem, snoozed, dormant int) {
	for _, it := range d.Sections[SecNeedsYou] {
		if it.Parked {
			parked = append(parked, it)
			if it.parkedKind == parkedReasonSnoozed {
				snoozed++
			} else {
				dormant++
			}
		} else {
			active = append(active, it)
		}
	}
	return active, parked, snoozed, dormant
}

func (d Digest) stale() int {
	n := 0
	for _, items := range d.Sections {
		for _, it := range items {
			if it.Stale {
				n++
			}
		}
	}
	return n
}

// headline is the digest in one line of counts.
func (d Digest) headline() string {
	active, parked, _, _ := d.needsYouSplit()
	needPart := fmt.Sprintf("%d need you", len(active))
	if len(parked) > 0 {
		needPart = fmt.Sprintf("%d need you (%d parked)", len(active), len(parked))
	}
	parts := []string{
		needPart,
		fmt.Sprintf("%d waiting", d.count(SecWaiting)),
		fmt.Sprintf("%d blocked", d.count(SecBlocked)),
		fmt.Sprintf("%d in progress", d.count(SecInProgress)),
		fmt.Sprintf("%d merged", d.count(SecMerged)),
		fmt.Sprintf("%d rolling out", d.count(SecRollingOut)),
		fmt.Sprintf("%d next", d.count(SecReady)),
		fmt.Sprintf("%d backlog", d.count(SecBacklog)),
	}
	if n := d.stale(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", n))
	}
	return strings.Join(parts, " · ")
}

// quiet is the line that tells a quiet digest from a broken one: whether
// anything needed TJ, and how much ran without them.
func (d Digest) quiet() string {
	active, parked, _, _ := d.needsYouSplit()
	s := "nothing needed you"
	switch {
	case len(active) > 0 && len(parked) > 0:
		s = fmt.Sprintf("%d need you (%d parked)", len(active), len(parked))
	case len(active) > 0:
		s = fmt.Sprintf("%d need you", len(active))
	case len(parked) > 0:
		s = fmt.Sprintf("nothing active needed you (%d parked)", len(parked))
	}
	if len(d.Auto) == 0 {
		return s + "; no steps without you"
	}
	s += fmt.Sprintf("; %d step(s) without you", len(d.Auto))
	if c := topClass(d.Auto); c >= 0 {
		s += fmt.Sprintf(", highest class %d", c)
	}
	return s
}

func renderDigest(d Digest) string {
	var s strings.Builder
	fmt.Fprintf(&s, "# Beads digest: %s\n\n%s (%d open)\n\n%s\n", d.At.Format("2006-01-02 15:04 MST"), d.headline(), d.Total, d.quiet())
	for _, w := range d.Warnings {
		fmt.Fprintf(&s, "\n> WARNING: %s\n", w)
	}
	fmt.Fprintf(&s, "\n## Changed since the last digest (%d)\n\n", len(d.Changes))
	if len(d.Changes) == 0 {
		s.WriteString("_Nothing._\n")
	}
	for _, c := range d.Changes {
		fmt.Fprintf(&s, "- %s\n", c)
	}
	writeItem := func(it DigestItem) {
		fmt.Fprintf(&s, "- %s\n", itemLine(it, d.At))
		if detail := itemDetail(it); detail != "" {
			fmt.Fprintf(&s, "  - %s\n", detail)
		}
	}
	for _, sec := range []Section{SecNeedsYou, SecWaiting, SecMerged, SecRollingOut, SecBlocked, SecInProgress, SecReady, SecBacklog} {
		items := d.Sections[sec]
		if sec == SecNeedsYou {
			active, parked, snoozed, dormant := d.needsYouSplit()
			if len(parked) > 0 {
				fmt.Fprintf(&s, "\n## Needs you — Active (%d)\n\n", len(active))
				if len(active) == 0 {
					s.WriteString("_None._\n")
				}
				for _, it := range active {
					writeItem(it)
				}
				fmt.Fprintf(&s, "\n### Parked / Dormant (%s)\n\n", formatParkedBreakdown(snoozed, dormant))
				for _, it := range parked {
					writeItem(it)
				}
				continue
			}
		}
		fmt.Fprintf(&s, "\n## %s (%d)\n\n", sec, len(items))
		if len(items) == 0 {
			s.WriteString("_None._\n")
			continue
		}
		shown := items
		if sec == SecBacklog && len(shown) > backlogTop {
			shown = shown[:backlogTop]
		}
		for _, it := range shown {
			writeItem(it)
		}
		if len(shown) < len(items) {
			fmt.Fprintf(&s, "- _and %d more; `bd list --status open` for all of them_\n", len(items)-len(shown))
		}
	}
	return s.String()
}

type digestSummaryJSON struct {
	Total          int    `json:"total"`
	Stale          int    `json:"stale"`
	NeedsYou       int    `json:"needs_you"`
	NeedsYouActive int    `json:"needs_you_active"`
	NeedsYouParked int    `json:"needs_you_parked"`
	Waiting        int    `json:"waiting"`
	Merged         int    `json:"merged"`
	RollingOut     int    `json:"rolling_out"`
	Blocked        int    `json:"blocked"`
	InProgress     int    `json:"in_progress"`
	Ready          int    `json:"ready"`
	Backlog        int    `json:"backlog"`
	Headline       string `json:"headline"`
	Quiet          string `json:"quiet"`
}

type digestItemJSON struct {
	Bead         string     `json:"bead"`
	Title        string     `json:"title"`
	Status       string     `json:"status"`
	Priority     int        `json:"priority"`
	Assignee     string     `json:"assignee,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at,omitzero"`
	Agent        string     `json:"agent,omitempty"`
	Linear       string     `json:"linear,omitempty"`
	Stale        bool       `json:"stale,omitempty"`
	Parked       bool       `json:"parked,omitempty"`
	ParkedReason string     `json:"parked_reason,omitempty"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	Blockers     []string   `json:"blockers,omitempty"`
	PR           *DigestPR  `json:"pr"`
	OpenPRs      int        `json:"open_prs,omitempty"`
}

type digestJSON struct {
	At             time.Time         `json:"at"`
	Summary        digestSummaryJSON `json:"summary"`
	Warnings       []string          `json:"warnings,omitempty"`
	Changes        []string          `json:"changes"`
	NeedsYou       []digestItemJSON  `json:"needs_you"`
	NeedsYouParked []digestItemJSON  `json:"needs_you_parked"`
	Waiting        []digestItemJSON  `json:"waiting"`
	Merged         []digestItemJSON  `json:"merged"`
	RollingOut     []digestItemJSON  `json:"rolling_out"`
	Blocked        []digestItemJSON  `json:"blocked"`
	InProgress     []digestItemJSON  `json:"in_progress"`
	Ready          []digestItemJSON  `json:"ready"`
	Backlog        []digestItemJSON  `json:"backlog"`
}

// renderDigestJSON encodes the digest sections as JSON, keyed by section name.
func renderDigestJSON(d Digest) ([]byte, error) {
	encodeItems := func(src []DigestItem) []digestItemJSON {
		out := make([]digestItemJSON, 0, len(src))
		for _, it := range src {
			var snoozed *time.Time
			if !it.SnoozedUntil.IsZero() {
				u := it.SnoozedUntil
				snoozed = &u
			}
			out = append(out, digestItemJSON{
				Bead:         it.Bead.ID,
				Title:        it.Bead.Title,
				Status:       it.Bead.Status,
				Priority:     it.Bead.Priority,
				Assignee:     it.Bead.Assignee,
				UpdatedAt:    it.Bead.UpdatedAt,
				Agent:        it.Agent,
				Linear:       it.Linear,
				Stale:        it.Stale,
				Parked:       it.Parked,
				ParkedReason: it.ParkedReason,
				SnoozedUntil: snoozed,
				Detail:       itemDetail(it),
				Blockers:     it.Blockers,
				PR:           it.PR,
				OpenPRs:      it.OpenPRs,
			})
		}
		return out
	}
	items := func(sec Section) []digestItemJSON {
		return encodeItems(d.Sections[sec])
	}
	activeNeeds, parkedNeeds, _, _ := d.needsYouSplit()
	changes := d.Changes
	if changes == nil {
		changes = []string{}
	}
	doc := digestJSON{
		At: d.At,
		Summary: digestSummaryJSON{
			Total:          d.Total,
			Stale:          d.stale(),
			NeedsYou:       d.count(SecNeedsYou),
			NeedsYouActive: len(activeNeeds),
			NeedsYouParked: len(parkedNeeds),
			Waiting:        d.count(SecWaiting),
			Merged:         d.count(SecMerged),
			RollingOut:     d.count(SecRollingOut),
			Blocked:        d.count(SecBlocked),
			InProgress:     d.count(SecInProgress),
			Ready:          d.count(SecReady),
			Backlog:        d.count(SecBacklog),
			Headline:       d.headline(),
			Quiet:          d.quiet(),
		},
		Warnings:       d.Warnings,
		Changes:        changes,
		NeedsYou:       encodeItems(activeNeeds),
		NeedsYouParked: encodeItems(parkedNeeds),
		Waiting:        items(SecWaiting),
		Merged:         items(SecMerged),
		RollingOut:     items(SecRollingOut),
		Blocked:        items(SecBlocked),
		InProgress:     items(SecInProgress),
		Ready:          items(SecReady),
		Backlog:        items(SecBacklog),
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// summary is the notification: the counts as its title, then what changed
// and the top of active needs-you (with a summary count for parked items).
// top caps both lists.
func (d Digest) summary(top int) (string, string) {
	title := "Beads: " + d.headline()
	body := []string{d.quiet()}
	for i, c := range d.Changes {
		if i == top {
			body = append(body, fmt.Sprintf("…and %d more changes", len(d.Changes)-top))
			break
		}
		body = append(body, c)
	}
	needs, parked, _, _ := d.needsYouSplit()
	if len(needs) > 0 {
		body = append(body, "", "Needs you:")
		for i, it := range needs {
			if i == top {
				body = append(body, fmt.Sprintf("…and %d more", len(needs)-top))
				break
			}
			line := fmt.Sprintf("%s P%d %s", it.Bead.ID, it.Bead.Priority, shortTitle(it.Bead.Title, 70))
			if it.Stale {
				line += " (stale " + ago(it.Bead.UpdatedAt, d.At) + ")"
			}
			body = append(body, line)
		}
	}
	if len(parked) > 0 {
		body = append(body, fmt.Sprintf("(%d parked / dormant)", len(parked)))
	}
	return title, strings.Join(body, "\n")
}

// worthNotifying is whether a digest should interrupt: something changed or
// an active (non-parked) thread is waiting on the user.
func (d Digest) worthNotifying() bool {
	active, _, _, _ := d.needsYouSplit()
	return len(d.Changes) > 0 || len(active) > 0
}

// DigestPRs looks up each PR's state, review decision and check rollup, keyed
// by URL, in one GraphQL query per digestBatch PRs, the queries in parallel.
// It also returns how many queries it made.
func (g GH) DigestPRs(urls []string) (map[string]DigestPR, int, error) {
	chunks := slices.Collect(slices.Chunk(urls, digestBatch))
	outs := make([][]byte, len(chunks))
	errs := make([]error, len(chunks))
	var wg sync.WaitGroup
	for i, chunk := range chunks {
		wg.Go(func() {
			q, _ := digestQuery(chunk)
			outs[i], errs[i] = g.graphql(q)
		})
	}
	wg.Wait()
	prs := map[string]DigestPR{}
	for i, chunk := range chunks {
		if errs[i] != nil {
			return prs, len(chunks), errs[i]
		}
		_, alias := digestQuery(chunk)
		if err := parseDigestPRs(outs[i], alias, prs); err != nil {
			return prs, len(chunks), err
		}
	}
	return prs, len(chunks), nil
}

// digestBatch is smaller than lookupBatch: GitHub answers a query in time
// roughly proportional to its PRs' check rollups, so smaller queries in
// parallel finish sooner for the same rate-limit points.
const digestBatch = 20

const digestPRFields = "number url state isDraft reviewDecision mergeable mergedAt headRefOid mergeStateStatus commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }"

func digestQuery(urls []string) (string, map[string]string) {
	var q strings.Builder
	alias := map[string]string{}
	for i, u := range urls {
		m := prURL.FindStringSubmatch(u)
		if m == nil {
			continue
		}
		owner, name, _ := strings.Cut(m[1], "/")
		a := fmt.Sprintf("p%d", i)
		alias[a] = u
		fmt.Fprintf(&q, "%s: repository(owner: %q, name: %q) { pullRequest(number: %s) { %s } } ", a, owner, name, m[2], digestPRFields)
	}
	return "{ " + q.String() + "}", alias
}

func parseDigestPRs(out []byte, alias map[string]string, prs map[string]DigestPR) error {
	type node struct {
		DigestPR
		Commits struct {
			Nodes []struct {
				Commit struct {
					Rollup *struct {
						State string `json:"state"`
					} `json:"statusCheckRollup"`
				} `json:"commit"`
			} `json:"nodes"`
		} `json:"commits"`
	}
	var resp struct {
		Data map[string]*struct {
			PullRequest *node `json:"pullRequest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return err
	}
	for a, u := range alias {
		r := resp.Data[a]
		if r == nil || r.PullRequest == nil {
			continue
		}
		pr := r.PullRequest.DigestPR
		if n := r.PullRequest.Commits.Nodes; len(n) > 0 && n[0].Commit.Rollup != nil {
			pr.Checks = n[0].Commit.Rollup.State
		}
		prs[u] = pr
	}
	return nil
}
