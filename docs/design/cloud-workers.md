# Cloud workers: running workers in cloud sessions and cloud machines

Status: **proposed** (2026-10-30, revised the same day to add `muse-remote`,
Muse over MSP for local workers, and the full remote-sync comparison for
Beads). Supersedes the remote-host half of [worker-hosts.md](worker-hosts.md);
see §18. Nothing here changes behavior until the phases below land. The open
decisions are at the end.

## Summary

[worker-hosts.md](worker-hosts.md) planned to run workers on more machines on
the LAN (a second box, `server.local`, in a worker container with a headless
Herdr). This design drops the LAN machines. Remote workers run in the cloud,
in one of three ways:

- **Claude Code on the web** (`claude.ai/code`): a full Claude Code session in
  an Anthropic-managed VM that clones the repo, pushes branches and keeps
  running with the desktop closed.
- **Codex cloud tasks** (`codex cloud`): a Codex run in an OpenAI-managed
  container that produces a diff.
- **Muse Code on a cloud machine we control** (`muse-remote`): `muse serve
  --listen` in a codespace or cloud VM, driven by quartermaster over the Muse
  Session Protocol (MSP), the same protocol Muse's own clients use.

The desktop keeps the control plane: the coordinator, the ticker, the Beads
database, the inbox, notifications, GitHub polling and everything that needs
the desktop's credentials or network. Quartermaster gains a **backend** per
thread (`local`, `claude-cloud`, `codex-cloud`, `muse-remote`) behind the
operations it already has (dispatch, observe, prompt, answer, resume, pull,
resolve). The bead records which backend it runs on, the way it records
`host:` today.

The backends are not equal, and the design doesn't pretend they are:

| | `claude-cloud` | `codex-cloud` | `muse-remote` |
|---|---|---|---|
| Start a run from a script | yes, through a scheduled task's API trigger (`/fire`) | yes, `codex cloud exec --env … --branch …` | yes, MSP `session/start` + `turn/start` |
| Send it a follow-up | yes, `claude -p "<msg>" --cloud <session>` | **no** (web UI only); a follow-up is a new task | yes, `turn/start` (queue or steer) |
| Read its state and transcript | **no** documented API | status only | yes: `session/statusChanged` (idle/running + approval/input pending), `view/subscribe`, `session/read` |
| Answer its approval prompts from quartermaster | no | n/a | yes, `approval/listPending` + `approval/decide` |
| Push branches, open PRs | yes, REST only (GraphQL is blocked) | not documented: treat as **no** | yes, full `gh` (our machine, our token) |
| Reach Beads | through a relay we publish (§5) | no | through the same relay, over the SSH link the desktop opens (no public endpoint) |
| Machines to run | none | none | one codespace or VM per worker (or a pool), created and deleted by quartermaster |
| What it is | a **worker**, partly unsupervised | a **patch producer** | a **worker**, fully supervised |

`muse-remote` is the only cloud backend that keeps everything the ticker does
today, because MSP gives quartermaster the state, transcript and approvals that
claude.ai/code doesn't expose. Its price is that we run the machine. 
`claude-cloud` needs no machine but can't be watched, only listened to.
`codex-cloud` is a narrower tool for self-contained changes, with the desktop
pushing its diffs.

## Goals and non-goals

Goals:

1. Dispatch a bead to a remote backend; the board, context, sidebar, inbox
   and coordinator treat it as a thread like any other.
2. Keep what the user relies on today (the capability table, §13), and say
   plainly what is lost and how to work around it.
3. Beads stays the only task state and the single source of truth.
4. No LAN machines to run, update or secure. Cloud machines, where a backend
   needs one, are created and deleted per bead by quartermaster.
5. Remote workers hold no long-lived secrets of the user's beyond what their
   machine needs to work (a repo-scoped GitHub token, the agent's own login),
   and nothing that reaches AWS, production or the private network.

Non-goals:

- Moving the coordinator, ticker or Beads off the desktop.
- Running admin, break-glass, kube/VPN, browser or production-apply work in
  the cloud (§12).
- A scheduler that picks cloud vs local on its own (manual first, §4).

## Phase 0 spike: what each backend can do

Checked 2026-10-30 against Claude Code 2.1.295 and codex-cli 0.153.2, their
`--help`, and the vendors' docs. "Verified" means run here; everything else
cites the doc. Items marked **live check** need a real session, which needs
the user's setup first (§15, phase 1).

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

### Muse Code over MSP

Checked against Muse Code 1.4.14: `muse --help`, `muse serve --help`, and the
wire schema the binary exports (`muse schema generate-json-schema --out DIR`
writes `manifest.json`, with a schema fingerprint, and `msp.schema.json`). MSP
is JSON-RPC; the schema lists every method, notification and error.

1. **Start.** `muse serve` serves an MSP host over stdio; `--listen
   [unix:<PATH>]` serves "a multi-client socket daemon instead" (bare: "the
   well-known per-user-machine socket"). **The listener is a Unix socket
   only**: reaching it from another machine needs a transport we provide
   (§9). Over the wire, `session/start` "creates a brand-new session, loads it
   on this host, durably records the start, and auto-subscribes this
   connection". It takes a `commandId` (UUIDv7 idempotency handle, required),
   `workspaceRoot`/`workspaceRoots` (the first root is the CWD),
   `approvalMode`, `permissionProfile` (a profile the host already defines),
   `modelId`, `providerId`. `turn/start` "submits user input to a session; the
   optional disposition controls queue, steer, or replace when a turn is
   already running".
2. **State and transcript.** `session/statusChanged` carries the load state
   (`notLoaded`, `idle`, `running`) and **attention flags**
   (`approvalPending`, `inputPending`). `turn/started`, `turn/completed`,
   `item/started|delta|completed` stream the work. `view/subscribe` re-attaches
   "at an explicit cursor, replaying `(after, head]` before any live event", so
   a dropped connection loses nothing. `session/read` "reads one stored session
   without attaching: no lease, no load, no subscription"; `session/list` pages
   stored sessions.
3. **Follow-ups.** `turn/start` (queued behind a running turn, or steering it)
   and `turn/steer`. `muse session-message send --target <session>` also
   exists for cross-session messages.
4. **Finished / PR.** `turn/completed` and `session/statusChanged` to `idle`.
   PRs are whatever the worker does with `gh`; on a machine we control that's
   the full `gh`, GraphQL included.
5. **Reach.** Whatever the machine can reach. Host flags fix the posture for
   the daemon's lifetime: `--disable-sandbox`, `--sandbox-network <MODE>`
   (default `proxy-only`), `--disable-write`, `--disable-shell`;
   `--allow-workspace-trust` lets sessions on a `--listen` daemon "choose
   workspace trust (sessions default untrusted)", which is the **trust
   ceiling**. Provider secrets go in config only "as whole-value `${VAR}`".
   MCP servers can be passed per session (`config.mcpServers` on
   `session/start`).
6. **Permissions.** Approval mode is chosen on the wire, never on the host:
   `ApprovalMode` is a closed enum, `allowAll`, `promptUnmatched`,
   `onRequest`, `denyUnmatched`, set at `session/start` or with
   `session/setApprovalMode` ("select, never create"). A pending approval
   raises `approval/requested`; `approval/listPending` "reads the full pending
   approval and user-input payloads for a session; a lease-free fold read";
   `approval/decide` "decides a pending approval, guarded by the current
   requirement id against the multi-stage race". Questions to the user are
   `userInput/requested` / `userInput/answer`.
7. **Cost and capacity.** Muse's own subscription (`muse usage`) plus the
   machine. Capacity is whatever machine type we pick.
8. **Pull it local.** The session log lives on the machine. `muse export`
   writes a transcript; resuming a remote session on the desktop isn't
   documented (**live check** whether copying the session directory and
   `muse resume <ref>` works).

Not found in 1.4.14's help: a way for the interactive TUI to attach to a
`--listen` daemon. That matters for local workers (§10).

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
- `muse-remote` closes the supervision gap: state, attention, transcript and
  approvals are all on the wire. What it needs from us is the machine and a
  transport to its Unix socket.

## Architecture

```
                         desktop (control plane, unchanged)
 ┌────────────────────────────────────────────────────────────────────────────────┐
 │ Herdr: coordinator pane, local worker panes                                    │
 │ quartermaster ticker ── observes every thread through its backend              │
 │   ├─ local:        herdr agent list/read/prompt (Muse: MSP reads, §10)         │
 │   ├─ claude-cloud: Beads notes + GitHub; prompts via claude -p --cloud          │
 │   ├─ codex-cloud:  codex cloud status/diff; applies, pushes, opens PRs          │
 │   └─ muse-remote:  MSP client on a forwarded socket (state, turns, approvals)   │
 │ Beads DB (one copy) ◀── quartermaster bd-relay (allowlisted bd)                 │
 │                          ├─ HTTPS listener ◀── outbound tunnel (claude-cloud)   │
 │                          └─ Unix listener  ◀── ssh -R (muse-remote)             │
 │ gh (GraphQL for claude-cloud threads), Linear, AWS, kube, browser               │
 └──────┬──────────────────────────────────────────────┬──────────────────────────┘
        │ outbound tunnel (no inbound port)            │ desktop dials out: ssh -L muse.sock, -R bd.sock
        ▼                                              ▼
 ┌──────────────────────────────┐        ┌──────────────────────────────────────────┐
 │ Claude cloud session (VM)    │        │ codespace / cloud VM we control          │
 │   bd shim → relay (HTTPS,    │        │   muse serve --listen unix:/run/qm/muse  │
 │   token as network secret)   │        │   bd shim → /run/qm/bd.sock (forwarded)  │
 │   git/REST via GitHub proxy  │        │   gh with its own scoped token           │
 └──────────────────────────────┘        └──────────────────────────────────────────┘
 ┌──────────────────────────────┐
 │ Codex cloud task (container) │── diff ──▶ desktop applies, pushes, opens PR
 └──────────────────────────────┘
```

## 1. Backends

A backend is how one thread runs. Quartermaster's commands stop calling
Herdr directly for worker operations and go through:

```go
type Backend interface {
	Name() string // "local", "claude-cloud", "codex-cloud", "muse-remote"

	// Dispatch starts a worker for the bead with the brief and returns the
	// handle to record on the bead.
	Dispatch(b Bead, br Brief) (Handle, error)

	// Observe reports each thread's state for the ticker's 15s pass. Backends
	// that can't see state return Unknown and let Beads/GitHub speak.
	Observe(hs []Handle) map[string]Observation

	// Prompt delivers a message (ticker events, the coordinator's relays).
	Prompt(h Handle, text string) error

	// Approval prompts: local (screen or MSP) and muse-remote (MSP) can
	// read and answer them.
	ReadPrompt(h Handle) (ApprovalPrompt, error)
	Answer(h Handle, choice, expect string) error

	Resume(h Handle) error  // restart an exited agent
	Pull(h Handle) error    // bring the thread to a local worktree (§11)
	Resolve(h Handle, force bool) error
}
```

| Operation | `local` (today) | `claude-cloud` | `codex-cloud` | `muse-remote` |
|---|---|---|---|---|
| Dispatch | herdr worktree + agent + brief | fire the scheduled task with the brief; record `SESSION: <url>` | `codex cloud exec --env … --branch <base> -`; record `TASK: <url>` | create the machine on the bead's branch, open the SSH link, `session/start` + `turn/start` with the brief; record `MACHINE: <name>` and `SESSION: <uuid>` (§9) |
| Observe | herdr agent state (working/idle/blocked/done) + screen | **Unknown** state; liveness from the relay's last-seen time and GitHub pushes (§6) | `codex cloud status` | `session/statusChanged` + attention flags, turn events |
| Prompt | `herdr agent prompt` once idle | `claude -p --cloud <session>` (queues; no idle wait needed) | a **new task** on the PR branch with the message as its prompt (§8) | `turn/start` with disposition `queue` |
| ReadPrompt / Answer | screen parse + hash (`quartermaster prompt/answer`) | not available: the user answers in the session page | n/a | `approval/listPending` / `approval/decide` with the requirement id as the race guard; `userInput/answer` |
| Resume | `claude --continue` in the worktree | send a follow-up (reopens a reclaimed VM: **live check**) | n/a: re-run | restart the machine, reopen the link, `session/resume` |
| Pull | n/a | `claude --teleport` in a new local worktree; thread becomes `local` | `codex cloud apply` in a local worktree | check out the branch locally, start a local agent with the brief and the exported transcript (§11) |
| Resolve | remove worktree, delete merged branch | archive nothing (no API); clear the handle; branch deleted by GitHub on merge | remove the local apply worktree | close the session, delete the machine |

The handle lives **on the bead**, as `host:` does today:

- label `backend:claude-cloud`, `backend:codex-cloud` or `backend:muse-remote`
  (absent = `local`);
- a note `SESSION: https://claude.ai/code/session_…`, `TASK: <url>`, or
  `MACHINE: <name>` plus `SESSION: <uuid>`, parsed like `PR:` notes (latest
  wins).

`resume`, the board, `context` and a fresh coordinator all read it from there.

## 2. Configuration

```toml
# ~/.config/quartermaster/config.toml

[cloud.claude]
# One scheduled task per repo, created once in claude.ai (§15). Its saved
# prompt opts in to the fire payload; its API token is a file, never config.
[cloud.claude.repos.herdr-quartermaster]
fire_url = "https://api.anthropic.com/v1/claude_code/routines/trig_…/fire"
token_file = "~/.config/quartermaster/secrets/claude-fire-herdr-quartermaster"

[cloud.codex]
[cloud.codex.repos.herdr-quartermaster]
env = "env_…"          # codex cloud environment id or label

[cloud.muse]
placement = "codespace"   # or "vm" (§9)
machine = "standardLinux32gb"   # codespace machine type, or the VM's instance type
idle_timeout = "60m"
approval_mode = "promptUnmatched"
permission_profile = ":auto-review"

[cloud.relay]
url = "https://beads-relay.example.net"   # the tunnel's public name; only for claude-cloud (§5)
```

`worker_agent` keeps choosing *which agent*; `--backend` (or the
`backend:` label) chooses *where*. Valid pairs: `claude` with `local` or
`claude-cloud`; `codex` with `local` or `codex-cloud`; `muse` with `local` or
`muse-remote`.

## 3. Dispatch

`quartermaster dispatch <bead> --backend claude-cloud`:

1. Checks the bead may run in the cloud (§12): no `needs:` label that only
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

`--backend muse-remote` creates the branch the same way, then creates the
machine on it and starts the session over MSP (§9).

## 4. Choosing a backend

Manual first, like `schedule = "manual"` in worker-hosts:

- `--backend <name>` on dispatch, else the bead's `backend:` label, else
  `local`.
- A bead or repo `needs:` label that the backend can't satisfy refuses the
  dispatch with the reason (`claude-cloud: needs aws`).
- Later, an `auto` mode could send beads with no `needs:` labels to a remote
  backend when local slots are full. Not in this design's phases.

## 5. Beads from a cloud session

Requirement: a remote worker reads and writes the same beads as everyone
else, claims are arbitrated in one place, and the ticker sees a worker's
writes within a tick or two. A cloud VM can't reach the desktop, and the
desktop has no inbound port.

The desktop's database today: bd 1.3.1, embedded Dolt, about 850 MB on disk,
about 900 beads and several hundred memories, written by every local worker,
the coordinator and the ticker. It already has a Dolt remote in S3 (an S3
bucket for chunks and a DynamoDB table for the root manifest, in a personal
AWS account), synced by a user timer twice a day with `bd sync` (pull,
positive conflict check, `is_blocked` recompute, push with retries; exit 2 on
a merge conflict, 3 on exhausted push races, 4 on a stuck working set).

### Options

| Option | How |
|---|---|
| **A. Relay** | `quartermaster bd-relay` on the desktop: takes a `bd` argv, runs it through the **same allowlist as `bd-proxy`** (`planBD`), pins `BEADS_DIR`, sets the actor and the bead, logs it. Two listeners on the same code: a **Unix socket** that `muse-remote` machines reach through `ssh -R` on the link the desktop opens (no public endpoint), and an **HTTPS** listener published through an outbound tunnel for `claude-cloud`, whose bearer and tunnel tokens are **network secrets**, so they never enter the VM. A `bd` shim on the remote side posts argv and stdin and prints the response. |
| **B. PR-comment mirror** | The worker opens a draft PR at once and writes notes and status as PR comments with a `qm:` prefix (`qm: note …`, `qm: status needs_me`). The ticker mirrors them into the bead on its 60s GitHub pass and marks them mirrored. The brief carries the bead's description, notes and the memories it needs. |
| **C. Remote sync through the S3 Dolt remote** | Each remote worker holds its own replica: it clones the database from the S3 remote, works on it with the real `bd`, and runs `bd sync` after each write (or on a short timer). The desktop's ticker runs `bd sync` every pass instead of twice a day. §5.1 goes through it in full. |
| D. `bd serve` through the tunnel | bd's own HTTP API, published as is. Built for automation clients, but it's the whole surface with no allowlist, and agents use the CLI, so a shim would still have to translate. Dropped in favour of A, which is the same idea with the allowlist. |

### 5.1 Remote sync through S3, in full

**Credentials the remote side needs.** Dolt's `aws://` remote talks to S3 and
DynamoDB with the AWS SDK, which signs every request with SigV4. A
network secret works by adding a header to matching requests after they
leave the VM; a SigV4 signature depends on the secret key and the request,
so it can't be added that way. The credentials therefore have to be **inside
the VM**:

- `claude-cloud`: environment variables, which the docs say are visible to
  anyone using the environment. That means a **long-lived IAM access key**;
  the VM has no workload identity to exchange for short-lived credentials.
- `codex-cloud`: environment variables, or legacy secrets that are removed
  before the agent phase (so the setup could clone, but the agent couldn't
  push).
- `muse-remote` on a VM we control: an **instance role**, short-lived and
  rotated by AWS, no static key. On a codespace: a Codespaces secret, which is
  again a static key.

**Scope.** IAM can narrow the key to the bucket's `backend/*` prefix and to
the DynamoDB item for this database (`dynamodb:LeadingKeys`). That is the
narrowest it gets, and it is still **the whole database**: every bead, every
note and every memory, readable; and the root manifest writable, so a replica
can move `main` anywhere, including back in time. IAM can't scope a write to a
Dolt branch or to one bead. S3 versioning (90 days on noncurrent versions)
makes a bad push recoverable, not preventable.

**Revocation.** Deactivate the key (or, for a VM, detach the role). Every
replica already cloned keeps its full copy of the database.

**Concurrent writers and merge conflicts.** Today there's one writer machine
and a sync twice a day. This makes N+1 replicas syncing every minute:

- Dolt merges cell by cell. Appends merge cleanly (new notes, new comments,
  new events, new beads). Conflicts come from the **same field of the same
  row** changed on two replicas between syncs, and quartermaster does exactly
  that by design: a worker sets `needs_me` while the ticker flips a
  `needs_me` bead back to `in_progress` (`auto: agent resumed`), the
  coordinator closes or defers a bead its worker is updating, `bd update
  --claim` on two replicas, `rolling-out` added on one side and the bead closed
  on the other. `bd sync` halts on these (exit 2) and needs `dolt conflicts`
  resolved by hand, on the desktop's **live** database.
- Push races: N replicas pushing one `main` retry each other out (exit 3).
  Pushing to a per-worker Dolt branch (`cloud/<bead>`) and letting only the
  desktop merge removes the race and keeps the desktop as the arbiter, but the
  credentials above still allow pushing `main`, so it's a convention, not a
  control.
- The desktop's pull merges into the database every local worker is writing.
  `bd sync`'s repair step refuses to run while another writer has uncommitted
  changes, so at a once-a-minute cadence it would retry often; the existing
  timer already hits the upstream `events`-table "would be stomped by merge"
  refusal.
- Claims: the point of one database is that `--claim` is arbitrated in one
  place. With replicas, two workers can claim the same bead and find out at
  merge time.

**Latency for `needs_me` and the inbox.** A worker's note reaches the ticker
after the worker's push and the desktop's next pull: with a 30–60s sync on
each side, **about 1–2 minutes**, plus the ticker's pass. Each sync is several
S3 and DynamoDB round trips. The relay is under a second; the PR mirror is up
to the 60s GitHub pass.

**Offline.** The strongest point of sync. With the desktop off, a worker keeps
reading and writing its replica and everything lands when both sides sync
again. With the relay, `bd` fails while the desktop is off; with the PR
mirror, writes wait on GitHub.

**Which `bd` commands work.** All of them, locally: `bd prime`, memories,
`show`, `ready`, `search`, `dep`, against a replica as fresh as its last pull.
`--claim` isn't arbitrated (above). The relay offers the allowlisted subset,
live and arbitrated. The PR mirror offers writes only; reads come from the
brief.

**Bootstrap.** A full clone is about 850 MB per worker. On `claude-cloud`, a
setup script could clone once into the environment's cached snapshot (cached
about seven days) and pull deltas per session, but the setup script then
needs the key too. Every machine also needs a bd new enough for the schema
(1.3.1 or later).

### Comparison

| | A. Relay | B. PR mirror | C. S3 sync |
|---|---|---|---|
| Credentials in the VM | none for `claude-cloud` (network secrets); none for `muse-remote` (socket over the desktop's SSH link) | none beyond GitHub | AWS key to the whole DB (instance role on a VM we control) |
| Blast radius if a worker is compromised | read/write all beads through the allowlist, while the desktop is up; same as a local worker today | comments on its own PR | read/write the whole DB, rewrite `main`, keep a full copy after revocation |
| Single source of truth, arbitrated claims | yes | yes (desktop writes) | no: N replicas, claims race |
| Merge conflicts | none | none | yes, routine with quartermaster's own status flips; resolved by hand |
| `needs_me` latency | < 1 s + ticker pass | ≤ 60 s + ticker pass | ~1–2 min + ticker pass |
| Desktop off | `bd` fails (fallback: B) | writes wait on GitHub | works; syncs later |
| `bd` coverage | allowlisted subset, live | writes only | everything, possibly stale |
| New moving parts | relay + (for `claude-cloud`) a tunnel | ticker mirror, PR noise | per-worker clone, minute-level sync on the desktop, conflict handling |

**Recommendation: a hybrid of A and B, not C.** The relay is the live path
(reads, writes, claims, memories, `autonomy check`); the PR-comment mirror is
the fallback a worker uses when the shim reports the relay unreachable, so
needs_me and notes still arrive while the desktop is off. Remote sync is the
better offline story but trades the single source of truth for routine merge
conflicts on the live database and puts a key to the whole database into every
VM. The one slice of C worth keeping in mind is a **read-only** replica on a
`muse-remote` VM with an instance role, as a cache for `bd prime` and memories
if relay latency ever matters; nothing here needs it.

Relay details:

- Authentication: on the HTTPS listener, the tunnel's own access check (a
  service token on a network secret's header) plus the relay's bearer token.
  On the Unix listener, the socket exists only on the remote end of an SSH
  connection the desktop opened, mode 0600 for the machine's user.
- Scope: the `bd-proxy` allowlist (no `dolt`, `sync`, `init`, `gc`,
  `config set`, no `--db`/`--directory`/profile flags). The relay also offers
  `quartermaster autonomy check <bead>` read-only, so autonomous beads still
  check their grant.
- Identity: the shim sends the worker's bead (`QM_BEAD`, from the brief or the
  machine's environment) and the relay sets `--actor <backend>:<bead>`. On the
  HTTPS listener the relay can't tell which session sent a request; any holder
  of the environment can reach it, which is the same trust as any worker today
  (full read/write on all beads). On the Unix listener, each machine's link
  carries one bead, so the relay knows it.
- Liveness: each request records `last_seen` per bead, which the ticker uses
  as the heartbeat for `claude-cloud` threads (§6).
- Logged like `bd-proxy.log`: time, bead, subcommand, verdict, never the
  arguments.
- The desktop asleep is a failure mode (§16).

## 6. The ticker and cloud threads

`muse-remote` threads are observed over MSP exactly like local ones are
observed through Herdr (§9): working, idle, blocked on an approval or a
question, finished turn. Everything below is about `claude-cloud`, which
can't be observed that way.

What the ticker reads per `claude-cloud` thread, every pass:

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

For `muse-remote`, almost nothing changes: the machine has the full `gh`
(GraphQL included), `bd` through the shim, and the repo checked out on the
bead's branch. The brief names the machine and adds the `NEEDS LOCAL:` rule
below.

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
  coordinator then offers `quartermaster pull <bead>` (§11).
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

## 9. The muse-remote backend

`muse serve --listen` on a cloud machine we control, one machine per worker,
driven by quartermaster's MSP client over a socket the desktop forwards.

### Where it runs

| | Codespace per worker (**recommended**) | Cloud VM (e.g. EC2), per worker or a pool | Container service (e.g. ECS/Fargate) |
|---|---|---|---|
| Create on a repo + branch | `gh codespace create -R <repo> -b <branch> -m <type> --idle-timeout … --retention-period …` | launch from an image; clone in user data | task definition; clone at start |
| Reach it | `gh codespace ssh -- <ssh flags>` (or plain `ssh` with `gh codespace ssh --config`), so `-L`/`-R` socket forwarding works | SSM Session Manager or SSH; no inbound port with SSM | `execute-command` only: no SSH socket forwarding; would need a sidecar tunnel |
| Image and tooling | the repo's `devcontainer.json`, plus the user's **dotfiles repo**, which Codespaces clones and installs in every codespace | our own image (the worker image from worker-hosts, minus Herdr) | our own image |
| GitHub | a codespace token scoped to the repo; commits can be signed by GitHub (**live check** that it covers GraphQL auto-merge and resolving threads) | a fine-grained token we inject | same as VM |
| AWS for the Beads sync slice | none | instance role | task role |
| Lifecycle | stops when idle, deleted after the retention period; billed per core-hour while running | we stop and terminate; billed while running | per task |
| Size | 2 to 32 cores | any | limited |

**Recommendation: a codespace per worker.** It creates a machine on the
bead's branch in one command, has the user's dotfiles applied by GitHub,
gives a scoped GitHub token without a secret of ours, and cleans itself up.
A VM pool is the fallback for work that needs AWS or a bigger, longer-lived
machine. Containers without SSH are a poor fit for a Unix-socket protocol.

Whether the repo owner's Codespaces policy allows it, and who is billed, is
per repo; dispatch refuses with the reason when `gh codespace create` does.

### The link

The desktop dials out; nothing listens publicly anywhere:

```
ssh -N -o ExitOnForwardFailure=yes -o StreamLocalBindUnlink=yes \
    -L $QM_STATE/muse/<bead>.sock:/run/qm/muse.sock \
    -R /run/qm/bd.sock:$QM_STATE/bd-relay.sock \
    <machine>
```

- `-L` brings the machine's MSP socket to the desktop; quartermaster's MSP
  client connects to the local end.
- `-R` puts the desktop's relay socket on the machine; the `bd` shim there
  talks to `/run/qm/bd.sock`.
- The ticker supervises one link per machine and reconnects with backoff. A
  dropped link loses nothing: the daemon keeps running, and quartermaster
  re-attaches with `view/subscribe` from the last cursor it stored in
  `state.json`, which replays the missed events.
- Authentication is SSH's: the codespace's key from `gh`, or SSM/SSH for a VM.
  The socket on the machine is 0600 for its user. Anyone with a shell on the
  machine controls the daemon, so a machine is single-tenant: one user, one
  bead.

### The daemon

Started by the machine's setup (devcontainer `postStartCommand`, or a
systemd unit on a VM):

```
muse serve --listen unix:/run/qm/muse.sock \
  --disable-sandbox --allow-workspace-trust --vacate-on-upgrade
```

- **Sandbox posture** is fixed by the host for the daemon's lifetime. Locally,
  quartermaster already runs Muse with `--disable-sandbox` because the sandbox
  hides `BEADS_DIR` and `gh`'s login from the worker. On a single-purpose
  machine the machine is the sandbox, so the same choice costs less here.
  `--sandbox-network` and `--disable-shell` stay available if a repo wants a
  tighter host.
- **Trust ceiling:** `--allow-workspace-trust` lets a session choose workspace
  trust (repo skills and rules); sessions default to untrusted. quartermaster
  asks for trust only in the bead's own checkout.
- **Approval mode** is per session, on the wire: `promptUnmatched` by default
  with the `:auto-review` permission profile (as local Muse workers use
  today); autonomous beads keep the same mode, since autonomy is about the
  worker's decisions, not tool approvals.
- **Provider login:** Muse's provider credentials on the machine. Config takes
  secrets only "as whole-value `${VAR}`", so a Codespaces secret (or the VM's
  secret store) supplies the variable (**live check** of headless login).

### MSP mapping

| quartermaster | MSP |
|---|---|
| dispatch | `session/start` {`commandId`, `workspaceRoot`: the checkout, `approvalMode`, `permissionProfile`}, then `turn/start` with the brief |
| observe | `session/statusChanged` (`idle`/`running`/`notLoaded`, attention `approvalPending`/`inputPending`), `turn/started`/`turn/completed` |
| transcript (board detail, `quartermaster read <bead>`) | `view/subscribe` from a cursor; `session/read` for a snapshot |
| prompt | `turn/start` with disposition `queue` (no idle wait) |
| approval prompt in the inbox | `approval/requested` → `approval/listPending` gives the full payload; the inbox item shows it as today |
| `quartermaster answer` | `approval/decide`, guarded by the **requirement id** (the protocol's own version of today's prompt hash) |
| needs_me from a question | `userInput/requested` → `userInput/answer` |
| resume | `session/resume` after the machine or daemon restarts |
| resolve | close the session, `gh codespace delete` / terminate |

quartermaster's MSP client is generated from `muse schema
generate-json-schema` and pins the schema fingerprint from `manifest.json`; a
daemon with a different fingerprint is refused with a clear error, so a Muse
upgrade on the machine can't silently change the protocol under it.

### What the user sees

The board and sidebar show `muse-remote` threads with the same states as
local ones. Focus opens the transcript in a Herdr pane (`quartermaster read
<bead> --follow`, rendering MSP items), and the user can type into it, which
`turn/start`s as the user. Approvals and questions reach the coordinator as
inbox items and are answered over MSP.

## 10. MSP for local Muse workers

Today a local Muse worker runs in a Herdr pane. Herdr tracks its working,
idle and blocked states (Muse reports them), and quartermaster reads an
approval prompt off the screen and answers with keystrokes, guarded by a hash
of the screen text. MSP can replace the screen reading.

| Option | How | Gains | Costs |
|---|---|---|---|
| **A. MSP reads, Herdr pane stays** (recommended first) | Keep the TUI in its pane. quartermaster reads the session's pending approvals and questions with the lease-free `approval/listPending` and its transcript with `session/read`, from a local daemon that shares the sessions root. Answers still go as keystrokes, but the inbox item is built from the structured payload, not parsed text | Exact prompt content, options and requirement id; no parser to maintain for Muse's layout; nothing changes for the user | Whether a lease-free read sees a session another host (the TUI) holds is a **live check**; deciding still goes through the screen |
| B. Headless local workers | Local Muse workers run as sessions in a local `muse serve --listen` daemon, driven entirely over MSP like `muse-remote`; the Herdr pane shows `quartermaster read --follow` | One code path for local and remote Muse; `approval/decide` with the requirement id; no screen scraping at all | No interactive TUI for the worker: Muse 1.4.14 has no flag to attach the TUI to a daemon. Typing into the follow view sends `turn/start`, which covers most of it |
| C. Leave it | Screen parsing as today | Nothing to build | Parser drift whenever Muse changes its prompt layout |

A comes almost free once the MSP client exists for `muse-remote`; B becomes
attractive if Muse ships a TUI that attaches to a daemon, at which point the
local and remote paths are the same.

## 11. Pulling a thread local

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
- `muse-remote`: the branch is pushed, so create a local worktree on it and
  start a local Muse with the brief plus the transcript (`muse export` on the
  machine, copied back); then delete the machine. A true resume of the remote
  session locally is a **live check**.

The reverse (local to cloud) has no handoff: dispatch to the cloud again from
the pushed branch, as a new session.

## 12. What stays local

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
box, and nothing here needs it. A `muse-remote` VM pool with an instance role
could later carry viewer-level `aws` work without any broker; that's a
separate decision, not part of this design.

## 13. Capabilities: today vs cloud

`local` is today; the other columns say what happens to each capability for a
remote thread. **kept** = works as today; **changed** = works differently;
**lost** = not available; **gained** = new.

| Capability today | `claude-cloud` | `codex-cloud` | `muse-remote` | Mitigation |
|---|---|---|---|---|
| Dispatch with a brief | **kept** (fire API) | **kept** (`cloud exec`) | **kept** (`session/start` + `turn/start`) | — |
| Worker named after the bead in the sidebar | **changed**: no pane; listed from the bead | **changed**: same | **changed**: no Herdr agent; listed from the bead, state from MSP | board and context rows from the bead |
| Bead notes, status, `needs_me`, `RUN:` | **kept** via relay | **changed**: once per task, from `report.md` | **kept** via relay over the SSH link | PR-comment mirror as fallback (§5) |
| `bd prime`, memories, `bd remember` | **kept** via relay | **lost** | **kept** via relay | brief carries memories by keyword for codex |
| Ticker sees working/idle/blocked | **lost** | **changed**: PENDING/READY/ERROR | **kept**: `idle`/`running` + `approvalPending`/`inputPending` | `cloud_quiet` item for claude-cloud |
| Transcript readable from quartermaster | **lost** (browser only) | **lost** (browser only) | **gained** over Herdr's screen: structured items, replay from a cursor | — |
| Approval prompts read and answered from the coordinator (`prompt`/`answer`) | **lost** | n/a | **kept**, better: `approval/listPending` + `approval/decide` with the requirement id | claude-cloud: auto mode makes them rare; answer in the app |
| Ticker prompts on checks, reviews, merge, runs | **kept** (`claude -p --cloud`, queued) | **changed**: each prompt is a new task | **kept** (`turn/start`, queued) | — |
| Coordinator relays answers to the worker | **kept** (`quartermaster prompt` routes by backend) | **changed**: new task | **kept** | — |
| PR found by branch or `PR:` note | **kept** | **kept** (desktop opens it) | **kept** | — |
| Auto-merge on every non-draft PR | **changed**: ticker turns it on from an `AUTO-MERGE:` note | **kept** (desktop) | **kept** (worker; GraphQL works, **live check** of the codespace token) | — |
| Resolve review threads after fix/reply | **changed**: worker replies, ticker resolves on `RESOLVE:` | **changed**: desktop | **kept** (worker) | — |
| Merge with `--match-head-commit` (autonomy) | **kept** over REST | **kept** (desktop) | **kept** | — |
| Bot reviews told apart from human | **kept** (ticker) | **kept** | **kept** | — |
| Actions runs followed, `rolling-out` | **kept** | **kept** | **kept** | — |
| Linear issue moves | **kept** if the Linear connector is on the scheduled task (**live check**) | **changed**: desktop | **changed**: per-session MCP config or the desktop moves them | ticker/coordinator moves them from the desktop |
| Autonomous mode (`autonomy check`) | **kept** via the relay | n/a | **kept** via the relay | — |
| `resume` after restarts | **not needed**; reclaimed VM reopens on a message (**live check**) | n/a | **kept**: `session/resume`; the link re-attaches from its cursor | — |
| Focus a worker (`↵`, clipboard) | **changed**: opens the session URL | **changed**: opens the task URL | **changed**: a Herdr pane following the transcript, which accepts input | — |
| `resolve` / `sweep` | **changed**: nothing local to remove | **kept** for the apply worktree | **changed**: deletes the machine | retention period cleans up anything missed |
| Global `CLAUDE.md`/`AGENTS.md`, user hooks, skills, plugins | **changed**: not carried | **changed**: personal skills not synced | **kept** through the Codespaces dotfiles repo (or the VM image) | §14 |
| SessionStart `bd prime` hook | **changed** | **lost** | **kept** if Muse's own hooks are in the dotfiles | the brief tells the worker to run `bd prime` |
| User-scope MCP servers | **lost** (claude.ai connectors only) | **lost** | **changed**: passed per session in `session/start` | — |
| Signed commits | **live check** | **kept** (desktop commits) | **kept**: GitHub signs codespace commits when enabled; a VM uses its own key | — |
| AWS, kube, VPN, browser, admin | **lost** by design | **lost** | **lost** by design (a VM pool could add viewer AWS later) | `needs:` labels keep them local; `pull` (§11) |
| Builds and tests | **kept** (4 vCPU/16 GB, Docker) | **kept** (VM size by plan) | **kept**, sized per machine (2–32 cores) | — |
| Work continues with the desktop off | **gained** (except bd writes) | **gained** (until READY) | **gained** (except bd writes and approvals) | PR-comment mirror for notes |
| Steer from the phone | **gained** (Claude app) | **gained** (ChatGPT app) | **lost** unless Muse has a mobile client | Remote Control covers local Claude workers |
| No machines to run | **gained** | **gained** | **changed**: machines, but created and deleted per bead by quartermaster | — |
| Cost | plan usage | plan usage | Muse usage **plus** machine time | idle timeout, retention period |

## 14. Getting user config to remote workers

What carries over to a Claude cloud session is the repo (Phase 0, Claude item 5). For the rest:

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
- **Muse on a codespace:** the user's **dotfiles repo**, which Codespaces
  clones and installs into every codespace the user creates, carries the
  global `AGENTS.md`, Muse's settings, hooks and skills, the `bd` shim and the
  daemon's start script, exactly as on the desktop. On a VM, the image does
  the same. This is the backend with real parity.

## 15. Setup the user does once

| # | Step | Who |
|---|---|---|
| 1 | Install the Claude GitHub App on each repo that will run cloud workers (needed for cloning without bundle uploads, PR linkage and auto-fix) | **you** |
| 2 | Create a cloud environment `qm-worker`: Custom network access (defaults + the relay host), env vars for Bash timeouts, the setup script from `quartermaster cloud setup-script` | **you** (UI) |
| 3 | Add the relay's network secrets to that environment (tunnel service token header, relay bearer token) | **you** (UI; values from step 5) |
| 4 | Per repo: a scheduled task with the fixed prompt (§3), that repo, the `qm-worker` environment, the connectors workers need, an **API trigger**; save its token to `~/.config/quartermaster/secrets/claude-fire-<repo>` (0600) and its fire URL to config | **you** (UI) |
| 5 | Tunnel for the relay: create it, route a hostname to `quartermaster bd-relay` on localhost, require a service token; quartermaster prints the relay token | **you** (tunnel login), rest scripted |
| 6 | Codex: a cloud environment per repo; put its id in config | **you** (UI) |
| 7 | Muse: allow Codespaces for the repos (owner policy), add a `devcontainer.json` (or use the default image) with sshd, and point Codespaces at the dotfiles repo; add Muse's provider credential as a Codespaces secret | **you** (settings), devcontainer scripted |
| 8 | Smoke: dispatch a no-op bead to each backend; check it notes through the relay, opens a draft PR, gets a ticker prompt, and closes; for `muse-remote` also an approval answered from the coordinator | scripted |

## 16. Failure modes

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
| `muse-remote` link drops | No state, prompts or bd for that machine; the daemon keeps working | Ticker reconnects with backoff, re-subscribes from the stored cursor; `link_down` inbox item after 3 failures |
| Codespace stops on idle | Daemon stops with it; a running turn is lost | Idle timeout above the longest quiet stretch; the open SSH link may count as activity (**live check**); on stop, `gh codespace start` + `session/resume` |
| Codespace deleted by retention | Unpushed work lost | Brief: push at milestones; retention ≥ a few days; resolve deletes it on purpose |
| MSP schema mismatch after a Muse upgrade | Client refuses the daemon | Pin Muse's version in the devcontainer/image; bump it with the client |

## 17. Security

- The VM holds no user secrets: GitHub credentials stay in Anthropic's proxy,
  the relay token and tunnel token are network secrets added outside the VM.
- The relay is the one new inbound path to the desktop. It's reached only
  through the tunnel, needs two tokens, runs only allowlisted `bd`
  subcommands, logs every request, and is off when the ticker isn't running.
  Its blast radius is the same as a local worker's: read/write all beads.
- Cloud workers can push only branches (no tags, no deletes) to repos attached
  to the session, and open PRs that still need review.
- Everything that can reach production, AWS or the private network stays on
  the desktop (§12).
- Session transcripts are stored by the vendor; that's already true of every
  cloud session the user runs today.
- `muse-remote` machines: no listener is public. The MSP socket is reachable
  only through the SSH link the desktop opens; the relay socket exists only
  on the machine's end of that link. The machine holds its GitHub token and
  Muse's provider credential; deleting the machine revokes both from it.
  Anyone with a shell on the machine can drive its daemon, so machines are
  per user and per bead.

## 18. What happens to worker-hosts.md

Marked **superseded** by this design. What carries over:

- the bead as the record of where a thread runs (`host:` label → `backend:`
  label);
- `bd-proxy`'s allowlist and argv handling, reused by `bd-relay`;
- per-host outbox (`outbox/<host>/`) becomes per-backend only where an agent
  can be "not ready" (local);
- `needs:` labels and `repo_needs`, with the local-only set in §12;
- failure-mode thinking: unreachable ≠ gone.

What's dropped: the LAN host registry, slots, worker image, Podman/Quadlet,
Herdr machine forwarding for workers, host bootstrap, the AWS credential
broker, nested containers, the desktop's move to Podman.

The worker image idea comes back in a smaller form as the `muse-remote` VM
image (no Herdr inside), if the VM placement is chosen.

Code already merged for hosts (`hosts.go`, `dispatch --host`, `host list`,
`bd-proxy`, the shim) stays until the cleanup phase below removes the parts
nothing uses; `bd-proxy` stays because the relay is built on it.

## Phases and effort

Agent-days; each phase one PR or a small stack. Ordered by the recommended
answers below (muse-remote first).

| Phase | What | Effort | Needs you |
|---|---|---|---|
| 1. Live spike | Muse: a codespace on a branch, `muse serve --listen`, `ssh -L/-R` socket forwarding, `session/start`/`turn/start`/`statusChanged`/`listPending`/`decide` by hand, idle behavior with a link open, codespace token and GraphQL, headless provider login, lease-free reads of a TUI session (for §10 A). Claude: fire with a branch-pinning brief, `claude -p --cloud` follow-up and reclaimed-VM wake, REST PR creation, signing, Linear connector, setup-script `settings.json`. Codex: `exec`/`apply` round trip | 1 | §15 steps 1, 2, 4, 6, 7 |
| 2. Backend interface | `Backend` over today's Herdr code with no behavior change; `backend:` label; `SESSION:`/`TASK:`/`MACHINE:` notes | 1 | — |
| 3. `bd-relay` | relay on `planBD` with the Unix listener; shim; `last_seen`; `autonomy check`; PR-comment fallback in the shim | 1 | — |
| 4. MSP client | generated from the schema, fingerprint pin, reconnect/replay from a cursor, tests against `muse serve` over stdio with the `echo` provider | 1.5 | — |
| 5. `muse-remote` | codespace create/delete, the SSH link supervisor, dispatch/observe/prompt/answer/resume/resolve over MSP, follow view in a Herdr pane, board/context/sidebar rows | 2 | — |
| 6. Local Muse over MSP (§10 A) | structured approval reads for local Muse workers | 0.5 | — |
| 7. `claude-cloud` | relay HTTPS listener + tunnel; fire dispatch, branch pre-create, cloud brief, `claude -p --cloud` prompts, `cloud_quiet`, `AUTO-MERGE:`/`RESOLVE:` handling, `pull` via teleport, setup-script renderer | 2.5 | §15 steps 3, 5 |
| 8. Cleanup | remove unused host-registry paths; README | 0.5 | — |
| 9. `codex-cloud` (optional) | exec/status/apply loop, report.md mirror, push and PR from the desktop, review rounds as new tasks | 1.5 | — |

Phases 1–6 (Muse remote and local): about **7 agent-days**; with 7 and 8,
about 10; with 9, about 11.5.

## Decisions needed

Recommendations updated for the revision; the earlier five are kept and
renumbered.

1. **Which remote backend first.** (A) `muse-remote` first, then
   `claude-cloud` [recommended: the only one that keeps the ticker's
   supervision, approvals and full `gh`, with no public endpoint; the MSP
   client also improves local Muse workers]; (B) `claude-cloud` first [no
   machines, but unsupervised and GraphQL-less]; (C) both in parallel.
2. **Where `muse-remote` runs.** (A) A codespace per worker [recommended];
   (B) cloud VMs (per worker or a pool, SSM, instance role); (C) start with
   codespaces and add a VM pool for heavier or AWS-flavoured work later.
3. **Beads from remote workers.** (A) Relay (Unix listener over the SSH link
   for `muse-remote`, HTTPS through a tunnel only when `claude-cloud` lands)
   with the PR-comment mirror as the offline fallback [recommended]; (B) S3
   Dolt remote sync per worker (§5.1); (C) PR-comment mirror only.
4. **Local Muse workers over MSP.** (A) MSP for reading approvals, questions
   and transcripts, keystrokes for answers [recommended, once the client
   exists]; (B) headless local Muse driven fully over MSP; (C) leave screen
   parsing.
5. **How quartermaster starts Claude cloud sessions.** (A) One scheduled task
   per repo with an API trigger [recommended]; (B) `claude --cloud` run
   interactively in a Herdr pane.
6. **Codex cloud.** (A) The diff-only backend, last and optional
   [recommended]; (B) drop it and keep Codex local-only; (C) build it early.
7. **Old worker-hosts code.** (A) Remove the host registry and machine
   forwarding paths, keep `bd-proxy` [recommended]; (B) keep them.
8. **Default backend.** (A) `local` unless `--backend` or a `backend:` label
   [recommended]; (B) a remote backend by default for beads with no `needs:`
   labels.
