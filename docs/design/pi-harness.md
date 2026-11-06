# Running the coordinator and workers on pi

Status: **proposed** (2026-11-06). Nothing here changes behavior; this is the
phase 1 design and gap analysis. Building starts once the open questions at
the end are answered.

## Summary

[pi](https://github.com/earendil-works/pi) is a terminal coding agent with a
small core (read, bash, edit, write) and a large extension API. This design
adds `pi` as a fourth agent kind next to `claude`, `codex` and `muse`, usable
for workers (`worker_agent = "pi"`) and the coordinator
(`coordinator_agent = "pi"`), without removing the others.

The plumbing is cheap: Herdr 0.9.1 already starts pi (`herdr agent start
--kind pi`) and already tracks its state through an extension it installs.
The expensive part is safety. Claude Code gives quartermaster a permission
system, an auto-mode classifier, PreToolUse hooks and claude.ai connectors;
pi has none of these built in. Its own security doc says so plainly: "it does
not ask for approval before every tool call", and safety comes from limiting
what the process can reach. Before pi runs unattended, quartermaster needs a
pi extension that rebuilds the guardrails it relies on, and an approval
prompt it can answer.

## Version

| | |
|---|---|
| Installed | 1.5.0 (`~/.local/bin/pi` runs `mise x pi@latest`) |
| Latest | 1.5.0, released 2026-11-02 (npm `@earendil-works/pi-coding-agent`) |

The install is current. The launcher floats on `pi@latest` with
`MISE_MINIMUM_RELEASE_AGE=0`, so every new pane can start a different pi than
the last. For workers that should be pinned: quartermaster starts the
executable named after the kind, so pin the version in mise (`pi@1.5.0`) and
upgrade deliberately, re-running the pilot checks below after each upgrade.
1.5.0 matters to us for: the `blocked` option on `ctx.ui.custom`, so an
approval prompt can report it is waiting; output caps on the extension
process-exec helper; and the fixed saved-trust handling.

## pi features worth adopting

| Feature | Use it for | Role |
|---|---|---|
| `--session-id <id>` | Deterministic sessions: dispatch and resume both pass `--session-id <bead-id>`, which opens that bead's session or creates it. Replaces "most recent session in this folder". | both |
| Extensions (`tool_call` can block, mutate input) | The guard extension: rm-guard, dangerous-bash, subagent and MCP deny rules, approval prompts. | both |
| `pi.events` + herdr's `herdr:blocked` event | Report an open approval as `blocked` to Herdr, so the ticker and board see it. | both |
| `ui_prompt_start` / `ui_prompt_end` | Same signal for any extension dialog. | both |
| `session_start`, `session_compact`, `agent_before_settle` | `bd prime` at start and after compaction, memory recall, end-of-run reminders (bead note, PR auto-merge). | both |
| `--tools` / `--exclude-tools` | Coarse allow/deny, including MCP tools (`mcp__server__*`). A coordinator without edit tools. | both |
| `pi mcp` / `~/.pi/agent/mcp.json` | MCP servers: Linear, Slack, memory, and any connector with a public MCP endpoint. | both |
| `--model provider/id:thinking`, `--models` | Per-role model: a strong model for workers, a cheaper fast one for the coordinator. | both |
| `subagent` example extension | Read-only explore and review subagents, each a separate pi process with its own tool list. | workers |
| `--approve` / `--no-approve`, `trust.json` | Skip the project-trust prompt that stalls a fresh worktree (`agent_not_ready`). | workers |
| `--mode rpc` / SDK | Not now. Herdr shows TUIs; an RPC worker has no pane to watch. Revisit for a headless host. | later |
| `/fork`, `--fork` | Not needed by quartermaster; useful by hand to branch a stuck worker. | — |
| codemode, tool search | Optional; reduces tool-definition tokens when many MCP tools load. | later |

## 1. Launch and briefing

Dispatch already does, per kind: export `BEADS_ACTOR`/`BEADS_DIR` in the pane,
`herdr agent start <name> --kind <kind> --pane <pane> -- <args>`, then
`herdr agent prompt <name> <brief>`. pi fits unchanged:

- `kinds` gains `"pi"`; Herdr's `--kind pi` runs the `pi` executable.
- Args come from `[agents.pi] args`. Proposed default:
  `--approve -e <guard extension> --model <worker model>`. `--approve` is
  safe here because the worktree is our own checkout; the guard extension is
  passed explicitly so it loads even if discovery is turned off.
- Session: quartermaster appends `--session-id <bead-id>` on dispatch and on
  resume (IDs may hold letters, digits, `.`, `_` and `-`, so a bead id fits).
  `resumeArgs` is a static per-kind table today; pi needs the bead id, so
  resume builds pi's args from the bead (a few lines in `resume.go`).
  `--continue` would also work and needs no code, but it picks the most recent
  session, which is wrong after a manual `pi` run in the same worktree.
- Brief: delivered as today, with `herdr agent prompt`, which types into the
  pi TUI and submits. The pilot confirms the brief's newlines survive.
  The brief's line "Start with `bd prime` unless its output is already in
  your context from a session hook" stays correct either way.
- Model and provider: `--model` per role. Because one `[agents.pi]` table
  serves workers and the coordinator, a per-role model needs a
  `coordinator_args` field on `AgentConfig`, used by `openCoordinator` when
  set.

## 2. State detection

Herdr already recognizes pi. Herdr installs
`~/.pi/agent/extensions/herdr-agent-state.ts` (integration version 8), which
reports over Herdr's socket:

- `working` from `agent_start`, `idle` from `agent_settled` once
  `ctx.isIdle` is true;
- `blocked` while any extension has emitted `herdr:blocked` with
  `active: true`, with its label as the message;
- the session id and path, so `herdr agent get` shows which session a pane
  runs.

Quartermaster reads only Herdr's `agent_status`, so the ticker, the board,
idle nudges and `needs_me` handling work as they do for Claude. "Done" is a
bead state, not an agent state, and is unaffected.

Gaps, both small and both in our own extension, not Herdr:

1. Herdr's extension does not listen to `ui_prompt_start`, so a plain
   `ctx.ui.select` does not show as blocked. The guard extension emits
   `herdr:blocked` itself around every prompt it opens.
2. The Herdr extension reports only in TUI mode. That is what we run.

## 3. Approval prompts and guardrails

### What is lost

Running a worker on pi instead of Claude Code drops all of these:

| Guardrail today | Where it lives | On pi |
|---|---|---|
| Auto-mode classifier (a model judges each risky action; blocks security-weakening, destructive and outward-facing actions) | Claude Code | **Gone.** No equivalent. |
| Permission prompts and `quartermaster prompt/answer --expect <hash>` | Claude Code TUI, screen parsed by `prompt.go` | **Gone.** pi runs every tool call without asking. |
| `block-dangerous-bash.py`, `rm-guard.sh` (PreToolUse Bash) | `~/.claude/settings.json` hooks | **Gone** unless ported. |
| `task-guard.sh` (subagent types with write tools prompt, read-only ones pass) | PreToolUse Task/Agent hook | **Gone**; pi has no built-in subagents. |
| Deny rules (memory delete, cleanup, consolidate, distill tools) | settings `permissions.deny` | Portable as `--exclude-tools`. |
| SessionStart / PreCompact `bd prime`, memory recall | hooks | **Gone** unless ported to `session_start` / `session_compact`. |
| `~/.claude/CLAUDE.md` global instructions | Claude Code | Not read; pi reads `~/.pi/agent/AGENTS.md` or `CLAUDE.md` instead. |
| Skills under `~/.claude/skills` and plugin skills | Claude Code | Not read; pi reads `~/.pi/agent/skills`, `~/.agents/skills`, `--skill`. |
| claude.ai connectors (Linear, Slack, Google, AWS, kubernetes, grafana, the internal review and deploy connectors) | claude.ai account | **Gone.** They are tied to the claude.ai account. Some have public MCP endpoints pi can sign in to; internal ones may not. |
| Artifact, ScheduleWakeup, Monitor, subagents (`Agent`), SendUserFile | Claude Code tools | **Gone**; see section 5. |
| Notification hooks (moshi) | hooks | Already ported: `~/.pi/agent/extensions/moshi-hooks.ts`. |
| atuin history | hooks | Already ported: `atuin.ts`. |

The classifier is the one that matters. Today an unattended worker is
stopped by it before, for example, weakening a security control or running a
production change; the brief tells it to turn such a denial into a `RUN:` note
and `needs_me`. On pi, that worker would simply run the command. The
deterministic hooks catch a few known shapes (`rm -rf` outside the worktree);
they do not catch "apply this Terraform", "kubectl delete", "push to main" or
"post to a customer channel".

### Proposed boundary: the quartermaster guard extension

A pi extension shipped in this repo (`pi/quartermaster-guard.ts`), installed
by `quartermaster configure` into `~/.pi/agent/extensions/` or passed with
`-e`. It runs inside the pi process on every `tool_call`:

1. **Reuse the hooks.** Run the existing `block-dangerous-bash.py` and
   `rm-guard.sh` with the same JSON Claude Code hands them (`tool_input`,
   `cwd`) through the extension API's process helper, with an argument list,
   not a shell string. A non-zero exit blocks the call with the script's
   message as the reason. One rule set serves both harnesses.
2. **Ask rules** for the actions the classifier catches today, as patterns
   over bash commands: `terraform`/`tofu` apply and destroy, `kubectl`
   writes, `helm` upgrade/install/uninstall, mutating `aws` verbs,
   `gh pr merge` without `--auto`, `gh api` with `-X POST|PATCH|DELETE`
   outside an allowlist (review-thread replies and resolves are allowed),
   `git push` to the base branch or with `--force`, `bd dolt push`, and
   commands reading known secret paths. MCP tools get the same treatment by
   name (Slack send tools ask; memory delete tools are excluded outright).
3. **An approval prompt quartermaster can answer.** On an ask rule the
   extension emits `herdr:blocked` (so the agent shows blocked) and opens a
   `ctx.ui.select` with a fixed shape: a title line, the command, the rule
   that matched, and options `Yes` / `No, and tell the agent why`. It also
   writes the pending prompt to
   `$QUARTERMASTER_STATE_DIR/approvals/<pane>.json` with a hash of
   (rule, command). `quartermaster prompt <bead>` reads that file first and
   falls back to screen parsing; `quartermaster answer --expect <hash>`
   checks the hash against the file, then sends the keys. A structured file
   is sturdier than teaching `prompt.go` a second TUI's layout, and the hash
   means what it does today.
4. **Autonomy.** The extension calls `quartermaster autonomy check <bead>
   --action merge|apply|decide` for the matching rule classes; when it passes,
   the call goes through without a prompt, exactly as the brief's autonomy
   rules allow. It never auto-allows the "never covered" list (destroying
   shared resources, secrets, spending money, messaging outside the team,
   approving PRs).
5. **Unattended timeout.** A prompt nobody answers within N minutes is
   denied with a reason telling the agent to write a `RUN:` note and go
   `needs_me`, mirroring Claude Code's auto-deny.
6. **Optional model judge (later).** For bash commands no rule matches, ask
   a small model "is this destructive, outward-facing or a security change?"
   through `ctx.modelRegistry`, and treat "yes" as an ask rule. This is the
   closest thing to the classifier, at the cost of latency and tokens per
   call; it is not in the pilot.

### The risk, stated plainly

Even with the guard extension, a pi worker is less contained than a Claude
Code worker:

- Pattern rules cover what we thought of. The classifier judges intent;
  rules do not. Shell indirection (`bash -c "$(…)"`, a script the agent
  wrote, `xargs`, a Makefile target) gets past patterns. The guard should
  treat "writes a script, then executes it" as an ask, but it will not be
  airtight.
- The extension runs in the same process and user as the agent. A worker
  that edits `~/.pi/agent/extensions/` and reloads can disable it. The guard
  must deny writes under `~/.pi/` and reload commands, and the pilot loads it
  with `-e` from a path the worker cannot write.
- pi's own docs say the only real boundary is OS isolation. The stronger
  option is running pi workers inside the worker container from
  [worker-hosts.md](worker-hosts.md) with only the credentials the bead
  needs. That is compatible with this design and is the recommended
  long-term answer.

Recommendation: no pi workers on beads that touch production, credentials or
shared infrastructure until the guard exists **and** pi workers run in the
container. Pilot on a code-only bead in this repo.

## 4. Instructions, bd prime and skills

- **Global instructions.** pi reads `~/.pi/agent/AGENTS.md` (or `CLAUDE.md`)
  for every directory. Symlink `~/.pi/agent/AGENTS.md` to
  `~/.claude/CLAUDE.md` so there is one source. Its Claude-specific lines
  (the Explore agent, auto mode, Claude Code's deletion check) are harmless
  noise for pi; a later pass can split them out.
- **Repo instructions.** pi reads `AGENTS.md` and `CLAUDE.md` from the
  working directory and its parents, so a worktree gets the repo's files as
  Claude does. Context files load regardless of project trust.
- **bd prime.** The guard extension (or a small sibling) runs `bd prime` on
  `session_start` and `session_compact` and adds its output as a hidden
  context message, matching the Claude SessionStart and PreCompact hooks.
  Then the brief's "unless it is already in your context" clause applies as
  it does for Claude.
- **Skills.** pi discovers `~/.pi/agent/skills` and `~/.agents/skills` and
  takes `--skill <dir>`. Link the skills workers actually use (PR review,
  resolve, rollout) into `~/.agents/skills`; most `SKILL.md` files work
  unchanged, but ones that name Claude-only tools (Agent, Artifact,
  AskUserQuestion) need a pi variant or are left out.
- **Memory recall.** `agent-memory-recall` in a `session_start` handler,
  with a `pi` client name.

## 5. The coordinator on pi

- **Home and guide.** `openCoordinator` writes the guide as `AGENTS.md` and
  `CLAUDE.md` in the coordinator's folder; pi reads `AGENTS.md` there, so this
  works unchanged. The guide's `--agent claude|codex|muse` line becomes
  `kindList()`.
- **Ticker messages.** The ticker delivers with `herdr agent prompt`, the
  same path as the brief. When pi is mid-run the text arrives as a queued
  message; the pilot checks it is not lost or merged into half-typed input.
- **ScheduleWakeup and Monitor.** The coordinator does not need them: the
  ticker already pushes every event (PR checks, reviews, idle workers,
  needs_me) into the coordinator's pane. Nothing to port.
- **Tools.** The coordinator mostly runs `quartermaster`, `bd`, `herdr` and
  `gh`, plain bash that works in pi as is. What it loses:
  - Linear: Linear has a public MCP server; add it with `pi mcp add linear
    --url https://mcp.linear.app/mcp` and sign in once with `/mcp`. The
    ticker already uses the `linear` CLI, which is another route.
  - Slack, Google, AWS, kubernetes, grafana and the internal connectors: each
    needs its own MCP endpoint and sign-in, or pi roles go without it.
  - Artifact and SendUserFile: no pi equivalent. Reports stay in the pane or
    in files.
  - Subagents: the `subagent` example extension, restricted to read-only
    tool lists, replaces Explore.
- **Tool set.** The coordinator "never does the work itself". On pi that can
  be enforced: `--exclude-tools edit,write` keeps bash (it needs it for
  `quartermaster` and `bd`) and drops file editing.

## 6. Rollout and rollback

1. **Code, behind config** (one PR): add `pi` to `kinds`; `--session-id
   <bead-id>` on dispatch and resume; `coordinator_args`; a board key; the
   README's "Choosing the agent". `auto` never picks pi (no quota reading, as
   for muse). Defaults stay `claude`, so nothing changes until someone asks
   for pi.
2. **Guard extension** (one PR): the extension, its approval file,
   `quartermaster prompt` and `answer` reading it, `bd prime` injection, and
   tests for the rule matcher using the Python hook's cases.
3. **Setup** (by hand, TJ): pin pi in mise; the `~/.pi/agent/AGENTS.md`
   symlink; Linear MCP sign-in; provider sign-in for the chosen models.
4. **Worker pilot**: `quartermaster dispatch <bead> --agent pi` on one
   low-risk, code-only bead in this repo. Check: the brief arrives intact;
   state goes working, idle, and blocked on a guarded command;
   `quartermaster prompt` shows the approval and `answer --expect` answers
   it; resume after killing the pane reopens the same session; the PR lands
   with notes and auto-merge.
5. **Coordinator pilot**: the coordinator on pi for a day with the Claude
   coordinator stopped; check ticker delivery and Linear moves.
6. **Defaults**: only after both pilots and TJ's call, set
   `worker_agent = "pi"` and `coordinator_agent = "pi"`, still keeping beads
   that touch production or shared infrastructure on Claude until pi workers
   run in the container.

**Rollback** at any stage: set the defaults back to `claude`, or pass
`--agent claude`. A pi worker's bead, branch and worktree are kind-neutral,
so `quartermaster resume` can restart the bead on Claude; it loses the pi
conversation, not the work, which the notes and commits carry. Removing `pi`
from `kinds` is not needed to roll back.

## 7. Effort and open questions

| Step | Size |
|---|---|
| 1. kind, session id, coordinator args, board, docs | ~0.5 day |
| 2. guard extension, approval file, prompt/answer, tests | 2–3 days |
| 3. setup | ~1 hour (TJ) |
| 4–5. pilots and fixes | 1–2 days |
| Container isolation for pi workers | part of worker-hosts |

Open questions for TJ:

1. **Model and provider.** pi has `openai-codex` (ChatGPT sign-in) and
   `openai` (API key) ready; `anthropic` is not configured. Which model for
   workers and which for the coordinator? An Anthropic model needs an API key
   or a sign-in in pi, and its usage is billed apart from the Claude Code
   subscription.
2. **Must-have guardrails before the pilot.** Proposed minimum: the reused
   hooks, ask rules with prompts quartermaster can answer, the unattended
   timeout, and denied writes under `~/.pi/`. Is the model judge required
   before switching defaults, or is container isolation the bar?
3. **Connectors.** Which of Slack, Linear, AWS, kubernetes and grafana must
   pi roles have? Each without a public MCP endpoint is a capability they go
   without.
4. **Pinning.** OK to pin pi in mise for quartermaster and upgrade only by
   hand?
5. **Scope of the switch.** Replace Claude everywhere, or make pi the default
   for code-only beads and keep Claude for infra and production work? The
   second keeps the classifier where it matters most.
