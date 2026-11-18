package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDesktopMissingWorkerObservationDoesNotResetEpisode(t *testing.T) {
	for _, outcome := range []string{"submitted", "failed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
			th := desktopThreads()[2]
			calls := 0
			submit := func() desktopResult { calls++; return desktopResult{outcome, "fixture-result"} }
			now := time.Now()
			for i := 0; i < 30; i++ {
				observation := th
				if i%2 == 1 {
					observation.Agent = nil
				} // transient RPC/discovery failure
				_ = reconcileDesktop([]Thread{observation}, now.Add(time.Duration(i)*time.Minute), submit)
			}
			want := 1
			if outcome == "failed" {
				want = desktopMaxAttempts
			}
			j, err := loadDesktop()
			if err != nil {
				t.Fatal(err)
			}
			r := j.Items[th.Bead.ID]
			if calls != want || r.Attempts != want || !r.Active || !r.Detected.Equal(now) || r.State != outcome {
				t.Fatalf("missing status reset episode: calls=%d receipt=%+v", calls, r)
			}
		})
	}
}

func TestDesktopDurableIntentBeforeSubmissionAndFailedSave(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now()
	calls := 0
	submit := func() desktopResult {
		calls++
		j, err := loadDesktop()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range j.Items {
			if r.State != "attempting" || r.Attempts != 1 {
				t.Fatalf("no durable intent: %+v", r)
			}
		}
		return desktopResult{"submitted", "notifier-exited-zero"}
	}
	if err := reconcileDesktop(desktopThreads(), now, submit); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("batch not coalesced")
	}
	// An unwritable journal cannot submit a new episode. Use a directory at
	// the destination rather than chmod (tests may run as root).
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	if err := os.MkdirAll(desktopPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := reconcileDesktop(desktopThreads(), now, submit); err == nil {
		t.Fatal("ignored persistence failure")
	}
	if calls != 1 {
		t.Fatal("submitted before journal commit")
	}
}

func TestDesktopConcurrentReconcileDoesNotSubmitTwice(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now()
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- reconcileDesktop(desktopThreads(), now, func() desktopResult {
			close(started)
			<-finish
			return desktopResult{"submitted", "notifier-exited-zero"}
		})
	}()
	<-started
	err := reconcileDesktop(desktopThreads(), now, func() desktopResult { t.Error("concurrent submit"); return desktopResult{} })
	close(finish)
	if err == nil {
		t.Fatal("concurrent reconciler not refused")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDesktopRetentionAndNoTaskWrites(t *testing.T) {
	dir := fakeTools(t)
	now := time.Now()
	if err := rpcPrivateDir(desktopDir()); err != nil {
		t.Fatal(err)
	}
	j := desktopJournal{Version: 1, Items: map[string]desktopReceipt{}}
	for i := 0; i < 300; i++ {
		id := time.Unix(int64(i), 0).Format(time.RFC3339)
		j.Items[id] = desktopReceipt{Detected: now.Add(-time.Hour), Ended: now.Add(-time.Duration(i) * time.Second), State: "submitted"}
	}
	if err := rpcSave(desktopPath(), j); err != nil {
		t.Fatal(err)
	}
	if err := reconcileDesktop(desktopThreads(), now, submitDesktop); err != nil {
		t.Fatal(err)
	}
	j, _ = loadDesktop()
	if len(j.Items) != 259 {
		t.Fatalf("retention is not bounded: %d", len(j.Items))
	}
	for _, tool := range []string{"bd", "gh", "herdr"} {
		if _, err := os.Stat(filepath.Join(dir, tool+".log")); !os.IsNotExist(err) {
			t.Fatalf("notification invoked %s", tool)
		}
	}
	if lines := logLines(t, dir, "notify-send"); len(lines) != 1 {
		t.Fatalf("not one coalesced local alert: %v", lines)
	}
}
