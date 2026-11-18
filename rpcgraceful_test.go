package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gracefulSelection(t *testing.T) rpcRequest {
	t.Helper()
	r, err := rpcClient("b-1", rpcRequest{Op: "status"})
	if err != nil {
		t.Fatal(err)
	}
	i := rpcStopSelection(*r.Status, r.Status.Phase == "parked")
	return rpcRequest{Op: "stop-graceful", Expected: &i}
}

func TestRPCGracefulLegacyRefusesWithoutMutation(t *testing.T) {
	_ = fixtureSupervisor(t)
	l, err := net.Listen("unix", rpcSocket("b-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ops := make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			var req rpcRequest
			_ = json.NewDecoder(c).Decode(&req)
			ops <- req.Op
			_ = json.NewEncoder(c).Encode(rpcReply{OK: true, Status: &rpcStatus{Phase: "idle", PID: 123}})
			c.Close()
		}
	}()
	before, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "worker.json"))
	_, err = rpcGracefulStop("b-1", time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "legacy supervisor") {
		t.Fatal(err)
	}
	if op := <-ops; op != "status" {
		t.Fatal(op)
	}
	select {
	case op := <-ops:
		t.Fatal("legacy mutation:", op)
	default:
	}
	after, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "worker.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("reservation changed")
	}
}

func TestRPCGracefulGuards(t *testing.T) {
	for _, tc := range []string{"working", "parked-working", "blocked", "error", "generation", "sequence", "session", "parking", "intent", "corrupt", "unknown", "pending-handler", "pending-rpc", "streaming", "compacting", "queued", "incomplete-state", "wrong-pi-session", "malformed-pi-session", "missing-guard"} {
		t.Run(tc, func(t *testing.T) {
			s := rpcTestSupervisorState(t)
			s.status.GracefulStop, s.status.Generation = true, "instance"
			s.status.PID = os.Getpid()
			s.status.SessionID, s.status.SessionFile = "actual-pi-id", "actual-session.jsonl"
			s.proc = fixtureProcess(t, s.event)
			s.status.ChildPID = s.proc.cmd.Process.Pid
			i := rpcStopSelection(s.status, false)
			var release func()
			switch tc {
			case "working":
				s.status.Phase = "working"
			case "parked-working":
				s.status.Phase = "working"
				i.Parked = true
				_ = rpcSave(rpcParkPath("b-1"), true)
			case "blocked":
				s.status.Phase = "blocked"
			case "error":
				s.status.Error = "uncertain"
			case "generation":
				i.Generation = "old-instance"
			case "sequence":
				i.Seq++
			case "session":
				i.SessionID = "another-session"
			case "parking":
				_ = rpcSave(rpcParkPath("b-1"), true)
			case "intent", "unknown":
				_ = rpcSave(filepath.Join(rpcDir("b-1"), "requests", "r.json"), rpcReceipt{ID: "r", State: tc})
			case "corrupt":
				_ = os.WriteFile(filepath.Join(rpcDir("b-1"), "requests", "r.json"), []byte("{"), 0600)
			case "pending-handler":
				s.promptMu.Lock()
				release = s.promptMu.Unlock
			case "pending-rpc":
				s.proc.mu.Lock()
				s.proc.pending["outstanding"] = make(chan rpcResult, 1)
				s.proc.mu.Unlock()
			case "missing-guard":
				s.worker.Args = []string{"--quartermaster-control"}
			default:
				// Replace only the fixture child; the original is stopped by cleanup.
				overrides := map[string]string{"streaming": `{"isStreaming":true}`, "compacting": `{"isCompacting":true}`, "queued": `{"pendingMessageCount":1}`, "incomplete-state": `{"isStreaming":null}`, "wrong-pi-session": `{"sessionId":"wrong-session"}`, "malformed-pi-session": `{"sessionId":42}`}
				t.Setenv("QM_RPC_STATE", overrides[tc])
				s.proc = fixtureProcess(t, s.event)
				s.status.ChildPID = s.proc.cmd.Process.Pid
				i.ChildPID = s.status.ChildPID
			}
			r := s.handle(rpcRequest{Op: "stop-graceful", Expected: &i})
			if release != nil {
				release()
			}
			if r.OK {
				t.Fatal("unsafe stop accepted", tc)
			}
			if s.stopping {
				t.Fatal("refusal changed admission")
			}
			select {
			case <-s.stop:
				t.Fatal("refusal requested shutdown")
			default:
			}
			if !processAlive(s.proc.cmd.Process.Pid) {
				t.Fatal("refusal touched child")
			}
			select {
			case <-s.proc.done:
				t.Fatal("refusal closed transport")
			default:
			}
			if s.status.SessionID != "actual-pi-id" {
				t.Fatal("refusal overwrote saved identity")
			}
		})
	}
}

func TestRPCGracefulStopResumePreserves(t *testing.T) {
	for _, parked := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "parked"}[parked], func(t *testing.T) {
			launch := fixtureSupervisor(t)
			if parked {
				_ = rpcSave(rpcParkPath("b-1"), map[string]string{"reason": "retained barrier"})
			}
			cmd := launch()
			receipt := filepath.Join(rpcDir("b-1"), "requests", "accepted.json")
			_ = rpcSave(receipt, rpcReceipt{ID: "accepted", State: "accepted", Hash: "retained"})
			_ = os.WriteFile(filepath.Join(stateDir(), "dirty-worktree"), []byte("do not modify\n"), 0600)
			st, _ := rpcClient("b-1", rpcRequest{Op: "status"})
			paths := []string{st.Status.SessionFile, receipt, filepath.Join(rpcDir("b-1"), "worker.json"), filepath.Join(stateDir(), "dirty-worktree")}
			if parked {
				paths = append(paths, rpcParkPath("b-1"))
			}
			original := map[string][]byte{}
			for _, path := range paths {
				original[path], _ = os.ReadFile(path)
			}
			if _, err := rpcGracefulStop("b-1", 5*time.Second); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			cmd = launch()
			after, _ := rpcClient("b-1", rpcRequest{Op: "status"})
			if after.Status.SessionID != st.Status.SessionID || after.Status.SessionFile != st.Status.SessionFile || after.Status.Generation == st.Status.Generation {
				t.Fatal("resume identity", after)
			}
			for path, before := range original {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, got) {
					t.Fatal("changed preservation artifact", path, err)
				}
			}
			events, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "events.jsonl"))
			if bytes.Contains(events, []byte("agent_start")) || bytes.Contains(events, []byte(`"command":"prompt"`)) {
				t.Fatal("automatic prompt/replay")
			}
			if parked && after.Status.Phase != "parked" {
				t.Fatal("lost parking")
			}
			if _, err := rpcGracefulStop("b-1", 5*time.Second); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRPCGracefulTimeoutRetainsOwnerBeyondOldKillWindow(t *testing.T) {
	launch := fixtureSupervisor(t)
	release := filepath.Join(stateDir(), "release-fixture")
	t.Setenv("QM_RPC_EOF_RELEASE", release)
	t.Setenv("QM_RPC_EOF_CLOSE_STDOUT", "1") // transport cleanup must not escalate either
	cmd := launch()
	defer os.WriteFile(release, nil, 0600)
	st, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	if _, err := rpcGracefulStop("b-1", 50*time.Millisecond); err == nil {
		t.Fatal("unresponsive child reported stopped")
	}
	time.Sleep(5200 * time.Millisecond)
	if !processAlive(st.Status.ChildPID) || !processAlive(st.Status.PID) {
		t.Fatal("timeout killed owner")
	}
	if lease, err := rpcLease("b-1"); err == nil {
		lease.Close()
		t.Fatal("lost reservation lease")
	}
	if err := rpcServe(Config{}, "b-1"); err == nil {
		t.Fatal("overlapping writer started")
	}
	if _, err := rpcResume(Config{}, "b-1", "worker"); err == nil {
		t.Fatal("resume accepted stopping owner")
	}
	for _, req := range []rpcRequest{{Op: "prompt", ID: "no-replay", Text: "never execute"}, {Op: "cancel"}, {Op: "activate"}} {
		if _, err := rpcClient("b-1", req); err == nil {
			t.Fatal("reopened admission", req.Op)
		}
	}
	if !rpcExists("b-1") {
		t.Fatal("reservation disappeared")
	}
	if _, err := os.Stat(filepath.Join(rpcDir("b-1"), "requests", "no-replay.json")); !os.IsNotExist(err) {
		t.Fatal("new receipt after barrier", err)
	}
	_ = os.WriteFile(release, nil, 0600)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	var final rpcStatus
	_ = rpcLoad(filepath.Join(rpcDir("b-1"), "status.json"), &final)
	if final.Phase != "stopped" || final.ChildPID != 0 {
		t.Fatal(final)
	}
}

func TestRPCGracefulSaveFailureRetainsBarrierWithoutEOF(t *testing.T) {
	s := rpcTestSupervisorState(t)
	s.status = rpcStatus{Phase: "idle", GracefulStop: true, Generation: "instance", PID: os.Getpid(), SessionID: "actual-pi-id", SessionFile: "actual-session.jsonl"}
	s.proc = fixtureProcess(t, nil)
	s.status.ChildPID = s.proc.cmd.Process.Pid
	i := rpcStopSelection(s.status, false)
	// Fail only the final status save; this transport fixture has no observer.
	path := filepath.Join(rpcDir("b-1"), "status.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if r := s.handle(rpcRequest{Op: "stop-graceful", Expected: &i}); r.OK {
		t.Fatal("save failure accepted")
	}
	if !s.stopping {
		t.Fatal("save failure reopened admission")
	}
	select {
	case <-s.stop:
		t.Fatal("save failure requested EOF")
	default:
	}
	if _, err := s.proc.call("still-open", "get_state", nil, time.Second); err != nil {
		t.Fatal("save failure touched transport", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Explicit recovery on this known new supervisor; never a legacy fallback.
	if r := s.handle(rpcRequest{Op: "stop"}); !r.OK {
		t.Fatal(r)
	}
	select {
	case <-s.stop:
	default:
		t.Fatal("explicit recovery did not request EOF")
	}
}

func TestRPCOrdinaryCrashResumeRetainsUncertainReceipt(t *testing.T) {
	launch := fixtureSupervisor(t)
	cmd := launch()
	before, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	_, _ = rpcClient("b-1", rpcRequest{Op: "prompt", ID: "crashed", Text: "crash"})
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rpcDir("b-1"), "requests", "crashed.json")
	receipt, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	launch()
	after, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	if before.Status.SessionID != after.Status.SessionID || before.Status.SessionFile != after.Status.SessionFile {
		t.Fatal("crash recovery changed identity")
	}
	if r, err := rpcClient("b-1", rpcRequest{Op: "prompt", ID: "crashed", Text: "crash"}); err != nil || r.Receipt.State != "intent" {
		t.Fatal("uncertain request replayed", r, err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(receipt, got) {
		t.Fatal("rewrote ambiguous receipt")
	}
	if _, err := rpcGracefulStop("b-1", time.Second); err == nil {
		t.Fatal("uncertain receipt eligible for guarded stop")
	}
}

func TestRPCGracefulResumeRejectsUncertainReceiptsAndWrongSession(t *testing.T) {
	launch := fixtureSupervisor(t)
	cmd := launch()
	if _, err := rpcGracefulStop("b-1", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rpcDir("b-1"), "requests", "uncertain.json")
	_ = rpcSave(path, rpcReceipt{ID: "uncertain", State: "intent"})
	if err := rpcServe(Config{}, "b-1"); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	var before, after rpcStatus
	_ = rpcLoad(filepath.Join(rpcDir("b-1"), "status.json"), &before)
	t.Setenv("QM_RPC_STATE", `{"sessionId":"wrong-session"}`)
	if err := rpcServe(Config{}, "b-1"); err == nil {
		t.Fatal("wrong session resumed")
	}
	_ = rpcLoad(filepath.Join(rpcDir("b-1"), "status.json"), &after)
	if after.SessionID != before.SessionID || after.SessionFile != before.SessionFile {
		t.Fatal("lost saved identity")
	}
}
