import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import {
  CHECK_WHEN_WORDS,
  CONTROL_LABELS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  type NodeType,
  nodeStateWords,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
  wordsIn,
} from "../web/src/features/issue/page-words";
import { getUnsubscribeCalls } from "./agents";
import {
  answerAsk,
  claimIssue,
  createProjectDocument,
  getArtifact,
  getArtifactVersion,
  getAsk,
  getIssue,
  getIssueArtifactText,
  getMyState,
  listComments,
  patchIssue,
  requestApproval,
} from "./api";
import {
  type BaselineSeed,
  seedFrontierDeep,
  seedFrontierShallow,
  seedNoChildren,
  seedNoSummary,
  seedReplyNesting,
  seedRequestBaseline,
  seedSpineCases,
  seedTwiceTheNodes,
  seedUnbrokenChildTitle,
  seedUnbrokenWords,
  seedWholeIssue,
  UNBROKEN_PATH,
} from "./issue-layout-seed";
import { asPlanner } from "./issue-layout-seed-support";
import {
  checkBlockId,
  OPEN_QUESTION_BLOCK_ID,
  planTaskBlockId,
  requirementBlockId,
  requirementText,
  SPINE_ANSWERED_QUESTION,
  SPINE_CHECK_BLOCK_ID,
  SPINE_REQUIREMENTS,
  SUMMARY_SENTENCES,
} from "./issue-layout-seed-text";
import {
  activePreset,
  addControl,
  ancestorsOfRow,
  badgeValue,
  blockNodeId,
  boxesPastTheirCard,
  branchMoreRows,
  checksRow,
  childRow,
  childRows,
  childrenBar,
  childrenDirectCount,
  childrenDisclosure,
  childrenSection,
  childrenSubtreeCount,
  chromeTexts,
  conversationStateControl,
  copyCommandControls,
  type DrawnRow,
  drawnRows,
  filterBox,
  focusedNodeFacts,
  focusedNodeId,
  focusThePage,
  gapKinds,
  gaps,
  gotoIssue,
  identityBlock,
  identityBlockBottom,
  issuePage,
  marginHideControl,
  marginResizeHandle,
  marginSection,
  marginSectionDisclosure,
  marginShell,
  marginShowControl,
  marginSlot,
  nodeBodies,
  nodeCommands,
  nodeMenuButton,
  nodeMenuItems,
  nodeRow,
  nodeRows,
  offered,
  openEveryDeeperBranch,
  openIssueActions,
  openMenu,
  openPreset,
  optionsOf,
  overlappingTexts,
  pageStatus,
  panelsRightOfMain,
  parentOfRow,
  pinnedBodies,
  planRow,
  presetAddress,
  presetChooser,
  presetCount,
  presetMenuItem,
  presetRow,
  presetTab,
  presetTablist,
  presetTabs,
  presetToolbar,
  rowAuthor,
  rowBody,
  rowFirstLine,
  rowGaps,
  rowPictures,
  rowsOfType,
  rowsWithoutAuthorScrollers,
  scrollersOutsideAuthorText,
  threeThings,
  threeThingsSentences,
  tooltipTexts,
  tree,
  treeText,
  undividedTexts,
  unfoldEveryBranch,
  uploadInput,
  versionPicker,
} from "./issue-page";
import { assertInventoryCarried, type InventoryEntry } from "./page-inventory";
import { resetDatabase } from "./seed";
import { type ApiRequests, watchApiRequests } from "./traffic";
import { assertFitsItsBox, assertPageFits, assertWhole, assertWholeAcross } from "./unclipped";
import { asUser } from "./users";

/**
 * The redesigned issue page (dispatch://INFRA-1196), one test per `check` block of
 * `dispatch://INFRA-1196/artifact/verification-md`, each named by that check's own `-g` string.
 * Every test reads the page through `e2e/issue-page.ts`, which is the one statement of what the
 * page's DOM is, and spells no selector of its own.
 *
 * Four checks are partial on today's data, and each test says where it stops:
 *
 * - **c-tree** proves the fold at the depth today's data reaches (issue, document, block); the
 *   four-levels-down seed waits on dispatch://INFRA-1290.
 * - **c-three-things** proves the three sentences and the two rows; who wrote the bullets and
 *   when they were last read back wait on dispatch://INFRA-1278, so the test asserts the page
 *   names that gap and never that an author is drawn.
 * - **c-home** proves the Spec preset over today's documents, where every requirement sits at the
 *   issue root because `from` waits on dispatch://INFRA-1098 item 21.
 * - **c-spine** proves what a document's own `serves` gives, because the model holds no `from`
 *   edge to seed.
 *
 * A seed makes the project `LAY`, and a second `POST /api/v1/projects` for it answers
 * `409 PROJECT_EXISTS`, so a test that reads two seeded issues runs each in a `phase` of its own:
 * the reset truncates `user_sessions`, so each phase signs in again (`packages/dispatch/AGENTS.md`,
 * "End-to-end tests").
 */

/** The two sizes every check names. */
const DESKTOP = { height: 900, width: 1440 } as const;
const PHONE = { height: 844, width: 390 } as const;

/** The title the fake Envoy gives the session that holds the claim and takes the route. */
const PLANNER_TITLE = "Planner (issue layout)";

/** One seeded issue, read by one signed-in page. The viewport starts at the desktop size in every
 *  project, because the `iphone` project runs each `@phone` test too and a test that asserts a
 *  desktop width sets it itself. */
async function phase<T>(
  browser: Browser,
  seed: () => Promise<T>,
  body: (page: Page, seeded: T) => Promise<void>
): Promise<void> {
  await resetDatabase();
  const seeded = await seed();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await body(page, seeded);
  } finally {
    await context.close();
  }
}

/** Fails unless the redesigned page is on screen, so every assertion after it reads the new page
 *  and not whatever else answered the address. Both readings are web-first, so a check made
 *  straight after a reload waits for the page's own chunk and its reads instead of racing them:
 *  the shell paints the frame from the address before the page is mounted (`page-frame.tsx`). */
async function expectIssuePageDrawn(page: Page): Promise<void> {
  await expect(
    issuePage(page),
    "the page draws no [data-issue-page] root, so this is not the redesigned issue page"
  ).toHaveCount(1);
  await expect(
    tree(page),
    'the page draws no role="tree" named "Issue tree" inside [data-issue-page]'
  ).toHaveCount(1);
}

/** How long nothing may be in flight before a load is counted finished. It is the recorded
 *  baseline's own window (dispatch://INFRA-1196/artifact/request-baseline-md: "between
 *  navigation and network idle (nothing in flight for 1500 ms)"). */
const LOAD_QUIET_MS = 1_500;

/** Fails unless the page has gone quiet, which is the moment a load's cost is read at. Measured
 *  on this pod, the same unchanged page counted 39, 35 and 38 requests when the count was taken
 *  at Playwright's `networkidle` instead: that window closes after 500 ms of quiet, which is
 *  less than a read waiting on another read's answer takes under load, so what it yields is
 *  whatever had arrived by then rather than what the load costs. */
async function expectLoadSettled(traffic: ApiRequests): Promise<void> {
  await expect
    .poll(() => traffic.quietFor(LOAD_QUIET_MS), {
      message: "the page never went quiet, so its first load has no settled cost",
      timeout: 30_000,
    })
    .toBe(true);
}

/** The slug of the issue's own spec, which is how its blocks are addressed. */
async function primarySlug(key: string): Promise<string> {
  const primary = ((await getIssue(key)).artifacts ?? []).find((artifact) => artifact.primary);
  expect(primary, `${key} carries no primary document`).toBeDefined();
  return primary?.slug ?? "";
}

function rowOf(rows: readonly DrawnRow[], id: string): DrawnRow | undefined {
  return rows.find((row) => row.id === id);
}

/**
 * Where the page's one right-hand panel is drawn, having first failed unless there is exactly
 * one of it. The shell's margin draws it while it keeps a column for it; wherever it keeps none
 * - at the width it is a sheet, and while the reader has collapsed it to its rail - the page
 * draws the same panel in its own column instead, so a collapse moves the panel rather than
 * taking the properties off the page (dispatch://INFRA-1196 R11, R12,
 * `web/src/features/margin/margin-slot.tsx`'s `useMarginDrawsSlot`). Two panels at once, or
 * none, fails here whichever state the margin is in.
 */
async function panelPlacement(page: Page, where: string): Promise<"column" | "margin"> {
  expect(await marginSlot(page).count(), `${where}: the page draws no one panel`).toBe(1);
  expect(
    await panelsRightOfMain(page),
    `${where}: the panel's sections are drawn in more than one place`
  ).toHaveLength(1);
  const inTheMargin = await marginShell(page).getByTestId("issue-margin").count();
  const inTheColumn = await page.locator("main").getByTestId("issue-margin").count();
  expect(
    inTheMargin + inTheColumn,
    `${where}: the panel is drawn neither in the shell's margin nor in the page's own column`
  ).toBe(1);
  return inTheMargin === 1 ? "margin" : "column";
}

/**
 * The element is inside the viewport sideways: its box starts at or after the left edge and
 * ends at or before the right one. This is the half of "in the viewport" that a column which
 * scrolls down by design can be held to - the issue page's right-hand panel holds the
 * properties, the focused node's facts and the whole conversation, so a value further down it
 * is below the fold and reached by scrolling, which R17 allows (it bans two dimensions, never
 * one). Sideways it allows nothing: the rail this redesign removed hid 744 px of the issue's
 * properties to the right of an inner scroller, and an element pushed out that way keeps its
 * box, its size and its text, so it is drawn still and only its place says it is gone.
 *
 * What this cannot read is a value hidden *below* the fold of a box that was never meant to
 * scroll; that is `assertWholeAcross`'s reading of the same element, which fails on a scroller
 * between it and the document.
 */
async function expectInsideTheViewportSideways(
  page: Page,
  at: Locator,
  where: string
): Promise<void> {
  const box = await at.boundingBox();
  const width = page.viewportSize()?.width ?? 0;
  expect(box, `${where} draws no box at ${width} px`).not.toBeNull();
  expect(
    Math.round(box?.x ?? -1),
    `${where} starts left of the ${width} px viewport`
  ).toBeGreaterThanOrEqual(0);
  expect(
    Math.round((box?.x ?? 0) + (box?.width ?? 0)),
    `${where} ends past the right edge of the ${width} px viewport`
  ).toBeLessThanOrEqual(width);
}

function idsOf(rows: readonly DrawnRow[]): string[] {
  return rows.map((row) => row.id ?? "<no id>");
}

/**
 * Every row carries its type's word and its type's shape, its state word and its state's shape
 * where the model holds a state, and nothing where it holds none. Every word and token comes from
 * `page-words.ts`, so a type or state the page draws without declaring one fails here.
 */
function expectRowVocabulary(rows: readonly DrawnRow[]): void {
  for (const row of rows) {
    const type = row.type ?? "";
    expect(
      Object.keys(NODE_TYPE_NAMES),
      `row ${row.id} carries data-node-type "${type}", which the model does not name`
    ).toContain(type);
    const typeName = NODE_TYPE_NAMES[type as NodeType];
    expect(row.text.toLowerCase(), `row ${row.id} does not say it is a ${typeName}`).toContain(
      typeName
    );
    const typeGlyph = GLYPH_TOKENS.types[type as NodeType];
    expect(
      row.glyphs.map((glyph) => glyph.token),
      `row ${row.id} draws no shape for its type (${typeGlyph})`
    ).toContain(typeGlyph);
    for (const glyph of row.glyphs) {
      expect(glyph.name, `the ${glyph.token} shape on row ${row.id} has no accessible name`).toBe(
        GLYPH_NAMES[glyph.token]
      );
    }

    const states = nodeStateWords(type as NodeType);
    if (states === undefined) {
      expect(
        row.state,
        `row ${row.id} is a ${typeName}, which the model gives no state, and it carries data-node-state "${row.state}"`
      ).toBeNull();
      expect(row.stateWord, `row ${row.id} draws a state word for a ${typeName}`).toBeNull();
      continue;
    }
    const state = row.state ?? "";
    expect(
      Object.keys(states),
      `row ${row.id} is a ${typeName} carrying data-node-state "${state}", which is no state of that type`
    ).toContain(state);
    expect(row.stateWord, `row ${row.id} draws no word for its state ${state}`).toBe(states[state]);
    const stateGlyphs: Readonly<Record<string, Readonly<Record<string, string>>>> =
      GLYPH_TOKENS.states;
    const stateGlyph = stateGlyphs[type]?.[state];
    expect(
      row.glyphs.map((glyph) => glyph.token),
      `row ${row.id} draws no shape for its state (${stateGlyph})`
    ).toContain(stateGlyph);
  }
}

/** The word and the shape each state is drawn with, one entry per state: a state told apart by
 *  colour alone collides with another here. */
function statesDrawn(rows: readonly DrawnRow[]): Map<string, string> {
  const drawn = new Map<string, string>();
  for (const row of rows) {
    if (row.state === null) continue;
    const token = row.glyphs.map((glyph) => glyph.token).join("+");
    drawn.set(`${row.type}:${row.state}`, `${row.stateWord} ${token}`);
  }
  return drawn;
}

/** A first line that ends inside a word: the stored text carries on without a break where the
 *  drawn line stops. */
function cutsAWord(stored: string, drawn: string): boolean {
  const line = drawn.replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
  if (!stored.startsWith(line) || stored === line) return false;
  return !/\s/.test(stored.charAt(line.length)) && !/[.!?]$/.test(line);
}

test.beforeEach(async () => {
  await resetDatabase();
});

/** How many rows the fold must leave undrawn on `seedFrontierShallow`, which is what says it
 *  reaches past the one branch the row below reads a count on: a page that folds nothing and a
 *  page that folds everything both satisfy a count of the rows drawn, and only this says how
 *  many rows the fold moved. Measured on 2026-11-12 on that seed at 1440 px: the page draws 7
 *  rows folded and 13 with every branch opened by hand, so the fold moves 6. The floor keeps a
 *  margin under that, so a seed that gains or loses a block or two does not re-open this
 *  number, while a page that folds nothing - the tree as it was before dispatch://INFRA-1196 R1
 *  - draws its whole seed at once and fails it. */
const FOLD_REACH_AT_LEAST = 4;

// c-tree. Partial on today's data: `document_blocks` holds no parent, so the tree reaches two
// levels (issue, document, block) and the four-levels-down seed is dispatch://INFRA-1290's. What
// is proved here is the fold itself, at the depth the data reaches, on both frontier seeds.
// This row reads the tree the whole of a width draws, so it carries no `@phone`: at 1024 px and
// below the tree draws two levels under a breadcrumb (`zoomedRows`) and every level, ancestor
// and fold below would be a different claim. The word clause it shares with that width is read
// there by the row under this one.
test("the body is one folded tree whose rows never truncate a word", async ({ browser }) => {
  await phase(browser, seedFrontierShallow, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    const specSlug = await primarySlug(seeded.key);

    const rows = await drawnRows(page);
    expect(
      rows.length,
      `the body is not one folded tree: ${seeded.key} draws no node row at all`
    ).toBeGreaterThan(0);
    await expectIssuePageDrawn(page);
    expect(
      rows.filter((row) => row.role !== "treeitem").map((row) => row.id),
      "these rows of the tree are no treeitem"
    ).toEqual([]);
    expectRowVocabulary(rows);

    // No row truncates a word: each first line is the start of the node's own text and the text
    // carries on at a word break, or the line ends a sentence.
    for (const row of rows) {
      if (row.firstLine === null) continue;
      const line = row.firstLine.replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
      expect(
        line.length,
        `row ${row.id} draws a first line of ${line.length} characters`
      ).toBeLessThanOrEqual(100);
      expect(row.text, `row ${row.id}'s first line is not its own text`).toContain(line);
    }
    const long = rowOf(rows, blockNodeId(specSlug, requirementBlockId(1)));
    expect(long, "the two-sentence requirement draws no row").toBeDefined();
    expect(
      cutsAWord(requirementText(1), long?.firstLine ?? ""),
      `the first line "${long?.firstLine}" cuts the stored text inside a word`
    ).toBe(false);

    // The folded branch: one document row, folded, counting what it holds, none of it drawn.
    const branch = rowOf(rows, seeded.foldedBranch.slug);
    expect(branch, `${seeded.key} draws no row for the folded document`).toBeDefined();
    expect(branch?.expanded, "the folded branch is not marked folded").toBe("false");
    expect(branch?.foldedCount, "the folded branch does not count what it holds").toBe(
      String(seeded.foldedBranch.blockIds.length)
    );
    expect(
      seeded.foldedBranch.blockIds.filter(
        (block) => rowOf(rows, blockNodeId(seeded.foldedBranch.slug, block)) !== undefined
      ),
      "the folded branch draws its blocks while it is folded"
    ).toEqual([]);

    // The frontier: the open question one level below the issue is drawn, unfolded to, with
    // nothing unfolded by hand.
    const question = rowOf(rows, seeded.askId);
    expect(question, `${seeded.key} draws no row for its open question`).toBeDefined();
    expect(question?.visible, "the open question's row is not drawn").toBe(true);
    expect(question?.level, "the open question does not sit one level below the issue").toBe(
      String(seeded.depth + 1)
    );

    // The focused node's body holds the whole text, which the first line is only the start of.
    const first = nodeRow(page, blockNodeId(specSlug, requirementBlockId(1))).first();
    await first.focus();
    expect(await rowBody(first).count(), "the focused row draws no body").toBe(1);
    expect(
      (await rowBody(first).innerText()).replace(/\s+/g, " "),
      "the focused node's body does not hold the node's whole stored text"
    ).toContain(requirementText(1));

    // How far the fold reaches, which the folded branch above does not say: that branch is one
    // branch, and a page that folds nothing and a page that folds everything both satisfy a
    // count of the rows drawn. So the first drawing is compared with the same seed drawn with
    // every branch opened by hand (`unfoldEveryBranch`, which is what a reader pressing each
    // twisty reaches), and the difference is how many rows the fold moved.
    const folded = rows.length;
    await unfoldEveryBranch(page);
    const whole = (await drawnRows(page)).length;
    expect(
      whole - folded,
      `the fold leaves ${whole - folded} rows undrawn: ${folded} of the seed's ${whole} rows are drawn folded`
    ).toBeGreaterThanOrEqual(FOLD_REACH_AT_LEAST);
  });

  await phase(browser, seedFrontierDeep, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const specSlug = await primarySlug(seeded.key);
    const question = blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID);

    const rows = await drawnRows(page);
    expectRowVocabulary(rows);
    const row = rowOf(rows, question);
    expect(row, "the open question as deep as the model reaches draws no row").toBeDefined();
    expect(row?.visible, "the open question's row is not drawn").toBe(true);
    expect(row?.level, "the open question does not sit two levels below the issue").toBe(
      String(seeded.depth + 1)
    );
    for (const ancestor of await ancestorsOfRow(page, question)) {
      expect(
        rowOf(rows, ancestor)?.expanded,
        `the question's ancestor ${ancestor} is folded, so its path was not unfolded`
      ).toBe("true");
    }

    // Every other branch is folded to its first level with a count of what it holds, and no
    // branch is deeper than the drawn levels on today's data.
    expect(
      rowOf(rows, seeded.foldedBranch.slug)?.foldedCount,
      "the other branch does not count what it holds"
    ).toBe(String(seeded.foldedBranch.blockIds.length));
    expect(
      await branchMoreRows(page).count(),
      "a branch is drawn as deeper than the three levels, which today's model cannot hold"
    ).toBe(0);
  });
});

// c-tree, at the width that decides the cut, which is a row of its own. The row above reads the
// whole tree - every level it names, every ancestor it walks and every fold it counts is the
// unzoomed tree's - and at 1024 px and below the tree draws two levels under a breadcrumb
// instead (`zoomedRows`), so that row cannot be the one that runs here. This reads the one
// clause the width changes: a row is one line below 1025 px (`ZOOMED_TREE_QUERY`), where the
// node's own first line is cut again to the characters the row's other words leave it
// (`NodeRow.tsx`'s `oneLineFirstLine`), and no cut of it ends inside a word (R1). It carries
// `@phone-only` beside `@phone` because the row above already reads the same clause against the
// same stored text wherever the tree draws the whole of itself, so no desktop project owns a
// second run of this one.
//
// It sets the width itself, as every row of this file does: `phase` starts every page at the
// desktop size in every project, the `iphone` project included, so the tag chooses which
// project owns the row and the viewport below is what puts the row on one line. A run of this
// row without that line reads the wrapped page and proves nothing: measured against the page
// before this change, built from 17507b5b1661, where it passed at 1440 px with the line's own
// box 658 px wide and `white-space: normal`.
test("a one-line row cuts the node's first line at a word @phone @phone-only", async ({
  browser,
}) => {
  await phase(browser, seedFrontierShallow, async (page, seeded) => {
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const specSlug = await primarySlug(seeded.key);
    const nodeId = blockNodeId(specSlug, requirementBlockId(1));
    // The zoomed tree draws the issue's children and the trail down to its open question, so
    // the two-sentence requirement is one zoom away: its document's own twisty opens it, which
    // is how a reader on a phone reaches a document's blocks (R17). A width that draws the
    // whole tree has the row already and opens nothing.
    if ((await nodeRow(page, nodeId).count()) === 0) {
      await nodeRow(page, specSlug)
        .first()
        .getByRole("button", {
          name: new RegExp(`^(?:${CONTROL_LABELS.fold}|${CONTROL_LABELS.unfold})$`),
        })
        .click();
      await expect(
        nodeRow(page, nodeId).first(),
        "the zoom into the spec drew no row for its first requirement"
      ).toBeVisible();
    }

    const rows = await drawnRows(page);
    expect(rows.length, `${seeded.key} draws no node row on one line`).toBeGreaterThan(0);
    for (const row of rows) {
      if (row.firstLine === null) continue;
      const line = row.firstLine.replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
      expect(row.text, `row ${row.id}'s first line is not its own text`).toContain(line);
    }
    const long = rowOf(rows, nodeId);
    expect(long, "the two-sentence requirement draws no row").toBeDefined();
    expect(
      cutsAWord(requirementText(1), long?.firstLine ?? ""),
      `the first line "${long?.firstLine}" cuts the stored text inside a word`
    ).toBe(false);
    expect(
      long?.firstLine?.endsWith("…"),
      `the first line "${long?.firstLine}" says nothing of the text it stops short of`
    ).toBe(true);

    // The three readings above are of the text, and the text alone cannot see the cut this
    // width makes: `text-overflow: ellipsis` paints an ellipsis and inserts no character, so a
    // page that laid the whole line out and let CSS hide its tail reads here exactly as a page
    // that cut the line before drawing it. Measured on the page without the cut, built from
    // 17507b5b1661: this row passed all three. So the box is read too, on the one row whose
    // line is long enough to overflow it: the line's own element holds every character it was
    // given, `scrollWidth` at its `clientWidth` (a pixel for rounding). A wider `scrollWidth`
    // is the page drawing more than the box holds and leaving the cut to CSS, which no word
    // boundary decided (R1).
    const line = rowFirstLine(nodeRow(page, nodeId).first());
    const box = await line.evaluate((element) => ({
      client: element.clientWidth,
      drawn: element.textContent ?? "",
      nowrap: window.getComputedStyle(element).whiteSpace === "nowrap",
      scroll: element.scrollWidth,
    }));
    // The row is on one line at all: a wrapped line fits its box by wrapping, so the reading
    // below would pass on any width that draws the whole tree (measured: 658 px of box and
    // `white-space: normal` at 1440 px).
    expect(box.nowrap, "the row is not drawn on one line, so this width reads nothing").toBe(true);
    expect(
      box.scroll,
      `the row drew "${box.drawn}" as ${box.scroll} px of text in a ${box.client} px box, so ` +
        "the page drew more than the row holds and left CSS to cut it"
    ).toBeLessThanOrEqual(box.client + 1);

    // What the cut left out is in the body the row opens on focus, whole (R2), so the reader
    // loses no word to it.
    const opened = nodeRow(page, nodeId).first();
    await opened.focus();
    expect(await rowBody(opened).count(), "the focused row draws no body").toBe(1);
    await expect
      .poll(async () => (await rowBody(opened).innerText()).replace(/\s+/g, " "), {
        message: "the focused node's body does not hold the node's whole stored text",
      })
      .toContain(requirementText(1));
  });
});

// c-focus-opens.
test("a body opens on focus and nothing opens on hover", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);

    // One body is drawn: the focused node's.
    await focusThePage(page);
    await page.keyboard.press("Tab");
    const focused = await focusedNodeId(page);
    expect(
      focused,
      "Tab put the focus on no row of the tree, so no body can open on focus"
    ).not.toBeNull();
    await expectIssuePageDrawn(page);
    expect(await nodeBodies(page).count(), "more than the focused node's body is drawn").toBe(1);
    expect(
      (await drawnRows(page)).filter((row) => row.body).map((row) => row.id),
      "the body drawn is not the focused node's"
    ).toEqual([focused]);

    // A node pinned open keeps its body when the focus leaves it.
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o pinned no body open").toBe(1);
    await page.keyboard.press("j");
    expect(
      await nodeBodies(page).count(),
      "the pinned body and the focused node's are not the two bodies drawn"
    ).toBe(2);
    await page.keyboard.press("k");
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o did not close the pinned body").toBe(0);

    // With the keyboard focus off the tree nothing is open, and the pointer opens nothing either.
    await focusThePage(page);
    expect(
      await nodeBodies(page).count(),
      "a body is drawn with the keyboard focus off the tree"
    ).toBe(0);
    const before = await treeText(page);
    const rows = nodeRows(page);
    const count = await rows.count();
    for (let index = 0; index < count; index += 1) {
      await rows.nth(index).hover();
      expect(
        await nodeBodies(page).count(),
        `the pointer over row ${index} opened a body with the focus elsewhere`
      ).toBe(0);
    }
    expect(await treeText(page), "the pointer crossing every row changed the tree's text").toBe(
      before
    );
    expect(
      await tooltipTexts(page),
      "the page carries tooltips, which a reader on a phone or on the keyboard never sees"
    ).toEqual([]);

    // Nothing is carried by colour: each state and type has a word and a shape no other carries,
    // and the reading holds with the colours forced. A greyscale filter is not the test - today's
    // three muted states already differ in luminance and would pass one.
    const drawn = await drawnRows(page);
    expectRowVocabulary(drawn);
    const states = statesDrawn(drawn);
    expect(
      [...new Set(states.values())].length,
      `two states are drawn the same way: ${JSON.stringify([...states])}`
    ).toBe(states.size);
    const types = new Map(drawn.map((row) => [row.type ?? "", row.glyphs[0]?.token ?? ""]));
    expect(
      [...new Set(types.values())].length,
      `two types draw the same shape: ${JSON.stringify([...types])}`
    ).toBe(types.size);
    await page.emulateMedia({ forcedColors: "active" });
    const forced = await drawnRows(page);
    expectRowVocabulary(forced);
    expect(
      [...statesDrawn(forced)],
      "the states are told apart differently once the colours are forced"
    ).toEqual([...states]);
  });
});

// c-presets.
test("the presets are queries over one tree and fit every width @phone", async ({ browser }) => {
  // The row walks all five presets - a cold load of the issue page each - and then every row
  // each preset draws, reading its ancestor chain out of the DOM one row at a time, before
  // measuring the two shapes at four widths. Measured at 30.9 s on an issue pod against
  // Playwright's 30 s default, so the row raced that default rather than failing on anything
  // it asserts. The deadline is no part of what the check claims, as it is not on the three
  // long rows below (`test.setTimeout` at the children section, the internal-word read and
  // the inventory walk).
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);

    // At 1280 px and above: a row of five tabs, each named by its preset and carrying its count.
    expect(await presetTabs(page).count(), "no row of five presets is drawn at 1440 px").toBe(5);
    expect(await presetTablist(page).count(), 'no role="tablist" named "Issue presets"').toBe(1);
    await expectIssuePageDrawn(page);
    for (const preset of PRESET_ORDER) {
      expect(
        await presetTab(page, preset).count(),
        `no tab is named exactly "${PRESET_NAMES[preset]}"`
      ).toBe(1);
      expect(
        await presetCount(page, preset).count(),
        `the ${PRESET_NAMES[preset]} preset draws no count of its own`
      ).toBe(1);
    }

    // Each preset is its own address, draws every match with the chain of ancestors above it, and
    // duplicates nothing.
    const chains = new Map<string, { chain: string; preset: Preset }>();
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetAddress(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await activePreset(page),
        `${presetAddress(seeded.key, preset)} does not open the ${PRESET_NAMES[preset]} preset`
      ).toBe(PRESET_NAMES[preset]);

      const rows = await drawnRows(page);
      const ids = idsOf(rows);
      expect(
        ids.filter((id, index) => ids.indexOf(id) !== index),
        `the ${PRESET_NAMES[preset]} preset draws a node twice`
      ).toEqual([]);
      const matches = rows.filter((row) => !row.context);
      expect(matches.length, `the ${PRESET_NAMES[preset]} preset matches nothing`).toBeGreaterThan(
        0
      );
      for (const row of rows) {
        const chain = (await ancestorsOfRow(page, row.id ?? "")).join(" > ");
        for (const ancestor of chain === "" ? [] : chain.split(" > ")) {
          expect(
            rowOf(rows, ancestor)?.role,
            `the ${PRESET_NAMES[preset]} preset draws ${row.id} without its ancestor ${ancestor} as a treeitem the arrows reach`
          ).toBe("treeitem");
        }
        // Nothing moves between presets: where two presets draw one node, it sits in the same
        // chain in both.
        const seen = chains.get(row.id ?? "");
        if (seen === undefined) {
          chains.set(row.id ?? "", { chain, preset });
          continue;
        }
        expect(
          chain,
          `${row.id} sits under "${seen.chain}" in ${PRESET_NAMES[seen.preset]} and under "${chain}" in ${PRESET_NAMES[preset]}`
        ).toBe(seen.chain);
      }
    }

    // The two shapes, and no sideways scroll at any width between them. Each shape is read
    // through `expect.poll` for the reason `assertPageFits` is (`e2e/unclipped.ts`): a resize
    // settles on two clocks, the engine applying the breakpoint the moment the viewport reaches
    // it and the preset row re-rendering a task or two later on the `useMediaQuery` state it
    // reads, so a reading taken the instant the viewport changes is of the shape the page is
    // leaving. The poll weakens nothing - a page settled on the wrong shape never passes, and
    // the failure at the timeout names both counts - and both counts are still asserted, so a
    // page drawing neither shape or both fails as it did.
    await gotoIssue(page, `/issues/${seeded.key}`);
    const presetShape = async () =>
      `${await presetTabs(page).count()}/${await presetChooser(page).count()}`;
    for (const width of [1440, 1280]) {
      await page.setViewportSize({ height: DESKTOP.height, width });
      await expect
        .poll(presetShape, {
          message: `at ${width} px the page draws no row of five, or one control beside it (tabs/controls)`,
        })
        .toBe("5/0");
      await assertPageFits(page);
    }
    for (const width of [1279, 1100]) {
      await page.setViewportSize({ height: DESKTOP.height, width });
      await expect
        .poll(presetShape, {
          message: `at ${width} px the page draws neither the row of five nor the one control (tabs/controls)`,
        })
        .toMatch(/^(5\/0|0\/1)$/);
      await assertPageFits(page);
      await assertWhole(presetRow(page));
    }
    for (const width of [1024, 390, 320]) {
      await page.setViewportSize({ height: PHONE.height, width });
      await expect
        .poll(presetShape, {
          message: `at ${width} px the row of five is still drawn, or the one control is missing (tabs/controls)`,
        })
        .toBe("0/1");
      await assertPageFits(page);
    }
    await presetChooser(page).click();
    expect(await openMenu(page).count(), "the one control opens no menu").toBe(1);
    for (const preset of PRESET_ORDER) {
      expect(
        await presetMenuItem(page, preset).count(),
        `the menu holds no item for the ${PRESET_NAMES[preset]} preset`
      ).toBe(1);
    }
  });
});

// c-spine. Partial on today's data: `requirement` carries no attributes, so no `from` edge exists
// to seed (dispatch://INFRA-1098 item 21). What is proved is what a document's own `serves`
// gives, plus the requirement whose origin is absent.
test("the spine is the reasoning and an unexplained node sits under the issue", async ({
  browser,
}) => {
  await phase(browser, seedSpineCases, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);

    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const unexplained = blockNodeId(specSlug, SPINE_REQUIREMENTS.unexplained);
    const check = blockNodeId(verificationSlug, SPINE_CHECK_BLOCK_ID);

    // A requirement whose origin is absent sits under the issue, drawn as a node nothing explains
    // rather than as an error: the page names that gap and the gap names its kind.
    expect(
      await parentOfRow(page, unexplained),
      "the requirement whose origin is absent sits under no node: the page draws no row for it"
    ).toBe(seeded.key);
    await expectIssuePageDrawn(page);
    expect(
      await gaps(page).count(),
      "the page names no gap at all, so nothing says the requirement is unexplained"
    ).toBeGreaterThan(0);
    expect(
      await rowGaps(nodeRow(page, unexplained).first()).count(),
      "the requirement nothing explains is drawn with no gap on it"
    ).toBeGreaterThan(0);
    expect(await gapKinds(page), "a gap the page draws names no kind").not.toContain("");

    // The check that serves two requirements is drawn under both, each row naming its other
    // parent, and both rows open the same block.
    const rows = await drawnRows(page);
    const drawnUnder = rows
      .map((row, index) => ({ index, row }))
      .filter((entry) => entry.row.id === check);
    expect(
      drawnUnder.map((entry) => entry.index).length,
      "the check that serves two requirements is not drawn under both of them"
    ).toBe(2);
    const parents = new Set<string | null | undefined>();
    for (const entry of drawnUnder) {
      expect(
        entry.row.text,
        "a transcluded check does not name the other requirement it sits under"
      ).toContain(CONTROL_LABELS.also_under);
      parents.add(await parentOfRow(page, check));
    }
    expect(
      [
        blockNodeId(specSlug, SPINE_REQUIREMENTS.servedTwiceA),
        blockNodeId(specSlug, SPINE_REQUIREMENTS.servedTwiceB),
      ].filter((requirement) => rowOf(rows, requirement) === undefined),
      "the two requirements the check serves are not both drawn"
    ).toEqual([]);

    const firstRow = nodeRow(page, check).nth(0);
    const secondRow = nodeRow(page, check).nth(1);
    await firstRow.focus();
    const fromFirstParent = (await rowBody(firstRow).innerText()).replace(/\s+/g, " ");
    await secondRow.focus();
    const fromSecondParent = (await rowBody(secondRow).innerText()).replace(/\s+/g, " ");
    expect(
      fromSecondParent,
      "the two parents open two copies of the block rather than the same block"
    ).toBe(fromFirstParent);

    // Every edge that is not the spine is a chip naming its target, found exactly once: the
    // decision's link to a requirement, and the answered question it names. Each is looked for
    // by the words the page draws for it, because that is what a chip draws: the target's own
    // words from the reference read, and its address in the short form `shortForm` writes -
    // never the target's id, which is a uuid for an ask and which R21 keeps off the page
    // (`web/src/features/issue/NodeRow.tsx`'s `Chip`).
    const chips = rows.flatMap((row) => row.chips);
    for (const target of [SPINE_REQUIREMENTS.shapedElsewhere, SPINE_ANSWERED_QUESTION]) {
      const naming = chips.filter((chip) => chip.includes(target));
      expect(
        naming.length,
        `the edge to ${target} is drawn ${naming.length} times as a chip, not once: ${JSON.stringify(chips)}`
      ).toBe(1);
    }
  });
});

// c-three-things. Partial on today's data: the node that holds the bullets, its author and
// `last_read_back_at` are dispatch://INFRA-1278, so the head names that gap and this test never
// asserts that an author is drawn.
test("the three things are at the head of every preset @phone", async ({ browser }) => {
  // Ten loads of the page - the five presets at each of two widths - each read for its head,
  // its boxes and the page's own draw. Measured on an issue pod's two CPUs across four whole
  // runs: 25.3 s to 29.1 s, against a default deadline of 30 s, so the row failed the run it
  // was slowest in rather than on anything it asserts. Four times the measurement leaves room
  // for a loaded box.
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        await gotoIssue(page, presetAddress(seeded.key, preset));
        expect(
          await threeThings(page).count(),
          `the ${PRESET_NAMES[preset]} preset draws no three things at ${size.width} px`
        ).toBe(1);
        await expectIssuePageDrawn(page);

        const head = await threeThings(page).boundingBox();
        const presets = await presetRow(page).boundingBox();
        expect(head, "the three things have no box").not.toBeNull();
        expect(presets, "the preset row has no box").not.toBeNull();
        expect(
          (head?.y ?? 0) + (head?.height ?? 0),
          `the three things are not above the presets at ${size.width} px`
        ).toBeLessThanOrEqual((presets?.y ?? 0) + 1);

        // The sentences are the summary node's words, which the batched reference read names
        // last of the page's reads, so the reading waits for them rather than racing that read.
        await expect
          .poll(
            async () =>
              (await threeThingsSentences(page).allInnerTexts()).map((sentence) =>
                sentence.replace(/\s+/g, " ").trim()
              ),
            {
              message: `the ${PRESET_NAMES[preset]} preset does not draw the spec's own three sentences at ${size.width} px`,
            }
          )
          .toEqual([...SUMMARY_SENTENCES]);
        expect(
          (await planRow(page).innerText()).replace(/\s+/g, " "),
          `the ${PRESET_NAMES[preset]} preset's head points at no Plan preset`
        ).toContain(PRESET_NAMES.plan);
        expect(
          (await checksRow(page).innerText()).replace(/\s+/g, " "),
          `the ${PRESET_NAMES[preset]} preset's head points at no Checks preset`
        ).toContain(PRESET_NAMES.checks);
      }
    }

    // Who wrote the bullets and when they were last read back wait on dispatch://INFRA-1278, so
    // the head says neither is known and draws no author at all.
    expect(
      await rowAuthor(threeThings(page)).count(),
      "the head names an author, which the model does not hold"
    ).toBe(0);
    expect(
      await rowGaps(threeThings(page)).count(),
      "the head says nothing about the author and the read-back it does not know"
    ).toBeGreaterThan(0);
  });

  await phase(browser, seedNoSummary, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      expect(
        await threeThings(page).count(),
        `a spec with no summary draws no row in that same place at ${size.width} px`
      ).toBe(1);
      await expectIssuePageDrawn(page);
      // The line waits on the spec's blocks read, which says whether the spec holds a summary
      // node at all: until it answers the head says nothing about one
      // (`web/src/features/issue/IssueThreeThings.tsx`), so the reading waits for the line.
      await expect(
        threeThings(page),
        "that row says something other than the page's own line for a spec with no summary"
      ).toContainText(EMPTY_STATE_LINES.spec_summary);
      expect(
        await threeThingsSentences(page).count(),
        "a spec with no summary draws sentences all the same"
      ).toBe(0);
    }
  });
});

// c-open-questions.
test("an open question is unfolded and on the first screen @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const question = blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID);

    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      // The head's own parts fill after the first paint, so a box read before they are drawn
      // measures a page no reader ever sees, in either direction: this page settles first and
      // reads once. The order below is the order the head stacks in, and the children section
      // is last because it is the part that fills last - 213 px of the head on this seed.
      await expectIssuePageDrawn(page);
      await expect(
        threeThings(page),
        `the head of the page is not drawn at ${size.width}x${size.height}`
      ).toHaveCount(1);
      await expect(
        childrenSection(page),
        `the children section is not drawn at ${size.width}x${size.height}`
      ).toHaveCount(1);
      // Drawn with its content, which is its rows where the section opens and its two counts
      // where it opens closed (R6, below `md`): either way the section has its height.
      await expect
        .poll(
          async () =>
            (await childRows(page).count()) > 0 || (await childrenDirectCount(page).count()) > 0,
          {
            message: `the children section is still empty at ${size.width}x${size.height}, so the head has not settled`,
          }
        )
        .toBe(true);
      await expect(
        nodeRow(page, question).first(),
        `the open question's row is not drawn at ${size.width}x${size.height}, so there is nothing settled to measure`
      ).toBeVisible();

      const rows = await drawnRows(page);
      const row = rowOf(rows, question);
      expect(
        row,
        `the open question draws no row at ${size.width}x${size.height} with nothing unfolded by hand`
      ).toBeDefined();
      const box = row?.box ?? null;
      expect(box, "the open question's row has no box").not.toBeNull();
      const headerBottom = (await identityBlockBottom(page)) ?? 0;
      expect(
        box?.y ?? -1,
        `the open question's row sits under the sticky identity block at ${size.width}x${size.height}`
      ).toBeGreaterThanOrEqual(headerBottom - 1);
      expect(
        (box?.y ?? 0) + (box?.height ?? 0),
        `the open question's row is below the first screen at ${size.width}x${size.height}`
      ).toBeLessThanOrEqual(size.height);
      for (const ancestor of await ancestorsOfRow(page, question)) {
        expect(
          rowOf(rows, ancestor)?.expanded,
          `the question's ancestor ${ancestor} is folded, so its path is not unfolded`
        ).toBe("true");
      }
    }

    // The next-question key moves to the next open question and says so when there is none.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(nodeRow(page, question).first()).toBeVisible();
    await focusThePage(page);
    await page.keyboard.press("q");
    expect(await focusedNodeId(page), "the next-question key did not reach the open question").toBe(
      question
    );
    await page.keyboard.press("q");
    expect(
      await pageStatus(page).count(),
      "with no further open question the key reports nothing"
    ).toBeGreaterThan(0);
    expect(
      wordsIn(await pageStatus(page).first().innerText()).filter(
        (word) => !DECLARED_WORDS.includes(word)
      ),
      "that report uses words the page does not declare"
    ).toEqual([]);

    // Those rows are not counted against the fold budget: with the question answered, the fold
    // closes its path and unfolds no other branch to spend what it freed.
    const withQuestion = idsOf(await drawnRows(page));
    const branch = await ancestorsOfRow(page, question);
    const outside = (ids: readonly string[]) =>
      ids.filter((id) => !branch.includes(id) && id !== question);
    const traffic = watchApiRequests(page);
    await answerAsk(seeded.openQuestion.askId, {
      expected_edited_at: null,
      selected: [],
      text: "1280 px.",
    });
    await page.reload();
    await expectIssuePageDrawn(page);
    // `expectIssuePageDrawn` is satisfied by the tree's own element, which is drawn empty and
    // fills as the reads land, and every reading below passes on a tree that holds nothing: it
    // draws no row for the question, it adds no row outside the question's path, and it draws
    // fewer rows than the tree that held the question. So the tree is waited for until the
    // page has stopped reading, which is what says it has every row it is going to draw, and
    // then read for drawing rows outside the question's path at all.
    //
    // The wait is not for a named row of another branch, because which rows a reload draws is
    // not which rows a cold load drew: the reader's own state decides which path is unfolded,
    // and on this seed the reload leaves the spec's summary row (`spec#b-summary`) folded
    // away, which is why the reading below is of what the fold added outside the path and
    // never of the whole set. The two ends of the branch are waited for by name, since the
    // tree draws them whichever path it unfolds: its root, and the document the question's own
    // block sat in. The question's own row is not waited for - the answer takes it off the
    // tree, which is the first reading below.
    await expectLoadSettled(traffic);
    const holder = branch[branch.length - 1] ?? seeded.key;
    await expect(
      nodeRow(page, seeded.key).first(),
      "the reloaded tree draws no row for the issue itself"
    ).toBeVisible();
    await expect(
      nodeRow(page, holder).first(),
      `the reloaded tree draws no row for ${holder}, which holds the question's own block`
    ).toBeVisible();
    const withoutQuestion = idsOf(await drawnRows(page));
    expect(
      outside(withoutQuestion).length,
      "the reloaded tree draws no row outside the question's own path, so the two readings below are read against nothing"
    ).toBeGreaterThan(0);
    expect(
      withoutQuestion,
      "the answered question's row is still drawn as an open question's path would be"
    ).not.toContain(question);
    expect(
      outside(withoutQuestion).filter((id) => !outside(withQuestion).includes(id)),
      "the fold changed outside the question's own path, so those rows were charged against it"
    ).toEqual([]);
    expect(
      withoutQuestion.length,
      "the tree draws as many rows without the open question as with it, so its path cost the fold nothing to prove"
    ).toBeLessThan(withQuestion.length);
  });
});

// c-children.
test("the children section opens each preset and collapses to its counts @phone", async ({
  browser,
}) => {
  // Two whole seeds and fifteen page loads, each one a cold load of the issue page: the work
  // is the check's own, not a page that got slower, and on the emulated-phone project it runs
  // within a second or two of the 30 s default. The repo's way of saying a test is long is to
  // name its own budget (`e2e/heavy-document-writes.e2e.ts`, `e2e/cli-body-cap.e2e.ts`).
  test.setTimeout(90_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // No seed claims a child, and the check reads who holds the running one, so this test puts a
    // child in progress and claims it: that is the state the assertion is read against.
    const running = seeded.children.keys[0] ?? "";
    await patchIssue(running, { status: "in_progress" });
    await claimIssue(running, asPlanner);
    const blocked = seeded.children.blocked;
    const blockerTitle = (await getIssue(running)).title;

    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        await gotoIssue(page, presetAddress(seeded.key, preset));
        expect(
          await childrenSection(page).count(),
          `the ${PRESET_NAMES[preset]} preset draws no children section at ${size.width} px`
        ).toBe(1);
        await expectIssuePageDrawn(page);
        const section = await childrenSection(page).boundingBox();
        const body = await tree(page).boundingBox();
        // Both boxes are read before they are compared: a section with no box answers `null`,
        // and `?? 0` would then read as "at the very top" and pass.
        expect(
          section,
          `the children section has no box in the ${PRESET_NAMES[preset]} preset at ${size.width} px`
        ).not.toBeNull();
        expect(
          body,
          `the tree has no box in the ${PRESET_NAMES[preset]} preset at ${size.width} px`
        ).not.toBeNull();
        expect(
          section?.y ?? 0,
          `the children section is not the first thing in the ${PRESET_NAMES[preset]} preset's content at ${size.width} px`
        ).toBeLessThanOrEqual((body?.y ?? 0) + 1);
      }
    }

    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    expect(await childRows(page).count(), "the open section lists no children").toBe(
      seeded.children.keys.length
    );
    for (const key of seeded.children.keys) {
      expect(
        await childRow(page, key).first().getAttribute("data-child-status"),
        `the row for ${key} does not name its status`
      ).toBe((await getIssue(key)).status);
    }
    expect(
      (await childRow(page, blocked).first().innerText()).replace(/\s+/g, " "),
      "the blocked child's row does not name the issue that blocks it"
    ).toMatch(new RegExp(`${running}|${blockerTitle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`));
    expect(
      (await childRow(page, running).first().innerText()).replace(/\s+/g, " "),
      "the running child's row does not say who holds it"
    ).toContain(PLANNER_TITLE);

    // Collapsed, the header keeps the bar and both counts, each labelled.
    await childrenDisclosure(page).click();
    expect(await childRows(page).count(), "the collapse left the child rows on the page").toBe(0);
    expect(await childrenBar(page).count(), "the collapsed header lost its completion bar").toBe(1);
    expect(
      (await childrenDirectCount(page).innerText()).replace(/\s+/g, " "),
      "the collapsed header's direct count carries no visible label"
    ).toContain(CONTROL_LABELS.children_done);
    expect(
      (await childrenSubtreeCount(page).innerText()).replace(/\s+/g, " "),
      "the collapsed header's subtree count carries no visible label"
    ).toContain(CONTROL_LABELS.subtree_done);
    expect(
      (await childrenDirectCount(page).innerText()).replace(/\s+/g, " "),
      `the direct count does not read one of ${seeded.children.keys.length} children done`
    ).toMatch(new RegExp(`1\\D+${seeded.children.keys.length}`));

    // The reader's choice is kept for the next issue they open. The section itself is read
    // first: the next issue draws one (its child is the issue this phase is about), so zero
    // rows mean the reader's collapse and not a section that is absent or not yet drawn.
    await gotoIssue(page, `/issues/${seeded.parentKey}`);
    await expectIssuePageDrawn(page);
    await expect(
      childrenSection(page),
      "the next issue draws no children section, so its rows say nothing about the collapse"
    ).toHaveCount(1);
    expect(
      await childRows(page).count(),
      "the next issue opens the children section this reader collapsed"
    ).toBe(0);
  });

  await phase(browser, seedNoChildren, async (page, seeded) => {
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetAddress(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await childrenSection(page).count(),
        `an issue with no children draws a children section on the ${PRESET_NAMES[preset]} preset`
      ).toBe(0);
    }
  });
});

// c-properties.
test("every property with a value is visible at every width @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const issue = await getIssue(seeded.key);
    // Every property the seed set, named one by one and read at the element that draws its
    // value. A reading of the page's own text would count text an inner scroller has pushed
    // out of sight - which is exactly how the rail this redesign removed hid 744 px of the
    // issue's properties - and a substring of that text ("11", "alice") can be met by another
    // value; so each value is read where it is drawn: for being drawn, for being where the
    // reader can see it, and for having nothing between it and the document that clips it
    // (`e2e/unclipped.ts`). Drawn is not enough on its own, because an element a scroller has
    // pushed past its edge keeps its box, its size and its text and is drawn still; what says
    // the reader can see it is where that box is. The block's values are on the first screen,
    // so they are read against the whole viewport; the panel's are read sideways only, which
    // is what a column that scrolls down by design can be held to
    // (`expectInsideTheViewportSideways`).
    const panel = marginSlot(page);
    const inTheBlock: readonly (readonly [string, Locator])[] = [
      [
        "the parent",
        identityBlock(page).getByRole("link", { exact: true, name: seeded.parentKey }),
      ],
      // The assignee is the badge its control draws, not any text saying `alice` inside the
      // state row: the control's own `select` holds an `option` with the same word.
      ["the assignee", badgeValue(page, `Assignee of ${seeded.key}`)],
      ["the first label", page.getByTestId("issue-labels").getByText("dispatch", { exact: true })],
      ["the second label", page.getByTestId("issue-labels").getByText("layout", { exact: true })],
      ["the third label", page.getByTestId("issue-labels").getByText("redesign", { exact: true })],
    ];
    // The panel is a column of its own that scrolls down by design, so what is read inside it
    // is the sideways half of both readings: its box inside the viewport sideways, and the
    // sideways half of the clipping walk (R17 bans two dimensions, never one). The repository
    // is read as the assignee is, at the badge its control draws, since its select and the
    // description beside it carry the same words.
    const inThePanel: readonly (readonly [string, Locator])[] = [
      ["the repository", badgeValue(page, `Repository of ${seeded.key}`)],
      ["the pull request", panel.getByRole("link", { name: /^#11\b/ })],
      [
        "a component",
        panel.getByTestId("issue-components").getByText("Issue panel", { exact: true }),
      ],
      [
        "the other component",
        panel.getByTestId("issue-components").getByText("Issue tree", { exact: true }),
      ],
    ];

    for (const width of [1440, 1280, 1024, 390]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      for (const [name, at] of inTheBlock) {
        await expect(
          at,
          `${name} is not drawn at ${width} px with no control opened`
        ).toBeVisible();
        await expect(
          at,
          `${name} is drawn outside the ${width} px viewport with no control opened`
        ).toBeInViewport();
        await assertWhole(at);
      }
      for (const [name, at] of inThePanel) {
        await expect(
          at,
          `${name} is not drawn at ${width} px with no control opened`
        ).toBeVisible();
        await expectInsideTheViewportSideways(page, at, `${name} at ${width} px`);
        await assertWholeAcross(at);
      }
      // The claim is the holder's own link, which `AgentName` draws the title in: a reading
      // by that text would find the link and the span around it both. It is read for being
      // drawn alone, because the chip shortens a long title with an ellipsis of its own
      // (`ClaimChip.tsx`), which is the page's choice and not a word lost.
      await expect(
        page.getByTestId("issue-claim").getByRole("link", { exact: true, name: PLANNER_TITLE }),
        `the claim is not drawn at ${width} px with no control opened`
      ).toBeVisible();
      await expectIssuePageDrawn(page);
      await assertPageFits(page);

      // The status, the priority, the assignee and the labels stay with the title, each read
      // at the element that draws it inside the block and never as a substring of the block's
      // own text: that text holds "alice" in the assignee control's own `option` list and
      // "dispatch" wherever another value of this seed spells it, so a substring of it says
      // nothing about where the value is drawn.
      expect(
        await identityBlock(page).getByRole("combobox", { name: "Status" }).count(),
        `the status control left the title block at ${width} px`
      ).toBe(1);
      expect(
        await identityBlock(page)
          .getByRole("combobox", { name: `Assignee of ${seeded.key}` })
          .count(),
        `the assignee left the title block at ${width} px`
      ).toBe(1);
      expect(
        await identityBlock(page)
          .getByTestId("issue-labels")
          .getByText("dispatch", { exact: true })
          .count(),
        `the first label left the title block at ${width} px`
      ).toBe(1);
      expect(
        await identityBlock(page)
          .getByRole("combobox", { name: `Priority of ${seeded.key}` })
          .count(),
        `the priority control left the title block at ${width} px`
      ).toBe(1);
    }

    // An empty field is reachable from one add control that lists it.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    expect(await addControl(page).count(), "the page offers no one add control").toBe(1);
    await addControl(page).click();
    expect(
      await openMenu(page).count(),
      "the add control opens no list of the fields with no value"
    ).toBe(1);
    expect(
      await openMenu(page).getByRole("menuitem").count(),
      "the add control's list names no field"
    ).toBeGreaterThan(0);
    await page.keyboard.press("Escape");

    // A section opens expanded on a first load, and a collapsed one keeps that state.
    expect(
      await marginSectionDisclosure(page, "development").getAttribute("aria-expanded"),
      "the Development section is collapsed on a first load"
    ).toBe("true");
    await marginSectionDisclosure(page, "development").click();
    expect(
      (await marginSection(page, "development").innerText()).replace(/\s+/g, " "),
      "collapsing the section left its values on the page"
    ).not.toContain(issue.repo ?? "");
    await gotoIssue(page, `/issues/${seeded.parentKey}`);
    await expectIssuePageDrawn(page);
    expect(
      await marginSectionDisclosure(page, "development").getAttribute("aria-expanded"),
      "the next issue opens the section this reader collapsed"
    ).toBe("false");
  });
});

// c-one-panel.
test("one right-hand panel, and the document page keeps its margin", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);

    expect(await marginSlot(page).count(), "the issue page draws no panel of its own").toBe(1);
    await expectIssuePageDrawn(page);
    expect(await marginShell(page).count(), "the shell's margin is not the panel it sits in").toBe(
      1
    );
    expect(
      await panelsRightOfMain(page),
      "the properties and the focused node's facts are not in one panel right of the main column"
    ).toHaveLength(1);
    for (const section of ["development", "people", "references"] as const) {
      expect(
        await marginSection(page, section).count(),
        `the panel holds no ${section} section`
      ).toBe(1);
    }
    expect(
      await focusedNodeFacts(page).count(),
      "the panel holds no facts of the focused node"
    ).toBe(1);
    expect(
      await conversationStateControl(page, "words").count(),
      "the panel holds no conversation"
    ).toBe(1);

    // The facts follow the focus.
    await focusThePage(page);
    await page.keyboard.press("Tab");
    const first = await focusedNodeFacts(page).innerText();
    const firstNode = await focusedNodeId(page);
    await page.keyboard.press("j");
    expect(await focusedNodeId(page), "j moved the focus to no other row").not.toBe(firstNode);
    expect(
      await focusedNodeFacts(page).innerText(),
      "the panel's facts did not follow the focus to the next node"
    ).not.toBe(first);

    // The handle, the collapse and the width the reader gave it.
    expect(await marginResizeHandle(page).count(), "the panel has no resize handle").toBe(1);
    const handle = await marginResizeHandle(page).boundingBox();
    expect(handle, "the resize handle has no box").not.toBeNull();
    const before = (await marginShell(page).boundingBox())?.width ?? 0;
    await page.mouse.move((handle?.x ?? 0) + 2, (handle?.y ?? 0) + 2);
    await page.mouse.down();
    await page.mouse.move((handle?.x ?? 0) - 120, (handle?.y ?? 0) + 2);
    await page.mouse.up();
    const resized = (await marginShell(page).boundingBox())?.width ?? 0;
    expect(resized, "dragging the handle did not widen the panel").toBeGreaterThan(before + 50);
    await page.reload();
    await expectIssuePageDrawn(page);
    expect(
      (await marginShell(page).boundingBox())?.width ?? 0,
      "the panel did not keep the width the reader gave it"
    ).toBeCloseTo(resized, 0);
    // The collapse moves the panel, it does not take it away: the margin draws no column for
    // it, so the page draws it in its own, and the reader keeps every property under either
    // preference and at every width (R11). One panel in either state, never two.
    expect(await panelPlacement(page, "the margin open"), "the margin draws no panel").toBe(
      "margin"
    );
    await marginHideControl(page).click();
    expect(
      await panelPlacement(page, "the margin collapsed to its rail"),
      "the collapse left the panel in the margin instead of the page's column"
    ).toBe("column");
    await marginShowControl(page).click();
    expect(
      await panelPlacement(page, "the margin opened again"),
      "the panel did not come back to the margin"
    ).toBe("margin");
    for (const width of [1280, 1024, PHONE.width]) {
      await page.setViewportSize({ height: DESKTOP.height, width });
      await expectIssuePageDrawn(page);
      // From 1280 px the margin keeps a column; below it the margin is a sheet, so the page's
      // own column draws the panel.
      expect(
        await panelPlacement(page, `${width} px`),
        `the panel is drawn in the wrong place at ${width} px`
      ).toBe(width >= 1280 ? "margin" : "column");
    }
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await expectIssuePageDrawn(page);

    // A project document page draws its margin as before.
    const document = await createProjectDocument("LAY", {
      content: "# A project document\n\nIts margin is unchanged.\n",
      name: "project-notes.md",
    });
    await page.goto(`/projects/LAY/documents/${document.artifact.slug}`);
    await expect(page.getByRole("heading", { level: 1, name: "A project document" })).toBeVisible();
    expect(await marginShell(page).count(), "the project document page lost its margin").toBe(1);
  });
});

// c-conversation-state. The state the reader picks is theirs: it is the state the page opens in
// after a navigation away and back, and on the next issue they open under the same login in the
// same browser (R13). Each reading is of the state the page draws and never of what the browser
// stored, so a pick kept somewhere the page does not read back fails this row - which is the
// negative control, a pick held in the page's own component state and lost with the page.
test("the conversation state is kept for the same login", async ({ browser }) => {
  // Five cold loads of the issue page, each one a navigation and the reads under it, on the
  // seed the rows around this one use: the same work c-three-things measured at 2.5 s to 3 s a
  // load, with room for a loaded box.
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const second = seeded.children.keys[0] ?? "";
    expect(second, "the seed holds no second issue to open under the same login").not.toBe("");

    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    expect(
      await conversationStateControl(page, "words").getAttribute("aria-selected"),
      "the conversation does not open in the words state"
    ).toBe("true");

    // The reader picks the questions, which is a state of the panel and no address of its own.
    await conversationStateControl(page, "questions").click();
    expect(
      await conversationStateControl(page, "questions").getAttribute("aria-selected"),
      "picking the questions did not put the conversation in that state"
    ).toBe("true");

    // Away to the second issue and back to the first: both open in the state they picked.
    for (const address of [`/issues/${second}`, `/issues/${seeded.key}`]) {
      await gotoIssue(page, address);
      await expectIssuePageDrawn(page);
      expect(
        await conversationStateControl(page, "questions").getAttribute("aria-selected"),
        `${address} does not open in the state the reader picked`
      ).toBe("true");
      expect(
        await conversationStateControl(page, "words").getAttribute("aria-selected"),
        `${address} opens in the words state, which the reader left`
      ).toBe("false");
    }

    // `/activity` names a state of its own, and arriving there is the reader asking for it, so
    // the address stands over what they kept - and the issue's own address goes on opening in
    // what they picked.
    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await expectIssuePageDrawn(page);
    expect(
      await conversationStateControl(page, "everything").getAttribute("aria-selected"),
      "/activity does not open the conversation in its everything state"
    ).toBe("true");
  });
});

// c-reply-nesting. The seed writes both replies through the route a person writes through, so
// the `answers` this row reads is the server's own: the write climbs `reply_to` to the head of
// the thread before it stores, which is read here first, and a page parenting by `reply_to`
// draws the second reply under that head - this row's negative control.
test("a reply is drawn under the entry it answers", async ({ browser }) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    // What the server stored, before the page is read: `reply_to` flattened onto the head and
    // the entry each reply answered kept beside it. Without this the row could pass on a page
    // that parents by `reply_to` over a thread the server never flattened.
    const stored = await listComments(seeded.key);
    const second = stored.find((comment) => comment.id === seeded.second);
    expect(second?.reply_to, "the server did not flatten the thread onto its head").toBe(
      seeded.head
    );
    expect(second?.answers, "the server kept no answered entry on the second reply").toBe(
      seeded.first
    );

    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    // A reply that answers a reply sits four levels below the issue, and a branch below the
    // drawn levels folds to one line counting what it holds (`tree-fold.ts`'s `DRAWN_LEVELS`),
    // so whatever the page folded that way is opened in place before the parents are read.
    await openEveryDeeperBranch(page);

    expect(
      await parentOfRow(page, seeded.second),
      `the reply that answers another reply is not drawn under it; the thread's head is ${seeded.head}`
    ).toBe(seeded.first);
    // Every other property of a turn's placement is unchanged: the thread's head sits under the
    // issue, and the first reply under the head, so a reply still belongs to its thread.
    expect(
      await parentOfRow(page, seeded.first),
      "the reply to the thread's head is not drawn under it"
    ).toBe(seeded.head);
    expect(
      await parentOfRow(page, seeded.head),
      "the thread's head is not drawn under the issue"
    ).toBe(seeded.key);
  });
});

// c-home. Partial on today's data: `from` waits on dispatch://INFRA-1098 item 21, so every
// requirement sits at the issue root and this test asserts that, not a decision's parentage.
test("the spec is a preset over the tree and an approval still names it", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The approval is requested before the page draws, so the target it pins is read back after.
    await requestApproval(seeded.specArtifactId, { summary: "The spec as the tree draws it." });

    await openPreset(page, seeded.key, "spec");
    expect(await activePreset(page), "the Spec address does not open the Spec preset").toBe(
      PRESET_NAMES.spec
    );
    await expectIssuePageDrawn(page);

    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const rows = await drawnRows(page);
    for (const requirement of seeded.requirements) {
      const id = blockNodeId(specSlug, requirement);
      expect(rowOf(rows, id), `the Spec preset does not draw ${requirement}`).toBeDefined();
      expect(
        await parentOfRow(page, id),
        `${requirement} does not sit at the issue root while its own from pointer is absent`
      ).toBe(seeded.key);
    }
    // A block with no `from` is never drawn as a block that had no conversation: every gap the
    // preset draws names its kind, so none is an unnamed mark on those rows.
    expect(await gapKinds(page), "a gap the Spec preset draws names no kind").not.toContain("");

    // The spec stays a thing an approval resolves: the version it pins still resolves after the
    // page has drawn.
    const artifact = await getArtifact(seeded.specArtifactId);
    expect(artifact.approval, "the spec carries no approval after the page drew").toBeDefined();
    const pinned = artifact.approval?.version ?? artifact.approval?.latest_version ?? 0;
    expect(pinned, "the approval pins no version of the spec").toBeGreaterThan(0);
    expect(
      (await getArtifactVersion(seeded.specArtifactId, pinned)).markdown,
      "the document version the approval pins does not resolve to the spec's own text"
    ).toContain(requirementText(2));
    expect(
      await presetToolbar(page).count(),
      "the preset whose document is open draws no toolbar to carry the approval"
    ).toBe(1);
  });

  // The same drawing on an issue that carries one document.
  await phase(browser, seedNoChildren, async (page, seeded) => {
    await openPreset(page, seeded.key, "spec");
    await expectIssuePageDrawn(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    for (const requirement of seeded.requirements) {
      expect(
        await parentOfRow(page, blockNodeId(specSlug, requirement)),
        `${requirement} is drawn differently on an issue that carries one document`
      ).toBe(seeded.key);
    }
  });
});

// c-checks-readable. The seeded Verification document carries no screenshot - no `check` block in
// `e2e/issue-layout-seed-text.ts` holds a picture - so "a screenshot renders as a picture" is
// asserted on the issue's image node in c-nodes instead, and not here.
test("the Checks preset is readable @phone", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;

    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await openPreset(page, seeded.key, "checks");
      expect(await activePreset(page), `the preset is not named Checks at ${width} px`).toBe(
        PRESET_NAMES.checks
      );
      await expectIssuePageDrawn(page);
      // The page opens folded to the frontier (R1), so a requirement whose every check has
      // passed arrives folded with the rest: at a width that draws the whole tree the reader
      // opens what the fold shut, which is what a reader reading the checks does. At 1024 px
      // and below the tree is zoomed one node at a time and draws every check the preset kept
      // under the issue already (`zoomedRows`), where a twisty zooms instead of unfolding.
      if (width > 1024) await unfoldEveryBranch(page);

      const checks = await rowsOfType(page, "check").count();
      expect(checks, `the Checks preset draws ${checks} checks, not the six seeded`).toBe(6);
      const ids = (await drawnRows(page))
        .filter((row) => row.type === "check")
        .map((row) => row.id ?? "");
      expect(
        ids.filter((id, index) => ids.indexOf(id) !== index),
        `a check is drawn twice at ${width} px`
      ).toEqual([]);
      expect(
        await boxesPastTheirCard(page, "rows"),
        `an element reaches past its card at ${width} px`
      ).toEqual([]);
      expect(
        await overlappingTexts(page, "rows"),
        `two pieces of text are drawn over each other at ${width} px`
      ).toEqual([]);
    }

    // A command renders as code with a copy control, and `when` reads as words. The check
    // that passed is one the frontier leaves folded away, so the reader opens the branches
    // again on this load: every one of the six is then a row, which the seeded states below
    // are read from too.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await openPreset(page, seeded.key, "checks");
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    const passed = nodeRow(page, blockNodeId(verificationSlug, checkBlockId(1))).first();
    await passed.focus();
    expect(await nodeCommands(page).count(), "the check's command is not drawn as code").toBe(1);
    expect(
      (
        await nodeCommands(page)
          .first()
          .evaluate((element) => getComputedStyle(element).fontFamily)
      ).toLowerCase(),
      "the command is not set in a monospace face"
    ).toMatch(/mono/);
    expect(await copyCommandControls(page).count(), "the command has no copy control").toBe(1);
    expect(
      (await passed.innerText()).replace(/\s+/g, " "),
      "the check draws its when attribute as a schema token instead of words"
    ).toContain(CHECK_WHEN_WORDS.before_merge);
    expect(
      await boxesPastTheirCard(page, "bodies"),
      "an element of the opened check reaches past its body"
    ).toEqual([]);

    // Every state the seed can produce carries a word and a shape no other state carries,
    // colours forced as well. Two states are not among them, and are named rather than left
    // out: `retracted` needs a result the seed withdraws none of, and `unreadable` needs a
    // server that sends no `pass_stands` at all, which this one always sends. A state added
    // to the page and not to this list fails here until somebody says which it is.
    const unseeded = ["retracted", "unreadable"];
    const states = statesDrawn((await drawnRows(page)).filter((row) => row.type === "check"));
    expect([...states.keys()].sort(), "the Checks preset does not draw every seeded state").toEqual(
      Object.keys(NODE_STATE_WORDS.check)
        .filter((state) => !unseeded.includes(state))
        .map((state) => `check:${state}`)
        .sort()
    );
    expect(
      [...new Set(states.values())].length,
      `two of the seeded states are drawn the same way: ${JSON.stringify([...states])}`
    ).toBe(states.size);
    await page.emulateMedia({ forcedColors: "active" });
    expect(
      [...statesDrawn((await drawnRows(page)).filter((row) => row.type === "check"))],
      "the seeded states are told apart differently once the colours are forced"
    ).toEqual([...states]);
  });
});

// c-plain-words.
test("the page draws no internal word where a name belongs @phone", async ({ browser }) => {
  // The check reads the page's chrome in ten views and in all 56 node bodies of the seeded
  // issue: about 11,700 readings, each read against five patterns and the declared list. One
  // `expect` per reading was 70,000 calls at about 4 ms each - 281 s of assertion beside 20 s
  // of page work - so each view gathers every offender it finds and names them in one
  // assertion, which reads the same strength and says more: a view that draws three internal
  // words names all three rather than the first. The row still carries a deadline of its own,
  // because ten navigations and 56 bodies outlast the 30 s default on their own.
  test.setTimeout(600_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // An identifier, a fragment, a slug, and the schema and enumeration tokens the check names.
    const banned = [
      /[0-9a-f]{8}-[0-9a-f]{4}-/i,
      /#b-/,
      /before_merge/,
      /no-result/,
      /plan_task/,
      /\bartifact\b/i,
    ];

    const readChrome = async (where: string) => {
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

    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        await gotoIssue(page, presetAddress(seeded.key, preset));
        await expectIssuePageDrawn(page);
        // `gotoIssue` waits for the head alone and the tree is drawn empty, so the view is
        // waited for by its rows: a preset read before its own query landed is a head and a
        // frame, which draw no internal word because they draw almost nothing. What this
        // cannot say is that the tree has finished filling - it reads the preset's first rows
        // and the ten views below cover the rest.
        await expect
          .poll(async () => await nodeRows(page).count(), {
            message: `the ${PRESET_NAMES[preset]} preset draws no row at ${size.width} px, so its words are not on the page yet`,
          })
          .toBeGreaterThan(0);
        await readChrome(`the ${PRESET_NAMES[preset]} preset at ${size.width} px`);
      }
    }

    // Every node's body in turn, where a type's attributes and its state are drawn in full.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    const rows = nodeRows(page);
    // The count is asserted, not only read: taken straight after the navigation it answers
    // zero for a tree whose reads have not landed, and the loop below would then read no body
    // at all and pass. The seed draws a row per requirement and far more besides.
    await expect
      .poll(async () => await rows.count(), {
        message: "the tree draws fewer rows than the seed's own requirements: it has not loaded",
      })
      .toBeGreaterThan(seeded.requirements.length);
    const count = await rows.count();
    for (let index = 0; index < count; index += 1) {
      const row = rows.nth(index);
      await row.focus();
      // The focus opens the body, and a document's body reads its own text as it opens: until
      // that read lands the body draws the line for a document that has not arrived (R16), so
      // a reading taken at once reads that line instead of the words this check is about. The
      // body is waited for in both directions - the loading line gone and words of its own
      // drawn - because a body still empty carries no loading line either, and `readChrome`
      // over an empty body reads the rest of the page and passes. What the wait cannot say is
      // which words those are; that is what `readChrome` is for.
      await expect(rowBody(row), `row ${index} opens no body on focus`).toBeAttached();
      await expect(
        rowBody(row),
        `the body of row ${index} still draws the line for a document that has not arrived`
      ).not.toContainText(EMPTY_STATE_LINES.document_reading);
      await expect(
        rowBody(row),
        `the body of row ${index} drew no text of its own, so there is nothing in it to read`
      ).not.toHaveText(/^\s*$/u);
      await readChrome(`the body of row ${index}`);
    }
  });
});

// c-nodes.
test("documents are nodes and nothing is a file list", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    // Nothing under the citing requirement is open, so its branch - the document it cites -
    // arrives folded with the rest (R1): the reader opens what the fold shut, which is the
    // twisty the folded row draws, and the document is a node under that block there.
    await unfoldEveryBranch(page);

    // The cited document is a node under the block that cites it, and opens from there.
    expect(
      await parentOfRow(page, seeded.documents.cited),
      "the cited document does not sit under the block that cites it"
    ).toBe(blockNodeId(specSlug, seeded.documents.citingBlock));
    const cited = nodeRow(page, seeded.documents.cited).first();
    await cited.focus();
    // The read is the node's own, made when the body opens: the page reads no document's text
    // before a reader opens it (R20, c-requests), so the body says so until the read lands.
    await expect
      .poll(async () => (await rowBody(cited).innerText()).replace(/\s+/g, " "), {
        message: "the cited document's node does not open its own text",
      })
      .toContain("The measurements behind the first requirement.");

    // What nothing cites is in the node menu, marked as cited by nothing, and so is the upload.
    expect(
      await nodeMenuButton(page).count(),
      "the page draws no node menu beside the presets"
    ).toBe(1);
    await nodeMenuButton(page).click();
    expect(await openMenu(page).count(), "the node menu opens nothing").toBe(1);
    const items = (await nodeMenuItems(page).allInnerTexts()).map((text) =>
      text.replace(/\s+/g, " ").trim()
    );
    for (const name of ["background.md", "shot.png"]) {
      const item = items.find((text) => text.includes(name));
      expect(item, `the node menu does not hold ${name}: ${JSON.stringify(items)}`).toBeDefined();
      expect(item, `${name} is not marked as cited by nothing`).toContain(
        CONTROL_LABELS.nothing_cites_it
      );
    }
    const upload = openMenu(page).getByRole("menuitem", { name: CONTROL_LABELS.upload });
    expect(await upload.count(), "the upload is not in the node menu").toBe(1);
    await upload.click();
    await uploadInput(page)
      .first()
      .setInputFiles({
        buffer: Buffer.from("# Uploaded\n\nFrom the node menu.\n"),
        mimeType: "text/markdown",
        name: "uploaded.md",
      });
    await expect
      .poll(async () => ((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.name))
      .toContain("uploaded.md");

    // An artifact's versions and the download of a version are reachable from the artifact.
    await page.keyboard.press("Escape");
    await cited.focus();
    expect(
      await offered(page, "button", CONTROL_LABELS.versions).count(),
      "the artifact's own versions are not reachable from it"
    ).toBeGreaterThan(0);
    expect(
      await page.getByRole("link", { name: CONTROL_LABELS.download }).count(),
      "a version's download is not reachable from the artifact"
    ).toBeGreaterThan(0);

    // The image is a node that draws its picture, and no preset is a file list.
    const image = nodeRow(page, seeded.documents.image).first();
    await image.focus();
    expect(await rowPictures(image).count(), "the image node draws no picture").toBeGreaterThan(0);
    expect(
      await presetTabs(page).allInnerTexts(),
      "a preset is still named Artifacts"
    ).not.toContain("Artifacts");
  });
});

// c-reflow.
test("the page never scrolls in two dimensions @phone", async ({ browser }) => {
  // Six widths, each a load of the page and a clipping walk over every row the tree draws at
  // it - 56 rows on the whole issue, each read in the browser - plus the page's own fit and the
  // panel's. Measured on an issue pod's two CPUs: 28.6 s in the phone project, which reads the
  // same six widths, and past 30 s in the desktop one, so the default deadline fails this row
  // on the machine it was written for rather than on a loaded one. The deadline below is four
  // times the measurement, which leaves room for a box under load.
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // What each row drew of its own text, kept by the width it was drawn at. The wrapped
    // widths are the stored text the one-line widths are read against, below the loop, once
    // both are in: a width reads the page it loaded, and a row is drawn on one line at one
    // width and wrapped at another.
    const wrapped = new Map<string, string>();
    const oneLineRows: { id: string; line: string; width: number }[] = [];
    // The test sets the viewport and writes no width rule of its own: every measure is the page's.
    for (const width of [320, 390, 768, 1024, 1280, 1440]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      // Nothing below is measured until the tree has drawn rows. `gotoIssue` waits for the
      // head and `expectIssuePageDrawn` for the tree's own element, which is drawn empty and
      // fills as the reads land, so every measurement taken here would read a page that holds
      // no row - and a page that holds no row is a page that fits, which is the first thing
      // `assertPageFits` retries its way into passing on. What this wait cannot say is that
      // the last row has landed; the walk below reads the rows that are drawn when it runs,
      // and six widths read them six times.
      await expect
        .poll(async () => await nodeRows(page).count(), {
          message: `the tree draws no row at ${width} px, so nothing here measures a page that holds its rows`,
        })
        .toBeGreaterThan(0);
      await assertWhole(identityBlock(page));
      // The walk is read against the rows it covers, so the count is an assertion of its own: a
      // walk over no row would pass every clipping check for want of anything to measure.
      const walked = await rowsWithoutAuthorScrollers(page);
      expect(
        walked.length,
        `the clipping walk covered no row of the tree at ${width} px: the page draws none`
      ).toBeGreaterThan(0);
      // The fit is measured with the tree's rows on the page, which the wait above is for: the
      // rows that make a page scroll sideways are measured only where they are drawn (the two
      // rows below this one wait for their own words for the same reason).
      await assertPageFits(page);
      await assertWhole(presetRow(page));
      // The panel holds the properties, the focused node's facts and the whole conversation, so
      // it is taller than the screen and scrolls down by design; R17 bans two dimensions, not
      // one, so the panel is read for a sideways clip and a clamped line (`e2e/unclipped.ts`).
      if ((await marginSlot(page).count()) === 1) await assertWholeAcross(marginSlot(page));
      // R17's reading of a row, which is of its box and not of its words: the row is one line
      // by design and cuts its own first line to what that line has room for (`NodeRow.tsx`'s
      // `oneLineFirstLine`), and the focused node's body holds the text whole (c-tree reads
      // the body against the stored text), so the cut takes nothing from a reader - they open
      // the node. What a row may not do is reach past the box it is drawn in or hold a box
      // that scrolls inside it, which is what `assertFitsItsBox` reads (`e2e/unclipped.ts` has
      // both readings and why a row takes the weaker one for its box). R1's reading of the
      // same rows stands beside it rather than being given up for it: the words the row drew
      // are collected here and read below the loop, where the wrapped widths are in too.
      for (const row of walked) {
        await assertFitsItsBox(row);
      }
      const drawn = await drawnRows(page);
      for (const row of drawn) {
        if (row.id === null || row.firstLine === null) continue;
        if (width > 1024) wrapped.set(row.id, row.firstLine);
        else oneLineRows.push({ id: row.id, line: row.firstLine, width });
      }
      expect(
        await scrollersOutsideAuthorText(page),
        `a code block, a table or a picture is drawn outside a node's own text at ${width} px`
      ).toEqual([]);

      if (width > 1024) continue;
      // At 1024 px and below the tree is zoomed one node at a time, with no second indent level.
      const rows = drawn.filter((row) => row.visible);
      const levels = [...new Set(rows.map((row) => row.level))];
      expect(
        levels.length,
        `the tree draws ${levels.length} indent levels at ${width} px: ${levels.join(", ")}`
      ).toBeLessThanOrEqual(2);
      expect(
        await nodeBodies(page).count(),
        `the zoomed tree draws more than the one node's body at ${width} px`
      ).toBeLessThanOrEqual(1);
    }
    // R1 on the rows that are one line, which is every row at 1024 px and below
    // (`ZOOMED_TREE_QUERY`): no row truncates a word. The stored text a one-line row is read
    // against is the line the same row draws where it wraps, and c-tree reads that line
    // against the node's own stored text, so a one-line line that is a word-boundary prefix
    // of the wrapped one is a word-boundary prefix of the stored text. A row drawn at a
    // one-line width and at no wrapped one has nothing to be read against, so the count below
    // is an assertion of its own: a reading over no row would pass for want of anything to
    // read.
    const read = oneLineRows.filter((row) => wrapped.has(row.id));
    expect(
      read.length,
      "no row of the tree was drawn both on one line and wrapped, so no line was read for R1"
    ).toBeGreaterThan(0);
    for (const row of read) {
      const stored = (wrapped.get(row.id) ?? "").replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
      expect(
        cutsAWord(stored, row.line),
        `the one-line row ${row.id} cuts "${stored}" inside a word at ${row.width} px`
      ).toBe(false);
    }
  });
});

// c-reflow, on words the seeded issue does not carry. R17 binds the page a reader opens, not one
// seed's text: a requirement whose words name a file path holds a run of 118 characters with no
// space in it, which is longer than the card that draws it at every width this reads. Found by
// the tester on 2026-11-09 at 39649c0f269d, where the Spec preset's requirement list draws that
// run in a span that neither wraps it nor gives it its own line, and the whole page scrolls
// sideways: 884 px of page in a 390 px viewport, and 3,644 px in a 1,440 px one for a 300
// character run.
test("a requirement whose words hold an unbroken run keeps the page inside its viewport @phone", async ({
  browser,
}) => {
  await phase(browser, seedUnbrokenWords, async (page, seeded) => {
    for (const width of [320, 390, 1440]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, presetAddress(seeded.key, "spec"));
      await expectIssuePageDrawn(page);
      // The words are on the page before it is measured, so a fit read before the list draws
      // cannot pass for the fit of a page that holds them.
      await expect(
        page.getByText(UNBROKEN_PATH, { exact: false }).first(),
        `the Spec preset draws none of the requirement's words at ${width} px`
      ).toBeVisible();
      await assertPageFits(page);
    }
  });
});

// c-reflow again, on the other surface the same run reaches: a child's own title. The children
// section draws `{key} · {title}` in a link, and the touch-target floor makes every link a flex
// box (`web/src/styles.css`), so the link's text is an anonymous flex item whose automatic
// minimum size is its min-content width - which `overflow-wrap` does not move. Measured at
// 320 px with the same 118 character path on 2026-11-09 at 454f2a3107a6: 807 px of text in a
// 288 px box and a page 823 px wide. Below `md` the section opens closed (R6), so this opens
// it, which is what a reader waiting on a child does.
test("a child whose title holds an unbroken run keeps the page inside its viewport @phone", async ({
  browser,
}) => {
  await phase(browser, seedUnbrokenChildTitle, async (page, seeded) => {
    for (const width of [320, 390, 1440]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      if ((await childRows(page).count()) === 0) await childrenDisclosure(page).click();
      // The title is on the page before it is measured, so a fit read before the rows draw
      // cannot pass for the fit of a page that holds them.
      await expect(
        childRow(page, seeded.childKey),
        `the children section draws no row for ${seeded.childKey} at ${width} px`
      ).toBeVisible();
      await expect(
        page.getByText(UNBROKEN_PATH, { exact: false }).first(),
        `the child's row draws none of its title's words at ${width} px`
      ).toBeVisible();
      await assertPageFits(page);
    }
  });
});

/** The fixture `e2e/record-page-inventory.e2e.ts` wrote: what the old page offered, by state. */
interface PageInventoryFixture {
  readonly commit: string;
  readonly renames: Readonly<Record<string, string>>;
  readonly "open-1440": readonly InventoryEntry[];
  readonly "open-390": readonly InventoryEntry[];
  readonly "closed-1440": readonly InventoryEntry[];
  readonly "closed-390": readonly InventoryEntry[];
}

/** Closes whatever the view before this one opened, so the next control is reached rather than
 *  dismissed: a press outside an open menu, or outside the identity block's actions group,
 *  closes it instead of reaching the control under it, and Escape closes each of them
 *  (`IssueActionsMenu.tsx`). */
async function closeOpenControls(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(openMenu(page)).toHaveCount(0);
  const actions = offered(page, "button", CONTROL_LABELS.issue_actions);
  if ((await actions.count()) === 1) {
    await expect(actions).not.toHaveAttribute("aria-expanded", "true");
  }
}

// c-nothing-lost.
test("the new page carries every element of the old one @phone", async ({ browser }) => {
  // Four states, each read through the page's own views - its five presets, the three
  // addresses beside them, the five views behind a control or a width, one per row of the
  // tree, and, where the narrow tree is the way in, the presets and the zooms walked again.
  // Each walk loads the page once and moves through it from there, which is how a reader moves
  // through it: 16 loads of the issue page per 390 px state and 10 per 1440 px one, naming 125
  // and 37 views, each settled over two readings of its accessibility tree, and the write walk
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

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The inventory first: what the old page offered is this check's own subject, and the page
    // root below it says whether the page carrying it is the redesigned one. The fixture is the
    // merge of the old page's nine tab panels, one view at a time, so the new page is read the
    // same way: its five presets and the three addresses beside them, the conversation, the
    // activity and the collaborators (R3, R12, R19). Three more views are the page's own,
    // because what they hold sits behind a control rather than behind an address: the node
    // menu, where a document no block cites lives (R16); the identity block's actions menu,
    // where Close and the parent editor live (R11); and each document's own node, whose body
    // is what a document opens (R2).
    // How far a walk over the tree reaches. The seeded issue draws fewer rows than this at
    // either width, and a view over a row the tree does not draw answers that it is not there
    // rather than reading the page the view before it left (`e2e/page-inventory.ts`), so the
    // bound costs a count and nothing else where the rows run out.
    const walkDepth = 24;
    // Which rows of the tree hold a zoom. The zoom walk's first view reads it off the tree it
    // loads and every view of that walk after it reads it back; each walk fills it again,
    // because the tree a width and a state draw is their own.
    const zoomsInto = new Set<number>();
    const views = (width: number) => [
      ...PRESET_ORDER.map((preset) => async () => {
        await gotoIssue(page, presetAddress(seeded.key, preset));
        return true;
      }),
      async () => {
        await gotoIssue(page, `/issues/${seeded.key}/conversation`);
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
        // The menu's rows are the issue's own uncited documents (`issue-artifacts.ts`'s
        // `uncitedArtifacts` over `issue.artifacts`), which arrive with the issue read rather
        // than with the shell: a menu opened before that read lands draws the upload item
        // alone, and the snapshot taken then records every document of this view as lost.
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
      // The preset row below 1280 px is one chooser rather than a row of tabs, the children
      // section opens closed below `md` (`useChildrenSection.ts`), and the panel is a sheet a
      // reader opens below the width the margin keeps a column at (R12): each is a view of the
      // page at that width, and at a width where it is already open the view is the page with
      // the menus closed, which is the page a reader is left on there.
      async () => {
        await closeOpenControls(page);
        if ((await presetChooser(page).count()) === 1) await presetChooser(page).click();
        return true;
      },
      async () => {
        await closeOpenControls(page);
        const disclosure = childrenDisclosure(page);
        if ((await disclosure.getAttribute("aria-expanded")) === "false") await disclosure.click();
        return true;
      },
      async () => {
        await closeOpenControls(page);
        const sheet = page.getByRole("button", { name: /review panel/i });
        if ((await sheet.count()) > 0) await sheet.first().click();
        return true;
      },
      // One view per row of the tree, because a tree opens one body at a time and a node's own
      // text, its facts and its controls are in that body (R2). The walk loads the page once
      // and then moves through the rows, which is how a reader moves through them: each view is
      // still read, and still fitted, on its own.
      //
      // At a width that draws the whole tree the walk opens the branches the page opened
      // folded first (R1): a node behind a fold - the document a requirement cites - is a
      // twisty away for a reader, and a walk that never pressed one would read it as lost. At
      // 1024 px and below a twisty zooms instead, which the zoom walk below does.
      ...Array.from({ length: walkDepth }, (_unused, index) => async () => {
        if (index === 0) {
          await gotoIssue(page, `/issues/${seeded.key}`);
          if (width > 1024) await unfoldEveryBranch(page);
          const first = nodeRows(page);
          if ((await first.count()) > 0) await first.first().focus();
          return true;
        }
        const rows = nodeRows(page);
        if ((await rows.count()) <= index) return false;
        await rows.nth(index).focus();
        return true;
      }),
      // Below 1025 px the tree is read one node at a time (R17), so what a reader reaches by
      // zooming the walk reaches by zooming too. The presets first, because a decision, a
      // check and a plan task sit under the issue in the preset that draws them; then one
      // zoom per row of the issue's own address, because a node that hangs off a row - the
      // document a requirement cites - is behind that row's own twisty and nowhere else. At a
      // width that draws the whole tree the views above already open every row, so neither
      // walk runs there. Each walk loads its page once, as the walk above does.
      ...(width > 1024
        ? []
        : PRESET_ORDER.flatMap((preset) =>
            Array.from({ length: 8 }, (_unused, index) => async () => {
              if (index === 0) {
                await gotoIssue(page, presetAddress(seeded.key, preset));
                const first = nodeRows(page);
                if ((await first.count()) > 0) await first.first().focus();
                return true;
              }
              const rows = nodeRows(page);
              if ((await rows.count()) <= index) return false;
              await rows.nth(index).focus();
              return true;
            })
          )),
      ...(width > 1024
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
                  // loaded box at 390 px: the cited document's own copy control read as lost
                  // on the attempt and was found on the retry.
                  const settling = watchApiRequests(page);
                  await gotoIssue(page, `/issues/${seeded.key}`);
                  await expectLoadSettled(settling);
                  zoomsInto.clear();
                  const rows = nodeRows(page);
                  const drawn = Math.min(await rows.count(), walkDepth);
                  for (let at = 0; at < drawn; at += 1) {
                    const fold = rows.nth(at).getByRole("button", { name: CONTROL_LABELS.unfold });
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
                zoomed = true;
              } else if (!zoomed) {
                return false;
              }
              // The zoomed node is the first row; its own nodes follow it, and the first of
              // them - a document the row's requirement cites - is read by the view that
              // opened the zoom.
              const under = nodeRows(page);
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
        renames: fixture.renames,
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
        renames: fixture.renames,
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

    await offered(page, "button", "Edit labels").click();
    await page.getByRole("combobox", { name: "Search or create label" }).fill("carried");
    await page
      .getByRole("option", { name: /carried/ })
      .first()
      .click();
    await page.keyboard.press("Escape");
    await expect.poll(async () => (await getIssue(seeded.key)).labels).toContain("carried");

    await openIssueActions(page);
    await offered(page, "button", "Edit parent issue").click();
    await page.getByRole("textbox", { name: "Parent" }).fill("");
    await offered(page, "button", CONTROL_LABELS.save_parent).click();
    await expect.poll(async () => (await getIssue(seeded.key)).parent).toBeNull();

    await offered(page, "button", `Messages default to ${PLANNER_TITLE}`).click();
    const route = page.getByRole("textbox", { name: "Message route" });
    await route.fill("role:reviewer");
    await offered(page, "button", CONTROL_LABELS.save_route).click();
    await expect.poll(async () => (await getIssue(seeded.key)).route).toBe("role:reviewer");

    const repository = offered(page, "combobox", `Repository of ${seeded.key}`);
    await repository.focus();
    const repoBefore = (await getIssue(seeded.key)).repo;
    await expect.poll(async () => (await optionsOf(repository)).length).toBeGreaterThan(1);
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

    await offered(page, "button", "Unpin issue").click();
    await expect.poll(async () => (await getMyState("alice"))[seeded.key]?.pinned).toBe(false);

    // The close before the closed-state inventory took the seed's claim with it, so the issue
    // is claimed again for the one control that hands a claim back.
    await claimIssue(seeded.key, asPlanner);
    await expect(offered(page, "button", `Release the claim on ${seeded.key}`)).toBeVisible();
    await offered(page, "button", `Release the claim on ${seeded.key}`).click();
    await expect.poll(async () => (await getIssue(seeded.key)).claim).toBeNull();

    await openIssueActions(page);
    await offered(page, "button", "Close issue").click();
    await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("done");
    await openIssueActions(page);
    await offered(page, "button", CONTROL_LABELS.reopen_into_backlog).click();
    await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");

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
    const decidingId = await deciding.first().getAttribute("data-node-id");
    expect(
      decidingId,
      "exactly one decision waits for review, and it is the one Keep names"
    ).not.toBeNull();
    const decidingRow = page.locator(`[data-node-row][data-node-id="${decidingId}"]`);
    await decidingRow
      .getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
      .click();
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
    await expect
      .poll(async () => ((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.name))
      .toContain("carried.md");

    // The panel's conversation, and its composer with it, is mounted once the conversation's
    // own address has been opened (`IssuePage.tsx`'s `activatedTabs`), so the composer is
    // written in where a reader writes in it.
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await marginSlot(page).getByRole("textbox", { name: "Comment" }).fill("The composer writes.");
    await marginSlot(page).getByRole("button", { name: "Send" }).click();
    await expect
      .poll(async () => (await listComments(seeded.key)).map((comment) => comment.body))
      .toContain("The composer writes.");

    // Back to the issue's own address, where the tree opens on the spec's nodes.
    await gotoIssue(page, `/issues/${seeded.key}`);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    // The ask is drawn as a row of the tree and again as a card in the panel (R1, R12), so the
    // answer is written on the row the reader focused.
    const question = marginSlot(page).getByTestId(`ask-${seeded.openQuestion.askId}`);
    await question.getByRole("radio").first().check();
    await question.getByRole("button", { name: /^Answer/ }).click();
    await expect
      .poll(async () => (await getAsk(seeded.openQuestion.askId)).ask.state)
      .toBe("answered");

    await nodeRow(page, blockNodeId(specSlug, requirementBlockId(3)))
      .first()
      .focus();
    await page.keyboard.press("Enter");
    await page.keyboard.type(" Edited in place.");
    await expect
      .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
      .toContain("Edited in place.");

    await presetToolbar(page).getByRole("button", { name: "Name version" }).click();
    const naming = page.getByRole("dialog", { name: "Name this version" });
    await naming.getByRole("textbox", { name: "What changed?" }).fill("Carried over");
    await naming.getByRole("button", { name: /^Save/ }).click();
    await expect
      .poll(async () =>
        ((await getArtifact(seeded.specArtifactId)).versions ?? []).map((one) => one.summary)
      )
      .toContain("Carried over");
  });
});

// c-urls.
test("every address of the old page still resolves", async ({ browser }) => {
  // Eighteen cold loads of the issue page, each one a navigation and its reads: 28 s of the
  // 30 s default when it was last measured, and over it once the panel draws its own cards
  // first. The row carries a deadline of its own rather than racing that default.
  test.setTimeout(300_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;

    // The two addresses whose tabs are gone.
    await gotoIssue(page, `/issues/${seeded.key}/verification`);
    expect(await activePreset(page), "/verification does not open the Checks preset").toBe(
      PRESET_NAMES.checks
    );
    await expectIssuePageDrawn(page);
    await gotoIssue(page, `/issues/${seeded.key}/artifacts`);
    await expectIssuePageDrawn(page);
    expect(await openMenu(page).count(), "/artifacts does not open the node menu").toBe(1);

    // Every preset's own address.
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetAddress(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await activePreset(page),
        `${presetAddress(seeded.key, preset)} does not open the ${PRESET_NAMES[preset]} preset`
      ).toBe(PRESET_NAMES[preset]);
    }

    // The conversation and the activity: one list, in two of its four states.
    for (const [address, state] of [
      ["conversation", "words"],
      ["activity", "everything"],
    ] as const) {
      await gotoIssue(page, `/issues/${seeded.key}/${address}`);
      await expectIssuePageDrawn(page);
      expect(
        await conversationStateControl(page, state).getAttribute("aria-selected"),
        `/${address} does not open the conversation in its ${state} state`
      ).toBe("true");
    }

    // The children, a document, and each entry of the conversation.
    await gotoIssue(page, `/issues/${seeded.key}/children`);
    await expectIssuePageDrawn(page);
    expect(await childRows(page).count(), "/children opens no children").toBe(
      seeded.children.keys.length
    );

    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.cited}`);
    await expectIssuePageDrawn(page);
    expect(
      await nodeRow(page, seeded.documents.cited).count(),
      "/artifacts/<slug> opens no node for that document"
    ).toBeGreaterThan(0);

    // An ask the server indexed from a document's `ask` block *is* that block's node
    // (`e2e/issue-page.ts`), which is what the `?ask=` reading below asserts too: no row of the
    // tree carries the ask's own uuid, so each address names the node it opens.
    for (const [kind, id, node] of [
      ["asks", seeded.openQuestion.askId, blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID)],
      ["comments", seeded.comments.anchored, seeded.comments.anchored],
      ["messages", seeded.messages.root, seeded.messages.root],
    ] as const) {
      await gotoIssue(page, `/issues/${seeded.key}/${kind}/${id}`);
      await expectIssuePageDrawn(page);
      expect(
        await nodeRow(page, node).count(),
        `/${kind}/<id> opens no node for it`
      ).toBeGreaterThan(0);
    }

    // The queries, each doing what it does today.
    await gotoIssue(page, `/issues/${seeded.key}/spec?v=1`);
    await expectIssuePageDrawn(page);
    // The picker's value, not the toolbar's text: a `select`'s option texts are part of its
    // container's `innerText`, so "Version 1" is in that text whichever version is drawn.
    expect(
      await versionPicker(page).inputValue(),
      "?v= does not open that version of the document"
    ).toBe("1");

    await gotoIssue(page, `/issues/${seeded.key}?q=${encodeURIComponent("Requirement 2")}`);
    await expectIssuePageDrawn(page);
    expect(await filterBox(page).inputValue(), "?q= does not fill the preset's filter").toBe(
      "Requirement 2"
    );

    await gotoIssue(page, `/issues/${seeded.key}?ask=${seeded.openQuestion.askId}`);
    await expectIssuePageDrawn(page);
    expect(await focusedNodeId(page), "?ask= does not open that ask's node").toBe(
      blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID)
    );

    await gotoIssue(page, `/issues/${seeded.key}?comment=${seeded.comments.anchored}`);
    await expectIssuePageDrawn(page);
    expect(
      await nodeRow(page, seeded.comments.anchored).count(),
      "?comment= opens no node for that comment"
    ).toBe(1);

    // A `#b-` fragment scrolls to its node, including one in a document the page does not open
    // first: the plan's last task is in no preset the issue's own address opens.
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    await gotoIssue(page, `/issues/${seeded.key}#b-${planTaskBlockId(9)}`);
    await expectIssuePageDrawn(page);
    const row = rowOf(await drawnRows(page), blockNodeId(planSlug, planTaskBlockId(9)));
    expect(
      row,
      "a #b- fragment into a document the page does not open first draws no node"
    ).toBeDefined();
    expect(
      row?.box?.y ?? -1,
      "the fragment's node is not scrolled into the viewport"
    ).toBeGreaterThanOrEqual(0);
    expect(
      row?.box?.y ?? Number.MAX_SAFE_INTEGER,
      "the fragment's node is below the screen"
    ).toBeLessThanOrEqual(DESKTOP.height);
  });
});

/** The load the budget is read against actually happened, and the page drew what its reads
 *  carry. A ceiling is only a budget while the page loaded: a page whose reads all failed sends
 *  few requests and would pass the ceiling for the opposite of the reason this check exists,
 *  which is a page that reads too much. `expectIssuePageDrawn` does not answer that question —
 *  the tree element is drawn before its rows are — so every requirement the seed wrote has a
 *  row of its own here, and the settle below is read after them. */
async function expectTheSeedIsDrawn(page: Page, seeded: BaselineSeed): Promise<void> {
  const slug = await primarySlug(seeded.key);
  await expect(
    nodeRow(page, seeded.key).first(),
    "the tree draws no row for the issue itself, so nothing was loaded to count the reads of"
  ).toBeVisible();
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

    // No read of the text of a document the page does not show. `GET /artifacts/{id}/text` is
    // that read and the only one: `GET /artifacts/{id}/blocks` answers `ArtifactBlock`s, which
    // carry a block's id, type, byte offsets, token, reference counts, attributes and a table's
    // cells and no prose at all (`packages/contracts/src/dispatch-api.ts`, `ArtifactBlock`), so
    // it is not a text read and naming it here would ban the one listing the tree is built
    // from. The spec is the one document the page does show on this address: it is the issue's
    // primary document, which the page stages for the Spec preset (`drawn-documents.ts`). The
    // head is no longer a reason for that read - it draws its sentences out of the summary
    // node's own text, which the tree already carries, and parses no stored markdown at all
    // (R20, `web/src/features/issue/IssueThreeThings.tsx`).
    expect(
      traffic.requests
        .filter(
          (request) =>
            /\/text(\?|$)/.test(request.path) && !request.path.includes(seeded.specArtifactId)
        )
        .map((request) => request.path),
      "the first load read the stored text of a document it does not show"
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
