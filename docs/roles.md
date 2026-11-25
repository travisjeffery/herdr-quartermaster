# Worker roles (`[roles.<name>]`)

Quartermaster supports configurable worker roles so the coordinator can
dispatch specialized workers — code reviewers, read-only investigators,
post-merge rollout operators, or custom roles — without creating throwaway git
worktrees or sending PR-authoring instructions to non-coding workers.

## Built-in presets

Four roles are always available with zero configuration and can be customized
under `[roles.<name>]` in `~/.config/quartermaster/config.toml`:

| Role | Default `worktree` | Default `brief` | Purpose |
| --- | --- | --- | --- |
| `implementer` | `true` | `implementer` | Default coding worker. Preserves existing worktree, branch, PR-authoring, and auto-merge workflow verbatim. |
| `reviewer` | `false` | `reviewer` | Skeptical, evidence-first PR reviewer. Inspects full diff, CI checks, and all review threads; omits PR-authoring/auto-merge boilerplate; approves at the exact verified head SHA only when explicitly authorized with zero blocking findings, otherwise stops at `needs_me` with file/line findings and a draft comment. Never includes bead IDs or AI tooling mentions in public reviews. |
| `investigator` | `false` | `investigator` | Strictly read-only triage and root-cause diagnosis. Gathers live evidence with zero mutations to repos, PRs, or infrastructure; summarizes findings and recommended follow-up beads with `## Acceptance` criteria. |
| `rollout` | `false` | `rollout` | Post-merge rollout and live verification. Uses `rolling-out` and Actions run URLs, validates IaC previews with `quartermaster gate plan`, respects autonomy dial and `needs_me` gates, verifies live acceptance criteria, and closes via `quartermaster close`. |

## Selecting and persisting a role

Pass `--role <name>` to `quartermaster dispatch` (or `quartermaster focus`), or
label the bead with `role:<name>`:

```sh
quartermaster dispatch task-42 --role reviewer
quartermaster dispatch task-43 --role investigator
quartermaster dispatch task-44 --role rollout
```

Resolution precedence:

1. `--role <name>` CLI flag
2. `role:<name>` label on the bead
3. `implementer` (default)

When a non-default role is selected via `--role`, `dispatch` persists
`role:<name>` on the bead (and replaces any prior `role:*` label) so
`quartermaster resume`, `quartermaster resolve`, and `quartermaster context`
stay consistent automatically. Default `implementer` dispatches add no label.

Validation fails closed:

- Multiple distinct `role:*` labels on one bead are rejected (even when
  `--role` is supplied).
- An empty `role:` label or an unknown role name is rejected before any
  worktree, role directory, or bead claim is created.

## Configuration (`[roles.<name>]`)

Override built-in presets or add custom roles in
`~/.config/quartermaster/config.toml`:

```toml
[roles.reviewer]
agent = "pi"
model = "anthropic/claude-opus-5-5"
thinking = "high"
exclude_tools = ["edit", "write"]
instructions = "Verify every edge case and concurrency invariant."
instructions_file = "~/prompts/review-checklist.md"

[roles.investigator]
agent = "pi"
thinking = "xhigh"
exclude_tools = ["edit", "write"]

[roles.security-audit]
worktree = false
agent = "claude"
model = "claude-opus-5-5"
thinking = "max"
brief = "investigator"
args = ["--verbose"]
instructions = "Cite exact file paths and reproduction steps."
```

### Fields

| Field | Type | Description |
| --- | --- | --- |
| `worktree` | `bool` | Overrides whether dispatch creates a git worktree and branch. Unset defaults to `true` for `implementer` and custom roles, and `false` for built-in `reviewer`, `investigator`, and `rollout`. |
| `agent` | `string` | Optional agent kind override: `claude`, `codex`, `muse`, `pi`, `agy`, or `auto`. Overridden by an explicit `--agent` CLI flag. |
| `model` | `string` | Optional model override. Replaces any existing `--model` (or `-m` for `codex`) flag in `[agents.<kind>] args`, or appends `--model <val>`. |
| `thinking` | `string` | Optional thinking/effort override: `--thinking` for `pi`, `--effort` for `claude` and `agy`, `--reasoning-effort` for `muse`, `-c model_reasoning_effort=<val>` for `codex`. Replaces any existing equivalent flag before resume subcommands. |
| `args` | `[]string` | Optional extra CLI arguments appended to the worker launch args (before resume subcommands). |
| `exclude_tools` | `[]string` | `pi`-only list of tool names or patterns (for example `edit`, `write`, `mcp__server__*`) merged into `--exclude-tools` without dropping base `[agents.pi]` exclusions and without duplicates. |
| `brief` | `string` | Brief template: `"implementer"`, `"reviewer"`, `"investigator"`, or `"rollout"`. Defaults to the role name for built-in presets, else `"implementer"`. |
| `instructions` | `string` | Optional inline text appended under `Role instructions (<name>):` after global `instructions.md`. |
| `instructions_file` | `string` | Optional path (`~`-expanded) appended under `Role instructions (<name>):`. Missing or unreadable files fail dispatch closed. |

Role names must match `^[a-z0-9][a-z0-9_-]*$`. Invalid names, unknown `agent`
or `brief` values, whitespace in `model`, invalid `thinking` levels, or
comma/whitespace-containing `exclude_tools` entries fail config loading closed.

## `worktree = false` directory lifecycle

When a role's effective `worktree` is `false`:

- **Dispatch**: skips git fetch, branch creation, and git worktree creation
  entirely. Works even when reviewing or investigating a repository that is not
  cloned locally. Quartermaster creates an isolated private working directory
  at `<state>/roles/<bead>` (mode `0700`), opens a Herdr workspace there, and
  mentions the bead's configured repository path (if any) in the brief strictly
  for read-only reference so the worker never dirties the main checkout.
- **Resume**: `quartermaster resume` (and `quartermaster context`'s resumable
  detection) discovers exited `worktree = false` workers via
  `<state>/roles/<bead>` without requiring git worktree listing, reuses an open
  free pane in that workspace (or opens a workspace at `<state>/roles/<bead>`),
  and restarts the worker with its role's `agent`, `model`, `thinking`, `args`,
  and `exclude_tools`.
- **Resolve**: `quartermaster resolve <bead>` closes any idle non-worktree
  Herdr workspace for the bead and removes `<state>/roles/<bead>` cleanly
  without touching git worktrees or branches. Calling `resolve` again is
  idempotent. Open beads or working agents still require `--force`.

## Inspecting configured roles

Run `quartermaster roles` (read-only) to list all built-in presets and custom
configured roles with their effective settings:

```sh
quartermaster roles
quartermaster roles --json
```

`quartermaster roles --json` outputs a JSON array of effective role objects
containing `name`, `worktree`, `agent`, `model`, `thinking`, `args`, `brief`,
`exclude_tools`, `instructions`, `instructions_file`, `custom`, and
`configured`.
