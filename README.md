# quartermaster

Run many coding agents in parallel from one conversation.

You talk to one coordinator. It turns what you want into
[beads](https://github.com/steveyegge/beads), starts a worker for each one in
its own git worktree, and keeps checking on them for you, so you're only pulled
in when someone needs a decision. It's a [Herdr](https://herdr.dev) plugin: the
workers are ordinary Claude Code, Codex or Muse Code sessions in Herdr panes, and their
state shows in Herdr's sidebar.

![quartermaster in Herdr: you ask the coordinator what's going on; it runs quartermaster context, says what needs you and proposes the next bead; you say go and it dispatches a worker, whose screen appears beside it, with every thread's state in the sidebar](docs/images/quartermaster.png)

<sub>The coordinator in a Herdr workspace, with two workers' screens beside it
and every thread's state in the sidebar. Made-up demo data and stand-in
agents; [the board](docs/images/board.png) and [a thread's
detail](docs/images/board-detail.png) look like this.</sub>

## Why

- **One place to coordinate from.** A single coordinator session plans, splits
  work into beads, dispatches workers and relays their questions. It never
  does the work itself, so it's always free to answer you.
- **Workers get checked on automatically.** A background ticker follows every
  thread on a schedule: it tells a worker when its PR's checks fail, when review
  feedback lands and when it merges, and nudges the coordinator when there's
  news. You don't poll agents; the ones that need you rise to the top of the
  sidebar.
- **Worktrees and workers are handled for you.** One bead gets one branch, one
  worktree and one agent named after the bead. Starting a thread is one key;
  cleaning up after a merge is one command.
- **Beads is the only state.** No task file or database of quartermaster's own:
  statuses, notes and memories live in `bd`, which many agents can update at
  once. That's what lets you run many threads without them stepping on each
  other, and what lets any session, including the coordinator after a restart,
  pick up exactly where things are.

## See it work

![quartermaster in Herdr: the coordinator answers what's going on and dispatches a worker, the board filters threads and opens one's detail, then the ticker sees a PR approved and the coordinator says it is ready to merge](docs/images/quartermaster-demo.gif)

<sub>30 seconds, also as [MP4](docs/images/quartermaster-demo.mp4): you ask
the coordinator what's going on, it reads `quartermaster context` and proposes
a bead, you say go and it dispatches a worker; the board filters to what's in
review; then a PR is approved, the ticker flips its thread to ready to merge
and nudges the coordinator, which tells you. Recorded in a scratch Herdr
session on made-up data. The agents are stand-ins that answer from a script
(no model runs) and `gh` answers from a file; quartermaster, Herdr and bd are
real. Waits for an agent to start and for the ticker's next GitHub pass are
cut. `scripts/demo/record.sh` records it again.</sub>

## How it works

### The pieces

| Piece | What it is | What it owns |
|---|---|---|
| **Beads** (`bd`) | The issue tracker in your repo | All task state: status, notes, questions, lessons. The only record. |
| **Herdr** | The terminal the agents run in | Panes, agent names and states, worktrees, the sidebar, notifications |
| **Coordinator** | One agent named `quartermaster`, in `~/.local/state/quartermaster/coordinator` | Talking to you, planning, creating beads, dispatching. Never does the work. |
| **Workers** | One agent per bead, named after the bead, in the bead's worktree | Doing the work and keeping their bead current |
| **Ticker** | A background loop (`quartermaster ticker`), started with the plugin | Watching threads, following PRs and runs, prompting workers, the inbox, the sidebar |

The coordinator and the ticker never talk to each other directly. The ticker
writes events to an **inbox** (files in `~/.local/state/quartermaster/inbox/`) and
nudges the coordinator; the coordinator reads the inbox through
`quartermaster context`. Everything a worker wants to say goes into its bead.

### The loop

```
  you ──── talk ────▶ coordinator ── quartermaster dispatch ──▶ worker (own worktree)
   ▲                    ▲     │                                  │  ▲
   │                    │     └── quartermaster context◀┐           │  │ prompts, once idle 60s
   │ notifications      │ nudge                      │ inbox     │  │ (checks, reviews, merge, runs)
   │ + sidebar          │                            │           ▼  │
   └──────────────────── ticker ─────────────────────┴──── reads bd and herdr every 15s,
                                                            GitHub PRs and runs every 60s
```

1. **You ask the coordinator for something.** It runs `quartermaster context`,
   creates beads for the work (`bd create`, with parents and dependencies), and
   proposes threads. It waits for your go-ahead.
2. **The coordinator dispatches.** `quartermaster dispatch <bead>` claims the bead,
   creates a worktree on `<branch_prefix><bead>-<slug>`, starts Claude, Codex or
   Muse named after the bead, and sends the **brief**: the bead id and title, the
   worktree and branch, the Linear key if there is one, how to report, and
   your `instructions.md`. If the agent opens on a startup prompt (folder
   trust), the brief goes to the **outbox** and you're notified; the ticker
   delivers it once the agent is ready.
3. **The worker works and writes to its bead.** Findings and decisions go in
   `bd note`. It notes `PR: <url>` when it opens a PR and any Actions run URL
   it's waiting on. It turns on auto-merge (`gh pr merge <url> --auto
   --squash`) for each PR once it isn't a draft, so an approved PR with
   passing checks merges without anyone clicking merge. Who has to approve is
   up to the repo's own rules (required reviews, code owners, automated
   reviewers), which auto-merge still waits for. A PR that needs a human
   decision has it off, and a repo that doesn't allow auto-merge gets a note
   on the bead. When it needs a decision, it writes the question and sets
   the bead to `needs_me`. When it needs you to run a command, it writes
   `RUN: <command>` and sets `needs_me`. After a merge with steps left, it adds
   the `rolling-out` label.
4. **The ticker watches.** Every 15 seconds it reads the active beads
   (`in_progress`, `needs_me`, `blocked`) and Herdr's agents, and joins them by
   name. Every 60 seconds it also asks GitHub about each bead's PR (found by
   branch, or by a PR link in the notes) and any Actions runs linked in the
   notes. It compares each thread with what it saw last time.
5. **The ticker acts on what changed:**
   - **The sidebar:** each worker's row gets its state line, like
     `review · PR #411 approved · ENG-1024`, and the agent view sorts what
     needs you to the top.
   - **The worker:** failing checks, new review feedback, a PR ready to merge,
     auto-merge turned off, a merge or a finished run become a prompt to that
     worker. GitHub turns auto-merge off when the base branch changes or
     someone without write access pushes; while the PR is open, not a draft
     and not waiting on you, the worker is asked to turn it back on. Prompts wait until the worker has
     been idle for `idle_seconds`, so they never land in the middle of
     something, and several are sent together.
   - **The inbox:** each change is also an event file for the coordinator.
   - **You:** entering needs-you, a PR ready to merge, a merge, a failed run
     or a human review notifies you.
   - **The coordinator:** when there are new events and the coordinator has
     been idle for `idle_seconds`, the ticker nudges it to run
     `quartermaster context`.
6. **The coordinator catches up.** It reads the inbox and threads, relays
   questions and `RUN:` commands to you, passes your answers to the worker
   (`herdr agent prompt`), and marks the items handled with
   `quartermaster inbox done`.
7. **The thread finishes.** After the merge the worker verifies and closes its
   bead with a reason. The ticker sees it leave the active set and writes a
   `closed` event, and the coordinator runs `quartermaster resolve` to remove the
   worktree.

### Finishing a thread

Usually there's nothing to do. When a worker's PR merges, the ticker tells it
to verify and close its bead (`bd close <id> --reason "<what shipped and how
it was verified>"`). A `rolling-out` bead is finished first, then closed. The
ticker sees the bead close and writes a `closed` event, and the coordinator
runs `quartermaster resolve <bead>`: that removes the bead's worktree and its
workspace, closing the worker's pane, and deletes the local branch if the PR
merged. With `auto_resolve = true` the ticker resolves on its own once the PR
has merged, the bead is closed and the agent is idle.

Other cases:

| Situation | What happens |
|---|---|
| Done without a PR (an investigation, an ops task) | The worker closes its bead with a reason when it finishes; the coordinator runs `quartermaster resolve <id>` (auto-resolve only covers merged PRs) |
| Dropping or parking the work | Tell the coordinator; it closes the bead (`--reason "dropped: …"`) or defers it, then resolves it, with `--force` only if nothing in the worktree is worth keeping |
| Done, but you want the worktree a while longer | Ask the coordinator to hold off on `resolve` |
| Finished worktrees have piled up | `quartermaster sweep`, then `quartermaster sweep --yes` to remove the safe ones |
| Claims nobody is going to finish | `quartermaster stale`, then `quartermaster stale --release` to reopen them |

`resolve` refuses while the bead is still open or its agent is working, unless
you pass `--force`, and it only ever removes a linked worktree on that bead's
branch, never the main checkout. If the worker ran somewhere other than its
own worktree, there's nothing to remove; close its pane.

Workers save what the next worker should know with `bd remember` and move
their Linear issues as they go, and the coordinator marks inbox items handled.
If you close a bead by hand, without the coordinator, do these yourself:

- save anything the next worker should know with `bd remember "…"`; every new
  session loads it at start-up;
- mark its inbox items handled (`quartermaster inbox done <bead>`);
- move its Linear issue to Done (`quartermaster context` lists the moves due).

### Messages

Every automated message says it isn't from you, and none approves anything.

| From → to | When | Starts with |
|---|---|---|
| dispatch → worker | Once, at start | `You are the quartermaster worker for bead …` |
| ticker → worker | Checks fail, review feedback, ready to merge, auto-merge turned off, merge, run finished | `[quartermaster: automated, not the user] PR #N …`, or `… <workflow> run <url> …` for a run |
| resume → worker | After `quartermaster resume` | `[quartermaster] You were resumed after your agent exited.` |
| ticker → coordinator | New inbox items and the coordinator is idle | `[quartermaster ticker: automated, not the user, approves nothing] N new inbox item(s).` |
| worker → everyone | Any time | A note on its bead (`bd note`), or a status change |
| coordinator → worker | Relaying your answer | Whatever it writes with `herdr agent prompt` |

Inbox event kinds: `needs_you`, `checks_failing`, `new_review`,
`ready_to_merge` (approved or needing no review, merge state CLEAN or
UNSTABLE, no checks running, no unresolved review thread that a push hasn't
outdated, and, when approved, an approval of the head commit within 7 days;
once per head commit; otherwise the board shows
`approved but blocked: 3 open threads, stale approval`), `merged`,
`auto_merge_off` (GitHub turned off a PR's auto-merge), `run_succeeded`,
`run_failed`, `finished` (a worker finished a turn),
`agent_gone` (its agent exited), `resumed` (its agent was restarted),
`likely_stale` and `auto_closed` (from the stale-bead check), and `closed`.

### Features in detail

- **Names.** Quartermaster ties a worker to its bead by name: the bead id with
  dots as dashes (`backend-ab12.3` → `backend-ab12-3`). Dispatch and resume
  set it. If something else renames a worker, the ticker renames it back once
  a minute, when it's the only agent in an active bead's worktree.
- **Needs you.** A thread needs you when its bead is `needs_me` (the question
  is its latest note) or its agent is stopped at a permission or question
  prompt. Answer it in its pane and it drops out of needs you as soon as the
  agent is working again (`working · needs_me`). On its next pass the ticker
  sets the bead back to `in_progress` with the note `auto: agent resumed after
  needs_me`, which keeps the question in the history. The ticker holds its own
  prompts to a `needs_me` thread until then, so they never count as your answer.
- **Commands only you can run** (a classifier denial, an interactive login, a
  production change) come back as a `RUN: <command>` note on a `needs_me`
  bead. The sidebar shows `needs you · run command`, the coordinator relays it
  ready to paste after `!`, and `p` in the board copies it. When you've run it
  the worker notes `RAN: <command> → <result>`. A `RUN:` only counts while it's
  one of the bead's last two notes, so an answered command is never offered
  again.
- **PRs** are found by branch name or by a PR link in the bead's notes, so a
  bead worked on a differently named branch is still followed.
- **Actions runs** linked in a bead's notes (the latest three) are followed.
  The worker is told when one succeeds and to continue, and when one fails
  you're notified and the worker is told to investigate. A running workflow
  shows on the thread's state line.
- **`rolling-out`.** A worker adds this label when steps remain after its PR
  merges. The thread then shows **rolling out** instead of **merged**, and the
  merge prompt says to carry on rather than close.
- **Bot reviews** (Codex, CodeRabbit and other GitHub Apps) are told apart from
  human ones. Both go to the worker; only a human review notifies you.
- **Resume.** Herdr restarts don't relaunch agents. `quartermaster resume` restarts
  each claimed bead's agent in its worktree with `claude --continue`,
  `codex resume --last` or `muse resume --last`, as whichever agent claimed it. `quartermaster context`
  lists resumable threads, stale claims (in progress, no agent, worktree or
  PR, untouched 7 days) and other claims separately.
- **Several repos.** Beads labelled `repo:<name>` work in that repo from
  `repos`; everything else uses `repo`. Dispatch refuses a name that isn't
  configured.
- **Linear.** The bead's issue key comes from its title, or from a `Linear:`
  note (any note, if `linear_prefixes` is set). It goes in the brief and at
  the end of the sidebar row. `quartermaster context` lists where each issue should
  be (In Review while its PR is open, Done once merged) and flags PR titles
  missing the key.
- **Choosing the agent.** `worker_agent = "auto"` picks Codex only when both
  agents' usage readings are under 6 hours old and Codex has more left;
  otherwise Claude. It never picks Muse, which records no usage reading to
  compare; ask for Muse with `--agent muse` or `worker_agent = "muse"`.
- **Sweep** lists linked worktrees whose PR merged, whose bead is closed, or
  that git reports prunable. `--yes` removes only the ones that are clean,
  fully pushed, have no agent in them and whose bead isn't still active. It
  deletes a local branch only when its PR merged.
- **Stale beads.** Every `verify_minutes` (hourly) the ticker re-checks open
  beads that have no agent and haven't been touched for `stale_days` (3),
  looking for evidence their work is already done:
  1. the bead's Linear issue is Done or Canceled (through the `linear` CLI,
     so only with `LINEAR_API_KEY` or `linear auth login` in the ticker's
     environment);
  2. every PR its notes link is merged, none open;
  3. merged PRs since it was opened mention its Linear key or id;
  4. for a bug, the code it quotes was on the default branch when it was
     opened and none of it is now (a local `git grep`);
  5. a closed bead has the same title, or the same Linear key outside its
     own family.

  Strong evidence closes the bead with a reason citing it, but only with
  `auto_close = true`: (1); (2) unless the title is rollout-shaped or the notes
  after the last PR say work remains; and (4) once a run it links has failed
  and a later run of that workflow passed. A `rolling-out`, `needs_me` or epic
  bead, or one with open children, is never closed. Everything else becomes a
  `likely_stale` inbox item for the coordinator; a closed one, `auto_closed`.

  Passes are incremental. Each starts with one Linear query, one GitHub
  GraphQL query (the linked PRs' state and head, and per repo whose default
  branch moved, the PRs merged since the last pass, matched against beads
  locally) and a `git fetch` per repo. A bead is rebuilt only when one of
  those inputs changed, its bead changed, or its daily full re-check is due
  (description, per-bead search, failed-run lookup). Only those extra GitHub
  calls count against `verify_max`; beads over it wait for the next pass. A
  pass is skipped while fewer than `gh_min_remaining` GraphQL points are left
  this hour, since the limit is shared with every agent. A verdict is written
  to the inbox once and again only when it changes. `quartermaster verify` shows what
  a pass would do; `quartermaster verify <bead>` checks one bead in full.
- **Reporting.** `quartermaster report` prints beads closed in the window with
  their close reasons and PRs, your PRs merged without a bead, open PRs in
  flight, and `needs_me` beads with their latest note.
- **The sidebar sort** is a Herdr agent view owned by `plugin:quartermaster`: the
  coordinator first, with a row summarising what it has to deal with
  (`coordinator · 2 need you · 1 review · 3 inbox`), then threads by state
  (needs you first), then agents that aren't on a bead. Herdr drops the view
  when the plugin is unlinked, uninstalled or disabled.

State lives in `~/.local/state/quartermaster/`: `state.json` (the ticker's memory
of each thread), `inbox/` and `inbox/done/` (events), `outbox/` (briefs
waiting for an agent), `coordinator/` (its folder), `ticker.log`, and
`digest.md` and `digest-state.json` (the last digest and what it remembers).

## A day with quartermaster

What this looks like on a real working day for someone who owns a service and
its infrastructure. Bead ids, PR numbers and names are made up.

**9:00: catching up.** You open the coordinator and ask where things stand. It
runs `quartermaster context`, which starts like this (trimmed):

```
1 needs you · 1 checks failing · 2 idle

## Inbox (unhandled; `quartermaster inbox done [bead...]` when dealt with)
- 02:14 app-8zk4 [closed] app-8zk4 closed after PR #402 merged; run `quartermaster resolve app-8zk4` to remove its worktree
- 04:40 app-c71m [checks_failing] PR #405 checks failing: integration-tests

## Threads (bead · state · agent)
- app-3fq1 · needs you · claude idle · Roll the new retry policy out to the worker fleet
  question: Staging has been clean for 12h. Roll to all regions at once, or one region first?
- app-c71m · checks failing · PR #405 (integration-tests) · claude working · Upgrade the HTTP client library
```

Overnight, #405's integration tests failed. The ticker already told that
worker, and it's fixing them, so there's nothing for you to do there. #402
merged, and its worker verified the change and closed its bead. You answer the
one question: "One region first, then the rest after an hour of clean
metrics." The coordinator passes that to the worker, which resets its bead to
`in_progress` and carries on. Then the coordinator runs
`quartermaster resolve app-8zk4`, which removes the merged thread's worktree and
branch.

**9:20: new work from yesterday's incident.** You paste your incident notes: a
DNS resolver saturated under load, requests timed out, and the existing alerts
said nothing because they watch error rates, not demand. You want an alert on
resolver request rate, the connection timeout in the API service raised to
match its retry budget, and a timeline written up for the postmortem.

The coordinator creates a parent bead with three children. It notes that the
postmortem should cite the alert's threshold, so it adds a dependency and
proposes:

> Start now: **app-q2d8** (resolver request-rate alert) and **app-q2d9**
> (API connection timeout). **app-q2da** (postmortem timeline) waits on
> app-q2d8. Claude for both? Go?

You say go. Two workspaces open, each on its own branch and worktree, and both
rows show **working**.

**10:30: the ticker does the chasing.** The alert worker opens PR #411 and
notes the link on its bead. The PR's lint check fails, and the row turns red:
**checks failing · PR #411 (lint)**. You don't touch it. Once the worker has sat
idle for a minute, the ticker sends it the failing check. It fixes the lint
error and pushes, and the row goes back to **working**, then
**review · PR #411 checks running**.

On the timeout PR a teammate asks for the new value to be configurable rather
than hard-coded. The ticker sees the new review and hands it to that worker,
which makes the change, replies on the thread and pushes. You found out from
the sidebar, not by refreshing GitHub.

**11:15: a worker needs you.** The alert worker wants to run the dry run of the
alert rules against production metrics, and its agent stops at a permission
prompt. That's **needs you** at the top of the sidebar. You press `↵` on its
row in the board, look at the command, approve it, and go back to what you
were doing.

**13:00: one-offs.** A colleague posts a bead id in chat: an intermittent test
failure that needs a look. You copy it and press your focus-clipboard key
(`quartermaster.focus-clipboard`), and a worker starts on it without going through
the coordinator. Anything with a bead can
be started from the clipboard or from the board (`prefix+j`, then `c` for
Claude, `x` for Codex or `m` for Muse).

**15:30: things land.** #411 is approved and merged. Its row shows **merged**,
and the ticker tells the worker, which checks that the rule actually loaded and
then closes `app-q2d8`. That unblocks the postmortem bead; the coordinator
mentions it in its next status and, when you say so, dispatches it with the
alert's final threshold already in the bead notes.

**17:30: wrapping up.** You ask the coordinator what's left. Two threads are in
review and one is idle waiting on a teammate. On the board you press `n` on two
ready beads to mark them `next` for tomorrow. One lesson from the day, that the
resolver needs alerts on demand as well as errors, goes into `bd remember`,
so every future worker that runs `bd prime` at start-up sees it.

Nothing about the day lived in your head or a scratch file: every thread's
state, question, PR and lesson is in beads, and the next morning starts with
the same `quartermaster context`.

The board (`prefix+j`) lists threads by state plus the beads you've marked
`next` and other ready beads: `↵` focus or start, `c`/`x`/`m` start with Claude,
Codex or Muse, `n` toggle `next`, `y` copy the id, `p` copy a thread's pending
`RUN:` command, `/` filter, `r` refresh, `q` quit. `/` narrows the rows as
you type, matching bead id, title, agent, state, PR number or repo and Linear
key (every word, any case); `↵` keeps the filter and goes back to the keys
above, which then act on the filtered rows, and `esc` clears it.

`space` (or `v`) opens a detail pane under the list for the selected bead, and
it follows `j`/`k`: status, priority, labels, parent and the start of the
description; the latest note in full (the needs_me question or `RUN:` command)
with a count of earlier ones; every linked PR with its state, merge state,
review decision and checks; the Linear key and where the issue should be; the
agent's status and the last lines of its screen; and the bead's inbox items.
PRs come from the ticker's last pass, so the pane never calls GitHub; the
description and the agent's screen load in the background. In the pane `o`
opens the PR, `l` the Linear issue (set `linear_workspace` unless the bead's
notes link it), `s` shows `bd show` in `$PAGER`, `J`/`K` scroll, `↵` focuses
or starts as in the list, and `esc`, `q` or `space` go back.

## Getting started

Once it's installed (see below), the first ten minutes:

1. **Install and configure it** (below). `configure` points quartermaster at your
   repo. Beads should already be set up there (`bd init`) with the `needs_me`
   status added.
2. **Open the coordinator** with `prefix+shift+j`. It starts in a folder of its
   own, reads `quartermaster context` and gives you a status. The first time, Claude
   asks whether to trust that folder; say yes.
3. **Give it something small.** "Add a `--json` flag to the `export` command."
   It creates a bead, proposes one thread and waits.
4. **Say go.** A new workspace opens with a worktree on
   `<prefix><bead>-add-a-json-flag`, and an agent named after the bead starts
   and gets its brief. Its row appears in the sidebar as **working**.
5. **Leave it.** When the worker opens a PR it notes the link on the bead, and
   from then on the ticker follows it. You'll see **review · PR #N** when it's
   ready for you.

## Requirements

Herdr 0.9.1+, [`bd`](https://github.com/steveyegge/beads) with a custom
`needs_me` status (`bd config set status.custom needs_me`), `gh` logged in, and
any of Claude Code, Codex and Muse Code. Linux or macOS, on amd64 or arm64. Go
is only needed to build from source.

### Muse Code workers

Herdr 0.9.1 starts and tracks Muse itself (`--kind muse`), and Muse reports
its own working, idle and blocked states to Herdr, so nothing extra is
installed for it. Two things differ from Claude and Codex:

- **Its sandbox hides what a worker needs.** Muse's default shell sandbox
  runs commands without your environment, so `bd` can't see `BEADS_DIR`, and
  `gh` comes back unauthenticated, so a sandboxed Muse worker can't keep its
  bead current or open a PR. quartermaster therefore starts Muse with
  `--disable-sandbox` and the `:auto-review` permission profile unless
  `[agents.muse] args` says otherwise, and exports `BEADS_DIR` in its pane (see
  Configuration).
- **It reads `AGENTS.md`, not `CLAUDE.md` or Claude's hooks.** It falls back to
  `CLAUDE.md` only when a project has no `AGENTS.md`, and it has hooks of its
  own, so a `bd prime` SessionStart hook set up for Claude Code doesn't run in
  it. The brief tells every worker to run `bd prime` first unless a session
  hook already loaded it, then names the bead and how to report; put anything else a
  worker must know in `AGENTS.md`. The coordinator's folder gets both files.

## Install

```sh
herdr plugin install travisjeffery/herdr-quartermaster --ref v0.5.3
```

Herdr downloads the prebuilt binary for your machine and checks it against the
release's checksums. Then, from a workspace in the repository you want
quartermaster to work in:

```sh
herdr plugin action invoke configure --plugin quartermaster
```

`configure` writes `~/.config/quartermaster/config.toml` with that repository (or
pass `--repo <path>` when running `quartermaster configure` directly), links
`~/.local/bin/quartermaster` (and `~/.local/bin/qm`), and sets up the sidebar sort. It's safe to run again
and never rewrites an existing config.

To build from source instead: clone the repo, run `go build -o bin/quartermaster .`,
then `herdr plugin link "$PWD"` and `bin/quartermaster configure --repo <path>`.

The name `quartermaster` is only this tool's binary and Herdr plugin id; it is not
published to any package registry.

### Keys and sidebar

Add the sidebar row and keys to `~/.config/herdr/config.toml`; Getting started
uses them:

```toml
[ui.sidebar.agents]
rows = [["state_icon", "machine", "workspace", "tab"], ["agent"],
        [{ token = "$sh_state", rules = [{ starts_with = "needs you", fg = "#f38ba8", bold = true },
                                          { starts_with = "checks failing", fg = "#f38ba8" },
                                          { starts_with = "review", fg = "#f9e2af" },
                                          { starts_with = "rolling out", fg = "#f9e2af" },
                                          { starts_with = "merged", fg = "#a6e3a1" }] }]]

[[keys.command]]
key = "prefix+j"
type = "plugin_action"
command = "quartermaster.board"

[[keys.command]]
key = "prefix+shift+j"
type = "plugin_action"
command = "quartermaster.coordinator"
```

`quartermaster.focus-clipboard` (Claude), `quartermaster.focus-clipboard-codex` and
`quartermaster.focus-clipboard-muse` focus or start the bead whose id is on the clipboard, if you want keys for those too.

### Upgrading from flatcircle, kelpie or shepherd

quartermaster was called flatcircle, kelpie before that, and shepherd before
that. The first quartermaster command moves `~/.config/flatcircle` to
`~/.config/quartermaster` and `~/.local/state/flatcircle` to
`~/.local/state/quartermaster` (or the `kelpie` or `shepherd` dirs, for an
install that never took a later name), leaving a link at each old path, so
the config, inbox, ticker log, worktree bookkeeping and coordinator folder
carry over and an older running ticker or coordinator keeps working. Links
left by earlier upgrades still resolve through the `flatcircle` one. If the
new directory already exists it is used and the old one is left alone.
`FLATCIRCLE_CONFIG_DIR`, `FLATCIRCLE_STATE_DIR` and their `KELPIE_` and
`SHEPHERD_` forms are still read when the `QUARTERMASTER_` ones are unset.

`flatcircle`, `kelpie` and `shepherd` still work as deprecated aliases: their
`bin/` links point at `bin/quartermaster`, `configure` repoints existing
`~/.local/bin` links under those names, and running one prints a one-line
notice. A coordinator agent still named by an old name is renamed
`quartermaster` by the ticker, and its workspace, if still labelled with an
old name, is relabelled `quartermaster`. An older ticker is restarted as
quartermaster by `quartermaster ticker start`. The Herdr plugin id changed
too, so unlink the old plugin, link or install this one, and rename
`flatcircle.*` (or `kelpie.*`, `shepherd.*`) keys in
`~/.config/herdr/config.toml` to `quartermaster.*`.

## Configuration

`~/.config/quartermaster/config.toml`; only `repo` is required:

```toml
repo = "~/src/myproject"
branch_prefix = "tj/"     # worker branches are <prefix><bead>-<slug>
worker_agent = "claude"   # "codex", "muse", or "auto": Claude or Codex, whichever has more quota left
coordinator_agent = "claude"
tick_seconds = 15         # how often the ticker checks threads
gh_seconds = 60           # how often it checks PRs
idle_seconds = 60         # it only types into an agent idle this long
nudge = true              # prompt the coordinator when there's news
auto_resolve = false      # remove a merged, closed bead's worktree automatically
repos = { infra = "~/src/infra", model = "~/src/model" }
linear_prefixes = ["ENG"] # Linear team keys to recognise; empty accepts any KEY-123
linear_workspace = "acme" # linear.app/<this>/issue/KEY: where the board's `l` opens issues
herdr_socket = "~/.config/herdr/herdr.sock"  # the herdr server the coordinator runs on
verify_minutes = 60       # how often to re-check stale beads; 0 turns it off
stale_days = 3            # a bead untouched this long is checked
auto_close = false        # close beads on strong evidence; off, they are only flagged
verify_max = 20           # GitHub calls a pass may make beyond its probes
gh_min_remaining = 1000   # skip a pass below this many GraphQL points left

[agents.muse]             # per-kind args for the agent's own CLI, on dispatch, resume and the coordinator
args = ["--disable-sandbox", "--permission-profile", ":auto-review"]
```

`[agents.<kind>] args` go to that agent's CLI every time quartermaster starts
it, ahead of any resume subcommand (`muse --disable-sandbox ... resume --last`),
and replace that kind's defaults. Muse defaults to the args above:
`--disable-sandbox` because its sandbox scrubs the environment and blocks
`gh`'s credentials, so a sandboxed worker can't use `bd` or open a PR; and the
`:auto-review` permission profile, which keeps Muse's approval judge but lets
it decide instead of waiting on a human nobody is watching. Set `args = []`
for Muse's own defaults. Other kinds start with no extra args.

Before starting any agent, quartermaster exports `BEADS_ACTOR` (the kind) and
`BEADS_DIR` (`beads_dir`) in its pane, so `bd` finds the database from a
worktree. Claude and Codex can set these in their own configs; Muse can't.

The ticker always follows `herdr_socket` (herdr's default server unless set),
not the server that happened to start it: every herdr server that loads the
plugin runs its startup, and a ticker on a server without the coordinator
sees no agents. If the coordinator runs in a named session, set it to that
session's socket (`~/.config/herdr/sessions/<name>/herdr.sock`).
`quartermaster ticker status` prints the socket the ticker follows, `quartermaster
context` warns when it isn't the coordinator's, and `quartermaster ticker start`
moves a ticker that is on the wrong one.

`quartermaster ticker status` also shows how long ago each pass last ran
(`tick 4s ago · gh 41s ago · nudge 10m ago · verify 21m ago`) and warns when a
pass is more than three intervals late, when gh is failing, or when verify is
paused below `gh_min_remaining`. The log records only actions, so a quiet
`ticker.log` doesn't mean a stopped ticker; check `ticker status`.

`repo` is the default repository. A bead whose work is in another one carries
the label `repo:<name>` for a name in `repos` (`bd label add <bead> repo:infra`);
its worktree, branch and PRs then live in that repository, and
`quartermaster context` tags its thread `[infra]`. A `repo:` label naming no
configured repository falls back to `repo` and shows as a warning in
`quartermaster context`.

### Worker hosts

Workers can run on other machines, each running a headless herdr server that
the desktop's herdr reaches as a saved machine (`herdr machine add --label
server server.local`); see [docs/design/worker-hosts.md](docs/design/worker-hosts.md).
Only dispatch and queued briefs know about hosts so far; the ticker, resume,
resolve and sweep still act on `local`.

```toml
schedule = "manual"       # local unless --host or a host: label; "auto" is not built yet

[hosts.local]             # optional: local is implicit and carries every label
slots = 5

[hosts.server]
machine = "server"        # the herdr machine label
ssh = "server.local"      # for git and probes, through the host's worker container
cores = 24
ram_gib = 15              # slots = 0 derives min(cores/4, (ram_gib-3)/4) = 3
labels = ["go-heavy"]
repos = { herdr-quartermaster = "~/src/herdr-quartermaster" }  # name -> path on the host
runtime = "podman"        # podman (default) or docker; container = "qm-worker"

[repo_needs]              # host labels every bead in a repo needs
infra = ["aws"]
```

`quartermaster dispatch <bead> --host server` fetches the base branch in the
host's clone (`ssh server.local podman exec -i qm-worker git -C ... fetch`),
creates the worktree and starts the worker through `herdr --machine server`,
and labels the bead `host:server`. Later dispatches and `focus` go to the
host the label names; `--host` moves it. A host must be enabled, list the
bead's repo (the default repo by its directory name) and carry every label
the bead needs (`needs:<label>` labels and `repo_needs`); `--force` skips the
label check. On a remote host the pane gets `BEADS_ACTOR` only: `bd` there is
the shim to the desktop's database. Briefs for a remote agent that is not ready
queue in `outbox/<host>/<agent>.md`. `quartermaster host list` prints the
registry with each host's slots.

`~/.config/quartermaster/instructions.md` is added to every worker's brief.

## Hourly digest

`quartermaster digest` is a snapshot for clearing through beads: every bead
that isn't closed, grouped as

1. **Needs you**: `needs_me`, with the question (or `RUN:` command) from its
   latest note, and in-progress beads whose agent is stopped at a prompt;
2. **Waiting on review/merge**: in progress with an open PR, with its checks,
   review decision and conflicts;
3. **Blocked**: `blocked`, with its latest note, or blocked by a bead that
   isn't closed;
4. **In progress**, with its agent's state;
5. **Ready / next**: open with the `next` label;
6. **Backlog**: a count and the top five by priority.

Each line has the id, priority, title, assignee, when it was last updated and
its Linear key, and is marked **STALE** when a needs-you bead has waited over
4 hours, an in-progress one has had no update in 24 hours, or a blocked one in
7 days. A "changed since the last digest" section lists new needs-you and
newly blocked beads, PRs whose checks went green, that were approved or that
merged, and beads closed since.

It's built from live state with no LLM: one `bd list --json` for the open beads
and one for those closed since the last run, Herdr's agent list, and one GitHub
GraphQL query per 20 PRs, run in parallel, for the PRs that active beads link.
A PR that has merged or closed is remembered in `digest-state.json` in the
state dir and not asked about again. A run takes a few seconds.

The full digest goes to `digest.md` in the state dir every run. Delivery is
`--deliver`, a comma-separated list (default `desktop`, or
`QUARTERMASTER_DIGEST_DELIVER`):

- `desktop`: `notify-send` with the counts, the changes and the top of needs
  you;
- `slack`: the same summary, a little longer, to Slack (off unless configured,
  below);
- `stdout`: the full digest;
- `none`: only the file.

Nothing is sent when nothing changed and nothing needs you, unless `--always`.
The `digest` plugin action (`Quartermaster: digest of every open bead`) opens
the last digest in a popup; `quartermaster digest show` pages it in a terminal.

**Hourly.** `quartermaster digest install` writes
`quartermaster-digest.service` and `quartermaster-digest.timer` (from
[`systemd/`](systemd/)) to `~/.config/systemd/user/`, plus a drop-in with
`BEADS_DIR` and a `PATH` that finds `bd`, `gh` and `notify-send` (and mise's
shims, if you use mise), then enables
the timer (`OnCalendar=hourly`, `Persistent=true`). Run it again after moving
`bd` or the database; `quartermaster digest uninstall` removes it all. It runs
`~/.local/bin/quartermaster`, so `configure` comes first. Check it with
`systemctl --user list-timers quartermaster-digest.timer` and
`journalctl --user -u quartermaster-digest`.

**Slack.** Put the settings in `~/.config/quartermaster/digest.env` (read by
the service if present; `chmod 600` it), either an incoming webhook:

```sh
QUARTERMASTER_DIGEST_DELIVER=desktop,slack
QUARTERMASTER_DIGEST_SLACK_WEBHOOK=https://hooks.slack.com/services/...
```

or a bot token with `chat:write` and a channel, or your user id for a DM from
the bot:

```sh
QUARTERMASTER_DIGEST_DELIVER=desktop,slack
QUARTERMASTER_DIGEST_SLACK_TOKEN=xoxb-...
QUARTERMASTER_DIGEST_SLACK_CHANNEL=U0123456789
```

## Commands

| Command | What it does |
|---|---|
| `quartermaster coordinator [--agent K]` | Open (or focus) the coordinator agent. Also `prefix+shift+j`. |
| `quartermaster dispatch <bead> [--agent K] [--focus] [--host H] [--force]` | Claim a bead, create its worktree and branch, start a worker and brief it, on host `H` (default: the bead's `host:` label, else local). |
| `quartermaster host list` | The worker hosts: herdr machine, slots, labels, repos. |
| `quartermaster focus [<bead>] [--agent K]` | Focus the bead's worker, or dispatch one. With no bead, reads the id from the clipboard. |
| `quartermaster context` | The coordinator's digest: inbox, threads by state, Linear moves, `next` and ready beads. |
| `quartermaster inbox done [<bead>...]` | Mark inbox items handled (all of them if no bead is given). |
| `quartermaster resume [<bead>...] [--agent K]` | Restart exited workers in their worktrees, continuing their last conversation. |
| `quartermaster stale [--days N] [--release]` | List claims with no agent, worktree or PR untouched N days (7); `--release` reopens them. |
| `quartermaster verify [<bead>...] [--yes]` | Stale beads with evidence their work is done. Lists what a pass would do; `--yes` acts like the ticker (closes only with `auto_close`). |
| `quartermaster resolve <bead> [--force]` | Remove a finished bead's worktree, and its branch if the PR merged. |
| `quartermaster sweep [--yes]` | List finished worktrees across every repo; `--yes` removes only the safe ones. |
| `quartermaster report [--since 24h\|7d\|DATE]` | Markdown summary of what shipped, what's in flight and what needs you. |
| `quartermaster digest [--deliver T,...] [--always]` | Every open bead by state and what changed since the last run, written to `digest.md`; notifies when something changed or needs you. See [Hourly digest](#hourly-digest). |
| `quartermaster digest show\|install\|uninstall` | Page the last digest (the plugin's `digest` action); install or remove the hourly systemd timer. |
| `quartermaster board` | The board popup (`prefix+j`). |
| `quartermaster ticker run\|start\|stop\|status`, `quartermaster tick` | The background loop, or one pass of it in the foreground. `status` shows the herdr socket it follows. |
| `quartermaster configure [--repo PATH]` | First-time setup: write the config, link `~/.local/bin/quartermaster` (and `qm`), install the agent view. Safe to run again. |
| `quartermaster unconfigure` | Stop the ticker and remove the agent view and sidebar tokens. |
| `quartermaster bd-proxy --host NAME [--bd PATH]` | The forced command behind a worker host's Beads key in `authorized_keys`: runs the host's `bd` request (an allowlisted subcommand, `BEADS_DIR` pinned, one line per request in `bd-proxy.log` in the state dir). The host side is [`scripts/bd-shim`](scripts/bd-shim). See [worker hosts §5](docs/design/worker-hosts.md#5-beads-one-database-for-every-host). |
| `quartermaster version` | Print the version. |

`K` is `claude`, `codex`, `muse` or `auto`.

`qm` is a short alias for `quartermaster`: `qm context` is `quartermaster context`.
