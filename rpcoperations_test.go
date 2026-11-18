package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func emitOperationEvent(t *testing.T, s *rpcSupervisor, event any) {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var rec rpcRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if err := s.event(raw, rec); err != nil {
		t.Fatal(err)
	}
}

func TestRPCSteeringStrandedCannotFlushWithLaterPrompt(t *testing.T) {
	for _, mode := range []string{"settled-before-response", "settled-after-response", "late-queue", "consumed"} {
		t.Run(mode, func(t *testing.T) {
			isolateRPCOperations(t)
			s := rpcTestSupervisorState(t)
			s.status.Steering, s.status.Phase = true, "working"
			s.status.SessionID, s.status.SessionFile = "actual-pi-id", "actual-session.jsonl"
			t.Setenv("QM_RPC_STATE", `{"isStreaming":true}`)
			s.proc = fixtureProcess(t, s.event)
			req := rpcRequest{Op: "steer", ID: "direction", Text: "change"}
			if mode == "settled-before-response" {
				req.Text = "delay"
				done := make(chan rpcReply, 1)
				go func() { done <- s.handle(req) }()
				waitRPC(t, func() bool {
					_, err := os.Stat(filepath.Join(rpcDir("b-1"), "requests", "direction.json"))
					return err == nil
				})
				emitOperationEvent(t, s, map[string]any{"type": "agent_settled"})
				if r := <-done; !r.OK {
					t.Fatal(r)
				}
			} else {
				if r := s.handle(req); !r.OK {
					t.Fatal(r)
				}
				if mode == "consumed" || mode == "late-queue" {
					emitOperationEvent(t, s, map[string]any{"type": "queue_update", "steering": []string{}})
				}
				emitOperationEvent(t, s, map[string]any{"type": "agent_settled"})
				if mode == "late-queue" {
					emitOperationEvent(t, s, map[string]any{"type": "queue_update", "steering": []string{"late direction"}})
				}
			}
			st := s.handle(rpcRequest{Op: "status"}).Status
			if mode == "consumed" {
				if st.Phase != "idle" || st.Error != "" {
					t.Fatal(st)
				}
				return
			}
			if st.Phase != "blocked" || st.Error != rpcStrandedSteering {
				t.Fatal(st)
			}
			if r := s.handle(rpcRequest{Op: "prompt", ID: "ticker", Text: "unrelated automatic notice"}); r.OK {
				t.Fatal("silent carry-over", r)
			}
			if r := s.handle(req); !r.OK || r.Receipt.State != "accepted" {
				t.Fatal("cannot reconcile while blocked", r)
			}
			if r := s.handle(rpcRequest{Op: "cancel"}); !r.OK {
				t.Fatal(r)
			}
			st = s.handle(rpcRequest{Op: "status"}).Status
			if st.Error != "" || st.Phase != "idle" {
				t.Fatal("cancel did not clear queue latch", st)
			}
			if r := s.handle(rpcRequest{Op: "prompt", ID: "explicit", Text: "new instruction after cancellation"}); !r.OK {
				t.Fatal(r)
			}
			// Cancellation cannot clear a real approval/dialog blocker.
			s.mu.Lock()
			s.status.Error = "unsupported extension dialog"
			s.status.Phase = "blocked"
			s.mu.Unlock()
			if r := s.handle(rpcRequest{Op: "cancel"}); !r.OK {
				t.Fatal(r)
			}
			if st := s.handle(rpcRequest{Op: "status"}).Status; st.Error == "" || st.Phase != "blocked" {
				t.Fatal("cancel answered blocker", st)
			}
		})
	}
}

func TestRPCForkRechecksUnderLeasesBeforeIntent(t *testing.T) {
	for _, refuseAt := range []int{1, 2} {
		t.Run(string(rune('0'+refuseAt)), func(t *testing.T) {
			f := newForkFixture(t)
			calls := 0
			recheck := func() error {
				calls++
				for _, id := range []string{f.source.ID, f.dest.ID} {
					if lease, err := rpcLease(id); err == nil {
						lease.Close()
						t.Fatal("recheck without ownership lease", id)
					}
				}
				if calls == refuseAt {
					return errors.New("fixture concurrent claim")
				}
				return nil
			}
			if _, err := rpcForkPrepare(f.cfg, f.source, f.dest, "checkpoint", []string{"--mode", "rpc"}, recheck); err == nil {
				t.Fatal("ignored concurrent claim")
			}
			if rpcExists(f.dest.ID) {
				t.Fatal("captured changed bead")
			}
		})
	}
}

func TestRPCOperationAutonomyAndAllowlist(t *testing.T) {
	f := newForkFixture(t)
	f.cfg.Autonomy = AutonomyConfig{Level: 2, Max: 7}
	source, _ := json.Marshal([]Bead{f.source})
	dest, _ := json.Marshal([]Bead{f.dest})
	script := "#!/bin/sh\nif [ \"$1\" != show ]; then exit 21; fi\ncase \"$2\" in source) printf '%s\\n' " + shellQuote(string(source)) + ";; child) printf '%s\\n' " + shellQuote(string(dest)) + ";; *) exit 22;; esac\n"
	if err := os.WriteFile(filepath.Join(stateDir(), "bin", "bd"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"steer", "fork"} {
		class, err := f.cfg.actionClass(f.source, action, "")
		if err != nil || class != 3 {
			t.Fatal(action, class, err)
		}
	}
	if _, err := rpcSteerCmd(f.cfg, f.source.ID, "id", "direction", "worker"); !errors.Is(err, errNotAutonomous) {
		t.Fatal("steer not gated", err)
	}
	var out bytes.Buffer
	if err := rpcForkCmd(f.cfg, []string{"fork", f.source.ID, f.dest.ID, "checkpoint"}, "worker", &out); !errors.Is(err, errNotAutonomous) {
		t.Fatal("fork not gated", err)
	}
	if rpcExists(f.dest.ID) {
		t.Fatal("autonomy refusal wrote reservation")
	}
	s := &rpcSupervisor{}
	for _, op := range []string{"fork", "clone", "switch_session", "new_session", "bash", "set_model", "extension_ui_response"} {
		if r := s.handle(rpcRequest{Op: op}); r.OK {
			t.Fatal("raw escape hatch", op)
		}
	}
}

func TestRPCMessageLegacyReceiptCompatibility(t *testing.T) {
	isolateRPCOperations(t)
	s := rpcTestSupervisorState(t)
	old := rpcReceipt{ID: "old", Hash: rpcMessageHash("prompt", "original"), State: "accepted"}
	if err := rpcSave(filepath.Join(rpcDir("b-1"), "requests", "old.json"), old); err != nil {
		t.Fatal(err)
	}
	if r := s.handle(rpcRequest{Op: "prompt", ID: "old", Text: "original"}); !r.OK {
		t.Fatal(r)
	}
	if r := s.handle(rpcRequest{Op: "steer", ID: "old", Text: "original"}); r.OK {
		t.Fatal("old receipt reinterpreted as steering")
	}
}

func TestRPCSteeringUncertainHandlerCannotEscapeCancellation(t *testing.T) {
	isolateRPCOperations(t)
	s := rpcTestSupervisorState(t)
	s.status.Phase = "working"
	s.steeringInFlight = true // transport deadline elapsed, pi input handler still unresolved
	s.proc = fixtureProcess(t, s.event)
	r := rpcReceipt{ID: "uncertain", Operation: "steer", State: "intent", Hash: rpcMessageHash("steer", "late")}
	if err := rpcSave(filepath.Join(rpcDir("b-1"), "requests", r.ID+".json"), r); err != nil {
		t.Fatal(err)
	}
	if got := s.handle(rpcRequest{Op: "cancel"}); got.OK {
		t.Fatal("cancel claimed to settle unknown acceptance")
	}
	if got := s.handle(rpcRequest{Op: "prompt", ID: "ticker", Text: "new work"}); got.OK {
		t.Fatal("escaped queue fence")
	}
	emitOperationEvent(t, s, map[string]any{"type": "queue_update", "steering": []string{"late"}})
	emitOperationEvent(t, s, rpcRecord{Type: "response", ID: r.ID, Command: "steer", Success: true, Data: json.RawMessage(`{"disposition":"queued"}`)})
	if got := s.handle(rpcRequest{Op: "prompt", ID: "ticker", Text: "new work"}); got.OK {
		t.Fatal("late acceptance reopened fence")
	}
	if got := s.handle(rpcRequest{Op: "cancel"}); !got.OK {
		t.Fatal(got)
	}
	if got := s.handle(rpcRequest{Op: "status"}); got.Status.Phase != "idle" || got.Status.Error != "" {
		t.Fatal(got.Status)
	}
}
