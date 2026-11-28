/** The config e2e/__tests__/shard-split.test.ts lists to hold e2e/shard-reporter.ts's key of each
 *  row against the listing's, through `row-keys.reporter.ts`, in the shapes where a row's title
 *  path and its titles in a JSON report part - a `describe` with no title, and one inside it - and
 *  in a title that holds a newline, which each keeps whole. It is only ever listed, so it starts no
 *  browser and no stack. */
import { fileURLToPath } from "node:url";
import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: fileURLToPath(new URL(".", import.meta.url)),
  testMatch: /row-keys\.fixture\.ts$/,
  projects: [{ name: "keys" }],
});
