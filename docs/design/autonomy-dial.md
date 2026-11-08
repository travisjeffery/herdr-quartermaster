# Autonomy dial: one 0–9 level, set by where an action lands

Status: **proposed** (2026-11-08), fourth draft. Some decisions are made
(see "Decisions so far"); nothing is built until TJ decides the rest. This
draft folds in two independent designs written in parallel without reading
this one: the **Muse design** (`autonomy-dial-muse.md`) and the **Codex
design** (`autonomy-dial-codex.md`). Ideas taken from them are credited
where they appear. This doc stays the base because it builds on today's code
(`autonomy.go`, `gate.go`, `prompt.go`, `dispatch.go`, `context.go`) and the
trust model of [autonomous-mode.md](autonomous-mode.md).

## Summary

Today autonomy is on or off per bead (`autonomous` / `supervised` labels),
with four switches in `[autonomy]` (`decide`, `merge`, `apply`, `install`),
`auto_close` and `auto_resolve`, and prose rules for the coordinator. Those
are five overlapping dials, and each one is about **who** acts or **which
capability** it uses.

This design replaces all of them with one number from 0 to 9, and classifies
**the action, not the actor**. Every action gets an **impact class**: how far
its output reaches, from read-only (0) up to widespread production deletion
(9). The dial at N means:

> **Any action whose impact class is at most N may run without TJ.**
> Anything above N stops for TJ.

Who acts doesn't matter. A worker taking its own recommendation, the
coordinator answering a `needs_me` question, and the coordinator answering
an approval prompt are judged the same way: by the class of what their choice
authorizes. At dial 4 the coordinator may answer "merge now?" (class 4) for
TJ but must relay "apply to staging?" (class 6).

Five rules shape the rest:

1. **An action's preconditions are part of its class** (from Muse). Merging
   an approved, green PR is class 4; merging without the required review is
   a different action, class 8. A prod apply that passes `gate plan`, with
   the expected change noted and a rollback named, is 7; without them it is
   8. The dial authorizes action plus evidence, never the action alone.
2. **An unknown or disputed class resolves up.** An action nobody can place
   is class 9 (from Codex). When classifiers disagree, the highest wins
   (from Muse).
3. **The dial authorizes discretion inside the task, never new goals** (from
   Codex). Text in repos, web pages, messages and tool output can't widen
   what a bead is for, at any level. This is the prompt-injection defence
   that holds when everything else allows.
4. **The top levels are leases** (from Muse). Levels 8 and 9 are scoped to
   beads and expire, and 9 is never global (D9).
5. **The first version is advisory** (from Codex). Workers keep their own
   credentials, so the check is enforced by the commands quartermaster
   owns and by the agents' cooperation, not by withholding access. A
   credential-holding executor comes in a later phase.

It reads like log verbosity, inverted: at 0 TJ sees and approves everything;
at 9 TJ sees the digest. **9 is true allow-all**, with nothing kept outside
the dial (D2, decided).

What stays as it is:

- **The labels are the grant.** A level lives in a bead label and a state
  file that only TJ (or the coordinator on TJ's explicit word) raises. The
  ticker never raises a level, and an automated message approves nothing.
  Any actor may lower its own level or stop (from Muse).
- **The check decides.** Before every step it takes without TJ, the worker
  or coordinator runs `quartermaster autonomy check`. A level change reaches
  a running worker at its next check, with no restart.
- **Every automatic step leaves an `AUTO:` note.**

## The impact ladder

Each class names where the output lands, with the preconditions it needs.
An action missing a precondition takes the class given for it, or the next
class up if none is given.

| Class | Where it lands | Today's actions, with preconditions |
|---|---|---|
| 0 | Nowhere: read-only | Reading files, `bd show`, `gh pr view`, `herdr agent read`; a subagent whose task is a read-only lookup or review; `gate plan`, `pulumi preview`, `tofu plan` |
| 1 | The agent's own worktree and scratch | Edits, local builds and tests, `git add`/`commit` in its own worktree, `rm` inside its worktree or a `mktemp -d` directory, `bd note` on its own bead |
| 2 | Its own branch and PR (unmerged, reviewed before it lands) | `git push` to its own branch, opening or updating its PR, replying on its PR's threads, `/plan`-style trigger comments (unless the trigger itself applies: then the class of the apply), retargeting a stacked PR, moving its own Linear issue's status. A choice that only shapes its own PR (a design option, naming, test approach) |
| 3 | TJ's machine and the shared bead state | Dispatching a bead TJ selected (`next`), resuming a thread, closing a `likely_stale` bead on strong evidence, releasing a stale claim, `resolve` of a merged worktree without `--force`, installing a merged herdr-quartermaster PR (pull, build, ticker restart). Creating beads for discovered work |
| 4 | The shared repo's main branch | Merging its own PR, or enabling auto-merge, **with the required review and checks green**; resolving review threads on its own PR after a fix or a reply |
| 5 | Internal messages | Posting to the team's internal channels (review requests, status), commenting on Linear issues or PRs other than its own |
| 6 | Non-production infrastructure | An apply to a dev or staging stack that passes `gate plan`; a deploy or rollout step to a non-prod environment; dispatching a bead TJ didn't select (Muse: choosing the queue spends TJ's attention) |
| 7 | Production, additive or in place | A prod apply that passes `gate plan` with no delete or replace, matches the change noted beforehand, and names its rollback; a prod deploy; the next step of a prod rollout once the previous one verified |
| 8 | Scoped deletion, and outside our systems | One delete or replace in a prod stack, or a stateful replace anywhere; deleting a non-prod database, bucket or volume; force-pushing to a shared branch; merging without the required review; **reading or writing a secret; a message to someone outside the team; spending money; approving a PR (D7)** |
| 9 | Widespread, irreversible, or control of the dial itself | Deletes across stacks or past the `widespread` threshold; deleting prod data; object lock or KMS key deletion; **answering a classifier denial and loosening agent settings or hooks (D7)**; raising its own level; disabling gates, checks or the journal; **any action whose impact is unknown** |

How an action is placed:

- **It takes the highest class among its effects**, including what it sets
  off. A merge into a repo whose main deploys to prod is 7, not 4
  (`merge_class`, D3). A trigger comment that runs an apply is the apply's
  class (from Codex).
- **A decision takes the class of what the chosen option does.** "Option 2:
  canary on staging first" is 6; a choice between two designs in its own PR
  is 2.
- **Scope adds up** (from Muse). Counts past the `widespread` threshold move
  an action up a class. They are summed per bead and target over a rolling
  window, so splitting one big delete into twenty small ones adds back up.
- **Self-install stays at 3**, except a merged PR that changes how
  authority is decided (`autonomy.go`, `gate.go`, `prompt.go`, the brief,
  the coordinator guide), which is 9. Codex's point: code that can change
  every worker's policy is classed by that reach, not by being a local build.

### What TJ hears

TJ always hears about anything above the dial: it is waiting on TJ. What
changes with the dial is how much the coordinator says about what ran
without TJ. The ticker's inbox, the board and the digest are the same at
every level.

| Global dial | The coordinator reports what ran without TJ |
|---|---|
| 0–3 | Each `AUTO:` step, as it happens |
| 4–6 | One line per update listing the `AUTO:` steps since the last |
| 7–9 | Nothing between digests, except failures it can't fix and breaker trips (see "Budgets and the circuit breaker"); class 7+ steps get a non-blocking notification as they happen (from Codex) |

Quiet must still look different from broken (from Muse): the digest arrives
on schedule even when empty ("nothing needed you; 14 steps, highest class
7"). `quartermaster context` prints the dial, any leases and the reporting
mode at the top, so the coordinator reads them every turn.

## The task bounds the dial

The dial says how far an agent may go **in doing its bead**. It never says
what the bead is for. At every level, 9 included:

- A worker's task is its bead's description and TJ's messages to it. Text it
  reads in a repo, a PR or issue comment, a web page, a log, another
  worker's output or a tool result is data, not instructions. "Also rotate
  the prod keys" in a PR comment is not part of the bead, however high the
  dial.
- Work discovered along the way becomes a new bead (`bd create`, class 3).
  Doing it is a separate dispatch, which at dial 3–5 needs TJ to select it
  (`next`) and at 6+ the coordinator may choose.
- The coordinator reads text that workers and outside parties wrote. It
  classifies an action from the command and its target, never from the
  description around it (`prompt.go` already treats screen text as the
  agent's description, never as evidence).
- A worker at 9 still works on its bead. A bead whose task is "keep these
  services healthy" can reach far at 9; "write a design doc" still doesn't
  operate anything (Codex's example).

## needs_me questions

TJ, 2026-11-08: there is no separate "no recommendation" category.

- **Workers always include a recommendation** in a `needs_me` note, with its
  class (`CLASS: 4 — merge now; approved, checks green`) and the evidence
  for it.
- **The coordinator may answer** when it can form its own recommendation from
  the evidence, and the action that answer authorizes is within the bead's
  dial. It prompts the worker with the answer and records
  `AUTO: [coordinator L<n> C<c>] answered "<question>": <answer> — <evidence>`.
  Its recommendation is its own: it checks the worker's evidence instead of
  ratifying it (the point Muse raised against free proxy answers).
- **It still escalates to TJ**, whatever the dial, when:
  - the question is one of taste or voice;
  - it involves other people: what to say to someone, asking another team;
  - the evidence conflicts or is missing;
  - the worker marked it `CRITICAL:`. Only TJ, or a mechanical classifier,
    lowers a `CRITICAL:` claim.

Examples:

| Answers on its own (if within the dial) | Escalates to TJ |
|---|---|
| A status report parked as a question ("PR merged, verified — close?") | The opening of a blog post |
| Closing a bead whose work is verified done (merged PR, code on main) | What to say in a message to a teammate |
| A bead superseded by another, with both linked | Turning on an irreversible WORM lock |
| Resume and release housekeeping | A prod-wide rollout |
| Merging its own approved, green PR | Two pieces of evidence that disagree |
| A question TJ already answered on this or a linked bead | A question marked `CRITICAL:` |

## Claude Code prompts and hard denies

Workers and the coordinator run inside an agent CLI (Claude Code, Codex,
Muse) with its own permission system. It stops an action in one of two ways:

1. **Prompts**: a hook that asks, a permission ask, an auto-mode classifier
   denial with a countdown. The agent waits on a menu that `quartermaster
   answer` can answer, so **the dial governs them**: an answer takes the
   class of the action it releases. Answering a classifier denial is class 9
   by itself (D7). Choosing a standing-permission option ("don't ask again",
   a mode switch) changes future policy, so it is classed as loosening
   settings (from Muse).
2. **Hard denies**: a settings deny rule, a hook that exits with a block, a
   sandbox refusal. No prompt is shown and there is nothing to answer.
   Lifting one means changing the CLI's settings or hooks, which is itself a
   ladder action: loosening is class 9, tightening is 1 or 3.

**The coordinator runs inside Claude Code too.** Its own classifier and hooks
judge `quartermaster answer` like any command, so they can stop it from
answering a prompt the dial allows. Then it relays the prompt to TJ. It
never works around its own permission system, and no agent answers the
coordinator's prompts for it (D8).

## Decisions for TJ

(R) is the recommendation. Decided items are in "Decisions so far".

### D1. The ladder

- **(R) a.** The ten classes above, with preconditions inside the class.
- b. The same ladder without class 3, folded into 1.
- c. A coarser ladder: local (0–1), own PR (2), shared repo and messages
  (3), non-prod (4), prod change (5–6), prod deletion (7–8), widespread (9).

### D3. How an action is classified

The class is the **highest** of three classifiers, so none can lower
another's call:

1. **Mechanical, where a tool can tell.** `gate plan` prints a class instead
   of only passing or failing: 6 or 7 by target, 8 for a delete or replace
   in prod or a stateful replace anywhere, 9 past `widespread`; a plan it
   can't parse, or a mismatch with `--expect`, can't be classified, so it
   stops. The action table (D6) gives the fixed class of `merge`, `dispatch`
   and the rest.
2. **The worker**, which states the class of a step in its `needs_me` note
   (`CLASS: 6 — apply staging-x, 2 create`) and in each `AUTO:` note.
3. **The coordinator**, which reads that line before it answers and may
   raise the class, never lower it.

- **(R) a.** Production by config patterns on the target the worker passes,
  **unmatched means production**:

  ```toml
  [autonomy]
  prod = ["*prod*", "*-production"]   # stacks, workspaces, environments
  widespread = 5                      # deletes past this, or in more than one stack, are class 9
  merge_class = { infra = 7 }         # repos whose main deploys on merge
  ```

- b. The worker's own judgement in its `CLASS:` line, without patterns.

### D4. Global level and per-bead override

```toml
[autonomy]
level = 4        # the global dial for beads without their own (was `default`)
max = 7          # the ceiling for standing settings (was `enabled`); 8–9 only by lease, D9
```

- A bead's own level is the label `autonomy:N`. Its **effective level** is
  `min(own level or the global one, max or its lease, the stop-file cap,
  the breaker cap)`.
- **(R) a.** A bead **may go above the global level**, up to `max`: "run this
  one bead to prod, keep the rest at 4". It may also go below it.
- b. The global level is a ceiling, and beads only go lower.
- Coordinator actions about a bead (dispatching it, answering its question
  or prompt) use that bead's effective level. Reporting uses the global one.

### D5. The default, and migrating today's settings

Today every worker's brief turns on auto-merge, a class 4 step whose
preconditions GitHub holds, and the coordinator answers prompts up to class
2. So today's default is closer to 4 than 2 (Muse's reading).

- **(R) a.** Default dial **4**. Workers keep enabling auto-merge, and also
  take their own recommendation on choices up to class 4. The coordinator
  may dispatch `next` beads, resume threads and close verified-done beads
  (3), which today it proposes and waits on.
- b. Default dial 2: workers stop enabling auto-merge on their own until
  raised to 4.
- Migration, for either default:
  - **(R)** `autonomous` reads as `autonomy:7`: decide, merge, self-install,
    rollout steps and applies on any stack that pass `gate plan`. Muse
    reached the same number. Because levels stack, 7 also lets those beads
    post internally (5), which today's label doesn't. Alternative (Codex):
    keep labelled beads on the old rules until TJ picks each one's level,
    with a preview command showing what each level adds and removes.
  - `supervised` reads as `autonomy:2`. `[autonomy] enabled = false` reads as
    `max = 2`, `default = true` as `level = 7`. The per-kind switches and
    `auto_close`/`auto_resolve` become "the ticker may do this when the
    global level is at least 3" (Muse: no booleans left to contradict the
    dial).
  - **(R)** `autonomy off` caps every bead at 2, so workers can still push
    their work and stop cleanly. Alternative (Muse): cap at 1, so nothing
    leaves the machine. Either way `off` stays a file in the state dir that
    TJ can create by hand.
  - One release reads the old labels and keys; `grant`/`revoke` stay as
    aliases for `level 7` and `level 2`.

### D6. CLI, the check, and enforcement

```
quartermaster autonomy level                       # global, max, leases, caps, each active bead's effective level; says "advisory"
quartermaster autonomy level N                     # set the global level (state file; wins over [autonomy] level)
quartermaster autonomy level N <bead>... [--for D] # set beads' own level; 8 and 9 need --for (D9)
quartermaster autonomy level - <bead>...           # back to the global level
quartermaster autonomy check <bead> --class N [--what "..."]
quartermaster autonomy check <bead> --action A [--target T]
quartermaster autonomy on|off
```

- `check` passes when the action's class is at most the bead's effective
  level and its budget isn't spent. `--action` looks the class up: `push` 2,
  `dispatch`/`resume`/`close`/`resolve`/`install` 3, `merge` 4 (or the repo's
  `merge_class`), `post` 5, `apply`/`deploy`/`rollout` 6 or 7 by `--target`,
  `delete`, `secret`, `message-external`, `spend` and `approve` 8,
  `classifier` and `loosen-settings` 9. `--class N` covers the rest, from
  the worker's `CLASS:` line. An unknown action fails.
- **(R) a.** Commands quartermaster owns enforce it: `dispatch`, `resume`,
  `answer`, `stale --release` and the ticker's `auto_close` refuse without
  `--by TJ` when the check fails. The coordinator's guide loses "wait for TJ"
  and says: "before acting for TJ, find the class of what you'd authorize and
  run `autonomy check`; act and note it if it passes, else relay". The level
  isn't baked into the guide; `context` and the checks are read every turn.
- b. Render the current level's allowed actions into the guide as a list.

**v1 is advisory** (from Codex). Workers hold their own credentials (`gh`,
cloud roles), so a worker that skips the check can still act. v1 enforces
what quartermaster owns (the commands above, prompt answers, launch modes)
and relies on the check for the rest. `autonomy level` and the board say
"advisory" until a later phase moves side-effecting credentials out of the
agents into an executor that checks before it acts (see Plan). `--by TJ` is a
label, not proof of identity, until then.

### D7. Where the remaining top-of-ladder items sit

Decided: messages outside the team and spending money at 8. Still open:

- **Approving a PR:**
  - **(R) a.** 8. Approving removes the human-review precondition that makes
    a merge class 4, so it is classed by the gate it removes (the
    coordinator's pick, and Muse's).
  - b. 4, plus whatever the merge it unlocks reaches (Codex: an ordinary
    review decision, made under the agent's own name).
  - c. 6.
- **Answering a classifier denial, and loosening agent settings or hooks:**
  - **(R) a.** 9. Each switches off an independent safeguard, so only
    allow-all includes it.
  - b. 8 (Muse), with loosening settings at 9 (Codex) only when it changes
    how authority is decided.

### D8. Launch permission modes follow the level

Answering prompts one by one is slow, and the coordinator's own classifier
can refuse to answer. The worker's launch mode can follow its level instead.
A mode is fixed when the agent starts, so prompts remain the revocable part
(Muse).

- **(R) a.** Workers launch **one band below** the dial: with their kind's
  configured args at every level below 9, as today, and with the kind's
  `bypass_args` only under a 9 lease. The checks and prompts cover the rest,
  and stay revocable:

  ```toml
  [agents.claude]
  args = ["--permission-mode", "auto"]               # every level, as today
  bypass_args = ["--dangerously-skip-permissions"]   # only under a 9 lease

  [agents.codex]
  bypass_args = ["--dangerously-bypass-approvals-and-sandbox"]
  ```

  A level change across 9 takes effect when the ticker restarts the worker at
  its next idle moment, through `resume`, which keeps the conversation. A
  worker lowered from 9, or whose lease expires, is restarted first, before
  anything else is sent to it. **The coordinator is never launched with
  bypass**, so a prompt TJ can see always remains somewhere (Muse).
- b. Launch at the dial's band, the coordinator included: fewer prompts, but
  lowering the dial doesn't reach a bypass worker until it restarts.
- c. Leave launch modes alone; 9 is then allow-all only as far as the
  coordinator's own classifier lets it answer.

### D9. Leases at the top

Risk at 8 and 9 grows with time left on (Muse).

- **(R) a.** Setting a level of 8 needs `--for <duration>` (default 4h); 9 is
  set only on beads, only with `--for`, and **never globally**. At expiry the
  ticker sets the bead back to its previous level, notes it and notifies TJ,
  so expiry doesn't depend on the worker.
- b. 8 and 9 are ordinary standing settings, global included.

## Auditing

- Every step taken without TJ leaves `bd note <bead> "AUTO: [<actor> L<n>
  C<c>] <what> — <evidence>"`: who (`worker` or `coordinator`), the
  effective level and the class. The prefix stays `AUTO:`, so `autoNotes`,
  the digest and the report keep working. For class 6 and up the note is
  written before the step, so a crash mid-step leaves the intent on record
  (from Muse).
- The commands that act (`dispatch`, `resume`, `answer`, `stale --release`,
  `level`, the ticker's auto-close) write the note themselves when run
  without `--by TJ`.
- **Journal** (from Muse). Bead notes can be edited by the agents being
  audited and carry no timestamps. The ticker appends every `AUTO:` record it
  sees, every prompt answer, level change, lease event and breaker trip to an
  append-only `journal.jsonl` in the state dir, with its own timestamp. An
  agent on the same machine can still rewrite it; that is stated, and
  shipping it off the machine is out of scope.
- **Digest by class.** The digest and `quartermaster report` group `AUTO:`
  steps by class, highest first: 7 and up each with its evidence, 4–6 one
  line each, 3 and below as counts per bead.
- Every level change is noted (per bead) or journaled (global), with who
  asked.
- Linear comments stay at milestones: a decision, an apply, a merge.

## Budgets and the circuit breaker

From Muse, against loops: a worker retrying an apply, a rollout advancing
itself overnight, a dispatch storm.

- **Budgets** per bead per rolling hour, enforced in `check`:

  ```toml
  [autonomy.budget]
  steps = 30       # AUTO steps of any class
  applies = 3      # class 6+
  dispatches = 5   # coordinator dispatches, global
  ```

  Over budget, `check` fails with "budget" as the reason, and the bead goes to
  TJ like any other out-of-level step.
- **Circuit breaker.** The ticker caps a bead at 2 and raises an inbox item
  when it sees N failed checks or applies in a row, or the same `AUTO:` step
  repeated. A breaker only lowers; TJ restores the level.

## What does not change

- The trust model: labels and the state file are the grant, checks enforce
  it, ticker messages approve nothing, only TJ raises a level.
- `gate plan`'s allowlist and `--expect`. It gains a printed class.
- Worker hosts: a host without the `quartermaster` binary fails every check,
  so its workers act at 2 at most, whatever their label says. That is a host
  cap, shown in `autonomy level` (Muse).
- Muse and Codex workers follow the same ladder.

## Plan (after TJ decides)

v1, advisory:

1. Level model: `autonomy:N` labels, `level`/`max`/`prod`/`widespread`/
   `merge_class`/budget config, the state file, leases, the effective level,
   the action table; tests for each class boundary, preconditions, leases and
   migration.
2. `gate plan` prints a class; `check --class/--action/--target`, budgets.
3. CLI: `autonomy level`, grant/revoke aliases, the level notice; the board's
   `U` and digit keys and a level marker on each row instead of `auto`.
4. `bypass_args` per agent kind under 9 leases, and the restart when a level
   crosses 9 or a lease expires.
5. Enforcement in `dispatch`, `resume`, `answer`, `stale --release` and
   `auto_close`, with their `AUTO:` notes; journal; breaker; digest by class.
6. Coordinator guide (needs_me rule, task bounds) and `context`'s dial
   line; the brief rewritten around `CLASS:` lines, recommendations and the
   check.
7. README and the standing authorization in TJ's CLAUDE.md, rewritten from
   "a bead labeled autonomous" to "an action whose class `autonomy check`
   passes".

Later, enforced (from Codex): a local policy service and a credential-holding
executor. Workers propose; the executor holds `gh` and cloud credentials,
rechecks the class and preconditions right before it acts (same PR head, same
plan digest, current policy revision), and journals the outcome. Until it
covers every path a worker could use (shell, MCP connectors, browser), the
install stays labelled advisory.

## Decisions so far

- **D2** (TJ, 2026-11-08): 9 is true allow-all, with no floor. The former
  floor items sit at the top of the ladder. Claude Code prompts, classifier
  denials included, are governed by the dial through `quartermaster answer`;
  hard denies are lifted only by changing settings or hooks, itself a ladder
  action; the coordinator runs in Claude Code too and its own permission
  system can still stop it. The level sets launch permission modes (D8).
- **needs_me** (TJ, 2026-11-08): no "no recommendation" category; workers
  always recommend; the coordinator answers on its own evidence within the
  dial and escalates taste, people, conflicting or missing evidence, and
  `CRITICAL:` (see "needs_me questions").
- **D7, in part** (TJ, 2026-11-08): messages outside the team and spending
  money at 8.
- **Folded in** (TJ, 2026-11-08): the Muse and Codex designs, as credited
  above.
