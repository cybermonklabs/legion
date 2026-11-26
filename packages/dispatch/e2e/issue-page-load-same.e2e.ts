/** the rows compare each view of the new issue page, once its load has ended, with the record of
 *  the rows the base drew on the same seed, and reach a row the page draws late in a slowed load
 *  by its keys, an address, the Contents pane and z, which the server's own answers to the issue,
 *  tree and view reads, the order and the keys it gives the seed's writes, and the timing of those
 *  answers decide */

import { type BrowserContext, errors, type Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import type { Preset } from "../web/src/features/issue-page/page-words";
import { DESKTOP, PHONE } from "./issue-page-g";
import {
  HEADER_SELECTORS,
  LOAD_VIEWS,
  type LoadOptions,
  oneLoad,
  rowAddress,
  test,
} from "./issue-page-load";
import { OWED_BY, owedUnless } from "./issue-page-t-tree";
import { waitForLoadEnd } from "./load-instruments";
import { type Browser, expect } from "./test";
import {
  CLAUSE_WORDS,
  CLAUSES,
  type Naming,
  RECORD_VARIABLE,
  type RecordWidth,
  readView,
  recordedView,
  recordLine,
  recordWidth,
  seedNaming,
  type ViewRecord,
  viewDifferences,
  viewRecord,
  widthRecord,
} from "./tools/issue-page-load-record";

/**
 * The rows of `c-same-page` and `c-late-row` (Verification v4), in the parallel projects:
 * `chromium` at 1440 by 900 and `iphone` at 390 by 844 with the phone's emulation, the viewports
 * the record is made at (`test.use` below; `chromium`'s own is 1280 by 720). They read the load
 * rows' worker seed (`seedLargestShape`, the `test` of `e2e/issue-page-load.ts`), so they run in
 * the order they are written, in one worker of each project.
 *
 * - `record the rows the base draws @phone` makes the record, at the base, only where
 *   `ISSUE_PAGE_LOAD_RECORD=1`: one load of each view the base draws, read once its load has
 *   ended (`readView`, `viewRecord`), printed as one `LOAD-RECORD <json>` line for its width,
 *   which `e2e/tools/issue-page-load-record.ts` writes into `e2e/fixtures/issue-page-load-rows.json`
 *   (its head says how).
 * - `once the load has ended, <view> holds the rows the base draws @phone`, for each view of
 *   `LOAD_VIEWS`, Dashboard, Threads and Documents owed until their parts add them to the view
 *   row: one load of the view's own address, read as the record was, and compared with the
 *   record's view at its width clause by clause (`viewDifferences`), each clause a soft assertion
 *   of its own. Rows are paired by key and their parent's key, never by their markup.
 *
 *   What may differ, and why: the record is made on one seed and the page read on another, and
 *   `seedLargestShape` writes the children, its asks and the posts six at a time, so the server
 *   gives those writes their times and child keys in the order it took them, which another run
 *   does not repeat. The two seeds are not the same data in exactly those places, so two things,
 *   and nothing else, may differ: (1) the order of two rows under one parent that the server
 *   orders by the same measure (when it took the write, or its key's number) where the two
 *   seeds' server order of them differs or ties; (2) under a parent whose rows past its first
 *   page are held back, which of its server-ordered children stand at the page's edge, with the
 *   rows under them, while the number drawn before the control and the control's words are the
 *   base's. A focused row that holds the same place in the server's order is the same focus.
 *   The head of `e2e/tools/issue-page-load-record.ts` holds the same rule, and its test
 *   `e2e/__tests__/issue-page-load-record.test.ts` each part of it.
 * - `a row drawn late is reached by j, k, an address, the Contents pane and z`, in `chromium`
 *   alone, makes each act in a load of Conversation with the processor slowed by 4, at the first
 *   drawing of the rows, and asserts first that the row the act must reach was not in the
 *   document then, so it fails on a page that draws every row at once: (1) `j`, from there to
 *   the last row, then `k` back to the first, each press onto the next of the visible rows the
 *   load draws once it has ended (which `c-same-page` holds to the base's order); (2) the
 *   address of the last row, followed as a link of the page follows one (the history takes it
 *   and the router hears it), leaves that row in the viewport under the header with the focus;
 *   (3) a press on its entry in the Contents pane does the same; (4) a fold of a row drawn at
 *   once and of the last row that holds rows, drawn late, then `z`, opens both, and the page
 *   draws every row a load with no fold draws. An address and an entry name a node and land on
 *   its own row, so (2) takes the last row whose node no other row draws and an address names
 *   (a block's or an item's), and (3) the last such row the Contents pane lists, since it lists
 *   asks and the threads the reader began.
 * - The same row's filter by words, a row of its own, owed until part N puts its filter in the
 *   view row: the last row's first words typed into it at the first drawing of the rows draw
 *   that row once the load has ended.
 */

// The rows share the worker's seed and are read in the order written: `default` runs them in one
// worker in that order, where `serial` would skip every row after one that failed.
test.describe.configure({ mode: "default" });

// Each context these rows open, `oneLoad`'s included, opens at the record's viewport: the phone's
// in `iphone`, whose device emulates a phone, and the desktop's in `chromium`.
test.use({
  viewport: async ({ isMobile }, use) => {
    await use(isMobile ? PHONE : DESKTOP);
  },
});

/** The processor's own rate: the record and its comparison read the page, not its time. */
const UNSLOWED = 1;

/** How much `c-late-row` slows the processor, so the page's later rows are drawn late. */
const LATE_ROW_RATE = 4;

/** How long the record row may take: a load of each of six views, every row's tree read. */
const RECORD_MS = 1_200_000;

/** How long a row of one view's comparison may take. */
const SAME_PAGE_MS = 300_000;

/** How long the late row may take: four slowed loads, and `j` and `k` pressed for each row. */
const LATE_ROW_MS = 900_000;

/** How long a key may take to move the focus before it is taken to have moved it nowhere. */
const STEP_MS = 15_000;

/** How long a row may take to be drawn: the first of a slowed load, or one drawn late. */
const DRAWN_MS = 120_000;

/** More presses of `j` than Conversation draws rows, so a press that keeps moving the focus
 *  forever fails the row rather than its time limit. */
const MOST_STEPS = 2_000;

/** How many differences of one clause a row's failure shows. */
const SHOWN = 20;

/** How many of the last row's words the filter by words is given. */
const FILTER_WORDS = 4;

/** Conversation's rows, a row of the outline each. */
const OUTLINE_ROWS = "[data-issue-body] [data-node-row]";

/** An entry of the Contents pane (`IssueContents.tsx`), by the node's id. */
const CONTENTS_ENTRY = "[data-contents-entry]";

/** A row's fold control (`NodeRow.tsx`). */
const FOLD_CONTROL = "button[data-node-bullet]";

/** Part N's filter by words, which the view row does not hold at this head. */
const WORDS_FILTER = '[data-filter-row] input[type="search"]';

/** The late row's title, which its owed clause's row begins with. */
const LATE_ROW_TITLE = "a row drawn late is reached by j, k, an address, the Contents pane and z";

/** What the page held at the first drawing of the rows: the outline's rows, by their keys and
 *  their nodes, those of them open, and the Contents pane's entries. */
interface FirstDrawing {
  readonly entries: readonly string[];
  readonly ids: readonly string[];
  readonly keys: readonly string[];
  readonly open: readonly string[];
}

declare global {
  interface Window {
    __firstDrawing?: FirstDrawing;
  }
}

/** One row of the outline, as `outlineRows` reads it. */
interface OutlineRow {
  /** Whether the Contents pane lists its node. */
  readonly entry: boolean;
  /** `aria-expanded`, null on a row that holds no rows. */
  readonly expanded: string | null;
  readonly id: string;
  readonly key: string;
  readonly type: string;
  /** Whether it is drawn, and not inside a folded row. */
  readonly visible: boolean;
  /** Its node's words, as `[data-node-words]` draws them. */
  readonly words: string;
}

/** A value inside a CSS attribute selector's quotes. */
function quotedValue(value: string): string {
  return value.replace(/["\\]/g, "\\$&");
}

/** One load of `address` at the processor's own rate, read once its load has ended and named as
 *  the record names it. */
async function loadedView(
  browser: Browser,
  address: string,
  width: RecordWidth,
  naming: Naming,
  where: string
): Promise<ViewRecord> {
  return await oneLoad(
    browser,
    UNSLOWED,
    { address, boxes: "none" },
    async ({ page, requests }) => {
      await waitForLoadEnd(page, requests);
      return viewRecord(await readView(page, width), naming, where);
    }
  );
}

// SLOW(>=2s): one load of each view the base draws, each waited out and every row's tree read.
test("record the rows the base draws @phone", async ({ browser, loadSeeds, viewport }) => {
  test.skip(
    process.env[RECORD_VARIABLE] !== "1",
    `the record is made only where ${RECORD_VARIABLE}=1, at the base, for e2e/fixtures/issue-page-load-rows.json`
  );
  test.setTimeout(RECORD_MS);
  const width = recordWidth(viewport);
  const seeded = await loadSeeds.shape();
  const naming = await seedNaming(seeded);
  const views: Record<string, ViewRecord> = {};
  for (const view of LOAD_VIEWS) {
    if (!view.gate.met) continue;
    const where = `${view.name} at ${width} px`;
    views[view.view] = await loadedView(
      browser,
      presetPath(seeded.key, view.view as Preset),
      width,
      naming,
      where
    );
  }
  console.log(recordLine(widthRecord(width, views)));
});

for (const view of LOAD_VIEWS) {
  // SLOW(>=2s): one load of the seeded issue's view, waited out, and every row's tree read.
  test(`once the load has ended, ${view.name} holds the rows the base draws @phone`, async ({
    browser,
    loadSeeds,
    viewport,
  }) => {
    owedUnless(view.gate);
    test.setTimeout(SAME_PAGE_MS);
    const width = recordWidth(viewport);
    const recorded = recordedView(view.view, width);
    const seeded = await loadSeeds.shape();
    const where = `${view.name} at ${width} px`;
    const drawn = await loadedView(
      browser,
      presetPath(seeded.key, view.view as Preset),
      width,
      await seedNaming(seeded),
      where
    );
    const differences = viewDifferences(recorded, drawn);
    for (const clause of CLAUSES) {
      const lines = differences.filter((one) => one.clause === clause).map((one) => one.line);
      expect
        .soft(
          lines.slice(0, SHOWN),
          `${where}: ${CLAUSE_WORDS[clause]}, and ${lines.length} differ from the record`
        )
        .toEqual([]);
    }
  });
}

/** Keeps, on each page `context` opens, what the page holds at the first drawing of the rows: the
 *  first frame whose document holds a row of Conversation's outline, read in that frame's
 *  callback, before anything after it is drawn. */
async function watchFirstDrawing(context: BrowserContext): Promise<void> {
  await context.addInitScript(
    ({ entries, rows }) => {
      if (window.top !== window) return;
      const look = () => {
        const drawn = [...document.querySelectorAll(rows)];
        if (drawn.length === 0) {
          requestAnimationFrame(look);
          return;
        }
        window.__firstDrawing = {
          entries: [...document.querySelectorAll(entries)].map(
            (entry) => entry.getAttribute("data-contents-entry") ?? ""
          ),
          ids: drawn.map((row) => row.getAttribute("data-node-id") ?? ""),
          keys: drawn.map((row) => row.getAttribute("data-row-key") ?? ""),
          open: drawn
            .filter((row) => row.getAttribute("aria-expanded") === "true")
            .map((row) => row.getAttribute("data-row-key") ?? ""),
        };
      };
      requestAnimationFrame(look);
    },
    { entries: CONTENTS_ENTRY, rows: OUTLINE_ROWS }
  );
}

/** A load of `address` for the late row: what it holds at the first drawing of the rows is kept
 *  (`watchFirstDrawing`). */
function lateLoad(address: string): LoadOptions {
  return { address, boxes: "none", prepare: watchFirstDrawing };
}

/** What the page held at the first drawing of the rows, once it has come. */
async function firstDrawing(page: Page): Promise<FirstDrawing> {
  const drawing = await page.waitForFunction(() => window.__firstDrawing ?? null, undefined, {
    polling: "raf",
    timeout: DRAWN_MS,
  });
  const first = await drawing.jsonValue();
  if (first === null) throw new Error("the page drew no row of Conversation's outline");
  return first;
}

/** Every row of the outline now. */
function outlineRows(page: Page): Promise<OutlineRow[]> {
  return page.evaluate(
    ({ entries, rows }) => {
      const listed = new Set(
        [...document.querySelectorAll(entries)].map((entry) =>
          entry.getAttribute("data-contents-entry")
        )
      );
      return [...document.querySelectorAll(rows)].map((row) => ({
        entry: listed.has(row.getAttribute("data-node-id")),
        expanded: row.getAttribute("aria-expanded"),
        id: row.getAttribute("data-node-id") ?? "",
        key: row.getAttribute("data-row-key") ?? "",
        type: row.getAttribute("data-node-type") ?? "",
        visible: row.getClientRects().length > 0,
        words: (row.querySelector("[data-node-words]")?.textContent ?? "")
          .replace(/\s+/g, " ")
          .trim(),
      }));
    },
    { entries: CONTENTS_ENTRY, rows: OUTLINE_ROWS }
  );
}

/** The key of the row of the outline that holds the focus, null where none does. */
function focusedRow(page: Page): Promise<string | null> {
  return page.evaluate(
    (rows) => document.activeElement?.closest(rows)?.getAttribute("data-row-key") ?? null,
    OUTLINE_ROWS
  );
}

/** Presses `key` up to `most` times, each once the press before it has moved the focus to
 *  another row, and answers the row each press moved it to; it stops at a press that moves it
 *  nowhere within `STEP_MS`. */
async function walk(page: Page, key: "j" | "k", most: number): Promise<string[]> {
  const reached: string[] = [];
  let from = await focusedRow(page);
  while (reached.length < most) {
    await page.keyboard.press(key);
    let moved: string | null;
    try {
      const handle = await page.waitForFunction(
        ({ rows, start }) => {
          const now = document.activeElement?.closest(rows)?.getAttribute("data-row-key") ?? null;
          return now !== null && now !== start ? now : null;
        },
        { rows: OUTLINE_ROWS, start: from },
        { polling: "raf", timeout: STEP_MS }
      );
      moved = await handle.jsonValue();
    } catch (error) {
      if (!(error instanceof errors.TimeoutError)) throw error;
      moved = null;
    }
    if (moved === null) break;
    reached.push(moved);
    from = moved;
  }
  return reached;
}

/** Where the presses of `key` first left the rows they must visit, in order, or null where they
 *  visited each, and no other. */
function walkMismatch(
  key: string,
  reached: readonly string[],
  expected: readonly string[]
): string | null {
  const at = expected.findIndex((row, index) => reached[index] !== row);
  if (at === -1) {
    return reached.length > expected.length
      ? `${key} went on past the last row it must reach, to ${reached[expected.length]}`
      : null;
  }
  const from = at === 0 ? "the row it started on" : expected[at - 1];
  return reached[at] === undefined
    ? `${key}, pressed ${at + 1} times, left the focus on ${from}, where ${expected[at]} follows it`
    : `${key}, pressed ${at + 1} times, put the focus on ${reached[at]}, where ${expected[at]} follows ${from}`;
}

/** The last row whose node no other row draws, of those `also` keeps: an address and the Contents
 *  pane name a node, and land on its own row. */
function lastDrawnOnce(
  rows: readonly OutlineRow[],
  what: string,
  also: (row: OutlineRow) => boolean = () => true
): OutlineRow {
  const drawn = new Map<string, number>();
  for (const row of rows) drawn.set(row.id, (drawn.get(row.id) ?? 0) + 1);
  const found = rows.filter((row) => drawn.get(row.id) === 1 && also(row)).at(-1);
  if (found === undefined) throw new Error(`Conversation draws no row ${what}`);
  return found;
}

/** Fails the row where the row an act must reach was in the document at the first drawing of the
 *  rows: then the act reached no row drawn late. */
function expectDrawnLate(drawnThen: boolean, row: OutlineRow, act: string): void {
  expect(
    drawnThen,
    `the row ${act} must reach, ${row.key}, was in the document at the first drawing of the ` +
      "rows, so this row reads no row drawn late: the page drew it at once"
  ).toBe(false);
}

/** Where a landing left the focus: the node of the outline's row that holds it, its top in the
 *  viewport (null where it is not in the viewport), and the header's foot. */
interface Landing {
  readonly focused: string | null;
  readonly foot: number;
  readonly top: number | null;
}

/** Which row of the outline holds the focus once a landing is over, where it stands, and where
 *  the header's foot is. */
function landing(page: Page): Promise<Landing> {
  return page.evaluate(
    ({ header, rows }) => {
      let foot = 0;
      for (const selector of header) {
        for (const part of document.querySelectorAll(selector)) {
          const box = part.getBoundingClientRect();
          if (box.height > 0) foot = Math.max(foot, box.bottom);
        }
      }
      const focused = document.activeElement?.closest(rows) ?? null;
      const box = focused?.getBoundingClientRect();
      return {
        focused: focused?.getAttribute("data-node-id") ?? null,
        foot: Math.round(foot),
        top:
          box === undefined || box.height === 0 || box.top >= window.innerHeight
            ? null
            : Math.round(box.top),
      };
    },
    { header: HEADER_SELECTORS, rows: OUTLINE_ROWS }
  );
}

/** Holds `landed` to the row `row`: it has the focus and stands in the viewport under the
 *  header. */
function expectLanded(landed: Landing, row: OutlineRow, act: string): void {
  expect
    .soft(
      landed.focused === row.id && landed.top !== null && landed.top >= landed.foot - 1,
      `${act} leaves the focus on ${landed.focused ?? "no row"}, ` +
        `${landed.top === null ? "outside the viewport" : `at ${landed.top} px`}, where the row ` +
        `${row.key} must have it in the viewport under the header's foot at ${landed.foot} px`
    )
    .toBe(true);
}

/** Moves the page to `address` as one of its own links does: the history takes the address and
 *  the router hears it, as it hears Back, so the load goes on. */
async function followAddress(page: Page, address: string): Promise<void> {
  await page.evaluate((to) => {
    window.history.pushState(null, "", to);
    window.dispatchEvent(new PopStateEvent("popstate", { state: null }));
  }, address);
}

/** The selector of the outline's row of key `key`. */
function rowSelector(key: string): string {
  return `${OUTLINE_ROWS}[data-row-key="${quotedValue(key)}"]`;
}

/** Folds the row `key` by a press on its fold control. */
async function fold(page: Page, key: string): Promise<void> {
  const row = page.locator(rowSelector(key));
  await row.locator(FOLD_CONTROL).first().click();
  await expect(row, `a press on the fold control of ${key} did not fold it`).toHaveAttribute(
    "aria-expanded",
    "false"
  );
}

// SLOW(>=2s): four loads at the processor slowed by 4, each act made at the first drawing of the
// rows and each load waited out, with j and k pressed once for each row of Conversation.
test(LATE_ROW_TITLE, async ({ browser, loadSeeds }) => {
  test.setTimeout(LATE_ROW_MS);
  const seeded = await loadSeeds.shape();
  const address = presetPath(seeded.key, "conversation");

  // (1) `j`, from the first drawing of the rows, to the last row, and `k` back to the first.
  const walked = await oneLoad(
    browser,
    LATE_ROW_RATE,
    lateLoad(address),
    async ({ page, requests }) => {
      const first = await firstDrawing(page);
      const start = await focusedRow(page);
      const forward = await walk(page, "j", MOST_STEPS);
      await waitForLoadEnd(page, requests);
      const rows = await outlineRows(page);
      const visible = rows.filter((row) => row.visible).map((row) => row.key);
      const last = visible.indexOf((await focusedRow(page)) ?? "");
      const back = await walk(page, "k", Math.max(last, 0));
      return { back, first, forward, last, rows, start, visible };
    }
  );
  const lastRow = walked.rows.at(-1);
  if (lastRow === undefined) throw new Error("Conversation drew no row once its load had ended");
  expectDrawnLate(walked.first.keys.includes(lastRow.key), lastRow, "j");
  const from = walked.start === null ? -1 : walked.visible.indexOf(walked.start);
  expect
    .soft(
      walkMismatch("j", walked.forward, walked.visible.slice(from + 1)),
      "j, pressed once for each row from the first drawing of the rows, puts the focus on each " +
        "visible row in the order the page draws them once its load has ended, to the last"
    )
    .toBeNull();
  expect
    .soft(
      walkMismatch("k", walked.back, walked.visible.slice(0, Math.max(walked.last, 0)).reverse()),
      "k, pressed once for each row from the last, puts the focus on each visible row back to the first"
    )
    .toBeNull();

  // (2) An address that names the last row, followed at the first drawing of the rows. An
  // address names a block or an item (`rowAddress`), so the row is the last of those.
  const named = lastDrawnOnce(
    walked.rows,
    "whose node no other row draws and an address names",
    (row) => row.id.includes("#b-") || ["ask", "comment", "message"].includes(row.type)
  );
  const namedAddress = rowAddress(seeded.key, named);
  if (namedAddress === undefined) throw new Error(`no address names the row ${named.key}`);
  const addressed = await oneLoad(
    browser,
    LATE_ROW_RATE,
    lateLoad(address),
    async ({ page, requests }) => {
      const first = await firstDrawing(page);
      await followAddress(page, namedAddress);
      await waitForLoadEnd(page, requests);
      return { first, landed: await landing(page) };
    }
  );
  expectDrawnLate(addressed.first.ids.includes(named.id), named, "an address");
  expectLanded(addressed.landed, named, "the address that names it");

  // (3) A press on the last row's entry in the Contents pane, at the first drawing of the rows.
  const listed = lastDrawnOnce(walked.rows, "that the Contents pane lists", (row) => row.entry);
  const pressed = await oneLoad(
    browser,
    LATE_ROW_RATE,
    lateLoad(address),
    async ({ page, requests }) => {
      const first = await firstDrawing(page);
      await page
        .locator(
          `${CONTENTS_ENTRY}[data-contents-entry="${quotedValue(listed.id)}"] [data-contents-line]`
        )
        .click();
      await waitForLoadEnd(page, requests);
      return { first, landed: await landing(page) };
    }
  );
  expectDrawnLate(pressed.first.ids.includes(listed.id), listed, "the Contents pane");
  expectLanded(pressed.landed, listed, "a press on its entry in the Contents pane");

  // (4) `z`, after the reader folded a row drawn at once and the last row that holds rows, which
  // is drawn late.
  const late = walked.rows.filter((row) => row.expanded !== null).at(-1);
  if (late === undefined) throw new Error("Conversation draws no row that holds rows to fold");
  const unfolded = await oneLoad(
    browser,
    LATE_ROW_RATE,
    lateLoad(address),
    async ({ page, requests }) => {
      const first = await firstDrawing(page);
      const early = first.open.find((key) => key !== late.key && !late.key.startsWith(`${key}>`));
      if (early === undefined) {
        throw new Error("no row open at the first drawing of the rows holds rows to fold");
      }
      await fold(page, early);
      await page.locator(rowSelector(late.key)).waitFor({ state: "attached", timeout: DRAWN_MS });
      await fold(page, late.key);
      await page.keyboard.press("z");
      await waitForLoadEnd(page, requests);
      return { early, first, rows: await outlineRows(page) };
    }
  );
  expectDrawnLate(unfolded.first.keys.includes(late.key), late, "z");
  const expandedOf = (key: string) =>
    unfolded.rows.find((row) => row.key === key)?.expanded ?? "no row";
  expect
    .soft(
      [expandedOf(unfolded.early), expandedOf(late.key)],
      `z leaves a row the reader folded folded: ${unfolded.early}, drawn at once, and ` +
        `${late.key}, drawn late`
    )
    .toEqual(["true", "true"]);
  const shown = unfolded.rows.filter((row) => row.visible).map((row) => row.key);
  const differs = walked.visible.findIndex((key, index) => shown[index] !== key);
  expect
    .soft(
      differs === -1 && shown.length === walked.visible.length,
      "after z the page draws every row a load with no fold draws, each visible, in its order: " +
        (differs === -1
          ? `it draws ${shown.length} visible rows, where that load draws ${walked.visible.length}`
          : `visible row ${differs + 1} is ${shown[differs] ?? "missing"}, where that load draws ` +
            `${walked.visible[differs]}`)
    )
    .toBe(true);
});

// SLOW(>=2s): a load to read the view row and the last row, then a load at the processor slowed
// by 4 with the last row's words typed into the filter at the first drawing of the rows.
test(`${LATE_ROW_TITLE}, and by the filter by words @owed`, async ({ browser, loadSeeds }) => {
  test.setTimeout(LATE_ROW_MS);
  const seeded = await loadSeeds.shape();
  const address = presetPath(seeded.key, "conversation");
  const opened = await oneLoad(
    browser,
    UNSLOWED,
    { address, boxes: "none" },
    async ({ page, requests }) => {
      await waitForLoadEnd(page, requests);
      return { filters: await page.locator(WORDS_FILTER).count(), rows: await outlineRows(page) };
    }
  );
  owedUnless({
    met: opened.filters > 0,
    owed:
      "owed until INFRA-3940, part N, puts its filter by words in the view row: the view row " +
      `holds no ${WORDS_FILTER} here; ${OWED_BY}`,
  });
  const target = lastDrawnOnce(
    opened.rows,
    "whose node no other row draws and that has words",
    (row) => row.words !== ""
  );
  const words = target.words.split(" ").slice(0, FILTER_WORDS).join(" ");
  const filtered = await oneLoad(
    browser,
    LATE_ROW_RATE,
    lateLoad(address),
    async ({ page, requests }) => {
      const first = await firstDrawing(page);
      await page.locator(WORDS_FILTER).first().fill(words);
      await waitForLoadEnd(page, requests);
      const shown = (await outlineRows(page)).some((row) => row.id === target.id && row.visible);
      return { first, shown };
    }
  );
  expectDrawnLate(filtered.first.ids.includes(target.id), target, "the filter by words");
  expect
    .soft(
      filtered.shown,
      `the filter by words ${JSON.stringify(words)} draws no visible row of ${target.key}, whose ` +
        "words hold them"
    )
    .toBe(true);
});
