package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDesktopTickerSurvivesAgentDiscoveryFailure(t *testing.T) {
	dir := fakeTools(t)
	// Only fixture executables are reachable; never fall back to a real
	// notifier, Beads database or Herdr server.
	t.Setenv("PATH", dir)
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	beads := []Bead{}
	for _, th := range desktopThreads() {
		beads = append(beads, th.Bead)
	}
	data, _ := json.Marshal(beads)
	t.Setenv("FIXTURE_BEADS", string(data))
	write("bd", "echo \"$*\" >> \"$LOG/bd.log\"\nprintf '%s\\n' \"$FIXTURE_BEADS\"\n")
	write("herdr", "exit 1\n")
	notifier := filepath.Join(dir, "notify-send")
	if err := os.Rename(notifier, notifier+".held"); err != nil {
		t.Fatal(err)
	}
	if err := rpcPrivateDir(desktopDir()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Round(0)
	previous := desktopReceipt{Active: true, Reason: "worker_blocked", State: "unknown", Attempts: 1, Detected: now.Add(-time.Hour), Result: "interrupted-attempt"}
	if err := rpcSave(desktopPath(), desktopJournal{Version: 1, Items: map[string]desktopReceipt{"task-three": previous}}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ticker := newTicker(defaultConfig(), log.New(&logs, "", 0))
	st := loadState()
	for i := 0; i < 2; i++ {
		if err := ticker.once(&st); err == nil {
			t.Fatal("agent discovery error hidden")
		}
	}
	if !st.LastTick.IsZero() || len(st.Threads) != 0 {
		t.Fatal("ran normal transitions on unknown agents")
	}
	j, err := loadDesktop()
	if err != nil {
		t.Fatal(err)
	}
	if j.Items["task-three"] != previous {
		t.Fatal("agent outage reset unknown prior episode")
	}
	for _, id := range []string{"task-one", "task-two"} {
		r := j.Items[id]
		if r.State != "failed" || r.Attempts != 1 {
			t.Fatalf("lost bead blocker while Herdr unavailable: %+v", r)
		}
		// Move fixture retry clocks forward without waiting one minute.
		r.NextTry = now.Add(-time.Minute)
		j.Items[id] = r
	}
	if !strings.Contains(logs.String(), "notifier-unavailable") {
		t.Fatal("missing actionable notifier error")
	}
	j.LastTry = now.Add(-2 * time.Minute)
	if err := rpcSave(desktopPath(), j); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(notifier+".held", notifier); err != nil {
		t.Fatal(err)
	}
	if err := ticker.once(&st); err == nil {
		t.Fatal("agent error hidden on recovery")
	}
	j, _ = loadDesktop()
	for _, id := range []string{"task-one", "task-two"} {
		if r := j.Items[id]; r.State != "submitted" || r.Attempts != 2 {
			t.Fatalf("retry not independent of Herdr: %+v", r)
		}
	}
	if j.Items["task-three"] != previous {
		t.Fatal("unknown episode resubmitted")
	}
	if lines := logLines(t, dir, "notify-send"); len(lines) != 1 {
		t.Fatalf("not coalesced: %v", lines)
	}
	for _, line := range logLines(t, dir, "bd") {
		if !strings.HasPrefix(line, "list ") {
			t.Fatalf("notification mutated task: %s", line)
		}
	}
}
