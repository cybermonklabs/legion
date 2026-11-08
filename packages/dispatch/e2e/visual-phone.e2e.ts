/** The visual review, phone: the pages of `e2e/review-routes.ts`'s shot set that a phone
 *  viewport draws differently (`phoneShotNames`), compared against a committed baseline in light
 *  and in dark, in the `visual-phone` project's iPhone 13 viewport under Chromium.
 *
 *  Everything `e2e/visual.e2e.ts`'s head says holds here word for word, and is not repeated: the
 *  layer runs inside the pinned Playwright image and skips itself anywhere else; it imports
 *  nothing that reaches `./psql` and reads the host's seed out of the JSON
 *  `DISPATCH_E2E_VISUAL_SEED` names; one seed serves the whole file, so no row resets or seeds
 *  and each row navigates afresh; and the clock, animations, the 150 ms colour ease, the caret,
 *  the focus ring, scrollbars and lazy chunks are settled there, each with its reason. This file
 *  is a second spec rather than a second project over the first because the two select the pages
 *  they shoot differently, and a project that ran this file's rows in the desktop viewport would
 *  shoot the same page twice.
 */
import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { phoneShotNames, type ReviewedRoute, type ReviewSeed, shotRoutes } from "./review-routes";
import { withAlice } from "./users";
import { workspaceSessions } from "./workspace";

const SCHEMES = ["light", "dark"] as const;

/** The JSON `e2e/visual-seed.ts` wrote on the host. Unset says this run is not the containerised
 *  one, which is every run but `e2e/visual-run.sh`'s. */
const seedPath = process.env.DISPATCH_E2E_VISUAL_SEED;

const SKIP_REASON =
  "This layer compares baselines, which are only the same rendering inside the pinned " +
  "Playwright image: run `bun e2e/run.ts -- e2e/visual-run.sh` from packages/dispatch, which " +
  "seeds on the host and takes every shot in that container. DISPATCH_E2E_VISUAL_SEED is unset.";

// The live registry, put back after Playwright's `globalSetup` cleared it, for the reason
// `e2e/visual.e2e.ts` gives at length: the host seeds before Playwright starts and
// `e2e/preflight.ts:73` resets the fake Envoy. This file shoots no Agents page, and it does it
// all the same, so a phone baseline is of the same corpus its desktop baseline is of.
test.beforeAll(async () => {
  if (seedPath === undefined) return;
  await setLiveSessions(workspaceSessions);
});

/** The desktop rows this file shoots again on a phone, by name: `e2e/review-routes.ts` holds
 *  which pages those are and why, so the subset is taken from it rather than written twice. */
const phoneRoutes: readonly ReviewedRoute[] = shotRoutes.filter((route) =>
  phoneShotNames.includes(route.name)
);

for (const route of phoneRoutes) {
  // SLOW(>=2s): a sign-in, a navigation, and a screenshot comparison in each of two schemes.
  test(`${route.name} matches its phone baseline in light and dark`, async ({ browser }) => {
    if (seedPath === undefined) {
      test.skip(true, SKIP_REASON);
      return;
    }
    const seed = JSON.parse(readFileSync(seedPath, "utf8")) as ReviewSeed;
    await withAlice(browser, async (page) => {
      await page.goto(route.path(seed));
      for (const colorScheme of SCHEMES) {
        await page.emulateMedia({ colorScheme });
        // Before the shot, so no baseline is taken of a loading skeleton or a 404, either of
        // which would compare clean against itself for ever after.
        await expect(route.ready(page)).toBeVisible();
        await expect(page).toHaveScreenshot(`${route.name}-${colorScheme}.png`, {
          fullPage: true,
          mask: [page.locator("time")],
          // MEASURED IN CI: placeholder, exactly as in `e2e/visual.e2e.ts`, and measured
          // separately: a phone emulation lays the page out at another scale, so its legitimate
          // maximum is its own number. Whoever runs this layer in the container replaces the
          // zero here with that measured value and states the largest ratio of N legitimate
          // runs, the ratio a one-colour control change gives, and the factor between them.
          maxDiffPixelRatio: 0,
        });
      }
    });
  });
}
