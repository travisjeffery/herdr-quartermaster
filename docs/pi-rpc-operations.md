# Explicit native steering and session forks

These are **opt-in controls**, not changes to `tell`, dispatch defaults, TUI
workers, the ticker or the headless coordinator. The supervisor remains the
sole pi protocol writer. There is no raw command, runtime session switch,
model/provider/permission change or approval-response endpoint.

## Steer a running worker

```sh
quartermaster rpc steer <bead> <request-id> 'Change direction after this turn.'
quartermaster rpc receipt <bead> <request-id>
```

`steer` sends pi's **`steer`** operation. Pi consumes steering after the current
assistant turn and its tool calls, before its next model call. It does not
interrupt a tool already executing. By contrast, `tell` still uses
`prompt(streamingBehavior=followUp)`: idle work starts immediately; busy work
waits for the run's pending tools and steering to finish.

Steering requires an in_progress bead and a new capable, healthy **running**
supervisor. The supervisor also asks pi for its exact session and streaming,
non-compacting state before admission. Known idle, parked, stopping, blocked,
errored, differently owned headless, and legacy supervisors refuse it. No
fallback to tell, TUI typing, activation or restart occurs. The capability is
`steering: true` in live status; installing a CLI does not upgrade old owners.

A run can settle between preflight and pi accepting the steer. The direct
`steer` operation never starts an idle run: in that boundary case it can remain
queued. **Accepted is not consumed or completed.** If a steer is pending or
its acceptance is still uncertain when the run settles, the supervisor latches
`blocked` with a steering-specific error. A late nonempty steering queue while
idle also latches it. Subsequent tells/ticker deliveries refuse rather than
silently carrying that direction into unrelated work. Inspect `queue_update`,
message events and the receipt, then explicitly `rpc cancel` to clear the queues
before continuing. Cancellation clears only this transport latch, not approval
or unsupported-dialog blockers. Do not send a prompt to flush a stranded steer.
Pi can also report `handled` if an input handler consumed the message.
Extension slash commands are not supported by pi's direct steer operation;
skills and templates retain pi's normal expansion semantics.

### Reconciliation, cancellation and audit

The request ID binds both text and operation. Repeating the same ID/text/op
returns its existing receipt without sending again, even after cancellation.
Reusing an ID across steer/tell or with changed text fails. Old receipts with
no `operation` remain follow-up/prompt receipts. Re-invoking `steer` still passes
its command-level autonomy/audit gate; use read-only `receipt` to reconcile
without another budgeted check or attempt note. An `intent` is ambiguous,
including after client timeout or crash; late responses still update it. Use
`receipt` for read-only reconciliation after the bead or worker stops.

Steering uses the same submission lock, headless-ownership fence, parking and
monotonic stop barrier as follow-ups. `rpc cancel` serializes behind admitted
messages, **clears both queues before abort**, and never resends their contents.
If an asynchronous steer handler is still unconfirmed (for example after a
transport deadline), cancellation remains blocked: the handler could queue after
clear-queue. Reconcile its receipt, then explicitly cancel again before allowing
new work. This cancels all pending work, not one request. It does not undo completed tool
side effects or change an accepted receipt to "never happened". Restart restores
saved history, not unsaved queues or uncertain operations. Never blindly retry
with a new ID. Client disconnection alone is not cancellation.

`steer` has a class-3 local-control gate and records an attempted-operation note
(ID, caller and text hash, not the text) before submission. The durable receipt
and pi response/events record transport outcome. The steering text does **not**
grant approval or authorize a higher-impact action: independently apply the
normal autonomy/authorization rules to what the instruction would cause.

## Fork an offline checkpoint into separate work

```sh
quartermaster rpc history <source>     # find an assistant entry ID
# Source must already have a confirmed guarded graceful-stop checkpoint.
quartermaster rpc fork <source> <fresh-destination> <assistant-entry-id>
quartermaster rpc fork-status <fresh-destination>
```

This is an offline, explicit **checkpoint copy**, not forwarding pi's runtime
`fork` or `switch_session`. It never changes a running worker's session and
never stops one for you. Unlike pi's user-message `/fork`, it copies through a
selected completed assistant message (`stopReason=stop`) and does not return
or resubmit an old user prompt.

Requirements:

- Source is a local native-owned reservation, not TUI adoption or cross-harness
  handoff. Its bead is in_progress or closed, not blocked/needs_me; it is not
  parked. Its capable supervisor recorded successful guarded graceful shutdown,
  no error or live child, and its lease is free. A disconnected socket, failed
  supervisor or legacy stop is insufficient.
- Every source receipt is resolved. Session ID/cwd/path match the native
  reservation. The v3 JSONL is complete, with unique entries and valid parent
  chains; the selected ancestry has no unfinished tool calls. Source input is
  bounded to 64 MiB, with the existing 16 MiB per-record limit. No partial copy
  or model-generated history repair is performed.
- Destination is a **different, unclaimed open bead** in the same configured
  repository on the local host. It has no reservation, session artifacts,
  checkout or existing task branch. Fork never adopts/reuses an existing branch.
- The source checkout is the exact registered native worktree/branch, clean
  including ignored/untracked files, without hidden index flags. The new branch
  starts at the source's **current committed HEAD**, recorded in fork metadata.
  Selecting an older conversation entry does **not** rewind Git to that time.

Preparation holds exclusive migration admission and both nonblocking worker
leases. It rechecks bead status/assignee/identity under those leases and again
before publishing intent. Existing current/legacy task branches or a pi control
endpoint refuse before a reservation is written; expected precondition failures
do not capture an old destination. The per-worker lease also fences resume (which does not take migration
admission). It copies only the checkpoint's ancestry, preserving raw entries,
compactions, context edits and opaque content. Unrelated branches and later
prompts are not copied. A new v3 header has a new random session ID and the
new worktree cwd, with parent-session provenance. A visible stored context entry
names the destination task, path and branch and states that inherited history
is background, not source authority or instructions to replay. Nothing is sent
to a model. Copied custom extension state is history, not certification of a
third-party extension's approval behavior; review such extensions separately.

The destination owns a new branch, worktree, physical session file and backend
reservation. Preparation writes a durable **incomplete intent first**, the seed,
parking marker and saved session identity next, and publishes `ready: true`
last. It does not claim the bead, start pi, enroll a coordinator, activate,
answer a blocker, or send a brief/user prompt. Source files are unchanged.

### Explicit continuation

After separately authorizing the new task, claim its bead, then:

```sh
quartermaster rpc resume <destination>   # exact new session; still parked
quartermaster rpc status <destination>   # verify identity and parking
quartermaster rpc activate <destination> # requires in_progress; no prompt
quartermaster tell <destination> 'Read your bead and work only on this new task.'
```

Resume verifies the seed's immutable prefix and exact identity before launching.
The normal managed pi guard receives the parking marker and blocks input/tools
until activation. Runtime session-fork/switch guards remain unchanged. Source
and destination can subsequently run independently under their own leases.
Activation permits normal ticker deliveries, so it is substantive continuation,
not a harmless live transport test. No source autonomy grant, claim or pending
queue is transferred; both beads' class-3 fork gates must pass independently.
The destination's current configuration/guard baseline applies on launch.

### Interruption, retention and rollback

The destination bead ID is the fork operation identity. Repeating `fork` refuses
any existing artifacts, whether complete or incomplete. **Inspect `fork-status`
instead of retrying.** A failed checkout creation, interrupted seed write or
failed final publication leaves an explicit reservation that cannot launch.
There is no automatic completion, cleanup, retry, fresh-session fallback or
rollback into a TUI. An interrupted caller may have completed preparation:
`ready: true` is evidence, not the caller's exit code.

Keep `worker.json` (including `fork` provenance), session seed/prefix, status,
receipts and parking. The forked session is self-contained: it does not read
its parent's transcript on resume. Source cleanup must still serialize on the
source lease; missing source checkouts and any `cleanup.json` intent/completion
marker fail future fork preparation. The same marker also fences forked resume. Never
unlink/recreate lease files to make either owner appear stopped.

Rollback means **do not activate/continue the destination**. Leave its reservation
and artifacts retained; if it was explicitly resumed, use a separately
authorized guarded graceful stop and verify exit. Source resume is separate
and does not depend on the fork. Any deletion/retirement requires the normal
independently authorized cleanup procedure. Do not downgrade to binaries that
ignore fork metadata while fork reservations remain: old launch paths do not
understand this new parking/validation contract.

Fork attempts and completion are noted on the destination bead; `fork-status`
reports source session/hash, selected entry, Git HEAD, destination identity,
seed hash/size and readiness. No session text is put in notes. Metadata and
sessions are private artifacts, not safe public PR attachments.

## Isolated verification

```sh
GOMAXPROCS=2 go test -p 1 . -run 'TestRPCSteer|TestRPCFork'
GOMAXPROCS=2 go test -race -p 1 . -run 'TestRPC|TestPi'
python3 scripts/pi-fork-trial.py
```

The no-model fixtures cover operation-bound reconciliation, late responses,
steer/follow-up distinction, cancellation ordering, refusal gates, separate
ownership, incomplete preparation, corrupted seeds, ancestry/compaction, and
parked exact-session reopen without replay. The real-pi trial first checks native steer queue/disposition/clear-queue
responses at idle without ever prompting. It then uses a synthetic source and
an isolated HOME/XDG/config/Beads environment, opens its destination
twice with the normal guard while its parent transcript is absent at the
recorded path, accepts a retained label pointing off the copied path, and makes
**no model calls**. It copies no live
credentials, installs nothing, touches no live worker, and retains evidence.
It uses `-buildvcs=false` for its scratch binary so unrelated parent Git metadata
cannot prevent a transport trial.
