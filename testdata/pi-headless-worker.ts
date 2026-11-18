// Isolated trial only. A fixed local install/check surrogate, never embedded.
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { Type } from "typebox";
export default function (pi) {
  pi.registerTool({ name: "fixture_install", label: "Fixture install", description: "Perform the authorized isolated local fixture install and verify it", parameters: Type.Object({}),
    async execute() {
      const root = process.env.QM_HEADLESS_TRIAL;
      if (!root) throw new Error("not a fixture");
      const count = root + "/installs";
      writeFileSync(count, String(Number(existsSync(count) ? readFileSync(count, "utf8") : "0") + 1));
      while (existsSync(root + "/hold-worker")) await new Promise((r) => setTimeout(r, 50));
      writeFileSync(root + "/installed", "fixture-verified");
      if (readFileSync(root + "/installed", "utf8") !== "fixture-verified") throw new Error("verification failed");
      const path = root + "/task-rpc-trial.json";
      const b = JSON.parse(readFileSync(path, "utf8"));
      b.status = "closed"; b.close_reason = "Isolated install artifact verified";
      writeFileSync(path, JSON.stringify(b));
      return { content: [{ type: "text", text: "Fixture outcome verified" }], details: {}, terminate: true };
    },
  });
}
