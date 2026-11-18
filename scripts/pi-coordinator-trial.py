#!/usr/bin/env python3
"""Real pi/PTY coordinator inbox regression. Loopback fixture model; no credentials,
network model, live settings, panes, tasks or workers. Private evidence retained.
"""
import argparse
import fcntl
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import pty
import signal
import socket
import struct
import subprocess
import tempfile
import termios
import threading
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--pi", default="pi")
parser.add_argument("--extension", default="pi/quartermaster.ts")
parser.add_argument("--tui-mode", choices=["regular", "fullscreen"], default="fullscreen")
options = parser.parse_args()
source = Path.cwd()
root = Path(tempfile.mkdtemp(prefix="qm-coord-"))
root.chmod(0o700)
print(f"Evidence: {root}", flush=True)
for name in ["agent", "sessions", "cwd", "state/pi/control"]:
    (root / name).mkdir(mode=0o700, parents=True)
hold = threading.Event()
hold.set()
requests = []

class Model(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass
    def do_POST(self):
        payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        requests.append(payload)
        if not hold.wait(20):
            self.send_error(503)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for delta, finish in [({"role": "assistant", "content": "FIXTURE_OK"}, None), ({}, "stop")]:
            chunk = {"id": "fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture",
                     "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

http = ThreadingHTTPServer(("127.0.0.1", 0), Model)
threading.Thread(target=http.serve_forever, daemon=True).start()
(root / "agent" / "models.json").write_text(json.dumps({"providers": {"fixture": {
    "baseUrl": f"http://127.0.0.1:{http.server_port}/v1", "api": "openai-completions", "apiKey": "fixture",
    "models": [{"id": "fixture", "contextWindow": 32768, "maxTokens": 1024}]}}}))
(root / "agent" / "settings.json").write_text(json.dumps({"quietStartup": True, "compaction": {"enabled": False, "keepRecentTokens": 100}, "retry": {"enabled": False}}))
env = {k: v for k, v in os.environ.items() if not k.startswith(("HERDR_", "PI_", "QUARTERMASTER_", "BEADS_"))}
env.update(PI_CODING_AGENT_DIR=str(root / "agent"), PI_CODING_AGENT_SESSION_DIR=str(root / "sessions"),
           QM_TRIAL_DIR=str(root), TERM="xterm-256color", QUARTERMASTER_PI_BASH_HOOKS="")
sock = root / "state" / "pi" / "control" / "coordinator-inbox.sock"
process = None
master = None

def snapshot():
    return json.loads((root / "snapshot.json").read_text())

def wait(fn, seconds=15):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError, KeyError):
            pass
        time.sleep(.05)
    raise AssertionError("checkpoint timed out; inspect private evidence")

def start(session=None):
    global process, master
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 45, 120, 0, 0))
    argv = [options.pi, "--offline", "--no-approve", "--no-extensions", "--no-mcp", "--no-skills", "--no-tools",
            "--no-context-files", "--no-prompt-templates", "--model", "fixture/fixture", "--thinking", "off",
            "--tui-mode", options.tui_mode, "-e", str(source / "testdata/pi-coordinator-observer.ts"),
            "-e", str(Path(options.extension).resolve()), "--quartermaster-coordinator", str(sock)]
    if session:
        argv += ["--session", session]
    process = subprocess.Popen(argv, cwd=root / "cwd", env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
    os.close(slave)
    fd = master
    def drain():
        with (root / f"tui-{process.pid}.log").open("wb") as log:
            try:
                while data := os.read(fd, 65536):
                    log.write(data)
                    log.flush()
            except OSError:
                pass
    threading.Thread(target=drain, daemon=True).start()
    wait(lambda: sock.exists() and Path(str(sock) + ".token").exists())
    wait(lambda: snapshot()["focus"] == "CustomEditor" and snapshot()["pid"] == process.pid)
    wait(lambda: call("inbox_status")["ok"])

def stop(crash=False):
    if process and process.poll() is None:
        process.send_signal(signal.SIGKILL if crash else signal.SIGTERM)
        process.wait(timeout=10)
    if master is not None:
        os.close(master)

def send(text):
    os.write(master, text.encode())

def call(op="inbox", through=None, session=None, token=None):
    req = {"op": op, "session": session or snapshot()["session"],
           "token": token or Path(str(sock) + ".token").read_text()}
    if through:
        req["through"] = str(through)
    with socket.socket(socket.AF_UNIX) as client:
        client.settimeout(3)
        client.connect(str(sock))
        client.sendall((json.dumps(req) + "\n").encode())
        return json.loads(client.makefile().readline())

def delivered(n):
    return str(n) in snapshot()["entries"]

def settled():
    return snapshot()["idle"] and call("inbox_status")["queued"] is False

try:
    start()
    send("fixture seed\r")
    wait(lambda: requests and snapshot()["idle"] and Path(snapshot()["session"]).exists())
    draft = "half written\n日本語 👩🏽‍💻 e\u0301\nlast"
    send("\x1b[200~" + draft + "\x1b[201~\x1b[D\x1b[D")
    before = wait(lambda: snapshot() if snapshot()["text"] == draft else None)
    c = 1700000000000000001
    for _ in range(3):
        assert call(through=c)["ok"]
    time.sleep(1.5)
    after = snapshot()
    assert (before["text"], before["cursor"]) == (after["text"], after["cursor"]), (before, after)
    assert not delivered(c)
    assert call("inbox_status")["queued"]
    assert len(requests) == 1
    # Insert a character at the preserved cursor, then submit the draft ONLY
    # with fixture user keystrokes. Human prompt must precede the custom event.
    send("X")
    wait(lambda: snapshot()["text"].endswith("laXst"))
    send("\r")
    wait(lambda: delivered(c) and settled())
    inputs = [json.loads(line) for line in (root / "inputs.jsonl").read_text().splitlines()]
    assert len(inputs) == 2 and all(i["source"] == "interactive" for i in inputs), inputs
    assert len(requests) == 2 # idle custom context does not start a competing run
    assert call(through=c)["ok"]
    time.sleep(1.3)
    assert len(requests) == 2
    print("PASS: Unicode multiline draft/cursor, user priority, empty editor, custom origin and duplicate reconnect", flush=True)

    # A notification arriving during an actual model request stays pending.
    hold.clear()
    send("active fixture turn\r")
    wait(lambda: not snapshot()["idle"] and len(requests) == 3)
    assert call(through=c+1)["ok"]
    send("draft during turn")
    hold.set()
    wait(lambda: snapshot()["idle"])
    time.sleep(1.3)
    assert not delivered(c+1) and snapshot()["text"] == "draft during turn"
    send("\x15") # real editor clear-line binding, fixture only
    wait(lambda: snapshot()["text"] == "")
    wait(lambda: delivered(c+1) and settled())
    print("PASS: active turn and newly composed pending draft", flush=True)

    count = len(requests)
    hold.clear()
    send("boundary fixture turn\r")
    wait(lambda: not snapshot()["idle"] and len(requests) == count + 1)
    assert call(through=c+2)["ok"]
    time.sleep(1.3)
    hold.set()
    wait(lambda: delivered(c+2) and settled())
    assert len(requests) == count + 2 # one final-boundary continuation, no separate idle run
    c += 1
    print("PASS: active final-boundary delivery continues exactly once", flush=True)

    count = len(requests)
    (root / "slow-input").write_text("hold human preflight")
    send("slow fixture prompt\r")
    wait(lambda: (root / "input-waiting").exists())
    assert call(through=c+2)["ok"]
    wait(lambda: delivered(c+2) and settled())
    assert len(requests) == count # custom context must not race the human run
    (root / "slow-input").unlink()
    wait(lambda: len(requests) == count + 1 and snapshot()["idle"])
    transcript = [json.loads(line) for line in Path(snapshot()["session"]).read_text().splitlines()]
    assert any(e.get("type") == "message" and e["message"].get("role") == "user" and "slow fixture prompt" in json.dumps(e["message"]["content"]) for e in transcript)
    c += 1
    print("PASS: delayed human input preflight is never rejected by an idle notification", flush=True)

    send("/fixture-dialog\r")
    wait(lambda: snapshot()["focus"] != "CustomEditor")
    assert call(through=c+2)["ok"]
    time.sleep(1.3)
    assert not delivered(c+2)
    send("\x1b")
    wait(lambda: delivered(c+2) and settled())
    send("/model\r")
    wait(lambda: snapshot()["focus"] != "CustomEditor")
    assert call(through=c+3)["ok"]
    time.sleep(1.3)
    assert not delivered(c+3)
    send("\x1b")
    wait(lambda: delivered(c+3) and settled())
    print("PASS: extension dialog and built-in model selector", flush=True)

    hold.clear()
    send("/compact\r")
    wait(lambda: not snapshot()["idle"])
    assert call(through=c+4)["ok"]
    time.sleep(1.3)
    assert not delivered(c+4)
    hold.set()
    wait(lambda: delivered(c+4) and settled())
    print("PASS: real manual compaction", flush=True)

    (root / "cancel-tree").write_text("hold")
    send("/fixture-cancel-tree\r")
    wait(lambda: (root / "tree-waiting").exists() and not snapshot()["idle"])
    assert call(through=c+5)["ok"]
    time.sleep(1.3)
    assert not delivered(c+5)
    (root / "cancel-tree").unlink()
    wait(lambda: delivered(c+5) and settled())
    print("PASS: canceled real tree navigation recovers without another prompt", flush=True)
    c += 1

    # Crash with a pending event, reopen the exact saved session. No session
    # or draft restoration is asserted of pi itself; notification code never
    # writes the editor. The durable event survives the stale endpoint.
    send("restart fixture draft")
    wait(lambda: snapshot()["text"] == "restart fixture draft")
    session = snapshot()["session"]
    assert call(through=c+5)["ok"]
    stop(crash=True)
    start(session)
    wait(lambda: delivered(c+5) and settled())
    assert snapshot()["entries"].count(str(c+5)) == 1
    assert not call(through=c+6, session="/stale/session")["ok"]
    assert not call(through=c+6, token="invalid")["ok"]
    assert not call("prompt")["ok"]
    print("PASS: crash/restart, stale socket, saved receipt, authentication and session pin", flush=True)

    # Exercise the actual Go reconciler against the real authenticated pi
    # endpoint and its durable receipt after archival. No live ticker or task
    # database is used. The fixture test writes only this scratch state dir.
    (root / "reconcile.json").write_text(json.dumps({"session": session, "through": str(c+5)}))
    count = len(requests)
    receipt_count = len(snapshot()["entries"])
    subprocess.run(["go", "test", "-buildvcs=false", ".", "-run", "^TestPiCoordinatorPTYReconcile$", "-count=1"],
                   cwd=source, env={**env, "QM_COORDINATOR_FIXTURE": str(root)}, check=True, timeout=120)
    time.sleep(1.3)
    assert len(requests) == count and len(snapshot()["entries"]) == receipt_count
    print("PASS: Go reconciles newest/all archived events with real pi receipt; no replay or model turn", flush=True)

    # Session replacement must neither deliver old pending events into the new
    # session nor accept requests addressed to the old one.
    send("pending across switch")
    wait(lambda: snapshot()["text"] == "pending across switch")
    assert call(through=c+7)["ok"]
    send("\x15/new\r")
    wait(lambda: snapshot()["session"] != session)
    wait(lambda: sock.exists())
    assert not call(through=c+7, session=session)["ok"]
    time.sleep(1.3)
    assert snapshot()["entries"] == []
    send("new session fixture seed\r")
    wait(lambda: snapshot()["idle"] and Path(snapshot()["session"]).exists())
    assert call(through=c+7)["ok"] # fresh ticker identity explicitly revalidates pending work
    wait(lambda: delivered(c+7) and settled())
    stop()
    start(session)
    assert call(through=c+7)["ok"]
    time.sleep(1.3)
    assert not delivered(c+7) and settled() # global ack prevents replay into A
    print("PASS: session replacement rejects stale delivery, revalidates pending work, and shares acknowledgements", flush=True)
    (root / "result.json").write_text(json.dumps({"result": "PASS", "pi": options.pi, "tui_mode": options.tui_mode,
                                                 "requests": len(requests), "session": session}))
finally:
    hold.set()
    stop()
    http.shutdown()
    print(f"Stopped isolated processes; evidence retained at {root}", flush=True)
