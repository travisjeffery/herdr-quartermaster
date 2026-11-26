# Draft-safe pi coordinator inbox

The pi TUI coordinator's ticker notifications are **extension-authored custom
messages**, not human prompts or authorizations. They never use terminal paste,
`sendUserMessage`, editor setters, an editor replacement, or synthetic keys.
Worker prompt/ask sockets and non-pi coordinator behavior are unchanged.

## Delivery and recovery

Local coordinator launches include `--quartermaster-coordinator <state>/pi/control/coordinator-inbox.sock`.
The bundled `pi/coordinator.ts` companion is written beside `quartermaster.ts`.
The socket and token are 0600 in a 0700 directory. The protocol accepts only
`inbox` and `inbox_status`, with the token and **exact current session file**.
An `inbox` request carries a fixed-width nanosecond inbox watermark, not arbitrary
text. The extension creates the notification text itself.

The existing durable event inbox remains the source of truth. The receiver
fsyncs a bounded journal (`<socket>.inbox.json`) containing one pending high-water
mark, one in-flight mark and one acknowledged mark. Repeated ticks coalesce; a
lost reply is safe to retry. The ticker advances `LastNudge` only on a
transcript-backed delivery acknowledgement, not acceptance into memory or disk.

The ticker checks the authenticated endpoint even when no unread events remain.
A newer acknowledgement must be nonfuture, name an exact durable event in either
`inbox/` or `inbox/done/`, and have a complete matching custom-message receipt in
the **exact current session file** returned by the endpoint. Thus archiving the
newest or all events cannot strand a valid receipt after a lost reply or ticker
state save. An arbitrary cursor, a journal watermark alone, an unrelated event's
maximum timestamp, or a receipt from another session is insufficient. Missing
historical event/receipt evidence fails closed; retain archives on rollback and
do not rewrite the live journal or `LastNudge` to manufacture convergence.
Acknowledgement is monotonic and does not archive tasks or certify human
readback. A newer valid same-session event can reconcile after a session switch.

## Guarded idle wakeups (pi 1.6.0)

The extension polls at 250ms, including after the last notification arrived, so
clearing a draft does not require another tick. Before triggering an idle turn
(`pi.sendMessage(..., { triggerTurn: true, deliverAs: "followUp" })`), every
one of these invariants must hold simultaneously:

- No active agent run (`ctx.isIdle()`), queued user message
  (`!ctx.hasPendingMessages()`), compaction, or tree navigation.
- No in-flight human-input preflight (see below).
- The existing `CustomEditor` is focused, no overlay or extension dialog is
  open, and both `ctx.ui.getEditorText()` and `editor.getText()` are **exactly
  empty** (`""`, not trimmed).
- No incomplete bracketed paste (`\x1b[200~` without matching `\x1b[201~`) and
  at least one second (`1000ms`) since the last terminal input or run
  settlement.
- A saved session file exists on disk matching the pending watermark's session,
  and a model is selected when `ctx` exposes `model`.

Until all invariants hold, the footer shows `inbox: pending` and the editor is
never touched. Draft text is read only for emptiness/submission detection and is
never saved, logged, restored, modified, or transmitted by the extension.
Cursor, selection, undo state, and pending input are untouched.

### Human-input preflight guard

In **pi 1.6.0**, `AgentSession` increments `_promptPreflightCount` during
`_preparePrompt()` (async `input` extension hooks and `checkAuth()`), but
`AgentSession.isIdle` (`!_isAgentRunActive && !isCompacting`) and
`sendCustomMessage({ triggerTurn: true })` still do not check
`_promptPreflightCount`. Furthermore, `ExtensionRunner.emitInput()` runs
extension `input` handlers sequentially in load order, so an earlier extension
can hold preflight before `coordinator.ts`'s own `input` listener runs.
Triggering `_startRun()` during that window would flip `_isAgentRunActive = true`
and cause `InteractiveMode`'s `session.prompt(userInput)` (called without
`streamingBehavior`) to reject the human prompt.

`pi/coordinator.ts` closes both halves of that window:

1. **Editor-submit detection (pre-`input`):** `onTerminalInput` runs before
   `Editor.handleInput`. When an idle, focused `CustomEditor` receives a submit
   key outside bracketed paste (Enter / Alt+Enter / Kitty Enter, excluding
   Shift+Enter newlines and key releases) for non-command text (not `!bash`,
   not a built-in `/command`, and not a registered extension command), a
   microtask verifies that `Editor.handleInput` synchronously cleared the editor
   while remaining idle and arms the preflight guard before any timer or
   earlier async `input` hook can race.
2. **`input` event guard:** Any idle `input` event without `streamingBehavior`
   arms/refreshes the preflight guard to cover remaining `input` handlers and
   async `checkAuth()`.
3. **Lifecycle clearing & bounded recovery:** `before_agent_start`,
   `agent_start`, and `agent_settled` clear the preflight guard immediately once
   `_startRun()` synchronously marks `_isAgentRunActive = true`. If a prompt is
   consumed (`{ action: "handled" }`) or fails auth without starting a run, the
   preflight guard expires cleanly after a bounded timeout (`15s`) so wakeups
   never wedge permanently.

Once `pi.sendMessage(..., { triggerTurn: true, deliverAs: "followUp" })` is
invoked while idle, `AgentSession._startRun()` sets `_isAgentRunActive = true`
synchronously before returning. Any keystrokes typed afterward stay safely in
the editor, and submitting them queues cleanly as steering/follow-up instead of
being rejected. During an already-active run, delivery at `agent_before_settle`
appends the custom message and requests one continuation without racing a
separate idle run.

### Bounded at-most-once wakeup (no retry/turn loops)

The custom message carries its watermark and session identity. Acknowledgement
requires the corresponding `custom_message` entry on disk, followed by an fsync:

- **Persisted wakeup:** `runAgentLoop` persists the `quartermaster.inbox`
  custom message on `message_end` before calling the model. Once acknowledged,
  `state.ack` advances to that watermark and the ticker advances `LastNudge`.
  Even if the subsequent model turn errors, is aborted, or chooses not to
  archive an inbox item, **no second wakeup fires** for that watermark until a
  strictly newer durable watermark arrives.
- **Unpersisted wakeup failure:** If `sendMessage({ triggerTurn: true })` fails
  before persisting the custom entry (either observed on `agent_settled` or
  after the 2s idle grace window), `state.flight` rolls back to `state.pending`,
  the footer displays `inbox: delivery error (retained)`, and `failedWake`
  latches that watermark so `pump()` **never loops model turns** on the same
  watermark. A strictly newer watermark, an explicit `/quartermaster-inbox-connect`
  resume, or a clean process restart (`recover()`) gets one fresh attempt.
- **Transient journal errors:** Disk errors while writing the pre-send intent
  occur *before* calling `pi.sendMessage` and still retry with backoff without
  tripping `failedWake`.

## Coordination with headless `inbox-runner`

Every `inbox` and `inbox_status` reply includes two privacy-safe readiness
fields (no draft text or user input):

- `wake_state`: fixed enum describing live wakeup readiness (`idle`, `ready`,
  `quiet_wait`, `waking`, `busy`, `compacting`, `preflight`, `draft`, `dialog`,
  `overlay`, `switching`, `unfocused`, `session`, `no_model`, `failed`,
  `error`, `closed`).
- `idle_ms`: milliseconds since the last terminal input or settled turn.

In `Ticker.once()`, `nudgeCoordinator` passes the observed `piCoordinatorStatus`
directly into `triggerInboxRunnerWithStatus` (and standalone `inbox-runner`
commands probe `inbox_status` read-only):

- When the live coordinator reports `ready`, `quiet_wait`, `waking`,
  `preflight`, `busy`, or `compacting`, `inbox-runner` steps aside (`waking` or
  `busy`) so live wakeups and headless runs never duplicate-execute the same
  inbox items.
- When the live coordinator reports recent TUI input (`idle_ms < idle_seconds`),
  `inbox-runner` skips (`recent_activity`).
- When the interactive coordinator is closed, or stuck on a non-empty user
  draft, dialog, overlay, switch pause, or failed wakeup for at least
  `idle_seconds`, `inbox-runner` proceeds cleanly as the AFK fallback without
  disturbing the TUI.

## Session replacement and recovery

On restart an in-flight intent is reconciled against the saved session before
retrying: a persisted custom message is not delivered twice. A missing or
unreadable session/receipt fails closed rather than guessing that a prior send
never happened. This guarantees notification receipt, not successful completion
of the coordinator's subsequent model/tool work.

Across `/new`, `/resume` or `/fork`, pending work retains its old session identity
until a **fresh authenticated ticker request addressed to the new session**
revalidates it. A shared acknowledgement prevents replay when returning to the
old session. Stale-session requests are rejected. A live socket is never
intentionally replaced; a refused stale socket is recovered on startup.

Pi 1.6.0 still has no completion event for a **canceled/failed session switch or
fork**. Such an operation conservatively pauses automatic notifications.
Submitting a human prompt, or explicitly running `/quartermaster-inbox-connect`
again after canceling the operation, releases that pause. Tree-navigation
cancellation is automatic: pi's `isIdle()` covers the full operation, including
aborts. No timeout is used to guess that an unresolved session replacement is
safe. Editors that do not inherit `CustomEditor` also fail closed rather than
being replaced.

## Activating an already-running coordinator

Do not restart a coordinator, submit its draft, or inject `/reload` to activate
this channel. After installing the reviewed build, ensure **both** extension
files in `<state>/pi/` match that build. Existing workers keep their already-loaded
extension and are not restarted. New launches write both files automatically.

An already-running coordinator without the new launch flag needs these commands
issued by its user, after preserving/submitting their draft themselves:

```text
/reload
/quartermaster-inbox-connect /absolute/state/directory/pi/control/coordinator-inbox.sock
```

The connection is recorded as a session-local custom entry so subsequent reloads
of the same session restore it. An inherited entry in a fork does not activate
the channel for a different session. If a manually connected coordinator changes
sessions, explicitly connect that new session too. Launch-flag coordinators
reestablish the channel automatically, with the session checks above.

While activation is pending, the ticker keeps its inbox; **there is no terminal
fallback**. Restarting only the ticker is safe and does not change editor state.
Never print token files or publish session/receipt contents during verification.

## Validation

```sh
go build ./...
go vet ./...
go test -race ./...
node --test testdata/pi-coordinator.test.mjs
python3 scripts/pi-coordinator-trial.py --pi "$(command -v pi)"
python3 scripts/pi-coordinator-trial.py --pi "$(command -v pi)" --tui-mode regular
```

The trial creates a private scratch agent directory, a real pi 1.6.0 TUI on an
isolated PTY and a loopback HTTP fixture model. It uses no credentials, external
model, live configuration, tasks or panes. It verifies Unicode/multiline drafts
and cursor position, user-priority ordering, idle turn wakeups on empty editor,
delayed human-input preflight blocking without prompt rejection, custom rather
than human origin, active-boundary continuation, dialogs (including a built-in
selector), compaction, canceled tree navigation, duplicate reconnect,
crash/restart and session identity. It stops only its own fixture processes and
retains private evidence.

The Go reconciler is also exercised against the real fixture pi endpoint after
archiving its newest/all inbox events, including a simulated lost ticker-state
save. Separate [desktop alerts](desktop-alerts.md) have their own operational
receipts and never depend on coordinator acknowledgement.

The Node policy fixtures additionally test guarded idle wakeups, human
submit/input preflight blocking and bounded timeout recovery, at-most-once
wakeup bounds (no retry/turn loops on failed or un-archived turns), bounded
coalescing, both intent/ack crash windows, transient journal errors, competing
listeners and cross-session revalidation. Go tests assert absent/stale sockets
and unknown identity cannot fall back to terminal input, and verify coordination
between live pi wakeups and the headless inbox runner. Test-only observer
extensions are never embedded.

For a live rollout, preserve the previous binary and extension files, activate
only with the safe user action above, and verify a pending footer notification
while a draft/cursor remain unchanged. Keep the durable inbox and journal on
rollback. An older ticker may resume terminal injection: stop it or disable its
nudges before using an older build. Do not restart workers or discard sessions.
