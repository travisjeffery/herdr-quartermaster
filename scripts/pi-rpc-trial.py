#!/usr/bin/env python3
"""Isolated, bounded real-pi trial. Uses a fixture bd, never the user's beads.

Run after no-model tests: python3 scripts/pi-rpc-trial.py --model PROVIDER/MODEL
Requires existing pi authentication, Go, Git and Python; installs nothing.
Leaves the private scratch directory for inspection. Stops only its own worker.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--model", required=True, help="an already-authorized subscription model")
args = parser.parse_args()
root = Path(tempfile.mkdtemp(prefix="qm-rpc-trial-"))
root.chmod(0o700)
print(f"Trial artifacts: {root}", flush=True)
repo = root / "repo"
config = root / "config"
state = root / "state"
bin_dir = root / "bin"
for d in (repo, config, state, bin_dir):
    d.mkdir(mode=0o700)
env = os.environ.copy()
env.update(QUARTERMASTER_STATE_DIR=str(state), QUARTERMASTER_CONFIG_DIR=str(config),
           BEADS_DIR=str(root / "fixture-beads"), HERDR_BIN_PATH=str(root / "no-herdr"))
env["PATH"] = str(bin_dir) + os.pathsep + env["PATH"]
# A fake bead for this trial only; all writes are logged to this private dir.
bd = bin_dir / "bd"
bd.write_text("""#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
with (Path(os.environ['QUARTERMASTER_STATE_DIR']) / 'bd-fixture.log').open('a') as f:
    f.write(json.dumps(sys.argv[1:])+'\\n')
if sys.argv[1] in ('show', 'list'):
    print(json.dumps([{'id':'rpc-trial','title':'Harmless native RPC smoke',
        'status':'in_progress','assignee':'pi','labels':[],
        'description':'Protocol trial only. Reply RPC_SMOKE_READY. Do not use tools.'}]))
""")
bd.chmod(0o700)
config.joinpath("config.toml").write_text(
    f'repo = {json.dumps(str(repo))}\nbeads_dir = {json.dumps(env["BEADS_DIR"])}\n'
    'base_branch = "main"\n[agents.pi]\n'
    f'args = ["--model", {json.dumps(args.model)}, "--no-tools", "--no-mcp", "--no-skills"]\n')
qm = bin_dir / "quartermaster"
subprocess.run(["go", "build", "-o", str(qm), "."], check=True)
subprocess.run(["git", "init", "-b", "main", str(repo)], check=True, capture_output=True)
subprocess.run(["git", "-C", str(repo), "-c", "user.name=RPC Trial", "-c", "user.email=trial@invalid",
                "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "Isolated RPC trial"],
               check=True, capture_output=True)

def run(*argv, check=True):
    p = subprocess.run([str(qm), *argv], env=env, text=True, capture_output=True, timeout=45)
    if check and p.returncode:
        raise RuntimeError(f'{argv}: {p.stdout}\n{p.stderr}')
    return p

def status():
    return json.loads(run("rpc", "status", "rpc-trial").stdout)["status"]

def settled(timeout=90):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        st = status()
        if st["phase"] == "idle":
            return st
        if st["phase"] in ("blocked", "failed", "stopped", "disconnected"):
            raise RuntimeError(st)
        time.sleep(0.25)
    raise TimeoutError("pi did not settle in 90s")

try:
    # Dispatch tests worktree isolation, guards, attribution, and real pipes.
    print(run("dispatch", "rpc-trial", "--backend", "pi-rpc", "--agent", "pi").stdout, flush=True)
    settled()
    # Separate CLI clients reconnect while the supervisor/process stays put.
    first = status()
    prompt = "This is a harmless transport test. Reply exactly RPC_SMOKE_OK. Do not use tools."
    print(run("rpc", "tell", "rpc-trial", "smoke-message", prompt).stdout, flush=True)
    settled()
    assert status()["supervisor_pid"] == first["supervisor_pid"]
    assert "RPC_SMOKE_OK" in run("rpc", "output", "rpc-trial").stdout
    # Same request id reconciles, never repeats the prompt.
    print(run("rpc", "tell", "rpc-trial", "smoke-message", prompt).stdout, flush=True)
    print(run("rpc", "cancel", "rpc-trial").stdout, flush=True)
    print(run("rpc", "stop", "rpc-trial").stdout, flush=True)
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline and status()["phase"] != "disconnected":
        time.sleep(0.1)
    print(run("rpc", "resume", "rpc-trial").stdout, flush=True)
    resumed = status()
    assert resumed["supervisor_pid"] != first["supervisor_pid"]
    assert resumed["session_id"] == first["session_id"]
    assert resumed["session_file"] == first["session_file"]
    print(run("rpc", "receipt", "rpc-trial", "smoke-message").stdout, flush=True)
    assert run("dispatch", "rpc-trial", "--backend", "herdr", check=False).returncode != 0
    print(json.dumps({"result": "PASS", "reconnected_pid": first["supervisor_pid"],
                      "resumed_pid": resumed["supervisor_pid"], "session_file": resumed["session_file"]}), flush=True)
finally:
    run("rpc", "stop", "rpc-trial", check=False)
    print(f"Retained logs/session/worktree under {root}; no live settings or sessions changed.", flush=True)
