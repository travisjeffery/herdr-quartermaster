// The quota router behind quartermaster's pi virtual models
// (quartermaster/worker-auto and quartermaster/coordinator-auto), registered
// by pi/quartermaster.ts. This module is pure: it parses the two JSON files
// quartermaster writes (router.json: ordered physical model chains and
// thresholds; quota.json: normalized subscription headroom from quota.go)
// and decides the physical model for one request. The extension does the
// file IO and the model-registry lookup, so the decision is testable offline
// (testdata/pi-router-test.mjs, run by pirouter_test.go).
//
// Routing, at turn boundaries only:
//   - Walk the role's ordered physical model chain (default: Antigravity
//     Neon XHigh -> Claude -> OpenAI), optionally filtered to models usable
//     in pi.
//   - Each candidate is gated ONLY by its own provider's quota reading
//     (antigravity/* -> quota.antigravity, anthropic/* -> quota.claude,
//     openai*/* -> quota.openai) and its own runtime failure cooldown. Low
//     headroom on one provider never switches away from a different healthy
//     provider.
//   - A fresh provider reading at or below the threshold (default 20% left
//     in the tightest live window) steps to the next healthy candidate in
//     the chain and holds off the low provider until a fresh reading
//     recovers to recover_pct (default 30%) or its tight window resets.
//   - Continuations keep the previous physical model; retries keep the
//     failed one unless it failed on quota, rate limits, provider
//     unavailability, or expired/invalid auth tokens, when the retry puts
//     the failed provider in cooldown (parsing "resets in <duration>" when
//     present) and steps to the next healthy candidate.
//   - Missing, stale or malformed quota data never triggers a switch and
//     never throws.
//
// @ts-nocheck

export const ROUTER_DEFAULTS = {
  worker_models: [
    "antigravity/gemini-4-neon-xhigh",
    "anthropic/claude-opus-5-5",
    "openai-codex/gpt-6.2-astra",
  ],
  coordinator_models: [
    "antigravity/gemini-4-neon-xhigh",
    "anthropic/claude-fable-5-2",
    "openai-codex/gpt-6.2-astra",
  ],
  worker_model: "antigravity/gemini-4-neon-xhigh",
  coordinator_model: "antigravity/gemini-4-neon-xhigh",
  fallback_model: "openai-codex/gpt-6.2-astra",
  threshold_pct: 20,
  recover_pct: 30,
  quota_max_age_seconds: 6 * 3600,
};

export const DEFAULT_QUOTA_COOLDOWN_MS = 15 * 60 * 1000;
export const DEFAULT_AUTH_COOLDOWN_MS = 15 * 60 * 1000;
const MAX_RESET_DURATION_MS = 30 * 24 * 3600 * 1000;

const MODEL_RE = /^[^/@\s]+\/[^/@\s]+$/;

function modelOr(v, dflt) {
  return typeof v === "string" && MODEL_RE.test(v) ? v : dflt;
}

function dedupeModels(list) {
  const out = [];
  for (const m of list) {
    if (typeof m === "string" && MODEL_RE.test(m) && !out.includes(m)) out.push(m);
  }
  return out;
}

function pctOr(v, dflt) {
  return typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 100 ? v : dflt;
}

export function providerOf(ref) {
  if (typeof ref !== "string") return "";
  const i = ref.indexOf("/");
  return i > 0 ? ref.slice(0, i).toLowerCase() : "";
}

export function quotaKeyForModel(ref) {
  const p = providerOf(ref);
  if (p === "antigravity") return "antigravity";
  if (p === "anthropic") return "claude";
  if (p === "openai" || p.startsWith("openai-")) return "openai";
  return null;
}

function sideOf(ref) {
  return quotaKeyForModel(ref) || providerOf(ref) || "unknown";
}

function resolveRoleChain(rawChain, rawPrimary, fallback, defaultChain) {
  if (Array.isArray(rawChain)) {
    const parsed = dedupeModels(rawChain);
    if (parsed.length > 0) return parsed;
  }
  if (typeof rawPrimary === "string" && MODEL_RE.test(rawPrimary)) {
    return dedupeModels([rawPrimary, fallback]);
  }
  return dedupeModels([...defaultChain.slice(0, -1), fallback]);
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
  const fallbackModel = modelOr(raw.fallback_model, ROUTER_DEFAULTS.fallback_model);
  const workerModels = resolveRoleChain(
    raw.worker_models,
    raw.worker_model,
    fallbackModel,
    ROUTER_DEFAULTS.worker_models,
  );
  const coordinatorModels = resolveRoleChain(
    raw.coordinator_models,
    raw.coordinator_model,
    fallbackModel,
    ROUTER_DEFAULTS.coordinator_models,
  );
  const cfg = {
    worker_models: workerModels,
    coordinator_models: coordinatorModels,
    worker_model: workerModels[0],
    coordinator_model: coordinatorModels[0],
    fallback_model: fallbackModel,
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

// parseQuotaSnapshot is quota.json as {antigravity, claude, openai} readings,
// each null when absent or malformed.
export function parseQuotaSnapshot(text, nowMs, maxAgeMs) {
  let raw = null;
  try {
    raw = JSON.parse(text || "null");
  } catch {
    raw = null;
  }
  if (!raw || typeof raw !== "object") return { antigravity: null, claude: null, openai: null };
  return {
    antigravity: providerReading(raw.antigravity, nowMs, maxAgeMs),
    claude: providerReading(raw.claude, nowMs, maxAgeMs),
    openai: providerReading(raw.openai, nowMs, maxAgeMs),
  };
}

// quotaFailure reports whether a failed request's error looks like quota,
// rate limiting, ResourceExhausted, or provider unavailability, the failures
// a retry may switch provider for. Context overflow is deliberately not one:
// compaction fixes it on the same model.
export function quotaFailure(errorText) {
  if (typeof errorText !== "string" || errorText === "") return false;
  return /\b(429|529)\b|rate[ _-]?limit|resource[ _-]?exhausted|quota|usage[ _-]?limit|spend[ _-]?limit|too many requests|overloaded|service unavailable|provider.{0,30}unavailable/i.test(
    errorText,
  );
}

// authFailure reports whether a failed request's error looks like an expired
// or invalid authentication token / credential so a retry may fail over to
// the next provider in the chain.
export function authFailure(errorText) {
  if (typeof errorText !== "string" || errorText === "") return false;
  return (
    /\b401\b|\bunauthorized\b|authentication[ _-]?(error|failed)|invalid[ _-]?api[ _-]?key|re-?authenticate|sign in again/i.test(
      errorText,
    ) ||
    /\btoken\s+(?:has\s+)?expired\b/i.test(errorText) ||
    /\b(?:auth(?:entication)?|access|refresh|bearer|oauth|session|api)[ _-]?token\b[^.\n]{0,30}\b(?:expired|invalid|revoked)\b/i.test(
      errorText,
    ) ||
    /\b(?:expired|invalid|revoked)\s+(?:auth(?:entication)?|access|refresh|bearer|oauth|session|api)[ _-]?token\b/i.test(
      errorText,
    )
  );
}

// failoverKind classifies a retryable failover error as "quota", "auth", or
// null (not a failover error).
export function failoverKind(errorText) {
  if (quotaFailure(errorText)) return "quota";
  if (authFailure(errorText)) return "auth";
  return null;
}

// parseResetDurationMs extracts a reset duration from messages like
// "resets in 153h38m14s" or "reset in 2 hours 15 minutes".
export function parseResetDurationMs(errorText) {
  if (typeof errorText !== "string" || errorText === "") return null;
  const m = /\bresets?\s+in\s+([^\n.;,)]+)/i.exec(errorText);
  if (!m) return null;
  const unitMs = { d: 86400_000, h: 3600_000, m: 60_000, s: 1000 };
  let total = 0;
  let matched = false;
  const re = /(\d+(?:\.\d+)?)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)(?![a-zA-Z])/gi;
  let part;
  while ((part = re.exec(m[1])) !== null) {
    const n = Number(part[1]);
    const u = part[2][0].toLowerCase();
    if (!Number.isFinite(n) || n < 0 || !unitMs[u]) continue;
    total += n * unitMs[u];
    matched = true;
  }
  if (!matched || total <= 0) return null;
  return Math.min(Math.round(total), MAX_RESET_DURATION_MS);
}

function normalizeLowQuota(state) {
  const out = {};
  if (state?.low_quota && typeof state.low_quota === "object") {
    for (const [prov, entry] of Object.entries(state.low_quota)) {
      if (entry && typeof entry === "object") {
        out[prov] = {
          tight_resets_at: typeof entry.tight_resets_at === "string" ? entry.tight_resets_at : null,
        };
      }
    }
  } else if (state?.side === "openai") {
    // Backward compatibility with 2-provider router branch state.
    out.anthropic = {
      tight_resets_at: typeof state.tight_resets_at === "string" ? state.tight_resets_at : null,
    };
  }
  return out;
}

function normalizeCooldowns(state) {
  const out = {};
  if (state?.cooldowns && typeof state.cooldowns === "object") {
    for (const [prov, entry] of Object.entries(state.cooldowns)) {
      if (entry && typeof entry === "object" && typeof entry.until === "string") {
        out[prov] = {
          until: entry.until,
          at: typeof entry.at === "string" ? entry.at : entry.until,
          kind: entry.kind === "auth" ? "auth" : "quota",
        };
      }
    }
  }
  return out;
}

function sortedRecord(obj) {
  const out = {};
  for (const k of Object.keys(obj || {}).sort()) {
    out[k] = obj[k];
  }
  return out;
}

// A new state is worth storing only when the routed model/side or tracked
// provider hysteresis/cooldown state changes (or there is none yet): pi stores
// every returned object on the branch, and headroom jitter between turns is
// not a routing decision.
function stateChanged(prev, next) {
  if (!prev || typeof prev !== "object") return true;
  return (
    prev.model !== next.model ||
    prev.side !== next.side ||
    JSON.stringify(sortedRecord(prev.low_quota)) !== JSON.stringify(sortedRecord(next.low_quota)) ||
    JSON.stringify(sortedRecord(prev.cooldowns)) !== JSON.stringify(sortedRecord(next.cooldowns))
  );
}

function roleChain(cfg, role) {
  const list = role === "coordinator" ? cfg.coordinator_models : cfg.worker_models;
  if (Array.isArray(list) && list.length > 0) return list;
  const primary = role === "coordinator" ? cfg.coordinator_model : cfg.worker_model;
  return dedupeModels([primary, cfg.fallback_model]);
}

function freshReadingFor(quota, ref) {
  const key = quotaKeyForModel(ref);
  if (!key) return null;
  const r = quota?.[key];
  return r && r.fresh ? r : null;
}

function buildState(chosen, why, nowMs, quota, lowQuota, cooldowns) {
  const cleanedLow = sortedRecord(lowQuota);
  const cleanedCooldowns = sortedRecord(cooldowns);
  const firstReset =
    Object.values(cleanedLow).find((e) => typeof e?.tight_resets_at === "string")?.tight_resets_at ?? null;
  return {
    model: chosen,
    provider: providerOf(chosen),
    side: sideOf(chosen),
    why,
    at: new Date(nowMs).toISOString(),
    antigravity_headroom: quota.antigravity?.fresh ? quota.antigravity.headroom : null,
    claude_headroom: quota.claude?.fresh ? quota.claude.headroom : null,
    openai_headroom: quota.openai?.fresh ? quota.openai.headroom : null,
    low_quota: cleanedLow,
    cooldowns: cleanedCooldowns,
    tight_resets_at: firstReset,
  };
}

// decideRoute picks the physical model ("provider/id") for one request.
//
// input: {
//   role: "worker" | "coordinator",
//   config, quota, nowMs,
//   usable?: string[],                // optional subset of chain usable in pi
//   reason: "user" | "continuation" | "retry" | "direct",
//   previous: "provider/id" | null,   // latest successful physical model
//   failed: "provider/id" | null,     // for retry: the failed physical model
//   failedError: string,              // for retry: its error text
//   state: router state | null,       // branch state from the last route
// }
// Returns {model, why, state?}: state is set only when it changed, so the
// caller can return it (or undefined) as pi's docs ask.
export function decideRoute(input) {
  const cfg = input.config || ROUTER_DEFAULTS;
  const quota = input.quota || { antigravity: null, claude: null, openai: null };
  const state = input.state && typeof input.state === "object" ? input.state : null;
  const nowMs = typeof input.nowMs === "number" ? input.nowMs : Date.now();
  const fullChain = roleChain(cfg, input.role);
  const usableSet = Array.isArray(input.usable) && input.usable.length > 0 ? new Set(input.usable) : null;
  const candidates = usableSet ? fullChain.filter((m) => usableSet.has(m)) : fullChain;
  const activeChain = candidates.length > 0 ? candidates : fullChain;

  // Sticky within a turn: tool follow-ups stay on the model that is
  // handling the turn, keeping its prompt cache and reasoning state.
  if (input.reason === "continuation" && input.previous) {
    return { model: input.previous, why: "sticky:continuation" };
  }

  // Direct requests (compaction summaries, extension calls) have no branch
  // state; follow the turn's model when there is one.
  if (input.reason === "direct" && input.previous) {
    return { model: input.previous, why: "sticky:direct" };
  }

  const prevLowQuota = normalizeLowQuota(state);
  const nextCooldowns = normalizeCooldowns(state);

  // Clear expired cooldowns and quota cooldowns cleared by a strictly newer
  // fresh reading above recover_pct.
  const recoveredCooldownProviders = new Set();
  for (const [prov, cd] of Object.entries(nextCooldowns)) {
    const untilMs = Date.parse(cd.until);
    const atMs = Date.parse(cd.at);
    if (!Number.isFinite(untilMs) || nowMs >= untilMs) {
      delete nextCooldowns[prov];
      recoveredCooldownProviders.add(prov);
      continue;
    }
    if (cd.kind === "quota") {
      const sampleModel = activeChain.find((m) => providerOf(m) === prov);
      const reading = sampleModel ? freshReadingFor(quota, sampleModel) : null;
      if (reading && Number.isFinite(atMs) && reading.atMs > atMs && reading.headroom >= cfg.recover_pct) {
        delete nextCooldowns[prov];
        recoveredCooldownProviders.add(prov);
      }
    }
  }

  // Evaluate a candidate's quota status using ONLY its own provider's reading.
  const evaluateQuota = (ref, lowQuotaMap) => {
    const prov = providerOf(ref);
    const reading = freshReadingFor(quota, ref);
    const lq = lowQuotaMap[prov];
    if (lq) {
      const resetMs = typeof lq.tight_resets_at === "string" ? Date.parse(lq.tight_resets_at) : NaN;
      const windowReset =
        Number.isFinite(resetMs) && nowMs >= resetMs && reading !== null && reading.headroom > cfg.threshold_pct;
      if (reading && reading.headroom >= cfg.recover_pct) {
        delete lowQuotaMap[prov];
        return { low: false, reading, recovered: "recovered" };
      }
      if (windowReset) {
        delete lowQuotaMap[prov];
        return { low: false, reading, recovered: "window-reset" };
      }
      if (
        (lq.tight_resets_at == null || (Number.isFinite(resetMs) && nowMs >= resetMs)) &&
        reading &&
        reading.headroom <= cfg.threshold_pct &&
        reading.tightResetsAtMs != null
      ) {
        lowQuotaMap[prov] = { tight_resets_at: new Date(reading.tightResetsAtMs).toISOString() };
      }
      return { low: true, reading, recovered: null };
    }
    if (reading && reading.headroom <= cfg.threshold_pct) {
      lowQuotaMap[prov] = {
        tight_resets_at: reading.tightResetsAtMs != null ? new Date(reading.tightResetsAtMs).toISOString() : null,
      };
      return { low: true, reading, recovered: null };
    }
    return { low: false, reading, recovered: null };
  };

  // Retries: stay on the failed model unless it failed with a retryable
  // failover error (quota, rate limit, ResourceExhausted, overloaded, or
  // expired/invalid auth token).
  if (input.reason === "retry" && input.failed) {
    const kind = failoverKind(input.failedError);
    if (!kind) {
      return { model: input.failed, why: "sticky:retry" };
    }
    const failedProv = providerOf(input.failed);
    const parsedResetMs = parseResetDurationMs(input.failedError);
    let untilMs;
    if (parsedResetMs != null) {
      untilMs = nowMs + parsedResetMs;
    } else if (kind === "quota") {
      const r = freshReadingFor(quota, input.failed);
      if (r && r.headroom <= cfg.threshold_pct && r.tightResetsAtMs != null && r.tightResetsAtMs > nowMs) {
        untilMs = r.tightResetsAtMs;
      } else {
        untilMs = nowMs + DEFAULT_QUOTA_COOLDOWN_MS;
      }
    } else {
      untilMs = nowMs + DEFAULT_AUTH_COOLDOWN_MS;
    }
    if (failedProv) {
      nextCooldowns[failedProv] = {
        until: new Date(untilMs).toISOString(),
        at: new Date(nowMs).toISOString(),
        kind,
      };
    }

    const nextLowQuota = { ...prevLowQuota };
    let chosen = null;
    let why = "";
    const lowAlternatives = [];

    for (const ref of activeChain) {
      if (ref === input.failed) continue;
      const prov = providerOf(ref);
      if (nextCooldowns[prov]) continue;
      const q = evaluateQuota(ref, nextLowQuota);
      if (!q.low) {
        chosen = ref;
        why = "retry:failover-" + kind;
        break;
      }
      lowAlternatives.push({ ref, headroom: q.reading ? q.reading.headroom : -1 });
    }
    if (!chosen && lowAlternatives.length > 0) {
      lowAlternatives.sort((a, b) => b.headroom - a.headroom);
      chosen = lowAlternatives[0].ref;
      delete nextLowQuota[providerOf(chosen)];
      why = "retry:failover-" + kind + "-low-quota";
    }
    if (!chosen) {
      chosen = input.failed;
      why = "retry:all-providers-cooled-down";
    }

    const next = buildState(chosen, why, nowMs, quota, nextLowQuota, nextCooldowns);
    return { model: chosen, why, state: stateChanged(state, next) ? next : undefined };
  }

  // Turn boundary ("user", or "direct" without a previous model): walk the
  // ordered chain and pick the first candidate that is neither cooled down nor
  // low on its own provider's quota.
  const nextLowQuota = { ...prevLowQuota };
  let chosen = null;
  let why = "";
  const nonCooledLow = [];
  const cooled = [];

  for (let i = 0; i < activeChain.length; i++) {
    const ref = activeChain[i];
    const prov = providerOf(ref);
    const cd = nextCooldowns[prov];
    if (cd) {
      cooled.push({ ref, untilMs: Date.parse(cd.until) || Infinity });
      continue;
    }
    const q = evaluateQuota(ref, nextLowQuota);
    if (!q.low) {
      chosen = ref;
      const side = sideOf(ref);
      if (q.recovered) {
        why = side + ":" + q.recovered;
      } else if (recoveredCooldownProviders.has(prov)) {
        why = side + ":cooldown-recovered";
      } else if (i === 0) {
        why = q.reading ? side + ":headroom-ok" : side + ":no-fresh-quota";
      } else {
        const prevSkippedCooling = activeChain.slice(0, i).some((m) => !!nextCooldowns[providerOf(m)]);
        const prevSkippedLow = activeChain.slice(0, i).some((m) => !!nextLowQuota[providerOf(m)]);
        const wasSame = state?.model === ref || state?.side === side;
        if (wasSame && prevSkippedLow && !prevSkippedCooling) {
          const prevLowHasFresh = activeChain
            .slice(0, i)
            .some((m) => !!nextLowQuota[providerOf(m)] && freshReadingFor(quota, m) !== null);
          why = prevLowHasFresh ? side + ":holding" : side + ":holding-no-fresh-quota";
        } else if (prevSkippedCooling) {
          why = side + ":failover-cooldown";
        } else {
          why = q.reading ? side + ":primary-low" : side + ":primary-low-no-quota";
        }
      }
      break;
    }
    nonCooledLow.push({ ref, idx: i, headroom: q.reading ? q.reading.headroom : -1 });
  }

  if (!chosen && nonCooledLow.length > 0) {
    // Every non-cooled candidate is low on its own quota: pick the one with
    // the highest fresh headroom (preferring earlier chain order on ties).
    let best = nonCooledLow[0];
    for (const c of nonCooledLow.slice(1)) {
      if (c.headroom > best.headroom) best = c;
    }
    chosen = best.ref;
    why = sideOf(chosen) + ":all-low-more-headroom";
    // Only keep low_quota markers for higher-priority providers skipped in
    // favor of the winner; do not mark the winner itself as held out.
    for (let i = best.idx; i < activeChain.length; i++) {
      delete nextLowQuota[providerOf(activeChain[i])];
    }
  }

  if (!chosen) {
    // Every candidate is in active cooldown: pick the one whose cooldown
    // expires soonest so routing still returns a valid physical model.
    cooled.sort((a, b) => a.untilMs - b.untilMs);
    chosen = cooled[0]?.ref || activeChain[0];
    why = sideOf(chosen) + ":all-providers-cooled-down";
  }

  const next = buildState(chosen, why, nowMs, quota, nextLowQuota, nextCooldowns);
  // State is branch state for agent-loop requests only; direct requests
  // neither read nor keep it.
  const keep = input.reason !== "direct" && stateChanged(state, next);
  return { model: chosen, why, state: keep ? next : undefined };
}
