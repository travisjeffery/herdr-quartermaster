# Read-only acceptance verification with pi

The pi verifier is an **opt-in, filesystem-only** alternative to the default
Claude verifier. Worker/coordinator agent selection does not select it, and
installing this code does not change live configuration or close any task.
Checked against pi 1.5.2.

## Configuration

After separately authorizing installation and verifier selection, configure:

```toml
[verifier]
backend = "pi"
# model = "provider/model" # optional; otherwise pi's configured default
max_rounds = 2
minutes = 15
```

Use an already configured, authorized model. Providers implemented by extensions
are unavailable in this backend. Missing pi, missing credentials, unknown models
or unsupported CLI flags fail closed; there is no Claude fallback. This adapter
does not install pi, sign in, change a provider, or change agent settings.

`command` is the legacy arbitrary-command override and cannot be combined with
`backend = "pi"`. Arbitrary worker/coordinator CLI arguments are never inherited.
An unknown backend or a model on the legacy backend is an error.

## Tool boundary

The fixed command is:

```text
pi --mode json --no-session --offline
   --no-extensions --no-mcp --no-skills --no-prompt-templates
   --no-themes --no-context-files --no-approve
   --tools read,grep,find,ls --system-prompt <verifier instructions>
```

An optional `--model` is a single argv value, not shell text. The acceptance
prompt goes to stdin. It contains criteria and evidence pointers, not the
worker's close reason or ordinary notes. No worker session is continued.

The boundary is the available tool implementations, **not a read-only prompt**:

- No shell, PowerShell, edit, write, subagent, tool search or generated-code tool.
- No discovered, configured or built-in extension, including the worker control
  extension. No MCP connection or hidden/deferred MCP tool. `--tools` alone
  would **not** remove MCP reachability in pi.
- Project trust is declined for this invocation; context files, skills, themes
  and prompt-template discovery are disabled. No persistent session is written.
- Inherited worker/session identity and ripgrep command configuration are
  removed. The verifier is attributed as `BEADS_ACTOR=verifier`.
- Each run has the existing timeout and bounded stdout/stderr; cancellation
  terminates the pi process group, including search helpers. The adapter
  refuses an observed tool name outside its fixed read set.

This is **not an OS sandbox or a confidentiality boundary**. The pi executable,
its built-in tools, and operator-controlled model/auth configuration are trusted.
The model can read files the OS user can read, including outside the repository;
do not use this adapter for untrusted tasks with access to sensitive files.
Normal pi credential refresh/cache bookkeeping is not an agent write capability.
No tool is provided to execute a test, query GitHub, inspect a cluster, or fetch a
URL. If direct live files cannot establish a criterion, the verifier must fail
it as uncheckable. Worker-produced logs are claims, not proof of current state.
Future remote evidence adapters require their own narrowly reviewed boundary;
adding bash or an arbitrary MCP server is not a supported workaround.

## Verdicts and retries

Pi's JSON-mode exit code is not enough: provider failures can exit zero.
Quartermaster accepts a verdict only from the last completed assistant message
followed by `agent_settled` with `aborted: false`. Thinking, tool results, stream
deltas and earlier replies never become verdicts. The final text must end in a
`VERDICT` JSON line. A pass also requires a successful read-tool result.

The result uses the existing per-criterion pass/evidence representation. Every
supplied criterion must be present, in order, with the same text and nonempty
evidence. Without an Acceptance section, two to four derived criteria are
required. Missing, malformed, incomplete or out-of-boundary output cannot close
a task. Malformed output consumes a failed round, like the legacy parser;
launch, timeout and provider failures spend no round. Oversized evidence fails
instead of creating an unreadable journal entry. The default two failed
rounds still escalate to `needs_me`.

The ordinary journal is authoritative, not a worker's `VERIFY:` note or close
reason. A pi journal entry additionally binds its cached pass to the backend,
model and acceptance prompt. An old legacy pass or changed prompt cannot skip a
new pi verification. Same-scope passes retain the one-hour retry window; failed
rounds are not reset by changing backends.

## Tests and isolated trial

```sh
GOMAXPROCS=2 go test -p 2 -run 'Test(PiVerifier|ParsePi|ValidatePi|RunPiVerifier|VerifierOutput)' .
python3 scripts/pi-verifier-trial.py --model PROVIDER/MODEL
```

The trial builds a scratch binary and runs the real pi process using existing
authentication. All beads, configuration, journal and close effects are fixtures
under a fresh private temporary directory. It checks a live-file pass, an unmet
criterion and two-round escalation, forbidden project resources, fixed launch
arguments and fresh process invocation. It retains evidence locally, never closes a live task,
and does not modify live verifier or model settings.

## Separately gated activation and rollback

1. Review and merge the adapter under the normal review/check policy.
2. Separately authorize installation and opt-in configuration. Record the
   previous binary revision and verifier configuration; install through the
   ordinary gated installation procedure.
3. Run the isolated trial, then inspect the selected real task's acceptance
   criteria and verify the closure gate. Only after explicit live-acceptance
   authorization, run `quartermaster close <task> --by worker --reason "..."`.
4. Read the genuine `verify` journal entry and task status. A failure stays open;
   fix the evidence or limitations, never supply a hand-written pass or use a
   manual close to claim the adapter worked.

To roll back, restore the previous verifier configuration/binary under the same
operational gate. If the task requires pi only, stop attempting closure instead
of letting the legacy default invoke Claude. Keep authentic journal records;
rollback is not permission to erase failures or manufacture passes.
