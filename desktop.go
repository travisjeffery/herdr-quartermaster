package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Operational delivery receipts only. Beads/worker state remains task truth;
// neither desktop submission nor reading this file answers a question.
const desktopMaxAttempts = 3

const desktopTitle = "Quartermaster: worker attention required"
const desktopBody = "Workers are blocked or need a decision. Run quartermaster context and quartermaster ticker notifications. Desktop submission is not human readback."

type desktopReceipt struct {
	Reason     string    `json:"reason"`
	PromptHash string    `json:"prompt_hash,omitempty"`
	Detected   time.Time `json:"detected"`
	Active     bool      `json:"active"`
	Ended      time.Time `json:"ended,omitempty"`
	State      string    `json:"state"` // detected, attempting, submitted, failed, unknown
	Attempts   int       `json:"attempts"`
	LastTry    time.Time `json:"last_attempt,omitempty"`
	NextTry    time.Time `json:"next_attempt,omitempty"`
	Result     string    `json:"result,omitempty"` // fixed codes, never subprocess output
}

type desktopJournal struct {
	Version int                       `json:"version"`
	LastTry time.Time                 `json:"last_attempt,omitempty"`
	Items   map[string]desktopReceipt `json:"items"`
}

type desktopResult struct{ State, Reason string }

func desktopDir() string  { return filepath.Join(stateDir(), "desktop") }
func desktopPath() string { return filepath.Join(desktopDir(), "notifications.json") }

func loadDesktop() (desktopJournal, error) {
	var j desktopJournal
	err := rpcLoad(desktopPath(), &j)
	if errors.Is(err, os.ErrNotExist) {
		return desktopJournal{Version: 1, Items: map[string]desktopReceipt{}}, nil
	}
	if err != nil {
		return j, err
	}
	if j.Version != 1 || j.Items == nil {
		return j, fmt.Errorf("invalid desktop notification journal; inspect %s", desktopPath())
	}
	for _, r := range j.Items {
		switch r.State {
		case "detected", "attempting", "submitted", "failed", "unknown":
		default:
			return j, fmt.Errorf("invalid desktop notification state")
		}
		if r.Attempts < 0 || r.Attempts > desktopMaxAttempts || r.Detected.IsZero() {
			return j, fmt.Errorf("invalid desktop notification receipt")
		}
	}
	return j, nil
}

func desktopReason(th Thread) string {
	switch {
	case th.Bead.Status == StatusNeedsMe:
		return "needs_me"
	case th.Bead.Status == StatusBlocked:
		return "blocked"
	case th.Agent != nil && th.Agent.Status == "blocked":
		// Migration parking is not a new approval question. A real task
		// blocker above still counts; never unpark or probe the worker.
		if th.Agent.Backend == rpcBackend && rpcParked(th.Bead.ID) {
			return ""
		}
		return "worker_blocked"
	default:
		return ""
	}
}

// submitDesktop uses the same local notify-send convention as the digest,
// without Herdr or any external delivery target. Never include task text.
// Failure to START is safe to retry. Once started, any unsuccessful result
// (including timeout) is ambiguous: it may have displayed before failing.
func submitDesktop() desktopResult {
	return desktopCommand(3 * time.Second)
}

func desktopCommand(timeout time.Duration) desktopResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "notify-send", "--app-name", toolName, "--", desktopTitle, desktopBody)
	if err := cmd.Start(); err != nil {
		return desktopResult{"failed", "notifier-unavailable"}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return desktopResult{"unknown", "notifier-timeout"}
		}
		return desktopResult{"unknown", "notifier-nonzero-exit"}
	}
	return desktopResult{"submitted", "notifier-exited-zero"}
}

// reconcileDesktop runs independently of transition suppression and state.json.
// Save detection and intent before the side effect; a lost result becomes
// unknown, never an unbounded retry. All due episodes coalesce into ONE fixed
// notification, at most once a minute, including initial existing blockers.
// We inspect current threads, never replay the historical inbox backlog.
func reconcileDesktop(threads []Thread, now time.Time, submit func() desktopResult) error {
	if err := rpcPrivateDir(desktopDir()); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(desktopDir(), "lease"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("desktop notifications already reconciling: %w", err)
	}
	j, err := loadDesktop()
	if err != nil {
		return err
	}
	active := map[string]bool{}
	for _, th := range threads {
		reason := desktopReason(th)
		if reason == "" {
			// A missing worker observation is not proof its question was
			// answered. Keep the episode through a transient status failure.
			if r, ok := j.Items[th.Bead.ID]; ok && r.Active && r.Reason == "worker_blocked" && th.Agent == nil && th.Bead.Status == StatusInProgress {
				active[th.Bead.ID] = true
			}
			continue
		}
		id, hash := th.Bead.ID, ""
		if th.Prompt != nil {
			hash = th.Prompt.Hash
		}
		r, exists := j.Items[id]
		// A temporarily unreadable prompt must not turn the same blocker
		// into a new episode, nor reset its retry budget.
		if !exists || !r.Active || r.Reason != reason || (hash != "" && r.PromptHash != "" && hash != r.PromptHash) {
			r = desktopReceipt{Reason: reason, PromptHash: hash, Detected: now, Active: true, State: "detected"}
		} else if hash != "" {
			r.PromptHash = hash
		}
		j.Items[id], active[id] = r, true
	}
	for id, r := range j.Items {
		if r.State == "attempting" {
			r.State, r.Result = "unknown", "interrupted-attempt"
			r.NextTry = time.Time{}
		}
		if r.Active && !active[id] {
			r.Active, r.Ended = false, now
			r.NextTry = time.Time{}
		}
		j.Items[id] = r
	}
	// Retain all current blockers plus the latest 256 inactive receipts.
	var inactive []string
	for id, r := range j.Items {
		if !r.Active {
			inactive = append(inactive, id)
		}
	}
	sort.Slice(inactive, func(i, k int) bool { return j.Items[inactive[i]].Ended.After(j.Items[inactive[k]].Ended) })
	for i := 256; i < len(inactive); i++ {
		delete(j.Items, inactive[i])
	}
	if err := rpcSave(desktopPath(), j); err != nil {
		return err // no submission without durable detection
	}
	if !j.LastTry.IsZero() && now.Sub(j.LastTry) < time.Minute {
		return nil
	}
	var due []string
	for id, r := range j.Items {
		if r.Active && (r.State == "detected" || r.State == "failed") && r.Attempts < desktopMaxAttempts && !now.Before(r.NextTry) {
			due = append(due, id)
		}
	}
	if len(due) == 0 {
		return nil
	}
	j.LastTry = now
	for _, id := range due {
		r := j.Items[id]
		r.State, r.LastTry, r.NextTry = "attempting", now, time.Time{}
		r.Attempts++
		j.Items[id] = r
	}
	if err := rpcSave(desktopPath(), j); err != nil {
		return err // no submission without durable intent
	}
	result := submit()
	if result.State != "submitted" && result.State != "failed" && result.State != "unknown" {
		result = desktopResult{"unknown", "invalid-adapter-result"}
	}
	for _, id := range due {
		r := j.Items[id]
		r.State, r.Result = result.State, result.Reason
		if r.State == "failed" && r.Attempts < desktopMaxAttempts {
			delay := time.Minute
			if r.Attempts > 1 {
				delay = 5 * time.Minute
			}
			r.NextTry = now.Add(delay)
		}
		j.Items[id] = r
	}
	if err := rpcSave(desktopPath(), j); err != nil {
		return err // durable intent will report unknown on recovery
	}
	if result.State != "submitted" {
		return fmt.Errorf("desktop notification %s (%s); inspect `quartermaster ticker notifications`", result.State, result.Reason)
	}
	return nil
}

func desktopNotifications(out io.Writer) error {
	j, err := loadDesktop()
	if err != nil {
		return err
	}
	// A read-only status must not rewrite an in-flight intent, but must not
	// present it as known submission either.
	return json.NewEncoder(out).Encode(struct {
		HumanReadback string         `json:"human_readback"`
		Journal       desktopJournal `json:"delivery"`
	}{"unknown; submission/attempting is not readback or task completion", j})
}
