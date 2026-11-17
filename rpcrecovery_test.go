package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rpcTestSupervisorState(t *testing.T) *rpcSupervisor {
	t.Helper()
	_ = fixtureSupervisor(t) // isolated paths and a fixture bd
	if err := rpcPrivateDir(filepath.Join(rpcDir("b-1"), "requests")); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(rpcDir("b-1"), "events.jsonl"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return &rpcSupervisor{worker: rpcWorker{Bead: "b-1"}, status: rpcStatus{Phase: "idle"}, log: log, stop: make(chan struct{})}
}

func TestRPCStopBarrierSurvivesLifecycleEvents(t *testing.T) {
	s := rpcTestSupervisorState(t)
	s.promptMu.Lock() // an already admitted submission
	stopped := make(chan rpcReply, 1)
	go func() { stopped <- s.handle(rpcRequest{Op: "stop"}) }()
	waitRPC(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.stopping })
	for _, rec := range []rpcRecord{{Type: "agent_start"}, {Type: "agent_settled"}, {Type: "extension_ui_request", Method: "setStatus", StatusKey: "quartermaster.ask"}} {
		raw, _ := json.Marshal(rec)
		if err := s.event(raw, rec); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-stopped:
		t.Fatal("stop acknowledged before admitted submission finished")
	default:
	}
	s.promptMu.Unlock()
	if r := <-stopped; !r.OK {
		t.Fatal(r)
	}
	if r := s.handle(rpcRequest{Op: "status"}); r.Status.Phase != "stopping" {
		t.Fatal(r.Status)
	}
	if r := s.handle(rpcRequest{Op: "prompt", ID: "after-stop", Text: "never run"}); r.OK {
		t.Fatal("late event reopened admission")
	}
	if _, err := os.Stat(filepath.Join(rpcDir("b-1"), "requests", "after-stop.json")); !os.IsNotExist(err) {
		t.Fatal("recorded a new prompt after stop")
	}
}

func TestRPCDialogAbortSerializesWithAdmittedPrompt(t *testing.T) {
	s := rpcTestSupervisorState(t)
	ui := make(chan struct{}, 1)
	clear := make(chan struct{}, 1)
	s.proc = fixtureProcess(t, func(_ []byte, r rpcRecord) error {
		if r.Type == "entry_appended" {
			ui <- struct{}{}
		}
		if r.Type == "response" && r.Command == "clear_queue" {
			clear <- struct{}{}
		}
		return nil
	})
	s.promptMu.Lock()
	done := make(chan struct{})
	go func() { s.blockDialog(rpcRecord{ID: "ui", Method: "confirm"}); close(done) }()
	select {
	case <-ui:
	case <-time.After(time.Second):
		t.Fatal("UI not cancelled before waiting on prompt")
	}
	select {
	case <-clear:
		t.Fatal("abort raced ahead of admitted prompt")
	case <-time.After(50 * time.Millisecond):
	}
	s.promptMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not finish")
	}
	select {
	case <-clear:
	default:
		t.Fatal("queue not cleared")
	}
}

func TestRPCResumeRefusesAmbiguousMetadata(t *testing.T) {
	for _, tc := range []struct{ name, body, artifact string }{
		{"malformed", "{", ""}, {"empty", "{}", ""},
		{"incomplete-launch", `{"phase":"starting","supervisor_pid":123}`, ""},
		{"missing-status", "", "sessions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = fixtureSupervisor(t)
			if tc.body != "" {
				if err := os.WriteFile(filepath.Join(rpcDir("b-1"), "status.json"), []byte(tc.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.artifact != "" {
				if err := os.Mkdir(filepath.Join(rpcDir("b-1"), tc.artifact), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := rpcPreviousStatus("b-1"); err == nil {
				t.Fatal("ambiguous identity treated as new launch")
			}
			if err := rpcServe(Config{}, "b-1"); err == nil {
				t.Fatal("started pi without exact identity")
			}
		})
	}
}

func TestRPCFirstLaunchCrashDoesNotCreateAnotherSession(t *testing.T) {
	_ = fixtureSupervisor(t)
	t.Setenv("QM_RPC_CRASH_STATE", "1")
	if err := rpcServe(Config{}, "b-1"); err == nil {
		t.Fatal("child failed before identity response")
	}
	session := filepath.Join(rpcDir("b-1"), "sessions", "actual-session.jsonl")
	if _, err := os.Stat(session); err != nil {
		t.Fatal("fixture did not create session before crash", err)
	}
	if err := rpcServe(Config{}, "b-1"); err == nil || !strings.Contains(err.Error(), "no confirmed session identity") {
		t.Fatalf("unsafe recovery: %v", err)
	}
}
