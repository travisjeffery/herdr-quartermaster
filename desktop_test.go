package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func desktopThreads() []Thread {
	return []Thread{
		{Bead: Bead{ID: "task-one", Status: StatusNeedsMe, Title: "private title", Notes: "private question"}},
		{Bead: Bead{ID: "task-two", Status: StatusBlocked}},
		{Bead: Bead{ID: "task-three", Status: StatusInProgress}, Agent: &Agent{Backend: rpcBackend, Status: "blocked"}},
	}
}

func TestDesktopDetectionRetryRestartAndDedup(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now()
	threads := desktopThreads()
	before, _ := json.Marshal(threads)
	calls := 0
	submit := func() desktopResult {
		calls++
		if calls == 1 {
			return desktopResult{"failed", "notifier-unavailable"}
		}
		return desktopResult{"submitted", "notifier-exited-zero"}
	}
	if err := reconcileDesktop(threads, now, submit); err == nil {
		t.Fatal("failed notifier hidden")
	}
	j, err := loadDesktop()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range j.Items {
		if r.State != "failed" || r.Attempts != 1 || !r.NextTry.Equal(now.Add(time.Minute)) {
			t.Fatalf("bad failure receipt: %+v", r)
		}
	}
	// Every call reloads disk, like a fresh ticker after restart. Detection
	// suppression and unrelated state.json failure cannot lose the retry.
	for i := 1; i < 60; i++ {
		if err := reconcileDesktop(threads, now.Add(time.Duration(i)*time.Second), submit); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("no backoff: %d", calls)
	}
	if err := reconcileDesktop(threads, now.Add(time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("retry lost: %d", calls)
	}
	for i := 2; i < 15; i++ {
		if err := reconcileDesktop(threads, now.Add(time.Duration(i)*time.Hour), submit); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("submitted alert replayed")
	}
	j, _ = loadDesktop()
	for _, r := range j.Items {
		if r.State != "submitted" || r.Attempts != 2 {
			t.Fatalf("bad submission receipt: %+v", r)
		}
	}
	after, _ := json.Marshal(threads)
	if !bytes.Equal(before, after) {
		t.Fatal("notification answered/changed task")
	}
	var out bytes.Buffer
	if err := desktopNotifications(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not readback or task completion") {
		t.Fatal("claims human readback")
	}
	readback, _ := loadDesktop()
	if !reflect.DeepEqual(j, readback) {
		t.Fatal("reading report changed delivery/task state")
	}
	if fi, _ := os.Stat(desktopPath()); fi.Mode().Perm() != 0600 {
		t.Fatal("journal not private")
	}
}

func TestDesktopBoundedFailureAndUnknown(t *testing.T) {
	for _, mode := range []string{"failed", "unknown", "crash-intent", "lost-result"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
			now := time.Now()
			calls := 0
			submit := func() desktopResult {
				calls++
				if mode == "lost-result" {
					// Simulate process loss by observing the durable intent and
					// returning a result, then restoring just that crash image.
					j, _ := loadDesktop()
					if err := rpcSave(filepath.Join(desktopDir(), "crash.json"), j); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "unknown" {
					return desktopResult{"unknown", "notifier-timeout"}
				}
				return desktopResult{"failed", "notifier-unavailable"}
			}
			if mode == "crash-intent" {
				rpcPrivateDir(desktopDir())
				rpcSave(desktopPath(), desktopJournal{Version: 1, LastTry: now, Items: map[string]desktopReceipt{
					"task-one": {Active: true, Reason: "needs_me", Detected: now, State: "attempting", Attempts: 1},
				}})
			}
			threads := desktopThreads()[:1]
			_ = reconcileDesktop(threads, now, submit)
			if mode == "lost-result" {
				var crash desktopJournal
				rpcLoad(filepath.Join(desktopDir(), "crash.json"), &crash)
				rpcSave(desktopPath(), crash)
			}
			for i := 1; i < 20; i++ {
				_ = reconcileDesktop(threads, now.Add(time.Duration(i)*time.Minute), submit)
			}
			j, _ := loadDesktop()
			r := j.Items["task-one"]
			if mode == "failed" {
				if calls != 3 || r.Attempts != 3 || !r.NextTry.IsZero() {
					t.Fatalf("unbounded failure: %d %+v", calls, r)
				}
			} else {
				want := 1
				if mode == "crash-intent" {
					want = 0
				}
				if calls != want || r.State != "unknown" {
					t.Fatalf("ambiguous submission replayed: %d %+v", calls, r)
				}
			}
		})
	}
}

func TestDesktopCurrentEpisodesNotBacklog(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	now := time.Now()
	for i := 0; i < 100; i++ {
		writeEvent(Event{Bead: "old-task", Kind: EventNeedsYou, At: now.Add(-time.Duration(i+1) * time.Hour)})
	}
	calls := 0
	submit := func() desktopResult { calls++; return desktopResult{"submitted", "notifier-exited-zero"} }
	if err := reconcileDesktop(nil, now, submit); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("replayed inbox backlog")
	}
	threads := desktopThreads()
	if err := reconcileDesktop(threads, now, submit); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("existing blockers did not coalesce")
	}
	if err := reconcileDesktop(nil, now.Add(time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	j, _ := loadDesktop()
	for _, r := range j.Items {
		if r.Active || r.State != "submitted" {
			t.Fatal("lost past outcome on resolution")
		}
	}
	if err := reconcileDesktop(threads, now.Add(2*time.Minute), submit); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("new episode not submitted")
	}
	// New prompts deduplicate; a transiently missing prompt doesn't reset it.
	threads[2].Prompt = &ApprovalPrompt{Hash: "first"}
	_ = reconcileDesktop(threads, now.Add(3*time.Minute), submit)
	threads[2].Prompt = nil
	_ = reconcileDesktop(threads, now.Add(4*time.Minute), submit)
	threads[2].Prompt = &ApprovalPrompt{Hash: "first"}
	_ = reconcileDesktop(threads, now.Add(5*time.Minute), submit)
	if calls != 2 {
		t.Fatal("transient prompt observation replayed")
	}
	threads[2].Prompt.Hash = "second"
	_ = reconcileDesktop(threads, now.Add(6*time.Minute), submit)
	if calls != 3 {
		t.Fatal("new approval prompt missed")
	}
}

func TestDesktopParkingAndCorruptionFailClosed(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	th := desktopThreads()[2]
	if err := os.MkdirAll(rpcDir(th.Bead.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rpcParkPath(th.Bead.ID), []byte("parked"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	submit := func() desktopResult { calls++; return desktopResult{"submitted", "notifier-exited-zero"} }
	if err := reconcileDesktop([]Thread{th}, time.Now(), submit); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("migration parking treated as an approval")
	}
	if !rpcParked(th.Bead.ID) {
		t.Fatal("notification changed parking")
	}
	for _, broken := range []string{"broken", "null", "{}", `{"version":1,"items":null}`, `{"version":2,"items":{}}`} {
		if err := os.WriteFile(desktopPath(), []byte(broken), 0600); err != nil {
			t.Fatal(err)
		}
		if err := reconcileDesktop(desktopThreads(), time.Now(), submit); err == nil {
			t.Fatalf("corrupt journal silently reset: %s", broken)
		}
		if calls != 0 {
			t.Fatal("submitted without reliable ledger")
		}
	}
}

func TestDesktopRealCommandAdapter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if got := submitDesktop(); got != (desktopResult{"failed", "notifier-unavailable"}) {
		t.Fatalf("missing notifier: %+v", got)
	}
	file := filepath.Join(dir, "notify-send")
	logfile := filepath.Join(dir, "args")
	t.Setenv("NOTIFY_ARGS", logfile)
	if err := os.WriteFile(file, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$NOTIFY_ARGS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := submitDesktop(); got.State != "submitted" {
		t.Fatalf("valid notifier: %+v", got)
	}
	args, _ := os.ReadFile(logfile)
	if string(args) != "--app-name\nquartermaster\n--\n"+desktopTitle+"\n"+desktopBody+"\n" {
		t.Fatalf("unsafe argv: %q", args)
	}
	os.WriteFile(file, []byte("#!/bin/sh\nexit 1\n"), 0700)
	if got := submitDesktop(); got != (desktopResult{"unknown", "notifier-nonzero-exit"}) {
		t.Fatalf("nonzero falsely safe: %+v", got)
	}
	os.WriteFile(file, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0700)
	if got := desktopCommand(25 * time.Millisecond); got != (desktopResult{"unknown", "notifier-timeout"}) {
		t.Fatalf("timeout falsely safe: %+v", got)
	}
}

func TestHerdrNotifyReturnsError(t *testing.T) {
	if err := (Herdr{bin: filepath.Join(t.TempDir(), "missing")}).Notify("title", "body"); err == nil {
		t.Fatal("notification error swallowed")
	}
}
