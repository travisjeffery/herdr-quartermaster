# Outbound notification sinks (`[notify]`) and event feed

Quartermaster records real-time, privacy-safe notification events in a durable
bounded feed (`<state>/notify/events.json`, served over Tailscale at
`GET /events` when `[tap]` is enabled and inspectable locally with
`quartermaster notify events`) and can also push them to opt-in external HTTP
webhooks and local commands when ticker milestones, blockers, or headless inbox
runner updates occur.

Outbound sinks under `[notify]` are **opt-in and empty by default**, so
upgrading Quartermaster sends no external webhooks or commands until you add at
least one `[[notify.webhooks]]` or `[[notify.commands]]` sink. Both the durable
event feed and outbound sinks run **independently of `[desktop]`**, allowing
headless servers to set `[desktop] enabled = false` while keeping the feed,
`GET /events`, webhooks, and commands active.

## Supported events

| Event | Source | When it fires |
| --- | --- | --- |
| `pr_merged` | Ticker | A tracked PR transitions to merged (baselined on first sight) |
| `pr_green` | Ticker | An open non-draft PR's checks pass with no conflicts, requested changes, or open threads |
| `pr_ready` | Ticker | An open PR becomes ready to merge (takes precedence over `pr_green`) |
| `needs_me` | Ticker | A bead enters `needs_me` or posts a new question/`RUN:` request |
| `blocked` | Ticker | A bead enters `blocked` status |
| `worker_blocked` | Ticker | A worker stops at an approval/question prompt |
| `inbox_runner` | Headless inbox runner | Items are handled (`acked > 0`) or a headless run fails/times out |
| `test` | `quartermaster notify test` | Synthetic diagnostic event sent only by the CLI test command |

## Configuration

Add `[[notify.webhooks]]` and/or `[[notify.commands]]` to
`~/.config/quartermaster/config.toml`:

```toml
# Optional master switch (defaults to true whenever at least one sink is defined;
# set enabled = false to pause all outbound sinks without deleting them).
[notify]
enabled = true

# Webhook sink (JSON envelope, secret URL & auth header read from environment)
[[notify.webhooks]]
name = "ops-webhook"
url_env = "QUARTERMASTER_NOTIFY_URL"
headers = { X-Source = "quartermaster" }
header_envs = { Authorization = "QUARTERMASTER_NOTIFY_AUTH" }
events = ["pr_merged", "pr_ready", "needs_me", "inbox_runner"]
format = "json"            # "json" (default) or "text"
timeout_seconds = 5        # default 5, capped at 30
cooldown_seconds = 30      # per-sink coalesce window (default 30; 0 disables)

# Plain-text webhook sink (e.g. ntfy)
[[notify.webhooks]]
name = "ntfy"
url_env = "NTFY_TOPIC_URL"
format = "text"            # sends "<title>\n<body>" as text/plain
timeout_seconds = 5
cooldown_seconds = 30

# Local command sink (executed directly without shell evaluation; JSON on stdin)
[[notify.commands]]
name = "bridge-cli"
argv = ["/usr/local/bin/notify-bridge", "--stdin"]
events = ["needs_me", "blocked", "worker_blocked", "pr_merged"]
timeout_seconds = 5        # kills entire process group (Setpgid) on expiry
cooldown_seconds = 30
```

### Secret hygiene (`url_env` and `header_envs`)

- Prefer `url_env` over literal `url` whenever a webhook URL contains an
  embedded secret path or token (Slack incoming webhooks, Discord webhooks,
  private ntfy topics, query-string keys).
- Prefer `header_envs` over literal `headers` for `Authorization`, API keys, or
  bearer tokens so secrets never live in `config.toml`.
- Set exactly one of `url` or `url_env` per webhook. Only `http://` and
  `https://` schemes are allowed; other schemes or empty resolved `url_env` /
  `header_envs` values fail closed at runtime without sending a request.
- Raw webhook URLs, paths, query strings, userinfo, and header values are
  **never** written to `<state>/notify/status.json`, `ticker.log`,
  `quartermaster notify status`, or `quartermaster notify test`. Status and
  diagnostics display only `env:<VAR>` for `url_env` or `scheme://host` for
  literal URLs, plus header names without values.

## Payload schema (version 1)

JSON webhooks (`format = "json"`) and command sinks (on `stdin`) receive a
versioned `version: 1` JSON envelope:

```json
{
  "version": 1,
  "event": "pr_merged",
  "at": "2026-11-24T20:00:00Z",
  "title": "Quartermaster: PR merged",
  "body": "task-42: acme/app#42 merged — Add caching layer",
  "inbox_url": "http://100.64.1.2:8642/inbox",
  "items": [
    {
      "bead": "task-42",
      "reason": "pr_merged",
      "summary": "task-42: acme/app#42 merged — Add caching layer",
      "pr": "acme/app#42"
    }
  ]
}
```

For `needs_me` items, `items[].class` (optional integer `0`–`9`, omitted when
unavailable) is extracted from live `CLASS: N` notes or structured question
cards so consumers can distinguish urgent (`class >= 7`) from batched items
without an extra round trip.

For `inbox_runner` events, the envelope includes `runner`:

```json
{
  "version": 1,
  "event": "inbox_runner",
  "at": "2026-11-24T20:05:00Z",
  "title": "Quartermaster: headless inbox runner",
  "body": "Handled 2/2 inbox items (merged, ready_for_review).",
  "runner": {
    "exit_status": "ok",
    "acked": 2,
    "eligible": 2,
    "kinds": ["merged", "ready_for_review"],
    "duration_ms": 4820
  }
}
```

Plain-text webhooks (`format = "text"`) receive `Content-Type: text/plain; charset=utf-8`
with body:

```text
<title>
<body>
```

### Privacy & sanitization guarantees

- Every text field (`title`, `body`, `items[].summary`, `items[].bead`,
  `items[].pr`, and persisted error summaries) passes through the shared privacy
  sanitizer (`desktop.go`), stripping or redacting bearer/basic tokens, API
  keys, passwords, cookies, private keys, generic URLs, ANSI escapes, control
  characters, and signed `/tap/<token>` links before dispatch or persistence.
- `blocked` and `worker_blocked` items always use fixed generic wording —
  pending approval commands, subprocess output, and transcripts never leave the
  machine.
- `inbox_url` is populated **only** when `[tap]` is enabled and validated as
  the non-secret bookmarkable `/inbox` path (`http://<tailnet-ip>:<port>/inbox`
  with no query string or token). Signed `/tap/<token>` bearer URLs are never
  emitted in outbound notifications or stored in the durable event feed.

## Durable event feed (`<state>/notify/events.json` and `GET /events`)

Quartermaster maintains a bounded, durable notification feed in
`<state>/notify/events.json` (mode `0600`, directory `0700`, `flock`-protected,
atomic rename + `fsync`):

- **Always recorded:** Sanitized ticker milestones/blockers (`pr_merged`,
  `pr_green`, `pr_ready`, `needs_me`, `blocked`, `worker_blocked`) and headless
  inbox runner outcomes (`inbox_runner` on `acked > 0`, failure, or timeout) are
  appended automatically — even when `[desktop] enabled = false` and no
  `[[notify.webhooks]]` or `[[notify.commands]]` sinks are configured. Synthetic
  events from `quartermaster notify test` are never written to the feed.
- **Monotonic sequence & ring buffer:** Each event gets a monotonically
  increasing integer `seq` (`1, 2, 3, ...`) persisted as `latest_seq` across
  restarts and evictions. The file retains the most recent 500 events; each
  entry carries `seq` alongside the sanitized `version: 1` envelope fields
  (`version`, `event`, `at`, `title`, `body`, `inbox_url`, `items`, `runner`).
- **Tailnet polling (`GET /events`):** When `[tap]` is enabled, the existing
  Tailscale-bound tap server exposes `GET /events?since=<seq>&limit=<N>&event=<kinds>`
  behind the exact same fail-closed auth guard as `/inbox`, `/inbox.json`, and
  `/tap-links/` (tailnet IP + `tailscale whois` + `[tap] devices`; disallowed
  sources — including the host machine itself — receive `403` and log
  `TAP REFUSED:` before reading any state). Only `GET` is allowed (`405` on
  other methods), requests are strictly read-only, and responses carry
  `Content-Type: application/json; charset=utf-8` and `Cache-Control: no-store`.
- **Query parameters** (malformed values return `400` JSON `{"error": ...}`):
  - `since` (optional non-negative integer, default `0`): return events with
    `seq > since` in ascending `seq` order.
  - `limit` (optional positive integer, default `100`, capped at `500`; `<= 0`
    or non-integer values return `400`).
  - `event` (optional comma-separated list of known event kinds: `pr_merged`,
    `pr_green`, `pr_ready`, `needs_me`, `blocked`, `worker_blocked`,
    `inbox_runner`; unknown kinds return `400`).
- **Response shape and cursor/truncation semantics:**
  `{"version":1,"latest_seq":<uint64>,"next_since":<uint64>,"has_more":<bool>,"truncated":<bool>,"events":[...]}`
  - `next_since` equals the last returned event's `seq` if `events` is
    non-empty, or `latest_seq` otherwise (so a fresh client starting at
    `?since=0` or a client whose cursor fell ahead of a reset state dir syncs
    cleanly).
  - `has_more` is `true` when additional matching events exist beyond `limit`.
  - `truncated` is `true` when `since > 0` and either `since` predates the
    oldest retained event (`oldest_seq > 1 && since < oldest_seq - 1`) or
    `since > latest_seq`, signaling the client should perform a snapshot sync
    (`quartermaster context --compact` / `quartermaster inbox --questions`).

See [docs/approval-taps.md](approval-taps.md#cursor-based-notification-feed-get-events)
for the full tailnet endpoint contract and example payload.

## Delivery semantics & state

- **Non-blocking ticker dispatch.** Inside `quartermaster ticker run`, outbound
  sinks execute asynchronously with bounded concurrency and per-sink timeouts
  (capped at 30s) so slow or unreachable endpoints never stall the ticker loop.
  Command sinks run directly via `exec.CommandContext` (no shell evaluation) in
  their own process group (`Setpgid: true`), which receives `SIGKILL` on timeout
  or ticker shutdown.
- **Per-sink cooldown without dropping across restarts.** Durable delivery state
  lives in `<state>/notify/status.json` (mode `0600`, directory `0700`,
  `flock`-protected, atomic rename + fsync). Events arriving while a sink is
  inside `cooldown_seconds` are persisted in that sink's pending queue (recording
  `skipped_cooldown`) and coalesced into the next delivery once cooldown expires
  — even across ticker restarts.
- **Delivery states:** `submitted`, `failed`, `timeout`, `skipped_cooldown`.

## CLI verification

```sh
# Inspect configured sinks (redacted), subscribed events, cooldown, and last delivery state:
quartermaster notify status
quartermaster notify status --json

# Send a synthetic event: "test" notification to all configured sinks (or one sink):
quartermaster notify test
quartermaster notify test --sink ops-webhook
quartermaster notify test --json

# Inspect the local durable notification event feed (strictly read-only):
quartermaster notify events
quartermaster notify events --since 10 --limit 50 --event needs_me,pr_merged --json
```

`quartermaster notify test` exits nonzero if no sinks are configured, if
`--sink` does not match a configured sink, or if any targeted sink fails or
times out. It never writes synthetic `test` events into `<state>/notify/events.json`.

## Rollback

To disable outbound notifications immediately:

1. Set `enabled = false` under `[notify]` in `~/.config/quartermaster/config.toml`
   (or remove all `[[notify.webhooks]]` / `[[notify.commands]]` blocks).
2. Restart the ticker so the updated configuration takes effect:

```sh
quartermaster ticker stop && quartermaster ticker start
```

Stopping the ticker cancels any in-flight webhook requests and terminates any
in-flight command sink process groups. `<state>/notify/status.json` can be
inspected or deleted at any time without affecting Beads, workers, or desktop
notifications.
