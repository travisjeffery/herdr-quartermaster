package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCoordinatorReceipt(t *testing.T, session, through string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": "custom_message", "customType": "quartermaster.inbox", "details": map[string]string{"session": session, "through": through}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

// Real authenticated Unix transport, with controllable responses. No live
// socket, journal, token, transcript, task database or terminal is touched.
func coordinatorFixture(t *testing.T, reply func(piRequest) piResponse) (Ticker, Agent, *[]piRequest) {
	t.Helper()
	fakeTools(t)
	dir, err := os.MkdirTemp("", "qm-ack-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("QUARTERMASTER_STATE_DIR", dir)
	if err := os.MkdirAll(piControlDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(piCoordinatorSocket()+".token", []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", piCoordinatorSocket())
	if err != nil {
		t.Fatal(err)
	}
	// Appends happen before the synchronous call returns; cleanup joins the
	// serving goroutine before inspecting the requests under the race detector.
	var calls []piRequest
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req piRequest
			json.Unmarshal(line, &req)
			calls = append(calls, req)
			res := piResponse{OK: false, Error: "bad token"}
			if req.Token == "fixture-token" {
				res = reply(req)
			}
			json.NewEncoder(conn).Encode(res)
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	a := Agent{Name: "quartermaster", Status: "working"}
	a.Session.Agent, a.Session.Kind, a.Session.Value = "pi", "path", filepath.Join(dir, "session.jsonl")
	return Ticker{log: log.New(io.Discard, "", 0)}, a, &calls
}

func TestPiCoordinatorArchivedAcknowledgement(t *testing.T) {
	for _, remaining := range []string{"older", "empty", "lost-state", "receipt-before-save"} {
		t.Run(remaining, func(t *testing.T) {
			now := time.Now()
			ack := now.Add(-time.Minute)
			cursor := fmt.Sprintf("%019d", ack.UnixNano())
			ticker, coord, _ := coordinatorFixture(t, func(req piRequest) piResponse {
				return piResponse{OK: true, Session: req.Session, Ack: cursor}
			})
			writeCoordinatorReceipt(t, coord.Session.Value, cursor)
			if err := writeEvent(Event{At: ack, Bead: "task-new", Kind: EventNeedsYou}); err != nil {
				t.Fatal(err)
			}
			if n, err := inboxDone(nil); err != nil || n != 1 {
				t.Fatalf("archive: %d %v", n, err)
			}
			if remaining == "older" {
				writeEvent(Event{At: ack.Add(-time.Minute), Bead: "task-old", Kind: EventNeedsYou})
			}
			st := TickerState{LastNudge: ack.Add(-time.Hour), CoordReady: now.Add(-time.Hour)}
			if remaining == "lost-state" {
				st = TickerState{}
			}
			if remaining == "receipt-before-save" {
				// Durable receipt survived; the older saved ticker state did not
				// observe it. Recover without writing the extension's journal.
				if err := saveState(st); err != nil {
					t.Fatal(err)
				}
				st = loadState()
			}
			before, _ := os.ReadFile(coord.Session.Value)
			for i := 0; i < 3; i++ {
				ticker.nudgePiCoordinator(&st, coord, now)
			}
			if !st.LastNudge.Equal(ack) || !st.CoordReady.IsZero() {
				t.Fatalf("not reconciled: %+v", st)
			}
			after, _ := os.ReadFile(coord.Session.Value)
			if string(before) != string(after) {
				t.Fatal("receipt replayed or transcript changed")
			}
			items, _ := readInbox()
			if remaining == "older" && len(items) != 1 {
				t.Fatal("ack cleared task inbox")
			}
			if err := saveState(st); err != nil {
				t.Fatal(err)
			}
			st = loadState()
			ticker.nudgePiCoordinator(&st, coord, now)
			if !st.LastNudge.Equal(ack) {
				t.Fatal("restart regressed ack")
			}
		})
	}
}

func TestPiCoordinatorRejectsUnverifiedAcknowledgement(t *testing.T) {
	for _, kind := range []string{"future", "malformed", "negative", "overflow", "stale-session", "unauthenticated", "missing-event", "event-mismatch", "missing-receipt", "receipt-session", "receipt-type", "partial-receipt", "bare-journal", "old"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			at := now.Add(-time.Minute)
			if kind == "future" {
				at = now.Add(time.Minute)
			}
			cursor := fmt.Sprintf("%019d", at.UnixNano())
			ticker, coord, _ := coordinatorFixture(t, func(req piRequest) piResponse {
				res := piResponse{OK: true, Session: req.Session, Ack: cursor}
				switch kind {
				case "malformed":
					res.Ack = "not-a-cursor"
				case "negative":
					res.Ack = "-000000000000000001"
				case "overflow":
					res.Ack = "9999999999999999999"
				case "stale-session":
					res.Session = "/other/session.jsonl"
				case "unauthenticated":
					res.OK, res.Error = false, "bad token"
				}
				return res
			})
			writeCoordinatorReceipt(t, coord.Session.Value, cursor)
			e := Event{At: at, Bead: "task-one", Kind: EventNeedsYou}
			if kind != "missing-event" {
				if err := writeEvent(e); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "event-mismatch":
				name := fmt.Sprintf("%d-task-one-needs_you.json", at.UnixNano())
				e.At = at.Add(-time.Second)
				rpcSave(filepath.Join(inboxDir(), name), e)
			case "missing-receipt", "bare-journal":
				os.WriteFile(coord.Session.Value, []byte("{}\n"), 0600)
			case "receipt-session":
				os.WriteFile(coord.Session.Value, []byte(fmt.Sprintf(`{"type":"custom_message","customType":"quartermaster.inbox","details":{"session":"/old","through":%q}}`+"\n", cursor)), 0600)
			case "receipt-type":
				os.WriteFile(coord.Session.Value, []byte(fmt.Sprintf(`{"type":"message","customType":"quartermaster.inbox","details":{"session":%q,"through":%q}}`+"\n", coord.Session.Value, cursor)), 0600)
			case "partial-receipt":
				b, _ := os.ReadFile(coord.Session.Value)
				os.WriteFile(coord.Session.Value, b[:len(b)-1], 0600)
			}
			st := TickerState{LastNudge: at.Add(-time.Hour)}
			if kind == "old" {
				st.LastNudge = at.Add(time.Second)
			}
			before := st.LastNudge
			ticker.nudgePiCoordinator(&st, coord, now)
			if !st.LastNudge.Equal(before) {
				t.Fatalf("accepted %s ack", kind)
			}
		})
	}
}
