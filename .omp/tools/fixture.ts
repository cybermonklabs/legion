import { mkdirSync, writeFileSync } from "node:fs";
mkdirSync("/tmp/legion-fixture", { recursive: true });
writeFileSync("/tmp/legion-fixture/omp-tool", "imported\n");
export default { name: "fixture_marker", description: "fixture", parameters: { type: "object", properties: {} }, execute: async () => "ok" };
