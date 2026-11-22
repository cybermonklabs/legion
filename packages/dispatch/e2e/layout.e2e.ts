/** The layout layer's real-stack rows: the routes of `e2e/layout-routes.ts` a recording cannot
 *  draw (each route's `needsStack` says why), over the review corpus and the stress layer of
 *  `e2e/layout-seed.ts`, read at every width and in every state by `checkLayout`
 *  (`e2e/layout-check.ts`). Every other route is read by the stubbed rows of
 *  `e2e/layout-rule.stub.e2e.ts`, on every pull request; these rows are untagged, so they run on
 *  the full lane: the merge queue and the nightly (e2e/playwright.config.ts, `chromium`).
 *
 *  A row fails on any breach `e2e/layout-breaches.ts` does not list, on a required group the
 *  page does not mark, and on a page that drew too little; after the worker's rows, on an entry
 *  whose route, state and width it measured and which matched nothing. Each reading prints one
 *  `LAYOUT …` line to stdout, so a shard leg's own log carries the counts. */
import { expectTimeout } from "./expect-timeout";
import { checkLayout, staleEntries } from "./layout-check";
import { layoutRoutes } from "./layout-routes";
import { seedLayoutStress } from "./layout-seed";
import { expect, test } from "./test";
import { withAlice } from "./users";

for (const route of layoutRoutes.filter((candidate) => candidate.needsStack !== undefined)) {
  // SLOW(>=2s): a database reset, the review corpus and its stress layer, a sign-in, and a load
  // of a drawn page at each of five widths, each settled and measured.
  test(`${route.name} breaks no layout rule from 360 to 1920 px`, async ({ browser }) => {
    // The seed's waits give up at the bound this row's own assertions use, inside the row's clock.
    const seed = await seedLayoutStress({ boundMs: expectTimeout });
    await withAlice(browser, (page) => checkLayout(page, route, seed));
  });
}

test.afterAll(() => {
  expect(
    staleEntries(),
    "entries of e2e/layout-breaches.ts whose route, state and width were measured and which matched nothing"
  ).toEqual([]);
});
