# Async coordinator jobs (`quartermaster job`)

Phase 1 of the true-async job mechanism specified in [docs/design/coordinator-api.md](design/coordinator-api.md): a durable, state-dir-backed job queue driven by the existing ticker with no additional daemon.

## Overview

Submitting a request writes an immutable spec and fsynced event log immediately and returns a durable content-addressed job ID without waiting for the ticker or an active coordinator session:

```sh
quartermaster job submit --key app-20261124-0017 --by TJ -- ANSWER task-1 2 — TJ tapped option 2
# -> job=7f3a9c... state=queued

quartermaster job status 7f3a9c      # queued | running | parked | done | blocked | needs_tj
quartermaster job result 7f3a9c      # audit note + fenced qm-status block
quartermaster job list [--bead B]    # newest first, with age (no daemon needed)
quartermaster job cancel 7f3a9c      # only before the effect runs (queued or parked)
```

All commands also support `--json` for structured output.

## Storage layout

Jobs live in private (`0700` / `0600`) files under `<state dir>/jobs/`, following the executor's `planEvent` journal pattern:

- `jobs/<id>/job.json` — immutable `JobSpec` (`version`, `id`, `key`, `request`, `created_at`). `<id>` is the SHA-256 hex digest of canonical `(version, key, request)`.
- `jobs/<id>/events.jsonl` — append-only, fsynced transition log (`queued`, `claimed`, `intent`, `outcome`, `parked`, `answer_applied`, `done`, `blocked`, `needs_tj`, `canceled`), mirrored to `journal.jsonl` (`kind: "job"`).
- `jobs/keys/<key>.json` — atomic idempotency-key index mapping `--key` to its job ID and request digest.
- `jobs/claims/<digest>.json` — atomic at-most-once claim records keyed by `(kind, bead, hash)` for answers (`prompt`, `needs_me`, `parked_answer`).
- `jobs/holds/<bead>.json` — durable `HOLD` markers checked by `DISPATCH` and cleared by `RESUME`.
- `jobs/jobs.lock` — cross-process `flock` guarding short atomic metadata transitions (never held across subprocess or network calls).

Status, result, and list commands read directly from disk and work with neither the ticker nor a coordinator session running.

## Gate verbs

| Verb | Target | Effect |
|---|---|---|
| `MERGE` | `<bead>` or GitHub PR URL | Resolves PR/bead and merges through the checked executor (`github.pr.merge`, `--match-head-commit`) |
| `DISPATCH` | `<bead>` | Starts a worker on the bead (refused if held via `HOLD`) |
| `CLOSE` | `<bead> — <reason>` | Closes the bead via `quartermaster close` (with verifier when in scope) |
| `HOLD` | `<bead>` | Notes the hold and blocks `DISPATCH` until `RESUME` |
| `RESUME` | `<bead>` | Clears any hold and restarts the bead's worker |
| `STATUS` | `<bead>`, PR URL, or none | Fresh read-only status (compact fleet summary when no target is given) |
| `STOP` | `<bead>` | Stops the worker while leaving the bead claimed |
| `ANSWER` | `<bead> <choice>` or `<bead> — <text>` | Answers a live approval prompt or `needs_me` question (with or without `OPTIONS:`) at most once |

## Semantics and crash recovery

- **Idempotency (`--key`):** Re-submitting the same `--key` and request returns the original job without creating a duplicate or re-running its effect. Re-submitting the same `--key` with a different request fails with a conflict error.
- **At-most-once answers:** Before answering a prompt or `needs_me` question, the ticker atomically claims `(kind, bead, hash)` under `jobs.lock` and checks existing notes and `journal.jsonl`. Racing jobs for the same live question resolve to one `done` and one `blocked` (`already answered`).
- **Human-in-the-loop waits (`parked`):** When a non-TJ job exceeds the bead's autonomy level (or rolling budget) and the bead has no other open `needs_me` question, the ticker writes a hash-compatible `QUESTION:`/`OPTIONS:`/`CLASS:` note, sets the bead to `needs_me`, and parks the job in `state=parked`. Parked jobs never expire silently; `job list` and `job status` display their age. When TJ answers (by phone tap or an `ANSWER --by TJ` job), the answer is claimed atomically (`parked_answer`) and applied at most once.
- **Crash windows:** Every mutating effect is bracketed by an fsynced `intent` and `outcome`. On restart, the ticker reconciles unfinished `running` jobs by probing live state first: effects already landed settle `done` without re-execution; retry-safe operations (`MERGE`, `CLOSE`, `HOLD`, `STATUS`) retry only when confirmed not yet applied; non-retry-safe operations (`ANSWER`, `DISPATCH`, `RESUME`, `STOP`) never blind-retry after `intent` and stop in `needs_tj`.
- **Shared `qm-status` result:** Terminal jobs (`done`, `blocked`, `needs_tj`) render their audit note and a fenced `qm-status` block including `job=<id>` via `quartermaster job result <id>`.
