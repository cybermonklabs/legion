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

// The hold has a deadline, and under load the reader's walk is slower than it. With the socket
// held the server never answers, so the page lets go of every box at once
// `LONGEST_WAIT_FOR_THE_DOCUMENT_MS` after it asked for the document
// (`web/src/features/doc/first-batch.ts`), which is that deadline's whole purpose. Measured at
// adcb2968 on two CPUs beside the full lane's shard 3 of 4 with three workers and no retry, 40
// runs, the page's hold state read in every focus event (dispatch://INFRA-341, R16): the hold
// stood at the reader's first Tab in all 40. The header's reach is the sixteenth stop, after the
// sidebar's fifteen. In 31 runs the page let go of both boxes within a millisecond of each
// other, 10.3 to 11.2 s after it drew their reaches, while the reader was between the second
// and the fifteenth of those stops; in the other 9 the reader's Tab reached the header's reach
// first, and in one of those the deadline then let the tab strip go before the row looked at
// it. No stop of any run landed past a box while the page still held it. So each walk below is
// judged on the page's own word, read in the focus event: a reader whose Tab meets a held box
// must land on its reach, and a reader whose Tab meets a box the page has already handed over
// must land in the chrome it holds. What fails is a Tab that goes past a box the page is
// holding.

/** The two boxes the page holds its chrome in until the editor's first batch, each found by
 *  what stands in it - the part itself, or the space kept for it while the part is held or its
 *  chunk arrives - and by the reach the box draws first in it for as long as it holds
 *  (`ChromeAfterTheFirstBatch`, `features/issue/IssuePage.tsx`). The box is the element both
 *  stand in, and it stands through the whole hand-over. */
const theBoxes = {
  header: {
    part: '[data-testid="issue-header"]',
    reach: "Show the issue header",
    space: '[data-testid="issue-header-box"]',
  },
  strip: {
    part: '[data-testid="issue-tabs"]',
    reach: "Show the issue tabs",
    space: '[data-testid="issue-tabs-box"]',
  },
} as const;

/** Where a stop stood against one of those boxes when the focus landed on it: before the box in
 *  the page's order, in it, or past it - or `no box`, on a page drawing neither - with whether
 *  the page was still holding the box, which is its reach standing in it, and whether the part
 *  itself stood in it. Read in the focus event itself, so it is the page as the reader's Tab met
 *  it rather than as a later read finds it: the hold can let go between any two reads from
 *  here. */
interface AgainstTheBox {
  readonly drawn: boolean;
  readonly held: boolean;
  readonly where: "before" | "in" | "no box" | "past";
}

/** One element the page put the focus on: enough for a failure to name the stop, and what the
 *  walks below are judged on - where it stood against each box, and whether the page was holding
 *  that box then. */
interface Stop {
  readonly header: AgainstTheBox;
  /** What a reader hears it as: its `aria-label`, else its own text, cut to one line. */
  readonly name: string;
  readonly strip: AgainstTheBox;
  /** The element it is, lowercased, so a failure says `button` rather than only a name. */
  readonly tag: string;
}

/** Where the focus stands now, which is a different question from the one above: after the
 *  hand-over the stop the Tab landed on is gone, and this is what the reader was left with. */
interface Here {
  /** The focus is on `<body>`, or the page holds none at all: what the hand-over used to
   *  leave behind, and the whole of what this row is about. */
  readonly body: boolean;
  readonly name: string;
  readonly tag: string;
}

declare global {
  interface Window {
    /** Every element this page has put the focus on, oldest first, which `watchTheFocus`
     *  records and the two walks of the row below read. */
    __theFocusLanded?: Stop[];
  }
}

/**
 * Records every element this page moves the focus onto, from before its own scripts run, with
 * where it stood against each box and whether the page was holding that box.
 *
 * A read of `document.activeElement` after the press cannot see the reach at all. The reach is
 * a focus handler (`ChromeAfterTheFirstBatch`'s `onFocusCapture`) and React treats focus as a
 * discrete event, so the hand-over's commit runs inside the Tab itself: by the time a read
 * from here arrives, the button the Tab landed on has been unmounted and the box holds the
 * focus in its place. The page's own `focusin` records both, in order, which is what says the
 * reader's Tab reached the control and what the page then did with their focus. Its listener
 * is the window's, in the capture phase, so it runs before React's own on the page's root and
 * reads the boxes as they stood when the focus landed.
 *
 * Install before the navigation; read with `theFocusTrail`.
 */
async function watchTheFocus(page: Page): Promise<void> {
  await page.addInitScript((boxes: typeof theBoxes) => {
    const landed: Stop[] = [];
    window.__theFocusLanded = landed;
    const against = (
      target: Element,
      { part, reach, space }: { part: string; reach: string; space: string }
    ): AgainstTheBox => {
      const box = document.querySelector(`${part}, ${space}`)?.parentElement ?? null;
      if (box === null) {
        return { drawn: false, held: false, where: "no box" };
      }
      const drawn = box.querySelector(`:scope > ${part}`) !== null;
      const held = [...box.children].some(
        (child) => child.tagName === "BUTTON" && child.textContent === reach
      );
      if (box.contains(target)) {
        return { drawn, held, where: "in" };
      }
      const past = (box.compareDocumentPosition(target) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
      return { drawn, held, where: past ? "past" : "before" };
    };
    window.addEventListener(
      "focusin",
      (event) => {
        const target = event.target;
        if (!(target instanceof Element)) {
          return;
        }
        const text = (target.textContent ?? "").replace(/\s+/gu, " ").trim();
        landed.push({
          header: against(target, boxes.header),
          name: (target.getAttribute("aria-label") ?? text).slice(0, 60),
          strip: against(target, boxes.strip),
          tag: target.tagName.toLowerCase(),
        });
      },
      true
    );
  }, theBoxes);
}

/** Every element the page has put the focus on so far, oldest first. */
function theFocusTrail(page: Page): Promise<Stop[]> {
  return page.evaluate(() => window.__theFocusLanded ?? []);
}

/** Where the focus stands now. `toBeFocused` answers one locator; a walk has to ask the page
 *  what it is standing on, and a row that reads `<body>` has to be able to say so. */
function whereTheFocusIs(page: Page): Promise<Here> {
  return page.evaluate(() => {
    const active = document.activeElement;
    if (active === null) {
      return { body: true, name: "", tag: "none" };
    }
    const text = (active.textContent ?? "").replace(/\s+/gu, " ").trim();
    return {
      body: active === document.body,
      name: (active.getAttribute("aria-label") ?? text).slice(0, 60),
      tag: active.tagName.toLowerCase(),
    };
  });
}

/** Presses `key` until the focus has landed on a stop `arrived` accepts, and answers that stop
 *  with every stop it landed on from this walk's first press, in order. Bounded, because a
 *  page whose box is missing is one this would tab around forever - the bound is far above the
 *  stops either walk of this row takes, and a walk that spends it answers the whole trail and
 *  no stop, which is what says where the reader went instead. */
async function tabbingTo(
  page: Page,
  arrived: (stop: Stop) => boolean,
  key: "Shift+Tab" | "Tab" = "Tab",
  stops = 60
): Promise<{ at: Stop | undefined; trail: Stop[] }> {
  const before = (await theFocusTrail(page)).length;
  for (let press = 0; press < stops; press += 1) {
    await page.keyboard.press(key);
    const trail = (await theFocusTrail(page)).slice(before);
    const at = trail.find(arrived);
    if (at !== undefined) {
      return { at, trail };
    }
  }
  return { at: undefined, trail: (await theFocusTrail(page)).slice(before) };
}

/** One stop as an assertion names it: the element and what a reader hears it as. A stop is
 *  read as this one string rather than matched field by field, so a walk that ends on the
 *  wrong stop - or on none at all - fails saying what the reader did reach, with the message
 *  the row gave and the trail in it. */
function theStop(stop: Stop | undefined): string {
  if (stop === undefined) {
    return "nothing at all";
  }
  return `<${stop.tag}> ${stop.name === "" ? "(no name)" : stop.name}`;
}

/** A walk as a failure prints it: one stop a line, in the order the reader met them, each with
 *  where it stood against the two boxes and what the page had in them then. */
function theTrail(trail: readonly Stop[]): string {
  const against = ({ drawn, held, where }: AgainstTheBox) =>
    `${held ? "held" : "handed over"}${held || drawn ? "" : ", not drawn"}, ${where}`;
  return trail
    .map(
      (stop, index) =>
        `${index}. ${theStop(stop)} [header ${against(stop.header)}; strip ${against(stop.strip)}]`
    )
    .join("\n");
}

/** Whether the page had already handed `box` over when the walk reached it: the reader's Tab
 *  landed in the box, on the chrome it holds, with the box no longer holding. */
function handedOverBeforeTheReach(box: AgainstTheBox | undefined): boolean {
  return box !== undefined && !box.held && box.where === "in";
}

/** Whether the reader's Tab went past `box` while it held nothing to land on: the page had let
 *  go of it and its part had not been drawn into it yet - the few hundred milliseconds its
 *  chunk and its render take after a deadline - so the space alone stood there. That is no Tab
 *  skipping a hold, and a reader who finds the chrome arrived behind them goes back for it. */
function passedBeforeItWasDrawn(box: AgainstTheBox | undefined): boolean {
  return box !== undefined && !box.held && !box.drawn && box.where === "past";
}

test("a reader with no pointer tabs into each held box and goes on into the chrome it hands them", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-chrome-tabbed.md", proseOnlyDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const header = page.getByTestId("issue-header");
    const tabs = page.getByTestId("issue-tabs");
    await watchTheReads(page);
    await watchTheFocus(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    // The page as this reader meets it: the document's own text, with its socket held so no
    // batch comes, and the issue page's own two boxes - not the app's skeleton of them, which
    // stands there while the page's chunk arrives and holds no reach. Each holds its reach
    // while the page holds it, or the part once the page has let go; which of the two is read
    // stop by stop below, in the focus events, because the page's deadline can hand them over
    // between any two reads from here.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: aDrawWithinMs,
    });
    await expect(
      page.getByRole("button", { name: theBoxes.header.reach }).or(header)
    ).toBeAttached();
    await expect(page.getByRole("button", { name: theBoxes.strip.reach }).or(tabs)).toBeAttached();

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

    // Tabbing on reaches the header's box. The walk stops at the first stop that is not before
    // it, and what that stop must be is the page's to say: the reach, a control of its own
    // standing first in the box, while the page holds it; the header's own chrome once the page
    // has let go of it. Anything else is a Tab that went past the box - unless the page had let
    // go and not yet drawn the header, when the reader goes back for it once it is there.
    let toTheHeader = await tabbingTo(page, (stop) => stop.header.where !== "before");
    if (passedBeforeItWasDrawn(toTheHeader.at?.header)) {
      await expect(header).toBeVisible();
      toTheHeader = await tabbingTo(page, (stop) => stop.header.where !== "past", "Shift+Tab");
    }
    if (handedOverBeforeTheReach(toTheHeader.at?.header)) {
      // The page let go before the reader got there, and it let go of every box at once, which
      // is its deadline: the reader's own reach brings its one box, and no first batch can come
      // through the held socket, which the row reads at its end. Their Tab went on into the
      // header it handed them.
      expect(
        toTheHeader.at?.strip.held,
        `the page let go of the header alone before the reader reached it. The trail:\n${theTrail(toTheHeader.trail)}`
      ).toBe(false);
      await expect(header).toBeVisible();
    } else {
      expect(
        theStop(toTheHeader.at),
        `no Tab reached the held header's own reach. The trail:\n${theTrail(toTheHeader.trail)}`
      ).toBe("<button> Show the issue header");

      // That Tab is the reader reaching for the chrome, and they get it then.
      await expect(header).toBeVisible({ timeout: 30_000 });
      await expect(page.getByRole("combobox", { name: "Status" })).toBeVisible();
      // And their reach handed over the header alone. The reach's own entry cannot say so: it
      // is read in the reach's `focusin`, before React commits the hand-over, so it says only
      // that the deadline had not fired. The entry after it can: the box taking the focus in the
      // commit the reach turned on, inside the same task, where no deadline can fire. That entry
      // stands in the header's box, and the tab strip is still held in it; a reach that let go
      // of every box would read the strip handed over there.
      const { at: reached, trail } = toTheHeader;
      const tookTheFocus = reached === undefined ? undefined : trail[trail.indexOf(reached) + 1];
      expect(
        tookTheFocus?.header.where,
        `the header's box did not take the focus in the task of its reach. The trail:\n${theTrail(trail)}`
      ).toBe("in");
      expect(
        tookTheFocus?.strip.held,
        "the tab strip was handed over in the commit of the header's reach, so the reach let go " +
          `of every box rather than the header alone. The trail:\n${theTrail(trail)}`
      ).toBe(true);

      // The commit that brought it took the control they were standing on off the page, and
      // their focus did not fall to `<body>` with it: the box they reached through is what
      // holds them.
      expect(
        (await whereTheFocusIs(page)).body,
        "the hand-over dropped the reader's focus to <body>"
      ).toBe(false);
      await expect(header.locator("..")).toBeFocused();

      // So their next Tab goes on into the header they asked for, rather than starting the
      // page again at the stop their first Tab took.
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
    }

    // The other half: tabbing on out of the header meets the strip's own box, whose space
    // holds nothing else a reader can reach first, before it meets the document under it. The
    // walk stops at the first stop that is not before the box, judged the same way.
    let toTheStrip = await tabbingTo(page, (stop) => stop.strip.where !== "before");
    if (passedBeforeItWasDrawn(toTheStrip.at?.strip)) {
      await expect(tabs).toBeVisible();
      toTheStrip = await tabbingTo(page, (stop) => stop.strip.where !== "past", "Shift+Tab");
    }
    if (handedOverBeforeTheReach(toTheStrip.at?.strip)) {
      // The page let go of the strip before the reader got there, and their Tab went on into
      // the strip it handed them.
      await expect(tabs).toBeVisible();
      await expect(page.getByRole("tab", { name: /^Spec$/ })).toBeVisible();
    } else {
      expect(
        theStop(toTheStrip.at),
        `the reader tabbed out of the header without meeting the tab strip's reach. The trail:\n${theTrail(toTheStrip.trail)}`
      ).toBe("<button> Show the issue tabs");

      // And the strip is handed over the same way, with their place kept the same way.
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
    }
    // No first batch ever landed behind them: what let go of each box was the reader's own Tab
    // or the page's deadline, never the editor.
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();
  } finally {
    await alice.close();
  }
});
