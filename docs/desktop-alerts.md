# Worker desktop alerts

The ticker reconciles **current** `blocked` / `needs_me` beads and workers
waiting at an approval/question prompt, including native pi RPC workers. This
is independent of transition-event suppression, coordinator inbox delivery and
headless enrollment. It never prompts, activates, answers, stops or restarts a
worker. Migration parking alone is not a new approval; an explicitly blocked
or needs_me task still counts.

## Evidence, not task state

```sh
quartermaster ticker notifications
quartermaster context
quartermaster ticker status
```

`notifications` is read-only JSON. It reports detection, each episode's attempt
count and timestamps, next safe retry, and the latest fixed outcome code. It
also explicitly labels human readback **unknown**. Read the question in the
existing task context; this report is not a second board or an approval API.

| State | Meaning |
| --- | --- |
| `detected` | Current blocker observed and recorded, not submitted yet. |
| `attempting` | Intent fsynced before calling the notifier; submission unknown. |
| `submitted` | Notifier exited zero. This is **not proof of visible display or human readback**. |
| `failed` | Notifier could not start; safe retries may remain. |
| `unknown` | Notifier started but exited nonzero/timed out, or a saved attempt lost its result. It may already have displayed; no automatic replay. |

Records live in `<state>/desktop/notifications.json` (0600, directory 0700).
A separate file lock excludes concurrent tick/notification attempts; saves use
atomic rename and file/directory fsync. The journal is independent of
`state.json`: saving a transition snapshot cannot suppress recovery. A malformed
journal fails closed with an error in the ticker log rather than silently
starting over. Missing worker status does not reset a previously observed
worker blocker; confirmed state changes end the episode. If all agent discovery
fails but the Beads read succeeds, bead-level blockers and safe retries still
reconcile; ordinary transitions are skipped and the discovery error remains
visible. If Beads itself is unreadable, reconciliation waits rather than
inventing task state.

Each current episode gets at most three **safe** attempts: immediate, after one
minute, then after five minutes. Every attempt coalesces all due episodes into
one generic local notification, globally limited to one per minute. Repeated
observations, ticker restarts and unchanged/missing prompt hashes cannot reset
the retry budget. A confirmed different approval prompt or a resolved-then-new
blocker creates a new episode. The latest receipt per task is retained, with all
active blockers plus at most 256 inactive receipts; this is an operational
outbox, not an append-only audit history.

First startup may summarize existing current blockers **once**, not one alert
per old inbox event. Historical inbox files are never scanned for desktop
replay. Successful or ambiguous submissions are never retried just because a
question remains unanswered. No desktop notification acknowledges an inbox item
or changes Beads status. There is no implicit human-readback receipt; actual
visibility/readback must be confirmed separately by the operator.

## Delivery and failure recovery

The adapter uses `notify-send`, the same local desktop convention as the digest,
without requiring a Herdr pane or a headless service. The ticker's environment
must find the notifier and reach the desktop user session. It has a three-second
timeout, no shell evaluation, and no Slack/email fallback. Desktop text is fixed:
no task identifiers/titles, commands awaiting approval, resource names, private
notes, subprocess output or transcripts appear on the desktop.

The ticker log records failed/unknown outcomes and storage errors with the
report command. For `notifier-unavailable`, restore the executable/PATH and wait
for a remaining scheduled attempt. After exhaustion or an unknown submission,
inspect desktop/session health and the durable report; do **not** delete or
rewrite receipts to force retries. The regular digest remains a separate
summary route. An explicit later manual diagnostic notification is not evidence
that the original submission displayed, and cannot answer the task question.

Other existing ticker notifications still use Herdr; `Herdr.Notify` now returns
its submission errors and the ticker logs them instead of discarding them.
This change does not promise durable retries for unrelated PR/review alerts.

## Tests and rollout

```sh
go build -buildvcs=false ./...
go vet -buildvcs=false ./...
go test -buildvcs=false .
go test -race -buildvcs=false . -run 'TestDesktop|TestHerdrNotify|TestPi|TestRPC'
python3 scripts/pi-coordinator-trial.py --pi /path/to/pi-1.5.2
python3 scripts/pi-coordinator-trial.py --pi /path/to/pi-1.5.2 --tui-mode regular
```

The adapter tests launch real isolated notifier executables, including missing,
successful, nonzero and timeout results; they never submit to the user's
desktop. Unit tests exercise failure/recovery/restart, lost-result uncertainty,
coalescing, budgets, file locking, malformed journals, bounded retention,
parking and task/readback independence. The real pi/PTY trial additionally calls
the Go ack reconciler against a fixture TUI's authenticated endpoint after
archival, verifying no duplicate receipt/model turn and unchanged transcript.

Source/tests/merge are not live repair. See the separate
[merged-only installation and verification plan](notification-rollout.md).
