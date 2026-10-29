# Autonomous mode: carry a bead to done without TJ, except critical operations

Status: **draft** (2026-10-28), waiting on TJ's decisions D1–D5 below. Nothing
here changes behavior yet. Linear: INF-2126.

## Summary

On INF-2119, TJ was the gate at almost every step: needs_me questions the
worker already had a recommendation for, "go step N" between staged rollout
steps whose checks had passed, merges, and clean provision applies. Autonomous
mode lets a worker on an opted-in bead **go ahead on its own recommendation**
at those points and leave an audit line each time. It still **stops for
critical operations**, always.

The main design choice: **the worker decides, not the ticker or the
coordinator.** Today a ticker message "approves nothing", and that stays true.
Autonomy is a standing grant written into the worker's brief at dispatch,
which TJ starts. The worker reads the grant, checks it before each step it
takes on its own, and records what it did. No automated message ever acts as
TJ's answer, so none can be spoofed into one.

## What gates exist, and which this can remove

| Gate | Owner | In scope? |
|---|---|---|
| needs_me where the worker has a recommendation | quartermaster rules (brief) | **Yes**: decide, note, carry on |
| "go step N" in a staged rollout | quartermaster rules | **Yes**: go when the previous step verified |
| Merge once approved and green | quartermaster rules + GitHub | **Yes**: auto-merge exists; add stacked PRs |
| Provision/deploy apply | quartermaster rules | **Yes**, only when the preview passes the critical check |
| `/plan-…` comment triggers on TJ's own PRs | quartermaster rules | **Yes** (a comment on our own PR) |
| Required human review (org ruleset) | org policy | **No.** Never `/sneaky-approve` or bot self-approval. Radar approve mode (INF-2114) is the legitimate route |
| Claude Code permission prompts, auto-mode classifier | Claude Code | **No.** Never answered or bypassed; see "Standing authorization" |
| TJ-only RUN commands (SSO/browser login, secret reads/writes) | TJ | **No**: still `RUN:` + needs_me |

**Muse (`--agent muse`).** The bead asked whether Muse workers avoid the
classifier. Choosing an agent kind *because* it lacks a safety gate is a way
around that gate, so autonomous mode will not do it. Muse stays a normal
worker kind, and the critical-op rules below apply to it in the same way.

## Critical operations: always stop

These always go to needs_me, autonomous or not:

1. Deletes and destroys: a Pulumi/Tofu `delete`, or a `replace` of a stateful
   resource; deleting data, buckets, tables, secrets, or Kubernetes PVs and
   namespaces; revoking credentials; force-pushing to or deleting a shared branch.
2. Irreversible operations (WORM/object-lock, KMS key deletion scheduling,
   public disclosure).
3. Money: purchases, plan or quota upgrades, reserved capacity.
4. Messages to people outside the team (Slack outside the infra channels,
   email, customer-facing comments).
5. Anything Claude Code blocks or prompts for.
6. Anything the worker is unsure about. **Unsure means critical.**

## Decisions for TJ

Each has a recommendation (R). Answer by number, e.g. "D1 R, D2 b, …".

### D1. Where the policy lives (config shape)

- **(R) a.** Two layers. Opt-in is per bead (label `autonomous`), and a global
  setting decides what that label may do:

  ```toml
  [autonomy]
  enabled = true              # global kill switch; false makes the label inert
  default = false             # true = every bead is autonomous unless labeled `supervised`
  merge = true                # merge own PRs once approved + required checks green
  apply = true                # run provision/deploy applies that pass the critical check
  decide = true               # answer own needs_me questions that have a recommendation
  ```

  Dispatch writes the grant into the brief only when the bead is autonomous.
- b. Global only (`autonomy = true`), with no per-bead label.
- c. Per bead only, with no config section.

### D2. How "critical" is detected

- **(R) a.** Check the plan mechanically, and use the worker's judgement for
  everything that has no plan:
  - Infra applies: before any apply, the worker runs
    `quartermaster gate plan <file>` on the machine-readable preview (Pulumi
    `preview --json`, `tofu show -json plan`). It exits nonzero if any step is a
    `delete`, a `replace`, or a `create-replacement`/`delete-replaced` on a type
    in a stateful list (databases, buckets, volumes, secrets, KMS, DNS zones,
    clusters, queues), or if the plan does not parse. If the plan does not
    parse, the check fails. The stateful list lives in code with a config
    override.
  - Expected change: the worker also states the change it expects (resource
    count and types) in a note before the preview, and stops if the preview
    differs.
  - Everything else (needs_me questions, rollout steps): the worker tags a
    stop as `CRITICAL: <category>` in its needs_me note. Without that tag, an
    autonomous worker may decide on its own recommendation. The tag is the
    worker's call under the list above. There is no keyword scan of notes,
    because a missed keyword would let a critical operation through.
- b. The worker's judgement only, with no plan parser.
- c. A keyword scan of needs_me notes ("delete", "destroy", "drop", …) as an
  extra check on top of (a).

### D3. Audit trail

- **(R) a.** Every step the worker takes on its own leaves three records:
  1. `bd note <id> "AUTO: <what> — <evidence>"`, for example
     `AUTO: chose option 2 (canary first), recommended; alternatives 1,3` or
     `AUTO: applied stack prod-x, preview 3 update / 0 delete (gate ok)`.
  2. A Linear comment for milestones only (a decision, an apply, a merge), not
     every step, which matches the existing Linear convention.
  3. A line in the hourly digest and in `quartermaster report`, listing each
     bead's AUTO notes since the last digest, so TJ can review them in one place.
- b. bd notes only.

### D4. Kill switch

- **(R) a.** Three levels, and the worker checks them before every autonomous step:
  - Global: `quartermaster autonomy off` writes a stop file in the state dir.
    `[autonomy].enabled = false` does the same and lasts across restarts.
  - Per bead: `bd label remove <id> autonomous`, or add `supervised`.
  - Check: `quartermaster autonomy check <id>` exits 0 only if autonomy is on
    for that bead right now. The brief says to run it before each autonomous
    step, and to stop to needs_me if it fails. A worker that is already running
    picks up the switch at its next step, with no restart.
  - The ticker also shows an `auto` marker on the bead's sidebar/board row, so
    TJ can see which threads run without TJ.
- b. Global switch only.

### D5. Stacked PRs and merging

- **(R) a.** The worker keeps using `gh pr merge --auto --squash`. For a stack,
  once the lower PR merges, the worker retargets the next PR to the base
  branch (`gh pr edit --base`), and closes and reopens it if checks don't
  fire on their own. Auto-merge then takes it through the same required review
  and checks. If a ruleset blocks auto-merge, it uses
  `gh pr merge --squash --match-head-commit <sha>` after the ticker reports
  "ready to merge", so it can't merge a head commit nobody reviewed.
- b. Leave stacks to TJ.

## Standing authorization TJ can add to CLAUDE.md

Claude Code's permission prompts and the auto-mode classifier judge each action
against what the user has authorized. Quartermaster can't and won't answer
them. What TJ *can* do is state the standing authorization explicitly, so that
routine autonomous steps on opted-in beads are clearly within what TJ asked
for. Proposed wording for `~/.claude/CLAUDE.md`:

> ## Autonomous beads
>
> A bead labeled `autonomous`, with `quartermaster autonomy check <id>`
> passing, authorizes its worker to do these without asking me: choose its own
> recommended option on a decision and record it as an `AUTO:` note; run the
> next step of a staged rollout once the previous step's verification passed;
> merge its own PR through auto-merge (or with `--match-head-commit` once it is
> approved and required checks are green), including retargeting stacked PRs;
> post `/plan`-style trigger comments on my PRs; and run provision/deploy
> applies whose preview passes `quartermaster gate plan` and matches the change
> it noted beforehand.
>
> This never covers: deleting or destroying anything, replacing stateful
> resources, irreversible operations, spending money, messaging people
> outside the team, approving PRs (including any bot approval), reading or
> writing secrets, or anything Claude Code blocks. For those, stop and set the
> bead to needs_me.

## Plan, once D1–D5 are answered

1. `[autonomy]` config, the `autonomous` and `supervised` labels,
   `quartermaster autonomy on|off|check`, and the grant in the brief (dispatch.go).
2. `quartermaster gate plan` for the Pulumi and Tofu JSON formats, with fixtures
   and tests for each refusal case.
3. AUTO notes in the digest and report, and the `auto` marker on the sidebar
   and board.
4. Coordinator rules: don't relay a needs_me from an autonomous bead unless it
   is tagged `CRITICAL:` or the worker has no recommendation.
