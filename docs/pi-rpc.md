# Native pi RPC workers (opt-in)

`pi-rpc` is an additional **local worker backend**, not a replacement for
Herdr. Existing TUI dispatch and defaults are unchanged. TUI coordinator ticker
notifications use the separate [draft-safe inbox](pi-coordinator-inbox.md); that
does not turn the coordinator into an RPC worker.
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
nothing and changes no live defaults. Each model turn has a 90-second limit.
New supervisors request shutdown with stdin EOF only and never escalate to
forced termination. An unresponsive child keeps its supervisor and inherited
session lease. Older running supervisors retain their old five-second kill
fallback; installing a new CLI does not change them (see below).

Verified 2026-11-17 using pi 1.5.2 and the existing OpenAI Codex subscription:
all checks passed, including the same actual session file across different
supervisor PIDs. Active cancellation and crash/framing/guard cases are covered
by no-model fixtures; the real-model trial cancels at idle to avoid unnecessary
model work. No live infrastructure or third-party approval was exercised.

## Read-only conversations in the board

Select a native worker in the existing Quartermaster board and press **Enter**
or **V**. This opens a conversation screen **in the same plugin pane**, not a
worker pane. Enter retains its old focus/start behavior for non-native rows;
V only views native history. A native reservation is enough: stopped workers
and parked native-adopted/fresh-handoff workers do not need a live agent row.
The board lists open tasks; use the CLI for a closed task's retained history:

```sh
quartermaster rpc view <bead>
quartermaster rpc inspect <bead>          # normalized visibility JSON
quartermaster rpc history <bead>         # one saved-session page
quartermaster rpc feed <bead>            # one execution-log page
quartermaster rpc history <bead> <cursor> # reconnect after a returned cursor
```

These entrypoints never dispatch, activate, resume, prompt, cancel, stop, fork,
answer an approval, or write task state. They read retained files; only
`inspect`/the screen asks the **supervisor** for its existing read-only status.
The supervisor remains pi's sole stdin/stdout owner. Closing a screen affects
only that screen. Do not use the plugin's **focus-or-start clipboard action**
as a viewer: it is a separate control, not a read-only entrypoint.

### Screen keys and evidence

- **h / e / Tab:** saved session history / live execution log. They are
  separate views because a completed message occurs in both sources; merging
  their records would duplicate messages and usage. Saved history is append
  order across **all branches**, including pre-compaction entries, not a
  reconstructed active model context. A fresh handoff shows its saved handoff
  message, not a converted legacy transcript.
- **j / k:** select a record and pause following. **Enter / Space:** expand
  output, entry/parent/tool-call IDs and PR/artifact URLs. **J / K:** scroll
  expanded output. Links are plain text; the viewer never opens a URL or file.
- **g:** replay from the beginning, paused. **n:** next bounded page.
  **f:** catch up and follow both files. **r:** read another page/refresh status.
  **q / Esc:** return to the board (or leave the standalone screen).

Assistant/user messages appear when finalized. Tool starts, results and errors
appear as they are recorded; token deltas and partial tool-result bodies are
intentionally omitted so fragmented credentials cannot escape per-message
redaction. Images, thinking/signatures, system prompts and opaque extension/tool
metadata are not rendered. Tool start detail includes only command/path, not
arbitrary argument objects. Output is expandable up to the documented limit.

The board's native row reports transport state separately from task state.
Details and the conversation header show run elapsed time, current action,
last meaningful activity and its timestamp, and when visibility was sampled.
Snapshots older than three seconds are marked stale; paused views freeze run
elapsed time at their observation rather than inventing ongoing progress.
PR checks in the task detail are explicitly **cached external evidence**, not
proof that the worker is progressing. State semantics:

| State | Evidence |
|---|---|
| running | Supervisor has seen a run start/accepted submission, not just a live PID. |
| idle (not parked) | pi settled; neither task completion nor migration parking. |
| parked | Live supervisor confirms the durable parking barrier. |
| waiting on approval | Existing quartermaster ask gate is waiting. |
| blocked (intervention) | Supervisor latched an unsupported-dialog error; viewing cannot answer it. |
| stopped | Saved supervisor terminal state (`stopped` or `failed`); no live status connection. |
| disconnected | No status connection and no saved terminal state; never inferred to be stopped or parked. |
| unknown | No supported evidence; no invented activity/model/usage. |

An old supervisor/session can be viewed without restarting it. Older raw
execution events have no receipt timestamp, so the viewer says **time unknown**
unless the message carries a timestamp. Older status files lack action/run
metadata: those fields remain unknown; recorded model/usage still come from
saved history. No live supervisor migration is required or performed.

### Replay, limits and privacy

Cursors bind the source, file identity, byte offset, and prefix/boundary hashes.
`seq` is the durable byte position, not a task-progress counter. Every call
opens a fresh read-only file descriptor and reads a size snapshot. A detected
replacement/truncation or mismatched cursor produces an explicit **GAP** and
restarts replay (`reset: true` is independent of the gap text). An incomplete
last line is retained for the next poll;
malformed records become gap records. No model call repairs or summarizes gaps.
Files are expected to be append-only; this is not a tamper-proof audit log.

Each page scans at most 256 records or 2 MiB **plus at most one 16 MiB record**
(with 256 bytes of headroom for the supervisor's log timestamp),
returns at most 64 records or 256 KiB plus one bounded projection, and clips
text to 16 KiB per field *after* redaction. Oversized records fail closed with
an explicit error rather than unbounded allocation. The screen retains at most
128 records / roughly 512 KiB of text per source, marks eviction, and permits
replay from disk. There is at most one read in flight per screen and a one-item
result channel. Viewers have no producer-side subscription queue: slow,
disconnected or absent screens cannot block the worker's stdout drain. Durable
logs grow while workers run; [explicit offline retention](pi-rpc-maintenance.md)
is disabled by default and reports durable replay gaps.

Usage is summed **only from saved records**, including assistant/tool usage,
compaction, branch-summary and standalone usage entries. Streaming/duplicate
event usage is never added. The screen labels totals incomplete until replay
catches up; no recorded usage is **unknown**, not zero. Costs are recorded pi
estimates, not a subscription balance or invoice. Provider/model metadata is
recorded evidence, not a request to select or query a model.

The normalized APIs and screen allowlist fields, strip terminal controls and
formatting overrides, and redact credential-shaped assignments, authorization
headers, cookies, private-key blocks, URL credentials and common token formats.
Redaction happens before clipping and links exclude query strings/userinfo.
This is defense in depth, **not a guarantee that arbitrary free-form output is
safe to publish**. Keep screenshots/exports private and review them manually.
Existing raw `rpc events`, `rpc output`, session/log files and task notes remain
private sensitive artifacts; these legacy diagnostic commands are not the
normalized viewer API. No credentials are copied from environment/config into
visibility or usage metadata.

Isolated real-pi and PTY validation (no model prompts, live tasks, Herdr
connection, settings changes or installation):

```sh
go test -race . -run 'TestRPCView|TestRPCBoard|TestRPCHandledAndLateResponse'
python3 scripts/pi-viewer-trial.py --model <configured-provider/model>
```

The trial opens/reopens the direct and board views, replays external saved
history, verifies unchanged worker/session/task fingerprints, and views again
after stopping **only its own fixture**. Live Herdr smoke/installation is a
separate coordinator-owned step; no worker needs `HERDR_ENV` or a pane.

## Guarded graceful-only lifecycle

For one **explicitly selected, authorized** native worker:

```sh
quartermaster rpc status <bead>          # inspect live capability and identity
quartermaster rpc stop-graceful <bead>  # existing class-3 lifecycle gate
# Only after confirmed exit and separate authorization to resume:
quartermaster rpc resume <bead>
```

`stop-graceful` is a distinct supervisor operation, not an extra field on
`stop`: old binaries ignore unknown fields, which would otherwise invoke their
force-capable path. The CLI requires a live `graceful_stop: true` capability
and nonempty `generation`, never a saved-status fallback. It selects that
in-memory generation, supervisor/child PIDs, exact session ID/file, lifecycle
sequence and parking state. The supervisor revalidates that selection before
and after its read-only pi preflight, under serialized admission. A restarted
supervisor or intervening lifecycle/parking change requires fresh inspection.
No OS signal or PID-only process targeting is used.

Only healthy **underlying idle** workers qualify. Parking is an additional
barrier, not evidence of quiescence. Running, blocked, errored, pending
prompt/cancel/activation operations, unresolved or malformed receipt files,
pending transport calls, streaming/compaction/queued pi state, missing state
fields and missing/busy managed guards are refused. The supervisor sends only
`get_state` (and managed guard `ping`) before latching admission and requesting
EOF. It never sends a prompt, clear-queue, abort, approval, or activation.
New prompts, cancellation and activation cannot enter after the latch; late
lifecycle events cannot reopen it. An already recorded receipt remains
reconcilable without replaying its request.

The CLI waits up to five seconds **after preflight/acceptance** for the inherited
lease to become free and the same generation/session to have durable `stopped`
state with no child PID. Preflight itself uses existing bounded RPC deadlines.
A disconnected socket alone never proves shutdown. On timeout or client loss,
**do not repeat shutdown, send signals, delete reservations, edit metadata, or
start a replacement**. Inspect `rpc status`, receipts, events and stderr. The
supervisor remains in `stopping` with its lease while the child lives, including
when stdout has closed or framing failed. There is no timer that kills it.
`resume` refuses a live stopping owner. If the child later exits naturally,
its supervisor saves `stopped` and releases ownership; only then may an
explicitly authorized `resume` restore the exact saved session. Incomplete
exit evidence requires investigation, not an automatic retry or forced cleanup.
A client RPC timeout can occur before acceptance is known: the server may still
complete its preflight. The CLI records an attempt before sending it and reports
an ambiguous outcome rather than treating loss of the reply as refusal.

If the final pre-EOF status write fails, admission stays latched but EOF has
**not** been requested. Preserve both processes while fixing the storage fault.
A separately authorized `rpc stop` on the **same confirmed capable generation**
can then finish EOF-only shutdown; this is deliberate recovery, never an
automatic fallback and never valid for a legacy supervisor. A missing durable
terminal checkpoint from a guarded stop prevents resume.

Resume never replays receipts, submits a brief or removes parking. After a
recorded graceful stop it refuses unresolved/malformed receipt records. Ordinary
crash recovery retains its previous behavior: exact-session resume may preserve
an ambiguous intent unchanged, without replay, but such a worker remains
ineligible for guarded stop until the uncertainty is resolved. Resume preserves
the recorded session identity if pi reports a different one, and retains
worktree and reservation files.
Read-only viewing does not require resume. These controls serialize managed
admission; they are not a sandbox against arbitrary same-user processes or
extensions independently starting work.

### Legacy supervisors: precise refusal, no live migration

A supervisor without the live capability is **untouched**: only `status` is
sent. Its old `stop` operation and supervisor signal cleanup close stdin and
arm `Process.Kill` after five seconds; stdout/framing failure also reaches that
same timer. Sending SIGTERM directly to the child cannot install a new atomic
admission barrier in the old supervisor and does not remove that force-capable
cleanup path. An already parked worker still has that cleanup hazard. No safe
in-place upgrade is claimed, and there is no raw-socket, signal, metadata-edit,
manual-unpark, permissions/config/provider, or headless-enrollment fallback.

The preservation-only remaining option is to leave the old owner alone until
it exits naturally, confirm ownership release and exact saved-session evidence,
then separately authorize resume under the new binary. If waiting is not
acceptable, the operator must explicitly decide on a different transition
policy; **this implementation does not authorize or perform it**. This means
an installation alone does not unblock a no-force restart of legacy owners.

Installation, stopping selected live owners, and resuming them are separate
authorization/rollout steps. Development and review use only isolated fixtures:

```sh
go test -race . -run 'TestRPCGraceful|TestRPCStopBarrier|TestRPCDialogAbort'
```

Fixtures cover legacy status-only refusal, stale selection, idle/parked
stop+exact-session resume, unchanged history/receipts/parking/worktree files,
no model turn/replay, uncertain receipts, and an unresponsive child retained
past the former five-second kill window even after stdout closes. Fixture
cleanup acts only on fixture-owned processes, never selected live workers.
Independent read-only review covered identity revalidation, failed-stop recovery
and separate rollout authorization. An isolated real-pi 1.5.2 trial also passed
two parked CLI stop/resume rounds with the managed guard and no model turns:
exact session, preserved conversation prefix and unchanged receipts, parking and
worktree. Pi may append normal model/thinking metadata on opening a session;
prefix preservation does not require whole-session byte equality.

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
work with no Herdr installed. The existing coordinator UI still requires Herdr; the separate opt-in
[headless post-merge coordinator](pi-headless.md) does not. If Herdr
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
to make a check pass. If RPC started, use the explicitly authorized lifecycle
path and verify both PIDs exited (new supervisors can wait indefinitely; legacy
`rpc stop` can force termination);
leave the saved reservation for inspection/recovery. There is intentionally
no force-adopt or automatic TUI rollback into the same session. The explicit
`--backend herdr` override remains available for other fresh tasks. Do not
downgrade the installed binary to a pre-adoption version while reservations
exist: older binaries do not understand the prepared/parked barriers.

The caller cannot prepare itself. An external verified owner must checkpoint,
adopt and retire the migration worker last. No batch stop/close is provided.
These are managed-worker safeguards, not isolation against the same OS user
manually opening a session, typing into a pane or modifying configuration.

## Fresh cross-harness handoff (explicit, local)

For an explicitly authorized change from Claude, Codex or Muse to pi, use a
**fresh session with a written handoff**, not native transcript conversion.
This separate route uses the existing configured pi protection baseline; it
does **not** certify classifier/sandbox equivalence. Strict pi-to-pi adoption
above is unchanged, including its full process-census refusal cases.

1. Inventory the exact named worker and pane, excluding the coordinator and
   caller. Retire closed tasks without creating a replacement. Wait for a safe
   checkpoint (idle/done, or an unanswered question with no in-flight action).
   Inspect the source transcript and process tree; independently detached jobs
   or ambiguous worktree ownership remain exceptions, not a reason to kill
   another process. Preserve all original transcripts and files.
2. Write a private JSON file with absolute `cwd` (the bead's linked worktree),
   absolute `transcript` (the original harness's exact transcript file), and
   `context` (current progress, the unanswered question **verbatim**, relevant
   evidence/PRs and the next separately authorized step). When a legacy pane
   reports a stale home-directory cwd, establish the actual worktree from its
   transcript and bead rather than guessing from the current pane directory.
3. `quartermaster rpc handoff-prepare <bead> <exact-pane> <handoff.json>`
   reserves the existing checkout under the usual lease/admission locks. It
   saves task context, transcript prefix, source process identities and a raw
   checkout fingerprint (including initialized submodules). It neither stops
   anything nor prompts a model. Existing reservations are never overwritten.
4. Reconfirm the exact source identity and safe checkpoint; gracefully stop
   only that source process. Wait for its recorded descendants and remaining
   source-session processes to exit. No force-kill or blanket process command.
   `quartermaster rpc handoff <bead>` checks those exits, the original idle
   shell, fresh Herdr ownership, unchanged files/history and task status, then
   starts a bounded, **parked** pi replacement. Its new v3 session contains
   only a freshly authored handoff custom message (no converted source entries
   or invented assistant response). This seed is durable before launch because
   pi otherwise buffers a fresh session until its first assistant response.
   An interrupted initial launch must pass the same validation; `rpc resume`
   cannot skip preparation.
5. Check `rpc status` is parked with a **new** session in the private RPC
   directory. The configured guard must confirm the exact handoff context in
   the active session branch; it is a `quartermaster.handoff` custom message,
   not a task prompt. Verify no startup `agent_start`, retained source history
   and dirty files, and refusal of `tell`. Only then close the exact old pane
   after freshly confirming it is still the original idle shell.

All replacements start parked, regardless of task status. Blocked/needs_me
questions stay unanswered. Activation is separate and never an approval; do
not activate a live worker merely to test transport. The ticker treats parked
workers as blocked. The new session retains the handoff on resume; the old
transcript is never passed to pi as a native session or rewritten. Initial
handoff context also remains durable in private `worker.json`.

Unlike strict adoption, this route checks exact recorded owners and source
Linux sessions, **not every process's cwd on the host**. It does not claim to
exclude an independently detached writer. Unrelated protected services do not
need to be stopped or made inspectable. No guard configuration, permission or
reservation bypass is provided. On failure preserve the reservation, pane and
files, stop expansion, and inspect; do not restart the old harness alongside
the replacement. Keep this version installed while fresh-handoff reservations
exist. There is no automatic rollback into the old transcript.

Isolated validation (fixture legacy source, real guarded pi, no live panes):

```sh
python3 scripts/pi-handoff-trial.py --model <authorized-provider/model>
# Repeat after installation:
python3 scripts/pi-handoff-trial.py --binary /absolute/quartermaster --model <authorized-provider/model>
```

## Explicit steering and offline forks

`rpc steer` is separate from queued `tell` follow-ups. `rpc fork` creates a
new parked identity/session/worktree from an explicit checkpoint of a stopped
native source; it never switches a live session. See [steering/fork controls,
receipts, autonomy and recovery](pi-rpc-operations.md) before using either.

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
adoption and fresh-handoff paths above are the only supported exceptions. The reservation
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
shutdown. New supervisors never send a kill or signal to pi, even on transport
failure or supervisor signal cleanup. A stuck child can therefore retain its
supervisor indefinitely, including after stdout closes; further supervisor
signals do not escalate. This favors preservation/ownership over automatic
recovery and does not claim that an unresponsive child is still observable.
Older running supervisors still arm their five-second kill timer: plain `stop`
is **not** a graceful-only operation across versions. Use `stop-graceful` for
guarded preservation-only work; it refuses those legacy owners untouched.

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

- `worker.json`: backend reservation, argv, worktree and branch; adoption or fresh-handoff checkpoint/context when applicable.
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
session logs grow on disk; [explicit offline event retention](pi-rpc-maintenance.md)
never expires saved sessions or receipts. `output` is a legacy raw snapshot of
completed assistant text. Use `view`/`history`/`feed` for the bounded, normalized
read-only viewer described above. Monitor disk usage and keep trials
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

`resolve` refuses native reservations and `sweep` skips their worktrees. Use
[explicit offline maintenance](pi-rpc-maintenance.md) to preview event retention
or guarded cleanup of one closed, stopped native-dispatch worker. It preserves
sessions, receipts, branches and the reservation; no automatic deletion ships.

The [headless coordinator](pi-headless.md) is a separate,
explicit post-merge opt-in. No remote fleet or live-default change is included here.
