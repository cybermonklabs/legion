/** the rows upload documents and files, name their versions and read the references the server
 *  indexes, then read back what the server stored; the history's live feed and an opened
 *  document's live socket carry the server's own frames */

/*
 * Part N of dispatch://INFRA-1196 (dispatch://INFRA-3940): the rows that only the whole new issue
 * page can pass, read with the new page turned on (`withIssuePage`, through `phase` or each row's
 * own context). Each row is its source's, under its source's title: the checkpoint 29111db16c41's
 * `e2e/issue-layout.e2e.ts`, `e2e/artifacts.e2e.ts` and `e2e/keyboard-issue.e2e.ts`, and
 * `750e2716`'s copies of today's rows that the checkpoint put back to `main`'s text. `main`'s
 * files of those rows do not change: they read today's page.
 */

import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import type { Artifact, ArtifactUploadResponse } from "../web/src/api/types";
import { assigneeControlName } from "../web/src/features/issue/AssigneeControl";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { shortDay } from "../web/src/features/issue-page/node-time";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_PHRASES,
  CONTROL_LABELS,
  DASHBOARD_WORDS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  FILE_WORDS,
  GAP_LINES,
  NODE_REFERENCES_WORDS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  PRESET_ORDER,
  PULL_REQUEST_STATE_WORDS,
  STATE_LINES,
  sentenceCase,
  UNASKED_SOURCES,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/statement-rules";
import { VIEW_LOADING } from "../web/src/features/issue-page/view-loading-line";
import { type FakeSession, getUnsubscribeCalls } from "./agents";
import {
  createAsk,
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createMessage,
  createNamedVersion,
  createProject,
  dashboardOrigin,
  documentBase,
  editArtifact,
  editAsk,
  getArtifact,
  getArtifactBlocks,
  getArtifactText,
  getArtifactVersion,
  getAsk,
  getIssue,
  getIssueArtifactText,
  getIssueDecisions,
  getIssueEvents,
  getMyState,
  listComments,
  listIssueArtifacts,
  patchIssue,
  putIssueState,
  requestApproval,
  rotateLiveSockets,
  withOpenedAsk,
} from "./api";
import {
  countDocumentSockets,
  documentEditor,
  openDocumentSockets,
  placeCaret,
  selectTextIn,
  typeAtEnd,
} from "./editor";
import { nextFrames } from "./frames";
import {
  type AdvancingIssueSeed,
  type BaselineSeed,
  seedNoChildren,
  seedRequestBaseline,
  seedTwiceTheNodes,
  seedWholeIssue,
  seedWholeIssueAdvancing,
  type WholeIssueSeed,
} from "./issue-layout-seed";
import { asPlanner, PLANNER_TITLE, PROJECT, plannerActor } from "./issue-layout-seed-support";
import {
  checkBlockId,
  OPEN_QUESTION_BLOCK_ID,
  planTaskBlockId,
  requirementBlockId,
  requirementText,
} from "./issue-layout-seed-text";
import {
  closeDetails,
  contentsPane,
  filterRow,
  gotoIssue,
  iconRail,
  issuePage,
  marginShell,
  openDetails,
  openMenu,
  outlineControl,
  outlineSheet,
  phase,
  presetChooser,
  presetRow,
  presetToggle,
  presetToggles,
  presetToolbar,
} from "./issue-page";
import {
  DESKTOP,
  type DrawnRow,
  drawnRows,
  expectIssuePageDrawn,
  expectOpeningLanded,
  focusRow,
  focusThePage,
  nodeRow,
  nodeRows,
  PHONE,
  rowBody,
  rowWords,
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import { marginSlot, TWENTY_LABELS } from "./issue-page-h";
import {
  activePreset,
  caretInNode,
  caretInRowBlock,
  childRows,
  childrenSection,
  expectActivePreset,
  FALLBACK_LINES,
  focusedNodeId,
  focusInside,
  GONE_COMMENT,
  nodeBodies,
  onScreen,
  openEveryDeeperBranch,
  openPreset,
  outlineRows,
  pickPreset,
  treeActiveDescendant,
} from "./issue-page-i";
import { drawnContentsEntries, wordAtCaret } from "./issue-page-j";
import {
  focusedNodeFacts,
  historyEntries,
  historyItem,
  historyPanel,
  stepsSection,
} from "./issue-page-m";
import {
  type AddressContent,
  type AddressSeed,
  AWAITING_THE_BOX,
  AWAITING_THREADS,
  assertInventoryCarried,
  assertNoSidewaysScroll,
  chromeTexts,
  dashboardView,
  documentLink,
  documentRow,
  documentsList,
  documentsRows,
  drawsPart,
  expectActiveControl,
  expectDashboardDrawn,
  expectDocumentConnected,
  expectFocusLandedOnce,
  filterBox,
  filterControl,
  filterRowLooks,
  filterRowReading,
  firstDocumentLink,
  type HeaderSize,
  headerLimit,
  headerSize,
  INVENTORY_PARENT_ROWS,
  INVENTORY_RENAMES,
  INVENTORY_ROWS,
  type InventoryEntry,
  identityBlock,
  logFocusMoves,
  nodeMenuButton,
  OLD_PAGE_ADDRESSES,
  offered,
  offThePalette,
  openDocuments,
  openedArtifact,
  openedEditor,
  openFilterBox,
  openIssueActions,
  openIssueAddress,
  openPageState,
  openSpecDocument,
  openVersionControl,
  PART_ADDRESSES,
  type ParentRowMapping,
  REFLOW_STATES,
  rowsWithoutAuthorScrollers,
  scrollersOutsideAuthorText,
  seedWaitingWalk,
  showMoreControl,
  specAddress,
  stateName,
  undividedTexts,
  uploadInDocuments,
  VIEWS_FOCUS,
  versionControl,
  versionControlButton,
  versionPicker,
  WORD_STATES,
  zoomCrumbs,
} from "./issue-page-n";
import { withIssuePage } from "./issue-page-option";
import {
  expectHeaderDrawn,
  TWENTY_LABELS as HEADER_LABELS,
  IPHONE_13,
  issueHeader,
  lineOrder,
  MEASURED_TITLE,
  propertiesLine,
  seedHeader,
  stickyReading,
  viewportAt,
  waitingCount,
} from "./issue-page-p";
import { connect, isLiveSocket, type LiveSockets, watchLiveSockets } from "./live-socket-watch";
import { settledScrollTop } from "./mermaid-helpers";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { sql } from "./psql";
import {
  ANCHOR,
  type ReferenceWorld,
  referenceAgent,
  seedReferenceWorld,
} from "./reference-surfaces";
import { navigateInApp } from "./sends";
import {
  type Browser,
  type BrowserContext,
  test as dispatchTest,
  expect,
  type Locator,
  type Page,
  type Request,
  type Route,
  type TestInfo,
  type WebSocketRoute,
} from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";
import { assertFitsItsBox, assertPageFits, assertWhole, assertWholeAcross } from "./unclipped";
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

// ---- from 29111db16c41:packages/dispatch/e2e/issue-layout.e2e.ts

/** The kinds of node the issue holds that are no node of its conversation, so no row of the
 *  outline is one (dispatch://INFRA-1196 R1, R3). */
const NOT_CONVERSATION: readonly string[] = ["document", "handoff", "image"];

/** Fails on every row of `rows` that is a document, a file or a handoff of the issue: by its
 *  kind, and by its id, which for a document is its slug. */
function expectNoIssueNodeRows(
  rows: readonly DrawnRow[],
  slugs: ReadonlySet<string>,
  where: string
): void {
  expect(
    rows
      .filter((row) => NOT_CONVERSATION.includes(row.type ?? "") || slugs.has(row.id ?? ""))
      .map((row) => `${row.type} ${row.id}`),
    `${where} draws a row for a document, a file or a handoff of the issue, which are nodes of the issue and not of its conversation`
  ).toEqual([]);
}

// The first screen, as the architect's rulings 5 and 8 measure it (build-rulings-md): at
// 1440 x 900 the first node of the conversation starts at most 150 px down and at least six nodes
// have their words on the first screen in full; at 390 px, in the tests' browser, it starts at
// most 305 px down while no approval request waits on the reader and at most 335 px while one
// does (ruling 8, which replaces ruling 5's 280 px and 310 px). Read on this issue's own
// 115-character title with twenty labels, with the page at its top.
test("the first node starts on the first screen, with six nodes in full at 1440 px @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  /** Where the outline's first row starts, and how many rows stand whole between the band at
   *  the top and the box at the foot, with the page scrolled to its top. */
  const firstScreen = (page: Page): Promise<{ top: number; whole: number }> =>
    tree(page).evaluate((outline) => {
      window.scrollTo(0, 0);
      const style = getComputedStyle(document.documentElement);
      const foot = Number.parseFloat(style.getPropertyValue("--issue-composer-height")) || 0;
      const cards = [...outline.querySelectorAll<HTMLElement>("[data-node-row]")].map((row) =>
        (row.querySelector("[data-node-card]") ?? row).getBoundingClientRect()
      );
      return {
        top: cards[0]?.top ?? Number.POSITIVE_INFINITY,
        whole: cards.filter((card) => card.top >= 0 && card.bottom <= window.innerHeight - foot)
          .length,
      };
    });
  await phase(browser, seedWholeIssueAdvancing, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: MEASURED_TITLE });
    const desktopProject = test.info().project.name !== "iphone";
    for (const requestWaiting of [false, true]) {
      if (requestWaiting)
        await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
      const seed = requestWaiting ? "a request waiting" : "no request waiting";
      for (const width of desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width]) {
        await page.setViewportSize(viewportAt(width));
        await gotoIssue(page, `/issues/${seeded.key}/conversation`);
        await expectIssuePageDrawn(page);
        await expect(identityBlock(page)).toContainText(MEASURED_TITLE);
        await expect(
          nodeRows(page).first(),
          `the outline draws no row at ${width} px`
        ).toBeVisible();
        // The page opens with the focus on what waits on the reader, which is drawn by then.
        await expect.poll(async () => (await focusedNodeId(page)) !== null).toBe(true);
        const reading = await firstScreen(page);
        const limit = width === DESKTOP.width ? 150 : requestWaiting ? 335 : 305;
        console.log(
          `first screen at ${width} px with ${seed}: first node at ${reading.top.toFixed(1)} px, ${reading.whole} nodes whole`
        );
        test.info().annotations.push({
          description: `${width} px, ${seed}: first node at ${reading.top.toFixed(1)} px, ${reading.whole} nodes whole`,
          type: "first-screen",
        });
        expect
          .soft(
            reading.top,
            `the first node starts ${reading.top.toFixed(1)} px down, past ${limit} px, at ${width} px with ${seed}`
          )
          .toBeLessThanOrEqual(limit);
        if (width !== DESKTOP.width) continue;
        expect
          .soft(
            reading.whole,
            `${reading.whole} nodes have their words on the first screen in full at 1440 x 900 with ${seed}`
          )
          .toBeGreaterThanOrEqual(6);
      }
    }
  });
});

// R5 of dispatch://INFRA-3940 (dispatch://INFRA-3678, under R13): the count of what waits on the
// reader is a control, and its number is the length of Dashboard's list. On the issue this row
// seeds - one question, one decision to review, an approval request on the spec, a stale approval
// of the Verification document and two unread threads: six items - a press, or Enter on the count,
// from an opened document closes it, pushes Dashboard's address and puts the focus on the list's
// first item. Each press after goes on to the next item, shows the next five past the fifth and
// comes round to the first, the list showing its first five again, so every item the count counts
// is reached by presses alone. An approval request alone, at the bare address that draws
// Dashboard, takes the press as a move of the focus, with no new address.
test("the count of what waits on the reader opens the first thing that waits @phone", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedWaitingWalk, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    const items = page.locator('[data-testid^="dashboard-item-"]');
    const nextFive = page.getByRole("button", {
      name: new RegExp(`^${DASHBOARD_WORDS.next_five}`),
    });
    const drawn = () =>
      items.evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-testid") ?? ""));
    const focused = () =>
      page.evaluate(() => document.activeElement?.getAttribute("data-testid") ?? "");
    const pressCount = async (key: "click" | "Enter") => {
      if (key === "click") await waitingCount(page).click();
      else {
        await waitingCount(page).focus();
        await page.keyboard.press("Enter");
      }
    };
    for (const [width, key] of (desktopProject
      ? [
          [1440, "click"],
          [390, "Enter"],
        ]
      : [[390, "click"]]) as readonly (readonly [number, "click" | "Enter"])[]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      const settling = watchApiRequests(page);
      await gotoIssue(page, await specAddress(seeded.key));
      await expectLoadSettled(settling);
      await expect(openedArtifact(page), "the document did not open").toBeVisible();
      await expect(waitingCount(page)).toHaveText(`Waiting on you (${seeded.items})`);
      await pressCount(key);
      await expect(openedArtifact(page), "the count left the document open").toHaveCount(0);
      await expect
        .poll(focused, { message: `the focus is not on the first item that waits at ${width} px` })
        .toMatch(/^dashboard-item-/);
      await expect(page, "the count did not push Dashboard's address").toHaveURL(
        new RegExp(`/issues/${seeded.key}/dashboard$`)
      );
      await expectActiveControl(page, "dashboard");
      await expect(
        page.getByTestId("dashboard-waiting-count"),
        "the header's count is not the length of Dashboard's list"
      ).toHaveText(String(seeded.items));
      const first = (await drawn())[0];
      expect(await focused(), `the focus is off Dashboard's first item at ${width} px`).toBe(first);
      expect(await items.count(), "Dashboard did not draw its first five").toBe(5);

      // Each press after the first goes on to the next item in the list's order, and the one past
      // the fifth draws the next five: every item the count holds is reached by pressing it.
      const walked = [first];
      for (let press = 1; press < seeded.items; press += 1) {
        await pressCount(key);
        let next = "";
        await expect
          .poll(
            async () => {
              next = await focused();
              return next.startsWith("dashboard-item-") && !walked.includes(next);
            },
            { message: `press ${press + 1} of the count did not move the focus on at ${width} px` }
          )
          .toBe(true);
        walked.push(next);
      }
      expect(await drawn(), `the presses did not walk the list's order at ${width} px`).toEqual(
        walked
      );
      expect(await nextFive.count(), "the next five were drawn and their control stayed").toBe(0);
      const kinds = new Set(walked.map((id) => id.replace(/^dashboard-item-/, "").split("-")[0]));
      expect(kinds, "the presses reached no item of one of the five kinds").toEqual(
        new Set(["approval", "decision", "question", "stale", "thread"])
      );

      // The press after the last comes round to the first, and the list shows its first five.
      await pressCount(key);
      await expect
        .poll(focused, { message: `the focus did not come round at ${width} px` })
        .toBe(first);
      await expect(items, "the list did not show its first five again").toHaveCount(5);
      await expect(nextFive, "the control for the next five did not come back").toHaveCount(1);
    }

    // An approval request is all that waits: at the bare address, which draws Dashboard, the press
    // moves the focus to that request's item and writes no history entry.
    const lone = await createIssue({ force: true, project: PROJECT, spec: "Plan.", title: "Lone" });
    await requestApproval(lone.primary_artifact_id ?? "", { summary: "Read it." });
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${lone.key}`);
    await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    const entries = await page.evaluate(() => history.length);
    await waitingCount(page).click();
    await expect
      .poll(focused, { message: "the press at the bare address did not reach the request's item" })
      .toMatch(/^dashboard-item-approval-/);
    expect(
      await page.evaluate(() => history.length),
      "a press at the bare address wrote a history entry"
    ).toBe(entries);
    await expect(page).toHaveURL(new RegExp(`/issues/${lone.key}$`));
  });
});

// Ruling 5 of dispatch://INFRA-1196/artifact/build-rulings-md, "The filter row": one line at
// every width, from 320 px to 1920 px. In the row Documents stands after Decisions, and in the one
// control below 1280 px; the two small controls, the version control (with a view on or a document
// open) and the filter by words, stand at its right end before Keys from 1024 px, each drawn as
// Keys is, and the words box opens from its control. Below 1024 px, with a view on, they stand in
// the Outline sheet the Outline control opens; on Documents, and with a document open, the page
// draws no sheet and the version control stands at the row's end. Each control and the open box
// is at least 24 px tall, the row is drawn in the page's colours alone, and `t a` and `t f` open
// Documents' list with the focus on its first row and the box with the caret in it (part N's R4).
test("the filter row is one line at every width @phone", async ({ browser }) => {
  // Measured in the issue pod, alone: 47.0 s on chromium, which reads nine widths of three
  // controls, and 13.4 s on iphone, which reads one; the bound is about three times the longer,
  // for a slow leg.
  test.setTimeout(150_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    const names = PRESET_ORDER.map((one) => PRESET_NAMES[one]);
    const specName =
      ((await getIssue(seeded.key)).artifacts ?? []).find((one) => one.id === seeded.specArtifactId)
        ?.name ?? "";
    for (const width of desktopProject
      ? [1920, 1440, 1279, 1025, 1024, 1000, 761, 390, 320]
      : [390]) {
      for (const preset of ["conversation", "spec", "documents"] as const) {
        await page.setViewportSize(viewportAt(width));
        await gotoIssue(page, presetPath(seeded.key, preset));
        if (preset !== "documents") await expectIssuePageDrawn(page);
        await expectActiveControl(page, preset);
        const on =
          preset === "documents"
            ? PRESET_NAMES.documents
            : preset === "spec"
              ? "the Spec filter"
              : "no filter";
        const at = `at ${width} px with ${on} on`;
        const version = preset === "spec" ? [CONTROL_LABELS.version] : [];
        const filter = preset === "documents" ? [] : [CONTROL_LABELS.filter];
        const keys = width > 760 ? [CONTROL_LABELS.keys] : [];
        // Below 1024 px a view's two small controls stand in the Outline sheet; Documents draws no
        // outline and so no sheet.
        const inSheet = width < 1024 && preset !== "documents";
        if (width >= 1024 && preset === "spec") {
          await expect(versionControlButton(page), `no version control ${at}`).toBeVisible();
        }
        if (inSheet && preset === "spec") {
          // The sheet opens from the Outline control, which stands in the row below 1024 px: a row
          // that draws none leaves the version control nowhere.
          await expect(
            outlineControl(page),
            `the version control is not in the Outline sheet at ${width} px`
          ).toBeVisible();
        }
        // `expectActiveControl` above waited for the view's controls, which stand inside the
        // filter row, so the row is drawn and one read finds it.
        const row = await filterRowReading(page);
        if (row === null) throw new Error(`the page draws no filter row ${at}`);
        console.log(`filter row ${at}: ${JSON.stringify(row)}`);
        const wrapped = `the filter row is not one line ${at}: ${row.controls.join(" | ")}`;
        expect(row.lines, wrapped).toBe(1);
        expect(row.overflow, `the filter row runs past its own edge ${at}`).toBeLessThanOrEqual(1);
        // 35 px for the row of six, drawn from 1280 px, and 8 px above and below the 34 px one
        // control below that.
        expect(row.height, `the filter row is taller than the picture's ${at}`).toBeLessThanOrEqual(
          width >= 1280 ? 36 : 51
        );
        // Each control, the view's and the small ones alike, takes WCAG 2.2's 24 px target at
        // least (the tester's F13).
        expect(
          row.controls.filter((_, index) => (row.heights[index] ?? 0) < 24),
          `a control of the filter row is under 24 px tall ${at}: ${JSON.stringify(row.heights)}`
        ).toEqual([]);
        // Every colour the row draws is one of B's page colours of the scheme drawn: the light
        // one here, the dark one at the end of the walk.
        expect(
          await offThePalette(page, "light"),
          `the filter row draws a colour that is no page colour ${at}`
        ).toEqual([]);
        await assertPageFits(page);
        // Documents stands after Decisions: among the six controls from 1280 px, and in the one
        // control's menu below that; then, in the row's order, what the sheet leaves in it.
        const head = width >= 1280 ? names : [expect.any(String)];
        const rest = inSheet ? [CONTROL_LABELS.outline] : [...version, ...filter];
        expect(row.controls, `the filter row's controls are not the picture's ${at}`).toEqual([
          ...head,
          ...rest,
          ...keys,
        ]);
        if (width >= 1024 && keys.length > 0) {
          // Each small control is drawn as the prototype draws Keys: a 1 px border in #ECEAE7, a
          // 7 px radius, 12 px text of weight 500 on a 20 px line, in #57534E.
          const looks = await filterRowLooks(page);
          expect(looks[CONTROL_LABELS.keys], `Keys is not the prototype's ${at}`).toMatchObject({
            border: "1px solid rgb(236, 234, 231)",
            color: "rgb(87, 83, 78)",
            font: "12px 500 20px",
            radius: "7px",
          });
          for (const name of [...version, ...filter]) {
            expect(looks[name], `${name} is not drawn as Keys is ${at}`).toEqual(
              looks[CONTROL_LABELS.keys]
            );
          }
        }
        if (width < 1280 && preset === "conversation") {
          // Documents is an entry of the one control, after Decisions.
          await presetChooser(page).click();
          const items = openMenu(page).getByRole("menuitemradio");
          await expect(items, `the one control's menu is not the six views ${at}`).toHaveCount(
            names.length
          );
          for (const [index, name] of names.entries()) {
            await expect(
              items.nth(index),
              `the one control's entry ${index + 1} is not ${name} ${at}`
            ).toHaveAccessibleName(new RegExp(`^${name}( |$)`, "u"));
          }
          await page.keyboard.press("Escape");
          await expect(openMenu(page)).toHaveCount(0);
        }
        if (preset === "documents") {
          await expect(filterControl(page), `Documents draws a Filter ${at}`).toHaveCount(0);
          await expect(filterBox(page), `Documents draws the words box ${at}`).toHaveCount(0);
          await expect(outlineControl(page), `Documents draws a sheet ${at}`).toHaveCount(0);
          continue;
        }
        if (!inSheet) continue;
        // Below 1024 px the version control of the view and the filter by words stand in the
        // sheet, drawn open; the box is at least 24 px tall.
        await outlineControl(page).click();
        const sheet = outlineSheet(page);
        await expect(sheet).toBeVisible();
        await expect(
          sheet.getByTestId("issue-version-control"),
          `the version control is not in the Outline sheet at ${width} px`
        ).toHaveCount(version.length);
        const box = sheet.getByRole("searchbox", { name: CONTROL_LABELS.filter });
        await expect(box, `the Outline sheet holds no words box ${at}`).toBeVisible();
        expect(
          (await box.boundingBox())?.height ?? 0,
          `the words box in the Outline sheet is under 24 px tall ${at}`
        ).toBeGreaterThanOrEqual(24);
        await page.keyboard.press("Escape");
        await expect(sheet).toHaveCount(0);
      }
    }

    // The dark scheme draws the row in its own page colours.
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await gotoIssue(page, presetPath(seeded.key, "spec"));
    await expectActiveControl(page, "spec");
    await page.emulateMedia({ colorScheme: "dark" });
    // A control eases its colour over 150 ms after the scheme changes (`styles.css`).
    await expect
      .poll(() => offThePalette(page, "dark"), {
        message: "the filter row draws a colour that is no page colour in the dark scheme",
      })
      .toEqual([]);
    await page.emulateMedia({ colorScheme: "light" });

    // With a document open from Documents' list, at 1000 px and at 390 px the page draws no
    // Outline sheet, and the version control stands at the row's end, where it opens.
    for (const width of desktopProject ? [1000, IPHONE_13.width] : [IPHONE_13.width]) {
      const at = `with a document open at ${width} px`;
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, presetPath(seeded.key, "documents"));
      await documentLink(page, specName).click();
      await expect(openedEditor(page), `the spec does not open from Documents ${at}`).toBeVisible();
      await expect
        .poll(async () => (await filterRowReading(page))?.controls, {
          message: `the row does not hold the one control and the version control ${at}`,
        })
        .toEqual([
          expect.any(String),
          CONTROL_LABELS.version,
          ...(width > 760 ? [CONTROL_LABELS.keys] : []),
        ]);
      await expect(outlineControl(page), `an Outline sheet is drawn ${at}`).toHaveCount(0);
      await openVersionControl(page);
      await expect(versionControl(page), `the version control does not open ${at}`).toBeVisible();
      await expect(
        filterControl(page),
        `a document Documents opened draws a Filter ${at}`
      ).toHaveCount(0);
    }

    // `t a` opens Documents' list with the focus on its first row, and `t f` there turns
    // Conversation on with the caret in the words box, at 1440 px and, where the box stands in the
    // Outline sheet, at 390 px.
    for (const width of desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}/conversation`);
      await expectIssuePageDrawn(page);
      await expectOpeningLanded(page);
      await page.keyboard.press("t");
      await page.keyboard.press("a");
      await expect(page, `t a does not open Documents' address at ${width} px`).toHaveURL(
        new RegExp(`/issues/${seeded.key}/documents$`)
      );
      await expect(
        firstDocumentLink(page),
        `t a does not put the focus on Documents' first row at ${width} px`
      ).toBeFocused();
      await page.keyboard.press("t");
      await page.keyboard.press("f");
      await expect(
        tree(page),
        `t f on Documents does not turn Conversation on at ${width} px`
      ).toBeVisible();
      await expect(
        filterBox(page),
        `t f does not put the caret in the words box at ${width} px`
      ).toBeFocused();
    }

    // The words box opens from its control with the caret in it, narrows the outline by `?q=`,
    // stays open while it holds words, and shuts back into its control once emptied and left.
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expect(
      filterBox(page),
      "the words box is drawn before its control is pressed"
    ).toHaveCount(0);
    await filterControl(page).click();
    await expect(filterBox(page)).toBeFocused();
    expect(
      (await filterBox(page).boundingBox())?.height ?? 0,
      "the open words box is under 24 px tall at 1440 px"
    ).toBeGreaterThanOrEqual(24);
    await filterBox(page).fill("zzz-no-such-words");
    await expect(page).toHaveURL(/[?&]q=zzz-no-such-words/);
    await filterBox(page).blur();
    await expect(filterBox(page), "the box shut while it held words").toBeVisible();
    expect((await filterRowReading(page))?.lines, "the open words box broke the line").toBe(1);
    await filterBox(page).fill("");
    await filterBox(page).blur();
    await expect(filterBox(page), "the emptied box stayed open").toHaveCount(0);
    await expect(filterControl(page)).toBeVisible();
  });
});

/** How many rows of each kind `seedWholeIssue` draws with every branch open, from the seed's own
 *  record of what it wrote where it keeps one: the 12 requirements and the one citing a
 *  document, a result for each check but the one recorded with none, the two comment threads
 *  with their replies and the comment anchored to a block, the three messages of the one
 *  conversation, and the children; and the open question and the answered one. The issue is no
 *  row: the header names it (part G's R8). The summary's callout is the head's three sentences
 *  and no row (R8), and no document and no file is a row (dispatch://INFRA-1196 R1, R3). */
function rowsOfTheWholeIssue(seeded: WholeIssueSeed): Record<string, number> {
  const checks = seeded.checks.all.length;
  return {
    ask: 2,
    check: checks,
    child_issue: seeded.children.keys.length,
    comment: seeded.comments.threads.length + seeded.comments.replies.length + 1,
    decision: seeded.decisions.length,
    message: Object.keys(seeded.messages).length,
    plan_task: seeded.planTasks.length,
    requirement: seeded.requirements.length + 1,
    result: checks - 1,
  };
}

/** How many of `rows` there are of each kind. */
function rowsByKind(rows: readonly DrawnRow[]): Record<string, number> {
  const kinds: Record<string, number> = {};
  for (const row of rows) {
    const kind = row.type ?? "<no type>";
    kinds[kind] = (kinds[kind] ?? 0) + 1;
  }
  return kinds;
}

/** Every string `value` holds, however deeply: a declared table's entries. */
function phrasesOf(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(phrasesOf);
  return [];
}

// c-plain-words, and part N's R9 over INV-2's word states (`WORD_STATES`): each control of the
// view row on, Documents' list among them, and a document open, at 1440 px and at 390 px.
test("the page draws no internal word and no helper text @phone", async ({ browser }) => {
  // The check reads the page's chrome in INV-2's fourteen word states and in the body of every
  // row the seeded issue draws with every branch open - 58 of them - each reading read against
  // seven patterns and the declared list. One `expect` per reading was 70,000 calls at about
  // 4 ms each - 281 s of assertion beside 20 s of page work - so each state gathers every
  // offender it finds and names them in one assertion, which reads the same strength and says
  // more: a state that draws three internal words names all three rather than the first.
  // Measured in the issue pod, alone, over every state but the document's two loads: 28.5 s on
  // chromium and 26.2 s on iphone; the bound is about six times that, for a slow leg.
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The page's one declared list of what it draws unasked holds names, states, labels and
    // one-line lines that say what a place, a node or the issue holds or lacks; an entry that
    // is a sentence about the page or about Dispatch is helper text, and fails here (R22).
    const lines = new Set([EMPTY_STATE_LINES, GAP_LINES, STATE_LINES].flatMap(phrasesOf));
    const helperText = UNASKED_SOURCES.flatMap(phrasesOf).filter((phrase) => {
      const words = wordsIn(phrase);
      if (words.includes("dispatch") || words.includes("press")) return true;
      if (!phrase.trimEnd().endsWith(".")) return false;
      return !lines.has(phrase) || /\bthe page\b/iu.test(phrase) || words.includes("row");
    });
    expect(helperText, "the page declares helper text among what it draws unasked").toEqual([]);
    // The phase words, the ask body's and decision card's lists join once N's stack has INFRA-5702.
    // What the page says only in answer to the reader's own act, read on each fresh load below.
    const answers = ANSWER_PHRASES.map((phrase) => phrase.trim()).filter(
      (phrase) => phrase.length > 3
    );

    // An identifier, a fragment, a slug, and the schema and enumeration tokens the check names.
    const banned = [
      /[0-9a-f]{8}-[0-9a-f]{4}-/i,
      /#b-/,
      /before_merge/,
      /no-result/,
      /plan_task/,
      /\bartifact\b/i,
      /\bblock\b/i,
    ];

    const readChrome = async (where: string) => {
      // A page still filling lists fewer of its own words, never a wrong one, and each later
      // view reads the words it adds, so one read of the words drawn now serves.
      const texts = await chromeTexts(page);
      expect(texts.length, `${where} marks none of its own words as the page's`).toBeGreaterThan(0);
      const offenders: string[] = [];
      for (const text of texts) {
        for (const pattern of banned) {
          if (pattern.test(text)) offenders.push(`"${text}" holds ${pattern}`);
        }
        const undeclared = wordsIn(text).filter((word) => !DECLARED_WORDS.includes(word));
        if (undeclared.length > 0) {
          offenders.push(
            `"${text}" draws ${JSON.stringify(undeclared)}, which the page declares nowhere`
          );
        }
      }
      expect(
        offenders,
        `${where} draws internal words where a name belongs: ${offenders.join("; ")}`
      ).toEqual([]);
      // The attribute is what decides what this test covers, so it is asserted too: text the page
      // divides into neither its own words nor an author's would never be read above.
      expect(
        await undividedTexts(page),
        `${where} draws text that is neither the page's own words nor an author's`
      ).toEqual([]);
    };

    for (const { state, width } of WORD_STATES) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      // `openPageState` waits for what the state draws - a view's rows, Dashboard's list read,
      // the list's rows, the document's words - since a page read before its own reads land is a
      // head and a frame, which draw no internal word because they draw almost nothing. What it
      // cannot say is that the page has finished filling: it reads the state's first rows, and
      // the walk over every row's body below covers the rest.
      await openPageState(page, seeded.key, state);
      const where = `${stateName(state)} at ${width} px`;
      await readChrome(where);
      // A fresh load of the page is no act of the reader's: nothing the page says in answer to
      // one - a refusal, a key's report, a write's line, a box's description - is drawn, in the
      // page or in the Outline sheet an address opened.
      const loaded = [
        await issuePage(page).innerText(),
        ...(await outlineSheet(page).allInnerTexts()),
      ].join("\n");
      expect(
        answers.filter((answer) => loaded.includes(answer)),
        `${where} draws on load what the page says only in answer to the reader`
      ).toEqual([]);
    }

    // Every node's body in turn, where a type's attributes and its state are drawn in full.
    // The page opens folded to the frontier (R1): measured on this seed, the first drawing is
    // 49 of its 67 rows, with half its comments, one of its three messages and none of its
    // results, so a walk over that drawing reads no result's body at all. So every branch is
    // opened first, and every branch below the drawn levels, as a reader opens them to read
    // past the fold - once the page has stopped reading, since a row that lands after the
    // twisties were pressed lands folded - and the rows are read against the seed's own count
    // of every kind, so a kind that is still folded, or not yet drawn, fails here rather than
    // leaving its bodies out of the walk.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectLoadSettled(settling);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const seededRows = rowsOfTheWholeIssue(seeded);
    await expect
      .poll(async () => rowsByKind(await drawnRows(page)), {
        message: "the unfolded tree does not draw a row for every node the seed wrote",
      })
      .toEqual(seededRows);
    const rows = nodeRows(page);
    // The poll above saw every row the seed wrote drawn, on a page whose reads had gone quiet,
    // so one count holds them all.
    const count = await rows.count();
    expect(count, "the walk is not over every row the seed wrote").toBe(
      Object.values(seededRows).reduce((sum, kind) => sum + kind, 0)
    );
    for (let index = 0; index < count; index += 1) {
      const row = rows.nth(index);
      await row.focus();
      // The focus opens the body, and the body is waited for to draw words of its own, because
      // `readChrome` over an empty body reads the rest of the page and passes. What the wait
      // cannot say is which words those are; that is what `readChrome` is for.
      await expect(rowBody(row), `row ${index} opens no body on focus`).toBeAttached();
      await expect(
        rowBody(row),
        `the body of row ${index} drew no text of its own, so there is nothing in it to read`
      ).not.toHaveText(/^\s*$/u);
      await readChrome(`the body of row ${index}`);
    }
  });
});

/** A paragraph of the seeded Verification document's own text, which no node of the outline
 *  holds: the page draws it only where that document is opened (`specDocument`'s siblings in
 *  `e2e/issue-layout-seed-text.ts`). */
const VERIFICATION_PROSE = "How the issue page is proved.";

/** The newest version of `artifact`: the one its row of Documents' list names. */
function newestVersion(artifact: Artifact): NonNullable<Artifact["versions"]>[number] | undefined {
  return [...(artifact.versions ?? [])].sort((one, other) => other.number - one.number)[0];
}

/** How many rows of a list the page draws at a time, and how many documents the row seeds so that
 *  Documents' list has a second page of its own (root R40). */
const LIST_PAGE = 20;
const DOCUMENTS_SEEDED = 25;

// c-documents (1), R2, R3, R4 and R16: a document is opened from Documents' list, and no menu, no
// row of the outline and no view draws one. The seeded issue is today's model, an issue the
// conversion has not reached, so its spec, plan and verification documents are listed as every
// other document is; a converted issue's query nodes are owed until dispatch://INFRA-1253 is on
// `main`.
test("documents open from Documents and are never rows of the outline @phone", async ({
  browser,
}) => {
  // Measured in the issue pod, alone: 41.5 s on chromium and 40.6 s on iphone, for the views and
  // Documents' list at two widths, a document opened and left four ways, 25 versions and 25
  // documents seeded and read; the bound is three times that, for a slow leg.
  test.setTimeout(150_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const artifacts = (await getIssue(seeded.key)).artifacts ?? [];
    const slugs = new Set(artifacts.map((artifact) => artifact.slug));
    const verification = artifacts.find((one) => one.id === seeded.verificationArtifactId);
    if (verification === undefined) throw new Error(`${seeded.key} holds no Verification document`);
    const documentsAddress = new RegExp(`/issues/${seeded.key}/documents$`);
    const verificationAddress = new RegExp(`/issues/${seeded.key}/artifacts/${verification.slug}$`);

    // No document and no file is a row of the outline, and no document's text and no editor
    // is drawn, with no filter and with each on, at 1440 px with every branch open and at
    // 390 px. Dashboard is drawn in place of the outline, so there it is read once Dashboard and
    // its list are drawn, with no outline to unfold.
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER.filter((one) => one !== "documents")) {
        const where = `the ${PRESET_NAMES[preset]} preset at ${size.width} px`;
        await gotoIssue(page, presetPath(seeded.key, preset));
        if (preset === "dashboard") {
          await expectDashboardDrawn(page, where);
        } else {
          await expectIssuePageDrawn(page);
          if (size.width >= 1024) await unfoldEveryBranch(page);
        }
        expectNoIssueNodeRows(await drawnRows(page), slugs, where);
        expect(
          await page.getByRole("region", { name: "Document editor" }).count(),
          `${where} draws a document's editor with no document opened`
        ).toBe(0);
        expect(
          await page.getByText(VERIFICATION_PROSE).count(),
          `${where} draws the Verification document's own text with no document opened`
        ).toBe(0);
      }
    }

    // Documents is the control of the view row that lists every document and file of the issue,
    // each with its version and its last change, and draws no outline beside the list, no three
    // sentences, no children's section, no Contents pane and no Filter. No menu of the page lists
    // a document, nor draws the node menu that held them. A document the list opens is drawn in
    // place of the list, with its head, its versions and its references; Back to Documents and
    // Escape return to the list, with the focus on the document's row.
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}/conversation`);
      await expectIssuePageDrawn(page);
      await expect(nodeMenuButton(page), `the page draws a node menu ${where}`).toHaveCount(0);
      await openIssueActions(page);
      for (const artifact of artifacts) {
        await expect(
          page.getByRole("menuitem").filter({ hasText: artifact.name }),
          `a menu of the page lists ${artifact.name} ${where}`
        ).toHaveCount(0);
      }
      await openDocuments(page);
      await expectActiveControl(page, "documents");
      await expect(page, `Documents does not open its address ${where}`).toHaveURL(
        documentsAddress
      );
      for (const artifact of artifacts) {
        const row = documentRow(page, artifact.name);
        const missing = `Documents' list draws no row of ${artifact.name} ${where}`;
        await expect(row, missing).toHaveCount(1);
        const latest = newestVersion(artifact);
        const day = latest === undefined ? "" : shortDay(latest.created_at);
        const named = `${FILE_WORDS.version} ${latest?.number}${day === "" ? "" : ` · ${day}`}`;
        await expect(
          row,
          `${artifact.name}'s row does not name its version and its last change ${where}`
        ).toContainText(named);
      }
      for (const name of [CONTROL_LABELS.upload, CONTROL_LABELS.new_document]) {
        const control = documentsList(page).getByRole("button", { exact: true, name });
        await expect(control, `Documents' list draws no ${name} ${where}`).toHaveCount(1);
      }
      for (const [what, drawn] of [
        ["the issue's three sentences", page.getByTestId("issue-three-things")],
        ["the children's section", childrenSection(page)],
        ["the Contents pane", contentsPane(page)],
        ["the outline", tree(page)],
        ["an Outline sheet", outlineControl(page)],
        ["a Filter", filterControl(page)],
        ["the words box", filterBox(page)],
      ] as const) {
        await expect(drawn, `Documents' list draws ${what} ${where}`).toHaveCount(0);
      }

      // The Verification document opened from the list: its text, its editor and its versions,
      // under its head, which names the version it shows, who wrote it and its day.
      await documentLink(page, verification.name).click();
      await expect(page, `the list's item does not open the document ${where}`).toHaveURL(
        verificationAddress
      );
      await expect(
        page.getByText(VERIFICATION_PROSE),
        `the document opened from Documents draws none of its own text ${where}`
      ).toBeVisible();
      await expect(
        openedEditor(page),
        `the opened document draws no editor ${where}`
      ).toBeVisible();
      for (const [what, drawn] of [
        ["the outline", tree(page)],
        ["the children's section", childrenSection(page)],
        ["the Contents pane", contentsPane(page)],
        ["the list it was opened from", documentsList(page)],
      ] as const) {
        await expect(drawn, `${what} is still drawn beside the document ${where}`).toHaveCount(0);
      }
      await expect(
        versionPicker(page),
        `the version control beside the filters does not read the opened document ${where}`
      ).toHaveCount(1);
      await expect(
        openedArtifact(page).getByRole("region", { name: /^Versions for / }),
        `the opened document draws no versions ${where}`
      ).toHaveCount(1);
      const newest = [...((await getArtifact(seeded.verificationArtifactId)).versions ?? [])].sort(
        (one, other) => other.number - one.number
      )[0];
      if (newest === undefined)
        throw new Error("the seeded Verification document holds no version");
      await expect(openedArtifact(page).getByTestId("artifact-header")).toContainText(
        verification.name
      );
      const authors = (newest.authors ?? []).length > 0 ? ".+ · " : "";
      await expect(
        openedArtifact(page).getByTestId("opened-artifact-version"),
        `the head does not name the version it shows, who wrote it and its day ${where}`
      ).toHaveText(
        new RegExp(
          `^${FILE_WORDS.version} ${newest.number} · ${authors}${shortDay(newest.created_at)}$`,
          "u"
        )
      );
      await expect(
        page.getByRole("button", { exact: true, name: CONTROL_LABELS.close_document }),
        `the opened document draws today's way back to the conversation ${where}`
      ).toHaveCount(0);
      const back = CONTROL_LABELS.back_to_documents;
      await page.getByRole("button", { exact: true, name: back }).click();
      await expect(
        documentsList(page),
        `${back} does not return to the list ${where}`
      ).toBeVisible();
      await expect(page).toHaveURL(documentsAddress);
      await expect(
        documentLink(page, verification.name),
        `${back} leaves the focus off the document's row ${where}`
      ).toBeFocused();
      const backLeft = `${back} leaves the document drawn ${where}`;
      await expect(openedArtifact(page), backLeft).toHaveCount(0);
      // Escape from a document the list opened returns to the list as well.
      await documentLink(page, verification.name).click();
      await expect(openedEditor(page), `the document did not open again ${where}`).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(
        documentsList(page),
        `Escape did not return to Documents' list ${where}`
      ).toBeVisible();
      await expect(page).toHaveURL(documentsAddress);
      await expect(
        documentLink(page, verification.name),
        `Escape leaves the focus off the document's row ${where}`
      ).toBeFocused();
      const escapeLeft = `Escape leaves the document drawn ${where}`;
      await expect(openedArtifact(page), escapeLeft).toHaveCount(0);
    }

    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });

    // Escape on the list returns to the view the reader came from, and, after a cold load of the
    // list, to Conversation.
    await gotoIssue(page, presetPath(seeded.key, "spec"));
    await expectIssuePageDrawn(page);
    await openDocuments(page);
    await page.keyboard.press("Escape");
    await expect(
      page,
      "Escape on Documents' list does not return to the view it came from"
    ).toHaveURL(new RegExp(`/issues/${seeded.key}/spec$`));
    await expectActiveControl(page, "spec");
    await gotoIssue(page, presetPath(seeded.key, "documents"));
    await expect(documentsList(page)).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(
      tree(page),
      "Escape on Documents' list after a cold load does not return to Conversation"
    ).toBeVisible();
    // Conversation's own address: the issue's own draws Dashboard (dispatch://INFRA-5702).
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/conversation$`));
    await expectActiveControl(page, "conversation");

    // The block that cites a document carries a chip in its words that opens it, named by the
    // page's word for a document: a document's name is its file name, which no chip draws
    // (part G's R4). Escape from the document the chip opened returns to the chip's row, with
    // the focus on the chip.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    await unfoldEveryBranch(page);
    const citing = nodeRow(page, blockNodeId(specSlug, seeded.documents.citingBlock)).first();
    await citing.focus();
    const chip = citing.locator(
      `a[data-row-chip][data-row-reference="dispatch://${seeded.key}/artifact/${seeded.documents.cited}"]`
    );
    await expect(chip, "the block that cites a document carries no chip for it").toHaveCount(1);
    await expect(chip, "the chip names the document other than by the page's word").toHaveText(
      NODE_TYPE_NAMES.document
    );
    await chip.click();
    await expect(page, "the chip does not open the document it names").toHaveURL(
      new RegExp(`/issues/${seeded.key}/artifacts/${seeded.documents.cited}$`)
    );
    await expect(
      page.getByText("The measurements behind the first requirement.", { exact: true }),
      "the chip opened no text of the document it names"
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(
      page,
      "Escape from a document a chip opened does not return to the outline"
    ).toHaveURL(new RegExp(`/issues/${seeded.key}/conversation$`));
    await expect(
      chip,
      "Escape from a document a chip opened leaves the focus off the chip"
    ).toBeFocused();

    // Upload, in Documents' list, uploads a file.
    await openDocuments(page);
    await uploadInDocuments(page, {
      buffer: Buffer.from("# Uploaded\n\nFrom Documents' list.\n"),
      mimeType: "text/markdown",
      name: "uploaded.md",
    });
    await expect
      .poll(async () => ((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.name))
      .toContain("uploaded.md");
    await expect(
      documentRow(page, "uploaded.md"),
      "the list draws no row of the upload"
    ).toHaveCount(1);

    // A document's versions, every one of them named or not, drawn 20 at a time with no control
    // that hides any; and what it references and what references it, drawn open (R2). The notes
    // hold more than 20 versions, one of them named, the newest not.
    const cited = artifacts.find((one) => one.slug === seeded.documents.cited);
    if (cited === undefined) throw new Error(`${seeded.key} holds no ${seeded.documents.cited}`);
    // The notes link a document of another issue, which is what the page's list of what the
    // issue's documents reference holds.
    const foreign = await createIssueArtifact(seeded.parentKey, {
      content: "# Foreign\n\nA document of the parent issue.\n",
      name: "foreign.md",
    });
    const revise = (revision: number) =>
      editArtifact(cited.id, {
        ops: [
          {
            after: "end",
            markdown:
              revision === 1
                ? `Revision 1, which reads dispatch://${seeded.parentKey}/artifact/${foreign.artifact.slug}.`
                : `Revision ${revision}.`,
            op: "insert",
          },
        ],
      });
    for (let revision = 1; revision <= 12; revision += 1) await revise(revision);
    await createNamedVersion(cited.id, "The twelfth revision");
    for (let revision = 13; revision <= 25; revision += 1) await revise(revision);
    const versions = (await getArtifact(cited.id)).versions ?? [];
    const manyVersions = "the seed holds no more versions of the notes than a page draws";
    expect(versions.length, manyVersions).toBeGreaterThan(LIST_PAGE);
    expect(
      versions.filter((one) => one.named),
      "the seed names no version of the notes"
    ).not.toEqual([]);
    const newestOfNotes = newestVersion({ ...cited, versions });
    expect(newestOfNotes?.named, "the newest version of the notes is named").toBe(false);
    await expect
      .poll(
        async () =>
          (
            (await (await page.request.get(`/api/v1/issues/${seeded.key}/references`)).json())
              .members ?? []
          ).length,
        { message: "the issue's references hold no document of another issue" }
      )
      .toBeGreaterThan(0);
    await gotoIssue(page, presetPath(seeded.key, "documents"));
    await documentLink(page, cited.name).click();
    const versionList = openedArtifact(page).getByRole("region", {
      name: `${FILE_WORDS.versions_for} ${cited.name}`,
    });
    const newestDownload = `${CONTROL_LABELS.download_version} ${newestOfNotes?.number}`;
    await expect(
      versionList.getByRole("link", { exact: true, name: newestDownload }),
      `an unnamed version is not drawn: version ${newestOfNotes?.number}`
    ).toBeVisible();
    await expect(
      openedArtifact(page).getByRole("button", { name: "Show all versions" }),
      "the opened document draws a control that hides versions"
    ).toHaveCount(0);
    await expect(
      versionList.getByRole("listitem"),
      "the versions are not drawn 20 at a time"
    ).toHaveCount(LIST_PAGE);
    await expect(showMoreControl(versionList)).toHaveText(
      `${CONTROL_LABELS.show_more} (${versions.length - LIST_PAGE} ${CONTROL_LABELS.show_more_left})`
    );
    await showMoreControl(versionList).click();
    await expect(
      versionList.getByRole("listitem"),
      "Show more does not draw the versions left"
    ).toHaveCount(versions.length);
    await expect(showMoreControl(versionList)).toHaveCount(0);
    const downloads = new RegExp(`^${CONTROL_LABELS.download_version} \\d+$`, "u");
    const drawn = (await versionList.getByRole("link", { name: downloads }).allInnerTexts()).map(
      (text) => Number(text.replace(/\D+/gu, ""))
    );
    expect(
      versions.map((one) => one.number).filter((number) => !drawn.includes(number)),
      "an unnamed version is not drawn: these versions are missing from the list"
    ).toEqual([]);
    // What references the document and what the issue's documents reference are drawn open,
    // under their own headings, with no control that opens either.
    const referencedBy = openedArtifact(page).getByRole("region", {
      name: PROPERTY_LABELS.referenced_by,
    });
    await expect(referencedBy, "the opened document draws no 'Referenced by'").toBeVisible();
    await expect(
      referencedBy.getByRole("listitem").first(),
      "the opened document's 'Referenced by' is drawn shut"
    ).toBeVisible();
    await expect(
      openedArtifact(page).getByRole("button", { name: PROPERTY_LABELS.referenced_by }),
      "'Referenced by' has a control that opens it"
    ).toHaveCount(0);
    const references = openedArtifact(page).getByRole("region", { name: "References" });
    await expect(
      references.getByRole("listitem").first(),
      "what the issue's documents reference is drawn shut"
    ).toBeVisible();

    // An artifact's version history and a version's download are reachable from the artifact
    // itself: its own address draws its versions, each with its download, and the version
    // control beside the views picks among them, behind its small control (R3, R4). The image
    // draws its picture at its own address.
    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.cited}`);
    await expect(issuePage(page)).toHaveCount(1);
    await openVersionControl(page);
    await expect(
      page.getByRole("combobox", { exact: true, name: CONTROL_LABELS.version }),
      "the document's versions are not reachable"
    ).toHaveCount(1);
    // The panel shuts by its own control, as a reader shuts it.
    await versionControlButton(page).click();
    await expect(versionControl(page)).toBeHidden();
    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.image}`);
    await expect(issuePage(page)).toHaveCount(1);
    await expect(
      page.locator('main img[src*="/versions/"]').first(),
      "the image draws no picture at its own address"
    ).toBeVisible();

    // Nothing is named Artifacts: on the issue's own address, Documents', and a document's.
    for (const address of [
      `/issues/${seeded.key}`,
      `/issues/${seeded.key}/documents`,
      `/issues/${seeded.key}/artifacts`,
      `/issues/${seeded.key}/artifacts/${seeded.documents.cited}`,
    ]) {
      await gotoIssue(page, address);
      await expect(issuePage(page)).toHaveCount(1);
      for (const role of ["region", "tab", "button", "link", "menuitem"] as const) {
        expect(
          await page.getByRole(role, { exact: true, name: "Artifacts" }).count(),
          `${address} draws a ${role} named Artifacts`
        ).toBe(0);
      }
    }

    // A slug the issue does not hold draws the page's one line for it: not the slug, and not the
    // old tab's word (R2).
    const unheld = "no-such-document";
    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${unheld}`);
    await expect(
      page.getByTestId("opened-artifact-missing"),
      "a slug the issue does not hold draws other than the page's line"
    ).toHaveText(EMPTY_STATE_LINES.document_missing);
    await expect(issuePage(page), "the page draws the slug its address wrote").not.toContainText(
      unheld
    );

    // While a document is open, Details holds its comments, as today's margin does (R2): the
    // spec's comment anchored to its second requirement.
    await gotoIssue(page, await specAddress(seeded.key));
    await expect(openedEditor(page), "the spec's address opens no editor").toBeVisible();
    await openDetails(page);
    await expect(
      marginShell(page)
        .locator("[data-margin-item]")
        .filter({ hasText: "This requirement is the one the tree reads first." }),
      "Details does not hold the comment of the open document"
    ).toHaveCount(1);
    await closeDetails(page);

    // The two readings of the checkpoint's "the spec is a preset over the tree and an approval
    // still names it" that part J's row leaves to this part (R3): under the Spec view the
    // version control beside the views reads the spec, and it carries no approval, which is the
    // header's, while an approval of the spec waits on the reader.
    await requestApproval(seeded.specArtifactId, { summary: "The spec as the tree draws it." });
    await gotoIssue(page, presetPath(seeded.key, "spec"));
    await expectIssuePageDrawn(page);
    await expect(
      identityBlock(page).locator("[data-approval-artifact]").first(),
      "the header draws no approval of the spec"
    ).toBeVisible();
    await expect(
      versionControl(page),
      "the Spec filter draws no version control beside the filters"
    ).toHaveCount(1);
    await openVersionControl(page);
    expect(
      await versionControl(page)
        .getByRole("button", { name: /^Approve/ })
        .count(),
      "the version control beside the filters carries an approval control"
    ).toBe(0);

    // Documents' list draws 20 rows at a time, with a control that says how many are left, on
    // a seed of 25 documents. Its control draws the next 20.
    const held = (await getIssue(seeded.key)).artifacts?.length ?? 0;
    for (let extra = held; extra < DOCUMENTS_SEEDED; extra += 1) {
      await createIssueArtifact(seeded.key, {
        content: `# Extra ${extra}\n\nA document the seed adds.\n`,
        name: `extra-${extra}.md`,
      });
    }
    const seededCount = (await getIssue(seeded.key)).artifacts?.length;
    expect(seededCount, "the seed does not hold 25 documents").toBe(DOCUMENTS_SEEDED);
    await gotoIssue(page, presetPath(seeded.key, "documents"));
    await expect(
      documentsRows(page),
      "Documents' list does not draw 20 rows and the count of the rest on 25 documents"
    ).toHaveCount(LIST_PAGE);
    await expect(showMoreControl(documentsList(page))).toHaveText(
      `${CONTROL_LABELS.show_more} (${DOCUMENTS_SEEDED - LIST_PAGE} ${CONTROL_LABELS.show_more_left})`
    );
    await showMoreControl(documentsList(page)).click();
    await expect(documentsRows(page), "the control does not draw the next 20").toHaveCount(
      DOCUMENTS_SEEDED
    );
    await expect(showMoreControl(documentsList(page))).toHaveCount(0);
  });
});

// c-reflow, and part N's R9 over INV-2's reflow states (`REFLOW_STATES`): each control of the view
// row on, Documents' list among them, a document open and the Outline sheet open, at seven widths.
test("the page never scrolls in two dimensions @phone", async ({ browser }) => {
  // Eight states at seven widths, each a load of the page, with a clipping walk over every row a
  // view draws, each read in the browser, plus the page's own fit, Documents' list's, the open
  // document's, the sheet's and the panel's. Measured in the issue pod, alone: 1.6 min on
  // chromium and 1.7 min on iphone, which read the same seven widths, where CI read 2.6 min at
  // six; the bound is about four times the pod's, for a slow leg.
  test.setTimeout(420_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The test sets the viewport and writes no width rule of its own: every measure is the page's.
    for (const { state, width } of REFLOW_STATES) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      // Nothing below is measured until the state is drawn: a view's rows, Dashboard's list read,
      // the list's rows, the document's words (`openPageState`). `gotoIssue` waits for the head
      // alone, and the tree is drawn empty and fills as the reads land, so a measurement taken
      // then would read a page that holds no row - and a page that holds no row is a page that
      // fits, which is the first thing `assertPageFits` retries its way into passing on. What the
      // wait cannot say is that the last row has landed; the walk below reads the rows that are
      // drawn when it runs, and seven widths read them seven times.
      await openPageState(page, seeded.key, state);
      const at = `${stateName(state)} at ${width} px`;
      // In every state the page fits its viewport, with its header and its view row whole.
      await assertPageFits(page);
      await assertWhole(identityBlock(page));
      await assertWhole(presetRow(page));
      if (state.kind === "view" && state.preset === "documents") {
        // Documents' list, drawn in place of the outline - its filter, Upload, New document and
        // each row with its controls - scrolls down with the page and is read for a sideways clip
        // and a clamped line, as Details is.
        await assertWholeAcross(documentsList(page));
        continue;
      }
      if (state.kind === "document") {
        // The open document - its head, its editor, its versions and what references it -
        // scrolls down and never sideways. Its editor scrolls down in a box of its own, and each
        // quote of what references it is clamped to three lines behind its own Show more (R2's
        // "folded"), so the reading is R9's own: nothing in it scrolls sideways.
        await assertNoSidewaysScroll(openedArtifact(page));
        expect(
          await scrollersOutsideAuthorText(page),
          `a code block, a table or a picture is drawn outside a node's own text with ${at}`
        ).toEqual([]);
        continue;
      }
      if (state.kind === "view" && state.preset === "dashboard") {
        // Dashboard, drawn in place of the outline, draws no row of it for the walk below: what
        // it draws - its phase, what waits on the reader, its tiles and its agents - scrolls down
        // with the page, so the reading is R9's own, as for the open document: nothing in it
        // scrolls sideways.
        await assertNoSidewaysScroll(dashboardView(page));
        expect(
          await scrollersOutsideAuthorText(page),
          `a code block, a table or a picture is drawn outside a node's own text with ${at}`
        ).toEqual([]);
        continue;
      }
      if (state.kind === "outline-sheet") {
        // From 1024 px the Contents pane stands at the outline's left: its list scrolls down by
        // design and is read for a sideways clip, and each of its one-line rows for a box that
        // reaches past its own. Below 1024 px the Outline control opened the same list in a
        // sheet over the Spec view, with the view row's small controls above it, which leaves
        // the page fitting its viewport.
        if (width >= 1024) {
          await assertWholeAcross(contentsPane(page));
          // The pane lists the tree whose rows the state's wait saw drawn, so one read finds its
          // entries, and a list still filling only checks fewer of them.
          const entries = await drawnContentsEntries(contentsPane(page)).all();
          expect(entries.length, `the Contents pane lists nothing at ${width} px`).toBeGreaterThan(
            0
          );
          for (const entry of entries) await assertFitsItsBox(entry);
        } else {
          await assertWholeAcross(outlineSheet(page));
          await page.keyboard.press("Escape");
          await expect(outlineSheet(page)).toHaveCount(0);
        }
        continue;
      }
      // A view, or none. The walk is read against the rows it covers, so the count is an
      // assertion of its own: a walk over no row would pass every clipping check for want of
      // anything to measure.
      const walked = await rowsWithoutAuthorScrollers(page);
      expect(
        walked.length,
        `the clipping walk covered no row with ${at}: the page draws none`
      ).toBeGreaterThan(0);
      // R17's reading of a row, which is of its box and not of its words: a row draws its
      // node's words whole, cut only where an agent's words run past six lines (the
      // architect's ruling 1 on R1), and a folded branch draws its one line, so no row may
      // reach past the box it is drawn in or hold a box that scrolls inside it, which is what
      // `assertFitsItsBox` reads (`e2e/unclipped.ts` has both readings and why a row takes the
      // weaker one for its box). c-tree reads the one line against R1.
      for (const row of walked) {
        await assertFitsItsBox(row);
      }
      expect(
        await scrollersOutsideAuthorText(page),
        `a code block, a table or a picture is drawn outside a node's own text with ${at}`
      ).toEqual([]);
      if (state.preset !== "conversation") continue;

      // With no view on, the frame (R14): the icon rail from 768 px up, 56 px of the width, and
      // none below it.
      if (width >= 768) {
        await expect(
          iconRail(page),
          `the issue page draws no icon rail at ${width} px`
        ).toHaveCount(1);
        expect((await iconRail(page).boundingBox())?.width, `the rail at ${width} px`).toBe(56);
      } else {
        await expect(iconRail(page), `a rail is drawn at ${width} px`).toHaveCount(0);
      }
      // The panel holds the properties, the focused node's facts and the whole conversation, so
      // it is taller than the screen and scrolls down by design; R18 bans two dimensions, not
      // one, so Details, open over the page, is read for a sideways clip and a clamped line
      // (`e2e/unclipped.ts`), with the page under it still fitting its viewport.
      await openDetails(page);
      await assertPageFits(page);
      await assertWholeAcross(marginSlot(page));
      await closeDetails(page);

      if (width >= 1024) continue;
      // Below 1024 px the tree is read one node at a time (R16): the node it is on and one
      // level below it, with the paths the frontier holds open, and one body at most.
      expect(
        await nodeBodies(page).count(),
        `the narrow tree draws more than the one node's body at ${width} px`
      ).toBeLessThanOrEqual(1);
    }
  });
});

/** The fixture `e2e/record-page-inventory.e2e.ts` wrote: what today's page offered, by state.
 *  What the new page offers in its place is `e2e/issue-page-n.ts`'s rename table. */
interface PageInventoryFixture {
  readonly commit: string;
  readonly "open-1440": readonly InventoryEntry[];
  readonly "open-390": readonly InventoryEntry[];
  readonly "closed-1440": readonly InventoryEntry[];
  readonly "closed-390": readonly InventoryEntry[];
}

/** One of the fixture's four states: the issue open or closed, at 1440 or at 390 px. */
type InventoryState = Exclude<keyof PageInventoryFixture, "commit">;

/** The priorities' picker, which "Edit priorities" opens over the page in a portal
 *  (`IssuePriorityLine.tsx`, `MultiSelect`): its search box, named as the page names it. */
function prioritiesPicker(page: Page): Locator {
  return page.getByRole("combobox", { exact: true, name: CONTROL_LABELS.priority_to_advance });
}

/** The control in Details that opens the priorities' picker, which a closed issue draws refused. */
function editPriorities(page: Page): Locator {
  return marginShell(page).getByRole("button", {
    exact: true,
    name: CONTROL_LABELS.edit_priorities,
  });
}

/** Closes whatever the view before this one opened, so the next control is reached rather than
 *  dismissed: a press outside an open menu, or outside the identity block's actions group,
 *  closes it instead of reaching the control under it, and Escape closes each of them
 *  (`IssueActionsMenu.tsx`). The priorities' picker closes by Escape too, as a reader closes it,
 *  and Details by its own Close, since on a phone it covers the page. */
async function closeOpenControls(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(openMenu(page)).toHaveCount(0);
  const actions = offered(page, "button", CONTROL_LABELS.issue_actions);
  // The header drew this control with the page an earlier view loaded, and Escape draws no
  // control, so one count says whether this page shows it.
  if ((await actions.count()) === 1) {
    await expect(actions).not.toHaveAttribute("aria-expanded", "true");
  }
  await expect(prioritiesPicker(page), "Escape left the priorities' picker open").toHaveCount(0);
  await closeDetails(page);
}

/**
 * Puts the page where the outline draws both rows a parent-row mapping names (`ParentRowMapping`):
 * every branch open where the outline draws the whole tree, and below 1024 px, where a branch is
 * zoomed by its row's own key, the target's own zoom, whose nodes follow it. It is read off a page that has
 * stopped reading, as the zoom walk of `inventoryViews` is, because the tree fills as its reads
 * land. A target the tree does not draw has no zoom to press, and the mapping is then lost, which
 * `parentRowMissing` names: a press waiting for that twisty times out and ends the row there,
 * leaving every state after it unread.
 */
function inventoryOutline(
  page: Page,
  key: string,
  width: number
): (mapping: ParentRowMapping) => Promise<void> {
  return async (mapping) => {
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${key}/conversation`);
    await expectLoadSettled(settling);
    if (width >= 1024) {
      await unfoldEveryBranch(page);
      return;
    }
    const parent = nodeRows(page).filter({ hasText: mapping.parent }).first();
    // The page's reads went quiet above, so the tree is drawn whole and one count says whether
    // the parent's row draws a twisty, which is what a branch has.
    if ((await parent.getByRole("button", { name: CONTROL_LABELS.unfold }).count()) > 0) {
      // A twisty opens a branch in place at every width; the zoom is the row's own key.
      await parent.focus();
      await page.keyboard.press(".");
      // The zoom is drawn once its crumbs are, and the rows are read the moment this returns.
      await expect(
        zoomCrumbs(page),
        `the zoom key of "${mapping.parent}" zoomed nowhere`
      ).not.toHaveCount(0);
    }
  };
}

/**
 * The views the new page is read through for the inventory (R7), each a call that puts the page
 * in one view and answers whether that view is there. The fixture is the merge of the old page's
 * ten tab panels and its priority picker, one view at a time, so the new page is read the same
 * way: its six presets, Documents' list among them, and the addresses beside them - the
 * conversation, the activity, the collaborators and the artifacts (R3, R12, R19). Some views are
 * the page's own, because what they hold sits behind a control rather than behind an address:
 * the spec opened from Documents' list, where every version, download and reference of a
 * document lives (R2, R3); the identity block's actions menu, where Close and the parent editor
 * live (R11); the priority row's picker, which Details' control opens (R13); and each row of the
 * tree, whose body is what a node opens (R6).
 *
 * A view over a row the tree does not draw answers that it is not there and is not read
 * (`e2e/issue-page-n.ts`), so the walk's depth costs a count and nothing else where the rows run
 * out. Each walk loads the page once and moves through it from there, which is how a reader
 * moves through it: the tree is drawn empty and fills as the reads land, so a walk reads it
 * only once the page has stopped reading.
 */
function inventoryViews(
  page: Page,
  key: string,
  documents: { readonly plan: string; readonly spec: string; readonly verification: string },
  width: number
): (() => Promise<boolean>)[] {
  // How far a walk over the tree reaches. The seeded issue draws up to about 70 rows at 1440 px
  // once every branch is open, and a view over a row the tree does not draw answers that it is
  // not there rather than reading the page the view before it left (`e2e/page-inventory.ts`), so
  // the bound costs a count and nothing else where the rows run out.
  const walkDepth = 80;
  // Which rows of the tree hold a zoom. The zoom walk's first view reads it off the tree it loads
  // and every view of that walk after it reads it back.
  const zoomsInto = new Set<number>();
  return [
    ...PRESET_ORDER.map((preset) => async () => {
      await gotoIssue(page, presetPath(key, preset));
      return true;
    }),
    // The conversation's address mounts the panel's list (`IssuePage.tsx`'s `activatedTabs`),
    // which is in Details, so the view opens it as a reader does (R14).
    async () => {
      await gotoIssue(page, `/issues/${key}/conversation`);
      await openDetails(page);
      return true;
    },
    // The shell's navigation: on the issue page the rail's foot control, or the header's `☰` on a
    // phone, opens the whole of it over the page, which holds every entry the old page's sidebar
    // offered (R14).
    async () => {
      await gotoIssue(page, `/issues/${key}`);
      await page.getByRole("button", { exact: true, name: "Open navigation" }).click();
      await expect(page.getByRole("dialog", { exact: true, name: "Navigation" })).toBeVisible();
      return true;
    },
    // Peek, which the old sidebar's Inbox entry carried and the navigation drawer carries on the
    // issue page, opens the Inbox drawer over the page without changing the address; the view
    // reads the page with that drawer open, and the next view loads the issue again.
    async () => {
      await expect(
        page.getByRole("dialog", { exact: true, name: "Navigation" }),
        "the navigation drawer is not open for Peek"
      ).toBeVisible();
      const address = page.url();
      await page
        .getByRole("button", { exact: true, name: "Peek at the inbox without leaving this page" })
        .click();
      await expect(
        page.getByRole("dialog", { exact: true, name: "Inbox" }),
        "Peek opened no Inbox drawer"
      ).toBeVisible();
      expect(page.url(), "Peek changed the issue's address").toBe(address);
      return true;
    },
    async () => {
      await gotoIssue(page, `/issues/${key}/activity`);
      return true;
    },
    // `/issues/<key>/collaborators`, the address the Collaborators tab answered: it is the one
    // address that opens the panel's list of everyone who worked the issue (R19, `IssuePage.tsx`'s
    // `opensCollaborators`), and the list's own People and Other agents sections
    // (`IssueCollaboratorList.tsx`) are drawn at no other address, so a walk without this view
    // reads that tab's landmarks as lost.
    async () => {
      await gotoIssue(page, `/issues/${key}/collaborators`);
      return true;
    },
    // `/issues/<key>/artifacts`, the address the Artifacts tab answered, opens Documents' list,
    // which carries that tab's list (R2, the Design's "Where each thing goes"): each file with
    // its download and, for a document the issue can drop, Remove, the list's filter, the upload
    // and New document, which a closed issue draws refused.
    async () => {
      await gotoIssue(page, `/issues/${key}/artifacts`);
      await expect(documentsList(page), "/artifacts does not open Documents").toBeVisible();
      return true;
    },
    // The spec opened from Documents' list, in place of the outline (R3): its editor, with its
    // collaborative cursors and its comment chord, its versions with their downloads, what
    // references it, and the version control beside the filters reading it, with the old spec
    // toolbar's link to the block the caret is in and its live connection, which belong to an
    // open editor. The version control opens from its own small control (ruling 5), or from the
    // Outline sheet at 760 px and below, and this view reads the page with it open.
    async () => {
      await openDocumentsFrom(page, `/issues/${key}`);
      await documentLink(page, documents.spec).click();
      await expect(
        openedEditor(page),
        "the spec opened from Documents' list draws no editor"
      ).toBeVisible();
      await openVersionControl(page);
      await expect(
        versionControl(page).getByRole("button", { name: "Copy link to block" })
      ).toHaveCount(1);
      return true;
    },
    // Escape shuts the version control again and leaves the document open; the margin's cards
    // beside the opened spec - its decisions, its comments - are in Details, which a reader opens
    // to read them beside the document (R14).
    async () => {
      await page.keyboard.press("Escape");
      await expect(versionControl(page)).toBeHidden();
      await expect(
        openedEditor(page),
        "Escape on the version control shut the document"
      ).toBeVisible();
      await openDetails(page);
      return true;
    },
    // The plan and the Verification document opened from Documents' list, each with its editor
    // and the block references beside it, which the old Plan and Verification tabs drew open: a
    // reference's source is named by its issue and its document, and lists in the aside only
    // where the document is opened.
    ...[documents.plan, documents.verification].map((name) => async () => {
      await openDocumentsFrom(page, `/issues/${key}`);
      await documentLink(page, name).click();
      await expect(
        openedEditor(page),
        `${name} opened from Documents' list draws no editor`
      ).toBeVisible();
      return true;
    }),
    // The identity block's actions control opens its menu on the issue's own address. Its first
    // check says the control is there before it reaches for it: a click waits for its element
    // with no deadline of its own (Playwright's `actionTimeout` is unset here), so a page
    // drawing no control would spend this row's whole deadline inside one click and report a
    // timeout; asserted first, such a page says which control is missing.
    async () => {
      await gotoIssue(page, `/issues/${key}`);
      const actions = offered(page, "button", CONTROL_LABELS.issue_actions);
      await expect(
        actions,
        `the page draws no ${CONTROL_LABELS.issue_actions} control: its view is unreadable`
      ).toHaveCount(1);
      await actions.click();
      return true;
    },
    // The priority row's picker (dispatch://INFRA-2947 R3), which Details' control opens and no
    // address does: the old page drew that row in its header's rail and its recording opened the
    // picker there (`record-page-inventory.e2e.ts`), and this page draws it in the Details
    // panel's Development section (R13), which a reader opens first (R14). A closed issue's
    // control is refused, so there the view is Details with the menus closed, as the views below
    // are where what they open is already open.
    async () => {
      await closeOpenControls(page);
      await openDetails(page);
      await expect(
        editPriorities(page),
        `the page draws no ${CONTROL_LABELS.edit_priorities} control: its view is unreadable`
      ).toHaveCount(1);
      // No write of the row is out, so only the issue's closed state refuses the control, and the
      // page read that state before Details opened: one read says whether it is refused.
      if (await editPriorities(page).isEnabled()) {
        await editPriorities(page).click();
        await expect(prioritiesPicker(page)).toBeVisible();
      }
      return true;
    },
    // The repository's box, which Details draws in place of the repository's text once the text's
    // button is pressed, with its clear control (`RepoControl.tsx`'s `inRow`). A closed issue's
    // button is refused, so there the view is Details as the views around it leave it, which
    // draws the clear control disabled beside the wrapped text (`clearWhenClosed`).
    async () => {
      await closeOpenControls(page);
      await openDetails(page);
      const text = marginShell(page).getByRole("button", {
        name: new RegExp(`, Repository of ${key}$`),
      });
      await expect(text, "Details draws no button for the repository's text").toHaveCount(1);
      // The page read the issue's state before Details opened, so one read says whether the
      // button is refused, as it is on a closed issue.
      if (await text.isEnabled()) {
        await text.click();
        await expect(offered(page, "combobox", `Repository of ${key}`)).toBeVisible();
      }
      return true;
    },
    // The preset row below 1280 px is one chooser rather than a row of tabs, and the panel is
    // Details, which a reader opens from the header at every width (R14): each is a view of the
    // page, and at a width where it is already open the view is the page with the menus closed,
    // which is the page a reader is left on there.
    async () => {
      await closeOpenControls(page);
      // The view row is drawn with the page an earlier view loaded, in its shape for a width
      // query read as it renders, so one count says whether this width draws the chooser.
      if ((await presetChooser(page).count()) === 1) await presetChooser(page).click();
      return true;
    },
    async () => {
      await closeOpenControls(page);
      await openDetails(page);
      return true;
    },
    // One view per row of the tree, because a tree opens one body at a time and a node's own
    // text, its facts and its controls are in that body (R2). The walk loads the page once and
    // then moves through the rows, which is how a reader moves through them: each view is still
    // read, and still fitted, on its own.
    //
    // At a width that draws the whole tree the walk opens the branches the page opened folded
    // first (R1): a node behind a fold - the check under a requirement - is a twisty away for a
    // reader, and a walk that never pressed one would read it as lost. Below 1024 px the zoom walk
    // below goes into a branch with the row's own key instead.
    ...Array.from({ length: walkDepth }, (_unused, index) => async () => {
      if (index === 0) {
        // The tree is drawn empty and fills as the reads land, so it is read once the page has
        // stopped reading, as `unfoldEveryBranch` asks of its caller.
        const settling = watchApiRequests(page);
        await gotoIssue(page, `/issues/${key}/conversation`);
        await expectLoadSettled(settling);
        if (width >= 1024) await unfoldEveryBranch(page);
        const first = nodeRows(page);
        // The page's reads went quiet above, so the tree is drawn whole and one count says
        // whether it draws a row.
        if ((await first.count()) > 0) await first.first().focus();
        return true;
      }
      const rows = nodeRows(page);
      // The walk's first view waited for a quiet load, and each view since moved the focus
      // alone, which draws no row, so one count holds the rows.
      if ((await rows.count()) <= index) return false;
      await rows.nth(index).focus();
      return true;
    }),
    // Below 1024 px the tree is read one node at a time (R16), so what a reader reaches by
    // zooming the walk reaches by zooming too. The presets first, because a decision, a check and
    // a plan task sit under the issue in the preset that draws them; then one zoom per row of the
    // issue's own address, because a node that hangs off a row - the document a requirement cites
    // - is behind that row's own twisty and nowhere else. At a width that draws the whole tree
    // the views above already open every row, so neither walk runs there. Each walk loads its
    // page once, as the walk above does.
    ...(width >= 1024
      ? []
      : PRESET_ORDER.flatMap((preset) =>
          Array.from({ length: 8 }, (_unused, index) => async () => {
            if (index === 0) {
              // Read once the page has stopped reading, as the walk above is: the tree is drawn
              // empty and fills as the reads land.
              const settling = watchApiRequests(page);
              await gotoIssue(page, presetPath(key, preset));
              await expectLoadSettled(settling);
              const first = nodeRows(page);
              // The page's reads went quiet above, so the tree is drawn whole and one count says
              // whether it draws a row.
              if ((await first.count()) > 0) await first.first().focus();
              return true;
            }
            const rows = nodeRows(page);
            // The walk's first view waited for a quiet load, and each view since moved the focus
            // alone, which draws no row, so one count holds the rows.
            if ((await rows.count()) <= index) return false;
            await rows.nth(index).focus();
            return true;
          })
        )),
    ...(width >= 1024
      ? []
      : Array.from({ length: walkDepth }, (_unused, into) => {
          // Whether this row's zoom was opened: the rows under one are read only where there was
          // one to open.
          let zoomed = false;
          return Array.from({ length: 2 }, (_also, index) => async () => {
            if (index === 0) {
              // A row with no branch of its own has no zoom to walk into, and this walk neither
              // loads the page for it nor reads the page the walk is already on: which rows hold
              // one is read once, off the tree the first view loads, and a row the tree does not
              // draw is not in it either. Every zoom that is there is still opened on a page of
              // its own, because a zoom is left by loading the issue again.
              if (into === 0) {
                // Read off a page that has stopped reading: the tree is drawn empty and fills as
                // the reads land (`expectLoadSettled`), so a set taken the moment the navigation
                // returns holds the twisties of a half-filled tree, and a row missed here is a
                // node this state never zooms into. Measured on a loaded box at 390 px: a
                // document's own copy control read as lost on the attempt and was found on the
                // retry.
                const settling = watchApiRequests(page);
                await gotoIssue(page, `/issues/${key}/conversation`);
                await expectLoadSettled(settling);
                zoomsInto.clear();
                const rows = nodeRows(page);
                // The page's reads went quiet above, so the tree is drawn whole and one count
                // holds its rows.
                const drawn = Math.min(await rows.count(), walkDepth);
                for (let at = 0; at < drawn; at += 1) {
                  const fold = rows.nth(at).getByRole("button", { name: CONTROL_LABELS.unfold });
                  // The tree was drawn whole above, and a row draws its twisty with it, so one
                  // count says whether this row has one.
                  if ((await fold.count()) > 0) zoomsInto.add(at);
                }
              } else if (!zoomsInto.has(into)) {
                return false;
              } else {
                await gotoIssue(page, `/issues/${key}/conversation`);
              }
              if (!zoomsInto.has(into)) return into === 0;
              // A twisty opens a branch in place at every width, and zooming is the row's own key
              // (`.`, `IssueTree.tsx`), pressed with the focus on the row.
              await nodeRows(page).nth(into).focus();
              await page.keyboard.press(".");
              await expect(
                zoomCrumbs(page),
                `row ${into}'s zoom key zoomed nowhere`
              ).not.toHaveCount(0);
              zoomed = true;
            } else if (!zoomed) {
              return false;
            }
            // The zoomed node is the first row; its own nodes follow it, and the first of them -
            // a check under the row's requirement - is read by the view that opened the zoom.
            const under = nodeRows(page);
            // The zoom was waited for where it was opened, and the focus alone moved since,
            // which draws no row, so one count holds the zoom's rows.
            if ((await under.count()) > index + 1) await under.nth(index + 1).focus();
            return true;
          });
        }).flat()),
  ];
}

/** Opens Documents' list from `path`, the way a reader does at any width: the issue's page, then
 *  the view row's Documents. */
async function openDocumentsFrom(page: Page, path: string): Promise<void> {
  await gotoIssue(page, path);
  await openDocuments(page);
}

/**
 * One row of "the new page carries every element of the old one" (R7, c-inventory): the fixture's
 * `state` read against the new page at that state's width, and then the writes of that state
 * (`writes`), each made through the page and read back from Dispatch (c-nothing-lost). The
 * fixture was recorded in a desktop browser, so the 1440 px rows run in the desktop project alone
 * - an iPhone profile at 1440 px draws a different shell around the same page - and the 390 px
 * rows, which carry `@phone`, run in both.
 */
async function carryEveryElement(
  browser: Browser,
  state: InventoryState,
  writes: (page: Page, seeded: AdvancingIssueSeed, specName: string) => Promise<void>
): Promise<void> {
  const fixture = JSON.parse(
    await readFile(
      fileURLToPath(new URL("./fixtures/issue-page-inventory.json", import.meta.url)),
      "utf8"
    )
  ) as PageInventoryFixture;
  test.info().annotations.push({ description: fixture.commit, type: "fixture recorded at" });
  const size = state.endsWith("-1440") ? DESKTOP : PHONE;
  await phase(browser, seedWholeIssueAdvancing, async (page, seeded) => {
    // The documents' names, which their rows in Documents' list carry.
    const documents = {
      plan: (await getArtifact(seeded.planArtifactId)).name,
      spec: (await getArtifact(seeded.specArtifactId)).name,
      verification: (await getArtifact(seeded.verificationArtifactId)).name,
    };
    const specName = documents.spec;
    await page.setViewportSize({ height: size.height, width: size.width });
    if (state.startsWith("closed")) {
      await patchIssue(seeded.key, { status: "done" });
      // Closing settles the issue's documents: Dispatch saves the spec's open question resolved,
      // as the spec's next version (`docs/settlement.go`), and the old page was recorded after it,
      // so the fixture's closed states hold "Version 3" and its open states do not. The settlement
      // the seed's last write armed runs about 17 s after it where the spec's room is open, as a
      // page that shows the spec opens it; where no one has the room open it waits for the
      // resumption pass, a minute or two on (`docs/resume.go`), which made the row's wait
      // unsteady. So the page opens the spec first, and the row reads once the version exists, as
      // the recording did, so the page's own read of the versions finds the same three.
      await gotoIssue(page, presetPath(seeded.key, "spec"));
      await expect
        .poll(async () => ((await getArtifact(seeded.specArtifactId)).versions ?? []).length, {
          message: "Dispatch wrote no settlement version of the closed issue's spec",
          timeout: 180_000,
        })
        .toBe(3);
    }
    await assertInventoryCarried(page, fixture[state], {
      awaiting: [...AWAITING_THE_BOX, ...AWAITING_THREADS],
      outline: inventoryOutline(page, seeded.key, size.width),
      parentRows: INVENTORY_PARENT_ROWS,
      renames: INVENTORY_RENAMES,
      rows: INVENTORY_ROWS,
      state,
      views: inventoryViews(page, seeded.key, documents, size.width),
    });
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    if (state.startsWith("closed")) await expectClosedClear(page, seeded.key);
    await writes(page, seeded, specName);
  });
}

/**
 * A closed issue's own repository, read in Details (R7): today's page draws its Clear control
 * disabled, and Details draws the same one beside the wrapped value, under the recorded name. The
 * name is read exactly, there is one, and it is disabled at the width the state is read at, so a
 * Details that draws none, draws two, or draws it enabled fails here, where the inventory's own
 * read finds a name and not its state.
 */
async function expectClosedClear(page: Page, key: string): Promise<void> {
  await openDetails(page);
  const clear = page.getByRole("button", { exact: true, name: `Clear the repository of ${key}` });
  await expect(clear, `Details draws one Clear of ${key}'s repository`).toHaveCount(1);
  await expect(clear, "the Clear of a closed issue is disabled").toBeDisabled();
  await closeDetails(page);
}

/**
 * The writes of a closed issue (c-nothing-lost): what its page still takes - the reader's own pin
 * and their subscribers' Unsubscribe, which are no write of the issue - and Reopen, which only a
 * closed issue draws. Close, Reopen and the parent editor are behind the identity block's one
 * actions control now (R11), so each is reached through it.
 */
async function writeFromAClosedIssue(page: Page, seeded: AdvancingIssueSeed): Promise<void> {
  // The pin is the overflow menu's.
  await openIssueActions(page);
  await offered(page, "button", "Unpin issue").click();
  await expect.poll(async () => (await getMyState("alice"))[seeded.key]?.pinned).toBe(false);

  await openDetails(page);
  await offered(page, "button", "Subscribers: 3").click();
  await page
    .getByRole("button", { name: /Unsubscribe/ })
    .first()
    .click();
  // Unsubscribing asks first: Confirm is the only path that removes a subscriber
  // (`UnsubscribeDialog.tsx`).
  await page
    .getByRole("dialog", { name: "Unsubscribe" })
    .getByRole("button", { name: "Confirm" })
    .click();
  await expect.poll(async () => (await getUnsubscribeCalls()).length).toBeGreaterThan(0);
  await closeDetails(page);

  await openIssueActions(page);
  await offered(page, "button", CONTROL_LABELS.reopen_into_backlog).click();
  await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");
}

/**
 * The writes of an open issue (c-nothing-lost): each recorded name that writes today still writes,
 * made through the page and the value read back from Dispatch. Close, Reopen and the parent
 * editor are behind the identity block's one actions control now (R11), so each is reached
 * through it; the upload and New document are Documents' list's (R2), and a document's text is
 * edited in the document opened from it (R3).
 */
async function writeFromAnOpenIssue(
  page: Page,
  seeded: AdvancingIssueSeed,
  specName: string
): Promise<void> {
  // Backlog and not Todo: the issue is in progress under a parent in Triage, and a move to
  // another status of work stops at the dialog that asks for the parents to be moved too
  // (`ParentStatusDialog.tsx`), which no write of c-nothing-lost goes through.
  await identityBlock(page).getByRole("combobox", { name: "Status" }).selectOption("backlog");
  await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("backlog");

  await offered(page, "combobox", `Priority of ${seeded.key}`).selectOption("3");
  await expect.poll(async () => (await getIssue(seeded.key)).priority).toBe(3);

  // A picker whose list is read when the reader reaches for it - the people and the
  // repositories (`AssigneeControl.tsx`, `RepoControl.tsx`) - is reached for first, as a reader
  // reaches for it; without that it offers the value it already holds and nothing else.
  const assignee = offered(page, "combobox", `Assignee of ${seeded.key}`);
  await assignee.focus();
  await expect(assignee.locator('option[value="bob"]')).toHaveCount(1);
  await assignee.selectOption("bob");
  await expect.poll(async () => (await getIssue(seeded.key)).assignee).toBe("bob");
  // The issue goes back to the reader: an approval request waits on the issue's assignee, and the
  // header draws the chip's two actions only for the reader it waits on (`waitingOnReader`).
  await assignee.selectOption("alice");
  await expect.poll(async () => (await getIssue(seeded.key)).assignee).toBe("alice");

  // The labels' control is Details' Labels row; the header draws the labels that fit (R13). The
  // assignee's select still holds the focus and takes Shift+M as the letter it types, so Details
  // opens from the header's own control, as a reader's press opens it.
  await offered(page, "button", CONTROL_LABELS.details).click();
  await expect(marginShell(page), "Details did not open from its control").toBeVisible();
  await offered(page, "button", "Edit labels").click();
  await page.getByRole("combobox", { name: "Search or create label" }).fill("carried");
  await page
    .getByRole("option", { name: /carried/ })
    .first()
    .click();
  await page.keyboard.press("Escape");
  await expect.poll(async () => (await getIssue(seeded.key)).labels).toContain("carried");
  await closeDetails(page);

  // The parent's editor opens in Details' Parent row, from the header's overflow menu.
  await openIssueActions(page);
  await offered(page, "button", "Edit parent issue").click();
  await page.getByRole("textbox", { name: "Parent" }).fill("");
  await offered(page, "button", CONTROL_LABELS.save_parent).click();
  await expect.poll(async () => (await getIssue(seeded.key)).parent).toBeNull();

  // The route and its editor are Details' Route row (R13), which the parent left open.
  await offered(page, "button", `Messages default to ${PLANNER_TITLE}`).click();
  const route = page.getByRole("textbox", { name: "Messages go to" });
  await route.fill("role:reviewer");
  await offered(page, "button", CONTROL_LABELS.save_route).click();
  await expect.poll(async () => (await getIssue(seeded.key)).route).toBe("role:reviewer");

  // The repository and every property under it are in Details (R13, R14). The repository is the
  // row's text, whose button draws the box in its place, and what is typed in the box is written
  // when Enter is pressed (`RepoControl.tsx`).
  await openDetails(page);
  const repoBefore = (await getIssue(seeded.key)).repo;
  await offered(page, "button", `${repoBefore}, Repository of ${seeded.key}`).click();
  const repository = offered(page, "combobox", `Repository of ${seeded.key}`);
  await repository.fill("acme/carried");
  await repository.press("Enter");
  await expect.poll(async () => (await getIssue(seeded.key)).repo).toBe("acme/carried");

  await offered(page, "button", `Edit components of ${seeded.key}`).click();
  await page.getByRole("option", { name: "Issue panel" }).first().click();
  await page.keyboard.press("Escape");
  await expect.poll(async () => (await getIssue(seeded.key)).components.ids).not.toContain("panel");
  // The header's own controls are written with Details closed, which on a phone covers them.
  await closeDetails(page);

  // The pin is the overflow menu's.
  await openIssueActions(page);
  await offered(page, "button", "Unpin issue").click();
  await expect.poll(async () => (await getMyState("alice"))[seeded.key]?.pinned).toBe(false);

  // The claim and its Release are Details' (R13).
  await openDetails(page);
  await expect(offered(page, "button", `Release the claim on ${seeded.key}`)).toBeVisible();
  await offered(page, "button", `Release the claim on ${seeded.key}`).click();
  await expect.poll(async () => (await getIssue(seeded.key)).claim).toBeNull();
  await closeDetails(page);

  await openIssueActions(page);
  await offered(page, "button", "Close issue").click();
  // The issue holds open children, so closing it asks first (`ParentStatusDialog.tsx`): Move them
  // with it closes the children too, and the issue's own status is read back from Dispatch.
  await page
    .getByRole("dialog", { name: `Move ${seeded.key} to Done?` })
    .getByRole("button", { name: "Move them with it" })
    .click();
  await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("done");
  await openIssueActions(page);
  await offered(page, "button", CONTROL_LABELS.reopen_into_backlog).click();
  await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");

  await openDetails(page);
  await offered(page, "button", "Subscribers: 3").click();
  await page
    .getByRole("button", { name: /Unsubscribe/ })
    .first()
    .click();
  // Unsubscribing asks first: Confirm is the only path that removes a subscriber
  // (`UnsubscribeDialog.tsx`).
  await page
    .getByRole("dialog", { name: "Unsubscribe" })
    .getByRole("button", { name: "Confirm" })
    .click();
  await expect.poll(async () => (await getUnsubscribeCalls()).length).toBeGreaterThan(0);

  // The approval: the seed requests none, so the planner requests it here, and it waits on the
  // reader, who approves it from the page.
  await requestApproval(seeded.specArtifactId, { summary: "Read it back." }, asPlanner);
  await openPreset(page, seeded.key, "spec");
  // Approve is the identity block's own chip, drawn with its two actions where the request waits
  // on the reader (`IssuePropertiesRow.tsx`); the preset's toolbar draws how the document stands
  // and no action (`features/doc/ApprovalChip.tsx`'s `showActions`).
  await page
    .getByTestId("issue-state-actions")
    .getByRole("button", { name: /^Approve( v\d+)?$/ })
    .click();
  await expect
    .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state)
    .toBe("approved");

  // Keep and Change, on the decision node they moved to. Both are named by the inventory's own
  // words, so they are still read by role and name - but scoped to one decision row. `Keep` is
  // drawn only while a decision waits for review and `Change` wherever the decision's quote has
  // resolved (`DecisionActions.tsx`), so a page whose second decision has its quote offers two
  // `Change` controls and one `Keep`, and an unscoped read of `Change` is ambiguous rather than
  // wrong. The row that offers `Keep` is the decision this phase is about.
  await openPreset(page, seeded.key, "decisions");
  const deciding = page
    .locator('[data-node-row][data-node-type="decision"]')
    .filter({ has: offered(page, "button", CONTROL_LABELS.keep_decision) });
  // `getAttribute` waits for a decision row that draws Keep, and the seed leaves one decision
  // waiting for review, so the row it finds is that decision's.
  const decidingId = await deciding.first().getAttribute("data-node-id");
  expect(
    decidingId,
    "exactly one decision waits for review, and it is the one Keep names"
  ).not.toBeNull();
  const decidingRow = page.locator(`[data-node-row][data-node-id="${decidingId}"]`);
  const decidingBlock = decidingId?.split("#b-")[1];
  await decidingRow
    .getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
    .click();
  // Keep's own write, read back from Dispatch, as every write of this walk is: the press is what
  // this step proves, and a press the page lost used to leave the decision waiting while the step
  // went on to Change regardless (`NodeRow.tsx`, the press that scrolled).
  await expect
    .poll(
      async () =>
        ((await getIssueDecisions(seeded.key)).decisions ?? []).find(
          (decision) => decision.block_id === decidingBlock
        )?.kept ?? null,
      { message: "Keep was pressed and Dispatch recorded no Keep of that decision" }
    )
    .not.toBeNull();
  // The page has taken the write once Keep is gone from the row. The row Keep settled draws no
  // Change on this page, and three decisions wait for review, so Change is pressed on another
  // decision row that draws it: the first row of the page with a Change in it.
  await expect(
    decidingRow.getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
  ).toHaveCount(0);
  await page
    .locator('[data-node-row][data-node-type="decision"]')
    .filter({
      has: page.getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision }),
    })
    .first()
    .getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision })
    .click();
  // The change is written in the decision's own composer, which names its box and its send apart
  // from the conversation's (`DecisionActions.tsx`, R7).
  await page
    .getByRole("textbox", { name: CONTROL_LABELS.your_change })
    .fill("Change this decision.");
  await offered(page, "button", CONTROL_LABELS.send_change).click();
  await expect
    .poll(async () => (await listComments(seeded.key)).map((comment) => comment.body))
    .toContain("Change this decision.");

  // The upload and New document are Documents' list's (R2). A file picked is staged behind its
  // summary field until its own Upload confirms it (`ArtifactUploadRow` in
  // `features/artifacts/ArtifactUpload.tsx`), as on every page that uploads an issue's file.
  await openDocumentsFrom(page, `/issues/${seeded.key}`);
  await uploadInDocuments(page, {
    buffer: Buffer.from("# Carried\n"),
    mimeType: "text/markdown",
    name: "carried.md",
  });
  await expect
    .poll(async () => ((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.name))
    .toContain("carried.md");
  await documentsList(page)
    .getByRole("button", { exact: true, name: CONTROL_LABELS.new_document })
    .click();
  await documentsList(page)
    .getByRole("textbox", { exact: true, name: "Title" })
    .fill("Carried new");
  await documentsList(page).getByRole("button", { exact: true, name: "Create" }).click();
  await expect
    .poll(async () =>
      ((await getIssue(seeded.key)).artifacts ?? []).some((one) => one.name.includes("Carried new"))
    )
    .toBe(true);

  // No write through the composer: on the new page it is part L's box, which follows this part
  // (part N's R11) and is in `AWAITING_THE_BOX`. Part L's pull request writes a comment in the
  // box here and reads it back. Nor is a message to an agent written: today's way is the
  // composer with an @ mention, and the new page's is Threads' New thread, which follows this part
  // too (`AWAITING_THREADS`).

  // Back to Conversation's address, where the tree opens on the spec's nodes.
  await gotoIssue(page, `/issues/${seeded.key}/conversation`);
  const specSlug = (await getArtifact(seeded.specArtifactId)).slug;

  await nodeRow(page, blockNodeId(specSlug, requirementBlockId(3)))
    .first()
    .focus();
  await page.keyboard.press("Enter");
  // The words go where the caret is, so they wait for it to land in the row's own text: typed
  // before it does, the space and the capital reach the page's own keys and the rest of the words
  // land in whatever those keys opened.
  await expect
    .poll(() => caretInNode(page), { message: "Enter did not put the caret in requirement 3" })
    .toBe(true);
  await page.keyboard.type(" Edited in place.");
  await expect
    .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
    .toContain("Edited in place.");

  // Naming a version, from the version control beside the filters, which the Spec filter draws
  // reading the spec; with no filter on none is drawn (R2).
  await page.keyboard.press("Escape");
  await pickPreset(page, PRESET_NAMES.spec);
  await openVersionControl(page);
  await versionControl(page).getByRole("button", { name: "Name version" }).click();
  const naming = page.getByRole("dialog", { name: "Name this version" });
  await naming.getByRole("textbox", { name: "What changed?" }).fill("Carried over");
  await naming.getByRole("button", { name: /^Save/ }).click();
  await expect
    .poll(async () =>
      ((await getArtifact(seeded.specArtifactId)).versions ?? []).map((one) => one.summary)
    )
    .toContain("Carried over");

  // Editing a document's text in the document opened from Documents' list, in place of the
  // outline, with its own editor.
  await openDocumentsFrom(page, `/issues/${seeded.key}`);
  await documentLink(page, specName).click();
  // The caret goes where a reader's click after a requirement's own words puts it: text the
  // document holds as editable, inside the requirement's block.
  await placeCaret(page, "after", requirementText(2), openedEditor(page));
  await page.keyboard.type(" Edited where it was opened.");
  await expect
    .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
    .toContain("Edited where it was opened.");

  // The open question is answered where the new page offers an answer today: Peek, which the
  // navigation drawer carries (R7), opens the Inbox drawer over the issue, with the same ask's own
  // card, whose option and Answer button write it. The outline's row writes nothing yet: its
  // controls set the line at the foot of the outline (`slots/foot.tsx`, part L's box), which
  // draws nothing while L follows, so the answer is not a write of the outline and this step does
  // not read it as one. What it reads is Dispatch's own record: the ask answered with the option
  // picked, and the issue's address as Peek found it.
  await gotoIssue(page, `/issues/${seeded.key}`);
  const address = page.url();
  await page.getByRole("button", { exact: true, name: "Open navigation" }).click();
  await page
    .getByRole("dialog", { exact: true, name: "Navigation" })
    .getByRole("button", { exact: true, name: "Peek at the inbox without leaving this page" })
    .click();
  const card = page
    .getByRole("dialog", { exact: true, name: "Inbox" })
    .getByTestId(`ask-${seeded.openQuestion.askId}`);
  const option = card.getByRole("radio").first();
  await option.check();
  await card.getByRole("button", { name: /^Answer/ }).click();
  await expect
    .poll(async () => (await getAsk(seeded.openQuestion.askId)).ask.state)
    .toBe("answered");
  const answered = (await getAsk(seeded.openQuestion.askId)).ask;
  expect(answered.answer?.selected, "the answer stores the option that was picked").toEqual([
    answered.options?.[0]?.label,
  ]);
  expect(page.url(), "answering in the Inbox drawer changed the issue's address").toBe(address);
}

// c-nothing-lost, row by row of the fixture's four states (R7). Each row loads the page about 18
// times at 390 px and 12 at 1440 px, naming 128 and 40 views, each settled over two readings of
// its accessibility tree, and then makes its state's writes. The old page's one row held all four
// states and measured 8.2 minutes, which outlasts the browser step's budget in CI: each row of
// the four ends well inside it.
test("the new page carries every element of the old one: open at 1440 px", async ({ browser }) => {
  // Measured in an e2e-rows Job, one worker, alone: 1.5 min on chromium, which loads the page
  // 12 times and makes the open writes; the bound is four times that, for a slow leg.
  test.setTimeout(360_000);
  await carryEveryElement(browser, "open-1440", writeFromAnOpenIssue);
});

test("the new page carries every element of the old one: closed at 1440 px", async ({
  browser,
}) => {
  // Measured in an e2e-rows Job, one worker, alone: 2.5 min on chromium, which includes the wait
  // of about 17 s for the settlement version a close brings; the bound is four times that, for a
  // slow leg.
  test.setTimeout(600_000);
  await carryEveryElement(browser, "closed-1440", writeFromAClosedIssue);
});

test("the new page carries every element of the old one: open at 390 px @phone", async ({
  browser,
}) => {
  // Measured in an e2e-rows Job, one worker, alone: 2.6 min on chromium and 1.9 min on iphone,
  // which load the page about 18 times at 390 px and make the open writes; the bound is about
  // four times the longer, for a slow leg.
  test.setTimeout(600_000);
  await carryEveryElement(browser, "open-390", writeFromAnOpenIssue);
});

test("the new page carries every element of the old one: closed at 390 px @phone", async ({
  browser,
}) => {
  // Measured in an e2e-rows Job, one worker, alone: 3.0 min on chromium and 3.1 min on iphone,
  // which include the wait of about 17 s for the settlement version a close brings; the bound is
  // about four times the longer, for a slow leg.
  test.setTimeout(720_000);
  await carryEveryElement(browser, "closed-390", writeFromAClosedIssue);
});

/** How many characters of an id an address writes short: the fewest the server resolves. */
const SHORT_ID = 8;

/**
 * A second comment of the issue `key` whose id shares its first eight characters with the
 * comment `id`, so one short id names both. Ids are minted, so two that a prefix names cannot be
 * made through the API: the row is written straight into Postgres as a copy of that comment with
 * an id and a body of its own, as `prefix-links.e2e.ts` writes its twin.
 */
async function twinComment(id: string, body: string): Promise<string> {
  const twin = `${id.slice(0, SHORT_ID)}-1111-4111-8111-111111111111`;
  await sql(
    `create temporary table twin_comment as select * from comments where id = '${id}'`,
    `update twin_comment set id = '${twin}', body = '${body}', created_at = now()`,
    "insert into comments select * from twin_comment",
    "drop table twin_comment"
  );
  return twin;
}

/** The block id of the typed block `addressSeed`'s document of tasks ends with, and how many
 *  lines of the document stand before it. */
const TASKS_BLOCK = "t-far";
const TASKS_BEFORE_THE_BLOCK = 60;

/** The records INV-1's addresses are written with (`AddressSeed`): the whole issue's, and an
 *  agents' comment, a twin comment, a comment of another issue, an approval request, a document
 *  of tasks, a message of the Legion daemon and a handoff, written here beside it. */
async function addressSeed(seeded: WholeIssueSeed): Promise<AddressSeed> {
  const issue = await getIssue(seeded.key);
  const notes = (issue.artifacts ?? []).find(
    (artifact) => artifact.slug === seeded.documents.cited
  );
  if (notes === undefined) throw new Error(`${seeded.key} holds no ${seeded.documents.cited}`);
  const notesWords = "The measurements behind the first requirement.";
  const { markdown } = await getArtifactText(notes.id);
  const paragraph = (await getArtifactBlocks(notes.id)).find((block) =>
    markdown.slice(block.from, block.to).includes(notesWords)
  );
  if (paragraph === undefined) throw new Error(`${notes.slug} holds no paragraph of its words`);
  const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
  const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
  const agentsOnly = await createComment(
    seeded.key,
    { body: "Planner to reviewer, outside every thread a person wrote in." },
    asPlanner
  );
  const [thread = ""] = seeded.comments.threads;
  const twin = await twinComment(thread, "The second comment the prefix names.");
  const otherWords = "A comment its own issue holds.";
  const other = await createComment(seeded.parentKey, { body: otherWords });
  const approval = withOpenedAsk(await requestApproval(seeded.specArtifactId, {}, asPlanner));
  // A document of tasks that is no plan of the issue (decision 449): ordinary, so the outline
  // draws no node of it, with its one typed block after so many lines that the block is off
  // screen until a link to it scrolls there.
  const taskWords = "Draw the last step of the tasks.";
  const tasks = await createIssueArtifact(seeded.key, {
    content: `${[
      "# Tasks",
      ...Array.from(
        { length: TASKS_BEFORE_THE_BLOCK },
        (_, index) => `Step ${index + 1} of the tasks, in a line of its own.`
      ),
      `:::plan_task{#${TASKS_BLOCK}}\n${taskWords}\n:::`,
    ].join("\n\n")}\n`,
    name: "plan-one-plan.md",
  });
  // A message the Legion daemon of the issue's project wrote with no target, and a handoff.
  const daemonWords = "The daemon handed the build to its next step.";
  const daemon = await createMessage(
    seeded.key,
    { body: daemonWords },
    {
      actor: {
        id: `legion-daemon:${seeded.key.slice(0, seeded.key.indexOf("-"))}`,
        kind: "session",
      },
      as: "agent",
    }
  );
  const handoff = await createHandoff(
    seeded.key,
    "plan",
    { phase: "plan", schemaVersion: 1 },
    plannerActor
  );
  return {
    agentsOnly: agentsOnly.id,
    approval: { artifactId: seeded.specArtifactId, askId: approval.ask.id },
    comment: {
      id: seeded.comments.anchored,
      short: seeded.comments.anchored.slice(0, SHORT_ID),
      words: "This requirement is the one the tree reads first.",
    },
    daemon: { id: daemon.id, words: daemonWords },
    goneAsk: "00000000-0000-4000-8000-000000000000",
    handoff: handoff.id,
    key: seeded.key,
    message: { id: seeded.messages.root, words: "The tree is folded to the frontier." },
    notes: { paragraph: paragraph.id, slug: notes.slug, words: notesWords },
    openQuestion: {
      askId: seeded.openQuestion.askId,
      node: blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID),
      short: seeded.openQuestion.askId.slice(0, SHORT_ID),
      words: "At which width does the identity block condense?",
    },
    otherIssue: { comment: other.id, key: seeded.parentKey, words: otherWords },
    planTask: {
      block: planTaskBlockId(9),
      node: blockNodeId(planSlug, planTaskBlockId(9)),
    },
    spec: { slug: specSlug, words: requirementText(2) },
    tasks: { block: TASKS_BLOCK, slug: tasks.artifact.slug, words: taskWords },
    twins: { ids: [thread, twin], prefix: thread.slice(0, SHORT_ID) },
  };
}

/** Reads that the page at `address`, its form `form` (the address after the issue's own), opens
 *  `content` (INV-1's `AddressContent`), failing with the form and what it does not open. */
async function expectAddressOpens(
  page: Page,
  address: string,
  form: string,
  content: AddressContent
): Promise<void> {
  const editors = page.getByRole("region", { name: "Document editor" });
  switch (content.kind) {
    case "view": {
      await expectIssuePageDrawn(page);
      await expect
        .poll(() => activePreset(page), {
          message: `${form} does not open ${PRESET_NAMES[content.preset]}`,
        })
        .toBe(PRESET_NAMES[content.preset]);
      await expect(editors, `${form} opens a document in place of the outline`).toHaveCount(0);
      if (content.preset === "conversation") {
        await expect(historyPanel(page), `${form} opens the history`).toHaveCount(0);
      }
      return;
    }
    case "documents": {
      await expect(documentsList(page), `${form} does not open Documents`).toBeVisible();
      await expect
        .poll(() => activePreset(page), { message: `${form} does not open Documents` })
        .toBe(PRESET_NAMES.documents);
      await expect(tree(page), `${form} draws the outline beside Documents' list`).toHaveCount(0);
      return;
    }
    case "document": {
      await expect(openedEditor(page), `${form} opens no document's editor`).toBeVisible();
      await expect(openedArtifact(page), `${form} does not open that document`).toContainText(
        content.words
      );
      await expect(tree(page), `${form} draws the outline beside the document`).toHaveCount(0);
      await expect(
        documentsList(page),
        `${form} draws Documents' list beside the document`
      ).toHaveCount(0);
      return;
    }
    case "document-block": {
      await expect(openedEditor(page), `${form} opens no document's editor`).toBeVisible();
      await expect(
        openedEditor(page).locator(`[data-block-id="${content.block}"]`),
        `${form} does not open its document at the block`
      ).toBeInViewport();
      await expect(openedEditor(page)).toContainText(content.words);
      await expect(
        documentsList(page),
        `${form} draws Documents' list in place of the document`
      ).toHaveCount(0);
      return;
    }
    case "document-item": {
      await expect(openedEditor(page), `${form} opens no document's editor`).toBeVisible();
      await expect(page, `${form} leaves its address`).toHaveURL(address);
      await expect(
        page.locator(`[data-margin-item="${content.id}"]`),
        `${form} does not mark its item beside the document`
      ).toHaveAttribute("aria-current", "true");
      return;
    }
    case "version": {
      await expectIssuePageDrawn(page);
      await openVersionControl(page);
      // The picker's value, not the toolbar's text: a `select`'s option texts are part of its
      // container's `innerText`, so "Version 1" is in that text whichever version is drawn.
      await expect(versionPicker(page), `${form} does not open that version`).toHaveValue(
        String(content.version)
      );
      return;
    }
    case "words": {
      await expectIssuePageDrawn(page);
      await openFilterBox(page);
      await expect(filterBox(page), `${form} does not fill the filter by words`).toHaveValue(
        content.words
      );
      return;
    }
    case "node": {
      await expectIssuePageDrawn(page);
      await expect
        .poll(() => focusedNodeId(page), { message: `${form} focuses no node for it` })
        .toBe(content.node);
      await expect
        .poll(() => onScreen(nodeRow(page, content.node).first()), {
          message: `${form} leaves its node off screen`,
        })
        .toBe(true);
      if (content.words !== undefined) {
        await expect(
          nodeRow(page, content.node).first(),
          `${form} does not draw its node's text`
        ).toContainText(content.words);
      }
      await expect(editors, `${form} opens a document in place of the outline`).toHaveCount(0);
      return;
    }
    case "children": {
      await expectIssuePageDrawn(page);
      await expect(childRows(page).first(), `${form} draws no row a child`).toBeVisible();
      const first = await childRows(page).first().getAttribute("data-node-id");
      await expect
        .poll(() => focusedNodeId(page), {
          message: `${form} does not focus the group's first row`,
        })
        .toBe(first);
      expect(
        await onScreen(childRows(page).first()),
        `${form} leaves the group's first row off screen`
      ).toBe(true);
      return;
    }
    case "details": {
      await expectIssuePageDrawn(page);
      await expect(marginShell(page), `${form} does not open Details`).toBeVisible();
      if (content.part === "history") {
        await expect(
          historyPanel(page),
          `${form} does not open Details at its history`
        ).toBeVisible();
      } else if (content.part === "collaborators") {
        await expect(
          marginShell(page)
            .locator('[data-details-row="collaborators"]')
            .getByRole("button", { exact: true, name: PROPERTY_LABELS.collaborators }),
          `${form} does not open Details at the collaborators`
        ).toBeFocused();
      } else {
        await expect(
          stepsSection(page),
          `${form} does not open Details at its steps`
        ).toBeInViewport();
        await expect(
          stepsSection(page).getByRole("heading", { level: 2 }),
          `${form} does not put the focus on the steps`
        ).toBeFocused();
      }
      return;
    }
    case "history-turn": {
      await expect(
        historyItem(page, content.id),
        `${form} marks no turn of the history`
      ).toHaveAttribute("aria-current", "true");
      await expect(historyItem(page, content.id)).toContainText(content.words);
      expect(await nodeRow(page, content.id).count(), `${form} draws the turn as a row`).toBe(0);
      return;
    }
    case "approval": {
      await expect(
        identityBlock(page).locator(`[data-approval-artifact="${content.artifact}"]`),
        `${form} does not focus the approval the header draws`
      ).toBeFocused();
      await expect(page, `${form} leaves its address`).toHaveURL(address);
      return;
    }
    case "matches": {
      await expect(
        page.getByRole("heading", { name: "Several items match that link" }),
        `${form} lists no items`
      ).toBeVisible();
      for (const id of content.ids) {
        await expect(
          page.locator(`a[href$="/comments/${id}"]`),
          `${form} does not list ${id}`
        ).toHaveCount(1);
      }
      await expect(page, `${form} leaves its address`).toHaveURL(address);
      return;
    }
    case "not-found": {
      await expect(
        page.getByRole("heading", { exact: true, name: content.heading }),
        `${form} does not say "${content.heading}"`
      ).toBeVisible();
      return;
    }
    case "retry": {
      await expect(
        page.getByRole("button", { name: "Retry" }),
        `${form} offers no Retry for its failed lookup`
      ).toBeVisible();
      return;
    }
    case "control": {
      await expect(issuePage(page), `${form} draws no page`).toHaveCount(1);
      await expect
        .poll(() => activePreset(page), { message: `${form} does not open ${content.name}` })
        .toBe(content.name);
      return;
    }
    case "handoff": {
      await expectIssuePageDrawn(page);
      await expect(page, `${form} leaves its address`).toHaveURL(address);
      await expect(
        page.getByRole("heading", { name: /not found/iu }),
        `${form} refuses the handoff`
      ).toHaveCount(0);
      return;
    }
    case "replaced": {
      await expect(page, `${form} is not replaced by ${content.to}`).toHaveURL(content.to);
      await expectAddressOpens(page, content.to, form, content.lands);
      return;
    }
  }
}

// c-urls; c-addresses (1): INV-1, every address of today's page and each that root R20 adds, and
// what it opens, at the project's width: 1440 px on chromium and 390 px on iphone. The addresses
// of the views other parts build are read only where the page draws that part
// (`PART_ADDRESSES`); the rest are named as not read.
test("every address of the old page still resolves and each new one opens its view @phone", async ({
  browser,
}) => {
  // About forty cold loads of the issue page, each one a navigation and its reads: the source's
  // eighteen took 28 s, so the row carries a deadline of its own rather than the 60 s default.
  test.setTimeout(300_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const seed = await addressSeed(seeded);
    const desktopProject = test.info().project.name !== "iphone";
    await page.setViewportSize(viewportAt(desktopProject ? DESKTOP.width : IPHONE_13.width));
    // Every address is read before the row fails, so one run names each one that does not
    // resolve, with the first line of what it does not open.
    const unresolved: string[] = [];
    const resolves = async (form: string, read: () => Promise<void>) => {
      try {
        await read();
      } catch (error) {
        unresolved.push(error instanceof Error ? (error.message.split("\n")[0] ?? form) : form);
      }
    };
    for (const one of [...Object.values(OLD_PAGE_ADDRESSES).flat(), ...PART_ADDRESSES]) {
      // Once Dashboard is drawn, the bare address opens it, and its own case reads that.
      if (one.until !== undefined && drawsPart(one.until)) continue;
      const address = one.address(seed);
      const form = address.slice(`/issues/${seed.key}`.length) || `/issues/${seed.key}`;
      if (one.builtBy !== undefined && !drawsPart(one.builtBy)) {
        // The page draws no part that builds this view: the address is named as not read.
        test.info().annotations.push({ description: `${form} (${one.builtBy})`, type: "not-read" });
        continue;
      }
      const unreadable = one.unreadable?.(seed);
      const failing = (url: URL) => unreadable?.(url) === true;
      if (unreadable !== undefined) {
        await page.route(failing, (route) =>
          route.fulfill({ json: { code: "SERVER_ERROR", error: "unreadable" }, status: 500 })
        );
      }
      await resolves(form, async () => {
        await gotoIssue(page, address);
        await expectAddressOpens(page, address, form, one.opens(seed));
      });
      if (unreadable !== undefined) await page.unroute(failing);
    }

    // `t e`, the Steps tab's chord, opens Details at its steps as `/steps` does (answer A).
    await resolves("t e", async () => {
      await gotoIssue(page, `/issues/${seed.key}/conversation`);
      await expectOpeningLanded(page);
      await focusThePage(page);
      await page.keyboard.press("t");
      await page.keyboard.press("e");
      await expect(page, "t e does not open the steps' address").toHaveURL(
        `/issues/${seed.key}/steps`
      );
      await expectAddressOpens(page, `/issues/${seed.key}/steps`, "t e", {
        kind: "details",
        part: "steps",
      });
    });
    expect(unresolved, `these addresses do not resolve:\n${unresolved.join("\n")}`).toEqual([]);
  });
});

/** The slug of `key`'s primary document, which the server named when it made it. */
async function primarySlug(key: string): Promise<string> {
  const primary = ((await getIssue(key)).artifacts ?? []).find((artifact) => artifact.primary);
  expect(primary, `${key} carries no primary document`).toBeDefined();
  return primary?.slug ?? "";
}

/** The load the budget is read against actually happened, and the page drew what its reads
 *  carry. A ceiling is only a budget while the page loaded: a page whose reads all failed sends
 *  few requests and would pass the ceiling for the opposite of the reason this check exists,
 *  which is a page that reads too much. `expectIssuePageDrawn` does not answer that question —
 *  the tree element is drawn before its rows are — so every requirement the seed wrote has a
 *  row of its own here, and the settle below is read after them. */
async function expectTheSeedIsDrawn(page: Page, seeded: BaselineSeed): Promise<void> {
  const slug = await primarySlug(seeded.key);
  // No row for the issue itself: the outline draws none (ruling 5, part N's c-addresses (4)).
  for (const requirement of seeded.requirements) {
    await expect(
      nodeRow(page, blockNodeId(slug, requirement)).first(),
      `the tree draws no row for ${requirement}, so the page did not load what the count is of`
    ).toBeVisible();
  }
  await expect
    .poll(async () => (await drawnRows(page)).length, {
      message: "the tree drew fewer rows than the seed's requirements alone",
    })
    .toBeGreaterThanOrEqual(seeded.requirements.length);
}

/** A document of an issue, by its id and its name. */
interface SeededDocument {
  readonly id: string;
  readonly name: string;
}

/** How many typed blocks the row's document that no view reads holds (part N's R8). */
const LONG_DOCUMENT_BLOCKS = 500;

/** Writes `key` a document of `LONG_DOCUMENT_BLOCKS` requirements that is none of the spec, the
 *  Verification, the plan and the decisions documents, whose blocks no load may read until the
 *  reader opens it from Documents (part N's R8). A typed block is what the tree draws a node of,
 *  so a page that built its tree out of every document's blocks would read these on every first
 *  load, as the checkpoint did: 25 of its 47 reads on the recipe's copy of INFRA-1196 were one
 *  document's blocks each (the tester's finding F17). */
async function seedLongDocument(key: string): Promise<SeededDocument> {
  const blocks = Array.from(
    { length: LONG_DOCUMENT_BLOCKS },
    (_, index) =>
      `:::requirement{#r-inventory-${index + 1}}\nEntry ${index + 1} of the inventory.\n:::`
  );
  const { artifact } = await createIssueArtifact(key, {
    content: `# Inventory\n\n${blocks.join("\n\n")}\n`,
    name: "inventory.md",
  });
  const issue = await getIssue(key);
  expect(
    [
      issue.primary_artifact_id,
      issue.plan_artifact_id,
      issue.verification_artifact_id,
      issue.decisions_artifact_id,
    ],
    `${artifact.name} is a document a view reads, so its blocks prove nothing of R8`
  ).not.toContain(artifact.id);
  expect(
    (await getArtifactBlocks(artifact.id)).filter((block) => block.type === "requirement"),
    `${artifact.name} does not hold ${LONG_DOCUMENT_BLOCKS} typed blocks`
  ).toHaveLength(LONG_DOCUMENT_BLOCKS);
  return { id: artifact.id, name: artifact.name };
}

/** `seed`'s issue, with the document of 500 typed blocks beside its own (`seedLongDocument`). */
function withLongDocument(
  seed: () => Promise<BaselineSeed>
): () => Promise<BaselineSeed & { readonly long: SeededDocument }> {
  return async () => {
    const seeded = await seed();
    return { ...seeded, long: await seedLongDocument(seeded.key) };
  };
}

/** Each read of `document`'s blocks (`GET /api/v1/artifacts/{id}/blocks`) that `traffic` holds. */
function blocksReadsOf(traffic: ApiRequests, document: SeededDocument): string[] {
  const blocks = `/api/v1/artifacts/${document.id}/blocks`;
  return traffic.requests
    .filter((request) => request.path.split("?")[0] === blocks)
    .map((request) => `${request.method} ${request.path}`);
}

// c-requests.
test("the first load stays inside its request budget", async ({ browser }) => {
  let baseline: { byRoute: string; total: number } | null = null;

  await phase(browser, withLongDocument(seedRequestBaseline), async (page, seeded) => {
    const traffic = watchApiRequests(page);
    traffic.reset();
    // Conversation's own address, whose load reads the tree: the bare address draws Dashboard
    // (dispatch://INFRA-5702), whose own cold load is read below.
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await expectTheSeedIsDrawn(page, seeded);
    await expectLoadSettled(traffic);
    const byRoute = JSON.stringify(traffic.byRoute());

    // No reference read for each pointer: the Spec tab sent 3 of them today and the Plan tab 14.
    const perPointer = traffic.requests.filter(
      (request) => request.route === "/api/v1/references"
    ).length;
    expect(
      perPointer,
      `the page read /api/v1/references ${perPointer} times, one for each pointer: ${byRoute}`
    ).toBe(0);

    // No read of any document's text before a document is opened (R21). `GET
    // /artifacts/{id}/text` is that read and the only one: `GET /artifacts/{id}/blocks` answers
    // `ArtifactBlock`s, which carry a block's id, type, byte offsets, token, reference counts,
    // attributes and a table's cells and no prose at all (`packages/contracts/src/
    // dispatch-api.ts`, `ArtifactBlock`), so it is not a text read and naming it here would ban
    // the one listing the tree is built from. The head draws its sentences out of the summary
    // node's own text, which the tree already carries (`IssueThreeThings.tsx`), and the version
    // control beside the filters reads the versions the issue's own read carries
    // (`VersionControl.tsx`), so nothing on this address reads the spec's text either.
    const textReads = () =>
      traffic.requests
        .filter((request) => /\/text(\?|$)/.test(request.path))
        .map((request) => request.path);
    expect(textReads(), "the first load read the stored text of a document").toEqual([]);

    // The tree is built from the blocks of the spec, the Verification, the plan and the
    // decisions documents alone (`treeDocumentIds`), so the 500 blocks of a document none of
    // them is are read once the reader opens it, and on no load before.
    expect(
      blocksReadsOf(traffic, seeded.long),
      `the first load of /conversation read the blocks of ${seeded.long.name}, a document of ${LONG_DOCUMENT_BLOCKS} typed blocks, before it was opened from Documents`
    ).toEqual([]);

    // The ceiling and a floor together, because a ceiling alone passes a page that read
    // nothing: the baseline's own load is 37 reads, so a count in single figures is a page
    // that failed rather than a page that improved, and it has to be looked at either way.
    expect(
      traffic.total(),
      `the first load sent ${traffic.total()} requests under /api/: ${byRoute}`
    ).toBeLessThanOrEqual(40);
    expect(
      traffic.total(),
      `the first load sent only ${traffic.total()} requests under /api/, too few to be a load of this issue: ${byRoute}`
    ).toBeGreaterThanOrEqual(20);
    baseline = { byRoute, total: traffic.total() };

    // The Spec view's own cold load, within the same ceiling.
    traffic.reset();
    await gotoIssue(page, presetPath(seeded.key, "spec"));
    await expectIssuePageDrawn(page);
    await expectTheSeedIsDrawn(page, seeded);
    await expectLoadSettled(traffic);
    const specByRoute = JSON.stringify(traffic.byRoute());
    expect(
      blocksReadsOf(traffic, seeded.long),
      `the first load of /spec read the blocks of ${seeded.long.name}, a document of ${LONG_DOCUMENT_BLOCKS} typed blocks, before it was opened from Documents`
    ).toEqual([]);
    expect(
      traffic.total(),
      `the first load of /spec sent ${traffic.total()} requests under /api/: ${specByRoute}`
    ).toBeLessThanOrEqual(40);

    // The bare address's own cold load, which draws Dashboard (dispatch://INFRA-5702), within the
    // same ceiling, and no read of the long document's blocks there either.
    traffic.reset();
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectDashboardDrawn(page, "the bare address");
    await expectLoadSettled(traffic);
    const bareByRoute = JSON.stringify(traffic.byRoute());
    expect(
      blocksReadsOf(traffic, seeded.long),
      `the first load of the bare address read the blocks of ${seeded.long.name}, a document of ${LONG_DOCUMENT_BLOCKS} typed blocks, before it was opened from Documents`
    ).toEqual([]);
    expect(
      traffic.total(),
      `the first load of the bare address sent ${traffic.total()} requests under /api/: ${bareByRoute}`
    ).toBeLessThanOrEqual(40);

    // Opening the document from Documents reads its text and its blocks.
    await pickPreset(page, PRESET_NAMES.documents);
    await documentsList(page).getByRole("link", { exact: true, name: seeded.long.name }).click();
    await expect(openedEditor(page)).toBeVisible();
    await expect
      .poll(textReads, { message: `opening ${seeded.long.name} read no text of it` })
      .toContainEqual(expect.stringContaining(seeded.long.id));
    await expect
      .poll(() => blocksReadsOf(traffic, seeded.long).length, {
        message: `opening ${seeded.long.name} from Documents read none of its blocks`,
      })
      .toBeGreaterThan(0);
  });

  // No request for each node: twice the nodes, the same count. The issue carries the same
  // document of 500 typed blocks, so the nodes are all that differs.
  await phase(browser, withLongDocument(seedTwiceTheNodes), async (page, seeded) => {
    const traffic = watchApiRequests(page);
    traffic.reset();
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await expectTheSeedIsDrawn(page, seeded);
    await expectLoadSettled(traffic);
    const byRoute = JSON.stringify(traffic.byRoute());
    expect(
      traffic.total(),
      `twice the nodes cost ${traffic.total()} requests where the baseline cost ${baseline?.total}: ${byRoute} against ${baseline?.byRoute}`
    ).toBe(baseline?.total);
  });
});

// The reviewer's finding on the not-found page (deep-links.e2e.ts:1193 on main reads "Ask not
// found"): the page's word for an ask is "question" (R34), and the article follows the noun.
test("an address naming a question the issue does not hold says so in the page's own words", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page, seeded) => {
    await page.goto(`/issues/${seeded.key}/asks/00000000-0000-4000-8000-000000000000`);
    await expect(
      page.getByRole("heading", { exact: true, name: "Question not found" }),
      "the page does not say the question was not found"
    ).toBeVisible();
    const said = (await page.locator("main").innerText()).replace(/\s+/g, " ");
    expect(said, "the page says 'an question'").not.toMatch(/\ban question\b/iu);
    expect(said, "the page calls the question an ask").not.toMatch(/\bask\b/iu);
  });
});

// Decision 413 (R6, c-addresses (6)): an address moves the focus once, onto where it lands. Each
// of the three rows keeps a log of every element the focus moves to from the first moment the
// document exists (`logFocusMoves`, an init script) and fails on any move before the last,
// naming the element that took the focus first. A view's tab stop and the outline's opening on
// its first row take the focus before a read the address waits for has answered, which is what
// the rows catch.

/** The row of node `id`, which is how a view and the outline both mark one. */
function rowSelector(id: string): string {
  return `[data-node-row][data-node-id="${id}"]`;
}

/** Opens `path` and fails unless the focus moves once, onto `lands`. */
async function expectAddressLandsOnce(
  page: Page,
  traffic: ApiRequests,
  path: string,
  lands: Locator,
  what: string
): Promise<void> {
  await gotoIssue(page, path);
  await expectFocusLandedOnce(page, traffic, lands, what);
}

test("an address lands the focus once on a node the view draws", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    await logFocusMoves(page);
    const traffic = watchApiRequests(page);
    // The second statement and the second check: the views' tab stops start on the first.
    const requirement = requirementBlockId(2);
    const check = checkBlockId(2);
    await expectAddressLandsOnce(
      page,
      traffic,
      `${presetPath(seeded.key, "spec")}#b-${requirement}`,
      page.locator(`[data-spec-body] ${rowSelector(blockNodeId(specSlug, requirement))}`),
      `the address of ${requirement} under ${PRESET_NAMES.spec}`
    );
    await expectAddressLandsOnce(
      page,
      traffic,
      `${presetPath(seeded.key, "verification")}#b-${check}`,
      page.locator(`[data-verification-body] ${rowSelector(blockNodeId(verificationSlug, check))}`),
      `the address of ${check} under ${PRESET_NAMES.verification}`
    );
  });
});

test("an address lands the focus once on a node beside the view", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    await logFocusMoves(page);
    const traffic = watchApiRequests(page);
    // A plan task is no row of the Verification view, and a comment none of the Plan view: each
    // tells the stage so, which draws the outline in its place. The Spec view draws every node
    // an address names, so none stands beside it.
    const task = blockNodeId(planSlug, planTaskBlockId(1));
    await expectAddressLandsOnce(
      page,
      traffic,
      `${presetPath(seeded.key, "verification")}#b-${planTaskBlockId(1)}`,
      page.locator(`[data-issue-body] ${rowSelector(task)}`),
      `the address of ${planTaskBlockId(1)} under ${PRESET_NAMES.verification}`
    );
    const [thread = ""] = seeded.comments.threads;
    await expectAddressLandsOnce(
      page,
      traffic,
      `${presetPath(seeded.key, "plan")}?comment=${thread}`,
      page.locator(`[data-issue-body] ${rowSelector(thread)}`),
      `the address of a comment under ${PRESET_NAMES.plan}`
    );
  });
});

test("an address lands the focus once on a gone node", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await logFocusMoves(page);
    const traffic = watchApiRequests(page);
    // The page's line that the comment is not found takes the focus, once its own read answers.
    for (const preset of ["spec", "verification"] as const) {
      await expectAddressLandsOnce(
        page,
        traffic,
        `${presetPath(seeded.key, preset)}?comment=${GONE_COMMENT}`,
        issuePage(page).getByRole("heading", {
          exact: true,
          name: FALLBACK_LINES.comment(seeded.key).heading,
        }),
        `the address of a gone comment under ${PRESET_NAMES[preset]}`
      );
    }
  });
});

// ---- from 29111db16c41:packages/dispatch/e2e/artifacts.e2e.ts

const tinyPng =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

// The files a row uploads are written in that row's own output directory, which Playwright gives
// each row of each worker (`testInfo.outputPath`): two workers running these rows at once would
// otherwise write one path in the checkout, and a run must leave no file changed under e2e/.
async function diagramFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("diagram.png");
  await writeFile(path, Buffer.from(tinyPng, "base64"));
  return path;
}

async function notesFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("notes.md");
  await writeFile(path, "# Review notes\n\nThese notes replace the initial spec.\n");
  return path;
}

/** Opens the issue's Documents' list at its own address by a move inside the page, closing
 *  Details first where it covers a phone's page (dispatch://INFRA-1196 R14). The list took the
 *  Artifacts tab's list (part N's R2): each row carries what the tab's row offered beside the
 *  file - the link that opens it, its copy control, its latest version's download and, for a
 *  document the issue can drop, Remove - and the file's versions, its picture and what references
 *  it are on its own page, which the row's link opens. */
async function openDocumentsList(page: Page, isPhone: boolean) {
  if (isPhone) await closeDetails(page);
  await openIssueAddress(page, "documents");
  await expect(documentsList(page), "Documents' list did not open").toBeVisible();
}

// Choosing a file only stages it; the row then shows a Summary field plus a confirming
// "Upload" button (or "Cancel" to discard the pick) before it actually creates the version.
async function confirmUpload(page: Page): Promise<void> {
  await page.getByRole("button", { exact: true, name: "Upload" }).click();
}

// `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
// cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
function postAsPage(page: Page, path: string, data?: object) {
  return page.request.post(path, { data, headers: { Origin: dashboardOrigin } });
}

// SLOW(>=2s): three real uploads with the versions they build, the artifact page's version list
// and comparison, and the references and phone layout read after them.
// Off the pull-request lane: `main`'s source row in `artifacts.e2e.ts` runs there, and the lane's
// real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
test("artifacts upload, version, references, and phone layout @phone", async ({
  page,
}, testInfo) => {
  const sockets = countDocumentSockets(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Initial spec\n\nThe original document.",
    title: "Artifact review",
  });
  await createAsk(issue.key, { question: "Can this ship?" });

  await page.goto(await specAddress(issue.key));
  await expect(documentEditor(page)).toContainText("The original document.");
  await expect.poll(sockets).toBe(1);
  await openDocumentsList(page, testInfo.project.name === "iphone");

  // Documents' list's upload stages the picked file, with its Summary, Upload and Cancel.
  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagramUpload = await diagramFile(testInfo);
  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  const diagram = documentRow(page, "diagram.png");
  await expect(diagram).toBeVisible();
  await expect(diagram.getByRole("link", { name: "Download version 1" })).toBeVisible();

  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  await expect(diagram.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  // The list's row is compact: the versions, their count, the picture, the compare and the
  // references live on the artifact's own page, not under every row.
  await expect(diagram.getByRole("button", { name: "Show all versions" })).toHaveCount(0);
  await expect(page.getByLabel(CONTROL_LABELS.filter_files)).toBeVisible();

  await navigateInApp(page, `/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page.getByTestId("artifact-header")).toContainText("diagram.png");
  await expect(page.getByRole("img", { name: "diagram.png version 2" })).toBeVisible();
  expect(sockets()).toBe(1);
  // Every version is drawn open under the file, with no control that hides any (part N's R2), so
  // the list is read with nothing pressed.
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  await expect(versions).toContainText("Version 1");
  await expect(versions).toContainText("Version 2");
  await expect(versions).not.toContainText("Version 3");
  await expect(versions).toContainText("SHA-256");
  await expect(versions.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  const compare = page.getByRole("region", { name: "Compare versions for diagram.png" });
  await expect(compare).toBeVisible();
  await expect(compare).not.toContainText("From · Version");
  await compare.getByRole("button", { name: "Show comparison" }).click();
  await expect(compare).toContainText("From · Version");
  await expect(compare).toContainText("To · Version");

  await navigateInApp(page, `/issues/${issue.key}`);
  await openDocumentsList(page, testInfo.project.name === "iphone");

  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  const notes = documentRow(page, "notes.md");
  await expect(notes).toBeVisible();
  await expect(notes.getByRole("button", { name: /make primary/i })).toHaveCount(0);
  await navigateInApp(page, await specAddress(issue.key));
  await expect(documentEditor(page)).toContainText("The original document.");

  const notesCitation = await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`,
  });

  const diagramCitation = await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/diagram-png before deciding.`,
  });
  // No reload: both citations reach the open page on its event feed, and the panel below reads
  // what the feed left.
  await openDocumentsList(page, testInfo.project.name === "iphone");

  await documentLink(page, "diagram.png").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  // The panel names the record that cites this artifact and quotes it; its link goes to that
  // source, not back to the artifact the reader is already on. What references the opened
  // artifact is drawn open, in the page's own words (part N's R2, R12): a comment of the issue's
  // own is named by its words, beside the page's word for a comment, with no issue named, and the
  // words name the document they cite by the name the page's one batched read gave it, its file
  // name, as a plain line reads a link to it (main's one reference framework, #3125); the page's
  // word for a document stands only until that read answers.
  const diagramWords = "See diagram.png before deciding.";
  const diagramRow = openedArtifact(page)
    .getByRole("region", { name: PROPERTY_LABELS.referenced_by })
    .getByRole("listitem")
    .filter({ has: page.getByRole("link", { exact: true, name: diagramWords }) });
  const diagramSource = diagramRow.getByRole("link", { exact: true, name: diagramWords });
  await expect(diagramSource).toHaveAttribute(
    "href",
    `/issues/${issue.key}/comments/${diagramCitation.id}`
  );
  await expect(diagramRow).toContainText(NODE_REFERENCES_WORDS.kinds.comment);
  await diagramSource.click();
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/comments/`));

  // One cold load, so the notes.md panel below is read by a page that cached nothing.
  await page.goto(`/issues/${issue.key}`);
  await openDocumentsList(page, testInfo.project.name === "iphone");
  await documentLink(page, "notes.md").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  const notesWords = "See notes.md before deciding.";
  const notesRow = openedArtifact(page)
    .getByRole("region", { name: PROPERTY_LABELS.referenced_by })
    .getByRole("listitem")
    .filter({ has: page.getByRole("link", { exact: true, name: notesWords }) });
  await expect(notesRow.getByRole("link", { exact: true, name: notesWords })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/comments/${notesCitation.id}`
  );
  await expect(notesRow).toContainText(NODE_REFERENCES_WORDS.kinds.comment);
  await navigateInApp(page, `/issues/${issue.key}`);
  await openDocumentsList(page, testInfo.project.name === "iphone");
  await page.screenshot({ path: testInfo.outputPath("artifacts-tab.png"), fullPage: true });

  if (testInfo.project.name === "iphone") {
    await page.goto("/");
    await expect(page.getByTestId(/ask-/).first()).toContainText("Can this ship?");
    await page.screenshot({ path: testInfo.outputPath("iphone-inbox.png"), fullPage: true });

    const openNavigation = page.getByRole("button", { name: "Open navigation" });
    await expect(openNavigation).toBeVisible();
    await openNavigation.click();
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
    await page.getByRole("button", { name: "Close navigation" }).click();
    await navigateInApp(page, `/issues/${issue.key}`);
    // On the issue page the panel is Details, which the header's control opens over the page,
    // the phone's sheet the whole width (dispatch://INFRA-1196 R14). The header's controls -
    // Open navigation and Details among them - take WCAG 2.2's 24 px target, not the 44 px floor
    // of today's page: the header at 390 px is held to eight lines and 250 px (ruling 8,
    // decision 47, the header's rule in `styles.css`).
    const openMargin = page
      .locator("header[data-issue-header]")
      .getByRole("button", { exact: true, name: "Details" });
    const [navigationBox, marginBox] = await Promise.all([
      openNavigation.boundingBox(),
      openMargin.boundingBox(),
    ]);
    expect(navigationBox?.width).toBeGreaterThanOrEqual(24);
    expect(navigationBox?.height).toBeGreaterThanOrEqual(24);
    expect(marginBox?.width).toBeGreaterThanOrEqual(24);
    expect(marginBox?.height).toBeGreaterThanOrEqual(24);
    await openMargin.click();
    await expect(page.getByRole("dialog", { exact: true, name: "Details" })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("iphone-details.png"), fullPage: true });
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
  }
});

// The From/To compare of a document shows what changed as the change view, word by word. Every
// artifact's own page draws `ArtifactDetails` - the version history and this compare - the spec's
// as much as any other (`ArtifactRoutePanel`); the row compares a second document so the spec's
// own text stays out of the comparison.
test("the version history compares two versions of a document with the change view", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Lease handover\n\nThe spec itself is the primary document.\n",
    title: "Version compare",
  });
  const notes = await createIssueArtifact(issue.key, {
    content: "# Review notes\n\nThe ledger keeps three leases.\n",
    name: "notes.md",
  });
  await editArtifact(
    notes.artifact.id,
    {
      ops: [
        {
          find: "The ledger keeps three leases.",
          op: "replace",
          with: "The ledger keeps seven leases.",
        },
      ],
      summary: "Seven leases",
    },
    { as: "agent" }
  );

  await page.goto(`/issues/${issue.key}/artifacts/notes-md`);
  const compare = page.getByRole("region", { name: "Compare versions for notes.md" });
  await compare.getByRole("button", { name: "Show comparison" }).click();
  await compare.getByLabel("Compare notes.md from").selectOption("1");
  await compare.getByLabel("Compare notes.md to").selectOption("2");
  const changed = compare.getByTestId("change-view").locator('[data-change="changed"]');
  await expect(changed).toHaveCount(1);
  await expect(changed.locator("del")).toHaveText(["three"]);
  await expect(changed.locator("ins")).toHaveText(["seven"]);
});

test("the node menu's filter narrows its files by name and kind", async ({ page }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Artifact filtering" });
  await page.goto(`/issues/${issue.key}`);
  await openDocumentsList(page, false);

  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagram = documentRow(page, "diagram.png");
  const notes = documentRow(page, "notes.md");
  await upload.setInputFiles(await diagramFile(testInfo));
  await confirmUpload(page);
  await expect(diagram).toBeVisible();
  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  await expect(notes).toBeVisible();

  const filter = page.getByLabel(CONTROL_LABELS.filter_files);
  await filter.fill("notes");
  await expect(notes).toBeVisible();
  await expect(diagram).toHaveCount(0);

  await filter.fill("image");
  await expect(diagram).toBeVisible();
  await expect(notes).toHaveCount(0);

  await filter.fill("");
  await expect(diagram).toBeVisible();
  await expect(notes).toBeVisible();
});

// Documents' list's upload stages a picked file as the Artifacts list's did (dispatch://INFRA-1196
// R19, "nothing lost"; part N's R2): the file's name, an optional Summary for the version it makes,
// and Upload and Cancel. Cancel sends nothing; Upload sends the file with its summary.
test("the node menu's upload stages a file with a Summary for its version, and Cancel sends nothing", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Staged upload" });
  const held = (await listIssueArtifacts(issue.key)).length;
  await page.goto(`/issues/${issue.key}/documents`);

  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const summary = page.getByRole("textbox", { name: "Summary (optional)" });
  await upload.setInputFiles(await notesFile(testInfo));
  await expect(page.getByText("notes.md", { exact: true })).toBeVisible();
  await expect(summary).toBeVisible();
  await page.getByRole("button", { exact: true, name: "Cancel" }).click();
  await expect(summary).toHaveCount(0);
  expect(await listIssueArtifacts(issue.key)).toHaveLength(held);

  await upload.setInputFiles(await notesFile(testInfo));
  await summary.fill("First review notes");
  await confirmUpload(page);
  await expect(documentRow(page, "notes.md")).toBeVisible();
  const notes = (await listIssueArtifacts(issue.key)).find((one) => one.name === "notes.md");
  expect(notes?.versions?.map((version) => version.summary)).toEqual(["First review notes"]);
});

// SLOW(>=2s): a real upload the history links, and the keyboard tour of the five presets with
// each tab's box measured, at 1440 px and at 390 px.
//
// The nine tabs are gone (dispatch://INFRA-1196 R3): the row is the views' controls, Documents
// among them (part N's R4), and how many artifacts an issue carries is Documents' list's, not a
// tab's badge, so the `Artifacts (N)` assertions this test carried are retired with it - what
// they proved, that the live feed refreshes the page's own counts without a reload, is proved
// here on the history, in Details.
//
// It is not tagged `@phone`: the row sets 390 px itself, so the iphone project would run the
// same assertions a second time (#2866, no browser row runs twice).
test("an uploaded document is linked from the history and the presets fit every width", async ({
  page,
}, testInfo) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({ project: "OPS", title: "Customer update" });
  await page.goto(`/issues/${issue.key}/activity`);

  // The history is drawn before the upload, so the line below is one the live feed brought and
  // not one the first read already held: without this the upload can land before that read
  // answers, and the row would pass with the feed disconnected. This is what the retired
  // `Artifacts 1` badge reading proved - that the page's own counts move without a reload.
  const rows = historyEntries(page);
  await expect(rows.filter({ hasText: "created the issue" })).toHaveCount(1);

  const name = "cu-update-2026-09-15.md";
  const added = rows.filter({ hasText: `added ${name}` });
  await expect(added).toHaveCount(0);
  const upload = await createIssueArtifact(issue.key, {
    content: "# Draft\n\nHi team, here is this week's update.\n",
    name,
  });

  // The live event feed puts the upload on the open history without a reload.
  await expect(added).toHaveCount(1);
  await page.screenshot({ path: testInfo.outputPath("activity-added-artifact.png") });

  // A second upload under the same name is a new version of the document. A document's versions
  // are its own toolbar's, and the history draws no line for one (dispatch://INFRA-1196 R15,
  // c-conversation-state), where the Activity list drew `saved <name> v2`: read afresh once
  // Dispatch holds the version, the history still names the document on its one line. It
  // replaces the whole document, so it names the token of the version it replaces.
  await createIssueArtifact(issue.key, {
    content: "# Draft\n\nRevised.\n",
    name,
    precondition: await documentBase(upload.artifact.id),
  });
  await expect
    .poll(
      async () =>
        (await listIssueArtifacts(issue.key)).find((one) => one.name === name)?.versions?.length
    )
    .toBe(2);
  await page.reload();
  await expect(rows.filter({ hasText: "created the issue" })).toHaveCount(1);
  await expect(rows.filter({ hasText: name })).toHaveCount(1);
  await expect(added).toHaveCount(1);

  const viewport = page.viewportSize();
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 1440 });
  const toolbar = presetToolbar(page);
  const toggles = presetToggles(page);
  await expect(toggles).toHaveCount(PRESET_ORDER.length);
  const insideToolbar = async (toggle: Locator) => {
    const [rowBox, box] = await Promise.all([toolbar.boundingBox(), toggle.boundingBox()]);
    expect(rowBox).not.toBeNull();
    expect(box).not.toBeNull();
    expect(box?.x).toBeGreaterThanOrEqual(rowBox?.x ?? 0);
    expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(
      (rowBox?.x ?? 0) + (rowBox?.width ?? 0) + 0.5
    );
  };
  // What the row promises at 1440 px, which is a pointer's width: each toggle is the
  // prototype's tab, 35 px tall (`web/src/features/issue/IssuePresets.tsx`), and no taller than
  // the row it shares. The touch target is read below at the width a finger is what reaches the
  // filters: these five are not drawn there at all, because below 1280 px the row is one
  // control instead, and that control carries the target.
  const fitsTheRow = async (toggle: Locator, label: string) => {
    const height = (await toggle.boundingBox())?.height ?? 0;
    expect(
      height,
      `the ${label} toggle is ${height} px tall, under the prototype's tab`
    ).toBeGreaterThanOrEqual(32);
    expect(
      height,
      `the ${label} toggle is ${height} px tall, taller than the row of five`
    ).toBeLessThanOrEqual(48);
  };
  // Every control is reachable from the keyboard, each is inside the row as it takes focus, and
  // each is drawn at the height the row gives it.
  const [head, ...rest] = PRESET_ORDER;
  const first = presetToggle(page, head);
  await first.focus();
  for (const preset of rest) {
    await page.keyboard.press("ArrowRight");
    const toggle = presetToggle(page, preset);
    await expect(toggle).toBeFocused();
    await insideToolbar(toggle);
    await fitsTheRow(toggle, preset);
  }
  await page.keyboard.press("Home");
  await expect(first).toBeFocused();
  await insideToolbar(first);
  await fitsTheRow(first, PRESET_NAMES[head]);

  // At 390 px the row is one control naming the one on, which opens the rest. This is the
  // width a finger is what reaches the filters, so this is where the touch target is read: the
  // one control a reader presses there carries it, and the row fails if it ever loses it. The
  // target is WCAG 2.2's 24 px (2.5.8), not the 44 px floor of today's tabs: the view row is one
  // line at most 51 px tall below 1280 px, 8 px above and below its controls, and each of them
  // is at least 24 px tall (part N's R4, ruling 5, the filter row's rule in `styles.css`).
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 390 });
  // The history is in Details, which its address opens (dispatch://INFRA-1196 R15) and which on
  // a phone is a sheet over the whole page, so a reader closes it to reach the filters.
  await closeDetails(page);
  await expect(toggles).toHaveCount(0);
  const chooser = presetChooser(page);
  await expect(chooser).toBeVisible();
  const chooserHeight = (await chooser.boundingBox())?.height ?? 0;
  expect(
    chooserHeight,
    `the preset control is ${chooserHeight} px tall at 390 px, under the 24 px a touch needs`
  ).toBeGreaterThanOrEqual(24);
  await chooser.click();
  // Each view's control is a radio item of the menu, the one on checked (part D's
  // `IssuePresets.tsx`), Documents among them (part N's R4).
  await expect(page.getByRole("menu").getByRole("menuitemradio")).toHaveCount(PRESET_ORDER.length);
  await page.keyboard.press("Escape");
  await page.setViewportSize({ height: viewport?.height ?? 800, width: viewport?.width ?? 1280 });

  // The upload is a line of the history and nothing else: a document is never a row of the
  // outline (R3), which draws no row naming it. The issue holds nothing else, so the outline is
  // drawn empty, with the page's line saying so once its reads have answered.
  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(tree(page)).toHaveCount(1);
  await expect(
    issuePage(page).getByText(EMPTY_STATE_LINES.conversation, { exact: true })
  ).toBeVisible();
  await expect(tree(page).getByText(name)).toHaveCount(0);
  await page.goto(`/issues/${issue.key}/activity`);

  await added.getByRole("link", { name }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByTestId("artifact-header")).toContainText(name);
});

// The Artifacts tab's badge and its rows' detail lines went with the tab (dispatch://INFRA-1196
// R3): Documents' list holds one row per file, whose name is never cut, and a file's versions are
// drawn open on its own page (part N's R2).
test("the node menu lists every file of the issue, and an item's name is not truncated @phone", async ({
  page,
}) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({
    project: "OPS",
    spec: "# Spec\n\nThe issue's own document.\n",
    title: "Counted tab",
  });
  await createIssueArtifact(issue.key, { content: "# Audit\n\nOne more.\n", name: "audit.md" });

  await page.goto(`/issues/${issue.key}/documents`);
  const files = documentsRows(page).filter({
    has: page.getByRole("link", { name: /^Download version/ }),
  });
  await expect(files).toHaveCount(2);

  // Every row shows its whole name: it wraps where it must rather than truncating. The count of
  // two above waited for both rows, so one read lists them.
  for (const file of await files.all()) {
    const name = file.getByRole("link").first();
    expect(await name.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
  }
  await documentLink(page, "audit.md").click();
  // Every version is drawn open, with no control that hides any (part N's R2).
  await expect(
    page.getByRole("region", { exact: true, name: "Versions for audit.md" })
  ).toContainText("Version 1");
});

// The reader leaves the document and opens it again without a page load, so the page still holds
// the text it read on the first visit. The second admission reads the text afresh and shows the
// repair; connecting on the cached text would open a socket into a tree outside the schema. The
// count is this document's sockets alone: the issue's own spec holds a socket of its own.
test("an out-of-schema document does not reconnect from cached text", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Do not reconnect from stale text" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "cached-repair.md",
  });
  const sockets = countDocumentSockets(page, upload.artifact.id);
  const row = documentRow(page, "cached-repair.md");
  const openDocument = () => documentLink(page, "cached-repair.md").click();

  await page.goto(`/issues/${issue.key}`);
  await openDocumentsList(page, false);
  // A page load would drop this marker, and the page's cached reads with it.
  await page.evaluate(() => Object.assign(window, { __e2eNavMarker: true }));
  await openDocument();
  await expect(documentEditor(page)).toContainText("before");
  await expect.poll(sockets).toBe(1);

  await page.goBack();
  await expect(row).toBeVisible();
  const corrupted = await postAsPage(
    page,
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);
  await openDocument();

  await expect(page.getByText("A person decides whether to replace the document")).toBeVisible();
  await expect(documentEditor(page)).toBeHidden();
  expect(await page.evaluate(() => "__e2eNavMarker" in window)).toBe(true);
  expect(sockets()).toBe(1);
});

// ---- from 29111db16c41:packages/dispatch/e2e/keyboard-issue.e2e.ts

test("the issue scope reaches the header controls and the tab chords, and ? lists them under Issue", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard issue" });
  // `t h` is the outline's own chord here, offered where the tree holds a child and no key at
  // all where it holds none (part I's `useTreeKeymap.ts`, its row "the children are one group of
  // rows of the outline"), where the source bound it in the issue's scope from anywhere: so the
  // issue the chord is read on carries a child.
  await createIssue({ parent: issue.key, project: "CORE", title: "A child to move to" });
  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    // Conversation's own address, where the outline and its opening are: the issue's own draws
    // Dashboard (dispatch://INFRA-5702), which draws no outline. The keymap binds only once
    // sign-in resolves, so the header's title is waited for before a key is pressed.
    await page.goto(presetPath(issue.key, "conversation"));
    await expect(
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Keyboard issue" })
    ).toBeVisible();
    await page.locator("body").focus();
    // The outline opens with the focus on a row of its own and takes it back from whatever holds
    // the page while nothing does, until the reader acts (`IssueTree.tsx`'s opening): so the
    // opening is waited out and the reader acts first - a Shift, which no key of the page binds -
    // and every press below starts from the page itself.
    await expectOpeningLanded(page);
    await page.keyboard.press("Shift");
    await focusThePage(page);
    const header = page.getByTestId("issue-header");
    const priority = page.getByRole("combobox", { name: `Priority of ${issue.key}` });
    const status = page.getByRole("combobox", { name: "Status" });

    // p and s reach their own control: two assertions, so a p that focused Status fails.
    await page.keyboard.press("p");
    await expect(priority).toBeFocused();
    await focusThePage(page);
    await page.keyboard.press("s");
    await expect(status).toBeFocused();
    await focusThePage(page);

    // A digit sets P0–P3 through the same write the picker uses, and Shift+P pins through the
    // header's own control. Both are optimistic, so each write is waited for rather than raced,
    // and one reload below carries both: a second one would prove nothing the first does not.
    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await page.keyboard.press("2");
    expect((await patch).postDataJSON()).toEqual({ priority: 2 });
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    await focusThePage(page);

    const pinned = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
    );
    await page.keyboard.press("Shift+P");
    // The pin is the overflow menu's action, pressed while the menu is shut (R13).
    await expect(header.locator('[data-issue-action="pin"]')).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect((await pinned).ok()).toBe(true);
    await focusThePage(page);

    await page.reload();
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect(header.locator('[data-issue-action="pin"]')).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    // The new load opens the outline on a row of its own again (decision 399), and the reader
    // acts before the focus leaves for the page, as above.
    await expectOpeningLanded(page);
    await page.keyboard.press("Shift");
    await focusThePage(page);

    // l opens the labels popover in Details' Labels row - the listbox itself, not just its
    // trigger (R13).
    await page.keyboard.press("l");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Search or create label" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toHaveCount(0);
    await focusThePage(page);

    // e edits the title, where every single key is text again: typing "sp0" neither focused
    // Status or the priority nor set P0, and Escape discards the edit.
    await page.keyboard.press("e");
    const titleInput = page.getByLabel("Issue title");
    await expect(titleInput).toBeFocused();
    await page.keyboard.type("sp0");
    await expect(titleInput).toBeFocused();
    await expect(titleInput).toHaveValue(/sp0/);
    await page.keyboard.press("Escape");
    await expect(
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Keyboard issue" })
    ).toBeVisible();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        priority: 2,
        title: "Keyboard issue",
      });
    await focusThePage(page);

    // t is a chord: the indicator shows it waiting, then the second key picks its target. c is a
    // global shortcut on its own (the create dialog), and completing the chord is not it: `t c`
    // is the conversation, the page with every filter off (the key table). The chord's second
    // key must follow within the keymap's 1000 ms window, so nothing slow sits between the two.
    await page.keyboard.press("t");
    const indicator = page.getByTestId("chord-indicator");
    await expect(indicator).toHaveText(/t/);
    await page.keyboard.press("c");
    await expect(indicator).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Create issue" })).toHaveCount(0);
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await expectActivePreset(page, PRESET_NAMES.conversation);
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-conversation-${testInfo.project.name}.png`),
    });

    // Every chord of the key table opens its target's address (R11, R20): a filter's chord
    // turns it on from any other view and off from its own, `t c` turns every filter off, and
    // the rest open the steps, Documents' list, the history and the collaborators.
    for (const [key, address] of [
      ["k", "verification"],
      ["p", "plan"],
      ["e", "steps"],
      ["d", "decisions"],
      ["a", "documents"],
      ["v", "activity"],
      ["o", "collaborators"],
      ["s", "spec"],
    ] as const) {
      await page.keyboard.press("t");
      await page.keyboard.press(key);
      await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/${address}$`));
    }
    // Pressed from its own filter, the chord turns it off.
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await page.keyboard.press("t");
    await page.keyboard.press("p");
    await page.keyboard.press("t");
    await page.keyboard.press("c");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));

    // `t h` is the outline's own chord (part I's `useTreeKeymap.ts`), so it is pressed with the
    // outline drawn - every filter off, as it is now - where the source pressed it from the
    // Decisions filter, which draws a tree of its own.
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/children$`));

    // ? lists the scope as "Issue"; the digits collapse to one 0 – 3 row.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const issueSection = dialog.getByRole("region", { name: "Issue", exact: true });
    await expect(issueSection).toContainText("Set priority P0–P3");
    await expect(issueSection).toContainText("Focus the status");
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Set priority P0–P3" }).locator("kbd")
    ).toHaveText(["0", "3"]);
    // Each chord is listed under what it does on this page, not under a tab the page no longer
    // has (`web/src/features/issue/useIssueKeymap.ts`'s `CHORDS`): the filters by the row's own
    // names, and the panel, the children and Documents' list as the page names them.
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `${PRESET_NAMES.conversation}: turn every filter off` })
        .locator("kbd")
    ).toHaveText(["t", "c"]);
    // `t v` is the activity and `t c` the conversation, so Verification takes `k`.
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `Turn the ${PRESET_NAMES.verification} filter on or off` })
        .locator("kbd")
    ).toHaveText(["t", "k"]);
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `Turn the ${PRESET_NAMES.plan} filter on or off` })
        .locator("kbd")
    ).toHaveText(["t", "p"]);
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: CONTROL_LABELS.open_steps })
        .locator("kbd")
    ).toHaveText(["t", "e"]);
    // The history takes `t v`, since `t a` opens Documents' list and `l` the labels. Its words
    // are the page's own (`useIssueKeymap.ts`): `/activity` opens Details at the issue's history
    // (R15), as the user docs' address table has it.
    await expect(issueSection).toContainText("Open the issue's history in Details");
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: "Open the issue's history in Details" })
        .locator("kbd")
    ).toHaveText(["t", "v"]);
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: "Open the issue's collaborators in Details" })
        .locator("kbd")
    ).toHaveText(["t", "o"]);
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: /\bTree\b/ }),
      "the help still names a Tree"
    ).toHaveCount(0);
    // No row of the help names a tab this page does not have (R21).
    for (const gone of ["Verification tab", "Children tab", "Artifacts tab", "Collaborators tab"]) {
      await expect(
        issueSection.getByRole("listitem").filter({ hasText: gone }),
        `the help still lists "${gone}", which this page has no such thing for`
      ).toHaveCount(0);
    }
    await page.screenshot({
      path: testInfo.outputPath(`issue-shortcut-help-${testInfo.project.name}.png`),
    });
  } finally {
    await context.close();
  }
});

interface OpenedTree {
  readonly context: BrowserContext;
  readonly page: Page;
  readonly seeded: WholeIssueSeed;
}

/** The whole issue, opened by one signed-in reader with the focus on the page itself, which is
 *  where reading mode is. A key pressed before this returns reaches no handler, because the
 *  keymap binds only once sign-in resolves. It asserts only that the page's opening put the focus
 *  on a row of its own and that the focus then left for the page itself (`expectOpeningLanded`,
 *  `focusThePage`): each row below asserts the key or the tree property it is about first, and
 *  `expectTreeDrawn` after it. */
async function openTree(browser: Browser): Promise<OpenedTree> {
  const seeded = await seedWholeIssue();
  const context = await asReader(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ height: 900, width: 1280 });
  await gotoIssue(page, `/issues/${seeded.key}/conversation`);
  await expectOpeningLanded(page);
  await focusThePage(page);
  return { context, page, seeded };
}

/** The redesigned page and its tree are on the screen, which every row asserts after its own
 *  subject: a page that answers a key but draws no tree fails here. */
async function expectTreeDrawn(page: Page): Promise<void> {
  expect(
    await issuePage(page).count(),
    "the page draws no [data-issue-page] root, so this is not the redesigned issue page"
  ).toBe(1);
  expect(await tree(page).count(), 'the page draws no role="tree" named "Issue tree"').toBe(1);
}

/** The ids of the rows a reader can see, in the order the keys move through them. */
async function visibleIds(page: Page): Promise<string[]> {
  return (await drawnRows(page)).filter((row) => row.visible).map((row) => row.id ?? "<no id>");
}

/** Where the keys have left the reader: the row holding the focus, or the one the tree names with
 *  `aria-activedescendant`. */
async function focusedRow(page: Page): Promise<string | null> {
  // Where the keys left the reader at this instant is what this answers, by the focus or else
  // by the row the tree names: a caller that waits for a row polls this whole function.
  return (await focusedNodeId(page)) ?? (await treeActiveDescendant(page));
}

test("every chord the page bound before the redesign brings a target on screen with the focus in it", async ({
  browser,
}) => {
  const { context, page } = await openTree(browser);
  try {
    // The filter chords of the key table: `t d`, `t k`, `t s` and `t p`, each turning its filter
    // on from the one before it, and `t c`, which turns every filter off. Each leaves the focus
    // in the outline.
    const chords = [
      { preset: PRESET_NAMES.decisions, second: "d" },
      { preset: PRESET_NAMES.verification, second: "k" },
      { preset: PRESET_NAMES.spec, second: "s" },
      { preset: PRESET_NAMES.plan, second: "p" },
      { preset: PRESET_NAMES.conversation, second: "c" },
    ] as const;
    for (const chord of chords) {
      await focusThePage(page);
      await page.keyboard.press("t");
      await expect(page.getByTestId("chord-indicator")).toHaveText(/t/);
      await page.keyboard.press(chord.second);
      await expect
        .poll(() => activePreset(page), {
          message: `t ${chord.second} does not open ${chord.preset}`,
        })
        .toBe(chord.preset);
      await expectTreeDrawn(page);
      // The tree is a column of its own and may be taller than the screen, which no reading of
      // "every pixel on screen" could pass; the page shows the control it leaves the reader on
      // where a part does not fit (`useIssueKeymap.ts`'s `showPart`), and that control is the
      // tree's one tab stop, so the row it lands on is what is read.
      await expect.poll(() => focusedRow(page)).not.toBeNull();
      const landed = await focusedRow(page);
      expect(
        await onScreen(nodeRow(page, landed ?? "").first()),
        `t ${chord.second} does not bring the row it lands on on screen`
      ).toBe(true);
      expect(
        await focusInside(tree(page)),
        `t ${chord.second} does not put the focus in the outline`
      ).toBe(true);
    }

    // The history, in Details: `t v` opens Details at it and puts the focus in it. The panel it
    // is drawn in is a full-height column, taller than the screen by construction, so what the
    // chord has to bring on screen is the history's own head, where it leaves the focus.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("v");
    await expect(historyPanel(page)).toBeVisible();
    await expect
      .poll(() => focusInside(historyPanel(page)), {
        message: "t v does not put the focus in the history",
      })
      .toBe(true);
    expect(
      await onScreen(historyPanel(page).getByRole("heading").first()),
      "t v leaves the history off screen"
    ).toBe(true);
    expect(await focusInside(marginSlot(page)), "t v does not put the focus in the panel").toBe(
      true
    );
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape does not close Details").toBeHidden();

    // The steps, in Details: `t e`, which named the Steps tab before this change (answer A),
    // opens Details at the issue's steps under the history and puts the focus on their heading,
    // which it brings to the top of Details' view.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("e");
    await expect(stepsSection(page)).toBeVisible();
    await expect
      .poll(() => focusInside(stepsSection(page)), {
        message: "t e does not put the focus in the steps",
      })
      .toBe(true);
    expect(
      await onScreen(stepsSection(page).getByRole("heading", { level: 2 })),
      "t e leaves the steps off screen"
    ).toBe(true);
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape does not close Details").toBeHidden();

    // The collaborators, in Details: `t o`. Details is a layer over the page, the identity block
    // included, with a scroll box of its own, so the chord scrolls that box to the row and leaves
    // the page under it where the reader had it.
    await focusThePage(page);
    await page.evaluate(() => window.scrollTo(0, 300));
    const pageTop = await page.evaluate(() => window.scrollY);
    expect(pageTop, "the seeded page is too short to be scrolled under Details").toBeGreaterThan(0);
    await page.keyboard.press("t");
    await page.keyboard.press("o");
    const collaborators = marginSlot(page).locator('[data-details-row="collaborators"]');
    await expect
      .poll(() => focusInside(collaborators), { message: "t o does not put the focus on them" })
      .toBe(true);
    expect(await onScreen(collaborators), "t o leaves the collaborators off screen").toBe(true);
    expect(
      await page.evaluate(() => window.scrollY),
      "t o scrolls the page under Details, which cannot move the row"
    ).toBe(pageTop);
    // Details is the layer over the page, which Escape closes before the page's own Escape
    // does anything else; the chords below are read on the page under it.
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape does not close Details").toBeHidden();

    // The children's group of rows in the outline: `t h` puts the focus on its first row
    // (the architect's ruling 5 on R12).
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    await expect
      .poll(
        () =>
          childRows(page)
            .first()
            .evaluate((row) => row === document.activeElement),
        {
          message: "t h does not put the focus on the first row of the children's group",
        }
      )
      .toBe(true);
    expect(await onScreen(childRows(page).first()), "t h leaves the children off screen").toBe(
      true
    );

    // The children's address takes the focus back from whatever holds the page while nothing
    // does, until the reader acts (`IssueTree.tsx`'s landing), so the reader acts first - a
    // Shift, which no key of the page binds - and the focus then leaves for the page for good.
    await page.keyboard.press("Shift");

    // Documents' list: `t a`, which named the Artifacts tab before this change, opens it with the
    // focus on its first row, on screen (part N's R4).
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("a");
    await expect(documentsList(page), "t a opens no Documents' list").toBeVisible();
    await expect(
      firstDocumentLink(page),
      "t a does not put the focus on Documents' first row"
    ).toBeFocused();
    expect(
      await onScreen(firstDocumentLink(page)),
      "t a leaves Documents' first row off screen"
    ).toBe(true);
    await expectActiveControl(page, "documents");
    const firstName = await firstDocumentLink(page).innerText();

    // From the row `t a` left the focus on, Enter opens its document in place of the list and of
    // the outline, and Escape - the key table's step after leaving a caret - closes it, back to
    // the list with the focus on that row (part N's R2); Escape on the list then draws the
    // outline again.
    await page.keyboard.press("Enter");
    await expect(
      openedArtifact(page),
      "Enter on Documents' first row opens no document"
    ).toHaveCount(1);
    expect(
      await tree(page).count(),
      "the opened document is not drawn in place of the outline"
    ).toBe(0);
    await page.keyboard.press("Escape");
    await expect(documentsList(page), "Escape does not close the opened document").toBeVisible();
    expect(await openedArtifact(page).count(), "Escape leaves the document drawn").toBe(0);
    await expect(
      documentLink(page, firstName),
      "Escape leaves the focus off the document's row"
    ).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(
      tree(page),
      "Escape on Documents' list does not draw the outline again"
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

test("Escape leaves the caret, then closes a document, then clears the words, then turns every filter off, then leaves the outline", async ({
  browser,
}) => {
  const { context, page, seeded } = await openTree(browser);
  try {
    const spec = await getArtifact(seeded.specArtifactId);
    const words = "Requirement";
    await gotoIssue(page, `/issues/${seeded.key}/spec?q=${words}`);
    await expectActivePreset(page, PRESET_NAMES.spec);
    await expect(filterBox(page)).toHaveValue(words);

    // A document opened from a chip of the Spec view, with the caret in its text: the caret is
    // left first, and the next Escape closes the document, back to the view and the words the
    // chip was pressed under (part N's R2: a chip's document goes back to the chip).
    const chip = issuePage(page).locator(
      `a[data-row-chip][data-row-reference="dispatch://${seeded.key}/artifact/${seeded.documents.cited}"]`
    );
    await expect(chip, "the Spec view draws no chip for the cited document").toHaveCount(1);
    await chip.click();
    await expect(page).toHaveURL(
      new RegExp(`/issues/${seeded.key}/artifacts/${seeded.documents.cited}$`)
    );
    await expect(openedArtifact(page)).toHaveCount(1);
    const editor = openedArtifact(page).getByRole("textbox", { name: "Document editor" });
    await editor.click();
    await expect
      .poll(() => editor.evaluate((box) => box.contains(document.activeElement)))
      .toBe(true);
    await page.keyboard.press("Escape");
    await expect
      .poll(() => editor.evaluate((box) => box.contains(document.activeElement)))
      .toBe(false);
    await expect(openedArtifact(page), "Escape closed the document with the caret").toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(openedArtifact(page), "Escape does not close the document").toHaveCount(0);
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/spec\\?q=${words}$`));

    // The caret step of the key table is read in the outline, with the words still on: part J's
    // Spec view draws each statement and edits none - Enter on a row of it goes to that node in
    // the conversation (`spec-filter/SpecFilterBody.tsx`) - and the outline is where this page
    // puts a caret in a row (part I's `useTreeKeymap.ts`). What the step proves is the order:
    // Escape takes the caret before it clears the words.
    await gotoIssue(page, `/issues/${seeded.key}?q=${words}`);
    await expectOpeningLanded(page);
    const outlineRow = nodeRow(page, blockNodeId(spec.slug, requirementBlockId(2))).first();
    await outlineRow.focus();
    const caretRow = await focusedRow(page);
    expect(caretRow, "the words draw no row to stand on").not.toBeNull();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the row" })
      .toBe(true);
    await page.keyboard.press("Escape");
    expect(await caretInNode(page), "Escape did not leave the caret").toBe(false);
    expect(await focusedRow(page)).toBe(caretRow);

    // Back to the view with the words on, for the three steps below, standing on a row of it.
    await gotoIssue(page, `/issues/${seeded.key}/spec?q=${words}`);
    await expectActivePreset(page, PRESET_NAMES.spec);
    const row = nodeRow(page, blockNodeId(spec.slug, requirementBlockId(2))).first();
    await row.focus();
    const on = await focusedRow(page);
    expect(on, "the filter draws no row to stand on").not.toBeNull();

    // Then the words: the filter stays on, and the reader stays in the outline.
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/spec$`));
    // The box shuts into its Filter control as its words go (part N's R4: it is drawn while it
    // holds words or has the caret), where the source's box stayed drawn and empty.
    await expect(filterBox(page), "the words box is drawn with no words in it").toHaveCount(0);
    await expect(filterControl(page)).toBeVisible();
    await expectActivePreset(page, PRESET_NAMES.spec);

    // Then the filter: every filter off, with the reader on the same node of the outline, at
    // Conversation's own address (`escape-order.ts`): the issue's own draws Dashboard
    // (dispatch://INFRA-5702).
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/conversation$`));
    await expectActivePreset(page, PRESET_NAMES.conversation);
    await expect.poll(() => focusedRow(page), { message: "the reader left the outline" }).toBe(on);

    // Then the outline itself.
    await page.keyboard.press("Escape");
    await expect
      .poll(() => focusedRow(page), { message: "Escape did not leave the outline" })
      .toBe(null);
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/conversation$`));
  } finally {
    await context.close();
  }
});

test("t f filters inside the preset, $mod+Alt+m comments on a selection, and / stays the global search", async ({
  browser,
}) => {
  const { context, page, seeded } = await openTree(browser);
  try {
    // t f filters inside the preset: the filter takes the focus and narrows the rows.
    const before = await visibleIds(page);
    await page.keyboard.press("t");
    await page.keyboard.press("f");
    expect(await filterBox(page).count(), "t f opens no filter").toBe(1);
    await expectTreeDrawn(page);
    expect(await focusInside(filterBox(page)), "t f does not put the focus in the filter").toBe(
      true
    );
    await page.keyboard.type("Requirement 2");
    await expect.poll(async () => (await visibleIds(page)).length).toBeLessThan(before.length);
    const narrowed = (await drawnRows(page)).filter((row) => row.visible && !row.context);
    expect(
      narrowed.filter((row) => !row.text.includes("Requirement 2")).map((row) => row.id),
      "the filter kept rows that do not match what was typed"
    ).toEqual([]);
    // A filter that hides every row meets that, and meets the count above too, so what is left
    // is asserted as well: the row the words name is still drawn.
    expect(
      narrowed.map((row) => row.id),
      "the filter hid every row of the tree, matches included"
    ).not.toEqual([]);

    // `/` is the application's global search, which this page does not shadow.
    await page.keyboard.press("Escape");
    await focusThePage(page);
    await page.keyboard.press("/");
    await expect(page.getByRole("dialog", { name: /search/i })).toBeVisible();
    await page.keyboard.press("Escape");

    // $mod+Alt+m still comments on a selection inside a node's own text, which the editor takes
    // the place of in the row's words (`NodeRow.tsx`'s stage).
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const requirement = nodeRow(page, blockNodeId(specSlug, requirementBlockId(2))).first();
    await requirement.focus();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the node" })
      .toBe(true);
    await selectTextIn(page, rowWords(requirement), "requirement");
    await page.keyboard.press("ControlOrMeta+Alt+m");
    // What the chord does while part L follows (part N's R11): it marks the selection as a
    // comment in the row's own editor, which is this page's proof of R30 of the root spec.
    // Today's composer, which the margin opened the mark in, is no part of this page yet
    // (`AWAITING_THE_BOX`), so nothing is offered to write the comment in; part L's pull
    // request reads its box here instead.
    await expect(
      rowWords(requirement).locator(".mark-comment"),
      "$mod+Alt+m marked no selection in the row's editor"
    ).toHaveCount(1);
    await expect(
      marginSlot(page).getByRole("form", { name: "Comment composer" }),
      "the page offers today's composer while part L follows"
    ).toBeHidden();
  } finally {
    await context.close();
  }
});

// ---- from 750e2716:packages/dispatch/e2e/live-socket.e2e.ts

const _agent: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/live-socket",
  machine_id: "live-socket-host",
  roles: [],
  session_id: "01a0e090-4848-7473-acc5-fc96e6a646d4",
  title: "Live Socket Agent",
};

const bob = { actor: { id: "bob", kind: "session" as const }, as: "agent" as const };

/** Waits until the page holds one open live socket and its event feed has started on it. */
async function untilFeedStarts(sockets: LiveSockets): Promise<void> {
  await expect.poll(() => sockets.open.size).toBe(1);
  await expect.poll(() => sockets.received.includes("events_start")).toBe(true);
}

function _replay(
  sessionID: string,
  extra: readonly Record<string, unknown>[] = []
): Record<string, unknown> {
  return {
    frames: [
      ...Array.from({ length: 40 }, (_, index) => ({
        kind: "message",
        message: {
          at: 1_000 + index,
          id: `turn-${index}`,
          parts: [
            {
              text: `Turn ${index}: ${"The live socket keeps this text. ".repeat(4)}`,
              type: "text",
            },
          ],
          role: index % 2 === 0 ? "user" : "assistant",
          streaming: false,
        },
        seq: index + 1,
        v: 1,
      })),
      ...extra,
    ],
    session_id: sessionID,
    v: 1,
  };
}

function trackApiRequests(page: Page): Map<Request, number> {
  const pending = new Map<Request, number>();
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/api/")) pending.set(request, Date.now());
  });
  const settled = (request: Request) => pending.delete(request);
  page.on("requestfailed", settled);
  page.on("requestfinished", settled);
  return pending;
}

async function waitForReads(pending: Map<Request, number>): Promise<void> {
  await expect.poll(() => pending.size).toBe(0);
}

/** What a recorded read path carries in place of the issue's key, so that two pages, each
 *  reading an issue of its own, compare as lists. */
const ISSUE_PATH = "/api/v1/issues/<issue>";

/** The reads an issue's activity tab makes for itself at its first load. A window that counts
 *  what an event costs a page opens only after these have gone out, since they are the page's
 *  own lazy first reads and not the event's doing. */
const FIRST_ISSUE_READS = [`${ISSUE_PATH}/comments`, `${ISSUE_PATH}/asks`, `${ISSUE_PATH}/events`];

/** Every read the page makes of the one issue `key` names, in order, as the request's pathname
 *  with that key replaced by `<issue>`. Reads of anything else are left out: the agent list, the
 *  inbox and the reader's own state are not what a comment on this issue invalidates, so a
 *  periodic one of those landing inside one page's window and outside another's would make two
 *  equal pages count differently. The query string is dropped because it names the page's own
 *  issue's rows - an incremental event read asks for the ids it is missing - which differ
 *  between two pages by construction and would read as different reads. */
function trackIssueReads(page: Page, key: string): string[] {
  const reads: string[] = [];
  const prefix = `/api/v1/issues/${key}`;
  page.on("request", (request) => {
    const { pathname } = new URL(request.url());
    if (pathname === prefix || pathname.startsWith(`${prefix}/`)) {
      reads.push(`${ISSUE_PATH}${pathname.slice(prefix.length)}`);
    }
  });
  return reads;
}

/** Leaves the page in the state a counting window needs: its own first reads of the issue have
 *  gone out, and nothing it started is still in flight. */
async function untilIssueReadsSettle(
  reads: readonly string[],
  pending: Map<Request, number>
): Promise<void> {
  for (const read of FIRST_ISSUE_READS) await expect.poll(() => reads).toContain(read);
  await waitForReads(pending);
}

/** Every document the page fetched for itself, in order: its first load and every reload after
 *  it. The `load` event is not that count - Playwright's Firefox fires one for the page's initial
 *  `about:blank`, Chromium none - while a reload is a navigation request of the main frame in
 *  every engine. */
function trackDocumentLoads(page: Page): string[] {
  const loaded: string[] = [];
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      loaded.push(request.url());
    }
  });
  return loaded;
}

// The page rotates before it has seen any event, so its event feed resumes from the sign-in
// check's head: the change committed while the next socket waits arrives on it, and the
// rotation costs no read of the issue beyond what that change costs a page with no rotation
// (the control). Reads of the issue are what the title claims and all this row counts; a
// request to any other route is outside both.
// Each window counts that page's reads of its own issue, by path, because a count of every
// `/api/` request counted two different things: in CI job 127925489549 the control's window held
// its tab's lazy first reads of `/comments` and `/asks?state=all` and a periodic
// `/api/v1/agents`, which no comment event invalidates, while the rotating page had made those
// reads long before its own window opened - 5 reads against 4, and job 127873213156 counted 6
// against 4 the same way. So both windows now open only once that page's own first reads have
// gone out and settled, and the two lists are compared read for read.
// SLOW(>=2s): the two quiet windows below, one on the control page and one on the rotating
// page, are what "no extra read of the issue" is counted over. A count of what a page did not
// do over a window is that window's own real time, and the two must stay equal for the counts
// to compare.
// Both tags in the title are what select this row: `@phone` puts it in `iphone`, and `@firefox`
// in `firefox`, which takes rows by title tag rather than by file name since
// cybermonklabs/legion#2866. The two occurrences this change fixes were seen in exactly those
// two projects, so the row must keep both.
test("a rotation loses no event and makes no extra read of the issue @phone @firefox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const baselineIssue = await createIssue({ project: "CORE", title: "Comment request control" });

  const alice = await asReader(browser, "alice");
  try {
    const control = await alice.newPage();
    const controlPending = trackApiRequests(control);
    const controlReads = trackIssueReads(control, baselineIssue.key);
    const controlSocket = control.waitForEvent("websocket", isLiveSocket);
    await control.goto(`/issues/${baselineIssue.key}/activity`);
    // The title is drawn as the page's heading and again as a fact of the focused node
    // (dispatch://INFRA-1196 R12): the page has loaded when its heading says it.
    await expect(
      control.getByRole("heading", { exact: true, name: "Comment request control" })
    ).toBeVisible();
    await controlSocket;
    // The rotating page's window opens long after its first reads; this leaves the control page
    // in that same state, so that neither window holds a read the page would have made anyway.
    await untilIssueReadsSettle(controlReads, controlPending);

    const controlWindow = controlReads.length;
    const controlComment = await createComment(
      baselineIssue.key,
      { body: "Control comment request count" },
      bob
    );
    await expect(
      control.locator(`a[href="/issues/${baselineIssue.key}/comments/${controlComment.id}"]`)
    ).toBeVisible();
    // A read an event invalidates goes out in the frame after that event, so a window that
    // starts once the comment's link is on screen is a settle rather than a guess.
    await control.waitForTimeout(1_500);
    const expectedReads = controlReads.slice(controlWindow).sort();
    await control.close();

    const issue = await createIssue({ project: "CORE", title: "Rotation target" });
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    const reads = trackIssueReads(page, issue.key);
    const sockets = watchLiveSockets(page);
    let holdReconnect = false;
    let held: WebSocketRoute | undefined;
    const reconnectReleased = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      if (holdReconnect) {
        held = route;
        await reconnectReleased.promise;
        connect(route);
        return;
      }
      connect(route);
    });
    const first = page.waitForEvent("websocket", isLiveSocket);
    await page.goto(`/issues/${issue.key}/activity`);
    await expect(page.getByRole("heading", { exact: true, name: "Rotation target" })).toBeVisible();
    await expect(historyPanel(page)).toBeVisible();
    await first;
    await expect.poll(() => sockets.received.includes("events_start")).toBe(true);
    await untilIssueReadsSettle(reads, pending);

    expect(sockets.received).not.toContain("event");
    holdReconnect = true;
    await rotateLiveSockets();
    await expect.poll(() => held !== undefined).toBe(true);
    const comment = await createComment(issue.key, { body: "Committed while rotation waits" }, bob);
    await expect(page.getByTestId("connection-pill")).toHaveCount(0);

    if (held === undefined) throw new Error("the reopened live socket was not held");
    const rotationWindow = reads.length;
    reconnectReleased.resolve();
    await expect.poll(() => sockets.received).toContain("event");
    await expect(
      page.locator(`a[href="/issues/${issue.key}/comments/${comment.id}"]`)
    ).toBeVisible();
    await expect(page.getByTestId("connection-pill")).toHaveCount(0);
    // The control's window, to the millisecond: the two lists are of equal quiet.
    await page.waitForTimeout(1_500);
    // Read for read, sorted, so a failure names the read that differs rather than a count.
    expect(reads.slice(rotationWindow).sort()).toEqual(expectedReads);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a cold load in WebKit and in Firefox with the page's first live socket held open
// through it, which each engine then has to replay the missed change on.
// Off the pull-request lane: `main`'s source row in `live-socket.e2e.ts` runs there, and the
// lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
test("a change committed while the page loads is shown without a reload", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Before the page load" });

  const alice = await asReader(browser, "alice");
  try {
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    const loaded = trackDocumentLoads(page);
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      held = route;
      await firstSocketReleased.promise;
      connect(route);
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    // The title is the page's heading; the focused node's facts draw it a second time.
    await expect(
      page.getByRole("heading", { exact: true, name: "Before the page load" })
    ).toBeVisible();
    await expect.poll(() => held !== undefined).toBe(true);
    await waitForReads(pending);

    await patchIssue(issue.key, { title: "Committed during the page load" }, bob);
    if (held === undefined) throw new Error("the first live socket was not held");
    const opened = page.waitForEvent("websocket", isLiveSocket);
    firstSocketReleased.resolve();
    await opened;

    await expect(
      page.getByRole("heading", { exact: true, name: "Committed during the page load" })
    ).toBeVisible({ timeout: 5_000 });
    expect(loaded).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("a first live socket opening without a change makes no API request", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Quiet page load" });
  // The reader has read this issue through its head already. The outline's end is on screen the
  // moment an empty issue loads, and a second there writes an unread issue's read mark
  // (`IssueReadMark.tsx`): a change of the reader's state, whose own event the opening socket
  // then delivers and the page reads the state back for. That is the reader reading, not the
  // socket's open, so the seed leaves the mark nothing to write, and the open is the one thing
  // left that could cause a request.
  const [head] = await getIssueEvents(issue.key, { limit: 1, order: "desc" });
  await putIssueState(issue.key, { last_read_seq: head?.seq ?? 0 }, { login: "alice" });

  const alice = await asReader(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchApiRequests(page);
    const sockets = watchLiveSockets(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      held = route;
      await firstSocketReleased.promise;
      connect(route);
    });
    const requestsAfterOpen: string[] = [];
    let countRequests = false;
    page.on("request", (request) => {
      if (countRequests && new URL(request.url()).pathname.startsWith("/api/")) {
        requestsAfterOpen.push(`${request.method()} ${request.url()}`);
      }
    });
    // Nothing writes the reader's state on this load: a mark written here would be a change.
    const stateWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        new URL(request.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
      ) {
        stateWrites.push(request.postData() ?? "");
      }
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    // The title is the page's heading; the focused node's facts draw it a second time.
    await expect(page.getByRole("heading", { exact: true, name: "Quiet page load" })).toBeVisible();
    await expect.poll(() => held !== undefined).toBe(true);
    // The load is over once nothing has been in flight for the recorded baseline's window, not at
    // the first moment nothing is: the outline's one batched read of the names its references
    // cite goes out only after each of the tree's reads has answered (`useIssueTree.ts`), so the
    // reads can all be in for a moment before it is sent, and a count started then would take it
    // for the open's own (CI at 88443e7d: `reference-targets?ref=dispatch://CORE-1`).
    await expectLoadSettled(traffic);
    if (held === undefined) throw new Error("the first live socket was not held");
    countRequests = true;
    firstSocketReleased.resolve();
    // The socket opened and the feed started on it, so the quiet below is the open's own.
    await untilFeedStarts(sockets);
    // One second of real quiet. A read the open itself causes goes out in the same tick as the
    // `events_start` awaited above, so this window is many frames past it. The page's clock
    // cannot stand in for it: a jump long enough to be worth making also fires the shell's own
    // credential poll (15 s, `features/credentials/pending.ts`), a read on a cadence of its own
    // that this row does not count.
    await page.waitForTimeout(1_000);
    expect(stateWrites, "the load wrote the reader's state of an issue they had read").toEqual([]);
    expect(requestsAfterOpen).toEqual([]);
  } finally {
    await alice.close();
  }
});

// ---- from 750e2716:packages/dispatch/e2e/live.e2e.ts

/** A person writing on the issue, whose message is a node of the outline (dispatch://INFRA-1196
 *  R1): an agent's message no person took part in is the history's, in Details (R15). */
const carol = { login: "carol" };

test("live: a fresh page load opens the socket at the current head and stays within a bounded request budget", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Budget target" });
  for (let index = 0; index < 5; index += 1) {
    await createMessage(issue.key, { body: `Backlog message ${index}` }, carol);
  }

  const alice = await asReader(browser, "alice");
  const page = await alice.newPage();
  const apiRequestPaths: string[] = [];
  // What says the load is over: nothing in flight for the recorded baseline's window
  // (`expectLoadSettled`), counting every request the page makes and not only these.
  const settling = watchApiRequests(page);
  page.on("request", (request) => {
    // Called for every request of a cold load this row measures, so the cheap string test comes
    // first: a path under /api/v1/ or /auth/ always shows in the raw URL.
    const raw = request.url();
    if (!(raw.includes("/api/v1/") || raw.includes("/auth/"))) return;
    const url = new URL(raw);
    if (url.pathname.startsWith("/api/v1/") || url.pathname.startsWith("/auth/")) {
      apiRequestPaths.push(url.pathname + url.search);
    }
  });

  const socketOpened = page.waitForEvent(
    "websocket",
    (socket) => new URL(socket.url()).pathname === "/ws/live"
  );
  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(tree(page).getByText("Backlog message 4")).toBeVisible();
  await socketOpened;

  // The socket is outside the HTTP request budget, and the sign-in check carries its event head.
  expect(apiRequestPaths.filter((path) => path.split("?")[0] === "/api/v1/events")).toEqual([]);

  // Exactly these addresses on a fresh load, and nothing else. A set rather than a count, and
  // waited for rather than read at an instant: the page issues the last requests of its first
  // load within a few milliseconds of painting the message this row waits for, so a count
  // taken the moment that message appears can be one request short, which is how this row
  // failed in CI - 20 against 21. The wait ends on the set itself, so a panel that starts
  // eagerly fetching before its own address is ever opened still trips this immediately
  // instead of only breaking some looser upper bound, and a failure names the address that is
  // missing or extra instead of leaving a reader to work out which of a count of N dropped.
  // The page pages no event log at all on this address: the history is Details', and it is
  // drawn only once Details opens at it (dispatch://INFRA-1196 R15).
  //
  // It is the set of addresses, not the list of requests: the page reads an address it has
  // already read when a query of the shell's invalidates, which is that query's business and
  // not this budget's. What this holds is that no surface reads an address the page has no
  // reason to read on a first load.
  //
  // The last reads of the set are the one tree the page's body is, which every preset is a
  // query over and which therefore draws on every address (dispatch://INFRA-1196 R1, R3): the
  // issue's coverage, its decisions, its check results, its references and the blocks of each
  // of its documents - this issue carries one, whose nodes the tree draws - and the one batched
  // read of every `dispatch://` address the tree holds. That read is one request and not one
  // per read that lands: the tree grows as each of its reads answers, so the batch waits until
  // all of them have (`features/issue-page/useIssueTree.ts`), where before it asked for the
  // addresses known so far and asked again for the rest. The tree's asks, comments and messages
  // are read under the keys the margin already owns, so they are shared rather than asked twice
  // and are the entries already here.
  //
  // It is the source's set (`750e2716`'s `e2e/live.e2e.ts`) less four addresses and with two
  // more, and no part on `main` adds one to this load. Three of the four are part L's, which
  // follows this part (part N's R11: `slots/foot.tsx` draws nothing, `AWAITING_THE_BOX`), and
  // L's pull request puts them back: the head read and the reader's state, which the read mark
  // writes, and the active-sessions read, which the box makes. The fourth, the repository
  // mappings, `main`'s page no longer reads. The two more are the reads of
  // dispatch://INFRA-5702's list of what waits on the reader, a part this branch holds, which
  // the header's count counts (R5). Each is named where it stood or stands.
  const firstLoad = [
    "/auth/whoami",
    // The project sidebar's Inbox query, which the margin's open-ask count shares rather than
    // fetching again.
    "/api/v1/inbox",
    // The Projects query. The sidebar's Pinned list and its Priorities badge
    // (`/api/v1/issues?pinned=true`, `/api/v1/priorities?summary=true`) are not read here: on
    // the issue page the navigation is the icon rail, which draws neither, and the full list
    // that does is read once the rail's foot control opens it (dispatch://INFRA-1196 R14).
    "/api/v1/projects",
    "/api/v1/me/state",
    // The issue's detail.
    `/api/v1/issues/${issue.key}`,
    // No active-sessions read (`/api/v1/agents`): on this address the source made it for the
    // box at the outline's foot, whose mention picker lists the live agents
    // (`conversation/MentionComposer.tsx`'s `useAgents`). That box is part L's (R11). Nothing
    // else on this load names an agent's session: the issue has no claim, and its messages are
    // a person's.
    // The primary artifact's comments, which the margin and the tree share rather than fetching
    // again.
    `/api/v1/issues/${issue.key}/comments`,
    // The margin's own list of the issue's asks, which the tree shares. The panel's conversation
    // list asked the same route for each ask's thread (`?thread=1`) as a request of its own; the
    // outline draws an ask's replies from the comments above, and that list is gone (R14).
    `/api/v1/issues/${issue.key}/asks?state=all`,
    // The issue's open asks, one of the two reads of dispatch://INFRA-5702's list of what waits
    // on the reader (`features/dashboard/useWaitingItems.ts`), whose items the header's count
    // counts on every view (`IssuePropertiesRow.tsx`, R5): its questions and approval requests.
    `/api/v1/issues/${issue.key}/asks?state=open`,
    // The issue's message threads, which the tree shares.
    `/api/v1/issues/${issue.key}/messages`,
    // No read of the issue's head (`events?limit=1&order=desc`): the read mark at the outline's
    // end that writes it is part L's (`IssueReadMark.tsx`), and part L follows this part (part
    // N's R11). Part L's pull request puts that address back in this list.
    // The live-agent query.
    `/api/v1/issues/${issue.key}/subscribers`,
    // The header's read of the project's architecture sources: the `Components:` line offers
    // its picker only when the project has one. The component tree itself,
    // `/api/v1/projects/CORE/architecture`, is fetched only once the picker opens, never on
    // load, which is why it is absent from this set.
    "/api/v1/projects/CORE/architecture-sources",
    // No read of the repository mappings (`/api/v1/settings/repo-projects`): `main` dropped them
    // at #4457 (85f90626), and the header's `Repository:` item reads the project's default
    // repository from the Projects query above.
    // me/agents/state, whose unread replies the Agents badge counts on every page (the
    // sidebar's on desktop, the compact header's on phone).
    "/api/v1/me/agents/state",
    // The credential requests waiting on the viewer, which the Inbox count adds up on every
    // page.
    "/api/v1/credential-requests?approver=me",
    // `me/replies?kind=reply,agent&limit=1`, whose total is the other half of that one Inbox
    // number (the sidebar's on desktop, the compact header's on phone).
    "/api/v1/me/replies?kind=reply%2Cagent&limit=1",
    // The reader's unread threads of the issue, the other read of dispatch://INFRA-5702's list
    // behind the header's count (R5).
    `/api/v1/me/replies?issue=${issue.key}&state=unread`,
    // No list of the issue's phase handoffs: the list drew them beside the messages, and they
    // are lines of the history now, read with its log.
    // No write of the reader's own state on this issue (`PUT /api/v1/me/issues/<key>/state`):
    // in the source it was the read mark's, sent once the outline's end had been on screen for
    // a second, through the head read above (`IssueReadMark.tsx`'s `last_read_seq`). The read
    // mark is part L's (R11).
    // The tree's own reads, and the blocks of the one document the issue carries, which are
    // the nodes the tree draws of it. The document's *text* is no read of a first load:
    // nothing on load parses stored markdown any more (dispatch://INFRA-1196 R20). The head's
    // three sentences were read out of the spec's own text at the summary block's span, and
    // R20 replaced both reads with the summary node the tree already carries
    // (`features/issue-page/IssueThreeThings.tsx`); the editor that reads a document's text is
    // mounted only where a document preset is open, which this address is not. The issue's
    // own reference closure (`references?own=1`) is no read either: it placed a document under
    // the block citing it, and a document is no row of the outline (R1, be70b3383a).
    `/api/v1/issues/${issue.key}/coverage`,
    `/api/v1/issues/${issue.key}/decisions`,
    `/api/v1/issues/${issue.key}/results`,
    `/api/v1/artifacts/${issue.primary_artifact_id}/blocks`,
    // The one batched read of every `dispatch://` address the tree holds. Its query is that
    // list, which is what the issue's own text holds rather than a fact about the budget, so
    // this set names the read and `seen` drops the query.
    "/api/v1/reference-targets",
  ].sort();
  // The watermark an event read starts past is the reader's, not a fact of the budget, and
  // neither is the list of references the batched read names, so both are dropped here.
  const seen = () =>
    [
      ...new Set(
        apiRequestPaths.map((path) =>
          path
            .replace(/^\/api\/v1\/reference-targets\?.*$/, "/api/v1/reference-targets")
            .replace(/\?after=\d+&limit=1$/, "?after=&limit=1")
        )
      ),
    ].sort();
  await expect.poll(seen).toEqual(firstLoad);

  // The batch is one request, which the set above cannot say: it holds the address once
  // however many times the page asked for it. The tree waits for all of its reads before it
  // asks (`features/issue-page/useIssueTree.ts`), where before it asked for the addresses known
  // so far and asked again for the rest, so the count is the assertion that rule needs. It is
  // counted once the page has gone quiet, not at the moment the set above first matched: that
  // moment is the first batch's own, so a second batch sent after the reads answer would land
  // after it and never be counted.
  await expectLoadSettled(settling);
  // The set again, once quiet: an address first read after the set above matched is no less a
  // read of the first load.
  expect(seen(), "the first load read another address once its set had matched").toEqual(firstLoad);
  expect(
    apiRequestPaths.filter((path) => path.split("?")[0] === "/api/v1/reference-targets"),
    "the page read /api/v1/reference-targets more than once on a first load"
  ).toHaveLength(1);

  await alice.close();
});

// ---- from 750e2716:packages/dispatch/e2e/phone-landing.e2e.ts

// The iPhone 13 viewport INFRA-95 measured the landing fault at, below the 390x844 `phone`
// layout `phone-conversation.e2e.ts` uses.
const iphone = { height: 664, width: 390 };

const session = {
  actor: { kind: "session" as const, id: "e2e-phone-landing" },
  as: "agent" as const,
};

// The panel's list of the conversation, which these rows held to opening on its newest message
// whichever of its reads answered last (INFRA-303), is gone (dispatch://INFRA-1196 R14): the
// conversation is the outline, read from the same three reads, and what a phone reader must
// have on the first screen of it is the question that waits on them (R6), clear of the line to
// write in at the foot (R16). These rows hold that landing to the same three orders and the same
// failure and absence the list's rows held its own landing to. The issue's own read carries its
// open questions (`tree-drafts.ts`'s `addAsks`), so a question waiting on the reader is drawn
// even while the asks read has failed or not answered, and the reader lands on it then too.

/** Seeds project CORE and an issue titled `title` whose conversation holds an open question
 *  waiting on its person, a root comment a person wrote, and nine messages an agent wrote. */
async function seedConversation(title: string) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  const question = await createAsk(
    issue.key,
    { question: "Which landing should the reader get?" },
    session
  );
  await createComment(issue.key, { body: "A root comment under the ask" });
  for (let index = 0; index < 9; index += 1) {
    await createMessage(issue.key, { body: `Agent message ${index}` }, session);
  }
  return { issue, question };
}

/** The three reads the outline's conversation is built from, under the keys the page reads them
 *  by: the asks, the comments, the messages. */
const READS = ["asks", "comments", "messages"] as const;

/** Matches the outline's read `read` for `issueKey`. */
function conversationRead(issueKey: string, read: (typeof READS)[number]): (url: URL) => boolean {
  return (url) => url.pathname === `/api/v1/issues/${issueKey}/${read}`;
}

/** The question's row in the outline. */
function questionRow(page: Page, askId: string): Locator {
  return nodeRow(page, askId).first();
}

/** A new page in `context` at the `iphone` viewport. */
async function openPhonePage(context: BrowserContext): Promise<Page> {
  const page = await context.newPage();
  await page.setViewportSize(iphone);
  return page;
}

/** Waits until `row` has finished rendering: visible, then its rect unchanged over 10 consecutive
 *  frames, sampled in the page. It only samples: nothing here scrolls, focuses, taps or hovers,
 *  so where the page put the row is where it is measured. */
async function settle(row: Locator, label: string): Promise<void> {
  await expect(row, `${label}: the question's row`).toBeVisible({ timeout: 20_000 });
  // The page function waits in the page until the row holds still or its deadline passes, so
  // the answer is that wait's own verdict.
  const never = await row.evaluate((element) => {
    const { promise, resolve } = Promise.withResolvers<string | null>();
    const deadline = Date.now() + 20_000;
    let last = "";
    let unchanged = 0;
    const sample = () => {
      const rect = JSON.stringify(element.getBoundingClientRect());
      unchanged = rect === last ? unchanged + 1 : 0;
      last = rect;
      if (unchanged >= 10) resolve(null);
      else if (Date.now() > deadline) resolve(last);
      else requestAnimationFrame(sample);
    };
    sample();
    return promise;
  });
  if (never !== null) {
    throw new Error(`${label}: the question's row never held still within 20 s (last ${never})`);
  }
}

/** One-shot: the question's row starts inside the first screen and ends above the viewport's
 *  foot. Its source measured to the line to write in at the foot of the outline, which is part
 *  L's box; part L follows this part (part N's R11), so the foot is the viewport's until part
 *  L's pull request measures to its box again. */
async function expectQuestionInView(page: Page, row: Locator, label: string): Promise<void> {
  // Every caller settled the row first, visible and still, so its card is drawn and has a box.
  const box = await row.locator("[data-node-card]").first().boundingBox();
  const view = await page.evaluate(() => ({ height: window.innerHeight, scrollY: window.scrollY }));
  if (box === null) throw new Error(`${label}: expected the question's row to have a box`);
  const where = `the question at ${JSON.stringify(box)}, the viewport ${view.height} tall at window.scrollY ${view.scrollY}`;
  expect(
    box.y >= 0 && box.y < view.height,
    `${label}: the question is off the first screen: ${where}`
  ).toBe(true);
  expect(
    box.y + box.height <= view.height,
    `${label}: the question runs past the viewport's foot: ${where}`
  ).toBe(true);
}

/** Opens the conversation on a fresh page with `last` held until the other two reads have
 *  answered and the page has had time to draw what they brought, then lets `last` answer. */
async function openWithReadLast(
  context: BrowserContext,
  issueKey: string,
  last: (typeof READS)[number]
): Promise<Page> {
  const page = await openPhonePage(context);
  const held: Route[] = [];
  let released = false;
  const heldRead = conversationRead(issueKey, last);
  const hold = async (route: Route) => {
    if (released) await route.continue();
    else held.push(route);
  };
  await page.route(heldRead, hold);
  const answered = READS.filter((read) => read !== last).map((read) =>
    page.waitForEvent("requestfinished", {
      predicate: (request) => conversationRead(issueKey, read)(new URL(request.url())),
    })
  );
  await page.goto(`/issues/${issueKey}/conversation`);
  await Promise.all(answered);
  await expect.poll(() => held.length).toBeGreaterThanOrEqual(1);
  // No in-page signal says the page has drawn the partial data, so this fixed margin - two
  // macrotasks, two frames and 300 ms - is the defended choice.
  await page.evaluate(async () => {
    for (let i = 0; i < 2; i++) {
      const turn = Promise.withResolvers<void>();
      setTimeout(turn.resolve, 0);
      await turn.promise;
    }
    for (let i = 0; i < 2; i++) {
      const frame = Promise.withResolvers<void>();
      requestAnimationFrame(() => frame.resolve());
      await frame.promise;
    }
    const margin = Promise.withResolvers<void>();
    setTimeout(margin.resolve, 300);
    await margin.promise;
  });
  // The held reads answer before the handler goes, which would otherwise hand them on itself.
  released = true;
  for (const route of held) await route.continue();
  await page.unroute(heldRead, hold);
  return page;
}

// SLOW(>=2s): three opens, each a fresh page, each holding the 300 ms margin the partial-data
// state has no in-page signal for.
// Off the pull-request lane: `main`'s source row in `phone-landing.e2e.ts` runs there, and the
// lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
test("on a phone, the conversation opens with its open question in view whichever read answers last @phone", async ({
  browser,
}) => {
  const { issue, question } = await seedConversation("Landing on the open question");
  const alice = await asReader(browser, "alice");

  try {
    let page: Page | undefined;
    for (const last of READS) {
      const label = `${last} answering last`;
      await page?.close();
      page = await openWithReadLast(alice, issue.key, last);
      await settle(questionRow(page, question.id), label);
      await expectQuestionInView(page, questionRow(page, question.id), label);
    }
  } finally {
    await alice.close();
  }
});

// A failed asks read leaves the question the issue's own read carries, and the reader lands on it;
// a refetch that then brings the asks keeps them there. The refetch is TanStack Query's own on
// focus: the `visibilitychange` this row dispatches on `document` bubbles to `window`, where its
// focusManager listens, and a read in error holds no data, so it is fetched again.
test("on a phone, a conversation whose asks read failed opens on its open question, and a refetch keeps it there @phone", async ({
  browser,
}) => {
  const { issue, question } = await seedConversation("Landing after a failed read");
  const alice = await asReader(browser, "alice");

  try {
    const page = await openPhonePage(alice);
    const asksRead = conversationRead(issue.key, "asks");
    let failing = true;
    let failed = 0;
    await page.route(asksRead, async (route) => {
      if (!failing) {
        await route.continue();
        return;
      }
      failed += 1;
      await route.fulfill({
        body: JSON.stringify({ code: "SERVER_ERROR", error: "boom" }),
        contentType: "application/json",
        status: 500,
      });
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(tree(page)).toBeVisible();
    // The app's own retries of the asks read fail too before the page settles on its failure.
    await expect.poll(() => failed, { timeout: 15_000 }).toBeGreaterThanOrEqual(3);
    await settle(questionRow(page, question.id), "with the asks read failed");
    await expectQuestionInView(page, questionRow(page, question.id), "with the asks read failed");

    failing = false;
    const answered = page.waitForResponse(
      (response) => asksRead(new URL(response.url())) && response.status() === 200
    );
    await page.evaluate(() =>
      document.dispatchEvent(new Event("visibilitychange", { bubbles: true }))
    );
    await answered;
    await settle(questionRow(page, question.id), "after the refetch");
    await expectQuestionInView(page, questionRow(page, question.id), "after the refetch");
  } finally {
    await alice.close();
  }
});

// INFRA-303: a reader who leaves the conversation while it is still being read was never in it,
// so coming back is a first open, not a return to a place: they land on the question, which they
// had on the first screen before the asks read answered too.
test("on a phone, a reader who leaves the conversation while it loads lands on its open question when they come back @phone", async ({
  browser,
}) => {
  const { issue, question } = await seedConversation("Leaving while it loads");
  const alice = await asReader(browser, "alice");

  try {
    const page = await openPhonePage(alice);
    const asksRead = conversationRead(issue.key, "asks");
    const held: Route[] = [];
    await page.route(asksRead, (route) => {
      held.push(route);
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect.poll(() => held.length).toBeGreaterThanOrEqual(1);
    await settle(questionRow(page, question.id), "before the asks read answers");
    await expectQuestionInView(
      page,
      questionRow(page, question.id),
      "before the asks read answers"
    );

    await openIssueAddress(page, "activity");
    const answered = page.waitForResponse(
      (response) => asksRead(new URL(response.url())) && response.status() === 200
    );
    for (const route of held) await route.continue();
    await page.unroute(asksRead);
    await answered;

    await openIssueAddress(page, "conversation");
    await settle(questionRow(page, question.id), "back on the conversation");
    await expectQuestionInView(page, questionRow(page, question.id), "back on the conversation");
  } finally {
    await alice.close();
  }
});

// ---- from 750e2716:packages/dispatch/e2e/ask-part-references.e2e.ts

const agent = {
  actor: { id: "e2e-ask-parts", kind: "session" as const },
  as: "agent" as const,
};

/** Opens the hover card of `link` from the keyboard, which every project can do (the phone
 *  project has no hover), and answers the card. No Escape first: the link may be in Details,
 *  which Escape closes, and no card is open before this. */
async function openCard(page: Page, link: Locator): Promise<Locator> {
  await page.mouse.move(0, 0);
  await page.keyboard.press("Tab");
  await link.focus();
  const card = page.getByRole("tooltip");
  await expect(card).toBeVisible();
  return card;
}

// Off the pull-request lane: `main`'s source row in `ask-part-references.e2e.ts` runs there, and
// the lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this
// copy.
test("a reference to an ask's option in a comment is a link that opens the ask at that part", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ask part references" });
  const ask = await createAsk(
    issue.key,
    {
      options: [
        { description: "Ship at the start of the week.", label: "Monday" },
        { description: "Ship once the review lands.", label: "Tuesday" },
      ],
      question:
        "The release train ships the reference graph. Monday ships it before the week's reviews " +
        "and holds nothing; Tuesday waits on the review that lands Monday evening. Which day " +
        "does it ship on? Recommendation: Monday, so a reader has the graph for the reviews.",
    },
    agent
  );
  // A write's own answer carries no part id: the ask's own read is what names the part each of
  // its options is stored as.
  const read = await getAsk(ask.id);
  const option = (read.ask.options ?? [])[1];
  expect(
    option?.id,
    "the ask's read names the part each of its options is stored as"
  ).toBeDefined();
  const part = option?.id ?? "";
  // A part's text is what the server stores for it: an option is its label, an em dash and its
  // description.
  const optionText = "Tuesday — Ship once the review lands.";
  const reference = `dispatch://${issue.key}/ask/${ask.id}#b-${part}`;
  const comment = await createComment(issue.key, { body: `We take ${reference}.` }, agent);

  // An agent wrote the comment, in a thread no person wrote in, so it is a turn of the history,
  // in Details, which `/activity` opens (dispatch://INFRA-1196 R15).
  await page.goto(`/issues/${issue.key}/activity`);
  const turns = historyItem(page, comment.id);
  // The link's text is the part's own text, read from the graph, not the reference it was
  // written as.
  const link = turns.getByRole("link", { name: optionText });
  await expect(link).toBeVisible();
  await expect(link).toHaveAttribute("data-dispatch-ref", reference);
  await expect(link).toHaveAttribute(
    "href",
    `/issues/${issue.key}/asks/${ask.id}#b-${encodeURIComponent(part)}`
  );

  // The card the link opens heads with the ask the part belongs to, where a block of a document
  // heads with its document, and shows the part's text.
  const card = await openCard(page, link);
  await expect(card).toContainText(`${issue.key} · Ask`);
  await expect(card).toContainText(optionText);
  await page.keyboard.press("Escape");
  await expect(page.getByRole("tooltip")).toHaveCount(0);

  // Following it opens the ask at that part: the ask's node of the outline takes the focus, on
  // screen (R20).
  await link.click();
  await expect(page).toHaveURL(`/issues/${issue.key}/asks/${ask.id}#b-${encodeURIComponent(part)}`);
  await expect.poll(() => focusedNodeId(page)).toBe(ask.id);
  const turn = nodeRow(page, ask.id).first();
  await expect(turn).toBeVisible();
  await expect(turn).toBeInViewport();
  // The ask shows an option as its label and its description, which the part's stored text joins
  // with an em dash: the part the link named is the option the reader lands on.
  await expect(turn).toContainText("Tuesday");
  await expect(turn).toContainText("Ship once the review lands.");
});

test("the backlink an ask's option writes names the ask as its source, not an artifact", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Target of an option's link" });
  const source = await createIssue({ project: "CORE", title: "Asking issue" });
  const ask = await createAsk(
    source.key,
    {
      options: [
        { description: "Ship the reference graph now.", label: "Ship" },
        { description: `Hold until dispatch://${target.key} ships.`, label: "Wait" },
      ],
      question:
        "The reference graph ships this week, and the issue it depends on has not landed. Ship " +
        "now and backfill, or wait for it? Recommendation: ship now, so the graph is readable " +
        "while the reviews run.",
    },
    agent
  );
  // The option's stored text, which the panel shows as the row's excerpt: its label, an em dash
  // and its description, drawn by the renderer, so the address in it reads as the issue's name.
  const optionText = `Wait — Hold until ${target.key} ${target.title} ships.`;

  await page.goto(`/issues/${target.key}`);
  // The backlink panel is the References section of the issue page's one panel, Details, where
  // what references the issue is one of its properties (dispatch://INFRA-1196 R12, R14).
  await openDetails(page);
  const panel = page.locator('[data-margin-section="references"]');
  await panel.getByRole("button", { name: /^Referenced by/ }).click();
  const references = panel.getByRole("region", { name: "Referenced by" });

  // The source is the ask the part belongs to, as a block of a comment reads as its comment, and
  // it links to the ask at that part. A part of an ask is no document, so nothing here reads
  // "Artifact", the label a block of a document carries - linked or as the plain text a build
  // that cannot place the part's reference writes.
  const askSource = references.getByRole("link", { name: `Ask · ${source.key}` });
  await expect(askSource).toBeVisible();
  await expect(references.getByText(/^Artifact · /)).toHaveCount(0);
  await expect(references.locator(".dispatch-markdown")).toHaveText(optionText);
  // One row, not two: an ask's own links are its question's, so the option's citation is the
  // part's edge alone.
  await expect(askSource).toHaveCount(1);

  // It opens the ask at the part the option is stored as, which the ask's own read names.
  const read = await getAsk(ask.id);
  const option = (read.ask.options ?? [])[1];
  expect(
    option?.id,
    "the ask's read names the part each of its options is stored as"
  ).toBeDefined();
  await expect(askSource).toHaveAttribute(
    "href",
    `/issues/${source.key}/asks/${ask.id}#b-${encodeURIComponent(option?.id ?? "")}`
  );
});

// ---- from 750e2716:packages/dispatch/e2e/address-links.e2e.ts

const entry = fixture("bare-addresses-each-with-its-own-host");

/** The links the engine reads in the entry's nine address paragraphs, one list per paragraph, in
 *  the order of the paragraphs after the corpus file's opening sentence. Taken from the entry's
 *  `pm_json` link marks, which `pmdoc/gen/differential.ts` recorded from the engine; a host per
 *  paragraph keeps every link name unique on the page, since a reader tells two links apart by
 *  their accessible names. The engine's two readers end the last two addresses differently: its
 *  tokenizer drops a trailing `_` after `;`, its transform keeps it after an escaped colon. */
const links: { href: string; name: string }[][] = [
  [{ href: "http://lower.test", name: "http://lower.test" }],
  [{ href: "https://semi.test", name: "https://semi.test" }],
  [{ href: "https://hash.test", name: "https://hash.test" }],
  [{ href: "https://amp.test", name: "https://amp.test" }],
  [{ href: "https://star.test", name: "https://star.test" }],
  [
    { href: "http://www.a.test/?u=", name: "www.a.test/?u=" },
    { href: "https://b.test", name: "https://b.test" },
  ],
  [{ href: "https://quote.test", name: "https://quote.test" }],
  [{ href: "https://a.test/a", name: "https://a.test/a" }],
  [{ href: "http://b.test/a_", name: "http://b.test/a_" }],
];

test.describe("a bare web address is a link in a document written through the API, as in a comment", () => {
  // SLOW(>=2s): 3,811 ms in this head's four-worker CI report (`dispatch-e2e-report-1` of run
  // 42334783358), 2,545 ms at the merge base's merge-group run. Nine paragraphs, each a bare
  // address, asserted twice - once in the comment's row of the outline and once in the document
  // editor - which is ten link lookups per surface.
  // Off the pull-request lane: `main`'s source row in `address-links.e2e.ts` runs there, and the
  // lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
  test("after punctuation or an escaped scheme colon", async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: entry.markdown,
      title: "Address links",
    });
    const comment = await createComment(issue.key, { body: entry.markdown });

    const context = await asReader(browser, "alice");
    try {
      const page = await context.newPage();
      // The corpus file's opening sentence holds no address; the nine paragraphs after it are the
      // rows, each paired with its own links.
      const rows = links.map((row, index) => ({
        links: row,
        text: entry.reading.paragraphs[index + 1].text,
      }));

      /** Asserts `root` shows every row's text and the name and href of every link in it. */
      const expectRows = async (root: Locator): Promise<void> => {
        for (const row of rows) {
          await expect(root).toContainText(row.text);
          for (const link of row.links) {
            await expect(root.getByRole("link", { name: link.name, exact: true })).toHaveAttribute(
              "href",
              link.href
            );
          }
        }
      };

      // A person's comment is a node of the outline, drawn with its words in full
      // (dispatch://INFRA-1196 R1, ruling 1).
      await page.goto(`/issues/${issue.key}/conversation`);
      await expectRows(nodeRow(page, comment.id).first());

      // Each row's text is asserted before its links, so a row Go read as text fails as "text
      // present, no link".
      await openSpecDocument(page, issue.key, rows[0].text);
      await expectRows(documentEditor(page));

      expect((await getArtifactText(issue.primary_artifact_id)).markdown).not.toContain("\\:");
    } finally {
      await context.close();
    }
  });
});

// ---- from 750e2716:packages/dispatch/e2e/download-names.e2e.ts

/** A file to upload, in the shape Playwright's multipart writes and file inputs take. */
interface UploadFile {
  buffer: Buffer;
  mimeType: string;
  name: string;
}

/** A file the server stores as a file and not a picture: `%PDF-` is no picture's signature. */
function pdf(name: string, text: string): UploadFile {
  return { buffer: Buffer.from(`%PDF-1.4\n% ${text}\n`), mimeType: "application/pdf", name };
}

/** Uploads `file` as the signed-in page through `path`, an owner's artifacts route, as a multipart
 *  write. `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
 *  cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin). */
async function upload(
  page: Page,
  path: string,
  file: UploadFile
): Promise<ArtifactUploadResponse["artifact"]> {
  const answer = await page.request.post(path, {
    headers: { Origin: dashboardOrigin },
    multipart: { file, name: file.name },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  return ((await answer.json()) as ArtifactUploadResponse).artifact;
}

/** The download `link` starts when it is clicked: the name the browser saves it under, the
 *  address it came from and, once it has finished, the file it wrote. */
async function saveFrom(page: Page, link: Locator) {
  const saved = page.waitForEvent("download");
  await link.click();
  return saved;
}

/** The path a link or a picture names for version `number`: the download address with
 *  `download`, the version address a picture is drawn from without it. */
function versionAddress(id: string, number: number, download = false): string {
  return `/api/v1/artifacts/${id}/versions/${number}${download ? "?download=true" : ""}`;
}

/** Opens the issue's Documents' list by a load of its own address: the list took the Artifacts
 *  tab's list (part N's R2), one row per file with the download of its latest version, and the
 *  file's versions and picture are on its own page, which the row's link opens. */
async function loadDocumentsList(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/documents`);
  await expect(documentsList(page), "Documents' list did not open").toBeVisible();
}

// SLOW(>=2s): a cold load of the issue's Documents' list, then the artifact's page and its whole
// version list, each link and picture read where it is drawn.
// @webkit @firefox: the address the engine follows for a download and the one it draws a picture
// from, which the rows below read the saved names of.
test("each Download version link names the download address and each picture the version address @webkit @firefox", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Download addresses" });
  const picture = {
    buffer: Buffer.from(ONE_PIXEL_PNG, "base64"),
    mimeType: "image/png",
    name: "diagram.png",
  };
  const diagram = await upload(page, `/api/v1/issues/${issue.key}/artifacts`, picture);
  await upload(page, `/api/v1/issues/${issue.key}/artifacts`, picture);

  await loadDocumentsList(page, issue.key);
  // The row draws no picture: the file's picture is on its own page, read below.
  const item = documentRow(page, "diagram.png");
  await expect(item.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "href",
    versionAddress(diagram.id, 2, true)
  );

  await documentLink(page, "diagram.png").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  const shown = page.getByRole("region", { exact: true, name: "diagram.png version 2" });
  await expect(shown.getByRole("img", { name: "diagram.png version 2" })).toHaveAttribute(
    "src",
    versionAddress(diagram.id, 2)
  );
  await expect(shown.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "href",
    versionAddress(diagram.id, 2, true)
  );
  // Every version is drawn open, with no control that hides any (part N's R2).
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  for (const number of [1, 2]) {
    await expect(
      versions.getByRole("link", { name: `Download version ${number}` })
    ).toHaveAttribute("href", versionAddress(diagram.id, number, true));
  }
});

// SLOW(>=2s): two uploads through Documents' list, each written and listed, and four downloads
// the browser saves from the list, the artifact's page and its version list.
// @webkit @firefox: the name the engine saves from `filename*` and `filename`, and whether it asks
// the server again for a download address it already followed: version 1's address saves a new
// name once version 2 exists.
test("a file saves under its own name, and its older version under its number once a newer one exists @webkit @firefox", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Report downloads" });
  await loadDocumentsList(page, issue.key);
  const item = documentRow(page, "report.pdf");

  await uploadInDocuments(page, pdf("report.pdf", "first draft"));
  const first = await saveFrom(page, item.getByRole("link", { name: "Download version 1" }));
  expect(first.suggestedFilename()).toBe("report.pdf");

  await uploadInDocuments(page, pdf("report.pdf", "second draft"));
  await expect(item.getByRole("link", { name: "Download version 2" })).toBeVisible();
  await documentLink(page, "report.pdf").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/report-pdf`);
  const shown = page.getByRole("region", { exact: true, name: "report.pdf version 2" });
  const current = await saveFrom(page, shown.getByRole("link", { name: "Download version 2" }));
  expect(current.suggestedFilename()).toBe("report.pdf");

  // Every version is drawn open, with no control that hides any (part N's R2).
  const versions = page.getByRole("region", { exact: true, name: "Versions for report.pdf" });
  const older = await saveFrom(page, versions.getByRole("link", { name: "Download version 1" }));
  expect(older.url()).toBe(first.url());
  expect(older.suggestedFilename()).toBe("report-v1.pdf");
  const newest = await saveFrom(page, versions.getByRole("link", { name: "Download version 2" }));
  expect(newest.suggestedFilename()).toBe("report.pdf");
});

// SLOW(>=2s): a cold load of the issue's Documents' list, an upload through it, and the download
// the browser saves.
// @webkit @firefox: the name the engine saves from `filename*` where the name holds spaces and a
// character outside ASCII, which the ASCII `filename` beside it cannot carry, and WebKit's own
// rule of writing each space of the name it read as `_`.
test("a file whose name holds spaces and a character outside ASCII saves under that name @webkit @firefox", async ({
  page,
  browserName,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Accented downloads" });
  await loadDocumentsList(page, issue.key);

  const name = "Q3 final résumé.pdf";
  await uploadInDocuments(page, pdf(name, "the quarter"));
  const saved = await saveFrom(
    page,
    documentRow(page, name).getByRole("link", { name: "Download version 1" })
  );
  // WebKit reads `filename*`, since the `é` stays where the ASCII `filename` has `_`, and then
  // writes each space as `_` (CI run 43750246861). Chromium and Firefox save the name exactly.
  const expected = browserName === "webkit" ? "Q3_final_résumé.pdf" : name;
  expect(saved.suggestedFilename()).toBe(expected);
});

// SLOW(>=2s): a cold load of the issue's Documents' list, and a download the browser saves and
// the row reads back from disk.
// @webkit @firefox: the name the engine saves a document's Markdown under, and the bytes it writes.
test("the spec saves as spec.md holding its Markdown @webkit @firefox", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Launch plan\n\nEvery download keeps its own name.\n",
    title: "Spec download",
  });
  const specName = (await getArtifact(issue.primary_artifact_id)).name;
  await loadDocumentsList(page, issue.key);

  const item = documentRow(page, specName);
  const saved = await saveFrom(page, item.getByRole("link", { name: "Download version 1" }));
  expect(saved.suggestedFilename()).toBe("spec.md");
  const spec = await getArtifactVersion(issue.primary_artifact_id, 1);
  expect(spec.markdown).toContain("Every download keeps its own name.");
  expect(await readFile(await saved.path(), "utf8")).toBe(spec.markdown);
});

// ---- from 750e2716:packages/dispatch/e2e/prefix-links.e2e.ts

/** How many characters of an id an address writes short here: the fewest the server resolves. */
const SHORT = 8;

function short(id: string): string {
  return id.slice(0, SHORT);
}

/** The id of the last filler paragraph of the host's spec, which stands below the fold. */
async function blockBelowTheFold(world: ReferenceWorld): Promise<string> {
  const { markdown } = await getArtifactText(world.host.primary_artifact_id);
  const blocks = await getArtifactBlocks(world.host.primary_artifact_id);
  const filler = blocks.filter((block) =>
    markdown.slice(block.from, block.to).startsWith("Filler paragraph ")
  );
  const last = filler.at(-1);
  if (last === undefined) {
    throw new Error("setup: the host's spec holds no filler paragraph");
  }
  return last.id;
}

// A `/spec` address naming a comment by `?comment=` opens the issue page on that comment, not the
// spec it quotes (dispatch://INFRA-1196 R20); written short, the id is resolved first and the
// address replaced by the whole one. The comment is on the spec's text, which part M's history in
// Details holds no turn of: the page draws it as a node, and the address focuses that node, on
// screen, with its text (part N's R6), as today's page opened its item in the margin. Threads
// lands a comment on text once dispatch://INFRA-5704 is on `main`.
test("a spec address that writes an anchored comment's id short opens it and is replaced by the whole id", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const anchored = await createComment(
    world.host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: "Anchored on the first line." },
    referenceAgent
  );
  expect(anchored.anchor?.quote).toBe(ANCHOR);

  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    const address = `/issues/${world.host.key}/spec?comment=`;
    await page.goto(`${address}${short(anchored.id)}`);
    await expect(page).toHaveURL(`${address}${anchored.id}`);
    await expect
      .poll(() => focusedNodeId(page), { message: "the address does not focus the comment" })
      .toBe(anchored.id);
    const turn = nodeRow(page, anchored.id).first();
    await expect(turn).toBeInViewport();
    await expect(turn).toContainText("Anchored on the first line.");
  } finally {
    await context.close();
  }
});

test("a document address whose block fragment writes the block's id short opens the document at that block", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const block = await blockBelowTheFold(world);

  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    const address = `${await specAddress(world.host.key)}#b-`;
    await page.goto(`${address}${short(block)}`);
    await expectDocumentConnected(page);
    await expect(documentEditor(page).locator(`[data-block-id="${block}"]`)).toBeInViewport();
    await expect(page).toHaveURL(`${address}${block}`);
  } finally {
    await context.close();
  }
});

test("a comment that writes an ask's id short reads as the ask's title, opens it, and follows an edit of the question with no reload", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const ask = world.items.ask;
  const reference = `dispatch://${world.host.key}/ask/${short(ask.id)}`;
  const comment = await createComment(
    world.host.key,
    { body: `Settled in ${reference} already.` },
    referenceAgent
  );

  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    // An agent wrote this comment, in a thread no person wrote in, so it is a turn of the
    // history, in Details, which `/activity` opens (dispatch://INFRA-1196 R15), marked by part
    // M's `data-history-item`.
    await page.goto(`/issues/${world.host.key}/activity`);
    const link = historyItem(page, comment.id).locator(
      `a[data-dispatch-ref="${world.items.ask.reference}"]`
    );
    await expect(link).toHaveText("Which cache backs the rollout?");
    // A reload would drop this mark.
    await page.evaluate(() => {
      document.documentElement.dataset.e2eNoReload = "kept";
    });
    await editAsk(ask.id, { question: "Which cache backs the rollout now?" }, referenceAgent);
    await expect(link).toHaveText("Which cache backs the rollout now?");
    await expect(page.locator("html")).toHaveAttribute("data-e2e-no-reload", "kept");

    await link.click();
    await expect(page).toHaveURL(`/issues/${world.host.key}/asks/${ask.id}`);
    // The question is a node of the outline (dispatch://INFRA-1196 R1), its row the turn today's
    // page drew.
    await expect(nodeRow(page, ask.id).first()).toContainText("Which cache backs the rollout now?");
  } finally {
    await context.close();
  }
});

// ---- from 750e2716:packages/dispatch/e2e/doc.e2e.ts

test("round-trips through the filters leave one open document connection and the typed text", async ({
  browser,
}) => {
  await createProject({ key: "TABS", name: "Tabs" });
  const issue = await createIssue({
    project: "TABS",
    spec: "# Keep this document",
    title: "Mounted panels",
  });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, carol);
  }

  const alice = await asReader(browser, "alice");
  try {
    const page = await alice.newPage();
    // Open connections, not connections ever opened: a document opened in place of the outline
    // is unmounted when the reader leaves it for a filter (dispatch://INFRA-1196 R3), which
    // closes its socket, and coming back opens one again. What must never happen is a tab that
    // accumulates live rooms.
    const sockets = openDocumentSockets(page);
    await page.goto(await specAddress(issue.key));
    const editor = documentEditor(page);
    await typeAtEnd(page, "stay mounted");
    await expect(editor).toContainText("stay mounted");
    await expect.poll(sockets).toBe(1);

    // The conversation is the outline, at its own address, reached by a move inside the page as
    // the Conversation tab was (dispatch://INFRA-1196 R1, R20): a fresh load of the address
    // would open a second document connection by itself and prove nothing.
    await navigateInApp(page, `/issues/${issue.key}/conversation`);
    await expect(tree(page).getByText("Existing message 29")).toBeVisible();
    // The outline drawn again opens with the focus on its first row (`IssueTree.tsx`'s opening),
    // which brings that row on screen: the reader's scroll is made after it, as a reader's is.
    await expectOpeningLanded(page);

    // A round-trip through the filter row: the Spec filter on, then every filter off again with
    // Conversation (dispatch://INFRA-1196 R2), the outline drawn again at the end of it where the
    // reader left it (part N's R1). It is made at 390 px, where the view row stays at the top of
    // the screen as the page scrolls, so the reader presses it where they stand. At 1440 px the
    // view row scrolls away with the header, so a press there comes after the reader has scrolled
    // back up to it, and the place they leave the outline at is the one they pressed from. Below
    // 1280 px the five toggles are one control whose menu holds them (part D's
    // `IssuePresets.tsx`), so each press is that control's and then its item's, and the view on is
    // read from the control's name (`expectActivePreset`), where the source read each toggle's
    // `aria-pressed`.
    //
    // The reader leaves the outline's row for the page before they scroll, as the source's reader
    // left the focus on the Conversation tab: the focused row draws its body, which closes as the
    // focus goes to the view row's control, and the browser's scroll anchoring then moves the page
    // by that body's height to keep what the reader sees, so the place they left would no longer
    // be the number they scrolled to.
    const wide = page.viewportSize();
    await page.setViewportSize({ height: 844, width: 390 });
    await focusThePage(page);
    await page.evaluate(() => window.scrollTo(0, 500));
    const scrollPosition = await page.evaluate(() => window.scrollY);
    expect(scrollPosition).toBeGreaterThan(0);
    for (const preset of ["spec", "conversation"] as const) {
      await presetChooser(page).click();
      await openMenu(page)
        .getByRole("menuitemradio", { name: new RegExp(`^${PRESET_NAMES[preset]}( |$)`) })
        .click();
      await expectActiveControl(page, preset);
    }
    await expect(tree(page).getByText("Existing message 29")).toBeVisible();
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(scrollPosition);

    // The same round-trip by the key table's chords, at the row's own width: `t s` turns the
    // Spec filter on, and `t c` turns every filter off (part N's R1). The reader stands on a row
    // the page shows, as one who moves by keys does, and goes to it as the keys go, the page
    // bringing it on screen. The outline is drawn again at that place with that row focused
    // (`stage-landing.tsx`'s `ViewNodeKeeper`), and `t c`, which puts the focus on the outline's
    // one tab stop, leaves the row where it stands (`useIssueKeymap.ts`'s `showPart`).
    if (wide !== null) await page.setViewportSize(wide);
    const spec = page.getByRole("button", { exact: true, name: "Spec" });
    const conversation = page.getByRole("button", { exact: true, name: "Conversation" });
    const standing = tree(page).getByRole("treeitem").filter({ hasText: "Existing message 20" });
    await standing.focus();
    await expect(standing).toBeInViewport();
    const chordPosition = await page.evaluate(() => window.scrollY);
    expect(chordPosition).toBeGreaterThan(0);
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(spec).toHaveAttribute("aria-pressed", "true");
    await page.keyboard.press("t");
    await page.keyboard.press("c");
    await expect(conversation).toHaveAttribute("aria-pressed", "true");
    await expect(tree(page).getByText("Existing message 29")).toBeVisible();
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(chordPosition);

    // Back to the document, with the text typed into it and one connection open, not two.
    await navigateInApp(page, await specAddress(issue.key));
    await expect(editor).toContainText("stay mounted");
    await expect.poll(sockets).toBe(1);
  } finally {
    await alice.close();
  }
});

// Method names survive minification where class and import names do not: `getXmlFragment(`
// as a definition (not a `.getXmlFragment(` call) is Yjs's `Doc`, and `permissionDeniedHandler`
// is the Hocuspocus provider. Dispatch's own host code calls these; only the libraries define them.
const editorTransportMarkers = [/[^.\w$]getXmlFragment\(/u, /permissionDeniedHandler/u];

test("the conversation downloads no editor transport; a document loads it on demand", async ({
  browser,
}) => {
  await createProject({ key: "LAZY", name: "Lazy" });
  const issue = await createIssue({
    project: "LAZY",
    spec: "# Lazy\n\nUse SQLite\n",
    title: "Transport on demand",
  });
  await createMessage(issue.key, { body: "A message, no document" }, carol);

  const alice = await asReader(browser, "alice");
  try {
    const page = await alice.newPage();
    const scripts = new Set<string>();
    page.on("request", (request) => {
      if (/\/assets\/.*\.js(\?|$)/u.test(request.url())) {
        scripts.add(request.url());
      }
    });
    const transportChunks = async () => {
      const chunks: string[] = [];
      for (const url of scripts) {
        const body = await (await page.request.get(url)).text();
        if (editorTransportMarkers.some((marker) => marker.test(body))) {
          chunks.push(url);
        }
      }
      return chunks;
    };

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(tree(page).getByText("A message, no document")).toBeVisible();
    expect(scripts.size).toBeGreaterThan(1);
    expect(await transportChunks()).toEqual([]);

    await navigateInApp(page, await specAddress(issue.key));
    await expect(documentEditor(page)).toContainText("Use SQLite");
    expect((await transportChunks()).length).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

// c-view-waiting. Hold the view's real API requests before opening its address. The
// assertion of the status is the first reading of the view, not a wait for any row.
test("a view that waits for its reads says what it waits for", async ({ browser }) => {
  // Eight cold opens of a seeded issue, including four first-read failures with query retries.
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const issue = `/api/v1/issues/${seeded.key}`;
    const spec = `/api/v1/artifacts/${seeded.specArtifactId}/blocks`;
    const plan = `/api/v1/artifacts/${seeded.planArtifactId}/blocks`;
    const verification = `/api/v1/artifacts/${seeded.verificationArtifactId}/blocks`;
    const coverage = `${issue}/coverage`;
    const results = `${issue}/results`;
    const decisions = `${issue}/decisions`;
    const asks = `${issue}/asks`;
    const views = [
      {
        body: "[data-spec-body]",
        failed: STATE_LINES.statements_unreadable,
        loading:
          "Loading which checks serve each statement, the statements, the results, the verification document, the decisions and the questions…",
        reads: [coverage, spec, results, verification, decisions, asks],
        ready: "[data-spec-statement]",
        view: "spec",
      },
      {
        body: "[data-verification-body]",
        failed: STATE_LINES.coverage_unreadable,
        loading: "Loading which checks serve each statement and the checks' results…",
        reads: [coverage, results, verification],
        ready: "[data-verification-check]",
        view: "verification",
      },
      {
        body: "[data-plan-body]",
        failed: STATE_LINES.plan_unreadable,
        loading: "Loading the plan and the spec…",
        reads: [plan, spec],
        ready: "[data-plan-bar-row]",
        view: "plan",
      },
      {
        body: "[data-decisions-body]",
        failed: STATE_LINES.issue_decisions_unreadable,
        loading: "Loading the decisions and the questions…",
        reads: [decisions, asks],
        ready: '[data-decisions-part="open_questions"] [data-node-row]',
        view: "decisions",
      },
    ] as const;
    for (const { body, failed, loading, reads, ready, view } of views) {
      await test.step(PRESET_NAMES[view], async () => {
        const released = Promise.withResolvers<void>();
        const seen = new Set<string>();
        const matchReads = ({ pathname }: URL) => reads.some((read) => read === pathname);
        const hold = async (route: Route) => {
          seen.add(new URL(route.request().url()).pathname);
          await released.promise;
          await route.continue();
        };
        const slot = issuePage(page).locator(body);
        const status = slot.getByRole("status").and(slot.locator(VIEW_LOADING));
        await page.route(matchReads, hold);
        try {
          await openPreset(page, seeded.key, view);
          await expect(status, "the view was blank while its reads were out").toBeVisible({
            timeout: 1_000,
          });
          await expect(status).toHaveText(loading, { timeout: 1_000 });
          await expect(outlineRows(page)).toHaveCount(0);
          await expect
            .poll(() => reads.every((read) => seen.has(read)), {
              message: "the page did not ask for every held read",
            })
            .toBe(true);
          released.resolve();
          await expect(
            slot.locator(ready).first(),
            "the view did not draw after its reads answered"
          ).toBeVisible();
          await expect(status).toHaveCount(0);
        } finally {
          released.resolve();
          await page.unroute(matchReads, hold);
        }

        // A new page load has no cached answer, so this failure replaces the loading line.
        const refusedRead = ({ pathname }: URL) => pathname === reads[0];
        const refuse = (route: Route) =>
          route.fulfill({ json: { code: "SERVER_ERROR", error: "unreadable" }, status: 500 });
        await page.route(refusedRead, refuse);
        try {
          await openPreset(page, seeded.key, view);
          const line = slot.locator("[data-failed-read]").filter({ hasText: failed });
          await expect(line.getByRole("button", { exact: true, name: "Retry" })).toBeVisible();
          await expect(status).toHaveCount(0);
        } finally {
          await page.unroute(refusedRead, refuse);
        }
      });
    }
  });
});

/** The block of the spec `c-press-caret` presses, which the row's seed inserts. */
const PRESSED_BLOCK = "r-pressed";
/** The word of that block a press lands in, and how far into it. */
const PRESSED_WORD = "Placeholder";
const PRESSED_INTO = 4;
/** The words of a comment another person wrote, and the word of them a press lands in. */
const THEIR_WORDS = "Bob's comment on the issue, which no other reader edits.";
const THEIR_WORD = "comment";

/** The words of `PRESSED_BLOCK`: `PRESSED_WORD` first, then a link to the issue's document
 *  `slug`, which a press follows. */
function pressedWords(key: string, slug: string): string {
  return `${PRESSED_WORD} words come first in this statement, and dispatch://${key}/artifact/${slug} comes after them.`;
}

/** Brings the word `word` of `row`'s drawn words to the middle of the screen by a turn of the
 *  wheel, the reader's own act, and answers the point of the page `into` characters into it: the
 *  left edge of that character, plus a pixel, halfway down its line, where the browser reads a
 *  place before it. */
async function pointInWord(
  page: Page,
  row: Locator,
  word: string,
  into: number
): Promise<{ x: number; y: number }> {
  const wordPointNow = () =>
    row.evaluate(
      (element, { wanted, at }) => {
        const words = element.querySelector("[data-node-words]");
        if (words === null) return null;
        const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
        for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
          const found = ` ${text.textContent ?? ""} `.indexOf(` ${wanted} `);
          if (found < 0) continue;
          const range = document.createRange();
          range.setStart(text, found + at);
          range.setEnd(text, found + at + 1);
          const box = range.getBoundingClientRect();
          return { x: box.left + 1, y: (box.top + box.bottom) / 2 };
        }
        return null;
      },
      { at: into, wanted: word }
    );
  const far = await wordPointNow();
  expect(far, `the row draws no word "${word}"`).not.toBeNull();
  await wheelToMiddle(page, far?.y ?? 0);
  const at = await wordPointNow();
  expect(at, `the word "${word}" is not on the page after the wheel`).not.toBeNull();
  const point = at ?? { x: 0, y: 0 };
  expect(
    await wordAtCaret(page, point),
    `the point (${point.x}, ${point.y}) is not on the word "${word}"`
  ).toBe(word);
  return point;
}

/** Brings `link` to the middle of the screen by a turn of the wheel, and answers its middle,
 *  where a press lands on it. */
async function pointOnLink(page: Page, link: Locator): Promise<{ x: number; y: number }> {
  const linkMiddleNow = () =>
    link.evaluate((element) => {
      const box = element.getBoundingClientRect();
      return { x: box.left + box.width / 2, y: (box.top + box.bottom) / 2 };
    });
  await wheelToMiddle(page, (await linkMiddleNow()).y);
  const point = await linkMiddleNow();
  expect(
    await link.evaluate(
      (element, { x, y }) => element.contains(document.elementFromPoint(x, y)),
      point
    ),
    `the point (${point.x}, ${point.y}) is not on the link`
  ).toBe(true);
  return point;
}

/** Turns the wheel, the reader's own act, to bring what stands `y` px down the screen to its
 *  middle, and waits for the page to stop. */
async function wheelToMiddle(page: Page, y: number): Promise<void> {
  const size = page.viewportSize() ?? DESKTOP;
  await page.mouse.move(size.width / 2, size.height / 2);
  await page.mouse.wheel(0, y - size.height / 2);
  await settledScrollTop(page);
}

// c-press-caret (R18, decision 420 (1)): a press on the words of an outline row that takes a caret
// opens its editor with the caret where the press landed, as Enter opens it at the block's start,
// and a letter typed then lands there; a press on a row that takes no caret - a comment another
// person wrote - moves the focus to it and opens no editor; and a press on a link in a row's words
// follows the link. Read on Conversation's outline at 1440 px and at 390 px, where the phone's
// press is a tap. Before R18 only Enter opened the editor, while the pointer's text cursor
// (decision 323) said the words took typing.
// SLOW(>=2s): the whole issue seeded with a statement inserted in its spec and a second reader's
// comment, and the conversation loaded at each width for a press, a letter typed and taken out
// again, a press on the comment and a link followed.
test("a press on a row puts the caret where it lands @phone", async ({ browser }) => {
  test.setTimeout(180_000);
  const seed = async () => {
    const seeded = await seedWholeIssue();
    const theirs = await createComment(seeded.key, { body: THEIR_WORDS }, { login: "bob" });
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(3)}`,
          markdown: `:::requirement{#${PRESSED_BLOCK}}\n${pressedWords(seeded.key, seeded.documents.cited)}\n:::`,
          op: "insert",
        },
      ],
    });
    return { ...seeded, theirs: theirs.id };
  };
  await phase(browser, seed, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = nodeRow(page, blockNodeId(specSlug, PRESSED_BLOCK)).first();
    const comment = nodeRow(page, seeded.theirs).first();
    // The phone's project presses by touch; the desktop's reads both widths with the mouse.
    const touch = test.info().project.name === "iphone";
    const press = (at: { x: number; y: number }) =>
      touch ? page.touchscreen.tap(at.x, at.y) : page.mouse.click(at.x, at.y);
    for (const width of touch ? [IPHONE_13.width] : [DESKTOP.width, IPHONE_13.width]) {
      await test.step(`${width} px`, async () => {
        await page.setViewportSize(viewportAt(width));
        const traffic = watchApiRequests(page);
        await gotoIssue(page, `/issues/${seeded.key}/conversation`);
        await expectOpeningLanded(page);
        await expectLoadSettled(traffic);
        await expect(statement, "the statement's row takes no typing").toHaveAttribute(
          "data-editable",
          ""
        );

        // A press in the middle of a word opens the editor with the caret there, the row where
        // the reader pressed it and the word under the pointer, and a letter typed lands there.
        // The row's place on the screen is what is read, not the page's scroll: rows above it
        // change as the press moves the focus, and Chromium's scroll anchoring moves the scroll to
        // keep the screen still. Measured at 1440 px: the scroll 50 px up, the row where it stood.
        const at = await pointInWord(page, statement, PRESSED_WORD, PRESSED_INTO);
        const pressedRowTop = () =>
          statement.evaluate((row) => Math.round(row.getBoundingClientRect().top));
        const topAt = await pressedRowTop();
        await press(at);
        await expect
          .poll(() => caretInRowBlock(page, PRESSED_BLOCK), {
            message: "a press on the text opened no editor",
          })
          .toBe(true);
        expect(
          await wordAtCaret(page, "selection"),
          `a press on "${PRESSED_WORD}" put the caret in another word`
        ).toBe(PRESSED_WORD);
        await settledScrollTop(page);
        const moved = (await pressedRowTop()) - topAt;
        expect(
          Math.abs(moved),
          `the press moved the row ${moved} px on the screen`
        ).toBeLessThanOrEqual(1);
        const caretY = await page.evaluate(() => {
          const caret = window.getSelection()?.getRangeAt(0).getBoundingClientRect();
          return caret === undefined ? null : (caret.top + caret.bottom) / 2;
        });
        expect(
          Math.abs((caretY ?? Number.POSITIVE_INFINITY) - at.y),
          `the caret's line stands at ${caretY} px, the press at ${at.y} px`
        ).toBeLessThanOrEqual(12);
        const block = statement.locator(
          `[contenteditable="true"] [data-block-id="${PRESSED_BLOCK}"]`
        );
        await page.keyboard.type("Q");
        await expect(
          block,
          "the letter typed after the press did not land at the press"
        ).toContainText(
          `${PRESSED_WORD.slice(0, PRESSED_INTO)}Q${PRESSED_WORD.slice(PRESSED_INTO)} words`
        );
        await page.keyboard.press("Backspace");
        await expect(block).toContainText(`${PRESSED_WORD} words`);
        await page.keyboard.press("Escape");
        await expect
          .poll(() => caretInNode(page), { message: "Escape left the caret in" })
          .toBe(false);

        // A press on a comment another person wrote moves the focus to its row alone.
        await expect(comment, "another person's comment takes typing").not.toHaveAttribute(
          "data-editable"
        );
        await press(await pointInWord(page, comment, THEIR_WORD, 2));
        await expect
          .poll(() => focusedNodeId(page), { message: "a press on the comment left the focus" })
          .toBe(seeded.theirs);
        await settledScrollTop(page);
        expect(await caretInNode(page), "a press on the comment opened an editor").toBe(false);
        await expect(
          comment.locator("[data-row-report]"),
          "the comment's row answered the press"
        ).toHaveCount(0);

        // A press on a link in the statement's words follows it, and opens no editor.
        const link = statement.locator("[data-node-words] a[href]").first();
        await expect(link, "the statement's words draw no link").toBeVisible();
        const href = await link.getAttribute("href");
        await press(await pointOnLink(page, link));
        await expect
          .poll(() => new URL(page.url()).pathname, {
            message: "a press on the link did not follow it",
          })
          .toBe(new URL(href ?? "", page.url()).pathname);
        expect(await caretInNode(page), "a press on the link opened an editor").toBe(false);
      });
    }
  });
});

/** The repository the pull requests of `c-pull-request-link` are written in. */
const PULL_REQUEST_REPOSITORY = "acme/widgets";

/** One `pull_request_heads` row attributed to `issue`, written as the `pull_request` envelope
 *  would have stored it (`e2e/verification.e2e.ts`'s `seedCICheck`), with the address and the
 *  time of the event it reported set by the row. */
async function writePullRequestHead(row: {
  readonly event: string;
  readonly issue: string;
  readonly number: number;
  readonly state: "closed" | "merged" | "open";
  readonly url: string;
}): Promise<void> {
  const sha = String(row.number).padStart(40, "0");
  await sql(
    `INSERT INTO pull_request_heads (repo, number, head_sha, state, issue_key, attribution_source, title, url, event_at, checked_at)
     VALUES ('${PULL_REQUEST_REPOSITORY}', ${row.number}, '${sha}', '${row.state}', '${row.issue}', 'title_body', '${row.issue} the header', '${row.url}', '${row.event}', now())`
  );
}

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
 *  approval request waiting on the reader or with none: part P's own, 9 lines and 280 px
 *  (decision 17 of dispatch://INFRA-1196/artifact/part-decisions-md, `headerLimit` of
 *  `e2e/issue-page-p.ts`). Root R13's 6 lines and 180 px hold for a title of at most two lines:
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
// gives it. An issue with none carries no `pull_request` and draws no link. Then the header with
// the 115-character title and 20 labels is read without the link and with it, twice: with only
// the two questions waiting on the reader, where the line of the count and the labels has the
// least room left, and with an approval request waiting too, the worst header. Each is at 1440 px
// inside R13's sizes and at 390 px inside `WORST_HEADER_AT_390`, and the link takes no line and
// no pixel of the header the issue draws without it. A pass does not show a state change while
// the page stays open (the field comes with the issue's read).
// SLOW(>=2s): 14.1 s on chromium and 8.2 s on iphone (one worker, the pod, at g12b): a project and
// the header's seed, four pull request rows written and deleted, a title, 20 labels and an
// approval request written, and six loads of the issue at each of the row's widths, each read
// for its header's size.
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
    for (const width of widths) {
      await loadAt(width);
      await expect(link(), "an issue with no pull request draws a link").toHaveCount(0);
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
    await expectOpenLinked(seededIssue, (reading, width) =>
      expectHeaderWithinItsLimit(reading, width, seededIssue, false)
    );

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
// window at each of two widths, each page scrolled to five places with two frames drawn after each.
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
        // The scroll's handler and the render it asked for are done within two frames.
        await nextFrames(page);
        const reading = await stickyReading(page);
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

/** Waits until the focus is on a row other than `from` (none, the first time), and Details' "The row
 *  you are on" names that row's node and its kind, and answers the row's node. A key that moved
 *  nothing, or a move Details did not follow, fails with `where`. */
async function expectDetailsFollowsRow(
  page: Page,
  from: string | null,
  where: string
): Promise<string> {
  const seen: { row: string | null } = { row: null };
  await expect
    .poll(
      async () => {
        seen.row = await focusedNodeId(page);
        const named = await focusedNodeFacts(page).getAttribute("data-focused-node");
        if (seen.row === null) return "the focus is on no row";
        if (seen.row === from) return `the focus did not leave ${from}`;
        return seen.row === named
          ? "follows"
          : `the focus is on ${seen.row} and Details names ${named}`;
      },
      { message: where }
    )
    .toBe("follows");
  const at = seen.row ?? "";
  // The Verification view's first row is the issue's own summary, which carries no kind of node:
  // the focused row is then the issue itself, whose facts name its title after "Issue:".
  const type = await page
    .locator(`[data-node-row][data-node-id="${at}"]`)
    .first()
    .getAttribute("data-node-type");
  const kind = NODE_TYPE_NAMES[(type ?? "issue") as keyof typeof NODE_TYPE_NAMES];
  expect(
    kind,
    `${where}: the row of ${at} is of kind ${type}, which the page has no word for`
  ).toBeDefined();
  const named = `${CONTROL_LABELS.node_kind}:\\s*${sentenceCase(kind ?? "")}`;
  await expect(
    focusedNodeFacts(page),
    `${where}: Details does not name the kind of ${at}`
  ).toContainText(new RegExp(`(?:${named}|${sentenceCase(NODE_TYPE_NAMES.issue)}:)`, "u"));
  return at;
}

// R14 (INV-4, `VIEWS_FOCUS`): under every view the row that holds the focus is part F's focused
// node, as the outline's row is, so Details' facts of 'The row you are on' follow `j` and `k`, and
// Escape or `t c` leaves that node focused in the outline. Details draws a node's kind and
// whether it stands, and the issue's own title: no first line of a node, so the node and its kind
// are what the row reads.
test("Details follows the focused row of every view", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    for (const preset of VIEWS_FOCUS) {
      for (const leave of ["Escape", "t c"] as const) {
        const view = `the ${PRESET_NAMES[preset]} view, left by ${leave}`;
        await gotoIssue(page, presetPath(seeded.key, preset));
        await expectActiveControl(page, preset);
        await expect
          .poll(() => nodeRows(page).count(), { message: `${view} draws too few rows` })
          .toBeGreaterThan(3);
        await openDetails(page);
        await page.locator('[data-node-row][tabindex="0"]').first().focus();
        let at = await expectDetailsFollowsRow(page, null, `${view}, on its first row`);
        for (const key of ["j", "j", "k"]) {
          await page.keyboard.press(key);
          at = await expectDetailsFollowsRow(page, at, `${view}, after ${key}`);
        }
        if (leave === "Escape") {
          // Details, open beside the view, takes the first Escape as it takes its own; the view
          // is turned off by the next.
          await page.keyboard.press("Escape");
          await expect(marginShell(page), `Escape from ${view} leaves Details open`).toBeHidden();
          await page.keyboard.press("Escape");
        } else {
          await page.keyboard.press("t");
          await page.keyboard.press("c");
        }
        await expect(
          page.locator(`[data-issue-body] [data-node-row][data-node-id="${at}"]`).first(),
          `${leave} from ${view} leaves no row of ${at} focused in the outline`
        ).toBeFocused();
      }
    }
  });
});

// R16 (decision 259): a Rebuild that succeeds draws the rebuilt document, takes the Rebuild offer
// away, and puts the focus on the opened document's heading, never on the page's body. The
// document's stored history cannot load for the page until its rebuild lands: its text read
// answers the unloadable refusal and its rebuild answers as a rebuilt one would, since the
// server refuses to rebuild a document that loads.
test("a Rebuild by keyboard keeps the focus in the opened document", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const issue = await getIssue(seeded.key);
    const notes = (issue.artifacts ?? []).find((one) => one.slug === seeded.documents.cited);
    if (notes === undefined) throw new Error(`${seeded.key} holds no ${seeded.documents.cited}`);
    const unloadable = {
      code: "DOCUMENT_UNLOADABLE",
      error:
        "the document's stored history cannot load. A rebuild from the latest saved version discards every edit made after that version. A person decides whether to rebuild the document",
    };
    const history = { loads: false };
    await page.route(`**/api/v1/artifacts/${notes.id}/text*`, (route) =>
      history.loads
        ? route.continue()
        : route.fulfill({
            body: JSON.stringify(unloadable),
            contentType: "application/json",
            status: 409,
          })
    );
    await page.route(`**/api/v1/artifacts/${notes.id}/rebuild`, (route) => {
      history.loads = true;
      return route.fulfill({
        body: JSON.stringify({
          head: 1,
          removed_checkpoints: 0,
          removed_snapshots: 0,
          source_version: 1,
        }),
        contentType: "application/json",
        status: 200,
      });
    });
    const rebuild = page.getByRole("button", { name: "Rebuild from the latest version" });
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      history.loads = false;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, presetPath(seeded.key, "documents"));
      await expect(
        documentLink(page, notes.name),
        `Documents' list draws no ${notes.name}`
      ).toBeVisible();
      await documentLink(page, notes.name).click();
      await expect(rebuild, `no Rebuild is offered ${where}`).toBeVisible();
      // The rebuild asks the reader to confirm, and the reader does.
      page.once("dialog", (dialog) => dialog.accept());
      await rebuild.focus();
      await page.keyboard.press("Enter");
      await expect(rebuild, `the Rebuild offer stays ${where}`).toHaveCount(0);
      await expect(openedEditor(page), `the rebuilt document is not drawn ${where}`).toContainText(
        "The measurements behind the first requirement."
      );
      await expect(
        openedArtifact(page).getByRole("heading", { exact: true, level: 2, name: notes.name }),
        `the focus is not on the opened document's heading ${where}`
      ).toBeFocused();
      await expect(
        page.locator("body"),
        `the focus is on the page's body ${where}`
      ).not.toBeFocused();
    }
  });
});

// Decision 396 (3): from an act of a focused row, Copy reference, Ctrl+K and then Escape give the
// focus back to the row that held the act, which went with the focus when the palette took it.
test("a dialog opened from a row's act gives the focus back to the row", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    const [thread = ""] = seeded.comments.threads;
    const row = await focusRow(page, thread);
    const copy = row.locator("[data-row-copy]");
    await expect(copy, "the focused row draws no Copy reference").toBeVisible();
    await copy.focus();
    await page.keyboard.press("ControlOrMeta+k");
    await expect(page.getByRole("dialog"), "Ctrl+K opens no dialog").toBeVisible();
    await expect(copy, "the act stayed drawn once the row lost the focus").toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog"), "Escape leaves the dialog open").toHaveCount(0);
    await expect(
      row,
      "the dialog's close left the focus off the row that held the act"
    ).toBeFocused();
  });
});
