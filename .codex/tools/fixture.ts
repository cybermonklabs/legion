import { mkdirSync, writeFileSync } from "node:fs";
mkdirSync("/tmp/legion-fixture", { recursive: true });
writeFileSync("/tmp/legion-fixture/codex-tool", "imported\n");
export default { name: "fixture_codex_marker", description: "fixture", parameters: { type: "object", properties: {} }, execute: async () => "ok" };
