package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in companion to scripts/pi-coordinator-trial.py, not a live-system probe.
func TestPiCoordinatorPTYReconcile(t *testing.T) {
	root := os.Getenv("QM_COORDINATOR_FIXTURE")
	if root == "" {
		t.Skip("isolated real-pi trial only")
	}
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), "qm-coord-") {
		t.Fatal("not an isolated fixture root")
	}
	var fixture struct {
		Session string `json:"session"`
		Through string `json:"through"`
	}
	if err := rpcLoad(filepath.Join(root, "reconcile.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fixture.Session, root+string(os.PathSeparator)) {
		t.Fatal("session outside fixture")
	}
	t.Setenv("QUARTERMASTER_STATE_DIR", filepath.Join(root, "state"))
	ns, err := strconv.ParseInt(fixture.Through, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(0, ns)
	for _, e := range []Event{
		{At: at.Add(-time.Second), Bead: "fixture-old", Kind: EventNeedsYou},
		{At: at, Bead: "fixture-new", Kind: EventNeedsYou},
	} {
		if err := writeEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := inboxDone([]string{"fixture-new"}); err != nil || n != 1 {
		t.Fatalf("archive: %d %v", n, err)
	}
	a := Agent{}
	a.Session.Agent, a.Session.Kind, a.Session.Value = "pi", "path", fixture.Session
	ticker := Ticker{log: log.New(io.Discard, "", 0)}
	before, err := os.ReadFile(fixture.Session)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st := TickerState{LastNudge: at.Add(-time.Hour)} // lost ticker save
		for n := 0; n < 3; n++ {
			ticker.nudgePiCoordinator(&st, a, time.Now())
		}
		if !st.LastNudge.Equal(at) {
			t.Fatalf("real receipt not reconciled after archive round %d", i)
		}
		if i == 0 {
			items, _ := readInbox()
			if len(items) != 1 {
				t.Fatal("receipt cleared actionable inbox")
			}
			if _, err := inboxDone(nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, err := os.ReadFile(fixture.Session)
	if err != nil || string(after) != string(before) {
		t.Fatal("reconciliation changed transcript")
	}
}
