package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type forkFixture struct {
	cfg          Config
	source, dest Bead
	worker       rpcWorker
	status       rpcStatus
	data         []byte
}

func newForkFixture(t *testing.T) forkFixture {
	t.Helper()
	isolateRPCOperations(t)
	_ = fixtureSupervisor(t) // fixture pi/bd only, never the real services
	f := forkFixture{source: Bead{ID: "source", Status: StatusInProgress}, dest: Bead{ID: "child", Title: "Independent task", Description: "Inspect the new task only", Status: StatusOpen}}
	repo := adoptionRepo(t)
	f.cfg = Config{Repo: repo, BranchPrefix: "test/"}
	lease, err := rpcLease(f.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	cwd := filepath.Join(rpcDir(f.source.ID), "worktree")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "test/source", cwd).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	for _, dir := range []string{"sessions", "requests"} {
		if err := rpcPrivateDir(filepath.Join(rpcDir(f.source.ID), dir)); err != nil {
			t.Fatal(err)
		}
	}
	f.worker = rpcWorker{Bead: f.source.ID, Cwd: cwd, Branch: "test/source", Args: []string{"--mode", "rpc"}}
	f.status = rpcStatus{Bead: f.source.ID, Phase: "stopped", SessionID: "source-session", SessionFile: filepath.Join(rpcDir(f.source.ID), "sessions", "source.jsonl"), GracefulStop: true, GracefulStopRequested: true, Generation: "source-generation"}
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": f.status.SessionID, "cwd": cwd})
	f.data = append(header, []byte("\n"+`{"type":"message","id":"user","parentId":null,"timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"source task","timestamp":1}}`+"\n"+`{"type":"message","id":"checkpoint","parentId":"user","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop","timestamp":2}}`+"\n"+`{"type":"message","id":"later","parentId":"checkpoint","message":{"role":"user","content":"never replay this prompt"}}`+"\n")...)
	if err := os.WriteFile(f.status.SessionFile, f.data, 0600); err != nil {
		t.Fatal(err)
	}
	f.save(t)
	return f
}
func (f forkFixture) save(t *testing.T) {
	t.Helper()
	if err := rpcSave(filepath.Join(rpcDir(f.source.ID), "worker.json"), f.worker); err != nil {
		t.Fatal(err)
	}
	if err := rpcSave(filepath.Join(rpcDir(f.source.ID), "status.json"), f.status); err != nil {
		t.Fatal(err)
	}
}
func (f forkFixture) fork() (*rpcFork, error) {
	return rpcForkPrepare(f.cfg, f.source, f.dest, "checkpoint", []string{"--mode", "rpc"}, nil)
}

func TestRPCForkCreatesIndependentParkedIdentity(t *testing.T) {
	f := newForkFixture(t)
	paths := []string{"worker.json", "status.json", "sessions/source.jsonl", "lease"}
	before := map[string][]byte{}
	for _, p := range paths {
		before[p], _ = os.ReadFile(filepath.Join(rpcDir(f.source.ID), p))
	}
	r, err := f.fork()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Ready || r.SessionID == f.status.SessionID || r.SessionFile == f.status.SessionFile || !rpcParked(f.dest.ID) {
		t.Fatal(r)
	}
	for _, p := range paths {
		after, _ := os.ReadFile(filepath.Join(rpcDir(f.source.ID), p))
		if !bytes.Equal(before[p], after) {
			t.Fatal("source changed", p)
		}
	}
	seed, err := os.ReadFile(r.SessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(seed, []byte("never replay")) || !bytes.Contains(seed, []byte("quartermaster.fork")) || !bytes.Contains(seed, []byte(f.dest.Description)) {
		t.Fatal(string(seed))
	}
	var w rpcWorker
	var st rpcStatus
	_ = rpcLoad(filepath.Join(rpcDir(f.dest.ID), "worker.json"), &w)
	_ = rpcLoad(filepath.Join(rpcDir(f.dest.ID), "status.json"), &st)
	if st.PID != 0 || st.ChildPID != 0 || st.Phase != "stopped" || w.Cwd == f.worker.Cwd || w.Branch == f.worker.Branch {
		t.Fatalf("started or shared: %+v %+v", w, st)
	}
	if err := rpcForkValidate(w, st); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{r.SessionFile, rpcParkPath(f.dest.ID), filepath.Join(rpcDir(f.dest.ID), "worker.json")} {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0600 {
			t.Errorf("%s permissions %o", path, fi.Mode().Perm())
		}
	}
	var out bytes.Buffer
	if err := rpcCmd(f.cfg, []string{"fork-status", f.dest.ID}, "", &out); err != nil || !strings.Contains(out.String(), r.SeedHash) {
		t.Fatalf("status: %v %s", err, &out)
	}
	if _, err := f.fork(); err == nil {
		t.Fatal("repeated preparation")
	}
	if _, err := os.Stat(filepath.Join(rpcDir(f.dest.ID), "events.jsonl")); !os.IsNotExist(err) {
		t.Fatal("fork started a supervisor")
	}
	// The source is not a runtime dependency. A retained copied seed still
	// validates if source retention later moves its saved transcript away.
	if err := os.Rename(f.status.SessionFile, f.status.SessionFile+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := rpcForkValidate(w, st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Cwd, "child-only"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.worker.Cwd, "child-only")); !os.IsNotExist(err) {
		t.Fatal("worktrees shared")
	}
}

func TestRPCForkRefusals(t *testing.T) {
	for _, mode := range []string{"same-id", "claimed", "blocked", "different-repo", "remote", "live-lease", "destination-lease", "admission", "working", "disconnected", "legacy-stop", "live-pid", "uncertain", "parked", "adoption", "handoff", "dirty", "ignored", "wrong-branch", "wrong-session", "partial-session", "external-session", "session-symlink", "missing-checkout", "checkpoint-user", "existing-branch", "cleanup-intent", "session-hardlink", "session-fifo", "add-failure", "oversized-context"} {
		t.Run(mode, func(t *testing.T) {
			f := newForkFixture(t)
			checkpoint := "checkpoint"
			switch mode {
			case "oversized-context":
				f.dest.Description = strings.Repeat("x", 1<<20)
			case "add-failure":
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				fake := filepath.Join(stateDir(), "bin", "git")
				if err := os.WriteFile(fake, []byte("#!/bin/sh\ncase \"$*\" in *\"worktree add\"*) exit 1;; esac\nexec "+shellQuote(realGit)+" \"$@\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			case "cleanup-intent":
				_ = os.WriteFile(filepath.Join(rpcDir(f.source.ID), "cleanup.json"), []byte(`{"phase":"intent"}`), 0600)
			case "session-hardlink":
				if err := os.Link(f.status.SessionFile, f.status.SessionFile+".alias"); err != nil {
					t.Fatal(err)
				}
			case "session-fifo":
				if err := os.Rename(f.status.SessionFile, f.status.SessionFile+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(f.status.SessionFile, 0600); err != nil {
					t.Fatal(err)
				}
			case "same-id":
				f.dest.ID = f.source.ID
			case "claimed":
				f.dest.Assignee = "other"
			case "blocked":
				f.source.Status = StatusNeedsMe
			case "different-repo":
				f.dest.Labels = []string{"repo:other"}
			case "remote":
				f.source.Labels = []string{"host:remote"}
			case "live-lease", "destination-lease":
				id := f.source.ID
				if mode == "destination-lease" {
					id = f.dest.ID
				}
				l, err := rpcLease(id)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
			case "admission":
				l, err := adoptionAdmission(false)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
			case "working":
				f.status.Phase = "working"
			case "disconnected":
				f.status.Phase = "starting"
			case "legacy-stop":
				f.status.GracefulStopRequested = false
			case "live-pid":
				f.status.PID = os.Getpid()
			case "uncertain":
				_ = rpcSave(filepath.Join(rpcDir(f.source.ID), "requests", "maybe.json"), rpcReceipt{ID: "maybe", State: "intent"})
			case "parked":
				_ = os.WriteFile(rpcParkPath(f.source.ID), []byte("parked"), 0600)
			case "adoption":
				f.worker.Adoption = &rpcAdoption{}
			case "handoff":
				f.worker.Handoff = &rpcHandoff{}
			case "dirty":
				_ = os.WriteFile(filepath.Join(f.worker.Cwd, "tracked"), []byte("dirty"), 0600)
			case "ignored":
				_ = os.WriteFile(filepath.Join(f.worker.Cwd, ".gitignore"), []byte("ignored\n"), 0600)
				_ = os.WriteFile(filepath.Join(f.worker.Cwd, "ignored"), []byte("retain"), 0600)
			case "wrong-branch":
				f.worker.Branch = "unowned"
			case "wrong-session":
				f.status.SessionID = "other"
			case "partial-session":
				_ = os.WriteFile(f.status.SessionFile, append(f.data, []byte(`{"partial":true}`)...), 0600)
			case "external-session":
				f.status.SessionFile = filepath.Join(t.TempDir(), "external.jsonl")
			case "session-symlink":
				_ = os.Rename(f.status.SessionFile, f.status.SessionFile+".saved")
				_ = os.Symlink(f.status.SessionFile+".saved", f.status.SessionFile)
			case "missing-checkout":
				f.worker.Cwd = filepath.Join(rpcDir(f.source.ID), "absent")
			case "checkpoint-user":
				checkpoint = "user"
			case "existing-branch":
				if out, err := exec.Command("git", "-C", f.cfg.Repo, "branch", branchFor(f.cfg.BranchPrefix, f.dest)).CombinedOutput(); err != nil {
					t.Fatalf("%s %v", out, err)
				}
			}
			f.save(t)
			if _, err := rpcForkPrepare(f.cfg, f.source, f.dest, checkpoint, []string{"--mode", "rpc"}, nil); err == nil {
				t.Fatal("accepted", mode)
			}
			if mode == "existing-branch" && rpcExists(f.dest.ID) {
				t.Fatal("predictable precondition failure captured destination reservation")
			}
			if mode == "add-failure" {
				var child rpcWorker
				if err := rpcLoad(filepath.Join(rpcDir(f.dest.ID), "worker.json"), &child); err != nil || child.Fork.Ready {
					t.Fatalf("intent not retained %+v %v", child, err)
				}
				if err := rpcServe(Config{}, f.dest.ID); err == nil || !strings.Contains(err.Error(), "incomplete") {
					t.Fatal("incomplete launch", err)
				}
				if _, err := f.fork(); err == nil {
					t.Fatal("silently retried partial preparation")
				}
			}
		})
	}
}

func TestRPCForkSessionAncestryAndMalformedTrees(t *testing.T) {
	f := newForkFixture(t)
	for _, tc := range []struct {
		name, suffix, through string
		valid                 bool
	}{
		{"ancestor-only", "", "checkpoint", true},
		{"missing-parent", `{"type":"custom","id":"bad","parentId":"missing"}` + "\n", "checkpoint", false},
		{"duplicate", `{"type":"custom","id":"user","parentId":null}` + "\n", "checkpoint", false},
		{"unfinished-tools", `{"type":"message","id":"bad","parentId":"checkpoint","message":{"role":"assistant","stopReason":"stop","content":[{"type":"toolCall","id":"call"}]}}` + "\n", "bad", false},
		{"compacted", `{"type":"compaction","id":"compact","parentId":"checkpoint","firstKeptEntryId":"checkpoint","summary":"retained"}` + "\n" + `{"type":"context_edit","id":"edit","parentId":"compact","targetId":"user","replacement":null}` + "\n" + `{"type":"message","id":"done","parentId":"edit","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"done"}]}}` + "\n", "done", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed, err := rpcForkSession(append(bytes.Clone(f.data), tc.suffix...), f.status, f.worker.Cwd, "/new", "new-id", tc.through, "new context")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if tc.valid && bytes.Contains(seed, []byte("never replay")) {
				t.Fatal("copied non-ancestor")
			}
			if tc.name == "compacted" && (!bytes.Contains(seed, []byte(`"firstKeptEntryId":"checkpoint"`)) || !bytes.Contains(seed, []byte(`"targetId":"user"`))) {
				t.Fatal("lost context semantics")
			}
		})
	}
}

func TestRPCForkResumeValidatesSeed(t *testing.T) {
	for _, mode := range []string{"incomplete", "identity", "tampered", "missing", "cleanup-intent"} {
		t.Run(mode, func(t *testing.T) {
			f := newForkFixture(t)
			r, err := f.fork()
			if err != nil {
				t.Fatal(err)
			}
			var w rpcWorker
			var st rpcStatus
			_ = rpcLoad(filepath.Join(rpcDir(f.dest.ID), "worker.json"), &w)
			_ = rpcLoad(filepath.Join(rpcDir(f.dest.ID), "status.json"), &st)
			switch mode {
			case "cleanup-intent":
				_ = os.WriteFile(filepath.Join(rpcDir(f.dest.ID), "cleanup.json"), []byte(`{"phase":"intent"}`), 0600)
			case "incomplete":
				w.Fork.Ready = false
			case "identity":
				st.SessionID = "different"
			case "tampered":
				_ = os.WriteFile(r.SessionFile, []byte("tampered\n"), 0600)
			case "missing":
				_ = os.Rename(r.SessionFile, r.SessionFile+".saved")
			}
			_ = rpcSave(filepath.Join(rpcDir(f.dest.ID), "worker.json"), w)
			_ = rpcSave(filepath.Join(rpcDir(f.dest.ID), "status.json"), st)
			if err := rpcServe(Config{}, f.dest.ID); err == nil {
				t.Fatal("launched invalid fork")
			}
			if _, err := os.Stat(filepath.Join(rpcDir(f.dest.ID), "events.jsonl")); !os.IsNotExist(err) {
				t.Fatal("pi started before validation")
			}
		})
	}
}

func TestRPCForkSupervisorProcess(t *testing.T) {
	if os.Getenv("QM_FORK_SUPERVISOR") != "1" {
		return
	}
	if err := rpcServe(Config{}, "child"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRPCForkResumeIsParkedAndNoReplay(t *testing.T) {
	f := newForkFixture(t)
	r, err := f.fork()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := os.ReadFile(r.SessionFile)
	for range 2 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRPCForkSupervisorProcess$")
		cmd.Env = append(os.Environ(), "QM_FORK_SUPERVISOR=1")
		var log bytes.Buffer
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		var waitErr error
		go func() { waitErr = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		waitRPC(t, func() bool {
			select {
			case <-done:
				t.Fatalf("fork supervisor exited: %v %s", waitErr, &log)
			default:
			}
			st, err := rpcClient(f.dest.ID, rpcRequest{Op: "status"})
			return err == nil && st.Status.Phase == "parked"
		})
		st, err := rpcClient(f.dest.ID, rpcRequest{Op: "status"})
		if err != nil || st.Status.SessionID != r.SessionID || st.Status.SessionFile != r.SessionFile {
			t.Fatalf("%+v %v", st, err)
		}
		if _, err := rpcClient(f.dest.ID, rpcRequest{Op: "prompt", ID: "not-authorized", Text: "do not run"}); err == nil {
			t.Fatal("unparked by resume")
		}
		if _, err := rpcClient(f.dest.ID, rpcRequest{Op: "steer", ID: "not-authorized-steer", Text: "do not run"}); err == nil {
			t.Fatal("steered parked fork")
		}
		if _, err := rpcGracefulStop(f.dest.ID, 2e9); err != nil {
			t.Fatal(err)
		}
		<-done
		if waitErr != nil {
			t.Fatalf("%v %s", waitErr, &log)
		}
	}
	after, _ := os.ReadFile(r.SessionFile)
	if !bytes.Equal(seed, after) {
		t.Fatal("seed changed on no-prompt resume")
	}
	log, _ := os.ReadFile(filepath.Join(rpcDir(f.dest.ID), "events.jsonl"))
	if bytes.Contains(log, []byte(`"type":"agent_start"`)) {
		t.Fatal("silent replay")
	}
}
