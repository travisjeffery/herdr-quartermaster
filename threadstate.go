package main

import "time"

// ThreadState is a read-only projection of one observation, not a lifecycle or
// merge authorization. All views use it; transitions still own side effects.
type ThreadState struct {
	Kind       ThreadKind
	Waiting    WaitOn
	Command    string
	PR         *PR
	Gate       DeliveryGate
	RollingOut bool
	Next       bool
	ActiveTurn bool // needs_me was set during a turn, or the user answered it
	Agent      string
	Blockers   []string
	UpdatedAt  time.Time
}

type ThreadKind string

const (
	ThreadWaiting    ThreadKind = "waiting"
	ThreadDelivering ThreadKind = "delivering"
	ThreadMerged     ThreadKind = "merged"
	ThreadWorking    ThreadKind = "working"
	ThreadIdle       ThreadKind = "idle"
	ThreadNoAgent    ThreadKind = "no_agent"
	ThreadBlocked    ThreadKind = "blocked"
	ThreadQueued     ThreadKind = "queued"
)

type WaitOn string

const (
	WaitUser    WaitOn = "user"
	WaitPrompt  WaitOn = "prompt"
	WaitCommand WaitOn = "command"
)

type DeliveryGate string

const (
	GateReview  DeliveryGate = "review"
	GateReady   DeliveryGate = "ready"
	GateFailing DeliveryGate = "failing"
)

// deriveThreadState is the precedence table for every view (first match wins):
//
//  1. blocked agent -> waiting on a prompt
//  2. needs_me without an active, prompted turn -> waiting on command/user
//  3. unstarted open bead -> blocked by dependencies, else queued (next/backlog)
//  4. explicit blocked status -> blocked
//  5. merged PR -> merged, with the rolling-out facet
//  6. ready PR -> delivering/ready (readiness is owned by the merge policy)
//  7. failing open PR -> delivering/failing
//  8. working agent -> working
//  9. open PR with a ready agent -> delivering/review
// 10. present agent -> idle (unknown is not proof the agent is absent)
// 11. otherwise -> no agent
//
// An open bead's idle agent and historical PRs do not start its work.
// Background compaction is not an answer to needs_me. Missing evidence retains
// the existing active-turn behavior; this function never resumes a bead.
func deriveThreadState(t Thread) ThreadState {
	s := ThreadState{PR: t.PR, RollingOut: t.Bead.HasLabel(LabelRollingOut),
		Next: t.Bead.HasLabel(LabelNext), ActiveTurn: answered(t),
		Blockers: t.Blockers, UpdatedAt: t.Bead.UpdatedAt}
	if t.Agent != nil {
		s.Agent = t.Agent.Status
	}
	switch {
	case s.Agent == "blocked":
		s.Kind, s.Waiting = ThreadWaiting, WaitPrompt
		if t.Bead.Status == StatusNeedsMe {
			s.Command = pendingCommand(t.Bead.Notes)
		}
	case t.Bead.Status == StatusNeedsMe && !s.ActiveTurn:
		s.Kind, s.Waiting = ThreadWaiting, WaitUser
		if s.Command = pendingCommand(t.Bead.Notes); s.Command != "" {
			s.Waiting = WaitCommand
		}
	case t.Bead.Status == StatusOpen && s.Agent != "working":
		s.Kind = ThreadQueued
		if len(s.Blockers) > 0 {
			s.Kind = ThreadBlocked
		}
	case t.Bead.Status == StatusBlocked:
		s.Kind = ThreadBlocked
	case t.PR != nil && t.PR.State == "MERGED":
		s.Kind = ThreadMerged
	case readyToMerge(t.PR, t.Checks):
		s.Kind, s.Gate = ThreadDelivering, GateReady
	case t.PR != nil && t.PR.State == "OPEN" && len(t.Checks.Failing) > 0:
		s.Kind, s.Gate = ThreadDelivering, GateFailing
	case s.Agent == "working":
		s.Kind = ThreadWorking
	case t.PR != nil && t.PR.State == "OPEN" && agentReady(t.Agent):
		s.Kind, s.Gate = ThreadDelivering, GateReview
	case t.Agent != nil:
		s.Kind = ThreadIdle
	default:
		s.Kind = ThreadNoAgent
	}
	return s
}

// derive freezes the projection after gathering evidence. Re-derive only when
// the observation changes (for example, the ticker successfully resumes it).
func (t Thread) derive() Thread {
	s := deriveThreadState(t)
	t.state = &s
	return t
}

func (t Thread) derived() ThreadState {
	if t.state != nil {
		return *t.state
	}
	// Raw observations are also useful to pure callers and fixtures.
	return deriveThreadState(t)
}

func (s ThreadState) group() Group {
	switch s.Kind {
	case ThreadWaiting:
		return GroupNeedsYou
	case ThreadMerged:
		if s.RollingOut {
			return GroupRollingOut
		}
		return GroupMerged
	case ThreadDelivering:
		switch s.Gate {
		case GateReady:
			return GroupReady
		case GateFailing:
			return GroupFailing
		default:
			return GroupReview
		}
	case ThreadWorking:
		return GroupWorking
	case ThreadIdle:
		return GroupIdle
	case ThreadBlocked:
		return GroupBlocked
	case ThreadQueued:
		if s.Next {
			return sectionNext
		}
		return sectionBacklog
	default:
		return GroupNoAgent
	}
}

// stale is display age, not permission to release a claim or close a bead.
// Those lifecycle operations deliberately have their own evidence and gates.
func (s ThreadState) stale(now time.Time) bool {
	if s.UpdatedAt.IsZero() {
		return false
	}
	var after time.Duration
	switch s.Kind {
	case ThreadWaiting:
		after = staleNeedsYou
	case ThreadBlocked:
		after = staleBlocked
	case ThreadQueued:
		return false
	default:
		after = staleInProgress
	}
	return now.Sub(s.UpdatedAt) > after
}

// restoreThreadEvidence carries a known background wake across views and
// ticker passes, only for the exact same question and agent observation.
func restoreThreadEvidence(t Thread, prev Snapshot) Thread {
	t.Unprompted = prev.Unprompted && t.Bead.Status == StatusNeedsMe &&
		t.Agent != nil && t.Agent.Status == "working" &&
		t.Agent.Status == prev.AgentStatus && t.Agent.Seq == prev.AgentSeq && asked(t.Bead) == prev.Asked
	return t
}
