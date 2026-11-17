// Read-only observation of an isolated real pi TUI. Never loaded in live sessions.
import { writeFileSync, appendFileSync, existsSync } from "node:fs";
export default function (pi) {
  let ui, timer;
  const root = process.env.QM_TRIAL_DIR;
  pi.on("session_start", (_event, ctx) => {
    ctx.ui.setWidget("fixture-observer", (tui) => { ui = tui; return { render: () => [], invalidate() {} }; });
    timer = setInterval(() => {
      const focused = ui?.getFocusedComponent();
      writeFileSync(root + "/snapshot.json", JSON.stringify({ pid: process.pid, text: ctx.ui.getEditorText(), cursor: focused?.getCursor?.(),
        focus: focused?.constructor?.name, idle: ctx.isIdle(), session: ctx.sessionManager.getSessionFile(),
        entries: ctx.sessionManager.getEntries().filter((e) => e.type === "custom_message" && e.customType === "quartermaster.inbox").map((e) => e.details?.through) }));
    }, 50);
  });
  pi.on("input", async (event) => {
    appendFileSync(root + "/inputs.jsonl", JSON.stringify({ text: event.text, source: event.source }) + "\n");
    if (event.text === "slow fixture prompt" && existsSync(root + "/slow-input")) {
      writeFileSync(root + "/input-waiting", "waiting");
      while (existsSync(root + "/slow-input")) await new Promise((r) => setTimeout(r, 50));
    }
  });
  pi.on("session_shutdown", () => clearInterval(timer));
  pi.on("session_before_tree", async () => {
    if (!existsSync(root + "/cancel-tree")) return;
    writeFileSync(root + "/tree-waiting", "waiting");
    while (existsSync(root + "/cancel-tree")) await new Promise((r) => setTimeout(r, 50));
    return { cancel: true };
  });
  pi.registerCommand("fixture-cancel-tree", { handler: async (_, ctx) => {
    const user = ctx.sessionManager.getEntries().find((e) => e.type === "message" && e.message.role === "user");
    await ctx.navigateTree(user.id, { summarize: true });
  } });
  pi.registerCommand("fixture-dialog", { handler: async (_, ctx) => { await ctx.ui.confirm("Fixture dialog", "Wait here for the trial"); } });
}
