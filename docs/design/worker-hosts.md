# Worker hosts: running workers on more than one machine

Status: **superseded** (2026-10-30) by [cloud-workers.md](cloud-workers.md):
remote workers run as cloud sessions instead of on LAN machines. Kept for its
reasoning; §16 of the new design lists what carries over (the bead as the
record of where a thread runs, `bd-proxy`'s allowlist, `needs:` labels). Was
accepted 2026-10-15; the Beads proxy and `dispatch --host` were built.

## Summary

Today every worker (agent, worktree, builds, tests) runs on the machine that
runs the coordinator. This design lets quartermaster spread workers over **N
worker hosts**, starting with a second machine (`server`, at `server.local`)
next to the desktop (host `local`).

The shape:

- **One control plane, on `local`.** The coordinator, the ticker, the inbox,
  GitHub polling, notifications and the Beads database stay where they are.
  Nothing about the coordinator's job changes.
- **Hosts are just places panes run.** Each host runs one long-lived **worker
  container** from a single image that carries the whole toolchain, with a
  headless Herdr server inside it. The host itself only needs a container
  runtime and SSH.
- **Herdr already does the remote control.** Herdr 0.9.1 forwards its socket
  API to saved SSH machines (`herdr --machine <label> agent list|prompt|read|rename`,
  `worktree create`, `pane report-metadata` …). Quartermaster's `Herdr` client
  gains a machine selector; most of the ticker is unchanged apart from asking
  every host instead of one.
- **Beads stays one database.** Workers everywhere read and write the
  desktop's database through a narrow, forced-command SSH proxy; there is no
  second copy and no sync loop.
- **A host registry and a scheduler** pick the host for each dispatch:
  explicit `--host`, then repo/label constraints, then least loaded, with
  `local` as the fallback. Hosts are health-checked and can be drained.
- **The desktop can run the same image**, so a worker behaves the same
  wherever it lands.

## Goals and non-goals

Goals:

1. Dispatch a bead to any registered host; the coordinator, ticker, board,
   context and sidebar treat remote threads exactly like local ones.
2. Hosts carry no hand-installed tooling: a host is a container runtime, SSH,
   disk and credentials.
3. Beads remains the only state and the single source of truth.
4. Adding a host is a repeatable, idempotent bootstrap.
5. Remote hosts hold the minimum credentials; privileged work stays on
   `local`.

Non-goals (for now):

- Moving the coordinator or ticker off the desktop, or running several
  coordinators.
- Live migration of a running worker between hosts (a worker can be
  re-dispatched elsewhere from its pushed branch; see failure modes).
- A general cluster scheduler. N is small (2–5 machines on a LAN).

## What exists today

Facts checked while writing this (2026-10-15):

| | `local` (desktop) | `server` (server.local) |
|---|---|---|
| CPU / RAM | 32 cores / 30 GiB + zram | 24 cores / 15 GiB |
| Free disk | — | 709 GiB on `/home` |
| SSH | sshd running | reachable non-interactively from `local` |
| Container runtime | Docker 29.8 (rootful, user in `docker` group), `systemd-nspawn` | Docker installed but **inactive**; `systemd-nspawn`; no `/etc/subuid` entry (rootless Podman not ready) |
| Tooling | everything | `go` (linuxbrew), `claude`, `codex`, `gh`; **no** `herdr`, `bd`, `muse`, `dolt`, no `~/src`, no `~/.aws` |
| VPN | Cloudflare WARP client | none |
| Git signing | `gpg.format = ssh`, `commit.gpgsign = true` | key not configured |
| Server → desktop SSH | — | fails (host key unknown); not needed by this design except the Beads proxy |

Quartermaster internals that this design touches:

- `Herdr` (`herdr.go`) shells out to `herdr` and can already be pinned to a
  socket (`Herdr.on`). Every call is `herdr <noun> <verb> …`, so adding
  `--machine <label>` in front is mechanical.
- The ticker joins beads to agents **by name** (`agentName(beadID)`); names
  are scoped per Herdr server, but bead ids are globally unique, so the join
  stays valid across hosts as long as every host's agents are listed.
- Local filesystem and git assumptions: `resolve` runs `git worktree remove`
  and `git branch -D` against `cfg.repoFor(b)`; `sweep` checks worktree
  cleanliness; `verify` runs `git grep` in the main checkout; the outbox,
  inbox and state live under `~/.local/state/quartermaster`.
- `agentEnv` exports `BEADS_DIR`/`BEADS_ACTOR` into a worker's pane.
- Notifications go through `herdr notification show` on the coordinator's
  server.

Herdr's remote model (from `herdr --skill`, 0.9.1):

- `herdr machine add --label <label> <ssh-target>` saves a profile; the TUI
  sidebar then shows that machine's panes (the sidebar row already includes the
  `machine` token in this config).
- `herdr --machine <label> <api command>` forwards one API command over SSH to
  that machine's running server. Forwarding **never installs or starts** a
  server and never falls back to local. IDs and agent names are per server.
- Remote worktree paths must be absolute or `~/…`.
- A connection failure does not prove a mutation was not applied: inspect
  before retrying.

## Architecture

```
                         local (desktop)                                   server (server.local)
 ┌──────────────────────────────────────────────────────┐        ┌───────────────────────────────────────────┐
 │ herdr server (TUI)  ── machine profile "server" ─────┼─ ssh ─▶│ host sshd ──▶ container "qm-worker"        │
 │   coordinator pane                                   │        │               herdr server (headless)      │
 │   local worker panes (or local container, optional)  │        │               worker panes: claude/codex   │
 │                                                      │        │               bd shim ──┐                  │
 │ quartermaster ticker                                 │        │                         │                  │
 │   ├─ herdr agent list           (local)              │        │ volumes: repos, worktrees, caches,          │
 │   ├─ herdr --machine server …   (each host)          │        │          agent homes                       │
 │   ├─ gh / Linear / verify       (local only)         │        └─────────────────────────┼──────────────────┘
 │   └─ inbox, state.json, notifications (local only)   │                                  │
 │                                                      │◀── ssh forced command: bd ───────┘
 │ Beads DB (embedded Dolt, BEADS_DIR) ── one copy ─────┤
 └──────────────────────────────────────────────────────┘
```

Control flows desktop → host over Herdr's machine forwarding. The only
host → desktop flow is the Beads proxy, which is a forced command that can run
`bd` and nothing else.

## 1. Host registry

Hosts live in quartermaster's config, next to `repos`:

```toml
# ~/.config/quartermaster/config.toml

# How dispatch picks a host when none is given: "manual" (always local unless
# --host or a host:<name> label), or "auto" (scheduler below).
schedule = "manual"

[hosts.local]
# local is implicit; this table only overrides its defaults.
slots = 5
labels = ["go-heavy", "admin-ok", "aws", "kube", "browser"]

[hosts.server]
machine = "server"            # herdr machine label (herdr machine add --label server server.local)
ssh = "server.local"          # for bootstrap, health probes and git outside herdr
cores = 24
ram_gib = 15
slots = 0                     # 0 = derive from cores/RAM (below)
labels = ["go-heavy"]
# Repos available on this host: name -> path on the host (~ allowed). Omit a
# repo and nothing that needs it is scheduled here.
repos = { backend = "~/src/backend", herdr-quartermaster = "~/src/herdr-quartermaster" }
worktrees = "~/worktrees"
image = "ghcr.io/<owner>/qm-worker:2026.10.1"   # pinned per host; see image rollout
runtime = "podman"            # rootless Podman on remote hosts; "docker" on local until it moves (§13.1)
enabled = true
```

**Slots** are the cap on concurrent workers. Derived when `slots = 0`:

```
slots = min( floor(cores / cores_per_worker),
             floor((ram_gib - reserve_gib) / ram_per_worker_gib) )
```

with defaults `cores_per_worker = 4`, `ram_per_worker_gib = 4` (a Go-heavy
worker: agent CLI ~0.5–1 GiB, `go test ./...` and gopls peaks of 2–3 GiB) and
`reserve_gib = 3` on a dedicated host, 8 on a desktop. That gives `server`
`min(6, 3) = 3` and `local` `min(8, 5) = 5`, which matches the intuition that
15 GiB holds two or three Go-heavy workers. All three knobs are overridable per
host and globally.

**Labels** are capabilities a bead or repo can require. Suggested starting
set: `go-heavy` (enough RAM for full builds), `admin-ok` (may hold
break-glass/admin credentials), `aws` (may use viewer/developer AWS
credentials from the desktop's broker, §9.1), `kube`, `browser` (Chrome automation),
`docker` (can run containers for tests; see §8). `local` has all of them; a
remote host gets only what its bootstrap actually provisioned.

**Runtime host state** (draining, last health probe, observed free memory)
is not config: it lives in the ticker's `state.json` under `hosts`, and
`quartermaster host …` commands change it.

## 2. Scheduling

`quartermaster dispatch <bead> [--host <name>] [--agent …]`.

Which host a bead is on is recorded **on the bead** as a label,
`host:<name>`, set at dispatch. Beads stays the only state: `resume`, the
board, `context` and a fresh coordinator all read the host from the bead, and
a label survives restarts of everything else. (A local bead may carry
`host:local` or no label.)

Algorithm:

1. **Sticky.** If the bead already has `host:<h>` (a resume or re-dispatch),
   use `h`. Moving it takes `--host <other>` explicitly.
2. **Explicit.** `--host <h>` picks `h`. Hard constraints (below) still apply;
   `--force` overrides them, never health.
3. **Candidates** are hosts that are enabled, healthy, not draining, have the
   bead's repo in `repos`, and carry every required label.
   Required labels come from:
   - the bead's `needs:<label>` labels (`needs:admin-ok`, `needs:aws`);
   - per-repo defaults: `[repo_needs] infra = ["aws"]`;
   - kind defaults: `muse` needs `muse` if not every image carries it.
4. **Schedule mode.** In `manual` the only automatic candidate is `local`. In
   `auto`, rank candidates by free slots (slots − active workers there),
   then by observed `MemAvailable`, then prefer `local` on a tie (lowest
   latency, the user is looking at it).
5. **Admission.** A host with zero free slots, or with `MemAvailable` below
   `ram_per_worker_gib` at its last probe, is not a candidate.
6. **Fallback.** No remote candidate → `local` if `local` qualifies; otherwise
   dispatch refuses with the reason per host (`server: draining`,
   `server: no repo infra`, `local: 0 free slots`), and the coordinator
   relays it.

Active workers per host are counted from that host's `herdr agent list`
joined with active beads, the same join the ticker already does, so the count
is never a separate counter that can drift.

The coordinator never has to know about hosts: it keeps running
`quartermaster dispatch <bead>`. `quartermaster context` shows hosts and free
slots so it can mention them, and the user can say "put this on server".

## 3. Herdr on the hosts

**Inside the worker container** (§6), a headless `herdr server` runs as the
container's main process with a named session (`qm`). The host's sshd is the
only entry point; Herdr's machine forwarding SSHes to the host and talks to the
server's socket. Two ways to make the container's socket reachable through
the host's SSH:

- **Recommended:** bind-mount the container's Herdr config/socket directory to
  the host user's `~/.config/herdr` and install a tiny `herdr` wrapper on the
  host that `exec`s into the container (`podman exec -i qm-worker herdr "$@"`).
  Herdr's forwarding runs the remote `herdr` binary over SSH, so a wrapper is
  enough; the host never installs Herdr itself. (Phase 0 confirms forwarding
  uses the remote CLI and not a raw socket path; if it uses the socket path,
  the bind mount alone suffices.)
- Alternative: run sshd inside the container on another port and point the
  machine profile at it. More moving parts, a second host key, no benefit.

On `local`, `herdr machine add --label server server.local` (Herdr asks before
replacing an incompatible remote server; answer no and fix the image instead).

**Quartermaster's Herdr client** gains `machine string`:

```go
func (h Herdr) onMachine(label string) Herdr   // prefixes every call with --machine label
```

and an `Agent.Host` field filled by whoever listed it. Changes by caller:

| Caller | Today | Multi-host |
|---|---|---|
| `ticker` tick | `h.Agents()` once | `Agents()` on every enabled host in parallel, per-host timeout (5s), merged; a host that fails keeps its last snapshot marked stale and its threads show `host unreachable` |
| prompts, `Read`, `Rename`, `ReportState` | by name / pane | routed to `a.Host`'s client; pane ids are per server so they're never used without the host |
| `Notify` | coordinator's server | unchanged: always `local` |
| `dispatch` | `WorktreeCreate` on local repo path | `onMachine(h).WorktreeCreate(hostRepoPath, …)` with `~/…` paths; `TabRename`, `Run(agentEnv)`, `AgentStart`, `Prompt` on the same client; duplicate-agent check scans **all** hosts |
| outbox | `outbox/<agent>.md` | `outbox/<host>/<agent>.md`, delivered when that host's agent is ready |
| `resume` | `claude --continue` in local worktree | same, on the bead's `host:` |
| `resolve` | herdr worktree remove + local `git branch -D` | herdr remove via `--machine`; branch deletion via `ssh <host> podman exec qm-worker git -C … branch -D` (a `hostExec` helper); `merged()` keeps using `gh` from the local checkout (same GitHub repo) |
| `sweep` | local `git status` per worktree | runs per host through `hostExec`; output grouped by host |
| `verify` | local `git grep`, `git fetch` | unchanged: verify only needs the default branch, which `local` has |

Folder-trust and other startup prompts on a remote pane still go to the
outbox and notify on `local`; the user answers them in the Herdr TUI, which
shows remote panes. Baking `~/worktrees` as trusted into the image's agent
configs (§6) removes the common case.

Sidebar: Herdr's agent rows already render the `machine` token, so remote
workers show `server` in the row. `pane report-metadata` is forwarded, so
`sh_state` tokens work. **To verify in phase 0:** whether the plugin's agent
view (sort by `sh_rank`) orders remote agents too, and whether metadata TTLs
behave the same through forwarding.

## 4. Repos and worktrees on a host

- Layout mirrors `local`: repos under `~/src/<name>` **on a volume**
  (`qm-src`), worktrees under `~/worktrees/<repo>/…` (`qm-worktrees`). Paths
  are the same inside and outside the container (mount at the same `$HOME`
  path) so paths in notes, briefs and Herdr output read the same everywhere.
- Clones are made by the bootstrap (`gh repo clone`) for each repo in the
  host's `repos`. They're ordinary clones with `origin` on GitHub; workers push
  branches like they do locally.
- Before `worktree create`, dispatch runs `git fetch origin <base>` on the
  host (through `hostExec`), so a host that sat idle doesn't cut worktrees from
  a stale base. (The stale-base problem has bitten local worktrees too; the
  brief's ancestor check applies unchanged.)
- `resolve` and `sweep` act on the host the bead's label names.

## 5. Beads: one database for every host

Requirement: a worker anywhere must read and write the same beads as the
coordinator, with claims arbitrated in one place, and `bd prime`/`bd remember`
must see the same memories.

The desktop's database is embedded Dolt (`dolt_mode: embedded`, bd 1.3.1),
which serializes writers with a file lock. Options:

| Option | How | Pros | Cons |
|---|---|---|---|
| **A. Forced-command SSH proxy** (recommended) | On each host, `bd` is a shim: `ssh -T -i ~/.config/qm-worker/beads-key -- desktop.local <quoted args>`. On the desktop, the key is in `authorized_keys` with `command="quartermaster bd-proxy --host <name>",restrict`. `bd-proxy` parses `SSH_ORIGINAL_COMMAND`, refuses anything that isn't a `bd` subcommand on an allowlist, pins `BEADS_DIR`, passes `--actor`, and execs `bd`. | No migration; same lock and semantics as local agents; one copy; nothing new to run on the desktop; the key can only run bd | Desktop must be reachable from hosts; ~50–150 ms per call on a LAN (ControlMaster helps); desktop asleep = no bd |
| B. Dolt sql-server on the desktop | Migrate the desktop DB to server mode; hosts reach it through an SSH reverse tunnel with bd's (experimental) `--proxied-server-external-host` | Native bd on every host; concurrent writers without a file lock | Migrates the live DB every local agent uses; a supervised server to keep up; the flags are marked experimental; SQL access = whole DB (same scope as A in practice) |
| C. `bd serve` HTTP API | Desktop runs `bd serve` with a token file; hosts call it | Designed for automation clients, token rotation built in | Agents use the `bd` CLI and hooks, not HTTP; a CLI-over-HTTP shim would have to be written |
| D. Dolt remote sync (`bd sync`, S3) | Each host has a copy, syncs periodically | Works offline | Two sources of truth; claims race; known merge failures with the current remote. **Rejected.** |

**Recommendation: A now, B later if latency or lock contention shows up.** A
keeps the single source of truth trivially true, changes nothing for the
coordinator or local agents, and its security scope is narrow and auditable.

Details for A:

- The proxy runs as the desktop user (it needs the DB), but only `bd`, only
  with the allowlisted subcommands (`prime show list search ready note update
  close create label dep remember memories comments …`, not `dolt`, `sync`,
  `init`, `flatten`, `gc`, `config set`), with stdin passed through for
  `--body-file -`.
- The shim sends `BEADS_ACTOR` from the pane (as `agentEnv` does today) as
  `--actor`; a request without one acts as the host's name. The proxy
  prefixes nothing else.
- Implemented (phase 2): `bdproxy.go` and `scripts/bd-shim`. The shim
  single-quotes each argument (ssh joins them with spaces); the proxy splits
  them with no shell, refuses unquoted shell syntax, refuses `--db`,
  `--database`, `-C/--directory`, `--global`, `--dolt-auto-commit` and the
  profile flags anywhere before `--`, scrubs `BEADS_*` from the environment,
  and execs `bd` so stdin and the exit code pass through. Each request is one
  line in the state dir's `bd-proxy.log` (host, actor, subcommand, verdict;
  never the arguments). The shim's ControlPath is its own (`qm-beads-%C`): a
  master connection opened with another key would not run the forced command.
- SSH multiplexing (`ControlMaster auto`, `ControlPersist 10m`) in the
  container's SSH config keeps calls cheap; Claude's SessionStart `bd prime`
  hook then costs one round trip.
- Reachability: hosts resolve the desktop by mDNS (`desktop.local`). The
  bootstrap pins its host key (`ssh-keyscan` from `local`, written into the
  container's `known_hosts`), which fixes today's "host key verification
  failed".
- Ticker sees remote writes immediately: they land in the same DB.

## 6. The worker image

Hosts must not depend on what's installed on them. Everything a worker uses
ships in one image; a host needs a container runtime, sshd, a user account,
disk and its credentials.

### Runtime choice

| | Verdict | Why |
|---|---|---|
| **OCI image, rootless Podman on remote hosts** | **Use this** | Same image on every host and in CI; registry distribution; volumes for state. Remote hosts run it under rootless Podman (below); `local` keeps its existing Docker until a later phase (§13.1). The image is runtime-neutral |
| systemd-nspawn | No | Present on both hosts, but no registry/build story: you'd build a rootfs yourself and ship tarballs; weaker tooling for volumes and updates; root-run by default |
| Nix (flake / home-manager) | Not now | Reproducible toolchains and arguably a better fit for pinning, but it installs onto the host (the thing we're avoiding), doesn't isolate workers from the host, and the agent CLIs are npm packages that update weekly, which fights Nix purity. A good way to *build* the image later (`dockerTools.buildLayeredImage`), not a substitute for it |

### Running it: rootless Podman and Quadlet

On a remote host the worker container is a **Quadlet** user unit, so systemd
starts it at boot (lingering is on), restarts it, and updates it:

```ini
# ~/.config/containers/systemd/qm-worker.container
[Unit]
Description=quartermaster worker

[Container]
ContainerName=qm-worker
Image=ghcr.io/<owner>/qm-worker:2026.10.1
AutoUpdate=registry
UserNS=keep-id:uid=1000,gid=1000
Volume=qm-src:%h/src
Volume=qm-worktrees:%h/worktrees
Volume=qm-cache:%h/.cache
Volume=qm-claude:%h/.claude
Volume=qm-codex:%h/.codex
Volume=qm-herdr:%h/.config/herdr
Volume=%h/.config/qm-worker/secrets:/run/qm-secrets:ro,Z
# Docker-API socket for tests that start containers (hosts labelled docker, §10)
Volume=%t/podman/podman.sock:/run/docker.sock
Environment=DOCKER_HOST=unix:///run/docker.sock
PodmanArgs=--memory=12g

[Service]
Restart=always

[Install]
WantedBy=default.target
```

- `--userns=keep-id` maps the host user to the container's uid 1000, so files
  on volumes and bind mounts stay owned by the host user with no remapping,
  and the container starts directly as the worker user (the entrypoint's
  non-root path).
- `AutoUpdate=registry` plus `podman auto-update` gives the rollout below a
  single command; the unit pins a dated tag, so "update" means changing the
  tag in the unit, never following `latest`.
- `podman.socket` (a user unit) serves the Docker API for tests that start
  containers (§10).
- Registry login is per host and runtime-only: `podman login ghcr.io` with a
  read-only package token (or `gh auth token` from a host-scoped login), kept
  in the user's `auth.json`, never in the image.

### Contents

Built from a **version manifest** (`worker-image/versions.toml`) so every
version is explicit and bumping one is a one-line PR:

- Base: a slim glibc distro (Debian or Ubuntu LTS; matches the agent CLIs'
  prebuilt binaries better than Alpine).
- Herdr (pinned; must be protocol-compatible with `local`'s Herdr, since
  machine forwarding refuses incompatible servers), quartermaster (same
  release as `local`), `bd` (same version as the desktop's, so the shim and
  hooks agree), `gh`, `git`, `openssh-client`, `jq`, `ripgrep`.
- Agent CLIs: `claude`, `codex`, `muse` (pinned npm versions; their
  self-updaters disabled so the image is the version).
- Toolchains via **mise**, pinned to what the repos declare (`go` from
  `go.mod`/`.tool-versions`, `node`, `python`, `terraform`, `helm` …). The
  manifest lists them; CI fails if a repo's declared version isn't in the
  image. mise also lets a worker install a missing pinned version into the
  cache volume on demand rather than failing.
- Dotfiles: global `CLAUDE.md`/`AGENTS.md`, Claude/Codex/Muse settings,
  hooks (SessionStart `bd prime`), skills, user plugins, git
  config (minus identity keys and signing key path, which come at runtime),
  `~/worktrees` pre-trusted in each agent's config.
- The `bd` shim (§5) as `/usr/local/bin/bd`, and an entrypoint that starts
  `herdr server --session qm` and stays in the foreground.

### Build and publish

- A `worker-image` directory (in the dotfiles repo, which already owns the
  dotfiles that go in; the image recipe and manifest live next to them).
- CI (GitHub Actions) builds on a manifest or dotfiles change, with BuildKit
  cache, and pushes to **GHCR** as a private package:
  `ghcr.io/<owner>/qm-worker:<yyyy.mm.n>` plus the digest. Multi-arch
  (`linux/amd64,linux/arm64`) only when a host needs it; both current hosts
  are amd64, so start amd64-only.
- Reproducibility: base image pinned by digest, apt packages from a snapshot
  mirror (or pinned versions), npm with a lockfile, mise with exact versions
  and checksums. The image's labels carry the manifest hash and dotfiles
  commit.
- A smoke job runs the image: `herdr --version`, `bd --version`,
  `claude --version`, `go version` against the manifest, and
  `herdr server` starts and answers `agent list`.

### Rolling out an update

1. CI publishes `qm-worker:2026.10.2`.
2. Bump `image` for one host in config (canary), `quartermaster host update
   server`:
   - **drain** the host (no new dispatches);
   - wait for active workers to be idle (or `--now`, which accepts losing
     their in-flight turn: worktrees and sessions are on volumes, so
     `resume` restarts them);
   - change the tag in the host's Quadlet unit, `systemctl --user daemon-reload`
     and `podman auto-update` (pull by digest, recreate with the same volumes);
   - wait for `herdr status` to answer, re-run health checks;
   - `quartermaster resume --host server` restarts that host's agents
     (`claude --continue` etc.; their histories are on the volume);
   - undrain.
3. When it's been fine for a day, bump the remaining hosts.

Rollback is the same with the previous tag. Because the image never holds
state or credentials, an update is a container recreate, never a migration.

## 7. Topology: one container per host

| | One long-lived container per host, many panes (**recommended**) | One container per worker |
|---|---|---|
| Fit with Herdr | Natural: one Herdr server per host, one machine profile | Herdr server per container → one machine profile per worker, or panes that `podman exec` into per-worker containers |
| Overhead | One set of daemons; shared GOCACHE/module cache | Per-worker Herdr, gopls caches, duplicated memory |
| Isolation | Workers share a container (as they share a desktop today) | Workers isolated from each other |
| Lifecycle | Update = recreate one container | Create/destroy per dispatch; more moving parts in quartermaster |
| Resource limits | Container-wide `--memory`/`--cpus`; per-worker by slots | Per-worker cgroup limits (nicer) |

Recommendation: **one container per host.** The threat this design is
defending against is a worker damaging the *host* or its credentials, not
workers attacking each other (they're the same user's agents on the same
beads). Per-worker isolation can come later without changing the image, by
having dispatch start the agent in a per-worker sub-container; nothing below
depends on that choice.

Give the container a memory limit a little under host RAM minus the host's
own reserve, so a runaway build is OOM-killed inside the container rather than
taking down sshd.

## 8. State on volumes, not in the image

Named volumes (or bind mounts under one host directory, which makes backup
and inspection easy):

| Volume | Mounted at | Holds |
|---|---|---|
| `qm-src` | `~/src` | repo clones |
| `qm-worktrees` | `~/worktrees` | worker worktrees |
| `qm-cache` | `~/.cache` | GOCACHE, GOMODCACHE (via `GOPATH=~/.cache/go`), npm/pnpm, mise installs, pip |
| `qm-agents` | `~/.claude`, `~/.codex`, `~/.config/muse` state subdirs | session histories (`--continue`, `resume --last`), per-host OAuth tokens (§9) |
| `qm-herdr` | `~/.config/herdr` | Herdr session state and socket |

Dotfile-managed config in those directories (settings, hooks, skills) comes
from the image and is re-applied by the entrypoint on each start, so a stale
volume can't pin old config; only the state subdirectories persist.

Beads is **not** on any host volume: §5 applies unchanged inside the
container.

## 9. Credentials: runtime only, scoped per host

Rule: **nothing secret in the image**, ever. CI's image scan (e.g. trufflehog
or gitleaks on the built filesystem) fails the build if it finds one.

Injected at container start, from a per-host directory on the host
(`~/.config/qm-worker/secrets/`, mode 0700), mounted read-only:

| Credential | Remote host (`server`) | `local` | How it's provided / rotated |
|---|---|---|---|
| GitHub (`gh`, git push) | yes: fine-grained token, repos in the host's `repos`, contents + PRs + actions read | user's normal `gh` login | `GH_TOKEN` file; 90-day expiry; renewing = replace the file and restart the container |
| Git commit signing | **per-host SSH signing key**, registered on GitHub as a signing key named for the host | existing key | key file mounted; revoke one host by removing its key on GitHub |
| Agent CLI logins (Claude, Codex, Muse OAuth) | per host, stored in `qm-agents` | existing | logged in once per host interactively (`podman exec -it qm-worker claude /login`, `codex login --device-auth`); persist on the volume; revoke from each vendor's sessions page |
| Beads proxy key | yes, can only run `bd` (§5) | n/a | key file; remove the `authorized_keys` line to revoke |
| Linear / Slack / other MCP | claude.ai-hosted connectors follow the Claude login; anything needing a local token: none on remote by default | as today | add per need, as a mounted file |
| AWS | only on hosts labelled `aws`: short-lived viewer/developer credentials from the desktop's broker (§9.1); no AWS secrets stored on the host | SSO profiles, admin break-glass | admin is never brokered; revoke a host by removing its broker key |
| Kubernetes | only on hosts labelled `aws` *and* with network reach to the cluster API: kubeconfigs that call `aws eks get-token` with the brokered profile | as today | follows the AWS broker; no static kube credentials on any host |
| Personal accounts (e.g. the Beads S3 sync remote) | never | as today | — |

Per-host scoping falls out of labels: work that needs `admin-ok`, `aws` or
`kube` only schedules where those credentials exist. `admin-ok` is `local`
only; `aws` is `local` plus remote hosts given a broker key. A compromised
remote host can push branches to the configured repos, open PRs (which still
need review), spend agent subscription quota, read/write beads and, if
labelled `aws`, act as the viewer/developer roles for at most an hour after
its key is revoked — never admin.

No SSH agent forwarding into the container (it would expose the desktop's
keys to every worker).

### 9.1 AWS through a desktop credential broker

Remote workers that need AWS get it from the desktop, which already holds an
SSO session, instead of holding AWS credentials of their own. Same pattern as
the Beads proxy (§5): a forced-command SSH key per host.

```
 worker (server)                                   desktop (local)
 aws --profile viewer …                            authorized_keys:
   └─ credential_process = qm-aws-creds viewer       command="quartermaster aws-broker --host server",restrict ssh-ed25519 …
        └─ ssh -i aws-broker-key desktop.local ───▶ quartermaster aws-broker
             "creds viewer --bead app-123"            ├─ host from the key line, never from the request
                                                      ├─ profile in this host's allowlist? admin? → deny
                                                      ├─ mint from the desktop's SSO session
                                                      ├─ verify the role, cap the lifetime
                                                      ├─ log the request
        ◀──── credential_process JSON ───────────────┘
```

**Desktop side: `quartermaster aws-broker`.**

- The host's identity comes from the `--host` in its `authorized_keys` line
  (one key per host), not from anything the host sends.
- Config names which profiles each host may have:

  ```toml
  [aws_broker]
  max_minutes = 60               # 15–60
  [aws_broker.hosts]
  server = ["viewer", "developer"]   # logical name -> desktop profile below
  [aws_broker.profiles]
  viewer = "<desktop SSO profile for the read-only role>"
  developer = "<desktop SSO profile for the developer role>"
  ```

- **Admin is never brokered**, whatever the config says. The broker refuses
  any profile whose configured role name or ARN matches an admin pattern
  (`*Admin*`, `*admin*`, break-glass names). After minting it checks the role
  ARN of the credentials it got (`sts get-caller-identity`) against the same
  patterns and against the expected role, and refuses on a mismatch. These
  checks are in code, not config.
- **Minting.** `aws configure export-credentials --profile <p> --format process`
  on the desktop returns the SSO role's temporary credentials. Their lifetime
  is the permission set's session duration. The broker hands them out only if
  they expire within `max_minutes`; otherwise it chains once (`sts
  assume-role` into the same role with `DurationSeconds` ≤ 3600, which AWS
  caps at an hour for chained roles anyway), which needs the role's trust
  policy to allow it. Phase 0 of this work checks the permission sets' session
  durations to see which path applies.
- **Every request is logged** on the desktop, granted or denied, as a JSON
  line in `~/.local/state/quartermaster/aws-broker.log`: time, host, profile,
  bead and actor (as the worker sent them), access key id (never the secret),
  expiry, and the outcome or denial reason. A denial is also an inbox event,
  so the coordinator sees it.
- **Fails closed.** If the desktop's SSO session has expired, the broker
  returns an error, not stale or broader credentials:
  `aws broker: the desktop's SSO session expired; run "aws sso login --profile <p>" on the desktop`.
  The worker turns that into a `RUN:` note, like any other command only the
  user can run.

**Host side.**

- Only hosts labelled `aws` get a broker key (`/run/qm-secrets/aws-broker-key`,
  separate from the Beads key so each can be revoked alone). With the key
  present, the entrypoint writes `~/.aws/config` with one profile per allowed
  name, each `credential_process = qm-aws-creds <name>`. With no key, there is
  no AWS config at all.
- `qm-aws-creds` passes the bead (`QM_BEAD`, which dispatch exports into the
  pane alongside `BEADS_ACTOR`) and prints the broker's `credential_process`
  JSON. The AWS CLI and SDKs cache it until it expires and then ask again.
- If the desktop is unreachable it exits non-zero with
  `aws broker: desktop unreachable; AWS is unavailable on this host until it's back`.
  Nothing falls back to other credentials.
- Kubernetes: `aws eks update-kubeconfig --profile viewer` writes kubeconfigs
  whose exec plugin runs `aws eks get-token` with the brokered profile, so
  kube access follows the broker's scope and revocation. Reaching a private
  cluster endpoint still needs network access (§10); without it, kube work
  stays on `local`.

**Revocation.** Remove the host's line from the desktop's `authorized_keys`.
Credentials already issued stay valid until they expire (at most
`max_minutes`).

**Future: IAM Roles Anywhere.** For a host that must use AWS while the desktop
is off, IAM Roles Anywhere gives it its own short-lived credentials: a
per-host X.509 certificate from a private CA, a trust anchor, and a profile
that maps only to the viewer/developer roles. It costs a CA to run (a managed
private CA, or a self-managed one with a CRL for revocation) and IAM changes,
and the certificate is a long-lived secret on the host. Not needed while every
remote host sits next to an always-on desktop.

## 10. Things that need the host

**Containers inside workers.** Some tests start containers (a Prometheus
rules check run via Docker, testcontainers). Options:

| | Security | Notes |
|---|---|---|
| Mount the host's **rootful** Docker socket | Equivalent to root on the host: defeats the sandbox | Don't |
| **Rootless Podman socket** of the host user (`podman.socket`, Docker API), mounted into the container as `/run/docker.sock` with `DOCKER_HOST` set (sibling containers) | Containers run as the host user; a worker can start containers with the user's privileges and mount what the user can read | **Use this** where needed; the host label `docker` marks it. The `docker` CLI and testcontainers talk to it unchanged (testcontainers may need `TESTCONTAINERS_RYUK_DISABLED=true` or the socket override; phase 0 checks). The secrets directory is readable by such containers; accept, or keep `docker` off hosts holding broker keys |
| Podman-in-container (nested, rootless) | Best isolation; no host socket | Needs `/dev/fuse`, subuid ranges in the container, and is slower; worth trying for the image as a later improvement |

`server` needs `/etc/subuid`/`/etc/subgid` entries for rootless Podman (a
root step the user does once; §18).

**Cluster / VPN access.** The Cloudflare WARP client is on `local` only.
Workers that need private network access (kube, internal MCP servers)
should, for now, carry the `kube` label and run on `local`. If remote workers
need it later: WARP's headless mode with a service token in a sidecar
container sharing the worker's network namespace, scoped to the minimum
routes, rather than a user-enrolled client on the host.

**Browser automation.** Chrome-driven work stays on `local` (`browser`).

## 11. Security posture

Upside versus "install everything on the host":

- Workers run inside a container with no access to the host's home, SSH
  keys, browser profiles or other credentials; only the volumes above and the
  read-only secrets mount.
- The host's sshd is reached only from `local`; the only reverse path is a
  forced command that runs `bd`.
- A host is disposable: wipe the volumes, revoke its GitHub token, signing
  key, agent sessions and proxy key, and nothing else is affected.
- Running the same image on `local` (optional; §13) gives desktop workers the
  same sandbox.

Residual risks to accept knowingly: the bd proxy gives full read/write on all
beads (the same as any worker today); a worker with the rootless Podman socket
can act as the host user; the GitHub token lets a worker push to every repo
in its scope.

## 12. Health checks and draining

Two kinds of check, both run by the ticker on `local`:

- **Liveness, every tick** (15s): the per-host `herdr agent list` call. A
  failure marks the host `unreachable`, keeps its last agent snapshot (so
  threads don't flap to `agent_gone`), and after 3 consecutive failures writes
  one `host_down` inbox event and a notification. Recovery writes `host_up`.
- **Readiness probe, every 5 minutes** (`hostExec`): `MemAvailable`, load,
  free disk on the volumes, `bd show <a known bead>` through the proxy (Beads
  reachable), `gh auth status`, container image digest. Results go to
  `state.json` `hosts.<name>` and feed scheduling (§2). A failed sub-check
  makes the host not schedulable and shows why (`server: bd proxy failing`).

`agent_gone` is not emitted for threads on an unreachable host; that's what
keeps a network blip from looking like every remote worker died.

Draining: `quartermaster host drain <name>` / `undrain <name>` set a flag in
state. A draining host gets no new dispatches; existing workers continue.
`quartermaster host list` shows per host: enabled, health, draining, image,
slots used/free, `MemAvailable`, workers.

## 13. Running the same image on `local`

`local` can run its own `qm-worker` container with the same volumes layout
under the desktop user, registered as a Herdr machine too (`local-c`), while
the coordinator and ticker stay in the desktop's own Herdr. Then:

- `local` (host process) keeps the work that needs the desktop's credentials
  and network: labels `admin-ok`, `aws`, `kube`, `browser`;
- `local-c` (container) runs everything else, behaving exactly like `server`.

This is a later phase; it's opt-in so nothing about today's desktop workflow
changes until it's proven on `server`.

### 13.1 Moving the desktop to Podman (later)

Not now: only after `server` works end to end. Then, as its own phase:

1. **Inventory** what uses Docker on the desktop: repository tests that start
   containers (a Prometheus rules check run through `docker run`,
   testcontainers suites), local image builds (`docker build`/`buildx`,
   multi-arch), any compose files or kind clusters, credential helpers in
   `~/.docker/config.json` (registry logins, `docker-credential-*`), scripts
   and Makefiles that call `docker`, and the `docker` group membership.
2. **Compatibility:** install Podman alongside Docker; enable the user
   `podman.socket` and point `DOCKER_HOST` at it per shell to run each
   inventoried workload against Podman first; then switch with the
   `podman-docker` shim (a `docker` command backed by Podman) and a global
   `DOCKER_HOST`. Buildx-specific flags and kind need individual checks.
3. **Cut over** with Docker installed but disabled
   (`systemctl disable --now docker.service docker.socket`), the worker
   container moved to a Quadlet unit like `server`'s.
4. **Rollback:** re-enable `docker.service`/`docker.socket`, unset
   `DOCKER_HOST`, remove `podman-docker`; images and volumes of each runtime are
   separate, so nothing is lost either way.

## 14. Views

- **Context:** a `## Hosts` section first: `local 3/5 · server 1/3 · local-c
  —` plus anything unhealthy or draining. Thread lines gain the host when it
  isn't `local`: `- app-c71m · checks failing · PR #405 · claude working @server · …`.
- **Board:** a host column; `/server` filters by host (it's part of the
  filter's match fields); the detail pane shows the host, its health and the
  worktree path on it. `H` cycles a host filter.
- **Sidebar:** Herdr's own `machine` token on the row already shows the host;
  the coordinator's summary row adds unhealthy hosts (`server down`).
- **Report:** unchanged; the host is incidental to what shipped.

## 15. Resume, focus, inbox, needs_me end to end

- **Dispatch** → `host:server` label, worktree and agent on `server`, brief
  sent via `--machine server`. The brief adds one line for remote hosts: the
  host name, and "push your branch at milestones" so work survives a host
  loss.
- **Worker reporting** → `bd note`/`bd update` through the proxy into the one
  DB; the ticker sees them on its next tick as today.
- **needs_me / RUN:** unchanged; the user answers in the coordinator, which
  relays with `herdr --machine server agent prompt <name> …` (the coordinator
  guide gains "use `quartermaster prompt <bead> <text>`", a wrapper that
  routes by host, so it never has to know the machine).
- **Focus** (`↵` on the board, focus-clipboard) → Herdr's TUI focuses a
  remote pane through the machine profile (`agent focus` forwarded; to verify
  in phase 0); if it can't, focus switches the TUI to that machine's view.
- **Resume** → per host, reading `host:` labels; `--host` limits it.
- **Inbox** → events carry the host; nothing else changes.
- **Resolve / sweep** → per host (§3).

## 16. Failure modes

| Failure | Effect | Handling |
|---|---|---|
| Host down / unreachable | Its workers stop; their state in Beads is intact; worktrees and sessions are on the host's volumes | `host_down` event; threads show `host unreachable`; when it returns, `resume --host`. To move a thread: `dispatch <bead> --host local` re-creates the worktree from the pushed branch (`WorktreeOpen` path), label updated |
| Network blip | Ticker misses ticks for that host | Kept snapshot; no `agent_gone`; Herdr forwarding may have applied a mutation that "failed": dispatch re-lists before retrying |
| Desktop asleep / off | Remote workers lose `bd` and the coordinator; they keep coding; `bd` calls fail | The brief tells workers to retry a failed `bd note` rather than proceed silently; a later improvement is a small local queue in the shim that replays notes. Practical mitigation: don't suspend the desktop while remote workers run (an inhibitor held by the ticker while any remote worker is active) |
| Beads proxy key revoked/misconfigured | `bd` fails on that host | Readiness probe marks the host unschedulable before more work lands there |
| Image regression | New container misbehaves | Canary one host; roll back by tag |
| Disk full on a host | Builds fail | Probe threshold makes it unschedulable; `sweep --host` clears merged worktrees; cache volume can be pruned |
| Herdr version skew | Forwarding refuses | Image pins Herdr; updating Herdr on `local` is paired with an image bump |

## 17. What stays on `local`

- The coordinator, ticker, inbox, notifications, GitHub/Linear polling,
  verify.
- The Beads DB and its S3 sync.
- Work labelled `admin-ok` (admin and break-glass AWS are never brokered),
  `browser`, and anything needing WARP; `aws`/`kube` work on hosts without a
  broker key.
- The AWS SSO session itself: remote hosts only ever see short-lived role
  credentials minted from it (§9.1).
- Releases that sign tags with the user's own key.

## 18. Bootstrap: adding a host

`quartermaster host bootstrap <name>` runs from `local`, reads the host's
config table, and executes an idempotent checklist over SSH. Each step is
check-then-apply and prints `ok`, `applied` or `needs you: <command>`;
re-running converges. Steps that need the user stop with the exact command
(and, under quartermaster, become a `RUN:` note).

| # | Step | Check | Apply | Who |
|---|---|---|---|---|
| 1 | SSH from `local` | `ssh -o BatchMode=yes <ssh> true` | — | **you** (key in the host's `authorized_keys`) |
| 2 | Rootless Podman | `podman info` reports rootless; `systemctl --user is-active podman.socket` | enable `podman.socket` (user unit) | **you** for the root part: install `podman passt`, `usermod --add-subuids/--add-subgids`, `loginctl enable-linger`, disable any rootful `docker.service`/`docker.socket` |
| 3 | Host directories | `~/.config/qm-worker/{secrets,volumes}` exist, 0700 | `mkdir -p`, `chmod` | auto |
| 4 | Registry access | `podman pull <image>` | `podman login ghcr.io` with a per-host read-only (`read:packages`) token, runtime only | **you** (create token) |
| 5 | Secrets present | files exist | print what to create | **you**: GitHub fine-grained token; generate the signing key (`ssh-keygen` on the host) and add it to GitHub as a signing key |
| 6 | Beads proxy | `bd` via shim answers `bd show <id>` | generate `qm-beads` key on host, append forced-command line to `local`'s `authorized_keys`, pin `local`'s host key | auto (it edits `local`'s `authorized_keys`: you confirm once) |
| 7 | Container | `systemctl --user is-active qm-worker`, image digest matches | write the Quadlet unit (§6), `systemctl --user daemon-reload && systemctl --user start qm-worker` | auto |
| 8 | Herdr wrapper on host + machine profile on `local` | `herdr --machine <name> agent list` | install wrapper; `herdr machine add --label <name> <ssh>` | auto (Herdr prompts if a server is incompatible: answer no) |
| 9 | Repos | each `repos` entry cloned, `origin` reachable | `gh repo clone` inside the container | auto |
| 10 | Agent logins | `claude`/`codex` auth status | print `podman exec -it qm-worker claude /login` etc. | **you** (OAuth, once per host) |
| 10a | AWS broker (hosts labelled `aws` only) | `qm-aws-creds viewer` returns credentials; `aws sts get-caller-identity --profile viewer` shows the viewer role | generate `aws-broker` key on host; append `command="quartermaster aws-broker --host <name>",restrict` line to `local`'s `authorized_keys`; add the host to `[aws_broker.hosts]` | auto (you confirm the `authorized_keys` line once) |
| 11 | Toolchains warm | `mise install` for each repo; `go mod download` | run them | auto |
| 12 | Smoke | dispatch a no-op bead with `--host <name>`, check it notes and closes | — | auto |

`server.local` today needs, from you: steps 2 (install Podman, subuid/subgid,
disable the rootful Docker units; linger is already on), 4, 5 and 10. Everything else is the
script.

## Phases and effort

Estimates are agent-days of focused work, each phase one PR or a small
stack.

| Phase | What | Effort | Needs you |
|---|---|---|---|
| 0. Spike | Hand-run Herdr 0.9.1 machine forwarding against a container on `server`: agent list/start/prompt/read/rename/focus, worktree create with `~/` paths, report-metadata + agent-view ordering, wrapper vs socket | 0.5 | `herdr machine add` approval |
| 1. Image | Manifest, Dockerfile, CI build/push to GHCR, smoke job, secret scan | 1.5–2 | GHCR package visibility |
| 2. Beads proxy | `quartermaster bd-proxy` (allowlist, actor), shim, tests | 0.5–1 | approve the `authorized_keys` line |
| 3. Host-aware Herdr + registry | `Herdr.onMachine`, `Agent.Host`, config `hosts`, `hostExec`, `dispatch --host`, `host:` label, outbox per host | 1.5 | — |
| 4. Ticker/resume/resolve/sweep fan-out | parallel listing, stale snapshots, unreachable handling, per-host resolve/sweep/resume | 1.5–2 | — |
| 5. Scheduler, health, drain, views | `schedule = auto`, probes, `host list/drain/undrain/update`, board/context host views | 1.5 | — |
| 6. Bootstrap | `host bootstrap` checklist, docs | 1 | steps 2, 4, 5, 10 on `server` (~30–45 min) |
| 7. AWS credential broker | `quartermaster aws-broker` (per-host allowlist, hard admin deny, role check, lifetime cap, request log, fail closed), `qm-aws-creds` and AWS config in the image, `QM_BEAD` in the pane env, kubeconfig via `aws eks get-token`, tests | 1–1.5 | permission-set session durations (or a trust-policy change for chaining); `aws sso login` on the desktop |
| 8. Optional | `local-c` container on the desktop; nested Podman; WARP sidecar; IAM Roles Anywhere | 1–2 | — |
| 9. Desktop on Podman (after `server` works end to end) | Docker-usage inventory, Podman side by side via `DOCKER_HOST`, `podman-docker` cut-over, rollback (§13.1) | 1–1.5 | approve the cut-over |

Total for phases 0–7: about **9–11.5 agent-days**.

## Decisions

Made 2026-10-15; each took the recommended option except 2 and 4, revised
the same day.

1. **Beads transport**: a forced-command SSH proxy to the desktop (§5,
   option A). A Dolt sql-server stays the fallback if latency or lock
   contention shows up.
2. **Container runtime on remote hosts**: rootless Podman with
   `--userns=keep-id`, the worker container as a Quadlet user unit updated
   with `podman auto-update`, and the user `podman.socket` for tests that use
   the Docker API (§6, §10). Needs a one-time root step for the packages and
   subuid/subgid. The desktop may move to Podman later as its own phase
   (§13.1). (First decided as rootless Docker; revised.)
3. **Commit signing on remote hosts**: a per-host SSH signing key,
   registered on GitHub as a signing key named for the host.
4. **AWS on remote hosts**: through a desktop credential broker (§9.1).
   Hosts labelled `aws` get short-lived (15–60 min) viewer/developer
   credentials minted from the desktop's SSO session over a forced-command
   SSH key; admin is never brokered; every request is logged; it fails
   closed when the desktop is offline or its SSO session has expired. IAM
   Roles Anywhere is the later option for hosts that must work with the
   desktop off. (First decided as "no AWS on remote hosts"; revised.)
5. **Scheduling**: `schedule = "manual"` first (remote only with `--host` or a
   `host:` label); `auto` comes after remote workers have run for a while.
6. **Image recipe**: lives in the dotfiles repo, next to the dotfiles it
   bakes in.
