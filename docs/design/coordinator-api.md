# Coordinator API: the desktop coordinator ↔ Muse app agent contract

Status: **proposed** (2026-11-23, bead backend-hvez) for TJ's approval.
Design ping-pong rounds 1–3 (2026-11-22, relayed by TJ) agreed the surface
below; TJ decided the four open questions the same day (see
[Decisions](#decisions-tjs-2026-11-22)). Nothing here is implemented yet:
each piece is a named follow-up bead (see
[Follow-ups](#follow-ups)). This bead changes no production or live-machine
state.

## Summary

Two agents talk to TJ about the same beads. The **desktop coordinator**
executes: it dispatches workers, follows PRs, answers what its autonomy
level allows, and closes beads. The **Muse app agent** is TJ-facing: it
surfaces decisions, relays TJ's answers, and verifies outcomes by
independent read. Without a contract, that split degrades into two agents
paraphrasing each other's prose, with no way to tell "the coordinator says
it merged" from "it merged".

This spec is that contract, in four parts:

1. **A message contract** between the two agents: eight gate verbs with
   explicit targets, one fenced status block ending every action reply,
   and formatting conventions that keep one question per message.
2. **A read surface** sized for the app: `context --compact / --section /
   --json`, `inbox --questions --json`, `show`, `memory get`, and a
   `GET /tap-links/<bead>` minting endpoint on the existing tap listener,
   so the app never needs a raw context dump or a stored tap link.
3. **Free-text taps**, so a `needs_me` answer from the phone can be prose,
   not only an enumerated option.
4. **An async direction**: tag-echo correlation (`[req:id]`) as the
   interim, a durable job mechanism as the target, with the five candidate
   stores scored and a recommendation.

## Roles and the single driver

| | Desktop coordinator | Muse app agent |
|---|---|---|
| Owns | Execution: dispatch, follow-up, merge/close within the autonomy dial | TJ's attention: which decisions surface, in what words, and when |
| Talks to TJ | When TJ is in the coordinator session | The phone surface; renders decisions, never raw dumps |
| Trusts | Bead state, PR state, its own command output | Only independent reads: fresh bead status, PR state, tap receipts — never coordinator prose |
| Never | Does the bead's work itself | Executes, dispatches, or answers a question itself |

**One driver.** The desktop ticker is the only loop. It already follows
every thread, writes the durable inbox (`stateDir()/inbox`, one JSON file
per event), nudges the coordinator, and serves taps. The app adds **no
second hourly loop, poller, or scheduler**; everything it shows TJ derives
from a read (`context`, `inbox --questions`, `show`) or from a coordinator
reply it then verifies. Two drivers would double-notify and could act on
the same prompt twice; the at-most-once machinery below assumes one.

**Verification is the app's job, and it is mechanical.** When a
coordinator reply claims an outcome, the app confirms it before telling
TJ: `pr_state` against `gh`, `bead_status` against a fresh `show`, a tap
answer against the `NEEDS_ME ANSWERED:` / `PROMPT ANSWERED:` note the tap
path already writes. A claim that does not verify is reported as
unverified, with the read that disagreed — never smoothed over.

## The gate vocabulary

TJ's decisions reach the coordinator as one of eight verbs. The app
normalizes whatever TJ did — a tap, a typed word or number, its own
same-second `answered:` notification — into this vocabulary before
anything acts on it.

| Verb | Target | Meaning | Coordinator acts by |
|---|---|---|---|
| `MERGE` | bead or PR URL | Merge the target's PR now | the merge path (auto-merge or executor), class 4 |
| `DISPATCH` | bead | Start a worker on it | `quartermaster dispatch <bead>` |
| `CLOSE` | bead | Close it as done | `quartermaster close <bead> --reason …` |
| `HOLD` | bead | Park it; take no further steps | note + no dispatch/resume until `RESUME` |
| `RESUME` | bead | Unpark / restart its worker | `quartermaster resume <bead>` |
| `STATUS` | bead (or none, see below) | Report current state, change nothing | a status-block reply from fresh reads |
| `STOP` | bead | Stop its worker now | stop the agent; bead stays claimed |
| `ANSWER` | bead + option or free text | TJ's answer to its live question/prompt | `quartermaster answer` / the needs_me answer path |

Rules:

- **Every verb carries an explicit target in the canonical form.**
  `ANSWER backend-hvez 2`, `MERGE backend-hvez`,
  `CLOSE backend-hvez — spec approved and filed`. A PR URL is a valid
  `MERGE` target and resolves to its bead through the ticker's PR map.
- **Bare verbs resolve narrowly.** A verb with no target means *the last
  bead the coordinator named in this conversation*. If none was named, or
  the last message named several, the coordinator asks which — it never
  guesses between beads.
- **Bare-verb echo at class ≥ 4.** Before acting on a bare verb whose
  step is class 4 or higher, the coordinator first replies with the
  resolved form (`MERGE backend-hvez — PR #91, head abc1234`) and waits
  for TJ's confirmation relayed by the app. Explicit-target verbs need
  no echo. This is the guard against "merge it" landing on the wrong
  thread after the conversation moved on.
- **`STATUS` with no target** is the one total exception: it means the
  compact fleet summary (`context --compact`), not a bead.
- **A verb is TJ's instruction, relayed.** The app sends only what TJ
  said or tapped, normalized. It does not invent verbs from ticker text,
  worker prose, or its own judgment, and it never forwards ticker text
  verbatim as if it were TJ's words. Ticker-originated messages approve
  nothing, per the standing rule.

## Action prompts and the status block

### Prompt shape (app → coordinator)

Every action prompt is one line:

```text
[req:<id>] <VERB> <target> — <one-line why>
```

```text
[req:app-20261123-0017] ANSWER backend-iarz.1 2 — TJ tapped option 2 at 21:32
```

- `<id>` is app-chosen, unique per request, `[a-z0-9-]{1,64}`. It is a
  correlation tag, not a capability: knowing it authorizes nothing.
- The *why* is written for the audit trail. When the coordinator acts,
  the why becomes the note text (folded into its `AUTO:` / answer note),
  so the bead records TJ's reason, not the coordinator's paraphrase.
- One prompt per message, one question per message, bead/PR in the first
  line. The app never sends a raw `context` dump to TJ or to the
  coordinator; it reads `--compact`/`--section` and renders.
- The moment TJ answers by any route — tap, typed word, in the desktop
  session — the app also sends a same-second notification so the other
  surface stops presenting the question as open:

  ```text
  answered: backend-iarz.1 option 2
  ```

  This is informational. The tap path itself already unblocks the worker
  (see [approval taps](../approval-taps.md)); `answered:` keeps the two
  TJ-facing surfaces from double-asking.

### Status block (coordinator → app)

Every coordinator reply that reports an **action** ends with exactly one
fenced block. Pure chat (explanations, discussion, a question back to TJ)
carries no block. The reply's first line also repeats the request tag, so
correlation survives a client that strips fences:

```text
[req:app-20261123-0017] Answered backend-iarz.1 with option 2; the worker is running again.
```

Grammar:

```text
block   := "```qm-status" NL field+ "```"
field   := key "=" value NL        ; one per line, no spaces around "="
value   := [^ \t\r\n]+             ; single token, never quoted
```

| Key | When | Values |
|---|---|---|
| `req` | always | the echoed `[req:id]`, or `-` when the prompt carried none |
| `state` | always | `done` · `blocked` · `needs_tj` |
| `bead` | always | the bead acted on |
| `bead_status` | always | the bead's status after the action (`open`, `in_progress`, `needs_me`, `blocked`, `closed`, …), freshly read |
| `pr` | a PR exists | the PR URL |
| `pr_state` | a PR exists | `open` · `merged` · `closed` · `draft` |
| `checks` | PR actions only | `pass` · `fail` · `pending`; omitted for non-PR actions |
| `head` | a PR exists | the PR head SHA the claim is about |
| `class` | an autonomy check ran | the class checked, `0`–`9` |
| `job` | true-async jobs only | the durable job id (see [Async](#async-tag-echo-now-true-async-as-the-target)) |

Unknown keys are ignored by readers; new keys may be added without a
version bump. `state=blocked` means the action could not complete for a
reason that is not TJ's to decide (tool failure, missing precondition);
`state=needs_tj` means it stopped at a decision only TJ can make, which
the reply's prose states as one question.

Worked example — this is the whole reply:

````text
[req:app-20261123-0017] Answered with option 2; backend-iarz.1 is in_progress and its worker has the answer.

```qm-status
req=app-20261123-0017
state=done
bead=backend-iarz.1
bead_status=in_progress
class=2
```
````

The block is deliberately `key=value`, not JSON: it must survive chat
clients, be writable by a model without escaping, and parse in one line.
This one-liner extracts it from a reply on stdin (run against the example
above for this spec; it prints the five fields):

```sh
python3 -c 'import re,sys; m=re.search(r"```qm-status\n(.*?)```", sys.stdin.read(), re.S); print(dict(l.split("=",1) for l in m.group(1).splitlines() if "=" in l))'
```

The status block is a **claim**, and the app treats it as one: it exists
so the app knows *what to verify*, compactly. `bead_status` is checked
against `quartermaster show`, `pr_state`/`checks`/`head` against `gh`,
before TJ is told the outcome.

## Cadence

- Class ≥ 7 blockers surface to TJ **immediately**.
- Everything else batches **hourly**, aligned with the existing digest
  timer — the app renders the batch, it does not run its own.
- **No quiet window** (TJ, 2026-11-22): prompts are not held overnight.
  Notification timing is the phone's Do Not Disturb, not quartermaster's
  business. The proposed 23:00–07:00 ET hold is rejected, not deferred.

## The read surface (CLI)

Conventions, verified against the shipped CLI on 2026-11-23: one shared
Go `flag.FlagSet` in `main.go` serves every subcommand, so new flags are
declared once there and `quartermaster <cmd> --help` prints the shared
set. Go's flag package accepts `-json` and `--json` identically; usage
strings write `--`. There is **no third surface**: the app uses these
commands and the tap endpoint, nothing else.

### `quartermaster context --compact / --section / --json`

Today `context` is the coordinator's per-turn digest and it is not
app-sized. Measured live for this spec (2026-11-23, one staged run):

| Section (`renderContext`, context.go) | Bytes |
|---|---|
| Preamble (counts, warnings) | 417 |
| `## Autonomy` | 286 |
| `## Inbox` | 2,002 |
| `## Threads` | 33,155 |
| `## Linear` | 3,274 |
| `## Selected next` | 859 |
| `## Ready` | 1,519 |
| **Total** | **41,512** |

Threads dominate because every live thread carries title, question text,
and PR URL. `--compact` is therefore a different rendering, not a trim:

```sh
quartermaster context --compact
quartermaster context --section inbox,threads
quartermaster context --section inbox --json
quartermaster digest --json
```

- `--section <list>`: comma-separated, composable, from
  `summary,autonomy,inbox,threads,linear,next,ready`. Unknown names are
  an error listing the valid set. Default (no flag) is today's full
  output, unchanged for the desktop coordinator.
- `--compact`: hard target **< 4 KB** regardless of fleet size. One
  counts line; inbox items as `bead [kind]` only; threads as one line
  each (`bead · group · PR#n`, no titles, no question bodies);
  `needs_me` beads are never dropped. Truncation order when over
  budget: ready list first, then non-live thread groups (folded to
  counts, as `GroupNoAgent` already is), then inbox summaries. The
  renderer asserts the budget in tests against a large fixture.
- `--json`: orthogonal to both. The same section objects the text form
  renders, keyed by section name; `--compact --json` truncates the
  objects the same way. `digest --json` emits the digest's sections the
  same way and does not alter `digest.md` or delivery.

Shape (field names follow the existing `Thread`/`Event` JSON tags):

```json
{
  "summary": {"needs_you": 59, "merged": 14, "ready_to_merge": 2, "working": 2},
  "inbox": [{"bead": "backend-9ebe", "kind": "agent_gone", "at": "2026-11-20T15:07:54-05:00"}],
  "threads": [{"bead": "backend-iarz.1", "group": "needs_you", "status": "needs_me", "pr": null}]
}
```

### `quartermaster inbox --questions --json`

The one call the app makes to learn *what TJ must decide right now*. It
joins the two live question sources quartermaster already parses — a
worker's approval prompt (`prompt.go`) and a `needs_me` OPTIONS question
(`tapquestion.go`, via `liveQuestion`) — into one array:

```sh
quartermaster inbox --questions --json
```

```json
[
  {
    "bead": "backend-iarz.1", "kind": "needs_me",
    "question": "Which customer-hosted accounts may endpoint scanning cover?",
    "recommendation": 1,
    "options": [{"n": 1, "label": "Member accounts only now"}, {"n": 2, "label": "Include opted-in customer-hosted"}],
    "class": 7, "bead_status": "needs_me", "hash": null, "expires": null
  },
  {
    "bead": "backend-5m0z", "kind": "prompt",
    "question": "Bash command: git push origin tj/example",
    "recommendation": null,
    "options": [{"n": 1, "label": "Yes"}, {"n": 2, "label": "No"}],
    "class": null, "bead_status": "in_progress", "hash": "bff9292a", "expires": null
  }
]
```

- **`hash` has two worlds and they stay separate.** For `kind=prompt` it
  is the tool-prompt hash — the `--expect` value from
  `quartermaster prompt`. For `kind=needs_me` it is **null**: that
  question's hash exists only bound inside signed tap tokens, and
  exposing it here would invite the app to treat it as an `--expect`
  equivalent, which it is not. Staleness for a needs_me row is detected
  by re-reading (question/options changed → row changed) and enforced by
  the tap/answer path, exactly as today.
- `expires` is null today and reserved for rows derived from minted tap
  links; a question row itself never expires.
- `RUN:` beads never appear (they are commands for TJ to run, not
  questions), already-answered questions never appear, and the command
  mints nothing and marks nothing done. `inbox done` keeps its current
  meaning; `--questions` is a read.

### `quartermaster show <bead>` and `quartermaster memory get <key>`

Workspace-resolving read wrappers, so the app (and any session) stops
depending on cwd or `BEADS_DIR` being set just right. They reuse the
`Beads` access the ticker already uses (`Beads{}.Show` is the same call
`planCmd` receives today):

```sh
quartermaster show backend-hvez
quartermaster show backend-hvez --json
quartermaster memory get admitter-image-tag-stored-config
```

`show --json` carries the `bd show --json` fields the app verifies
against (`id`, `title`, `status`, notes tail). `memory get` returns one
`bd remember` memory by key, with a clean not-found error. **Writes stay
in `bd`**: there is no `quartermaster note` in this spec. Notes, claims,
and closes keep their existing commands and audit shape; these two
wrappers exist only because reads are what the app does constantly.

## Tap links on demand: `GET /tap-links/<bead>`

Today links reach the phone two ways: the ticker mints them into an inbox
event, and the coordinator mints them with `quartermaster tap links
<bead>` and relays them in its reply. Both leave the app dependent on
timing — an inbox event's links may be 14 minutes old when TJ opens the
app. TJ approved (2026-11-22) growing the listener instead:

```http
GET /tap-links/backend-iarz.1 HTTP/1.1
Host: 100.78.70.17:8642
```

```json
{
  "bead": "backend-iarz.1", "kind": "needs_me",
  "question": "Which customer-hosted accounts may endpoint scanning cover?",
  "expires": "2026-11-23T21:47:00-05:00",
  "links": [
    {"n": 1, "label": "Member accounts only now", "url": "http://100.78.70.17:8642/tap/<token>"},
    {"n": 2, "label": "Include opted-in customer-hosted", "url": "http://100.78.70.17:8642/tap/<token>"}
  ],
  "free_text": {"url": "http://100.78.70.17:8642/tap/<token>", "max_chars": 2000}
}
```

Guards — the same five checks as a tap, applied at mint time (see
[approval taps](../approval-taps.md)):

1. **Off unless enabled.** The route exists only when `[tap] enabled` is
   set; it registers beside `/tap/` in `tapServer.handler()` (tap.go).
2. **Tailnet-only.** The listener already binds the machine's Tailscale
   IPv4 only (never `0.0.0.0`); the handler re-checks the source is a
   tailnet address, as `handleTap` does.
3. **Whois/device-gated.** `tailscale whois` must name a device in
   `[tap] devices`. Minting is write-capable — a minted link *is* an
   answer waiting for one press — so it gets the same identity proof as
   spending one. A worker on this machine is the wrong device and is
   refused, which is the case that matters.
4. **Fresh mint per call, 15-minute TTL.** Every call mints new nonces
   through the same code as `tap links`. Earlier links stay valid until
   expiry or use; the one-use nonce spend still makes the *answer*
   at-most-once even when several live link sets exist.
5. **Live target only.** The bead must be at a parseable prompt or a
   live `needs_me` question at mint time, re-read like the confirm page
   re-reads it. Otherwise `404` with a JSON `{"error": …}` saying which
   (gone, changed, `RUN:` bead, no prompt showing).

**Links are bearer credentials.** The response is the only place they
exist. They are never written to the inbox, the journal, a bead note, or
any log — the journal records the mint event (bead, device, count, *no
URLs*), and refusals as `TAP REFUSED:` like today's. The app must not
store, republish, cache, or reuse them beyond presenting the buttons;
when TJ reopens the question, it calls again. Until this endpoint
lands, the interim stands: the coordinator includes freshly minted links
in its reply payloads, as it does today.

Errors: `403` (not tailnet / device not allowed / whois failed), `404`
(no live prompt or question), `410` is not used here — expiry belongs to
the links themselves. One bead per call; no listing route (listing what
needs TJ is `inbox --questions`, which mints nothing).

## Free-text taps

TJ's decision (2026-11-22): the tap flow must let him write free text —
more detail, a different option, or prose instead of the options —
similar to Muse's answer affordance. This applies to `needs_me`
questions **with and without** an OPTIONS block. (Tool approval prompts
keep enumerated answers only: the worker's own menu is the contract
there, and free text typed into a TUI menu is not an answer.)

- **Capture.** The needs_me confirm page gains a text area under the
  question, re-read live from the bead like the rest of the page, plus
  the existing option buttons when the question has options. Three
  submissions are valid: an option alone (today's behavior), text alone,
  or an option *with* text. The page's primary button still requires a
  deliberate press; a GET still answers nothing.
- **Without an OPTIONS block**, the page is text-only: one text area, one
  button. The link kind is still the question kind; its hash is taken
  over the question lines as today, with an empty option set. `RUN:`
  beads remain excluded.
- **Length.** At most **2,000 characters** (counted in runes), enforced
  on the POST before the nonce is spent, and shown on the page. Over
  the limit is a refusal that spends nothing, like the other pre-spend
  refusals — TJ edits and presses again on the same link.
- **Landing, verbatim.** The text is recorded **exactly as typed** — no
  trimming beyond a single trailing newline the form itself adds, no
  re-wrapping, no coordinator paraphrase. The answer note keeps the
  existing shape and gains the text:

  ```text
  NEEDS_ME ANSWERED: free text to Which customer-hosted accounts… [95c6105e] by TJ via tap (device iphone181): Include the two accounts named in the 2026-10 agreement; leave the rest off until each signs.
  ```

  Option+text records both: `NEEDS_ME ANSWERED: 2 … [hash] by TJ via tap
  (device …): <text>`. Then, in the existing order: the bead returns to
  `in_progress`, and the worker is prompted with the answer — option and
  text together — over its control socket when it has one, never typed
  into a worker that is blocked or gone (the note stands and says so).
- **At-most-once is unchanged.** One nonce, spent atomically before the
  answer is attempted; the stale guards (bead left `needs_me`, question
  hash changed, worker already working) refuse exactly as for options.
  A free-text answer and an option tap racing for the same question
  resolve to whichever spends first; the second is refused.
- **The app side.** Free text through the app (not the tap page) travels
  as `ANSWER <bead> — <TJ's text>` and the coordinator notes it verbatim
  the same way; the audit trail must not depend on which surface TJ used.

## The decision flow, end to end

```text
worker stops (prompt / needs_me)
  │  ticker writes the inbox event (durable, existing behavior)
  ▼
coordinator is nudged ── or ── app reads inbox --questions --json
  │  one question per message; bead/PR in the first line
  ▼
app renders for TJ: question, recommendation, class, fresh links
  │  (GET /tap-links/<bead> at render time — never stored links)
  ▼
TJ taps an option, taps with free text, or answers in chat
  ├─ tap ──► tap endpoint answers + unblocks the worker directly
  │          app sends "answered: <bead> option <n>" the same second
  └─ chat ─► app sends [req:id] ANSWER <bead> <n> — <why>
             coordinator answers, the why becomes the audit note
  ▼
coordinator reply: [req:id] first line, one qm-status block last
  ▼
app VERIFIES by independent read (show / gh / the ANSWERED note)
  ▼
app tells TJ the verified outcome — or that it did not verify, and what disagreed
```

Note the asymmetry the flow preserves: a tap does not need the
coordinator at all (that is its point), while everything else routes
through the coordinator so execution stays in one place. The app's
`answered:` notice and its verification read are what keep the two
surfaces consistent after a tap bypasses the coordinator.

## Async: tag-echo now, true async as the target

**Interim (v1, available the day the conventions land): tag-echo only.**
The app tags each action prompt `[req:id]`; the coordinator echoes it in
the reply's first line and in `req=` in the status block. There is no
job table and no queue. Correlation works, but the request lives only in
the two conversations: if the coordinator session dies, restarts, or
compacts before replying, the app cannot ask "what happened to req X?" —
it can only re-derive state from beads and PRs. That is acceptable for
v1 because beads already are the durable record of *outcomes*; what
tag-echo cannot give is a durable record of *requests*.

**Target (TJ, 2026-11-22: true async over tag-echo): a durable job
mechanism.** Submitting a request returns a durable id immediately;
status and result are retrievable later, by anyone, after the ticker or
the coordinator session restarts:

```sh
quartermaster job submit --key app-20261123-0017 -- ANSWER backend-iarz.1 2
# → job=7f3a9c… state=queued
quartermaster job status 7f3a9c…     # queued | running | parked | done | blocked | needs_tj
quartermaster job result 7f3a9c…     # the qm-status fields + the audit note text
quartermaster job list [--bead B]    # recent jobs, newest first
quartermaster job cancel 7f3a9c…     # only before the effect runs
```

Semantics the mechanism must hold, whichever store backs it:

- **Idempotency.** `--key` is the submitter's key (the `[req:id]`, made
  durable). Re-submitting the same key returns the original job and
  never duplicates the effect.
- **At-most-once answers.** A job that answers a prompt/question claims
  it the way a tap spends its nonce: two jobs for the same live question
  resolve to one effect and one `blocked` ("already answered").
- **Human-in-the-loop waits are a state, not a timeout.** A job may sit
  `parked` on a `needs_me` answer for days. Nothing expires it silently;
  `job list` shows its age.
- **The status vocabulary is shared.** Job states end in the status
  block's `state=` values; a coordinator reply to a job-backed request
  adds `job=<id>`. Tag-echo clients keep working unchanged.
- **Beads stay the record of outcomes.** A finished job's result points
  at the bead note it caused; the job store is not a second task system
  and holds no state a bead should hold.

### The five candidate stores, scored

Local facts, verified 2026-11-23: no `nats-server`, `temporal`,
`valkey-server`, or `redis-server` binary, image, or process exists on
this machine for quartermaster; `go.mod` has no queue, Postgres, or
Redis client (the whole dependency list is `BurntSushi/toml`,
`rivo/uniseg`, `x/term`). A Postgres server *is* running, but it is a
foreign instance (uid 100069, up since Nov 20, started with
`fsync=off`) — not quartermaster's to use, and with fsync off it could
not offer the durability it is usually chosen for. Quartermaster already
runs two durable patterns of its own: the ticker's state + JSON inbox
files, and the executor's per-plan event journal with resume
(`executor.go`: digest ids, fsynced events, restart-recovery of
unfinished plans).

Scores: ● strong · ◐ workable · ○ weak, against this machine's reality.

| Criterion | JetStream (NATS) | Temporal | Valkey | Postgres (new instance) | State-dir job store in the ticker (baseline) |
|---|---|---|---|---|---|
| Footprint on one desktop | ◐ one small static binary, but a new always-on daemon + its data dir | ○ server + its own DB, by far the heaviest thing here | ◐ one small daemon | ◐ one daemon, well understood; needs its own instance + backups | ● none — files beside `inbox/`, owned by the ticker that already runs |
| Go integration cost | ◐ `nats.go` is a real dep; streams/KV model to learn and wrap | ○ Go SDK + workflow/activity split + determinism rules around LLM steps | ◐ small client; streams consumer groups are close to the metal | ◐ `pgx` + migrations; SQL is the easy part | ● extends the executor's existing event-journal code; zero new deps (JSONL now, embedded SQLite only if queries grow) |
| Durability across ticker/coordinator restarts | ● file-backed streams, replay | ● its core promise | ◐ AOF must be tuned on; easy to run semi-durable by accident | ● strongest, once it is *our* instance with fsync on | ● fsynced append-only journal — the executor already proves this pattern here |
| Human waits parked for days | ◐ a parked job is our KV row + consumer state to design | ● signals/timers are built for exactly this | ◐ our state machine on top of streams | ● a row with a state column; `LISTEN/NOTIFY` or ticker polling | ● a job file with `state=parked`; the ticker already polls everything else |
| At-most-once answer semantics | ◐ dedupe headers help; claim logic still ours | ● activity idempotency + history | ○ consumer-group acks are at-least-once; claiming is ours to build carefully | ● transactional claim (`UPDATE … WHERE state='queued'`) is textbook | ◐ single-writer ticker + atomic rename — the tap nonce store already works this way |
| Observability from the CLI | ◐ `nats` CLI is good, but it is a second tool's worldview | ◐ web UI/CLI, another surface to run | ○ `valkey-cli` raw keys; meaning lives in our schema | ● SQL against one table | ● `quartermaster job list/status` over files `jq` can also read |
| Credibly carries coordinator↔worker traffic later | ● designed for multi-machine subjects over the tailnet | ◐ could, at workflow-authoring cost per message kind | ◐ pub/sub + streams could, with our protocol on top | ◐ cross-machine means every worker host needs DB reachability + creds | ○ local files do not cross machines; cross-host stays SSH/`bd-proxy` |

### Recommendation

**Phase 1: the baseline — a job journal in the ticker, following the
executor's pattern.** One append-only JSONL per job under the state dir
(beside `inbox/`), job id = digest of (key, request), states as above,
the ticker as sole writer/executor of queued work, `quartermaster job
submit|status|result|list|cancel` as the only interface. It beats every
daemon on footprint and integration cost because quartermaster already
built and operates this shape twice (inbox, executor plans), and it
meets the actual requirements: durable ids, retrieval after any session
death, days-long parks, at-most-once claims under a single writer. If
querying outgrows a directory scan, the same journal loads into an
embedded SQLite index without changing the interface — that is an
implementation detail of the follow-up bead, not a new decision.

**Phase 2 trigger for JetStream:** the day job traffic must cross
machines as a first-class bus (worker hosts submitting/claiming jobs
directly, rather than through the coordinator machine's ticker), adopt
JetStream behind the same `quartermaster job` interface. It is the only
candidate whose multi-machine story is its home turf, and its Go client
fits this codebase. Adopting it now would pay a daemon, a dependency,
and a state-machine build for a single-machine problem.

**Not recommended:** Temporal — its workflow determinism and
server-plus-DB operation buy durable *execution of code*, but our jobs
are mostly "hold this request until TJ or a PR moves", and the org's
in-product Temporal clusters must not be coupled to a single-user
desktop tool. Valkey — a new daemon whose durability and claim semantics
are ours to assemble, scoring below both neighbors on either side.
Postgres — the runner-up on semantics alone, and the recommendation
flips to it *if* TJ blesses a dedicated quartermaster instance with
backups; the instance currently running here is someone else's and runs
`fsync=off`, so it settles nothing today. Standing up a database to hold
dozens of jobs a day, next to a state dir that already works, is the
cost without a present benefit.

### Does the same mechanism carry coordinator↔worker traffic?

Evaluate, per TJ's question — with a boundary, not a wholesale swap:

- **Adopt the envelope, not the transport.** Worker-directed requests
  already have durable, idempotent shapes: `quartermaster rpc tell
  <bead> <request-id>` is an idempotent submission with a separately
  retrievable receipt (`rpc receipt`), the executor runs approved plans
  durably per bead, and Herdr panes + the ticker carry the rest. What
  generalizes is the *contract* — durable id, idempotency key,
  `queued/running/parked/done/blocked/needs_tj` states, result
  retrieval — so a future `job status` can present an rpc tell, a plan,
  and an app request in one vocabulary.
- **Do not reroute the worker hot paths.** Prompts to live workers (pane
  typing, control sockets, pi RPC), prompt answering, and tap spending
  stay exactly where they are. They are latency- and guard-sensitive,
  and each already owns its at-most-once mechanism. Putting a job queue
  between the coordinator and a worker stopped at an approval prompt
  would add a hop to the path that most needs to be short and provable.
- **New cross-machine worker messaging is the JetStream trigger**, not a
  phase-1 concern. If remote worker hosts ever need to submit requests
  (not just receive prompts via SSH/`bd-proxy`), that is the evaluated
  moment to stand JetStream up — with this same job interface in front
  of it, so the app and coordinator contracts do not change.

## Decisions (TJ's, 2026-11-22)

All four questions this spec opened with are decided; the agreed surface
above already reflects them, and nothing else in the spec depends on
re-deciding any of them.

| # | Question | Decision | Where it lands |
|---|---|---|---|
| (i) | May the tap listener grow `GET /tap-links/<bead>`? | **Yes**, with the stated guards (tailnet-only bind, whois/device-gated, off unless `[tap]` enabled) | [Tap links on demand](#tap-links-on-demand-get-tap-linksbead) |
| (ii) | Free-text taps for `needs_me` questions without OPTIONS? | **Yes** — Muse-style free text, with and without options, recorded verbatim | [Free-text taps](#free-text-taps) |
| (iii) | True async (job table) vs tag-echo forever? | **True async** as the target; tag-echo is the interim only | [Async](#async-tag-echo-now-true-async-as-the-target) |
| (iv) | Quiet window 23:00–07:00 ET? | **No** — no holding at any hour; the phone's Do Not Disturb owns timing | [Cadence](#cadence) |

Recommendation lines, for the record: the spec recommended *yes* on (i)
and (ii) as written, *true async behind the job interface with the
state-dir store first* on (iii), and *no quiet window* on (iv). TJ's
answers match on all four, so the surface needs no contingency branches.

## Follow-ups

Implementation lands as these beads (filed from this spec, each with its
own acceptance criteria); this bead ships only the spec.

| Bead | Piece |
|---|---|
| backend-y4wl | `context --compact/--section/--json`, `digest --json` |
| backend-tocb | `inbox --questions --json` |
| backend-a9xj | `GET /tap-links/<bead>` minting endpoint |
| backend-9qpg | Free-text taps for `needs_me` questions |
| backend-n4me | `show <bead>` and `memory get <key>` read wrappers |
| backend-a0k5 | Coordinator guide: gate verbs, prompt shape, status block |
| backend-nouq | True-async job mechanism (phase 1: state-dir job journal) |

Suggested order: backend-a0k5 first (the conventions cost nothing and
make tag-echo v1 real), then backend-y4wl + backend-tocb (the app's
reads), backend-a9xj + backend-9qpg (the phone paths), backend-n4me any
time, backend-nouq last and largest.

## Verification of this spec's examples

Checked while writing, 2026-11-23:

- `quartermaster --help` and the shared flag set in `main.go` — every
  command and flag named as *existing* (`context`, `digest`, `inbox
  done`, `prompt`, `answer --expect`, `tap links`, `rpc tell/receipt`)
  is spelled as shipped; new flags follow the same shared-`FlagSet`
  convention.
- One staged `quartermaster context` run measured 41,512 bytes with the
  per-section split in the table above; live inbox files confirm the
  `{bead, kind, summary, at}` event shape.
- `bd show backend-hvez --json` confirms the fields `show --json` is
  specified to carry.
- The status-block example parses with the python one-liner printed in
  this doc (it prints the example's five fields).
- Host facts for the async comparison (`command -v` for the four
  daemons, the running Postgres's owner and `fsync=off`, `go.mod`'s
  dependency list) were read from this machine, not assumed.
