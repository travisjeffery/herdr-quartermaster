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
		if slices.Contains(args, "--list-models") {
			return []byte(listing), 0, nil
		}
		return nil, authCode, nil
	}
}

func TestCheckPiModel(t *testing.T) {
	cfg := defaultConfig()
	var calls []string
	if err := cfg.checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", "meta/muse-wonder-full-1.0"}, "worker"); err != nil {
		t.Fatalf("a listed model failed: %v", err)
	}
	if err := cfg.checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", "muse-wonder-full-1.0"}, "worker"); err != nil {
		t.Fatalf("a listed model without a provider failed: %v", err)
	}
	if err := cfg.checkPiModel(fakePi("", 0, &calls), []string{"--approve"}, "worker"); err != nil {
		t.Fatalf("no --model is pi's default and isn't checked: %v", err)
	}

	err := cfg.checkPiModel(fakePi(piListing, 1, &calls), []string{"--model", piWorkerModel}, "worker")
	if err == nil || !strings.Contains(err.Error(), "no credentials for anthropic") || !strings.Contains(err.Error(), "pi auth login anthropic") {
		t.Fatalf("missing sign-in: %v", err)
	}
	err = cfg.checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", piCoordinatorModel}, "coordinator")
	if err == nil || !strings.Contains(err.Error(), "no model anthropic/claude-fable-5-2") || !strings.Contains(err.Error(), "coordinator_args") {
		t.Fatalf("a model pi doesn't know, with credentials: %v", err)
	}
	// Another provider's model of the same id doesn't count.
	err = cfg.checkPiModel(fakePi(piListing, 0, &calls), []string{"--model", "other/muse-wonder-full-1.0"}, "worker")
	if err == nil {
		t.Fatal("matched a model under the wrong provider")
	}
	calls = nil
	if err := cfg.checkPiModel(fakePi(piListing, 0, &calls), []string{"-e", "/x/provider.ts", "--model", "meta/muse-wonder-full-1.0"}, "worker"); err != nil || calls[0] != "-e /x/provider.ts --list-models muse-wonder-full-1.0" {
		t.Errorf("an extension's models aren't listed: %q, %v", calls, err)
	}
	for _, c := range calls {
		if strings.Contains(c, "print-") {
			t.Fatalf("read a credential: %q", c)
		}
	}
}

const piAutoListing = `provider       model                 context  max-out  thinking  images
quartermaster  worker-auto           272K     65.5K    yes       yes
quartermaster  coordinator-auto      272K     65.5K    yes       yes
antigravity    gemini-4-neon-xhigh   1M       65.5K    no        yes
anthropic      claude-opus-5-5       1M       128K     yes       yes
anthropic      claude-fable-5-2      1M       128K     yes       yes
openai-codex   gpt-6.2-astra         272K     128K     yes       yes
`

func TestCheckPiVirtualModel(t *testing.T) {
	cfg := defaultConfig()
	var calls []string
	auto := []string{"--model", piWorkerVirtualModel, "-e", "/x/quartermaster.ts"}
	if err := cfg.checkPiModel(fakePi(piAutoListing, 0, &calls), auto, "worker"); err != nil {
		t.Fatalf("virtual model with full chain listed: %v", err)
	}
	if err := cfg.checkPiModel(fakePi(piAutoListing, 0, &calls), []string{"--model", piCoordinatorVirtualModel, "-e", "/x/qm.ts"}, "coordinator"); err != nil {
		t.Fatalf("coordinator virtual model: %v", err)
	}
	// Subset: Claude + OpenAI without Antigravity bridge still passes.
	noAgy := strings.ReplaceAll(piAutoListing, "antigravity    gemini-4-neon-xhigh   1M       65.5K    no        yes\n", "")
	if err := cfg.checkPiModel(fakePi(noAgy, 0, &calls), auto, "worker"); err != nil {
		t.Fatalf("virtual model without Antigravity bridge: %v", err)
	}
	// Subset: Antigravity alone (when Claude and OpenAI are unavailable) still passes.
	onlyAgy := "provider       model                 context  max-out  thinking  images\nquartermaster  worker-auto           272K     65.5K    yes       yes\nantigravity    gemini-4-neon-xhigh   1M       65.5K    no        yes\n"
	if err := cfg.checkPiModel(fakePi(onlyAgy, 1, &calls), auto, "worker"); err != nil {
		t.Fatalf("virtual model with only Antigravity listed: %v", err)
	}
	// No extension on the command line: a remote start, where the virtual
	// model is never registered.
	err := cfg.checkPiModel(fakePi(piAutoListing, 0, &calls), []string{"--model", piWorkerVirtualModel}, "worker")
	if err == nil || !strings.Contains(err.Error(), "only a pi start on this machine loads") {
		t.Fatalf("virtual model without the extension: %v", err)
	}
	// The extension loads but pi doesn't list the virtual model (old pi).
	err = cfg.checkPiModel(fakePi(piListing, 0, &calls), auto, "worker")
	if err == nil || !strings.Contains(err.Error(), "doesn't list") {
		t.Fatalf("virtual model not listed: %v", err)
	}
	// No physical model in the chain is usable: fail closed with actionable errors.
	onlyVirtual := "provider       model             context  max-out  thinking  images\nquartermaster  worker-auto       272K     65.5K    yes       yes\n"
	err = cfg.checkPiModel(fakePi(onlyVirtual, 1, &calls), auto, "worker")
	if err == nil || !strings.Contains(err.Error(), "no usable physical model") || !strings.Contains(err.Error(), "antigravity/gemini-4-neon-xhigh") || !strings.Contains(err.Error(), "pi auth login anthropic") {
		t.Fatalf("virtual model with no usable physical model: %v", err)
	}
	// A configured chain with no usable model fails closed naming it.
	cfg.Agents = map[string]AgentConfig{"pi": {RouterWorkerModels: []string{"meta/muse-wonder-full-1.0"}}}
	err = cfg.checkPiModel(fakePi(piAutoListing, 0, &calls), auto, "worker")
	if err == nil || !strings.Contains(err.Error(), "meta/muse-wonder-full-1.0") {
		t.Fatalf("configured router chain missing: %v", err)
	}
	// Configured chain with :thinking suffixes strips :thinking for --list-models and auth check.
	calls = nil
	cfg.Agents = map[string]AgentConfig{"pi": {RouterWorkerModels: []string{"anthropic/claude-opus-5-5:high", "openai-codex/gpt-6.2-astra:xhigh"}}}
	if err := cfg.checkPiModel(fakePi(piAutoListing, 0, &calls), auto, "worker"); err != nil {
		t.Fatalf("virtual model with :thinking chain failed: %v", err)
	}
	if !slices.Contains(calls, "-e /x/quartermaster.ts --list-models claude-opus-5-5") {
		t.Errorf("expected --list-models without :thinking suffix, got calls: %q", calls)
	}
	calls = nil
	cfg.Agents = map[string]AgentConfig{"pi": {RouterWorkerModels: []string{"anthropic/claude-opus-5-5:high"}}}
	err = cfg.checkPiModel(fakePi(onlyVirtual, 1, &calls), auto, "worker")
	if err == nil || !strings.Contains(err.Error(), "pi auth login anthropic") || !slices.Contains(calls, "auth check --provider anthropic") {
		t.Fatalf("missing :thinking model auth check: err=%v calls=%q", err, calls)
	}
}

func TestPiDefaults(t *testing.T) {
	cfg := defaultConfig()
	if got := strings.Join(cfg.agentArgs("pi", false), " "); got != "--approve --model quartermaster/worker-auto" {
		t.Errorf("worker args = %q", got)
	}
	if got := strings.Join(cfg.coordinatorArgs("pi"), " "); got != "--approve --model quartermaster/coordinator-auto" {
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

func TestPiLaunch(t *testing.T) {
	dir := fakeTools(t)
	fakePiTool(t, dir, piAutoListing, 0)
	cfg := defaultConfig()
	args, err := cfg.piLaunch(localHost, cfg.agentArgs("pi", false), "worker", "backend-ab12.1")
	if err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(stateDir(), "pi", "quartermaster.ts")
	if got, want := strings.Join(args, " "), "--approve --model quartermaster/worker-auto -e "+ext+" --quartermaster-router "+piRouterConfigPath()+" --quartermaster-control "+piControlSocket("backend-ab12-1")+" --session-id backend-ab12.1"; got != want {
		t.Errorf("worker args = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(ext); err != nil || string(b) != string(piExtension) {
		t.Errorf("extension not written: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(stateDir(), "pi", "router.ts")); err != nil || string(b) != string(piRouterExtension) {
		t.Errorf("router extension not written: %v", err)
	}
	if _, err := os.Stat(piRouterConfigPath()); err != nil {
		t.Errorf("router config not written: %v", err)
	}
	if _, err := os.Stat(piQuotaSnapshotPath()); err != nil {
		t.Errorf("quota snapshot not written: %v", err)
	}
	// The control configuration starts asking about nothing.
	if b, err := os.ReadFile(filepath.Join(piControlDir(), "config.json")); err != nil || !strings.Contains(string(b), `"ask": []`) {
		t.Errorf("control config = %s, %v", b, err)
	}
	if fi, err := os.Stat(piControlDir()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("control dir mode: %v %v", fi, err)
	}
	// The coordinator gets a separate inbox channel, not worker prompt access.
	// A remote worker gets neither file nor -e.
	fakePiTool(t, dir, piAutoListing, 0)
	args, err = cfg.piLaunch(localHost, cfg.coordinatorArgs("pi"), "coordinator", "")
	if err != nil || slices.Contains(args, "--session-id") || slices.Contains(args, "--quartermaster-control") || !slices.Contains(args, "--quartermaster-coordinator") || !slices.Contains(args, piCoordinatorSocket()) || !slices.Contains(args, ext) || !slices.Contains(args, "--quartermaster-router") {
		t.Errorf("coordinator args = %q, %v", args, err)
	}
}

// fakePiTool puts a pi on PATH that lists listing and logs its calls.
func fakePiTool(t *testing.T, dir, listing string, authCode int) {
	t.Helper()
	t.Setenv("FAKE_PI_LIST", listing)
	body := "#!/bin/sh\necho \"$*\" >> \"$LOG/pi.log\"\ncase \"$*\" in *--list-models*) printf '%s' \"$FAKE_PI_LIST\" ;; \"auth check\"*) exit " + string(rune('0'+authCode)) + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchPi(t *testing.T) {
	dir := fakeTools(t)
	fakePiTool(t, dir, piAutoListing, 0)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	if _, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{Kind: "pi"}); err != nil {
		t.Fatal(err)
	}
	if !hasLine(logLines(t, dir, "herdr"), "agent start b-1 --kind pi --pane w9:p1 --timeout 30000 -- --approve --model quartermaster/worker-auto -e "+filepath.Join(stateDir(), "pi", "quartermaster.ts")+" --quartermaster-router "+piRouterConfigPath()+" --quartermaster-control "+piControlSocket("b-1")+" --session-id b-1") {
		t.Errorf("pi not started on the bead's session: %q", logLines(t, dir, "herdr"))
	}
}

func TestDispatchPiWithoutModelLeavesNothing(t *testing.T) {
	dir := fakeTools(t)
	fakePiTool(t, dir, "provider model\nquartermaster worker-auto\n", 1)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	_, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{Kind: "pi"})
	if err == nil || !strings.Contains(err.Error(), "no usable physical model") || !strings.Contains(err.Error(), "pi auth login anthropic") {
		t.Fatalf("got %v", err)
	}
	if hasLine(logLines(t, dir, "herdr"), "worktree create") || hasLine(logLines(t, dir, "bd"), "--claim") {
		t.Error("a failed model check still created a worktree or claimed the bead")
	}
}
