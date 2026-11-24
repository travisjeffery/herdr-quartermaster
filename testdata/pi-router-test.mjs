// Offline tests for pi/router.ts, run by pirouter_test.go under node:
// node testdata/pi-router-test.mjs. No pi, no network: the router decision
// is pure. Exits nonzero on the first failed case.
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";

const {
  DEFAULT_AUTH_COOLDOWN_MS,
  DEFAULT_QUOTA_COOLDOWN_MS,
  VALID_THINKING_LEVELS,
  authFailure,
  decideRoute,
  failoverKind,
  formatVirtualModelName,
  isValidThinkingLevel,
  parseModelSpec,
  parseQuotaSnapshot,
  parseResetDurationMs,
  parseRouterConfig,
  quotaFailure,
  quotaKeyForModel,
  stripThinkingLevel,
  thinkingLevelOf,
  thresholdsFor,
} = await import("../pi/router.ts");

const now = Date.parse("2026-11-23T17:00:00Z");
const cfg = parseRouterConfig("{}");
const AGY = "antigravity/gemini-4-neon-xhigh";
const CLAUDE_W = "anthropic/claude-opus-5-5";
const CLAUDE_C = "anthropic/claude-fable-5-2";
const OPENAI = "openai-codex/gpt-6.2-astra";

let passed = 0;
function check(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error(name + ": got " + g + ", want " + w);
  passed++;
}

function quota(agyLeft, claudeLeft, openaiLeft, opts = {}) {
  const provider = (left, pOpts = {}) =>
    left === null
      ? null
      : {
          headroom_pct: left,
          fresh: (pOpts.fresh ?? opts.fresh) !== false,
          at: new Date(now - (pOpts.ageMs ?? opts.ageMs ?? 60_000)).toISOString(),
          tight_resets_at: pOpts.resetsAt ?? opts.resetsAt ?? new Date(now + 3600_000).toISOString(),
        };
  return parseQuotaSnapshot(
    JSON.stringify({
      generated_at: new Date(now).toISOString(),
      antigravity: provider(agyLeft, opts.agy),
      claude: provider(claudeLeft, opts.claude),
      openai: provider(openaiLeft, opts.openai),
    }),
    now,
    cfg.quota_max_age_seconds * 1000,
  );
}

const base = { config: cfg, nowMs: now, reason: "user", previous: null, failed: null, failedError: "", state: null };
const route = (over) => decideRoute({ ...base, role: "worker", quota: quota(null, null, null), ...over });

// Default ordered chains: Antigravity Neon XHigh first for both worker and coordinator.
check("default worker chain", cfg.worker_models, [AGY, CLAUDE_W, OPENAI]);
check("default coordinator chain", cfg.coordinator_models, [AGY, CLAUDE_C, OPENAI]);
check("worker defaults to Antigravity", route({}).model, AGY);
check("coordinator defaults to Antigravity", route({ role: "coordinator" }).model, AGY);

// Provider quota mapping: known aliases + generic fallback to lowercase provider name.
check("antigravity quota key", quotaKeyForModel(AGY), "antigravity");
check("anthropic quota key", quotaKeyForModel(CLAUDE_W), "claude");
check("openai-codex quota key", quotaKeyForModel(OPENAI), "openai");
check("openai quota key", quotaKeyForModel("openai/gpt-6.2-astra"), "openai");
check("generic provider quota key (meta)", quotaKeyForModel("meta/muse-wonder-full-1.0"), "meta");
check("generic provider quota key (google)", quotaKeyForModel("google/gemini-3-pro:high"), "google");
check("generic provider quota key (vertex)", quotaKeyForModel("Vertex/gemini-3-pro"), "vertex");
check("generic provider quota key (openrouter)", quotaKeyForModel("openrouter/deepseek/r1:medium"), "openrouter");
check("no-provider model quota key", quotaKeyForModel("invalid-model"), null);

// Missing, stale and malformed quota data stays on Antigravity.
check("no quota file", route({ quota: parseQuotaSnapshot("", now, 1000) }).model, AGY);
check("malformed quota", route({ quota: parseQuotaSnapshot("{nope", now, 1000) }).model, AGY);
check("stale antigravity reading stays", route({ quota: quota(0, 90, 90, { ageMs: 7 * 3600_000 }) }).model, AGY);
check("unfresh antigravity reading stays", route({ quota: quota(0, 90, 90, { fresh: false }) }).model, AGY);

// Per-provider quota isolation: Claude/OpenAI low headroom NEVER switches away from Antigravity.
check("claude 0% does not move off healthy antigravity", route({ quota: quota(94, 0, 0) }).model, AGY);
check("claude 0% does not move off antigravity without agy quota", route({ quota: quota(null, 0, 0) }).model, AGY);

// Ordered failover on low quota: Antigravity -> Claude -> OpenAI.
check("antigravity at threshold switches to claude worker", route({ quota: quota(20, 80, 90) }).model, CLAUDE_W);
check("antigravity at threshold switches to claude coordinator", route({ role: "coordinator", quota: quota(20, 80, 90) }).model, CLAUDE_C);
check("antigravity just above threshold stays", route({ quota: quota(20.5, 80, 90) }).model, AGY);
check("antigravity and claude both low switches to openai", route({ quota: quota(10, 15, 80) }).model, OPENAI);
check("antigravity low and claude low, no openai reading, switches to openai", route({ quota: quota(5, 5, null) }).model, OPENAI);

// All three low: pick the provider with strictly most headroom (chain order breaks ties).
check("all low, agy highest, stays agy", route({ quota: quota(15, 10, 5) }).model, AGY);
check("all low, claude highest, picks claude", route({ quota: quota(10, 15, 5) }).model, CLAUDE_W);
check("all low, openai highest, picks openai", route({ quota: quota(5, 10, 18) }).model, OPENAI);
check("all low, tied, stays on preferred", route({ quota: quota(15, 15, 15) }).model, AGY);

// Hysteresis from branch state: hold Claude while Antigravity is between threshold and recover.
const onClaude = route({ quota: quota(10, 80, 90) }).state;
check("switch records side claude", onClaude?.side, "claude");
check("holds claude at agy 25%", route({ quota: quota(25, 80, 90), state: onClaude }).model, CLAUDE_W);
check("no state churn while holding claude", route({ quota: quota(25, 80, 90), state: onClaude }).state, undefined);
check("holds claude with no fresh agy quota", route({ quota: quota(null, 80, 90), state: onClaude }).model, CLAUDE_W);
check("back to agy at recover 30%", route({ quota: quota(30, 80, 90), state: onClaude }).model, AGY);
check("recovery reason", route({ quota: quota(30, 80, 90), state: onClaude }).why, "antigravity:recovered");
const resetAgyState = { ...onClaude, low_quota: { antigravity: { tight_resets_at: new Date(now - 1000).toISOString() } } };
check("back to agy after tight window reset", route({ quota: quota(25, 80, 90), state: resetAgyState }).model, AGY);
check("window reset does not beat still-low agy reading", route({ quota: quota(10, 80, 90), state: resetAgyState }).model, CLAUDE_W);

// Usable subset filtering: environment with only Claude + OpenAI (no Antigravity bridge).
const claudeOpenAI = [CLAUDE_W, OPENAI];
check("subset without agy defaults to claude", route({ usable: claudeOpenAI }).model, CLAUDE_W);
check("subset without agy switches to openai on low claude", route({ usable: claudeOpenAI, quota: quota(90, 15, 80) }).model, OPENAI);
check("subset with only agy stays on agy even when low", route({ usable: [AGY], quota: quota(5, 90, 90) }).model, AGY);

// Backward compatibility with legacy 2-provider router.json and branch state.
const legacyCfg = parseRouterConfig(
  JSON.stringify({ worker_model: "anthropic/claude-sonnet-5-5", threshold_pct: 40, recover_pct: 10, fallback_model: "bad" }),
);
check("legacy worker model", legacyCfg.worker_model, "anthropic/claude-sonnet-5-5");
check("legacy worker chain", legacyCfg.worker_models, ["anthropic/claude-sonnet-5-5", OPENAI]);
check("bad fallback falls back", legacyCfg.fallback_model, OPENAI);
check("recover clamped to threshold", legacyCfg.recover_pct, 40);
check("legacy custom threshold routes to openai", route({ config: legacyCfg, quota: quota(null, 35, 90) }).model, OPENAI);

const legacyOpenAIState = { side: "openai", tight_resets_at: new Date(now + 3600_000).toISOString() };
check("legacy openai state holds openai in 2-provider subset", route({ usable: claudeOpenAI, quota: quota(null, 25, 80), state: legacyOpenAIState }).model, OPENAI);
check("legacy openai state recovers to claude at 30%", route({ usable: claudeOpenAI, quota: quota(null, 30, 80), state: legacyOpenAIState }).model, CLAUDE_W);

// Sticky within a turn, whatever quota says.
check("continuation keeps previous", route({ reason: "continuation", previous: AGY, quota: quota(0, 90, 90) }).model, AGY);
check("direct keeps previous", route({ reason: "direct", previous: OPENAI, quota: quota(90, 90, 0) }).model, OPENAI);
check("direct without previous routes by quota", route({ reason: "direct", quota: quota(0, 0, 90) }).model, OPENAI);
check("direct keeps no state", route({ reason: "direct", quota: quota(0, 0, 90) }).state, undefined);

// Reset duration parser.
check("parseResetDurationMs h/m/s", parseResetDurationMs("agy rate limit (ResourceExhausted): Antigravity quota exhausted; resets in 153h38m14s"), (153 * 3600 + 38 * 60 + 14) * 1000);
check("parseResetDurationMs 2h30m", parseResetDurationMs("resets in 2h30m"), (2 * 3600 + 30 * 60) * 1000);
check("parseResetDurationMs words", parseResetDurationMs("reset in 1 day 2 hours 15 minutes"), (26 * 3600 + 15 * 60) * 1000);
check("parseResetDurationMs seconds", parseResetDurationMs("resets in 45s"), 45_000);
check("parseResetDurationMs ignores ms", parseResetDurationMs("resets in 10ms"), null);
check("parseResetDurationMs none", parseResetDurationMs("429 Too Many Requests"), null);

// Error classifiers.
check("quotaFailure catches agy ResourceExhausted", quotaFailure("agy rate limit (ResourceExhausted): Antigravity quota exhausted; resets in 2h"), true);
check("quotaFailure catches 429 monthly spend limit", quotaFailure("429 monthly spend limit exceeded"), true);
check("quotaFailure catches 529 overloaded", quotaFailure("529 Overloaded"), true);
check("quotaFailure ignores context overflow", quotaFailure("context window exceeded"), false);
check("quotaFailure ignores max_tokens", quotaFailure("max_tokens exceeded"), false);
check("authFailure catches expired authentication token", authFailure("Your authentication token has expired"), true);
check("authFailure catches 401 unauthorized", authFailure("401 Unauthorized: invalid token"), true);
check("authFailure catches invalid api key", authFailure("invalid_api_key"), true);
check("authFailure ignores max_tokens", authFailure("max_tokens exceeded"), false);
check("authFailure ignores connection reset", authFailure("connection reset"), false);
check("failoverKind quota", failoverKind("ResourceExhausted"), "quota");
check("failoverKind auth", failoverKind("Your authentication token has expired"), "auth");
check("failoverKind none", failoverKind("connection reset"), null);

// Multi-hop retry failover cascade: Antigravity -> Claude -> OpenAI -> all cooled down.
check("non-failover retry stays sticky", route({ reason: "retry", failed: AGY, failedError: "connection reset" }).model, AGY);
check("context overflow retry stays sticky", route({ reason: "retry", failed: AGY, failedError: "context window exceeded" }).model, AGY);

const r1 = route({
  reason: "retry",
  failed: AGY,
  failedError: "agy rate limit (ResourceExhausted): Antigravity quota exhausted; resets in 2h30m",
  quota: quota(94, 49, null),
});
check("retry 1: agy ResourceExhausted steps to claude", r1.model, CLAUDE_W);
check("retry 1: records agy cooldown from resets in", r1.state?.cooldowns?.antigravity?.until, new Date(now + (2 * 3600 + 30 * 60) * 1000).toISOString());

// Turn 2 shortly after retry 1: holds Claude (does NOT immediately bounce back to Antigravity on pre-failure 94% snapshot!).
check(
  "turn 2 holds claude despite pre-failure agy 94% snapshot",
  route({ nowMs: now + 60_000, quota: quota(94, 49, null, { agy: { ageMs: 60_000 } }), state: r1.state }).model,
  CLAUDE_W,
);
// Turn 2 with a strictly newer post-failure fresh Antigravity reading >= recover_pct recovers!
const newerQuota = parseQuotaSnapshot(
  JSON.stringify({
    generated_at: new Date(now + 120_000).toISOString(),
    antigravity: { headroom_pct: 95, fresh: true, at: new Date(now + 60_000).toISOString(), tight_resets_at: new Date(now + 7200_000).toISOString() },
    claude: { headroom_pct: 49, fresh: true, at: new Date(now).toISOString(), tight_resets_at: new Date(now + 7200_000).toISOString() },
    openai: null,
  }),
  now + 120_000,
  cfg.quota_max_age_seconds * 1000,
);
check("recovers to agy when strictly newer quota reading >= recover", route({ nowMs: now + 120_000, quota: newerQuota, state: r1.state }).model, AGY);
// Recovers to Antigravity once the parsed resets-in cooldown expires!
check(
  "recovers to agy after resets-in cooldown expires",
  route({ nowMs: now + (2 * 3600 + 31 * 60) * 1000, quota: quota(null, 49, null), state: r1.state }).model,
  AGY,
);

// Retry 2: Claude fails with 429 monthly spend limit -> steps to OpenAI!
const r2 = route({
  reason: "retry",
  failed: CLAUDE_W,
  failedError: "429 monthly spend limit reached",
  quota: quota(94, 49, null),
  state: r1.state,
});
check("retry 2: claude 429 steps to openai", r2.model, OPENAI);
check("retry 2: preserves agy cooldown and adds anthropic cooldown", Object.keys(r2.state?.cooldowns || {}).sort(), ["anthropic", "antigravity"]);
check("retry 2: anthropic default cooldown 15m", r2.state?.cooldowns?.anthropic?.until, new Date(now + DEFAULT_QUOTA_COOLDOWN_MS).toISOString());

// Retry 3: OpenAI fails with expired auth token -> all 3 cooled down.
const r3 = route({
  reason: "retry",
  failed: OPENAI,
  failedError: "Your authentication token has expired",
  quota: quota(94, 49, null),
  state: r2.state,
});
check("retry 3: all cooled down stays on failed", r3.model, OPENAI);
check("retry 3: records openai-codex auth cooldown", r3.state?.cooldowns?.["openai-codex"]?.kind, "auth");
check("retry 3: openai-codex auth cooldown 15m", r3.state?.cooldowns?.["openai-codex"]?.until, new Date(now + DEFAULT_AUTH_COOLDOWN_MS).toISOString());

// Auth cooldown does NOT clear early from quota snapshots; clears only when cooldown expires.
const openaiAuthState = route({
  reason: "retry",
  failed: OPENAI,
  failedError: "Your authentication token has expired",
  usable: [OPENAI, CLAUDE_W],
}).state;
check("auth failure steps to next provider", openaiAuthState?.model, CLAUDE_W);
const newerOpenAIQuota = parseQuotaSnapshot(
  JSON.stringify({
    generated_at: new Date(now + 120_000).toISOString(),
    openai: { headroom_pct: 100, fresh: true, at: new Date(now + 60_000).toISOString(), tight_resets_at: new Date(now + 7200_000).toISOString() },
  }),
  now + 120_000,
  cfg.quota_max_age_seconds * 1000,
);
const openaiFirstCfg = parseRouterConfig(JSON.stringify({ worker_models: [OPENAI, CLAUDE_W] }));
check(
  "auth cooldown ignores fresh quota reading before expiry",
  route({ config: openaiFirstCfg, nowMs: now + 120_000, quota: newerOpenAIQuota, state: openaiAuthState }).model,
  CLAUDE_W,
);
check(
  "auth cooldown clears after expiry",
  route({ config: openaiFirstCfg, nowMs: now + DEFAULT_AUTH_COOLDOWN_MS + 1000, quota: newerOpenAIQuota, state: openaiAuthState }).model,
  OPENAI,
);

// Explicit worker_models / coordinator_models config parsing.
const customChainCfg = parseRouterConfig(
  JSON.stringify({
    worker_models: [AGY, "bad-model", CLAUDE_W, AGY],
    coordinator_models: [CLAUDE_C, OPENAI],
  }),
);
check("custom worker_models filters bad and dedupes", customChainCfg.worker_models, [AGY, CLAUDE_W]);
check("custom coordinator_models", customChainCfg.coordinator_models, [CLAUDE_C, OPENAI]);

// Extension integration: quartermaster.ts virtual model route() thinkingLevel and usable filtering.
const { default: registerQuartermaster } = await import("../pi/quartermaster.ts");
const virtualModels = new Map();
const fakePi = {
  registerFlag() {},
  registerCommand() {},
  getFlag: () => "",
  on() {},
  appendEntry() {},
  sendMessage() {},
  sendUserMessage() {},
  events: { emit() {} },
  registerVirtualModel(vm) {
    virtualModels.set(vm.provider + "/" + vm.id, vm);
  },
};
registerQuartermaster(fakePi);
const workerVM = virtualModels.get("quartermaster/worker-auto");
const coordVM = virtualModels.get("quartermaster/coordinator-auto");
check("worker-auto registered", !!workerVM, true);
check("coordinator-auto registered", !!coordVM, true);

const catalog = [
  { provider: "antigravity", id: "gemini-4-neon-xhigh", reasoning: false },
  { provider: "anthropic", id: "claude-opus-5-5", reasoning: true },
  { provider: "anthropic", id: "claude-fable-5-2", reasoning: true },
  { provider: "openai-codex", id: "gpt-6.2-astra", reasoning: true },
];
const makeCtx = (availRefs) => {
  const avail = catalog.filter((m) => availRefs.includes(m.provider + "/" + m.id));
  return {
    modelRegistry: {
      find: (p, id) => catalog.find((m) => m.provider === p && m.id === id) || null,
      getAvailable: () => avail,
    },
  };
};

const fullCtx = makeCtx([AGY, CLAUDE_W, CLAUDE_C, OPENAI]);
const extW1 = workerVM.route({ reason: "user", thinkingLevel: "xhigh" }, fullCtx);
check("extension worker turn 1 picks agy", extW1.model.provider + "/" + extW1.model.id, AGY);
check("extension worker turn 1 passes thinkingLevel off for non-reasoning agy", extW1.thinkingLevel, "off");

const extC1 = coordVM.route({ reason: "user", thinkingLevel: "xhigh" }, fullCtx);
check("extension coordinator turn 1 picks agy", extC1.model.provider + "/" + extC1.model.id, AGY);
check("extension coordinator turn 1 passes thinkingLevel off for non-reasoning agy", extC1.thinkingLevel, "off");

const extCRetry = coordVM.route(
  {
    reason: "retry",
    thinkingLevel: "high",
    failed: { model: extC1.model, message: { errorMessage: "agy rate limit (ResourceExhausted): Antigravity quota exhausted; resets in 1h" } },
    state: extC1.state,
  },
  fullCtx,
);
check("extension coordinator retry steps to fable", extCRetry.model.provider + "/" + extCRetry.model.id, CLAUDE_C);
check("extension coordinator retry keeps thinkingLevel for reasoning fable", extCRetry.thinkingLevel, "high");

const noAgyCtx = makeCtx([CLAUDE_W, OPENAI]);
const extNoAgy = workerVM.route({ reason: "user" }, noAgyCtx);
check("extension skips unavailable agy and picks claude", extNoAgy.model.provider + "/" + extNoAgy.model.id, CLAUDE_W);
check("extension defaults thinkingLevel xhigh for reasoning claude", extNoAgy.thinkingLevel, "xhigh");

let emptyErr = "";
try {
  workerVM.route({ reason: "user" }, makeCtx([]));
} catch (e) {
  emptyErr = String(e?.message || e);
}
check("extension throws actionable error when no model is usable", emptyErr.includes("antigravity/gemini-4-neon-xhigh"), true);

// Neutral virtual-model display labels when no router.json flag is set (never hardcoded to Antigravity/Claude/OpenAI).
check("neutral worker-auto label without router.json", workerVM.name, "Worker auto (ordered failover)");
check("neutral coordinator-auto label without router.json", coordVM.name, "Coordinator auto (ordered failover)");
check("formatVirtualModelName neutral on null", formatVirtualModelName("worker", null), "Worker auto (ordered failover)");
check(
  "formatVirtualModelName dynamic order + deduped providers",
  formatVirtualModelName(
    "coordinator",
    parseRouterConfig(
      JSON.stringify({
        coordinator_models: ["openai-codex/gpt-6.2-astra:xhigh", "anthropic/claude-fable-5-2:high", "anthropic/claude-opus-5-5:low"],
      }),
    ),
  ),
  "Coordinator auto (openai-codex -> anthropic)",
);

// Per-model :thinking parsing, validation, and canonical deduplication (first wins).
check("valid thinking levels include off..xhigh", VALID_THINKING_LEVELS.includes("minimal") && VALID_THINKING_LEVELS.includes("xhigh"), true);
check("isValidThinkingLevel rejects bogus", isValidThinkingLevel("turbo"), false);
check("parseModelSpec plain", parseModelSpec("anthropic/claude-opus-5-5"), {
  ref: "anthropic/claude-opus-5-5",
  provider: "anthropic",
  rawProvider: "anthropic",
  id: "claude-opus-5-5",
  thinkingLevel: null,
  raw: "anthropic/claude-opus-5-5",
});
check("parseModelSpec with :thinking", parseModelSpec("anthropic/claude-opus-5-5:high"), {
  ref: "anthropic/claude-opus-5-5",
  provider: "anthropic",
  rawProvider: "anthropic",
  id: "claude-opus-5-5",
  thinkingLevel: "high",
  raw: "anthropic/claude-opus-5-5:high",
});
check("parseModelSpec nested slash + thinking", parseModelSpec("openrouter/deepseek/r1:medium")?.id, "deepseek/r1");
check("parseModelSpec rejects invalid thinking", parseModelSpec("anthropic/claude-opus-5-5:turbo"), null);
check("parseModelSpec rejects empty thinking", parseModelSpec("anthropic/claude-opus-5-5:"), null);
check("parseModelSpec rejects uppercase thinking", parseModelSpec("anthropic/claude-opus-5-5:HIGH"), null);
check("parseModelSpec rejects double colon", parseModelSpec("anthropic/claude-opus-5-5:high:low"), null);
check("stripThinkingLevel", stripThinkingLevel("openai-codex/gpt-6.2-astra:xhigh"), "openai-codex/gpt-6.2-astra");
check("thinkingLevelOf", thinkingLevelOf("openai-codex/gpt-6.2-astra:xhigh"), "xhigh");

const thinkingCfg = parseRouterConfig(
  JSON.stringify({
    worker_models: [
      "anthropic/claude-opus-5-5:high",
      "anthropic/claude-opus-5-5:low",
      "openai-codex/gpt-6.2-astra:xhigh",
      "anthropic/claude-opus-5-5:invalid",
    ],
    coordinator_models: [AGY + ":xhigh", CLAUDE_C + ":minimal", OPENAI + ":off"],
  }),
);
check("dedupe by canonical ref keeps first :thinking", thinkingCfg.worker_models, [
  "anthropic/claude-opus-5-5:high",
  "openai-codex/gpt-6.2-astra:xhigh",
]);
check("coordinator chain keeps :thinking", thinkingCfg.coordinator_models, [
  AGY + ":xhigh",
  CLAUDE_C + ":minimal",
  OPENAI + ":off",
]);
const decThinking1 = route({ config: thinkingCfg, quota: quota(null, 80, 90) });
check("decideRoute returns canonical model", decThinking1.model, CLAUDE_W);
check("decideRoute returns entry thinkingLevel", decThinking1.thinkingLevel, "high");
const decThinking2 = route({ config: thinkingCfg, quota: quota(null, 10, 90) });
check("decideRoute failover returns next entry thinkingLevel", decThinking2.model, OPENAI);
check("decideRoute failover thinkingLevel", decThinking2.thinkingLevel, "xhigh");
const decThinkingRetry = route({
  config: thinkingCfg,
  reason: "retry",
  failed: CLAUDE_W,
  failedError: "429 rate limit",
  quota: quota(null, 80, 90),
});
check("decideRoute retry skips canonical failed even with :thinking in chain", decThinkingRetry.model, OPENAI);
check("decideRoute retry carries next entry thinkingLevel", decThinkingRetry.thinkingLevel, "xhigh");

// Per-provider thresholds across gating, recovery, window-reset, cooldown recovery, and alias matching.
const provCfg = parseRouterConfig(
  JSON.stringify({
    worker_models: [AGY, CLAUDE_W, OPENAI],
    threshold_pct: 20,
    recover_pct: 30,
    providers: {
      antigravity: { threshold_pct: 10, recover_pct: 25 },
      anthropic: { threshold_pct: 35, recover_pct: 50 },
      openai: { threshold_pct: 5, recover_pct: 15 },
    },
  }),
);
check("thresholdsFor exact provider", thresholdsFor(provCfg, AGY), { threshold_pct: 10, recover_pct: 25 });
check("thresholdsFor exact anthropic", thresholdsFor(provCfg, CLAUDE_W), { threshold_pct: 35, recover_pct: 50 });
check("thresholdsFor openai alias applies to openai-codex", thresholdsFor(provCfg, OPENAI), { threshold_pct: 5, recover_pct: 15 });
check("thresholdsFor unknown falls back to global", thresholdsFor(provCfg, "google/gemini-3-pro"), { threshold_pct: 20, recover_pct: 30 });

// Antigravity threshold=10 (global is 20): at 15% Antigravity stays healthy!
check("per-provider threshold 10 keeps agy at 15%", route({ config: provCfg, quota: quota(15, 80, 80) }).model, AGY);
// Antigravity at 10% switches; Claude threshold=35: at 30% Claude is ALSO low, so steps straight to OpenAI!
const provFail = route({ config: provCfg, quota: quota(10, 30, 10) });
check("per-provider thresholds skip low agy (10<=10) and low claude (30<=35) to healthy openai (10>5)", provFail.model, OPENAI);
// Recovery uses Antigravity's recover=25 (not global 30):
check("per-provider recover 25 holds off agy at 24%", route({ config: provCfg, quota: quota(24, 80, 80), state: provFail.state }).model, CLAUDE_W);
check("per-provider recover 25 recovers agy at 25%", route({ config: provCfg, quota: quota(25, 80, 80), state: provFail.state }).model, AGY);
// Window reset uses Antigravity's threshold=10 (so 12% > 10% recovers on window reset!):
const provResetState = { ...provFail.state, low_quota: { antigravity: { tight_resets_at: new Date(now - 1000).toISOString() } } };
check("per-provider window reset uses provider threshold 10 (12% recovers)", route({ config: provCfg, quota: quota(12, 80, 80), state: provResetState }).model, AGY);
// Cooldown recovery uses Antigravity's recover=25 (not global 30):
const provCooldown = route({
  config: provCfg,
  reason: "retry",
  failed: AGY,
  failedError: "agy rate limit (ResourceExhausted): resets in 2h",
  quota: quota(80, 80, 80),
});
const newerAgy25 = parseQuotaSnapshot(
  JSON.stringify({
    generated_at: new Date(now + 120_000).toISOString(),
    antigravity: { headroom_pct: 25, fresh: true, at: new Date(now + 60_000).toISOString(), tight_resets_at: new Date(now + 7200_000).toISOString() },
    claude: { headroom_pct: 80, fresh: true, at: new Date(now).toISOString(), tight_resets_at: new Date(now + 7200_000).toISOString() },
  }),
  now + 120_000,
  provCfg.quota_max_age_seconds * 1000,
);
check("per-provider cooldown clears at provider recover 25%", route({ config: provCfg, nowMs: now + 120_000, quota: newerAgy25, state: provCooldown.state }).model, AGY);

// Generic extensible provider quota lookup (google, vertex, openrouter).
const genericCfg = parseRouterConfig(
  JSON.stringify({
    worker_models: ["google/gemini-3-pro:high", "vertex/gemini-3-pro:medium", "openrouter/deepseek/r1:low"],
    threshold_pct: 20,
    recover_pct: 30,
    providers: { google: { threshold_pct: 15, recover_pct: 25 } },
  }),
);
const genericQuota = parseQuotaSnapshot(
  JSON.stringify({
    generated_at: new Date(now).toISOString(),
    antigravity: null,
    claude: null,
    openai: null,
    google: { headroom_pct: 14, fresh: true, at: new Date(now - 30_000).toISOString(), tight_resets_at: new Date(now + 3600_000).toISOString() },
    vertex: { headroom_pct: 75, fresh: true, at: new Date(now - 30_000).toISOString(), tight_resets_at: new Date(now + 3600_000).toISOString() },
    openrouter: { headroom_pct: 90, fresh: true, at: new Date(now - 30_000).toISOString(), tight_resets_at: null },
  }),
  now,
  genericCfg.quota_max_age_seconds * 1000,
);
check("parseQuotaSnapshot includes google", genericQuota.google?.headroom, 14);
check("parseQuotaSnapshot includes vertex", genericQuota.vertex?.headroom, 75);
check("parseQuotaSnapshot includes openrouter", genericQuota.openrouter?.headroom, 90);
const gDec = route({ config: genericCfg, quota: genericQuota });
check("generic provider low google switches to vertex", gDec.model, "vertex/gemini-3-pro");
check("generic provider carries vertex :thinking", gDec.thinkingLevel, "medium");
check("generic provider records google in low_quota", !!gDec.state?.low_quota?.google, true);
const genericQuotaRecovered = parseQuotaSnapshot(
  JSON.stringify({
    generated_at: new Date(now + 60_000).toISOString(),
    google: { headroom_pct: 25, fresh: true, at: new Date(now + 30_000).toISOString(), tight_resets_at: new Date(now + 3600_000).toISOString() },
    vertex: { headroom_pct: 75, fresh: true, at: new Date(now + 30_000).toISOString(), tight_resets_at: new Date(now + 3600_000).toISOString() },
  }),
  now + 60_000,
  genericCfg.quota_max_age_seconds * 1000,
);
const gRec = route({ config: genericCfg, nowMs: now + 60_000, quota: genericQuotaRecovered, state: gDec.state });
check("generic provider google recovers at its 25% threshold", gRec.model, "google/gemini-3-pro");
check("generic provider google carries :high thinking", gRec.thinkingLevel, "high");

// Extension integration with real router.json file: dynamic labels, session_start refresh,
// per-model :thinking overrides, non-reasoning safety, and live reload across turns!
const tmpDir = mkdtempSync(path.join(os.tmpdir(), "qm-router-test-"));
try {
  const routerPath = path.join(tmpDir, "router.json");
  writeFileSync(
    routerPath,
    JSON.stringify({
      worker_models: [AGY + ":xhigh", CLAUDE_W + ":high", OPENAI + ":off"],
      coordinator_models: [OPENAI + ":medium", CLAUDE_C],
      threshold_pct: 20,
      recover_pct: 30,
    }),
  );

  // Case A: flag available at session_start (simulating real pi CLI flag parsing order).
  let flagValue = "";
  const vms = new Map();
  const handlers = new Map();
  const fakePi2 = {
    registerFlag() {},
    registerCommand() {},
    getFlag: (name) => (name === "quartermaster-router" ? flagValue : ""),
    on(event, fn) {
      handlers.set(event, fn);
    },
    appendEntry() {},
    sendMessage() {},
    sendUserMessage() {},
    events: { emit() {} },
    registerVirtualModel(vm) {
      vms.set(vm.provider + "/" + vm.id, vm);
    },
  };
  registerQuartermaster(fakePi2);
  check("before flags parsed: neutral worker label", vms.get("quartermaster/worker-auto").name, "Worker auto (ordered failover)");
  flagValue = routerPath;
  handlers.get("session_start")({}, { mode: "interactive" });
  check(
    "after session_start: dynamic worker label from router.json",
    vms.get("quartermaster/worker-auto").name,
    "Worker auto (antigravity -> anthropic -> openai-codex)",
  );
  check(
    "after session_start: dynamic coordinator label from router.json",
    vms.get("quartermaster/coordinator-auto").name,
    "Coordinator auto (openai-codex -> anthropic)",
  );

  const wvm = vms.get("quartermaster/worker-auto");
  const cvm = vms.get("quartermaster/coordinator-auto");

  // Non-reasoning Antigravity configured with :xhigh STILL gets "off"!
  const rAgy = wvm.route({ reason: "user", thinkingLevel: "xhigh" }, fullCtx);
  check("non-reasoning agy with :xhigh still returns off", rAgy.thinkingLevel, "off");

  // Retry failover from non-reasoning Antigravity to reasoning Claude configured with :high returns "high" (overriding session "xhigh")!
  const rFailToClaude = wvm.route(
    {
      reason: "retry",
      thinkingLevel: "xhigh",
      failed: {
        model: rAgy.model,
        thinkingLevel: "off",
        message: { errorMessage: "agy rate limit (ResourceExhausted): resets in 1h" },
      },
      state: rAgy.state,
    },
    fullCtx,
  );
  check("retry failover picks claude", rFailToClaude.model.provider + "/" + rFailToClaude.model.id, CLAUDE_W);
  check("per-model :high overrides session xhigh", rFailToClaude.thinkingLevel, "high");

  // Continuation preserves previous model and thinkingLevel ("high").
  const rCont = wvm.route(
    {
      reason: "continuation",
      thinkingLevel: "xhigh",
      previous: { model: rFailToClaude.model, thinkingLevel: "high" },
      state: rFailToClaude.state,
    },
    fullCtx,
  );
  check("continuation keeps model", rCont.model.provider + "/" + rCont.model.id, CLAUDE_W);
  check("continuation keeps thinkingLevel high", rCont.thinkingLevel, "high");

  // Retry failover from Claude to OpenAI configured with :off returns "off" even though OpenAI is reasoning!
  const rFailToOpenAI = wvm.route(
    {
      reason: "retry",
      thinkingLevel: "xhigh",
      failed: {
        model: rFailToClaude.model,
        thinkingLevel: "high",
        message: { errorMessage: "429 rate limit" },
      },
      state: rFailToClaude.state,
    },
    fullCtx,
  );
  check("retry failover picks openai", rFailToOpenAI.model.provider + "/" + rFailToOpenAI.model.id, OPENAI);
  check("per-model :off applies to reasoning openai", rFailToOpenAI.thinkingLevel, "off");

  // Coordinator model without :thinking suffix falls back to request.thinkingLevel; with :medium uses medium.
  const rCoord1 = cvm.route({ reason: "user", thinkingLevel: "xhigh" }, fullCtx);
  check("coordinator picks openai with :medium", rCoord1.model.provider + "/" + rCoord1.model.id, OPENAI);
  check("coordinator uses :medium override", rCoord1.thinkingLevel, "medium");
  const rCoordNoOpenAI = cvm.route({ reason: "user", thinkingLevel: "low" }, makeCtx([CLAUDE_C]));
  check("coordinator falls back to request.thinkingLevel when no suffix", rCoordNoOpenAI.thinkingLevel, "low");

  // Live reload: rewrite router.json on disk and confirm next turn immediately routes to new primary + thinking!
  writeFileSync(
    routerPath,
    JSON.stringify({
      worker_models: [OPENAI + ":minimal", CLAUDE_W],
      coordinator_models: [CLAUDE_C + ":high"],
      threshold_pct: 20,
      recover_pct: 30,
    }),
  );
  const rLive = wvm.route({ reason: "user", thinkingLevel: "xhigh" }, fullCtx);
  check("live reload picks new primary openai", rLive.model.provider + "/" + rLive.model.id, OPENAI);
  check("live reload applies new :minimal thinking", rLive.thinkingLevel, "minimal");

  // Live reload changing ONLY :thinking on the same physical model applies on the next user turn (not stuck on previous.thinkingLevel)!
  writeFileSync(
    routerPath,
    JSON.stringify({
      worker_models: [OPENAI + ":xhigh", CLAUDE_W],
      coordinator_models: [CLAUDE_C + ":high"],
      threshold_pct: 20,
      recover_pct: 30,
    }),
  );
  const rLiveSameModel = wvm.route(
    { reason: "user", thinkingLevel: "low", previous: { model: rLive.model, thinkingLevel: "minimal" }, state: rLive.state },
    fullCtx,
  );
  check("live reload same model applies updated :xhigh on new user turn", rLiveSameModel.thinkingLevel, "xhigh");
} finally {
  rmSync(tmpDir, { recursive: true, force: true });
}

console.log("pi-router: " + passed + " passed");
