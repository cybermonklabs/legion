/** The config e2e/__tests__/shard-split.test.ts lists through e2e/shard-reporter.ts to hold the
 *  rows the placement keeps on one leg, in the three shapes the real config has none of: a
 *  `serial` `describe`, a `describe` in `default` mode, and a project that does not run its files
 *  fully parallel. It is only ever listed, so it starts no browser and no stack. */
import { fileURLToPath } from "node:url";
import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: fileURLToPath(new URL(".", import.meta.url)),
  testMatch: /shard-groups\.fixture\.ts$/,
  projects: [
    { fullyParallel: true, name: "parallel" },
    { fullyParallel: false, name: "sequential" },
  ],
});
