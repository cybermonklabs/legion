/** The accessibility layer's real-stack half: the pages of `e2e/review-routes.ts`'s scanned set
 *  that carry a `realStackScan` (`realStackScannedRoutes`), each read in light and in dark by the
 *  scan of `e2e/accessibility-scan.ts`. Every other scanned page is read against a recording by
 *  `e2e/accessibility.stub.e2e.ts`, whose rows carry `@pr` and reach every pull request.
 *
 *  These pages stay here because a part of each is drawn from something no recording replays -
 *  the live document's Yjs socket, or a `/ws/live` frame the stubbed layer's contract fixtures do
 *  not carry - and each entry's `realStackScan` says which. Scanned against a recording, such a
 *  page would be read without that part (packages/dispatch/README.md, "The stubbed layer"). The
 *  rows are untagged: the pull-request lane already holds its 150 real-stack rows, so `chromium`
 *  runs them on the full lane, which the merge queue and the nightly run.
 *
 *  Every row reads one review corpus (e2e/review-seed.ts), which the worker seeds once and hands
 *  each row as its `reviewSeed` (e2e/test.ts): the rows read it alike, so none seeds it again. */
import { expectNoStaleExclusion, scanRoute } from "./accessibility-scan";
import { realStackScannedRoutes } from "./review-routes";
import { test } from "./test";
import { withAlice } from "./users";

for (const route of realStackScannedRoutes) {
  // SLOW(>=2s): the fakes' reset, the review corpus where the worker has not seeded it yet, a
  // sign-in and two axe scans of a drawn page.
  test(`${route.name} has no serious or critical accessibility violation in light or dark`, async ({
    browser,
    reviewSeed,
  }, testInfo) => {
    await withAlice(browser, (page) => scanRoute(page, route, reviewSeed, testInfo));
  });
}

test.afterAll(expectNoStaleExclusion);
