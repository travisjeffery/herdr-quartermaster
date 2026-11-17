package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func mappingGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	out, err := adoptionGit(cwd, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func mappingFixture(t *testing.T, cwd string, pid int, ref string) *handoffCheckoutMapping {
	t.Helper()
	owner, err := adoptionIdentity("/proc", pid)
	if err != nil {
		t.Fatal(err)
	}
	return &handoffCheckoutMapping{Head: mappingGit(t, cwd, "rev-parse", "HEAD"), Ref: ref, SourcePID: pid, SourceStart: owner.Start, SourceBoot: owner.Boot, Evidence: "Operator verified original transcript and task notes: this named source inherited the legacy checkout. Pending question remains unanswered."}
}

func TestFreshHandoffMappingAdmission(t *testing.T) {
	cwd := adoptionRepo(t)
	mappingGit(t, cwd, "switch", "-c", "tj/legacy-work")
	transcript := filepath.Join(t.TempDir(), "source-session.jsonl")
	if err := os.WriteFile(transcript, []byte("original history\n"), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	good := mappingFixture(t, cwd, child.Process.Pid, "refs/heads/tj/legacy-work")
	cases := []struct {
		name   string
		mutate func(*handoffCheckoutMapping)
	}{
		{"missing-evidence", func(m *handoffCheckoutMapping) { m.Evidence = " " }},
		{"oversized-evidence", func(m *handoffCheckoutMapping) { m.Evidence = strings.Repeat("x", (32<<10)+1) }},
		{"short-head", func(m *handoffCheckoutMapping) { m.Head = m.Head[:8] }},
		{"stale-head", func(m *handoffCheckoutMapping) { m.Head = strings.Repeat("0", 40) }},
		{"wrong-ref", func(m *handoffCheckoutMapping) { m.Ref = "HEAD" }},
		{"wrong-pid", func(m *handoffCheckoutMapping) { m.SourcePID = os.Getpid() }},
		{"reused-pid", func(m *handoffCheckoutMapping) { m.SourceStart = "0" }},
		{"wrong-boot", func(m *handoffCheckoutMapping) { m.SourceBoot = "other-boot" }},
		{"missing-identity", func(m *handoffCheckoutMapping) { m.SourceStart = "" }},
		{"unlinked", nil}, {"conflicting-agent", nil}, {"reserved-path", nil}, {"pi-source", nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
			t.Setenv("HERDR_PANE_ID", "caller:p1")
			mapping := *good
			if tt.mutate != nil {
				tt.mutate(&mapping)
			}
			in := handoffInput{Cwd: cwd, Transcript: transcript, Context: "Waiting for approval", Mapping: &mapping}
			input := filepath.Join(t.TempDir(), "input.json")
			if err := rpcSave(input, in); err != nil {
				t.Fatal(err)
			}
			source := Agent{Name: "b-1", Kind: "claude", Status: "idle", PaneID: "old:p1", Cwd: cwd, Session: AgentSession{Kind: "id", Value: "source-session"}}
			if tt.name == "pi-source" {
				source.Kind = "pi"
			}
			agents := []Agent{source}
			if tt.name == "conflicting-agent" {
				agents = append(agents, Agent{Name: "b-2", Kind: "claude", Status: "idle", PaneID: "other:p1", Cwd: cwd})
			}
			wts := []Worktree{{Linked: true, Path: cwd}}
			if tt.name == "unlinked" {
				wts = nil
			}
			if tt.name == "reserved-path" {
				if err := rpcPrivateDir(rpcDir("b-2")); err != nil {
					t.Fatal(err)
				}
				if err := rpcSave(filepath.Join(rpcDir("b-2"), "worker.json"), rpcWorker{Bead: "b-2", Cwd: cwd}); err != nil {
					t.Fatal(err)
				}
			}
			h := adoptionHerdr(t, agents, adoptionProcessInfo{ShellPID: os.Getpid(), Procs: []adoptionProcess{{PID: child.Process.Pid, Cwd: cwd}}}, wts)
			cfg := Config{Repo: cwd, BranchPrefix: "tj/", Agents: map[string]AgentConfig{"pi": {Args: []string{}}}}
			b := Bead{ID: "b-1", Status: StatusNeedsMe, Assignee: "claude"}
			if err := handoffPrepare(cfg, h, b, source.PaneID, input); err == nil {
				t.Fatal("invalid mapping admitted")
			}
			if rpcExists(b.ID) || rpcParked(b.ID) {
				t.Fatal("failed prepare left a reservation/park barrier")
			}
			if _, err := adoptionIdentity("/proc", child.Process.Pid); err != nil {
				t.Fatal("source interrupted", err)
			}
		})
	}
}

func TestFreshHandoffMappingPinsCommitAndRef(t *testing.T) {
	cwd := adoptionRepo(t)
	mappingGit(t, cwd, "switch", "--detach", "HEAD")
	m := mappingFixture(t, cwd, os.Getpid(), "HEAD")
	branch, _, err := handoffCheckout(cwd, m)
	if err != nil || branch != "HEAD" {
		t.Fatal(branch, err)
	}
	// Detached status is not inferred from arbitrary Git failure.
	if _, _, err := handoffCheckout(t.TempDir(), m); err == nil {
		t.Fatal("non-repository accepted as detached")
	}
	mappingGit(t, cwd, "-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "new fixture commit")
	if _, _, err := handoffCheckout(cwd, m); err == nil {
		t.Fatal("advanced detached HEAD accepted")
	}
}
