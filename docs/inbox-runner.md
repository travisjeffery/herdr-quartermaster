# Headless Inbox Runner (opt-in)

The headless inbox runner lets Quartermaster autonomously drive bounded
coordinator turns for allowlisted inbox events while the interactive
coordinator TUI is idle or closed — without touching the TUI, injecting
terminal keystrokes, or racing human input.

It is **opt-in** (`[inbox_runner] enabled = false` by default) and separate
from both:

- the [draft-safe pi coordinator inbox](pi-coordinator-inbox.md)
  (`pi/coordinator.ts`), which triggers guarded idle turns inside a live TUI
  session when the editor is empty and safe, and
- the fenced post-merge [headless pi coordinator](pi-headless.md)
  (`quartermaster headless`), which operates only on explicitly enrolled
  native `pi-rpc` workers.

## Configuration

Add `[inbox_runner]` to `~/.config/quartermaster/config.toml`:

```toml
[inbox_runner]
enabled = true                 # opt-in; default false
idle_seconds = 120             # min age of eligible inbox item AND min interactive coordinator idle time
cooldown_seconds = 120         # min interval between completed ticker-triggered runs
timeout_seconds = 300          # hard wall-clock deadline; kills child process group on expiry
max_attempts = 2               # max invocations per inbox filename before capping
kinds = [                      # allowlisted non-human-gated event kinds (default shown)
  "ready_for_review",
  "review_bump",
  "orphan_finish",
  "merged",
  "closed",
  "unblocked",
  "checks_failing",
  "likely_stale",
]
# command = ["pi", "--no-approve", "--print", "--no-session", "--model", "quartermaster/coordinator-auto"]
notify = true                  # emit privacy-safe desktop summary via notify-send on handled items or failure
suppress_interactive_nudges = false  # opt-in: stop routine ticker nudges to the interactive coordinator for allowlisted kinds while enabled
auto_answer_needs_me = false   # opt-in: allow dial-gated auto-answering of structured, non-critical needs_me questions with a Recommendation
auto_answer_max_class = 5      # max impact class for auto_answer_needs_me (0-7; default 5; classes 8 and 9 are always human-only)
```

When `auto_answer_needs_me = true`, the headless inbox runner may also evaluate
structured `needs_me` questions (`QUESTION:` / `OPTIONS:` / `CLASS:`) whose
`Clears.Kind` is `needs_me` and that pass all deterministic preflight checks:
the bead is actively `needs_me`, has no pending `RUN:` command, has no
`CRITICAL:` line, states an explicit valid `Recommendation: <n>` and `CLASS: <c>`,
and satisfies `c <= min(bead_level, auto_answer_max_class)` (where
`auto_answer_max_class` is capped at `0..7`). The runner must still verify live
evidence independently and answer through `quartermaster answer <bead> <choice>
--question --expect <hash> --class <c> --by inbox-runner --reason "..."`, which
re-enforces the hash, autonomy dial, and `CRITICAL:`/`RUN:` blocks at execution
time. Approval prompts (`prompt`), `agent_blocked`, `breaker_tripped`,
`lease_expired`, `unsafe_auto_merge`, `RUN:` commands, `CRITICAL:` questions,
and unstructured/free-text questions remain unconditionally human-gated.

When `enabled = true` and `suppress_interactive_nudges = true`, the ticker stops
nudging the interactive coordinator pane (and skips enqueueing `quartermaster.inbox`
notices on a `pi` coordinator) for allowlisted routine inbox kinds handled by the
headless runner. Human-gated events (`needs_you`, `prompt`, `breaker_tripped`,
`lease_expired`, `unsafe_auto_merge`) and non-allowlisted kinds still nudge the
interactive coordinator as usual. Leaving `suppress_interactive_nudges = false`
(the default) or setting `enabled = false` preserves existing nudge behavior.

### Default command and custom `command`

When `command` is omitted, Quartermaster derives the headless command from
`coordinator_agent` and runs it in the coordinator directory
(`<state>/coordinator`, containing `AGENTS.md` and `CLAUDE.md`) with the runner
prompt supplied on **stdin**:

| `coordinator_agent` | Default headless argv |
|---|---|
| `pi` | `pi --no-approve --print --no-session` + `[agents.pi] coordinator_args` stripped of interactive-only flags (`--approve`, `--tui-mode`, `--mode`, `--session*`, `--continue`, `--resume`, `--fork`, `--quartermaster-coordinator`, `--quartermaster-control`, etc.) + `-e <state>/pi/quartermaster.ts --quartermaster-router <state>/pi/router.json` (unless `--no-extensions` is set) |
| `claude` | `claude --print` + `[agents.claude] coordinator_args` stripped of `--continue`/`--resume` |
| `codex` | `codex exec` + `[agents.codex] coordinator_args` + `-` |
| `agy` | `agy --print` + `[agents.agy] coordinator_args` stripped of `--continue`/`--resume` |
| `muse` | `muse --print` + `[agents.muse] coordinator_args` stripped of `--continue`/`--resume` |

Notice that for `pi`:

- `--no-approve` prevents interactive project-local trust prompts,
- `--print --no-session` runs one ephemeral turn and exits without mutating the
  interactive coordinator's session history,
- `--quartermaster-coordinator` is never passed, so `pi/coordinator.ts` and the
  live TUI socket are untouched, and
- `quartermaster.ts` + `router.json` remain loaded so virtual models
  (`quartermaster/coordinator-auto`), PreToolUse bash guards, and read-only
  subagent checks still apply.

To override the command completely, set `[inbox_runner] command = [...]`.
Quartermaster executes `command[0]` with `command[1:]` directly in the
coordinator directory and writes the runner prompt to its stdin.

## Commands

```sh
# Inspect enabled state, lock state, interactive coordinator check, cooldown,
# inbox item eligibility, last run diagnostics, and per-file attempt counts:
quartermaster inbox-runner status
quartermaster inbox-runner status --json

# Preview eligibility and resolved argv without running or mutating state:
quartermaster inbox-runner run --dry-run

# Run one headless cycle immediately (skips the ticker's enabled, cooldown,
# and item-age gates, while keeping the single-flight flock, coordinator-busy
# check, kind allowlist, and per-file max_attempts cap):
quartermaster inbox-runner run
```

When `[inbox_runner] enabled = true`, the background ticker checks eligibility
at the end of each tick and launches the runner **asynchronously** in the
background so the ticker loop is never blocked.

## Safety invariants

1. **Opt-in by default.** `enabled` defaults to `false`; installing or
   upgrading Quartermaster never starts autonomous headless coordinator turns
   automatically.
2. **Single-flight process lock.** Every execution acquires an exclusive
   non-blocking `flock` on `<state>/inbox-runner.lock` (mode `0600`).
   Overlapping ticker ticks or manual invocations skip immediately rather than
   queuing concurrent coordinator runs.
3. **Interactive coordinator busy / waking / recent-activity skip.** Before
   running, the runner checks the interactive coordinator via Herdr, its
   session transcript (when readable), and — for a live local `pi` coordinator —
   its read-only `inbox_status` readiness (`wake_state` and `idle_ms`). If the
   interactive coordinator is working, blocked, compacting, in human-input
   preflight, actively waking on unread inbox events (`ready`, `quiet_wait`,
   `waking`), had recent human/turn/UI activity within `idle_seconds`, or
   changed Herdr state within `idle_seconds`, the runner skips without touching
   the TUI. Passive `quartermaster.inbox` TUI notifications without a model
   turn do not count as activity. If Herdr is live (socket present) but listing
   agents fails, the runner fails closed and skips. If no interactive
   coordinator is running (or Herdr is closed), or a live `pi` coordinator has
   been blocked from waking (for example by an untouched draft, dialog, or
   failed wakeup) for at least `idle_seconds`, the runner proceeds so
   autonomous follow-through works AFK.
4. **Strict allowlist excluding human-gated kinds.** `kinds` is validated at
   config load (must be non-empty and known) and enforced again at runtime.
   Human-gated and escalation kinds — `needs_you`, `prompt`, `needs_me`,
   `agent_blocked`, `breaker_tripped`, `lease_expired`, and
   `unsafe_auto_merge` — are rejected in config validation and unconditionally
   filtered out at runtime. Items belonging to beads enrolled in the separate
   `quartermaster headless` post-merge consumer are also skipped.
5. **Autonomy dial and provenance.** The runner prompt marks automated
   provenance (`[quartermaster: automated headless inbox runner, not the user, approves nothing]`),
   lists only the exact eligible inbox filenames, instructs the agent to follow
   `AGENTS.md`, run `quartermaster autonomy check` before class-3+ actions,
   record `AUTO: [inbox-runner L<n> C<c>]` notes, acknowledge only handled
   eligible files with exact filenames (`quartermaster inbox done <file>...`),
   and never touch `needs_you`/`prompt` items.
6. **Bounded retries and cooldown.** Per-file attempt counts and the last
   completed run timestamp persist atomically in `<state>/inbox-runner.json`
   (mode `0600`) **before** launching the child process, so crashes or un-acked
   items cannot spin infinitely past `max_attempts` (default 2). Once an inbox
   file is archived or removed, its attempt entry is automatically pruned.
   Ticker-triggered runs also wait at least `cooldown_seconds` after each
   completed invocation.
7. **Isolated environment and process-group timeout.** The child runs in its
   own process group (`Setpgid: true`) with `HERDR_ENV`, `HERDR_PANE_ID`,
   `HERDR_TAB_ID`, `HERDR_WORKSPACE_ID`, `HERDR_TERMINAL_ID`,
   `HERDR_PLUGIN_CONTEXT_JSON`, `QUARTERMASTER_BEAD`, and
   `QUARTERMASTER_EXEC_SOCKET` stripped so it never mistakes itself for an
   interactive pane or worker. Exceeding `timeout_seconds` sends `SIGKILL` to
   the entire process group (`-pgid`). Captured stdout/stderr diagnostics are
   bounded and sanitized (secrets, tokens, URLs, and ANSI escapes redacted).
8. **Privacy-safe desktop summaries.** When `notify = true` (and `[desktop]` is
   enabled), the runner emits a concise desktop notification via `notify-send`
   when items are handled or a run fails/times out — containing only counts,
   event kinds, and fixed status messages (never raw output, secrets, or tap
   links).

## Rollback

To disable automatic headless inbox runs immediately:

1. Set `enabled = false` under `[inbox_runner]` in
   `~/.config/quartermaster/config.toml` (or remove the `[inbox_runner]`
   table).
2. Restart the ticker so the updated configuration takes effect:

```sh
quartermaster ticker stop && quartermaster ticker start
```

Stopping the ticker also cancels any in-flight ticker-launched headless runner
and cleans up its process group. Existing inbox files remain safely in
`<state>/inbox/` for the interactive coordinator, and
`<state>/inbox-runner.json` can be inspected or removed without affecting
Beads or active workers.
