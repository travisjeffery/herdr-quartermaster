package main

import (
	"fmt"
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
	Bead       Bead
	Agent      *Agent
	PR         *PR
	Checks     Checks
	Reviews    int // human reviews by someone other than the user
	BotReviews []string
	Runs       []Run
	Linear     string
	// Merged is every merged PR of the bead's, the pick or not, so a merge
	// behind another open PR is still announced.
	Merged []PR
	// Auto is whether the bead runs without TJ right now (autonomous mode).
	Auto bool
	// Prompt is the approval prompt a blocked agent is showing, when the
	// ticker could read it.
	Prompt *ApprovalPrompt
}

func agentReady(a *Agent) bool {
	return a != nil && (a.Status == "idle" || a.Status == "done")
}

// answered reports a needs_me bead whose agent is working again: the user
// answered it in its pane, or it is still in the turn that asked. Either way
// nobody is waiting on the user yet.
func answered(t Thread) bool {
	return t.Bead.Status == StatusNeedsMe && t.Agent != nil && t.Agent.Status == "working"
}

// classify decides a thread's group. Precedence matters: a human owed an
// answer outranks everything, and a merged PR outranks review because the
// only thing left is closing up.
func classify(t Thread) Group {
	switch {
	case t.Bead.Status == StatusNeedsMe && !answered(t), t.Agent != nil && t.Agent.Status == "blocked":
		return GroupNeedsYou
	case t.PR != nil && t.PR.State == "MERGED" && t.Bead.HasLabel(LabelRollingOut):
		return GroupRollingOut
	case t.PR != nil && t.PR.State == "MERGED":
		return GroupMerged
	case readyToMerge(t.PR, t.Checks):
		return GroupReady
	case t.PR != nil && t.PR.State == "OPEN" && len(t.Checks.Failing) > 0:
		return GroupFailing
	case t.Agent != nil && t.Agent.Status == "working":
		return GroupWorking
	case t.PR != nil && t.PR.State == "OPEN" && agentReady(t.Agent):
		return GroupReview
	case t.Bead.Status == StatusBlocked:
		return GroupBlocked
	case agentReady(t.Agent), t.Agent != nil:
		return GroupIdle
	default:
		return GroupNoAgent
	}
}

// pendingCommand is the command a worker asked the user to run, from its
// latest "RUN:" note, until a later "RAN:" or "DONE:" note answers it.
func pendingCommand(notes string) string {
	var lines []string
	for _, l := range strings.Split(notes, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	// A RUN: is only live while it is one of the last two notes (the command
	// and its one line of why). Anything later means the thread moved on, so
	// an answered command is never offered again even without a RAN: line.
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-2; i-- {
		l := lines[i]
		if strings.HasPrefix(l, "RAN:") || strings.HasPrefix(l, "DONE:") {
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
func runCommand(t Thread) string {
	if t.Bead.Status != StatusNeedsMe || answered(t) {
		return ""
	}
	return pendingCommand(t.Bead.Notes)
}

func stateLine(t Thread) string {
	g := classify(t)
	s := g.String()
	if answered(t) {
		s += " · needs_me"
	}
	if runCommand(t) != "" {
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
	if t.Auto {
		s += " · auto"
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
	Failing     []string  `json:"failing"`
	Reviews     int       `json:"reviews"`
	BotReviews  int       `json:"bot_reviews"`
	// ReviewsSplit marks a snapshot that counted bot and human reviews apart.
	ReviewsSplit bool              `json:"reviews_split"`
	Runs         map[string]string `json:"runs,omitempty"`
	Pending      []string          `json:"pending_prompts"`
	// NeedsMe is the bead's needs_me status at this snapshot.
	NeedsMe bool `json:"needs_me"`
	// ReadyKey is the PR and head commit last seen ready to merge (readyKey),
	// so ready_to_merge fires once per push and again for a replacement PR.
	ReadyKey string `json:"ready_key,omitempty"`
	// Merged holds the URL of every PR of the bead's already announced as
	// merged, so a bead with several PRs hears of each merge once.
	// MergedTracked marks a snapshot that kept it; older ones knew only their
	// own PR's state.
	Merged        []string `json:"merged,omitempty"`
	MergedTracked bool     `json:"merged_tracked,omitempty"`
	// AutoMerge is the URL of the open PR last seen with auto-merge on, so the
	// worker hears once when GitHub turns it off.
	AutoMerge string `json:"auto_merge,omitempty"`
	// PromptHash is the approval prompt last announced, so each prompt a
	// blocked agent shows reaches the inbox once.
	PromptHash string `json:"prompt_hash,omitempty"`
}

type EventKind string

const (
	EventNeedsYou     EventKind = "needs_you"
	EventFailing      EventKind = "checks_failing"
	EventReview       EventKind = "new_review"
	EventMerged       EventKind = "merged"
	EventReady        EventKind = "ready_to_merge"
	EventFinished     EventKind = "finished"
	EventAgentGone    EventKind = "agent_gone"
	EventResumed      EventKind = "resumed"
	EventRunSucceeded EventKind = "run_succeeded"
	EventRunFailed    EventKind = "run_failed"
	EventLikelyStale  EventKind = "likely_stale"
	EventAutoClosed   EventKind = "auto_closed"
	EventAutoMergeOff EventKind = "auto_merge_off"
)

type Event struct {
	Bead    string    `json:"bead"`
	Kind    EventKind `json:"kind"`
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
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

// resumeNote is the note left on a bead the ticker resumed; the question
// stays in the notes above it.
const resumeNote = "auto: agent resumed after needs_me"

func snapshot(t Thread, prev Snapshot, now time.Time) Snapshot {
	s := Snapshot{Group: classify(t), Reviews: t.Reviews, BotReviews: len(t.BotReviews), ReviewsSplit: true, Runs: runStates(t.Runs), Failing: t.Checks.Failing, Pending: freshPending(t, prev.Pending), NeedsMe: t.Bead.Status == StatusNeedsMe, ReadyKey: prev.ReadyKey}
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
	if t.Prompt != nil {
		s.PromptHash = t.Prompt.Hash
	}
	s.Merged, s.MergedTracked = slices.Clone(prev.Merged), true
	for _, pr := range mergedPRs(t) {
		if !slices.Contains(s.Merged, pr.URL) {
			s.Merged = append(s.Merged, pr.URL)
		}
	}
	if t.PR != nil {
		s.PRNumber, s.PRState = t.PR.Number, t.PR.State
		if k := readyKey(t); k != "" {
			s.ReadyKey = k
		}
		if t.PR.State == "OPEN" && t.PR.AutoMerge != nil {
			s.AutoMerge = t.PR.URL
		}
	}
	return s
}

// transition compares a thread against its last snapshot. first is true the
// first time the ticker sees a bead, when nothing counts as a change.
func transition(t Thread, prev Snapshot, first bool, now time.Time) Outcome {
	var o Outcome
	if first {
		return o
	}
	id := t.Bead.ID
	g := classify(t)
	ev := func(k EventKind, format string, a ...any) {
		o.Events = append(o.Events, Event{Bead: id, Kind: k, Summary: fmt.Sprintf(format, a...), At: now})
	}
	// Only a needs_me that was already settled, with the agent at rest, counts:
	// the turn that asked the question is itself still working.
	if answered(t) && prev.NeedsMe && (agentReady(&Agent{Status: prev.AgentStatus}) || prev.AgentStatus == "blocked") {
		o.Resume = true
		ev(EventResumed, "%s went back to work after needs_me; set it to in_progress", id)
	}
	// A blocked agent can move from one prompt to the next between ticks.
	newPrompt := t.Prompt != nil && t.Prompt.Hash != prev.PromptHash
	if g == GroupNeedsYou && (prev.Group != GroupNeedsYou || newPrompt) {
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
		default:
			ev(EventNeedsYou, "%s is marked needs_me; its latest note holds the question", id)
		}
	}
	if t.PR != nil && t.PR.State == "OPEN" {
		if len(t.Checks.Failing) > 0 && !slices.Equal(t.Checks.Failing, prev.Failing) {
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
		if k := readyKey(t); k != "" && k != prev.ReadyKey {
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
		// GitHub turns auto-merge off when the base branch changes or someone
		// without write access pushes. A draft or a PR waiting on a human
		// decision is meant to have it off.
		if prev.AutoMerge != "" && prev.AutoMerge == t.PR.URL && t.PR.AutoMerge == nil && !t.PR.IsDraft && t.Bead.Status != StatusNeedsMe {
			ev(EventAutoMergeOff, "PR #%d auto-merge was turned off", t.PR.Number)
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] PR #%d no longer has auto-merge on; GitHub turns it off when the base branch changes or someone without write access pushes. Unless you turned it off on purpose, turn it back on with `gh pr merge %s --auto --squash`.",
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
				"[quartermaster: automated, not the user] %s merged. Continue the rollout with its next step. When the rollout is finished and verified, remove the label with `bd label remove %s %s`, then close the bead with `bd close %s --reason \"<what shipped and how it was verified>\"`.",
				what, id, LabelRollingOut, id))
		case t.PR != nil && t.PR.State == "OPEN":
			// Another of the bead's PRs is still open; closing waits for it.
		case t.Bead.Status != StatusClosed:
			o.Prompts = append(o.Prompts, fmt.Sprintf(
				"[quartermaster: automated, not the user] %s merged. Verify what needs verifying after merge, then close the bead with `bd close %s --reason \"<what shipped and how it was verified>\"` and stop.",
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
	// A whole turn can fit between two ticks; a moved state counter on a ready
	// agent means it worked since the last look.
	turned := prev.AgentStatus == "working" || (prev.AgentSeq != 0 && t.Agent != nil && t.Agent.Seq != prev.AgentSeq)
	if t.Agent != nil && agentReady(t.Agent) && turned && g != GroupNeedsYou {
		ev(EventFinished, "%s finished a turn (%s)", id, stateLine(t))
	}
	if t.Agent == nil && prev.AgentStatus != "" {
		ev(EventAgentGone, "%s's agent exited while the bead is still %s", id, t.Bead.Status)
	}
	return o
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
		keep = append(keep, p)
	}
	return keep
}

func approval(pr *PR) string {
	if pr.ReviewDecision == "APPROVED" {
		return "approved"
	}
	return "no review required"
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
