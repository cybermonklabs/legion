/** the rows write `pull_request_heads` rows into the database and read the issue the server
 *  builds from them, and time the header on a copy the server's block index has taken in */

/*
 * Three rows of part N (dispatch://INFRA-3940), moved for piece 2 (dispatch://INFRA-6229):
 * the header fits its labels within 50 ms of a load, the header links the issue's pull request,
 * and the title block draws its lower line from the first pixel scrolled. All three read the
 * new issue page (`withIssuePage`).
 */

import { assigneeControlName } from "../web/src/features/issue/AssigneeControl";
import { PULL_REQUEST_STATE_WORDS } from "../web/src/features/issue-page/page-words";
import { getIssue, getMyState, patchIssue, requestApproval } from "./api";
import { filterRow, gotoIssue, phase } from "./issue-page";
import { DESKTOP, expectOpeningLanded, focusThePage } from "./issue-page-g";
import { withIssuePage } from "./issue-page-option";
import {
  expectHeaderDrawn,
  TWENTY_LABELS as HEADER_LABELS,
  IPHONE_13,
  issueHeader,
  labelsAgainstTheCount,
  lineOrder,
  MEASURED_TITLE,
  propertiesLine,
  seedHeader,
  stickyReading,
  viewportAt,
} from "./issue-page-p";
import { sql } from "./psql";
import { PULL_REQUEST_REPOSITORY, writePullRequestHead } from "./pull-request-heads";
import { seedLargestScaleCopy } from "./scale-seed";
import { settledValue } from "./settled";
import { type Browser, type BrowserContext, test as dispatchTest, expect, type Page } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertWhole } from "./unclipped";
import { asUser } from "./users";

/** e2e/test.ts's `test`, whose own context - the `page` a row takes - has the new page turned on
 *  for the login it signs in (`withIssuePage`): each row of this file reads the new page. */
const test = dispatchTest.extend({
  context: async ({ context, signedIn }, use) => {
    if (signedIn !== false) await withIssuePage(context, signedIn);
    await use(context);
  },
});

/** A context signed in as `login` with the new page turned on, for a row that opens its own. */
async function asReader(browser: Browser, login: string): Promise<BrowserContext> {
  const context = await asUser(browser, login);
  await withIssuePage(context, login);
  return context;
}

const SELECTORS = {
  /** The whole filter row: the five controls or the one control, Outline, and the small
   *  controls at its right end (`IssuePresets.tsx`). */
  filterRow: "[data-filter-row]",
  /** The part of the identity block that stays at the top of the screen from 761 px up: the
   *  key's row and the title (`IssueHeader.tsx`). */
  identityBand: "[data-identity-block]",
  identityBlock: "header[data-issue-header]",
  issuePage: "[data-issue-page]",
} as const;

/** The header's size as root R13 states it: the height from the top of the identity block to the
 *  top of the filter row, and the lines of text and control values drawn between the two, each as
 *  the words it holds, left to right. */
interface HeaderSize {
  readonly height: number;
  readonly lines: readonly string[];
}

/** Root R13's limit on the header at `width` (R22 repeats it for the pull request's link): at
 *  1440 px 3 lines and 130 px, at 390 px 6 lines and 180 px, none at any other width. */
function headerLimit(
  width: number
): { readonly height: number; readonly lines: number } | undefined {
  if (width === 1440) return { height: 130, lines: 3 };
  if (width === 390) return { height: 180, lines: 6 };
  return undefined;
}

/**
 * The header as it is drawn now, with the page at its top: the lines are those of the text nodes
 * and the controls' values that stand between the top of the identity block and the top of the
 * filter row, each text that wraps counted line by line, and a line is every piece whose top
 * lies above the lowest bottom of the pieces gathered into it. Text laid out unseen (hidden,
 * transparent, or a clipped box a pixel wide or high) is left out, as are the options of a
 * select, which stand in the list and are drawn as its value. Null where the page draws no
 * header or no filter row.
 */
function headerSize(page: Page): Promise<HeaderSize | null> {
  return page.evaluate((selectors) => {
    window.scrollTo(0, 0);
    const header = document.querySelector(`${selectors.issuePage} ${selectors.identityBlock}`);
    const band = header?.querySelector(selectors.identityBand) ?? null;
    const row = document.querySelector(`${selectors.issuePage} ${selectors.filterRow}`);
    if (header === null || band === null || row === null) return null;
    const top = band.getBoundingClientRect().top;
    const bottom = row.getBoundingClientRect().top;
    const inBand = (box: DOMRect): boolean =>
      box.width > 0 && box.height > 0 && box.bottom > top && box.top < bottom;
    const drawn = (element: Element): boolean => {
      for (let at: Element | null = element; at !== null; at = at.parentElement) {
        const style = getComputedStyle(at);
        if (style.visibility !== "visible" || style.opacity === "0") return false;
        const box = at.getBoundingClientRect();
        if (style.overflow !== "visible" && (box.width <= 1 || box.height <= 1)) return false;
      }
      return true;
    };
    const pieces: { bottom: number; left: number; text: string; top: number }[] = [];
    const range = document.createRange();
    const walker = document.createTreeWalker(header, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const content = node.textContent ?? "";
      const element = node.parentElement;
      if (content.trim() === "" || element === null || element.closest("select") !== null) continue;
      range.selectNodeContents(node);
      const boxes = [...range.getClientRects()].filter(inBand);
      if (boxes.length === 0 || !drawn(element)) continue;
      // A text that wraps is one box per line: each line is given the characters drawn in it.
      const words = boxes.length === 1 ? [content] : boxes.map(() => "");
      for (let at = 0; boxes.length > 1 && at < content.length; at += 1) {
        range.setStart(node, at);
        range.setEnd(node, at + 1);
        const glyph = range.getClientRects()[0];
        if (glyph === undefined) continue;
        const line = boxes.findIndex(
          (box) => glyph.top >= box.top - 1 && glyph.bottom <= box.bottom + 1
        );
        if (line >= 0) words[line] += content[at];
      }
      boxes.forEach((box, line) => {
        const text = (words[line] ?? "").replace(/\s+/g, " ").trim();
        pieces.push({ bottom: box.bottom, left: box.left, text, top: box.top });
      });
    }
    for (const control of header.querySelectorAll("select, input:not([type='hidden']), textarea")) {
      const box = control.getBoundingClientRect();
      if (!inBand(box) || !drawn(control)) continue;
      const size = Number.parseFloat(getComputedStyle(control).fontSize);
      const middle = (box.top + box.bottom) / 2;
      const text =
        control instanceof HTMLSelectElement
          ? (control.selectedOptions[0]?.textContent ?? "").trim()
          : (control as HTMLInputElement).value;
      pieces.push({ bottom: middle + size / 2, left: box.left, text, top: middle - size / 2 });
    }
    pieces.sort((a, b) => a.top - b.top);
    const lines: (typeof pieces)[] = [];
    let lineBottom = Number.NEGATIVE_INFINITY;
    for (const piece of pieces) {
      const line = lines.at(-1);
      if (line !== undefined && piece.top < lineBottom - 1) {
        line.push(piece);
        lineBottom = Math.max(lineBottom, piece.bottom);
      } else {
        lines.push([piece]);
        lineBottom = piece.bottom;
      }
    }
    return {
      height: bottom - top,
      lines: lines.map((line) =>
        line
          .sort((a, b) => a.left - b.left)
          .map((piece) => piece.text)
          .join(" ")
      ),
    };
  }, SELECTORS);
}

/** The measure each reading of the header's labels is in the shipped code (`HeaderLabels`'s
 *  `measured`, `web/src/features/issue-page/IssuePropertiesRow.tsx`). */
const HEADER_LABELS_MEASURE = "issue-header-labels";

// c-header-labels (R21, decision 500 (3), the plan's g24): on production's build the header's
// reading of which label chips its line holds (`HeaderLabels`, `IssuePropertiesRow.tsx`) took
// about 1.1 s of a load of INFRA-1196 with 20 labels, several forced layouts a reading on a page
// that large. Each reading is one `issue-header-labels` measure, and the row sums them over one
// cold load of Conversation's address, `/issues/<key>/conversation`, the outline, where
// production's reading of about 1.1 s was measured; the bare address opens Dashboard, a smaller
// page, on which the merge-base's reading took 17.3 ms (builds Job e2e-rows-p6qk7) against
// 125.6 ms at Conversation (e2e-rows-5ldfv). The load is of the copy of INFRA-3149, the largest
// copy `e2e/scale-seed.ts` writes, written alone (`seedLargestScaleCopy`) with 20 labels, signed
// in as its reader, who never read it: at 1440 px under chromium and at 390 px under iphone, each
// project seeding a copy of its own. The sum is read at network idle (`expectLoadSettled`) and one
// second more, and printed; then the line, at rest, holds no chip past its first line or its end,
// and its count names every chip it leaves out.
// The row slows Chromium's CPU four times (`Emulation.setCPUThrottlingRate`) from before the
// cold load until it has read the sum. It uses the same rate as Lighthouse's mobile run, as
// `e2e/issue-page-i.e2e.ts` does for a busy page. Slowing only adds time, so a sum of at most
// 50 ms still proves R21 at full speed. It also widens the gap between this reading and the
// merge-base's. At full speed on `main`'s page, the merge-base's reading takes 32 to 74 ms,
// so it does not fail the row every time.
// SLOW(>=2s): the copy of INFRA-3149 written - 69 documents of 45,738 blocks, 39 files and 113
// comments - and the block index's drain waited for, then one cold load with the CPU slowed
// four times.
test("the header fits its labels within 50 ms of a load @phone", async ({ browser }) => {
  // Measured in builds Job e2e-rows-5tkp5 at dd4508e8, with the cold load at Conversation's
  // address: 1.1 min on chromium and 37.1 s on iphone, each the copy written and the block index
  // drained, then one cold load; the bound is about three times the longer, for a slow leg.
  test.setTimeout(200_000);
  const viewport = test.info().project.name === "iphone" ? IPHONE_13 : DESKTOP;
  const { copy, reader } = await seedLargestScaleCopy();
  await patchIssue(copy.key, { labels: [...HEADER_LABELS] });
  const where = `the cold load of ${copy.key}, the copy of ${copy.census} with ${HEADER_LABELS.length} labels, at ${viewport.width} px`;
  expect(
    (await getMyState(reader))[copy.key]?.last_read_seq ?? 0,
    `${reader} has read ${copy.key} before ${where}, so it is no load of a person who never read it`
  ).toBe(0);
  const context = await asReader(browser, reader);
  try {
    const page = await context.newPage();
    await page.setViewportSize(viewport);
    const cpu = await context.newCDPSession(page);
    await cpu.send("Emulation.setCPUThrottlingRate", { rate: 4 });
    let readings: number[];
    try {
      const traffic = watchApiRequests(page);
      await gotoIssue(page, `/issues/${copy.key}/conversation`);
      await expectHeaderDrawn(page);
      await expectLoadSettled(traffic);
      // The second past network idle that c-header-labels names: a reading the load's last answers
      // set off, by a width they changed or a typeface that finished loading, is taken and
      // measured in the frames of that second.
      await page.waitForTimeout(1_000);
      readings = await page.evaluate(
        (name) => performance.getEntriesByName(name, "measure").map((entry) => entry.duration),
        HEADER_LABELS_MEASURE
      );
    } finally {
      await cpu.send("Emulation.setCPUThrottlingRate", { rate: 1 });
      await cpu.detach();
    }
    const sum = readings.reduce((total, duration) => total + duration, 0);
    const each = readings.map((duration) => duration.toFixed(1)).join(", ");
    const said = `SCALE header labels ${copy.census} ${viewport.width} px, CPU slowed 4×: ${readings.length} readings, ${sum.toFixed(1)} ms (${each})`;
    console.log(said);
    test.info().annotations.push({ description: said, type: "header-labels" });
    expect(
      readings.length,
      `${where} took no ${HEADER_LABELS_MEASURE} measure, so the header read none of its labels`
    ).toBeGreaterThan(0);
    expect(
      sum,
      `the header's label reading took ${sum.toFixed(1)} ms of ${where}, more than 50 ms, in ${readings.length} readings: ${each} ms`
    ).toBeLessThanOrEqual(50);

    // The line once it is at rest: the chips it holds, those of them drawn whole and those cut,
    // and the number its count names.
    const drawn = await settledValue(`the header's labels after ${where}`, async () => ({
      ...(await labelsAgainstTheCount(page)),
      held: await propertiesLine(page).evaluate(
        (properties) =>
          properties.querySelectorAll('[data-testid="issue-header-labels"] > [data-header-label]')
            .length
      ),
    }));
    expect(drawn.cut, `${where}: the labels' box cuts a chip the line holds`).toEqual([]);
    expect(
      drawn.held,
      `${where}: the line holds ${drawn.held} chips and draws ${drawn.whole} of them whole on its first line`
    ).toBe(drawn.whole);
    expect(
      drawn.whole + drawn.left,
      `${where}: ${drawn.whole} chips drawn whole and a count of ${drawn.left}, of ${HEADER_LABELS.length} labels`
    ).toBe(HEADER_LABELS.length);
  } finally {
    await context.close();
  }
});

/** A style rule that narrows the header's labels' box by 170 px from the moment a reading's fit is
 *  drawn, which is when the box loses `invisible` (`HeaderLabels`, `IssuePropertiesRow.tsx`): the
 *  narrowing lands in the reading's own frame, through the browser's own `ResizeObserver`, as a
 *  classic scrollbar that comes in that frame would narrow it. */
const NARROWED_ONCE_DRAWN =
  '[data-testid="issue-state-actions"] [data-testid="issue-header-labels"]:not(.invisible) { max-width: calc(100% - 170px) !important; }';

// R21: the line draws no chip it hides, and its count names every chip left out, also where the
// box narrows in the frame of the reading that drew its fit. At 1000 px on part P's header seed
// with 20 labels, the reading takes the box while it is 54 px wide and draws it on to the count's
// line with 11 chips; there it narrows, by the rule above, so that the 11th chip wraps onto the
// line the box clips away. Read once the line holds still: every chip it holds is drawn whole,
// and the count names the rest.
// SLOW(>=2s): the header's seed with 20 labels written, then one load of the issue.
test("a box the fit moves on to the count's line draws no chip it hides when it narrows in the reading's frame", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...HEADER_LABELS] });
    // An init script runs before the document holds its root element, so the rule goes in once
    // the document is parsed: before the issue's read answers, so before any reading of the labels.
    await page.context().addInitScript((css) => {
      document.addEventListener("DOMContentLoaded", () => {
        const style = document.createElement("style");
        style.textContent = css;
        document.documentElement.append(style);
      });
    }, NARROWED_ONCE_DRAWN);
    await page.setViewportSize(viewportAt(1000));
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectHeaderDrawn(page);
    const where = "at 1000 px, the box narrowed in the frame of the reading that drew its fit";
    const drawn = await settledValue(`the header's labels ${where}`, async () => ({
      ...(await labelsAgainstTheCount(page)),
      held: await propertiesLine(page).evaluate(
        (properties) =>
          properties.querySelectorAll('[data-testid="issue-header-labels"] > [data-header-label]')
            .length
      ),
      maxWidth: await propertiesLine(page).evaluate((properties) => {
        const box = properties.querySelector('[data-testid="issue-header-labels"]');
        return box === null ? "no box" : getComputedStyle(box).maxWidth;
      }),
    }));
    // The rule holds the drawn box, so the row reads the narrowing it is about and not a line that
    // never narrowed.
    expect(
      drawn.maxWidth,
      `${where}: the narrowing rule does not hold the labels' box`
    ).not.toMatch(/^(none|no box)$/);
    expect(drawn.cut, `${where}: the labels' box cuts a chip the line holds`).toEqual([]);
    expect(
      drawn.held,
      `${where}: the line holds ${drawn.held} chips and draws ${drawn.whole} of them whole on its first line`
    ).toBe(drawn.whole);
    expect(
      drawn.whole + drawn.left,
      `${where}: ${drawn.whole} chips drawn whole and a count of ${drawn.left}, of ${HEADER_LABELS.length} labels`
    ).toBe(HEADER_LABELS.length);
  });
});

/** The header as it is drawn at `width`, read with the page at its top, and what it took, in the
 *  run's annotations and its output. `where` names the seed. At 390 px the title is drawn whole,
 *  so the reading counts all of it. */
async function readHeader(page: Page, width: number, where: string): Promise<HeaderSize> {
  await expect(filterRow(page), `the page draws no view row ${where}`).toBeVisible();
  // The view row waited for above is drawn after the header and under it, so both are in the page
  // by now and a null reading only throws.
  const reading = await headerSize(page);
  if (reading === null) throw new Error(`the page draws no header or no view row ${where}`);
  const description = `${where} at ${width} px: ${reading.lines.length} lines, ${reading.height.toFixed(1)} px`;
  console.log(`header ${description}: ${JSON.stringify(reading.lines)}`);
  test.info().annotations.push({ description, type: "header" });
  if (width === IPHONE_13.width) {
    await assertWhole(issueHeader(page).getByRole("heading", { level: 1 }));
  }
  return reading;
}

/** The header's limit at 390 px on the seeds of the 115-character title with 20 labels, with an
 *  approval request waiting on the reader or with none: part P's limit while a request waits, 9
 *  lines and 280 px (decision 17 of dispatch://INFRA-1196/artifact/part-decisions-md, `headerLimit`
 *  of `e2e/issue-page-p.ts`, which gives 8 lines and 250 px while none waits), applied to both
 *  seeds. Root R13's 6 lines and 180 px hold for a title of at most two lines:
 *  the owner's answer to the root's ask, "Long titles excepted"
 *  (dispatch://INFRA-1196/ask/7c644731-f514-498b-b57a-4e76136ce1a3), which the architect of
 *  INFRA-3940 applied to N's measurement (dispatch://INFRA-3940/comment/a5dd812e-6287-4be1-823a-220f3c1fa097).
 *  This title takes four lines, and with no pull request link the header takes 9 lines and
 *  265.0 px there with a request waiting, 8 lines and 235.0 px with none. A ruling that sets
 *  another limit changes this one line. */
const WORST_HEADER_AT_390 = { height: 280, lines: 9 } as const;

/** The limit on the header at `width` for a seed of the row: root R13's, but on a seed with the
 *  115-character title at 390 px `WORST_HEADER_AT_390`. */
function headerLimitOf(width: number, longTitle: boolean): { height: number; lines: number } {
  const limit = longTitle && width === IPHONE_13.width ? WORST_HEADER_AT_390 : headerLimit(width);
  if (limit === undefined) throw new Error(`no limit on the header at ${width} px`);
  return limit;
}

/** `reading` against the limit on the header at `width`, soft so that one run reports every
 *  width and seed, with the lines it counted in the message. `longTitle` is a seed with the
 *  115-character title. */
function expectHeaderWithinItsLimit(
  reading: HeaderSize,
  width: number,
  where: string,
  longTitle: boolean
): void {
  const limit = headerLimitOf(width, longTitle);
  const drawnAs = `${reading.lines.length} lines, ${reading.height.toFixed(1)} px`;
  expect
    .soft(
      reading.lines.length,
      `the header takes ${drawnAs} ${where} at ${width} px: ${JSON.stringify(reading.lines)}`
    )
    .toBeLessThanOrEqual(limit.lines);
  expect
    .soft(reading.height, `the header takes ${drawnAs} ${where} at ${width} px`)
    .toBeLessThanOrEqual(limit.height);
}

/** The header with the link against the same header without it: the link takes no line and no
 *  pixel of the header's height. */
function expectLinkAddsNothing(
  reading: HeaderSize,
  without: HeaderSize,
  width: number,
  where: string
): void {
  const lines = reading.lines.length - without.lines.length;
  const height = reading.height - without.height;
  expect
    .soft(lines, `the link adds ${lines} lines to the header ${where} at ${width} px`)
    .toBeLessThanOrEqual(0);
  expect
    .soft(height, `the link adds ${height.toFixed(1)} px to the header ${where} at ${width} px`)
    .toBeLessThanOrEqual(0);
}

// c-pull-request-link (R22): the header draws the issue's pull request as one link on the
// properties line, after the assignee and before the count of what waits on the reader, with its
// number and its state in the page's words, and keeps root R13's sizes: 3 lines and 130 px at
// 1440 px, 6 lines and 180 px at 390 px. The harness runs with NATS off and serves no webhook, so
// the row writes the `pull_request_heads` rows an issue's pull requests would have left, an open
// one and a newer merged one, and reads the answer of `GET /api/v1/issues/{key}` as the server
// gives it. An issue with none carries no `pull_request` and draws no link. The row reads the
// plain seed at each width without the link and with it. Then it reads the header with the
// 115-character title and 20 labels without the link and with it, twice: with only the two
// questions waiting on the reader, where the line of the count and the labels has the
// least room left, and with an approval request waiting too, the worst header. Each is at 1440 px
// inside R13's sizes and at 390 px inside `WORST_HEADER_AT_390`, and the link takes no line and
// no pixel of the header the issue draws without it. A pass does not show a state change while
// the page stays open (the field comes with the issue's read).
// SLOW(>=2s): 14.1 s on chromium and 8.2 s on iphone (one worker, the pod): a project and
// the header's seed, four pull request rows written and deleted, a title, 20 labels and an
// approval request written, and six loads at each width: the plain seed and both long-title
// seeds, each read for its header's size without the link and with it.
test("the header links the issue's pull request @phone", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    const widths = desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width];
    const openUrl = `https://github.com/${PULL_REQUEST_REPOSITORY}/pull/101`;
    const mergedUrl = `https://github.com/${PULL_REQUEST_REPOSITORY}/pull/102`;
    /** The header's links to a pull request: the one link the line draws, none where absent. */
    const link = () => propertiesLine(page).getByRole("link");
    const loadAt = async (width: number) => {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
    };
    /** The line's parts in the order it draws them: the assignee, the link, the count. */
    const lineParts = () =>
      lineOrder(page, [
        { name: "assignee", selector: `select[aria-label="${assigneeControlName(seeded.key)}"]` },
        { name: "pull request", selector: '[data-testid="issue-pull-request"]' },
        { name: "count", selector: '[data-testid="issue-whose-turn"]' },
      ]);
    /** Reads the header's link as the open pull request, after the assignee and before the count,
     *  at each width, and hands the header's size to `judge`. */
    const expectOpenLinked = async (
      where: string,
      judge: (reading: HeaderSize, width: number) => void
    ) => {
      for (const width of widths) {
        await loadAt(width);
        await expect(link(), "the header draws no pull request link").toHaveCount(1);
        await expect(link(), "the header's link names another pull request").toHaveAttribute(
          "href",
          openUrl
        );
        await expect(link(), "the header's link says no number or no state").toHaveText(
          `#101 ${PULL_REQUEST_STATE_WORDS.open}`
        );
        expect(await lineParts(), "the link is not between the assignee and the count").toEqual([
          "assignee",
          "pull request",
          "count",
        ]);
        judge(await readHeader(page, width, where), width);
      }
    };
    /** Reads the header of `where`, which has the 115-character title, at each width with no pull
     *  request and then with the open one: each inside its limit, and the link taking no line
     *  and no pixel of the header drawn without it. The pull request is deleted after. */
    const expectLinkAddsNothingOn = async (where: string) => {
      const without = new Map<number, HeaderSize>();
      for (const width of widths) {
        await loadAt(width);
        await expect(issueHeader(page)).toContainText(MEASURED_TITLE);
        await expect(link(), "an issue with no pull request draws a link").toHaveCount(0);
        const reading = await readHeader(page, width, `${where}, no pull request`);
        without.set(width, reading);
        expectHeaderWithinItsLimit(reading, width, `${where}, no pull request`, true);
      }
      await writePullRequestHead({
        event: "2026-11-10T10:00:00Z",
        issue: seeded.key,
        number: 101,
        state: "open",
        url: openUrl,
      });
      await expectOpenLinked(`${where}, and a pull request`, (reading, width) => {
        const bare = without.get(width);
        if (bare === undefined)
          throw new Error(`no reading of the header without the link at ${width} px`);
        expectHeaderWithinItsLimit(reading, width, `${where}, and a pull request`, true);
        expectLinkAddsNothing(reading, bare, width, where);
      });
      await sql(`DELETE FROM pull_request_heads WHERE issue_key = '${seeded.key}'`);
    };

    // An issue that holds no pull request carries no field, and draws no link.
    expect(
      Object.hasOwn(await getIssue(seeded.key), "pull_request"),
      "an issue with no pull request carries a pull_request"
    ).toBe(false);
    const without = new Map<number, HeaderSize>();
    for (const width of widths) {
      await loadAt(width);
      await expect(link(), "an issue with no pull request draws a link").toHaveCount(0);
      const reading = await readHeader(page, width, "on the seeded issue, no pull request");
      without.set(width, reading);
      expectHeaderWithinItsLimit(reading, width, "on the seeded issue, no pull request", false);
    }

    // An open pull request and a newer merged one: the server names the open one, and the header
    // links it.
    await writePullRequestHead({
      event: "2026-11-10T10:00:00Z",
      issue: seeded.key,
      number: 101,
      state: "open",
      url: openUrl,
    });
    await writePullRequestHead({
      event: "2026-11-12T10:00:00Z",
      issue: seeded.key,
      number: 102,
      state: "merged",
      url: mergedUrl,
    });
    expect(
      (await getIssue(seeded.key)).pull_request,
      "the issue names another pull request"
    ).toEqual({ number: 101, repo: PULL_REQUEST_REPOSITORY, state: "open", url: openUrl });
    const seededIssue = "on the seeded issue";
    await expectOpenLinked(seededIssue, (reading, width) => {
      const bare = without.get(width);
      if (bare === undefined)
        throw new Error(`no reading of the header without the link at ${width} px`);
      expectHeaderWithinItsLimit(reading, width, seededIssue, false);
      expectLinkAddsNothing(reading, bare, width, seededIssue);
    });

    // The pull requests gone, the field and the link go with them.
    await sql(`DELETE FROM pull_request_heads WHERE issue_key = '${seeded.key}'`);
    expect(
      Object.hasOwn(await getIssue(seeded.key), "pull_request"),
      "the issue still carries a pull_request"
    ).toBe(false);

    // The 115-character title and 20 labels, with only the two questions waiting on the reader:
    // the line that holds the count holds the labels and their count too, with the least room
    // left of any line of the header.
    await patchIssue(seeded.key, { labels: [...HEADER_LABELS], title: MEASURED_TITLE });
    await expectLinkAddsNothingOn("with the longest title and 20 labels, no request");
    // The worst header: the same, and an approval request waiting on the reader.
    await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
    await expectLinkAddsNothingOn("with the longest title, 20 labels and a request");
  });
});

/** The layers of the title block's computed `box-shadow`, in words: "the lower line" for the
 *  1 px line in `lineColour`, the page's line colour as the browser writes it, "the soft shadow"
 *  for the prototype's 6 px shadow, each other layer as the browser writes it, and nothing for
 *  `none` or for the transparent layers Tailwind sets under every shadow. A comma inside a
 *  colour's parentheses splits no layer. */
function titleBlockEdge(shadow: string, lineColour: string): string[] {
  if (shadow === "none") return [];
  return shadow
    .split(/,(?![^(]*\))/)
    .map((layer) => layer.trim())
    .filter((layer) => !layer.startsWith("rgba(0, 0, 0, 0) "))
    .map((layer) => {
      if (layer === `${lineColour} 0px 1px 0px 0px`) return "the lower line";
      return layer.endsWith(" 0px 6px 14px 0px") ? "the soft shadow" : layer;
    });
}

// c-title-edge (R22): from 761 px, where the title block stays at the top of the screen, it draws
// its lower line, 1 px in the page's line colour, from the first pixel the page scrolls, and its
// soft shadow under that line past 40 px as the prototype does, so the properties line passes
// under an edge and not under a cut; at the page's top it draws neither. It is read on part P's
// header seed with 20 labels written, so the properties line that passes under the block holds
// labels and a photo, the assignee's circle: at 1440 px the block's computed `box-shadow` is
// `none` at the top, holds the line and no soft shadow at 1, 20 and 40 px scrolled, and the line
// and the soft shadow at 320 px. At 390 px the header scrolls away with the page and the block
// draws neither at any of those places. A pass does not show 760 px or 761 px, which the row does
// not read; part P's sticky rows read both.
// SLOW(>=2s): the header's seed with 20 labels written, then a load of the issue into a short
// window at each of two widths, each page scrolled to five places and its edge read once it holds
// still (`settledValue`).
test("the title block draws its lower line from the first pixel scrolled", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...HEADER_LABELS] });
    for (const width of [DESKTOP.width, IPHONE_13.width]) {
      const stays = width > 760;
      // A short window and a page made taller than it, so the page scrolls 320 px at both widths,
      // opened as part P's sticky rows open it (`openScrollingPage` in `e2e/issue-page-p.e2e.ts`):
      // the outline's opening lands and the focus then leaves it, or the page's growth would
      // scroll back to the row it holds.
      await page.setViewportSize({ height: 420, width });
      await gotoIssue(page, `/issues/${seeded.key}/conversation`);
      await expectHeaderDrawn(page);
      await expectOpeningLanded(page);
      await focusThePage(page);
      await page.evaluate(() => {
        const issue = document.querySelector("[data-issue-page]");
        if (issue instanceof HTMLElement) issue.style.minHeight = "2000px";
        window.scrollTo(0, 0);
      });
      // The page's line colour as the browser writes a computed colour, read off an element that
      // takes it, so it compares with a layer of a computed `box-shadow`.
      const lineColour = await page.evaluate(() => {
        const root = getComputedStyle(document.documentElement);
        if (root.getPropertyValue("--color-page-line").trim() === "") {
          throw new Error("the page defines no --color-page-line");
        }
        const probe = document.createElement("span");
        probe.style.color = "var(--color-page-line)";
        document.body.append(probe);
        const colour = getComputedStyle(probe).color;
        probe.remove();
        return colour;
      });
      if (stays) {
        // What passes under the block as the page scrolls: the seed's labels and its photo.
        await expect(
          propertiesLine(page).locator("[data-header-label]").first(),
          "the properties line holds no label"
        ).toBeVisible();
        await expect(
          propertiesLine(page).locator("[data-initial]"),
          "the properties line holds no photo"
        ).toBeVisible();
      }
      for (const y of [0, 1, 20, 40, 320]) {
        await page.evaluate((top) => window.scrollTo(0, top), y);
        await expect
          .poll(async () => (await stickyReading(page))?.scrolled, {
            message: `the page did not scroll to ${y} px at ${width} px`,
          })
          .toBe(y);
        // The band read once two reads a poll apart agree: the scroll's handler and the render it
        // asked for are done by then.
        const reading = await settledValue(`the band at ${y} px scrolled at ${width} px`, () =>
          stickyReading(page)
        );
        if (reading === null) throw new Error(`the page draws no header at ${width} px`);
        expect(reading.scrolled, `the page left ${y} px scrolled at ${width} px`).toBe(y);
        if (!stays) {
          expect
            .soft(
              reading.shadow,
              `the title block draws an edge at ${y} px scrolled at ${width} px, where the header scrolls away`
            )
            .toBe("none");
          if (y === 320) {
            expect(
              reading.block.bottom,
              `the header stayed on screen at ${width} px`
            ).toBeLessThanOrEqual(0);
          }
          continue;
        }
        if (y === 0) {
          expect
            .soft(reading.shadow, "the title block draws an edge at the page's top")
            .toBe("none");
          continue;
        }
        const drawn = titleBlockEdge(reading.shadow, lineColour);
        const under = drawn.filter((layer) => layer !== "the lower line");
        expect
          .soft(drawn, `the title block draws no lower line at ${y} px scrolled`)
          .toContain("the lower line");
        if (y <= 40) {
          expect
            .soft(under, `the title block draws more than its lower line at ${y} px scrolled`)
            .toEqual([]);
        } else {
          expect
            .soft(under, `the title block draws no soft shadow at ${y} px scrolled`)
            .toEqual(["the soft shadow"]);
          expect(
            Math.abs(reading.block.top),
            `the title block left the top of the screen at ${y} px scrolled`
          ).toBeLessThanOrEqual(1);
        }
      }
    }
  });
});
