# Autonomy dial: one 0–9 level, set by where an action lands

Status: **proposed** (2026-11-08), second draft. Nothing is built until TJ
decides D1–D6 below. Builds on [autonomous-mode.md](autonomous-mode.md),
whose trust model it keeps.

## Summary

Today autonomy is on or off per bead (`autonomous` / `supervised` labels),
with four switches in `[autonomy]` (`decide`, `merge`, `apply`, `install`)
and a separate set of coordinator rules. Each rule is about **who** acts
(the worker, the coordinator) and **which capability** (decide, merge).

This design replaces all of that with one number from 0 to 9, and classifies
**the action, not the actor**. Every action gets an **impact class**: how far
its output reaches, from the agent's own scratch files (0) up to widespread
production deletion (9). The dial at N means:

> **Any action whose impact class is at most N may run without TJ.**
> Anything above N stops for TJ.

Who takes the action doesn't matter. A worker taking its own recommendation,
the coordinator answering a `needs_me` question, and the coordinator answering
an approval prompt are judged the same way: by the class of the action their
choice authorizes. "The coordinator answers needs_me" is no longer a level of
its own. At dial 4, the coordinator may answer "merge now or wait?" for TJ
(class 4) but must relay "apply to staging?" (class 6).

It reads like log verbosity, inverted: at 0 TJ sees and approves everything;
at 9 TJ sees only what's left outside the dial (see D2) and the digest.

Three things stay as they are:

- **The labels are the grant.** A level lives in a bead label and a state file
  that only TJ (or the coordinator on TJ's explicit word) changes. The ticker
  never raises a level, and an automated message approves nothing.
- **The check decides.** Before every step it takes without TJ, the worker or
  coordinator runs `quartermaster autonomy check`. A level change reaches a
  running worker at its next check, with no restart.
- **Every automatic step leaves an `AUTO:` note.**

## The impact ladder

Each class names where the output lands. The examples are today's real
actions.

| Class | Where it lands | Examples today |
|---|---|---|
| 0 | Nowhere: read-only | Reading files, `bd show`, `gh pr view`, `herdr agent read`; a subagent whose task is a read-only lookup or review; `gate plan`, `pulumi preview`, `tofu plan` |
| 1 | The agent's own worktree and scratch | Edits, local builds and tests, `git add`/`commit` in its own worktree, `rm` inside its worktree or a `mktemp -d` directory, `bd note` on its own bead |
| 2 | Its own branch and PR (remote, unmerged, reviewed before it lands) | `git push` to its own branch, opening or updating its PR, replying on its PR's threads, `/plan`-style trigger comments, retargeting a stacked PR, moving its own Linear issue's status. **A choice that only shapes its own PR** (a design option, a naming, a test approach), since the PR still gets human review |
| 3 | TJ's machine and the shared bead state | Dispatching a `next` bead, resuming a thread, closing a `likely_stale` bead, releasing a stale claim, `resolve` of a merged worktree, installing a merged herdr-quartermaster PR (pull, build, ticker restart) |
| 4 | The shared repo's main branch | Merging its own approved, green PR (`--auto` or `--match-head-commit`) |
| 5 | Internal messages | Posting to the team's internal channels (review requests, status), commenting on Linear issues other than its own |
| 6 | Non-production infrastructure | An apply to a dev or staging stack that passes `gate plan`; a deploy dispatch or rollout step to a non-prod environment |
| 7 | Production, additive or in place | A prod apply that passes `gate plan` (create, update, no delete or replace); a prod deploy dispatch; the next step of a prod rollout once the previous one verified |
| 8 | Production deletion, scoped | One delete or replace in a prod stack, or a stateful replace anywhere; deleting a non-prod database, bucket or volume; force-pushing to a shared branch |
| 9 | Widespread or irreversible | Deletes across stacks or past the `widespread` threshold (D3); deleting prod data; object lock or KMS key deletion |

Rules that place an action:

- **An action takes the highest class among its effects.** A merge into a repo
  that deploys main to prod on merge is class 7, not 4 (D3 lists such repos).
- **A decision takes the class of what the chosen option does.** "Option 2:
  canary on staging first" is class 6 when it runs the canary; a choice
  between two code designs in its own PR is class 2.
- **Unsure means 9.** An action nobody can place stops for TJ unless the
  dial is at 9, and with D2 (a) not even then if it touches the floor.

### What TJ hears

TJ always hears about anything above the dial: it is waiting on TJ. What
changes with the dial is how much the coordinator says about what ran
without TJ. The ticker's inbox, the board and the digest are the same at
every level.

| Global dial | The coordinator reports what ran without TJ |
|---|---|
| 0–3 | Each `AUTO:` step, as it happens |
| 4–6 | One line per update listing the `AUTO:` steps since the last |
| 7–9 | Nothing between digests, except failures it can't fix; the hourly digest's "Done without you" section has them all |

`quartermaster context` prints the dial and the reporting mode at the top,
so the coordinator reads them every turn instead of remembering them.

## Decisions for TJ

### D1. The ladder

- **(R) a.** The ten classes above. Between "own branch/PR" and "main" sits
  class 3, TJ's machine and the bead state: dispatching, resuming, closing
  stale beads and the self-install touch nothing anyone else sees, but they
  start work and spend quota, so they sit above a push.
- b. The same ladder without class 3. Those actions fold into class 1, so
  any dial that lets an agent commit also lets the coordinator dispatch and
  resume beads.
- c. A coarser ladder: local (0–1), own PR (2), shared repo and messages (3),
  non-prod (4), prod change (5–6), prod deletion (7–8), widespread (9), with
  the in-between numbers unused. Fewer distinctions, same scale.

### D2. Does anything stay outside the dial, even at 9?

This is the open question. Some actions don't fit "how far does it reach":
approving PRs, secrets, messaging people outside the team, spending money,
and anything Claude Code blocks.

- **(R) a.** **Yes, a floor outside the dial**: no level lifts it.
  - **Approving a PR**, including any bot approval. Required review is the
    org's control on what reaches main; lifting it makes class 4 unreviewed.
  - **Reading or writing secrets.** The blast radius of a leaked secret isn't
    knowable when it's read.
  - **Anything Claude Code blocks or prompts for that TJ hasn't listed**, and
    every auto-mode classifier denial. Quartermaster can't lift these anyway:
    Claude Code enforces them. Placing them on the dial would only promise
    something it can't deliver.
  - **Messaging people outside the team** and **spending money.** Both act
    on people and accounts outside the systems the ladder measures, and
    neither can be taken back.

  At 9 TJ hears only about the floor and the digest. 9 then means "anything
  inside our systems, including widespread prod deletion".
- b. A smaller floor: approving PRs, secrets and Claude Code blocks only.
  Outside messages go on the ladder at 8 and spending money at 9.
- c. No floor except what Claude Code itself enforces: 9 lifts everything
  else, including PR approval and secrets.

### D3. How an action is classified

Three classifiers, and **the action's class is the highest any of them
gives**, so none can lower another's call:

1. **Mechanical, where a tool can tell.** `gate plan` prints a class from the
   plan: 6 or 7 by target (below), 8 for any delete or replace in prod or a
   stateful replace anywhere, 9 past the `widespread` threshold. The action
   table (D6) gives the fixed class of `merge`, `dispatch` and the rest.
2. **The worker**, which states the class of a step it wants to take in its
   `needs_me` note (`CLASS: 6 — apply staging-x, 2 create`), and in each
   `AUTO:` note.
3. **The coordinator**, which reads that line before answering a question or
   a prompt and may raise the class, never lower it.

Production versus non-production decides most of the upper ladder:

- **(R) a.** Config patterns matched against the target the worker passes
  (`--target <stack, workspace or environment>`):

  ```toml
  [autonomy]
  prod = ["*prod*", "*-production"]
  widespread = 5                       # deletes past this count, or in more than one stack, are class 9
  merge_class = { infra = 7 }          # repos whose main deploys on merge
  ```

  **A missing or unmatched target counts as production**, so a gap in the
  list costs a stop, not an unwanted prod apply.
- b. The worker's own judgement in its `CLASS:` line, with no patterns. The
  line between 6 and 7 would then be the agent's word.

### D4. Global level and per-bead override

```toml
[autonomy]
level = 2        # the global dial: every bead without its own (was `default`)
max = 9          # the ceiling no bead goes above (was `enabled`)
```

- A bead's own level is the label `autonomy:N`. Its **effective level** is
  `min(own level, or the global one if it has none, max, the stop-file cap)`.
- **(R) a.** A bead **may go above the global level**, up to `max`: "run this
  one bead to prod, keep the rest at 2". It may also go below it
  (`autonomy:1` on a sensitive bead).
- b. The global level is a ceiling, and a bead can only go lower.
- Coordinator actions about a bead (dispatching it, answering its question or
  its prompt) use that bead's effective level. The reporting mode uses the
  global level, since it is about TJ's attention, not one bead.

### D5. The default, and migrating today's settings

Today's supervised behavior doesn't fit one number. The coordinator already
answers prompts up to class 2 (read-only subagents, `rm` in the worktree,
commit and push to its own branch), but workers ask TJ about every decision,
even one that only shapes their own PR.

- **(R) a.** Default dial **2**. Workers then take their own recommendation on
  choices that only shape their PR, with an `AUTO:` note, because the PR
  still gets TJ's review before anything lands. A design question TJ wants to
  answer can still go to TJ: the worker raises the class if it isn't sure.
  - `autonomous` reads as `autonomy:7`, which is exactly what it allows
    today: decide, merge, self-install, rollout steps, and applies on any
    stack that pass `gate plan` with no delete or replace. Cumulatively, 7
    also allows dispatch and resume (3) and internal posts (5), which today's
    label doesn't. That is the one widening; the alternative is mapping it to
    6 and raising beads to 7 by hand.
  - `supervised` reads as `autonomy:2`. `[autonomy] enabled = false` reads as
    `max = 2`, `default = true` as `level = 7`. The per-kind switches
    (`decide`, `merge`, `apply`, `install`) go: the dial replaces them.
  - `autonomy off` caps every bead at 2, today's supervised behavior, not 0:
    it is the panic switch for autonomy, and shouldn't stop the coordinator
    answering the push prompts that keep workers moving. `autonomy level 0`
    is the explicit "only read-only runs without me".
  - One release reads the old labels and keys, and the ticker rewrites the
    labels with a note on each bead. `grant`/`revoke` stay as aliases for
    `level 7` and `level 2`.
- b. Default dial 1: workers keep asking about every decision, and the
  coordinator answers push prompts only because a coordinator rule says so,
  outside the dial.

### D6. CLI, the check, and the coordinator's rules

```
quartermaster autonomy level                # global level, max, cap, each active bead's effective level
quartermaster autonomy level N              # set the global level (a state file; wins over [autonomy] level)
quartermaster autonomy level N <bead>...    # set beads' own level: label autonomy:N, note, tell the worker
quartermaster autonomy level - <bead>...    # back to the global level
quartermaster autonomy check <bead> --class N
quartermaster autonomy check <bead> --action A [--target T]
quartermaster autonomy on|off               # off caps at 2 (D5)
```

- `check` passes when the action's class is at most the bead's effective
  level and the action isn't on the floor. `--action` looks the class up:
  `push` 2, `dispatch`/`resume`/`close`/`resolve`/`install` 3, `merge` 4
  (or the repo's `merge_class`), `post` 5, `apply`/`deploy`/`rollout` 6 or 7
  by `--target`, `delete` 8. `--class N` is for everything else, with the
  class from the worker's `CLASS:` line. An unknown action fails.
- **(R) a.** The commands that act on their own enforce it: `dispatch`,
  `resume`, `answer`, `stale --release` and the ticker's `auto_close` refuse
  without `--by TJ` when the check fails. The coordinator's guide (rendered in
  `context.go`) loses "wait for TJ" and "never answer a needs_me for TJ", and
  says instead: "before acting on TJ's behalf, find the class of what you'd
  authorize (the worker's `CLASS:` line, raised if you disagree) and run
  `autonomy check`; act and note it if it passes, else relay to TJ". The
  level isn't baked into the guide, which is read once per session; `context`
  and the checks are read every turn.
- b. Render the current level's allowed actions into the guide as a list.
  Easier to read, but stale as soon as the level changes.
- The hook-prompt categories TJ listed for the coordinator become examples of
  classes 0–2 and leave `coordinator_rules`. A prompt in no category is
  classified like any action and is answered only if it is inside the dial
  and not on the floor; a classifier denial is always the floor.
- Setting a bead's level sends its worker a notice tagged `[quartermaster
  autonomy: level N set by TJ]`, with the same delivery rules as today's
  grant and revoke. The worker trusts it only as far as `autonomy check`
  passes.

## Auditing

- Every step taken without TJ leaves `bd note <bead> "AUTO: [<actor> L<n>
  C<c>] <what> — <evidence>"`: who (`worker` or `coordinator`), the
  effective level, and the class, e.g. `AUTO: [coordinator L4 C4] answered
  'merge now' with the worker's recommendation` or `AUTO: [worker L7 C6]
  applied staging-x, 2 create / 0 delete (gate ok)`. The prefix stays
  `AUTO:`, so `autoNotes`, the digest and the report need no change.
- The commands that act (`dispatch`, `resume`, `answer`, `stale --release`,
  `level`, and the ticker's auto-close) write the note themselves when run
  without `--by TJ`, so the record doesn't depend on the agent remembering.
- Every level change is noted (per bead) or logged as an inbox event
  (global), with who asked for it.
- Linear comments stay at milestones: a decision, an apply, a merge.

## What does not change

- The trust model: labels and the state file are the grant, checks enforce
  it, ticker messages approve nothing.
- `gate plan`'s allowlist and `--expect`. It gains a printed class.
- Worker hosts: a host without the `quartermaster` binary fails every check,
  so its workers act at class 2 at most, whatever their label says.
- Muse workers follow the same ladder.

## Plan (after TJ decides)

1. Level model: `autonomy:N` labels, `level`/`max`/`prod`/`widespread`/
   `merge_class` config, the state file, the effective level, the action
   table and the floor, with tests for each class boundary and for migrating
   old labels and keys.
2. `gate plan` prints a class; `check --class/--action/--target`.
3. CLI: `autonomy level`, grant/revoke aliases, the level notice; the board's
   `U` and digit keys, and a level marker on each row instead of `auto`.
4. Enforcement in `dispatch`, `resume`, `answer`, `stale --release` and
   `auto_close`, with their `AUTO:` notes.
5. Coordinator guide and `context`'s dial and reporting line; the brief's
   grant section rewritten around `CLASS:` lines and the check.
6. README and the standing authorization in TJ's CLAUDE.md, rewritten from
   "a bead labeled autonomous" to "an action whose class `autonomy check`
   passes".
