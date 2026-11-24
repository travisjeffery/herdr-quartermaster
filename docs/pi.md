# Workers and the coordinator on pi

[pi](https://github.com/earendil-works/pi) is an opt-in agent kind next
to `claude`, `codex`, `muse` and [`agy`](agy.md): `--agent pi`, `worker_agent = "pi"` or
`coordinator_agent = "pi"`. Nothing changes until you ask for it. This page
covers what works under pi, how it differs from Claude Code, and the setup
steps only you can do. Checked against pi 1.6.0 and herdr 0.9.5; the quota
router below needs pi 1.6 (virtual models).

## What works

| | How |
|---|---|
| Start | `herdr agent start --kind pi`; herdr's own pi extension reports working, idle and blocked. |
| Models | Workers `quartermaster/worker-auto`, coordinator `quartermaster/coordinator-auto`: quartermaster's pi virtual models, preferring `antigravity/gemini-4-neon-xhigh` first and failing over through `anthropic/claude-opus-5-5` / `anthropic/claude-fable-5-2` and `openai-codex/gpt-6.2-astra` on low provider quota or failover errors (below). `[agents.pi] args` and `coordinator_args` replace them; a physical `--model` opts out of routing. |
| Model check | Before starting pi, quartermaster runs `pi --list-models <id>` (with the extensions the start will load, quartermaster's included, so the virtual models list) and, if the model isn't listed, `pi auth check --provider <p>`, and refuses with the fix. For a virtual model, at least one physical model in its ordered chain must be listed by pi; if none is usable, dispatch fails closed with actionable per-model errors before creating a worktree or claiming the bead. Neither command prints a credential. |
| Brief | Typed in with `herdr agent prompt`, as for Claude. pi has no SessionStart hook; the brief already says to run `bd prime` first. |
| Prompts after the brief | Workers use their control socket (below), with the existing pane fallback. **Coordinator ticker notifications** use a separate authenticated, durable inbox channel, never terminal paste, even when the socket is absent. |
| Ask-gates | A guarded command on `[agents.pi] ask` waits for `quartermaster answer` instead of blocking (below). The list starts empty. |
| Sessions | A worker runs `pi --session-id <bead>`, so its session file is `~/.pi/agent/sessions/--<worktree>--/<time>_<bead>.jsonl`. Dispatch and resume open that session by id. |
| needs_me wake check | Read from the session file: a prompt after the worker settled resumes the bead, pi waking itself doesn't (below). |
| Finished turns | The id of the session's last turn-ending assistant entry is kept in the ticker's snapshot; a new id is a finished turn even if no pass saw the agent working, and an `error` or `aborted` turn is reported as `stopped on error: <message>`. On a transient ACP stall error (such as `ACP stalled for 5m with no output`), the ticker automatically nudges an active `in_progress` worker up to `[stall_retry] max_retries` times (default `2`, with exponential `backoff_seconds` starting at `30s`), recording each retry in `journal.jsonl` and a bead note, resetting after a healthy turn, and escalating to the inbox once the retry cap is exhausted. Beads that are `needs_me`, `blocked`, `closed`, on job `HOLD`, or at an approval prompt are never auto-retried. |
| Guards | quartermaster's pi extension, loaded into every pi it starts (below). |
| Structured cards | Worker sessions (`QUARTERMASTER_BEAD` / `--quartermaster-control`, never the coordinator) get `ask_user` and `report_milestone` tools backed by `quartermaster card` ([workflows and cards](workflows.md)). |
| Close verifier | Default remains `claude -p`. Opt in separately to the [filesystem-only pi verifier](pi-verifier.md) with `[verifier] backend = "pi"`. |

## Quota routing: the auto models

`quartermaster/worker-auto` and `quartermaster/coordinator-auto` are pi
virtual models (`docs/virtual-models.md` in pi) registered by
quartermaster's extension (`pi/router.ts`), so the TUI and native RPC
launches both get them. The selection stays the virtual model for the
life of the session; each request is dispatched to a physical model, and
assistant messages record which one answered. One session, one worktree,
any provider in the chain.

- **Ordered physical model chains and per-model `:thinking` overrides.**
  Both roles default to `antigravity/gemini-4-neon-xhigh` first, then fail
  over through Claude (`anthropic/claude-opus-5-5` for workers,
  `anthropic/claude-fable-5-2` for the coordinator) and OpenAI
  (`openai-codex/gpt-6.2-astra`). Entries in `[agents.pi]`
  `router_worker_models`, `router_coordinator_models`, and the legacy
  singular `router_worker_model`, `router_coordinator_model`, and
  `router_fallback_model` fields accept an optional `:thinking` suffix
  (`off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`), e.g.
  `anthropic/claude-opus-5-5:high`. Duplicate entries for the same
  canonical `provider/model` are deduplicated keeping the first
  occurrence. Fixed-tier non-reasoning models such as
  `antigravity/gemini-4-neon-xhigh` always receive `"off"`; reasoning
  models use the chain entry's `:thinking` override when set, else the
  selected session thinking level (`xhigh` unless the launch sets
  another). Only physical models listed as available in pi are routed to
  (stripping `:thinking` during `pi --list-models` and registry lookup),
  so environments with a subset of providers work without extra
  configuration. Virtual-model display labels derive dynamically from the
  configured provider order in `router.json` with a neutral
  `(ordered failover)` fallback when no `router.json` is loaded.
- **Per-provider quota gating and thresholds.** quartermaster writes one
  normalized quota snapshot, `<state dir>/pi/quota.json`, from the
  Antigravity statusline observation (`agy-statusline.observation.json`),
  Claude's statusline observation, and Codex's latest rollout (a reading
  older than 6h is stale). It is written on every pi launch and refreshed
  by the ticker each minute, and the router reads it per request. Each
  candidate in the chain is gated **only** by its own provider's reading
  (known aliases `antigravity/*` -> `quota.antigravity`, `anthropic/*` ->
  `quota.claude`, `openai*/*` -> `quota.openai`, plus generic lookup by
  provider name for any additional provider key present in `quota.json`,
  such as `google`, `vertex`, or `openrouter`), so low headroom on one
  provider never moves a session off a different healthy provider. Global
  `router_threshold` / `router_recover` (defaults 20 / 30) can be
  overridden per provider under `[agents.pi.router_providers.<provider>]`
  with optional `threshold` and `recover` percents (omitted bounds inherit
  the global setting). When a fresh reading shows at most that provider's
  threshold percent left in its tightest live window, new turns step to
  the next healthy model in the chain; if every non-cooled candidate is
  low, the one with the most headroom wins.
- **Failover errors, cooldowns, and recovery.** Tool follow-ups stay
  sticky on the turn's physical model. On a retry after quota
  exhaustion, rate limits (`429`, `529`, `ResourceExhausted` /
  `agy rate limit`), provider unavailability, or expired/invalid auth
  tokens (such as `authentication token has expired` or `401`), the
  router puts the failed provider in cooldown on the session branch
  (parsing `resets in <duration>` when present, else defaulting to 15m)
  and steps to the next healthy usable model in the chain. Once switched
  away for low quota, a provider is held out until a fresh reading
  recovers to its effective `recover` percent or its tight window resets;
  a quota cooldown clears when its reset time passes or a strictly newer
  fresh reading reaches its effective `recover` percent, and an auth
  cooldown clears when its cooldown expires. Context overflow and
  ordinary network errors stay on the same model.
- **Live reload without restarting the ticker.** On every minute refresh,
  the ticker reloads `config.toml` before updating `router.json`, so edits
  take effect on the next turn boundary without restarting the ticker or
  live pi sessions. If `config.toml` has a transient syntax or validation
  error while being edited, the ticker logs the error and preserves the
  last known-good `router.json`.
- **CLI inspection and updates.** Use `quartermaster router` (or
  `quartermaster router status`) to inspect the effective worker and
  coordinator chains, global and per-provider thresholds, `router.json`
  sync status, and provider headroom/freshness from `quota.json`. Run
  `quartermaster router sync` to validate `config.toml` and write
  `router.json` immediately, or
  `quartermaster router set worker|coordinator <model>...` to update the
  ordered chain in `config.toml` (preserving comments, formatting, file
  permissions, and unrelated settings) and sync `router.json` right away.
- **No fresh quota.** A missing, stale or malformed snapshot never
  triggers a switch and never fails a request over quota data. (A
  provider already held out for low quota stays held until a fresh
  reading says it recovered.)
- **Existing sessions are not migrated.** A session keeps the model it
  was started with; only new pi starts select the auto models (a resume
  of a session started on one restores it, as pi restores any selection).
  Remote pi starts load no extension, so the auto models don't exist
  there: the model check refuses them with the fix (a physical `--model`
  in `[agents.pi] args`).

```toml
[agents.pi]
router_threshold = 20   # global percent provider headroom left: switch at or below
router_recover = 30     # global percent: switch back at or above
# router_worker_models = [
#   "antigravity/gemini-4-neon-xhigh",
#   "anthropic/claude-opus-5-5:high",
#   "openai-codex/gpt-6.2-astra:xhigh",
# ]
# router_coordinator_models = [
#   "antigravity/gemini-4-neon-xhigh",
#   "anthropic/claude-fable-5-2:high",
#   "openai-codex/gpt-6.2-astra:xhigh",
# ]
#
# [agents.pi.router_providers.antigravity]
# threshold = 10
# recover = 25
```

The physical chains, per-model `:thinking` overrides, and global/per-provider
thresholds reach the extension as `<state dir>/pi/router.json`, named by
`--quartermaster-router`, which quartermaster writes on each pi start,
reloads from `config.toml` on ticker refreshes, and updates immediately on
`quartermaster router sync` / `quartermaster router set`; the router
re-reads it per request.

## Native RPC alternative

For local headless workers, use `dispatch <bead> --backend pi-rpc --agent pi`.
This is an additional opt-in transport, with a persistent supervisor, rather
than a migration of these TUI sessions. See [native RPC workers](pi-rpc.md) for
the isolated trial, approvals, lifecycle, limitations and rollback. A separate
[headless coordinator](pi-headless.md) can consume explicitly enrolled post-merge
follow-through without waking or replacing the interactive TUI.

## TUI durable sessions, and why the file rather than RPC

pi stores a conversation as an append-only JSONL tree of entries with stable
ids (pi's `docs/session-format.md`); `docs/rpc-commands.md` calls an entry id
"a durable cursor" (`get_entries` with `since`). quartermaster uses the same
property without RPC: herdr's pi extension reports the session file's path
(`agent_session.kind = "path"`), and the ticker reads it.

RPC mode (`pi --mode rpc`) would replace the TUI that the worker's herdr pane
shows and that herdr's state extension reports from, and would need a client
holding pi's stdin and stdout. The file is pi's own durable record, readable
whether pi is running or not. That is the smallest thing that is correct.
What RPC would add, a structured way to send a prompt and answer a question,
comes from the control socket instead (below), and the pane stays the view.

- **Wake check.** pi's user messages carry no origin, so quartermaster's
  extension appends a `quartermaster.input` custom entry for each prompt with
  pi's input source: `interactive` (typed, which is how `herdr agent prompt`
  delivers), `rpc`, `quartermaster` (sent over the control socket) or
  `extension` (`pi.sendUserMessage` from any other extension). Custom entries
  never reach the model. A needs_me worker that starts working again resumes
  the bead only if an `interactive`, `rpc` or `quartermaster` entry falls
  after it settled. Without those entries, any user message after it settled
  counts. The entry is written when pi accepts the prompt, which for a prompt
  queued behind a turn is before the user message itself.
- **Turns.** The ticker reads the last 1 MiB of the file each pass for the last
  assistant message whose `stopReason` is `stop`, `length`, `error` or
  `aborted`.
- **Resume.** `--session-id <bead>` opens the worktree's session with that id
  or creates it, so a resume after the pane or the machine went away picks up
  the bead's own conversation, not "the most recent session in this folder".

## Approvals: guards that block, and ask only when told to

pi asks before nothing: its `docs/security.md` says it "does not ask for
approval before every tool call", and that the only boundary is what the
process can reach. A quartermaster worker is unattended, so a prompt in its
pane would only stall it. pi workers therefore run without pane approvals.
What replaces them is quartermaster's extension, `pi/quartermaster.ts`
(embedded in the binary, written to `<state dir>/pi/quartermaster.ts`, and
passed with `-e`):

- **Bash hooks.** It runs the PreToolUse hooks Claude Code workers run
  (`~/.claude/hooks/block-dangerous-bash.py` and `rm-guard.sh`, or the
  colon-separated `QUARTERMASTER_PI_BASH_HOOKS`) with the JSON they expect.
  Exit 2 or `deny` blocks the call. So does `ask`, unless the command is on
  the ask list (next). The reason it returns tells the worker to hand the
  step over as it would a Claude Code denial: a `RUN:` note, `needs_me`, and
  stop.
- **Ask-gates.** A command a hook would ask about, which matches a pattern in
  `[agents.pi] ask`, waits for quartermaster instead. The worker shows as
  blocked; the inbox gets "at an approval prompt [hash]" with the command,
  the hook's reason and two options, `1 Yes, run it` and `2 No, block it`;
  `quartermaster prompt <bead>` shows it and
  `quartermaster answer <bead> <n> --expect <hash> [--class N]` answers it,
  with the same autonomy check as a Claude prompt. The session records
  `quartermaster.ask` and `quartermaster.answer` custom entries. With no
  answer within `ask_timeout` (default 10m), or with no control socket, it
  blocks with the usual hand-off. The list starts empty, so nothing asks
  until you add to it; a hook's `deny` is never asked about.

  ```toml
  [agents.pi]
  ask = ['^git push --force(-with-lease)? origin tj/']  # JavaScript regexps on the bash command
  ask_timeout = "15m"
  ```
- **Subagents.** A `subagent` call is allowed only when every agent it names
  is read-only by its definition (`tools:` limited to read, grep, find, ls).
  A pi subagent is a separate pi process without this extension.
- **Dialogs.** An extension dialog is reported to herdr as blocked, so the
  thread shows as waiting on you; answer it in the pane.
- **Compaction.** For a beads worker, `bd prime` goes back into context after
  a compaction, like Claude Code's PreCompact hook.
- **Structured worker tools (`ask_user`, `report_milestone`).** When the
  extension loads in a worker session (`QUARTERMASTER_BEAD`, `--session-id`, or
  `--quartermaster-control`, and never the coordinator) and `pi.registerTool`
  is available, it registers two first-class tools backed by `quartermaster card`:
  - `ask_user`: accepts structured `{ question, details?, options, class,
    class_reason?, recommendation?, critical?, evidence?, iac_preview?,
    iac_target?, iac_expect? }`, invokes `quartermaster card ask <bead> --file - --json`
    (persisting `<state dir>/cards/<bead>/question.json`, mirroring a
    tap-compatible `QUESTION:`/`OPTIONS:`/`CLASS:` note, setting `needs_me`,
    and never lowering an attached IaC preview's class), appends a
    `quartermaster.ask_user` custom session entry, and returns the recorded
    card hash with a reminder to stop and wait for the answer.
  - `report_milestone`: accepts `{ phase, status?, summary, evidence? }`,
    invokes `quartermaster card milestone <bead> --file - --json` (appending to
    `<state dir>/cards/<bead>/milestones.jsonl` and mirroring a `MILESTONE`
    bead note), appends a `quartermaster.milestone` custom session entry, and
    returns confirmation. See [workflows and cards](workflows.md).

### The control socket

quartermaster starts a local pi worker with
`--quartermaster-control <state dir>/pi/control/<bead>.sock`. The extension
listens there and writes a random token to `<bead>.sock.token`; both are mode
600 in a 700 directory, and every request must carry the token. quartermaster
is always the client, one JSON line each way (`picontrol.go`):

| op | does |
|---|---|
| `prompt` | `pi.sendUserMessage`: at once when idle, else as a follow-up that pi delivers when the running turn ends. Recorded as source `quartermaster`. |
| `ask` | the ask waiting on an answer, or none |
| `answer` | answers it by id with an option number |
| `ping` | whether pi is idle |

`<state dir>/pi/control/config.json` carries the ask list and timeout,
written from `[agents.pi]` on each pi start and read on each ask. The token
keeps out other users and stale clients; it does not keep out the worker,
which runs as the same user, any more than a pane does. An answer still goes
through `quartermaster answer`'s autonomy check. If the socket is missing or
doesn't answer, quartermaster types into the pane and the guards block, as
before. This fallback applies only to workers; the coordinator has a separate
[notification-only channel](pi-coordinator-inbox.md), with durable coalescing,
editor-aware delivery and no terminal fallback. That page also covers safe
user-issued activation for an already-running coordinator.

What this does not replace is Claude Code's auto-mode classifier, which judges
intent. The hooks match known shapes (recursive deletes outside the worktree,
force pushes, `kubectl delete`, `terraform destroy`); they don't catch an
apply, a production change or a message to a customer by intent, and shell
indirection gets past patterns. The extension runs in the worker's own
process and user, so a worker that edits it can disable it. Keep pi workers
on code-only beads until they run in an isolated environment with only the
credentials the bead needs.

## MCP for a pi coordinator

pi reads MCP servers from `~/.pi/agent/mcp.json` (`pi mcp add`; `/mcp` inside
pi signs in to OAuth servers). Claude Code's hosted connectors belong to the
Claude account and don't carry over; each needs its own server and sign-in in
pi. None is configured here.

| Connector today | pi-reachable MCP | pi configuration | Without it |
|---|---|---|---|
| Chat (Slack) | Slack's hosted MCP server (OAuth); availability depends on the workspace's admin settings | `pi mcp add slack --url https://mcp.slack.com/mcp`, then `/mcp` to sign in | none in bash |
| Issue tracker (Linear) | Linear's hosted server (OAuth) | `pi mcp add linear --url https://mcp.linear.app/mcp`, then `/mcp` | the `linear` CLI, which the ticker already uses |
| Docs (Notion) | Notion's hosted server (OAuth) | `pi mcp add notion --url https://mcp.notion.com/mcp`, then `/mcp` | — |
| Docs and spreadsheets (Google) | no first-party server confirmed | — | the `gws` CLI through bash |
| Deploy tool (internal) | only if it exposes an MCP URL pi can reach with a token | `pi mcp add deploy --url <its MCP URL> --bearer-token-env-var <VAR>` | its own CLI or API, if any |
| Cluster, read-only | `kubernetes-mcp-server --read-only` (stdio, uses kubeconfig) | `pi mcp add k8s -- npx -y kubernetes-mcp-server@latest --read-only` | `kubectl get` through bash |
| Metrics and logs (Grafana, VictoriaMetrics) | `mcp-grafana` (stdio, service-account token); the VictoriaMetrics MCP servers (stdio) | `pi mcp add grafana --env GRAFANA_URL=… --env GRAFANA_SERVICE_ACCOUNT_TOKEN='${VAR}' -- mcp-grafana` | — |
| Incidents (PagerDuty) | PagerDuty's hosted server, as Claude Code already uses | `pi mcp add pagerduty --url https://mcp.pagerduty.com/mcp --bearer-token-env-var <VAR>` | — |
| Shared agent memory | the local stdio server Claude Code runs | `pi mcp add memory -- ~/.local/bin/mcp-memory-stdio`, with `--exclude-tools` for its delete, cleanup and consolidate tools in `[agents.pi] args` | — |
| Browser | `chrome-devtools-mcp` or the Playwright MCP server (stdio) | `pi mcp add browser -- npx -y chrome-devtools-mcp@latest` | — |

The hosted URLs above are the vendors' published endpoints as of this
writing; confirm each with `pi mcp list` after adding it. The coordinator's
own work (`quartermaster`, `bd`, `herdr`, `gh`) is plain bash and needs no
MCP.

## Setup only you can do

1. Configure at least one provider in the auto-model chain (all three for
   full failover): the Antigravity bridge for `antigravity/gemini-4-neon-xhigh`,
   `pi auth login anthropic` (account sign-in, or `--api-key`), and
   `pi auth login openai-codex`. Check with `pi --list-models` and
   `pi auth check --provider <p>` (`ready`). The model check refuses an
   auto-model start when no physical model in the chain is usable; a
   physical `--model` in `[agents.pi]` needs only its own.
2. Add Fable 5.2, which older pi catalogs lack, under `providers` in
   `~/.pi/agent/models.json`, next to any provider already there:

   ```json
   "anthropic": { "models": [
     { "id": "claude-fable-5-2", "name": "Claude Fable 5.2", "reasoning": true,
       "input": ["text", "image"], "contextWindow": 1000000, "maxTokens": 128000 }
   ] }
   ```

   Then `pi --list-models claude-` should list `claude-opus-5-5` and
   `claude-fable-5-2` under `anthropic`.
3. MCP sign-ins from the table above, for the connectors the coordinator
   needs.
4. Optional: `ln -s ~/.claude/CLAUDE.md ~/.pi/agent/AGENTS.md`, so pi reads the
   same global instructions; pin pi's version in mise rather than `pi@latest`,
   so a new pane doesn't start a different pi.

## What still assumes Claude Code

- **The default close verifier** still runs `claude -p`; the [pi backend](pi-verifier.md) is a separate opt-in, independent of the worker kind.
- **Approval prompts.** `quartermaster prompt` and `answer` parse Claude Code
  and Codex screens, and read a pi worker's ask-gates from its control
  socket. A pi extension dialog shows the thread as blocked with no hash to
  answer; answer it in the pane.
- **The auto-mode classifier** has no pi equivalent (see above).
- **The brief's last line** names Claude Code permission prompts and the
  classifier. For pi, the extension's block reason carries the same
  instruction.
- **Relaunch on a level change** sends `/exit`; pi's command is `/quit`. It
  only happens for a worker started with bypass args, which pi has none of.
- **A dead pane record after a reboot** (herdr restores the pane's name with
  no process) blocks dispatch and resume for every kind, pi included; that is
  a separate fix. A pi worker that quits leaves no such record.
- **Remote hosts.** The model check runs on the host, but the extension is a
  local file, so a remote pi worker starts without it, and without a control
  socket.
- **`worker_agent = "auto"`** never picks pi: it records no usage reading.
- **Ticker messages** read as `interactive` when typed and `quartermaster`
  over the socket; either way a ticker prompt to a needs_me worker resumes
  it, as it does for Claude. The ticker holds them while a bead is needs_me.
- **Claude Code only:** global `~/.claude/CLAUDE.md`, skills under
  `~/.claude/skills`, the memory-recall SessionStart hook, the hosted
  connectors, and tools such as Artifact, ScheduleWakeup, Monitor and the
  Explore subagent.
