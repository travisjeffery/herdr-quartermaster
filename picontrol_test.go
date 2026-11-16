package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// harness runs the real extension (pi/quartermaster.ts) under node with a
// fake pi (testdata/pi-harness.mjs), listening on the control socket of the
// worker named name.
type harness struct {
	t     *testing.T
	stdin io.WriteCloser
	lines chan map[string]any
}

func startHarness(t *testing.T, name string) *harness {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node")
	}
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	// The guard hook: deny a command naming "deny", ask about the rest.
	hook := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(hook, []byte(`#!/bin/sh
if grep -q deny; then
  echo '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"never this"}}'
else
  echo '{"hookSpecificOutput":{"permissionDecision":"ask","permissionDecisionReason":"force push rewrites history"}}'
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUARTERMASTER_PI_BASH_HOOKS", hook)
	ext, err := filepath.Abs("pi/quartermaster.ts")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "testdata/pi-harness.mjs", ext, piControlSocket(name))
	cmd.Stderr = io.Discard
	h := &harness{t: t, lines: make(chan map[string]any, 100)}
	h.stdin, _ = cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Skipf("node: %v", err)
	}
	t.Cleanup(func() { h.stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				h.lines <- m
			}
		}
		close(h.lines)
	}()
	h.next(func(m map[string]any) bool { return m["ready"] == true })
	return h
}

func (h *harness) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stdin, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

// next waits for the harness line match accepts, returning the lines before it too.
func (h *harness) next(match func(map[string]any) bool) (map[string]any, []map[string]any) {
	h.t.Helper()
	var seen []map[string]any
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-h.lines:
			if !ok {
				h.t.Fatalf("harness exited; saw %v", seen)
			}
			if match(m) {
				return m, seen
			}
			seen = append(seen, m)
		case <-timeout:
			h.t.Fatalf("timed out; saw %v", seen)
		}
	}
}

func writeAskConfig(t *testing.T, ask []string, timeout float64) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"ask": ask, "ask_timeout_seconds": timeout})
	if err := os.WriteFile(filepath.Join(piControlDir(), "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPiControlSocketAndToken(t *testing.T) {
	startHarness(t, "b-1")
	for _, f := range []string{piControlDir(), piControlSocket("b-1"), piControlSocket("b-1") + ".token"} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if fi.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", f, fi.Mode().Perm(), want)
		}
	}
	c, ok := piControlFor("b-1")
	if !ok {
		t.Fatal("no control channel")
	}
	bad := c
	bad.token = strings.Repeat("0", len(c.token))
	if _, err := bad.Ask(); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("wrong token: err = %v", err)
	}
	if _, ok := piControlFor("b-2"); ok {
		t.Error("another worker's channel")
	}
}

// A prompt over the socket is a user message: queued as a follow-up while a
// turn runs, sent at once when idle, and recorded as from quartermaster.
func TestPiControlPrompt(t *testing.T) {
	h := startHarness(t, "b-1")
	c, _ := piControlFor("b-1")
	h.send("idle false")
	// Let the harness take the line before the prompt arrives.
	time.Sleep(50 * time.Millisecond)
	queued, err := c.Prompt("PR #3 has failing checks")
	if err != nil || !queued {
		t.Fatalf("busy prompt: queued %v, err %v", queued, err)
	}
	m, seen := h.next(func(m map[string]any) bool { return m["user"] != nil })
	if m["deliverAs"] != "followUp" {
		t.Errorf("delivered as %v", m["deliverAs"])
	}
	if !hasEntry(seen, "quartermaster.input", "source", "quartermaster") {
		t.Errorf("source not recorded: %v", seen)
	}
	h.send("idle true")
	time.Sleep(50 * time.Millisecond)
	if queued, err := c.Prompt("carry on"); err != nil || queued {
		t.Fatalf("idle prompt: queued %v, err %v", queued, err)
	}
	if m, _ := h.next(func(m map[string]any) bool { return m["user"] != nil }); m["deliverAs"] != nil {
		t.Errorf("idle prompt delivered as %v", m["deliverAs"])
	}
	if _, err := c.Prompt("  "); err == nil {
		t.Error("an empty prompt was taken")
	}
}

func hasEntry(lines []map[string]any, entry, key, val string) bool {
	for _, m := range lines {
		if d, ok := m["data"].(map[string]any); ok && m["entry"] == entry && d[key] == val {
			return true
		}
	}
	return false
}

// A guarded command on the ask list waits for quartermaster's answer; one
// off it, or one a hook denies, blocks as before; no answer in time blocks.
func TestPiControlAsk(t *testing.T) {
	h := startHarness(t, "b-1")
	c, _ := piControlFor("b-1")
	if ask, err := c.Ask(); err != nil || ask != nil {
		t.Fatalf("ask before any: %v, %v", ask, err)
	}

	// The list starts empty: the ask blocks at once, with the hand-off.
	h.send("tool git push --force")
	m, _ := h.next(func(m map[string]any) bool { return m["tool"] != nil })
	if m["block"] != true || !strings.Contains(m["reason"].(string), "RUN: note") {
		t.Fatalf("empty ask list: %v", m)
	}

	writeAskConfig(t, []string{`^git push --force`}, 30)
	h.send("tool git push --force origin x")
	var ask *piAsk
	for deadline := time.Now().Add(5 * time.Second); ask == nil && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		ask, _ = c.Ask()
	}
	if ask == nil || ask.Command != "git push --force origin x" || ask.Reason != "force push rewrites history" || len(ask.Options) != 2 {
		t.Fatalf("ask = %+v", ask)
	}
	if err := c.Answer("nope", 1, "TJ"); err == nil {
		t.Error("answered an ask that isn't waiting")
	}
	if err := c.Answer(ask.ID, 3, "TJ"); err == nil {
		t.Error("answered with no such option")
	}
	if err := c.Answer(ask.ID, 1, "TJ"); err != nil {
		t.Fatal(err)
	}
	m, seen := h.next(func(m map[string]any) bool { return m["tool"] != nil })
	if m["block"] != false {
		t.Errorf("yes still blocked: %v", m)
	}
	if !hasEntry(seen, "quartermaster.ask", "id", ask.ID) || !hasEntry(seen, "quartermaster.answer", "by", "TJ") {
		t.Errorf("session entries: %v", seen)
	}
	if a, _ := c.Ask(); a != nil {
		t.Errorf("answered ask still waiting: %+v", a)
	}

	// No.
	h.send("tool git push --force again")
	for ask = nil; ask == nil; ask, _ = c.Ask() {
		time.Sleep(20 * time.Millisecond)
	}
	if err := c.Answer(ask.ID, 2, "coordinator"); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.next(func(m map[string]any) bool { return m["tool"] != nil }); m["block"] != true || !strings.Contains(m["reason"].(string), "Declined through quartermaster by coordinator") {
		t.Errorf("no: %v", m)
	}

	// A command off the list, and a hook's deny, block without asking.
	h.send("tool git status")
	if m, _ := h.next(func(m map[string]any) bool { return m["tool"] != nil }); m["block"] != true {
		t.Errorf("off the list: %v", m)
	}
	h.send("tool git push --force deny")
	if m, _ := h.next(func(m map[string]any) bool { return m["tool"] != nil }); m["block"] != true || !strings.Contains(m["reason"].(string), "never this") {
		t.Errorf("deny: %v", m)
	}

	// Nobody answers.
	writeAskConfig(t, []string{`^git push --force`}, 0.2)
	h.send("tool git push --force late")
	m, _ = h.next(func(m map[string]any) bool { return m["tool"] != nil })
	if m["block"] != true || !strings.Contains(m["reason"].(string), "no answer in time") || !strings.Contains(m["reason"].(string), "RUN: note") {
		t.Errorf("timeout: %v", m)
	}

	h.send("shutdown")
	h.next(func(m map[string]any) bool { return m["shutdown"] == true })
	if _, ok := piControlFor("b-1"); ok {
		t.Error("socket left behind after shutdown")
	}
}

func TestPiAskPrompt(t *testing.T) {
	a := piAsk{ID: "1-abc", Command: "git push --force", Reason: "rewrites history", Options: []string{"Yes, run it", "No, block it"}}
	p := a.prompt()
	if p.Summary() != "pi guard asks: Bash command: git push --force" || p.Choices() != "1 Yes, run it · 2 No, block it" || !p.Numbered || p.askID != "1-abc" {
		t.Errorf("prompt = %+v", p)
	}
	b := a
	b.ID = "2-def"
	if b.prompt().Hash == p.Hash {
		t.Error("the same command asked twice hashes the same")
	}
	if a.prompt().Hash != p.Hash {
		t.Error("hash not stable")
	}
}

// quartermaster prompt and answer reach a pi worker's ask over its socket,
// with the same hash and autonomy checks as a prompt on screen.
func TestAnswerPiAsk(t *testing.T) {
	ask := &piAsk{ID: "1-abc", Command: "git push --force", Reason: "r", Options: []string{"Yes, run it", "No, block it"}}
	var answered []string
	f := &fakePane{screens: []string{"nothing on screen"}, level: 4}
	env := f.env()
	env.piAsk = func(host string, a *Agent) (*piAsk, bool, error) { return ask, true, nil }
	env.piAnswer = func(a *Agent, id string, choice int, by string) error {
		answered = append(answered, a.Name+" "+id+" "+string(rune('0'+choice))+" "+by)
		ask = nil
		return nil
	}
	p := ask.prompt()
	out, err := showPrompt(env, "b-1")
	if err != nil || !strings.Contains(out, "["+p.Hash+"]") || !strings.Contains(out, "git push --force") {
		t.Fatalf("show: %q, %v", out, err)
	}
	if _, err := answerPrompt(env, "b-1", "1", "deadbeef", "coordinator", 2); err == nil || !strings.Contains(err.Error(), "the prompt changed") {
		t.Errorf("wrong hash: %v", err)
	}
	if _, err := answerPrompt(env, "b-1", "1", p.Hash, "coordinator", 5); !errors.Is(err, errNotAutonomous) {
		t.Errorf("above level: %v", err)
	}
	if len(answered) != 0 || len(f.sent) != 0 {
		t.Fatalf("answered %q, keys %q", answered, f.sent)
	}
	if _, err := answerPrompt(env, "b-1", "yes", p.Hash, "coordinator", 2); err != nil {
		t.Fatal(err)
	}
	if len(answered) != 1 || answered[0] != "b-1 1-abc 1 coordinator" || len(f.sent) != 0 {
		t.Errorf("answered %q, keys %q", answered, f.sent)
	}
	if len(f.notes) != 1 || !strings.HasPrefix(f.notes[0], "AUTO: [coordinator L4 C2] PROMPT ANSWERED: 1 Yes, run it to pi guard asks: Bash command: git push --force ["+p.Hash+"]") {
		t.Errorf("notes = %q", f.notes)
	}
	// Nothing waiting: no prompt, and the screen isn't read instead.
	if _, err := showPrompt(env, "b-1"); !errors.Is(err, errNoPrompt) {
		t.Errorf("after: %v", err)
	}
}

// Without a control socket a pi worker is prompted through its pane.
func TestPromptPiAgentFallsBack(t *testing.T) {
	dir := fakeTools(t)
	a := Agent{Name: "b-1", PaneID: "w1:p1", Session: AgentSession{Agent: "pi"}}
	via, err := promptPiAgent(newHerdr(), a, "hello")
	if err != nil || via {
		t.Fatalf("via socket %v, err %v", via, err)
	}
	if !hasLine(logLines(t, dir, "herdr"), "agent prompt w1:p1 hello") {
		t.Errorf("herdr calls: %q", logLines(t, dir, "herdr"))
	}
}

func TestValidatePiAsk(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agents = map[string]AgentConfig{"pi": {Ask: []string{`^git push --force`}, AskTimeout: "5m"}}
	if err := cfg.validateAgents(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]AgentConfig{
		{"pi": {Ask: []string{`(`}}},
		{"pi": {AskTimeout: "soon"}},
		{"pi": {AskTimeout: "-1m"}},
		{"claude": {Ask: []string{"x"}}},
	} {
		cfg.Agents = bad
		if err := cfg.validateAgents(); err == nil {
			t.Errorf("%v passed", bad)
		}
	}
}

func TestWritePiControlConfig(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := defaultConfig()
	cfg.Agents = map[string]AgentConfig{"pi": {Ask: []string{`^git push --force`}, AskTimeout: "90s"}}
	if err := cfg.writePiControlConfig(); err != nil {
		t.Fatal(err)
	}
	var got piControlConfig
	b, _ := os.ReadFile(filepath.Join(piControlDir(), "config.json"))
	if err := json.Unmarshal(b, &got); err != nil || len(got.Ask) != 1 || got.AskTimeout != 90 {
		t.Errorf("config = %s, %v", b, err)
	}
}

// A socket nothing listens on, as a killed pi leaves, reads as no socket.
func TestPiAskOfStaleSocket(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	if err := os.MkdirAll(piControlDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", piControlSocket("b-1"))
	if err != nil {
		t.Fatal(err)
	}
	// Closing a unix listener removes its file; keep it, as SIGKILL would.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := os.WriteFile(piControlSocket("b-1")+".token", []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Name: "b-1", Session: AgentSession{Agent: "pi"}}
	if _, ok := piControlFor("b-1"); !ok {
		t.Fatal("stale socket not found at all")
	}
	if ask, ok, err := piAskOf(localHost, a); ok || ask != nil || err != nil {
		t.Errorf("stale socket: %v %v %v", ask, ok, err)
	}
	// A remote agent of the same name never reads the local socket.
	a.Host = "server"
	if _, ok, _ := piAskOf(localHost, a); ok {
		t.Error("remote agent read the local socket")
	}
}
