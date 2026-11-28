/** the rows upload documents and files, name their versions, read the references the server
 *  indexes, and read back what the server stored; an opened document's live socket and the
 *  page's own carry the server's frames */

/*
 * Piece 7 of part N (dispatch://INFRA-6480, of dispatch://INFRA-3940): the rows of Documents and
 * the document it opens, with its versions, its Rebuild and what it references; of the filter row,
 * the page's own words and its reflow at each control; of every address of today's page and an
 * address that moves the focus once; of the first screen and the first load's budget; and of
 * Escape and `t c` out of a view. Each reads the new page turned on (`withIssuePage`, through
 * `phase` or each row's own context). Each row but the one of Escape and `t c`, which is new, was
 * `e2e/issue-page-n.e2e.ts`'s, under its source's title; that file keeps part N's other rows. The
 * rows' helpers are `e2e/issue-page-n-documents.ts`'s.
 */

import type { Artifact } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { shortDay } from "../web/src/features/issue-page/node-time";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  ADDRESS_FALLBACK_WORDS,
  ANSWER_SOURCES,
  CONTROL_LABELS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  FILE_WORDS,
  GAP_LINES,
  NODE_REFERENCES_WORDS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  PRESET_ORDER,
  STATE_LINES,
  UNASKED_SOURCES,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { PAGE_SIZE, showMoreName } from "../web/src/features/issue-page/show-more-fixture";
import { blockNodeId } from "../web/src/features/issue-page/statement-rules";
import {
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createMessage,
  createNamedVersion,
  createProject,
  editArtifact,
  getArtifact,
  getArtifactBlocks,
  getArtifactText,
  getIssue,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "./api";
import {
  type BaselineSeed,
  seedNoChildren,
  seedRequestBaseline,
  seedTwiceTheNodes,
  seedWholeIssue,
  seedWholeIssueAdvancing,
  type WholeIssueSeed,
} from "./issue-layout-seed";
import { asPlanner, plannerActor } from "./issue-layout-seed-support";
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
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import { marginSlot, TWENTY_LABELS } from "./issue-page-h";
import {
  activePreset,
  childRows,
  childrenSection,
  FALLBACK_LINES,
  focusedNodeId,
  GONE_COMMENT,
  nodeBodies,
  onScreen,
  openEveryDeeperBranch,
  pickPreset,
} from "./issue-page-i";
import { drawnContentsEntries } from "./issue-page-j";
import { daemonOf, historyItem, historyPanel, rowFacts, stepsSection } from "./issue-page-m";
import {
  type AddressContent,
  type AddressSeed,
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
  expectFocusLandedOnce,
  filterBox,
  filterControl,
  filterRowLooks,
  filterRowReading,
  firstDocumentLink,
  identityBlock,
  logFocusMoves,
  nodeMenuButton,
  OLD_PAGE_ADDRESSES,
  offThePalette,
  openDocuments,
  openedArtifact,
  openedEditor,
  openFilterBox,
  openIssueActions,
  openPageState,
  openVersionControl,
  PART_ADDRESSES,
  REFLOW_STATES,
  rowSelector,
  rowsWithoutAuthorScrollers,
  scrollersOutsideAuthorText,
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
} from "./issue-page-n-documents";
import { withIssuePage } from "./issue-page-option";
import { IPHONE_13, MEASURED_TITLE, viewportAt } from "./issue-page-p";
import { expectThreadsDrawn } from "./issue-page-threads";
import { sql } from "./psql";
import {
  type Browser,
  type BrowserContext,
  test as dispatchTest,
  expect,
  type Locator,
  type Page,
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

// Ruling 5 of dispatch://INFRA-1196/artifact/build-rulings-md, "The filter row": one line at
// every width, from 320 px to 1920 px. Of its eight controls Documents stands after Threads, in
// the row and in the one control below 1280 px; the two small controls, the version control (with
// a view on or a document open) and the filter by words, stand at its right end before Keys from
// 1024 px, each drawn as Keys is, and the words box opens from its control. Below 1024 px, with a
// view on, they stand in the Outline sheet the Outline control opens; on Documents, and with a
// document open, the page draws no sheet and the version control stands at the row's end. Each
// control and the open box is at least 24 px tall, the row is drawn in the page's colours alone,
// and `t a` and `t f` open Documents' list with the focus on its first row and the box with the
// caret in it (part N's R4).
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
      await page.setViewportSize(viewportAt(width));
      for (const preset of ["conversation", "spec", "documents"] as const) {
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
          await expect(outlineControl(page), `no Outline control at ${width} px`).toBeVisible();
        }
        // `expectActiveControl` above waited for the view's controls, which stand inside the
        // filter row, so the row is drawn and one read finds it.
        const row = await filterRowReading(page);
        if (row === null) throw new Error(`the page draws no filter row ${at}`);
        console.log(`filter row ${at}: ${JSON.stringify(row)}`);
        const wrapped = `the filter row is not one line ${at}: ${row.controls.join(" | ")}`;
        expect(row.lines, wrapped).toBe(1);
        expect(row.overflow, `the filter row runs past its own edge ${at}`).toBeLessThanOrEqual(1);
        // 35 px for the row of eight, drawn from 1280 px, and 8 px above and below the 34 px one
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
        // Documents stands after Threads: among the eight controls from 1280 px, and in the one
        // control's menu below that; then, in the row's order, what the sheet leaves in it.
        const head = width >= 1280 ? names : [expect.any(String)];
        const rest = inSheet ? [CONTROL_LABELS.outline] : [...version, ...filter];
        expect(row.controls, `the filter row's controls are not the picture's ${at}`).toEqual([
          ...head,
          ...rest,
          ...keys,
        ]);
        if (width >= 1024) {
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
          // Documents is an entry of the one control, after Threads.
          await presetChooser(page).click();
          const items = openMenu(page).getByRole("menuitemradio");
          await expect(
            items,
            `the one control's menu does not hold the row's ${names.length} controls ${at}`
          ).toHaveCount(names.length);
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

/** Every string `value` holds, however deeply, but that each line of it cut where the issue's
 *  own key goes (`{ before, after }`) is one string, with `key` in its place. */
function linesWithKey(value: unknown, key: string): string[] {
  if (typeof value !== "object" || value === null) return phrasesOf(value);
  if (
    "before" in value &&
    "after" in value &&
    typeof value.before === "string" &&
    typeof value.after === "string"
  ) {
    return [`${value.before}${key}${value.after}`];
  }
  return Object.values(value).flatMap((child) => linesWithKey(child, key));
}

// c-plain-words, and part N's R9 over INV-2's word states (`WORD_STATES`): each control of the
// view row on, Documents' list among them, and a document open, at 1440 px and at 390 px.
test("the page draws no internal word and no helper text @phone", async ({ browser }) => {
  // The check reads the page's chrome in INV-2's sixteen word states and in the body of every
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
    // What the page says only in answer to the reader's own act, read on each fresh load below.
    // `ADDRESS_FALLBACK_WORDS` cuts its lines where the issue's own key goes, and the page draws
    // each whole, `Back to <key>`, so its lines are read whole, with the seeded issue's key in
    // its place: a piece of one read alone is a piece of the page's own words too - `back` alone
    // is "Back to ", which part N's "Back to Documents" holds. Every other cut line is cut where
    // another name goes - a key the reader typed (`BLOCKED_BY_REFUSALS`), an author, a time, a
    // count of hours - which no load knows, so its pieces are read alone, as before.
    const answers = ANSWER_SOURCES.flatMap((source) =>
      source === ADDRESS_FALLBACK_WORDS ? linesWithKey(source, seeded.key) : phrasesOf(source)
    )
      .map((phrase) => phrase.trim())
      .filter((phrase) => phrase.length > 3);

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
    // The page opens folded to the frontier (R1): measured on this seed, the first drawing holds
    // half its comments, one of its three messages and none of its results, so a walk over that
    // drawing reads no result's body at all. So every branch is opened first, and every branch
    // below the drawn levels, as a reader opens them to read past the fold - once the page has
    // stopped reading, since a row that lands after the twisties were pressed lands folded - and
    // the rows are read against the seed's own count of every kind, so a kind that is still
    // folded, or not yet drawn, fails here rather than leaving its bodies out of the walk.
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

/** How many documents the row seeds so that Documents' list, which draws `PAGE_SIZE` rows at a
 *  time, has a second page of its own (root R40). */
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
    // 390 px. Dashboard and Threads are drawn in place of the outline, so there it is read once
    // Dashboard and its list, or Threads' own view, are drawn, with no outline to unfold.
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER.filter((one) => one !== "documents")) {
        const where = `the ${PRESET_NAMES[preset]} preset at ${size.width} px`;
        await gotoIssue(page, presetPath(seeded.key, preset));
        if (preset === "dashboard") {
          await expectDashboardDrawn(page, where);
        } else if (preset === "threads") {
          await expectThreadsDrawn(page, where);
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
      const newest = newestVersion(await getArtifact(seeded.verificationArtifactId));
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
    expect(versions.length, manyVersions).toBeGreaterThan(PAGE_SIZE);
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
    ).toHaveCount(PAGE_SIZE);
    await expect(showMoreControl(versionList)).toHaveText(
      showMoreName(versions.length - PAGE_SIZE)
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
    await openVersionControl(page);
    await expect(
      page.getByRole("combobox", { exact: true, name: CONTROL_LABELS.version }),
      "the document's versions are not reachable"
    ).toHaveCount(1);
    // The panel shuts by its own control, as a reader shuts it.
    await versionControlButton(page).click();
    await expect(versionControl(page)).toBeHidden();
    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.image}`);
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
    ).toHaveCount(PAGE_SIZE);
    await expect(showMoreControl(documentsList(page))).toHaveText(
      showMoreName(DOCUMENTS_SEEDED - PAGE_SIZE)
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
  // Nine states at seven widths, each a load of the page, with a clipping walk over every row a
  // view draws, each read in the browser, plus the page's own fit, Documents' list's, the open
  // document's, the sheet's and the panel's. Measured in the issue pod, alone: 1.6 min on
  // chromium and 1.7 min on iphone, which read the same seven widths; the bound is about four
  // times the pod's, for a slow leg.
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
      const at = `${stateName(state, width)} at ${width} px`;
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

/** The block id of the typed block each document of tasks here ends with - `addressSeed`'s, and
 *  decision 449's row's - and how many lines of the document stand before it. */
const TASKS_BLOCK = "t-far";
const TASKS_BEFORE_THE_BLOCK = 60;

/** The records INV-1's addresses are written with (`AddressSeed`): the whole issue's, and an
 *  agents' comment, a twin comment, a comment of another issue, an approval request, a document
 *  of tasks, a message of the Legion daemon and a handoff, written here beside it. */
async function addressSeed(seeded: WholeIssueSeed): Promise<AddressSeed> {
  const issue = await getIssue(seeded.key);
  const artifacts = issue.artifacts ?? [];
  const slugOf = (id: string): string => {
    const artifact = artifacts.find((one) => one.id === id);
    if (artifact === undefined) throw new Error(`${seeded.key} holds no document ${id}`);
    return artifact.slug;
  };
  const notes = artifacts.find((artifact) => artifact.slug === seeded.documents.cited);
  if (notes === undefined) throw new Error(`${seeded.key} holds no ${seeded.documents.cited}`);
  const notesWords = "The measurements behind the first requirement.";
  const [{ markdown }, blocks] = await Promise.all([
    getArtifactText(notes.id),
    getArtifactBlocks(notes.id),
  ]);
  const paragraph = blocks.find((block) =>
    markdown.slice(block.from, block.to).includes(notesWords)
  );
  if (paragraph === undefined) throw new Error(`${notes.slug} holds no paragraph of its words`);
  const specSlug = slugOf(seeded.specArtifactId);
  const planSlug = slugOf(seeded.planArtifactId);
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
    daemonOf(seeded.key.slice(0, seeded.key.indexOf("-")))
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
        page.getByRole("button", { name: CONTROL_LABELS.retry }),
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
 *  row of its own here, by the slug of its primary document (`primarySlug`), and the caller
 *  reads the load's settle after them. */
async function expectTheSeedIsDrawn(page: Page, seeded: BaselineSeed, slug: string): Promise<void> {
  // No row for the issue itself: the outline draws none (ruling 5, part N's c-addresses (4)).
  for (const requirement of seeded.requirements) {
    await expect(
      nodeRow(page, blockNodeId(slug, requirement)).first(),
      `the tree draws no row for ${requirement}, so the page did not load what the count is of`
    ).toBeVisible();
  }
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
    const slug = await primarySlug(seeded.key);
    const traffic = watchApiRequests(page);
    traffic.reset();
    // Conversation's own address, whose load reads the tree: the bare address draws Dashboard
    // (dispatch://INFRA-5702), whose own cold load is read below.
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await expectTheSeedIsDrawn(page, seeded, slug);
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
    await expectTheSeedIsDrawn(page, seeded, slug);
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
    const slug = await primarySlug(seeded.key);
    const traffic = watchApiRequests(page);
    traffic.reset();
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await expectTheSeedIsDrawn(page, seeded, slug);
    await expectLoadSettled(traffic);
    const byRoute = JSON.stringify(traffic.byRoute());
    expect(
      traffic.total(),
      `twice the nodes cost ${traffic.total()} requests where the baseline cost ${baseline?.total}: ${byRoute} against ${baseline?.byRoute}`
    ).toBe(baseline?.total);
  });
});

// The not-found page (`deep-links.e2e.ts:1170` on main reads "Ask not found"): the page's word for
// an ask is "question" (R34), and the article follows the noun.
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

// R14 (INV-4, `VIEWS_FOCUS`): under every view the row that holds the focus is part F's focused
// node, as the outline's row is, so Escape or `t c` turns the view off and leaves that node
// focused in the outline, wherever `j` and `k` moved the focus in the view.
// SLOW(>=2s): eight loads of the issue at 1440 px, one for each of the four views and each of the
// two ways it is left, with three moves of the focus in each.
test("Escape and t c turn a view off and leave the node the reader was on focused in the outline", async ({
  browser,
}) => {
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
        await page.locator('[data-node-row][tabindex="0"]').first().focus();
        let at = await focusedNodeId(page);
        for (const key of ["j", "j", "k"]) {
          const from = at;
          await page.keyboard.press(key);
          await expect
            .poll(
              async () => {
                at = await focusedNodeId(page);
                return at !== null && at !== from;
              },
              { message: `${view}: ${key} did not move the focus to another row` }
            )
            .toBe(true);
        }
        if (leave === "Escape") {
          await page.keyboard.press("Escape");
        } else {
          await page.keyboard.press("t");
          await page.keyboard.press("c");
        }
        await expect(
          page.locator(`[data-issue-body] ${rowSelector(at ?? "")}`).first(),
          `${leave} from ${view} leaves no row of ${at} focused in the outline`
        ).toBeFocused();
      }
    }
  });
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
  try {
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
    // taken the moment that message appears can be one request short. The wait ends on the set
    // itself, so a panel that starts eagerly fetching before its own address is ever opened still
    // trips this immediately instead of only breaking some looser upper bound, and a failure names
    // the address that is missing or extra instead of leaving a reader to work out which of a
    // count of N dropped.
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
    // all of them have (`features/issue-page/useIssueTree.ts`). The tree's asks, comments and
    // messages are read under the keys the margin already owns, so they are shared rather than
    // asked twice and are the entries already here.
    //
    // It is the source's set (`750e2716`'s `e2e/live.e2e.ts`) less one address and with two
    // more, and no part on `main` adds one to this load. Part L's three are in it, as in the
    // source: the head read and the reader's state, which the read mark at the outline's end
    // writes, and the active-sessions read, which the box at the foot makes (`slots/foot.tsx`).
    // The one less, the repository mappings, `main`'s page no longer reads. The two more are the
    // reads of dispatch://INFRA-5702's list of what waits on the reader, a part this branch
    // holds, which the header's count counts (R5). Each is named where it stood or stands.
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
      // The active-sessions read of the box at the foot of the page, whose mention picker lists
      // the live agents (`conversation/MentionComposer.tsx`'s `useAgents`, part L's
      // `IssueComposer.tsx`).
      "/api/v1/agents",
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
      // The issue's head, which the read mark at the outline's end writes when the reader has
      // seen it (`IssueReadMark.tsx`, part L's R9).
      `/api/v1/issues/${issue.key}/events?limit=1&order=desc`,
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
      // The read mark's write of the reader's own state on this issue
      // (`PUT /api/v1/me/issues/<key>/state`), sent once the outline's end has been on screen for
      // a second, through the head read above (`IssueReadMark.tsx`'s `last_read_seq`): the
      // seed's five messages are carol's, so the issue is unread for alice, and its short outline
      // ends on her first screen.
      `/api/v1/me/issues/${issue.key}/state`,
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
    // The list of references the batched read names is not a fact of the budget, so it is dropped
    // here.
    const seen = () =>
      [
        ...new Set(
          apiRequestPaths.map((path) =>
            path.replace(/^\/api\/v1\/reference-targets\?.*$/, "/api/v1/reference-targets")
          )
        ),
      ].sort();
    await expect.poll(seen).toEqual(firstLoad);

    // The batch is one request, which the set above cannot say: it holds the address once
    // however many times the page asked for it. The tree waits for all of its reads before it
    // asks (`features/issue-page/useIssueTree.ts`), so the count is the assertion that rule needs.
    // It is counted once the page has gone quiet, not at the moment the set above first matched:
    // that moment is the first batch's own, so a second batch sent after the reads answer would
    // land after it and never be counted.
    await expectLoadSettled(settling);
    // The set again, once quiet: an address first read after the set above matched is no less a
    // read of the first load.
    expect(seen(), "the first load read another address once its set had matched").toEqual(
      firstLoad
    );
    expect(
      apiRequestPaths.filter((path) => path.split("?")[0] === "/api/v1/reference-targets"),
      "the page read /api/v1/reference-targets more than once on a first load"
    ).toHaveLength(1);
  } finally {
    await alice.close();
  }
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

// Decision 449 of dispatch://INFRA-1196/artifact/part-decisions-md (c-reference-block): a
// reference to a block of a document the outline does not draw opens that document on the block.
// The issue holds `plan-one-plan.md`, a document of tasks uploaded as an ordinary document and not
// as the issue's plan, so the outline places none of its tasks (`tree-drafts.ts`'
// `servePlacements` places the tasks of the plan alone); its one task, far down its text, serves a
// requirement of the spec, which the outline draws. From the requirement's row, part M's "What
// references this row" lists the task, named by the first line of its words (part M's decision
// 560), and the task's row opens the address of its block: the document in the main column with
// its head, and the task's block on screen, at 1440 px and at 390 px. A block of a document the
// outline draws is part M's own check.
// SLOW(>=2s): a load of the issue at each width, with Details and its list opened, and the
// document opened from the list.
test("a reference to a block of a document the outline does not draw opens that document on the block @phone", async ({
  browser,
}) => {
  const taskWords = "Draw the task that a reference to it opens on.";
  await phase(
    browser,
    async () => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({
        project: "CORE",
        spec: ":::requirement{#r-served}\nA reference to a task opens on the task.\n:::\n",
        title: "A task of a document that is no plan",
      });
      const served = `dispatch://${issue.key}/spec#b-r-served`;
      const lines = [
        "# Tasks",
        ...Array.from(
          { length: TASKS_BEFORE_THE_BLOCK },
          (_, index) => `Step ${index + 1} of the tasks, in a line of its own.`
        ),
        `:::plan_task{#${TASKS_BLOCK} serves="${served}"}\n${taskWords}\n:::`,
      ];
      const tasks = await createIssueArtifact(issue.key, {
        content: `${lines.join("\n\n")}\n`,
        name: "plan-one-plan.md",
      });
      const spec = await getArtifact(issue.primary_artifact_id ?? "");
      return {
        address: `/issues/${issue.key}/artifacts/${tasks.artifact.slug}#b-${TASKS_BLOCK}`,
        key: issue.key,
        name: tasks.artifact.name,
        statement: blockNodeId(spec.slug, "r-served"),
        task: blockNodeId(tasks.artifact.slug, TASKS_BLOCK),
      };
    },
    async (page, seeded) => {
      const desktopProject = test.info().project.name !== "iphone";
      const opened = page.getByTestId("issue-column").getByTestId("opened-artifact");
      for (const size of desktopProject ? [DESKTOP, PHONE] : [PHONE]) {
        const where = `at ${size.width} px`;
        await page.setViewportSize({ height: size.height, width: size.width });
        const facts = await rowFacts(page, seeded.key, seeded.statement);
        await expect(
          nodeRow(page, seeded.task),
          `the outline draws a row of the task, so its document is one the outline draws ${where}`
        ).toHaveCount(0);
        const references = facts.locator("[data-node-references]");
        await references
          .getByRole("button", { exact: true, name: NODE_REFERENCES_WORDS.what_references })
          .click();
        const task = references.getByRole("link", { exact: true, name: taskWords });
        await expect(
          task,
          `what references the requirement draws no row of the task that serves it ${where}`
        ).toHaveAttribute("href", seeded.address);
        await task.click();
        await expect(
          page,
          `the task's row does not open the address of its block in its document ${where}`
        ).toHaveURL(seeded.address);
        const block = openedEditor(page).locator(`[data-block-id="${TASKS_BLOCK}"]`);
        await expect(
          block,
          `the document did not open on the referenced block ${where}`
        ).toBeInViewport();
        await expect(
          block,
          `the block the document opened on does not hold the task's words ${where}`
        ).toContainText(taskWords);
        await expect(
          opened.getByRole("heading", { exact: true, level: 2, name: seeded.name }),
          `the main column draws the document with no head naming it ${where}`
        ).toBeVisible();
        await expect(
          documentsList(page),
          `Documents' list is drawn in place of the document ${where}`
        ).toHaveCount(0);
        await expect(
          tree(page),
          `the outline is drawn beside the document the task's row opened ${where}`
        ).toHaveCount(0);
      }
    }
  );
});
