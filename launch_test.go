package main

import (
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLaunchedWith(t *testing.T) {
	argv := [][]string{{"/bin/claude", "--model", "x", "--dangerously-skip-permissions"}, {"mcp-proxy"}}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"--dangerously-skip-permissions"}, true},
		{[]string{"--model", "x"}, true},
		{[]string{"x", "--model"}, false},
		{[]string{"--dangerously-bypass-approvals-and-sandbox"}, false},
		{nil, false},
	} {
		if got := launchedWith(argv, tc.args); got != tc.want {
			t.Errorf("launchedWith(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// A worker launched with bypass args whose level dropped below 9 is asked to
// exit once idle, gets nothing else meanwhile, and is resumed on a later pass.
func TestRelaunchOnLevelChange(t *testing.T) {
	dir := fakeTools(t)
	tk := Ticker{cfg: defaultConfig(), herdr: newHerdr(), log: log.New(io.Discard, "", 0)}
	now := time.Now()
	idle := Agent{Name: "b-1", Kind: "claude", PaneID: "w1:p1", Status: "idle"}
	th := Thread{Bead: Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:4"}}, Agent: &idle}

	t.Setenv("FAKE_PROCS", `[{"argv":["claude","--dangerously-skip-permissions"]}]`)
	due := tk.launchMismatch([]Thread{th})
	if !due["b-1"] {
		t.Fatal("a bypass worker below 9 is not due a relaunch")
	}
	// Not idle long enough yet: nothing happens.
	snap := Snapshot{ReadySince: now}
	tk.relaunch(th, &snap, true, now)
	if snap.Relaunch || hasLine(logLines(t, dir, "herdr"), "agent prompt") {
		t.Fatal("asked a just-idle worker to exit")
	}
	snap.ReadySince = now.Add(-time.Hour)
	tk.relaunch(th, &snap, true, now)
	if !snap.Relaunch || !hasLine(logLines(t, dir, "herdr"), "agent prompt w1:p1 /exit") {
		t.Fatalf("not asked to exit: %+v", snap)
	}
	// Its exit is not news to the coordinator.
	gone := func(prev Snapshot) bool {
		for _, e := range transition(Thread{Bead: th.Bead}, prev, false, now).Events {
			if e.Kind == EventAgentGone {
				return true
			}
		}
		return false
	}
	if gone(Snapshot{AgentStatus: "idle", Relaunch: true}) || !gone(Snapshot{AgentStatus: "idle"}) {
		t.Fatal("agent_gone events wrong around a relaunch")
	}
	// Its agent gone, it is resumed; resume finds nothing in the fake bd,
	// which the bead's notes record.
	th.Agent = nil
	tk.relaunch(th, &snap, false, now)
	if snap.Relaunch || !hasLine(logLines(t, dir, "bd"), "note b-1 quartermaster could not relaunch the worker without") {
		t.Fatalf("not resumed: %+v %q", snap, logLines(t, dir, "bd"))
	}

	// Without bypass args below 9, or with them under a 9 lease, nothing is due.
	t.Setenv("FAKE_PROCS", `[{"argv":["claude"]}]`)
	if due := tk.launchMismatch([]Thread{th, {Bead: th.Bead, Agent: &idle}}); len(due) != 0 {
		t.Fatalf("due %v", due)
	}
	lease := autonomyState{Leases: map[string]autonomyLease{"b-1": {Level: 9, Until: now.Add(time.Hour), Prev: ptr(4)}}}
	if err := saveAutonomy(lease); err != nil {
		t.Fatal(err)
	}
	at9 := Thread{Bead: Bead{ID: "b-1", Status: StatusInProgress, Labels: []string{"autonomy:9"}}, Agent: &idle}
	if due := tk.launchMismatch([]Thread{at9}); !due["b-1"] {
		t.Fatal("a worker at 9 without bypass args is not due a relaunch")
	}
	t.Setenv("FAKE_PROCS", `[{"argv":["claude","--dangerously-skip-permissions"]}]`)
	if due := tk.launchMismatch([]Thread{at9}); len(due) != 0 {
		t.Fatalf("due %v", due)
	}
	// A needs_me worker waits on TJ and is left alone.
	at9.Bead.Status, at9.Bead.Labels = StatusNeedsMe, []string{"autonomy:4"}
	if due := tk.launchMismatch([]Thread{at9}); len(due) != 0 {
		t.Fatalf("needs_me due %v", due)
	}
	// Autonomy off caps the lease too.
	if err := os.WriteFile(autonomyStopPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	at9.Bead.Status, at9.Bead.Labels = StatusInProgress, []string{"autonomy:9"}
	if tk.cfg.launchBypass(at9.Bead, localHost) {
		t.Fatal("bypass while autonomy is off")
	}
	if err := os.Remove(autonomyStopPath()); err != nil {
		t.Fatal(err)
	}
	if !tk.cfg.launchBypass(at9.Bead, localHost) || tk.cfg.launchBypass(at9.Bead, "server") {
		t.Fatal("bypass is for 9 leases on this machine only")
	}
}

// A level notice for a worker in bypass is queued, not typed in, so the
// ticker relaunches it first.
func TestTellWorkerHoldsForBypass(t *testing.T) {
	fakeTools(t)
	f := &fakeAutonomy{agent: &Agent{Name: "b-1", PaneID: "w1:p1", Status: "idle"}}
	env := f.env()
	env.bypassed = func(string, Agent) bool { return true }
	msg, err := tellWorker(env, Bead{ID: "b-1", Status: StatusInProgress}, levelNotice("b-1", 4, "the ticker (lease expired)"))
	if err != nil || len(f.calls) != 0 || queuedNotice(t, localHost, "b-1") == "" || !strings.Contains(msg, "bypass") {
		t.Fatalf("%q %v calls %q", msg, err, f.calls)
	}
}
