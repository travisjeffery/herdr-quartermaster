import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, readFileSync, existsSync, appendFileSync, readdirSync, mkdirSync, rmdirSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import net from "node:net";
import { coordinatorInbox } from "../pi/coordinator.ts";

const pause = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, tries = 100) { for (let i = 0; i < tries; i++) { if (fn()) return; await pause(50); } assert.fail("condition timed out"); }
const cursor = (n) => String(1700000000000000000n + BigInt(n));
function fixture(root = mkdtempSync(path.join(tmpdir(), "qm-inbox-")), sessionName = "session", opts = {}) {
  const sock = path.join(root, "inbox.sock"), session = path.join(root, sessionName);
  if (!existsSync(session)) writeFileSync(session, "", { mode: 0o600 });
  const entries = readFileSync(session, "utf8").split("\n").filter(Boolean).map(JSON.parse);
  const handlers = new Map(), messages = [], inputs = [];
  let terminal;
  const model = {
    text: "",
    cursor: { line: 0, col: 0 },
    selection: [1, 3],
    idle: true,
    pending: false,
    overlay: false,
    status: undefined,
    hold: false,
    throwOnSend: false,
    selectedModel: opts.selectedModel !== undefined ? opts.selectedModel : { provider: "fixture", id: "fixture" },
    commands: opts.commands || [],
  };
  const editor = { getText: () => model.text };
  let focus = editor;
  const ctx = {
    mode: "tui",
    get model() { return model.selectedModel; },
    isIdle: () => model.idle,
    hasPendingMessages: () => model.pending,
    sessionManager: { getSessionFile: () => session, getEntries: () => entries },
    ui: {
      getEditorText: () => model.text,
      setStatus: (_, value) => { model.status = value; },
      setWidget: (_, factory) => factory?.({ getFocusedComponent: () => focus, hasOverlay: () => model.overlay }),
      onTerminalInput: (fn) => { terminal = fn; return () => { terminal = undefined; }; },
      setEditorText: () => assert.fail("editor write"),
      pasteToEditor: () => assert.fail("paste"),
    },
  };
  const pi = {
    on: (name, fn) => { const list = handlers.get(name) || []; list.push(fn); handlers.set(name, list); return () => list.splice(list.indexOf(fn), 1); },
    getCommands: () => model.commands,
    sendUserMessage: () => assert.fail("human message"),
    sendMessage: (m, sendOpts) => {
      messages.push({ m, opts: sendOpts });
      if (model.throwOnSend) throw new Error("send boom");
      if (!model.hold) persist(m);
    },
  };
  function persist(m) { const entry = { type: "custom_message", ...m }; entries.push(entry); appendFileSync(session, JSON.stringify(entry) + "\n"); }
  const inbox = coordinatorInbox(pi, ctx, sock, (e) => e === editor, opts.inboxOpts);
  const emit = (event, payload = {}) => [...(handlers.get(event) || [])].map((h) => h(payload, ctx));
  return {
    root, sock, session, entries, messages, inputs, model,
    close: inbox.close, resume: inbox.resume, emit, persist,
    type: (text) => { terminal?.(text); model.text = text; },
    key: (data, nextText) => { terminal?.(data); if (nextText !== undefined) model.text = nextText; },
    submit: (text, key = "\r") => {
      if (text !== undefined) { terminal?.(text); model.text = text; }
      terminal?.(key);
      model.text = "";
    },
    focus: (main) => { focus = main ? editor : {}; },
  };
}
async function call(f, req) {
  await until(() => existsSync(f.sock + ".token"));
  const token = readFileSync(f.sock + ".token", "utf8");
  return new Promise((resolve, reject) => {
    const conn = net.createConnection(f.sock); let data = "";
    conn.on("error", reject);
    conn.on("connect", () => conn.write(JSON.stringify({ token, session: f.session, ...req }) + "\n"));
    conn.on("data", (chunk) => { data += chunk; });
    conn.on("end", () => resolve(JSON.parse(data)));
  });
}

test("multiline Unicode draft, cursor, selection, raw paste, idle wakeup and duplicate reconnect", async () => {
  const f = fixture();
  try {
    f.type("half written\n日本語 👩🏽‍💻 e\u0301\nlast");
    f.model.cursor = { line: 1, col: 4 };
    const before = JSON.stringify([f.model.text, f.model.cursor, f.model.selection]);
    for (let i = 0; i < 10; i++) assert.equal((await call(f, { op: "inbox", through: cursor(1) })).ok, true);
    await pause(1400);
    assert.equal(f.messages.length, 0);
    assert.equal(JSON.stringify([f.model.text, f.model.cursor, f.model.selection]), before);
    assert.equal(f.model.status, "inbox: pending");
    const draftStatus = await call(f, { op: "inbox_status" });
    assert.equal(draftStatus.wake_state, "draft");
    assert.equal(typeof draftStatus.idle_ms, "number");
    assert.ok(draftStatus.idle_ms >= 1000);

    // Bracketed paste blocks wakeup even while editor text is empty.
    f.type("\x1b[200~"); f.model.text = "";
    await pause(1200); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "draft");

    // End paste with empty editor -> guarded idle wakeup fires once with triggerTurn: true.
    f.type("\x1b[201~"); f.model.text = "";
    await until(() => f.messages.length === 1);
    await until(() => f.model.status === undefined);
    assert.equal(f.messages[0].opts.deliverAs, "followUp");
    assert.equal(f.messages[0].opts.triggerTurn, true);
    assert.equal(f.messages[0].m.customType, "quartermaster.inbox");
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "idle");

    // Duplicate watermark never wakes a second turn.
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1200); assert.equal(f.messages.length, 1);
  } finally { f.close(); }
});

test("active turns, queued user input, dialogs, builtin focus, overlays, compaction, no model and session transitions hold delivery", async () => {
  const f = fixture();
  try {
    f.model.idle = false;
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1200); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "busy");

    f.model.idle = true; f.model.pending = true;
    await pause(300); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "busy");

    f.model.pending = false; f.emit("ui_prompt_start");
    await pause(300); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "dialog");

    f.emit("ui_prompt_end"); f.focus(false);
    await pause(1200); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "unfocused");

    f.focus(true); f.model.overlay = true;
    await pause(300); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "overlay");

    f.model.overlay = false; f.emit("session_before_compact");
    await pause(300); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "compacting");

    f.emit("session_compact_failed"); f.emit("session_before_tree");
    f.model.idle = false; // pi holds isCompacting until canceled tree work finishes
    await pause(300); assert.equal(f.messages.length, 0);

    f.model.idle = true; f.model.selectedModel = undefined;
    await pause(1200); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "no_model");

    f.model.selectedModel = { provider: "fixture", id: "fixture" };
    f.emit("session_before_switch");
    await pause(300); assert.equal(f.messages.length, 0);
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "switching");

    // Human prompt releases canceled switch, holds during preflight, and wakes after settlement.
    f.emit("input", { source: "interactive" });
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await pause(1200); assert.equal(f.messages.length, 0);
    f.model.idle = false;
    f.emit("before_agent_start");
    f.emit("agent_start");
    await pause(300); assert.equal(f.messages.length, 0);
    f.model.idle = true;
    f.emit("agent_settled");
    await until(() => f.messages.length === 1);
    assert.equal(f.messages[0].opts.triggerTurn, true);
  } finally { f.close(); }
});

test("editor submit arms preflight before earlier input handlers run and clears on agent_start/settled", async () => {
  const f = fixture(undefined, "session", {
    inboxOpts: { quietWindowMs: 200, pollMs: 50, preflightTimeoutMs: 5000 },
  });
  try {
    // Human types and hits Enter; editor synchronously clears before input event arrives
    // (simulating an earlier loaded extension blocking in its async input handler).
    f.submit("slow human prompt");
    await pause(20); // allow microtask
    await call(f, { op: "inbox", through: cursor(1) });
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await pause(500); // well past quietWindowMs (200ms)
    assert.equal(f.messages.length, 0);

    // Earlier extension finishes -> our input handler fires -> auth finishes -> run starts.
    f.emit("input", { source: "interactive" });
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await pause(350);
    assert.equal(f.messages.length, 0);

    f.model.idle = false;
    f.emit("before_agent_start");
    f.emit("agent_start");
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "busy");
    f.model.idle = true;
    f.emit("agent_settled");

    await until(() => f.messages.length === 1);
    assert.equal(f.messages[0].opts.triggerTurn, true);
    assert.equal(f.messages[0].m.details.through, cursor(1));
  } finally { f.close(); }
});

test("non-prompt submissions and draft clears do not trigger preflight; handled/failed prompts recover after bounded timeout", async () => {
  const f = fixture(undefined, "session", {
    commands: [{ name: "fixture-dialog", source: "extension" }],
    inboxOpts: { quietWindowMs: 150, pollMs: 40, preflightTimeoutMs: 400 },
  });
  try {
    // Built-in slash command does not enter preflight.
    f.submit("/session");
    await pause(20);
    await call(f, { op: "inbox", through: cursor(1) });
    assert.notEqual((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await until(() => f.messages.length === 1);

    // Extension slash command does not enter preflight.
    f.submit("/fixture-dialog");
    await pause(20);
    await call(f, { op: "inbox", through: cursor(2) });
    assert.notEqual((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await until(() => f.messages.length === 2);

    // !bash command does not enter preflight.
    f.submit("!ls");
    await pause(20);
    await call(f, { op: "inbox", through: cursor(3) });
    assert.notEqual((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await until(() => f.messages.length === 3);

    // Clearing a draft with Ctrl+U does not enter preflight.
    f.type("draft to clear");
    f.key("\x15", "");
    await pause(20);
    await call(f, { op: "inbox", through: cursor(4) });
    assert.notEqual((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await until(() => f.messages.length === 4);

    // Normal prompt consumed by { action: "handled" } or auth failure (never emits agent_start)
    // blocks during preflightTimeoutMs, then recovers cleanly without wedging.
    f.submit("handled by another extension");
    await pause(20);
    await call(f, { op: "inbox", through: cursor(5) });
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "preflight");
    await pause(220);
    assert.equal(f.messages.length, 4); // still blocked within preflightTimeoutMs (400ms)
    await until(() => f.messages.length === 5); // recovered once bounded preflight expired
  } finally { f.close(); }
});

test("bounded at-most-once wakeup prevents retry/turn loops on unpersisted failure", async () => {
  const f = fixture(undefined, "session", {
    inboxOpts: { quietWindowMs: 100, pollMs: 40, flightGraceMs: 150 },
  });
  try {
    // 1. Wakeup fails before persisting custom entry -> rolls flight back, latches failedWake, no loop.
    f.model.hold = true;
    await call(f, { op: "inbox", through: cursor(1) });
    await until(() => f.messages.length === 1);
    await until(() => f.model.status === "inbox: delivery error (retained)");
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "failed");
    assert.equal((await call(f, { op: "inbox_status" })).queued, true);
    // Repeated polls and re-sent same watermark must NOT trigger a second turn.
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(500);
    assert.equal(f.messages.length, 1);

    // 2. Strictly newer watermark clears latch and gets one fresh wakeup attempt.
    f.model.hold = false;
    await call(f, { op: "inbox", through: cursor(2) });
    await until(() => f.messages.length === 2);
    await until(() => f.model.status === undefined);
    assert.equal(f.messages[1].m.details.through, cursor(2));
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "idle");

    // 3. Synchronous sendMessage throw also fails closed without looping, and resume() retries once.
    f.model.throwOnSend = true;
    await call(f, { op: "inbox", through: cursor(3) });
    await until(() => f.messages.length === 3);
    await until(() => f.model.status === "inbox: delivery error (retained)");
    assert.equal((await call(f, { op: "inbox_status" })).wake_state, "failed");
    await pause(400);
    assert.equal(f.messages.length, 3);

    f.model.throwOnSend = false;
    f.resume();
    await until(() => f.messages.length === 4);
    await until(() => f.model.status === undefined);
    assert.equal(f.messages[3].m.details.through, cursor(3));
  } finally { f.close(); }
});

test("bounded durable coalescing, restart and acknowledged replay", async () => {
  let f = fixture(); const root = f.root;
  try {
    f.type("draft");
    for (let i = 1; i <= 100; i++) await call(f, { op: "inbox", through: cursor(i) });
    const receipt = readdirSync(root).find((p) => p.endsWith(".inbox.json"));
    assert.ok(readFileSync(path.join(root, receipt)).length < 1024);
    f.close(); await pause(50); f = fixture(root);
    await until(() => f.messages.length === 1);
    await until(() => f.model.status === undefined);
    assert.equal(f.messages[0].m.details.through, cursor(100));
    assert.equal(f.messages[0].opts.triggerTurn, true);
    f.close(); await pause(50); f = fixture(root);
    await call(f, { op: "inbox", through: cursor(100) });
    await pause(1300); assert.equal(f.messages.length, 0);
  } finally { f.close(); }
});

test("intent reconciles the session receipt after restart; no duplicate or lost pending event", async () => {
  let f = fixture(); const root = f.root;
  try {
    f.model.hold = true;
    await call(f, { op: "inbox", through: cursor(1) });
    await until(() => f.messages.length === 1);
    f.persist(f.messages[0].m); // crash window: persisted custom entry, no ack yet
    f.close(); await pause(50); f = fixture(root);
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1300); assert.equal(f.messages.length, 0);
    f.model.hold = true;
    await call(f, { op: "inbox", through: cursor(2) });
    await until(() => f.messages.length === 1);
    f.close(); await pause(50); f = fixture(root); // intent before pi persisted anything
    await until(() => f.messages.length === 1);
    assert.equal(f.messages[0].m.details.through, cursor(2));
  } finally { f.close(); }
});

test("authentication, session pinning, old-session pending state and non-notification operations", async () => {
  let f = fixture(); const root = f.root;
  try {
    assert.equal((await call(f, { op: "inbox", token: "wrong", through: cursor(1) })).ok, false);
    assert.equal((await call(f, { op: "inbox", session: "other", through: cursor(1) })).ok, false);
    assert.equal((await call(f, { op: "prompt", text: "approve" })).ok, false);
    assert.equal((await call(f, { op: "answer", choice: 1 })).ok, false);
    assert.equal((await call(f, { op: "inbox", through: "invalid" })).ok, false);
    f.type("draft"); await call(f, { op: "inbox", through: cursor(1) });
    const old = f.session;
    f.close(); await pause(50); f = fixture(root, "other-session");
    assert.equal((await call(f, { op: "inbox", session: old, through: cursor(1) })).ok, false);
    await pause(1300); assert.equal(f.messages.length, 0);
    f.close(); await pause(50); f = fixture(root);
    await until(() => f.messages.length === 1);
  } finally { f.close(); }
});

test("pending work rebinds only on a fresh current-session tick; global ack prevents later replay", async () => {
  let f = fixture(); const root = f.root;
  try {
    f.type("draft in A");
    await call(f, { op: "inbox", through: cursor(1) });
    f.close(); await pause(50); f = fixture(root, "session-B");
    await pause(1300); assert.equal(f.messages.length, 0);
    await call(f, { op: "inbox", through: cursor(1) });
    await until(() => f.messages.length === 1);
    await until(() => f.model.status === undefined);
    f.close(); await pause(50); f = fixture(root);
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1300); assert.equal(f.messages.length, 0);
  } finally { f.close(); }
});

test("transient intent-persistence failure retries without losing or duplicating the event", async () => {
  const f = fixture();
  const tmp = f.sock + ".inbox.json.tmp";
  try {
    f.type("draft");
    await call(f, { op: "inbox", through: cursor(1) });
    mkdirSync(tmp); // deterministic EISDIR even when a test runner is root
    f.type("");
    await until(() => f.model.status === "inbox: delivery error (retained)");
    assert.equal(f.messages.length, 0);
    rmdirSync(tmp);
    await until(() => f.messages.length === 1);
    await until(() => f.model.status === undefined);
    await pause(1200); assert.equal(f.messages.length, 1);
  } finally { if (existsSync(tmp)) rmdirSync(tmp); f.close(); }
});

test("cancelled tree operation does not starve the inbox", async () => {
  const f = fixture();
  try {
    f.model.idle = false;
    f.emit("session_before_tree");
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1300); assert.equal(f.messages.length, 0);
    f.model.idle = true; // canceled navigation emits no session_tree in pi
    await until(() => f.messages.length === 1);
  } finally { f.close(); }
});

test("a second live owner cannot rewrite a receipt or steal the endpoint", async () => {
  const first = fixture(); let second;
  try {
    first.type("draft");
    await call(first, { op: "inbox", through: cursor(1) });
    const before = readFileSync(first.sock + ".inbox.json", "utf8");
    const token = readFileSync(first.sock + ".token", "utf8");
    second = fixture(first.root);
    await pause(1300);
    assert.equal(readFileSync(first.sock + ".inbox.json", "utf8"), before);
    assert.equal(readFileSync(first.sock + ".token", "utf8"), token);
    second.close();
    first.type("");
    await until(() => first.messages.length === 1);
  } finally { second?.close(); first.close(); }
});

for (const suffix of [".inbox.json.tmp", ".token"]) {
  test(`transient startup failure at ${suffix} recovers without crashing the host`, async () => {
    const f = fixture();
    const obstruction = f.sock + suffix;
    mkdirSync(obstruction);
    try {
      await until(() => f.model.status === "inbox: delivery error (retained)");
      rmdirSync(obstruction);
      assert.equal((await call(f, { op: "inbox", through: cursor(1) })).ok, true);
      await until(() => f.messages.length === 1);
      await until(() => f.model.status === undefined);
    } finally { f.close(); }
  });
}

test("active final boundary emits one custom message and requests one continuation", async () => {
  const f = fixture();
  try {
    f.model.idle = false;
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1300);
    const result = f.emit("agent_before_settle", { entries: [] }).find(Boolean);
    assert.equal(result.continue, true);
    assert.equal(result.entries.length, 1);
    assert.equal(result.entries[0].customType, "quartermaster.inbox");
    f.persist(result.entries[0]);
    await until(() => f.model.status === undefined);
    assert.equal(f.emit("agent_before_settle", { entries: [] }).find(Boolean), undefined);
    assert.equal(f.messages.length, 0); // no fire-and-forget send at the boundary
  } finally { f.close(); }
});
