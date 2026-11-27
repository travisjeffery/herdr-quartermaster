# Read-only live verification (`[verifier]`)

`quartermaster close` runs an independent read-only verifier before closing
beads that have post-merge rollout steps (`rolling-out` or Actions runs noted)
or no authored PR.

By default, the verifier runs in **strict** mode (`mode = "strict"`): it checks
local repository files, `git`, `gh`, `bd` (`BD_READONLY=1`), and `kubectl
get`/`describe`, while blocking cloud CLIs, HTTP requests, and MCP tools. That
default never weakens automatically.

When beads verify live infrastructure, deployments, HTTP endpoints, or
observability metrics/logs, you can explicitly opt into **`mode =
"live-readonly"`**. This enables narrowly allowlisted read-only live inspection
while **failing closed** on all file writes, mutating CLI subcommands,
non-`GET`/`HEAD` HTTP methods, and credential/secret reads.

---

## Supported backends

| Backend | Strict mode (default) | `mode = "live-readonly"` | Notes |
|---|---|---|---|
| `claude` (default) | Local files, `git`, `gh`, `bd`, `kubectl get/describe`, `--permission-mode dontAsk`, `--strict-mcp-config` | Adds validated `--allowedTools`, `--disallowedTools` (writes, secrets, mutations), optional `--mcp-config` | Uses Claude Code tool allow/deny rules |
| `agy` | Headless `agy --sandbox` with read-only OS namespace sandbox, `write_file(*)` / `mcp(*)` / `read_url(*)` / live CLIs denied | Same read-only OS sandbox plus word-by-word RE2 `command(regex:...)` rules, read-only `curl` GET/HEAD, explicit `read_url` domains, and explicit read-only `mcp(server/tool)` allowlist | Enforces `Deny > Ask > Allow` on top of Linux/macOS OS sandbox |
| `pi` | Filesystem-only (`read`, `grep`, `find`, `ls`); no shell, network, or MCP | **Rejected** at config load time | See [docs/pi-verifier.md](pi-verifier.md) |
| Custom `command = [...]` | Runs arbitrary verifier command | **Rejected** when combined with `live-readonly` or policy fields | Use `backend = "claude"` or `backend = "agy"` so Quartermaster enforces the read-only boundary |

---

## Opt-in configuration (`config.toml`)

All `[verifier]` settings are validated when `quartermaster` loads
`config.toml`. Setting any broadening field (`allow_commands`, `allow_http`,
`allow_domains`, `allow_mcp`, `mcp_config`) without `mode = "live-readonly"`
fails config validation immediately.

### Example: `agy` backend with read-only live checks

```toml
[verifier]
backend = "agy"
mode = "live-readonly"
model = "gemini-4-neon-xhigh"   # optional; omit for agy's default model
max_rounds = 2
minutes = 15

# Validated read-only CLI prefixes or constrained read-only verb wildcards.
allow_commands = [
  "aws --profile viewer * describe-*",
  "aws --profile viewer * list-*",
  "aws --profile viewer * get-*",
  "aws --profile viewer s3 ls",
  "cf --profile personal * get",
  "cf --profile personal * list",
  "deployer get environments",
  "deployer get service-deployments",
  "deployer get cloud-deployments",
  "kubectl logs",
  "kubectl top",
  "pulumi stack ls",
  "pulumi stack history",
  "openssl s_client",
  "dig",
]

# Strictly read-only HTTP GET/HEAD checks via curl.
allow_http = true

# Optional: pin read_url, sandbox outbound access, and curl targets to explicit domains.
# Wildcards such as "*" or "*.example.com" are rejected.
allow_domains = [
  "example.com",
  "status.example.org",
]

# Explicit read-only MCP tools ("server/tool" only; wildcards are rejected).
allow_mcp = [
  "grafana/query_prometheus",
  "grafana/query_loki_logs",
  "victorialogs/query",
  "victoriametrics/query",
  "victoriametrics/alerts",
  "victoriatraces/traces",
  "kubernetes/pods_list",
  "kubernetes/pods_log",
  "kubernetes/events_list",
]

# Optional extra explicit denies layered on top of immutable built-in denies.
deny_commands = []
deny_mcp = [
  "slack/*",
]
deny_paths = [
  "~/.config/example-tokens",
]
```

### Example: `claude` backend with read-only live checks

When using `allow_mcp` with `backend = "claude"`, provide `mcp_config` because
Quartermaster always passes `--strict-mcp-config`.

```toml
[verifier]
backend = "claude"
mode = "live-readonly"
max_rounds = 2
minutes = 15

allow_commands = [
  "aws --profile viewer * describe-*",
  "aws --profile viewer * list-*",
  "kubectl logs",
  "dig",
]
allow_http = true
allow_domains = ["example.com"]
allow_mcp = ["victorialogs/query", "victoriametrics/query"]
mcp_config = "~/.config/quartermaster/verifier-mcp.json"
```

---

## Fail-closed security boundary

Quartermaster enforces read-only boundaries at **both** config-load validation
and verifier invocation time:

1. **No silent weakening of defaults.** Omitting `mode` (or setting
   `mode = "strict"`) keeps the strict boundary unchanged.
2. **File writes are always denied.**
   - `agy`: `write_file(*)`, `execute_url(*)`, `agentMode = ""`,
     `toolPermission = "proceed-in-sandbox"`, and `--sandbox`
     (`enableTerminalSandbox = true`), mounting the sandbox filesystem
     read-only.
   - `claude`: `--tools` excludes `Edit`/`Write`/`NotebookEdit` and
     `--disallowedTools` denies them explicitly alongside
     `--permission-mode dontAsk`.
3. **Credential and secret reads are always denied.**
   - Built-in sensitive paths under `$HOME` (`~/.ssh`, `~/.aws`,
     `~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.kube/config`,
     `~/.docker/config.json`, `~/.npmrc`, `~/.pypirc`,
     `~/.pulumi/credentials.json`, `~/.config/gcloud`, `~/.azure`,
     `~/.config/cloudflare`, `~/.cloudflare`, agent OAuth/credential files,
     and `~/.local/state/quartermaster/tap/secret`) plus any `deny_paths` are
     denied to file-reading tools and matched by a shell-argument path deny
     rule.
   - Secret-exposing CLI operations and flags are rejected in `allow_commands`
     and explicitly denied at runtime (`kubectl` Secrets, `aws secretsmanager`,
     `aws ssm`, `aws kms`, `aws sts`, `aws ecr get-login-password`,
     `aws s3api get-object`, `deployer get deployment-secrets`,
     `deployer get otel-headers`, `pulumi stack export`, `--show-secrets`,
     `--with-decryption`, `--cli-input-json`, `file://`).
   - Every verifier subprocess runs with `BEADS_ACTOR=verifier`,
     `BD_READONLY=1`, and worker/nested-agent session environment variables
     scrubbed.
4. **`allow_commands` validation is fail-closed.**
   - Rejects shells and interpreters (`sh`, `bash`, `python3`, `node`, `sudo`,
     `env`, `xargs`, `find`, etc.), IaC/container mutators (`terraform`,
     `tofu`, `docker`, `podman`), file mutators (`rm`, `mv`, `cp`, `tee`,
     etc.), `gh api`, and direct HTTP clients (`curl`/`wget`, which must go
     through `allow_http = true`).
   - Rejects shell metacharacters (`;`, `|`, `&`, `>`, `<`, `` ` ``, `$`,
     newlines) and raw `regex:` strings.
   - Requires an explicit read-only verb (or constrained `describe-*`,
     `list-*`, `get-*`, `head-*` verb wildcard). Bare CLIs and trailing `*`
     (such as `aws --profile viewer *`) are rejected.
   - On `agy`, each rule compiles to word-by-word `command(...)` /
     `command(regex:...)` rules paired with positional surgical per-CLI
     mutation/secret deny rules and duplicate-`--profile` override protection.
     Any unconfigured live CLI receives a coarse `command(<cli>)` deny rule so
     accidental attempts return a clean recoverable tool error instead of
     terminating headless execution.
5. **`allow_http` is strictly `GET`/`HEAD` only.**
   - Allows only `http://` and `https://` `curl` `GET`/`HEAD` checks (scoped
     to `allow_domains` when set).
   - Denies non-`GET`/`HEAD` methods (`-X POST/PUT/PATCH/DELETE`), request
     bodies/uploads (`-d`, `--data*`, `--json`, `-F`, `-T`), local file output
     (`-o`, `-O`, `-D`, `-c`, including bundled short flags like `-sSo`),
     credential/config/`@file` flags (`-K`, `-b`, `--netrc`, `-E`, `--key`,
     `-u`, `-H @file`), non-HTTP schemes (`file://`, `ftp://`, `gopher://`),
     and cloud instance metadata endpoints (`169.254.169.254`,
     `169.254.170.2`, `metadata.google.internal`).
6. **`allow_mcp` requires explicit read-only `server/tool` names.**
   - Wildcards (`*`, `server/*`) and mutating or secret-access tool names are
     rejected at config load time.
   - When `allow_mcp` is empty, `agy` denies `mcp(*)` outright. When
     `allow_mcp` is set, only those exact tools are allowlisted and unlisted
     MCP tools remain denied in headless mode.
7. **Journal pass caching is bound to policy.**
   - Cached passes for `backend = "agy"` or `mode = "live-readonly"` include a
     `verifier_context` digest covering backend, mode, model, compiled policy,
     and prompt, so changing verifier policy or switching modes never reuses a
     pass from an older boundary.

---

## Policy-blocked vs evidence-failure diagnostics

Verifiers report structured per-criterion status in the terminal `VERDICT` JSON:

```json
VERDICT {"verdict":"fail","derived":false,"criteria":[{"criterion":"Live endpoint returns 200","pass":false,"status":"policy_blocked","blocked_by":"command(curl)","evidence":"blocked by verifier policy"}]}
```

- `"status": "pass"` (`"pass": true`): confirmed from live evidence.
- `"status": "fail"` (`"pass": false`): live evidence shows the criterion is
  not satisfied (for example, an unhealthy pod, HTTP 503, or target-system IAM
  403).
- `"status": "policy_blocked"` (`"pass": false`, optional `"blocked_by"`): the
  verifier could not inspect live state because verifier policy or sandbox
  rules blocked a command, MCP tool, or network call.

`policy_blocked` **always fails closed** (a bead never closes on a policy
block), and contradictory verdicts (`pass: true` with `status: "policy_blocked"`
or non-empty `blocked_by`) are rejected.

Quartermaster surfaces the distinction clearly in stdout, `VERIFY: fail` bead
notes, and `needs_me` escalations:

- **Pure policy block:** tagged `(policy-blocked)` with
  `[policy-blocked: <target>]` on each affected criterion and a note confirming
  no acceptance criteria failed on live evidence.
- **Mixed failure:** tagged `(evidence+policy-blocked)` so workers fix real
  evidence failures while operators see which checks were policy-blocked.
- **Process-level headless permission auto-deny:** stderr permission denials
  from headless verifiers (for example, `tool required "command" permission`)
  are automatically classified as `[policy-blocked: ...]` instead of appearing
  as an opaque malformed-output bug.
- **Genuine evidence failure:** unchanged `✗ <criterion> — <evidence>` output;
  target-system errors such as an application HTTP 403 or IAM `AccessDenied`
  are never misclassified as verifier policy blocks.

---

## Rollback

To return to strict mode at any time, set `mode = "strict"` (or remove
`mode` and the `allow_*` keys from `[verifier]` in
`~/.config/quartermaster/config.toml`). No ticker restart is required for
`quartermaster close` to pick up the updated config.
