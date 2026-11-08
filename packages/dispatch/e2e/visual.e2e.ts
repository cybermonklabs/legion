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
 *  - Lazy chunks, a pending query, a document still connecting, a reference still resolving. One
 *    wait covers them all: `waitForRouteSettled` (e2e/review-routes.ts), which the accessibility
 *    layer calls too, so no baseline is of a skeleton, of a route whose chunk is still coming or
 *    of a page that is still filling itself in.
 */
import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { type ReviewSeed, shotRoutes, waitForRouteSettled } from "./review-routes";
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
      // The page's clock, pinned a minute after the corpus was written. Every relative label
      // these pages draw - "just now", "2 minutes ago", "0m", "Updated just now" - is that
      // label's own time against `Date.now()`, so without this a row read "just now" in a run
      // that reached it inside a minute of the seed and "2 minutes ago" in a slower one. That
      // is what made the last rows of a run differ and the first rows match: rows 13, 14 and
      // 22 to 24 crossed the minute and rows 1 to 12 did not.
      //
      // A mask does not fix it, which is the thing to know before reaching for one: a mask
      // paints over the element's own box, and "just now" and "2 minutes ago" are boxes of
      // different widths, so the painted rectangle changes with the text under it.
      //
      // Pinned from the corpus rather than from a fixed literal, because what the label reads
      // is the gap between the two: a literal would drift against a corpus written now.
      if (seed.seededAt !== undefined) {
        await page.clock.install({ time: new Date(Date.parse(seed.seededAt) + 60_000) });
      }
      await page.goto(route.path(seed));
      for (const colorScheme of SCHEMES) {
        await page.emulateMedia({ colorScheme });
        // Before the shot, so no baseline is taken of a loading skeleton, a 404 or a page
        // still resolving itself - each would compare clean against itself for ever after,
        // and the third is what two container runs of this corpus disagreed about
        // (`waitForRouteSettled`).
        await waitForRouteSettled(page, route);
        // `soft`, so a row that fails in light still shoots dark and reports both. A hard
        // assertion throws out of the `SCHEMES` loop, so a change that moved both schemes was
        // reported as one, and the reader fixed light, re-ran, and met dark the round after.
        // It also costs one screenshot on a failing row only, which a failing row can afford.
        await expect.soft(page).toHaveScreenshot(`${route.name}-${colorScheme}.png`, {
          fullPage: true,
          // The one thing on these pages that is not the same twice and cannot be waited for:
          // the moment the page answered. A `<time>` carries the relative labels of the Inbox
          // and the Agents page, and the page's own clock is pinned above so each reads the
          // same; the Delivery and Pipeline pages write that moment as plain text instead
          // ("Generated 11/8/2026, 2:27:03 AM · window 11/1/2026-11/8/2026",
          // DeliveryPage.tsx and PipelinePage.tsx), and a measures card dates its claim the
          // same way, in a 2-digit hour and minute whose width does not move.
          //
          // That stamp is masked by the `header` that HOLDS it rather than by its own box,
          // which is the one thing a reader of this list has to know. A mask paints the
          // element's own rectangle: the hour's digit count and the window's two dates move
          // with the run and with the day, so the rectangle's right edge moved with them -
          // 112 pixels on `delivery` and 128 on `pipeline`, in each of two clean container
          // runs of one unchanged tree (runs 42476315586 and 42476318547). The header is a
          // full-width row, so its box holds still. The cost is on those two pages alone:
          // their heading, and Delivery's window control, are inside that rectangle and in no
          // baseline. Every other page's heading is compared as before.
          //
          // Nothing else is masked, and the Decisions panel is why that matters. It prints the
          // id of the spec block a decision shaped until the reference graph answers, and a
          // mask over that id did NOT settle it: a uuid is a fixed count of characters and not
          // a fixed width, so the painted rectangle moved with the glyphs. That row waits for
          // the block's own words instead (`e2e/review-routes.ts`). A mask is the last resort
          // here and not the first, because it hides only a difference whose box holds still.
          //
          // Each of these was found by reading a diff image of a container run against the run
          // before it, not by widening the bound until the rows passed.
          mask: [
            page.locator("time"),
            page
              .locator("header")
              .filter({ has: page.getByText(/^Generated /) })
              .last(),
            page.getByText(/, as of /),
          ],
          // MEASURED, both numbers, by starting at zero.
          // (docs/solutions/testing/a-tolerance-is-measured-by-setting-it-to-zero-and-ships-with
          // -its-separation-stated-pr2502.md asks for the bound to start at zero, for the
          // legitimate maximum and the value a real change gives to be recorded, and for the
          // separation between them to ship with the constant. Here they are.)
          //
          // `threshold` STAYS AT ZERO, and it is the number that decides what this layer can
          // see. pixelmatch counts a pixel as different when its YIQ distance,
          // `0.5053*dy^2 + 0.299*di^2 + 0.1957*dq^2` over the two 8-bit triples, exceeds
          // `35215 * threshold^2`. Playwright's inherited default of 0.2 is a cut at 1408,
          // which tolerates every re-colour an 8-bit image can carry: that is why three
          // controls of this layer compared clean over all 22 rows - `slate-50` to `amber-50`
          // at 0.0005 of that cut, `slate-200` to `sky-200` at 0.0046, `slate-900` to
          // `sky-900` at 0.017 - while the file claimed nothing was tolerated. The smallest
          // colour change an 8-bit rendering can hold, one step of red, is a distance of
          // 0.160, so any threshold above zero blinds this layer to it.
          //
          // So the legitimate maximum is a COUNT, and the bound below is one. Two clean
          // container runs of one unchanged tree differ from their baselines on 8 of their 88
          // comparisons, by 2 to 7 pixels, each pixel one 8-bit step in one channel: a
          // distance of 0.4 to 0.5, the same magnitude as the smallest real colour change
          // there is, so NO threshold separates this drift from a re-colour. Area separates
          // them. The control measured for this head - `slate-50` to `#f7fafc`, one step of
          // red on the app shell's canvas, nothing else changed - reddens the light shot of
          // every page, 451,448 pixels on `inbox-light` alone
          // (control/infra-1409-visual-light, run 42476321285). 20 against 451,448 is a
          // separation of 22,572, and 20 is the measured 7 with room for three times it.
          //
          // What this layer therefore does not see: a colour change confined to fewer than 20
          // pixels, which is a patch of about 4 by 4. It sees any change above that, down to
          // one 8-bit step, because the per-pixel bound is zero.
          //
          // Eleven sources of difference were measured on the way here and nine removed rather
          // than tolerated - two races in the seeded corpus, a reconcile tick jittered across
          // five minutes, an event-order race, four pages read before they had settled, and
          // every relative label, by pinning the page clock above. The two that could not be
          // removed are pages this layer does not shoot at all, each with its reason in
          // `e2e/review-routes.ts`.
          threshold: 0,
          maxDiffPixels: 20,
        });
      }
    });
  });
}
