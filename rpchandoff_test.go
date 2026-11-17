package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestFreshHandoffCheckpointAndExit(t *testing.T) {
	for _, mode := range []string{"claude", "codex", "muse", "legacy-branch", "detached"} {
		t.Run(mode, func(t *testing.T) {
			kind := mode
			if mode == "legacy-branch" || mode == "detached" {
				kind = "claude"
			}
			t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
			t.Setenv("HERDR_PANE_ID", "coordinator:p1")
			cwd := adoptionRepo(t)
			if mode == "legacy-branch" {
				mappingGit(t, cwd, "switch", "-c", "tj/legacy-work")
			} else if mode == "detached" {
				mappingGit(t, cwd, "switch", "--detach", "HEAD")
				if _, _, err := adoptionDigest(cwd); err == nil {
					t.Fatal("strict native adoption accepted detached HEAD")
				}
			}
			transcript := filepath.Join(t.TempDir(), "original-id.jsonl")
			os.WriteFile(transcript, []byte("original history\n"), 0600)
			untracked := filepath.Join(cwd, "notes")
			os.WriteFile(untracked, []byte("preserve dirty bytes"), 0600)
			input := filepath.Join(t.TempDir(), "handoff.json")
			in := handoffInput{Cwd: cwd, Transcript: transcript, Context: "Question: may I deploy? Unanswered. No deployment authorized."}
			rpcSave(input, in)
			child := exec.Command("sleep", "60")
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { child.Process.Kill(); child.Wait() }()
			a := Agent{Name: "b-1", Kind: kind, Status: "blocked", PaneID: "old:p1", Cwd: cwd, Session: AgentSession{Kind: "id", Agent: kind, Value: "original-id"}}
			info := adoptionProcessInfo{ShellPID: os.Getpid(), Procs: []adoptionProcess{{PID: child.Process.Pid, Cwd: cwd}}}
			h := adoptionHerdr(t, []Agent{a}, info, []Worktree{{Linked: true, Path: cwd}})
			cfg := Config{Repo: cwd, BranchPrefix: "tj/", Agents: map[string]AgentConfig{"pi": {Args: []string{}}}}
			b := Bead{ID: "b-1", Status: StatusNeedsMe, Assignee: kind, Notes: "Pending permission remains unanswered"}
			if mode == "legacy-branch" || mode == "detached" {
				if err := handoffPrepare(cfg, h, b, a.PaneID, input); err == nil || rpcExists(b.ID) {
					t.Fatal("unmapped legacy/detached checkout admitted", err)
				}
				ref := "refs/heads/tj/legacy-work"
				if mode == "detached" {
					ref = "HEAD"
				}
				in.Mapping = mappingFixture(t, cwd, child.Process.Pid, ref)
				rpcSave(input, in)
			}
			if err := handoffPrepare(cfg, h, b, a.PaneID, input); err != nil {
				t.Fatal(err)
			}
			var w rpcWorker
			rpcLoad(filepath.Join(rpcDir(b.ID), "worker.json"), &w)
			if w.Adoption != nil || w.Handoff == nil || w.Handoff.Checkpoint.Ready || !rpcParked(b.ID) || !strings.Contains(w.Handoff.Context, b.Notes) {
				t.Fatalf("checkpoint: %+v", w)
			}
			if in.Mapping != nil && (w.Handoff.Mapping == nil || *w.Handoff.Mapping != *in.Mapping || !strings.Contains(w.Handoff.Context, in.Mapping.Evidence)) {
				t.Fatal("mapping evidence not persisted in reservation/context")
			}
			if !adoptionHeldAgent(a) || !rpcReservedPath(cwd) {
				t.Fatal("reservation did not hold legacy delivery/sweep")
			}
			if err := handoffPrepare(cfg, h, b, a.PaneID, input); err == nil {
				t.Fatal("overwrote reservation")
			}
			if err := rpcServe(cfg, b.ID); err == nil || !strings.Contains(err.Error(), "prepared") {
				t.Fatal(err)
			}
			if err := handoffValidate(h, b, w); err == nil || !strings.Contains(err.Error(), "still alive") {
				t.Fatalf("live owner: %v", err)
			}
			if err := child.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			child.Wait()
			h = adoptionHerdr(t, nil, adoptionProcessInfo{ShellPID: os.Getpid(), Procs: []adoptionProcess{{PID: os.Getpid()}}}, nil)
			if err := handoffValidate(h, b, w); err != nil {
				t.Fatal(err)
			}
			changed := b
			changed.Status = StatusInProgress
			if err := handoffValidate(h, changed, w); err == nil {
				t.Fatal("blocker silently cleared")
			}
			w.Handoff.Checkpoint.Shell.Start = "reused"
			if err := handoffValidate(h, b, w); err == nil {
				t.Fatal("reused shell accepted")
			}
			w.Handoff.Checkpoint.Shell, _ = adoptionIdentity("/proc", os.Getpid())
			os.WriteFile(untracked, []byte("changed bytes"), 0600)
			if err := handoffValidate(h, b, w); err == nil {
				t.Fatal("changed checkout accepted")
			}
			os.WriteFile(untracked, []byte("preserve dirty bytes"), 0600)
			os.WriteFile(transcript, []byte("changed history\n"), 0600)
			if err := handoffValidate(h, b, w); err == nil {
				t.Fatal("lost transcript accepted")
			}
			os.WriteFile(transcript, []byte("original history\n"), 0600)
			if in.Mapping != nil {
				// Same commit/files, different attached/detached identity.
				mappingGit(t, cwd, "switch", "-c", "tj/rebound")
				if err := handoffValidate(h, b, w); err == nil {
					t.Fatal("changed ref accepted")
				}
			}
		})
	}
}

func TestFreshHandoffSeedIsDurableAndNeverOverwritten(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
	w := rpcWorker{Bead: "b-1", Cwd: t.TempDir(), Handoff: &rpcHandoff{Context: "Unanswered question; no approval"}}
	file, err := handoffSeed(w)
	if err != nil {
		t.Fatal(err)
	}
	id, _, _, err := adoptionSession(file, w.Cwd)
	if err != nil || id != w.Bead {
		t.Fatalf("fresh v3 session: %s %v", id, err)
	}
	before, _ := os.ReadFile(file)
	if !strings.Contains(string(before), "quartermaster.handoff") || strings.Contains(string(before), "assistant") {
		t.Fatal("not a context-only seed")
	}
	if _, err := handoffSeed(w); err == nil {
		t.Fatal("overwrote existing session")
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("seed changed")
	}
	if _, err := rpcPreviousStatus(w.Bead); err == nil {
		t.Fatal("interrupted seed mistaken for new launch")
	}
}

func TestFreshHandoffAdmissionAndExclusions(t *testing.T) {
	for _, status := range []string{"unknown", StatusClosed} {
		if err := handoffPrepare(Config{}, Herdr{}, Bead{ID: "b-1", Status: status}, "p", "absent"); err == nil {
			t.Fatal("unknown/closed task accepted")
		}
	}
	for _, kind := range []string{"pi", "claude"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
			t.Setenv("HERDR_PANE_ID", "self:p1")
			file := filepath.Join(t.TempDir(), "in.json")
			rpcSave(file, handoffInput{Cwd: "/checkout", Transcript: "/session", Context: "Waiting"})
			source := Agent{Name: "b-1", Kind: kind, PaneID: "old:p1", Status: "working"}
			h := adoptionHerdr(t, []Agent{source}, adoptionProcessInfo{}, nil)
			b := Bead{ID: "b-1", Status: StatusInProgress}
			if err := handoffPrepare(Config{}, h, b, source.PaneID, file); err == nil {
				t.Fatal("busy/pi accepted")
			}
			source.PaneID = "self:p1"
			source.Status = "idle"
			h = adoptionHerdr(t, []Agent{source}, adoptionProcessInfo{}, nil)
			if err := handoffPrepare(Config{}, h, b, source.PaneID, file); err == nil || !strings.Contains(err.Error(), "caller") {
				t.Fatal(err)
			}
			source.PaneID = "old:p1"
			h = adoptionHerdr(t, []Agent{source}, adoptionProcessInfo{}, nil)
			if err := handoffPrepare(Config{CoordinatorName: "b-1"}, h, b, source.PaneID, file); err == nil || !strings.Contains(err.Error(), "coordinator") {
				t.Fatal(err)
			}
			lock, err := adoptionAdmission(false)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := handoffPrepare(Config{}, h, b, source.PaneID, file); err == nil || !strings.Contains(err.Error(), "admission busy") {
				t.Fatal(err)
			}
		})
	}
}

func TestFreshHandoffSessionCensus(t *testing.T) {
	root := t.TempDir()
	boot := filepath.Join(root, "sys/kernel/random/boot_id")
	os.MkdirAll(filepath.Dir(boot), 0700)
	os.WriteFile(boot, []byte("boot"), 0600)
	proc := func(pid, sid int, start string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		os.MkdirAll(dir, 0700)
		fields := make([]string, 20)
		for i := range fields {
			fields[i] = "0"
		}
		fields[0] = "S"
		fields[3] = strconv.Itoa(sid)
		fields[19] = start
		os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (fixture) %s", pid, strings.Join(fields, " "))), 0600)
		// No readable cwd at all. Only exact source sessions are in this contract.
	}
	a := &rpcAdoption{ShellPID: 500, Owners: []adoptionOwner{{PID: 100, Start: "10", Boot: "boot", Session: 500, SessionStart: "5"}}}
	proc(600, 600, "20")
	if err := handoffRemaining(root, a); err != nil {
		t.Fatalf("unrelated protected service: %v", err)
	}
	proc(100, 500, "10")
	if err := handoffRemaining(root, a); err == nil {
		t.Fatal("live source")
	}
	os.Remove(filepath.Join(root, "100", "stat"))
	proc(110, 500, "11")
	if err := handoffRemaining(root, a); err == nil {
		t.Fatal("late orphan in source session")
	}
	proc(500, 500, "30") // session leader PID reused, old session ended
	if err := handoffRemaining(root, a); err != nil {
		t.Fatalf("reused session: %v", err)
	}
}

func TestFreshHandoffSubmoduleBytes(t *testing.T) {
	root := adoptionRepo(t)
	sub := adoptionRepo(t)
	out, err := exec.Command("git", "-C", root, "-c", "protocol.file.allow=always", "submodule", "add", sub, "nested").CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, _, err := adoptionDigest(root); err == nil {
		t.Fatal("strict adoption changed")
	}
	_, before, err := handoffDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "nested", "tracked"), []byte("dirty submodule"), 0600)
	_, after, err := handoffDigest(root)
	if err != nil || before == after {
		t.Fatalf("lost submodule bytes: %v", err)
	}
}

func TestFreshHandoffContextAndParking(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "worker.json")
	park := filepath.Join(dir, "parked")
	context := "Question: approve deployment? Unanswered; no task action authorized."
	rpcSave(file, rpcWorker{Handoff: &rpcHandoff{Context: context}})
	os.WriteFile(park, []byte("parked"), 0600)
	t.Setenv("QUARTERMASTER_RPC_HANDOFF_FILE", file)
	t.Setenv("QUARTERMASTER_RPC_PARK_FILE", park)
	t.Setenv("QUARTERMASTER_RPC", "1")
	t.Setenv("QM_TEST_PI_MODE", "rpc")
	entries, _ := json.Marshal([]map[string]any{{"type": "custom_message", "customType": "quartermaster.handoff", "content": context}})
	t.Setenv("QM_TEST_PI_ENTRIES", string(entries))
	h := startHarness(t, "b-1")
	c, _ := piControlFor("b-1")
	ping, err := c.call(piRequest{Op: "ping"})
	if err != nil || ping.HandoffHash != fmt.Sprintf("%x", sha256.Sum256([]byte(context))) {
		t.Fatalf("handoff not in context: %+v %v", ping, err)
	}
	h.send("reload")
	r, _ := h.next(func(m map[string]any) bool { return m["reload"] == true })
	if r["entries"] != float64(1) {
		t.Fatal("handoff duplicated", r)
	}
	if _, err := c.Prompt("continue"); err == nil {
		t.Fatal("park bypass")
	}
	h.send("tool echo harmless")
	r, _ = h.next(func(m map[string]any) bool { return m["tool"] != nil })
	if r["block"] != true {
		t.Fatal("tool park bypass", r)
	}
}
