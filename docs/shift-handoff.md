# Clean-context shift handoffs and shared-resource lock labels

Quartermaster provides two harness primitives for long-running multi-agent
workflows:

1. **Clean-context shift handoffs (`quartermaster shift`)** — replace an
   at-rest worker session with a fresh-context worker on the exact same
   worktree or role directory without discarding git or workspace state.
2. **Shared-resource mutex labels (`lock:<name>`)** — prevent concurrent beads
   from mutating the same live shared resource at the same time.

---

## Clean-context shift handoffs (`quartermaster shift`)

Long multi-round threads (for example after several review iterations or deep
debugging) accumulate noisy transcripts and context bloat. `quartermaster
resume` deliberately continues the old transcript (`--continue` /
`--session-id <bead>`), whereas `quartermaster shift` hands the thread over to
a **fresh-context session** carrying a structured summary of where things
stand.

### Usage

```sh
# Hand off an at-rest worker to a fresh-context session on the same worktree/role dir:
quartermaster shift task-42 \
  --reason "context bloat after 2 review rounds" \
  --next "address the remaining concurrency finding and re-run go test -race ./..."

# Switch agent kind for the incoming shift (optional):
quartermaster shift task-42 --agent pi --reason "fresh pass"

# Emit machine-readable JSON result:
quartermaster shift task-42 --reason "shift handoff" --json

# Inspect the latest recorded handoff artifact for a bead:
quartermaster shift show task-42
quartermaster shift show task-42 --json

# Override the mid-turn / active-prompt safety guard only when intentional:
quartermaster shift task-42 --force --by TJ --reason "wedged worker"
```

### What `quartermaster shift` guarantees

- **Zero git or workspace destruction:** never deletes the worktree, never
  deletes or resets the branch, and never touches committed, staged, unstaged,
  or untracked files. For `worktree = false` roles, `<state>/roles/<bead>` and
  its contents are preserved intact.
- **Durable handoff artifact (`0600`):** atomically writes a structured JSON
  handoff record to `<state>/handoffs/<bead>.json` (mode `0600` inside a `0700`
  directory) under a per-bead exclusive file lock, capturing:
  - shift number and compact history of prior shifts (`prior_handoffs`)
  - working directory, role, branch, 40-char `HEAD` SHA, dirty status,
    modified/untracked paths, and recent commits
  - open PR metadata, bounded review-loop state and latest blocking findings,
    recent structured milestones, `## Acceptance` checklist, and recent bead notes
  - previous worker metadata (kind, pane ID, session path/ID) and incoming
    worker metadata (kind, pane ID, fresh session ID)
  - shift `--reason` and requested `--next` step
- **Bead audit trail (`HANDOFF:` note):** mirrors a concise `HANDOFF:` note on
  the bead naming the shift number, path, branch, `HEAD` SHA, dirty status,
  PR/review/milestone/acceptance summary, reason, next step, and artifact path.
- **At-rest guard:** refuses to interrupt a worker that is currently `working`,
  `blocked`, waiting on an active control-socket approval ask, or running in
  the caller's own pane unless `--force` is explicitly passed.
- **Autonomy dial gating (Class 3):** requires the bead's effective autonomy
  level to reach Class 3 (matching `dispatch` and `resume`), or `--by TJ`.
  Checks run before any artifact, note, or pane mutation so denied shifts leave
  no side effects.
- **Guaranteed fresh context:**
  - `claude`, `codex`, `muse`, and `agy` launch without `--continue` /
    `resume --last`.
  - `pi` launches with a fresh unique `--session-id <bead>-shift-<N>` while
    preserving the bead's control socket (`--quartermaster-control`) and
    leaving the previous session JSONL intact on disk.
  - The incoming worker receives its normal role brief plus a structured
    `## Shift Handoff` section pointing directly at the artifact and preserved
    state.

---

## Shared-resource mutex labels (`lock:<name>`)

When two independent beads mutate the same live shared resource — such as a
shared staging cluster, a single-slot test database, or a CI workflow toggle —
ordinary `blocks` edges are brittle because the beads may not have a fixed
order. Label both beads with the same `lock:<name>` mutex instead:

```sh
bd label add task-10 lock:staging-cluster
bd label add task-11 lock:staging-cluster
```

A bead may carry more than one `lock:<name>` label (for example both
`lock:staging-cluster` and `lock:ci-workflow-toggle`).

### Rules and lifecycle

- **Valid names:** `<name>` must start with an ASCII letter or digit and
  contain only letters, digits, `.`, `_`, `:`, `/`, or `-` (no whitespace).
  Comparison is case-insensitive so `lock:Staging` and `lock:staging` protect
  the same resource. Malformed or empty `lock:` labels fail closed.
- **Who holds a lock:** any non-closed `in_progress` bead carrying
  `lock:<name>` holds that lock. `open`, `blocked`, `needs_me`, and `closed`
  beads do not hold locks, and a bead never conflicts with itself.
- **Manual dispatch (`quartermaster dispatch`):** checks held locks before
  consuming autonomy dispatch budget, creating a worktree/role directory, or
  claiming the bead. If another `in_progress` bead holds a matching
  `lock:<name>`, `dispatch` fails closed naming every conflicting lock and
  holding bead. Pass `--force` to override a held lock explicitly.
- **Automatic ticker dispatch and wakeups (`blockerWakeupPass`):**
  - Skips locked `open` or `blocked` beads cleanly with zero errors and zero
    duplicate inbox spam while another `in_progress` bead holds a conflicting
    `lock:<name>`, persisting deferred blockers in `state.json` across restarts.
  - Serializes multiple beads sharing a `lock:<name>` even when they unblock on
    the exact same tick: once the first bead auto-dispatches, the remaining
    beads immediately see it as holding the lock and wait cleanly.
  - Automatically dispatches (for chained `role:rollout` beads within the
    autonomy dial) or wakes/notifies waiting beads as soon as the holding bead
    closes or removes its `lock:<name>` label.

See also [docs/roles.md](roles.md) and
[docs/workflow-delivery-loop.md](workflow-delivery-loop.md).
