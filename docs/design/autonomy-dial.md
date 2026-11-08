# Autonomy dial: one 0–9 level for the coordinator and workers

Status: **proposed** (2026-11-08). Nothing is built until TJ decides D1–D6
below. Builds on [autonomous-mode.md](autonomous-mode.md), whose trust model
it keeps.

## Summary

Today autonomy is on or off per bead (`autonomous` / `supervised` labels),
with four switches in `[autonomy]` (`decide`, `merge`, `apply`, `install`)
and a separate set of coordinator rules: three hook-prompt categories it may
answer, and "wait for TJ" for everything else. There is no way to say "let
the coordinator start the beads I queued, but keep workers asking", or "go
fully hands-off for the weekend".

This design replaces the on/off with a **level from 0 to 9** that covers both
the coordinator and the workers. It reads like log verbosity, inverted: at 0
TJ sees and approves everything; at 9 TJ hears only about critical operations
and the hourly digest.

Three things stay as they are:

- **The hard floor.** Some operations are never automatic at any level (see
  "The floor"). Level 9 means "everything except the floor".
- **The labels are the grant.** A level lives in a bead label and a state file
  that only TJ (or the coordinator on TJ's explicit word) changes. The ticker
  never raises a level, and an automated message approves nothing.
- **The check decides.** Before every step it takes without TJ, the worker or
  coordinator runs `quartermaster autonomy check`. A level change reaches a
  running worker at its next check, with no restart.

## The floor

Never automatic, at any level. These always go to TJ as `needs_me` with a
note starting `CRITICAL: <category>`, as today:

1. Approving a PR, including any bot approval.
2. Reading or writing secrets.
3. Deleting or destroying shared or remote resources, or replacing stateful
   ones; force-pushing to or deleting a shared branch; revoking credentials.
4. Irreversible operations (object lock, KMS key deletion, public disclosure).
5. Spending money.
6. Messaging people outside the team.
7. Anything Claude Code blocks or prompts for, other than the hook-prompt
   categories TJ has listed for the coordinator (see level 1). A
   classifier denial is always on the floor.
8. Anything `gate plan` blocks, and anything the agent is unsure about.
   **Unsure means critical.**

Floor actions have no level in the table below, so `autonomy check` can never
pass for one.

## The levels

Levels are cumulative: each adds one kind of step to everything below it.
"Coordinator" steps are taken by the coordinator session; "worker" steps by
the bead's own worker.

| Level | Name | Adds | `--action` |
|---|---|---|---|
| 0 | Hold | Nothing is automatic. The coordinator answers no prompts, `auto_close` and `auto_resolve` act as off, every check fails. The ticker still watches and reports. | — |
| 1 | Default | Today's supervised behavior. The coordinator answers hook prompts in TJ's listed categories and resolves merged worktrees. Workers ask on every decision. | `answer-prompt`, `resolve` |
| 2 | Housekeeping | Coordinator: dispatch beads labeled `next`, resume resumable threads, close `likely_stale` beads on strong evidence, release stale claims. All of it is TJ-selected or bookkeeping, and reversible. | `dispatch`, `resume`, `close` |
| 3 | Decide | Worker: take its own recommendation on a reversible decision. | `decide` |
| 4 | Merge | Worker: merge its own PR once approved and green (stacks retargeted), post `/plan`-style triggers on TJ's PRs, and for herdr-quartermaster beads install the merged PR. | `merge`, `install` |
| 5 | Roll | Worker: the next step of a staged rollout once the previous one verified, and **non-production** applies that pass `gate plan`. | `rollout`, `apply` (non-prod target) |
| 6 | Production | Worker: **production** applies that pass `gate plan` and match the change noted beforehand. | `apply` (prod target) |
| 7 | Delegate | Worker: take its recommendation on any non-critical decision, reversible or not. Coordinator: answer a non-critical `needs_me` with the worker's recommendation, after checking the note has one and no `CRITICAL:`. | `decide --any`, `answer` |
| 8 | Speak | Coordinator and workers: post to the team's internal channels listed in `[autonomy] channels` (review requests, status), and comment on Linear issues beyond the bead's own. | `post` |
| 9 | Hands-off | Nothing new is allowed. Only reporting changes: TJ hears about the floor (`CRITICAL:`, `RUN:`) and the digest, nothing else. | — |

Level 9 adds no permission on purpose. Everything below the floor is already
allowed at 8; 9 is the "don't tell me" setting.

Today's `autonomous` label is level **6** (decide, merge, install, rollout,
apply on any stack). Today's unlabeled bead is level **1**.

### Reporting volume

The coordinator's updates to TJ shrink as the level rises. The ticker's
inbox, the board and the digest are unchanged at every level; this is only
what the coordinator says.

| Global level | The coordinator tells TJ |
|---|---|
| 0–2 | Everything, as today. |
| 3–5 | As today, but `AUTO:` steps are listed once per update, not one message each. |
| 6–8 | Needs-you items (anything at `needs_me`, a prompt it may not answer), failures it can't fix, and a one-line count of `AUTO:` steps since the last update. |
| 9 | Only `CRITICAL:` and `RUN:` items, as they arrive. Everything else waits for the hourly digest, which keeps its full "Done without you" section. |

`quartermaster context` prints the current reporting mode at the top, so the
coordinator reads it every turn instead of remembering it.

## Decisions for TJ

### D1. How many levels

- **(R) a.** Keep 0–9, re-packed as in the table above, so each level is one
  risk boundary. Against the starting draft: resuming threads and closing
  `likely_stale` beads move down to 2 (they are bookkeeping, like
  dispatching a `next` bead), and non-prod applies join the rollout step at 5.
  Production applies get a level of their own, because that is the biggest
  single jump in risk. The numbers TJ will actually use are likely 1, 4, 6
  and 9; the rest cost a row in a table, and leave room to split a level later
  without renumbering.
- b. The draft as written (resume at 5, non-prod apply at 6, close at 7).
- c. Six levels, 0–5: Hold, Default, Housekeeping+Decide, Merge+Roll,
  Production+Delegate, Hands-off. Fewer choices, but merging and production
  applies can then only be granted together with something else.

### D2. Global level and per-bead override

Three settings, which map onto today's three:

```toml
[autonomy]
level = 1        # the global level: every bead without its own (was `default`)
max = 9          # the ceiling no bead goes above (was `enabled`)
channels = []    # internal channels level 8 may post to
# decide, merge, apply, install stay as per-kind kill switches at any level
```

- A bead's own level is the label `autonomy:N`. Its **effective level** is
  `min(own level, or the global level if it has none, max, stop file)`.
- **(R) a.** A bead **may go above the global level**, up to `max`. That is
  what the `autonomous` label does today on top of `default = false`, and it
  is the common case: "run this one bead hands-off, keep the rest
  supervised". A bead may also go below it (`autonomy:1` on a sensitive bead
  while the global level is 6), replacing `supervised`.
- b. The global level is a ceiling, and a bead can only go lower.
- Coordinator steps about a bead (dispatch, resume, close, answer) use that
  bead's effective level. Reporting volume uses the global level only, since
  it is about TJ's attention, not any one bead.

### D3. CLI and the check

```
quartermaster autonomy level              # show global, max, stop file, and each active bead's effective level
quartermaster autonomy level N            # set the global level (state file, see below)
quartermaster autonomy level N <bead>...  # set beads' own level: label autonomy:N, note, tell the worker
quartermaster autonomy level - <bead>...  # drop a bead's own level, back to the global one
quartermaster autonomy check <bead> --action A [--target <stack>]
quartermaster autonomy on|off             # unchanged, see D4
```

- The global level set from the CLI is a state file (`autonomy-level` in the
  state dir), like today's stop file; it wins over `[autonomy] level`, which
  is the level after a fresh install or when the file is removed. This keeps
  `level N` from rewriting `config.toml`.
- `check` becomes a table lookup: each action has a minimum level (the table
  above), and the check passes when the bead's effective level is at least
  that, the per-kind switch is on, and the action-specific condition holds
  (`install` only in herdr-quartermaster; `apply` uses `--target`, see D5).
  An unknown action fails. `--action` omitted means `decide`, the lowest
  worker step, so a worker's old bare check means what it did.
- The coordinator gets checks too. Today "wait for TJ before dispatch" is a
  prose rule; with the dial the guide says "run `autonomy check <bead>
  --action dispatch`; if it passes, dispatch and note it, else propose it".
  `quartermaster dispatch`, `resume`, `answer` and `stale --release`
  themselves refuse to run without `--by TJ` when the check fails, and the
  ticker's `auto_close` closes a `likely_stale` bead only when `close` passes
  for it, so the rule is enforced in code, not only in the coordinator's
  reading of its guide.
- Setting a bead's level sends its worker a notice tagged `[quartermaster
  autonomy: level N set by TJ]` with the same delivery rules as today's grant
  and revoke (prompted when idle, else queued; a lowering waits out
  `needs_me`). The worker trusts it only as far as `autonomy check` passes.

### D4. Migration

- **(R) a.** One release reads both forms, then the old ones go:
  - Label `autonomous` reads as `autonomy:6`, `supervised` as `autonomy:1`.
    On first run the ticker rewrites them to the new labels and notes each
    bead. Both mean exactly what they mean today.
  - `[autonomy] enabled = false` reads as `max = 1`; `default = true` as
    `level = 6`. `quartermaster configure` writes the new keys.
  - `autonomy grant <bead>` becomes `level 6 <bead>`, and `revoke <bead>`
    `level 1 <bead>`; both stay as aliases. The board's `U` key cycles
    1 → 4 → 6 → 1, and a digit key sets any level.
  - `autonomy off` caps every bead at **1**, not 0: it is the panic switch
    for autonomy, and should not also stop the coordinator answering the safe
    hook prompts that keep workers moving. `autonomy level 0` is the
    separate, explicit "hold everything". `on` removes the cap.
- b. Map `autonomous` to 4 (decide and merge only), so beads granted today
  stop for their applies until TJ raises them. Safer, but it changes what
  running beads do on upgrade.

### D5. Production vs non-production applies

Level 5 needs to know whether an apply touches production; `gate plan` reads
the plan, not the stack's purpose.

- **(R) a.** A config list of patterns matched against the target the worker
  passes (`--target <stack or workspace>`): `[autonomy] prod = ["*prod*",
  "*-production"]`. **A missing or unmatched target counts as production**,
  so a mistake in the list costs a stop, not an unwanted prod apply. The
  worker also states the target in its pre-apply note, so the `AUTO:` record
  shows what it claimed.
- b. The worker's own judgement, tagged in the note. Simpler, but the only
  boundary between 5 and 6 would be the agent's word.
- c. No split: one apply level. Then D1 (a) loses level 6's reason to exist,
  and merges it with 5.

### D6. Coordinator rules, and where the guide changes

- **(R) a.** The coordinator guide (generated in `context.go`) keeps one rule
  per kind of step, each pointing at its check, and loses the fixed "wait
  for TJ". The level is not baked into the guide text: the guide is read once
  per session, while `context` and the checks are read every turn, so a level
  change takes effect without a new coordinator. Concretely:
  - "Propose threads and wait for TJ" → "dispatch a `next` bead when `check
    --action dispatch` passes; otherwise propose it".
  - "Offer resume / stale --release / closing a likely_stale bead" → the
    same, behind `resume` and `close`.
  - "Relay a needs_me verbatim and never answer it for TJ" → "relay it,
    unless `check --action answer` passes, the note has a recommendation and
    no `CRITICAL:`; then prompt the worker with `AUTO: take your
    recommendation` and note it".
  - The hook-prompt categories in TJ's `coordinator_rules` stay where they
    are, behind `check --action answer-prompt` (level 1), so level 0 turns
    them off.
  - Reporting follows the mode `context` prints (see "Reporting volume").
  - The coordinator changes a level only on TJ's explicit word, as with
    `grant` today.
- b. Render level-specific rules into the guide. Fewer checks to run, but
  the guide goes stale the moment the level changes.

## Auditing

Unchanged in spirit from autonomous mode, extended to the coordinator:

- Every step taken without TJ leaves `bd note <bead> "AUTO: [<actor> L<n>]
  <what> — <evidence>"`, where actor is `worker` or `coordinator` and `n` the
  effective level at the time, e.g. `AUTO: [coordinator L2] dispatched (next
  label)` or `AUTO: [worker L5] applied stack staging-x, 3 update / 0 delete
  (gate ok, non-prod)`. The prefix stays `AUTO:`, so `autoNotes`, the digest
  and the report pick them up with no change.
- The commands that act (`dispatch`, `resume`, `answer`, `stale --release`,
  `level`, and the ticker's auto-close)
  write the note themselves when run without `--by TJ`, so the record does not
  depend on the agent remembering.
- Every level change, global or per bead, is noted (per bead) or logged to
  the inbox as an event (global), with who asked for it.
- Linear comments stay at milestones only: a decision, an apply, a merge.

## What does not change

- The trust model: labels and the state file are the grant; checks enforce
  it; ticker messages approve nothing; Claude Code's prompts and classifier
  are never answered beyond TJ's listed hook categories or worked around.
- `gate plan`, its allowlist, and `--expect`.
- Worker hosts: a host without the `quartermaster` binary fails every check,
  so its workers act at level 1 at most, whatever their label says.
- Muse workers follow the same levels.

## Plan (after TJ decides)

1. Level model: `autonomy:N` labels, `level`/`max`/`prod`/`channels` config,
   the state file, the effective-level function and the action table, with
   tests for each threshold and for migration of old labels and keys.
2. CLI: `autonomy level`, `check --action … --target`, grant/revoke aliases,
   the level notice; the board's `U` and digit keys and a level marker on each
   row instead of `auto`.
3. Enforcement in `dispatch`, `resume`, `answer`, `stale --release` (refuse
   without `--by TJ` when the check fails), `auto_close` gated on `close`,
   and their `AUTO:` notes.
4. Coordinator guide and `context`'s reporting-mode line.
5. Brief: the grant section lists the steps with their checks as today; the
   check, not the brief, decides which pass at the bead's current level.
6. README and the standing authorization in TJ's CLAUDE.md, rewritten from
   "a bead labeled autonomous" to "a step whose `autonomy check` passes".
