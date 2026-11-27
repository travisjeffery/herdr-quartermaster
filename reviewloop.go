package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// Bounded implementer <-> skeptical reviewer ping-pong loop.
//
// Each implementer bead's review loop state persists atomically in
// <state>/review-loops/<bead>.json (mode 0600 inside a 0700 directory) and is
// mirrored to structured bead notes (REVIEW REQUEST / REVIEW VERDICT / REVIEW
// FEEDBACK / REVIEW ESCALATED) and journal.jsonl.
//
// Guarantees:
//   - Head-SHA deduplication: the same 40-char commit SHA is never reviewed
//     twice (whether pending, already passed, or awaiting fixes after
//     cannot_merge).
//   - Hard round cap: back-and-forth is bounded by max_rounds (default 2,
//     configurable 1..5). Reaching max_rounds on a cannot_merge verdict
//     escalates cleanly to needs_me with a structured QUESTION/OPTIONS/CLASS
//     note instead of spinning forever. Automated callers cannot reset the cap.
//   - Worktree-free reviewer integration: a companion role:reviewer bead runs
//     without creating a git worktree and routes structured blocking findings
//     back to the implementer via quartermaster tell.

const (
	reviewLoopSchemaVersion = 1
	defaultReviewMaxRounds  = 2
	maxAllowedReviewRounds  = 5

	reviewVerdictCanMerge    = "can_merge"
	reviewVerdictCannotMerge = "cannot_merge"

	reviewStatePending   = "pending"
	reviewStateApproved  = "can_merge"
	reviewStateChanges   = "changes_requested"
	reviewStateEscalated = "escalated"

	reviewHeaderPrefix = "[" + toolName + ": automated, not the user]"
)

var errReviewLoop = errors.New("review loop")

// ReviewFinding is one concrete blocking defect reported by a skeptical reviewer.
type ReviewFinding struct {
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Summary     string `json:"summary"`
	Consequence string `json:"consequence,omitempty"`
}

func (f ReviewFinding) String() string {
	loc := ""
	switch {
	case f.File != "" && f.Line > 0:
		loc = fmt.Sprintf("%s:%d: ", f.File, f.Line)
	case f.File != "":
		loc = f.File + ": "
	}
	s := loc + strings.TrimSpace(f.Summary)
	if c := strings.TrimSpace(f.Consequence); c != "" {
		s += " — " + c
	}
	return s
}

// ReviewRound records one bounded review pass against a single head commit SHA.
type ReviewRound struct {
	Round       int             `json:"round"`
	HeadSHA     string          `json:"head_sha"`
	PRURL       string          `json:"pr_url,omitempty"`
	Reviewer    string          `json:"reviewer_bead,omitempty"`
	RequestedAt time.Time       `json:"requested_at"`
	RequestedBy string          `json:"requested_by,omitempty"`
	Verdict     string          `json:"verdict,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Findings    []ReviewFinding `json:"findings,omitempty"`
	Later       []string        `json:"later,omitempty"`
	VerdictAt   time.Time       `json:"verdict_at,omitzero"`
	VerdictBy   string          `json:"verdict_by,omitempty"`
	RoutedAt    time.Time       `json:"routed_at,omitzero"`
	RouteResult string          `json:"route_result,omitempty"`
}

// ReviewLoopState is the durable ping-pong state for one implementer bead.
type ReviewLoopState struct {
	Version         int           `json:"version"`
	Bead            string        `json:"bead"`
	ReviewerBead    string        `json:"reviewer_bead,omitempty"`
	MaxRounds       int           `json:"max_rounds"`
	State           string        `json:"state"`
	CurrentRound    int           `json:"current_round"`
	LastHeadSHA     string        `json:"last_head_sha,omitempty"`
	PassedHeadSHA   string        `json:"passed_head_sha,omitempty"`
	NotifiedHeadSHA string        `json:"notified_head_sha,omitempty"`
	EscalatedAt     time.Time     `json:"escalated_at,omitzero"`
	EscalatedWhy    string        `json:"escalated_why,omitempty"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Rounds          []ReviewRound `json:"rounds,omitempty"`
}

func reviewLoopsDir() string { return filepath.Join(stateDir(), "review-loops") }

func reviewLoopPath(id string) (string, error) {
	if !beadName.MatchString(id) {
		return "", fmt.Errorf("%w: invalid bead id %q", errReviewLoop, id)
	}
	return filepath.Join(reviewLoopsDir(), id+".json"), nil
}

func lockReviewLoop(id string) (func(), error) {
	if !beadName.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid bead id %q", errReviewLoop, id)
	}
	dir := filepath.Join(runDir(), "locks")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "review-"+id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: review loop for %s is already updating", errBusy, id)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func loadReviewLoop(id string) (ReviewLoopState, bool, error) {
	p, err := reviewLoopPath(id)
	if err != nil {
		return ReviewLoopState{}, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ReviewLoopState{}, false, nil
	}
	if err != nil {
		return ReviewLoopState{}, false, err
	}
	var st ReviewLoopState
	if err := json.Unmarshal(b, &st); err != nil {
		return ReviewLoopState{}, false, fmt.Errorf("%w: parse %s: %v", errReviewLoop, p, err)
	}
	if st.Version == 0 {
		st.Version = reviewLoopSchemaVersion
	}
	if st.MaxRounds <= 0 {
		st.MaxRounds = defaultReviewMaxRounds
	}
	return st, true, nil
}

func saveReviewLoop(st ReviewLoopState) error {
	p, err := reviewLoopPath(st.Bead)
	if err != nil {
		return err
	}
	if st.Version == 0 {
		st.Version = reviewLoopSchemaVersion
	}
	if st.MaxRounds <= 0 {
		st.MaxRounds = defaultReviewMaxRounds
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicPrivate(p, append(b, '\n'))
}

func listReviewLoops() ([]ReviewLoopState, error) {
	entries, err := os.ReadDir(reviewLoopsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ReviewLoopState
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		st, ok, err := loadReviewLoop(id)
		if err == nil && ok {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bead < out[j].Bead })
	return out, nil
}

// reviewLoopContextLine returns a compact single-line summary for quartermaster
// context output, or "" when bead id has no recorded review loop.
func reviewLoopContextLine(id string) string {
	st, ok, err := loadReviewLoop(id)
	if err != nil || !ok || st.State == "" {
		return ""
	}
	head := short(st.LastHeadSHA)
	if head == "" {
		head = short(st.PassedHeadSHA)
	}
	s := fmt.Sprintf("round %d/%d %s", st.CurrentRound, st.MaxRounds, st.State)
	if head != "" {
		s += " @" + head
	}
	if st.ReviewerBead != "" {
		s += " (reviewer " + st.ReviewerBead + ")"
	}
	return s
}

func normalizeReviewVerdict(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	v = strings.ReplaceAll(v, "-", "_")
	v = strings.Join(strings.Fields(v), "_")
	switch v {
	case reviewVerdictCanMerge, "pass", "passed", "ok":
		return reviewVerdictCanMerge, nil
	case reviewVerdictCannotMerge, "fail", "failed", "block", "blocked":
		return reviewVerdictCannotMerge, nil
	default:
		return "", fmt.Errorf("%w: unknown verdict %q (want can_merge or cannot_merge)", errReviewLoop, raw)
	}
}

func normalizeHeadSHA(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if !fullSHA.MatchString(s) {
		return "", fmt.Errorf("%w: head commit must be a 40-character hex SHA, got %q", errReviewLoop, raw)
	}
	return s, nil
}

// parseFindingFlag parses "file:line: summary — consequence", "file: summary",
// or a plain defect summary into a structured ReviewFinding.
func parseFindingFlag(raw string) (ReviewFinding, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ReviewFinding{}, fmt.Errorf("%w: empty finding", errReviewLoop)
	}
	var f ReviewFinding
	body := s
	if idx := strings.Index(s, ": "); idx > 0 {
		prefix := strings.TrimSpace(s[:idx])
		rest := strings.TrimSpace(s[idx+2:])
		if !strings.ContainsAny(prefix, " \t") && rest != "" {
			if colon := strings.LastIndex(prefix, ":"); colon > 0 {
				if line, err := strconv.Atoi(prefix[colon+1:]); err == nil && line > 0 {
					f.File = prefix[:colon]
					f.Line = line
					body = rest
				}
			}
			if f.File == "" && (strings.Contains(prefix, "/") || strings.Contains(prefix, ".")) {
				f.File = prefix
				body = rest
			}
		}
	}
	if sum, cons, ok := strings.Cut(body, " — "); ok {
		f.Summary = strings.TrimSpace(sum)
		f.Consequence = strings.TrimSpace(cons)
	} else {
		f.Summary = strings.TrimSpace(body)
	}
	if f.Summary == "" {
		return ReviewFinding{}, fmt.Errorf("%w: finding missing summary: %q", errReviewLoop, raw)
	}
	return f, nil
}

// reviewHeadInfo holds resolved git/PR target metadata for a review pass.
type reviewHeadInfo struct {
	HeadSHA string
	PRURL   string
	Branch  string
	Workdir string
}

// reviewLoopEnv injects external dependencies so ping-pong review state,
// reviewer dispatch, implementer tell routing, and escalation are fully
// testable without live bd, herdr, or git subprocesses.
type reviewLoopEnv struct {
	show             func(id string) (Bead, error)
	note             func(id, text string) error
	setStatus        func(id, status string) error
	createBead       func(opts BeadCreateOpts) (Bead, error)
	resolveHead      func(b Bead) (reviewHeadInfo, error)
	tell             func(id, text string) (string, error)
	dispatchReviewer func(id, agent, by string) (string, error)
	enforce          func(b Bead, action, by string) (string, error)
	event            func(Event) error
	journal          func(journalRecord) error
	now              func() time.Time
}

func liveReviewLoopEnv(cfg Config, h Herdr) reviewLoopEnv {
	hh := h.on(cfg.coordSocket())
	bd := Beads{}
	return reviewLoopEnv{
		show:       bd.Show,
		note:       bd.Note,
		setStatus:  bd.SetStatus,
		createBead: bd.Create,
		resolveHead: func(b Bead) (reviewHeadInfo, error) {
			return liveResolveReviewHead(cfg, hh, b)
		},
		tell: func(id, text string) (string, error) {
			return tellCmd(cfg, hh, bd.Show, id, text)
		},
		dispatchReviewer: func(id, agent, by string) (string, error) {
			return dispatch(cfg, hh, id, DispatchOpts{Kind: agent, Role: roleReviewer, By: by})
		},
		enforce: func(b Bead, action, by string) (string, error) {
			return cfg.enforce(b, action, by)
		},
		event:   writeEvent,
		journal: appendJournal,
		now:     time.Now,
	}
}

func (e reviewLoopEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func liveResolveReviewHead(cfg Config, h Herdr, b Bead) (reviewHeadInfo, error) {
	var info reviewHeadInfo
	st := loadState()
	if pr, ok := st.PRs[b.ID]; ok {
		info.PRURL = pr.URL
		info.Branch = pr.Head
		if fullSHA.MatchString(strings.ToLower(pr.HeadSHA)) {
			info.HeadSHA = strings.ToLower(pr.HeadSHA)
		}
	}
	if wt, _, err := linkedWorktree(cfg, h, b); err == nil && wt != nil {
		info.Workdir = wt.Path
		if info.Branch == "" {
			info.Branch = wt.Branch
		}
		if info.HeadSHA == "" {
			if sha, err := gitHeadSHA(wt.Path); err == nil {
				info.HeadSHA = sha
			}
		}
	}
	if info.HeadSHA == "" {
		return info, fmt.Errorf("%w: could not resolve 40-char head SHA for %s (pass --head <sha>)", errReviewLoop, b.ID)
	}
	return info, nil
}

func gitHeadSHA(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return normalizeHeadSHA(string(out))
}

func gitWorktreeClean(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	return err == nil && len(bytes.TrimSpace(out)) == 0
}

// ReviewRequestOpts configures `quartermaster review request <bead>`.
type ReviewRequestOpts struct {
	HeadSHA       string
	PRURL         string
	ReviewerBead  string
	SpawnReviewer bool
	Agent         string
	MaxRounds     int
	Reset         bool
	By            string
}

// ReviewRequestResult describes the outcome of requesting a bounded review round.
type ReviewRequestResult struct {
	State          ReviewLoopState `json:"state"`
	Deduplicated   bool            `json:"deduplicated,omitempty"`
	SpawnedBead    string          `json:"spawned_bead,omitempty"`
	ReviewerAction string          `json:"reviewer_action,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Message        string          `json:"message"`
}

// requestReview starts (or idempotently deduplicates) one bounded skeptical
// review pass for implementer bead id at head SHA.
func requestReview(cfg Config, env reviewLoopEnv, id string, opts ReviewRequestOpts) (ReviewRequestResult, error) {
	unlock, err := lockReviewLoop(id)
	if err != nil {
		return ReviewRequestResult{}, err
	}
	defer unlock()

	if env.show == nil {
		return ReviewRequestResult{}, fmt.Errorf("%w: bead lookup unavailable", errReviewLoop)
	}
	b, err := env.show(id)
	if err != nil {
		return ReviewRequestResult{}, err
	}
	if b.Status == StatusClosed {
		return ReviewRequestResult{}, fmt.Errorf("%w: %s is closed", errReviewLoop, id)
	}
	if opts.MaxRounds < 0 || opts.MaxRounds > maxAllowedReviewRounds {
		return ReviewRequestResult{}, fmt.Errorf("%w: --max-rounds must be 1-%d", errReviewLoop, maxAllowedReviewRounds)
	}

	var info reviewHeadInfo
	if env.resolveHead != nil {
		if resolved, rerr := env.resolveHead(b); rerr == nil {
			info = resolved
		} else if opts.HeadSHA == "" {
			return ReviewRequestResult{}, rerr
		}
	}
	if opts.HeadSHA != "" {
		sha, err := normalizeHeadSHA(opts.HeadSHA)
		if err != nil {
			return ReviewRequestResult{}, err
		}
		info.HeadSHA = sha
	}
	if info.HeadSHA == "" {
		return ReviewRequestResult{}, fmt.Errorf("%w: head commit SHA is required (pass --head <40-char-sha>)", errReviewLoop)
	}
	if opts.PRURL != "" {
		info.PRURL = strings.TrimSpace(opts.PRURL)
	}

	now := env.clock()
	st, exists, err := loadReviewLoop(id)
	if err != nil {
		return ReviewRequestResult{}, err
	}
	if !exists {
		maxR := opts.MaxRounds
		if maxR == 0 {
			maxR = defaultReviewMaxRounds
		}
		st = ReviewLoopState{
			Version:   reviewLoopSchemaVersion,
			Bead:      id,
			MaxRounds: maxR,
		}
	} else if opts.MaxRounds > 0 {
		st.MaxRounds = opts.MaxRounds
	}

	by := strings.TrimSpace(opts.By)
	if by == "" {
		by = "coordinator"
	}

	if opts.Reset {
		if by != "TJ" {
			return ReviewRequestResult{}, fmt.Errorf("%w: --reset requires --by TJ; automated callers cannot bypass the review round cap", errReviewLoop)
		}
		st.State = ""
		st.CurrentRound = 0
		st.LastHeadSHA = ""
		st.PassedHeadSHA = ""
		st.NotifiedHeadSHA = ""
		st.EscalatedAt = time.Time{}
		st.EscalatedWhy = ""
	}

	if st.State == reviewStateEscalated {
		return ReviewRequestResult{}, fmt.Errorf("%w: %s escalated to needs_me after %d/%d rounds; resolve needs_me or rerun with --reset --by TJ",
			errReviewLoop, id, st.CurrentRound, st.MaxRounds)
	}
	if st.State == reviewStatePending && st.LastHeadSHA == info.HeadSHA {
		msg := fmt.Sprintf("review round %d/%d already pending for %s at %s", st.CurrentRound, st.MaxRounds, id, short(info.HeadSHA))
		return ReviewRequestResult{State: st, Deduplicated: true, Message: msg}, nil
	}
	if st.State == reviewStateApproved && st.PassedHeadSHA == info.HeadSHA {
		msg := fmt.Sprintf("%s already passed skeptical review (can_merge) at %s in round %d/%d", id, short(info.HeadSHA), st.CurrentRound, st.MaxRounds)
		return ReviewRequestResult{State: st, Deduplicated: true, Message: msg}, nil
	}
	if st.State == reviewStateChanges && st.LastHeadSHA == info.HeadSHA {
		return ReviewRequestResult{}, fmt.Errorf("%w: head %s already received cannot_merge in round %d/%d; commit/push a new head SHA before starting round %d/%d",
			errReviewLoop, short(info.HeadSHA), st.CurrentRound, st.MaxRounds, st.CurrentRound+1, st.MaxRounds)
	}
	if st.CurrentRound >= st.MaxRounds {
		why := fmt.Sprintf("review round cap (%d/%d) reached before reviewing %s", st.CurrentRound, st.MaxRounds, short(info.HeadSHA))
		if escErr := escalateReviewLoop(env, b, &st, info.HeadSHA, why, now); escErr != nil {
			return ReviewRequestResult{}, escErr
		}
		return ReviewRequestResult{}, fmt.Errorf("%w: %s; escalated %s to needs_me", errReviewLoop, why, id)
	}

	nextRound := st.CurrentRound + 1
	prompt := buildReviewPassPrompt(b, st, nextRound, info)

	revBead := strings.TrimSpace(opts.ReviewerBead)
	if revBead == "" {
		revBead = st.ReviewerBead
	}
	// If the previously linked reviewer bead was closed, allow spawning a fresh one.
	if revBead != "" && opts.SpawnReviewer && env.show != nil {
		if rb, rerr := env.show(revBead); rerr == nil && rb.Status == StatusClosed {
			revBead = ""
		}
	}

	var spawnedBead string
	var reviewerAction string
	if revBead == "" && opts.SpawnReviewer {
		if env.createBead == nil {
			return ReviewRequestResult{}, fmt.Errorf("%w: cannot spawn reviewer bead (bead creation unavailable)", errReviewLoop)
		}
		if env.enforce != nil && by != "TJ" {
			if _, err := env.enforce(b, "dispatch", by); err != nil {
				return ReviewRequestResult{}, err
			}
		}
		labels := []string{roleLabel + roleReviewer}
		if repoName, ok := cfg.repoName(b); ok && repoName != "" {
			labels = append(labels, repoLabel+repoName)
		}
		// Never parent the reviewer under the implementer bead: open children
		// prevent the implementer from transitioning to merged on PR merge.
		created, err := env.createBead(BeadCreateOpts{
			Title:       fmt.Sprintf("Review %s (%s)", shortTitle(b.Title, 48), b.ID),
			Type:        "task",
			Priority:    b.Priority,
			Parent:      b.Parent,
			Labels:      labels,
			Description: buildReviewerBeadDescription(b, info, nextRound, st.MaxRounds),
		})
		if err != nil {
			return ReviewRequestResult{}, fmt.Errorf("create reviewer bead for %s: %w", id, err)
		}
		revBead = created.ID
		spawnedBead = created.ID
	}

	if revBead != "" {
		if env.note != nil {
			_ = env.note(revBead, prompt)
		}
		told := false
		if env.tell != nil {
			if msg, terr := env.tell(revBead, prompt); terr == nil {
				reviewerAction = "told reviewer " + revBead + ": " + msg
				told = true
			}
		}
		if !told && env.dispatchReviewer != nil {
			dispatchBy := by
			if spawnedBead != "" && by != "TJ" {
				// Already enforced and journaled the dispatch check before
				// creating the reviewer bead above.
				dispatchBy = "TJ"
			}
			msg, derr := env.dispatchReviewer(revBead, opts.Agent, dispatchBy)
			if derr != nil {
				return ReviewRequestResult{}, fmt.Errorf("dispatch reviewer %s: %w", revBead, derr)
			}
			reviewerAction = "dispatched reviewer " + revBead + ": " + msg
		}
		st.ReviewerBead = revBead
	}

	round := ReviewRound{
		Round:       nextRound,
		HeadSHA:     info.HeadSHA,
		PRURL:       info.PRURL,
		Reviewer:    revBead,
		RequestedAt: now,
		RequestedBy: by,
	}
	st.CurrentRound = nextRound
	st.State = reviewStatePending
	st.LastHeadSHA = info.HeadSHA
	st.NotifiedHeadSHA = info.HeadSHA
	st.UpdatedAt = now
	st.Rounds = append(st.Rounds, round)
	if err := saveReviewLoop(st); err != nil {
		return ReviewRequestResult{}, err
	}

	noteText := fmt.Sprintf("REVIEW REQUEST: round %d/%d head=%s", nextRound, st.MaxRounds, short(info.HeadSHA))
	if revBead != "" {
		noteText += " reviewer=" + revBead
	}
	if info.PRURL != "" {
		noteText += " pr=" + info.PRURL
	}
	if env.note != nil {
		_ = env.note(id, noteText)
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalReview,
			Bead:   id,
			Class:  classHousekeeping,
			Action: "request",
			Pass:   true,
			Text:   noteText,
		})
	}

	msg := fmt.Sprintf("started review round %d/%d for %s at %s", nextRound, st.MaxRounds, id, short(info.HeadSHA))
	if reviewerAction != "" {
		msg += " (" + reviewerAction + ")"
	}
	return ReviewRequestResult{
		State:          st,
		SpawnedBead:    spawnedBead,
		ReviewerAction: reviewerAction,
		Prompt:         prompt,
		Message:        msg,
	}, nil
}

// ReviewVerdictInput captures a structured skeptical reviewer verdict.
type ReviewVerdictInput struct {
	Verdict  string          `json:"verdict"`
	HeadSHA  string          `json:"head_sha"`
	Summary  string          `json:"summary"`
	Findings []ReviewFinding `json:"findings,omitempty"`
	Later    []string        `json:"later,omitempty"`
	By       string          `json:"by,omitempty"`
	NoRoute  bool            `json:"no_route,omitempty"`
}

// ReviewVerdictResult describes the persisted and routed outcome of a verdict.
type ReviewVerdictResult struct {
	State       ReviewLoopState `json:"state"`
	Verdict     string          `json:"verdict"`
	Routed      bool            `json:"routed,omitempty"`
	Escalated   bool            `json:"escalated,omitempty"`
	RouteResult string          `json:"route_result,omitempty"`
	Message     string          `json:"message"`
}

// recordReviewVerdict validates and persists a skeptical review verdict, routes
// blocking findings back to the implementer via quartermaster tell when under
// max_rounds, or escalates cleanly to needs_me when max_rounds is reached.
func recordReviewVerdict(env reviewLoopEnv, id string, in ReviewVerdictInput) (ReviewVerdictResult, error) {
	unlock, err := lockReviewLoop(id)
	if err != nil {
		return ReviewVerdictResult{}, err
	}
	defer unlock()

	verdict, err := normalizeReviewVerdict(in.Verdict)
	if err != nil {
		return ReviewVerdictResult{}, err
	}
	headSHA, err := normalizeHeadSHA(in.HeadSHA)
	if err != nil {
		return ReviewVerdictResult{}, err
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return ReviewVerdictResult{}, fmt.Errorf("%w: --summary is required", errReviewLoop)
	}
	var findings []ReviewFinding
	for _, f := range in.Findings {
		if strings.TrimSpace(f.Summary) != "" {
			findings = append(findings, ReviewFinding{
				File:        strings.TrimSpace(f.File),
				Line:        f.Line,
				Summary:     strings.TrimSpace(f.Summary),
				Consequence: strings.TrimSpace(f.Consequence),
			})
		}
	}
	if verdict == reviewVerdictCannotMerge && len(findings) == 0 {
		return ReviewVerdictResult{}, fmt.Errorf("%w: cannot_merge verdict requires at least one concrete blocking --finding", errReviewLoop)
	}
	var later []string
	for _, l := range in.Later {
		if l = strings.TrimSpace(l); l != "" {
			later = append(later, l)
		}
	}

	if env.show == nil {
		return ReviewVerdictResult{}, fmt.Errorf("%w: bead lookup unavailable", errReviewLoop)
	}
	b, err := env.show(id)
	if err != nil {
		return ReviewVerdictResult{}, err
	}
	if b.Status == StatusClosed {
		return ReviewVerdictResult{}, fmt.Errorf("%w: %s is closed", errReviewLoop, id)
	}

	now := env.clock()
	st, exists, err := loadReviewLoop(id)
	if err != nil {
		return ReviewVerdictResult{}, err
	}
	if !exists {
		st = ReviewLoopState{
			Version:   reviewLoopSchemaVersion,
			Bead:      id,
			MaxRounds: defaultReviewMaxRounds,
		}
	}
	if st.State == reviewStateEscalated {
		return ReviewVerdictResult{}, fmt.Errorf("%w: %s is already escalated to needs_me after %d/%d rounds", errReviewLoop, id, st.CurrentRound, st.MaxRounds)
	}

	by := strings.TrimSpace(in.By)
	if by == "" {
		by = roleReviewer
	}

	var roundIdx int
	if st.State == reviewStatePending && len(st.Rounds) > 0 {
		if st.LastHeadSHA != headSHA {
			return ReviewVerdictResult{}, fmt.Errorf("%w: stale verdict head %s; pending round %d/%d is for %s",
				errReviewLoop, short(headSHA), st.CurrentRound, st.MaxRounds, short(st.LastHeadSHA))
		}
		roundIdx = len(st.Rounds) - 1
	} else {
		for _, prev := range st.Rounds {
			if prev.HeadSHA == headSHA && prev.Verdict != "" {
				return ReviewVerdictResult{}, fmt.Errorf("%w: head %s already recorded verdict %s in round %d/%d",
					errReviewLoop, short(headSHA), prev.Verdict, prev.Round, st.MaxRounds)
			}
		}
		if st.CurrentRound >= st.MaxRounds {
			return ReviewVerdictResult{}, fmt.Errorf("%w: review round limit (%d/%d) reached; cannot record additional round without --reset --by TJ",
				errReviewLoop, st.CurrentRound, st.MaxRounds)
		}
		st.CurrentRound++
		st.Rounds = append(st.Rounds, ReviewRound{
			Round:       st.CurrentRound,
			HeadSHA:     headSHA,
			Reviewer:    st.ReviewerBead,
			RequestedAt: now,
			RequestedBy: by,
		})
		roundIdx = len(st.Rounds) - 1
	}

	r := &st.Rounds[roundIdx]
	r.Verdict = verdict
	r.Summary = summary
	r.Findings = findings
	r.Later = later
	r.VerdictAt = now
	r.VerdictBy = by
	st.LastHeadSHA = headSHA
	st.NotifiedHeadSHA = headSHA
	st.UpdatedAt = now

	verdictLabel := "can merge"
	if verdict == reviewVerdictCannotMerge {
		verdictLabel = "cannot merge"
	}
	var noteBuf strings.Builder
	fmt.Fprintf(&noteBuf, "REVIEW VERDICT: %s round=%d/%d head=%s — %s", verdictLabel, r.Round, st.MaxRounds, short(headSHA), summary)
	for i, f := range findings {
		fmt.Fprintf(&noteBuf, "\n  %d. %s", i+1, f.String())
	}
	if len(later) > 0 {
		fmt.Fprintf(&noteBuf, "\n  Later: %s", strings.Join(later, "; "))
	}
	if env.note != nil {
		_ = env.note(id, noteBuf.String())
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalReview,
			Bead:   id,
			Class:  0,
			Action: "verdict:" + verdict,
			Pass:   verdict == reviewVerdictCanMerge,
			Text:   fmt.Sprintf("round %d/%d head=%s: %s", r.Round, st.MaxRounds, short(headSHA), shortTitle(summary, 140)),
		})
	}

	res := ReviewVerdictResult{Verdict: verdict}

	if verdict == reviewVerdictCanMerge {
		st.State = reviewStateApproved
		st.PassedHeadSHA = headSHA
		if !in.NoRoute && env.tell != nil {
			passedMsg := buildReviewPassedPrompt(b, *r, st.MaxRounds)
			if routeMsg, terr := env.tell(id, passedMsg); terr == nil {
				r.RoutedAt = now
				r.RouteResult = routeMsg
				res.Routed = true
				res.RouteResult = routeMsg
			} else {
				r.RouteResult = "tell skipped: " + terr.Error()
				res.RouteResult = r.RouteResult
			}
		}
		if err := saveReviewLoop(st); err != nil {
			return ReviewVerdictResult{}, err
		}
		if env.event != nil {
			_ = env.event(Event{
				Bead:    id,
				Kind:    EventReviewPassed,
				Summary: fmt.Sprintf("%s passed skeptical review round %d/%d at %s (can merge): %s", id, r.Round, st.MaxRounds, short(headSHA), shortTitle(summary, 120)),
				At:      now,
				Clears:  &InboxCondition{Kind: "bead_closed", Bead: id, Key: headSHA},
			})
		}
		res.State = st
		res.Message = fmt.Sprintf("recorded can_merge for %s round %d/%d at %s", id, r.Round, st.MaxRounds, short(headSHA))
		return res, nil
	}

	// cannot_merge: escalate once max_rounds is reached, else route findings back via tell.
	if st.CurrentRound >= st.MaxRounds {
		why := fmt.Sprintf("reached max %d/%d review rounds with verdict cannot merge at %s (%s)",
			st.CurrentRound, st.MaxRounds, short(headSHA), shortTitle(summary, 100))
		r.RouteResult = "escalated to needs_me (max rounds reached)"
		if err := escalateReviewLoop(env, b, &st, headSHA, why, now); err != nil {
			return ReviewVerdictResult{}, err
		}
		res.State = st
		res.Escalated = true
		res.RouteResult = r.RouteResult
		res.Message = fmt.Sprintf("recorded cannot_merge for %s round %d/%d at %s; max rounds reached, escalated to needs_me",
			id, r.Round, st.MaxRounds, short(headSHA))
		return res, nil
	}

	st.State = reviewStateChanges
	if !in.NoRoute {
		feedbackPrompt := buildReviewFeedbackPrompt(b, *r, st.MaxRounds)
		if env.tell != nil {
			routeMsg, terr := env.tell(id, feedbackPrompt)
			if terr == nil {
				r.RoutedAt = now
				r.RouteResult = routeMsg
				res.Routed = true
				res.RouteResult = routeMsg
				if env.note != nil {
					_ = env.note(id, fmt.Sprintf("REVIEW FEEDBACK: round %d/%d head=%s routed to %s (%d blocking finding(s))",
						r.Round, st.MaxRounds, short(headSHA), id, len(findings)))
				}
			} else {
				r.RouteResult = "tell failed: " + terr.Error()
				res.RouteResult = r.RouteResult
				if env.event != nil {
					_ = env.event(Event{
						Bead:    id,
						Kind:    EventReviewFeedback,
						Summary: fmt.Sprintf("%s round %d/%d cannot merge at %s (%d blocking finding(s)); could not tell implementer (%s)", id, r.Round, st.MaxRounds, short(headSHA), len(findings), shortTitle(terr.Error(), 80)),
						At:      now,
						Clears:  &InboxCondition{Kind: "review_feedback", Bead: id, Key: headSHA},
					})
				}
			}
		}
	}
	if err := saveReviewLoop(st); err != nil {
		return ReviewVerdictResult{}, err
	}
	res.State = st
	res.Message = fmt.Sprintf("recorded cannot_merge for %s round %d/%d at %s (%d blocking finding(s))",
		id, r.Round, st.MaxRounds, short(headSHA), len(findings))
	if res.Routed {
		res.Message += "; routed feedback to implementer"
	} else if res.RouteResult != "" {
		res.Message += "; " + res.RouteResult
	}
	return res, nil
}

func escalateReviewLoop(env reviewLoopEnv, b Bead, st *ReviewLoopState, headSHA, why string, now time.Time) error {
	st.State = reviewStateEscalated
	st.EscalatedAt = now
	st.EscalatedWhy = why
	st.UpdatedAt = now
	if err := saveReviewLoop(*st); err != nil {
		return err
	}
	if env.note != nil {
		_ = env.note(b.ID, fmt.Sprintf("REVIEW ESCALATED: round %d/%d head=%s — %s", st.CurrentRound, st.MaxRounds, short(headSHA), why))
		question := fmt.Sprintf(
			"QUESTION: Skeptical review loop for %s reached max rounds (%d/%d) at %s (%s). How should we proceed? Recommendation: 1 — inspect the remaining blocking findings and either grant one more fix round or narrow scope.\nOPTIONS:\n1. Reset review cap (`quartermaster review request %s --reset --by TJ`) and have implementer address remaining findings\n2. Accept current head %s and proceed to human PR review\n3. Pause implementer and take over manually\nCLASS: 3 — review loop escalation after max rounds",
			b.ID, st.CurrentRound, st.MaxRounds, short(headSHA), shortTitle(why, 120), b.ID, short(headSHA))
		if err := env.note(b.ID, question); err != nil {
			return err
		}
	}
	if env.setStatus != nil {
		if err := env.setStatus(b.ID, StatusNeedsMe); err != nil {
			return err
		}
	}
	if env.journal != nil {
		_ = env.journal(journalRecord{
			At:     now,
			Kind:   journalReview,
			Bead:   b.ID,
			Class:  classHousekeeping,
			Action: "escalated",
			Pass:   false,
			Text:   why,
		})
	}
	return nil
}

func buildReviewerBeadDescription(impl Bead, info reviewHeadInfo, round, maxRounds int) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Skeptical review pass (round %d/%d) for implementer bead %s: %s\n\n", round, maxRounds, impl.ID, impl.Title)
	fmt.Fprintf(&s, "- Target bead: `%s`\n", impl.ID)
	fmt.Fprintf(&s, "- Target head SHA: `%s`\n", info.HeadSHA)
	if info.Branch != "" {
		fmt.Fprintf(&s, "- Branch: `%s`\n", info.Branch)
	}
	if info.Workdir != "" {
		fmt.Fprintf(&s, "- Implementer worktree (read-only reference): `%s`\n", info.Workdir)
	}
	if info.PRURL != "" {
		fmt.Fprintf(&s, "- Pull request: %s\n", info.PRURL)
	}
	s.WriteString("\nReview the diff at the exact head commit against the target bead's description, design notes, and `## Acceptance` criteria.\n")
	fmt.Fprintf(&s, "Record the structured verdict with:\n`quartermaster review verdict %s --head %s --verdict can_merge|cannot_merge --summary \"...\" [--finding \"file:line: defect — consequence\"] [--later \"...\"]`\n\n", impl.ID, info.HeadSHA)
	s.WriteString("## Acceptance\n\n")
	fmt.Fprintf(&s, "- Exact commit `%s` inspected against `%s` description, design notes, and acceptance criteria\n", short(info.HeadSHA), impl.ID)
	fmt.Fprintf(&s, "- Structured `can_merge` or `cannot_merge` verdict recorded via `quartermaster review verdict %s --head %s ...`\n", impl.ID, info.HeadSHA)
	s.WriteString("- Any blocking findings cite exact file/line and consequence; optional polish separated under Later\n")
	return s.String()
}

func buildReviewPassPrompt(impl Bead, st ReviewLoopState, round int, info reviewHeadInfo) string {
	var s strings.Builder
	fmt.Fprintf(&s, "%s Skeptical review round %d/%d for implementer bead %s: %s\n\n", reviewHeaderPrefix, round, st.MaxRounds, impl.ID, impl.Title)
	fmt.Fprintf(&s, "- Head SHA: `%s`\n", info.HeadSHA)
	if info.Branch != "" {
		fmt.Fprintf(&s, "- Branch: `%s`\n", info.Branch)
	}
	if info.Workdir != "" {
		fmt.Fprintf(&s, "- Worktree (read-only inspection only): `%s`\n", info.Workdir)
	}
	if info.PRURL != "" {
		fmt.Fprintf(&s, "- PR: %s\n", info.PRURL)
	}
	if desc := strings.TrimSpace(impl.Description); desc != "" {
		fmt.Fprintf(&s, "\nTarget bead description & acceptance criteria:\n%s\n", desc)
	}
	if len(st.Rounds) > 0 {
		prev := st.Rounds[len(st.Rounds)-1]
		if len(prev.Findings) > 0 {
			fmt.Fprintf(&s, "\nPrior round %d/%d (%s) blocking findings to re-verify:\n", prev.Round, st.MaxRounds, short(prev.HeadSHA))
			for i, f := range prev.Findings {
				fmt.Fprintf(&s, "%d. %s\n", i+1, f.String())
			}
		}
	}
	fmt.Fprintf(&s, "\nInspect the exact commit `%s` (full diff, tests/checks, and acceptance criteria) and record your structured verdict:\n", info.HeadSHA)
	fmt.Fprintf(&s, "  quartermaster review verdict %s --head %s --verdict can_merge|cannot_merge --summary \"<concise verdict>\" [--finding \"file:line: concrete defect — consequence\"] [--later \"<non-blocking item>\"]\n", impl.ID, info.HeadSHA)
	return s.String()
}

func buildReviewFeedbackPrompt(impl Bead, r ReviewRound, maxRounds int) string {
	var s strings.Builder
	fmt.Fprintf(&s, "%s Skeptical review round %d/%d for %s returned verdict: cannot merge at head %s.\n", reviewHeaderPrefix, r.Round, maxRounds, impl.ID, short(r.HeadSHA))
	fmt.Fprintf(&s, "Summary: %s\n", r.Summary)
	s.WriteString("Blocking findings:\n")
	for i, f := range r.Findings {
		fmt.Fprintf(&s, "%d. %s\n", i+1, f.String())
	}
	if len(r.Later) > 0 {
		s.WriteString("Non-blocking (Later):\n")
		for _, l := range r.Later {
			fmt.Fprintf(&s, "- %s\n", l)
		}
	}
	fmt.Fprintf(&s, "\nAddress every valid blocking finding (or reply with concrete evidence if invalid), verify tests, commit/push a new head SHA, and note the updated head for round %d/%d.", r.Round+1, maxRounds)
	return s.String()
}

func buildReviewPassedPrompt(impl Bead, r ReviewRound, maxRounds int) string {
	var s strings.Builder
	fmt.Fprintf(&s, "%s Skeptical review round %d/%d for %s passed (can merge) at head %s: %s.", reviewHeaderPrefix, r.Round, maxRounds, impl.ID, short(r.HeadSHA), r.Summary)
	if len(r.Later) > 0 {
		fmt.Fprintf(&s, " Non-blocking Later items: %s.", strings.Join(r.Later, "; "))
	}
	s.WriteString(" Proceed to open or update the PR and follow normal CI, review, and merge policy (this internal review grants no GitHub PR approval).")
	return s.String()
}

// reviewLoopTriggerEnv injects git/event lookup for deterministic ticker tests.
type reviewLoopTriggerEnv struct {
	resolveHead func(th Thread) (string, bool) // headSHA, readyForReview
	event       func(Event) error
	logf        func(format string, args ...any)
}

// checkReviewLoopsPass inspects active implementer threads with either an
// existing changes_requested review loop or the opt-in review:required label.
// Whenever a new head SHA appears (and current_round < max_rounds), it emits
// one deduplicated review_needed inbox event per head SHA.
func checkReviewLoopsPass(cfg Config, env reviewLoopTriggerEnv, threads []Thread, now time.Time) []string {
	var lines []string
	for _, th := range threads {
		b := th.Bead
		if b.Status == StatusClosed || b.Status == StatusNeedsMe {
			continue
		}
		if role, err := cfg.resolveRoleForBead(b, ""); err != nil || role.Name != roleImplementer {
			continue
		}
		st, exists, err := loadReviewLoop(b.ID)
		if err != nil {
			if env.logf != nil {
				env.logf("review loop %s: %v", b.ID, err)
			}
			continue
		}
		needsCheck := false
		switch {
		case exists && st.State == reviewStateChanges && st.CurrentRound < st.MaxRounds:
			needsCheck = true
		case b.HasLabel(reviewRequiredLabel):
			if !exists || (st.State != reviewStatePending && st.State != reviewStateEscalated && st.CurrentRound < st.MaxRounds) {
				needsCheck = true
			}
		}
		if !needsCheck {
			continue
		}

		var headSHA string
		if env.resolveHead != nil {
			var ok bool
			headSHA, ok = env.resolveHead(th)
			if !ok {
				continue
			}
		}
		headSHA = strings.ToLower(strings.TrimSpace(headSHA))
		if !fullSHA.MatchString(headSHA) {
			continue
		}
		if exists {
			if headSHA == st.LastHeadSHA || headSHA == st.PassedHeadSHA || headSHA == st.NotifiedHeadSHA {
				continue
			}
		}

		unlock, err := lockReviewLoop(b.ID)
		if err != nil {
			continue
		}
		// Re-read under lock so concurrent review requests never race.
		cur, curExists, lerr := loadReviewLoop(b.ID)
		if lerr != nil {
			unlock()
			continue
		}
		if !curExists {
			cur = ReviewLoopState{
				Version:   reviewLoopSchemaVersion,
				Bead:      b.ID,
				MaxRounds: defaultReviewMaxRounds,
			}
		}
		if cur.State == reviewStatePending || cur.State == reviewStateEscalated || cur.CurrentRound >= cur.MaxRounds ||
			headSHA == cur.LastHeadSHA || headSHA == cur.PassedHeadSHA || headSHA == cur.NotifiedHeadSHA {
			unlock()
			continue
		}
		cur.NotifiedHeadSHA = headSHA
		cur.UpdatedAt = now
		if err := saveReviewLoop(cur); err != nil {
			unlock()
			if env.logf != nil {
				env.logf("review loop %s save: %v", b.ID, err)
			}
			continue
		}
		unlock()

		nextRound := cur.CurrentRound + 1
		summary := fmt.Sprintf("%s has new head %s ready for skeptical review round %d/%d; run `quartermaster review request %s --head %s --spawn`",
			b.ID, short(headSHA), nextRound, cur.MaxRounds, b.ID, headSHA)
		if cur.ReviewerBead != "" {
			summary = fmt.Sprintf("%s has new head %s ready for skeptical review round %d/%d (reviewer %s); run `quartermaster review request %s --head %s`",
				b.ID, short(headSHA), nextRound, cur.MaxRounds, cur.ReviewerBead, b.ID, headSHA)
		}
		if env.event != nil {
			if err := env.event(Event{
				Bead:    b.ID,
				Kind:    EventReviewNeeded,
				Summary: summary,
				At:      now,
				Clears:  &InboxCondition{Kind: "review_needed", Bead: b.ID, Key: headSHA},
			}); err != nil {
				if env.logf != nil {
					env.logf("review loop %s event: %v", b.ID, err)
				}
				continue
			}
		}
		lines = append(lines, summary)
	}
	return lines
}

func (t Ticker) checkReviewLoops(threads []Thread, now time.Time) {
	env := reviewLoopTriggerEnv{
		resolveHead: func(th Thread) (string, bool) {
			if th.PR != nil && th.PR.State == "OPEN" && fullSHA.MatchString(strings.ToLower(th.PR.HeadSHA)) {
				return strings.ToLower(th.PR.HeadSHA), true
			}
			// Pre-PR worktree head triggers only when the worker is at rest
			// with a clean working tree so we never review half-edited files.
			if th.Agent == nil || !agentReady(th.Agent) {
				return "", false
			}
			wt, _, err := linkedWorktree(t.cfg, t.herdr, th.Bead)
			if err != nil || wt == nil || !gitWorktreeClean(wt.Path) {
				return "", false
			}
			sha, err := gitHeadSHA(wt.Path)
			if err != nil {
				return "", false
			}
			return sha, true
		},
		event: writeEvent,
		logf:  t.log.Printf,
	}
	for _, line := range checkReviewLoopsPass(t.cfg, env, threads, now) {
		t.log.Printf("review loop: %s", line)
	}
}

type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, "; ") }
func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

const reviewUsage = `usage:
  quartermaster review request <bead> [--head <40-sha>] [--pr <url>] [--reviewer <rev-bead>] [--spawn] [--agent K] [--max-rounds N] [--reset] [--by WHO] [--json]
  quartermaster review verdict <bead> (--file <json|-> | --verdict can_merge|cannot_merge --head <40-sha> --summary "..." [--finding "file:line: defect — consequence"]... [--later "..."]...) [--by WHO] [--no-route] [--json]
  quartermaster review status [<bead>] [--json]`

func reviewCmd(cfg Config, env reviewLoopEnv, args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(reviewUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "request", "start":
		fs := flag.NewFlagSet("review request", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		head := fs.String("head", "", "40-character head commit SHA to review")
		prU := fs.String("pr", "", "pull request URL (optional)")
		reviewer := fs.String("reviewer", "", "existing role:reviewer bead ID")
		spawn := fs.Bool("spawn", false, "create and dispatch a worktree-free role:reviewer bead if none is linked")
		agent := fs.String("agent", "", "agent kind override for spawned reviewer")
		maxRounds := fs.Int("max-rounds", 0, "max ping-pong rounds before escalating to needs_me (default 2, max 5)")
		reset := fs.Bool("reset", false, "reset round counter and escalation state (requires --by TJ)")
		by := fs.String("by", "coordinator", "actor requesting review (coordinator, inbox-runner, TJ)")
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, reviewUsage)
		}
		if len(pos) != 1 {
			return errors.New(reviewUsage)
		}
		res, err := requestReview(cfg, env, pos[0], ReviewRequestOpts{
			HeadSHA:       *head,
			PRURL:         *prU,
			ReviewerBead:  *reviewer,
			SpawnReviewer: *spawn,
			Agent:         *agent,
			MaxRounds:     *maxRounds,
			Reset:         *reset,
			By:            *by,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(out, res)
		}
		fmt.Fprintln(out, res.Message)
		return nil

	case "verdict":
		fs := flag.NewFlagSet("review verdict", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		filePath := fs.String("file", "", "JSON verdict payload file or - for stdin")
		verdict := fs.String("verdict", "", "can_merge or cannot_merge")
		head := fs.String("head", "", "40-character reviewed head commit SHA")
		summary := fs.String("summary", "", "concise verdict summary")
		var findingsFlag stringListFlag
		fs.Var(&findingsFlag, "finding", "blocking finding (repeatable): 'file:line: defect — consequence'")
		var laterFlag stringListFlag
		fs.Var(&laterFlag, "later", "non-blocking polish item (repeatable)")
		by := fs.String("by", roleReviewer, "actor recording verdict (reviewer, coordinator, TJ)")
		noRoute := fs.Bool("no-route", false, "record verdict without calling tell on the implementer")
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, reviewUsage)
		}
		if len(pos) != 1 {
			return errors.New(reviewUsage)
		}
		var in ReviewVerdictInput
		if *filePath != "" {
			var raw []byte
			if *filePath == "-" {
				raw, err = io.ReadAll(stdin)
			} else {
				raw, err = os.ReadFile(*filePath)
			}
			if err != nil {
				return err
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				return fmt.Errorf("%w: invalid verdict JSON: %v", errReviewLoop, err)
			}
		}
		if *verdict != "" {
			in.Verdict = *verdict
		}
		if *head != "" {
			in.HeadSHA = *head
		}
		if *summary != "" {
			in.Summary = *summary
		}
		for _, raw := range findingsFlag {
			f, err := parseFindingFlag(raw)
			if err != nil {
				return err
			}
			in.Findings = append(in.Findings, f)
		}
		in.Later = append(in.Later, laterFlag...)
		if in.By == "" || *by != roleReviewer {
			in.By = *by
		}
		if *noRoute {
			in.NoRoute = true
		}
		res, err := recordReviewVerdict(env, pos[0], in)
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(out, res)
		}
		fmt.Fprintln(out, res.Message)
		return nil

	case "status":
		fs := flag.NewFlagSet("review status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "emit JSON output")
		pos, err := interspersed(fs, rest)
		if err != nil {
			return fmt.Errorf("%v\n%s", err, reviewUsage)
		}
		if len(pos) > 1 {
			return errors.New(reviewUsage)
		}
		if len(pos) == 1 {
			st, ok, err := loadReviewLoop(pos[0])
			if err != nil {
				return err
			}
			if !ok {
				if *asJSON {
					return writePrettyJSON(out, ReviewLoopState{Version: reviewLoopSchemaVersion, Bead: pos[0], MaxRounds: defaultReviewMaxRounds})
				}
				fmt.Fprintf(out, "%s: no review loop recorded\n", pos[0])
				return nil
			}
			if *asJSON {
				return writePrettyJSON(out, st)
			}
			writeReviewLoopStatusText(out, st)
			return nil
		}
		loops, err := listReviewLoops()
		if err != nil {
			return err
		}
		if *asJSON {
			if loops == nil {
				loops = []ReviewLoopState{}
			}
			return writePrettyJSON(out, loops)
		}
		if len(loops) == 0 {
			fmt.Fprintln(out, "(no active review loops)")
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "BEAD\tSTATE\tROUND\tHEAD\tREVIEWER")
		for _, st := range loops {
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\n",
				st.Bead, orDash(st.State), st.CurrentRound, st.MaxRounds, orDash(short(st.LastHeadSHA)), orDash(st.ReviewerBead))
		}
		return tw.Flush()

	default:
		return fmt.Errorf("unknown review command %q\n%s", sub, reviewUsage)
	}
}

func writeReviewLoopStatusText(w io.Writer, st ReviewLoopState) {
	fmt.Fprintf(w, "bead:     %s\n", st.Bead)
	fmt.Fprintf(w, "state:    %s\n", orDash(st.State))
	fmt.Fprintf(w, "rounds:   %d/%d\n", st.CurrentRound, st.MaxRounds)
	if st.LastHeadSHA != "" {
		fmt.Fprintf(w, "head:     %s\n", st.LastHeadSHA)
	}
	if st.PassedHeadSHA != "" {
		fmt.Fprintf(w, "passed:   %s\n", st.PassedHeadSHA)
	}
	if st.ReviewerBead != "" {
		fmt.Fprintf(w, "reviewer: %s\n", st.ReviewerBead)
	}
	if st.EscalatedWhy != "" {
		fmt.Fprintf(w, "escalated: %s\n", st.EscalatedWhy)
	}
	for _, r := range st.Rounds {
		v := r.Verdict
		if v == "" {
			v = "pending"
		}
		fmt.Fprintf(w, "  round %d/%d @%s [%s]", r.Round, st.MaxRounds, short(r.HeadSHA), v)
		if r.Summary != "" {
			fmt.Fprintf(w, " — %s", r.Summary)
		}
		fmt.Fprintln(w)
		for i, f := range r.Findings {
			fmt.Fprintf(w, "    %d. %s\n", i+1, f.String())
		}
	}
}

func writePrettyJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
