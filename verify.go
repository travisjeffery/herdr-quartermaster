package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// verify re-checks stale open beads for evidence that their work is already
// done, closing the ones the evidence settles and flagging the rest to the
// coordinator. Nothing is closed on weak evidence, and nothing is closed at
// all unless auto_close is on.

// Evidence classes, cheapest first.
const (
	evLinear    = "linear"    // the bead's Linear issue is Done or Canceled
	evPR        = "pr"        // every PR the bead links is merged
	evRefs      = "refs"      // merged PRs since its creation mention its key or id
	evGone      = "gone"      // the code a bug bead quotes is no longer on main
	evDuplicate = "duplicate" // a closed bead has the same title or Linear key
)

type evidence struct {
	Class  string `json:"class"`
	Strong bool   `json:"strong,omitempty"`
	Detail string `json:"detail"`
}

type verdict struct {
	Bead     Bead
	Evidence []evidence
	// Action is close, flag or none.
	Action string
	// Why is what kept a strong verdict from closing.
	Why string
	// Level is the bead's autonomy level the verdict was reached at.
	Level int
}

type linearIssue struct {
	Key   string
	URL   string
	State string // Linear's state type: completed, canceled, started, ...
	Name  string
	// Updated is the issue's updatedAt, which a pass compares to skip it.
	Updated string
}

// prRef is a PR the verify query returned: by URL for linked ones, by search
// for mentions.
type prRef struct {
	URL      string    `json:"url"`
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	State    string    `json:"state"`
	MergedAt time.Time `json:"mergedAt"`
	Head     string    `json:"headRefOid"`
	Branch   string    `json:"headRefName"`
	// Body is fetched only by the per-repo search, to match beads locally.
	Body string `json:"body,omitempty"`
}

// verifyEnv is everything a verify pass reads or changes.
type verifyEnv struct {
	beads       func() ([]Bead, error) // every bead not closed
	closed      func() ([]Bead, error)
	show        func(id string) (Bead, error)
	agents      func() ([]Agent, error)
	graphqlLeft func() (int, error)
	linear      func(keys []string) (map[string]linearIssue, error)
	// github answers, in one query, the state of each linked PR and the
	// merged PRs each search finds.
	github func(urls []string, searches map[string]string) (map[string]prRef, map[string][]prRef, error)
	slug   func(repo string) string
	// head fetches repo's default branch and returns its head SHA.
	head func(repo string) (string, error)
	// revAt is the default branch's commit as of at.
	revAt func(repo string, at time.Time) (string, error)
	// grep reports whether needle is in repo at rev, within paths if any.
	grep func(repo, rev, needle string, paths []string) (bool, error)
	// laterPass is a successful run of the failed run's workflow started after it.
	runView   func(url string) (Run, error)
	laterPass func(r Run) (Run, bool, error)
	close     func(id, reason string) error
	note      func(id, text string) error
	event     func(Event) error
}

func liveVerifyEnv(cfg Config, h Herdr) verifyEnv {
	gh := GH{repo: cfg.Repo}
	slugs := map[string]string{}
	return verifyEnv{
		beads: func() ([]Bead, error) {
			return Beads{}.list("--status", strings.Join([]string{StatusOpen, StatusInProgress, StatusBlocked, StatusNeedsMe}, ","))
		},
		closed:      func() ([]Bead, error) { return Beads{}.list("--status", StatusClosed) },
		show:        Beads{}.Show,
		agents:      h.Agents,
		graphqlLeft: gh.GraphQLLeft,
		linear:      linearIssues,
		github:      gh.VerifyQuery,
		slug: func(repo string) string {
			if s, ok := slugs[repo]; ok {
				return s
			}
			out, _ := gitRun(repo, "remote", "get-url", "origin")
			slugs[repo] = repoSlug(strings.TrimSpace(out))
			return slugs[repo]
		},
		grep: gitGrep,
		head: func(repo string) (string, error) {
			if _, err := gitRun(repo, "fetch", "--quiet", "origin", cfg.BaseBranch); err != nil {
				return "", err
			}
			out, err := gitRun(repo, "rev-parse", "origin/"+cfg.BaseBranch)
			return strings.TrimSpace(out), err
		},
		revAt: func(repo string, at time.Time) (string, error) {
			out, err := gitRun(repo, "rev-list", "-1", "--before="+at.Format(time.RFC3339), "origin/"+cfg.BaseBranch)
			return strings.TrimSpace(out), err
		},
		runView: func(url string) (Run, error) {
			m := runURL.FindStringSubmatch(url)
			if m == nil {
				return Run{}, fmt.Errorf("not a run URL: %s", url)
			}
			return gh.RunView(m[1], m[2])
		},
		laterPass: gh.LaterPass,
		close: func(id, reason string) error {
			_, err := Beads{}.run("close", id, "--reason", reason)
			return err
		},
		note:  Beads{}.Note,
		event: writeEvent,
	}
}

var slugPattern = regexp.MustCompile(`github\.com[:/]([\w.-]+/[\w.-]+?)(?:\.git)?$`)

// repoSlug is owner/name from a GitHub remote URL, "" for anything else.
func repoSlug(remote string) string {
	if m := slugPattern.FindStringSubmatch(remote); m != nil {
		return m[1]
	}
	return ""
}

// closable is why a bead must not be closed automatically, "" if it may be.
// Open children are follow-ups its own evidence says nothing about.
func closable(b Bead, openChildren int) string {
	switch {
	case openChildren > 0:
		if openChildren == 1 {
			return "1 open child"
		}
		return fmt.Sprintf("%d open children", openChildren)
	case b.HasLabel(LabelRollingOut):
		return "rolling out"
	case b.Status == StatusNeedsMe:
		return "waiting on TJ"
	case b.Status == StatusBlocked:
		return "blocked"
	case b.Type == "epic":
		return "an epic"
	}
	return ""
}

// decide turns a bead's evidence into an action: close only on strong
// evidence, with auto-close on, for a bead that may be closed.
func decide(b Bead, ev []evidence, openChildren int, autoClose bool) verdict {
	v := verdict{Bead: b, Evidence: ev, Action: "none"}
	if len(ev) == 0 {
		return v
	}
	v.Action = "flag"
	if !slices.ContainsFunc(ev, func(e evidence) bool { return e.Strong }) {
		return v
	}
	switch why := closable(b, openChildren); {
	case why != "":
		v.Why = why
	case workRemains(b) != "":
		v.Why = workRemains(b)
	case !autoClose:
		v.Why = "its autonomy level is below 3"
	default:
		v.Action = "close"
	}
	return v
}

func (v verdict) summary() string {
	var parts []string
	for _, e := range v.Evidence {
		parts = append(parts, e.Class+": "+e.Detail)
	}
	return strings.Join(parts, "; ")
}

func (v verdict) line() string {
	s := fmt.Sprintf("- %s %s: %s", v.Bead.ID, v.Action, shortTitle(v.Bead.Title, 60))
	if len(v.Evidence) > 0 {
		s += " — " + v.summary()
	}
	if v.Why != "" {
		s += " (strong, not closed: " + v.Why + ")"
	}
	return s
}

// linearEvidence is (a): the bead's Linear issue is finished. It is never
// strong: an issue often spans more than the bead's own part of the work.
func linearEvidence(key string, issues map[string]linearIssue) []evidence {
	is, ok := issues[key]
	if !ok || (is.State != "completed" && is.State != "canceled") {
		return nil
	}
	return []evidence{{Class: evLinear, Detail: fmt.Sprintf("%s is %s %s", key, is.Name, is.URL)}}
}

// prEvidence is (b): the bead links PRs, at least one merged and none still
// open. It is strong only when every linked PR merged, one of them is the
// bead's own, and the bead doesn't say work remains after the merge; a closed,
// unmerged one leaves it a flag.
func prEvidence(b Bead, urls []string, prs map[string]prRef, branchPrefix string) []evidence {
	var merged, unmerged []string
	owned := false
	for _, u := range urls {
		pr, ok := prs[u]
		if !ok {
			return nil
		}
		switch pr.State {
		case "OPEN":
			return nil
		case "MERGED":
			merged = append(merged, u)
			owned = owned || ownsPR(b, pr, branchPrefix)
		default:
			unmerged = append(unmerged, u)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	e := evidence{Class: evPR, Strong: true, Detail: "merged " + strings.Join(merged, ", ")}
	if len(unmerged) > 0 {
		e.Strong = false
		e.Detail += " (but closed unmerged: " + strings.Join(unmerged, ", ") + ")"
	} else if !owned {
		e.Strong = false
		e.Detail += " (but none is its own PR)"
	} else if why := workRemains(b); why != "" {
		e.Strong = false
		e.Detail += " (but " + why + ")"
	}
	return []evidence{e}
}

// notedOwnPR is a PR a bead's notes record as its own: the "PR: <url>" every
// worker notes when it opens one.
var notedOwnPR = regexp.MustCompile(`(?i)\bPR:\s*(https://github\.com/[\w.-]+/[\w.-]+/pull/\d+)`)

// ownsPR reports whether pr is the bead's own work: noted as its PR, or cut
// from its branch. A PR it only mentions may be anyone's.
func ownsPR(b Bead, pr prRef, branchPrefix string) bool {
	if pr.Branch != "" && onBeadBranch(branchPrefix, pr.Branch, b) {
		return true
	}
	for _, m := range notedOwnPR.FindAllStringSubmatch(b.Notes, -1) {
		if m[1] == pr.URL {
			return true
		}
	}
	return false
}

var (
	// Work that a merge starts rather than finishes.
	rolloutTitle = regexp.MustCompile(`(?i)\b(roll ?out|deploy|provision|migrat|backfill|canary|enable)`)
	// Notes that say something is still to do.
	openWork = regexp.MustCompile(`(?i:\b(remaining|still (on|to|need|open|fail|broken|pending)|not (yet|done)|todo|follow[- ]up|next step|pending|waiting|deferred|blocked|keep open)\b)|\bOPEN\b`)
	// verify's own close notes, which quote the PRs it saw and so would hide
	// the notes before them once the bead is reopened.
	verifyNote = regexp.MustCompile(`(?m)^.*AUTO: \[verify\b.*$`)
)

// workRemains is why a bead's merged PRs may not finish it: rollout-shaped
// work, or notes after its last PR link that say work remains. "" if neither.
func workRemains(b Bead) string {
	if m := rolloutTitle.FindString(b.Title); m != "" {
		return fmt.Sprintf("its title says %q", strings.ToLower(m))
	}
	after := verifyNote.ReplaceAllString(b.Notes, "")
	if locs := prURL.FindAllStringIndex(after, -1); len(locs) > 0 {
		after = after[locs[len(locs)-1][1]:]
	}
	if m := openWork.FindString(after); m != "" {
		return fmt.Sprintf("its notes say %q", strings.ToLower(m))
	}
	return ""
}

// maxSharers is how many beads a merged PR may mention and still be evidence
// about one of them; past it, it is an umbrella PR for a whole family.
const maxSharers = 3

// refEvidence is (c): merged PRs since the bead was created that mention its
// Linear key or id, other than the ones it links, and few other beads.
func refEvidence(refs []string, sharers map[string]int) []evidence {
	var urls []string
	for _, u := range refs {
		if sharers[u] <= maxSharers {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return nil
	}
	return []evidence{{Class: evRefs, Detail: "merged since it was opened and mention it: " + strings.Join(urls, ", ")}}
}

var (
	backtickSpan = regexp.MustCompile("`([^`\n]{6,120})`")
	quotedSpan   = regexp.MustCompile(`"([^"\n]{12,120})"`)
	urlSpan      = regexp.MustCompile(`https?://\S+`)
	// Code-shaped words: camelCase, PascalCase with two humps, or dotted.
	codeWord = regexp.MustCompile(`\b(?:[a-z]+[A-Z]\w*|[A-Z][a-z0-9]+(?:[A-Z][a-z0-9]+)+|[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)+)\b`)
	filePath = regexp.MustCompile(`\b[\w./-]+\.(?:go|ts|tsx|js|py|rs|tf|ya?ml|json|sh|toml)\b`)
)

const maxNeedles = 8

// needles are the strings a bug bead quotes or names, and the files it names
// to look for them in.
func needles(text string) ([]string, []string) {
	text = urlSpan.ReplaceAllString(text, " ")
	var files []string
	for _, f := range filePath.FindAllString(text, -1) {
		if !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) >= 8 && len(out) < maxNeedles && !slices.Contains(out, s) && !slices.Contains(files, s) {
			out = append(out, s)
		}
	}
	for _, m := range backtickSpan.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range quotedSpan.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, w := range codeWord.FindAllString(text, -1) {
		if !filePath.MatchString(w) {
			add(w)
		}
	}
	return out, files
}

// pathspecs scopes a grep to the files a bead names, wherever they live.
func pathspecs(files []string) []string {
	var out []string
	for _, f := range files {
		if strings.Contains(f, "/") {
			out = append(out, f)
		} else {
			out = append(out, "**/"+f)
		}
	}
	return out
}

var titleNoise = regexp.MustCompile(`[^a-z0-9]+`)

func normTitle(t string) string {
	return strings.Trim(titleNoise.ReplaceAllString(strings.ToLower(t), " "), " ")
}

// duplicateEvidence is (e): a closed bead with the same title or Linear key.
// Beads in one family often share a Linear issue, so a key alone doesn't make
// a sibling, parent, child or any bead under the same root id a duplicate.
func duplicateEvidence(b Bead, key string, closed []Bead, prefixes []string) []evidence {
	title := normTitle(b.Title)
	for _, c := range closed {
		if c.ID == b.ID {
			continue
		}
		family := (b.Parent != "" && c.Parent == b.Parent) || c.Parent == b.ID || b.Parent == c.ID || idRoot(c.ID) == idRoot(b.ID)
		same := title != "" && normTitle(c.Title) == title
		if !same && !family && key != "" && linearKey(c, prefixes) == key {
			same = true
		}
		if same {
			return []evidence{{Class: evDuplicate, Detail: fmt.Sprintf("%s closed: %s", c.ID, shortTitle(c.CloseReason, 80))}}
		}
	}
	return nil
}

// idRoot is the top of a bead's id hierarchy: backend-x for backend-x.20.1.
func idRoot(id string) string {
	root, _, _ := strings.Cut(id, ".")
	return root
}

// verifySearch finds merged PRs in the bead's repo, since it was created, that
// mention its Linear key or its id.
func verifySearch(slug string, b Bead, key string) string {
	terms := strconv.Quote(b.ID)
	if key != "" {
		terms = strconv.Quote(key) + " OR " + terms
	}
	q := fmt.Sprintf("repo:%s is:pr is:merged %s", slug, terms)
	if !b.CreatedAt.IsZero() {
		q += " merged:>=" + b.CreatedAt.UTC().Format("2006-01-02")
	}
	return q
}

func applyVerdict(env verifyEnv, v verdict, now time.Time) error {
	switch v.Action {
	case "close":
		// The close is a class 3 step taken without TJ, noted like any other.
		auto := fmt.Sprintf("AUTO: [verify L%d C%d] closed as already done — %s", v.Level, classHousekeeping, v.summary())
		if err := env.note(v.Bead.ID, auto); err != nil {
			return err
		}
		reason := "quartermaster verify: already done. " + v.summary()
		if err := env.close(v.Bead.ID, reason); err != nil {
			return err
		}
		return env.event(Event{Bead: v.Bead.ID, Kind: EventAutoClosed, Summary: "closed as already done: " + v.summary(), At: now})
	case "flag":
		summary := "likely stale: " + v.summary()
		if v.Why != "" {
			summary += " (strong evidence, not closed: " + v.Why + ")"
		}
		return env.event(Event{Bead: v.Bead.ID, Kind: EventLikelyStale, Summary: summary, At: now})
	}
	return nil
}

// GraphQLLeft is the user's remaining GitHub GraphQL points this hour, for a
// point. The free REST rate_limit endpoint can't stand in: its graphql
// resource tracks another bucket and reads nearly full while this one is out.
func (g GH) GraphQLLeft() (int, error) {
	out, err := g.run("api", "graphql", "-f", "query={ rateLimit { remaining } }", "--jq", ".data.rateLimit.remaining")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// VerifyQuery fetches each linked PR's state and each search's merged PRs in
// one GraphQL query.
func (g GH) VerifyQuery(urls []string, searches map[string]string) (map[string]prRef, map[string][]prRef, error) {
	q, alias := verifyGraphQL(urls, searches)
	if q == "" {
		return nil, nil, nil
	}
	out, err := g.graphql(q)
	if err != nil {
		return nil, nil, err
	}
	return parseVerify(out, alias)
}

const prRefFields = "url number title state mergedAt headRefOid headRefName"

func verifyGraphQL(urls []string, searches map[string]string) (string, map[string]string) {
	var q strings.Builder
	alias := map[string]string{}
	for i, u := range urls {
		m := prURL.FindStringSubmatch(u)
		if m == nil || slices.Contains(urls[:i], u) {
			continue
		}
		owner, name, _ := strings.Cut(m[1], "/")
		a := fmt.Sprintf("p%d", i)
		alias[a] = u
		fmt.Fprintf(&q, "%s: repository(owner: %q, name: %q) { pullRequest(number: %s) { %s } } ", a, owner, name, m[2], prRefFields)
	}
	for _, a := range slices.Sorted(func(yield func(string) bool) {
		for k := range searches {
			if !yield(k) {
				return
			}
		}
	}) {
		// A repo's search (r*) lists everything merged since the last pass and
		// is matched locally, so it needs the body and more rows.
		fields, n := prRefFields, 10
		if strings.HasPrefix(a, "r") {
			fields, n = prRefFields+" body", 100
		}
		fmt.Fprintf(&q, "%s: search(query: %q, type: ISSUE, first: %d) { nodes { ... on PullRequest { %s } } } ", a, searches[a], n, fields)
	}
	if q.Len() == 0 {
		return "", nil
	}
	return "{ " + q.String() + "}", alias
}

func parseVerify(out []byte, alias map[string]string) (map[string]prRef, map[string][]prRef, error) {
	var resp struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, nil, err
	}
	prs := map[string]prRef{}
	found := map[string][]prRef{}
	for a, raw := range resp.Data {
		if url, ok := alias[a]; ok {
			var r struct {
				PullRequest *prRef `json:"pullRequest"`
			}
			if json.Unmarshal(raw, &r) == nil && r.PullRequest != nil {
				prs[url] = *r.PullRequest
			}
			continue
		}
		var s struct {
			Nodes []prRef `json:"nodes"`
		}
		if json.Unmarshal(raw, &s) == nil {
			for _, n := range s.Nodes {
				if n.URL != "" {
					found[a] = append(found[a], n)
				}
			}
		}
	}
	return prs, found, nil
}

// LaterPass is a successful run of r's workflow that started after r.
func (g GH) LaterPass(r Run) (Run, bool, error) {
	out, err := g.run("run", "list", "-R", r.Repo, "--workflow", r.Workflow, "--status", "success", "--limit", "1", "--json", runFields)
	if err != nil {
		return Run{}, false, err
	}
	var runs []Run
	if err := json.Unmarshal(out, &runs); err != nil {
		return Run{}, false, err
	}
	if len(runs) == 0 || !runs[0].CreatedAt.After(r.CreatedAt) {
		return Run{}, false, nil
	}
	return runs[0], true, nil
}

// linearIssues looks the keys up in one query through the linear CLI, which
// fails without credentials (LINEAR_API_KEY or `linear auth login`).
func linearIssues(keys []string) (map[string]linearIssue, error) {
	var q strings.Builder
	for i, k := range keys {
		fmt.Fprintf(&q, "i%d: issue(id: %q) { identifier url updatedAt state { name type } } ", i, k)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "linear", "api", "{ "+q.String()+"}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	issues, err := parseLinear(stdout.Bytes())
	// A key that doesn't exist fails the command but leaves the others' data.
	if len(issues) == 0 && runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("linear api: %v: %s", runErr, msg)
	}
	return issues, err
}

func parseLinear(out []byte) (map[string]linearIssue, error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var resp struct {
		Data map[string]*struct {
			Identifier string `json:"identifier"`
			URL        string `json:"url"`
			UpdatedAt  string `json:"updatedAt"`
			State      struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	issues := map[string]linearIssue{}
	for _, is := range resp.Data {
		if is != nil && is.Identifier != "" {
			issues[is.Identifier] = linearIssue{Key: is.Identifier, URL: is.URL, State: is.State.Type, Name: is.State.Name, Updated: is.UpdatedAt}
		}
	}
	return issues, nil
}

// gitGrep reports whether needle occurs in repo at rev; git grep exits 1 for
// no match, which is not an error.
func gitGrep(repo, rev, needle string, paths []string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"-C", repo, "grep", "-q", "-F", "-e", needle, rev}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("git grep: %v: %s", err, strings.TrimSpace(stderr.String()))
}

// VerifyMark is what a pass knows about a bead: the inputs its evidence was
// built from and the evidence itself. A later pass rebuilds only the classes
// whose inputs changed, and every class once fullEvery has passed.
type VerifyMark struct {
	Updated time.Time `json:"updated"` // the bead's updated_at
	Linear  string    `json:"linear,omitempty"`
	PRs     string    `json:"prs,omitempty"`  // linked PRs' state and head
	Main    string    `json:"main,omitempty"` // the default branch head (d) last looked at
	// Desc is the bead's description, which bd list leaves out.
	Desc string `json:"desc,omitempty"`
	// Refs are the merged PRs (c) found mentioning the bead.
	Refs []string `json:"refs,omitempty"`
	// Present is the code (d) found on main when the bead was opened, Gone the
	// part of it missing at Main, FailedRun the linked run that failed, whose
	// workflow is watched for a later pass.
	Present   []string            `json:"present,omitempty"`
	Gone      []string            `json:"gone,omitempty"`
	FailedRun *Run                `json:"failed_run,omitempty"`
	Evidence  map[string]evidence `json:"evidence,omitempty"`
	FullAt    time.Time           `json:"full_at"`
	At        time.Time           `json:"at"`
	// Line is the last verdict, logged again only when it changes; Acted the
	// last one written to the inbox.
	Line  string `json:"line,omitempty"`
	Acted string `json:"acted,omitempty"`
	// Rejected is the evidence present when the bead's verdict was turned
	// down, RejectedN the rejections seen; only evidence since then counts.
	Rejected  []string `json:"rejected,omitempty"`
	RejectedN int      `json:"rejected_n,omitempty"`
}

// VerifyRepo is where a repo's default branch was at the last pass, and when
// its merged PRs were last searched.
type VerifyRepo struct {
	Head     string    `json:"head"`
	Searched time.Time `json:"searched"`
}

// fullEvery is how often a bead is re-checked from scratch even when nothing
// it depends on changed.
const fullEvery = 24 * time.Hour

// searchOverlap re-reads a little before the last search, so a PR merged as
// it ran is not missed.
const searchOverlap = 10 * time.Minute

// verifyCandidates are the beads a pass looks at: not closed, no live agent,
// untouched for age. Never-checked beads come first, then the least recently
// fully checked.
func verifyCandidates(beads []Bead, live map[string]bool, marks map[string]VerifyMark, now time.Time, age time.Duration) []Bead {
	var out []Bead
	for _, b := range beads {
		if b.Status != StatusClosed && !live[agentName(b.ID)] && now.Sub(b.UpdatedAt) >= age {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := marks[out[i].ID].FullAt, marks[out[j].ID].FullAt
		if !ai.Equal(aj) {
			return ai.Before(aj)
		}
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out
}

// verifyWork is one bead's place in a pass: what changed and what that costs
// in GitHub calls.
type verifyWork struct {
	b    Bead
	m    VerifyMark
	repo string
	key  string
	urls []string
	full bool
	code bool    // main moved: re-grep (d) at the new head
	run  bool    // look for a later pass of (d)'s failed run
	refs []prRef // newly merged PRs that mention it
}

func prSig(urls []string, prs map[string]prRef) string {
	var parts []string
	for _, u := range urls {
		parts = append(parts, u+"="+prs[u].State+"@"+prs[u].Head)
	}
	return strings.Join(parts, ",")
}

func linearSig(key string, issues map[string]linearIssue) string {
	if key == "" {
		return ""
	}
	return key + "=" + issues[key].State + "@" + issues[key].Updated
}

// cost is the GitHub calls a bead's work needs beyond the pass's own query: a
// full check searches for its mentions and looks up its failed run.
func (w verifyWork) cost() int {
	n := 0
	if w.full {
		n++
	}
	if w.run {
		n++ // gh run list for a later pass
		if w.full {
			n += len(NotedRuns(w.b.Notes)) // gh run view of each linked run
		}
	}
	return n
}

// mentions reports whether a merged PR names the bead's id or Linear key.
func mentions(pr prRef, id, key string) bool {
	text := pr.Title + "\n" + pr.Body
	for _, t := range []string{id, key} {
		if t == "" {
			continue
		}
		for i := 0; ; {
			j := strings.Index(text[i:], t)
			if j < 0 {
				break
			}
			if standalone(text, i+j, i+j+len(t)) {
				return true
			}
			i += j + len(t)
		}
	}
	return false
}

// plan decides what each bead needs this pass. Linear, linked PRs and newly
// merged PRs come from the pass's own queries, so a change there costs
// nothing more; a moved default branch re-greps a bug's code, which is local;
// a bug whose quoted code is gone keeps watching its failed workflow. Beads
// past the per-pass cap of GitHub calls wait for the next pass, unless named.
func plan(cfg Config, cands []Bead, marks map[string]VerifyMark, mains map[string]string, issues map[string]linearIssue, prs map[string]prRef, merged map[string][]prRef, named bool, now time.Time) (work, deferred []verifyWork) {
	used := 0
	for _, b := range cands {
		m := marks[b.ID]
		w := verifyWork{b: b, m: m, repo: cfg.repoFor(b), key: linearKey(b, cfg.LinearPrefixes)}
		w.full = named || m.At.IsZero() || !m.Updated.Equal(b.UpdatedAt) || now.Sub(m.FullAt) >= fullEvery
		bug := b.Type == "bug"
		w.code = bug && !w.full && len(m.Present) > 0 && mains[w.repo] != "" && mains[w.repo] != m.Main
		w.run = bug && ((w.full && len(NotedRuns(b.Notes)) > 0) || (!w.full && len(m.Gone) > 0 && m.FailedRun != nil && !m.Evidence[evGone].Strong))
		for _, pr := range merged[w.repo] {
			if !pr.MergedAt.Before(b.CreatedAt) && !slices.Contains(m.Refs, pr.URL) && mentions(pr, b.ID, w.key) {
				w.refs = append(w.refs, pr)
			}
		}
		if !w.full {
			w.b.Description = m.Desc
			w.urls = NotedPRs(b.Notes + "\n" + m.Desc)
			if !w.code && !w.run && len(w.refs) == 0 && linearSig(w.key, issues) == m.Linear && prSig(w.urls, prs) == m.PRs {
				continue
			}
		}
		if c := w.cost(); c > 0 && !named && cfg.VerifyMax > 0 && used+c > cfg.VerifyMax {
			deferred = append(deferred, w)
			continue
		}
		used += w.cost()
		work = append(work, w)
	}
	return work, deferred
}

// verifyPass checks the stale beads, or the named ones, and with act closes or
// flags them and caches what it found in st. Without act it changes nothing
// and returns every verdict; with act, only the verdicts that changed.
func verifyPass(cfg Config, env verifyEnv, st *TickerState, ids []string, act bool, now time.Time) ([]string, error) {
	left, err := env.graphqlLeft()
	if err == nil && act {
		st.GHLeft, st.GHLeftAt = left, now
	}
	if err == nil && left < cfg.GHMinRemaining {
		return []string{fmt.Sprintf("verify skipped: %d GitHub GraphQL points left, below gh_min_remaining %d", left, cfg.GHMinRemaining)}, errBudget
	}
	marks, repos := st.Verified, st.VerifyRepos
	if !act || marks == nil {
		marks = maps.Clone(st.Verified)
		if marks == nil {
			marks = map[string]VerifyMark{}
		}
	}
	if !act || repos == nil {
		repos = maps.Clone(st.VerifyRepos)
		if repos == nil {
			repos = map[string]VerifyRepo{}
		}
	}
	cands, children, err := listVerify(cfg, env, marks, ids, now)
	if err != nil || len(cands) == 0 {
		if err == nil {
			return []string{"verify: no stale beads"}, nil
		}
		return nil, err
	}

	// One query probes every candidate's Linear issue; one GitHub query every
	// linked PR and, per repo whose default branch moved, the PRs merged since
	// the last pass. Heads come from git, which costs no API call.
	var lines []string
	var keys, urls []string
	for _, b := range cands {
		if k := linearKey(b, cfg.LinearPrefixes); k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
		urls = append(urls, NotedPRs(b.Notes+"\n"+marks[b.ID].Desc)...)
	}
	issues := map[string]linearIssue{}
	if len(keys) > 0 {
		if issues, err = env.linear(keys); err != nil {
			lines = append(lines, "verify: Linear unavailable, skipping its evidence: "+err.Error())
		}
	}
	mains := map[string]string{}
	repoSearch := map[string]string{} // alias → repo
	searches := map[string]string{}
	for _, b := range cands {
		repo := cfg.repoFor(b)
		if _, ok := mains[repo]; ok {
			continue
		}
		if mains[repo], err = env.head(repo); err != nil {
			lines = append(lines, fmt.Sprintf("verify: %s head: %v", repo, err))
			continue
		}
		r := repos[repo]
		if slug := env.slug(repo); slug != "" && !r.Searched.IsZero() && mains[repo] != r.Head {
			a := fmt.Sprintf("r%d", len(repoSearch))
			repoSearch[a] = repo
			searches[a] = fmt.Sprintf("repo:%s is:pr is:merged merged:>=%s", slug, r.Searched.Add(-searchOverlap).UTC().Format(time.RFC3339))
		}
	}
	prs, found, err := env.github(slices.Compact(slices.Sorted(slices.Values(urls))), searches)
	if err != nil {
		return lines, fmt.Errorf("verify: github: %w", err)
	}
	merged := map[string][]prRef{}
	for a, repo := range repoSearch {
		merged[repo] = found[a]
	}

	work, deferred := plan(cfg, cands, marks, mains, issues, prs, merged, len(ids) > 0, now)
	searches = map[string]string{}
	for i := range work {
		w := &work[i]
		if !w.full {
			continue
		}
		full, err := env.show(w.b.ID)
		if err != nil {
			return lines, err
		}
		w.b.Description = full.Description
		w.urls = NotedPRs(w.b.Notes + "\n" + w.b.Description)
		if slug := env.slug(w.repo); slug != "" {
			searches[fmt.Sprintf("s%d", i)] = verifySearch(slug, w.b, w.key)
		}
	}
	// Linked PRs only in a description just loaded weren't probed yet.
	var more []string
	for _, w := range work {
		for _, u := range w.urls {
			if _, ok := prs[u]; !ok && !slices.Contains(more, u) {
				more = append(more, u)
			}
		}
	}
	found = map[string][]prRef{}
	if len(searches) > 0 || len(more) > 0 {
		extra, f, err := env.github(more, searches)
		if err != nil {
			return lines, fmt.Errorf("verify: github: %w", err)
		}
		maps.Copy(prs, extra)
		found = f
	}
	var closed []Bead
	if len(work) > 0 {
		if closed, err = env.closed(); err != nil {
			return lines, err
		}
	}

	// How many beads each merged PR mentions, this pass's finds included.
	sharers := map[string]int{}
	seen := map[string]bool{}
	count := func(id, u string) {
		if !seen[id+" "+u] {
			seen[id+" "+u] = true
			sharers[u]++
		}
	}
	for id, m := range marks {
		for _, u := range m.Refs {
			count(id, u)
		}
	}
	for i, w := range work {
		for _, pr := range append(found[fmt.Sprintf("s%d", i)], w.refs...) {
			count(w.b.ID, pr.URL)
		}
	}

	var errs []error
	for i, w := range work {
		m := w.m
		if w.full {
			m = VerifyMark{Desc: w.b.Description, Line: w.m.Line, Acted: w.m.Acted, Rejected: w.m.Rejected, RejectedN: w.m.RejectedN}
		}
		if m.Evidence == nil {
			m.Evidence = map[string]evidence{}
		}
		set := func(class string, ev []evidence) {
			if len(ev) > 0 {
				m.Evidence[class] = ev[0]
			} else {
				delete(m.Evidence, class)
			}
		}
		set(evLinear, linearEvidence(w.key, issues))
		set(evPR, prEvidence(w.b, w.urls, prs, cfg.BranchPrefix))
		refs := w.refs
		if w.full {
			refs = append(found[fmt.Sprintf("s%d", i)], refs...)
		}
		for _, pr := range refs {
			if pr.State == "MERGED" && !pr.MergedAt.Before(w.b.CreatedAt) && !slices.Contains(w.urls, pr.URL) && !slices.Contains(m.Refs, pr.URL) {
				m.Refs = append(m.Refs, pr.URL)
			}
		}
		set(evRefs, refEvidence(m.Refs, sharers))
		if w.b.Type == "bug" && (w.full || w.code || w.run) {
			gone, err := updateGone(env, &m, w, mains[w.repo])
			if err != nil {
				lines = append(lines, fmt.Sprintf("verify %s: code check: %v", w.b.ID, err))
			} else {
				set(evGone, gone)
			}
		}
		set(evDuplicate, duplicateEvidence(w.b, w.key, closed, cfg.LinearPrefixes))

		m.Updated, m.Linear, m.PRs, m.At = w.b.UpdatedAt, linearSig(w.key, issues), prSig(w.urls, prs), now
		if w.full {
			m.FullAt = now
		}
		lvl, _ := cfg.beadLevel(w.b, autonomyStopped())
		ev := orderedEvidence(m.Evidence)
		// shortcut: a rejection is dated by the pass that first sees it, so
		// evidence that arrived between the note and that pass counts as seen.
		if n := rejections(w.b); n > m.RejectedN {
			m.RejectedN = n
			for _, e := range ev {
				if !slices.Contains(m.Rejected, e.key()) {
					m.Rejected = append(m.Rejected, e.key())
				}
			}
		}
		ev = slices.DeleteFunc(ev, func(e evidence) bool { return slices.Contains(m.Rejected, e.key()) })
		v := decide(w.b, ev, children[w.b.ID], lvl >= classHousekeeping)
		v.Level = lvl
		if !act || v.line() != m.Line {
			lines = append(lines, v.line())
		}
		m.Line = v.line()
		if v.Action == "none" {
			m.Acted = ""
		}
		if act && v.Action == "close" {
			if why := stillClosable(env, w.b); why != "" {
				lines = append(lines, fmt.Sprintf("verify %s: not closed: %s", w.b.ID, why))
				continue
			}
		}
		if act && v.Action != "none" && v.line() != m.Acted {
			if err := applyVerdict(env, v, now); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", w.b.ID, err))
				continue
			}
			m.Acted = v.line()
		}
		marks[w.b.ID] = m
	}
	if len(deferred) > 0 {
		var ids []string
		for _, w := range deferred {
			ids = append(ids, w.b.ID)
		}
		lines = append(lines, fmt.Sprintf("verify: %d deferred to the next pass (verify_max %d GitHub calls): %s", len(ids), cfg.VerifyMax, strings.Join(ids, " ")))
	}
	if len(work)+len(deferred) == 0 {
		lines = append(lines, fmt.Sprintf("verify: %d stale, nothing changed", len(cands)))
	} else {
		lines = append(lines, fmt.Sprintf("verify: %d stale, %d checked, %d deferred", len(cands), len(work), len(deferred)))
	}
	// A repo's window moves on only once its search ran: the first pass just
	// records where it is, and the beads' own full checks cover the past.
	for repo, head := range mains {
		if head == "" {
			continue
		}
		r := repos[repo]
		if r.Searched.IsZero() || r.Head != head {
			r.Searched = now
		}
		r.Head = head
		repos[repo] = r
	}
	if act {
		st.Verified, st.VerifyRepos = marks, repos
	}
	return lines, errors.Join(errs...)
}

var errBudget = errors.New("GitHub budget too low")

// stillClosable re-reads a bead just before it is closed, since a pass's
// lookups take long enough for it to be claimed, noted, given a child or an
// agent. "" if it may still be closed.
func stillClosable(env verifyEnv, b Bead) string {
	cur, err := env.show(b.ID)
	switch {
	case err != nil:
		return "could not re-read it: " + err.Error()
	case cur.Status != b.Status || !cur.UpdatedAt.Equal(b.UpdatedAt):
		return "it changed during the pass"
	}
	agents, err := env.agents()
	if err != nil {
		return "could not list agents: " + err.Error()
	}
	if slices.ContainsFunc(agents, func(a Agent) bool { return a.Name == agentName(b.ID) }) {
		return "an agent started on it during the pass"
	}
	all, err := env.beads()
	if err != nil {
		return "could not list beads: " + err.Error()
	}
	children := 0
	for _, c := range all {
		if c.Parent == b.ID {
			children++
		}
	}
	if why := closable(cur, children); why != "" {
		return "now " + why
	}
	return ""
}

// rejectedNote is a note turning a verdict down: the coordinator's or TJ's, or
// verify's own close note on a bead that was reopened since.
var rejectedNote = regexp.MustCompile(`(?i)likely[_ -]stale( flag)? rejected|\bkeep open\b|AUTO: \[verify\b[^\]]*\] closed as already done`)

// LabelStaleRejected marks a bead whose likely_stale verdict was turned down.
const LabelStaleRejected = "stale-rejected"

// rejections counts the times a bead's verdict was turned down.
func rejections(b Bead) int {
	n := len(rejectedNote.FindAllStringIndex(b.Notes, -1))
	if b.HasLabel(LabelStaleRejected) {
		n++
	}
	return n
}

// key identifies evidence across passes, without the parts that change while
// it says the same thing: why it was weakened, the latest passing run.
func (e evidence) key() string {
	d, _, _ := strings.Cut(e.Detail, " (but ")
	d, _, _ = strings.Cut(d, ", and ")
	return e.Class + ": " + d
}

var evidenceOrder = []string{evLinear, evPR, evRefs, evGone, evDuplicate}

func orderedEvidence(m map[string]evidence) []evidence {
	var out []evidence
	for _, c := range evidenceOrder {
		if e, ok := m[c]; ok {
			out = append(out, e)
		}
	}
	return out
}

// listVerify is the stale candidates, or the named beads, with the count of
// open children of each, and forgets the cache of beads that closed or went
// away.
func listVerify(cfg Config, env verifyEnv, marks map[string]VerifyMark, ids []string, now time.Time) ([]Bead, map[string]int, error) {
	all, err := env.beads()
	if err != nil {
		return nil, nil, err
	}
	children := map[string]int{}
	for _, b := range all {
		if b.Parent != "" {
			children[b.Parent]++
		}
	}
	if len(ids) > 0 {
		var out []Bead
		for _, id := range ids {
			b, err := env.show(id)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, b)
		}
		return out, children, nil
	}
	agents, err := env.agents()
	if err != nil {
		return nil, nil, err
	}
	live := map[string]bool{}
	for _, a := range agents {
		live[a.Name] = true
	}
	for id := range marks {
		if !slices.ContainsFunc(all, func(b Bead) bool { return b.ID == id }) {
			delete(marks, id)
		}
	}
	return verifyCandidates(all, live, marks, now, cfg.staleAge()), children, nil
}

// updateGone is (d): the code a bug bead quotes was on main when it was opened
// and none of it is now. A run it links that failed, followed by a passing run
// of the same workflow, makes it strong. A full check finds what was there at
// creation; after that only those strings are looked for, at each new head.
func updateGone(env verifyEnv, m *VerifyMark, w verifyWork, head string) ([]evidence, error) {
	if w.full {
		present, err := presentCode(env, w.repo, w.b)
		if err != nil {
			return nil, err
		}
		m.Present, m.FailedRun = present, nil
		// The latest failure: a pass after an older one proves nothing while a
		// later run still fails.
		for _, u := range NotedRuns(w.b.Notes) {
			if r, err := env.runView(u); err == nil && runFailed(r) && (m.FailedRun == nil || r.CreatedAt.After(m.FailedRun.CreatedAt)) {
				m.FailedRun = &r
			}
		}
	}
	if (w.full || w.code) && head != "" {
		paths := pathspecs(filesIn(w.b))
		m.Gone = nil
		for _, s := range m.Present {
			is, err := env.grep(w.repo, head, s, paths)
			if err != nil {
				return nil, err
			}
			if is {
				m.Gone = nil
				break
			}
			m.Gone = append(m.Gone, s)
		}
		m.Main = head
	}
	if len(m.Gone) == 0 {
		return nil, nil
	}
	if prev, ok := m.Evidence[evGone]; ok && prev.Strong && !w.full && !w.code {
		return []evidence{prev}, nil
	}
	e := evidence{Class: evGone, Detail: fmt.Sprintf("%q no longer on main", m.Gone)}
	if m.FailedRun != nil {
		later, ok, err := env.laterPass(*m.FailedRun)
		if err != nil {
			return nil, err
		}
		if ok {
			e.Strong = true
			e.Detail += fmt.Sprintf(", and %s passed in %s after %s failed", m.FailedRun.Workflow, later.URL, m.FailedRun.URL)
		}
	}
	return []evidence{e}, nil
}

func filesIn(b Bead) []string {
	_, files := needles(b.Title + "\n" + b.Description)
	return files
}

// presentCode is the needles of a bug bead that were on main when it was
// opened.
func presentCode(env verifyEnv, repo string, b Bead) ([]string, error) {
	words, files := needles(b.Title + "\n" + b.Description)
	if len(words) == 0 || b.CreatedAt.IsZero() {
		return nil, nil
	}
	then, err := env.revAt(repo, b.CreatedAt)
	if err != nil || then == "" {
		return nil, err
	}
	paths := pathspecs(files)
	var present []string
	for _, w := range words {
		was, err := env.grep(repo, then, w, paths)
		if err != nil {
			return nil, err
		}
		if was {
			present = append(present, w)
		}
	}
	return present, nil
}

// verifyDue reports whether the ticker's verify pass is due.
func verifyDue(cfg Config, st TickerState, now time.Time) bool {
	every := cfg.verifyEvery()
	return every > 0 && now.Sub(st.LastVerify) >= every
}

// verify runs the ticker's pass. A pass skipped for budget is retried after a
// quarter of the interval.
func (t Ticker) verify(st *TickerState, now time.Time) {
	st.LastVerify = now
	lines, err := verifyPass(t.cfg, liveVerifyEnv(t.cfg, t.herdr), st, nil, true, now)
	for _, l := range lines {
		t.log.Print("verify ", strings.TrimPrefix(l, "verify: "))
	}
	if errors.Is(err, errBudget) {
		st.LastVerify = now.Add(-t.cfg.verifyEvery() * 3 / 4)
	} else if err != nil {
		t.log.Printf("verify: %v", err)
	}
}
