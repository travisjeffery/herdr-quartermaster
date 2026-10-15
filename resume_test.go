package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var resumeCfg = Config{Repo: "/r", BranchPrefix: "tj/", WorkerAgent: "claude"}

func ids(beads []Bead) []string {
	var out []string
	for _, b := range beads {
		out = append(out, b.ID)
	}
	return out
}

func TestBeadWorktreeMatchesOwnBranchOrPRHead(t *testing.T) {
	b := Bead{ID: "backend-ab12", Title: "Fix the thing"}
	own := Worktree{Branch: "tj/backend-ab12-fix-the-thing", Path: "/w/own", Linked: true}
	moved := Worktree{Branch: "tj/renamed", Path: "/w/moved", Linked: true}
	checkout := Worktree{Branch: "tj/backend-ab12-fix-the-thing", Path: "/r"}
	if w, ok := beadWorktree(resumeCfg, b, nil, []Worktree{checkout, own}); !ok || w.Path != "/w/own" {
		t.Fatalf("own branch: %+v %v; the main checkout must never match", w, ok)
	}
	prs := map[string]PR{b.ID: {Head: "tj/renamed"}}
	if w, ok := beadWorktree(resumeCfg, b, prs, []Worktree{moved}); !ok || w.Path != "/w/moved" {
		t.Fatalf("PR head: %+v %v", w, ok)
	}
	if _, ok := beadWorktree(resumeCfg, b, nil, []Worktree{moved}); ok {
		t.Fatal("an unrelated branch matched")
	}
}

func TestOrphansSkipLiveAgentsAndSplitOnWorktree(t *testing.T) {
	beads := []Bead{
		{ID: "backend-live", Title: "a"},
		{ID: "backend-wt.1", Title: "b"},
		{ID: "backend-none", Title: "c"},
	}
	calls := 0
	list := cachedWorktrees(func(repo string) ([]Worktree, error) {
		calls++
		return []Worktree{
			{Branch: "tj/backend-live-a", Linked: true},
			{Branch: "tj/backend-wt-1-b", Linked: true, WorkspaceID: "w3"},
		}, nil
	})
	res, bare, err := orphans(resumeCfg, beads, map[string]bool{"backend-live": true}, nil, list)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Bead.ID != "backend-wt.1" || res[0].Worktree.WorkspaceID != "w3" {
		t.Fatalf("resumable: %+v", res)
	}
	if !slices.Equal(ids(bare), []string{"backend-none"}) {
		t.Fatalf("bare: %v", ids(bare))
	}
	if calls != 1 {
		t.Fatalf("listed worktrees %d times for one repo", calls)
	}
}

func TestStaleClaimsAreOldInProgressOnly(t *testing.T) {
	old := t0.Add(-8 * 24 * time.Hour)
	bare := []Bead{
		{ID: "old", Status: StatusInProgress, UpdatedAt: old},
		{ID: "young", Status: StatusInProgress, UpdatedAt: t0.Add(-6 * 24 * time.Hour)},
		{ID: "old-blocked", Status: StatusBlocked, UpdatedAt: old},
		{ID: "old-needs-me", Status: StatusNeedsMe, UpdatedAt: old},
		{ID: "old-merged", Status: StatusInProgress, UpdatedAt: old},
	}
	prs := map[string]PR{"old-merged": {Number: 7, State: "MERGED"}}
	if got := ids(staleClaims(bare, prs, t0, 7*24*time.Hour)); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("got %v", got)
	}
}

func TestPickResumable(t *testing.T) {
	res := []Resumable{{Bead: Bead{ID: "a"}}, {Bead: Bead{ID: "b"}}}
	if got, missing := pickResumable(res, nil); len(got) != 2 || missing != nil {
		t.Fatalf("no names should pick all: %v %v", got, missing)
	}
	got, missing := pickResumable(res, []string{"b", "zz"})
	if len(got) != 1 || got[0].Bead.ID != "b" || !slices.Equal(missing, []string{"zz"}) {
		t.Fatalf("got %v missing %v", got, missing)
	}
}

func TestResumeKind(t *testing.T) {
	codexClaimed := Bead{Assignee: "codex"}
	cases := []struct {
		b    Bead
		kind string
		want string
	}{
		{codexClaimed, "claude", "claude"},
		{codexClaimed, "", "codex"},
		{Bead{Assignee: "tj"}, "", "claude"},
	}
	for _, c := range cases {
		if got := resumeKind(resumeCfg, c.b, c.kind); got != c.want {
			t.Errorf("resumeKind(%q, %q) = %q, want %q", c.b.Assignee, c.kind, got, c.want)
		}
	}
}

func TestFreePaneSkipsAgentPanes(t *testing.T) {
	if p, ok := freePane([]Pane{{ID: "w1:p1", Agent: "claude"}, {ID: "w1:p2"}}); !ok || p != "w1:p2" {
		t.Fatalf("got %q %v", p, ok)
	}
	if _, ok := freePane([]Pane{{ID: "w1:p1", Agent: "codex"}}); ok {
		t.Fatal("an agent's pane was offered")
	}
}

func TestAgentStartPassesResumeArgsAfterDashes(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argv + "\necho '{\"result\":{}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (Herdr{bin: bin}).AgentStart("backend-ab12", "codex", "w1:p1", resumeArgs["codex"]...); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(argv)
	want := "agent start backend-ab12 --kind codex --pane w1:p1 --timeout 30000 -- resume --last"
	if s := strings.Join(strings.Fields(string(got)), " "); s != want {
		t.Fatalf("got %q, want %q", s, want)
	}
}

func TestContextSplitsNoAgentThreads(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	threads := []Thread{
		{Bead: Bead{ID: "backend-wt", Status: StatusInProgress, UpdatedAt: t0.Add(-time.Hour)}},
		{Bead: Bead{ID: "backend-old", Status: StatusInProgress, UpdatedAt: t0.Add(-30 * 24 * time.Hour)}},
		{Bead: Bead{ID: "backend-older", Status: StatusInProgress, UpdatedAt: t0.Add(-40 * 24 * time.Hour)}},
	}
	out := renderContext(Config{Repo: "/r"}, threads, nil, nil, nil, TickerState{}, map[string]bool{"backend-wt": true}, t0)
	for _, want := range []string{
		"- resumable (1): backend-wt  → quartermaster resume\n",
		"- stale claims (2): backend-old, backend-older — no agent, no worktree, no PR, untouched 7d+  → quartermaster stale\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if out := renderContext(Config{Repo: "/r"}, threads, nil, nil, nil, TickerState{}, nil, t0); !strings.Contains(out, "- claimed, no agent (3): ") {
		t.Errorf("unknown worktrees should not call anything stale:\n%s", out)
	}
}

func TestResumeKindResolvesAuto(t *testing.T) {
	cfg := Config{WorkerAgent: "auto"}
	for _, k := range []string{resumeKind(cfg, Bead{Assignee: "tj@example.com"}, ""), resumeKind(cfg, Bead{}, "auto")} {
		if !validKinds[k] {
			t.Fatalf("auto must resolve to a real agent kind, got %q", k)
		}
	}
	if k := resumeKind(cfg, Bead{Assignee: "codex"}, ""); k != "codex" {
		t.Fatalf("the claimer's kind wins over auto, got %q", k)
	}
}

func TestMuseResumesItsLastSession(t *testing.T) {
	if !validKinds["muse"] {
		t.Fatal("muse is not a valid kind")
	}
	if got := strings.Join(resumeArgs["muse"], " "); got != "resume --last" {
		t.Fatalf("muse resume args = %q", got)
	}
	if k := resumeKind(resumeCfg, Bead{Assignee: "muse"}, ""); k != "muse" {
		t.Fatalf("a muse-claimed bead resumes as %q", k)
	}
}

func TestEveryKindResumes(t *testing.T) {
	for _, k := range kinds {
		if _, ok := resumeArgs[k]; !ok {
			t.Errorf("%s has no resume args", k)
		}
	}
}

func TestKindList(t *testing.T) {
	if got := kindList(); got != "claude, codex or muse" {
		t.Errorf("kindList() = %q", got)
	}
	if got := kindList("auto"); got != "claude, codex, muse or auto" {
		t.Errorf(`kindList("auto") = %q`, got)
	}
}

func TestAgentArgs(t *testing.T) {
	var cfg Config
	if got := strings.Join(cfg.agentArgs("muse"), " "); got != "--disable-sandbox --permission-profile :auto-review" {
		t.Errorf("muse default args = %q", got)
	}
	if got := strings.Join(cfg.agentArgs("muse", resumeArgs["muse"]...), " "); got != "--disable-sandbox --permission-profile :auto-review resume --last" {
		t.Errorf("muse resume args = %q", got)
	}
	if got := cfg.agentArgs("claude"); len(got) != 0 {
		t.Errorf("claude has no default args, got %q", got)
	}
	cfg.Agents = map[string]AgentConfig{"muse": {Args: []string{}}, "codex": {Args: []string{"-m", "x"}}}
	if got := cfg.agentArgs("muse"); len(got) != 0 {
		t.Errorf("an empty [agents.muse] args clears the default, got %q", got)
	}
	if got := strings.Join(cfg.agentArgs("codex", resumeArgs["codex"]...), " "); got != "-m x resume --last" {
		t.Errorf("codex args = %q", got)
	}
}

func TestAgentArgsFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QUARTERMASTER_CONFIG_DIR", dir)
	conf := "repo = \"/r\"\n[agents.muse]\nargs = [\"--disable-sandbox\", \"--sandbox-network\", \"enabled\"]\n[agents.claude]\nargs = [\"--model\", \"opus\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.agentArgs("muse"), " "); got != "--disable-sandbox --sandbox-network enabled" {
		t.Errorf("muse args = %q", got)
	}
	if got := strings.Join(cfg.agentArgs("claude", "--continue"), " "); got != "--model opus --continue" {
		t.Errorf("claude args = %q", got)
	}
}

func TestAgentEnvExportsBeadsDirAndActor(t *testing.T) {
	cfg := Config{BeadsDir: "/home/me/it's/.beads"}
	want := `export BEADS_ACTOR='muse' BEADS_DIR='/home/me/it'\''s/.beads'`
	if got := cfg.agentEnv("muse"); got != want {
		t.Fatalf("agentEnv = %s, want %s", got, want)
	}
	out, err := exec.Command("sh", "-c", want+`; printf '%s|%s' "$BEADS_ACTOR" "$BEADS_DIR"`).Output()
	if err != nil || string(out) != "muse|/home/me/it's/.beads" {
		t.Fatalf("the shell read it as %q (%v)", out, err)
	}
}
