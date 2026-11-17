package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A no-model child exercising the actual OS pipes. It intentionally splits
// records, interleaves events/responses, delays replies and dies mid-request.
func TestRPCChildProcess(t *testing.T) {
	if os.Getenv("QM_RPC_CHILD") != "1" {
		return
	}
	args := os.Args
	arg := func(key string) string {
		for i, a := range args {
			if a == key && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	session := arg("--session")
	if session == "" {
		session = filepath.Join(arg("--session-dir"), "actual-session.jsonl")
	}
	if session != "actual-session.jsonl" {
		if _, err := os.Stat(session); os.IsNotExist(err) {
			_ = os.WriteFile(session, []byte("{\"type\":\"session\",\"id\":\"actual-pi-id\"}\n"), 0600)
		}
	}
	var mu sync.Mutex
	emit := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		defer mu.Unlock()
		os.Stdout.Write(b[:len(b)/2])
		os.Stdout.Write(append(b[len(b)/2:], '\n'))
	}
	response := func(id, typ string, data any) {
		emit(map[string]any{"type": "response", "id": id, "command": typ, "success": true, "data": data})
	}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 1024), 2<<20)
	for scan.Scan() {
		var req map[string]any
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			continue
		}
		typ, _ := req["type"].(string)
		id, _ := req["id"].(string)
		msg, _ := req["message"].(string)
		switch typ {
		case "get_state":
			if os.Getenv("QM_RPC_CRASH_STATE") == "1" {
				os.Exit(17)
			}
			response(id, typ, map[string]any{"sessionId": "actual-pi-id", "sessionFile": session})
		case "prompt":
			fmt.Fprintln(os.Stderr, "diagnostic, NOT protocol")
			if msg == "crash" {
				os.Exit(17)
			}
			if msg == "silence" {
				continue
			}
			if msg == "delay" {
				go func() {
					time.Sleep(120 * time.Millisecond)
					response(id, "prompt", map[string]any{"disposition": "handled"})
				}()
				continue
			}
			if msg == "dialog" {
				emit(map[string]any{"type": "extension_ui_request", "method": "confirm", "title": "Dangerous operation", "id": "ui-1"})
				response(id, typ, map[string]any{"disposition": "handled"})
				continue
			}
			emit(map[string]any{"type": "agent_start"})
			response(id, typ, map[string]any{"disposition": "started"})
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": "hello\u2028world"}}}})
			emit(map[string]any{"type": "agent_end"})
			if msg != "hold" {
				emit(map[string]any{"type": "agent_settled", "aborted": false})
			}
		case "clear_queue":
			response(id, typ, map[string]any{"steering": []string{}, "followUp": []string{}})
		case "abort":
			emit(map[string]any{"type": "agent_settled", "aborted": true})
			response(id, typ, nil)
		case "extension_ui_response":
			emit(map[string]any{"type": "entry_appended", "entry": req})
		}
	}
	if os.Getenv("QM_RPC_HOLD_EOF") == "1" {
		time.Sleep(300 * time.Millisecond)
	}
	os.Exit(0)
}
func TestRPCSupervisorProcess(t *testing.T) {
	if os.Getenv("QM_RPC_SUPERVISOR") != "1" {
		return
	}
	err := rpcServe(Config{}, "b-1")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func rpcTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "qm-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
func waitRPC(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	log, _ := os.ReadFile(filepath.Join(stateDir(), "fixture.log"))
	t.Fatalf("timed out waiting for RPC fixture: %s", log)
}
func fixtureProcess(t *testing.T, observe func([]byte, rpcRecord) error) *rpcProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRPCChildProcess$")
	cmd.Env = append(os.Environ(), "QM_RPC_CHILD=1")
	p, err := startRPC(cmd, nil, io.Discard, observe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.stop(); <-p.exited })
	return p
}
func TestRPCFraming(t *testing.T) {
	input := "{\"type\":\"a\",\"text\":\"a\u2028b\u2029c\"}\r\n{\"type\":\"b\"}\n"
	var got [][]byte
	err := readRPC(&oneByteReader{r: strings.NewReader(input)}, func(b []byte) error { got = append(got, bytes.Clone(b)); return nil })
	if !errors.Is(err, io.EOF) || len(got) != 2 || !bytes.Contains(got[0], []byte("\u2028")) {
		t.Fatalf("framing: %q %v", got, err)
	}
	for _, bad := range []string{"{\"type\":\"partial\"}", "not json\n", "[]\n", strings.Repeat("x", rpcMaxRecord+1)} {
		if err := readRPC(strings.NewReader(bad), func([]byte) error { return nil }); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("accepted %q", bad[:min(len(bad), 60)])
		}
	}
}

type oneByteReader struct{ r io.Reader }

func (r *oneByteReader) Read(b []byte) (int, error) { return r.r.Read(b[:1]) }
func TestRPCRequestLifecycle(t *testing.T) {
	var mu sync.Mutex
	events := []string{}
	p := fixtureProcess(t, func(_ []byte, r rpcRecord) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, r.Type)
		return nil
	})
	_, err := p.call("slow", "prompt", map[string]any{"message": "delay"}, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "delivery unknown") {
		t.Fatalf("timeout = %v", err)
	}
	// Another response arrives before the late response; correlation is by id.
	r, err := p.call("state", "get_state", nil, time.Second)
	if err != nil || r.ID != "state" {
		t.Fatalf("state: %+v %v", r, err)
	}
	waitRPC(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(events) >= 2 })
	if _, err = p.call("hold", "prompt", map[string]any{"message": "hold"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err = p.call("abort", "abort", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined := strings.Join(events, ",")
	mu.Unlock()
	if !strings.Contains(joined, "agent_end,agent_settled") {
		t.Fatalf("missing lifecycle: %s", joined)
	}
}
func TestRPCChildFailureAndEOF(t *testing.T) {
	p := fixtureProcess(t, nil)
	_, err := p.call("crash", "prompt", map[string]any{"message": "crash"}, time.Second)
	if err == nil {
		t.Fatal("crash succeeded")
	}
	select {
	case <-p.exited:
	case <-time.After(time.Second):
		t.Fatal("not reaped")
	}
	cmd := exec.Command("sh", "-c", "printf 'not-json\\n'; exec sleep 30")
	q, err := startRPC(cmd, nil, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.stop()
	select {
	case <-q.done:
	case <-time.After(time.Second):
		t.Fatal("malformed stdout not detected")
	}
	// Test cleanup kills only this fixture's direct process.
	_ = cmd.Process.Kill()
	<-q.exited
}
func fixtureSupervisor(t *testing.T) func() *exec.Cmd {
	t.Helper()
	dir := rpcTestDir(t)
	t.Setenv("QUARTERMASTER_STATE_DIR", dir)
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	exe, _ := filepath.Abs(os.Args[0])
	script := "#!/bin/sh\nexport QM_RPC_CHILD=1\nexec " + shellQuote(exe) + " -test.run=^TestRPCChildProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// Unsupported dialogs update a fixture bd, never the real database.
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$QUARTERMASTER_STATE_DIR/bd.log\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	lease, err := rpcLease("b-1")
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err = rpcSave(filepath.Join(rpcDir("b-1"), "worker.json"), rpcWorker{Bead: "b-1", Cwd: dir, Args: []string{"--mode", "rpc"}}); err != nil {
		t.Fatal(err)
	}
	return func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRPCSupervisorProcess$")
		cmd.Env = append(os.Environ(), "QM_RPC_SUPERVISOR=1")
		log, err := os.OpenFile(filepath.Join(dir, "fixture.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = rpcClient("b-1", rpcRequest{Op: "stop"})
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			log.Close()
		})
		waitRPC(t, func() bool { _, err := rpcClient("b-1", rpcRequest{Op: "status"}); return err == nil })
		return cmd
	}
}
func TestRPCSupervisorReconnectResumeAndOwnership(t *testing.T) {
	launch := fixtureSupervisor(t)
	cmd := launch()
	first, err := rpcClient("b-1", rpcRequest{Op: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status.SessionID != "actual-pi-id" || !strings.HasSuffix(first.Status.SessionFile, "actual-session.jsonl") {
		t.Fatalf("invented identity: %+v", first)
	}
	if lease, err := rpcLease("b-1"); err == nil {
		lease.Close()
		t.Fatal("second owner acquired lease")
	}
	for _, path := range []string{rpcDir("b-1"), rpcSocket("b-1"), filepath.Join(rpcDir("b-1"), "worker.json"), filepath.Join(rpcDir("b-1"), "events.jsonl"), filepath.Join(rpcDir("b-1"), "stderr.log")} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if fi.IsDir() {
			want = 0700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %o", path, fi.Mode().Perm())
		}
	}
	// Send, disconnect without reading, reconnect and reconcile using same id.
	conn, err := net.Dial("unix", rpcSocket("b-1"))
	if err != nil {
		t.Fatal(err)
	}
	req := rpcRequest{Op: "prompt", ID: "once", Text: "hold"}
	_ = json.NewEncoder(conn).Encode(req)
	conn.Close()
	waitRPC(t, func() bool {
		r, err := rpcClient("b-1", rpcRequest{Op: "receipt", ID: "once"})
		return err == nil && r.Receipt.State == "accepted"
	})
	r, err := rpcClient("b-1", req)
	if err != nil || r.Receipt.State != "accepted" {
		t.Fatalf("reconcile: %+v %v", r, err)
	}
	st, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	if st.Status.Phase != "working" {
		t.Fatalf("agent_end marked idle: %+v", st.Status)
	}
	req.Text = "different"
	if _, err = rpcClient("b-1", req); err == nil {
		t.Fatal("reused id with different payload")
	}
	if _, err = rpcClient("b-1", rpcRequest{Op: "cancel"}); err != nil {
		t.Fatal(err)
	}
	waitRPC(t, func() bool {
		r, _ := rpcClient("b-1", rpcRequest{Op: "status"})
		return r.Status != nil && r.Status.Phase == "idle"
	})
	var output bytes.Buffer
	if err := rpcCmd(Config{}, []string{"output", "b-1"}, "", &output); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "hello") != 1 {
		t.Fatalf("duplicate execution: %s", output.String())
	}
	_, _ = rpcClient("b-1", rpcRequest{Op: "stop"})
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QM_RPC_HOLD_EOF", "1")
	cmd = launch()
	after, _ := rpcClient("b-1", rpcRequest{Op: "status"})
	if after.Status.SessionFile != first.Status.SessionFile || after.Status.PID == first.Status.PID {
		t.Fatalf("not saved-session resume: %+v", after)
	}
	req.Text = "hold"
	if _, err = rpcClient("b-1", req); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	_ = rpcCmd(Config{}, []string{"output", "b-1"}, "", &output)
	if strings.Count(output.String(), "hello") != 1 {
		t.Fatal("replayed prompt after resume")
	}
	// An orphaned pi inherits the lease even when the supervisor is SIGKILLed.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if lease, err := rpcLease("b-1"); err == nil {
		lease.Close()
		t.Fatal("orphaned pi lost its inherited lease")
	}
	waitRPC(t, func() bool {
		f, err := rpcLease("b-1")
		if err == nil {
			f.Close()
			return true
		}
		return false
	})
	// Resume after supervisor death too, including when init has not yet
	// reaped the old child. A zombie cannot write the saved session.
	t.Setenv("QM_RPC_HOLD_EOF", "")
	launch()
	if _, err = dispatch(Config{}, Herdr{}, "b-1", DispatchOpts{Backend: "herdr"}); err == nil {
		t.Fatal("TUI fallback allowed")
	}
}
func TestRPCUnknownReceiptAndDialogFailClosed(t *testing.T) {
	launch := fixtureSupervisor(t)
	launch()
	path := filepath.Join(rpcDir("b-1"), "requests", "unknown.json")
	req := rpcRequest{Op: "prompt", ID: "unknown", Text: "hold"}
	// Simulate a crash after intent but before response. Retry must reconcile.
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(req.Text)))
	if err := rpcSave(path, rpcReceipt{ID: req.ID, Hash: sum, State: "intent"}); err != nil {
		t.Fatal(err)
	}
	r, err := rpcClient("b-1", req)
	if err != nil || r.Receipt.State != "intent" {
		t.Fatalf("unknown: %+v %v", r, err)
	}
	_, err = rpcClient("b-1", rpcRequest{Op: "prompt", ID: "dialog", Text: "dialog"})
	if err != nil {
		t.Fatal(err)
	}
	waitRPC(t, func() bool {
		r, _ := rpcClient("b-1", rpcRequest{Op: "status"})
		return r.Status != nil && r.Status.Phase == "blocked"
	})
	waitRPC(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(stateDir(), "bd.log"))
		return bytes.Contains(b, []byte("needs_me"))
	})
	if _, err = rpcClient("b-1", rpcRequest{Op: "prompt", ID: "next", Text: "hold"}); err == nil {
		t.Fatal("blocked worker accepted more work")
	}
	events, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "events.jsonl"))
	if !bytes.Contains(events, []byte(`"cancelled":true`)) || bytes.Contains(events, []byte(`"confirmed":true`)) {
		t.Fatalf("dialog response: %s", events)
	}
}
