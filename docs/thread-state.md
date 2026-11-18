# Shared thread state

`ThreadState` in `threadstate.go` is the read-only projection of a bead, agent,
selected PR and its checks. It is not a lifecycle transition or permission to
merge. `threadFor` gathers an observation and freezes its derived state; a
changed observation (prompt evidence, dependencies, or a successful resume)
is derived again before rendering. Raw test observations can derive on demand.

## Precedence

First matching row wins. The active-thread order preserves the sidebar's
existing behavior, including ready-to-merge ahead of optional check failures.

| Observation | State |
| --- | --- |
| Agent blocked at a prompt | Waiting on prompt |
| `needs_me`, without an active answered/asking turn | Waiting on command if a current `RUN:` exists, otherwise user |
| Open, unstarted bead with live blocking dependencies | Blocked |
| Open, unstarted bead | Queued (next or backlog) |
| Selected PR merged | Merged, with a rolling-out facet |
| Merge-policy readiness passes | Delivering, ready |
| Open PR with failing checks | Delivering, failing |
| Working agent | Working |
| Open PR with idle/done agent | Delivering, review |
| Explicit blocked status | Blocked |
| Any remaining present agent | Idle |
| No agent | NoAgent |

An open bead's idle agent does not start work. A working or blocked agent does
surface in the board instead of hiding in its backlog. Unknown agent status is
not proof that the agent is absent; dead-process reconciliation is separate.

A known background wake does not answer `needs_me`. The ticker's existing
transcript check supplies `Unprompted`; the snapshot carries that evidence to
other views for the same question, agent status and sequence. The active turn
that asks a question (or a working agent without that evidence) still renders
as working until it settles. Nothing in the projection updates bead status.

## Views

- `classify` maps the shared state to the existing sidebar/board group names.
- The digest maps it to broader sections, with separate merged and rolling-out
  sections. An open PR without an agent does not claim somebody is reviewing it.
- The report shows every active non-waiting thread in flight, including a merged
  PR awaiting close and work with no PR. Waiting threads appear in Needs you,
  not in two contradictory sections. Closed beads remain in Shipped.
- Detail uses its row's state, not a second classification from its slower
  full-bead lookup. That lookup only supplies description and notes.
- Context keeps summarizing no-agent threads, using the same group as the
  other views.

Different collection times or missing evidence can still give different
observations. In particular, digest rollups cannot invent a merge approval:
full gate evidence is retained only when a fresh observation has the same
nonempty head SHA as the ticker's selected PR.

## PR selection and age

`selectPR` is the only ranking function. Candidate sources keep own/current
branch first, then latest noted links first. Open beats merged beats closed or
unknown; among merged PRs, the latest merge wins; other ties keep source order.
Duplicate URLs count once. Selection preserves the entire PR, including gate
and local policy metadata. Cached views consume the ticker's already-selected
PR; a failed detailed fetch must not promote a light noted lookup into a
zero-review lifecycle baseline. A known PR belonging to another thread is a
reference, not this thread's delivery. Detail still exposes reference links.

Display staleness is a `ThreadState` property: waiting over four hours, active
work/delivery over one day, or blocked over seven days since the bead update.
Queued work and observations with no timestamp are not called stale. This is
not the lifecycle policy for releasing claims, verifier evidence, or expiring
an autonomy lease; none of those policies changes here.

`TestThreadStateMatrixAndViews` enumerates bead status × agent status × PR
state/readiness × rolling-out, and checks the sidebar, context, board, detail,
digest and report against that one table. Additional tests cover commands,
background evidence, dependency blocks and selection without losing metadata.
