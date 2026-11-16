// quartermaster's pi extension, loaded into every pi worker and coordinator
// quartermaster starts (`pi -e <state dir>/pi/quartermaster.ts`). The binary
// embeds this file and rewrites it there on each start; edit it in the
// quartermaster repo, not in place.
//
// pi asks before nothing: it runs every tool call with the user's
// permissions (pi's docs/security.md). A quartermaster worker runs
// unattended, so there is nobody to answer a prompt anyway. This extension
// therefore blocks rather than asks, and the block's reason tells the worker
// to hand the step to the user the way it hands over a Claude Code denial:
// a RUN: note and needs_me.
//
// It does four things:
//   1. Runs the same PreToolUse bash hooks Claude Code workers run
//      (QUARTERMASTER_PI_BASH_HOOKS, else block-dangerous-bash.py and
//      rm-guard.sh under ~/.claude/hooks), with the JSON Claude Code hands
//      them. Exit 2 blocks with the hook's message; an "ask" or "deny"
//      decision blocks with its reason. A hook that can't run fails open,
//      as the hooks themselves do.
//   2. Allows a subagent only when every agent it names is read-only by its
//      definition (tools: read, grep, find, ls). A pi subagent is a separate
//      pi process without this extension, so a broad one would run unguarded.
//   3. Records where each prompt came from as a "quartermaster.input" custom
//      entry (never sent to the model), so the ticker can tell a prompt from
//      pi waking itself (pisession.go).
//   4. Reports any extension dialog to herdr as blocked, so the ticker shows
//      the thread as waiting on the user.
//   5. After a compaction, puts `bd prime` back in context for a beads
//      worker (BEADS_ACTOR set), as Claude Code's PreCompact hook does.
//
// @ts-nocheck

import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
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

// runHook returns a reason to block, or undefined to let the call through.
function runHook(argv: string[], payload: string, cwd: string): string | undefined {
  const r = spawnSync(argv[0], argv.slice(1), { input: payload, cwd, encoding: "utf8", timeout: 15000 });
  if (r.error) return undefined;
  if (r.status === 2) return (r.stderr || r.stdout || "blocked by " + path.basename(argv[1])).trim();
  try {
    const out = JSON.parse((r.stdout || "").trim() || "{}");
    const h = out.hookSpecificOutput || {};
    if (h.permissionDecision === "deny" || h.permissionDecision === "ask") {
      return (h.permissionDecisionReason || path.basename(argv[1]) + " would ask the user").trim();
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

export default function (pi) {
  pi.on("input", (event) => {
    pi.appendEntry("quartermaster.input", { source: event.source });
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

  pi.on("tool_call", async (event, ctx) => {
    const cwd = ctx?.cwd || process.cwd();
    if (event.toolName === "bash" && typeof event.input?.command === "string") {
      const payload = JSON.stringify({
        hook_event_name: "PreToolUse",
        tool_name: "Bash",
        tool_input: { command: event.input.command },
        cwd,
      });
      for (const hook of bashHooks()) {
        const reason = runHook(hook, payload, cwd);
        if (reason) return { block: true, reason: reason + "\n\n" + handOff };
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
