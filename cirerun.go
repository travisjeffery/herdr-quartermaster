package main

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCIRerunMaxReruns = 2
	defaultCIRerunMinLevel  = classHousekeeping // 3
	ciRerunPendingTimeout   = 10 * time.Minute
	ciRerunMaxLogBytes      = 512 * 1024
)

// defaultCIRerunPatterns are the built-in case-insensitive regular expressions
// matching transient CI runner and infrastructure failures when [ci_rerun]
// patterns is not overridden in config.toml.
var defaultCIRerunPatterns = []string{
	`(?i)\b(?:spot|preemptible)\b[^\n]{0,60}\b(?:preempt(?:ed|ion)?|terminat(?:ed|ion)|interrupt(?:ed|ion)|reclaim(?:ed)?)\b`,
	`(?i)\b(?:runner|node|instance)\b[^\n]{0,60}\b(?:preempt(?:ed|ion)?|evict(?:ed|ion)|reclaim(?:ed)?|unexpectedly\s+terminated)\b`,
	`(?i)\b(?:process\s+completed\s+with\s+)?exit\s+(?:code|status)\s*:?\s*137\b`,
	`(?i)\b(?:killed\s+by\s+sigkill|terminated\s+with\s+sigkill|signal:\s*killed)\b`,
	`(?i)\blost\s+communication\s+with\s+the\s+server\b`,
	`(?i)\brunner[^\n]{0,120}\blost\s+communication\b`,
	`(?i)\brunner\s+has\s+received\s+a\s+shutdown\s+signal\b`,
	`(?i)\bthe\s+self-hosted\s+runner\b[^\n]{0,120}\b(?:disconnected|offline|shut\s*down|terminated)\b`,
	`(?i)\bstartup_failure\b`,
	`(?i)\bfailed\s+to\s+(?:acquire|start|provision)\s+(?:a\s+)?runner\b`,
	`(?i)\b(?:502\s+bad\s+gateway|503\s+service\s+unavailable|504\s+gateway\s+timeout)\b`,
}

// CIRerunConfig is [ci_rerun] in config.toml: bounded automatic rerun of failed
// GitHub Actions jobs when a tracked PR fails solely on transient runner or
// infrastructure flakes.
type CIRerunConfig struct {
	// Enabled turns bounded CI flake auto-rerun on or off. Omitted defaults to
	// true when MaxReruns > 0; zero Config{} leaves it disabled because
	// MaxReruns == 0.
	Enabled *bool `toml:"enabled"`
	// MaxReruns is the maximum number of automatic `gh run rerun --failed`
	// calls per (pr, head_sha); 0 disables auto-rerun. Default: 2.
	MaxReruns int `toml:"max_reruns"`
	// MinLevel is the minimum bead autonomy level required before the ticker
	// automatically reruns failed CI jobs. Default: 3.
	MinLevel *int `toml:"min_level"`
	// Patterns are regular expressions matched against failing job conclusions,
	// failure annotations, and failed log output. Omitted (nil) uses
	// defaultCIRerunPatterns; an explicit empty list ([]) disables matching.
	Patterns []string `toml:"patterns"`
}

func (c CIRerunConfig) enabled() bool {
	if c.Enabled != nil && !*c.Enabled {
		return false
	}
	return c.MaxReruns > 0 && len(c.effectivePatterns()) > 0
}

func (c CIRerunConfig) minLevel() int {
	if c.MinLevel == nil {
		return defaultCIRerunMinLevel
	}
	return *c.MinLevel
}

func (c CIRerunConfig) effectivePatterns() []string {
	if c.Patterns == nil {
		return defaultCIRerunPatterns
	}
	return c.Patterns
}

func (c CIRerunConfig) compilePatterns() ([]*regexp.Regexp, error) {
	raw := c.effectivePatterns()
	out := make([]*regexp.Regexp, 0, len(raw))
	for _, p := range raw {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("[ci_rerun] patterns contains an empty pattern")
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("[ci_rerun] pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func (c Config) validateCIRerun() error {
	if c.CIRerun.MaxReruns < 0 {
		return fmt.Errorf("[ci_rerun] max_reruns %d is negative; use 0 to disable", c.CIRerun.MaxReruns)
	}
	if c.CIRerun.MinLevel != nil && (*c.CIRerun.MinLevel < 0 || *c.CIRerun.MinLevel > 9) {
		return fmt.Errorf("[ci_rerun] min_level %d must be 0-9", *c.CIRerun.MinLevel)
	}
	if _, err := c.CIRerun.compilePatterns(); err != nil {
		return err
	}
	return nil
}

// CIRerunSnapshot persists bounded CI flake auto-rerun state for the current
// (pr, head_sha) inside Snapshot (state.json) across ticks and restarts.
type CIRerunSnapshot struct {
	// Key is the (pr, head_sha) identity ("<url>#<num>@<sha>") this counter
	// applies to. A replacement PR or new head SHA resets the counter.
	Key string `json:"key,omitempty"`
	// Reruns is how many `gh run rerun --failed` calls have succeeded for Key.
	Reruns int `json:"reruns,omitempty"`
	// LastFingerprint identifies the exact failing check instances last acted on.
	LastFingerprint string `json:"last_fingerprint,omitempty"`
	// LastRerunAt is when the last automatic rerun was triggered.
	LastRerunAt time.Time `json:"last_rerun_at,omitempty"`
	// PendingRerun is true while waiting for GitHub to pick up a rerun we
	// triggered for LastFingerprint.
	PendingRerun bool `json:"pending_rerun,omitempty"`
	// WaitingCompletion is true when every currently failing job matched a
	// flake pattern, but an affected workflow run is still in progress and
	// cannot be rerun yet.
	WaitingCompletion bool `json:"waiting_completion,omitempty"`
	// SurfacedFingerprint is the failure fingerprint for which checks_failing
	// has already surfaced, preventing duplicate alerts and redundant log
	// fetches on subsequent ticks.
	SurfacedFingerprint string `json:"surfaced_fingerprint,omitempty"`
}

func (s CIRerunSnapshot) isZero() bool {
	return s.Key == "" && s.Reruns == 0 && s.LastFingerprint == "" &&
		s.LastRerunAt.IsZero() && !s.PendingRerun && !s.WaitingCompletion &&
		s.SurfacedFingerprint == ""
}

func cloneCIRerunSnapshot(s *CIRerunSnapshot) *CIRerunSnapshot {
	if s == nil || s.isZero() {
		return nil
	}
	cp := *s
	return &cp
}

func ciRerunStateFrom(s Snapshot) CIRerunSnapshot {
	if s.CIRerun == nil {
		return CIRerunSnapshot{}
	}
	return *s.CIRerun
}

func applyCIRerunSnapshot(snap *Snapshot, st CIRerunSnapshot) {
	if st.isZero() {
		snap.CIRerun = nil
		return
	}
	cp := st
	snap.CIRerun = &cp
}

// ciRerunPRKey identifies a PR at one head SHA.
func ciRerunPRKey(pr *PR) string {
	if pr == nil || pr.HeadSHA == "" {
		return ""
	}
	return fmt.Sprintf("%s#%d@%s", pr.URL, pr.Number, pr.HeadSHA)
}

// ciFailureFingerprint hashes the exact failing check instances on pr so a new
// job attempt (new detailsUrl/jobID or completedAt) is distinguished from a
// cached observation of the same failed job even when the check name is
// unchanged.
func ciFailureFingerprint(pr *PR) string {
	return ciFailureFingerprintWith(pr, nil)
}

func ciFailureFingerprintWith(pr *PR, ignore []string) string {
	if pr == nil {
		return ""
	}
	var parts []string
	for _, c := range pr.Checks {
		if checkIgnored(c, ignore) || !checkFailed(c) {
			continue
		}
		result := strings.ToUpper(cmp.Or(c.Conclusion, c.State))
		part := strings.Join([]string{
			c.label(),
			result,
			cmp.Or(c.DetailsURL, c.TargetURL),
			c.CompletedAt,
			c.StartedAt,
		}, "|")
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("%x", sum)[:16]
}

var actionRunJobURL = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/actions/runs/(\d+)(?:/jobs?/(\d+))?`)

// parseActionsRunURL extracts owner/repo, workflow run ID, and optional job ID
// from a GitHub Actions check details URL.
func parseActionsRunURL(raw string) (repo, runID string, jobID int64, ok bool) {
	m := actionRunJobURL.FindStringSubmatch(raw)
	if m == nil {
		return "", "", 0, false
	}
	if m[3] != "" {
		jobID, _ = strconv.ParseInt(m[3], 10, 64)
	}
	return m[1], m[2], jobID, true
}

func jobFailed(j RunJob) bool {
	if s := strings.ToLower(j.Status); s != "" && s != "completed" {
		return false
	}
	switch strings.ToLower(j.Conclusion) {
	case "failure", "error", "timed_out", "startup_failure", "action_required":
		return true
	}
	return false
}

// matchCIRerunPatterns returns the first matching text snippet when text
// matches any compiled pattern, or ("", false) otherwise.
func matchCIRerunPatterns(patterns []*regexp.Regexp, text string) (string, bool) {
	if text == "" {
		return "", false
	}
	for _, re := range patterns {
		if loc := re.FindString(text); loc != "" {
			return shortTitle(oneLine(loc), 80), true
		}
	}
	return "", false
}

type ciRunRef struct {
	Repo  string
	RunID string
}

type ciJobMatch struct {
	Repo    string
	RunID   string
	JobID   int64
	JobName string
	Matched string
}

type ciFailureAnalysis struct {
	AllFlake       bool
	AnyInProgress  bool
	Runs           []ciRunRef
	Matches        []ciJobMatch
	NonFlakeReason string
}

// ciRerunEnv isolates all GitHub, Beads, autonomy, and guard dependencies so
// CI flake detection and bounded rerun logic can be tested deterministically.
type ciRerunEnv struct {
	runDetail   func(repo, runID string) (RunDetail, error)
	annotations func(repo string, jobID int64) ([]CheckAnnotation, error)
	failedLog   func(repo, runID string, jobID int64) (string, error)
	rerunFailed func(repo, runID string) error
	canRunLevel func(b Bead) (bool, string)
	enforce     func(b Bead, now time.Time) (prefix string, level int, class int, err error)
	note        func(id, text string) error
	journal     func(journalRecord) error
	held        func(id string) bool
	headless    func(id string) bool
	logf        func(format string, args ...any)
}

func (c Config) enforceCIRerun(b Bead, now time.Time) (string, int, int, error) {
	class := c.CIRerun.minLevel()
	lvl, why := c.beadLevel(b, autonomyStopped())
	prefix, err := allow(b.ID, "ticker", "ci-rerun", class, lvl, why)
	if err != nil {
		return "", lvl, class, err
	}
	recs, err := readJournal(now.Add(-budgetWindow))
	if err != nil {
		return "", lvl, class, err
	}
	if spent := budgetSpent(c.Autonomy.Budget, recs, b.ID, class, "ci-rerun"); spent != "" {
		return "", lvl, class, fmt.Errorf("%w: %s", errNotAutonomous, spent)
	}
	if err := appendJournal(journalRecord{At: now, Kind: journalCheck, Bead: b.ID, Class: class, Action: "ci-rerun", Pass: true, Text: "by ticker"}); err != nil {
		return "", lvl, class, err
	}
	return prefix, lvl, class, nil
}

func (t Ticker) liveCIRerunEnv() ciRerunEnv {
	return ciRerunEnv{
		runDetail:   t.gh.RunDetail,
		annotations: t.gh.CheckRunAnnotations,
		failedLog:   t.gh.RunFailedLog,
		rerunFailed: t.gh.RunRerunFailed,
		canRunLevel: func(b Bead) (bool, string) {
			class := t.cfg.CIRerun.minLevel()
			lvl, why := t.cfg.beadLevel(b, autonomyStopped())
			if lvl >= class {
				return true, ""
			}
			if why != "" {
				why = ", capped by " + why
			}
			return false, fmt.Sprintf("ci-rerun (min_level %d) is above %s's level %d%s", class, b.ID, lvl, why)
		},
		enforce: t.cfg.enforceCIRerun,
		note:    t.beads.Note,
		journal: appendJournal,
		held: func(id string) bool {
			_, ok := isJobHeld(id)
			return ok
		},
		headless: headlessOwned,
		logf:     t.log.Printf,
	}
}

func ciRerunBlockReason(th Thread, env ciRerunEnv) string {
	switch {
	case th.Bead.Status == StatusNeedsMe:
		return "bead is needs_me"
	case env.held != nil && env.held(th.Bead.ID):
		return "bead is on hold"
	case env.headless != nil && env.headless(th.Bead.ID):
		return "bead is headless-owned"
	default:
		return ""
	}
}

// inspectCIRerunFailure checks every failing check on pr and every failed job
// in each affected GitHub Actions workflow run. It returns AllFlake=true only
// when every failing check belongs to a GitHub Actions run and every failed
// job individually matches at least one pattern in patterns.
func inspectCIRerunFailure(pr *PR, patterns []*regexp.Regexp, env ciRerunEnv) ciFailureAnalysis {
	return inspectCIRerunFailureWith(pr, patterns, nil, env)
}

func inspectCIRerunFailureWith(pr *PR, patterns []*regexp.Regexp, ignore []string, env ciRerunEnv) ciFailureAnalysis {
	if pr == nil {
		return ciFailureAnalysis{NonFlakeReason: "no PR"}
	}
	var failing []Check
	for _, c := range pr.Checks {
		if !checkIgnored(c, ignore) && checkFailed(c) {
			failing = append(failing, c)
		}
	}
	if len(failing) == 0 {
		return ciFailureAnalysis{NonFlakeReason: "no failing checks"}
	}

	byRun := map[ciRunRef][]Check{}
	var runs []ciRunRef
	for _, c := range failing {
		repo, runID, _, ok := parseActionsRunURL(cmp.Or(c.DetailsURL, c.TargetURL))
		if !ok {
			return ciFailureAnalysis{NonFlakeReason: fmt.Sprintf("check %q is not a GitHub Actions run", c.label())}
		}
		ref := ciRunRef{Repo: repo, RunID: runID}
		if _, seen := byRun[ref]; !seen {
			runs = append(runs, ref)
		}
		byRun[ref] = append(byRun[ref], c)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Repo != runs[j].Repo {
			return runs[i].Repo < runs[j].Repo
		}
		return runs[i].RunID < runs[j].RunID
	})

	if env.runDetail == nil {
		return ciFailureAnalysis{NonFlakeReason: "run inspection unavailable"}
	}

	var matches []ciJobMatch
	anyInProgress := false
	for _, ref := range runs {
		detail, err := env.runDetail(ref.Repo, ref.RunID)
		if err != nil {
			return ciFailureAnalysis{NonFlakeReason: fmt.Sprintf("inspect run %s: %v", ref.RunID, err)}
		}
		if s := strings.ToLower(detail.Status); s != "" && s != "completed" {
			anyInProgress = true
		}
		if strings.EqualFold(detail.Status, "completed") && strings.EqualFold(detail.Conclusion, "success") {
			return ciFailureAnalysis{NonFlakeReason: fmt.Sprintf("run %s already completed with success", ref.RunID)}
		}

		// Gather every failed job reported by the run, plus any failing check
		// job ID from statusCheckRollup not listed in detail.Jobs.
		var failedJobs []RunJob
		seenJob := map[int64]bool{}
		for _, j := range detail.Jobs {
			if jobFailed(j) && !checkIgnored(Check{Name: j.Name}, ignore) {
				failedJobs = append(failedJobs, j)
				if j.ID > 0 {
					seenJob[j.ID] = true
				}
			}
		}
		for _, c := range byRun[ref] {
			_, _, jid, _ := parseActionsRunURL(cmp.Or(c.DetailsURL, c.TargetURL))
			if jid > 0 && !seenJob[jid] {
				seenJob[jid] = true
				failedJobs = append(failedJobs, RunJob{
					ID:         jid,
					Name:       c.label(),
					Status:     "completed",
					Conclusion: cmp.Or(c.Conclusion, c.State),
					URL:        cmp.Or(c.DetailsURL, c.TargetURL),
				})
			}
		}

		if len(failedJobs) == 0 {
			// Startup failures or jobless runs: inspect run/check conclusions,
			// descriptions, and run-level failed logs.
			matched := ""
			for _, candidate := range []string{detail.Conclusion} {
				if m, ok := matchCIRerunPatterns(patterns, candidate); ok {
					matched = m
					break
				}
			}
			if matched == "" {
				for _, c := range byRun[ref] {
					if m, ok := matchCIRerunPatterns(patterns, cmp.Or(c.Conclusion, c.State)+"\n"+c.Description); ok {
						matched = m
						break
					}
				}
			}
			if matched == "" && env.failedLog != nil {
				if logText, err := env.failedLog(ref.Repo, ref.RunID, 0); err == nil {
					if m, ok := matchCIRerunPatterns(patterns, logText); ok {
						matched = m
					}
				}
			}
			if matched == "" {
				return ciFailureAnalysis{NonFlakeReason: fmt.Sprintf("run %s failed without matching [ci_rerun] patterns", ref.RunID)}
			}
			matches = append(matches, ciJobMatch{
				Repo:    ref.Repo,
				RunID:   ref.RunID,
				JobName: cmp.Or(detail.WorkflowName, byRun[ref][0].label()),
				Matched: matched,
			})
			continue
		}

		for _, j := range failedJobs {
			matched, ok := matchFailedJob(ref, j, patterns, env)
			if !ok {
				label := cmp.Or(j.Name, fmt.Sprintf("job %d", j.ID))
				return ciFailureAnalysis{NonFlakeReason: fmt.Sprintf("job %q (run %s) did not match any [ci_rerun] pattern", label, ref.RunID)}
			}
			matches = append(matches, ciJobMatch{
				Repo:    ref.Repo,
				RunID:   ref.RunID,
				JobID:   j.ID,
				JobName: cmp.Or(j.Name, fmt.Sprintf("job %d", j.ID)),
				Matched: matched,
			})
		}
	}

	return ciFailureAnalysis{
		AllFlake:      true,
		AnyInProgress: anyInProgress,
		Runs:          runs,
		Matches:       matches,
	}
}

func matchFailedJob(ref ciRunRef, j RunJob, patterns []*regexp.Regexp, env ciRerunEnv) (string, bool) {
	if m, ok := matchCIRerunPatterns(patterns, j.Conclusion); ok {
		return m, true
	}
	if j.ID > 0 && env.annotations != nil {
		if anns, err := env.annotations(ref.Repo, j.ID); err == nil {
			for _, a := range anns {
				// Ignore informational/warning annotations (such as runtime
				// deprecation notices) so they never mask a real failure.
				if a.AnnotationLevel != "" && !strings.EqualFold(a.AnnotationLevel, "failure") {
					continue
				}
				if m, ok := matchCIRerunPatterns(patterns, a.Title+"\n"+a.Message); ok {
					return m, true
				}
			}
		}
	}
	if env.failedLog != nil {
		if logText, err := env.failedLog(ref.Repo, ref.RunID, j.ID); err == nil {
			if m, ok := matchCIRerunPatterns(patterns, logText); ok {
				return m, true
			}
		}
	}
	return "", false
}

type ciRerunResult struct {
	SuppressFailing bool
	EmitFailing     *Event
	FailingPrompt   string
}

func formatCIRunList(runs []ciRunRef) string {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.RunID
	}
	if len(ids) == 1 {
		return "run " + ids[0]
	}
	return "runs " + strings.Join(ids, ", ")
}

func formatCIMatchEvidence(matches []ciJobMatch) string {
	if len(matches) == 0 {
		return "matched transient CI flake pattern"
	}
	parts := make([]string, 0, min(3, len(matches)))
	for _, m := range matches[:min(3, len(matches))] {
		parts = append(parts, fmt.Sprintf("%s (%s)", m.JobName, m.Matched))
	}
	extra := ""
	if len(matches) > 3 {
		extra = fmt.Sprintf(" (+%d more)", len(matches)-3)
	}
	return "matched transient CI flake: " + strings.Join(parts, "; ") + extra
}

func ciFailingEvent(th Thread, summary string, now time.Time) Event {
	return Event{
		Bead:    th.Bead.ID,
		Kind:    EventFailing,
		Summary: summary,
		At:      now,
		Clears:  eventCondition(th, EventFailing),
	}
}

func hasEventKind(events []Event, kind EventKind) bool {
	return slices.ContainsFunc(events, func(e Event) bool { return e.Kind == kind })
}

// handleCIRerun evaluates and executes bounded auto-reruns for transient CI
// infrastructure flakes on th's tracked PR. It persists counter/dedup state in
// snap.CIRerun and returns whether transitionWith's checks_failing event and
// worker prompt should be suppressed or replaced.
func (t Ticker) handleCIRerun(th Thread, prev Snapshot, first, ghRefreshed, hadTransitionFailing bool, snap *Snapshot, now time.Time) ciRerunResult {
	var env ciRerunEnv
	if t.ciRerunEnv != nil {
		env = *t.ciRerunEnv
	} else {
		env = t.liveCIRerunEnv()
	}
	if env.logf == nil && t.log != nil {
		env.logf = t.log.Printf
	}
	return runCIRerun(t.cfg, env, th, prev, first, ghRefreshed, hadTransitionFailing, snap, now)
}

func runCIRerun(cfg Config, env ciRerunEnv, th Thread, prev Snapshot, first, ghRefreshed, hadTransitionFailing bool, snap *Snapshot, now time.Time) ciRerunResult {
	logf := env.logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	id := th.Bead.ID
	if th.PR == nil || th.PR.State != "OPEN" || th.PR.HeadSHA == "" {
		snap.CIRerun = nil
		return ciRerunResult{}
	}
	if !th.PR.DetailsFresh {
		snap.CIRerun = cloneCIRerunSnapshot(prev.CIRerun)
		return ciRerunResult{}
	}

	key := ciRerunPRKey(th.PR)
	st := ciRerunStateFrom(prev)
	if st.Key != "" && st.Key != key {
		// New head SHA or replacement PR: reset bounded rerun counter.
		st = CIRerunSnapshot{Key: key}
	}
	st.Key = key

	if first {
		if len(th.Checks.Failing) > 0 {
			fp := ciFailureFingerprintWith(th.PR, cfg.IgnoreChecks)
			st.LastFingerprint = fp
			st.SurfacedFingerprint = fp
			applyCIRerunSnapshot(snap, st)
		} else {
			snap.CIRerun = nil
		}
		return ciRerunResult{}
	}

	if len(th.Checks.Failing) == 0 {
		// Checks went pending or green: clear per-failure tracking while
		// retaining the (pr, head_sha) rerun counter.
		st.PendingRerun = false
		st.WaitingCompletion = false
		st.LastFingerprint = ""
		st.SurfacedFingerprint = ""
		if st.Reruns == 0 {
			snap.CIRerun = nil
		} else {
			applyCIRerunSnapshot(snap, st)
		}
		return ciRerunResult{}
	}

	fp := ciFailureFingerprintWith(th.PR, cfg.IgnoreChecks)
	wasSuppressed := st.PendingRerun || st.WaitingCompletion
	class := cfg.CIRerun.minLevel()

	surface := func(exhausted bool) ciRerunResult {
		st.PendingRerun = false
		st.WaitingCompletion = false
		st.LastFingerprint = fp
		st.SurfacedFingerprint = fp
		applyCIRerunSnapshot(snap, st)
		if exhausted {
			summary := fmt.Sprintf("PR #%d checks failing: %s (CI auto-rerun %d/%d exhausted)",
				th.PR.Number, strings.Join(th.Checks.Failing, ", "), st.Reruns, cfg.CIRerun.MaxReruns)
			ev := ciFailingEvent(th, summary, now)
			if env.journal != nil {
				_ = env.journal(journalRecord{
					At:     now,
					Kind:   journalCIRerun,
					Bead:   id,
					Class:  class,
					Action: "exhausted",
					Pass:   false,
					Text:   summary,
				})
			}
			logf("ci rerun %s: %s", id, summary)
			return ciRerunResult{
				SuppressFailing: true,
				EmitFailing:     &ev,
				FailingPrompt:   failingChecksPrompt(th.PR.Number, th.Checks.Failing),
			}
		}
		if hadTransitionFailing {
			return ciRerunResult{}
		}
		summary := fmt.Sprintf("PR #%d checks failing: %s", th.PR.Number, strings.Join(th.Checks.Failing, ", "))
		ev := ciFailingEvent(th, summary, now)
		return ciRerunResult{
			EmitFailing:   &ev,
			FailingPrompt: failingChecksPrompt(th.PR.Number, th.Checks.Failing),
		}
	}

	// Already surfaced for this exact failure fingerprint: do nothing and avoid
	// re-fetching GitHub logs.
	if st.SurfacedFingerprint == fp && !wasSuppressed {
		applyCIRerunSnapshot(snap, st)
		return ciRerunResult{}
	}

	// Pre-upgrade or already-reported unchanged failure with no prior suppression:
	// baseline its fingerprint without re-emitting or rerunning.
	if !hadTransitionFailing && !wasSuppressed && st.LastFingerprint == "" &&
		slices.Equal(th.Checks.Failing, prev.Failing) &&
		(prev.InboxPRKey == "" || prev.InboxPRKey == inboxPRKey(th)) {
		st.LastFingerprint = fp
		st.SurfacedFingerprint = fp
		applyCIRerunSnapshot(snap, st)
		return ciRerunResult{}
	}

	// Waiting for GitHub to pick up a rerun we already triggered for this exact
	// failure fingerprint.
	if st.PendingRerun && st.LastFingerprint == fp {
		if !st.LastRerunAt.IsZero() && now.Sub(st.LastRerunAt) >= ciRerunPendingTimeout {
			logf("ci rerun %s: PR #%d pending rerun did not clear after %s; surfacing checks_failing", id, th.PR.Number, ciRerunPendingTimeout)
			if env.journal != nil {
				_ = env.journal(journalRecord{
					At:     now,
					Kind:   journalCIRerun,
					Bead:   id,
					Class:  class,
					Action: "timeout",
					Pass:   false,
					Text:   fmt.Sprintf("PR #%d rerun did not clear after %s", th.PR.Number, ciRerunPendingTimeout),
				})
			}
			return surface(false)
		}
		applyCIRerunSnapshot(snap, st)
		return ciRerunResult{SuppressFailing: true}
	}

	// Waiting for an in-progress workflow run whose currently failing jobs all
	// matched flake patterns: skip re-inspecting on cached ticks between GH polls.
	if st.WaitingCompletion && st.LastFingerprint == fp && !ghRefreshed && th.Checks.Pending > 0 {
		applyCIRerunSnapshot(snap, st)
		return ciRerunResult{SuppressFailing: true}
	}

	if !cfg.CIRerun.enabled() {
		if wasSuppressed || hadTransitionFailing {
			return surface(false)
		}
		st.LastFingerprint = fp
		st.SurfacedFingerprint = fp
		applyCIRerunSnapshot(snap, st)
		return ciRerunResult{}
	}

	if reason := ciRerunBlockReason(th, env); reason != "" {
		logf("ci rerun %s: skipped PR #%d (%s)", id, th.PR.Number, reason)
		return surface(false)
	}

	if st.Reruns >= cfg.CIRerun.MaxReruns {
		return surface(true)
	}

	if env.canRunLevel != nil {
		if ok, why := env.canRunLevel(th.Bead); !ok {
			logf("ci rerun %s: skipped PR #%d (%s)", id, th.PR.Number, why)
			return surface(false)
		}
	}

	patterns, err := cfg.CIRerun.compilePatterns()
	if err != nil {
		logf("ci rerun %s: invalid patterns: %v", id, err)
		return surface(false)
	}

	analysis := inspectCIRerunFailureWith(th.PR, patterns, cfg.IgnoreChecks, env)
	if !analysis.AllFlake {
		logf("ci rerun %s: PR #%d not a pure flake (%s)", id, th.PR.Number, analysis.NonFlakeReason)
		return surface(false)
	}

	if analysis.AnyInProgress {
		st.WaitingCompletion = true
		st.PendingRerun = false
		st.LastFingerprint = fp
		st.SurfacedFingerprint = ""
		applyCIRerunSnapshot(snap, st)
		logf("ci rerun %s: PR #%d failing jobs match flake patterns; waiting for in-progress run(s) to finish", id, th.PR.Number)
		return ciRerunResult{SuppressFailing: true}
	}

	if st.Reruns+len(analysis.Runs) > cfg.CIRerun.MaxReruns {
		return surface(true)
	}

	autoPrefix := fmt.Sprintf("AUTO: [ticker L%d C%d]", class, class)
	if env.enforce != nil {
		prefix, _, enforcedClass, err := env.enforce(th.Bead, now)
		if err != nil {
			logf("ci rerun %s: autonomy denied PR #%d: %v", id, th.PR.Number, err)
			if env.journal != nil {
				_ = env.journal(journalRecord{
					At:     now,
					Kind:   journalCIRerun,
					Bead:   id,
					Class:  enforcedClass,
					Action: "autonomy_denied",
					Pass:   false,
					Text:   err.Error(),
				})
			}
			return surface(false)
		}
		if prefix != "" {
			autoPrefix = prefix
		}
		class = enforcedClass
	}

	if env.rerunFailed == nil {
		return surface(false)
	}

	var succeeded []ciRunRef
	var rerunErr error
	for _, r := range analysis.Runs {
		if err := env.rerunFailed(r.Repo, r.RunID); err != nil {
			rerunErr = fmt.Errorf("rerun %s (%s): %w", r.RunID, r.Repo, err)
			break
		}
		succeeded = append(succeeded, r)
		st.Reruns++
	}

	if len(succeeded) > 0 {
		autoNote := fmt.Sprintf("%s reran failed CI jobs for PR #%d at %s (%d/%d, %s) — %s",
			autoPrefix, th.PR.Number, short(th.PR.HeadSHA), st.Reruns, cfg.CIRerun.MaxReruns,
			formatCIRunList(succeeded), formatCIMatchEvidence(analysis.Matches))
		if env.note != nil {
			if err := env.note(id, autoNote); err != nil {
				logf("ci rerun %s: note: %v", id, err)
			} else {
				snap.Auto++
			}
		}
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalAuto, Bead: id, Class: class, Action: "ci_rerun", Pass: true, Text: autoNote})
			_ = env.journal(journalRecord{At: now, Kind: journalCIRerun, Bead: id, Class: class, Action: "rerun", Pass: true, Text: autoNote})
		}
		logf("ci rerun %s: %s", id, autoNote)
	}

	if rerunErr != nil {
		logf("ci rerun %s: %v", id, rerunErr)
		if env.journal != nil {
			_ = env.journal(journalRecord{At: now, Kind: journalCIRerun, Bead: id, Class: class, Action: "rerun_failed", Pass: false, Text: rerunErr.Error()})
		}
		return surface(false)
	}

	st.PendingRerun = true
	st.WaitingCompletion = false
	st.LastFingerprint = fp
	st.LastRerunAt = now
	st.SurfacedFingerprint = ""
	applyCIRerunSnapshot(snap, st)
	return ciRerunResult{SuppressFailing: true}
}
