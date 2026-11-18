import { test } from "node:test";
import assert from "node:assert/strict";
import factory from "../pi/headless.ts";

test("restricted consumer blocks every executable tool and preserves automated provenance", async () => {
  const handlers = {}, commands = {}, messages = [];
  let active;
  factory({
    on: (name, fn) => { handlers[name] = fn; },
    registerCommand: (name, value) => { commands[name] = value; },
    setActiveTools: (names) => { active = names; },
    sendMessage: (message, options) => messages.push({ message, options }),
  });
  handlers.session_start();
  assert.deepEqual(active, []);
  const event = { systemPrompt: "existing baseline", systemPromptOptions: { selectedTools: ["bash"] } };
  const prompt = handlers.before_agent_start(event);
  assert.deepEqual(event.systemPromptOptions.selectedTools, []);
  assert.ok(prompt.systemPrompt.startsWith("existing baseline"));
  for (const toolName of ["bash", "write", "edit", "codemode", "subagent", "mcp__remote__mutate", "unknown"]) {
    assert.equal(handlers.tool_call({ toolName }).block, true);
  }
  assert.equal(handlers.user_bash().result.exitCode, 1);
  assert.equal(handlers.session_before_switch().cancel, true);
  assert.equal(handlers.session_before_fork().cancel, true);
  await commands["quartermaster-consume"].handler("automated data", { mode: "rpc" });
  assert.equal(messages[0].message.customType, "quartermaster.automated-event");
  assert.equal(messages[0].options.triggerTurn, true);
  await assert.rejects(commands["quartermaster-consume"].handler("data", { mode: "tui" }));
});
