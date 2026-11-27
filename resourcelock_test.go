package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBeadResourceLocksParsingAndValidation(t *testing.T) {
	t.Run("no lock labels", func(t *testing.T) {
		got, err := beadResourceLocks(Bead{ID: "task-1", Labels: []string{"next", "role:rollout"}})
		if err != nil || len(got) != 0 {
			t.Fatalf("got=%v err=%v, want empty nil", got, err)
		}
	})

	t.Run("multiple valid sorted and case-insensitively deduped", func(t *testing.T) {
		b := Bead{
			ID:     "task-2",
			Labels: []string{"lock:staging-cluster", "lock:ci-workflow-toggle", "lock:Staging-Cluster", "lock:db/primary:v1"},
		}
		got, err := beadResourceLocks(b)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		want := []string{"ci-workflow-toggle", "db/primary:v1", "staging-cluster"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	badCases := []struct {
		name  string
		label string
		want  string
	}{
		{"empty lock name", "lock:", "empty lock:"},
		{"whitespace", "lock:staging cluster", "invalid resource lock name"},
		{"leading dash", "lock:-staging", "invalid resource lock name"},
		{"leading dot", "lock:.staging", "invalid resource lock name"},
		{"leading slash", "lock:/staging", "invalid resource lock name"},
		{"shell metachar", "lock:staging;rm", "invalid resource lock name"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := beadResourceLocks(Bead{ID: "task-bad", Labels: []string{tc.label}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("label %q: err=%v, want substring %q", tc.label, err, tc.want)
			}
		})
	}
}

func TestFindResourceLockConflicts(t *testing.T) {
	target := Bead{
		ID:     "task-new",
		Title:  "Deploy staging migration",
		Status: StatusOpen,
		Labels: []string{"lock:staging-cluster", "lock:ci-workflow-toggle"},
	}
	candidates := []Bead{
		// Same bead never conflicts with itself.
		{ID: "task-new", Status: StatusInProgress, Labels: []string{"lock:staging-cluster"}},
		// Non-in_progress statuses never hold the mutex.
		{ID: "task-open", Status: StatusOpen, Labels: []string{"lock:staging-cluster"}},
		{ID: "task-closed", Status: StatusClosed, Labels: []string{"lock:staging-cluster"}},
		{ID: "task-blocked", Status: StatusBlocked, Labels: []string{"lock:staging-cluster"}},
		{ID: "task-needs-me", Status: StatusNeedsMe, Labels: []string{"lock:staging-cluster"}},
		// Disjoint lock on in_progress bead does not conflict.
		{ID: "task-other", Status: StatusInProgress, Labels: []string{"lock:sandbox-db"}},
		// Malformed lock label on an unrelated peer bead must not break detection.
		{ID: "task-typo", Status: StatusInProgress, Labels: []string{"lock:", "lock:bad name"}},
		// Conflicting in_progress holders (including case-insensitive match).
		{ID: "holder-b", Title: "Toggle CI workflow", Status: StatusInProgress, Labels: []string{"lock:CI-Workflow-Toggle"}},
		{ID: "holder-a", Title: "Upgrade staging cluster", Status: StatusInProgress, Labels: []string{"lock:staging-cluster"}},
	}

	conflicts, err := findResourceLockConflicts(target, candidates)
	if err != nil {
		t.Fatalf("findResourceLockConflicts: %v", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("got %d conflicts (%+v), want 2", len(conflicts), conflicts)
	}
	if conflicts[0].Lock != "ci-workflow-toggle" || conflicts[0].HolderBead != "holder-b" {
		t.Errorf("conflicts[0] = %+v, want ci-workflow-toggle / holder-b", conflicts[0])
	}
	if conflicts[1].Lock != "staging-cluster" || conflicts[1].HolderBead != "holder-a" {
		t.Errorf("conflicts[1] = %+v, want staging-cluster / holder-a", conflicts[1])
	}

	err = resourceLockConflictError(target.ID, conflicts)
	if !errors.Is(err, errResourceLocked) {
		t.Fatalf("error does not wrap errResourceLocked: %v", err)
	}
	for _, want := range []string{"task-new", "lock:ci-workflow-toggle", "holder-b", "lock:staging-cluster", "holder-a", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("conflict error missing %q: %v", want, err)
		}
	}
}

func TestCheckDispatchResourceLocksFailClosedAndNoExtraQuery(t *testing.T) {
	dir := t.TempDir()
	bdScript := `#!/bin/sh
echo "$*" >> "$LOG/bd.log"
if [ "$1" = "list" ]; then
  echo "database unavailable" >&2
  exit 1
fi
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Bead with no lock:* labels skips bd list completely.
	if err := checkDispatchResourceLocks(Beads{}, Bead{ID: "task-plain", Status: StatusOpen}, false); err != nil {
		t.Fatalf("plain bead should not fail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bd.log")); !os.IsNotExist(err) {
		t.Fatalf("plain bead unexpectedly invoked bd list")
	}

	// Malformed lock label fails closed even with force=true.
	if err := checkDispatchResourceLocks(Beads{}, Bead{ID: "task-bad", Labels: []string{"lock:"}}, true); err == nil || !strings.Contains(err.Error(), "empty lock:") {
		t.Fatalf("malformed lock with force should fail closed, got %v", err)
	}

	// Bead with valid lock label fails closed when bd list fails.
	err := checkDispatchResourceLocks(Beads{}, Bead{ID: "task-locked", Labels: []string{"lock:staging-cluster"}}, false)
	if !errors.Is(err, errResourceLocked) || !strings.Contains(err.Error(), "cannot verify shared resource locks") {
		t.Fatalf("expected fail-closed lock error on bd list failure, got %v", err)
	}

	// force=true bypasses lock check even when bd list is unavailable.
	if err := checkDispatchResourceLocks(Beads{}, Bead{ID: "task-locked", Labels: []string{"lock:staging-cluster"}}, true); err != nil {
		t.Fatalf("force=true should override lock check, got %v", err)
	}
}
