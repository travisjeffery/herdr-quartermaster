# Multi-model planning council (`quartermaster council`)

Quartermaster includes a generic multi-model planning council primitive that runs
multiple independent, strictly read-only planning roles in parallel and then
automatically fans in to a blind, fact-checking Chair role to synthesize one
canonical implementation plan and file-disjoint child-bead DAG.

```text
target bead (open or in_progress)
  │  quartermaster council start <bead> --panel <name>
  ├────────────────────┬────────────────────┐
  ▼                    ▼                    ▼
planner-a            planner-b            planner-c   (worktree-free, read-only, zero cross-talk)
  │                    │                    │
  │  quartermaster council submit <bead> --member <role> --file -
  └────────────────────┴────────────────────┘
                       │  ticker fan-in (all submitted OR >= min_quorum at deadline)
                       ▼  anonymize as shuffled Plan A, Plan B, Plan C
                planner-chair                         (worktree-free, blind, live repo fact-check)
                       │
                       │  quartermaster council submit <bead> --chair --file -
                       ▼
  synthesis.md + post-scoring Attribution Table
  + target bead summary note + council_done inbox event
  + automatic helper bead close & resolve (PendingResolveBeads drain)
```

## Configuration (`[councils.<name>]`)

Define planning roles under `[roles.<name>]` (see [docs/roles.md](roles.md)) and
compose them into named council panels under `[councils.<name>]` in
`~/.config/quartermaster/config.toml`:

```toml
[roles.planner-a]
worktree = false
agent = "pi"
model = "provider/model-a"
thinking = "high"
brief = "investigator"
exclude_tools = ["edit", "write"]

[roles.planner-b]
worktree = false
agent = "pi"
model = "provider/model-b"
thinking = "high"
brief = "investigator"
exclude_tools = ["edit", "write"]

[roles.planner-chair]
worktree = false
agent = "pi"
model = "provider/model-chair"
thinking = "high"
brief = "investigator"
exclude_tools = ["edit", "write"]

[councils.default]
members = ["planner-a", "planner-b", "planner-chair"]
chair = "planner-chair"
anonymize = true
min_quorum = 2
timeout_minutes = 20
auto_cleanup = true
```

### Fields

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `members` | `[]string` | required | At least 2 unique role names referencing built-in (`implementer`, `reviewer`, `investigator`, `rollout`) or configured `[roles.<name>]` presets. |
| `chair` | `string` | required | Built-in or configured `[roles.<name>]` preset that fact-checks and synthesizes submitted member plans. The chair role may also participate as a member (it runs in a fresh isolated session and evaluates shuffled blind plans). |
| `anonymize` | `bool` | `true` | Withholds all member role names, model names, agent kinds, and member helper bead IDs from Chair inputs, shuffling candidate plans into `Plan A`, `Plan B`, ... before synthesis. |
| `min_quorum` | `int` | `len(members)` | Minimum submitted member plans required to advance to Chair synthesis once the deadline expires (`1 <= min_quorum <= len(members)`). |
| `timeout_minutes` | `int` | `20` | Fan-out deadline in minutes (`> 0`). |
| `auto_cleanup` | `bool` | `true` | Automatically closes and resolves all spawned member and Chair helper beads after synthesis completes, never touching the target bead. |

Validation fails closed at config load on invalid council names, fewer than 2
members, duplicate members, unknown member or chair roles, out-of-range
`min_quorum`, or non-positive `timeout_minutes`.

## CLI commands

### Start a council (`quartermaster council start`)

```sh
# Start using a configured [councils.<name>] panel:
quartermaster council start task-42 --panel default

# Start ad-hoc without a named panel (or override panel defaults):
quartermaster council start task-42 \
  --members planner-a,planner-b \
  --chair planner-chair \
  --min-quorum 2 \
  --timeout 20m \
  --anonymize=true
```

Guarantees:

- **Class 3 autonomy gated** (or `--by TJ`), matching `dispatch`.
- **No duplicate active councils**: refuses starting a second council on a bead
  whose existing council is in `fanout` or `synthesizing` phase. Once a council
  reaches a terminal phase (`done`, `failed`, `canceled`), a new council can be
  started cleanly.
- **Worktree-free sibling helper beads**: each member bead shares the target
  bead's parent (never parented under the target bead), carries
  `role:<member>`, `council:<target>`, `council-role:member` (plus the target's
  `repo:<name>` label if present), and always runs in an isolated role directory
  (`<state>/roles/<helper-bead>`) with no git worktree or branch.
- **Structured independent planning prompt**: every panelist is briefed to
  inspect the target bead and repository strictly read-only with zero cross-talk
  and submit a plan covering:
  1. Architecture & Trade-offs
  2. Hidden Risks / Failure Modes / Blind Spots
  3. Proposed File-Disjoint Bead DAG (`## Boundaries` + default-FAIL `## Acceptance`)
  4. Open Decisions / Unknowns

### Submit a plan or synthesis (`quartermaster council submit`)

```sh
# Panelist submission via stdin (works with exclude_tools = ["edit", "write"]):
quartermaster council submit task-42 --member planner-a --file - <<'EOF'
# Architecture & Trade-offs
...
EOF

# Panelist submission from a file:
quartermaster council submit task-42 --member planner-b --file plan.md

# Chair synthesis submission via stdin:
quartermaster council submit task-42 --chair --file - <<'EOF'
# Canonical Merged Plan
...
EOF
```

`council submit` acquires a per-bead exclusive lock, validates non-empty
content and phase preconditions, and writes atomically (`0600` temp file +
fsync + rename inside `0700` directories). Because `--file -` reads stdin,
strictly read-only worker roles with `exclude_tools = ["edit", "write"]` can
submit artifacts cleanly through the CLI without any file-write tools.

### Inspect status (`quartermaster council status`)

```sh
quartermaster council status
quartermaster council status task-42
quartermaster council status task-42 --json
```

Active and completed councils also surface on matching threads in
`quartermaster context`.

### Cancel an active council (`quartermaster council cancel`)

```sh
quartermaster council cancel task-42 [--by WHO] [--json]
```

Cancels an active `fanout` or `synthesizing` council (Class 3 gated unless
`--by TJ`), marks unfinished runs `canceled`, records a `COUNCIL CANCELED:`
note on the target bead, and closes/resolves all spawned helper beads without
touching the target bead.

## Automated ticker fan-in, blind Chair synthesis, and auto-cleanup

On every tick, the ticker reconciles durable council state under
`<state>/councils/<bead>/state.json`:

1. **Fan-in trigger**:
   - Advances from `fanout` to `synthesizing` immediately when **all** members
     submit, or when `submitted >= min_quorum` and the deadline is reached.
   - Marks any unsubmitted members `timed_out`, writes candidate plan files
     under `<state>/councils/<bead>/chair/` sorted by anonymous label
     (`plan-a.md`, `plan-b.md`, ...), and spawns the Chair sibling bead
     (`role:<chair>`, `council:<target>`, `council-role:chair`).
2. **Strict anonymization (`anonymize = true`)**:
   - Anonymous labels (`Plan A`, `Plan B`, ...) are randomly shuffled at council
     start so config order does not leak authorship.
   - Chair bead titles, descriptions, input filenames, and input file headers
     omit all member role names, model names, agent kinds, and member helper
     bead IDs.
   - The Chair is briefed to independently fact-check code/file/symbol claims
     against the live repo and drop hallucinations, compare consensus vs genuine
     divergences, highlight **Unique Catches**, and synthesize one canonical
     plan + file-disjoint child-bead DAG.
3. **Insufficient quorum failure**:
   - If fewer than `min_quorum` members submit by the deadline, the ticker marks
     the council `failed`, notes `COUNCIL FAILED:` on the target bead, and emits
     a `council_failed` inbox event.
4. **Post-synthesis attribution & auto-cleanup**:
   - When the Chair submits via `quartermaster council submit <bead> --chair`,
     Quartermaster writes `<state>/councils/<bead>/synthesis.md` with an
     automatically appended **Attribution Table** mapping `Plan A..N` back to
     actual role names, models, statuses, helper beads, and member artifacts.
   - Records a concise `COUNCIL DONE:` summary + synthesis path note on the
     target bead and emits a `council_done` inbox event (cleared when the target
     bead closes or is acknowledged via `quartermaster inbox done`).
   - When `auto_cleanup = true`, closes and resolves every spawned member and
     Chair helper bead without touching the target bead. If a helper agent (such
     as the Chair submitting mid-turn) is still working when `resolve` runs, its
     bead ID is queued in `pending_resolve_beads` and automatically resolved on
     a later tick once the agent finishes its turn.

See also [docs/roles.md](roles.md) and
[docs/workflow-delivery-loop.md](workflow-delivery-loop.md).
