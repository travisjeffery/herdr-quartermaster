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
)

var sectionNames = map[Section]string{
	SecNeedsYou:   "Needs you",
	SecWaiting:    "Waiting on review/merge",
	SecBlocked:    "Blocked",
	SecInProgress: "In progress",
	SecReady:      "Ready / next",
	SecBacklog:    "Backlog",
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
	Mergeable      string    `json:"mergeable,omitempty"` // MERGEABLE, CONFLICTING or UNKNOWN
	MergedAt       time.Time `json:"mergedAt,omitzero"`
	Checks         string    `json:"checks,omitempty"` // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED or ""
}

// DigestItem is one bead's line in the digest.
type DigestItem struct {
	Bead     Bead
	Section  Section
	PR       *DigestPR
	OpenPRs  int
	Blockers []string // non-closed beads blocking this one, "id (status)"
	Agent    string   // its herdr agent's status, "" without one
	Linear   string
	Stale    bool
}

type Digest struct {
	At       time.Time
	Sections map[Section][]DigestItem
	Changes  []string
	Warnings []string
	Total    int
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
	open   []Bead // every bead that isn't closed
	closed []Bead // beads closed since the last digest
	agents map[string]Agent
	prs    map[string]DigestPR // by URL
	branch map[string]string   // bead id → the PR URL the ticker found on its branch

	asked, queries int // PRs looked up on GitHub, and the GraphQL queries that took
}

// digestEnv is the outside world, so a test can build a digest from fixtures.
type digestEnv struct {
	open   func() ([]Bead, error)
	closed func(since time.Time) ([]Bead, error)
	agents func() ([]Agent, error)
	ticker func() TickerState
	lookup func(urls []string) (map[string]DigestPR, int, error)
}

func liveDigestEnv(cfg Config) digestEnv {
	return digestEnv{
		open: func() ([]Bead, error) { return Beads{}.list() },
		closed: func(since time.Time) ([]Bead, error) {
			return Beads{}.list("--status", StatusClosed, "--closed-after", since.UTC().Format(time.RFC3339))
		},
		agents: newHerdr().on(cfg.coordSocket()).Agents,
		ticker: loadState,
		lookup: GH{repo: cfg.Repo}.DigestPRs,
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

// pickPR is the PR a bead's line shows: its first open one, else the last to
// merge. It also counts the open ones.
func pickPR(urls []string, prs map[string]DigestPR) (*DigestPR, int) {
	var best *DigestPR
	open := 0
	for _, u := range urls {
		pr, ok := prs[u]
		if !ok {
			continue
		}
		switch {
		case pr.State == "OPEN":
			open++
			if best == nil || best.State != "OPEN" {
				best = &pr
			}
		case pr.State == "MERGED" && (best == nil || best.State == "MERGED" && pr.MergedAt.After(best.MergedAt)):
			best = &pr
		}
	}
	return best, open
}

// sectionFor places a bead. A question for the user outranks everything, an
// explicit blocked status outranks a PR, and an open PR outranks a blocking
// dependency, since the PR is what is moving.
func sectionFor(b Bead, pr *DigestPR, blockers []string, agent string) Section {
	switch {
	case b.Status == StatusNeedsMe && agent != "working", b.Status == StatusInProgress && agent == "blocked":
		return SecNeedsYou
	case b.Status == StatusBlocked:
		return SecBlocked
	case b.Status == StatusInProgress && pr != nil && pr.State == "OPEN":
		return SecWaiting
	case len(blockers) > 0:
		return SecBlocked
	case b.Status == StatusInProgress, b.Status == StatusNeedsMe:
		return SecInProgress
	case b.HasLabel(LabelNext):
		return SecReady
	default:
		return SecBacklog
	}
}

func staleFor(s Section, b Bead, now time.Time) bool {
	age := now.Sub(b.UpdatedAt)
	switch s {
	case SecNeedsYou:
		return age > staleNeedsYou
	case SecWaiting, SecInProgress:
		return age > staleInProgress
	case SecBlocked:
		return age > staleBlocked
	}
	return false
}

// buildDigest sorts every open bead into its section and notes what changed
// since prev. It returns the state to remember for the next run.
func buildDigest(in digestInputs, prev digestState, prefixes []string, now time.Time) (Digest, digestState) {
	d := Digest{At: now, Sections: map[Section][]DigestItem{}, Total: len(in.open)}
	byID := map[string]Bead{}
	for _, b := range in.open {
		byID[b.ID] = b
	}
	next := digestState{At: now, Beads: map[string]Section{}, PRs: map[string]DigestPR{}, Auto: map[string]int{}}
	prOwner := map[string]string{}
	for _, b := range in.open {
		var blockers []string
		for _, dep := range b.Dependencies {
			if blk, ok := byID[dep.DependsOn]; ok && dep.Type == "blocks" {
				blockers = append(blockers, fmt.Sprintf("%s (%s)", blk.ID, blk.Status))
			}
		}
		var agent string
		if a, ok := in.agents[agentName(b.ID)]; ok {
			agent = a.Status
		}
		urls := beadPRURLs(b, in.branch)
		var pr *DigestPR
		var open int
		if b.Status != StatusOpen {
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
		s := sectionFor(b, pr, blockers, agent)
		next.Beads[b.ID] = s
		if n := len(autoNotes(b.Notes)); n > 0 {
			next.Auto[b.ID] = n
		}
		d.Sections[s] = append(d.Sections[s], DigestItem{
			Bead: b, Section: s, PR: pr, OpenPRs: open, Blockers: blockers, Agent: agent,
			Linear: linearKey(b, prefixes), Stale: staleFor(s, b, now),
		})
	}
	for s, items := range d.Sections {
		sort.SliceStable(items, func(i, j int) bool {
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
	d.Changes = append(d.Changes, autoChanges(prev, append(slices.Clone(in.open), in.closed...))...)
	return d, next
}

// autoChanges are the AUTO: notes added since the last digest, one change
// each, so every step a worker took without TJ is in front of them once.
// Notes have no timestamps: what is new is what lies past the last count. The
// first digest has nothing to compare with, so it reports nothing.
func autoChanges(prev digestState, beads []Bead) []string {
	if prev.At.IsZero() {
		return nil
	}
	var out []string
	for _, b := range beads {
		notes := autoNotes(b.Notes)
		if seen := prev.Auto[b.ID]; seen < len(notes) {
			for _, n := range notes[seen:] {
				out = append(out, b.ID+" "+n)
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
	if it.Linear != "" {
		parts = append(parts, it.Linear)
	}
	return strings.Join(parts, " · ")
}

// itemDetail is the second line: why the bead is where it is.
func itemDetail(it DigestItem) string {
	b := it.Bead
	var parts []string
	switch it.Section {
	case SecNeedsYou:
		switch {
		case b.Status == StatusInProgress:
			parts = append(parts, "agent stopped at a prompt")
		case pendingCommand(b.Notes) != "":
			parts = append(parts, "RUN: "+shortTitle(pendingCommand(b.Notes), 200))
		default:
			parts = append(parts, lastNote(b.Notes, 240))
		}
	case SecBlocked:
		if len(it.Blockers) > 0 {
			parts = append(parts, "blocked by "+strings.Join(it.Blockers, ", "))
		}
		if b.Status == StatusBlocked {
			parts = append(parts, lastNote(b.Notes, 200))
		}
	case SecInProgress:
		if it.Agent != "" {
			parts = append(parts, "agent "+it.Agent)
		} else {
			parts = append(parts, "no agent")
		}
		if b.HasLabel(LabelRollingOut) {
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
	parts := []string{
		fmt.Sprintf("%d need you", d.count(SecNeedsYou)),
		fmt.Sprintf("%d waiting", d.count(SecWaiting)),
		fmt.Sprintf("%d blocked", d.count(SecBlocked)),
		fmt.Sprintf("%d in progress", d.count(SecInProgress)),
		fmt.Sprintf("%d next", d.count(SecReady)),
		fmt.Sprintf("%d backlog", d.count(SecBacklog)),
	}
	if n := d.stale(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", n))
	}
	return strings.Join(parts, " · ")
}

func renderDigest(d Digest) string {
	var s strings.Builder
	fmt.Fprintf(&s, "# Beads digest: %s\n\n%s (%d open)\n", d.At.Format("2006-01-02 15:04 MST"), d.headline(), d.Total)
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
	for _, sec := range []Section{SecNeedsYou, SecWaiting, SecBlocked, SecInProgress, SecReady, SecBacklog} {
		items := d.Sections[sec]
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
			fmt.Fprintf(&s, "- %s\n", itemLine(it, d.At))
			if detail := itemDetail(it); detail != "" {
				fmt.Fprintf(&s, "  - %s\n", detail)
			}
		}
		if len(shown) < len(items) {
			fmt.Fprintf(&s, "- _and %d more; `bd list --status open` for all of them_\n", len(items)-len(shown))
		}
	}
	return s.String()
}

// summary is the notification: the counts as its title, then what changed
// and the top of needs-you. top caps both lists.
func (d Digest) summary(top int) (string, string) {
	title := "Beads: " + d.headline()
	var body []string
	for i, c := range d.Changes {
		if i == top {
			body = append(body, fmt.Sprintf("…and %d more changes", len(d.Changes)-top))
			break
		}
		body = append(body, c)
	}
	needs := d.Sections[SecNeedsYou]
	if len(needs) > 0 {
		if len(body) > 0 {
			body = append(body, "")
		}
		body = append(body, "Needs you:")
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
	return title, strings.Join(body, "\n")
}

// worthNotifying is whether a digest should interrupt: something changed or
// something is waiting on the user.
func (d Digest) worthNotifying() bool {
	return len(d.Changes) > 0 || d.count(SecNeedsYou) > 0
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

const digestPRFields = "number url state isDraft reviewDecision mergeable mergedAt commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }"

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
