# Durable rollout workflows and structured cards

Quartermaster provides two composable building blocks for structured human-in-the-loop
decisions and multi-step rollouts:

1. **Structured `ask_user` and `report_milestone` cards** (`cards.go`, `quartermaster card`,
   and the `ask_user` / `report_milestone` Pi extension tools).
2. **Durable, code-defined rollout workflows** (`workflow.go`, `quartermaster workflow`)
   layered cleanly on top of the checked execution-plan IR and executor (`plan.go` / `executor.go`).

## Structured cards (`quartermaster card`)

### Storage and versioned JSON schema

Each bead's cards live in a private directory (`0700` dir, `0600` files) under the state directory:

- `<state dir>/cards/<bead>/question.json` — atomically written active question card (schema `version: 1`)
- `<state dir>/cards/<bead>/milestones.jsonl` — append-only, fsynced milestone records (schema `version: 1`)

#### `QuestionCard` (`question.json`)

```json
{
  "version": 1,
  "bead": "task-1",
  "created_at": "2026-11-24T00:00:00Z",
  "hash": "a1b2c3d4",
  "question": "Roll out to one region first or all regions?",
  "details": "Canary metrics are still warming up in region A.",
  "critical": false,
  "class": 7,
  "class_reason": "production rollout",
  "recommendation": { "option": 2, "rationale": "limit blast radius" },
  "options": [
    { "number": 1, "label": "All regions at once", "description": "Faster rollout", "class": 7 },
    { "number": 2, "label": "One region first", "description": "Canary first", "class": 7 }
  ],
  "evidence": {
    "pr_url": "https://github.com/acme/app/pull/42",
    "plan_digest": "0123456789abcdef...",
    "run_urls": ["https://github.com/acme/app/actions/runs/999"],
    "cost_estimate": "$120/month",
    "iac": {
      "tool": "pulumi",
      "target": "org/app/prod",
      "create": 1,
      "update": 0,
      "replace": 0,
      "delete": 0,
      "summary": "1 create, 0 update, 0 replace, 0 destroy",
      "excerpt": "plan: pulumi, 1 create\nclass: 7 (org/app/prod: production)\n..."
    },
    "links": [{ "label": "PR", "url": "https://github.com/acme/app/pull/42" }]
  }
}
```

#### `MilestoneRecord` (`milestones.jsonl`)

```json
{
  "version": 1,
  "at": "2026-11-24T00:00:00Z",
  "bead": "task-1",
  "phase": "pr_opened",
  "status": "ok",
  "summary": "Opened PR #42",
  "evidence": {
    "pr_url": "https://github.com/acme/app/pull/42",
    "run_url": "https://github.com/acme/app/actions/runs/999",
    "sha": "0123456789abcdef0123456789abcdef01234567",
    "plan_digest": "abcdef...",
    "links": []
  }
}
```

### Compatibility and safety guarantees

- **Hash parity with approval taps (`tapquestion.go`).** When `card ask` records a question,
  it formats the matching `QUESTION:` / `OPTIONS:` / `CLASS:` (and optional `CRITICAL:`) bead
  note, computes `QuestionCard.Hash` through `parseNeedsMeQuestion`, sets bead status to
  `needs_me`, and appends a journal record. Signed `/tap/<token>` links and web inbox actions
  target that exact hash.
- **Class elevation (never lowering).** Options must be 2–9 items, and `class` must be 0–9.
  When `--iac-preview <file>` is supplied, quartermaster classifies the preview via `gate.go`
  (`checkPlan` + `classify`) and elevates `class` to at least the preview's computed class
  (and class 9 on parse/expect problems). Raw JSON `evidence.iac` counts enforce a conservative
  minimum class as well. Class 8 and 9 questions are automatically marked `critical: true`.
- **Stale detection.** Readers verify `question.json` against the live bead: if the bead leaves
  `needs_me`, waits on a `RUN:` command, or its notes no longer end in an `OPTIONS:` question
  hashing to `QuestionCard.Hash`, the card is treated as stale/superseded.

### Card CLI commands

```sh
# Ask via JSON on stdin or file
quartermaster card ask <bead> --file <json|-> [--iac-preview preview.json --iac-target org/app/prod] [--json]

# Ask via flags
quartermaster card ask <bead> \
  --question "Apply stack change?" \
  --option "Approve apply" --option "Abort" \
  --class 7 --class-reason "production apply" \
  --recommend 1 --rationale "preview matches expected changes" \
  --iac-preview preview.json --iac-target org/app/prod --iac-expect create=1,update=1

# Append a structured milestone
quartermaster card milestone <bead> --phase merged --status ok --summary "Merged PR #42" \
  --pr-url https://github.com/acme/app/pull/42 --sha <40-char-sha>

# Inspect or list cards
quartermaster card show <bead> [--json]
quartermaster card list [--json]
```

## Durable rollout workflows (`quartermaster workflow`)

Rollout workflows orchestrate multi-step delivery (waiting on checks/runs, gating IaC previews,
requesting digest-bound approvals, executing checked plans, recording milestones, and verifying
post-apply state) without arbitrary shell evaluation.

### Storage and durability

Each workflow run persists under `<state dir>/workflows/<id>/` (`0700` dir, `0600` files):

- `spec.json` — immutable workflow spec, template name, variables, version, and canonical SHA-256 digest.
  Loading verifies the digest so on-disk edits are rejected.
- `events.jsonl` — append-only, fsynced transition log (`started`, `step_started`, `step_waiting`,
  `step_approved`, `step_ok`, `step_failed`, `completed`, `canceled`), mirrored into `journal.jsonl`.

Per-workflow file locks prevent concurrent advances from racing. Crash recovery replays
`events.jsonl` and checks underlying `plan.go` / `executor.go` event logs and `milestones.jsonl`
before acting, so `workflow resume` is idempotent.

### Typed step kinds

Custom JSON specs are decoded with `DisallowUnknownFields()` and accept only these typed kinds:

| Kind | Behavior |
|---|---|
| `plan_step` | Lowers a typed `plan.go` `Step` (`github.pr.merge`, `github.pr.automerge`, `iac.apply`, or `opaque`) into a checked `Plan` and executes it through `executor.go`, preserving SHA-256 digests, preconditions, CAS, and probes. |
| `gate_check` | Runs `gate.go` (`checkPlan` + `expect` + `classify`) on a preview file, records its SHA-256 digest and computed impact class (never lowering it; missing `expect` moves one class up), and fails on parse/expect problems. |
| `approval` | Computes effective class (`max(step.class, actionClass, gate_check class, next plan_step class)`) and binds approval to the preview/plan digest. If `!always_ask`, `class <= beadLevel`, and `[autonomy.budget]` allows, records dial approval + `AUTO:` note; otherwise emits a structured `QuestionCard` (attaching IaC/PR/digest evidence), sets `needs_me`, and parks in `step_waiting`. Approvals expire (default `1h`) and are rechecked before subsequent steps execute. |
| `wait` | Non-blocking, idempotent wait gate: `pr_ready`, `pr_merged`, `run_completed` (skips cleanly if `optional: true` and `run_url` is empty), or `signal`. |
| `milestone` | Appends a structured `MilestoneRecord` (`cards.go`) and `MILESTONE [<phase>]: <summary>` note. |
| `verify` | Either `no_drift` (runs `gate.go` expecting zero non-quiet changes on a post-apply preview) or `acceptance` (checks a fresh verifier pass in `journal.jsonl` or runs the configured read-only acceptance verifier). |

### Built-in templates

Run `quartermaster workflow list-templates [--json]` to inspect built-in templates:

- **`pr-merge-verify`** (`--var pr_url=...`, optional `head_sha`, `method`, `run_url`):
  `wait(pr_ready)` → `approval(merge)` → `plan_step(github.pr.merge)` → `milestone(merged)` → `wait(run_completed, optional)` → `milestone(verified)`
- **`iac-rollout`** (`--var target=pulumi:<stack>|tofu:<name> --var preview=<file>`, optional `expect`, `rev`, `repo`, `dir`, `stack`, `workdir`, `planfile`, `cas`, `verify_preview`):
  `gate_check(preview)` → `approval(apply)` → `plan_step(iac.apply)` → `milestone(applied)` → `verify(no_drift)` → `milestone(verified)`

### Workflow CLI commands

```sh
quartermaster workflow list-templates [--json]

quartermaster workflow start <bead> --template pr-merge-verify \
  --var pr_url=https://github.com/acme/app/pull/42 --var head_sha=<40-char-sha>

quartermaster workflow start <bead> --template iac-rollout \
  --var target=pulumi:org/app/prod --var preview=preview.json \
  --var expect=create=1,update=1 --var verify_preview=post-apply.json

quartermaster workflow start <bead> --file workflow.json

quartermaster workflow status [<id|bead>] [--json]
quartermaster workflow advance <id> [--json]
quartermaster workflow resume <id> [--json]
quartermaster workflow approve <id> --step <n> --by TJ [--for 1h] [--json]
quartermaster workflow cancel <id> [--reason "..."] [--json]
```

### Ticker advancement and approval security

On each pass, the ticker runs a bounded, non-blocking advancement sweep over active workflows
in `waiting` (or unfinished `running`) state:

- Satisfied wait gates (`pr_ready`, `pr_merged`, `run_completed`) advance automatically.
- Waiting `approval` steps advance only when TJ explicitly approved via
  `quartermaster workflow approve <id> --step <n> --by TJ` or tapped option 1 on the
  emitted structured `QuestionCard` (verified against `journal.jsonl` by card hash and `by TJ`),
  and the bound preview/plan digest still matches and has not expired.
- Tapping option 2 on the emitted `QuestionCard` cancels the workflow without executing
  downstream steps.
- If a preview file changes on disk between `gate_check` / `approval` and `plan_step`, both
  `workflow approve` and `plan_step` refuse execution on the digest mismatch.
- The ticker never approves anything above the bead's autonomy level or budget on its own.

## End-to-end delivery loop (ping-pong review and chained rollouts)

For the coordinator-driven **design → implement ↔ skeptical reviewer ping-pong →
PR/merge → auto-chained rollout-to-target** workflow (`quartermaster review` and
`quartermaster rollout`), see
[docs/workflow-delivery-loop.md](workflow-delivery-loop.md).
