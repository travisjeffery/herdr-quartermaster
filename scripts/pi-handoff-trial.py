#!/usr/bin/env python3
"""Isolated fresh legacy-worker -> real pi RPC trial; no live tasks or panes.

Uses a sleeping source process with a retained legacy transcript, fixture
Herdr/Beads, and a real tools-disabled pi. Evidence stays in a private directory.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--model', required=True)
p.add_argument('--binary')
o = p.parse_args()
root = Path(tempfile.mkdtemp(prefix='qm-handoff-'))
root.chmod(0o700)
print(f'Evidence: {root}', flush=True)
for name in ('repo', 'state', 'config', 'bin'):
    (root / name).mkdir(mode=0o700)
repo, state, config = (root / n for n in ('repo', 'state', 'config'))
bead, branch = 'handoff-trial', 'tj/handoff-trial-work'
worktree = root / 'checkout'
env = os.environ.copy()
for name in list(env):
    if name.startswith(('HERDR_', 'PI_SESSION_')) or name in ('QUARTERMASTER_RPC_PARK_FILE', 'QUARTERMASTER_RPC_HANDOFF_FILE'):
        env.pop(name)
env.update(QUARTERMASTER_STATE_DIR=str(state), QUARTERMASTER_CONFIG_DIR=str(config),
           BEADS_DIR=str(root / 'fixture-beads'), BEADS_ACTOR='pi', HERDR_PANE_ID='caller:p1')
env['PATH'] = str(root / 'bin') + os.pathsep + env['PATH']
qm = Path(o.binary).resolve() if o.binary else root / 'bin' / 'quartermaster'
if not o.binary:
    subprocess.run(['go', 'build', '-o', str(qm), '.'], check=True)
subprocess.run(['git', 'init', '-b', 'main', str(repo)], check=True, capture_output=True)
subprocess.run(['git', '-C', str(repo), '-c', 'user.name=Trial', '-c', 'user.email=trial@invalid', '-c', 'commit.gpgsign=false', 'commit', '--allow-empty', '-m', 'fixture'], check=True, capture_output=True)
subprocess.run(['git', '-C', str(repo), 'worktree', 'add', '-b', branch, str(worktree), 'main'], check=True, capture_output=True)
(worktree / 'untracked.txt').write_text('Retain these exact bytes.\n')
transcript = root / 'legacy-session.jsonl'
transcript.write_text(json.dumps({'type': 'assistant', 'text': 'Awaiting explicit deployment approval.'}) + '\n')
history = transcript.read_bytes()
context = 'HANDOFF_CONTEXT_SENTINEL. Question: may I deploy? Unanswered. No deployment or blocked task action is authorized.'
(root / 'handoff.json').write_text(json.dumps(dict(cwd=str(worktree), transcript=str(transcript), context=context)))
args = ['--no-approve', '--model', o.model, '--thinking', 'high', '--no-tools', '--no-mcp', '--no-skills']
(config / 'config.toml').write_text(f'repo = {json.dumps(str(repo))}\nbranch_prefix = "tj/"\n[agents.pi]\nargs = {json.dumps(args)}\n')
fixture = dict(id=bead, title='fresh handoff fixture', status='needs_me', assignee='claude', labels=[], notes='Waiting for deployment approval; transport only.')
(root / 'bead.json').write_text(json.dumps(fixture))
(root / 'bin' / 'bd').write_text(f'''#!/usr/bin/env python3
import sys
from pathlib import Path
p=Path({str(root)!r})
if sys.argv[1] in ('show','list'): print('['+(p/'bead.json').read_text()+']')
''')
(root / 'bin' / 'bd').chmod(0o700)
herdr = root / 'bin' / 'herdr'
herdr.write_text(f'''#!/usr/bin/env python3
import sys
from pathlib import Path
p=Path({str(root)!r})
key={{('agent','list'):'agents.json',('pane','process-info'):'process.json',('worktree','list'):'worktrees.json'}}.get(tuple(sys.argv[1:3]))
if not key: sys.exit(1)
print((p/key).read_text())
''')
herdr.chmod(0o700)
env['HERDR_BIN_PATH'] = str(herdr)
old = subprocess.Popen(['sleep', '300'], cwd=worktree, start_new_session=True)
agent = dict(name=bead, agent='claude', agent_status='blocked', pane_id='source:p1', cwd=str(worktree), agent_session=dict(agent='claude', kind='id', value='legacy-session'))
(root / 'agents.json').write_text(json.dumps({'result': {'agents': [agent]}}))
(root / 'worktrees.json').write_text(json.dumps({'result': {'worktrees': [dict(branch=branch, path=str(worktree), is_linked_worktree=True)]}}))

def process(pid):
    (root / 'process.json').write_text(json.dumps({'result': {'process_info': {'shell_pid': os.getpid(), 'foreground_processes': [dict(pid=pid, cwd=str(worktree))]}}}))


def run(*argv, check=True):
    r = subprocess.run([str(qm), *argv], cwd=worktree, env=env, capture_output=True, text=True, timeout=45)
    if check and r.returncode:
        raise RuntimeError(f'{argv}: {r.stdout} {r.stderr}')
    return r


def status():
    return json.loads(run('rpc', 'status', bead).stdout)['status']


def wait(fn, seconds=120):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if fn():
            return
        time.sleep(.2)
    raise TimeoutError('trial checkpoint timed out')

process(old.pid)
try:
    print(run('rpc', 'handoff-prepare', bead, 'source:p1', str(root / 'handoff.json')).stdout, flush=True)
    assert run('rpc', 'handoff', bead, check=False).returncode != 0
    assert run('rpc', 'resume', bead, check=False).returncode != 0
    assert run('dispatch', bead, '--backend', 'herdr', check=False).returncode != 0
    old.send_signal(signal.SIGTERM)
    old.wait(timeout=15)
    (root / 'agents.json').write_text(json.dumps({'result': {'agents': []}}))
    process(os.getpid())
    print(run('rpc', 'handoff', bead).stdout, flush=True)
    st = status()
    assert st['phase'] == 'parked' and st['session_id'] == bead and st['session_file'] != str(transcript), st
    reservation = json.loads((state / 'rpc' / bead / 'worker.json').read_text())
    assert reservation['handoff']['checkpoint']['completed']
    assert run('tell', bead, 'This must be refused', check=False).returncode != 0
    assert run('rpc', 'activate', bead, check=False).returncode != 0
    assert 'agent_start' not in (state / 'rpc' / bead / 'events.jsonl').read_text()
    assert transcript.read_bytes() == history
    assert (worktree / 'untracked.txt').read_text() == 'Retain these exact bytes.\n'
    # A parked fresh handoff is recoverable without a first model turn.
    run('rpc', 'stop', bead)
    wait(lambda: not (state / 'rpc' / bead / 'control.sock').exists(), 20)
    run('rpc', 'resume', bead)
    assert status()['phase'] == 'parked' and status()['session_file'] == st['session_file']
    assert 'agent_start' not in (state / 'rpc' / bead / 'events.jsonl').read_text()
    # Only this synthetic fixture is explicitly authorized to leave parking.
    fixture['status'] = 'in_progress'
    (root / 'bead.json').write_text(json.dumps(fixture))
    run('rpc', 'activate', bead)
    run('tell', bead, 'Transport fixture only. Repeat the HANDOFF_CONTEXT_SENTINEL and the unanswered question from the handoff. Do not use any tools.')
    wait(lambda: status()['phase'] == 'idle')
    output = run('rpc', 'output', bead).stdout
    assert 'HANDOFF_CONTEXT_SENTINEL' in output and 'deploy' in output.lower(), output
    messages = [json.loads(line) for line in Path(st['session_file']).read_text().splitlines()]
    assert len([m for m in messages if m.get('customType') == 'quartermaster.handoff']) == 1
    run('rpc', 'stop', bead)
    wait(lambda: not (state / 'rpc' / bead / 'control.sock').exists(), 20)
    run('rpc', 'resume', bead)
    assert status()['session_file'] == st['session_file']
    messages = [json.loads(line) for line in Path(st['session_file']).read_text().splitlines()]
    assert len([m for m in messages if m.get('customType') == 'quartermaster.handoff']) == 1
    assert transcript.read_bytes() == history
    result = dict(result='PASS', source_pid=old.pid, replacement_pid=st['pi_pid'], session=st['session_file'], transcript_sha256=hashlib.sha256(history).hexdigest(), assertions='live-source refusal; unfinished-resume refusal; no duplicate managed writer; fresh context; no startup turn; parked tell/activation refusal; original bytes preserved; explicit fixture continuation; exact session resume')
    (root / 'result.json').write_text(json.dumps(result, indent=2))
    print(json.dumps(result), flush=True)
finally:
    run('rpc', 'stop', bead, check=False)
    if old.poll() is None:
        old.terminate()
        old.wait(timeout=15)
    print(f'Fixture stopped; evidence retained at {root}', flush=True)
