package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"
)

type PR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	Head           string    `json:"headRefName"`
	State          string    `json:"state"` // OPEN, MERGED, CLOSED
	IsDraft        bool      `json:"isDraft"`
	ReviewDecision string    `json:"reviewDecision"`
	HeadSHA        string    `json:"headRefOid"`
	MergeState     string    `json:"mergeStateStatus"`
	Checks         []Check   `json:"statusCheckRollup"`
	Reviews        []Review  `json:"reviews"`
	MergedAt       time.Time `json:"mergedAt"`
	// AutoMerge is set while auto-merge is on.
	AutoMerge *AutoMerge `json:"autoMergeRequest"`
	// Gate is what readyToMerge needs beyond gh pr view, fetched only for PRs
	// that are otherwise mergeable.
	Gate *MergeGate `json:"gate,omitempty"`
}

// AutoMerge is a PR's pending auto-merge request.
type AutoMerge struct {
	Method string `json:"mergeMethod"`
}

// MergeGate is a PR's review state at one head: what GitHub's reviewDecision
// hides, since an approval stays APPROVED across later pushes unless the repo
// dismisses stale reviews.
type MergeGate struct {
	Head string `json:"head"`
	// OpenThreads counts unresolved review threads not outdated by a push.
	OpenThreads int `json:"open_threads"`
	// MoreThreads marks a PR with threads past the first page, which were not
	// counted.
	MoreThreads bool `json:"more_threads,omitempty"`
	// At is the ticker pass that last applied the gate, which approvals are
	// aged against.
	At time.Time `json:"at"`
}

// approvalMaxAge is how old the approval of a head may be and still let it
// merge.
const approvalMaxAge = 7 * 24 * time.Hour

type Check struct {
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

type Review struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt"`
	Commit      struct {
		OID string `json:"oid"`
	} `json:"commit"`
	// Bot is set from the REST API: gh pr view drops the [bot] suffix.
	Bot bool `json:"bot,omitempty"`
}

func (r Review) isBot() bool { return r.Bot || strings.HasSuffix(r.Author.Login, "[bot]") }

// listFields stay light: statusCheckRollup across 100 PRs in a repo with many
// checks per PR makes GitHub's GraphQL time out (504).
const listFields = "number,title,url,headRefName,state,isDraft,mergedAt"
const detailFields = listFields + ",reviewDecision,statusCheckRollup,reviews,headRefOid,mergeStateStatus,autoMergeRequest"

type Checks struct {
	Failing []string
	Pending int
	Passing int
}

func (c Check) label() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

func summarizeChecks(checks []Check) Checks {
	var s Checks
	seen := map[string]bool{}
	for _, c := range checks {
		result := strings.ToUpper(c.Conclusion)
		if result == "" {
			result = strings.ToUpper(c.State)
		}
		switch {
		case strings.ToUpper(c.Status) != "" && strings.ToUpper(c.Status) != "COMPLETED":
			s.Pending++
		case result == "PENDING" || result == "EXPECTED":
			s.Pending++
		case result == "FAILURE" || result == "ERROR" || result == "TIMED_OUT" || result == "STARTUP_FAILURE" || result == "ACTION_REQUIRED":
			if !seen[c.label()] {
				seen[c.label()] = true
				s.Failing = append(s.Failing, c.label())
			}
		default:
			s.Passing++
		}
	}
	sort.Strings(s.Failing)
	return s
}

// readyToMerge reports an open PR that only needs merging: approved or
// needing no review, and GitHub's merge state says branch protection is met.
// CLEAN and HAS_HOOKS mean every check passed; UNSTABLE means only checks
// that aren't required failed, since a required one failing or pending makes
// it BLOCKED. Pending checks still count against it, so a repo without
// required checks doesn't look ready the moment its PR opens.
func readyToMerge(pr *PR, c Checks) bool {
	return mergeable(pr, c) && pr.Gate != nil && pr.Gate.Head == pr.HeadSHA && len(mergeBlockers(pr)) == 0
}

// mergeable is readyToMerge before the review gate: everything gh pr view
// can tell.
func mergeable(pr *PR, c Checks) bool {
	if pr == nil || pr.State != "OPEN" || pr.IsDraft || c.Pending > 0 {
		return false
	}
	if pr.ReviewDecision != "APPROVED" && pr.ReviewDecision != "" {
		return false
	}
	switch pr.MergeState {
	case "CLEAN", "UNSTABLE", "HAS_HOOKS":
		return true
	}
	return false
}

// mergeBlockers is what keeps a mergeable PR from being ready: review threads
// still open or uncounted, or no approval of the head within approvalMaxAge.
// A repo that requires no review needs no approval.
func mergeBlockers(pr *PR) []string {
	g := pr.Gate
	if g == nil {
		return nil
	}
	var out []string
	if g.OpenThreads > 0 {
		out = append(out, fmt.Sprintf("%d open %s", g.OpenThreads, plural(g.OpenThreads, "thread")))
	}
	if g.MoreThreads {
		out = append(out, "over 100 threads")
	}
	if pr.ReviewDecision == "APPROVED" && !freshApproval(pr.Reviews, g) {
		out = append(out, "stale approval")
	}
	return out
}

func freshApproval(reviews []Review, g *MergeGate) bool {
	for _, r := range reviews {
		if r.State == "APPROVED" && r.Commit.OID == g.Head && g.At.Sub(r.SubmittedAt) <= approvalMaxAge {
			return true
		}
	}
	return false
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

// reviewsBy counts reviews not written by login, so a thread's own replies
// never look like new feedback.
func reviewsNotBy(reviews []Review, login string) int {
	n := 0
	for _, r := range reviews {
		if r.Author.Login != login && !r.isBot() {
			n++
		}
	}
	return n
}

func botReviews(reviews []Review) []string {
	var out []string
	for _, r := range reviews {
		if r.isBot() {
			out = append(out, r.Author.Login)
		}
	}
	return out
}

type GH struct {
	repo string
}

func (g GH) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = g.repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("gh %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// graphql runs a query and keeps a partial answer: GitHub answers every alias
// it can and errors the rest, such as a PR in a repository since deleted, and
// gh exits 1 either way. Only an answer without data, such as a rate limit,
// is an error.
func (g GH) graphql(q string) ([]byte, error) {
	out, err := g.run("api", "graphql", "-f", "query="+q)
	if err == nil {
		return out, nil
	}
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(out, &resp) == nil && len(resp.Data) > 0 {
		return out, nil
	}
	return nil, err
}

func (g GH) Login() string {
	out, err := g.run("api", "user", "--jq", ".login")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Mine lists the user's recent PRs in the repo, newest first.
func (g GH) Mine() ([]PR, error) {
	out, err := g.run("pr", "list", "--author", "@me", "--state", "all", "--limit", "100", "--json", listFields)
	if err != nil {
		return nil, err
	}
	var prs []PR
	return prs, json.Unmarshal(out, &prs)
}

func (g GH) View(url string, detail bool) (PR, error) {
	fields := listFields
	if detail {
		fields = detailFields
	}
	out, err := g.run("pr", "view", url, "--json", fields)
	if err != nil {
		return PR{}, err
	}
	var pr PR
	return pr, json.Unmarshal(out, &pr)
}

// MarkBotReviews flags pr's bot reviews, which only the REST API marks.
func (g GH) MarkBotReviews(pr *PR) error {
	m := prURL.FindStringSubmatch(pr.URL)
	if len(pr.Reviews) == 0 || m == nil {
		return nil
	}
	out, err := g.run("api", "--paginate", fmt.Sprintf("repos/%s/pulls/%s/reviews", m[1], m[2]), "--jq", `.[] | select(.user.type == "Bot") | .user.login`)
	if err != nil {
		return err
	}
	bots := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		bots[strings.TrimSuffix(l, "[bot]")] = true
	}
	for i := range pr.Reviews {
		pr.Reviews[i].Bot = bots[pr.Reviews[i].Author.Login]
	}
	return nil
}

// Gates fetches the merge gate of every PR in one GraphQL query, keyed by
// URL. A PR missing from the result has no gate and so is not ready.
func (g GH) Gates(prs []PR, now time.Time) (map[string]MergeGate, error) {
	var q strings.Builder
	alias := map[string]string{}
	for i, pr := range prs {
		m := prURL.FindStringSubmatch(pr.URL)
		if m == nil {
			continue
		}
		owner, name, _ := strings.Cut(m[1], "/")
		a := fmt.Sprintf("p%d", i)
		alias[a] = pr.URL
		fmt.Fprintf(&q, `%s: repository(owner: %q, name: %q) { pullRequest(number: %s) { headRefOid reviewThreads(first: 100) { pageInfo { hasNextPage } nodes { isResolved isOutdated } } } } `, a, owner, name, m[2])
	}
	if len(alias) == 0 {
		return nil, nil
	}
	out, err := g.graphql("{ " + q.String() + "}")
	if err != nil {
		return nil, err
	}
	return parseGates(out, alias, now)
}

// lookupBatch caps the PRs one Lookup query asks for.
const lookupBatch = 50

// Lookup fetches the list fields of each PR, keyed by URL, in one GraphQL
// query per lookupBatch PRs, where a gh pr view each would cost a point each.
// A PR GitHub can't resolve is missing from the result.
func (g GH) Lookup(urls []string) (map[string]PR, error) {
	prs := map[string]PR{}
	for chunk := range slices.Chunk(urls, lookupBatch) {
		var q strings.Builder
		alias := map[string]string{}
		for i, u := range chunk {
			m := prURL.FindStringSubmatch(u)
			if m == nil {
				continue
			}
			owner, name, _ := strings.Cut(m[1], "/")
			a := fmt.Sprintf("p%d", i)
			alias[a] = u
			fmt.Fprintf(&q, "%s: repository(owner: %q, name: %q) { pullRequest(number: %s) { %s } } ", a, owner, name, m[2], lookupFields)
		}
		if len(alias) == 0 {
			continue
		}
		out, err := g.graphql("{ " + q.String() + "}")
		if err != nil {
			return prs, err
		}
		if err := parseLookup(out, alias, prs); err != nil {
			return prs, err
		}
	}
	return prs, nil
}

// lookupFields are listFields in GraphQL.
const lookupFields = "number title url headRefName state isDraft mergedAt"

func parseLookup(out []byte, alias map[string]string, prs map[string]PR) error {
	var resp struct {
		Data map[string]*struct {
			PullRequest *PR `json:"pullRequest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return err
	}
	for a, u := range alias {
		if r := resp.Data[a]; r != nil && r.PullRequest != nil {
			prs[u] = *r.PullRequest
		}
	}
	return nil
}

func parseGates(out []byte, alias map[string]string, now time.Time) (map[string]MergeGate, error) {
	var resp struct {
		Data map[string]*struct {
			PullRequest *struct {
				HeadRefOid    string `json:"headRefOid"`
				ReviewThreads struct {
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						IsResolved bool `json:"isResolved"`
						IsOutdated bool `json:"isOutdated"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	gates := map[string]MergeGate{}
	for a, url := range alias {
		r := resp.Data[a]
		if r == nil || r.PullRequest == nil {
			continue
		}
		pr := r.PullRequest
		gate := MergeGate{Head: pr.HeadRefOid, MoreThreads: pr.ReviewThreads.PageInfo.HasNextPage, At: now}
		for _, t := range pr.ReviewThreads.Nodes {
			if !t.IsResolved && !t.IsOutdated {
				gate.OpenThreads++
			}
		}
		gates[url] = gate
	}
	return gates, nil
}

// prForBead picks the PR a bead is being delivered through, from the PRs on its
// own branch and the PRs its notes link to, and returns the merged ones among
// them. A bead worked on a branch named for its Linear key (tj/eng-1102-…) is
// found through its notes, but a noted PR that owner gives to another bead is
// that bead's. An open PR wins, own branch first, then the latest noted; with
// none open, the last to merge, so a bead with several PRs settles on one pick
// instead of flipping between them and is only told to close once none is
// open.
func prForBead(b Bead, branchPrefix string, mine []PR, byURL map[string]PR, owner func(PR) string) (PR, []PR, bool) {
	var cands []PR
	seen := map[string]bool{}
	add := func(pr PR) {
		if pr.URL == "" || !seen[pr.URL] {
			seen[pr.URL] = true
			cands = append(cands, pr)
		}
	}
	for _, pr := range mine {
		if onBeadBranch(branchPrefix, pr.Head, b) {
			add(pr)
		}
	}
	urls := NotedPRs(b.Notes)
	for i := len(urls) - 1; i >= 0; i-- {
		pr, ok := byURL[urls[i]]
		if !ok {
			continue
		}
		if owner != nil {
			if id := owner(pr); id != "" && id != b.ID {
				continue
			}
		}
		add(pr)
	}
	s := selectPR(cands)
	if s.PR == nil {
		return PR{}, s.Merged, false
	}
	return *s.PR, s.Merged, true
}

// prRank reports whether a outranks b as a bead's PR: open beats merged beats
// closed, and between merged PRs the later merge wins. Ties keep b, the
// earlier candidate.
func prRank(a, b PR) bool {
	rank := func(pr PR) int {
		switch pr.State {
		case "OPEN":
			return 2
		case "MERGED":
			return 1
		}
		return 0
	}
	if rank(a) != rank(b) {
		return rank(a) > rank(b)
	}
	return a.State == "MERGED" && a.MergedAt.After(b.MergedAt)
}
