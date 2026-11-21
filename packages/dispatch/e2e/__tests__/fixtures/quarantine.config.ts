import { fileURLToPath } from "node:url";
import { defineConfig } from "@playwright/test";

import { type QuarantineEntry, quarantined, quarantineRun } from "../../quarantine";

/** The planted control of e2e/__tests__/quarantine.test.ts for the quarantine's pattern: two
 *  projects over the rows of `quarantine-rows.fixture.ts`, each passed through `quarantined` as
 *  e2e/playwright.config.ts passes its own, with the planted entries below in place of the real
 *  list. The planted list is what the look-alike rows of that file are read against: beside each
 *  row it names stands one that only looks like it, so a listing of these shows the pattern takes
 *  the one and leaves the other, whatever the real list holds.
 *
 * `alpha` takes every row, and `alpha-phone` - whose name begins with `alpha`, so a pattern not
 * anchored at the project would reach it - only the `@phone` one. Like the real config, it reads
 * DISPATCH_E2E_QUARANTINE, so a listing under `exclude` must hold every row but the two the
 * entries name, and one under `only` those two alone. */
export const plantedQuarantine: readonly QuarantineEntry[] = [
  {
    file: "quarantine-rows.fixture.ts",
    project: "alpha",
    title: "saves the draft @pr",
    issue: "dispatch://INFRA-983",
    added: "2026-11-19",
  },
  {
    file: "quarantine-rows.fixture.ts",
    project: "alpha-phone",
    title: "offline › costs $5 (or more)? @phone",
    issue: "dispatch://INFRA-983",
    added: "2026-11-19",
  },
  // A row the file does not hold: its title without the tag the row's own title carries.
  {
    file: "quarantine-rows.fixture.ts",
    project: "alpha",
    title: "saves the draft",
    issue: "dispatch://INFRA-983",
    added: "2026-11-19",
  },
];

const selection = quarantineRun(process.env.DISPATCH_E2E_QUARANTINE);

export default defineConfig({
  projects: [{ name: "alpha" }, { grep: /(^|\s)@phone(\s|$)/, name: "alpha-phone" }].map(
    (project) => quarantined(project, plantedQuarantine, selection)
  ),
  testDir: fileURLToPath(new URL(".", import.meta.url)),
  testMatch: /quarantine-rows\.fixture\.ts$/,
});
