/** The visual review, desktop: one row per page of `e2e/review-routes.ts`'s shot set, each
 *  compared against a committed baseline in light and in dark. The accessibility layer
 *  (`e2e/accessibility.e2e.ts`) reads the same corpus and the same readiness locators, so a
 *  finding of one layer is a finding about the page the other shot.
 *
 *  WHERE IT RUNS. Inside the pinned Playwright image, started by `e2e/visual-run.sh`, and
 *  nowhere else: a baseline a person takes and a baseline CI compares must be the same
 *  rendering, and a host's browser build, fonts and GPU stack are not the image's. The `visual`
 *  project is declared in every run all the same (e2e/playwright.config.ts says why), so a plain
 *  `bun run e2e`, which passes no `--project`, reaches these rows - and every one of them skips
 *  itself, because `DISPATCH_E2E_VISUAL_SEED` is unset outside that container.
 *
 *  WHAT IT MAY IMPORT. Nothing that reaches `./psql`: not `./seed`, not `./review-seed`, not
 *  `./delivery-seed`. The image carries no `psql` binary. `e2e/visual-seed.ts` runs on the host
 *  before the container starts, writes the corpus and saves its ids as JSON, and
 *  `DISPATCH_E2E_VISUAL_SEED` names that file here. `./review-routes` imports Playwright's types
 *  and `./two-builds-routes`'s route paths, which is a module of data that imports nothing, and
 *  `./users` reaches no further than `./api` and the harness ports, so both are safe.
 *
 *  ONE SEED FOR THE WHOLE FILE. No row resets the database and no row seeds it: the corpus every
 *  baseline was taken against is the one the host wrote once. What that costs is that a row
 *  cannot assume the page is as a row before it left it - a row that wrote would be seen by
 *  every row after it - so each row signs in and navigates afresh, and no row writes.
 *
 *  WHAT WOULD DRIFT BETWEEN TWO RUNS OF ONE PAGE, and what is done about each:
 *
 *  - The clock. `/` and `/agents` draw relative times ("2 minutes ago") that move with the gap
 *    between the seed and the shot, and `<EarlierApproval>` draws the seeded day's own date.
 *    Nothing in this suite freezes a clock for a whole page - `page.clock.install()` is used by
 *    rows that drive time themselves, and here it would only trade a moving delta for a fixed
 *    fake `now` measured against a `created_at` the seed still writes at wall-clock time. Every
 *    one of those labels is a `<time>` element (`web/src/features/refs/Timestamp.tsx`,
 *    `web/src/features/inbox/Inbox.tsx`, `web/src/features/inbox/EarlierApproval.tsx`), so they
 *    are masked. The cost is that the text of a timestamp is in no baseline; its place, its size
 *    and the layout around it still are.
 *  - Animations. `toHaveScreenshot` disables CSS animations, CSS transitions and Web Animations
 *    by default (`animations: "disabled"`), so no option is passed for them.
 *  - The 150 ms colour ease. `web/src/styles.css:51-57` transitions `color`, `background-color`
 *    and `border-color` on every button and link, so the frames right after `emulateMedia`
 *    switches the scheme are mid-ease. Two things cover it: the assertion waits until two
 *    consecutive screenshots are identical before it compares, and a finite transition is
 *    fast-forwarded to completion by the default above.
 *  - The caret and the focus ring. No row clicks, types or focuses anything: it signs in,
 *    navigates and waits. A page that focuses an element of its own on load draws the same ring
 *    in the baseline and in the comparison, and the text caret is hidden by `caret: "hide"`,
 *    `toHaveScreenshot`'s default.
 *  - Scrollbars. Headless Chromium is launched with `--hide-scrollbars`
 *    (`playwright-core/lib/coreBundle.js`), so no scrollbar is in any capture. The shot is
 *    `fullPage`, the whole scrollable page rather than whatever part of it the window is
 *    scrolled to, so a scroll position is in no baseline either, and a change below the fold is
 *    reviewed like one above it. The project pins the viewport (1280x800), so the width and
 *    every layout breakpoint are fixed and only the page's own height follows its content.
 *  - Lazy chunks. A route's `ready` locator is what waits for them: it resolves once the page is
 *    drawn, so no baseline is of a loading skeleton or of a route whose chunk is still coming.
 */
import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { type ReviewSeed, shotRoutes } from "./review-routes";
import { withAlice } from "./users";
import { workspaceSessions } from "./workspace";

/** Both schemes in one row, as `e2e/accessibility.e2e.ts` reads them: a sign-in and a navigation
 *  paid once rather than twice, and each shot names its scheme. */
const SCHEMES = ["light", "dark"] as const;

/** The JSON `e2e/visual-seed.ts` wrote on the host. Unset says this run is not the containerised
 *  one, which is every run but `e2e/visual-run.sh`'s. */
const seedPath = process.env.DISPATCH_E2E_VISUAL_SEED;

const SKIP_REASON =
  "This layer compares baselines, which are only the same rendering inside the pinned " +
  "Playwright image: run `bun e2e/run.ts -- e2e/visual-run.sh` from packages/dispatch, which " +
  "seeds on the host and takes every shot in that container. DISPATCH_E2E_VISUAL_SEED is unset.";

// The live registry, put back after Playwright's own `globalSetup` cleared it. The host's seed
// runs before Playwright starts (`e2e/visual-run.sh`), and `e2e/preflight.ts:73` resets the fake
// Envoy when its sentinel check ends, so the sessions `seedWorkspace()` wrote are gone by the
// time the first row opens a page: the Agents page then draws "No agents are connected." and
// every shot of it would be a baseline of an empty registry. The database half of the corpus is
// untouched by that reset, which is why only this half is written again here. `./agents` and
// `./workspace` reach no module that spawns `psql`.
test.beforeAll(async () => {
  if (seedPath === undefined) return;
  await setLiveSessions(workspaceSessions);
});

for (const route of shotRoutes) {
  // SLOW(>=2s): a sign-in, a navigation, and a screenshot comparison in each of two schemes.
  test(`${route.name} matches its baseline in light and dark`, async ({ browser }) => {
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
          // Everything on these pages that is not the same twice, and there are two kinds.
          //
          // A clock. A `<time>` carries the relative labels of the Inbox and the Agents page;
          // the Delivery and Pipeline pages write the moment they answered as plain text
          // instead ("Generated 11/8/2026, 2:27:03 AM", DeliveryPage.tsx and PipelinePage.tsx),
          // and a measures card dates its claim the same way, so those two are masked by the
          // words they begin with.
          //
          // An identifier the server makes. The Decisions panel prints the id of the spec block
          // a decision shaped ("Block 0c1f…"), and that is a fresh uuid in every seeded corpus:
          // unlike an issue's key, which e2e/workspace.ts now writes one at a time so the
          // numbering is the same twice, a uuid cannot be pinned and is masked instead.
          //
          // Each of these was found by reading a diff image of a container run against the run
          // before it, not by widening the bound until the rows passed.
          mask: [
            page.locator("time"),
            page.getByText(/^Generated /),
            page.getByText(/, as of /),
            page.getByText(/^Block [0-9a-f]{8}/),
          ],
          // MEASURED IN CI: placeholder. This is the zero the measurement starts from
          // (docs/solutions/testing/a-tolerance-is-measured-by-setting-it-to-zero-and-ships-with
          // -its-separation-stated-pr2502.md), not a measured bound: the pod this was written in
          // has no container runtime, so no run of this layer has happened yet. Whoever runs it
          // replaces this with the measured value and states, here, in both projects: the
          // largest ratio a legitimate run produced over N repeats of this layer in the
          // container, the ratio a control change of one colour produces, and the factor between
          // them. If the two are not separated by at least five times, report that rather than
          // raise this number until the rows pass. `threshold` stays at Playwright's default 0.2
          // - the per-pixel YIQ difference at which a pixel counts as different - and is part of
          // what is measured.
          maxDiffPixelRatio: 0,
        });
      }
    });
  });
}
