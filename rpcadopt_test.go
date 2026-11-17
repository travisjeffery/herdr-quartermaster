package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func adoptionRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	git("init", "-b", "tj/b-1-work")
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	return dir
}
func TestRPCAdoptionCheckoutFingerprint(t *testing.T) {
	dir := adoptionRepo(t)
	branch, original, err := adoptionDigest(dir)
	if err != nil || branch != "tj/b-1-work" {
		t.Fatalf("%s %v", branch, err)
	}
	p := filepath.Join(dir, "tracked")
	os.WriteFile(p, []byte("staged\n"), 0600)
	exec.Command("git", "-C", dir, "add", "tracked").Run()
	os.WriteFile(p, []byte("base\n"), 0600) // diff HEAD is empty, index is not
	_, staged, err := adoptionDigest(dir)
	if err != nil || staged == original {
		t.Fatalf("lost staged difference: %v", err)
	}
	p = filepath.Join(dir, "untracked file\nwith newline")
	os.WriteFile(p, []byte("one"), 0600)
	_, first, _ := adoptionDigest(dir)
	os.WriteFile(p, []byte("two"), 0600)
	_, second, _ := adoptionDigest(dir)
	if first == second {
		t.Fatal("untracked content not fingerprinted")
	}
	os.Chmod(p, 0700)
	_, mode, _ := adoptionDigest(dir)
	if mode == second {
		t.Fatal("untracked mode not fingerprinted")
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Fatal("snapshot modified files")
	}
}
func TestRPCAdoptionTrackedBytesIgnoreGitFlags(t *testing.T) {
	dir := adoptionRepo(t)
	if out, err := exec.Command("git", "-C", dir, "update-index", "--assume-unchanged", "tracked").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	_, before, err := adoptionDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "tracked"), []byte("different raw bytes\n"), 0600)
	status, err := adoptionGit(dir, "status", "--porcelain")
	if err != nil || len(status) != 0 {
		t.Fatalf("fixture not hidden: %s %v", status, err)
	}
	_, after, err := adoptionDigest(dir)
	if err != nil || before == after {
		t.Fatalf("tracked bytes hidden by Git: %v", err)
	}
	os.Chmod(filepath.Join(dir, "tracked"), 0700)
	_, mode, err := adoptionDigest(dir)
	if err != nil || mode == after {
		t.Fatalf("tracked mode hidden by Git: %v", err)
	}
}

func TestRPCAdoptionSessionIdentityAndPrefix(t *testing.T) {
	cwd := t.TempDir()
	file := filepath.Join(t.TempDir(), "session.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "b-1", "cwd": cwd})
	original := append(header, '\n')
	os.WriteFile(file, original, 0600)
	id, n, hash, err := adoptionSession(file, cwd)
	if err != nil || id != "b-1" {
		t.Fatal(err)
	}
	a := &rpcAdoption{SessionFile: file, SessionSize: n, SessionHash: hash}
	os.WriteFile(file, append(bytes.Clone(original), []byte("{\"type\":\"custom\"}\n")...), 0600)
	if err := adoptionPrefix(a); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := adoptionSession(file, "/other"); err == nil {
		t.Fatal("wrong cwd accepted")
	}
	os.WriteFile(file, []byte("truncated\n"), 0600)
	if adoptionPrefix(a) == nil {
		t.Fatal("history loss accepted")
	}
	os.WriteFile(file, bytes.Replace(original, []byte("b-1"), []byte("b-2"), 1), 0600)
	if adoptionPrefix(a) == nil {
		t.Fatal("changed identity accepted")
	}
}

func adoptionHerdr(t *testing.T, agents []Agent, info adoptionProcessInfo, wts []Worktree) Herdr {
	t.Helper()
	dir := t.TempDir()
	for name, v := range map[string]any{"agents": map[string]any{"result": map[string]any{"agents": agents}}, "process": map[string]any{"result": map[string]any{"process_info": info}}, "worktrees": map[string]any{"result": map[string]any{"worktrees": wts}}} {
		data, _ := json.Marshal(v)
		os.WriteFile(filepath.Join(dir, name), data, 0600)
	}
	file := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n'agent list') cat " + shellQuote(filepath.Join(dir, "agents")) + ";;\n'pane process-info') cat " + shellQuote(filepath.Join(dir, "process")) + ";;\n'worktree list') cat " + shellQuote(filepath.Join(dir, "worktrees")) + ";;\n*) exit 1;;\nesac\n"
	os.WriteFile(file, []byte(script), 0700)
	return Herdr{bin: file}
}
func TestRPCAdoptionOwnershipAndParking(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
	lease, err := rpcLease("b-1")
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	shell, err := adoptionIdentity("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	w := rpcWorker{Bead: "b-1", Cwd: "/fixture/worktree", Adoption: &rpcAdoption{Source: Agent{Name: "b-1", PaneID: "w1:p1"}, ShellPID: os.Getpid(), Shell: shell}}
	h := adoptionHerdr(t, nil, adoptionProcessInfo{ShellPID: os.Getpid(), Procs: []adoptionProcess{{PID: os.Getpid()}}}, nil)
	if err := adoptionStopped(h, w); err != nil {
		t.Fatal(err)
	}
	w.Adoption.Shell.Start = "reused-pid"
	if err := adoptionStopped(h, w); err == nil || !strings.Contains(err.Error(), "shell identity changed") {
		t.Fatalf("reused shell PID: %v", err)
	}
	w.Adoption.Shell = shell
	w.Adoption.PIDs = []int{os.Getpid()}
	if adoptionStopped(h, w) == nil {
		t.Fatal("live owner accepted")
	}
	w.Adoption.PIDs = nil
	bad := adoptionHerdr(t, []Agent{{Kind: "pi", Name: "someone", Cwd: w.Cwd}}, adoptionProcessInfo{}, nil)
	if adoptionStopped(bad, w) == nil {
		t.Fatal("another worktree owner accepted")
	}
	bad = adoptionHerdr(t, nil, adoptionProcessInfo{ShellPID: 124, Procs: []adoptionProcess{{PID: 124}}}, nil)
	if adoptionStopped(bad, w) == nil {
		t.Fatal("reused pane accepted")
	}
	if adoptionStopped(Herdr{bin: "/missing-herdr"}, w) == nil {
		t.Fatal("Herdr failure accepted as exit evidence")
	}
	rpcSave(filepath.Join(rpcDir("b-1"), "worker.json"), w)
	if !rpcReservedPath(w.Cwd) {
		t.Fatal("adopted external worktree not protected from sweep")
	}
	if !adoptionHeldAgent(w.Adoption.Source) {
		t.Fatal("legacy prompt not held")
	}
	if err := h.Prompt("w1:p1", "continue"); err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("legacy pane fallback: %v", err)
	}
	if _, err := resumeOne(Config{}, h, Resumable{Bead: Bead{ID: "b-1"}}, "pi"); err == nil {
		t.Fatal("TUI resume accepted reserved bead")
	}
	if err := rpcServe(Config{}, "b-1"); err == nil || !strings.Contains(err.Error(), "prepared") {
		t.Fatalf("prepared reservation launched: %v", err)
	}
	rpcSave(rpcParkPath("b-1"), true)
	s := rpcSupervisor{worker: w, status: rpcStatus{Phase: "idle"}}
	if r := s.handle(rpcRequest{Op: "status"}); r.Status.Phase != "parked" {
		t.Fatalf("status: %+v", r)
	}
	if r := s.handle(rpcRequest{Op: "prompt", ID: "cannot-run", Text: "continue"}); r.OK {
		t.Fatal("parked prompt accepted")
	}
	if _, err := os.Stat(filepath.Join(rpcDir("b-1"), "requests", "cannot-run.json")); !os.IsNotExist(err) {
		t.Fatal("parked prompt journaled as delivered")
	}
}
func TestRPCAdoptionPrepareGuards(t *testing.T) {
	for _, tc := range []struct{ name, kind, state, sessionKind string }{
		{"claude", "claude", "idle", "id"}, {"codex", "codex", "idle", "path"}, {"muse", "muse", "done", "path"},
		{"busy", "pi", "working", "path"}, {"approval", "pi", "blocked", "path"}, {"unknown", "pi", "unknown", "path"}, {"no-path", "pi", "idle", "id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
			t.Setenv("HERDR_PANE_ID", "coordinator:p1")
			a := Agent{Name: "b-1", Kind: tc.kind, Status: tc.state, PaneID: "w1:p1", Session: AgentSession{Agent: tc.kind, Kind: tc.sessionKind}}
			h := adoptionHerdr(t, []Agent{a}, adoptionProcessInfo{}, nil)
			err := rpcPrepare(Config{}, h, Bead{ID: "b-1", Status: StatusInProgress}, a.PaneID)
			if err == nil || !strings.Contains(err.Error(), "only idle/done pi") {
				t.Fatalf("guard: %v", err)
			}
			if rpcExists("b-1") {
				t.Fatal("refusal reserved worker")
			}
		})
	}
	t.Run("caller", func(t *testing.T) {
		t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
		t.Setenv("HERDR_PANE_ID", "w1:p1")
		h := adoptionHerdr(t, []Agent{{Name: "b-1", PaneID: "w1:p1"}}, adoptionProcessInfo{}, nil)
		if err := rpcPrepare(Config{}, h, Bead{ID: "b-1", Status: StatusInProgress}, "w1:p1"); err == nil || !strings.Contains(err.Error(), "caller") {
			t.Fatal(err)
		}
	})
	for _, status := range []string{StatusOpen, StatusClosed, "unknown"} {
		t.Run(status, func(t *testing.T) {
			if err := rpcPrepare(Config{}, Herdr{}, Bead{ID: "b-1", Status: status}, "w1:p1"); err == nil {
				t.Fatal("non-active bead adopted")
			}
		})
	}
}
func TestRPCAdoptionPrepareCheckpoint(t *testing.T) {
	startHarness(t, "b-1")
	t.Setenv("HERDR_PANE_ID", "coordinator:p1")
	cwd := adoptionRepo(t)
	file := filepath.Join(t.TempDir(), "session.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "b-1", "cwd": cwd})
	os.WriteFile(file, append(header, '\n'), 0600)
	cfg := Config{Repo: cwd, BranchPrefix: "tj/", Agents: map[string]AgentConfig{"pi": {Args: []string{}}}}
	args, err := rpcLaunchArgs(cfg, "b-1")
	if err != nil {
		t.Fatal(err)
	}
	argv := append(append([]string{"pi"}, args[:len(args)-2]...), piSessionArgs("b-1")...)
	source := Agent{Name: "b-1", Kind: "pi", Status: "idle", Cwd: cwd, PaneID: "w1:p1", Session: AgentSession{Agent: "pi", Kind: "path", Value: file}}
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	info := adoptionProcessInfo{ShellPID: os.Getpid(), Procs: []adoptionProcess{{PID: child.Process.Pid, Argv: argv, Cwd: cwd}}}
	h := adoptionHerdr(t, []Agent{source}, info, []Worktree{{Linked: true, Path: cwd, Branch: "tj/b-1-work"}})
	b := Bead{ID: "b-1", Status: StatusNeedsMe, Assignee: "pi", Notes: "RUN: forbidden-command; awaiting TJ"}
	if err := rpcPrepare(cfg, h, b, source.PaneID); err != nil {
		t.Fatal(err)
	}
	var w rpcWorker
	if err := rpcLoad(filepath.Join(rpcDir(b.ID), "worker.json"), &w); err != nil {
		t.Fatal(err)
	}
	if w.Adoption.Ready || w.Adoption.Bead.Status != StatusNeedsMe || w.Adoption.Bead.Notes != b.Notes || !rpcParked(b.ID) {
		t.Fatalf("checkpoint lost blocker: %+v", w)
	}
	if err := rpcPrepare(cfg, h, b, source.PaneID); err == nil {
		t.Fatal("duplicate preparation replaced checkpoint")
	}
	if err := rpcAdopt(cfg, h, b); err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("live worker adopted: %v", err)
	}
	b.Status = StatusInProgress
	if err := rpcAdopt(cfg, h, b); err == nil || !strings.Contains(err.Error(), "status/owner changed") {
		t.Fatalf("blocker change not checked: %v", err)
	}
}

func TestRPCAdoptionParkingGuard(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "parked")
	os.WriteFile(marker, []byte("parked"), 0600)
	t.Setenv("QUARTERMASTER_RPC_PARK_FILE", marker)
	h := startHarness(t, "b-1")
	c, _ := piControlFor("b-1")
	if _, err := c.Prompt("continue task"); err == nil {
		t.Fatal("control socket bypassed parking")
	}
	h.send("tool echo harmless")
	r, _ := h.next(func(m map[string]any) bool { return m["tool"] != nil })
	if r["block"] != true || !strings.Contains(r["reason"].(string), "parked") {
		t.Fatal(r)
	}
	os.Remove(marker)
	if _, err := c.Prompt("explicitly activated"); err != nil {
		t.Fatal(err)
	}
}
func TestRPCAdoptionSavedSessionStartsParkedAndResumes(t *testing.T) {
	launch := fixtureSupervisor(t)
	file := filepath.Join(stateDir(), "existing-session.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "actual-pi-id", "cwd": stateDir()})
	original := append(header, '\n')
	os.WriteFile(file, original, 0600)
	sid, size, hash, err := adoptionSession(file, stateDir())
	if err != nil {
		t.Fatal(err)
	}
	var w rpcWorker
	rpcLoad(filepath.Join(rpcDir("b-1"), "worker.json"), &w)
	w.Adoption = &rpcAdoption{Ready: true, Completed: true, PIDs: []int{os.Getpid()}, SessionID: sid, SessionFile: file, SessionSize: size, SessionHash: hash}
	rpcSave(filepath.Join(rpcDir("b-1"), "worker.json"), w)
	rpcSave(rpcParkPath("b-1"), true)
	for round := 0; round < 2; round++ {
		cmd := launch()
		r, err := rpcClient("b-1", rpcRequest{Op: "status"})
		if err != nil || r.Status.Phase != "parked" || r.Status.SessionFile != file || r.Status.SessionID != sid {
			t.Fatalf("identity/park: %+v %v", r, err)
		}
		if _, err := rpcClient("b-1", rpcRequest{Op: "prompt", ID: "automated", Text: "continue"}); err == nil {
			t.Fatal("migration implicitly continued work")
		}
		if _, err := rpcClient("b-1", rpcRequest{Op: "activate"}); err == nil {
			t.Fatal("unreadable bead activated")
		}
		events, _ := os.ReadFile(filepath.Join(rpcDir("b-1"), "events.jsonl"))
		if bytes.Contains(events, []byte("agent_start")) {
			t.Fatal("startup triggered model")
		}
		if b, _ := os.ReadFile(file); !bytes.Equal(b, original) {
			t.Fatal("original history overwritten")
		}
		rpcClient("b-1", rpcRequest{Op: "stop"})
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRPCAdoptionAdmissionSerializesDelivery(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
	shared, err := adoptionAdmission(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := rpcPrepare(Config{}, Herdr{}, Bead{ID: "b-1", Status: StatusInProgress}, "w1:p1"); err == nil || !strings.Contains(err.Error(), "admission busy") {
		t.Fatalf("prepare crossed delivery: %v", err)
	}
	shared.Close()
	exclusive, err := adoptionAdmission(true)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	h := Herdr{bin: "/must-not-be-called"}
	if _, err := promptPiAgent(h, Agent{Name: "b-1", PaneID: "w1:p1"}, "continue"); err == nil || !strings.Contains(err.Error(), "admission busy") {
		t.Fatal(err)
	}
	if err := h.Prompt("w1:p1", "continue"); err == nil || !strings.Contains(err.Error(), "admission busy") {
		t.Fatal(err)
	}
	show := func(string) (Bead, error) { return Bead{ID: "b-1"}, nil }
	if _, err := tellCmd(Config{}, h, show, "b-1", "continue"); err == nil || !strings.Contains(err.Error(), "admission busy") {
		t.Fatal(err)
	}
}
func TestRPCAdoptionAllThreadDescendants(t *testing.T) {
	root := t.TempDir()
	for file, children := range map[string]string{"100/task/100/children": "101", "100/task/150/children": "102", "101/task/101/children": "", "102/task/102/children": "103", "103/task/103/children": ""} {
		p := filepath.Join(root, file)
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte(children), 0600)
	}
	got, err := adoptionDescendantsIn(root, 100)
	if err != nil || len(got) != 4 || got[3] != 103 {
		t.Fatalf("missed worker-thread descendants: %v %v", got, err)
	}
}
func TestRPCAdoptionRejectsSubmodules(t *testing.T) {
	cwd := adoptionRepo(t)
	sha, _ := adoptionGit(cwd, "rev-parse", "HEAD")
	if out, err := exec.Command("git", "-C", cwd, "update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(string(sha))+",nested").CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	if _, _, err := adoptionDigest(cwd); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatal(err)
	}
}
func TestRPCAdoptionOrphansAndPIDReuse(t *testing.T) {
	root := t.TempDir()
	boot := filepath.Join(root, "sys/kernel/random/boot_id")
	os.MkdirAll(filepath.Dir(boot), 0700)
	os.WriteFile(boot, []byte("boot-one"), 0600)
	proc := func(pid, sid int, start, cwd string) {
		t.Helper()
		dir := filepath.Join(root, strconv.Itoa(pid))
		os.MkdirAll(dir, 0700)
		fields := make([]string, 20)
		for i := range fields {
			fields[i] = "0"
		}
		fields[0] = "S"
		fields[3] = strconv.Itoa(sid)
		fields[19] = start
		os.WriteFile(filepath.Join(dir, "stat"), []byte(strconv.Itoa(pid)+" (fixture) "+strings.Join(fields, " ")), 0600)
		os.Symlink(cwd, filepath.Join(dir, "cwd"))
	}
	w := rpcWorker{Cwd: "/checkout", Adoption: &rpcAdoption{Owners: []adoptionOwner{{PID: 100, Start: "10", Boot: "boot-one", Session: 500, SessionStart: "5"}}}}
	// The recorded parent is gone, but a child forked after checkpoint remains
	// in its process session, even after leaving the worktree and reparenting.
	proc(110, 500, "11", "/elsewhere")
	if err := adoptionRemaining(root, w, nil); err == nil || !strings.Contains(err.Error(), "110") {
		t.Fatalf("orphan missed: %v", err)
	}
	os.Remove(filepath.Join(root, "110", "stat")) // the last member exited
	// Reuse of both a recorded PID and an old session leader is not the owner.
	proc(100, 600, "20", "/elsewhere")
	proc(500, 500, "50", "/elsewhere")
	if err := adoptionRemaining(root, w, nil); err != nil {
		t.Fatalf("reused PID/session mistaken for old owner: %v", err)
	}
	proc(111, 900, "21", "/checkout/subdir")
	if err := adoptionRemaining(root, w, nil); err == nil || !strings.Contains(err.Error(), "111") {
		t.Fatalf("new process in checkout missed: %v", err)
	}
}

func TestRPCAdoptionIntentIsNotCompletedHandoff(t *testing.T) {
	fixtureSupervisor(t)
	cwd := adoptionRepo(t)
	branch, digest, err := adoptionDigest(cwd)
	if err != nil {
		t.Fatal(err)
	}
	b := Bead{ID: "b-1", Status: StatusInProgress, Assignee: "pi"}
	h := adoptionHerdr(t, nil, adoptionProcessInfo{ShellPID: os.Getpid(), Procs: []adoptionProcess{{PID: os.Getpid()}}}, nil)
	t.Setenv("HERDR_BIN_PATH", h.bin)
	bd := filepath.Join(stateDir(), "bin", "bd")
	data, _ := json.Marshal([]Bead{b})
	os.WriteFile(bd, []byte("#!/bin/sh\nprintf '%s' "+shellQuote(string(data))+"\n"), 0700)
	shell, err := adoptionIdentity("/proc", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	w := rpcWorker{Bead: b.ID, Cwd: cwd, Branch: branch, Adoption: &rpcAdoption{Ready: true, Bead: b, Digest: digest, ShellPID: os.Getpid(), Shell: shell, Source: Agent{PaneID: "w1:p1"}}}
	rpcSave(filepath.Join(rpcDir(b.ID), "worker.json"), w)
	os.WriteFile(filepath.Join(cwd, "new"), []byte("changed since startup intent"), 0600)
	if err := rpcServe(Config{}, b.ID); err == nil || !strings.Contains(err.Error(), "checkout changed") {
		t.Fatalf("Ready bypassed validation: %v", err)
	}
	w.Adoption.Bead.Status = StatusNeedsMe
	rpcSave(filepath.Join(rpcDir(b.ID), "worker.json"), w)
	if err := rpcServe(Config{}, b.ID); err == nil || !strings.Contains(err.Error(), "status/owner changed") {
		t.Fatalf("Ready implicitly cleared blocker: %v", err)
	}
}

func TestRPCAdoptionSharedReservationLock(t *testing.T) {
	t.Setenv("QUARTERMASTER_STATE_DIR", rpcTestDir(t))
	lease, err := rpcLease("b-1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := dispatch(Config{}, Herdr{}, "b-1", DispatchOpts{Backend: "herdr"}); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("dispatch not serialized: %v", err)
	}
	if _, err := resumeOne(Config{}, Herdr{}, Resumable{Bead: Bead{ID: "b-1"}}, "pi"); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("resume not serialized: %v", err)
	}
}
