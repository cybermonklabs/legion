/** The shared search, sort and filter controls where proving them needs a server: seeded counts,
 *  writes, reloads, the keyboard, and the two pages a recording cannot answer - the Agents page,
 *  whose groups are a session's last-seen time against the clock, and the Delivery page, whose
 *  window is a range of dates. The rows that read only what a page draws over one fixed answer
 *  are in `table-controls.stub.e2e.ts`.
 */

import { IDEMPOTENCY_KEY_COMMITTED, ISSUE_STATUSES } from "@legion/contracts";
import type { APIResponse, Locator, Request, Route } from "@playwright/test";
import { REQUEST_DEADLINE_MS } from "../web/src/api/client";
import { CONTROL, NARROW_BELOW } from "../web/src/components/control-names";
import * as theme from "../web/src/theme/classes";
import {
  agentFilter,
  agentGroupToggle,
  agentRow,
  type FakeSession,
  getSentMessages,
  issueSearchIn,
  pickAgentFilter,
  plannerSession,
  reviewerSession,
  seedAgents,
  setInterests,
  setLiveSessions,
  shownAgentRows,
  toggleAgentGroup,
} from "./agents";
import {
  answerAsk,
  changeIssueStatus,
  claimIssue,
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createNamedVersion,
  createPriority,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getMyState,
  listIssueArtifacts,
  listOpenAsks,
  meInboxThreads,
  mintAgentToken,
  patchIssue,
  putAgentState,
  putArchitectureSource,
  putIssueState,
  putProjectDefaultRepository,
  replyToMessageDelivery,
  resolveAsk,
  resolveComment,
} from "./api";
import { recordClipboard } from "./clipboard";
import {
  FIXTURE_WINDOW,
  PIPELINE_AT,
  seedDeliveryFixture,
  seedMeasuresFixture,
  seedPipelineFixture,
  seedSessionGroupsFixture,
  TIMELINE_FIXTURE_QUERY,
} from "./delivery-seed";
import { seedDismissedThreads } from "./dismissed-seed";
import { connectedDot, documentEditor, placeCaret } from "./editor";
import {
  setCredentialGrants,
  setPendingCredentialRequests,
  setSignIns,
} from "./fake-broker-helpers";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  barAction,
  chooseShow,
  control,
  controlRow,
  displayChoice,
  displayTrigger,
  filterPicker,
  listOf,
  listTrigger,
  openFilterPanel,
  pickFilterOption,
  pickFromList,
  shutDisplayMenu,
} from "./filters";
import { focusHolder } from "./focus";
import { nextFrames, settledScrollTop } from "./frames";
import { drawer, peekTrigger, seedAsk } from "./inbox-drawer";
import { assigneeFilter, assigneeOptions, closeAssigneeFilter, pickAssignee } from "./inbox-filter";
import {
  askCard,
  askControls,
  hasReadingPane,
  inboxList,
  inboxPane,
  inboxRow,
  paneScroller,
  selectedRow,
  selectRow,
} from "./inbox-pane";
import { asSession, seedAgentThread, seedCommentThread, THREAD_SESSION } from "./inbox-threads";
import {
  bandHeadings,
  bandLabel,
  countsOf,
  drawnIds,
  lastActivity,
  seedEveryField,
  sortedIds,
  type ViewRow,
  valueLabel,
  valuesOf,
} from "./inbox-view";
import { focusThePage } from "./issue-page-g";
import { rowSnooze } from "./keyboard-inbox";
import { seedLegionDaemon } from "./legion-seed";
import { sql } from "./psql";
import { sessionIds } from "./session-ids";
import { expandedState, settledState } from "./settled";
import {
  closeControlPanel,
  drawnControlNames,
  expectBarDesign,
  expectNoGrowth,
  expectOneBarOn,
  INBOX_TOP,
  isPhoneProject,
  MORE_CHIPS,
  openDisplay,
  openIssuesTab,
  openSurface,
  pickInboxField,
  REAL_STACK_SURFACES,
  REMOVE_CHIP,
  resetScroll,
  SURFACE_CONTROLS,
  SURFACE_SESSION,
  seedSurfaces,
  setBoardLanes,
  topOf,
  widthsOf,
} from "./table-controls";
import { expect, type Page, test } from "./test";
import { assertPageFits } from "./unclipped";

/** The window the delivery fixture's two pull requests were merged in, with the list drawn. */
const DELIVERY_WINDOW = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

/** The seed the Agents and Delivery surfaces are read over.
 *
 *  The Delivery page's freshness rows are pinned to the verdict a reconcile pass reaches here,
 *  as `delivery.e2e.ts` and `review-seed.ts` pin them. The fake GitHub holds no App
 *  installation for acme/widgets, so every pass the server runs on its own five-minute tick
 *  fails with exactly the error written below, and the page's live socket carries that error to
 *  the PRs, Deploy runs and PR CI rows. Unpinned, a pass that landed between two reads moved
 *  the first row with no act of the row's own: at 1440x900 the Source freshness block grew from
 *  one line to two, 16 px to 36 px, between the narrowing's read and the panel's read, and the
 *  first row went from 681 px to 701 px. With the verdict written here, the page shows the same
 *  rows whether or not a pass has run. */
async function seedRealStackSurfaces(): Promise<{
  deliveryQuery: string;
  issue: string;
  project: string;
}> {
  const issue = await seedAgents();
  await seedDeliveryFixture();
  await sql(
    `UPDATE delivery_settings
       SET last_error = 'the GitHub App is not installed on acme/widgets'
     WHERE singleton`
  );
  return { deliveryQuery: DELIVERY_WINDOW, issue, project: "CORE" };
}

// SLOW(>=2s): two pages at each of this project's widths over two seeded fixtures, each read with
// no narrowing and with six.
// k-one-row on the Agents page and the Delivery page (`expectOneBarOn`, `table-controls.ts`): one
// bar of one line above each list, with nothing else between the title and the first row.
test("k-one-row: the Agents page and the Delivery page each draw one bar of one line above their list, its search box first and its placeholder whole, and nothing else between the title and the first row @phone", async ({
  page,
}, testInfo) => {
  test.setTimeout(180_000);
  const scene = await seedRealStackSurfaces();
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of REAL_STACK_SURFACES) await expectOneBarOn(page, name, scene, size, phone);
  }
});

// SLOW(>=2s): two pages over two seeded fixtures, each read at rest and under one, three or six
// narrowings.
// k-bar on the Agents page and the Delivery page (`expectBarDesign`, `table-controls.ts`): the
// search box, Filter and the chips, and no other control.
test("k-bar: the Agents page and the Delivery page draw the search box, Filter and the chips and no other control, quiet at rest, with Filter's count, its chips and its panel's list @phone", async ({
  page,
}, testInfo) => {
  test.setTimeout(180_000);
  const scene = await seedRealStackSurfaces();
  const phone = isPhoneProject(testInfo);
  const size = phone
    ? (page.viewportSize() ?? { height: 664, width: 390 })
    : { height: 800, width: 1280 };
  if (!phone) await page.setViewportSize(size);
  for (const name of REAL_STACK_SURFACES) await expectBarDesign(page, name, scene, size, phone);
});

// SLOW(>=2s): the same two pages, each narrowed, opened and - on the Agents page - selected at
// each of this project's widths.
// k-no-growth on the Agents page and the Delivery page (`expectNoGrowth`, `table-controls.ts`).
// The Delivery page's list marks no row: `PRList` hands `useDataTable` no `selection`
// (`features/delivery/PRList.tsx`), and a press on a row opens that one pull request's details,
// which marks nothing for an action.
test("k-no-growth: no narrowing, panel or selection on the Agents page or the Delivery page moves the first row, in the layout its bar's width gives @phone", async ({
  page,
}, testInfo) => {
  test.setTimeout(180_000);
  const scene = await seedRealStackSurfaces();
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of REAL_STACK_SURFACES) await expectNoGrowth(page, name, scene, size, phone);
  }
});

test("the Agents page and the Delivery page draw every shared control their design gives them, and no other", async ({
  page,
}) => {
  const scene = await seedRealStackSurfaces();
  await page.setViewportSize({ height: 900, width: 1440 });
  for (const name of REAL_STACK_SURFACES) {
    const surface = await openSurface(page, name, scene);
    // A chip is drawn only under a narrowing. The Agents page selects rows, for a broadcast, but
    // what a mark draws - the select-all header's count and the broadcast composer
    // (`features/agents/AgentsPage.tsx`) - carries no `data-control` and is no control the design
    // gives the page, so no row is marked here. The Delivery page's list selects none: `PRList`
    // hands `useDataTable` no `selection` (`features/delivery/PRList.tsx`).
    await surface.narrow();
    const expected = [...SURFACE_CONTROLS[name]].sort();
    for (const wanted of expected) {
      await expect(control(surface.root, wanted), `${name} draws no ${wanted}`).not.toHaveCount(0);
    }
    const drawn = await drawnControlNames(surface.root);
    const unexpected = drawn.filter((value) => !(expected as string[]).includes(value)).sort();
    expect(
      unexpected,
      `${name} draws ${unexpected.join(", ")}, which the design does not give it`
    ).toEqual([]);
  }
});

/** Twenty-two questions, so the list is longer than either window, with no credential request
 *  waiting and nothing selected. Each ask waits on Alice, who holds every issue. */
async function seedLongInbox(): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const priorities = [0, 1, 2, 3] as const;
  const urgencies = ["blocking", "high", "med", "low"] as const;
  for (let index = 0; index < 22; index++) {
    const issue = await createIssue({ project: "CORE", title: `Long inbox item ${index}` });
    await patchIssue(issue.key, { priority: priorities[index % 4] });
    await createAsk(
      issue.key,
      { question: `Item ${index}: what is the rule?`, urgency: urgencies[index % 4] },
      SURFACE_SESSION
    );
  }
}

/** `count` items of three Inbox kinds - questions, Agent rows and reply threads - all in Alice's
 *  Inbox, with no credential request waiting: twenty or more items of several kinds, so the list
 *  is longer than either window. Each question is a blocking ask on a P0 issue and waits on
 *  Alice, who holds every issue; an Agent row is a message to a live session and has no issue. */
async function seedInboxHeight(count: number): Promise<void> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < count; index++) {
    if (index % 6 === 4) {
      await seedAgentThread({ body: `Is mirror ${index} warm?`, replies: ["Warming it."] });
      continue;
    }
    const issue = await createIssue({
      priority: 0,
      project: "CORE",
      title: `Height check ${index}`,
    });
    if (index % 6 === 5) {
      await seedCommentThread(issue.key, "When is the rollout?", "Tomorrow.");
    } else {
      await createAsk(
        issue.key,
        { question: `Ready for item ${index}?`, urgency: "blocking" },
        SURFACE_SESSION
      );
    }
  }
}

// SLOW(>=2s): twenty-four seeded asks and threads, read at two widths through five states each.
// k-inbox-height: at 1440x900 and at 1280x800 the top of the Inbox's first item is at most 203 px
// from the top of the window with no narrowing on, with the filter panel open, with the display
// menu open, with three narrowings on and with one row selected. A panel and the selection bar
// draw over the page, and the chips stay in the bar's one line, so none takes room in the layout.
// Before it measures, the row reads that the seed draws its twenty-four items each with its kind
// mark, in three kinds at least.
test("k-inbox-height: the Inbox's first item starts within 203 px of the window at 1440x900 and 1280x800, whatever is narrowed, open or selected", async ({
  page,
}) => {
  const count = 24;
  await seedInboxHeight(count);
  const rows = page.locator("[data-inbox-row]");
  const firstRow = rows.first();
  const main = page.getByTestId("main-content");
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
  ]) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;
    const within = async (state: string) => {
      await resetScroll(page, firstRow);
      const top = await topOf(firstRow);
      expect(top, `${at}: ${state}, the first item starts ${top}px down`).toBeLessThanOrEqual(
        INBOX_TOP
      );
    };
    await page.goto("/");
    await expect(firstRow).toBeVisible();
    await expect(rows).toHaveCount(count);
    // The kinds the seed's items are drawn as, each by its row's own kind mark (`RowKindIcon`),
    // which every drawn row carries.
    const drawn = await rows.evaluateAll((nodes) =>
      nodes.map((node) => ({
        id: node.getAttribute("data-inbox-row") ?? "",
        kind: node.querySelector("[data-inbox-kind]")?.getAttribute("data-inbox-kind") ?? null,
      }))
    );
    expect(
      drawn.filter(({ kind }) => kind === null).map(({ id }) => id),
      `${at}: the Inbox rows drawn with no kind mark`
    ).toEqual([]);
    const kinds = [...new Set(drawn.flatMap(({ kind }) => kind ?? []))].sort();
    expect(
      kinds.length,
      `${at}: the Inbox draws its ${count} items in ${kinds.length} kinds (${kinds.join(", ")}), want three at least`
    ).toBeGreaterThanOrEqual(3);
    await resetScroll(page, firstRow);
    const lastBottom = await rows.last().evaluate((node) => node.getBoundingClientRect().bottom);
    expect(lastBottom, `${at}: the Inbox list is not longer than the window`).toBeGreaterThan(
      size.height
    );
    await within("with no narrowing on");

    await openFilterPanel(page, main);
    await within("with the filter panel open");
    await closeControlPanel(main);

    await openDisplay(page, main);
    await within("with the display menu open");
    await page.keyboard.press("Escape");

    // Three narrowings, opened in one address so the chips are the only thing that changed.
    await page.goto("/?priority=P0&urgency=blocking&type=question");
    await expect(firstRow).toBeVisible();
    // The chips stand in the bar's line, those it has no room for behind one `+N more`.
    await expect(control(controlRow(main), CONTROL.panelTrigger)).toHaveAccessibleName(
      "Filter · 3"
    );
    await within("with three narrowings on");

    await page.goto("/");
    await firstRow.getByRole("checkbox").first().click();
    await expect(control(main, CONTROL.selectionBar)).toBeVisible();
    await within("with one row selected");
  }
});

/** Nine narrowings over the long Inbox that still keep seventeen of its twenty-two items: three
 *  priorities, the three urgencies the seed pairs with them, two kinds and the one project. */
const NINE_NARROWINGS =
  "/?priority=P0,P1,P2&urgency=blocking,high,med&type=question,approval&project=CORE";

/** The boxes of the control row `row`, the row's own among them, that a reader can scroll
 *  sideways: each whose `overflow-x` is auto or scroll and that holds more than its own width,
 *  named by its tag, its `data-` attributes and the two widths. */
function sidewaysScrollers(row: Element): string[] {
  return [row, ...row.querySelectorAll("*")].flatMap((box) => {
    const { overflowX } = getComputedStyle(box);
    if (overflowX !== "auto" && overflowX !== "scroll") return [];
    if (box.scrollWidth <= box.clientWidth + 1) return [];
    const marks = [...box.attributes]
      .filter((mark) => mark.name.startsWith("data-"))
      .map((mark) => `[${mark.name}]`)
      .join("");
    return [`${box.tagName.toLowerCase()}${marks} (${box.scrollWidth}px in ${box.clientWidth}px)`];
  });
}

// SLOW(>=2s): twenty-two seeded questions, read at three widths with and without nine narrowings.
// R8: the chips stand in the bar's one line, which never grows. From 640 px of the bar's width
// the line draws the chips that fit and one `+N more` for the rest; below it no chip stands in
// the line, **Filter**'s badge says their number, and its panel lists every one.
test("however many narrowings are on, the Inbox's bar stays one line at every width, its chips behind +N more or, below 640 px, in its Filter panel", async ({
  page,
}) => {
  await seedLongInbox();
  const firstRow = page.locator("[data-inbox-row]").first();
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const trigger = control(row, CONTROL.panelTrigger);
  const readings: string[] = [];
  const over: string[] = [];
  const sideways: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;
    await page.goto("/");
    await expect(firstRow).toBeVisible();
    await resetScroll(page, firstRow);
    // The first item was waited for and the scroll reset above, so its top is the layout's.
    const plain = await topOf(firstRow);

    await page.goto(NINE_NARROWINGS);
    await expect(firstRow).toBeVisible();
    await expect(trigger, `${at}: Filter does not count nine`).toHaveAccessibleName("Filter · 9");
    // Filter's count was waited for above, so the bar is drawn, and its width follows the
    // viewport this turn set at its top.
    const width = await row.evaluate((node) => node.getBoundingClientRect().width);
    let split: string;
    if (width >= NARROW_BELOW) {
      // One chip per narrowing, so nine chips: the ones the line has room for, each whole, and
      // the rest in the panel its `+N more` opens.
      const chips = await chipsInLineAndPanel(row, 9, at);
      split = `${chips.line.length} chips in the line and ${chips.panel.length} behind +N more`;
    } else {
      // Below 640 px of the bar's width no chip stands in the line: the badge says their number,
      // and the panel lists every one with its remove control.
      await expect(trigger, `${at}: Filter's badge`).toHaveText("9");
      await expect(
        control(row, CONTROL.chip),
        `${at}: chips stand in the narrow bar's line`
      ).toHaveCount(0);
      const panel = await openFilterPanel(page, main);
      await expect(
        panel.getByRole("button", { name: REMOVE_CHIP }),
        `${at}: the Filter panel does not list the nine narrowings`
      ).toHaveCount(9);
      await closeControlPanel(main);
      split = "no chip in the line, nine in the Filter panel";
    }
    // Nothing in the bar scrolls sideways. The chips were read above from a settled line, so
    // the boxes read here are the narrowed bar's.
    const scrolling = await row.evaluate(sidewaysScrollers);
    sideways.push(...scrolling.map((box) => `${at}: ${box}`));
    await resetScroll(page, firstRow);
    // The nine narrowings were read and the scroll reset above, so this is the narrowed layout.
    const narrowed = await topOf(firstRow);
    const where = `${at}, a ${Math.round(width)}px bar: nine narrowings moved the first item from ${Math.round(plain)}px to ${Math.round(narrowed)}px`;
    readings.push(`${where}, with ${split}`);
    if (Math.abs(narrowed - plain) > 1) over.push(where);
  }
  await test.info().attach("first item with and without nine narrowings", {
    body: readings.join("\n"),
    contentType: "text/plain",
  });
  // R8: the bar never grows, so no narrowing moves the first item, within 1 px.
  expect(over, over.join("; ")).toEqual([]);
  // Nothing in the bar scrolls sideways, at any width: a chip the line has no room for is listed
  // behind its `+N more`, never scrolled to.
  expect(sideways, `${sideways.join("; ")} scroll sideways in the Inbox's bar`).toEqual([]);
});

/** The seven labels every issue of the Board's seed carries: the Board narrows by every label
 *  it names at once, so each issue holds them all and stays on the Board under all seven. */
const BOARD_LABELS = ["alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"];

// SLOW(>=2s): four seeded issues, read at three widths with and without nine narrowings.
// R8: the chips stand in the bar's one line, which never grows. From 640 px of the bar's width
// the line draws the chips that fit and one `+N more` for the rest; below it no chip stands in
// the line, **Filters**' badge says their number, and its panel lists every one.
test("however many narrowings are on, the Board's bar stays one line at every width, its chips behind +N more or, below 640 px, in its Filters panel", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  let first = "";
  for (const title of ["Rollout one", "Rollout two", "Rollout three", "Rollout four"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { labels: BOARD_LABELS, status: "todo" });
    await createAsk(issue.key, { question: `${title}: what is the rule?` }, SURFACE_SESSION);
    if (first === "") first = issue.key;
  }
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const trigger = control(row, CONTROL.panelTrigger);
  const firstCard = page.locator("[data-board-card]").first();
  // Seven labels, the search text and the needs-you flag: nine chips, and every issue kept.
  const nine = `/projects/CORE/issues?${BOARD_LABELS.map((label) => `label=${label}`).join("&")}&q=Rollout&needs-you=1`;
  const readings: string[] = [];
  const over: string[] = [];
  const sideways: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;
    // The Board is the stored view from here on, so the address below opens on it too.
    await openSurface(page, "project Board", { issue: first, project: "CORE" });
    await resetScroll(page, firstCard);
    // `openSurface` waited for the first card, and the scroll is reset, so this is the layout.
    const plain = await topOf(firstCard);

    await page.goto(nine);
    await expect(firstCard).toBeVisible();
    // The Board opens its Filter panel by itself while a narrowing is on, unless the reader shut
    // it before (`project.issue-filters`). That panel lies over the page and takes the pointer, so
    // it is shut first by its trigger, as a reader shuts it, and the chips are read with no panel
    // open. The panel is drawn over the page, so the layout stays the same.
    await closeControlPanel(main);
    await expect(
      page.getByRole("dialog", { name: "Filters" }),
      `${at}: the Board's Filter panel stayed open over the row`
    ).toHaveCount(0);
    await expect(trigger, `${at}: Filters does not count nine`).toHaveAccessibleName("Filters · 9");
    // Filters' count was waited for above, so the bar is drawn, and its width follows the
    // viewport this turn set at its top.
    const width = await row.evaluate((node) => node.getBoundingClientRect().width);
    let split: string;
    if (width >= NARROW_BELOW) {
      // One chip per narrowing, so nine chips: the ones the line has room for, each whole, and
      // the rest in the panel its `+N more` opens.
      const chips = await chipsInLineAndPanel(row, 9, at);
      split = `${chips.line.length} chips in the line and ${chips.panel.length} behind +N more`;
    } else {
      // Below 640 px of the bar's width no chip stands in the line: the badge says their number,
      // and the panel lists every one with its remove control.
      await expect(trigger, `${at}: Filters' badge`).toHaveText("9");
      await expect(
        control(row, CONTROL.chip),
        `${at}: chips stand in the narrow bar's line`
      ).toHaveCount(0);
      const panel = await openFilterPanel(page, main);
      await expect(
        panel.getByRole("button", { name: REMOVE_CHIP }),
        `${at}: the Filters panel does not list the nine narrowings`
      ).toHaveCount(9);
      await closeControlPanel(main);
      split = "no chip in the line, nine in the Filters panel";
    }
    // Nothing in the row scrolls sideways. The chips were read above from a settled line, so
    // the boxes read here are the narrowed row's.
    const scrolling = await row.evaluate(sidewaysScrollers);
    sideways.push(...scrolling.map((box) => `${at}: ${box}`));
    await resetScroll(page, firstCard);
    // The nine narrowings were read and the scroll reset above, so this is the narrowed layout.
    const narrowed = await topOf(firstCard);
    const where = `${at}, a ${Math.round(width)}px bar: nine narrowings moved the Board's first card from ${Math.round(plain)}px to ${Math.round(narrowed)}px`;
    readings.push(`${where}, with ${split}`);
    if (Math.abs(narrowed - plain) > 1) over.push(where);
  }
  await test.info().attach("first card with and without nine narrowings", {
    body: readings.join("\n"),
    contentType: "text/plain",
  });
  // R8: the bar never grows, so no narrowing moves the first card, within 1 px.
  expect(over, over.join("; ")).toEqual([]);
  // Nothing in the row scrolls sideways, at any width: a chip the line has no room for is listed
  // behind its `+N more`, never scrolled to.
  expect(sideways, `${sideways.join("; ")} scroll sideways in the Board's row`).toEqual([]);
});

/** The widths a reader's window takes, from the desktop down: R1's two, a tablet held upright,
 *  the `sm` breakpoint itself (640 px, where the row stops wrapping), and a phone. */
const READING_WIDTHS = [
  { height: 900, width: 1440 },
  { height: 800, width: 1280 },
  { height: 1024, width: 768 },
  { height: 800, width: 640 },
  { height: 664, width: 390 },
];

/** How much of `element` the reader can see: its box cut by every box above it that clips
 *  sideways, and by the window. */
function shownOf(element: Element): { left: number; right: number; width: number; shown: number } {
  const own = element.getBoundingClientRect();
  let left = 0;
  let right = window.innerWidth;
  for (let box = element.parentElement; box !== null; box = box.parentElement) {
    if (getComputedStyle(box).overflowX === "visible") continue;
    const clip = box.getBoundingClientRect();
    left = Math.max(left, clip.left);
    right = Math.min(right, clip.right);
  }
  return {
    left: own.left,
    right: own.right,
    width: own.width,
    shown: Math.max(0, Math.min(own.right, right) - Math.max(own.left, left)),
  };
}

// SLOW(>=2s): twenty-two seeded questions, the bar's keyboard order walked at five widths.
test("a chip of the Inbox's bar that has the keyboard focus is shown whole, with its whole name, at every width, and where no whole chip fits +N more stands alone and lists every chip", async ({
  page,
}) => {
  await seedLongInbox();
  const row = controlRow(page.getByTestId("main-content"));
  const more = row.getByRole("button", { name: MORE_CHIPS });
  /** What the reader sees of the focused control the instant after a key, or null once the focus
   *  is outside `within`: whether it is a chip, and for a chip its name, whether its words are
   *  cut short, and how much of it is shown (`shownOf`). */
  const focusedChip = (within: string) =>
    page.evaluate(
      ({ measure, selector }) => {
        const shown = new Function(`return (${measure})`)() as typeof shownOf;
        const focused = document.activeElement;
        if (!(focused instanceof HTMLElement)) return null;
        if (focused.closest(selector) === null) return null;
        if (focused.getAttribute("data-control") !== "chip") return { chip: false } as const;
        const name = focused.querySelector(".truncate");
        return {
          chip: true,
          label: focused.getAttribute("aria-label") ?? "",
          nameCut: name !== null && name.scrollWidth > name.clientWidth + 1,
          ...shown(focused),
        } as const;
      },
      { measure: shownOf.toString(), selector: within }
    );
  const cut: string[] = [];
  const walks: string[] = [];
  /** Notes a focused chip that the reader does not see whole, or whose name is cut short. */
  const look = (
    where: string,
    reading: { label: string; nameCut: boolean; shown: number; width: number }
  ) => {
    const holder = `${where}: "${reading.label}" has the focus`;
    if (reading.shown < reading.width - 1) {
      cut.push(
        `${holder} and ${Math.round(reading.shown)} of its ${Math.round(reading.width)}px are shown`
      );
    } else if (reading.nameCut) {
      cut.push(`${holder} and its name is cut short`);
    }
  };
  for (const size of READING_WIDTHS) {
    const where = `${size.width}x${size.height}`;
    await page.setViewportSize(size);
    await page.goto(NINE_NARROWINGS);
    // Below 640 px of the bar's width no chip stands in its line (R8).
    if (await chipsLeaveLine(row, where)) continue;
    // Nine chips, one per narrowing: the ones the chips line has room for, each whole, then the
    // ones its `+N more` lists in its panel - every one of them where the line has room for no
    // whole chip, so `+N more` stands alone (R8, `ControlRow`).
    const chips = await chipsInLineAndPanel(row, 9, where);
    const moreName = `${chips.panel.length} more ${chips.panel.length === 1 ? "filter" : "filters"}`;
    // Every chip the line draws shows its whole words, focused or not.
    const cutInLine = await row.evaluate((node) =>
      [...node.querySelectorAll('[data-chip-strip] [data-control="chip"]')]
        .filter((chip) => {
          const words = chip.querySelector(".truncate");
          return words !== null && words.scrollWidth > words.clientWidth + 1;
        })
        .map((chip) => chip.getAttribute("aria-label") ?? "")
    );
    for (const label of cutInLine) cut.push(`${where}: the line draws "${label}" cut short`);
    // The Tab key walks the chips line's chips in their order, then `+N more`, which stands for
    // the chips the line has no room for. The walk starts on the row's first control, its filter
    // trigger: the Inbox's search box stands in its header line, above the row (R8).
    await control(row, CONTROL.panelTrigger).first().focus();
    const walked: string[] = [];
    // The page's own controls and the display menu, then the line's chips, `+N more` and Clear
    // filters: twenty presses walk past the end of the row.
    for (let press = 0; press < 20; press++) {
      await page.keyboard.press("Tab");
      // The Tab moves the focus while the browser handles the press, and nothing in the row
      // scrolls or moves when a control takes it, so the instant after it is what the reader sees.
      const reading = await focusedChip('[data-control="control-row"]');
      if (reading === null) break;
      if (!reading.chip) continue;
      walked.push(reading.label);
      look(where, reading);
    }
    expect(walked, `${where}: the Tab key walks ${walked.join(", ")} in the chips line`).toEqual(
      chips.panel.length === 0 ? chips.line : [...chips.line, moreName]
    );
    // Opening `+N more` from the keyboard reaches the rest: its panel takes the focus onto its
    // first chip, Tab walks the panel's chips, and Tab past the last hands it back to `+N more`.
    const listed: string[] = [];
    if (chips.panel.length > 0) {
      await more.focus();
      await page.keyboard.press("Enter");
      const panel = page.getByRole("dialog", { exact: true, name: moreName });
      await expect(
        panel.getByRole("button").first(),
        `${where}: Enter on ${moreName} put the focus on no chip of its panel`
      ).toBeFocused();
      for (let press = 0; press < 12; press++) {
        // The panel took the focus above, and each Tab moves it at once, to the panel's next chip
        // or past the last onto `+N more`, so the instant after it is what the reader sees.
        const reading = await focusedChip('[role="dialog"]');
        if (reading === null) break;
        if (reading.chip) {
          listed.push(reading.label);
          look(where, reading);
        }
        await page.keyboard.press("Tab");
      }
      await expect(
        panel,
        `${where}: Tab past the last chip of ${moreName} left its panel open`
      ).toHaveCount(0);
    }
    expect(
      listed,
      `${where}: the Tab key walks ${listed.join(", ")} in the panel of ${moreName}`
    ).toEqual(chips.panel);
    walks.push(`${where}: ${walked.join(", ")}; in its panel ${listed.join(", ")}`);
  }
  await test.info().attach("the Inbox's chips walked by the Tab key", {
    body: walks.join("\n"),
    contentType: "text/plain",
  });
  // The row's own promise (components/ControlRow.tsx): the Tab key reaches the chips the line
  // draws in place, and the rest in the panel `+N more` opens, each shown whole. A reader who
  // cannot see the chip they are on cannot tell which narrowing Enter takes off, which the chips
  // showed whole before this change.
  expect(cut, cut.join("; ")).toEqual([]);
});

// SLOW(>=2s): twenty-two seeded questions, read at five widths.
test("Clear filters stays in view in the Inbox's bar however many narrowings are on, wherever the bar is 640 px wide or wider, and is not drawn below", async ({
  page,
}) => {
  await seedLongInbox();
  const row = controlRow(page.getByTestId("main-content"));
  const hidden: string[] = [];
  for (const size of READING_WIDTHS) {
    const where = `${size.width}x${size.height}`;
    await page.setViewportSize(size);
    await page.goto(NINE_NARROWINGS);
    const clear = row.getByRole("button", { exact: true, name: "Clear filters" });
    if (await chipsLeaveLine(row, where)) {
      await expect(clear, `${where}: Clear filters stands in the narrow bar`).toHaveCount(0);
      continue;
    }
    // One chip per narrowing, so nine chips: the ones the chips line has room for, each whole,
    // and the rest behind its `+N more`.
    await settledChipsLine(row, 9, where);
    await expect(clear).toHaveCount(1);
    // The nine chips and Clear filters were waited for above; how much of it shows is the reading.
    const reading = await clear.evaluate((button, measure) => {
      const shown = new Function(`return (${measure})`)() as typeof shownOf;
      return shown(button);
    }, shownOf.toString());
    if (reading.shown < reading.width - 1) {
      hidden.push(
        `${size.width}x${size.height}: ${Math.round(reading.shown)} of Clear filters' ${Math.round(reading.width)}px are in view`
      );
    }
  }
  // R8: Clear filters stays in the bar wherever the bar is 640 px wide or wider, and it is the
  // one press that takes nine narrowings off. A mouse's own wheel does not scroll a strip
  // sideways, so a control scrolled out of view is one a pointer reader cannot find. Below 640 px
  // no Clear filters is drawn: the reader takes the narrowings off in the Filter panel.
  expect(hidden, hidden.join("; ")).toEqual([]);
});

/** The names of the narrowings' chips `within` draws, `Remove <label> filter` as `filterChips`
 *  names each, in their order: the chips line's, or the panel's that `+N more` opens. */
function chipNames(within: Locator): Promise<string[]> {
  return within
    .getByRole("button", { name: /^Remove .* filter$/ })
    .evaluateAll((all) => all.map((chip) => chip.getAttribute("aria-label") ?? ""));
}

/** The controls of the chips line of the control row `row` that the reader does not see whole
 *  (`shownOf`, handed in as `measure`): each chip, `+N more` and Clear filters, by its name. */
function cutInChipsLine(row: Element, measure: string): string[] {
  const shown = new Function(`return (${measure})`)() as typeof shownOf;
  const line = row.lastElementChild;
  return [...(line?.querySelectorAll("a[href], button") ?? [])].flatMap((control) => {
    const reading = shown(control);
    if (reading.shown >= reading.width - 1) return [];
    const name = (control.getAttribute("aria-label") ?? control.textContent ?? "").trim();
    return [`"${name}" (${Math.round(reading.shown)} of ${Math.round(reading.width)}px shown)`];
  });
}

/** Sixteen narrowings over the long Inbox that keep all twenty-two of its items: its four
 *  priorities and four urgencies, four kinds, and four projects of which CORE holds them all (the
 *  address keeps three project keys no project has, EDGE, MESH and PIER, which match no row).
 *  Their chips are more than the chips line holds at any of `READING_WIDTHS`, so each width
 *  draws `+N more`. */
const SIXTEEN_NARROWINGS = `/?${[
  "priority=P0,P1,P2,P3",
  "urgency=blocking,high,med,low",
  "type=question,decision,approval,reply",
  "project=CORE,EDGE,MESH,PIER",
].join("&")}`;

/** The chips line of the control row `row` once it stands for the `count` narrowings that are on:
 *  the chips it draws, by name and in their order, and how many its `+N more` names (`left`). The
 *  line is read until its chips and that number add up to `count`. A chip the line draws in part
 *  is in neither place for the reader, so a control the line cuts fails the row by its name
 *  (`cutInChipsLine`). The panel of `+N more` is not opened. */
async function settledChipsLine(
  row: Locator,
  count: number,
  where: string
): Promise<{ left: number; line: string[] }> {
  const more = row.getByRole("button", { name: MORE_CHIPS });
  let left = 0;
  await expect
    .poll(
      async () => {
        const [line, named] = await Promise.all([
          chipNames(row),
          more.evaluateAll((all) => all.map((chip) => chip.getAttribute("aria-label") ?? "")),
        ]);
        left = Number.parseInt(named[0] ?? "0", 10);
        return line.length + left;
      },
      {
        message: `${where}: the chips line and its +N more stand for another number of chips than the ${count} narrowings that are on`,
      }
    )
    .toBe(count);
  // Every chip was drawn when the poll above passed, and the line is laid out again only when its
  // room or a chip changes size, so what it cuts is read from a settled line.
  const cut = await row.evaluate(cutInChipsLine, shownOf.toString());
  expect(cut, `${where}: the chips line cuts ${cut.join(", ")}`).toEqual([]);
  return { left, line: await chipNames(row) };
}

/** Whether the Inbox's bar `row`, standing for the narrowings that are on, is in its narrow
 *  layout, below 640 px of its own width, where no chip stands in its line (R8): read here, so a
 *  row that reads the chips line reads the wide layout alone, and the Filter panel, which lists
 *  every chip below 640 px, is read by `k-bar`. */
async function chipsLeaveLine(row: Locator, where: string): Promise<boolean> {
  await expect(control(row, CONTROL.panelTrigger)).toHaveAccessibleName(/^Filters? · \d+$/);
  // The trigger's count was waited for above, so the bar is drawn, and it takes its layout from
  // its own width before it first paints: the width read here is the one it chose by.
  const width = await row.evaluate((node) => node.getBoundingClientRect().width);
  if (width >= NARROW_BELOW) return false;
  await expect(
    control(row, CONTROL.chip),
    `${where}: chips stand in the narrow bar's line`
  ).toHaveCount(0);
  return true;
}

/** The chips of the `count` narrowings that are on in the control row `row`, by name and in their
 *  order: the ones its chips line draws, then the ones its `+N more` lists in the panel it opens.
 *  The line is read once it stands for them all (`settledChipsLine`), and the panel is then opened
 *  by a pointer and shut by Escape, which hands the focus back to `+N more`. Another count, or a
 *  chip drawn twice, fails the row. */
async function chipsInLineAndPanel(
  row: Locator,
  count: number,
  where: string
): Promise<{ line: string[]; panel: string[] }> {
  const page = row.page();
  const settled = await settledChipsLine(row, count, where);
  let { line } = settled;
  let listed: string[] = [];
  if (settled.left > 0) {
    await row.getByRole("button", { name: MORE_CHIPS }).click();
    const panel = page.getByRole("dialog", { name: MORE_CHIPS });
    await expect(panel, `${where}: +N more opened no panel named for it`).toBeVisible();
    // The panel lists the chips the line leaves out in the commit that draws it, which was waited
    // for above, so the line and the panel are read here as one split of the chips.
    line = await chipNames(row);
    listed = await chipNames(panel);
    await page.keyboard.press("Escape");
    await expect(panel, `${where}: Escape left the panel of +N more open`).toHaveCount(0);
  }
  const all = [...line, ...listed];
  expect(
    `${all.length} chips, ${new Set(all).size} of them named once`,
    `${where}: the line draws ${line.join(", ")}, and the panel of +N more lists [${listed.join(", ")}]`
  ).toBe(`${count} chips, ${count} of them named once`);
  return { line, panel: listed };
}

// SLOW(>=2s): twenty-two seeded questions, the bar's chips read at five widths with the panel of
// its +N more opened at each width its line draws chips, and a chip taken off from that panel.
test("the Inbox's chips that the line has no room for are listed behind one +N more, each drawn once, and a pointer takes one off from its panel, at every width its line draws chips", async ({
  page,
}) => {
  await seedLongInbox();
  const row = controlRow(page.getByTestId("main-content"));
  const more = row.getByRole("button", { name: MORE_CHIPS });
  const readings: string[] = [];
  for (const size of READING_WIDTHS) {
    const where = `${size.width}x${size.height}`;
    await page.setViewportSize(size);
    // Where every chip fits the line no `+N more` is drawn: one chip, which every line holds.
    // Below 640 px of the bar's width no chip stands in its line at all (R8).
    await page.goto("/?priority=P0");
    if (await chipsLeaveLine(row, where)) continue;
    await expect(
      row.getByRole("button", { exact: true, name: "Remove Priority: P0 filter" })
    ).toBeVisible();
    await expect(more, `${where}: one chip, which the line holds, draws +N more`).toHaveCount(0);

    await page.goto(SIXTEEN_NARROWINGS);
    await expect(more, `${where}: sixteen chips draw no +N more`).toBeVisible();
    // `+N more` was waited for above, and the row draws it in the commit that draws the chips
    // beside it, so its name and the line's chips are read from one settled line.
    const name = (await more.getAttribute("aria-label")) ?? "";
    const line = await chipNames(row);
    const cut = await row.evaluate(cutInChipsLine, shownOf.toString());
    expect(cut, `${where}: the chips line cuts ${cut.join(", ")}`).toEqual([]);
    // Opened by a pointer: a panel named as `+N more` is, listing every chip the line left out
    // and none it draws, with the focus on its first chip.
    await more.click();
    const panel = page.getByRole("dialog", { exact: true, name });
    await expect(panel, `${where}: ${name} opened no panel named for it`).toBeVisible();
    await expect(
      panel.getByRole("button").first(),
      `${where}: the panel of ${name} opened with the focus elsewhere`
    ).toBeFocused();
    const listed = await chipNames(panel);
    expect(
      `${line.length + listed.length} chips, ${new Set([...line, ...listed]).size} of them named once, ${listed.length} in the panel`,
      `${where}: the line draws ${line.join(", ")} and the panel of ${name} lists ${listed.join(", ")}`
    ).toBe(`16 chips, 16 of them named once, ${name.split(" ")[0]} in the panel`);
    // Escape closes it, and hands the focus back to `+N more`.
    await page.keyboard.press("Escape");
    await expect(panel, `${where}: Escape left the panel of ${name} open`).toHaveCount(0);
    await expect(more, `${where}: Escape did not hand the focus back to ${name}`).toBeFocused();
    readings.push(`${where}: ${line.length} chips in the line, and ${name} listing the rest`);
  }

  // And a chip the panel lists is one a pointer takes off there, in place.
  await page.setViewportSize({ height: 800, width: 1280 });
  await page.goto(SIXTEEN_NARROWINGS);
  await expect(more, "1280x800: sixteen chips draw no +N more").toBeVisible();
  await more.click();
  const panel = page.getByRole("dialog", { name: MORE_CHIPS });
  const last = panel.getByRole("button", { exact: true, name: "Remove Project: PIER filter" });
  await expect(
    last,
    "1280x800: the panel does not list the last chip, Project: PIER"
  ).toBeVisible();
  await last.click();
  await expect(last, "1280x800: pressing Project: PIER in the panel left it drawn").toHaveCount(0);
  await expect
    .poll(() => new URL(page.url()).searchParams.get("project"), {
      message: "1280x800: the address kept PIER in ?project=",
    })
    .toBe("CORE,EDGE,MESH");
  expect(
    new URL(page.url()).searchParams.get("priority"),
    "1280x800: the press took the priorities"
  ).toBe("P0,P1,P2,P3");
  // The panel still lists the chips before it, so the focus stays in the panel, on its last.
  await expect(
    panel.getByRole("button").last(),
    "1280x800: the focus left the panel when Project: PIER was taken off"
  ).toBeFocused();
  readings.push("1280x800: Project: PIER taken off from the panel of +N more");
  await test.info().attach("the row's chips and the panel of its +N more", {
    body: readings.join("\n"),
    contentType: "text/plain",
  });
});

/** Where the keyboard focus is, as the chips line reads it: on a chip, by its name, of the line or
 *  of the panel its `+N more` opens; on the row's filter trigger; on Clear filters; or anywhere
 *  else - the body included - with how much of the focused element the reader can see. */
async function focusInRow(
  row: Locator
): Promise<{ where: "chip" | "trigger" | "clear" | "elsewhere"; label: string; whole: boolean }> {
  return row.evaluate(
    (root, { measure, more }) => {
      const shown = new Function(`return (${measure})`)() as typeof shownOf;
      const focused = document.activeElement;
      // The panel `+N more` opens is drawn on the page's body, outside the row, and is named as
      // `+N more` is.
      const panel = focused?.closest('[role="dialog"]')?.getAttribute("aria-label") ?? "";
      const inPanel = new RegExp(more).test(panel);
      if (!(focused instanceof HTMLElement) || !(root.contains(focused) || inPanel)) {
        return { where: "elsewhere", label: focused?.tagName ?? "nothing", whole: false } as const;
      }
      const reading = shown(focused);
      const whole = reading.shown >= reading.width - 1;
      const label = focused.getAttribute("aria-label") ?? focused.textContent?.trim() ?? "";
      const control = focused.getAttribute("data-control");
      if (control === "chip") return { where: "chip", label, whole } as const;
      if (control === "panel-trigger") return { where: "trigger", label, whole } as const;
      if (label === "Clear filters") return { where: "clear", label, whole } as const;
      return { where: "elsewhere", label, whole } as const;
    },
    { measure: shownOf.toString(), more: MORE_CHIPS.source }
  );
}

/** The two pages of R2 whose chips this row presses: the Inbox with nine narrowings on, which
 *  draw nine chips, and the project List with a label, its search text and its three flags on,
 *  of which the three flags and the label draw chips, four (the label is its column's filter,
 *  whose chip the row draws whether its column shows or not, and the text is its box).
 *  `chips` is how many chips each address draws, in the chips line and in the panel of its
 *  `+N more` together. */
const CHIP_FOCUS_SURFACES = [
  { chips: 9, name: "Inbox", path: NINE_NARROWINGS },
  {
    chips: 4,
    name: "project List",
    path: "/projects/CORE/issues?label=frontend&q=Rollout&needs-you=1&unread=1&unclaimed=1",
  },
] as const;

// SLOW(>=2s): two pages at two widths, every chip of each taken off from the keyboard.
test("a chip taken off from the keyboard hands the focus to the next chip, then the previous, then the filter trigger, and Clear filters hands it to the trigger", async ({
  page,
}) => {
  await seedLongInbox();
  const scene = await seedSurfaces();
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const chips = control(row, CONTROL.chip);
  const more = row.getByRole("button", { name: MORE_CHIPS });
  const panel = page.getByRole("dialog", { name: MORE_CHIPS });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const surface of CHIP_FOCUS_SURFACES) {
      const where = `${surface.name} at ${size.width}x${size.height}`;
      // The List is the stored view from here on, so its address below opens on it.
      if (surface.name === "project List") await openSurface(page, "project List", scene);
      await page.goto(surface.path);
      // Below 640 px of the bar's width its chips leave the line and no Clear filters is drawn
      // (R8): each narrowing is taken off at the top of the Filter panel, which stays open. That
      // list hands the focus on as the line does (D26): to the chip now at the taken chip's place,
      // else to the one before it, and once no chip is left to the panel's first field - never onto
      // the page. The turns: the list's last chip by Enter, then its first by Space, then its first
      // by Enter until none is left.
      const barWidth = await row.evaluate((node) => node.getBoundingClientRect().width);
      if (barWidth < NARROW_BELOW) {
        const trigger = control(row, CONTROL.panelTrigger);
        await expect(
          trigger,
          `${where}: the trigger does not count the narrowings`
        ).toHaveAccessibleName(
          `${surface.name === "Inbox" ? "Filter" : "Filters"} · ${surface.chips}`
        );
        await expect(chips, `${where}: chips stand in the narrow bar's line`).toHaveCount(0);
        await expect(
          row.getByRole("button", { exact: true, name: "Clear filters" }),
          `${where}: Clear filters stands in the narrow bar`
        ).toHaveCount(0);
        await trigger.focus();
        await page.keyboard.press("Enter");
        const filters = page.getByRole("dialog", { name: /^Filters?$/ });
        const listed = filters.getByRole("button", { name: REMOVE_CHIP });
        await expect(
          listed,
          `${where}: the Filters panel does not list every narrowing`
        ).toHaveCount(surface.chips);
        // The panel's count of listed chips was waited for above, so the names read here are
        // those of the panel the press opened.
        let order = await chipNames(filters);
        for (let turn = 0; order.length > 0; turn++) {
          const key = turn === 1 ? "Space" : "Enter";
          const index = turn === 0 ? order.length - 1 : 0;
          const taken = order[index] ?? "";
          const chip = filters.getByRole("button", { exact: true, name: taken });
          await chip.focus();
          await page.keyboard.press(key);
          await expect(
            chip,
            `${where}: ${key} on "${taken}" in the Filters panel took it not off`
          ).toHaveCount(0);
          order = order.filter((name) => name !== taken);
          const next = order[index] ?? order[index - 1];
          const landed = () =>
            page.evaluate(() => {
              const focused = document.activeElement;
              return {
                label: (focused?.getAttribute("aria-label") ?? focused?.textContent ?? "").trim(),
                panel: focused?.closest('[role="dialog"]')?.getAttribute("aria-label") ?? "",
              };
            });
          const right = (focus: { label: string; panel: string }) =>
            /^Filters?$/.test(focus.panel) &&
            (next === undefined ? !REMOVE_CHIP.test(focus.label) : focus.label === next);
          const settled = await expect
            .poll(async () => right(await landed()), { timeout: 2_000 })
            .toBe(true)
            .then(() => true)
            .catch(() => false);
          if (!settled) {
            // The poll above gave up after two seconds, so this reads where the focus stood when
            // it did, for the line the failure records.
            const focus = await landed();
            wrong.push(
              `${where}: after ${key} took "${taken}" off in the Filters panel the focus is on "${focus.label}"${/^Filters?$/.test(focus.panel) ? "" : ", outside the panel"}, not on ${next === undefined ? "the panel's first field" : `"${next}"`}`
            );
          }
        }
        await expect(filters, `${where}: the Filters panel shut as its chips went`).toHaveCount(1);
        await page.keyboard.press("Escape");
        await expect(filters).toHaveCount(0);
        continue;
      }
      // Every chip the address's narrowings draw, in their order: the chips line's, then the
      // ones its `+N more` lists in its panel.
      const drawn = await chipsInLineAndPanel(row, surface.chips, where);
      let order = [...drawn.line, ...drawn.panel];
      // A chip taken off hands the focus to the chip now at its place among the chips the row
      // draws, which can be one that moved into the line from the panel, else to the chip before
      // it, else to the filter trigger. One taken off in a panel that stays hands it to the
      // panel's next chip there, else to the one before it, and the panel's last chip, which
      // takes `+N more` with it, hands it on from the place `+N more` stood at. The turns: the
      // line's last chip by Enter, whose place a chip of the panel may take; the line's first by
      // Space; then, while `+N more` stands for a chip, the panel's first, from inside the panel,
      // by Delete and then by Enter; then the line's first by Enter until no chip is left.
      for (let removal = 0; removal < 20 && order.length > 0; removal++) {
        const key = ["Enter", "Space", "Delete"][removal] ?? "Enter";
        // The line was read settled before the first turn, and each later turn follows a
        // removal that was waited for, so the line read here is the one the page settled on.
        const line = await chipNames(row);
        const fromPanel = removal >= 2 && order.length > line.length;
        const index = fromPanel ? line.length : removal === 0 ? line.length - 1 : 0;
        const taken = (fromPanel ? order[index] : line[index]) ?? "";
        const target = (fromPanel ? panel : row).getByRole("button", { exact: true, name: taken });
        const place = fromPanel ? " in the panel of +N more" : "";
        // The panel stays open while a chip taken off in it leaves it any, and else Enter on
        // `+N more` opens it from the keyboard, with the focus on its first chip.
        if (fromPanel && (await expandedState(`${where}: +N more`, more)) === "shut") {
          await more.focus();
          await page.keyboard.press("Enter");
        }
        await target.focus();
        await page.keyboard.press(key);
        const gone = page.getByRole("button", { exact: true, name: taken });
        const took = await expect(gone)
          .toHaveCount(0, { timeout: 5_000 })
          .then(() => true)
          .catch(() => false);
        let used = key;
        if (!took) {
          wrong.push(`${where}: ${key} on "${taken}"${place} took no chip off`);
          await target.focus();
          await page.keyboard.press("Enter");
          await expect(gone).toHaveCount(0);
          used = "Enter";
        }
        order = order.filter((name) => name !== taken);
        // The chip's going was waited for above, and `ControlRow` draws the line, shuts a panel it
        // emptied and hands the focus on in the commit that takes it off, so this read is settled.
        const lineAfter = await chipNames(row);
        // Read after the same commit as the line just above, so the panel is the one it settled on.
        const listedAfter = await chipNames(panel);
        // Each chip is drawn once, in its order: the line's from the first, then an open panel's.
        const shown = [...lineAfter, ...listedAfter];
        if (shown.join("\n") !== order.slice(0, shown.length).join("\n")) {
          wrong.push(
            `${where}: after ${used} took "${taken}" off${place} the row draws ${shown.join(", ")}, not the first of ${order.join(", ")}`
          );
        }
        const standing = fromPanel && listedAfter.length > 0;
        const among = standing ? listedAfter : lineAfter;
        const at = standing ? 0 : index;
        const next = among[at] ?? among[at - 1];
        const expected =
          next === undefined
            ? { where: "trigger", label: "the filter trigger" }
            : { where: "chip", label: next };
        // The chip's going was waited for above, and `ControlRow` hands the focus on in that
        // commit's layout effect, so the focus read is where the removal left it.
        const focus = await focusInRow(row);
        const right =
          focus.where === expected.where &&
          (expected.where === "trigger" || focus.label === expected.label);
        if (!right) {
          wrong.push(
            `${where}: after ${used} took "${taken}" off${place} the focus is on ${focus.where === "elsewhere" ? `${focus.label}, outside the row's controls` : `"${focus.label}"`}, not on ${expected.where === "chip" ? `"${expected.label}"` : expected.label}`
          );
        } else if (!focus.whole) {
          wrong.push(`${where}: after ${used} the focused "${focus.label}" is not shown whole`);
        }
      }
      if (order.length > 0) wrong.push(`${where}: 20 removals left ${order.join(", ")} on`);
      // Clear filters unmounts itself, so the focus it leaves goes to the filter trigger.
      await page.goto(surface.path);
      const clear = row.getByRole("button", { exact: true, name: "Clear filters" });
      await clear.focus();
      await page.keyboard.press("Enter");
      await expect(chips, `${where}: Clear filters left chips on`).toHaveCount(0);
      // The count above saw Clear filters' commit, and `ControlRow` hands the focus on in that
      // commit's layout effect, so the focus read is where the press left it.
      const focus = await focusInRow(row);
      if (focus.where !== "trigger") {
        wrong.push(
          `${where}: after Clear filters the focus is on ${focus.where === "elsewhere" ? `${focus.label}, outside the row's controls` : `"${focus.label}"`}, not on the filter trigger`
        );
      }
    }
  }
  // R6 keeps every chip's removal and Clear filters, and these are now this change's own
  // components: a removal that drops the focus onto the page sends a keyboard reader back to the
  // page's first link for the next chip, which cost 66 presses for 13 chips at 1440 px.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two pages at two widths, each page's selection bar taken away from the keyboard.
test("the selection bar gone from the keyboard hands the focus to the row marked last, else to the row now at its place, never the body, on the Inbox and the project List", async ({
  page,
}) => {
  await seedLongInbox();
  const scene = await seedSurfaces();
  // Forty more issues, so the List's one page holds rows its table does not draw at all: past
  // the window, and past the rows it draws beyond each end of the window.
  for (let index = 0; index < 40; index++) {
    await createIssue({ project: "CORE", title: `Far row ${index}` });
  }
  const main = page.getByTestId("main-content");
  const wrong: string[] = [];
  /** Where the keyboard focus is - the focused element's name, or the tag of the body - and
   *  whether the window and every box that clips it show the whole of it. */
  const focusNow = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { label: focused?.tagName ?? "nothing", whole: false };
      }
      const own = focused.getBoundingClientRect();
      let top = 0;
      let bottom = window.innerHeight;
      let left = 0;
      let right = window.innerWidth;
      for (let box = focused.parentElement; box !== null; box = box.parentElement) {
        const style = getComputedStyle(box);
        const clip = box.getBoundingClientRect();
        if (style.overflowY !== "visible") {
          top = Math.max(top, clip.top);
          bottom = Math.min(bottom, clip.bottom);
        }
        if (style.overflowX !== "visible") {
          left = Math.max(left, clip.left);
          right = Math.min(right, clip.right);
        }
      }
      const whole =
        own.top >= top - 1 &&
        own.bottom <= bottom + 1 &&
        own.left >= left - 1 &&
        own.right <= right + 1;
      const label = focused.getAttribute("aria-label") ?? focused.textContent?.trim() ?? "";
      return { label: label === "" ? focused.tagName : label, whole };
    });
  /** Records a focus that is not on `wanted`, or is on it out of the reader's sight. The bar
   *  hands the focus on once the commit that took it away has settled, which for a write that
   *  lands rather than a key pressed is a moment after the bar is seen gone; the focus is read
   *  until it is there, for the suite's expect timeout (`playwright.config.ts`). */
  const focusOn = async (where: string, after: string, wanted: string) => {
    const landed = await expect
      .poll(async () => (await focusNow()).label)
      .toBe(wanted)
      .then(() => true)
      .catch(() => false);
    // The poll above waited until the focus landed or its timeout ran out, so this is where the
    // focus settled.
    const focus = await focusNow();
    if (!landed) {
      wrong.push(`${where}: after ${after} the focus is on ${focus.label}, not on "${wanted}"`);
    } else if (!focus.whole) {
      wrong.push(`${where}: after ${after} the focused "${wanted}" is not in view`);
    }
  };
  /** Scrolls the list until `row` is above the window, so a focus handed back to it has to bring
   *  it into view: the box around it that scrolls, or the window where none does. */
  const scrollAbove = (row: Locator) =>
    row.evaluate((node) => {
      const by = node.getBoundingClientRect().bottom + 16;
      for (let box = node.parentElement; box !== null; box = box.parentElement) {
        const { overflowY } = getComputedStyle(box);
        if (
          (overflowY === "auto" || overflowY === "scroll") &&
          box.scrollHeight > box.clientHeight
        ) {
          box.scrollTop += by;
          return;
        }
      }
      window.scrollBy(0, by);
    });
  /** Marks a row from the keyboard, by Space on its checkbox. */
  const mark = async (box: Locator) => {
    await box.focus();
    await page.keyboard.press("Space");
    await expect(box).toBeChecked();
  };
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);

    // The Inbox, whose Escape is the keyboard's Clear while no row holds the focus.
    let where = `Inbox at ${size.width}x${size.height}`;
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.nth(3), `${where}: fewer than four rows`).toBeVisible();
    const inboxBar = page.getByRole("toolbar", { name: "Selected rows" });
    const inboxBox = (index: number) => rows.nth(index).getByRole("checkbox").first();
    const inboxClear = inboxBar.getByRole("button", { exact: true, name: "Clear selection" });
    // The third row and then the second: the row marked last, which is neither the list's first
    // nor the last marked in the list's order.
    await mark(inboxBox(2));
    await mark(inboxBox(1));
    await expect(inboxBar).toContainText("2 selected");
    await scrollAbove(rows.nth(1));
    await inboxClear.focus();
    await page.keyboard.press("Enter");
    await expect(inboxBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "Enter on Clear selection",
      (await inboxBox(1).getAttribute("aria-label")) ?? ""
    );
    await mark(inboxBox(0));
    await mark(inboxBox(3));
    await scrollAbove(rows.nth(3));
    await inboxClear.focus();
    await page.keyboard.press("Escape");
    await expect(inboxBar, `${where}: Escape from the bar left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "Escape from the bar's Clear selection",
      (await inboxBox(3).getAttribute("aria-label")) ?? ""
    );
    // A snooze picked in the bar folds every marked row into Later, the row marked last with
    // them, and the bar goes once the write lands: the focus goes, with no scroll, to the row that
    // moved up into the place of the row marked last, the second row now, which the reader sees.
    await mark(inboxBox(2));
    await mark(inboxBox(1));
    const picker = listTrigger(inboxBar, "Snooze selected asks");
    await picker.focus();
    await pickFromList(picker, "Later today");
    await expect(inboxBar, `${where}: the snooze left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "a snooze took the marked rows away",
      (await inboxBox(1).getAttribute("aria-label")) ?? ""
    );
    // The bar never held the focus, so it is not the bar's to move: on a control of the page's
    // own, Escape clears the selection and leaves the focus there.
    const trigger = control(controlRow(main), CONTROL.panelTrigger).first();
    await mark(inboxBox(1));
    await trigger.focus();
    await page.keyboard.press("Escape");
    await expect(inboxBar, `${where}: Escape on the filter trigger left the bar up`).toHaveCount(0);
    // The bar was waited gone above, and for a key press its hand-off lands with that commit, so
    // this reads where the Escape left the focus.
    if (!(await trigger.evaluate((node) => node === document.activeElement))) {
      // The check above found the focus off the trigger, and this names where it went instead.
      const focus = await focusNow();
      wrong.push(
        `${where}: Escape on the filter trigger moved the focus to ${focus.label}, off the trigger`
      );
    }
    // And on a row: `x` takes the one mark off the row in hand, the bar goes, and the focus stays
    // on the row. Escape is not this case: on a row it steps off the row first.
    await rows.nth(0).focus();
    await page.keyboard.press("x");
    await expect(inboxBox(0), `${where}: x marked another row than the focused one`).toBeChecked();
    await expect(inboxBar).toContainText("1 selected");
    await page.keyboard.press("x");
    await expect(inboxBar, `${where}: x on the only marked row left the bar up`).toHaveCount(0);
    // The bar was waited gone above, and for a key press its hand-off lands with that commit, so
    // this reads where the `x` left the focus.
    if (!(await rows.nth(0).evaluate((node) => node === document.activeElement))) {
      // The check above found the focus off the row, and this names where it went instead.
      const focus = await focusNow();
      wrong.push(`${where}: x on the focused row moved the focus to ${focus.label}, off the row`);
    }

    // The project List binds no Escape to clearing its selection (`issues-table-page.tsx`,
    // `IssuesTable.tsx`, `components/table-keys.ts`), so its bar goes by its button alone.
    where = `project List at ${size.width}x${size.height}`;
    await openSurface(page, "project List", scene);
    const issueRows = page.locator("[data-issue-row]");
    await expect(issueRows.nth(2), `${where}: fewer than three rows`).toBeVisible();
    const listBar = page.getByRole("toolbar", { name: "Selected issues" });
    const second = await issueRows.nth(1).getAttribute("data-issue-row");
    const third = await issueRows.nth(2).getAttribute("data-issue-row");
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${third}` }));
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${second}` }));
    await expect(listBar).toContainText("2 selected");
    await scrollAbove(issueRows.nth(1));
    await listBar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(listBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(where, "Enter on Clear selection", `Select ${second}`);
    // The row marked last, scrolled so far off that the table no longer draws it: the focus is
    // on its checkbox all the same, brought back into view, and not on a row that happens to be
    // drawn.
    const listBox = page.getByTestId("issues-table-scroll");
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${second}` }));
    await listBox.evaluate((node) => {
      node.scrollTop = node.scrollHeight;
    });
    await expect(
      page.locator(`[data-issue-row="${second}"]`),
      `${where}: the row marked is still drawn with the list scrolled to its end`
    ).toHaveCount(0);
    await listBar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(listBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "Enter on Clear selection with the row marked scrolled out of the drawn rows",
      `Select ${second}`
    );
    // A select-all marks no row of the reader's own. Two rows listed: the reader marks the
    // second, the header's checkbox takes in the first, and the focus goes back to the second.
    await page.goto("/projects/CORE/issues?q=Rollout");
    await expect(issueRows, `${where}: the search does not list two rows`).toHaveCount(2);
    const taken = await issueRows.nth(0).getAttribute("data-issue-row");
    const own = await issueRows.nth(1).getAttribute("data-issue-row");
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${own}` }));
    await mark(page.getByRole("checkbox", { exact: true, name: "Select every row on this page" }));
    await expect(
      page.getByRole("checkbox", { exact: true, name: `Select ${taken}` })
    ).toBeChecked();
    await expect(listBar).toContainText("2 selected");
    await listBar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(listBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(where, "Enter on Clear selection after a select-all", `Select ${own}`);
  }
  // A bar that drops the focus onto the page sends a keyboard reader back to its first link, from
  // which the rows they were marking are a walk through every control above them.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two widths, a thread dismissed from the bar and moved back to the Inbox from it.
test("a thread dismissed from the selection bar, or moved back to the Inbox from it, hands the focus to the row now at its place or the list's own stop, never the body", async ({
  page,
}) => {
  await seedLongInbox();
  // Two threads the reader wrote in, each with a reply under it: the Inbox lists each as a row a
  // reader can mark and dismiss from the bar.
  for (const title of ["Nightly mirror", "Weekly digest"]) {
    const issue = await createIssue({ project: "CORE", title });
    await seedCommentThread(issue.key, `${title}: when does it run?`, "It moved to 03:00.");
  }
  const main = page.getByTestId("main-content");
  const rows = main.locator("[data-inbox-row]");
  const bar = main.locator(`[data-control="${CONTROL.selectionBar}"]`);
  const threads = main.locator('[data-inbox-row^="reply:"]');
  const wrong: string[] = [];
  /** The focused element's name, or the tag of what holds the focus when it is no control. */
  const focused = () =>
    page.evaluate(() => {
      const element = document.activeElement;
      if (!(element instanceof HTMLElement) || element === document.body) {
        return element?.tagName ?? "nothing";
      }
      return element.getAttribute("aria-label") ?? element.textContent?.trim() ?? element.tagName;
    });
  /** The focus once it lands on `wanted` within two seconds, or wherever it is then. */
  const settled = async (wanted: string) =>
    expect
      .poll(focused, { timeout: 2_000 })
      .toBe(wanted)
      .then(() => wanted)
      .catch(() => focused());
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Inbox at ${size.width}x${size.height}`;

    // Dismiss: the marked thread leaves this view with the bar, so the focus goes, with no scroll,
    // to the checkbox of the row that moves up into its place - the row marked last is gone.
    await page.goto("/");
    await expect(threads, `${where}: no thread row is listed`).toHaveCount(2);
    const thread = await threads.first().getAttribute("data-inbox-row");
    await threads.first().getByRole("checkbox").first().check();
    // The row after the thread, which moves up into its place once the thread goes.
    const after = await main
      .locator(`[data-inbox-row="${thread}"]`)
      .locator("xpath=following::*[@data-inbox-row][1]")
      .getAttribute("data-inbox-row");
    const dismiss = bar.getByRole("button", { exact: true, name: "Dismiss selected threads" });
    await dismiss.focus();
    await page.keyboard.press("Enter");
    await expect(main.locator(`[data-inbox-row="${thread}"]`)).toHaveCount(0);
    await expect(bar, `${where}: Dismiss left the bar up`).toHaveCount(0);
    // The dismissed row and the bar were both waited gone above, and `getAttribute` waits for the
    // checkbox of the row that moved up to attach.
    const movedUp = await main
      .locator(`[data-inbox-row="${after}"]`)
      .getByRole("checkbox")
      .first()
      .getAttribute("aria-label");
    // The hand-off may land a moment after the bar goes, so the focus is read until it settles.
    const afterDismiss = await settled(movedUp ?? "");
    if (afterDismiss !== movedUp) {
      wrong.push(
        `${where}: after Enter on Dismiss the focus is on ${afterDismiss}, not on "${movedUp}"`
      );
    }

    // Move to Inbox, from the dismissed view: the thread leaves that view with the bar, and it is
    // the one thread the turn dismissed, so the view is left empty and the focus goes, with no
    // scroll, to the list column's own stop, named "Inbox list".
    await page.goto("/?show=dismissed");
    const back = main.locator(`[data-inbox-row="${thread}"]`);
    await expect(back, `${where}: the dismissed thread is not listed`).toHaveCount(1);
    await back.getByRole("checkbox").first().check();
    const restore = bar.getByRole("button", {
      exact: true,
      name: "Move to Inbox selected threads",
    });
    await restore.focus();
    await page.keyboard.press("Enter");
    await expect(back).toHaveCount(0);
    await expect(bar, `${where}: Move to Inbox left the bar up`).toHaveCount(0);
    await expect(rows, `${where}: the dismissed view still lists a thread`).toHaveCount(0);
    const target = "Inbox list";
    const afterRestore = await settled(target);
    if (afterRestore !== target) {
      wrong.push(
        `${where}: after Enter on Move to Inbox the focus is on ${afterRestore}, not on "${target}"`
      );
    }
  }
  // The rule the bar's hand-off is built to: a focus the bar held goes to the row marked last
  // while the list holds it, else to the row now at its place or the nearest row before it that
  // the reader can see, else to the list's own stop, and never to the body.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

/** What holds the keyboard focus, as a press's outcome reads: the body (the focus is lost), or
 *  the focused control's name and whether the window shows it. */
async function focusOutcome(
  page: Page
): Promise<{ lost: boolean; label: string; inView: boolean }> {
  return page.evaluate(() => {
    const element = document.activeElement;
    if (!(element instanceof HTMLElement) || element === document.body) {
      return { lost: true, label: element?.tagName ?? "nothing", inView: false };
    }
    const box = element.getBoundingClientRect();
    return {
      lost: false,
      label: (element.getAttribute("aria-label") ?? element.textContent ?? element.tagName)
        .trim()
        .slice(0, 60),
      inView:
        box.width > 0 &&
        box.height > 0 &&
        box.bottom > 0 &&
        box.top < window.innerHeight &&
        box.right > 0 &&
        box.left < window.innerWidth,
    };
  });
}

// SLOW(>=2s): three pages' own controls, each pressed from the keyboard at two widths.
test("the Agents page, the Delivery page and the project List keep the keyboard focus when a press takes its own control away, at 1440 and 390", async ({
  page,
}) => {
  await seedAgents();
  await setLiveSessions([plannerSession, reviewerSession]);
  await seedDeliveryFixture();
  const first = await createIssue({ project: "CORE", title: "Preserve the caller identity" });
  const second = await createIssue({ project: "CORE", title: "Document release verification" });
  const adopted = await createIssue({ project: "CORE", title: "Adopt the interrupted member" });
  const epic = await createIssue({ project: "CORE", title: "The interrupted action's epic" });
  // An issue whose title the new epic below repeats, so the server answers it as a possible
  // duplicate and the List draws its Create anyway and Cancel new epic notice.
  await createIssue({ project: "CORE", title: "Recover one created epic" });
  const main = page.getByTestId("main-content");
  const lost: string[] = [];
  /** Reads where the focus went once a press settles, and keeps the ones it lost. */
  const after = async (where: string, press: string) => {
    await expect
      .poll(async () => (await focusOutcome(page)).lost, { timeout: 2_000 })
      .toBe(false)
      .catch(() => undefined);
    // The poll above waited up to two seconds for the focus to land, so this is where it settled.
    const outcome = await focusOutcome(page);
    if (outcome.lost) lost.push(`${where}: after ${press} the focus is on ${outcome.label}`);
    else if (!outcome.inView) {
      lost.push(`${where}: after ${press} the focused "${outcome.label}" is out of view`);
    }
  };
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;

    // The Agents page's own Clear selection, in its select-all header: it goes with the selection.
    await page.goto("/agents");
    await main
      .getByRole("checkbox", { name: /^Select .* for broadcast$/ })
      .first()
      .check();
    const clear = main.getByRole("button", { exact: true, name: "Clear selection" });
    await clear.focus();
    await page.keyboard.press("Enter");
    await expect(clear).toHaveCount(0);
    await after(`Agents page at ${at}`, "Enter on Clear selection");

    // The Delivery page's Window control: a new preset changes the window under the list's
    // trigger the reader is on, which stays mounted (`WindowControl.tsx`). The seeded window is a
    // custom range, the last option: ArrowUp opens the list on it, ArrowUp again moves to the last
    // preset, and Enter picks that. The pick hands the focus back to the trigger, so `after`'s
    // poll ends at once; the read still sees the press's result, because `toHaveURL` waits for
    // the commit that wrote the new window.
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
    const windowList = listTrigger(main, "Window");
    await windowList.focus();
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("Enter");
    await expect(page).not.toHaveURL(/from=2024-06-01/);
    await after(`Delivery page at ${at}`, "Enter on the Window list's last preset");

    // The Delivery page's clear brush window: it goes with the brush.
    await page.goto(
      `/delivery?${DELIVERY_WINDOW}&mode=list&ws=2024-06-01T06%3A00%3A00Z&we=2024-06-01T18%3A00%3A00Z`
    );
    const brush = main.getByRole("button", { exact: true, name: "clear brush window" });
    await brush.focus();
    await page.keyboard.press("Enter");
    await expect(brush).toHaveCount(0);
    await after(`Delivery page at ${at}`, "Enter on clear brush window");

    // The project List's interrupted-action notice: Dismiss and Finish each take the notice
    // with them. The action is the record a stopped New epic leaves, here naming an open epic,
    // so Finish adopts its member and ends.
    /** The key the project List of CORE keeps its record under, its interrupted action in it. */
    const tableRecord = "dispatch.project-table.CORE";
    const interruptedNotice = async (id: string) => {
      await page.goto("/projects/CORE/issues");
      await page.evaluate(
        ({ action, key }) => {
          localStorage.setItem(
            key,
            JSON.stringify({ action: { ...action, startedAt: Date.now() } })
          );
        },
        {
          action: {
            id,
            keys: [adopted.key],
            kind: "newEpic",
            project: "CORE",
            target: epic.key,
            writes: {},
          },
          key: tableRecord,
        }
      );
      await page.reload();
      const notice = page.getByRole("region", { name: "Interrupted table action" });
      await expect(notice).toHaveCount(1);
      return notice;
    };
    for (const press of ["Dismiss", "Finish"]) {
      const notice = await interruptedNotice(`interrupted-${press}-${at}`);
      await notice.getByRole("button", { exact: true, name: press }).focus();
      await page.keyboard.press("Enter");
      await expect(notice).toHaveCount(0);
      await after(`project List at ${at}`, `Enter on the interrupted action's ${press}`);
      // Finish goes on writing after its notice goes, and a page load before its last write
      // stops it, so the List keeps the action and refuses the New epic below. The row waits
      // until the List has let go of the action's record.
      await expect
        .poll(
          () =>
            page.evaluate((key) => {
              const record: unknown = JSON.parse(localStorage.getItem(key) ?? "{}");
              return typeof record === "object" && record !== null && "action" in record
                ? (record.action ?? null)
                : null;
            }, tableRecord),
          { message: `project List at ${at}: the interrupted action's ${press} never ended` }
        )
        .toBeNull();
    }

    // The project List's possible-duplicate notice: Cancel new epic and Create anyway each take
    // the notice with them.
    for (const press of ["Cancel new epic", "Create anyway"]) {
      await page.goto("/projects/CORE/issues");
      await main.getByRole("checkbox", { exact: true, name: `Select ${first.key}` }).check();
      await main.getByRole("checkbox", { exact: true, name: `Select ${second.key}` }).check();
      await page.getByRole("button", { exact: true, name: "New epic from these" }).click();
      await page
        .getByRole("dialog", { name: "Name the new epic" })
        .getByRole("textbox", { name: "Name" })
        .fill("Recover one created epic");
      await page.getByRole("button", { exact: true, name: "Create epic" }).click();
      const button = main.getByRole("button", { exact: true, name: press });
      await expect(button, `project List at ${at}: no possible-duplicate notice`).toHaveCount(1);
      await button.focus();
      await page.keyboard.press("Enter");
      await expect(button).toHaveCount(0);
      await after(`project List at ${at}`, `Enter on ${press}`);
    }
  }
  // The architect's ruling ff1a60f5, on every control of the pages this change draws: a press
  // that takes its own focused control away hands the focus on, and never to the body.
  expect(lost, lost.join("; ")).toEqual([]);
});

/** The project List's narrowable columns and what shape each one's panel takes. No column of
 *  either table is a date or a number that narrows by two ends: Created and Updated narrow by
 *  the age bands they are grouped into, and Open asks by its own counts, so both are columns of
 *  values like the rest. */
const LIST_COLUMN_FILTERS: Record<string, "picks" | "text"> = {
  Assignee: "picks",
  "Claimed by": "picks",
  Created: "picks",
  "Created by": "picks",
  Epic: "picks",
  Key: "text",
  Labels: "picks",
  "Open asks": "picks",
  Priority: "picks",
  Queue: "picks",
  Status: "picks",
  Title: "text",
};

/** The Delivery list's, which are the five facets that are columns of that table. */
const DELIVERY_COLUMN_FILTERS: Record<string, "picks" | "text"> = {
  Author: "picks",
  Deployed: "picks",
  Issue: "picks",
  "Parent agent": "picks",
  Rework: "picks",
};

/** Reads one table's column filters: which headers carry one, that each sits inside its own
 *  header and at its right end, that a press opens exactly one panel, and what that panel
 *  holds. */
async function readColumnFilters(
  page: Page,
  table: string,
  expected: Record<string, "picks" | "text">
): Promise<void> {
  const headers = page.getByRole("columnheader");
  // Each caller waited for the table's first row, so its header row is drawn, and the assertion
  // below fails a read that missed a filter.
  const named = await headers.evaluateAll((cells) =>
    cells.map((cell) => ({
      control: cell.querySelector('[data-control="column-filter"]') !== null,
      // The header's own label, which is its sort button's text or its plain text.
      name: (cell.querySelector("button")?.textContent ?? cell.textContent ?? "")
        .replace(/[▲▼]/g, "")
        .trim(),
      right: cell.getBoundingClientRect().right,
      filterRight: cell.querySelector('[data-control="column-filter"]')?.getBoundingClientRect()
        .right,
    }))
  );
  const carrying = named.filter((cell) => cell.control).map((cell) => cell.name);
  expect(
    [...carrying].sort(),
    `${table}: the headers carrying a filter are ${carrying.join(", ")}`
  ).toEqual(Object.keys(expected).sort());

  for (const cell of named) {
    if (!cell.control) continue;
    expect(
      (cell.filterRight ?? 0) - cell.right,
      `${table}: the ${cell.name} filter is not at the right end of its own header`
    ).toBeGreaterThanOrEqual(-24);
    expect((cell.filterRight ?? 0) - cell.right).toBeLessThanOrEqual(1);
  }

  for (const [name, kind] of Object.entries(expected)) {
    // A column that already narrows reads `Filter Status: 4 picked`, which is the project
    // List's own opening view.
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${name}(: \\d+ picked)?$`),
    });
    await expect(trigger, `${table}: no filter control on ${name}`).toHaveCount(1);
    // A press on one header's funnel while another's panel is still shutting is taken by that
    // panel's own outside-dismiss, so the press is made until the trigger says it is open.
    await expect(async () => {
      if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
      expect(await trigger.getAttribute("aria-expanded")).toBe("true");
    }).toPass({ timeout: 10_000 });
    // One panel, and one only: a column of values opens a listbox over its options, a column
    // of free text a dialog holding the box to type in, and neither opens the other's.
    await expect(
      page.locator('[role="listbox"], [role="dialog"]'),
      `${table}: pressing the ${name} filter opened more than one panel`
    ).toHaveCount(1);
    if (kind === "text") {
      await expect(
        page.getByRole("dialog").getByRole("textbox", { name: `Filter ${name}` }),
        `${table}: the ${name} panel holds no text box`
      ).toHaveCount(1);
    } else {
      await expect(
        page.getByRole("combobox", { exact: true, name: `Search ${name}` }),
        `${table}: the ${name} panel holds no text box over its own options`
      ).toHaveCount(1);
    }
    await trigger.click();
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
  }

  // Nothing draws a filter field between a header and the first row: every column filter of
  // this table is inside a header cell.
  const stray = await page.evaluate(() =>
    [...document.querySelectorAll('[data-control="column-filter"]')]
      .filter((element) => element.closest("th") === null)
      .map((element) => element.getAttribute("aria-label") ?? "")
  );
  expect(stray, `${table}: ${stray.join(", ")} is drawn outside a column header`).toEqual([]);
}

// SLOW(>=2s): two tables, every narrowable column of each opened and read.
test("every column that narrows does it from its own header, and no page draws a filter field under one", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  const scene = await seedSurfaces();
  const list = await openSurface(page, "project List", scene);
  await expect(list.firstRow).toBeVisible();
  await readColumnFilters(page, "the project List", LIST_COLUMN_FILTERS);

  // Typing in a column of values leaves only the matching options.
  const status = page.getByRole("button", { name: /^Filter Status(: \d+ picked)?$/ });
  await status.click();
  // The filter's own options: every row of the table draws a priority `select`, whose options
  // carry the same role.
  const options = page.getByRole("listbox", { name: "Status options" }).getByRole("option");
  const before = await options.count();
  await page.getByRole("combobox", { exact: true, name: "Search Status" }).fill("tod");
  await expect(options).toHaveCount(1);
  expect(before, "the Status filter offered only the one option before any text").toBeGreaterThan(
    1
  );
  await expect(options.first()).toHaveAccessibleName(/^Todo/);
  await status.click();

  await seedDeliveryFixture();
  const delivery = await openSurface(page, "Delivery page", {
    ...scene,
    deliveryQuery: DELIVERY_WINDOW,
  });
  await expect(delivery.firstRow).toBeVisible();
  await readColumnFilters(page, "the Delivery list", DELIVERY_COLUMN_FILTERS);
});

// SLOW(>=2s): four pages, each opened and its one trigger read.
test("every narrowing that is no column opens from its page's one filter trigger", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedAgents();
  const scene = await seedSurfaces();
  // The session-groups fixture rather than the timeline one: its pull requests name sessions,
  // and the Session facet draws nothing while no pull request in the window names one
  // (`Facet`, `features/delivery/DeliveryPage.tsx`). Its merges are in the last seven days,
  // which is the window the page opens on, so the Delivery surface below names none.
  await seedSessionGroupsFixture();

  const pages: { controls: string[]; name: Parameters<typeof openSurface>[1] }[] = [
    {
      controls: [
        "Filter by priority",
        "Filter by urgency",
        "Filter by type",
        "Filter by project",
        "Filter by assignee",
      ],
      name: "Inbox",
    },
    { controls: ["Labels", "Needs you", "Unread", "Unclaimed"], name: "project Board" },
    { controls: ["Filter by group", "Filter by machine", "Filter by role"], name: "Agents page" },
    { controls: ["Repository", "Session", "Priority", "Component"], name: "Delivery page" },
  ];
  for (const { controls, name } of pages) {
    const surface = await openSurface(page, name, { ...scene, deliveryQuery: "" });
    const triggers = control(controlRow(surface.root), CONTROL.panelTrigger);
    await expect(triggers, `${name} draws more than one filter trigger in its row`).toHaveCount(1);
    const panel = await openFilterPanel(page, surface.root);
    for (const label of controls) {
      // A narrowing is named either on its own control or beside it, which is what the
      // Delivery page's facets do.
      await expect(
        panel
          .getByRole("button", { name: new RegExp(`^${label}`) })
          .or(panel.getByText(label, { exact: true })),
        `${name}: its panel holds no ${label}`
      ).not.toHaveCount(0);
    }
    await closeControlPanel(surface.root);
  }
});

test("selecting a row moves no row, and the bar that appears is fixed over the list", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const scene = await seedSurfaces();
  for (const name of ["Inbox", "project List"] as const) {
    const surface = await openSurface(page, name, scene);
    await resetScroll(page, surface.firstRow);
    const before = await topOf(surface.firstRow);
    await surface.select?.();
    await resetScroll(page, surface.firstRow);
    const after = await topOf(surface.firstRow);
    expect(
      after,
      `${name}: selecting a row moved the first row from ${Math.round(before)}px to ${Math.round(after)}px`
    ).toBeCloseTo(before, 0);
    const bar = control(surface.root, CONTROL.selectionBar);
    await expect(bar, `${name}: no selection bar`).toBeVisible();
    await expect(bar, `${name}: the bar does not carry the count`).toContainText("1 selected");
    expect(
      await bar.evaluate((node) => getComputedStyle(node).position),
      `${name}: the selection bar is not fixed over the list`
    ).toBe("fixed");
  }
});

// SLOW(>=2s): eleven seeded rows read through each of the Inbox's own parameters.
test("each page's own parameters open the rows they name", async ({ page }) => {
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const idsWhere = (match: (row: (typeof rows)[number]) => boolean) =>
    rows.filter(match).map((row) => row.id);
  const expectRows = async (address: string, expected: readonly string[], why: string) => {
    await page.goto(address);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(expected.length);
    expect([...(await drawnIds(page))].sort(), why).toEqual([...expected].sort());
  };

  // The Inbox: the four comma lists, the assignee, the text, the section and the agent.
  await expectRows(
    "/?assignee=everyone&type=question",
    idsWhere((row) => row.type === "question"),
    "?type= lists the questions"
  );
  await expectRows(
    "/?assignee=everyone&priority=P0",
    idsWhere((row) => row.priority === "P0"),
    "?priority= lists the P0 rows"
  );
  await expectRows(
    "/?assignee=everyone&urgency=blocking",
    idsWhere((row) => row.urgency === "blocking"),
    "?urgency= lists the blocking rows"
  );
  await expectRows(
    "/?assignee=everyone&project=MIRROR",
    idsWhere((row) => row.project === "MIRROR"),
    "?project= lists the Mirror rows"
  );
  await expectRows(
    "/?assignee=bob",
    idsWhere((row) => row.assignee === "bob"),
    "?assignee= lists one person's rows"
  );
  await expectRows(
    "/?assignee=everyone&q=Lease handover",
    idsWhere((row) => row.id === rows[2]?.id),
    "?q= lists the one row whose question holds those words"
  );
  // `?show=dismissed` is the reader's other list, which nothing has been dismissed into. The
  // reading is the **Show** control's own, not the empty list's: a list that has not drawn yet
  // is empty too, so a count of nothing would pass whatever the address said.
  await page.goto("/?assignee=everyone&show=dismissed");
  const showing = await openDisplay(page, page.getByTestId("main-content"));
  await expect(
    showing.getByRole("group", { name: "Show" }).locator('input[value="dismissed"]'),
    "?show=dismissed does not put the Show control on the dismissed list"
  ).toBeChecked();
  await page.keyboard.press("Escape");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(0);
  // `?sort=` and `?group=` keep every row and change its place.
  await page.goto("/?assignee=everyone&sort=priority&group=none");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  expect(await drawnIds(page), "?sort=priority orders by priority").toEqual(
    sortedIds(rows, "priority", at)
  );
  await page.goto("/?assignee=everyone&group=type");
  // A band heading reads its plural and its count in brackets.
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  expect(await bandHeadings(page), "?group=type bands by type").toEqual(
    valuesOf(rows, "type").map((value) => {
      const plural = {
        agent: "Agent messages",
        approval: "Approvals",
        decision: "Decisions",
        question: "Questions",
        reply: "Threads",
      }[value];
      return `${plural} (${rows.filter((row) => row.type === value).length})`;
    })
  );
  // `?section=needs-you` is the asks waiting on the reader; `?agent=` one session's asks.
  await page.goto("/?section=needs-you");
  // Read the list once it has drawn: an address read before the first row lands is an empty
  // list whatever the address said, which is how CI run 43270255462 read 0 rows here while a
  // faster machine read them all.
  await expect(
    page.locator("[data-inbox-row]").first(),
    "?section=needs-you lists the asks waiting on the reader"
  ).toBeVisible();
  const waiting = await drawnIds(page);
  expect(
    waiting,
    "?section=needs-you drops the one question handed back to its agent"
  ).not.toContain(rows[2]?.id);
  expect(waiting.length, "?section=needs-you lists the asks waiting on the reader").toBeGreaterThan(
    0
  );
  await page.goto(`/?assignee=everyone&agent=${encodeURIComponent("e2e-inbox-view")}`);
  await expect(page.locator("[data-inbox-row]")).toHaveCount(0);
});

test("the project List, the Board and the Agents page open the rows their parameters name", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedSurfaces();
  const listed = async (address: string): Promise<string[]> => {
    await page.goto(address);
    await expect(page.getByRole("table", { name: "Project issues table" })).toBeVisible();
    return page
      .locator("[data-issue-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-issue-row") ?? ""));
  };
  // Six issues, CORE-1 to CORE-6, every one of them `todo`, holding one open ask, the odd ones
  // labelled `frontend` and the even ones `backend`.
  const every = ["CORE-1", "CORE-2", "CORE-3", "CORE-4", "CORE-5", "CORE-6"];
  expect(await listed("/projects/CORE/issues?sort=-key"), "?sort= orders by a column").toEqual(
    [...every].reverse()
  );
  expect(await listed("/projects/CORE/issues?q=Rollout"), "?q= searches the rows").toEqual([
    "CORE-1",
    "CORE-2",
  ]);
  expect(await listed("/projects/CORE/issues?label=frontend"), "?label= picks a label").toEqual([
    "CORE-1",
    "CORE-3",
    "CORE-5",
  ]);
  expect(await listed("/projects/CORE/issues?status=todo"), "?status= picks a status").toEqual(
    every
  );
  expect(
    await listed("/projects/CORE/issues?f.assignee=alice"),
    "?f.<column>= picks another column's value"
  ).toEqual(every);
  expect(await listed("/projects/CORE/issues?needs-you=1"), "?needs-you=1 keeps the asked").toEqual(
    every
  );
  expect(await listed("/projects/CORE/issues?unread=1"), "?unread=1 keeps the unread").toEqual(
    every
  );
  expect(
    await listed("/projects/CORE/issues?unclaimed=1"),
    "?unclaimed=1 keeps the unclaimed"
  ).toEqual(every);
  expect(
    await listed("/projects/CORE/issues?nofilter=1&size=10&page=1"),
    "?nofilter=, ?size= and ?page= page the whole list"
  ).toEqual(every);
  await page.goto("/projects/CORE/issues?group=status");
  await expect(
    page.getByRole("button", { name: /^Todo/ }).first(),
    "?group= bands the table"
  ).toBeVisible();

  // The Board reads the same five of them.
  const carded = async (address: string): Promise<string[]> => {
    await page.goto(address);
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    return page
      .locator("[data-board-card]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-board-card") ?? ""));
  };
  await openIssuesTab(page, "CORE", "Board");
  expect(await carded("/projects/CORE/issues?label=backend"), "the Board's ?label=").toEqual([
    "CORE-2",
    "CORE-4",
    "CORE-6",
  ]);
  expect(await carded("/projects/CORE/issues?q=Ledger"), "the Board's ?q=").toEqual(["CORE-3"]);
  expect(await carded("/projects/CORE/issues?needs-you=1"), "the Board's ?needs-you=").toEqual(
    every
  );
  expect(await carded("/projects/CORE/issues?unread=1"), "the Board's ?unread=").toEqual(every);
  expect(await carded("/projects/CORE/issues?unclaimed=1"), "the Board's ?unclaimed=").toEqual(
    every
  );

  // The Agents page's own two.
  await setLiveSessions([plannerSession, reviewerSession]);
  await page.goto("/agents?q=Reviewer");
  await expect(page.locator(`[data-agent-row="${sessionIds.reviewer}"]`)).toBeVisible();
  await expect(page.locator(`[data-agent-row="${sessionIds.planner}"]`)).toHaveCount(0);
  await page.goto("/agents?machine=box-1");
  await expect(page.locator(`[data-agent-row="${sessionIds.reviewer}"]`)).toBeVisible();
  await expect(page.locator(`[data-agent-row="${sessionIds.planner}"]`)).toBeVisible();
  await page.goto("/agents?machine=nowhere");
  await expect(page.locator("[data-agent-row]")).toHaveCount(0);
});

test("the Delivery page's facet parameters open the pull requests they name", async ({ page }) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedDeliveryFixture();
  const listed = async (query: string): Promise<string[]> => {
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list&${query}`);
    await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
    await expect(page.locator("table")).not.toHaveCount(0);
    return page
      .locator("[data-pr-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-pr-row") ?? ""));
  };
  // Two pull requests in the window: #1, shipped and attributed to ACME-1, and #2, a rework
  // with no issue. The page spells each facet in the address with its own key, not the
  // server's: `parentAgent`, never `parent_agent`.
  // The list opens newest merge first, so #2, merged at 03:00, is above #1, merged at 01:00.
  const both = ["acme/widgets#2", "acme/widgets#1"];
  expect(await listed("repo=acme%2Fwidgets"), "?repo=").toEqual(both);
  expect(await listed("author=octocat"), "?author=").toEqual(both);
  expect(await listed("issue=ACME-1"), "?issue=").toEqual(["acme/widgets#1"]);
  expect(await listed("rework=rework"), "?rework=").toEqual(["acme/widgets#2"]);
  expect(await listed("deployed=deployed"), "?deployed=").toEqual(["acme/widgets#1"]);
  expect(await listed("parentAgent=__no_session__"), "?parentAgent=").toEqual(both);
  // The fixture's two pull requests name no session, and the server counts none under the
  // placeholder, so this parameter narrows the list to nothing where `parentAgent` keeps both:
  // the two are read apart.
  expect(await listed("session=__no_session__"), "?session=").toEqual([]);
  // ACME-1, the issue #1 is attributed to, is the window's one P0; #2 links no issue at all.
  expect(await listed("priority=0"), "?priority=").toEqual(["acme/widgets#1"]);
  const knownChips = await chipNames(page.locator("main"));
  expect(knownChips, "?priority=: the known value's chip").not.toEqual([]);
  // A value that names no value of its facet is kept, as at the merge base (R5): Dispatch
  // selects no pull request by it, so beside a known value the list is the one the known value
  // alone lists, and alone it lists none. The control row draws its chip by its own word, so the
  // reader can take it off.
  expect(await listed("priority=P9&priority=0"), "?priority= beside an unknown value").toEqual([
    "acme/widgets#1",
  ]);
  expect(
    await chipNames(page.locator("main")),
    "?priority= beside an unknown value: the control row's chips"
  ).toEqual(["Remove Priority: P9 filter", ...knownChips]);
  for (const [query, chip] of [
    ["priority=P9", "Remove Priority: P9 filter"],
    ["rework=constructor", "Remove Rework: constructor filter"],
    ["deployed=bogus", "Remove Deployed: bogus filter"],
  ] as const) {
    // The count is the answer's for the address (`counted`), so the rows read after it are
    // Dispatch's answer, not a table drawn before one came.
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list&${query}`);
    await expect(page.getByText("0 PRs in current filter/window"), `?${query} alone`).toBeVisible();
    await expect(page.locator("[data-pr-row]"), `?${query} alone`).toHaveCount(0);
    expect(await chipNames(page.locator("main")), `?${query} alone: its chip`).toEqual([chip]);
  }
  // ACME-1 carries no component, and #2 links no issue at all, so the server counts only the
  // attributed pull request under the placeholder.
  expect(await listed("component=__no_component__"), "?component=").toEqual(["acme/widgets#1"]);
  // And the window and the mode: a preset window outside the fixture's day lists nothing, and
  // without `mode=list` there is no table at all.
  await page.goto(`/delivery?window=7d&mode=list`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await expect(page.locator("[data-pr-row]")).toHaveCount(0);
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await expect(page.locator("[data-pr-row]")).toHaveCount(0);
});

// SLOW(>=2s): four parameters, each set, read in the address and found again after a reload.
test("the four parameters this change adds ride in the address and survive a reload, and the drawer writes none", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedAgents();
  const scene = await seedSurfaces();
  await seedDeliveryFixture();

  // The Agents page's `?group=` and `?role=`, neither in the address until its narrowing is on.
  await page.goto("/agents");
  await expect(page).not.toHaveURL(/[?&]group=/);
  await expect(page).not.toHaveURL(/[?&]role=/);
  const agents = page.getByTestId("main-content");
  await openFilterPanel(page, agents);
  for (const [name, option] of [
    ["group", "Needs you"],
    ["role", "planner"],
  ] as const) {
    const trigger = page.getByRole("button", { name: `Filter by ${name}` });
    if ((await expandedState(`the ${name} filter`, trigger)) === "shut") await trigger.click();
    await page.getByRole("option", { exact: true, name: option }).click();
    await trigger.click();
  }
  await closeControlPanel(agents);
  await expect(page).toHaveURL(/[?&]group=/);
  await expect(page).toHaveURL(/[?&]role=planner/);
  const agentsAddress = new URL(page.url()).search;
  await page.reload();
  expect(new URL(page.url()).search, "the Agents page's narrowing is gone after a reload").toBe(
    agentsAddress
  );
  await expect(control(controlRow(agents), CONTROL.chip)).not.toHaveCount(0);

  // The Artifacts tab's `?q=`.
  await page.goto(`/issues/${scene.issue}/artifacts`);
  await expect(page).not.toHaveURL(/[?&]q=/);
  const tab = page.getByTestId("main-content");
  await control(controlRow(tab), CONTROL.searchBox).fill("plan");
  await expect(page).toHaveURL(/[?&]q=plan/);
  await page.reload();
  await expect(page, "the Artifacts tab's narrowing is gone after a reload").toHaveURL(
    /[?&]q=plan/
  );
  await expect(control(controlRow(tab), CONTROL.searchBox)).toHaveValue("plan");
  await expect(page.locator('[data-testid^="artifact-"]')).toHaveCount(1);

  // The Delivery page's `?sort=`, absent until a header is pressed.
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  await expect(page.locator("[data-pr-row]").first()).toBeVisible();
  await expect(page).not.toHaveURL(/[?&]sort=/);
  await page.getByRole("button", { exact: true, name: "Author" }).click();
  await expect(page).toHaveURL(/[?&]sort=author/);
  await page.reload();
  await expect(page, "the Delivery page's sort is gone after a reload").toHaveURL(
    /[?&]sort=author/
  );

  // The drawer's own state is its own: narrowing it over the project List writes no parameter.
  // The narrowing takes in the drawer: its bar takes the narrow layout in every window 768 px
  // wide or wider, so no chip stands in its line, Filter's badge stands for the one narrowing,
  // and the open panel lists it at its top with its remove control (R8).
  await openIssuesTab(page, scene.project, "List");
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  const before = page.url();
  await peekTrigger(page).click();
  const peek = drawer(page);
  await expect(peek.locator("[data-inbox-row]").first()).toBeVisible();
  await pickInboxField(page, "type", /^Question,/, peek);
  const peekBar = controlRow(peek);
  await expect(control(peekBar, CONTROL.panelTrigger)).toHaveAccessibleName("Filter · 1");
  await expect(control(peekBar, CONTROL.chip), "a chip stands in the drawer's line").toHaveCount(0);
  await expect(
    (await openFilterPanel(page, peek)).getByRole("button", { name: REMOVE_CHIP }),
    "the drawer's Filter panel does not list its narrowing"
  ).toHaveCount(1);
  await closeControlPanel(peek);
  expect(page.url(), "the drawer wrote its narrowing into the page's address").toBe(before);
});

/** The keys over a range of the rows `order` lists, from its first, `rowOf` finding a row by its
 *  id and `marks` the selection bar: `x` marks the row in hand, `Shift+J` carries the range from
 *  the row marked last to the next row and `Shift+K` back again, `Shift+X` takes every row from
 *  the row marked last to the row in hand, and Escape twice clears the marks. */
async function walkRowRange(
  page: Page,
  rowOf: (id: string) => Locator,
  order: readonly string[],
  marks: Locator
): Promise<void> {
  const first = rowOf(order[0] ?? "");
  await first.focus();
  await page.keyboard.press("x");
  await expect(marks, "the x key").toContainText("1 selected");
  await page.keyboard.press("Shift+J");
  await expect(rowOf(order[1] ?? ""), "the Shift+J key").toBeFocused();
  await expect(marks, "the Shift+J key").toContainText("2 selected");
  await page.keyboard.press("Shift+K");
  await expect(first, "the Shift+K key").toBeFocused();
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await expect(rowOf(order[2] ?? "")).toBeFocused();
  await page.keyboard.press("Shift+X");
  await expect(marks, "the Shift+X key").toContainText("3 selected");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await expect(marks, "the Escape key that clears the marks").toHaveCount(0);
}

/** The chip `pick` finds among the remove controls of the bar `row`'s chips, where a reader finds
 *  it: in the bar's line where it fits whole, else among the chips the panel of the line's one
 *  `+N more` lists, which this opens. The line draws a chip only whole (`chipsThatFit`,
 *  `components/ControlRow.tsx`), so a chip wider than the line's room stands behind `+N more`. */
async function barChip(
  page: Page,
  row: Locator,
  pick: (chips: Locator) => Locator
): Promise<Locator> {
  const inLine = pick(row.locator("[data-chip-strip]").getByRole("button", { name: REMOVE_CHIP }));
  const more = row.getByRole("button", { name: MORE_CHIPS });
  if (
    (await settledState("the chip", [
      ["line", inLine],
      ["more", more],
    ])) === "line"
  ) {
    return inLine;
  }
  await more.click();
  return pick(
    page.getByRole("dialog", { name: MORE_CHIPS }).getByRole("button", { name: REMOVE_CHIP })
  );
}

/** Every option each Inbox field offers on the k-keep row's seed. */
const INBOX_FIELD_OPTIONS = {
  assignee: ["unassigned", "alice", "bob", "carol"],
  priority: ["P0", "P1", "P2", "P3", "none"],
  project: ["CORE", "MIRROR", "none"],
  type: ["question", "decision", "approval", "reply", "agent"],
  urgency: ["blocking", "high", "med", "low", "none"],
} as const;

/** Every option of the other fields, including values that now have zero rows. */
async function expectInboxFacetCounts(
  page: Page,
  main: Locator,
  applied: keyof typeof INBOX_FIELD_OPTIONS,
  narrowed: readonly ViewRow[]
): Promise<void> {
  const panel = await openFilterPanel(page, main);
  for (const field of Object.keys(INBOX_FIELD_OPTIONS) as (keyof typeof INBOX_FIELD_OPTIONS)[]) {
    if (field === applied) continue;
    const trigger = panel.getByRole("button", { name: `Filter by ${field}` });
    await trigger.click();
    const label = field[0]?.toUpperCase() + field.slice(1);
    const options = page.getByRole("listbox", { name: `${label} options` }).getByRole("option");
    const counts = countsOf(narrowed, field);
    for (const value of INBOX_FIELD_OPTIONS[field]) {
      const name = value === "unassigned" ? "Unassigned" : valueLabel(field, value);
      const detail = field === "assignee" && value === "alice" ? "you, " : "";
      await expect(
        options.filter({ has: page.getByText(name, { exact: true }) }),
        `with ${applied} narrowed: ${label} option ${name} has the wrong row count`
      ).toHaveAccessibleName(`${name}, ${detail}${counts.get(value) ?? 0} rows`);
    }
    await expect(options, `with ${applied} narrowed: all ${label} options`).toHaveCount(
      INBOX_FIELD_OPTIONS[field].length
    );
    await trigger.click();
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
  }
  await closeControlPanel(main);
}

// SLOW(>=2s): eleven seeded rows worked through every field, sort, grouping and view.
test("k-keep: the Inbox keeps every field, sort, grouping, view, Mark all read and clear it had, and Clear filters keeps Blocked on you", async ({
  page,
}) => {
  // Every field, sort, grouping, view, clear and key worked in turn, over three page loads at two
  // widths: the row states a budget of its own, as the drawer's row beside it does.
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const main = page.getByTestId("main-content");
  const chips = control(controlRow(main), CONTROL.chip);
  await page.goto("/?assignee=everyone");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);

  // Each narrowing in turn: only the matching rows are left, a chip is in the bar, and every
  // option of each other field counts the rows that the narrowing kept.
  for (const [field, value, matches] of [
    ["type", /^Question,/, (row: (typeof rows)[number]) => row.type === "question"],
    ["priority", /^P0,/, (row: (typeof rows)[number]) => row.priority === "P0"],
    ["urgency", /^Blocking,/, (row: (typeof rows)[number]) => row.urgency === "blocking"],
    ["project", /^CORE,/, (row: (typeof rows)[number]) => row.project === "CORE"],
  ] as const) {
    await page.goto("/?assignee=everyone");
    await pickInboxField(page, field, value, main);
    const narrowed = rows.filter(matches);
    const expectedIds = narrowed.map((row) => row.id).sort();
    // The list is read once it holds the narrowed count, never in the frame the pick landed in.
    await expect(page.locator("[data-inbox-row]")).toHaveCount(expectedIds.length);
    expect([...(await drawnIds(page))].sort(), `the ${field} field`).toEqual(expectedIds);
    await expect(chips.filter({ hasText: new RegExp(`^${field}`, "i") })).not.toHaveCount(0);
    await expectInboxFacetCounts(page, main, field, narrowed);
  }

  await page.goto("/?assignee=everyone");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  await pickAssignee(page, "bob");
  await closeAssigneeFilter(page);
  await expect(
    chips.and(
      controlRow(main).getByRole("button", { exact: true, name: "Remove Assignee: bob filter" })
    ),
    "picking Bob draws no Assignee: bob chip in the Inbox bar"
  ).toBeVisible();
  const bobs = rows.filter((row) => row.assignee === "bob");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(bobs.length);
  expect([...(await drawnIds(page))].sort(), "the assignee control").toEqual(
    bobs.map((row) => row.id).sort()
  );
  await expectInboxFacetCounts(page, main, "assignee", bobs);

  // The six sorts and the seven groupings, worked from the display menu.
  await page.goto("/?assignee=everyone&group=none");
  for (const sort of ["newest", "priority", "urgency", "type", "project", "assignee"] as const) {
    const menu = await openDisplay(page, main);
    await menu.getByRole("group", { name: "Sort by" }).locator(`input[value="${sort}"]`).click();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    expect(await drawnIds(page), `the ${sort} sort`).toEqual(sortedIds(rows, sort, at));
  }
  await page.keyboard.press("Escape");
  // Each banding draws the bands the fixture holds, each with its own count: the five fields
  // one band per value they hold, the turn bands the four rows waiting on a person and the one
  // handed back to its session, and None no band at all.
  for (const group of [
    "priority",
    "urgency",
    "type",
    "project",
    "assignee",
    "waiting-on",
    "none",
  ] as const) {
    const menu = await openDisplay(page, main);
    await menu.getByRole("group", { name: "Group by" }).locator(`input[value="${group}"]`).click();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    const expected =
      group === "none"
        ? []
        : group === "waiting-on"
          ? [`Waiting on a person (${rows.length - 1})`, "Waiting on agents (1)"]
          : valuesOf(rows, group).map(
              (value) => `${bandLabel(group, value)} (${countsOf(rows, group).get(value)})`
            );
    expect(await bandHeadings(page), `the ${group} grouping`).toEqual(expected);
  }
  await page.keyboard.press("Escape");

  // The dismissed view and the thread order.
  const menu = await openDisplay(page, main);
  await menu.getByRole("group", { name: "Show" }).locator('input[value="dismissed"]').click();
  await expect(page).toHaveURL(/show=dismissed/);
  await menu.getByRole("group", { name: "Show" }).locator('input[value="inbox"]').click();
  await menu.getByRole("group", { name: "Thread order" }).locator("input").last().click();
  await expect(
    menu.getByRole("group", { name: "Thread order" }).locator("input").last()
  ).toBeChecked();
  await page.keyboard.press("Escape");

  // Mark all read, from the bar's More, and the fold of one group.
  await page.goto("/?assignee=everyone&group=type");
  const more = controlRow(main).getByRole("button", { exact: true, name: "More" });
  await more.click();
  const markAll = page
    .getByRole("menu", { name: "More" })
    .getByRole("menuitem", { exact: true, name: "Mark all read" });
  await markAll.click();
  // More held Mark all read alone in the wide bar, so it goes once nothing is left unread.
  await expect(more).toHaveCount(0);
  const band = page.getByTestId("inbox-list").locator("h2 button").first();
  await band.click();
  await expect(band).toHaveAttribute("aria-expanded", "false");

  // Clear filters takes off the four fields, the assignee and the agent, and leaves the text, the
  // sort, the grouping and the view. The agent is the session that asked the seed's questions,
  // so the narrowing is on and its chip drawn before the press.
  await page.goto(
    `/?assignee=bob&type=question&priority=P0&urgency=blocking&project=CORE&agent=${encodeURIComponent(THREAD_SESSION.session_id)}&q=rule&sort=priority&group=type&show=dismissed`
  );
  await expect(controlRow(main).getByRole("button", { name: /^Remove Asks from / })).toBeVisible();
  await controlRow(main).getByRole("button", { exact: true, name: "Clear filters" }).click();
  await expect(page).not.toHaveURL(/[?&]agent=/);
  const address = new URL(page.url());
  for (const gone of ["assignee", "type", "priority", "urgency", "project", "agent"]) {
    expect(address.searchParams.has(gone), `Clear filters left ?${gone}=`).toBe(false);
  }
  expect(address.searchParams.get("q"), "Clear filters took the text").toBe("rule");
  expect(address.searchParams.get("sort"), "Clear filters took the sort").toBe("priority");
  expect(address.searchParams.get("group"), "Clear filters took the grouping").toBe("type");
  expect(address.searchParams.get("show"), "Clear filters took the view").toBe("dismissed");

  // Blocked on you, then one field's narrowing, which the switch's choice does not clear, then
  // Clear filters, which takes the field off and keeps `section=needs-you` (ST7 rows 5 and 6).
  await page.goto("/?assignee=everyone");
  await controlRow(main)
    .getByRole("button", { name: /^Blocked on you\b/ })
    .click();
  await expect(page).toHaveURL(/[?&]section=needs-you(&|$)/);
  await pickInboxField(page, "type", /^Question,/, main);
  await expect(page).toHaveURL(/[?&]type=question(&|$)/);
  await expect(page, "a field's narrowing took the section off").toHaveURL(
    /[?&]section=needs-you(&|$)/
  );
  await controlRow(main).getByRole("button", { exact: true, name: "Clear filters" }).click();
  await expect(page).not.toHaveURL(/[?&]type=/);
  await expect(page, "Clear filters took the section off").toHaveURL(/[?&]section=needs-you(&|$)/);

  // The keys the Inbox's list answers (`Inbox.tsx`'s keymap), below the reading column's width,
  // where each row is a card the keys move the focus over; `l`, which takes the focus into the
  // reading column where one is drawn; and `g i`, the global binding that opens the Inbox.
  const copied = await recordClipboard(page);
  await page.setViewportSize({ height: 900, width: 1024 });
  await page.goto("/?assignee=everyone&group=none");
  const listed = main.locator("[data-inbox-row]");
  await expect(listed).toHaveCount(rows.length);
  const rowOf = (id: string) => main.locator(`[data-inbox-row="${id}"]`);
  const seeded = (match: (row: ViewRow) => boolean): ViewRow => {
    const found = rows.find(match);
    if (found === undefined) throw new Error("the seed holds no such row");
    return found;
  };
  const order = await drawnIds(page);
  const search = page.getByRole("searchbox", { name: "Search asks" });
  await page.locator("body").focus();
  await page.keyboard.press("f");
  await expect(search, "the f key").toBeFocused();
  await page.keyboard.press("Escape");
  await expect(rowOf(order[0] ?? ""), "the Escape key, out of the search box").toBeFocused();
  await page.keyboard.press("j");
  await expect(rowOf(order[1] ?? ""), "the j key").toBeFocused();
  await page.keyboard.press("k");
  await expect(rowOf(order[0] ?? ""), "the k key").toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(rowOf(order[1] ?? ""), "the ArrowDown key").toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(rowOf(order[0] ?? ""), "the ArrowUp key").toBeFocused();
  const ledger = seeded((row) => row.type === "question" && row.priority === "P0");
  const ask = rowOf(ledger.id);
  await ask.focus();
  await page.keyboard.press("Enter");
  await expect(ask.locator("[data-ask-answer]"), "the Enter key").toBeFocused();
  await page.keyboard.press("Escape");
  await expect(ask, "the Escape key, back to the row").toBeFocused();
  await page.keyboard.press("h");
  await expect(ask.locator("[data-inbox-snooze]"), "the h key").toBeFocused();
  await page.keyboard.press("Escape");
  await expect(ask).toBeFocused();
  await page.keyboard.press("y");
  await expect
    .poll(async () => (await copied()).join(" "), { message: "the y key" })
    .toContain(ledger.issueKey ?? "");
  const decision = rowOf(seeded((row) => row.type === "decision").id);
  await decision.focus();
  await page.keyboard.press("1");
  await expect(decision.locator("[data-ask-option]").first(), "the 1 key").toBeChecked();
  await walkRowRange(page, rowOf, order, control(page, CONTROL.selectionBar));
  const thread = rowOf(seeded((row) => row.type === "reply").id);
  await thread.focus();
  await page.keyboard.press("e");
  await expect(thread, "the e key").toHaveCount(0);
  await page.locator("body").focus();
  await page.keyboard.press("n");
  const compose = page.getByRole("dialog", { name: "New message" });
  await expect(compose, "the n key").toBeVisible();
  await page.keyboard.press("Escape");
  await expect(compose).toHaveCount(0);
  await ask.focus();
  await page.keyboard.press("o");
  await expect(page, "the o key").toHaveURL(new RegExp(`/issues/${ledger.issueKey ?? ""}`));
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("i");
  await expect(page, "the g i keys").toHaveURL(/\/$/);
  // `l` takes the focus into the reading column, which the Inbox draws from 1280 px for the row
  // it has selected.
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto("/?assignee=everyone&group=none");
  await rowOf(order[0] ?? "")
    .locator("time")
    .first()
    .click();
  await expect(page.getByTestId("inbox-pane")).toBeVisible();
  await rowOf(order[0] ?? "").focus();
  await page.keyboard.press("l");
  await expect(page.getByTestId("inbox-pane-scroller"), "the l key").toBeFocused();
});

// SLOW(>=2s): the List's whole layout worked and then read back after a reload, and the Board's
// two disclosures each read back after a reload.
// k-keep on the project List and the Board, both on the bar's slots: the List's grouping, its
// column reorder, resize and visibility, its pager, its virtual rows, its bar of status counts
// and the layout it stores at `dispatch.project-table.<project>`; the Board's filters opened, its
// last chip cleared and the disclosure it stores at `project.issue-filters` found as it was left;
// each list's own keys (`list-*` in `IssuesTable.tsx`, the Board's `o`), and `g p`, the global
// binding that opens a project's page.
test("k-keep: the project List keeps its grouping, columns, pager, layout and keys, and the Board its filters' disclosure and its keys", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 60; index++) {
    const issue = await createIssue({ project: "CORE", title: `Listed work ${index}` });
    await patchIssue(issue.key, { labels: ["legion"], status: "todo" });
  }
  // `g p`, the global binding that opens a project's page: the one bar, where the project is a
  // row the reader types their way to. The keys wait for the Inbox's heading, since pressed as
  // the page loads they can open nothing.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("p");
  await page.getByRole("combobox", { name: "Search" }).fill("Core");
  await expect(
    page.getByRole("dialog", { name: "Search" }).getByRole("group", { name: "Projects" })
  ).toBeVisible();
  await page.getByRole("combobox", { name: "Search" }).press("Enter");
  await expect(page).toHaveURL(/\/projects\/CORE\/issues/);
  await openIssuesTab(page, "CORE", "List");
  const table = page.getByRole("table", { name: "Project issues table" });
  await expect(table).toBeVisible();
  const main = page.getByTestId("main-content");

  // The bar of status counts, which this change leaves alone.
  await expect(page.getByRole("button", { name: /^Todo/ }).first()).toBeVisible();

  // The virtual rows: the table draws a window of its page, never all sixty.
  const rows = page.locator("[data-issue-row]");
  const drawn = await rows.count();
  expect(drawn, "the table drew every row of the page at once").toBeLessThan(50);
  expect(drawn, "the table drew no rows").toBeGreaterThan(0);

  // The pager: its own words, and a second page.
  const pager = control(main, CONTROL.pager);
  await expect(pager).toContainText("60 of 60 issues");
  await pager.getByRole("button", { name: "Next" }).click();
  await expect(page).toHaveURL(/[?&]page=2/);
  const perPage = listTrigger(pager, "Rows per page");
  await pickFromList(perPage, "100 per page");
  await expect(page).toHaveURL(/[?&]size=100/);

  // The grouping, from the display menu.
  const menu = await openDisplay(page, main);
  await menu.getByRole("group", { name: "Group by" }).locator('input[value="status"]').click();
  await expect(page).toHaveURL(/[?&]group=status/);
  await page.keyboard.press("Escape");

  // The column visibility, from the columns menu.
  await control(controlRow(main), CONTROL.columnsMenu).click();
  await page
    .getByRole("dialog", { name: "Columns" })
    .getByRole("checkbox", { exact: true, name: "Updated" })
    .check();
  await page.keyboard.press("Escape");
  const updated = page
    .getByRole("columnheader")
    .filter({ has: page.getByRole("button", { exact: true, name: /^Updated/ }) });
  await expect(updated).toHaveCount(1);

  // The column resize, dragged on the header's own edge.
  const header = page
    .getByRole("columnheader")
    .filter({ has: page.getByRole("button", { exact: true, name: /^Epic/ }) });
  // The Updated header was waited for above, and the Epic header stands in that same header
  // row, so its box is drawn.
  const before = await header.boundingBox();
  if (before === null) throw new Error("the Epic column has no box");
  await page.mouse.move(before.x + before.width - 2, before.y + before.height / 2);
  await page.mouse.down();
  await page.mouse.move(before.x + before.width + 60, before.y + before.height / 2, { steps: 8 });
  await page.mouse.up();
  const after = await header.boundingBox();
  expect(after?.width ?? 0, "the Epic column did not resize").toBeGreaterThan(before.width + 20);

  // The column reorder. The header moves a column on HTML5 drag events - `dragstart` on its
  // sort button, `dragover` and `drop` on the header it lands in (`components/DataTable.tsx`) -
  // which a synthesised mouse drag never produces, so the events are dispatched as the browser
  // would, sharing one `DataTransfer` so the drop reads what the start wrote.
  const order = async () =>
    page
      .getByRole("columnheader")
      .evaluateAll((cells) =>
        cells.map((cell) => (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, ""))
      );
  const orderBefore = await order();
  // The start and the drop are frames apart: the header cell takes a drop only once React has
  // re-rendered with the dragged column in hand, which it has not done in the tick the
  // `dragstart` was dispatched in.
  await page.evaluate(async () => {
    const headerOf = (label: string): HTMLElement | undefined =>
      [...document.querySelectorAll<HTMLElement>("th")].find((cell) =>
        (cell.querySelector("button")?.textContent ?? "").startsWith(label)
      );
    const handle = headerOf("Epic")?.querySelector("button");
    const to = headerOf("Labels");
    if (handle === null || handle === undefined || to === undefined) {
      throw new Error("the Epic and Labels headers are not both drawn");
    }
    const dataTransfer = new DataTransfer();
    handle.dispatchEvent(new DragEvent("dragstart", { bubbles: true, dataTransfer }));
    for (let frame = 0; frame < 3; frame++) {
      const drawn = Promise.withResolvers<void>();
      requestAnimationFrame(() => drawn.resolve());
      await drawn.promise;
    }
    to.dispatchEvent(new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer }));
    to.dispatchEvent(new DragEvent("drop", { bubbles: true, dataTransfer }));
    handle.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer }));
  });
  await expect.poll(order, { message: "the columns did not reorder" }).not.toEqual(orderBefore);

  // Reloaded, the stored layout is as it was left.
  const widths = await page
    .getByRole("columnheader")
    .evaluateAll((cells) => cells.map((cell) => Math.round(cell.getBoundingClientRect().width)));
  const kept = await order();
  await page.reload();
  await expect(table).toBeVisible();
  await expect
    .poll(order, { message: "the stored column order is not as it was left" })
    .toEqual(kept);
  expect(
    await page
      .getByRole("columnheader")
      .evaluateAll((cells) => cells.map((cell) => Math.round(cell.getBoundingClientRect().width))),
    "the stored column widths are not as they were left"
  ).toEqual(widths);

  // The List's own keys: `j` takes the first row in hand, `x` marks it and marks it off again,
  // and `o` opens its issue.
  await page.locator("body").focus();
  await page.keyboard.press("j");
  await page.keyboard.press("x");
  await expect(control(main, CONTROL.selectionBar), "j then x marked no row").toBeVisible();
  await page.keyboard.press("x");
  await expect(control(main, CONTROL.selectionBar), "x again left the row marked").toHaveCount(0);
  await page.keyboard.press("o");
  await expect(page, "o opened no issue from the List").toHaveURL(/\/issues\/CORE-\d+/);
  await page.goBack();
  await expect(table).toBeVisible();

  // The Board: the filters opened by the reader, the last chip taken off at the top of their
  // panel, which stays open, and the panel's disclosure, which the Board stores at
  // `project.issue-filters`, found as it was left after a reload - open, and then shut.
  const board = page.getByRole("button", { exact: true, name: "Board" });
  await board.click();
  await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
  await page.goto("/projects/CORE/issues?needs-you=1&unread=1");
  const boardChips = control(controlRow(main), CONTROL.chip);
  const boardTrigger = control(controlRow(main), CONTROL.panelTrigger);
  await expect(boardChips).toHaveCount(2);
  // The Board opens the panel by itself while a narrowing is on and nothing is stored; shut first,
  // so the open below is the reader's own, which is what the Board stores.
  await closeControlPanel(main);
  const filters = await openFilterPanel(page, main);
  await filters.getByRole("button", { name: REMOVE_CHIP }).last().click();
  await expect(boardChips).toHaveCount(1);
  await expect(boardTrigger, "the last chip's removal shut the panel").toHaveAttribute(
    "aria-expanded",
    "true"
  );
  await page.reload();
  await expect(
    boardTrigger,
    "the Board's filter panel, left open, is not open after a reload"
  ).toHaveAttribute("aria-expanded", "true");
  await closeControlPanel(main);
  await page.reload();
  await expect(boardChips).toHaveCount(1);
  await expect(
    boardTrigger,
    "the Board's filter panel, left shut, is not shut after a reload"
  ).toHaveAttribute("aria-expanded", "false");
  // The Board's other disclosure, Icebox & Done in the project header.
  const edges = page.getByRole("button", { name: /Icebox & Done$/ });
  const edgesLabel = await edges.textContent();
  await edges.click();
  await page.reload();
  await expect(
    page.getByRole("button", { name: /Icebox & Done$/ }),
    "the Board's Icebox & Done is not as it was left"
  ).not.toHaveText(edgesLabel ?? "");
  // The Board's own `o` opens the issue of the card in hand.
  await page.goto("/projects/CORE/issues");
  const card = page.locator("[data-board-card]").first();
  await card.focus();
  await page.keyboard.press("o");
  await expect(page, "o opened no issue from the Board").toHaveURL(/\/issues\/CORE-\d+/);
});

// k-keep on the priorities page and a priority's own page. What each offered before R8, paths
// under `web/src/features/priorities/`: the priorities table's search box, the filter on each of
// its column headers with a chip each in its bar, its grouping in the display menu, its columns
// menu and New priority (`priority-table-controls.tsx`, `PrioritiesTable.tsx`), and its `j`, `k`,
// `o` and Enter (`PrioritiesTable.tsx`, `NAMES`); a priority's own page draws the project List's
// bar above its issues table, with the List's `j`, `k`, `x`, `o` and Enter. No global binding opens
// either page: the rail's Priorities link does.
// SLOW(>=2s): three priorities and their work seeded, and both pages worked through.
test("k-keep: the priorities page keeps its search, column filter and chip, grouping, columns, New priority and keys, and a priority's own page its issues table's", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "BOOKS", name: "Books" });
  const ids: string[] = [];
  for (const [owner, title] of [
    ["alice", "Keep the books whole"],
    ["bob", "Ship the ledger"],
    ["alice", "Close the quarter"],
  ] as const) {
    ids.push((await createPriority({ owner, title })).id);
  }
  for (const title of ["Audit the books", "Reconcile the books"]) {
    const issue = await createIssue({ project: "BOOKS", title });
    await patchIssue(issue.key, { advances: [ids[0] ?? ""], status: "in_progress" });
  }
  const main = page.getByTestId("main-content");
  await page.goto("/priorities");
  const table = main.getByRole("region", { exact: true, name: "Priorities" });
  const bar = controlRow(table);
  const rows = table.locator("tr[data-priority-row]");
  await expect(rows).toHaveCount(3);

  // The search.
  await control(bar, CONTROL.searchBox).fill("ledger");
  await expect(rows, "the search left another number of priorities").toHaveCount(1);
  await control(bar, CONTROL.searchBox).fill("");
  await expect(rows).toHaveCount(3);

  // A column filter from its header, and its chip in the bar, whose press takes it off.
  await table.getByRole("button", { name: /^Filter Owner/ }).click();
  await page.getByRole("option", { name: /^bob\b/ }).click();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
  await expect(rows, "the Owner filter left another number of priorities").toHaveCount(1);
  const chip = bar.getByRole("button", { exact: true, name: "Remove Owner: bob filter" });
  await expect(chip, "the Owner filter has no chip in the bar").toBeVisible();
  await chip.click();
  await expect(rows).toHaveCount(3);

  // The grouping, from the display menu.
  const menu = await openDisplay(page, table);
  await menu.getByRole("group", { name: "Group by" }).locator('input[value="owner"]').click();
  await expect(page).toHaveURL(/[?&]p\.group=owner/);
  await page.keyboard.press("Escape");
  await expect(
    table.getByRole("button", { name: /^Collapse .+, \d+ priorit(y|ies)$/ }),
    "no group per owner"
  ).toHaveCount(2);

  // The columns menu: its Triage, Todo box takes that count's column out of the table.
  const triageTodo = table
    .getByRole("table", { exact: true, name: "Priorities table" })
    .getByRole("columnheader", { name: /^Triage, Todo/ });
  await expect(triageTodo, "the table draws no Triage, Todo column to hide").toBeVisible();
  await control(bar, CONTROL.columnsMenu).click();
  await page
    .getByRole("dialog", { name: "Columns" })
    .getByRole("checkbox", { exact: true, name: "Triage, Todo" })
    .uncheck();
  await page.keyboard.press("Escape");
  await expect(triageTodo, "the columns menu left Triage, Todo shown").toHaveCount(0);

  // New priority: the bar's action opens its form under the bar, which writes the priority.
  await bar.getByRole("button", { exact: true, name: "New priority" }).click();
  const form = table.locator('form[aria-label="New priority"]');
  await form.getByLabel("Title of the new priority").fill("Retire the old ledger");
  await form.getByRole("button", { name: "Add priority" }).click();
  await expect(form).toHaveCount(0);
  await expect(table.getByRole("row", { name: /Retire the old ledger$/ })).toHaveCount(1);

  // The table's own keys: `j` takes a priority in hand and `o` opens its page.
  await page.locator("body").focus();
  await page.keyboard.press("j");
  await page.keyboard.press("o");
  await expect(page, "j then o opened no priority's page").toHaveURL(/\/priorities\/[^/?]+/);

  // A priority's own page: the project List's bar above its issues table.
  await page.goto(`/priorities/${ids[0]}`);
  const work = controlRow(main);
  const issueRows = main.locator("[data-issue-row]");
  await expect(issueRows).toHaveCount(2);
  await control(work, CONTROL.searchBox).fill("Audit");
  await expect(issueRows, "the search left another number of issues").toHaveCount(1);
  await control(work, CONTROL.searchBox).fill("");
  await expect(issueRows).toHaveCount(2);
  // A column filter from its header: Todo taken out of the open statuses it opens with, which is
  // a narrowing of the reader's, so its chip stands in the bar - in its line, or behind its one
  // `+N more` where it does not fit whole - and its press there puts them back.
  await pickFilterOption(page, "Status", "Todo");
  const workChips = control(work, CONTROL.chip);
  await expect(workChips, "the Status filter has no chip in the bar").toHaveCount(1);
  await expect(issueRows).toHaveCount(2);
  const statusChip = await barChip(page, work, (chips) => chips.filter({ hasText: /^Status: / }));
  await statusChip.click();
  await expect(statusChip).toHaveCount(0);
  await expect(workChips, "the Status filter's chip stayed in the bar").toHaveCount(0);
  const workMenu = await openDisplay(page, main);
  await workMenu.getByRole("group", { name: "Group by" }).locator('input[value="status"]').click();
  await expect(page).toHaveURL(/[?&]w\.group=status/);
  await page.keyboard.press("Escape");
  await control(work, CONTROL.columnsMenu).click();
  await page
    .getByRole("dialog", { name: "Columns" })
    .getByRole("checkbox", { exact: true, name: "Updated" })
    .check();
  await page.keyboard.press("Escape");
  await expect(
    main
      .getByRole("columnheader")
      .filter({ has: page.getByRole("button", { exact: true, name: /^Updated/ }) }),
    "the columns menu left Updated hidden"
  ).toHaveCount(1);
  // Its table's own keys: `j` takes the first issue in hand and `x` marks it and marks it off.
  await page.locator("body").focus();
  await page.keyboard.press("j");
  await page.keyboard.press("x");
  await expect(control(main, CONTROL.selectionBar), "j then x marked no issue").toBeVisible();
  await page.keyboard.press("x");
  await expect(control(main, CONTROL.selectionBar), "x again left it marked").toHaveCount(0);
});

test("k-keep: the Delivery list still answers the keyboard and stays windowed, and each page's own bindings still run", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedDeliveryFixture();
  // 36 px per row plus 12 rows of overscan on each side (PRList and DataTable). Even a
  // table as tall as this viewport can draw fewer than half these pull requests.
  const extra = 120;
  const seeded = extra + 2;
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     )
     SELECT 'acme/widgets', 100 + n, 'feat: windowed widget ' || n,
            'https://github.com/acme/widgets/pull/' || (100 + n), 'octocat',
            timestamptz '2024-06-01T04:00:00Z' + n * interval '1 minute',
            timestamptz '2024-06-01T06:00:00Z' + n * interval '1 minute',
            1, 1, false, null, ARRAY[]::text[], false
       FROM generate_series(1, ${extra}) AS n`
  );
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  const box = control(page.getByTestId("main-content"), CONTROL.dataTable);
  const prs = box.locator("tbody tr[data-pr-row]");
  const first = prs.and(box.locator('[data-index="0"]'));
  const last = prs.and(box.locator(`[data-index="${seeded - 1}"]`));
  await expect(first).toBeVisible();
  // The first row has mounted in the shared table, so the count is of its drawn window.
  expect(await prs.count(), "the Delivery list draws all seeded pull requests").toBeLessThan(
    seeded
  );
  await expect(last, "the last pull request is drawn before scrolling").toHaveCount(0);
  await box.evaluate((node) => {
    node.scrollTop = node.scrollHeight;
  });
  await expect(
    last,
    "scrolling the Delivery list does not draw its last pull request"
  ).toBeVisible();
  await box.evaluate((node) => {
    node.scrollTop = 0;
  });
  await first.focus();
  await page.keyboard.press("ArrowDown");
  await expect(prs.nth(1)).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(first).toBeFocused();
  await page.keyboard.press("End");
  await expect(last, "End did not focus the last pull request").toBeFocused();
  await page.keyboard.press("Home");
  await expect(first, "Home did not focus the first pull request").toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog", { name: "Details" })).toBeVisible();
  await page.keyboard.press("Escape");

  // The global binding that opens each page, and each page's own list binding.
  await seedAgents();
  await seedSurfaces();
  // The keys wait for the Inbox's heading: pressed as the page loads, before the shell has
  // registered its keys, they can open nothing.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("a");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  await page.keyboard.press("g");
  await page.keyboard.press("i");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  // The Inbox's own list keys: `j` moves the hand down the list and `x` marks the row.
  await page.locator("body").focus();
  await page.keyboard.press("j");
  await page.keyboard.press("x");
  await expect(control(page.getByTestId("main-content"), CONTROL.selectionBar)).toBeVisible();
  // The project page's own `v`, which flips List and Board.
  await page.goto("/projects/CORE/issues");
  await page.locator("body").focus();
  const boardBefore = await page
    .getByRole("button", { exact: true, name: "Board" })
    .getAttribute("aria-pressed");
  await page.keyboard.press("v");
  await expect(page.getByRole("button", { exact: true, name: "Board" })).not.toHaveAttribute(
    "aria-pressed",
    boardBefore ?? "false"
  );
});

/** The Agents page's keep row's sessions, by id: UUIDs, which Dispatch's agent routes require. */
const KEPT_AGENT = {
  auditor: "01a0e812-3135-7000-8000-000000000003",
  planner: "01a0e812-3135-7000-8000-000000000001",
  reviewer: "01a0e812-3135-7000-8000-000000000002",
  sleeper: "01a0e812-3135-7000-8000-000000000005",
  tester: "01a0e812-3135-7000-8000-000000000004",
} as const;

/** The Agents page's four groups, in the order it draws them. */
const AGENT_GROUP_ORDER = ["needs-you", "active", "idle", "quiet"] as const;

/** A live session of that row. `last_seen` is taken when the row seeds, never at this module's
 *  scope, which a worker evaluates once and every later row of that worker ages. */
function keptAgent(
  sessionID: string,
  title: string,
  machine: string,
  role: string,
  silentFor = 5_000
): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: `/srv/${title.toLowerCase()}`,
    last_seen: Date.now() - silentFor,
    machine_id: machine,
    roles: [role],
    session_id: sessionID,
    title,
  };
}

/**
 * Five sessions over two machines and three roles, one in each group but Idle, which holds two
 * so a pin has an order to change: the Planner needs the reader (an open ask of its own waits on
 * them), has a question whose next move is its own and a reply the reader has not read; the
 * Reviewer wrote a comment a moment ago; the Auditor and the Tester have done nothing in
 * Dispatch; the Sleeper has not been heard from for an hour. Returns the issue the Planner's asks
 * name, which the search box finds it by.
 */
async function seedKeptAgents(): Promise<string> {
  await setLiveSessions([
    keptAgent(KEPT_AGENT.planner, "Planner", "box-1", "planner"),
    keptAgent(KEPT_AGENT.reviewer, "Reviewer", "box-1", "reviewer"),
    keptAgent(KEPT_AGENT.auditor, "Auditor", "box-2", "tester"),
    keptAgent(KEPT_AGENT.tester, "Tester", "box-2", "tester"),
    keptAgent(KEPT_AGENT.sleeper, "Sleeper", "box-2", "tester", 60 * 60_000),
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agents kept" });
  const asPlanner = {
    actor: { id: KEPT_AGENT.planner, kind: "session" as const },
    as: "agent" as const,
  };
  await createAsk(issue.key, { question: "Which release?" }, asPlanner);
  // The reader has answered the second, so its next move is the Planner's.
  const answered = await createAsk(issue.key, { question: "Which branch?" }, asPlanner);
  await createComment(issue.key, { ask_id: answered.id, body: "Which one do you mean?" });
  await createComment(
    issue.key,
    { body: "Reviewing the diff." },
    { actor: { id: KEPT_AGENT.reviewer, kind: "session" }, as: "agent" }
  );
  const asked = await createAgentMessage(KEPT_AGENT.planner, {
    body: "How far along?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: "Nearly there." },
    { id: KEPT_AGENT.planner, kind: "session" }
  );
  return issue.key;
}

/** Reads each group heading's name and count, finding each heading by its test id, in
 *  `AGENT_GROUP_ORDER`. */
async function expectAgentGroups(
  page: Page,
  headings: readonly [string, string, string, string],
  what: string
): Promise<void> {
  for (const [index, group] of AGENT_GROUP_ORDER.entries()) {
    await expect(
      agentGroupToggle(page, group),
      `${what}: the ${group} group's heading and count`
    ).toHaveAccessibleName(headings[index] ?? "");
  }
}

// R6 on the Agents page. What the page offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Fields: the group (`features/agents/AgentsPage.tsx:932-949`), the machine (:950-962) and the
//   role (:963-975), each a multi-select, and the search box (:978-990), which matches a
//   session's title, directory, machine, id and the issues its open asks name
//   (`features/agents/agent-search.ts:67-76`). Each is put on and the rows it leaves read, with
//   the group counts under it, and its chip taken off again.
// - Sort: no control. In a group, pinned rows first, then who needs the reader, open asks, newest
//   activity and title (`AgentsPage.tsx:114-144`). Read as the rows' order, and again after a pin.
// - Grouping: Needs you, Active, Idle and Quiet (`features/agents/agent-groups.ts:21-67`,
//   `AgentsPage.tsx:146-173`), each folding (:854-895, :1523-1540). Each group's place is read,
//   and one is folded and opened.
// - Counts: each group heading's `(N)` and `(N, K selected)` (`AgentsPage.tsx:885-887`,
//   `features/agents/selection.ts:61-66`); the select-all header's `N matching` and
//   `K of N matching selected` (`AgentsPage.tsx:1032-1034`, `selection.ts:45-59`); a row's
//   `New reply N` (`AgentsPage.tsx:721-735`), `Needs you N` (:736-744) and `Waiting on agent N`
//   (:745-760); the composer's `Broadcast to N of M selected` (:1136). Each is read.
// - Views: none; the page offers no choice of view.
// - Bulk action: a row's checkbox (`AgentsPage.tsx:631-638`), select-all (:1022-1031), Clear
//   selection (:1035-1043) and the broadcast composer's Send (:1048-1219). Driven to a send, and
//   the fake Envoy read for the one message each selected session was sent.
// - Keys (`features/agents/keyboard.ts:148-244`): `/` (:155-161), `j` (:164-171), `k`
//   (:172-179), Enter (:180-189), `i` (:190-217), Shift+P (:218-224), `x` (:225-232) and Escape
//   (:233-243); and `g a`, the global binding that opens the page (`app.tsx:576`). Each pressed
//   and what it did read.
// - Stored for the reader: the pins (`features/agents/pins.ts:61-125`) and the read mark an
//   opened row writes (`features/agents/unread.ts:128-145`), both the reader's agent state in
//   Dispatch; the page kept nothing in the browser (no `useUserPreference`, no storage). Both are
//   found again after a reload.
// SLOW(>=2s): five seeded sessions worked through every narrowing, count, key, pin and a send.
test("k-keep: the Agents page keeps every narrowing, grouping, count, key, pin and broadcast it had @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  const issue = await seedKeptAgents();
  const main = page.getByTestId("main-content");
  const titles = shownAgentRows(page).locator("h2");
  const planner = agentRow(page, KEPT_AGENT.planner);
  const plannerToggle = planner.getByRole("button", { exact: true, name: "Planner" });
  const reviewer = agentRow(page, KEPT_AGENT.reviewer);
  const tester = agentRow(page, KEPT_AGENT.tester);
  const everyRow = ["Planner", "Reviewer", "Auditor", "Tester", "Sleeper"];
  const opened = ["Needs you (1)", "Active (1)", "Idle (2)", "Quiet (1)"] as const;

  // `g a`, from another page.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("a");
  await expect(
    page.getByRole("heading", { level: 1, name: "Agents" }),
    "the g a key opened no Agents page"
  ).toBeVisible();
  await expect(planner).toBeVisible();
  if (phone) await assertPageFits(page);

  // The four groups with their counts, and the order inside them.
  await expectAgentGroups(page, opened, "the page as it opens");
  await expect(titles, "the rows' order").toHaveText(everyRow);
  await expect(
    main.getByText("5 matching", { exact: true }),
    "the select-all header's count"
  ).toBeVisible();
  await expect(
    planner.getByRole("link", { exact: true, name: "Needs you 1" }),
    "the row's Needs you count"
  ).toBeVisible();
  await expect(
    planner.getByRole("link", { exact: true, name: "Waiting on agent 1" }),
    "the row's Waiting on agent count"
  ).toBeVisible();
  const unread = planner.getByRole("button", { exact: true, name: "New reply 1 from Planner" });
  await expect(unread, "the row's count of replies not read").toBeVisible();

  // A group's fold.
  await toggleAgentGroup(page, "idle");
  await expect(titles, "the Idle group's fold").toHaveText(["Planner", "Reviewer", "Sleeper"]);
  await toggleAgentGroup(page, "idle");
  await expect(titles).toHaveText(everyRow);

  // The search box, by a title and by the issue an open ask names.
  const search = control(controlRow(main), CONTROL.searchBox);
  await search.fill("reviewer");
  await expect(titles, "the search box, by a title").toHaveText(["Reviewer"]);
  await expectAgentGroups(
    page,
    ["Needs you (0)", "Active (1)", "Idle (0)", "Quiet (0)"],
    "the search box"
  );
  await expect(
    main.getByText("1 matching", { exact: true }),
    "the select-all header's count under the search box"
  ).toBeVisible();
  await search.fill(issue);
  await expect(titles, "the search box, by an open ask's issue").toHaveText(["Planner"]);
  await search.fill("");
  await expect(titles).toHaveText(everyRow);

  // The three multi-selects, each with its chip.
  const chips = control(controlRow(main), CONTROL.chip);
  for (const [field, option, shown, headings] of [
    ["group", "Quiet", ["Sleeper"], ["Needs you (0)", "Active (0)", "Idle (0)", "Quiet (1)"]],
    [
      "machine",
      "box-2",
      ["Auditor", "Tester", "Sleeper"],
      ["Needs you (0)", "Active (0)", "Idle (2)", "Quiet (1)"],
    ],
    ["role", "reviewer", ["Reviewer"], ["Needs you (0)", "Active (1)", "Idle (0)", "Quiet (0)"]],
  ] as const) {
    await openFilterPanel(page, main);
    await expect(agentFilter(page, field), `the ${field} narrowing`).toBeVisible();
    await pickAgentFilter(page, field, option);
    await expect(titles, `the ${field} narrowing`).toHaveText(shown);
    await expectAgentGroups(page, headings, `the ${field} narrowing`);
    // The chip stands in the bar's line where the bar is 640 px wide or wider; below it, as at
    // the phone's width, Filter's badge stands for it and the open panel lists it at its top (R8).
    const chip = phone
      ? (await openFilterPanel(page, main)).getByRole("button", { name: REMOVE_CHIP })
      : chips;
    await expect(chip, `the ${field} narrowing's chip`).toHaveCount(1);
    await chip.click();
    await expect(titles, `the ${field} narrowing's chip taken off`).toHaveText(everyRow);
    // Shut as a reader shuts it, so the next press lands on the page rather than on the panel.
    if (phone) await closeControlPanel(main);
  }

  // The read mark: the count's own press opens the conversation, which reads it. The row is
  // shut again, and the focus left in it.
  await unread.click();
  await expect(unread, "opening the row left its replies unread").toHaveCount(0);
  await plannerToggle.click();

  // The page's own keys. `/` is the page's search box, shadowing the global search bar; `j` from
  // the box takes the first row, the one the press above left the focus in.
  await page.keyboard.press("/");
  await expect(search, "the / key").toBeFocused();
  await expect(page.getByRole("dialog", { name: "Search" })).toHaveCount(0);
  await search.blur();
  await page.keyboard.press("j");
  await expect(planner, "the j key").toBeFocused();
  await page.keyboard.press("j");
  await expect(reviewer, "the j key, again").toBeFocused();
  await page.keyboard.press("k");
  await expect(planner, "the k key").toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    planner.getByRole("textbox", { name: "Comment" }),
    "the Enter key, into the row's composer"
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(planner, "the Escape key, out of the composer").toBeFocused();
  const plannerMark = planner.getByRole("checkbox", { name: "Select Planner for broadcast" });
  await page.keyboard.press("x");
  await expect(plannerMark, "the x key").toBeChecked();
  await page.keyboard.press("x");
  await expect(plannerMark).not.toBeChecked();
  await page.keyboard.press("j");
  await expect(reviewer).toBeFocused();
  await page.keyboard.press("i");
  const picker = reviewer.getByRole("button", { name: "Choose issue" });
  await expect(picker, "the i key").toHaveAttribute("aria-expanded", "true");
  await expect(issueSearchIn(reviewer)).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(picker, "the Escape key, out of the Issue list").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  await expect(reviewer).toBeFocused();
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await expect(tester).toBeFocused();
  await page.keyboard.press("Shift+P");
  await expect(
    tester.getByRole("button", { name: "Unpin Tester" }),
    "the Shift+P key"
  ).toBeVisible();
  const pinned = ["Planner", "Reviewer", "Tester", "Auditor", "Sleeper"];
  await expect(titles, "the pin's place at the head of its group").toHaveText(pinned);

  // The read mark, read where the page draws it. The Planner answers a second message while its
  // row is shut, which marks nothing read, so its badge counts that one reply; a mark that was
  // lost would count the first reply too.
  await plannerToggle.click();
  await expect(plannerToggle, "the Planner's row did not shut").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  const askedAgain = await createAgentMessage(KEPT_AGENT.planner, {
    body: "And the tests?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    askedAgain.id,
    { attempt: 1, body: "Green." },
    { id: KEPT_AGENT.planner, kind: "session" }
  );
  const plannerBadge = planner.getByRole("button", { name: /^New repl/ });
  await expect(plannerBadge, "the Planner's reply to a shut row").toHaveAccessibleName(
    "New reply 1 from Planner"
  );

  // What the page stores for the reader, found again after a reload.
  await page.reload();
  await expect(
    tester.getByRole("button", { name: "Unpin Tester" }),
    "the pin, after a reload"
  ).toBeVisible();
  await expect(titles, "the pinned order, after a reload").toHaveText(pinned);
  // The badge is drawn from the read state the reload loads, so its number is read only once
  // that state has answered: one reply unread, the one written after the mark above.
  await expect(plannerBadge, "the read mark, after a reload").toHaveAccessibleName(
    "New reply 1 from Planner"
  );

  // The bulk action: select-all over a narrowing, Clear selection, then two rows and a send.
  await pickAgentFilter(page, "role", "tester");
  await main.getByRole("checkbox", { exact: true, name: "Select all matching agents" }).check();
  await expect(
    main.getByText("3 of 3 matching selected", { exact: true }),
    "select-all"
  ).toBeVisible();
  await expectAgentGroups(
    page,
    ["Needs you (0)", "Active (0)", "Idle (2, 2 selected)", "Quiet (1, 1 selected)"],
    "select-all"
  );
  const composer = page.getByRole("region", { name: "Broadcast" });
  await expect(composer, "select-all drew no broadcast composer").toBeVisible();
  await main.getByRole("button", { exact: true, name: "Clear selection" }).click();
  await expect(composer, "Clear selection").toHaveCount(0);
  // The role chip, in the bar's line, or at the phone's width at the open panel's top (R8).
  if (phone) {
    await (await openFilterPanel(page, main)).getByRole("button", { name: REMOVE_CHIP }).click();
    await closeControlPanel(main);
  } else {
    await chips.click();
  }
  await expect(titles).toHaveText(pinned);
  await reviewer.getByRole("checkbox", { name: "Select Reviewer for broadcast" }).check();
  await tester.getByRole("checkbox", { name: "Select Tester for broadcast" }).check();
  await expect(
    main.getByText("2 of 5 matching selected", { exact: true }),
    "two rows' checkboxes"
  ).toBeVisible();
  await expect(
    composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" }),
    "the composer's count"
  ).toBeVisible();
  await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");
  await composer.getByRole("button", { name: "Send to 2" }).click();
  await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
  await expect
    .poll(
      async () =>
        (await getSentMessages())
          .filter((sent) => sent.message === "Report status.")
          .map((sent) => String(sent.target_session))
          .sort(),
      { message: "the broadcast sent no message to each selected session" }
    )
    .toEqual([KEPT_AGENT.reviewer, KEPT_AGENT.tester].sort());
});

// R6 on an issue's Artifacts tab. What the tab offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Field: the one text box, which keeps an artifact whose name or kind holds the words
//   (`features/artifacts/ArtifactsTab.tsx:243`, :281-289, :306-317) and says so when none does
//   (:333-338). Worked by a name, by a kind and by words nothing holds.
// - Sort: no control. The primary document first, then the newest written, then the id
//   (:269-280). Read as the rows' order, and again once an upload is the newest.
// - Grouping: none.
// - Counts: each row's `N versions` (:196-202) and the references' `References (N)` (:116-150).
//   Both read; the references opened and their rows counted against the number.
// - Views: the references, shut until opened (:121-127). Opened.
// - Bulk action: none; a row's own controls (copy, approval, Download, Remove, :211-231) act on
//   that row alone.
// - The two controls that add to the list, now the control bar's actions (R8): New document
//   (`features/artifacts/CreateDocument.tsx`) and Upload (`features/artifacts/ArtifactUpload.tsx`),
//   each an item of More below 640 px of the bar's width, as at the phone's width. Each used, and
//   what it added read in the list.
// - Keys: the tab's list answers none; `t a`, the issue page's binding that opens the tab
//   (`features/issue/IssuePage.tsx:153`, :324-330). Pressed.
// - Stored for the reader: nothing; the words were the tab's own state (:243), kept nowhere.
// SLOW(>=2s): four seeded documents, an upload and a new document, read in the tab.
test("k-keep: an issue's Artifacts tab keeps its narrowing, its order, its counts, its actions and its key @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const home = await createIssue({ project: "CORE", title: "Runbook home" });
  const runbook = await createIssueArtifact(home.key, {
    content: "# Runbook\n\nThe steps, in order.\n",
    name: "runbook.md",
  });
  const issue = await createIssue({
    project: "CORE",
    spec: `# Kept spec\n\nThe steps are dispatch://${home.key}/artifact/${runbook.artifact.slug}.\n`,
    title: "Artifacts kept",
  });
  const plan = await createIssueArtifact(issue.key, {
    content: "# Plan\n\nThe first draft.\n",
    name: "plan.md",
  });
  const notes = await createIssueArtifact(issue.key, {
    content: "# Notes\n\nKept as written.\n",
    name: "notes.md",
  });
  // The plan takes a second version after the notes were written, so it is the newer of the two.
  await editArtifact(
    plan.artifact.id,
    { ops: [{ find: "first draft", op: "replace", with: "second draft" }] },
    { as: "agent" }
  );
  await expect
    .poll(async () => ((await getArtifact(plan.artifact.id)).versions ?? []).length)
    .toBe(2);
  const spec = (await listIssueArtifacts(issue.key)).find((artifact) => artifact.primary);
  if (spec === undefined) throw new Error("the issue holds no primary document");

  // `t a`, from the issue's own page. On a phone the review panel opens over the page as a
  // dialog, which holds the keyboard, so it is shut first.
  await page.goto(`/issues/${issue.key}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  if (phone) {
    const reviewPanel = page.getByRole("button", { name: /review panel/i });
    if ((await expandedState("the review panel", reviewPanel)) === "open") {
      await reviewPanel.click();
    }
  }
  await page.locator("body").focus();
  await page.keyboard.press("t");
  await page.keyboard.press("a");
  await expect(page, "the t a key").toHaveURL(new RegExp(`/issues/${issue.key}/artifacts$`));
  const main = page.getByTestId("main-content");
  const rows = main.locator('[data-testid^="artifact-"]');
  const order = () =>
    rows.evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-testid") ?? ""));
  await expect(rows).toHaveCount(3);
  if (phone) await assertPageFits(page);

  // The order, and each row's count of versions.
  expect(await order(), "the order: the primary document, then the newest written").toEqual([
    `artifact-${spec.slug}`,
    `artifact-${plan.artifact.slug}`,
    `artifact-${notes.artifact.slug}`,
  ]);
  await expect(rows.first().getByText("Primary", { exact: true })).toBeVisible();
  await expect(
    main.getByTestId(`artifact-${plan.artifact.slug}`),
    "a row's count of versions"
  ).toContainText("doc · 2 versions");
  await expect(main.getByTestId(`artifact-${notes.artifact.slug}`)).toContainText(
    "doc · 1 version"
  );

  // The references' count, which is the number of rows they open on.
  const references = main.getByText(/^References \(\d+\)$/);
  await expect(references, "the references' count").toBeVisible();
  const counted = Number(/\((\d+)\)/.exec((await references.textContent()) ?? "")?.[1]);
  await references.click();
  const referenced = main.getByRole("region", { name: "References" });
  await expect(referenced.getByRole("listitem"), "the references' count").toHaveCount(counted);
  await expect(referenced.getByRole("link", { exact: true, name: "runbook.md" })).toBeVisible();

  // Upload, which adds a file - a kind the text box narrows by.
  await page.getByLabel("Upload artifact").setInputFiles({
    buffer: Buffer.from("one line\n"),
    mimeType: "text/plain",
    name: "trace.log",
  });
  // The staged file's own Upload, under the bar: the bar offers none while a file is staged.
  await main.getByRole("button", { exact: true, name: "Upload" }).click();
  const trace = main.getByTestId("artifact-trace-log");
  await expect(trace, "Upload").toContainText("file · 1 version");
  expect(await order(), "the order, with the upload the newest written").toEqual([
    `artifact-${spec.slug}`,
    "artifact-trace-log",
    `artifact-${plan.artifact.slug}`,
    `artifact-${notes.artifact.slug}`,
  ]);

  // The text box, by a name, by a kind, and by words no artifact holds.
  const filter = control(controlRow(main), CONTROL.searchBox);
  await filter.fill("plan");
  await expect(rows, "the text box, by a name").toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", `artifact-${plan.artifact.slug}`);
  await filter.fill("file");
  await expect(rows, "the text box, by a kind").toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", "artifact-trace-log");
  await filter.fill("nothing holds this");
  await expect(rows).toHaveCount(0);
  await expect(
    main.getByText("No artifacts match this filter.", { exact: true }),
    "the text box's empty state"
  ).toBeVisible();
  await filter.fill("");
  await expect(rows).toHaveCount(4);

  // New document, which opens what it made; the tab then lists it.
  await (await barAction(page, "New document")).click();
  const form = page.getByRole("form", { name: "New document" });
  await form.getByRole("textbox", { exact: true, name: "Title" }).fill("Drafted");
  await form.getByRole("button", { exact: true, name: "Create" }).click();
  await expect(page, "New document").toHaveURL(
    new RegExp(`/issues/${issue.key}/artifacts/drafted`)
  );
  await page.goto(`/issues/${issue.key}/artifacts`);
  await expect(rows, "New document's row").toHaveCount(5);
  await expect(rows.nth(1), "New document's row").toContainText("Drafted");
});

/** The search bar's keep row's seed: four issues and a comment holding `harbour`, over two
 *  projects, two authors and two statuses; a project document holding it too, whose paragraph
 *  the writing cursor goes into; and twenty-two `beacon` issues, more than the bar lists at once
 *  (`searchDefaultLimit`, `packages/envoy/internal/dispatch/api/search.go`). */
async function seedKeptSearch(): Promise<{ documentSlug: string; rollout: string }> {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  const rollout = await createIssue({ project: "CORE", title: "Harbour rollout" });
  await createComment(rollout.key, { body: "The harbour tide decides the day." });
  const lights = await createIssue({ project: "CORE", title: "Harbour lights" });
  await patchIssue(lights.key, { status: "in_progress" });
  await createIssue({ project: "CORE", title: "Harbour pilot" }, { login: "bob" });
  await createIssue({ project: "OPS", title: "Harbour crane" }, { login: "bob" });
  const document = await createProjectDocument("CORE", {
    content: "# Harbour notes\n\nThe sextant sights the horizon.\n",
    name: "Harbour notes",
  });
  for (let index = 1; index <= 22; index++) {
    await createIssue({ project: "CORE", title: `Beacon ${index}` });
  }
  return { documentSlug: document.artifact.slug, rollout: rollout.key };
}

/** The owners the bar's hits are grouped under, each as its group is named: `KEY: title`. */
function hitOwners(bar: Locator): Promise<string[]> {
  return bar
    .locator(
      '#search-results [id^="search-group-issue:"], #search-results [id^="search-group-document:"]'
    )
    .evaluateAll((heads) => heads.map((head) => head.getAttribute("aria-label") ?? "").sort());
}

// R6 on the search bar. What the bar offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Fields: the words in the box (`features/search/SearchPalette.tsx:724-742`); Project
//   (`features/search/SearchFilterBar.tsx:149-156`), Status (:157-164), Author, taken on Enter
//   (:165-181), and Updated since (:182-198), each a removable chip once set (:137-148,
//   `features/search/search-filters.ts:65-89`) and each a parameter of the search (:91-104). Each
//   is set, the hits it leaves read, and its chip taken off again.
// - Sort: none to choose; the hits come in the order the search ranks them.
// - Grouping: the hits under the issue or document they belong to (`SearchPalette.tsx:472`,
//   :780-851, `features/search/search-model.ts:25-44`); the Recent, Actions and Projects groups
//   above them (:546-574), and More actions, This issue's agent and New issue below (:575-596).
//   The hits' groups, Recent, Actions, Projects and New issue are read. More actions holds what
//   the page's keys offer, and This issue's agent only a live session holding the issue.
// - Count: `Showing the best N of M matches` (:891-896). Read.
// - Views: the recent items while the box is empty, the hits while it holds words (:507-524).
//   Both read.
// - Bulk action: none.
// - Keys: in the box, ArrowDown and ArrowUp move the highlight (:685-700) and Enter opens it
//   (:701-704); Tab narrows the scope to the block being written in and Backspace on an empty
//   box widens it to the project (`features/search/useBarScope.tsx:72-90`); `$mod+k` shuts the
//   bar (`SearchPalette.tsx:433-441`) and so does Escape (`features/shell/useDialog.ts:124-137`);
//   and the three global bindings that open it, `$mod+k`, `/` and `g p` (`app.tsx:547-597`).
//   Each pressed and what it did read.
// - Stored for the reader: the recent items, in local storage under `search.recents`
//   (`features/search/recents.ts:26-87`). Found again after a reload.
// SLOW(>=2s): twenty-six seeded issues and a document, the bar opened six times over four pages.
test("k-keep: the search bar keeps its narrowings, its groups, its count, its recent items and its keys @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  const seed = await seedKeptSearch();
  const bar = page.getByRole("dialog", { name: "Search" });
  const box = bar.getByRole("combobox", { name: "Search" });
  const scope = bar.getByTestId("search-scope");
  // Four issues, and the project document, which is grouped under its own name.
  const harbour = [
    "CORE-1: Harbour rollout",
    "CORE-2: Harbour lights",
    "CORE-3: Harbour pilot",
    "CORE: Harbour notes",
    "OPS-1: Harbour crane",
  ].sort();

  // The three keys that open the bar, and the two that shut it.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  for (const [keys, shut] of [
    [["/"], "Escape"],
    [["g", "p"], "Control+k"],
    [["Control+k"], "Escape"],
  ] as const) {
    await page.locator("body").focus();
    for (const key of keys) await page.keyboard.press(key);
    await expect(bar, `the ${keys.join(" ")} key`).toBeVisible();
    await expect(box).toBeFocused();
    await page.keyboard.press(shut);
    await expect(bar, `the ${shut} key in the bar`).toHaveCount(0);
  }

  // The words, and the hits grouped under what they belong to.
  await page.keyboard.press("Control+k");
  await box.fill("harbour");
  await expect.poll(() => hitOwners(bar), { message: "the words' hits" }).toEqual(harbour);
  await expect(
    bar.getByRole("group", { name: "CORE-1: Harbour rollout" }).getByRole("option"),
    "an issue's group, holding the issue and its comment"
  ).toHaveCount(2);
  if (phone) await assertPageFits(page);

  // The four narrowings, each from the one panel and each a chip.
  const chips = control(controlRow(bar), CONTROL.chip);
  const filters = page.getByRole("dialog", { name: "Filter" });
  const tomorrow = await page.evaluate(() => {
    const day = new Date();
    day.setDate(day.getDate() + 1);
    const pad = (value: number) => String(value).padStart(2, "0");
    return `${day.getFullYear()}-${pad(day.getMonth() + 1)}-${pad(day.getDate())}`;
  });
  const pick = async (list: string, option: string) => {
    await filters.getByRole("button", { exact: true, name: list }).click();
    await page
      .getByRole("listbox", { name: `${list} options` })
      .getByRole("option", { exact: true, name: option })
      .click();
  };
  for (const [narrowing, field, set, left] of [
    [
      "Project",
      filters.getByRole("button", { exact: true, name: "Project" }),
      () => pick("Project", "OPS"),
      ["OPS-1: Harbour crane"],
    ],
    [
      "Status",
      filters.getByRole("button", { exact: true, name: "Status" }),
      () => pick("Status", "In progress"),
      ["CORE-2: Harbour lights"],
    ],
    [
      "Author",
      filters.getByRole("searchbox", { name: "Author" }),
      async () => {
        await filters.getByRole("searchbox", { name: "Author" }).fill("bob");
        await page.keyboard.press("Enter");
      },
      ["CORE-3: Harbour pilot", "OPS-1: Harbour crane"],
    ],
    [
      "Updated since",
      filters.getByLabel("Updated since"),
      () => filters.getByLabel("Updated since").fill(tomorrow),
      [],
    ],
  ] as const) {
    await openFilterPanel(page, bar);
    await expect(field, `the ${narrowing} narrowing`).toBeVisible();
    await set();
    // The chip stands in the bar where the bar is 640 px wide or wider - in its line, or behind
    // its one `+N more` where it does not fit whole; below it, as at the phone's width, Filter's
    // badge stands for it and the open panel lists it at its top (R8).
    if (!phone) await closeControlPanel(bar);
    const named = (all: Locator) => all.filter({ hasText: new RegExp(`^${narrowing} `) });
    const chip = phone
      ? named(filters.getByRole("button", { name: REMOVE_CHIP }))
      : await barChip(page, controlRow(bar), named);
    await expect(chip, `the ${narrowing} narrowing's chip`).toHaveCount(1);
    await expect
      .poll(() => hitOwners(bar), { message: `the ${narrowing} narrowing` })
      .toEqual([...left].sort());
    await chip.click();
    await expect(chip).toHaveCount(0);
    if (!phone) await expect(chips, `the ${narrowing} narrowing's chip stayed`).toHaveCount(0);
    if (phone) await closeControlPanel(bar);
    await expect.poll(() => hitOwners(bar)).toEqual(harbour);
  }

  // The count of what the bar holds back, and the projects the words name.
  await box.fill("beacon");
  await expect(
    bar.getByText("Showing the best 20 of 22 matches", { exact: true }),
    "the count of matches"
  ).toBeVisible();
  await box.fill("ops");
  await expect(
    bar.getByRole("group", { name: "Projects" }).getByRole("option"),
    "the Projects group"
  ).toContainText(["Ops"]);

  // The arrows move the highlight over the hits, and Enter opens the one it is on.
  await box.fill("harbour");
  await expect.poll(() => hitOwners(bar)).toEqual(harbour);
  const first = await box.getAttribute("aria-activedescendant");
  await page.keyboard.press("ArrowDown");
  await expect(box, "the ArrowDown key").not.toHaveAttribute("aria-activedescendant", first ?? "");
  await page.keyboard.press("ArrowUp");
  await expect(box, "the ArrowUp key").toHaveAttribute("aria-activedescendant", first ?? "");
  await page.keyboard.press("Enter");
  await expect(bar, "the Enter key").toHaveCount(0);
  await expect(page, "the Enter key").toHaveURL(/\/(issues|projects)\//);

  // Over an issue: the page's actions on an empty box, Backspace widening the scope from the
  // issue to its project, and the new issue the bar offers words nothing holds, which it files
  // in the page's project.
  await page.goto(`/issues/${seed.rollout}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(scope).toHaveText(new RegExp(`Acting on ${seed.rollout}`));
  await expect(
    bar.getByRole("group", { name: "Actions" }).getByRole("option"),
    "the Actions group"
  ).not.toHaveCount(0);
  await page.keyboard.press("Backspace");
  await expect(scope, "the Backspace key").toHaveText(/Acting on the project CORE/);
  await box.fill("zebrafish");
  await expect(
    bar.getByRole("group", { name: "New issue" }).getByRole("option", {
      name: /Make a new issue from .zebrafish./,
    }),
    "the New issue group"
  ).toHaveCount(1);
  await page.keyboard.press("Escape");

  // Tab narrows it to the block the writing cursor is in.
  await page.goto(`/projects/CORE/documents/${seed.documentSlug}`);
  await expect(documentEditor(page)).toContainText("The sextant sights the horizon.");
  await placeCaret(page, "after", "sextant");
  await expect(documentEditor(page)).toBeFocused();
  await page.keyboard.press("Control+k");
  await expect(scope).toHaveText(/Harbour notes/);
  await page.keyboard.press("Tab");
  await expect(scope, "the Tab key").toHaveText(/The sextant sights the horizon/);
  await page.keyboard.press("Escape");

  // The recent items, kept in the browser and listed again after a reload.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(box).toHaveValue("");
  const recent = bar.getByRole("group", { name: "Recent" });
  await expect(
    recent.getByRole("option", { name: new RegExp(seed.rollout) }),
    "a recent issue, after a reload"
  ).toHaveCount(1);
  await expect(
    recent.getByRole("option", { name: /Harbour notes/ }),
    "a recent document, after a reload"
  ).toHaveCount(1);
  expect(
    await page.evaluate(() => localStorage.getItem("dispatch.search.recents:alice")),
    "the recent items' stored value"
  ).toContain(`/issues/${seed.rollout}`);
});

/** Shuts the surface's display menu by pressing its trigger again. Escape would be the drawer's
 *  too once the focus has left the menu, and would shut the drawer with it. Where the bar holds
 *  the display menu among **More**'s items (the drawer's, below 640 px), its item is pressed
 *  again and **More**, which stays open under the menu, is shut after it. */
async function shutDisplay(root: Locator): Promise<void> {
  const trigger = control(controlRow(root), CONTROL.displayMenu);
  // Every caller opened the display menu with `openDisplay` just before this, which waited for
  // it, so the bar has drawn its trigger in its line or among More's items, as its width gives.
  if ((await trigger.count()) === 0) {
    await shutDisplayMenu(root.page(), root);
    return;
  }
  if ((await expandedState("the surface's View menu", trigger)) === "open") await trigger.click();
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
}

// R6 on the Inbox drawer. At the merge base, dccaf8369449 (`fork_point(main@origin | @)`), the
// drawer is the Inbox's own list in a dialog, its narrowing its own state rather than the
// address (`features/inbox/InboxDrawer.tsx:39`, :68-74), so it offered what the Inbox offered.
// Paths under `web/src/`, and how the row drives each:
// - Fields: priority, urgency, type and project (`features/inbox/InboxBar.tsx:227-249`), the
//   assignee control (:250-257, `features/inbox/AssigneeFilter.tsx:26-87`) and the search box
//   (`features/inbox/Inbox.tsx:1513-1522`); a chip for each value that is on
//   (`InboxBar.tsx:149-181`, :260-275); and Clear filters (:183-187, :276-283). Each put on, the
//   rows it leaves read, and its chip taken off; Clear filters read for what it takes off and
//   what it leaves.
// - Sorts: newest, priority, urgency, type, project and assignee (`features/inbox/ViewMenu.tsx:169-180`,
//   `features/refs/routes.ts:76`, :84). Each chosen and the order read.
// - Groupings: the five fields, Waiting on and None (`ViewMenu.tsx:181-192`, `routes.ts:91`). Each
//   chosen and its bands read.
// - Counts: each value's rows in its field's list, taken under the other narrowings
//   (`InboxBar.tsx:133-141`, :231, :240-241, `AssigneeFilter.tsx:74-77`); each band's
//   (`features/inbox/InboxGroups.tsx:53`); what waits on the reader, `Blocked on you: N items`
//   (`Inbox.tsx:1558-1563`, `features/inbox/BlockedOnYou.tsx:111-137`); and the selection's
//   `N selected` (`features/inbox/BulkBar.tsx:68-82`). Each read.
// - Views: the Inbox and Dismissed (`ViewMenu.tsx:193-204`), and the thread order (:205-216). Each
//   chosen and read.
// - Bulk actions: Snooze selected asks, Dismiss and Move to Inbox selected threads, Select all
//   and Clear selection (`BulkBar.tsx:98-147`); and Mark all read (`InboxBar.tsx:201-212`). Each
//   pressed and its write read.
// - Keys, on the drawer's own dialog scope (`Inbox.tsx:1162-1416`): `j` and `k` (:1167-1182),
//   the arrows (:1183-1189), Enter (:1190-1221), 1-9 (:1222-1231), `o` (:1232-1238), `y`
//   (:1239-1247), Escape back to the row (:1248-1280), `l` (:1281-1309), which stands down in a
//   list with no reading pane, `x` (:1310-1321), `e` (:1322-1338), `h` (:1339-1362), Escape out
//   of the bulk picker (:1363-1374), Escape clearing the selection (:1375-1390), `f`
//   (:1391-1401) and Escape out of the search box (:1402-1415); the dialog's own Escape
//   (`features/shell/useDialog.ts:124-137`); and `i`, the global binding that opens the drawer
//   (`app.tsx:598-606`). Each pressed and what it did read.
// - Stored for the reader, in local storage and read by the page and the drawer alike: the thread
//   order (`features/threads/thread-order.ts:23-30`) and the folded bands
//   (`features/inbox/preferences.ts:41-62`). Each set, and found again after a reload.
// SLOW(>=2s): eleven seeded rows worked through every field, sort, grouping, key and bulk action.
test("k-keep: the Inbox drawer keeps every field, sort, grouping, count, view, bulk action, Blocked on you, Clear filters, key and stored choice it had @phone", async ({
  page,
}, testInfo) => {
  // Measured in an issue pod (two cores), one worker: the row took 46.4 s in Chromium and 41.2 s
  // in the iPhone project, against the config's 60 s, and with the `e` binding taken out it ran
  // out of that budget while its 15 s expectation was naming the key. Twice the budget keeps a
  // step that fails late naming itself rather than the budget.
  test.setTimeout(120_000);
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  const copied = await recordClipboard(page);
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const peek = drawer(page);
  const listed = peek.locator("[data-inbox-row]");
  // The drawer's bar is narrower than 640 px at every window this row drives but the one it
  // widens for Clear filters, so its chips stand at the top of its Filter panel rather than in its
  // line (R8): each narrowing is read, and taken off, there. `lineChips` are the line's.
  const chips = page
    .getByRole("dialog", { name: "Filter" })
    .getByRole("button", { name: REMOVE_CHIP });
  const lineChips = control(controlRow(peek), CONTROL.chip);
  const drawerTrigger = control(controlRow(peek), CONTROL.panelTrigger);
  const rowOf = (id: string) => peek.locator(`[data-inbox-row="${id}"]`);
  const idsOf = (match: (row: ViewRow) => boolean) =>
    rows
      .filter(match)
      .map((row) => row.id)
      .sort();
  const seeded = (match: (row: ViewRow) => boolean): ViewRow => {
    const found = rows.find(match);
    if (found === undefined) throw new Error("the seed holds no such row");
    return found;
  };
  const alices = idsOf((row) => row.assignee === "alice");

  // `i`, over the project List, and the dialog's own Escape.
  await openIssuesTab(page, "CORE", "List");
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("i");
  await expect(peek, "the i key").toBeVisible();
  await expect(listed).toHaveCount(alices.length);
  expect((await drawnIds(peek)).sort(), "the drawer as it opens, on the reader's own").toEqual(
    alices
  );
  await page.keyboard.press("Escape");
  await expect(peek, "the drawer's own Escape").toHaveCount(0);
  await peekTrigger(page).click();
  await expect(listed).toHaveCount(alices.length);
  if (phone) await assertPageFits(page);

  // The assignee control, with the count beside each person, cleared to everyone.
  await openFilterPanel(page, peek);
  const assignee = assigneeFilter(page);
  await assignee.click();
  const people = assigneeOptions(page);
  await expect(
    people.getByRole("option", { exact: true, name: "bob, 1 rows" }),
    "the count beside a person"
  ).toHaveCount(1);
  await people
    .getByRole("option", { exact: true, name: `alice, you, ${alices.length} rows` })
    .click();
  await assignee.click();
  await closeControlPanel(peek);
  await expect(listed, "the assignee control").toHaveCount(rows.length);
  await openFilterPanel(page, peek);
  await expect(chips.filter({ hasText: "Assignee: everyone" })).toHaveCount(1);
  await closeControlPanel(peek);

  // The four fields, each with its chip, which the Filter panel lists and takes off.
  for (const [field, value, chip, matches] of [
    ["type", /^Question,/, "Type: Question", (row: ViewRow) => row.type === "question"],
    ["priority", /^P0,/, "Priority: P0", (row: ViewRow) => row.priority === "P0"],
    ["urgency", /^Blocking,/, "Urgency: Blocking", (row: ViewRow) => row.urgency === "blocking"],
    ["project", /^CORE,/, "Project: CORE", (row: ViewRow) => row.project === "CORE"],
  ] as const) {
    await pickInboxField(page, field, value, peek);
    const expected = idsOf(matches);
    await expect(listed, `the drawer's ${field} field`).toHaveCount(expected.length);
    expect((await drawnIds(peek)).sort(), `the drawer's ${field} field`).toEqual(expected);
    await openFilterPanel(page, peek);
    await expect(chips.filter({ hasText: chip }), `the ${field} field's chip`).toHaveCount(1);
    await chips.filter({ hasText: chip }).click();
    await closeControlPanel(peek);
    await expect(listed).toHaveCount(rows.length);
  }

  // The count beside each value of a field is taken under the narrowing on another.
  await pickInboxField(page, "priority", /^P0,/, peek);
  await openFilterPanel(page, peek);
  const typeTrigger = page.getByRole("button", { name: "Filter by type" });
  await typeTrigger.click();
  await expect(
    page.getByRole("listbox", { name: "Type options" }).getByRole("option", { name: /^Question,/ }),
    "the Type counts under the priority narrowing"
  ).toHaveAccessibleName(
    `Question, ${idsOf((row) => row.priority === "P0" && row.type === "question").length} rows`
  );
  await typeTrigger.click();
  await closeControlPanel(peek);
  await openFilterPanel(page, peek);
  await chips.filter({ hasText: "Priority: P0" }).click();
  await closeControlPanel(peek);
  await expect(listed).toHaveCount(rows.length);

  // The six sorts, under no banding, and the seven groupings. Each sort's order is the client's
  // (`sortRows`, `features/inbox/grouping.ts`), the assignee's as much as the others', which
  // `sortedIds` models from the seed.
  await displayChoice(await openDisplay(page, peek), "Group by", "none").click();
  for (const sort of ["newest", "priority", "urgency", "type", "project", "assignee"] as const) {
    const menu = await openDisplay(page, peek);
    await displayChoice(menu, "Sort by", sort).click();
    await expect(displayChoice(menu, "Sort by", sort)).toBeChecked();
    await expect
      .poll(() => drawnIds(peek), { message: `the drawer's ${sort} sort` })
      .toEqual(sortedIds(rows, sort, at));
  }
  for (const group of [
    "priority",
    "urgency",
    "type",
    "project",
    "assignee",
    "waiting-on",
    "none",
  ] as const) {
    const menu = await openDisplay(page, peek);
    await displayChoice(menu, "Group by", group).click();
    const expected =
      group === "none"
        ? []
        : group === "waiting-on"
          ? [`Waiting on a person (${rows.length - 1})`, "Waiting on agents (1)"]
          : valuesOf(rows, group).map(
              (value) => `${bandLabel(group, value)} (${countsOf(rows, group).get(value)})`
            );
    await expect
      .poll(() => bandHeadings(peek), { message: `the drawer's ${group} grouping` })
      .toEqual(expected);
  }
  await displayChoice(await openDisplay(page, peek), "Sort by", "newest").click();
  await shutDisplay(peek);
  await expect.poll(() => drawnIds(peek)).toEqual(sortedIds(rows, "newest", at));

  // The keys over the rows: `f` and the Escape out of the box, `j`, `k` and the arrows.
  const order = sortedIds(rows, "newest", at);
  // The drawer's search box, which stands in its header line beside its title.
  const search = peek.getByRole("searchbox", { name: "Search asks" });
  await page.keyboard.press("f");
  await expect(search, "the f key").toBeFocused();
  await page.keyboard.press("Escape");
  await expect(rowOf(order[0] ?? ""), "the Escape key, out of the search box").toBeFocused();
  await page.keyboard.press("j");
  await expect(rowOf(order[1] ?? ""), "the j key").toBeFocused();
  await page.keyboard.press("k");
  await expect(rowOf(order[0] ?? ""), "the k key").toBeFocused();
  await page.keyboard.press("ArrowDown");
  await expect(rowOf(order[1] ?? ""), "the ArrowDown key").toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(rowOf(order[0] ?? ""), "the ArrowUp key").toBeFocused();

  // The keys on one question: Enter into its answer, Escape back, `h` to its Snooze, `y`, and
  // `l`, which has no reading pane to go to here.
  const ledger = seeded((row) => row.type === "question" && row.priority === "P0");
  const ask = rowOf(ledger.id);
  await ask.focus();
  await page.keyboard.press("Enter");
  await expect(ask.locator("[data-ask-answer]"), "the Enter key").toBeFocused();
  await page.keyboard.press("Escape");
  await expect(ask, "the Escape key, back to the row").toBeFocused();
  await page.keyboard.press("h");
  await expect(ask.locator("[data-inbox-snooze]"), "the h key").toBeFocused();
  await page.keyboard.press("Escape");
  await expect(ask).toBeFocused();
  await page.keyboard.press("y");
  await expect
    .poll(async () => (await copied()).join(" "), { message: "the y key" })
    .toContain(ledger.issueKey ?? "");
  await page.keyboard.press("l");
  await expect(ask, "the l key moved the focus with no reading pane to take it").toBeFocused();

  // A number key picks that option of the decision in hand.
  const decision = rowOf(seeded((row) => row.type === "decision").id);
  await decision.focus();
  await page.keyboard.press("1");
  await expect(decision.locator("[data-ask-option]").first(), "the 1 key").toBeChecked();

  // `x`, `h` with a row marked, and the three Escapes out: the bulk picker, the row, the marks.
  const bar = control(peek, CONTROL.selectionBar);
  await ask.focus();
  await page.keyboard.press("x");
  await expect(bar, "the x key").toContainText("1 selected");
  await page.keyboard.press("h");
  await expect(
    listTrigger(peek, "Snooze selected asks"),
    "the h key, with a row marked"
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(ask, "the Escape key, out of the bulk picker").toBeFocused();
  await page.keyboard.press("Escape");
  await expect(ask).not.toBeFocused();
  await page.keyboard.press("Escape");
  await expect(bar, "the Escape key that clears the marks").toHaveCount(0);
  await expect(peek).toBeVisible();

  // `Shift+J` carries the range from the row marked last to the next row, `Shift+K` back again,
  // and `Shift+X` takes every row from the row marked last to the row in hand.
  await walkRowRange(page, rowOf, order, bar);
  // `n` opens the compose dialog over the drawer, and its Escape shuts it and no more.
  await page.keyboard.press("n");
  const compose = page.getByRole("dialog", { name: "New message" });
  await expect(compose, "the n key").toBeVisible();
  await page.keyboard.press("Escape");
  await expect(compose).toHaveCount(0);
  await expect(peek).toBeVisible();

  // Mark all read, from the bar's More, while threads are unread. The drawer's bar is narrow, so
  // More stays for View, and holds no Mark all read once nothing is left unread.
  const more = controlRow(peek).getByRole("button", { exact: true, name: "More" });
  const markAll = page
    .getByRole("menu", { name: "More" })
    .getByRole("menuitem", { exact: true, name: "Mark all read" });
  await more.click();
  await markAll.click();
  await more.click();
  await expect(page.getByRole("menu", { name: "More" })).toBeVisible();
  await expect(markAll, "Mark all read is offered over a list with nothing unread").toHaveCount(0);
  await more.click();
  await expect
    .poll(async () => (await meInboxThreads({ state: "unread" })).rows.length, {
      message: "Mark all read",
    })
    .toBe(0);

  // The selection bar: two marks, Select all, Clear selection.
  const clock = seeded((row) => row.type === "question" && row.priority === "P3");
  const comment = seeded((row) => row.type === "reply" && row.priority === "P3");
  const message = seeded((row) => row.type === "reply" && row.project === "MIRROR");
  await rowOf(clock.id).getByRole("checkbox").first().check();
  await rowOf(comment.id).getByRole("checkbox").first().check();
  await expect(bar, "two marks").toContainText("2 selected");
  await bar.getByRole("button", { exact: true, name: "Select all" }).click();
  await expect(bar, "Select all").toContainText("Every row selected");
  await bar.getByRole("button", { exact: true, name: "Clear selection" }).click();
  await expect(bar, "Clear selection").toHaveCount(0);

  // Snooze selected asks, Dismiss selected threads, and `e` on a thread.
  await rowOf(clock.id).getByRole("checkbox").first().check();
  await pickFromList(listTrigger(bar, "Snooze selected asks"), "Tomorrow");
  await expect(rowOf(clock.id), "Snooze selected asks").toHaveCount(0);
  await expect
    .poll(
      async () => (await listOpenAsks()).find((row) => row.id === clock.id)?.snoozed_until ?? null,
      { message: "Snooze selected asks wrote no snooze" }
    )
    .not.toBeNull();
  await rowOf(comment.id).getByRole("checkbox").first().check();
  await bar.getByRole("button", { exact: true, name: "Dismiss selected threads" }).click();
  await expect(rowOf(comment.id), "Dismiss selected threads").toHaveCount(0);
  await rowOf(message.id).focus();
  await page.keyboard.press("e");
  await expect(rowOf(message.id), "the e key").toHaveCount(0);
  const dismissed = async () =>
    (await meInboxThreads({ state: "dismissed" })).rows
      .map((thread) => `reply:${thread.thread_id}`)
      .sort();
  await expect
    .poll(dismissed, { message: "the two dismissals" })
    .toEqual([comment.id, message.id].sort());

  // The Dismissed view, and Move to Inbox from it.
  await displayChoice(await openDisplay(page, peek), "Show", "dismissed").click();
  await shutDisplay(peek);
  await expect
    .poll(async () => (await drawnIds(peek)).sort(), { message: "the Dismissed view" })
    .toEqual([comment.id, message.id].sort());
  await rowOf(comment.id).getByRole("checkbox").first().check();
  await bar.getByRole("button", { exact: true, name: "Move to Inbox selected threads" }).click();
  await expect(rowOf(comment.id), "Move to Inbox selected threads").toHaveCount(0);
  await expect.poll(dismissed, { message: "Move to Inbox" }).toEqual([message.id]);
  await displayChoice(await openDisplay(page, peek), "Show", "inbox").click();
  await shutDisplay(peek);
  await expect(rowOf(comment.id)).toHaveCount(1);

  // What waits on the reader, counted on the switch's Blocked on you, and choosing it narrowing
  // the drawer's list to exactly that, with no chip.
  const sections = controlRow(peek).getByRole("group", { name: "Inbox section" });
  const blocked = sections.getByRole("button", { name: /^Blocked on you: \d+ items?\b/ });
  await expect(blocked, "the count of what waits on the reader").toBeVisible();
  // The name was waited for above until it carried a number, which the part draws only once
  // every count it adds up has answered.
  const waiting = Number(
    /: (\d+) item/.exec((await blocked.getAttribute("aria-label")) ?? "")?.[1]
  );
  const address = page.url();
  await blocked.click();
  await expect(blocked).toHaveAttribute("aria-pressed", "true");
  await expect(listed, "the count of what waits on the reader").toHaveCount(waiting);
  // The switch's choice draws no chip, and Filter does not count it.
  await expect(drawerTrigger, "Blocked on you drew a chip").toHaveAccessibleName("Filter");

  // Under Blocked on you a field's narrowing stands beside it, and Clear filters takes off the
  // fields and the assignee and leaves the words, the sort, the grouping and the switch's choice
  // (ST7 rows 5, 6 and 11). The drawer's bar draws Clear filters only where it is 640 px wide or
  // wider, which it is where the drawer is as wide as a window below 768 px, so the window is
  // that wide for it.
  await pickInboxField(page, "type", /^Question,/, peek);
  await expect(blocked, "a field's narrowing took Blocked on you off").toHaveAttribute(
    "aria-pressed",
    "true"
  );
  const size = page.viewportSize();
  await page.setViewportSize({ height: 900, width: 720 });
  await search.fill("rule");
  const menu = await openDisplay(page, peek);
  for (const [section, value] of [
    ["Sort by", "priority"],
    ["Group by", "type"],
  ] as const) {
    await displayChoice(menu, section, value).click();
  }
  await shutDisplay(peek);
  await controlRow(peek).getByRole("button", { exact: true, name: "Clear filters" }).click();
  await expect(lineChips, "Clear filters left a narrowing on").toHaveCount(0);
  await expect(drawerTrigger, "Clear filters left a narrowing on").toHaveAccessibleName("Filter");
  await expect(search, "Clear filters took the words").toHaveValue("rule");
  await expect(blocked, "Clear filters took Blocked on you off").toHaveAttribute(
    "aria-pressed",
    "true"
  );
  // The Dismissed view takes the switch's choice off, and no switch is drawn in it.
  await displayChoice(await openDisplay(page, peek), "Show", "dismissed").click();
  await shutDisplay(peek);
  await expect(sections, "the switch is drawn in the Dismissed view").toHaveCount(0);
  expect(page.url(), "the drawer wrote its choices into the page's address").toBe(address);
  if (size !== null) await page.setViewportSize(size);
  const kept = await openDisplay(page, peek);
  for (const [section, value] of [
    ["Sort by", "priority"],
    ["Group by", "type"],
    ["Show", "dismissed"],
  ] as const) {
    await expect(
      displayChoice(kept, section, value),
      `Clear filters took the ${section}`
    ).toBeChecked();
  }

  // The thread order and a folded band, stored for the reader and found again after a reload.
  await displayChoice(kept, "Show", "inbox").click();
  await displayChoice(kept, "Group by", "priority").click();
  await displayChoice(kept, "Thread order", "newest").click();
  await shutDisplay(peek);
  await search.fill("");
  const p0 = peek.getByTestId("inbox-list").locator("h2 button").filter({ hasText: /^P0 / });
  await p0.click();
  await expect(p0).toHaveAttribute("aria-expanded", "false");
  await page.reload();
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  await peekTrigger(page).click();
  await expect(listed.first()).toBeVisible();
  await expect(p0, "the folded band, after a reload").toHaveAttribute("aria-expanded", "false");
  await expect(
    displayChoice(await openDisplay(page, peek), "Thread order", "newest"),
    "the thread order, after a reload"
  ).toBeChecked();
  await shutDisplay(peek);

  // `o` opens the row's issue, which takes the reader out of the drawer.
  const mirror = seeded((row) => row.type === "question" && row.priority === "P1");
  await rowOf(mirror.id).focus();
  await page.keyboard.press("o");
  await expect(page, "the o key").toHaveURL(new RegExp(`/issues/${mirror.issueKey}`));
  await expect(peek).toHaveCount(0);
});

/** A `class` attribute's tokens as a sorted set, so neither their order nor a repeat is read. */
function classTokens(text: string): string[] {
  return [...new Set(text.split(/\s+/).filter((token) => token !== ""))].sort();
}

/** The classes of the project page's `Blocked on you · N` pill at
 *  6ef60ff0ceeba4ccaf285f06f5bbe9b859b2e4d8, the merge base when they were read, as a sorted set
 *  of tokens, so neither their order nor a repeat is read. That commit's `web/dist` was served by
 *  `DISPATCH_E2E_SERVER_WEB_DIST` (e2e/run-server.sh) over the row below's seed, and the pill's
 *  `textContent` and its `href` and `class` attributes were read at 1440x900 on
 *  `/projects/CORE/issues`: they answered `Blocked on you · 3`, `/` and the `class` these tokens
 *  make. Its colours are written by the three roles that base's `BlockedOnYou.tsx` drew them
 *  with, because no file here may write a colour utility of its own
 *  (`web/src/theme/no-raw-colors.test.ts`); `theme/classes.ts` gives each the value the base
 *  read. */
const PILL_CLASSES_AT_THE_BASE = classTokens(
  [
    "order-3 shrink-0 md:order-5 inline-flex min-h-11 items-center rounded-full px-3 text-xs",
    "font-medium md:min-h-9 md:px-2",
    theme.surfaceMutedStrongBg,
    theme.textSecondaryOnSurface,
    theme.textSecondaryHoverToPrimary,
  ].join(" ")
);

// SLOW(>=2s): a seeded inbox of asks, threads and one credential request, read through the
// switch, the Dismissed view, an agent narrowing and the project page.
// k-needs-you: the banner stays off; the switch's Blocked on you carries its number and narrows
// the list to what waits on the reader, keeping the credential requests; All takes it off; the
// Dismissed view and an agent narrowing take it out of the address, and no switch is drawn beside
// either; with nothing waiting the part shows no number; the project page's pill is unchanged.
test("k-needs-you: Blocked on you carries what the banner carried, narrows the Inbox to what waits on the reader, and leaves the address under the Dismissed view and an agent narrowing", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  // Three asks waiting on Alice, one waiting on Bob, and one handed back to its agent.
  const mine: { id: string; issue: string }[] = [];
  for (const title of ["Ledger rollout", "Mirror retries", "Clock drift"]) {
    const issue = await createIssue({ project: "CORE", title });
    const ask = await createAsk(issue.key, { question: `${title}?` }, SURFACE_SESSION);
    mine.push({ id: ask.id, issue: issue.key });
  }
  const bobs = await createIssue({ project: "CORE", title: "Lease handover" });
  await patchIssue(bobs.key, { assignee: "bob" });
  await createAsk(bobs.key, { question: "Whose lease?" }, SURFACE_SESSION);
  const answered = await createIssue({ project: "CORE", title: "Replay budget" });
  const answeredAsk = await createAsk(
    answered.key,
    { question: "How many replays?" },
    SURFACE_SESSION
  );
  // Handed back to its agent, so it waits on nobody here.
  await createComment(
    answered.key,
    { ask_id: answeredAsk.id, body: "Checking the budget now.", turn: "agent" },
    SURFACE_SESSION
  );
  // One unread thread, which the sidebar's own number adds and the switch does not: a reply
  // under a comment the reader wrote, which is what makes it a thread rather than a comment.
  const thread = await createIssue({ project: "CORE", title: "Nightly mirror" });
  await seedCommentThread(
    thread.key,
    "Is the nightly job still at 02:00?",
    "It moved to 03:00 last week."
  );
  // The session that asks is live, so the Agents page lists it with its Needs you pill.
  await setLiveSessions([
    {
      dir: "/workspaces/controls",
      machine_id: "box-1",
      session_id: SURFACE_SESSION.actor.id,
      title: "Controls",
    },
  ]);
  // One thread she dismissed, which the Dismissed view lists.
  const archived = await createIssue({ project: "CORE", title: "Archived mirror" });
  await seedDismissedThreads({ count: 1, issueKey: archived.key, prefix: "Archived" });
  // And one credential request waiting on her.
  await setPendingCredentialRequests([
    {
      approver: "alice",
      identifiers: ["ANTHROPIC_API_KEY"],
      kind: "agent_secret",
      record_id: "record-table-controls",
      requested_at: "2026-09-27T00:00:00Z",
      session: { enrollment: null, request: null },
    },
  ]);

  await page.goto("/");
  const main = page.getByTestId("main-content");
  const list = page.getByTestId("inbox-list");
  const rows = page.locator("[data-inbox-row]");
  const bar = controlRow(main);
  const sections = bar.getByRole("group", { name: "Inbox section" });
  const all = sections.getByRole("button", { exact: true, name: "All" });
  const blocked = sections.getByRole("button", { name: /^Blocked on you\b/ });
  await expect(list).toBeVisible();
  // No element above the list carries the words the banner carried.
  await expect(
    main.getByText(/^Blocked on you: /),
    "the old banner's words are still drawn above the Inbox's list"
  ).toHaveCount(0);

  // The switch's Blocked on you carries the asks waiting on her plus the credential request:
  // three and one, the number the needs-you control carried before the redesign.
  await expect(blocked).toHaveAccessibleName(/^Blocked on you: 4 items/);
  await expect(blocked).toHaveText("Blocked on you 4");
  const sidebar = page.getByRole("navigation", { name: "Navigation" }).getByRole("link", {
    name: /^Inbox/,
  });
  await expect(sidebar, "the sidebar's Inbox entry draws no number").toHaveText(/\d/);
  // The number was waited for above, and the entry draws it only once both counts it adds up
  // have answered, so the number read is the whole sum.
  const sidebarCount = Number((await sidebar.textContent())?.replace(/\D+/g, "") ?? "0");
  expect(sidebarCount, "the sidebar's number does not add the unread thread").toBeGreaterThan(4);

  // Choosing it narrows the list to the asks waiting on her and keeps the credential section; it
  // writes `section=needs-you`, takes `assignee`, the four fields and the search text out, and
  // keeps the sort, the grouping and the view. It draws no chip, and Filter does not count it.
  await page.goto(
    "/?assignee=bob&type=question&priority=P2&urgency=med&project=CORE&q=lease&sort=urgency&group=waiting-on&show=inbox"
  );
  await expect(page).toHaveURL(/[?&]show=inbox(&|$)/);
  await blocked.click();
  // Inbox is the default view and route writes omit show=inbox. Dismissed is the only other
  // view and draws no switch, so check the address before checking the pressed switch.
  await expect(page, "Blocked on you changed the Inbox view").not.toHaveURL(/[?&]show=/);
  await expect(page).toHaveURL(/[?&]section=needs-you(&|$)/);
  for (const param of ["assignee", "type", "priority", "urgency", "project", "q"]) {
    await expect(page, `Blocked on you left ?${param}= on`).not.toHaveURL(
      new RegExp(`[?&]${param}=`)
    );
  }
  await expect(page).toHaveURL(/[?&]sort=urgency(&|$)/);
  await expect(page).toHaveURL(/[?&]group=waiting-on(&|$)/);
  await expect(blocked).toHaveAttribute("aria-pressed", "true");
  const credentials = page.getByRole("region", { name: "Credential requests" });
  await expect(
    credentials,
    "the credential requests section went with the narrowing"
  ).toBeVisible();
  await expect(rows).toHaveCount(mine.length);
  const drawn = await drawnIds(page);
  expect([...drawn].sort(), "the narrowed list is not the asks waiting on the reader").toEqual(
    mine.map((ask) => ask.id).sort()
  );
  expect(
    drawn.length + (await credentials.getByRole("listitem").count()),
    "the page lists a different number of items than Blocked on you carries"
  ).toBe(4);
  await expect(control(bar, CONTROL.chip), "Blocked on you drew a chip").toHaveCount(0);
  await expect(
    control(bar, CONTROL.panelTrigger),
    "Filter counts Blocked on you"
  ).toHaveAccessibleName("Filter");
  const view = await openDisplay(page, main);
  await expect(
    displayChoice(view, "Show", "inbox"),
    "Blocked on you did not keep the Inbox view"
  ).toBeChecked();
  await page.keyboard.press("Escape");

  // All takes `section` out and changes nothing else: every item shows again.
  await all.click();
  await expect(page).not.toHaveURL(/section=/);
  await expect(page).toHaveURL(/[?&]sort=urgency(&|$)/);
  await expect(all).toHaveAttribute("aria-pressed", "true");
  // Her three asks, the one handed back to its agent, and the unread thread.
  await expect(rows).toHaveCount(mine.length + 2);

  // With Blocked on you chosen, the Dismissed view takes `section` out and lists the dismissed
  // thread, and no switch is drawn there.
  await blocked.click();
  await expect(page).toHaveURL(/[?&]section=needs-you(&|$)/);
  await chooseShow(page, "Dismissed");
  await expect(page).toHaveURL(/[?&]show=dismissed(&|$)/);
  await expect(page).not.toHaveURL(/section=/);
  await expect(rows, "the Dismissed view lists no dismissed thread").toHaveCount(1);
  await expect(sections, "the switch is drawn in the Dismissed view").toHaveCount(0);

  // With Blocked on you chosen again, an agent narrowing takes `section` out too: the reader
  // leaves for the Agents page and follows the session's Needs you pill, which opens the agent
  // narrowing alone (D29), and no switch is drawn beside it.
  await page.goto("/");
  await blocked.click();
  await expect(page).toHaveURL(/[?&]section=needs-you(&|$)/);
  await page.goto("/agents");
  await page.getByRole("link", { name: /^Needs you \d+$/ }).click();
  await expect(page).toHaveURL(
    new RegExp(`[?&]agent=${encodeURIComponent(SURFACE_SESSION.actor.id)}(&|$)`)
  );
  await expect(page).not.toHaveURL(/section=/);
  await expect(sections, "the switch is drawn beside an agent narrowing").toHaveCount(0);

  // With nothing waiting on her - her asks handed back, the request gone - the part shows no
  // number.
  for (const ask of mine) {
    await createComment(
      ask.issue,
      { ask_id: ask.id, body: "Over to you.", turn: "agent" },
      SURFACE_SESSION
    );
  }
  await setPendingCredentialRequests([]);
  await page.goto("/");
  await expect(blocked).toHaveAccessibleName(/^Blocked on you: 0 items/);
  await expect(blocked).toHaveText("Blocked on you");

  // On the project page the same component draws its pill as it did at the merge base: its
  // words with the project's count, its link to the Inbox, and its classes
  // (`PILL_CLASSES_AT_THE_BASE`). Nothing waits on her now, so a fresh ask on her issue counts.
  await createAsk(mine[0]?.issue ?? "", { question: "One more?" }, SURFACE_SESSION);
  await page.goto("/projects/CORE/issues");
  const pill = page.getByRole("link", { name: /^Blocked on you · / });
  await expect(pill, "the project page's pill is gone").toHaveText("Blocked on you · 1");
  await expect(pill, "the project page's pill links elsewhere").toHaveAttribute("href", "/");
  // The pill's words were waited for above, so the classes read are the ones it is drawn with.
  const pillClass = (await pill.getAttribute("class")) ?? "";
  expect(
    classTokens(pillClass),
    "the project page's pill wears other classes than it wore at the base"
  ).toEqual(PILL_CLASSES_AT_THE_BASE);
});

// SLOW(>=2s): nine tables across three pages, each seeded and read.
test("every table with no controls draws its rows in the shared markup and offers no search, filter or sort", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await createProject({ key: "CORE", name: "Core" });
  await putProjectDefaultRepository("CORE", "owner/repo");
  await mintAgentToken("table-controls", "alice");
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: { "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  await setSignIns([
    {
      created_at: "2026-10-28T09:00:00Z",
      last_used_at: "2026-11-01T18:30:00Z",
      machine: "workstations@example-project.iam.gserviceaccount.com",
      person: "alice",
      signin_id: "11111111-1111-4111-8111-111111111111",
      thumbprint: "aaaabbbbccccddddeeeeffffgggghhhhiiiijjjjkkk",
    },
  ]);
  await setCredentialGrants([
    {
      approver: "alice",
      created_at: "2026-10-03T12:07:00Z",
      decided_by: "alice",
      enrollment: { kind: "pod", operator: "", runtime_id: "pod-7", slot: null },
      expires_at: "2026-10-03T12:22:00Z",
      grant_id: "22222222-2222-4222-8222-222222222222",
      granted: "approval",
      listedFor: "alice",
      names: ["BILLING_KEY"],
      record_id: "bb22cc33dd44ee55ff6677889900aa11bb22cc33dd44ee55ff6677889900aa11",
    },
  ]);
  // A daemon named for a project of its own, so the Legion daemons table draws a row of it
  // rather than its empty line.
  await seedLegionDaemon("http://legion.acme.internal:9731");

  /** Reads one plain table: it is inside the shared markup, it has rows, and it offers no
   *  search box, no panel trigger and no sort control of its own. */
  const readPlainTable = async (name: string): Promise<void> => {
    const shell = page.locator(`[data-control="${CONTROL.tableShell}"]`).filter({
      has: page.getByRole("table", { name }),
    });
    await expect(shell, `${name} is not drawn in the shared table markup`).toHaveCount(1);
    await expect(
      shell.getByRole("row"),
      `${name} draws no rows at all, not even a head`
    ).not.toHaveCount(0);
    for (const forbidden of [CONTROL.searchBox, CONTROL.panelTrigger, CONTROL.columnFilter]) {
      await expect(
        shell.locator(`[data-control="${forbidden}"]`),
        `${name} draws a ${forbidden}`
      ).toHaveCount(0);
    }
    // What a sort control is: the header button `DataTable`'s `TableHeaderCell` draws for a
    // sortable column. `th[aria-sort]` would say nothing here, since that attribute is written
    // only while a column IS sorted, so a freshly drawn sortable table carries none either.
    await expect(shell.locator("thead button"), `${name} draws a sort control`).toHaveCount(0);
    await expect(
      control(shell, CONTROL.dataTable),
      `${name} is drawn by the shared TanStack table rather than the plain shell`
    ).toHaveCount(0);
  };

  await page.goto("/settings");
  for (const name of [
    "Projects",
    "Agent tokens",
    "Architecture sources",
    "Sign-ins",
    "Live grants",
    "Legion daemons",
  ]) {
    await readPlainTable(name);
  }

  await seedPipelineFixture();
  await page.goto(`/pipeline?at=${PIPELINE_AT}`);
  await expect(page.getByRole("heading", { level: 1, name: "Pipeline" })).toBeVisible();
  for (const name of ["PR CI: slowest jobs", "PR CI: jobs that failed in the last 24 hours"]) {
    await readPlainTable(name);
  }

  // No second delivery fixture: `seedPipelineFixture` already wrote the one delivery settings
  // row, and a second insert is refused by its primary key. The measures table is behind the
  // panel's own fold, which the page opens shut.
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await page.getByRole("button", { name: /Definitions & alternatives/ }).click();
  await readPlainTable("Per day (UTC), newest first");
});

/** R8's reading of one row or card: what it draws, in order, and its box. */
interface DrawnItem {
  /** One entry per part, in document order, joined by ` | `: a control as its role and its
   *  words (`button "ID"`), a run of text outside any control as its words in quotes, and a
   *  relative time as `time`, since its words move with the clock. Nothing the item hides is a
   *  part. */
  readonly parts: string;
  /** The item's height and width, and the width of the list it is drawn in, in px. */
  readonly box: readonly [height: number, width: number, list: number];
}

/** The four lists R8's row reads, each named as its failures name it. */
type ItemList = "project Board" | "project List" | "Agents page" | "Delivery list";

/** What R8 holds one row or card to: the parts it drew at the base, and its box there. Each size
 *  is the px the base's CSS gave it, which no font moves; a height the fonts decide is `"lines"`
 *  instead, the height `linedHeights` gives the item from the lines the page drew it in and the
 *  base's classes. */
interface ItemAtTheBase {
  readonly parts: string;
  readonly box: readonly [height: number | "lines", width: number, list: number];
}

/** The lists whose items' heights the fonts decide below `md`, as `linedHeights` reads them. */
type LinedList = Extract<ItemList, "project Board" | "Agents page">;

/**
 * The height each item of `list` takes from the lines the page drew it in and the classes the
 * base drew it with, for a list whose items' heights the fonts decide below `md`. There every
 * control is at least 44 px tall and every link `inline-flex` (styles.css), so how many lines an
 * item's controls wrap onto, and how tall the line box a link stands in is, follow the font's
 * widths and its ascent and descent - the runner's installed fonts, since the stack (`Inter,
 * ui-sans-serif, system-ui`, styles.css) loads none of its own. Read in this pod with three fonts
 * in place of the stack's (DejaVu Sans Mono, and DejaVu Sans with its ascent and descent moved and
 * its size 12% and 40% larger), these were the only sizes of R8's four lists that moved, in either
 * project: the Board's cards from 129 to 130 px, and an Agents row from 126 to 174 px, at 390.
 *
 * - A Board card: its `border` (1 px) and `p-3` (12 px) above and below; the line box its title
 *   link stands in, read from the page as the space from the top of the card's content box to the
 *   margin of its row of controls - the font's part; that row's `mt-3` (12 px); and the row, each
 *   of its lines the 44 px of its tallest control (PriorityControl's badge wrapper, `min-h-11`),
 *   with `gap-2` (8 px) between two.
 * - An Agents row: its `border` (1 px) and `py-1.5` (6 px) above and below, and `gap-y-1` (4 px)
 *   between two of its lines; each line of controls is 44 px (the toggle's `min-h-11`, and every
 *   control below 1280 px), and the directory's own line (`basis-full`) its lines of `text-xs`
 *   (16 px). How many lines there are is the font's part.
 */
async function linedHeights(items: Locator, list: LinedList): Promise<number[]> {
  return items.evaluateAll((nodes, which) => {
    const px = (value: string): number => Number.parseFloat(value);
    /** The boxes a flex box lays out: a `display: contents` wrapper's children stand in its
     *  place, and an element drawn nowhere is no box. */
    const flexItems = (container: Element): Element[] =>
      [...container.children].flatMap((child): Element[] => {
        if (getComputedStyle(child).display === "contents") return flexItems(child);
        return child.getClientRects().length > 0 ? [child] : [];
      });
    /** How many lines `boxes` stand on: a box starts a line where it begins below every box of
     *  the line before it. */
    const lineCount = (boxes: readonly { bottom: number; top: number }[]): number => {
      let lines = 0;
      let bottom = Number.NEGATIVE_INFINITY;
      for (const box of [...boxes].sort((a, b) => a.top - b.top)) {
        if (box.top >= bottom) lines += 1;
        bottom = box.top >= bottom ? box.bottom : Math.max(bottom, box.bottom);
      }
      return lines;
    };
    const rect = (element: Element) => element.getBoundingClientRect();
    /** The lines `element`'s text is drawn on. */
    const textLines = (element: Element): number => {
      const rects: DOMRect[] = [];
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
        const range = document.createRange();
        range.selectNodeContents(text);
        rects.push(...[...range.getClientRects()].filter((line) => line.width > 0));
      }
      return lineCount(rects);
    };
    return nodes.map((node): number => {
      if (which === "project Board") {
        const row = node.querySelector("[data-issue-link]")?.nextElementSibling;
        if (row === null || row === undefined) return Number.NaN;
        const card = getComputedStyle(node);
        const contentTop = rect(node).top + px(card.borderTopWidth) + px(card.paddingTop);
        const titleLine = rect(row).top - px(getComputedStyle(row).marginTop) - contentTop;
        const rowLines = lineCount(flexItems(row).map(rect));
        return 1 + 12 + titleLine + 12 + rowLines * 44 + (rowLines - 1) * 8 + 12 + 1;
      }
      const box = node.firstElementChild;
      if (box === null) return Number.NaN;
      const directory = box.querySelector("[data-agent-dir]");
      const boxes = flexItems(box).filter((item) => item !== directory);
      const controlLines = lineCount(boxes.map(rect));
      const directoryLines = directory === null ? 0 : textLines(directory);
      const lines = controlLines + (directoryLines > 0 ? 1 : 0);
      return 1 + 6 + controlLines * 44 + directoryLines * 16 + (lines - 1) * 4 + 6 + 1;
    });
  }, list);
}

/** Reads every item `items` finds, each against the nearest ancestor `list` names. */
async function drawnItems(items: Locator, list: string): Promise<DrawnItem[]> {
  return items.evaluateAll((nodes, listSelector) => {
    const controls =
      'a[href], button, input, select, textarea, [role="button"], [role="checkbox"], [role="combobox"], [role="link"]';
    const tagRoles: Record<string, string> = {
      A: "link",
      BUTTON: "button",
      SELECT: "combobox",
      TEXTAREA: "textbox",
    };
    const roleOf = (element: Element): string => {
      const role = element.getAttribute("role");
      if (role !== null) return role;
      if (element instanceof HTMLInputElement) {
        return element.type === "checkbox" ? "checkbox" : "textbox";
      }
      return tagRoles[element.tagName] ?? element.tagName.toLowerCase();
    };
    /** Whether an element is drawn: a `display: contents` wrapper draws no box of its own, so
     *  `checkVisibility` calls it hidden, while what it holds is drawn. */
    const shown = (element: Element): boolean =>
      getComputedStyle(element).display === "contents" ||
      element.checkVisibility({ visibilityProperty: true });
    const textOf = (node: Node): string => (node.textContent ?? "").replace(/\s+/g, " ").trim();
    /** The words under a control, each run of text once, a relative time as `time`. */
    const wordsOf = (element: Element): string[] =>
      [...element.childNodes].flatMap((child): string[] => {
        if (child.nodeType === Node.TEXT_NODE) return textOf(child) === "" ? [] : [textOf(child)];
        if (!(child instanceof Element) || !shown(child)) return [];
        return child.tagName === "TIME" ? ["time"] : wordsOf(child);
      });
    const partsOf = (element: Element): string[] =>
      [...element.childNodes].flatMap((child): string[] => {
        if (child.nodeType === Node.TEXT_NODE) {
          return textOf(child) === "" ? [] : [`"${textOf(child)}"`];
        }
        if (!(child instanceof Element) || !shown(child)) return [];
        if (child.tagName === "TIME") return ["time"];
        if (!child.matches(controls)) return partsOf(child);
        const name = child.getAttribute("aria-label") ?? wordsOf(child).join(" ");
        return [`${roleOf(child)} "${name}"`];
      });
    const round = (value: number): number => Math.round(value * 100) / 100;
    return nodes.map((node): DrawnItem => {
      const box = node.getBoundingClientRect();
      const list = node.closest(listSelector)?.getBoundingClientRect().width ?? Number.NaN;
      return {
        box: [round(box.height), round(box.width), round(list)],
        parts: partsOf(node).join(" | "),
      };
    });
  }, list);
}

/**
 * What R8's four lists drew at commit bdbce69a7da8c48fa5aa5b6d26373f65cae53514, the merge base,
 * `fork_point(main@origin | @)`, since the forward merge 702816d8 brought that main commit in, in
 * each project the row runs in: `chromium` at 1440x900 and `iphone` at its own 390x664. Every
 * value is the one the first reading took at the earlier merge base,
 * 6ef60ff0ceeba4ccaf285f06f5bbe9b859b2e4d8: the two readings drew the same parts and the same
 * sizes, item by item, in both projects.
 *
 * Measured, not written from the code, in builds Job `infra-3135-krows-base-swts8` on
 * 2026-11-23. The merge base's `packages/dispatch/web` was built in that Job by `vite build` over
 * the head's install; outside `packages/dispatch/web` and `e2e` only `packages/dispatch/AGENTS.md`
 * differed between the merge base and the head the reading ran at, `868402ec`, so the server,
 * the contracts and the build tools were that head's own. The build was served to this row in
 * place of `web/dist` by `DISPATCH_E2E_SERVER_WEB_DIST` (e2e/run-server.sh), over the row's own
 * seed, and the row was run over it in both projects, with a line that printed each list's last
 * reading (`drawnItems`, and `linedHeights` for the lists whose heights the fonts decide). The
 * Delivery list's rows are found by `tbody tr[aria-rowindex]`, because the base's carry no
 * `data-pr-row`: both builds write `aria-rowindex` on each pull request's row in the list's body.
 *
 * Every size kept as a number is one the CSS fixes, which a runner with other fonts draws the
 * same: none moved when this pod drew the lists in three other fonts (`linedHeights`). The Board
 * card's 288 px and its column's are `w-72`, and its 114 px at 1440 is `border`, `p-3`, its key and
 * title each one `text-sm` line of 20 px with `mt-1`, `mt-3` and the 32 px of its row of controls
 * from `md`. A List row is the cell's `py-2` and border with its tallest cell: one 20 px title
 * line, `space-y-1` and the 20 px asks badge (61 px), else the 32 px priority control (49 px); at
 * 390 the title link's 44 px and `space-y-1`, with the badge (85 px) or without (65 px). The
 * List's 2319.19 and 2106.69 px are its columns' widths, the title's a share of the box
 * (`ISSUE_TABLE_COLUMNS`), so no text sets them. An Agents row at 1440 is one line from `md`
 * (`md:flex-nowrap`): `border`, `py-1.5` and its 32 px controls. The Delivery row is the table's
 * 36 px row height and its filled width. The heights the fonts do decide - the Board's cards and
 * the Agents rows at 390, where a link's line box and how many lines the controls wrap onto
 * follow the font - are `"lines"`.
 */
const ITEMS_AT_THE_BASE: Partial<Record<string, Record<ItemList, readonly ItemAtTheBase[]>>> = {
  chromium: {
    "project Board": [
      {
        box: [114, 288, 288],
        parts: 'link "CORE-1 Keyboard issue" | "Priority" | combobox "Priority of CORE-1" | "1"',
      },
      {
        box: [114, 288, 288],
        parts: 'link "CORE-2 Rollout plan" | "Priority" | combobox "Priority of CORE-2"',
      },
      {
        box: [114, 288, 288],
        parts:
          'link "CORE-3 Rollout order" | "Priority" | combobox "Priority of CORE-3" | "frontend" | "1"',
      },
      {
        box: [114, 288, 288],
        parts:
          'link "CORE-4 Rollout budget" | "Priority" | combobox "Priority of CORE-4" | "backend" | "1"',
      },
      {
        box: [114, 288, 288],
        parts:
          'link "CORE-5 Ledger mirror" | "Priority" | combobox "Priority of CORE-5" | "frontend" | "1"',
      },
      {
        box: [114, 288, 288],
        parts:
          'link "CORE-6 Clock drift" | "Priority" | combobox "Priority of CORE-6" | "backend" | "1"',
      },
      {
        box: [114, 288, 288],
        parts:
          'link "CORE-7 Replay window" | "Priority" | combobox "Priority of CORE-7" | "frontend" | "1"',
      },
      {
        box: [114, 288, 288],
        parts:
          'link "CORE-8 Lease handover" | "Priority" | combobox "Priority of CORE-8" | "backend" | "1"',
      },
    ],
    "project List": [
      {
        box: [61, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-1" | "Priority" | combobox "Priority of CORE-1" | link "CORE-1" | link "CORE-1 Keyboard issue" | "1" | "Triage" | "No epic" | "alice" | "alice" | "1" | time',
      },
      {
        box: [49, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-2" | "Priority" | combobox "Priority of CORE-2" | link "CORE-2" | link "CORE-2 Rollout plan" | "Triage" | "No epic" | "alice" | "alice" | time',
      },
      {
        box: [61, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-3" | "Priority" | combobox "Priority of CORE-3" | link "CORE-3" | link "CORE-3 Rollout order" | "1" | "Todo" | "No epic" | "alice" | "alice" | "frontend" | "1" | time',
      },
      {
        box: [61, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-4" | "Priority" | combobox "Priority of CORE-4" | link "CORE-4" | link "CORE-4 Rollout budget" | "1" | "Todo" | "No epic" | "alice" | "alice" | "backend" | "1" | time',
      },
      {
        box: [61, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-5" | "Priority" | combobox "Priority of CORE-5" | link "CORE-5" | link "CORE-5 Ledger mirror" | "1" | "Todo" | "No epic" | "alice" | "alice" | "frontend" | "1" | time',
      },
      {
        box: [61, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-6" | "Priority" | combobox "Priority of CORE-6" | link "CORE-6" | link "CORE-6 Clock drift" | "1" | "Todo" | "No epic" | "alice" | "alice" | "backend" | "1" | time',
      },
      {
        box: [61, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-7" | "Priority" | combobox "Priority of CORE-7" | link "CORE-7" | link "CORE-7 Replay window" | "1" | "Todo" | "No epic" | "alice" | "alice" | "frontend" | "1" | time',
      },
      {
        box: [61, 2319.19, 2319.19],
        parts:
          'checkbox "Select CORE-8" | "Priority" | combobox "Priority of CORE-8" | link "CORE-8" | link "CORE-8 Lease handover" | "1" | "Todo" | "No epic" | "alice" | "alice" | "backend" | "1" | time',
      },
    ],
    "Agents page": [
      {
        box: [46, 1072, 1072],
        parts:
          'checkbox "Select Planner for broadcast" | button "Planner" | link "Open live conversation with Planner" | button "Copy session ID 019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01" | button "Copy session title Planner" | link "box-1" | "· planner" | "planner" | time | link "Needs you 1" | button "Pin Planner"',
      },
      {
        box: [46, 1072, 1072],
        parts:
          'checkbox "Select Reviewer for broadcast" | button "Reviewer" | link "Open live conversation with Reviewer" | button "Copy session ID 019a1c03-2f0d-7c4c-ab85-0c7d1e4f5a03" | button "Copy session title Reviewer" | link "box-1" | "· reviewer" | "reviewer" | time | button "Pin Reviewer"',
      },
    ],
    "Delivery list": [
      {
        box: [36, 1152, 1152],
        parts:
          '"Jun 1, 3:00 AM" | link "widgets#2" | "fix: a waiting widget" | "No issue" | "No session" | "octocat" | "+" | "4" | "-" | "1" | "waiting" | "rework"',
      },
      {
        box: [36, 1152, 1152],
        parts:
          '"Jun 1, 1:00 AM" | link "widgets#1" | "feat: a shipped widget" | "ACME-1 · P0" | "No session" | "octocat" | "+" | "12" | "-" | "3" | button "Open deploy run #500" | "Jun 1, 1:40 AM" | "40m"',
      },
    ],
  },
  iphone: {
    "project Board": [
      {
        box: ["lines", 288, 288],
        parts: 'link "CORE-1 Keyboard issue" | "Priority" | combobox "Priority of CORE-1" | "1"',
      },
      {
        box: ["lines", 288, 288],
        parts: 'link "CORE-2 Rollout plan" | "Priority" | combobox "Priority of CORE-2"',
      },
      {
        box: ["lines", 288, 288],
        parts:
          'link "CORE-3 Rollout order" | "Priority" | combobox "Priority of CORE-3" | "frontend" | "1"',
      },
      {
        box: ["lines", 288, 288],
        parts:
          'link "CORE-4 Rollout budget" | "Priority" | combobox "Priority of CORE-4" | "backend" | "1"',
      },
      {
        box: ["lines", 288, 288],
        parts:
          'link "CORE-5 Ledger mirror" | "Priority" | combobox "Priority of CORE-5" | "frontend" | "1"',
      },
      {
        box: ["lines", 288, 288],
        parts:
          'link "CORE-6 Clock drift" | "Priority" | combobox "Priority of CORE-6" | "backend" | "1"',
      },
      {
        box: ["lines", 288, 288],
        parts:
          'link "CORE-7 Replay window" | "Priority" | combobox "Priority of CORE-7" | "frontend" | "1"',
      },
      {
        box: ["lines", 288, 288],
        parts:
          'link "CORE-8 Lease handover" | "Priority" | combobox "Priority of CORE-8" | "backend" | "1"',
      },
    ],
    "project List": [
      {
        box: [85, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-1" | link "CORE-1" | link "CORE-1 Keyboard issue" | "1" | "Priority" | combobox "Priority of CORE-1" | "Triage" | "No epic" | "alice" | "alice" | "1" | time',
      },
      {
        box: [65, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-2" | link "CORE-2" | link "CORE-2 Rollout plan" | "Priority" | combobox "Priority of CORE-2" | "Triage" | "No epic" | "alice" | "alice" | time',
      },
      {
        box: [85, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-3" | link "CORE-3" | link "CORE-3 Rollout order" | "1" | "Priority" | combobox "Priority of CORE-3" | "Todo" | "No epic" | "alice" | "alice" | "frontend" | "1" | time',
      },
      {
        box: [85, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-4" | link "CORE-4" | link "CORE-4 Rollout budget" | "1" | "Priority" | combobox "Priority of CORE-4" | "Todo" | "No epic" | "alice" | "alice" | "backend" | "1" | time',
      },
      {
        box: [85, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-5" | link "CORE-5" | link "CORE-5 Ledger mirror" | "1" | "Priority" | combobox "Priority of CORE-5" | "Todo" | "No epic" | "alice" | "alice" | "frontend" | "1" | time',
      },
      {
        box: [85, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-6" | link "CORE-6" | link "CORE-6 Clock drift" | "1" | "Priority" | combobox "Priority of CORE-6" | "Todo" | "No epic" | "alice" | "alice" | "backend" | "1" | time',
      },
      {
        box: [85, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-7" | link "CORE-7" | link "CORE-7 Replay window" | "1" | "Priority" | combobox "Priority of CORE-7" | "Todo" | "No epic" | "alice" | "alice" | "frontend" | "1" | time',
      },
      {
        box: [85, 2106.69, 2106.69],
        parts:
          'checkbox "Select CORE-8" | link "CORE-8" | link "CORE-8 Lease handover" | "1" | "Priority" | combobox "Priority of CORE-8" | "Todo" | "No epic" | "alice" | "alice" | "backend" | "1" | time',
      },
    ],
    "Agents page": [
      {
        box: ["lines", 342, 342],
        parts:
          'checkbox "Select Planner for broadcast" | button "Planner" | link "Open live conversation with Planner" | button "Copy session ID 019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01" | button "Copy session title Planner" | link "box-1" | "· /srv/planner" | "planner" | time | link "Needs you 1" | button "Pin Planner"',
      },
      {
        box: ["lines", 342, 342],
        parts:
          'checkbox "Select Reviewer for broadcast" | button "Reviewer" | link "Open live conversation with Reviewer" | button "Copy session ID 019a1c03-2f0d-7c4c-ab85-0c7d1e4f5a03" | button "Copy session title Reviewer" | link "box-1" | "· /srv/reviewer" | "reviewer" | time | button "Pin Reviewer"',
      },
    ],
    "Delivery list": [
      {
        box: [36, 1152, 1152],
        parts:
          '"Jun 1, 3:00 AM" | link "widgets#2" | "fix: a waiting widget" | "No issue" | "No session" | "octocat" | "+" | "4" | "-" | "1" | "waiting" | "rework"',
      },
      {
        box: [36, 1152, 1152],
        parts:
          '"Jun 1, 1:00 AM" | link "widgets#1" | "feat: a shipped widget" | "ACME-1 · P0" | "No session" | "octocat" | "+" | "12" | "-" | "3" | button "Open deploy run #500" | "Jun 1, 1:40 AM" | "40m"',
      },
    ],
  },
};

/** An ask card's "Asked of" line from `md`, at the base: the 32 px of its badge's wrapper
 *  (`badgeSelectWrapper`'s `md:min-h-8`), with the hand-on laid over the badge taking no room -
 *  a native select then, a button now whose 44 px below 1280 px (styles.css) overhangs it. */
const ASKED_OF_LINE = 32;

/** The issue header's Assignee badge from `md`, at the base: its wrapper, the badge's own item in
 *  the header's wrapping row, 32 px (`badgeSelectWrapper`'s `md:min-h-8`), with the native
 *  select laid over the badge taking no room - the list's 44 px trigger now, which overhangs it
 *  below 1280 px (`badgeSelectTrigger`). */
const ASSIGNEE_BADGE = 32;

/** The issue header's `Repository:` line in its details rail from `md`, around the shared list
 *  trigger's wrapper: the 28 px of that wrapper (`badgeSelectRailWrapper`'s `md:min-h-7`), which
 *  the trigger overhangs below 1280 px (`badgeSelectRailTrigger`). The reference is the head's own
 *  shared trigger box, not a reading at the base: R3's intended difference from main's visible
 *  text box, which this change replaces with the shared list's trigger. At 6ef60ff0, the merge
 *  base when that line was read, the line holds that text box, and the same read of that line,
 *  this row's run over that commit's build, measured 44 px at 1024x768. */
const REPOSITORY_LINE = 28;

/** The parts a Board card or a List row draws now where it drew `parts` at the base. R3 changes
 *  one of them, the issue's priority: the base drew the badge's words with a native select laid
 *  over it (`"P1" | combobox "Priority of CORE-1"`), and the head draws the shared list's trigger
 *  holding that badge, read by its words (`button "P1"`). Every other part is the base's. */
function priorityAtTheHead(parts: string): string {
  return parts.replace(/"([^"]*)" \| combobox "Priority of [A-Z]+-\d+"/g, 'button "$1"');
}

// R8: "The rows, the cards, the reading pane and the panes' widths do not change." The Inbox's
// rows and its reading pane are the stub row's (`table-controls.stub.e2e.ts`), and the tables
// with no controls the row above; this one holds the Board's cards and the rows of the Agents
// page, the project List and the Delivery list to what they drew at the base
// (`ITEMS_AT_THE_BASE`), item by item: the same parts, in the same order, save the priority's,
// which is the shared list's trigger now (`priorityAtTheHead`), in a box of the same
// size, where a height the fonts decide is the one its lines and the base's classes give it
// (`linedHeights`). It holds an ask card's "Asked of" line and the issue header's Assignee badge
// to their heights at the base too, and the header's `Repository:` line to the box of the shared
// trigger that replaces the base's text box (`REPOSITORY_LINE`), at 1024x768, between `md` and
// `xl`, the widths where every button is 44 px tall. The Delivery list draws its clock times in
// the browser's own zone and words, so both are pinned to the ones the base was read in.
test.describe("R8's rows and cards", () => {
  test.use({ locale: "en-US", timezoneId: "UTC" });

  // SLOW(>=2s): six pages over two seeded fixtures, each list read until it is the base's.
  test("the Board's cards and the rows of the Agents page, the project List and the Delivery list draw the parts and the size they had at the base @phone", async ({
    page,
  }, testInfo) => {
    // Measured in an issue pod (two cores), one worker: the row took 23.6 s in Chromium. A list
    // that never draws what the base drew is read for the suite's 15 s expect timeout before it
    // is named, so the four lists can add a minute, which the config's 60 s budget would cut off
    // before the row named them; two minutes holds both.
    test.setTimeout(120_000);
    const atTheBase = ITEMS_AT_THE_BASE[testInfo.project.name];
    if (atTheBase === undefined) {
      throw new Error(`no reading at the base for the ${testInfo.project.name} project`);
    }
    const { issue } = await seedRealStackSurfaces();
    await seedSurfaces();
    if (!isPhoneProject(testInfo)) await page.setViewportSize({ height: 900, width: 1440 });
    const main = page.getByTestId("main-content");
    const lists: {
      name: ItemList;
      noun: string;
      open: () => Promise<void>;
      items: Locator;
      list: string;
    }[] = [
      {
        name: "project Board",
        noun: "card",
        open: () => openIssuesTab(page, "CORE", "Board"),
        items: main.locator("[data-board-card]"),
        list: "[data-board-column]",
      },
      {
        name: "project List",
        noun: "row",
        open: () => openIssuesTab(page, "CORE", "List"),
        items: main.locator("[data-issue-row]"),
        list: "table",
      },
      {
        name: "Agents page",
        noun: "row",
        open: async () => {
          await page.goto("/agents");
        },
        items: main.locator("[data-agent-row]:not([hidden])"),
        list: "[data-agents-list]",
      },
      {
        name: "Delivery list",
        noun: "row",
        open: async () => {
          await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
        },
        items: main.locator("tbody tr[aria-rowindex]"),
        list: "table",
      },
    ];
    const changed: string[] = [];
    for (const { items, list, name, noun, open } of lists) {
      await open();
      await expect(items.first(), `the ${name} draws no ${noun}`).toBeVisible();
      const base = atTheBase[name];
      const lined = base.some((was) => was.box[0] === "lines");
      /** What one reading draws other than the base: the parts of each item, and each size within
       *  half a pixel, the rounding of a box's edge. */
      const differences = (drawn: readonly DrawnItem[], heights: readonly number[]): string[] => {
        const differs: string[] = [];
        if (drawn.length !== base.length) {
          differs.push(
            `the ${name} draws ${drawn.length} ${noun}s, and ${base.length} at the base`
          );
        }
        for (const [index, was] of base.entries()) {
          const item = drawn[index];
          if (item === undefined) continue;
          const at = `the ${name}'s ${noun} ${index + 1}`;
          if (item.parts !== priorityAtTheHead(was.parts)) {
            differs.push(
              `${at} draws ${item.parts}, and at the base ${was.parts}, which is ${priorityAtTheHead(was.parts)} with the priority's list`
            );
          }
          const [height, width, listWidth] = was.box;
          const ruled = height === "lines";
          for (const [size, now, then, from] of [
            [
              "height",
              item.box[0],
              ruled ? (heights[index] ?? Number.NaN) : height,
              ruled ? "by its lines and the base's classes" : "at the base",
            ],
            ["width", item.box[1], width, "at the base"],
            ["list's width", item.box[2], listWidth, "at the base"],
          ] as const) {
            if (!(Math.abs(now - then) <= 0.5)) {
              differs.push(
                `${at}'s ${size} is ${now}px, and ${Math.round(then * 100) / 100}px ${from}`
              );
            }
          }
        }
        return differs;
      };
      // An item's parts arrive in turns - the read state, the asks, the live link - so the list
      // is read again until it is what the base drew or the suite's expect timeout runs out
      // (`playwright.config.ts`), and what differs then is named below; a list no read reached
      // is named for that.
      let found = [`the ${name} was never read`];
      await expect
        .poll(async () => {
          const drawn = await drawnItems(items, list);
          const heights =
            lined && (name === "project Board" || name === "Agents page")
              ? await linedHeights(items, name)
              : [];
          found = differences(drawn, heights);
          return found;
        })
        .toEqual([])
        .catch(() => undefined);
      changed.push(...found);
    }

    await page.setViewportSize({ height: 768, width: 1024 });
    await page.goto("/?assignee=everyone");
    const handOns = main.locator("[data-ask-hand-on]");
    await expect(handOns.first(), "the Inbox draws no ask card at 1024x768").toBeVisible();
    const askedOf = await handOns.evaluateAll((wrappers) =>
      wrappers
        .filter((wrapper) => wrapper.getClientRects().length > 0)
        .map(
          (wrapper) =>
            Math.round((wrapper.parentElement?.getBoundingClientRect().height ?? 0) * 100) / 100
        )
    );
    /** Each line read at 1024x768: what it is, its height now, the height it is held to, and
     *  where that height was read. */
    const lines: [line: string, height: number, held: number, from: string][] = askedOf.map(
      (height, index) => [
        `the Inbox's ask card ${index + 1}'s "Asked of" line`,
        height,
        ASKED_OF_LINE,
        "at the base",
      ]
    );

    // The issue header's two controls, read once CORE has a default repository, so its rail
    // names one: the Assignee's wrapper, the control's parent at the base as now and the badge's
    // own item in the wrapping row, and the `Repository:` line, the control's grandparent, around
    // the shared trigger's wrapper.
    await putProjectDefaultRepository("CORE", "acme/widgets");
    await page.goto(`/issues/${issue}`);
    const assigneeControl = page.getByLabel(`Assignee of ${issue}`);
    const repositoryControl = page.getByLabel(`Repository of ${issue}`);
    await expect(assigneeControl, "the issue header draws no Assignee at 1024x768").toBeVisible();
    await expect(
      repositoryControl,
      "the issue header draws no Repository at 1024x768"
    ).toBeVisible();
    lines.push(
      [
        "the issue header's Assignee badge",
        await assigneeControl.evaluate(
          (control) =>
            Math.round((control.parentElement?.getBoundingClientRect().height ?? 0) * 100) / 100
        ),
        ASSIGNEE_BADGE,
        "at the base",
      ],
      [
        "the issue header's `Repository:` line",
        await repositoryControl.evaluate(
          (control) =>
            Math.round(
              (control.parentElement?.parentElement?.getBoundingClientRect().height ?? 0) * 100
            ) / 100
        ),
        REPOSITORY_LINE,
        "in the shared trigger's box",
      ]
    );
    for (const [line, height, held, from] of lines) {
      if (Math.abs(height - held) > 0.5) {
        changed.push(`${line} at 1024x768 is ${height}px, and ${held}px ${from}`);
      }
    }
    expect(
      changed,
      "a card or row draws other parts, or a box of another size, than it drew at the base, or the Repository line is not the shared trigger's box"
    ).toEqual([]);
  });
});

// SLOW(>=2s): three pages at two widths, a press in each shared control that takes away, hides or
// holds the control it was made on.
test("no press in the shared controls drops the keyboard focus onto the page, at every width", async ({
  page,
}) => {
  // Three pages at two widths, each press waiting out its write. Measured in an issue pod at
  // c17d720e: the row ran out of the config's 60 s budget 7 of 7 times in Chromium at its last
  // width, and with a longer one it finished in 57.7 s and 1.0 min; it takes its siblings' budget.
  test.setTimeout(150_000);
  const scene = await seedSurfaces();
  // Enough issues for two of the List's pages at 25 a page.
  await Promise.all(
    Array.from({ length: 24 }, (_, index) =>
      createIssue({ project: "CORE", title: `Paged row ${index}` })
    )
  );
  // Two issues in Legion's queue: the second's row offers a Start next of its own, which moves it
  // to the head at the first width, so at the second the other row offers it.
  for (const title of ["Queued first", "Queued second"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { labels: ["legion"], status: "todo" });
  }
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const wrong: string[] = [];
  /** Where each target has already been missed: a target missed once at a width is not waited
   *  for again there, so a hand-off that is gone costs the row one wait at each width rather
   *  than one for each press that misses it. */
  const missed = new Set<string>();
  /** Holds every issue write until `release`, as a slow server would, so a control can be read
   *  while its write is out; `stop` takes the hold off once the writes have landed. */
  const holdIssueWrites = async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    const pattern = "**/api/v1/issues/*";
    await page.route(pattern, async (route) => {
      if (route.request().method() !== "PATCH") return route.fallback();
      await promise;
      return route.fallback();
    });
    return { release: resolve, stop: () => page.unroute(pattern) };
  };
  /** The focused element's name, or the tag of the body when nothing is focused. */
  const focusNow = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return focused?.tagName ?? "nothing";
      }
      const label =
        focused.getAttribute("aria-label") ??
        (focused instanceof HTMLInputElement ? focused.labels?.[0]?.textContent?.trim() : null) ??
        focused.textContent?.trim() ??
        "";
      return `"${label === "" ? focused.tagName : label}"`;
    });
  /** Records a focus that has not landed on `wanted` within the suite's expect timeout
   *  (`playwright.config.ts`): the element named, or whatever a check reads off the page. Each
   *  check follows the change it is about - the control gone, the page turned, the bar gone -
   *  and a hand-off lands in that same commit or the frame after it. */
  const focusOn = async (
    where: string,
    press: string,
    wanted: Locator | (() => Promise<boolean>),
    name: string
  ) => {
    if (missed.has(`${where} ${name}`)) return;
    const landed = await (typeof wanted === "function"
      ? expect.poll(wanted).toBe(true)
      : expect(wanted).toBeFocused()
    ).then(
      () => true,
      () => false
    );
    if (!landed) {
      missed.add(`${where} ${name}`);
      wrong.push(`${where}: after ${press} the focus is on ${await focusNow()}, not on ${name}`);
    }
  };

  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;

    // The Inbox's Mark all read, an item of the bar's More, goes once nothing is left unread. In
    // the wide bar More held it alone and goes with it, handing the focus to the main action, New
    // message (D26); in the narrow bar More stays, holding View, and keeps the focus its menu
    // handed back. A thread of its own at each width, since the press at the first marks the
    // first one read.
    let where = `Inbox at ${at}`;
    const thread = await createIssue({ project: "CORE", title: `Nightly mirror at ${at}` });
    await seedCommentThread(thread.key, "Is the nightly job still at 02:00?", "It moved to 03:00.");
    await page.goto("/");
    const moreOfBar = row.getByRole("button", { exact: true, name: "More" });
    const newMessage = row.getByRole("button", { exact: true, name: "New message" });
    const markAll = page
      .getByRole("menu", { name: "More" })
      .getByRole("menuitem", { exact: true, name: "Mark all read" });
    await expect(moreOfBar, `${where}: More is not drawn`).toBeVisible();
    // More was waited for above, so the bar is drawn, and its width follows the viewport this
    // turn set at its top.
    const barWidth = await row.evaluate((node) => node.getBoundingClientRect().width);
    const narrowAt = barWidth < NARROW_BELOW;
    await moreOfBar.focus();
    await page.keyboard.press("Enter");
    await expect(markAll, `${where}: More holds no Mark all read`).toBeVisible();
    await markAll.focus();
    await page.keyboard.press("Enter");
    if (narrowAt) {
      await focusOn(where, "Enter on Mark all read", moreOfBar, "More");
    } else {
      await expect(moreOfBar, `${where}: More stayed with nothing in it`).toHaveCount(0);
      await focusOn(where, "Enter on Mark all read", newMessage, "New message");
    }

    // `+N more` of the chips line, opened by Enter: its panel takes the focus onto its first chip,
    // Escape hands it back to `+N more`, and a chip taken off in the panel by Enter hands it to
    // the panel's chip now at its place. Below 640 px of the bar's width no chip stands in its
    // line and no `+N more` is drawn (R8), so this is read in the wide bar alone.
    if (!narrowAt) {
      await page.goto(SIXTEEN_NARROWINGS);
      const more = row.getByRole("button", { name: MORE_CHIPS });
      await expect(more, `${where}: sixteen narrowings draw no +N more`).toBeVisible();
      const morePanel = page.getByRole("dialog", { name: MORE_CHIPS });
      const firstInPanel = morePanel.getByRole("button").first();
      await more.focus();
      await page.keyboard.press("Enter");
      await focusOn(where, "Enter on +N more", firstInPanel, "the panel's first chip");
      await page.keyboard.press("Escape");
      await expect(morePanel, `${where}: Escape left the panel of +N more open`).toHaveCount(0);
      await focusOn(where, "Escape in the panel of +N more", more, "+N more");
      await page.keyboard.press("Enter");
      await focusOn(where, "Enter on +N more again", firstInPanel, "the panel's first chip again");
      // The chip the Enter below takes off, read once the panel has handed it the focus above.
      const taken = (await firstInPanel.getAttribute("aria-label")) ?? "";
      await page.keyboard.press("Enter");
      await expect(
        morePanel.getByRole("button", { exact: true, name: taken }),
        `${where}: Enter on "${taken}" in the panel of +N more left it drawn`
      ).toHaveCount(0);
      await focusOn(
        where,
        `Enter on "${taken}" in the panel of +N more`,
        firstInPanel,
        "the panel's chip now at its place"
      );
    }

    // The List's pager: Next pressed until the last page and Previous until the first, each
    // refused there rather than disabled, so the focus stays on the button pressed.
    where = `project List at ${at}`;
    await openSurface(page, "project List", scene);
    await page.goto("/projects/CORE/issues?size=25");
    const pager = control(main, CONTROL.pager);
    await expect(pager, `${where}: the List draws no first page`).toContainText(/Page 1 of \d/);
    // The pager was waited for above until it read `Page 1 of N`, so the count of pages is drawn.
    const pages = Number(/Page 1 of (\d+)/.exec((await pager.textContent()) ?? "")?.[1] ?? "1");
    expect(pages, `${where}: the seed fills fewer than two pages`).toBeGreaterThan(1);
    for (const [step, name, last] of [
      ["Next", "Next ›", pages],
      ["Previous", "‹ Previous", 1],
    ] as const) {
      const button = pager.getByRole("button", { exact: true, name });
      await button.focus();
      for (let press = 1; press < pages; press++) {
        const to = step === "Next" ? press + 1 : pages - press;
        await page.keyboard.press("Enter");
        await expect(pager, `${where}: Enter on ${step} did not reach page ${to}`).toContainText(
          `Page ${to} of ${pages}`
        );
        await focusOn(where, `Enter on ${step} to page ${to}`, button, `"${name}"`);
      }
      await expect(button, `${where}: ${step} on page ${last} is still pressable`).toBeDisabled();
    }

    // A bar action: Take out of epic, held busy while its write is out and through the list's
    // re-read, and pressable by nobody meanwhile. The focus stays on it while it is busy, and once
    // the re-read settles and the bar goes with the selection it hands the focus to the row marked.
    const first = await page.locator("[data-issue-row]").first().getAttribute("data-issue-row");
    const box = page.getByRole("checkbox", { exact: true, name: `Select ${first}` });
    await box.focus();
    await page.keyboard.press("Space");
    await expect(box, `${where}: Space did not mark ${first}`).toBeChecked();
    const bar = page.getByRole("toolbar", { name: "Selected issues" });
    const takeOut = bar.getByRole("button", { exact: true, name: "Take out of epic" });
    const writes = await holdIssueWrites();
    await takeOut.focus();
    await page.keyboard.press("Enter");
    await expect(
      takeOut,
      `${where}: Take out of epic is pressable while its write is out`
    ).toBeDisabled();
    await focusOn(
      where,
      "Enter on Take out of epic, while its write is out",
      takeOut,
      `"Take out of epic"`
    );
    writes.release();
    await expect(bar, `${where}: the write landed and left the bar up`).toHaveCount(0);
    await focusOn(where, "Take out of epic's write landed", box, `"Select ${first}"`);
    await writes.stop();

    // A row's own Start next: held while its write is out, then gone once its issue starts next,
    // while the row stays. The focus stays on it while it is busy, then goes to the control now
    // at its place in the row, else the one before it, else the row itself.
    const offering = page
      .locator("[data-issue-row]")
      .filter({ has: page.getByRole("button", { exact: true, name: "Start next" }) });
    await expect(
      offering.first(),
      `${where}: no row offers a Start next of its own`
    ).toBeAttached();
    const queued = await offering.first().getAttribute("data-issue-row");
    const queuedRow = page.locator(`[data-issue-row="${queued}"]`);
    const startNext = queuedRow.getByRole("button", { exact: true, name: "Start next" });
    const rank = await holdIssueWrites();
    await startNext.focus();
    await page.keyboard.press("Enter");
    await expect(
      startNext,
      `${where}: ${queued}'s Start next is pressable while its write is out`
    ).toBeDisabled();
    await focusOn(
      where,
      `Enter on ${queued}'s Start next, while its write is out`,
      startNext,
      `${queued}'s "Start next"`
    );
    rank.release();
    await expect(startNext, `${where}: Start next did not start ${queued} next`).toHaveCount(0);
    await focusOn(
      where,
      `Start next started ${queued} next`,
      () => queuedRow.evaluate((node) => node.contains(document.activeElement)),
      "that row or a control in it"
    );
    await rank.stop();

    // The search bar's Filter panel: a pick from its Project menu and from its Status menu, and
    // an author taken, each take their own control away; the focus goes to the next control in
    // the panel that takes no typed text, else to the panel's own stop.
    where = `search bar at ${at}`;
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const palette = page.getByRole("dialog", { name: "Search" });
    await expect(palette, `${where}: Control+K opened no search bar`).toBeVisible();
    await control(controlRow(palette), CONTROL.panelTrigger).focus();
    await page.keyboard.press("Enter");
    const filters = page.getByRole("dialog", { name: "Filter" });
    await expect(filters, `${where}: Enter on Filter opened no panel`).toBeVisible();
    const author = filters.getByRole("searchbox", { name: "Author" });
    const lastChip = filters.getByRole("button", { name: REMOVE_CHIP }).last();
    const panelSteps = [
      {
        menu: "Project",
        option: scene.project,
        next: filters.getByRole("button", { exact: true, name: "Status" }),
        nextName: "the Status menu",
      },
      // The Author box now at the Status menu's place takes typed text, so the hand-off passes
      // over it to the nearest control before it that takes none: the last of the chips the
      // panel lists at its top (R8), the narrowing just picked.
      { menu: "Status", option: "Todo", next: lastChip, nextName: "the panel's last chip" },
    ] as const;
    for (const { menu, option, next, nextName } of panelSteps) {
      const opener = filters.getByRole("button", { exact: true, name: menu });
      await opener.focus();
      await page.keyboard.press("Enter");
      const pick = page
        .getByRole("listbox", { name: `${menu} options` })
        .getByRole("option", { exact: true, name: option });
      await pick.focus();
      await page.keyboard.press("Enter");
      await expect(
        opener,
        `${where}: Enter on the ${menu} menu's ${option} left the ${menu} menu drawn`
      ).toHaveCount(0);
      await focusOn(where, `Enter on the ${menu} menu's ${option}`, next, nextName);
    }
    await author.fill("alice");
    await page.keyboard.press("Enter");
    await expect(author, `${where}: Enter on the Author box left it drawn`).toHaveCount(0);
    // The Updated since box now at its place takes typed text too, so the focus goes to the
    // nearest control before it that takes none: again the last chip at the panel's top.
    await focusOn(where, "Enter on the Author box", lastChip, "the panel's last chip");
  }
  // A press that drops the focus onto the page sends a keyboard reader back to its first link,
  // from which the control they were using is a walk through everything above it.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): four pages, one press of each page's own controls that disables, takes away or
// draws again the control it was made on: the Inbox's reading column at 1440, and the project
// List, the Delivery page and the Artifacts tab at 1440 and 390.
// `@firefox` and `@webkit`: a focused control taken away fires no `focusout` in these engines, the
// clause of `playwright.config.ts` on a focused control taken away.
test("no page-own press on the project List, the Delivery page, the Artifacts tab or the Inbox's reading column drops the keyboard focus onto the page, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  // Seven presses, four at 1440 and three at 390. A press that loses the focus waits out the
  // suite's 15 s expect timeout (`playwright.config.ts`) for the focus to come back, so the budget
  // holds all seven losing it, 105 s, beside the rest of the row, which took less than the 61.1 s
  // the row took with 24 presses on seven pages (measured in an issue pod, two cores, one worker,
  // in Chromium): the row names every press it lost rather than ending at the budget.
  test.setTimeout(150_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = (await createIssue({ project: "CORE", title: "Rollout plan" })).key;
  await seedDeliveryFixture();
  await createIssueArtifact(issue, {
    content: "# plan.md\n\nThe rollout plan.\n",
    name: "plan.md",
  });
  const listFirst = await createIssue({ project: "CORE", title: "Epic member one" });
  const listSecond = await createIssue({ project: "CORE", title: "Epic member two" });
  const main = page.getByTestId("main-content");
  const lost: string[] = [];
  /** Reads where the focus went once a press settles, and keeps the ones it lost. */
  const after = async (where: string, press: string) => {
    await expect
      .poll(async () => (await focusOutcome(page)).lost)
      .toBe(false)
      .catch(() => undefined);
    // The poll above waited up to the expect timeout for the focus to land, so this reads where
    // it settled.
    const outcome = await focusOutcome(page);
    if (outcome.lost) lost.push(`${where}: after ${press} the focus is on ${outcome.label}`);
    else if (!outcome.inView) {
      lost.push(`${where}: after ${press} the focused "${outcome.label}" is out of view`);
    }
  };
  /** A question waiting on Alice, with two options to answer it by: its id. */
  const question = async (title: string) => {
    const owner = await createIssue({ project: "CORE", title });
    const ask = await createAsk(
      owner.key,
      { options: [{ label: "Yes" }, { label: "No" }], question: `${title}?` },
      SURFACE_SESSION
    );
    return ask.id;
  };

  for (const [step, size] of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ].entries()) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;

    // The Inbox's reading column: a Snooze pick in its head. The row stays in its band while the
    // snooze saves, and folds into Later, which starts folded, once the snooze lands; the column
    // then shows the question seeded before it and hands the focus to its own stop. Below the
    // column's width a row's own Snooze takes its row away with no hand-off of the Inbox's own,
    // so the row presses it only where the column is drawn.
    if (hasReadingPane(page)) {
      const inbox = `Inbox at ${at}`;
      await question(`Renew the lease at ${at}`);
      const snoozed = await question(`Rotate the keys at ${at}`);
      await page.goto("/");
      const snooze = await rowSnooze(page, snoozed);
      await snooze.focus();
      await pickFromList(snooze, "Later today");
      await expect(inboxRow(page, snoozed), `${inbox}: the snooze kept its row`).toHaveCount(0);
      await after(inbox, "a Snooze pick in the reading column's head");
    }

    // The project List's unanswered-epic notice: its Dismiss goes with it. The epic's create is
    // answered as one Dispatch made and kept no answer for (`409 KEY_COMMITTED`), which is what
    // draws the notice.
    let where = `project List at ${at}`;
    const keyCommitted = async (route: Route) => {
      if (route.request().method() !== "POST") return route.fallback();
      return route.fulfill({
        json: { code: IDEMPOTENCY_KEY_COMMITTED, error: "the key's write was made" },
        status: 409,
      });
    };
    await page.route("**/api/v1/issues", keyCommitted);
    await openIssuesTab(page, "CORE", "List");
    await main.getByRole("checkbox", { exact: true, name: `Select ${listFirst.key}` }).check();
    await main.getByRole("checkbox", { exact: true, name: `Select ${listSecond.key}` }).check();
    await page.getByRole("button", { exact: true, name: "New epic from these" }).click();
    await page
      .getByRole("dialog", { name: "Name the new epic" })
      .getByRole("textbox", { name: "Name" })
      .fill(`The unanswered epic at ${at}`);
    await page.getByRole("button", { exact: true, name: "Create epic" }).click();
    const epicNotice = main.getByText(/The selected issues have not been moved/);
    await expect(epicNotice, `${where}: no unanswered-epic notice`).toBeVisible();
    await epicNotice.getByRole("button", { exact: true, name: "Dismiss" }).focus();
    await page.keyboard.press("Enter");
    await expect(epicNotice).toHaveCount(0);
    await after(where, "Enter on the unanswered epic's Dismiss");
    await page.unroute("**/api/v1/issues", keyCommitted);

    // The Delivery page's Window: Apply on a custom range sets a new window.
    where = `Delivery page at ${at}`;
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
    await main.getByLabel("To", { exact: true }).fill(`2024-06-0${step + 3}`);
    await main.getByRole("button", { exact: true, name: "Apply" }).focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`to=2024-06-0${step + 4}`));
    await after(where, "Enter on Apply over a custom range");

    // The Artifacts tab: a staged file's Upload holds the focus while the upload is out, and
    // then goes with the staged row.
    where = `Artifacts tab at ${at}`;
    await page.goto(`/issues/${issue}/artifacts`);
    await page.getByLabel("Upload artifact").setInputFiles({
      buffer: Buffer.from(`# Notes at ${at}\n`),
      mimeType: "text/markdown",
      name: `notes-${size.width}.md`,
    });
    const summary = main.getByRole("textbox", { exact: true, name: "Summary (optional)" });
    await expect(summary, `${where}: the file was not staged`).toBeVisible();
    await main.getByRole("button", { exact: true, name: "Upload" }).focus();
    await page.keyboard.press("Enter");
    await expect(summary, `${where}: the upload left the file staged`).toHaveCount(0);
    await after(where, "Enter on a staged file's Upload");
  }
  // The architect's ruling ff1a60f5, read here on the presses whose landing the page itself
  // makes: a press that disables, takes away or draws again the control it was made on hands the
  // focus on, and never to the body.
  expect(lost, lost.join("; ")).toEqual([]);
});

/** How many frames running a table's box or a panel holds still before its `scrollTop` is read
 *  (`settledScrollTop`): a wheel step's scroll lands over frames, and every hand-off of a loss
 *  runs within a frame and a task of it, so a box pulled back shows within three. */
const WHEEL_STILL_FRAMES = 3;

/** The row holding the keyboard focus, by the value of `rowAttribute` it carries, or null where
 *  the focus is in no such row. */
async function focusedRowOf(page: Page, rowAttribute: string): Promise<string | null> {
  return page.evaluate(
    (attribute) =>
      document.activeElement?.closest(`[${attribute}]`)?.getAttribute(attribute) ?? null,
    rowAttribute
  );
}

// SLOW(>=2s): 120 issues and 120 pull requests seeded, and two tables each scrolled by 25 wheel
// steps with a settled read after each.
// `@firefox` and `@webkit`: a focused row a virtual table takes away fires no `focusout` in these
// engines, and each engine lands a wheel step's scroll over its own frames.
// INFRA-3135's review round 1 (thread PRRT_kwDORFy7ds63p6y_): with a row's checkbox focused, 20
// wheel steps over a virtual table pulled its box back from 690 px to 24 px and moved the focus
// onto another row. The reader's scroll is theirs: the box goes where the wheel takes it. The row
// they picked leaves with the drawn window and takes the focus with it, and nothing hands the
// focus to a row they did not pick, so `x` then marks none.
test("a table that holds the keyboard focus scrolls freely, and the focus moves to no row the reader did not pick @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(150_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  const main = page.getByTestId("main-content");

  /** Sends 25 wheel steps of 100 px with the pointer over `box`, reads its `scrollTop` once each
   *  has settled, and answers those reads with what went wrong: a read more than 1 px above the
   *  one before it, the focus in any row but `picked`, and a box that ended less than 1,500 px
   *  below where it started. It answers where the focus ended too. */
  const wheelOver = async (box: Locator, rowAttribute: string, picked: string) => {
    await box.scrollIntoViewIfNeeded();
    // `scrollIntoViewIfNeeded` waited above for the box to be visible and stable, so it has a box.
    const area = await box.boundingBox();
    if (area === null) throw new Error("the table's box is not on screen");
    // Near the box's left edge, in the first cell's padding, where no row draws a link: a pointer
    // resting on a row's link opens that link's hover card, which takes the wheel steps after it
    // for itself. Measured in WebKit with the pointer at the box's middle: the box stopped at
    // 400 px for the last 20 steps.
    await page.mouse.move(area.x + 4, area.y + area.height / 2);
    const readings = [await settledScrollTop(box, WHEEL_STILL_FRAMES)];
    const wrong: string[] = [];
    for (let step = 1; step <= 25; step++) {
      await page.mouse.wheel(0, 100);
      // `settledScrollTop` waits until the box held still for three frames: that is the reading.
      const top = await settledScrollTop(box, WHEEL_STILL_FRAMES);
      const before = readings.at(-1) ?? 0;
      if (top < before - 1) wrong.push(`step ${step} pulled the box back from ${before} to ${top}`);
      readings.push(top);
      // Read after the box held still for three frames, by which every hand-off of a loss has run
      // (`WHEEL_STILL_FRAMES`), so the focus read is where the step left it.
      const focused = await focusedRowOf(page, rowAttribute);
      if (focused !== null && focused !== picked) {
        wrong.push(`after step ${step} the focus is in ${focused}`);
      }
    }
    const moved = (readings.at(-1) ?? 0) - (readings[0] ?? 0);
    if (moved < 1_500) wrong.push(`the box ended ${moved} px below where it started`);
    const ended = await page.evaluate(() => document.activeElement?.tagName ?? "nothing");
    return { ended, readings, wrong };
  };

  // The project List, on one page of 120 issues, far more than its box draws. Each title differs
  // from every other in a word of its own, since the server refuses a near copy of a title it
  // holds (409 POSSIBLE_DUPLICATE).
  await createProject({ key: "CORE", name: "Core" });
  for (let batch = 0; batch < 120; batch += 10) {
    await Promise.all(
      Array.from({ length: 10 }, (_, offset) =>
        createIssue({ project: "CORE", title: `Scrolled row ${batch + offset + 1}` })
      )
    );
  }
  await openIssuesTab(page, "CORE", "List");
  const pager = control(main, CONTROL.pager);
  await expect(pager).toContainText("120 of 120 issues");
  const perPage = listTrigger(pager, "Rows per page");
  await pickFromList(perPage, "250 per page");
  await expect(page).toHaveURL(/[?&]size=250/);
  const listBox = control(main, CONTROL.dataTable);
  const listRows = listBox.locator("[data-issue-row]");
  // `getAttribute` waits for the third row to attach, and a row carries its key from its first
  // render, so the key read is that row's own.
  const picked = (await listRows.nth(2).getAttribute("data-issue-row")) ?? "";
  const pickedBox = listBox.getByRole("checkbox", { exact: true, name: `Select ${picked}` });
  await pickedBox.click();
  const bar = control(main, CONTROL.selectionBar);
  await expect(bar).toContainText("1 selected");
  expect(await focusedRowOf(page, "data-issue-row"), "the click left the focus in no row").toBe(
    picked
  );
  const list = await wheelOver(listBox, "data-issue-row", picked);
  console.log(
    `project List, ${picked}'s checkbox clicked: ${list.readings.join(" ")}; the focus ends on ${list.ended}`
  );
  expect(list.wrong, `project List: ${list.wrong.join("; ")}`).toEqual([]);
  // `x` marks no row the reader did not pick: the row they marked is still the one marked, and
  // the only one, once the box is back at its top.
  await page.keyboard.press("x");
  await listBox.evaluate((node) => {
    node.scrollTop = 0;
  });
  await expect(pickedBox).toBeChecked();
  await expect(bar).toContainText("1 selected");

  // The Delivery list, over the delivery fixture and 120 more pull requests merged in its
  // window. A press on one of its rows opens that row's details, so the row takes the focus as
  // its keys give it one. It marks nothing, so its scroll and its focus are all there is to read.
  await seedDeliveryFixture();
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     )
     SELECT 'acme/widgets', 100 + n, 'feat: scrolled widget ' || n,
            'https://github.com/acme/widgets/pull/' || (100 + n), 'octocat',
            timestamptz '2024-06-01T04:00:00Z' + n * interval '1 minute',
            timestamptz '2024-06-01T06:00:00Z' + n * interval '1 minute',
            1, 1, false, null, ARRAY[]::text[], false
       FROM generate_series(1, 120) AS n`
  );
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  const prBox = control(main, CONTROL.dataTable);
  const pickedRow = prBox.locator("tbody tr[data-pr-row]").nth(2);
  await expect(pickedRow).toBeVisible();
  const pickedPR = (await pickedRow.getAttribute("data-pr-row")) ?? "";
  await pickedRow.focus();
  const delivery = await wheelOver(prBox, "data-pr-row", pickedPR);
  console.log(
    `Delivery list, ${pickedPR} focused: ${delivery.readings.join(" ")}; the focus ends on ${delivery.ended}`
  );
  expect(delivery.wrong, `Delivery list: ${delivery.wrong.join("; ")}`).toEqual([]);
});

// INFRA-3135's review round 1 (threads PRRT_kwDORFy7ds63p6zI and PRRT_kwDORFy7ds63p6zJ): a panel
// is portalled to the end of the body, outside the dialog it is opened in, so Tab from its last
// control went past the end of the page and on to the page behind the search bar; and after
// Shift+Tab out of the search bar's Project list every Escape was refused while the list stood
// open, so the bar could not be closed from the keyboard. And a list of the shared list's single
// pick closed by Tab or Shift+Tab out of its text box leaves the focus on its trigger, on a page
// and in a dialog (round 12). `@firefox` and `@webkit`: each engine keeps a Tab order of its own -
// a stop for each field of a date box, one for a radio group - and the panel follows the engine's,
// the clause of `playwright.config.ts` on the engine's own Tab order; and each engine takes the
// focus out of a text box the close removes in its own way, the clause on how the engine takes
// the focus out of one on Tab.
test("Tab and Escape never leave a panel for the page behind it, in a dialog or on a page @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const { issueKey } = await seedAsk("Which lease holds?");
  const main = page.getByTestId("main-content");

  /** Focuses `last`, the last control of its panel, and presses Tab until the focus has left it:
   *  a date box holds a stop for each of its fields, as many as the engine draws. The control is
   *  held by its handle, since the press that closes the panel takes it out of the page. */
  const tabOutOf = async (last: Locator) => {
    await last.focus();
    // `focus` above waited for the control to attach and focused it, so the handle names it.
    const handle = await last.elementHandle();
    for (let press = 0; press < 5; press++) {
      await page.keyboard.press("Tab");
      // A Tab moves the focus while the browser handles the press, so the instant after it shows
      // whether the focus has left the control.
      if (!(await handle?.evaluate((node) => node === document.activeElement))) break;
    }
    await handle?.dispose();
  };

  // On a page: the Inbox's Filter panel, whose last control is the Assignee filter.
  await page.goto("/");
  const inboxTrigger = control(controlRow(main), CONTROL.panelTrigger);
  const inboxPanel = await openFilterPanel(page, main);
  const assignee = inboxPanel.getByRole("button", { name: "Filter by assignee" });
  await tabOutOf(assignee);
  await expect(
    inboxPanel,
    "Inbox: Tab from the last control left the Filter panel open"
  ).toHaveCount(0);
  await expect(inboxTrigger, "Inbox: Tab from the panel's last control").toBeFocused();

  // In a dialog: the Inbox drawer's View menu, whose last control is its last section's choice.
  // The drawer's bar takes the narrow layout in every window 768 px wide or wider, where its
  // display menu is an item of More (R8): the menu opens from that item, and Tab from its last
  // choice puts the focus back on it, in More, which stays open under the menu it opened.
  await openIssuesTab(page, "CORE", "List");
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  await peekTrigger(page).click();
  const viewTrigger = await displayTrigger(page, "View", drawer(page));
  await viewTrigger.click();
  const viewMenu = page.getByRole("dialog", { name: "View" });
  await expect(viewMenu).toHaveCount(1);
  const lastChoice = viewMenu.getByRole("group").last().locator('input[type="radio"]:checked');
  await tabOutOf(lastChoice);
  await expect(
    viewMenu,
    "Inbox drawer: Tab from the last choice left the View menu open"
  ).toHaveCount(0);
  await expect(viewTrigger, "Inbox drawer: Tab from the View menu's last choice").toBeFocused();
  await expect(drawer(page)).toBeVisible();

  // In a dialog again: the search bar's Filter panel, whose last control is Updated since. The
  // key waits for the Inbox's own search box: pressed as the page loads, before the shell has
  // registered its keys, it opened nothing in WebKit (full lane 44466817714, passed on retry).
  await page.goto("/");
  await expect(page.getByRole("searchbox", { name: "Search asks" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  const palette = page.getByRole("dialog", { name: "Search" });
  await expect(palette).toBeVisible();
  const barTrigger = control(controlRow(palette), CONTROL.panelTrigger);
  const barPanel = await openFilterPanel(page, palette);
  const since = barPanel.getByLabel("Updated since");
  await tabOutOf(since);
  await expect(
    barPanel,
    "search bar: Tab from Updated since left the Filter panel open"
  ).toHaveCount(0);
  await expect(barTrigger, "search bar: Tab from the Filter panel's last control").toBeFocused();
  await expect(palette).toBeVisible();

  // Filter, then Project, then Shift+Tab out of the list: three presses of Escape close what is
  // left, the search bar last.
  await barTrigger.click();
  const barProject = listTrigger(page.getByRole("dialog", { name: "Filter" }), "Project");
  await barProject.click();
  await page.getByRole("combobox", { exact: true, name: "Search projects" }).focus();
  await page.keyboard.press("Shift+Tab");
  await expect(
    barProject,
    "search bar: Shift+Tab out of the Project list's text box"
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await expect(
    palette,
    "the search bar is still open after Shift+Tab and three Escapes"
  ).toHaveCount(0);

  // On a page: the issue header's Assignee list, which Tab out of its text box closes onto its
  // trigger, the badge.
  await page.goto(`/issues/${issueKey}`);
  const assigneeTrigger = listTrigger(page, `Assignee of ${issueKey}`);
  await assigneeTrigger.click();
  const assigneeBox = page.getByRole("combobox", { exact: true, name: "Search people" });
  await expect(assigneeBox).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(assigneeBox, "issue header: Tab left the Assignee list open").toHaveCount(0);
  await expect(
    assigneeTrigger,
    "issue header: Tab out of the Assignee list's text box"
  ).toBeFocused();
});

// SLOW(>=2s): the Delivery page opened four times, in each view, at two widths.
// INFRA-3135's review round 1 (threads PRRT_kwDORFy7ds63p6zB and PRRT_kwDORFy7ds63p6zE): on the
// Timeline, the view the page opens on, the row's panel held four of the nine facets and the row
// drew no chip or count for the other five, which `/delivery?author=x` still narrowed by; a
// value picked on a column's header in the List drew no chip in the row; and the List's arrows
// were answered while the list held any row, so ArrowDown with the focus on the page's body
// moved the focus into the list instead of scrolling the page.
test("the Delivery page reaches every facet from its row, in both views, and its list answers keys only on a focused row", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  // The delivery fixture with its reconcile verdict pinned, as `seedRealStackSurfaces` pins it,
  // and one more pull request merged in its window: another author's, written by a session, so
  // taking the Author chip off gives a pull request back and the Session facet has a value.
  await seedDeliveryFixture();
  await sql(
    `UPDATE delivery_settings
       SET last_error = 'the GitHub App is not installed on acme/widgets'
     WHERE singleton`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 3, 'feat: a hubot widget', 'https://github.com/acme/widgets/pull/3', 'hubot',
        '2024-06-01T04:00:00Z', '2024-06-01T05:00:00Z', 2, 1, false, null, ARRAY['e2e-delivery-row'], false)`
  );
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const trigger = control(row, CONTROL.panelTrigger);
  const shown = main.getByText(/^\d+ PRs in current filter\/window$/);

  // A link naming one of the five column facets, on the Timeline: its chip and its count are in
  // the row, and the chip takes it off.
  await page.goto(`/delivery?${DELIVERY_WINDOW}&author=octocat`);
  await expect(main.getByRole("img", { name: /^Delivery timeline/ })).toBeVisible();
  await expect(shown).toHaveText("2 PRs in current filter/window");
  const octocat = row.getByRole("button", { exact: true, name: "Remove Author: octocat filter" });
  await expect(octocat, "the Timeline draws no chip for ?author=").toBeVisible();
  await expect(trigger).toHaveText("Filter · 1");
  await octocat.click();
  await expect(shown).toHaveText("3 PRs in current filter/window");
  await expect(page).not.toHaveURL(/[?&]author=/);
  // In the bar's look the trigger says its count only while a narrowing is on (R8).
  await expect(trigger).toHaveText("Filter");

  // No column header is drawn on the Timeline, so its panel holds all nine.
  const panel = await openFilterPanel(page, main);
  for (const facet of [
    "Repository",
    "Session",
    "Priority",
    "Component",
    "Issue",
    "Parent agent",
    "Author",
    "Rework",
    "Deployed",
  ]) {
    await expect(
      panel.getByRole("button", { name: new RegExp(`^${facet}: `) }),
      `the Timeline's panel offers no ${facet}`
    ).toHaveCount(1);
  }
  await closeControlPanel(main);

  // In the List, Author is filtered on its column's header, and what it picks is a chip of the
  // row, whose press takes it off the header too.
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  const prs = page.locator("tbody tr[data-pr-row]");
  await expect(prs).toHaveCount(3);
  await main.getByRole("button", { exact: true, name: "Filter Author" }).click();
  await page
    .getByRole("listbox", { name: "Author options" })
    .getByRole("option", { name: /^hubot/ })
    .click();
  await page.keyboard.press("Escape");
  await expect(prs).toHaveCount(1);
  const hubot = row.getByRole("button", { exact: true, name: "Remove Author: hubot filter" });
  await expect(hubot, "the List draws no chip for its Author column's filter").toBeVisible();
  await expect(trigger).toHaveText("Filter · 1");
  await hubot.click();
  await expect(prs).toHaveCount(3);
  await expect(main.getByRole("button", { exact: true, name: "Filter Author" })).toBeVisible();

  // Below `xl` the list's box is three quarters of the window under the page's measures, so the
  // page scrolls. With the focus on the body, ArrowDown is the page's: it scrolls, and no row
  // takes the focus.
  await page.setViewportSize({ height: 768, width: 1024 });
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  await expect(prs).toHaveCount(3);
  await focusThePage(page);
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  await page.keyboard.press("ArrowDown");
  expect(
    await focusedRowOf(page, "data-pr-row"),
    "ArrowDown with the focus on the body put it in a row"
  ).toBeNull();
  await expect
    .poll(() => page.evaluate(() => window.scrollY), {
      message: "ArrowDown with the focus on the body did not scroll the page",
    })
    .toBeGreaterThan(0);
});

// INFRA-3135's review round 1 (thread PRRT_kwDORFy7ds63p6zU): Blocked on you counts the whole
// inbox, and its press kept a field narrowing and the search text on, so the list under it held
// fewer asks than the number it showed. Choosing it takes both off, as it takes the assignee off,
// and keeps the sort and the grouping.
test("with a field narrowing and a search on, choosing Blocked on you lists exactly the number it shows", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  // Three asks waiting on Alice. Only the first is P0, and only its question holds the word the
  // address searches for.
  const mine: string[] = [];
  for (const title of ["Ledger rollout", "Mirror retries", "Clock drift"]) {
    const issue = await createIssue({ project: "CORE", title });
    if (mine.length === 0) await patchIssue(issue.key, { priority: 0 });
    const ask = await createAsk(issue.key, { question: `${title}?` }, SURFACE_SESSION);
    mine.push(ask.id);
  }

  await page.goto("/?priority=P0&q=ledger&sort=urgency&group=waiting-on");
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(1);
  // The switch's Blocked on you carries the whole inbox's count, the three asks waiting on her.
  const blocked = page.getByRole("button", { name: /^Blocked on you: 3 items/ });
  await expect(blocked).toHaveText("Blocked on you 3");

  await blocked.click();
  await expect(page).toHaveURL(/[?&]section=needs-you(&|$)/);
  await expect(page, "the press left a field narrowing or the search on").not.toHaveURL(
    /[?&](priority|q)=/
  );
  await expect(page).toHaveURL(/[?&]sort=urgency(&|$)/);
  await expect(page).toHaveURL(/[?&]group=waiting-on(&|$)/);
  await expect(page.getByRole("searchbox", { name: "Search asks" })).toHaveValue("");
  await expect(
    rows,
    "the list holds a different number of asks than the control shows"
  ).toHaveCount(3);
  expect([...(await drawnIds(page))].sort()).toEqual([...mine].sort());
});

// SLOW(>=2s): an ask seeded, eight wheel steps over the panel, each scroll read once it has held
// still, and a Tab through every control the panel holds (3.8 s in Chromium, 4.6 s in Firefox,
// 5.8 s in WebKit).
// INFRA-3135's review round 1, R10: a panel with no room for itself on either side of its
// trigger is cut to the roomier side. To decide the side, the placement took the cut off the
// panel and read its height, which laid it out at its full height and clamped its own scroll to
// that layout's, after every render and at every scroll, the panel's own included: the wheel's
// scroll was pushed back at each step, and a Tab onto a control below the cut scrolled it into
// view only for the clamp to hide it again. `@firefox` and `@webkit`: each engine lays out a
// scroll box, lands a wheel step's scroll and clamps its scroll its own way, the clause of
// `playwright.config.ts` on how the engine lays out a scroll box.
test("a filter panel cut to a short window scrolls to its last control @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 340, width: 1440 });
  await seedAsk("Which lease holds?");
  await page.goto("/");
  const main = page.getByTestId("main-content");
  const trigger = control(controlRow(main), CONTROL.panelTrigger);
  const panel = await openFilterPanel(page, main);
  const measured = await panel.evaluate((node) => ({
    client: node.clientHeight,
    cut: (node as HTMLElement).style.maxHeight,
    holds: node.scrollHeight,
  }));
  const under = await trigger.boundingBox();
  console.log(
    `Filter panel at 1440x340, under a trigger at ${under?.y}-${(under?.y ?? 0) + (under?.height ?? 0)} px: cut to ${measured.cut || "nothing"}, ${measured.client} px shown of ${measured.holds}`
  );
  expect(measured.cut, "the window is not short enough to cut the Filter panel").not.toBe("");
  const wrong: string[] = [];

  await panel.scrollIntoViewIfNeeded();
  // `scrollIntoViewIfNeeded` waited above for the panel to be visible and stable, so it has a box.
  const area = await panel.boundingBox();
  if (area === null) throw new Error("the Filter panel is not on screen");
  await page.mouse.move(area.x + area.width / 2, area.y + area.height / 2);
  // The wheel over the panel, more steps than it has room to move.
  for (let step = 0; step < 8; step++) await page.mouse.wheel(0, 100);
  // `settledScrollTop` waits until the panel held still for three frames: that is the reading.
  const top = await settledScrollTop(panel, WHEEL_STILL_FRAMES);
  // Read after the panel held still above, so its layout and the room it scrolls are settled.
  const end = await panel.evaluate((node) => node.scrollHeight - node.clientHeight);
  if (top < end - 1) wrong.push(`the wheel left the panel at ${top} px of the ${end} it scrolls`);

  // Tab from the control the panel opened on to its last, the Assignee filter, which is then in
  // sight inside the panel. The control is held by its handle, in case a Tab closes the panel;
  // `elementHandle` waits for the filter to attach in the panel opened above.
  const last = await panel.getByRole("button", { name: "Filter by assignee" }).elementHandle();
  let reached = false;
  for (let press = 0; press < 30 && !reached; press++) {
    await page.keyboard.press("Tab");
    // A Tab moves the focus while the browser handles the press, so the instant after it shows
    // where the focus went.
    reached = await last.evaluate((node) => node === document.activeElement);
  }
  if (!reached) wrong.push("Tab never reached the Assignee filter");
  await settledScrollTop(panel, WHEEL_STILL_FRAMES);
  // Read after the panel's scroll held still for three frames above, so this is where it settled.
  const shown = await last.evaluate((node) => {
    const own = node.getBoundingClientRect();
    const box = node.closest('[role="dialog"]')?.getBoundingClientRect();
    return {
      inside: box !== undefined && own.top >= box.top - 1 && own.bottom <= box.bottom + 1,
      place: `${Math.round(own.top)}-${Math.round(own.bottom)} px in a panel at ${Math.round(box?.top ?? 0)}-${Math.round(box?.bottom ?? 0)} px`,
    };
  });
  if (reached && !shown.inside)
    wrong.push(`the focused Assignee filter is out of sight: ${shown.place}`);
  await last.dispose();
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135's review round 1, F2 item 1 (the architect's chip rule, restated 2026-11-15): a
// column filter showed only on its column's header, and the columns menu hides any column, so a
// reader could hide the column a link had narrowed by and be left with a list narrowed by a
// filter they could neither see nor take off. Every narrowing that is on is a chip of the row,
// whatever set it, and the row's trigger counts it.
test("a column filter is visible and removable from the row even while its column is hidden", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  for (const [title, label] of [
    ["Ledger rollout", "frontend"],
    ["Mirror retries", "backend"],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { labels: [label] });
  }
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const trigger = control(row, CONTROL.panelTrigger);
  const rows = page.locator("[data-issue-row]");
  const chip = row.getByRole("button", { exact: true, name: "Remove Label: frontend filter" });

  // A link narrowing by a label: one row, one chip, and the trigger counts it.
  await openIssuesTab(page, "CORE", "List");
  await page.goto("/projects/CORE/issues?label=frontend");
  await expect(rows).toHaveCount(1);
  await expect(chip).toBeVisible();
  await expect(trigger).toHaveText("Filters · 1");

  // The reader hides the Labels column: its header, and the filter control on it, leave the
  // page, and the narrowing stays on.
  await control(row, CONTROL.columnsMenu).click();
  await page.getByRole("checkbox", { exact: true, name: "Labels" }).click();
  await page.keyboard.press("Escape");
  await expect(main.getByRole("button", { name: /^Filter Labels/ })).toHaveCount(0);
  await expect(rows).toHaveCount(1);

  // The chip is the one place that narrowing is now, and its press takes it off.
  await expect(chip, "no chip is left for a filter whose column is hidden").toBeVisible();
  await expect(trigger).toHaveText("Filters · 1");
  await chip.click();
  await expect(rows).toHaveCount(2);
  await expect(page).not.toHaveURL(/[?&]label=/);
  // In the bar's look the trigger says its count only while a narrowing is on (R8).
  await expect(trigger).toHaveText("Filters");
  await expect(chip).toHaveCount(0);
});

// SLOW(>=2s): 60 issues seeded, the List opened, a middle row marked and unmarked and then closed
// by a second person through the API.
// INFRA-3135's review round 1, follow-up F4 (thread PRRT_kwDORFy7ds63p6y_): a focused row a live
// update removes hands the focus to no other row and scrolls nothing, though the reader's last
// press was on that row: a press is the cause only of the update that answers it, and the
// reader's two clicks on the row's checkbox have been answered, the selection bar drawn and gone
// again, before the second person's close arrives. `@firefox` and `@webkit`: a focused row taken
// away fires no `focusout` in these engines, the clause of `playwright.config.ts` on a focused
// control taken away.
test("a row a live update removes while it holds the focus hands the focus to no other row and scrolls nothing @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  // More rows than the box draws, each title its own word, since the server refuses a near copy of
  // a title it holds (409 POSSIBLE_DUPLICATE).
  for (let batch = 0; batch < 60; batch += 10) {
    await Promise.all(
      Array.from({ length: 10 }, (_, offset) =>
        createIssue({ project: "CORE", title: `Removed row ${batch + offset + 1}` })
      )
    );
  }
  // The List opens on the open statuses and not done (`openIssueStatuses`), so closing an issue as
  // another person takes its row off this view; the List view is opened first, since a project
  // opens on whichever view the reader last left.
  await openIssuesTab(page, "CORE", "List");
  const main = page.getByTestId("main-content");
  const listBox = control(main, CONTROL.dataTable);
  await expect(listBox.locator("[data-issue-row]").first()).toBeVisible();
  // Scroll the box so a row in the middle is in sight, and pick it.
  await listBox.evaluate((node) => {
    node.scrollTop = Math.round(node.scrollHeight / 2);
  });
  await settledScrollTop(listBox, WHEEL_STILL_FRAMES);
  // Read after the box held still for three frames above, so the rows in sight are settled.
  const picked = await listBox.evaluate((node) => {
    const within = node.getBoundingClientRect();
    const mid = [...node.querySelectorAll<HTMLElement>("[data-issue-row]")].find((row) => {
      const box = row.getBoundingClientRect();
      return box.top >= within.top + 40 && box.bottom <= within.bottom - 40;
    });
    return mid?.dataset.issueRow ?? null;
  });
  if (picked === null) throw new Error("no row sits wholly in the middle of the box");
  const pickedBox = listBox.getByRole("checkbox", { exact: true, name: `Select ${picked}` });
  const bar = control(main, CONTROL.selectionBar);
  // The reader presses in the row itself: a click on its checkbox marks it, and a second click
  // unmarks it. WebKit leaves a clicked checkbox unfocused, so the focus is then put on it with no
  // press, which leaves it there in every engine alike.
  await pickedBox.click();
  await expect(bar).toBeVisible();
  await pickedBox.click();
  await pickedBox.evaluate((node) => (node as HTMLElement).focus({ preventScroll: true }));
  await expect(pickedBox).toBeFocused();
  // The second click's update is drawn - the selection bar gone - before the close is sent, so
  // that press has been answered by the time the live update takes the row away.
  await expect(bar).toHaveCount(0);
  const beforeTop = await listBox.evaluate((node) => node.scrollTop);
  const beforeY = await page.evaluate(() => window.scrollY);
  // A second person closes the issue through the API; the live update takes its row off the view.
  await patchIssue(picked, { status: "done" }, { login: "bob" });
  await expect(page.locator(`[data-issue-row="${picked}"]`)).toHaveCount(0);
  // The focus is in no row, and nothing scrolled.
  const inRow = await page.evaluate(
    () => document.activeElement?.closest("[data-issue-row]") != null
  );
  const afterTop = await listBox.evaluate((node) => node.scrollTop);
  const afterY = await page.evaluate(() => window.scrollY);
  expect(inRow, "the focus landed in a row the reader did not pick").toBe(false);
  expect(Math.abs(afterTop - beforeTop), "the box scrolled").toBeLessThanOrEqual(1);
  expect(Math.abs(afterY - beforeY), "the window scrolled").toBeLessThanOrEqual(1);
  // `x` marks nothing: no row holds the focus for it to act on, so no bar appears.
  await page.keyboard.press("x");
  await expect(bar).toHaveCount(0);
});

// SLOW(>=2s): two surfaces, each with three questions seeded and one withdrawn by its asker.
// INFRA-3135, round 17 (the tester's, measured in builds Job infra-3135-r17h2 at e4d8b3b1): the
// reader is typing an answer in the Inbox's list layout - the Inbox page below 1280 px, and the
// drawer at any width - when the asker withdraws the question. The card swaps to its record
// under them, a loss they did not cause, and the record draws a Reply box where the answer form
// was. At e4d8b3b1 Chromium leaves the focus in that Reply box, in three runs of three, so the
// reader's next `j` is typed into it. At 90fa8211 and at the merge base bacd5d7e the focus goes to
// the question's row and `j` steps to the next row, in Chromium, Firefox and WebKit alike. The
// architect's rule of 2026-11-19 (13:0xZ): a loss the reader did not cause never lands in a field
// that takes typed text. A focused control taken away, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus
// row (e2e/playwright.config.ts).
test("an ask withdrawn while the reader types its answer in the Inbox's list takes none of their next keys as text @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  const typed = "Only the last lease, on a cold start.";
  await createProject({ key: "CORE", name: "Core" });
  const wrong: string[] = [];
  for (const surface of ["page at 390x664", "drawer at 1440x900"] as const) {
    const ids: string[] = [];
    for (const title of ["Keep going?", "Ship it?", "Describe the lease format."]) {
      const issue = await createIssue({ project: "CORE", title: `${surface}: ${title}` });
      await patchIssue(issue.key, { assignee: "alice" });
      ids.push((await createAsk(issue.key, { question: title }, SURFACE_SESSION)).id);
    }
    // The Inbox lists newest first: the question the reader answers is the first row.
    const withdrawn = ids[2] as string;
    let root: Page | Locator = page;
    if (surface === "page at 390x664") {
      await page.setViewportSize({ height: 664, width: 390 });
      await page.goto("/");
    } else {
      await page.setViewportSize({ height: 900, width: 1440 });
      await page.goto("/settings");
      await expect(page.getByRole("heading", { exact: true, name: "Agent tokens" })).toBeVisible();
      await page.keyboard.press("i");
      root = drawer(page);
      await expect(root).toBeVisible();
    }
    const card = root.getByTestId(`ask-${withdrawn}`);
    const answer = card.locator("[data-ask-answer]");
    await answer.click();
    await answer.fill(typed);
    await expect(answer).toBeFocused();
    await resolveAsk(
      withdrawn,
      { kind: "retracted", reason: "Answered on another issue." },
      SURFACE_SESSION
    );
    // The card shows its closed state, or its row has gone from the list.
    await expect
      .poll(() =>
        page.evaluate((id) => {
          const shown = document.querySelector(`[data-testid="ask-${id}"]`);
          return (
            shown === null || shown.querySelector('[data-testid="ask-resolution-badge"]') !== null
          );
        }, withdrawn)
      )
      .toBe(true);
    await expect(answer).toHaveCount(0);
    // The card's own hand-off runs in the commit, and a component's focus effect after its
    // commit: both are done by now.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: every hand-off of the loss has run, and nothing else moves the focus.
    const holder = await focusHolder(page);
    if (holder.takesText)
      wrong.push(`${surface}: the focus is in ${holder.name}, which takes text`);
    await page.keyboard.press("j");
    await nextFrames(page);
    // One read after the key's frames is the answer: a key typed into a box is there at once.
    const typedInto = await page.evaluate(() =>
      [...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>("textarea, input")]
        .filter((field) => field.value.includes("j"))
        .map(
          (field) =>
            field.getAttribute("aria-label") ||
            field.labels?.[0]?.textContent?.trim() ||
            field.getAttribute("placeholder") ||
            field.tagName.toLowerCase()
        )
    );
    if (typedInto.length > 0) {
      wrong.push(`${surface}: j was typed into ${typedInto.join(", ")}`);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two widths, each with an issue and four comments seeded and one resolved by its
// author.
// INFRA-3135, round 18 (the tester's, measured in builds Jobs infra-3135-r18xh, -r18xp and
// -r18xb at e9ed2743, 90fa8211 and the merge base a20e0b05): the reader has narrowed an issue's
// Conversation to Open and holds the focus on a comment's Reply when the comment's author
// resolves it, so the filter takes the turn away, a loss the reader did not cause. At e9ed2743
// and at 90fa8211 the focus goes into the next turn - its Pin, its Reply, its Expand thread or its
// body - at 1440 and at 390 px in Chromium, Firefox and WebKit, six runs of six: a turn carries no
// row marker (`components/row-marker.ts`), so the app-wide hand-off those commits had takes the
// next turn's control for a control in no row. At a20e0b05 the focus goes to the page body in all
// six.
// The architect's rule of 2026-11-19: a loss the reader did not cause moves the focus into no
// other row of a list. A focused control taken away, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus row
// (e2e/playwright.config.ts).
test("a Conversation turn resolved elsewhere while its Reply holds the focus hands the focus into no other turn @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    const issue = await createIssue({ project: "CORE", title: `Conversation at ${size.width} px` });
    const ids: string[] = [];
    for (const turn of [1, 2, 3, 4]) {
      const made = await createComment(
        issue.key,
        { body: `Turn ${turn}: a thought.` },
        SURFACE_SESSION
      );
      ids.push(made.id);
    }
    await page.setViewportSize(size);
    await page.goto(`/issues/${issue.key}/conversation`);
    const resolved = page.locator(`[data-turn="comment:${ids[1]}"]`);
    await expect(resolved).toBeVisible();
    const open = page.getByRole("button", { exact: true, name: "Open" });
    await open.click();
    await expect(open).toHaveAttribute("aria-pressed", "true");
    const reply = resolved.getByRole("button", { exact: true, name: "Reply" });
    await reply.focus();
    await expect(reply).toBeFocused();
    await resolveComment(ids[1] as string, SURFACE_SESSION);
    await expect(resolved).toHaveCount(0);
    // Every hand-off of the loss runs within a frame and a task of it, so it is done by now.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: every hand-off of the loss has run, and nothing else moves the focus.
    const holder = await page.evaluate(() => {
      const active = document.activeElement;
      const named =
        active?.getAttribute("aria-label") ||
        active?.textContent?.replace(/\s+/g, " ").trim().slice(0, 40);
      return {
        name: `${active?.tagName.toLowerCase()} "${named}"`,
        turn: active?.closest("[data-turn]")?.getAttribute("data-turn") ?? null,
      };
    });
    if (holder.turn !== null) {
      wrong.push(`at ${size.width} px: the focus is on ${holder.name} in the turn ${holder.turn}`);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two widths, each with an issue and three comments pinned, and one card unpinned.
// INFRA-3135, round 19 (the tester's, measured in builds Jobs infra-3135-r19h and -r19b at
// 89072a56 and e9ed2743): the reader holds the focus on a pinned card's Unpin on the issue
// margin's Pinned tab when their other device unpins that card, a loss the reader did not cause.
// At both commits the focus goes into the next card - its Unpin in Chromium, its author link in
// Firefox and WebKit - at 1440 and 1280 px: there the tab's cards, each an <article> in a <div>
// (`features/margin/PinnedTab.tsx`), were no row by the app-wide hand-off's rule at those commits
// and carried no marker (`components/row-marker.ts`), so that hand-off took the next card's
// control for a control in no row. The architect's rule of 2026-11-20 (open-items.md, "an
// uncaused loss does what main does"): a loss the reader did not cause leaves the focus on the
// body, as main does,
// but in an open modal dialog, which takes it itself; so it lands in no card. A focused control
// taken away, for which Firefox and WebKit fire no `focusout`: the clause of the `webkit` and
// `firefox` projects that names the page-own focus row (e2e/playwright.config.ts).
test("a pinned card unpinned on another device while its Unpin holds the focus hands the focus into no other card @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
  ]) {
    const issue = await createIssue({ project: "CORE", title: `Pinned at ${size.width} px` });
    const markers: string[] = [];
    for (const turn of [1, 2, 3]) {
      const made = await createComment(
        issue.key,
        { body: `Pinned turn ${turn}.` },
        SURFACE_SESSION
      );
      markers.push(`pinned_items:comment:${made.id}`);
    }
    // The reader's own state of the issue, as their other device reads it before it writes.
    const state = async () => {
      const mine = (await getMyState("alice"))[issue.key];
      return { dismissed: mine?.dismissed ?? [], seq: mine?.seq ?? 0 };
    };
    const before = await state();
    await putIssueState(
      issue.key,
      { dismissed: [...before.dismissed, ...markers], seq: before.seq + 1 },
      { login: "alice" }
    );
    await page.setViewportSize(size);
    await page.goto(`/issues/${issue.key}`);
    const margin = page.getByTestId("margin-sheet");
    await margin.getByRole("tab", { name: "Pinned" }).click();
    const cards = margin.getByRole("article");
    await expect(cards).toHaveCount(3);
    const second = cards.filter({ hasText: "Pinned turn 2." });
    const unpin = second.getByRole("button", { name: "Unpin" });
    await unpin.focus();
    await expect(unpin).toBeFocused();
    const now = await state();
    await putIssueState(
      issue.key,
      { dismissed: now.dismissed.filter((marker) => marker !== markers[1]), seq: now.seq + 1 },
      { login: "alice" }
    );
    await expect(second).toHaveCount(0);
    // Every hand-off of the loss runs within a frame and a task of it, so it is done by now.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: every hand-off of the loss has run, and nothing else moves the focus.
    const holder = await page.evaluate(() => {
      const active = document.activeElement;
      const named =
        active?.getAttribute("aria-label") ||
        active?.textContent?.replace(/\s+/g, " ").trim().slice(0, 40);
      return {
        card:
          active
            ?.closest('[data-testid="margin-sheet"] article')
            ?.textContent?.replace(/\s+/g, " ")
            .trim()
            .slice(0, 30) ?? null,
        name: `${active?.tagName.toLowerCase()} "${named}"`,
      };
    });
    if (holder.card !== null) {
      wrong.push(
        `at ${size.width} px: the focus is on ${holder.name} in the card "${holder.card}"`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the search bar and its Filter panel opened twice, for a typed date and a set one.
// The Filter panel draws its Updated since box only while no date is set (`SearchPalette.tsx`),
// and the box takes a typed date once its year has four digits, so the reader's own date takes
// the focused box away. A date set or typed in the box is the reader's press, whether the engine
// fires `change` before the page drops the box (Chromium, for a typed date) or `input` alone
// (Firefox, as the last digit completes the date, and Playwright's fill(), the value set without
// keys as assistive technology sets it). A caused walk-back passes over Author, which takes typed
// text, so the reader's date keeps the focus inside the Filter panel on no text field; a loss
// misread as uncaused leaves the focus on the page's body, outside that panel. WebKit's date box
// takes no typed date from Playwright's keys, so the typed case is not read there. A focused
// control taken away, for which Firefox and WebKit fire no `focusout`: the clause of the `webkit`
// and `firefox` projects that names the page-own focus row (e2e/playwright.config.ts).
test("a date the reader gives the search bar's Updated since is their press, so the focus stays in the Filter panel @firefox @webkit", async ({
  browserName,
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Harbour lights" });
  await page.setViewportSize({ height: 900, width: 1440 });
  const wrong: string[] = [];
  for (const how of ["typed", "set"] as const) {
    if (how === "typed" && browserName === "webkit") continue;
    await page.goto("/");
    await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
    await page.keyboard.press("Control+k");
    const bar = page.getByRole("dialog", { name: "Search" });
    await expect(bar).toBeVisible();
    await page.keyboard.type("Harbour");
    await control(controlRow(bar), CONTROL.panelTrigger).click();
    const filters = page.getByRole("dialog", { name: "Filter" });
    // The label whole: once the date is set, the top of the panel lists its chip, whose
    // "Remove Updated since …" holds these words too, and the chip is not the box.
    const since = filters.getByLabel("Updated since", { exact: true });
    await expect(since).toBeVisible();
    const day = await page.evaluate(() => {
      const next = new Date();
      next.setDate(next.getDate() + 1);
      const pad = (value: number) => String(value).padStart(2, "0");
      return {
        iso: `${next.getFullYear()}-${pad(next.getMonth() + 1)}-${pad(next.getDate())}`,
        typed: `${pad(next.getMonth() + 1)}${pad(next.getDate())}${next.getFullYear()}`,
      };
    });
    if (how === "typed") {
      await since.focus();
      await page.keyboard.type(day.typed);
    } else {
      await since.fill(day.iso);
    }
    await expect(since).toHaveCount(0);
    // Every hand-off of the loss runs within a frame and a task of it, so it is done by now.
    await nextFrames(page);
    await nextFrames(page);
    // The answer is read now: every hand-off of the loss has run, and nothing else moves the focus.
    const holder = await focusHolder(page);
    // Read after the same hand-offs as the holder above, so where the focus is is settled.
    const inFilter = await filters.evaluate((panel) => panel.contains(document.activeElement));
    if (!inFilter)
      wrong.push(`a ${how} date: the focus is on ${holder.name}, outside the Filter panel`);
    else if (holder.takesText)
      wrong.push(`a ${how} date: the focus is in ${holder.name}, which takes text`);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two free-text questions seeded and one snoozed from the reading column's head.
// INFRA-3135, round 20 (the tester's, measured in builds Jobs infra-3135-r20ph, -r20pm and -r20rh
// at 46b75d84 and main's 1c7c8253): at 1440 px the reader snoozes the question the reading
// column shows, by its Snooze in the column's head, a loss the reader caused. Once the write
// lands the row leaves and the column shows the next question. At 46b75d84 the focus then lands
// in that question's own answer box, so the reader's next j, a binding R6 keeps, is typed into
// the answer: 9 of 9 runs in Chromium, Firefox and WebKit (r20rh). At main the focus falls to the
// body and j steps the column's selection. The architect's rule of 2026-11-19 13:0xZ
// (open-items.md, "The replacement rule, narrowed"): in the reading column the focus goes to the
// column's own focus stop, whoever caused the loss. A focused control taken away, for which
// Firefox and WebKit fire no `focusout`: the clause of the `webkit` and `firefox` projects that
// names the page-own focus row (e2e/playwright.config.ts).
test("a Snooze pick in the reading column's head takes none of the reader's next keys as text @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const ids: string[] = [];
  for (const question of ["Describe the lease format.", "Describe the handover."]) {
    const issue = await createIssue({ project: "CORE", title: `Reading column: ${question}` });
    await patchIssue(issue.key, { assignee: "alice" });
    ids.push((await createAsk(issue.key, { question }, SURFACE_SESSION)).id);
  }
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto("/");
  // The Inbox lists newest first: the question the reader snoozes is the first row.
  const snoozed = ids[1] as string;
  const snooze = listTrigger(await askControls(page, snoozed), /^Snooze /);
  await snooze.focus();
  await pickFromList(snooze, "Later today");
  await expect(inboxRow(page, snoozed)).toHaveCount(0);
  // Every hand-off of the loss runs within a frame and a task of it, so it is done by now.
  await nextFrames(page);
  await nextFrames(page);
  const wrong: string[] = [];
  // One read is the answer: every hand-off of the loss has run, and nothing else moves the focus.
  const holder = await focusHolder(page);
  if (holder.takesText) wrong.push(`the focus is in ${holder.name}, which takes text`);
  await page.keyboard.press("j");
  await nextFrames(page);
  // One read after the key's frames is the answer: a key typed into a box is there at once.
  const typedInto = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>("textarea, input")]
      .filter((field) => field.value.includes("j"))
      .map(
        (field) =>
          field.getAttribute("aria-label") ||
          field.labels?.[0]?.textContent?.trim() ||
          field.getAttribute("placeholder") ||
          field.tagName.toLowerCase()
      )
  );
  if (typedInto.length > 0) wrong.push(`j was typed into ${typedInto.join(", ")}`);
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): three priorities and two asks seeded, and two pages opened.
// INFRA-3135's review round 1, follow-up F5: the display menu replaced the Priorities page's
// grouping select, where `j` and `k` did nothing, with radios, which take no text, so `j` on one
// of them walked the table behind the open menu. At 1440 px the Inbox's `j` steps its reading
// column's selection rather than the focus, so there it is the selection that must not move.
// No engine tag: the rule reads the element the key is pressed on, which is the focused element in
// every engine, and the row reads that the focus is on the menu's radio before it presses `j`, so
// another engine would repeat what Chromium shows (`playwright.config.ts`, the comment above
// `tagged`).
test("a page's single-key shortcuts do not act from inside an open panel", async ({ page }) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  // Written one after another, since each takes the last place of the one order.
  for (const title of ["Ship the daemon", "Cut the review queue", "Retire the old ledger"]) {
    await createPriority({ owner: "alice", title });
  }
  const menu = page.getByRole("dialog", { name: "View" });
  /** Opens the View menu from its trigger with the keyboard, and checks the focus went to one of
   *  its radios. */
  const openFromKeyboard = async (trigger: Locator) => {
    await trigger.focus();
    await page.keyboard.press("Enter");
    await expect(menu).toHaveCount(1);
    await expect(menu.getByRole("radio").first()).toBeFocused();
  };
  /** Presses `j` and reads, two frames later, whether the focus is still in the View menu and
   *  whether it is in a row `rows` names. */
  const pressJ = async (rows: string) => {
    await page.keyboard.press("j");
    await nextFrames(page);
    return page.evaluate(
      (selector) => ({
        inPanel: document.activeElement?.closest('[role="dialog"][aria-label="View"]') != null,
        inRow: document.activeElement?.closest(selector) != null,
      }),
      rows
    );
  };

  await page.goto("/priorities");
  const priorities = page.locator('section[aria-label="Priorities"]');
  await expect(priorities.locator("[data-priority-row]")).toHaveCount(3);
  await openFromKeyboard(control(controlRow(priorities), CONTROL.displayMenu));
  expect(await pressJ("[data-priority-row]"), "Priorities: j on the View menu's radio").toEqual({
    inPanel: true,
    inRow: false,
  });
  await expect(menu, "Priorities: j closed the View menu").toHaveCount(1);

  const { issueKey } = await seedAsk("Which lease holds?");
  await createAsk(issueKey, { question: "Which mirror replays first?" }, asSession());
  await page.goto("/");
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(2);
  // The two rows were counted above, and a row carries its id from its first render.
  const top = await rows.first().getAttribute("data-inbox-row");
  if (top === null) throw new Error("the Inbox's first row carries no id");
  await selectRow(page, top);
  const main = page.getByTestId("main-content");
  await openFromKeyboard(control(controlRow(main), CONTROL.displayMenu));
  expect(await pressJ("[data-inbox-row]"), "Inbox: j on the View menu's radio").toEqual({
    inPanel: true,
    inRow: false,
  });
  await expect(selectedRow(page), "Inbox: j moved the reading column's selection").toHaveAttribute(
    "data-inbox-row",
    top
  );
  await expect(menu, "Inbox: j closed the View menu").toHaveCount(1);
});

/** What a text box is, in the count R3 reads inside a list's own panel: each kind of field a
 *  reader types into. */
const TEXT_BOXES = [
  "input:not([type])",
  'input[type="text"]',
  'input[type="search"]',
  "textarea",
  '[role="combobox"]',
  '[role="searchbox"]',
  '[role="textbox"]',
].join(", ");

/** One list of options R3 holds to a text box, as the row below drives it at one width. */
interface PickList {
  /** The list as a failure names it: its page, and its trigger's words. */
  readonly name: string;
  /** Brings the page to where the trigger is drawn; absent where the list read before this one
   *  leaves it drawn. */
  readonly reach?: () => Promise<void>;
  readonly trigger: Locator;
  /** The list's value, where a reader reads it: the trigger's words, or - where a pick takes the
   *  trigger away - the chip the pick puts in the row. */
  readonly value: () => Promise<string>;
  /** A word a reader types into the list's text box, and the options it leaves, in order. */
  readonly word: string;
  readonly left: readonly (string | RegExp)[];
  /** What `value` reads once Enter has picked the option the word left. */
  readonly picked: RegExp;
  /** Where the pick leaves the focus, where that is not the trigger: the control the panel
   *  hands it to when the pick takes the trigger away, or the message the composer's pick hands
   *  the reader to. Null where the row reads no focus after the pick: no hand-off of the page's
   *  own lands the focus the pick took away. */
  readonly focusAfterPick?: Locator | null;
  /** Where Escape leaves the focus, where that is not the trigger: the composer's row. */
  readonly focusAfterEscape?: Locator;
}

/** What the lists are read over, as `seedPickLists` made it. */
interface PickListSeed {
  /** One question waiting on alice for each width, since a width's pick hands its own on. */
  readonly asks: readonly string[];
  /** The one member of the first epic, which the List's selection marks. */
  readonly member: string;
  /** The issue whose spec, and whose `notes.md`, are each in two versions. */
  readonly documented: string;
  /** The issue whose header's Assignee, Repository and Edit priorities are read, assigned to
   *  alice and inheriting CORE's default repository. */
  readonly header: string;
  /** For each width, three questions waiting on alice that the Inbox lists newest first, one
   *  after another: the one a row's Snooze defers, the one the selection bar's Snooze defers, and
   *  the one that moves up into its place. */
  readonly snoozes: readonly {
    readonly after: string;
    readonly bulk: string;
    readonly row: string;
  }[];
  /** For each width, an issue of CORE at P3 with a question waiting on alice, whose priority the
   *  List's row, the Board's card and the Inbox's question pick in turn, and that question. */
  readonly priorities: readonly { readonly ask: string; readonly issue: string }[];
}

/** The Agents page's two live sessions and CORE's Keyboard issue and Rollout plan
 *  (`seedAgents`); a second project, OPS, which carol makes, so the search bar's and the Create
 *  issue dialog's Project lists and the Settings page's Source project list hold more than one
 *  project, OPS names carol as its default assignee, and the people a question can be handed to,
 *  an issue assigned to, or a project named by, are alice, bob and carol; three epics, the first
 *  with a member; a spec and a second document of one issue, each in two versions; a question for
 *  each width; CORE's default repository, acme/widgets; three priorities; an issue of CORE
 *  assigned to alice, which names no repository of its own and advances no priority; for each
 *  width an issue of CORE at P3 with a question; the Delivery page's fixture
 *  (`seedDeliveryFixture`), which adds project ACME; and, last, so the Inbox lists them first,
 *  three questions for each width that the Snoozes defer. */
async function seedPickLists(): Promise<PickListSeed> {
  await seedAgents();
  await createProject({ key: "OPS", name: "Ops" }, { login: "carol" });
  const epics: string[] = [];
  for (const title of ["Lease epic", "Mirror epic", "Ledger epic"]) {
    const epic = await createIssue({ project: "CORE", title });
    await patchIssue(epic.key, { labels: ["epic"] });
    epics.push(epic.key);
  }
  const member = await createIssue({ project: "CORE", parent: epics[0], title: "Handover step" });
  const documented = await createIssue({
    project: "CORE",
    spec: "The lease passes at noon.\n",
    title: "Lease handover",
  });
  const notes = await createIssueArtifact(documented.key, {
    content: "The ledger keeps three leases.\n",
    name: "notes.md",
  });
  for (const [id, find, replacement] of [
    [documented.primary_artifact_id, "noon", "midnight"],
    [notes.artifact.id, "three", "seven"],
  ] as const) {
    await editArtifact(id, { ops: [{ find, op: "replace", with: replacement }] }, { as: "agent" });
    await expect
      .poll(async () => ((await getArtifact(id)).versions ?? []).length)
      .toBeGreaterThanOrEqual(2);
  }
  const asks: string[] = [];
  for (const width of [1440, 390]) {
    const owner = await createIssue({ project: "CORE", title: `Lease question at ${width}` });
    const ask = await createAsk(
      owner.key,
      { question: `Who holds the lease at ${width}?` },
      SURFACE_SESSION
    );
    asks.push(ask.id);
  }
  await putProjectDefaultRepository("CORE", "acme/widgets");
  for (const title of ["Ship the daemon", "Cut the review queue", "Retire the old ledger"]) {
    await createPriority({ owner: "alice", title });
  }
  const header = await createIssue({ project: "CORE", title: "Harbour crane" });
  await patchIssue(header.key, { assignee: "alice" });
  const priorities: { ask: string; issue: string }[] = [];
  for (const width of [1440, 390]) {
    const owner = await createIssue({
      priority: 3,
      project: "CORE",
      title: `Priority picked at ${width}`,
    });
    const ask = await createAsk(
      owner.key,
      { question: `How urgent is it at ${width}?` },
      SURFACE_SESSION
    );
    priorities.push({ ask: ask.id, issue: owner.key });
  }
  await seedDeliveryFixture();
  const snoozes: { after: string; bulk: string; row: string }[] = [];
  for (const width of [1440, 390]) {
    // Made oldest first, so the Inbox lists the row's question, the bar's, then the one after.
    const ids: string[] = [];
    for (const part of ["moves up", "bar", "row"]) {
      const owner = await createIssue({ project: "CORE", title: `Snooze ${part} at ${width}` });
      const question = `Defer the ${part} at ${width}?`;
      ids.push((await createAsk(owner.key, { question }, SURFACE_SESSION)).id);
    }
    const [after = "", bulk = "", row = ""] = ids;
    snoozes.push({ after, bulk, row });
  }
  return {
    asks,
    documented: documented.key,
    header: header.key,
    member: member.key,
    priorities,
    snoozes,
  };
}

/** The twenty-five single-pick lists, in the order the row reads them at one width; `step` is the
 *  width's place, which picks the question it hands on, the issue it addresses, and the person
 *  and the repository the issue header's lists and the Settings page's people lists pick. */
function pickLists(page: Page, seed: PickListSeed, step: number): PickList[] {
  const main = page.getByTestId("main-content");
  const palette = page.getByRole("dialog", { name: "Search" });
  const filters = page.getByRole("dialog", { name: "Filter" });
  /** The words a trigger shows. */
  const shown = (trigger: Locator) => async () =>
    (await trigger.innerText()).replace(/\s+/g, " ").trim();
  /** The search bar's chip for one narrowing, where the open Filter panel lists it at its top: the
   *  panel lists each narrowing that is on at every width, while the bar's line takes its chips
   *  out below 640 px and puts the ones that do not fit behind `+N more` (R8). The panel stands
   *  open across both of the bar's lists. */
  const chip = (narrowing: string) => async () =>
    (
      await control(filters, CONTROL.chip).evaluateAll((chips) =>
        chips.map((node) => node.getAttribute("aria-label") ?? "")
      )
    )
      .filter((label) => label.startsWith(`Remove ${narrowing} `))
      .join("; ");
  const project = listTrigger(filters, "Project");
  const status = listTrigger(filters, "Status");
  const moveTo = listTrigger(page, "Move to epic");
  const mergeInto = listTrigger(page, "Merge into another epic");
  const question = seed.asks[step];
  const handOn = listTrigger(page.getByTestId(`ask-${question}`), "Hand this question to");
  const agent = agentRow(page, plannerSession.session_id);
  const issue = listTrigger(agent, "Choose issue");
  const version = listTrigger(page, "Version");
  const compare = page.getByRole("region", { name: "Compare versions for notes.md" });
  const from = listTrigger(compare, "Compare notes.md from");
  const to = listTrigger(compare, "Compare notes.md to");
  const notesPage = `/issues/${seed.documented}/artifacts/notes-md`;
  // A version is typed as the words its option starts with, `Version <n>`, which a summary may
  // follow; the current text's option, `Current` or `Current version`, holds neither.
  const versionOne = /^Version 1(?: — .+)?$/;
  const versionTwo = /^Version 2(?: — .+)?$/;
  // The issue each width's pick addresses, so the second width's pick is a change whatever the
  // composer kept of the first.
  const [issueWord, issueOption, issueKey] =
    step === 0
      ? ["rollout", "CORE-2 · Rollout plan", "CORE-2"]
      : ["keyboard", "CORE-1 · Keyboard issue", "CORE-1"];
  const assignee = listTrigger(page, `Assignee of ${seed.header}`);
  const repository = listTrigger(page, `Repository of ${seed.header}`);
  const createIssueDialog = page.getByRole("dialog", { name: "Create issue" });
  const dialogProject = listTrigger(createIssueDialog, "Project");
  // The Settings page's lists. The New project form's Default assignee is the one whose label
  // does not go on with " of <KEY>".
  const opsDefault = listTrigger(page, "Default assignee of OPS");
  const newProjectDefault = listTrigger(page, /^Default assignee(?! of )/);
  const sourceProject = listTrigger(page, "Source project");
  // Each width's pick is a change: the first hands the issue from alice to bob, the second from
  // bob to carol, and OPS's default from carol to bob and back; the first gives the issue
  // acme/gadgets of its own, typed, since the list suggests the project's default alone and the
  // word leaves no option it holds, and the second takes it back to the default it inherits.
  const [personWord, person] = step === 0 ? ["bo", "bob"] : ["car", "carol"];
  const [repositoryWord, repositoryLeft, repositoryPicked] =
    step === 0 ? ["acme/gadgets", [], /^acme\/gadgets$/] : ["inh", ["Inherit"], /^acme\/widgets$/];
  // The Inbox's two Snoozes. With the reading column a row's Snooze is in the column's head, for
  // the row the column shows; without it, in the row itself.
  const snoozed = seed.snoozes[step] ?? { after: "", bulk: "", row: "" };
  const rowSnooze = listTrigger(
    hasReadingPane(page) ? inboxPane(page) : inboxRow(page, snoozed.row),
    /^Snooze /
  );
  const bulkSnooze = listTrigger(
    page.getByRole("toolbar", { name: "Selected rows" }),
    "Snooze selected asks"
  );
  /** Where a reader reads what a Snooze did: how many rows the Inbox's Later band holds, in the
   *  band's own words, `Later (0)` while there is no band. Which of the two the Inbox shows is
   *  read once its list has settled on one of them (`settledState`): the band, or the list drawn
   *  with no band in it. */
  const later = async () => {
    const band = page.getByRole("button", { name: /^Later \(\d+\)/ });
    const shown = await settledState("the Inbox's Later band", [
      ["band", band],
      ["no band", inboxList(page).filter({ hasNot: band })],
    ]);
    if (shown === "no band") return "Later (0)";
    return /Later \(\d+\)/.exec((await band.innerText()).replace(/\s+/g, " "))?.[0] ?? "";
  };
  // Each Snooze pick defers one more question, two at each width, so Later counts one more.
  const laterAfter = (picks: number) => new RegExp(`^Later \\(${2 * step + picks}\\)$`);
  const mode = listTrigger(page.getByRole("region", { name: "Broadcast" }), "Delivery mode");
  const [modeWord, modeOption] = step === 0 ? ["asi", "Aside"] : ["sen", "Send"];
  const rowsPerPage = listTrigger(control(main, CONTROL.pager), "Rows per page");
  const [sizeWord, sizeOption] = step === 0 ? ["250", "250 per page"] : ["100", "100 per page"];
  // The priority of the width's own issue, which the List's row, the Board's card and the
  // Inbox's question pick in turn: P3 to P1, P1 to P2, then P2 to P0. The Inbox draws a
  // question's priority in the reading column's head, for the row the column shows, and without
  // the column in the row itself.
  const prioritized = seed.priorities[step] ?? { ask: "", issue: "" };
  const priorityOf = (scope: Locator) => listTrigger(scope, `Priority of ${prioritized.issue}`);
  const listPriority = priorityOf(main.locator(`[data-issue-row="${prioritized.issue}"]`));
  const cardPriority = priorityOf(main.locator(`[data-board-card="${prioritized.issue}"]`));
  const inboxPriority = priorityOf(
    hasReadingPane(page) ? inboxPane(page) : inboxRow(page, prioritized.ask)
  );
  // The Delivery page over the fixture's custom range, so the Window starts on Custom range….
  const deliveryPage = `/delivery?${DELIVERY_WINDOW}&mode=list`;
  const deliveryWindow = listTrigger(main, "Window");
  const [windowWord, windowOption] = step === 0 ? ["84", "Last 84 days"] : ["14", "Last 14 days"];
  const colorBy = listTrigger(main, "Color merges by");
  const [colorWord, colorOption] = step === 0 ? ["prio", "Priority"] : ["comp", "Component"];
  return [
    {
      name: "the search bar's Project",
      reach: async () => {
        await page.goto("/");
        await expect(page.locator("[data-inbox-row]").first()).toBeVisible();
        await page.locator("body").focus();
        await page.keyboard.press("Control+k");
        await expect(palette).toBeVisible();
        await openFilterPanel(page, palette);
      },
      trigger: project,
      value: chip("Project"),
      word: "op",
      left: ["OPS"],
      picked: /^Remove Project OPS filter$/,
      // A pick takes the list's control out of the panel, which hands the focus to the control
      // now at its place, as the shared controls' focus row reads it.
      focusAfterPick: status,
    },
    {
      name: "the search bar's Status",
      trigger: status,
      value: chip("Status"),
      word: "prog",
      left: ["In progress"],
      picked: /^Remove Status In progress filter$/,
      // The control now at its place is the Author box, which takes typed text, so the hand-off
      // passes over the box to the control before it: the chip the pick put at the top of the
      // panel, which lists each narrowing that is on (R8).
      focusAfterPick: filters.getByRole("button", {
        exact: true,
        name: "Remove Status In progress filter",
      }),
    },
    {
      name: "the List's Move to epic",
      reach: async () => {
        await openIssuesTab(page, "CORE", "List");
        await main.getByRole("checkbox", { exact: true, name: `Select ${seed.member}` }).check();
      },
      trigger: moveTo,
      value: shown(moveTo),
      word: "mirr",
      left: ["Mirror epic"],
      picked: /^Mirror epic$/,
    },
    {
      name: "an epic's Merge into another epic",
      reach: async () => {
        await page.goto("/projects/CORE/issues?group=epic");
        await page
          .getByRole("row")
          .filter({ has: page.getByRole("button", { name: /Lease epic/ }) })
          .getByRole("button", { name: "Epic actions ▾" })
          .click();
      },
      trigger: mergeInto,
      value: shown(mergeInto),
      word: "ledg",
      left: ["Ledger epic"],
      picked: /^Ledger epic$/,
    },
    {
      name: "the List's Rows per page",
      reach: async () => {
        await openIssuesTab(page, "CORE", "List");
      },
      trigger: rowsPerPage,
      value: shown(rowsPerPage),
      word: sizeWord,
      left: [sizeOption],
      picked: new RegExp(`^${sizeOption}$`),
    },
    {
      name: "the List's Priority of a row",
      trigger: listPriority,
      value: shown(listPriority),
      word: "p1",
      left: ["P1"],
      picked: /^P1$/,
    },
    {
      name: "the Board's Priority of a card",
      reach: async () => {
        await openIssuesTab(page, "CORE", "Board");
        await setBoardLanes(page, "hidden");
      },
      trigger: cardPriority,
      value: shown(cardPriority),
      word: "p2",
      left: ["P2"],
      picked: /^P2$/,
    },
    {
      // Every person's questions, so the question stays listed once it waits on bob. Its list is
      // on its card: the reading column's, for the row selected, or the row's own where the
      // Inbox is too narrow for that column.
      name: "the Inbox's Hand this question to",
      reach: async () => {
        await page.goto("/?assignee=everyone");
        await selectRow(page, question);
      },
      trigger: handOn,
      value: shown(handOn),
      word: "bo",
      left: ["bob"],
      picked: /^Asked of bob$/,
    },
    {
      // The reader's own questions, newest first. With the reading column the row's question is
      // selected first, so the column's head draws its Snooze.
      name: "the Inbox's Snooze of a row",
      reach: async () => {
        await page.goto("/");
        await expect(inboxRow(page, snoozed.row)).toBeVisible();
        await selectRow(page, snoozed.row);
      },
      trigger: rowSnooze,
      value: later,
      word: "tom",
      left: ["Tomorrow"],
      picked: laterAfter(1),
      // The pick takes the row away once its write lands. With the reading column the focus goes
      // to the column's own stop; without it no hand-off of the Inbox's own lands the focus the
      // row took with it, so the row reads none.
      focusAfterPick: hasReadingPane(page) ? paneScroller(page) : null,
    },
    {
      name: "the Inbox's Snooze selected asks",
      reach: async () => {
        await inboxRow(page, snoozed.bulk).getByRole("checkbox").first().check();
      },
      trigger: bulkSnooze,
      value: later,
      word: "week",
      left: ["Next week"],
      picked: laterAfter(2),
      // The bar goes with the row it deferred, and its focus goes to the mark of the row that
      // moves up into that row's place.
      focusAfterPick: inboxRow(page, snoozed.after).getByRole("checkbox").first(),
    },
    {
      // The reader's own questions; with the reading column the question is selected first, so
      // the column's head draws its priority.
      name: "the Inbox's Priority of a question",
      reach: async () => {
        await page.goto("/");
        await expect(inboxRow(page, prioritized.ask)).toBeVisible();
        await selectRow(page, prioritized.ask);
      },
      trigger: inboxPriority,
      value: shown(inboxPriority),
      word: "p0",
      left: ["P0"],
      picked: /^P0$/,
    },
    {
      name: "the Agents composer's Issue",
      reach: async () => {
        await page.goto("/agents");
        const toggle = agent.locator("[data-agent-toggle]");
        await expect(toggle).toBeVisible();
        if ((await expandedState("the agent row", toggle)) === "shut") await toggle.click();
      },
      trigger: issue,
      value: shown(issue),
      word: issueWord,
      left: [issueOption],
      picked: new RegExp(`^Issue: ${issueKey}$`),
      // The architect's ruling: a pick hands the reader to the message it addresses, and Escape
      // is one level out, to the row.
      focusAfterPick: agent.getByRole("textbox", { name: "Comment" }),
      focusAfterEscape: agent,
    },
    {
      // The broadcast composer opens with a session ticked for it.
      name: "the Agents page's Delivery mode",
      reach: async () => {
        await page.goto("/agents");
        await main
          .getByRole("checkbox", { name: /^Select .* for broadcast$/ })
          .first()
          .check();
      },
      trigger: mode,
      value: shown(mode),
      word: modeWord,
      left: [modeOption],
      picked: new RegExp(`^${modeOption}$`),
    },
    {
      name: "the Spec tab's Version",
      reach: async () => {
        await page.goto(`/issues/${seed.documented}/spec`);
      },
      trigger: version,
      value: shown(version),
      word: "version 1",
      left: [versionOne],
      picked: versionOne,
    },
    {
      // From starts on the older version and To on the newer, so each word picks the other.
      name: "a document page's compare From",
      reach: async () => {
        await page.goto(notesPage);
        // The editor draws the document's text after the page has drawn the rest, and that text
        // is above the compare: in Firefox at 1440 a press made while it was still to come
        // opened nothing, and the trace's next snapshot held the text the press came before.
        await expect(documentEditor(page)).toContainText("seven leases");
        await expect(connectedDot(page)).toHaveText("connected");
        await compare.getByRole("button", { name: "Show comparison" }).click();
        await expect(compare.getByRole("button", { name: "Hide comparison" })).toBeVisible();
      },
      trigger: from,
      value: shown(from),
      word: "version 2",
      left: [versionTwo],
      picked: versionTwo,
    },
    {
      name: "a document page's compare To",
      trigger: to,
      value: shown(to),
      word: "version 1",
      left: [versionOne],
      picked: versionOne,
    },
    {
      // Last on the page, since its pick leaves the page on a past version.
      name: "a document page's Version",
      trigger: version,
      value: shown(version),
      word: "version 1",
      left: [versionOne],
      picked: versionOne,
    },
    {
      name: "the issue header's Assignee",
      reach: async () => {
        await page.goto(`/issues/${seed.header}`);
      },
      trigger: assignee,
      value: shown(assignee),
      word: personWord,
      left: [person],
      picked: new RegExp(`^${person}$`),
    },
    {
      // Its value is the badge: the trigger's words of its own, which say whether the issue
      // inherits it, are what its name goes on to after its label and are never shown.
      name: "the issue header's Repository",
      trigger: repository,
      // The badge, the one hidden element of the trigger that is not its chevron's `svg`.
      value: async () =>
        (await repository.locator('[aria-hidden="true"]:not(svg)').innerText()).trim(),
      word: repositoryWord,
      left: repositoryLeft,
      picked: repositoryPicked,
    },
    {
      // Opened with `c` over the issue page, whose project it starts on, and read once its title
      // has the focus the dialog gives it on opening. The focus is taken off the list read before
      // this one first: a character typed on a list's closed trigger is the list's search, so `c`
      // pressed on it opens that list rather than the dialog (`MultiSelect`).
      name: "the Create issue dialog's Project",
      reach: async () => {
        await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
        await page.keyboard.press("c");
        await expect(createIssueDialog.getByRole("textbox", { name: "Title" })).toBeFocused();
      },
      trigger: dialogProject,
      value: shown(dialogProject),
      word: "op",
      left: ["OPS · Ops"],
      picked: /^OPS · Ops$/,
    },
    {
      // A project's own list, read once the defaults are: until then it is disabled and shows
      // that they are loading.
      name: "the Settings page's Default assignee of OPS",
      reach: async () => {
        await page.goto("/settings");
        await expect(opsDefault).toBeEnabled();
      },
      trigger: opsDefault,
      value: shown(opsDefault),
      word: personWord,
      left: [person],
      picked: new RegExp(`^${person}$`),
    },
    {
      // The New project form's, which starts on the viewer, alice, at each load of the page.
      name: "the Settings page's New project Default assignee",
      trigger: newProjectDefault,
      value: shown(newProjectDefault),
      word: personWord,
      left: [person],
      picked: new RegExp(`^${person}$`),
    },
    {
      name: "the Settings page's Source project",
      trigger: sourceProject,
      value: shown(sourceProject),
      word: "op",
      left: ["OPS · Ops"],
      picked: /^OPS · Ops$/,
    },
    {
      name: "the Delivery page's Window",
      reach: async () => {
        await page.goto(deliveryPage);
      },
      trigger: deliveryWindow,
      value: shown(deliveryWindow),
      word: windowWord,
      left: [windowOption],
      picked: new RegExp(`^${windowOption}$`),
    },
    {
      // Read over the fixture's range again, which draws its merges and the list beside them.
      name: "the Delivery page's Color merges by",
      reach: async () => {
        await page.goto(deliveryPage);
      },
      trigger: colorBy,
      value: shown(colorBy),
      word: colorWord,
      left: [colorOption],
      picked: new RegExp(`^${colorOption}$`),
    },
  ];
}

// SLOW(>=2s): twenty-six lists on nine pages, each list opened three times at each of two widths.
// R3: "Every list of options a reader picks from holds a text box that narrows the options."
// The row covers the twenty-five single picks of the shared list (`components/MultiSelect.tsx`),
// the lists whose options grow with the project among them: the search bar's Project and Status,
// the List's Move to epic and Rows per page (`Pager`), an epic's Merge into another epic, the
// priority of a List row, a Board card and an Inbox question (`PriorityControl`), the Inbox's
// Hand this question to (`AskHandOn`), a row's Snooze (`SnoozeControl`) and the selection bar's
// Snooze selected asks (`Inbox`), the Agents composer's Issue, the Agents page's Delivery mode
// (`BroadcastComposer`), the Spec tab's Version (`SpecToolbar`), a document page's Version
// (`ArtifactHeader`) and its compare From and To (`ArtifactDetails`), the issue header's Assignee
// (`AssigneeControl`) and Repository (`RepoControl`),
// the Create issue dialog's Project (`CreateIssueDialog`), the Settings page's Default assignee
// of a project and the New project form's Default assignee (`ProjectsSection`), and its Source
// project (`ProjectSelect`), and the Delivery page's Window (`WindowControl`) and
// Color merges by (`DeliveryPage`), and, in multi-pick mode, the issue header's Edit priorities
// (`IssuePriorityLine`). The issue header's Status is the shared single pick too
// (20f534c5); this row does not open it, and its own unit cases hold its text box
// (`features/issue/IssuePage.test.tsx`, `features/issue/issue-writes.test.tsx`). At each width
// each list is
// opened from its trigger: exactly one text box is in the list's own panel - at 390 an `input` of
// 16 px, the size `styles.css` gives every input below 1280 px and under which iOS Safari zooms
// the page onto the box - the option a typed word leaves is not the value the list starts on, the
// word leaves only the options it matches, Escape picks nothing and changes no value, Tab out of
// the text box (Shift+Tab at the second width) picks nothing and leaves the reader on the
// trigger, `?` pressed then on the closed trigger opens the list with `?` in its text box and no
// Keyboard shortcuts, since a character typed on a closed trigger is the list's search
// (`MultiSelect`), and Enter picks the option the word left - the trigger shows it with the focus
// back on it, or where the pick takes the trigger away the panel hands the focus on. A Snooze's
// pick shows in the Inbox's Later band, which holds one more row once the write lands. The
// selection bar's Snooze hands the focus to the mark of the row now at the deferred row's place,
// and a row's Snooze, with the reading column drawn, to the column's own stop; at 390, where it is
// not, no hand-off of the Inbox's own lands the focus a row's Snooze took, and the row reads none.
// A list that holds no text box is named and the row goes on, so one run names every such list.
// It runs in Chromium alone. What an engine decides here is one component's, the same in every
// list, and two rows read it in Firefox and WebKit at a fraction of this row's cost: the keys the
// list's text box takes, as each engine delivers them, in `keyboard-agents-picker`'s rows of the
// composer's Issue list, and where a Tab or Shift+Tab out of the text box leaves the focus - on
// the trigger - on a page and in a dialog, in the row that Tab and Escape never leave a panel for
// the page behind it. This row in Firefox read no more than that one: with the list's own hand-off
// to its trigger taken out, it passed there too (2026-11-18). In both engines it took 2.1 min of
// its shard (full-lane run 44206548149), which put that shard over its 9-minute step.
test("every list of options a reader picks from holds a text box that narrows it, at 1440 and 390", async ({
  page,
}) => {
  // Sixteen page loads and a dialog at each width, with the lists read between them. Measured in
  // an issue pod (two cores), with each of the eighteen lists it then read opened three times: the
  // row took 2.3 min in Chromium (2026-11-17), and 41.9 s in CI's Chromium (full-lane run
  // 44206548149). Six minutes kept the pod's time under two thirds of the budget then.
  test.setTimeout(360_000);
  const seed = await seedPickLists();
  /** The lists the row could not read: no trigger, a trigger that names no list of its own, or
   *  a list holding other than one text box. */
  const missing: string[] = [];
  /** The closed triggers a page's single key acted through. */
  const keyed: string[] = [];
  const shortcuts = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  /** Opens `list` from its trigger and answers the options of its own panel, once the focus is
   *  in that panel's one text box; or records why not, and shuts what the press opened. */
  const open = async (
    list: Pick<PickList, "trigger">,
    at: string
  ): Promise<Locator | undefined> => {
    await list.trigger.click();
    const named = await expect(list.trigger)
      .toHaveAttribute("aria-controls", /\S/)
      .then(
        () => true,
        () => false
      );
    if (named) {
      const panel = listOf(list.trigger);
      await expect(panel, `${at}: the list its trigger names is not drawn`).toBeVisible();
      const box = panel.locator(TEXT_BOXES);
      // The panel was waited for above, and the shared list draws its text box in the panel's
      // own render, so how many it draws is the count this row records.
      const boxes = await box.count();
      if (boxes === 1) {
        await expect(box, `${at}: the open list left the focus outside its text box`).toBeFocused();
        if ((page.viewportSize()?.width ?? Number.POSITIVE_INFINITY) < 1280) {
          expect(
            await box.evaluate((node) => `${node.tagName} ${getComputedStyle(node).fontSize}`),
            `${at}: the list's text box is not an input of 16 px`
          ).toBe("INPUT 16px");
        }
        // A row that takes the typed text as it is (the Repository list's `Use "…"`, a single
        // pick's `onCreate`, drawn with a leading `+`) is no option the list holds.
        return panel.getByRole("option").filter({ hasNotText: /^\+/ });
      }
      missing.push(`${at}: the list its trigger opens holds ${boxes} text boxes, not one`);
    } else {
      missing.push(`${at}: its trigger names no list of its own once pressed`);
    }
    await page.keyboard.press("Escape");
    return undefined;
  };

  for (const [step, size] of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ].entries()) {
    await page.setViewportSize(size);
    for (const list of pickLists(page, seed, step)) {
      const at = `${list.name} at ${size.width}x${size.height}`;
      await list.reach?.();
      const drawn = await expect(list.trigger)
        .toBeVisible()
        .then(
          () => true,
          () => false
        );
      if (!drawn) {
        missing.push(`${at}: no control opens the list`);
        continue;
      }
      // A trigger says its list is loading until the read of its options lands: read before then,
      // the value the row compares each later read with would be one no press of the reader's gave.
      await expect
        .poll(() => list.value(), { message: `${at}: the list's options never loaded` })
        .not.toMatch(/^Loading/);
      const before = await list.value();
      expect(
        before,
        `${at}: the list starts on the option "${list.word}" leaves, so no pick could change it`
      ).not.toMatch(list.picked);

      // The word makes an option other than the value the active one, and Escape picks nothing.
      const escaped = await open(list, at);
      if (escaped === undefined) continue;
      await expect
        .poll(() => escaped.count(), {
          message: `${at}: the list holds no option "${list.word}" does not match`,
        })
        .toBeGreaterThan(list.left.length);
      await page.keyboard.type(list.word);
      await expect(escaped, `${at}: "${list.word}" left other options than its own`).toHaveText(
        list.left
      );
      await page.keyboard.press("Escape");
      await expect(list.trigger, `${at}: Escape left the list open`).toHaveAttribute(
        "aria-expanded",
        "false"
      );
      await expect(
        list.focusAfterEscape ?? list.trigger,
        `${at}: Escape left the focus elsewhere`
      ).toBeFocused();
      expect(await list.value(), `${at}: Escape changed the list's value`).toBe(before);

      // Tab out of the text box, or Shift+Tab, picks nothing and leaves the reader on the trigger.
      const tab = step === 0 ? "Tab" : "Shift+Tab";
      const tabbed = await open(list, at);
      if (tabbed === undefined) continue;
      await page.keyboard.press(tab);
      await expect(list.trigger, `${at}: ${tab} left the list open`).toHaveAttribute(
        "aria-expanded",
        "false"
      );
      await expect(list.trigger, `${at}: ${tab} left the focus elsewhere`).toBeFocused();
      expect(await list.value(), `${at}: ${tab} changed the list's value`).toBe(before);

      // A single key of the page, pressed on the closed trigger, is the list's: it opens the list
      // with that character in its text box, and the shell's `?` opens no Keyboard shortcuts.
      await page.keyboard.press("?");
      await nextFrames(page);
      // One read after the press's frames is the answer: the shell draws Keyboard shortcuts in
      // the render the key's binding starts, which nextFrames has let run.
      if ((await shortcuts.count()) > 0) {
        keyed.push(`${at}: ? on the closed trigger opened the Keyboard shortcuts`);
        await page.keyboard.press("Escape");
        await expect(shortcuts, `${at}: Escape left the Keyboard shortcuts open`).toHaveCount(0);
        await list.trigger.focus();
      } else {
        const typed = listOf(list.trigger).locator(TEXT_BOXES);
        await expect(
          typed,
          `${at}: ? on the closed trigger is not in its list's text box`
        ).toHaveValue("?");
        await expect(
          typed,
          `${at}: ? on the closed trigger left the focus off its list`
        ).toBeFocused();
        await page.keyboard.press("Escape");
        await expect(list.trigger, `${at}: Escape left the list ? opened open`).toHaveAttribute(
          "aria-expanded",
          "false"
        );
        expect(await list.value(), `${at}: ? and Escape changed the list's value`).toBe(before);
      }

      // Enter picks the option the word left.
      const entered = await open(list, at);
      if (entered === undefined) continue;
      await page.keyboard.type(list.word);
      await expect(entered, `${at}: "${list.word}" left other options than its own`).toHaveText(
        list.left
      );
      await page.keyboard.press("Enter");
      await expect
        .poll(list.value, { message: `${at}: Enter did not pick the option "${list.word}" left` })
        .toMatch(list.picked);
      if (list.focusAfterPick !== null) {
        await expect(
          list.focusAfterPick ?? list.trigger,
          `${at}: the pick left the focus elsewhere`
        ).toBeFocused();
      }
    }
    // The issue header's Edit priorities (main's #4316) is the shared list in multi-pick mode:
    // its trigger stays `+`, Enter toggles the narrowed priority in the draft, and closing saves.
    const at = `the issue header's Edit priorities at ${size.width}x${size.height}`;
    const [priorityWord, priorityOption] =
      step === 0
        ? ["queue", /^#\d+ Cut the review queue$/]
        : ["ledger", /^#\d+ Retire the old ledger$/];
    await page.goto(`/issues/${seed.header}`);
    const header = page.getByTestId("issue-header");
    const editPriorities = header.getByRole("button", { exact: true, name: "Edit priorities" });
    const chipLink = header.getByRole("link", { name: priorityOption });
    const drawn = await expect(editPriorities)
      .toBeVisible()
      .then(
        () => true,
        () => false
      );
    if (!drawn) {
      missing.push(`${at}: no control opens the list`);
    } else {
      await expect(chipLink, `${at}: the priority is already on the issue`).toHaveCount(0);
      const escaped = await open({ trigger: editPriorities }, at);
      if (escaped !== undefined) {
        await expect
          .poll(() => escaped.count(), {
            message: `${at}: the list holds no option "${priorityWord}" does not match`,
          })
          .toBeGreaterThan(1);
        await page.keyboard.type(priorityWord);
        await expect(
          escaped,
          `${at}: "${priorityWord}" left other options than its own`
        ).toHaveText([priorityOption]);
        await page.keyboard.press("Escape");
        await expect(editPriorities, `${at}: Escape left the list open`).toHaveAttribute(
          "aria-expanded",
          "false"
        );
        await expect(editPriorities, `${at}: Escape left the focus elsewhere`).toBeFocused();
        await expect(chipLink, `${at}: Escape added a priority`).toHaveCount(0);

        const tab = step === 0 ? "Tab" : "Shift+Tab";
        const tabbed = await open({ trigger: editPriorities }, at);
        if (tabbed !== undefined) {
          await page.keyboard.press(tab);
          await expect(editPriorities, `${at}: ${tab} left the list open`).toHaveAttribute(
            "aria-expanded",
            "false"
          );
          await expect(editPriorities, `${at}: ${tab} left the focus elsewhere`).toBeFocused();
          await expect(chipLink, `${at}: ${tab} added a priority`).toHaveCount(0);
        }

        await page.keyboard.press("?");
        const typed = listOf(editPriorities).locator(TEXT_BOXES);
        const opened = await settledState(`${at} after ?`, [
          ["shortcuts", shortcuts],
          ["list", typed],
        ]);
        if (opened === "shortcuts") {
          keyed.push(`${at}: ? on the closed trigger opened the Keyboard shortcuts`);
          await page.keyboard.press("Escape");
          await expect(shortcuts, `${at}: Escape left the Keyboard shortcuts open`).toHaveCount(0);
          await editPriorities.focus();
        } else {
          await expect(
            typed,
            `${at}: ? on the closed trigger is not in its list's text box`
          ).toHaveValue("?");
          await expect(
            typed,
            `${at}: ? on the closed trigger left the focus off its list`
          ).toBeFocused();
          await page.keyboard.press("Escape");
          await expect(editPriorities, `${at}: Escape left the list ? opened open`).toHaveAttribute(
            "aria-expanded",
            "false"
          );
          await expect(chipLink, `${at}: ? and Escape added a priority`).toHaveCount(0);
        }

        const entered = await open({ trigger: editPriorities }, at);
        if (entered !== undefined) {
          await page.keyboard.type(priorityWord);
          await expect(
            entered,
            `${at}: "${priorityWord}" left other options than its own`
          ).toHaveText([priorityOption]);
          await page.keyboard.press("Enter");
          await expect(
            entered.first(),
            `${at}: Enter did not tick the option "${priorityWord}" left`
          ).toHaveAttribute("aria-selected", "true");
          await page.keyboard.press("Escape");
          await expect(
            editPriorities,
            `${at}: Escape left the list open after pick`
          ).toHaveAttribute("aria-expanded", "false");
          await expect(editPriorities, `${at}: closing left the focus elsewhere`).toBeFocused();
          await expect(chipLink, `${at}: closing did not save the ticked priority`).toBeVisible();
        }
      }
    }
  }
  expect(missing, missing.join("; ")).toEqual([]);
  expect(keyed, keyed.join("; ")).toEqual([]);
});

/** A title wider than any list's trigger is at 1024x768, given to an epic and a named version,
 *  each of which a list then picks. */
const LONG_TITLE =
  "Retire the ledger mirror and hand every lease it still holds to the regional daemons";

/** What is wrong with how a list's trigger draws a value longer than it: the element the value's
 *  text is in has to be the box that cuts it (its `scrollWidth` past its `clientWidth`) and end
 *  it in an ellipsis, and the trigger's chevron has to be drawn whole, 16 px wide, at the end of
 *  the trigger's content box. A value that fits is named, since then the read shows nothing. The
 *  chevron turns back over `transition-transform` as the list closes, and a box read while it
 *  turns is wider than the chevron (CI read 17.5 to 21 px), so the read waits for the trigger's
 *  own animations to end first. */
function longValueDrawing(trigger: Locator): Promise<string[]> {
  return trigger.evaluate(async (node) => {
    await Promise.all(node.getAnimations({ subtree: true }).map((animation) => animation.finished));
    const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT);
    let text = walker.nextNode();
    while (text !== null && (text.textContent ?? "").trim() === "") text = walker.nextNode();
    const holder = text?.parentElement;
    if (text === null || holder === null || holder === undefined) return ["it holds no text"];
    const style = getComputedStyle(node);
    const box = node.getBoundingClientRect();
    const contentRight =
      box.right - Number.parseFloat(style.borderRightWidth) - Number.parseFloat(style.paddingRight);
    const room =
      node.clientWidth -
      Number.parseFloat(style.paddingLeft) -
      Number.parseFloat(style.paddingRight);
    const range = document.createRange();
    range.selectNodeContents(text);
    const width = range.getBoundingClientRect().width;
    if (width <= room) {
      return [`its value is ${Math.round(width)} px wide, which fits its ${Math.round(room)} px`];
    }
    const wrong: string[] = [];
    const element = `the value's own element, a ${holder.tagName.toLowerCase()}`;
    if (!(holder.scrollWidth > holder.clientWidth)) {
      wrong.push(
        `${element}, is as wide as the value, so a box around it cuts it with no ellipsis`
      );
    }
    const ending = getComputedStyle(holder).textOverflow;
    if (ending !== "ellipsis") wrong.push(`${element}, ends it in "${ending}", not an ellipsis`);
    const chevron = node.querySelector("svg");
    if (chevron === null) {
      wrong.push("it draws no chevron");
    } else {
      const own = chevron.getBoundingClientRect();
      if (Math.abs(own.width - 16) > 0.5 || Math.abs(own.right - contentRight) > 1) {
        wrong.push(
          `its chevron is ${own.width} px wide at ${Math.round(own.left)}-${Math.round(own.right)} px, not whole at the trigger's end, ${Math.round(contentRight)} px`
        );
      }
    }
    return wrong;
  });
}

// SLOW(>=2s): three pages and a document version, four lists each picked from at 1024x768.
// INFRA-3135, round 12: below 1280 px `styles.css` makes every button a flex box, whose own text
// ends in no ellipsis (`TruncatedText`), so a list's trigger cut a long picked value mid-word; no
// trigger showed that it opens a list, as a `<select>`'s arrow did. At 1024x768, between `md`
// and `xl`, three lists of the shared list (`components/MultiSelect.tsx`) each pick a value
// longer than their trigger - the List's Move to epic, an epic's Merge into another epic, and
// the Spec tab's Version: the element holding the value cuts it and ends it in an ellipsis, and
// the chevron is whole at the trigger's end. (Priority to advance became the fixed `+` multi-pick
// Edit priorities trigger in main's #4316, which displays no picked title and has no chevron.)
test("a picked value too long for its list's trigger ends in an ellipsis before a whole chevron, at 1024x768", async ({
  page,
}) => {
  await page.setViewportSize({ height: 768, width: 1024 });
  await createProject({ key: "CORE", name: "Core" });
  const longEpic = await createIssue({ project: "CORE", title: LONG_TITLE });
  const shortEpic = await createIssue({ project: "CORE", title: "Short epic" });
  for (const epic of [longEpic, shortEpic]) await patchIssue(epic.key, { labels: ["epic"] });
  const member = await createIssue({
    project: "CORE",
    parent: shortEpic.key,
    title: "Handover step",
  });
  const documented = await createIssue({
    project: "CORE",
    spec: "The lease passes at noon.\n",
    title: "Lease handover",
  });
  await createNamedVersion(documented.primary_artifact_id, LONG_TITLE);
  const wrong: string[] = [];
  /** Opens `trigger`'s list, types `word`, and takes the one option the word leaves with Enter. */
  const pick = async (trigger: Locator, word: string) => {
    await trigger.click();
    await expect(listOf(trigger).getByRole("combobox")).toBeFocused();
    await page.keyboard.type(word);
    await expect(listOf(trigger).getByRole("option")).toHaveCount(1);
    await page.keyboard.press("Enter");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
  };
  /** Records what is wrong with how `trigger` draws the long value just picked. */
  const read = async (name: string, trigger: Locator) => {
    await expect(trigger, `${name} does not show the value picked`).toContainText(LONG_TITLE);
    // The value was waited for above, and `longValueDrawing` waits out the trigger's animations
    // before it measures, so the drawing read is the settled one.
    for (const what of await longValueDrawing(trigger)) wrong.push(`${name}: ${what}`);
  };

  await openIssuesTab(page, "CORE", "List");
  await page
    .getByTestId("main-content")
    .getByRole("checkbox", { exact: true, name: `Select ${member.key}` })
    .check();
  const moveTo = listTrigger(page, "Move to epic");
  await pick(moveTo, "regional");
  await read("the List's Move to epic", moveTo);

  await page.goto("/projects/CORE/issues?group=epic");
  await page
    .getByRole("row")
    .filter({ has: page.getByRole("button", { name: /Short epic/ }) })
    .getByRole("button", { name: "Epic actions ▾" })
    .click();
  const mergeInto = listTrigger(page, "Merge into another epic");
  await pick(mergeInto, "regional");
  await read("an epic's Merge into another epic", mergeInto);

  await page.goto(`/issues/${documented.key}/spec`);
  const version = listTrigger(page, "Version");
  await pick(version, "regional");
  await read("the Spec tab's Version", version);
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): one issue page at two widths, Edit priorities opened and dismissed four times.
// INFRA-3135, round 13 (the tester's, rewritten in round 28 after main's #4316 replaced the
// inline Add a priority form with the Edit priorities multi-pick trigger): pressing Edit
// priorities opens its list wholly in view with the focus in its Priority to advance text box,
// and Escape closes the list and gives the focus back to the trigger. Escape unmounts the
// portalled list while its text box holds the focus, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus row
// (e2e/playwright.config.ts). By a press of Enter and by a click, at 1440 and 390: the panel
// stands whole in the window, its text box holds the focus, and Escape returns it to the trigger.
test("Edit priorities opens its list in view with the focus in its text box, and Escape gives the focus back, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Harbour crane" });
  await createPriority({ owner: "alice", title: "Ship the daemon" });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const press of ["Enter", "click"] as const) {
      const at = `${size.width}x${size.height}, by ${press}`;
      await page.goto(`/issues/${issue.key}`);
      const trigger = page
        .getByTestId("issue-header")
        .getByRole("button", { exact: true, name: "Edit priorities" });
      await expect(trigger).toBeVisible();
      if (press === "Enter") {
        await trigger.focus();
        await page.keyboard.press("Enter");
      } else {
        await trigger.click();
      }
      const panel = listOf(trigger);
      await expect(panel, `${at}: Edit priorities opened no list`).toBeVisible();
      const search = panel.getByRole("combobox", { name: "Priority to advance" });
      const focused = await expect(search)
        .toBeFocused()
        .then(
          () => true,
          () => false
        );
      if (!focused) {
        const holder = await page.evaluate(
          () =>
            `${document.activeElement?.tagName.toLowerCase()} "${document.activeElement?.getAttribute("aria-label") ?? document.activeElement?.textContent?.trim().slice(0, 40)}"`
        );
        wrong.push(`${at}: the focus is on ${holder}, not on Priority to advance`);
      }
      const placed = await expect
        .poll(() =>
          panel.evaluate((node) => {
            const box = node.getBoundingClientRect();
            const inWindow =
              box.left >= 0 &&
              box.right <= window.innerWidth &&
              box.top >= 0 &&
              box.bottom <= window.innerHeight;
            return inWindow
              ? "whole"
              : `at ${Math.round(box.left)}-${Math.round(box.right)} x ${Math.round(box.top)}-${Math.round(box.bottom)} px in ${window.innerWidth}x${window.innerHeight}`;
          })
        )
        .toBe("whole")
        .then(
          () => "whole",
          async () =>
            panel.evaluate((node) => {
              const box = node.getBoundingClientRect();
              return `at ${Math.round(box.left)}-${Math.round(box.right)} x ${Math.round(box.top)}-${Math.round(box.bottom)} px in ${window.innerWidth}x${window.innerHeight}`;
            })
        );
      if (placed !== "whole") {
        wrong.push(`${at}: the Priorities list stands ${placed}, out of the window`);
      }
      await page.keyboard.press("Escape");
      await expect(trigger, `${at}: Escape left the list open`).toHaveAttribute(
        "aria-expanded",
        "false"
      );
      const back = await expect(trigger)
        .toBeFocused()
        .then(
          () => true,
          () => false
        );
      if (!back) {
        const holder = await page.evaluate(
          () =>
            `${document.activeElement?.tagName.toLowerCase()} "${document.activeElement?.getAttribute("aria-label") ?? document.activeElement?.textContent?.trim().slice(0, 40)}"`
        );
        wrong.push(`${at}: Escape left the focus on ${holder}, not on Edit priorities`);
      }
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the Session groups dialog opened at two widths, a group made and renamed in each.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p1-h and -p1-b at
// da0804df and a1c78223): the reader's focus is on a session group's Rename in the Session groups
// dialog, and Enter renames the group. The card holds its buttons while the write is out and
// until the live update carrying the rename has drawn the group again in place
// (`useHoldUntilDrawn`), and holds them by `aria-disabled` (`heldPress`), so Rename keeps the focus
// the reader's press left on it once the write has landed, at 1440 and 390 px
// (`features/delivery/SessionGroupsEditor.tsx`). A control held under the reader's press, which a
// native `disabled` takes the focus from: the clause of the `webkit` and `firefox` projects that
// names the page-own focus row (e2e/playwright.config.ts).
test("a group card's Rename by Enter keeps the keyboard focus once its write lands, in the Session groups dialog @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(90_000);
  // Once: the fixture's rows carry no ON CONFLICT. Each width groups a session of its own, since
  // a session one group holds is free for no other.
  await seedSessionGroupsFixture();
  for (const [height, width, session] of [
    [900, 1440, "Planner, first run"],
    [664, 390, "Planner, second run"],
  ] as const) {
    const name = `Crew at ${width}`;
    await page.setViewportSize({ height, width });
    await page.goto("/delivery?mode=list");
    await expect(page.getByText("3 PRs in current filter/window")).toBeVisible();
    await openFilterPanel(page);
    await page.getByRole("button", { name: "Edit groups" }).click();
    const editor = page.getByRole("dialog", { name: "Session groups" });
    await editor.getByRole("checkbox", { exact: true, name: session }).check();
    await editor.getByLabel("New group name").fill(name);
    await editor.getByRole("button", { name: "Create group" }).click();
    const card = editor.getByRole("group", { name });
    // Every button of a card is disabled while a write of the editor is out, and Delete group
    // by nothing else, so its coming back is the create's end.
    await expect(card.getByRole("button", { name: "Delete group" })).toBeEnabled();
    const renamed = `${name}, renamed`;
    await card.getByLabel("Name", { exact: true }).fill(renamed);
    await card.getByRole("button", { exact: true, name: "Rename" }).focus();
    await page.keyboard.press("Enter");
    const drawn = editor.getByRole("group", { name: renamed });
    // The card's hold ends once the live update carrying the rename has drawn it.
    await expect(
      drawn.getByRole("button", { name: "Delete group" }),
      `${width} px: the rename was never drawn`
    ).toBeEnabled();
    await expect(
      drawn.getByRole("button", { exact: true, name: "Rename" }),
      `${width} px: Rename lost the keyboard focus once its write landed`
    ).toBeFocused();
  }
});

// SLOW(>=2s): a save whose re-read is held offline until the 30 s request deadline ends its hold.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p1-h and -p1-b at
// da0804df and a1c78223): at 1440 px the reader presses Enter on Save delivery settings, the
// save lands, and the browser is offline before the page reads its answer, so the re-read that
// draws the stored record is one TanStack pauses. Save is disabled from the press, so the focus
// falls to the body. At da0804df the hold ends at the request deadline, 30 s after the save's
// answer, but the app-wide hand-off those commits had hands the focus on at 30.03 s, before Save
// is enabled again, to the Preview section's New issue page checkbox, so Save gets nothing back and
// the reader's next Space toggles that setting. At a1c78223 that hand-off moves the focus to the
// same checkbox at 30.07 s, with Save still held after 45 s. The architect's rulings of
// 2026-11-20 20:3xZ (open-items.md, "Review round 3"): the form keeps its identity through a
// save, so Save keeps the reader's focus, and every hold ends at a timer of its own while the
// browser is offline too. No engine tag: where the focus rests is decided by the hold's timer, in
// the page's own code, so another engine would repeat what Chromium shows.
test("Save delivery settings keeps the keyboard focus through a save whose re-read the browser cannot send", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await seedDeliveryFixture();
  // The save proves the GitHub App can read the deploy repository; the fake answers it.
  await seedFakeGithub({ "acme/widgets": { contents: "read", installation_id: 301 } });
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto("/settings");
  const save = page.getByRole("button", { name: "Save delivery settings" });
  await expect(save).toBeEnabled();
  await page.getByLabel("Deploy workflow").fill(".github/workflows/deploy-two.yml");
  const context = page.context();
  // The save reaches Dispatch and lands. The browser is offline before the page has its answer,
  // so the re-read the answer starts is one the browser cannot send.
  await page.route("**/api/v1/settings/delivery", async (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    const response = await route.fetch();
    await context.setOffline(true);
    await page.waitForFunction(() => !navigator.onLine);
    await route.fulfill({ response });
  });
  try {
    await save.focus();
    // A log, not one instant's read: every element the focus enters after the press.
    const entered = await save.evaluateHandle((button) => {
      const log: string[] = [];
      document.addEventListener(
        "focusin",
        (event) => {
          const target = event.target;
          if (target === button) {
            log.push("Save");
            return;
          }
          if (!(target instanceof HTMLElement)) return;
          const label = target instanceof HTMLInputElement ? target.labels?.[0]?.textContent : null;
          const named = target.getAttribute("aria-label") || label || target.textContent || "";
          log.push(`${target.tagName.toLowerCase()} "${named.trim().slice(0, 40)}"`);
        },
        true
      );
      return log;
    });
    await page.keyboard.press("Enter");
    // The hold's own contract is the request deadline, 30 s from the save's answer
    // (REQUEST_DEADLINE_MS, web/src/api/client.ts); 45 s holds it with room to spare.
    await expect(save).toBeEnabled({ timeout: 45_000 });
    // Every hand-off runs within a frame and a task of the commit that drew Save again, so it is
    // done by now.
    await nextFrames(page);
    await nextFrames(page);
    const wrong: string[] = [];
    // One read is the answer: Save is enabled again, and nothing else moves the focus now.
    const onSave = await save.evaluate((button) => button === document.activeElement);
    // Read after the same hand-offs as the read above, so where the focus is is settled.
    const holder = await focusHolder(page);
    if (!onSave) wrong.push(`1440 px, offline re-read: the focus is on ${holder.name}, not Save`);
    const others = (await entered.jsonValue()).filter((name) => name !== "Save");
    if (others.length > 0)
      wrong.push(
        `1440 px, offline re-read: after the press the focus entered ${others.join(", ")}`
      );
    expect(wrong, wrong.join("; ")).toEqual([]);
  } finally {
    await context.setOffline(false);
  }
});

// SLOW(>=2s): the Delivery page opened eight times, at two widths, by two presses, with its Window
// control scrolled wholly out of sight and left wholly in sight.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p3g-h and -p3g-b at
// da0804df and a1c78223, and this row in -p3-red-h at da0804df): with a brush window set - the
// address's `ws` and `we`, as a drag along the chart's time axis sets them - the Delivery page's
// header draws "clear brush window" in its generated line. Its press takes the button away with
// the brush, and the header's hand-off (`useFocusPlace` in DeliveryPage.tsx), finding no other
// control in that line, focuses the Window control on the line above with the browser's own
// scroll. With the page scrolled so that the select stands wholly above the window and the button
// wholly in it, the page scrolls back to the select, by Enter and by a click. With the select's
// foot 8 px above the window it scrolls 153 px in Chromium and Firefox and 52 px in WebKit at
// 390, and 62, 62 and 68 px at 1440, at da0804df and at a1c78223 alike; with its foot 2 px above,
// as this row places it, 147, 147 and 46 px at 390 and 56, 56 and 62 px at 1440 at da0804df, in
// both runs of each. The architect's ruling of 2026-11-21 12:25Z: a walk-back never scrolls to
// reach a control out of sight; it passes over those and lands on the region's own stop where
// none is in sight, and only a landing partly in sight may be scrolled, the least distance that
// shows it whole. With the select wholly in sight the focus lands on it and nothing scrolls. A
// focused control taken away, for which Firefox and WebKit fire no `focusout`, and each engine's
// own scroll of a focused control into view: the clause of the `webkit` and `firefox` projects
// that names the page-own focus row (e2e/playwright.config.ts).
test("clear brush window pressed with the Window control scrolled out of sight scrolls the Delivery page nothing and hands the focus to no control out of sight @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await seedDeliveryFixture();
  // A brush window inside the fixture's day, as a drag along the chart's time axis sets it.
  const brushed = `/delivery?${TIMELINE_FIXTURE_QUERY}&ws=2024-06-01T06%3A00%3A00Z&we=2024-06-01T18%3A00%3A00Z`;
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const placed of ["out of sight", "in sight"] as const) {
      for (const press of ["Enter", "click"] as const) {
        const at = `${size.width}x${size.height}, the Window control ${placed}, by ${press}`;
        await page.goto(brushed);
        const header = page.locator("header", {
          has: page.getByRole("heading", { level: 1, name: "Delivery timeline" }),
        });
        const clear = header.getByRole("button", { exact: true, name: "clear brush window" });
        const windowSelect = listTrigger(header, "Window");
        await expect(clear, `${at}: the brush drew no clear brush window`).toBeVisible();
        await expect(page.getByTestId("delivery-chart")).toBeVisible();
        /** Marks the box the page scrolls in, as the reader's scroll finds it: from `xl` up the
         *  page scrolls inside its own section where it overflows the window (main's #4631, the
         *  section's `xl:overflow-y-auto` in DeliveryPage.tsx), else the document. Answers its
         *  height, and whether it is the document. The reads below find the box by its mark. */
        const markScrollBox = () =>
          windowSelect.evaluate((node) => {
            for (const marked of document.querySelectorAll("[data-case-scroller]")) {
              marked.removeAttribute("data-case-scroller");
            }
            let scroller: Element = document.scrollingElement ?? document.documentElement;
            for (let box = node.parentElement; box !== null; box = box.parentElement) {
              const { overflowY } = getComputedStyle(box);
              if (
                (overflowY === "auto" || overflowY === "scroll") &&
                box.scrollHeight > box.clientHeight
              ) {
                scroller = box;
                break;
              }
            }
            scroller.setAttribute("data-case-scroller", "");
            return {
              document: scroller === document.scrollingElement,
              height: scroller.scrollHeight,
            };
          });
        // The page is drawn whole below the header, so it scrolls as far as the case needs: the
        // box it scrolls in, and that box's height, hold across two frames. The section overflows
        // only once the chart below has drawn, so the box is found again each time.
        await expect
          .poll(async () => {
            const was = await markScrollBox();
            await nextFrames(page);
            const now = await markScrollBox();
            return was.document === now.document && was.height === now.height;
          })
          .toBe(true);
        // The reader's own scroll, of that box: the select's foot 2 px above the box's visible
        // top, the button on the line under it wholly in sight; or the select's top 24 px under
        // that top. Read with the window's own scroll, so that a hand-off that scrolls either one
        // is caught at every width, as the row read the page before main's #4631.
        const before = await windowSelect.evaluate((node, out) => {
          const scroller =
            document.querySelector("[data-case-scroller]") ??
            document.scrollingElement ??
            document.documentElement;
          const sightTop =
            scroller === document.scrollingElement ? 0 : scroller.getBoundingClientRect().top;
          const box = node.getBoundingClientRect();
          scroller.scrollTop += out ? box.bottom - sightTop + 2 : box.top - sightTop - 24;
          return {
            box: Math.round(scroller.scrollTop),
            document: scroller === document.scrollingElement,
            window: Math.round(window.scrollY),
          };
        }, placed === "out of sight");
        /** The top of what the reader sees of the page: the marked scroll box's, or the window's. */
        const sightTop = await page.evaluate(() => {
          const scroller = document.querySelector("[data-case-scroller]");
          return scroller === null || scroller === document.scrollingElement
            ? 0
            : Math.round(scroller.getBoundingClientRect().top);
        });
        const select = await windowSelect.evaluate((node) => {
          const box = node.getBoundingClientRect();
          return { bottom: Math.round(box.bottom), top: Math.round(box.top) };
        });
        // One read places the case: the reader's scroll above is synchronous, and nothing else draws.
        const button = await clear.evaluate((node) => {
          const box = node.getBoundingClientRect();
          return { bottom: Math.round(box.bottom), top: Math.round(box.top) };
        });
        const selectAsPlaced =
          placed === "out of sight"
            ? select.bottom < sightTop
            : select.top >= sightTop && select.bottom <= size.height;
        if (!selectAsPlaced || button.top < sightTop || button.bottom > size.height) {
          wrong.push(
            `${at}: the page could not be scrolled to the case (scroll box ${before.box}, scrollY ${before.window}, select ${select.top}..${select.bottom} px, button ${button.top}..${button.bottom} px)`
          );
          continue;
        }
        if (press === "Enter") {
          // Focused with no scroll, as a Tab to the button before the reader's own scroll left it.
          await clear.evaluate((node) => {
            if (node instanceof HTMLElement) node.focus({ preventScroll: true });
          });
          await page.keyboard.press("Enter");
        } else {
          await clear.click();
        }
        await expect(clear, `${at}: clear brush window stayed drawn`).toHaveCount(0);
        await expect(page, `${at}: the brush stayed in the address`).not.toHaveURL(/[?&]ws=/);
        // The header's hand-off runs in the commit that takes the button away, so it is done by now.
        await nextFrames(page);
        await nextFrames(page);
        // One read is the answer: every hand-off has run, and nothing else moves the focus.
        const holder = await focusHolder(page);
        // Read after the same hand-offs as the read above, so where the focus is is settled.
        const landed = await page.evaluate((sightTop) => {
          const active = document.activeElement;
          const box = active?.getBoundingClientRect();
          const scroller =
            document.querySelector("[data-case-scroller]") ??
            document.scrollingElement ??
            document.documentElement;
          return {
            body: active === null || active === document.body,
            bottom: Math.round(box?.bottom ?? 0),
            meets:
              box !== undefined &&
              box.bottom > sightTop &&
              box.top < window.innerHeight &&
              box.right > 0 &&
              box.left < window.innerWidth,
            ownStop:
              active instanceof HTMLElement && active.matches('main, [data-testid="main-content"]'),
            scrolled: { box: Math.round(scroller.scrollTop), window: Math.round(window.scrollY) },
            top: Math.round(box?.top ?? 0),
          };
        }, sightTop);
        // Read after the same hand-offs as the reads above, so where the focus is is settled.
        const onSelect = await windowSelect.evaluate((node) => node === document.activeElement);
        const where = `${holder.name} at ${landed.top}..${landed.bottom} px, scroll box ${before.box} -> ${landed.scrolled.box}, scrollY ${before.window} -> ${landed.scrolled.window}`;
        if (!before.document && landed.scrolled.box !== before.box) {
          wrong.push(
            `${at}: the page's scroll box scrolled ${landed.scrolled.box - before.box} px, to ${where}`
          );
        }
        if (landed.scrolled.window !== before.window) {
          wrong.push(
            `${at}: the window scrolled ${landed.scrolled.window - before.window} px, to ${where}`
          );
        }
        if (landed.body) {
          wrong.push(`${at}: the focus is on the body, ${where}`);
        } else if (placed === "in sight" && !onSelect) {
          wrong.push(`${at}: the focus is on ${where}, not on the Window control`);
        } else if (!landed.ownStop && !landed.meets) {
          wrong.push(`${at}: the focus is on ${where}, out of sight`);
        }
      }
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): 38 Inbox rows seeded, and at two widths one thread and then two dismissed deep in
// the list.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p5-h2 and -p5-b2 at
// da0804df and a1c78223): the Inbox lists 38 asks and comment threads. The reader scrolls a thread
// row to the middle of the list, marks it (and the thread after it) with Space, and presses the
// selection bar's Dismiss with Enter. At da0804df the focus goes to the checkbox of the row after
// the dismissed ones while they are still drawn. As they leave, the list scrolls by their height,
// so that row stays where it stood rather than taking the marked row's place. At 1440 px the
// list's scrollTop drops by 81 to 82 px for one thread and by 162 to 163 px for two. At 390 px
// window.scrollY drops by 470 to 472 px and by 941 to 995 px, and the focused row is left below
// the window. This happened in Chromium, Firefox and WebKit, in 2 of 2 runs each. At a1c78223
// the focus goes to the list's first row, and the list scrolls to its top. The architect's ruling
// of 2026-11-21 06:0xZ (open-items.md, "The selection bar's return"): after a bulk action takes
// away the row marked last, the focus goes to the row that now stands at that row's place when it
// is in sight, else to the nearest row in sight before it, else to the list's own stop, and never
// to the body. That ruling also said the press never scrolls; INFRA-3135's architect's ruling of
// 2026-11-23 11:42Z replaces that rule: the press scrolls by no more than the line that closes
// above the list in its commit (below `sm`, Mark all read's line), so the landing's row stands
// where the row marked last stood. A focused control taken away, for which Firefox and WebKit
// fire no `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own
// focus row (e2e/playwright.config.ts).
test("a bulk Dismiss deep in the Inbox's list hands the focus to the row now at the marked row's place in sight, and scrolls by no more than the line that closes above the list @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const seedQuestion = async (title: string) => {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title}: what is the rule?` }, SURFACE_SESSION);
  };
  // Written oldest first and listed newest first, so the six threads stand between the late asks
  // above them and the early asks below them: rows 20 to 25 of 38.
  for (let index = 0; index < 12; index++) await seedQuestion(`Early ask ${index}`);
  for (let index = 0; index < 6; index++) {
    const issue = await createIssue({ project: "CORE", title: `Thread issue ${index}` });
    await seedCommentThread(issue.key, `Thread ${index}: when does it run?`, "At 03:00.");
  }
  for (let index = 0; index < 20; index++) await seedQuestion(`Late ask ${index}`);
  const list = page.getByTestId("inbox-list");
  const rows = page.locator("[data-inbox-row]");
  const bar = page.getByRole("toolbar", { name: "Selected rows" });
  const inboxRowOf = (id: string) => page.locator(`[data-inbox-row="${id}"]`);
  /** Where the window and the list's box stand, and what holds the focus: its name, the row it
   *  is in, its top, and whether its box is inside the window and inside the list's box. */
  const landing = () =>
    page.evaluate(() => {
      const frame = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
      const scrollTop = Math.round(frame?.scrollTop ?? 0);
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { inSight: false, name: "the body", row: null, scrollTop, scrollY, top: null };
      }
      const own = focused.getBoundingClientRect();
      const clip = frame?.getBoundingClientRect();
      const top = Math.max(0, clip?.top ?? 0);
      const bottom = Math.min(window.innerHeight, clip?.bottom ?? window.innerHeight);
      const label =
        focused instanceof HTMLInputElement || focused instanceof HTMLSelectElement
          ? focused.labels?.[0]?.textContent?.trim()
          : undefined;
      return {
        inSight:
          own.top >= top - 1 &&
          own.bottom <= bottom + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        name:
          focused.getAttribute("aria-label") ||
          label ||
          focused.textContent?.trim().slice(0, 40) ||
          focused.tagName.toLowerCase(),
        row: focused.closest("[data-inbox-row]")?.getAttribute("data-inbox-row") ?? null,
        scrollTop,
        scrollY,
        top: Math.round(own.top),
      };
    });
  /** Where the list's box stands in the page and where a row stands on screen: the row `id`
   *  names, else the row that holds the focus. */
  const rowAndList = (id: string | null) =>
    page.evaluate((rowId) => {
      const frame = document.querySelector('[data-testid="inbox-list"]');
      const row =
        rowId === null
          ? document.activeElement?.closest("[data-inbox-row]")
          : document.querySelector(`[data-inbox-row="${CSS.escape(rowId)}"]`);
      return {
        listTop: (frame?.getBoundingClientRect().top ?? 0) + window.scrollY,
        rowTop: row?.getBoundingClientRect().top ?? null,
      };
    }, id);
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    // One thread, then two: each case takes threads the cases before it left in the list.
    for (const count of [1, 2]) {
      const where = `Inbox at ${size.width}x${size.height}, ${count} thread${count === 1 ? "" : "s"} dismissed`;
      await page.goto("/?group=none");
      await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(30);
      // One read of the order: the poll above found the list drawn, and nothing moves it before the marks.
      const order = await rows.evaluateAll((nodes) =>
        nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
      );
      // The first run of `count` threads at least twelve rows down, with a row after it.
      let first = -1;
      for (let index = 12; first === -1 && index + count < order.length; index++) {
        if (order.slice(index, index + count).every((id) => id.startsWith("reply:"))) first = index;
      }
      expect(first, `${where}: no ${count} threads twelve rows down`).toBeGreaterThanOrEqual(12);
      const marked = order.slice(first, first + count);
      const after = order[first + count] as string;
      const afterName = await inboxRowOf(after)
        .getByRole("checkbox")
        .first()
        .getAttribute("aria-label");
      // The first marked row to the middle of what shows it: the list's own box beside the
      // reading column, else the window.
      await inboxRowOf(marked[0] as string).evaluate((node) => {
        const by = node.getBoundingClientRect().top;
        for (let box = node.parentElement; box !== null; box = box.parentElement) {
          const { overflowY } = getComputedStyle(box);
          if (
            (overflowY === "auto" || overflowY === "scroll") &&
            box.scrollHeight > box.clientHeight
          ) {
            const view = box.getBoundingClientRect();
            box.scrollTop += by - (view.top + view.height * 0.4);
            return;
          }
        }
        window.scrollBy(0, by - window.innerHeight * 0.4);
      });
      await settledScrollTop(list);
      await settledScrollTop(page);
      // The thread listed first is marked last: the row whose place the focus is to take.
      for (const id of [...marked].reverse()) {
        const box = inboxRowOf(id).getByRole("checkbox").first();
        await box.focus();
        await page.keyboard.press("Space");
        await expect(box).toBeChecked();
      }
      await expect(bar).toContainText(`${count} selected`);
      await bar.getByRole("button", { exact: true, name: "Dismiss selected threads" }).focus();
      await nextFrames(page);
      // One instant is the point: where the list and the window stand just before the press.
      const before = await landing();
      // One instant is the point here too: where the row marked last stands on screen and where
      // the list's box stands in the page, just before the press.
      const stood = await rowAndList(marked[0] as string);
      await page.keyboard.press("Enter");
      for (const id of marked) await expect(inboxRowOf(id)).toHaveCount(0);
      await expect(bar).toHaveCount(0);
      // The bar's hand-off and the list's hold of its rows run with the commits that took the
      // rows away, so both are done by now.
      await nextFrames(page);
      await nextFrames(page);
      // One read is the answer: every hand-off of the press has run, and nothing else moves.
      const now = await landing();
      const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}, list scrollTop ${before.scrollTop} -> ${now.scrollTop}`;
      if (now.row === null || marked.includes(now.row)) {
        wrong.push(`${where}: the focus is on ${now.name}, in no listed row (${numbers})`);
      } else if (!now.inSight) {
        wrong.push(
          `${where}: the focus is on ${now.name}, out of sight at ${now.top} px in a ${size.height} px window (${numbers})`
        );
      } else if (now.row !== after) {
        wrong.push(
          `${where}: the focus is on ${now.name}, not on "${afterName}", the row now at the marked row's place (${numbers})`
        );
      }
      // The landing's row stands where the row marked last stood on screen, and the window moved
      // by the height that closed above the list in the press's commit and no more: below `sm`,
      // Mark all read's line, which goes with the last unread threads. One read of the list's box
      // and the landing's row: every hand-off of the press has run, and nothing else moves them.
      const placed = await rowAndList(null);
      const closed = stood.listTop - placed.listTop;
      if (
        placed.rowTop === null ||
        stood.rowTop === null ||
        Math.abs(placed.rowTop - stood.rowTop) > 1
      ) {
        wrong.push(
          `${where}: the row of ${now.name} stands at ${placed.rowTop} px, not where the row marked last stood, at ${stood.rowTop} px (${numbers})`
        );
      }
      if (
        Math.abs(before.scrollY - now.scrollY - closed) > 1 ||
        now.scrollTop !== before.scrollTop
      ) {
        wrong.push(
          `${where}: the press scrolled by more than the ${Math.round(closed)} px that closed above the list, with the focus on ${now.name}: ${numbers}`
        );
      }
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): an epic of 44 issues seeded, and at two widths two of them taken out of it from the
// List's selection bar.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p5-h2 and -p5-b2 at
// da0804df and a1c78223): the project List, narrowed by its Epic filter to one epic, lists the
// epic and its 44 issues. The reader scrolls the table's box half way, marks two adjacent rows in
// sight with Space, and presses the bar's Take out of epic with Enter, which takes both rows out
// of the view. At da0804df the bar goes once the writes land, before the list drops the two rows.
// So its hand-off finds the row marked last still listed and focuses its checkbox. That row goes
// 30 to 60 ms later, and the focus with it: onto the body in Chromium, at 1440 px and 390 px in 2
// of 2 runs, and onto the pager's Rows per page in WebKit, 2 of 2 at each width, and in Firefox,
// 2 of 2 at 390 px and 1 of 2 at 1440 px, the other run ending on the body. At 390 px the pager's
// landing also scrolls the window by 22 px. At a1c78223 the same: the body or Rows per page. The
// focus never reaches the row now at the marked rows' place. The architect's ruling of 2026-11-21
// 06:0xZ (open-items.md, "The selection bar's return"): after a bulk action takes away the row
// marked last, the focus goes to the row that now stands at that row's place when it is in sight,
// else to the nearest row in sight before it, else to the list's own stop. It never scrolls and
// never goes to the body. A focused control taken away, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus
// row (e2e/playwright.config.ts).
test("a bulk action that takes the marked rows out of the project List's view hands the focus to the row now at their place, never the body or the pager @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 44; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  // The Epic filter on that epic: its value is the epic's key and title, as the column shows them.
  const view = `/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees:
   *  inside the window and the table's box, with nothing, such as the sticky head, drawn over its
   *  centre. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        const above = Math.max(0, frame.top);
        const below = Math.min(window.innerHeight, frame.bottom);
        if (own.top < above || own.bottom > below) return false;
        const hit = document.elementFromPoint(own.left + own.width / 2, own.top + own.height / 2);
        return hit !== null && row.contains(hit);
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** Where the window and the table's box stand, and what holds the focus: its name, the row it
   *  is in, its top, and whether its box is inside the window and inside the table's box. */
  const landing = () =>
    page.evaluate(() => {
      const frame = document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');
      const scrollTop = Math.round(frame?.scrollTop ?? 0);
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { inSight: false, name: "the body", row: null, scrollTop, scrollY, top: null };
      }
      const own = focused.getBoundingClientRect();
      const clip = frame?.getBoundingClientRect();
      const top = Math.max(0, clip?.top ?? 0);
      const bottom = Math.min(window.innerHeight, clip?.bottom ?? window.innerHeight);
      const label =
        focused instanceof HTMLInputElement || focused instanceof HTMLSelectElement
          ? focused.labels?.[0]?.textContent?.trim()
          : undefined;
      return {
        inSight:
          own.top >= top - 1 &&
          own.bottom <= bottom + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        name:
          focused.getAttribute("aria-label") ||
          label ||
          focused.textContent?.trim().slice(0, 40) ||
          focused.tagName.toLowerCase(),
        row: focused.closest("[data-issue-row]")?.getAttribute("data-issue-row") ?? null,
        scrollTop,
        scrollY,
        top: Math.round(own.top),
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    const where = `project List at ${size.width}x${size.height}, two rows taken out of the epic`;
    await page.setViewportSize(size);
    await page.goto(view);
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(6);
    // The window brings the table's box to just below the app's 56 px header, so its rows are in
    // the window at either width, and the box scrolls half way down its rows.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
    const { order, shown } = await drawn();
    // Two adjacent rows in the middle of what the reader sees, and the row after them.
    const at = order.indexOf(shown[Math.floor(shown.length / 2) - 1] ?? "");
    expect(
      at !== -1 && shown.includes(order[at + 1] ?? "") && at + 2 < order.length,
      `${where}: no two adjacent rows in sight mid-list (${shown.join(", ")})`
    ).toBe(true);
    const taken = [order[at] as string, order[at + 1] as string];
    const after = order[at + 2] as string;
    // The lower row first, so the row marked last is the upper one, whose place the focus takes.
    for (const key of [...taken].reverse()) {
      await markOf(key).focus();
      await page.keyboard.press("Space");
      await expect(markOf(key)).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
    await nextFrames(page);
    // One instant is the point: where the table and the window stand just before the press.
    const before = await landing();
    await page.keyboard.press("Enter");
    await expect(bar).toHaveCount(0);
    for (const key of taken) await expect(markOf(key)).toHaveCount(0);
    // The bar's hand-off runs with the commit that took the bar away, so it is done by now.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: every hand-off of the press has run, and nothing else moves.
    const now = await landing();
    const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}, table scrollTop ${before.scrollTop} -> ${now.scrollTop}`;
    if (now.row === null || taken.includes(now.row)) {
      wrong.push(`${where}: the focus is on ${now.name}, in no listed row (${numbers})`);
    } else if (!now.inSight) {
      wrong.push(
        `${where}: the focus is on ${now.name}, out of sight at ${now.top} px (${numbers})`
      );
    } else if (now.row !== after) {
      wrong.push(
        `${where}: the focus is on ${now.name}, not on "Select ${after}", the row now at the marked rows' place (${numbers})`
      );
    }
    if (now.scrollY !== before.scrollY || now.scrollTop !== before.scrollTop) {
      wrong.push(`${where}: the press scrolled, with the focus on ${now.name}: ${numbers}`);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135, round 24 (the tester's, from the architect's goal red team of 2026-11-22T11:06Z;
// measured in builds Job infra-3135-s-h-z2mdn at 3fce6150): with the Inbox narrowed to the
// Planner's asks and to questions, the Type chip is the shared `Chip`, a `<button
// data-control="chip">`, but the agent's is `<a aria-label="Clear agent filter" class="inline-flex
// min-h-11 items-center rounded-full">` around a `LabelPill` reading "Asks from Planner · clear",
// which `features/inbox/Inbox.tsx` builds, with no `data-control`, at 1440 and at 390 px alike.
// Spec v8, R2: "no file under `features/` holds a search box, the panel of a narrowing control,
// a chip, ... of its own". The agent's live link beside its chip, "Open live conversation with
// Planner", opens the agent's page and is no chip. Read at 1440 alone: the chip's markup is the
// same at every width.
test("the Inbox's agent narrowing draws the shared chip, as the row's other narrowings do", async ({
  page,
}) => {
  await seedAgents();
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(`/?agent=${encodeURIComponent(plannerSession.session_id)}&type=question`);
  const row = controlRow(page.getByTestId("main-content"));
  const typeChip = row.getByRole("button", { exact: true, name: "Remove Type: Question filter" });
  await expect(typeChip).toBeVisible();
  // The agent narrowing's chip is drawn from the address, and names the agent by its title.
  await expect(row.getByText(/Planner/).first()).toBeVisible();
  // One read is the answer: both chips are drawn, and nothing redraws the row now.
  const type = await typeChip.evaluate((chip) => ({
    class: chip.getAttribute("class") ?? "",
    tag: chip.tagName.toLowerCase(),
  }));
  // One read is the answer: both chips are drawn, and nothing redraws the row now.
  const agentChips = await row.evaluate((node) =>
    [...node.querySelectorAll("a, button")]
      .map((chip) => ({
        class: chip.getAttribute("class") ?? "",
        control: chip.getAttribute("data-control"),
        name: (chip.getAttribute("aria-label") ?? "").trim(),
        tag: chip.tagName.toLowerCase(),
        text: (chip.textContent ?? "").trim(),
      }))
      .filter(
        (chip) =>
          `${chip.name} ${chip.text}`.includes("Planner") &&
          !chip.name.startsWith("Open live conversation with")
      )
  );
  const wrong: string[] = [];
  if (agentChips.length !== 1) {
    wrong.push(
      `the row draws ${agentChips.length} controls naming the Planner where its one chip is: ${agentChips.map((chip) => `${chip.tag} "${chip.name || chip.text}"`).join(", ")}`
    );
  }
  for (const chip of agentChips) {
    const what = `the agent's chip, ${chip.tag} "${chip.name || chip.text}"`;
    if (chip.control !== CONTROL.chip) {
      wrong.push(`${what}, carries data-control=${chip.control ?? "none"}, not "${CONTROL.chip}"`);
    }
    if (chip.tag !== type.tag || chip.class !== type.class) {
      wrong.push(
        `${what}, is not drawn as the Type chip is: <${chip.tag} class="${chip.class}"> against <${type.tag} class="${type.class}">`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): eight pages opened at two widths over three seeded fixtures, with rows marked, the
// reading column drawn and the Agents page's broadcast composer open.
// INFRA-3135, round 24 (the tester's, from the architect's goal red team of 2026-11-22T11:06Z;
// measured in builds Job infra-3135-s-h-z2mdn at 3fce6150): the pages R2 lists draw lists of
// options as native `<select>` elements, whose list holds no text box that narrows it. At 1440
// px the Inbox draws "Snooze selected asks" in its selection bar, and "Priority of <issue>" and
// "Snooze <ask>" in the reading column's head; the Inbox drawer those three and a Priority and a
// Snooze on each row; the project List a Priority on each row and its pager's "Rows per page";
// the Board a Priority on each card; the Agents page's broadcast composer "Delivery mode"; the
// Delivery page "Window" and "Color merges by", in its list and in its timeline. At 390 px the
// same, the Inbox's own rows each drawing a Priority and a Snooze. The search bar and the
// Artifacts tab's own panel draw none, and every other list of options on these pages opened
// with a text box of its own: the filter panels' lists, the column filters, "Asked of" and
// "Move to epic". Spec v8, R3: "Every list of options a reader picks from holds a text box that
// narrows the options." The issue page above an Artifacts tab is not the tab (the architect's
// ruling of 2026-11-22 11:1xZ), so on that page only the tab's own panel is read.
test("no list of options on R2's pages lacks a text box: no page draws a native select, at 1440 and 390", async ({
  page,
}) => {
  test.setTimeout(300_000);
  const real = await seedRealStackSurfaces();
  const scene = { ...(await seedSurfaces()), deliveryQuery: real.deliveryQuery };
  /** The native selects `root` draws, each by its accessible name: its own label, or the words of
   *  the `<label>` around it without the text of its options. */
  const nativeSelects = (root: Locator) =>
    root.evaluate((node) =>
      [...node.querySelectorAll("select")]
        .filter((select) => select.getClientRects().length > 0)
        .map((select) => {
          const label = select.labels?.[0];
          const words =
            label === undefined
              ? ""
              : [...label.childNodes]
                  .filter((part) => part.nodeType === Node.TEXT_NODE)
                  .map((part) => part.textContent ?? "")
                  .join("")
                  .trim();
          return select.getAttribute("aria-label") || words || select.title || "unnamed";
        })
    );
  const wrong: string[] = [];
  /** Names the native selects one page draws. A row's own control names its row's issue, so the
   *  names are folded at the issue's key and counted, and each page is one line. */
  const refuse = (where: string, selects: readonly string[]) => {
    const counted = new Map<string, number>();
    for (const select of selects) {
      const control = select.replace(/\b[A-Z][A-Z0-9]*-\d+\b.*$/, "<row>");
      counted.set(control, (counted.get(control) ?? 0) + 1);
    }
    if (counted.size === 0) return;
    const names = [...counted].map(([control, count]) =>
      count > 1 ? `"${control}" x${count}` : `"${control}"`
    );
    wrong.push(`${where}: ${names.join(", ")} list options in a native select with no text box`);
  };
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const name of [
      "Inbox",
      "Inbox drawer",
      "project List",
      "project Board",
      "Agents page",
      "Delivery page",
      "search bar",
      "Artifacts tab",
    ] as const) {
      const surface = await openSurface(page, name, scene, { phone: size.width < 1280 });
      // Each list the page holds is drawn: the Inbox's reading column for its selected row, the
      // selection bar for a marked row, and the Agents page's broadcast composer.
      if (name === "Inbox" && hasReadingPane(page)) {
        await surface.firstRow.locator("time").first().click();
        await expect(surface.firstRow).toHaveAttribute("data-inbox-selected", "true");
      }
      await surface.select?.();
      if (name === "Agents page") {
        await surface.root
          .getByRole("checkbox", { exact: true, name: "Select all matching agents" })
          .check();
        await expect(surface.root.getByRole("region", { name: "Broadcast" })).toBeVisible();
      }
      const root = name === "Artifacts tab" ? page.locator("#issue-artifacts-panel") : surface.root;
      // One read is the answer: the waits above found every list this page draws drawn.
      refuse(`${name} at ${size.width}x${size.height}`, await nativeSelects(root));
      // The Delivery page's timeline, beside the list `openSurface` opens it on.
      if (name === "Delivery page") {
        await page.goto(`/delivery?${real.deliveryQuery}`);
        await expect(page.getByTestId("delivery-chart")).toBeVisible();
        // One read is the answer: the chart is drawn, and the header's controls with it.
        const timeline = await nativeSelects(surface.root);
        refuse(`Delivery page timeline at ${size.width}x${size.height}`, timeline);
      }
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two pages opened twice each at 390 px, once with no narrowing and once with more
// chips than one line holds.
// INFRA-3135, round 24 (the tester's, from the architect's goal red team of 2026-11-22T11:06Z;
// measured in builds Job infra-3135-s-h-z2mdn at 3fce6150): at 390x664, with more narrowings on
// than one line of chips holds, the chips stand in one strip that scrolls sideways
// (`ControlRow.tsx`, the strip's `flex-nowrap ... overflow-x-auto`). On the Inbox, with the 17
// chips this row puts on, the strip is 253 px wide with a scrollWidth of 2342 px, a scrollLeft
// set to 120 reads back 120, and 14 chips stand right of the row's 24..366 px; the row grows from
// 96 to 148 px and the first row moves 52 px, from 305 to 357 px. On the project List, with six
// labels and three narrowings, the strip's scrollWidth is 1502 px against 253, 6 chips stand
// outside the row, and the first row holds at 611 px. Spec v8, R1: "At a phone's width the
// row wraps rather than scrolling sideways, and the rule above holds there too", the rule being
// that the chips may take one more line inside the row, at most 52 px. The architect's ruling of
// 2026-11-22 11:1xZ: chips that do not fit the one chip line collapse into one shared '+N more'
// chip that opens the page's Filter panel, at every width, so nothing in the row scrolls sideways
// and the chip line stays one line within 52 px. A box scrolls sideways where its `overflow-x`
// lets the reader scroll it and its content is wider than it is; a chip's own text cut short with
// an ellipsis is clipped rather than scrolled.
test("at 390 px narrowings over one line scroll nothing in the bar sideways and move no first row, on the Inbox and the project List", async ({
  page,
}) => {
  test.setTimeout(120_000);
  const labels = [
    "alpha-rollout-review",
    "beta-ledger-mirror",
    "gamma-clock-drift",
    "delta-replay-window",
    "epsilon-lease-handover",
    "zeta-budget-plan",
  ];
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Rollout order", "Rollout budget", "Ledger mirror", "Clock drift"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { labels, status: "todo" });
    await createAsk(issue.key, { question: `${title}: what is the rule?` }, SURFACE_SESSION);
  }
  await page.setViewportSize({ height: 664, width: 390 });
  // The List is the view the project page opens on from here on: the reader's stored choice.
  await openIssuesTab(page, "CORE", "List");
  const row = controlRow(page.getByTestId("main-content"));
  const wrong: string[] = [];
  for (const [name, address, narrowed, rows] of [
    // Every value of the Inbox's four fields that a seeded row holds, and some it does not, so
    // every row stays listed under 17 narrowings.
    [
      "Inbox",
      "/",
      "/?priority=P0,P1,P2,P3,none&urgency=blocking,high,med,low,none&type=question,decision,approval,reply,agent&project=CORE,none",
      "[data-inbox-row]",
    ],
    // Six labels every seeded issue carries, and three narrowings every one of them passes.
    [
      "project List",
      "/projects/CORE/issues",
      `/projects/CORE/issues?${labels.map((label) => `label=${label}`).join("&")}&needs-you=1&status=todo&f.key=CORE`,
      "[data-issue-row]",
    ],
  ] as const) {
    const at = `${name} at 390x664 with narrowings over one line`;
    const firstRow = page.locator(rows).first();
    await page.goto(address);
    await expect(firstRow).toBeVisible();
    await resetScroll(page, firstRow);
    // One read is the measure: the list is drawn with no narrowing, scrolled to its top.
    const plain = await topOf(firstRow);
    await page.goto(narrowed);
    await expect(firstRow).toBeVisible();
    // The List's bar and the Inbox's are on the slots (R8): below 640 px of the bar's width no
    // chip stands in its line, and the trigger's badge stands for every narrowing.
    await expect(
      control(row, CONTROL.panelTrigger),
      `${at}: the trigger does not count the narrowings`
    ).toHaveAccessibleName(name === "Inbox" ? "Filter · 17" : "Filters · 9");
    await expect(
      control(row, CONTROL.chip),
      `${at}: chips stand in the narrow bar's line`
    ).toHaveCount(0);
    await resetScroll(page, firstRow);
    await nextFrames(page);
    // One read is the measure: the bar stands for the narrowings and the list is at its top.
    const top = await topOf(firstRow);
    if (Math.abs(top - plain) > 1) {
      // R8: the bar never grows, so the first row stays where it was, within 1 px.
      wrong.push(
        `${at}: the first row moved from ${Math.round(plain)} px to ${Math.round(top)} px, though the bar never grows`
      );
    }
    // One read is the answer: the chips are drawn, and nothing redraws the row now.
    const read = await row.evaluate((node) => {
      const box = node.getBoundingClientRect();
      const scrolls = [node, ...node.querySelectorAll<HTMLElement>("*")]
        .filter((element) => {
          const { overflowX } = getComputedStyle(element);
          const scrollable = overflowX === "auto" || overflowX === "scroll";
          return (
            (scrollable || element.hasAttribute("data-chip-strip")) &&
            element.scrollWidth > element.clientWidth + 1
          );
        })
        .map(
          (element) =>
            `${element.hasAttribute("data-chip-strip") ? "the chip strip" : element.tagName.toLowerCase()} (overflow-x ${getComputedStyle(element).overflowX}, scrollWidth ${element.scrollWidth} against clientWidth ${element.clientWidth})`
        );
      const outside = [...node.querySelectorAll<HTMLElement>("a, button, input, select")]
        .filter((element) =>
          element.checkVisibility({ opacityProperty: true, visibilityProperty: true })
        )
        .flatMap((element) => {
          const own = element.getBoundingClientRect();
          return own.left < box.left - 1 || own.right > box.right + 1
            ? [
                `"${element.getAttribute("aria-label") ?? (element.textContent ?? "").trim()}" at ${Math.round(own.left)}..${Math.round(own.right)} px`,
              ]
            : [];
        });
      return { left: Math.round(box.left), outside, right: Math.round(box.right), scrolls };
    });
    for (const scroll of read.scrolls) wrong.push(`${at}: ${scroll} scrolls sideways`);
    if (read.outside.length > 0) {
      wrong.push(
        `${at}: ${read.outside.length} controls stand outside the row's ${read.left}..${read.right} px: ${read.outside.join(", ")}`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

/** The height of a row of the Delivery page's measures table at 1440x900 as main drew it before
 *  this change, at 143b57a7, where `MeasuresPanel.tsx` wrote its own table with `py-0.5` cells
 *  of a 16 px line: `getBoundingClientRect().height` of each `tbody tr` in Chromium, read in
 *  builds Job infra-3135-s-b-h4hh4 over the measures fixture. Rows 1 to 28 of its 29 read 21 px
 *  and the last 20.5 px, which the table's collapsed borders give it: both within the 0.5 px the
 *  row allows. */
const MEASURES_ROW_HEIGHT = 21;

// INFRA-3135, round 24 (the tester's, from the architect's goal red team of 2026-11-22T11:06Z;
// measured in builds Jobs infra-3135-s-h-z2mdn at 3fce6150 and infra-3135-s-b-h4hh4 at
// 143b57a7): the measures table of `features/delivery/MeasuresPanel.tsx`, in the Delivery page's
// Definitions & alternatives fold, is drawn through `components/TableShell.tsx`'s compact recipe,
// whose cells are `py-1` where main's own were `py-0.5`. Over the measures fixture at 1440x900
// in Chromium, rows 1 to 28 of 29 are 25 px tall and the last 24.5 px at 3fce6150, against 21
// and 20.5 px at 143b57a7, with 4 px above and below each cell's 16 px line against 2 px, and
// the table is 749 px tall against 633 px inside its box held at 192 px. Spec v8, R8: "The rows,
// the cards, the reading pane and the panes' widths do not change", and of the measures table,
// "each draws the shared table markup with its behaviour unchanged".
test("the Delivery page's measures table keeps the height of its rows from before the shared table markup, at 1440", async ({
  page,
}) => {
  await seedMeasuresFixture();
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto(
    `/delivery?mode=list&from=${encodeURIComponent(FIXTURE_WINDOW.from)}&to=${encodeURIComponent(FIXTURE_WINDOW.to)}`
  );
  await expect(page.getByLabel("KPI targets")).toBeVisible();
  await page.getByRole("button", { name: "Definitions & alternatives" }).click();
  const table = page.getByRole("table").filter({ hasText: "Reached production" });
  await expect(table.locator("tbody tr").first()).toHaveText(/^2026-09-27 \(partial\)/);
  // One read is the answer: the fold has drawn every row, and nothing redraws it.
  const heights = await table
    .locator("tbody tr")
    .evaluateAll((rows) => rows.map((row) => row.getBoundingClientRect().height));
  expect(heights.length, "the measures table drew no rows").toBeGreaterThan(0);
  const wrong = heights.flatMap((height, index) =>
    Math.abs(height - MEASURES_ROW_HEIGHT) > 0.5
      ? [
          `row ${index + 1} of ${heights.length} is ${height} px tall, not ${MEASURES_ROW_HEIGHT} px`,
        ]
      : []
  );
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): three live agents pinned again at each of two widths, and the Agents page opened
// at each.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-a5b-h and -a5b-b at
// 3fce6150 and da0804df, and infra-3135-a5m at main's merge base 143b57a7): three live agents
// are pinned, Alpha first, and the reader presses Enter on Alpha's Unpin on the Agents page. The
// page lists pinned agents first, in the order they were pinned (`orderAgents`,
// features/agents/AgentsPage.tsx), so Alpha's row moves to the end of the list, below Beta's and
// Gamma's, and the focus follows it to that row's Pin Alpha, in Chromium, Firefox and WebKit at
// both commits. At 1440x900 the three rows stand in the window and Pin Alpha with them, at
// 505 px. At 390x664 the rows are 126 px tall. In Chromium and Firefox the focus put on Unpin
// Alpha has scrolled the window to 328 px, and Pin Alpha lands at 546 px, in sight. In WebKit the
// window stands at 58 px, Pin Alpha lands at 816 px, below the 664 px window, and nothing
// scrolls, at both commits. Main does the same in WebKit: Pin Alpha at 505 px at 1440, and at
// 816 px with the window held at 58 px at 390. The architect's rulings: a landing the reader
// caused must be in view (2026-11-19 10:3xZ, WCAG 2.2's 2.4.11 Focus Not Obscured), and the one
// scroll a landing may make is to the nearest edge of a control partly in sight, the least
// distance that shows it whole (2026-11-21 12:3xZ). Which control the press lands on is not read
// here: 2026-11-19 22:3xZ names the same control in the row now at the pressed row's place, and
// main's page means the focus to follow the row to where it lands (AgentsPage.tsx,
// `focusAfterPin`). A focused control taken away, for which Firefox and WebKit fire no
// `focusout` (React moves the row's node): the clause of the `webkit` and `firefox` projects that
// names the page-own focus row (e2e/playwright.config.ts).
test("Unpin on a pinned agent keeps the keyboard focus on a control in sight, on the Agents page at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  const pinnedAgent = (sessionId: string, title: string): FakeSession => ({
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - 5_000,
    machine_id: "pin-host",
    roles: [],
    session_id: sessionId,
    title,
  });
  const alpha = pinnedAgent("01a0e200-4848-7473-acc5-0000000000a5", "Alpha");
  const beta = pinnedAgent("01a0e200-4848-7473-acc5-0000000000b5", "Beta");
  const gamma = pinnedAgent("01a0e200-4848-7473-acc5-0000000000c5", "Gamma");
  const agents = [alpha, beta, gamma];
  await setLiveSessions(agents);
  /** What holds the focus and where the window stands: the focused element's name, the agent row
   *  it is in, its top and height, and whether its box stands wholly inside the window. */
  const landing = () =>
    page.evaluate(() => {
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { height: 0, inSight: false, name: "the body", row: null, scrollY, top: null };
      }
      const own = focused.getBoundingClientRect();
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      return {
        height: Math.round(own.height),
        inSight:
          own.top >= -1 &&
          own.bottom <= window.innerHeight + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        name: `${focused.tagName.toLowerCase()} "${words}"`,
        row: focused.closest("[data-agent-row]")?.getAttribute("data-agent-row") ?? null,
        scrollY,
        top: Math.round(own.top),
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Agents page at ${size.width}x${size.height}`;
    // Pinned again in this order at each width, so Alpha is the first pinned row each time.
    for (const agent of agents) await putAgentState(agent.session_id, { pinned: false });
    for (const agent of agents) await putAgentState(agent.session_id, { pinned: true });
    await page.goto("/agents");
    const unpin = agentRow(page, alpha.session_id).getByRole("button", {
      exact: true,
      name: "Unpin Alpha",
    });
    await expect(unpin).toBeVisible();
    await expect(
      agentRow(page, gamma.session_id).getByRole("button", { exact: true, name: "Unpin Gamma" })
    ).toBeVisible();
    await unpin.focus();
    await nextFrames(page);
    // One instant is the point: where the window stands just before the press.
    const before = await landing();
    await page.keyboard.press("Enter");
    // The pin's write has landed once Alpha's row offers to pin it again.
    await expect(
      agentRow(page, alpha.session_id).getByRole("button", { exact: true, name: "Pin Alpha" })
    ).toBeVisible();
    // The poll waits, up to the expect timeout, for the focus the moved row took to land again.
    await expect
      .poll(async () => (await landing()).top !== null)
      .toBe(true)
      .catch(() => undefined);
    await settledScrollTop(page);
    // One read is the answer: every hand-off of the press has run, and the window holds still.
    const now = await landing();
    const rowTitle = agents.find((agent) => agent.session_id === now.row)?.title ?? now.row;
    const what = now.row === null ? now.name : `${now.name} in ${rowTitle}'s row`;
    const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}`;
    if (now.top === null) {
      wrong.push(`${where}: after Enter on Unpin Alpha the focus is on the body (${numbers})`);
    } else if (!now.inSight) {
      wrong.push(
        `${where}: after Enter on Unpin Alpha the focus is on ${what}, out of sight at ${now.top} px in a ${size.height} px window (${numbers})`
      );
    }
    // The 12:3xZ allowance: the landing's own height, and a pixel for the rounding of a box.
    const allowed = now.height + 1;
    if (Math.abs(now.scrollY - before.scrollY) > allowed) {
      wrong.push(
        `${where}: after Enter on Unpin Alpha the window scrolled ${now.scrollY - before.scrollY} px, more than the landing's own height, to ${what} (${numbers})`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): three comment threads seeded at each of two widths, and the Inbox opened at each.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-a5b-h and -a5b-b at
// 3fce6150 and da0804df, and infra-3135-a5m at main's merge base 143b57a7): the Inbox lists
// three comment threads, newest first, and the reader presses Enter on the first one's Dismiss.
// The row goes once its write lands, and the next thread's row moves up into its place. At 1440
// nothing scrolls. At 390 the rows are 459 px tall and the focus put on the first Dismiss has
// scrolled the window to 437 px, with that row's top 132 px above the window. In Chromium and
// Firefox the window holds at 437 px. In WebKit it scrolls up to 167 px, though it can scroll to
// 1642 px, so the page did not clamp it; at both commits. At main, WebKit scrolls the window at
// 390 from 445 px to 0, though it can scroll to 1650 px. The architect's ruling of 2026-11-21
// 17:0xZ: a row the reader's own press takes away frees its whole height and nothing scrolls. A
// focused control taken away, for which Firefox and WebKit fire no `focusout`: the clause of the
// `webkit` and `firefox` projects that names the page-own focus row (e2e/playwright.config.ts).
test("a thread's Dismiss in the Inbox's list scrolls nothing, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const list = page.getByTestId("inbox-list");
  /** Where the window, the list's box and the reading column's box, where it is drawn, stand and
   *  how far each can scroll, and, for a failure's message, what holds the focus and its top. */
  const standing = () =>
    page.evaluate(() => {
      const frame = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
      const column = document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]');
      const scroller = document.scrollingElement ?? document.documentElement;
      const scroll = {
        farthestTop: frame === null ? 0 : Math.round(frame.scrollHeight - frame.clientHeight),
        farthestY: Math.round(scroller.scrollHeight - scroller.clientHeight),
        pane:
          column === null
            ? null
            : {
                farthest: Math.round(column.scrollHeight - column.clientHeight),
                top: Math.round(column.scrollTop),
              },
        scrollTop: Math.round(frame?.scrollTop ?? 0),
        scrollY: Math.round(window.scrollY),
      };
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { ...scroll, name: "the body", top: null };
      }
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .trim()
        .slice(0, 60);
      return {
        ...scroll,
        name: `${focused.tagName.toLowerCase()} "${words}"`,
        top: Math.round(focused.getBoundingClientRect().top),
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Inbox at ${size.width}x${size.height}`;
    const issue = await createIssue({ project: "CORE", title: `Mirror lag at ${size.width}` });
    for (const word of ["first", "second", "third"]) {
      await seedCommentThread(
        issue.key,
        `Is the ${word} mirror caught up at ${size.width}?`,
        "It is."
      );
    }
    await page.goto("/");
    const dismissable = page
      .locator("[data-inbox-row]")
      .filter({ has: page.locator("[data-inbox-dismiss]") });
    await expect(dismissable.nth(2)).toBeVisible();
    // One read of the order: the wait above found three thread rows drawn, newest first.
    const target = (await dismissable.first().getAttribute("data-inbox-row")) ?? "";
    const row = page.locator(`[data-inbox-row="${target}"]`);
    await row.locator("[data-inbox-dismiss]").focus();
    await nextFrames(page);
    // One instant is the point: where the window, the list and the reading column stand just
    // before the press.
    const before = await standing();
    await page.keyboard.press("Enter");
    await expect(row).toHaveCount(0);
    // A scroll lands over frames, so each box is read once it holds still.
    await settledScrollTop(list);
    await settledScrollTop(page);
    // The reading column's box, where it was drawn before the press, is read once it holds still
    // too: the Inbox's own hand-off can land in it at 1440.
    if (before.pane !== null) await settledScrollTop(page.getByTestId("inbox-pane-scroller"));
    // One read is the answer: every box holds still, and nothing else moves them.
    const now = await standing();
    const columnNumbers =
      before.pane === null || now.pane === null
        ? ""
        : `, reading column scrollTop ${before.pane.top} -> ${now.pane.top} (farthest ${now.pane.farthest})`;
    const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY} (farthest ${now.farthestY}), list scrollTop ${before.scrollTop} -> ${now.scrollTop} (farthest ${now.farthestTop})${columnNumbers}`;
    // A box the page clamped stands at its farthest scroll now, and stood farther before.
    const windowClamped = now.scrollY >= now.farthestY - 1 && before.scrollY > now.farthestY;
    const listClamped = now.scrollTop >= now.farthestTop - 1 && before.scrollTop > now.farthestTop;
    if (Math.abs(now.scrollY - before.scrollY) > 1 && !windowClamped) {
      wrong.push(
        `${where}: after Enter on the first thread's Dismiss the window scrolled ${now.scrollY - before.scrollY} px, with the focus on ${now.name} at ${now.top} px (${numbers})`
      );
    }
    if (Math.abs(now.scrollTop - before.scrollTop) > 1 && !listClamped) {
      wrong.push(
        `${where}: after Enter on the first thread's Dismiss the list scrolled ${now.scrollTop - before.scrollTop} px, with the focus on ${now.name} at ${now.top} px (${numbers})`
      );
    }
    if (before.pane !== null && now.pane !== null) {
      const columnClamped =
        now.pane.top >= now.pane.farthest - 1 && before.pane.top > now.pane.farthest;
      if (Math.abs(now.pane.top - before.pane.top) > 1 && !columnClamped) {
        wrong.push(
          `${where}: after Enter on the first thread's Dismiss the reading column scrolled ${now.pane.top - before.pane.top} px, with the focus on ${now.name} at ${now.top} px (${numbers})`
        );
      }
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// The row above reads the reading column's scroll at 1440, where its threads' replies are one
// line: the column then cannot scroll, and that reading proves nothing. Here each reply is as tall
// as forty paragraphs, so the column overflows the 900 px window, and the row fails where the
// column cannot scroll. The same press takes away the focused Dismiss, so the same clause of the
// `webkit` and `firefox` projects as the row above: the page-own focus row
// (e2e/playwright.config.ts).
test("a thread's Dismiss in the Inbox's list leaves a reading column that can scroll where it stood, at 1440 @firefox @webkit", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await page.setViewportSize({ height: 900, width: 1440 });
  const issue = await createIssue({ project: "CORE", title: "Mirror lag with a long reply" });
  const reply = Array.from({ length: 40 }, (_, line) => `Line ${line + 1} of the report.`).join(
    "\n\n"
  );
  for (const word of ["first", "second", "third"]) {
    await seedCommentThread(issue.key, `Is the ${word} mirror caught up?`, reply);
  }
  await page.goto("/");
  const dismissable = page
    .locator("[data-inbox-row]")
    .filter({ has: page.locator("[data-inbox-dismiss]") });
  await expect(dismissable.nth(2)).toBeVisible();
  // One read of the order: the wait above found three thread rows drawn, newest first.
  const target = (await dismissable.first().getAttribute("data-inbox-row")) ?? "";
  const row = page.locator(`[data-inbox-row="${target}"]`);
  await row.locator("[data-inbox-dismiss]").focus();
  await nextFrames(page);
  const column = page.getByTestId("inbox-pane-scroller");
  /** Where the reading column stands and how far it can scroll. */
  const standing = () =>
    column.evaluate((box) => ({
      farthest: Math.round(box.scrollHeight - box.clientHeight),
      top: Math.round(box.scrollTop),
    }));
  // The column draws its thread's forty paragraphs over frames, so the row waits until the column
  // can scroll and holds still; then one read is where it stands just before the press.
  await expect
    .poll(async () => (await standing()).farthest, {
      message: "the reading column can scroll before the press",
    })
    .toBeGreaterThan(0);
  await settledScrollTop(column);
  const before = await standing();
  await page.keyboard.press("Enter");
  await expect(row).toHaveCount(0);
  // A scroll lands over frames, so the column is read once it holds still.
  await settledScrollTop(column);
  const now = await standing();
  // A column the page clamped stands at its farthest scroll now, and stood farther before.
  const clamped = now.top >= now.farthest - 1 && before.top > now.farthest;
  expect(
    Math.abs(now.top - before.top) <= 1 || clamped,
    `after Enter on the first thread's Dismiss the reading column scrolled from ${before.top} to ${now.top} px (farthest ${before.farthest} -> ${now.farthest})`
  ).toBe(true);
});

/** Where the keyboard focus is, read once: the focused element's tag and name, whether it is the
 *  body, whether it stands inside an element `inside` matches (a CSS selector, or none), whether
 *  its box stands wholly inside the window, and where the window stands. */
function focusPlace(
  page: Page,
  inside?: string
): Promise<{ body: boolean; inSight: boolean; inside: boolean; name: string; scrollY: number }> {
  return page.evaluate((inside) => {
    const scrollY = Math.round(window.scrollY);
    const focused = document.activeElement;
    if (!(focused instanceof HTMLElement) || focused === document.body) {
      return { body: true, inSight: false, inside: false, name: "the body", scrollY };
    }
    const own = focused.getBoundingClientRect();
    const words = (focused.getAttribute("aria-label") || focused.textContent || "")
      .replace(/\s+/g, " ")
      .trim()
      .slice(0, 40);
    return {
      body: false,
      inSight:
        own.top >= -1 &&
        own.bottom <= window.innerHeight + 1 &&
        own.left >= -1 &&
        own.right <= window.innerWidth + 1,
      inside: inside !== null && focused.closest(inside) !== null,
      name: `${focused.tagName.toLowerCase()} "${words}"`,
      scrollY,
    };
  }, inside ?? null);
}

// SLOW(>=2s): three questions seeded and the Inbox opened at each of two widths, and a decline
// held through a Tab at each.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-n-h2 and -n-b at 3fce6150
// and da0804df, and this row in -n-red at 3fce6150): the reader opens a question's Decline to
// answer from its card in the Inbox, types a note, and presses Enter on the dialog's Decline to
// answer with the decline held. The dialog's button turns "Declining…" and is natively disabled
// (`disabled={saving}`, features/inbox/DeclineControl.tsx:150), and so are its Cancel (:142) and
// its note (:116), so nothing in the dialog can hold the focus. The dialog stays open, modal, and
// the focus falls to the body: in Chromium and Firefox within 40 ms of the press, in WebKit at its
// next rendering update, 65 to 1060 ms after it. A Tab then moves the focus out of the open dialog,
// to the card's Reaches 1 behind it, in Chromium, Firefox and WebKit at 1440x900 and 390x664, at
// both commits. After the decline lands at 1440, with the focus the Tab took on that card's
// Reaches 1, the focus is on a control in sight, in all three engines at 3fce6150; without the Tab
// it is on the Resize reading pane divider at 3fce6150 and on the next card's Reaches 1 at
// da0804df. The rules: a control held while the write its press started is out is held through
// components/held-press.ts, never natively disabled, so it keeps the keyboard focus
// (its own doc comment); a focus never leaves an open modal dialog, whose own stop
// takes it (the architect's correction of 2026-11-22 02:2xZ); and in the reading column the focus
// goes to the column's own focus stop, whoever caused the loss (2026-11-19 13:0xZ, open-items.md,
// "The replacement rule, narrowed"). Below the column's width no hand-off of the Inbox's own lands
// the focus the decline took, so the row reads the landing at 1440 alone. A focused control
// disabled under the press, which each engine moves to the body at its own time, and a focused
// control taken away, for which Firefox and WebKit fire no `focusout`: the clause of the `webkit`
// and `firefox` projects that names the page-own focus row (e2e/playwright.config.ts).
test("the Inbox's Decline dialog keeps the keyboard focus inside itself while its write is out, and the reading column keeps it on a control after it lands @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with three questions seeded and a decline held through a Tab.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const asker = {
    actor: { id: "e2e-decline-asker", kind: "session" as const },
    as: "agent" as const,
  };
  // Every decline is held until the row lets it through, so the page is read while it is out.
  const held: Route[] = [];
  await page.route("**/api/v1/asks/*/decline", async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    held.push(route);
  });
  const inDialog = '[role="dialog"][aria-label="Decline to answer"]';
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Inbox at ${size.width}x${size.height}`;
    const asks: string[] = [];
    for (const what of ["lease format", "rollout order", "cache size"]) {
      const issue = await createIssue({ project: "CORE", title: `The ${what} at ${size.width}` });
      await patchIssue(issue.key, { assignee: "alice" });
      const question = `Which ${what} at ${size.width}?`;
      const ask = await createAsk(
        issue.key,
        { options: [{ label: "Ship" }, { label: "Hold" }], question },
        asker
      );
      asks.push(ask.id);
    }
    await page.goto("/");
    for (const id of asks) await expect(inboxRow(page, id)).toBeVisible();
    const target = asks[0] ?? "";
    const card = await askCard(page, target);
    const trigger = card.getByRole("button", { exact: true, name: "Decline to answer" });
    await trigger.focus();
    await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog", { name: "Decline to answer" });
    await expect(dialog).toBeVisible();
    // The dialog puts the focus in its note a frame after it opens.
    await expect(dialog.getByLabel("Note (optional)")).toBeFocused();
    await page.keyboard.type("Not ours to decide.");
    await dialog.getByRole("button", { exact: true, name: "Decline to answer" }).focus();
    await page.keyboard.press("Enter");
    // The store sends a write again under its own key when a request got no answer, so the
    // declines are counted by their Idempotency-Keys: one per decline, however many requests.
    await expect
      .poll(
        () => new Set(held.map((route) => route.request().headers()["idempotency-key"])).size,
        `${where}: the decline was never sent`
      )
      .toBe(1);
    await expect(dialog.getByRole("button", { name: "Declining…" })).toBeVisible();
    // The focus fixup of a disabled control runs in a rendering update; two frames hold one.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: the decline is held, and its press's renders have all run.
    const holding = await focusPlace(page, inDialog);
    if (!holding.inside) {
      wrong.push(
        `${where}: with the decline held the focus is on ${holding.name}, outside the open Decline to answer dialog`
      );
    }
    await page.keyboard.press("Tab");
    await nextFrames(page);
    // One read is the answer: the Tab has moved the focus, and its renders have run.
    const tabbed = await focusPlace(page, inDialog);
    if (!tabbed.inside) {
      wrong.push(
        `${where}: with the decline held a Tab moves the focus to ${tabbed.name}, outside the open Decline to answer dialog`
      );
    }
    for (const route of held.splice(0)) await route.continue();
    await expect(dialog).toHaveCount(0);
    await expect(inboxRow(page, target)).toHaveCount(0);
    // The reading column hands the focus its question took with it to the column's own stop.
    // Below the column's width no hand-off of the Inbox's own lands it, so the landing is read
    // where the column is drawn alone.
    if (hasReadingPane(page)) {
      await settledScrollTop(page);
      await nextFrames(page);
      // One read is the answer: the decline has landed, its row is gone, the window holds still.
      const landed = await focusPlace(page);
      if (landed.body) {
        wrong.push(`${where}: after the decline lands the focus is on the body`);
      } else if (!landed.inSight) {
        wrong.push(
          `${where}: after the decline lands the focus is on ${landed.name}, out of sight (window.scrollY ${landed.scrollY})`
        );
      }
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): an issue's references read answered, failed over its answer and retried, at each
// of two widths.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-n2-h and -n2-b at 3fce6150
// and da0804df, and this row in -n-red at 3fce6150): an issue's Artifacts tab has read the issue's
// references once, and a message on another issue that mentions it makes the live feed read them
// again, which fails (500, three times by the shared retry policy). The tab says "References
// unavailable" with Retry, over the answer the read still holds. Enter on Retry, with the read it
// sends held, draws Retry natively disabled (`disabled={retrying}`, components/QueryError.tsx:49,
// given `retrying={references.isFetching}` by features/artifacts/ArtifactsTab.tsx:488), and the
// focus falls to the body for the whole hold, in Chromium, Firefox and WebKit at 1440x900 and
// 390x664, at both commits. A read that holds no answer yet is another path, which this row does
// not take: TanStack sets it back to pending the moment Retry sends, so the line and Retry go in
// the press's own render. The rule: a control held while the read its press started is out is
// held through components/held-press.ts, never natively disabled, so it keeps the keyboard focus
// (its own doc comment). A focused control disabled under the press, which each engine
// moves to the body at its own time: the clause of the `webkit` and `firefox` projects that names
// the page-own focus row (e2e/playwright.config.ts).
test("a references Retry on the Artifacts tab keeps the keyboard focus while its read is out @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with a read answered, failed through the shared policy's retries and held.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  // The references read of the issue the row is on: answered, failed or held as the row says.
  const reads = { held: [] as Route[], mode: "answer" as "answer" | "fail" | "hold", path: "" };
  await page.route(
    (url) => url.pathname === reads.path,
    async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (reads.mode === "hold") {
        reads.held.push(route);
        return;
      }
      if (reads.mode === "answer") return route.continue();
      await route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "The references read failed." }),
        contentType: "application/json",
        status: 500,
      });
    }
  );
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Artifacts tab at ${size.width}x${size.height}`;
    const issue = await createIssue({ project: "CORE", title: `Lease format at ${size.width}` });
    const source = await createIssue({ project: "OPS", title: `Mirror plan at ${size.width}` });
    await createMessage(source.key, { body: `Depends on dispatch://${issue.key}.` });
    reads.mode = "answer";
    reads.path = `/api/v1/issues/${issue.key}/references`;
    await page.goto(`/issues/${issue.key}/artifacts`);
    // The read has answered once the tab counts what references the issue.
    await expect(
      page.locator("summary").filter({ hasText: /^Referenced by \(1\)$/ })
    ).toBeVisible();
    reads.mode = "fail";
    await createMessage(source.key, { body: `Also dispatch://${issue.key}.` });
    // The live feed reads the references again, and the shared policy retries the 500 twice.
    const alert = page.getByRole("alert").filter({ hasText: "References unavailable" });
    await expect(alert).toBeVisible();
    await alert.getByRole("button", { exact: true, name: "Retry" }).focus();
    reads.mode = "hold";
    await page.keyboard.press("Enter");
    await expect.poll(() => reads.held.length, `${where}: Retry sent no read`).toBe(1);
    // The focus fixup of a disabled control runs in a rendering update; two frames hold one.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: Retry's read is held, and its press's renders have all run.
    const holding = await focusPlace(page);
    if (holding.body) {
      wrong.push(`${where}: with the read Retry sent held the focus is on the body`);
    }
    reads.mode = "answer";
    for (const route of reads.held.splice(0)) await route.continue();
    await expect(alert).toHaveCount(0);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): a question's backlinks read answered, failed over its answer and retried in the
// Inbox, at each of two widths.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-n2-h and -n2-b at 3fce6150
// and da0804df, and this row in -n-red at 3fce6150): a message on another issue mentions a
// question, and the reader opens Referenced by (1) on the question's card in the Inbox, which reads
// its backlinks. A second such message makes the live feed read them again, which fails (500, three
// times by the shared retry policy), and the panel says "Referenced by unavailable." with Retry
// over the list it still holds. Enter on Retry, with the read it sends held, draws Retry natively
// disabled (`disabled={query.isFetching}`, features/refs/ReferencedBy.tsx:323; the card's panel,
// features/inbox/AskCard.tsx:558, passes no `retried`, which only the new issue page's Details
// does), and the focus falls to the body for the whole hold, in Chromium, Firefox and WebKit at
// 1440x900 and 390x664, at both commits. A read that holds no answer yet is another path, which
// this row does not take: TanStack sets it back to pending the moment Retry sends, so the line and
// Retry go in the press's own render. The rule: a control held while the read its press started is
// out is held through components/held-press.ts, never natively disabled, so it keeps the keyboard
// focus (its own doc comment). A focused control disabled under the press, which each
// engine moves to the body at its own time: the clause of the `webkit` and `firefox` projects that
// names the page-own focus row (e2e/playwright.config.ts).
test("the Inbox card's Referenced by Retry keeps the keyboard focus while its read is out @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with a read answered, failed through the shared policy's retries and held.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const asker = {
    actor: { id: "e2e-backlink-asker", kind: "session" as const },
    as: "agent" as const,
  };
  // The backlinks read of the question the row is on: answered, failed or held as the row says.
  const reads = { held: [] as Route[], mode: "answer" as "answer" | "fail" | "hold", target: "" };
  await page.route(
    (url) =>
      url.pathname === "/api/v1/references" &&
      reads.target !== "" &&
      (url.searchParams.get("to") ?? "").includes(reads.target),
    async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (reads.mode === "hold") {
        reads.held.push(route);
        return;
      }
      if (reads.mode === "answer") return route.continue();
      await route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "The backlinks read failed." }),
        contentType: "application/json",
        status: 500,
      });
    }
  );
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Inbox at ${size.width}x${size.height}`;
    const issue = await createIssue({ project: "CORE", title: `Rollout order at ${size.width}` });
    await patchIssue(issue.key, { assignee: "alice" });
    const ask = await createAsk(
      issue.key,
      {
        options: [{ label: "Ship" }, { label: "Hold" }],
        question: `Which rollout order at ${size.width}?`,
      },
      asker
    );
    const source = await createIssue({ project: "OPS", title: `Mirror plan at ${size.width}` });
    await createMessage(source.key, { body: `Depends on dispatch://${issue.key}/ask/${ask.id}.` });
    reads.mode = "answer";
    reads.target = ask.id;
    await page.goto("/");
    await expect(inboxRow(page, ask.id)).toBeVisible();
    const card = await askCard(page, ask.id);
    await card.getByRole("button", { name: /^Referenced by \(1\)$/ }).focus();
    await page.keyboard.press("Enter");
    const panel = card.getByRole("region", { name: "Referenced by" });
    // The read has answered once the panel lists the message that mentions the question.
    await expect(panel.getByText(`Message · ${source.key}`).first()).toBeVisible();
    reads.mode = "fail";
    await createMessage(source.key, { body: `Again dispatch://${issue.key}/ask/${ask.id}.` });
    // The live feed reads the backlinks again, and the shared policy retries the 500 twice.
    const failed = panel.getByText("Referenced by unavailable.");
    await expect(failed).toBeVisible();
    await panel.getByRole("button", { exact: true, name: "Retry" }).focus();
    reads.mode = "hold";
    await page.keyboard.press("Enter");
    await expect.poll(() => reads.held.length, `${where}: Retry sent no read`).toBe(1);
    // The focus fixup of a disabled control runs in a rendering update; two frames hold one.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: Retry's read is held, and its press's renders have all run.
    const holding = await focusPlace(page);
    if (holding.body) {
      wrong.push(`${where}: with the read Retry sent held the focus is on the body`);
    }
    reads.mode = "answer";
    for (const route of reads.held.splice(0)) await route.continue();
    await expect(failed).toHaveCount(0);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): four questions seeded with a follower and a subscriber, and an Unfollow and an
// Unsubscribe confirmed and held, at each of two widths.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-n-h2 and -n-b at 3fce6150
// and da0804df, and this row in -n-red at 3fce6150): a question in the Inbox reaches its asker, a
// session that replied to it, and a session subscribed to its issue, so its card reads Reaches 3
// and lists an Unfollow for each follower and an Unsubscribe for the subscriber. The reader presses
// Enter on the replier's Unfollow, or on the subscriber's Unsubscribe, and Enter on the dialog's
// Confirm, with the write held. Confirm closes the dialog in the same press
// (AskRecipients.tsx:484-485, :501-502), and the dialog gives the focus back to the control that
// opened it (features/shell/useDialog.ts:180-184), which is natively disabled while the write is
// out (`disabled={unfollow.isPending}`, features/inbox/AskRecipients.tsx:365, every Unfollow of the
// card; `disabled={unsubscribe.isPending}`, :420), so the focus falls to the body and the app-wide
// hand-off those commits had lands it: after Unfollow on the subscriber's Unsubscribe, after
// Unsubscribe at 1440 on the subscriber's own name link, and at 390 on another question's Select
// checkbox, outside the card, in Chromium, Firefox and WebKit at both commits. The rule: a control
// held while the write its press started is out is held through components/held-press.ts, never
// natively disabled, so it keeps the keyboard focus, and the dialog's hand-back finds it (its own
// doc comment). A focused control disabled under the press, which each engine moves to the body
// at its own time, and Confirm taken away with its dialog, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus row
// (e2e/playwright.config.ts).
test("Unfollow and Unsubscribe on an Inbox card's recipients keep the keyboard focus on a control of the card while their write is out @firefox @webkit", async ({
  page,
}) => {
  // Two widths by two removals, each with a question seeded and the Inbox opened for it.
  test.setTimeout(150_000);
  await createProject({ key: "CORE", name: "Core" });
  const asker = { id: "e2e-recipients-asker", kind: "session" as const };
  const replier = { id: "e2e-recipients-replier", kind: "session" as const };
  const watcher = "e2e-recipients-watcher";
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: replier.id, title: "Replier (e2e)" },
    { session_id: watcher, title: "Watcher (e2e)" },
  ]);
  // The removal the row presses is held until the row lets it through.
  const writes = { held: [] as Route[], path: "" };
  await page.route(
    (url) => url.pathname === writes.path,
    async (route) => {
      if (route.request().method() !== "DELETE") return route.fallback();
      writes.held.push(route);
    }
  );
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const label of ["Unfollow", "Unsubscribe"] as const) {
      const where = `Inbox at ${size.width}x${size.height}, ${label}`;
      const issue = await createIssue({
        project: "CORE",
        title: `Who hears the ${label} at ${size.width}`,
      });
      await patchIssue(issue.key, { assignee: "alice" });
      const ask = await createAsk(
        issue.key,
        {
          options: [{ label: "Ship" }, { label: "Hold" }],
          question: `Ship the ${label} at ${size.width}?`,
        },
        { actor: asker, as: "agent" }
      );
      await createComment(
        issue.key,
        { ask_id: ask.id, body: "Tests are green on my side." },
        { actor: replier, as: "agent" }
      );
      await setInterests([
        { session_id: watcher, topics: [`notifications.dispatch.issue.${issue.key}.>`] },
      ]);
      const who = label === "Unfollow" ? replier.id : watcher;
      writes.path =
        label === "Unfollow"
          ? `/api/v1/asks/${ask.id}/followers/${replier.id}`
          : `/api/v1/issues/${issue.key}/subscribers/${watcher}`;
      await page.goto("/");
      await expect(inboxRow(page, ask.id)).toBeVisible();
      const card = await askCard(page, ask.id);
      const inCard = `[data-testid="ask-${ask.id}"]`;
      const recipients = card.getByRole("region", { name: "Recipients" });
      const reaches = recipients.getByRole("button", { name: /^Reaches/ });
      await expect(reaches).toHaveText(/^Reaches 3/);
      await reaches.focus();
      await page.keyboard.press("Enter");
      const chip = recipients
        .getByRole("listitem")
        .filter({ has: page.locator(`[title="${who}"]`) });
      const opener = chip.getByRole("button", { exact: true, name: label });
      await opener.focus();
      await page.keyboard.press("Enter");
      const dialog = page.getByRole("dialog", { name: label });
      await expect(dialog).toBeVisible();
      await dialog.getByRole("button", { exact: true, name: "Confirm" }).focus();
      await page.keyboard.press("Enter");
      // A DELETE carries no key, and the store may send it again when a request got no answer,
      // so the row asks only that Confirm sent it.
      await expect.poll(() => writes.held.length > 0, `${where}: Confirm sent no write`).toBe(true);
      await expect(dialog).toHaveCount(0);
      // The focus fixup of a disabled control runs in a rendering update; two frames hold one.
      await nextFrames(page);
      await nextFrames(page);
      // One read is the answer: the write is held, and its press's renders have all run.
      const holding = await focusPlace(page, inCard);
      if (!holding.inside) {
        wrong.push(
          `${where}: with the write held the focus is on ${holding.name}, not on a control of the card`
        );
      }
      for (const route of writes.held.splice(0)) await route.continue();
      await expect(chip).toHaveCount(0);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

/** The record the row below keeps in the page of the project List's offline Take out of epic:
 *  whether the page has heard that the browser is offline, the animation frames counted from the
 *  press on, every element a `focusin` reached with the frame it reached it in, and the selection
 *  bar's going. */
interface OfflineTakeOut {
  offline: boolean;
  frame: number;
  readonly focusins: { readonly element: Element; readonly frame: number }[];
  /** Once the bar has left the page: the page's clock and the frame then, what held the focus at
   *  the end of the task that removed the bar, and what held it at the next animation frame. */
  going: {
    readonly at: number;
    readonly frame: number;
    readonly atGoing: Element | null;
    atNextFrame: Element | null;
    nextFrameSeen: boolean;
  } | null;
}

declare global {
  interface Window {
    __offlineTakeOut?: OfflineTakeOut;
  }
}

// SLOW(>=2s): an epic of 44 issues seeded, and at two widths two of them taken out of it with the
// browser offline, the 30 s request deadline waited out on the wall clock at each.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-d-h, -d2-h, -d2h2 and
// -d-b, -d2-b at 3fce6150 and da0804df, on the wall clock): the project List, narrowed by its
// Epic filter to one epic of 44 issues, its box scrolled half way; the reader marks two adjacent
// rows in sight with Space, the upper one last, and presses the bar's Take out of epic with
// Enter. Both writes land at Dispatch, and the browser goes offline before the page has their
// answers, so the List's read after them cannot be sent and both rows stay listed. At 3fce6150
// the bar stays, its action held, until the read's bound runs out at the request deadline
// (`REQUEST_DEADLINE_MS`), about 30.3 s after the press, and then goes. At 1440x900 that held in
// 21 of 21 runs: the focus went to the checkbox of the row marked last in Chromium 7 of 7 and in
// Firefox 6 of 7, but to the pager's Rows per page once in Firefox and 5 times of 7 in WebKit,
// and it stayed on the body in WebKit twice. Where it reached the checkbox, it did so 5 to 12 ms
// after the bar went, a frame later, the body holding the focus at that frame, in 12 of 13 runs.
// At 390x664 the bar held to the deadline in 12 of 21 runs, and each time the checkbox took the
// focus in the frame the bar went in. In the other 9 the bar went as the writes landed, 0.28 to
// 0.41 s after the press, with no line saying the List could not be read and both rows still
// listed, and the focus went to the checkbox 5 times and to Rows per page 4 times, each time
// with the window scrolled 22 px. At da0804df the bar went as the writes landed in 24 of 24
// runs: the focus went to the checkbox in Chromium and Firefox 16 of 16 and in WebKit 2 of 8, and
// to Rows per page in WebKit 6 times, at 390 with the window scrolled 22 px. The architect's
// round-24 brief: past the deadline the focus goes to the marked row's checkbox, and the bar goes
// in the same render. The rulings of 2026-11-21 06:0xZ and 09:0xZ: the bar's hand-off goes to a
// row, never to the body, and scrolls nothing; the pager is no row. The row waits the deadline out
// on the wall clock, as the row that Save delivery settings keeps the keyboard focus through a
// save whose re-read the browser cannot send does. The page's own clock fakes
// `requestAnimationFrame`, on which the bar's frame and an armed press's wait of a frame and a
// task both rest (`afterComponents`, components/focus-place.ts), and on it WebKit's landing does
// not show: in builds Job infra-3135-dred-k5lxv at 3fce6150 this row on the page's clock passed
// 3 of 3 in WebKit, and failed in Chromium and Firefox at 1440 on the frame alone. The row counts
// the page's animation frames from the press on, as the measurements above did. Without that
// count it failed in WebKit 1 of 3 (infra-3135-dred2-bnb7t), so WebKit's landing rests on the
// frames the page draws; with it, it failed at 1440 9 of 9 (infra-3135-dred3-k9mrh): WebKit on
// Rows per page, Chromium and Firefox on the frame, the checkbox taking the focus a frame after
// the bar went. Whichever way the bar goes, the row holds the same rule and its message names the
// way. A focused control taken away with its bar, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus row
// (e2e/playwright.config.ts).
test("offline, Take out of epic hands the focus to the marked row's checkbox in the render the bar goes, never the body or the pager @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 44; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  // The Epic filter on that epic: its value is the epic's key and title, as the column shows them.
  const view = `/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees:
   *  inside the window and the table's box, with nothing, such as the sticky head, drawn over its
   *  centre. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        const above = Math.max(0, frame.top);
        const below = Math.min(window.innerHeight, frame.bottom);
        if (own.top < above || own.bottom > below) return false;
        const hit = document.elementFromPoint(own.left + own.width / 2, own.top + own.height / 2);
        return hit !== null && row.contains(hit);
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** Where the window and the table's box stand, where the checkbox of issue `marked` stands on
   *  screen, and what holds the focus: its kind and name, and whether its box is inside the
   *  window and inside the table's box. */
  const landing = (marked: string) =>
    page.evaluate((key) => {
      const frame = document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');
      const mark = document.querySelector(`[data-issue-row="${key}"] input[type="checkbox"]`);
      const markTop = mark === null ? null : Math.round(mark.getBoundingClientRect().top);
      const scrollTop = Math.round(frame?.scrollTop ?? 0);
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { inSight: false, markTop, name: "the body", scrollTop, scrollY };
      }
      const own = focused.getBoundingClientRect();
      const clip = frame?.getBoundingClientRect();
      const top = Math.max(0, clip?.top ?? 0);
      const bottom = Math.min(window.innerHeight, clip?.bottom ?? window.innerHeight);
      const kind =
        focused instanceof HTMLInputElement ? focused.type : focused.tagName.toLowerCase();
      const label =
        focused.getAttribute("aria-label") ||
        (focused as HTMLInputElement).labels?.[0]?.textContent?.trim() ||
        (focused.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
      return {
        inSight:
          own.top >= top - 1 &&
          own.bottom <= bottom + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        markTop,
        name: `${kind} "${label}"`,
        scrollTop,
        scrollY,
      };
    }, marked);
  /** The page's record of the press (`OfflineTakeOut`), each element named as `landing` names
   *  the focus. */
  const barRecord = () =>
    page.evaluate(() => {
      const named = (element: Element | null) => {
        if (element === null || element === document.body) return "the body";
        const kind =
          element instanceof HTMLInputElement ? element.type : element.tagName.toLowerCase();
        const label =
          element.getAttribute("aria-label") ||
          (element as HTMLInputElement).labels?.[0]?.textContent?.trim() ||
          (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
        return `${kind} "${label}"`;
      };
      const kept = window.__offlineTakeOut;
      const going = kept?.going ?? null;
      return {
        atGoing: going === null ? null : named(going.atGoing),
        atNextFrame: going?.nextFrameSeen === true ? named(going.atNextFrame) : null,
        focusins: (kept?.focusins ?? []).map(
          (focusin) => `${named(focusin.element)} at frame ${focusin.frame}`
        ),
        goneAt: going?.at ?? null,
        goneFrame: going?.frame ?? null,
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    const where = `project List at ${size.width}x${size.height}, two rows taken out of the epic offline`;
    await page.setViewportSize(size);
    await page.goto(view);
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(6);
    // The window brings the table's box to just below the app's 56 px header, so its rows are in
    // the window at either width, and the box scrolls half way down its rows.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
    const { order, shown } = await drawn();
    // Two adjacent rows in the middle of what the reader sees.
    const at = order.indexOf(shown[Math.floor(shown.length / 2) - 1] ?? "");
    expect(
      at !== -1 && shown.includes(order[at + 1] ?? ""),
      `${where}: no two adjacent rows in sight mid-list (${shown.join(", ")})`
    ).toBe(true);
    const taken = [order[at] as string, order[at + 1] as string];
    // The upper row is marked last: the row whose checkbox the bar's hand-off is to focus, and
    // which the List still lists while the browser is offline.
    const last = taken[0] as string;
    for (const key of [...taken].reverse()) {
      await markOf(key).focus();
      await page.keyboard.press("Space");
      await expect(markOf(key)).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
    await nextFrames(page);
    const context = page.context();
    // The two writes go to Dispatch as the page sends them, and their answers are held until the
    // browser is offline. A read of the issue list made from the first write on is held with no
    // answer, so the List's read after the action cannot land while the browser is online.
    const written = new RegExp(`^/api/v1/issues/(${taken.join("|")})$`);
    const answers: { response: APIResponse; route: Route }[] = [];
    const heldReads: Route[] = [];
    let writing = false;
    let landed = false;
    await page.route(
      (url) => url.pathname === "/api/v1/issues" || written.test(url.pathname),
      async (route) => {
        const request = route.request();
        if (request.method() === "GET" && new URL(request.url()).pathname === "/api/v1/issues") {
          if (!writing) return route.fallback();
          heldReads.push(route);
          return;
        }
        if (request.method() !== "PATCH") return route.fallback();
        writing = true;
        answers.push({ response: await route.fetch(), route });
        // The action sends its writes together (`WRITES_IN_FLIGHT`, issues-table-model.ts), so
        // the second answer is the last one.
        if (answers.length < taken.length) return;
        await context.setOffline(true);
        // The page's query client hears the browser go offline by the `offline` event, and the
        // record's own listener, added after the app's, hears it after.
        await page.waitForFunction(() => window.__offlineTakeOut?.offline === true);
        for (const answer of answers) await answer.route.fulfill({ response: answer.response });
        landed = true;
      }
    );
    try {
      await page.evaluate(() => {
        const kept: OfflineTakeOut = { focusins: [], frame: 0, going: null, offline: false };
        window.__offlineTakeOut = kept;
        // The frames the page draws, counted by an animation frame callback in each.
        const tick = () => {
          kept.frame += 1;
          requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
        window.addEventListener("offline", () => {
          kept.offline = true;
        });
        document.addEventListener("focusin", (event) => {
          if (event.target instanceof Element) {
            kept.focusins.push({ element: event.target, frame: kept.frame });
          }
        });
        const watcher = new MutationObserver(() => {
          if (document.querySelector('[role="toolbar"][aria-label="Selected issues"]') !== null)
            return;
          watcher.disconnect();
          const going = {
            at: Date.now(),
            atGoing: document.activeElement,
            atNextFrame: null as Element | null,
            frame: kept.frame,
            nextFrameSeen: false,
          };
          kept.going = going;
          // The next frame's callbacks run before it paints: what holds the focus then is what
          // that frame draws focused.
          requestAnimationFrame(() => {
            going.atNextFrame = document.activeElement;
            going.nextFrameSeen = true;
          });
        });
        watcher.observe(document.body, { childList: true, subtree: true });
      });
      // One instant is the point: where the table, the window and the marked row stand just
      // before the press.
      const before = await landing(last);
      // The press's time on the page's clock, against which the bar's going is timed.
      const pressed = await page.evaluate(() => Date.now());
      await page.keyboard.press("Enter");
      await expect
        .poll(() => landed, { message: `${where}: the two writes never both landed` })
        .toBe(true);
      // The row's claim is a span of time: the bar goes at the request deadline, 30 s after the
      // press, or sooner where the List's read settles first, and the row waits for its going on
      // the wall clock with room to spare. A bar that never goes is the record's to say, below.
      await expect(bar)
        .toHaveCount(0, { timeout: REQUEST_DEADLINE_MS + 15_000 })
        .catch(() => undefined);
      // The bar's hand-off runs after the commit that took the bar away, so it is done after
      // these frames.
      await nextFrames(page);
      await nextFrames(page);
      // The record's next frame after the bar went is what that frame drew focused; wait for
      // that frame to be noted, or for nothing where the bar never went.
      await page.waitForFunction(() => {
        const going = window.__offlineTakeOut?.going;
        return going == null || going.nextFrameSeen;
      });
      // One read once every hand-off has run and the frame after the bar's going was drawn:
      // nothing else moves the focus or scrolls the page while the browser stays offline.
      const log = await barRecord();
      // One read of where the focus rests, taken after the record and in the same still page.
      const now = await landing(last);
      const expected = `checkbox "Select ${last}"`;
      const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}, table scrollTop ${before.scrollTop} -> ${now.scrollTop}, the checkbox of ${last} at ${before.markTop} -> ${now.markTop} px`;
      const way =
        log.goneAt === null
          ? "never"
          : log.goneAt - pressed < REQUEST_DEADLINE_MS
            ? `as its writes landed, ${log.goneAt - pressed} ms after the press, its read settled with the browser offline`
            : `at the request deadline, ${log.goneAt - pressed} ms after the press`;
      const moves = `the focus after the press: ${log.focusins.join(" -> ") || "nowhere"}`;
      if (log.goneAt === null) {
        wrong.push(
          `${where}: the selection bar never went; the focus rests on ${now.name} (${moves})`
        );
      } else if (log.atGoing !== expected && log.atNextFrame !== expected) {
        wrong.push(
          `${where}: the bar went ${way}, in frame ${log.goneFrame}, with the focus on ${log.atGoing}, and the next frame drew it on ${log.atNextFrame ?? "no frame seen"}, not on ${expected} (${moves})`
        );
      }
      if (now.name !== expected) {
        wrong.push(
          `${where}: once the bar went ${way}, the focus rests on ${now.name}, not on ${expected} (${moves}; ${numbers})`
        );
      } else if (!now.inSight) {
        wrong.push(
          `${where}: once the bar went ${way}, the focus rests on ${expected}, out of sight (${numbers})`
        );
      }
      if (now.scrollY !== before.scrollY) {
        wrong.push(`${where}: once the bar went ${way}, the window scrolled (${numbers})`);
      }
      // The table's box may rise by the height of the line that says the List could not be read,
      // drawn above it, only while the marked row keeps its place on screen.
      const markStood =
        now.markTop !== null &&
        before.markTop !== null &&
        Math.abs(now.markTop - before.markTop) <= 1;
      if (now.scrollTop !== before.scrollTop && !markStood) {
        wrong.push(`${where}: once the bar went ${way}, the table's box scrolled (${numbers})`);
      }
    } finally {
      await context.setOffline(false);
      for (const route of heldReads) {
        await route.abort("internetdisconnected").catch(() => undefined);
      }
      await page.unrouteAll({ behavior: "ignoreErrors" });
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): one issue's Conversation at two widths, its decided ask changed four times.
// INFRA-3135, round 14 (the tester's, measured in builds Job infra-3135-r14flake at aa057a19):
// Change answer on a decided ask takes itself away and puts the answer form in its place, as
// Add a priority's + does. At aa057a19 a click on it scrolled the page 65 px at 1440 in Firefox
// and WebKit. Restored by the tester in round 25, by the architect's ruling of 2026-11-24
// 08:07Z: by a press of Enter and by a click, at 1440 and 390, the form opens and its first
// choice, Ship, stands whole in the window. In builds Job infra-3135-r-cah-ml77m at 683f1fdf it
// did in Chromium, Firefox and WebKit, Ship at 650 px in the 900 px window at 1440 and at 388 px
// in the 664 px one at 390, and the press scrolled nothing. Where the focus goes once the button
// has gone is not read: the round-14 row's check that it lands in the form read a landing the
// page's focus rescue made, and it left with the rescue (dispatch://INFRA-5564). A focused
// control taken away, for which Firefox and WebKit fire no `focusout`: the clause of the
// `webkit` and `firefox` projects that names the page-own focus row (e2e/playwright.config.ts).
test("Change answer opens the answer form with its first choice whole in the window, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Release train" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    SURFACE_SESSION
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const press of ["Enter", "click"] as const) {
      const at = `${size.width}x${size.height}, by ${press}`;
      await page.goto(`/issues/${issue.key}/conversation`);
      const turn = page.locator(`[data-turn="ask:${ask.id}"]`);
      await expect(turn).toContainText("Answered by alice");
      const change = turn.getByRole("button", { name: "Change answer" });
      await change.scrollIntoViewIfNeeded();
      if (press === "Enter") {
        await change.focus();
        await page.keyboard.press("Enter");
      } else {
        await change.click();
      }
      const form = turn.locator("form").filter({ has: page.getByRole("radio", { name: "Ship" }) });
      await expect(form, `${at}: Change answer opened no answer form`).toBeVisible();
      const ship = form.getByRole("radio", { name: "Ship" });
      const whole = await expect
        .poll(() =>
          ship.evaluate((node) => {
            const box = node.getBoundingClientRect();
            return (
              box.top >= 0 &&
              box.bottom <= window.innerHeight &&
              box.left >= 0 &&
              box.right <= window.innerWidth
            );
          })
        )
        .toBe(true)
        .then(
          () => true,
          () => false
        );
      if (!whole) wrong.push(`${at}: the form's first choice, Ship, is not whole in the window`);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): an issue page opened at each of two widths, and its Status list walked by the arrows
// through its eight open statuses at each.
// INFRA-3135, round 25 (the tester's, measured in builds Jobs infra-3135-p-hc-fhjbh, -p-hf-x8mfh
// and -p-hw-m2tvp at 683f1fdf and -p-bc-cdxbx, -p-bf-45fn7 and -p-bw-ptvwn at 3fce6150, and
// this row in infra-3135-p-red2-kvtkk at 683f1fdf): the reader opens the issue header's Status
// from the keyboard, over the issue's Artifacts tab, and walks its statuses with ArrowDown. The
// list holds the eight open statuses in a box 286 px tall whose text box stands stuck to its top,
// so it shows five of them; at the sixth, Testing, the option the list marks active (its text
// box's `aria-activedescendant`) stands below what the box shows, and so do Needs review and
// Retro, in Chromium, Firefox and WebKit at 1440x900 and 390x664. Enter there picks an option the
// reader cannot see (components/MultiSelect.tsx:436-442); a pointer reaches the three by
// scrolling the box. At 3fce6150 the Status was a native select, whose popup the browser draws
// and scrolls. The cause: the shared list's ArrowDown and ArrowUp move its active row and scroll
// nothing (components/MultiSelect.tsx:424-435), in a panel capped at `max-h-72` (:548), and
// 20f534c5 made the Status that list (features/issue/IssueHeader.tsx:538-563). The rule: a list
// the reader walks with the arrows shows the option it marks active, as the select it replaced
// did (the round's item 6: every option of an open list can be reached).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
test("the issue header's Status list keeps the option its arrows mark active wholly inside the list's box, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with an issue page opened and its eight statuses walked.
  test.setTimeout(90_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Statuses in sight" });
  // The statuses an open issue's Status offers, in the list's order: every one but done.
  const statuses = ISSUE_STATUSES.filter((status) => status !== "done");
  /** Where the option the open list marks active (the text box's `aria-activedescendant`) stands
   *  in the list's scroll box, in that box's content: the option's top and bottom, and the part
   *  of the content the box shows - below what it draws stuck to its top (the text box), above
   *  what it draws stuck to its bottom, and cut to the window - with the box's scrollTop and
   *  height. `inside` is whether the option stands wholly in that part. */
  const activeOptionSight = () =>
    page.evaluate(() => {
      const id = document.activeElement?.getAttribute("aria-activedescendant") ?? "";
      const option = id === "" ? null : document.getElementById(id);
      const none = {
        bottom: 0,
        height: 0,
        inside: false,
        name: "no option",
        scrollTop: 0,
        shownBottom: 0,
        shownTop: 0,
        top: 0,
      };
      if (option === null) return none;
      const name = `"${(option.textContent ?? "").replace("✓", "").replace(/\s+/g, " ").trim()}"`;
      let found = option.parentElement;
      while (found !== null && !/(auto|scroll)/.test(getComputedStyle(found).overflowY)) {
        found = found.parentElement;
      }
      if (found === null) return { ...none, name };
      const box = found;
      const boxTop = box.getBoundingClientRect().top + box.clientTop;
      let shownTop = Math.max(boxTop, 0);
      let shownBottom = Math.min(boxTop + box.clientHeight, window.innerHeight);
      for (const child of box.children) {
        if (getComputedStyle(child).position !== "sticky") continue;
        const stuck = child.getBoundingClientRect();
        if (stuck.top <= boxTop + 1) shownTop = Math.max(shownTop, stuck.bottom);
        else if (stuck.bottom >= boxTop + box.clientHeight - 1) {
          shownBottom = Math.min(shownBottom, stuck.top);
        }
      }
      const own = option.getBoundingClientRect();
      const content = (y: number) => Math.round(y - boxTop + box.scrollTop);
      return {
        bottom: content(own.bottom),
        height: box.clientHeight,
        inside: own.top >= shownTop - 1 && own.bottom <= shownBottom + 1,
        name,
        scrollTop: Math.round(box.scrollTop),
        shownBottom: content(shownBottom),
        shownTop: content(shownTop),
        top: content(own.top),
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `the issue header's Status at ${size.width}x${size.height}`;
    await page.goto(`/issues/${issue.key}/artifacts`);
    const trigger = listTrigger(page.getByTestId("issue-header"), "Status");
    await expect(trigger).toBeVisible();
    await trigger.focus();
    await page.keyboard.press("Enter");
    const panel = listOf(trigger);
    await expect(panel.getByRole("combobox", { name: "Search statuses" })).toBeFocused();
    await expect(panel.getByRole("option")).toHaveCount(statuses.length);
    // The list opens on the status the issue holds, Triage, the first, so the arrows mark every
    // status active once in as many steps as there are statuses.
    for (let step = 0; step < statuses.length; step += 1) {
      if (step > 0) {
        await page.keyboard.press("ArrowDown");
        await nextFrames(page);
      }
      // One read is the answer: the key's render has run, and nothing else scrolls the box.
      const sight = await activeOptionSight();
      if (!sight.inside) {
        wrong.push(
          `${where}: after ${step} ArrowDown the list marks ${sight.name} active at ${sight.top}-${sight.bottom} px of its content, and its box shows ${sight.shownTop}-${sight.shownBottom} px of it (scrollTop ${sight.scrollTop}, height ${sight.height}): the active option is out of sight`
        );
        break;
      }
    }
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): eight questions seeded at each of two widths, and two snoozes of two marked rows
// held through the reader's presses at each.
// INFRA-3135, round 25 (the tester's, measured in builds Jobs infra-3135-p-hc-fhjbh, -p-hf-x8mfh
// and -p-hw-m2tvp at 683f1fdf and -p-bc-cdxbx, -p-bf-45fn7 and -p-bw-ptvwn at 3fce6150, isolated
// in -p3-h-cj42g at 683f1fdf and -p3-b-zzd9d at 3fce6150, and this row in
// infra-3135-p-red2-kvtkk at 683f1fdf): the reader marks Inbox rows, picks Tomorrow from the
// selection bar's Snooze selected asks with the keyboard, and presses Enter again on the trigger
// while the snooze is out. The trigger is held (`aria-disabled`), keeps the focus, opens nothing
// and sends no second snooze. Once the snooze lands and the bar goes, the focus is on the body,
// in Chromium, Firefox and WebKit at 1440x900 and 390x664. The same pick with no second Enter
// puts it on a row's checkbox at 1440 and on the Inbox list's own stop at 390, as the bar's
// hand-off says. At 3fce6150 the page's focus rescue put it on Answered by you, a row's checkbox
// or the Inbox list. The cause: the pick's Enter arms the bar's hand-off on the list's text box
// (the shared list picks at that box's keydown, components/MultiSelect.tsx:436-443, and puts the
// focus on its trigger, :312-318), which the pick takes away. A second Enter on the held trigger
// is a press on another control: armHandOff keeps the first press only for a press on its
// control or inside a control that press holds (components/focus-place.ts:782-795), read by
// `heldControl`, which answers null for an element gone from the page (:664-666). So it ends the
// pick's press and arms one on the trigger, held already at the arm, which nothing keeps live
// (:900-905), and when the snooze lands the bar's hand-off (components/SelectionBar.tsx:371-380)
// finds nothing armed. The rules: a repeated Enter on a held control keeps the first press as the
// cause (components/focus-place.ts:744-747), and a shared control whose own press takes the
// focused control away and drops the focus onto the body, where its code arms a hand-off, fails
// (the architect's ruling of 2026-11-23 21:2xZ, open-items.md, "#3729 sheds the rescue").
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
test("a second Enter on the held Snooze selected asks leaves the focus where the pick alone puts it once the snooze lands and the bar goes, never on the body @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with eight questions seeded and two snoozes held.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  // Every snooze sent while `holding` is held until the row lets it through, so the page is read
  // while the bar's write is out; the row lets each through once the reader's presses are done.
  let holding = false;
  const held: Route[] = [];
  await page.route("**/api/v1/me/asks/*/snooze", async (route) => {
    if (route.request().method() !== "PUT" || !holding) return route.fallback();
    held.push(route);
  });
  /** Where the keyboard focus rests once the selection bar has gone: the body, or the focused
   *  element and its place among the rows a pick seeded (`slots`, by ask id), as "the checkbox
   *  of the row below the marked two". An element in no seeded row - the Inbox list's own stop -
   *  is its own place. */
  const landingOf = (slots: Record<string, string>) =>
    page.evaluate((slots) => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { body: true, name: "the body", place: "the body" };
      }
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      const name = `${focused.tagName.toLowerCase()} "${words}"`;
      const row = focused.closest<HTMLElement>("[data-inbox-row]")?.dataset.inboxRow;
      if (row === undefined) return { body: false, name, place: name };
      const what = focused.matches('input[type="checkbox"]')
        ? "the checkbox"
        : `a ${focused.tagName.toLowerCase()}`;
      return { body: false, name, place: `${what} of ${slots[row] ?? "a row no pick seeded"}` };
    }, slots);
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const landings: Partial<
      Record<"no second Enter" | "a second Enter", { body: boolean; name: string; place: string }>
    > = {};
    for (const press of ["no second Enter", "a second Enter"] as const) {
      const where = `Snooze selected asks at ${size.width}x${size.height} with ${press}`;
      // Four questions made oldest first, so the Inbox lists them newest first, over the rows an
      // earlier pick left: one above the two the reader marks, and one below them. Their titles
      // share most of their words, so each is filed past the server's duplicate check.
      const ids: string[] = [];
      for (const part of ["below", "second marked", "first marked", "above"]) {
        const owner = await createIssue({
          force: true,
          project: "CORE",
          title: `Snooze the ${part} row at ${size.width} with ${press}`,
        });
        const question = `Defer the ${part} row at ${size.width} with ${press}?`;
        ids.push((await createAsk(owner.key, { question }, SURFACE_SESSION)).id);
      }
      const [below = "", second = "", first = "", above = ""] = ids;
      const slots = {
        [above]: "the row above the marked two",
        [first]: "the first marked row",
        [second]: "the second marked row",
        [below]: "the row below the marked two",
      };
      await page.goto("/");
      for (const id of ids) await expect(inboxRow(page, id)).toBeVisible();
      await inboxRow(page, first).getByRole("checkbox").first().check();
      await inboxRow(page, second).getByRole("checkbox").first().check();
      const bar = page.getByRole("toolbar", { name: "Selected rows" });
      const trigger = listTrigger(bar, "Snooze selected asks");
      await trigger.focus();
      holding = true;
      await page.keyboard.press("Enter");
      await page.keyboard.type("tom");
      await expect(listOf(trigger).getByRole("option")).toHaveText(["Tomorrow"]);
      await page.keyboard.press("Enter");
      await expect
        .poll(() => held.length, { message: `${where}: the bar sent no snooze` })
        .toBeGreaterThan(0);
      await expect(trigger, `${where}: the trigger is not held`).toHaveAttribute(
        "aria-disabled",
        "true"
      );
      await expect(trigger, `${where}: the held trigger lost the focus`).toBeFocused();
      if (press === "a second Enter") {
        await page.keyboard.press("Enter");
        await nextFrames(page);
        await expect(trigger, `${where}: the held trigger opened its list`).toHaveAttribute(
          "aria-expanded",
          "false"
        );
      }
      holding = false;
      for (const route of held.splice(0)) await route.continue();
      await expect(bar).toHaveCount(0);
      await expect(inboxRow(page, first)).toHaveCount(0);
      await expect(inboxRow(page, second)).toHaveCount(0);
      // The bar's hand-off runs after the commit that took the bar away, so it is done after
      // these frames.
      await nextFrames(page);
      await nextFrames(page);
      // One read is the answer: the snooze has landed, the bar has gone and its hand-off has run.
      const landed = await landingOf(slots);
      landings[press] = landed;
      if (landed.body && press === "no second Enter") {
        wrong.push(`${where}: once the snooze lands and the bar goes the focus is on the body`);
      }
    }
    const alone = landings["no second Enter"];
    const again = landings["a second Enter"];
    if (alone !== undefined && again !== undefined && again.place !== alone.place) {
      wrong.push(
        `Snooze selected asks at ${size.width}x${size.height}: with a second Enter on the held trigger, once the snooze lands and the bar goes the focus is on ${again.body ? "the body" : `${again.name} (${again.place})`}; the same pick with no second Enter puts it on ${alone.name} (${alone.place})`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the Delivery page opened three times in a fixture, with its live socket held, closed and open.
// INFRA-3135, round 25 (the tester's), by the architect's ruling of 2026-11-24 08:07Z: a count
// beside a value is drawn only once the data it counts has answered, never as a 0 that stands for
// "not read yet". Measured in builds Jobs infra-3135-dl-h2-pbn4c (head 683f1fdf) and
// infra-3135-dl-m2-hdf8z (main 642bc1ed), in Chromium, Firefox and WebKit at 1440x900: with the
// live socket held (nothing answers the timeline's subscribe) or closed by the network, the
// head's Filter panel offers Rework and Deployed, and every verdict reads "0 PRs"
// (DeliveryPage.tsx:415 `counts[value] ?? 0` over `data?.facet_counts`, :495, where `data` is
// undefined until the first answer). Main drew its FacetPanel only once the timeline answered.
// With the socket open the same fixture reads value 1, rework 1, deployed 1, waiting 1 and
// not tracked 0, which this row reads too, so it can tell a count from a placeholder.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
test("the Delivery page's Filter panel draws no count beside a verdict before the timeline answers, nor after its socket fails @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedDeliveryFixture();
  const main = page.getByTestId("main-content");
  /** The accessible name of each option of one facet of the open Filter panel, read once its
   *  list holds every verdict, so the read is of a list that is whole. */
  const verdicts = async (facet: "Rework" | "Deployed", count: number): Promise<string[]> => {
    const panel = await openFilterPanel(page, main);
    await panel.getByRole("button", { name: new RegExp(`^${facet}: `) }).click();
    const options = page.getByRole("listbox", { name: `${facet} options` }).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("aria-label") ?? element.textContent ?? "")
    );
    await page.keyboard.press("Escape");
    await closeControlPanel(main);
    return names;
  };
  const wrong: string[] = [];
  // One route for the page's live socket, whose mode each load sets: open passes it through to
  // the server, held opens it and answers nothing, closed drops it as a network would.
  let mode: "open" | "held" | "closed" = "open";
  await page.routeWebSocket(/\/ws\/live$/u, (socket) => {
    if (mode === "open") socket.connectToServer();
    if (mode === "closed") socket.close({ code: 1011, reason: "test network drop" });
  });
  // The control: the page with its socket open reads the fixture's own counts.
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(main.getByRole("img", { name: /^Delivery timeline/ })).toBeVisible();
  expect(await verdicts("Rework", 2)).toEqual(["value, 1 PR", "rework, 1 PR"]);
  for (const how of ["held", "closed"] as const) {
    mode = how;
    await page.goto(`/delivery?${DELIVERY_WINDOW}`);
    for (const [facet, count] of [
      ["Rework", 2],
      ["Deployed", 3],
    ] as const) {
      const counted = (await verdicts(facet, count)).filter((name) => /\d+ PRs?$/u.test(name));
      wrong.push(
        ...counted.map(
          (name) => `socket ${how}: ${facet} offers "${name}" before the timeline answered`
        )
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the Inbox and its Dismissed view opened six times over two asks and four threads, with the one read held, refused and answered.
// INFRA-3135, round 25 (the tester's), by the architect's ruling of 2026-11-24 08:07Z: a count
// beside a value is drawn only once the data it counts has answered, never as a 0 that stands for
// "not read yet" (B2 of dispatch://INFRA-3135/artifact/t23-class-search-md, moved into this pull
// request). Measured in builds Job infra-3135-b2-h-lrxq6 (head 683f1fdf), in Chromium, Firefox and
// WebKit at 1440x900: with the reader's thread read (/api/v1/me/replies?state=inbox) held or
// answered 500, the Filter panel's Type list reads "Reply, 0 rows" and "Agent message, 0 rows"
// while two reply threads wait, because the counts are taken over the rows read so far
// (Inbox.tsx:2051-2052, `counts.get(value) ?? 0`). With the read answered it reads "Reply, 2 rows",
// which this row reads too, so it can tell a count from a placeholder.
// On this branch the Inbox has no thread read of its own: the one read, `GET /api/v1/me/inbox`,
// answers the reader's own asks and their threads together (`useInboxAsks`, `useInboxThreadRows`),
// its `inbox` state for the Inbox and its `dismissed` state for the Dismissed view's threads, while
// the bar stands on the `inbox` state alone. So the row holds and refuses each state of the one
// read in the view it lists. In the Inbox that state holds the asks too, so no kind may carry a
// count while it is out or refused. The Dismissed view lists no ask, and the asks' read, the
// `inbox` state, answered before its bar was drawn, so there each ask kind keeps its count, 0,
// as INFRA-3135's rule keeps the questions' count, and only the thread kinds carry none. With the
// read answered the Inbox reads "Question, 2 rows" and "Threads, 2 rows" (this branch's name for
// the reply threads' kind), and the Dismissed view "Threads, 2 rows".
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
test("the Inbox's Type list draws no count for a kind while the one read its rows come from is out or refused, in the Inbox and in its Dismissed view, whose ask kinds keep their count @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 2; index++) {
    const issue = await createIssue({ project: "CORE", title: `Ask item ${index}` });
    await createAsk(
      issue.key,
      { question: `Item ${index}: what is the rule?`, urgency: "med" },
      SURFACE_SESSION
    );
  }
  for (const title of ["Nightly mirror", "Weekly digest"]) {
    const issue = await createIssue({ project: "CORE", title });
    await seedCommentThread(issue.key, `${title}: when does it run?`, "It moved to 03:00.");
  }
  const done = await createIssue({ project: "CORE", title: "Dealt-with threads" });
  await seedDismissedThreads({ count: 2, issueKey: done.key, prefix: "Dealt with" });
  const main = page.getByTestId("main-content");
  const rows = main.locator("[data-inbox-row]");
  const trigger = control(controlRow(main), CONTROL.panelTrigger);
  /** Each option of the Type list, by its accessible name, read once the list holds all five:
   *  its `aria-label` where it has one, else its text, as a row with no count beside it is named
   *  by its label alone. None where the bar draws no Filter, which then offers no count either. */
  const typeOptions = async (): Promise<string[]> => {
    // Every caller waited for the list's loading line, its error line, the bar or the rows, and the
    // Inbox decides whether its bar stands in the commit that draws them, so this read is settled.
    const bars = await trigger.count();
    if (bars === 0) return [];
    await openFilterPanel(page, main);
    await page.getByRole("button", { name: "Filter by type" }).click();
    const options = page.getByRole("listbox", { name: "Type options" }).getByRole("option");
    await expect(options).toHaveCount(5);
    const names = await options.evaluateAll((elements) =>
      elements.map(
        (element) => element.getAttribute("aria-label") ?? element.textContent?.trim() ?? ""
      )
    );
    await page.keyboard.press("Escape");
    await closeControlPanel(main);
    return names;
  };
  const views = [
    { address: "/", name: "the Inbox", state: "inbox" },
    { address: "/?show=dismissed", name: "the Dismissed view", state: "dismissed" },
  ] as const;
  /** The Type list of the Dismissed view while its threads' read is out or refused, quoted as a
   *  failure names it: each ask kind with its count, none, and each thread kind with no count. */
  const dismissedUnanswered = [
    "Question, 0 rows",
    "Decision, 0 rows",
    "Approval, 0 rows",
    "Threads",
    "Agent message",
  ]
    .map((name) => `"${name}"`)
    .join(", ");
  const wrong: string[] = [];
  try {
    for (const view of views) {
      const oneRead = (url: URL) =>
        url.pathname === "/api/v1/me/inbox" && url.searchParams.get("state") === view.state;
      for (const how of ["held", "refused"] as const) {
        const where = `${view.name} with the one read's ${view.state} state ${how}`;
        await page.route(oneRead, async (route) => {
          // Held: the read is never answered. Refused: the server answers it 500 every time.
          if (how === "refused") {
            await route.fulfill({
              body: '{"error":{"code":"TEST_REFUSED","message":"test refusal"}}',
              contentType: "application/json",
              status: 500,
            });
          }
        });
        await page.goto(view.address);
        // The list says it is loading, or that it could not load, and lists no row of the read.
        if (view.state === "inbox") {
          await expect(
            how === "held"
              ? main.getByRole("status", { name: "Loading your inbox" })
              : main.getByText("Could not load your inbox.")
          ).toBeVisible({ timeout: 30_000 });
        } else {
          await expect(trigger, `${where}: the bar draws no Filter`).toBeVisible();
        }
        await expect(rows, `${where}: the list draws rows`).toHaveCount(0);
        const names = await typeOptions();
        if (view.state === "dismissed") {
          const offered = names.map((name) => `"${name}"`).join(", ");
          if (offered !== dismissedUnanswered) {
            wrong.push(`${where}: the Type list offers ${offered}, not ${dismissedUnanswered}`);
          }
        } else {
          wrong.push(
            ...names
              .filter((name) => /, \d+ rows?$/u.test(name))
              .map((name) => `${where}: the Type list offers "${name}"`)
          );
        }
        await page.unrouteAll({ behavior: "ignoreErrors" });
      }
    }
    // The control: the same two views with the one read answered count their rows.
    await page.goto("/");
    await expect(rows).toHaveCount(4);
    const inInbox = await typeOptions();
    expect(inInbox, "the Inbox with the one read answered").toContain("Question, 2 rows");
    expect(inInbox, "the Inbox with the one read answered").toContain("Threads, 2 rows");
    await page.goto("/?show=dismissed");
    await expect(rows).toHaveCount(2);
    expect(await typeOptions(), "the Dismissed view with the one read answered").toContain(
      "Threads, 2 rows"
    );
  } finally {
    await page.unrouteAll({ behavior: "ignoreErrors" });
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135, round 25 (the tester's): the panel `+N more` opens lists the chips the line has no
// room for in a box at most 20rem tall that scrolls what it cannot hold (`MORE_PANEL_CLASS`,
// web/src/components/ControlRow.tsx:36, over `PANEL_FRAME`, web/src/components/PanelTrigger.tsx:12-13,
// drawn at ControlRow.tsx:524-552). At 390x664 these eleven narrowings put ten chips in it, 528 px
// of chips in a box 318 px tall, so the Tab key walks onto chips that start out of sight. Chromium
// and WebKit scroll each chip the focus lands on wholly into the box. Firefox scrolls the panel to
// 183 of the 210 px it can go and leaves the last chip, Waiting on you, 19 of its 44 px below the
// box's bottom, the same 600 ms later (builds Jobs infra-3135-c2-h-wp7ns and infra-3135-c3-h-rjqps
// at 683f1fdf). A keyboard reader then sees part of the chip Enter takes off, which round 13
// failed. The row above that walks the same panel by Tab, "a chip of the Inbox's row that has the
// keyboard focus is shown whole", runs in Chromium alone and reads a chip's clip sideways alone
// (`shownOf`), so a chip cut at the panel's bottom passes it.
// R8 (INFRA-4948) moves that panel at 390: below 640 px of the bar's width the chips leave the
// bar's line and no `+N more` stands for them. The Filter panel lists every narrowing that is on
// at its top, above its fields, each with its remove control (`PanelChips`,
// web/src/components/ControlRow.tsx), in the same frame (`PANEL_FRAME`), and that is where a
// keyboard reader at 390 takes one off. So the row walks that list: the ten chips these
// narrowings draw (the switch's Blocked on you draws none) over the panel's five fields, more than
// its box holds.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): twenty-two seeded questions, ten narrowings, and the Filter panel walked chip by chip.
test("each chip the Tab key reaches in the Inbox's Filter panel stands wholly inside the panel's visible box while the panel scrolls, at 390 @firefox @webkit", async ({
  page,
}) => {
  await seedLongInbox();
  await page.setViewportSize({ height: 664, width: 390 });
  const row = controlRow(page.getByTestId("main-content"));
  const trigger = control(row, CONTROL.panelTrigger);
  // Three priorities, three urgencies, two kinds, the one project and everyone's asks, a chip
  // each, over the seventeen questions they keep in the needs-you section; neither the text nor
  // the switch's choice draws one.
  await page.goto(
    "/?section=needs-you&q=Item&assignee=everyone&priority=P0,P1,P2&urgency=blocking,high,med&type=question,approval&project=CORE"
  );
  expect(
    await chipsLeaveLine(row, "390x664"),
    "390x664: the Inbox's bar is 640 px wide or wider"
  ).toBe(true);
  await expect(
    trigger,
    "390x664: the trigger does not count the ten narrowings"
  ).toHaveAccessibleName("Filter · 10");
  // Opened from the keyboard, the panel takes the focus onto its first control, its first chip.
  await trigger.focus();
  await page.keyboard.press("Enter");
  const panel = page.getByRole("dialog", { name: "Filter" });
  await expect(
    panel.getByRole("button", { name: REMOVE_CHIP }),
    "390x664: the Filter panel does not list every narrowing"
  ).toHaveCount(10);
  // The count of listed chips was waited for above, so the names read here are the whole list.
  const listed = await chipNames(panel);
  const standing: {
    name: string;
    above: number;
    below: number;
    left: number;
    right: number;
    chip: string;
    box: string;
    scrolled: number;
  }[] = [];
  for (const [index, name] of listed.entries()) {
    const chip = panel.getByRole("button", { exact: true, name });
    // The panel opens with the focus on its first chip, and each Tab moves it to the next.
    if (index > 0) await page.keyboard.press("Tab");
    await expect(
      chip,
      `390x664: the Tab key did not bring the focus to "${name}" in the Filter panel`
    ).toBeFocused();
    // The browser scrolls the panel to the chip as the focus moves onto it, so the boxes read
    // once the chip holds the focus are where they settle: Firefox's stood the same 600 ms later.
    // The visible box is the panel's padding box, cut by the window.
    const reading = await chip.evaluate((node) => {
      const holder = node.closest('[role="dialog"]') ?? document.documentElement;
      const frame = holder.getBoundingClientRect();
      const top = Math.max(0, frame.top + holder.clientTop);
      const left = Math.max(0, frame.left + holder.clientLeft);
      const bottom = Math.min(innerHeight, frame.top + holder.clientTop + holder.clientHeight);
      const right = Math.min(innerWidth, frame.left + holder.clientLeft + holder.clientWidth);
      const own = node.getBoundingClientRect();
      return {
        above: Math.max(0, top - own.top),
        below: Math.max(0, own.bottom - bottom),
        left: Math.max(0, left - own.left),
        right: Math.max(0, own.right - right),
        chip: `${Math.round(own.top)}-${Math.round(own.bottom)} px down, ${Math.round(own.left)}-${Math.round(own.right)} px across`,
        box: `${Math.round(top)}-${Math.round(bottom)} px down, ${Math.round(left)}-${Math.round(right)} px across`,
        scrolled: Math.round(holder.scrollTop),
      };
    });
    standing.push({ name, ...reading });
  }
  await test.info().attach("the Filter panel's chips walked by the Tab key at 390x664", {
    body: standing
      .map(
        ({ name, chip, box, scrolled }) => `${name}: ${chip}, box ${box}, scrolled ${scrolled} px`
      )
      .join("\n"),
    contentType: "text/plain",
  });
  // The row is about chips that start out of sight, so a panel that never scrolled read none.
  expect(
    Math.max(...standing.map(({ scrolled }) => scrolled)),
    `390x664: the Filter panel never scrolled while the Tab key walked ${listed.join(", ")}`
  ).toBeGreaterThan(0);
  // A pixel of rounding either way is no cut.
  const cut = standing.flatMap(({ name, above, below, left, right, chip, box, scrolled }) => {
    const sides = [
      [above, "above"],
      [below, "below"],
      [left, "left of"],
      [right, "right of"],
    ] as const;
    const out = sides
      .filter(([pixels]) => pixels > 1)
      .map(([pixels, side]) => `${Math.round(pixels)}px ${side}`);
    return out.length === 0
      ? []
      : [
          `390x664: "${name}" holds the focus with ${out.join(" and ")} the panel's visible box (the chip at ${chip}, the box at ${box}, the panel scrolled ${scrolled} px)`,
        ];
  });
  // R1, R8 and the keyboard's own promise in ControlRow.tsx: below 640 px every chip is one the
  // Filter panel lists at its top and the Tab key reaches whole, so a keyboard reader sees which
  // narrowing Enter takes off.
  expect(cut, cut.join("; ")).toEqual([]);
});

// SLOW(>=2s): at two widths, a project List's Move refused for two open issues and a Move with
// only closed issues marked, each followed by another person's live update that empties the bar.
// INFRA-3135, round 25 (the tester's, from t23's B8, which the architect ruled into this round on
// 2026-11-24 03:2xZ: a hold that only says there is nothing to do keeps no press live). Measured in
// builds Jobs infra-3135-h-h2-thk26 at 683f1fdf, infra-3135-h-b2-gjpk7 at 3fce6150 and
// infra-3135-h-p8-4nhkv at ae48d8a1, the commit before a5464d6e. The reader marks two issues on
// the project List, picks an epic in Move to epic and presses Move by Enter; Move's own press
// leaves it held with no epic staged (`ariaDisabled={busy || epic === ""}`,
// features/project/issues-table-page.tsx:394-397). Then bob closes or reopens the marked issues,
// and his live update takes them off the view and empties the selection bar while Move holds the
// focus. That going is not the reader's, so the bar hands the focus nowhere (SelectionBar.tsx,
// "Nor does a going the reader did not cause ... leaves the focus on the body").
// - The Move refused (400) for two open issues: at 683f1fdf the focus stays on the body in
//   Chromium, Firefox and WebKit at 1440 and 390; at ae48d8a1 it lands on a row's checkbox
//   (`Select MVA-4`), which a5464d6e fixed.
// - The Move with only closed issues marked, which the List answers "Only open issues can change
//   epic." without a write: at 683f1fdf, 3fce6150 and ae48d8a1 the bar's hand-off lands the focus
//   on `TABLE:Project issues table`, the list's own stop, in all three engines at both widths.
//   `moveTo` returns at that answer before any action begins a hold on what answers the press
//   (features/project/issues-table-actions.ts:284-289), so nothing marks the press answered, and
//   `writeOut` (components/focus-place.ts:882-883) keeps counting Move's own hold, begun after the
//   arm, up to the 30 s write deadline: the press stays live, and the bar reads its going as the
//   reader's (`causedByReader`).
// The refused Move is the control: the row asserts that the closed-issues path ends where the
// refused path ends, so it pins no landing of its own.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
test("a Move with only closed issues marked keeps no press live: a live update that empties the List's bar then lands the focus where it lands after a refused Move, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  const sizes = [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ] as const;
  /** One project per width and path: an epic and six tasks, the first two closed on the closed
   *  path. Each task's title is its own words, since the server refuses a near copy of a title it
   *  holds (409 POSSIBLE_DUPLICATE). */
  const seeds: { epic: string; key: string; marked: [string, string] }[] = [];
  const words = ["Harbor", "Lantern", "Quarry", "Meadow", "Falcon", "Copper"];
  for (const [index, path] of ["refused", "closed", "refused", "closed"].entries()) {
    const key = `MV${String.fromCharCode(65 + index)}`;
    await createProject({ key, name: `Move ${path} ${index}` });
    const epic = await createIssue({ project: key, title: `Target epic ${key}` });
    await patchIssue(epic.key, { labels: ["epic"] });
    const tasks: string[] = [];
    for (const [at, word] of words.entries()) {
      tasks.push((await createIssue({ project: key, title: `${word} ${key} task ${at}` })).key);
    }
    if (path === "closed") {
      for (const task of tasks.slice(0, 2)) await patchIssue(task, { status: "done" });
    }
    seeds.push({ epic: `Target epic ${key}`, key, marked: [tasks[1] ?? "", tasks[0] ?? ""] });
  }
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const move = bar.getByRole("button", { exact: true, name: "Move" });
  /** What holds the focus: its name, or "the body", and the row and the table around it. */
  const landing = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { inRow: null, inTable: false, name: "the body" };
      }
      return {
        inRow: focused.closest("[data-issue-row]")?.getAttribute("data-issue-row") ?? null,
        inTable: focused.closest("table") !== null,
        name: `${focused.tagName}:${(focused.getAttribute("aria-label") ?? focused.textContent ?? "").trim().slice(0, 40)}`,
      };
    });
  /** Marks the seed's two issues from the keyboard, the lower one first, and picks its epic. */
  const markAndPick = async (seed: (typeof seeds)[number]) => {
    for (const key of seed.marked) {
      const mark = page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
      await mark.focus();
      await page.keyboard.press("Space");
      await expect(mark).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    await pickFromList(listTrigger(bar, "Move to epic"), seed.epic);
    await expect(move).not.toHaveAttribute("aria-disabled", "true");
    await move.focus();
  };
  /** bob's live update takes the marked issues off the view, and the bar goes with them. The
   *  bar's hand-off runs with the commit that took it away, which two frames see done. */
  const emptyTheBar = async (seed: (typeof seeds)[number], status: "done" | "todo") => {
    for (const key of seed.marked) await patchIssue(key, { status }, { login: "bob" });
    await expect(bar).toHaveCount(0);
    await nextFrames(page);
    await nextFrames(page);
  };
  const refuseUpdates = async (route: Route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    return route.fulfill({
      body: JSON.stringify({ code: "REFUSED_FOR_THE_ROW", error: "refused for the row" }),
      contentType: "application/json",
      status: 400,
    });
  };
  const wrong: string[] = [];
  for (const [step, size] of sizes.entries()) {
    await page.setViewportSize(size);
    const where = `project List at ${size.width}x${size.height}`;
    const refused = seeds[step * 2];
    const closed = seeds[step * 2 + 1];
    if (refused === undefined || closed === undefined)
      throw new Error("the seed holds four projects");

    // The control: a Move every write of which is refused. The action is answered and lets its
    // controls go, and Move stays held with no epic staged.
    await openIssuesTab(page, refused.key, "List");
    await markAndPick(refused);
    await page.route(/\/api\/v1\/issues\/[^/?]+$/, refuseUpdates);
    await page.keyboard.press("Enter");
    await expect(page.getByText(/Retry each failed change/).first()).toBeVisible();
    await expect(
      bar.getByRole("button", { exact: true, name: "Take out of epic" })
    ).not.toHaveAttribute("aria-disabled", "true");
    await page.unroute(/\/api\/v1\/issues\/[^/?]+$/, refuseUpdates);
    await expect(move).toHaveAttribute("aria-disabled", "true");
    await expect(move).toBeFocused();
    await nextFrames(page);
    await emptyTheBar(refused, "done");
    // One read is the answer: the bar went two frames back, its hand-off ran with that commit,
    // and nothing else moves the focus now.
    const afterRefused = await landing();

    // The closed path: the List answers the press without a write, and Move stays held with no
    // epic staged, as above.
    await openIssuesTab(page, closed.key, "List");
    await page.goto(`/projects/${closed.key}/issues?status=done`);
    await expect(page.locator("[data-issue-row]")).toHaveCount(2);
    await markAndPick(closed);
    await page.keyboard.press("Enter");
    await expect(page.getByText(/^Only open issues can change epic\./).first()).toBeVisible();
    await expect(move).toHaveAttribute("aria-disabled", "true");
    await expect(move).toBeFocused();
    await nextFrames(page);
    await emptyTheBar(closed, "todo");
    // One read is the answer: the bar went two frames back, its hand-off ran with that commit,
    // and nothing else moves the focus now.
    const afterClosed = await landing();

    if (afterClosed.name !== afterRefused.name || afterClosed.inRow !== afterRefused.inRow) {
      wrong.push(
        `${where}: after a Move with only closed issues marked and a live update that emptied the bar, the focus is on ${afterClosed.name}${afterClosed.inTable ? " (in the table)" : ""}${afterClosed.inRow === null ? "" : ` in row ${afterClosed.inRow}`}, where after a refused Move and the same update it is on ${afterRefused.name}${afterRefused.inRow === null ? "" : ` in row ${afterRefused.inRow}`}`
      );
    }
  }
  // A hand-off that runs for a loss the reader did not cause moves a keyboard reader's place to a
  // control they never chose.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135, round 25 (the tester's; measured at 683f1fdf in builds Jobs
// infra-3135-f-h-ch-2mcxd, infra-3135-f-h-fi-j5t4t and infra-3135-f-h-we-glhk8, again in
// infra-3135-f-h2-ch-g4f2b, infra-3135-f-h2-fi-k2jtf and infra-3135-f-h2-we-spngw, and at 3fce6150
// in infra-3135-f-b-ch-6v9q6, infra-3135-f-b-fi-scqg8 and infra-3135-f-b-we-mx5fq): on the Inbox at
// 390x664, narrowed to its questions with threads waiting above them, the window scrolled 30 px
// with the control row in sight, Enter on the only chip hands the focus to the Filter trigger
// (components/ControlRow.tsx:438-439), and in the same commit the Inbox's anchor holds the row
// nearest the list's centre in place once the window is scrolled at all
// (features/shell/ViewportAnchor.tsx:541-549, mounted at features/inbox/Inbox.tsx:2634) and
// scrolls the window by every row the cleared narrowing draws above that row
// (ViewportAnchor.tsx:744-748). The window went from 30 px to 797 px and the trigger stood at
// -668 px; Enter on Clear filters took it from 30 px to 1,740 px, the trigger at -1,611 px.
// This row, run at 683f1fdf in builds Job infra-3135-f-red-62spp over three threads, read 30 px
// -> 1,269 px with the trigger at -1,140 px after either press (Firefox: 1,265.7 px, -1,137 px).
// Chromium, Firefox and WebKit alike; with the window at 0 the anchor holds no row and the trigger
// stays wholly in sight. A shared control's own press that lands the focus out of sight is a
// failure of this pull request's controls (open-items.md, the rescue's removal, 2026-11-23 21:2xZ).
// WebKit's own scroll anchoring adjusts a layout late (features/shell/ViewportAnchor.tsx:328-332),
// beside the anchor's, so the row runs in all three engines.
// R8 (INFRA-4948) takes both presses out of the bar at 390: below 640 px of the bar's width the
// chips leave its line, and no Clear filters stands beside Filter; the reader takes a narrowing
// off at the top of the Filter panel (components/ControlRow.tsx, `PanelChips`), which stays open
// and hands the focus to its first field once no chip is left (D26). So the row presses the only
// chip there, with the window scrolled as above, and reads R8's rule where it pressed Clear
// filters: none is drawn.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): twenty-three seeded rows, and two presses at 390 px waiting out the list's redraw.
test("taking off the Inbox's only narrowing by its chip in the Filter panel at 390 leaves the focus on a control in sight, with the window scrolled a little or not at all, and no Clear filters is drawn there @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 664, width: 390 });
  await createProject({ key: "CORE", name: "Core" });
  // The questions first and the threads last, so the threads are the newest rows: the ones the
  // narrowing to questions leaves out and that come back at the head of the list when it goes.
  for (let index = 0; index < 20; index++) {
    const issue = await createIssue({ project: "CORE", title: `Narrowed ask ${index}` });
    await createAsk(
      issue.key,
      { question: `Narrowed ${index}: what is the rule?` },
      SURFACE_SESSION
    );
  }
  for (let index = 0; index < 3; index++) {
    const issue = await createIssue({ project: "CORE", title: `Unnarrowed thread ${index}` });
    await seedCommentThread(issue.key, `Unnarrowed ${index}: when does it run?`, "At 03:00.");
  }
  const row = controlRow(page.getByTestId("main-content"));
  const trigger = control(row, CONTROL.panelTrigger);
  const panel = page.getByRole("dialog", { name: "Filter" });
  const chip = panel.getByRole("button", { name: REMOVE_CHIP });
  const clear = row.getByRole("button", { exact: true, name: "Clear filters" });
  const rows = page.locator("[data-inbox-row]");
  const threads = page.locator('[data-inbox-row^="reply:"]');
  /** What holds the keyboard focus: its name, its `data-control`, its top, and whether the
   *  window and every box that clips it show the whole of it; the body is in no sight. */
  const focusNow = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { control: null, inSight: false, name: "the body", top: null };
      }
      const own = focused.getBoundingClientRect();
      let top = 0;
      let bottom = window.innerHeight;
      let left = 0;
      let right = window.innerWidth;
      for (let box = focused.parentElement; box !== null; box = box.parentElement) {
        const style = getComputedStyle(box);
        const clip = box.getBoundingClientRect();
        if (style.overflowY !== "visible") {
          top = Math.max(top, clip.top);
          bottom = Math.min(bottom, clip.bottom);
        }
        if (style.overflowX !== "visible") {
          left = Math.max(left, clip.left);
          right = Math.min(right, clip.right);
        }
      }
      return {
        control: focused.getAttribute("data-control"),
        inSight:
          own.height > 0 &&
          own.top >= top - 1 &&
          own.bottom <= bottom + 1 &&
          own.left >= left - 1 &&
          own.right <= right + 1,
        name: focused.getAttribute("aria-label") || focused.textContent?.trim() || focused.tagName,
        top: Math.round(own.top),
      };
    });
  const wrong: string[] = [];
  for (const offset of [30, 0]) {
    const where = `Inbox at 390x664, the window at ${offset} px, Enter on the only chip`;
    await page.goto("/?group=none&type=question");
    await expect(
      trigger,
      `${where}: the trigger does not count one narrowing`
    ).toHaveAccessibleName("Filter · 1");
    await expect(rows.nth(12), `${where}: the list is no longer than the window`).toBeAttached();
    await expect(threads, `${where}: the narrowing lists a thread`).toHaveCount(0);
    // R8: below 640 px of the bar's width no Clear filters stands beside the trigger.
    expect(await chipsLeaveLine(row, where), `${where}: the bar is 640 px wide or wider`).toBe(
      true
    );
    await expect(clear, `${where}: Clear filters stands in the narrow bar`).toHaveCount(0);
    await page.evaluate((top) => window.scrollTo(0, top), offset);
    // The trigger is focused where it stands, so the focus itself scrolls nothing; Enter opens
    // the panel with the focus on its first control, the only chip.
    await trigger.evaluate((node) => (node as HTMLElement).focus({ preventScroll: true }));
    await page.keyboard.press("Enter");
    await expect(chip, `${where}: the Filter panel does not list the one narrowing`).toHaveCount(1);
    await expect(chip, `${where}: the Filter panel opened with the focus elsewhere`).toBeFocused();
    // Where the window stands just before the press, read once it has held still after the
    // scroll and the open above; it is only reported, never branched on.
    const before = await settledScrollTop(page);
    await page.keyboard.press("Enter");
    await expect(chip, `${where}: the narrowing stayed on`).toHaveCount(0);
    await expect(threads, `${where}: the list did not draw the threads again`).toHaveCount(3);
    // The commit that drew the threads ran the panel's hand-off and the list's anchor in its
    // layout effects, so the window is read once it has held still after them; it is only
    // reported.
    await nextFrames(page);
    const after = await settledScrollTop(page);
    // One read is the answer: the hand-off and the anchor ran in the commit waited for above,
    // and the window has settled since, so nothing moves the focus or the page after this.
    const focus = await focusNow();
    if (!focus.inSight) {
      wrong.push(
        `${where}: the focus is on "${focus.name}" (data-control ${focus.control}), its top at ${focus.top} px in the 664 px window, out of sight; window.scrollY ${before} -> ${after}`
      );
    }
  }
  // A keyboard reader who takes the narrowing off is left on a control they cannot see: their next
  // Tab starts from a place the window has carried away from them.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): twenty labelled issues seeded, and at each of two widths the issue header's Status
// and the project List's Labels filter walked by the arrows, then narrowed by a word and cleared.
// INFRA-3135, round 26 (the tester's, measured in builds Jobs infra-3135-p-hc-pmzhr and
// infra-3135-p-hfw-wx7zj at 8e7e463a and infra-3135-p-bc-td6pm and infra-3135-p-bfw-4tfm7 at
// 34dc6a07, and this row in infra-3135-p-red-7lb7z and infra-3135-p-red2-p8kpf at 8e7e463a): the
// reader opens a shared list from the keyboard, walks it with ArrowDown until its box has scrolled,
// and types a letter that narrows it. The list marks its first remaining option active and the box
// stays where the arrows left it, so that option stands wholly above what the box shows: Triage 39
// px above it in the issue header's Status after "e", lab-01 259 px above it in the project List's
// Labels filter after "1", and the same in the Inbox Filter panel's Project list, the issue's
// Assignee and the Delivery page's Repository and Author lists, in Chromium, Firefox and WebKit at
// 1440x900 and 390x664. Clearing the word leaves the first option out of sight too: lab-01 611 px
// above it in Firefox and WebKit, 655 px in Chromium. Enter then picks an option the reader cannot
// see. At 34dc6a07 the arrows scrolled the box nothing, so a walk never took the box off its top; a
// box the reader scrolled with PageDown left the first option out of sight the same way once the
// word was cleared. The cause: a change of the text box's word sets no active value
// (components/MultiSelect.tsx:704-711), so the list marks its first row active (:322), and only the
// arrows and a single pick's open scroll the box to the option they mark (`showOption`). The rule
// (the architect's ruling of 2026-11-24 08:07Z on round 25's Status row): the shared list shows the
// option it marks active, on every list it draws.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("the shared list keeps the option it marks active wholly inside its box when a typed word narrows it and when the word is cleared, in the issue header's Status and the project List's Labels filter, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with an issue page's Status and the project List's Labels filter walked.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Statuses in sight" });
  for (let label = 1; label <= 20; label += 1) {
    const labelled = await createIssue({ project: "CORE", title: `Labelled ${label}` });
    await patchIssue(labelled.key, { labels: [`lab-${String(label).padStart(2, "0")}`] });
  }
  /** Where the option the open list marks active (its text box's `aria-activedescendant`) stands
   *  in the list's scroll box, in that box's content: the option's top and bottom, and the part
   *  of the content the box shows - below what it draws stuck to its top (the text box), above
   *  what it draws stuck to its bottom (the Clear foot), and cut to the window - with the box's
   *  scrollTop and height. `inside` is whether the option stands wholly in that part. */
  const activeOptionSight = () =>
    page.evaluate(() => {
      const id = document.activeElement?.getAttribute("aria-activedescendant") ?? "";
      const option = id === "" ? null : document.getElementById(id);
      const none = {
        bottom: 0,
        height: 0,
        inside: false,
        name: "no option",
        scrollTop: 0,
        shownBottom: 0,
        shownTop: 0,
        top: 0,
      };
      if (option === null) return none;
      const spoken = option.getAttribute("aria-label") ?? option.textContent ?? "";
      const name = `"${spoken.replace("✓", "").replace(/\s+/g, " ").trim()}"`;
      let found = option.parentElement;
      while (found !== null && !/(auto|scroll)/.test(getComputedStyle(found).overflowY)) {
        found = found.parentElement;
      }
      if (found === null) return { ...none, name };
      const box = found;
      const boxTop = box.getBoundingClientRect().top + box.clientTop;
      let shownTop = Math.max(boxTop, 0);
      let shownBottom = Math.min(boxTop + box.clientHeight, window.innerHeight);
      for (const child of box.children) {
        if (getComputedStyle(child).position !== "sticky") continue;
        const stuck = child.getBoundingClientRect();
        if (stuck.top <= boxTop + 1) shownTop = Math.max(shownTop, stuck.bottom);
        else if (stuck.bottom >= boxTop + box.clientHeight - 1) {
          shownBottom = Math.min(shownBottom, stuck.top);
        }
      }
      const own = option.getBoundingClientRect();
      const content = (y: number) => Math.round(y - boxTop + box.scrollTop);
      return {
        bottom: content(own.bottom),
        height: box.clientHeight,
        inside: own.top >= shownTop - 1 && own.bottom <= shownBottom + 1,
        name,
        scrollTop: Math.round(box.scrollTop),
        shownBottom: content(shownBottom),
        shownTop: content(shownTop),
        top: content(own.top),
      };
    });
  // Each list: how it is reached, its options, how many ArrowDown take its box off its top (its
  // box shows five options), and the letter that narrows it while leaving more than the box shows.
  // The Labels filter is walked to its last option: with its box scrolled 215 px, at lab-10, the
  // narrowing left lab-01 in sight in all three engines (builds Job infra-3135-p-red-7lb7z), and
  // with the box scrolled to its end the narrowing leaves it at 259 px.
  const lists = [
    {
      name: "the issue header's Status",
      listbox: "Status options",
      options: 8,
      downs: 5,
      word: "e",
      narrowed: 6,
      reach: async () => {
        await page.goto(`/issues/${issue.key}/artifacts`);
        return listTrigger(page.getByTestId("issue-header"), "Status");
      },
    },
    {
      name: "the project List's Labels filter",
      listbox: "Labels options",
      options: 20,
      downs: 19,
      word: "1",
      narrowed: 11,
      reach: async () => {
        await openIssuesTab(page, "CORE", "List");
        await expect(page.locator("[data-issue-row]").first()).toBeVisible();
        return filterPicker(page, "Labels");
      },
    },
  ];
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const list of lists) {
      const where = `${list.name} at ${size.width}x${size.height}`;
      const trigger = await list.reach();
      await expect(trigger).toBeVisible();
      await trigger.focus();
      await page.keyboard.press("Enter");
      const listbox = page.getByRole("listbox", { name: list.listbox });
      await expect(listbox.getByRole("option")).toHaveCount(list.options);
      for (let step = 0; step < list.downs; step += 1) {
        await page.keyboard.press("ArrowDown");
        await nextFrames(page);
      }
      // The word below is typed into a list whose box the arrows have taken off its top.
      await expect
        .poll(() => listbox.evaluate((node) => node.parentElement?.scrollTop ?? 0), {
          message: `${where}: ${list.downs} ArrowDown scrolled the list's box nothing`,
        })
        .toBeGreaterThan(0);
      for (const { change, key, count } of [
        { change: `typing "${list.word}"`, key: list.word, count: list.narrowed },
        { change: "clearing the word", key: "Backspace", count: list.options },
      ]) {
        await page.keyboard.press(key);
        await expect(listbox.getByRole("option")).toHaveCount(count);
        await nextFrames(page);
        // One read is the answer: the list has drawn its rows, and nothing else scrolls the box.
        const sight = await activeOptionSight();
        if (!sight.inside) {
          wrong.push(
            `${where}: after ${list.downs} ArrowDown and ${change} the list marks ${sight.name} active at ${sight.top}-${sight.bottom} px of its content, and its box shows ${sight.shownTop}-${sight.shownBottom} px of it (scrollTop ${sight.scrollTop}, height ${sight.height}): the active option is out of sight`
          );
        }
      }
      await page.keyboard.press("Escape");
      await expect(trigger).toHaveAttribute("aria-expanded", "false");
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): twenty labelled issues and fifteen repositories' pull requests seeded, and at each of
// two widths a value of the project List's Labels filter ticked and one of the Delivery page's
// Repository list ticked and unticked, each by Enter, with the list redrawn after each.
// INFRA-3135, round 26 (the tester's, measured in builds Jobs infra-3135-p-hc-pmzhr and
// infra-3135-p-hfw-wx7zj at 8e7e463a and infra-3135-p-bc-td6pm and infra-3135-p-bfw-4tfm7 at
// 34dc6a07, and this row in infra-3135-p-red-7lb7z and infra-3135-p-red2-p8kpf at 8e7e463a): the
// reader walks a shared multi-select with ArrowDown until the option it marks active stands at the
// bottom of its box, and presses Enter. In the project List's Labels filter, with nothing ticked
// before, the tick draws the list's Clear foot, 61 px tall and stuck to the box's bottom, over that
// option: lab-07 stands wholly under it, and so does the ticked option of the Inbox Filter panel's
// Project list and of the Delivery page's Author list, in Chromium, Firefox and WebKit at 1440x900
// and 390x664. The Delivery page lists the picked repositories first, so the tick moves acme/r06 to
// the top of its Repository list, where the box, still scrolled 83 px, shows it nothing in Chromium
// and WebKit, and the untick moves it back to its own place, below what the box shows in Chromium
// (scrolled 39 px) and Firefox (scrolled back to its top at the tick). At 34dc6a07 the arrows
// scrolled the box nothing, so the active option stood below the box before the Enter already. The
// cause: Enter ticks or unticks the active option (components/MultiSelect.tsx:574-581, :349-353),
// the first tick draws the Clear foot (:805-821) and the Delivery page orders the picked values
// first (features/delivery/DeliveryPage.tsx:411), and nothing shows the active option again once
// the list has redrawn: only the arrows and a single pick's open scroll the box to it
// (`showOption`). The rule (the architect's ruling of 2026-11-24 08:07Z on round 25's Status row):
// the shared list shows the option it marks active, on every list it draws.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("Enter on a value of the shared multi-select keeps the option the list marks active wholly inside its box once the list has redrawn, in the project List's Labels filter and the Delivery page's Repository list, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with a Labels tick and a Repository tick and untick.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  for (let label = 1; label <= 20; label += 1) {
    const labelled = await createIssue({ project: "CORE", title: `Labelled ${label}` });
    await patchIssue(labelled.key, { labels: [`lab-${String(label).padStart(2, "0")}`] });
  }
  await seedDeliveryFixture();
  // Fourteen repositories more in the fixture's window, a pull request of the population's each,
  // so the Repository list holds fifteen.
  const pullRequests = Array.from({ length: 14 }, (_, index) => {
    const repo = `acme/r${String(index + 1).padStart(2, "0")}`;
    const number = 100 + index;
    return `('${repo}', ${number}, 'feat: a change in ${repo}', 'https://github.com/${repo}/pull/${number}', 'octocat', '2024-06-01T04:00:00Z', '2024-06-01T05:00:00Z', 1, 1, false, null, ARRAY[]::text[], false)`;
  });
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES ${pullRequests.join(", ")}`
  );
  /** Where the option the open list marks active (its text box's `aria-activedescendant`) stands
   *  in the list's scroll box, in that box's content: the option's id, top and bottom, and the
   *  part of the content the box shows - below what it draws stuck to its top (the text box),
   *  above what it draws stuck to its bottom (the Clear foot), and cut to the window - with the
   *  box's scrollTop and height. `inside` is whether the option stands wholly in that part. */
  const activeOptionSight = () =>
    page.evaluate(() => {
      const id = document.activeElement?.getAttribute("aria-activedescendant") ?? "";
      const option = id === "" ? null : document.getElementById(id);
      const none = {
        bottom: 0,
        height: 0,
        id,
        inside: false,
        name: "no option",
        scrollTop: 0,
        shownBottom: 0,
        shownTop: 0,
        top: 0,
      };
      if (option === null) return none;
      const spoken = option.getAttribute("aria-label") ?? option.textContent ?? "";
      const name = `"${spoken.replace("✓", "").replace(/\s+/g, " ").trim()}"`;
      let found = option.parentElement;
      while (found !== null && !/(auto|scroll)/.test(getComputedStyle(found).overflowY)) {
        found = found.parentElement;
      }
      if (found === null) return { ...none, name };
      const box = found;
      const boxTop = box.getBoundingClientRect().top + box.clientTop;
      let shownTop = Math.max(boxTop, 0);
      let shownBottom = Math.min(boxTop + box.clientHeight, window.innerHeight);
      for (const child of box.children) {
        if (getComputedStyle(child).position !== "sticky") continue;
        const stuck = child.getBoundingClientRect();
        if (stuck.top <= boxTop + 1) shownTop = Math.max(shownTop, stuck.bottom);
        else if (stuck.bottom >= boxTop + box.clientHeight - 1) {
          shownBottom = Math.min(shownBottom, stuck.top);
        }
      }
      const own = option.getBoundingClientRect();
      const content = (y: number) => Math.round(y - boxTop + box.scrollTop);
      return {
        bottom: content(own.bottom),
        height: box.clientHeight,
        id,
        inside: own.top >= shownTop - 1 && own.bottom <= shownBottom + 1,
        name,
        scrollTop: Math.round(box.scrollTop),
        shownBottom: content(shownBottom),
        shownTop: content(shownTop),
        top: content(own.top),
      };
    });
  // Each list: how it is reached, its options, its Clear foot, the count its options carry once
  // the rows they count have answered, and the Enters pressed on the option six ArrowDown mark
  // active, which stands at the bottom of the box (its box shows five options).
  const lists = [
    {
      name: "the project List's Labels filter",
      listbox: "Labels options",
      options: 20,
      clear: "Clear labels",
      counted: /, \d+ issues?$/,
      enters: [{ change: "ticks it, the first value ticked", selected: "true", foot: 1 }],
      reach: async () => {
        await openIssuesTab(page, "CORE", "List");
        await expect(page.locator("[data-issue-row]").first()).toBeVisible();
        return filterPicker(page, "Labels");
      },
    },
    {
      name: "the Delivery page's Repository list",
      listbox: "Repository options",
      options: 15,
      clear: "Clear repository",
      counted: /, \d+ PRs?$/,
      enters: [
        { change: "ticks it, which lists it first", selected: "true", foot: 1 },
        { change: "unticks it, which lists it at its own place", selected: "false", foot: 0 },
      ],
      reach: async () => {
        await page.goto(`/delivery?${TIMELINE_FIXTURE_QUERY}&mode=list`);
        await expect(page.locator("[data-pr-row]").first()).toBeVisible();
        const panel = await openFilterPanel(page);
        return panel.getByRole("button", { name: /^Repository:/ });
      },
    },
  ];
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const list of lists) {
      const where = `${list.name} at ${size.width}x${size.height}`;
      const trigger = await list.reach();
      await expect(trigger).toBeVisible();
      await trigger.focus();
      await page.keyboard.press("Enter");
      const listbox = page.getByRole("listbox", { name: list.listbox });
      await expect(listbox.getByRole("option")).toHaveCount(list.options);
      for (let step = 0; step < 6; step += 1) {
        await page.keyboard.press("ArrowDown");
        await nextFrames(page);
      }
      // The Enter below is pressed on an option that stands whole at the bottom of the box.
      await expect
        .poll(
          () =>
            activeOptionSight().then((sight) => Math.abs(sight.shownBottom - sight.bottom) <= 1),
          { message: `${where}: six ArrowDown left the active option off the bottom of its box` }
        )
        .toBe(true);
      const marked = await activeOptionSight();
      const option = page.locator(`[id="${marked.id}"]`);
      const foot = listbox.locator("xpath=..").getByRole("button", { name: list.clear });
      for (const enter of list.enters) {
        await page.keyboard.press("Enter");
        // Redrawn: the option says what the Enter made of it, the foot stands or has gone, and
        // the option carries the count of the rows the new narrowing lists.
        await expect(option).toHaveAttribute("aria-selected", enter.selected);
        await expect(foot).toHaveCount(enter.foot);
        await expect(option).toHaveAttribute("aria-label", list.counted);
        await nextFrames(page);
        // One read is the answer: the list has redrawn, and nothing else scrolls the box.
        const sight = await activeOptionSight();
        if (!sight.inside) {
          wrong.push(
            `${where}: after ArrowDown to ${marked.name} and Enter that ${enter.change}, the list marks ${sight.name} active at ${sight.top}-${sight.bottom} px of its content, and its box shows ${sight.shownTop}-${sight.shownBottom} px of it (scrollTop ${sight.scrollTop}, height ${sight.height}): the active option is out of sight`
          );
        }
      }
      await page.keyboard.press("Escape");
      await expect(listbox).toHaveCount(0);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the Delivery page opened over the session-groups fixture, and its window changed while the live socket holds the new window's answer.
// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 08:07Z: a count
// beside a value is drawn only once the data it counts has answered, and the class search's C14
// (a previous answer's counts are not shown after a change). Measured in builds Jobs
// infra-3135-c-h-n62l7 (head 8e7e463a), infra-3135-c-b-qzzxg (34dc6a07) and infra-3135-c-m-672j8
// (main 0dbe5048), in Chromium, Firefox and WebKit at 1440x900: with the window changed to Last 7
// days and its timeline answer held, the Filter panel and the Own colour list draw no count, but
// the session-groups editor its **Edit groups** opens lists "Sessions in this window" with the
// previous window's sessions and counts ("2 PRs"). **Edit groups** is offered whenever an answer is
// on screen (DeliveryPage.tsx:601-609, `data === undefined ? null`), with no `counted` gate, and
// the editor is handed that answer's counts (`counts={data.session_pr_counts ?? {}}`, :615-621),
// which it draws beside each session (SessionGroupsEditor.tsx:290, :312). Once the new window
// answers, the editor lists that window's own sessions and counts, which this row reads too, so it
// can tell a count from a placeholder.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("the Delivery page's session-groups editor draws no count from the previous window while a new window's answer is held @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedSessionGroupsFixture();
  const main = page.getByTestId("main-content");
  /** Whether a frame the page sends is a timeline view's subscribe, which `holding` keeps. */
  const timelineSubscribe = (message: string | Buffer): boolean => {
    try {
      const frame = JSON.parse(String(message)) as { type?: string; view?: { kind?: string } };
      return frame.type === "subscribe" && frame.view?.kind === "timeline";
    } catch {
      return false;
    }
  };
  // One route for the page's live socket: it passes every frame through to the server, and while
  // `holding` keeps each timeline subscribe the page sends, so the new window has no answer.
  let holding = false;
  const held: (() => void)[] = [];
  await page.routeWebSocket(/\/ws\/live$/u, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      if (holding && timelineSubscribe(message)) {
        held.push(() => server.send(message));
        return;
      }
      server.send(message);
    });
  });
  /** Each row of the editor's "Sessions in this window", by its text, read once the list holds
   *  `count` rows, so the read is of a list that is whole. */
  const sessions = async (count: number): Promise<string[]> => {
    await openFilterPanel(page, main);
    await page.getByRole("button", { name: "Edit groups" }).click();
    const editor = page.getByRole("dialog", { name: "Session groups" });
    const rows = editor
      .getByRole("list", { name: "Sessions in this window" })
      .getByRole("listitem");
    await expect(rows).toHaveCount(count);
    const names = await rows.evaluateAll((elements) =>
      elements.map((element) => (element.textContent ?? "").replace(/\s+/g, " ").trim())
    );
    await editor.getByRole("button", { name: "Close session groups" }).click();
    await expect(editor).toHaveCount(0);
    await closeControlPanel(main);
    return names;
  };
  await page.goto("/delivery");
  await expect(main.getByText("3 PRs in current filter/window")).toBeVisible();
  // The control: the default window's answer counts each of its three sessions.
  const answered = await sessions(3);
  expect(answered.filter((name) => /\b1 PR$/u.test(name))).toHaveLength(3);
  holding = true;
  await pickFromList(listTrigger(page, "Window"), "Last 7 days");
  await expect(page).toHaveURL(/[?&]window=7d/u);
  await expect.poll(() => held.length).toBeGreaterThan(0);
  await expect(main.getByText("Counting PRs in current filter/window…")).toBeVisible();
  const wrong = (await sessions(3))
    .filter((name) => /\b\d+ PRs?\b/u.test(name))
    .map((name) => `window held: the editor offers "${name}" before the new window answered`);
  holding = false;
  for (const send of held.splice(0)) send();
  await expect(main.getByText("2 PRs in current filter/window")).toBeVisible();
  // The new window's own answer: its two sessions, each with its count, and no Reviewer.
  const sevenDays = await sessions(2);
  expect(
    sevenDays.filter((name) => /^Planner, (first|second) run\b.*\b1 PR$/u.test(name))
  ).toHaveLength(2);
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the project List opened with the agent list held, and its Claimed by filter read before and after the list answers.
// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 08:07Z and the
// round-23 rule (an instance in main's code, in a file this pull request changes, is fixed here).
// Measured in builds Jobs infra-3135-c-h-n62l7 (head 8e7e463a) and infra-3135-c-m-672j8 (main
// 0dbe5048), and infra-3135-c-b-qzzxg (34dc6a07), in Chromium, Firefox and WebKit at 1440x900: with
// the agent list (`GET /api/v1/agents`) held, the Claimed by filter offers "Nobody or lapsed, 1
// issue" over an unclaimed issue and one claimed by a session that is no longer running; once the
// list answers it offers "Nobody or lapsed, 2 issues". Which claims have lapsed is the agent
// list's answer (IssuesTable.tsx:196-199); until it answers every claim is judged held
// (issues-table-data.ts:150, `claimIsTakeable` over a registry whose states are `unknown`), and the
// table hands its header filters the counts with no gate (`filterCounts={openFacets}`, no
// `filterCounted`, IssuesTable.tsx:596).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("the project List's Claimed by filter draws no count of the lapsed claims before the agent list answers @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Nobody holds this" });
  const gone = await createIssue({ project: "CORE", title: "Held by a session that is gone" });
  const token = await mintAgentToken("claim-lapsed", "alice");
  const holder = { session_id: "e2e-claim-lapsed", title: "Lapsed holder" };
  await setLiveSessions([holder]);
  await claimIssue(gone.key, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });
  await setLiveSessions([]);
  // The agent list is held until the row lets it through.
  let holding = true;
  const held: Route[] = [];
  await page.route(
    (url) => url.pathname === "/api/v1/agents",
    async (route) => {
      if (holding) {
        held.push(route);
        return;
      }
      await route.continue();
    }
  );
  /** Each option of the Claimed by filter, by its accessible name, read once it holds all three. */
  const claimOptions = async (): Promise<string[]> => {
    const trigger = page.getByRole("button", { name: /^Filter Claimed by(?:: \d+ picked)?$/u });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(3);
    const names = await options.evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("aria-label") ?? "")
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names;
  };
  await page.goto("/projects/CORE/issues");
  await expect(page.locator(`[data-issue-row="${gone.key}"]`)).toBeVisible();
  const wrong = (await claimOptions())
    .filter((name) => /^Nobody or lapsed, \d+ issues?$/u.test(name))
    .map((name) => `agent list out: Claimed by offers "${name}"`);
  // The agent list was asked for and is held: what the filter offered is the unanswered state.
  await expect.poll(() => held.length).toBeGreaterThan(0);
  holding = false;
  for (const route of held.splice(0)) await route.continue();
  // The control: once the list answers, the lapsed claim counts beside the unclaimed issue.
  await expect.poll(claimOptions).toContain("Nobody or lapsed, 2 issues");
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the project List opened over two open and two closed issues, with Done added while the closed issues read is held.
// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 08:07Z (never a
// count of the rows read so far) and the round-23 rule. Measured in builds Jobs
// infra-3135-c3-h-pwg42 (head 8e7e463a), infra-3135-c3-b-w7gw5 (34dc6a07) and infra-3135-c3-m-9gdl4
// (main 0dbe5048), in Chromium, Firefox and WebKit at 1440x900: with Done added to the Status
// filter and the closed issues read (`GET /api/v1/issues?project=CORE` without `open`) held, the
// page says "Loading closed issues…" and Status offers "Done, not loaded", but Assignee offers
// "alice, 1 issue" and Priority "P1, 1 issue", counts of the open issues alone; once the closed
// issues answer they read "alice, 3 issues" and "P1, 3 issues". The table is drawn from the open
// read while the closed one is out (IssuesTable.tsx:211-213), only Done is marked unread
// (`DONE_UNLOADED`, :69, :597), and every other value is counted over the rows read so far
// (`filterCounts={openFacets}`, :596, with no `filterCounted`; the `counted` of :249 gates the
// Showing chip alone).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("the project List's header filters draw no count while the closed issues they count are out @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const open = await createIssue({ project: "CORE", title: "Open work for alice" });
  await patchIssue(open.key, { assignee: "alice", priority: 1 });
  const other = await createIssue({ project: "CORE", title: "Open work for bob" });
  await patchIssue(other.key, { assignee: "bob", priority: 2 });
  const closed: string[] = [];
  for (const title of ["Closed work one", "Closed work two"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { assignee: "alice", priority: 1 });
    await changeIssueStatus(issue.key, { status: "done" });
    closed.push(issue.key);
  }
  // The closed issues read - the project's whole list, asked for once Done is picked - is held
  // until the row lets it through; the open issues read goes through.
  let holding = true;
  const held: Route[] = [];
  await page.route(
    (url) =>
      url.pathname === "/api/v1/issues" &&
      url.searchParams.get("project") === "CORE" &&
      !url.searchParams.has("open"),
    async (route) => {
      if (holding) {
        held.push(route);
        return;
      }
      await route.continue();
    }
  );
  /** Each option of one header filter, by its accessible name, read once it holds `count`. */
  const headerOptions = async (header: string, count: number): Promise<string[]> => {
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${header}(?:: \\d+ picked)?$`, "u"),
    });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("aria-label") ?? "")
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names;
  };
  await page.goto("/projects/CORE/issues");
  await expect(page.locator(`[data-issue-row="${other.key}"]`)).toBeVisible();
  const status = page.getByRole("button", { name: /^Filter Status(?:: \d+ picked)?$/u });
  await status.click();
  await listOf(status)
    .getByRole("option", { name: /^Done\b/u })
    .click();
  await page.keyboard.press("Escape");
  await expect(page).toHaveURL(/[?&]status=done/u);
  await expect(page.getByText("Loading closed issues…")).toBeVisible();
  await expect.poll(() => held.length).toBeGreaterThan(0);
  const wrong: string[] = [];
  for (const header of ["Assignee", "Priority"]) {
    wrong.push(
      ...(await headerOptions(header, 2))
        .filter((name) => /, \d+ issues?$/u.test(name))
        .map((name) => `closed issues out: ${header} offers "${name}"`)
    );
  }
  holding = false;
  for (const route of held.splice(0)) await route.continue();
  await expect(page.getByText("Loading closed issues…")).toHaveCount(0);
  await expect(page.locator(`[data-issue-row="${closed[0]}"]`)).toBeVisible();
  // The control: with the closed issues read, each value counts them too.
  expect(await headerOptions("Assignee", 2)).toEqual(["alice, 3 issues", "bob, 1 issue"]);
  expect(await headerOptions("Priority", 2)).toEqual(["P1, 3 issues", "P2, 1 issue"]);
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): a question seeded with two followers and a subscriber, opened in the Inbox at two widths with the subscriber read held.
// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 08:07Z (a count
// beside a value is drawn only once the data it counts has answered) and the round-23 rule (an
// instance in main's code, in a file this pull request changes, is fixed here). Measured in builds
// Jobs infra-3135-cr-h-5jqpq (head 8e7e463a) and infra-3135-cr-m-bfr59 (main 0dbe5048), in
// Chromium, Firefox and WebKit at 1440x900 (the reading column's card) and 390x664 (the list's
// card): a question that reaches its asker, a session that replied to it and a session subscribed
// to its issue reads "Reaches 2" while the issue's subscriber read
// (`GET /api/v1/issues/{key}/subscribers`) is out, and "Reaches 3" once it answers. The card counts
// the subscriber read's rows as none while it has no answer
// (`subscribersQuery.data ?? EMPTY_SUBSCRIBERS`, features/inbox/AskRecipients.tsx:276) and holds
// the count back only on a failure (`const counted = !subscribersQuery.isError`, :288), so the
// toggle draws `reachLabel` (:372) over the followers alone.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("an Inbox card's Reaches draws no count while the subscribers it counts are out @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with a question seeded and the Inbox opened for it.
  test.setTimeout(90_000);
  await createProject({ key: "CORE", name: "Core" });
  const asker = { id: "e2e-reach-asker", kind: "session" as const };
  const replier = { id: "e2e-reach-replier", kind: "session" as const };
  const watcher = "e2e-reach-watcher";
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: replier.id, title: "Replier (e2e)" },
    { session_id: watcher, title: "Watcher (e2e)" },
  ]);
  // Every issue's subscriber read is held until the row lets it through.
  let holding = true;
  const held: Route[] = [];
  await page.route(
    (url) => /^\/api\/v1\/issues\/[^/]+\/subscribers$/u.test(url.pathname),
    async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (holding) {
        held.push(route);
        return;
      }
      await route.continue();
    }
  );
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    holding = true;
    const issue = await createIssue({ project: "CORE", title: `Who hears it at ${size.width}` });
    await patchIssue(issue.key, { assignee: "alice" });
    const ask = await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: `Ship it at ${size.width}?` },
      { actor: asker, as: "agent" }
    );
    await createComment(
      issue.key,
      { ask_id: ask.id, body: "Tests are green on my side." },
      { actor: replier, as: "agent" }
    );
    await setInterests([
      { session_id: watcher, topics: [`notifications.dispatch.issue.${issue.key}.>`] },
    ]);
    await page.goto("/");
    await expect(inboxRow(page, ask.id)).toBeVisible();
    const card = await askCard(page, ask.id);
    const toggle = card
      .getByRole("region", { name: "Recipients" })
      .getByRole("button", { name: /^(?:Reaches|Recipients)\b/u });
    await expect(toggle).toBeVisible();
    await expect.poll(() => held.length).toBeGreaterThan(0);
    // The subscriber read is held, so what the toggle names now is the unanswered state.
    const whileOut = (await toggle.textContent()) ?? "";
    wrong.push(
      ...[whileOut]
        .filter((label) => /^Reaches \d/u.test(label))
        .map(
          (label) =>
            `${size.width}x${size.height}: the card reads "${label}" while its subscribers are out`
        )
    );
    holding = false;
    for (const route of held.splice(0)) await route.continue();
    // The control: once the subscribers answer, the card counts all three.
    await expect(toggle).toHaveText(/^Reaches 3\b/u);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135, round 26 (the tester's): 5a0806a1 and 1946cb1c leave the pick the cause when a
// second press lands on a control the pick's write holds, and the round-25 red row holds the
// bar's landing then to where the pick alone puts it, never the body. Measured at 8e7e463a in
// builds Jobs infra-3135-f-h-w2-cpzw7 and infra-3135-f-h-d-q7gm7, and at 34dc6a07 in
// infra-3135-f-b-w2-tdtlg and infra-3135-f-b-d-tm7ct, in Chromium, Firefox and WebKit at 1440x900
// and 390x664: under the project List's Epic filter, with Take out of epic's writes held, a
// double click or two clicks on the held Start next of a marked row move the focus onto that
// Start next, and once the writes land and take the row away the focus is on the body; the same
// press with no second one puts it on the checkbox of the row after the marked two. The focus is
// then on the row's Start next and not in the bar, so the bar's hand-off has nothing to hand on
// (components/SelectionBar.tsx:375-381), and the List's row hand-off reads the loss as no press of
// the reader's (components/focus-place.ts:296): `causedByReader` (:1142-1144) reads the lost
// control, its rows and its region, which hold neither Take out of epic nor a root it was armed
// at, so nothing lands the focus. With the focus put back on Take out of epic after the clicks, as
// Safari's click leaves it, 8e7e463a lands on the row after the marked two and 34dc6a07 on the
// body, so 1946cb1c holds the press; it is the landing that is missing.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): two projects of thirteen issues, and six Take out of epic presses with held writes.
test("a double click or two clicks on a marked row's held Start next while Take out of epic's writes are out leave the focus where Take out of epic alone puts it once the bar goes, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  // Every issue write sent while `holding` is held until the row lets it through, so the second
  // presses land while Take out of epic's writes are out.
  let holding = false;
  const held: Route[] = [];
  await page.route(/\/api\/v1\/issues\/[^/?]+$/, async (route) => {
    if (route.request().method() !== "PATCH" || !holding) return route.fallback();
    held.push(route);
  });
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const rows = page.locator("[data-issue-row]");
  /** Where the keyboard focus rests: the body, or the focused element and the row it is in, by
   *  the names `slots` gives the rows read before the press. */
  const landingOf = (slots: Record<string, string>) =>
    page.evaluate((slots) => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) return "the body";
      const label =
        focused instanceof HTMLInputElement ? focused.labels?.[0]?.textContent : undefined;
      const words = (focused.getAttribute("aria-label") || label || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      const row = focused.closest("[data-issue-row]")?.getAttribute("data-issue-row");
      const control = row == null ? words : words.replace(row, "").trim();
      const name = `${focused.tagName.toLowerCase()} "${control}"`;
      if (row === undefined || row === null) return `${name} in no row`;
      return `${name} in ${slots[row] ?? `row ${row}`}`;
    }, slots);
  const got: string[] = [];
  const want: string[] = [];
  for (const [step, size] of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ].entries()) {
    await page.setViewportSize(size);
    // One project per width: an epic and twelve tasks under it, each in Legion's queue, so every
    // row but the queue's first draws a Start next.
    const key = step === 0 ? "SNA" : "SNB";
    await createProject({ key, name: `Start next ${size.width}` });
    const epic = await createIssue({ project: key, title: `Queue epic ${key}` });
    await patchIssue(epic.key, { labels: ["epic"] });
    for (let index = 0; index < 12; index++) {
      const task = await createIssue({
        parent: epic.key,
        project: key,
        title: `Queued ${key} task ${index}`,
      });
      await patchIssue(task.key, { labels: ["legion"], status: "todo" });
    }
    const view = `/projects/${key}/issues?f.epic=${encodeURIComponent(`${epic.key} · Queue epic ${key}`)}`;
    let alone = "";
    for (const press of ["no second press", "a double click", "two clicks"] as const) {
      const where = `project List at ${size.width}x${size.height} with ${press} on a marked row's held Start next`;
      await page.goto(view);
      await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(4);
      // The order the rows stand in once the poll above found them drawn: nothing moves them
      // before the marks below, and it only names the rows.
      const order = await rows.evaluateAll((nodes) =>
        nodes.map((node) => node.getAttribute("data-issue-row") ?? "")
      );
      const [above = "", upper = "", lower = "", after = ""] = order;
      const slots = {
        [above]: "the row above the marked two",
        [upper]: "the upper marked row",
        [lower]: "the lower marked row",
        [after]: "the row after the marked two",
      };
      // The lower row first, so the row marked last is the upper one, whose place the focus takes.
      for (const issue of [lower, upper]) {
        const mark = page.getByRole("checkbox", { exact: true, name: `Select ${issue}` });
        await mark.focus();
        await page.keyboard.press("Space");
        await expect(mark).toBeChecked();
      }
      await expect(bar).toContainText("2 selected");
      const takeOut = bar.getByRole("button", { exact: true, name: "Take out of epic" });
      await takeOut.focus();
      holding = true;
      await page.keyboard.press("Enter");
      await expect
        .poll(() => held.length, { message: `${where}: Take out of epic sent no write` })
        .toBeGreaterThan(0);
      const startNext = page
        .locator(`[data-issue-row="${upper}"]`)
        .getByRole("button", { exact: true, name: "Start next" });
      await expect(startNext, `${where}: the marked row's Start next is not held`).toHaveAttribute(
        "aria-disabled",
        "true"
      );
      if (press === "a double click") await startNext.dblclick({ force: true });
      if (press === "two clicks") {
        await startNext.click({ force: true });
        await startNext.click({ force: true });
      }
      await nextFrames(page);
      holding = false;
      for (const route of held.splice(0)) await route.continue();
      await expect(bar).toHaveCount(0);
      await expect(page.locator(`[data-issue-row="${upper}"]`)).toHaveCount(0);
      // The bar's hand-off runs after the commit that took the bar away, so it is done after
      // these frames.
      await nextFrames(page);
      await nextFrames(page);
      // One read is the answer: the writes have landed, the bar has gone and every hand-off of
      // the press has run, so nothing moves the focus after this.
      const landing = await landingOf(slots);
      if (press === "no second press") {
        alone = landing;
      } else {
        got.push(`${where}: ${landing}`);
        want.push(`${where}: ${alone}`);
      }
    }
  }
  // A keyboard reader whose second press of a held control lands their focus on the body starts
  // their next Tab from the top of the page, far from the rows they were working in.
  expect(got, got.join("; ")).toEqual(want);
});

// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 10:03Z ("Round 25
// failed"), which put into round 26 the armed hand-off's partly-in-sight landing, scrolled to its
// nearest edge (the ruling of 2026-11-21 12:3xZ). Measured at 8e7e463a in builds Job
// infra-3135-f-h-w2-cpzw7 and at 34dc6a07 in infra-3135-f-b-w2-tdtlg, in Chromium, Firefox and
// WebKit at 390x664: under the project List's Epic filter, with the mark of the row marked last
// cut by 30% of its height at the bottom edge of the table's scroll box, Take out of epic lands the
// focus on the checkbox of the row now at its place, "Select CORE-21", top 458 to bottom 471 in a
// box whose visible bottom is at 467, and scrolls nothing. The selection bar's hand-off takes the
// focus with no scroll and never shows its landing whole (components/SelectionBar.tsx:137), where
// the other hand-offs of this pull request land through `land`, which shows a control partly in
// sight whole at its nearest edge (components/focus-place.ts:486-503). This row makes the same cut
// at 1440x900 too.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): forty-five seeded issues, and two Take out of epic presses read once the box settles.
test("Take out of epic with the row marked last cut by the table box's bottom edge hands the focus to a control wholly inside every box that clips it, on the project List at 390 and 1440 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 44; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  const view = `/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees:
   *  inside the window and the table's box, with nothing, such as the sticky head, drawn over its
   *  centre. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        const above = Math.max(0, frame.top);
        const below = Math.min(window.innerHeight, frame.bottom);
        if (own.top < above || own.bottom > below) return false;
        const hit = document.elementFromPoint(own.left + own.width / 2, own.top + own.height / 2);
        return hit !== null && row.contains(hit);
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** How a control stands against the window and every box around it that clips it: the mark of
   *  the row `key` names, else the focused element. `whole` says whether it lies wholly inside
   *  all of them, and `text` names it, its rect and each edge that cuts it. */
  const standing = (key?: string) =>
    page.evaluate((key) => {
      const element =
        key === null
          ? document.activeElement
          : document.querySelector(`[data-issue-row="${CSS.escape(key)}"] input[type="checkbox"]`);
      if (!(element instanceof HTMLElement) || element === document.body) {
        return { text: "the body", whole: false };
      }
      const round = (value: number) => Math.round(value * 10) / 10;
      const own = element.getBoundingClientRect();
      const label =
        element instanceof HTMLInputElement ? element.labels?.[0]?.textContent?.trim() : undefined;
      const name = element.getAttribute("aria-label") || label || element.tagName.toLowerCase();
      const cuts: string[] = [];
      const cutBy = (box: string, clip: Omit<DOMRectReadOnly, "toJSON" | "x" | "y">) => {
        if (own.top < clip.top - 0.5) cuts.push(`${box}'s top edge at ${round(clip.top)}`);
        if (own.bottom > clip.bottom + 0.5) {
          cuts.push(`${box}'s bottom edge at ${round(clip.bottom)}`);
        }
        if (own.left < clip.left - 0.5) cuts.push(`${box}'s left edge at ${round(clip.left)}`);
        if (own.right > clip.right + 0.5) cuts.push(`${box}'s right edge at ${round(clip.right)}`);
      };
      cutBy("the window", {
        bottom: window.innerHeight,
        height: window.innerHeight,
        left: 0,
        right: window.innerWidth,
        top: 0,
        width: window.innerWidth,
      });
      for (let around = element.parentElement; around !== null; around = around.parentElement) {
        const style = getComputedStyle(around);
        if (style.overflowX !== "visible" || style.overflowY !== "visible") {
          cutBy(
            `the box ${around.dataset.testid ?? around.tagName.toLowerCase()}`,
            around.getBoundingClientRect()
          );
        }
      }
      const rect = `top ${round(own.top)} to bottom ${round(own.bottom)}, left ${round(own.left)} to right ${round(own.right)}`;
      const verdict =
        cuts.length === 0
          ? "wholly inside every box that clips it"
          : `cut by ${cuts.join(" and ")}`;
      return {
        text: `${element.tagName.toLowerCase()} "${name}" at ${rect}, ${verdict}`,
        whole: cuts.length === 0,
      };
    }, key ?? null);
  const landings: { text: string; whole: boolean }[] = [];
  for (const size of [
    { height: 664, width: 390 },
    { height: 900, width: 1440 },
  ]) {
    await page.setViewportSize(size);
    const where = `project List at ${size.width}x${size.height}`;
    await page.goto(view);
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(12);
    // The window brings the table's box to just below the app's 56 px header, and the box
    // scrolls half way down its rows, so rows stand in it above and below the ones marked.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
    // The rows read once the box has held still and the poll above found them in sight; they
    // only name the two rows marked below, the middle one in sight and the one after it.
    const { order, shown } = await drawn();
    const at = order.indexOf(shown[Math.floor(shown.length / 2)] ?? "");
    const upper = order[at] ?? "";
    const lower = order[at + 1] ?? "";
    // The lower row first, so the row marked last is the upper one, whose place the focus takes.
    for (const key of [lower, upper]) {
      await markOf(key).focus();
      await page.keyboard.press("Space");
      await expect(markOf(key)).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    // The box scrolls so that 30% of the upper row's mark stands below the box's visible bottom:
    // the row after the marked two moves up into that place when they go, cut the same way, and
    // the centre of its mark stays in sight, so the bar's hand-off takes it.
    await markOf(upper).evaluate((mark) => {
      const scroller = mark.closest<HTMLElement>('[data-testid="issues-table-scroll"]');
      const own = mark.getBoundingClientRect();
      const bottom = Math.min(window.innerHeight, scroller?.getBoundingClientRect().bottom ?? 0);
      scroller?.scrollBy(0, own.bottom - (bottom + own.height * 0.3));
    });
    await settledScrollTop(box);
    // One read is the answer: the box has held still, and the cut is checked before the press so
    // that a case the page cannot make fails here rather than passing.
    const arranged = await standing(upper);
    expect(arranged.whole, `${where}: the row marked last is not cut: ${arranged.text}`).toBe(
      false
    );
    await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
    await page.keyboard.press("Enter");
    await expect(bar).toHaveCount(0);
    await expect(markOf(upper)).toHaveCount(0);
    // The bar's hand-off runs after the commit that took the bar away, so it is done after these
    // frames.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: the writes have landed, the bar has gone and every hand-off of the
    // press has run, so nothing moves the focus or the box after this.
    const landing = await standing();
    landings.push({ text: `${where}: the focus is on ${landing.text}`, whole: landing.whole });
  }
  // A landing the box cuts is a control the reader cannot read whole: the press that took their
  // rows away leaves them looking for the focus at the box's edge.
  expect(
    landings.map((landing) => landing.whole),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-25 04:59Z: a click of
// the reader's that puts the focus on the body while a Board move's dry run is out is the reader
// going elsewhere, so nothing moves the focus once the move lands; the body is the place the press
// left only when the press's own card went away. Measured at 8e7e463a in builds Jobs
// infra-3135-f-h-ch-wlcgj (Chromium) and infra-3135-f-h-fw-7ssrl (WebKit) at 390x664, with a
// click that landed below the window's bottom, and in infra-3135-f-red2-qhtr5 with a click on a
// blank spot of the shell's header: a child's card moved by Shift+L, with the rule's dry run held
// and the focus put on the body by that click meanwhile, took the focus from the body to the
// moved card once the move landed. A click inside the main content puts the focus on the main
// content itself (`tabIndex={-1}`), which the effect leaves where it is. 34dc6a07
// (infra-3135-f-b-ch-cb98p, infra-3135-f-b-fw-t8fw8) sends no dry run, so the case cannot be made
// there. The move's focus effect counts any focus on the body, once the card it moved has left
// the page, as where the press left it (features/project/IssueBoard.tsx:521), so it cannot tell
// the reader's own click from the card's going. Firefox kept the focus on the card at the click
// the first Jobs sent, so the row runs in Chromium and WebKit, as the ruling asks.
// Its engines: the clause of the `webkit` project that names the rows INFRA-3135's round-26
// tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): two projects of three issues on the Board, and two keyboard moves with held dry runs.
test("a click that puts the focus on the body while a Board move's dry run is out keeps it there once the move lands, where the same move with no click takes it to the moved card, at 390 @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 664, width: 390 });
  // Every dry run of a status change sent while `holding` is held until the row lets it through.
  let holding = false;
  const held: Route[] = [];
  await page.route(/\/api\/v1\/issues\/[^/?]+\/status-change$/, async (route) => {
    const dryRun = (route.request().postData() ?? "").includes('"dry_run":true');
    if (!holding || !dryRun) return route.fallback();
    held.push(route);
  });
  /** What holds the keyboard focus: the body, or its tag and name. */
  const focusName = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) return "the body";
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      return `${focused.tagName.toLowerCase()} "${words}"`;
    });
  /** A point of the window where the element drawn, and every element around it, takes no focus,
   *  outside the main content and the board's columns: a click there leaves no control focused,
   *  and Chromium and WebKit put the focus on the body. */
  const blankSpot = () =>
    page.evaluate(() => {
      const takesFocus =
        "a[href], button, input, select, textarea, summary, iframe, label, [tabindex], [contenteditable]";
      for (let y = 4; y < window.innerHeight; y += 8) {
        for (let x = 4; x < window.innerWidth; x += 8) {
          const hit = document.elementFromPoint(x, y);
          if (
            hit !== null &&
            hit.closest(takesFocus) === null &&
            hit.closest("main, [data-board-column]") === null
          ) {
            return { x, y };
          }
        }
      }
      return null;
    });
  for (const press of ["no click", "a click on a blank spot"] as const) {
    const where = `Board at 390x664 with ${press} while the dry run is out`;
    // One project per press: a parent in work and a child under it, so the child's move is one
    // the rule's dry run checks first, and one more card in the same column.
    const key = press === "no click" ? "BDNC" : "BDCL";
    await createProject({ key, name: `Board ${press}` });
    const parent = await createIssue({ project: key, title: `Parent ${key}` });
    const child = await createIssue({ parent: parent.key, project: key, title: `Child ${key}` });
    const other = await createIssue({ project: key, title: `Other ${key}` });
    for (const issue of [parent, child, other]) await patchIssue(issue.key, { status: "todo" });
    await openIssuesTab(page, key, "Board");
    const card = page.locator(`[data-board-card="${child.key}"]`);
    await card.focus();
    await expect(card).toBeFocused();
    holding = true;
    await page.keyboard.press("Shift+L");
    await expect
      .poll(() => held.length, { message: `${where}: the move sent no dry run` })
      .toBeGreaterThan(0);
    let afterClick = "";
    if (press === "a click on a blank spot") {
      // The reader scrolls the window back to the top, which moves no focus, and clicks a blank
      // spot of the shell's header there: the main content takes the focus at a click anywhere
      // inside it (`app.tsx` gives it `tabIndex={-1}`), and the header is outside it.
      await page.evaluate(() => window.scrollTo(0, 0));
      await settledScrollTop(page);
      // The spot is read once the window has held still at its top, and nothing is drawn over
      // the page while the dry run is held.
      const spot = await blankSpot();
      expect(spot, `${where}: no point of the window takes no focus`).not.toBeNull();
      await page.mouse.click(spot?.x ?? 0, spot?.y ?? 0);
      await nextFrames(page);
      // One read is the answer: the click's change of focus is done after the frames above, and
      // nothing else moves the focus while the dry run is held.
      afterClick = await focusName();
    }
    holding = false;
    for (const route of held.splice(0)) await route.continue();
    await expect(
      page.locator(`[data-board-column="in_progress"] [data-board-card="${child.key}"]`),
      `${where}: the card did not move to In progress`
    ).toHaveCount(1);
    // The move's focus effect runs with the render that draws the card in its column, so it is
    // done after these frames.
    await nextFrames(page);
    await nextFrames(page);
    if (press === "no click") {
      await expect(
        card,
        `${where}: the move did not take the focus to the moved card`
      ).toBeFocused();
    } else {
      // One read is the answer: the move has landed and its focus effect has run, so nothing
      // moves the focus after this.
      const afterMove = await focusName();
      expect(
        afterMove,
        `${where}: the click left the focus on ${afterClick}, and the move took it to ${afterMove}`
      ).toBe(afterClick);
    }
  }
});

// INFRA-3135, round 27 (the tester's), by the architect's ruling of 2026-11-25 13:01Z: "a count is
// drawn once every read it counts has answered; a read that a count does not count never hides it
// (the architect, 2026-11-25 13:01Z)". Measured in builds Jobs infra-3135-c-h-kwghj and
// infra-3135-c2-h-qmwvp (head 7deb75af) and infra-3135-c-b-pcmr9 and infra-3135-c2-b-25xqq
// (5ae79d88), in Chromium, Firefox and WebKit at 1440x900: with the agent list
// (`GET /api/v1/agents`) answered 500 every time and Nobody or lapsed picked, 7deb75af draws no
// count in any header filter for good, after the page has said that it could not check which
// claims have lapsed. Over three issues that no one claims, Assignee offers "alice" where 5ae79d88
// offers "alice, 3 issues", and Claimed by offers "Nobody" where 5ae79d88 offers "Nobody, 3
// issues". Those counts read the issues alone, which answered. The header filters wait for the
// agent registry whenever Claimed by asks for it (`readsRegistry`, IssuesTable.tsx:197), the
// registry's `known` stays false after a read that failed (features/refs/agent-registry.ts:160),
// and `filterCounted = counted && (!readsRegistry || registry.known)` (IssuesTable.tsx:449) then
// hides every count of every filter: the ones the registry decides and the ones it does not.
// 5ae79d88 draws both kinds, the registry's own among them ("Nobody or lapsed, 1 issue" over a
// claim whose session had gone, which it could not judge), so this row holds the counts the
// registry decides to none while it is refused. The control: with the agent list answered, the
// same pages draw every count, Nobody or lapsed and a holder's among them.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): two projects on the List, each read with the agent list refused and then answered.
test("with the agent list refused, the project List's header filters draw each count the list does not decide once the issues answer, and none that it decides @firefox @webkit", async ({
  page,
}) => {
  // Two projects, each read refused and then answered, five filters each.
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  /** One project's three issues: the first and the third for alice at P1 labelled `alpha`, the
   *  second for bob at P2. No title shares a word with another, so none reads as a duplicate. */
  const seed = async (key: string, name: string, titles: readonly [string, string, string]) => {
    await createProject({ key, name });
    const alice = await createIssue({ project: key, title: titles[0] });
    await patchIssue(alice.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
    const bob = await createIssue({ project: key, title: titles[1] });
    await patchIssue(bob.key, { assignee: "bob", priority: 2 });
    const more = await createIssue({ project: key, title: titles[2] });
    await patchIssue(more.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
    return { alice: alice.key, bob: bob.key, more: more.key };
  };
  // CORE: no issue is claimed. HELD: one issue is claimed by a session that runs, so the answered
  // agent list keeps that claim held, and Nobody or lapsed lists the same two issues it lists
  // while the list is refused, where the page counts every claim as held.
  const core = await seed("CORE", "Core", ["Harbour crane", "Signal tower", "Ferry ramp"]);
  const held = await seed("HELD", "Held", ["Granite quarry", "Orchard fence", "Lantern repair"]);
  const holder = { session_id: "e2e-claim-running", title: "Running holder" };
  await setLiveSessions([holder]);
  const token = await mintAgentToken("claim-running", "alice");
  await claimIssue(held.more, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });
  // The agent list answers 500 every time it is asked while `refusing`, and goes through after.
  let refusing = true;
  let served = 0;
  await page.route(
    (url) => url.pathname === "/api/v1/agents",
    async (route) => {
      if (refusing) {
        await route.fulfill({
          body: '{"error":{"code":"INTERNAL","message":"agent list refused"}}',
          contentType: "application/json",
          status: 500,
        });
        return;
      }
      served += 1;
      await route.continue();
    }
  );
  /** Each option of one header filter, by its accessible name, read once it holds `count`, in
   *  sorted order: an option that draws a count is named by its `aria-label` (`alice, 2 issues`),
   *  and one that draws none by its label's own text (`alice`), its check mark left out. */
  const headerFilterOptions = async (header: string, count: number): Promise<string[]> => {
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${header}(?:: \\d+ picked)?$`, "u"),
    });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map(
        (element) =>
          element.getAttribute("aria-label") ??
          Array.from(element.children)
            .filter((child) => child.getAttribute("aria-hidden") !== "true")
            .map((child) => child.textContent ?? "")
            .join("")
            .trim()
      )
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names.sort();
  };
  /** An option's value, with its count taken off where it draws one. */
  const optionValue = (name: string) => name.replace(/, \d+ issues?$/u, "");
  /** Each of `wanted` that `offered` does not hold as it is written. */
  const missing = (offered: readonly string[], wanted: readonly string[]) =>
    wanted.filter((want) => !offered.includes(want));
  // Each filter's counts that read the issues alone, once Nobody or lapsed is picked: Status lists
  // every status, so only the one these issues stand in is named, and Claimed by's Nobody counts
  // the issues no one claims. `claimed` is everything Claimed by offers with the list answered.
  const cases = [
    {
      key: "CORE",
      listed: [core.alice, core.bob, core.more],
      hidden: [] as string[],
      filters: [
        { count: 2, header: "Assignee", wanted: ["alice, 2 issues", "bob, 1 issue"] },
        { count: 2, header: "Priority", wanted: ["P1, 2 issues", "P2, 1 issue"] },
        { count: 9, header: "Status", wanted: ["Triage, 3 issues"] },
        { count: 1, header: "Labels", wanted: ["alpha, 2 issues"] },
        { count: 2, header: "Claimed by", wanted: ["Nobody, 3 issues"] },
      ],
      claimed: ["Nobody or lapsed, 3 issues", "Nobody, 3 issues"],
    },
    {
      key: "HELD",
      listed: [held.alice, held.bob],
      hidden: [held.more],
      filters: [
        { count: 2, header: "Assignee", wanted: ["alice, 1 issue", "bob, 1 issue"] },
        { count: 2, header: "Priority", wanted: ["P1, 1 issue", "P2, 1 issue"] },
        { count: 9, header: "Status", wanted: ["Triage, 2 issues"] },
        { count: 1, header: "Labels", wanted: ["alpha, 1 issue"] },
        { count: 3, header: "Claimed by", wanted: ["Nobody, 2 issues"] },
      ],
      claimed: [
        "Nobody or lapsed, 2 issues",
        "Nobody, 2 issues",
        "Running holder (for alice), 1 issue",
      ],
    },
  ];
  const wrong: string[] = [];
  for (const { key, listed, hidden, filters, claimed } of cases) {
    const at = `${key}, agent list refused`;
    refusing = true;
    await page.goto(`/projects/${key}/issues?unclaimed=1`);
    for (const row of listed) await expect(page.locator(`[data-issue-row="${row}"]`)).toBeVisible();
    for (const row of hidden)
      await expect(page.locator(`[data-issue-row="${row}"]`)).toHaveCount(0);
    // The page says the agent list's read failed after its retries, so every read these filters
    // count has answered or been refused.
    await expect(page.getByTestId("unclaimed-unchecked")).toBeVisible();
    for (const { count, header, wanted } of filters) {
      await expect
        .poll(async () => missing(await headerFilterOptions(header, count), wanted), {
          timeout: 5_000,
        })
        .toEqual([])
        .catch(() => undefined);
      // The poll above waited up to five seconds for these counts after every read they count had
      // answered, so this reads what the filter settled on.
      const offered = await headerFilterOptions(header, count);
      wrong.push(
        ...missing(offered, wanted).map(
          (want) =>
            `${at}: ${header} offers "${offered.find((name) => optionValue(name) === optionValue(want)) ?? "no such value"}" where it should offer "${want}"`
        )
      );
    }
    // Read after Claimed by's own poll above, so the panel has drawn whatever it will draw: every
    // value but Nobody counts what the refused agent list decides, and draws no count.
    const claimedOffered = await headerFilterOptions("Claimed by", claimed.length);
    wrong.push(
      ...claimedOffered
        .filter((name) => optionValue(name) !== "Nobody" && optionValue(name) !== name)
        .map((name) => `${at}: Claimed by offers "${name}", a count the refused agent list decides`)
    );
    // The control: with the agent list answered, the same page draws every count, the ones the
    // list decides right.
    served = 0;
    refusing = false;
    await page.goto(`/projects/${key}/issues?unclaimed=1`);
    for (const row of listed) await expect(page.locator(`[data-issue-row="${row}"]`)).toBeVisible();
    await expect.poll(() => served).toBeGreaterThan(0);
    for (const { count, header, wanted } of filters) {
      await expect
        .poll(async () => missing(await headerFilterOptions(header, count), wanted), {
          message: `${key}, agent list answered: ${header} draws ${wanted.join(" and ")}`,
        })
        .toEqual([]);
    }
    await expect
      .poll(() => headerFilterOptions("Claimed by", claimed.length), {
        message: `${key}, agent list answered: Claimed by draws ${claimed.join(" and ")}`,
      })
      .toEqual(claimed);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135, round 27 (the tester's), by the architect's ruling of 2026-11-25 04:59Z: a reader
// who has gone elsewhere while a Board move's dry run is out keeps the focus where they put it,
// and the body counts as the place the press left only when the press's own card went away.
// Measured at 7deb75af in builds Job infra-3135-f-h1-qgpz2 and at 5ae79d88 in
// infra-3135-f-b1-q7ttr, in Chromium, Firefox and WebKit at 1440x900 and 390x664: a child's card
// moved by Shift+L (moved at once) or a parent's by Shift+H (asked, then Unlink them), with the
// rule's dry run held and a Tab meanwhile that lands on the moving card's own link, ends on the
// body once the move lands at 7deb75af, where 5ae79d88 takes it to the moved card. The move
// draws the card in its new column, which takes the link away with the card, and the move sets
// no focus to follow it: it does so only while the focus is the card itself, or the question
// took it from the card itself (features/project/IssueBoard.tsx:644, 1cb2c03e), and a focus on a
// control inside the card is neither, so the effect at :551 has no plan to run.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): eight projects of three issues on the Board, and eight keyboard moves with held dry runs.
test("a Tab onto the moving card's own link while a Board move's dry run is out leaves the focus on the moved card once the move lands, as the same move with the focus left on the card does, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  // Every dry run of a status change sent while `holding` is held until the row lets it through.
  let holding = false;
  const held: Route[] = [];
  await page.route(/\/api\/v1\/issues\/[^/?]+\/status-change$/, async (route) => {
    const dryRun = (route.request().postData() ?? "").includes('"dry_run":true');
    if (!holding || !dryRun) return route.fallback();
    held.push(route);
  });
  /** What holds the keyboard focus: the moved card, the body, or the focused element's tag and
   *  name. */
  const focusName = (moved: string) =>
    page.evaluate((moved) => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) return "the body";
      if (focused.matches(`[data-board-card="${moved}"]`)) return "the moved card";
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      return `${focused.tagName.toLowerCase()} "${words}"`;
    }, moved);
  const got: string[] = [];
  const want: string[] = [];
  let made = 0;
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const move of ["moved at once", "asked, then Unlink them"] as const) {
      for (const press of ["no Tab", "a Tab onto the card's own link"] as const) {
        const where = `Board at ${size.width}x${size.height}, a move ${move}, with ${press} while the dry run is out`;
        // One project per case: a parent and a child under it, both in To do, so the child's
        // move into work is one the rule's dry run checks and moves at once, and the parent's
        // move to the backlog, with its child in To do, is one it asks about.
        made += 1;
        const key = `BT${String.fromCharCode(64 + made)}`;
        await createProject({ key, name: `Board Tab ${made}` });
        const parent = await createIssue({ project: key, title: `Parent ${key}` });
        const child = await createIssue({
          parent: parent.key,
          project: key,
          title: `Child ${key}`,
        });
        const other = await createIssue({ project: key, title: `Other ${key}` });
        for (const issue of [parent, child, other]) await patchIssue(issue.key, { status: "todo" });
        await openIssuesTab(page, key, "Board");
        const moved = move === "moved at once" ? child.key : parent.key;
        const card = page.locator(`[data-board-card="${moved}"]`);
        await card.focus();
        await expect(card).toBeFocused();
        holding = true;
        await page.keyboard.press(move === "moved at once" ? "Shift+L" : "Shift+H");
        await expect
          .poll(() => held.length, { message: `${where}: the move sent no dry run` })
          .toBeGreaterThan(0);
        if (press === "a Tab onto the card's own link") {
          await page.keyboard.press("Tab");
          await expect(
            card.locator("a").first(),
            `${where}: the Tab did not land on the card's own link`
          ).toBeFocused();
        }
        holding = false;
        for (const route of held.splice(0)) await route.continue();
        if (move === "asked, then Unlink them") {
          const question = page.getByRole("dialog", { name: `Move ${moved} to Backlog?` });
          // The choice is disabled until its own dry run answers, and the click waits for that.
          await question.getByRole("button", { name: "Unlink them" }).click();
          await expect(question).toHaveCount(0);
        }
        const column = move === "moved at once" ? "in_progress" : "backlog";
        await expect(
          page.locator(`[data-board-column="${column}"] [data-board-card="${moved}"]`),
          `${where}: the card did not move`
        ).toHaveCount(1);
        // The move's focus effect runs with the render that draws the card in its column, and
        // the question gives the focus back as it closes, so both are done after these frames.
        await nextFrames(page);
        await nextFrames(page);
        // One read is the answer: the move has landed and every hand-off of it has run.
        got.push(`${where}: the focus is on ${await focusName(moved)}`);
        want.push(`${where}: the focus is on the moved card`);
      }
    }
  }
  // The Tab put the focus inside the card the press moved; the move took that link away with the
  // card, so a focus left on the body there is the press's doing, and a keyboard reader would
  // start their next Tab from the top of the page.
  expect(got, got.join("; ")).toEqual(want);
});

// INFRA-3135, round 27 (the tester's), by the architect's rulings of 2026-11-25 05:3xZ (every
// landing of the selection bar's return takes the least scroll, the marked row's checkbox
// wherever it stands) and 13:01Z. Measured at 7deb75af in builds Jobs infra-3135-f-h1-qgpz2 and
// infra-3135-f-hx-dl7bw, and at 5ae79d88 in infra-3135-f-b1-q7ttr and infra-3135-f-bx-hxjsw: on the
// project List, with the row marked last near the end of a 90-row list and the table's box
// scrolled back to its top, so the table's window does not draw that row, Clear selection lands
// the focus on its checkbox at top 737 to bottom 750 in a box whose visible bottom is 736 at
// 1440x900, and at 499 to 512 under a visible bottom of 494 at 390x664, in Chromium and Firefox,
// at both heads: wholly out of sight. In those Jobs' Epic-filtered view WebKit centred it at 1440
// and varied at 390; in this row's own view it failed every run of infra-3135-f-red3-8hflf at
// 7deb75af (746 to 758 under 736, 510 to 522 under 460), six of six. The return goes to the table's own focus of a row it has not drawn
// (features/project/IssuesTable.tsx:324, `model.focusRow`), which scrolls the virtualizer to the
// row with `align: "auto"` (components/table-keys.ts:107, components/DataTable.tsx:304) and focuses
// the checkbox with no scroll once the row is drawn (components/table-keys.ts:134). The
// virtualizer measures its rows from the box's top (DataTable.tsx:286-294, no `scrollMargin`),
// while the box draws the sticky header row above them (:651), so a row aligned to the box's end
// stands one header row below it.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): ninety seeded issues, and two Clear selection presses read once the box settles.
test("Clear selection with the row marked last below the view and not drawn by the project List's table hands the focus to its checkbox wholly inside the box under the sticky header, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "LONG", name: "Long list" });
  for (let index = 0; index < 90; index++) {
    await createIssue({ project: "LONG", title: `Long row ${index}` });
  }
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  /** How the focused element stands against the window, the table's box and the box's sticky
   *  header row: its name, its rect, the part of the box the reader sees under the header, and
   *  whether it lies wholly inside that part. */
  const standing = () =>
    page.evaluate(() => {
      const round = (value: number) => Math.round(value * 10) / 10;
      const focused = document.activeElement;
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      if (!(focused instanceof HTMLElement) || focused === document.body || scroller === null) {
        return { text: "the focus is on the body", whole: false };
      }
      const own = focused.getBoundingClientRect();
      const frame = scroller.getBoundingClientRect();
      const head = scroller.querySelector("thead th")?.getBoundingClientRect().bottom ?? frame.top;
      const top = Math.max(0, frame.top, head);
      const bottom = Math.min(window.innerHeight, frame.bottom);
      const name = focused.getAttribute("aria-label") || focused.tagName.toLowerCase();
      return {
        text:
          `the focus is on "${name}" at top ${round(own.top)} to bottom ${round(own.bottom)}, ` +
          `where the box's visible part under its header runs from ${round(top)} to ${round(bottom)}`,
        whole: own.top >= top - 0.5 && own.bottom <= bottom + 0.5,
      };
    });
  const landings: { text: string; whole: boolean }[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `project List at ${size.width}x${size.height}`;
    await openIssuesTab(page, "LONG", "List");
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(12);
    // The window brings the table's box to just below the app's header where the page is taller
    // than the window, and the box scrolls to its end, where the row marked last is drawn.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = node.scrollHeight;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    // The rows read once the box has held still at its end; they only name the row marked.
    const order = await rows.evaluateAll((nodes) =>
      nodes
        .map((node) => ({
          key: node.getAttribute("data-issue-row") ?? "",
          top: node.getBoundingClientRect().top,
        }))
        .sort((one, other) => one.top - other.top)
        .map((row) => row.key)
    );
    const marked = order[order.length - 4] ?? "";
    const mark = page.getByRole("checkbox", { exact: true, name: `Select ${marked}` });
    await mark.focus();
    await page.keyboard.press("Space");
    await expect(mark).toBeChecked();
    await expect(bar).toContainText("1 selected");
    // Back to the box's top: the table's window no longer draws the row marked last.
    await box.evaluate((node) => {
      node.scrollTop = 0;
    });
    await settledScrollTop(box);
    await expect(
      page.locator(`[data-issue-row="${marked}"]`),
      `${where}: the table still draws the row marked last`
    ).toHaveCount(0);
    await bar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(bar).toHaveCount(0);
    await expect(
      mark,
      `${where}: the focus is not on the checkbox of the row marked last`
    ).toBeFocused();
    await settledScrollTop(box);
    await nextFrames(page);
    // One read is the answer: the bar has gone, the focus has landed on the row's checkbox and
    // the box has held still, so nothing moves the focus or the box after this.
    const landing = await standing();
    landings.push({ text: `${where}: ${landing.text}`, whole: landing.whole });
  }
  // A checkbox below the box's visible bottom is a landing the reader cannot see: the focus is
  // on the row they marked, out of their sight, with no scroll that shows it.
  expect(
    landings.map((landing) => landing.whole),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 27 (the tester's), by the architect's rulings of 2026-11-25 05:3xZ (every
// landing of the selection bar's return takes the least scroll, and a landing partly in sight is
// shown whole at its nearest edge) and 13:21Z, which counts this row. Measured at 7deb75af in
// builds Jobs infra-3135-f-h1-qgpz2 and infra-3135-f-hx-dl7bw, and at 5ae79d88 in
// infra-3135-f-b1-q7ttr and infra-3135-f-bx-hxjsw, in Chromium, Firefox and WebKit at 1440x900
// (the window at its top) and 390x664: under the project List's Epic filter, with the mark of the
// row marked last cut by 30% of its height at the table box's bottom edge or under its sticky
// header, Take out of epic lands the focus on the checkbox of the row now at its place and shows
// it whole at that edge (7deb75af; 5ae79d88 left it cut). Cut by 70%, the same row at the place
// is passed over: at the bottom edge the focus goes to the checkbox of the row before it, and
// under the header to the table itself, with no scroll. The hand-off takes a row's mark only
// where the centre of the mark is in the window and what the page draws there is that row
// (components/SelectionBar.tsx:341-343, `markShown`); the sticky header, or the box's clipped
// edge, stands over the centre of a mark cut by 70%, so the row at the place is passed over for
// the nearest row before it in sight (:278) and else the list's own stop (:155).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): sixty seeded issues, and eight Take out of epic presses read once the box settles.
test("Take out of epic with the row now at the place of the row marked last cut by 70% at the table box's bottom edge or under its sticky header hands the focus to that row's checkbox at its nearest edge, as a cut of 30% does, on the project List at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CUT", name: "Cut" });
  const epic = await createIssue({ project: "CUT", title: "Cut epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 60; index++) {
    await createIssue({ parent: epic.key, project: "CUT", title: `Cut row ${index}` });
  }
  const view = `/projects/CUT/issues?f.epic=${encodeURIComponent(`${epic.key} · Cut epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees
   *  whole: inside the window and the box, below its sticky header. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const head = scroller?.querySelector("thead th")?.getBoundingClientRect().bottom ?? 0;
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        return (
          own.top >= Math.max(0, frame.top, head) &&
          own.bottom <= Math.min(window.innerHeight, frame.bottom)
        );
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** Where the focus is against the edge `edge` names: the checkbox of the row `key` names
   *  shown whole with that edge of it at most 2 px from the same edge of the box's visible part
   *  under its header (`ok`), and in words what holds the focus, its rect and that part. */
  const landingOn = (key: string, edge: "bottom" | "header") =>
    page.evaluate(
      ([key, edge]) => {
        const round = (value: number) => Math.round(value * 10) / 10;
        const focused = document.activeElement;
        const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
        if (!(focused instanceof HTMLElement) || focused === document.body || scroller === null) {
          return { ok: false, text: "the focus is on the body" };
        }
        const own = focused.getBoundingClientRect();
        const frame = scroller.getBoundingClientRect();
        const head =
          scroller.querySelector("thead th")?.getBoundingClientRect().bottom ?? frame.top;
        const top = Math.max(0, frame.top, head);
        const bottom = Math.min(window.innerHeight, frame.bottom);
        const name = focused.getAttribute("aria-label") || focused.tagName.toLowerCase();
        const whole = own.top >= top - 0.5 && own.bottom <= bottom + 0.5;
        const gap = edge === "bottom" ? bottom - own.bottom : own.top - top;
        return {
          ok: name === `Select ${key}` && whole && gap <= 2,
          text:
            `the focus is on "${name}" at top ${round(own.top)} to bottom ${round(own.bottom)}, ` +
            `in a visible part from ${round(top)} to ${round(bottom)}`,
        };
      },
      [key, edge] as const
    );
  const landings: { ok: boolean; text: string }[] = [];
  for (const size of [
    { height: 900, lift: false, width: 1440 },
    { height: 664, lift: true, width: 390 },
  ]) {
    await page.setViewportSize({ height: size.height, width: size.width });
    for (const [edge, part] of [
      ["bottom", 0.3],
      ["bottom", 0.7],
      ["header", 0.3],
      ["header", 0.7],
    ] as const) {
      const where = `project List at ${size.width}x${size.height} with the mark of the row marked last cut by ${part * 100}% ${edge === "bottom" ? "at the box's bottom edge" : "under its sticky header"}`;
      await page.goto(view);
      await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(12);
      // At 390 the window brings the table's box to just below the app's header; at 1440 the box
      // stands whole in the window, which stays at its top. The box scrolls half way down its rows.
      await box.evaluate((node, lift) => {
        if (lift) window.scrollBy(0, node.getBoundingClientRect().top - 60);
        node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
      }, size.lift);
      await settledScrollTop(page);
      await settledScrollTop(box);
      await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
      // The rows read once the box has held still and the poll above found them in sight; they
      // only name the two rows marked below, the middle one in sight and the one after it, and
      // the row after those two, which takes their place once they have gone.
      const { order, shown } = await drawn();
      const at = order.indexOf(shown[Math.floor(shown.length / 2)] ?? "");
      const upper = order[at] ?? "";
      const lower = order[at + 1] ?? "";
      const after = order[at + 2] ?? "";
      // The lower row first, so the row marked last is the upper one, whose place the focus takes.
      for (const key of [lower, upper]) {
        await markOf(key).focus();
        await page.keyboard.press("Space");
        await expect(markOf(key)).toBeChecked();
      }
      await expect(bar).toContainText("2 selected");
      await markOf(upper).evaluate(
        (mark, [edge, part]) => {
          const scroller = mark.closest<HTMLElement>('[data-testid="issues-table-scroll"]');
          const own = mark.getBoundingClientRect();
          const head = scroller?.querySelector("thead th")?.getBoundingClientRect().bottom ?? 0;
          const bottom = Math.min(
            window.innerHeight,
            scroller?.getBoundingClientRect().bottom ?? 0
          );
          scroller?.scrollBy(
            0,
            edge === "bottom"
              ? own.bottom - (bottom + own.height * part)
              : own.top - (head - own.height * part)
          );
        },
        [edge, part] as const
      );
      await settledScrollTop(box);
      await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
      await page.keyboard.press("Enter");
      await expect(bar).toHaveCount(0);
      await expect(markOf(upper)).toHaveCount(0);
      // The bar's hand-off runs after the commit that took the bar away, and its scroll lands
      // in the frames after it, so both are done once the box has held still.
      await nextFrames(page);
      await settledScrollTop(box);
      // One read is the answer: the writes have landed, the bar has gone and every hand-off of the
      // press has run, so nothing moves the focus or the box after this.
      const landing = await landingOn(after, edge);
      landings.push({
        ok: landing.ok,
        text: `${where}: ${landing.text}, want "Select ${after}" whole at that edge`,
      });
    }
  }
  // The row now at the place stands partly in the reader's sight; passing it over puts the focus
  // on a row they did not mark the place of, or on the table with no row in hand.
  expect(
    landings.map((landing) => landing.ok),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 27 (the tester's), by the architect's rulings of 2026-11-25 05:3xZ (every
// landing of the selection bar's return takes the least scroll, to the nearest edge) and 13:21Z,
// which counts this row: the least scroll shows the checkbox whole by the least distance, with no
// room added for the foot the bar left. Measured at 7deb75af in builds Jobs infra-3135-f-h1-qgpz2
// (the List) and infra-3135-f-h2-n8pwt (the Inbox), in Chromium, Firefox and WebKit at 390x664:
// with the checkbox of the row marked last cut by the window's bottom edge, Clear selection
// scrolls the window 80 px past the cut - 86 px for a cut of 6 px on the project List, 85 px for
// 4.5 px and 92 px for 11.5 px on the Inbox - and leaves the checkbox 80 px above the window's
// bottom. Cut by the window's top edge, the same press scrolls the cut alone. 5ae79d88
// (infra-3135-f-b1-q7ttr, infra-3135-f-b2-jwrlv) centred the checkbox instead, 372 px and 369 px.
// The landing shows the checkbox with `scrollIntoView({ block: "nearest" })`
// (components/focus-place.ts:528, through `landWhole` from components/SelectionBar.tsx:314), and
// the engine keeps clear of the root's `scroll-padding-bottom: 5rem`, which main sets below
// 1280 px for the review sheet at the foot of the screen (web/src/styles.css:415).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): forty issues and thirty asks seeded, and three Clear selection presses read once the window settles.
test("at 390 the selection bar's return to the row marked last, with its checkbox cut by the window's bottom edge, scrolls the window no further than the cut, on the project List and the Inbox, as it does with the checkbox cut by the window's top edge @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 664, width: 390 });
  await createProject({ key: "WIN", name: "Window" });
  for (let index = 0; index < 40; index++) {
    await createIssue({ project: "WIN", title: `Window row ${index}` });
  }
  await createProject({ key: "ASKS", name: "Asks" });
  for (let index = 0; index < 30; index++) {
    const owner = await createIssue({ force: true, project: "ASKS", title: `Ask owner ${index}` });
    await createAsk(owner.key, { question: `Ask ${index}: which way?` }, SURFACE_SESSION);
  }
  /** The rect of the element `selector` finds and the window's scroll. */
  const standing = (selector: string) =>
    page.evaluate((selector) => {
      const own = document.querySelector(selector)?.getBoundingClientRect();
      return {
        bottom: own?.bottom ?? Number.NaN,
        scrollY: window.scrollY,
        top: own?.top ?? Number.NaN,
      };
    }, selector);
  /** Whether the focused element is the one `selector` finds. */
  const focusedIs = (selector: string) =>
    page.evaluate(
      (selector) => document.activeElement === document.querySelector(selector),
      selector
    );
  const landings: { ok: boolean; text: string }[] = [];
  /** Presses Clear selection in `bar` and reads how far the checkbox `selector` finds moved,
   *  against the cut it stood at before the press. */
  const clearAndRead = async (
    where: string,
    bar: Locator,
    selector: string,
    edge: "bottom" | "top"
  ) => {
    const before = await standing(selector);
    const cut = edge === "bottom" ? before.bottom - 664 : -before.top;
    await bar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(bar).toHaveCount(0);
    await expect
      .poll(() => focusedIs(selector), { message: `${where}: the focus is not on the checkbox` })
      .toBe(true);
    await settledScrollTop(page);
    await nextFrames(page);
    // One read is the answer: the bar has gone, the focus has landed on the checkbox and the
    // window has held still, so nothing moves the focus or the window after this.
    const after = await standing(selector);
    const moved = Math.abs(after.top - before.top);
    const scrolled = Math.round(after.scrollY - before.scrollY);
    landings.push({
      ok: after.top >= -1 && after.bottom <= 665 && moved <= cut + 1,
      text:
        `${where}: the checkbox was cut by ${Math.round(cut * 10) / 10} px, and the window ` +
        `scrolled ${scrolled} px, leaving it at top ${Math.round(after.top * 10) / 10} to bottom ` +
        `${Math.round(after.bottom * 10) / 10}`,
    });
  };

  // The project List: the table's box stands below the window's middle and runs past its bottom,
  // and the box scrolls the checkbox of the row marked last to half its height below that bottom.
  const issueBar = page.getByRole("toolbar", { name: "Selected issues" });
  const box = page.getByTestId("issues-table-scroll");
  const issueRows = page.locator("[data-issue-row]");
  await openIssuesTab(page, "WIN", "List");
  await expect.poll(() => issueRows.count()).toBeGreaterThanOrEqual(12);
  await box.evaluate((node) => {
    window.scrollBy(0, node.getBoundingClientRect().top - 60);
    node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
  });
  await settledScrollTop(page);
  await settledScrollTop(box);
  // The rows read once the box has held still; they only name the row marked, one in the middle.
  const order = await issueRows.evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute("data-issue-row") ?? "")
  );
  const issue = order[Math.floor(order.length / 2)] ?? "";
  const issueMark = page.getByRole("checkbox", { exact: true, name: `Select ${issue}` });
  await issueMark.focus();
  await page.keyboard.press("Space");
  await expect(issueMark).toBeChecked();
  await issueMark.evaluate((mark) => {
    const scroller = mark.closest<HTMLElement>('[data-testid="issues-table-scroll"]');
    window.scrollBy(0, (scroller?.getBoundingClientRect().top ?? 0) - (window.innerHeight - 260));
    const own = mark.getBoundingClientRect();
    scroller?.scrollBy(0, own.top - (window.innerHeight - own.height / 2));
  });
  await settledScrollTop(page);
  await settledScrollTop(box);
  await clearAndRead(
    "project List with the checkbox cut by the window's bottom edge",
    issueBar,
    `[data-issue-row="${issue}"] input[type="checkbox"]`,
    "bottom"
  );

  // The Inbox: the window scrolls its rows, and the checkbox of the row marked last stands cut by
  // 30% of its height at the window's bottom edge, then at its top edge.
  const rowBar = page.getByRole("toolbar", { name: "Selected rows" });
  for (const edge of ["bottom", "top"] as const) {
    await page.goto("/?group=none");
    await expect.poll(() => page.locator("[data-inbox-row]").count()).toBeGreaterThanOrEqual(20);
    // The rows read once the poll above found the list drawn; they only name the rows marked.
    const ids = await page
      .locator("[data-inbox-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
    const upper = ids[12] ?? "";
    const lower = ids[13] ?? "";
    // The lower row first, so the row marked last is the upper one.
    for (const id of [lower, upper]) await inboxRow(page, id).getByRole("checkbox").first().check();
    await expect(rowBar).toBeVisible();
    await inboxRow(page, upper)
      .getByRole("checkbox")
      .first()
      .evaluate((mark, edge) => {
        const own = mark.getBoundingClientRect();
        window.scrollBy(
          0,
          edge === "bottom"
            ? own.bottom - (window.innerHeight + own.height * 0.3)
            : own.top + own.height * 0.3
        );
      }, edge);
    await settledScrollTop(page);
    await clearAndRead(
      `Inbox with the checkbox cut by the window's ${edge} edge`,
      rowBar,
      `[data-inbox-row="${upper}"] input[type="checkbox"]`,
      edge
    );
  }
  // A scroll past the cut moves the reader's rows further than showing the checkbox needs, here
  // by the room main keeps for a foot sheet that neither page draws.
  expect(
    landings.map((landing) => landing.ok),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 28 (the tester's), by the architect's rulings of 2026-11-25 13:01Z ("a count
// is drawn once every read it counts has answered; a read that a count does not count never hides
// it") and 17:5xZ (F5: while a Claimed-by narrowing that needs the agent registry is on, and
// before the registry's first read answers or settles on its error, no column draws a count;
// after that the per-value rule applies). Measured at 5a444912 in builds Job
// infra-3135-f8-h1-sn5q6, in Chromium, Firefox and WebKit at 1440x900: with the agent list
// (`GET /api/v1/agents`) refused every time and Nobody or lapsed picked, the header filters draw
// their counts once the page says it could not check which claims have lapsed, and lose them
// again while each later poll of the list is out: Assignee offers "alice" where it offered
// "alice, 2 issues" a moment before. The list has never answered, so each poll's read puts the
// registry's query back to pending (`isError` false, `known` false), and
// `registryPending = wantsFree && !registry.known && !registry.isError` (IssuesTable.tsx:505)
// then hides every column's count, where the registry's first read settled long before. The rows
// the list holds are the same before and during that read: every claim counts as held while no
// read has answered. The control: before the poll, with the list's first read settled on its
// error, the same filters draw these counts.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-28 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): one project on the List, read with the agent list refused, and one poll of the list held.
test("with the agent list refused, the project List's header filters keep each count the list does not decide while a later poll of the list is out @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const alice = await createIssue({ project: "CORE", title: "Harbour crane" });
  await patchIssue(alice.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
  const bob = await createIssue({ project: "CORE", title: "Signal tower" });
  await patchIssue(bob.key, { assignee: "bob", priority: 2 });
  const more = await createIssue({ project: "CORE", title: "Ferry ramp" });
  await patchIssue(more.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
  // The agent list answers 500 while `holding` is false, and is held, unanswered, while it is true.
  let holding = false;
  const held: Route[] = [];
  await page.route(
    (url) => url.pathname === "/api/v1/agents",
    async (route) => {
      if (holding) {
        held.push(route);
        return;
      }
      await route.fulfill({
        body: '{"error":{"code":"INTERNAL","message":"agent list refused"}}',
        contentType: "application/json",
        status: 500,
      });
    }
  );
  /** Each option of one header filter, by its accessible name, read once it holds `count`, in
   *  sorted order: an option that draws a count is named by its `aria-label` (`alice, 2 issues`),
   *  and one that draws none by its label's own text (`alice`), its check mark left out. */
  const headerFilterOptions = async (header: string, count: number): Promise<string[]> => {
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${header}(?:: \\d+ picked)?$`, "u"),
    });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map(
        (element) =>
          element.getAttribute("aria-label") ??
          Array.from(element.children)
            .filter((child) => child.getAttribute("aria-hidden") !== "true")
            .map((child) => child.textContent ?? "")
            .join("")
            .trim()
      )
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names.sort();
  };
  // The counts that read the issues alone, which answered.
  const filters = [
    { count: 2, header: "Assignee", wanted: ["alice, 2 issues", "bob, 1 issue"] },
    { count: 2, header: "Priority", wanted: ["P1, 2 issues", "P2, 1 issue"] },
    { count: 1, header: "Labels", wanted: ["alpha, 2 issues"] },
  ];
  /** What each filter offers that is not one of its wanted counts, written for a message. */
  const lacking = async (where: string): Promise<string[]> => {
    const wrong: string[] = [];
    for (const { count, header, wanted } of filters) {
      const offered = await headerFilterOptions(header, count);
      for (const want of wanted)
        if (!offered.includes(want))
          wrong.push(
            `${where}: ${header} offers ${offered.join(" | ")} where it should hold "${want}"`
          );
    }
    return wrong;
  };
  await page.goto("/projects/CORE/issues?unclaimed=1");
  for (const key of [alice.key, bob.key, more.key])
    await expect(page.locator(`[data-issue-row="${key}"]`)).toBeVisible();
  // The control: the list's first read has settled on its error, as the page says, and every
  // count the list does not decide is drawn.
  await expect(page.getByTestId("unclaimed-unchecked")).toBeVisible();
  await expect.poll(() => lacking("first read refused"), { timeout: 10_000 }).toEqual([]);
  // The shell's next poll of the list (every 15 s, `AGENT_REGISTRY_POLL_MS`) is held: its read is
  // out while the filters are read again.
  holding = true;
  await expect
    .poll(() => held.length, { message: "a later poll of the agent list", timeout: 20_000 })
    .toBeGreaterThan(0);
  const whileOut = await lacking("a later poll held");
  holding = false;
  for (const route of held.splice(0))
    await route
      .fulfill({
        body: '{"error":{"code":"INTERNAL","message":"agent list refused"}}',
        contentType: "application/json",
        status: 500,
      })
      .catch(() => undefined);
  expect(whileOut, whileOut.join("; ")).toEqual([]);
});

// INFRA-3135, round 28 (the tester's), by the architect's ruling of 2026-11-25 05:3xZ (every
// landing of the selection bar's return takes the least scroll to its nearest edge, and a landing
// partly in sight is shown whole). Measured at 5a444912 in builds Job infra-3135-f8-h3-wwf5b and
// at 4f1ad776 in infra-3135-f8-b3-4prbk: with two Inbox threads marked and the checkbox of the
// row marked last cut by 30% at the bottom edge of what the reader sees (the list's box at
// 1440x900, the window at 390x664), the bar's Dismiss lands the focus on the checkbox of the row
// now at the place in Chromium and Firefox, shown whole by a scroll of the cut alone (5 px), and
// 4f1ad776 shows it whole in all three engines. In WebKit at 5a444912 the box (1440) or the window (390)
// instead moves back by the rows the Dismiss took away - 162 px in the box, 940 px in the window
// - and the focus is left on that checkbox wholly below the reader's sight (its top at 1027 px
// under a box that ends at 876, at 1593 px in a window 664 px high), with no scroll to show it:
// `ViewportAnchor.showFocused` (features/shell/ViewportAnchor.tsx:679) leaves a control wholly out
// of sight where it is, and by the time it reads the landing, the anchoring has moved it out.
// The race is WebKit's: this row failed 4 of 6 WebKit repeats at 5a444912 (builds Jobs
// infra-3135-infra-3135-red28-krw9f and -final28-wglpb), both widths together in a failing run,
// and it passes in every Chromium and Firefox run there, where the landing is right; at 4f1ad776
// WebKit's landing at 1440 was whole in 3 of 3 probe runs. So a pass at a fixed head counts only
// over 8 or more WebKit repeats (the architect, 2026-11-25 21:20Z).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-28 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): eighty threads seeded, and two Dismiss presses read once the scroll settles.
test("the Inbox bar's Dismiss with the row marked last cut by 30% at the bottom of the reader's sight shows the checkbox of the row now at the place whole by the least scroll, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 80; index++) {
    const issue = await createIssue({ project: "CORE", title: `Thread issue ${index}` });
    await seedCommentThread(
      issue.key,
      `Thread ${index}: when does it run?`,
      `At 0${index % 10}:00.`
    );
  }
  const bar = page.getByRole("toolbar", { name: "Selected rows" });
  /** The part of the window the reader sees the Inbox's rows in: the list's own box where it
   *  scrolls (a desktop), else the window; and that scroller's scroll. */
  const readerSight = () =>
    page.evaluate(() => {
      const box = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
      const scrolls =
        box !== null &&
        box.scrollHeight > box.clientHeight + 1 &&
        getComputedStyle(box).overflowY !== "visible";
      const frame = scrolls && box !== null ? box.getBoundingClientRect() : null;
      return {
        bottom: Math.min(window.innerHeight, frame?.bottom ?? window.innerHeight),
        scroll: scrolls && box !== null ? box.scrollTop : window.scrollY,
        top: Math.max(0, frame?.top ?? 0),
      };
    });
  /** The rect of the checkbox of the Inbox row `id`. */
  const checkboxRect = (id: string) =>
    page.evaluate((id) => {
      const own = document
        .querySelector(`[data-inbox-row="${id}"] input[type="checkbox"]`)
        ?.getBoundingClientRect();
      return {
        bottom: own?.bottom ?? Number.NaN,
        height: own?.height ?? Number.NaN,
        top: own?.top ?? Number.NaN,
      };
    }, id);
  const landings: { ok: boolean; text: string }[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    const where = `Inbox at ${size.width}x${size.height}`;
    await page.setViewportSize(size);
    await page.goto("/?group=none");
    await expect.poll(() => page.locator("[data-inbox-row]").count()).toBeGreaterThanOrEqual(20);
    // The rows read once the poll above found the list drawn; they only name the rows marked and
    // the one after them.
    const ids = await page
      .locator("[data-inbox-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
    const upper = ids[12] ?? "";
    const lower = ids[13] ?? "";
    const after = ids[14] ?? "";
    // The lower row first, so the row marked last is the upper one.
    for (const id of [lower, upper]) await inboxRow(page, id).getByRole("checkbox").first().check();
    await expect(bar).toBeVisible();
    // The upper row's checkbox is cut by 30% of its height at the bottom of the reader's sight.
    const view = await readerSight();
    const own = await checkboxRect(upper);
    const by = own.bottom - (view.bottom + own.height * 0.3);
    await page.evaluate((by) => {
      const box = document.querySelector<HTMLElement>('[data-testid="inbox-list"]');
      if (
        box !== null &&
        box.scrollHeight > box.clientHeight + 1 &&
        getComputedStyle(box).overflowY !== "visible"
      )
        box.scrollBy(0, by);
      else window.scrollBy(0, by);
    }, by);
    await settledScrollTop(page);
    await settledScrollTop(page.getByTestId("inbox-list"));
    await nextFrames(page);
    const before = await readerSight();
    const placed = await checkboxRect(upper);
    const cut = placed.bottom - before.bottom;
    await bar.getByRole("button", { name: /selected threads$/u }).focus();
    await page.keyboard.press("Enter");
    await expect(page.locator(`[data-inbox-row="${upper}"]`)).toHaveCount(0, { timeout: 15_000 });
    await expect(page.locator(`[data-inbox-row="${lower}"]`)).toHaveCount(0, { timeout: 15_000 });
    await expect
      .poll(
        () =>
          page.evaluate(
            (id) =>
              document.activeElement ===
              document.querySelector(`[data-inbox-row="${id}"] input[type="checkbox"]`),
            after
          ),
        { message: `${where}: the focus is not on the checkbox of the row now at the place` }
      )
      .toBe(true);
    await settledScrollTop(page);
    await settledScrollTop(page.getByTestId("inbox-list"));
    await nextFrames(page);
    // One read is the answer: both rows have gone, the focus has landed and the scroll has held
    // still, so nothing moves the focus or the rows after this.
    const now = await readerSight();
    // Read in the same settled moment as the sight above, so it is where the press left it.
    const landed = await checkboxRect(after);
    const scrolled = Math.round(now.scroll - before.scroll);
    landings.push({
      ok:
        landed.top >= now.top - 1 &&
        landed.bottom <= now.bottom + 1 &&
        Math.abs(scrolled) <= cut + 1,
      text:
        `${where}: the checkbox of the row marked last was cut by ${Math.round(cut * 10) / 10} px, ` +
        `the rows scrolled ${scrolled} px, and the checkbox now at the place stands at top ` +
        `${Math.round(landed.top)} to bottom ${Math.round(landed.bottom)} in a sight from ` +
        `${Math.round(now.top)} to ${Math.round(now.bottom)}`,
    });
  }
  expect(
    landings.map((landing) => landing.ok),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 28 (the tester's), by the architect's rulings of 2026-11-25 05:1xZ (the rule
// reads the checkbox, the control that takes the focus, and not its row), 05:3xZ (every landing of
// the selection bar's return takes the least scroll to its nearest edge, and a landing partly in
// sight is shown whole) and 20:3xZ, which counts this row at 390x664, a size the spec names.
// Measured at 5a444912 in builds Jobs infra-3135-f8-h3-wwf5b and
// infra-3135-infra-3135-probe28c-cqhhk and at 4f1ad776 in infra-3135-f8-b3-4prbk, in Chromium and
// WebKit at 390x664 (Firefox lands whole): on the project List of an epic's ninety issues, with
// the table's box standing 60 px below the window's top, the two rows marked last near the end of
// the list and the box scrolled back to its top, so neither is drawn, the box ends at y 494 while
// the selection bar stands, and CORE-4's checkbox stands at 483 to 496, cut by 2 px at that edge.
// Take out of epic lands the focus on that checkbox and scrolls the box 3 px, the least scroll that
// shows it whole in the box as it stood. In the frame the bar goes, the box's bottom edge moves up
// 20 px, to 474, while the rows move by the 3 px scroll alone, and the checkbox, now at 480 to 493, is left
// wholly below the box's edge. The landing measures the box before the bar's going has changed it,
// and does not read the checkbox again once it has.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-28 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): ninety issues of an epic seeded, and one Take out of epic read once the box settles.
test("at 390 Take out of epic with the rows marked last far below the view and not drawn lands the focus on a checkbox wholly in the box's sight, or on the table, by no more than the least scroll @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 664, width: 390 });
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 90; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const box = page.getByTestId("issues-table-scroll");
  await page.goto(`/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`);
  await expect.poll(() => page.locator("[data-issue-row]").count()).toBeGreaterThanOrEqual(12);
  // The box's top 60 px below the window's top, and the box scrolled to its end, where it draws
  // the rows near the end of the list.
  await box.evaluate((node) => {
    window.scrollBy(0, node.getBoundingClientRect().top - 60);
    node.scrollTop = node.scrollHeight;
  });
  await settledScrollTop(box);
  await settledScrollTop(page);
  await nextFrames(page);
  const order = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>("[data-issue-row]")]
      .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
      .map((row) => row.getAttribute("data-issue-row") ?? "")
  );
  const upper = order[order.length - 4] ?? "";
  const lower = order[order.length - 3] ?? "";
  // The lower row first, so the row marked last is the upper one.
  for (const key of [lower, upper]) {
    const mark = page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
    await mark.focus();
    await page.keyboard.press("Space");
    await expect(mark).toBeChecked();
  }
  await expect(bar).toContainText("2 selected");
  await box.evaluate((node) => {
    node.scrollTop = 0;
  });
  await settledScrollTop(box);
  await settledScrollTop(page);
  await nextFrames(page);
  await expect(page.locator(`[data-issue-row="${upper}"]`)).toHaveCount(0);
  /** The part of the window the box shows its rows in, below its sticky head; the box's and the
   *  window's scroll; and the rect of each drawn row's checkbox, by the row's key. */
  const takeOutView = () =>
    page.evaluate(() => {
      const node = document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');
      const frame = node?.getBoundingClientRect();
      const head = node?.querySelector("thead")?.getBoundingClientRect();
      const marks: Record<string, { bottom: number; top: number }> = {};
      for (const row of document.querySelectorAll<HTMLElement>("[data-issue-row]")) {
        const own = row.querySelector('input[type="checkbox"]')?.getBoundingClientRect();
        if (own !== undefined)
          marks[row.getAttribute("data-issue-row") ?? ""] = { bottom: own.bottom, top: own.top };
      }
      const focused = document.activeElement;
      return {
        bottom: Math.min(window.innerHeight, frame?.bottom ?? 0),
        boxScroll: node?.scrollTop ?? 0,
        focusedRow:
          focused instanceof HTMLInputElement && focused.type === "checkbox"
            ? (focused.closest("[data-issue-row]")?.getAttribute("data-issue-row") ?? null)
            : null,
        onTable: focused !== null && focused === node?.querySelector("table"),
        focusedName: focused?.getAttribute("aria-label") ?? focused?.tagName ?? "nothing",
        marks,
        top: Math.max(0, head?.bottom ?? frame?.top ?? 0),
        windowScroll: window.scrollY,
      };
    });
  const before = await takeOutView();
  await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
  await page.keyboard.press("Enter");
  await expect(bar).toHaveCount(0, { timeout: 15_000 });
  await settledScrollTop(box);
  await settledScrollTop(page);
  await nextFrames(page);
  // One read is the answer: the bar has gone and the box and the window have held still, so
  // nothing moves the focus or the rows after this.
  const now = await takeOutView();
  const scrolled =
    Math.abs(now.boxScroll - before.boxScroll) + Math.abs(now.windowScroll - before.windowScroll);
  const landed = now.focusedRow === null ? undefined : now.marks[now.focusedRow];
  const was = now.focusedRow === null ? undefined : before.marks[now.focusedRow];
  // The least scroll that shows the landed checkbox whole, from where it stood before the press.
  const least =
    was === undefined ? 0 : Math.max(0, was.bottom - before.bottom, before.top - was.top);
  const where =
    `the focus is on ${now.focusedName}` +
    (landed === undefined
      ? ""
      : ` at top ${Math.round(landed.top)} to bottom ${Math.round(landed.bottom)}, in the box's sight from ${Math.round(now.top)} to ${Math.round(now.bottom)}`) +
    `, and the box and the window scrolled ${Math.round(scrolled)} px where the least is ${Math.round(least)} px`;
  const whole =
    landed !== undefined && landed.top >= now.top - 1 && landed.bottom <= now.bottom + 1;
  expect(now.focusedRow !== null || now.onTable, where).toBe(true);
  expect(now.onTable || (whole && scrolled <= least + 1), where).toBe(true);
});

// INFRA-3135, round 28 (the tester's), by the architect's rulings of 2026-11-25 05:3xZ (every
// landing of the selection bar's return takes the least scroll to its nearest edge, and a landing
// partly in sight is shown whole) and 20:3xZ, which counts this row: it is the 70%-cut rule of the
// round-27 row on Take out of epic (eba5a52d), on the Inbox's Snooze, since the shared bar carries
// one rule on both pages. Measured at 5a444912 in builds Job infra-3135-f8-h3-wwf5b and at
// 4f1ad776 in infra-3135-f8-b3-4prbk, at 1440x900: with two asks marked and the checkbox of the
// row marked last cut by 70% at the bottom edge of the Inbox list's box, the bar's Snooze
// (Tomorrow) lands the focus on the checkbox of the row now at the place in Chromium and Firefox,
// shown whole by a scroll of the cut (11 px). In WebKit it leaves the focus on the page body.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-28 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): thirty asks seeded, and one Snooze read once the list's box settles.
test("the Inbox bar's Snooze with the row marked last cut by 70% at the list's bottom edge shows the checkbox of the row now at the place whole by the least scroll @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "ASKS", name: "Asks" });
  for (let index = 0; index < 30; index++) {
    const owner = await createIssue({ force: true, project: "ASKS", title: `Ask owner ${index}` });
    await createAsk(owner.key, { question: `Ask ${index}: which way?` }, SURFACE_SESSION);
  }
  const bar = page.getByRole("toolbar", { name: "Selected rows" });
  const list = page.getByTestId("inbox-list");
  /** The part of the window the list's box shows its rows in, and the box's scroll. */
  const readerSight = () =>
    list.evaluate((box) => {
      const frame = box.getBoundingClientRect();
      return {
        bottom: Math.min(window.innerHeight, frame.bottom),
        scroll: box.scrollTop,
        top: Math.max(0, frame.top),
      };
    });
  /** The rect of the checkbox of the Inbox row `id`. */
  const checkboxRect = (id: string) =>
    page.evaluate((id) => {
      const own = document
        .querySelector(`[data-inbox-row="${id}"] input[type="checkbox"]`)
        ?.getBoundingClientRect();
      return {
        bottom: own?.bottom ?? Number.NaN,
        height: own?.height ?? Number.NaN,
        top: own?.top ?? Number.NaN,
      };
    }, id);
  await page.goto("/?group=none");
  await expect.poll(() => page.locator("[data-inbox-row]").count()).toBeGreaterThanOrEqual(20);
  // The rows read once the poll above found the list drawn; they only name the rows marked and
  // the one after them.
  const ids = await page
    .locator("[data-inbox-row]")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
  const upper = ids[12] ?? "";
  const lower = ids[13] ?? "";
  const after = ids[14] ?? "";
  // The lower row first, so the row marked last is the upper one.
  for (const id of [lower, upper]) await inboxRow(page, id).getByRole("checkbox").first().check();
  await expect(bar).toBeVisible();
  // The upper row's checkbox is cut by 70% of its height at the bottom edge of the list's box.
  const view = await readerSight();
  const own = await checkboxRect(upper);
  await list.evaluate(
    (box, by) => box.scrollBy(0, by),
    own.bottom - (view.bottom + own.height * 0.7)
  );
  await settledScrollTop(list);
  await nextFrames(page);
  const before = await readerSight();
  const placed = await checkboxRect(upper);
  const cut = placed.bottom - before.bottom;
  const trigger = listTrigger(bar, "Snooze selected asks");
  await trigger.focus();
  await page.keyboard.press("Enter");
  await page.keyboard.type("tom");
  await expect(listOf(trigger).getByRole("option")).toHaveText(["Tomorrow"]);
  await page.keyboard.press("Enter");
  await expect(page.locator(`[data-inbox-row="${upper}"]`)).toHaveCount(0, { timeout: 15_000 });
  await expect(page.locator(`[data-inbox-row="${lower}"]`)).toHaveCount(0, { timeout: 15_000 });
  await settledScrollTop(list);
  await settledScrollTop(page);
  await nextFrames(page);
  // One read is the answer: both rows have gone and the box has held still, so nothing moves the
  // focus or the rows after this.
  const now = await readerSight();
  // Read in the same settled moment as the sight above, so it is where the press left it.
  const landed = await checkboxRect(after);
  // The focus read once, in that same settled moment: nothing moves it after the box holds still.
  const focused = await page.evaluate(
    (id) => ({
      name:
        document.activeElement?.getAttribute("aria-label") ??
        document.activeElement?.tagName ??
        "nothing",
      onMark:
        document.activeElement ===
        document.querySelector(`[data-inbox-row="${id}"] input[type="checkbox"]`),
    }),
    after
  );
  const scrolled = Math.round(now.scroll - before.scroll);
  const text =
    `the checkbox of the row marked last was cut by ${Math.round(cut * 10) / 10} px; the focus is on ` +
    `${focused.name}, the box scrolled ${scrolled} px, and the checkbox now at the place stands at ` +
    `top ${Math.round(landed.top)} to bottom ${Math.round(landed.bottom)} in a sight from ` +
    `${Math.round(now.top)} to ${Math.round(now.bottom)}`;
  expect(
    focused.onMark &&
      landed.top >= now.top - 1 &&
      landed.bottom <= now.bottom + 1 &&
      Math.abs(scrolled) <= cut + 1,
    text
  ).toBe(true);
});

// INFRA-3135, round 29 (the implementer's own row, named in the bar-return design note): the
// bar's Snooze names its rows as leaving from the pick until the first read of the Inbox the list
// adopts after the write, and the bar's hand-off waits only while a named row is still listed.
// Under `?group=none` the snoozed rows leave the list once the write lands, in the commit the
// bar goes in, while the read that will end their names is still out (builds Jobs
// `infra-3135-infra-3135-r30t-heldp` and `-r30v-namep`, Chromium: the rows and the bar gone 14 ms
// after the writes landed, the read asked at once and held, the landing made while both names
// stood, and the names ended two seconds later, when the list adopted that read). So with every
// read of the Inbox held for two seconds, the landing is made while the read is still held, on the
// row now at the snoozed rows' place. Then each read the route held is let go, and once the answer
// of every one of them has reached the page and the page has drawn two frames after it, the focus
// is still on that landing and the snoozed rows are still gone: a name that outlives the write
// neither holds the landing back nor moves it once the read that ends it is adopted. The row waits
// for those answers themselves, since a read the route lets go is not yet a read the page has: two
// frames after the route let go can come before the answer arrives, and would read the page as the
// held read left it. `@firefox` and `@webkit`: WebKit drew a snoozed row in its band in a commit
// after its write had settled (builds Job `infra-3135-infra-3135-r30f-s70p`), so the row runs in
// each engine.
// SLOW(>=2s): thirty asks seeded, and the reads of the Inbox held for two seconds.
test("the Inbox bar's Snooze with the Inbox's own read held lands while the read is out, on the row now at the snoozed rows' place, and the held read's answer moves nothing once the page has it @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "ASKS", name: "Asks" });
  for (let index = 0; index < 30; index++) {
    const owner = await createIssue({ force: true, project: "ASKS", title: `Ask owner ${index}` });
    await createAsk(owner.key, { question: `Ask ${index}: which way?` }, SURFACE_SESSION);
  }
  const bar = page.getByRole("toolbar", { name: "Selected rows" });
  await page.goto("/?group=none");
  await expect.poll(() => page.locator("[data-inbox-row]").count()).toBeGreaterThanOrEqual(20);
  const ids = await page
    .locator("[data-inbox-row]")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
  const upper = ids[4] ?? "";
  const lower = ids[5] ?? "";
  const after = ids[6] ?? "";
  // The lower row first, so the row marked last is the upper one.
  for (const id of [lower, upper]) await inboxRow(page, id).getByRole("checkbox").first().check();
  await expect(bar).toBeVisible();
  // From here on every read of the Inbox is held for two seconds. `heldNow` counts the reads held
  // at this moment, and `unanswered` keeps each read the route held until its answer has reached
  // the page, or until it failed: a read the page cancels while it is held brings no answer.
  let heldNow = 0;
  const unanswered = new Set<Request>();
  page.on("requestfinished", (request) => unanswered.delete(request));
  page.on("requestfailed", (request) => unanswered.delete(request));
  await page.route(/\/api\/v1\/inbox(?:\?|$)/u, async (route) => {
    unanswered.add(route.request());
    heldNow += 1;
    await new Promise((settle) => setTimeout(settle, 2_000));
    heldNow -= 1;
    return route.fallback();
  });
  const trigger = listTrigger(bar, "Snooze selected asks");
  await trigger.focus();
  await page.keyboard.press("Enter");
  await page.keyboard.type("tom");
  await expect(listOf(trigger).getByRole("option")).toHaveText(["Tomorrow"]);
  await page.keyboard.press("Enter");
  const landing = inboxRow(page, after).getByRole("checkbox").first();
  await expect(landing).toBeFocused({ timeout: 1_500 });
  expect(heldNow, "no read of the Inbox was out when the landing was made").toBeGreaterThan(0);
  await expect(page.locator(`[data-inbox-row="${upper}"]`)).toHaveCount(0);
  await expect(page.locator(`[data-inbox-row="${lower}"]`)).toHaveCount(0);
  // The route lets every held read go, and is taken off, so no later read is held.
  await expect.poll(() => heldNow, { timeout: 15_000 }).toBe(0);
  await page.unroute(/\/api\/v1\/inbox(?:\?|$)/u);
  // Each held read's answer reaches the page, and the page draws two frames after the last of
  // them, in which the list adopts it: the focus stays where the landing put it.
  await expect.poll(() => unanswered.size, { timeout: 15_000 }).toBe(0);
  await nextFrames(page);
  await expect(landing).toBeFocused();
  await expect(page.locator(`[data-inbox-row="${upper}"]`)).toHaveCount(0);
  await expect(page.locator(`[data-inbox-row="${lower}"]`)).toHaveCount(0);
});

// INFRA-3135, round 28 (the tester's finding beside its red row `35bec491`, which the architect
// ruled on at 2026-11-25 21:20Z: the implementer fixes it in the same change, and the next round
// checks it in five or more WebKit repeats). Measured at 5a444912 in builds Jobs
// infra-3135-f8-h3-wwf5b and infra-3135-infra-3135-conf-h-c58jz, 3 of 3 runs, and at 4f1ad776 in
// 3 of 3: in WebKit at 1440x900, with two Inbox threads marked and the checkbox of the row marked
// last cut by 70% of its height at the bottom edge of the list's box, the bar's Dismiss moves the
// box back 162 px - the height of the rows it took away - and leaves the focus on the checkbox of
// the row now at the place wholly below the reader's sight, at 1034 to 1050 under a box that ends
// at 876. Chromium and Firefox show that checkbox whole by the cut alone. The same landing at 30%
// is the row above; this one is its deeper cut, where the rows the press takes away free the most
// space above the place.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-28 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): eighty threads seeded, and one Dismiss read once the box settles.
test("the Inbox bar's Dismiss with the row marked last cut by 70% at the list's bottom edge shows the checkbox of the row now at the place whole by the least scroll @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 80; index++) {
    const issue = await createIssue({ project: "CORE", title: `Thread issue ${index}` });
    await seedCommentThread(
      issue.key,
      `Thread ${index}: when does it run?`,
      `At 0${index % 10}:00.`
    );
  }
  const bar = page.getByRole("toolbar", { name: "Selected rows" });
  const list = page.getByTestId("inbox-list");
  /** The part of the window the list's box shows its rows in, and the box's scroll. */
  const readerSight = () =>
    list.evaluate((box) => {
      const frame = box.getBoundingClientRect();
      return {
        bottom: Math.min(window.innerHeight, frame.bottom),
        scroll: box.scrollTop,
        top: Math.max(0, frame.top),
      };
    });
  /** The rect of the checkbox of the Inbox row `id`. */
  const checkboxRect = (id: string) =>
    page.evaluate((id) => {
      const own = document
        .querySelector(`[data-inbox-row="${id}"] input[type="checkbox"]`)
        ?.getBoundingClientRect();
      return {
        bottom: own?.bottom ?? Number.NaN,
        height: own?.height ?? Number.NaN,
        top: own?.top ?? Number.NaN,
      };
    }, id);
  await page.goto("/?group=none");
  await expect.poll(() => page.locator("[data-inbox-row]").count()).toBeGreaterThanOrEqual(20);
  // The rows read once the poll above found the list drawn; they only name the rows marked and
  // the one after them.
  const ids = await page
    .locator("[data-inbox-row]")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
  const upper = ids[12] ?? "";
  const lower = ids[13] ?? "";
  const after = ids[14] ?? "";
  // The lower row first, so the row marked last is the upper one.
  for (const id of [lower, upper]) await inboxRow(page, id).getByRole("checkbox").first().check();
  await expect(bar).toBeVisible();
  // The upper row's checkbox is cut by 70% of its height at the bottom edge of the list's box.
  const view = await readerSight();
  const own = await checkboxRect(upper);
  await list.evaluate(
    (box, by) => box.scrollBy(0, by),
    own.bottom - (view.bottom + own.height * 0.7)
  );
  await settledScrollTop(list);
  await nextFrames(page);
  const before = await readerSight();
  const placed = await checkboxRect(upper);
  const cut = placed.bottom - before.bottom;
  await bar.getByRole("button", { name: /selected threads$/u }).focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(`[data-inbox-row="${upper}"]`)).toHaveCount(0, { timeout: 15_000 });
  await expect(page.locator(`[data-inbox-row="${lower}"]`)).toHaveCount(0, { timeout: 15_000 });
  await settledScrollTop(list);
  await settledScrollTop(page);
  await nextFrames(page);
  // One read is the answer: both rows have gone and the box has held still, so nothing moves the
  // focus or the rows after this.
  const now = await readerSight();
  // Read in the same settled moment as the sight above, so it is where the press left it.
  const landed = await checkboxRect(after);
  // The focus read once, in that same settled moment: nothing moves it after the box holds still.
  const focused = await page.evaluate(
    (id) => ({
      name:
        document.activeElement?.getAttribute("aria-label") ??
        document.activeElement?.tagName ??
        "nothing",
      onMark:
        document.activeElement ===
        document.querySelector(`[data-inbox-row="${id}"] input[type="checkbox"]`),
    }),
    after
  );
  const scrolled = Math.round(now.scroll - before.scroll);
  const text =
    `the checkbox of the row marked last was cut by ${Math.round(cut * 10) / 10} px; the focus is on ` +
    `${focused.name}, the box scrolled ${scrolled} px, and the checkbox now at the place stands at ` +
    `top ${Math.round(landed.top)} to bottom ${Math.round(landed.bottom)} in a sight from ` +
    `${Math.round(now.top)} to ${Math.round(now.bottom)}`;
  expect(
    focused.onMark &&
      landed.top >= now.top - 1 &&
      landed.bottom <= now.bottom + 1 &&
      Math.abs(scrolled) <= cut + 1,
    text
  ).toBe(true);
});
