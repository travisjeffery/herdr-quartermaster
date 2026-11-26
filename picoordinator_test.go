package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiCoordinatorPolicy(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command("node", "--test", "testdata/pi-coordinator.test.mjs")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestPiCoordinatorNeverPastes(t *testing.T) {
	dir := fakeTools(t)
	cfg := defaultConfig()
	cfg.CoordinatorAgent = "pi"
	cfg.Nudge = true
	ticker := Ticker{cfg: cfg, herdr: newHerdr(), log: log.New(io.Discard, "", 0)}
	now := time.Now()
	if err := writeEvent(Event{At: now, Kind: "changed", Bead: "task-1"}); err != nil {
		t.Fatal(err)
	}
	a := Agent{Name: cfg.CoordinatorName, PaneID: "fixture:p1", Status: "idle"}
	a.Session.Agent = "pi"
	a.Session.Kind = "path"
	a.Session.Value = "/fixture/session.jsonl"
	agents := map[string]Agent{a.Name: a}
	st := &TickerState{CoordReady: now.Add(-time.Hour)}
	ticker.nudgeCoordinator(st, agents, now) // absent socket
	if !st.LastNudge.IsZero() {
		t.Fatal("absent socket acknowledged")
	}
	if err := os.MkdirAll(piControlDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", piCoordinatorSocket())
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	listener.Close() // stale socket/token
	os.WriteFile(piCoordinatorSocket()+".token", []byte("fixture"), 0o600)
	ticker.nudgeCoordinator(st, agents, now)
	if !st.LastNudge.IsZero() {
		t.Fatal("stale socket acknowledged")
	}
	// Unknown/mismatched identity and remote pi must fail closed too.
	for _, kind := range []string{"", "id"} {
		a.Session.Kind = kind
		agents[a.Name] = a
		ticker.nudgeCoordinator(st, agents, now)
	}
	a.Session.Agent = ""
	agents[a.Name] = a
	ticker.nudgeCoordinator(st, agents, now)
	if lines := logLines(t, dir, "herdr"); strings.TrimSpace(strings.Join(lines, "")) != "" {
		t.Fatalf("terminal fallback: %v", lines)
	}
}

func TestPiCoordinatorDurableAcceptanceAndRetry(t *testing.T) {
	fakeTools(t)
	short, err := os.MkdirTemp("", "qm-inbox-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	t.Setenv("QUARTERMASTER_STATE_DIR", short)
	os.MkdirAll(piControlDir(), 0o700)
	os.WriteFile(piCoordinatorSocket()+".token", []byte("fixture"), 0o600)
	ln, err := net.Listen("unix", piCoordinatorSocket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	calls := make(chan piRequest, 8)
	go func() {
		for i := 0; i < 3; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req piRequest
			json.Unmarshal(line, &req)
			calls <- req
			// First reply lost after acceptance, then stale-session response, then success.
			if i == 1 {
				json.NewEncoder(conn).Encode(piResponse{OK: true, Session: "/wrong"})
			}
			if i == 2 {
				json.NewEncoder(conn).Encode(piResponse{OK: true, Session: req.Session, Ack: req.Through})
			}
			conn.Close()
		}
	}()
	cfg := defaultConfig()
	cfg.CoordinatorAgent = "pi"
	cfg.Nudge = true
	ticker := Ticker{cfg: cfg, herdr: newHerdr(), log: log.New(io.Discard, "", 0)}
	now := time.Now()
	writeEvent(Event{At: now, Bead: "task-1", Kind: "changed"})
	a := Agent{Name: cfg.CoordinatorName, Status: "working"}
	a.Session.Agent = "pi"
	a.Session.Kind = "path"
	a.Session.Value = filepath.Join(short, "session.jsonl")
	writeCoordinatorReceipt(t, a.Session.Value, fmt.Sprintf("%019d", now.UnixNano()))
	st := &TickerState{}
	for i := 0; i < 3; i++ {
		ticker.nudgeCoordinator(st, map[string]Agent{a.Name: a}, now)
		if i < 2 && !st.LastNudge.IsZero() {
			t.Fatal("unconfirmed delivery acknowledged")
		}
	}
	first, second, third := <-calls, <-calls, <-calls
	if first != second || second != third || first.Op != "inbox" || first.Text != "" {
		t.Fatalf("unstable retry: %#v %#v %#v", first, second, third)
	}
	if !st.LastNudge.Equal(now) {
		t.Fatalf("not acknowledged: %v", st.LastNudge)
	}
}

func TestPiCoordinatorCompanionEmbedded(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	file, err := piExtensionFile()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "coordinator.ts"))
	if err != nil || string(data) != string(piCoordinatorExtension) {
		t.Fatalf("missing companion: %v", err)
	}
	// Keep the coordinator transport free of editor mutations and user prompts.
	for _, s := range []string{".sendUserMessage(", ".setEditorText(", ".pasteToEditor(", ".setEditorComponent("} {
		if strings.Contains(string(data), s) {
			t.Fatalf("unsafe coordinator API: %s", s)
		}
	}
}

func TestPiCoordinatorStatusAndWakeState(t *testing.T) {
	idleMs := int64(42000)
	ticker, coord, _ := coordinatorFixture(t, func(req piRequest) piResponse {
		return piResponse{
			OK:        true,
			Session:   req.Session,
			Queued:    true,
			WakeState: "ready",
			IdleMs:    &idleMs,
		}
	})
	now := time.Now()
	if err := writeEvent(Event{At: now.Add(-time.Minute), Bead: "task-1", Kind: EventMerged}); err != nil {
		t.Fatal(err)
	}
	st := TickerState{}
	got := ticker.nudgePiCoordinator(&st, coord, now)
	if got == nil || got.WakeState != "ready" || !got.Queued || got.IdleMs == nil || *got.IdleMs != idleMs {
		t.Fatalf("nudgePiCoordinator status = %+v, want ready/queued/42000ms", got)
	}
	probed, ok := probePiCoordinatorStatus(coord)
	if !ok || probed == nil || probed.WakeState != "ready" || !probed.Queued || probed.IdleMs == nil || *probed.IdleMs != idleMs {
		t.Fatalf("probePiCoordinatorStatus = %+v (%v), want ready/queued/42000ms", probed, ok)
	}
}
