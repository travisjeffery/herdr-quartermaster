# Native pi RPC workers (opt-in)

`pi-rpc` is an additional **local worker backend**, not a replacement for
Herdr. Existing TUI dispatch, coordinator sessions and defaults are unchanged.
Tested with pi 1.5.2 on Linux. Start with a code-only trial, not infrastructure
or production work: the [pi guard limitations](pi.md#approvals-guards-that-block-and-ask-only-when-told-to)
still apply. There is no new sandbox or Claude auto-mode classifier.

## Try it without touching live task state

From this checkout, first run the no-model fixtures:

```sh
go build .
go vet ./...
go test .
go test -race . -run 'TestRPC|TestPi'
```

Then use an **already authorized** model/subscription (no purchase or login
is performed by the script):

```sh
pi auth check --provider openai-codex --no-refresh
python3 scripts/pi-rpc-trial.py --model openai-codex/gpt-6-astra
```

The script builds a scratch binary, creates a private scratch Git repository,
uses a fixture `bd` rather than the real database, points Herdr at a nonexistent
binary, and dispatches one native worker with tools/MCP disabled. Two harmless
model turns exercise the brief and a follow-up. It checks duplicate request
reconciliation, cancellation, stop, saved-session resume, and refusal of an
implicit TUI fallback. It stops its worker in `finally` and retains the printed
scratch directory (logs, session and worktree) for inspection. It installs
nothing and changes no live defaults. Each model turn has a 90-second limit;
the process stop has a five-second graceful window before killing pi.

Verified 2026-11-17 using pi 1.5.2 and the existing OpenAI Codex subscription:
all checks passed, including the same actual session file across different
supervisor PIDs. Active cancellation and crash/framing/guard cases are covered
by no-model fixtures; the real-model trial cancels at idle to avoid unnecessary
model work. No live infrastructure or third-party approval was exercised.

## Dispatch an actual bead

Use a fresh bead whose branch/worktree does not already exist:

```sh
quartermaster dispatch <bead> --backend pi-rpc --agent pi
quartermaster rpc status <bead>
quartermaster tell <bead> 'Inspect the tests next.'
quartermaster rpc output <bead>
quartermaster rpc events <bead>
quartermaster rpc cancel <bead>
quartermaster rpc stop <bead>
quartermaster rpc resume <bead>
quartermaster tell <bead> 'Read the bead and continue from the saved work.'
```

`dispatch`, `resume`, `cancel` and `stop` use the existing class-3 autonomy
check. `--by TJ` is only for a decision **actually made by TJ**, not a way for
an agent to override the dial. The worker receives the normal brief, Beads
identity, guard extension, configured environment, and executor socket/shims
when the executor is running. No shell/PTY or pane is created. A native worker
is pi regardless of `worker_agent`; an explicitly different `--agent` is an
error. Remote `--host` and `--focus` are refused.

For future dispatches only, optionally set:

```toml
worker_backend = "pi-rpc" # omitted or "herdr" keeps the existing default

[agents.pi]
args = ["--model", "openai-codex/gpt-6-astra"]
```

Use the model and project-trust arguments appropriate to your existing pi
setup. This example deliberately does not grant project trust. The native
backend owns mode, session selection and the quartermaster control socket;
conflicting CLI arguments are refused. All configured/global extensions still
load normally; the quartermaster guard is always loaded explicitly.

Live native workers join `quartermaster context`, ticker messaging and the
normal `prompt`/`answer` ask-gate flow. Their `rpc:<bead>` handle is not a pane.
`rpc status`, `output`, `events`, `tell`, `receipt`, `cancel`, `stop` and `resume`
work with no Herdr installed. The coordinator UI still requires Herdr. If Herdr
listing fails but native workers exist, the combined context contains the
native workers only: use native status as the authoritative transport view,
not absence from that combined list as proof a TUI worker died.

## Reconnection is not resume

A detached supervisor process owns pi's stdin/stdout for the worker's lifetime.
Short-lived CLI/coordinator exits do not interrupt it. Each client reconnects
to the same private Unix socket; no request is replayed on reconnect.

After pi exits, `rpc resume` (or `resume <bead>` for one explicitly named native
worker) opens **the exact session file returned by pi's `get_state`**. This
restores conversation history, not an interrupted tool, running shell, pending
queue or automatic continuation. It sends **no resume prompt**: inspect the
receipt/events and explicitly tell the worker what to continue. Calling resume
on a live supervisor only reconnects. There is no automatic restart after a
host reboot, and batch resume does not implicitly convert native workers.

Recovery fails closed if saved status is malformed or missing beside prior
launch artifacts, or if the supervisor died before persisting the first
`get_state` identity. The supervisor persists a launch intent before starting
pi, so that window cannot be mistaken for a fresh launch. Inspect the retained
session/event artifacts and explicitly repair metadata only after verifying
both prior PIDs exited and the exact session identity. No automatic guessing,
new-session fallback, or force-resume command is provided.

An exclusive flock is held by the supervisor and inherited by pi. A new
supervisor also refuses a potentially live previous child PID. Git refuses
adopting an existing branch/worktree on initial dispatch. The reservation
continues to block TUI dispatch even after stop. These protections cover
quartermaster-managed workers, not someone manually opening the session in
another pi process or modifying files as the same OS user. Never manually
open a native session for writing while its worker is alive.

## Requests and uncertain delivery

For scripts, give the prompt a stable ID and reuse **that same ID and text**
when reconnecting:

```sh
quartermaster rpc tell <bead> review-001 'Review the changed tests.'
quartermaster rpc receipt <bead> review-001
```

Receipts are atomically persisted and fsynced **before** writing to pi:

- `intent`: delivery/acceptance is unknown; may already have run. Inspect
  `events`, `output`, and the saved session. Reusing the ID returns the receipt
  without resending. Do not blindly submit the same work under a new ID.
- `accepted`: pi accepted, queued or handled the prompt. This does **not** mean
  the work succeeded or finished. The response includes pi's disposition.
- `rejected`: pi rejected the request before acceptance.

A missing receipt during a timed-out submission is not proof of non-delivery:
the server may still be processing it. Reconcile using the same ID. Reusing an
ID with different text is refused. `tell` prints its generated request ID;
automated ticker deliveries use stable content-derived IDs so a failed client
cannot silently replay them. Identical automated notices reconcile to the old
receipt rather than triggering another turn (even if the same condition recurs).

Idle is driven by `agent_settled`, not prompt acceptance, assistant output or
`agent_end`. Provider failures/aborts remain in the event/session stream; idle
means no automatic work remains, **not task success**. `cancel` clears queued
messages before aborting the current run; `stop` closes stdin, waits for orderly
shutdown and kills the direct pi child after five seconds if necessary.

## Approvals and guards

The same `pi/quartermaster.ts` extension runs the same bash hooks, denies broad
subagents, records prompt origins and refreshes `bd prime` after compaction.
Configured ask-gates retain their existing authenticated control socket and
`quartermaster prompt` / `quartermaster answer --expect` autonomy/audit path.
Native ask status comes over a structured `setStatus` event. A hook denial is
never converted into an approval. Native fork/session-switch hooks refuse
changing the session behind its supervisor.

General RPC extension `confirm`/`select`/`input`/`editor` requests are **not
approved automatically**. This increment sends cancellation, clears/aborts
work, latches the worker as blocked, and records an actionable `needs_me` note.
Notifications and widgets are retained in events. There is deliberately no raw
RPC forwarding or arbitrary extension-UI-answer endpoint. TUI-only extension
interfaces such as `custom()` are unsupported by pi RPC itself; third-party
TUI-only approval extensions are **not certified** for this backend. Keep those
workflows on explicitly selected Herdr/TUI workers. The existing quartermaster
ask-gates are the tested approval path; do not disable a guard to make a native
worker run.

## Files, limits and recovery

All native data is under `<state dir>/rpc/<bead>/`:

- `worker.json`: backend reservation, argv, isolated worktree and branch.
- `status.json`: actual pi session ID/file, supervisor/child PIDs and lifecycle.
- `requests/<id>.json`: durable request receipts, retained across resume.
- `events.jsonl`: continuously drained pi stdout, strict LF JSON records.
- `stderr.log`, `supervisor.log`: separate diagnostics.
- `sessions/`, `worktree/`, `lease`, `control.sock`.

Directories are 0700; control socket and supervisor artifacts are 0600. These
contain prompts, outputs and potentially sensitive tool results: don't publish
them without review. Pi's own session file modes are governed by pi, inside
the private directory. This is isolation from **other OS users**, not from
agents running as the same user. Configured environment values are inherited,
not copied into worker.json.

Records are limited to 16 MiB, prompt text to 1 MiB, simultaneous local client
handlers to 16, and pipe writes/command responses to 15 seconds each. Writes
honor backpressure; write errors stop the transport because a partial record
cannot safely be followed by another command. Malformed/oversized stdout,
partial EOF, disk-write failures and child exit stop the worker, never trigger
a fallback launch. Clients never participate in the stdout drain. Event and
session logs currently grow on disk; `output` is a snapshot of completed
assistant text, not a streaming viewer. Monitor disk usage and keep trials
bounded. Socket paths must fit the platform's Unix socket path limit; use a
short absolute `QUARTERMASTER_STATE_DIR` for deep scratch directories.

## Rollout and rollback

1. Build/test the branch and run the isolated trial above.
2. After review, try a fresh, code-only bead with explicit `--backend pi-rpc`.
3. Inspect guard behavior, status, receipts and saved-session resume before
   optionally selecting a default for **future** dispatches. Never migrate
   active sessions implicitly.

Rollback: `quartermaster rpc stop <bead>`, verify the socket is gone and the
saved status says stopped (disconnected CLI status is only cached evidence),
and remove `worker_backend = "pi-rpc"` if you set it. Use
`dispatch <different-fresh-bead> --backend herdr --agent pi` for the explicit
TUI fallback. Stopping does not release the bead or delete history/worktrees.
Do not delete a lease or reservation to force a second writer.

`resolve` refuses native reservations and `sweep` skips their worktrees for now. For deliberate cleanup after
the bead is closed, first stop and verify both PIDs exited, review/preserve
session/receipt artifacts, and use Git's normal non-force `worktree remove`
on the recorded path. Keep the reservation/history until a later cleanup tool
can preserve the needed recovery evidence. No automatic deletion is shipped.

Separately tracked follow-ons: structured viewer/usage, steering/forks,
retention/cleanup, and a headless coordinator. No remote fleet, new UI, or
live-default change is included here.
