// A reader with no pointer reaching for the chrome a document address holds back until the
// editor's first batch: their Tab into each held box hands them the chrome it holds, and their
// next Tab goes on into it.
//
// This row stood in `large-documents-first-screen.e2e.ts` until that file was split at its
// seams; it is a file of its own under a name that lists after that file and after
// `large-documents-first-screen.the-chrome.e2e.ts`, whose rows stood above it, so every project
// runs it in the order it did. What these three read the page's order by is
// `large-documents-after-the-batch.ts`. The other files of this family, and what each holds,
// are listed once, at the head of `large-documents-rows.ts`, which holds what they share.
import { expect, type Page, test } from "@playwright/test";

import { documentTransport } from "./editor";
import { proseOnlyDocument } from "./large-documents";
import { readTheReads, watchTheReads } from "./large-documents-after-the-batch";
import { aDrawWithinMs, seedDocument } from "./large-documents-rows";
import { asUser } from "./users";

// The row's one wait for something the page draws is bounded by `aDrawWithinMs`
// (`large-documents-rows.ts`, which says the leg the row runs in, the rule a bound is held to and
// what the waits it bounds cost).

// The reach of the last row of `large-documents-first-screen.the-chrome.e2e.ts`, taken the other
// way: by a reader with no pointer, whose Tab is the only way they have into a box the page is
// holding.
//
// The hand-over unmounts whatever they were standing on - the space and the chrome are
// different elements in the same slot, so React takes the space off the page in the commit the
// reach turns on, the reach inside it with it. Their focus fell to `<body>` then and their next
// Tab started again at the top of the page, which is the whole page walked a second time to get
// back to the chrome they had just asked for (dispatch://INFRA-341). `ChromeAfterTheFirstBatch`
// draws the reach as a control of its own standing first in the box, carries `tabIndex={-1}` on
// the box, and puts the focus back on the box in the commit the swap turned on.
// `IssuePage.test.tsx` holds that in happy-dom, which lays nothing out and moves no focus of its
// own; this row is the same hand-over under a real browser's own Tab, which is the only place
// the browser's focus and its tab order are the product's rather than a harness's.
//
// Both boxes are driven, because the finding had two halves. The header's is the one the reader
// meets first. The tab strip's keeps a row of `aria-hidden` spans and nothing else
// (`IssueTabsSkeleton`, `features/issue/issue-page-skeleton.tsx`), so without a reach of its own
// a reader tabbed from the header straight past the strip and into the document, with no way to
// ask for a tab at all.
//
// No `@phone`: what this reads is where a Tab leaves the focus, which is the same at either
// width, and the reach it drives is the page's own chrome at both. Untagged, it runs in the
// `chromium` project, and that is the full lane rather than every pull request: the
// pull-request lane names `chromium`, `iphone` and the two stubbed projects and greps `@pr`
// over them (`.github/workflows/dispatch-browser.yaml`'s shard job), so a row without that tag
// is run by the merge queue and the nightly and by a pull request carrying `full-ci`. It stays
// untagged deliberately. The tag is for a real-stack row whose failure a pull request has to
// see first, and the lane's real-stack half is held to 150 rows
// (`e2e/__tests__/pull-request-lane.test.ts`), all 150 of them taken since the eighteenth
// merge, so tagging this row would mean untagging another; and this row seeds a document of
// its own and walks the page's whole tab order twice over, to guard a focus order the merge
// queue catches before it reaches `main`.

/** The document's own draw, in every surface it can stand in: where a reader who tabbed past
 *  the chrome ends up, and the one place this row's second walk must not reach before the tab
 *  strip's reach. */
const theDocumentsOwnDraw = '#first-screen, [aria-label="Document text"], .proof-editor';

/** One element the page put the focus on: enough for a failure to name the stop, and the one
 *  thing the walks below are judged on - whether it stands in the document rather than on the
 *  chrome over it. */
interface Stop {
  /** Whether it stands inside the document's own draw (`theDocumentsOwnDraw`). */
  readonly inTheDocument: boolean;
  /** What a reader hears it as: its `aria-label`, else its own text, cut to one line. */
  readonly name: string;
  /** The element it is, lowercased, so a failure says `button` rather than only a name. */
  readonly tag: string;
}

/** Where the focus stands now, which is a different question from the one above: after the
 *  hand-over the stop the Tab landed on is gone, and this is what the reader was left with. */
interface Here extends Stop {
  /** The focus is on `<body>`, or the page holds none at all: what the hand-over used to
   *  leave behind, and the whole of what this row is about. */
  readonly body: boolean;
}

declare global {
  interface Window {
    /** Every element this page has put the focus on, oldest first, which `watchTheFocus`
     *  records and the two walks of the row below read. */
    __theFocusLanded?: Stop[];
  }
}

/**
 * Records every element this page moves the focus onto, from before its own scripts run.
 *
 * A read of `document.activeElement` after the press cannot see the reach at all. The reach is
 * a focus handler (`ChromeAfterTheFirstBatch`'s `onFocusCapture`) and React treats focus as a
 * discrete event, so the hand-over's commit runs inside the Tab itself: by the time a read
 * from here arrives, the button the Tab landed on has been unmounted and the box holds the
 * focus in its place. The page's own `focusin` records both, in order, which is what says the
 * reader's Tab reached the control and what the page then did with their focus.
 *
 * Install before the navigation; read with `theFocusTrail`.
 */
async function watchTheFocus(page: Page): Promise<void> {
  await page.addInitScript((draw: string) => {
    const landed: Stop[] = [];
    window.__theFocusLanded = landed;
    window.addEventListener(
      "focusin",
      (event) => {
        const target = event.target;
        if (!(target instanceof Element)) {
          return;
        }
        const text = (target.textContent ?? "").replace(/\s+/gu, " ").trim();
        landed.push({
          inTheDocument: target.closest(draw) !== null,
          name: (target.getAttribute("aria-label") ?? text).slice(0, 60),
          tag: target.tagName.toLowerCase(),
        });
      },
      true
    );
  }, theDocumentsOwnDraw);
}

/** Every element the page has put the focus on so far, oldest first. */
function theFocusTrail(page: Page): Promise<Stop[]> {
  return page.evaluate(() => window.__theFocusLanded ?? []);
}

/** Where the focus stands now. `toBeFocused` answers one locator; a walk has to ask the page
 *  what it is standing on, and a row that reads `<body>` has to be able to say so. */
function whereTheFocusIs(page: Page): Promise<Here> {
  return page.evaluate((draw: string) => {
    const active = document.activeElement;
    if (active === null) {
      return { body: true, inTheDocument: false, name: "", tag: "none" };
    }
    const text = (active.textContent ?? "").replace(/\s+/gu, " ").trim();
    return {
      body: active === document.body,
      inTheDocument: active.closest(draw) !== null,
      name: (active.getAttribute("aria-label") ?? text).slice(0, 60),
      tag: active.tagName.toLowerCase(),
    };
  }, theDocumentsOwnDraw);
}

/** Tabs forward until the focus has landed on a stop `arrived` accepts, and answers every stop
 *  it landed on from this walk's first press, in order. Bounded, because a page whose reach is
 *  missing is one this would tab around forever - the bound is far above the stops either walk
 *  of this row takes, and a walk that spends it answers the whole trail, which is what says
 *  where the reader went instead. */
async function tabbingTo(
  page: Page,
  arrived: (stop: Stop) => boolean,
  stops = 60
): Promise<Stop[]> {
  const before = (await theFocusTrail(page)).length;
  for (let press = 0; press < stops; press += 1) {
    await page.keyboard.press("Tab");
    const walked = (await theFocusTrail(page)).slice(before);
    if (walked.some(arrived)) {
      return walked;
    }
  }
  return (await theFocusTrail(page)).slice(before);
}

/** One stop as an assertion names it: the element and what a reader hears it as. A stop is
 *  read as this one string rather than matched field by field, so a walk that ends on the
 *  wrong stop - or on none at all - fails saying what the reader did reach, with the message
 *  the row gave and the trail in it. */
function theStop(stop: Stop): string {
  return `<${stop.tag}> ${stop.name === "" ? "(no name)" : stop.name}`;
}

/** A walk as a failure prints it: one stop a line, in the order the reader met them. */
function theTrail(trail: readonly Stop[]): string {
  return trail.map((stop, index) => `${index}. ${theStop(stop)}`).join("\n");
}

test("a reader with no pointer tabs into each held box and goes on into the chrome it hands them", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-chrome-tabbed.md", proseOnlyDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await watchTheFocus(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    // The page as this reader meets it: the document's own text, the two boxes the chrome will
    // stand in, and nothing of the chrome in either of them.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: aDrawWithinMs,
    });
    await expect(page.getByTestId("issue-header-box")).toBeAttached();
    await expect(page.getByTestId("issue-tabs-box")).toBeAttached();
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toHaveCount(0);
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();

    // The reader starts where a page just opened leaves them, and their first Tab is kept as a
    // handle: it is the stop the page's own tab order starts at, which a reader walked round to
    // the long way whenever this hand-over lost their focus, and which the Tab after the
    // hand-over must therefore not be.
    expect((await whereTheFocusIs(page)).body, "the page did not open with the focus on it").toBe(
      true
    );
    await page.keyboard.press("Tab");
    const theTopOfThePage = await page.evaluateHandle(() => document.activeElement);
    expect((await whereTheFocusIs(page)).body, "the page's first Tab reached nothing").toBe(false);

    // Tabbing on reaches the header's box while its space is still held: the reach is a
    // control of its own standing first in the box, and it says what it brings. It is read
    // from the page's own focus events rather than from `document.activeElement`, because the
    // hand-over has already taken it off the page by the time a read from here lands
    // (`watchTheFocus`).
    const toTheHeader = await tabbingTo(page, (stop) => stop.name === "Show the issue header");
    expect(
      toTheHeader.map(theStop),
      `no Tab reached the held header's own reach. The trail:\n${theTrail(toTheHeader)}`
    ).toContain("<button> Show the issue header");

    // That Tab is the reader reaching for the chrome, and they get it then.
    const header = page.getByTestId("issue-header");
    await expect(header).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole("combobox", { name: "Status" })).toBeVisible();
    // And it was their own Tab that brought it rather than the hold letting go: a deadline that
    // fired would release every box at once, and the tab strip is still a skeleton.
    await expect(page.getByTestId("issue-tabs-box")).toBeAttached();
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toHaveCount(0);

    // The commit that brought it took the control they were standing on off the page, and their
    // focus did not fall to `<body>` with it: the box they reached through is what holds them.
    expect(
      (await whereTheFocusIs(page)).body,
      "the hand-over dropped the reader's focus to <body>"
    ).toBe(false);
    await expect(header.locator("..")).toBeFocused();

    // So their next Tab goes on into the header they asked for, rather than starting the page
    // again at the stop their first Tab took.
    await page.keyboard.press("Tab");
    const intoTheHeader = await whereTheFocusIs(page);
    expect(
      await page.evaluate((top) => document.activeElement === top, theTopOfThePage),
      "the Tab after the hand-over started again at the top of the page"
    ).toBe(false);
    expect(
      await header.evaluate((node) => node.contains(document.activeElement)),
      `the Tab after the hand-over left the header: <${intoTheHeader.tag}> ${intoTheHeader.name}`
    ).toBe(true);

    // The other half: tabbing on out of the header meets the strip's own box, whose space
    // holds nothing else a reader can reach, before it meets the document under it. The walk
    // stops at whichever of the two comes first, and the first one is the reach.
    const theReachOrTheDocument = (stop: Stop) =>
      stop.name === "Show the issue tabs" || stop.inTheDocument;
    const toTheStrip = await tabbingTo(page, theReachOrTheDocument);
    const theFirstOfTheTwo = toTheStrip.find(theReachOrTheDocument);
    expect(
      theFirstOfTheTwo === undefined ? "nothing at all" : theStop(theFirstOfTheTwo),
      `the reader tabbed out of the header without meeting the tab strip's reach. The trail:\n${theTrail(toTheStrip)}`
    ).toBe("<button> Show the issue tabs");

    // And the strip is handed over the same way, with their place kept the same way.
    const tabs = page.getByTestId("issue-tabs");
    await expect(tabs).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toBeVisible();
    expect(
      (await whereTheFocusIs(page)).body,
      "the strip's hand-over dropped the reader's focus to <body>"
    ).toBe(false);
    await expect(tabs.locator("..")).toBeFocused();

    await page.keyboard.press("Tab");
    const intoTheStrip = await whereTheFocusIs(page);
    expect(
      await tabs.evaluate((node) => node.contains(document.activeElement)),
      `the Tab after the strip's hand-over left the strip: <${intoTheStrip.tag}> ${intoTheStrip.name}`
    ).toBe(true);
    // The whole of it was the reader's doing: no first batch ever landed behind them.
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();
  } finally {
    await alice.close();
  }
});
