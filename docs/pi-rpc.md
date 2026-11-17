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

## Explicit adoption of existing pi TUI workers

Adoption is **Linux/local, pi-to-pi only**. It never stops a process, closes a
pane, claims a bead, changes task status, sends a prompt, or changes defaults.
Claude/Codex/Muse classifiers, sandboxes and transcripts are not equivalent:
keep those workers intact as named exceptions. A later cross-harness path
needs an explicit durable handoff into a fresh session and verified guard
parity, not a renamed transcript or removal of a permission gate.

Test before installation:

```sh
go test -race . -run 'TestRPCAdoption|TestPiRPC' -count=1
python3 scripts/pi-adoption-trial.py --model openai-codex/gpt-6-astra
```

The isolated trial starts one tools-disabled real pi TUI on a private PTY,
using fixture Herdr/Beads and a scratch linked checkout; then adopts its exact
session. It verifies history, parking, refusal while the old owner lives,
refusal to activate a needs_me bead, explicit tell after activation, and
untracked-file preservation. It retains private evidence and stops its own
processes. Use `--binary /absolute/installed/quartermaster` to test an installed
build; run from the matching source checkout so the TUI guard matches.

For an explicitly authorized live migration, inventory **actual live managed
workers** first. Never dispatch restored empty panes or backlog. Protect the
coordinator, caller and workers still performing commands. Record individual
exceptions for unsupported approvals, uncertain ownership, other harnesses,
remote hosts and incompatible sessions. Unmanaged background jobs that detach
from both their process session and the checkout are unsupported: process
snapshots are not cgroup containment. Preserve workers with such jobs or an
uncertain process history. The exit census tracks process start/boot identity
and scans for surviving process-session members and newer checkout processes,
including children orphaned after checkpoint. Review global extensions for RPC
compatibility: argv parity does not certify a TUI-only approval extension.

For each eligible idle code-only pi worker, one at a time:

1. Save a private before inventory: bead/status/blocker, exact pane, process
   tree, worktree/branch, dirty/untracked files and transcript pointer.
2. `quartermaster rpc prepare <bead> <exact-pane>` atomically reserves it.
   This requires idle/done Herdr state plus an idle guard socket and no pending
   ask, a linked bead-owned worktree, matching guarded launch argv and guard
   environment, and a v3 session with the exact bead ID/cwd. Custom agent
   environments are not supported. The checkpoint in `worker.json` includes
   bead context, original descendants, git status/content digest and session
   prefix hash. TUI dispatch/resume share the reservation lock; managed legacy
   deliveries are held. Nothing is stopped.
3. Reconfirm the original process is idle with no in-flight command, then
   gracefully stop **that exact process only**. Pi handles SIGTERM with an
   orderly session shutdown. Wait for it and its recorded descendants to exit;
   never force-kill to pass an adoption check. Keep its pane.
4. `quartermaster rpc adopt <bead>` requires proof of exit, a fresh successful
   Herdr listing, the original pane back at its original shell, unchanged
   branch/HEAD/index/working/untracked state, preserved session prefix and
   unchanged bead status/assignee. It starts the exact saved session **parked**.
   `rpc resume` cannot bypass an unfinished prepare.
5. Verify live `rpc status` says `parked` with the exact session file/ID,
   inspect `events` (no `agent_start`) and the preserved session history,
   verify `tell` refuses while parked, compare files, and confirm the
   coordinator can see it as blocked. Only then close that one original pane
   after freshly verifying it is still the original idle shell.
6. Keep blocked/needs_me workers parked on their exact unanswered question.
   Migration is not approval. `rpc activate <bead>` is a separate class-3
   action, refuses anything other than a healthy idle in_progress worker,
   and sends no prompt. Use it only after verification and when substantive
   continuation is separately authorized; then `tell` works normally.
   Activation resumes ticker deliveries, so do not activate merely to test a
   live worker's transport. A parked worker's direct guard socket and tool
   calls also refuse execution.

On failure, **stop expansion**. A prepared reservation remains intact, as do
all files and histories. Do not delete locks/reservations or rewrite metadata
to make a check pass. If RPC started, `rpc stop` and verify both PIDs exited;
leave the saved reservation for inspection/recovery. There is intentionally
no force-adopt or automatic TUI rollback into the same session. The explicit
`--backend herdr` override remains available for other fresh tasks. Do not
downgrade the installed binary to a pre-adoption version while reservations
exist: older binaries do not understand the prepared/parked barriers.

The caller cannot prepare itself. An external verified owner must checkpoint,
adopt and retire the migration worker last. No batch stop/close is provided.
These are managed-worker safeguards, not isolation against the same OS user
manually opening a session, typing into a pane or modifying configuration.

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
adopting an existing branch/worktree on initial dispatch; the explicit staged
adoption path above is the only supported exception. The reservation
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

- `worker.json`: backend reservation, argv, worktree and branch; adoption checkpoint when applicable.
- `parked`: durable migration barrier, removed only by explicit activation.
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
