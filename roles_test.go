package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuiltinRolePresetsAndOverrides(t *testing.T) {
	cfg := defaultConfig()

	wantBuiltins := []struct {
		name     string
		worktree bool
		brief    string
	}{
		{roleImplementer, true, roleImplementer},
		{roleReviewer, false, roleReviewer},
		{roleInvestigator, false, roleInvestigator},
		{roleRollout, false, roleRollout},
	}
	for _, want := range wantBuiltins {
		got, err := cfg.resolveRole(want.name)
		if err != nil {
			t.Fatalf("resolveRole(%q): %v", want.name, err)
		}
		if got.Name != want.name || got.Worktree != want.worktree || got.Brief != want.brief || got.Custom || got.Configured {
			t.Errorf("resolveRole(%q) = %+v, want worktree=%v brief=%q custom=false configured=false", want.name, got, want.worktree, want.brief)
		}
	}

	// Empty role name defaults to implementer.
	def, err := cfg.resolveRole("")
	if err != nil || def.Name != roleImplementer || !def.Worktree || def.Brief != roleImplementer {
		t.Fatalf("resolveRole(\"\") = %+v, %v", def, err)
	}

	// User overrides on a built-in preset plus a custom role loaded via TOML.
	cfgDir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	conf := `
repo = "/r"

[roles.reviewer]
agent = "pi"
model = "anthropic/claude-opus-5-5"
thinking = "high"
args = ["--no-lens-context"]
exclude_tools = ["edit", "write", "edit"]
instructions = "Focus on concurrency bugs."
instructions_file = "~/review-checklist.md"

[roles.security-audit]
worktree = false
agent = "claude"
model = "claude-opus-5-5"
thinking = "max"
brief = "investigator"
instructions = "Report findings with CWE references."
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	rev, err := loaded.resolveRole("reviewer")
	if err != nil {
		t.Fatalf("resolveRole(reviewer): %v", err)
	}
	if rev.Worktree || rev.Agent != "pi" || rev.Model != "anthropic/claude-opus-5-5" || rev.Thinking != "high" ||
		rev.Brief != roleReviewer || rev.Custom || !rev.Configured {
		t.Errorf("overridden reviewer = %+v", rev)
	}
	if !slices.Equal(rev.ExcludeTools, []string{"edit", "write"}) {
		t.Errorf("reviewer exclude_tools = %q, want [edit write]", rev.ExcludeTools)
	}
	if !slices.Equal(rev.Args, []string{"--no-lens-context"}) {
		t.Errorf("reviewer args = %q", rev.Args)
	}
	if wantFile := filepath.Join(home, "review-checklist.md"); rev.InstructionsFile != wantFile {
		t.Errorf("instructions_file = %q, want %q", rev.InstructionsFile, wantFile)
	}

	custom, err := loaded.resolveRole("security-audit")
	if err != nil {
		t.Fatalf("resolveRole(security-audit): %v", err)
	}
	if custom.Worktree || custom.Agent != "claude" || custom.Model != "claude-opus-5-5" ||
		custom.Thinking != "max" || custom.Brief != roleInvestigator || !custom.Custom || !custom.Configured {
		t.Errorf("custom role = %+v", custom)
	}

	// Custom role with no worktree/brief explicitly set defaults to implementer behavior.
	loaded.Roles["custom-default"] = RoleConfig{Model: "sonnet"}
	cd, err := loaded.resolveRole("custom-default")
	if err != nil {
		t.Fatalf("resolveRole(custom-default): %v", err)
	}
	if !cd.Worktree || cd.Brief != roleImplementer || !cd.Custom {
		t.Errorf("custom-default = %+v, want worktree=true brief=implementer custom=true", cd)
	}

	// Overriding a built-in worktree=false role to worktree=true works.
	loaded.Roles["reviewer"] = RoleConfig{Worktree: boolPtr(true)}
	revWT, err := loaded.resolveRole("reviewer")
	if err != nil || !revWT.Worktree {
		t.Errorf("reviewer worktree=true override: %+v, %v", revWT, err)
	}
}

func TestValidateRolesFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		roles map[string]RoleConfig
		want  string
	}{
		{"bad uppercase name", map[string]RoleConfig{"Reviewer": {}}, "invalid role name"},
		{"bad leading dash", map[string]RoleConfig{"-rev": {}}, "invalid role name"},
		{"bad colon in name", map[string]RoleConfig{"role:rev": {}}, "invalid role name"},
		{"bad space in name", map[string]RoleConfig{"my role": {}}, "invalid role name"},
		{"unknown agent", map[string]RoleConfig{"reviewer": {Agent: "bogus"}}, "unknown agent"},
		{"unknown brief", map[string]RoleConfig{"custom": {Brief: "architect"}}, "unknown brief"},
		{"model whitespace", map[string]RoleConfig{"reviewer": {Model: "a b"}}, "invalid model"},
		{"thinking invalid token", map[string]RoleConfig{"reviewer": {Thinking: "HIGH!"}}, "invalid thinking"},
		{"pi invalid thinking", map[string]RoleConfig{"reviewer": {Agent: "pi", Thinking: "turbo"}}, "invalid pi thinking level"},
		{"exclude_tools empty", map[string]RoleConfig{"reviewer": {ExcludeTools: []string{""}}}, "invalid exclude_tools"},
		{"exclude_tools comma", map[string]RoleConfig{"reviewer": {ExcludeTools: []string{"edit,write"}}}, "invalid exclude_tools"},
		{"exclude_tools space", map[string]RoleConfig{"reviewer": {ExcludeTools: []string{"edit write"}}}, "invalid exclude_tools"},
		{"instructions_file whitespace", map[string]RoleConfig{"reviewer": {InstructionsFile: "   "}}, "instructions_file"},
		{"valid auto agent", map[string]RoleConfig{"custom": {Agent: "auto", Brief: "rollout", Thinking: "high", ExcludeTools: []string{"edit", "mcp__mem__*"}}}, ""},
	}
	for _, tc := range cases {
		cfg := defaultConfig()
		cfg.Roles = tc.roles
		err := cfg.validateRoles()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected err: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

func TestResolveRoleForBeadPrecedenceAndFailClosed(t *testing.T) {
	cfg := defaultConfig()
	cfg.Roles = map[string]RoleConfig{"audit": {Worktree: boolPtr(false), Brief: roleInvestigator}}

	// Default -> implementer.
	r, err := cfg.resolveRoleForBead(Bead{ID: "b-1"}, "")
	if err != nil || r.Name != roleImplementer {
		t.Fatalf("default: %+v, %v", r, err)
	}
	// Label selects role when flag omitted.
	r, err = cfg.resolveRoleForBead(Bead{ID: "b-1", Labels: []string{"next", "role:reviewer"}}, "")
	if err != nil || r.Name != roleReviewer || r.Worktree {
		t.Fatalf("label: %+v, %v", r, err)
	}
	// --role flag overrides role:<name> label.
	r, err = cfg.resolveRoleForBead(Bead{ID: "b-1", Labels: []string{"role:reviewer"}}, "audit")
	if err != nil || r.Name != "audit" || r.Brief != roleInvestigator {
		t.Fatalf("flag override: %+v, %v", r, err)
	}
	// Multiple role:* labels fail closed even when --role is supplied.
	for _, flag := range []string{"", "reviewer"} {
		if _, err := cfg.resolveRoleForBead(Bead{ID: "b-1", Labels: []string{"role:reviewer", "role:investigator"}}, flag); err == nil || !strings.Contains(err.Error(), "multiple role labels") {
			t.Errorf("multiple labels (flag=%q): err = %v", flag, err)
		}
	}
	// Empty role: label fails closed.
	if _, err := cfg.resolveRoleForBead(Bead{ID: "b-1", Labels: []string{"role:"}}, ""); err == nil || !strings.Contains(err.Error(), "empty role:") {
		t.Errorf("empty role label: err = %v", err)
	}
	// Unknown --role or unknown role:<name> label fails closed.
	if _, err := cfg.resolveRoleForBead(Bead{ID: "b-1"}, "nonexistent"); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("unknown flag role: err = %v", err)
	}
	if _, err := cfg.resolveRoleForBead(Bead{ID: "b-1", Labels: []string{"role:nonexistent"}}, ""); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("unknown label role: err = %v", err)
	}
}

func TestBriefGenerationInvariants(t *testing.T) {
	b := Bead{ID: "task-42", Title: "Review gateway retry logic", Status: StatusInProgress}
	instrDir := t.TempDir()
	instrPath := filepath.Join(instrDir, "role-extra.md")
	if err := os.WriteFile(instrPath, []byte("Always verify timeout bounds."), 0o644); err != nil {
		t.Fatal(err)
	}

	// Implementer with worktree=true and no role instructions preserves brief() verbatim.
	imp := builtinRolePreset(roleImplementer)
	wantImp := brief(b, "tj/task-42-review", "/wt/task-42", "ENG-10", "Global rule.")
	gotImp := briefForRole(b, imp, "tj/task-42-review", "/wt/task-42", "", "ENG-10", "Global rule.", "")
	if gotImp != wantImp {
		t.Errorf("implementer brief diverged from brief():\ngot:\n%s\nwant:\n%s", gotImp, wantImp)
	}

	// Non-implementer briefs must omit PR-authoring and auto-merge boilerplate.
	forbidden := []string{
		"When you open a PR",
		"gh pr merge",
		"--auto --squash",
		"auto-merge",
		"Prefix the PR title",
		"Before your first push",
	}

	for _, name := range []string{roleReviewer, roleInvestigator, roleRollout} {
		role := builtinRolePreset(name)
		role.Instructions = "Inline role rule."
		role.InstructionsFile = instrPath
		roleInstr, err := loadRoleInstructions(role)
		if err != nil {
			t.Fatalf("loadRoleInstructions(%s): %v", name, err)
		}
		got := briefForRole(b, role, "", "/state/roles/task-42", "/src/app", "ENG-10", "Global standing rule.", roleInstr)
		for _, bad := range forbidden {
			if strings.Contains(got, bad) {
				t.Errorf("%s brief unexpectedly contains PR-authoring/auto-merge text %q:\n%s", name, bad, got)
			}
		}
		for _, must := range []string{
			"Working directory: /state/roles/task-42",
			"Configured repository (read-only reference only",
			"/src/app",
			"quartermaster close task-42 --by worker",
			"Global standing rule.",
			"Role instructions (" + name + "):",
			"Inline role rule.",
			"Always verify timeout bounds.",
		} {
			if !strings.Contains(got, must) {
				t.Errorf("%s brief missing %q:\n%s", name, must, got)
			}
		}
	}

	// Role-specific semantic requirements.
	revText := briefForRole(b, builtinRolePreset(roleReviewer), "", "/state/roles/task-42", "/src/app", "", "", "")
	for _, must := range []string{"skeptical", "head commit SHA", "needs_me", "file/line", "never include bead IDs"} {
		if !strings.Contains(strings.ToLower(revText), strings.ToLower(must)) {
			t.Errorf("reviewer brief missing %q", must)
		}
	}
	invText := briefForRole(b, builtinRolePreset(roleInvestigator), "", "/state/roles/task-42", "/src/app", "", "", "")
	for _, must := range []string{"strictly read-only", "ZERO mutations", "follow-up beads"} {
		if !strings.Contains(invText, must) {
			t.Errorf("investigator brief missing %q", must)
		}
	}
	rollText := briefForRole(b, builtinRolePreset(roleRollout), "", "/state/roles/task-42", "/src/app", "", "", "")
	for _, must := range []string{"rolling-out", "Actions run URL", "quartermaster gate plan", "Acceptance"} {
		if !strings.Contains(rollText, must) {
			t.Errorf("rollout brief missing %q", must)
		}
	}

	// Missing instructions_file fails closed.
	badRole := builtinRolePreset(roleReviewer)
	badRole.InstructionsFile = filepath.Join(instrDir, "does-not-exist.md")
	if _, err := loadRoleInstructions(badRole); err == nil {
		t.Error("expected missing instructions_file to fail closed")
	}
}

func TestWorkerArgsMergingAndPiExcludeTools(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agents = map[string]AgentConfig{
		"pi": {
			Args: []string{
				"--approve",
				"--model", "quartermaster/worker-auto",
				"--thinking", "medium",
				"--exclude-tools", "mcp__memory__memory_delete,mcp__memory__memory_cleanup",
			},
		},
		"claude": {Args: []string{"--model=opus", "--effort", "low"}},
		"muse":   {Args: []string{"--disable-sandbox", "--effort=low"}},
		"codex":  {Args: []string{"-m", "gpt-5", "-c", "model_reasoning_effort=low"}},
		"agy":    {Args: []string{"--mode", "auto", "--model", "gemini-old"}},
	}

	// pi: replaces model & thinking, appends extra args, merges exclude_tools preserving base without dupes.
	piRole := EffectiveRole{
		Name:         roleInvestigator,
		Model:        "anthropic/claude-opus-5-5",
		Thinking:     "xhigh",
		Args:         []string{"--no-lens-context"},
		ExcludeTools: []string{"edit", "mcp__memory__memory_delete", "write"},
	}
	gotPi := cfg.workerArgs("pi", false, piRole)
	wantPi := []string{
		"--approve",
		"--model", "anthropic/claude-opus-5-5",
		"--thinking", "xhigh",
		"--exclude-tools", "mcp__memory__memory_delete,mcp__memory__memory_cleanup,edit,write",
		"--no-lens-context",
	}
	if !slices.Equal(gotPi, wantPi) {
		t.Errorf("pi workerArgs:\n got %q\nwant %q", gotPi, wantPi)
	}

	// pi with -xt= form and no existing --model.
	gotPiShort := applyRoleArgs("pi", []string{"--approve", "-xt=bash,edit"}, EffectiveRole{
		Model:        "openai/gpt-5",
		ExcludeTools: []string{"write", "edit"},
	})
	wantPiShort := []string{"--approve", "--exclude-tools", "bash,edit,write", "--model", "openai/gpt-5"}
	if !slices.Equal(gotPiShort, wantPiShort) {
		t.Errorf("pi -xt merge:\n got %q\nwant %q", gotPiShort, wantPiShort)
	}

	// pi with no base exclusions appends --exclude-tools.
	gotPiFresh := applyRoleArgs("pi", []string{"--approve"}, EffectiveRole{ExcludeTools: []string{"edit", "write"}})
	wantPiFresh := []string{"--approve", "--exclude-tools", "edit,write"}
	if !slices.Equal(gotPiFresh, wantPiFresh) {
		t.Errorf("pi fresh exclude_tools:\n got %q\nwant %q", gotPiFresh, wantPiFresh)
	}

	// claude: replaces --model= and --effort, keeps --continue last.
	gotClaude := cfg.workerArgs("claude", false, EffectiveRole{Model: "sonnet", Thinking: "high"}, resumeArgs["claude"]...)
	wantClaude := []string{"--model", "sonnet", "--effort", "high", "--continue"}
	if !slices.Equal(gotClaude, wantClaude) {
		t.Errorf("claude workerArgs:\n got %q\nwant %q", gotClaude, wantClaude)
	}

	// muse: replaces --effort= with --reasoning-effort, keeps resume --last at end.
	gotMuse := cfg.workerArgs("muse", false, EffectiveRole{Model: "m1", Thinking: "high"}, resumeArgs["muse"]...)
	wantMuse := []string{"--disable-sandbox", "--reasoning-effort", "high", "--model", "m1", "resume", "--last"}
	if !slices.Equal(gotMuse, wantMuse) {
		t.Errorf("muse workerArgs:\n got %q\nwant %q", gotMuse, wantMuse)
	}

	// codex: replaces -m with --model and -c model_reasoning_effort=..., keeps resume --last at end.
	gotCodex := cfg.workerArgs("codex", false, EffectiveRole{Model: "o4", Thinking: "high"}, resumeArgs["codex"]...)
	wantCodex := []string{"--model", "o4", "-c", "model_reasoning_effort=high", "resume", "--last"}
	if !slices.Equal(gotCodex, wantCodex) {
		t.Errorf("codex workerArgs:\n got %q\nwant %q", gotCodex, wantCodex)
	}

	// agy: replaces --model and appends --effort.
	gotAgy := cfg.workerArgs("agy", false, EffectiveRole{Model: "gemini-4-neon-xhigh", Thinking: "xhigh"}, resumeArgs["agy"]...)
	wantAgy := []string{"--mode", "auto", "--model", "gemini-4-neon-xhigh", "--effort", "xhigh", "--continue"}
	if !slices.Equal(gotAgy, wantAgy) {
		t.Errorf("agy workerArgs:\n got %q\nwant %q", gotAgy, wantAgy)
	}
}

// fakeRoleTools extends fakeTools with workspace create/list/close and bd list
// support so worktree = false dispatch, resume, and resolve run end-to-end.
func fakeRoleTools(t *testing.T) string {
	t.Helper()
	dir := fakeTools(t)
	herdrScript := `#!/bin/sh
echo "$*" >> "$LOG/herdr.log"
remote=
if [ "$1" = --machine ]; then remote=1; shift 2; fi
case "$1 $2" in
"agent list")
  if [ -n "$remote" ]; then echo "{\"result\":{\"agents\":${FAKE_REMOTE_AGENTS:-[]}}}"; else echo "{\"result\":{\"agents\":${FAKE_LOCAL_AGENTS:-[]}}}"; fi ;;
"worktree create")
  echo '{"result":{"workspace":{"workspace_id":"w9"},"worktree":{"path":"/home/u/worktrees/x"},"root_pane":{"pane_id":"w9:p1"}}}' ;;
"worktree list")
  echo "{\"result\":{\"worktrees\":${FAKE_WORKTREES:-[]}}}" ;;
"workspace create")
  echo '{"result":{"workspace":{"workspace_id":"w5"},"root_pane":{"pane_id":"w5:p1"}}}' ;;
"workspace list")
  echo "{\"result\":{\"workspaces\":${FAKE_WORKSPACES:-[]}}}" ;;
"pane list")
  echo "{\"result\":{\"panes\":${FAKE_PANES:-[]}}}" ;;
"pane process-info")
  echo "{\"result\":{\"process_info\":{\"foreground_processes\":${FAKE_PROCS:-[]}}}}" ;;
*)
  echo '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(herdrScript), 0o755); err != nil {
		t.Fatal(err)
	}
	bdScript := `#!/bin/sh
echo "$*" >> "$LOG/bd.log"
case "$1" in
show) echo "$FAKE_BEAD" ;;
list) echo "${FAKE_BEADS_LIST:-[]}" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorktreeFalseDispatchResumeResolveLifecycle(t *testing.T) {
	dir := fakeRoleTools(t)
	fakePiTool(t, dir, piAutoListing, 0)

	cfg := hostsConfig()
	cfg.WorkerAgent = "pi"
	cfg.Agents = map[string]AgentConfig{
		"pi": {
			Args: []string{"--approve", "--model", piWorkerVirtualModel, "--exclude-tools", "mcp__memory__memory_delete"},
		},
	}
	cfg.Roles = map[string]RoleConfig{
		roleReviewer: {
			Model:        "anthropic/claude-opus-5-5",
			Thinking:     "high",
			Args:         []string{"--no-lens-context"},
			ExcludeTools: []string{"edit", "write"},
			Instructions: "Check edge cases.",
		},
	}

	// Bead references an uncloned repo label: worktree = false must still dispatch cleanly!
	t.Setenv("FAKE_BEAD", `[{"id":"task-7","title":"Review PR #99","status":"open","labels":["repo:uncloned"]}]`)
	msg, err := dispatch(cfg, newHerdr(), "task-7", DispatchOpts{Role: roleReviewer})
	if err != nil {
		t.Fatalf("dispatch reviewer: %v", err)
	}
	rdir, err := roleDir("task-7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "role reviewer") || !strings.Contains(msg, rdir) {
		t.Errorf("dispatch msg = %q, want role reviewer and dir %s", msg, rdir)
	}

	// Verify <state>/roles/task-7 exists with mode 0700.
	fi, err := os.Stat(rdir)
	if err != nil {
		t.Fatalf("role dir not created: %v", err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("role dir mode = %v, want dir 0700", fi.Mode())
	}

	// Zero git worktree/branch creation; workspace created at roleDir instead.
	herdrLog := logLines(t, dir, "herdr")
	if hasLine(herdrLog, "worktree create") || hasLine(herdrLog, "worktree open") {
		t.Errorf("worktree = false unexpectedly created/opened a git worktree:\n%s", strings.Join(herdrLog, "\n"))
	}
	if !hasLine(herdrLog, "workspace create --cwd "+rdir+" --label task-7 --no-focus") {
		t.Errorf("workspace create missing:\n%s", strings.Join(herdrLog, "\n"))
	}
	// Agent started with role model, thinking, merged exclude_tools, and extra args.
	wantStart := "agent start task-7 --kind pi --pane w5:p1 --timeout 30000 -- --approve --model anthropic/claude-opus-5-5 --exclude-tools mcp__memory__memory_delete,edit,write --thinking high --no-lens-context"
	if !hasLine(herdrLog, wantStart) {
		t.Errorf("agent start missing role args:\nwant prefix: %s\ngot:\n%s", wantStart, strings.Join(herdrLog, "\n"))
	}
	// Env includes QUARTERMASTER_ROLE.
	if !slices.ContainsFunc(herdrLog, func(l string) bool {
		return strings.HasPrefix(l, "pane run w5:p1 ") && strings.Contains(l, "QUARTERMASTER_ROLE='reviewer'")
	}) {
		t.Errorf("QUARTERMASTER_ROLE not exported:\n%s", strings.Join(herdrLog, "\n"))
	}
	// Brief uses reviewer template (no auto-merge) and appends role instructions.
	fullHerdrLog := strings.Join(herdrLog, "\n")
	if !strings.Contains(fullHerdrLog, "agent prompt task-7 You are the quartermaster reviewer worker (role: reviewer) for bead task-7") ||
		!strings.Contains(fullHerdrLog, "Role instructions (reviewer):\nCheck edge cases.") ||
		strings.Contains(fullHerdrLog, "auto-merge") {
		t.Errorf("reviewer brief not sent properly:\n%s", fullHerdrLog)
	}
	// Persists role:reviewer label on the bead.
	bdLog := logLines(t, dir, "bd")
	if !slices.Contains(bdLog, "label add task-7 role:reviewer") {
		t.Errorf("role:reviewer label not persisted: %q", bdLog)
	}

	// Clear logs and test resume discovery & relaunch with role settings.
	_ = os.Remove(filepath.Join(dir, "herdr.log"))
	_ = os.Remove(filepath.Join(dir, "bd.log"))

	// orphans must discover task-7 via <state>/roles/task-7 without calling worktree list!
	wtListCalls := 0
	resList, bareList, err := orphans(cfg, []Bead{{
		ID:       "task-7",
		Title:    "Review PR #99",
		Status:   StatusInProgress,
		Assignee: "pi",
		Labels:   []string{"role:reviewer", "repo:uncloned"},
	}}, map[string]bool{}, nil, func(repo string) ([]Worktree, error) {
		wtListCalls++
		return nil, nil
	})
	if err != nil {
		t.Fatalf("orphans: %v", err)
	}
	if wtListCalls != 0 {
		t.Errorf("orphans called worktree list %d times for worktree=false bead", wtListCalls)
	}
	if len(bareList) != 0 || len(resList) != 1 || resList[0].RoleDir != rdir {
		t.Fatalf("orphans got res=%+v bare=%+v", resList, bareList)
	}

	// Run resume() end-to-end for task-7.
	t.Setenv("FAKE_BEADS_LIST", `[{"id":"task-7","title":"Review PR #99","status":"in_progress","assignee":"pi","labels":["role:reviewer","repo:uncloned"]}]`)
	t.Setenv("FAKE_WORKSPACES", `[{"workspace_id":"w5","label":"task-7","worktree":null}]`)
	t.Setenv("FAKE_PANES", `[{"pane_id":"w5:p1","agent":""}]`)
	lines, err := resume(cfg, newHerdr(), []string{"task-7"}, "", "TJ")
	if err != nil {
		t.Fatalf("resume: %v (%v)", err, lines)
	}
	herdrLog = logLines(t, dir, "herdr")
	if hasLine(herdrLog, "worktree open") || hasLine(herdrLog, "worktree list") {
		t.Errorf("resume touched git worktrees for worktree=false bead:\n%s", strings.Join(herdrLog, "\n"))
	}
	if !hasLine(herdrLog, wantStart) {
		t.Errorf("resume did not preserve role args:\nwant prefix: %s\ngot:\n%s", wantStart, strings.Join(herdrLog, "\n"))
	}

	// resolve refuses while bead is still open/in_progress without --force.
	t.Setenv("FAKE_BEAD", `[{"id":"task-7","title":"Review PR #99","status":"in_progress","labels":["role:reviewer","repo:uncloned"]}]`)
	if _, err := resolve(cfg, newHerdr(), "task-7", false); err == nil {
		t.Fatal("resolve should refuse open worktree=false bead without --force")
	}

	// Close bead and resolve: closes non-worktree workspace and deletes <state>/roles/task-7 cleanly without touching git.
	_ = os.Remove(filepath.Join(dir, "herdr.log"))
	t.Setenv("FAKE_BEAD", `[{"id":"task-7","title":"Review PR #99","status":"closed","labels":["role:reviewer","repo:uncloned"]}]`)
	resMsg, err := resolve(cfg, newHerdr(), "task-7", false)
	if err != nil {
		t.Fatalf("resolve closed role bead: %v", err)
	}
	if !strings.Contains(resMsg, "removed role directory "+rdir) {
		t.Errorf("resolve msg = %q", resMsg)
	}
	if _, err := os.Stat(rdir); !os.IsNotExist(err) {
		t.Errorf("role dir still exists after resolve: %v", err)
	}
	herdrLog = logLines(t, dir, "herdr")
	if !slices.Contains(herdrLog, "workspace close w5") {
		t.Errorf("resolve did not close role workspace:\n%s", strings.Join(herdrLog, "\n"))
	}
	if hasLine(herdrLog, "worktree list") || hasLine(herdrLog, "worktree remove") {
		t.Errorf("resolve touched git worktrees for worktree=false bead:\n%s", strings.Join(herdrLog, "\n"))
	}

	// Second resolve is clean and idempotent.
	resMsg2, err := resolve(cfg, newHerdr(), "task-7", false)
	if err != nil || !strings.Contains(resMsg2, "nothing to remove: no role directory for task-7") {
		t.Errorf("idempotent resolve: %q, %v", resMsg2, err)
	}

	// Resume when no open workspace exists calls workspace create (never worktree open).
	if _, err := ensureRoleDir("task-8"); err != nil {
		t.Fatal(err)
	}
	rdir8, _ := roleDir("task-8")
	_ = os.Remove(filepath.Join(dir, "herdr.log"))
	t.Setenv("FAKE_BEADS_LIST", `[{"id":"task-8","title":"Investigate spike","status":"in_progress","assignee":"pi","labels":["role:investigator"]}]`)
	t.Setenv("FAKE_WORKSPACES", `[]`)
	t.Setenv("FAKE_PANES", `[]`)
	if _, err := resume(cfg, newHerdr(), []string{"task-8"}, "", "TJ"); err != nil {
		t.Fatalf("resume without open workspace: %v", err)
	}
	herdrLog = logLines(t, dir, "herdr")
	if !hasLine(herdrLog, "workspace create --cwd "+rdir8+" --label task-8 --no-focus") {
		t.Errorf("resume did not open workspace at role dir:\n%s", strings.Join(herdrLog, "\n"))
	}
	if hasLine(herdrLog, "worktree open") {
		t.Errorf("resume called worktree open for worktree=false bead:\n%s", strings.Join(herdrLog, "\n"))
	}
}

func TestWorktreeFalseConfiguredRepoRefAndMissingInstructionsFile(t *testing.T) {
	dir := fakeRoleTools(t)
	cfg := hostsConfig()

	// Configured repo:infra is referenced strictly read-only in the brief.
	t.Setenv("FAKE_BEAD", `[{"id":"b-ref","title":"Audit infra","status":"open","labels":["repo:infra"]}]`)
	if _, err := dispatch(cfg, newHerdr(), "b-ref", DispatchOpts{Role: roleInvestigator}); err != nil {
		t.Fatalf("dispatch investigator with repo:infra: %v", err)
	}
	fullLog := strings.Join(logLines(t, dir, "herdr"), "\n")
	if !strings.Contains(fullLog, "Configured repository (read-only reference only — never edit, checkout, commit, or dirty it): /home/u/src/infra.") {
		t.Errorf("brief missing read-only repo reference:\n%s", fullLog)
	}

	// Missing instructions_file fails closed before claiming or creating role dir.
	_ = os.Remove(filepath.Join(dir, "bd.log"))
	cfg.Roles = map[string]RoleConfig{
		roleRollout: {InstructionsFile: filepath.Join(t.TempDir(), "missing.md")},
	}
	t.Setenv("FAKE_BEAD", `[{"id":"b-miss","title":"Rollout","status":"open"}]`)
	if _, err := dispatch(cfg, newHerdr(), "b-miss", DispatchOpts{Role: roleRollout}); err == nil || !strings.Contains(err.Error(), "instructions_file") {
		t.Fatalf("expected missing instructions_file error, got %v", err)
	}
	if _, ok := hasRoleDir("b-miss"); ok {
		t.Error("failed dispatch still created role dir")
	}
	if hasLine(logLines(t, dir, "bd"), "update b-miss --claim") {
		t.Error("failed dispatch still claimed bead")
	}
}

func TestRunRolesWithoutRepoConfigured(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", cfgDir)
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	conf := "[roles.triage]\nworktree = false\nbrief = \"investigator\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"roles", "--json"}); err != nil {
		t.Fatalf("run(roles --json) without repo failed: %v", err)
	}
}

func TestDispatchRoleFailClosedAndLabelReplacement(t *testing.T) {
	dir := fakeRoleTools(t)
	cfg := hostsConfig()

	// Unknown --role fails closed before creating anything.
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"T","status":"open"}]`)
	if _, err := dispatch(cfg, newHerdr(), "b-1", DispatchOpts{Role: "unknown"}); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("unknown --role: err=%v", err)
	}
	if hasLine(logLines(t, dir, "bd"), "update b-1 --claim") {
		t.Error("failed unknown --role still claimed bead")
	}

	// Multiple role:* labels on bead fail closed.
	t.Setenv("FAKE_BEAD", `[{"id":"b-2","title":"T","status":"open","labels":["role:reviewer","role:rollout"]}]`)
	if _, err := dispatch(cfg, newHerdr(), "b-2", DispatchOpts{}); err == nil || !strings.Contains(err.Error(), "multiple role labels") {
		t.Fatalf("multiple role labels: err=%v", err)
	}

	// Overriding existing role:reviewer label with --role investigator replaces the label.
	t.Setenv("FAKE_BEAD", `[{"id":"b-3","title":"Diagnose","status":"open","labels":["role:reviewer"]}]`)
	if _, err := dispatch(cfg, newHerdr(), "b-3", DispatchOpts{Role: roleInvestigator}); err != nil {
		t.Fatalf("override role: %v", err)
	}
	bdLog := logLines(t, dir, "bd")
	if !slices.Contains(bdLog, "label remove b-3 role:reviewer") || !slices.Contains(bdLog, "label add b-3 role:investigator") {
		t.Errorf("role label replacement failed: %q", bdLog)
	}
}

func TestRolesCmdTextAndJSON(t *testing.T) {
	cfg := defaultConfig()
	cfg.Roles = map[string]RoleConfig{
		roleReviewer: {
			Agent:        "pi",
			Model:        "anthropic/claude-opus-5-5",
			Thinking:     "high",
			ExcludeTools: []string{"edit", "write"},
		},
		"triage": {
			Worktree: boolPtr(false),
			Brief:    roleInvestigator,
			Agent:    "claude",
			Model:    "sonnet",
		},
	}

	// JSON output parses structurally with all built-ins + custom roles and required fields.
	var jsonBuf bytes.Buffer
	if err := rolesCmd(cfg, nil, true, &jsonBuf); err != nil {
		t.Fatalf("rolesCmd --json: %v", err)
	}
	var got []EffectiveRole
	if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, jsonBuf.String())
	}
	if len(got) != 5 {
		t.Fatalf("got %d roles, want 5: %+v", len(got), got)
	}
	byName := map[string]EffectiveRole{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if r := byName[roleImplementer]; !r.Worktree || r.Brief != roleImplementer || r.Custom {
		t.Errorf("implementer = %+v", r)
	}
	if r := byName[roleReviewer]; r.Worktree || r.Agent != "pi" || r.Model != "anthropic/claude-opus-5-5" ||
		r.Thinking != "high" || r.Brief != roleReviewer || !slices.Equal(r.ExcludeTools, []string{"edit", "write"}) || r.Custom || !r.Configured {
		t.Errorf("reviewer = %+v", r)
	}
	if r := byName[roleInvestigator]; r.Worktree || r.Brief != roleInvestigator || r.Custom {
		t.Errorf("investigator = %+v", r)
	}
	if r := byName[roleRollout]; r.Worktree || r.Brief != roleRollout || r.Custom {
		t.Errorf("rollout = %+v", r)
	}
	if r := byName["triage"]; r.Worktree || r.Agent != "claude" || r.Model != "sonnet" || r.Brief != roleInvestigator || !r.Custom || !r.Configured {
		t.Errorf("triage = %+v", r)
	}

	// Text table output includes headers and roles.
	var textBuf bytes.Buffer
	if err := rolesCmd(cfg, nil, false, &textBuf); err != nil {
		t.Fatalf("rolesCmd text: %v", err)
	}
	txt := textBuf.String()
	for _, want := range []string{"ROLE", "WORKTREE", "EXCLUDE_TOOLS", "CUSTOM", "implementer", "reviewer", "investigator", "rollout", "triage", "edit,write"} {
		if !strings.Contains(txt, want) {
			t.Errorf("roles table missing %q:\n%s", want, txt)
		}
	}

	// Extra positional args return usage error.
	if err := rolesCmd(cfg, []string{"extra"}, false, &textBuf); err == nil {
		t.Error("expected usage error on extra args")
	}
}

func TestContextAndCoordinatorGuideRoles(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	cfg := Config{Repo: "/r"}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	threads := []Thread{
		{
			Bead:  Bead{ID: "b-rev", Title: "Review auth PR", Status: StatusInProgress, Labels: []string{"role:reviewer"}, UpdatedAt: now},
			Agent: &Agent{Name: "b-rev", Kind: "pi", Status: "working"},
		},
		{
			Bead:  Bead{ID: "b-bad", Title: "Conflicting roles", Status: StatusInProgress, Labels: []string{"role:reviewer", "role:rollout"}, UpdatedAt: now},
			Agent: &Agent{Name: "b-bad", Kind: "pi", Status: "idle"},
		},
	}

	out := renderContext(cfg, threads, nil, nil, nil, TickerState{}, nil, now)
	if !strings.Contains(out, "- b-rev [role:reviewer] · ") {
		t.Errorf("context text missing role tag:\n%s", out)
	}
	if !strings.Contains(out, "WARNING: b-bad has multiple role labels") {
		t.Errorf("context text missing multiple-role warning:\n%s", out)
	}

	// JSON context carries role field on non-default role thread.
	jsonOut, err := renderContextWith(cfg, threads, nil, nil, nil, TickerState{}, nil, now, ContextOptions{JSON: true})
	if err != nil {
		t.Fatalf("renderContextWith JSON: %v", err)
	}
	var doc struct {
		Threads []struct {
			Bead string `json:"bead"`
			Role string `json:"role"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatalf("unmarshal context JSON: %v", err)
	}
	foundRev := false
	for _, th := range doc.Threads {
		if th.Bead == "b-rev" && th.Role == roleReviewer {
			foundRev = true
		}
	}
	if !foundRev {
		t.Errorf("context JSON did not include role=reviewer for b-rev: %+v", doc.Threads)
	}

	// coordinatorGuide documents --role, role:<name>, and quartermaster roles.
	guide := coordinatorGuide(cfg)
	for _, want := range []string{"--role implementer|reviewer|investigator|rollout", "role:<name>", "quartermaster roles"} {
		if !strings.Contains(guide, want) {
			t.Errorf("coordinatorGuide missing %q", want)
		}
	}
}
