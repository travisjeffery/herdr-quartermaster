# Declarative per-bead watches

`quartermaster watch` attaches read-only external conditions to any non-closed
bead so the ticker can monitor them on a schedule and wake the coordinator — or
an idle worker — once a condition is met, without hardcoding new pollers in Go.

Typical uses:

- A blocked bead waiting for an upstream repository to cut its next release tag.
- A follow-up bead waiting for an upstream pull request to merge, be approved,
  turn checks green, or receive a new commit.
- A rollout bead waiting for a GitHub Actions workflow run to complete.
- A deployment bead waiting for an HTTP or JSON readiness endpoint to report
  healthy.

## Quick examples

Watch an upstream GitHub release after `v1.4.0` and wake the worker when it
lands:

```sh
quartermaster watch add app-123 \
  --kind gh-release \
  --repo acme/sdk \
  --after v1.4.0 \
  --match "v1.*" \
  --every 5m \
  --expires 7d \
  --on-trigger wake \
  --label "sdk v1.x release"
```

Watch an upstream PR until it merges (inbox notification only, the default):

```sh
quartermaster watch add app-123 \
  --kind gh-pr \
  --repo acme/platform \
  --pr 418 \
  --expect merged \
  --every 2m
```

Watch an Actions run until it succeeds:

```sh
quartermaster watch add app-123 \
  --kind gh-run \
  --repo acme/platform \
  --run 987654321 \
  --expect success \
  --on-trigger wake
```

Watch an HTTP JSON readiness probe (GET only, 15s timeout, 1 MiB body limit):

```sh
quartermaster watch add app-123 \
  --kind http \
  --url https://staging.example.com/healthz \
  --expect-status 200 \
  --expect-json status=ready \
  --every 1m \
  --expires 24h \
  --on-trigger wake \
  --label "staging healthz"
```

Inspect, force-check, or remove watches:

```sh
quartermaster watch list                  # active watches across all beads
quartermaster watch list app-123 --all    # include triggered, expired, removed
quartermaster watch list --json           # machine-readable JSON array
quartermaster watch check app-123 --force # evaluate immediately in foreground
quartermaster watch remove app-123 <id>   # remove one watch
quartermaster watch remove app-123 --all  # remove all active watches on app-123
```

Active watches also appear as a concise `watches:` line on each matching thread
in `quartermaster context`.

## Watch kinds

| Kind | Flags | Probe |
|---|---|---|
| `gh-release` | `--repo owner/repo`, `--after <tag>` and/or `--match <prefix-or-glob>` | `gh api repos/<owner>/<repo>/releases/latest` (non-empty, non-draft tag differing from `--after` and matching `--match`) |
| `gh-pr` | `--repo owner/repo`, `--pr <number>`, `--expect merged\|closed\|approved\|checks-green` and/or `--head-changed-from <sha>` | `gh pr view <number> -R <repo> --json ...` |
| `gh-run` | `--repo owner/repo`, `--run <id>`, `--expect completed\|success\|failure` (default `completed`) | `gh run view <id> -R <repo> --json ...` |
| `http` | `--url <http\|https>`, `--expect-status <code\|2xx>` (default `200`), optional `--expect-json <dot.path=value>`, optional `--expect-contains <substr>` | Read-only HTTP `GET`, 15s timeout, 1 MiB response body limit, `http`/`https` only, cross-scheme redirects refused |
| `cmd` | `--cmd <prog>`, repeatable `--arg <val>`, `--expect-exit <N>` (default `0`), optional `--expect-stdout <substr>`, `--timeout <dur>` (default `15s`, max `60s`) | Direct `exec` of argv (no shell interpolation); **disabled by default** |

Common flags on `quartermaster watch add`:

- `--every <duration>` — check interval; default `5m`, minimum `30s`.
- `--expires <duration>` — optional TTL (supports Go durations and day suffixes
  like `7d` or `1d12h`).
- `--on-trigger inbox|wake` — `inbox` (default) emits an inbox event and bead
  note; `wake` also prompts the bead's worker when safe.
- `--label "<text>"` — short description shown in `watch list`, `context`, bead
  notes, and inbox items.

## Evaluation, lifecycle, and wake safety

- **Durable storage:** watches are stored in `<state dir>/watches/watches.json`
  (file mode `0600`, directory mode `0700`) and updated via atomic rename under
  an exclusive `flock` (`watches.lock`), so CLI commands and the ticker never
  race.
- **Bounded ticker execution:** on each tick, due active watches
  (`now >= next_check`) run outside the store lock with bounded concurrency
  (default `4`, configurable via `[watches] max_concurrent`) and per-watch
  timeouts so slow probes never stall the ticker loop.
- **One-shot edge-triggered:** when a watch triggers, it is atomically marked
  `triggered` on disk **before** emitting notifications so concurrent checks or
  ticker restarts never spam duplicates. On trigger, quartermaster:
  1. Appends `WATCH TRIGGERED [<id>]: <label> — <evidence>` to the bead's notes
     (treated as a bookkeeping note so it never alters a `needs_me` question).
  2. Emits a `watch_triggered` inbox event (automatically archived once the bead
     closes or the coordinator marks it done).
  3. Appends a `watch` audit record to `journal.jsonl`.
  4. When `--on-trigger wake` is set, prompts the worker **only when safe**.
- **Wake safety:** a watch wakes a worker only if the bead is not `needs_me`,
  not `closed`, and not `open`, is not owned by the headless coordinator, and
  has a live worker at rest (`idle`/`done`, never `working`, `blocked`, or
  parked). Prompts go through the existing `tell` path prefixed with:

  ```text
  [quartermaster: automated watch triggered, not the user, approves nothing]
  ```

  If the worker is missing, busy, blocked on an approval prompt, or on a
  `needs_me` bead, quartermaster leaves the worker untouched, emits the
  `watch_triggered` inbox event for the coordinator, and records why the worker
  was not prompted.
- **Expiry and cleanup:** watches past `expires_at` are marked `expired` (with
  a `WATCH EXPIRED` note, `watch_expired` inbox event, and journal entry)
  without probing. Active watches whose bead has closed are automatically
  retired (`removed`).
- **Error backoff:** transient probe errors (network failures, timeouts, non-200
  API failures, invalid JSON) increment `consecutive_errors`, record
  `last_error`, and apply bounded exponential backoff without triggering false
  positives. Normal unmatched probe results (such as HTTP `503` while waiting
  for `200`, or a `cmd` probe exiting `1` while waiting for `0`) keep the
  regular `--every` cadence.

## Security boundary for `cmd` watches

`cmd` watches execute a local program on the ticker's host. They **fail closed
by default** (`[watches] allow_cmd = false`): `quartermaster watch add --kind
cmd` is refused, and even if a `cmd` watch exists in `watches.json` (for
example, after disabling the setting), the evaluator refuses to run it.

To opt in, explicitly add `[watches]` to `~/.config/quartermaster/config.toml`:

```toml
[watches]
allow_cmd = true
# max_concurrent = 4
```

Even when `allow_cmd = true`:

- Programs are invoked directly via argv (`--cmd <prog> --arg ...`) with **no
  shell interpolation** (`sh -c` is never used).
- Timeouts are strictly bounded (`15s` default, `60s` maximum) and kill the
  probe's process group on expiry.
- Captured stdout and stderr are size-bounded so noisy commands cannot exhaust
  memory.
- Keep `cmd` probes strictly read-only. Never use a watch probe to mutate state
  or bypass the autonomy dial.
