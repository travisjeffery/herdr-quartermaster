// The quota router behind quartermaster's pi virtual models
// (quartermaster/worker-auto and quartermaster/coordinator-auto), registered
// by pi/quartermaster.ts. This module is pure: it parses the two JSON files
// quartermaster writes (router.json: physical models and thresholds;
// quota.json: normalized subscription headroom from quota.go) and decides
// the physical model for one request. The extension does the file IO and
// the model-registry lookup, so the decision is testable offline
// (testdata/pi-router-test.mjs, run by pirouter_test.go).
//
// Routing, at turn boundaries only:
//   - Default to the role's Claude model.
//   - A fresh Claude reading at or below the threshold (default 20% left in
//     the tightest live window) routes new turns to the OpenAI fallback,
//     unless a fresh OpenAI reading has even less headroom.
//   - Once on OpenAI, stay there until a fresh Claude reading recovers to
//     the recover level (default 30%) or the tight window that caused the
//     switch has reset. The choice is router state on the session branch.
//   - Continuations keep the previous physical model; retries keep the
//     failed one unless it failed on quota, rate limits or provider
//     unavailability, when the retry may use the other provider.
//   - Missing, stale or malformed quota data keeps Claude and never throws.
//
// @ts-nocheck

export const ROUTER_DEFAULTS = {
  worker_model: "anthropic/claude-opus-5-5",
  coordinator_model: "anthropic/claude-fable-5-2",
  fallback_model: "openai-codex/gpt-6.2-astra",
  threshold_pct: 20,
  recover_pct: 30,
  quota_max_age_seconds: 6 * 3600,
};

const MODEL_RE = /^[^/@\s]+\/[^/@\s]+$/;

function modelOr(v, dflt) {
  return typeof v === "string" && MODEL_RE.test(v) ? v : dflt;
}

function pctOr(v, dflt) {
  return typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 100 ? v : dflt;
}

// parseRouterConfig is router.json with defaults for anything missing or
// malformed; a bad file routes on the defaults rather than failing.
export function parseRouterConfig(text) {
  let raw = {};
  try {
    raw = JSON.parse(text || "{}") || {};
  } catch {
    raw = {};
  }
  const cfg = {
    worker_model: modelOr(raw.worker_model, ROUTER_DEFAULTS.worker_model),
    coordinator_model: modelOr(raw.coordinator_model, ROUTER_DEFAULTS.coordinator_model),
    fallback_model: modelOr(raw.fallback_model, ROUTER_DEFAULTS.fallback_model),
    threshold_pct: pctOr(raw.threshold_pct, ROUTER_DEFAULTS.threshold_pct),
    recover_pct: pctOr(raw.recover_pct, ROUTER_DEFAULTS.recover_pct),
    quota_max_age_seconds:
      typeof raw.quota_max_age_seconds === "number" && raw.quota_max_age_seconds > 0
        ? raw.quota_max_age_seconds
        : ROUTER_DEFAULTS.quota_max_age_seconds,
  };
  if (cfg.recover_pct < cfg.threshold_pct) cfg.recover_pct = cfg.threshold_pct;
  return cfg;
}

function providerReading(raw, nowMs, maxAgeMs) {
  if (!raw || typeof raw !== "object") return null;
  const at = typeof raw.at === "string" ? Date.parse(raw.at) : NaN;
  const left = raw.headroom_pct;
  if (!Number.isFinite(at) || typeof left !== "number" || !Number.isFinite(left)) return null;
  const resets = typeof raw.tight_resets_at === "string" ? Date.parse(raw.tight_resets_at) : NaN;
  return {
    headroom: Math.min(100, Math.max(0, left)),
    atMs: at,
    // fresh mirrors quota.go: the reading is recent enough to trust. The
    // snapshot's own fresh flag is computed when quartermaster wrote it;
    // the age is checked again here against the request's clock.
    fresh: raw.fresh === true && nowMs - at <= maxAgeMs && at - nowMs <= 5 * 60 * 1000,
    tightResetsAtMs: Number.isFinite(resets) ? resets : null,
  };
}

// parseQuotaSnapshot is quota.json as {claude, openai} readings, each null
// when absent or malformed.
export function parseQuotaSnapshot(text, nowMs, maxAgeMs) {
  let raw = null;
  try {
    raw = JSON.parse(text || "null");
  } catch {
    raw = null;
  }
  if (!raw || typeof raw !== "object") return { claude: null, openai: null };
  return {
    claude: providerReading(raw.claude, nowMs, maxAgeMs),
    openai: providerReading(raw.openai, nowMs, maxAgeMs),
  };
}

// quotaFailure reports whether a failed request's error looks like quota,
// rate limiting or provider unavailability, the failures a retry may switch
// provider for. Context overflow is deliberately not one: compaction fixes
// it on the same model.
export function quotaFailure(errorText) {
  if (typeof errorText !== "string" || errorText === "") return false;
  return /\b(429|529)\b|rate[ _-]?limit|quota|usage limit|too many requests|overloaded|service unavailable|provider.{0,30}unavailable/i.test(
    errorText,
  );
}

// A new state is worth storing only when the routed side changes (or
// there is none yet): pi stores every returned object on the branch, and
// headroom jitter between turns is not a routing decision.
function stateChanged(prev, next) {
  return !prev || typeof prev !== "object" || prev.side !== next.side;
}

// decideRoute picks the physical model ("provider/id") for one request.
//
// input: {
//   role: "worker" | "coordinator",
//   config, quota, nowMs,
//   reason: "user" | "continuation" | "retry" | "direct",
//   previous: "provider/id" | null,   // latest successful physical model
//   failed: "provider/id" | null,     // for retry: the failed physical model
//   failedError: string,              // for retry: its error text
//   state: router state | null,       // branch state from the last route
// }
// Returns {model, why, state?}: state is set only when it changed, so the
// caller can return it (or undefined) as pi's docs ask. State records the
// routed provider side, why, the headrooms it saw, and the tight Claude
// window's reset, so quartermaster can read the reason back off the branch.
export function decideRoute(input) {
  const cfg = input.config || ROUTER_DEFAULTS;
  const claudeModel = input.role === "coordinator" ? cfg.coordinator_model : cfg.worker_model;
  const fallbackModel = cfg.fallback_model;
  const quota = input.quota || { claude: null, openai: null };
  const state = input.state && typeof input.state === "object" ? input.state : null;
  const nowMs = typeof input.nowMs === "number" ? input.nowMs : Date.now();

  // Sticky within a turn: tool follow-ups stay on the model that is
  // handling the turn, keeping its prompt cache and reasoning state.
  if (input.reason === "continuation" && input.previous) {
    return { model: input.previous, why: "sticky:continuation" };
  }
  if (input.reason === "retry" && input.failed) {
    if (quotaFailure(input.failedError)) {
      const toFallback = input.failed === claudeModel || input.failed.startsWith("anthropic/");
      const model = toFallback ? fallbackModel : claudeModel;
      const next = {
        side: toFallback ? "openai" : "claude",
        why: "retry:quota-failure",
        at: new Date(nowMs).toISOString(),
        claude_headroom: quota.claude?.fresh ? quota.claude.headroom : null,
        openai_headroom: quota.openai?.fresh ? quota.openai.headroom : null,
        tight_resets_at: state?.tight_resets_at ?? null,
      };
      return { model, why: next.why, state: stateChanged(state, next) ? next : undefined };
    }
    return { model: input.failed, why: "sticky:retry" };
  }
  // Direct requests (compaction summaries, extension calls) have no branch
  // state; follow the turn's model when there is one.
  if (input.reason === "direct" && input.previous) {
    return { model: input.previous, why: "sticky:direct" };
  }

  const cl = quota.claude?.fresh ? quota.claude : null;
  const ox = quota.openai?.fresh ? quota.openai : null;
  let side = "claude";
  let why = cl ? "claude:headroom-ok" : "claude:no-fresh-quota";
  if (state?.side === "openai") {
    // Hysteresis: back only on recovery, or once the tight window that
    // caused the switch has reset and a fresh reading is above threshold.
    const resetMs = typeof state.tight_resets_at === "string" ? Date.parse(state.tight_resets_at) : NaN;
    const windowReset = Number.isFinite(resetMs) && nowMs >= resetMs && cl !== null && cl.headroom > cfg.threshold_pct;
    if (cl && cl.headroom >= cfg.recover_pct) {
      why = "claude:recovered";
    } else if (windowReset) {
      why = "claude:window-reset";
    } else {
      side = "openai";
      why = cl ? "openai:holding" : "openai:holding-no-fresh-quota";
    }
  } else if (cl && cl.headroom <= cfg.threshold_pct) {
    if (ox && ox.headroom < cl.headroom) {
      why = "claude:both-low-more-headroom";
    } else {
      side = "openai";
      why = ox ? "openai:claude-low" : "openai:claude-low-no-openai-quota";
    }
  }

  const next = {
    side,
    why,
    at: new Date(nowMs).toISOString(),
    claude_headroom: cl ? cl.headroom : null,
    openai_headroom: ox ? ox.headroom : null,
    tight_resets_at:
      side === "openai"
        ? cl?.tightResetsAtMs != null
          ? new Date(cl.tightResetsAtMs).toISOString()
          : (state?.tight_resets_at ?? null)
        : null,
  };
  // State is branch state for agent-loop requests only; direct requests
  // neither read nor keep it.
  const keep = input.reason !== "direct" && stateChanged(state, next);
  return { model: side === "openai" ? fallbackModel : claudeModel, why, state: keep ? next : undefined };
}
