// Offline tests for pi/router.ts, run by pirouter_test.go under node:
// node testdata/pi-router-test.mjs. No pi, no network: the router decision
// is pure. Exits nonzero on the first failed case.
const { decideRoute, parseQuotaSnapshot, parseRouterConfig, quotaFailure } = await import("../pi/router.ts");

const now = Date.parse("2026-11-23T17:00:00Z");
const cfg = parseRouterConfig("{}");
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

function quota(claudeLeft, openaiLeft, opts = {}) {
  const provider = (left) =>
    left === null
      ? null
      : {
          headroom_pct: left,
          fresh: opts.fresh !== false,
          at: new Date(now - (opts.ageMs ?? 60_000)).toISOString(),
          tight_resets_at: opts.resetsAt ?? new Date(now + 3600_000).toISOString(),
        };
  return parseQuotaSnapshot(
    JSON.stringify({ generated_at: new Date(now).toISOString(), claude: provider(claudeLeft), openai: provider(openaiLeft) }),
    now,
    cfg.quota_max_age_seconds * 1000,
  );
}

const base = { config: cfg, nowMs: now, reason: "user", previous: null, failed: null, failedError: "", state: null };
const route = (over) => decideRoute({ ...base, role: "worker", quota: quota(null, null), ...over });

// Role mapping: worker defaults to Opus, coordinator to Fable.
check("worker defaults to its Claude model", route({}).model, CLAUDE_W);
check("coordinator defaults to its Claude model", route({ role: "coordinator" }).model, CLAUDE_C);

// Missing, stale and malformed quota data stays on Claude.
check("no quota file", route({ quota: parseQuotaSnapshot("", now, 1000) }).model, CLAUDE_W);
check("malformed quota", route({ quota: parseQuotaSnapshot("{nope", now, 1000) }).model, CLAUDE_W);
check("stale claude reading", route({ quota: quota(0, 90, { ageMs: 7 * 3600_000 }) }).model, CLAUDE_W);
check("unfresh claude reading", route({ quota: quota(0, 90, { fresh: false }) }).model, CLAUDE_W);

// Threshold: at or below 20 switches, above stays.
check("claude at threshold switches", route({ quota: quota(20, 80) }).model, OPENAI);
check("claude just above threshold stays", route({ quota: quota(20.5, 80) }).model, CLAUDE_W);
check("claude low, no openai reading, switches", route({ quota: quota(5, null) }).model, OPENAI);
check("switch records state", route({ quota: quota(10, 80) }).state?.side, "openai");
check("switch records why", route({ quota: quota(10, 80) }).why, "openai:claude-low");

// Both low: the provider with more headroom wins, with the reason recorded.
check("both low, openai lower, stays claude", route({ quota: quota(15, 10) }).model, CLAUDE_W);
check("both low reason", route({ quota: quota(15, 10) }).why, "claude:both-low-more-headroom");
check("both low, tied, switches", route({ quota: quota(15, 15) }).model, OPENAI);

// Hysteresis from branch state: hold OpenAI below recover, back at 30.
const onOpenAI = route({ quota: quota(10, 80) }).state;
check("holds openai at 25", route({ quota: quota(25, 80), state: onOpenAI }).model, OPENAI);
check("no state churn while holding", route({ quota: quota(25, 80), state: onOpenAI }).state, undefined);
check("back to claude at recover", route({ quota: quota(30, 80), state: onOpenAI }).model, CLAUDE_W);
check("holds openai with no fresh quota", route({ quota: quota(null, null), state: onOpenAI }).model, OPENAI);
const resetState = { ...onOpenAI, tight_resets_at: new Date(now - 1000).toISOString() };
check("back after tight window reset", route({ quota: quota(25, 80), state: resetState }).model, CLAUDE_W);
check("window reset does not beat a still-low reading", route({ quota: quota(10, 80), state: resetState }).model, OPENAI);

// Sticky within a turn, whatever quota says.
check(
  "continuation keeps previous",
  route({ reason: "continuation", previous: CLAUDE_W, quota: quota(0, 90) }).model,
  CLAUDE_W,
);
check("direct keeps previous", route({ reason: "direct", previous: OPENAI, quota: quota(90, 0) }).model, OPENAI);
check("direct without previous routes by quota", route({ reason: "direct", quota: quota(0, 90) }).model, OPENAI);
check("direct keeps no state", route({ reason: "direct", quota: quota(0, 90) }).state, undefined);

// Retries keep the failed model, except quota-class failures from Claude.
check(
  "retry keeps failed model",
  route({ reason: "retry", failed: CLAUDE_W, failedError: "connection reset" }).model,
  CLAUDE_W,
);
check(
  "retry after 429 switches to openai",
  route({ reason: "retry", failed: CLAUDE_W, failedError: "429 Too Many Requests: rate limit exceeded" }).model,
  OPENAI,
);
check(
  "retry after usage limit switches",
  route({ reason: "retry", failed: CLAUDE_C, failedError: "You have reached your usage limit" }).model,
  OPENAI,
);
check(
  "retry after openai quota failure switches back",
  route({ reason: "retry", failed: OPENAI, failedError: "quota exceeded" }).model,
  CLAUDE_W,
);
check("quotaFailure ignores context overflow", quotaFailure("context window exceeded"), false);
check("quotaFailure catches overloaded", quotaFailure("529 Overloaded"), true);

// Config parsing: overrides apply, junk falls back, recover clamps up.
const custom = parseRouterConfig(
  JSON.stringify({ worker_model: "anthropic/claude-sonnet-5-5", threshold_pct: 40, recover_pct: 10, fallback_model: "bad" }),
);
check("custom worker model", custom.worker_model, "anthropic/claude-sonnet-5-5");
check("bad fallback falls back", custom.fallback_model, OPENAI);
check("recover clamped to threshold", custom.recover_pct, 40);
check(
  "custom threshold routes",
  route({ config: custom, quota: quota(35, 90) }).model,
  OPENAI,
);

console.log("pi-router: " + passed + " passed");
