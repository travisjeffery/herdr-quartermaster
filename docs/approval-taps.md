# Approval taps: answering a worker's prompt from your phone

A worker stopped at an approval prompt used to mean typing an answer into the
coordinator session ("bead: 1"), which meant being at a keyboard with that
session open. Approval taps make the same decision one tap per option on a
phone, with the guards of `quartermaster answer` unchanged. The same links
also answer a worker's `needs_me` question when it is written with
enumerated options (see "needs_me questions" below).

## How it works

```toml
[tap]
enabled = true
devices = ["iphone181"]   # Tailscale node names allowed to tap (`tailscale status`)
# port = 8642             # on this machine's Tailscale address only
```

With `[tap]` on, the ticker also serves a small HTTP endpoint bound to this
machine's Tailscale IPv4 address (never `0.0.0.0`; no tailnet, no listener).
When a worker's parsed approval prompt reaches the inbox — after
`prompt_debounce_seconds` (default 90) of it continuously showing, unless
that knob is 0 — the event carries
the prompt's own lines — the actual command, not only the agent's description
of it — and one signed link per option. `quartermaster tap links <bead>`
mints the same links at any time. The coordinator relays the event as it
already relays prompts, so the links reach the phone through the chat app
already in use; nothing new pushes.

Following a link shows a confirm page with the prompt **re-read live from the
worker** and one large button for the linked option. Pressing it answers
through the same code as `quartermaster answer <bead> <choice> --expect
<hash> --by TJ`: the prompt is read again, a changed prompt is refused, the
keys are sent, the wait-for-clear runs, and the bead gets the usual audit
note, extended with the transport and its identity evidence:

```
PROMPT ANSWERED: 1 Yes to Bash command: … [bff9292a] by TJ via tap (device iphone181)
```

The confirm step is deliberate. A bare GET that answered would let a chat
client's link unfurl or prefetch approve a command nobody read, and a
fat-fingered notification tap is one extra press away from releasing
anything. The link is still one tap target per option; the page it opens is
the review, and its button is the answer.

## Tailnet Web Inbox (`/inbox`)

The same Tailscale-bound tap server also hosts a persistent, mobile-first
**Tailnet Web Inbox** with zero external assets (inline HTML/CSS only):

- **`GET /`** redirects to `/inbox` after authentication.
- **`GET /inbox`** renders a bookmarkable mobile/desktop queue page with a
  manual Refresh button, an opt-in 30s auto-refresh toggle, summary counts,
  and a `Viewing as TJ from <device>` identity footer.
- **`GET /inbox.json`** returns the same live queues as JSON (keeping tool
  prompt hashes separate from `needs_me` question hashes, and never exposing
  `tap/secret`).
- **`quartermaster tap url`** prints `http://<tailscale-ip>:<port>/inbox` when
  `[tap]` is enabled with a valid `devices` allowlist and Tailscale resolves;
  it fails closed otherwise.

Every route (`/`, `/inbox`, `/inbox.json`, `/inbox/plan/<digest>`,
`/tap-links/<bead>`, `/events`, and `/tap/<token>`) enforces the exact same fail-closed auth guard: tailnet source
IP, `tailscale whois` lookup, and explicit membership in `[tap] devices`. Any
non-tailnet source, failed `whois`, or disallowed device — including the host
machine itself — receives `403` and is journalled as `TAP REFUSED: …` before
any state is read or touched. `GET` requests never mutate state, spend nonces,
answer prompts, or approve plans.

### On-demand tap links (`GET /tap-links/<bead>`)

Clients on an allowed tailnet device can mint fresh, 15-minute one-time
`/tap/<token>` links on demand for one bead at a live approval prompt or live
`needs_me` question (with or without an `OPTIONS:` block):

- **`GET /tap-links/<bead>`** returns JSON (`bead`, `kind`, `question`,
  `expires`, `links`, plus `free_text: {url, max_chars}` for `needs_me`
  questions) with `Cache-Control: no-store`.
- Every call mints fresh nonces without invalidating earlier sets; the one-use
  nonce spend and live re-read keep answering at-most-once.
- Non-live targets (missing or closed bead, `RUN:` bead, worker already working
  again, answered question, or no parseable prompt/question) return `404` JSON
  `{"error": …}` and mint nothing. There is no listing route.
- Minted URLs are bearer credentials returned only in the HTTP response — never
  written to the inbox, logs, bead notes, or `journal.jsonl` (the journal
  records `TAP MINTED:` with bead, device, and link count only, and refusals as
  `TAP REFUSED:`).

### Cursor-based notification feed (`GET /events`)

Allowed tailnet clients can poll a durable, bounded notification event feed
(`<state>/notify/events.json`, retaining the most recent 500 events) over the
same Tailscale-bound tap server without running a client webhook listener or
managing shared bearer tokens:

- **Auth & method:** Enforces the exact same fail-closed tailnet IP +
  `tailscale whois` + `[tap] devices` guard before reading any state (returning
  `403` JSON `{"error": …}` and journalling `TAP REFUSED:` on refusal, including
  requests from the host machine itself). `GET` only (`405` on other methods);
  strictly read-only (never creates or mutates files). Responses always carry
  `Content-Type: application/json; charset=utf-8` and `Cache-Control: no-store`.
- **Query parameters** (malformed values return `400` JSON `{"error": …}`):
  - `since` (optional non-negative integer, default `0`): return events with
    `seq > since` in ascending `seq` order.
  - `limit` (optional positive integer, default `100`, capped at `500`; values
    `<= 0` or non-integer are rejected with `400`).
  - `event` (optional comma-separated list of known event kinds: `pr_merged`,
    `pr_green`, `pr_ready`, `needs_me`, `blocked`, `worker_blocked`,
    `inbox_runner`; unknown kinds — including synthetic `test` — are rejected
    with `400`).
- **Response schema:**

```json
{
  "version": 1,
  "latest_seq": 42,
  "next_since": 42,
  "has_more": false,
  "truncated": false,
  "events": [
    {
      "seq": 42,
      "version": 1,
      "event": "needs_me",
      "at": "2026-11-25T08:00:00Z",
      "title": "Quartermaster: needs_me",
      "body": "task-12: Approve staging rollout?",
      "inbox_url": "http://100.64.1.2:8642/inbox",
      "items": [
        {
          "bead": "task-12",
          "reason": "needs_me",
          "summary": "Approve staging rollout?",
          "class": 6
        }
      ]
    }
  ]
}
```

- **Cursor & truncation semantics:**
  - `next_since` is the last returned event's `seq` when `events` is non-empty,
    and `latest_seq` otherwise (so a fresh client starting at `?since=0` or a
    client whose cursor fell ahead of a reset state directory syncs cleanly).
  - `has_more` is `true` when additional matching events exist beyond `limit`.
  - `truncated` is `true` when `since > 0` and either `since` predates the
    oldest retained event (`oldest_seq > 1 && since < oldest_seq - 1`) or
    `since > latest_seq`, signaling the client should perform a snapshot sync
    (such as `quartermaster context --compact` or `quartermaster inbox --questions`).
- **Privacy guarantees:** Events inherit the shared `notify.go`/`desktop.go`
  sanitization — secrets, tokens, generic URLs, and signed `/tap/<token>` bearer
  links are never stored or returned. Only the validated non-secret `inbox_url`
  (`/inbox`) is included when `[tap]` is enabled. `items[].class` (`0`–`9`,
  omitted when unset) is extracted for `needs_me` items from live `CLASS:` notes
  or structured question cards so consumers can separate urgent (`class >= 7`)
  from batched notifications without an extra round trip.
- **Local CLI inspection:** `quartermaster notify events [--since <seq>] [--limit <N>] [--event <kinds>] [--json]`
  reads the same local feed file strictly read-only. See
  [docs/notifications.md](notifications.md).

The inbox shows four live queues:

1. **Approval prompts:** blocked workers with a parsed prompt (or pi control
   ask), displaying bead id/title, autonomy level, prompt hash, the **actual
   command/prompt lines**, and fresh server-minted one-time `/tap/<token>`
   links per option (opening the standard confirm+POST page).
2. **`needs_me` questions and structured cards:** split into **Active** (shown
   first: live unparked agent, recent activity within `needs_me_parked_days`
   default 7d, or unsnoozed `CRITICAL:`) vs **Parked / Dormant** (collapsed
   disclosure section with summary count for agentless threads past the
   threshold or explicitly snoozed via `quartermaster snooze <bead> <duration>`;
   emitted under `parked_questions` in `/inbox.json`). Trailing housekeeping
   notes (`AUTO:` handoffs, `likely_stale rejected`, etc.) never hide the real
   question or command:
   - **Structured cards** from `<state dir>/cards/<bead>/question.json` (plus
     optional recent timeline from `<state dir>/cards/<bead>/milestones.jsonl`),
     rendering class badges (`C0`–`C9`, highlighting `C7`–`C9` and `CRITICAL`),
     recommendation callouts, option descriptions, and review cards (IaC
     create/update/replace/delete counts and preview excerpt, PR link, cost
     estimate, plan digest, and workflow run links). Missing, malformed, or
     stale card files fall back cleanly to bead notes.
   - **`QUESTION:` / `OPTIONS:` / `CLASS:` / `CRITICAL:` notes**, minting
     fresh `/tap/<token>` confirm links per option plus a free-text reply link,
     and highlighting detected plan digests or gate plan counts.
   - **Free-text `needs_me` notes** (without an `OPTIONS:` block), minting a
     fresh `/tap/<token>` free-text confirm link (up to 2,000 characters,
     recorded verbatim), and **read-only `RUN:` commands** (with a copyable
     command block, never tappable).
3. **Pending checked execution plans (`plans/<digest>/`):** checked plans
   waiting for TJ approval (`needs_me`, `expired`, or `checked` with
   `pass == false`, excluding `done`/`failed` runs and closed beads), showing
   steps, classes, and IaC preview counts when readable. Clicking **Review &
   Approve Plan** opens `GET /inbox/plan/<digest>` (a read-only review page
   carrying a freshly minted, HMAC-signed, one-time token bound to the exact
   digest, bead, and `tailscale whois` device); submitting `POST
   /inbox/plan/<digest>` verifies signature, expiry, digest, device, and
   atomic one-time nonce consumption before approving the plan via the same
   `--by TJ` path and journalling the approval with device identity.
4. **Ready-to-merge PRs:** read-only list of active threads currently in
   `ready to merge` state with PR link, short head SHA, checks summary,
   review state, autonomy level, and merge class (no unconfirmed merge
   buttons).

All existing `/tap/<token>` links and `quartermaster tap links <bead>`
behavior stay unchanged.

## What makes a tap TJ's

A tap changes something only when all of these hold, checked in this order —
a failure at any step spends nothing and is journalled (`TAP REFUSED: …` in
`journal.jsonl`):

1. **Signature.** The link is `kind | bead | prompt-or-question hash |
   option | expiry | nonce`, HMAC-signed with a key quartermaster generated
   into the state dir (`tap/secret`, mode 0600). Deleting that file revokes
   every link. Links live 15 minutes.
2. **Tailnet source.** The request arrived from a Tailscale address
   (100.64.0.0/10 or the tailnet IPv6 range). The listener binds the tailnet
   address, so LAN requests cannot even connect; this check also keeps a
   future non-tailnet bind from accepting them.
3. **Allowed device.** `tailscale whois` names the source node, and it must
   be in `[tap] devices`. This is the identity proof, and it is what a leaked
   link does not survive: links sit in the inbox event on this machine, where
   a worker could in principle read one — but a tap from this machine is the
   wrong device and is refused before anything is spent.
4. **One use.** The nonce is spent atomically before the answer is attempted;
   a replay is refused. Refusals in steps 1–3 happen before the nonce is
   spent, so a refused attempt by anyone else does not burn TJ's link.
5. **Same prompt.** The answer path re-reads the worker's screen and refuses
   when the prompt's hash differs from the link's, exactly as `--expect`
   does. A tap for a prompt that changed, cleared, or was answered another
   way changes nothing. A question tap re-reads the bead instead: it must
   still be `needs_me`, still end in the same question hash, and its worker
   must not already be working again.

A tap answers as TJ (`--by TJ` semantics): it is not gated by the bead's
autonomy level or the class of the command it releases, because it *is* TJ
deciding, with the command in front of him. Coordinator answers keep their
class gate; nothing about `quartermaster answer` changes.

## Transport decision

Three ways to put the option buttons on a phone were compared against what
this machine already runs (observed 2026-11-22: Tailscale up, an iPhone on
the tailnet, no `ntfy`, no Slack digest configuration):

| | Phone experience | Identity proof | Public listener | New always-online pieces |
|---|---|---|---|---|
| **Signed one-time links on a tailnet endpoint (chosen)** | Any browser; links ride the chat already used | HMAC link + `tailscale whois` device | No: tailnet address only | None: the ticker hosts it |
| Slack interactive components | Native buttons in Slack | Slack user id + request signing | Slack must reach the handler: a public request URL, or a Socket Mode websocket held open | A Slack app with interactivity, tokens, and that socket or a tunnel; Slack is not configured here at all today |
| ntfy action buttons | Native buttons in the ntfy app | Whatever the action URL itself proves — still this design's token | No, if the action URL is the tailnet endpoint | An ntfy server (or ntfy.sh) and a new phone app; it is a carriage for these same links, not an alternative to them |

Slack interactive loses on reachability: its button presses are webhooks from
Slack's cloud, so something public (or a new outbound socket daemon with app
tokens) must exist to receive them, for a chat this setup does not currently
use. ntfy loses as a *starting point* rather than as an idea: its action
buttons would still call an endpoint that has to authenticate the tap, so
the endpoint and token are needed either way — ntfy becomes a possible
future carriage for the same links, alongside the coordinator relay, without
changing anything here.

## Coverage, said plainly

Taps exist only for prompts quartermaster can parse: Claude Code and Codex
approval menus (and folder-trust), and a pi worker's ask over its control
socket. **Muse workers are not covered yet**: the parser does not recognize
Muse TUI approval prompts (it reports "no prompt showing"), so a blocked muse
worker produces the same generic "waiting at an approval or question prompt"
inbox item as before, with no links. Covering muse needs a captured muse
prompt screen as a parser fixture; once `parsePrompt` returns the prompt,
taps cover it for free, since they key off the parsed prompt and its hash.

`needs_me` questions are covered both with and without an `OPTIONS:` block
(see below); `RUN:` command beads stay read-only and never get taps. Tool
approval prompts keep enumerated answers only.

Prompts on remote worker hosts work the same as local ones: the answer path
reads and keys the worker wherever it runs, and only the endpoint's machine
needs the tailnet address.

## needs_me questions

A worker that needs a decision with enumerable options writes its question
as one note (workers are briefed on this form):

```
QUESTION: Ship to all regions or one first? Recommendation: 2 — one region
first, because the new alerts are unproven.
OPTIONS:
1. All regions at once
2. One region first, then the rest
CLASS: 7 — production rollout
```

The `CLASS:` line is the one a `needs_me` question already carries, and may
sit among the question lines instead of after the options; a `CRITICAL:`
line immediately before `QUESTION:` stays part of the question. The ticker
parses only the **last** such block, and only while nothing but an optional
`CLASS:` line follows it — any later note is the thread moving on, and the
block is history. `RUN:` beads never get question taps. When the block is
live, the bead's needs-you inbox item carries the question and one signed
link per option, like a prompt's; `quartermaster tap links <bead>` mints
the same links when no approval prompt is showing. The coordinator's
context shows the question and its options.

A question tap goes through the same endpoint and the same five checks; the
link's hash is taken over the question's lines, its options (empty when there
is no `OPTIONS:` block) and its class, and binds it to that question instance.
The confirm page shows the question re-read from the bead alongside a free-text
field (up to 2,000 characters, counted in runes). Three submissions are valid
for a question with options — an option alone, free text alone, or an option
with free text — and free-text-only questions show a single text area and
submit button. Over-limit or empty free-text submissions are refused before the
nonce is spent so the user can edit and resubmit on the same link. Free text is
recorded verbatim (stripping at most a single trailing newline added by form
submission). Pressing the button does what the coordinator does when TJ answers
in the coordinator session, in this order:

1. Notes the answer on the bead —
   `NEEDS_ME ANSWERED: 2 One region first… to Ship to all regions… [hash] by TJ via tap (device iphone181)`
   (or with `: <text>` appended for option+text, or `NEEDS_ME ANSWERED: free text to … [hash] by TJ via tap (device iphone181): <text>` for free text alone) —
   and journals it.
2. Returns the bead to `in_progress`.
3. Prompts the worker with the answer — option and verbatim text together —
   (over its control socket when it has one), so the tap, not a later
   coordinator message, unblocks it.

The stale guards are the needs_me analogues of a changed prompt: the tap is
refused when the bead has left `needs_me`, when the question's hash has
changed (the worker edited or re-asked it), or when the worker is already
working again — it may have its answer another way, and that is the state
in which the ticker resumes it on its own. A worker at rest is prompted; a
worker blocked at an approval prompt, or one that is no longer running, is
never typed into — the answer stands on the bead, a follow-up note says it
was not prompted, and the result page says so too.

As with prompts, a tap is TJ deciding, so it is not gated by the bead's
autonomy class; the `CLASS:` line is shown on the confirm page (as part of
the question) so the decision is made with the class of what each option
authorizes in view. That includes `CRITICAL:` questions: they are reserved
for TJ, and a tap *is* TJ. The coordinator's own rules for answering
`needs_me` questions are unchanged.

## Rollback and failure modes

- **Off switch:** set `[tap] enabled = false` (or delete the section) and
  restart the ticker. No listener, no `/inbox`, no links minted, and
  `quartermaster tap links` / `quartermaster tap url` refuse. Outstanding
  links die with the listener.
- **Revoke everything now:** delete `~/.local/state/quartermaster/tap/secret`;
  every outstanding link's signature stops verifying. A new key is made on
  next use.
- **Ticker keeps ticking:** a tap bind failure (Tailscale down, port taken)
  is logged once at ticker start and the ticker runs without taps.
- **Phone off the tailnet** (VPN off, another tailnet): links simply do not
  connect. Nothing falls back to a public address.
- **Audit:** answered taps are journalled like every prompt answer; refused
  taps are journalled as `TAP REFUSED` with the bead (when the signature
  verified) and the reason. `tap/used.json` holds spent nonces until their
  links would have expired.

## Threat model, in one paragraph

The endpoint is reachable only from devices on TJ's tailnet. Another tailnet
device holding a link (say a laptop) is refused by the device allowlist. A
worker on this machine is refused the same way, which is the case that
matters, since workers can read the inbox. What this does not defend against
is the standing advisory limit: an agent running as TJ on this machine can
read `tap/secret`, mint its own links, or simply run `quartermaster answer
--by TJ` — taps neither widen nor narrow that, and `--by TJ` remains a label
until the executor owns credentials. The prompt text a tap page renders is
screen text the agent controls; it is HTML-escaped, and the page takes no
action without a press.
