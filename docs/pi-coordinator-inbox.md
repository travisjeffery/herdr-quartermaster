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

The extension polls at 250ms, including after the last notification arrived, so
clearing a draft does not require another tick. It waits for:

- No active agent run, queued user message, compaction or tree navigation.
- The existing `CustomEditor` focused, no overlay or extension dialog, and both
  the expanded editor text and editor text **exactly empty** (not trimmed).
- No incomplete bracketed paste and at least one second since terminal input.
- A saved session file. A new session must first save a conversation message;
  notifications cannot make an unsaved session's in-memory receipt durable.

Until then the footer says `inbox: pending`. Draft text is read but never saved,
logged, restored or transmitted by the notification extension. Cursor,
selection, undo state and pending input are not modified.

**Idle delivery does not start a model turn.** It appends a visible, durable
custom message that the coordinator sees on its next request. During an active
run, delivery at `agent_before_settle` appends the message and requests one
continuation. This deliberately replaces automatic idle model wake with passive
idle notification: pi 1.5.2's `isIdle()` excludes asynchronous human-input
preflight, so `sendMessage({triggerTurn: true})` could race and reject a submitted
human prompt. We never use that unsafe idle wake. The check and passive append
have no asynchronous gap, and later keystrokes stay in the editor.

The custom message carries its watermark and session identity. Acknowledgement
requires the corresponding entry on disk, followed by an fsync. On restart an
in-flight intent is reconciled against that session before retrying: a persisted
custom message is not delivered twice. Errors retain state and show a footer
warning; transient journal write failures retry with backoff, including during
channel initialization. A missing or
unreadable session/receipt fails closed rather than guessing that a prior send
never happened. This guarantees notification receipt, not successful completion
of the coordinator's subsequent model/tool work.

Across `/new`, `/resume` or `/fork`, pending work retains its old session identity
until a **fresh authenticated ticker request addressed to the new session**
revalidates it. A shared acknowledgement prevents replay when returning to the
old session. Stale-session requests are rejected. A live socket is never
intentionally replaced; a refused stale socket is recovered on startup.

Pi 1.5.2 has no completion event for a **canceled/failed session switch or fork**.
Such an operation conservatively pauses automatic notifications. Submitting a
human prompt, or explicitly running `/quartermaster-inbox-connect` again after
canceling the operation, releases that pause. Tree-navigation cancellation is
automatic: pi's `isIdle()` covers the full operation, including aborts. No timeout
is used to guess that an unresolved session replacement is safe. Editors that do
not inherit `CustomEditor` also fail closed rather than being replaced.

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
go test .
go test -race . -run 'TestPi|TestRPC'
python3 scripts/pi-coordinator-trial.py --pi /path/to/pi-1.5.2
python3 scripts/pi-coordinator-trial.py --pi /path/to/pi-1.5.2 --tui-mode regular
```

The trial creates a private scratch agent directory, a real pi TUI on an isolated
PTY and a loopback HTTP fixture model. It uses no credentials, external model,
live configuration, tasks or panes. It verifies Unicode/multiline drafts and
cursor position, user-priority ordering, delayed human-input preflight, custom
rather than human origin, active-boundary continuation, dialogs (including a built-in selector), compaction, canceled tree
navigation, duplicate reconnect, crash/restart and session identity. It stops
only its own fixture processes and retains private evidence.

The Node policy fixtures additionally test bounded coalescing, both intent/ack
crash windows, transient journal errors, competing listeners and cross-session
revalidation. Go tests assert absent/stale sockets and unknown identity cannot
fall back to terminal input. Test-only observer extensions are never embedded.

For a live rollout, preserve the previous binary and extension files, activate
only with the safe user action above, and verify a pending footer notification
while a draft/cursor remain unchanged. Keep the durable inbox and journal on
rollback. An older ticker may resume terminal injection: stop it or disable its
nudges before using an older build. Do not restart workers or discard sessions.
