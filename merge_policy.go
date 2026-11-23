package main

import (
	"slices"
	"strings"
)

// MergePolicy records task permission separately from GitHub requirements and
// review evidence. There is deliberately no no-review-required waiver: the
// shared current-head review floor cannot be lowered by a task or repository.
// This is advisory task scoping, not a security boundary against forged notes.
type MergePolicy struct {
	Allowed bool
	Reason  string
}

func mergeTaskPolicy(cfg Config, b Bead, pr PR, authorized bool) MergePolicy {
	if b.ID == "" || b.Status != StatusInProgress {
		return MergePolicy{Reason: "task is not in progress"}
	}
	if !onBeadBranch(cfg.BranchPrefix, pr.Head, b) && !slices.Contains(NotedPRs(b.Notes), pr.URL) {
		return MergePolicy{Reason: "PR is not associated with this task"}
	}
	if !authorized {
		return MergePolicy{Reason: "task merge authorization unavailable"}
	}
	return MergePolicy{Allowed: true}
}

// mergePlanPolicy additionally recognizes a live, explicit approval of this
// exact plan as an answer to an older needs_me hold. It cannot waive blocked
// or closed state, a newer hold, ownership, or any GitHub/review prerequisite.
// The executor alone supplies approval after reading its durable journal.
func mergePlanPolicy(cfg Config, b Bead, pr PR, approval planEvent) MergePolicy {
	if b.Status == StatusNeedsMe && approval.By == "TJ" && !approval.At.Before(b.UpdatedAt) {
		b.Status = StatusInProgress
	}
	return mergeTaskPolicy(cfg, b, pr, true)
}

// tickerMergePolicy is a read-only advisory check. The executor independently
// rechecks the plan's digest approval, level and budget immediately before an
// action. GitHub mergeability never substitutes for that authorization.
func tickerMergePolicy(cfg Config, b Bead, pr PR) MergePolicy {
	class, err := cfg.actionClass(b, "merge", "", "")
	if m := prURL.FindStringSubmatch(pr.URL); m != nil {
		_, name, _ := strings.Cut(m[1], "/")
		class = max(class, cfg.Autonomy.MergeClass[name])
	}
	level, _ := cfg.beadLevel(b, autonomyStopped())
	return mergeTaskPolicy(cfg, b, pr, err == nil && level >= class)
}
