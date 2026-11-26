/** the rows time the new issue page's loads in Chromium, its processor slowed, on an issue the
 *  server seeds at the largest issue's shape: how soon each view's first screen is painted, how
 *  soon each act shows its result during the load and after it, and whether a box the page drew
 *  moves, which the server's own answers to the issue, tree and view reads, their timing, the
 *  live socket's events and the edit notices of the seed's documents decide */

import { setTimeout as sleep } from "node:timers/promises";
import type { BrowserContext } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import type { Preset } from "../web/src/features/issue-page/page-words";
import {
  ACT_MS,
  AFTER_LOAD_TITLE,
  actAddress,
  actRowTitle,
  afterLoadOrder,
  boundWord,
  FIRST_SCREEN_MS,
  fiveLoads,
  holdAct,
  holdBound,
  holdTasks,
  LAYOUT_SHIFT,
  LOAD_ACTS,
  LOAD_VIEWS,
  LOADS,
  loadCpuRate,
  loadWidth,
  makeSteps,
  namedRow,
  type OpenedLoad,
  oneLoad,
  outlineRowSelector,
  outlineRows,
  printReading,
  test,
} from "./issue-page-load";
import { WAITING_QUESTION } from "./issue-page-t-scale";
import { owedUnless } from "./issue-page-t-tree";
import { typefaceAddress } from "./issue-page-typeface";
import {
  actReading,
  boxesMoved,
  firstScreenOf,
  firstScreenPaint,
  groupsUndrawn,
  type LoadCapture,
  type LoadRecord,
  largestContentfulPaint,
  layoutShift,
  longestTaskAfterViewRow,
  moveLine,
  namedRowAt,
  pageShape,
  type ResourceEntry,
  recordOf,
  underHeader,
  waitForLoadEnd,
} from "./load-instruments";
import { refuseASharedRun } from "./main-thread-hold";
import { expect } from "./test";

/**
 * The load rows of `c-load-1440` and `c-load-390`, in the projects `chromium-load` (1440 by 900,
 * the processor slowed by 2) and `iphone-load` (390 px, slowed by 4), one worker and no retry.
 * Each bound is read over five loads of the seed (`seedLargestShape`), each in a new browser
 * context so its cache is empty, and holds when the fourth smallest of its five readings meets
 * it; each row prints one reading line for each bound it reads (`holdBound`). A row reads the
 * clock, so it refuses a run of more than one worker before it reads one (`refuseASharedRun`).
 *
 * - Row 1 (R1, R2, R3): a load of each view's own address with no input paints its first screen,
 *   the rows in the viewport under the header as the load ends, within 2.5 s by Element Timing,
 *   and its largest contentful paint by then; no task after the first drawing of the view row
 *   runs over 200 ms; the layout shift is at most 0.1; and no control of the view row or the
 *   header, nor the header's band, nor a row in the viewport changes its box after its first
 *   drawing, but in a frame of the typeface's swap. Dashboard, Threads and Documents are owed
 *   until their parts add them to the view row's list.
 * - Row 2 (R3): the row an address names, and the ask the root's first focus names, stand in the
 *   viewport at the first drawing of the rows and keep their place until the load has ended. The
 *   row an address names is the last row Conversation draws as the load ends - a child issue's
 *   row on the seed, named at `?node=` of its key - or, where no address names that row, the last
 *   row before it that one does; its bound is named `named-row.last-row` or
 *   `named-row.last-addressed-row` for the two, and a line before its bounds names the row and its
 *   address.
 * - Row 3 (R3): with the typeface's file answered 3 s late, the layout shift is at most 0.1.
 * - Row 4 (R2): each act of the width, made at the first moment it can be made, shows its result
 *   within 200 ms, as `holdAct` reads an act that needs a read; no Event Timing entry and no task
 *   after the first drawing of the view row runs over 200 ms. At 390 px the first screen is the
 *   children's section and holds no row of the outline with children, so the row a fold and an
 *   unfold act on - the first in the document that holds children - is scrolled into view first,
 *   and the act is made at the first moment after that; at 1440 px it is the first such row in
 *   the viewport, as the first drawing of the rows draws it.
 * - Row 5 (R2): each act, made once in each load after it has ended, shows its result within
 *   200 ms, read as Row 4 reads it. A control an earlier act scrolled out of reach is scrolled
 *   back into view first (`revealStep`).
 *
 * The rows of one worker share its seed (`test` of `e2e/issue-page-load.ts`), so they run in the
 * order they are written, and Row 2, whose first focus seeds an ask that waits on the reader in
 * a phase of its own, comes last: a row after it would seed the shape again first.
 */

// The rows share the worker's seed, and are read in the order written: `default` runs them in
// one worker in that order, where `serial` would skip every row after one that failed and print
// none of its readings.
test.describe.configure({ mode: "default" });

/** How long a row of five loads may take. */
const LOAD_ROW_MS = 300_000;

/** How long the row of every act after the load may take: five loads, each with every act of
 *  its width made once and waited out. */
const AFTER_LOAD_ROW_MS = 900_000;

/** How long the row of the named rows may take: eleven loads and the seed of its first focus. */
const NAMED_ROW_MS = 900_000;

/** How long the row of the held typeface holds the typeface's file back. */
const TYPEFACE_HOLD_MS = 3_000;

/** A load once it has ended: when it did, and what the init script collected. */
async function ended({
  page,
  requests,
}: OpenedLoad): Promise<{ readonly end: number; readonly record: LoadRecord }> {
  const end = await waitForLoadEnd(page, requests);
  return { end, record: await recordOf(page) };
}

/** Holds the typeface's file back on a load's context: `address` is answered 3 s late. */
function holdTypeface(address: string): (context: BrowserContext) => Promise<void> {
  return async (context) => {
    // A predicate, not the address as a pattern: Playwright reads a string as a glob.
    await context.route(
      (url) => url.href === address,
      async (route) => {
        // The hold is what the row reads: the page's text stands in the fallback face for these
        // 3 s, and the swap that ends them is the shift the row bounds.
        await sleep(TYPEFACE_HOLD_MS);
        await route.continue();
      }
    );
  };
}

for (const view of LOAD_VIEWS) {
  // SLOW(>=2s): five cold loads of the seeded issue at the slowed rate, each waited out to its end.
  test(`a load of ${view.name} draws its first screen within 2.5 s`, async ({
    browser,
    loadSeeds,
  }, testInfo) => {
    refuseASharedRun();
    owedUnless(view.gate);
    test.setTimeout(LOAD_ROW_MS);
    const rate = loadCpuRate(testInfo.project.name);
    const seeded = await loadSeeds.shape();
    const loads = await fiveLoads(
      browser,
      rate,
      { address: presetPath(seeded.key, view.view as Preset), boxes: "all" },
      async (load) => ({
        ...(await ended(load)),
        screen: await firstScreenOf(load.page),
        shape: view.view === "conversation" ? await pageShape(load.page) : null,
      })
    );
    const prefix = `first-screen.${view.view}`;
    const paints = loads.map((load) => firstScreenPaint(load.screen));
    holdBound({
      bound: `${prefix}.paint-ms`,
      limit: FIRST_SCREEN_MS,
      rate,
      readings: paints.map((paint) => paint.reading),
    });
    holdBound({
      bound: `${prefix}.largest-contentful-paint-ms`,
      limit: FIRST_SCREEN_MS,
      rate,
      readings: loads.map((load) => largestContentfulPaint(load.record.lcp, load.end)),
    });
    holdBound({
      bound: `${prefix}.longest-task-ms`,
      limit: ACT_MS,
      rate,
      readings: loads.map((load) => longestTaskAfterViewRow(load.record)),
    });
    holdBound({
      bound: `${prefix}.layout-shift`,
      limit: LAYOUT_SHIFT,
      rate,
      readings: loads.map((load) => layoutShift(load.record.cls)),
    });
    for (const [at, load] of loads.entries()) {
      const which = `load ${at + 1} of ${LOADS}`;
      if (load.shape !== null) {
        printReading(
          `page-shape.${view.view} rate=${rate} load=${at + 1} rows=${load.shape.rows} ` +
            `elements=${load.shape.elements} bytes=${load.shape.bytes}`
        );
      }
      expect
        .soft(
          paints[at]?.unpainted,
          `${which}: rows of the first screen whose first line has no Element Timing entry`
        )
        .toEqual([]);
      expect
        .soft(
          groupsUndrawn(load.record.boxes),
          `${which}: groups of boxes R3 holds that matched no element at this width`
        )
        .toEqual([]);
      expect
        .soft(
          boxesMoved(load.record, load.end).map(moveLine),
          `${which}: boxes that changed after their first drawing, outside the typeface's swap, ` +
            "before the load ended"
        )
        .toEqual([]);
    }
  });
}

// SLOW(>=2s): one load to read the typeface's address, then five cold loads with it held back 3 s.
test("a load with the typeface held back for 3 s moves nothing", async ({
  browser,
  loadSeeds,
}, testInfo) => {
  refuseASharedRun();
  test.setTimeout(LOAD_ROW_MS);
  const rate = loadCpuRate(testInfo.project.name);
  const seeded = await loadSeeds.shape();
  const address = presetPath(seeded.key, "conversation");
  const font = await oneLoad(browser, rate, { address, boxes: "none" }, async (load) => {
    await waitForLoadEnd(load.page, load.requests);
    return await typefaceAddress(load.page);
  });
  const loads = await fiveLoads(
    browser,
    rate,
    { address, boxes: "none", prepare: holdTypeface(font) },
    ended
  );
  holdBound({
    bound: "typeface-held.layout-shift",
    limit: LAYOUT_SHIFT,
    rate,
    readings: loads.map((load) => layoutShift(load.record.cls)),
  });
  for (const [at, load] of loads.entries()) {
    // A load whose typeface came on time read no held typeface, and would pass for nothing.
    const held = load.record.resource.find((entry) => entry.name === font);
    expect
      .soft(
        held === undefined ? null : held.responseEnd - held.startTime,
        `load ${at + 1} of ${LOADS}: the typeface's file ${font} was not answered ` +
          `${TYPEFACE_HOLD_MS} ms late`
      )
      .toBeGreaterThanOrEqual(TYPEFACE_HOLD_MS);
  }
});

/** Every act of either width, once: each project runs the rows of its own width's acts, and its
 *  `grepInvert` (`e2e/playwright.config.ts`) leaves out the row of the other width's act. */
const ACT_NAMES = [...new Set([...LOAD_ACTS[1440], ...LOAD_ACTS[390]].map((act) => act.name))];

for (const name of ACT_NAMES) {
  // SLOW(>=2s): five cold loads at the slowed rate, the act made at its first moment in each.
  test(actRowTitle(name), async ({ browser, loadSeeds }, testInfo) => {
    refuseASharedRun();
    test.setTimeout(LOAD_ROW_MS);
    const width = loadWidth(testInfo);
    const act = LOAD_ACTS[width].find((each) => each.name === name);
    if (act === undefined) {
      throw new Error(
        `${name} is no act at ${width} px, so ${testInfo.project.name} runs no row of it: its ` +
          "grepInvert in e2e/playwright.config.ts leaves this row out"
      );
    }
    const rate = loadCpuRate(testInfo.project.name);
    const seeded = await loadSeeds.shape();
    const loads = await fiveLoads(
      browser,
      rate,
      { address: actAddress(seeded.key, act), boxes: "none" },
      async ({ page, requests }) => {
        const made = await makeSteps(page, 0, act.steps, act.when);
        await waitForLoadEnd(page, requests);
        return { last: made.last, record: await recordOf(page) };
      }
    );
    const prefix = `during-load.${boundWord(name)}`;
    holdAct(
      prefix,
      rate,
      loads.map((load) => actReading(load.record, load.last))
    );
    holdTasks(
      prefix,
      rate,
      loads.map((load) => load.record)
    );
  });
}

// SLOW(>=2s): five cold loads, each waited out and then every act of its width made once in it.
test(AFTER_LOAD_TITLE, async ({ browser, loadSeeds }, testInfo) => {
  refuseASharedRun();
  test.setTimeout(AFTER_LOAD_ROW_MS);
  const acts = afterLoadOrder(LOAD_ACTS[loadWidth(testInfo)]);
  const rate = loadCpuRate(testInfo.project.name);
  const seeded = await loadSeeds.shape();
  const loads = await fiveLoads(
    browser,
    rate,
    { address: presetPath(seeded.key, "conversation"), boxes: "none" },
    async ({ page, requests }) => {
      await waitForLoadEnd(page, requests);
      // Each act is made once the page has gone quiet again after the one before it.
      const settle = () => waitForLoadEnd(page, requests);
      const measured: number[] = [];
      let next = 0;
      for (const act of acts) {
        const made = await makeSteps(page, next, act.steps, "now", settle);
        measured.push(made.last);
        next = (await makeSteps(page, made.next, act.after, "now")).next;
      }
      await waitForLoadEnd(page, requests);
      return { measured, record: await recordOf(page) };
    }
  );
  for (const [at, act] of acts.entries()) {
    holdAct(
      `after-load.${boundWord(act.name)}`,
      rate,
      loads.map((load) => actReading(load.record, load.measured[at] ?? -1))
    );
  }
  holdTasks(
    "after-load",
    rate,
    loads.map((load) => load.record)
  );
});

/** Prints one load's raw readings as one line of the run's log, `LOAD-CAPTURE <name> <json>`,
 *  which is cut into `e2e/__tests__/fixtures/load-readings/<name>.json`. */
function printCapture(name: string, capture: LoadCapture): void {
  console.log(`LOAD-CAPTURE ${name} ${JSON.stringify(capture)}`);
}

/** One load's raw readings once it has ended. */
async function captured(rate: number, { page, requests }: OpenedLoad): Promise<LoadCapture> {
  const end = await waitForLoadEnd(page, requests);
  const record = await recordOf(page);
  return {
    end,
    firstScreen: await firstScreenOf(page),
    rate,
    record,
    requests: requests.spans(record.timeOrigin),
  };
}

// SLOW(>=2s): up to four cold loads at the slowed rate, one with its typeface held back 3 s.
test("capture the raw readings of one load", async ({ browser, loadSeeds }, testInfo) => {
  test.skip(
    process.env.ISSUE_PAGE_LOAD_CAPTURE !== "1",
    "the capture runs only where ISSUE_PAGE_LOAD_CAPTURE=1, for the fixtures of load-readings.test.ts"
  );
  refuseASharedRun();
  test.setTimeout(LOAD_ROW_MS);
  const width = loadWidth(testInfo);
  const rate = loadCpuRate(testInfo.project.name);
  const seeded = await loadSeeds.shape();
  const address = presetPath(seeded.key, "conversation");
  printCapture(
    `conversation-${width}`,
    await oneLoad(browser, rate, { address, boxes: "all" }, (load) => captured(rate, load))
  );
  // The other two fixtures are one load each, at 1440 px.
  if (width !== 1440) return;
  const font = await oneLoad(browser, rate, { address, boxes: "none" }, async (load) => {
    await waitForLoadEnd(load.page, load.requests);
    return await typefaceAddress(load.page);
  });
  printCapture(
    "typeface-held-back",
    await oneLoad(browser, rate, { address, boxes: "all", prepare: holdTypeface(font) }, (load) =>
      captured(rate, load)
    )
  );
  // A press on a view whose read the page does not hold once the load has ended, so the press
  // sends that read and the next capture can hold a result that waits for it.
  printCapture(
    "view-press-read",
    await oneLoad(browser, rate, { address, boxes: "none" }, async (load) => {
      await waitForLoadEnd(load.page, load.requests);
      const sent = (await recordOf(load.page)).resource;
      const view = UNHELD_ORDER.find((each) => !viewReadSent(sent, seeded.key, each));
      const press = LOAD_ACTS[1440].find((act) => act.kind === "view" && act.view === view);
      if (view === undefined || press === undefined) {
        throw new Error(
          `the load of Conversation sent the read of every view the capture may press: ` +
            UNHELD_ORDER.join(", ")
        );
      }
      await makeSteps(load.page, 0, press.steps, "now");
      return await captured(rate, load);
    })
  );
});

/** The views the capture's press may choose, in the order it tries them, the first whose read the
 *  load did not send: Decisions, Plan and Verification, whose reads a load of Conversation sends
 *  where its issue's shape asks for them (`tree-read/view-read.tsx`); Graph, whose read is the
 *  whole tree; and Spec last, which at the base draws its first screen from Conversation's read
 *  before its own answers (`view-press-read.json` of the first capture). */
const UNHELD_ORDER = ["decisions", "plan", "verification", "graph", "spec"] as const;

/** Whether `resource` holds a read of `view`'s rows of issue `key`: the tree read with that
 *  `view`, Graph's tree read with no `view` and no `kind`, and for Decisions the decisions read
 *  too, which is one of the reads a load sends. */
function viewReadSent(
  resource: readonly ResourceEntry[],
  key: string,
  view: (typeof UNHELD_ORDER)[number]
): boolean {
  const issue = `/api/v1/issues/${encodeURIComponent(key)}`;
  return resource.some((entry) => {
    const url = new URL(entry.name);
    if (view === "decisions" && url.pathname === `${issue}/decisions`) return true;
    if (url.pathname !== `${issue}/tree`) return false;
    return view === "graph"
      ? !url.searchParams.has("view") && !url.searchParams.has("kind")
      : url.searchParams.get("view") === view;
  });
}

/** Holds the five loads of a named row to R3 under `prefix`: at the first drawing of the rows
 *  the named row stands in the viewport under the header; it keeps its box until the load has
 *  ended, but in a frame of the typeface's swap; and the layout shift is at most 0.1. */
function holdNamedRow(
  prefix: string,
  rate: number,
  loads: readonly { readonly end: number; readonly record: LoadRecord }[]
): void {
  holdBound({
    bound: `${prefix}.layout-shift`,
    limit: LAYOUT_SHIFT,
    rate,
    readings: loads.map((load) => layoutShift(load.record.cls)),
  });
  for (const [at, load] of loads.entries()) {
    const which = `load ${at + 1} of ${LOADS}`;
    const rows = load.record.frames.findIndex((frame) => frame.rows);
    const frame = load.record.frames[rows];
    const box = rows === -1 ? null : namedRowAt(load.record, rows);
    expect
      .soft(
        box !== null && frame !== undefined && underHeader(box, frame),
        `${which}: at the first drawing of the rows the named row was ${
          box === null
            ? "not drawn"
            : `at ${box.y} px, outside the viewport under the header's foot at ${frame?.foot} px`
        }`
      )
      .toBe(true);
    expect
      .soft(
        boxesMoved(load.record, load.end).map(moveLine),
        `${which}: the named row changed its place before the load ended`
      )
      .toEqual([]);
  }
}

// SLOW(>=2s): a load to find the last row, five cold loads of its address, the seed of an ask that
// waits on the reader with its waits, and five cold loads of it.
test("a load draws the named row in the viewport at once and keeps it there", async ({
  browser,
  loadSeeds,
}, testInfo) => {
  refuseASharedRun();
  test.setTimeout(NAMED_ROW_MS);
  const rate = loadCpuRate(testInfo.project.name);
  const seeded = await loadSeeds.shape();
  // The last row the page draws once the load has ended, before any control of the paging rule
  // is pressed, named by its own address; where no address names that row, the last row before it
  // that one does, and the bound's name says which (`namedRow`).
  const named = await oneLoad(
    browser,
    rate,
    { address: presetPath(seeded.key, "conversation"), boxes: "none" },
    async (load) => {
      await waitForLoadEnd(load.page, load.requests);
      return namedRow(seeded.key, await outlineRows(load.page));
    }
  );
  const prefix = named.last ? "named-row.last-row" : "named-row.last-addressed-row";
  printReading(
    `${prefix} rate=${rate} row=${named.row.type}:${named.row.id} address=${named.address}`
  );
  holdNamedRow(
    prefix,
    rate,
    await fiveLoads(
      browser,
      rate,
      {
        address: named.address,
        boxes: "named",
        named: { selector: outlineRowSelector(named.row.id), text: null },
      },
      ended
    )
  );
  // The root's rule for the first focus names the ask that waits on the reader.
  const waiting = await loadSeeds.waiting();
  holdNamedRow(
    "named-row.first-focus",
    rate,
    await fiveLoads(
      browser,
      rate,
      {
        address: presetPath(waiting.key, "conversation"),
        boxes: "named",
        named: {
          selector: '[data-issue-body] [data-node-row][data-node-type="ask"]',
          text: WAITING_QUESTION,
        },
      },
      ended
    )
  );
});
