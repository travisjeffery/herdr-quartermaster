# Cloud workers: running workers in Claude Code and Codex cloud sessions

Status: **proposed** (2026-10-30). Supersedes the remote-host half of
[worker-hosts.md](worker-hosts.md); see "What happens to worker-hosts.md".
Nothing here changes behavior until the phases below land. The open
decisions are at the end.

## Summary

[worker-hosts.md](worker-hosts.md) planned to run workers on more machines on
the LAN (a second box, `server.local`, in a worker container with a headless
Herdr). This design drops the LAN machines and runs remote workers as **cloud
sessions** instead:

- **Claude Code on the web** (`claude.ai/code`): a full Claude Code session in
  an Anthropic-managed VM that clones the repo, pushes branches and keeps
  running with the desktop closed.
- **Codex cloud tasks** (`codex cloud`): a Codex run in an OpenAI-managed
  container that produces a diff.

The desktop keeps the control plane: the coordinator, the ticker, the Beads
database, the inbox, notifications, GitHub polling and everything that needs
the desktop's credentials or network. Quartermaster gains a **backend** per
thread (`local`, `claude-cloud`, `codex-cloud`) behind the operations it
already has (dispatch, observe, prompt, answer, resume, pull, resolve). The
bead records which backend it runs on, the way it records `host:` today.

The two cloud backends are not equal, and the design doesn't pretend they are:

| | `claude-cloud` | `codex-cloud` |
|---|---|---|
| Start a run from a script | yes, through a scheduled task's API trigger (`/fire`) | yes, `codex cloud exec --env … --branch …` |
| Send it a follow-up | yes, `claude -p "<msg>" --cloud <session>` | **no** (web UI only); a follow-up is a new task |
| Read its state or transcript | **no** documented API | status only (`codex cloud status`, `list --json`) |
| Push branches, open PRs | yes (REST only; GraphQL is blocked) | not documented: treat as **no** |
| Reach Beads | only through a relay we add (§5) | no |
| What it is | a **worker**: carries a bead to done | a **patch producer**: one task, one diff |

So `claude-cloud` replaces a remote host for most work, and `codex-cloud` is a
narrower tool for self-contained changes, with the desktop pushing its diffs.

## Goals and non-goals

Goals:

1. Dispatch a bead to a cloud session; the board, context, sidebar, inbox and
   coordinator treat it as a thread like any other.
2. Keep what the user relies on today (the capability table, §11), and say
   plainly what is lost and how to work around it.
3. Beads stays the only task state and the single source of truth.
4. No LAN machines to run, update or secure.
5. Cloud sessions hold no long-lived secrets of the user's.

Non-goals:

- Moving the coordinator, ticker or Beads off the desktop.
- Running admin, break-glass, kube/VPN, browser or production-apply work in
  the cloud (§10).
- A scheduler that picks cloud vs local on its own (manual first, §4).

## Phase 0 spike: what each backend can do

Checked 2026-10-30 against Claude Code 2.1.295 and codex-cli 0.153.2, their
`--help`, and the vendors' docs. "Verified" means run here; everything else
cites the doc. Items marked **live check** need a real session, which needs
the user's setup first (§13, phase 1).

### Claude Code on the web

Docs: `claude-code-on-the-web`, `cloud-environments`, `routines`,
`self-hosted-environments`, `remote-control` on code.claude.com.

1. **Start a session on a repo + branch with a brief.**
   - `claude --cloud "<task>"` creates a session that "clones your current
     directory's GitHub remote at your current branch, not your local
     checkout". **Verified:** `claude -p --cloud "<task>"` is refused:
     "Starting a new cloud session with --cloud is interactive only". So the
     CLI can start one only from a terminal.
   - **Scheduled tasks (formerly routines) with an API trigger** are the
     scriptable path: `POST https://api.anthropic.com/v1/claude_code/routines/<trig_id>/fire`
     with the task's bearer token and an optional `text`, which "returns a
     JSON body with the new session ID and URL"
     (`{"claude_code_session_id", "claude_code_session_url"}`). The `text`
     arrives "wrapped in a `<routine-fire-payload>` block that labels it as
     untrusted data", so the task's saved prompt must opt in to following it.
     The endpoint is in research preview behind a dated beta header.
   - Branch: a task "starts from the repository's default branch unless your
     prompt specifies otherwise" and "pushes its work to a branch prefixed
     with `claude/` unless your prompt directs it to push to another branch".
     The brief names the branch (**live check** that this holds for every
     run).
   - Limits: 30 fires an hour per task, 100 an hour per account; runs use the
     plan's normal usage.
2. **Status and transcript.** No documented API or CLI lists cloud sessions or
   reads their state or transcript. **Verified:** `claude agents --json` lists
   only local sessions. The session URL shows everything in a browser or the
   Claude app.
3. **Follow-ups.** `claude -p "<msg>" --cloud <session-id|url>` "queues the
   message into the session and exits without waiting for a reply", from any
   machine logged in to the same account. `--output-format json` returns
   `{ok, session_id, url}` or `{ok: false, session_id, error}`. An archived
   session refuses messages.
4. **Finished / PR.** No completion notification or webhook is documented.
   Sessions that create a PR are linked to it (`claude --from-pr`). With the
   Claude GitHub App on the repo, **auto-fix** can subscribe the session to the
   PR's CI failures and review comments (not to merge conflicts).
5. **Reach.**
   - GitHub goes through a proxy that keeps credentials server-side. The proxy
     **rejects every GraphQL request**, "regardless of the credentials you
     supply": `gh pr …`, `gh issue …`, and GraphQL-only APIs fail with 403.
     REST (`gh api repos/{o}/{r}/…`) works for repositories attached to the
     session. It refuses branch deletions and tag pushes.
   - Network: None / Trusted (package registries, GitHub, cloud SDKs) /
     Full / Custom allowlist, per environment. **Network secrets** are
     attached to matching outbound requests "after they leave the session",
     so the VM never sees them. MCP connector traffic goes through Anthropic
     and bypasses the allowlist; scheduled tasks include the account's
     connectors by default.
   - Config: repo `CLAUDE.md`, `.claude/settings.json` (hooks, permissions;
     single-repo sessions), `.claude/skills|agents|commands`, `.mcp.json`
     carry over. The user's `~/.claude/CLAUDE.md`, user skills, user hooks,
     user plugins and user-scope MCP servers **don't**. A setup script (root,
     Ubuntu 24.04, cached if it finishes in ~5 minutes) may write
     `~/.claude/CLAUDE.md`, which loads as user instructions. Skills enabled on
     claude.ai load automatically.
6. **Permissions.** Scheduled-task sessions start in auto mode; a run stops
   and waits on an `ask` rule or after auto mode's repeated-block threshold.
   The user answers in the session page or app. A session waiting on an MCP
   approval counts as idle and can expire.
7. **Cost and capacity.** Plan usage, shared with everything else; no
   per-VM charge. VM: about 4 vCPU, 16 GB RAM, 30 GB disk. Foreground commands
   default to 2 minutes (10 max) and move to the background for up to 30 more.
   An idle VM pauses, and a paused VM can be reclaimed (history kept, running
   background work lost). No numeric concurrency cap is documented; an org
   limit on concurrent sessions exists.
8. **Pull it local.** `claude --teleport <session>` fetches the session's
   branch and loads the conversation into the terminal (clean git state, same
   repo, branch pushed, same account). One-way: the local copy diverges from
   the cloud one. A local terminal session can't be pushed to the cloud.

Related but separate:

- **Remote Control** (`claude --remote-control`) lets claude.ai or the phone
  steer a session that runs **on the user's machine**. It's how to reach local
  workers from the phone; it doesn't move work off the desktop.
- **Self-hosted environments** run cloud sessions on runners the user's
  organization operates (`--environment ccpool_…`). They restore internal
  network access, runner-image tooling and the runner host's `~/.claude`, but
  they're a Team/Enterprise beta an org Owner turns on, and they bring back
  machines to run. Not part of this design; noted as a later option.

### Codex cloud

Docs: learn.chatgpt.com (the old developers.openai.com/codex pages redirect
there), `cloud`, `environments/cloud-environments`, `cloud/internet-access`,
`cli/reference`, `codex-sdk`, `third-party/github`, `pricing`.

1. **Start.** `codex cloud exec --env <ENV_ID> [--branch <BRANCH>]
   [--attempts N] <QUERY>` (**verified** in `--help`): `--env` is required, the
   branch defaults to the current one, the prompt can come on stdin, and it
   prints the task URL. Environments are created and published in the web UI;
   no API creates them.
2. **Status.** `codex cloud status <TASK_ID>` prints PENDING, READY, APPLIED
   or ERROR and exits non-zero unless READY. `codex cloud list --json` gives
   `id, url, title, status, updated_at, environment_id, summary{…},
   attempt_total`. `codex cloud diff <TASK_ID>` prints the diff. The
   transcript is web-only.
3. **Follow-ups.** None from the CLI or SDK: the SDK drives local threads,
   `codex queue` targets local sessions. Follow-ups happen in the web UI, or
   with `@codex` on a PR (which runs on the legacy cloud).
4. **Finished / PR.** Poll `status`. Opening a PR is a user step in the UI;
   pushing from inside a task isn't documented, nor are GitHub credentials in
   the container.
5. **Reach.** Internet during the agent phase is off by default on the legacy
   cloud; the current cloud allows package domains plus listed hosts, network
   secrets (substituted by a proxy) and Tailscale. Repo `AGENTS.md` and repo
   skills apply; personal skills don't. MCP in cloud isn't documented.
6. **Permissions.** Runs in an isolated container; no approval pauses are
   documented.
7. **Cost and capacity.** Shares the plan's allowance ("cloud tasks may use
   more"); 2 vCPU/8 GiB on Plus, 4 vCPU/16 GiB on Pro/Business/Enterprise.
   Concurrency and duration caps aren't documented.
8. **Pull it local.** `codex cloud apply <TASK_ID>` applies the diff to the
   local tree. No resume of a cloud task as a local session.

### What the spike changes

- `claude-cloud` dispatch goes through a scheduled task's API trigger, not
  `claude --cloud`.
- The ticker can't see a cloud session's state. It learns about a cloud
  thread only from what the worker writes (Beads, §5) and from GitHub.
- A Claude cloud worker can't use `gh pr …` or any GraphQL. Creating PRs,
  replying to threads and merging work over REST; **turning on auto-merge and
  resolving review threads are GraphQL-only**, so the desktop does them (§7).
- Codex cloud can't carry a bead through review. It produces a diff; the
  desktop pushes it and opens the PR, and each review round is a new task.

## Architecture

```
                         desktop (control plane, unchanged)
 ┌───────────────────────────────────────────────────────────────────────────────┐
 │ Herdr: coordinator pane, local worker panes                                   │
 │ quartermaster ticker ── observes every thread through its backend ──┐         │
 │   ├─ local:        herdr agent list/read/prompt (today)             │         │
 │   ├─ claude-cloud: Beads notes + GitHub; prompts via claude -p --cloud         │
 │   └─ codex-cloud:  codex cloud status/diff; applies, pushes, opens PRs         │
 │ Beads DB (one copy) ◀── quartermaster bd-relay (HTTPS, allowlisted bd) ◀──┐    │
 │ gh (GraphQL: auto-merge, resolve threads), Linear, AWS, kube, browser      │    │
 └───────────────────────────────────────────────────────────────────────────┼───┘
            ▲ outbound tunnel only (no inbound port on the desktop)            │
            │                                                                  │
 ┌──────────┴───────────────────┐   network secret added outside the VM ───────┘
 │ Claude cloud session (VM)    │── git/REST via GitHub proxy ──▶ GitHub
 │   bd shim → bd-relay         │── MCP connectors (Linear …) via Anthropic
 └──────────────────────────────┘
 ┌──────────────────────────────┐
 │ Codex cloud task (container) │── diff ──▶ desktop applies, pushes, opens PR
 └──────────────────────────────┘
```

## 1. Backends

A backend is how one thread runs. Quartermaster's commands stop calling
Herdr directly for worker operations and go through:

```go
type Backend interface {
	Name() string // "local", "claude-cloud", "codex-cloud"

	// Dispatch starts a worker for the bead with the brief and returns the
	// handle to record on the bead.
	Dispatch(b Bead, br Brief) (Handle, error)

	// Observe reports each thread's state for the ticker's 15s pass. Backends
	// that can't see state return Unknown and let Beads/GitHub speak.
	Observe(hs []Handle) map[string]Observation

	// Prompt delivers a message (ticker events, the coordinator's relays).
	Prompt(h Handle, text string) error

	// Approval prompts: only local can read and answer them.
	ReadPrompt(h Handle) (ApprovalPrompt, error)
	Answer(h Handle, choice, expect string) error

	Resume(h Handle) error  // restart an exited agent
	Pull(h Handle) error    // bring the thread to a local worktree (§9)
	Resolve(h Handle, force bool) error
}
```

| Operation | `local` (today) | `claude-cloud` | `codex-cloud` |
|---|---|---|---|
| Dispatch | herdr worktree + agent + brief | fire the scheduled task with the brief; record `SESSION: <url>` | `codex cloud exec --env … --branch <base> -`; record `TASK: <url>` |
| Observe | herdr agent state (working/idle/blocked/done) + screen | **Unknown** state; liveness from the relay's last-seen time and GitHub pushes (§6) | `codex cloud status` |
| Prompt | `herdr agent prompt` once idle | `claude -p --cloud <session>` (queues; no idle wait needed) | a **new task** on the PR branch with the message as its prompt (§8) |
| ReadPrompt / Answer | screen parse + hash (`quartermaster prompt/answer`) | not available: the user answers in the session page | n/a |
| Resume | `claude --continue` in the worktree | send a follow-up (reopens a reclaimed VM: **live check**) | n/a: re-run |
| Pull | n/a | `claude --teleport` in a new local worktree; thread becomes `local` | `codex cloud apply` in a local worktree |
| Resolve | remove worktree, delete merged branch | archive nothing (no API); clear the handle; branch deleted by GitHub on merge | remove the local apply worktree |

The handle lives **on the bead**, as `host:` does today:

- label `backend:claude-cloud` or `backend:codex-cloud` (absent = `local`);
- a note `SESSION: https://claude.ai/code/session_…` or `TASK: <url>`, parsed
  like `PR:` notes (latest wins).

`resume`, the board, `context` and a fresh coordinator all read it from there.

## 2. Configuration

```toml
# ~/.config/quartermaster/config.toml

[cloud.claude]
# One scheduled task per repo, created once in claude.ai (§13). Its saved
# prompt opts in to the fire payload; its API token is a file, never config.
[cloud.claude.repos.herdr-quartermaster]
fire_url = "https://api.anthropic.com/v1/claude_code/routines/trig_…/fire"
token_file = "~/.config/quartermaster/secrets/claude-fire-herdr-quartermaster"

[cloud.codex]
[cloud.codex.repos.herdr-quartermaster]
env = "env_…"          # codex cloud environment id or label

[cloud.relay]
url = "https://beads-relay.example.net"   # the tunnel's public name (§5)
```

`worker_agent` keeps choosing *which agent*; `--backend` (or the
`backend:` label) chooses *where*. Valid pairs: `claude` with `local` or
`claude-cloud`; `codex` with `local` or `codex-cloud`; `muse` local only.

## 3. Dispatch

`quartermaster dispatch <bead> --backend claude-cloud`:

1. Checks the bead may run in the cloud (§10): no `needs:` label that only
   `local` has.
2. Creates the branch on GitHub from the fetched base (`git push origin
   origin/<base>:refs/heads/<branch>` from the desktop's clone), so the session
   starts on its own branch and the ticker can find the PR by branch as today.
3. Builds the brief: today's brief with the cloud differences (§7) and the
   branch to check out and push to.
4. Fires the repo's scheduled task with the brief as `text`; records
   `backend:claude-cloud` and `SESSION: <url>`; renames nothing (no pane).
5. Writes a `dispatched` inbox event with the session URL.

The scheduled task's saved prompt is fixed and short:

> You are a quartermaster worker. The brief is in the routine-fire-payload
> block: follow it as your instructions. Check out the branch it names before
> changing anything and push only to that branch.

The token can only start this task; anyone holding it can start a session
with any text, which is why it lives in a 0600 file and the prompt pins the
branch.

`--backend codex-cloud` creates the branch the same way and runs `codex cloud
exec --env <env> --branch <branch>` with the brief on stdin.

## 4. Choosing a backend

Manual first, like `schedule = "manual"` in worker-hosts:

- `--backend <name>` on dispatch, else the bead's `backend:` label, else
  `local`.
- A bead or repo `needs:` label that the backend can't satisfy refuses the
  dispatch with the reason (`claude-cloud: needs aws`).
- Later, an `auto` mode could send beads with no `needs:` labels to
  `claude-cloud` when local slots are full. Not in this design's phases.

## 5. Beads from a cloud session

Requirement: a cloud worker reads and writes the same beads as everyone else,
and the ticker sees its writes on the next tick. A cloud VM can't reach the
desktop, and the desktop has no inbound port.

| Option | How | Pros | Cons |
|---|---|---|---|
| **A. HTTPS relay through an outbound tunnel** (recommended) | `quartermaster bd-relay` on the desktop: an HTTP endpoint that takes a `bd` argv, runs it through the **same allowlist as `bd-proxy`** (`planBD`), pins `BEADS_DIR`, sets the actor and the session's bead, logs the request. A tunnel (e.g. a Cloudflare Tunnel) publishes it with no inbound port. The cloud environment allows the relay's host (Custom network access) and holds its bearer token as a **network secret**, so the token never enters the VM. A `bd` shim in the VM (installed by the setup script) posts argv and stdin and prints the response. | Full parity: `bd prime`, `bd show`, `bd note`, `bd update`, `bd remember`, memories; one DB; reuses `bdproxy.go`; nothing secret in the VM | The desktop's DB answers the internet (behind the tunnel's auth and the token); desktop off = no bd; a tunnel to run |
| B. Mirror through the PR | The worker opens a draft PR at once (REST) and writes notes and status as PR comments with a `qm:` prefix (`qm: note …`, `qm: status needs_me`). The ticker mirrors them into the bead and marks them mirrored. The brief carries the bead's description, notes and the relevant memories. | No endpoint on the desktop; works with the desktop off (mirrored later) | Writes only, delayed by the 60s GitHub poll; no `bd show` of other beads, no `bd prime`, no claims; PR noise; a draft PR for every bead |
| C. `bd serve` directly | Publish `bd serve`'s HTTP API through the tunnel | Built for automation clients | Its API is the whole surface (no allowlist), and agents use the CLI, so a shim to translate is still needed |
| D. Copy of the DB in the VM | Dolt remote sync | — | Two sources of truth; claims race. **Rejected** in worker-hosts already |

**Recommendation: A**, with B's mirror kept as a fallback the ticker can use
when the relay is down (a worker told by the shim that bd is unreachable
writes the `qm:` comment instead).

Relay details:

- Authentication: the tunnel's own access check (a service token on the
  network secret's header) plus the relay's bearer token. Two network
  secrets on the same host are fine.
- Scope: the `bd-proxy` allowlist (no `dolt`, `sync`, `init`, `gc`,
  `config set`, no `--db`/`--directory`/profile flags). The relay also offers
  `quartermaster autonomy check <bead>` read-only, so autonomous beads still
  check their grant.
- Identity: the shim sends the session's bead (from the brief, exported by
  the setup script's profile as `QM_BEAD`) and the relay sets `--actor
  cloud:<bead>`. The relay can't verify which session sent a request; any
  holder of the environment can reach it, which is the same trust as any
  worker today (full read/write on all beads).
- Liveness: each request records `last_seen` per bead, which the ticker uses
  as the cloud thread's heartbeat (§6).
- Logged like `bd-proxy.log`: time, bead, subcommand, verdict, never the
  arguments.
- The desktop asleep is a failure mode (§14).

## 6. The ticker and cloud threads

What the ticker reads per cloud thread, every pass:

- the bead (notes, status, labels) as today: that's where a cloud worker
  speaks;
- GitHub every 60s as today: PR by branch or `PR:` note, checks, reviews,
  runs, merge state;
- the relay's `last_seen` for the bead and the branch's last push time.

State on the board and sidebar: `cloud · <last activity>` (`cloud · 3m ago ·
PR #12 checks running`) instead of herdr's working/idle. A thread with no bd
call, push or PR activity for `cloud_quiet_minutes` (30) gets a `cloud_quiet`
inbox item with the session URL: it's either waiting at a prompt, waiting on
the user in chat, or its VM was reclaimed. The user opens the URL; nothing is
inferred beyond "quiet".

Prompts: the ticker's events (checks failing, review feedback, ready to
merge, merged, run finished) go out with `claude -p --cloud <session>`, with
the same `[quartermaster: automated …]` prefix. Messages queue in the
session, so the "wait until idle 60s" rule doesn't apply; batching still
does. A send that fails with "archived" or "not found" becomes an inbox item.

`agent_gone` has no cloud equivalent (nothing to see go). `finished` is
replaced by the worker's own notes.

## 7. What changes in the brief

The brief stays one text; the cloud parts are appended for cloud backends.

For `claude-cloud`:

- **Branch:** "Check out `<branch>` (it exists on origin) and push only to it."
- **No GraphQL:** "GitHub's GraphQL API is blocked here, so `gh pr …` and
  `gh issue …` fail. Open the PR with `gh api repos/{o}/{r}/pulls -f head=… -f
  base=… -f title=… -F draft=…`; comment and reply with the REST endpoints."
- **Auto-merge:** can't be turned on from the session. "Note `AUTO-MERGE: on`
  (or `off`) on the bead; the ticker turns it on or off from the desktop."
  The ticker already prompts when auto-merge goes off; for cloud threads it
  acts on the note itself. Merging an approved, green PR with a pinned head
  (`--match-head-commit`) works over REST (`PUT /pulls/{n}/merge` with `sha`),
  so the autonomy rules for merging are unchanged.
- **Review threads:** "Reply on the thread over REST, then note
  `RESOLVE: <thread url>`; the ticker resolves it." The rule that a thread is
  resolved only after a fix or a reply stays with the worker; the ticker only
  executes it, and only on the user's own PRs.
- **What isn't here:** AWS, kube, VPN, the browser, admin flows. "If the work
  needs any of these, note `NEEDS LOCAL: <why>` and set `needs_me`." The
  coordinator then offers `quartermaster pull <bead>` (§9).
- **`RUN:`** is unchanged: the user runs the command on the desktop.

For `codex-cloud`, the brief is a task brief, not a thread brief: the change
to make, the branch it starts from, and "write `.quartermaster/report.md`
with your findings, decisions and anything the user must decide; don't open a
PR". The desktop strips that file from the diff and turns it into bead notes
(§8).

## 8. The codex-cloud flow

1. Dispatch runs `codex cloud exec`; the bead gets `TASK: <url>`.
2. The ticker polls `codex cloud status` (or `list --json` once per pass for
   all tasks).
3. READY: the desktop applies the diff in a local worktree on the bead's
   branch (`codex cloud apply`), moves `.quartermaster/report.md` into a bead
   note, commits (signed, with the desktop's key), pushes, and opens the PR
   with the Linear key in the title. Auto-merge on, as for any PR.
4. Review feedback, failing checks: a new `codex cloud exec --branch <pr
   branch>` with the feedback as the prompt; READY applies and pushes again.
   Each round is a new task with no memory of the last beyond the code and
   the PR.
5. ERROR, or a diff that doesn't apply: an inbox item; the coordinator
   re-dispatches locally or to `claude-cloud`.

The local apply worktree holds no agent; it's resolved like any other.

## 9. Pulling a thread local

`quartermaster pull <bead>` moves a cloud thread to the desktop when it needs
something only the desktop has, or the user wants to drive it:

- `claude-cloud`: create a local worktree on the bead's branch, start a Herdr
  pane named after the bead running `claude --teleport <session>`, set
  `backend:` to local, note `PULLED: local from <session url>`. Teleport needs
  a clean tree and the branch pushed, both true for a fresh worktree. The cloud
  session is then stale; the brief told the worker to stop when it sees
  "pulled local" (the ticker sends that message before teleporting).
- `codex-cloud`: `codex cloud apply` into the local worktree and start a local
  agent with the bead's brief.

The reverse (local to cloud) has no handoff: dispatch to the cloud again from
the pushed branch, as a new session.

## 10. What stays local

By label: a bead or repo with any of these needs runs on `local` only, and
dispatch refuses a cloud backend for it (`--force` doesn't override these):

| `needs:` | Why |
|---|---|
| `admin-ok` | break-glass and admin cloud credentials never leave the desktop |
| `aws`, `kube` | SSO sessions and kubeconfigs are on the desktop; the cloud has no AWS credentials and no route to private cluster APIs |
| `vpn` | private networks (zero-trust client) are on the desktop |
| `browser` | Chrome automation |
| `prod-apply` | production applies run where the user can see them |
| `signing` | releases that sign tags with the user's own key |

Always local, whatever the bead: the coordinator, ticker, inbox, notifications,
the Beads DB and its backups, GitHub GraphQL actions (auto-merge, resolving
threads), Linear moves done by the ticker, verify, stale checks.

The worker-hosts AWS credential broker is **not** carried over: brokering
AWS credentials to a VM on the internet is a larger exposure than to a LAN
box, and nothing here needs it.

## 11. Capabilities: today vs cloud

`local` is today; the other columns say what happens to each capability for a
cloud thread.

| Capability today | `claude-cloud` | `codex-cloud` | Mitigation |
|---|---|---|---|
| Dispatch with a brief | **kept** (fire API) | **kept** (`cloud exec`) | — |
| Worker named after the bead in the sidebar | **changed**: no pane; the board/sidebar list the thread from the bead | **changed**: same | board and context rows from the bead; session/task URL on the row |
| Bead notes, status, `needs_me`, `RUN:` | **kept** via relay | **changed**: once per task, from `report.md` | relay (§5); PR-comment mirror as fallback |
| `bd prime`, memories, `bd remember` | **kept** via relay | **lost** | brief includes the bead's memories by keyword |
| Ticker sees working/idle/blocked | **lost** | **changed**: PENDING/READY/ERROR | relay heartbeat + push times; `cloud_quiet` item with the URL |
| Approval prompts read and answered from the coordinator (`prompt`/`answer`) | **lost** | n/a | auto mode makes them rare; `cloud_quiet` points the user at the session; answer in the app |
| Ticker prompts on checks, reviews, merge, runs | **kept** (`claude -p --cloud`, queued) | **changed**: each prompt is a new task | — |
| Coordinator relays answers to the worker | **kept** (`quartermaster prompt` routes by backend) | **changed**: new task | — |
| PR found by branch or `PR:` note | **kept** | **kept** (desktop opens it) | — |
| Auto-merge on every non-draft PR | **changed**: ticker turns it on from an `AUTO-MERGE:` note | **kept** (desktop) | — |
| Resolve review threads after fix/reply | **changed**: worker replies, ticker resolves on `RESOLVE:` | **changed**: desktop | — |
| Merge with `--match-head-commit` (autonomy) | **kept** over REST | **kept** (desktop) | — |
| `@codex`/bot reviews told apart from human | **kept** (ticker) | **kept** | — |
| Actions runs followed, `rolling-out` | **kept** | **kept** | — |
| Linear issue moves | **kept** if the Linear connector is on the scheduled task (**live check**); ticker lists moves as today | **changed**: desktop | ticker/coordinator moves them from the desktop |
| Autonomous mode (`autonomy check`) | **kept** via the relay | n/a (no decisions in a task) | — |
| `resume` after Herdr restarts | **not needed** (session outlives the desktop); reclaimed VM reopens on a message (**live check**) | n/a | — |
| Focus a worker's pane (`↵`, clipboard) | **changed**: opens the session URL | **changed**: opens the task URL | `xdg-open` the URL |
| `resolve` / `sweep` | **changed**: nothing local to remove | **kept** for the apply worktree | — |
| Global `CLAUDE.md`, user hooks, user skills, plugins | **changed**: not carried | **changed**: personal skills not synced; repo `AGENTS.md` applies | §12: setup script writes `~/.claude/CLAUDE.md`; claude.ai skills; repo-committed skills/hooks where they belong |
| SessionStart `bd prime` hook | **changed** | **lost** | the brief tells the worker to run `bd prime` (it already does) |
| User-scope MCP servers | **lost** (claude.ai connectors only) | **lost** | connectors on the scheduled task |
| Signed commits | **live check** (docs say signing keys stay outside the sandbox) | **kept** (desktop commits) | if unsigned and a repo requires signing: that repo stays local or uses `codex-cloud` |
| AWS, kube, VPN, browser, admin | **lost** by design | **lost** | `needs:` labels keep them local; `pull` (§9) |
| Builds and tests | **kept** (4 vCPU/16 GB, Docker available) | **kept** (VM size by plan) | Bash timeouts raised per environment (`BASH_DEFAULT_TIMEOUT_MS`) |
| Work continues with the desktop off | **gained** (except bd writes) | **gained** (until READY) | PR-comment mirror for notes while the relay is down |
| Steer from the phone | **gained** (Claude app) | **gained** (ChatGPT app) | Remote Control does the same for local workers |
| No machines to run | **gained** | **gained** | — |

## 12. Getting user config into cloud sessions

What carries over is the repo (§Phase 0, 5). For the rest:

- **Global `CLAUDE.md`:** `quartermaster cloud setup-script` renders a setup
  script that writes `~/.claude/CLAUDE.md` from the user's dotfiles (the
  generic parts: working conventions, Beads rules) and installs the `bd`
  shim. The user pastes it into the cloud environment (no API to set it);
  re-rendering is how it's updated. The setup script is visible to anyone
  using the environment, so it carries no secrets.
- **Skills:** enable the ones workers need on claude.ai, which cloud sessions
  load automatically; repo-specific skills belong in the repo's
  `.claude/skills`.
- **Hooks:** user-level hooks don't run. **Live check** whether a
  `~/.claude/settings.json` written by the setup script is honoured; if not,
  hooks that matter for every user of a repo go in the repo's
  `.claude/settings.json`, and the rest are instructions in the brief.
- **MCP:** claude.ai connectors (on by default for scheduled tasks; trim the
  list to what workers need).
- **Codex:** repo `AGENTS.md` and repo skills; the task brief carries the
  rest.

## 13. Setup the user does once

| # | Step | Who |
|---|---|---|
| 1 | Install the Claude GitHub App on each repo that will run cloud workers (needed for cloning without bundle uploads, PR linkage and auto-fix) | **you** |
| 2 | Create a cloud environment `qm-worker`: Custom network access (defaults + the relay host), env vars for Bash timeouts, the setup script from `quartermaster cloud setup-script` | **you** (UI) |
| 3 | Add the relay's network secrets to that environment (tunnel service token header, relay bearer token) | **you** (UI; values from step 5) |
| 4 | Per repo: a scheduled task with the fixed prompt (§3), that repo, the `qm-worker` environment, the connectors workers need, an **API trigger**; save its token to `~/.config/quartermaster/secrets/claude-fire-<repo>` (0600) and its fire URL to config | **you** (UI) |
| 5 | Tunnel for the relay: create it, route a hostname to `quartermaster bd-relay` on localhost, require a service token; quartermaster prints the relay token | **you** (tunnel login), rest scripted |
| 6 | Codex: a cloud environment per repo; put its id in config | **you** (UI) |
| 7 | Smoke: `quartermaster dispatch <no-op bead> --backend claude-cloud`; check it notes through the relay, opens a draft PR over REST, gets a ticker prompt, and closes | scripted |

## 14. Failure modes

| Failure | Effect | Handling |
|---|---|---|
| Desktop off or asleep | Cloud workers keep coding; `bd` fails; no ticker prompts | Shim says so; worker falls back to `qm:` PR comments, mirrored when the ticker returns; ticker holds a sleep inhibitor while cloud threads are active |
| Relay or tunnel down | Same as above for bd | Ticker health-checks the relay through the tunnel each pass and writes one `relay_down` item |
| VM reclaimed while idle | Background commands lost; history kept | `cloud_quiet` item; a ticker prompt or the user's message reopens it (**live check**) |
| Session waiting at a permission prompt | Thread stalls; ticker can't see why | `cloud_quiet` with the URL |
| Fire rate limit (30/h per task, 100/h account) | Dispatch fails | Dispatch reports it; the coordinator retries later or dispatches locally |
| `/fire` beta changes | Dispatch breaks | Header pinned in one place; fall back to the user starting `claude --cloud` in a pane (interactive) |
| Fire token leaked | Anyone can start a session in the task's repo with arbitrary text | Saved prompt pins branch; payload arrives marked untrusted; rotate in the UI |
| Codex diff doesn't apply | Task wasted | Inbox item; re-dispatch |
| Plan usage exhausted | Sessions stop | Same as local today; `worker_agent = auto` already weighs usage |

## 15. Security

- The VM holds no user secrets: GitHub credentials stay in Anthropic's proxy,
  the relay token and tunnel token are network secrets added outside the VM.
- The relay is the one new inbound path to the desktop. It's reached only
  through the tunnel, needs two tokens, runs only allowlisted `bd`
  subcommands, logs every request, and is off when the ticker isn't running.
  Its blast radius is the same as a local worker's: read/write all beads.
- Cloud workers can push only branches (no tags, no deletes) to repos attached
  to the session, and open PRs that still need review.
- Everything that can reach production, AWS or the private network stays on
  the desktop (§10).
- Session transcripts are stored by the vendor; that's already true of every
  cloud session the user runs today.

## 16. What happens to worker-hosts.md

Marked **superseded** by this design. What carries over:

- the bead as the record of where a thread runs (`host:` label → `backend:`
  label);
- `bd-proxy`'s allowlist and argv handling, reused by `bd-relay`;
- per-host outbox (`outbox/<host>/`) becomes per-backend only where an agent
  can be "not ready" (local);
- `needs:` labels and `repo_needs`, with the local-only set in §10;
- failure-mode thinking: unreachable ≠ gone.

What's dropped: the LAN host registry, slots, worker image, Podman/Quadlet,
Herdr machine forwarding for workers, host bootstrap, the AWS credential
broker, nested containers, the desktop's move to Podman.

Code already merged for hosts (`hosts.go`, `dispatch --host`, `host list`,
`bd-proxy`, the shim) stays until phase 5 below removes the parts nothing
uses; `bd-proxy` stays because the relay is built on it.

## Phases and effort

Agent-days; each phase one PR or a small stack.

| Phase | What | Effort | Needs you |
|---|---|---|---|
| 1. Live spike | With steps 1–4 of §13 done by hand: fire a task with a brief that pins a branch; follow-up via `claude -p --cloud`; message a reclaimed session; REST PR creation; commit signing; Linear connector; setup-script `~/.claude/settings.json`; Codex `exec`/`apply` round trip. Results into this doc | 0.5 | §13 steps 1, 2, 4, 6 |
| 2. Backend interface | `Backend` over today's Herdr code with no behavior change; `backend:` label, `SESSION:`/`TASK:` notes | 1 | — |
| 3. `bd-relay` + shim | HTTP relay on `planBD`, tokens, logging, `last_seen`, `autonomy check`; shim; tunnel docs | 1 | §13 steps 3, 5 |
| 4. `claude-cloud` | fire dispatch, branch pre-create, cloud brief, `claude -p --cloud` prompts, `cloud_quiet`, `AUTO-MERGE:`/`RESOLVE:` handling, board/context/sidebar rows, `pull` via teleport, setup-script renderer | 2–2.5 | — |
| 5. Cleanup | remove unused host-registry code paths; README; mark worker-hosts superseded in code comments | 0.5 | — |
| 6. `codex-cloud` (optional) | exec/status/apply loop, report.md mirror, push and PR from the desktop, review rounds as new tasks | 1.5 | §13 step 6 |
| 7. PR-comment mirror (optional) | fallback channel for bd writes when the relay is down | 1 | — |

Phases 1–5: about **5–5.5 agent-days**; with 6 and 7, about 8.

## Decisions needed

1. **Beads from the cloud.** (A) HTTPS relay through an outbound tunnel with
   network-secret tokens [recommended]; (B) PR-comment mirror only; (C) no
   bd in the cloud: brief in, `report.md`-style notes out.
2. **Codex cloud.** (A) Build it as the patch-producer backend in phase 6
   [recommended, after `claude-cloud` has run a while]; (B) drop it and keep
   Codex local-only; (C) build it now alongside `claude-cloud`.
3. **Dispatch transport for Claude.** (A) One scheduled task per repo with an
   API trigger [recommended]; (B) start `claude --cloud` in a Herdr pane
   interactively and scrape the session id (no beta API, but fragile and
   needs a pane per dispatch).
4. **Worker-hosts code.** (A) Remove the host registry and machine
   forwarding paths in phase 5, keep `bd-proxy` [recommended]; (B) keep them
   for a possible self-hosted environment later.
5. **Default backend.** (A) `local` unless `--backend` or a `backend:` label
   [recommended]; (B) `claude-cloud` by default for beads with no `needs:`
   labels.
