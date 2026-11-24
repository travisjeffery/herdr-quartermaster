package main

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Group is where a thread sits from the user's point of view. The order is the
// sidebar and board order: what needs the user first.
type Group int

const (
	GroupNeedsYou Group = iota
	GroupMerged
	GroupReady
	GroupReview
	GroupRollingOut
	GroupFailing
	GroupIdle
	GroupWorking
	GroupBlocked
	GroupNoAgent
)

var groupNames = map[Group]string{
	GroupNeedsYou:   "needs you",
	GroupMerged:     "merged",
	GroupReady:      "ready to merge",
	GroupReview:     "review",
	GroupRollingOut: "rolling out",
	GroupFailing:    "checks failing",
	GroupIdle:       "idle",
	GroupWorking:    "working",
	GroupBlocked:    "blocked",
	GroupNoAgent:    "no agent",
}

func (g Group) String() string { return groupNames[g] }

// Thread is one bead with whatever is working on it and delivering it.
type Thread struct {
	// state is derived once after gathering, then shared by all view adapters.
	state        *ThreadState
	Blockers     []string
	OpenChildren int
	Bead         Bead
	Agent        *Agent
	// AgentUnknown means a partial discovery could not observe this worker.
	AgentUnknown bool
	PR           *PR
	Checks       Checks
	Reviews      int // human reviews by someone other than the user
	BotReviews   []string
	Runs         []Run
	Linear       string
	// Merged is every merged PR of the bead's, the pick or not, so a merge
	// behind another open PR is still announced.
	Merged []PR
	// Level is the bead's effective autonomy level as levelMark shows it.
	Level string
	// Prompt is the approval prompt a blocked agent is showing, when the
	// ticker could read it.
	Prompt *ApprovalPrompt
	// Unprompted marks a needs_me thread whose agent went back to work with
	// no prompt since it settled, by its transcript: it compacted or a
	// background task finished, and nobody answered it.
	Unprompted bool
	// Turn is how a pi worker's last turn ended, read from its session file;
	// nil for other kinds.
	Turn *PiTurn
}

func agentReady(a *Agent) bool {
	return a != nil && (a.Status == "idle" || a.Status == "done")
}

// answered reports a needs_me bead whose agent is working again: the user
// answered it in its pane, or it is still in the turn that asked. Either way
// nobody is waiting on the user yet.
func answered(t Thread) bool {
	return t.Bead.Status == StatusNeedsMe && t.Agent != nil && t.Agent.Status == "working" && !t.Unprompted
}

// classify is the sidebar/board adapter, not another precedence table.
func classify(t Thread) Group { return t.derived().group() }

// pendingCommand is the command a worker asked the user to run, from its
// latest "RUN:" note, until a later answer boundary or non-housekeeping note
// moves the thread on. Trailing housekeeping notes (such as AUTO: handoff or
// likely_stale rejected) and CLASS: lines do not hide a live RUN: command.
func pendingCommand(notes string) string {
	var lines []string
	for _, l := range strings.Split(notes, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	end := len(lines)
	for end > 0 && (isNonAnswerHousekeepingNote(lines[end-1]) || isClassLine(lines[end-1])) {
		end--
	}
	lines = lines[:end]
	// A RUN: is only live while it is one of the last two substantive notes
	// (the command and its one line of why). Anything later means the thread
	// moved on, so an answered command is never offered again even without a
	// RAN: line.
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-2; i-- {
		l := lines[i]
		if isAnswerBoundaryLine(l) {
			return ""
		}
		if cmd, ok := strings.CutPrefix(l, "RUN:"); ok {
			return strings.TrimSpace(cmd)
		}
	}
	return ""
}

// runCommand is the pending command of a needs_me thread, the only state in
// which one is still waiting on the user.
func runCommand(t Thread) string { return t.derived().Command }

func stateLine(t Thread) string {
	state := t.derived()
	g := state.group()
	s := sectionName(g)
	if state.ActiveTurn {
		s += " · needs_me"
	}
	if state.Command != "" {
		s += " · run command"
	}
	if t.PR != nil {
		s += fmt.Sprintf(" · PR #%d", t.PR.Number)
		switch {
		case g == GroupFailing:
			s += " (" + strings.Join(t.Checks.Failing, ", ") + ")"
		case t.PR.State == "OPEN" && t.PR.ReviewDecision == "CHANGES_REQUESTED":
			s += " changes requested"
		case mergeable(t.PR, t.Checks) && len(mergeBlockers(t.PR)) > 0:
			s += " " + approval(t.PR) + " but blocked: " + strings.Join(mergeBlockers(t.PR), ", ")
		case t.PR.State == "OPEN" && t.PR.ReviewDecision == "APPROVED":
			s += " approved"
		case t.PR.State == "OPEN" && t.Checks.Pending > 0:
			s += " checks running"
		}
	}
	for _, r := range t.Runs {
		if r.Status != "completed" {
			s += " · " + r.Workflow + " running"
		}
	}
	if t.Linear != "" {
		s += " · " + t.Linear
	}
	if t.Level != "" {
		s += " · " + t.Level
	}
	return s
}

// Snapshot is what the ticker remembers about a bead between ticks.
type Snapshot struct {
	Group       Group     `json:"group"`
	AgentStatus string    `json:"agent_status"`
	AgentSeq    int64     `json:"agent_seq"`
	ReadySince  time.Time `json:"ready_since"`
	PRNumber    int       `json:"pr_number"`
	PRState     string    `json:"pr_state"`
	InboxPRKey  string    `json:"inbox_pr_key,omitempty"`
	Failing     []string  `json:"failing"`
	Reviews     int       `json:"reviews"`
	BotReviews  int       `json:"bot_reviews"`
	// ReviewsSplit marks a snapshot that counted bot and human reviews apart.
	ReviewsSplit bool              `json:"reviews_split"`
	Runs         map[string]string `json:"runs,omitempty"`
	Pending      []string          `json:"pending_prompts"`
	// NeedsMe is the bead's needs_me status at this snapshot.
	NeedsMe bool `json:"needs_me"`
	// Unprompted retains background-wake evidence for read-only views.
	Unprompted bool `json:"unprompted,omitempty"`
	// ReadyKey is the PR and head commit last seen ready to merge (readyKey),
	// so ready_to_merge fires once per push and again for a replacement PR.
	ReadyKey string `json:"ready_key,omitempty"`
	// GreenKey is the PR and head commit last seen green and awaiting review
	// (greenKey), so the green-PR desktop milestone fires once per head.
	// GreenTracked marks a snapshot that kept it; pre-upgrade snapshots
	// baseline without announcing already-green PRs.
	GreenKey     string `json:"green_key,omitempty"`
	GreenTracked bool   `json:"green_tracked,omitempty"`
	// ReviewReadyKey is the PR and head commit last seen ready for review
	// (reviewReadyKey), so ready_for_review fires once per (bead, PR, head).
	// ReviewReadyTracked marks a snapshot that kept it; pre-upgrade snapshots
	// baseline without announcing already-ready PRs. ReviewReadyAt records
	// when ReviewReadyKey entered ready-for-review, ReviewReadyReviews is the
	// human review count at that time, ReviewReadyReviewed latches once a
	// newer human review arrives on that head, and ReviewBumpKey records the
	// head that already received its single bounded review_bump reminder.
	ReviewReadyKey      string    `json:"review_ready_key,omitempty"`
	ReviewReadyTracked  bool      `json:"review_ready_tracked,omitempty"`
	ReviewReadyAt       time.Time `json:"review_ready_at,omitempty"`
	ReviewReadyReviews  int       `json:"review_ready_reviews,omitempty"`
	ReviewReadyReviewed bool      `json:"review_ready_reviewed,omitempty"`
	ReviewBumpKey       string    `json:"review_bump_key,omitempty"`
	// Merged holds the URL of every PR of the bead's already announced as
	// merged, so a bead with several PRs hears of each merge once.
	// MergedTracked marks a snapshot that kept it; older ones knew only their
	// own PR's state.
	Merged        []string `json:"merged,omitempty"`
	MergedTracked bool     `json:"merged_tracked,omitempty"`
	// AutoMerge is the URL of the open PR last seen with auto-merge on, so the
	// worker hears once when GitHub turns it off.
	AutoMerge string `json:"auto_merge,omitempty"`
	// UnsafeAutoKey deduplicates warnings for native auto on an ineligible head.
	UnsafeAutoKey string `json:"unsafe_auto_key,omitempty"`
	// PromptHash is the approval prompt last announced, so each prompt a
	// blocked agent shows reaches the inbox once. For a blocked agent whose
	// prompt cannot be parsed it holds promptUnparsedKey once that prompt
	// has surfaced (or was baselined on first sight).
	PromptHash string `json:"prompt_hash,omitempty"`
	// PromptPending is a worker prompt seen but not yet surfaced while the
	// prompt debounce runs: its hash, or promptUnparsedKey, first seen at
	// PromptSince. PromptLastSeen is when the tracked prompt (pending or
	// announced) was last detected; it bounds the flap tolerance after
	// which a missing prompt counts as cleared. All three ride state.json
	// with the snapshot, so a ticker restart mid-window keeps the clock.
	PromptPending  string    `json:"prompt_pending,omitempty"`
	PromptSince    time.Time `json:"prompt_since,omitempty"`
	PromptLastSeen time.Time `json:"prompt_last_seen,omitempty"`
	// TurnID is the id of the last turn end in a pi worker's session.
	TurnID string `json:"turn_id,omitempty"`
	// StallRetries is how many consecutive transient ACP stall auto-retries
	// the ticker sent in the current stall episode; resets after a healthy turn.
	StallRetries int `json:"stall_retries,omitempty"`
	// StallTurnID is the failed turn id tracked for ACP stall auto-retry.
	StallTurnID string `json:"stall_turn_id,omitempty"`
	// StallNextAt is when a pending ACP stall retry becomes eligible after backoff.
	StallNextAt time.Time `json:"stall_next_at,omitempty"`
	// StallError is the stall error message for StallTurnID.
	StallError string `json:"stall_error,omitempty"`
	// StallEscalated marks that the current stall episode has already surfaced
	// to the inbox so subsequent ticks do not duplicate it.
	StallEscalated bool `json:"stall_escalated,omitempty"`
	// Relaunch marks a worker asked to exit so it can be resumed in the
	// launch mode its level calls for.
	Relaunch bool `json:"relaunch,omitempty"`
	// Auto is how many AUTO: notes the bead had, so each new one is journaled
	// once.
	Auto int `json:"auto,omitempty"`
	// Asked is the needs_me question last seen (asked), so a thread that
	// wakes and settles again without a new one isn't announced twice.
	Asked string `json:"asked,omitempty"`
}

type EventKind string

const (
	EventNeedsYou        EventKind = "needs_you"
	EventFailing         EventKind = "checks_failing"
	EventReview          EventKind = "new_review"
	EventMerged          EventKind = "merged"
	EventReady           EventKind = "ready_to_merge"
	EventReadyForReview  EventKind = "ready_for_review"
	EventReviewBump      EventKind = "review_bump"
	EventFinished        EventKind = "finished"
	EventAgentGone       EventKind = "agent_gone"
	EventResumed         EventKind = "resumed"
	EventRunSucceeded    EventKind = "run_succeeded"
	EventRunFailed       EventKind = "run_failed"
	EventLikelyStale     EventKind = "likely_stale"
	EventAutoClosed      EventKind = "auto_closed"
	EventOrphanFinish    EventKind = "orphan_finish"
	EventAutoMergeOff    EventKind = "auto_merge_off"
	EventUnsafeAutoMerge EventKind = "unsafe_auto_merge"
	EventLeaseExpired    EventKind = "lease_expired"
	EventBreaker         EventKind = "breaker_tripped"
	EventWatchTriggered  EventKind = "watch_triggered"
	EventWatchExpired    EventKind = "watch_expired"
	EventUnblocked       EventKind = "unblocked"
)

type Event struct {
	Bead    string          `json:"bead"`
	Kind    EventKind       `json:"kind"`
	Summary string          `json:"summary"`
	At      time.Time       `json:"at"`
	Clears  *InboxCondition `json:"clears,omitempty"`
}

// Outcome is what one thread's transition asks the ticker to do.
type Outcome struct {
	Events []Event
	// Prompts go to the thread's agent once it has been ready long enough.
	Prompts []string
	Notify  bool
	// Resume sets a needs_me bead back to in_progress: its agent went back to
	// work after the question was asked, so the user answered it in the pane.
	Resume bool
}

// wokeAfterNeedsMe reports a needs_me thread whose agent is working again
// after the ticker saw it settled at rest: an answer, unless it woke on its own.
func wokeAfterNeedsMe(t Thread, prev Snapshot) bool {
	return answered(t) && prev.NeedsMe && (agentReady(&Agent{Status: prev.AgentStatus}) || prev.AgentStatus == "blocked")
}

// asked fingerprints a needs_me bead's active question rather than the full
// notes log, so unrelated or AUTO notes appended while waiting neither archive
// the inbox item nor trigger duplicate needs_you notifications.
func asked(b Bead) string {
	if b.Status != StatusNeedsMe {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(activeQuestionText(b.Notes))))[:16]
}

var housekeepingNoteRe = regexp.MustCompile(`(?i)\blikely[_ -]stale\b|^keep open\b|^(?:\d{4}-\d{2}-\d{2}(?:[T ]\S+)?\s+)?coordinator(?:\s*\([^)]*\))?:\s*(?:agent session closed|handoff|likely[_ -]stale|parked|snoozed|inbox triage)\b`)

func isHousekeepingNote(line string) bool {
	for _, prefix := range []string{
		"AUTO:", "auto:", "VERIFY:", "PR:", "Linear:", "MILESTONE:",
		"WATCH TRIGGERED", "WATCH EXPIRED",
		"SNOOZE:", "SNOOZED:",
		"quartermaster relaunched the worker ",
		"quartermaster could not relaunch the worker ",
		"pi-rpc fork prepared,",
		"released by quartermaster ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return housekeepingNoteRe.MatchString(line)
}

func isBookkeepingNote(line string) bool {
	return isAnswerBoundaryLine(line) || isNonAnswerHousekeepingNote(line)
}

// structuredQuestionBlock returns the canonical QUESTION/OPTIONS/CLASS block
// containing line idx if idx is the OPTIONS: header or one of its options.
func structuredQuestionBlock(lines []string, idx int) (string, bool) {
	at := idx
	if lines[at] != "OPTIONS:" {
		if !questionOptionRe.MatchString(lines[at]) {
			return "", false
		}
		for at > 0 && questionOptionRe.MatchString(lines[at-1]) {
			at--
		}
		if at == 0 || lines[at-1] != "OPTIONS:" {
			return "", false
		}
		at--
	}
	start := -1
	for j := at - 1; j >= 0; j-- {
		if isBookkeepingNote(lines[j]) {
			break
		}
		if strings.HasPrefix(lines[j], "QUESTION:") {
			start = j
			break
		}
	}
	if start < 0 {
		return "", false
	}
	if start > 0 && strings.HasPrefix(lines[start-1], "CRITICAL:") {
		start--
	}
	end := at + 1
	for end < len(lines) && questionOptionRe.MatchString(lines[end]) {
		end++
	}
	if end < len(lines) {
		if _, _, ok := parseClassLine(lines[end]); ok {
			end++
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// activeQuestionText extracts the question a needs_me bead is currently asking.
// Only notes after the last answer boundary count; trailing housekeeping notes
// (AUTO: handoff, likely_stale rejected, etc.) leave the active question intact,
// and explicit CRITICAL:/QUESTION:/RUN:/OPTIONS: directives take precedence
// over non-directive notes.
func activeQuestionText(notes string) string {
	after := linesAfterAnswer(noteLines(notes))
	if len(after) == 0 {
		return ""
	}
	var latestAsks, latestNonBookkeeping string
	for i := len(after) - 1; i >= 0; i-- {
		l := after[i]
		if isBookkeepingNote(l) || isClassLine(l) {
			continue
		}
		if latestNonBookkeeping == "" {
			latestNonBookkeeping = l
		}
		if block, ok := structuredQuestionBlock(after, i); ok {
			return block
		}
		if l == "OPTIONS:" && i > 0 && !isBookkeepingNote(after[i-1]) && !isClassLine(after[i-1]) {
			start := i - 1
			if start > 0 && strings.HasPrefix(after[start-1], "CRITICAL:") {
				start--
			}
			end := i + 1
			for end < len(after) && questionOptionRe.MatchString(after[end]) {
				end++
			}
			return strings.Join(after[start:end], "\n")
		}
		if isExplicitQuestionDirective(l) {
			if strings.HasPrefix(l, "CRITICAL:") {
				if strings.HasPrefix(latestAsks, l+"\n") {
					return latestAsks
				}
				return l
			}
			if i > 0 && strings.HasPrefix(after[i-1], "CRITICAL:") {
				return after[i-1] + "\n" + l
			}
			return l
		}
		if latestAsks == "" && asks(l) {
			if i > 0 && strings.HasPrefix(after[i-1], "CRITICAL:") {
				latestAsks = after[i-1] + "\n" + l
			} else {
				latestAsks = l
			}
		}
	}
	if latestAsks != "" {
		return latestAsks
	}
	return latestNonBookkeeping
}

// resumeNote is the note left on a bead the ticker resumed; the question
// stays in the notes above it.
const resumeNote = "auto: agent resumed after needs_me"

// promptUnparsedKey is the identity of a blocked agent's prompt that
// cannot be parsed off its screen. Parsed prompts are identified by their
// hash (a pi ask's hash covers its ask id, so the same command asked twice
// is two prompts); an unparsed one is only known by its blocked episode.
const promptUnparsedKey = "unparsed"

// promptKey is the identity of the worker prompt t is showing, or false when
// its agent is not blocked at one.
func promptKey(t Thread) (string, bool) {
	if t.Agent == nil || t.Agent.Status != "blocked" {
		return "", false
	}
	if t.Prompt != nil && t.Prompt.Hash != "" {
		return t.Prompt.Hash, true
	}
	return promptUnparsedKey, true
}

// promptDebounced reports whether t's worker prompt is held by the prompt
// debounce. A needs_me bead is already waiting on a human decision, so a
// prompt on it surfaces at once, as its question does.
func promptDebounced(t Thread, debounce time.Duration) bool {
	if debounce <= 0 || t.Bead.Status == StatusNeedsMe {
		return false
	}
	_, present := promptKey(t)
	return present
}

// promptTrack is one tick's prompt tracking: what the next snapshot
// remembers, and whether a pending prompt surfaces on this tick.
type promptTrack struct {
	key      string // the prompt showing now, "" when none is
	surfaced string // next PromptHash
	pending  string
	since    time.Time
	lastSeen time.Time
	due      bool
}

// trackPrompt advances the debounce state for t against prev. The same
// prompt seen again keeps the clock it started on its first tick; a
// different hash is a new instance with a fresh window. While the prompt
// goes undetected for at most flap (the agent briefly not blocked, or a
// parsed prompt's screen briefly unreadable) its clock and its
// already-surfaced dedup are kept; a longer absence clears both, so the
// prompt counts as cleared and a later showing starts over. A prompt
// surfaces only on a tick where it is present: due is never set in its
// absence, so one that clears inside the window never surfaces.
func trackPrompt(t Thread, prev Snapshot, now time.Time, debounce, flap time.Duration) promptTrack {
	key, present := promptKey(t)
	tr := promptTrack{key: key, surfaced: prev.PromptHash, pending: prev.PromptPending, since: prev.PromptSince, lastSeen: prev.PromptLastSeen}
	if !present {
		if tr.lastSeen.IsZero() || now.Sub(tr.lastSeen) > flap {
			tr.surfaced, tr.pending, tr.since, tr.lastSeen = "", "", time.Time{}, time.Time{}
		}
		return tr
	}
	// A snapshot from before the debounce kept no pending state and left
	// PromptHash empty for an unparsed prompt, announced or baselined on
	// its entry to needs-you: adopt it as surfaced rather than announcing
	// it again after the upgrade's first window.
	if key == promptUnparsedKey && prev.PromptPending == "" && prev.PromptHash == "" && prev.PromptLastSeen.IsZero() && prev.Group == GroupNeedsYou && prev.AgentStatus == "blocked" {
		tr.surfaced, tr.lastSeen = promptUnparsedKey, now
		return tr
	}
	switch {
	case key == tr.surfaced:
		tr.pending, tr.since, tr.lastSeen = "", time.Time{}, now
	case key == promptUnparsedKey && !tr.lastSeen.IsZero() && now.Sub(tr.lastSeen) <= flap &&
		((tr.pending != "" && tr.pending != promptUnparsedKey) || (tr.pending == "" && tr.surfaced != "" && tr.surfaced != promptUnparsedKey)):
		// The screen is unreadable this tick while a parsed prompt is
		// tracked: a flap in detection, not a new prompt. Clock, dedup
		// and lastSeen (when it was last actually read) stand.
	case key == tr.pending || tr.pending == promptUnparsedKey:
		// The pending prompt again, or an unreadable blocked episode
		// that just became parseable: the same prompt, now identified,
		// keeps the clock it started unparsed.
		tr.pending = key
		if tr.since.IsZero() {
			tr.since = now
		}
		tr.lastSeen = now
		tr.due = now.Sub(tr.since) >= debounce
	default:
		// A new instance: a different hash, or the first detection.
		tr.pending, tr.since, tr.lastSeen = key, now, now
	}
	return tr
}

type transitionOpts struct {
	debounce   time.Duration
	flap       time.Duration
	reviewBump time.Duration
}

func snapshot(t Thread, prev Snapshot, now time.Time) Snapshot {
	return snapshotWith(t, prev, now, transitionOpts{}, false)
}

func snapshotDebounced(t Thread, prev Snapshot, now time.Time, debounce, flap time.Duration, first bool) Snapshot {
	return snapshotWith(t, prev, now, transitionOpts{debounce: debounce, flap: flap}, first)
}

func snapshotWith(t Thread, prev Snapshot, now time.Time, opts transitionOpts, first bool) Snapshot {
	debounce, flap := opts.debounce, opts.flap
	s := Snapshot{
		Unprompted:          t.Unprompted,
		Group:               classify(t),
		Reviews:             t.Reviews,
		BotReviews:          len(t.BotReviews),
		ReviewsSplit:        true,
		Runs:                runStates(t.Runs),
		Failing:             t.Checks.Failing,
		Pending:             freshPending(t, prev.Pending),
		NeedsMe:             t.Bead.Status == StatusNeedsMe,
		ReadyKey:            prev.ReadyKey,
		GreenKey:            prev.GreenKey,
		GreenTracked:        true,
		ReviewReadyKey:      prev.ReviewReadyKey,
		ReviewReadyTracked:  true,
		ReviewReadyAt:       prev.ReviewReadyAt,
		ReviewReadyReviews:  prev.ReviewReadyReviews,
		ReviewReadyReviewed: reviewReadyReviewed(t, prev),
		ReviewBumpKey:       prev.ReviewBumpKey,
		Relaunch:            prev.Relaunch,
		StallRetries:        prev.StallRetries,
		StallTurnID:         prev.StallTurnID,
		StallNextAt:         prev.StallNextAt,
		StallError:          prev.StallError,
		StallEscalated:      prev.StallEscalated,
	}
	if t.Agent != nil {
		s.AgentStatus, s.AgentSeq = t.Agent.Status, t.Agent.Seq
		switch {
		case !agentReady(t.Agent):
		case agentReady(&Agent{Status: prev.AgentStatus}) && prev.AgentSeq == t.Agent.Seq && !prev.ReadySince.IsZero():
			s.ReadySince = prev.ReadySince
		default:
			s.ReadySince = now
		}
	}
	switch {
	case debounce <= 0 || t.Bead.Status == StatusNeedsMe:
		// No debounce: a prompt counts as announced from the tick it is
		// seen, exactly as before the debounce existed.
		if t.Prompt != nil {
			s.PromptHash = t.Prompt.Hash
		}
	case first:
		// First sight is a baseline, not news: a prompt already showing
		// is remembered as surfaced, so it never produces an event.
		if key, present := promptKey(t); present {
			s.PromptHash, s.PromptLastSeen = key, now
		}
	default:
		tr := trackPrompt(t, prev, now, debounce, flap)
		s.PromptHash, s.PromptPending, s.PromptSince, s.PromptLastSeen = tr.surfaced, tr.pending, tr.since, tr.lastSeen
		if tr.due {
			s.PromptHash, s.PromptPending, s.PromptSince, s.PromptLastSeen = tr.key, "", time.Time{}, now
		}
	}
	if t.Turn != nil {
		s.TurnID = t.Turn.ID
	} else if t.Agent != nil {
		s.TurnID = prev.TurnID
	}
	// The question counts as asked once the thread has sat in needs you, and
	// stays asked while the bead is needs_me, through any wake or prompt.
	switch {
	case s.Group == GroupNeedsYou && !t.Unprompted && t.Prompt == nil && (t.Agent == nil || t.Agent.Status != "blocked"):
		s.Asked = asked(t.Bead)
	case s.NeedsMe:
		s.Asked = prev.Asked
	}
	s.UnsafeAutoKey = unsafeAutoKey(t)
	s.Merged, s.MergedTracked = slices.Clone(prev.Merged), true
	for _, pr := range mergedPRs(t) {
		if !slices.Contains(s.Merged, pr.URL) {
			s.Merged = append(s.Merged, pr.URL)
		}
	}
	if t.Bead.Status == StatusNeedsMe {
		// A human decision supersedes an earlier ready notice. After the
		// answer the same head needs a fresh announcement.
		s.ReadyKey = ""
	}
	if t.PR != nil {
		s.PRNumber, s.PRState = t.PR.Number, t.PR.State
		s.InboxPRKey = inboxPRKey(t)
		// A PR is ready to announce only once nobody is waiting on TJ: a
		// worker at needs_me leaves auto-merge off for the decision it asked for,
		// and the announcement follows the answer.
		if k := readyKey(t); k != "" && t.Bead.Status != StatusNeedsMe {
			s.ReadyKey = k
		}
		if k := greenKey(t); k != "" && t.Bead.Status != StatusNeedsMe {
			s.GreenKey = k
		}
		if t.PR.State == "OPEN" && t.PR.AutoMerge != nil {
			s.AutoMerge = t.PR.URL
		}
	}
	rkReview := ""
	if t.Bead.Status != StatusNeedsMe {
		rkReview = reviewReadyKey(t)
	}
	switch {
	case rkReview != "" && rkReview != prev.ReviewReadyKey:
		// Newly ready for review (or baselined on first sight / pre-upgrade).
		s.ReviewReadyKey = rkReview
		s.ReviewReadyAt = now
		s.ReviewReadyReviews = t.Reviews
		s.ReviewReadyReviewed = false
		s.ReviewBumpKey = ""
	case rkReview != "" && rkReview == prev.ReviewReadyKey:
		// Still ready for review on the exact same PR and head.
		if s.ReviewReadyAt.IsZero() {
			s.ReviewReadyAt = now
		}
		if !first && reviewBumpDue(t, prev, rkReview, now, opts.reviewBump) {
			s.ReviewBumpKey = rkReview
		}
	default:
		// Not currently ready for review. Keep ReviewReadyKey and ReviewBumpKey
		// so transient blips or later recovery on the same head never duplicate
		// ready_for_review or review_bump. Keep ReviewReadyAt across transient
		// observation gaps on the same head, and clear it on confirmed
		// ineligibility or a head change.
		sameHead := t.PR == nil || (t.PR.HeadSHA != "" && fmt.Sprintf("%d@%s", t.PR.Number, t.PR.HeadSHA) == prev.ReviewReadyKey)
		if !sameHead || !reviewReadyTransient(t) {
			s.ReviewReadyAt = time.Time{}
		}
	}
	return s
}

// transition compares a thread against its last snapshot. first is true the
// first time the ticker sees a bead, when nothing counts as a change.
func transition(t Thread, prev Snapshot, first bool, now time.Time) Outcome {
	return transitionWith(t, prev, first, now, transitionOpts{})
}

func transitionDebounced(t Thread, prev Snapshot, first bool, now time.Time, debounce, flap time.Duration) Outcome {
	return transitionWith(t, prev, first, now, transitionOpts{debounce: debounce, flap: flap})
}

func transitionWith(t Thread, prev Snapshot, first bool, now time.Time, opts transitionOpts) Outcome {
	debounce, flap := opts.debounce, opts.flap
	var o Outcome
	if first {
		return o
	}
	id := t.Bead.ID
	g := classify(t)
	ev := func(k EventKind, format string, a ...any) {
		o.Events = append(o.Events, Event{Bead: id, Kind: k, Summary: fmt.Sprintf(format, a...), At: now, Clears: eventCondition(t, k)})
	}
	// Warn only when native auto-merge is newly armed on a base branch that
	// requires no approving review (never on first sight or for PRs already
	// armed before this check).
	if k := unsafeAutoKey(t); k != "" && k != prev.UnsafeAutoKey && prev.AutoMerge != t.PR.URL {
		o.Notify = true
		ev(EventUnsafeAutoMerge, "PR #%d has unsafe native auto-merge enabled", t.PR.Number)
		o.Prompts = append(o.Prompts, fmt.Sprintf(unsafeAutoPrompt+"%s: the base branch requires no approving review and the current head is not authorized and ready. Disable it with `gh pr merge %s --disable-auto`. This warning grants no merge permission.", k, t.PR.URL))
	}
	// Only a needs_me that was already settled, with the agent at rest, counts:
	// the turn that asked the question is itself still working.
	if wokeAfterNeedsMe(t, prev) && !t.Unprompted {
		o.Resume = true
		ev(EventResumed, "%s went back to work after needs_me; set it to in_progress", id)
	}
	// A blocked agent can move from one prompt to the next between ticks.
	newPrompt := t.Prompt != nil && t.Prompt.Hash != prev.PromptHash
	promptResolved := prev.AgentStatus == "blocked" && (t.Agent == nil || t.Agent.Status != "blocked")
	// A new question on a waiting bead (or an unannounced question revealed
	// when a prompt clears) surfaces once the worker is not at a prompt or in
	// an unprompted background wake. asked() ignores unrelated/AUTO notes.
	newQuestion := t.Bead.Status == StatusNeedsMe && !t.Unprompted && (t.Agent == nil || t.Agent.Status != "blocked") &&
		(promptResolved || (prev.Asked != "" && prev.Asked != asked(t.Bead)))
	// A worker prompt under the debounce surfaces only when its window
	// expires with it still showing; a prompt pending when its bead is set
	// to needs_me is exempt and surfaces at once. Every other needs-you
	// entry (a needs_me question above all) is undebounced.
	surface := prev.Group != GroupNeedsYou || newPrompt || newQuestion || (t.Bead.Status == StatusNeedsMe && prev.PromptPending != "")
	if promptDebounced(t, debounce) {
		surface = trackPrompt(t, prev, now, debounce, flap).due
	}
	if g == GroupNeedsYou && surface {
		o.Notify = true
		switch {
		case t.Prompt != nil:
			p := t.Prompt
			countdown := ""
			if p.Countdown != "" {
				countdown = ", auto-denies in " + p.Countdown
			}
			ev(EventNeedsYou, "%s is at an approval prompt [%s]: %s (%s%s); answer with `quartermaster answer %s <choice> --expect %s`",
				id, p.Hash, p.Summary(), p.Choices(), countdown, id, p.Hash)
		case t.Agent != nil && t.Agent.Status == "blocked":
			ev(EventNeedsYou, "%s is waiting at an approval or question prompt", id)
		case prev.Asked != "" && prev.Asked == asked(t.Bead):
			// Back at rest after waking on its own: the question is the one
			// already announced.
			o.Notify = false
		default:
			ev(EventNeedsYou, "%s is marked needs_me; its latest note holds the question", id)
		}
	}
	if t.PR != nil && t.PR.State == "OPEN" {
		if len(t.Checks.Failing) > 0 && (!slices.Equal(t.Checks.Failing, prev.Failing) || (prev.InboxPRKey != "" && prev.InboxPRKey != inboxPRKey(t))) {
			ev(EventFailing, "PR #%d checks failing: %s", t.PR.Number, strings.Join(t.Checks.Failing, ", "))
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] PR #%d has failing checks: %s. Investigate with `gh pr checks %d`, fix them on this branch, and push.",
				t.PR.Number, strings.Join(t.Checks.Failing, ", "), t.PR.Number))
		}
		review := func(what string) {
			ev(EventReview, "PR #%d has %s", t.PR.Number, what)
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] PR #%d has %s. Read it with `gh pr view %d --comments` and the review threads; fix what is valid, reply to what is not, and push.",
				t.PR.Number, what, t.PR.Number))
		}
		// Snapshots from before bots were told apart counted them as human
		// reviews; the first pass after upgrading only sets the baseline.
		comparable := prev.PRNumber == t.PR.Number && prev.ReviewsSplit
		if t.Reviews > prev.Reviews && comparable {
			o.Notify = true
			review("new human review feedback")
		}
		if len(t.BotReviews) > prev.BotReviews && comparable {
			review("a new review from " + strings.Join(slices.Compact(slices.Sorted(slices.Values(t.BotReviews[prev.BotReviews:]))), ", "))
		}
		if k := readyKey(t); k != "" && k != prev.ReadyKey && t.Bead.Status != StatusNeedsMe {
			o.Notify = true
			ev(EventReady, "PR #%d is ready to merge (%s)", t.PR.Number, approval(t.PR))
			if t.PR.AutoMerge != nil {
				o.Prompts = append(o.Prompts, fmt.Sprintf(
					readyPrompt+"%d is ready to merge at %s: %s, no review thread is open, and its required checks passed. Auto-merge is on, so GitHub merges it; don't merge it yourself. Finish anything left before merge (the Linear issue, your report). The ticker tells you when it merges.",
					t.PR.Number, t.PR.HeadSHA, approval(t.PR)))
			} else {
				o.Prompts = append(o.Prompts, fmt.Sprintf(
					readyPrompt+"%d is ready to merge at %s: %s, no review thread is open, and its required checks passed. Finish anything left before merge (the Linear issue, your report), then merge it with `gh pr merge %d --match-head-commit %s` only if you were told to merge; otherwise say it is ready to merge and stop. The ticker tells you when it merges.",
					t.PR.Number, t.PR.HeadSHA, approval(t.PR), t.PR.Number, t.PR.HeadSHA))
			}
		}
		if k := reviewReadyKey(t); k != "" && prev.ReviewReadyTracked && t.Bead.Status != StatusNeedsMe {
			switch {
			case k != prev.ReviewReadyKey:
				ev(EventReadyForReview, "%s", formatReadyForReviewSummary(*t.PR))
			case reviewBumpDue(t, prev, k, now, opts.reviewBump):
				ev(EventReviewBump, "%s", formatReviewBumpSummary(*t.PR, prev.ReviewReadyAt, now))
			}
		}
		// GitHub turns auto-merge off when the base branch changes or someone
		// without write access pushes. A draft, a PR waiting on a human
		// decision, or a PR on a branch requiring no approving review is meant
		// to have it off.
		if prev.AutoMerge != "" && prev.AutoMerge == t.PR.URL && t.PR.AutoMerge == nil && !t.PR.IsDraft && t.Bead.Status != StatusNeedsMe && requiresGitHubReview(t.PR) {
			ev(EventAutoMergeOff, "PR #%d auto-merge was turned off", t.PR.Number)
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] PR #%d no longer has auto-merge on; GitHub turns it off when the base branch changes or someone without write access pushes. If you disabled it intentionally, leave it off; otherwise turn it back on with `gh pr merge %s --auto --squash` once your merge check passes.",
				t.PR.Number, t.PR.URL))
		}
	}
	if news := newlyMerged(t, prev); len(news) > 0 {
		o.Notify = true
		var nums []string
		for _, pr := range news {
			ev(EventMerged, "PR #%d merged", pr.Number)
			nums = append(nums, fmt.Sprintf("#%d", pr.Number))
		}
		what := "PR " + strings.Join(nums, ", ")
		switch {
		case t.Bead.HasLabel(LabelRollingOut):
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] %s merged. Continue the rollout with its next step. When the rollout is finished and verified, close the bead with `quartermaster close %s --by worker --reason \"<what shipped and how it was verified>\"`; it runs the verifier against the bead's acceptance criteria and removes the %s label once it passes.",
				what, id, LabelRollingOut))
		case t.PR != nil && t.PR.State == "OPEN":
			// Another of the bead's PRs is still open; closing waits for it.
		case t.OpenChildren > 0:
			// Open child beads remain; closing waits for them.
		case t.Bead.Status != StatusClosed:
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] %s merged. Verify what needs verifying after merge, then close the bead with `quartermaster close %s --by worker --reason \"<what shipped and how it was verified>\"` and stop.",
				what, id))
		}
	}
	for _, r := range finishedRuns(t.Runs, prev.Runs) {
		switch {
		case r.Conclusion == "success":
			ev(EventRunSucceeded, "%s run %d succeeded", r.Workflow, r.ID)
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] %s run %s succeeded. Continue with the next step of the rollout.", r.Workflow, r.URL))
		case runFailed(r):
			o.Notify = true
			ev(EventRunFailed, "%s run %d ended %s", r.Workflow, r.ID, r.Conclusion)
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] %s run %s ended %s. Investigate with `gh run view %d -R %s --log-failed`, fix what is wrong, and carry on with the rollout.",
				r.Workflow, r.URL, r.Conclusion, r.ID, r.Repo))
		}
	}
	if turnFinishedOnTick(t, prev) {
		if t.Turn.Failed() {
			ev(EventFinished, "%s stopped on %s%s (%s)", id, t.Turn.StopReason, errSuffix(t.Turn.Error), stateLine(t))
		} else {
			ev(EventFinished, "%s finished a turn (%s)", id, stateLine(t))
		}
	}
	// A relaunch asked it to exit, and the ticker resumes it itself.
	if t.Agent == nil && prev.AgentStatus != "" && !prev.Relaunch {
		ev(EventAgentGone, "%s's agent exited while the bead is still %s", id, t.Bead.Status)
	}
	return o
}

// turnFinishedOnTick reports whether t's ready agent completed a turn since prev.
func turnFinishedOnTick(t Thread, prev Snapshot) bool {
	if t.Agent == nil || !agentReady(t.Agent) || classify(t) == GroupNeedsYou {
		return false
	}
	// A whole turn can fit between two ticks; a moved state counter on a ready
	// agent means it worked since the last look.
	turned := prev.AgentStatus == "working" || (prev.AgentSeq != 0 && t.Agent.Seq != prev.AgentSeq)
	// A pi session's last turn end is a durable cursor: a new one is a new
	// turn, whatever herdr's state counter did.
	if t.Turn != nil && prev.TurnID != "" && t.Turn.ID != prev.TurnID {
		turned = true
	}
	return turned
}

// mergedPRs is every merged PR of t's: its merged list and its pick.
func mergedPRs(t Thread) []PR {
	prs := slices.Clone(t.Merged)
	if t.PR != nil && t.PR.State == "MERGED" && !slices.ContainsFunc(prs, func(p PR) bool { return p.URL == t.PR.URL }) {
		prs = append(prs, *t.PR)
	}
	return prs
}

// newlyMerged is t's merged PRs that prev has not announced. A snapshot from
// before Merged was kept knew only its own PR, so the first pass after
// upgrading announces only the pick, as before.
func newlyMerged(t Thread, prev Snapshot) []PR {
	if !prev.MergedTracked {
		if t.PR != nil && t.PR.State == "MERGED" && !(prev.PRNumber == t.PR.Number && prev.PRState == "MERGED") {
			return []PR{*t.PR}
		}
		return nil
	}
	var news []PR
	for _, pr := range mergedPRs(t) {
		if !slices.Contains(prev.Merged, pr.URL) {
			news = append(news, pr)
		}
	}
	return news
}

const readyPrompt = "[" + toolName + ": automated, not the user] PR #"

// cutReadyPrompt is the rest of a ready-to-merge prompt after its prefix. A
// prompt queued under an old name, before the rename, counts too.
func cutReadyPrompt(p string) (string, bool) {
	for _, n := range append([]string{toolName}, legacyNames...) {
		if rest, ok := strings.CutPrefix(p, "["+n+": automated, not the user] PR #"); ok && strings.Contains(rest, " is ready to merge at ") {
			return rest, true
		}
	}
	return "", false
}

// readyKey names the PR and head commit a thread is ready to merge at, or ""
// when it isn't ready.
func readyKey(t Thread) string {
	if !readyToMerge(t.PR, t.Checks) {
		return ""
	}
	return fmt.Sprintf("%d@%s", t.PR.Number, t.PR.HeadSHA)
}

// greenKey names the PR and head commit of an open non-draft PR whose checks
// are all green and that is awaiting review (no failing or pending checks, no
// requested changes, no merge conflicts, and no known open review threads).
func greenKey(t Thread) string {
	if t.PR == nil || t.PR.State != "OPEN" || t.PR.IsDraft {
		return ""
	}
	if t.Checks.Passing == 0 || t.Checks.Pending > 0 || len(t.Checks.Failing) > 0 {
		return ""
	}
	if t.PR.ReviewDecision == "CHANGES_REQUESTED" {
		return ""
	}
	if t.PR.MergeState == "DIRTY" {
		return ""
	}
	if t.PR.Gate != nil && t.PR.Gate.OpenThreads > 0 {
		return ""
	}
	return fmt.Sprintf("%d@%s", t.PR.Number, t.PR.HeadSHA)
}

// reviewReadyKey names the PR and head commit of an open, non-draft, green,
// mergeable, REVIEW_REQUIRED PR with fresh gate evidence confirming zero
// unresolved review threads, or "" otherwise.
func reviewReadyKey(t Thread) string {
	if !readyForReview(t.PR, t.Checks) {
		return ""
	}
	return fmt.Sprintf("%d@%s", t.PR.Number, t.PR.HeadSHA)
}

// reviewReadyTransient reports whether a thread currently lacks fresh, settled
// GitHub observations (API failure, missing/stale gate, or UNKNOWN merge state)
// rather than being confirmed ineligible for review.
func reviewReadyTransient(t Thread) bool {
	if t.Bead.Status == StatusNeedsMe {
		return false
	}
	if t.PR == nil {
		return true
	}
	if t.PR.State != "OPEN" || t.PR.IsDraft || t.PR.HeadSHA == "" {
		return false
	}
	if !t.PR.DetailsFresh {
		return true
	}
	if t.PR.ReviewDecision != "REVIEW_REQUIRED" {
		return false
	}
	if t.Checks.Pending > 0 || len(t.Checks.Failing) > 0 || t.Checks.Passing == 0 {
		return false
	}
	if t.PR.Mergeable == "CONFLICTING" || t.PR.MergeState == "DIRTY" || t.PR.MergeState == "DRAFT" {
		return false
	}
	if t.PR.Mergeable == "UNKNOWN" || t.PR.MergeState == "UNKNOWN" {
		return true
	}
	if t.PR.Gate == nil || t.PR.Gate.Head != t.PR.HeadSHA {
		return true
	}
	return false
}

func reviewReadyReviewed(t Thread, prev Snapshot) bool {
	if prev.ReviewReadyReviewed {
		return true
	}
	if t.PR == nil || prev.ReviewReadyKey == "" || !prev.ReviewsSplit {
		return false
	}
	if fmt.Sprintf("%d@%s", t.PR.Number, t.PR.HeadSHA) != prev.ReviewReadyKey {
		return false
	}
	return t.Reviews > prev.ReviewReadyReviews
}

func reviewBumpDue(t Thread, prev Snapshot, k string, now time.Time, after time.Duration) bool {
	if after <= 0 || k == "" || !prev.ReviewReadyTracked || k != prev.ReviewReadyKey {
		return false
	}
	if t.Bead.Status == StatusNeedsMe {
		return false
	}
	if prev.ReviewBumpKey == k || prev.ReviewReadyAt.IsZero() {
		return false
	}
	if reviewReadyReviewed(t, prev) {
		return false
	}
	return now.Sub(prev.ReviewReadyAt) >= after
}

func formatReadyForReviewSummary(pr PR) string {
	s := fmt.Sprintf("PR #%d is ready for review at %s (checks green, mergeable, 0 open threads)", pr.Number, short(pr.HeadSHA))
	if pr.URL != "" {
		s += ": " + pr.URL
	}
	return s
}

func formatReviewBumpSummary(pr PR, since, now time.Time) string {
	waited := age(since, now)
	if waited == "" {
		waited = "review_bump_hours"
	}
	s := fmt.Sprintf("PR #%d is still awaiting review at %s after %s with no new review", pr.Number, short(pr.HeadSHA), waited)
	if pr.URL != "" {
		s += ": " + pr.URL
	}
	return s
}

// freshPending drops a queued ready-to-merge prompt once its PR and head are
// no longer the ones ready to merge: a push or a new review since it was
// queued must not reach the worker as a go-ahead.
func freshPending(t Thread, pending []string) []string {
	var keep []string
	for _, p := range pending {
		if rest, ok := cutReadyPrompt(p); ok {
			k := readyKey(t)
			if k == "" || !strings.HasPrefix(rest, strings.Replace(k, "@", " is ready to merge at ", 1)+":") {
				continue
			}
		}
		if rest, ok := strings.CutPrefix(p, unsafeAutoPrompt); ok {
			if k := unsafeAutoKey(t); k == "" || !strings.HasPrefix(rest, k+":") {
				continue
			}
		}
		keep = append(keep, p)
	}
	return keep
}

const unsafeAutoPrompt = "[" + toolName + ": automated, not the user] Native auto-merge on PR #"

// unsafeAutoKey names an open PR with native auto-merge armed on a base branch
// that requires no approving review (unless the current head is already
// authorized and ready to merge).
func unsafeAutoKey(t Thread) string {
	if t.PR == nil || t.PR.State != "OPEN" || t.PR.AutoMerge == nil {
		return ""
	}
	if requiresGitHubReview(t.PR) {
		return ""
	}
	if t.Bead.Status == StatusInProgress && readyToMerge(t.PR, t.Checks) {
		return ""
	}
	return fmt.Sprintf("%d@%s", t.PR.Number, t.PR.HeadSHA)
}

func approval(pr *PR) string {
	if pr.ReviewDecision == "APPROVED" {
		return "approved"
	}
	if pr.Gate != nil && freshApproval(pr.Gate.Reviews, pr.Gate) {
		return "current-head approved"
	}
	return "approval required by policy"
}

// deliverable reports whether the ticker may type into an agent now: it has
// been ready, untouched, for at least idle.
func deliverable(s Snapshot, now time.Time, idle time.Duration) bool {
	return !s.ReadySince.IsZero() && now.Sub(s.ReadySince) >= idle
}

// coordinatorRank sorts the coordinator above every thread: thread ranks start
// with a digit, and '!' sorts before digits.
const coordinatorRank = "!coordinator"

// coordinatorLine is the coordinator's sidebar row: what it has to deal with.
func coordinatorLine(threads []Thread, inbox int) string {
	counts := map[Group]int{}
	for _, t := range threads {
		counts[classify(t)]++
	}
	s := "coordinator"
	for _, g := range []Group{GroupNeedsYou, GroupReady, GroupFailing, GroupReview, GroupWorking} {
		if n := counts[g]; n > 0 {
			label := g.String()
			if g == GroupNeedsYou {
				label = "need you"
			}
			s += fmt.Sprintf(" · %d %s", n, label)
		}
	}
	if inbox > 0 {
		s += fmt.Sprintf(" · %d inbox", inbox)
	}
	return s
}

// errSuffix is ": <err>" shortened for an event line, or nothing.
func errSuffix(err string) string {
	if err = strings.TrimSpace(err); err == "" {
		return ""
	}
	return ": " + shortTitle(err, 120)
}
