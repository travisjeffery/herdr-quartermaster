# Workers and the coordinator on pi

[pi](https://github.com/earendil-works/pi) is an opt-in fourth agent kind next
to `claude`, `codex` and `muse`: `--agent pi`, `worker_agent = "pi"` or
`coordinator_agent = "pi"`. Nothing changes until you ask for it. This page
covers what works under pi, how it differs from Claude Code, and the setup
steps only you can do. Checked against pi 1.5.2 and herdr 0.9.5.

## What works

| | How |
|---|---|
| Start | `herdr agent start --kind pi`; herdr's own pi extension reports working, idle and blocked. |
| Models | Workers `anthropic/claude-opus-5-5`, coordinator `anthropic/claude-fable-5-2`; `[agents.pi] args` and `coordinator_args` replace them. |
| Model check | Before starting pi, quartermaster runs `pi --list-models <id>` (with the args' `-e` extensions) and, if the model isn't listed, `pi auth check --provider <p>`, and refuses with the fix. Neither prints a credential. Dispatch checks before it creates the worktree or claims the bead. |
| Brief | Typed in with `herdr agent prompt`, as for Claude. pi has no SessionStart hook; the brief already says to run `bd prime` first. |
| Sessions | A worker runs `pi --session-id <bead>`, so its session file is `~/.pi/agent/sessions/--<worktree>--/<time>_<bead>.jsonl`. Dispatch and resume open that session by id. |
| needs_me wake check | Read from the session file: a prompt after the worker settled resumes the bead, pi waking itself doesn't (below). |
| Finished turns | The id of the session's last turn-ending assistant entry is kept in the ticker's snapshot; a new id is a finished turn even if no pass saw the agent working, and an `error` or `aborted` turn is reported as `stopped on error: <message>`. |
| Guards | quartermaster's pi extension, loaded into every pi it starts (below). |
| Close verifier | Unchanged: `claude -p`, whatever the worker's kind. |

## Durable sessions, and why the file rather than RPC

pi stores a conversation as an append-only JSONL tree of entries with stable
ids (pi's `docs/session-format.md`); `docs/rpc-commands.md` calls an entry id
"a durable cursor" (`get_entries` with `since`). quartermaster uses the same
property without RPC: herdr's pi extension reports the session file's path
(`agent_session.kind = "path"`), and the ticker reads it.

RPC mode (`pi --mode rpc`) would replace the TUI that the worker's herdr pane
shows and that herdr's state extension reports from, and would need a client
holding pi's stdin and stdout. The file is pi's own durable record, readable
whether pi is running or not. That is the smallest thing that is correct.

- **Wake check.** pi's user messages carry no origin, so quartermaster's
  extension appends a `quartermaster.input` custom entry for each prompt with
  pi's input source: `interactive` (typed, which is how `herdr agent prompt`
  delivers), `rpc`, or `extension` (`pi.sendUserMessage`). Custom entries
  never reach the model. A needs_me worker that starts working again resumes
  the bead only if an `interactive` or `rpc` entry falls after it settled.
  Without those entries, any user message after it settled counts.
- **Turns.** The ticker reads the last 1 MiB of the file each pass for the last
  assistant message whose `stopReason` is `stop`, `length`, `error` or
  `aborted`.
- **Resume.** `--session-id <bead>` opens the worktree's session with that id
  or creates it, so a resume after the pane or the machine went away picks up
  the bead's own conversation, not "the most recent session in this folder".

## Approvals: none, by design

pi asks before nothing: its `docs/security.md` says it "does not ask for
approval before every tool call", and that the only boundary is what the
process can reach. A quartermaster worker is unattended, so a prompt would
only stall it. pi workers therefore run without interactive approvals, and
`quartermaster prompt` and `answer` have nothing to answer for them.
What replaces them is quartermaster's extension, `pi/quartermaster.ts`
(embedded in the binary, written to `<state dir>/pi/quartermaster.ts`, and
passed with `-e`):

- **Bash hooks.** It runs the PreToolUse hooks Claude Code workers run
  (`~/.claude/hooks/block-dangerous-bash.py` and `rm-guard.sh`, or the
  colon-separated `QUARTERMASTER_PI_BASH_HOOKS`) with the JSON they expect.
  Exit 2, `deny` or `ask` blocks the call. The reason it returns tells the
  worker to hand the step over as it would a Claude Code denial: a `RUN:`
  note, `needs_me`, and stop.
- **Subagents.** A `subagent` call is allowed only when every agent it names
  is read-only by its definition (`tools:` limited to read, grep, find, ls).
  A pi subagent is a separate pi process without this extension.
- **Dialogs.** An extension dialog is reported to herdr as blocked, so the
  thread shows as waiting on you; answer it in the pane.
- **Compaction.** For a beads worker, `bd prime` goes back into context after
  a compaction, like Claude Code's PreCompact hook.

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

1. Sign pi in to Anthropic: `pi auth login anthropic` (account sign-in), or
   `pi auth login anthropic --api-key`. Check with
   `pi auth check --provider anthropic` (prints `ready`).
2. Add Fable 5.2, which pi 1.5.2's catalog lacks, under `providers` in
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

- **The close verifier** runs `claude -p`, on purpose.
- **Approval prompts.** `quartermaster prompt` and `answer` parse Claude Code
  and Codex screens. A pi dialog shows the thread as blocked with no hash to
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
  local file, so a remote pi worker starts without it.
- **`worker_agent = "auto"`** never picks pi: it records no usage reading.
- **Typed ticker messages** read as `interactive`, so a ticker prompt typed
  into a needs_me worker resumes it, as it does for Claude.
- **Claude Code only:** global `~/.claude/CLAUDE.md`, skills under
  `~/.claude/skills`, the memory-recall SessionStart hook, the hosted
  connectors, and tools such as Artifact, ScheduleWakeup, Monitor and the
  Explore subagent.
