package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

type rpcMaintenanceFixture struct {
	cfg     Config
	bead    Bead
	worker  rpcWorker
	env     rpcCleanupEnv
	session string
}

func maintenanceWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func maintenanceRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func maintenanceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}
func maintenanceFixture(t *testing.T) rpcMaintenanceFixture {
	t.Helper()
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	// Do not inherit real state, credentials, sockets, executors or Beads DB.
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "QUARTERMASTER_STATE_DIR", "QUARTERMASTER_CONFIG_DIR", "BEADS_DIR", "PI_CODING_AGENT_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	for _, key := range []string{"QUARTERMASTER_EXEC_SOCKET", "HERDR_SOCKET_PATH", "HERDR_BIN_PATH", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	bin := filepath.Join(root, "bin")
	maintenanceWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\ncase \"$1\" in\n show) printf '%s\\n' '[{\"id\":\"demo-1\",\"status\":\"closed\"}]';;\n note) exit 0;;\n *) exit 1;;\nesac\n")
	maintenanceWrite(t, filepath.Join(bin, "gh"), "#!/bin/sh\nprintf '[]\\n'\n")
	maintenanceWrite(t, filepath.Join(bin, "herdr"), "#!/bin/sh\nexit 91\n")
	for _, name := range []string{"bd", "gh", "herdr"} {
		if err := os.Chmod(filepath.Join(bin, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	maintenanceGit(t, repo, "init", "-b", "main")
	maintenanceGit(t, repo, "config", "user.email", "fixture@example.invalid")
	maintenanceGit(t, repo, "config", "user.name", "Fixture")
	maintenanceWrite(t, filepath.Join(repo, "file"), "tracked\n")
	maintenanceGit(t, repo, "add", "file")
	maintenanceGit(t, repo, "commit", "-m", "fixture")
	lease, err := rpcLease("demo-1")
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	w := rpcWorker{Bead: "demo-1", Cwd: filepath.Join(rpcDir("demo-1"), "worktree"), Branch: "fixture-work"}
	maintenanceGit(t, repo, "worktree", "add", "-b", w.Branch, w.Cwd, "main")
	if err := rpcSave(filepath.Join(rpcDir(w.Bead), "worker.json"), w); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(rpcDir(w.Bead), "sessions", "saved.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "id": "saved-session", "cwd": w.Cwd, "version": 3})
	maintenanceWrite(t, session, string(header)+"\n"+`{"type":"message","id":"m1","message":{"role":"assistant","content":[{"type":"text","text":"full saved transcript"}]}}`+"\n")
	st := rpcStatus{Bead: w.Bead, Phase: "stopped", SessionID: "saved-session", SessionFile: session}
	if err := rpcSave(filepath.Join(rpcDir(w.Bead), "status.json"), st); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rpcDir(w.Bead), "requests"), 0700); err != nil {
		t.Fatal(err)
	}
	r := rpcReceipt{ID: "request-1", Hash: "durable-text-hash", State: "accepted", Response: rpcRecord{Type: "response", ID: "request-1", Command: "prompt", Success: true}}
	if err := rpcSave(filepath.Join(rpcDir(w.Bead), "requests", "request-1.json"), r); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(r.Response)
	log := string(response) + "\n" + strings.Repeat(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"streamed text"}}`+"\n", 100) + `{"type":"agent_settled"}` + "\n"
	maintenanceWrite(t, filepath.Join(rpcDir(w.Bead), "events.jsonl"), log)
	maintenanceWrite(t, filepath.Join(rpcDir(w.Bead), "stderr.log"), "diagnostics\n")
	maintenanceWrite(t, filepath.Join(rpcDir(w.Bead), "supervisor.log"), "supervisor diagnostics\n")
	maintenanceWrite(t, rpcParkPath(w.Bead), "parked evidence")
	maintenanceWrite(t, filepath.Join(stateDir(), "inbox", "archive", "receipt.md"), "archived inbox evidence")
	cfg := Config{Repo: repo, BaseBranch: "main", RPCRetention: RPCRetentionConfig{EventBytes: 2048}}
	return rpcMaintenanceFixture{cfg, Bead{ID: w.Bead, Status: StatusClosed}, w, rpcCleanupEnv{git: gitRun, merged: rpcCleanupMerged}, session}
}

func TestRPCRetentionPreservesEvidenceAndReportsGaps(t *testing.T) {
	f := maintenanceFixture(t)
	id := f.bead.ID
	paths := []string{f.session, filepath.Join(rpcDir(id), "requests", "request-1.json"), filepath.Join(rpcDir(id), "worker.json"), filepath.Join(rpcDir(id), "status.json"), filepath.Join(rpcDir(id), "lease"), rpcParkPath(id), filepath.Join(rpcDir(id), "stderr.log"), filepath.Join(rpcDir(id), "supervisor.log"), filepath.Join(stateDir(), "inbox", "archive", "receipt.md")}
	before := map[string]string{}
	for _, path := range paths {
		before[path] = maintenanceRead(t, path)
	}
	log := filepath.Join(rpcDir(id), "events.jsonl")
	original := maintenanceRead(t, log)
	page, err := rpcViewRead(id, "events", "")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", id}, "TJ", false, 0, &out); err != nil {
		t.Fatal(err)
	}
	if maintenanceRead(t, log) != original {
		t.Fatal("preview modified log")
	}
	out.Reset()
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", id}, "TJ", true, 0, &out); err != nil {
		t.Fatal(err)
	}
	var report rpcMaintenanceReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	result := maintenanceRead(t, log)
	if !report.Applied || report.After != int64(len(result)) || len(result) > 2048 || !strings.Contains(result, `"id":"request-1"`) {
		t.Fatalf("bad retention: %+v", report)
	}
	for _, path := range paths {
		if maintenanceRead(t, path) != before[path] {
			t.Fatalf("changed protected %s", path)
		}
	}
	replay, err := rpcViewRead(id, "events", page.Cursor)
	if err != nil || !replay.Reset || replay.Gap == "" || len(replay.Events) == 0 || replay.Events[0].Kind != "gap" {
		t.Fatalf("stale replay: %+v %v", replay, err)
	}
	fresh, err := rpcViewRead(id, "events", "")
	if err != nil || len(fresh.Events) == 0 || fresh.Events[0].Kind != "gap" {
		t.Fatalf("fresh replay: %+v %v", fresh, err)
	}
	out.Reset()
	if err := rpcCmd(f.cfg, []string{"output", id}, "TJ", &out); err != nil || !strings.Contains(out.String(), "GAP:") || !strings.Contains(out.String(), "]\n") {
		t.Fatal("raw output hid retention", out.String(), err)
	}
	out.Reset()
	if err := rpcCmd(f.cfg, []string{"receipt", id, "request-1"}, "TJ", &out); err != nil || !strings.Contains(out.String(), "accepted") {
		t.Fatal("receipt no longer reconciles", err)
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", id}, "TJ", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	if maintenanceRead(t, log) != result {
		t.Fatal("repeat retention changed bounded log")
	}
	// A new generation can append normally; a second retention keeps cumulative gaps.
	appendLog, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(appendLog, strings.Repeat(`{"type":"agent_start"}`+"\n", 200))
	appendLog.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", id}, "TJ", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	var gap rpcLogGap
	if err := json.Unmarshal([]byte(strings.SplitN(maintenanceRead(t, log), "\n", 2)[0]), &gap); err != nil {
		t.Fatal(err)
	}
	if gap.Records <= report.Removed {
		t.Fatal("forgot earlier gap", gap)
	}
}

func TestRPCMaintenanceRefusesUnsafeOwners(t *testing.T) {
	for _, tc := range []string{"lease", "live-pid", "working", "failed", "status-error", "adopted", "handoff", "incomplete-fork", "fork-session-mismatch", "worktree-free", "foreign-path", "missing-session", "wrong-session", "intent", "steer-intent", "malformed-receipt", "incomplete-receipt", "symlink", "hardlink"} {
		t.Run(tc, func(t *testing.T) {
			f := maintenanceFixture(t)
			id := f.bead.ID
			wp := filepath.Join(rpcDir(id), "worker.json")
			sp := filepath.Join(rpcDir(id), "status.json")
			rp := filepath.Join(rpcDir(id), "requests", "request-1.json")
			var st rpcStatus
			if err := rpcLoad(sp, &st); err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "lease":
				l, err := rpcLease(id)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
			case "live-pid":
				st.PID = os.Getpid()
				rpcSave(sp, st)
			case "working", "failed":
				st.Phase = tc
				rpcSave(sp, st)
			case "status-error":
				st.Error = rpcStrandedSteering
				rpcSave(sp, st)
			case "adopted":
				f.worker.Adoption = &rpcAdoption{}
				rpcSave(wp, f.worker)
			case "handoff":
				f.worker.Handoff = &rpcHandoff{}
				rpcSave(wp, f.worker)
			case "incomplete-fork":
				f.worker.Fork = &rpcFork{Source: "src-1", SessionID: st.SessionID, SessionFile: st.SessionFile, Ready: false}
				rpcSave(wp, f.worker)
			case "fork-session-mismatch":
				f.worker.Fork = &rpcFork{Source: "src-1", SessionID: "other-session", SessionFile: st.SessionFile, Ready: true}
				rpcSave(wp, f.worker)
			case "worktree-free":
				f.worker.Cwd = filepath.Join(stateDir(), "roles", id)
				if err := os.MkdirAll(f.worker.Cwd, 0700); err != nil {
					t.Fatal(err)
				}
				f.worker.Branch = ""
				rpcSave(wp, f.worker)
			case "foreign-path":
				f.worker.Cwd = f.cfg.Repo
				rpcSave(wp, f.worker)
			case "missing-session":
				os.Remove(f.session)
			case "wrong-session":
				st.SessionID = "different-session"
				rpcSave(sp, st)
			case "intent":
				rpcSave(rp, rpcReceipt{ID: "request-1", State: "intent"})
			case "steer-intent":
				rpcSave(filepath.Join(rpcDir(id), "requests", "steer-1.json"), rpcReceipt{ID: "steer-1", Operation: "steer", Hash: "h", State: "intent"})
			case "malformed-receipt":
				maintenanceWrite(t, rp, "{")
			case "incomplete-receipt":
				rpcSave(rp, rpcReceipt{ID: "request-1", State: "accepted"})
			case "symlink":
				os.Rename(wp, wp+".saved")
				if err := os.Symlink(wp+".saved", wp); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(wp, wp+".saved"); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(rpcDir(id), "events.jsonl")
			before := maintenanceRead(t, log)
			for _, op := range []string{"retain", "cleanup"} {
				if err := rpcMaintenanceCmd(f.cfg, []string{op, id}, "TJ", true, 0, io.Discard); err == nil {
					t.Fatal("unsafe maintenance allowed", tc, op)
				}
			}
			if maintenanceRead(t, log) != before {
				t.Fatal("refusal changed log")
			}
			if _, err := os.Stat(f.worker.Cwd); err != nil {
				t.Fatal("refusal removed checkout", err)
			}
		})
	}
}

func TestRPCRetentionRefusesInvalidLogsAndBudgets(t *testing.T) {
	for _, tc := range []string{"disabled", "negative", "partial", "malformed", "protected", "symlink"} {
		t.Run(tc, func(t *testing.T) {
			f := maintenanceFixture(t)
			log := filepath.Join(rpcDir(f.bead.ID), "events.jsonl")
			limit := int64(1024)
			switch tc {
			case "disabled":
				limit = 0
				f.cfg.RPCRetention.EventBytes = 0
			case "negative":
				limit = -1
			case "partial":
				maintenanceWrite(t, log, `{"type":"message_update"}`)
			case "malformed":
				maintenanceWrite(t, log, "not json\n")
			case "protected":
				maintenanceWrite(t, log, strings.Repeat(`{"type":"response","command":"future","data":"protected"}`+"\n", 100))
			case "symlink":
				os.Rename(log, log+".saved")
				if err := os.Symlink(log+".saved", log); err != nil {
					t.Fatal(err)
				}
			}
			before := maintenanceRead(t, log)
			if err := rpcMaintenanceCmd(f.cfg, []string{"retain", f.bead.ID}, "TJ", true, limit, io.Discard); err == nil {
				t.Fatal("unsafe retention allowed")
			}
			if maintenanceRead(t, log) != before {
				t.Fatal("refusal changed log")
			}
		})
	}
}

func TestRPCCleanupAndInterruptedCompletion(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "interrupted"}[interrupted], func(t *testing.T) {
			f := maintenanceFixture(t)
			original := maintenanceRead(t, f.session)
			if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, false, f.env); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(rpcCleanupPath(f.bead.ID)); !os.IsNotExist(err) {
				t.Fatal("preview wrote checkpoint", err)
			}
			if interrupted {
				f.env.git = func(dir string, args ...string) (string, error) {
					out, err := gitRun(dir, args...)
					if err == nil && len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
						return out, errors.New("interrupted after git removed worktree")
					}
					return out, err
				}
			}
			_, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env)
			if (err != nil) != interrupted {
				t.Fatalf("remove err=%v", err)
			}
			f.env.git = gitRun
			if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err != nil {
				t.Fatal("cannot reconcile interrupted cleanup", err)
			}
			var checkpoint rpcCleanupRecord
			if err := rpcLoad(rpcCleanupPath(f.bead.ID), &checkpoint); err != nil || checkpoint.Phase != "removed" {
				t.Fatal("bad completion", checkpoint, err)
			}
			if _, err := os.Stat(f.worker.Cwd); !os.IsNotExist(err) {
				t.Fatal("checkout retained", err)
			}
			if maintenanceRead(t, f.session) != original || !rpcExists(f.bead.ID) {
				t.Fatal("lost saved session/reservation")
			}
			maintenanceGit(t, f.cfg.Repo, "rev-parse", f.worker.Branch) // rollback branch survives
			if err := rpcStart(f.bead.ID); err == nil || !strings.Contains(err.Error(), "cleanup") {
				t.Fatal("resume not fenced", err)
			}
			if err := rpcServe(f.cfg, f.bead.ID); err == nil || !strings.Contains(err.Error(), "cleanup") {
				t.Fatal("serve not fenced", err)
			}
		})
	}
}

func TestRPCCleanupRefusesUnsafeWorktrees(t *testing.T) {
	for _, tc := range []string{"active", "dirty", "untracked", "ignored", "hidden-index", "unmerged", "branch-changed", "locked", "dangling-checkpoint", "interrupted-dirty"} {
		t.Run(tc, func(t *testing.T) {
			f := maintenanceFixture(t)
			switch tc {
			case "active":
				f.bead.Status = StatusInProgress
			case "dirty":
				maintenanceWrite(t, filepath.Join(f.worker.Cwd, "file"), "modified")
			case "untracked":
				maintenanceWrite(t, filepath.Join(f.worker.Cwd, "untracked"), "precious")
			case "ignored":
				maintenanceWrite(t, filepath.Join(f.cfg.Repo, ".git", "info", "exclude"), "ignored\n")
				maintenanceWrite(t, filepath.Join(f.worker.Cwd, "ignored"), "precious")
			case "hidden-index":
				maintenanceGit(t, f.worker.Cwd, "update-index", "--skip-worktree", "file")
				maintenanceWrite(t, filepath.Join(f.worker.Cwd, "file"), "hidden local edit")
			case "unmerged":
				maintenanceWrite(t, filepath.Join(f.worker.Cwd, "file"), "new commit")
				maintenanceGit(t, f.worker.Cwd, "commit", "-am", "unmerged")
			case "branch-changed":
				maintenanceGit(t, f.worker.Cwd, "switch", "-c", "another")
			case "locked":
				maintenanceGit(t, f.cfg.Repo, "worktree", "lock", f.worker.Cwd)
			case "dangling-checkpoint":
				if err := os.Symlink(filepath.Join(rpcDir(f.bead.ID), "missing-target"), rpcCleanupPath(f.bead.ID)); err != nil {
					t.Fatal(err)
				}
			case "interrupted-dirty":
				f.env.git = func(dir string, args ...string) (string, error) {
					if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
						maintenanceWrite(t, filepath.Join(f.worker.Cwd, "untracked"), "keep me")
						return "", errors.New("interrupted")
					}
					return gitRun(dir, args...)
				}
			}
			if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err == nil {
				t.Fatal("unsafe cleanup allowed")
			}
			if _, err := os.Stat(f.worker.Cwd); err != nil {
				t.Fatal("checkout removed", err)
			}
			if tc == "interrupted-dirty" {
				f.env.git = gitRun
				if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err == nil {
					t.Fatal("retry destroyed new dirty files")
				}
				if err := rpcCleanupBarrier(f.bead.ID); err == nil {
					t.Fatal("interrupted cleanup did not fence resume")
				}
			}
		})
	}
}

func TestRPCRetentionInterruptedStageAndTUIIsolation(t *testing.T) {
	f := maintenanceFixture(t)
	// A crash before rename can leave a private stage. It is not the log and
	// cannot be interpreted as a gap, a source of receipts, or garbage to sweep.
	staged := filepath.Join(rpcDir(f.bead.ID), ".retention-interrupted")
	maintenanceWrite(t, staged, "incomplete staged replacement")
	tui := filepath.Join(filepath.Dir(f.cfg.Repo), "tui-worktree")
	maintenanceGit(t, f.cfg.Repo, "worktree", "add", "-b", "tui-work", tui, "main")
	maintenanceWrite(t, filepath.Join(tui, "untracked"), "TUI draft")
	tuiSession := filepath.Join(os.Getenv("HOME"), ".pi", "agent", "sessions", "tui.jsonl")
	maintenanceWrite(t, tuiSession, "TUI saved session")
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", f.bead.ID}, "TJ", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"cleanup", f.bead.ID}, "TJ", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	if maintenanceRead(t, staged) != "incomplete staged replacement" || maintenanceRead(t, tuiSession) != "TUI saved session" || maintenanceRead(t, filepath.Join(tui, "untracked")) != "TUI draft" {
		t.Fatal("unrelated evidence changed")
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"cleanup", "tui-1"}, "TJ", true, 0, io.Discard); err == nil {
		t.Fatal("TUI-only bead cleaned")
	}
}

func TestRPCMaintenanceAdmissionAndAutonomy(t *testing.T) {
	f := maintenanceFixture(t)
	id := f.bead.ID
	log := filepath.Join(rpcDir(id), "events.jsonl")
	before := maintenanceRead(t, log)
	lease, _, err := rpcMaintenanceLock(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := rpcServe(f.cfg, id); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatal("supervisor entered during maintenance", err)
	}
	lease.Close()
	f.cfg.Autonomy.Level = 0
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", id}, "worker", true, 0, io.Discard); !errors.Is(err, errNotAutonomous) {
		t.Fatal("ungated retention", err)
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"cleanup", id}, "worker", true, 0, io.Discard); !errors.Is(err, errNotAutonomous) {
		t.Fatal("ungated cleanup", err)
	}
	if maintenanceRead(t, log) != before {
		t.Fatal("denied gate changed log")
	}
	if _, err := os.Stat(rpcCleanupPath(id)); !os.IsNotExist(err) {
		t.Fatal("denied gate wrote cleanup intent", err)
	}
	// Dry runs remain available below the mutation gate.
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", id}, "worker", false, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	maintenanceWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\nprintf '%s\\n' '[{\"id\":\"demo-1\",\"status\":\"in_progress\"}]'\n")
	for _, op := range []string{"retain", "cleanup"} {
		if err := rpcMaintenanceCmd(f.cfg, []string{op, id}, "TJ", true, 0, io.Discard); err == nil || !strings.Contains(err.Error(), "active bead") {
			t.Fatal("active bead permitted", err)
		}
	}
}

func TestRPCCleanupSquashRequiresExactHead(t *testing.T) {
	f := maintenanceFixture(t)
	maintenanceWrite(t, filepath.Join(f.worker.Cwd, "file"), "squashed change")
	maintenanceGit(t, f.worker.Cwd, "commit", "-am", "feature")
	head := maintenanceGit(t, f.worker.Cwd, "rev-parse", "HEAD")
	maintenanceGit(t, f.cfg.Repo, "merge", "--squash", f.worker.Branch)
	maintenanceGit(t, f.cfg.Repo, "commit", "-m", "squash")
	merged := maintenanceGit(t, f.cfg.Repo, "rev-parse", "HEAD")
	gh := filepath.Join(strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0], "gh")
	setPR := func(head string) {
		maintenanceWrite(t, gh, "#!/bin/sh\nprintf '%s\\n' '[{\"headRefOid\":\""+head+"\",\"mergeCommit\":{\"oid\":\""+merged+"\"}}]'\n")
	}
	setPR(strings.Repeat("f", 40))
	if err := rpcCleanupMerged(f.cfg.Repo, f.worker.Branch, head, "main"); err == nil {
		t.Fatal("accepted merged branch at a different head")
	}
	setPR(head)
	if err := rpcCleanupMerged(f.cfg.Repo, f.worker.Branch, head, "main"); err != nil {
		t.Fatal("exact squash merge not recognized", err)
	}
	// New commits on a previously merged branch must still be preserved.
	maintenanceWrite(t, filepath.Join(f.worker.Cwd, "file"), "later work")
	maintenanceGit(t, f.worker.Cwd, "commit", "-am", "later work")
	if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err == nil {
		t.Fatal("deleted post-merge work")
	}
}

func TestRPCCleanupInterruptedRegistrationIsNotPruned(t *testing.T) {
	f := maintenanceFixture(t)
	backup := filepath.Join(filepath.Dir(f.cfg.Repo), "interrupted-checkout")
	f.env.git = func(dir string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
			if err := os.Rename(f.worker.Cwd, backup); err != nil {
				t.Fatal(err)
			}
			return "", errors.New("interrupted before registration removed")
		}
		return gitRun(dir, args...)
	}
	if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err == nil {
		t.Fatal("fixture interruption not reported")
	}
	f.env.git = gitRun
	if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err == nil || !strings.Contains(err.Error(), "still registered") {
		t.Fatal("pruned ambiguous registration", err)
	}
	if maintenanceRead(t, filepath.Join(backup, "file")) != "tracked\n" {
		t.Fatal("lost interrupted checkout")
	}
}

func TestRPCMaintenanceRevalidationAndPublicationFailure(t *testing.T) {
	t.Run("ignored-during-merge-lookup", func(t *testing.T) {
		f := maintenanceFixture(t)
		maintenanceWrite(t, filepath.Join(f.cfg.Repo, ".git", "info", "exclude"), "ignored\n")
		f.env.merged = func(_, _, _, _ string) error {
			maintenanceWrite(t, filepath.Join(f.worker.Cwd, "ignored"), "precious new file")
			return nil
		}
		if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err == nil || !strings.Contains(err.Error(), "changed during cleanup") {
			t.Fatal("late ignored file lost", err)
		}
		if maintenanceRead(t, filepath.Join(f.worker.Cwd, "ignored")) != "precious new file" {
			t.Fatal("late ignored file changed")
		}
	})
	t.Run("bead-reopened", func(t *testing.T) {
		f := maintenanceFixture(t)
		f.env.recheck = func() error { return errors.New("bead reopened") }
		if _, err := rpcRemoveWorktree(f.cfg, f.bead, f.worker, true, f.env); err == nil {
			t.Fatal("ignored closed-bead recheck")
		}
		if _, err := os.Stat(f.worker.Cwd); err != nil {
			t.Fatal("removed reopened checkout", err)
		}
	})
	t.Run("retention-before-publish", func(t *testing.T) {
		f := maintenanceFixture(t)
		path := filepath.Join(rpcDir(f.bead.ID), "events.jsonl")
		before := maintenanceRead(t, path)
		lease, _, err := rpcMaintenanceLock(f.bead.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		_, err = rpcRetainEvents(f.bead.ID, 2048, true, func() error { return errors.New("publication interrupted") })
		if err == nil || maintenanceRead(t, path) != before {
			t.Fatal("failed publication changed authoritative log", err)
		}
		if _, err := rpcRetainEvents(f.bead.ID, 2048, true, nil); err != nil {
			t.Fatal("publication retry failed", err)
		}
	})
}

func TestRPCMaintenanceAuditOnlyAppliedChanges(t *testing.T) {
	f := maintenanceFixture(t)
	f.cfg.Autonomy.Level, f.cfg.Autonomy.Max = 3, 3
	bin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	maintenanceWrite(t, filepath.Join(bin, "bd"), "#!/bin/sh\ncase \"$1\" in\n show) printf '%s\\n' '[{\"id\":\"demo-1\",\"status\":\"closed\"}]';;\n note) printf '%s\\n' \"$*\" >> \"$QUARTERMASTER_STATE_DIR/fixture-notes\";;\n *) exit 1;;\nesac\n")
	notes := filepath.Join(stateDir(), "fixture-notes")
	if err := rpcMaintenanceCmd(f.cfg, []string{"cleanup", f.bead.ID}, "worker", true, 4096, io.Discard); err == nil {
		t.Fatal("invalid cleanup arguments accepted")
	}
	if _, err := os.Stat(notes); !os.IsNotExist(err) {
		t.Fatal("noted refused action", err)
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", f.bead.ID}, "worker", true, 1<<20, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(notes); !os.IsNotExist(err) {
		t.Fatal("noted no-op", err)
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", f.bead.ID}, "worker", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	before := maintenanceRead(t, notes)
	if !strings.Contains(before, "completed offline RPC retain") {
		t.Fatal("missing applied outcome", before)
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", f.bead.ID}, "worker", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	if maintenanceRead(t, notes) != before {
		t.Fatal("repeat no-op added audit claim")
	}
}

func TestRPCMaintenanceForkAndSteerCompatibility(t *testing.T) {
	f := maintenanceFixture(t)
	id := f.bead.ID
	forkSession := filepath.Join(rpcDir(id), "sessions", "fork.jsonl")
	if err := os.Rename(f.session, forkSession); err != nil {
		t.Fatal(err)
	}
	f.worker.Fork = &rpcFork{Source: "src-1", SessionID: "saved-session", SessionFile: forkSession, Ready: true}
	if err := rpcSave(filepath.Join(rpcDir(id), "worker.json"), f.worker); err != nil {
		t.Fatal(err)
	}
	st := rpcStatus{Bead: id, Phase: "stopped", SessionID: "saved-session", SessionFile: forkSession}
	if err := rpcSave(filepath.Join(rpcDir(id), "status.json"), st); err != nil {
		t.Fatal(err)
	}
	steer := rpcReceipt{ID: "steer-1", Operation: "steer", Hash: rpcMessageHash("steer", "adjust"), State: "accepted", Response: rpcRecord{Type: "response", ID: "steer-1", Command: "steer", Success: true}}
	steerPath := filepath.Join(rpcDir(id), "requests", "steer-1.json")
	if err := rpcSave(steerPath, steer); err != nil {
		t.Fatal(err)
	}
	steerResp, _ := json.Marshal(steer.Response)
	logPath := filepath.Join(rpcDir(id), "events.jsonl")
	maintenanceWrite(t, logPath, string(steerResp)+"\n"+maintenanceRead(t, logPath))
	beforeWorker := maintenanceRead(t, filepath.Join(rpcDir(id), "worker.json"))
	beforeSession := maintenanceRead(t, forkSession)
	beforeSteer := maintenanceRead(t, steerPath)
	if err := rpcMaintenanceCmd(f.cfg, []string{"retain", id}, "TJ", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(maintenanceRead(t, logPath), `"id":"steer-1"`) {
		t.Fatal("retention dropped protected steer response")
	}
	if err := rpcMaintenanceCmd(f.cfg, []string{"cleanup", id}, "TJ", true, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	if maintenanceRead(t, filepath.Join(rpcDir(id), "worker.json")) != beforeWorker || maintenanceRead(t, forkSession) != beforeSession || maintenanceRead(t, steerPath) != beforeSteer {
		t.Fatal("maintenance altered fork provenance, session, or steer receipt")
	}
	if err := rpcForkNoCleanup(id); err == nil {
		t.Fatal("cleanup.json did not fence future fork preparation")
	}
}

func TestRPCRetentionConfig(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode("[rpc_retention]\nevent_bytes = 4096\n", &cfg); err != nil || cfg.RPCRetention.EventBytes != 4096 {
		t.Fatal(cfg.RPCRetention, err)
	}
	if defaultConfig().RPCRetention.EventBytes != 0 {
		t.Fatal("retention enabled by default")
	}
}
