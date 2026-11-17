// Draft-safe coordinator notifications for pi 1.5.2. No editor writes, user
// messages, terminal keystrokes, guard configuration or approval operations.
// @ts-nocheck
import net from "node:net";
import path from "node:path";
import { randomBytes, timingSafeEqual } from "node:crypto";
import { chmodSync, closeSync, existsSync, fsyncSync, mkdirSync, openSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync } from "node:fs";

const messageType = "quartermaster.inbox";
const notice = "[quartermaster ticker: automated, not the user, approves nothing] The inbox has changed. Run `quartermaster context`.";
const cursorOK = (v) => typeof v === "string" && /^[0-9]{19}$/.test(v);
const max = (a, b) => a > b ? a : b;

function save(file, data) {
  const tmp = file + ".tmp";
  const fd = openSync(tmp, "w", 0o600);
  try { writeFileSync(fd, JSON.stringify(data)); fsyncSync(fd); } finally { closeSync(fd); }
  renameSync(tmp, file);
  const dir = openSync(path.dirname(file), "r");
  try { fsyncSync(dir); } finally { closeSync(dir); }
}

// Exported separately so fixtures exercise the actual policy without loading
// pi's UI package or substituting the production editor.
export function coordinatorInbox(pi, ctx, sock, isEditor) {
  const session = ctx.sessionManager.getSessionFile();
  if (ctx.mode !== "tui" || !session || !path.isAbsolute(sock)) throw new Error("coordinator inbox needs a saved TUI session and absolute socket path");
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
  const subscriptions = [];
  const on = (event, fn) => subscriptions.push(pi.on(event, fn));
  const persist = () => save(file, state);
  // Receipts cover all session entries, not just the current branch: navigating
  // backwards must not replay an already delivered event into an older branch.
  function recorded(through) {
    return ctx.sessionManager.getEntries().some((e) => e.type === "custom_message" && e.customType === messageType && e.details?.session === session && e.details?.through === through);
  }
  function acknowledge() {
    if (!state.flight) return;
    // Pi buffers a new session until its first conversation message. We never
    // send before the session exists, and require the receipt on disk before
    // acknowledging. No tokens or transcript content leave this process.
    const disk = readFileSync(state.session, "utf8").split("\n").some((line) => {
      try { const e = JSON.parse(line); return e.type === "custom_message" && e.customType === messageType && e.details?.session === state.session && e.details?.through === state.flight; } catch { return false; }
    });
    if (!disk) return;
    const fd = openSync(state.session, "r");
    try { fsyncSync(fd); } finally { closeSync(fd); }
    const next = { ...state, ack: max(state.ack, state.flight), flight: "" };
    if (next.pending <= next.ack) next.pending = "";
    save(file, next);
    state = next;
    sending = false;
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

  function status() {
    ctx.ui.setStatus(messageType, fault ? "inbox: delivery error (retained)" : state.pending || state.flight ? "inbox: pending" : undefined);
  }
  function safe(boundary = false) {
    const editor = tui?.getFocusedComponent();
    return !closed && !dialog && !compacting && !switching && !pasting &&
      Date.now() - lastInput >= 1000 && (boundary || ctx.isIdle()) && !ctx.hasPendingMessages() &&
      !tui?.hasOverlay() && isEditor(editor) && ctx.ui.getEditorText() === "" &&
      editor.getText() === "" && existsSync(session) && state.session === session;
  }
  function pump() {
    if (closed) return;
    try {
      if (Date.now() < retryAfter) return;
      acknowledge();
      // Passive sendMessage reports persistence errors asynchronously. Once
      // idle, an absent receipt means no append/boundary commit is still in
      // progress. Retry the same durable watermark after backoff; never retry
      // an unconfirmed active boundary.
      if (state.flight && state.session === session && ctx.isIdle() && Date.now() - sentAt >= 2000 && !recorded(state.flight)) {
        const next = { ...state, pending: max(state.pending, state.flight), flight: "" };
        save(file, next);
        state = next;
        sending = false;
        fault = "append did not deliver";
        retryAfter = Date.now() + 5000;
        status();
        return;
      }
      if (!sending) fault = "";
      if (!state.flight && state.pending && safe()) {
        const next = { ...state, flight: state.pending };
        save(file, next); // intent before fire-and-forget pi API
        state = next;
        sending = true;
        sentAt = Date.now();
        pi.sendMessage({ customType: messageType, content: notice, display: true,
          details: { session, through: state.flight } }, { triggerTurn: false, deliverAs: "followUp" });
      }
    } catch { fault = "delivery failed"; retryAfter = Date.now() + 1000; }
    status();
  }
  // isIdle in pi 1.5.2 excludes asynchronous human-input preflight. Starting
  // a new run here could reject a submitted human prompt. Idle delivery only
  // appends context. During an existing run, use the final actionable boundary
  // to deliver and continue without racing a separate idle run.
  on("agent_before_settle", (event) => {
    if (state.flight || !state.pending || fault || !safe(true)) return;
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
    if (data.includes("\x1b[200~")) pasting = true;
    if (data.includes("\x1b[201~")) pasting = false;
    // Never consume, transform, retain or log input.
  });
  on("ui_prompt_start", () => { dialog = true; });
  on("ui_prompt_end", () => { dialog = false; lastInput = Date.now(); });
  on("session_before_compact", () => { compacting = true; });
  for (const event of ["session_compact", "session_compact_failed"]) on(event, () => { compacting = false; });
  // Tree navigation (including cancellation) is already covered by isIdle:
  // pi 1.5.2 holds isCompacting through its entire branch-summary operation.
  // Replacement has no cancelled/failed event; fail closed until human input
  // or the explicit reconnect command after a canceled switch/fork.
  for (const event of ["session_before_switch", "session_before_fork"]) on(event, () => { switching = true; });
  on("session_tree", () => { switching = false; lastInput = Date.now(); });
  on("input", () => { switching = false; lastInput = Date.now(); });
  on("agent_settled", () => {
    // Keep a failed boundary commit visible until the idle retry reconciles
    // it. Idle retries append context only; they never loop model requests.
    if (sending && state.flight && !recorded(state.flight)) fault = "unconfirmed delivery";
    sending = false;
    lastInput = Date.now();
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
      }
      status();
    } else if (req.op !== "inbox_status") return { ok: false, error: "coordinator channel only accepts inbox notifications" };
    return { ok: true, session, ack: state.ack, queued: !!(state.pending || state.flight) };
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
      timer = setInterval(pump, 250);
      status();
    });
  }
  listen();
  return { close, socket: sock, isOpen: () => owned && !closed, resume: () => { switching = false; lastInput = Date.now(); } };
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
