package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// New tests never inherit the user's coordinator/config/Beads/home state.
func isolateRPCOperations(t *testing.T) {
	t.Helper()
	dir := rpcTestDir(t)
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "QUARTERMASTER_CONFIG_DIR", "QUARTERMASTER_STATE_DIR", "PI_CODING_AGENT_DIR", "BEADS_DIR"} {
		t.Setenv(key, filepath.Join(dir, key))
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(dir, "absent-herdr.sock"))
	t.Setenv("HERDR_ENV", "")
	t.Setenv("QUARTERMASTER_EXEC_SOCKET", "")
}

func TestRPCSteeringSeparateFromFollowUps(t *testing.T) {
	isolateRPCOperations(t)
	launch := fixtureSupervisor(t)
	t.Setenv("QM_RPC_TRACE", "1")
	t.Setenv("QM_RPC_STATE", `{"isStreaming":true}`)
	launch()
	if _, err := rpcClient("b-1", rpcRequest{Op: "steer", ID: "idle", Text: "direction"}); err == nil {
		t.Fatal("steered idle worker")
	}
	if _, err := rpcClient("b-1", rpcRequest{Op: "prompt", ID: "start", Text: "hold"}); err != nil {
		t.Fatal(err)
	}
	req := rpcRequest{Op: "steer", ID: "direction", Text: "use another approach"}
	first, err := rpcClient("b-1", req)
	if err != nil || first.Receipt.Operation != "steer" || first.Receipt.State != "accepted" {
		t.Fatalf("steer: %+v %v", first, err)
	}
	// Reconciliation is read-only even after completion/cancellation.
	if _, err := rpcClient("b-1", rpcRequest{Op: "cancel"}); err != nil {
		t.Fatal(err)
	}
	again, err := rpcClient("b-1", req)
	if err != nil || again.Receipt.Hash != first.Receipt.Hash {
		t.Fatalf("reconcile: %+v %v", again, err)
	}
	req.Op = "prompt"
	if _, err := rpcClient("b-1", req); err == nil {
		t.Fatal("steer receipt reused as follow-up")
	}
	req.Op, req.ID, req.Text = "steer", "start", "hold"
	if _, err := rpcClient("b-1", req); err == nil {
		t.Fatal("follow-up receipt reused as steer")
	}
	data, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "events.jsonl"))
	var commands []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		var e struct {
			Type  string
			Entry map[string]any
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Type == "entry_appended" {
			commands = append(commands, e.Entry)
		}
	}
	var steer, prompt, clear, abort int
	for i, c := range commands {
		switch c["type"] {
		case "steer":
			steer++
			if c["streamingBehavior"] != nil {
				t.Fatal("steer forwarded as prompt")
			}
		case "prompt":
			prompt++
			if c["streamingBehavior"] != "followUp" {
				t.Fatal("tell no longer queues follow-ups")
			}
		case "clear_queue":
			clear = i
		case "abort":
			abort = i
		}
	}
	if steer != 1 || prompt != 1 || clear >= abort || clear == 0 {
		t.Fatalf("replay or wrong cancellation ordering: %+v", commands)
	}
}

func TestRPCSteeringRefusals(t *testing.T) {
	for _, mode := range []string{"parked", "stopping", "blocked", "error", "legacy", "not-streaming", "compacting", "wrong-session", "headless"} {
		t.Run(mode, func(t *testing.T) {
			isolateRPCOperations(t)
			s := rpcTestSupervisorState(t)
			s.status.Steering, s.status.Phase = true, "working"
			s.status.SessionID, s.status.SessionFile = "actual-pi-id", "actual-session.jsonl"
			t.Setenv("QM_RPC_STATE", `{"isStreaming":true}`)
			switch mode {
			case "parked":
				_ = os.WriteFile(rpcParkPath("b-1"), []byte("park"), 0600)
			case "stopping":
				s.stopping = true
			case "blocked":
				s.status.Phase = "blocked"
			case "error":
				s.status.Error = "unanswered dialog"
			case "legacy":
				s.status.Steering = false
			case "not-streaming":
				t.Setenv("QM_RPC_STATE", `{"isStreaming":false}`)
			case "compacting":
				t.Setenv("QM_RPC_STATE", `{"isStreaming":true,"isCompacting":true}`)
			case "wrong-session":
				t.Setenv("QM_RPC_STATE", `{"isStreaming":true,"sessionId":"other"}`)
			case "headless":
				// A caller cannot claim an epoch for a non-enrolled worker.
			}
			s.proc = fixtureProcess(t, s.event)
			req := rpcRequest{Op: "steer", ID: "direction", Text: "change"}
			if mode == "headless" {
				req.Owner = "stale-epoch"
			}
			if r := s.handle(req); r.OK {
				t.Fatal("admitted", mode)
			}
			if _, err := os.Stat(filepath.Join(rpcDir("b-1"), "requests", req.ID+".json")); !os.IsNotExist(err) {
				t.Fatal("recorded intent despite refusal", err)
			}
		})
	}
}

func TestRPCSteeringLateResponseAndCancellation(t *testing.T) {
	isolateRPCOperations(t)
	s := rpcTestSupervisorState(t)
	s.status.Steering, s.status.Phase = true, "working"
	s.status.SessionID, s.status.SessionFile = "actual-pi-id", "actual-session.jsonl"
	t.Setenv("QM_RPC_STATE", `{"isStreaming":true}`)
	var mu sync.Mutex
	var responses []string
	s.proc = fixtureProcess(t, func(raw []byte, r rpcRecord) error {
		if r.Type == "response" {
			mu.Lock()
			responses = append(responses, r.Command)
			mu.Unlock()
		}
		return s.event(raw, r)
	})
	result := make(chan rpcReply, 1)
	go func() { result <- s.handle(rpcRequest{Op: "steer", ID: "late", Text: "delay"}) }()
	waitRPC(t, func() bool {
		var r rpcReceipt
		return rpcLoad(filepath.Join(rpcDir("b-1"), "requests", "late.json"), &r) == nil
	})
	cancel := s.handle(rpcRequest{Op: "cancel"})
	if r := <-result; !r.OK || !cancel.OK {
		t.Fatalf("steer/cancel: %+v %+v", r, cancel)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(responses, ",") != "get_state,steer,clear_queue,abort" {
		t.Fatal(responses)
	}
	// An intent left by a crash remains uncertain; it never reaches pi again.
	intent := rpcReceipt{ID: "unknown", Operation: "steer", Hash: rpcMessageHash("steer", "original"), State: "intent"}
	if err := rpcSave(filepath.Join(rpcDir("b-1"), "requests", "unknown.json"), intent); err != nil {
		t.Fatal(err)
	}
	if r := s.handle(rpcRequest{Op: "steer", ID: "unknown", Text: "original"}); !r.OK || r.Receipt.State != "intent" {
		t.Fatal(r)
	}
}

func TestRPCSteeringLateReceiptWithoutClient(t *testing.T) {
	isolateRPCOperations(t)
	s := rpcTestSupervisorState(t)
	r := rpcReceipt{ID: "late", Operation: "steer", Hash: rpcMessageHash("steer", "direction"), State: "intent"}
	if err := rpcSave(filepath.Join(rpcDir("b-1"), "requests", "late.json"), r); err != nil {
		t.Fatal(err)
	}
	// Event processing reconciles even after the waiting call has disappeared.
	s.proc = fixtureProcess(t, s.event)
	_, _ = s.proc.call("late", "steer", map[string]any{"message": "delay"}, time.Millisecond)
	waitRPC(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		var got rpcReceipt
		return rpcLoad(filepath.Join(rpcDir("b-1"), "requests", "late.json"), &got) == nil && got.State == "accepted"
	})
}
