import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";
import type { Actor, DispatchEvent } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue/issue-address";
import { firstLineOf } from "../web/src/features/issue/issue-tree";
import {
  ANSWER_PHRASES,
  BLOCK_ATTRIBUTE_LABELS,
  CHECK_WHEN_WORDS,
  type CheckState,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  NODE_REFERENCES_WORDS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  type NodeType,
  nodeStateWords,
  PLAN_WORDS,
  PRESET_NAMES,
  PRESET_ORDER,
  type Preset,
  SPEC_WORDS,
  STATE_LINES,
  UNASKED_SOURCES,
  VERIFICATION_WORDS,
  wordsIn,
} from "../web/src/features/issue/page-words";
import { blockNodeId, checkState } from "../web/src/features/issue/tree-drafts";
import { agentRoleWord } from "../web/src/features/refs/agent-role";
import { REF_PREVIEW_OPEN_DELAY_MS } from "../web/src/features/refs/ref-preview-timing";
import { getUnsubscribeCalls } from "./agents";
import {
  answerAsk,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getArtifactVersion,
  getAsk,
  getIssue,
  getIssueArtifactText,
  getIssueCoverage,
  getIssueDecisions,
  getIssueEvents,
  getIssueResults,
  getMyState,
  listComments,
  listIssueAsks,
  listIssueMessages,
  listMyReplies,
  patchIssue,
  recordCheckResult,
  requestApproval,
  resolveAsk,
  retractCheckResult,
  reviewArtifact,
  setGatedSteps,
} from "./api";
import { placeCaret } from "./editor";
import {
  type BaselineSeed,
  seedConversationState,
  seedDecisionsFilter,
  seedNoChildren,
  seedNoSummary,
  seedPlanOrder,
  seedReplyNesting,
  seedRequestBaseline,
  seedReviewList,
  seedSpineCases,
  seedTwiceTheNodes,
  seedUnbrokenChildTitle,
  seedUnbrokenWords,
  seedWholeIssue,
  seedWholeIssueAdvancing,
  UNBROKEN_PATH,
  type WholeIssueSeed,
} from "./issue-layout-seed";
import {
  asPlanner,
  HEAD_SHA,
  PLANNER,
  PLANNER_TITLE,
  PROJECT,
  plannerActor,
  recorderActor,
  WHOLE_REPO,
} from "./issue-layout-seed-support";
import {
  OPEN_QUESTION_BLOCK_ID,
  PLAN_ORDER_TASKS,
  planTaskBlockId,
  planTaskText,
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
  badgeValue,
  becauseLines,
  boxesPastTheirCard,
  branchMoreRows,
  caretInNode,
  childRow,
  childRows,
  childrenSection,
  chromeTexts,
  closeDetails,
  composer,
  composerField,
  contentsEntries,
  contentsPane,
  copyCommandControls,
  type DrawnRow,
  decidedByCells,
  decisionsFoot,
  decisionsPartsDrawn,
  detailsControl,
  detailsRow,
  drawnRows,
  expectActivePreset,
  filterBox,
  filterControl,
  filterRowReading,
  focusBranchControl,
  focusedNodeFacts,
  focusedNodeId,
  focusRow,
  focusThePage,
  gapKinds,
  gotoIssue,
  headerLabels,
  headerReading,
  historyEntries,
  historyItem,
  historyPanel,
  iconRail,
  identityBlock,
  identityBlockBottom,
  identityBlockFamilies,
  issuePage,
  labelsLeft,
  marginResizeHandle,
  marginSectionDisclosure,
  marginShell,
  marginSlot,
  namedCommit,
  nodeBodies,
  nodeCommands,
  nodeMenuButton,
  nodeMenuItems,
  nodeRow,
  nodeRows,
  type Overlap,
  offered,
  onScreen,
  openDetails,
  openEveryDeeperBranch,
  openedArtifact,
  openedEditor,
  openIssueActions,
  openMenu,
  openPreset,
  openVersionControl,
  optionsOf,
  outlineControl,
  outlineSheet,
  overlappingTexts,
  pageStatus,
  parentOfRow,
  pickPreset,
  pinnedBodies,
  planChartRows,
  planView,
  presetChooser,
  presetCount,
  presetMenuItem,
  presetRow,
  presetToggle,
  presetToggles,
  presetToolbar,
  type RowLayout,
  replyFromRow,
  rowAct,
  rowAuthor,
  rowBody,
  rowGaps,
  rowLayouts,
  rowMore,
  rowsOfType,
  rowsWithoutAuthorScrollers,
  rowWords,
  scrollersOutsideAuthorText,
  sendFromComposer,
  stickyReading,
  TYPEFACE,
  threeThings,
  tooltipTexts,
  tree,
  treeText,
  typefaceAddress,
  typefaceStatuses,
  undividedTexts,
  unfoldEveryBranch,
  uploadInput,
  verificationCards,
  versionControl,
  versionPicker,
  waitingCount,
  watchPanelDraws,
} from "./issue-page";
import {
  assertInventoryCarried,
  type InventoryEntry,
  type ParentRowMapping,
} from "./page-inventory";
import { resetDatabase, setEventCreatedAt } from "./seed";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertFitsItsBox, assertPageFits, assertWhole, assertWholeAcross } from "./unclipped";
import { asUser } from "./users";

/**
 * The redesigned issue page (dispatch://INFRA-1196), one test per `check` block of
 * `dispatch://INFRA-1196/artifact/verification-md`, each named by that check's own `-g` string.
 * Every test reads the page through `e2e/issue-page.ts`, which is the one statement of what the
 * page's DOM is, and spells no selector of its own.
 *
 * Several checks are partial on today's data, and each test says where it stops: every node
 * kind that waits on another issue - a criterion, a reading, a lens and its findings, a query
 * node, a proposed or overtaken state - is named beside the row that would read it.
 *
 * - **c-three-things** proves the three sentences and the two rows; who wrote the bullets and
 *   when they were last read back wait on dispatch://INFRA-1278, so the test asserts the head
 *   draws neither an author nor a gap for them.
 * - **c-home** proves the Spec preset over today's documents, where every requirement sits at the
 *   issue root because `from` waits on dispatch://INFRA-1098 item 21.
 * - **c-spine** proves the edges the model stores today: an answered ask over the decision that
 *   answers it, `serves`, a result's check and a reply's entry.
 *
 * A seed makes the project `LAY`, and a second `POST /api/v1/projects` for it answers
 * `409 PROJECT_EXISTS`, so a test that reads two seeded issues runs each in a `phase` of its own:
 * the reset truncates `user_sessions`, so each phase signs in again (`packages/dispatch/AGENTS.md`,
 * "End-to-end tests").
 */

/** The two sizes every check names. */
const DESKTOP = { height: 900, width: 1440 } as const;
const PHONE = { height: 844, width: 390 } as const;

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
    "the page draws no [data-issue-frame] root, so this is not the redesigned issue page"
  ).toHaveCount(1);
  await expect(
    tree(page),
    'the page draws no role="tree" named "Issue tree" inside [data-issue-frame]'
  ).toHaveCount(1);
}

/** Details' overlapping texts, read once two frames after it opened: the frames are the
 *  margin's own measuring of what it drew while hidden, and nothing after them - no later
 *  render that would correct a stale placement - is waited for, since the reader sees the
 *  drawer from the frame it opens. */
async function overlapsOnceOpened(page: Page): Promise<Overlap[]> {
  await page.evaluate(() => {
    const { promise, resolve } = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    return promise;
  });
  return overlappingTexts(page, "details");
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

/** The kinds a row draws no tag for, because they are the conversation's own words rather than
 *  a kind it revealed (R34: "as one tag where the node has a typed kind"). */
const UNTYPED_KINDS: readonly string[] = ["comment", "message"];

/**
 * Every row of a typed kind carries its kind's word and its kind's shape on its one tag (R34),
 * and a turn of the conversation carries neither, its author's name and shape saying whose words
 * it is (R9); its state word and its state's shape where the model holds a state, and nothing
 * where it holds none. Every word and token comes from `page-words.ts`, so a type or state the
 * page draws without declaring one fails here.
 */
function expectRowVocabulary(rows: readonly DrawnRow[]): void {
  for (const row of rows) {
    const type = row.type ?? "";
    expect(
      Object.keys(NODE_TYPE_NAMES),
      `row ${row.id} carries data-node-type "${type}", which the model does not name`
    ).toContain(type);
    const typeName = NODE_TYPE_NAMES[type as NodeType];
    const typeGlyph = GLYPH_TOKENS.types[type as NodeType];
    if (UNTYPED_KINDS.includes(type)) {
      expect(
        row.glyphs.map((glyph) => glyph.token),
        `row ${row.id} is a ${typeName}, which carries no tag, and draws its kind's shape`
      ).not.toContain(typeGlyph);
      expect(row.author, `row ${row.id} is a ${typeName} and names no author`).not.toBeNull();
    } else {
      expect(row.text.toLowerCase(), `row ${row.id} does not say it is a ${typeName}`).toContain(
        typeName
      );
      expect(
        row.glyphs.map((glyph) => glyph.token),
        `row ${row.id} draws no shape for its type (${typeGlyph})`
      ).toContain(typeGlyph);
    }
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

/**
 * R1 on one row's one line - a folded branch's - read twice, because the text alone cannot see
 * the cut a box makes: a box that hides the tail of its line leaves every character of it in the
 * line's text. So the line is read for its words - the start of the stored text, ending at a
 * word break, with an ellipsis where the text carries on - and its box for holding every
 * character it was given (`scrollWidth` at its `clientWidth`, a pixel for rounding).
 */
function expectCutAtAWord(row: DrawnRow | undefined, stored: string, where: string): void {
  expect(row, `${where} draws no row`).toBeDefined();
  const drawn = row?.firstLine ?? "";
  const line = drawn.replace(/\s*…$/u, "").trimEnd();
  expect(line, `${where} draws none of its own words`).not.toBe("");
  expect(stored.startsWith(line), `${where} draws "${drawn}", which is not "${stored}"`).toBe(true);
  expect(cutsAWord(stored, drawn), `${where} cuts "${stored}" inside a word: "${drawn}"`).toBe(
    false
  );
  if (line !== stored) {
    expect(drawn.endsWith("…"), `${where} says nothing of the text "${drawn}" stops short of`).toBe(
      true
    );
  }
  const box = row?.firstLineBox;
  expect(
    box?.scroll,
    `${where} draws "${drawn}" as ${box?.scroll} px of text in a ${box?.client} px box, so the ` +
      "box hides its tail where no word boundary decided it"
  ).toBeLessThanOrEqual((box?.client ?? -2) + 1);
  const words = row?.wordsBox;
  expect(
    words?.scroll,
    `${where} lays its words out over ${words?.scroll} px in a ${words?.client} px box, so the ` +
      "row hides a word beside its first line"
  ).toBeLessThanOrEqual((words?.client ?? -2) + 1);
}

test.beforeEach(async () => {
  await resetDatabase();
});

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

/** What of one ask says where the tree draws it: the document block it was indexed from, where
 *  it has one, and its own id. */
interface PlacedAsk {
  readonly artifact_id?: string | null;
  readonly block_artifact?: { readonly slug: string };
  readonly block_id?: string | null;
  readonly id: string;
}

/** The id the tree draws `ask` under: the block the server indexed it from, where it has one,
 *  and the ask's own id otherwise. */
function askNodeId(ask: PlacedAsk, slugOf: ReadonlyMap<string, string>): string {
  const slug = ask.block_artifact?.slug ?? slugOf.get(ask.artifact_id ?? "");
  return slug !== undefined && ask.block_id != null ? blockNodeId(slug, ask.block_id) : ask.id;
}

/** What the outline of `seedWholeIssue` holds, read from the server's own reads rather than from
 *  the page: every node of its conversation by the id the tree draws it under, the slugs of its
 *  documents and files, which no row may be, the summary callout, which the head draws
 *  (dispatch://INFRA-1196 R8, c-three-things), and the frontier - every node open and unanswered,
 *  which the fold opens the path to (R1). */
interface SeededOutline {
  readonly frontier: ReadonlySet<string>;
  readonly nodes: ReadonlySet<string>;
  readonly slugs: ReadonlySet<string>;
  readonly summary: string;
}

async function seededOutline(seeded: WholeIssueSeed): Promise<SeededOutline> {
  const artifacts = (await getIssue(seeded.key)).artifacts ?? [];
  const slugOf = new Map(artifacts.map((artifact) => [artifact.id, artifact.slug]));
  const slug = (id: string) => slugOf.get(id) ?? "";
  const nodes = new Set<string>([seeded.key, ...seeded.children.keys]);
  const frontier = new Set<string>();
  for (const ask of await listIssueAsks(seeded.key)) {
    if (ask.kind !== "question") continue;
    nodes.add(askNodeId(ask, slugOf));
    if (ask.state === "open") frontier.add(askNodeId(ask, slugOf));
  }
  for (const comment of await listComments(seeded.key)) nodes.add(comment.id);
  for (const thread of await listIssueMessages(seeded.key)) {
    for (const message of [thread.message, ...(thread.replies ?? [])]) nodes.add(message.id);
  }
  for (const requirement of (await getIssueCoverage(seeded.key)).requirements ?? []) {
    const id = blockNodeId(slug(seeded.specArtifactId), requirement.block_id);
    nodes.add(id);
    if (requirement.checks === 0) frontier.add(id);
  }
  for (const decision of (await getIssueDecisions(seeded.key)).decisions ?? []) {
    const id = blockNodeId(slug(seeded.decisionsArtifactId), decision.block_id);
    nodes.add(id);
    if (decision.waiting_review) frontier.add(id);
  }
  for (const check of (await getIssueResults(seeded.key)).checks ?? []) {
    const id = blockNodeId(slug(check.artifact_id), check.block_id);
    nodes.add(id);
    if (checkState(check) !== "passed") frontier.add(id);
    for (const result of check.results ?? []) nodes.add(result.id);
  }
  for (const task of seeded.planTasks) nodes.add(blockNodeId(slug(seeded.planArtifactId), task));
  return {
    frontier,
    nodes,
    slugs: new Set(artifacts.map((artifact) => artifact.slug)),
    summary: blockNodeId(slug(seeded.specArtifactId), seeded.summaryBlockId),
  };
}

/** The ids above a row of `id` in `rows` - its first, or the one at index `at` - outermost
 *  first, as the drawn levels give them: one read of the page for the whole chain rather than
 *  one per level. */
function ancestorsIn(
  rows: readonly DrawnRow[],
  id: string,
  at: number = rows.findIndex((row) => row.id === id)
): string[] {
  const chain: string[] = [];
  let level = at === -1 ? 0 : Number(rows[at]?.level ?? 0);
  for (let index = at - 1; index >= 0 && level > 1; index -= 1) {
    const row = rows[index];
    if (Number(row?.level ?? 0) !== level - 1) continue;
    chain.unshift(row?.id ?? "");
    level -= 1;
  }
  return chain;
}

/** How many rows `rows` draws under the first row of `id`, which with every branch open is how
 *  many nodes that node holds. */
function heldIn(rows: readonly DrawnRow[], id: string): number {
  const at = rows.findIndex((row) => row.id === id);
  const level = Number(rows[at]?.level ?? 0);
  let held = 0;
  for (let index = at + 1; index < rows.length; index += 1) {
    if (Number(rows[index]?.level ?? 0) <= level) break;
    held += 1;
  }
  return held;
}

// c-tree. On the issue today's model holds, the one the conversion has not reached
// (dispatch://INFRA-1253), with no filter on, at 1440 px. This row carries no `@phone`: below
// 1024 px the tree is read one node at a time under a breadcrumb, one level below the node it is
// on (R16), and every level, ancestor and fold below would be a different claim; the row under
// this one reads the clauses the width keeps.
//
// What waits on another issue, and is not read here: a criterion, a reading, a lens and a
// finding, which the model cannot hold yet (dispatch://INFRA-1253, INFRA-1150, INFRA-1950); the
// summary callout, which the head draws (R8) and c-three-things reads, and which this row
// leaves out of its reading either way; and which row of a node the edit key opens, which is
// c-editing's and touches nothing this row reads.
test("with no filter the page is the conversation, each node edited once, folded to the frontier", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const outline = await seededOutline(seeded);
    const specSlug = await primarySlug(seeded.key);
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    await expectIssuePageDrawn(page);

    // No document, file or handoff is a row, folded or not (R1, R3): the spec is drawn nowhere
    // in the outline.
    const folded = await drawnRows(page);
    expectNoIssueNodeRows(folded, outline.slugs, "the page as it opens");
    expect(
      folded.filter((row) => row.role !== "treeitem").map((row) => row.id),
      "these rows of the tree are no treeitem"
    ).toEqual([]);
    expectRowVocabulary(folded);

    // The page opens folded to the frontier: the open ask, every check that has not passed,
    // every decision waiting for review and every requirement no check serves is drawn with
    // its whole path open, and no branch is open that holds none of them - so no plan task
    // holds one open, and the first requirement, which holds a check that passed and a plan
    // task, arrives folded.
    expect(outline.frontier.size, "the seed holds nothing open and unanswered").toBeGreaterThan(0);
    for (const id of outline.frontier) {
      expect(
        rowOf(folded, id),
        `${id}, open and unanswered, draws no row as the page opens`
      ).toBeDefined();
      for (const above of ancestorsIn(folded, id)) {
        expect(
          rowOf(folded, above)?.expanded,
          `${above} is folded over ${id}, which is open and unanswered`
        ).toBe("true");
      }
    }
    const paths = new Set([...outline.frontier].flatMap((id) => ancestorsIn(folded, id)));
    expect(
      folded
        .filter((row) => row.expanded === "true" && !paths.has(row.id ?? ""))
        .map((row) => row.id),
      "a branch opens on load that holds nothing open and unanswered"
    ).toEqual([]);
    expect(
      rowOf(folded, blockNodeId(specSlug, requirementBlockId(1)))?.expanded,
      "the requirement whose check passed opens because a plan task serves it"
    ).toBe("false");

    // With every branch open, the outline draws a row for each node of the conversation the seed
    // wrote - each ask, comment, message, reply, requirement, decision, check, result and plan
    // task, and each child under the issue - read by node id against the server's own reads,
    // and a row for nothing else.
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const whole = await drawnRows(page);
    expectNoIssueNodeRows(whole, outline.slugs, "the page with every branch open");
    // The memo callout is the head's three sentences and no row of the outline (R8).
    expect(idsOf(whole), "the memo callout is drawn as a row of the outline").not.toContain(
      outline.summary
    );
    expect(
      [...new Set(idsOf(whole))].sort(),
      "the rows of the outline are not the nodes of the issue's conversation"
    ).toEqual([...outline.nodes].sort());
    for (const child of seeded.children.keys) {
      expect(ancestorsIn(whole, child), `the child ${child} does not sit under the issue`).toEqual([
        seeded.key,
      ]);
    }

    // Every branch the page opened folded shows its first line and counts the nodes under it.
    for (const row of folded) {
      if (row.expanded !== "false") continue;
      expect(row.foldedCount, `the folded ${row.id} does not count the nodes under it`).toBe(
        String(heldIn(whole, row.id ?? ""))
      );
    }

    // A node shown under a second parent names its other parents on that row.
    for (const id of new Set(idsOf(whole))) {
      const placed = whole.filter((row) => row.id === id);
      if (placed.length < 2) continue;
      for (const row of placed) {
        expect(row.text, `${id} is drawn twice and this row names no other parent`).toContain(
          CONTROL_LABELS.also_under
        );
      }
    }

    // An ask's answer is drawn in the ask's own row, by who answered and in their words, and is
    // no row of its own.
    const stored = (await getAsk(seeded.answeredAskId)).ask.answer;
    const words = stored?.text ?? "";
    expect(words, "the seed's answered ask holds no answer").not.toBe("");
    const asked = rowOf(whole, seeded.answeredAskId);
    expect(asked?.answerLine ?? "", "the answered ask's row draws no answer").toContain(
      firstLineOf(words).line
    );
    expect(asked?.answerAuthor, "the answered ask's row does not name who answered").toBe(
      stored?.user
    );
    expect(
      whole
        .filter((row) => row.id !== seeded.answeredAskId && (row.words ?? row.firstLine) === words)
        .map((row) => row.id),
      "the answer is drawn as a row of its own"
    ).toEqual([]);

    // A folded branch is where the page draws one line for a node (the architect's ruling 1 on
    // R1): that line ends at a sentence end or at 100 characters, always at a word, and is the
    // node's own text. Every other row draws its words whole, so the focused row holds them.
    const lines = folded.filter((row) => row.firstLine !== null);
    expect(lines.length, "the page as it opens draws no folded branch's line").toBeGreaterThan(0);
    for (const row of lines) {
      const line = (row.firstLine ?? "").replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
      expect(
        line.length,
        `row ${row.id} draws a first line of ${line.length} characters`
      ).toBeLessThanOrEqual(100);
      expect(row.text, `row ${row.id}'s first line is not its own text`).toContain(line);
      expect(row.words, `the folded row ${row.id} draws its words whole as well`).toBeNull();
    }
    const long = rowOf(folded, blockNodeId(specSlug, requirementBlockId(1)));
    expect(long?.firstLine, "the two-sentence requirement draws no folded line").not.toBeNull();
    expect(
      cutsAWord(requirementText(1), long?.firstLine ?? ""),
      `the first line "${long?.firstLine}" cuts the stored text inside a word`
    ).toBe(false);
    const first = nodeRow(page, blockNodeId(specSlug, requirementBlockId(1))).first();
    await first.focus();
    expect(await rowBody(first).count(), "the focused row draws no body").toBe(1);
    expect(
      (await rowWords(first).innerText()).replace(/\s+/g, " "),
      "the focused row does not draw the node's whole stored text"
    ).toContain(requirementText(1));
  });
});

/** The widths a folded branch's line is read at below 1024 px: the floor R18 names, the phone
 *  the `iphone` project runs, and the narrowest width above a phone's (`PHONE_VIEWPORT_QUERY` is
 *  under 768 px). */
const ONE_LINE_WIDTHS = [320, 390, 768] as const;

// c-tree, at the widths the tree is read one node at a time at (R16), which is a row of its own:
// the row above reads the whole tree at 1440 px, and every level it names, every ancestor it
// walks and every fold it counts is that tree's. This reads the clauses the narrow tree keeps:
// a drawn row draws its node's words whole (the architect's ruling 1 on R1), and a folded branch
// draws one line - its node's first line, cut at a word by R1's rule - and how many nodes it
// holds, in a box that hides none of it. It carries `@phone-only` beside `@phone` because the
// row above reads the same clauses wherever the tree draws the whole of itself.
//
// Two rows of the issue's own screen: the first requirement, which holds a check that passed
// and a plan task and so arrives folded, its author the page's own words for one nobody
// recorded (R9); and the open question, which the frontier draws two levels down whatever the
// depth (R16), its words whole.
//
// It sets the width itself, as every row of this file does: `phase` starts every page at the
// desktop size in every project, the `iphone` project included.
test("a folded branch's line is its first line, cut at a word @phone @phone-only", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = await primarySlug(seeded.key);
    const nodeId = blockNodeId(specSlug, requirementBlockId(1));
    const questionId = blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID);
    const question = (await getAsk(seeded.openQuestion.askId)).ask.question;
    for (const width of ONE_LINE_WIDTHS) {
      await page.setViewportSize({ height: PHONE.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(
        nodeRow(page, questionId).first(),
        `the open question draws no row at ${width} px`
      ).toBeVisible();
      await expect(
        nodeRow(page, nodeId).first(),
        `the issue's own screen draws no row for its first requirement at ${width} px`
      ).toBeVisible();
      const rows = await drawnRows(page);

      // The open question: its words whole.
      const asked = rowOf(rows, questionId);
      expect(asked?.firstLine, `the open question is cut to one line at ${width} px`).toBeNull();
      expect(asked?.words, `the open question's row at ${width} px`).toContain(question);

      // The folded requirement: its first line, cut at a word, and its count.
      const required = rowOf(rows, nodeId);
      expect(required?.expanded, `the first requirement is not folded at ${width} px`).toBe(
        "false"
      );
      // Ruling 5: the model holds no author for a requirement, so its row draws none; its
      // opened body says so, which is read below once the row has the focus.
      expect(
        required?.author,
        `the first requirement's row draws an author the model does not hold at ${width} px`
      ).toBeNull();
      expectCutAtAWord(
        required,
        requirementText(1),
        `the two-sentence requirement's line at ${width} px`
      );
      expect(required?.text, `the folded requirement counts nothing at ${width} px`).toContain(
        `${required?.foldedCount} ${CONTROL_LABELS.folded_rows}`
      );
    }

    // What the line left out is drawn the moment the row has the focus (R6), so the reader
    // loses no word to it.
    const opened = nodeRow(page, nodeId).first();
    await opened.focus();
    await expect
      .poll(async () => (await rowWords(opened).innerText()).replace(/\s+/g, " "), {
        message: "the focused row does not draw the node's whole stored text",
      })
      .toContain(requirementText(1));
    await expect(
      rowBody(opened).locator("[data-node-author]"),
      "the focused requirement's body does not say its author is unknown"
    ).toHaveText(CONTROL_LABELS.unknown_author);
  });
});

/** `count` words in paragraphs of forty, each word told apart by its place in the text. */
function longWords(lead: string, count: number): string {
  const words = Array.from({ length: count }, (_unused, at) => `${lead}${at + 1}`);
  const paragraphs: string[] = [];
  for (let at = 0; at < words.length; at += 40) {
    paragraphs.push(`${words.slice(at, at + 40).join(" ")}.`);
  }
  return paragraphs.join("\n\n");
}

/** The reader every browser row of this file signs in as (`phase`). */
const READER = "alice";

/**
 * R34 on every row one view draws, read off the page's own layout: the words of every node
 * start at one left edge from the node's own bullet, so the fold's indent is not in the number,
 * and the reader's own words at a second, inside it, behind their ink rule; nothing stands
 * between a bullet and its words; a node that holds others has its bullet as its fold control;
 * the bullet is its kind's colour; a typed kind draws one tag naming it - a child issue's is its
 * key (ruling 5) - with its author and its time after it, in grey, and a turn draws none. At
 * 761 px and above the tag stands at the right end of the words' first line; at 760 px and below
 * the group stands under the words (ruling 4), which `expectRowGroups` reads.
 */
function expectRowLayouts(
  layouts: readonly RowLayout[],
  where: string,
  ownDrawn: boolean,
  width: number
): void {
  expect(layouts.length, `${where} draws no row`).toBeGreaterThan(0);
  const edge = (row: RowLayout) => Math.round(row.wordsLeft - row.bullet.left);
  const others = [...new Set(layouts.filter((row) => !row.own).map(edge))];
  const own = [...new Set(layouts.filter((row) => row.own).map(edge))];
  expect(
    others,
    `${where}: the words of the nodes but the reader's start at more than one edge from their bullet`
  ).toHaveLength(1);
  expect(own.length, `${where}: the reader's own words start at more than one edge`).toBeLessThan(
    2
  );
  if (ownDrawn) {
    expect(own, `${where} draws none of the reader's own words`).toHaveLength(1);
    expect(
      own[0] ?? 0,
      `${where}: the reader's own words do not stand at their own edge, inside every other node's`
    ).toBeGreaterThan(others[0] ?? 0);
  }
  for (const row of layouts) {
    const at = `the ${row.type} ${row.id} on ${where}`;
    expect(row.wordsFirst, `${at} draws something between its bullet and its words`).toBe(true);
    expect(row.rule, `${at} draws the ink rule on words that are not the reader's`).toBe(row.own);
    expect(row.bullet.left, `${at} draws its bullet right of its words`).toBeLessThan(
      row.wordsLeft
    );
    if (row.fold !== null) {
      expect(row.fold.isBullet, `${at} holds nodes and its bullet is not its fold control`).toBe(
        true
      );
      expect([CONTROL_LABELS.fold, CONTROL_LABELS.unfold], `${at}'s fold control`).toContain(
        row.fold.name
      );
    }
    if (!row.context) {
      expect(row.bullet.fill, `${at}'s bullet is not its kind's colour`).toBe(row.bullet.kindFill);
    }
    if (UNTYPED_KINDS.includes(row.type)) {
      expect(row.tag, `${at} draws a tag, which a turn of the conversation has none of`).toBeNull();
    } else {
      expect(row.tag?.text, `${at} does not name its kind on one tag`).toBe(
        row.type === "child_issue" ? row.id : NODE_TYPE_NAMES[row.type as NodeType]
      );
      // A node the model holds no words for draws no first line to stand the tag beside.
      if (row.line.right > 0 && width > 760) {
        expect(
          row.tag?.left ?? -1,
          `${at} draws its tag before the end of its words' first line`
        ).toBeGreaterThanOrEqual(row.line.right - 1);
        expect(
          row.tag?.top ?? -1,
          `${at} draws its tag above its first line`
        ).toBeGreaterThanOrEqual(row.line.top - 8);
        expect(row.tag?.top ?? 1e6, `${at} draws its tag under its first line`).toBeLessThan(
          row.line.bottom
        );
      }
      // A node the model holds no author for, and no time, draws neither (ruling 5).
      if (row.byline !== null) {
        const after =
          row.byline.left >= (row.tag?.right ?? 0) - 1 || row.byline.top > (row.tag?.top ?? 0) + 1;
        expect(after, `${at} draws its author and its time before its tag`).toBe(true);
      }
    }
    if (row.author !== null && row.author.name !== READER) {
      expect(row.author.color, `${at} draws its author other than in grey`).toBe(row.quiet);
    }
    if (row.time !== null) {
      expect(row.bylineColor, `${at} draws its time other than in grey`).toBe(row.quiet);
    }
  }
}

/** What the architect's rulings 4 and 5 read of one row, off the page's own layout (c-row): the
 *  line of the kind's tag, the author and the time and where it stands against the row's words,
 *  the state under it, the chips that name what the node points at, the links in its words, and
 *  its fold control's box. */
interface RowGroup {
  /** The author the row's group names, or null where it names none. */
  readonly author: string | null;
  /** Each chip: the words it names its target by, all it draws, and whether it stands inside
   *  the box of the node's words and on their first line. */
  readonly chips: readonly {
    readonly inWords: boolean;
    readonly name: string;
    readonly onFirstLine: boolean;
    readonly text: string;
  }[];
  /** The bottom of what the row draws under its bullet's line: its words, its options, its
   *  answer and its body. */
  readonly contentBottom: number;
  /** Whether the row's words are cut, which draws the chips beside the control that opens the
   *  rest. */
  readonly cut: boolean;
  /** The fold control's box, or null for a row that holds no node. */
  readonly fold: { readonly height: number; readonly width: number } | null;
  /** The page's grey for a bullet, as computed, which underlines a link in a node's words. */
  readonly grey: string;
  /** The group's first line - the tag, the author and the time: how many lines of its own line
   *  height it takes, and its top. Null where the row draws no group. */
  readonly group: { readonly lines: number; readonly top: number } | null;
  readonly id: string;
  /** Each link in the row's words: its colour, the colour of the words it stands in, and its
   *  underline's. */
  readonly links: readonly {
    readonly color: string;
    readonly underline: string;
    readonly words: string;
  }[];
  /** Whether the row draws its body. */
  readonly open: boolean;
  /** The top of the state's word, or null where the row draws no state. */
  readonly stateTop: number | null;
  readonly type: string;
}

/** Every visible row's group, chips and links, in document order (c-row, rulings 4 and 5). */
function rowGroups(page: Page): Promise<RowGroup[]> {
  return nodeRows(page).evaluateAll((rows) => {
    const text = (element: Element | null | undefined): string | null =>
      element == null ? null : (element.textContent ?? "").replace(/\s+/g, " ").trim();
    const probe = document.createElement("i");
    probe.style.color = "var(--color-page-bullet)";
    document.body.append(probe);
    const grey = getComputedStyle(probe).color;
    probe.remove();
    return rows
      .filter((row) => row.getClientRects().length > 0)
      .map((row) => {
        const words = row.querySelector("[data-node-words]");
        const margin = row.querySelector("[data-node-margin]");
        const line = margin?.firstElementChild ?? null;
        const lineBox = line?.getBoundingClientRect();
        const pitch = line === null ? 0 : Number.parseFloat(getComputedStyle(line).lineHeight);
        // The top of the words' first line, read from their text alone, the chips' left out.
        const tops: number[] = [];
        if (words !== null) {
          const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
          for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
            if (node.parentElement?.closest("[data-node-trail]") != null) continue;
            const range = document.createRange();
            range.selectNodeContents(node);
            for (const rect of range.getClientRects()) if (rect.width > 0) tops.push(rect.top);
          }
        }
        const firstTop = tops.length === 0 ? null : Math.min(...tops);
        const fold = row.querySelector("button[data-node-bullet]")?.getBoundingClientRect();
        return {
          author: text(margin?.querySelector("[data-node-author]")),
          chips: [...row.querySelectorAll("[data-node-chip]")].map((chip) => {
            const name = chip.querySelector("[data-chip-name]");
            const box = (name ?? chip).getClientRects()[0];
            return {
              inWords: words?.contains(chip) === true,
              name: text(name) ?? "",
              onFirstLine:
                firstTop !== null && box !== undefined && Math.abs(box.top - firstTop) < 8,
              text: text(chip) ?? "",
            };
          }),
          contentBottom:
            row.querySelector("[data-node-content]")?.getBoundingClientRect().bottom ?? 0,
          cut: row.querySelector("[data-node-more]") !== null,
          fold: fold === undefined ? null : { height: fold.height, width: fold.width },
          grey,
          group:
            lineBox === undefined || lineBox.height === 0
              ? null
              : {
                  lines: Math.round(lineBox.height / (Number.isFinite(pitch) ? pitch : 24)),
                  top: lineBox.top,
                },
          id: row.getAttribute("data-node-id") ?? "",
          links: [...(words?.querySelectorAll("a[href]") ?? [])]
            // The node's own words: not its chips, and not a child issue's facts after its title,
            // which draw their keys as the ink on the quiet grey (`IssueChildren.tsx`).
            .filter((link) => link.closest("[data-node-trail], [data-child-facts]") === null)
            .map((link) => ({
              color: getComputedStyle(link).color,
              underline: getComputedStyle(link).textDecorationColor,
              words: link.parentElement === null ? "" : getComputedStyle(link.parentElement).color,
            })),
          open: row.querySelector("[data-node-body]") !== null,
          stateTop: margin?.querySelector("[data-node-state]")?.getBoundingClientRect().top ?? null,
          type: row.getAttribute("data-node-type") ?? "",
        };
      });
  });
}

/**
 * The architect's rulings 4 and 5 on every row one view draws (c-row). At 761 px and above a
 * row that is not open draws its kind, its author and its time on one line, and its state on a
 * second line under it; at 760 px and below that line stands under the row's words. No row
 * draws the page's words for an author nobody recorded: its opened body says them. Each tie is a
 * chip inside the box of the node's words, named by at most 32 characters and by no address,
 * slug or short form of one (`addresses`). A link in a node's words is the words' own colour,
 * underlined in the bullets' grey. On a phone a fold control is 24 px square at least (WCAG
 * 2.5.8).
 */
function expectRowGroups(
  groups: readonly RowGroup[],
  where: string,
  width: number,
  addresses: RegExp
): void {
  for (const row of groups) {
    const at = `the ${row.type} ${row.id} on ${where}`;
    expect(
      row.author,
      `${at} draws the page's words for an author nobody recorded on its row`
    ).not.toBe(CONTROL_LABELS.unknown_author);
    if (row.group !== null && width > 760) {
      if (!row.open) {
        expect(
          row.group.lines,
          `${at} draws its kind, its author and its time on more than one line`
        ).toBe(1);
      }
      if (row.stateTop !== null) {
        expect(
          row.stateTop,
          `${at} draws its state on the line of its kind, its author and its time`
        ).toBeGreaterThan(row.group.top + 8);
      }
    }
    if (row.group !== null && width <= 760) {
      expect(
        row.group.lines,
        `${at} draws its kind, its author and its time on more than one line`
      ).toBe(1);
      expect(
        row.group.top,
        `${at} draws its kind, its author and its time beside its words and not under them`
      ).toBeGreaterThanOrEqual(row.contentBottom - 1);
    }
    for (const chip of row.chips) {
      expect(
        chip.name.length,
        `${at} names a target by more than 32 characters: "${chip.name}"`
      ).toBeLessThanOrEqual(32);
      expect(
        addresses.test(chip.text),
        `${at} draws an address or a slug on a chip: "${chip.text}"`
      ).toBe(false);
      if (!row.cut) {
        expect(chip.inWords, `${at} draws the chip "${chip.text}" on a line under its words`).toBe(
          true
        );
      }
    }
    for (const link of row.links) {
      expect(link.color, `${at} draws a link in its words in another colour than the words'`).toBe(
        link.words
      );
      expect(
        link.underline,
        `${at} underlines a link in its words in another colour than the bullets' grey`
      ).toBe(row.grey);
    }
    if (width <= 760 && row.fold !== null) {
      expect(
        Math.min(row.fold.width, row.fold.height),
        `${at}'s fold control is smaller than 24 px at ${width} px`
      ).toBeGreaterThanOrEqual(24);
    }
  }
}

// c-row. Every row of every view, at both widths, read off the page's layout, with the
// architect's rulings 4 and 5 beside R34; then ruling 1 on R1: an agent's words and an answered
// question that run past six lines with more than 20 words hidden are cut, "more · N words"
// opens the rest in place and is drawn on no row whose body draws the words whole, and the
// reader's own words, their answer and a question that waits on them are never cut. The seed
// adds those five long texts to the whole issue, and an agent's message holding a link an author
// wrote and a reference to an issue, which the row draws by its key and title (R19, F14).
test("a node draws its kind at the right of its row and every node starts at one left edge @phone", async ({
  browser,
}) => {
  // Ten loads of the whole issue, each read in one pass of the browser, and the cut read once.
  test.setTimeout(240_000);
  const answerWords = longWords("said", 160);
  await phase(
    browser,
    async () => {
      const seeded = await seedWholeIssue();
      const agent = await createMessage(seeded.key, { body: longWords("plan", 160) }, asPlanner);
      const own = await createComment(seeded.key, { body: longWords("mine", 160) });
      const asked = await createAsk(
        seeded.key,
        { question: "Which of the two layouts is the page?", urgency: "high" },
        asPlanner
      );
      await answerAsk(asked.id, { expected_edited_at: null, selected: [], text: answerWords });
      const longAnswered = await createAsk(
        seeded.key,
        { question: longWords("asked", 160), urgency: "high" },
        asPlanner
      );
      await answerAsk(longAnswered.id, {
        expected_edited_at: null,
        selected: [],
        text: "The first of the two.",
      });
      const waiting = await createAsk(
        seeded.key,
        { question: longWords("waits", 160), urgency: "high" },
        asPlanner
      );
      // An agent's words holding a link its author wrote, a reference to another issue, and a
      // word the reference grammar reads as an issue key and no issue is.
      const cited = seeded.children.keys[0] ?? "";
      const linked = await createMessage(
        seeded.key,
        {
          body: `Read [the notes](https://example.com/notes) with dispatch://${cited} beside them, in UTF-16.`,
        },
        asPlanner
      );
      return {
        agent: agent.id,
        answered: asked.id,
        cited,
        key: seeded.key,
        linked: linked.id,
        longAnswered: longAnswered.id,
        own: own.id,
        title: (await getIssue(cited)).title,
        waiting: waiting.id,
      };
    },
    async (page, seeded) => {
      for (const size of [DESKTOP, PHONE]) {
        await page.setViewportSize(size);
        for (const preset of PRESET_ORDER) {
          const settling = watchApiRequests(page);
          await gotoIssue(page, presetPath(seeded.key, preset));
          await expectLoadSettled(settling);
          await expectIssuePageDrawn(page);
          // Plan opens on its chart; its steps are rows of the outline, which its Steps control
          // draws.
          if (preset === "plan") await page.locator('[data-plan-view="steps"]').click();
          await expect
            .poll(() => nodeRows(page).count(), {
              message: `${PRESET_NAMES[preset]} at ${size.width} px draws no row`,
            })
            .toBeGreaterThan(0);
          const where = `${PRESET_NAMES[preset]} at ${size.width} px`;
          // R34's layout is read where the page draws the outline's rows: with no filter, under
          // Decisions and in Plan's steps. Spec, Verification and Plan's chart are drawn as their
          // own approved pictures (ruling 5), which c-home, c-checks-readable and c-plan-order
          // read.
          if (preset === "conversation" || preset === "decisions" || preset === "plan") {
            expectRowLayouts(await rowLayouts(page), where, preset === "conversation", size.width);
          }
          // Rulings 4 and 5. No chip draws an address, the short form of one, or a slug.
          expectRowGroups(
            await rowGroups(page),
            where,
            size.width,
            new RegExp(`dispatch://|${seeded.key} \\S+( block)?$|-md\\b`, "u")
          );
        }
      }

      // The cut, at 1440 px with no filter on and the focus nowhere.
      await page.setViewportSize(DESKTOP);
      const settling = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectLoadSettled(settling);
      // R21, F17: a row names what its words cite from the page's one batched read, so this
      // load reads no summary of the issue the message cites and asks nothing about `UTF-16`.
      expect(
        settling.requests
          .map((request) => request.path)
          .filter(
            (path) =>
              path.includes(`/issues/${seeded.cited}/summary`) || path.includes("/issues/UTF-16")
          ),
        "the rows read a target of their own for a reference their words cite"
      ).toEqual([]);
      const agent = nodeRow(page, seeded.agent).first();
      await expect(agent, "the agent's long message draws no row").toBeVisible();
      const cut = (await rowLayouts(page)).find((row) => row.id === seeded.agent);
      const hidden = Number(
        /^more · ([\d,]+) words$/u.exec(cut?.more ?? "")?.[1]?.replace(",", "")
      );
      expect(cut?.more, "the agent's long words carry no control that opens the rest").toMatch(
        /^more · [\d,]+ words$/u
      );
      expect(hidden, "the cut holds back 20 words or fewer").toBeGreaterThan(20);
      expect(cut?.wordsHidden, "the agent's long words are drawn whole").toBeGreaterThan(0);
      expect(
        (await rowWords(agent).boundingBox())?.height ?? 1e6,
        "the agent's words are drawn past six lines"
      ).toBeLessThanOrEqual(6 * 24 + 12);
      // The control opens the rest in place, and it stays open once the focus has left.
      await rowMore(agent).click();
      await focusThePage(page);
      await expect
        .poll(async () => (await rowLayouts(page)).find((row) => row.id === seeded.agent), {
          message: "more did not open the rest of the agent's words in place",
        })
        .toMatchObject({ more: CONTROL_LABELS.read_less, wordsHidden: 0 });

      // The reader's own long words and long answer are never cut.
      const layouts = await rowLayouts(page);
      expect(
        layouts.find((row) => row.id === seeded.own),
        "the reader's own long comment is cut"
      ).toMatchObject({ more: null, own: true, wordsHidden: 0 });
      const answer = nodeRow(page, seeded.answered).first().locator("[data-answer-text]");
      await expect(answer, "the reader's long answer is not drawn whole").toHaveText(answerWords, {
        useInnerText: true,
      });
      expect(
        await answer.evaluate((element) => element.scrollHeight - element.clientHeight),
        "the reader's long answer is cut"
      ).toBeLessThanOrEqual(1);

      // Ruling 1 on a question: a long one a person answered is cut as an agent's words are,
      // and a long one that waits on the reader is drawn whole.
      expect(
        (await getAsk(seeded.waiting)).ask.waiting_on_person,
        "the seeded open question does not wait on the reader"
      ).toBe(READER);
      expect(
        layouts.find((row) => row.id === seeded.longAnswered)?.more ?? "",
        "the long answered question is drawn whole"
      ).toMatch(/^more · [\d,]+ words$/u);
      expect(
        layouts.find((row) => row.id === seeded.waiting),
        "the long question that waits on the reader is cut"
      ).toMatchObject({ more: null, wordsHidden: 0 });

      // A body draws its node's words whole, so a row whose body is open draws no control that
      // would open what is open already; the control is back once the focus has left.
      const answered = nodeRow(page, seeded.longAnswered).first();
      await answered.focus();
      await expect(
        rowMore(answered),
        "a focused row draws more beside words its body draws whole"
      ).toHaveCount(0);
      expect(
        await rowWords(answered).evaluate((element) => element.scrollHeight - element.clientHeight),
        "a focused row's words are still cut"
      ).toBeLessThanOrEqual(1);
      await focusThePage(page);
      await expect(rowMore(answered), "more is gone once the focus has left").toHaveCount(1);

      // A folded branch keeps its count once its focus draws its words whole, and a node the
      // model holds no author for says so in its opened body and nowhere on its row (ruling 5).
      const folded = nodeRow(
        page,
        blockNodeId(await primarySlug(seeded.key), requirementBlockId(1))
      ).first();
      await expect(folded, "the first requirement is not folded").toHaveAttribute(
        "aria-expanded",
        "false"
      );
      await folded.focus();
      await expect(
        folded.locator("[data-node-words] [data-node-count]"),
        "a focused folded branch no longer says how many rows it hides"
      ).toHaveText(
        new RegExp(`^\\d+ (${CONTROL_LABELS.folded_row}|${CONTROL_LABELS.folded_rows})$`, "u")
      );
      await expect(
        rowBody(folded).locator("[data-node-author]"),
        "an opened body does not say its node's author is unknown"
      ).toHaveText(CONTROL_LABELS.unknown_author);
      await expect(
        folded.locator("[data-node-margin] [data-node-author]"),
        "a row draws an author the model does not hold"
      ).toHaveCount(0);

      // A reference in a node's words is a link named by its target's key and title, never a
      // gap; a word that only looks like a key is the word it is; and both of the message's
      // links are the words' colour over the bullets' grey, which the walk above read on every
      // view.
      const linked = nodeRow(page, seeded.linked).first();
      await expect(
        linked.locator("[data-node-words] a[data-row-reference]"),
        "a reference in a node's words is not drawn by its target's key and title"
      ).toHaveText(`${seeded.cited} ${seeded.title}`);
      await expect(
        linked.locator("a", { hasText: "UTF-16" }),
        "a word that names no issue is drawn as a link"
      ).toHaveCount(0);
      await expect(rowWords(linked), "the row drops a word that names no issue").toContainText(
        "in UTF-16."
      );
      expect(
        (await rowGroups(page)).find((row) => row.id === seeded.linked)?.links,
        "the message's link and its reference are not both links of its words"
      ).toHaveLength(2);
    }
  );
});

// c-focus-opens.
test("a body opens on focus and nothing opens on hover", async ({ browser }) => {
  // The row rests the pointer on every reference the page draws, each for longer than a card
  // takes to open elsewhere, which is a second or two of absences on top of the row's own walk.
  test.setTimeout(120_000);
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

    // No link opens a card on hover either, a link in an open body included: the issue page is
    // worked from the keyboard (R6). A decision's words name the requirements it shaped as
    // links, and its row is pinned open, so the pointer crosses every reference the page draws -
    // the children's, the panel's and that row's - with the keyboard focus elsewhere.
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const decision = nodeRow(page, blockNodeId(decisionsSlug, seeded.decisions[0] ?? "")).first();
    await decision.focus();
    await page.keyboard.press("o");
    await focusThePage(page);
    const bodyLinks = rowWords(decision).locator("[data-dispatch-ref]");
    await expect
      .poll(() => bodyLinks.count(), { message: "the pinned decision's words draw no link" })
      .toBeGreaterThan(0);
    const references = issuePage(page).locator(
      "[data-dispatch-ref]:visible, [data-dispatch-href]:visible"
    );
    const referenceCount = await references.count();
    for (let index = 0; index < referenceCount; index += 1) {
      await references.nth(index).hover();
      // A pointer resting on the link past the delay a card opens after on every other page:
      // the absence is the claim, so the wait is the span the card would have taken to open.
      await page.waitForTimeout(REF_PREVIEW_OPEN_DELAY_MS + 200);
      expect(
        await page.getByRole("tooltip").count(),
        `the pointer resting on link ${index} of the issue page opened a card`
      ).toBe(0);
    }
    // The preview a link offers opens from the keyboard on the focused link.
    await page.mouse.move(0, 0);
    await page.keyboard.press("Tab");
    await bodyLinks.first().focus();
    await expect(
      page.getByRole("tooltip"),
      "the keyboard focus on a link of the issue page opened no card"
    ).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("tooltip")).toHaveCount(0);
    await decision.focus();
    await page.keyboard.press("o");
    await focusThePage(page);

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
    // Each typed kind's tag carries its shape, which is the first shape its row draws; a turn
    // of the conversation draws no tag (R34), and its author's shape says whose words it is.
    const types = new Map(
      drawn
        .filter((row) => !UNTYPED_KINDS.includes(row.type ?? ""))
        .map((row) => [row.type ?? "", row.glyphs[0]?.token ?? ""])
    );
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

    // Another page keeps its hover card: this change touches no page but the issue's, and the
    // Inbox's owner link opens a card for a resting pointer as it always has.
    await page.emulateMedia({ forcedColors: "none" });
    await page.goto("/");
    const owner = page.locator("[data-inbox-row] [data-dispatch-ref]").first();
    await expect(owner, "the Inbox lists no row of the seeded issue's open question").toBeVisible();
    await owner.hover();
    await expect(
      page.getByRole("tooltip"),
      "the Inbox's owner link opened no card for a resting pointer"
    ).toBeVisible();
  });
});

/** What each filter draws as rows, and of those the kind it counts (R2). */
const FILTER_KINDS = {
  decisions: { counted: ["ask", "decision"], rows: ["ask", "decision"] },
  plan: { counted: ["plan_task"], rows: ["plan_task"] },
  spec: { counted: ["requirement"], rows: ["requirement"] },
  verification: { counted: ["check"], rows: ["requirement", "check", "result"] },
} as const satisfies Record<
  Exclude<Preset, "conversation">,
  { readonly counted: readonly NodeType[]; readonly rows: readonly NodeType[] }
>;

/** Each drawn row's parent, read from the rows' own levels in one reading of the page: the row
 *  above it one level up, or null for a row at the top. A node drawn under two parents has a
 *  parent per placement, so the answer is every parent of each node. */
function parentsOf(rows: readonly DrawnRow[]): Map<string, Set<string | null>> {
  const parents = new Map<string, Set<string | null>>();
  rows.forEach((row, index) => {
    const level = Number(row.level ?? "0");
    let parent: string | null = null;
    for (let above = index - 1; above >= 0; above -= 1) {
      if (Number(rows[above]?.level ?? "0") === level - 1) {
        parent = rows[above]?.id ?? null;
        break;
      }
    }
    const id = row.id ?? "";
    parents.set(id, (parents.get(id) ?? new Set()).add(parent));
  });
  return parents;
}

// c-presets. What waits on dispatch://INFRA-1253, and is not read here: each filter's ids against
// `GET /api/v1/issues/{key}/tree?view=`, a criterion, a parent's requirements on a child's Spec,
// a child's checks under a parent's requirements, and `?v=` drawing a filter as the query node
// stood. Today every issue is one the conversion has not reached, so each filter is read against
// the outline the page draws with no filter on and every branch open: the same reads, one tree.
test("the five controls are toggles that draw their query's answer @phone", async ({ browser }) => {
  // Five widths of the row, every filter pressed on and off, and the whole outline unfolded once
  // to read each filter against: a minute and a half on an issue pod's two CPUs.
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);

    // One row of five toggles, in R2's order, each named by its name alone with its count
    // beside it; none is called Tree, and the page opens with no filter on.
    await expect(presetToolbar(page), "no toolbar named Filters holds the five").toHaveCount(1);
    await expect(presetToggles(page)).toHaveCount(5);
    expect(
      await presetToggles(page).evaluateAll((buttons) =>
        buttons.map((button) => button.querySelector("[data-preset-name]")?.textContent ?? "")
      ),
      "the row's controls are not Conversation, Spec, Verification, Plan and Decisions"
    ).toEqual(PRESET_ORDER.map((preset) => PRESET_NAMES[preset]));
    await expect(presetRow(page).getByRole("button", { name: /tree/i })).toHaveCount(0);
    for (const preset of PRESET_ORDER) {
      await expect(presetToggle(page, preset), `no toggle named ${preset}`).toHaveCount(1);
      await expect(presetCount(page, preset), `${preset} draws no count`).toHaveCount(1);
      await expect(presetToggle(page, preset)).toHaveAttribute(
        "aria-pressed",
        preset === "conversation" ? "true" : "false"
      );
    }
    await expect(versionControl(page), "a version control is drawn with no filter on").toHaveCount(
      0
    );

    // The outline with no filter on, every branch open: the nodes each filter is read against.
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const whole = await drawnRows(page);
    const wholeParents = parentsOf(whole);
    const kindOf = new Map(whole.map((row) => [row.id ?? "", row.type ?? ""]));

    for (const filter of ["spec", "verification", "plan", "decisions"] as const) {
      const { counted, rows: kinds } = FILTER_KINDS[filter];
      // Pressing a filter turns it on, and the address is its own.
      await presetToggle(page, filter).click();
      await expect(page).toHaveURL(new RegExp(`${presetPath(seeded.key, filter)}$`));
      for (const other of PRESET_ORDER) {
        await expect(presetToggle(page, other)).toHaveAttribute(
          "aria-pressed",
          other === filter ? "true" : "false"
        );
      }
      await expectIssuePageDrawn(page);
      const name = PRESET_NAMES[filter];
      // Each filter draws as its approved picture does (ruling 5 of build-rulings-md).
      // Verification draws the body of `verification-filter/` and no row of the outline: each
      // statement as context with the checks that serve it under it and their newest results
      // (R2, R17, ruling 6), read here in the body's own terms.
      if (filter === "verification") {
        await expect
          .poll(async () => (await verificationCards(page)).length, {
            message: `${name} draws no statement`,
          })
          .toBeGreaterThan(0);
        const cards = await verificationCards(page);
        const wholeChecks = whole.filter((row) => row.type === "check");
        expect(
          cards.map((card) => card.id).sort(),
          `${name} draws other statements than the issue's`
        ).toEqual(
          [...kindOf]
            .filter(([, kind]) => kind === "requirement")
            .map(([id]) => id)
            .sort()
        );
        expect(
          cards.filter((card) => !card.context).map((card) => card.id),
          `${name} draws a statement that is no context row`
        ).toEqual([]);
        // Each check under every statement it serves, once under each, as the outline draws it.
        for (const check of new Set(wholeChecks.map((row) => row.id ?? ""))) {
          const placements = cards.filter((card) =>
            card.checks.some((drawnCheck) => drawnCheck.id === check)
          ).length;
          expect(
            placements,
            `${check} is drawn under ${placements} statements, not under each it serves`
          ).toBe(wholeChecks.filter((row) => row.id === check).length);
        }
        // Every check that holds a result draws its newest one, and one only.
        expect(
          cards
            .flatMap((card) => card.checks)
            .filter((check) => check.results !== (check.state === "no-result" ? 0 : 1))
            .map((check) => check.id),
          `${name} draws other than the newest result of a check`
        ).toEqual([]);
        // The count is the checks it draws, each counted once.
        expect(
          Number(await presetCount(page, filter).textContent()),
          `${name}'s count is not the checks it draws`
        ).toBe(new Set(wholeChecks.map((row) => row.id)).size);
        await expect(versionControl(page), `${name} draws no version control`).toHaveCount(1);
        // Pressed again, the filter turns off: the page with no filter on.
        await presetToggle(page, filter).click();
        await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
        await expect(presetToggle(page, "conversation")).toHaveAttribute("aria-pressed", "true");
        await expect(versionControl(page)).toHaveCount(0);
        continue;
      }
      // Plan opens on the chart of time (c-plan-order reads it); its control switches to the
      // ordered steps, which are the rows read here. Decisions folds the agents' decisions no
      // statement rests on at its foot, unfolded here so that every entry it holds is a row.
      if (filter === "plan") await planView(page, "steps").click();
      if (filter === "decisions") await decisionsFoot(page).click();
      const drawn = await drawnRows(page);

      // The rows are the filter's kinds and no other: no issue row, no comment or message.
      expect(
        drawn
          .filter((row) => !(kinds as readonly string[]).includes(row.type ?? ""))
          .map((row) => `${row.type}:${row.id}`),
        `${name} draws rows of kinds it does not draw`
      ).toEqual([]);
      // Every node of those kinds the outline holds is drawn, and nothing else. One kind of
      // node is the exception the Decisions filter makes (R2, R9): the section of decisions.md
      // that restates a person's answer hangs under that answer in the outline, and is no entry
      // of the filter, whose entry for that decision is the answer itself.
      const restated = (id: string): boolean =>
        filter === "decisions" &&
        kindOf.get(id) === "decision" &&
        [...(wholeParents.get(id) ?? [])].some((parent) => kindOf.get(parent ?? "") === "ask");
      const wanted = [...kindOf]
        .filter(([id, kind]) => (kinds as readonly string[]).includes(kind) && !restated(id))
        .map(([id]) => id)
        .sort();
      expect([...new Set(idsOf(drawn))].sort(), `${name} draws other nodes than its kinds`).toEqual(
        wanted
      );
      // The count is the distinct nodes of its main kind.
      const main = new Set(
        drawn
          .filter((row) => (counted as readonly string[]).includes(row.type ?? ""))
          .map((row) => row.id)
      );
      expect(
        Number(await presetCount(page, filter).textContent()),
        `${name}'s count is not the ${counted.join(" and ")} it draws`
      ).toBe(main.size);
      // Only Verification draws context, and it is read above: no row of these three is one.
      expect(
        drawn.filter((row) => row.context).map((row) => row.type),
        `${name} marks rows as context`
      ).toEqual([]);

      // Every other parent is one line on its child's row, read from that parent: a row at the
      // top whose parent in the outline is no row here and not the issue carries the line, and
      // the line names that parent by its id, its kind and its first line.
      const drawnParents = parentsOf(drawn);
      let parentLines = 0;
      for (const row of drawn) {
        const id = row.id ?? "";
        const above = [...(wholeParents.get(id) ?? [])];
        const placedUnderRow = [...(drawnParents.get(id) ?? [])].some((parent) => parent !== null);
        const underIssue = above.includes(seeded.key);
        if (placedUnderRow || underIssue) {
          expect(
            row.parent,
            `${name} draws a parent line on ${id}, whose parent is drawn`
          ).toBeNull();
          continue;
        }
        expect(row.parent, `${name} draws ${id} with no line for its parent`).not.toBeNull();
        expect(above, `${name} names ${row.parent?.id} as ${id}'s parent`).toContain(
          row.parent?.id
        );
        expect(row.parent?.type).toBe(kindOf.get(row.parent?.id ?? ""));
        const parentRow = rowOf(whole, row.parent?.id ?? "");
        // The line holds the parent's own words: not empty, no longer than R1's first line, and
        // the start of what the parent's own row draws.
        const line = row.parent?.line ?? "";
        expect(line, `${id}'s parent line draws no words`).not.toBe("");
        expect(
          line.length,
          `${id}'s parent line is longer than R1's first line`
        ).toBeLessThanOrEqual(100);
        expect(
          parentRow?.words ?? parentRow?.firstLine ?? "",
          `${id}'s parent line is not its parent's first line`
        ).toContain(line.slice(0, 20));
        parentLines += 1;
      }
      // The seed's plan tasks serve statements, which Plan draws as no rows: the loop above read
      // at least one parent line there, so its assertions are not passing on nothing.
      if (filter === "plan") {
        expect(parentLines, "Plan drew no row with a parent line to read").toBeGreaterThan(0);
      }

      // The version control reads the filter's own document, and Decisions has none.
      await expect(versionControl(page), `${name} draws no version control`).toHaveCount(
        filter === "decisions" ? 0 : 1
      );

      // Pressed again, the filter turns off: the page with no filter on, at the issue's address.
      await presetToggle(page, filter).click();
      await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
      await expect(presetToggle(page, "conversation")).toHaveAttribute("aria-pressed", "true");
      await expect(versionControl(page)).toHaveCount(0);
    }

    // Pressing a filter turns any other off, and Conversation turns every filter off.
    await presetToggle(page, "spec").click();
    await presetToggle(page, "plan").click();
    await expect(page).toHaveURL(new RegExp(`${presetPath(seeded.key, "plan")}$`));
    await expect(presetToggle(page, "spec")).toHaveAttribute("aria-pressed", "false");
    await expect(presetToggle(page, "plan")).toHaveAttribute("aria-pressed", "true");
    await presetToggle(page, "conversation").click();
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
    await expect(presetToggle(page, "plan")).toHaveAttribute("aria-pressed", "false");

    // Each has its own address, which opens it.
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetPath(seeded.key, preset));
      await expect(presetToggle(page, preset)).toHaveAttribute("aria-pressed", "true");
    }

    // The two shapes, and no sideways scroll at any width between them. Each shape is read
    // through `expect.poll` for the reason `assertPageFits` is (`e2e/unclipped.ts`): a resize
    // settles on two clocks, the engine applying the breakpoint at once and the row re-rendering
    // a task or two later on the `useMediaQuery` state it reads.
    await gotoIssue(page, `/issues/${seeded.key}`);
    const presetShape = async () =>
      `${await presetToggles(page).count()}/${await presetChooser(page).count()}`;
    for (const width of [1440, 1280]) {
      await page.setViewportSize({ height: DESKTOP.height, width });
      await expect
        .poll(presetShape, { message: `at ${width} px the page draws no row of five` })
        .toBe("5/0");
      await assertPageFits(page);
      await assertWhole(presetRow(page));
    }
    // The row is the prototype's at every width that draws the five (ruling 5): each toggle its
    // `.tab`, 33 px of button over its 2 px rule, never the 44 px floor the rest of the app keeps
    // below 1280 px and never under 24 px (WCAG 2.5.8).
    for (const width of [1440, 1279, 1100] as const) {
      await page.setViewportSize({ height: DESKTOP.height, width });
      await expect.poll(presetShape).toMatch(/^(5\/0|0\/1)$/);
      await assertPageFits(page);
      await assertWhole(presetRow(page));
      if ((await presetToggles(page).count()) === 0) continue;
      const heights = await presetToggles(page).evaluateAll((buttons) =>
        buttons.map((button) => button.getBoundingClientRect().height)
      );
      console.log(`c-presets: toggle heights at ${width} px: ${heights.join(", ")}`);
      for (const height of heights) {
        expect(height, `a toggle is ${height} px tall at ${width} px`).toBeGreaterThanOrEqual(24);
        expect(height, `a toggle is ${height} px tall at ${width} px`).toBeLessThanOrEqual(36);
      }
    }
    for (const width of [1024, 390, 320]) {
      await page.setViewportSize({ height: PHONE.height, width });
      await expect
        .poll(presetShape, { message: `at ${width} px the row of five is still drawn` })
        .toBe("0/1");
      await assertPageFits(page);
    }
    // The one control names the one on with its signal, and opens the five.
    await expect(presetChooser(page)).toHaveAccessibleName(
      new RegExp(`^${PRESET_NAMES.conversation} \\d+`)
    );
    await presetChooser(page).click();
    expect(await openMenu(page).count(), "the one control opens no menu").toBe(1);
    for (const preset of PRESET_ORDER) {
      await expect(presetMenuItem(page, preset)).toHaveCount(1);
    }
    await presetMenuItem(page, "spec").click();
    await expect(page).toHaveURL(new RegExp(`${presetPath(seeded.key, "spec")}$`));
    await expect(presetChooser(page)).toHaveAccessibleName(
      new RegExp(`^${PRESET_NAMES.spec} \\d+`)
    );
    await assertPageFits(page);
  });
});

// c-spine. Each node under the parent the model stores (R4), read against the server's own
// edges on the two seeds that hold today's: an ask over the decision that answers it, `serves`
// over checks and plan tasks, a check over its results, and the entry a reply answers. On the
// issue the conversion has not reached every other typed block sits under the issue.
//
// What waits on another issue, and is not read here: a criterion under the words that raised
// it, a requirement under its criterion, and the left arrow walking up from a requirement to
// them (dispatch://INFRA-1253); a lens under its node and a finding under its lens
// (INFRA-1950); a reading under its decision (INFRA-1150); and a comment anchored to a node
// under that node, an edge the model does not store (INFRA-1149, INFRA-1253), so the anchored
// comment sits under the issue as any node with no stored parent does.
test("each node sits under the parent the model stores", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedSpineCases, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const decision = blockNodeId(decisionsSlug, seeded.decisionBlockId);
    const check = blockNodeId(verificationSlug, SPINE_CHECK_BLOCK_ID);

    // The decision sits under the ask it answers: the question its block names.
    const stored = ((await getIssueDecisions(seeded.key)).decisions ?? []).find(
      (one) => one.block_id === seeded.decisionBlockId
    );
    expect(stored?.asks, "the server holds no ask the decision answers").toEqual([
      seeded.answeredAskId,
    ]);
    expect(
      await parentOfRow(page, decision),
      "the decision is not drawn under the ask it answers"
    ).toBe(seeded.answeredAskId);
    expect(
      await parentOfRow(page, seeded.answeredAskId),
      "the answered ask is not drawn under the issue"
    ).toBe(seeded.key);

    // A requirement whose origin is absent sits under the issue, drawn as a node like any other
    // rather than as an error: the gap it draws is the one that tells it apart, that no check
    // serves it.
    const unexplained = blockNodeId(specSlug, SPINE_REQUIREMENTS.unexplained);
    expect(await parentOfRow(page, unexplained), "the unexplained requirement has no row").toBe(
      seeded.key
    );
    expect(
      await rowGaps(nodeRow(page, unexplained).first()).evaluateAll((drawn) =>
        drawn.map((gap) => gap.getAttribute("data-gap-kind"))
      ),
      "the requirement nothing explains draws a gap other than the one that tells it apart"
    ).toEqual(["no_check"]);
    expect(await gapKinds(page), "a gap the page draws names no kind").not.toContain("");

    // No requirement is drawn under a decision, and each sits under the issue.
    const rows = await drawnRows(page);
    const typeOf = new Map(rows.map((row) => [row.id ?? "", row.type]));
    for (const requirement of Object.values(SPINE_REQUIREMENTS)) {
      const parent = await parentOfRow(page, blockNodeId(specSlug, requirement));
      expect(typeOf.get(parent ?? ""), `${requirement} is drawn under a decision`).not.toBe(
        "decision"
      );
      expect(parent, `${requirement} is not drawn under the issue`).toBe(seeded.key);
    }

    // The check that serves two requirements sits under the first its `serves` names and is
    // shown under the other, each row naming its other parent, and both open the same block.
    const placed = rows.filter((row) => row.id === check);
    for (const row of placed) {
      expect(row.text, "a placement of the check does not name its other parent").toContain(
        CONTROL_LABELS.also_under
      );
    }
    expect(
      placed.map((row) => ancestorsIn(rows, check, rows.indexOf(row)).at(-1)),
      "the check is not under each of the two requirements it serves"
    ).toEqual([
      blockNodeId(specSlug, SPINE_REQUIREMENTS.servedTwiceA),
      blockNodeId(specSlug, SPINE_REQUIREMENTS.servedTwiceB),
    ]);
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

    // Every edge that is not the spine is a chip naming its target, and the spine is none: the
    // decision's link to a requirement is its chip, once, and the ask it sits under is not one.
    // A folded branch draws one line and no chip (ruling 1 on R1), so the tree is read open.
    await unfoldEveryBranch(page);
    const chips = (await drawnRows(page)).flatMap((row) => row.chips);
    expect(
      chips.filter((chip) => chip.includes(SPINE_REQUIREMENTS.shapedElsewhere)).length,
      `the decision's link is not drawn once as a chip: ${JSON.stringify(chips)}`
    ).toBe(1);
    expect(
      chips.filter((chip) => chip.includes(SPINE_ANSWERED_QUESTION)),
      "the ask the decision sits under is drawn as its chip as well"
    ).toEqual([]);
  });

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const slugs = new Set(((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.slug));
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const rows = await drawnRows(page);
    const parentOf = (id: string) => ancestorsIn(rows, id).at(-1);

    // The decision that answers the issue's answered ask sits under that ask.
    expect(
      parentOf(blockNodeId(decisionsSlug, seeded.decisions[1] ?? "")),
      "the decision that answers the ask is not drawn under it"
    ).toBe(seeded.answeredAskId);

    // Each check under the requirement its `serves` names, and each result under its check.
    for (const check of (await getIssueResults(seeded.key)).checks ?? []) {
      const id = blockNodeId(verificationSlug, check.block_id);
      const index = Number(check.block_id.slice(1));
      expect(parentOf(id), `${check.block_id} is not under the requirement it serves`).toBe(
        blockNodeId(specSlug, requirementBlockId(index))
      );
      for (const result of check.results ?? []) {
        expect(parentOf(result.id), `a result of ${check.block_id} is not under it`).toBe(id);
      }
    }
    // Each plan task under the requirement it serves, and the one that serves none under the
    // issue.
    seeded.planTasks.forEach((task, index) => {
      expect(
        parentOf(blockNodeId(planSlug, task)),
        `${task} is not under the requirement it serves`
      ).toBe(
        index === seeded.planTasks.length - 1
          ? seeded.key
          : blockNodeId(specSlug, requirementBlockId(index + 1))
      );
    });
    // Each reply under the entry it answers.
    expect(parentOf(seeded.messages.reply), "the reply is not under its message").toBe(
      seeded.messages.root
    );
    expect(
      parentOf(seeded.messages.replyToReply),
      "the reply to a reply is not under the reply it answers"
    ).toBe(seeded.messages.reply);
    seeded.comments.replies.forEach((reply, index) => {
      expect(parentOf(reply), "a comment's reply is not under the comment it answers").toBe(
        seeded.comments.threads[index % seeded.comments.threads.length]
      );
    });

    // Every typed block the model gives no parent sits under the issue - the open question's
    // block and every requirement - and no row sits under a document. The summary callout is
    // the head's three sentences and no row (R8).
    expect(
      rows.some((row) => row.id === blockNodeId(specSlug, seeded.summaryBlockId)),
      "the summary callout is drawn as a row of the outline"
    ).toBe(false);
    for (const id of [
      blockNodeId(specSlug, seeded.openQuestion.blockId),
      ...seeded.requirements.map((requirement) => blockNodeId(specSlug, requirement)),
    ]) {
      expect(parentOf(id), `${id}, which the model gives no parent, is not under the issue`).toBe(
        seeded.key
      );
    }
    expect(
      rows.filter((row) => slugs.has(parentOf(row.id ?? "") ?? "")).map((row) => row.id),
      "a node is drawn under a document"
    ).toEqual([]);
    const typeOfRow = new Map(rows.map((row) => [row.id ?? "", row.type]));
    expect(
      rows
        .filter((row) => row.type === "requirement")
        .filter((row) => typeOfRow.get(parentOf(row.id ?? "") ?? "") === "decision")
        .map((row) => row.id),
      "a requirement is drawn under a decision"
    ).toEqual([]);
  });
});

// c-three-things, as the architect's ruling 5 reads R8 (build-rulings-md): the three sentences
// stand each after its label; with no filter on they are in the Contents pane at 1440 px and in
// the Outline sheet at 390 px, never above the outline; under Spec they are the filter's first
// group; under Verification, Plan and Decisions they are not drawn; no Contents pane is drawn
// under a filter; and the two rows "How it gets done" and "What you will be shown" are drawn
// nowhere. Partial on today's data: who wrote the sentences and when they were last read back
// are dispatch://INFRA-1278, so the head draws neither and this test never asserts an author.
test("the three sentences stand with their labels in the Contents pane, the Spec filter and the Outline sheet @phone", async ({
  browser,
}) => {
  // Twelve loads of the page, each read for its head and the page's own draw.
  test.setTimeout(180_000);
  const labels = [
    CONTROL_LABELS.in_short_what,
    CONTROL_LABELS.in_short_why,
    CONTROL_LABELS.in_short_done,
  ];
  const spoken = async (head: Locator): Promise<string[]> =>
    (await head.locator("li [data-author-text]").allInnerTexts()).map((sentence) =>
      sentence.replace(/\s+/g, " ").trim()
    );
  const labelled = async (head: Locator): Promise<string[]> =>
    (await head.locator("li [data-page-chrome]").allInnerTexts()).map((label) => label.trim());
  /** The head `head` draws: the spec's own three sentences, each after its label, and neither
   *  of the two rows. The sentences are the summary node's words, which the batched reference
   *  read names last of the page's reads, so the reading waits for them. */
  const expectTheThree = async (head: Locator, where: string): Promise<void> => {
    await expect(head, `the three sentences are not drawn ${where}`).toHaveCount(1);
    await expect
      .poll(() => spoken(head), {
        message: `the spec's own three sentences are not drawn ${where}`,
      })
      .toEqual([...SUMMARY_SENTENCES]);
    expect(await labelled(head), `the three sentences carry no labels ${where}`).toEqual(labels);
    const words = (await head.innerText()).replace(/\s+/g, " ");
    for (const row of ["How it gets done", "What you will be shown"]) {
      expect(words, `"${row}" is drawn ${where}`).not.toContain(row);
    }
    expect(await head.getByRole("link").count(), `the head holds a link ${where}`).toBe(0);
  };

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // 1440 px, no filter: in the Contents pane, at the left of the outline.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(contentsPane(page), "no Contents pane is drawn with no filter on").toHaveCount(1);
    await expectTheThree(
      contentsPane(page).getByTestId("issue-three-things"),
      "in the Contents pane at 1440 px"
    );
    expect(await threeThings(page).count(), "the three sentences are drawn twice").toBe(1);
    const pane = await contentsPane(page).boundingBox();
    const outline = await tree(page).boundingBox();
    expect(pane, "the Contents pane has no box").not.toBeNull();
    expect(outline, "the outline has no box").not.toBeNull();
    expect(
      (pane?.x ?? 0) + (pane?.width ?? 0),
      "the Contents pane is not at the left of the outline"
    ).toBeLessThanOrEqual((outline?.x ?? 0) + 1);

    // Who wrote the sentences and when they were last read back wait on dispatch://INFRA-1278,
    // and no issue holds either, so the head draws no author and no gap for them.
    expect(
      await rowAuthor(threeThings(page)).count(),
      "the head names an author, which the model does not hold"
    ).toBe(0);
    expect(
      await rowGaps(threeThings(page)).count(),
      "the head draws a gap that every issue has, which tells the reader nothing about this one"
    ).toBe(0);

    // Under a filter, at both widths: no Contents pane; the Spec filter draws the three as its
    // first group, above its first statement; the other three draw none.
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        if (preset === "conversation") continue;
        const where = `with the ${PRESET_NAMES[preset]} filter at ${size.width} px`;
        // A filter's body draws rows of its own shape - a table under Verification, a chart
        // under Plan - so the page is waited for until it has stopped reading, which is what
        // says it has drawn everything it is going to.
        const traffic = watchApiRequests(page);
        await gotoIssue(page, presetPath(seeded.key, preset));
        await expectIssuePageDrawn(page);
        await expectLoadSettled(traffic);
        expect(await contentsPane(page).count(), `a Contents pane is drawn ${where}`).toBe(0);
        if (preset !== "spec") {
          expect(await threeThings(page).count(), `the three sentences are drawn ${where}`).toBe(0);
          continue;
        }
        await expectTheThree(threeThings(page), where);
        const head = await threeThings(page).boundingBox();
        const firstRow = await nodeRows(page).first().boundingBox();
        expect(
          head?.y ?? Number.POSITIVE_INFINITY,
          `the three sentences are not the Spec filter's first group ${where}`
        ).toBeLessThan(firstRow?.y ?? 0);
      }
    }

    // 390 px, no filter: not above the outline, and in the sheet the Outline control opens.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(() => nodeRows(page).count(), { message: "no row is drawn at 390 px" })
      .toBeGreaterThan(0);
    expect(await contentsPane(page).count(), "a Contents pane is drawn at 390 px").toBe(0);
    expect(
      await threeThings(page).count(),
      "the three sentences are drawn above the outline at 390 px"
    ).toBe(0);
    await outlineControl(page).click();
    await expectTheThree(
      outlineSheet(page).getByTestId("issue-three-things"),
      "in the Outline sheet at 390 px"
    );
  });

  await phase(browser, seedNoSummary, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    // The line waits on the spec's blocks read, which says whether the spec holds a summary
    // node at all: until it answers the head says nothing about one
    // (`web/src/features/issue/IssueThreeThings.tsx`), so the reading waits for the line.
    await expect(
      threeThings(page),
      "the head says something other than the page's own line for a spec with no summary"
    ).toContainText(EMPTY_STATE_LINES.spec_summary);
    expect(
      await threeThings(page).locator("li").count(),
      "a spec with no summary draws sentences all the same"
    ).toBe(0);
  });
});

// c-open-questions (R7). The page opens with the keyboard focus on the first open question that
// waits on the reader, in the outline's order, its row inside the viewport with nothing unfolded
// or scrolled by hand; `q` moves to the next one; and where none waits on the reader the page
// opens with the focus on the outline's first row. The second question is asked after forty
// older nodes, so the scroll is what puts it in the viewport: on a seed whose one question is
// the outline's first row, the row is on the first screen whatever the page does.
test("an open question is unfolded and on the first screen @phone", async ({ browser }) => {
  // Eight cold loads of an issue of some ninety rows, at two sizes.
  test.setTimeout(240_000);
  /** Whether node `id`'s row is whole inside what the reader sees: under the band that stays at
   *  the top of the screen and above the box at its foot. */
  const inView = (page: Page, id: string): Promise<boolean> =>
    nodeRow(page, id)
      .first()
      .evaluate((row) => {
        const style = getComputedStyle(document.documentElement);
        const band = Number.parseFloat(style.getPropertyValue("--issue-identity-height")) || 0;
        const foot = Number.parseFloat(style.getPropertyValue("--issue-composer-height")) || 0;
        const card = row.querySelector("[data-node-card]") ?? row;
        const box = card.getBoundingClientRect();
        return box.top >= band - 1 && box.top < window.innerHeight - foot;
      });
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const first = blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID);
    // Forty nodes a person wrote, each a row of the outline, and then a question asked of the
    // issue itself, which the outline draws after them, by the time it was asked.
    for (let older = 1; older <= 40; older += 1) {
      await createComment(seeded.key, { body: `Older words, number ${older}.` });
    }
    const later = await createAsk(
      seeded.key,
      {
        options: [{ label: "Left" }, { label: "Right" }],
        question: "Which of the two ships first?",
      },
      asPlanner
    );

    // Both wait on the reader: the page opens on the first of them in the outline's order.
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect
        .poll(() => focusedNodeId(page), {
          message: `the page does not open with the focus on the first open question that waits on the reader at ${at}`,
        })
        .toBe(first);
      expect(
        await inView(page, first),
        `the focused question is not in the viewport at ${at}`
      ).toBe(true);
    }

    // The next-question key moves to the next open question and says so, at the row, when there
    // is none; the reader's next move lets that line go.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect.poll(() => focusedNodeId(page)).toBe(first);
    await page.keyboard.press("q");
    await expect
      .poll(() => focusedNodeId(page), { message: "q did not reach the second open question" })
      .toBe(later.id);
    expect(await inView(page, later.id), "q left the second question off screen").toBe(true);
    await page.keyboard.press("q");
    await expect(
      nodeRow(page, later.id).first().locator("[data-row-report]"),
      "with no further open question the key says nothing at the row it was pressed on"
    ).toHaveCount(1);
    expect(await pageStatus(page).count(), "the key's report is drawn at more than one row").toBe(
      1
    );
    expect(
      wordsIn(await pageStatus(page).first().innerText()).filter(
        (word) => !DECLARED_WORDS.includes(word)
      ),
      "that report uses words the page does not declare"
    ).toEqual([]);
    await page.keyboard.press("k");
    await expect(pageStatus(page), "the report stays after the reader moved on").toHaveCount(0);

    // With the first answered, the one question that waits is under forty older nodes: the page
    // opens on it, and the scroll is what puts it in the viewport.
    await answerAsk(seeded.openQuestion.askId, {
      expected_edited_at: null,
      selected: [],
      text: "1280 px.",
    });
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect
        .poll(() => focusedNodeId(page), {
          message: `the page does not open with the focus on the question under forty older nodes at ${at}`,
        })
        .toBe(later.id);
      await expect
        .poll(() => inView(page, later.id), {
          message: `the question under forty older nodes is not in the viewport at ${at}`,
        })
        .toBe(true);
      expect(
        await page.evaluate(() => window.scrollY),
        `the page did not scroll to the question at ${at}, so the seed put it on the first screen`
      ).toBeGreaterThan(size.height);
    }

    // With neither waiting on the reader, the page opens with the focus on the outline's first
    // row, at the top.
    await answerAsk(later.id, { expected_edited_at: null, selected: ["Left"] });
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect
        .poll(async () => (await focusedNodeId(page)) !== null, {
          message: `the page opens with the focus on no row at ${at}`,
        })
        .toBe(true);
      const rows = await drawnRows(page);
      expect(
        await focusedNodeId(page),
        `the page does not open with the focus on the outline's first row at ${at}`
      ).toBe(rows[0]?.id ?? "");
      expect(await page.evaluate(() => window.scrollY), `the page opens scrolled at ${at}`).toBe(0);
    }
  });
});

// c-children, as the architect's ruling 5 reads R12 (build-rulings-md): no view draws a
// children's section above the outline; the children are one group of rows of the outline, where
// the model places them, which for every child today is under the issue; each child's row holds
// its title, its status, what blocks it and who holds it, with its key as the tag; the header
// keeps the bar and both counts; and `/children` and `t h` move the focus to that group.
test("the children are rows of the outline, and the header keeps the bar and both counts @phone", async ({
  browser,
}) => {
  // Two whole seeds and some fifteen cold loads of the issue page.
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // No seed claims a child, and the check reads who holds the running one, so this test puts a
    // child in progress and claims it: that is the state the assertion is read against.
    const running = seeded.children.keys[0] ?? "";
    await patchIssue(running, { status: "in_progress" });
    await claimIssue(running, asPlanner);
    const blocked = seeded.children.blocked;

    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        // A filter's body draws rows of its own shape, so the page is waited for until it has
        // stopped reading rather than for a row of the outline.
        const traffic = watchApiRequests(page);
        await gotoIssue(page, presetPath(seeded.key, preset));
        await expectIssuePageDrawn(page);
        await expectLoadSettled(traffic);
        expect(
          await childrenSection(page).count(),
          `the ${PRESET_NAMES[preset]} view draws a children's section above the outline at ${size.width} px`
        ).toBe(0);
        // The header keeps the bar and both counts at every width, with every view.
        const direct = identityBlock(page).getByTestId("issue-progress-children");
        const whole = identityBlock(page).getByTestId("issue-progress-subtree");
        await expect(
          direct,
          `the header draws no children's count at ${size.width} px`
        ).toBeVisible();
        await expect(
          whole,
          `the header draws no whole-tree count at ${size.width} px`
        ).toBeVisible();
        // The bar is the count's own shape, drawn beside its words and hidden from a reader of
        // the words, who hears the count.
        expect(
          await direct.locator('[aria-hidden="true"] > *').count(),
          `the header draws no completion bar at ${size.width} px`
        ).toBeGreaterThan(0);
        expect(
          (await direct.innerText()).replace(/\s+/g, " "),
          `the header's count does not read one of ${seeded.children.keys.length} children done`
        ).toMatch(new RegExp(`1\\D+${seeded.children.keys.length}`));
        if (preset !== "conversation") continue;

        // With no filter on, each child is a row of the outline, the group of them together,
        // under no other row: the issue itself is no row.
        await expect(
          childRows(page),
          `the outline draws other than one row a child at ${size.width} px`
        ).toHaveCount(seeded.children.keys.length);
        const rows = await drawnRows(page);
        const places = seeded.children.keys.map((key) => idsOf(rows).indexOf(key));
        expect(
          places.map((place, index) => place - (places[0] ?? 0) - index),
          `the children are not one group of rows at ${size.width} px`
        ).toEqual(places.map(() => 0));
        for (const key of seeded.children.keys) {
          expect(
            ancestorsIn(rows, key),
            `the child ${key} sits under another row at ${size.width} px`
          ).toEqual([]);
        }
      }
    }

    // What a child's row holds: its title as its words, how it stands, what blocks it, who
    // holds it, and its key as the tag, which opens the child's own page.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(childRows(page)).toHaveCount(seeded.children.keys.length);
    for (const key of seeded.children.keys) {
      const child = await getIssue(key);
      const row = childRow(page, key).first();
      await expect(rowWords(row), `the row for ${key} does not draw its title`).toContainText(
        child.title
      );
      expect(
        await row.locator("[data-child-facts]").getAttribute("data-child-status"),
        `the row for ${key} does not name its status`
      ).toBe(child.status);
      const tag = row.locator("[data-child-key]");
      await expect(tag, `the row for ${key} does not carry its key as the tag`).toHaveText(key);
      expect(await tag.getAttribute("href"), `the tag of ${key} opens another page`).toBe(
        `/issues/${key}`
      );
      // Every colour is the page's own palette: the old application's link blue is gone.
      expect(
        await row
          .locator("a")
          .evaluateAll((links) => links.map((link) => getComputedStyle(link).color)),
        `a link of the row for ${key} is drawn in the old application's blue`
      ).not.toContain("rgb(0, 105, 168)");
    }
    expect(
      (await childRow(page, blocked).first().innerText()).replace(/\s+/g, " "),
      "the blocked child's row does not name the issue that blocks it"
    ).toContain(`Blocked by ${running}`);
    expect(
      (await childRow(page, running).first().innerText()).replace(/\s+/g, " "),
      "the running child's row does not say who holds it"
    ).toMatch(/Claimed by \S/);

    // The children's own address, and `t h`, put the focus on the group's first row, on screen;
    // from there the rows are moved through like any other.
    const [firstChild, secondChild] = seeded.children.keys;
    await gotoIssue(page, `/issues/${seeded.key}/children`);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "/children does not focus the group's first row",
      })
      .toBe(firstChild ?? "");
    expect(
      await onScreen(childRow(page, firstChild ?? "").first()),
      "/children leaves it off screen"
    ).toBe(true);
    await page.keyboard.press("j");
    await expect
      .poll(() => focusedNodeId(page), { message: "j does not move to the next child's row" })
      .toBe(secondChild ?? "");
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(childRows(page)).toHaveCount(seeded.children.keys.length);
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    await expect
      .poll(() => focusedNodeId(page), { message: "t h does not focus the group's first row" })
      .toBe(firstChild ?? "");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/children$`));
  });

  await phase(browser, seedNoChildren, async (page, seeded) => {
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetPath(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await childrenSection(page).count(),
        `an issue with no children draws a children section with the ${PRESET_NAMES[preset]} view`
      ).toBe(0);
      expect(
        await childRows(page).count(),
        `an issue with no children draws a child's row with the ${PRESET_NAMES[preset]} view`
      ).toBe(0);
    }
  });
});

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

// R16, the tester's F1, and the placement of a new write. What the reader sends from the box at
// the foot is drawn at once, its branch unfolded, inside the viewport and with the focus on it,
// whether it is a reply under a folded thread, a reply under an answered question, or a new
// comment or question on the issue, which both land at the outline's end.
test("what the reader sends from the box is drawn, in view and focused, wherever it lands", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(nodeRow(page, seeded.head).first()).toBeVisible();
    /** The row drawing `words`, which the reader has just sent: drawn, focused and on screen. */
    const expectLandedOn = async (words: string, how: string): Promise<string> => {
      const row = nodeRows(page).filter({ hasText: words }).first();
      await expect(row, `${how}: the reader's own new words are not drawn`).toBeVisible();
      const id = (await row.getAttribute("data-node-id")) ?? "";
      await expect
        .poll(() => focusedNodeId(page), { message: `${how}: the focus is not on the new row` })
        .toBe(id);
      expect(await onScreen(row), `${how}: the new row is off screen`).toBe(true);
      return id;
    };
    /** Sends `words` from the box with the keyboard alone, after `key` on node `id`'s row. */
    const sendWithKeys = async (id: string, key: string, words: string): Promise<void> => {
      await focusRow(page, id);
      await page.keyboard.press(key);
      await expect(composerField(page), `${key} put no caret in the box`).toBeFocused();
      await page.keyboard.type(words);
      await page.keyboard.press("Enter");
    };

    // A reply under a thread the page folds: `r`, the words, Enter.
    await expect(
      nodeRow(page, seeded.first),
      "the seed's thread is open, so a reply under it proves nothing about the fold"
    ).toHaveCount(0);
    await sendWithKeys(seeded.head, "r", "A reply under the folded thread.");
    const reply = await expectLandedOn("A reply under the folded thread.", "a reply under a fold");
    expect(await parentOfRow(page, reply), "the reply is not drawn under the row it answers").toBe(
      seeded.head
    );
    // The focus is the reader's place: the next move goes on from the new row, to the row
    // after it, and not from the page with nothing focused, which starts at the first row.
    await page.keyboard.press("j");
    await expect
      .poll(
        async () => {
          const on = await focusedNodeId(page);
          return on === reply ? "still on the new row" : on === seeded.head ? "the first row" : on;
        },
        { message: "after a keyboard send the next key does not go on from the new row" }
      )
      .toBe(seeded.answeredAsk);

    // A reply under an answered question.
    await sendWithKeys(seeded.answeredAsk, "r", "A word under the answered question.");
    const under = await expectLandedOn(
      "A word under the answered question.",
      "a reply under an answered question"
    );
    expect(await parentOfRow(page, under), "that reply is not drawn under the question").toBe(
      seeded.answeredAsk
    );

    // A new comment and a new question on the issue both land at the outline's end, in the
    // order they were written, each where the reader is taken.
    await composerField(page).click();
    await sendFromComposer(page, "A new comment on the issue.");
    const comment = await expectLandedOn("A new comment on the issue.", "a new comment");
    await composerField(page).click();
    await composer(page).locator('[data-composer-kind="ask"]').click();
    await sendFromComposer(page, "A new question of the issue?");
    const question = await expectLandedOn("A new question of the issue?", "a new question");
    const top = (await drawnRows(page)).filter((row) => row.level === "1").map((row) => row.id);
    expect(
      top.slice(-2),
      "a new comment and a new question do not land together at the outline's end"
    ).toEqual([comment, question]);
  });
});

// The tester's F10 (and the reviewer's IssueComposer.tsx:160): a send made with the keyboard
// alone - `a` on an open question, the words, Enter - leaves the focus on a row of the outline,
// the question's own, and the next key goes on from there, not from the outline's first row.
test("a send with the keyboard leaves the focus on the reader's row of the outline", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await focusRow(page, seeded.openAsk);
    await page.keyboard.press("a");
    await expect(composerField(page), "a put no caret in the box").toBeFocused();
    await page.keyboard.type("Left, in my own words.");
    await page.keyboard.press("Enter");
    await expect(nodeRow(page, seeded.openAsk).first()).toContainText("Left, in my own words.");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "after an answer sent with the keyboard the focus is not on the question's row",
      })
      .toBe(seeded.openAsk);
    await page.keyboard.press("k");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the next key does not go on from the question's row",
      })
      .toBe(seeded.resolvedAsk);
  });
});

// The tester's F16: what a key says when it has nowhere to go - `w` on a row no decision stands
// behind - is drawn at the row it was pressed on, once, and the reader's next move lets it go.
test("a key with nowhere to go says so at its row, and the next move lets it go", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await focusRow(page, seeded.answeredAsk);
    await page.keyboard.press("w");
    await expect(
      nodeRow(page, seeded.answeredAsk).first().locator("[data-row-report]"),
      "the key's report is not drawn at the row it was pressed on"
    ).toHaveCount(1);
    expect(
      await issuePage(page).locator("[data-row-report]").count(),
      "the key's report is drawn at more than one place"
    ).toBe(1);
    expect(
      (await issuePage(page).getByRole("status").allInnerTexts()).filter(
        (words) => words.trim() !== ""
      ).length,
      "the key's report is drawn a second time away from the row"
    ).toBe(1);
    await page.keyboard.press("j");
    await expect(
      issuePage(page).locator("[data-row-report]"),
      "the report stays after the reader moved on"
    ).toHaveCount(0);
  });
});

// The box at the foot (the tester's F2 and F9, the architect's ruling 5): empty and at rest it is
// one line with Send, as the pictures draw it, at 390 px as at 1440 px; while it replies its
// placeholder says where the words go; and on a phone the box that answers a question has the
// line's width, with the options under it.
test("the box at the foot is one line at rest, and says where a reply goes @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(composerField(page), `the box is not drawn at ${at}`).toBeVisible();
      // At rest: no caret in it. The page opens with the focus on a row of the outline.
      await expect.poll(async () => (await focusedNodeId(page)) !== null).toBe(true);
      const box = await composer(page).locator("form").first().boundingBox();
      expect(box, `the box has no box at ${at}`).not.toBeNull();
      console.log(`the box at rest at ${at}: ${box?.width.toFixed(0)} x ${box?.height.toFixed(0)}`);
      expect(
        box?.height ?? Number.POSITIVE_INFINITY,
        `the empty box at rest is ${box?.height.toFixed(0)} px tall at ${at}, where the picture draws one line, 38 px`
      ).toBeLessThanOrEqual(40);
      await expect(composerField(page)).toHaveAttribute(
        "placeholder",
        CONTROL_LABELS.write_placeholder
      );
      // The box stands at the foot of the screen.
      const foot = await composer(page).boundingBox();
      expect(
        Math.round((foot?.y ?? 0) + (foot?.height ?? 0)),
        `the box does not stand at the foot of the screen at ${at}`
      ).toBe(size.height);

      // While it replies, its placeholder says where the words go.
      await focusRow(page, seeded.head);
      await page.keyboard.press("r");
      await expect(composerField(page)).toBeFocused();
      await expect(
        composerField(page),
        `while replying, the box still says its words go to the top of the conversation at ${at}`
      ).toHaveAttribute("placeholder", CONTROL_LABELS.reply_placeholder);
      await page.keyboard.press("Escape");

      // The box that answers a question: its field has room to type in.
      await focusRow(page, seeded.openAsk);
      await page.keyboard.press("a");
      await expect(composerField(page)).toBeFocused();
      const field = await composerField(page).boundingBox();
      console.log(
        `the answer field at ${at}: ${field?.width.toFixed(0)} x ${field?.height.toFixed(0)}`
      );
      expect(
        field?.width ?? 0,
        `the answer's field is ${field?.width.toFixed(0)} px wide at ${at}`
      ).toBeGreaterThanOrEqual(240);
      await page.keyboard.press("Escape");
      await expect
        .poll(() => focusedNodeId(page), {
          message: `Escape from the answering box does not give the question's row back at ${at}`,
        })
        .toBe(seeded.openAsk);
    }
  });
});

// c-tree, the places that draw one line for a node (the architect's ruling 1): a Contents entry
// and a crumb are the node's first line by R1's rule, cut at a word, never cut again mid-word by
// their box, and a line that is a whole sentence carries no ellipsis after its own full stop.
// The issue itself is no entry of the list, and its crumb is its key (ruling 5).
test("a Contents entry and a crumb draw a node's first line, cut at a word", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const words = new Map<string, string>([
      ...(await listComments(seeded.key)).map((one) => [one.id, one.body] as const),
      ...(await listIssueAsks(seeded.key)).map((one) => [one.id, one.question] as const),
    ]);
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const entries = contentsEntries(contentsPane(page));
    await expect
      .poll(() => entries.count(), { message: "the list holds no entry" })
      .toBeGreaterThan(0);
    const drawn = await entries.evaluateAll((rows) =>
      rows.map((row) => ({
        id: row.getAttribute("data-contents-entry") ?? "",
        line: (row.querySelector("[data-contents-line]")?.textContent ?? "").trim(),
      }))
    );
    // A line that is a whole sentence carries no ellipsis after its own full stop: "Thread 1
    // opens here.…" read as four dots.
    expect(
      drawn.filter((entry) => /[.!?]…$/.test(entry.line)).map((entry) => entry.line),
      "an entry ends a sentence and then an ellipsis"
    ).toEqual([]);
    expect(
      drawn.map((entry) => entry.id),
      "the issue itself is an entry of the list, which repeats the header"
    ).not.toContain(seeded.key);
    const thread = drawn.find((entry) => entry.id === seeded.comments.threads[0]);
    expect(thread, "the reader's own thread is no entry of the list").toBeDefined();
    for (const entry of drawn) {
      const whole = words.get(entry.id);
      if (whole === undefined) continue;
      const first = firstLineOf(whole).line;
      const cut = entry.line.endsWith("…") ? entry.line.slice(0, -1) : entry.line;
      expect(first.startsWith(cut), `the entry ${entry.id} draws other than its first line`).toBe(
        true
      );
      expect(
        cut.length === first.length || first.charAt(cut.length) === " ",
        `the entry ${entry.id} is cut inside a word: "${entry.line}"`
      ).toBe(true);
    }

    // The crumb, where the tree is read one node at a time: zoomed into a reply of a thread, the
    // issue's crumb is its key and the thread's is its first line, whole.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}/comments/${seeded.comments.replies[0] ?? ""}`);
    await expect
      .poll(() => focusedNodeId(page), { message: "the reply's address does not focus its row" })
      .toBe(seeded.comments.replies[0] ?? "");
    await page.keyboard.press(".");
    const crumbs = tree(page).locator("xpath=..").locator("[data-crumb]");
    await expect.poll(() => crumbs.count(), { message: "no crumb is drawn" }).toBeGreaterThan(1);
    const texts = (await crumbs.allInnerTexts()).map((crumb) => crumb.trim());
    expect(texts[0], "the issue's crumb repeats its title").toBe(seeded.key);
    for (const crumb of texts.slice(1)) {
      expect(crumb, `the crumb "${crumb}" ends a sentence and then an ellipsis`).not.toMatch(
        /[.!?]…$/
      );
      expect(crumb.length, `the crumb "${crumb}" is longer than a first line`).toBeLessThanOrEqual(
        101
      );
    }
    expect(
      await crumbs.evaluateAll(
        (all) =>
          all.filter(
            (crumb) =>
              getComputedStyle(crumb).textOverflow === "ellipsis" ||
              crumb.scrollWidth > crumb.clientWidth + 1
          ).length
      ),
      "a crumb is cut by its box, which cuts inside a word"
    ).toBe(0);
  });
});

/** Twenty labels, the most Dispatch lets an issue hold (`maxIssueLabels`,
 *  `packages/envoy/internal/dispatch/api/issue_queries.go`): more than the header's line holds
 *  at 1440 px and at 390 px, so it ends with the count of those left over. */
const TWENTY_LABELS = [
  "dispatch",
  "layout",
  "redesign",
  "header",
  "details",
  "keyboard",
  "phone",
  "typeface",
  "accessibility",
  "approval",
  "outline",
  "presets",
  "contents",
  "composer",
  "decisions",
  "verification",
  "plan",
  "children",
  "routing",
  "web",
] as const;

/** Every element the header draws that a reader reads or presses, by what it is: its test id,
 *  or its tag with the name a reader hears. Text it lays out unseen is left out. */
function headerParts(page: Page): Promise<string[]> {
  return identityBlock(page).evaluate((header) =>
    [...header.querySelectorAll("a, button, select, h1, textarea, [data-testid]")]
      .filter((part) => part.checkVisibility())
      .map(
        (part) =>
          part.getAttribute("data-testid") ??
          `${part.tagName.toLowerCase()} ${part.getAttribute("aria-label") ?? (part.textContent ?? "").replace(/\s+/g, " ").trim()}`
      )
  );
}

/**
 * The header's controls a phone reader presses that fail WCAG 2.2's target size (2.5.8) as
 * written: each target at least 24 by 24 CSS px, or, where one is smaller, a 24 px circle
 * centred on it that overlaps no other target. The header's controls are 24 px on a phone, as
 * the prototype draws them, because the header there is held to eight lines and 250 px, nine
 * and 280 px while an approval request waits on the reader (ruling 8), which leaves no room for
 * the 44 px the rest of a phone page gives a control.
 */
function undersizedTargets(page: Page): Promise<string[]> {
  return identityBlock(page).evaluate((header) => {
    const targets = [...header.querySelectorAll<HTMLElement>("a, button, select")]
      .filter((target) => target.checkVisibility())
      .map((target) => ({
        box: target.getBoundingClientRect(),
        name: target.getAttribute("aria-label") ?? (target.textContent ?? "").trim(),
      }));
    const overlaps = (centre: { x: number; y: number }, box: DOMRect) => {
      const x = Math.max(box.left, Math.min(centre.x, box.right));
      const y = Math.max(box.top, Math.min(centre.y, box.bottom));
      return (x - centre.x) ** 2 + (y - centre.y) ** 2 < 12 ** 2;
    };
    return targets
      .filter(({ box }, index) => {
        if (box.width >= 24 - 0.5 && box.height >= 24 - 0.5) return false;
        const centre = { x: box.left + box.width / 2, y: box.top + box.height / 2 };
        return targets.some((other, at) => at !== index && overlaps(centre, other.box));
      })
      .map(({ box, name }) => `${name}: ${box.width.toFixed(1)} x ${box.height.toFixed(1)}`);
  });
}

/** The iPhone 13's viewport, the suite's phone (`e2e/playwright.config.ts`'s `iphone` project),
 *  which is the screen the header's limit at 390 px is for. */
const IPHONE_13 = { height: 664, width: 390 } as const;

/** The viewport a row reads `width` in: the iPhone 13's at 390 px, else the suite's own height
 *  for a phone or a desktop width. */
function viewportAt(width: number): { readonly height: number; readonly width: number } {
  if (width === IPHONE_13.width) return IPHONE_13;
  return { height: width < 1024 ? PHONE.height : DESKTOP.height, width };
}

/**
 * R13's limit on the header, measured from the top of the identity block to the top of the
 * filter row, as dispatch://INFRA-1196/artifact/build-rulings-md ruling 8 sets it: 3 lines and
 * 130 px at 1440 px; at 390 px, on the 20-label seed and this issue's 115-character title drawn
 * whole - four lines of 20/26 px in the browser the checks run in, as the prototype itself is
 * there - with the prototype's 8 px above the filter control, 8 lines and 250 px while no
 * approval request waits on the reader and 9 lines and 280 px while one does, since Approve
 * and Request changes take a line of their own there. `firstRow` is how far down the outline's
 * first row may start at 390 px by the same ruling: 305 px and 335 px. No other width has a
 * limit.
 */
function headerLimit(
  width: number,
  requestWaiting: boolean
): { readonly firstRow?: number; readonly height: number; readonly lines: number } | undefined {
  if (width === DESKTOP.width) return { height: 130, lines: 3 };
  if (width !== IPHONE_13.width) return undefined;
  return requestWaiting
    ? { firstRow: 335, height: 280, lines: 9 }
    : { firstRow: 305, height: 250, lines: 8 };
}

/**
 * The header read against its limit at `width`, softly so one run reports every width and seed;
 * and at 390 px what the limit is for (ruling 8): the title drawn whole, with no clamp,
 * and the top of the outline's first row within its own limit, in the iPhone 13's 390 x 664
 * viewport with the page at its top. `drawnIn` names the typeface the page is drawn in, for the
 * message.
 */
async function expectHeaderWithinItsLimit(
  page: Page,
  width: number,
  requestWaiting: boolean,
  drawnIn: string
): Promise<void> {
  const limit = headerLimit(width, requestWaiting);
  if (limit === undefined) return;
  const where = `at ${width} px in ${drawnIn} with ${requestWaiting ? "a request" : "no request"} waiting`;
  // Read with the page at its top, once the outline has drawn: at 760 px and below the filter
  // row stays at the top of the screen while the header scrolls away, so on a page a load or a
  // key has scrolled past the header the row stands lower than the header's own end.
  await expect(
    tree(page).locator("[data-node-row]").first(),
    `no row is drawn ${where}`
  ).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 0));
  const reading = await headerReading(page);
  if (reading === null) throw new Error(`the page draws no header or no filter row ${where}`);
  const drawnAs = `${reading.lines.length} lines, ${reading.height.toFixed(1)} px`;
  console.log(`header ${where}: ${drawnAs}`);
  test.info().annotations.push({ description: `${where}: ${drawnAs}`, type: "header" });
  expect
    .soft(
      reading.lines.length,
      `the header takes ${drawnAs} ${where}: ${JSON.stringify(reading.lines)}`
    )
    .toBeLessThanOrEqual(limit.lines);
  expect
    .soft(reading.height, `the header takes ${drawnAs} ${where}`)
    .toBeLessThanOrEqual(limit.height);
  if (limit.firstRow === undefined) return;

  await assertWhole(identityBlock(page).getByRole("heading", { level: 1 }));
  const firstRow = tree(page).locator("[data-node-row]").first();
  await expect(firstRow, `the outline draws no row ${where}`).toBeVisible();
  const top = await firstRow.evaluate((row) => {
    for (let box = row.parentElement; box !== null; box = box.parentElement) box.scrollTop = 0;
    window.scrollTo(0, 0);
    return row.getBoundingClientRect().top;
  });
  console.log(`header ${where}: the outline's first row starts at ${top.toFixed(1)} px`);
  test.info().annotations.push({
    description: `${where}: the outline's first row starts at ${top.toFixed(1)} px`,
    type: "outline",
  });
  expect
    .soft(
      top,
      `the outline's first row starts ${top.toFixed(1)} px down, past ${limit.firstRow} px, ${where}`
    )
    .toBeLessThanOrEqual(limit.firstRow);
}

// c-properties.
test("every property with a value is visible at every width @phone", async ({ browser }) => {
  // Six loads of the seeded issue: two with no approval request waiting, read against the
  // header's limit, and four with one waiting, each read in the header and in Details; then the
  // two-label issue, the parent, an issue that advances nothing and the child that inherits a
  // priority.
  test.setTimeout(300_000);
  await phase(browser, seedWholeIssueAdvancing, async (page, seeded) => {
    // This issue's own title and every property set, with twenty labels; and a parent whose
    // route reaches nobody, which is the one issue that draws the header's route marker.
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: MEASURED_TITLE });
    await patchIssue(seeded.parentKey, { route: "role:nobody-holds-this" });
    const desktopProject = test.info().project.name !== "iphone";

    // The first seed of the header's limit (dispatch://INFRA-1196/artifact/build-rulings-md,
    // ruling 8): no approval request waits on the reader, so the approval control reads Not
    // requested and carries no Approve, and the header holds its limit for that case.
    for (const width of desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(identityBlock(page)).toContainText(MEASURED_TITLE);
      await expect(identityBlock(page)).toContainText(CONTROL_LABELS.not_requested);
      await expect(identityBlock(page).getByRole("button", { name: "Approve" })).toHaveCount(0);
      await expectHeaderWithinItsLimit(page, width, false, TYPEFACE);
    }

    // Its second: an approval request that waits on the reader here and on no other issue.
    await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
    // An issue that waits on this one, which is what Details' Blocks row reads.
    const waiter = await createIssue({ force: true, project: PROJECT, title: "Waits on it" });
    await patchIssue(waiter.key, { blocked_by: [seeded.key] });
    // R13's list, each part of the header by what it is: nothing else may be drawn there. Its one
    // link is the parent's, named exactly, so no property Details holds as a link - the pull
    // request, a blocker, a priority - passes for it.
    const named = [
      new RegExp(`^a ${seeded.parentKey}$`),
      /^button Open navigation$/,
      /^button Copy issue key /,
      /^button Issue actions$/,
      /^button Details$/,
      /^h1 /,
      /^select Status$/,
      new RegExp(`^select Priority of ${seeded.key}$`),
      new RegExp(`^select Assignee of ${seeded.key}$`),
      /^issue-header$/,
      /^issue-details-control$/,
      /^issue-state-actions$/,
      /^issue-header-labels$/,
      /^issue-labels-left$/,
      /^issue-progress-children$/,
      /^issue-progress-subtree$/,
      /^issue-whose-turn$/,
      /^issue-approval-steps$/,
      /^button (Awaiting approval|Approve|Request changes)/,
      /^issue-route-unreachable$/,
    ];

    for (const width of desktopProject ? [1440, 1280, 1024, 390] : [390]) {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(identityBlock(page)).toContainText(MEASURED_TITLE);
      // At 760 px and below the header scrolls with the page (ruling 5), and a load may have
      // moved the page to what waits: the header is read with the page at its top.
      await expect(tree(page).locator("[data-node-row]").first()).toBeVisible();
      await page.evaluate(() => window.scrollTo(0, 0));

      // The header's own values, each where it is drawn, with no control opened.
      const header = identityBlock(page);
      const inTheHeader: readonly (readonly [string, Locator, (string | RegExp)?])[] = [
        ["the parent", header.getByRole("link", { exact: true, name: seeded.parentKey })],
        ["the status", header.getByRole("combobox", { name: "Status" })],
        ["the priority", badgeValue(page, `Priority of ${seeded.key}`), "P1"],
        ["the assignee", badgeValue(page, `Assignee of ${seeded.key}`), "alice"],
        ["the first label", headerLabels(page).first(), TWENTY_LABELS[0]],
        ["the children's progress", header.getByTestId("issue-progress-children")],
        ["the whole tree's progress", header.getByTestId("issue-progress-subtree")],
        ["the count of what waits", waitingCount(page), /^Waiting on you \(\d+\)$/],
        ["Approve", header.getByRole("button", { exact: true, name: "Approve" })],
        ["Request changes", header.getByRole("button", { exact: true, name: "Request changes" })],
      ];
      for (const [name, at, words] of inTheHeader) {
        await expect(at, `${name} is not drawn in the header at ${width} px`).toBeVisible();
        if (words !== undefined) {
          await expect(at, `${name} does not read "${words}" at ${width} px`).toHaveText(words);
        }
        await expect(at, `${name} is outside the ${width} px viewport`).toBeInViewport();
        await assertWhole(at);
      }
      expect(
        await header.getByRole("combobox", { name: "Status" }).inputValue(),
        `the status reads otherwise at ${width} px`
      ).toBe("in_progress");

      // The header draws R13's list and nothing else.
      const parts = await headerParts(page);
      expect(
        parts.filter((part) => !named.some((pattern) => pattern.test(part))),
        `the header draws parts R13 does not name at ${width} px`
      ).toEqual([]);

      // The labels that fit, and a count equal to the labels the line does not draw, which
      // opens Details at its Labels row.
      await expect(labelsLeft(page), `the labels end on no count at ${width} px`).toBeVisible();
      const drawn = await headerLabels(page).count();
      expect(drawn, `the header draws every label at ${width} px`).toBeLessThan(20);
      await expect(labelsLeft(page)).toHaveText(
        `+${20 - drawn} ${20 - drawn === 1 ? CONTROL_LABELS.labels_left_one : CONTROL_LABELS.labels_left_many}`
      );

      await expectHeaderWithinItsLimit(page, width, true, TYPEFACE);
      if (width === IPHONE_13.width) {
        expect(
          await undersizedTargets(page),
          "a header control fails WCAG 2.5.8 at 390 px"
        ).toEqual([]);
      }
      await assertPageFits(page);

      // Every other property is in Details, each in its own row, read one by one.
      await labelsLeft(page).click();
      await expect(marginShell(page), "the labels' count opened no Details").toBeVisible();
      const labelsRow = detailsRow(page, "Labels");
      await expect
        .poll(async () =>
          (await labelsRow.locator("[data-author-text]").allTextContents()).filter((label) =>
            (TWENTY_LABELS as readonly string[]).includes(label)
          )
        )
        .toEqual([...TWENTY_LABELS]);
      const inDetails: readonly (readonly [string, Locator, string?])[] = [
        ["the repository", badgeValue(page, `Repository of ${seeded.key}`), WHOLE_REPO],
        [
          "the pull request",
          detailsRow(page, "Pull request").getByRole("link", { name: /^#11\b/ }),
        ],
        ["a component", detailsRow(page, "Components").getByText("Issue panel", { exact: true })],
        [
          "the claim",
          detailsRow(page, "Claimed by").getByRole("link", { exact: true, name: PLANNER_TITLE }),
        ],
        [
          "the route",
          detailsRow(page, "Messages go to").getByRole("button", {
            name: `Messages default to ${PLANNER_TITLE}`,
          }),
        ],
        [
          "the release",
          detailsRow(page, "Claimed by").getByRole("button", {
            name: `Release the claim on ${seeded.key}`,
          }),
        ],
        ["the priority row", detailsRow(page, "Advances").getByRole("link").first()],
        ["who opened it", detailsRow(page, "Opened by")],
        ["the approval", detailsRow(page, "Approval")],
        // The files row, in Details' sections: the history beneath them links a document it
        // added by name too.
        [
          "the files",
          marginSlot(page)
            .locator("[data-margin-section]")
            .getByRole("link", { name: /^notes\.md/ }),
        ],
        ["what blocks it", detailsRow(page, "Blocked by"), "None"],
        ["what it blocks", detailsRow(page, "Blocks"), waiter.key],
      ];
      for (const [name, at, words] of inDetails) {
        await expect(at, `${name} is not drawn in Details at ${width} px`).toBeVisible();
        if (words !== undefined) {
          await expect(at, `${name} does not read "${words}"`).toContainText(words);
        }
        await expectInsideTheViewportSideways(page, at, `${name} at ${width} px`);
      }
      await closeDetails(page);
    }

    // An issue whose labels fit draws them all and no count: with the request still waiting,
    // the 1440 px line leaves the labels room for one.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await patchIssue(seeded.key, { labels: ["dispatch"] });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(headerLabels(page)).toHaveText(["dispatch"]);
    await expect(labelsLeft(page), "a label that fits ends on a count").toHaveCount(0);

    // The issue no approval request waits on draws its control with no Approve, and its route
    // that reaches nobody is the header's one marker.
    await gotoIssue(page, `/issues/${seeded.parentKey}`);
    await expectIssuePageDrawn(page);
    await expect(identityBlock(page).getByTestId("issue-route-unreachable")).toHaveCount(1);
    await expect(identityBlock(page).getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(identityBlock(page)).toContainText(CONTROL_LABELS.not_requested);
    // An empty field reads None in its own row, and is reachable from the one add control.
    await openDetails(page);
    await expect(detailsRow(page, "Parent")).toContainText("None");
    expect(await addControl(page).count(), "Details offers no one add control").toBe(1);
    await addControl(page).click();
    expect(
      await openMenu(page).getByRole("menuitem").allTextContents(),
      "the add control lists no empty field"
    ).toContain("Parent");
    await page.keyboard.press("Escape");
    await closeDetails(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expect(identityBlock(page).getByTestId("issue-route-unreachable")).toHaveCount(0);

    // INFRA-2947's priority row is Details': an issue that advances none draws no such row, and
    // its control adds one, whose row links to the priority's page and is removed again; a
    // child that inherits a priority says so.
    const plain = await createIssue({ force: true, project: PROJECT, title: "Advances nothing" });
    await gotoIssue(page, `/issues/${plain.key}`);
    await openDetails(page);
    await expect(detailsRow(page, "Advances")).toHaveCount(0);
    // What nothing waits on reads None in its Blocks row, drawn all the same.
    await expect(detailsRow(page, "Blocks")).toContainText("None");
    // The add control's entry opens the control it names: on this load, with no repository set,
    // Repository puts the focus in the repository's own select, on screen in Details.
    await addControl(page).click();
    await openMenu(page).getByRole("menuitem", { exact: true, name: "Repository" }).click();
    const repository = marginSlot(page).getByRole("combobox", {
      exact: true,
      name: `Repository of ${plain.key}`,
    });
    await expect(repository, "the add control's Repository focused nothing").toBeFocused();
    await expect(repository, "the repository's select is off the screen").toBeInViewport();
    await expect(openMenu(page), "the add menu stayed open over what it opened").toHaveCount(0);
    await marginSlot(page)
      .getByRole("button", { name: `${CONTROL_LABELS.add_priority} to ${plain.key}` })
      .click();
    await marginSlot(page)
      .getByRole("combobox", { name: CONTROL_LABELS.priority_to_advance })
      .selectOption(seeded.priorities.own);
    await marginSlot(page)
      .getByRole("button", { exact: true, name: CONTROL_LABELS.add })
      .last()
      .click();
    const added = detailsRow(page, "Advances").getByRole("link");
    await expect(added, "the priority added drew no row").toHaveCount(1);
    await added.click();
    await expect(page).toHaveURL(`/priorities/${seeded.priorities.own}`);
    await gotoIssue(page, `/issues/${plain.key}`);
    await openDetails(page);
    await detailsRow(page, "Advances")
      .getByRole("button", { name: /^Remove / })
      .click();
    await expect(detailsRow(page, "Advances"), "the priority removed left its row").toHaveCount(0);
    await gotoIssue(page, `/issues/${seeded.children.keys[0]}`);
    await openDetails(page);
    await expect(detailsRow(page, "Advances")).toContainText(CONTROL_LABELS.advances_inherited);

    // A section opens expanded on a first load, and a collapsed one keeps that state.
    expect(
      await marginSectionDisclosure(page, "development").getAttribute("aria-expanded"),
      "the Development section is collapsed on a first load"
    ).toBe("true");
    await marginSectionDisclosure(page, "development").click();
    await expect(detailsRow(page, "Repository")).toHaveCount(0);
    await gotoIssue(page, `/issues/${seeded.parentKey}`);
    await expectIssuePageDrawn(page);
    await openDetails(page);
    expect(
      await marginSectionDisclosure(page, "development").getAttribute("aria-expanded"),
      "the next issue opens the section this reader collapsed"
    ).toBe("false");
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
      await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.cited}`);
      await expect(openedArtifact(page), "the document did not open").toBeVisible();
      await expect(waitingCount(page)).toHaveText(/^Waiting on you \((\d+)\)$/);
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
        await expect
          .poll(async () => (await focusedNodeId(page)) ?? "", {
            message: `press ${press + 2} of the count left the focus on ${from} at ${width} px`,
          })
          .not.toBe(from);
        const next = (await focusedNodeId(page)) ?? "";
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

// The tester's F4, under R13: the count holds what waits on the reader and nothing that waits on
// somebody else. A question waits on the person the server resolves it to - whoever it was handed
// to, else the issue's assignee - so a question on an issue another person holds does not raise
// this reader's count, the reader's own question there does not either, and handing the issue to
// the reader moves both to them.
test("the count of what waits holds only what waits on the reader", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedNoChildren, async (alice) => {
    const issue = await createIssue({ force: true, project: PROJECT, title: "Whose turn" });
    const bobs = await asUser(browser, "bob");
    try {
      const bob = await bobs.newPage();
      await bob.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
      await patchIssue(issue.key, { assignee: "alice" });
      await createAsk(issue.key, { question: "Which way does it go?" }, asPlanner);

      // The agent's question waits on alice, who holds the issue: her count, not bob's.
      await gotoIssue(alice, `/issues/${issue.key}`);
      await expectIssuePageDrawn(alice);
      await expect(waitingCount(alice)).toHaveText("Waiting on you (1)");
      await gotoIssue(bob, `/issues/${issue.key}`);
      await expectIssuePageDrawn(bob);
      await expect(rowsOfType(bob, "ask"), "bob's page draws no question yet").not.toHaveCount(0);
      await expect(waitingCount(bob), "a question waiting on alice counts for bob").toHaveCount(0);

      // Bob's own question waits on alice too: it raises her count and not his.
      await createAsk(issue.key, { question: "And who decides it?" }, { as: "user", login: "bob" });
      await gotoIssue(bob, `/issues/${issue.key}`);
      await expect(rowsOfType(bob, "ask")).toHaveCount(2);
      await expect(waitingCount(bob), "bob's own question raised bob's count").toHaveCount(0);
      await gotoIssue(alice, `/issues/${issue.key}`);
      await expect(waitingCount(alice)).toHaveText("Waiting on you (2)");

      // The issue handed to bob: both questions wait on him now, and none on alice.
      await patchIssue(issue.key, { assignee: "bob" });
      await gotoIssue(bob, `/issues/${issue.key}`);
      await expect(waitingCount(bob)).toHaveText("Waiting on you (2)");
      await gotoIssue(alice, `/issues/${issue.key}`);
      await expect(rowsOfType(alice, "ask")).toHaveCount(2);
      await expect(
        waitingCount(alice),
        "the count stayed with alice after the issue went to bob"
      ).toHaveCount(0);
    } finally {
      await bobs.close();
    }
  });
});

// The tester's F5, under R13: the title saves when its editor loses the focus, and the press
// that took the focus lands on what it pressed. A press on the status straight out of an edited
// title puts the focus on the status, as the same press does with nothing edited, and a status
// picked then is written.
test("a press on the status straight out of the title's editor lands on the status", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const title = identityBlock(page).getByRole("heading", { level: 1 });
    const editor = identityBlock(page).getByRole("textbox", { name: "Issue title" });
    const status = identityBlock(page).getByRole("combobox", { name: "Status" });

    // The control: with nothing edited, the press out of the editor focuses the status.
    await title.click();
    await expect(editor).toBeFocused();
    await status.click();
    await expect(status, "the status took no focus with nothing edited").toBeFocused();
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");

    // The same press with the title edited, and no Enter: the title saves and the press lands.
    await title.click();
    await expect(editor).toBeFocused();
    await editor.pressSequentially("edited ");
    await status.click();
    await expect(status, "the press out of an edited title landed on nothing").toBeFocused();
    await expect
      .poll(async () => (await getIssue(seeded.key)).title, {
        message: "the title the reader left was not saved",
      })
      .toContain("edited");
    await status.selectOption("todo");
    await expect
      .poll(async () => (await getIssue(seeded.key)).status, {
        message: "the status picked straight after the title was not written",
      })
      .toBe("todo");

    // Escape and Enter leave the title's editor with the focus on the heading it opened from,
    // which does not open the editor again (R11).
    for (const leave of ["Escape", "Enter"] as const) {
      await page.keyboard.press("Escape");
      await title.click();
      await expect(editor).toBeFocused();
      await page.keyboard.press(leave);
      await expect(editor, `${leave} left the title's editor open`).toHaveCount(0);
      await expect(title, `${leave} dropped the focus out of the header`).toBeFocused();
    }
  });
});

// c-header, the part of it ruling 5 of dispatch://INFRA-1196/artifact/build-rulings-md adds
// ("What stays when the page scrolls", and the defect it names in `identity-block.ts`). With the
// page at its top the header is not stuck and the title is 600 22/29 px, 20/26 px at 760 px and
// below. Scrolled, at 761 px and above the identity and the title stay, 63 px tall, in the same
// type, and the properties line goes under them; at 760 px and below the whole header scrolls
// away and the filter control stays at the top, and the page publishes the height of whichever
// stays.
test("the identity and the title stay when the page scrolls, and on a phone the filter control @phone", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    for (const width of desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width]) {
      const wide = width > 760;
      // A short screen, so the seeded page is taller than it and scrolls.
      await page.setViewportSize({ height: 420, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(tree(page).locator("[data-node-row]").first()).toBeVisible();
      await page.evaluate(() => window.scrollTo(0, 0));

      // The defect ruling 5 names, read off the header and its heading themselves: at the top
      // of the page the header is not stuck, and the title is drawn in its own type.
      await expect(
        identityBlock(page),
        `the header is stuck with the page at its top at ${width} px`
      ).toHaveAttribute("data-condensed", "false");
      expect(
        await identityBlock(page)
          .getByRole("heading", { level: 1 })
          .evaluate((heading) => {
            const type = getComputedStyle(heading);
            return [type.fontWeight, type.fontSize, type.lineHeight];
          }),
        `the title's type at ${width} px with the page at its top`
      ).toEqual(wide ? ["600", "22px", "29px"] : ["600", "20px", "26px"]);
      const top = await stickyReading(page);
      if (top === null) throw new Error(`the page draws no header or no filter row at ${width} px`);
      console.log(`c-header at ${width} px, at the top: ${JSON.stringify(top)}`);
      expect(
        top.properties.top,
        "the properties line is not under the title"
      ).toBeGreaterThanOrEqual(top.block.bottom - 1);

      await page.evaluate(() => window.scrollTo(0, 320));
      await expect
        .poll(async () => (await stickyReading(page))?.scrolled ?? 0, {
          message: `the page did not scroll at ${width} px`,
        })
        .toBeGreaterThan(300);
      await expect
        .poll(async () => (await stickyReading(page))?.condensed, {
          message: `the header's stuck state is wrong once scrolled at ${width} px`,
        })
        .toBe(wide);
      const scrolled = await stickyReading(page);
      if (scrolled === null) throw new Error("the header left the page as it scrolled");
      console.log(`c-header at ${width} px, scrolled: ${JSON.stringify(scrolled)}`);
      if (wide) {
        // The identity and the title stay, 63 px, in the type they had; the properties are gone.
        expect(Math.abs(scrolled.block.top), "the identity block left the top").toBeLessThanOrEqual(
          1
        );
        expect(scrolled.block.height, "the block that stays is not 63 px").toBeGreaterThanOrEqual(
          62
        );
        expect(scrolled.block.height, "the block that stays is not 63 px").toBeLessThanOrEqual(64);
        expect([scrolled.title.size, scrolled.title.line]).toEqual(["22px", "29px"]);
        expect(scrolled.title.top).toBeGreaterThanOrEqual(0);
        expect(scrolled.title.bottom).toBeLessThanOrEqual(scrolled.block.bottom);
        expect(
          scrolled.properties.bottom,
          "the properties line stayed on screen with the identity block"
        ).toBeLessThanOrEqual(scrolled.block.bottom);
        expect(scrolled.published, "the page publishes another height than the block's").toBe(
          Math.round(scrolled.block.height)
        );
      } else {
        // The header is gone and the filter control is at the top, one line of it.
        expect(scrolled.block.bottom, "the header stayed on a phone").toBeLessThanOrEqual(0);
        expect(scrolled.properties.bottom).toBeLessThanOrEqual(0);
        expect(
          Math.abs(scrolled.filters.top),
          "the filter control left the top"
        ).toBeLessThanOrEqual(1);
        expect(
          scrolled.filters.height,
          "the filter control's band is taller than the picture's"
        ).toBeLessThanOrEqual(52);
        await expect(presetChooser(page)).toBeInViewport();
        expect(scrolled.published, "the page publishes another height than the band's").toBe(
          Math.round(scrolled.filters.height)
        );
      }
    }
  });
});

// Ruling 5 of dispatch://INFRA-1196/artifact/build-rulings-md, "The filter row": one line at
// every width. From 761 px up the node menu, the filter by words, Keys and - with a filter on -
// the version control are small controls at its right end, and the words box opens from its
// control; at 760 px and below the row is the one control and Outline, and those three are in
// the Outline sheet.
test("the filter row is one line at every width @phone", async ({ browser }) => {
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
        await assertPageFits(page);
        const version = preset === "spec" ? [CONTROL_LABELS.version] : [];
        if (width > 760) {
          expect(row.controls.slice(-(3 + version.length)), `the small controls ${at}`).toEqual([
            ...version,
            ...small,
          ]);
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
        await expect(sheet.getByRole("searchbox", { name: CONTROL_LABELS.filter })).toBeVisible();
        await expect(sheet.getByTestId("issue-version-control")).toHaveCount(version.length);
        await page.keyboard.press("Escape");
        await expect(sheet).toHaveCount(0);
      }
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

// c-approval. The parts the model holds today: the converted issue's query nodes, which an
// entry reads after the conversion, wait on dispatch://INFRA-1253.
test("the approval entries stay in the identity block and read the node each step approves", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await resetDatabase();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await createProject({ key: "GATED", name: "Gated" });
    await setGatedSteps("GATED", ["decisions", "criteria", "spec"]);
    await createProject({ key: "PLAIN", name: "Plain" });

    // A root of a project that gates steps: one entry per step, in the project's order, each
    // reading its own document - the decisions approved where they stand, the criteria with no
    // document written, and the spec awaiting the approval its open request asks for.
    const root = await createIssue({ project: "GATED", spec: "The plan.", title: "Gated root" });
    const decisions = await createIssueArtifact(root.key, {
      content: "# Decisions\n\nHTTP it is.\n",
      name: "decisions.md",
    });
    await reviewArtifact(decisions.artifact.id, { state: "approved" });
    await requestApproval(root.primary_artifact_id ?? "", { summary: "Read it." });
    await gotoIssue(page, `/issues/${root.key}`);
    const steps = identityBlock(page).getByTestId("issue-approval-steps");
    await expect
      .poll(async () =>
        steps
          .locator("[data-approval-step]")
          .evaluateAll((entries) =>
            entries.map((entry) => entry.getAttribute("data-approval-step"))
          )
      )
      .toEqual(["decisions", "criteria", "spec"]);
    const entry = (step: string) => steps.locator(`[data-approval-step="${step}"]`);
    await expect(entry("decisions").getByRole("button", { name: "Approved v1" })).toBeVisible();
    await expect(entry("criteria")).toContainText(CONTROL_LABELS.not_requested);
    await expect(entry("spec").getByRole("button", { name: "Awaiting approval" })).toBeVisible();
    // The request waits on the reader, so the spec's entry carries Approve in the header (R31),
    // and the approval it records reads back.
    await entry("spec").getByRole("button", { exact: true, name: "Approve" }).click();
    await expect
      .poll(async () => (await getArtifact(root.primary_artifact_id ?? "")).approval?.state)
      .toBe("approved");
    await expect(entry("spec").getByRole("button", { exact: true, name: "Approve" })).toHaveCount(
      0
    );

    // A root of a project that gates nothing carries the one chip, "Not requested" while its
    // approval is absent.
    const plain = await createIssue({ project: "PLAIN", spec: "The plan.", title: "Plain root" });
    await gotoIssue(page, `/issues/${plain.key}`);
    await expect(identityBlock(page).getByTestId("issue-approval-steps")).toHaveCount(0);
    await expect(identityBlock(page)).toContainText(CONTROL_LABELS.not_requested);

    // Every child carries the one chip, and no list of steps: the child of a parent not yet
    // approved, the child of a parent whose spec changed since its approval, and the child of an
    // approved parent; none has an approval of its own, so each chip reads "Not requested".
    const oneChip = async (key: string) => {
      await gotoIssue(page, `/issues/${key}`);
      await expectIssuePageDrawn(page);
      await expect(identityBlock(page).getByTestId("issue-approval-steps")).toHaveCount(0);
      await expect(
        identityBlock(page).getByText(CONTROL_LABELS.not_requested, { exact: true })
      ).toHaveCount(1);
    };
    const childOf = (parent: string, title: string) =>
      createIssue({ parent, project: "PLAIN", spec: "The child's plan.", title });
    const unapproved = await createIssue({
      project: "PLAIN",
      spec: "Not yet.",
      title: "Unapproved",
    });
    await oneChip((await childOf(unapproved.key, "Child of an unapproved parent")).key);
    const changed = await createIssue({
      project: "PLAIN",
      spec: "Before.",
      title: "Changed since",
    });
    await reviewArtifact(changed.primary_artifact_id ?? "", { state: "approved" });
    await editArtifact(changed.primary_artifact_id ?? "", {
      ops: [{ find: "Before.", op: "replace", with: "After." }],
    });
    expect((await getArtifact(changed.primary_artifact_id ?? "")).approval?.state).toBe("stale");
    await oneChip((await childOf(changed.key, "Child of a changed parent")).key);
    await requestApproval(plain.primary_artifact_id ?? "", { summary: "Read it." });
    await reviewArtifact(plain.primary_artifact_id ?? "", { state: "approved" });
    const child = await childOf(plain.key, "Child of an approved parent");
    await oneChip(child.key);

    // A child with an approval request open answers it from its chip in the header, and the
    // approval it records reads back.
    await requestApproval(child.primary_artifact_id ?? "", { summary: "Read it." });
    await gotoIssue(page, `/issues/${child.key}`);
    await identityBlock(page).getByRole("button", { exact: true, name: "Approve" }).click();
    await expect
      .poll(async () => (await getArtifact(child.primary_artifact_id ?? "")).approval?.state)
      .toBe("approved");
  } finally {
    await context.close();
  }
});

/** The box the header's rows stand in. The header is the prototype's `#hdr-sticky`, a band as
 *  wide as the column whose 28 px insets are its own padding, so this is what the page's
 *  column measure reads. */
function headerRowsBox(page: Page): Promise<{ width: number; x: number; y: number } | null> {
  return identityBlock(page).evaluate((block) => {
    const rows = block.firstElementChild;
    if (rows === null) return null;
    const box = rows.getBoundingClientRect();
    const style = getComputedStyle(rows);
    const left = Number.parseFloat(style.paddingLeft);
    return {
      width: box.width - left - Number.parseFloat(style.paddingRight),
      x: box.x + left,
      y: box.y,
    };
  });
}

// c-one-panel, with R14's rail. At 1440 px the shell's navigation is an icon rail of 56 px and
// Details is a drawer over the right of the page, closed on every load, so the header and the
// outline keep the window less the rail and its 28 px insets: 1328 px. The panel holds the
// properties and the focused node's facts and the Contents pane holds none; its content is
// drawn once per load; its handle, its collapse and its kept width work; at 390 px it is the
// phone's sheet. The panel holds no conversation list: no comment, message or ask the outline
// draws is drawn in it.
test("one panel of properties holds no conversation list @phone", async ({ browser }, testInfo) => {
  // Four loads of the issue page and one of a project document, each read for its frame.
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const traffic = watchApiRequests(page);
    const panelDraws = await watchPanelDraws(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect.poll(() => nodeRows(page).count()).toBeGreaterThan(0);
    await expectLoadSettled(traffic);
    // The panel's content is drawn once per load: one element for a section of it, through the
    // load, Details opening and Details closing, where a panel drawn plain and again inside the
    // page's slot is two (INFRA-2004). Its reads cannot say so: a second copy reads the first
    // one's answers from the cache and sends nothing.
    await openDetails(page);
    await closeDetails(page);
    await openDetails(page);
    expect(await panelDraws(), "the load drew the panel's content other than once").toBe(1);
    await closeDetails(page);

    // The rail: 56 px at the left edge, the whole height, an icon with its accessible name for
    // each of the sidebar's own entries, none carrying a tooltip, and the sidebar drawn nowhere.
    await expect(iconRail(page), "the issue page draws no icon rail").toHaveCount(1);
    const rail = await iconRail(page).boundingBox();
    expect(rail, "the issue page draws no icon rail").not.toBeNull();
    expect(rail?.x, "the rail is not at the left edge").toBe(0);
    expect(rail?.width, "the rail is not 56 px wide").toBe(56);
    expect(rail?.height, "the rail is not the window's height").toBe(DESKTOP.height);
    for (const name of ["Inbox", "Agents", "Priorities", "Delivery", "Pipeline", "Settings"]) {
      await expect(
        iconRail(page).getByRole("link", { name: new RegExp(`^${name}\\b`) }),
        `the rail offers no ${name}`
      ).toHaveCount(1);
    }
    expect(await iconRail(page).locator("[title]").count(), "a rail entry carries a tooltip").toBe(
      0
    );
    await expect(
      page.getByRole("complementary", { exact: true, name: "Navigation" }),
      "the issue page draws the sidebar beside its rail"
    ).toHaveCount(0);
    // Its foot opens the whole navigation over the page, which holds every entry the sidebar
    // does, the ones the rail draws no icon for among them.
    await iconRail(page).getByRole("button", { exact: true, name: "Open navigation" }).click();
    const sheet = page.getByRole("dialog", { exact: true, name: "Navigation" });
    await expect(sheet, "the rail's foot opens no navigation").toBeVisible();
    for (const name of [/^Search/, /^New issue/]) {
      await expect(
        sheet.getByRole("button", { name }),
        `the navigation holds no ${name}`
      ).toBeVisible();
    }
    await expect(sheet.getByRole("button", { exact: true, name: "Sign out" })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(sheet).toBeHidden();

    // The header and the outline have the window less the rail: 1328 px between the insets.
    const header = await headerRowsBox(page);
    expect(Math.round(header?.width ?? 0), "the header is not 1328 px wide at 1440 px").toBe(1328);
    expect(Math.round(header?.x ?? 0), "the header does not start 28 px right of the rail").toBe(
      84
    );

    // The frame as a reader first sees it, beside page-v3-conversation-top-1440.
    await page.screenshot({ path: testInfo.outputPath("issue-1440-details-closed.png") });

    // The Contents pane's list (R14): each question and each of the reader's own threads, a
    // press or Enter going to its node in the outline, the entry the reader is in marked, and
    // Focus branch drawing that branch alone, then the whole outline again.
    const question = blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID);
    const entry = contentsPane(page).locator(`[data-contents-entry="${question}"]`);
    await expect(entry, "the Contents pane does not list the open question").toHaveCount(1);
    await nodeRows(page).first().focus();
    await entry.focus();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedNodeId(page), { message: "Enter on an entry went to no node" })
      .toBe(question);
    await expect(entry, "the entry the reader is in is not marked").toHaveAttribute(
      "aria-selected",
      "true"
    );
    await nodeRows(page).first().focus();
    await entry.click();
    await expect
      .poll(() => focusedNodeId(page), { message: "a press on an entry went to no node" })
      .toBe(question);
    const focusBranch = focusBranchControl(contentsPane(page));
    await focusBranch.click();
    await expect(focusBranch).toHaveAttribute("aria-pressed", "true");
    await expect
      .poll(async () => (await drawnRows(page)).find((row) => row.visible)?.id, {
        message: "Focus branch did not draw the entry's branch alone",
      })
      .toBe(question);
    await focusBranch.click();
    await expect
      .poll(async () => (await drawnRows(page)).find((row) => row.visible)?.id, {
        message: "Focus branch turned off did not draw the whole outline again",
      })
      .toBe(seeded.key);

    // Details: closed on the load, opened by the control at the right of the header's first
    // row, over the right of the page, which keeps its own width under it.
    await expect(marginShell(page), "Details is open on a fresh load").toBeHidden();
    const control = await detailsControl(page).boundingBox();
    expect(
      (control?.x ?? 0) + (control?.width ?? 0),
      "the Details control is not at the right of the header's first row"
    ).toBeGreaterThan((header?.x ?? 0) + (header?.width ?? 0) - 120);
    expect(control?.y ?? 0, "the Details control is not in the header's first row").toBeLessThan(
      (header?.y ?? 0) + 60
    );
    await openDetails(page);
    // Its cards and its sections each keep a place of their own from the frame it opens: the
    // margin's anchored cards are measured again once the drawer is shown, so none lies over the
    // sections under them. Read once, two frames after it opens: a card placed by a height read
    // while the drawer was hidden lay over them until some later render happened to correct it.
    expect(
      await overlapsOnceOpened(page),
      "Details draws one text over another at 1440 px"
    ).toEqual([]);
    // Details open, beside page-v3-details-1440.
    await page.screenshot({ path: testInfo.outputPath("issue-1440-details-open.png") });
    const drawer = await marginShell(page).boundingBox();
    expect(
      Math.round((drawer?.x ?? 0) + (drawer?.width ?? 0)),
      "Details is not against the right edge"
    ).toBe(DESKTOP.width);
    expect(Math.round(drawer?.width ?? 0), "Details does not open at its width").toBe(384);
    expect(
      Math.round((await headerRowsBox(page))?.width ?? 0),
      "opening Details took a column from the page rather than opening over it"
    ).toBe(1328);

    // One panel: the slot, its three sections and the focused node's facts, each drawn once and
    // each inside Details; the Contents pane at the left holds none of them.
    for (const [name, holder] of [
      ["the panel", '[data-testid="issue-margin"]'],
      ["the development section", '[data-margin-section="development"]'],
      ["the people section", '[data-margin-section="people"]'],
      ["the references section", '[data-margin-section="references"]'],
      ["the focused node's facts", '[data-testid="focused-node-facts"]'],
    ] as const) {
      expect(await page.locator(holder).count(), `the page draws ${name} other than once`).toBe(1);
      expect(await marginShell(page).locator(holder).count(), `${name} is not in Details`).toBe(1);
    }
    await expect(contentsPane(page), "the page draws no Contents pane").toHaveCount(1);
    expect(
      await contentsPane(page)
        .locator(
          '[data-margin-section], [data-testid="focused-node-facts"], [data-testid="issue-margin"], [data-issue-header]'
        )
        .count(),
      "the Contents pane holds a property"
    ).toBe(0);
    // It holds the reader's pins and the history, and no list of the conversation: no comment,
    // message or ask of the seed that the outline draws is drawn in Details (R14).
    await expect(marginShell(page).getByTestId("issue-pins")).toHaveCount(1);
    await expect(marginShell(page).getByTestId("issue-history")).toHaveCount(1);
    await expect(page.locator("#issue-conversation-panel, [data-conversation-state]")).toHaveCount(
      0
    );
    for (const words of [
      "Thread 1 opens here.",
      "Reply 1 answers the second paragraph of its thread.",
      "Which is why the deepest question is still on the first screen.",
    ]) {
      await expect(
        marginShell(page).getByText(words, { exact: false }),
        `Details draws "${words}", which the outline draws`
      ).toHaveCount(0);
    }

    // The facts follow the focus.
    await nodeRows(page).first().focus();
    const first = await focusedNodeFacts(page).innerText();
    const firstNode = await focusedNodeId(page);
    await page.keyboard.press("j");
    expect(await focusedNodeId(page), "j moved the focus to no other row").not.toBe(firstNode);
    expect(
      await focusedNodeFacts(page).innerText(),
      "the panel's facts did not follow the focus to the next node"
    ).not.toBe(first);

    // What references the focused row (R14; R19's table: "A block's 'Referenced by' list" ->
    // "The focused node's facts in the panel"). The outline draws what serves a node as its
    // parent row and what needs it as a chip, and nothing pointing back from a node to what
    // mentions it, so this list is where a reader reads that. It costs nothing per row (R21):
    // the list is read when the reader opens it, once, and the focus moving to the next row
    // closes it again and reads nothing.
    const referenced = nodeRow(
      page,
      blockNodeId(await primarySlug(seeded.key), requirementBlockId(1))
    ).first();
    await expect(referenced, "the outline draws no row for the first statement").toHaveCount(1);
    await referenced.focus();
    const referencesControl = focusedNodeFacts(page).getByRole("button", {
      exact: true,
      name: NODE_REFERENCES_WORDS.what_references,
    });
    await expect(
      referencesControl,
      "the focused row's facts carry no control for what references it"
    ).toHaveAttribute("aria-expanded", "false");
    traffic.reset();
    await referencesControl.click();
    // The plan task that serves it, which the list names by the task's own words, and the
    // decisions document's block that mentions it, which the outline draws no link to from
    // this row; every row of the list lands inside the issue.
    await expect(
      focusedNodeFacts(page).getByRole("link", { exact: true, name: planTaskText(1) }),
      "the list does not name the plan task that serves the statement"
    ).toHaveCount(1);
    const citing = await focusedNodeFacts(page)
      .getByRole("listitem")
      .getByRole("link")
      .evaluateAll((links) => links.map((link) => link.getAttribute("href") ?? ""));
    expect(
      citing.some((href) => href.includes("/artifacts/decisions-md#b-")),
      `the list holds no mention of the statement: ${JSON.stringify(citing)}`
    ).toBe(true);
    expect(
      citing.every((href) => href.startsWith(`/issues/${seeded.key}`)),
      `a row of the list lands outside the issue: ${JSON.stringify(citing)}`
    ).toBe(true);
    expect(
      traffic.requests.filter((request) => request.path.startsWith("/api/v1/references")).length,
      "opening the list read other than once"
    ).toBe(1);
    traffic.reset();
    await referenced.focus();
    await page.keyboard.press("j");
    await expect(
      focusedNodeFacts(page).getByRole("button", {
        exact: true,
        name: NODE_REFERENCES_WORDS.what_references,
      }),
      "the list stayed open when the focus moved to another row"
    ).toHaveAttribute("aria-expanded", "false");
    expect(
      traffic.requests.filter((request) => request.path.startsWith("/api/v1/references")).length,
      "moving the focus read what references the next row"
    ).toBe(0);

    // Its collapse: Escape and Close close it, and its one key opens it.
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape did not close Details").toBeHidden();
    await focusThePage(page);
    await page.keyboard.press("Shift+M");
    await expect(marginShell(page), "Shift+M did not open Details").toBeVisible();
    await closeDetails(page);
    // It opens on the node the reader stood on, from its control as from its key: the press
    // moves the focus off the outline, and the facts stay that node's.
    await nodeRows(page).first().focus();
    await openDetails(page);
    expect(
      await focusedNodeFacts(page).innerText(),
      "Details opened from its control holds none of the facts of the row the reader stood on"
    ).toBe(first);
    await closeDetails(page);

    // Its handle and the width the reader gave it, kept across a load.
    await openDetails(page);
    const handle = await marginResizeHandle(page).boundingBox();
    expect(handle, "Details has no resize handle").not.toBeNull();
    const before = (await marginShell(page).boundingBox())?.width ?? 0;
    await page.mouse.move((handle?.x ?? 0) + 2, (handle?.y ?? 0) + 2);
    await page.mouse.down();
    await page.mouse.move((handle?.x ?? 0) - 120, (handle?.y ?? 0) + 2);
    await page.mouse.up();
    const resized = (await marginShell(page).boundingBox())?.width ?? 0;
    expect(resized, "dragging the handle did not widen Details").toBeGreaterThan(before + 50);
    await page.reload();
    await expectIssuePageDrawn(page);
    await expect(marginShell(page), "a load opened Details").toBeHidden();
    await openDetails(page);
    expect(
      (await marginShell(page).boundingBox())?.width ?? 0,
      "Details did not keep the width the reader gave it"
    ).toBeCloseTo(resized, 0);

    // At 390 px: no rail, the header's own control for the navigation, and the panel's content
    // in the phone's sheet, the whole width, which Escape closes.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(iconRail(page), "the phone draws the rail").toHaveCount(0);
    await expect.poll(() => nodeRows(page).count()).toBeGreaterThan(0);
    // The phone's frame, beside page-v3-conversation-top-390.
    await page.screenshot({ path: testInfo.outputPath("issue-390.png") });
    await expect(
      identityBlock(page).getByRole("button", { exact: true, name: "Open navigation" }),
      "the phone's header carries no control for the navigation"
    ).toBeVisible();
    await openDetails(page);
    expect(
      await overlapsOnceOpened(page),
      "Details draws one text over another on the phone"
    ).toEqual([]);
    const phoneSheet = page.getByRole("dialog", { exact: true, name: CONTROL_LABELS.details });
    await expect(phoneSheet, "Details is no sheet on the phone").toBeVisible();
    expect(Math.round((await phoneSheet.boundingBox())?.width ?? 0)).toBe(PHONE.width);
    for (const section of ["development", "people", "references"] as const) {
      await expect(phoneSheet.locator(`[data-margin-section="${section}"]`)).toHaveCount(1);
    }
    await page.keyboard.press("Escape");
    await expect(phoneSheet, "Escape did not close the phone's sheet").toBeHidden();

    // Every other page keeps the sidebar and the margin: a project document page.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const document = await createProjectDocument("LAY", {
      content: "# A project document\n\nIts margin is unchanged.\n",
      name: "project-notes.md",
    });
    await page.goto(`/projects/LAY/documents/${document.artifact.slug}`);
    await expect(page.getByRole("heading", { level: 1, name: "A project document" })).toBeVisible();
    await expect(marginShell(page), "the project document page lost its margin").toBeVisible();
    await expect(
      page.getByRole("complementary", { exact: true, name: "Navigation" }),
      "the project document page lost its sidebar"
    ).toBeVisible();
    await expect(iconRail(page), "the project document page draws the rail").toHaveCount(0);
  });
});

/** Every event of `key`'s log, newest first, read 200 at a time as the history pages it. */
async function allEvents(key: string): Promise<DispatchEvent[]> {
  const all: DispatchEvent[] = [];
  let before: number | undefined;
  for (;;) {
    const page = await getIssueEvents(
      key,
      before === undefined ? { limit: 200, order: "desc" } : { before, limit: 200 }
    );
    all.push(...page);
    if (page.length < 200) return all;
    before = page.at(-1)?.seq;
  }
}

// c-conversation-state. The conversation is the outline and nothing else (dispatch://INFRA-1196
// R1, R14): each seeded comment and message a person took part in is one node, nothing offers a
// choice between words, questions, pinned and everything, and Details draws none of them. The
// history in the panel (R15) draws what no node does - the status change, the claim, both
// handoffs, the daemon's message, an agent's status note and the two comments agents wrote to
// each other, each once - and no event of a node, no version of a document, no result against a
// check and no child's move. The negative control is the four-state list: Details draws a
// seeded comment a person took part in a second time, and the row fails on it.
test("the conversation is the outline and the history is in the panel", async ({ browser }) => {
  // The whole issue's seed and two loads of its page, the history opened day by day between.
  test.setTimeout(240_000);
  await phase(browser, seedConversationState, async (page, seeded) => {
    // A day before today, which the history folds to one line with its count.
    const created = (await allEvents(seeded.key)).find((event) => event.type === "issue.created");
    if (created === undefined) throw new Error(`${seeded.key} recorded no issue.created`);
    await setEventCreatedAt(created.id, new Date(Date.now() - 86_400_000).toISOString());

    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const turns = [
      ...seeded.comments.threads,
      ...seeded.comments.replies,
      seeded.messages.root,
      seeded.messages.reply,
      seeded.messages.replyToReply,
    ];
    for (const id of turns) {
      await expect(
        page.locator(`[data-node-row][data-node-id="${id}"]`),
        `${id} is not one node of the outline`
      ).toHaveCount(1);
      await expect(historyItem(page, id), `${id} is drawn in the history too`).toHaveCount(0);
    }
    await expect(page.locator("[data-conversation-state], [data-turn]")).toHaveCount(0);
    for (const name of ["Words", "Questions", "Pinned", "Everything"]) {
      await expect(page.getByRole("tab", { exact: true, name })).toHaveCount(0);
      await expect(page.getByRole("radio", { exact: true, name })).toHaveCount(0);
    }
    await openDetails(page);
    await expect(
      marginShell(page).getByText("Thread 1 opens here.", { exact: false }),
      "Details draws a comment a person took part in, which is the outline's"
    ).toHaveCount(0);

    // A version of the spec saved and a question asked and resolved: neither is a line of the
    // history, which leaves every event of a node and every document version out.
    await editArtifact(seeded.specArtifactId, {
      ops: [{ find: requirementText(1), op: "replace", with: `${requirementText(1)} Saved.` }],
    });
    const asked = await createAsk(
      seeded.key,
      { question: "Is a resolved question left out of the history?" },
      asPlanner
    );
    await resolveAsk(asked.id, { kind: "resolved", reason: "It is." }, asPlanner);

    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await expect(
      historyPanel(page),
      "/activity does not open the panel at its history"
    ).toBeVisible();
    // The history pages the log 200 events at a time: every page is read in before it is.
    const older = historyPanel(page).getByRole("button", { exact: true, name: "Load older" });
    while (await older.isVisible()) {
      await older.click();
      await expect(older).not.toHaveText("Loading…");
    }
    const folded = historyPanel(page).locator('[data-history-day] > button[aria-expanded="false"]');
    expect(await folded.count(), "no day before today is folded to one line").toBeGreaterThan(0);
    await expect(folded.first()).toHaveText(/ · \d+$/);
    while ((await folded.count()) > 0) await folded.first().click();

    const events = await allEvents(seeded.key);
    const seqs = (
      await historyEntries(page).evaluateAll((items) =>
        items.map((item) => Number(item.getAttribute("data-event-seq")))
      )
    ).filter((seq) => seq > 0);
    expect(seqs, "the history is not newest first").toEqual([...seqs].sort((a, b) => b - a));
    const drawn = new Set(seqs);
    const people = new Set(turns);
    const ofNodes = events.filter(
      (event) =>
        ((event.type === "ask.opened" ||
          event.type === "ask.answered" ||
          event.type === "ask.resolved") &&
          event.payload.kind === "question") ||
        event.type === "result.recorded" ||
        event.type === "child.status" ||
        (event.type === "artifact.version" &&
          event.payload.artifact_id === seeded.specArtifactId) ||
        (event.type === "comment.created" && people.has(event.payload.id))
    );
    for (const type of ["ask.resolved", "result.recorded", "artifact.version"]) {
      expect(
        ofNodes.some((event) => event.type === type),
        `the seed recorded no ${type}`
      ).toBe(true);
    }
    expect(
      ofNodes.filter((event) => drawn.has(event.seq)).map((event) => event.type),
      "the history draws an event of a node, a document's version, a result or a child's move"
    ).toEqual([]);

    // What it draws, each once.
    const once = async (seq: number | undefined, what: string) => {
      expect(seq, `the seed recorded no ${what}`).toBeDefined();
      await expect(historyPanel(page).locator(`[data-event-seq="${seq}"]`), what).toHaveCount(1);
    };
    const status = events.filter(
      (event) => event.type === "issue.updated" && event.payload.status === "in_progress"
    );
    await once(status.at(-1)?.seq, "the status change");
    await once(events.find((event) => event.type === "issue.claimed")?.seq, "the claim");
    const handoffs = events.filter((event) => event.type === "handoff.recorded");
    expect(handoffs, "the seed recorded both handoffs").toHaveLength(2);
    for (const handoff of handoffs) await once(handoff.seq, `handoff ${handoff.seq}`);
    for (const [id, words] of [
      [seeded.daemonMessage, "The planner's session was replaced"],
      [seeded.statusNote, "Status: the plan is drafted"],
      [seeded.agentComments.root, "the plan's third task needs a second look"],
      [seeded.agentComments.reply, "the third task stands as written"],
    ] as const) {
      await expect(historyItem(page, id), `${words} is not drawn once`).toHaveCount(1);
      await expect(historyItem(page, id)).toContainText(words);
    }
    for (const id of [seeded.agentComments.root, seeded.agentComments.reply]) {
      await expect(historyPanel(page).locator(`[data-history-reply="${id}"]`)).toHaveCount(1);
    }
    const uploads = historyPanel(page).locator('[data-history-uploads="3"]');
    await expect(uploads, "the three uploads are not one entry").toHaveCount(1);
    for (const name of seeded.uploads) {
      await expect(uploads.getByRole("link", { exact: true, name })).toHaveCount(1);
    }

    // A person's reply to one of the agents' comments moves that thread into the outline.
    await historyPanel(page).locator(`[data-history-reply="${seeded.agentComments.root}"]`).click();
    await sendFromComposer(page, "A person joins the agents' thread.");
    await expect(historyItem(page, seeded.agentComments.root)).toHaveCount(0);
    await expect(
      page.locator(`[data-node-row][data-node-id="${seeded.agentComments.root}"]`)
    ).toHaveCount(1);
  });
});

// c-reply-nesting. The seed writes both replies through the route a person writes through, so
// the `answers` this row reads is the server's own: the write climbs `reply_to` to the head of
// the thread before it stores, which is read here first, and a page parenting by `reply_to`
// draws the second reply under that head - this row's negative control. Every node that takes a
// reply draws its control, an answered and a resolved question included, and what each sends is
// drawn where it belongs (R16).
test("a reply is drawn under the entry it answers, and every node takes a reply @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    // What the server stored, before the page is read: `reply_to` flattened onto the head and
    // the entry each reply answered kept beside it.
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
    // Below the third level under the issue a laptop draws a branch as one line saying how many
    // nodes it holds, which opens them in place: the reply that answers `second` is the fifth
    // level of the tree.
    const deeper = branchMoreRows(page);
    await expect(deeper, "the fifth level is not folded to one line").not.toHaveCount(0);
    await expect(deeper.first()).toContainText("1");
    await expect(nodeRow(page, seeded.third)).toHaveCount(0);
    await openEveryDeeperBranch(page);
    await expect(nodeRow(page, seeded.third)).not.toHaveCount(0);
    expect(await parentOfRow(page, seeded.third)).toBe(seeded.second);

    expect(
      await parentOfRow(page, seeded.second),
      `the reply that answers another reply is not drawn under it; the thread's head is ${seeded.head}`
    ).toBe(seeded.first);
    expect(
      await parentOfRow(page, seeded.first),
      "the reply to the thread's head is not drawn under it"
    ).toBe(seeded.head);
    expect(
      await parentOfRow(page, seeded.head),
      "the thread's head is not drawn under the issue"
    ).toBe(seeded.key);

    // Every node that takes a reply draws the control, an answered and a resolved ask included;
    // the open ask draws the control that answers it.
    for (const id of [
      seeded.head,
      seeded.first,
      seeded.second,
      seeded.answeredAsk,
      seeded.resolvedAsk,
    ]) {
      const row = await focusRow(page, id);
      await expect(rowAct(row, "reply"), `${id} draws no reply control`).toBeVisible();
    }
    await expect(rowAct(await focusRow(page, seeded.openAsk), "answer")).toBeVisible();

    /** The id of the comment whose body is `body`, once Dispatch holds it. */
    const stored_ = async (body: string) => {
      await expect
        .poll(async () => (await listComments(seeded.key)).some((one) => one.body === body))
        .toBe(true);
      return (await listComments(seeded.key)).find((one) => one.body === body)?.id ?? "";
    };
    const drawnUnder = async (id: string, parent: string, what: string) => {
      await unfoldEveryBranch(page);
      await openEveryDeeperBranch(page);
      await expect.poll(() => parentOfRow(page, id), { message: what }).toBe(parent);
    };

    // A reply sent from a resolved ask's control is drawn under that ask (INFRA-3677).
    await replyFromRow(page, seeded.resolvedAsk, "A reply on the resolved question.");
    await drawnUnder(
      await stored_("A reply on the resolved question."),
      seeded.resolvedAsk,
      "the reply on the resolved ask is not drawn under it"
    );
    // A reply sent from a reply's control is drawn under that reply.
    await replyFromRow(page, seeded.first, "A reply to the first answer.");
    await drawnUnder(
      await stored_("A reply to the first answer."),
      seeded.first,
      "the reply to a reply is not drawn under it"
    );

    // An answer sent from the open ask's control is drawn in the ask's row.
    await focusRow(page, seeded.openAsk);
    await nodeRow(page, seeded.openAsk).first().locator('[data-pick-option="Left"]').click();
    await composer(page).getByRole("button", { name: CONTROL_LABELS.send_answer }).click();
    await expect(nodeRow(page, seeded.openAsk).first()).toHaveAttribute(
      "data-node-state",
      "answered"
    );
    await expect(
      nodeRow(page, seeded.openAsk).first().locator('[data-node-option="chosen"]')
    ).toContainText("Left");

    // The composer at the end of the outline posts a comment drawn under the issue.
    const instead = composer(page).getByRole("button", { name: CONTROL_LABELS.write_on_issue });
    if (await instead.isVisible()) await instead.click();
    await sendFromComposer(page, "A comment on the issue from the composer.");
    await drawnUnder(
      await stored_("A comment on the issue from the composer."),
      seeded.key,
      "the composer's comment is not drawn under the issue"
    );

    // On a phone the outline draws one level below the node it is read from (R16): opening the
    // thread's head draws what it holds as one line saying how many nodes that is, and pressing
    // the line draws them in its place, under the head.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await nodeRow(page, seeded.head)
      .first()
      .getByRole("button", { exact: true, name: CONTROL_LABELS.unfold })
      .click();
    const counted = branchMoreRows(page);
    await expect(
      counted,
      "the branch below the first level is not one line counting its nodes on a phone"
    ).toHaveCount(1);
    await expect(counted).toContainText(/\d+/);
    await expect(nodeRow(page, seeded.first)).toHaveCount(0);
    await counted.getByRole("button").click();
    await expect(
      nodeRow(page, seeded.first),
      "pressing the counted line draws none of the nodes it holds"
    ).not.toHaveCount(0);
    expect(await parentOfRow(page, seeded.first)).toBe(seeded.head);
  });
});

// dispatch://INFRA-3677: a question its asker resolved still takes a reply, and the reply is
// drawn under the question rather than under the issue or nowhere.
test("a reply on a resolved ask is drawn under the ask", async ({ browser }) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(nodeRow(page, seeded.resolvedAsk).first()).toHaveAttribute(
      "data-node-state",
      "resolved"
    );
    await replyFromRow(page, seeded.resolvedAsk, "Still worth a word after it was resolved.");
    await expect
      .poll(async () =>
        (await listComments(seeded.key)).find(
          (one) => one.body === "Still worth a word after it was resolved."
        )
      )
      .toMatchObject({ ask_id: seeded.resolvedAsk });
    const reply = (await listComments(seeded.key)).find(
      (one) => one.body === "Still worth a word after it was resolved."
    );
    // A resolved question is no path to anything open, so its branch arrives folded (R1).
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    await expect.poll(() => parentOfRow(page, reply?.id ?? "")).toBe(seeded.resolvedAsk);
    await expect(nodeRow(page, reply?.id ?? "").first()).toContainText(
      "Still worth a word after it was resolved."
    );
  });
});

// Reading a thread on the issue page marks its Inbox row read, as the panel's list of the
// conversation did on its own address: the row of the thread's newest reply, on screen for a
// second, marks the thread read through that reply (`outline-thread-read.tsx`). At the commit
// before the outline replaced the list, the issue's own address marked nothing read.
test("a thread read on the outline is read in the Inbox", async ({ browser }) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    // An agent replies in alice's thread, which makes the thread unread in her Inbox.
    await createComment(
      seeded.key,
      { body: "An agent's answer alice has not read.", reply_to: seeded.head },
      asPlanner
    );
    // The route's own default is the caller's unread threads alone.
    const unread = async () =>
      ((await listMyReplies({ issue: seeded.key }, { login: "alice" })).rows ?? []).some(
        (row) => row.thread_id === seeded.head
      );
    await expect.poll(unread, { message: "the agent's reply left the thread read" }).toBe(true);

    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    await expect(page.getByText("An agent's answer alice has not read.")).toBeVisible();
    await expect.poll(unread, { message: "reading the thread did not mark it read" }).toBe(false);
  });
});

// c-home. Partial on today's data: `from` waits on dispatch://INFRA-1098 item 21, so every
// statement sits at the issue root, and the Spec filter, which draws no issue row (R2), draws
// each as a row of its own top level with no parent line; this test asserts that, not a
// decision's parentage.
test("the spec is a preset over the tree and an approval still names it", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // The approval is requested before the page draws, so the target it pins is read back after.
    await requestApproval(seeded.specArtifactId, { summary: "The spec as the tree draws it." });

    await openPreset(page, seeded.key, "spec");
    expect(await activePreset(page), "the Spec address does not open the Spec filter").toBe(
      PRESET_NAMES.spec
    );
    await expectIssuePageDrawn(page);

    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const rows = await drawnRows(page);
    expect(rowOf(rows, seeded.key), "the Spec filter draws the issue as a row").toBeUndefined();
    for (const requirement of seeded.requirements) {
      const id = blockNodeId(specSlug, requirement);
      const row = rowOf(rows, id);
      expect(row, `the Spec filter does not draw ${requirement}`).toBeDefined();
      expect(
        [row?.level, row?.parent],
        `${requirement} does not sit at the top with no parent line while its own from pointer is absent`
      ).toEqual(["1", null]);
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
    // The Spec filter's version control reads the spec's versions; the approval is the identity
    // block's own (R31), so the control carries none.
    expect(
      await versionControl(page).count(),
      "the Spec filter draws no version control beside the filters"
    ).toBe(1);
    expect(
      await versionControl(page)
        .getByRole("button", { name: /^Approve/ })
        .count(),
      "the version control beside the filters carries an approval control"
    ).toBe(0);
  });

  // The same drawing on an issue that carries one document.
  await phase(browser, seedNoChildren, async (page, seeded) => {
    await openPreset(page, seeded.key, "spec");
    await expectIssuePageDrawn(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const rows = await drawnRows(page);
    for (const requirement of seeded.requirements) {
      const row = rowOf(rows, blockNodeId(specSlug, requirement));
      expect(
        [row?.level, row?.parent],
        `${requirement} is drawn differently on an issue that carries one document`
      ).toEqual(["1", null]);
    }
  });
});

// c-checks-readable. The body the Verification filter draws is
// `web/src/features/issue/verification-filter/`: each statement with the checks that serve it,
// each check beside its newest result, and its earlier results folded to one line (ruling 6 of
// build-rulings-md, the tester's F15). Two of the seven states the seed cannot make, so this
// row makes them: `retracted` by withdrawing every result of one check, and `unreadable` by
// taking `pass_stands` out of the results answer in a route handler of its own, which is the
// one way a page meets a server that does not send it. The seeded Verification document holds
// no picture, so the row records a result carrying the issue's own uploaded shot.
test("the Verification filter is readable @phone", async ({ browser }) => {
  // Four loads of the filter - three widths and the rewritten answer - each read clause by
  // clause.
  test.setTimeout(240_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = await primarySlug(seeded.key);
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;

    await recordCheckResult(
      seeded.key,
      {
        check: seeded.checks.passed,
        commit: HEAD_SHA,
        failed: 0,
        negative_control: "Removed the row's state and the run failed.",
        observed: "1 passed, with a shot of the page",
        role: "tester",
        run: "bun run e2e -- e2e/issue-layout.e2e.ts",
        screenshots: [`dispatch://${seeded.key}/artifact/${seeded.documents.image}`],
        surface: "browser",
        verdict: "pass",
      },
      { actor: recorderActor, as: "agent" }
    );
    const recorded = await getIssueResults(seeded.key);
    const withdrawing = (recorded.checks ?? []).find(
      (check) => check.block_id === seeded.checks.uncontrolled
    );
    expect(withdrawing?.results ?? [], "the seed recorded no result to withdraw").not.toEqual([]);
    for (const result of withdrawing?.results ?? []) {
      await retractCheckResult(result.id, { reason: "The run was wrong." });
    }

    // What the issue's own reads say, which the page is read against.
    const statements = (await getIssueCoverage(seeded.key)).requirements ?? [];
    const covered = statements.filter((statement) => statement.checks > 0).length;
    const nodeOf = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const twice = blockNodeId(verificationSlug, seeded.checks.passed);

    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await openPreset(page, seeded.key, "verification");
      expect(await activePreset(page), `the filter is not named Verification at ${width} px`).toBe(
        PRESET_NAMES.verification
      );
      await expectIssuePageDrawn(page);
      await expect
        .poll(async () => (await verificationCards(page)).length, {
          message: `the Verification filter draws no statement at ${width} px`,
        })
        .toBeGreaterThan(0);
      const cards = await verificationCards(page);

      // Each statement of the issue is drawn, with the checks that serve it under it (R2).
      expect(
        cards.map((card) => card.id).sort(),
        `the Verification filter draws other statements than the issue's at ${width} px`
      ).toEqual(statements.map((statement) => blockNodeId(specSlug, statement.block_id)).sort());
      expect(
        new Set(cards.flatMap((card) => card.checks.map((check) => check.id))).size,
        `the Verification filter draws no six checks at ${width} px`
      ).toBe(6);

      // No value renders outside its card at any width, and no two values overlap: the sixteen
      // faults of the old tab this requirement is here for.
      expect(
        await boxesPastTheirCard(page, "verification"),
        `an element reaches past its card at ${width} px`
      ).toEqual([]);
      expect(
        await overlappingTexts(page, "verification"),
        `two pieces of text are drawn over each other at ${width} px`
      ).toEqual([]);

      // The page names one commit for the issue's results, the newest standing result's, drawn
      // once, in the ten characters the prototype draws a commit in, and on no result's own
      // line.
      expect(
        await namedCommit(page),
        `the page names no one commit for the issue's results at ${width} px`
      ).toBe(HEAD_SHA.slice(0, 10));
      expect(
        (await page.locator("[data-verification-result]").allInnerTexts()).filter((drawn) =>
          drawn.includes(HEAD_SHA.slice(0, 10))
        ),
        `a result draws a commit of its own at ${width} px`
      ).toEqual([]);

      // How many statements carry a check is drawn as covered, and no count, legend or heading
      // reads proven or met for it.
      expect(
        (await page.locator("[data-verification-covered]").innerText()).replace(/\s+/g, " "),
        `the count of the statements that carry a check does not read covered at ${width} px`
      ).toBe(
        `${covered} ${VERIFICATION_WORDS.of} ${statements.length} ${VERIFICATION_WORDS.covered}`
      );
      for (const part of [
        "[data-verification-covered]",
        "[data-verification-legend]",
        "[data-verification-head]",
      ]) {
        const read = (await page.locator(part).allInnerTexts()).join(" ").toLowerCase();
        expect(read, `${part} reads proven at ${width} px`).not.toContain("proven");
      }
      expect(
        (await page.locator("[data-verification-covered]").innerText()).toLowerCase(),
        `the coverage count reads met at ${width} px`
      ).not.toContain("met");

      // A statement reads Met where every check of it passed at that commit, Not met where one
      // failed there, and Unknown otherwise - here no result, an out-of-date one, a commit
      // Dispatch cannot place, and a withdrawn one.
      const stateOf = new Map(cards.map((card) => [card.id, card.state]));
      expect(
        [1, 2, 3, 4, 5, 6].map((index) => stateOf.get(nodeOf(index))),
        `a statement does not read by how its checks stand at ${width} px`
      ).toEqual(["met", "not_met", "unknown", "unknown", "unknown", "unknown"]);
      expect(
        cards.find((card) => card.id === nodeOf(1))?.stateWord,
        `the statement every check of which passed does not read Met at ${width} px`
      ).toBe(VERIFICATION_WORDS.states.met);
      expect(
        cards.find((card) => card.id === nodeOf(2))?.stateWord,
        `the statement one check of which failed does not read Not met at ${width} px`
      ).toBe(VERIFICATION_WORDS.states.not_met);

      // A statement no check serves carries its gap (R10).
      expect(
        cards.find((card) => card.id === nodeOf(7))?.gapKinds,
        `a statement no check serves carries no gap at ${width} px`
      ).toEqual(["no_check"]);
      expect(
        (await page.locator('[data-gap-kind="no_check"]').first().innerText()).trim(),
        `the gap is not said in the page's own words at ${width} px`
      ).toBe(GAP_LINES.no_check);

      // Each of the seven states a check can be in is told apart by a word and a shape. Six of
      // them stand on the seeded checks; the seventh is below, on the rewritten answer.
      const drawnChecks = cards.flatMap((card) => card.checks);
      const states = new Map(
        drawnChecks.map((check) => [check.state ?? "", `${check.word} ${check.shape}`])
      );
      expect(
        [...states.keys()].sort(),
        `the Verification filter draws not every seeded state at ${width} px`
      ).toEqual(["failed", "no-result", "older", "passed", "retracted", "unmarked"]);
      expect(
        [...states].map(([state, drawn]) => [state, drawn]),
        `a state is drawn in other words or another shape than the page declares at ${width} px`
      ).toEqual(
        [...states.keys()].map((state) => [
          state,
          `${NODE_STATE_WORDS.check[state as CheckState]} ${GLYPH_TOKENS.states.check[state as CheckState]}`,
        ])
      );
      expect(
        new Set([...states.values()]).size,
        `two of the seeded states are drawn the same way at ${width} px: ${JSON.stringify([...states])}`
      ).toBe(states.size);

      // Ruling 6: the body draws the newest result of each check, and its earlier results are
      // one line with their count, which opens them in place. The check with two results draws
      // one, says "1 earlier result", and draws both once that line is pressed.
      expect(
        drawnChecks.filter((check) => check.results > 1).map((check) => check.id),
        `a check draws more than its newest result at ${width} px`
      ).toEqual([]);
      expect(
        drawnChecks.find((check) => check.id === twice)?.earlier,
        `the check with two results does not say how many earlier ones it holds at ${width} px`
      ).toBe(`1 ${VERIFICATION_WORDS.earlier_one}`);
      const folded = page.locator(
        `[data-verification-check][data-node-id="${twice}"] [data-verification-earlier]`
      );
      await folded.click();
      await expect(folded, "the line does not say it opened").toHaveAttribute(
        "aria-expanded",
        "true"
      );
      expect(
        (await verificationCards(page))
          .flatMap((card) => card.checks)
          .find((check) => check.id === twice)?.results,
        `the line does not open the earlier result in place at ${width} px`
      ).toBe(2);
      await folded.click();

      // A command renders as code with a copy control, and `when` reads as words: both in the
      // check's own facts, which the reader's press on its words opens (R6).
      const opened = page.locator(`[data-verification-check][data-node-id="${twice}"]`);
      await opened.locator("[data-verification-check-words]").click();
      expect(
        await nodeCommands(page).count(),
        `the check's command is not drawn as code at ${width} px`
      ).toBeGreaterThan(0);
      expect(
        (
          await nodeCommands(page)
            .first()
            .evaluate((element) => getComputedStyle(element).fontFamily)
        ).toLowerCase(),
        `the command is not set in a monospace face at ${width} px`
      ).toMatch(/mono/);
      expect(
        await copyCommandControls(page).count(),
        `a command has no copy control at ${width} px`
      ).toBe(await nodeCommands(page).count());
      expect(
        (await opened.innerText()).replace(/\s+/g, " "),
        `the check draws its when attribute as a schema token at ${width} px`
      ).toContain(CHECK_WHEN_WORDS.before_merge);
      expect(
        await opened.innerText(),
        `the check draws the schema's own token at ${width} px`
      ).not.toContain("before_merge");

      // The screenshot the result recorded renders as a picture.
      const shot = opened.locator("[data-node-body] img").first();
      await expect(
        shot,
        `the seeded screenshot is drawn as no picture at ${width} px`
      ).toBeVisible();
      await expect
        .poll(() => shot.evaluate((element: HTMLImageElement) => element.naturalWidth), {
          message: `the seeded screenshot draws no image at ${width} px`,
        })
        .toBeGreaterThan(0);
      // The opened facts stay inside their card too.
      expect(
        await boxesPastTheirCard(page, "verification"),
        `an element of the opened check reaches past its card at ${width} px`
      ).toEqual([]);

      // Nothing the body draws is told apart by colour alone.
      await page.emulateMedia({ forcedColors: "active" });
      const forced = await verificationCards(page);
      expect(
        forced.flatMap((card) => card.checks.map((check) => `${check.word} ${check.shape}`)),
        `the states are told apart differently once the colours are forced at ${width} px`
      ).toEqual(drawnChecks.map((check) => `${check.word} ${check.shape}`));
      expect(
        forced.map((card) => card.stateWord),
        `the statements are told apart differently once the colours are forced at ${width} px`
      ).toEqual(cards.map((card) => card.stateWord));
      await page.emulateMedia({ forcedColors: "none" });
    }

    // The seventh state: a payload carrying no answer over a check's results at all, which is
    // an older build behind the same load balancer as the one this page came from. The page
    // says what is true of it - that it could not read how the check stands - and never one of
    // the other six.
    await page.route("**/api/v1/issues/*/results", async (route) => {
      const answer = await route.fetch();
      // The results answer as the server sends it, of which this handler reads two fields.
      const body: { checks?: { block_id: string; pass_stands?: unknown }[] } = await answer.json();
      for (const check of body.checks ?? []) {
        if (check.block_id === seeded.checks.passed) delete check.pass_stands;
      }
      await route.fulfill({ json: body, response: answer });
    });
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await openPreset(page, seeded.key, "verification");
    await expectIssuePageDrawn(page);
    await expect
      .poll(
        async () =>
          (await verificationCards(page))
            .flatMap((card) => card.checks)
            .filter((check) => check.state === "unreadable")
            .map((check) => [check.word, check.shape]),
        { message: "a check whose results the page cannot read draws no state of its own" }
      )
      .toEqual([[NODE_STATE_WORDS.check.unreadable, GLYPH_TOKENS.states.check.unreadable]]);
    await page.unroute("**/api/v1/issues/*/results");
  });
});

// R17, R20, the reviewer's finding on verification.e2e.ts:440: an issue that holds no statement
// and no check says so in the Verification filter's own column once the load has settled, in
// the page's one line for it, and the line is read inside the tree the filter draws and not off
// the whole page, which says it nowhere else.
test("the Verification filter says an issue holds no checks, in its own column", async ({
  browser,
}) => {
  const bare = async () => {
    await createProject({ key: PROJECT, name: "Issue layout" });
    return await createIssue({ force: true, project: PROJECT, title: "Nothing to verify yet" });
  };
  await phase(browser, bare, async (page, seeded) => {
    const settling = watchApiRequests(page);
    await openPreset(page, seeded.key, "verification");
    await expectLoadSettled(settling);
    await expectIssuePageDrawn(page);
    const body = issuePage(page).locator("[data-verification-body]");
    await expect(body, "the Verification filter draws no body of its own").toHaveCount(1);
    await expect(
      tree(page).getByText(EMPTY_STATE_LINES.verification, { exact: true }),
      "the filter's tree does not say the issue holds no checks"
    ).toHaveCount(1);
    await expect(
      page.getByText(EMPTY_STATE_LINES.verification, { exact: true }),
      "the line is drawn outside the Verification filter's tree as well"
    ).toHaveCount(1);
    expect(await verificationCards(page), "an issue with no statement draws one").toEqual([]);
    expect(await nodeRows(page).count(), "an issue with no check draws a row").toBe(0);
  });
});

/** How many rows of each kind `seedWholeIssue` draws with every branch open, from the seed's own
 *  record of what it wrote where it keeps one: the 12 requirements and the one citing a
 *  document, a result for each check but the one recorded with none, the two comment threads
 *  with their replies and the comment anchored to a block, the three messages of the one
 *  conversation, and the children. The rest it writes once each: the issue, and the open
 *  question and the answered one. The summary's callout is the head's three sentences and no
 *  row (R8), and no document and no file is a row (dispatch://INFRA-1196 R1, R3). */
function rowsOfTheWholeIssue(seeded: WholeIssueSeed): Record<string, number> {
  const checks = seeded.checks.all.length;
  return {
    ask: 2,
    check: checks,
    child_issue: seeded.children.keys.length,
    comment: seeded.comments.threads.length + seeded.comments.replies.length + 1,
    decision: seeded.decisions.length,
    issue: 1,
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

/** Who the model holds as each node's author, by the id the tree draws the node under: the
 *  issue, every question asked, every comment and message, and every recorded result. A node of
 *  any other kind has no author the model holds, and its row says so (R9). */
async function storedAuthors(key: string): Promise<Map<string, Actor>> {
  const issue = await getIssue(key);
  const slugOf = new Map((issue.artifacts ?? []).map((artifact) => [artifact.id, artifact.slug]));
  const authors = new Map<string, Actor>();
  if (issue.created_by !== null) authors.set(key, issue.created_by);
  for (const ask of await listIssueAsks(key)) {
    if (ask.kind !== "question") continue;
    authors.set(askNodeId(ask, slugOf), ask.author);
  }
  for (const comment of await listComments(key)) authors.set(comment.id, comment.author);
  for (const thread of await listIssueMessages(key)) {
    for (const message of [thread.message, ...(thread.replies ?? [])]) {
      authors.set(message.id, message.author);
    }
  }
  for (const check of (await getIssueResults(key)).checks ?? []) {
    for (const result of check.results ?? []) authors.set(result.id, result.actor);
  }
  return authors;
}

/** The name a row must draw for `actor`: a person by their login, which is their address, and
 *  an agent by its role's word, as the title its session stamped on the write gives it - the
 *  seed stamps every agent's (R9, the architect's ruling 6). */
function expectedAuthorName(actor: Actor): string {
  return actor.kind === "user" ? actor.id : agentRoleWord(actor.origin?.session_title ?? "");
}

/** An identifier where a name belongs: a UUID, which is what an Envoy session id is, or the
 *  short form `session:<8>…` other pages draw an untitled session by. */
const SESSION_ID_SHAPE = /[0-9a-f]{8}-[0-9a-f]{4}-|\bsession:/iu;

/** Every way the drawn authors of `rows` differ from what the model holds: a row whose node
 *  has an author and draws another name or an id, and a row whose node has none and draws one
 *  - it draws none, and only its opened body says the page's own "Author unknown" (ruling 5);
 *  once every name is right, a row drawing a shape of another kind than its author's. The names
 *  are read first, because a name drawn as an id is the defect this reading exists for. */
function authorOffences(rows: readonly DrawnRow[], stored: ReadonlyMap<string, Actor>): string[] {
  const offences: string[] = [];
  const ids = new Set([...stored.values()].map((actor) => actor.id));
  for (const row of rows) {
    const actor = stored.get(row.id ?? "");
    const unrecorded = row.body ? CONTROL_LABELS.unknown_author : null;
    const name = actor === undefined ? unrecorded : expectedAuthorName(actor);
    if (row.author !== name) {
      offences.push(
        `${row.id} draws its ${actor?.kind ?? "unrecorded"} author as "${row.author}", not "${name}"`
      );
    }
    if (
      actor !== undefined &&
      actor.kind !== "user" &&
      (SESSION_ID_SHAPE.test(row.author ?? "") || ids.has(row.author ?? ""))
    ) {
      offences.push(`${row.id} draws its author by an id: "${row.author}"`);
    }
  }
  if (offences.length > 0) return offences;
  for (const row of rows) {
    const actor = stored.get(row.id ?? "");
    const unrecorded = row.body ? GLYPH_TOKENS.authors.unknown : null;
    const shape =
      actor === undefined
        ? unrecorded
        : actor.kind === "user"
          ? GLYPH_TOKENS.authors.person
          : GLYPH_TOKENS.authors.agent;
    if (row.authorShape !== shape) {
      offences.push(
        `${row.id} draws its ${actor?.kind ?? "unrecorded"} author with the shape ${row.authorShape}`
      );
    }
  }
  return offences;
}

// c-author. What waits on dispatch://INFRA-1150, and is not read here: a reading whose author
// the server never recorded (no reading exists yet; every node the model holds no author for -
// a requirement, a check, a plan task, a note, a decision - is read for "Author unknown"
// instead), and an agent's decision's level of uncertainty and its reason, which today's model
// holds as one yes or no (`Decision.unsure`), read below as the word it draws.
test("an author is drawn by name and shape, and an agent's decision by its uncertainty @phone", async ({
  browser,
}) => {
  // The whole issue's every row, read with every branch open, beside the stored actor of each:
  // the same unfolding c-plain-words waits out, so the same deadline.
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const stored = await storedAuthors(seeded.key);
    // The seed holds both kinds of author, and an agent whose id is a session's UUID, so the
    // reading below meets a person, a titled agent and an id it must not draw.
    const kinds = new Set([...stored.values()].map((actor) => actor.kind));
    expect([...kinds].sort(), "the seed holds no author of one of the two kinds").toEqual([
      "session",
      "user",
    ]);
    expect(
      [...stored.values()].some((actor) => actor.id === recorderActor.id),
      "the seed records no result under the session whose id is a UUID"
    ).toBe(true);
    expect(
      [...stored.values()].some((actor) => actor.id === plannerActor.id),
      "the seed writes nothing as the planner's session"
    ).toBe(true);
    expect(PLANNER).toBe(plannerActor.id);

    // At 1440 px with every branch open, every node of the issue is a row: each node the model
    // holds an author for draws that author's name - an agent's by its role's word - and its
    // kind's shape, and every other draws no author on its row (rulings 5 and 6).
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    await expect
      .poll(async () => authorOffences(await drawnRows(page), stored), {
        message: "a row draws its author other than by the stored actor's name and kind",
      })
      .toEqual([]);
    await expect
      .poll(async () => rowsByKind(await drawnRows(page)), {
        message: "the unfolded tree does not draw a row for every node the seed wrote",
      })
      .toEqual(rowsOfTheWholeIssue(seeded));
    const rows = await drawnRows(page);
    const shapes = (kind: string | undefined) =>
      new Set(
        rows.filter((row) => stored.get(row.id ?? "")?.kind === kind).map((row) => row.authorShape)
      );
    expect([...shapes("user")], "the people's rows do not share one shape").toHaveLength(1);
    expect([...shapes("session")], "the agents' rows do not share one shape").toHaveLength(1);
    // A node the model holds no author for says so in its opened body, in the page's own words
    // and with a shape neither other kind draws, and an agent's opened body gives the whole
    // title its row's one word stands for.
    const unrecorded = rows.find(
      (row) => row.type === "requirement" && !stored.has(row.id ?? "") && !row.body
    );
    expect(
      unrecorded,
      "the unfolded tree draws no statement the model holds no author for"
    ).toBeDefined();
    const opened = await focusRow(page, unrecorded?.id ?? "");
    const said = rowBody(opened).locator("[data-node-author]");
    await expect(said, "an opened body does not say its node's author is unknown").toHaveText(
      CONTROL_LABELS.unknown_author
    );
    const unknownShape = await said.locator("svg[data-glyph]").getAttribute("data-glyph");
    expect(
      new Set([...shapes("user"), ...shapes("session"), unknownShape]).size,
      "a person, an agent and an author nobody recorded do not each have a shape of their own"
    ).toBe(3);
    const planned = rows.find((row) => stored.get(row.id ?? "")?.id === plannerActor.id);
    await expect(
      rowBody(await focusRow(page, planned?.id ?? "")).locator("[data-node-author-title]"),
      "the opened body of an agent's node does not give its session's whole title"
    ).toHaveText(PLANNER_TITLE);

    // The person's answer is drawn word for word, as the store holds it, and carries neither an
    // uncertainty nor a reason.
    const answer = (await getAsk(seeded.answeredAskId)).ask.answer;
    await nodeRow(page, seeded.answeredAskId).first().focus();
    await expect
      .poll(async () => rowOf(await drawnRows(page), seeded.answeredAskId)?.answerText ?? null, {
        message: "the answered ask's body does not draw the person's answer word for word",
      })
      .toBe(answer?.text ?? "");
    expect(
      rowOf(await drawnRows(page), seeded.answeredAskId)?.unsure ?? null,
      "the person's answer is drawn with an uncertainty"
    ).toBeNull();

    // The decision the seed marks unsure - its section holds a paragraph opening `Unsure:` -
    // says so in one word, and no other decision says it.
    const unsure = rows
      .filter((row) => row.type === "decision" && row.unsure !== null)
      .map((row) => ({ text: row.text, word: row.unsure }));
    expect(unsure, "the decisions marked unsure are not the one the seed marks").toHaveLength(1);
    expect(unsure[0]?.word).toBe(CONTROL_LABELS.unsure);
    expect(unsure[0]?.text).toContain("Fold to the frontier and not to a depth");

    // At 390 px the tree is read one node at a time, each row's author at the right end of its
    // first line (R34), and every row it draws there holds to the same reading.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => (await drawnRows(page)).filter((row) => row.author !== null).length, {
        message: "the narrow tree draws no row with an author at 390 px",
      })
      .toBeGreaterThan(0);
    await expect
      .poll(async () => authorOffences(await drawnRows(page), stored), {
        message: "a row at 390 px draws its author other than by the stored actor's name and kind",
      })
      .toEqual([]);
  });
});

// c-chain.
test("the chain is reachable and only gaps that tell a node apart are drawn", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const informed = blockNodeId(specSlug, requirementBlockId(1));
    const decision = blockNodeId(decisionsSlug, seeded.decisions[0] ?? "");
    const check = blockNodeId(verificationSlug, seeded.checks.passed);
    const results = (await getIssueResults(seeded.key)).checks ?? [];
    const latest = results.find((one) => one.block_id === seeded.checks.passed)?.results?.[0];
    expect(latest, "the seed recorded no result against the check that serves r1").toBeDefined();

    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);

    // The gaps the page draws are those that tell a node apart and name work somebody owes: each
    // requirement no check serves, and the plan task that serves no requirement - and no other
    // node draws any. The check whose latest result records no negative control is one of those
    // others: dispatch://INFRA-1253 R3 refuses such a result, so the page names nothing it lacks.
    const coverage = await getIssueCoverage(seeded.key);
    const wanted = new Map<string, string[]>();
    for (const requirement of coverage.requirements ?? []) {
      if (requirement.checks === 0) {
        wanted.set(blockNodeId(specSlug, requirement.block_id), ["no_check"]);
      }
    }
    wanted.set(blockNodeId(planSlug, planTaskBlockId(seeded.planTasks.length)), ["nobody_asked"]);
    const uncontrolled = (await getIssueResults(seeded.key)).checks?.find(
      (one) => one.block_id === seeded.checks.uncontrolled
    );
    expect(
      uncontrolled?.results?.[0]?.negative_control,
      "the seed's uncontrolled check's latest result records a negative control"
    ).toBe("");
    const drawnGaps = async () => {
      const drawn = new Map<string, string[]>();
      for (const row of await drawnRows(page)) {
        if (row.gapKinds.length > 0) drawn.set(row.id ?? "", [...row.gapKinds]);
      }
      return Object.fromEntries([...drawn].sort());
    };
    await expect
      .poll(drawnGaps, {
        message: "the page draws a gap on a node it does not tell apart, or none on one it does",
      })
      .toEqual(Object.fromEntries([...wanted].sort()));
    await expect
      .poll(async () => rowsByKind(await drawnRows(page)), {
        message: "the unfolded tree does not draw a row for every node the seed wrote",
      })
      .toEqual(rowsOfTheWholeIssue(seeded));

    // From the decision to the requirement it informed: the decision's chip names it, and the
    // keys follow it - `f` steps to the chip and Enter follows it to the requirement's own row.
    await nodeRow(page, decision).first().focus();
    await page.keyboard.press("f");
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "following the decision's chip does not put the focus on the requirement",
      })
      .toBe(informed);

    // From that requirement to its checks and their latest results: each is drawn under it, and
    // the arrow keys walk down to them from the requirement's row. The chip's address opens the
    // Spec preset, which keeps requirements alone, so the chain is read where nothing filters
    // it: the issue's own address, with every branch open (a filter that keeps a requirement
    // with its checks is a later item of this round).
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    expect(await parentOfRow(page, check), "the check that serves r1 is not under it").toBe(
      informed
    );
    expect(
      await parentOfRow(page, latest?.id ?? ""),
      "the check's latest result is not under the check"
    ).toBe(check);
    await nodeRow(page, informed).first().focus();
    const walked: (string | null)[] = [];
    for (let step = 0; step < 6; step += 1) {
      await page.keyboard.press("ArrowDown");
      walked.push(await focusedNodeId(page));
    }
    expect(walked, "the arrows from the requirement do not reach its check").toContain(check);
    expect(walked, "the arrows from the requirement do not reach the check's result").toContain(
      latest?.id ?? ""
    );

    // From the check, `w` reaches back to the decision that informed its requirement.
    await nodeRow(page, check).first().focus();
    await page.keyboard.press("w");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "w on the check does not move the focus to the decision behind its requirement",
      })
      .toBe(decision);

    // No gap that every node of a kind carries is drawn: no requirement, no decision and not
    // the issue's head says anything about a source, a read-back or an author.
    expect(
      (await drawnRows(page))
        .filter((row) => row.type === "requirement" || row.type === "decision")
        .flatMap((row) => row.gapKinds)
        .filter((kind) => kind !== "no_check"),
      "a requirement or a decision draws a gap its whole kind carries"
    ).toEqual([]);
    expect(await rowGaps(threeThings(page)).count(), "the head draws a gap").toBe(0);
    for (const words of ["No source given", "Not confirmed in Dispatch", "No author in Dispatch"]) {
      expect(await issuePage(page).getByText(words).count(), `the page draws "${words}"`).toBe(0);
    }

    // The Spec preset's list of requirements draws the same gap and no other: no check.
    await openPreset(page, seeded.key, "spec");
    await expectIssuePageDrawn(page);
    const listed = page.getByTestId("spec-requirement-gap");
    await expect
      .poll(() => listed.count(), { message: "the Spec preset's list draws no gap" })
      .toBeGreaterThan(0);
    expect(
      [...new Set(await listed.allInnerTexts())],
      "the Spec preset's list draws a gap other than no check"
    ).toEqual([GAP_WORDS.no_check]);
  });
});

/** Every string `value` holds, however deeply: a declared table's entries. */
function phrasesOf(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(phrasesOf);
  return [];
}

// c-plain-words. "With each filter on" waits on the filters, a later item of this round: the
// row walks the five presets the page draws today.
test("the page draws no internal word and no helper text @phone", async ({ browser }) => {
  // The check reads the page's chrome in ten views and in the body of every row the seeded
  // issue draws with every branch open - 67 of them - each reading read against five patterns
  // and the declared list. One `expect` per reading was 70,000 calls at about 4 ms each - 281 s
  // of assertion beside 20 s of page work - so each view gathers every offender it finds and
  // names them in one assertion, which reads the same strength and says more: a view that
  // draws three internal words names all three rather than the first. The row still carries a
  // deadline of its own, because ten navigations and 67 bodies outlast the 30 s default on
  // their own.
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
        await gotoIssue(page, presetPath(seeded.key, preset));
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
        // A fresh load of the page is no act of the reader's: nothing the page says in answer
        // to one - a refusal, a key's report, a write's line, a box's description - is drawn.
        const loaded = await issuePage(page).innerText();
        expect(
          answers.filter((answer) => loaded.includes(answer)),
          `the ${PRESET_NAMES[preset]} preset at ${size.width} px draws on load what the page says only in answer to the reader`
        ).toEqual([]);
      }
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
    // its own address, with its own text, its editor and its versions; Escape returns to the
    // outline.
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
    await page.keyboard.press("Escape");
    await expect(tree(page), "Escape does not return to the outline").toBeVisible();
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
    expect(await openedArtifact(page).count(), "Escape leaves the document drawn").toBe(0);

    // The block that cites a document carries a chip that opens it.
    const cited = artifacts.find((artifact) => artifact.slug === seeded.documents.cited);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    await unfoldEveryBranch(page);
    // A row drawn on one line carries its chips in its body, which the focus on it opens (R6).
    const citing = nodeRow(page, blockNodeId(specSlug, seeded.documents.citingBlock)).first();
    await citing.focus();
    const chip = citing
      .locator("[data-node-chip]")
      .getByRole("link", { exact: true, name: cited?.name ?? "" });
    await expect(chip, "the block that cites a document carries no chip for it").toHaveCount(1);
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
    // itself: its own address draws its versions, each with its download. The image draws its
    // picture at its own address.
    await gotoIssue(page, `/issues/${seeded.key}/artifacts/${seeded.documents.cited}`);
    await expect(issuePage(page)).toHaveCount(1);
    await expect(
      page.getByRole("combobox", { exact: true, name: "Version" }),
      "the document's versions are not reachable"
    ).toHaveCount(1);
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
      // The frame (R14): the icon rail from 768 px up, 56 px of the width, and none below it.
      if (width >= 768) {
        await expect(
          iconRail(page),
          `the issue page draws no icon rail at ${width} px`
        ).toHaveCount(1);
        expect((await iconRail(page).boundingBox())?.width, `the rail at ${width} px`).toBe(56);
      } else {
        await expect(iconRail(page), `a rail is drawn at ${width} px`).toHaveCount(0);
      }
      // From 1024 px the Contents pane stands at the outline's left: its list scrolls down by
      // design and is read for a sideways clip, and each of its one-line rows for a box that
      // reaches past its own. Below 1024 px the Outline control opens the same list in a sheet
      // over the page, which leaves the page fitting its viewport.
      if (width >= 1024) {
        await assertWholeAcross(contentsPane(page));
        const entries = await contentsEntries(contentsPane(page)).all();
        expect(entries.length, `the Contents pane lists nothing at ${width} px`).toBeGreaterThan(0);
        for (const entry of entries) await assertFitsItsBox(entry);
      } else {
        await expect(
          outlineControl(page),
          `the page draws no Outline control at ${width} px`
        ).toHaveCount(1);
        await outlineControl(page).click();
        await expect(
          outlineSheet(page),
          `the Outline control opens no list at ${width} px`
        ).toBeVisible();
        await assertPageFits(page);
        await assertWholeAcross(outlineSheet(page));
        await page.keyboard.press("Escape");
        await expect(outlineSheet(page)).toHaveCount(0);
      }
      // The panel holds the properties, the focused node's facts and the whole conversation, so
      // it is taller than the screen and scrolls down by design; R18 bans two dimensions, not
      // one, so Details, open over the page, is read for a sideways clip and a clamped line
      // (`e2e/unclipped.ts`), with the page under it still fitting its viewport.
      await openDetails(page);
      await assertPageFits(page);
      await assertWholeAcross(marginSlot(page));
      await closeDetails(page);
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
        `a code block, a table or a picture is drawn outside a node's own text at ${width} px`
      ).toEqual([]);

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
      await gotoIssue(page, presetPath(seeded.key, "spec"));
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

// c-reflow again, on the other surface the same run reaches: a child's own title, which is the
// words of the child's row of the outline (the architect's ruling 5). Measured at 320 px with
// the same 118 character path on 2026-11-09 at 454f2a3107a6, in the children's section the page
// drew then: 807 px of text in a 288 px box and a page 823 px wide.
test("a child whose title holds an unbroken run keeps the page inside its viewport @phone", async ({
  browser,
}) => {
  await phase(browser, seedUnbrokenChildTitle, async (page, seeded) => {
    for (const width of [320, 390, 1440]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      // The title is on the page before it is measured, so a fit read before the rows draw
      // cannot pass for the fit of a page that holds them.
      await expect(
        childRow(page, seeded.childKey).first(),
        `the outline draws no row for ${seeded.childKey} at ${width} px`
      ).toBeVisible();
      await expect(
        page.getByText(UNBROKEN_PATH, { exact: false }).first(),
        `the child's row draws none of its title's words at ${width} px`
      ).toBeVisible();
      await assertPageFits(page);
    }
  });
});

// c-typeface. The header is measured on this issue's own 115-character title, the one the spec's
// header widths were read on (dispatch://INFRA-1196/comment/f31b8ac4-874c-4735-94c8-8af17a95eba2):
// at 1440 px it fills most of the header's line in Inter, so the fallback the page drew before it
// shipped Inter, which drew that title a quarter wider, wraps it and gives the header a fourth
// line.
const MEASURED_TITLE =
  "Dispatch's issue page is redesigned from design research: an ordered body and a header that fits, with nothing lost";

test("the header holds its rule when the typeface does not load @phone", async ({ browser }) => {
  // Thirteen loads of the page: the first with the typeface; then on each of ruling 3's two
  // seeds two more with it and four without it.
  test.setTimeout(240_000);
  await phase(browser, seedWholeIssueAdvancing, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...TWENTY_LABELS], title: MEASURED_TITLE });

    // The built application serves the typeface, and the page is drawn in it.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(() => typefaceStatuses(page), {
        message: `the document's font set holds no ${TYPEFACE} face that loaded`,
      })
      .toEqual(["loaded"]);
    expect(
      (await identityBlockFamilies(page))[0],
      `the identity block's text is not drawn in ${TYPEFACE} first`
    ).toBe(TYPEFACE);
    const address = await typefaceAddress(page);
    if (address === null) {
      throw new Error(`the page's stylesheet declares no @font-face for ${TYPEFACE}`);
    }
    const file = await page.request.get(address);
    expect(file.status(), `${address} answers ${file.status()}`).toBe(200);
    expect(file.headers()["content-type"], `${address} is not served as a font`).toBe("font/woff2");
    expect(
      (await file.body()).subarray(0, 4).toString("latin1"),
      `${address} is not a WOFF2 file`
    ).toBe("wOF2");
    const notices = await (await page.request.get("/THIRD_PARTY_NOTICES.txt")).text();
    expect(notices, "the build's notices list no entry for the typeface").toContain(
      `${TYPEFACE}@4.1\nLicense: OFL-1.1`
    );
    expect(notices, "the build's notices hold no text of the typeface's licence").toContain(
      "SIL OPEN FONT LICENSE Version 1.1"
    );

    // On each of ruling 3's two seeds, no approval request waiting on the reader and then one
    // waiting, the header holds its limit at 1440 px and at 390 px in the typeface, and at
    // 390 px the outline's first row starts within its own (`headerLimit`:
    // dispatch://INFRA-1196/artifact/build-rulings-md, ruling 8); and with every request
    // for that file refused, the page is drawn in the fallback and holds the same limits, and
    // R18's reflow rule at four widths. Each reading is soft, so one run reports every width.
    const blocked = await asUser(browser, "alice");
    try {
      let refused = 0;
      await blocked.route(address, async (route) => {
        refused += 1;
        await route.abort();
      });
      const without = await blocked.newPage();
      for (const requestWaiting of [false, true]) {
        if (requestWaiting) {
          await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
        }
        for (const width of [DESKTOP.width, IPHONE_13.width]) {
          await page.setViewportSize(viewportAt(width));
          await gotoIssue(page, `/issues/${seeded.key}`);
          await expectIssuePageDrawn(page);
          await expect(identityBlock(page)).toContainText(MEASURED_TITLE);
          await expect
            .poll(() => typefaceStatuses(page), {
              message: `the ${TYPEFACE} face did not load at ${width} px`,
            })
            .toEqual(["loaded"]);
          await expectHeaderWithinItsLimit(page, width, requestWaiting, TYPEFACE);
        }
        for (const width of [DESKTOP.width, 1024, IPHONE_13.width, 320]) {
          await without.setViewportSize(viewportAt(width));
          await gotoIssue(without, `/issues/${seeded.key}`);
          await expectIssuePageDrawn(without);
          await expect(identityBlock(without)).toContainText(MEASURED_TITLE);
          // The fit is read with the tree's rows drawn, as c-reflow reads it: a page that holds
          // no row yet is a page that fits.
          await expect
            .poll(async () => await nodeRows(without).count(), {
              message: `the tree draws no row at ${width} px`,
            })
            .toBeGreaterThan(0);
          await expect
            .poll(() => typefaceStatuses(without), {
              message: `the ${TYPEFACE} face did not fail at ${width} px, so this is not the fallback`,
            })
            .toEqual(["error"]);
          await expectHeaderWithinItsLimit(without, width, requestWaiting, "the fallback");
          const fit = await without.evaluate(() => ({
            clientWidth: document.documentElement.clientWidth,
            scrollWidth: document.documentElement.scrollWidth,
          }));
          test.info().annotations.push({
            description: `${width} px: scrollWidth ${fit.scrollWidth}, clientWidth ${fit.clientWidth}`,
            type: "fit",
          });
          expect
            .soft(
              fit.scrollWidth,
              `the page scrolls sideways at ${width} px without the typeface: ${fit.scrollWidth} px of page in ${fit.clientWidth} px`
            )
            .toBe(fit.clientWidth);
        }
      }
      expect(refused, "no request for the typeface reached the refusal").toBeGreaterThan(0);
    } finally {
      await blocked.close();
    }
  });
});

// c-typeface, the typeface arriving late. The Contents list cuts each entry's first line at a word
// to the room its box leaves it, measuring the words on a canvas in the face the list is drawn in
// (`one-line.ts`), and `font-display: swap` (`styles.css`) draws the page's first frames in the
// fallback while Inter's file is on its way. This question is made of words the two faces draw at
// very different widths - at 14 px a "1" is 5 px in Inter and 8 px in the DejaVu fallback the
// browser suite's machines draw - so at the pane's width and at the sheet's the fallback's cut
// holds two words or more fewer than Inter's: an entry not cut again once the file arrives leaves
// out words its box has room for.
const FACE_MOVING_QUESTION =
  "Roll 1.1.11, 1.11.1, 11.1.1, 1.1.1, 11.11, 1.111, 111.1, 11.1.11 and 1.11.11 to the hosts first?";

/**
 * What is wrong with the line a Contents entry draws of `text`, read from the browser's own
 * layout in the face the page draws in now: the line must fit the box the entry leaves it, and
 * must hold the next word of `text` too wherever that word and the ellipsis fit there (R1's
 * first-line rule, cut at a word to the entry's width). Empty where the line is the right one.
 */
function contentsLineFaults(entry: Locator, text: string): Promise<string[]> {
  return entry.evaluate((element, text) => {
    const line = element.querySelector<HTMLElement>("[data-contents-line]");
    if (line === null) return ["the entry draws no line"];
    const shown = line.textContent ?? "";
    const words = shown.endsWith("…") ? shown.slice(0, -1) : shown;
    if (!text.startsWith(words)) {
      return [`the entry draws "${shown}", which "${text}" does not begin with`];
    }
    const room = line.getBoundingClientRect().width;
    const drawn = document.createRange();
    drawn.selectNodeContents(line);
    const width = drawn.getBoundingClientRect().width;
    const faults: string[] = [];
    if (width > room + 1) {
      faults.push(`"${shown}" is ${width.toFixed(1)} px, past its ${room.toFixed(1)} px box`);
    }
    const next = text.slice(words.length).trim().split(" ")[0] ?? "";
    if (next === "") return faults;
    // The line with its next word, laid out beside it in the same face and size.
    const probe = line.cloneNode(false) as HTMLElement;
    probe.textContent = `${words} ${next}…`;
    probe.style.cssText =
      "position: absolute; visibility: hidden; flex: none; width: auto; white-space: nowrap";
    line.after(probe);
    const longer = probe.getBoundingClientRect().width;
    probe.remove();
    if (longer <= room - 2) {
      faults.push(
        `"${shown}" leaves out "${next}", though "${words} ${next}…" is ${longer.toFixed(1)} px, inside its ${room.toFixed(1)} px box`
      );
    }
    return faults;
  }, text);
}

test("a Contents entry is cut again in the typeface once its file arrives @phone", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page, seeded) => {
    const question = await createAsk(seeded.key, { question: FACE_MOVING_QUESTION });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const address = await typefaceAddress(page);
    if (address === null) {
      throw new Error(`the page's stylesheet declares no @font-face for ${TYPEFACE}`);
    }

    // A reader whose request for the file is held until the list is drawn in the fallback: the
    // pane at 1440 px, and on a phone the Outline sheet, opened before the file arrives.
    const phone = test.info().project.name === "iphone";
    const reader = await asUser(browser, "alice");
    const arrives = Promise.withResolvers<void>();
    try {
      let held = 0;
      await reader.route(address, async (route) => {
        held += 1;
        await arrives.promise;
        await route.continue();
      });
      const late = await reader.newPage();
      await late.setViewportSize(phone ? IPHONE_13 : DESKTOP);
      // `commit`, because the page's load event waits on the held file.
      await late.goto(`/issues/${seeded.key}`, { waitUntil: "commit" });
      await expectIssuePageDrawn(late);
      if (phone) await outlineControl(late).click();
      const entry = (phone ? outlineSheet(late) : contentsPane(late)).locator(
        `[data-contents-entry="${question.id}"]`
      );
      await expect(entry, "the Contents list does not list the question").toBeVisible();
      await expect
        .poll(() => typefaceStatuses(late), { message: `the ${TYPEFACE} file is not held` })
        .toEqual(["loading"]);
      expect(held, "no request for the typeface reached the hold").toBeGreaterThan(0);
      await expect
        .poll(() => contentsLineFaults(entry, FACE_MOVING_QUESTION), {
          message: "the entry's line is not cut to its box in the fallback",
        })
        .toEqual([]);
      const inFallback = await entry.locator("[data-contents-line]").textContent();

      arrives.resolve();
      await expect
        .poll(() => typefaceStatuses(late), {
          message: `the ${TYPEFACE} face did not load once its file was let through`,
        })
        .toEqual(["loaded"]);
      await expect
        .poll(() => contentsLineFaults(entry, FACE_MOVING_QUESTION), {
          message: `the entry's line is not cut again in ${TYPEFACE} once its file arrived`,
        })
        .toEqual([]);
      expect(
        await entry.locator("[data-contents-line]").textContent(),
        "the typeface moved no word of the cut, so this row read nothing"
      ).not.toBe(inFallback);
    } finally {
      arrives.resolve();
      await reader.close();
    }
  });
});

/** The fixture `e2e/record-page-inventory.e2e.ts` wrote: what the old page offered, by state. */
interface PageInventoryFixture {
  readonly commit: string;
  readonly renames: Readonly<Record<string, string>>;
  /** Why a rename is the same control under a new name, keyed as `renames` is, for a rename
   *  that no tab-to-preset or block-to-words rule above explains on its own. */
  readonly renameReasons?: Readonly<Record<string, string>>;
  /** Links of the old page carried by a parent row of the outline (`page-inventory.ts`). */
  readonly parentRows: Readonly<Record<string, ParentRowMapping>>;
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
  if ((await actions.count()) === 1) {
    await expect(actions).not.toHaveAttribute("aria-expanded", "true");
  }
  const picker = priorityRow(page).getByRole("combobox", {
    exact: true,
    name: CONTROL_LABELS.priority_to_advance,
  });
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
      if ((await twisty.count()) > 0) await twisty.click();
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
          await gotoIssue(page, `/issues/${seeded.key}`);
          if (width >= 1024) await unfoldEveryBranch(page);
          const first = nodeRows(page);
          if ((await first.count()) > 0) await first.first().focus();
          return true;
        }
        const rows = nodeRows(page);
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
                await gotoIssue(page, presetPath(seeded.key, preset));
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
              // them - a check under the row's requirement - is read by the view that
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
        outline: outline(size.width),
        parentRows: fixture.parentRows,
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
        outline: outline(size.width),
        parentRows: fixture.parentRows,
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

    // The panel's conversation, and its composer with it, is mounted once the conversation's
    // own address has been opened (`IssuePage.tsx`'s `activatedTabs`), so the composer is
    // written in where a reader writes in it.
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await openDetails(page);
    await marginSlot(page).getByRole("textbox", { name: "Comment" }).fill("The composer writes.");
    await marginSlot(page).getByRole("button", { name: "Send" }).click();
    await expect
      .poll(async () => (await listComments(seeded.key)).map((comment) => comment.body))
      .toContain("The composer writes.");

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
    expect(await activePreset(page), "/verification does not open the Verification filter").toBe(
      PRESET_NAMES.verification
    );
    await expectIssuePageDrawn(page);
    await gotoIssue(page, `/issues/${seeded.key}/artifacts`);
    await expectIssuePageDrawn(page);
    expect(await openMenu(page).count(), "/artifacts does not open the node menu").toBe(1);

    // `/spec`, `/verification`, `/plan` and `/decisions` open their filter, and the issue's own
    // address the page with no filter on; none draws a document under it.
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetPath(seeded.key, preset));
      await expectIssuePageDrawn(page);
      expect(
        await activePreset(page),
        `${presetPath(seeded.key, preset)} does not open ${PRESET_NAMES[preset]}`
      ).toBe(PRESET_NAMES[preset]);
      expect(
        await page.getByRole("region", { name: "Document editor" }).count(),
        `${presetPath(seeded.key, preset)} draws a document under its filter`
      ).toBe(0);
    }

    // `/conversation` is the page with no filter on, the outline whole (R20); `/activity` opens
    // the panel at its history, beside that same outline (R15).
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    expect(await activePreset(page), "/conversation opens a filter").toBe(
      PRESET_NAMES.conversation
    );
    await expect(historyPanel(page), "/conversation opens the history").toHaveCount(0);
    await gotoIssue(page, `/issues/${seeded.key}/activity`);
    await expectIssuePageDrawn(page);
    await expect(
      historyPanel(page),
      "/activity does not open the panel at its history"
    ).toBeVisible();
    expect(await activePreset(page), "/activity opens a filter").toBe(PRESET_NAMES.conversation);
    // A comment agents wrote to each other is no row of the outline: its address opens the
    // history at its turn, marked, and draws its text there.
    const agentsOnly = await createComment(
      seeded.key,
      { body: "Planner to reviewer, outside every thread a person wrote in." },
      asPlanner
    );
    await gotoIssue(page, `/issues/${seeded.key}/comments/${agentsOnly.id}`);
    await expect(historyItem(page, agentsOnly.id)).toHaveAttribute("aria-current", "true");
    await expect(historyItem(page, agentsOnly.id)).toContainText("Planner to reviewer");
    expect(await nodeRow(page, agentsOnly.id).count(), "the agents' comment is a row").toBe(0);

    // The children, a document, and each entry of the conversation.
    await gotoIssue(page, `/issues/${seeded.key}/children`);
    await expectIssuePageDrawn(page);
    await expect(childRows(page), "/children draws no row a child").toHaveCount(
      seeded.children.keys.length
    );

    // A document's address opens that document - its own text on the page - in place of the
    // outline, and no row of the outline is the document (R3). `/artifact/<slug>` opens the same
    // document, and so does the spec's own address, which `/spec` does not.
    for (const [address, words] of [
      [`/issues/${seeded.key}/artifacts/${seeded.documents.cited}`, "notes"],
      [`/issues/${seeded.key}/artifact/${seeded.documents.cited}`, "notes"],
      [`/issues/${seeded.key}/artifacts/${specSlug}`, "spec"],
    ] as const) {
      await gotoIssue(page, address);
      await expect(issuePage(page), `${address} draws no issue page`).toHaveCount(1);
      if (words === "notes") {
        await expect(
          page.getByText("The measurements behind the first requirement.", { exact: true }),
          `${address} does not open that document`
        ).toBeVisible();
      }
      await expect(openedEditor(page), `${address} opens no document's editor`).toBeVisible();
      expect(await tree(page).count(), `${address} draws the outline beside the document`).toBe(0);
      expect(
        await nodeRow(page, seeded.documents.cited).count(),
        `${address} draws the document as a row of the outline`
      ).toBe(0);
    }

    // An ask the server indexed from a document's `ask` block *is* that block's node
    // (`e2e/issue-page.ts`), which is what the `?ask=` reading below asserts too: no row of the
    // tree carries the ask's own uuid, so each address names the node it opens. Each address
    // focuses that node in the outline, on screen, and draws its text there, wherever the item
    // is anchored: the comment is anchored in the spec, and the spec's addresses beside it open
    // the outline on it too (R20).
    const commentWords = "This requirement is the one the tree reads first.";
    for (const [address, node, words] of [
      [
        `asks/${seeded.openQuestion.askId}`,
        blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID),
        "At which width does the identity block condense?",
      ],
      [`comments/${seeded.comments.anchored}`, seeded.comments.anchored, commentWords],
      [
        `messages/${seeded.messages.root}`,
        seeded.messages.root,
        "The tree is folded to the frontier.",
      ],
      [`spec?comment=${seeded.comments.anchored}`, seeded.comments.anchored, commentWords],
      [
        `artifacts/${specSlug}?comment=${seeded.comments.anchored}`,
        seeded.comments.anchored,
        commentWords,
      ],
    ] as const) {
      await gotoIssue(page, `/issues/${seeded.key}/${address}`);
      await expectIssuePageDrawn(page);
      await expect
        .poll(() => focusedNodeId(page), { message: `/${address} focuses no node for it` })
        .toBe(node);
      await expect
        .poll(() => onScreen(nodeRow(page, node).first()), {
          message: `/${address} leaves its node off screen`,
        })
        .toBe(true);
      await expect(
        nodeRow(page, node).first(),
        `/${address} does not draw its node's text`
      ).toContainText(words);
      expect(
        await openedEditor(page).count(),
        `/${address} opens a document in place of the outline`
      ).toBe(0);
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
    await expect
      .poll(() => focusedNodeId(page), { message: "?comment= does not focus that comment" })
      .toBe(seeded.comments.anchored);

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

// c-plan-order, as the architect's ruling 5 reads R26 (build-rulings-md): the Plan filter opens
// on the chart of time, and one control switches to the ordered steps. The order and each bar's
// place are the page's own, worked out from the `needs` the plan document holds
// (`web/src/features/issue/plan-filter/plan-order.ts`), because today's reads give no plan
// read. What waits on dispatch://INFRA-1253, and is not read here: the position and the size
// the plan read gives each task - so every bar is one unit long and its row says "No size" -
// and a parent's Plan filter drawing its children's tasks from one request. The overtaken task
// waits on dispatch://INFRA-1950 R5, which is the state the model holds none of today. The
// tester's F19 is read here too: each task is drawn once, and after every task it needs.
test("the Plan filter draws the tasks as ordered steps @phone", async ({ browser }) => {
  test.setTimeout(150_000);
  await phase(browser, seedPlanOrder, async (page, seeded) => {
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const specSlug = await primarySlug(seeded.key);
    const step = (blockId: string) => blockNodeId(planSlug, blockId);
    const T = PLAN_ORDER_TASKS;
    // Each task after every task in its needs, which is not the order the plan is written in:
    // the document holds t1, t2, t3, and t1 needs t2 and t2 needs t3. The two that need each
    // other stand where the order puts them; the task whose need the plan no longer holds
    // waits on nothing.
    const bars = [step(T.third), step(T.second), step(T.first), step(T.loose)];
    const charted = [step(T.third), step(T.second), step(T.first), "cycle", step(T.loose)];
    const stepped = [
      step(T.third),
      step(T.second),
      step(T.first),
      step(T.knotA),
      step(T.knotB),
      step(T.loose),
    ];
    const chartOrder = async () =>
      (await planChartRows(page)).map((row) => (row.kind === "cycle" ? "cycle" : row.id));

    await openPreset(page, seeded.key, "plan");
    await expectIssuePageDrawn(page);

    // The filter opens on the chart, and its one control says so.
    await expect(planView(page, "chart"), "Plan does not open on the chart").toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(planView(page, "steps")).toHaveAttribute("aria-pressed", "false");
    await expect
      .poll(chartOrder, { message: "the chart does not draw each step after the steps it needs" })
      .toEqual(charted);
    const chart = await planChartRows(page);
    const bar = (id: string) => chart.find((row) => row.id === id);

    // One bar per step, each drawn once, starting where every task it needs has ended.
    expect(
      new Set(chart.filter((row) => row.kind === "bar").map((row) => row.id)).size,
      "a step is drawn more than once"
    ).toBe(bars.length);
    expect(
      bars.map((id) => bar(id)?.start),
      "a bar does not start after the tasks it needs"
    ).toEqual([0, 1, 2, 0]);
    for (const [task, needed] of [
      [T.first, T.second],
      [T.second, T.third],
    ] as const) {
      const mine = bar(step(task))?.bar;
      const theirs = bar(step(needed))?.bar;
      expect(
        mine?.left ?? -1,
        `the bar of ${task} starts before the bar of ${needed}, which it needs, has ended`
      ).toBeGreaterThanOrEqual((theirs?.left ?? 0) + (theirs?.width ?? 1e6) - 1);
    }
    // Today's model holds no size, so each bar is one unit long and its row says so (R26).
    const widths = bars.map((id) => Math.round(bar(id)?.bar?.width ?? 0));
    expect(new Set(widths).size, `the bars are not one unit long each: ${widths}`).toBe(1);
    expect(widths[0] ?? 0, "a bar draws no box").toBeGreaterThan(20);
    expect(
      bars.map((id) => bar(id)?.size),
      "a bar whose task holds no size does not say so"
    ).toEqual(bars.map(() => PLAN_WORDS.no_size));

    // Each step shows what it needs and the statements it serves, each statement a link to it;
    // the step that serves two statements is one row naming both.
    expect(bar(step(T.first))?.needs, `${T.first} does not name the step it needs`).toEqual([
      step(T.second),
    ]);
    expect(bar(step(T.third))?.needs, `${T.third} names a step it does not need`).toEqual([]);
    expect(bar(step(T.loose))?.needs, `${T.loose} names a step the plan does not hold`).toEqual([]);
    expect(
      bar(step(T.first))?.serves,
      `${T.first} names other than the one statement it serves`
    ).toHaveLength(1);
    expect(
      bar(step(T.third))?.serves,
      `${T.third} does not name both statements it serves`
    ).toHaveLength(2);
    await expect(
      nodeRow(page, step(T.first)).locator(
        `a[data-plan-serve][href$="#b-${requirementBlockId(1)}"]`
      ),
      `${T.first} does not link the statement it serves`
    ).toHaveCount(1);
    // The lines between a step and the steps it needs: two on this plan.
    await expect(
      issuePage(page).locator("[data-plan-link]"),
      "the chart does not draw one line for each need"
    ).toHaveCount(2);

    // The two tasks that need each other are one line that lists both, each a link to its own
    // node, and neither is a bar: no order can put either after every task it needs.
    const cycle = issuePage(page).locator("[data-plan-chart] [data-plan-cycle]");
    await expect(cycle, "the cycle is not drawn as one line").toHaveCount(1);
    await expect(cycle).toContainText(PLAN_WORDS.cycle);
    for (const knot of [T.knotA, T.knotB]) {
      await expect(cycle, `the cycle's line does not list ${knot}`).toContainText(`Task ${knot}`);
      await expect(
        cycle.locator(`a[href$="#b-${knot}"]`),
        `the cycle's line does not link ${knot} to its own node`
      ).toHaveCount(1);
      expect(bar(step(knot)), `${knot} is drawn as a bar as well as in the cycle`).toBeUndefined();
    }

    // The keys reach the bars in order.
    await nodeRow(page, step(T.third)).first().focus();
    for (const next of bars.slice(1)) {
      await page.keyboard.press("ArrowDown");
      await expect
        .poll(() => focusedNodeId(page), { message: `the arrow did not reach ${next}` })
        .toBe(next);
    }

    // The control switches to the ordered steps, which are the outline's own rows in the same
    // order, with the cycle's tasks as rows where the order puts the cycle.
    await planView(page, "steps").click();
    await expect(planView(page, "steps")).toHaveAttribute("aria-pressed", "true");
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: "the steps are not drawn after the steps they need",
      })
      .toEqual(stepped);
    const rows = await drawnRows(page);
    const first = rowOf(rows, step(T.first));
    expect(first?.chips.join(" "), `${T.first} does not say what it needs`).toContain(
      BLOCK_ATTRIBUTE_LABELS.needs
    );
    expect(first?.chips.join(" "), `${T.first} does not name the task it needs`).toContain(
      `Task ${T.second}`
    );
    expect(first?.parent?.id, `${T.first} does not name the statement it serves`).toBe(
      blockNodeId(specSlug, requirementBlockId(1))
    );
    expect(first?.parent?.type).toBe("requirement");
    await expect(
      issuePage(page).locator("[data-plan-cycle]"),
      "the steps do not draw the cycle's one line"
    ).toHaveCount(1);

    // The same order on a phone, in both views, where the page fits in one direction (R18).
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: "the steps' order is not the same at 390 px",
      })
      .toEqual(stepped);
    await assertPageFits(page);
    await planView(page, "chart").click();
    await expect
      .poll(chartOrder, { message: "the chart's order is not the same at 390 px" })
      .toEqual(charted);
    expect(
      (await planChartRows(page)).filter((row) => row.kind === "bar").map((row) => row.size),
      "a bar does not say it holds no size at 390 px"
    ).toEqual(bars.map(() => PLAN_WORDS.no_size));
    await assertPageFits(page);
  });
});

// c-review-list (R35). What waits on dispatch://INFRA-1150: the `shaped` tuple itself; today's
// tie is a decision block's link to a block of the spec.
test("only a decision that changed a statement waits for review @phone", async ({ browser }) => {
  test.setTimeout(150_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const id = (block: string) => blockNodeId(seeded.decisionsSlug, block);
    const partRows = async (part: string) =>
      (await decisionsPartsDrawn(page)).find((one) => one.part === part)?.rows ?? [];
    for (const size of [DESKTOP, PHONE]) {
      const at = `at ${size.width} px`;
      await page.setViewportSize(size);
      await openPreset(page, seeded.key, "decisions");
      await expectIssuePageDrawn(page);
      // The review list holds the two a tie names a statement for, newest first.
      await expect
        .poll(() => partRows("review"), { message: `the review list ${at}` })
        .toEqual([id(seeded.tiedNewer), id(seeded.tiedOlder)]);
      // Each with its Keep and Change controls, with no focus on it.
      for (const block of [seeded.tiedNewer, seeded.tiedOlder]) {
        await expect(
          nodeRow(page, id(block)).locator("button[data-row-action]"),
          `${block} draws no Keep and Change ${at}`
        ).toHaveText([CONTROL_LABELS.keep_decision, CONTROL_LABELS.change_decision]);
      }
      // The count holds those two and the two open questions, four, and neither other decision.
      await expect(waitingCount(page), `the count ${at}`).toHaveText("Waiting on you (4)");
      // The other two are in the filter's foot, with no Keep and no Change.
      expect(await partRows("agents"), `the foot draws its rows unasked ${at}`).toEqual([]);
      await decisionsFoot(page).click();
      await expect
        .poll(async () => [...(await partRows("agents"))].sort(), {
          message: `the foot's rows ${at}`,
        })
        .toEqual([id(seeded.tiedToNote), id(seeded.untied)].sort());
      for (const block of [seeded.tiedToNote, seeded.untied]) {
        await expect(
          nodeRow(page, id(block)).locator("button[data-row-action]"),
          `${block} draws a Keep or a Change ${at}`
        ).toHaveCount(0);
      }
    }
    // The other two are drawn in the outline under the issue, with no Keep or Change control,
    // the focus on them included.
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    for (const block of [seeded.tiedToNote, seeded.untied]) {
      const row = await focusRow(page, id(block));
      expect(await parentOfRow(page, id(block)), `${block} hangs under another row`).toBeNull();
      await expect(
        row.locator("button[data-row-action]"),
        `${block} draws a Keep or a Change in the outline`
      ).toHaveCount(0);
    }
    await expect(waitingCount(page)).toHaveText("Waiting on you (4)");
  });
});

// The tester's F6, under R7 and R35: a Change sent on a decision is a comment anchored to the
// decision's words. Once it is sent the decision says a change was asked for, leaves the review
// list for the reader's own decisions, takes no Keep and no Change, and draws the change under
// its words with a link to it; the count of what waits on the reader drops by one; and with no
// filter on the decision says the same and the change hangs under it.
test("a change sent on a decision is drawn on it, and the decision leaves the review list", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const id = (block: string) => blockNodeId(seeded.decisionsSlug, block);
    const changed = id(seeded.tiedNewer);
    const words = "Tie it to the first statement instead.";
    const partRows = async (part: string) =>
      (await decisionsPartsDrawn(page)).find((one) => one.part === part)?.rows ?? [];

    await openPreset(page, seeded.key, "decisions");
    await expectIssuePageDrawn(page);
    await expect.poll(() => partRows("review")).toEqual([changed, id(seeded.tiedOlder)]);
    await expect(waitingCount(page)).toHaveText("Waiting on you (4)");

    const row = nodeRow(page, changed).first();
    await row.locator('button[data-row-action="change"]').click();
    await row.getByLabel(CONTROL_LABELS.your_change).fill(words);
    await row.getByRole("button", { exact: true, name: CONTROL_LABELS.send_change }).click();

    // The decision leaves the review list and stands with the reader's own decisions.
    await expect
      .poll(() => partRows("review"), { message: "the changed decision still waits for review" })
      .toEqual([id(seeded.tiedOlder)]);
    expect(await partRows("yours"), "the changed decision is not among the reader's own").toEqual([
      changed,
    ]);
    // It says a change was asked for, by a word and a shape, and takes no Keep and no Change.
    expect(rowOf(await drawnRows(page), changed)).toMatchObject({
      state: "change_requested",
      stateWord: NODE_STATE_WORDS.decision.change_requested,
    });
    await expect(row.locator("button[data-row-action]")).toHaveCount(0);
    // The change is drawn under the decision's words, word for word, by who asked, with a link
    // to it in the conversation.
    const comment = (await listComments(seeded.key)).find((one) => one.body === words);
    expect(comment, "the server holds no comment for the change").toBeDefined();
    await expect(row.locator("[data-decision-change] [data-change-words]")).toHaveText(words);
    await expect(row.locator("[data-decision-change] [data-change-by]")).toHaveText("alice");
    await expect(row.locator("[data-decision-change] [data-change-link]")).toHaveAttribute(
      "href",
      `/issues/${seeded.key}/comments/${comment?.id}`
    );
    // The counts move: one decision fewer waits on the reader.
    await expect(waitingCount(page), "the count did not move").toHaveText("Waiting on you (3)");

    // With no filter on, the decision says the same and the change hangs under it.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    expect(rowOf(await drawnRows(page), changed)).toMatchObject({
      state: "change_requested",
      stateWord: NODE_STATE_WORDS.decision.change_requested,
    });
    expect(
      await parentOfRow(page, comment?.id ?? ""),
      "the change is not drawn under the decision it changes"
    ).toBe(changed);
    const outlined = await focusRow(page, changed);
    await expect(outlined.locator("button[data-row-action]")).toHaveCount(0);
    await expect(waitingCount(page)).toHaveText("Waiting on you (3)");
  });
});

// c-home. The Spec filter's body: the decision beside the statements it shaped, in the words it
// came from, and one line on each statement's own row (R5), drawn with the focus elsewhere and
// the pointer off the page; the tester's F7 is read here too - the line is a link to the
// decision, and `w` moves the focus to it. Partial on today's data: a statement carries no
// `from` (dispatch://INFRA-1098 item 21), so no statement hangs under an answered ask and the
// body draws one group with no heading; a criterion is dispatch://INFRA-1253's; an agent
// decision's level of uncertainty and its reason are dispatch://INFRA-1150's; and the versions
// of a filter's query are dispatch://INFRA-1253 R8, so this row reads the live Spec filter.
test("a spec node shows the decisions that shaped it at every version", async ({ browser }) => {
  test.setTimeout(150_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const decisionNode = (index: number) =>
      blockNodeId(decisionsSlug, seeded.decisions[index] ?? "");
    const stored = (await getIssueDecisions(seeded.key)).decisions ?? [];
    expect(
      stored.some((one) => one.kind === "person"),
      "the seed holds no decision the server reads as a person's"
    ).toBe(true);
    const answered = await getAsk(seeded.answeredAskId);
    const answer = answered.ask.answer?.text ?? "";
    expect(answer, "the seeded ask holds no answer to quote").not.toBe("");
    const quote = firstLineOf(answer).line;

    await openPreset(page, seeded.key, "spec");
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => (await decidedByCells(page)).filter((cell) => cell.said !== null).length, {
        message: "the Spec filter draws no decision beside its statements",
      })
      .toBeGreaterThan(0);
    // Nothing is focused and nothing is hovered: the lines are drawn by the page itself.
    await focusThePage(page);

    // The person's decision draws the three statements its ties point to, once, under one
    // cell, in the words it came from: the person's own answer to its first sentence end, the
    // question it answers, who answered, and a link to the whole answer.
    const cells = await decidedByCells(page);
    const mine = cells.filter((cell) => cell.said === "you");
    expect(mine, "the person's decision is not drawn once").toHaveLength(1);
    expect(
      mine[0]?.ids,
      "the person's decision does not stand beside the three statements it shaped"
    ).toEqual([statement(3), statement(4), statement(5)]);
    expect(mine[0]?.text).toContain(SPEC_WORDS.said_you);
    expect(mine[0]?.text).toContain(`“${quote}”`);
    expect(mine[0]?.text).toContain(answered.ask.question);
    expect(mine[0]?.text).toContain("alice");
    await expect(
      issuePage(page).locator('[data-spec-cell][data-said="you"] [data-spec-whole]'),
      "the cell carries no link to the whole answer"
    ).toHaveAttribute("href", `/issues/${seeded.key}/asks/${seeded.answeredAskId}`);

    // The agent's decision stands beside the two it shaped, marked as an agent's, in its own
    // words.
    const agents = cells.filter(
      (cell) => cell.said === "agent" && cell.text.includes("Chosen so the page makes no request")
    );
    expect(
      agents.flatMap((cell) => cell.ids).sort(),
      "the agent's decision does not stand beside the two statements it shaped"
    ).toEqual([statement(1), statement(2)].sort());
    expect(agents[0]?.text).toContain(SPEC_WORDS.said_agent);

    // One line on each statement's own row, with the same words, who decided and when; a
    // statement a second decision shaped names it after the first.
    const lines = await becauseLines(page);
    for (const index of [3, 4, 5]) {
      expect(
        lines[statement(index)],
        `statement ${index} carries no line for its decision`
      ).toContain(`${SPEC_WORDS.because} “${quote}”`);
      expect(lines[statement(index)]).toContain("alice");
    }
    expect(lines[statement(4)], "the second decision that shaped it is not named").toContain(
      SPEC_WORDS.informed_by
    );
    expect(lines[statement(1)], "the agent's line is drawn on the wrong statement").toContain(
      "Chosen so the page makes no request per node."
    );
    // A replaced decision draws no line: the statement it shaped stands with none, and its
    // cell says so, as does the cell of a statement no decision shaped (R35).
    expect(lines[statement(6)] ?? "", "the replaced decision draws a line").toBe("");
    expect(lines[statement(7)] ?? "", "a statement no decision shaped carries a line").toBe("");
    const unshaped = cells.find((cell) => cell.ids.includes(statement(6)));
    expect(
      unshaped?.said,
      "a statement only a replaced decision shaped names a decision"
    ).toBeNull();
    expect([
      EMPTY_STATE_LINES.spec_no_decision_one,
      EMPTY_STATE_LINES.spec_no_decision_many,
    ]).toContain(unshaped?.text);

    // Every statement of the query is a row of this body, and no decision and no issue is.
    const rows = await drawnRows(page);
    for (const requirement of seeded.requirements) {
      expect(
        rowOf(rows, blockNodeId(specSlug, requirement)),
        `the Spec filter does not draw ${requirement}`
      ).toBeDefined();
    }
    expect(
      rowOf(rows, decisionNode(0)),
      "the Spec filter draws a decision as a row"
    ).toBeUndefined();
    expect(rowOf(rows, seeded.key), "the Spec filter draws the issue as a row").toBeUndefined();

    // Nothing opens on hover: the pointer crosses every cell and the body's text is the same.
    const before = await issuePage(page).locator("[data-spec-body]").innerText();
    for (const cell of await issuePage(page).locator("[data-spec-cell]").all()) await cell.hover();
    expect(
      await issuePage(page).locator("[data-spec-body]").innerText(),
      "the body changed under the pointer"
    ).toBe(before);

    // F7: with the focus on a statement, its line is the link to the decision that shaped it,
    // and `w` moves the focus to that decision, which then draws the whole answer.
    const shaped = nodeRow(page, statement(3)).first();
    await shaped.focus();
    await expect(
      shaped.locator("[data-spec-because] a[data-spec-because-link]"),
      "the focused statement does not link the decision that shaped it"
    ).toHaveAttribute("href", `/issues/${seeded.key}/asks/${seeded.answeredAskId}`);
    await page.keyboard.press("w");
    await expect
      .poll(
        () =>
          page.evaluate(() => ({
            cell: document.activeElement?.hasAttribute("data-spec-cell") ?? false,
            said: document.activeElement?.getAttribute("data-said") ?? null,
          })),
        { message: "w did not move the focus to the decision behind the statement" }
      )
      .toEqual({ cell: true, said: "you" });
    await expect(
      issuePage(page).locator('[data-spec-cell][data-said="you"] [data-spec-whole-answer]'),
      "the focused decision does not draw the person's whole answer"
    ).toHaveText(answer, { useInnerText: true });
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

// c-decisions. Clause by clause, at 1440 px and at 390 px. What waits on dispatch://INFRA-1150
// and is not read here: an agent decision's author (`decided_by`), its level of uncertainty and
// its reason, the reading under an agent's decision, and the `shaped` tuple itself (today's tie
// is a decision block's link to a spec block). What waits on dispatch://INFRA-1253: the
// unconverted issue, `?view=decisions`, and "the page makes no request to /decisions".
test("the Decisions filter draws its four parts and quotes the person's answer", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedDecisionsFilter, async (page, seeded) => {
    const specSlug = await primarySlug(seeded.key);
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const D = (index: number) => blockNodeId(decisionsSlug, seeded.decisions[index] ?? "");
    const open = blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID);
    const stored = (await getAsk(seeded.optionedAskId)).ask.answer;
    expect(stored?.text ?? "", "the seed's answered ask holds no answer").not.toBe("");
    // A heading is its words and its count; the foot's is its control's, which says whether the
    // rows under it are drawn. White space between the parts of one is the browser's to lay out.
    const squeezed = (text: string) => text.replace(/\s+/gu, "");
    const headings = {
      agents: squeezed(`▸ 3 ${DECISIONS_WORDS.agents_many}`),
      open_questions: squeezed(`${DECISIONS_WORDS.parts.open_questions} 1`),
      review: squeezed(`${DECISIONS_WORDS.parts.review} 2`),
      yours: squeezed(`${DECISIONS_WORDS.parts.yours} 4`),
    };
    const parts = async () =>
      (await decisionsPartsDrawn(page)).map((part) => ({
        ...part,
        heading: squeezed(part.heading),
      }));
    for (const size of [DESKTOP, PHONE]) {
      const at = `at ${size.width} px`;
      await page.setViewportSize(size);
      await openPreset(page, seeded.key, "decisions");
      await expectIssuePageDrawn(page);
      // Four parts under their own headings, in this order: the open question first, the two
      // decisions waiting for review newest first, the reader's own decisions newest first -
      // the one kept, the question declined, the two answered - and the foot, folded.
      await expect.poll(parts, { message: `the four parts ${at}` }).toEqual([
        { count: "1", heading: headings.open_questions, part: "open_questions", rows: [open] },
        { count: "2", heading: headings.review, part: "review", rows: [D(5), D(0)] },
        {
          count: "4",
          heading: headings.yours,
          part: "yours",
          rows: [D(4), seeded.declinedAskId, seeded.optionedAskId, seeded.answeredAskId],
        },
        { count: "3", heading: headings.agents, part: "agents", rows: [] },
      ]);
      // No other sentence stands over them or between them.
      expect(
        await page.evaluate(() => {
          const body = document.querySelector("[data-decisions-body]")?.cloneNode(true);
          if (!(body instanceof Element)) return null;
          for (const row of body.querySelectorAll("[data-node-row]")) row.remove();
          return {
            paragraphs: body.querySelectorAll("p").length,
            text: (body.textContent ?? "").replace(/\s+/gu, ""),
          };
        }),
        `words outside the rows ${at}`
      ).toEqual({
        paragraphs: 0,
        text: `${headings.open_questions}${headings.review}${headings.yours}${headings.agents}`,
      });

      // A declined ask reads as a question the person declined, never as an answer.
      const declined = nodeRow(page, seeded.declinedAskId).first();
      await expect(declined.locator("[data-ask-declined] p").first()).toHaveText(
        `alice ${DECISIONS_WORDS.declined}`
      );
      await expect(declined.locator("[data-declined-note]")).toHaveText(
        "The release owner decides this one."
      );
      expect(rowOf(await drawnRows(page), seeded.declinedAskId)).toMatchObject({
        answerLine: null,
        answerText: null,
        state: "declined",
        stateWord: NODE_STATE_WORDS.ask.declined,
      });

      // The answered ask draws its question and options; with the focus on it, the person's
      // answer character for character as the store holds it, by who answered, with no reading
      // under it: the section of decisions.md that restates it is no row of the filter.
      const asked = nodeRow(page, seeded.optionedAskId).first();
      await asked.focus();
      await expect
        .poll(async () => rowOf(await drawnRows(page), seeded.optionedAskId)?.answerText ?? null, {
          message: `the answered ask does not draw the person's answer as the store holds it ${at}`,
        })
        .toBe(stored?.text ?? "");
      await expect(asked.locator('[data-node-option="chosen"]')).toHaveCount(1);
      await expect(asked.locator('[data-node-option="chosen"]')).toContainText("Rail");
      await expect(asked.locator("[data-node-option]")).toHaveCount(2);
      expect(
        rowOf(await drawnRows(page), seeded.optionedAskId)?.answerAuthor,
        `the answered ask does not name who answered ${at}`
      ).toBe(stored?.user);
      expect(await nodeRow(page, D(1)).count(), `the reading is a row of the filter ${at}`).toBe(0);

      // The three states, as words: waiting for review, kept with the keeper's name, replaced.
      await decisionsFoot(page).click();
      await expect
        .poll(async () => (await parts()).find((part) => part.part === "agents")?.rows.length, {
          message: `the foot did not unfold ${at}`,
        })
        .toBe(3);
      const rows = await drawnRows(page);
      expect(rowOf(rows, D(5))).toMatchObject({
        state: "waiting_review",
        stateWord: NODE_STATE_WORDS.decision.waiting_review,
      });
      expect(rowOf(rows, D(4))).toMatchObject({
        state: "kept",
        stateWord: `${NODE_STATE_WORDS.decision.kept} alice`,
      });
      expect(rowOf(rows, D(6))).toMatchObject({
        state: "replaced",
        stateWord: NODE_STATE_WORDS.decision.replaced,
      });
      expect(rowOf(rows, D(2))?.unsure, `the unsure decision does not say so ${at}`).toBe(
        CONTROL_LABELS.unsure
      );

      // The person's decision that shaped three statements is one entry with one node id - the
      // answer itself - and it reaches each of the three; the agent's reaches its two.
      expect(await nodeRow(page, seeded.answeredAskId).count()).toBe(1);
      await expect(
        nodeRow(page, seeded.answeredAskId).locator("[data-decision-statement] a"),
        `the person's decision does not reach the three statements it shaped ${at}`
      ).toHaveCount(3);
      await expect(nodeRow(page, D(0)).locator("[data-decision-statement] a")).toHaveCount(2);

      // Each entry links to the node that holds it, in the conversation.
      expect(
        await page.evaluate(() =>
          [...document.querySelectorAll("[data-decisions-body] [data-node-row]")]
            .filter((row) => row.querySelectorAll("a[data-entry-link]").length !== 1)
            .map((row) => row.getAttribute("data-node-id"))
        ),
        `entries with no link of their own ${at}`
      ).toEqual([]);
      await expect(
        nodeRow(page, seeded.optionedAskId).locator("a[data-entry-link]")
      ).toHaveAttribute("href", `/issues/${seeded.key}/asks/${seeded.optionedAskId}`);
    }

    // The same three words wherever a decision is drawn: with no filter on too, where the
    // section that restates the person's answer hangs under the ask it answers.
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    const outline = await drawnRows(page);
    expect(rowOf(outline, D(5))?.stateWord).toBe(NODE_STATE_WORDS.decision.waiting_review);
    expect(rowOf(outline, D(4))?.stateWord).toBe(`${NODE_STATE_WORDS.decision.kept} alice`);
    expect(rowOf(outline, D(6))?.stateWord).toBe(NODE_STATE_WORDS.decision.replaced);
    expect(
      await parentOfRow(page, D(1)),
      "the decision that answers the ask is not drawn under it"
    ).toBe(seeded.answeredAskId);
  });
});
