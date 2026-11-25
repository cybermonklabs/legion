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
import type { ArtifactUploadResponse } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { shortDay } from "../web/src/features/issue-page/node-time";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_PHRASES,
  CONTROL_LABELS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  FILE_WORDS,
  GAP_LINES,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  PRESET_ORDER,
  STATE_LINES,
  UNASKED_SOURCES,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { type FakeSession, getUnsubscribeCalls } from "./agents";
import {
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
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
import {
  type BaselineSeed,
  seedNoChildren,
  seedRequestBaseline,
  seedTwiceTheNodes,
  seedWholeIssue,
  seedWholeIssueAdvancing,
  type WholeIssueSeed,
} from "./issue-layout-seed";
import { asPlanner, PLANNER_TITLE, PROJECT } from "./issue-layout-seed-support";
import {
  OPEN_QUESTION_BLOCK_ID,
  planTaskBlockId,
  requirementBlockId,
  requirementText,
} from "./issue-layout-seed-text";
import {
  closeDetails,
  contentsPane,
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
  childRows,
  childrenSection,
  expectActivePreset,
  focusedNodeId,
  focusInside,
  nodeBodies,
  onScreen,
  openEveryDeeperBranch,
  openPreset,
  pickPreset,
  treeActiveDescendant,
} from "./issue-page-i";
import { drawnContentsEntries } from "./issue-page-j";
import { historyEntries, historyItem, historyPanel, stepsSection } from "./issue-page-m";
import {
  type AddressContent,
  type AddressSeed,
  AWAITING_THE_BOX,
  assertInventoryCarried,
  assertNoSidewaysScroll,
  chromeTexts,
  expectDocumentConnected,
  filterBox,
  filterControl,
  filterRowLooks,
  filterRowReading,
  INVENTORY_PARENT_ROWS,
  INVENTORY_RENAMES,
  type InventoryEntry,
  identityBlock,
  nodeMenuButton,
  nodeMenuItems,
  OLD_PAGE_ADDRESSES,
  offered,
  offThePalette,
  openedArtifact,
  openedEditor,
  openIssueActions,
  openIssueAddress,
  openPageState,
  openSpecDocument,
  openVersionControl,
  optionsOf,
  type ParentRowMapping,
  REFLOW_STATES,
  rowsWithoutAuthorScrollers,
  scrollersOutsideAuthorText,
  specAddress,
  stateName,
  undividedTexts,
  uploadInput,
  versionControl,
  versionControlButton,
  versionPicker,
  WORD_STATES,
  zoomCrumbs,
} from "./issue-page-n";
import { withIssuePage } from "./issue-page-option";
import { IPHONE_13, MEASURED_TITLE, viewportAt, waitingCount } from "./issue-page-p";
import { openIssue } from "./keyboard-issue";
import { connect, isLiveSocket, type LiveSockets, watchLiveSockets } from "./live-socket-watch";
import { sql } from "./psql";
import {
  ANCHOR,
  type ReferenceWorld,
  referenceAgent,
  seedReferenceWorld,
} from "./reference-surfaces";
import { navigateInApp, ONE_PIXEL_PNG } from "./sends";
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
import { expectLoadSettled, watchApiRequests } from "./traffic";
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
        await gotoIssue(page, `/issues/${seeded.key}`);
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

/** The id of the first row the outline draws that waits on the reader: an open question or a
 *  decision waiting for their review, in the order the rows stand. */
function firstWaitingRow(page: Page): Promise<string | null> {
  return page.evaluate(
    () =>
      [...document.querySelectorAll<HTMLElement>('[role="tree"] [data-node-row]')]
        .find(
          (row) =>
            (row.dataset.nodeType === "ask" && row.dataset.nodeState === "open") ||
            (row.dataset.nodeType === "decision" && row.dataset.nodeState === "waiting_review")
        )
        ?.getAttribute("data-node-id") ?? null
  );
}

// dispatch://INFRA-3678, under R13: the count of what waits on the reader is a control. Pressed,
// or Enter on it, from a document of an issue holding more than one item that waits, it returns
// to the outline, turns the Decisions filter on and puts the focus on the first item that waits
// in the filter's order; an approval request alone puts the focus on the header's approval
// control.
test("the count of what waits on the reader opens the first thing that waits @phone", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    for (const [width, key] of (desktopProject
      ? [
          [1440, "click"],
          [390, "Enter"],
        ]
      : [[390, "click"]]) as readonly (readonly [number, "click" | "Enter"])[]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      const settling = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.cited}`);
      await expectLoadSettled(settling);
      await expect(openedArtifact(page), "the document did not open").toBeVisible();
      await expect(waitingCount(page)).toHaveText(/^Waiting on you \((\d+)\)$/);
      // The count is drawn from the page's reads, which went quiet above, and the wait above saw
      // it drawn, so one read of its words is the whole count.
      const counted = Number(/\((\d+)\)/.exec((await waitingCount(page).textContent()) ?? "")?.[1]);
      expect(counted, "the issue holds fewer than two items that wait").toBeGreaterThanOrEqual(2);
      if (key === "click") await waitingCount(page).click();
      else {
        await waitingCount(page).focus();
        await page.keyboard.press("Enter");
      }
      await expect(openedArtifact(page), "the count left the document open").toHaveCount(0);
      await expectActivePreset(page, PRESET_NAMES.decisions);
      await expect
        .poll(async () => (await focusedNodeId(page)) ?? "", {
          message: `the focus is not on the first item that waits at ${width} px`,
        })
        .not.toBe("");
      expect(await focusedNodeId(page)).toBe(await firstWaitingRow(page));

      // Each press after the first goes on to the next item that waits, in the filter's order,
      // and round to the first again (the tester's F12): every item the count holds is reached
      // by pressing it, an open question among them, with no key a phone lacks.
      const first = (await focusedNodeId(page)) ?? "";
      const walked = [first];
      for (let press = 0; press < counted; press += 1) {
        const from = walked.at(-1) ?? "";
        if (key === "click") await waitingCount(page).click();
        else {
          await waitingCount(page).focus();
          await page.keyboard.press("Enter");
        }
        // The item the press landed on is the one the poll saw, so the walk reads no second
        // answer: an empty one is the focus on no row, which is no item it moved to.
        let next = "";
        await expect
          .poll(
            async () => {
              next = (await focusedNodeId(page)) ?? "";
              return next !== "" && next !== from;
            },
            {
              message: `press ${press + 2} of the count did not move the focus from ${from} to another item at ${width} px`,
            }
          )
          .toBe(true);
        if (next === first) break;
        walked.push(next);
      }
      expect(
        (await focusedNodeId(page)) ?? "",
        `the count's presses did not come round to the first item in ${counted} at ${width} px`
      ).toBe(first);
      expect(new Set(walked).size, "a press of the count went back to an item it had left").toBe(
        walked.length
      );
      expect(walked.length, "the count's presses reach one item only").toBeGreaterThanOrEqual(2);
      const kinds = await Promise.all(
        walked.map((id) => nodeRow(page, id).first().getAttribute("data-node-type"))
      );
      expect(kinds, `the count's presses reach no open question: ${walked.join(", ")}`).toContain(
        "ask"
      );
    }

    // An approval request is all that waits: the count puts the focus on the approval control.
    const lone = await createIssue({ force: true, project: PROJECT, spec: "Plan.", title: "Lone" });
    await requestApproval(lone.primary_artifact_id ?? "", { summary: "Read it." });
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${lone.key}`);
    await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    await waitingCount(page).click();
    await expect
      .poll(() =>
        page.evaluate(() => document.activeElement?.hasAttribute("data-approval-artifact"))
      )
      .toBe(true);
    expect(
      await page.evaluate(() => document.activeElement?.closest("[data-issue-header]") !== null),
      "the focus is on an approval control outside the header"
    ).toBe(true);
  });
});

// Ruling 5 of dispatch://INFRA-1196/artifact/build-rulings-md, "The filter row": one line at
// every width. From 761 px up the node menu, the filter by words, Keys and - with a filter on -
// the version control are small controls at its right end, each drawn as Keys is, and the words
// box opens from its control; at 760 px and below the row is the one control and Outline, and
// those three are in the Outline sheet, with a document open too. Each control and the open box
// is at least 24 px tall, the row is drawn in the page's colours alone, and `t a` and `t f` open
// the menu and the box with the focus in them (part N's R4).
test("the filter row is one line at every width @phone", async ({ browser }) => {
  // Measured in the issue pod, alone: 49.5 s on chromium, which reads six widths, and 22.6 s on
  // iphone, which reads one; the bound is three times the longer, for a slow leg.
  test.setTimeout(150_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    const small = [CONTROL_LABELS.other_nodes, CONTROL_LABELS.filter, CONTROL_LABELS.keys];
    for (const width of desktopProject ? [1440, 1279, 1025, 1024, 761, 390] : [390]) {
      for (const preset of ["conversation", "spec"] as const) {
        await page.setViewportSize(viewportAt(width));
        await gotoIssue(page, presetPath(seeded.key, preset));
        await expectIssuePageDrawn(page);
        await expectActivePreset(page, PRESET_NAMES[preset]);
        const at = `at ${width} px with ${preset === "spec" ? "the Spec filter" : "no filter"} on`;
        // `expectActivePreset` above waited for the view's controls, which stand inside the
        // filter row, so the row is drawn and one read finds it.
        const row = await filterRowReading(page);
        if (row === null) throw new Error(`the page draws no filter row ${at}`);
        console.log(`filter row ${at}: ${JSON.stringify(row)}`);
        expect(row.lines, `the filter row is not one line ${at}: ${row.controls.join(" | ")}`).toBe(
          1
        );
        expect(row.overflow, `the filter row runs past its own edge ${at}`).toBeLessThanOrEqual(1);
        // 35 px for the row of five, drawn from 1280 px, and 8 px above and below the 34 px one
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
        const version = preset === "spec" ? [CONTROL_LABELS.version] : [];
        if (width > 760) {
          expect(row.controls.slice(-(3 + version.length)), `the small controls ${at}`).toEqual([
            ...version,
            ...small,
          ]);
          // Each is drawn as the prototype draws Keys: a 1 px border in #ECEAE7, a 7 px radius,
          // 12 px text of weight 500 on a 20 px line, in #57534E.
          const looks = await filterRowLooks(page);
          expect(looks[CONTROL_LABELS.keys], `Keys is not the prototype's ${at}`).toMatchObject({
            border: "1px solid rgb(236, 234, 231)",
            color: "rgb(87, 83, 78)",
            font: "12px 500 20px",
            radius: "7px",
          });
          for (const name of [...version, CONTROL_LABELS.other_nodes, CONTROL_LABELS.filter]) {
            expect(looks[name], `${name} is not drawn as Keys is ${at}`).toEqual(
              looks[CONTROL_LABELS.keys]
            );
          }
          continue;
        }
        // On a phone the row holds the one control and Outline alone, and the sheet the rest.
        expect(row.controls.length, `the row draws more than its two controls ${at}`).toBe(2);
        expect(row.controls.at(-1)).toBe(CONTROL_LABELS.outline);
        await outlineControl(page).click();
        const sheet = outlineSheet(page);
        await expect(sheet).toBeVisible();
        await expect(
          sheet.getByRole("button", { exact: true, name: CONTROL_LABELS.other_nodes })
        ).toBeVisible();
        const box = sheet.getByRole("searchbox", { name: CONTROL_LABELS.filter });
        await expect(box).toBeVisible();
        expect(
          (await box.boundingBox())?.height ?? 0,
          `the words box in the Outline sheet is under 24 px tall ${at}`
        ).toBeGreaterThanOrEqual(24);
        await expect(sheet.getByTestId("issue-version-control")).toHaveCount(version.length);
        await page.keyboard.press("Escape");
        await expect(sheet).toHaveCount(0);
      }
    }

    // The dark scheme draws the row in its own page colours.
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await gotoIssue(page, presetPath(seeded.key, "spec"));
    await expectActivePreset(page, PRESET_NAMES.spec);
    await page.emulateMedia({ colorScheme: "dark" });
    // A control eases its colour over 150 ms after the scheme changes (`styles.css`).
    await expect
      .poll(() => offThePalette(page, "dark"), {
        message: "the filter row draws a colour that is no page colour in the dark scheme",
      })
      .toEqual([]);
    await page.emulateMedia({ colorScheme: "light" });

    // With a document open, at 760 px and below, the row is the one control and Outline, and the
    // sheet holds the document's version control, the node menu and the words box (R4).
    await page.setViewportSize(viewportAt(IPHONE_13.width));
    await gotoIssue(page, await specAddress(seeded.key));
    await expect(openedEditor(page), "the spec's address opens no editor").toBeVisible();
    // The page draws the view row before the document under it, whose editor the wait above saw.
    const documentRow = await filterRowReading(page);
    expect(
      documentRow === null ? [] : documentRow.controls,
      "the row draws other than the one control and Outline with a document open at 390 px"
    ).toEqual([expect.any(String), CONTROL_LABELS.outline]);
    await outlineControl(page).click();
    const withDocument = outlineSheet(page);
    await expect(withDocument).toBeVisible();
    await expect(
      withDocument.getByTestId("issue-version-control"),
      "the Outline sheet holds no version control of the open document"
    ).toHaveCount(1);
    await expect(
      withDocument.getByRole("button", { exact: true, name: CONTROL_LABELS.other_nodes })
    ).toBeVisible();
    await expect(
      withDocument.getByRole("searchbox", { name: CONTROL_LABELS.filter })
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(withDocument).toHaveCount(0);

    // `t a` opens the node menu with the focus on its first item, and `t f` the words box with
    // the caret in it, at 1440 px and, where the three stand in the Outline sheet, at 390 px.
    for (const width of desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expectOpeningLanded(page);
      await page.keyboard.press("t");
      await page.keyboard.press("a");
      await expect(page, `t a does not open the node menu's address at ${width} px`).toHaveURL(
        new RegExp(`/issues/${seeded.key}/artifacts$`)
      );
      await expect(
        openMenu(page).getByRole("menuitem").first(),
        `t a does not put the focus on the node menu's first item at ${width} px`
      ).toBeFocused();
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expectOpeningLanded(page);
      await page.keyboard.press("t");
      await page.keyboard.press("f");
      await expect(
        filterBox(page),
        `t f does not put the caret in the words box at ${width} px`
      ).toBeFocused();
    }

    // The words box opens from its control with the caret in it, narrows the outline by `?q=`,
    // stays open while it holds words, and shuts back into its control once emptied and left.
    await page.setViewportSize(viewportAt(DESKTOP.width));
    await gotoIssue(page, `/issues/${seeded.key}`);
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

// c-plain-words, and part N's R9 over INV-2's word states (`WORD_STATES`): no view, each view,
// the node menu open and a document open, at 1440 px and at 390 px.
test("the page draws no internal word and no helper text @phone", async ({ browser }) => {
  // The check reads the page's chrome in INV-2's fourteen word states and in the body of every
  // row the seeded issue draws with every branch open - 58 of them - each reading read against
  // seven patterns and the declared list. One `expect` per reading was 70,000 calls at about
  // 4 ms each - 281 s of assertion beside 20 s of page work - so each state gathers every
  // offender it finds and names them in one assertion, which reads the same strength and says
  // more: a state that draws three internal words names all three rather than the first. The
  // whole walk measured about 66 s on chromium in the issue pod, alone, so the row carries a
  // deadline of its own, with room for a slow leg.
  test.setTimeout(600_000);
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
      // `openPageState` waits for what the state draws - a view's rows, the menu's items, the
      // document's words - since a page read before its own reads land is a head and a frame,
      // which draw no internal word because they draw almost nothing. What it cannot say is that
      // the page has finished filling: it reads the state's first rows, and the walk over every
      // row's body below covers the rest.
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
    await gotoIssue(page, `/issues/${seeded.key}`);
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

// c-nodes. The seeded issue is today's model, an issue the conversion has not reached, so its
// spec, plan and verification documents are listed in the menu as every other document is.
test("documents open from the node menu and are never rows of the outline @phone", async ({
  browser,
}) => {
  // Measured in the issue pod, alone: 53.8 s on chromium and 50.4 s on iphone, for the five
  // presets at two widths, the menu's documents and an upload; the bound is three times that.
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const artifacts = (await getIssue(seeded.key)).artifacts ?? [];
    const slugs = new Set(artifacts.map((artifact) => artifact.slug));

    // No document and no file is a row of the outline, and no document's text and no editor
    // is drawn, with no filter and with each on, at 1440 px with every branch open and at
    // 390 px.
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        const where = `the ${PRESET_NAMES[preset]} preset at ${size.width} px`;
        await gotoIssue(page, presetPath(seeded.key, preset));
        await expectIssuePageDrawn(page);
        if (size.width >= 1024) await unfoldEveryBranch(page);
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

    // The node menu lists every document and artifact of the issue - its spec, plan,
    // verification and decisions documents among them - with the upload and New document.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    expect(
      await nodeMenuButton(page).count(),
      "the page draws no node menu beside the presets"
    ).toBe(1);
    await nodeMenuButton(page).click();
    expect(await openMenu(page).count(), "the node menu opens nothing").toBe(1);
    await expect(
      openMenu(page)
        .getByRole("button", { name: /^Copy reference/ })
        .first()
    ).toBeVisible();
    const items = (await nodeMenuItems(page).allInnerTexts()).map((text) =>
      text.replace(/\s+/g, " ").trim()
    );
    for (const artifact of artifacts) {
      expect(
        items.some((text) => text.includes(artifact.name)),
        `the node menu does not list ${artifact.name}: ${JSON.stringify(items)}`
      ).toBe(true);
    }
    for (const id of [
      seeded.specArtifactId,
      seeded.planArtifactId,
      seeded.verificationArtifactId,
    ]) {
      const name = artifacts.find((artifact) => artifact.id === id)?.name ?? id;
      expect(
        items.some((text) => text.includes(name)),
        `the node menu does not list the issue's role document ${name}`
      ).toBe(true);
    }
    for (const name of [CONTROL_LABELS.upload, CONTROL_LABELS.new_document]) {
      expect(
        await openMenu(page).getByRole("menuitem", { name }).count(),
        `the node menu holds no ${name}`
      ).toBe(1);
    }

    // A document opened from the menu is drawn in the main column in place of the outline, at
    // its own address, with its own text, its editor and its versions, under its head; its
    // control back to the conversation and Escape each return to the outline.
    const verification = artifacts.find((one) => one.id === seeded.verificationArtifactId);
    await openMenu(page)
      .getByRole("button", { exact: true, name: verification?.name ?? "" })
      .click();
    await expect(page, "the node menu's item does not open the document's address").toHaveURL(
      new RegExp(`/issues/${seeded.key}/artifacts/${verification?.slug ?? ""}$`)
    );
    await expect(
      page.getByText(VERIFICATION_PROSE),
      "the document opened from the node menu draws none of its own text"
    ).toBeVisible();
    await expect(openedEditor(page), "the opened document draws no editor").toBeVisible();
    expect(await tree(page).count(), "the outline is still drawn beside the document").toBe(0);
    expect(
      await childrenSection(page).count(),
      "the children section is still drawn beside the document"
    ).toBe(0);
    await expect(
      versionPicker(page),
      "the version control beside the filters does not read the opened document"
    ).toHaveCount(1);
    await expect(
      openedArtifact(page).getByRole("region", { name: /^Versions for / }),
      "the opened document draws no versions"
    ).toHaveCount(1);
    // Its head, as the prototype's `openDoc` draws it (R2): its name, then the version it shows
    // with who wrote that version and its day, and the control back to the conversation.
    const newest = [...((await getArtifact(seeded.verificationArtifactId)).versions ?? [])].sort(
      (one, other) => other.number - one.number
    )[0];
    if (newest === undefined) throw new Error("the seeded Verification document holds no version");
    await expect(openedArtifact(page).getByTestId("artifact-header")).toContainText(
      verification?.name ?? ""
    );
    await expect(
      openedArtifact(page).getByTestId("opened-artifact-version"),
      "the head does not name the version it shows, who wrote it and its day"
    ).toHaveText(
      new RegExp(
        `^${FILE_WORDS.version} ${newest.number} · ${(newest.authors ?? []).length > 0 ? ".+ · " : ""}${shortDay(newest.created_at)}$`,
        "u"
      )
    );
    const back = issuePage(page).getByRole("button", {
      exact: true,
      name: CONTROL_LABELS.close_document,
    });
    await expect(back, "the opened document draws no way back to the conversation").toBeVisible();
    await back.click();
    await expect(
      tree(page),
      `${CONTROL_LABELS.close_document} does not return to the outline`
    ).toBeVisible();
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
    expect(
      await openedArtifact(page).count(),
      `${CONTROL_LABELS.close_document} leaves the document drawn`
    ).toBe(0);
    await nodeMenuButton(page).click();
    await openMenu(page)
      .getByRole("button", { exact: true, name: verification?.name ?? "" })
      .click();
    await expect(openedEditor(page), "the document did not open again").toBeVisible();
    await page.keyboard.press("Escape");
    await expect(tree(page), "Escape does not return to the outline").toBeVisible();
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
    expect(await openedArtifact(page).count(), "Escape leaves the document drawn").toBe(0);

    // The block that cites a document carries a chip in its words that opens it, named by the
    // page's word for a document: a document's name is its file name, which no chip draws
    // (part G's R4).
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

    // The upload uploads a file.
    await nodeMenuButton(page).click();
    await openMenu(page).getByRole("menuitem", { name: CONTROL_LABELS.upload }).click();
    await uploadInput(page)
      .first()
      .setInputFiles({
        buffer: Buffer.from("# Uploaded\n\nFrom the node menu.\n"),
        mimeType: "text/markdown",
        name: "uploaded.md",
      });
    // The pick is staged, with its Summary, Upload and Cancel; Upload sends it.
    await page.getByRole("button", { exact: true, name: CONTROL_LABELS.upload }).click();
    await expect
      .poll(async () => ((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.name))
      .toContain("uploaded.md");

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
    // The panel shuts by its own control, as a reader shuts it, before the document's own list
    // is opened under it.
    await versionControlButton(page).click();
    await expect(versionControl(page)).toBeHidden();
    // The version list is folded at rest; its control opens it, with each version's download.
    await openedArtifact(page).getByRole("button", { name: "Show all versions" }).click();
    await expect(
      openedArtifact(page)
        .getByRole("link", { name: /^Download version \d+$/ })
        .first(),
      "no version's download is reachable from the artifact"
    ).toBeVisible();
    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.image}`);
    await expect(issuePage(page)).toHaveCount(1);
    await expect(
      page.locator('main img[src*="/versions/"]').first(),
      "the image draws no picture at its own address"
    ).toBeVisible();

    // Nothing is named Artifacts: on the issue's own address, the node menu's, and a
    // document's.
    for (const address of [
      `/issues/${seeded.key}`,
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
  });
});

// c-reflow, and part N's R9 over INV-2's reflow states (`REFLOW_STATES`): no view, each view, the
// node menu open, a document open and the Outline sheet open, at six widths.
test("the page never scrolls in two dimensions @phone", async ({ browser }) => {
  // Eight states at six widths, each a load of the page, with a clipping walk over every row a
  // view draws, each read in the browser, plus the page's own fit, the open menu's, the open
  // document's, the sheet's and the panel's. Measured in the issue pod, alone: 2.6 min on
  // chromium and 2.5 min on iphone, which read the same six widths; the bound is about four
  // times that, for a slow leg.
  test.setTimeout(600_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The test sets the viewport and writes no width rule of its own: every measure is the page's.
    for (const { state, width } of REFLOW_STATES) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      // Nothing below is measured until the state is drawn: a view's rows, the menu's items, the
      // document's words (`openPageState`). `gotoIssue` waits for the head alone, and the tree
      // is drawn empty and fills as the reads land, so a measurement taken then would read a
      // page that holds no row - and a page that holds no row is a page that fits, which is the
      // first thing `assertPageFits` retries its way into passing on. What the wait cannot say is
      // that the last row has landed; the walk below reads the rows that are drawn when it runs,
      // and six widths read them six times.
      await openPageState(page, seeded.key, state);
      const at = `${stateName(state)} at ${width} px`;
      // In every state the page fits its viewport, with its header and its view row whole.
      await assertPageFits(page);
      await assertWhole(identityBlock(page));
      await assertWhole(presetRow(page));
      if (state.kind === "node-menu") {
        // The open menu - its filter, its items and the controls on each - fits across the
        // screen; at 760 px and below it stands in the Outline sheet, which does too (R4).
        await assertWholeAcross(openMenu(page).locator(".."));
        if (width <= 760) await assertWholeAcross(outlineSheet(page));
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
      if (state.kind === "outline-sheet") {
        // From 1024 px the Contents pane stands at the outline's left: its list scrolls down by
        // design and is read for a sideways clip, and each of its one-line rows for a box that
        // reaches past its own. Below 1024 px the Outline control opened the same list in a
        // sheet over the page, which leaves the page fitting its viewport.
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

/** The issue's priority row, the one place its add control and that control's picker are drawn
 *  (`IssuePriorityLine.tsx`). */
function priorityRow(page: Page): Locator {
  return page.getByTestId("issue-advances");
}

/** Closes whatever the view before this one opened, so the next control is reached rather than
 *  dismissed: a press outside an open menu, or outside the identity block's actions group,
 *  closes it instead of reaching the control under it, and Escape closes each of them
 *  (`IssueActionsMenu.tsx`). The priority row's picker closes by its own Cancel, as a reader
 *  closes it, and Details by its own Close, since on a phone it covers the page. */
async function closeOpenControls(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(openMenu(page)).toHaveCount(0);
  const actions = offered(page, "button", CONTROL_LABELS.issue_actions);
  // The header drew this control with the page an earlier view loaded, and Escape draws no
  // control, so one count says whether this page shows it.
  if ((await actions.count()) === 1) {
    await expect(actions).not.toHaveAttribute("aria-expanded", "true");
  }
  const picker = priorityRow(page).getByRole("combobox", {
    exact: true,
    name: CONTROL_LABELS.priority_to_advance,
  });
  // The picker opens only on a press, and Escape above settles inside its own keydown whether
  // it shut it, so one count says whether it is still open.
  if ((await picker.count()) > 0) {
    await priorityRow(page)
      .getByRole("button", { exact: true, name: CONTROL_LABELS.cancel })
      .click();
    await expect(picker).toHaveCount(0);
  }
  await closeDetails(page);
}

// c-nothing-lost.
test("the new page carries every element of the old one @phone", async ({ browser }) => {
  // Four states, each read through the page's own views - its five presets, the four
  // addresses beside them, the seven views behind a control or a width, one per row of the
  // tree, and, where the narrow tree is the way in, the presets and the zooms walked again.
  // Each walk loads the page once and moves through it from there, which is how a reader moves
  // through it: 18 loads of the issue page per 390 px state and 12 per 1440 px one, naming 128
  // and 40 views, each settled over two readings of its accessibility tree, and the write walk
  // below after them. A view over a row the tree does not draw answers that it is not there
  // and is not read, so a state pays for the views it has.
  //
  // Measured on an issue pod's two CPUs, the whole row in the desktop project, before the
  // walks shared their loads: 8.2 minutes, of which the four states' walks were 424 s - 173 s
  // of them 106 page loads and 198 s the settles. The row carries a deadline of its own
  // because that outlasts the 30 s default many times over, and the deadline leaves room for
  // a loaded box.
  test.setTimeout(900_000);
  const fixture = JSON.parse(
    await readFile(
      fileURLToPath(new URL("./fixtures/issue-page-inventory.json", import.meta.url)),
      "utf8"
    )
  ) as PageInventoryFixture;

  await phase(browser, seedWholeIssueAdvancing, async (page, seeded) => {
    // The inventory first: what the old page offered is this check's own subject, and the page
    // root below it says whether the page carrying it is the redesigned one. The fixture is the
    // merge of the old page's nine tab panels and its priority picker, one view at a time, so
    // the new page is read the same way: its five presets and the four addresses beside them,
    // the conversation, the activity, the collaborators and the artifacts (R3, R12, R19). Four
    // more views are the page's own, because what they hold sits behind a control rather than
    // behind an address: the node menu, where every document and artifact of the issue lives
    // (R3); the identity block's actions menu, where Close and the parent editor live (R11);
    // the priority row's picker, which its add control opens (R13); and each row of the tree,
    // whose body is what a node opens (R6).
    // How far a walk over the tree reaches. The seeded issue draws fewer rows than this at
    // either width, and a view over a row the tree does not draw answers that it is not there
    // rather than reading the page the view before it left (`e2e/page-inventory.ts`), so the
    // bound costs a count and nothing else where the rows run out.
    const walkDepth = 24;
    // Which rows of the tree hold a zoom. The zoom walk's first view reads it off the tree it
    // loads and every view of that walk after it reads it back; each walk fills it again,
    // because the tree a width and a state draw is their own.
    const zoomsInto = new Set<number>();
    // The spec's name, which its item in the node menu carries.
    const specName = (await getArtifact(seeded.specArtifactId)).name;
    // The outline drawing both rows a parent-row mapping names (`page-inventory.ts`): every
    // branch open where the outline draws the whole tree, and below 1024 px, where a twisty
    // zooms instead, the target's own zoom, whose nodes follow it. It is read off a page that
    // has stopped reading, as the zoom walk below is, because the tree fills as its reads land.
    // A target the tree does not draw has no zoom to press, and the mapping is then lost, which
    // `parentRowMissing` names: a press waiting for that twisty times out and ends the row
    // there, leaving every state after it unread.
    const outline = (width: number) => async (mapping: ParentRowMapping) => {
      const settling = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectLoadSettled(settling);
      if (width >= 1024) {
        await unfoldEveryBranch(page);
        return;
      }
      const twisty = nodeRows(page)
        .filter({ hasText: mapping.parent })
        .first()
        .getByRole("button", { name: CONTROL_LABELS.unfold });
      // The page's reads went quiet above, so the tree is drawn whole and one count says whether
      // the parent's row draws a twisty.
      if ((await twisty.count()) > 0) {
        await twisty.click();
        // The zoom is drawn once its crumbs are, and the rows are read the moment this returns.
        await expect(
          zoomCrumbs(page),
          `the twisty of "${mapping.parent}" zoomed nowhere`
        ).not.toHaveCount(0);
      }
    };
    const views = (width: number) => [
      ...PRESET_ORDER.map((preset) => async () => {
        await gotoIssue(page, presetPath(seeded.key, preset));
        return true;
      }),
      // The conversation's address mounts the panel's list (`IssuePage.tsx`'s
      // `activatedTabs`), which is in Details, so the view opens it as a reader does (R14).
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}/conversation`);
        await openDetails(page);
        return true;
      },
      // The shell's navigation: on the issue page the rail's foot control, or the header's
      // `☰` on a phone, opens the whole of it over the page, which holds every entry the old
      // page's sidebar offered (R14).
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        await page.getByRole("button", { exact: true, name: "Open navigation" }).click();
        await expect(page.getByRole("dialog", { exact: true, name: "Navigation" })).toBeVisible();
        return true;
      },
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}/activity`);
        return true;
      },
      // `/issues/<key>/collaborators`, the address the Collaborators tab answered: it is the
      // one address that opens the panel's list of everyone who worked the issue (R19,
      // `IssuePage.tsx`'s `opensCollaborators`), and the list's own People and Other agents
      // sections (`IssueCollaboratorList.tsx`) are drawn at no other address, so a walk
      // without this view reads that tab's landmarks as lost.
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}/collaborators`);
        return true;
      },
      // `/issues/<key>/artifacts`, the address the Artifacts tab answered, opens the node menu,
      // which carries that tab's list (R20, the Design's "Where each thing goes"): each file
      // with its download and, for a document the issue can drop, Remove, the list's filter,
      // the upload and New document, which a closed issue draws refused.
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}/artifacts`);
        await expect(openMenu(page), "/artifacts opens no node menu").toHaveCount(1);
        await expect(
          openMenu(page)
            .getByRole("button", { name: /^Copy reference/ })
            .first()
        ).toBeVisible();
        return true;
      },
      // The spec opened from the node menu, in place of the outline (R3): its editor, with its
      // collaborative cursors and its comment chord, and the version control beside the filters
      // reading it, with the old spec toolbar's link to the block the caret is in and its live
      // connection, which belong to an open editor.
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        await nodeMenuButton(page).click();
        await openMenu(page).getByRole("button", { exact: true, name: specName }).click();
        await expect(
          openedEditor(page),
          "the spec opened from the menu draws no editor"
        ).toBeVisible();
        // The version control opens from its own small control (ruling 5), or from the Outline
        // sheet at 760 px and below; Escape shuts it again and leaves the document open.
        await openVersionControl(page);
        await expect(
          versionControl(page).getByRole("button", { name: "Copy link to block" })
        ).toHaveCount(1);
        await page.keyboard.press("Escape");
        await expect(versionControl(page)).toBeHidden();
        await expect(
          openedEditor(page),
          "Escape on the version control shut the document"
        ).toBeVisible();
        // The margin's cards beside the opened spec - its decisions, its comments - are in
        // Details, which a reader opens to read them beside the document (R14).
        await openDetails(page);
        return true;
      },
      // The five views a control opens are all of the issue's own address, so they are one
      // load walked through rather than a load each: a reader opens them one after another on
      // the page they are on. Each opens where the last left off, and the menus are closed
      // first, because a press outside an open menu closes that menu instead of reaching the
      // control under it. Each of the first two says the control is there before it reaches
      // for it. A click waits for its element with no deadline of its own (Playwright's
      // `actionTimeout` is unset here), so a page drawing neither control would spend this
      // row's whole deadline inside one click and report a timeout; asserted first, such a
      // page says which control is missing.
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}`);
        await expect(
          nodeMenuButton(page),
          `the page draws no ${CONTROL_LABELS.other_nodes} control: its view is unreadable`
        ).toHaveCount(1);
        await nodeMenuButton(page).click();
        await expect(openMenu(page)).toHaveCount(1);
        // The menu's rows are every artifact of the issue (`issue.artifacts`), which arrive with
        // the issue read rather than with the shell: a menu opened before that read lands draws
        // the upload item alone, and the snapshot taken then records every document of this view
        // as lost.
        // Measured on this branch at 390 px, three times in two runs, each naming exactly
        // `button "Copy reference dispatch://LAY-2/artifact/notes-md"` and each passing on the
        // retry. So the view is not there until a document row of it is.
        await expect(
          openMenu(page)
            .getByRole("button", { name: /^Copy reference/ })
            .first()
        ).toBeVisible();
        return true;
      },
      async () => {
        await closeOpenControls(page);
        const actions = offered(page, "button", CONTROL_LABELS.issue_actions);
        await expect(
          actions,
          `the page draws no ${CONTROL_LABELS.issue_actions} control: its view is unreadable`
        ).toHaveCount(1);
        await actions.click();
        return true;
      },
      // The priority row's picker (dispatch://INFRA-2947 R3), which the row's add control opens
      // and no address does: the old page drew that row in its header's rail and its recording
      // opened the picker there (`record-page-inventory.e2e.ts`), and this page draws it in the
      // Details panel's Development section (R13), which a reader opens first (R14). A closed
      // issue's add control is refused, so there the view is Details with the menus closed, as
      // the three views below are where what they open is already open; the next view closes
      // the picker by its Cancel.
      async () => {
        await closeOpenControls(page);
        await openDetails(page);
        const add = priorityRow(page).getByRole("button", {
          exact: true,
          name: `${CONTROL_LABELS.add_priority} to ${seeded.key}`,
        });
        await expect(
          add,
          `the page draws no ${CONTROL_LABELS.add_priority} control: its view is unreadable`
        ).toHaveCount(1);
        // No write of the row is out, so only the issue's closed state refuses Add, and the
        // page read that state before Details opened: one read says whether Add is refused.
        if (await add.isEnabled()) {
          await add.click();
          await expect(
            priorityRow(page).getByRole("combobox", {
              exact: true,
              name: CONTROL_LABELS.priority_to_advance,
            })
          ).toBeVisible();
        }
        return true;
      },
      // The preset row below 1280 px is one chooser rather than a row of tabs, and the panel is
      // Details, which a reader opens from the header at every width (R14): each is a view of
      // the page, and at a width where it is already open the view is the page with the menus
      // closed, which is the page a reader is left on there.
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
      // text, its facts and its controls are in that body (R2). The walk loads the page once
      // and then moves through the rows, which is how a reader moves through them: each view is
      // still read, and still fitted, on its own.
      //
      // At a width that draws the whole tree the walk opens the branches the page opened
      // folded first (R1): a node behind a fold - the check under a requirement - is a
      // twisty away for a reader, and a walk that never pressed one would read it as lost.
      // Below 1024 px a twisty zooms instead, which the zoom walk below does.
      ...Array.from({ length: walkDepth }, (_unused, index) => async () => {
        if (index === 0) {
          // The tree is drawn empty and fills as the reads land, so it is read once the page has
          // stopped reading, as `unfoldEveryBranch` asks of its caller.
          const settling = watchApiRequests(page);
          await gotoIssue(page, `/issues/${seeded.key}`);
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
      // zooming the walk reaches by zooming too. The presets first, because a decision, a
      // check and a plan task sit under the issue in the preset that draws them; then one
      // zoom per row of the issue's own address, because a node that hangs off a row - the
      // document a requirement cites - is behind that row's own twisty and nowhere else. At a
      // width that draws the whole tree the views above already open every row, so neither
      // walk runs there. Each walk loads its page once, as the walk above does.
      ...(width >= 1024
        ? []
        : PRESET_ORDER.flatMap((preset) =>
            Array.from({ length: 8 }, (_unused, index) => async () => {
              if (index === 0) {
                // Read once the page has stopped reading, as the walk above is: the tree is
                // drawn empty and fills as the reads land.
                const settling = watchApiRequests(page);
                await gotoIssue(page, presetPath(seeded.key, preset));
                await expectLoadSettled(settling);
                const first = nodeRows(page);
                // The page's reads went quiet above, so the tree is drawn whole and one count
                // says whether it draws a row.
                if ((await first.count()) > 0) await first.first().focus();
                return true;
              }
              const rows = nodeRows(page);
              // The walk's first view waited for a quiet load, and each view since moved the
              // focus alone, which draws no row, so one count holds the rows.
              if ((await rows.count()) <= index) return false;
              await rows.nth(index).focus();
              return true;
            })
          )),
      ...(width >= 1024
        ? []
        : Array.from({ length: walkDepth }, (_unused, into) => {
            // Whether this row's zoom was opened: the rows under one are read only where there
            // was one to open.
            let zoomed = false;
            return Array.from({ length: 2 }, (_also, index) => async () => {
              if (index === 0) {
                // A row with no branch of its own has no zoom to walk into, and this walk
                // neither loads the page for it nor reads the page the walk is already on:
                // which rows hold one is read once, off the tree the first view loads, and a
                // row the tree does not draw is not in it either. Every zoom that is there is
                // still opened on a page of its own, because a zoom is left by loading the
                // issue again.
                if (into === 0) {
                  // Read off a page that has stopped reading: the tree is drawn empty and
                  // fills as the reads land (`expectLoadSettled`), so a set taken the moment
                  // the navigation returns holds the twisties of a half-filled tree, and a
                  // row missed here is a node this state never zooms into. Measured on a
                  // loaded box at 390 px: a document's own copy control read as lost
                  // on the attempt and was found on the retry.
                  const settling = watchApiRequests(page);
                  await gotoIssue(page, `/issues/${seeded.key}`);
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
                  await gotoIssue(page, `/issues/${seeded.key}`);
                }
                if (!zoomsInto.has(into)) return into === 0;
                const twisty = nodeRows(page)
                  .nth(into)
                  .getByRole("button", { name: CONTROL_LABELS.unfold });
                await twisty.first().click();
                await expect(
                  zoomCrumbs(page),
                  `row ${into}'s twisty zoomed nowhere`
                ).not.toHaveCount(0);
                zoomed = true;
              } else if (!zoomed) {
                return false;
              }
              // The zoomed node is the first row; its own nodes follow it, and the first of
              // them - a check under the row's requirement - is read by the view that
              // opened the zoom.
              const under = nodeRows(page);
              // The zoom was waited for where it was opened, and the focus alone moved since,
              // which draws no row, so one count holds the zoom's rows.
              if ((await under.count()) > index + 1) await under.nth(index + 1).focus();
              return true;
            });
          }).flat()),
    ];
    // Which states this project reads. The fixture was recorded in a desktop browser, so its
    // 1440 px states are a desktop shell's: setting 1440 px on an iPhone profile draws a
    // different shell around the same page, and what that shell offers - its search control
    // among it - is no part of what the issue page carries. So the desktop project reads all
    // four states and the phone project the two 390 px ones, and between them the check's own
    // words, at 1440 px and at 390 px, are both still asserted.
    const desktopProject = test.info().project.name !== "iphone";
    const states = (
      [
        ["open-1440", DESKTOP],
        ["open-390", PHONE],
      ] as const
    ).filter(([state]) => desktopProject || state.endsWith("-390"));
    for (const [state, size] of states) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await assertInventoryCarried(page, fixture[state], {
        outline: outline(size.width),
        awaiting: AWAITING_THE_BOX,
        parentRows: INVENTORY_PARENT_ROWS,
        renames: INVENTORY_RENAMES,
        state: `${state}, recorded at ${fixture.commit}`,
        views: views(size.width),
      });
    }
    await patchIssue(seeded.key, { status: "done" });
    const closed = (
      [
        ["closed-1440", DESKTOP],
        ["closed-390", PHONE],
      ] as const
    ).filter(([state]) => desktopProject || state.endsWith("-390"));
    for (const [state, size] of closed) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await assertInventoryCarried(page, fixture[state], {
        outline: outline(size.width),
        awaiting: AWAITING_THE_BOX,
        parentRows: INVENTORY_PARENT_ROWS,
        renames: INVENTORY_RENAMES,
        state: `${state}, recorded at ${fixture.commit}`,
        views: views(size.width),
      });
    }
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);

    // Each recorded name that writes today still writes: the write is made through the page and
    // the value read back from Dispatch. Close, Reopen and the parent editor are behind the
    // identity block's one actions control now (R11), so each is reached through it.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await openIssueActions(page);
    await offered(page, "button", CONTROL_LABELS.reopen_into_backlog).click();
    await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");

    await identityBlock(page).getByRole("combobox", { name: "Status" }).selectOption("todo");
    await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("todo");

    await offered(page, "combobox", `Priority of ${seeded.key}`).selectOption("3");
    await expect.poll(async () => (await getIssue(seeded.key)).priority).toBe(3);

    // A picker whose list is read when the reader reaches for it - the people and the
    // repositories (`AssigneeControl.tsx`, `RepoControl.tsx`) - is reached for first, as a
    // reader reaches for it; without that it offers the value it already holds and nothing else.
    const assignee = offered(page, "combobox", `Assignee of ${seeded.key}`);
    await assignee.focus();
    await expect(assignee.locator('option[value="bob"]')).toHaveCount(1);
    await assignee.selectOption("bob");
    await expect.poll(async () => (await getIssue(seeded.key)).assignee).toBe("bob");

    // The labels' control is Details' Labels row; the header draws the labels that fit (R13).
    await openDetails(page);
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

    // The repository and every property under it are in Details (R13, R14).
    await openDetails(page);
    const repository = offered(page, "combobox", `Repository of ${seeded.key}`);
    await repository.focus();
    const repoBefore = (await getIssue(seeded.key)).repo;
    await expect.poll(async () => (await optionsOf(repository)).length).toBeGreaterThan(1);
    // Any option but the issue's own repository serves, and the write and the check below both
    // follow whichever one this read finds, so one read is enough.
    const other = (await optionsOf(repository)).find((option) => option.value !== repoBefore);
    await repository.selectOption(other?.value ?? "");
    // The control's empty option is "No repository", which the issue reads back as none.
    const wroteRepo = other?.value === "" || other === undefined ? null : other.value;
    await expect.poll(async () => (await getIssue(seeded.key)).repo).toBe(wroteRepo);

    await offered(page, "button", `Edit components of ${seeded.key}`).click();
    await page.getByRole("option", { name: "Issue panel" }).first().click();
    await page.keyboard.press("Escape");
    await expect
      .poll(async () => (await getIssue(seeded.key)).components.ids)
      .not.toContain("panel");
    // The header's own controls are written with Details closed, which on a phone covers them.
    await closeDetails(page);

    // The pin is the overflow menu's.
    await openIssueActions(page);
    await offered(page, "button", "Unpin issue").click();
    await expect.poll(async () => (await getMyState("alice"))[seeded.key]?.pinned).toBe(false);

    // The close before the closed-state inventory took the seed's claim with it, so the issue
    // is claimed again for the one control that hands a claim back.
    await claimIssue(seeded.key, asPlanner);
    // The claim and its Release are Details' (R13).
    await openDetails(page);
    await expect(offered(page, "button", `Release the claim on ${seeded.key}`)).toBeVisible();
    await offered(page, "button", `Release the claim on ${seeded.key}`).click();
    await expect.poll(async () => (await getIssue(seeded.key)).claim).toBeNull();
    await closeDetails(page);

    await openIssueActions(page);
    await offered(page, "button", "Close issue").click();
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

    // The approval: the seed requests none, so it is requested here and approved from the page.
    await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
    await openPreset(page, seeded.key, "spec");
    // Approve is the identity block's own chip: the preset's toolbar draws how the document
    // stands and no action (`features/doc/ApprovalChip.tsx`'s `showActions`).
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
    // Keep's own write, read back from Dispatch, as every write of this walk is: the press is
    // what this step proves, and a press the page lost used to leave the decision waiting
    // while the step went on to Change regardless (`NodeRow.tsx`, the press that scrolled).
    await expect
      .poll(
        async () =>
          ((await getIssueDecisions(seeded.key)).decisions ?? []).find(
            (decision) => decision.block_id === decidingBlock
          )?.kept ?? null,
        { message: "Keep was pressed and Dispatch recorded no Keep of that decision" }
      )
      .not.toBeNull();
    // The page has taken the write once Keep is gone from the row, and the preset has opened
    // the next waiting decision's body by then. This row's body is still open: Keep moves the
    // focus onto its decision's row before its write disables it (`DecisionActions.tsx`), so
    // Change is pressed where it was, on the row the reader never left.
    await expect(
      decidingRow.getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
    ).toHaveCount(0);
    await decidingRow
      .getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision })
      .click();
    // The change is written in the decision's own composer, which names its box and its send
    // apart from the conversation's (`DecisionActions.tsx`, R7).
    await page
      .getByRole("textbox", { name: CONTROL_LABELS.your_change })
      .fill("Change this decision.");
    await offered(page, "button", CONTROL_LABELS.send_change).click();
    await expect
      .poll(async () => (await listComments(seeded.key)).map((comment) => comment.body))
      .toContain("Change this decision.");

    // The upload, the composer, an ask inside a document, a node's text, and a named version.
    await nodeMenuButton(page).click();
    await openMenu(page).getByRole("menuitem", { name: CONTROL_LABELS.upload }).click();
    await uploadInput(page)
      .first()
      .setInputFiles({
        buffer: Buffer.from("# Carried\n"),
        mimeType: "text/markdown",
        name: "carried.md",
      });
    // The pick is staged behind its summary field until its own Upload confirms it
    // (`ArtifactUploadRow` in `features/artifacts/ArtifactUpload.tsx`), as on every page that
    // uploads an issue's file.
    await page.getByRole("button", { exact: true, name: "Upload" }).click();
    await expect
      .poll(async () => ((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.name))
      .toContain("carried.md");

    // No write through the composer: on the new page it is part L's box, which follows this part
    // (part N's R11) and is in `AWAITING_THE_BOX`. Part L's pull request writes a comment in the
    // box here and reads it back.

    // Back to the issue's own address, where the tree opens on the spec's nodes.
    await gotoIssue(page, `/issues/${seeded.key}`);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    // The ask is drawn as a row of the tree and again as a card in the panel (R1, R12), which is
    // Details (R14): the answer is written on the card, with Details open, and Details closed
    // again for the outline, which on a phone it covers.
    await openDetails(page);
    const question = marginSlot(page).getByTestId(`ask-${seeded.openQuestion.askId}`);
    await question.getByRole("radio").first().check();
    await question.getByRole("button", { name: /^Answer/ }).click();
    await expect
      .poll(async () => (await getAsk(seeded.openQuestion.askId)).ask.state)
      .toBe("answered");
    await closeDetails(page);

    await nodeRow(page, blockNodeId(specSlug, requirementBlockId(3)))
      .first()
      .focus();
    await page.keyboard.press("Enter");
    // The words go where the caret is, so they wait for it to land in the row's own text: typed
    // before it does, the space and the capital reach the page's own keys and the rest of the
    // words land in whatever those keys opened.
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in requirement 3" })
      .toBe(true);
    await page.keyboard.type(" Edited in place.");
    await expect
      .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
      .toContain("Edited in place.");

    // Naming a version, from the version control beside the filters, which the Spec filter
    // draws reading the spec; with no filter on none is drawn (R2).
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

    // Editing a document's text in the document opened from the node menu, in place of the
    // outline, with its own editor.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await nodeMenuButton(page).click();
    await openMenu(page).getByRole("button", { exact: true, name: specName }).click();
    // The caret goes where a reader's click after a requirement's own words puts it: text the
    // document holds as editable, inside the requirement's block.
    await placeCaret(page, "after", requirementText(2), openedEditor(page));
    await page.keyboard.type(" Edited where it was opened.");
    await expect
      .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
      .toContain("Edited where it was opened.");
  });
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

/** The records INV-1's addresses are written with (`AddressSeed`): the whole issue's, and an
 *  agents' comment, a twin comment, a comment of another issue and an approval request, written
 *  here beside it. */
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
  return {
    agentsOnly: agentsOnly.id,
    approval: { artifactId: seeded.specArtifactId, askId: approval.ask.id },
    comment: {
      id: seeded.comments.anchored,
      short: seeded.comments.anchored.slice(0, SHORT_ID),
      words: "This requirement is the one the tree reads first.",
    },
    goneAsk: "00000000-0000-4000-8000-000000000000",
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
    case "node-menu": {
      await expectIssuePageDrawn(page);
      await expect(openMenu(page), `${form} does not open the node menu`).toHaveCount(1);
      return;
    }
    case "document": {
      await expect(openedEditor(page), `${form} opens no document's editor`).toBeVisible();
      await expect(openedArtifact(page), `${form} does not open that document`).toContainText(
        content.words
      );
      await expect(tree(page), `${form} draws the outline beside the document`).toHaveCount(0);
      return;
    }
    case "document-block": {
      await expect(openedEditor(page), `${form} opens no document's editor`).toBeVisible();
      await expect(
        openedEditor(page).locator(`[data-block-id="${content.block}"]`),
        `${form} does not open its document at the block`
      ).toBeInViewport();
      await expect(openedEditor(page)).toContainText(content.words);
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
    case "replaced": {
      await expect(page, `${form} is not replaced by ${content.to}`).toHaveURL(content.to);
      await expectAddressOpens(page, content.to, form, content.lands);
      return;
    }
  }
}

// c-urls; c-addresses (1): INV-1, every address of today's page and what it opens.
test("every address of the old page still resolves", async ({ browser }) => {
  // Thirty-five cold loads of the issue page, each one a navigation and its reads: the source's
  // eighteen took 28 s, so the row carries a deadline of its own rather than the 60 s default.
  test.setTimeout(300_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const seed = await addressSeed(seeded);
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
    for (const cases of Object.values(OLD_PAGE_ADDRESSES)) {
      for (const one of cases) {
        const address = one.address(seed);
        const form = address.slice(`/issues/${seed.key}`.length) || `/issues/${seed.key}`;
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
    }

    // `t e`, the Steps tab's chord, opens Details at its steps as `/steps` does (answer A).
    await resolves("t e", async () => {
      await gotoIssue(page, `/issues/${seed.key}`);
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

// c-requests.
test("the first load stays inside its request budget", async ({ browser }) => {
  let baseline: { byRoute: string; total: number } | null = null;

  await phase(browser, seedRequestBaseline, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    traffic.reset();
    await gotoIssue(page, `/issues/${seeded.key}`);
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

    // Opening a document reads its text: the spec, opened from the node menu.
    const specName = (await getArtifact(seeded.specArtifactId)).name;
    await nodeMenuButton(page).click();
    await openMenu(page).getByRole("button", { exact: true, name: specName }).click();
    await expect(openedEditor(page)).toBeVisible();
    await expect
      .poll(textReads, { message: "opening the spec read no text of it" })
      .toContainEqual(expect.stringContaining(seeded.specArtifactId));
  });

  // No request for each node: twice the nodes, the same count.
  await phase(browser, seedTwiceTheNodes, async (page, seeded) => {
    const traffic = watchApiRequests(page);
    traffic.reset();
    await gotoIssue(page, `/issues/${seeded.key}`);
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

/** Opens the issue's `/artifacts`, closing Details first where it covers a phone's page
 *  (dispatch://INFRA-1196 R14). */
async function openArtifacts(page: Page, isPhone: boolean) {
  if (isPhone) await closeDetails(page);
  await openIssueAddress(page, "artifacts");
}

/** The node menu's item for the file `name`. The menu took the Artifacts tab's list
 *  (dispatch://INFRA-1196 R3, the Design's "Where each thing goes"), and its item carries what
 *  the list's row offered beside the file: the control that opens it, its copy control, its
 *  latest version's download and, for a document the issue can drop, Remove. The file's versions,
 *  its picture and what references it are on its own page, which the item opens. */
function menuItem(page: Page, name: string): Locator {
  return page
    .getByRole("menu")
    .getByRole("menuitem")
    .filter({ has: page.getByRole("button", { exact: true, name }) });
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
// @pr: Upload and download: three real uploads, the versions they build, the Download link on a version, and the references that cite them.
test("artifacts upload, version, references, and phone layout @phone @pr", async ({
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
  await openArtifacts(page, testInfo.project.name === "iphone");

  // The menu's upload stages the picked file, with its Summary, Upload and Cancel.
  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagramUpload = await diagramFile(testInfo);
  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  const diagram = menuItem(page, "diagram.png");
  await expect(diagram).toBeVisible();
  await expect(diagram.getByRole("link", { name: "Download version 1" })).toBeVisible();

  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  await expect(diagram.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  // The menu item is compact: the versions, their count, the picture, the compare and the
  // references live on the artifact's own page, not under every item.
  await expect(diagram.getByRole("button", { name: "Show all versions" })).toHaveCount(0);
  await expect(page.getByLabel(CONTROL_LABELS.filter_files)).toBeVisible();

  await navigateInApp(page, `/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page.getByTestId("artifact-header")).toContainText("diagram.png");
  await expect(page.getByRole("img", { name: "diagram.png version 2" })).toBeVisible();
  expect(sockets()).toBe(1);
  await page.getByRole("button", { name: "Show all versions" }).click();
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
  await openArtifacts(page, testInfo.project.name === "iphone");

  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  const notes = menuItem(page, "notes.md");
  await expect(notes).toBeVisible();
  await expect(notes.getByRole("button", { name: /make primary/i })).toHaveCount(0);
  await navigateInApp(page, await specAddress(issue.key));
  await expect(documentEditor(page)).toContainText("The original document.");

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`,
  });

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/diagram-png before deciding.`,
  });
  // No reload: both citations reach the open page on its event feed, and the panel below reads
  // what the feed left.
  await openArtifacts(page, testInfo.project.name === "iphone");

  await diagram.getByRole("button", { exact: true, name: "diagram.png" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  // The panel names the record that cites this artifact and quotes it; its link goes to that
  // source, not back to the artifact the reader is already on.
  // What references the opened artifact is folded under its control (part N's R2).
  await openedArtifact(page).getByRole("button", { exact: true, name: "Referenced by" }).click();
  const diagramReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(diagramReferencedBy).toContainText(`Comment · ${issue.key}`);
  await expect(diagramReferencedBy).toContainText(
    `See dispatch://${issue.key}/artifact/diagram-png before deciding.`
  );
  await diagramReferencedBy.getByRole("link", { name: `Comment · ${issue.key}` }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/comments/`));

  // One cold load, so the notes.md panel below is read by a page that cached nothing.
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await notes.getByRole("button", { exact: true, name: "notes.md" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  await openedArtifact(page).getByRole("button", { exact: true, name: "Referenced by" }).click();
  const notesReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(notesReferencedBy).toContainText(`Comment · ${issue.key}`);
  await expect(notesReferencedBy).toContainText(
    `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`
  );
  await navigateInApp(page, `/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
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
  await openArtifacts(page, false);

  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagram = menuItem(page, "diagram.png");
  const notes = menuItem(page, "notes.md");
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

// The node menu's upload stages a picked file as the Artifacts list's did (dispatch://INFRA-1196
// R19, "nothing lost"): the file's name, an optional Summary for the version it makes, and Upload
// and Cancel. Cancel sends nothing; Upload sends the file with its summary.
test("the node menu's upload stages a file with a Summary for its version, and Cancel sends nothing", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Staged upload" });
  const held = (await listIssueArtifacts(issue.key)).length;
  await page.goto(`/issues/${issue.key}/artifacts`);

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
  await expect(menuItem(page, "notes.md")).toBeVisible();
  const notes = (await listIssueArtifacts(issue.key)).find((one) => one.name === "notes.md");
  expect(notes?.versions?.map((version) => version.summary)).toEqual(["First review notes"]);
});

// SLOW(>=2s): a real upload the history links, and the keyboard tour of the five presets with
// each tab's box measured, at 1440 px and at 390 px.
//
// The nine tabs are gone (dispatch://INFRA-1196 R3): the row is five presets, and how many
// artifacts an issue carries is the node menu's, not a tab's badge, so the `Artifacts (N)`
// assertions this test carried are retired with it - what they proved, that the live feed
// refreshes the page's own counts without a reload, is proved here on the history, in Details.
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
  await expect(toggles).toHaveCount(5);
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
  const first = presetToggle(page, "conversation");
  await first.focus();
  for (const preset of ["spec", "verification", "plan", "decisions"] as const) {
    await page.keyboard.press("ArrowRight");
    const toggle = presetToggle(page, preset);
    await expect(toggle).toBeFocused();
    await insideToolbar(toggle);
    await fitsTheRow(toggle, preset);
  }
  await page.keyboard.press("Home");
  await expect(first).toBeFocused();
  await insideToolbar(first);
  await fitsTheRow(first, "Conversation");

  // At 390 px the row is one control naming the one on, which opens the five. This is the
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
  // The five are radio items of the menu, the one on checked (part D's `IssuePresets.tsx`).
  await expect(page.getByRole("menu").getByRole("menuitemradio")).toHaveCount(5);
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
// R3): the node menu holds one item per file, whose name is never cut, and a file's version, size
// and upload time are on its own page.
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

  await page.goto(`/issues/${issue.key}/artifacts`);
  const files = page
    .getByRole("menu")
    .getByRole("menuitem")
    .filter({ has: page.getByRole("link", { name: /^Download version/ }) });
  await expect(files).toHaveCount(2);

  // Every item shows its whole name: it wraps where it must rather than truncating. The count of
  // two above waited for both items, so one read lists them.
  for (const file of await files.all()) {
    const name = file.getByRole("button").first();
    expect(await name.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
  }
  await files.getByRole("button", { exact: true, name: "audit.md" }).click();
  await page.getByRole("button", { name: "Show all versions" }).click();
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
  const row = menuItem(page, "cached-repair.md");
  const openDocument = () =>
    row.getByRole("button", { exact: true, name: "cached-repair.md" }).click();

  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, false);
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
    await openIssue(page, issue.key, "Keyboard issue");
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
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}$`));
    await expectActivePreset(page, PRESET_NAMES.conversation);
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-conversation-${testInfo.project.name}.png`),
    });

    // Every chord of the key table opens its target's address (R11, R20): a filter's chord
    // turns it on from any other view and off from its own, `t c` turns every filter off, and
    // the rest open the steps, the node menu, the history and the collaborators.
    for (const [key, address] of [
      ["k", "verification"],
      ["p", "plan"],
      ["e", "steps"],
      ["d", "decisions"],
      ["a", "artifacts"],
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
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}$`));
    await page.keyboard.press("t");
    await page.keyboard.press("p");
    await page.keyboard.press("t");
    await page.keyboard.press("c");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}$`));

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
    // names, and the panel, the children and the node menu as the page names them.
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
    // The history takes `t v`, since `t a` opens the node menu and `l` the labels. Its words
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
  await gotoIssue(page, `/issues/${seeded.key}`);
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

    // The node menu: `t a`, which named the Artifacts tab before this change.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("a");
    expect(await openMenu(page).count(), "t a opens no menu").toBe(1);
    expect(await focusInside(openMenu(page)), "t a does not put the focus in the node menu").toBe(
      true
    );
    expect(
      await nodeMenuButton(page).count(),
      `the menu t a opened is not the one named "${CONTROL_LABELS.other_nodes}" beside the presets`
    ).toBe(1);

    // From the menu `t a` left the focus in, Enter opens the document on the focused item in
    // place of the outline, and Escape - the key table's step after leaving a caret - closes it
    // and draws the outline again.
    await page.keyboard.press("Enter");
    await expect(
      openedArtifact(page),
      "Enter on the node menu's item opens no document"
    ).toHaveCount(1);
    expect(
      await tree(page).count(),
      "the opened document is not drawn in place of the outline"
    ).toBe(0);
    await page.keyboard.press("Escape");
    await expect(tree(page), "Escape does not close the opened document").toBeVisible();
    expect(await openedArtifact(page).count(), "Escape leaves the document drawn").toBe(0);
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

    // A document opened from the node menu, with the caret in its text: the caret is left
    // first, and the next Escape closes the document, back to the filter it was opened from.
    await nodeMenuButton(page).click();
    await openMenu(page).getByRole("button", { exact: true, name: spec.name }).click();
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

    // Then the filter: every filter off, with the reader on the same node of the outline.
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
    await expectActivePreset(page, PRESET_NAMES.conversation);
    await expect.poll(() => focusedRow(page), { message: "the reader left the outline" }).toBe(on);

    // Then the outline itself.
    await page.keyboard.press("Escape");
    await expect
      .poll(() => focusedRow(page), { message: "Escape did not leave the outline" })
      .toBe(null);
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
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
// @pr: The live socket: a change committed while the page loads has to be replayed, with no reload.
test("a change committed while the page loads is shown without a reload @pr", async ({
  browser,
}) => {
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
    const pending = trackApiRequests(page);
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
    await waitForReads(pending);
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
  // all of them have (`features/issue/useIssueTree.ts`), where before it asked for the
  // addresses known so far and asked again for the rest. The tree's asks, comments and messages
  // are read under the keys the margin already owns, so they are shared rather than asked twice
  // and are the entries already here.
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
    // The active-sessions read.
    "/api/v1/agents",
    // The primary artifact's comments, which the margin and the tree share rather than fetching
    // again.
    `/api/v1/issues/${issue.key}/comments`,
    // The margin's own list of the issue's asks, which the tree shares. The panel's conversation
    // list asked the same route for each ask's thread (`?thread=1`) as a request of its own; the
    // outline draws an ask's replies from the comments above, and that list is gone (R14).
    `/api/v1/issues/${issue.key}/asks?state=all`,
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
    // The header's read of the repository mappings: an issue that resolves to no repository,
    // as this one does, shows the `Repository:` item only when its project maps one.
    "/api/v1/settings/repo-projects",
    // me/agents/state, whose unread replies the Agents badge counts on every page (the
    // sidebar's on desktop, the compact header's on phone).
    "/api/v1/me/agents/state",
    // The credential requests waiting on the viewer, which the Inbox count adds up on every
    // page.
    "/api/v1/credential-requests?approver=me",
    // `me/replies?kind=reply,agent&limit=1`, whose total is the other half of that one Inbox
    // number (the sidebar's on desktop, the compact header's on phone).
    "/api/v1/me/replies?kind=reply%2Cagent&limit=1",
    // No list of the issue's phase handoffs: the list drew them beside the messages, and they
    // are lines of the history now, read with its log.
    // The reader's own state on this issue, which the unread mark and the pin read.
    `/api/v1/me/issues/${issue.key}/state`,
    // The tree's own reads, and the blocks of the one document the issue carries, which are
    // the nodes the tree draws of it. The document's *text* is no read of a first load:
    // nothing on load parses stored markdown any more (dispatch://INFRA-1196 R20). The head's
    // three sentences were read out of the spec's own text at the summary block's span, and
    // R20 replaced both reads with the summary node the tree already carries
    // (`features/issue/IssueThreeThings.tsx`); the editor that reads a document's text is
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
  // asks (`features/issue/useIssueTree.ts`), where before it asked for the addresses known so
  // far and asked again for the rest, so the count is the assertion that rule needs. It is
  // counted once the page has gone quiet, not at the moment the set above first matched: that
  // moment is the first batch's own, so a second batch sent after the reads answer would land
  // after it and never be counted.
  await expectLoadSettled(settling);
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
// @pr: The phone conversation's first paint: the question waiting on the reader is on the first screen whichever of its reads answers last.
test("on a phone, the conversation opens with its open question in view whichever read answers last @phone @pr", async ({
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

// @pr: The ask-part reference's only reading row: a link to an option opens the ask at that part.
test("a reference to an ask's option in a comment is a link that opens the ask at that part @pr", async ({
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
  // and its description.
  const optionText = `Wait — Hold until dispatch://${target.key} ships.`;

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
  await expect(references.getByText(optionText, { exact: true })).toBeVisible();
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
  // @pr: The bare-address family's one row on the real stack: a link written through the API must read as a link in the document and in a comment.
  test("after punctuation or an escaped scheme colon @pr", async ({ browser }) => {
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

/** Picks `file` in the node menu's own upload control and confirms it, as a person does:
 *  choosing a file only stages it until the staged row's "Upload" is pressed. */
async function uploadFromMenu(page: Page, file: UploadFile): Promise<void> {
  await page.getByLabel(CONTROL_LABELS.upload_file).setInputFiles(file);
  await page.getByRole("button", { exact: true, name: "Upload" }).click();
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

/** Opens the issue's node menu at the address the Artifacts tab answered, which opens the menu
 *  now: the menu took the tab's list (dispatch://INFRA-1196 R3), one item per file with the
 *  download of its latest version, and the file's versions and picture are on its own page. */
async function openNodeMenu(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/artifacts`);
  await expect(page.getByRole("menu")).toBeVisible();
}

// SLOW(>=2s): a cold load of the issue and its node menu, then the artifact's page and its whole
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

  await openNodeMenu(page, issue.key);
  // The item draws no picture: the file's picture is on its own page, read below.
  const item = menuItem(page, "diagram.png");
  await expect(item.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "href",
    versionAddress(diagram.id, 2, true)
  );

  await item.getByRole("button", { exact: true, name: "diagram.png" }).click();
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
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  await versions.getByRole("button", { name: "Show all versions" }).click();
  for (const number of [1, 2]) {
    await expect(
      versions.getByRole("link", { name: `Download version ${number}` })
    ).toHaveAttribute("href", versionAddress(diagram.id, number, true));
  }
});

// SLOW(>=2s): two uploads through the node menu, each written and listed, and four downloads the
// browser saves from the menu, the artifact's page and its version list.
// @webkit @firefox: the name the engine saves from `filename*` and `filename`, and whether it asks
// the server again for a download address it already followed: version 1's address saves a new
// name once version 2 exists.
test("a file saves under its own name, and its older version under its number once a newer one exists @webkit @firefox", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Report downloads" });
  await openNodeMenu(page, issue.key);
  const item = menuItem(page, "report.pdf");

  await uploadFromMenu(page, pdf("report.pdf", "first draft"));
  const first = await saveFrom(page, item.getByRole("link", { name: "Download version 1" }));
  expect(first.suggestedFilename()).toBe("report.pdf");

  await uploadFromMenu(page, pdf("report.pdf", "second draft"));
  await expect(item.getByRole("link", { name: "Download version 2" })).toBeVisible();
  await item.getByRole("button", { exact: true, name: "report.pdf" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/report-pdf`);
  const shown = page.getByRole("region", { exact: true, name: "report.pdf version 2" });
  const current = await saveFrom(page, shown.getByRole("link", { name: "Download version 2" }));
  expect(current.suggestedFilename()).toBe("report.pdf");

  const versions = page.getByRole("region", { exact: true, name: "Versions for report.pdf" });
  await versions.getByRole("button", { name: "Show all versions" }).click();
  const older = await saveFrom(page, versions.getByRole("link", { name: "Download version 1" }));
  expect(older.url()).toBe(first.url());
  expect(older.suggestedFilename()).toBe("report-v1.pdf");
  const newest = await saveFrom(page, versions.getByRole("link", { name: "Download version 2" }));
  expect(newest.suggestedFilename()).toBe("report.pdf");
});

// SLOW(>=2s): a cold load of the issue and its node menu, an upload through it, and the download
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
  await openNodeMenu(page, issue.key);

  const name = "Q3 final résumé.pdf";
  await uploadFromMenu(page, pdf(name, "the quarter"));
  const saved = await saveFrom(
    page,
    menuItem(page, name).getByRole("link", { name: "Download version 1" })
  );
  // WebKit reads `filename*`, since the `é` stays where the ASCII `filename` has `_`, and then
  // writes each space as `_` (CI run 43750246861). Chromium and Firefox save the name exactly.
  const expected = browserName === "webkit" ? "Q3_final_résumé.pdf" : name;
  expect(saved.suggestedFilename()).toBe(expected);
});

// SLOW(>=2s): a cold load of the issue and its node menu, and a download the browser saves and
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
  await openNodeMenu(page, issue.key);

  const item = menuItem(page, specName);
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
// address replaced by the whole one. An agent wrote this comment, in a thread no person wrote in,
// so it lands in the history in Details, which marks its turn: part M's history marks a turn by
// `data-history-item` (`historyItem`), where today's page marks it `data-turn`.
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
    const turn = historyItem(page, anchored.id);
    await expect(turn).toHaveAttribute("aria-current", "true");
    await expect(turn).toContainText("Anchored on the first line.");
    await expect(historyPanel(page)).toBeVisible();
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
      await expectActivePreset(page, PRESET_NAMES[preset]);
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
