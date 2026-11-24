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
	Mergeable      string    `json:"mergeable,omitempty"` // MERGEABLE, CONFLICTING or UNKNOWN
	Checks         []Check   `json:"statusCheckRollup"`
	Reviews        []Review  `json:"reviews"`
	MergedAt       time.Time `json:"mergedAt"`
	// AutoMerge is set while auto-merge is on.
	AutoMerge *AutoMerge `json:"autoMergeRequest"`
	// Gate is what readyToMerge needs beyond gh pr view, fetched only for PRs
	// that are otherwise mergeable.
	Gate *MergeGate `json:"gate,omitempty"`
	// Policy is local task authorization, never GitHub data or cached consent.
	Policy       *MergePolicy `json:"-"`
	DetailsFresh bool         `json:"details_fresh,omitempty"` // last detail attempt succeeded; bounded by Gate.At in threadFor
}

// AutoMerge is a PR's pending auto-merge request.
type AutoMerge struct {
	Method string `json:"mergeMethod"`
}

// MergeGate is a PR's review state at one head: what GitHub's reviewDecision
// hides, since an approval stays APPROVED across later pushes unless the repo
// dismisses stale reviews.
type MergeGate struct {
	Head          string   `json:"head"`
	Viewer        string   `json:"viewer"`
	Author        string   `json:"author"`
	Reviews       []Review `json:"reviews"`
	MoreReviews   bool     `json:"more_reviews,omitempty"`
	ReviewProblem string   `json:"review_problem,omitempty"`
	// OpenThreads counts ALL unresolved threads, including outdated ones.
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
	// Fetched by Gates, not inferred from login or from absent protection.
	CanPush bool `json:"authorCanPushToRepository"`
}

func (r Review) isBot() bool { return r.Bot || strings.HasSuffix(r.Author.Login, "[bot]") }

// listFields stay light: statusCheckRollup across 100 PRs in a repo with many
// checks per PR makes GitHub's GraphQL time out (504).
const listFields = "number,title,url,headRefName,state,isDraft,mergedAt"
const detailFields = listFields + ",reviewDecision,statusCheckRollup,reviews,headRefOid,mergeStateStatus,mergeable,autoMergeRequest"

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

// readyToMerge requires GitHub eligibility AND independently established
// task authorization, ownership and current-head review evidence. Neither an
// empty reviewDecision nor an unprotected repository conveys consent.
func readyToMerge(pr *PR, c Checks) bool {
	return mergeable(pr, c) && len(mergeBlockers(pr)) == 0
}

// requiresGitHubReview reports whether GitHub enforces an approving review on
// pr's base branch. An empty reviewDecision means GitHub requires no approving
// review, so arming native --auto could merge immediately without approval.
func requiresGitHubReview(pr *PR) bool {
	if pr == nil {
		return false
	}
	switch pr.ReviewDecision {
	case "REVIEW_REQUIRED", "APPROVED", "CHANGES_REQUESTED":
		return true
	}
	return false
}

// mergeable reports only GitHub's requirements, not permission to act.
// UNSTABLE allows optional failures; pending checks still block even when no
// checks are required by protection.
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

// reviewMergeable reports whether pr is mergeable (not conflicting, draft, or
// unknown) for review readiness. Requires an explicit non-conflicting signal
// from either Mergeable or MergeState.
func reviewMergeable(pr *PR) bool {
	if pr == nil {
		return false
	}
	switch pr.Mergeable {
	case "CONFLICTING", "UNKNOWN":
		return false
	}
	switch pr.MergeState {
	case "DIRTY", "UNKNOWN", "DRAFT":
		return false
	}
	if pr.Mergeable == "MERGEABLE" {
		return true
	}
	switch pr.MergeState {
	case "CLEAN", "BLOCKED", "BEHIND", "UNSTABLE", "HAS_HOOKS":
		return true
	}
	return false
}

// reviewGateCandidate reports whether pr satisfies all pre-gate conditions for
// ready_for_review, so gatePRs fetches its current-head review threads.
func reviewGateCandidate(pr *PR, c Checks) bool {
	if pr == nil || pr.State != "OPEN" || pr.IsDraft || pr.HeadSHA == "" {
		return false
	}
	if pr.ReviewDecision != "REVIEW_REQUIRED" {
		return false
	}
	if c.Passing == 0 || c.Pending > 0 || len(c.Failing) > 0 {
		return false
	}
	return reviewMergeable(pr)
}

// readyForReview reports whether pr is an open, non-draft, green, mergeable,
// REVIEW_REQUIRED PR with fresh current-head gate evidence confirming zero
// unresolved review threads.
func readyForReview(pr *PR, c Checks) bool {
	if !reviewGateCandidate(pr, c) {
		return false
	}
	if !pr.DetailsFresh {
		return false
	}
	g := pr.Gate
	if g == nil || g.Head == "" || g.Head != pr.HeadSHA {
		return false
	}
	if g.OpenThreads > 0 || g.MoreThreads {
		return false
	}
	return true
}

// mergeBlockers are policy prerequisites beyond GitHub's mergeability.
func mergeBlockers(pr *PR) []string {
	if pr == nil {
		return []string{"no PR evidence"}
	}
	var out []string
	if !pr.DetailsFresh {
		out = append(out, "current PR details unavailable")
	}
	if pr.Policy == nil || !pr.Policy.Allowed {
		why := "task merge authorization unavailable"
		if pr.Policy != nil && pr.Policy.Reason != "" {
			why = pr.Policy.Reason
		}
		out = append(out, why)
	}
	g := pr.Gate
	if g == nil {
		return append(out, "current-head review evidence unavailable")
	}
	if g.Head == "" || g.Head != pr.HeadSHA || g.At.IsZero() {
		return append(out, "review evidence is not for the current head")
	}
	if g.Author == "" || g.Viewer == "" || !strings.EqualFold(g.Author, g.Viewer) {
		out = append(out, "PR is not owned by the authenticated user")
	}
	if g.OpenThreads > 0 {
		out = append(out, fmt.Sprintf("%d open %s", g.OpenThreads, plural(g.OpenThreads, "thread")))
	}
	if g.MoreThreads {
		out = append(out, "over 100 threads")
	}
	if g.MoreReviews {
		out = append(out, "incomplete review history")
	}
	if g.ReviewProblem != "" {
		out = append(out, g.ReviewProblem)
	}
	if !freshApproval(g.Reviews, g) {
		out = append(out, "missing or stale current-head approval")
	}
	for _, r := range latestOpinions(g.Reviews) {
		if r.State == "CHANGES_REQUESTED" {
			out = append(out, "changes requested by "+r.Author.Login)
		}
	}
	return out
}

// A later dismissal or change request supersedes that reviewer's approval;
// a comment does not. Never count a self-review, missing identity or date.
func latestOpinions(reviews []Review) []Review {
	latest := map[string]Review{}
	for _, r := range reviews {
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
			continue
		}
		key := strings.ToLower(r.Author.Login)
		if old, ok := latest[key]; !ok || !r.SubmittedAt.Before(old.SubmittedAt) {
			latest[key] = r
		}
	}
	var out []Review
	for _, key := range sortedKeys(latest) {
		out = append(out, latest[key])
	}
	return out
}

func freshApproval(reviews []Review, g *MergeGate) bool {
	for _, r := range latestOpinions(reviews) {
		age := g.At.Sub(r.SubmittedAt)
		if r.State == "APPROVED" && r.CanPush && r.Author.Login != "" && !strings.EqualFold(r.Author.Login, g.Author) &&
			r.Commit.OID == g.Head && !r.SubmittedAt.IsZero() && age >= 0 && age <= approvalMaxAge {
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
	q, alias := gatesQuery(prs)
	if len(alias) == 0 {
		return nil, nil
	}
	out, err := g.graphql(q)
	if err != nil {
		return nil, err
	}
	return parseGates(out, alias, now)
}

func gatesQuery(prs []PR) (string, map[string]string) {
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
		fmt.Fprintf(&q, `%s: repository(owner: %q, name: %q) { pullRequest(number: %s) { headRefOid author { login } reviews(first: 100) { pageInfo { hasNextPage } nodes { author { login } authorCanPushToRepository state submittedAt commit { oid } } } reviewThreads(first: 100) { pageInfo { hasNextPage } nodes { isResolved } } } } `, a, owner, name, m[2])
	}
	return "{ viewer { login } " + q.String() + "}", alias
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
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Path []any `json:"path"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	var viewer struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(resp.Data["viewer"], &viewer); err != nil || viewer.Login == "" {
		return nil, fmt.Errorf("merge gate: missing authenticated viewer")
	}
	bad := map[string]bool{}
	for _, e := range resp.Errors {
		if len(e.Path) == 0 || e.Path[0] == "viewer" {
			return nil, fmt.Errorf("merge gate: GraphQL errors")
		}
		if a, ok := e.Path[0].(string); ok {
			bad[a] = true
		}
	}
	type pageInfo struct {
		HasNextPage *bool `json:"hasNextPage"`
	}
	gates := map[string]MergeGate{}
	for a, url := range alias {
		if bad[a] || len(resp.Data[a]) == 0 {
			continue
		}
		var r struct {
			PullRequest *struct {
				Head   string `json:"headRefOid"`
				Author struct {
					Login string `json:"login"`
				} `json:"author"`
				Reviews *struct {
					PageInfo *pageInfo `json:"pageInfo"`
					Nodes    *[]Review `json:"nodes"`
				} `json:"reviews"`
				Threads *struct {
					PageInfo *pageInfo `json:"pageInfo"`
					Nodes    *[]*struct {
						Resolved *bool `json:"isResolved"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		}
		if err := json.Unmarshal(resp.Data[a], &r); err != nil {
			return nil, err
		}
		p := r.PullRequest
		if p == nil || p.Head == "" || p.Author.Login == "" || p.Reviews == nil || p.Threads == nil ||
			p.Reviews.PageInfo == nil || p.Reviews.PageInfo.HasNextPage == nil || p.Reviews.Nodes == nil ||
			p.Threads.PageInfo == nil || p.Threads.PageInfo.HasNextPage == nil || p.Threads.Nodes == nil {
			continue
		}
		gate := MergeGate{Head: p.Head, Author: p.Author.Login, Viewer: viewer.Login, Reviews: *p.Reviews.Nodes,
			MoreReviews: *p.Reviews.PageInfo.HasNextPage, MoreThreads: *p.Threads.PageInfo.HasNextPage, At: now}
		complete := true
		for _, t := range *p.Threads.Nodes {
			if t == nil || t.Resolved == nil {
				complete = false
				break
			}
			if !*t.Resolved {
				gate.OpenThreads++
			}
		}
		for _, review := range gate.Reviews {
			if review.Author.Login == "" {
				gate.ReviewProblem = "review history contains an unavailable reviewer"
			}
			if review.State == "" {
				complete = false
			}
		}
		if complete {
			gates[url] = gate
		}
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
