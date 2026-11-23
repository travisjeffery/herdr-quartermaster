# Workers and the coordinator on Antigravity (agy)

[Antigravity](https://antigravity.google/)'s CLI, `agy`, is an opt-in fifth
agent kind next to `claude`, `codex`, `muse` and `pi`: `--agent agy`,
`worker_agent = "agy"` or `coordinator_agent = "agy"`. Nothing changes until
you ask for it. It is the way to run a worker on Google's Gemini models
(Gemini 4 Neon and friends). Checked against agy 1.5.15 and herdr 0.9.5.

## What works

| | How |
|---|---|
| Start | `herdr agent start --kind agy`; herdr detects agy itself, tracks its conversation id (`herdr:antigravity_cli`), and reports working, idle, done and blocked. |
| Models | agy's own default, unless `[agents.agy] args` passes `--model <name>` (below). `agy models` lists the names your sign-in can run. |
| Mode | Workers and the coordinator start with `--mode auto` unless `[agents.agy] args` says otherwise (below). |
| Brief | Typed in with `herdr agent prompt`, as for Claude. A brand-new worktree opens on agy's folder-trust prompt (`↑/↓ Navigate · enter Confirm`); quartermaster queues the brief in the outbox and delivers it once trust is confirmed. agy has no SessionStart hook that loads `bd prime`; the brief already says to run it first. |
| Resume | `quartermaster resume` restarts agy with `--continue` in the bead's worktree: its most recent conversation there (same conversation id), as with Claude. A bead agy claimed resumes as agy. |
| Board | `t` starts agy (the board's `a`, `g` and `y` were already taken). The detail pane and Herdr's sidebar show the kind as agy. |
| Env | `BEADS_ACTOR=agy` and `BEADS_DIR` are exported in the pane before agy starts, as for every kind, so `bd` works from the worktree and claims are recorded as agy. |
| Close verifier | Unchanged: `claude -p` (or `[verifier]`), whatever the worker's kind. |

## Interactive sessions only: the stdin caveat

quartermaster starts agy the interactive way — `herdr agent start --kind agy`
in a pane, then the brief is prompted into that live session (agy's
`--prompt-interactive` shape, delivered by herdr). It never uses agy's print
mode (`agy --print`/`-p`): print mode takes its prompt from its argument or
stdin, and with stdin closed or empty it waits instead of working — and
signed out, it drops into an interactive OAuth wait. A worker has nobody
watching its stdin, so the pane session is the only covered path. The same
goes for `quartermaster tell` and ticker prompts after the brief: typed into
the pane, only when the worker is at rest, as for Claude, Codex and Muse.

## Sign-in and folder trust

Antigravity keeps its own auth, session and folder-trust state (on Linux
under `~/.gemini/antigravity-cli`); quartermaster never reads or writes it,
and exports nothing into agy's environment but the usual `BEADS_*` and what
`[agents.agy] env` names. Sign in once, yourself, before dispatching:

```sh
agy            # pick Google OAuth (or a Google Cloud project) and finish in the browser
agy models     # should list models instead of "Please sign in"
```

Signed out, a dispatched agy worker sits at its "Select login method" screen
and the brief goes nowhere useful; herdr still reports the pane as an idle
agy agent, so check `agy models` first. A `GEMINI_API_KEY` or Application
Default Credentials sign-in works too and changes which models `agy models`
offers; quartermaster doesn't care which you used.

On a fresh worktree, agy asks `Do you trust the contents of this project?`
(`> Yes, I trust this folder` / `No, exit`, `↑/↓ Navigate · enter Confirm`),
just as Claude Code does. `quartermaster dispatch` detects the startup prompt,
queues the brief, and the ticker delivers it once trust is confirmed — in the
pane, with `quartermaster prompt` / `quartermaster answer`, or by approval
tap. Confirming saves the worktree path to `trustedWorkspaces` in
`~/.gemini/antigravity-cli/settings.json`, so `quartermaster resume` in the
same worktree opens straight into the resumed conversation without asking
again.

## Choosing a Gemini model

There is no quartermaster-level model setting; agy's own `--model` goes in
the kind's args, like any other CLI's:

```toml
[agents.agy]
args = ["--mode", "auto", "--model", "gemini-4-neon-high"]
coordinator_args = ["--mode", "auto", "--model", "gemini-4-neon-high"]
```

Run `agy models` for the exact names your sign-in offers (for example
`gemini-4-neon-xhigh`, `gemini-4-neon-high`, `gemini-4-neon-medium`,
`gemini-4-neon-low`, `gemini-4-hydrogen-*`, `gemini-4-carbon-*`,
`gemini-4-argon-*`, `gemini-3.8-flash-*`).

`args` replace the default (`--mode auto`), so keep `--mode auto` in them
unless you mean to run `accept-edits` or `plan`. `--effort
low|medium|high|xhigh|max` is agy's reasoning-effort flag and goes in the
same place. Then:

```sh
quartermaster dispatch <bead> --agent agy
quartermaster coordinator --agent agy   # or coordinator_agent = "agy"
```

## Permissions

Without an `[agents.agy]` table, agy starts with `--mode auto`. Auto is
agy's own default mode: work inside the workspace proceeds, reads outside it
are allowed, writes outside it ask, and a review judge decides commands that
would leave agy's terminal sandbox — the same shape as Muse's
`:auto-review` default, so an unattended worker isn't left waiting on a
prompt nobody is watching for routine work. quartermaster pins it because a
mode saved in agy's `/config` (say `plan`) would otherwise apply to workers
too. Commands that access paths outside the worktree (including `BEADS_DIR`
in another directory) may fail once inside agy's sandbox and then succeed
when agy retries outside it under Auto mode's review judge.

agy has no default bypass args, like Muse and pi: a worker under a 9 lease
keeps Auto mode's prompts. To opt in, name agy's own switch:

```toml
[agents.agy]
bypass_args = ["--dangerously-skip-permissions"]
```

When a worker's level crosses 9, the ticker relaunches it by prompting
`/exit` and resuming; on agy 1.5.15, `/exit` exits cleanly to the shell and
`quartermaster resume` continues the same conversation via `--continue`.

## What still assumes the other kinds

- **Approval prompts.** `quartermaster prompt`, `answer` and approval taps
  parse agy's folder-trust prompt (`↑/↓ Navigate · enter Confirm`). Other
  agy tool-approval menus have not been verified end to end yet; if one
  doesn't parse, the thread shows as blocked with no hash to answer — answer
  it in the pane.
- **needs_me wake check.** Herdr reports agy's conversation id, but
  quartermaster doesn't read agy's session files, so it can't tell a typed
  prompt from agy waking on its own — the same "unknown" as Codex and Muse
  workers.
- **No headless or RPC path.** agy is a Herdr-pane kind only. `--backend
  pi-rpc` still requires `--agent pi`, and the fresh-session handoff to pi
  still starts from a Claude, Codex or Muse worker, not an agy one.
- **`worker_agent = "auto"`** never picks agy: it records no usage reading
  to compare.
- **The brief's last lines** name Claude Code permission prompts and the
  classifier. For agy, Auto mode's judge and its prompts are the equivalent
  surface; a command the judge won't pass is handed over the same way (a
  `RUN:` note and `needs_me`).
