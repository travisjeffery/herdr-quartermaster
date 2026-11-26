// Draft-safe coordinator notifications and guarded idle wakeups for pi 1.6.0.
// No editor writes, user messages, terminal keystrokes, guard configuration or
// approval operations.
// @ts-nocheck
import net from "node:net";
import path from "node:path";
import { randomBytes, timingSafeEqual } from "node:crypto";
import { chmodSync, closeSync, existsSync, fsyncSync, mkdirSync, openSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync } from "node:fs";

const messageType = "quartermaster.inbox";
const notice = "[quartermaster ticker: automated, not the user, approves nothing] The inbox has changed. Run `quartermaster context`.";
const cursorOK = (v) => typeof v === "string" && /^[0-9]{19}$/.test(v);
const max = (a, b) => a > b ? a : b;

const defaultQuietWindowMs = 1000;
const defaultPreflightTimeoutMs = 15000;
const defaultPollMs = 250;
const defaultFlightGraceMs = 2000;

// Built-in interactive slash commands handled directly by InteractiveMode
// without entering AgentSession.prompt() / _preparePrompt().
const builtinCommands = new Set([
  "settings", "scoped-models", "model", "thinking", "export", "import", "share",
  "bug", "copy", "name", "session", "changelog", "hotkeys", "fork", "clone",
  "tree", "trust", "login", "logout", "new", "compact", "reload", "debug",
  "arminsayshi", "dementedelves", "resume", "quit",
]);

function save(file, data) {
  const tmp = file + ".tmp";
  const fd = openSync(tmp, "w", 0o600);
  try { writeFileSync(fd, JSON.stringify(data)); fsyncSync(fd); } finally { closeSync(fd); }
  renameSync(tmp, file);
  const dir = openSync(path.dirname(file), "r");
  try { fsyncSync(dir); } finally { closeSync(dir); }
}

// Tracks bracketed paste transitions across chunks in order, returning the
// ending paste state and only the terminal bytes outside bracketed paste.
function scanPaste(data, pasting) {
  const startTok = "\x1b[200~", endTok = "\x1b[201~";
  let i = 0, inPaste = pasting, outside = "";
  while (i < data.length) {
    if (inPaste) {
      const end = data.indexOf(endTok, i);
      if (end < 0) return { inPaste: true, outside };
      inPaste = false;
      i = end + endTok.length;
    } else {
      const start = data.indexOf(startTok, i);
      if (start < 0) {
        outside += data.slice(i);
        return { inPaste: false, outside };
      }
      outside += data.slice(i, start);
      inPaste = true;
      i = start + startTok.length;
    }
  }
  return { inPaste, outside };
}

// Matches Enter / Alt+Enter submit sequences (plain CR, Kitty CSI-u Enter, or
// modifyOtherKeys Enter), excluding Shift+Enter newlines and Kitty key releases.
function hasSubmitKey(outside) {
  if (!outside) return false;
  if (outside.includes("\r")) return true;
  if (/\x1b\[13(?:;(?:1|3)(?::(?!3\b)[0-9]*)?)?u/.test(outside)) return true;
  if (/\x1b\[27;(?:1|3);13~/.test(outside)) return true;
  return false;
}

function textBeforeSubmit(outside) {
  const idx = outside.indexOf("\r");
  if (idx <= 0) return "";
  return outside.slice(0, idx).replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, "").replace(/\x1b/g, "").trim();
}

function isNonPromptSubmission(text, pi) {
  if (!text) return true;
  if (text.startsWith("!")) return true;
  if (text.startsWith("/")) {
    const cmd = text.slice(1).split(/\s/, 1)[0];
    if (builtinCommands.has(cmd)) return true;
    if (typeof pi.getCommands === "function") {
      try {
        if (pi.getCommands().some((c) => c?.source === "extension" && c?.name === cmd)) return true;
      } catch {}
    }
  }
  return false;
}

// Exported separately so fixtures exercise the actual policy without loading
// pi's UI package or substituting the production editor.
export function coordinatorInbox(pi, ctx, sock, isEditor, opts = {}) {
  const session = ctx.sessionManager.getSessionFile();
  if (ctx.mode !== "tui" || !session || !path.isAbsolute(sock)) throw new Error("coordinator inbox needs a saved TUI session and absolute socket path");
  const quietWindowMs = Number(opts?.quietWindowMs) > 0 ? Number(opts.quietWindowMs) : defaultQuietWindowMs;
  const preflightTimeoutMs = Number(opts?.preflightTimeoutMs) > 0 ? Number(opts.preflightTimeoutMs) : defaultPreflightTimeoutMs;
  const pollMs = Number(opts?.pollMs) > 0 ? Number(opts.pollMs) : defaultPollMs;
  const flightGraceMs = Number(opts?.flightGraceMs) > 0 ? Number(opts.flightGraceMs) : defaultFlightGraceMs;

  // One bounded journal per coordinator, shared across session replacements.
  // Pending work retains its old session until a freshly addressed ticker
  // request explicitly revalidates it for the current session.
  const file = sock + ".inbox.json";
  mkdirSync(path.dirname(sock), { recursive: true, mode: 0o700 });
  chmodSync(path.dirname(sock), 0o700);
  let state = { session, ack: "", pending: "", flight: "" };
  let tui, timer, server, token = "", owned = false, closed = false;
  let dialog = false, compacting = false, switching = false, lastInput = Date.now(), sending = false;
  let pasting = false, fault = "", retryAfter = 0, sentAt = 0;
  // pi 1.6.0 tracks _promptPreflightCount during async input hooks and auth
  // checks, but AgentSession.isIdle and sendCustomMessage({ triggerTurn: true })
  // do not check it. Guard both editor-submit preflight (before earlier
  // extensions' input handlers run) and input-event preflight until
  // before_agent_start / agent_start / agent_settled (or bounded timeout).
  let preflightSince = 0;
  // Bounded at-most-once idle wakeup per durable watermark: if a wakeup fails
  // before persisting its custom message, retain pending/fault without looping
  // model turns until a strictly newer watermark arrives or explicit resume.
  let failedWake = "";
  const subscriptions = [];
  const on = (event, fn) => subscriptions.push(pi.on(event, fn));
  const persist = () => save(file, state);

  function preflightActive() {
    if (!preflightSince) return false;
    if (Date.now() - preflightSince >= preflightTimeoutMs) {
      preflightSince = 0;
      return false;
    }
    return true;
  }
  function modelReady() {
    return !("model" in ctx) || Boolean(ctx.model);
  }
  // Receipts cover all session entries, not just the current branch: navigating
  // backwards must not replay an already delivered event into an older branch.
  function recorded(through) {
    return ctx.sessionManager.getEntries().some((e) => e.type === "custom_message" && e.customType === messageType && e.details?.session === session && e.details?.through === through);
  }
  function acknowledge() {
    if (!state.flight) return false;
    // Pi buffers a new session until its first conversation message. We never
    // send before the session exists, and require the receipt on disk before
    // acknowledging. No tokens or transcript content leave this process.
    const disk = readFileSync(state.session, "utf8").split("\n").some((line) => {
      try { const e = JSON.parse(line); return e.type === "custom_message" && e.customType === messageType && e.details?.session === state.session && e.details?.through === state.flight; } catch { return false; }
    });
    if (!disk) return false;
    const fd = openSync(state.session, "r");
    try { fsyncSync(fd); } finally { closeSync(fd); }
    const next = { ...state, ack: max(state.ack, state.flight), flight: "" };
    if (next.pending <= next.ack) next.pending = "";
    save(file, next);
    state = next;
    if (failedWake && state.ack >= failedWake) failedWake = "";
    sending = false;
    return true;
  }
  function failInFlight(reason) {
    if (!state.flight) return;
    const failedCursor = state.flight;
    const next = { ...state, pending: max(state.pending, failedCursor), flight: "" };
    save(file, next);
    state = next;
    failedWake = max(failedWake, failedCursor);
    sending = false;
    fault = reason;
    retryAfter = Date.now() + 5000;
  }
  // A process restart has no queued pi action. Reconcile a saved intent with
  // the durable transcript before retrying anything; a live in-flight send is
  // never retried based on an asynchronous sendMessage return value.
  function recover() {
    if (existsSync(file)) {
      const saved = JSON.parse(readFileSync(file, "utf8"));
      if (typeof saved.session !== "string" || !path.isAbsolute(saved.session) || ![saved.ack, saved.pending, saved.flight].every((v) => v === "" || cursorOK(v))) throw new Error("invalid coordinator inbox receipt");
      state = saved;
    }
    acknowledge();
    if (state.flight) { state.pending = max(state.pending, state.flight); state.flight = ""; }
    persist();
  }

  function wakeBlocked() {
    return Boolean(failedWake && state.pending && state.pending <= failedWake);
  }
  function status() {
    ctx.ui.setStatus(messageType, fault ? "inbox: delivery error (retained)" : state.pending || state.flight ? "inbox: pending" : undefined);
  }
  function wakeState() {
    if (closed) return "closed";
    if (!ctx.isIdle() || ctx.hasPendingMessages()) return "busy";
    if (compacting) return "compacting";
    if (preflightActive()) return "preflight";
    if (state.flight || sending) return "waking";
    if (!state.pending) return "idle";
    if (wakeBlocked()) return "failed";
    if (fault) return "error";
    if (state.session !== session || !existsSync(session)) return "session";
    if (dialog) return "dialog";
    if (switching) return "switching";
    if (pasting) return "draft";
    if (tui?.hasOverlay()) return "overlay";
    const editor = tui?.getFocusedComponent();
    if (!isEditor(editor)) return "unfocused";
    if (ctx.ui.getEditorText() !== "" || editor.getText() !== "") return "draft";
    if (!modelReady()) return "no_model";
    if (Date.now() - lastInput < quietWindowMs) return "quiet_wait";
    return "ready";
  }
  function safe(boundary = false) {
    const editor = tui?.getFocusedComponent();
    return !closed && !dialog && !compacting && !switching && !pasting &&
      Date.now() - lastInput >= quietWindowMs &&
      (boundary || (ctx.isIdle() && !preflightActive() && modelReady())) &&
      !ctx.hasPendingMessages() &&
      !tui?.hasOverlay() && isEditor(editor) && ctx.ui.getEditorText() === "" &&
      editor.getText() === "" && existsSync(session) && state.session === session;
  }
  function pump() {
    if (closed) return;
    try {
      if (Date.now() < retryAfter) return;
      acknowledge();
      // sendMessage({ triggerTurn: true }) starts _startRun synchronously, so
      // ctx.isIdle() stays false for the entire run (including compaction and
      // streaming). Seeing ctx.isIdle() true after the grace window with no
      // receipt means the wakeup failed before persisting its custom entry:
      // roll flight back to pending and latch failedWake so we never spin in
      // a retry/turn loop on the same watermark.
      if (state.flight && state.session === session && ctx.isIdle() && Date.now() - sentAt >= flightGraceMs && !recorded(state.flight)) {
        failInFlight("wakeup did not deliver");
        status();
        return;
      }
      if (!sending && !wakeBlocked()) fault = "";
      if (!state.flight && state.pending && !wakeBlocked() && safe()) {
        const next = { ...state, flight: state.pending };
        save(file, next); // durable intent before fire-and-forget pi API
        state = next;
        sending = true;
        sentAt = Date.now();
        try {
          pi.sendMessage({ customType: messageType, content: notice, display: true,
            details: { session, through: state.flight } }, { triggerTurn: true, deliverAs: "followUp" });
        } catch {
          if (!acknowledge()) failInFlight("wakeup failed");
        }
      }
    } catch { fault = "delivery failed"; retryAfter = Date.now() + 1000; }
    status();
  }
  // During an existing run, deliver at the final actionable boundary with one
  // continuation instead of racing a separate idle run.
  on("agent_before_settle", (event) => {
    if (state.flight || !state.pending || fault || wakeBlocked() || !safe(true)) return;
    try {
      const next = { ...state, flight: state.pending };
      save(file, next);
      state = next;
      sentAt = Date.now();
      return { entries: [...event.entries, { type: "custom_message", customType: messageType,
        content: notice, display: true, details: { session, through: state.flight } }], continue: true };
    } catch { fault = "delivery failed"; retryAfter = Date.now() + 1000; status(); }
  });
  // Capturing the existing TUI is read-only. Do NOT replace/wrap the editor:
  // setEditorComponent itself restores text and can discard cursor/selection.
  ctx.ui.setWidget(messageType, (ui) => { tui = ui; return { render: () => [], invalidate() {} }; });
  const unsubscribeInput = ctx.ui.onTerminalInput((data) => {
    lastInput = Date.now();
    const { inPaste, outside } = scanPaste(data, pasting);
    pasting = inPaste;
    // Detect an idle editor submit synchronously before earlier extensions'
    // async input handlers can run: TUI notifies onTerminalInput before
    // Editor.handleInput clears the editor and invokes onSubmit.
    if (!closed && !inPaste && ctx.isIdle() && !dialog && !compacting && !switching && !tui?.hasOverlay() && hasSubmitKey(outside)) {
      const focused = tui?.getFocusedComponent();
      if (isEditor(focused)) {
        const before = (ctx.ui.getEditorText() || focused.getText() || "").trim();
        const candidate = before || textBeforeSubmit(outside);
        if (candidate && !isNonPromptSubmission(candidate, pi)) {
          queueMicrotask(() => {
            if (closed || !ctx.isIdle() || dialog || compacting || switching || tui?.hasOverlay()) return;
            const after = tui?.getFocusedComponent();
            if (isEditor(after) && ctx.ui.getEditorText() === "" && after.getText() === "") {
              preflightSince = Date.now();
            }
          });
        }
      }
    }
    // Never consume, transform, retain or log input.
  });
  on("ui_prompt_start", () => { dialog = true; });
  on("ui_prompt_end", () => { dialog = false; lastInput = Date.now(); });
  on("session_before_compact", () => { compacting = true; });
  for (const event of ["session_compact", "session_compact_failed"]) on(event, () => { compacting = false; });
  // Tree navigation (including cancellation) is already covered by isIdle:
  // pi 1.6.0 holds isCompacting through its entire branch-summary operation.
  // Replacement has no cancelled/failed event; fail closed until human input
  // or the explicit reconnect command after a canceled switch/fork.
  for (const event of ["session_before_switch", "session_before_fork"]) on(event, () => { switching = true; preflightSince = 0; });
  on("session_tree", () => { switching = false; lastInput = Date.now(); });
  on("user_bash", () => { lastInput = Date.now(); });
  on("input", (event) => {
    switching = false;
    lastInput = Date.now();
    if (ctx.isIdle() && !event?.streamingBehavior) preflightSince = Date.now();
  });
  on("before_agent_start", () => { preflightSince = 0; });
  on("agent_start", () => { preflightSince = 0; });
  on("agent_settled", () => {
    preflightSince = 0;
    try {
      acknowledge();
      if (sending && state.flight && !recorded(state.flight)) {
        failInFlight("unconfirmed delivery");
      } else {
        sending = false;
      }
    } catch {
      sending = false;
      fault = "delivery failed";
      retryAfter = Date.now() + 1000;
    }
    lastInput = Date.now();
    status();
  });

  function handle(req) {
    const supplied = typeof req?.token === "string" ? Buffer.from(req.token) : Buffer.alloc(0);
    const expected = Buffer.from(token);
    if (supplied.length !== expected.length || !timingSafeEqual(supplied, expected)) return { ok: false, error: "bad token" };
    if (req.session !== session || ctx.sessionManager.getSessionFile() !== session || closed) return { ok: false, error: "stale session" };
    if (req.op === "inbox") {
      if (!cursorOK(req.through)) return { ok: false, error: "invalid inbox cursor" };
      if (req.through > state.ack) {
        const next = { ...state, pending: max(state.pending, req.through) };
        if (!state.flight && req.through >= state.pending) next.session = session;
        save(file, next);
        state = next;
        if (failedWake && state.pending > failedWake) {
          failedWake = "";
          retryAfter = 0;
          if (!sending) fault = "";
        }
      }
      status();
    } else if (req.op !== "inbox_status") return { ok: false, error: "coordinator channel only accepts inbox notifications" };
    return {
      ok: true,
      session,
      ack: state.ack,
      queued: !!(state.pending || state.flight),
      wake_state: wakeState(),
      idle_ms: Math.max(0, Date.now() - lastInput),
    };
  }
  function close() {
    if (closed) return;
    closed = true;
    clearInterval(timer);
    unsubscribeInput();
    for (const off of subscriptions) off?.();
    server?.close();
    // An old shutdown must never remove a newer runtime's endpoint.
    if (owned) {
      try { if (readFileSync(sock + ".token", "utf8") === token) { unlinkSync(sock); unlinkSync(sock + ".token"); } } catch {}
    }
    ctx.ui.setStatus(messageType, undefined);
    ctx.ui.setWidget(messageType, undefined);
  }
  on("session_shutdown", close);
  function listen(retried = false) {
    server = net.createServer((conn) => {
      let buffer = "", handled = false;
      conn.setEncoding("utf8");
      conn.setTimeout(3000, () => conn.destroy());
      conn.on("error", () => {});
      conn.on("data", (chunk) => {
        if (handled) return;
        buffer += chunk;
        if (buffer.length > 4096) { handled = true; conn.destroy(); return; }
        const nl = buffer.indexOf("\n");
        if (nl < 0) return;
        handled = true;
        let reply;
        try { reply = handle(JSON.parse(buffer.slice(0, nl))); } catch { reply = { ok: false, error: "inbox unavailable; retained for retry" }; }
        conn.end(JSON.stringify(reply) + "\n");
      });
    });
    server.on("error", (error) => {
      if (error.code !== "EADDRINUSE" || retried) { fault = "socket unavailable"; status(); return; }
      // Do not unlink a live listener. Only recover an unchanged, refused
      // stale socket; bind races fail closed on the single retry.
      let inode;
      try { inode = statSync(sock).ino; } catch { return; }
      const probe = net.createConnection(sock);
      probe.setTimeout(1000, () => probe.destroy());
      probe.on("connect", () => { probe.destroy(); fault = "socket already owned"; status(); });
      probe.on("error", (e) => {
        try {
          if (e.code === "ECONNREFUSED" && statSync(sock).ino === inode && !closed) { unlinkSync(sock); listen(true); }
        } catch { fault = "socket unavailable"; status(); }
      });
    });
    server.listen(sock, () => {
      if (closed) { server.close(); return; }
      try {
        recover();
        token = randomBytes(32).toString("hex");
        chmodSync(sock, 0o600);
        writeFileSync(sock + ".token", token, { mode: 0o600 });
        chmodSync(sock + ".token", 0o600);
        owned = true;
      } catch {
        // Async listen callbacks are outside pi's extension error boundary.
        // A token/permission/disk failure must not crash the TUI and its draft.
        fault = "endpoint unavailable"; status();
        server.close(() => { if (!closed) timer = setTimeout(() => listen(true), 1000); });
        return;
      }
      timer = setInterval(pump, pollMs);
      status();
    });
  }
  listen();
  return {
    close,
    socket: sock,
    isOpen: () => owned && !closed,
    resume: () => { switching = false; preflightSince = 0; failedWake = ""; fault = ""; retryAfter = 0; lastInput = Date.now(); status(); },
  };
}

export function installCoordinatorInbox(pi) {
  pi.registerFlag("quartermaster-coordinator", { description: "draft-safe coordinator inbox socket", type: "string", default: "" });
  let active;
  async function start(ctx, socket) {
    if (active) return;
    const { CustomEditor } = await import("@earendil-works/pi-coding-agent");
    active = coordinatorInbox(pi, ctx, socket, (editor) => editor instanceof CustomEditor);
  }
  pi.on("session_start", async (_event, ctx) => {
    let socket = pi.getFlag("quartermaster-coordinator");
    if (!socket) {
      const entry = ctx.sessionManager.getBranch().findLast((e) => e.type === "custom" && e.customType === "quartermaster.coordinator" && e.data?.session === ctx.sessionManager.getSessionFile());
      socket = entry?.data?.socket;
    }
    if (socket) await start(ctx, socket);
  });
  // Existing coordinators may load the updated extension with a USER-issued
  // /reload, then explicitly activate it. Never inject these commands.
  pi.registerCommand("quartermaster-inbox-connect", {
    description: "Activate the coordinator inbox on an absolute socket path",
    handler: async (args, ctx) => {
      if (active?.isOpen()) { active.resume(); ctx.ui.notify("Coordinator inbox resumed after session operation", "info"); return; }
      const socket = args.trim() || active?.socket;
      active?.close();
      active = undefined;
      if (!path.isAbsolute(socket)) throw new Error("provide an absolute socket path");
      await start(ctx, socket);
      pi.appendEntry("quartermaster.coordinator", { session: ctx.sessionManager.getSessionFile(), socket });
    },
  });
}
