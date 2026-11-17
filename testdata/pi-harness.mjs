// Runs quartermaster's pi extension under a fake pi for picontrol_test.go:
// node pi-harness.mjs <extension.ts> <socket>. It prints one JSON line per
// thing the extension does (entries, user messages, tool-call results), and
// "ready" once the control socket is up. Steps come from stdin, one a line:
//   tool <command>   run a bash tool call through the guard
//   idle true|false  what ctx.isIdle() answers
//   shutdown         session_shutdown, then exit
import readline from "node:readline";

const [ext, sock] = process.argv.slice(2);
const { default: factory } = await import(ext);
const handlers = {};
let idle = true;
const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const ctx = { cwd: process.cwd(), mode: process.env.QM_TEST_PI_MODE || "tui", isIdle: () => idle,
  ui: { setStatus: (statusKey, statusText) => out({ statusKey, statusText: statusText ?? null }) } };
const pi = {
  registerFlag() {},
  getFlag: (name) => (name === "quartermaster-control" ? sock : undefined),
  on: (name, fn) => ((handlers[name] ||= []).push(fn)),
  appendEntry: (customType, data) => out({ entry: customType, data }),
  sendMessage: (m) => out({ message: m.customType }),
  // pi runs the input event as it accepts a message, then delivers it.
  sendUserMessage: (text, opts) => {
    for (const h of handlers.input || []) h({ text, source: "extension", streamingBehavior: opts?.deliverAs }, ctx);
    out({ user: text, deliverAs: opts?.deliverAs ?? null });
  },
  events: { emit: (name, data) => out({ event: name, active: data.active }) },
};
factory(pi);
for (const h of handlers.session_start || []) h({ reason: "startup" }, ctx);

const fs = await import("node:fs");
const wait = setInterval(() => {
  if (fs.existsSync(sock + ".token")) {
    clearInterval(wait);
    out({ ready: true });
  }
}, 10);

const rl = readline.createInterface({ input: process.stdin });
for await (const line of rl) {
  const [op, ...rest] = line.split(" ");
  const arg = rest.join(" ");
  if (op === "idle") idle = arg === "true";
  if (op === "tool") {
    // Not awaited: an ask waits on quartermaster while the test answers it.
    Promise.all((handlers.tool_call || []).map((h) => h({ toolName: "bash", input: { command: arg } }, ctx))).then((rs) => {
      const r = rs.find((x) => x);
      out({ tool: arg, block: !!r?.block, reason: r?.reason ?? null });
    });
  }
  if (op === "switch" || op === "fork") {
    const rs = await Promise.all((handlers[op === "switch" ? "session_before_switch" : "session_before_fork"] || []).map((h) => h({}, ctx)));
    out({ op, cancel: rs.some((r) => r?.cancel) });
  }
  if (op === "subagent") {
    const rs = await Promise.all((handlers.tool_call || []).map((h) => h({ toolName: "subagent", input: { agent: arg } }, ctx)));
    out({ op, block: rs.some((r) => r?.block) });
  }
  if (op === "shutdown") {
    for (const h of handlers.session_shutdown || []) h({ reason: "quit" }, ctx);
    out({ shutdown: true });
    process.exit(0);
  }
}
