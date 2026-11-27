# End-to-end delivery loop: design → ping-pong review → merge → chained rollout

Quartermaster supports a complete, bounded autonomous delivery loop that connects
implementation, skeptical pre-PR or new-head review, pull request landing, and
post-merge rollout verification:

```text
design (bead description + ## Acceptance)
  │
  ▼
implement (role:implementer, git worktree)
  │  ▲
  │  │  quartermaster tell (blocking findings only; capped at max_rounds)
  ▼  │
skeptical reviewer (role:reviewer, worktree-free, head-SHA tracked)
  │  verdict: can_merge (or escalate to needs_me at max_rounds)
  ▼
PR / CI / required review / merge → implementer bead closes
  │
  ▼  unblocked auto-dispatch (gated by [autonomy] target class)
chained rollout (role:rollout + rollout-target:<target>, worktree-free)
  │  watch build/deploy runs → gate plan → live verify → quartermaster close
  ▼
verified done
```

Every transition is durable, idempotent, deduplicated, and bounded by the
autonomy dial.

## Stage 1: Design and acceptance criteria

Start with an implementer bead whose description states the design, scope
boundaries (for parallel lanes), and 2–4 checkable bullets under
`## Acceptance`:

```sh
bd create "Add token-bucket rate limiter" -t task -p 2 -d "$(cat <<'EOF'
Design: enforce per-tenant token bucket in the API gateway middleware.

## Acceptance
- Exceeding burst limit returns HTTP 429 with a Retry-After header
- Concurrent requests pass `go test -race ./...`
- Deployed and verified healthy on `dev`
EOF
)"
```

To enable automatic bounded pre-PR and new-head implementer ↔ reviewer
ping-pong whenever the implementer settles on a new clean head commit (pre-PR
or on an open PR), label the bead:

```sh
bd label add task-10 review:required
```

## Stage 2: Bounded implementer ↔ reviewer ping-pong (`quartermaster review`)

Instead of pushing straight to GitHub review without a skeptical pass against
the bead's design and acceptance criteria, run a bounded review loop using a
worktree-free `--role reviewer` (or path-escalated security-review) worker.

### Automatic trigger and risk-path role routing (`[review]`)

Configure optional risk-path routing in `~/.config/quartermaster/config.toml`:

```toml
[roles.security-review]
worktree = false
brief = "reviewer"
instructions = "Audit auth, IAM, workflow, and schema migration diffs for security and access-control regressions."

[review]
auto_trigger = true            # default true; set false to emit review_needed without auto-spawning
security_role = "security-review"  # default; falls back to "reviewer" if the role is not configured
security_paths = [
  "**/auth/**",
  "**/iam/**",
  "**/.github/workflows/**",
  "**/migrations/**",
]
```

How automatic triggering and routing work:

- **Automatic pre-PR & new-head trigger**: when an active `role:implementer`
  bead labelled `review:required` reaches a new clean head commit (either an
  open PR head or an at-rest clean worktree with commits ahead of `base_branch`)
  while `current_round < max_rounds`, the ticker automatically invokes
  `quartermaster review request <bead> --head <sha> --spawn --by ticker`, gated
  by autonomy Class 3 (`dispatch`/`wake`), rolling-hour budgets, and head-SHA
  deduplication. If auto-trigger is skipped or fails (for example below Class 3,
  over budget, held, or `auto_trigger = false`), Quartermaster emits one
  deduplicated `review_needed` inbox event instead.
- **Headless `inbox-runner` auto-trigger**: when a `review_needed` inbox item
  for an active `review:required` implementer bead is processed by
  `inbox-runner`, it invokes `quartermaster review request <bead> --head <sha>
  --spawn --by inbox-runner` deterministically (subject to Class 3 autonomy,
  budget, and head-SHA dedup) and acknowledges the inbox file without spending a
  headless LLM turn.
- **Path-based security-review selection**: `review request --spawn` inspects
  changed files with `git diff --name-only <base>...<head>`. If any changed path
  matches `[review] security_paths` (supporting `**` across directories and
  `path.Match` segment wildcards) and `security_role` exists in config,
  Quartermaster spawns and dispatches a `role:<security_role>` sibling bead;
  otherwise it falls back cleanly to `role:reviewer`. If a later ping-pong round
  introduces changes matching `security_paths`, Quartermaster upgrades from a
  plain `role:reviewer` to `role:<security_role>`.

### Requesting a review round manually

```sh
# Resolve HEAD from the open PR or clean local worktree (or pass --head <40-sha>),
# spawn a worktree-free sibling role:reviewer bead, and start round 1/2:
quartermaster review request task-10 --spawn

# Reuse an existing reviewer bead or customize the round cap (1..5, default 2):
quartermaster review request task-10 --head <40-char-sha> --reviewer task-rev --max-rounds 3
```

What `review request` guarantees:

- **Durable per-bead state** in `<state>/review-loops/<bead>.json` (`0700` dir,
  `0600` atomic writes, exclusive per-bead `flock`), mirrored to `REVIEW REQUEST:`
  notes and `journal.jsonl`.
- **Head-SHA deduplication**:
  - Requesting a head that is already `pending` or already passed (`can_merge`)
    is an idempotent no-op.
  - Requesting the exact same head that previously received `cannot_merge` is
    refused until the implementer commits or pushes a new head SHA.
- **No parent-child deadlock**: spawned reviewer beads share the implementer's
  parent (if any) rather than parenting under the implementer bead, so they
  never hold the implementer out of `merged` state.

### Recording structured verdicts

The reviewer inspects the exact head commit SHA against the implementer bead's
description, design notes, and `## Acceptance` criteria, then records a
structured verdict:

```sh
# Blocking defect found (routes findings back to implementer via `quartermaster tell`):
quartermaster review verdict task-10 \
  --head <40-char-sha> \
  --verdict cannot_merge \
  --summary "Map race and missing Retry-After header" \
  --finding "middleware/limit.go:42: unsynchronized map write — panics under concurrent traffic" \
  --finding "middleware/limit.go:78: 429 omits Retry-After — violates acceptance criterion" \
  --later "extract helper constant for default window"

# Ready for PR / human review:
quartermaster review verdict task-10 \
  --head <40-char-sha> \
  --verdict can_merge \
  --summary "All blockers resolved; unit and race tests pass"

# Or submit structured JSON via --file <path|->:
quartermaster review verdict task-10 --file verdict.json --json
```

Verdict safety invariants:

- **Concrete blockers only**: `cannot_merge` requires at least one `--finding`;
  optional polish belongs under `--later`.
- **Stale-head protection**: a verdict must match the pending round's exact
  40-character head SHA.
- **Automatic implementer routing**: when `verdict == cannot_merge` and
  `current_round < max_rounds`, Quartermaster routes the numbered findings back
  to the implementer via `quartermaster tell` (or emits a `review_feedback`
  inbox event if the implementer worker is offline).
- **Hard round cap and `needs_me` escalation**: when `verdict == cannot_merge`
  at `current_round >= max_rounds`, Quartermaster stops routing back to the
  implementer, records `REVIEW ESCALATED:`, appends a structured
  `QUESTION:` / `OPTIONS:` / `CLASS: 3` note, and sets the implementer bead to
  `needs_me`. Automated callers cannot reset the cap; only explicit human action
  (`quartermaster review request <bead> --reset --by TJ`) resets it.
- **Automatic sibling reviewer cleanup on `can_merge`**: when
  `quartermaster review verdict <bead> --verdict can_merge` records a passing
  verdict for a spawned sibling reviewer bead, Quartermaster automatically
  closes the sibling reviewer bead with the verdict summary (gated by autonomy
  Class 3) and resolves its role directory or worktree — never touching the
  implementer bead or manually supplied `--reviewer` beads. If the reviewer
  agent invoked `review verdict` mid-turn (still working), resolution is
  deferred and completed automatically on the next ticker pass once the agent
  goes idle.
- **New-head wakeups**: on each tick, if an implementer bead in
  `changes_requested` (or labelled `review:required`) advances to a new clean
  head SHA while under `max_rounds`, `review:required` beads automatically start
  the next bounded review round (falling back to one deduplicated
  `review_needed` inbox event if gated/disabled), and non-`review:required`
  active loops emit one deduplicated `review_needed` inbox event for coordinator
  or `inbox-runner` handling without looping.

Inspect review-loop state at any time:

```sh
quartermaster review status
quartermaster review status task-10 --json
```

Active review loops also appear on threads in `quartermaster context` (text and
`--json`).

## Stage 3: Auto-chained rollout-to-target (`quartermaster rollout`)

Most changes are not truly finished when a PR merges — they need to build,
deploy to a target environment (such as `dev`, `latest`, or `staging`), and pass
live verification. Quartermaster makes the two-stage `implement → rollout` chain
first-class.

### Attaching a chained rollout bead

Attach a dependent worktree-free `role:rollout` bead either at dispatch time or
later via `quartermaster rollout chain`:

```sh
# Option 1: chain automatically when dispatching the implementer
quartermaster dispatch task-10 --rollout-target dev

# Option 2: attach (or idempotently reuse) a chained rollout bead anytime
quartermaster rollout chain task-10 --target latest
quartermaster rollout chain task-10 --target staging \
  --title "Roll out rate limiter to staging" \
  --acceptance "Staging deploy workflow completes green" \
  --acceptance "Gateway returns 429 with Retry-After on burst probe"

# Inspect chained rollout beads and whether the autonomy dial permits auto-dispatch
quartermaster rollout status
quartermaster rollout status task-10 --json
```

How the chain is structured:

- Labels: `role:rollout`, `rollout-target:<target>`, and the implementer's
  `repo:<name>` label (if present).
- Dependency: `blocked-by:<impl-bead>` (`blocks` edge).
- Parenting: shares the implementer bead's parent (sibling under the same epic
  or standalone) — **never** parented under the implementer bead itself, so the
  implementer bead has zero open children, moves cleanly to `merged` when its PR
  merges, and closes.
- Idempotent: calling `rollout chain` or `dispatch --rollout-target` again for
  the same `(impl-bead, target)` reuses the existing non-closed rollout bead.

### Autonomy dial gating by target class

Configure `[autonomy]` patterns in `~/.config/quartermaster/config.toml`:

```toml
[autonomy]
level = 5
max = 7
prod = ["*prod*", "*production*"]
dev = ["dev*", "latest"]
```

Dispatching a targeted `role:rollout` bead (via ticker auto-dispatch on unblock,
manual coordinator `quartermaster dispatch`, or headless `inbox-runner`) enforces
`max(3, targetClass(target))` plus `[autonomy.budget]`:

| Target example | `[autonomy]` match | Effective dispatch class | Auto-dispatches at L5? |
| --- | --- | --- | --- |
| `dev`, `dev-east`, `latest` | Matches `dev` (and not `prod`, with `prod` set) | **Class 5** | Yes (L5+) |
| `staging`, `qa` | Matches neither `prod` nor `dev` (with `prod` set) | **Class 6** | No — surfaces `unblocked` inbox item (auto-dispatches at L6+) |
| `prod`, `app/prod` | Matches `prod` (or `prod` is unset) | **Class 7** | No — surfaces `unblocked` inbox item (auto-dispatches at L7+ or `--by TJ`) |

### Unblocked auto-dispatch and `inbox-runner` coordination

When the implementer bead closes (or all remaining `blocks` dependencies and
referenced upstream PRs clear):

1. **Ticker auto-dispatch**: on the next tick, `blockerWakeupPass` verifies that
   the rollout bead is still `open` (live re-read), all blockers are cleared,
   agent discovery is complete, no live worker exists, and the bead is not held
   or headless-owned. If the target's autonomy class and hourly budget pass, the
   ticker notes the upstream cleared blocker / merged PR URL(s) on the rollout
   bead, dispatches the worktree-free `role:rollout` worker, records an
   `AUTO: [ticker L<n> C<c>]` note and journal entries, and marks the transition
   in `state.json` so ticker restarts never double-dispatch.
2. **Above-dial fallback**: if the target's class exceeds the bead's autonomy
   level (for example `staging` C6 or `prod` C7 at L5), the ticker does not
   dispatch; it emits an `unblocked` inbox event (`Clears.Kind =
   "unblocked_dispatch"`).
3. **Headless `inbox-runner` & idle coordinator coordination**:
   - `inbox-runner` preflight skips `unblocked_dispatch` items whose bead is
     already running (`already_dispatched`) or whose target class exceeds the
     bead's level (`rollout_above_level`), avoiding duplicate dispatches and
     wasted headless turns.
   - Once any actor dispatches or closes the rollout bead, `reconcileInbox`
     automatically archives stale `unblocked_dispatch` inbox items on the next
     tick.
   - `rolloutBrief` includes the explicit `rollout-target:<target>` and
     `quartermaster autonomy check <id> --action rollout --target <target>`
     instructions. The rollout worker labels the bead `rolling-out`, tracks
     post-merge Actions run URLs, validates IaC previews via `quartermaster gate
     plan --target <target>`, verifies live health against `## Acceptance`, and
     closes via `quartermaster close` (which runs the read-only verifier and
     removes `rolling-out` on pass).
