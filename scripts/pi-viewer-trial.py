#!/usr/bin/env python3
"""Read-only viewer trial: real pi, fictional saved history, isolated PTYs.

No prompts/model calls, real Beads writes, Herdr connection or installation.
The only lifecycle actions create/terminate this script's scratch supervisor.
Evidence stays in the printed private directory.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--model', required=True, help='An already configured provider/model; never prompted')
p.add_argument('--binary')
o = p.parse_args()
root = Path(tempfile.mkdtemp(prefix='qm-view-'))
root.chmod(0o700)
print(f'Evidence: {root}', flush=True)
for name in ('state', 'config', 'bin', 'checkout', 'adopted'):
    (root / name).mkdir(mode=0o700)
state, config, checkout = (root / n for n in ('state', 'config', 'checkout'))
worker = state / 'rpc' / 'demo-view'
worker.mkdir(parents=True, mode=0o700)
qm = Path(o.binary).resolve() if o.binary else root / 'bin' / 'quartermaster'
if not o.binary:
    subprocess.run(['go', 'build', '-buildvcs=false', '-o', str(qm), '.'], check=True)
# No fixture impersonates a live pane; Herdr is deliberately unavailable.
env = os.environ.copy()
for name in list(env):
    if name.startswith(('HERDR_', 'PI_SESSION_')) or name in ('QUARTERMASTER_EXEC_SOCKET', 'QUARTERMASTER_RPC_PARK_FILE', 'QUARTERMASTER_RPC_HANDOFF_FILE'):
        env.pop(name)
env.update(QUARTERMASTER_STATE_DIR=str(state), QUARTERMASTER_CONFIG_DIR=str(config),
           BEADS_DIR=str(root / 'fixture-beads'), HERDR_BIN_PATH=str(root / 'no-herdr'))
env['PATH'] = str(root / 'bin') + os.pathsep + env['PATH']
(config / 'config.toml').write_text(f'repo = {json.dumps(str(checkout))}\n')
bead = dict(id='demo-view', title='Example viewer task', status='in_progress', priority=2,
            notes='PR: https://example.test/pull/1', labels=[])
(root / 'bead.json').write_text(json.dumps(bead))
(root / 'bin' / 'bd').write_text(f'''#!/usr/bin/env python3
import sys
from pathlib import Path
p=Path({str(root)!r})
with (p/'bd-calls.log').open('a') as f: f.write(' '.join(sys.argv[1:])+'\\n')
if len(sys.argv)>1 and sys.argv[1] in ('show','list'): print('['+(p/'bead.json').read_text()+']')
else: sys.exit(1)
''')
(root / 'bin' / 'bd').chmod(0o700)
provider, model = o.model.split('/', 1)
session = root / 'adopted' / 'saved.jsonl'
entries = [dict(type='session', version=3, id='demo-view', timestamp='2026-01-02T03:04:05Z', cwd=str(checkout)),
           dict(type='model_change', id='model', parentId=None, timestamp='2026-01-02T03:04:05Z', provider=provider, modelId=model)]
for i in range(150):
    entries.append(dict(type='message', id=f'entry-{i}', parentId='model' if i == 0 else f'entry-{i-1}',
                        timestamp='2026-01-02T03:04:05Z', message=dict(role='assistant', provider=provider,
                        model=model, api='openai-codex-responses', stopReason='stop', timestamp=1767323045000,
                        content=[dict(type='text', text=f'History fixture {i}. password=fixture-private-value\nhttps://example.test/pull/1')],
                        usage=dict(input=10, output=2, cacheRead=0, cacheWrite=0, totalTokens=12,
                                   cost=dict(input=0, output=0, cacheRead=0, cacheWrite=0, total=0)))))
session.write_text(''.join(json.dumps(e) + '\n' for e in entries))
args = ['--mode', 'rpc', '--model', o.model, '--no-tools', '--no-mcp', '--no-skills', '--no-approve',
        '-e', str(Path('pi/quartermaster.ts').resolve()), '--quartermaster-control', str(state / 'pi' / 'control' / 'demo-view.sock')]
(worker / 'worker.json').write_text(json.dumps(dict(bead='demo-view', cwd=str(checkout), branch='fixture', args=args)))
(worker / 'status.json').write_text(json.dumps(dict(bead='demo-view', phase='stopped', session_id='demo-view', session_file=str(session))))
(worker / 'parked').write_text('fixture barrier\n')


def run(*args):
    r = subprocess.run([str(qm), *args], cwd=checkout, env=env, capture_output=True, text=True, timeout=15)
    if r.returncode:
        raise RuntimeError(f'{args}: {r.stderr}')
    return json.loads(r.stdout)


def wait(fn, seconds=40):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if fn():
            return
        time.sleep(.1)
    raise TimeoutError('fixture checkpoint')


def fingerprint():
    # Only files a viewer must never change. Board's own cache is not task state.
    files = [session, worker / 'status.json', worker / 'worker.json', worker / 'parked', root / 'bead.json', worker / 'events.jsonl']
    return {str(f.relative_to(root)): hashlib.sha256(f.read_bytes()).hexdigest() for f in files if f.exists()}


def ui(command, keys, name):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 30, 120, 0, 0))
    proc = subprocess.Popen([str(qm), *command], stdin=slave, stdout=slave, stderr=slave, env=env, cwd=checkout)
    os.close(slave)
    output = bytearray()

    def drain(seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if select.select([master], [], [], .05)[0]:
                try:
                    output.extend(os.read(master, 65536))
                except OSError:
                    break

    try:
        drain(2)
        for key in keys:
            os.write(master, key)
            drain(.8)
        proc.wait(timeout=10)
        drain(.1)
        assert proc.returncode == 0, output.decode(errors='replace')[-2000:]
        assert b'READ ONLY' in output, name
        assert b'fixture-private-value' not in output, name
        (root / f'{name}.pty').write_bytes(output)
    finally:
        if proc.poll() is None:
            proc.terminate()
            proc.wait(timeout=10)
        os.close(master)


supervisor_log = (root / 'supervisor.log').open('w')
supervisor = subprocess.Popen([str(qm), 'rpc', 'serve', 'demo-view'], env=env, cwd=checkout, stdout=supervisor_log, stderr=supervisor_log)
try:
    wait(lambda: (worker / 'control.sock').exists() or supervisor.poll() is not None)
    assert supervisor.poll() is None, (root / 'supervisor.log').read_text()
    assert run('rpc', 'inspect', 'demo-view')['state'] == 'parked'
    before = fingerprint()
    cursor, count = '', 0
    while True:
        page = run('rpc', 'history', 'demo-view', *([cursor] if cursor else []))
        count += len(page['events'])
        assert 'fixture-private-value' not in json.dumps(page)
        cursor = page['cursor']
        if not page['more']:
            break
    assert count == 151, count  # selected model plus saved assistant records
    assert run('rpc', 'history', 'demo-view', cursor)['events'] == []
    ui(['rpc', 'view', 'demo-view'], [b'e', b'h', b'\r', b'J', b'g', b'n', b'f', b'q'], 'direct')
    ui(['board'], [b'\r', b'A R c x m i 9', b'e', b'h', b'q', b'q'], 'board')
    # Same-read trailing board keys must not execute after the viewer returns.
    ui(['board'], [b'Vn', b'q', b'q'], 'board-buffered')
    assert fingerprint() == before, 'viewing mutated worker/task files'
    assert 'agent_start' not in (worker / 'events.jsonl').read_text()
    calls = (root / 'bd-calls.log').read_text().splitlines()
    assert all(line.split()[0] in ('show', 'list') for line in calls), calls
    # Only the trial owner stops its own process. A stopped view must not restart it.
    supervisor.send_signal(signal.SIGTERM)
    supervisor.wait(timeout=20)
    before = fingerprint()
    assert run('rpc', 'inspect', 'demo-view')['state'] == 'stopped'
    ui(['rpc', 'view', 'demo-view'], [b'\r', b'q'], 'stopped')
    assert fingerprint() == before and not (worker / 'control.sock').exists()
    result = dict(result='PASS', assertions='real pi; adopted-path history; cursor replay; board/direct/stopped PTYs; redaction; no lifecycle/task mutation; no prompts or model calls', history_records=count)
    (root / 'result.json').write_text(json.dumps(result, indent=2))
    print(json.dumps(result), flush=True)
finally:
    if supervisor.poll() is None:
        supervisor.send_signal(signal.SIGTERM)
        supervisor.wait(timeout=20)
    supervisor_log.close()
    print(f'Fixture stopped; private evidence retained at {root}', flush=True)
