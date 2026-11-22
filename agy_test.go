package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAgyResumesItsLastConversation(t *testing.T) {
	if !validKinds["agy"] {
		t.Fatal("agy is not a valid kind")
	}
	if got := strings.Join(resumeArgs["agy"], " "); got != "--continue" {
		t.Fatalf("agy resume args = %q", got)
	}
	if k := resumeKind(resumeCfg, Bead{Assignee: "agy"}, ""); k != "agy" {
		t.Fatalf("an agy-claimed bead resumes as %q", k)
	}
	// Resume starts agy with its default mode, then --continue, in the
	// worktree's pane.
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argv + "\necho '{\"result\":{}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	args := cfg.agentArgs("agy", false, resumeArgs["agy"]...)
	if err := (Herdr{bin: bin}).AgentStart("b-1", "agy", "w1:p1", args...); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(argv)
	want := "agent start b-1 --kind agy --pane w1:p1 --timeout 30000 -- --mode auto --continue"
	if s := strings.Join(strings.Fields(string(got)), " "); s != want {
		t.Fatalf("got %q, want %q", s, want)
	}
}

func TestAgyArgs(t *testing.T) {
	var cfg Config
	if got := strings.Join(cfg.agentArgs("agy", false), " "); got != "--mode auto" {
		t.Errorf("agy default args = %q", got)
	}
	if got := strings.Join(cfg.coordinatorArgs("agy"), " "); got != "--mode auto" {
		t.Errorf("agy coordinator args = %q", got)
	}
	// Like muse and pi, agy has no bypass default: a 9 lease adds nothing
	// until [agents.agy] bypass_args names agy's switch.
	if got := strings.Join(cfg.agentArgs("agy", true), " "); got != "--mode auto" {
		t.Errorf("agy bypass args = %q", got)
	}
	cfg.Agents = map[string]AgentConfig{"agy": {Args: []string{"--mode", "auto", "--model", "gemini-4-neon-high"}, BypassArgs: []string{"--dangerously-skip-permissions"}}}
	if got := strings.Join(cfg.agentArgs("agy", false), " "); got != "--mode auto --model gemini-4-neon-high" {
		t.Errorf("configured agy args = %q", got)
	}
	if got := strings.Join(cfg.agentArgs("agy", true, resumeArgs["agy"]...), " "); got != "--mode auto --model gemini-4-neon-high --dangerously-skip-permissions --continue" {
		t.Errorf("configured agy bypass+resume args = %q", got)
	}
	if defaultConfig().WorkerAgent == "agy" || defaultConfig().CoordinatorAgent == "agy" {
		t.Error("agy became a default")
	}
}

func TestAgyEnvNamesTheKind(t *testing.T) {
	cfg := Config{BeadsDir: "/home/u/.beads"}
	if got := cfg.agentEnv("agy"); got != "export BEADS_ACTOR='agy' BEADS_DIR='/home/u/.beads'" {
		t.Errorf("agy env = %q", got)
	}
	if got := cfg.agentEnvOn("server", "agy"); got != "export BEADS_ACTOR='agy'" {
		t.Errorf("agy remote env = %q", got)
	}
}

func TestDispatchAgy(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	msg, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{Kind: "agy"})
	if err != nil {
		t.Fatal(err)
	}
	if msg != "started agy on b-1 (w9:p1, branch tj/b-1-do-it)" {
		t.Errorf("dispatch said %q", msg)
	}
	lines := logLines(t, dir, "herdr")
	if !hasLine(lines, "pane run w9:p1 export BEADS_ACTOR='agy'") {
		t.Errorf("agy pane env not exported: %q", lines)
	}
	if !hasLine(lines, "agent start b-1 --kind agy --pane w9:p1 --timeout 30000 -- --mode auto") {
		t.Errorf("agy not started interactively with its defaults: %q", lines)
	}
	if !hasLine(logLines(t, dir, "bd"), "update b-1 --claim") {
		t.Errorf("bead not claimed: %q", logLines(t, dir, "bd"))
	}
	// The brief is prompted into the interactive session, never piped to
	// agy's stdin (its print mode reads the prompt there and hangs).
	if !slices.ContainsFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "agent prompt b-1 You are the quartermaster worker for bead b-1")
	}) {
		t.Errorf("brief not prompted: %q", lines)
	}
}

func TestDispatchAgyWithGeminiModel(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	cfg := hostsConfig()
	// [agents.agy] args replace the default, exactly as TOML would load them.
	cfg.Agents = map[string]AgentConfig{"agy": {Args: []string{"--mode", "auto", "--model", "gemini-4-neon-high"}}}
	if _, err := dispatch(cfg, newHerdr(), "b-1", DispatchOpts{Kind: "agy"}); err != nil {
		t.Fatal(err)
	}
	if !hasLine(logLines(t, dir, "herdr"), "agent start b-1 --kind agy --pane w9:p1 --timeout 30000 -- --mode auto --model gemini-4-neon-high") {
		t.Errorf("agy not started on the configured model: %q", logLines(t, dir, "herdr"))
	}
}

func TestDispatchAgyRejectsPiRPC(t *testing.T) {
	fakeTools(t)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	_, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{Kind: "agy", Backend: rpcBackend})
	if err == nil || !strings.Contains(err.Error(), "pi-rpc requires --agent pi") {
		t.Fatalf("agy on the pi-rpc backend: %v", err)
	}
}

func TestBoardTStartsAgy(t *testing.T) {
	f := testFleet()
	m := testModel(t, f)
	selectBead(t, m, "bk-nx1")
	press(t, m, "t")
	if m.ask == nil || !strings.Contains(m.ask.prompt, "start agy on bk-nx1") {
		t.Fatalf("t on a bead with no agent: %+v", m.ask)
	}
	press(t, m, "y")
	if !slices.Equal(f.calls, []string{"dispatch bk-nx1 agy"}) || !m.quit {
		t.Fatalf("calls %v quit %v", f.calls, m.quit)
	}
	if help := strings.Join(helpLines(false), "\n"); !strings.Contains(help, "c x m i t start claude, codex, muse, pi or agy") {
		t.Errorf("board help does not name the agy key:\n%s", help)
	}
}

func TestContextRendersAgyWorker(t *testing.T) {
	threads := []Thread{
		{Bead: Bead{ID: "b-agy", Title: "On agy", Status: StatusInProgress, Assignee: "agy", UpdatedAt: t0},
			Agent: &Agent{Name: "b-agy", Kind: "agy", Status: "idle"}},
	}
	out := renderContext(Config{Repo: "/r"}, threads, nil, nil, nil, TickerState{}, nil, t0)
	if !strings.Contains(out, "b-agy") || !strings.Contains(out, "agy idle") {
		t.Errorf("context does not render the agy worker:\n%s", out)
	}
	// The board's agent column is the same glyphs as every other kind, and the
	// detail pane names the agent kind and status.
	if g, _ := agentGlyph(threads[0].Agent); g != "○" {
		t.Errorf("idle agy glyph = %q", g)
	}
	r := boardRow{bead: threads[0].Bead, agent: threads[0].Agent, line: stateLine(threads[0])}
	var detailText strings.Builder
	for _, l := range detailLines(r, &beadDetail{}, 80) {
		detailText.WriteString(l.text + "\n")
	}
	if !strings.Contains(detailText.String(), "Agent b-agy · agy idle") {
		t.Errorf("board detail does not render the agy worker:\n%s", detailText.String())
	}
}

func TestAgyInKindHelp(t *testing.T) {
	if !strings.Contains(usage, "claude, codex, muse, pi or agy") {
		t.Error("usage does not name agy")
	}
	guide := coordinatorGuide(Config{})
	if !strings.Contains(guide, "--agent claude|codex|muse|pi|agy") {
		t.Error("coordinator guide does not name agy")
	}
}
