# Headless native pi coordinator (explicit opt-in)

This is a **post-merge follow-through consumer**, separate from the interactive
coordinator. It does not replace the TUI, take over its conversation, or treat a
passive inbox message as autonomous orchestration. Herdr/default dispatch,
existing workers and the [draft-safe TUI inbox](pi-coordinator-inbox.md) stay as
before unless a particular native worker is explicitly enrolled.

The initial action allowlist is intentionally small: ask an already active,
claimed, idle native worker to inspect and finish its **already-authorized
post-merge scope**. The service does not install, merge, deploy, approve, close
beads, start/restart workers, change providers, or dispatch backlog. Those
substantive actions remain the worker's responsibility under the existing
scope, autonomy, executor and verification gates. Pre-merge work stays on the
existing ticker path; enrolling an open PR is refused.

## Ownership and authorization

Enrollment pins the exact worker reservation, branch, merged PR URL/head,
repository, task description/title and assignee. It requires a healthy idle
supervisor advertising `coordinator_fence`, no migration parking, and class-3
permission. It writes a fresh durable `merged` observation for **that explicitly
selected resource**, not a batch replay of old inbox files. Retrying the same
enrollment while paused repairs a lost observation without resetting receipts.
Changing the scope/head is refused, not silently re-enrolled.

The background consumer owns an inherited process `flock`; a surviving pi child
keeps a replacement consumer out after supervisor death. Short-lived admission
locks serialize ownership transitions with downstream prompt acceptance. Every
background request carries the current epoch. The native worker's supervisor
checks it again under the shared admission lock. Old epochs, paused ownership,
and foreground prompts while background owns the worker are refused. Takeover
never waits in a queue to run later; contention returns an error to retry after
the currently admitted operation settles.

The ticker retains, but does not send, queued prompts for background-owned
workers. Native guard sockets no longer accept direct prompts: the supervisor
is the sole prompt/receipt path. Neither a foreground CLI exit nor consumer
failure releases ownership. An explicit takeover is required. A takeover stops
**new admission**, not actions a worker already accepted. Inspect those receipts
and the worker's saved conversation before issuing anything new.

These are safeguards for **managed local processes**, not a sandbox against the
same OS user modifying files, opening a session manually, invoking unrelated
external commands, or running an old binary. Do not run old supervisor/ticker
binaries alongside enrolled workers. Existing supervisors without fencing are
ineligible: wait for a separately authorized safe lifecycle transition rather
than stopping an active worker to enroll it. No live migration is performed.

The model receives only a fixed automated event envelope and can return
`{"decision":"follow_up"}` or `{"decision":"block"}`. It has no executable tools;
an additional fail-closed extension blocks even late-loaded/nested tools. The
normal configured extensions and quartermaster guard baseline still load.
Custom event messages preserve automated provenance, not human authorization.
The Go supervisor constructs the worker notice itself and freshly checks task,
scope, resource, worker state and autonomy **again after the model turn**. Model
output cannot supply a command, target, approval, report, or evidence of success.
Normal pi extension trust/security limitations still apply.

## Durable follow-through and reports

`<state>/headless/` is private (0700, records 0600):

- `control.json`: explicit enrollment and foreground/background/paused epoch.
- `journal.json`: event identities and received → delivered → processing →
  action-requested → verified-handled or explicitly-blocked reconciliation.
- `consumer.lease`, `admission`: process ownership and mutation admission locks.
- `sessions/`, `events.jsonl`, `stderr.log`: private consumer diagnostics.
- `reports/`: deduplicated completed/blocked/decision-needed milestones.
- `acks/`: explicit report readback receipts, independent of processing.
- `notifications/`: local desktop submission attempts, **not** human readback.
- `watch.json`: bounded stall-alert episode state.

Beads remains task truth. These files store operational receipts, not a second
task board. A model response, RPC `accepted`, `agent_end`, `agent_settled`, or an
idle worker does **not** handle an event. The service observes the canonical
bead outcome; workers must still use the existing `quartermaster close` verifier.
Only a closed bead marks follow-through verified and archives the corresponding
inbox files. `inbox done` cannot clear background-owned events on mere delivery.
Coordination failures do not falsely change an active worker's task status.

Downstream request IDs are stable for the exact enrolled resource. Intent is
fsynced before sending. An unknown receipt, including a crash between intent
and sending, blocks instead of replaying. A late accepted receipt reconciles
without sending again. An accepted request remains outstanding until task
completion or an explicit blocker/deadline. The worker can be inspected while
it runs; it is never repeatedly restarted or prompted just because it is idle.

Proposal-only failures retry with exponential backoff and a capped attempt
count. Usage/provider failures eventually remain blocked without model polling.
Task blockers remain durable. A changed `needs_me`/`blocked` task status or newly
passing autonomy is relevant evidence to reconsider an unsent notification;
automated events cannot answer a human blocker. Unknown deliveries only
reconcile receipts. Other failures require foreground investigation. Clearing a
blocker and encountering it again creates a new report/readback episode.

Raw diagnostics may contain sensitive context: do not publish them. Desktop
text is fixed and generic, with no task names, PRs, resource names, model output
or transcripts. The outbox contains only identifiers, fixed reason codes and
timestamps. One desktop notification is attempted per watchdog invocation;
each report gets at most one attempt, even after a crash with uncertain desktop
delivery. The durable local report remains readable if desktop delivery fails.
Acknowledging a report never clears an event or approves an action.

## Isolated verification (no live cutover)

```sh
go build -buildvcs=false ./...
go vet -buildvcs=false ./...
go test .
go test -race . -run 'TestHeadless|TestRPC|TestPi'
python3 scripts/pi-headless-trial.py --pi /path/to/pi-1.5.2
python3 scripts/pi-coordinator-trial.py --pi /path/to/pi-1.5.2
```

The headless trial uses real pi processes: a restricted RPC coordinator, native
workers, and an interactive TUI on a private PTY. Its loopback model and fixture
Beads/GitHub need no credentials or external model. A fixed test-only tool
installs and reads back an isolated artifact before setting the fixture task
closed. It tests accepted-but-unfinished work, crash/restart without duplicate
action, duplicate/out-of-order events, blocked foreground delivery, bounded real
provider failures, report/readback and a healthy ticker with a dead consumer.
The interactive Unicode/multiline draft and cursor stay unchanged throughout.
It stops only its fixtures and retains private evidence. The separate TUI trial
covers human-input preflight, dialogs, compaction and the passive inbox race fix.
Go tests cover unknown/missing/late/mismatched receipts, authorization and scope
changes, stale epochs, takeover contention, no backlog execution, per-event
stall detection and report episodes. No trial proves production installation.

## Explicit cutover

After review and a **separate authorized installation**, choose a post-merge
code-only worker whose remaining work is already authorized. Keep all other
workers and the interactive coordinator intact. Do not reload the TUI, submit a
draft, or reactivate parked workers as part of this procedure.

```sh
quartermaster rpc status <bead> # require idle and coordinator_fence: true
quartermaster headless enroll <bead> https://github.com/owner/repo/pull/123
quartermaster headless status  # enrollment starts paused
quartermaster headless reports
quartermaster headless resume  # enable background admission; not permission for new work
quartermaster headless run     # foreground supervisor, normally run by a user service
```

Enrollment and enabling/takeover check every enrolled bead's class-3 authority.
`--by TJ` is only for commands the human actually decided to run, never an agent
workaround. Pausing always works without permission to resume a task or a live
task database. There is no implicit default switch, batch enrollment, or automatic
backlog activation. The initial bounded rollout holds at most 32 enrollments.

For a supervised background process, example units are in `systemd/`:
`quartermaster-headless.service`, `quartermaster-headless-watch.service`, and
`quartermaster-headless-watch.timer`. They are **not installed automatically**.
After separately authorizing installation, place the three files in your user
unit directory and set drop-ins for the actual binary/PATH, `BEADS_DIR`, and
`QUARTERMASTER_CONFIG_DIR`/`QUARTERMASTER_STATE_DIR` when non-default. Review these
paths; do not point a trial unit at live state. Then the explicit user commands
are:

```sh
systemctl --user daemon-reload
systemctl --user enable --now quartermaster-headless.service quartermaster-headless-watch.timer
quartermaster headless status
quartermaster headless reports
quartermaster headless ack <exact-report-id> # only after reading that report
```

The watchdog timer is independent of both ticker and consumer. `headless status`
shows last ticker tick, consumer loop, delivery and successful processing,
oldest actionable event, backlog, explicit blockers and oldest worker
follow-through timestamp. Check these independently; a healthy ticker is not a
healthy coordinator. The watchdog inspects not-yet-consumed inbox entries too,
reports one stalled episode and a recovery/accounted-for milestone, and never
restarts a worker or changes settings/credentials.

Optional config (defaults shown; values are bounded in code):

```toml
[headless]
poll_seconds = 15       # 1..300
stall_seconds = 300     # 5..86400, per event; unrelated progress cannot mask a stall
follow_seconds = 1800   # 5..86400, after downstream intent
model_seconds = 120     # 1..600 per proposal
backoff_seconds = 60    # 1..3600 base, exponential with at most five attempts
max_attempts = 3        # 1..5
```

## Pause, takeover, recovery and rollback

```sh
quartermaster headless pause    # immediately fence new background AND foreground prompts
quartermaster headless status
quartermaster rpc status <bead>
quartermaster rpc receipt <bead> <request-id-from-headless-journal>
quartermaster rpc view <bead>    # read-only retained history
```

Inspect task truth and any in-flight worker action. Do not replay a mutation
whose outcome is unknown. Then, when foreground control is separately warranted:

```sh
quartermaster headless takeover # new epoch, no background admission, no worker prompt
# Foreground can now use normal tell with context-specific, already-authorized instructions.
```

To roll back **this service**, pause first, stop only
`quartermaster-headless.service` and `quartermaster-headless-watch.timer`, then
explicitly take over after inspecting receipts. Do not stop the ticker, workers,
TUI, or delete journals/reservations. Keep this binary while enrolled worker
supervisors exist; an older binary cannot honor the new fence. Resume the same
service after repair with `headless resume`; the journal reconciles known worker
receipts, never replays unfinished mutations or treats old prose as permission.
Missing/corrupt ownership or journals fail closed and require inspection, not
file deletion to make a launch pass.
