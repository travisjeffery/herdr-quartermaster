package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func serverHost() HostConfig {
	return HostConfig{
		Machine: "server", SSH: "server.local", Cores: 24, RAMGiB: 15,
		Labels: []string{"go-heavy"},
		Repos:  map[string]string{"herdr-quartermaster": "~/src/herdr-quartermaster"},
	}
}

func hostsConfig() Config {
	cfg := defaultConfig()
	cfg.Repo = "/home/u/src/herdr-quartermaster"
	cfg.Repos = map[string]string{"infra": "/home/u/src/infra"}
	cfg.Hosts = map[string]HostConfig{"server": serverHost()}
	return cfg
}

func TestValidateHosts(t *testing.T) {
	off := false
	for _, c := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"valid", func(*Config) {}, ""},
		{"disabled is valid", func(c *Config) { h := c.Hosts["server"]; h.Enabled = &off; c.Hosts["server"] = h }, ""},
		{"manual", func(c *Config) { c.Schedule = "manual" }, ""},
		{"auto not yet", func(c *Config) { c.Schedule = "auto" }, "not implemented"},
		{"bad schedule", func(c *Config) { c.Schedule = "random" }, "want"},
		{"no machine", func(c *Config) { h := c.Hosts["server"]; h.Machine = ""; c.Hosts["server"] = h }, "set machine"},
		{"bad runtime", func(c *Config) { h := c.Hosts["server"]; h.Runtime = "lxc"; c.Hosts["server"] = h }, "runtime"},
		{"relative repo", func(c *Config) {
			h := c.Hosts["server"]
			h.Repos = map[string]string{"x": "src/x"}
			c.Hosts["server"] = h
		}, "absolute or ~/"},
		{"bad name", func(c *Config) { c.Hosts["Server"] = serverHost() }, "host name"},
		{"local with machine", func(c *Config) { c.Hosts["local"] = HostConfig{Machine: "x"} }, "local takes no machine"},
	} {
		cfg := hostsConfig()
		c.edit(&cfg)
		err := cfg.validateHosts()
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

func TestSlots(t *testing.T) {
	cfg := hostsConfig()
	if got := cfg.slots("server"); got != 3 {
		t.Errorf("server: derived %d, want min(24/4, (15-3)/4) = 3", got)
	}
	cfg.Hosts["local"] = HostConfig{Cores: 32, RAMGiB: 30}
	if got := cfg.slots("local"); got != 5 {
		t.Errorf("local: derived %d, want min(32/4, (30-8)/4) = 5", got)
	}
	cfg.RAMPerWorkerGiB = 2
	if got := cfg.slots("server"); got != 6 {
		t.Errorf("server with 2 GiB a worker: %d, want min(6, 6) = 6", got)
	}
	h := cfg.Hosts["server"]
	h.Slots = 2
	cfg.Hosts["server"] = h
	if got := cfg.slots("server"); got != 2 {
		t.Errorf("configured slots: %d", got)
	}
	cfg.Hosts["tiny"] = HostConfig{Machine: "tiny", SSH: "tiny", Cores: 2, RAMGiB: 4}
	if got := cfg.slots("tiny"); got != 1 {
		t.Errorf("a host smaller than one worker still gets one slot: %d", got)
	}
}

func TestHostFor(t *testing.T) {
	off := false
	cfg := hostsConfig()
	cfg.Hosts["off"] = HostConfig{Machine: "off", SSH: "off", Enabled: &off, Repos: serverHost().Repos}
	cfg.RepoNeeds = map[string][]string{"infra": {"aws"}}
	bead := func(labels ...string) Bead { return Bead{ID: "b-1", Labels: labels} }
	for _, c := range []struct {
		name     string
		b        Bead
		explicit string
		force    bool
		want     string
		err      string
	}{
		{"default local", bead(), "", false, "local", ""},
		{"local carries every label", bead("needs:aws"), "", false, "local", ""},
		{"explicit", bead(), "server", false, "server", ""},
		{"sticky", bead("host:server"), "", false, "server", ""},
		{"explicit moves a sticky bead", bead("host:server"), "local", false, "local", ""},
		{"unknown", bead(), "nope", false, "", `no host "nope"`},
		{"disabled", bead(), "off", false, "", "off: disabled"},
		{"repo missing on host", bead("repo:infra"), "server", false, "", "no repo infra"},
		{"needs label", bead("needs:aws"), "server", false, "", "lacks label aws"},
		{"force skips labels", bead("needs:aws"), "server", true, "server", ""},
		{"has label", bead("needs:go-heavy"), "server", false, "server", ""},
	} {
		got, err := cfg.hostFor(c.b, c.explicit, c.force)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: got %q, %v; want error %q", c.name, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	// repo_needs apply to local too once local lists its labels.
	cfg.Hosts["local"] = HostConfig{Labels: []string{"go-heavy"}}
	if _, err := cfg.hostFor(bead("repo:infra"), "", false); err == nil || !strings.Contains(err.Error(), "lacks label aws") {
		t.Errorf("repo_needs on a labelled local: %v", err)
	}
}

func TestHostExec(t *testing.T) {
	cfg := hostsConfig()
	if got := cfg.hostExec("local", "git", "-C", "/r", "fetch").Args; !slices.Equal(got, []string{"git", "-C", "/r", "fetch"}) {
		t.Errorf("local: %q", got)
	}
	got := cfg.hostExec("server", "git", "-C", "~/src/my repo", "branch", "-D", "tj/it's").Args
	want := []string{"ssh", "-o", "BatchMode=yes", "server.local", `podman exec -i 'qm-worker' 'git' '-C' ~/'src/my repo' 'branch' '-D' 'tj/it'\''s'`}
	if !slices.Equal(got, want) {
		t.Errorf("remote:\n got %q\nwant %q", got, want)
	}
}

func TestRepoOn(t *testing.T) {
	cfg := hostsConfig()
	if p, ok := cfg.repoOn("server", Bead{}); !ok || p != "~/src/herdr-quartermaster" {
		t.Errorf("default repo by directory name: %q %v", p, ok)
	}
	if p, ok := cfg.repoOn("local", Bead{Labels: []string{"repo:infra"}}); !ok || p != "/home/u/src/infra" {
		t.Errorf("local: %q %v", p, ok)
	}
}

func TestAgentEnvOnRemoteLeavesBeadsDirToProxy(t *testing.T) {
	cfg := Config{BeadsDir: "/home/u/.beads"}
	if got := cfg.agentEnvOn("server", "claude"); got != "export BEADS_ACTOR='claude'" {
		t.Errorf("remote: %q", got)
	}
	if got := cfg.agentEnv("claude"); !strings.Contains(got, "BEADS_DIR='/home/u/.beads'") {
		t.Errorf("local: %q", got)
	}
}

// fakeTools puts fake herdr, bd and ssh first on PATH. Each logs its argv,
// one call per line, to <name>.log in the returned dir. herdr answers agent
// list on a remote machine with $FAKE_REMOTE_AGENTS and locally with
// $FAKE_LOCAL_AGENTS; bd show prints $FAKE_BEAD.
func fakeTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"herdr": `echo "$*" >> "$LOG/herdr.log"
remote=
if [ "$1" = --machine ]; then remote=1; shift 2; fi
case "$1 $2" in
"agent list") if [ -n "$remote" ]; then echo "{\"result\":{\"agents\":${FAKE_REMOTE_AGENTS:-[]}}}"; else echo "{\"result\":{\"agents\":${FAKE_LOCAL_AGENTS:-[]}}}"; fi ;;
"worktree create") echo '{"result":{"workspace":{"workspace_id":"w9"},"worktree":{"path":"/home/u/worktrees/x"},"root_pane":{"pane_id":"w9:p1"}}}' ;;
*) echo '{"result":{}}' ;;
esac
`,
		"bd": `echo "$*" >> "$LOG/bd.log"
case "$1" in show) echo "$FAKE_BEAD" ;; esac
`,
		"ssh": `echo "$*" >> "$LOG/ssh.log"
`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOG", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERDR_BIN_PATH", filepath.Join(dir, "herdr"))
	t.Setenv("QUARTERMASTER_STATE_DIR", t.TempDir())
	t.Setenv("QUARTERMASTER_CONFIG_DIR", t.TempDir())
	return dir
}

func logLines(t *testing.T, dir, name string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, name+".log"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func hasLine(lines []string, prefix string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

func TestHerdrOnMachine(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("FAKE_REMOTE_AGENTS", `[{"name":"b-1","pane_id":"w1:p1"}]`)
	agents, err := hostsConfig().herdrOn(newHerdr(), "server").Agents()
	if err != nil || len(agents) != 1 || agents[0].Host != "server" {
		t.Fatalf("got %+v, %v", agents, err)
	}
	if got := logLines(t, dir, "herdr"); got[0] != "--machine server agent list" {
		t.Fatalf("herdr ran with %q", got[0])
	}
	local, _ := hostsConfig().herdrOn(newHerdr(), "local").Agents()
	if got := logLines(t, dir, "herdr"); got[1] != "agent list" || len(local) != 0 {
		t.Fatalf("local ran with %q", got[1])
	}
}

func TestDispatchToRemoteHost(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open","labels":["host:old"]}]`)
	cfg := hostsConfig()
	cfg.BeadsDir = "/home/u/.beads"
	cfg.Hosts["old"] = HostConfig{Machine: "old", SSH: "old", Repos: serverHost().Repos}
	msg, err := dispatch(cfg, newHerdr(), "b-1", DispatchOpts{Host: "server"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "w9:p1 on server") {
		t.Errorf("message: %q", msg)
	}
	herdr := logLines(t, dir, "herdr")
	for _, want := range []string{
		"--machine server worktree create --cwd ~/src/herdr-quartermaster --branch tj/b-1-do-it --base main --label b-1",
		"--machine server pane run w9:p1 export BEADS_ACTOR='claude'",
		"--machine server agent start b-1 --kind claude --pane w9:p1",
		"--machine server agent prompt b-1 You are the quartermaster worker for bead b-1",
		"--machine server tab rename w9:t1",
		"--machine old agent list",
		"agent list",
	} {
		if !hasLine(herdr, want) {
			t.Errorf("no herdr call %q in\n%s", want, strings.Join(herdr, "\n"))
		}
	}
	if hasLine(herdr, "--machine server pane run w9:p1 export BEADS_ACTOR='claude' BEADS_DIR") {
		t.Error("BEADS_DIR exported on a remote host")
	}
	if ssh := logLines(t, dir, "ssh"); !hasLine(ssh, "-o BatchMode=yes server.local podman exec -i 'qm-worker' 'git' '-C' ~/'src/herdr-quartermaster' 'fetch' 'origin' 'main'") {
		t.Errorf("no fetch before worktree create: %q", ssh)
	}
	bd := logLines(t, dir, "bd")
	for _, want := range []string{"update b-1 --claim", "label remove b-1 host:old", "label add b-1 host:server"} {
		if !slices.Contains(bd, want) {
			t.Errorf("no bd %q in %q", want, bd)
		}
	}
}

func TestDispatchLocalAddsNoHostLabel(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("FAKE_BEAD", `[{"id":"b-1","title":"Do it","status":"open"}]`)
	if _, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{}); err != nil {
		t.Fatal(err)
	}
	if bd := logLines(t, dir, "bd"); hasLine(bd, "label") {
		t.Errorf("local dispatch labelled the bead: %q", bd)
	}
	if !hasLine(logLines(t, dir, "herdr"), "worktree create --cwd /home/u/src/herdr-quartermaster") {
		t.Error("local worktree not created from the local repo")
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh.log")); err == nil {
		t.Error("local dispatch used ssh")
	}
}

func TestDispatchFindsAgentOnAnyHost(t *testing.T) {
	fakeTools(t)
	t.Setenv("FAKE_REMOTE_AGENTS", `[{"name":"b-1","pane_id":"w3:p1"}]`)
	msg, err := dispatch(hostsConfig(), newHerdr(), "b-1", DispatchOpts{})
	if err != nil || msg != "b-1 already has an agent (w3:p1) on server" {
		t.Fatalf("got %q, %v", msg, err)
	}
}

func TestDeliverOutboxOnRemoteHost(t *testing.T) {
	dir := fakeTools(t)
	t.Setenv("FAKE_REMOTE_AGENTS", `[{"name":"b-1","pane_id":"w3:p1","agent_status":"idle"}]`)
	if err := queueBriefOn("server", "b-1", "brief"); err != nil {
		t.Fatal(err)
	}
	if err := queueBrief("b-2", "local brief"); err != nil {
		t.Fatal(err)
	}
	sent := deliverOutbox(hostsConfig(), newHerdr(), map[string]Agent{"b-2": {Name: "b-2", PaneID: "w1:p1", Status: "done"}})
	if !slices.Equal(sent, []string{"b-2", "b-1 on server"}) {
		t.Fatalf("sent %q", sent)
	}
	herdr := logLines(t, dir, "herdr")
	if !slices.Contains(herdr, "--machine server agent prompt w3:p1 brief") || !slices.Contains(herdr, "agent prompt w1:p1 local brief") {
		t.Fatalf("prompts: %q", herdr)
	}
	if _, err := os.Stat(filepath.Join(outboxDir(), "server", "b-1.md")); err == nil {
		t.Fatal("delivered brief left in the outbox")
	}
}

func TestAgentEnvAppendsConfiguredEnv(t *testing.T) {
	cfg := Config{BeadsDir: "/home/u/.beads", Agents: map[string]AgentConfig{
		"claude": {Env: []string{"OTEL_LOGS_EXPORTER=otlp", "ATTRS=a=1,b='2'"}},
	}}
	want := `export BEADS_ACTOR='claude' BEADS_DIR='/home/u/.beads' OTEL_LOGS_EXPORTER='otlp' ATTRS='a=1,b='\''2'\'''`
	if got := cfg.agentEnv("claude"); got != want {
		t.Errorf("local:\n got %s\nwant %s", got, want)
	}
	if got := cfg.agentEnvOn("server", "claude"); got != `export BEADS_ACTOR='claude' OTEL_LOGS_EXPORTER='otlp' ATTRS='a=1,b='\''2'\'''` {
		t.Errorf("remote: %s", got)
	}
	if got := cfg.agentEnvOn("server", "codex"); got != "export BEADS_ACTOR='codex'" {
		t.Errorf("other kind: %s", got)
	}
}

func TestValidateAgentsEnv(t *testing.T) {
	for _, tc := range []struct {
		env []string
		ok  bool
	}{
		{[]string{"A=1", "B_2="}, true},
		{[]string{"NOEQUALS"}, false},
		{[]string{"A;rm -rf ~=1"}, false},
		{[]string{"1A=x"}, false},
		{[]string{"BEADS_DIR=/x"}, false},
		{[]string{"BEADS_ACTOR=x"}, false},
	} {
		cfg := Config{Agents: map[string]AgentConfig{"claude": {Env: tc.env}}}
		if err := cfg.validateAgents(); (err == nil) != tc.ok {
			t.Errorf("%q: err %v, want ok=%v", tc.env, err, tc.ok)
		}
	}
}
