package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	defaultStallMaxRetries     = 2
	defaultStallBackoffSeconds = 30
	stallRetryMaxBackoff       = 10 * time.Minute
)

// StallRetryConfig is [stall_retry] in config.toml: bounded automatic recovery
// when a pi worker stops a turn on a transient ACP stall error.
type StallRetryConfig struct {
	// MaxRetries is the maximum number of automatic nudges per stall episode
	// before escalating to the inbox; 0 disables auto-retry. Default: 2.
	MaxRetries int `toml:"max_retries"`
	// BackoffSeconds is the base cooldown before retrying a stalled turn;
	// doubles on each consecutive retry (backoff, 2*backoff, ...) capped at
	// stallRetryMaxBackoff. Default: 30.
	BackoffSeconds int `toml:"backoff_seconds"`
}

func (c StallRetryConfig) enabled() bool { return c.MaxRetries > 0 }

// backoff returns the cooldown before retry attempt n (1-based):
// base * 2^(n-1), capped at stallRetryMaxBackoff.
func (c StallRetryConfig) backoff(attempt int) time.Duration {
	if c.BackoffSeconds <= 0 {
		return 0
	}
	d := time.Duration(c.BackoffSeconds) * time.Second
	for i := 1; i < attempt && d < stallRetryMaxBackoff; i++ {
		if d > stallRetryMaxBackoff/2 {
			return stallRetryMaxBackoff
		}
		d *= 2
	}
	if d > stallRetryMaxBackoff {
		return stallRetryMaxBackoff
	}
	return d
}

func (c Config) validateStallRetry() error {
	if c.StallRetry.MaxRetries < 0 {
		return fmt.Errorf("[stall_retry] max_retries %d is negative; use 0 to disable", c.StallRetry.MaxRetries)
	}
	if c.StallRetry.BackoffSeconds < 0 {
		return fmt.Errorf("[stall_retry] backoff_seconds %d is negative", c.StallRetry.BackoffSeconds)
	}
	return nil
}

var (
	acpStallRE         = regexp.MustCompile(`(?i)\bacp\b[^\n]{0,80}\bstall(?:ed|ing)?\b`)
	stalledNoOutputRE  = regexp.MustCompile(`(?i)\bstall(?:ed)?\s+for\s+\d+[smhd]?\s+with\s+no\s+output\b`)
	agyStalledOutputRE = regexp.MustCompile(`(?i)\bantigravity\s+stalled\s+with\s+no\s+output\b`)
)

// isTransientACPStallError reports whether an assistant turn error message
// looks like a transient ACP output/inactivity stall eligible for bounded
// auto-retry. Auth, quota, rate-limit, abort-without-stall, and other errors
// return false.
func isTransientACPStallError(msg string) bool {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return false
	}
	return acpStallRE.MatchString(msg) || stalledNoOutputRE.MatchString(msg) || agyStalledOutputRE.MatchString(msg)
}

// isTransientACPStall reports whether turn ended on a transient ACP stall error.
func isTransientACPStall(turn *PiTurn) bool {
	return turn != nil && turn.Failed() && isTransientACPStallError(turn.Error)
}

type stallRetryGuard struct {
	Held        bool // durable job HOLD active on this bead
	Headless    bool // headless-owned bead
	Relaunching bool // autonomy launch-mode relaunch in progress
	LiveAsk     bool // live pi control-socket approval ask active
}

type stallRetryAction int

const (
	stallActionNone stallRetryAction = iota
	// stallActionSchedule records a pending retry after backoff and suppresses
	// the raw stopped-on-error inbox event while auto-recovery is active.
	stallActionSchedule
	// stallActionWait holds a pending retry until its backoff deadline arrives.
	stallActionWait
	// stallActionRetry delivers the retry nudge now.
	stallActionRetry
	// stallActionEscalate stops retrying (cap exhausted or retry blocked after
	// suppression) and surfaces cleanly to the inbox.
	stallActionEscalate
	// stallActionReset clears stall episode state after a healthy or non-stall turn.
	stallActionReset
)

type stallSnapshot struct {
	Retries   int
	TurnID    string
	NextAt    time.Time
	Error     string
	Escalated bool
}

func stallStateFrom(s Snapshot) stallSnapshot {
	return stallSnapshot{
		Retries:   s.StallRetries,
		TurnID:    s.StallTurnID,
		NextAt:    s.StallNextAt,
		Error:     s.StallError,
		Escalated: s.StallEscalated,
	}
}

func (s stallSnapshot) hasState() bool {
	return s.Retries > 0 || s.TurnID != "" || !s.NextAt.IsZero() || s.Error != "" || s.Escalated
}

func applyStallSnapshot(snap *Snapshot, st stallSnapshot) {
	snap.StallRetries = st.Retries
	snap.StallTurnID = st.TurnID
	snap.StallNextAt = st.NextAt
	snap.StallError = st.Error
	snap.StallEscalated = st.Escalated
}

type stallRetryPlan struct {
	Action           stallRetryAction
	SuppressFinished bool
	Attempt          int
	MaxRetries       int
	Backoff          time.Duration
	Prompt           string
	Note             string
	JournalAction    string
	JournalText      string
	EscalateEvent    *Event
	Notify           bool
	Reason           string
	Next             stallSnapshot
}

// stallRetryBlockReason returns why th must not be auto-retried, or "" when
// all safety guards pass.
func stallRetryBlockReason(th Thread, guard stallRetryGuard) string {
	switch {
	case th.Bead.Status != StatusInProgress:
		return "bead status is " + th.Bead.Status
	case guard.Held:
		return "bead is on hold"
	case guard.Headless:
		return "bead is headless-owned"
	case guard.Relaunching:
		return "worker is scheduled for relaunch"
	case th.Agent == nil:
		return "no active agent"
	case th.Agent.Status == "blocked" || th.Prompt != nil || guard.LiveAsk || classify(th) == GroupNeedsYou:
		return "worker is at an approval prompt or needs_you"
	case !agentReady(th.Agent):
		return "agent is " + th.Agent.Status
	case th.Agent.Session.Agent != "pi" && th.Agent.Kind != "pi":
		return "agent is not pi"
	case th.Turn == nil || th.Turn.ID == "":
		return "no pi turn cursor"
	default:
		return ""
	}
}

func stallRetryPrompt(attempt, maxRetries int, turnID, errMsg string) string {
	return fmt.Sprintf(
		"[%s: automated, not the user, approves nothing] Your previous turn (%s) stopped on a transient ACP stall (%s). Auto-retry %d/%d: inspect your current state and resume where you left off.",
		toolName, turnID, shortTitle(strings.TrimSpace(errMsg), 120), attempt, maxRetries,
	)
}

func stallRetryNote(attempt, maxRetries int, backoff time.Duration, turnID, errMsg string) string {
	return fmt.Sprintf(
		"auto: ACP stall retry %d/%d after %s on turn %s (%s)",
		attempt, maxRetries, backoff, turnID, shortTitle(strings.TrimSpace(errMsg), 120),
	)
}

func stallExhaustedNote(retries, maxRetries int, turnID, errMsg string) string {
	return fmt.Sprintf(
		"auto: ACP stall auto-retry exhausted (%d/%d) on turn %s (%s); escalated to inbox",
		retries, maxRetries, turnID, shortTitle(strings.TrimSpace(errMsg), 120),
	)
}

func stallDeliveryExhaustedNote(retries, maxRetries int, turnID, errMsg string, deliverErr error) string {
	return fmt.Sprintf(
		"auto: ACP stall auto-retry exhausted (%d/%d) on turn %s (%s); nudge failed: %s; escalated to inbox",
		retries, maxRetries, turnID, shortTitle(strings.TrimSpace(errMsg), 120), shortTitle(deliverErr.Error(), 120),
	)
}

func stallExhaustedEvent(th Thread, retries, maxRetries int, stopReason, errMsg string, now time.Time) Event {
	if stopReason == "" {
		stopReason = "error"
	}
	return Event{
		Bead:    th.Bead.ID,
		Kind:    EventFinished,
		Summary: fmt.Sprintf("%s stopped on %s%s after %d/%d auto-retries (%s)", th.Bead.ID, stopReason, errSuffix(errMsg), retries, maxRetries, stateLine(th)),
		At:      now,
		Clears:  eventCondition(th, EventFinished),
	}
}

func stallSkippedEvent(th Thread, stopReason, errMsg, reason string, now time.Time) Event {
	if stopReason == "" {
		stopReason = "error"
	}
	return Event{
		Bead:    th.Bead.ID,
		Kind:    EventFinished,
		Summary: fmt.Sprintf("%s stopped on %s%s (auto-retry skipped: %s) (%s)", th.Bead.ID, stopReason, errSuffix(errMsg), reason, stateLine(th)),
		At:      now,
		Clears:  eventCondition(th, EventFinished),
	}
}

func stallDeliveryExhaustedEvent(th Thread, retries, maxRetries int, errMsg string, deliverErr error, now time.Time) Event {
	return Event{
		Bead:    th.Bead.ID,
		Kind:    EventFinished,
		Summary: fmt.Sprintf("%s stopped on error%s; auto-retry %d/%d failed (%s) (%s)", th.Bead.ID, errSuffix(errMsg), retries, maxRetries, shortTitle(deliverErr.Error(), 80), stateLine(th)),
		At:      now,
		Clears:  eventCondition(th, EventFinished),
	}
}

// planStallRetry is the pure state machine for bounded transient ACP stall
// recovery on one thread and one tick.
func planStallRetry(cfg StallRetryConfig, th Thread, prev Snapshot, first bool, now time.Time, guard stallRetryGuard) stallRetryPlan {
	prevStall := stallStateFrom(prev)
	if first {
		return stallRetryPlan{Action: stallActionNone, Next: stallSnapshot{}}
	}

	// Any newer turn that did not end in a transient ACP stall (a healthy
	// non-error turn, or a different non-stall error) closes the stall episode
	// and resets the retry counter.
	if th.Turn != nil && th.Turn.ID != "" && th.Turn.ID != prevStall.TurnID && !isTransientACPStall(th.Turn) {
		if prevStall.hasState() {
			return stallRetryPlan{
				Action: stallActionReset,
				Next:   stallSnapshot{},
				Reason: "turn " + th.Turn.ID + " completed without ACP stall",
			}
		}
		return stallRetryPlan{Action: stallActionNone, Next: stallSnapshot{}}
	}

	// Disabled in config: clear any leftover state, surfacing a previously
	// suppressed stall once so disabling mid-backoff does not drop it.
	if !cfg.enabled() {
		if !prevStall.NextAt.IsZero() && !prevStall.Escalated &&
			th.Agent != nil && agentReady(th.Agent) &&
			th.Bead.Status != StatusNeedsMe && th.Bead.Status != StatusClosed && classify(th) != GroupNeedsYou {
			ev := stallSkippedEvent(th, "error", prevStall.Error, "auto-retry disabled", now)
			return stallRetryPlan{
				Action:        stallActionEscalate,
				EscalateEvent: &ev,
				JournalAction: "skipped",
				JournalText:   fmt.Sprintf("ACP stall auto-retry skipped on turn %s: auto-retry disabled", prevStall.TurnID),
				Reason:        "auto-retry disabled",
				Next:          stallSnapshot{Retries: prevStall.Retries, TurnID: prevStall.TurnID, Error: prevStall.Error, Escalated: true},
			}
		}
		return stallRetryPlan{Action: stallActionNone, Next: stallSnapshot{}}
	}

	// A new transient ACP stall turn finished on this tick.
	if turnFinishedOnTick(th, prev) && isTransientACPStall(th.Turn) && th.Turn.ID != "" && th.Turn.ID != prevStall.TurnID {
		if reason := stallRetryBlockReason(th, guard); reason != "" {
			// Do not suppress transitionDebounced's normal EventFinished;
			// cancel any stale pending backoff timer.
			return stallRetryPlan{
				Action: stallActionNone,
				Reason: reason,
				Next:   stallSnapshot{Retries: prevStall.Retries, TurnID: th.Turn.ID, Error: th.Turn.Error, Escalated: true},
			}
		}
		if prevStall.Retries >= cfg.MaxRetries {
			ev := stallExhaustedEvent(th, prevStall.Retries, cfg.MaxRetries, th.Turn.StopReason, th.Turn.Error, now)
			return stallRetryPlan{
				Action:           stallActionEscalate,
				SuppressFinished: true,
				MaxRetries:       cfg.MaxRetries,
				EscalateEvent:    &ev,
				Notify:           true,
				Note:             stallExhaustedNote(prevStall.Retries, cfg.MaxRetries, th.Turn.ID, th.Turn.Error),
				JournalAction:    "exhausted",
				JournalText:      fmt.Sprintf("ACP stall auto-retry exhausted (%d/%d) on turn %s: %s", prevStall.Retries, cfg.MaxRetries, th.Turn.ID, shortTitle(th.Turn.Error, 160)),
				Reason:           "retry cap exhausted",
				Next:             stallSnapshot{Retries: prevStall.Retries, TurnID: th.Turn.ID, Error: th.Turn.Error, Escalated: true},
			}
		}
		attempt := prevStall.Retries + 1
		delay := cfg.backoff(attempt)
		if delay <= 0 {
			return stallRetryPlan{
				Action:           stallActionRetry,
				SuppressFinished: true,
				Attempt:          attempt,
				MaxRetries:       cfg.MaxRetries,
				Backoff:          0,
				Prompt:           stallRetryPrompt(attempt, cfg.MaxRetries, th.Turn.ID, th.Turn.Error),
				Note:             stallRetryNote(attempt, cfg.MaxRetries, 0, th.Turn.ID, th.Turn.Error),
				JournalAction:    "retry",
				JournalText:      fmt.Sprintf("ACP stall retry %d/%d (backoff 0s) on turn %s: %s", attempt, cfg.MaxRetries, th.Turn.ID, shortTitle(th.Turn.Error, 160)),
				Next:             stallSnapshot{Retries: prevStall.Retries, TurnID: th.Turn.ID, NextAt: now, Error: th.Turn.Error},
			}
		}
		return stallRetryPlan{
			Action:           stallActionSchedule,
			SuppressFinished: true,
			Attempt:          attempt,
			MaxRetries:       cfg.MaxRetries,
			Backoff:          delay,
			Next:             stallSnapshot{Retries: prevStall.Retries, TurnID: th.Turn.ID, NextAt: now.Add(delay), Error: th.Turn.Error},
		}
	}

	// A retry was previously scheduled and is waiting for backoff or ready to fire.
	if !prevStall.NextAt.IsZero() && prevStall.TurnID != "" {
		// Worker woke up on its own (user input or another prompt): cancel the
		// pending timer, keeping the episode count until a healthy turn finishes.
		if th.Agent != nil && th.Agent.Status == "working" {
			next := prevStall
			next.NextAt = time.Time{}
			return stallRetryPlan{Action: stallActionNone, Reason: "agent resumed working", Next: next}
		}
		// Session file temporarily unreadable while agent is still idle: hold
		// steady instead of nudging blindly or falsely escalating.
		if th.Agent != nil && agentReady(th.Agent) && th.Turn == nil &&
			th.Bead.Status == StatusInProgress && !guard.Held && !guard.Headless && !guard.Relaunching && !guard.LiveAsk && th.Prompt == nil && classify(th) != GroupNeedsYou {
			return stallRetryPlan{Action: stallActionWait, Reason: "turn cursor temporarily unavailable", Next: prevStall}
		}
		if reason := stallRetryBlockReason(th, guard); reason != "" {
			next := prevStall
			next.NextAt = time.Time{}
			// Surface the previously suppressed stall once if the bead became
			// blocked/held while still idle; needs_me, prompts, closed, and
			// agent_gone already have their own events.
			if !prevStall.Escalated && th.Agent != nil && agentReady(th.Agent) &&
				th.Bead.Status != StatusNeedsMe && th.Bead.Status != StatusClosed && classify(th) != GroupNeedsYou {
				next.Escalated = true
				ev := stallSkippedEvent(th, "error", prevStall.Error, reason, now)
				return stallRetryPlan{
					Action:        stallActionEscalate,
					EscalateEvent: &ev,
					JournalAction: "skipped",
					JournalText:   fmt.Sprintf("ACP stall auto-retry skipped on turn %s: %s", prevStall.TurnID, reason),
					Reason:        reason,
					Next:          next,
				}
			}
			return stallRetryPlan{Action: stallActionNone, Reason: reason, Next: next}
		}
		// Confirm the session's last turn is still the tracked stalled turn.
		if th.Turn.ID != prevStall.TurnID {
			return stallRetryPlan{Action: stallActionReset, Next: stallSnapshot{}, Reason: "turn advanced"}
		}
		if now.Before(prevStall.NextAt) {
			return stallRetryPlan{
				Action:     stallActionWait,
				Attempt:    prevStall.Retries + 1,
				MaxRetries: cfg.MaxRetries,
				Next:       prevStall,
			}
		}
		// Backoff elapsed: re-check cap in case config lowered max_retries.
		if prevStall.Retries >= cfg.MaxRetries {
			ev := stallExhaustedEvent(th, prevStall.Retries, cfg.MaxRetries, th.Turn.StopReason, prevStall.Error, now)
			return stallRetryPlan{
				Action:        stallActionEscalate,
				MaxRetries:    cfg.MaxRetries,
				EscalateEvent: &ev,
				Notify:        true,
				Note:          stallExhaustedNote(prevStall.Retries, cfg.MaxRetries, prevStall.TurnID, prevStall.Error),
				JournalAction: "exhausted",
				JournalText:   fmt.Sprintf("ACP stall auto-retry exhausted (%d/%d) on turn %s: %s", prevStall.Retries, cfg.MaxRetries, prevStall.TurnID, shortTitle(prevStall.Error, 160)),
				Reason:        "retry cap exhausted",
				Next:          stallSnapshot{Retries: prevStall.Retries, TurnID: prevStall.TurnID, Error: prevStall.Error, Escalated: true},
			}
		}
		attempt := prevStall.Retries + 1
		delay := cfg.backoff(attempt)
		return stallRetryPlan{
			Action:        stallActionRetry,
			Attempt:       attempt,
			MaxRetries:    cfg.MaxRetries,
			Backoff:       delay,
			Prompt:        stallRetryPrompt(attempt, cfg.MaxRetries, prevStall.TurnID, prevStall.Error),
			Note:          stallRetryNote(attempt, cfg.MaxRetries, delay, prevStall.TurnID, prevStall.Error),
			JournalAction: "retry",
			JournalText:   fmt.Sprintf("ACP stall retry %d/%d (backoff %s) on turn %s: %s", attempt, cfg.MaxRetries, delay, prevStall.TurnID, shortTitle(prevStall.Error, 160)),
			Next:          prevStall,
		}
	}

	return stallRetryPlan{Action: stallActionNone, Next: prevStall}
}

func (t Ticker) stallGuardFor(th Thread, prev Snapshot, relaunching bool) stallRetryGuard {
	_, held := isJobHeld(th.Bead.ID)
	g := stallRetryGuard{
		Held:        held,
		Headless:    headlessOwned(th.Bead.ID),
		Relaunching: relaunching,
	}
	// Only probe the pi control socket for a live ask-gate when a stall or
	// pending retry is actually active on this thread, avoiding extra dials on
	// every routine tick.
	if th.Agent != nil && th.Prompt == nil && (isTransientACPStall(th.Turn) || !prev.StallNextAt.IsZero()) {
		if ask, ok, err := piAskOf(t.herdr.hostName(), th.Agent); ok && err == nil && ask != nil {
			g.LiveAsk = true
		}
	}
	return g
}

func (t Ticker) applyStallRetry(th Thread, snap *Snapshot, plan stallRetryPlan, pendingDelivered bool, now time.Time) bool {
	id := th.Bead.ID
	switch plan.Action {
	case stallActionSchedule:
		applyStallSnapshot(snap, plan.Next)
		t.log.Printf("stall retry %s: scheduled attempt %d/%d in %s on turn %s (%s)",
			id, plan.Attempt, plan.MaxRetries, plan.Backoff, plan.Next.TurnID, shortTitle(plan.Next.Error, 100))
	case stallActionWait:
		applyStallSnapshot(snap, plan.Next)
	case stallActionReset:
		applyStallSnapshot(snap, plan.Next)
		t.log.Printf("stall retry %s: reset counter (%s)", id, plan.Reason)
	case stallActionRetry:
		if pendingDelivered {
			next := plan.Next
			next.NextAt = time.Time{}
			applyStallSnapshot(snap, next)
			t.log.Printf("stall retry %s: skipped attempt %d/%d (worker already prompted this tick)", id, plan.Attempt, plan.MaxRetries)
			return false
		}
		if _, err := promptPiAgent(t.herdr, *th.Agent, plan.Prompt); err != nil {
			t.log.Printf("stall retry %s: nudge failed (attempt %d/%d): %v", id, plan.Attempt, plan.MaxRetries, err)
			failText := fmt.Sprintf("ACP stall retry %d/%d nudge failed on turn %s: %v", plan.Attempt, plan.MaxRetries, plan.Next.TurnID, err)
			if jerr := appendJournal(journalRecord{At: now, Kind: journalStallRetry, Bead: id, Class: -1, Action: "retry_failed", Pass: false, Text: failText}); jerr != nil {
				t.log.Printf("stall retry journal %s: %v", id, jerr)
			}
			next := plan.Next
			next.Retries = plan.Attempt
			if next.Retries >= t.cfg.StallRetry.MaxRetries {
				next.NextAt = time.Time{}
				next.Escalated = true
				applyStallSnapshot(snap, next)
				ev := stallDeliveryExhaustedEvent(th, next.Retries, t.cfg.StallRetry.MaxRetries, next.Error, err, now)
				if werr := writeEvent(ev); werr != nil {
					t.log.Printf("inbox: %v", werr)
				}
				if nerr := t.beads.Note(id, stallDeliveryExhaustedNote(next.Retries, t.cfg.StallRetry.MaxRetries, next.TurnID, next.Error, err)); nerr != nil {
					t.log.Printf("stall retry note %s: %v", id, nerr)
				}
				if desktopReason(th) == "" && classify(th) != GroupNeedsYou {
					if derr := t.herdr.Notify(fmt.Sprintf("%s: %s", id, classify(th)), shortTitle(th.Bead.Title, 60)); derr != nil {
						t.log.Printf("desktop notification submission failed: %v", derr)
					}
				}
				return false
			}
			next.NextAt = now.Add(t.cfg.StallRetry.backoff(next.Retries + 1))
			applyStallSnapshot(snap, next)
			return false
		}
		next := plan.Next
		next.Retries = plan.Attempt
		next.NextAt = time.Time{}
		next.Escalated = false
		applyStallSnapshot(snap, next)
		snap.ReadySince = time.Time{}
		t.log.Printf("stall retry %s: nudged worker (attempt %d/%d, turn %s)", id, plan.Attempt, plan.MaxRetries, next.TurnID)
		if jerr := appendJournal(journalRecord{At: now, Kind: journalStallRetry, Bead: id, Class: -1, Action: plan.JournalAction, Pass: true, Text: plan.JournalText}); jerr != nil {
			t.log.Printf("stall retry journal %s: %v", id, jerr)
		}
		if plan.Note != "" {
			if nerr := t.beads.Note(id, plan.Note); nerr != nil {
				t.log.Printf("stall retry note %s: %v", id, nerr)
			}
		}
		return true
	case stallActionEscalate:
		applyStallSnapshot(snap, plan.Next)
		t.log.Printf("stall retry %s: escalated (%s)", id, plan.Reason)
		if plan.EscalateEvent != nil {
			if werr := writeEvent(*plan.EscalateEvent); werr != nil {
				t.log.Printf("inbox: %v", werr)
			}
		}
		if plan.JournalAction != "" {
			if jerr := appendJournal(journalRecord{At: now, Kind: journalStallRetry, Bead: id, Class: -1, Action: plan.JournalAction, Pass: false, Text: plan.JournalText}); jerr != nil {
				t.log.Printf("stall retry journal %s: %v", id, jerr)
			}
		}
		if plan.Note != "" {
			if nerr := t.beads.Note(id, plan.Note); nerr != nil {
				t.log.Printf("stall retry note %s: %v", id, nerr)
			}
		}
		if plan.Notify && desktopReason(th) == "" && classify(th) != GroupNeedsYou {
			if derr := t.herdr.Notify(fmt.Sprintf("%s: %s", id, classify(th)), shortTitle(th.Bead.Title, 60)); derr != nil {
				t.log.Printf("desktop notification submission failed: %v", derr)
			}
		}
	default:
		applyStallSnapshot(snap, plan.Next)
	}
	return false
}
