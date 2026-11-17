package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiRPCGuardParity(t *testing.T) {
	// Exercise the real embedded extension's deny, ask, allow-list, timeout,
	// stale-answer and audit behavior in RPC mode, not just a protocol mock.
	t.Setenv("QM_TEST_PI_MODE", "rpc")
	t.Setenv("QUARTERMASTER_RPC", "1")
	t.Run("ask", TestPiControlAsk)
	t.Run("checked-answer", TestAnswerPiAsk)
	t.Run("session-and-subagent", func(t *testing.T) {
		h := startHarness(t, "b-1")
		for _, op := range []string{"switch", "fork"} {
			h.send(op)
			r, _ := h.next(func(m map[string]any) bool { return m["op"] == op })
			if r["cancel"] != true {
				t.Fatalf("session changed behind lease: %v", r)
			}
		}
		h.send("subagent missing-broad-agent")
		r, _ := h.next(func(m map[string]any) bool { return m["op"] == "subagent" })
		if r["block"] != true {
			t.Fatal("unguarded subagent allowed")
		}
	})
}

func TestRPCBackpressureStopsTransport(t *testing.T) {
	cmd := exec.Command("sleep", "30") // deliberately never reads stdin
	p, err := startRPC(cmd, nil, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); p.stop(); <-p.exited; <-p.drained }()
	p.writeTimeout = 20 * time.Millisecond
	start := time.Now()
	if err = p.write(map[string]string{"type": "prompt", "message": strings.Repeat("x", 1<<20)}); err == nil {
		t.Fatal("write unexpectedly drained")
	}
	if time.Since(start) > time.Second {
		t.Fatal("backpressure not bounded")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("partially written protocol left usable")
	}
}

func TestRPCEnvironmentAndArgs(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "coordinator:p1")
	t.Setenv("PI_SESSION_ID", "coordinator")
	t.Setenv("PI_SESSION_FILE", "/coordinator.jsonl")
	t.Setenv("PI_MODEL", "wrong")
	t.Setenv("QUARTERMASTER_EXEC_SOCKET", "/wrong.sock")
	cfg := Config{BeadsDir: "/fixture/beads", Agents: map[string]AgentConfig{"pi": {Env: []string{"CUSTOM=value"}}}}
	env := strings.Join(rpcWorkerEnv(cfg, "b-1"), "\n")
	for _, bad := range []string{"HERDR_ENV=", "HERDR_PANE_ID=", "PI_SESSION_ID=", "PI_SESSION_FILE=", "PI_MODEL=", "QUARTERMASTER_EXEC_SOCKET=/wrong"} {
		if strings.Contains(env, bad) {
			t.Errorf("inherited coordinator identity: %s", bad)
		}
	}
	for _, want := range []string{"BEADS_ACTOR=pi", "BEADS_DIR=/fixture/beads", "CUSTOM=value", "QUARTERMASTER_RPC=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, flag := range []string{"--mode=rpc", "--session", "--no-session", "--session-dir", "--quartermaster-control", "--print", "--fork"} {
		cfg.Agents["pi"] = AgentConfig{Args: []string{flag}}
		if _, err := rpcLaunchArgs(cfg, "b-1"); err == nil {
			t.Errorf("accepted conflicting flag %s", flag)
		}
	}
	if cfg := defaultConfig(); cfg.WorkerBackend != "" && cfg.WorkerBackend != "herdr" {
		t.Fatal("changed default backend")
	}
}

func TestRPCHandledAndLateResponse(t *testing.T) {
	launch := fixtureSupervisor(t)
	launch()
	r, err := rpcClient("b-1", rpcRequest{Op: "prompt", ID: "handled", Text: "delay"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Receipt.State != "accepted" {
		t.Fatal(r)
	}
	st, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	if st.Status.Phase != "idle" {
		t.Fatalf("handled prompt waits for nonexistent run: %+v", st.Status)
	}
	if !rpcReservedPath(filepath.Join(rpcDir("b-1"), "worktree")) || rpcReservedPath("/unrelated/worktree") {
		t.Fatal("worktree retention scope")
	}
	// Slow/absent output viewers don't participate in the stdout drain.
	for i := 0; i < 8; i++ {
		_, err = rpcClient("b-1", rpcRequest{Op: "prompt", ID: rpcID(), Text: "hello"})
		if err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "events.jsonl"))
	if bytes.Count(b, []byte(`"type":"agent_settled"`)) != 8 {
		t.Fatal("stdout stopped without viewer")
	}
}
