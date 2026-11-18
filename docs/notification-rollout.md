# Notification fixes: merged-only installation and live verification

This is a preparation plan, **not an installation record or live acceptance**.
Implementation and isolated tests do not authorize runtime changes. A separately
authorized installer owns this procedure, with independent verification.

## Gates and immutable target

1. Require an approving GitHub review on the exact PR head, green required
   checks, and no unresolved findings. An unprotected repository or a local
   source review is not an approving GitHub review. Keep the PR draft and
   auto-merge off until this gate; check merge autonomy before marking ready
   and enabling auto-merge. Do not self-approve or bypass checks.
2. After merge, record the exact merge SHA, approved head and PR URL privately.
   The installer must separately check install autonomy and obtain exclusive
   installation ownership from the coordinator. Do not install a branch build
   or let `git pull` silently include unrelated unreviewed runtime changes.
3. Baseline for this work is `4e282900b1125aecee1aa4a0337c17d2530f29a2`.
   Re-read the actual installed/main state at install time; if it moved, review
   the full delta and redo this plan before changing anything.

## Preservation checkpoint (private evidence)

Create a fresh private scratch directory. Record without exposing credentials:

- Installed binary symlink/real path, SHA256, main checkout HEAD/clean status,
  ticker PID/start identity and `ticker status`.
- Coordinator process/session identity and name binding; hash the session
  prefix, control endpoint identity and inbox journal; capture `LastNudge` and
  pending/in-flight/ack watermarks **without token contents**. Do not read or
  export editor drafts: the user verifies those directly.
- Every existing worker reservation, supervisor/child PID and start identity,
  actual session path/id, parking files, request receipts and conversation
  prefix hashes. Hash task statuses/questions and retain working/untracked
  checkout state; do not dump private transcripts to public logs.
- Inbox unread/done file identities and hashes and any existing desktop journal.
  Save copies of the installed binary and bundled extension files as rollback
  artifacts, retaining their hashes and modes.

Read-only observations may race normal ongoing work: compare preserved prefixes
and identify append-only changes rather than requiring active transcripts to
remain byte-identical. No worker stop/restart, activation, enrollment, task
prompt, question answer, model/permission/config/credential change is allowed.
Do not activate the headless coordinator or watchdog as part of this repair.

## Install, only after separate authorization

In the main checkout identified by the installed binary (not this worktree):

```sh
git status --short             # must be clean
git pull --ff-only
git rev-parse HEAD             # must equal the recorded merged target
# Stop here if HEAD is not exactly the reviewed merged installation target.
go build -o bin/quartermaster .
quartermaster ticker stop && quartermaster ticker start
quartermaster autonomy level
quartermaster ticker status
quartermaster ticker notifications
```

Only the ticker restarts. Confirm both extension files are unchanged from the
preservation checkpoint: **this fix changes no pi extension**, needs no `/reload`
or connect command and must not touch the editor. Never inject a terminal
command. Existing workers retain their exact owners, sessions, parking and guard
configuration. Record installed SHA/binary hash/new ticker identity and errors.

## End-to-end verification (independent observer)

1. Confirm the exact installed binary and healthy ticker progression. Compare
   the preservation checkpoint; no supervisor/child replacement, parking or
   request-receipt changes attributable to installation are acceptable.
2. Inspect normal coordinator receipt and ticker state evolution read-only.
   If an archived valid same-session receipt is ahead of `LastNudge`, observe
   natural convergence on the next tick with no new receipt for that watermark.
   If no such live condition exists, say so; isolated regression evidence is
   not a fabricated live test. Never rewrite journals, timestamps, task status
   or inbox events to force a pass.
3. For the next naturally occurring already-authorized blocked/needs_me episode,
   check `ticker notifications`: detection precedes attempt, the result is
   persisted, repeated ticks do not resubmit, and the canonical task remains
   unanswered. Existing current blockers may produce one coalesced first-run
   alert. Do not manufacture task blockers or activate parked workers.
4. Ask the user to confirm actual desktop visibility for that exact episode,
   identifying the fixed generic text and approximate time. Record a separate
   visibility/readback observation. `submitted`, a zero exit, receipt bytes,
   a healthy ticker or coordinator acknowledgement is **not** that evidence.
   A failure/unknown result is incomplete visibility acceptance, not success.
5. With the user's chosen normal draft present, the user confirms text/cursor
   preservation while a natural coordinator notification stays pending; after
   they voluntarily clear/submit their draft, observe normal delivery and no
   duplicate. Never sample, submit or clear the draft for them.

Keep implementation/merge, installed revision, transport convergence, actual
visibility, human readback and task completion as separate claims. If natural
live events or user observation are unavailable, leave live acceptance pending
with the exact missing evidence; do not close the rollout on tests alone.

## Rollback / hold

On regression, preserve receipts/inbox/session state and stop expansion. Under
the installer's separate authorization stop **only the ticker**, restore the
saved baseline binary atomically, and restart only the ticker after confirming
that baseline preserves coordinator no-paste and worker parking semantics.
Do not delete/rewrite the new desktop journal or archived events; an older
binary ignores the outbox and may once again lose desktop errors or strand an
archived ack. State that limitation rather than claiming rollback repairs it.
Retain unchanged extension files, all workers and coordinator processes. If the
baseline is incompatible with any independently installed feature, hold the
ticker and request the smallest explicit recovery decision instead of rolling
back workers or their sessions.
