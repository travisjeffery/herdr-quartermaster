package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Shared-resource mutex labels (lock:<name>) prevent concurrent beads from
// racing on a shared live mutable resource (for example a staging cluster or
// a CI workflow toggle).
//
// Any non-closed in_progress bead carrying lock:<name> holds that mutex until
// it closes, leaves in_progress, or removes the label. Both manual dispatch
// (`quartermaster dispatch`, unless overridden with `--force`) and automatic
// ticker dispatch/wakeup (`blockerWakeupPass`) fail closed / defer cleanly
// while another in_progress bead holds a conflicting lock.

const resourceLockLabel = "lock:"

var (
	resourceLockNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	errResourceLocked  = errors.New("resource locked")
)

// ResourceLockConflict identifies one shared resource lock held by another
// active in_progress bead.
type ResourceLockConflict struct {
	Lock        string `json:"lock"`
	HolderBead  string `json:"holder_bead"`
	HolderTitle string `json:"holder_title,omitempty"`
}

// validateResourceLockName checks that name is a safe, non-empty resource lock
// token without whitespace.
func validateResourceLockName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || !resourceLockNameRE.MatchString(name) {
		return fmt.Errorf("invalid resource lock name %q (want letters, digits, '.', '_', ':', '/', or '-' with no whitespace)", name)
	}
	return nil
}

// beadResourceLocks extracts and validates all lock:<name> labels on b.
// Returns sorted, case-insensitively deduplicated lock names. Fails closed on
// an empty or malformed lock:* label.
func beadResourceLocks(b Bead) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, l := range b.Labels {
		name, ok := strings.CutPrefix(l, resourceLockLabel)
		if !ok {
			continue
		}
		if name == "" {
			return nil, fmt.Errorf("%s has an empty %s label", b.ID, resourceLockLabel)
		}
		if err := validateResourceLockName(name); err != nil {
			return nil, fmt.Errorf("%s: %w", b.ID, err)
		}
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return out, nil
}

// validBeadResourceLocks returns the lowercase set of valid lock:<name> tokens
// on a peer bead, ignoring malformed labels on unrelated beads so a typo
// elsewhere never blocks unrelated dispatches.
func validBeadResourceLocks(b Bead) map[string]string {
	var out map[string]string
	for _, l := range b.Labels {
		name, ok := strings.CutPrefix(l, resourceLockLabel)
		if !ok || validateResourceLockName(name) != nil {
			continue
		}
		key := strings.ToLower(name)
		if out == nil {
			out = map[string]string{}
		}
		if _, exists := out[key]; !exists {
			out[key] = name
		}
	}
	return out
}

// findResourceLockConflicts checks whether any other non-closed in_progress
// bead in candidates holds one or more lock:<name> labels carried by b.
// Comparison is case-insensitive on the resource name and never conflicts with
// b itself. Results are sorted deterministically by (lock, holder bead ID).
func findResourceLockConflicts(b Bead, candidates []Bead) ([]ResourceLockConflict, error) {
	want, err := beadResourceLocks(b)
	if err != nil {
		return nil, err
	}
	if len(want) == 0 {
		return nil, nil
	}
	wantByKey := make(map[string]string, len(want))
	for _, name := range want {
		wantByKey[strings.ToLower(name)] = name
	}
	var conflicts []ResourceLockConflict
	seen := map[string]bool{}
	for _, other := range candidates {
		if other.ID == "" || other.ID == b.ID || other.Status != StatusInProgress {
			continue
		}
		held := validBeadResourceLocks(other)
		for key := range held {
			lockName, needed := wantByKey[key]
			if !needed {
				continue
			}
			dedupe := key + "\x00" + other.ID
			if seen[dedupe] {
				continue
			}
			seen[dedupe] = true
			conflicts = append(conflicts, ResourceLockConflict{
				Lock:        lockName,
				HolderBead:  other.ID,
				HolderTitle: other.Title,
			})
		}
	}
	slices.SortFunc(conflicts, func(a, b ResourceLockConflict) int {
		if c := strings.Compare(strings.ToLower(a.Lock), strings.ToLower(b.Lock)); c != 0 {
			return c
		}
		return strings.Compare(a.HolderBead, b.HolderBead)
	})
	return conflicts, nil
}

func formatResourceLockLabels(locks []string) string {
	parts := make([]string, len(locks))
	for i, l := range locks {
		parts[i] = resourceLockLabel + l
	}
	return strings.Join(parts, ", ")
}

func formatResourceLockConflicts(conflicts []ResourceLockConflict) string {
	parts := make([]string, len(conflicts))
	for i, c := range conflicts {
		if c.HolderTitle != "" {
			parts[i] = fmt.Sprintf("%s%s held by %s (%s)", resourceLockLabel, c.Lock, c.HolderBead, shortTitle(c.HolderTitle, 50))
		} else {
			parts[i] = fmt.Sprintf("%s%s held by %s", resourceLockLabel, c.Lock, c.HolderBead)
		}
	}
	return strings.Join(parts, "; ")
}

func resourceLockConflictError(beadID string, conflicts []ResourceLockConflict) error {
	return fmt.Errorf("%w: cannot dispatch %s: %s (pass --force to override)",
		errResourceLocked, beadID, formatResourceLockConflicts(conflicts))
}

// checkDispatchResourceLocks enforces lock:<name> mutual exclusion before a
// dispatch proceeds. Malformed lock:* labels on b fail closed even with force.
// When b carries no lock:* labels, no extra bd query is issued.
func checkDispatchResourceLocks(bd Beads, b Bead, force bool) error {
	locks, err := beadResourceLocks(b)
	if err != nil {
		return err
	}
	if len(locks) == 0 || force {
		return nil
	}
	inProgress, err := bd.list("--status", StatusInProgress)
	if err != nil {
		return fmt.Errorf("%w: cannot verify shared resource locks (%s) for %s: %v (pass --force to override)",
			errResourceLocked, formatResourceLockLabels(locks), b.ID, err)
	}
	conflicts, err := findResourceLockConflicts(b, inProgress)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return resourceLockConflictError(b.ID, conflicts)
	}
	return nil
}
