package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPiModel(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--approve", "--model", "anthropic/claude-opus-5-5"}, "anthropic/claude-opus-5-5"},
		{[]string{"--model=anthropic/claude-fable-5-2:high"}, "anthropic/claude-fable-5-2"},
		{[]string{"--model", "sonnet:high"}, "sonnet"},
		{[]string{"--approve"}, ""},
		{[]string{"--model"}, ""},
	} {
		if got := piModel(c.args); got != c.want {
			t.Errorf("piModel(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

const piListing = `provider  model                 context  max-out  thinking  images
meta      muse-wonder-full-1.0  1M       131.1K   yes       yes
`

// fakePi answers --list-models with listing and auth check with code.
func fakePi(listing string, authCode int, calls *[]string) piRunner {
	return func(args ...string) ([]byte, int, error) {
		*calls = append(*calls, strings.Join(args, " "))
		if args[0] == "--list-models" {
			return []byte(listing), 0, nil
		}
		return nil, authCode, nil
	}
}

func TestCheckPiModel(t *testing.T) {
	var calls []string
	if err := checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", "meta/muse-wonder-full-1.0"}, "worker"); err != nil {
		t.Fatalf("a listed model failed: %v", err)
	}
	if err := checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", "muse-wonder-full-1.0"}, "worker"); err != nil {
		t.Fatalf("a listed model without a provider failed: %v", err)
	}
	if err := checkPiModel(fakePi("", 0, &calls), []string{"--approve"}, "worker"); err != nil {
		t.Fatalf("no --model is pi's default and isn't checked: %v", err)
	}

	err := checkPiModel(fakePi(piListing, 1, &calls), []string{"--model", piWorkerModel}, "worker")
	if err == nil || !strings.Contains(err.Error(), "no credentials for anthropic") || !strings.Contains(err.Error(), "pi auth login anthropic") {
		t.Fatalf("missing sign-in: %v", err)
	}
	err = checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", piCoordinatorModel}, "coordinator")
	if err == nil || !strings.Contains(err.Error(), "no model anthropic/claude-fable-5-2") || !strings.Contains(err.Error(), "coordinator_args") {
		t.Fatalf("a model pi doesn't know, with credentials: %v", err)
	}
	// Another provider's model of the same id doesn't count.
	err = checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", "other/muse-wonder-full-1.0"}, "worker")
	if err == nil {
		t.Fatal("matched a model under the wrong provider")
	}
	for _, c := range calls {
		if strings.Contains(c, "print-") {
			t.Fatalf("read a credential: %q", c)
		}
	}
}

func TestPiDefaults(t *testing.T) {
	cfg := defaultConfig()
	if got := strings.Join(cfg.agentArgs("pi", false), " "); got != "--approve --model anthropic/claude-opus-5-5" {
		t.Errorf("worker args = %q", got)
	}
	if got := strings.Join(cfg.coordinatorArgs("pi"), " "); got != "--approve --model anthropic/claude-fable-5-2" {
		t.Errorf("coordinator args = %q", got)
	}
	if got := cfg.agentArgs("pi", true); slices.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("pi got another kind's bypass args: %q", got)
	}
	// A configured table replaces both; without coordinator_args the
	// coordinator runs what workers do.
	cfg.Agents = map[string]AgentConfig{"pi": {Args: []string{"--model", "openai/gpt-5.2"}}}
	if got := strings.Join(cfg.coordinatorArgs("pi"), " "); got != "--model openai/gpt-5.2" {
		t.Errorf("configured coordinator args = %q", got)
	}
	cfg.Agents["pi"] = AgentConfig{Args: []string{"--model", "a/w"}, CoordinatorArgs: []string{"--model", "a/c"}}
	if got := strings.Join(cfg.coordinatorArgs("pi"), " "); got != "--model a/c" {
		t.Errorf("coordinator_args = %q", got)
	}
	// Other kinds keep theirs.
	if got := defaultConfig().coordinatorArgs("claude"); len(got) != 0 {
		t.Errorf("claude coordinator args = %q", got)
	}
	if defaultConfig().WorkerAgent != "claude" || defaultConfig().CoordinatorAgent != "claude" {
		t.Error("pi became a default")
	}
}

func TestPiResumesItsBeadSession(t *testing.T) {
	if got := strings.Join(resumeArgsFor("pi", "backend-ab12.1"), " "); got != "--session-id backend-ab12.1" {
		t.Errorf("pi resume args = %q", got)
	}
	if got := strings.Join(resumeArgsFor("claude", "x"), " "); got != "--continue" {
		t.Errorf("claude resume args = %q", got)
	}
}

// fakePiTool puts a pi on PATH that lists listing and logs its calls.
func fakePiTool(t *testing.T, dir, listing string, authCode int) {
	t.Helper()
	t.Setenv("FAKE_PI_LIST", listing)
	body := "#!/bin/sh\necho \"$*\" >> \"$LOG/pi.log\"\ncase \"$1\" in --list-models) printf '%s' \"$FAKE_PI_LIST\" ;; auth) exit " + string(rune('0'+authCode)) + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchPi(t *testing.T) {
	dir := fakeTools(t)
	fakePiTool(t, dir, "provider model\nanthropic claude-opus-5-5\n", 0)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	if _, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{Kind: "pi"}); err != nil {
		t.Fatal(err)
	}
	if !hasLine(logLines(t, dir, "herdr"), "agent start b-1 --kind pi --pane w9:p1 --timeout 30000 -- --approve --model anthropic/claude-opus-5-5 --session-id b-1") {
		t.Errorf("pi not started on the bead's session: %q", logLines(t, dir, "herdr"))
	}
}

func TestDispatchPiWithoutModelLeavesNothing(t *testing.T) {
	dir := fakeTools(t)
	fakePiTool(t, dir, "provider model\n", 1)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	_, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{Kind: "pi"})
	if err == nil || !strings.Contains(err.Error(), "pi auth login anthropic") {
		t.Fatalf("got %v", err)
	}
	if hasLine(logLines(t, dir, "herdr"), "worktree create") || hasLine(logLines(t, dir, "bd"), "--claim") {
		t.Error("a failed model check still created a worktree or claimed the bead")
	}
}
