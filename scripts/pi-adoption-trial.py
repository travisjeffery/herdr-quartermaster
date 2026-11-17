#!/usr/bin/env python3
"""Isolated real-pi TUI -> RPC adoption trial, with fixture Herdr and Beads.

Uses an already authorized model. No live panes, tasks, or settings change.
Retains private evidence; starts one tools-disabled TUI then its replacement.
"""
import argparse
import json
import os
from pathlib import Path
import pty
import signal
import socket
import subprocess
import tempfile
import threading
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--model", required=True)
parser.add_argument("--binary", help="test an installed binary instead of building")
options = parser.parse_args()
root = Path(tempfile.mkdtemp(prefix="qm-adopt-"))
root.chmod(0o700)
print(f"Evidence: {root}", flush=True)
for name in ("repo", "state", "config", "bin", "sessions"):
    (root / name).mkdir(mode=0o700)
repo, state, config = root / "repo", root / "state", root / "config"
bead = "adopt-trial"
branch = f"tj/{bead}-trial"
worktree = root / "checkout"
env = os.environ.copy()
for name in list(env):
    if name.startswith("HERDR_") or name.startswith("PI_SESSION_") or name == "QUARTERMASTER_RPC_PARK_FILE":
        env.pop(name)
env.update(QUARTERMASTER_STATE_DIR=str(state), QUARTERMASTER_CONFIG_DIR=str(config),
           PI_CODING_AGENT_SESSION_DIR=str(root / "sessions"), BEADS_DIR=str(root / "fixture-beads"),
           BEADS_ACTOR="pi", HERDR_PANE_ID="fixture-coordinator:p1", TERM="xterm-256color")
env["PATH"] = str(root / "bin") + os.pathsep + env["PATH"]
qm = Path(options.binary).resolve() if options.binary else root / "bin" / "quartermaster"
if not options.binary:
    subprocess.run(["go", "build", "-o", str(qm), "."], check=True)
subprocess.run(["git", "init", "-b", "main", str(repo)], check=True, capture_output=True)
subprocess.run(["git", "-C", str(repo), "-c", "user.name=Trial", "-c", "user.email=trial@invalid",
                "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture"], check=True, capture_output=True)
subprocess.run(["git", "-C", str(repo), "worktree", "add", "-b", branch, str(worktree), "main"], check=True, capture_output=True)
(worktree / "untracked.txt").write_text("Keep these untracked bytes.\n")
args = ["--no-approve", "--model", options.model, "--thinking", "high", "--no-tools", "--no-mcp", "--no-skills"]
config.joinpath("config.toml").write_text(f"repo = {json.dumps(str(repo))}\nbranch_prefix = \"tj/\"\n[agents.pi]\nargs = {json.dumps(args)}\n")
fixture = {"id": bead, "title": "adoption trial", "status": "in_progress", "assignee": "pi", "labels": [],
           "notes": "Protocol fixture only; no task actions authorized."}
(root / "bead.json").write_text(json.dumps(fixture))
(root / "bin" / "bd").write_text(f'''#!/usr/bin/env python3
import json,sys
from pathlib import Path
p=Path({str(root)!r})
with (p/'bd-calls.jsonl').open('a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')
if sys.argv[1] in ('show','list'): print('['+(p/'bead.json').read_text()+']')
''')
(root / "bin" / "bd").chmod(0o700)
herdr = root / "bin" / "herdr"
herdr.write_text(f'''#!/usr/bin/env python3
import json,sys
from pathlib import Path
p=Path({str(root)!r})
key={{('agent','list'):'agents.json',('pane','process-info'):'process.json',('worktree','list'):'worktrees.json' }}.get(tuple(sys.argv[1:3]))
if not key: sys.exit(1)
print((p/key).read_text())
''')
herdr.chmod(0o700)
env["HERDR_BIN_PATH"] = str(herdr)

def run(*argv, check=True):
    r = subprocess.run([str(qm), *argv], env=env, cwd=worktree, capture_output=True, text=True, timeout=45)
    if check and r.returncode:
        raise RuntimeError(f"{argv}: {r.stdout} {r.stderr}")
    return r

# Generate only the embedded guard via the isolated binary's model preflight.
# A nonexistent Herdr source refuses prepare before launch; use the embedded
# checkout source copied to scratch, which the binary validates/rewrites later.
extension = state / "pi" / "quartermaster.ts"
extension.parent.mkdir(mode=0o700)
extension.write_bytes(Path("pi/quartermaster.ts").read_bytes())
control = state / "pi" / "control" / f"{bead}.sock"
argv = ["pi", *args, "-e", str(extension), "--quartermaster-control", str(control), "--session-id", bead]
master, slave = pty.openpty()
old = subprocess.Popen(argv, cwd=worktree, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
os.close(slave)

def drain():
    with (root / "tui.log").open("wb") as log:
        try:
            while data := os.read(master, 65536):
                log.write(data)
        except OSError:
            pass

threading.Thread(target=drain, daemon=True).start()

def control_call(op, **kwargs):
    token = Path(str(control) + ".token").read_text().strip()
    with socket.socket(socket.AF_UNIX) as s:
        s.settimeout(5)
        s.connect(str(control))
        s.sendall((json.dumps(dict(op=op, token=token, **kwargs)) + "\n").encode())
        return json.loads(s.makefile().readline())

def wait_until(fn, timeout=45):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError):
            pass
        time.sleep(.2)
    raise TimeoutError("trial checkpoint timed out")

def status():
    return json.loads(run("rpc", "status", bead).stdout)["status"]

try:
    wait_until(lambda: control.exists() and Path(str(control)+".token").exists())
    reply = control_call("prompt", text="Transport fixture only. Reply ADOPTION_HISTORY_SENTINEL. Do not use any tools.")
    assert reply["ok"], reply
    def saved():
        files = list((root / "sessions").rglob(f"*_{bead}.jsonl"))
        if len(files) != 1:
            return None
        text = files[0].read_text()
        return files[0] if '"role":"assistant"' in text and control_call("ping")["idle"] else None
    session = wait_until(saved, 120)
    agent = dict(name=bead, agent="pi", agent_status="idle", pane_id="fixture:p1", cwd=str(worktree),
                 agent_session=dict(agent="pi", kind="path", value=str(session)))
    (root / "agents.json").write_text(json.dumps({"result": {"agents": [agent]}}))
    (root / "process.json").write_text(json.dumps({"result": {"process_info": {"shell_pid": os.getpid(),
        "foreground_processes": [dict(pid=old.pid, argv=argv, cwd=str(worktree))]}}}))
    (root / "worktrees.json").write_text(json.dumps({"result": {"worktrees": [dict(branch=branch, path=str(worktree), is_linked_worktree=True)]}}))
    print(run("rpc", "prepare", bead, "fixture:p1").stdout, flush=True)
    assert run("rpc", "adopt", bead, check=False).returncode != 0
    assert run("dispatch", bead, "--backend", "herdr", check=False).returncode != 0
    history = session.read_bytes()
    # Graceful termination of only the fixture's idle process.
    old.send_signal(signal.SIGTERM)
    old.wait(timeout=15)
    (root / "agents.json").write_text(json.dumps({"result": {"agents": []}}))
    (root / "process.json").write_text(json.dumps({"result": {"process_info": {"shell_pid": os.getpid(),
        "foreground_processes": [dict(pid=os.getpid())]}}}))
    print(run("rpc", "adopt", bead).stdout, flush=True)
    st = status()
    assert st["phase"] == "parked" and st["session_file"] == str(session) and st["session_id"] == bead, st
    assert session.read_bytes().startswith(history)
    assert run("tell", bead, "No task work; this must be refused", check=False).returncode != 0
    assert list((state / "rpc" / bead / "requests").glob("*.json")) == []
    assert "agent_start" not in (state / "rpc" / bead / "events.jsonl").read_text()
    # Blockers are checked on explicit activation too; no implicit approval.
    fixture["status"] = "needs_me"
    (root / "bead.json").write_text(json.dumps(fixture))
    assert run("rpc", "activate", bead, check=False).returncode != 0
    assert status()["phase"] == "parked"
    fixture["status"] = "in_progress"
    (root / "bead.json").write_text(json.dumps(fixture))
    print(run("rpc", "activate", bead).stdout, flush=True)
    print(run("tell", bead, "Transport fixture only. Reply ADOPTION_RPC_OK. Do not use tools.").stdout, flush=True)
    wait_until(lambda: status()["phase"] == "idle", 120)
    assert "ADOPTION_RPC_OK" in run("rpc", "output", bead).stdout
    assert (worktree / "untracked.txt").read_text() == "Keep these untracked bytes.\n"
    print(json.dumps(dict(result="PASS", session=str(session), old_pid=old.pid, new_pid=st["pi_pid"],
                          assertions="same history; no concurrent owner; parked tell refused; needs_me activation refused; explicit tell works; untracked retained")), flush=True)
finally:
    run("rpc", "stop", bead, check=False)
    if old.poll() is None:
        old.terminate()
        try:
            old.wait(timeout=15)
        except subprocess.TimeoutExpired:
            old.kill()
            old.wait()
    os.close(master)
    print(f"Stopped fixture processes; evidence retained at {root}", flush=True)
