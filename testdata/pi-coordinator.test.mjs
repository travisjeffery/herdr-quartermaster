import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, readFileSync, existsSync, appendFileSync, readdirSync, mkdirSync, rmdirSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import net from "node:net";
import { coordinatorInbox } from "../pi/coordinator.ts";

const pause = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn) { for (let i = 0; i < 100; i++) { if (fn()) return; await pause(50); } assert.fail("condition timed out"); }
const cursor = (n) => String(1700000000000000000n + BigInt(n));
function fixture(root = mkdtempSync(path.join(tmpdir(), "qm-inbox-")), sessionName = "session") {
  const sock = path.join(root, "inbox.sock"), session = path.join(root, sessionName);
  if (!existsSync(session)) writeFileSync(session, "", { mode: 0o600 });
  const entries = readFileSync(session, "utf8").split("\n").filter(Boolean).map(JSON.parse);
  const handlers = new Map(), messages = [], inputs = [];
  let terminal;
  const model = { text: "", cursor: { line: 0, col: 0 }, selection: [1, 3], idle: true, pending: false, overlay: false, status: undefined, hold: false };
  const editor = { getText: () => model.text };
  let focus = editor;
  const ctx = { mode: "tui", isIdle: () => model.idle, hasPendingMessages: () => model.pending,
    sessionManager: { getSessionFile: () => session, getEntries: () => entries },
    ui: { getEditorText: () => model.text,
      setStatus: (_, value) => { model.status = value; },
      setWidget: (_, factory) => factory?.({ getFocusedComponent: () => focus, hasOverlay: () => model.overlay }),
      onTerminalInput: (fn) => { terminal = fn; return () => { terminal = undefined; }; },
      setEditorText: () => assert.fail("editor write"), pasteToEditor: () => assert.fail("paste") } };
  const pi = { on: (name, fn) => { const list = handlers.get(name) || []; list.push(fn); handlers.set(name, list); return () => list.splice(list.indexOf(fn), 1); },
    sendUserMessage: () => assert.fail("human message"),
    sendMessage: (m, opts) => { messages.push({ m, opts }); if (!model.hold) persist(m); } };
  function persist(m) { const entry = { type: "custom_message", ...m }; entries.push(entry); appendFileSync(session, JSON.stringify(entry) + "\n"); }
  const inbox = coordinatorInbox(pi, ctx, sock, (e) => e === editor);
  const emit = (event, payload = {}) => [...(handlers.get(event) || [])].map((h) => h(payload, ctx));
  return { root, sock, session, entries, messages, inputs, model, close: inbox.close, emit, persist,
    type: (text) => { terminal?.(text); model.text = text; }, focus: (main) => { focus = main ? editor : {}; } };
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

test("multiline Unicode draft, cursor, selection, raw paste and duplicate reconnect", async () => {
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
    f.type("\x1b[200~"); f.model.text = "";
    await pause(1200); assert.equal(f.messages.length, 0);
    f.type("\x1b[201~"); f.model.text = "";
    await until(() => f.messages.length === 1);
    await until(() => f.model.status === undefined);
    assert.equal(f.messages[0].opts.deliverAs, "followUp");
    assert.equal(f.messages[0].opts.triggerTurn, false); // never race human async preflight
    assert.equal(f.messages[0].m.customType, "quartermaster.inbox");
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1200); assert.equal(f.messages.length, 1);
  } finally { f.close(); }
});

test("active turns, queued user input, dialogs, builtin focus, overlays, compaction and session transitions hold delivery", async () => {
  const f = fixture();
  try {
    f.model.idle = false;
    await call(f, { op: "inbox", through: cursor(1) });
    await pause(1200); assert.equal(f.messages.length, 0);
    f.model.idle = true; f.model.pending = true;
    await pause(300); assert.equal(f.messages.length, 0);
    f.model.pending = false; f.emit("ui_prompt_start");
    await pause(300); assert.equal(f.messages.length, 0);
    f.emit("ui_prompt_end"); f.focus(false);
    await pause(1200); assert.equal(f.messages.length, 0);
    f.focus(true); f.model.overlay = true;
    await pause(300); assert.equal(f.messages.length, 0);
    f.model.overlay = false; f.emit("session_before_compact");
    await pause(300); assert.equal(f.messages.length, 0);
    f.emit("session_compact_failed"); f.emit("session_before_tree");
    f.model.idle = false; // pi holds isCompacting until canceled tree work finishes
    await pause(300); assert.equal(f.messages.length, 0);
    f.model.idle = true;
    f.emit("session_before_switch");
    await pause(300); assert.equal(f.messages.length, 0);
    f.emit("input");
    await until(() => f.messages.length === 1);
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
