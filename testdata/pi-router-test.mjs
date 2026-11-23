// Offline tests for pi/router.ts, run by pirouter_test.go under node:
// node testdata/pi-router-test.mjs. No pi, no network: the router decision
// is pure. Exits nonzero on the first failed case.
const {
  DEFAULT_AUTH_COOLDOWN_MS,
  DEFAULT_QUOTA_COOLDOWN_MS,
  authFailure,
  decideRoute,
  failoverKind,
  parseQuotaSnapshot,
  parseResetDurationMs,
  parseRouterConfig,
  quotaFailure,
  quotaKeyForModel,
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

// Provider quota mapping.
check("antigravity quota key", quotaKeyForModel(AGY), "antigravity");
check("anthropic quota key", quotaKeyForModel(CLAUDE_W), "claude");
check("openai-codex quota key", quotaKeyForModel(OPENAI), "openai");
check("openai quota key", quotaKeyForModel("openai/gpt-6.2-astra"), "openai");
check("unknown provider quota key", quotaKeyForModel("meta/muse-wonder-full-1.0"), null);

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

console.log("pi-router: " + passed + " passed");
