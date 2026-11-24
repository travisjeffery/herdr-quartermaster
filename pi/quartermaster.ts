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
//   7. Registers the virtual models quartermaster/worker-auto and
//      quartermaster/coordinator-auto (pi/router.ts), which route each turn
//      through the role's ordered physical model chain (Antigravity Neon
//      XHigh first, then Claude, then OpenAI), failing over on per-provider
//      low quota (quota.json beside the --quartermaster-router file), rate
//      limits, provider unavailability, or expired/invalid auth tokens.
//   8. For bead workers, registers structured `ask_user` and `report_milestone`
//      tools backed by `quartermaster card ask|milestone` (cards.go), writing
//      versioned JSON cards and recording `quartermaster.ask_user` and
//      `quartermaster.milestone` custom session entries.
//
// @ts-nocheck

import { installCoordinatorInbox } from "./coordinator.ts";
import {
  decideRoute,
  formatVirtualModelName,
  isValidThinkingLevel,
  parseModelSpec,
  parseQuotaSnapshot,
  parseRouterConfig,
} from "./router.ts";
import { spawnSync } from "node:child_process";
import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
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

const beadNamePattern = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const milestoneStatuses = new Set(["info", "ok", "warn", "blocked"]);

function argvFlagValue(flagName: string): string {
  const argv = process.argv || [];
  const prefix = flagName + "=";
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === flagName && i + 1 < argv.length) return String(argv[i + 1] || "").trim();
    if (a.startsWith(prefix)) return a.slice(prefix.length).trim();
  }
  return "";
}

function hasArgvFlag(flagName: string): boolean {
  const argv = process.argv || [];
  const prefix = flagName + "=";
  return argv.some((a) => a === flagName || a.startsWith(prefix));
}

function defaultWorkerBead(): string {
  const envBead = (process.env.QUARTERMASTER_BEAD || "").trim();
  if (envBead) return envBead;
  const sessionId = argvFlagValue("--session-id");
  if (sessionId && beadNamePattern.test(sessionId)) return sessionId;
  return "";
}

function resolveWorkerBead(explicit: unknown): string {
  const bead = typeof explicit === "string" && explicit.trim() !== "" ? explicit.trim() : defaultWorkerBead();
  if (!bead) {
    throw new Error("no bead id: set QUARTERMASTER_BEAD or pass an explicit bead argument");
  }
  if (!beadNamePattern.test(bead)) {
    throw new Error("invalid bead id: " + bead);
  }
  return bead;
}

function quartermasterBinary(): string {
  const custom = (process.env.QUARTERMASTER_BIN || "").trim();
  return custom || "quartermaster";
}

function runQuartermasterCard(args: string[], payload: unknown): any {
  const r = spawnSync(quartermasterBinary(), args, {
    input: JSON.stringify(payload),
    encoding: "utf8",
    timeout: 30000,
  });
  if (r.error) {
    throw new Error("quartermaster card failed: " + String(r.error.message || r.error));
  }
  if (r.status !== 0) {
    const msg = (r.stderr || r.stdout || "quartermaster exited with status " + r.status).trim();
    throw new Error(msg);
  }
  const text = (r.stdout || "").trim();
  try {
    return JSON.parse(text);
  } catch {
    throw new Error("quartermaster card returned non-JSON output: " + text);
  }
}

function registerWorkerCardTools(pi: any) {
  if (typeof pi.registerTool !== "function") return;
  const isCoordinator =
    hasArgvFlag("--quartermaster-coordinator") ||
    (typeof pi.getFlag === "function" && Boolean(pi.getFlag("quartermaster-coordinator")));
  if (isCoordinator) return;
  const isWorker =
    Boolean(defaultWorkerBead()) ||
    hasArgvFlag("--quartermaster-control") ||
    (typeof pi.getFlag === "function" && Boolean(pi.getFlag("quartermaster-control")));
  if (!isWorker) return;

  const linkSchema = {
    type: "object",
    properties: {
      label: { type: "string", description: "Short display label for the link" },
      url: { type: "string", description: "Link URL" },
    },
    required: ["label", "url"],
    additionalProperties: false,
  };

  pi.registerTool({
    name: "ask_user",
    label: "Ask User",
    description:
      "Record a structured needs_me question card (2-9 options, impact class 0-9, optional recommendation and evidence), " +
      "mirror a tap-compatible QUESTION:/OPTIONS:/CLASS: note on the bead, set bead status to needs_me, and pause for the user's answer.",
    promptSnippet: "Ask the user a structured needs_me question with 2-9 options, impact class, recommendation, and evidence",
    promptGuidelines: [
      "Use ask_user when a decision or step above your autonomy level requires user input; after calling ask_user, stop and wait for the user's answer.",
    ],
    parameters: {
      type: "object",
      properties: {
        bead: { type: "string", description: "Bead ID (optional when QUARTERMASTER_BEAD is set)" },
        question: { type: "string", description: "Short question line" },
        details: { type: "string", description: "Optional extra context" },
        options: {
          type: "array",
          minItems: 2,
          maxItems: 9,
          description: "2-9 numbered decision options",
          items: {
            type: "object",
            properties: {
              number: { type: "integer", minimum: 1, maximum: 9 },
              label: { type: "string" },
              description: { type: "string" },
              class: { type: "integer", minimum: 0, maximum: 9 },
            },
            required: ["label"],
            additionalProperties: false,
          },
        },
        class: { type: "integer", minimum: 0, maximum: 9, description: "Impact class 0-9 (cannot lower an attached IaC preview class)" },
        class_reason: { type: "string", description: "Short explanation of why the step has this class" },
        recommendation: {
          type: "object",
          properties: {
            option: { type: "integer", minimum: 1, maximum: 9 },
            rationale: { type: "string" },
          },
          required: ["option"],
          additionalProperties: false,
        },
        critical: { type: "boolean", description: "Mark true for class 8-9 or irreversible decisions" },
        evidence: {
          type: "object",
          properties: {
            pr_url: { type: "string" },
            plan_digest: { type: "string" },
            run_urls: { type: "array", items: { type: "string" } },
            cost_estimate: { type: "string" },
            iac: {
              type: "object",
              properties: {
                tool: { type: "string" },
                target: { type: "string" },
                create: { type: "integer", minimum: 0 },
                update: { type: "integer", minimum: 0 },
                replace: { type: "integer", minimum: 0 },
                delete: { type: "integer", minimum: 0 },
                summary: { type: "string" },
                excerpt: { type: "string" },
              },
              additionalProperties: false,
            },
            links: { type: "array", items: linkSchema },
          },
          additionalProperties: false,
        },
        iac_preview: { type: "string", description: "Optional path to pulumi preview --json or tofu show -json file for gate classification" },
        iac_target: { type: "string", description: "Optional IaC target stack/environment when iac_preview is supplied" },
        iac_expect: { type: "string", description: "Optional expected IaC counts (e.g. create=1,update=1)" },
      },
      required: ["question", "options", "class"],
      additionalProperties: false,
    },
    async execute(_toolCallId: string, params: any) {
      const bead = resolveWorkerBead(params?.bead);
      const question = typeof params?.question === "string" ? params.question.trim() : "";
      if (!question) throw new Error("ask_user: question is required");
      if (!Array.isArray(params?.options) || params.options.length < 2 || params.options.length > 9) {
        throw new Error("ask_user: options must contain 2-9 items");
      }
      const options = params.options.map((opt: any, idx: number) => {
        if (typeof opt === "string") {
          const label = opt.trim();
          if (!label) throw new Error("ask_user: option " + (idx + 1) + " label is empty");
          return { number: idx + 1, label };
        }
        const label = typeof opt?.label === "string" ? opt.label.trim() : "";
        if (!label) throw new Error("ask_user: option " + (idx + 1) + " label is empty");
        const out: any = { number: typeof opt?.number === "number" ? opt.number : idx + 1, label };
        if (typeof opt?.description === "string" && opt.description.trim() !== "") out.description = opt.description.trim();
        if (typeof opt?.class === "number") {
          if (!Number.isInteger(opt.class) || opt.class < 0 || opt.class > 9) {
            throw new Error("ask_user: option " + (idx + 1) + " class must be an integer 0-9");
          }
          out.class = opt.class;
        }
        return out;
      });
      if (!Number.isInteger(params?.class) || params.class < 0 || params.class > 9) {
        throw new Error("ask_user: class must be an integer 0-9");
      }
      const payload: any = {
        question,
        options,
        class: params.class,
      };
      if (typeof params.details === "string" && params.details.trim() !== "") payload.details = params.details.trim();
      if (typeof params.class_reason === "string" && params.class_reason.trim() !== "") payload.class_reason = params.class_reason.trim();
      if (typeof params.critical === "boolean") payload.critical = params.critical;
      if (params.recommendation && typeof params.recommendation === "object") {
        const optNum = params.recommendation.option;
        if (!Number.isInteger(optNum) || optNum < 1 || optNum > options.length) {
          throw new Error("ask_user: recommendation.option must be between 1 and " + options.length);
        }
        payload.recommendation = {
          option: optNum,
          ...(typeof params.recommendation.rationale === "string" && params.recommendation.rationale.trim() !== ""
            ? { rationale: params.recommendation.rationale.trim() }
            : {}),
        };
      }
      if (params.evidence && typeof params.evidence === "object") {
        payload.evidence = params.evidence;
      }
      const cliArgs = ["card", "ask", bead, "--file", "-", "--json"];
      if (typeof params.iac_preview === "string" && params.iac_preview.trim() !== "") {
        cliArgs.push("--iac-preview", params.iac_preview.trim());
      }
      if (typeof params.iac_target === "string" && params.iac_target.trim() !== "") {
        cliArgs.push("--iac-target", params.iac_target.trim());
      }
      if (typeof params.iac_expect === "string" && params.iac_expect.trim() !== "") {
        cliArgs.push("--iac-expect", params.iac_expect.trim());
      }
      const card = runQuartermasterCard(cliArgs, payload);
      pi.appendEntry("quartermaster.ask_user", {
        bead,
        hash: card.hash,
        class: card.class,
        critical: Boolean(card.critical),
        question: card.question,
        options: card.options,
      });
      return {
        content: [
          {
            type: "text",
            text:
              "Recorded question card [" +
              card.hash +
              "] on " +
              bead +
              " (class " +
              card.class +
              (card.critical ? ", CRITICAL" : "") +
              ") and set bead status to needs_me. Stop now and wait for the user's answer.",
          },
        ],
        details: card,
      };
    },
  });

  pi.registerTool({
    name: "report_milestone",
    label: "Report Milestone",
    description:
      "Append a structured milestone record (phase, status, summary, evidence) to the bead's milestone log and mirror a concise MILESTONE note.",
    promptSnippet: "Record a structured progress milestone (phase, status, summary, evidence) on the current bead",
    promptGuidelines: [
      "Use report_milestone at meaningful rollout or delivery checkpoints (such as pr_opened, merged, applied, or verified).",
    ],
    parameters: {
      type: "object",
      properties: {
        bead: { type: "string", description: "Bead ID (optional when QUARTERMASTER_BEAD is set)" },
        phase: { type: "string", description: "Short milestone phase identifier (e.g. pr_opened, merged, applied, verified)" },
        status: {
          type: "string",
          enum: ["info", "ok", "warn", "blocked"],
          description: "Milestone status (default ok)",
        },
        summary: { type: "string", description: "One-line milestone summary" },
        evidence: {
          type: "object",
          properties: {
            pr_url: { type: "string" },
            run_url: { type: "string" },
            sha: { type: "string" },
            plan_digest: { type: "string" },
            links: { type: "array", items: linkSchema },
          },
          additionalProperties: false,
        },
      },
      required: ["phase", "summary"],
      additionalProperties: false,
    },
    async execute(_toolCallId: string, params: any) {
      const bead = resolveWorkerBead(params?.bead);
      const phase = typeof params?.phase === "string" ? params.phase.trim() : "";
      const summary = typeof params?.summary === "string" ? params.summary.trim() : "";
      if (!phase) throw new Error("report_milestone: phase is required");
      if (!summary) throw new Error("report_milestone: summary is required");
      const status = typeof params?.status === "string" && params.status.trim() !== "" ? params.status.trim().toLowerCase() : "ok";
      if (!milestoneStatuses.has(status)) {
        throw new Error("report_milestone: status must be info, ok, warn or blocked");
      }
      const payload: any = { phase, status, summary };
      if (params.evidence && typeof params.evidence === "object") {
        payload.evidence = params.evidence;
      }
      const rec = runQuartermasterCard(["card", "milestone", bead, "--file", "-", "--json"], payload);
      pi.appendEntry("quartermaster.milestone", {
        bead,
        phase: rec.phase,
        status: rec.status,
        summary: rec.summary,
        at: rec.at,
      });
      return {
        content: [
          {
            type: "text",
            text: "Recorded milestone [" + rec.phase + "] (" + rec.status + ") on " + bead + ": " + rec.summary,
          },
        ],
        details: rec,
      };
    },
  });
}

export default function (pi) {
  installCoordinatorInbox(pi);
  pi.registerFlag("quartermaster-control", {
    description: "unix socket quartermaster talks to this worker on (set by quartermaster)",
    type: "string",
    default: "",
  });
  pi.registerFlag("quartermaster-router", {
    description: "router.json for the quartermaster virtual models (set by quartermaster)",
    type: "string",
    default: "",
  });
  registerWorkerCardTools(pi);

  // The virtual auto models (pi/router.ts): one selectable model per role
  // that starts each turn on the highest-priority healthy physical model in
  // the role's ordered chain (default: Antigravity Neon XHigh -> Claude ->
  // OpenAI), failing over on per-provider low quota, rate limits, provider
  // unavailability, or expired auth tokens. Route reads both JSON files per
  // request, so a ticker refresh or `quartermaster router sync` applies at
  // the next turn boundary; a missing or bad file routes on defaults with no
  // quota, never an error.
  let refreshRouterModels: (() => void) | undefined;
  if (typeof pi.registerVirtualModel === "function") {
    const readOr = (p: string): string => {
      try {
        return readFileSync(p, "utf8");
      } catch {
        return "";
      }
    };
    const refOf = (m: any): string | null =>
      m && typeof m.provider === "string" && typeof m.id === "string" ? m.provider + "/" + m.id : null;
    const findUsable = (ctx: any, specStr: string): { model: any; ref: string; thinkingLevel: string | null } | null => {
      const spec = parseModelSpec(specStr);
      if (!spec) return null;
      const model = ctx.modelRegistry.find(spec.rawProvider, spec.id);
      if (!model) return null;
      const avail = typeof ctx.modelRegistry.getAvailable === "function" ? ctx.modelRegistry.getAvailable() : null;
      if (Array.isArray(avail) && !avail.some((m: any) => refOf(m) === spec.ref)) return null;
      return { model, ref: spec.ref, thinkingLevel: spec.thinkingLevel };
    };
    const readConfiguredForLabel = () => {
      const routerPath = pi.getFlag("quartermaster-router");
      if (typeof routerPath !== "string" || !routerPath) return null;
      const rawText = readOr(routerPath);
      if (!rawText) return null;
      try {
        const parsed = JSON.parse(rawText);
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return null;
      } catch {
        return null;
      }
      return parseRouterConfig(rawText);
    };
    const registeredNames: Record<string, string> = {};
    refreshRouterModels = () => {
      const labelCfg = readConfiguredForLabel();
      for (const [role, id] of [
        ["worker", "worker-auto"],
        ["coordinator", "coordinator-auto"],
      ] as const) {
        const name = formatVirtualModelName(role, labelCfg);
        if (registeredNames[id] === name) continue;
        registeredNames[id] = name;
        pi.registerVirtualModel({
          provider: "quartermaster",
          id,
          name,
          thinkingLevels: ["off", "minimal", "low", "medium", "high", "xhigh", "max"],
          // The smaller physical window, shown only before the first
          // response; compaction re-checks the routed model's own limits.
          contextWindow: 272_000,
          maxTokens: 65_536,
          route(request: any, ctx: any) {
            const routerPath = pi.getFlag("quartermaster-router");
            const config = parseRouterConfig(typeof routerPath === "string" && routerPath ? readOr(routerPath) : "");
            const quotaPath =
              typeof routerPath === "string" && routerPath ? path.join(path.dirname(routerPath), "quota.json") : "";
            const nowMs = Date.now();
            const quota = parseQuotaSnapshot(quotaPath ? readOr(quotaPath) : "", nowMs, config.quota_max_age_seconds * 1000);
            const chain = role === "coordinator" ? config.coordinator_models : config.worker_models;
            const usableMap = new Map<string, { model: any; thinkingLevel: string | null }>();
            for (const spec of chain) {
              const resolved = findUsable(ctx, spec);
              if (resolved && !usableMap.has(resolved.ref)) {
                usableMap.set(resolved.ref, { model: resolved.model, thinkingLevel: resolved.thinkingLevel });
              }
            }
            const decision = decideRoute({
              role,
              config,
              quota,
              nowMs,
              usable: [...usableMap.keys()],
              reason: request.reason,
              previous: refOf(request.previous?.model),
              failed: refOf(request.failed?.model),
              failedError: request.failed?.message
                ? String(request.failed.message.errorMessage || "") + " " + String(request.failed.message.stopReason || "")
                : "",
              state: request.state ?? null,
            });
            // Keep the exact model object of a sticky route; else use the
            // resolved usable physical model from the chain, falling back to
            // the first usable model so route() errors only when no physical
            // model can serve at all.
            let model = null;
            const stickyObj = request.failed ?? request.previous;
            const sticky = stickyObj?.model;
            const stickyMatched = !!(sticky && refOf(sticky) === decision.model);
            const isStickyRoute =
              stickyMatched && typeof decision.why === "string" && decision.why.startsWith("sticky:");
            if (stickyMatched) model = sticky;
            if (!model) model = usableMap.get(decision.model)?.model || findUsable(ctx, decision.model)?.model || null;
            if (!model) {
              for (const entry of usableMap.values()) {
                model = entry.model;
                break;
              }
            }
            if (!model && sticky) model = sticky;
            if (!model) {
              throw new Error(
                "quartermaster router: none of " +
                  chain.join(", ") +
                  " is usable in pi (check `pi --list-models` and `pi auth check`)",
              );
            }
            let thinkingLevel = "off";
            if (model.reasoning) {
              const modelRef = refOf(model);
              const entryThinking =
                modelRef === decision.model
                  ? decision.thinkingLevel
                  : (modelRef && usableMap.get(modelRef)?.thinkingLevel) || null;
              if (isStickyRoute && isValidThinkingLevel(stickyObj?.thinkingLevel)) {
                thinkingLevel = stickyObj.thinkingLevel;
              } else if (isValidThinkingLevel(entryThinking)) {
                thinkingLevel = entryThinking;
              } else if (isValidThinkingLevel(request.thinkingLevel)) {
                thinkingLevel = request.thinkingLevel;
              } else {
                thinkingLevel = "xhigh";
              }
            }
            return { model, thinkingLevel, state: decision.state };
          },
        });
      }
    };
    refreshRouterModels();
  }

  // Texts of prompts quartermaster sent over the socket that pi hasn't yet
  // passed to the input event, so it can be recorded as their source.
  const fromQuartermaster: string[] = [];

  // Adoption starts parked: neither an automated prompt nor a startup
  // extension may turn a transport check into task execution. The supervisor
  // removes this marker only on explicit, gated activation.
  const migrationParked = () => {
    const p = process.env.QUARTERMASTER_RPC_PARK_FILE;
    if (!p) return false;
    try { readFileSync(p); return true; } catch (e) { return e?.code !== "ENOENT"; }
  };
  const parkedReason = "Transport migration is parked; no task continuation or approval was granted.";
  pi.on("user_bash", () => migrationParked() ? { result: { output: parkedReason, exitCode: 1, cancelled: false, truncated: false } } : undefined);

  // A native supervisor owns exactly one saved session. Never let an
  // extension command fork/switch it behind the supervisor and its lease.
  const nativeRPC = (ctx) => ctx?.mode === "rpc" && process.env.QUARTERMASTER_RPC === "1";
  pi.on("session_before_switch", (_event, ctx) => nativeRPC(ctx) ? { cancel: true } : undefined);
  pi.on("session_before_fork", (_event, ctx) => nativeRPC(ctx) ? { cancel: true } : undefined);

  pi.on("input", (event) => {
    if (migrationParked()) return { action: "handled" };
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
  let handoffContext = "";
  const hasHandoff = (ctx) => !!handoffContext && ctx.sessionManager.getBranch().some(
    (entry) => entry.type === "custom_message" && entry.customType === "quartermaster.handoff" && entry.content === handoffContext,
  );
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
        return { ok: true, idle: ctxNow ? ctxNow.isIdle() : true,
          handoff_hash: hasHandoff(ctxNow) ? createHash("sha256").update(handoffContext).digest("hex") : "" };
      case "prompt": {
        // Native prompts must enter through the supervisor receipt/fencing path.
        // The guard socket is only for ping/ask/answer on native workers.
        if (nativeRPC(ctxNow)) return { ok: false, error: "native prompts require supervisor admission" };
        if (migrationParked()) return { ok: false, error: parkedReason };
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
    if (typeof refreshRouterModels === "function") {
      try {
        refreshRouterModels();
      } catch {
        // Keep the initial neutral virtual-model labels if refresh fails.
      }
    }
    const handoff = process.env.QUARTERMASTER_RPC_HANDOFF_FILE;
    if (handoff && nativeRPC(ctx)) {
      // Verify the freshly seeded custom message, never convert old history
      // or send a task prompt. Startup failures leave the guard socket absent.
      handoffContext = JSON.parse(readFileSync(handoff, "utf8")).handoff.context;
      if (typeof handoffContext !== "string" || !handoffContext) throw new Error("missing handoff context");
      if (!hasHandoff(ctx)) throw new Error("handoff context absent from session branch");
    }
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
    if (migrationParked()) return { block: true, terminate: true, reason: parkedReason };
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
