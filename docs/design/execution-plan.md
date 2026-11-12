# Execution plans: a checked, durable executor for the autonomy dial

Status: **accepted** (2026-11-08); P1 in progress: the IR, digest and
`quartermaster plan check|approve|status` are in (`plan.go`), and so is the
executor daemon with per-bead sockets, durable steps and resume
(`executor.go`, `plan run|resume`, `quartermaster executor`); the shims and
the ops' runners are not. It designs the
"later, enforced" phase of [autonomy-dial.md](autonomy-dial.md) (the
credential-holding executor). TJ chose the recommendation (R) for every
decision; see "Decisions" at the end.

## Summary

The [post that prompted this](https://x.com/tobi/status/2101832189469929494)
argues that MCP versus CLI is the wrong layer. Both work well inside a
REPL-like environment that keeps state, and that is a property of the
harness. It predicts an embeddable, SQLite-style execution environment that
parses bash, TypeScript or tool calls into one intermediate representation,
an **execution plan**, that can be security-checked before it runs and is
built for durable execution. CLI, MCP and WebMCP would all plug into it.

Quartermaster needs most of this already. The autonomy dial is
**advisory**: workers hold their own `gh` token and cloud roles, and they
classify their own steps with `quartermaster autonomy check`. A worker that
skips the check can still merge, apply or post. The dial becomes enforced
only when the credentials for those effects move out of the agents and into
something that checks before it acts.

This design builds that thing, scoped to what quartermaster needs:

1. **The plan is a list of typed operations, not a program.** Each step names
   an operation (`github.pr.merge`, `iac.apply`, ...), a canonical target,
   its arguments, the preconditions it must still meet when it runs (PR head
   SHA, plan-file digest), and the credential it needs. A plan has a digest,
   and approval binds to that digest.
2. **The executor holds the credentials and the agents don't.** It is a small
   Go component of the `quartermaster` binary. It checks each plan against
   the dial with the classifiers that exist today (`actionClass`,
   `targetClass`, `gate plan`), rechecks preconditions right before each
   step, journals intent and outcome, and resumes after a crash.
3. **Lower argv, not bash.** Most credentialed commands are built from shell
   variables and substitutions (see "What workers run today"), so a parser
   that reads the command string can't see what will run. A `gh`, `pulumi`
   or `tofu` shim on the worker's `PATH` receives the **final argv** after
   the shell expanded it, and lowers that. The executor checks exactly what
   it will execute. Parsing bash is useful only for a preview that asks or
   refuses; it is never what grants permission.
4. **Anything that can't be lowered is opaque, and opaque is class 9.** An
   opaque step runs only with TJ's approval of its digest. Lowering is a
   convenience: credential custody does the enforcing.
5. **v1 is thin:** merges and IaC applies, the effects at classes 4, 6 and
   7 that the dial exists for. Deploy-service provisions, messages and
   other connectors follow with the executor's MCP server. Browser actions
   stay advisory, and `autonomy level` says which are enforced.

The full bet, one interpreter for bash, TypeScript and tool calls, is not
recommended for quartermaster. The side effects that matter funnel through
five binaries and three connectors, and interposing at those points gets
the security property without reimplementing a shell (see "Build or adopt").

## What workers run today

This is a census of the 336 Claude Code transcripts on this machine (44,155
Bash calls and 9,538 MCP calls), counting commands that need a credential
beyond the worktree. "Substituted" means the command contains `$(...)`,
backticks, a `$VAR` or `sh -c`, so its effect can't be read from the string.

| Effect | Calls | Substituted |
|---|---:|---:|
| `git push` | 1,368 | 63% |
| `gh pr create/edit/ready` | 983 | 90% |
| `gh pr merge` | 702 | 45% |
| `gh workflow run`, `gh run rerun` | 332 | 40% |
| `gh api` writes (REST writes, GraphQL mutations) | 332 | 91% |
| `gh pr/issue comment` | 214 | 79% |
| `helm upgrade/install/rollback` | 103 | 51% |
| `aws` writes | 101 | 69% |
| `gh pr review` | 36 | 61% |
| `kubectl` writes | 34 | 52% |
| `pulumi up/destroy/refresh` | 19 | 63% |
| `tofu`/`terraform apply` | 2 | 50% |

Write-shaped MCP calls: 2,241. Most are issue-tracker writes (1,675). The
rest are chat messages (146), deploy-service provisions (62), spreadsheet
and doc writes, and browser actions (around 200).

What this says:

- **A string parser can't classify most of these.** `gh pr merge "$url"
  --squash --match-head-commit "$sha"` is the common shape. The executor has
  to see the argv after expansion. That is what the shim gets.
- **The dangerous set is small and concentrated.** Nearly all class 4+
  effects go through `gh`, `pulumi`/`tofu`/`helm`/`kubectl`/`aws`, the
  deploy-service connector and the chat connector.
- **Hosted connectors are a different problem.** Their credentials are held
  by the agent product's account and never pass through the machine, so the
  executor can't take them away. It can only replace them (see
  "Integration").

## The IR: an execution plan

```go
// A Plan is what a worker proposes and the executor checks, approves and runs.
type Plan struct {
	Bead   string // the bead it serves; the dial level comes from it
	Actor  string // worker or coordinator, and its pane
	Source string // how it arrived: shim, mcp, hook, cli
	Steps  []Step
}

type Step struct {
	Op     string            // a name from the op table: "github.pr.merge"
	Target string            // canonical: "github:owner/repo#55", "pulumi:org/proj/stack"
	Args   map[string]string // op-specific, already expanded: no shell left in them
	Pre    map[string]string // must still hold when the step runs: head_sha, plan_digest, rev
	Text   string            // for an opaque step, the exact argv or tool call
}
```

- **The digest** is SHA-256 over the canonical JSON of the plan: sorted keys,
  without run state. Approving a plan, whether by the dial or by TJ, binds
  to the digest. A new push changes `head_sha`, so it makes a new plan with a
  new digest that needs a new approval.
- **Effects aren't declared by the proposer.** The executor derives each
  step's class from the op table and the target, then takes the highest of
  that, the class the worker states, and `gate plan`'s class. This is the
  dial's D3 rule. A worker can raise a class but never lower one.
- **Credentials are named grants**, such as `github:merge:owner/repo` or
  `aws:deploy:account/role`. A step says which one it needs, and the
  executor maps it to the token or role it holds. Agents never see the
  secret.
- **Preconditions are checked by the executor, against the provider.** It
  doesn't take the worker's word that checks are green; it asks GitHub.
  Where the provider offers a compare-and-swap, the executor uses it, so the
  check and the act can't drift apart:

| Op | Class | Preconditions, and the provider's CAS | Probe (did it happen?) |
|---|---|---|---|
| `github.pr.merge`, `github.pr.automerge` | 4, or the repo's `merge_class` | Required review and checks green, read by the executor; `--match-head-commit <head_sha>` | PR merged at that head, or auto-merge enabled |
| `github.comment` (trigger comments such as `/plan`) | 2, or the class of what the trigger runs (a table keyed by the first word) | PR head SHA | A comment carrying the marker `<!-- qm:<digest>:<step> -->` |
| `iac.apply` (Pulumi, OpenTofu) | 6 or 7 by target; `gate plan` may raise it to 8 or 9 | Commit `rev` and expected counts. The executor plans from a clean checkout of `rev`, never from the agent's worktree, and the saved plan's digest must match: `pulumi preview --save-plan` then `pulumi up --plan`, or `tofu plan -out` then `tofu apply <planfile>` | Stack history shows an update tagged `qm:<digest>:<step>`, or a fresh plan shows no changes |
| `deploy.provision` (a deploy service's API) | 6 or 7 by environment | Version and environment; an idempotency key derived from the digest | Provision status for that key |
| `opaque` | 9 | None that can be checked | None, so a crash mid-step stops for TJ |

Pulumi's update plans are still experimental. Until they are stable,
`iac.apply` on Pulumi re-runs the preview immediately before `up` and
refuses if its counts differ from the approved preview. That is weaker than
a CAS, and the doc says so where it matters (D7).

### Front ends: how each surface lowers

| Surface | Lowers how | What can't lower |
|---|---|---|
| CLI (`gh`, `pulumi`, `tofu`, later `helm`, `kubectl`, `aws`) | A shim on `PATH` sends its final argv to the executor, which parses the argv with a per-tool table: `gh pr merge <n> --squash --match-head-commit <sha>` becomes `github.pr.merge`. Read-only subcommands (`gh pr view`, `pulumi preview`) run straight through, using the agent's read-only credentials | A subcommand or flag outside the table, which becomes `opaque` |
| `gh api` | Method plus path pattern: `PUT repos/{o}/{r}/pulls/{n}/merge` becomes `github.pr.merge`. GraphQL lowers by mutation name (`resolveReviewThread` is class 2 on the worker's own PR) | Unlisted paths and mutations, which become `opaque` |
| MCP | The executor serves an MCP server with `plan_run`, `plan_status` and typed tools (`merge_pr`, `apply_stack`, `provision`). Each call is already a step | Nothing on the executor's own server. Other connectors' write tools are denied to workers (see "Integration") |
| Agent pre-tool hooks (Claude Code `PreToolUse`) | Preview only: parse the Bash command with `mvdan.cc/sh`, and when it calls a credentialed binary in a way the shim would refuse, deny early with the reason. It never allows anything | Everything the parser can't resolve, which falls through to the shim |
| Browser | Doesn't lower. Workers get no logged-in browser profile for systems the executor covers | All of it, so it stays advisory |

**The rule that keeps this sound, taken from Loom (below): analyze what you
execute.** If one parser summarizes a command and a different one runs it,
any disagreement between them becomes a way around the policy. Here the
executor parses the argv it will pass to the real binary itself, so the only
mismatch possible is between the lowering table and the binary's real
behaviour, and that is testable. The hook's bash parse can only refuse, so
when it's wrong the cost is a false refusal, never a bypass.

## Checking

`quartermaster plan check <plan>` runs before anything else and makes no
changes:

1. Look up each step's op. An unknown op lowers to `opaque` (class 9).
2. Classify it: `actionClass`/`targetClass` for the op and target, and for
   `iac.apply`, the executor's own preview through `checkPlan`/`classify`
   from `gate.go`. The plan's class is its highest step's.
3. Compare it with the bead's effective level (`Config.beadLevel`) and the
   budgets (`budgetSpent`). It passes if the class is at most the level,
   exactly the existing `autonomy check`.
4. Record a `check` journal record with the digest.

A plan that passes is **approved by the dial**. One that fails goes to TJ as
a `needs_me` with the digest, the class and a one-screen rendering of the
plan; `quartermaster plan approve <digest>` approves it. An approval expires
(default 1 hour), and before every step the executor rechecks the level,
budget and preconditions. A level lowered mid-plan stops it at the next
step.

## Execution and durability

The executor is part of the `quartermaster` binary, run as `quartermaster
executor` under its own systemd user unit next to the ticker, and it
listens on a Unix socket. It runs one plan per bead at a time, and plans for
different beads in parallel.

**State** lives in the state dir: `plans/<digest>/plan.json` (immutable) and
`plans/<digest>/events.jsonl` (append-only, fsynced), mirrored into
`journal.jsonl` so the digest and report see it. These are the events:

```
checked(class, level, policy_rev) → approved(by: dial|TJ, expires)
  → intent(step i, attempt n) → outcome(step i: ok|failed|unknown, evidence)
  → … → done | failed | needs_me | expired
```

**Write intent before every effect.** This is the dial's existing rule for
class 6 and up, applied to every step.

**Resume.** On start, and after any crash, the executor walks the plans
that haven't finished. For a step with an intent and no outcome:

1. Run its **probe**. If the effect happened (PR merged at that head, stack
   update tagged with this digest), record the outcome and go on.
2. If it didn't, **recheck preconditions**. A head SHA that moved, or a plan
   digest that no longer matches, fails the step cleanly. A retry can't
   apply something stale.
3. If the op is marked retry-safe, which holds for every op with a provider
   CAS, retry with backoff up to a limit. If not, or if the probe can't
   tell, stop the plan and raise `needs_me`.

This is at-least-once with a probe, not exactly-once, which no external API
offers. An op with neither a CAS nor a probe can't be retried
automatically. That is the honest form of "durable" here.

**Results go back to the agent** as the shim's exit status and output, so a
worker running `gh pr merge` sees an ordinary result. A plan that is waiting
on TJ exits nonzero with the digest and "waiting for approval:
`quartermaster plan status <digest>`".

## Integration: routing workers through it, and moving credentials out

Routing comes first and custody second. Routing without custody is still
advisory, but it puts every credentialed effect on one checked, journaled
path, and that is worth having on its own.

| Agent | Routing |
|---|---|
| Any agent | `dispatch` puts a shim directory first on the worker's `PATH` (`gh`, `pulumi`, `tofu`). Each shim runs `quartermaster run -- <argv>`, which reads stdin/stdout and the exit code over the socket |
| Claude Code | The `PreToolUse` hook gives an early refusal with a reason. Settings deny the write tools of connectors the executor replaces |
| Codex, Muse | The shim alone. Their own pre-tool mechanisms, where they have them, can take the same preview hook later |
| The coordinator | Its existing enforced commands (`dispatch`, `answer`, ...) stay as they are. It submits no plans of its own in v1 |

**Who is calling.** A request has to name its bead. The executor shouldn't
trust an environment variable the agent controls, so each worker gets
**its own socket** (`run/<bead>.sock`), created by `dispatch` and mounted
or placed only where that worker can reach it. The socket a request arrives
on is its identity. On the same Unix user that is still forgeable, and the
custody stages below say when it stops being.

**Credential custody, in stages:**

| Stage | Agents hold | Executor holds | Enforced? |
|---|---|---|---|
| S0 (today) | Everything | Nothing | No |
| S1 (v1) | Everything, but the shims route `gh pr merge`, applies and provisions through the executor | The same credentials | No: routing and journal only |
| S2 | A GitHub token that can push branches and comment, but can't update default branches: repository rulesets name only the executor's GitHub App as allowed to update them. Read-only cloud roles that can still preview | A GitHub App installation token with merge rights; deploy roles; the IaC state-backend token | Merges and applies, **if** the executor runs as a different Unix user or the workers run in containers (the [worker-hosts](worker-hosts.md) image), so agents can't read its files or trace it |
| S3 | Connector write tools denied in agent settings | Its own tokens for chat, issue-tracker and deploy-service APIs, exposed through its MCP server | Messages and provisions too |
| — | Browser sessions | — | Never: browser actions stay advisory and `autonomy level` says so |

There are two things about S2 that are easy to get wrong:

- GitHub can't separate "push a branch" from "merge a PR" by token scope,
  since both need `contents: write`. The separation comes from rulesets on
  the default branch, which apply to merges through the API too. Setting
  them is an admin change on each repo (class 8 by the ladder, so it's
  TJ's). Approving a PR, which needs `pull_requests: write`, can't be
  separated from commenting either. It stays advisory until the agents'
  token loses `pull_requests: write` and comments move to the executor.
- **A separate Unix user is the real boundary.** If the executor runs as
  the same user, any agent can read its credential files or attach a
  debugger to it. S2 isn't "enforced" until the executor runs as its own
  user, with the socket's group limited to the worker it serves, or until
  workers run in containers that can't see the host's files.

When an op family becomes enforced, `autonomy level` and the board change
"advisory" to "enforced: merge, apply; advisory: messages, browser".

## Build or adopt

| Prior art | What it does | Use here |
|---|---|---|
| [Loom #525, "lower bash into an analyzable effect plan"](https://github.com/Roasbeef/loom/issues/525) | The closest design. It cites the same post. Bash is lowered to a plan whose nodes are capability calls, constructs it can't handle become `Opaque` at the top of a lattice, and the summary may only ask or refuse until the plan itself is what runs. It also starts with a census | **Borrow the rules**: analyze what you execute, opaque at the top, census first. Not its scope: a full shell interpreter is unnecessary when credentials funnel through a few binaries |
| [`mvdan.cc/sh`](https://github.com/mvdan/sh) (`shfmt`'s parser and interpreter, in Go) | Parses bash to an AST; `interp` executes it | **Adopt** for the hook's preview in phase 4. It's Go, so there's no FFI. Don't use `interp` as the executor: argv-level interposition makes it unnecessary |
| [just-bash](https://github.com/vercel-labs/just-bash) (TypeScript) | An emulated bash with a virtual filesystem for untrusted agent scripts | Not adopted: it's in the wrong language, and the problem here is credentials, not containing reads |
| Code mode ([Cloudflare](https://blog.cloudflare.com/code-mode/)), isolate REPLs (QuickJS, `isolated-vm`) | The model writes TypeScript against typed bindings in an isolate with no network. Credentials live in the bindings, outside the sandbox | **Same idea as the custody split**, applied to a CLI-shaped harness instead. Not adopted: workers already have a REPL (bash plus the filesystem), so there's nothing to replace |
| [CaMeL](https://arxiv.org/abs/2503.18813) (prompt injection defeated by design) | A restricted interpreter that tracks where each value came from, so a policy can refuse a tool call whose arguments came from untrusted data | Later, maybe. The plan records `Source` and the bead; tracking taint per argument needs the interpreter this design avoids. The dial's "task bounds the dial" rule stays the advisory defence |
| Temporal, Restate | Durable execution services | Not adopted: they mean running a cluster, for one machine with tens of short plans a day |
| [DBOS Transact for Go](https://github.com/dbos-inc/dbos-transact-golang) | A durable workflow library, checkpointing to Postgres and, in recent releases, SQLite | **Revisit** if plans grow long-running timers (multi-day staged rollouts). Today's plans are data of 1–5 steps, so a step table and an event log are about 200 lines, and a workflow-as-code library fits them poorly |
| OPA/Rego, Cedar (`cedar-go`) | Policy languages | Not now: the policy is a class lookup that already exists in Go (`actionClasses`, `targetClass`, `classify`). Revisit if per-repo or per-target rules multiply |
| GitHub Apps, rulesets, `--match-head-commit`; AWS role split and session policies; Pulumi update plans; OpenTofu saved plans | Provider-side scoping and compare-and-swap | **Adopt** all of them. They are what make the preconditions binding, not the executor's own checks |
| Agent sandboxes (Claude Code's sandbox mode, `bubblewrap`, containers) | Limit an agent's filesystem and network | **Complementary**: they shrink class 1–3 blast radius. They don't hold credentials for the agent, so they don't replace custody |
| MCP gateways and proxies | Policy in front of MCP servers | Not adopted: the executor's own MCP server is a few tools. It can sit behind a gateway later |

**Does the bet already exist?** In pieces. Loom has the bash design, code
mode has the isolate-with-bindings design, CaMeL has the policy, and DBOS
and Temporal have the durability. Nothing found ships the combination as an
embeddable library. For quartermaster the combination isn't needed:
**build** the small executor, and **adopt** the provider-side CAS
mechanisms and `mvdan.cc/sh`.

## Plan and size

| Phase | What | Size | Value on its own |
|---|---|---|---|
| P0 | The census above, committed as `scripts/census.py`, rerunnable | Done in this PR; about 60 lines | Picks the v1 op set from data |
| **P1 (v1)** | `Plan`/`Step`, digest, op table for `github.pr.merge`/`automerge` and `iac.apply` (Pulumi, OpenTofu); `plan check|approve|status|resume`; `quartermaster executor` with per-bead sockets, intent/outcome events, probes and resume; the `gh`/`pulumi`/`tofu` shims placed by `dispatch` (anything not in the table passes through to the real binary); journal and digest integration | About 1,800 lines of Go with tests; 4 PRs (IR and check, executor and durability, shims and dispatch, IaC apply) | Every merge and apply on one checked, journaled, resumable path, with applies from a clean `rev` instead of the agent's worktree. Still advisory (S1) |
| P2 | Custody for GitHub and cloud (S2): the executor as its own Unix user, a GitHub App, rulesets on the repos it covers, split cloud roles; "enforced" in `autonomy level` | About 400 lines plus setup TJ runs (App creation, rulesets, IAM roles: class 8–9 steps) | **The dial is enforced for merges and applies** |
| P3 | The executor's MCP server; `deploy.provision`, and chat, issue-tracker and deploy-service writes, through it with its own tokens; connector write tools denied to workers (S3) | About 600 lines | Messages and provisions enforced |
| P4 | `PreToolUse` preview with `mvdan.cc/sh`: early refusals with reasons, and the plan rendered in TJ's approval | About 300 lines | Fewer wasted turns, better approval screens |
| Later, maybe | `helm`/`kubectl`/`aws` op tables; CaMeL-style provenance; DBOS if plans need timers; a general plan interpreter only if the census shows workers need effects the shims can't see | — | — |

## Decisions for TJ

The options as they were put to TJ; (R) is the recommendation. The outcome
is in "Decisions" at the end.

### E1. How wide the IR is

- **(R) a.** Typed operations for credentialed effects only. The agent's
  shell, edits and builds stay where they are, and anything that can't be
  lowered is opaque at class 9.
- b. Tobi's full bet: an interpreter that lowers all of bash, TypeScript and
  tool calls into one plan, and executes the plan (Loom's direction).
  Stronger for read and filesystem policy, but several times the work, and
  the dial doesn't need it.

### E2. Where lowering happens

- **(R) a.** At argv, in a shim, after the shell has expanded it. The hook's
  bash parse may only refuse.
- b. In the `PreToolUse` hook, from the command string. That misses 45–90%
  of credentialed commands (see the census) and is Claude Code only.

### E3. Where the executor runs

- **(R) a.** A `quartermaster executor` daemon on a Unix socket, per-bead
  sockets for identity; in v1 the same user, from P2 its own Unix user, or
  the host outside the worker containers.
- b. In-process in each `quartermaster run`. That's simpler, but the
  credentials are then readable by the caller, so it can never enforce.
- c. A remote service. That's overkill for one machine, and the worker-hosts
  design already gives a boundary between host and container.

### E4. Durability

- **(R) a.** Build it: a step table, an fsynced event log per plan,
  provider CAS plus probes, at-least-once delivery, and `needs_me` when a
  step can't be retried and its probe can't tell.
- b. Adopt DBOS for Go on SQLite now.
- c. Temporal.

### E5. Policy

- **(R) a.** Keep the Go class tables and reuse `gate.go`'s classifier.
- b. Move policy to Cedar or OPA now.

### E6. Approval

- **(R) a.** Approval binds to the plan digest and expires after 1 hour.
  The level, budget and preconditions are rechecked before every step, and
  a new head or a new IaC plan means a new digest.
- b. Approval binds to the bead and op, so TJ approves "merges on this
  bead" once. Fewer prompts, but an approval no longer says what it
  approved.

### E7. Pulumi without a stable update-plan CAS

- **(R) a.** Use `--save-plan`/`--plan` with `PULUMI_EXPERIMENTAL`, and fall
  back to "re-preview right before `up`, refuse on any difference" if it
  misbehaves. Note the fallback window in the AUTO note.
- b. Only re-preview, and wait for update plans to be stable.

### E8. v1 scope

- **(R) a.** Merges and IaC applies (classes 4, 6 and 7). Deploy-service
  provisions, messages and other connectors in P3, since they arrive as
  connector calls and need the executor's MCP server; the browser stays
  advisory.
- b. Merges only, the smallest useful slice, with applies in P2.
- c. Include provisions and messages in v1, which brings the MCP server
  forward.

## Decisions

2026-11-08, TJ: **all R** on E1–E8.

- **E1** typed operations for credentialed effects only; anything that
  can't be lowered is opaque, class 9. No general bash or TypeScript
  interpreter.
- **E2** lowering happens at argv, in `PATH` shims; the hook's bash parse
  may only refuse.
- **E3** a `quartermaster executor` daemon with per-bead sockets; the same
  Unix user in v1, its own user (or outside the worker containers) from P2.
- **E4** durability is built: an event log per plan, provider CAS plus
  probes, at-least-once delivery, `needs_me` when a step can't be retried.
- **E5** policy stays in the Go class tables and `gate.go`'s classifier.
- **E6** approval binds to the plan digest and expires after 1 hour; the
  level, budget and preconditions are rechecked before every step.
- **E7** Pulumi applies use `--save-plan`/`--plan`, with a re-preview
  fallback noted in the AUTO note.
- **E8** v1 is merges and IaC applies; deploy-service provisions, messages
  and other connectors come in P3; the browser stays advisory.
