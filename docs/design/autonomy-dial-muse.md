# Autonomy dial: one 0–9 setting for how far the system acts without the operator

Status: **draft, independent design** (2026-11-08). Written from first
principles against the code as it stands (`autonomy.go`, `gate.go`,
`prompt.go`, `dispatch.go`, `config.go`, the coordinator guide in
`context.go`, [autonomous-mode.md](autonomous-mode.md)). The parallel dial
draft was deliberately not read, so disagreements between the two are real
signal, not editing drift. No implementation until the operator picks the
decisions at the end.

## Summary

Today autonomy is a boolean per bead: the `autonomous` label (minus
`supervised`, minus the global stop file, minus per-action config switches)
plus prose rules for the coordinator. This replaces the boolean with one
number, 0–9, that answers a single question for every action by every actor
— worker, coordinator, ticker:

> **How far from the operator may this action's effect land before a human
> must consent?**

Every action gets a **class** from where its effect lands (its destination),
how hard the effect is to undo, how much of it there is, and whether it
crosses a trust boundary. An actor may perform an action without asking iff
`class(action) ≤ effective level`. The level is set globally, overridable
per bead, capped per repo and per host. 9 is allow-all quartermaster can
allow; 0 is advise-only.

Three positions this doc takes that cut against the requested framing are
argued in "Where I disagree with the framing" and summarised up front so
they are not buried:

1. **Destination alone does not order actions.** Merging an approved, green
   PR and merging an unreviewed one write to the same place. What separates
   them is preconditions and evidence. The dial therefore grants
   *conditional* authority: a class is only reached when its named
   preconditions hold, and an action missing them classifies higher.
2. **"Nothing outside the dial" is not achievable, and pretending otherwise
   is dangerous.** Harness hard denies, hosting rulesets, cloud IAM and the
   absence of credentials on a host all sit outside quartermaster and cap
   what 9 can actually do. 9 means "quartermaster consents to everything";
   the doc names the external ceilings instead of hiding them inside the
   number.
3. **A scalar is a lossy compression of at least four axes** (blast radius,
   reversibility, audience, policy-changing-ness). Keep the one dial the
   operator asked for as the commitment device, but implement it as
   `class = max(sub-scores)` with named caps, leases for the top levels, and
   rate budgets — or level 6 will quietly mean different things to
   different agents within a month.

## First principles

### Autonomy is delegated consent, not capability

The agents can already run the commands. Nothing about the dial makes a
worker more able; it changes **who must consent, on what evidence, before
the effect happens**. That framing decides most design questions:

- The unit of policy is not a tool call but an **effect**: an action, at a
  destination, of some size, with some undo path. `gh pr merge` is not a
  class; "merge this approved PR into a trunk that deploys on merge" is.
- Consent can be given in advance (the dial), in the moment (a prompt the
  operator answers), or by proxy under conditions (the coordinator answers
  because the dial already consented to that class). Proxy consent is not a
  separate power — but see the disagreement about escalations below.
- Because consent is what's being delegated, the audit question is never
  "what ran?" but "**under what consent, granted when, at what level, on
  what evidence?**" Every auto action must be able to answer that after the
  fact, or the dial is just unlogged trust.

### Asking is a cost; so is not asking

A stop costs the operator's attention and the thread's latency. A wrong
auto action costs undo work, or is un-undoable. The dial exists because the
operator would rather pre-pay the second cost for small effects than keep
paying the first. Two consequences:

- The interesting boundary is not "safe vs unsafe" but "**cheap to undo vs
  expensive to undo**", which is why reversibility is a first-class input
  to classification and not a footnote.
- Interruption volume should fall as the level rises, but **record volume
  must rise**. The logging-verbosity analogy is inverted in exactly this
  way: at 9 the operator is interrupted only by catastrophes, yet the
  journal should be at its most detailed, because review-after-the-fact is
  the only review left.

### Enforcement beats instruction

A rule in a brief is a request to a language model. The current system
already knows this: labels are the grant, `autonomy check` is the truth,
`gate plan` parses the plan instead of trusting the worker's summary of it,
and `answer` re-reads the screen and compares hashes instead of trusting
that the prompt is still the prompt. The dial should push policy further
into the same places — the `quartermaster` binary, harness launch modes,
hooks, and credential availability — and leave as little as possible to
prose an agent can talk itself out of under prompt injection.

### Lowering is always allowed; raising is the operator's

Any actor may lower its own effective level, refuse an action, or stop.
Only the operator may raise a level. This asymmetry is the main defence
against a steered agent widening its own authority: a worker that wants
more rope must produce a `needs_me`, which is the one thing it cannot
forge into a yes.

## The ladder: classes by destination

Classes are cumulative rings of effect, from "nothing leaves the agent's
head" to "nothing is out of bounds". A level *N* authorises every action of
class ≤ *N*, subject to the preconditions in the table. Names matter more
than numbers; the numbers are for the CLI.

| Class | Ring | What lands there (this system's real actions) | Preconditions |
|---|---|---|---|
| 0 | **Observe** | Read code, logs, metrics, bead/PR/tracker state; produce plans and recommendations. Coordinator reports status. | Writes limited to notes on the actor's own bead (reporting is infrastructure, not an effect). |
| 1 | **Draft** | Edit own worktree, run local builds/tests, commit to own branch, scratch files, own-bead notes and labels. | Inside the bead's worktree. Discardable by deleting the worktree. |
| 2 | **Publish own work** | Push own branch (including force-push of own branch), open/update own PR, edit own PR description, comment and post trigger comments (`/plan`-style) on own PRs, take decisions whose whole effect is inside own PR. | Branch/PR belong to this bead. |
| 3 | **Operate locally** | Dispatch or resume a worker on an operator-selected bead; create/claim/close beads; close a stale bead on strong evidence; resolve a merged bead's worktree; delete own merged local branches; install toolchains; write outside the worktree on the operator's machine; self-install quartermaster (pull, build, restart ticker). | Dispatch only for operator-selected (`next` or named) beads; resolve without `--force` unless the work is pushed. |
| 4 | **Land** | Merge own PR (auto-merge, or `--match-head-commit` once reported ready); retarget and reopen stacked PRs; resolve review threads on own PR after fixing or replying; close a superseded PR; other writes to shared repo state. | Required human review present and required checks green. **If the repo deploys on merge, the merge takes the class of that deploy instead.** |
| 5 | **Speak internally** | Post to internal team channels; comment on other people's PRs and issues; move other tracker issues; assign work to people. | Internal audience only. |
| 6 | **Change non-prod** | Apply/deploy to dev or staging; delete or replace non-prod resources; mutate non-prod data; rotate non-prod credentials; dispatch work the operator did not select (choosing the queue is an operator-attention effect, hence here, not at 3). | Target mechanically recognised as non-prod (see Classification). |
| 7 | **Change prod reversibly** | Prod apply whose plan is create/update only; prod deploy and canary advance after the previous step verified; reversible prod config and flag changes. | Machine-readable plan passes the plan gate, matches the change noted beforehand, and a rollback is named. |
| 8 | **Break glass / cross a trust boundary** | Scoped prod delete or stateful replace; revoke credentials; read or write secrets; message people outside the team; approve a PR; merge with admin bypass; force-push or delete a shared branch; spend money; answer a classifier-denial prompt; grant standing permission or edit harness settings/hooks/gates. | Announce-before (note + notification naming target and undo plan, or its absence) at any level below 9. |
| 9 | **Unbounded** | Widespread prod deletion; irreversible operations (key deletion, WORM/object-lock, public disclosure); unbudgeted spend; approving one's own PR or arranging bot approval; raising one's own level; disabling gates or audit. | None quartermaster can impose. External ceilings still apply (below). |

Notes on placements that are easy to get wrong:

- **Approving a PR is class 8, not 4.** Its bytes land in the same repo as a
  merge, but its *effect* is removing the human-review gate that class 4's
  precondition depends on. Actions whose effect is "another check no longer
  applies" classify by the boundary they dissolve. Approving your *own* PR,
  or arranging a bot to do it, is 9: separation of duties is gone entirely.
- **Reading a secret is class 8.** The destination is the agent's context,
  its transcript, tool logs and a model provider — a disclosure surface,
  not a null effect. (Trusted providers are still a wider audience than
  the secret's store.)
- **Deleting unpublished work bumps.** Removing a clean, pushed worktree is
  3. Force-removing one holding unpushed commits destroys the only copy;
  irreversibility moves it up a ring (see the bump rules).
- **Self-install stays low (3)** for the reasons the accepted design gives:
  local pull/build/restart, undone by rebuilding the previous commit. It is
  a useful reminder that "deploy" is not a class — destinations are.
- **Answering a prompt is not a class.** It inherits the class of the
  command it releases, with two bumps below. The coordinator answering for
  a worker and the worker acting itself are the same consent question —
  with the escalation caveat in the disagreements section.

## Classification: how an action gets its class

Classification is where this design succeeds or fails; the ladder is just a
table. `class(action)` is the **maximum** of four sub-scores, computed in
this order of authority: mechanical classifiers first, then the acting
agent's declaration, then the coordinator's reading. Any disagreement
resolves upward. Unknown resolves upward. There is no path by which an
agent's say-so lowers a mechanically derived class.

1. **Destination ring** (the table). Where does the effect land, in whose
   namespace, visible to whom? Own worktree < own branch/PR < operator's
   machine and bead state < shared trunk < team audience < non-prod <
   prod.
2. **Reversibility bump.** Undo is a command away: +0. Undo needs a restore
   or a person's intervention: +1. No undo (only copy destroyed, message
   already read, key deletion scheduled): the action is at least 8, and 9
   at any real scale.
3. **Scope bump.** Count matters: one resource is a different action from
   four hundred. Mechanical counters (plan step counts, files, beads,
   recipients) bump +1 past configurable thresholds, and "widespread" is
   defined as a number in config, not as a vibe. Sequences aggregate by
   bead + target + rolling window, so splitting one big delete into
   twenty small ones (`salami slicing`) sums back up.
4. **Trust-boundary bump.** The action removes or satisfies another gate
   (approving, granting standing permission, editing settings/hooks,
   answering a classifier denial), or crosses to an outside audience, or
   moves a secret into a transcript: at least 8.

**Preconditions are part of the class, not an alternative to it.** "Merge"
is class 4 *with* review and green checks; without them it is not a
cheaper merge, it is class 8 (bypass). "Prod apply" is class 7 *with* a
passing plan gate, an expectation noted beforehand, and a named rollback;
an unplanned console change to prod has none of those and classifies 8+.
This is the single most important sentence in the design: **the dial
authorises action-plus-evidence, never action alone.**

### Mechanical classifiers (build these; trust nothing else as far)

- **Plan gate → class emitter.** `quartermaster gate plan` already parses
  Pulumi and OpenTofu previews and refuses deletes, replaces, unknown ops
  and unparseable plans. Generalise it from pass/fail to emitting a class:
  create/update/read-only ⇒ 7 if the target is prod, 6 if not; any delete
  or replace ⇒ 8; stateful delete/replace, or delete/replace count past
  the widespread threshold ⇒ 9; unparseable or errored ⇒ refuses to
  classify, which means stop. `--expect` stays the mechanical form of "the
  change the worker said it expected"; a mismatch refuses classification.
- **Prod recognition.** Per-repo target patterns in config (stack names,
  account ids, clusters, namespaces, hostnames) matched against
  `--target`/stack/cluster arguments and plan metadata. **Unmatched means
  prod.** The failure mode of a missing pattern must be asking, never
  assuming staging.
- **Merge class.** Per repo: does merging deploy, and to what? `merge_class`
  in repo config (default 4; 7 for deploy-on-merge to prod). The ticker
  already knows review and check state; "ready to merge" is the
  machine-checked form of class 4's precondition.
- **Dangerous-verb table** for shell commands, used by hooks and by the
  coordinator when it reads a prompt: `pulumi up/destroy`, `tofu
  apply/destroy`, `kubectl delete/apply`, `aws … delete|terminate|revoke`,
  `gh pr merge|review --approve`, `git push --force` to a shared ref,
  package publishes, `curl` to a chat/email API, and so on. This table
  will always be incomplete; it exists to catch the known-sharp verbs
  mechanically, not to certify everything else class 1.
- **Path test.** Writes under the bead's worktree are candidates for class
  1; the same write elsewhere on the machine is class 3. Path checks are
  cheap and hard to argue with.

### Declared classification

Everything with no mechanical classifier is declared by the actor at check
time: `quartermaster autonomy check <bead> --class 5 --what "post rollout
summary to team channel"`. The declaration, the evidence for its
preconditions, and the effective level are written into the `AUTO:` record
(structured, below) *before* the action for class ≥ 6, so a crash mid-act
still leaves intent on record. A worker's `CRITICAL:` tag on a `needs_me`
is a classification claim of ≥ 8 and is honoured as one: neither the
coordinator nor a later worker reclassifies it down on prose alone; only a
mechanical classifier or the operator can.

**Unsure means stop**, exactly as today. An agent that cannot name the
destination, the undo path, or whether the target is prod does not have a
classification problem to solve creatively; it has a `needs_me` to file.

## Setting the dial

### Effective level

```
effective(bead) = clamp( bead level ?? global level,
                         0,
                         min(global max, repo cap, host cap) )
```

- **Global level** — the default for every bead and the coordinator's
  cross-bead actions. Set in config and changeable live (below).
- **Bead level** — an override, up or down, recorded as the label
  `autonomy:<n>` so it survives restarts and is visible on the board,
  replacing `autonomous`/`supervised` (migration below).
- **Global max** — a ceiling in config that no bead override exceeds.
  Raising a bead *above the global level* is allowed (per-bead opt-in is
  the pattern that already works), but only up to this ceiling, and
  raising the ceiling itself is a config edit: deliberately two steps,
  never a reflex.
- **Repo cap** — a repo whose trunk deploys to prod, or which holds
  especially sensitive code, can cap its beads regardless of global
  enthusiasm.
- **Host cap** — hosts without the binary, the stop file, or the
  credentials cap low. Today a remote host with no `quartermaster` fails
  `autonomy check` and therefore acts supervised; that fail-closed
  behaviour is a host cap of (in dial terms) 2 and should be made explicit
  rather than accidental.

Authority attaches to **action + bead**, not to actor. The coordinator
answering a prompt or a `needs_me` for bead X uses X's effective level; it
cannot do for X what X's worker may not do itself. Cross-bead coordinator
actions (dispatching a selected bead, board housekeeping) use the global
level. The ticker takes no action above the classes its existing
config-gated behaviours already imply (`auto_resolve` ≈ 3, `auto_close`
≈ 3), and under the dial those switches should be re-expressed as "the
ticker may do this iff global level ≥ 3" rather than left as independent
booleans that can contradict the dial.

### Leases at the top

Levels 8 and 9 as standing settings are where the risk actually lives:
exposure is probability × time. So:

- Setting any scope to 8 accepts `--for <duration>` and defaults to one
  (suggest 4h); on expiry it falls back to what it was before.
- **9 is lease-only and bead-scoped**: no global 9, no repo 9, no
  indefinite 9. `quartermaster autonomy level 9 <bead> --for 1h` is a
  deliberate, expiring act by the operator, noted on the bead and in the
  journal. If 9 must exist as true allow-all, this is how it exists
  without becoming the forgotten default.
- Expiry is enforced by the ticker (it relabels and notifies), so it does
  not depend on the worker noticing.

### CLI

```
quartermaster autonomy level                 # show global, max, caps, per-bead effective (the status view)
quartermaster autonomy level <n>             # set global (operator only; 8+ takes --for)
quartermaster autonomy level <n> <bead>...   # set bead override(s)
quartermaster autonomy level --clear <bead>  # drop override, back to global
quartermaster autonomy check <bead> --class <n> [--what ...] [--target ...]
#   exit 0 iff n ≤ effective(bead) right now; prints the arithmetic when it refuses
quartermaster autonomy check <bead> --action decide|merge|apply|install
#   legacy spelling during migration: maps to classes 2/4/6–7/3 via the classifiers
quartermaster autonomy off                   # panic: cap everything at 1 until `on`
```

`off` caps at **1, not 0**: a worker must still be able to write its own
notes, commit what it has, and stop cleanly; what it must not do is let
any effect leave its sandbox. `off` remains a state-dir file the operator
can create by hand (`touch`) if every agent is suspect — the brake must
not require an agent's cooperation, or a quartermaster release, to pull.

The board shows each thread's effective level (`auto7`, replacing `auto`),
`quartermaster context` prints the global dial and any leases with their
expiries in its header, and `autonomy level` with no setter is the one
place that shows the whole picture. A dial nobody can see will be at the
wrong setting.

## Enforcement: five layers, in order of strength

No single layer is trustworthy. The dial is the *policy input* to all of
them; each layer catches what the ones above it miss.

### 1. Capability, not consent (strongest)

An agent without prod credentials cannot do a class-7 action whatever the
dial says. Where the worker-hosts design already brokers credentials
per host, the broker is the natural enforcement point: issue the prod role
only when the bead's effective level reaches it, scoped and short-lived,
and never issue delete-capable prod credentials below 8. The same holds
locally in weaker form: credentials that only exist in the operator's
interactive session are a real ceiling. This layer is mostly outside
quartermaster, which is precisely why it works — and why "9 overrides
everything" is false advertising (see External ceilings).

### 2. Harness launch modes (pre-action, no screen-scraping)

The dial sets how each worker is *launched* (`agentArgs` in `config.go`,
used by dispatch, resume and the coordinator alike). Harnesses offer only
three or four distinguishable postures, so levels map to **bands**, not
one-to-one. Indicative mapping (exact flags are per-kind configuration, as
today's Muse default args already are):

| Band | Posture | Claude Code | Codex | Muse |
|---|---|---|---|---|
| 0 | Read-only / plan | plan mode | read-only sandbox | plan/read-only profile |
| 1–2 | Ask, but edits in worktree flow | default with worktree allow-rules | workspace-write, ask on escalation | `:auto-review` (today's default) |
| 3–5 | Judge decides routine, prompts on escalation | auto mode (classifier as judge) | workspace-write + auto-review | `:auto-review` |
| 6–7 | Judge decides, standing authorisation text names the dial | auto mode | as above, wider network | as above |
| 8 | Judge decides; coordinator may override its denials case-by-case (layer 4) | auto mode | as above | as above |
| 9 | Permission checks off, lease duration only | bypass permissions | full access / no approvals | bypass profile |

Two hard facts about this layer:

- **A running agent's mode does not change when the dial moves.** Raising a
  level mid-flight reaches the worker at its next `check` (layer 3) or
  next answered prompt (layer 4); its launch posture only changes on
  resume/restart. *Lowering* is worse: a worker launched at band 9 cannot
  be tightened by any message — it must be stopped and relaunched.
  `autonomy off` and any drop out of band 9 should therefore offer (and at
  9, perform) a pane restart, and the status view must show "launched at
  band 9, dial now 4" as the inconsistency it is. This revocation latency
  is the strongest argument for launching one band *below* what the dial
  allows and letting layers 3–4 supply the rest: prompts are revocable
  consent; bypass is not.
- **The coordinator is launched too**, at the band for the *global* level,
  and never above band 8: someone must remain who can be stopped by a
  prompt the operator sees. Its own prompts surface to the operator in
  its pane; nobody answers them by proxy. The recursion ends at a human.

### 3. The check (immediate, cross-harness, honour-system)

`autonomy check` before every named step remains the spine, extended from
`--action` to `--class`. It is honour-system — a worker can simply not run
it — but it is immediate (a revoked grant fails at the next step, as
today), identical across claude/codex/muse, and it produces the audit
record. Briefs keep the current contract: check fails or binary missing ⇒
supervised behaviour. The coordinator guide stops restating policy in
prose and instead says: *run the check; the check is the policy* — one
implementation, in Go, with tests, instead of three paraphrases in
prompts that drift.

### 4. Prompt answering by the coordinator (the revocable bridge)

Prompts still occur in every band below 9. Answering one is consent by
proxy, governed by the class of what it releases:

- **Ordinary permission prompts** (tool approvals, folder trust for a
  quartermaster-created worktree): class of the underlying command, judged
  from the command itself — `prompt.go` already treats screen text as
  agent-controlled description, never as evidence of safety. The
  coordinator answers iff class ≤ the bead's effective level. The existing
  `coordinator_rules` categories ("answer Go test runs", etc.) become
  rows in the dangerous-verb/class table instead of free prose, so the
  `answer` command itself can refuse an out-of-level answer unless it
  carries `--by <operator>`.
- **Classifier-denial prompts** (the auto-deny countdown): an independent
  judge has already said no. Overriding it is class 8 regardless of the
  underlying command — not because the command grew, but because the
  *evidence* changed: two judges now disagree, and the coordinator is
  being asked to side against the one that erred on caution. Below 8
  these stay `RUN:` items for the operator, as today.
- **Standing-permission options** ("don't ask again", mode switches):
  choosing one edits future policy, so it is class 8 even when the
  immediate command is class 1. The hash check in `answer` (refuse if the
  prompt changed) stays; it is the model for how proxy consent should
  behave everywhere.
- The ticker never answers prompts and never grants, as today. Automated
  messages approve nothing — under the dial this sentence means: no
  automated message raises a level or substitutes for a check.

### 5. Hooks on the dangerous verbs (last mechanical net)

A pre-tool hook in each harness (Claude Code hooks first; equivalents per
kind as they exist) matches the dangerous-verb table and, for a match,
either runs `autonomy check --class <derived>` synchronously or converts
the call to an ask. This is the only layer that can catch a worker that
skips layer 3 entirely. Keep the verb list short and sharp; a hook that
fires on everything will be disabled, and disabling hooks is class 8.

### Hard denies: the prompts that never appear

Some harness blocks show no prompt at all: settings deny rules, hooks
that exit with a block, sandbox refusals. There is nothing for the
coordinator to answer. Reaching past them means editing harness settings
or hooks, which is class 8 (policy change), takes effect at next launch,
and at band 9 is moot because nothing is checking. A worker hitting a
hard deny files a `RUN:` or a `needs_me`, exactly as for a classifier
denial today; the dial does not conjure a prompt where the harness
offers none.

### External ceilings (what 9 cannot do)

Independent of the dial: harness hard denies and non-bypassable product
limits; repository rulesets requiring human review (an agent approving
its own PR is class 9 *in quartermaster's consent model* and still
impossible or policy-violating at the host, which is the point of having
the ceiling outside); cloud IAM and credential absence; spending rails
that need a human-held payment method; other people's systems entirely.
The correct statement of 9 is: **quartermaster withholds nothing; the
world still might.** The dial governing consent while other systems
govern capability is defense in depth, not a contradiction — but the UI
must say so (`level 9 (lease 42m left; repo ruleset still requires human
review)`), or the operator will form a false model of what they just
authorised.

## Staying informed as the level rises

Interruption and record decouple:

- **Interrupts** (notifications, inbox `needs_you`): every out-of-level
  action, every failed precondition, every class-8 announce-before, every
  lease expiry, every circuit-breaker trip (below). At high levels this
  is a short list by construction — which is why everything on it must be
  real. A dial at 7 that still interrupts hourly will be ignored or
  cranked to 9; both are failure.
- **Digest**: `AUTO:` notes become structured —
  `AUTO: class=7 level=7 action=apply target=<stack> evidence="gate: 3
  update, expect ok" bead=<id>` — so the digest and `quartermaster
  report` can group by class descending: 7+ itemised with evidence, 4–6
  one line each, ≤3 as counts per bead. Today's digest already keys off
  `AUTO:` prefixes; structure makes it queryable instead of greppable.
- **Journal**: bead notes are writable by the very agents being audited.
  From level 6 up, the ticker should append every `AUTO:` record, prompt
  answer, level change and lease event it observes to an append-only
  journal file in the state dir, timestamped by the ticker (notes carry
  no timestamps today). Same-host caveat stated plainly: an agent at band
  9 on the same machine can rewrite that file; the journal raises the bar
  from "edit a note" to "forge a log", and real tamper-evidence wants the
  journal shipped off-box, which is out of scope here.
- **Silence must be distinguishable from death.** At any level the digest
  arrives on schedule even when empty ("nothing needed you; 14 actions
  taken, highest class 7"), and ticker staleness warnings already exist.
  Hands-off must never look identical to broken-off.

## Failure modes

- **Misclassification is the main risk**, ahead of malice. A staging-named
  stack that is really prod-adjacent, a merge that deploys, a "config
  tweak" that rotates a credential. Defences: mechanical classifiers over
  declarations; unmatched target ⇒ prod; disagreements resolve up;
  `CRITICAL:` claims honoured; and a standing audit habit the design
  should support mechanically — `report` re-derives classes for sampled
  past `AUTO:` records and flags under-declarations, so classification
  quality is measured, not assumed.
- **Prompt injection steering the coordinator.** The coordinator reads
  worker screens, PR text, issue text and logs — all attacker-influenceable
  in the weakest case, agent-generated in the normal one. A worker (or a
  poisoned PR comment it relays) that can phrase a prompt as routine can
  harvest proxy consent. Defences already present and kept: labels/checks
  are the grant, never a message; the ticker approves nothing; `answer`
  binds to a hash of the actual prompt. Added by the dial: classification
  from the command and target, not the description; `answer` refusing
  out-of-level choices in code; and the raise/lower asymmetry — the worst
  a steered coordinator can do is spend authority the operator already
  granted that bead, it cannot mint more.
- **The worker as its own classifier.** Declared classes are self-graded
  homework. Cap the damage: declarations above 5 require named evidence
  for preconditions; mechanical classifiers override; the coordinator may
  raise a class on reading, never lower it; sampled re-derivation in
  `report` makes systematic under-grading visible.
- **Runaway loops.** A worker retrying applies, a coordinator dispatching
  into a slot famine, a rollout advancing itself overnight. Budgets per
  bead per rolling hour (auto actions, applies, dispatches, spend where
  measurable) enforced in `check`: over budget, the check fails with
  "budget" as the reason and the bead needs a human. The ticker also
  trips a **circuit breaker**: N failed checks/applies or repeated
  identical `AUTO:` actions on one bead caps that bead at 1 and raises an
  inbox item, whoever set its level. Breakers only lower; restoring is the
  operator's, by the asymmetry rule.
- **Revocation latency** (layer 2): a band-9 pane does not hear the dial
  move. Mitigated by launching below the dial, leasing 9, and treating
  "launched band > current band" as a visible, restart-prompting state.
- **Level creep.** 7 is convenient; soon everything is 7 and the dial is
  decorative. Leases at 8–9, a monthly line in `report` (time-at-level per
  bead/repo, highest classes taken), and making *raise* slightly more
  frictionful than *grant for this bead* was before are the counterweights.
  Convenience is the threat model for defaults.
- **Two sources of truth.** `auto_close`, `auto_resolve`, `[autonomy]`
  per-action switches and the new dial must not coexist as independent
  booleans that can contradict. Migration folds them into the dial or
  deletes them; a config that says level 2 and `auto_close = true` is a
  bug farm.
- **Remote drift.** Stop file and config live on the control machine;
  remote workers see levels only through `check` reaching the binary or
  its proxy. Until that path exists on a host, its cap stays at 2 and the
  status view says why.
- **The coordinator is an agent too.** Its own classifier or hooks can
  stop it mid-answer; its context is built from bead text workers wrote.
  It gets no exemption from classification, no proxy to answer its
  prompts, and a launch band below the workers it supervises at the top
  end. A design where the supervisor is the least-checked actor has the
  arrow backwards.

## Where I disagree with the framing

1. **Destination-only grouping under-determines the order.** The clearest
   evidence is inside today's system: every worker, supervised or not, is
   briefed to enable auto-merge — a class-4 effect — while the coordinator
   must *propose and wait* before dispatching, a class-3 effect. By pure
   destination the merge should need more permission than the dispatch.
   It needs less, because GitHub enforces its preconditions (human review,
   green checks) externally. Precondition strength, not destination, is
   doing the work. Hence classes as action-plus-evidence. I have kept the
   destination ladder as the backbone, as asked, but a ladder that ignores
   *who else is checking* will mis-rank its two most common actions.
2. **"The coordinator answering is free whenever the underlying action is
   in level" discards information.** An escalation is evidence: a worker
   stopped because it was unsure, and "unsure means critical" is current
   policy. If every escalation within level is auto-answered by proxy,
   the system trains its workers that stopping is never load-bearing, and
   loses its best uncertainty signal. My version: proxy-answering inherits
   the underlying class, *but* a stop the worker tagged `CRITICAL:`, or
   one with no recommendation to ratify, is not the same action as the
   worker proceeding on its own recommendation — the coordinator is then
   the decider, must record its own evidence, and needs the class
   available at the bead's level with that evidence actually in hand.
3. **A single number invites false precision and false completeness.**
   False precision: 6 vs 7 will be litigated by agents at runtime; the
   harness bands underneath only distinguish ~4 postures anyway. False
   completeness: 9 as "nothing outside" hides the external ceilings and
   the self-reference problem — at 9 an agent may edit the dial, the
   gates and the audit, i.e. the top of the ladder is not another
   destination but control of the ladder. I have kept 0–9 (it is a good
   commitment device and the operator's explicit ask) but implemented
   underneath it as max-of-sub-scores with caps, leases and budgets, and
   named the ceilings in the UI. If forced to choose between "one honest
   number with machinery under it" and "one number that is the whole
   policy", the second will fail silently at exactly the levels where
   failure is expensive.

## Today, mapped onto the dial (a reality check)

Reading the current brief and guide as a dial is instructive, because the
system is already further up it than "supervised vs autonomous" suggests:

- An ordinary worker today: edits (1), pushes and opens PRs (2), enables
  auto-merge — a conditional merge, class 4 with GitHub holding the
  preconditions — posts trigger comments (2), and asks on decisions and
  anything sharper. **Effective today ≈ 4, with class 4 reachable only
  through GitHub's gate.**
- An `autonomous` bead today adds: own-recommendation decisions (2–5 by
  content), staged-rollout advance (6–7), direct merge paths (4), gate-plan
  applies **including prod create/update (7)**, self-install (3). **The
  honest equivalent of today's label is level 7.** Anyone who finds that
  number startlingly high has discovered something about the current
  grant, not about this ladder — it is an argument for naming levels, so
  that granting 7 is a conscious act.
- The coordinator today: answers a small allow-list of prompt categories
  named in `coordinator_rules` prose, resolves merged worktrees (3), and
  proposes-waits on dispatch (capped below 3 by rule). **Effective ≈ 3
  with a prose exception carved out** — exactly the
  kind of exception the dial should either ratify (dispatch of unselected
  beads ⇒ class 6, as this design does) or remove.
- `autonomy off`, `[autonomy] enabled=false`, per-action switches
  (`merge/apply/decide/install`) and `auto_close`/`auto_resolve` are five
  overlapping dials already. Consolidation is half the value even if the
  numbers never change.

## Migration

1. `autonomous` label → bead level **7** (today's behaviour, named).
   Grandfathered without a lease; the board showing `auto7` will start the
   right conversations on its own.
2. `supervised` label → bead level **2** (publish own work only). Note
   this *does* change one behaviour: enabling auto-merge is class 4, so a
   level-2 worker stops doing it — see D4, the one genuinely awkward
   mapping, since today every worker enables it.
3. Unlabelled beads → global level. Set the initial global to **4** to
   preserve today's auto-merge norm honestly, with global max **7** so
   bead overrides cannot exceed today's widest grant until the operator
   edits config deliberately.
4. `[autonomy]` per-action booleans are read once, translated (any false
   caps the corresponding class), then removed. `enabled=false` ⇒ global 0
   with a migration note. The stop file keeps working throughout: it caps
   at 1 under both regimes.
5. `--action decide|merge|apply|install` keeps working for one release as
   syntactic sugar over classes, because running workers' briefs already
   contain it. New briefs use `--class`.
6. `coordinator_rules` entries that allow prompt categories become rows in
   the class table; the rest stay as guide bullets.

## Implementation outline (after the decisions)

1. **Policy core**: class table + bumps, effective-level arithmetic,
   `autonomy level`/`check --class`, leases, budgets; migrate labels;
   status/board/context show levels. Briefs and the coordinator guide
   rewritten to point at the check. (Behaviour ≈ today, one source of
   truth.)
2. **Enforcement in the binary**: `answer` refuses out-of-level without
   `--by`; `dispatch`/`resume`/`resolve --force`/`stale --release`/
   verify-close consult the policy for coordinator-initiated calls;
   `gate plan` emits a class; prod patterns + `merge_class` in config.
3. **Launch bands**: per-kind args derived from effective level at
   dispatch/resume; launched-band recorded and surfaced; restart prompt
   when the dial drops below the launched band.
4. **Audit upgrades**: structured `AUTO:` records, ticker journal,
   digest/report by class, sampled re-derivation, time-at-level stats.
5. **Circuit breaker + hooks** on the dangerous verbs; credential-broker
   gating rides along with worker hosts whenever that lands.

Each phase is independently useful; none requires 8 or 9 to ever be
switched on. That is deliberate: the dial should earn its top levels by
being boring at its middle ones.

## Is ten levels too many?

For *enforcement*, yes — the harness bands distinguish about four
postures, and mechanical classifiers distinguish perhaps six. For
*consent*, no: the destination rings in this system's real action set
genuinely number about nine, and collapsing, say, "speak internally"
into "land on trunk" would conflate a social effect with a code effect
that fail very differently. The resolution used here — ten named classes
for classification and audit, four bands for launching — keeps the
operator's one dial while being honest that adjacent levels (1/2, 3/5
in quiet repos) will often move together in practice. Name the levels,
use the names in prose, keep the digit for the CLI and the board. The
alternative worth rejecting explicitly is named postures *instead of*
numbers (`supervised|trusted|hands-off`): fewer, vaguer, and they hide
exactly the destination distinctions the operator asked to see.

## Decisions for the operator

(R) = this design's recommendation.

- **D1. Ladder shape.** (R) The 0–9 destination ladder above, with
  class = max(destination, reversibility, scope, trust-boundary) and
  preconditions inside the class. Alternatives: destination-only ordering
  (preconditions as separate gates, as today); fewer levels with named
  postures.
- **D2. Bead above global?** (R) Yes, up to a config `max`; raising the max
  is a config edit. Alternative: global is a hard ceiling for everything.
- **D3. Top of the dial.** (R) 8 lease-by-default; 9 lease-only,
  bead-scoped, never global; external ceilings named in the UI.
  Alternatives: 8–9 as ordinary standing settings; a floor that even 9
  does not lift (rejected by the operator's stated requirement — noted
  here only because the self-reference problem is real: at 9 the dial
  itself is editable by its subjects).
- **D4. Migration values.** (R) `autonomous` ⇒ 7, `supervised` ⇒ 2,
  global default 4, initial max 7; `off` caps at 1. Alternatives:
  `autonomous` ⇒ 4 (a real reduction of today's grant — defensible, but
  say so out loud rather than achieving it by relabelling); global
  default 2 (workers stop enabling auto-merge unsupervised).
- **D5. Classifier-denial answering.** (R) Class 8, coordinator-only, in
  code — or the blunter alternative: never answered by proxy at any
  level below a 9-lease; the countdown judge outranking everyone except
  the operator is a coherent position and the cheapest one to implement.
- **D6. Launch posture vs dial.** (R) Launch one band below the effective
  level and bridge with checks/prompts (revocable), reserving true
  bypass for 9-leases. Alternative: launch at the dial's band (fewer
  prompts, worse revocation).
- **D7. Where policy lives.** (R) In the binary (check, answer, gate,
  launch args), with briefs and the coordinator guide reduced to pointers.
  Alternative: keep policy in the rendered guide, parameterised by level
  (faster to ship, drifts, and a steered agent is arguing with prose).

*End of draft. Pick per decision — "all R" is a valid answer — and this
becomes the accepted design; until then nothing above changes behaviour.*



