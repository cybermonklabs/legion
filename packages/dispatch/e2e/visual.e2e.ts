/** The visual review: one row per page of `e2e/review-routes.ts`'s shot set, each compared
 *  against a committed baseline in light and in dark. The accessibility layer
 *  (`e2e/accessibility.e2e.ts`) reads the same corpus and the same readiness locators, so a
 *  finding of one layer is a finding about the page the other shot.
 *
 *  TWO PROJECTS, ONE FILE. The desktop rows run in the `visual` project at 1280x800; the pages a
 *  phone width rearranges run again, in the `visual-phone` project's iPhone 13 viewport, and
 *  their titles carry ` @phone` - the tag the suite already selects phone rows by. That project
 *  greps the tag and `visual` inverts it (e2e/playwright.config.ts), so each row runs under
 *  exactly one project and each project writes into its own `__screenshots__` directory.
 *
 *  WHERE IT RUNS. Inside the pinned Playwright image, started by `e2e/visual-run.sh`, and
 *  nowhere else: a baseline a person takes and a baseline CI compares must be the same
 *  rendering, and a host's browser build, fonts and GPU stack are not the image's. Both projects
 *  are declared in every run all the same (e2e/playwright.config.ts says why), so a plain
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
 *  baseline was taken against is the one the host wrote once. What that asks of a row is that its
 *  page write nothing, since a write would be seen by every row after it, so each row signs in and
 *  navigates afresh, and the seed writes first each read mark a page of the shot set writes as it
 *  draws - the broadcast page's read of its replies and the Spec tab's read of the spec
 *  (`e2e/review-seed.ts`) - so a page's own write changes nothing any row compares. Every other
 *  write a row's page tries, to the API or on a document socket, is stopped before it reaches the
 *  corpus and fails that row by name (`guardSharedCorpus`, `e2e/visual-guard.ts`).
 *
 *  WHAT WOULD DRIFT BETWEEN TWO RUNS OF ONE PAGE, and what is done about each:
 *
 *  - The clock. `/` and `/agents` draw relative times ("2 minutes ago") that move with the gap
 *    between the seed and the shot, and `<EarlierApproval>` draws the seeded day's own date.
 *    Nothing else in this suite freezes a clock for a whole page; `page.clock.install()` is used
 *    by rows that drive time themselves. Here each row installs it a minute after the moment
 *    `e2e/visual-seed.ts` wrote the corpus (`seededAt`), so every relative label reads the same
 *    gap on every run - a fixed literal would drift against a corpus written at wall-clock time.
 *    The `<time>` elements (`web/src/features/refs/Timestamp.tsx`, `web/src/features/inbox/
 *    Inbox.tsx`, `web/src/features/inbox/EarlierApproval.tsx`) are painted over as well, by the
 *    page itself (`e2e/visual-time.css`; the mask list below says why that is not a mask). The
 *    cost is that the text of a timestamp is in no baseline; its place, its size and the layout
 *    around it still are.
 *  - Animations. `toHaveScreenshot` disables CSS animations, CSS transitions and Web Animations
 *    by default (`animations: "disabled"`), so no option is passed for them.
 *  - The 150 ms colour ease. `web/src/styles.css:51-57` transitions `color`, `background-color`
 *    and `border-color` on every button and link, so the frames right after `emulateMedia`
 *    switches the scheme are mid-ease. Two things cover it: the assertion waits until two
 *    consecutive screenshots are identical before it compares, and a finite transition is
 *    fast-forwarded to completion by the default above.
 *  - The steps before the light shot. Four CI runs of this job failed the light shot of
 *    `issue-activity` by 22 pixels against the bound of 20 (`maxDiffPixels` below): runs
 *    45217963089, 45240350309 and 45279483024 ran the rows in the normal and in the reverse
 *    order and failed in the reverse one, and run 45340018649, at 18928330, ran them once, in
 *    the normal order. The failed renders of 45279483024 and 45340018649 are byte-identical
 *    PNGs, and each of the 54 pixels that differ from the baseline is one 8-bit step: the
 *    anti-aliasing of the issue card's four corners, and the glyph `i` of "Which" in the
 *    activity row `asked “Which sign-in path?”`. In both failed traces the assertion took two
 *    identical screenshots before it compared, so the page was not still changing: the render
 *    it settled on differed only by how it was painted. Run 45276346586, at f548b89b, and the
 *    runs at 16adf424, ad6106ff and 843fe1b9 passed on the same page tree, and the dark shot
 *    matched its baseline in every run. The steps before the two shots differed. The dark shot
 *    came after a load in light, a settle, the light shot (which takes screenshots, with
 *    animations disabled, until two match), a switch to dark by `emulateMedia` and a second
 *    settle. The light shot came after the load and one settle alone: the context starts in
 *    light, so the loop's `emulateMedia({ colorScheme: "light" })` changes nothing.
 *    So the light shot now follows the dark shot's steps: before the loop, each row switches
 *    the page to dark, waits in `waitForRouteSettled` and takes one capture that is compared
 *    with nothing. That capture is taken as `toHaveScreenshot` takes its own - full page, under
 *    the same mask list and the same `<time>` paint (`style`, the text of the `stylePath` file),
 *    with `animations: "disabled"`, `caret: "hide"` and `scale: "css"` - because
 *    `page.screenshot` lets animations run and captures at the device's scale, which is 3 on the
 *    phone rows, and because a mask is drawn into the page: for each capture
 *    Playwright adds a pane over the masked boxes and removes it after (`_maskElements`,
 *    `playwright-core/lib/coreBundle.js`). The loop then shoots light and dark as before, and
 *    `SCHEMES` keeps its order, so the file names and the report order stay the same. The cost
 *    is one more settle and one uncompared capture per row.
 *    This reads CI's evidence; it proves nothing about what the renderer does inside. The
 *    issue pod cannot judge it either way: its host Chromium is a noisier renderer than CI's,
 *    and a normal and a reverse run of the rows there differed on 7 and on 6 of 42 shots with
 *    the light shot first and on 9 with the dark shot first, and, one pair of runs each, on 6
 *    with a switch to dark before the light shot and on 3 with these steps. A light baseline
 *    that moves under these steps is taken again from CI's render, as the header of
 *    `e2e/visual-run.sh` says.
 *  - The caret and the focus ring. No row clicks, types or focuses anything: it signs in,
 *    navigates and waits. A page that focuses an element of its own on load draws the same ring
 *    in the baseline and in the comparison, and the text caret is hidden by `caret: "hide"`,
 *    `toHaveScreenshot`'s default.
 *  - Scrollbars. Headless Chromium is launched with `--hide-scrollbars`
 *    (`playwright-core/lib/coreBundle.js`), so no scrollbar is in any capture. The shot is
 *    `fullPage`, the whole scrollable page rather than whatever part of it the window is
 *    scrolled to, so a scroll position is in no baseline either, and a change below the fold is
 *    reviewed like one above it. Each project pins its viewport, so the width and every layout
 *    breakpoint are fixed and only the page's own height follows its content.
 *  - Lazy chunks, a pending query, a document still connecting, a reference still resolving, and
 *    the rail's Inbox number while a read mark the page wrote is still reaching it. One wait
 *    covers them all: `waitForRouteSettled` (e2e/review-routes.ts), which the accessibility
 *    layer calls too, so no baseline is of a skeleton, of a route whose chunk is still coming or
 *    of a page that is still filling itself in.
 *  - The rail's Inbox number after that wait. CI drew a value that a host run of the same seed
 *    never reaches, so the rail's Inbox entry is masked (the mask list below says what was
 *    measured).
 */
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { type Browser, expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import {
  phoneShotRoutes,
  type ReviewedRoute,
  type ReviewSeed,
  railInboxLink,
  type SeededReadMarks,
  shotRoutes,
  topBarInboxBadge,
  type VisualSeed,
  waitForRouteSettled,
} from "./review-routes";
import { withAlice } from "./users";
import {
  CORPUS_WRITE_MESSAGE,
  guardSharedCorpus,
  UNLISTED_READ_MESSAGE,
  unlistedShotReads,
  unlistedShotViews,
} from "./visual-guard";
import { reviewSessions } from "./workspace";

/** Both schemes in one row, as `e2e/accessibility.e2e.ts` reads them: a sign-in and a navigation
 *  paid once rather than twice, and each shot names its scheme. */
const SCHEMES = ["light", "dark"] as const;

/** The stylesheet each shot is taken under, which paints every `<time>` over (the mask list
 *  below says why it is painted and not masked). */
const TIME_PAINT = fileURLToPath(new URL("./visual-time.css", import.meta.url));

/** The JSON `e2e/visual-seed.ts` wrote on the host. Unset says this run is not the containerised
 *  one, which is every run but `e2e/visual-run.sh`'s. */
const seedPath = process.env.DISPATCH_E2E_VISUAL_SEED;

const SKIP_REASON =
  "This layer compares baselines, which are only the same rendering inside the pinned " +
  "Playwright image: run `e2e/visual-run.sh` from packages/dispatch, which " +
  "seeds on the host and takes every shot in that container. DISPATCH_E2E_VISUAL_SEED is unset.";

// The live registry, put back after Playwright's own `globalSetup` cleared it. The host's seed
// runs before Playwright starts (`e2e/visual-run.sh`), and `e2e/global-setup.ts:198` resets the
// fake Envoy when its sentinel check ends, so the sessions `seedReviewRoutes` wrote are gone by
// the time the first row opens a page: the Agents page then draws "No agents are connected."
// and every shot of it would be a baseline of an empty registry, and every claim the corpus's
// two claim holders hold would read as free. The database half of the corpus is untouched by
// that reset, which is why only this half is written again here. `./agents` and `./workspace`
// reach no module that spawns `psql`.
test.beforeAll(async () => {
  if (seedPath === undefined) return;
  await setLiveSessions(reviewSessions);
});

/** The host's seed, with the moment it was written. A file without `seededAt` fails the row
 *  rather than leaving the clock unpinned: an unpinned page draws whatever relative label the
 *  run's own pace earned it, which is the minute-boundary drift this layer removed, and it would
 *  show up only as the late rows of a run differing from their baselines. `e2e/visual-seed.ts`
 *  always writes the field, so a file that lacks it did not come from it. */
function readVisualSeed(path: string): VisualSeed {
  // The file as e2e/visual-seed.ts writes it, read without trusting the two fields every row
  // needs, which the checks below hold.
  const seed = JSON.parse(readFileSync(path, "utf8")) as Omit<ReviewSeed, "readMarks"> & {
    readonly readMarks?: SeededReadMarks;
    readonly seededAt?: string;
  };
  const { readMarks, seededAt } = seed;
  if (seededAt === undefined) {
    throw new Error(
      `${path}, which DISPATCH_E2E_VISUAL_SEED names, carries no \`seededAt\`, so this row ` +
        "cannot pin its page clock and every relative label it shot would move with the run's " +
        "pace. `e2e/visual-seed.ts` writes that field on every seed."
    );
  }
  if (readMarks === undefined) {
    throw new Error(
      `${path}, which DISPATCH_E2E_VISUAL_SEED names, carries no \`readMarks\`, so this row's ` +
        "guard would let its page write no read mark again and fail it on the seed's own. " +
        "`e2e/visual-seed.ts` writes that field on every seed."
    );
  }
  return { ...seed, readMarks, seededAt };
}

/** One row's work, which the desktop and the phone rows share: the project they run in is the
 *  only difference between them, and it decides the viewport and the directory the baseline is
 *  read from. The skip lives here so the two loops below stay one line each. */
async function shoot(browser: Browser, route: ReviewedRoute): Promise<void> {
  if (seedPath === undefined) {
    test.skip(true, SKIP_REASON);
    return;
  }
  const seed = readVisualSeed(seedPath);
  await withAlice(browser, async (page) => {
    // Before the first navigation, so every write the page tries is stopped and named.
    const writes = await guardSharedCorpus(page, seed.readMarks);
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
    await page.clock.install({ time: new Date(Date.parse(seed.seededAt) + 60_000) });
    // What every capture below paints over: the uncompared one and the two shots alike.
    //
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
    // baseline. Every other page's heading is compared as before. Neither phone page
    // draws that stamp, so on those two rows the header mask matches nothing.
    //
    // The rail's Inbox entry is masked too. The mask covers the whole link and not only
    // the number's pill, for the reason the header above is: the pill's width can move
    // with the digit, and the link is a full-width row of the rail whose box holds still.
    // The cost is the rail's "Inbox" label, which is in no baseline. The phone rows draw
    // no rail; they draw the same number in the compact top bar's badge, which is masked
    // for the same reason. That badge is a pill in a row, so its own box is what the mask
    // paints: a one-digit count holds that box still, and a count of 10 or more would
    // widen it and move the badge beside it. CI run 44308307388 read 6 on `issue-spec`'s
    // phone row against the baseline's 5, with nothing else moved in that badge. The
    // number itself is proven by its own tests, not by these pixels.
    //
    // Why it is not waited for instead: the number CI drew after the broadcast page's
    // read mark was not the number this seed gave anywhere else. These were measured while
    // that mark was still the page's own, before the seed wrote it first: the baselines
    // read 7 on rows 1-4 and 5 from row 5 on. CI run 43517763817 read 6 in light and 5 in
    // dark on `broadcast`, and an earlier run read 6 on `issue-activity`. A host run of this
    // file's own path (`e2e/visual-seed.ts` on a `e2e/harness-up.ts` stack, the rows in
    // host Chromium, alice's counts read every 3 s for six minutes) read 7 on rows 1-4
    // and 6 on every row after the read mark, for three passes over the rows. After the
    // read mark the server did not change for the rest of those six minutes. The guess
    // was the delivery sweep (`delivery_sweep.go`): it resends an owed attempt about once
    // a minute, and that resend would take a thread out of the unread total. That is
    // ruled out. In the host run every delivery attempt of the seed stayed `sent`, and
    // none was accepted as `user_turn`, so the sweep owed nothing. CI run 43517763817
    // logs no resend from the sweep. Its count went from 7 to 5 about 16 s after its
    // seed, and the read mark was the only write the page made. The seed now writes that
    // mark before any row (`e2e/review-seed.ts`), so no row's page moves the number; the
    // drop CI drew past it was never explained, no seed state can pin a step that was never
    // found, and so this mask stands in for one.
    //
    // A `<time>` is painted over by the page itself and not masked: the stylesheet the
    // shot is taken under (`stylePath`, `e2e/visual-time.css`) gives every `<time>` and
    // everything inside one a background in Playwright's own mask colour and transparent
    // text, so its box is in the baseline and its words are not, as under the mask. What
    // the mask did that this does not is paint where the page shows nothing. Playwright
    // paints a mask's rectangle over the finished shot at the element's box wherever that
    // box is, and an element its box has scrolled out of view still has one: a priorities
    // row the table's box had scrolled away painted its time over the pager's "50 per
    // page" under the box, at 1280 and on a phone, and a row the Active tab's box had
    // scrolled away painted its time over that tab's pager. A background is drawn by the
    // page, so it is clipped wherever the element is: by the box it scrolls in, under a
    // sticky header drawn over it, and not drawn at all where the element is hidden. A
    // time a box cuts part of is painted up to the box's edge and no further, which is all
    // of it the page shows. The painted rectangles are the time's own line boxes, which is
    // the rectangle the mask painted for a time on one line; a time that wraps is painted
    // line by line, where the mask painted one rectangle round both lines. They hold still
    // for the reason a mask's did: the clock is pinned, so the words, and the box they
    // make, are the same on every run.
    //
    // Every mask in the list below stands where the page shows the whole of it - a page's
    // own header row, the rail's link, the top bar's badge, a measures card's line - and
    // none in a box that scrolls, so a mask paints nothing there the page does not show. A
    // `<time>` stands in rows a box scrolls, which is why it is painted instead.
    //
    // Nothing else is masked or painted, and the Decisions panel is why that matters. It
    // prints the id of the spec block a decision shaped until the reference graph answers,
    // and a mask over that id did NOT settle it: a uuid is a fixed count of characters and
    // not a fixed width, so the painted rectangle moved with the glyphs. That page is
    // scanned and not shot now, since one block it draws has no words to wait for
    // (`e2e/review-routes.ts`). A mask is the last resort here and not the first, because
    // it hides only a difference whose box holds still.
    //
    // Each of these was found by reading a diff image of a container run against the run
    // before it, not by widening the bound until the rows passed.
    const mask = [
      page
        .locator("header")
        .filter({ has: page.getByText(/^Generated /) })
        .last(),
      page.getByText(/, as of /),
      railInboxLink(page),
      topBarInboxBadge(page),
    ];
    await page.goto(route.path(seed));
    // The light shot's steps match the dark shot's ("The steps before the light shot", above).
    await page.emulateMedia({ colorScheme: "dark" });
    await waitForRouteSettled(page, route);
    await page.screenshot({
      fullPage: true,
      animations: "disabled",
      caret: "hide",
      mask,
      // `page.screenshot` takes the stylesheet's text where `toHaveScreenshot` takes its path.
      style: readFileSync(TIME_PAINT, "utf8"),
      scale: "css",
    });
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
        mask,
        // Every `<time>`, painted in the mask's colour by the page, which clips it where it
        // clips the time (the list above says why). No layout property, so nothing moves.
        stylePath: TIME_PAINT,
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
        // them, and `maxDiffPixels: 20` is where that line is drawn. Neither phone baseline
        // drifted at all in those two runs, and the bound is the desktop figure for them too,
        // because the drift it absorbs is one 8-bit step of the renderer that draws both.
        //
        // WHAT THIS LAYER THEREFORE DOES NOT SEE: a colour change confined to fewer than 20
        // pixels, which is a patch of about 4 by 4. That is the sentence to read this bound
        // by. It sees any change above that, down to one 8-bit step, because the per-pixel
        // bound is zero.
        //
        // What a real change gives is NOT one number, and a reader judging whether 20 is
        // safe needs the small end of it rather than the large. Two controls, each one
        // 8-bit step and nothing else, measured in this container:
        //
        //   `canvasText`, the app shell's own background and the largest area any token
        //   covers - `slate-50` to `#f7fafc` - reddens the light shot of every page,
        //   451,448 pixels on `inbox-light` (run 42476321285).
        //
        //   `TEXT_PRIMARY`, which paints glyphs and is an ordinary token - `slate-900` to
        //   `#0f172c` and `slate-100` to `#f1f5fa`, distance 0.0565 - reddens 79 pixels on
        //   `project-documents-light` (run 42486290091) and 84 on `project-documents-dark`
        //   (run 42486321928).
        //
        // So the honest picture is drift 7 < bound 20 < nearest measured real change 79: a
        // factor of 3.95, not the 22,572 the canvas alone suggests. A token drawn over less
        // than about 20 pixels of a page is outside this gate, and the kept row of
        // `e2e/theme.e2e.ts` is what binds an element to its swatch there.
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
    // The server answers the page's reads and live views itself, so a write it made while
    // answering one is outside what the guard stops; the Go test that holds those writes sends
    // the routes and view kinds its list names, and any other fails the row here, by name.
    expect(
      [...unlistedShotReads(writes.reads), ...unlistedShotViews(writes.views)],
      UNLISTED_READ_MESSAGE
    ).toEqual([]);
    expect(writes.stopped, CORPUS_WRITE_MESSAGE).toEqual([]);
  });
}

for (const route of shotRoutes) {
  // SLOW(>=2s): a sign-in, a navigation, and a screenshot comparison in each of two schemes.
  test(`${route.name} matches its baseline in light and dark`, async ({ browser }) => {
    await shoot(browser, route);
  });
}

// The pages a phone width rearranges rather than only reframes, shot a second time in that
// viewport (`phoneShotRoutes`, e2e/review-routes.ts, which says which pages those are and why).
// The ` @phone` tag is what puts a row in the `visual-phone` project and keeps it out of
// `visual` (e2e/playwright.config.ts), so the two shots of one page land in two baseline
// directories under the same file name.
for (const route of phoneShotRoutes) {
  // SLOW(>=2s): a sign-in, a navigation, and a screenshot comparison in each of two schemes.
  test(`${route.name} matches its phone baseline in light and dark @phone`, async ({ browser }) => {
    await shoot(browser, route);
  });
}
