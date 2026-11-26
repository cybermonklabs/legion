/** The accessibility layer on every pull request: each page of `e2e/review-routes.ts`'s scanned
 *  set that a recording can draw whole (`stubScannedRoutes`), read in light and in dark by the
 *  scan of `e2e/accessibility-scan.ts`, axe at WCAG 2.0 and 2.1 level A and AA. A scan reads the
 *  DOM a page draws from the answers it was given, and these pages draw from HTTP answers a
 *  recording holds, so a recording can answer them (packages/dispatch/README.md,
 *  "The stubbed layer"). A page a part of which comes from the live document's Yjs socket or from
 *  a `/ws/live` frame the contract fixtures do not carry is read on the real stack by
 *  `e2e/accessibility.e2e.ts`, and its entry's `realStackScan` says which part.
 *
 *  Each row pins the page's clock to `seededAt`, the moment the corpus's seeding finished, where
 *  the visual layer pins it a minute after that moment (`e2e/visual.e2e.ts`). The Agents and
 *  Machines pages read each session's `last_seen` against that clock, and a recording freezes
 *  `last_seen`, so an unpinned replay would draw every seeded session as one gone quiet and scan
 *  a page the recording never saw. */
import { expectNoStaleExclusion, scanRoute } from "./accessibility-scan";
import { expectTimeout } from "./expect-timeout";
import { stubScannedRoutes, trackDocumentRequests, type VisualSeed } from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { stubbed } from "./stub/stub-test";

/** The review corpus, and the moment its seeding finished, which each row's page clock is
 *  pinned to. The seed's waits give up at the bound the rows' own assertions use. */
const test = stubbed(async (): Promise<VisualSeed> => {
  const seed = await seedReviewRoutes({ boundMs: expectTimeout });
  return { ...seed, seededAt: new Date().toISOString() };
});

for (const route of stubScannedRoutes) {
  // SLOW(>=2s): two axe scans of a drawn page, each behind the settle wait's 500 ms quiet window.
  test(`${route.name} has no serious or critical accessibility violation in light or dark @pr`, async ({
    page,
    scene,
  }, testInfo) => {
    await page.clock.install({ time: new Date(scene.seededAt) });
    // A recording answers the page from routes, so the settle wait reads its requests by the
    // document that sent them (`trackDocumentRequests` says why), from before the first load.
    await scanRoute(page, route, scene, testInfo, await trackDocumentRequests(page));
  });
}

test.afterAll(expectNoStaleExclusion);
