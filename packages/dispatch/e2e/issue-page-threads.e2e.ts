/** What each thread of an issue lists under is the server's decision: which threads a person wrote
 *  in or no person wrote in (`person=`), each thread's last activity and its page position
 *  (`order=activity`, `before`, `remaining`), which threads the reader wrote in and has not read
 *  (`me/replies` and its read marks), the reader's last read of the issue and the event log past
 *  it, and the records each write the rows send is stored as, which a recording would freeze. */

import { DISPATCH_WRITING_BOXES } from "@legion/contracts";
import type { Locator, Page, Response } from "@playwright/test";

import { plainDelivery } from "../web/src/features/conversation/delivery";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  CONTROL_LABELS,
  DECLARED_WORDS,
  PRESET_NAMES,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { compareTimestamps } from "../web/src/lib/timestamps";
import { createComment, createMessage, getAsk, getComment, getMessage, listComments } from "./api";
import { literal } from "./dashboard-support";
import { nextFrames } from "./frames";
import { replyRowId } from "./inbox-threads";
import {
  contentsPane,
  gotoIssue,
  issuePage,
  openDetails,
  openMenu,
  phase,
  presetChooser,
  presetRow,
  presetToggles,
  viewportAt,
} from "./issue-page";
import {
  expectIssuePageDrawn,
  expectOpeningLanded,
  focusRow,
  focusThePage,
  nodeRow,
  parentOfRow,
  rowAct,
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import { childrenSection, expectActivePreset, focusedNodeId, focusInside } from "./issue-page-i";
import { composer, composerTarget, outlineEnd, panelEntry, replyFromRow } from "./issue-page-l";
import {
  agentsPages,
  allFirstPages,
  allPages,
  allThreads,
  backToList,
  CHOICE_ORDER,
  CHOICES,
  CHOOSER,
  CHOOSER_FILTER,
  type Choice,
  type ChoicePage,
  choiceButton,
  chooserParties,
  cutAtWord,
  daemonName,
  drawEveryPage,
  drawnEntries,
  drawnRows,
  EDIT,
  expectedRow,
  expectRowsDrawn,
  expectThreadsDrawn,
  GONE_NODE,
  LIVE_PLANNER,
  loadThreads,
  MAIN_LINE,
  markReads,
  minePages,
  NEW_REPLIES,
  NEW_THREAD,
  NEW_THREAD_BOX,
  NOT_FOUND_LINE,
  newThreadPanel,
  openedRecord,
  openRow,
  PLANNER,
  pickChoice,
  pressThreads,
  READER,
  REPLY,
  REPLY_BOX,
  REVIEWER,
  readAs,
  recordBox,
  recordEntry,
  SELECTORS,
  SEND,
  type ServerThread,
  seedParties,
  seedThreads,
  showNextControl,
  THREADS,
  type ThreadRecord,
  type ThreadsSeed,
  threadRow,
  threadRows,
  threadsCount,
  threadsList,
  threadsMain,
  threadsMenuItem,
  threadsPath,
  threadsToggle,
  threadsView,
  waitingPages,
} from "./issue-page-threads";
import { refPicker } from "./ref-picker";
import { expect, test } from "./test";
import { type ApiRequest, expectLoadSettled, watchApiRequests } from "./traffic";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * The Threads view of the new issue page (dispatch://INFRA-5704), read on the real stack. Each
 * row is one row of the Verification document's "The browser file", under its title, and each
 * reading the Verification document's "Owed" paragraph lists is a `test.skip` of its own, tagged
 * `@owed`, with an annotation naming the work it waits for.
 *
 * Each row but row 14 turns the new page on with `phase` (`e2e/issue-page.ts`, which calls
 * `withIssuePage`), seeds its own issue under a fresh project key (`seedThreads`), and reads what
 * it expects from the server's records through the API (`e2e/issue-page-threads.ts`), never
 * from a count written here. Each row's first reading of Threads is an assertion of its own, so
 * at the stack base, which draws no Threads, each row fails there and not on its deadline.
 *
 * The rows read the view only through the contract `e2e/issue-page-threads.ts` states. Rows
 * tagged `@phone` also run in the `iphone` project at its own viewport; the others read 1440 px
 * and, where R2 names it, 390 px.
 */

/** The widths the rows read, the width from which the view row is a toolbar of toggles
 *  (`IssuePresets.tsx`), and the width from which the list and the main column stand side by
 *  side (R2). */
const DESKTOP_WIDTH = 1440;
const PHONE_WIDTH = 390;
const TOOLBAR_FROM = 1280;
const SIDE_BY_SIDE_FROM = 1024;

/** The widths R2 holds the view to one direction of scrolling at. */
const SCROLL_WIDTHS = [320, 390, 1024, 1440, 1920] as const;

/** The most requests under `/api/` a cold load sends (R15, root R21). */
const COLD_LOAD_AT_MOST = 40;

/** The widths a row reads in the project it runs in: the phone's alone in the `iphone`
 *  project, and both in the desktop one. */
function rowWidths(): number[] {
  return test.info().project.name === "iphone" ? [PHONE_WIDTH] : [DESKTOP_WIDTH, PHONE_WIDTH];
}

/** The seed with the second issue, for the rows that read it. */
function seedWithSecond(): Promise<ThreadsSeed> {
  return seedThreads({ second: true });
}

/** The seed with New thread's parties: the planner live and the reviewer's stopped role. */
async function seedWithParties(): Promise<ThreadsSeed> {
  const seed = await seedThreads();
  await seedParties(seed);
  return seed;
}

/** The URL of `path`, as a pattern a page's address ends with. */
function endsWith(path: string): RegExp {
  return new RegExp(`${literal(path)}$`);
}

/** The names of the view row's controls, in the order drawn: the toggles' names from 1280 px up,
 *  and the names of the one control's menu items below. */
async function viewRowNames(page: Page): Promise<string[]> {
  await presetRow(page).waitFor();
  // The view row is mounted and chooses toggles or one chooser synchronously from viewport width.
  if ((await presetChooser(page).count()) === 0) {
    return (await presetToggles(page).locator("[data-preset-name]").allTextContents()).map((name) =>
      name.trim()
    );
  }
  await presetChooser(page).click();
  const names = await openMenu(page).evaluate((menu) =>
    [...menu.querySelectorAll('[role="menuitemradio"]')].map((item) => {
      const nameId = (item.getAttribute("aria-labelledby") ?? "").split(" ")[0] ?? "";
      return (document.getElementById(nameId)?.textContent ?? "").trim();
    })
  );
  await page.keyboard.press("Escape");
  return names;
}

/** Fails unless the Threads control carries `count` in R1's words: as its description from
 *  1280 px up, and in its menu item's name below. */
async function expectThreadsCount(page: Page, width: number, count: number): Promise<void> {
  const at = `at ${width} px`;
  if (width >= TOOLBAR_FROM) {
    await expect(threadsCount(page), `the Threads control draws no count ${at}`).toHaveText(
      String(count)
    );
    await expect(
      threadsToggle(page),
      `the Threads control's count ${at} is not the rows of Waiting on me`
    ).toHaveAccessibleDescription(`${count} ${NEW_REPLIES}`);
    return;
  }
  await presetChooser(page).click();
  await expect(
    threadsMenuItem(page),
    `the Threads item's count ${at} is not the rows of Waiting on me`
  ).toHaveAccessibleName(`${THREADS} ${count} ${NEW_REPLIES}`);
  await page.keyboard.press("Escape");
  await expect(openMenu(page)).toHaveCount(0);
}

/** Fails where Threads draws what R1 leaves out of it: the outline, the Contents pane, the
 *  children's section, the three sentences, a version control, and the foot's box and read mark
 *  (R16). */
async function expectOnlyThreads(page: Page, where: string): Promise<void> {
  await expect(tree(page), `${where} draws the outline`).toHaveCount(0);
  await expect(contentsPane(page), `${where} draws the Contents pane`).toHaveCount(0);
  await expect(childrenSection(page), `${where} draws a children's section`).toHaveCount(0);
  for (const label of [
    CONTROL_LABELS.in_short,
    CONTROL_LABELS.in_short_what,
    CONTROL_LABELS.in_short_why,
    CONTROL_LABELS.in_short_done,
  ]) {
    await expect(
      issuePage(page).getByText(label, { exact: true }),
      `${where} draws the three sentences' "${label}"`
    ).toHaveCount(0);
  }
  await expect(
    issuePage(page).getByLabel(CONTROL_LABELS.version, { exact: true }),
    `${where} draws a version control`
  ).toHaveCount(0);
  await expect(composer(page), `${where} draws the box at the foot`).toBeHidden();
  await expect(outlineEnd(page), `${where} draws the issue's read mark`).toHaveCount(0);
}

/** Fails unless R2's layout holds at `width` with the record `id` opened and closed: from
 *  1024 px up the list at the left and the main column beside it, with its one line and then the
 *  record; below, the list alone, then the record alone with the control back to the list. */
async function expectLayout(page: Page, key: string, width: number, id: string): Promise<void> {
  const at = `at ${width} px`;
  await expect(threadsList(page), `Threads draws no list ${at}`).toBeVisible();
  if (width >= SIDE_BY_SIDE_FROM) {
    await expect(
      threadsMain(page).getByText(MAIN_LINE, { exact: true }),
      `the main column draws no line with no record open ${at}`
    ).toBeVisible();
    const list = await threadsList(page).boundingBox();
    const main = await threadsMain(page).boundingBox();
    expect(
      list !== null && main !== null && list.x + list.width <= main.x + 1,
      `the list does not stand at the left of the main column ${at}`
    ).toBe(true);
    await openRow(page, key, id);
    await expect(threadsList(page), `the list leaves when a record opens ${at}`).toBeVisible();
    await expect(openedRecord(page)).toBeVisible();
    await page.goBack();
    await expect(openedRecord(page)).toHaveCount(0);
    return;
  }
  await expect(
    threadsMain(page).getByText(MAIN_LINE, { exact: true }),
    `the main column's line stands beside the list ${at}`
  ).toBeHidden();
  await openRow(page, key, id);
  await expect(openedRecord(page)).toBeVisible();
  await expect(
    threadsList(page),
    `the list and the record both stand on screen ${at}`
  ).toBeHidden();
  await expect(
    backToList(page),
    `an opened record draws no control back to the list ${at}`
  ).toBeVisible();
  await backToList(page).click();
  await expect(threadsList(page)).toBeVisible();
  await expect(openedRecord(page)).toHaveCount(0);
}

/** The id of the record a `201` answer holds. */
async function createdId(response: Response): Promise<string> {
  const body: unknown = await response.json();
  if (typeof body !== "object" || body === null || !("id" in body) || typeof body.id !== "string") {
    throw new Error(`the write answered no id: ${JSON.stringify(body)}`);
  }
  return body.id;
}

/** Fails where an element of the view scrolls sideways or the page is wider than its viewport
 *  (R2, root R18). */
async function expectOneDirection(page: Page, where: string): Promise<void> {
  const sideways = await threadsView(page).evaluate((view) =>
    [view, ...view.querySelectorAll("*")]
      .filter((element) => {
        const style = getComputedStyle(element);
        return (
          (style.overflowX === "auto" || style.overflowX === "scroll") &&
          element.scrollWidth > element.clientWidth + 1
        );
      })
      .map((element) => `${element.tagName}: ${element.scrollWidth} in ${element.clientWidth}`)
  );
  expect(sideways, `${where}: part of Threads scrolls sideways`).toEqual([]);
  await assertPageFits(page);
}

/** Presses `t` and then `t`, with the focus on the page itself. */
async function chordTT(page: Page): Promise<void> {
  await focusThePage(page);
  await page.keyboard.press("t");
  await page.keyboard.press("t");
}

/** Every record of every thread of All, by the record's id. */
async function recordsById(
  key: string
): Promise<ReadonlyMap<string, { readonly record: ThreadRecord; readonly thread: ServerThread }>> {
  const records = new Map<string, { record: ThreadRecord; thread: ServerThread }>();
  for (const thread of (await allThreads(key)).values()) {
    for (const record of thread.records) records.set(record.id, { record, thread });
  }
  return records;
}

/** The ids of each page's rows. */
function pageIds(pages: readonly ChoicePage[]): string[][] {
  return pages.map((page) => page.rows.map((record) => record.id));
}

/** Waits for the answer to `method` on `path` (a path with no query), and answers it. */
function answerTo(page: Page, method: string, path: string) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === method && new URL(response.url()).pathname === path
  );
}

/** Writes `words` into the box `box` and sends it, and answers the id of the record the server
 *  stored and the body the page sent, read from the `POST` to `path`. */
async function sendFromBox(
  page: Page,
  box: Locator,
  words: string,
  path: string,
  field: string = REPLY_BOX
): Promise<{ readonly id: string; readonly sent: Record<string, unknown> }> {
  await box.getByRole("textbox", { exact: true, name: field }).fill(words);
  const answered = answerTo(page, "POST", path);
  await box.getByRole("button", { exact: true, name: SEND }).click();
  const response = await answered;
  expect(response.status(), `the send from the box answered ${response.status()}`).toBe(201);
  const sent: Record<string, unknown> = response.request().postDataJSON();
  return { id: await createdId(response), sent };
}

/** The GETs of `requests` that read one thread of `ids` through its own route, as Dashboard's
 *  thread item reads it (`threadQuery`): an ask, a comment thread or an issue message. */
function threadReads(
  requests: readonly ApiRequest[],
  key: string,
  ids: ReadonlySet<string>
): string[] {
  return requests
    .filter((sent) => sent.method === "GET")
    .map((sent) => new URL(sent.path, "http://dispatch").pathname)
    .filter((path) => {
      const id =
        /^\/api\/v1\/(?:asks|comments)\/([^/]+)$/.exec(path)?.[1] ??
        new RegExp(`^/api/v1/issues/${literal(key)}/messages/([^/]+)$`).exec(path)?.[1];
      return id !== undefined && ids.has(id);
    });
}

/** The entries of `record` in the order an opened record draws them: each reply after the entry
 *  it answers and that entry's earlier replies, with its depth (R9). */
function preorder(record: ThreadRecord): { readonly id: string; readonly depth: number }[] {
  const children = new Map<string, ThreadRecord["entries"][number][]>();
  for (const entry of record.entries.slice(1)) {
    const parent =
      entry.answers !== null && record.ids.has(entry.answers) ? entry.answers : record.root.id;
    children.set(parent, [...(children.get(parent) ?? []), entry]);
  }
  const order: { id: string; depth: number }[] = [];
  const walk = (id: string, depth: number) => {
    order.push({ depth, id });
    for (const child of children.get(id) ?? []) walk(child.id, depth + 1);
  };
  walk(record.root.id, 0);
  return order.filter((entry) => !entry.id.includes("#"));
}

/** Fails unless the opened record draws `record`'s entries in R9's order and depth, each reply to
 *  the right of the entry it answers, each with its time. */
async function expectEntriesDrawn(page: Page, record: ThreadRecord, where: string): Promise<void> {
  const expected = preorder(record);
  const drawn = await drawnEntries(page);
  expect(
    drawn.map(({ depth, id }) => ({ depth, id })),
    `${where}: the record draws its entries out of the order and depth R9 gives`
  ).toEqual(expected);
  const left = new Map(drawn.map((entry) => [entry.id, entry.left]));
  for (const entry of record.entries.slice(1)) {
    if (entry.answers === null || !record.ids.has(entry.answers)) continue;
    expect(
      (left.get(entry.id) ?? 0) > (left.get(entry.answers) ?? 0),
      `${where}: the reply ${entry.id} stands at the indent of the entry it answers`
    ).toBe(true);
  }
  for (const entry of record.entries) {
    const shown = drawn.find((one) => one.id === entry.id);
    if (shown === undefined) continue;
    expect(
      shown.time !== null && Date.parse(shown.time) === Date.parse(entry.at),
      `${where}: the entry ${entry.id} draws no time of its creation`
    ).toBe(true);
  }
}

/** Every text of the view that is no value: each visible text node outside `[data-author-text]`
 *  and `time`, and each placeholder. */
function viewWords(page: Page): Promise<string[]> {
  return threadsView(page).evaluate((view) => {
    const found: string[] = [];
    const walker = document.createTreeWalker(view, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const parent = node.parentElement;
      const text = (node.textContent ?? "").replace(/\s+/g, " ").trim();
      if (parent === null || text === "" || !parent.checkVisibility()) continue;
      if (parent.closest("[data-author-text], time") !== null) continue;
      found.push(text);
    }
    for (const field of view.querySelectorAll("[placeholder]")) {
      if (field.checkVisibility()) found.push(field.getAttribute("placeholder") ?? "");
    }
    return found;
  });
}

// =============================================================================================
// Row 1
// =============================================================================================

// SLOW(>=2s): the whole seed, then at each width three loads with the view row's presses and
// chords and a record opened and closed; at 1440 px a reply target held in Conversation, each
// counted thread opened until its read mark is written, and five widths read for scrolling.
test("Threads is the control after Decisions, a press or t t opens and closes it, and its count is the rows of Waiting on me @phone", async ({
  browser,
}) => {
  test.setTimeout(420_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const waiting = await waitingPages(key, seed.project);
    const counted = waiting.rows.length;
    const firstRecord = (await allPages(key))[0]?.rows[0]?.id ?? "";
    for (const width of rowWidths()) {
      const at = `at ${width} px`;
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, presetPath(key, "conversation"));

      // R1: the control after Decisions, named Threads, in root R2's order.
      const names = await viewRowNames(page);
      expect(
        names.slice(
          names.indexOf(PRESET_NAMES.decisions),
          names.indexOf(PRESET_NAMES.decisions) + 2
        ),
        `the view row ${at} holds no control named Threads right after Decisions`
      ).toEqual([PRESET_NAMES.decisions, THREADS]);

      // Its count is the rows of Waiting on me, in R1's words, and not the read's total, which
      // holds the daemon's thread.
      await expectThreadsCount(page, width, counted);

      // A press opens Threads at its own address, with All on, and nothing R1 leaves out.
      await pressThreads(page);
      await expect(page, `the Threads control ${at} did not open /threads`).toHaveURL(
        endsWith(threadsPath(key))
      );
      await expectThreadsDrawn(page, `the Threads control ${at}`);
      await expectActivePreset(page, THREADS);
      await expect(
        choiceButton(page, "all"),
        `All is not on when Threads opens ${at}`
      ).toHaveAttribute("aria-pressed", "true");
      await expectRowsDrawn(page, `Threads ${at}`);
      await expectOnlyThreads(page, `Threads at rest ${at}`);

      // R2: the list and the record side by side from 1024 px, and in turn below.
      await expectLayout(page, key, width, firstRecord);

      // A press while Threads is on returns to Conversation. The one control's menu keeps the
      // view that is on when its item is chosen, so below 1280 px the Conversation item is the
      // press that leaves Threads.
      if (width >= TOOLBAR_FROM) {
        await threadsToggle(page).click();
      } else {
        await presetChooser(page).click();
        await openMenu(page)
          .getByRole("menuitemradio", { name: new RegExp(`^${PRESET_NAMES.conversation}( |$)`) })
          .click();
      }
      await expect(page, `the press that leaves Threads ${at} did not open Conversation`).toHaveURL(
        endsWith(presetPath(key, "conversation"))
      );
      await expect(threadsView(page), `Conversation ${at} draws Threads`).toHaveCount(0);

      // t t does what a press does: it opens Threads, and again returns to Conversation.
      if (width >= TOOLBAR_FROM) await expectOpeningLanded(page);
      await chordTT(page);
      await expect(page, `t t did not open Threads ${at}`).toHaveURL(endsWith(threadsPath(key)));
      await expectThreadsDrawn(page, `t t ${at}`);
      await chordTT(page);
      await expect(page, `t t in Threads did not return to Conversation ${at}`).toHaveURL(
        endsWith(presetPath(key, "conversation"))
      );
    }
    if (test.info().project.name === "iphone") return;
    await page.setViewportSize(viewportAt(DESKTOP_WIDTH));

    // R16: a reply target held in Conversation draws no box in Threads.
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectIssuePageDrawn(page);
    const remark = await focusRow(page, seed.comments.remark);
    await rowAct(remark, "reply").click();
    await expect(composerTarget(page), "Reply on the remark holds no target").toBeVisible();
    await pressThreads(page);
    await expectThreadsDrawn(page, "Threads with a held target");
    await expect(
      composer(page),
      "Threads draws the box at the foot with a held target"
    ).toBeHidden();
    await expect(outlineEnd(page), "Threads draws the read mark with a held target").toHaveCount(0);

    // R2: one direction of scrolling from 320 px to 1920 px, with the list and with the record
    // whose root's first line is the longest.
    for (const width of SCROLL_WIDTHS) {
      await page.setViewportSize(viewportAt(width));
      await loadThreads(page, key);
      await expectRowsDrawn(page, `Threads at ${width} px`);
      await expectOneDirection(page, `the list at ${width} px`);
      await loadThreads(page, key, seed.messages.second);
      await expect(openedRecord(page)).toBeVisible();
      await expectOneDirection(page, `an opened record at ${width} px`);
    }
    await page.setViewportSize(viewportAt(DESKTOP_WIDTH));

    // The count draws nothing at 0: each counted thread opened, its newest record first, until
    // the read mark is written.
    const records = await recordsById(key);
    const threads = new Map([...records.values()].map(({ thread }) => [thread.id, thread]));
    for (const row of waiting.rows) {
      const newest = threads.get(row.thread_id)?.records[0];
      expect(newest, `${row.thread_id} of Waiting on me is in no page of All`).toBeDefined();
      const marked = answerTo(page, "PUT", `/api/v1/me/threads/${row.thread_id}/read`);
      await loadThreads(page, key, newest?.id);
      await marked;
    }
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectIssuePageDrawn(page);
    await expect(threadsCount(page), "the Threads control draws a count at 0").toHaveCount(0);
    await expect(presetRow(page), "the Threads control draws its words at 0").not.toContainText(
      NEW_REPLIES
    );
  });
});

test.skip("Threads is the control after Decisions, a press or t t opens and closes it, and its count is the rows of Waiting on me: Dashboard at rest draws neither the box nor the read mark @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-5702: the foot draws its box and the issue's read mark at rest on Dashboard until that branch holds the fix of its join with INFRA-3949 (decision 428), so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedThreads, async (page, seed) => {
    for (const width of rowWidths()) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, presetPath(seed.key, "dashboard"));
      await expectActivePreset(page, PRESET_NAMES.dashboard);
      await expect(composer(page), `Dashboard at rest draws the box at ${width} px`).toBeHidden();
      await expect(
        outlineEnd(page),
        `Dashboard at rest draws the read mark at ${width} px`
      ).toHaveCount(0);
    }
  });
});

// =============================================================================================
// Row 2
// =============================================================================================

// SLOW(>=2s): the whole seed, then every page of All drawn and read against every page of the
// three routes.
test("All merges the three pages by last activity, 20 at a time, and says how many are left", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const expected = await allPages(key);
    await loadThreads(page, key);
    await expect(choiceButton(page, "all"), "All is not on when Threads opens").toHaveAttribute(
      "aria-pressed",
      "true"
    );
    const drawn = await drawEveryPage(page, "All");
    expect(
      drawn.pages,
      "All's pages are not the server's records in R3's order, 20 rows a page"
    ).toEqual(pageIds(expected));
    expect(drawn.left, "the control under All's rows does not say how many R3 leaves").toEqual(
      expected.map((one) => one.left)
    );
    expect(expected.length, "the seed gives All fewer than three pages").toBeGreaterThanOrEqual(3);
    for (const [at, ids] of drawn.pages.entries()) {
      if (at < drawn.pages.length - 1) {
        expect(ids.length, `All's page ${at + 1} draws other than 20 rows`).toBe(20);
      }
    }
    const every = drawn.pages.flat();
    expect(new Set(every).size, "All draws a record on two pages").toBe(every.length);

    // What All holds and leaves out (R3).
    expect(every, "All draws no row of the open ask").toContain(seed.asks.open);
    expect(every.indexOf(seed.messages.second), "All puts the unread first").toBeLessThan(
      every.indexOf(seed.messages.first)
    );
    for (const [id, what] of [
      [seed.messages.daemon, "the Legion daemon's thread"],
      [seed.comments.agentsTalk, "the agents' comments to each other"],
      [seed.comments.status, "the agent's status note"],
      [seed.messages.agents, "the thread between agents"],
    ] as const) {
      expect(every, `All draws ${what}`).not.toContain(id);
    }
  });
});

// =============================================================================================
// Row 3
// =============================================================================================

// SLOW(>=2s): the whole seed with the resolved question, then every page of Waiting on me, Mine
// and Agents drawn, each read against its own route's pages and the requests the page sent.
test("Mine, Waiting on me and Agents each list their own threads from their own reads", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  const seedWithResolvedQuestion = () => seedThreads({ resolved: true });
  await phase(browser, seedWithResolvedQuestion, async (page, seed) => {
    const key = seed.key;
    const reads = watchApiRequests(page);
    await loadThreads(page, key);
    await expectRowsDrawn(page, "All");
    expect(
      await threadsView(page).locator(SELECTORS.choices).getByRole("button").allTextContents(),
      "the four choices are not drawn in R2's order"
    ).toEqual(CHOICE_ORDER.map((choice) => CHOICES[choice]));
    await expectLoadSettled(reads);
    const held = await allFirstPages(key);

    // Waiting on me: the rows of `me/replies` less the daemon's, in the route's order, each
    // drawn from a page the view holds, else from its thread's own read.
    const waiting = await waitingPages(key, seed.project);
    // The resolved question: the route lists it by its newest reply, below the reader's remark,
    // while its resolution, newer than every other entry of either thread, makes its last
    // activity the newer, so the route's order and last activity's order differ.
    const resolved = seed.asks.resolved ?? "";
    const routeOrder = waiting.rows.map((row) => row.thread_id);
    expect(routeOrder, "the route lists no row of the reader's remark").toContain(
      seed.comments.remark
    );
    expect(
      routeOrder.indexOf(seed.comments.remark),
      "the route lists the resolved question above the reader's remark"
    ).toBeLessThan(routeOrder.indexOf(resolved));
    const threads = await allThreads(key);
    const question = threads.get(resolved);
    const remark = threads.get(seed.comments.remark);
    const resolution = question?.entries.find((entry) => entry.id === `${resolved}#resolution`);
    if (question === undefined || remark === undefined || resolution === undefined) {
      throw new Error("All holds no resolved question with its resolution, or no remark");
    }
    expect(
      [...question.entries, ...remark.entries]
        .filter((entry) => entry !== resolution && compareTimestamps(entry.at, resolution.at) >= 0)
        .map((entry) => entry.id),
      "an entry of the question or of the remark is no older than the question's resolution"
    ).toEqual([]);
    reads.reset();
    await pickChoice(page, "waiting");
    const waitingDrawn = await drawEveryPage(page, "Waiting on me");
    const waitingIds = waitingDrawn.pages.flat();
    expect(waitingIds, "Waiting on me draws no row of the reader's remark").toContain(
      seed.comments.remark
    );
    expect(
      waitingIds.indexOf(seed.comments.remark),
      "Waiting on me draws the resolved question above the reader's remark"
    ).toBeLessThan(waitingIds.indexOf(resolved));
    expect(waitingDrawn.pages, "Waiting on me is not the route's rows in its order").toEqual(
      pageIds(waiting.pages)
    );
    expect(waitingDrawn.left).toEqual(waiting.pages.map((one) => one.left));
    expect(waitingIds, "Waiting on me draws no row of the first message thread").toContain(
      seed.messages.first
    );
    expect(waitingIds, "Waiting on me draws the third message thread").not.toContain(
      seed.messages.third
    );
    expect(waitingIds, "Waiting on me draws the Legion daemon's thread").not.toContain(
      seed.messages.daemon
    );
    await expectLoadSettled(reads);
    const unheld = new Set(
      waiting.rows.map((row) => row.thread_id).filter((thread) => !held.has(thread))
    );
    expect(
      threadReads(reads.requests, key, new Set(waiting.rows.map((row) => row.thread_id))).sort(),
      "Waiting on me reads a thread a page of All holds, or reads a thread twice"
    ).toEqual(threadReads(reads.requests, key, unheld).sort());
    expect(
      threadReads(reads.requests, key, unheld).length,
      "Waiting on me does not read each thread no page holds once"
    ).toBe(Math.min(unheld.size, 20));

    // Mine: the three pages with `author=` the reader, less the records the reader did not
    // write in, 20 rows a page while 20 are left.
    const mine = await minePages(key);
    reads.reset();
    await pickChoice(page, "mine");
    const mineDrawn = await drawEveryPage(page, "Mine");
    expect(mineDrawn.pages, "Mine is not the records the reader wrote in, in R3's order").toEqual(
      pageIds(mine)
    );
    expect(mineDrawn.left).toEqual(mine.map((one) => one.left));
    expect(mine.length, "the seed gives Mine fewer than two pages").toBeGreaterThanOrEqual(2);
    for (const [at, ids] of mineDrawn.pages.entries()) {
      if (at < mineDrawn.pages.length - 1) {
        expect(ids.length, `Mine's page ${at + 1} draws other than 20 rows`).toBe(20);
      }
    }
    const mineIds = mineDrawn.pages.flat();
    for (const [id, what] of [
      [seed.messages.first, "the first message thread"],
      [seed.messages.second, "the second message thread"],
      [seed.messages.fourth, "the fourth message thread"],
      [seed.comments.remark, "the reader's remark"],
    ] as const) {
      expect(mineIds, `Mine draws no row of ${what}`).toContain(id);
    }
    expect(mineIds, "Mine draws the thread another person began").not.toContain(
      seed.messages.third
    );
    expect(mineIds, "Mine draws a record the reader did not write in").not.toContain(
      seed.messages.quotingReply
    );
    const authored = reads.requests
      .filter((sent) => sent.method === "GET")
      .map((sent) => new URL(sent.path, "http://dispatch"))
      .filter((url) => /\/(comments|messages|asks)$/.test(url.pathname));
    expect(
      authored.length > 0 && authored.every((url) => url.searchParams.get("author") === READER),
      "Mine reads a page with no author="
    ).toBe(true);

    // Agents: `messages?person=none` alone, and the thread between agents alone.
    const agents = await agentsPages(key);
    reads.reset();
    await pickChoice(page, "agents");
    const agentsDrawn = await drawEveryPage(page, "Agents");
    expect(agentsDrawn.pages, "Agents is not the route's threads between agents").toEqual(
      pageIds(agents)
    );
    expect(agentsDrawn.pages.flat(), "Agents holds other than the thread between agents").toEqual([
      seed.messages.agents,
    ]);
    const routes = reads.requests
      .filter((sent) => sent.method === "GET")
      .map((sent) => new URL(sent.path, "http://dispatch"));
    expect(
      routes.some(
        (url) =>
          url.pathname === `/api/v1/issues/${key}/messages` &&
          url.searchParams.get("person") === "none"
      ),
      "Agents sends no read with person=none"
    ).toBe(true);
    expect(
      routes.filter(
        (url) =>
          /\/(comments|asks)$/.test(url.pathname) && url.searchParams.get("person") === "none"
      ),
      "a read of comments or asks carries person=none"
    ).toEqual([]);
  });
});

// =============================================================================================
// Row 4
// =============================================================================================

// SLOW(>=2s): the whole seed with the second issue's 205 events, then at each width every page of
// All drawn and every row read against the server's records, a record opened, and the second
// issue's list read.
test("each row draws its kind, its first line, who is in it, its last activity and its count of new writes @phone", async ({
  browser,
}) => {
  test.setTimeout(420_000);
  await phase(browser, seedWithSecond, async (page, seed) => {
    const key = seed.key;
    for (const width of rowWidths()) {
      const at = `at ${width} px`;
      await page.setViewportSize(viewportAt(width));
      await loadThreads(page, key);
      await drawEveryPage(page, `All ${at}`);
      const [records, reads] = await Promise.all([recordsById(key), markReads(key)]);
      // Every page of All has already settled in drawEveryPage above before this read runs.
      const drawn = await drawnRows(page);
      for (const row of drawn) {
        const known = records.get(row.id);
        expect(known, `${row.id} ${at} is no record of the server's pages`).toBeDefined();
        if (known === undefined) continue;
        const expected = expectedRow(known.record, known.thread, reads, key, seed.project);
        const where = `the row of ${row.id} ${at}`;
        expect(row.hrefs, `${where} is not one link to its record`).toHaveLength(1);
        expect(row.hrefs[0] ?? "", `${where} links elsewhere`).toMatch(endsWith(expected.href));
        expect(row.kind, `${where} names another kind`).toBe(expected.kind);
        expect(
          cutAtWord(row.firstLine, expected.firstLine),
          `${where} draws "${row.firstLine}", not its first line "${expected.firstLine}" cut at a word`
        ).toBe(true);
        expect(row.people, `${where} names other people`).toEqual(expected.people);
        expect(
          row.activity !== null && Date.parse(row.activity) === Date.parse(expected.activity),
          `${where} draws ${row.activity}, not its last activity ${expected.activity}`
        ).toBe(true);
        expect(row.mark, `${where} counts other than its new writes`).toBe(expected.mark);
      }

      // The marks the Verification document names.
      const markOfRow = (id: string) => drawn.find((row) => row.id === id)?.mark ?? null;
      if (width === rowWidths()[0]) {
        expect(markOfRow(seed.messages.first), "the first message thread's mark").toBe("2");
      }
      expect(markOfRow(seed.messages.third), "the third message thread's mark").toBe("1");
      expect(markOfRow(seed.asks.open), "the open ask's mark").toBe("1");
      expect(markOfRow(seed.messages.note), "the agent's note carries a mark").toBeNull();
      expect(
        drawn.find((row) => row.id === seed.comments.remark)?.people,
        `the remark's row ${at} does not name the Legion daemon by its words and key`
      ).toContain(daemonName(seed.project));
      await expect(threadsList(page), `a row ${at} names a session by its id`).not.toContainText(
        "legion-daemon:"
      );
      await expect(threadsList(page), `a row ${at} draws "Waits on you"`).not.toContainText(
        "Waits on you"
      );

      // The longest first line is cut at a word in script, and by no style.
      const long = threadRow(page, seed.messages.second).locator(SELECTORS.firstLine);
      const longText = ((await long.textContent()) ?? "").replace(/\s+/g, " ").trim();
      expect(longText.endsWith("…"), `the long first line ${at} is drawn whole in the DOM`).toBe(
        true
      );
      const styled = await long.evaluate((line) =>
        [line, ...line.querySelectorAll("*")].some((element) => {
          const style = getComputedStyle(element);
          return style.textOverflow === "ellipsis" || style.webkitLineClamp !== "none";
        })
      );
      expect(styled, `the long first line ${at} is cut by a style`).toBe(false);

      // The first message thread's mark goes once the reader opens it.
      if (width === rowWidths()[0]) {
        const marked = answerTo(page, "PUT", `/api/v1/me/threads/${seed.messages.first}/read`);
        await openRow(page, key, seed.messages.first);
        await marked;
        await loadThreads(page, key);
        await expectRowsDrawn(page, `All ${at}`);
        await expect(
          threadRow(page, seed.messages.first).locator(SELECTORS.mark),
          `the first message thread keeps its mark ${at} once opened`
        ).toHaveCount(0);
      }
    }

    // The second issue: more than 200 events past the reader's last read, so the count of the
    // note, whose write is one of them, carries "+".
    const second = seed.second;
    expect(second, "the seed wrote no second issue").not.toBeNull();
    if (second === null) return;
    const [secondRecords, secondReads] = await Promise.all([
      recordsById(second.key),
      markReads(second.key),
    ]);
    const note = secondRecords.get(second.note);
    expect(note, "the second issue's note is in no page of All").toBeDefined();
    if (note === undefined) return;
    const expected = expectedRow(note.record, note.thread, secondReads, second.key, seed.project);
    expect(expected.mark ?? "", "the server's records give the note no + mark").toMatch(/^\d+\+$/);
    await loadThreads(page, second.key);
    await expectRowsDrawn(page, `All of ${second.key}`);
    await expect(
      threadRow(page, second.note).locator(SELECTORS.mark),
      "the note's mark past the newest 200 events draws no +"
    ).toHaveText(expected.mark ?? "");
  });
});

// =============================================================================================
// Row 5
// =============================================================================================

// SLOW(>=2s): the whole seed, then every page of All drawn, four records opened, and one reply
// sent from a record a reply with a quote begins, read back after a reload.
test("a reply with a quote begins its own record, and a thread on a deleted node keeps its row with a mark", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const { comments, messages } = seed;
    const [records, reads] = await Promise.all([recordsById(key), markReads(key)]);
    await loadThreads(page, key);
    await drawEveryPage(page, "All");

    // The post and the record its quoting reply begins are two rows, the reply in one of them.
    for (const [post, quote] of [
      [comments.post, comments.postQuote],
      [messages.quoting, messages.quotingReply],
    ] as const) {
      await expect(
        threadRow(page, quote),
        `the reply with a quote ${quote} begins no row`
      ).toHaveCount(1);
      await expect(threadRow(page, post), `the post ${post} keeps no row of its own`).toHaveCount(
        1
      );
      for (const id of [post, quote]) {
        const known = records.get(id);
        expect(known, `${id} is no record of the server's pages`).toBeDefined();
        if (known === undefined) continue;
        const expected = expectedRow(known.record, known.thread, reads, key, seed.project);
        await expect(
          threadRow(page, id).locator(SELECTORS.mark),
          `the row of ${id} counts writes of another record`
        ).toHaveCount(expected.mark === null ? 0 : 1);
        if (expected.mark !== null) {
          await expect(threadRow(page, id).locator(SELECTORS.mark)).toHaveText(expected.mark);
        }
      }
    }
    await expect(
      threadRow(page, comments.textQuote),
      "the reply with a quote inside a record on text begins a row of its own"
    ).toHaveCount(0);

    // Each record drawn opened: the quoting record with its quoted words above its root, the
    // post's without the quoting reply's entries, and the record on text with the reply that
    // joined it.
    for (const id of [comments.postQuote, comments.post, messages.quotingReply, comments.text]) {
      const known = records.get(id);
      if (known === undefined) continue;
      await openRow(page, key, id);
      await expectEntriesDrawn(page, known.record, `the record ${id}`);
      if (known.record.quote !== null) {
        await expect(
          openedRecord(page).locator(SELECTORS.quote),
          `the record on text ${id} draws no words it quotes above its root`
        ).toHaveText(known.record.quote);
      }
      await page.goBack();
      await expect(openedRecord(page)).toHaveCount(0);
    }

    // A reply sent from the quoting record's box at its foot is drawn in it after a reload.
    await openRow(page, key, comments.postQuote);
    const reply = await sendFromBox(
      page,
      recordBox(page, "foot"),
      "Fifty would scroll.",
      `/api/v1/issues/${key}/comments`
    );
    await page.reload();
    await expect(
      recordEntry(page, reply.id),
      "the reply sent in the quoting record is not drawn in it after a reload"
    ).toBeVisible();
    const stored = (await getComment(comments.post)).replies?.find((one) => one.id === reply.id);
    expect(stored?.answers, "the reply is stored answering another entry").toBe(comments.postQuote);
  });
});

test.skip("a reply with a quote begins its own record, and a thread on a deleted node keeps its row with a mark: the deleted node's words and the node anchor @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-1253: the comments read carries node_anchor, and a node can be deleted in Graph, only once that part's node anchor is on the base, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedThreads, async (page, seed) => {
    // The server's comment page with the node anchor: the threads whose node is gone.
    const page1 = await readAs<{
      readonly threads: readonly {
        readonly comment: {
          readonly id: string;
          readonly node_anchor?: { readonly node?: { readonly state?: string } };
        };
      }[];
    }>(`/api/v1/issues/${seed.key}/comments?order=activity&person=in&limit=20&node_anchor=true`);
    const gone = page1.threads.filter((row) => row.comment.node_anchor?.node?.state === "gone");
    expect(gone.length, "the seed holds no thread on a node deleted in Graph").toBeGreaterThan(0);
    await loadThreads(page, seed.key);
    await drawEveryPage(page, "All");
    for (const { comment } of gone) {
      const row = threadRow(page, comment.id);
      await expect(
        row.locator(SELECTORS.gone),
        "a row on a deleted node draws no words for it"
      ).toHaveText(GONE_NODE);
      await expect(
        row.locator(SELECTORS.mark),
        "a row on a deleted node draws no mark"
      ).toHaveCount(1);
      await openRow(page, seed.key, comment.id);
      await expect(openedRecord(page).locator(SELECTORS.gone)).toHaveText(GONE_NODE);
      await expect(openedRecord(page).locator(SELECTORS.nodeLink)).toHaveCount(0);
      await page.goBack();
    }
    await openRow(page, seed.key, seed.comments.text);
    await expect(
      openedRecord(page).locator(SELECTORS.nodeLink),
      "the record on text draws no link to its node"
    ).toHaveCount(1);
  });
});

// =============================================================================================
// Row 6
// =============================================================================================

// SLOW(>=2s): the whole seed, then at each width two records opened, two replies sent from the
// first message thread and read back after a reload, and its read mark waited on.
test("an opened thread draws each reply under the entry it answers, with one box at its foot, and marks the thread read @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const { first, firstAgentReply } = seed.messages;
    const reads = watchApiRequests(page);
    for (const width of rowWidths()) {
      const at = `at ${width} px`;
      await page.setViewportSize(viewportAt(width));
      await loadThreads(page, key);
      await expectRowsDrawn(page, `All ${at}`);
      const opened = new Set<string>();

      // The first message thread: each reply under the entry it answers, to every depth.
      const records = await recordsById(key);
      const record = records.get(first)?.record;
      expect(record, "the first message thread is in no page of All").toBeDefined();
      if (record === undefined) return;
      const marked =
        width === rowWidths()[0] ? answerTo(page, "PUT", `/api/v1/me/threads/${first}/read`) : null;
      await openRow(page, key, first);
      opened.add(first);
      if (width < SIDE_BY_SIDE_FROM) {
        await expect(threadsList(page), `the record ${at} stands beside the list`).toBeHidden();
      }
      await expectEntriesDrawn(page, record, `the first message thread ${at}`);
      await expect(
        openedRecord(page).locator(SELECTORS.box),
        `the record ${at} draws other than one box at rest`
      ).toHaveCount(1);
      await expect(
        recordBox(page, "foot"),
        `the record ${at} draws no box at its foot`
      ).toHaveCount(1);
      expect(
        (await drawnEntries(page)).filter((entry) => entry.edit),
        `a message ${at} carries Edit`
      ).toEqual([]);

      // The read mark, read before the reader writes in the thread, since a write of the
      // reader's own moves the server's mark too: the PUT once the newest entry has stood on
      // screen, and no mark on the row after a reload.
      if (marked !== null) {
        await marked;
        await loadThreads(page, key);
        await expectRowsDrawn(page, `All ${at}`);
        await expect(
          threadRow(page, first).locator(SELECTORS.mark),
          "the first message thread keeps its mark after its read mark was written"
        ).toHaveCount(0);
        await openRow(page, key, first);
      }

      // A reply control opens a box under its entry, one at a time, and its reply is drawn
      // under that entry.
      await recordEntry(page, first).getByRole("button", { exact: true, name: REPLY }).click();
      await expect(recordBox(page, first)).toHaveCount(1);
      await recordEntry(page, firstAgentReply)
        .getByRole("button", { exact: true, name: REPLY })
        .click();
      await expect(
        recordBox(page, firstAgentReply),
        `the reply control ${at} opens no box under its entry`
      ).toHaveCount(1);
      await expect(recordBox(page, first), `two reply boxes ${at} stand open`).toHaveCount(0);
      const underEntry = await sendFromBox(
        page,
        recordBox(page, firstAgentReply),
        `Under the agent's reply ${at}.`,
        `/api/v1/issues/${key}/messages`
      );
      expect(
        underEntry.sent.in_reply_to,
        `the reply control's box ${at} answers another entry`
      ).toBe(firstAgentReply);
      const atFoot = await sendFromBox(
        page,
        recordBox(page, "foot"),
        `At the foot ${at}.`,
        `/api/v1/issues/${key}/messages`
      );
      expect(
        atFoot.sent.in_reply_to,
        `the box at the foot ${at} answers another entry than the root`
      ).toBe(first);
      await page.reload();
      await expect(recordEntry(page, atFoot.id)).toBeVisible();
      const entries = await drawnEntries(page);
      const parent = entries.findIndex((entry) => entry.id === firstAgentReply);
      const under = entries.findIndex((entry) => entry.id === underEntry.id);
      expect(under, `the reply control's reply ${at} is not drawn under its entry`).toBeGreaterThan(
        parent
      );
      expect(
        entries
          .slice(parent + 1, under)
          .every((entry) => entry.depth > (entries[parent]?.depth ?? 0)),
        `the reply control's reply ${at} is drawn outside its entry's replies`
      ).toBe(true);
      expect(entries[under]?.depth, `the reply control's reply ${at} stands at another depth`).toBe(
        (entries[parent]?.depth ?? 0) + 1
      );
      expect(entries.at(-1)?.id, `the foot's reply ${at} is not drawn at the record's foot`).toBe(
        atFoot.id
      );
      const stored = (await getMessage(key, first)).replies ?? [];
      expect(
        stored.find((one) => one.id === underEntry.id)?.in_reply_to,
        "the reply control's reply is stored answering another entry"
      ).toBe(firstAgentReply);
      expect(
        stored.find((one) => one.id === atFoot.id)?.in_reply_to,
        "the foot's reply is stored answering another entry"
      ).toBe(first);

      // The reader's own comment carries Edit, and no other entry does.
      await loadThreads(page, key);
      await expectRowsDrawn(page, `All ${at}`);
      await openRow(page, key, seed.comments.remark);
      opened.add(seed.comments.remark);
      await expect(
        recordEntry(page, seed.comments.remark).getByRole("button", { exact: true, name: EDIT }),
        `the reader's own comment ${at} carries no Edit`
      ).toHaveCount(1);
      expect(
        (await drawnEntries(page)).filter((entry) => entry.edit).map((entry) => entry.id),
        `an entry ${at} that is not the reader's carries Edit`
      ).toEqual([seed.comments.remark]);

      // The read mark: a PUT only for a thread opened, and one for the first message thread.
      const puts = reads.requests
        .filter((sent) => sent.method === "PUT")
        .map(
          (sent) =>
            /^\/api\/v1\/me\/threads\/([^/]+)\/read$/.exec(
              new URL(sent.path, "http://dispatch").pathname
            )?.[1]
        )
        .filter((id) => id !== undefined);
      expect(
        puts.filter((id) => !opened.has(id)),
        `Threads ${at} writes a read mark for a thread it did not open`
      ).toEqual([]);
      if (marked !== null) {
        expect(puts, "opening the first message thread writes no read mark").toContain(first);
      }
      reads.reset();
    }
  });
});

test.skip("an opened thread draws each reply under the entry it answers, with one box at its foot, and marks the thread read: the selection control in the opened record @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-5706 (R16): its selection control is mounted in the opened record, under its gate THREADS, only once that part is on the base, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedThreads, async (page, seed) => {
    await loadThreads(page, seed.key);
    await expectRowsDrawn(page, "All");
    await openRow(page, seed.key, seed.comments.text);
    const root = recordEntry(page, seed.comments.text);
    await root.evaluate((entry) => {
      const range = document.createRange();
      range.selectNodeContents(entry.querySelector("[data-author-text]") ?? entry);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
    });
    await expect(
      page.getByRole("button", { exact: true, name: "Comment" }),
      "selected words of an opened record draw no selection control"
    ).toBeVisible();
  });
});

// =============================================================================================
// Row 7
// =============================================================================================

// SLOW(>=2s): the whole seed, then the open ask opened, answered in its ask body, and read until
// the server holds it settled.
test("an open ask opened in Threads draws the ask body, its replies and one box", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const ask = seed.asks.open;
    const record = (await recordsById(key)).get(ask)?.record;
    expect(record, "the open ask is in no page of All").toBeDefined();
    if (record === undefined) return;
    const reads = watchApiRequests(page);
    await loadThreads(page, key);
    await expectRowsDrawn(page, "All");
    await openRow(page, key, ask);
    const body = openedRecord(page).locator("[data-ask-body]");
    await expect(body, "the open ask is drawn by no ask body").toHaveCount(1);
    await expectEntriesDrawn(page, record, "the open ask");
    await expect(
      openedRecord(page).locator(`${SELECTORS.box}, [data-ask-answer]`),
      "the open ask draws other than one box"
    ).toHaveCount(1);
    await expect(body.locator("[data-ask-answer]"), "the ask body draws no answer box").toHaveCount(
      1
    );
    await expectLoadSettled(reads);
    expect(
      reads.requests.filter(
        (sent) =>
          sent.method === "GET" &&
          new URL(sent.path, "http://dispatch").pathname === `/api/v1/asks/${ask}`
      ),
      "the open ask's replies are read through GET /api/v1/asks/{id}"
    ).toEqual([]);

    // Answered in the ask body, the ask settles, and its record's foot draws a reply box.
    await body.locator("[data-ask-answer]").fill("Yes, leave them out.");
    await body.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect
      .poll(async () => (await getAsk(ask)).ask.state, {
        message: "the answer did not settle the ask",
      })
      .not.toBe("open");
    await expect(
      recordBox(page, "foot"),
      "the settled ask's record draws no box at its foot"
    ).toHaveCount(1);
    await expect(openedRecord(page).locator(SELECTORS.box)).toHaveCount(1);
  });
});

// =============================================================================================
// Row 8
// =============================================================================================

// SLOW(>=2s): the whole seed with two parties, then New thread opened twice, each send read back
// from the server, an agent's reply written through the API, and the Inbox opened.
test("New thread sends a message to an agent, and the agent's reply comes back in that thread @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  await phase(browser, seedWithParties, async (page, seed) => {
    const key = seed.key;
    const width = rowWidths()[0] ?? DESKTOP_WIDTH;
    await page.setViewportSize(viewportAt(width));
    const parties = await chooserParties(key);
    const live = parties.find((party) => party.target === "role:planner");
    const stopped = parties.find(
      (party) => party.target.startsWith("role:") && party.capabilities.length === 0
    );
    expect(live, "the seed's live planner holds no role the chooser offers").toBeDefined();
    expect(stopped, "the seed holds no role that no live session holds").toBeDefined();
    if (live === undefined || stopped === undefined) return;
    const reads = watchApiRequests(page);
    await loadThreads(page, key);
    await expectRowsDrawn(page, "All");
    await expectLoadSettled(reads);

    for (const [party, delivery] of [
      [live, plainDelivery([live.capabilities])],
      [stopped, "aside"],
    ] as const) {
      const words = `A new thread to ${party.title}.`;
      // On pass one the list is open; on pass two the previous loop settled on the opened record.
      if (width < SIDE_BY_SIDE_FROM && (await backToList(page).count()) > 0) {
        await backToList(page).click();
      }
      reads.reset();
      await expect(
        threadsList(page).getByRole("button", { exact: true, name: NEW_THREAD }),
        "Threads draws no New thread under its rows"
      ).toBeVisible();
      await threadsList(page).getByRole("button", { exact: true, name: NEW_THREAD }).click();
      const panel = newThreadPanel(page);
      await expect(panel, "New thread opens nothing").toBeVisible();
      const box = panel.getByRole("textbox", { exact: true, name: NEW_THREAD_BOX });
      await expect(box, "New thread puts no caret in its box").toBeFocused();

      // The chooser lists the parties the `@` picker offers, each an agent, with a filter.
      const chooser = panel.getByRole("listbox", { exact: true, name: CHOOSER });
      await expect(chooser.getByRole("option").first()).toBeVisible();
      const offered = await chooser
        .getByRole("option")
        .evaluateAll((options) =>
          options.map((option) => option.getAttribute("data-thread-party") ?? "")
        );
      expect(
        offered.filter((target) => !/^(role|session):/.test(target)),
        "New thread offers a choice that is not an agent"
      ).toEqual([]);
      expect(
        [...offered].sort(),
        "New thread's chooser lists other parties than the picker"
      ).toEqual(parties.map((one) => one.target).sort());
      const filter = panel.getByRole("textbox", { exact: true, name: CHOOSER_FILTER });
      await filter.fill(live.title);
      await expect
        .poll(
          async () => {
            const names = await chooser.getByRole("option").allTextContents();
            return (
              names.length > 0 &&
              names.every((name) => name.toLowerCase().includes(live.title.toLowerCase()))
            );
          },
          { message: "the chooser's filter keeps a party it does not match" }
        )
        .toBe(true);
      await filter.fill("");
      const send = panel.getByRole("button", { exact: true, name: SEND });
      await expect(send, "Send is offered with no agent chosen").toBeDisabled();
      const option = chooser.locator(`[data-thread-party="${party.target}"]`);
      await option.click();
      await expect(option).toHaveAttribute("aria-selected", "true");
      await expect(send, "Send is offered with no words").toBeDisabled();
      await box.fill(words);
      await expect(send, "Send is not offered once an agent and words are there").toBeEnabled();
      expect(
        reads.requests
          .filter((sent) => sent.method === "GET")
          .map((sent) => new URL(sent.path, "http://dispatch").pathname)
          .filter(
            (path) => path !== "/api/v1/agents" && path !== `/api/v1/issues/${key}/collaborators`
          ),
        "New thread's chooser reads what the @ picker does not"
      ).toEqual([]);

      // Send writes a root message with the target and the delivery, and opens its thread.
      const answered = answerTo(page, "POST", `/api/v1/issues/${key}/messages`);
      await send.click();
      const response = await answered;
      expect(response.status(), "New thread's send was not stored").toBe(201);
      const sent: Record<string, unknown> = response.request().postDataJSON();
      expect(sent, "New thread sent another message").toMatchObject({
        body: words,
        delivery,
        target: party.target,
      });
      expect(sent.in_reply_to ?? null, "New thread's message answers a thread").toBeNull();
      const id = await createdId(response);
      await expect(page, "the new thread is not opened at its address").toHaveURL(
        endsWith(threadsPath(key, id))
      );
      await expect(openedRecord(page)).toHaveAttribute("data-thread-record", id);
      const stored = await getMessage(key, id);
      expect(stored.message.target, "the stored message names another target").toBe(party.target);
      expect(
        stored.message.deliveries?.find((attempt) => attempt.attempt === 1)?.delivery,
        "the stored message carries another delivery"
      ).toBe(delivery);

      // The agent's reply comes back in the thread, with no reload.
      const reply = await createMessage(
        key,
        { body: "On my way.", in_reply_to: id },
        { actor: PLANNER, as: "agent" }
      );
      await expect(
        recordEntry(page, reply.id),
        "the agent's reply is not drawn in the thread"
      ).toBeVisible();

      // The thread is the first row of All and of Mine.
      if (width < SIDE_BY_SIDE_FROM) await backToList(page).click();
      for (const choice of ["all", "mine"] as const) {
        await pickChoice(page, choice);
        await expect
          .poll(async () => (await drawnRows(page))[0]?.id, {
            message: `the new thread is not the first row of ${CHOICES[choice]}`,
          })
          .toBe(id);
      }
      if (party === live) {
        // The Inbox lists the thread by its own rule.
        await page.goto("/");
        await expect(
          page.locator(`[data-inbox-row="${replyRowId(id)}"]`),
          "the new thread with the agent's reply is not on the reader's Inbox"
        ).toBeVisible();
        await loadThreads(page, key);
        await expectRowsDrawn(page, "All");
      }
    }
  });
});

test.skip("New thread sends a message to an agent, and the agent's reply comes back in that thread: the new thread is no row of the outline @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-4062: the outline draws a message thread a person wrote in until that part's read is on the base (R10), so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedWithParties, async (page, seed) => {
    await loadThreads(page, seed.key);
    await expectRowsDrawn(page, "All");
    await threadsList(page).getByRole("button", { exact: true, name: NEW_THREAD }).click();
    const panel = newThreadPanel(page);
    await panel
      .getByRole("listbox", { exact: true, name: CHOOSER })
      .locator('[data-thread-party="role:planner"]')
      .click();
    const sent = await sendFromBox(
      page,
      panel,
      "No row of the outline.",
      `/api/v1/issues/${seed.key}/messages`,
      NEW_THREAD_BOX
    );
    await gotoIssue(page, presetPath(seed.key, "conversation"));
    await expectIssuePageDrawn(page);
    await expect(
      nodeRow(page, sent.id),
      "New thread's message is a row of the outline"
    ).toHaveCount(0);
  });
});

// =============================================================================================
// Row 9
// =============================================================================================

// SLOW(>=2s): the whole seed with the live planner, then each box of Threads the writing list
// names typed into, and one comment with a mention sent and read back.
test("@ in a box that writes a comment names an agent, and a box that writes a message offers no @", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedWithParties, async (page, seed) => {
    const key = seed.key;
    await loadThreads(page, key);
    await expectRowsDrawn(page, "All");

    // The comment box of the reader's remark: `@` lists the parties, and the agent chosen is
    // stored in the comment's mentions.
    await openRow(page, key, seed.comments.remark);
    const commentBox = recordBox(page, "foot");
    await commentBox.getByRole("textbox", { exact: true, name: REPLY_BOX }).fill("@");
    const picker = refPicker(page);
    await expect(picker, "@ in a comment box of Threads opens no picker").toBeVisible();
    await picker
      .getByRole("option", { name: /^planner\b/ })
      .first()
      .click();
    const field = commentBox.getByRole("textbox", { exact: true, name: REPLY_BOX });
    await field.press("End");
    await field.pressSequentially(" can you check the list?");
    const answered = answerTo(page, "POST", `/api/v1/issues/${key}/comments`);
    await commentBox.getByRole("button", { exact: true, name: SEND }).click();
    const id = await createdId(await answered);
    const stored = (await getComment(seed.comments.remark)).replies?.find((one) => one.id === id);
    expect(
      (stored?.mentions ?? []).map((mention) => mention.target),
      "the comment sent from Threads stores no mention of the agent chosen"
    ).toContain("role:planner");

    // The message box of the first message thread offers no `@`.
    await page.goBack();
    await openRow(page, key, seed.messages.first);
    await recordBox(page, "foot").getByRole("textbox", { exact: true, name: REPLY_BOX }).fill("@");
    await nextFrames(page);
    await expect(refPicker(page), "@ in a message box of Threads opens the picker").toHaveCount(0);

    // New thread's box offers no `@`.
    await loadThreads(page, key);
    await threadsList(page).getByRole("button", { exact: true, name: NEW_THREAD }).click();
    await newThreadPanel(page)
      .getByRole("textbox", { exact: true, name: NEW_THREAD_BOX })
      .fill("@");
    await expect(refPicker(page), "@ in New thread's box opens the picker").toHaveCount(0);

    // Each box the writing list names under Threads: a covered one writes a comment and carries
    // the picker, and one that writes a message carries none (R11).
    const rows = DISPATCH_WRITING_BOXES.filter((row) =>
      row.files.some((file) => file.includes("/features/issue-page/threads/"))
    );
    expect(rows.length, "the writing boxes' list names no box of Threads").toBeGreaterThan(0);
    for (const row of rows) {
      const write: string = row.write;
      expect(
        row.covered,
        `the box ${row.box} writing ${write} carries the picker where it writes a message, or none where it writes a comment's words, a comment or its edit`
      ).toBe(write === "comment" || write === "comment-edit");
      await loadThreads(page, key);
      await expectRowsDrawn(page, "All");
      let scope: Locator;
      if (write === "comment") {
        await openRow(page, key, seed.comments.remark);
        scope = openedRecord(page);
      } else if (write === "comment-edit") {
        // The edit box opens only from Edit on the reader's own comment, as a reader opens it.
        await openRow(page, key, seed.comments.remark);
        await recordEntry(page, seed.comments.remark)
          .getByRole("button", { exact: true, name: EDIT })
          .click();
        scope = openedRecord(page);
      } else if (write === "message-reply") {
        await openRow(page, key, seed.messages.first);
        scope = openedRecord(page);
      } else {
        await threadsList(page).getByRole("button", { exact: true, name: NEW_THREAD }).click();
        scope = newThreadPanel(page);
      }
      const reached = row.reach.startsWith("[")
        ? scope.locator(row.reach)
        : scope.getByRole("textbox", { exact: true, name: row.reach });
      await expect(reached.first(), `the box ${row.box} is not where its row says`).toBeVisible();
      await reached.first().fill("@");
      if (row.covered) {
        await expect(refPicker(page), `@ in the box ${row.box} opens no picker`).toBeVisible();
      } else {
        await expect(refPicker(page), `@ in the box ${row.box} opens the picker`).toHaveCount(0);
      }
    }
  });
});

// =============================================================================================
// Row 10
// =============================================================================================

/** One address of R12 and what it opens: its record, under which choice. */
interface Address {
  readonly path: string;
  readonly record: string;
  readonly under: Choice;
  /** The message the address names, which holds the focus. */
  readonly focus?: string;
}

// SLOW(>=2s): the whole seed with the second issue, then eleven loads of R12's addresses, six
// rows followed by their links, and the record closed by Back and by Escape.
test("/threads, /threads/<id> and the address of a message only Threads draws open Threads, and Back and Escape return to the list", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  await phase(browser, seedWithSecond, async (page, seed) => {
    const key = seed.key;
    const reads = watchApiRequests(page);
    const [allFirst, agentsFirst] = await Promise.all([allPages(key), agentsPages(key)]);
    const heldRows = {
      agents: new Set(pageIds(agentsFirst.slice(0, 1)).flat()),
      all: new Set(pageIds(allFirst.slice(0, 1)).flat()),
    };

    // `/threads`: Threads with All on and no record open.
    await gotoIssue(page, threadsPath(key));
    await expectThreadsDrawn(page, "/threads");
    await expect(choiceButton(page, "all"), "/threads opens with All off").toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(openedRecord(page), "/threads opens a record").toHaveCount(0);

    // `/threads/<id>` of each kind of id, and `/messages/<id>` of the two kinds no view draws.
    const addresses: Address[] = [
      { path: threadsPath(key, seed.asks.open), record: seed.asks.open, under: "all" },
      { path: threadsPath(key, seed.comments.remark), record: seed.comments.remark, under: "all" },
      { path: threadsPath(key, seed.messages.first), record: seed.messages.first, under: "all" },
      {
        path: threadsPath(key, seed.comments.postQuote),
        record: seed.comments.postQuote,
        under: "all",
      },
      {
        path: threadsPath(key, seed.messages.agents),
        record: seed.messages.agents,
        under: "agents",
      },
      { path: threadsPath(key, seed.messages.note), record: seed.messages.note, under: "all" },
      {
        focus: seed.messages.agentsReply,
        path: buildIssuePath({ id: seed.messages.agentsReply, key, kind: "message" }),
        record: seed.messages.agents,
        under: "agents",
      },
      {
        focus: seed.messages.note,
        path: buildIssuePath({ id: seed.messages.note, key, kind: "message" }),
        record: seed.messages.note,
        under: "all",
      },
    ];
    for (const address of addresses) {
      await gotoIssue(page, address.path);
      await expectThreadsDrawn(page, address.path);
      await expect(openedRecord(page), `${address.path} opens another record`).toHaveAttribute(
        "data-thread-record",
        address.record
      );
      await expect(
        choiceButton(page, address.under),
        `${address.path} opens with ${CHOICES[address.under]} off`
      ).toHaveAttribute("aria-pressed", "true");
      if (
        address.under !== "mine" &&
        address.under !== "waiting" &&
        heldRows[address.under].has(address.record)
      ) {
        await expect(
          threadRow(page, address.record),
          `${address.path} draws no row of its record`
        ).toHaveCount(1);
      }
      if (address.focus !== undefined) {
        await expect
          .poll(() => focusInside(recordEntry(page, address.focus ?? "")), {
            message: `${address.path} does not focus its message`,
          })
          .toBe(true);
      }
    }

    // The same records by their rows' links.
    let opened = 0;
    for (const address of addresses.slice(0, 6)) {
      await loadThreads(page, key);
      await pickChoice(page, address.under);
      if (!heldRows[address.under === "agents" ? "agents" : "all"].has(address.record)) continue;
      await openRow(page, key, address.record);
      opened += 1;
    }
    expect(
      opened,
      `row 10 opened ${opened} held records by their rows' links, expected at least 1`
    ).toBeGreaterThan(0);

    // `/messages/<id>` of a thread the outline draws opens there, as `main` opens it, and so does
    // a message of the thread the Legion daemon began.
    for (const message of [seed.messages.first, seed.messages.daemon]) {
      const path = buildIssuePath({ id: message, key, kind: "message" });
      await gotoIssue(page, path);
      await expect(
        threadsView(page),
        `${path} opens Threads where the outline draws it`
      ).toHaveCount(0);
      await expectActivePreset(page, PRESET_NAMES.conversation);
      await expectOpeningLanded(page);
      await expect
        .poll(() => focusedNodeId(page), {
          message: `${path} does not focus its row in the outline`,
        })
        .toBe(message);
    }

    // An id of another issue's thread, and an id that is no UUID, name no record of this issue;
    // the second is refused before any read.
    const second = seed.second;
    expect(second, "the seed wrote no second issue").not.toBeNull();
    for (const id of [second?.note ?? "", "not-a-thread"]) {
      reads.reset();
      await gotoIssue(page, threadsPath(key, id));
      await expectThreadsDrawn(page, threadsPath(key, id));
      await expect(
        threadsMain(page).getByText(NOT_FOUND_LINE, { exact: true }),
        `/threads/${id} draws no line for an address it cannot find`
      ).toBeVisible();
      await expect(openedRecord(page), `/threads/${id} opens a record`).toHaveCount(0);
    }
    await expectLoadSettled(reads);
    expect(
      reads.requests.filter((sent) => sent.path.includes("/not-a-thread")),
      "an id that is no UUID is looked up"
    ).toEqual([]);

    // Back returns to the list, and so does Escape with no caret in a box; with a caret, Escape
    // leaves the record and the words.
    const target = [...heldRows.all][0] ?? "";
    await loadThreads(page, key);
    await expectRowsDrawn(page, "All");
    await openRow(page, key, target);
    await page.goBack();
    await expect(page, "Back does not return to the list").toHaveURL(endsWith(threadsPath(key)));
    await expect(openedRecord(page)).toHaveCount(0);
    await expect(choiceButton(page, "all")).toHaveAttribute("aria-pressed", "true");
    await openRow(page, key, target);
    await focusThePage(page);
    await page.keyboard.press("Escape");
    await expect(page, "Escape does not return to the list").toHaveURL(endsWith(threadsPath(key)));
    await expect(openedRecord(page)).toHaveCount(0);
    await openRow(page, key, target);
    const box = recordBox(page, "foot").getByRole("textbox", { exact: true, name: REPLY_BOX });
    await box.click();
    await box.pressSequentially("Words kept.");
    await page.keyboard.press("Escape");
    await expect(openedRecord(page), "Escape in a box closes the record").toHaveAttribute(
      "data-thread-record",
      target
    );
    await expect(box, "Escape in a box keeps the caret there").not.toBeFocused();
    await expect(box, "Escape in a box loses its words").toHaveValue("Words kept.");
  });
});

test.skip("/threads, /threads/<id> and the address of a message only Threads draws open Threads, and Back and Escape return to the list: a message's context row opens its record in Threads @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-4062: a message's context row in Conversation, a message under which a node it produced stands, comes with that part's read, so this reading is skipped until it is on the base.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedThreads, async (page, seed) => {
    await gotoIssue(page, presetPath(seed.key, "conversation"));
    await expectIssuePageDrawn(page);
    const context = issuePage(page).locator("[data-node-row][data-node-context]").first();
    await expect(context, "the outline draws no context row of a message").toBeVisible();
    const id = (await context.getAttribute("data-node-id")) ?? "";
    await context.getByRole("link").first().click();
    await expectThreadsDrawn(page, "a message's context row");
    await expect(
      recordEntry(page, id),
      "the context row opens no record holding its message"
    ).toHaveCount(1);
  });
});

test.skip("/threads, /threads/<id> and the address of a message only Threads draws open Threads, and Back and Escape return to the list: a comment on text opens by INFRA-5706's rule @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-5706 (R12): a comment on text opens in the view that draws its node, with its margin thread open, only once that part is on the base, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedThreads, async (page, seed) => {
    const path = buildIssuePath({ id: seed.comments.text, key: seed.key, kind: "comment" });
    await gotoIssue(page, path);
    await expect(
      threadsView(page),
      `${path} opens Threads where a view draws its node`
    ).toHaveCount(0);
    await expect(
      issuePage(page).locator("[data-text-margin]").getByText("Is a tie broken by the id?"),
      `${path} opens no margin thread of the comment`
    ).toBeVisible();
  });
});

// =============================================================================================
// Row 11
// =============================================================================================

// SLOW(>=2s): the whole seed, then every page of each choice drawn, and the outline read.
test("an agent's note and a thread between agents are drawn in Threads and nowhere else on the page", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const { messages, comments } = seed;
    await loadThreads(page, key);
    const drawn: Record<Choice, string[]> = { agents: [], all: [], mine: [], waiting: [] };
    for (const choice of CHOICE_ORDER) {
      await pickChoice(page, choice);
      drawn[choice] = (await drawEveryPage(page, CHOICES[choice])).pages.flat();
    }
    expect(drawn.all, "All draws no row of the agent's note").toContain(messages.note);
    expect(drawn.agents, "Agents draws no row of the thread between agents").toContain(
      messages.agents
    );
    for (const choice of CHOICE_ORDER) {
      if (choice !== "all") {
        expect(drawn[choice], `${CHOICES[choice]} draws the agent's note`).not.toContain(
          messages.note
        );
      }
      if (choice !== "agents") {
        expect(drawn[choice], `${CHOICES[choice]} draws the thread between agents`).not.toContain(
          messages.agents
        );
      }
      for (const [id, what] of [
        [messages.daemon, "the Legion daemon's thread"],
        [comments.agentsTalk, "the agents' comments to each other"],
        [comments.status, "the agent's status note"],
      ] as const) {
        expect(drawn[choice], `${CHOICES[choice]} draws ${what}`).not.toContain(id);
      }
    }
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectIssuePageDrawn(page);
    await expect(nodeRow(page, messages.note), "the outline draws the agent's note").toHaveCount(0);
    await expect(
      nodeRow(page, messages.agents),
      "the outline draws the thread between agents"
    ).toHaveCount(0);
  });
});

test.skip("an agent's note and a thread between agents are drawn in Threads and nowhere else on the page: the history in Details @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-3948: the history in Details is a stub on the base (DetailsHistory in details-more.tsx draws nothing) until that part is on it, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedThreads, async (page, seed) => {
    await gotoIssue(page, presetPath(seed.key, "conversation"));
    await expectIssuePageDrawn(page);
    await openDetails(page);
    await expect(
      panelEntry(page, "Planning started."),
      "the history holds no daemon's thread"
    ).toBeVisible();
    await expect(
      panelEntry(page, "Status: the plan is written."),
      "the history holds no agents' post"
    ).toBeVisible();
    await expect(
      panelEntry(page, "The planner reads the spec before it writes the plan."),
      "the history holds the agent's note"
    ).toHaveCount(0);
    await expect(
      panelEntry(page, "Which reads does the list send?"),
      "the history holds the thread between agents"
    ).toHaveCount(0);
  });
});

// =============================================================================================
// Row 12
// =============================================================================================

/** Where the page keeps what `watchListRows` records, until `seenListRows` reads it. */
const ROWS_SEEN = "threadsRowsSeen";

/** What the page keeps under `ROWS_SEEN`: the observer, and the list's row ids at each change. */
interface RowsSeen {
  readonly observer: MutationObserver;
  readonly seen: string[][];
}

/** Starts recording the ids of the rows the list draws at every change of the page, so a row
 *  that goes and is drawn again between two of a row's readings is still seen gone (R14). */
async function watchListRows(page: Page): Promise<void> {
  await page.evaluate(
    ({ key, list, row }) => {
      const seen: string[][] = [];
      const read = () =>
        [...document.querySelectorAll(`${list} ${row}`)].map(
          (element) => element.getAttribute("data-thread-row") ?? ""
        );
      const observer = new MutationObserver(() => seen.push(read()));
      observer.observe(document.body, { characterData: true, childList: true, subtree: true });
      // The window holds the recording under a name of the row's own, which no type declares.
      const held = window as unknown as Record<string, RowsSeen>;
      held[key] = { observer, seen };
    },
    { key: ROWS_SEEN, list: SELECTORS.list, row: SELECTORS.row }
  );
}

/** Stops the recording `watchListRows` started, and answers each list of row ids it recorded. */
async function seenListRows(page: Page): Promise<string[][]> {
  return page.evaluate((key) => {
    // The same name `watchListRows` wrote the recording under.
    const held = window as unknown as Record<string, RowsSeen | undefined>;
    held[key]?.observer.disconnect();
    return held[key]?.seen ?? [];
  }, ROWS_SEEN);
}

// SLOW(>=2s): the whole seed, then All's second page drawn, a record of it opened, an agent's
// reply written through the API and the reader's own reply sent, each waited on in the list.
test("a write that lands while Threads is open is drawn with no reload, and the rows paged to stay", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const expected = await allPages(key);
    const secondPage = expected[1]?.rows ?? [];
    const target = secondPage.find(
      (record) =>
        record.kind === "comment" && record.entries.every((entry) => entry.author.kind !== "user")
    );
    expect(target, "the seed puts no comment of agents alone on All's second page").toBeDefined();
    if (target === undefined) return;
    await loadThreads(page, key);
    await expectRowsDrawn(page, "All");
    await showNextControl(page).click();
    await expect
      .poll(() => threadRows(page).count())
      .toBeGreaterThanOrEqual((expected[0]?.rows.length ?? 0) + secondPage.length);
    const paged = (await drawnRows(page)).map((row) => row.id);
    expect(paged, "All's second page is not the server's").toEqual(
      pageIds(expected.slice(0, 2)).flat()
    );
    await openRow(page, key, target.id);
    // The rows of the second page drawn before the write, which the list draws at every moment
    // from the write until the row moves (R14), and not only where it comes to rest.
    const secondPageDrawn = paged.slice(expected[0]?.rows.length ?? 0);
    let replyId = "";
    let seen: string[][] = [];
    await watchListRows(page);
    try {
      // Another's write: an agent's reply, through the API.
      const reply = await createComment(
        key,
        { body: "A reply while the list is open.", reply_to: target.id },
        { actor: REVIEWER, as: "agent" }
      );
      replyId = reply.id;
      await expect
        .poll(async () => (await drawnRows(page))[0]?.id, {
          message: "the record of an agent's new reply does not move to the list's top",
        })
        .toBe(target.id);
    } finally {
      // seenListRows disconnects the mutation observer and returns snapshots already recorded before the poll settled.
      seen = await seenListRows(page);
    }
    expect(seen.length, "the list drew nothing as the agent's reply landed").toBeGreaterThan(0);
    for (const drawn of seen) {
      expect(
        secondPageDrawn.filter((id) => !drawn.includes(id)),
        "a row of the second page left the list while the agent's reply landed"
      ).toEqual([]);
    }
    const known = (await recordsById(key)).get(target.id);
    const mark =
      known === undefined
        ? null
        : expectedRow(known.record, known.thread, await markReads(key), key, seed.project).mark;
    expect(mark, "the server's records count no new write in the record").not.toBeNull();
    await expect(
      threadRow(page, target.id).locator(SELECTORS.mark),
      "the moved row draws no mark of its new write"
    ).toHaveText(mark ?? "");
    const after = (await drawnRows(page)).map((row) => row.id);
    expect(
      paged.filter((id) => !after.includes(id)),
      "the rows of the second page are gone after the write"
    ).toEqual([]);
    await expect(recordEntry(page, replyId), "the open record draws no new reply").toBeVisible();

    // The reader's own write, from the open record: its row stays first with no mark.
    const own = await sendFromBox(
      page,
      recordBox(page, "foot"),
      "My own reply.",
      `/api/v1/issues/${key}/comments`
    );
    await expect(
      recordEntry(page, own.id),
      "the open record draws no reply of the reader's"
    ).toBeVisible();
    await expect.poll(async () => (await drawnRows(page))[0]?.id).toBe(target.id);
    await expect(
      threadRow(page, target.id).locator(SELECTORS.mark),
      "the reader's own write is counted"
    ).toHaveCount(0);
    const afterOwn = (await drawnRows(page)).map((row) => row.id);
    expect(
      paged.filter((id) => !afterOwn.includes(id)),
      "the rows paged to are gone after the reader's write"
    ).toEqual([]);
    expect(
      afterOwn.length,
      "the list draws fewer rows after the reader's write"
    ).toBeGreaterThanOrEqual(paged.length);
  });
});

// =============================================================================================
// Row 13
// =============================================================================================

/** One read of the cold load, by what the six reads of R15 are. */
const SIX_READS = {
  asks: (url: URL, key: string) =>
    url.pathname === `/api/v1/issues/${key}/asks` &&
    url.searchParams.get("order") === "activity" &&
    url.searchParams.get("thread") === "1" &&
    url.searchParams.get("limit") === "20" &&
    !url.searchParams.has("before") &&
    !url.searchParams.has("author"),
  comments: (url: URL, key: string) =>
    url.pathname === `/api/v1/issues/${key}/comments` &&
    url.searchParams.get("order") === "activity" &&
    url.searchParams.get("person") === "in" &&
    url.searchParams.get("limit") === "20" &&
    !url.searchParams.has("before") &&
    !url.searchParams.has("author"),
  events: (url: URL, key: string) =>
    url.pathname === `/api/v1/issues/${key}/events` &&
    url.searchParams.get("order") === "desc" &&
    url.searchParams.get("limit") === "200",
  messages: (url: URL, key: string) =>
    url.pathname === `/api/v1/issues/${key}/messages` &&
    url.searchParams.get("order") === "activity" &&
    url.searchParams.get("person") === "in" &&
    url.searchParams.get("limit") === "20" &&
    !url.searchParams.has("before") &&
    !url.searchParams.has("author"),
  replies: (url: URL, key: string) =>
    url.pathname === "/api/v1/me/replies" &&
    url.searchParams.get("issue") === key &&
    url.searchParams.get("state") === "unread" &&
    !url.searchParams.has("limit"),
  state: (url: URL) => url.pathname === "/api/v1/me/state",
} as const;

/** A request as one string of its method, path and sorted query, so two loads compare. */
function readKey(sent: ApiRequest): string {
  const url = new URL(sent.path, "http://dispatch");
  const query = [...url.searchParams]
    .sort(([one], [two]) => one.localeCompare(two))
    .map(([name, value]) => `${name}=${value}`)
    .join("&");
  return `${sent.method} ${url.pathname}${query === "" ? "" : `?${query}`}`;
}

// SLOW(>=2s): the whole seed, then a cold load of /plan and one of /threads, each read until it
// settles, and two rows opened, each read until its read mark is answered.
test("opening Threads sends six reads and none for each row", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;

    // The same issue's Plan view, cold, in a tab of its own: what the page sends beside Threads.
    const plan = await page.context().newPage();
    const planReads = watchApiRequests(plan);
    await gotoIssue(plan, presetPath(key, "plan"));
    await expectLoadSettled(planReads);
    const planKeys = new Set(
      planReads.requests.filter((sent) => sent.method === "GET").map(readKey)
    );
    await plan.close();

    // `/threads`, cold, in a tab of its own.
    const cold = await page.context().newPage();
    const reads = watchApiRequests(cold);
    await loadThreads(cold, key);
    await expectRowsDrawn(cold, "a cold load of /threads");
    await expectLoadSettled(reads);
    const gets = reads.requests.filter((sent) => sent.method === "GET");
    for (const [name, matches] of Object.entries(SIX_READS)) {
      expect(
        gets.filter((sent) => matches(new URL(sent.path, "http://dispatch"), key)).length,
        `a cold load of /threads sends other than one ${name} read`
      ).toBe(1);
    }
    // Each read the Plan view does not send, named by the read of R15 it is, else by its path.
    const named = (sent: ApiRequest) => {
      const url = new URL(sent.path, "http://dispatch");
      const read = Object.entries(SIX_READS).find(([, matches]) => matches(url, key));
      return read?.[0] ?? `${url.pathname}${url.search}`;
    };
    const extra = gets.filter((sent) => !planKeys.has(readKey(sent))).map(named);
    expect(
      [...new Set(extra)].sort(),
      "a cold load of /threads sends other reads than All's three pages and the events read"
    ).toEqual(["asks", "comments", "events", "messages"]);
    expect(
      gets
        .map((sent) => new URL(sent.path, "http://dispatch"))
        .filter(
          (url) =>
            url.pathname === "/api/v1/me/replies" &&
            url.searchParams.get("state") === "unread" &&
            url.searchParams.has("limit")
        ),
      "the unread read carries a limit"
    ).toEqual([]);
    expect(
      threadReads(
        reads.requests,
        key,
        new Set((await allPages(key))[0]?.rows.map((record) => record.threadId))
      ),
      "a cold load of /threads reads a row's thread"
    ).toEqual([]);
    expect(
      reads.total(),
      `a cold load of /threads sent more than ${COLD_LOAD_AT_MOST} requests under /api/: ${JSON.stringify(reads.byRoute())}`
    ).toBeLessThanOrEqual(COLD_LOAD_AT_MOST);

    // The first row opened sends the read mark's read and its PUT, and the second its PUT alone.
    // Each window opens once the page has gone quiet, so the reads a mark that landed sets off -
    // its own refresh of the unread threads and its event's - fall before the next press.
    for (const [at, thread] of [seed.messages.first, seed.comments.remark].entries()) {
      await expectLoadSettled(reads);
      reads.reset();
      const marked = answerTo(cold, "PUT", `/api/v1/me/threads/${thread}/read`);
      await threadRow(cold, thread).getByRole("link").click();
      await marked;
      const window = [...reads.requests];
      expect(
        window.filter((sent) => sent.method === "PUT").map((sent) => sent.path),
        `opening row ${at + 1} sends other than its one read mark`
      ).toEqual([`/api/v1/me/threads/${thread}/read`]);
      expect(
        window.filter((sent) => sent.method === "GET").map(readKey),
        `opening row ${at + 1} sends other reads than the read mark's`
      ).toEqual(at === 0 ? [`GET /api/v1/me/replies?issue=${key}&limit=200`] : []);
      await cold.goBack();
      await expect(openedRecord(cold)).toHaveCount(0);
    }
    await cold.close();
  });
});

// =============================================================================================
// Row 14
// =============================================================================================

// SLOW(>=2s): the whole seed, then two loads of /threads addresses and one of today's Dashboard,
// every item drawn.
test("with the setting off, today's page draws for /threads what main draws", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const seed = await seedThreads();
  const context = await asUser(browser, READER);
  try {
    const page = await context.newPage();
    await page.setViewportSize(viewportAt(DESKTOP_WIDTH));
    for (const path of [threadsPath(seed.key), threadsPath(seed.key, seed.messages.first)]) {
      await page.goto(path);
      await expect(
        page.getByRole("heading", { level: 1 }),
        `today's page draws for ${path} other than main's page not found`
      ).toHaveText("Page not found");
      await expect(page.locator("[data-issue-page]"), `${path} draws the new page`).toHaveCount(0);
    }

    // Today's Dashboard links each thread item to the thread's own address.
    await page.goto(`/issues/${seed.key}/dashboard`);
    const panel = page.locator("#issue-dashboard-panel");
    await expect(panel, "today's page draws no Dashboard panel").toBeVisible();
    const waiting = panel.getByRole("region", { exact: true, name: "Waiting on you" });
    await expect(waiting).toBeVisible();
    const next = waiting.getByRole("button", { name: /^Show the next five/ });
    // Today's Dashboard renders Waiting on you only after its single replies read has settled.
    while ((await next.count()) > 0) {
      await next.click();
    }
    for (const [id, kind] of [
      [seed.messages.first, "message"],
      [seed.comments.remark, "comment"],
    ] as const) {
      const address = buildIssuePath({ id, key: seed.key, kind });
      await expect(
        waiting.getByTestId(`dashboard-item-thread-${id}`).locator(`a[href$="${address}"]`),
        `today's Dashboard does not link ${id} to its own address`
      ).not.toHaveCount(0);
    }
    await expect(
      panel.locator('a[href*="/threads/"]'),
      "today's Dashboard links to /threads/"
    ).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// =============================================================================================
// Row 15
// =============================================================================================

// SLOW(>=2s): the whole seed, then three cold loads of /threads/<id>, each read until it settles.
test("a cold load of /threads/<id> stays within 40 requests, for a record past All's first pages and for an id that names no record", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    const held = await allFirstPages(key);
    expect(
      held.has(seed.comments.oldestFiller) || held.has(seed.comments.post),
      "the seed puts the oldest comment threads on the comment route's first page"
    ).toBe(false);
    const nobody = "00000000-0000-4000-8000-000000000000";
    for (const load of [
      { id: seed.comments.oldestFiller, thread: null, what: "a record past All's first pages" },
      {
        id: seed.comments.postQuote,
        thread: seed.comments.post,
        what: "a reply that begins a record",
      },
      { id: nobody, thread: null, what: "an id that names no record" },
    ]) {
      const cold = await page.context().newPage();
      const reads = watchApiRequests(cold);
      await loadThreads(cold, key, load.id);
      if (load.id === nobody) {
        await expect(
          threadsMain(cold).getByText(NOT_FOUND_LINE, { exact: true }),
          `a cold load of ${load.what} draws no line for an address it cannot find`
        ).toBeVisible();
      } else {
        await expect(
          openedRecord(cold),
          `a cold load of ${load.what} opens no record`
        ).toHaveAttribute("data-thread-record", load.id);
      }
      await expectLoadSettled(reads);
      const paths = reads.requests
        .filter((sent) => sent.method === "GET")
        .map((sent) => new URL(sent.path, "http://dispatch").pathname);
      for (const lookup of [
        `/api/v1/asks/${load.id}`,
        `/api/v1/comments/${load.id}`,
        `/api/v1/messages/${load.id}`,
      ]) {
        expect(
          paths.filter((path) => path === lookup).length,
          `a cold load of ${load.what} sends ${lookup} other than once`
        ).toBe(1);
      }
      if (load.thread !== null) {
        expect(
          paths.filter((path) => path === `/api/v1/comments/${load.thread}`).length,
          `a cold load of ${load.what} reads its thread other than once`
        ).toBe(1);
      }
      expect(
        reads.total(),
        `a cold load of ${load.what} sent more than ${COLD_LOAD_AT_MOST} requests under /api/: ${JSON.stringify(reads.byRoute())}`
      ).toBeLessThanOrEqual(COLD_LOAD_AT_MOST);
      await cold.close();
    }
  });
});

// =============================================================================================
// Row 16
// =============================================================================================

// SLOW(>=2s): the whole seed with the parties, then at each width every choice drawn, a record
// opened and New thread opened, each read for the view's words.
test("every word that Threads draws outside the text of an entry is in the page's declared list @phone", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedWithParties, async (page, seed) => {
    const key = seed.key;
    const parties = await chooserParties(key);
    const names = new Set(
      [...seed.names, LIVE_PLANNER.title, ...parties.map((party) => party.title)].flatMap(wordsIn)
    );
    const hints = new Set(DISPATCH_WRITING_BOXES.map((row) => row.hint));
    const undeclared = (texts: readonly string[]) =>
      texts
        .filter((text) => !hints.has(text))
        .flatMap(wordsIn)
        .filter(
          (word) => !DECLARED_WORDS.includes(word) && !/^\d+$/.test(word) && !names.has(word)
        );
    for (const width of rowWidths()) {
      const at = `at ${width} px`;
      await page.setViewportSize(viewportAt(width));
      await loadThreads(page, key);
      for (const choice of CHOICE_ORDER) {
        await pickChoice(page, choice);
        await expectRowsDrawn(page, `${CHOICES[choice]} ${at}`);
        expect(
          undeclared(await viewWords(page)),
          `Threads under ${CHOICES[choice]} ${at} draws words the page's list does not declare`
        ).toEqual([]);
      }
      await pickChoice(page, "all");
      await openRow(page, key, seed.comments.remark);
      expect(
        undeclared(await viewWords(page)),
        `an opened record ${at} draws words the page's list does not declare`
      ).toEqual([]);
      if (width < SIDE_BY_SIDE_FROM) await backToList(page).click();
      await threadsList(page).getByRole("button", { exact: true, name: NEW_THREAD }).click();
      await expect(newThreadPanel(page)).toBeVisible();
      expect(
        undeclared(await viewWords(page)),
        `New thread ${at} draws words the page's list does not declare`
      ).toEqual([]);
    }
  });
});

// =============================================================================================
// Row 17
// =============================================================================================

// SLOW(>=2s): the whole seed, then two replies sent in Threads and read in Conversation, and one
// sent in Conversation and read in Threads, each read back from the server.
test("a thread is one record: a reply sent in Threads is drawn in Conversation, and one sent in Conversation is drawn in Threads", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedThreads, async (page, seed) => {
    const key = seed.key;
    await loadThreads(page, key);
    await expectRowsDrawn(page, "All");

    // In Threads: a reply to the reader's remark from its foot, and one to the reader's reply in
    // the open ask's thread from that entry's reply control.
    await openRow(page, key, seed.comments.remark);
    const toRemark = await sendFromBox(
      page,
      recordBox(page, "foot"),
      "A reply from Threads to the remark.",
      `/api/v1/issues/${key}/comments`
    );
    expect(
      (await getComment(seed.comments.remark)).replies?.find((one) => one.id === toRemark.id)
        ?.answers,
      "the reply to the remark is stored answering another entry"
    ).toBe(seed.comments.remark);
    await page.goBack();
    await openRow(page, key, seed.asks.open);
    await recordEntry(page, seed.asks.openReply)
      .getByRole("button", { exact: true, name: REPLY })
      .click();
    const inAsk = await sendFromBox(
      page,
      recordBox(page, seed.asks.openReply),
      "A reply from Threads in the open ask.",
      `/api/v1/issues/${key}/comments`
    );
    const askReply = (await getComment(inAsk.id)).comment;
    expect(askReply.ask_id, "the reply in the open ask is stored on another ask").toBe(
      seed.asks.open
    );
    expect(askReply.answers, "the reply in the open ask answers another entry").toBe(
      seed.asks.openReply
    );

    // In Conversation, each under its entry. The outline folds a thread's replies into its
    // parent's row, so every branch is opened before a reply's row is read.
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    for (const [reply, parent] of [
      [toRemark.id, seed.comments.remark],
      [inAsk.id, seed.asks.openReply],
    ] as const) {
      await expect(
        nodeRow(page, reply).first(),
        `Conversation draws no row of ${reply}`
      ).toBeVisible();
      await expect
        .poll(() => parentOfRow(page, reply), {
          message: `Conversation draws ${reply} under another entry than ${parent}`,
        })
        .toBe(parent);
    }

    // A reply sent in Conversation, drawn in the record in Threads.
    const words = "A reply from Conversation to the remark.";
    await replyFromRow(page, seed.comments.remark, words);
    await expect
      .poll(async () => (await listComments(key)).find((one) => one.body === words)?.id, {
        message: "the reply sent in Conversation is not stored",
      })
      .toBeDefined();
    const fromConversation = (await listComments(key)).find((one) => one.body === words)?.id ?? "";
    await loadThreads(page, key, seed.comments.remark);
    await expect(
      recordEntry(page, fromConversation),
      "the reply sent in Conversation is not drawn in the record in Threads"
    ).toBeVisible();
  });
});

test.skip("a thread is one record: a reply sent in Threads is drawn in Conversation, and one sent in Conversation is drawn in Threads: the margin @owed", {
  annotation: {
    description:
      "Owed on dispatch://INFRA-5706: the margin beside a node draws a record on text, through the one function of R8, only once that part is on the base, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedThreads, async (page, seed) => {
    await loadThreads(page, seed.key);
    await expectRowsDrawn(page, "All");
    await openRow(page, seed.key, seed.comments.text);
    const reply = await sendFromBox(
      page,
      recordBox(page, "foot"),
      "A reply from Threads on the spec's words.",
      `/api/v1/issues/${seed.key}/comments`
    );
    await gotoIssue(page, presetPath(seed.key, "spec"));
    await expect(
      issuePage(page)
        .locator("[data-text-margin]")
        .getByText("A reply from Threads on the spec's words."),
      `the margin beside the node draws no reply ${reply.id} sent in Threads`
    ).toBeVisible();
  });
});
