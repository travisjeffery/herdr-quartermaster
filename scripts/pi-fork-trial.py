#!/usr/bin/env python3
"""Offline real-pi fork/resume compatibility trial. No model calls or live state.

A synthetic stopped source is forked with the public CLI, then the destination
is opened twice in real pi with the normal guard. All HOME/XDG/Beads/Herdr paths
are isolated; no credentials are copied. Evidence is retained, nothing installed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import select
import subprocess
import tempfile
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--binary', help='test this build instead of building a scratch binary')
opts = parser.parse_args()
root = Path(tempfile.mkdtemp(prefix='qm-fork-'))
root.chmod(0o700)
print(f'Evidence: {root}', flush=True)
for name in ('repo', 'state', 'config', 'bin', 'home', 'agent'):
    (root / name).mkdir(mode=0o700)
repo, state, config = (root / n for n in ('repo', 'state', 'config'))
qm = Path(opts.binary).resolve() if opts.binary else root / 'bin' / 'quartermaster'
if not opts.binary:
    subprocess.run(['go', 'build', '-buildvcs=false', '-p', '1', '-o', str(qm), '.'], check=True,
                   env={**os.environ, 'GOMAXPROCS': '2'})
pi = shutil.which('pi')
assert pi, 'pi must already be installed'
# Keep only OS process essentials. No provider keys, inherited session/agent
# identities, executor endpoints, user configuration or real Beads connection.
env = {k: os.environ[k] for k in ('PATH', 'LANG', 'LC_ALL', 'TMPDIR') if k in os.environ}
env.update(HOME=str(root / 'home'), XDG_CONFIG_HOME=str(config),
           XDG_STATE_HOME=str(state), XDG_CACHE_HOME=str(root / 'cache'),
           QUARTERMASTER_STATE_DIR=str(state), QUARTERMASTER_CONFIG_DIR=str(config),
           PI_CODING_AGENT_DIR=str(root / 'agent'), PI_OFFLINE='1',
           BEADS_DIR=str(root / 'fixture-beads'), BEADS_ACTOR='pi',
           HERDR_BIN_PATH=str(root / 'absent-herdr'),
           HERDR_SOCKET_PATH=str(root / 'absent-herdr.sock'),
           GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1')
env['PATH'] = str(root / 'bin') + os.pathsep + env['PATH']
# Avoid environment-sensitive package-manager wrappers for the real child.
(root / 'bin' / 'pi').symlink_to(Path(pi).resolve())

def git(*args):
    return subprocess.run(['git', '-C', str(repo), *args], env=env, check=True,
                          capture_output=True, text=True).stdout.strip()


def save(path, value):
    path.write_text(json.dumps(value))
    path.chmod(0o600)


git('init', '-b', 'main')
git('-c', 'user.name=Trial', '-c', 'user.email=trial@invalid', '-c',
    'commit.gpgsign=false', 'commit', '--allow-empty', '-m', 'fixture')
source, child = 'source', 'child'
src = state / 'rpc' / source
for sub in ('sessions', 'requests'):
    (src / sub).mkdir(parents=True, mode=0o700)
cwd = src / 'worktree'
git('worktree', 'add', '-b', 'trial/source', str(cwd), 'main')
session = src / 'sessions' / 'source.jsonl'
records = [
    dict(type='session', version=3, id='source-session', cwd=str(cwd), timestamp='2026-01-01T00:00:00Z'),
    dict(type='message', id='user', parentId=None, timestamp='2026-01-01T00:00:00Z',
         message=dict(role='user', content='Synthetic source context only.', timestamp=1)),
    dict(type='message', id='sibling', parentId='user', timestamp='2026-01-01T00:00:00Z',
         message=dict(role='user', content='OMITTED_SIBLING', timestamp=1)),
    dict(type='label', id='label', parentId='user', targetId='sibling', label='other branch',
         timestamp='2026-01-01T00:00:00Z'),
    dict(type='message', id='checkpoint', parentId='label', timestamp='2026-01-01T00:00:01Z',
         message=dict(role='assistant', content=[dict(type='text', text='Synthetic completed checkpoint.')],
                      api='openai-responses', provider='openai', model='gpt-4o',
                      usage=dict(input=0, output=0, cacheRead=0, cacheWrite=0, totalTokens=0,
                                 cost=dict(input=0, output=0, cacheRead=0, cacheWrite=0, total=0)),
                      stopReason='stop', timestamp=2)),
]
session.write_text(''.join(json.dumps(r) + '\n' for r in records))
session.chmod(0o600)
source_bytes = session.read_bytes()
save(src / 'worker.json', dict(bead=source, cwd=str(cwd), branch='trial/source', args=['--mode', 'rpc']))
save(src / 'status.json', dict(bead=source, phase='stopped', session_id='source-session',
                             session_file=str(session), supervisor_pid=0, pi_pid=0,
                             graceful_stop=True, graceful_stop_requested=True, generation='fixture'))
args = ['--offline', '--no-approve', '--no-extensions', '--no-tools', '--no-mcp',
        '--no-skills', '--no-prompt-templates']
(config / 'config.toml').write_text(
    f'repo = {json.dumps(str(repo))}\nbranch_prefix = "trial/"\n'
    f'[autonomy]\nlevel = 3\nmax = 3\n[agents.pi]\nargs = {json.dumps(args)}\n')
beads = {source: dict(id=source, title='Source fixture', status='in_progress'),
         child: dict(id=child, title='Independent fixture', description='Do not execute source work.', status='open')}
save(root / 'beads.json', beads)
(root / 'bin' / 'bd').write_text(f'''#!/usr/bin/env python3
import json, sys
from pathlib import Path
p = Path({str(root)!r})
if sys.argv[1] == 'show':
    print(json.dumps([json.loads((p/'beads.json').read_text())[sys.argv[2]]]))
elif sys.argv[1] == 'prime':
    print('Fixture Beads only. No model turn authorized.')
else:
    with (p/'bd-audit.jsonl').open('a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')
''')
(root / 'bin' / 'bd').chmod(0o700)


def run(*args, check=True):
    r = subprocess.run([str(qm), *args], cwd=repo, env=env, text=True,
                       capture_output=True, timeout=50)
    if check and r.returncode:
        raise RuntimeError(f'{args}: {r.stdout} {r.stderr}')
    return r


def check_pi_steer_contract():
    # Test pi itself, not a protocol escape hatch on a managed supervisor.
    # Queue at idle, inspect, clear and exit: never prompt or call a model.
    log = (root / 'pi-contract-stderr.log').open('wb')
    proc = subprocess.Popen([str(root / 'bin' / 'pi'), '--mode', 'rpc', '--no-session',
                             *args], cwd=repo, env=env, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=log)
    pending = bytearray()
    events = []

    def call(kind):
        request = dict(id=kind, type=kind)
        if kind == 'steer':
            request['message'] = 'Offline protocol fixture; do not run any work.'
        proc.stdin.write(json.dumps(request).encode() + b'\n')
        proc.stdin.flush()
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if b'\n' not in pending:
                readable, _, _ = select.select([proc.stdout], [], [], 1)
                if not readable:
                    continue
                chunk = os.read(proc.stdout.fileno(), 65536)
                if not chunk:
                    raise RuntimeError('pi contract fixture exited before response')
                pending.extend(chunk)
                continue
            line, _, rest = pending.partition(b'\n')
            pending[:] = rest
            event = json.loads(line)
            events.append(event)
            assert event.get('type') != 'agent_start', 'unexpected model run'
            if event.get('type') == 'response' and event.get('id') == kind:
                assert event['success'], event
                return event.get('data', {})
        raise TimeoutError(f'pi contract response timeout: {kind}')

    try:
        assert call('get_state')['isStreaming'] is False
        assert call('steer')['disposition'] == 'queued'
        assert call('get_state')['pendingMessageCount'] == 1
        assert any(e.get('type') == 'queue_update' and len(e.get('steering', [])) == 1 for e in events)
        assert len(call('clear_queue')['steering']) == 1
        call('abort')
        assert call('get_state')['pendingMessageCount'] == 0
        assert any(e.get('type') == 'queue_update' and e.get('steering') == [] for e in events)
        save(root / 'pi-steer-contract.json', events)
    finally:
        proc.stdin.close()  # orderly EOF only, no forced termination
        proc.wait(timeout=20)
        log.close()


try:
    check_pi_steer_contract()
    result = json.loads(run('rpc', 'fork', source, child, 'checkpoint', '--by', 'worker').stdout)
    assert result['ready'] and result['session_id'] != 'source-session'
    child_session = Path(result['session_file'])
    seed = child_session.read_bytes()
    assert b'OMITTED_SIBLING' not in seed and b'other branch' in seed
    assert json.loads(seed.splitlines()[0])['cwd'] == str(state / 'rpc' / child / 'worktree')
    assert session.read_bytes() == source_bytes
    assert not (state / 'rpc' / child / 'control.sock').exists()
    assert run('rpc', 'fork', source, child, 'checkpoint', '--by', 'worker', check=False).returncode
    # Parent-session provenance is not a runtime dependency in real pi.
    retained_source = session.with_suffix('.retained')
    session.rename(retained_source)
    for _ in range(2):
        run('rpc', 'resume', child, '--by', 'worker')
        st = json.loads(run('rpc', 'status', child).stdout)['status']
        assert st['phase'] == 'parked' and st['session_id'] == result['session_id'], st
        assert st['session_file'] == str(child_session)
        assert run('rpc', 'tell', child, 'blocked', 'Never run.', check=False).returncode
        assert run('rpc', 'activate', child, '--by', 'worker', check=False).returncode
        run('rpc', 'stop-graceful', child, '--by', 'worker')
        assert child_session.read_bytes().startswith(seed)
        assert 'agent_start' not in (state / 'rpc' / child / 'events.jsonl').read_text()
        assert not list((state / 'rpc' / child / 'requests').iterdir())
    assert retained_source.read_bytes() == source_bytes and not session.exists()
    result = dict(result='PASS', source_sha256=hashlib.sha256(source_bytes).hexdigest(),
                  session=str(child_session), assertions='real pi steer queue/disposition/clear contract without a model run; new identity/cwd; source unchanged and absent at original path; off-path label accepted; no replay; durable parking; exact-session reopen twice; normal managed guard')
    save(root / 'result.json', result)
    print(json.dumps(result), flush=True)
finally:
    # Only the fixture's capable supervisor is eligible; never fall back to
    # legacy stop, cancel, signals or killing if graceful shutdown is refused.
    if (state / 'rpc' / child / 'control.sock').exists():
        stop = run('rpc', 'stop-graceful', child, '--by', 'worker', check=False)
        if stop.returncode:
            print(f'Fixture retained for inspection: {stop.stderr}', flush=True)
    print(f'Evidence retained at {root}', flush=True)
