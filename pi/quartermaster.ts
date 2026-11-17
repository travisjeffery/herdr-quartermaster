// quartermaster's pi extension, loaded into every pi worker and coordinator
// quartermaster starts (`pi -e <state dir>/pi/quartermaster.ts`). The binary
// embeds this file and rewrites it there on each start; edit it in the
// quartermaster repo, not in place.
//
// pi asks before nothing: it runs every tool call with the user's
// permissions (pi's docs/security.md). A quartermaster worker runs
// unattended, so there is nobody at the pane to answer a prompt. This
// extension therefore blocks rather than asks, and the block's reason tells
// the worker to hand the step to the user the way it hands over a Claude Code
// denial: a RUN: note and needs_me. The one exception is a command the
// configuration lists as one to ask about (below).
//
// It does these things:
//   1. Runs the same PreToolUse bash hooks Claude Code workers run
//      (QUARTERMASTER_PI_BASH_HOOKS, else block-dangerous-bash.py and
//      rm-guard.sh under ~/.claude/hooks), with the JSON Claude Code hands
//      them. Exit 2 or a "deny" decision blocks with the hook's message. An
//      "ask" decision blocks too, unless the command matches the control
//      configuration's ask list and quartermaster is listening: then the
//      question goes to quartermaster (6). A hook that can't run fails open,
//      as the hooks themselves do.
//   2. Allows a subagent only when every agent it names is read-only by its
//      definition (tools: read, grep, find, ls). A pi subagent is a separate
//      pi process without this extension, so a broad one would run unguarded.
//   3. Records where each prompt came from as a "quartermaster.input" custom
//      entry (never sent to the model), so the ticker can tell a prompt from
//      pi waking itself (pisession.go). A prompt quartermaster delivers over
//      the control socket is recorded as "quartermaster".
//   4. Reports any extension dialog to herdr as blocked, so the ticker shows
//      the thread as waiting on the user.
//   5. After a compaction, puts `bd prime` back in context for a beads
//      worker (BEADS_ACTOR set), as Claude Code's PreCompact hook does.
//   6. With --quartermaster-control <socket>, which quartermaster passes a
//      worker, listens on that unix socket for quartermaster (picontrol.go):
//      prompts, delivered as user messages that queue behind a running turn,
//      and the pending ask, which quartermaster shows in its inbox and
//      answers. The socket and a token beside it (<socket>.token) are mode
//      600 in a 700 directory; a request must carry the token. Without the
//      flag, or if the socket can't be opened, the worker behaves as without
//      this: prompts come through the pane and guards block.
//
// @ts-nocheck

import { spawnSync } from "node:child_process";
import { randomBytes, timingSafeEqual } from "node:crypto";
import { chmodSync, existsSync, mkdirSync, readFileSync, unlinkSync, writeFileSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const home = os.homedir();

const handOff =
  "This needs the user. Do not retry it another way or work around this guard: " +
  "write the exact command in a RUN: note on your bead (`bd note <bead> \"RUN: <command>\"`) " +
  "with a one-line note of why, set the bead to needs_me (`bd update <bead> --status needs_me`), and stop.";

function bashHooks(): string[][] {
  const env = process.env.QUARTERMASTER_PI_BASH_HOOKS;
  if (env !== undefined) {
    return env
      .split(":")
      .filter((p) => p !== "")
      .map((p) => (p.endsWith(".py") ? ["python3", p] : ["bash", p]));
  }
  const dir = path.join(home, ".claude", "hooks");
  const hooks: string[][] = [];
  const py = path.join(dir, "block-dangerous-bash.py");
  if (existsSync(py)) hooks.push(["python3", py]);
  const rm = path.join(dir, "rm-guard.sh");
  if (existsSync(rm)) hooks.push(["bash", rm]);
  return hooks;
}

// runHook returns the hook's decision: "block" (exit 2 or deny) or "ask",
// with its reason, or undefined to let the call through.
function runHook(argv: string[], payload: string, cwd: string): { kind: "block" | "ask"; reason: string } | undefined {
  const r = spawnSync(argv[0], argv.slice(1), { input: payload, cwd, encoding: "utf8", timeout: 15000 });
  if (r.error) return undefined;
  if (r.status === 2) return { kind: "block", reason: (r.stderr || r.stdout || "blocked by " + path.basename(argv[1])).trim() };
  try {
    const out = JSON.parse((r.stdout || "").trim() || "{}");
    const h = out.hookSpecificOutput || {};
    if (h.permissionDecision === "deny") {
      return { kind: "block", reason: (h.permissionDecisionReason || path.basename(argv[1]) + " denied it").trim() };
    }
    if (h.permissionDecision === "ask") {
      return { kind: "ask", reason: (h.permissionDecisionReason || path.basename(argv[1]) + " would ask the user").trim() };
    }
  } catch {
    // Not JSON: no decision.
  }
  return undefined;
}

const readOnlyTools = new Set(["read", "grep", "find", "ls"]);

// agentTools is the tools: list of a pi agent definition, or undefined when
// it has none (the agent then gets every default tool) or can't be found.
function agentTools(name: string, cwd: string): string[] | undefined {
  if (!/^[A-Za-z0-9._-]+$/.test(name)) return undefined;
  for (const dir of [path.join(cwd, ".pi", "agents"), path.join(home, ".pi", "agent", "agents")]) {
    const file = path.join(dir, name + ".md");
    if (!existsSync(file)) continue;
    const text = readFileSync(file, "utf8");
    const front = text.startsWith("---") ? text.split(/^---\s*$/m)[1] || "" : "";
    const line = front.split("\n").find((l) => /^tools:/.test(l));
    if (!line) return undefined;
    return line
      .replace(/^tools:/, "")
      .split(/[\s,[\]"']+/)
      .filter((t) => t !== "");
  }
  return undefined;
}

function subagentNames(input: any): string[] {
  const names: string[] = [];
  if (typeof input?.agent === "string") names.push(input.agent);
  for (const list of [input?.tasks, input?.chain]) {
    if (Array.isArray(list)) for (const t of list) if (typeof t?.agent === "string") names.push(t.agent);
  }
  return names;
}

// controlConfig is what quartermaster writes beside the sockets
// (control/config.json): the commands to ask about rather than block, as
// regular expressions matched against the bash command, and how long an ask
// waits for an answer. It is read on each ask, so an edit applies at once. A
// missing or unreadable file asks about nothing.
function controlConfig(sock: string): { ask: RegExp[]; timeoutMs: number } {
  const cfg = { ask: [] as RegExp[], timeoutMs: 10 * 60 * 1000 };
  try {
    const raw = JSON.parse(readFileSync(path.join(path.dirname(sock), "config.json"), "utf8"));
    for (const p of raw.ask || []) {
      try {
        cfg.ask.push(new RegExp(p));
      } catch {
        // quartermaster checks the patterns; one that still fails asks nothing.
      }
    }
    if (typeof raw.ask_timeout_seconds === "number" && raw.ask_timeout_seconds > 0) {
      cfg.timeoutMs = raw.ask_timeout_seconds * 1000;
    }
  } catch {
    // No configuration: ask about nothing.
  }
  return cfg;
}

const askOptions = ["Yes, run it", "No, block it"];

export default function (pi) {
  pi.registerFlag("quartermaster-control", {
    description: "unix socket quartermaster talks to this worker on (set by quartermaster)",
    type: "string",
    default: "",
  });

  // Texts of prompts quartermaster sent over the socket that pi hasn't yet
  // passed to the input event, so it can be recorded as their source.
  const fromQuartermaster: string[] = [];

  // A native supervisor owns exactly one saved session. Never let an
  // extension command fork/switch it behind the supervisor and its lease.
  const nativeRPC = (ctx) => ctx?.mode === "rpc" && process.env.QUARTERMASTER_RPC === "1";
  pi.on("session_before_switch", (_event, ctx) => nativeRPC(ctx) ? { cancel: true } : undefined);
  pi.on("session_before_fork", (_event, ctx) => nativeRPC(ctx) ? { cancel: true } : undefined);

  pi.on("input", (event) => {
    let source = event.source;
    if (source === "extension") {
      const i = fromQuartermaster.indexOf(event.text);
      if (i >= 0) {
        fromQuartermaster.splice(i, 1);
        source = "quartermaster";
      }
    }
    pi.appendEntry("quartermaster.input", { source });
    return { action: "continue" };
  });

  pi.on("ui_prompt_start", (event) => {
    pi.events.emit("herdr:blocked", { active: true, label: event.title || "waiting at a " + event.kind + " prompt" });
  });
  pi.on("ui_prompt_end", () => {
    pi.events.emit("herdr:blocked", { active: false });
  });

  pi.on("session_compact", () => {
    if (!process.env.BEADS_ACTOR) return;
    const r = spawnSync("bd", ["prime"], { encoding: "utf8", timeout: 30000 });
    if (r.status !== 0 || !r.stdout) return;
    // Stored for the next turn; it doesn't start one.
    pi.sendMessage({ customType: "quartermaster.bd-prime", content: r.stdout, display: false });
  });

  // The control socket (6).
  let server: net.Server | undefined;
  let sockPath = "";
  let token = "";
  let ctxNow;
  let askSeq = 0;
  // The ask waiting on quartermaster, at most one: tool calls that ask wait
  // their turn behind it.
  let pending: { id: string; command: string; reason: string; resolve: (choice: number, by: string) => void } | undefined;
  let askChain: Promise<unknown> = Promise.resolve();

  function closeControl() {
    if (server) server.close();
    server = undefined;
    for (const f of [sockPath, sockPath + ".token"]) {
      try {
        if (f) unlinkSync(f);
      } catch {}
    }
  }

  function handle(req: any): any {
    if (typeof req?.token !== "string" || req.token.length !== token.length ||
      !timingSafeEqual(Buffer.from(req.token), Buffer.from(token))) {
      return { ok: false, error: "bad token" };
    }
    switch (req.op) {
      case "ping":
        return { ok: true, idle: ctxNow ? ctxNow.isIdle() : true };
      case "prompt": {
        if (typeof req.text !== "string" || req.text.trim() === "") return { ok: false, error: "no text" };
        const busy = ctxNow ? !ctxNow.isIdle() : false;
        fromQuartermaster.push(req.text);
        try {
          // Busy: a follow-up, delivered when the running turn ends, as a
          // prompt typed then would be.
          if (busy) pi.sendUserMessage(req.text, { deliverAs: "followUp" });
          else pi.sendUserMessage(req.text);
        } catch (e) {
          const i = fromQuartermaster.indexOf(req.text);
          if (i >= 0) fromQuartermaster.splice(i, 1);
          return { ok: false, error: String(e?.message || e) };
        }
        return { ok: true, queued: busy };
      }
      case "ask":
        return {
          ok: true,
          ask: pending ? { id: pending.id, command: pending.command, reason: pending.reason, options: askOptions } : null,
        };
      case "answer": {
        if (!pending || req.id !== pending.id) return { ok: false, error: "no such ask waiting" };
        const n = Number(req.choice);
        if (!Number.isInteger(n) || n < 1 || n > askOptions.length) return { ok: false, error: "no option " + req.choice };
        pending.resolve(n, typeof req.by === "string" ? req.by : "");
        return { ok: true };
      }
    }
    return { ok: false, error: "unknown op " + req?.op };
  }

  function openControl(p: string) {
    const dir = path.dirname(p);
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    chmodSync(dir, 0o700);
    try {
      unlinkSync(p); // a pi that died without closing it
    } catch {}
    token = randomBytes(32).toString("hex");
    const srv = net.createServer((conn) => {
      let buf = "";
      conn.setEncoding("utf8");
      conn.setTimeout(10000, () => conn.destroy());
      conn.on("data", (d) => {
        buf += d;
        if (buf.length > 1 << 20) return conn.destroy();
        const nl = buf.indexOf("\n");
        if (nl < 0) return;
        let res;
        try {
          res = handle(JSON.parse(buf.slice(0, nl)));
        } catch (e) {
          res = { ok: false, error: "bad request" };
        }
        conn.end(JSON.stringify(res) + "\n");
      });
      conn.on("error", () => {});
    });
    srv.on("error", () => closeControl());
    srv.listen(p, () => {
      chmodSync(p, 0o600);
      // The token goes last: quartermaster takes a token file as the socket
      // being ready.
      writeFileSync(p + ".token", token, { mode: 0o600 });
    });
    server = srv;
    sockPath = p;
  }

  pi.on("session_start", (_event, ctx) => {
    ctxNow = ctx;
    const p = pi.getFlag("quartermaster-control");
    if (typeof p === "string" && p !== "" && !server) {
      try {
        openControl(p);
      } catch {
        closeControl();
      }
    }
  });
  pi.on("session_shutdown", () => closeControl());
  process.once("exit", () => closeControl());

  // askQuartermaster waits for quartermaster's answer to a guarded command:
  // undefined lets it run, a string is the reason to block it.
  function askQuartermaster(command: string, reason: string, timeoutMs: number): Promise<string | undefined> {
    const run = () =>
      new Promise<string | undefined>((resolve) => {
        if (!server) return resolve(reason + "\n\n" + handOff);
        const id = String(++askSeq) + "-" + randomBytes(3).toString("hex");
        pi.appendEntry("quartermaster.ask", { id, command, reason });
        pi.events.emit("herdr:blocked", { active: true, label: "asking quartermaster: " + command.slice(0, 80) });
        if (ctxNow?.mode === "rpc") ctxNow.ui.setStatus("quartermaster.ask", command);
        const finish = (out: string | undefined, entry: any) => {
          clearTimeout(timer);
          pending = undefined;
          if (ctxNow?.mode === "rpc") ctxNow.ui.setStatus("quartermaster.ask", undefined);
          pi.events.emit("herdr:blocked", { active: false });
          pi.appendEntry("quartermaster.answer", { id, ...entry });
          resolve(out);
        };
        const timer = setTimeout(
          () => finish(reason + "\n\nquartermaster had no answer in time.\n\n" + handOff, { choice: 0, timeout: true }),
          timeoutMs,
        );
        pending = {
          id,
          command,
          reason,
          resolve: (choice, by) =>
            finish(
              choice === 1
                ? undefined
                : "Declined through quartermaster" + (by ? " by " + by : "") + ": " + reason +
                    "\n\nDo not run it another way. If the bead needs it, ask on the bead.",
              { choice, option: askOptions[choice - 1], by },
            ),
        };
      });
    const next = askChain.then(run, run);
    askChain = next.catch(() => {});
    return next;
  }

  pi.on("tool_call", async (event, ctx) => {
    ctxNow = ctx;
    const cwd = ctx?.cwd || process.cwd();
    if (event.toolName === "bash" && typeof event.input?.command === "string") {
      const command = event.input.command;
      const payload = JSON.stringify({
        hook_event_name: "PreToolUse",
        tool_name: "Bash",
        tool_input: { command },
        cwd,
      });
      let ask: string | undefined;
      for (const hook of bashHooks()) {
        const d = runHook(hook, payload, cwd);
        if (d?.kind === "block") return { block: true, reason: d.reason + "\n\n" + handOff };
        if (d?.kind === "ask" && ask === undefined) ask = d.reason;
      }
      if (ask !== undefined) {
        const cfg = server ? controlConfig(sockPath) : { ask: [], timeoutMs: 0 };
        if (!cfg.ask.some((re) => re.test(command))) return { block: true, reason: ask + "\n\n" + handOff };
        const reason = await askQuartermaster(command, ask, cfg.timeoutMs);
        if (reason !== undefined) return { block: true, reason };
      }
    }
    if (event.toolName === "subagent") {
      const names = subagentNames(event.input);
      const broad = names.filter((n) => {
        const tools = agentTools(n, cwd);
        return !tools || tools.length === 0 || tools.some((t) => !readOnlyTools.has(t));
      });
      if (names.length === 0 || broad.length > 0) {
        return {
          block: true,
          reason:
            "quartermaster allows only read-only subagents (tools: read, grep, find, ls) under pi; " +
            (broad.length > 0 ? broad.join(", ") + " can do more, or has no definition. " : "") +
            "Do the work yourself, or use a read-only agent.",
        };
      }
    }
    return undefined;
  });
}
