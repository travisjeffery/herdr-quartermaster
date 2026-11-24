// The quota router behind quartermaster's pi virtual models
// (quartermaster/worker-auto and quartermaster/coordinator-auto), registered
// by pi/quartermaster.ts. This module is pure: it parses the two JSON files
// quartermaster writes (router.json: ordered physical model chains, optional
// per-model :thinking overrides, and global/per-provider thresholds;
// quota.json: normalized subscription headroom from quota.go) and decides the
// physical model and thinking override for one request. The extension does
// the file IO and the model-registry lookup, so the decision is testable
// offline (testdata/pi-router-test.mjs, run by pirouter_test.go).
//
// Routing, at turn boundaries only:
//   - Walk the role's ordered physical model chain (default: Antigravity
//     Neon XHigh -> Claude -> OpenAI), deduplicated by canonical provider/id
//     (first occurrence wins, preserving any :thinking override) and
//     optionally filtered to models usable in pi.
//   - Each candidate is gated ONLY by its own provider's quota reading
//     (known aliases: antigravity/* -> quota.antigravity, anthropic/* ->
//     quota.claude, openai*/* -> quota.openai; any other provider key in
//     quota.json is looked up by provider name) and its own runtime failure
//     cooldown. Low headroom on one provider never switches away from a
//     different healthy provider.
//   - Each provider uses its configured per-provider threshold_pct /
//     recover_pct when set in router.json's providers map, else the global
//     threshold_pct / recover_pct (default 20% / 30%). A fresh provider
//     reading at or below threshold steps to the next healthy candidate in
//     the chain and holds off the low provider until a fresh reading
//     recovers to recover_pct or its tight window resets.
//   - Continuations keep the previous physical model; retries keep the
//     failed one unless it failed on quota, rate limits, provider
//     unavailability, or expired/invalid auth tokens, when the retry puts
//     the failed provider in cooldown (parsing "resets in <duration>" when
//     present) and steps to the next healthy candidate.
//   - Missing, stale or malformed quota data never triggers a switch and
//     never throws.
//
// @ts-nocheck

export const VALID_THINKING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];
const VALID_THINKING_SET = new Set(VALID_THINKING_LEVELS);

export function isValidThinkingLevel(v) {
  return typeof v === "string" && VALID_THINKING_SET.has(v);
}

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
  providers: {},
  quota_max_age_seconds: 6 * 3600,
};

export const DEFAULT_QUOTA_COOLDOWN_MS = 15 * 60 * 1000;
export const DEFAULT_AUTH_COOLDOWN_MS = 15 * 60 * 1000;
const MAX_RESET_DURATION_MS = 30 * 24 * 3600 * 1000;

// parseModelSpec parses "provider/id" or "provider/id:thinking".
// Rejects whitespace, "@", empty segments, colons in provider, invalid
// thinking levels, or multiple colons.
export function parseModelSpec(spec) {
  if (typeof spec !== "string" || spec === "" || /[\s@]/.test(spec)) return null;
  const slash = spec.indexOf("/");
  if (slash <= 0 || slash === spec.length - 1) return null;
  const provider = spec.slice(0, slash);
  const rest = spec.slice(slash + 1);
  if (provider.includes(":")) return null;
  let id = rest;
  let thinkingLevel = null;
  const colon = rest.indexOf(":");
  if (colon >= 0) {
    id = rest.slice(0, colon);
    const suffix = rest.slice(colon + 1);
    if (!id || !isValidThinkingLevel(suffix)) return null;
    thinkingLevel = suffix;
  }
  if (!id || id.startsWith("/") || id.endsWith("/") || id.includes("//")) return null;
  const ref = provider + "/" + id;
  return {
    ref,
    provider: provider.toLowerCase(),
    rawProvider: provider,
    id,
    thinkingLevel,
    raw: thinkingLevel ? ref + ":" + thinkingLevel : ref,
  };
}

export function stripThinkingLevel(spec) {
  const parsed = parseModelSpec(spec);
  return parsed ? parsed.ref : "";
}

export function thinkingLevelOf(spec) {
  const parsed = parseModelSpec(spec);
  return parsed ? parsed.thinkingLevel : null;
}

function modelOr(v, dflt) {
  const parsed = parseModelSpec(v);
  return parsed ? parsed.raw : dflt;
}

// Deduplicate by canonical provider/id, keeping the first occurrence (along
// with its optional :thinking override).
function dedupeModels(list) {
  const out = [];
  const seen = new Set();
  for (const m of list) {
    const parsed = parseModelSpec(m);
    if (!parsed || seen.has(parsed.ref)) continue;
    seen.add(parsed.ref);
    out.push(parsed.raw);
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

function quotaKeyForProvider(p) {
  if (!p) return null;
  if (p === "antigravity") return "antigravity";
  if (p === "anthropic") return "claude";
  if (p === "openai" || p.startsWith("openai-")) return "openai";
  return p;
}

export function quotaKeyForModel(ref) {
  return quotaKeyForProvider(providerOf(ref));
}

function sideOf(ref) {
  return quotaKeyForModel(ref) || providerOf(ref) || "unknown";
}

function resolveRoleChain(rawChain, rawPrimary, fallback, defaultChain) {
  if (Array.isArray(rawChain)) {
    const parsed = dedupeModels(rawChain);
    if (parsed.length > 0) return parsed;
  }
  if (parseModelSpec(rawPrimary)) {
    return dedupeModels([rawPrimary, fallback]);
  }
  return dedupeModels([...defaultChain.slice(0, -1), fallback]);
}

function parseProviderThresholds(rawProviders, globalThreshold, globalRecover) {
  const out = {};
  if (!rawProviders || typeof rawProviders !== "object" || Array.isArray(rawProviders)) return out;
  for (const [rawKey, entry] of Object.entries(rawProviders)) {
    const key = String(rawKey || "").trim().toLowerCase();
    if (!key || !entry || typeof entry !== "object" || Array.isArray(entry)) continue;
    const threshold_pct = pctOr(entry.threshold_pct, globalThreshold);
    let recover_pct = pctOr(entry.recover_pct, globalRecover);
    if (recover_pct < threshold_pct) recover_pct = threshold_pct;
    out[key] = { threshold_pct, recover_pct };
  }
  return out;
}

// thresholdsFor returns the effective {threshold_pct, recover_pct} for a
// model ref or provider name: exact provider override wins, then quota-key
// alias override (e.g. "openai" for "openai-codex", "claude" for "anthropic"),
// then global defaults.
export function thresholdsFor(cfg, refOrProvider) {
  const globalTh = pctOr(cfg?.threshold_pct, ROUTER_DEFAULTS.threshold_pct);
  let globalRec = pctOr(cfg?.recover_pct, ROUTER_DEFAULTS.recover_pct);
  if (globalRec < globalTh) globalRec = globalTh;
  const providers = cfg?.providers;
  if (providers && typeof providers === "object") {
    const prov =
      typeof refOrProvider === "string" && refOrProvider.includes("/")
        ? providerOf(refOrProvider)
        : String(refOrProvider || "").trim().toLowerCase();
    const alias = quotaKeyForProvider(prov);
    const entry = (prov && providers[prov]) || (alias && providers[alias]) || null;
    if (entry && typeof entry === "object") {
      const th = pctOr(entry.threshold_pct, globalTh);
      let rec = pctOr(entry.recover_pct, globalRec);
      if (rec < th) rec = th;
      return { threshold_pct: th, recover_pct: rec };
    }
  }
  return { threshold_pct: globalTh, recover_pct: globalRec };
}

// formatVirtualModelName builds a dynamic provider-ordered label when a valid
// router config is provided, or a clean neutral provider-agnostic label when
// no config is loaded yet.
export function formatVirtualModelName(role, cfg) {
  const prefix = role === "coordinator" ? "Coordinator auto" : "Worker auto";
  if (!cfg || typeof cfg !== "object") return prefix + " (ordered failover)";
  const chain = roleChain(cfg, role);
  const providers = [];
  for (const spec of chain) {
    const p = providerOf(spec);
    if (p && !providers.includes(p)) providers.push(p);
  }
  if (providers.length === 0) return prefix + " (ordered failover)";
  return prefix + " (" + providers.join(" -> ") + ")";
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
    providers: {},
    quota_max_age_seconds:
      typeof raw.quota_max_age_seconds === "number" && raw.quota_max_age_seconds > 0
        ? raw.quota_max_age_seconds
        : ROUTER_DEFAULTS.quota_max_age_seconds,
  };
  if (cfg.recover_pct < cfg.threshold_pct) cfg.recover_pct = cfg.threshold_pct;
  cfg.providers = parseProviderThresholds(raw.providers, cfg.threshold_pct, cfg.recover_pct);
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

// parseQuotaSnapshot parses quota.json into per-provider readings: known keys
// (antigravity, claude, openai) are always present (null when absent or
// malformed), and any additional provider keys (e.g. google, vertex,
// openrouter) are parsed and included by lowercase provider name.
export function parseQuotaSnapshot(text, nowMs, maxAgeMs) {
  let raw = null;
  try {
    raw = JSON.parse(text || "null");
  } catch {
    raw = null;
  }
  const out = { antigravity: null, claude: null, openai: null };
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return out;
  for (const [rawKey, val] of Object.entries(raw)) {
    const k = String(rawKey || "").trim().toLowerCase();
    if (!k || k === "generated_at") continue;
    const reading = providerReading(val, nowMs, maxAgeMs);
    if (k === "antigravity" || k === "claude" || k === "openai") {
      out[k] = reading;
    } else if (reading !== null) {
      out[k] = reading;
    }
  }
  return out;
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

function freshReadingForProvider(quota, prov) {
  if (!quota || typeof quota !== "object" || !prov) return null;
  const p = String(prov).toLowerCase();
  const direct = quota[p];
  if (direct && direct.fresh) return direct;
  const key = quotaKeyForProvider(p);
  if (key && key !== p) {
    const aliased = quota[key];
    if (aliased && aliased.fresh) return aliased;
  }
  return null;
}

function freshReadingFor(quota, ref) {
  return freshReadingForProvider(quota, providerOf(ref));
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

// decideRoute picks the physical model ("provider/id") and any per-model
// thinkingLevel override for one request.
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
// Returns {model, thinkingLevel, why, state?}: model is canonical "provider/id",
// thinkingLevel is the chain entry's explicit :thinking level or null, and
// state is set only when it changed, so the caller can return it (or undefined)
// as pi's docs ask.
export function decideRoute(input) {
  const cfg = input.config || ROUTER_DEFAULTS;
  const quota = input.quota || { antigravity: null, claude: null, openai: null };
  const state = input.state && typeof input.state === "object" ? input.state : null;
  const nowMs = typeof input.nowMs === "number" ? input.nowMs : Date.now();
  const fullEntries = roleChain(cfg, input.role)
    .map((spec) => parseModelSpec(spec))
    .filter((e) => e !== null);
  const usableSet =
    Array.isArray(input.usable) && input.usable.length > 0
      ? new Set(input.usable.map((u) => stripThinkingLevel(u) || u).filter(Boolean))
      : null;
  const candidates = usableSet ? fullEntries.filter((e) => usableSet.has(e.ref)) : fullEntries;
  const activeChain = candidates.length > 0 ? candidates : fullEntries;

  const entryThinkingFor = (canonicalRef) => {
    const found =
      activeChain.find((e) => e.ref === canonicalRef) || fullEntries.find((e) => e.ref === canonicalRef);
    return found ? found.thinkingLevel : null;
  };

  const prevRef = input.previous ? stripThinkingLevel(input.previous) || input.previous : null;
  const failedRef = input.failed ? stripThinkingLevel(input.failed) || input.failed : null;

  // Sticky within a turn: tool follow-ups stay on the model that is
  // handling the turn, keeping its prompt cache and reasoning state.
  if (input.reason === "continuation" && prevRef) {
    return { model: prevRef, thinkingLevel: entryThinkingFor(prevRef), why: "sticky:continuation" };
  }

  // Direct requests (compaction summaries, extension calls) have no branch
  // state; follow the turn's model when there is one.
  if (input.reason === "direct" && prevRef) {
    return { model: prevRef, thinkingLevel: entryThinkingFor(prevRef), why: "sticky:direct" };
  }

  const prevLowQuota = normalizeLowQuota(state);
  const nextCooldowns = normalizeCooldowns(state);

  // Clear expired cooldowns and quota cooldowns cleared by a strictly newer
  // fresh reading above that provider's effective recover_pct.
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
      const sampleEntry = activeChain.find((e) => e.provider === prov);
      const reading = sampleEntry ? freshReadingFor(quota, sampleEntry.ref) : freshReadingForProvider(quota, prov);
      const th = thresholdsFor(cfg, sampleEntry ? sampleEntry.ref : prov);
      if (reading && Number.isFinite(atMs) && reading.atMs > atMs && reading.headroom >= th.recover_pct) {
        delete nextCooldowns[prov];
        recoveredCooldownProviders.add(prov);
      }
    }
  }

  // Evaluate a candidate's quota status using ONLY its own provider's reading
  // and its effective threshold_pct / recover_pct.
  const evaluateQuota = (entry, lowQuotaMap) => {
    const prov = entry.provider;
    const reading = freshReadingFor(quota, entry.ref);
    const th = thresholdsFor(cfg, entry.ref);
    const lq = lowQuotaMap[prov];
    if (lq) {
      const resetMs = typeof lq.tight_resets_at === "string" ? Date.parse(lq.tight_resets_at) : NaN;
      const windowReset =
        Number.isFinite(resetMs) && nowMs >= resetMs && reading !== null && reading.headroom > th.threshold_pct;
      if (reading && reading.headroom >= th.recover_pct) {
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
        reading.headroom <= th.threshold_pct &&
        reading.tightResetsAtMs != null
      ) {
        lowQuotaMap[prov] = { tight_resets_at: new Date(reading.tightResetsAtMs).toISOString() };
      }
      return { low: true, reading, recovered: null };
    }
    if (reading && reading.headroom <= th.threshold_pct) {
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
  if (input.reason === "retry" && failedRef) {
    const kind = failoverKind(input.failedError);
    if (!kind) {
      return { model: failedRef, thinkingLevel: entryThinkingFor(failedRef), why: "sticky:retry" };
    }
    const failedProv = providerOf(failedRef);
    const failedTh = thresholdsFor(cfg, failedRef);
    const parsedResetMs = parseResetDurationMs(input.failedError);
    let untilMs;
    if (parsedResetMs != null) {
      untilMs = nowMs + parsedResetMs;
    } else if (kind === "quota") {
      const r = freshReadingFor(quota, failedRef);
      if (r && r.headroom <= failedTh.threshold_pct && r.tightResetsAtMs != null && r.tightResetsAtMs > nowMs) {
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
    let chosenEntry = null;
    let why = "";
    const lowAlternatives = [];

    for (const entry of activeChain) {
      if (entry.ref === failedRef) continue;
      const prov = entry.provider;
      if (nextCooldowns[prov]) continue;
      const q = evaluateQuota(entry, nextLowQuota);
      if (!q.low) {
        chosenEntry = entry;
        why = "retry:failover-" + kind;
        break;
      }
      lowAlternatives.push({ entry, headroom: q.reading ? q.reading.headroom : -1 });
    }
    if (!chosenEntry && lowAlternatives.length > 0) {
      lowAlternatives.sort((a, b) => b.headroom - a.headroom);
      chosenEntry = lowAlternatives[0].entry;
      delete nextLowQuota[chosenEntry.provider];
      why = "retry:failover-" + kind + "-low-quota";
    }
    const chosenRef = chosenEntry ? chosenEntry.ref : failedRef;
    const chosenThinking = chosenEntry ? chosenEntry.thinkingLevel : entryThinkingFor(failedRef);
    if (!chosenEntry) {
      why = "retry:all-providers-cooled-down";
    }

    const next = buildState(chosenRef, why, nowMs, quota, nextLowQuota, nextCooldowns);
    return {
      model: chosenRef,
      thinkingLevel: chosenThinking,
      why,
      state: stateChanged(state, next) ? next : undefined,
    };
  }

  // Turn boundary ("user", or "direct" without a previous model): walk the
  // ordered chain and pick the first candidate that is neither cooled down nor
  // low on its own provider's quota.
  const nextLowQuota = { ...prevLowQuota };
  let chosenEntry = null;
  let why = "";
  const nonCooledLow = [];
  const cooled = [];

  for (let i = 0; i < activeChain.length; i++) {
    const entry = activeChain[i];
    const prov = entry.provider;
    const cd = nextCooldowns[prov];
    if (cd) {
      cooled.push({ entry, untilMs: Date.parse(cd.until) || Infinity });
      continue;
    }
    const q = evaluateQuota(entry, nextLowQuota);
    if (!q.low) {
      chosenEntry = entry;
      const side = sideOf(entry.ref);
      if (q.recovered) {
        why = side + ":" + q.recovered;
      } else if (recoveredCooldownProviders.has(prov)) {
        why = side + ":cooldown-recovered";
      } else if (i === 0) {
        why = q.reading ? side + ":headroom-ok" : side + ":no-fresh-quota";
      } else {
        const prevSkippedCooling = activeChain.slice(0, i).some((m) => !!nextCooldowns[m.provider]);
        const prevSkippedLow = activeChain.slice(0, i).some((m) => !!nextLowQuota[m.provider]);
        const wasSame = state?.model === entry.ref || state?.side === side;
        if (wasSame && prevSkippedLow && !prevSkippedCooling) {
          const prevLowHasFresh = activeChain
            .slice(0, i)
            .some((m) => !!nextLowQuota[m.provider] && freshReadingFor(quota, m.ref) !== null);
          why = prevLowHasFresh ? side + ":holding" : side + ":holding-no-fresh-quota";
        } else if (prevSkippedCooling) {
          why = side + ":failover-cooldown";
        } else {
          why = q.reading ? side + ":primary-low" : side + ":primary-low-no-quota";
        }
      }
      break;
    }
    nonCooledLow.push({ entry, idx: i, headroom: q.reading ? q.reading.headroom : -1 });
  }

  if (!chosenEntry && nonCooledLow.length > 0) {
    // Every non-cooled candidate is low on its own quota: pick the one with
    // the highest fresh headroom (preferring earlier chain order on ties).
    let best = nonCooledLow[0];
    for (const c of nonCooledLow.slice(1)) {
      if (c.headroom > best.headroom) best = c;
    }
    chosenEntry = best.entry;
    why = sideOf(chosenEntry.ref) + ":all-low-more-headroom";
    // Only keep low_quota markers for higher-priority providers skipped in
    // favor of the winner; do not mark the winner itself as held out.
    for (let i = best.idx; i < activeChain.length; i++) {
      delete nextLowQuota[activeChain[i].provider];
    }
  }

  if (!chosenEntry) {
    // Every candidate is in active cooldown: pick the one whose cooldown
    // expires soonest so routing still returns a valid physical model.
    cooled.sort((a, b) => a.untilMs - b.untilMs);
    chosenEntry = cooled[0]?.entry || activeChain[0];
    why = sideOf(chosenEntry.ref) + ":all-providers-cooled-down";
  }

  const next = buildState(chosenEntry.ref, why, nowMs, quota, nextLowQuota, nextCooldowns);
  // State is branch state for agent-loop requests only; direct requests
  // neither read nor keep it.
  const keep = input.reason !== "direct" && stateChanged(state, next);
  return {
    model: chosenEntry.ref,
    thinkingLevel: chosenEntry.thinkingLevel,
    why,
    state: keep ? next : undefined,
  };
}
