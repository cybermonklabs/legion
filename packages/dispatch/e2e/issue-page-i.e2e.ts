import AxeBuilder from "@axe-core/playwright";
import type { Browser, BrowserContext, Locator, Page } from "@playwright/test";
import type { IssueTreeRead } from "../web/src/api/types";
import { blockIdOfNode, decisionContent } from "../web/src/features/issue-page/decision-review";
import { firstLineOf } from "../web/src/features/issue-page/first-line";
import {
  FOOT_HEIGHT_PROPERTY,
  IDENTITY_HEIGHT_PROPERTY,
} from "../web/src/features/issue-page/identity-block";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { KEY_REPORTS } from "../web/src/features/issue-page/page-structure";
import {
  CONTROL_LABELS,
  DECLARED_WORDS,
  GLYPH_NAMES,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  type NodeType,
  nodeStateWords,
  PRESET_NAMES,
  PRESET_ORDER,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { REF_PREVIEW_OPEN_DELAY_MS } from "../web/src/features/refs/ref-preview-timing";
import { pageFocusedRow } from "../web/src/theme/classes";
import {
  answerAsk,
  claimIssue,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  getArtifact,
  getArtifactText,
  getAsk,
  getIssue,
  getIssueCoverage,
  getIssueDecisions,
  getIssueResults,
  listComments,
  listIssueAsks,
  listIssueMessages,
  listMyReplies,
  patchIssue,
  resolveComment,
} from "./api";
import { countDocumentSockets, cursorLabel } from "./editor";
import { nextFrames } from "./frames";
import {
  seedNoChildren,
  seedReplyNesting,
  seedSpineCases,
  seedUnbrokenChildTitle,
  seedUnbrokenWords,
  seedWholeIssue,
  seedWideCode,
  UNBROKEN_PATH,
  type WholeIssueSeed,
} from "./issue-layout-seed";
import { asPlanner, PROJECT } from "./issue-layout-seed-support";
import {
  ANSWERED_QUESTION_BLOCK_ID,
  decisionClosingWords,
  OPEN_QUESTION_BLOCK_ID,
  planTaskBlockId,
  requirementBlockId,
  requirementText,
  SPINE_ANSWERED_QUESTION,
  SPINE_CHECK_BLOCK_ID,
  SPINE_REQUIREMENTS,
  SPINE_SPEC,
} from "./issue-layout-seed-text";
import { filterRow, gotoIssue, issuePage, phase, presetToggle } from "./issue-page";
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
  parentOfRow,
  rowBody,
  rowLayouts,
  rowOf,
  rowWords,
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  bulletBoxes,
  caretInNode,
  caretInRowBlock,
  childEntry,
  childRow,
  childRows,
  childrenSection,
  childrenToggle,
  focusedNodeId,
  focusInside,
  gapKinds,
  identityBlockBottom,
  idsOf,
  keysListLines,
  onScreen,
  outlineRows,
  pageStatus,
  placeInStage,
  rowGaps,
  rowReference,
  rowTags,
  SHAPE_CONTRAST,
  shapesPainted,
  standInControlBelow,
  standInFootBox,
  tabStopRow,
  tooltipTexts,
  treeActiveDescendant,
  treeText,
} from "./issue-page-i";
import { composerField } from "./issue-page-l";
import { withIssuePage } from "./issue-page-option";
import {
  decisionUnderAskGate,
  heldAnswer,
  owedUnless,
  treeAnswerOf,
  treeReadRoute,
} from "./issue-page-t-tree";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * The outline's rows of the new issue page: the outline - the conversation, every node unfolded,
 * its children as rows below their section, its keys, and editing in a node - read on the real
 * stack. Each row turns the new page on for its reader with `withIssuePage` (`e2e/issue-page.ts`'s
 * `phase` and this file's `openTree`), seeds what it reads with the seeds of
 * `e2e/issue-layout-seed.ts`,
 * and reads the page through `e2e/issue-page.ts`, `e2e/issue-page-g.ts` and
 * `e2e/issue-page-i.ts`. The few selectors it spells itself are marks one row reads: a key's
 * report at a row, a crumb, a child's facts and key, a row's card and author, the links the
 * hover row rests on and the Inbox's owner link.
 *
 * The rows come from the checkpoint's `e2e/issue-layout.e2e.ts` and `e2e/keyboard-issue.e2e.ts`,
 * each under the title its check names (c-outline and c-keys).
 */

interface OpenedTree {
  readonly context: BrowserContext;
  readonly page: Page;
  readonly seeded: WholeIssueSeed;
}

/** The whole issue, opened on the new page by one signed-in reader with the focus on the page
 *  itself, which is where reading mode is. A key pressed before this returns reaches no handler,
 *  because the keymap binds only once sign-in resolves. It asserts only that the page loaded,
 *  that the page's opening put the focus on a row of its own and that the focus then left for the
 *  page itself (`gotoIssue`, `expectOpeningLanded`, `focusThePage`): each row below asserts the
 *  key or the tree property it is about first, and `expectTreeDrawn` after it. The suite's
 *  `test` has emptied the database before the row. */
async function openTree(browser: Browser): Promise<OpenedTree> {
  const seeded = await seedWholeIssue();
  const context = await asUser(browser, "alice");
  await withIssuePage(context, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ height: 900, width: 1280 });
  await gotoIssue(page, `/issues/${seeded.key}`);
  await expectOpeningLanded(page);
  await focusThePage(page);
  return { context, page, seeded };
}

/** The redesigned page and its tree are on the screen, which every row asserts after its own
 *  subject: a page that answers a key but draws no tree fails here. It waits for both, so a row
 *  that reads it straight after a load or a reload reads the page the app has drawn. */
async function expectTreeDrawn(page: Page): Promise<void> {
  await expect(
    issuePage(page),
    "the page draws no [data-issue-page] root, so this is not the redesigned issue page"
  ).toHaveCount(1);
  await expect(tree(page), 'the page draws no role="tree" named "Issue tree"').toHaveCount(1);
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

/** The focused row is on screen, clear of the sticky identity block and above the box at the
 *  foot, as it must be after every move and not only at the end. The foot is bound as `onScreen`
 *  bounds it: the window's height less the height the page publishes for that box
 *  (`FOOT_HEIGHT_PROPERTY`), unset while no box is drawn. */
async function expectFocusedRowClear(page: Page, after: string): Promise<void> {
  const id = await focusedRow(page);
  expect(id, `${after} left the focus on no row`).not.toBeNull();
  const row = (await drawnRows(page)).find((candidate) => candidate.id === id);
  const box = row?.box ?? null;
  expect(box, `${after} left the focus on a row with no box`).not.toBeNull();
  const header = (await identityBlockBottom(page)) ?? 0;
  expect(
    box?.y ?? -1,
    `after ${after} the focused row sits under the sticky header`
  ).toBeGreaterThanOrEqual(header - 1);
  const floor = await page.evaluate(
    (property) =>
      window.innerHeight -
      (Number.parseFloat(getComputedStyle(document.documentElement).getPropertyValue(property)) ||
        0),
    FOOT_HEIGHT_PROPERTY
  );
  expect(
    (box?.y ?? 0) + (box?.height ?? 0),
    `after ${after} the focused row is below the viewport or under the box at its foot`
  ).toBeLessThanOrEqual(floor);
}

/** The focused row is whole on screen, clear below the band that stays at the top and above the
 *  box at the foot, read with `onScreen`, which finds what stays at the top itself and bounds a
 *  row by the height the page publishes for that box. `failure` says what the move did wrong. */
async function expectFocusedRowOnScreen(page: Page, after: string, failure: string): Promise<void> {
  const id = await focusedRow(page);
  expect(id, `${after} left the focus on no row`).not.toBeNull();
  await expect
    .poll(() => onScreen(nodeRow(page, id ?? "").first()), { message: `${after} ${failure}` })
    .toBe(true);
}

/**
 * What is wrong with where a zoom left the crumb line it draws and the focused row, one line of
 * the failure each: the line's top under the band that stays at the top, as tall as the page
 * publishes on the document element (`IDENTITY_HEIGHT_PROPERTY`), and the focused row's top off
 * the screen - above that band, or below the window's foot less the box the page publishes the
 * height of there (`FOOT_HEIGHT_PROPERTY`). Empty where both stand on screen. The row is read by
 * its top, since a row taller than the window stands whole on it nowhere.
 */
function zoomMisplaced(page: Page): Promise<string[]> {
  return tree(page).evaluate(
    (element, { band, foot }) => {
      const published = getComputedStyle(document.documentElement);
      const top = Number.parseFloat(published.getPropertyValue(band)) || 0;
      const floor = window.innerHeight - (Number.parseFloat(published.getPropertyValue(foot)) || 0);
      const wrong: string[] = [];
      const line = element.parentElement
        ?.querySelector("[data-crumb]")
        ?.parentElement?.getBoundingClientRect();
      if (line === undefined) wrong.push("no crumb line is drawn");
      else if (line.top < top - 1) {
        wrong.push(
          `the crumb line stands at ${Math.round(line.top)} to ${Math.round(line.bottom)} px, under a band ${top} px tall`
        );
      }
      const row = document.activeElement?.closest("[data-node-row]")?.getBoundingClientRect();
      if (row === undefined) wrong.push("the focus is on no row");
      else if (row.top < top - 1 || row.top >= floor) {
        wrong.push(
          `the focused row's top stands at ${Math.round(row.top)} px, off the screen between ${top} and ${Math.round(floor)} px`
        );
      }
      return wrong;
    },
    { band: IDENTITY_HEIGHT_PROPERTY, foot: FOOT_HEIGHT_PROPERTY }
  );
}

/** The crumb line a zoom draws is below the band that stays at the top and the focused row's top
 *  is on screen (`zoomMisplaced`), once the zoom has drawn and still a second later. */
async function expectZoomPlaced(page: Page, after: string): Promise<void> {
  await expect
    .poll(() => zoomMisplaced(page), {
      message: `${after} left the crumb line under the band or the focused row's top off the screen`,
    })
    .toEqual([]);
  // The zoomed row goes on growing as its body draws, and a row placed again as it grew took
  // the line back under the band, where it still stood 1.5 s after the zoom: the same reading
  // after a second of that.
  await page.waitForTimeout(1_000);
  expect(await zoomMisplaced(page), `${after}, a second later`).toEqual([]);
}

/** Presses End or Home and reads that it moved the focus to the outline's last or first drawn
 *  row, which a row reads before where that row stands: a key that moved nowhere, or to another
 *  row, leaves a row on screen just as well. */
async function pressToOutlineEnd(page: Page, key: "End" | "Home"): Promise<void> {
  await page.keyboard.press(key);
  const ids = await visibleIds(page);
  const [end, row] = key === "End" ? ["last", ids.at(-1)] : ["first", ids[0]];
  expect(await focusedNodeId(page), `${key} did not move to the outline's ${end} row`).toBe(
    row ?? ""
  );
}

/** The slug of the issue's own spec, which is how its blocks are addressed. */
async function primarySlug(key: string): Promise<string> {
  const primary = ((await getIssue(key)).artifacts ?? []).find((artifact) => artifact.primary);
  expect(primary, `${key} carries no primary document`).toBeDefined();
  return primary?.slug ?? "";
}

/** The markdown the server stores for the document `artifactId`. */
async function storedMarkdown(artifactId: string): Promise<string> {
  return (await getArtifactText(artifactId)).markdown;
}

/** Opens the issue `key` on `page` - at its own address, or at `at` - and, once the page has put
 *  the focus on a row, presses End, which reaches the node `nodeId`, the outline's last row,
 *  named `what` in a failure; with `enter`, then Enter, which puts the caret in that node. */
async function enterNode(
  page: Page,
  key: string,
  nodeId: string,
  what: string,
  { at = `/issues/${key}`, enter }: { readonly at?: string; readonly enter: boolean }
): Promise<void> {
  await gotoIssue(page, at);
  await expectIssuePageDrawn(page);
  await expectOpeningLanded(page);
  await page.keyboard.press("End");
  await expect.poll(() => focusedRow(page), { message: `End did not reach ${what}` }).toBe(nodeId);
  if (!enter) return;
  await page.keyboard.press("Enter");
  await expect
    .poll(() => caretInNode(page), { message: `Enter did not put the caret in ${what}` })
    .toBe(true);
}

/** Makes the project CITED and an issue of it whose title runs past a phone's line, and answers
 *  that issue's key, for a document of the seeded project to cite. */
async function seedCitedIssue(): Promise<string> {
  await createProject({ key: "CITED", name: "Cited issues" });
  const cited = await createIssue({
    force: true,
    project: "CITED",
    title: "A cited issue of another project, whose title runs past a phone's line",
  });
  return cited.key;
}

/** The kinds a row draws no tag for, because they are the conversation's own words rather than
 *  a kind it revealed: one tag where the node has a typed kind. */
const UNTYPED_KINDS: readonly string[] = ["comment", "message"];

/**
 * Every row of a typed kind carries its kind's word and its kind's shape on its one tag, but a
 * child issue, whose tag is its key; a turn of the conversation carries neither, its author's
 * name and shape saying whose words it is; and every row its state word and its state's shape
 * where the model holds a state, and nothing where it holds none. Every word and token comes
 * from `page-words.ts`, so a type or state the page draws without declaring one fails here.
 * `tags` is each row's tag by its node's id (`rowTags`).
 */
function expectRowVocabulary(rows: readonly DrawnRow[], tags: ReadonlyMap<string, string>): void {
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
    } else if (type === "child_issue") {
      // G draws the key alone, a link to the child's own page, with no shape beside it: how the
      // child stands is its facts after its title (`NodeRow.tsx`'s `RowMargin`).
      expect(
        tags.get(row.id ?? ""),
        `row ${row.id} is a child issue whose tag is not its key`
      ).toBe(row.id);
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

/** The chips of `chips` that name a target whose stored words are `words`: a chip draws its
 *  target's words, cut as a title is, with an ellipsis where they carry on (`NodeRow.tsx`'s
 *  `Chip`). */
function chipsNaming(chips: readonly string[], words: string): string[] {
  return chips.filter((chip) => {
    const drawn = chip.replace(/\s*…$/u, "").trimEnd();
    return drawn !== "" && words.startsWith(drawn);
  });
}

/** The words of the block `id` of `spec`, a document a seed writes as one line of words under
 *  each block's opening line (`SPINE_SPEC`). */
function seededBlockWords(spec: string, id: string): string {
  const at = spec.indexOf(`{#${id}}\n`);
  expect(at, `the seeded spec holds no block ${id}`).not.toBe(-1);
  return spec.slice(at).split("\n")[1] ?? "";
}

/**
 * The rule for a row's one line - a folded branch's - read twice, because the text alone cannot
 * see the cut a box makes: a box that hides the tail of its line leaves every character of it in
 * the line's text. So the line is read for its words - the start of the stored text, ending at a
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

/** The kinds of node the issue holds that are no node of its conversation, so no row of the
 *  outline is one. */
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
 *  the page: every node Conversation draws by the id the tree draws it under - Conversation's
 *  read holds no message, result or plan task, and a comment on a node's text is drawn beside
 *  that node and not as a row - the nodes of the issue's conversation it leaves out
 *  (`leftOut`), which Graph draws, the slugs of its documents and files, which no row may be,
 *  the summary callout, which the head draws (c-three-things), and the words the server stores
 *  for each node whose read sends them, by the same id: an ask's question, a comment's and a
 *  message's body, and a requirement's, a decision's and a check's text. */
interface SeededOutline {
  readonly leftOut: ReadonlySet<string>;
  readonly nodes: ReadonlySet<string>;
  readonly slugs: ReadonlySet<string>;
  readonly summary: string;
  readonly words: ReadonlyMap<string, string>;
}

async function seededOutline(seeded: WholeIssueSeed): Promise<SeededOutline> {
  const artifacts = (await getIssue(seeded.key)).artifacts ?? [];
  const slugOf = new Map(artifacts.map((artifact) => [artifact.id, artifact.slug]));
  const slug = (id: string) => slugOf.get(id) ?? "";
  // The issue itself is no row: the header is the issue.
  const nodes = new Set<string>(seeded.children.keys);
  const leftOut = new Set<string>();
  const words = new Map<string, string>();
  const add = (id: string, stored: string) => {
    nodes.add(id);
    words.set(id, stored);
  };
  for (const ask of await listIssueAsks(seeded.key)) {
    if (ask.kind !== "question") continue;
    add(askNodeId(ask, slugOf), ask.question);
  }
  for (const comment of await listComments(seeded.key)) {
    if (comment.anchor === null) add(comment.id, comment.body);
    else leftOut.add(comment.id);
  }
  for (const thread of await listIssueMessages(seeded.key)) {
    for (const message of [thread.message, ...(thread.replies ?? [])]) {
      leftOut.add(message.id);
      words.set(message.id, message.body);
    }
  }
  for (const requirement of (await getIssueCoverage(seeded.key)).requirements ?? []) {
    add(blockNodeId(slug(seeded.specArtifactId), requirement.block_id), requirement.text);
  }
  // Each decision is a `decision` block of a document of the issue, which the decisions read's
  // entry names (`document`), drawn under that block's node with its own content's words
  // (`decisionContent`); an entry that names no document is an ask, read above.
  for (const entry of (await getIssueDecisions(seeded.key)).entries ?? []) {
    if (entry.document === null) continue;
    const id = blockNodeId(slug(entry.document.artifact_id), entry.document.block_id);
    add(id, decisionContent(entry).text);
  }
  for (const check of (await getIssueResults(seeded.key)).checks ?? []) {
    add(blockNodeId(slug(check.artifact_id), check.block_id), check.text);
    for (const result of check.results ?? []) leftOut.add(result.id);
  }
  for (const task of seeded.planTasks) {
    leftOut.add(blockNodeId(slug(seeded.planArtifactId), task));
  }
  return {
    leftOut,
    nodes,
    slugs: new Set(artifacts.map((artifact) => artifact.slug)),
    summary: blockNodeId(slug(seeded.specArtifactId), seeded.summaryBlockId),
    words,
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

/** How many rows of each kind Conversation draws of `seedWholeIssue` with every branch open, from
 *  the seed's own record of what it wrote where it keeps one: the 12 requirements and the one
 *  citing a document, each check, the two comment threads with their replies, and the children.
 *  The rest it writes once each: the open question and the answered one. Conversation's read
 *  holds no result, plan task or message, and the comment anchored to a block is drawn beside
 *  that block and not as a row. The issue itself is no row, the summary's callout is the head's
 *  three sentences and no row, and no document and no file is a row. */
function rowsOfTheWholeIssue(seeded: WholeIssueSeed): Record<string, number> {
  return {
    ask: 2,
    check: seeded.checks.all.length,
    child_issue: seeded.children.keys.length,
    comment: seeded.comments.threads.length + seeded.comments.replies.length,
    decision: seeded.decisions.length,
    requirement: seeded.requirements.length + 1,
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

// c-tree. On the issue today's model holds, the one the conversion has not reached, with no
// filter on, at 1440 px. This row carries no `@phone`: below 1024 px the tree is read one node
// at a time under a breadcrumb, one level below the node it is on, and every level, ancestor and
// fold below would be a different claim; the row under this one reads the clauses the width
// keeps.
//
// What waits on another issue, and is not read here: a criterion, a reading, a lens and a
// finding, which the model cannot hold yet; the summary callout, which the head draws and
// c-three-things reads, and which this row leaves out of its reading either way; and which row
// of a node the edit key opens, which is c-editing's and touches nothing this row reads.
// SLOW(>=2s): the whole issue seeded and read back through seven API reads, one load waited out
// until its reads settle, then every branch unfolded press by press and the rows read whole.
test("with no filter the page is the conversation, each node edited once, every node unfolded", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // One more turn of the seed's conversation before the page opens: the planner cites the
    // first child by its address, which the page draws as a link to an issue, by its key and
    // its title. It answers the seed's message, since the outline draws a thread only once a
    // person wrote in it (`agents-only.ts`).
    const cited = seeded.children.keys[0] ?? "";
    const citing = await createMessage(
      seeded.key,
      {
        body: `The child dispatch://${cited} is where the rows were first drawn.`,
        in_reply_to: seeded.messages.root,
      },
      asPlanner
    );
    const outline = await seededOutline(seeded);
    const specSlug = await primarySlug(seeded.key);
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);

    // The outline's measure: at most 1000 px wide, the prototype's `#pane`, and centred in
    // the stage, its gaps to the stage's insets on its two sides a pixel apart at most.
    const place = await placeInStage(tree(page));
    expect(place, "the tree stands in no stage").not.toBeNull();
    expect(
      place?.width ?? Number.POSITIVE_INFINITY,
      `the tree is ${place?.width} px wide, wider than the outline's 1000 px`
    ).toBeLessThanOrEqual(1000);
    expect(
      Math.abs((place?.left ?? 0) - (place?.right ?? Number.POSITIVE_INFINITY)),
      `the tree is not centred in the stage: ${place?.left} px at its left, ${place?.right} px at its right`
    ).toBeLessThanOrEqual(1);

    // No document, file or handoff is a row, folded or not: the spec is drawn nowhere
    // in the outline.
    const folded = await drawnRows(page);
    expectNoIssueNodeRows(folded, outline.slugs, "the page as it opens");
    expect(
      folded.filter((row) => row.role !== "treeitem").map((row) => row.id),
      "these rows of the tree are no treeitem"
    ).toEqual([]);
    expectRowVocabulary(folded, await rowTags(page));

    // No row repeats the issue: the first row is the first node under it (row 2). What stands
    // first under the view row starts at most 15 px under its foot, the prototype's 14 px gap
    // under `#tabs`, and 1 px for rounding: the outline's first row, or, where the issue has
    // children, the children's section above the outline (R14), which the outline's first row
    // stands under.
    expect(
      rowOf(folded, seeded.key),
      "the outline draws a row for the issue itself"
    ).toBeUndefined();
    const viewRowBox = await filterRow(page).boundingBox();
    expect(viewRowBox, "the page draws no view row").not.toBeNull();
    const viewRowFoot = (viewRowBox?.y ?? 0) + (viewRowBox?.height ?? 0);
    const firstTop = folded.find((row) => row.visible)?.box?.y ?? Number.POSITIVE_INFINITY;
    // The seed files children under the issue, so the children's section stands first.
    const sectionBox =
      seeded.children.keys.length > 0 ? await childrenSection(page).boundingBox() : null;
    expect(
      (sectionBox?.y ?? firstTop) - viewRowFoot,
      "what stands first under the view row stands more than 15 px under its foot"
    ).toBeLessThanOrEqual(15);
    expect(
      firstTop,
      "the outline's first row stands above the foot of the children's section"
    ).toBeGreaterThanOrEqual((sectionBox?.y ?? 0) + (sectionBox?.height ?? 0));

    // The page opens with every node unfolded: no branch is folded but by the reader.
    expect(
      folded.filter((row) => row.expanded === "false").map((row) => row.id),
      "a branch opens folded"
    ).toEqual([]);

    // The outline draws a row for each node of the conversation the seed wrote that
    // Conversation's read holds - each ask, post and reply, requirement, decision and check, and
    // each child under the issue - read by node id against the server's own reads, and a row for
    // nothing else: no message, result or plan task, and no comment on a node's text.
    await unfoldEveryBranch(page);
    // No fold is left to press, and the ids read here are held to every node of the
    // conversation below, so a read short of a row fails there before the loop at the foot.
    const whole = await drawnRows(page);
    expectNoIssueNodeRows(whole, outline.slugs, "the page with every branch open");
    // The memo callout is the head's three sentences and no row of the outline.
    expect(idsOf(whole), "the memo callout is drawn as a row of the outline").not.toContain(
      outline.summary
    );
    expect(
      [...new Set(idsOf(whole))].sort(),
      "the rows of the outline are not the nodes of the issue's conversation"
    ).toEqual([...outline.nodes].sort());
    // The children are rows of the outline's top level, under no other row: the issue itself
    // is no row.
    for (const child of seeded.children.keys) {
      expect(ancestorsIn(whole, child), `the child ${child} sits under another row`).toEqual([]);
    }

    // A branch the reader folds shows its first line and counts the nodes under it: the first
    // requirement, folded by its control.
    const firstRequirement = blockNodeId(specSlug, requirementBlockId(1));
    await nodeRow(page, firstRequirement)
      .first()
      .getByRole("button", { exact: true, name: CONTROL_LABELS.fold })
      .click();
    const reFolded = await drawnRows(page);
    expect(
      rowOf(reFolded, firstRequirement)?.foldedCount,
      "the folded first requirement does not count the nodes under it"
    ).toBe(String(heldIn(whole, firstRequirement)));

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
    const answered = blockNodeId(specSlug, ANSWERED_QUESTION_BLOCK_ID);
    const asked = rowOf(whole, answered);
    expect(asked?.answerLine ?? "", "the answered ask's row draws no answer").toContain(
      firstLineOf(words).line
    );
    expect(asked?.answerAuthor, "the answered ask's row does not name who answered").toBe(
      stored?.user
    );
    expect(
      whole
        .filter((row) => row.id !== answered && (row.words ?? row.firstLine) === words)
        .map((row) => row.id),
      "the answer is drawn as a row of its own"
    ).toEqual([]);

    // A folded branch is where the page draws one line for a node (the architect's ruling on a
    // node's one line): that line ends at a sentence end or at 100 characters, always at a word,
    // and is the first line of the words the server stores for the node, cut by the page's own
    // rule (`firstLineOf`). Every other row draws its words whole. The first requirement, folded
    // above, is that branch.
    const row = rowOf(reFolded, firstRequirement);
    const line = (row?.firstLine ?? "").replace(/\s*(?:…|\.\.\.)$/u, "").trimEnd();
    expect(
      row?.firstLine,
      "the folded two-sentence requirement draws no folded line"
    ).not.toBeNull();
    expect(line.length, `the folded line is ${line.length} characters`).toBeLessThanOrEqual(100);
    expect(line, "the folded line is not the first line of its stored words").toBe(
      firstLineOf(outline.words.get(firstRequirement) ?? "").line
    );
    expect(row?.words, "the folded row draws its words whole as well").toBeNull();
    // A line that ends at a sentence end has no mark after it, and a cut line has one `…` (row 20
    // drew four dots).
    expect(
      /[.!?]\s*(?:…|\.\.\.)$/u.test(row?.firstLine ?? ""),
      `the folded line "${row?.firstLine}" ends with a sentence end and a mark`
    ).toBe(false);
    expect(row?.firstLine ?? "", "the folded line ends with three dots").not.toMatch(/\.\.\.$/u);
    expect(
      cutsAWord(requirementText(1), row?.firstLine ?? ""),
      `the first line "${row?.firstLine}" cuts the stored text inside a word`
    ).toBe(false);
    // Unfolded again, the row draws the node's whole stored text.
    const first = nodeRow(page, firstRequirement).first();
    await first.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
    await expect
      .poll(async () => (await rowWords(first).innerText()).replace(/\s+/g, " "), {
        message: "the unfolded row does not draw the node's whole stored text",
      })
      .toContain(requirementText(1));

    // Graph, which draws the whole tree read, draws the messages, results and plan tasks
    // Conversation leaves out, the planner's message among them: its citation of a child is a
    // link to the child, by its key and its title.
    const graphSettling = watchApiRequests(page);
    await gotoIssue(page, presetPath(seeded.key, "graph"));
    await expectIssuePageDrawn(page);
    await expectLoadSettled(graphSettling);
    const onText = new Set(
      (await listComments(seeded.key)).flatMap((one) => (one.anchor === null ? [] : [one.id]))
    );
    const graph = await drawnRows(page);
    expect(
      [...outline.leftOut].filter((id) => !onText.has(id) && rowOf(graph, id) === undefined),
      "Graph draws no row of a message, a result or a plan task Conversation leaves out"
    ).toEqual([]);
    await expect(
      rowReference(nodeRow(page, citing.id).first(), `dispatch://${cited}`),
      `the message that cites ${cited} does not draw it as its key and its title in Graph`
    ).toHaveText(`${cited} ${(await getIssue(cited)).title}`);
  });
});

/** The widths a folded branch's line is read at below 1024 px: the narrowest width the page
 *  holds to, the phone the `iphone` project runs, the last width the phone's classes draw and
 *  the first that draws the desktop's (`max-[761px]:` is `width < 761px`), and the narrowest
 *  width above a phone's (`PHONE_VIEWPORT_QUERY` is under 768 px). */
const ONE_LINE_WIDTHS = [320, 390, 760, 761, 768] as const;

/**
 * The two gaps at a phone's bullet. Between a node's bullet column and its words stands the
 * row's own 6 px (`NODE_ROW_LAYOUT.box`, `NodeRow.tsx`) and no inset of the tree's box: at most
 * 7 px, a pixel for rounding, read on each row whose node holds no other, where the bullet is
 * the column itself, and whose words are not the reader's own, which stand inside their ink rule
 * (the row's second left edge). A node that holds others draws its 24 px fold control
 * reaching past that column on both sides (`NODE_ROW_LAYOUT.foldControl`): it ends before its
 * words, so it covers none of them, and it stays inside the page.
 */
async function expectPhoneBulletGaps(page: Page, spec: string, where: string): Promise<void> {
  // Its one caller waited for every read of the load to settle and for the rows to draw, so the
  // rows' layout is read as it stands now; the two counts at the foot fail a reading of no row.
  const layouts = await rowLayouts(page, spec);
  const bullets = await bulletBoxes(page);
  expect(
    bullets.map((bullet) => bullet.id),
    `${where}: the bullets were read off other rows than the rows' layouts`
  ).toEqual(layouts.map((row) => row.id));
  let leaves = 0;
  let holders = 0;
  for (const [at, row] of layouts.entries()) {
    const bullet = bullets[at];
    if (row.fold === null) {
      if (row.own) continue;
      leaves += 1;
      const gap = row.wordsLeft - (bullet?.right ?? Number.NEGATIVE_INFINITY);
      expect(
        gap,
        `${where}: the words of ${row.id} start ${gap} px right of its bullet column`
      ).toBeLessThanOrEqual(7);
      continue;
    }
    holders += 1;
    expect(
      bullet?.right ?? Number.POSITIVE_INFINITY,
      `${where}: the fold control of ${row.id} ends at ${bullet?.right} px, over its words, which start at ${row.wordsLeft} px`
    ).toBeLessThanOrEqual(row.wordsLeft);
    expect(
      bullet?.left ?? Number.NEGATIVE_INFINITY,
      `${where}: the fold control of ${row.id} reaches past the page's left edge, to ${bullet?.left} px`
    ).toBeGreaterThanOrEqual(0);
  }
  expect(leaves, `${where} draws no row of a node that holds no other`).toBeGreaterThan(0);
  expect(holders, `${where} draws no row of a node that holds others`).toBeGreaterThan(0);
}

// c-tree, at the widths below 1024 px, which is a row of its own: the row above reads the whole
// tree at 1440 px. This reads the clauses a narrow page keeps: every node opens unfolded, its
// words whole, and a branch the reader folds draws one line - its node's first line, cut at a
// word by the rule of a node's one line - and how many nodes it holds, in a box that hides none
// of it. It carries `@phone-only` beside `@phone` because the row above reads the same clauses
// where the page is wide.
//
// Two rows of the issue's own screen: the first requirement, which holds a check, its author the
// page's own words for one nobody recorded; and the open question, its words whole.
//
// It sets the width itself, as every row of this file does: `phase` starts every page at the
// desktop size in every project, the `iphone` project included.
// SLOW(>=2s): the whole issue seeded, then five cold loads at five widths, each read row by row
// and measured for its fit, and a branch folded at each and opened again at two of them.
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
      // Every read of the load settles before the rows are measured: the comments come by reads
      // of their own, and the phone's bullet reading below reads every row drawn.
      const settling = watchApiRequests(page);
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectLoadSettled(settling);
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

      // The first requirement opens unfolded, its words whole; the model holds no author for a
      // requirement, so its row draws none.
      const opened = rowOf(rows, nodeId);
      expect(opened?.expanded, `the first requirement opens folded at ${width} px`).toBe("true");
      expect(
        opened?.firstLine,
        `the first requirement opens cut to one line at ${width} px`
      ).toBeNull();
      expect(
        opened?.author,
        `the first requirement's row draws an author the model does not hold at ${width} px`
      ).toBeNull();
      // No row makes the page scroll sideways at any of these widths.
      await assertPageFits(page);
      if (width === PHONE.width) {
        await expectPhoneBulletGaps(page, specSlug, `the page at ${width} px`);
      }

      // Folded by the reader: its first line, cut at a word, and its count.
      const row = nodeRow(page, nodeId).first();
      await row.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
      await expect(row, `the first requirement does not fold at ${width} px`).toHaveAttribute(
        "aria-expanded",
        "false"
      );
      // The fold is waited on just above, so the row stands folded and its count holds still.
      const required = rowOf(await drawnRows(page), nodeId);
      expectCutAtAWord(
        required,
        requirementText(1),
        `the two-sentence requirement's line at ${width} px`
      );
      // The count reads as the page words it, "row" for one: no result is a row of Conversation
      // (R9), so the requirement holds its one check alone.
      const counted =
        required?.foldedCount === "1" ? CONTROL_LABELS.folded_row : CONTROL_LABELS.folded_rows;
      expect(required?.text, `the folded requirement counts nothing at ${width} px`).toContain(
        `${required?.foldedCount} ${counted}`
      );
      await assertPageFits(page);

      // The phone's form ends at 760 px itself and the desktop's starts at 761. Opened again,
      // the branch draws every level under it as rows, with no line that stands for them.
      if (width !== 760 && width !== 761) continue;
      await row.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
      await expect(row, `the first requirement does not unfold at ${width} px`).toHaveAttribute(
        "aria-expanded",
        "true"
      );
      const unfolded = await drawnRows(page);
      expect(
        heldIn(unfolded, nodeId),
        `the first requirement opened again draws no row under it at ${width} px`
      ).toBeGreaterThan(0);
    }

    // Unfolded again, the row draws the node's whole stored text, and its face says its author
    // is unknown, whatever the focus.
    const unfolded = nodeRow(page, nodeId).first();
    await unfolded.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
    await expect
      .poll(async () => (await rowWords(unfolded).innerText()).replace(/\s+/g, " "), {
        message: "the unfolded row does not draw the node's whole stored text",
      })
      .toContain(requirementText(1));
    await expect(
      rowBody(unfolded).locator("[data-node-author]"),
      "the requirement's face does not say its author is unknown"
    ).toHaveText(CONTROL_LABELS.unknown_author);
  });
});

// c-spine. Each node under the parent the model stores, read against the server's own
// edges on the two seeds that hold today's: an ask over the decision that answers it, `serves`
// over checks and plan tasks, a check over its results, and the entry a reply answers. On the
// issue the conversion has not reached every other typed block sits under the issue.
//
// What waits on another issue, and is not read here: a criterion under the words that raised
// it, a requirement under its criterion, and the left arrow walking up from a requirement to
// them; a lens under its node and a finding under its lens; a reading under its decision; and a
// comment anchored to a node under that node, an edge the model does not store, so the anchored
// comment sits under the issue as any node with no stored parent does.
// SLOW(>=2s): three seeds, each loaded cold with its branches unfolded press by press, and the
// first seed's parents read off the page one whole read at a time.
test("each node sits under the parent the model stores", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedSpineCases, async (page, seeded) => {
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectLoadSettled(settling);
    await expectOpeningLanded(page);
    await unfoldEveryBranch(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const check = blockNodeId(verificationSlug, SPINE_CHECK_BLOCK_ID);

    // The decision that settles the answered ask stands under that ask once the model stores it
    // there: the owed row below reads it.
    // A node with no stored parent stands at the outline's top level, under no row: the issue
    // itself is no row.
    expect(
      await parentOfRow(page, seeded.answeredAskId),
      "the answered ask is not drawn at the outline's top level"
    ).toBeNull();

    // A requirement whose origin is absent sits under the issue, drawn as a node like any other
    // rather than as an error: the gap it draws is the one that tells it apart, that no check
    // serves it.
    const unexplained = blockNodeId(specSlug, SPINE_REQUIREMENTS.unexplained);
    expect(
      await parentOfRow(page, unexplained),
      "the unexplained requirement is not drawn at the outline's top level"
    ).toBeNull();
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
      expect(parent, `${requirement} is not drawn at the outline's top level`).toBeNull();
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
    // A folded branch draws one line and no chip, so the tree is read open. A chip names its
    // target by the target's stored words, cut as a title is (`NodeRow.tsx`'s `Chip`), so each is
    // found by the words the seed stored.
    await unfoldEveryBranch(page);
    const chips = (await drawnRows(page)).flatMap((row) => row.chips);
    expect(
      chipsNaming(chips, seededBlockWords(SPINE_SPEC, SPINE_REQUIREMENTS.shapedElsewhere)).length,
      `the decision's link is not drawn once as a chip: ${JSON.stringify(chips)}`
    ).toBe(1);
    expect(
      chipsNaming(chips, SPINE_ANSWERED_QUESTION),
      "the ask the decision sits under is drawn as its chip as well"
    ).toEqual([]);
  });

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const slugs = new Set(((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.slug));
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    await expectOpeningLanded(page);
    await unfoldEveryBranch(page);
    const rows = await drawnRows(page);
    const parentOf = (id: string) => ancestorsIn(rows, id).at(-1);

    // Each check under the requirement its `serves` names. Conversation draws no result, plan
    // task or message: Graph, which draws the tree read whole, draws each of them under the
    // parent the model stores (below).
    const parentsIn = (drawn: readonly DrawnRow[], id: string) =>
      drawn.flatMap((row, at) => (row.id === id ? [ancestorsIn(drawn, id, at).at(-1)] : []));
    const checks = (await getIssueResults(seeded.key)).checks ?? [];
    for (const check of checks) {
      const id = blockNodeId(verificationSlug, check.block_id);
      const index = Number(check.block_id.slice(1));
      expect(
        parentsIn(rows, id),
        `${check.block_id} is not under the requirement it serves`
      ).toContain(blockNodeId(specSlug, requirementBlockId(index)));
    }
    const graphOnly = [
      ...checks.flatMap((check) => (check.results ?? []).map((result) => result.id)),
      ...seeded.planTasks.map((task) => blockNodeId(planSlug, task)),
      seeded.messages.root,
      seeded.messages.reply,
      seeded.messages.replyToReply,
    ];
    expect(
      rows.filter((row) => graphOnly.includes(row.id ?? "")).map((row) => row.id),
      "Conversation draws a result, a plan task or a message as a row"
    ).toEqual([]);
    seeded.comments.replies.forEach((reply, index) => {
      expect(parentOf(reply), "a comment's reply is not under the comment it answers").toBe(
        seeded.comments.threads[index % seeded.comments.threads.length]
      );
    });

    // Every typed block the model gives no parent sits under the issue - the open question's
    // block and every requirement - and no row sits under a document. The summary callout is
    // the head's three sentences and no row.
    expect(
      rows.some((row) => row.id === blockNodeId(specSlug, seeded.summaryBlockId)),
      "the summary callout is drawn as a row of the outline"
    ).toBe(false);
    for (const id of [
      blockNodeId(specSlug, seeded.openQuestion.blockId),
      ...seeded.requirements.map((requirement) => blockNodeId(specSlug, requirement)),
    ]) {
      expect(
        parentOf(id),
        `${id}, which the model gives no parent, is not at the outline's top level`
      ).toBeUndefined();
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

    // Graph: each result under its check, each plan task under the requirement it serves and
    // the one that serves none at the top level, and each reply under the message it answers.
    const graphSettling = watchApiRequests(page);
    await gotoIssue(page, presetPath(seeded.key, "graph"));
    await expectIssuePageDrawn(page);
    await expectLoadSettled(graphSettling);
    const graph = await drawnRows(page);
    for (const check of checks) {
      const id = blockNodeId(verificationSlug, check.block_id);
      for (const result of check.results ?? []) {
        expect(
          parentsIn(graph, result.id),
          `a result of ${check.block_id} is not under it`
        ).toContain(id);
      }
    }
    seeded.planTasks.forEach((task, index) => {
      const parents = parentsIn(graph, blockNodeId(planSlug, task));
      if (index === seeded.planTasks.length - 1) {
        expect(parents, `${task}, which serves none, is not at the top level of Graph`).toEqual([
          undefined,
        ]);
        return;
      }
      expect(parents, `${task} is not under the requirement it serves`).toContain(
        blockNodeId(specSlug, requirementBlockId(index + 1))
      );
    });
    expect(parentsIn(graph, seeded.messages.reply), "the reply is not under its message").toContain(
      seeded.messages.root
    );
    expect(
      parentsIn(graph, seeded.messages.replyToReply),
      "the reply to a reply is not under the reply it answers"
    ).toContain(seeded.messages.reply);
  });

  // Every level is drawn, every node unfolded as the page opens. The thread of
  // `seedReplyNesting` is four levels deep - its head, the answer to it, the answer to that
  // answer, and a third answer under that - and `phase` opens it at 1440 by 900 px: each level
  // is a row, one level under the entry it answers, with no fold to open first.
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectLoadSettled(settling);
    await expectOpeningLanded(page);
    await expect(nodeRow(page, seeded.head), "the thread's head draws no row").toHaveCount(1);
    await expect(
      nodeRows(page).getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }),
      "a fold control of the thread opens shut"
    ).toHaveCount(0);
    await focusThePage(page);
    const rows = await drawnRows(page);
    const thread = [seeded.head, seeded.first, seeded.second, seeded.third];
    for (const [depth, id] of thread.entries()) {
      const row = rowOf(rows, id);
      expect(row, `${id}, of the thread's level ${depth + 1}, draws no row`).toBeDefined();
      if (depth === 0) continue;
      const above = rowOf(rows, thread[depth - 1] ?? "");
      expect(
        ancestorsIn(rows, id).at(-1),
        `${id}, of the thread's level ${depth + 1}, is not drawn under the entry it answers`
      ).toBe(thread[depth - 1]);
      expect(
        Number(row?.level),
        `${id}, of the thread's level ${depth + 1}, is not one level under the entry it answers`
      ).toBe(Number(above?.level) + 1);
    }
  });
});

// c-spine's decision under the question it settles: the model stores the decision under that
// question, and the page invents no parent, so the row is guarded by `decisionUnderAskGate`
// and stands where the read puts it on a base whose tree read answers no such parent.
// SLOW(>=2s): two seeds, each loaded cold with its branches unfolded press by press.
test("each node sits under the parent the model stores, and a decision stands under the question it settles", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedSpineCases, async (page, seeded) => {
    const tree = treeAnswerOf(page);
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectLoadSettled(settling);
    owedUnless(
      decisionUnderAskGate(
        await heldAnswer(tree, "Conversation's read"),
        seeded.decisionBlockId,
        seeded.answeredAskId
      )
    );
    await unfoldEveryBranch(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    // The decision sits under the ask it settles: the ask its block's `from_ask` names.
    const stored = ((await getIssueDecisions(seeded.key)).entries ?? []).find(
      (one) => one.document?.block_id === seeded.decisionBlockId
    );
    expect(stored?.from_ask, "the server holds no ask the decision answers").toEqual([
      seeded.answeredAskId,
    ]);
    expect(
      await parentOfRow(page, blockNodeId(specSlug, seeded.decisionBlockId)),
      "the decision is not drawn under the ask it answers"
    ).toBe(seeded.answeredAskId);
  });

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const tree = treeAnswerOf(page);
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectLoadSettled(settling);
    const decision = seeded.decisions[1] ?? "";
    owedUnless(
      decisionUnderAskGate(
        await heldAnswer(tree, "Conversation's read"),
        decision,
        seeded.answeredAskId
      )
    );
    await unfoldEveryBranch(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    // The decision that answers the issue's answered ask sits under that ask.
    expect(
      await parentOfRow(page, blockNodeId(specSlug, decision)),
      "the decision that answers the ask is not drawn under it"
    ).toBe(blockNodeId(specSlug, ANSWERED_QUESTION_BLOCK_ID));
  });
});

// c-open-questions. The page opens with the keyboard focus on the first open question that
// waits on the reader, in the outline's order, its row inside the viewport with nothing unfolded
// or scrolled by hand; `q` moves to the next one; and where none waits on the reader the page
// opens with the focus on the outline's first row. The second question is asked after forty
// older nodes, so the scroll is what puts it in the viewport: on a seed whose one question is
// the outline's first row, the row is on the first screen whatever the page does.
// SLOW(>=2s): forty comments and a question written, then ten cold loads at three sizes, of this
// issue and of a second one of one long comment, each waited on until the focus lands.
test("an open question is unfolded and on the first screen @phone", async ({ browser }) => {
  // Ten cold loads at three sizes, eight of them of an issue of some ninety rows.
  test.setTimeout(240_000);
  /** Whether node `id`'s row starts inside what the reader sees: the top of its card under the
   *  band that stays at the top of the screen and above the box at its foot. Only the top is
   *  read, so a row that runs on below the foot still counts. */
  const inView = (page: Page, id: string): Promise<boolean> =>
    nodeRow(page, id)
      .first()
      .evaluate(
        (row, properties) => {
          const style = getComputedStyle(document.documentElement);
          const band = Number.parseFloat(style.getPropertyValue(properties.band)) || 0;
          const foot = Number.parseFloat(style.getPropertyValue(properties.foot)) || 0;
          const card = row.querySelector("[data-node-card]") ?? row;
          const box = card.getBoundingClientRect();
          return box.top >= band - 1 && box.top < window.innerHeight - foot;
        },
        { band: IDENTITY_HEIGHT_PROPERTY, foot: FOOT_HEIGHT_PROPERTY }
      );
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
      await expectOpeningLanded(page);
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
    await expectOpeningLanded(page);
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
      await expectOpeningLanded(page);
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
    // row, at the top, and the load scrolls nothing: at 390 by 664 px too, the phone of the
    // browser suite, where a load once scrolled to 249 px and hid the header.
    await answerAsk(later.id, { expected_edited_at: null, selected: ["Left"] });
    for (const size of [DESKTOP, PHONE, { height: 664, width: 390 }]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expectOpeningLanded(page);
      const rows = await drawnRows(page);
      expect(
        await focusedNodeId(page),
        `the page does not open with the focus on the outline's first row at ${at}`
      ).toBe(rows[0]?.id ?? "");
      expect(await page.evaluate(() => window.scrollY), `the page opens scrolled at ${at}`).toBe(0);
    }

    // A first row taller than the window opens where it stands too, as the architect ruled. The
    // row draws its words whole whatever the focus, and the page's one rule for placing a focused
    // row (`bringOnScreen`) puts a row taller than the screen at its top, under the band: on a
    // long answered question a load once opened at 89 px at 1440 by 900 and at 161 px at 390 by
    // 664, and hid the header. The issue's one node is a person's comment of 120 paragraphs.
    const tall = await createIssue({
      force: true,
      project: PROJECT,
      title: "An issue whose first row is taller than the window",
    });
    const long = await createComment(tall.key, {
      body: Array.from(
        { length: 120 },
        (_, paragraph) => `Paragraph ${paragraph + 1} of words taller than any window.`
      ).join("\n\n"),
    });
    for (const size of [DESKTOP, { height: 664, width: 390 }]) {
      const at = `${size.width}x${size.height} on a first row taller than the window`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${tall.key}`);
      await expectIssuePageDrawn(page);
      await expectOpeningLanded(page);
      await expect
        .poll(() => focusedNodeId(page), {
          message: `the page does not open with the focus on the first row at ${at}`,
        })
        .toBe(long.id);
      expect(
        idsOf(await drawnRows(page))[0],
        `the long comment is not the outline's first row at ${at}`
      ).toBe(long.id);
      const row = nodeRow(page, long.id).first();
      expect(
        await row.evaluate((element) => element.getBoundingClientRect().height),
        `the first row is not taller than the window at ${at}`
      ).toBeGreaterThan(size.height);
      expect(await page.evaluate(() => window.scrollY), `the page opens scrolled at ${at}`).toBe(0);
    }
  });
});

// The tester's finding at #4205's e3136741: the page's opening read as a reader reaches an
// issue, by a link of the dashboard itself and with no document load - from the Inbox, and from
// the parent's children's section. The dashboard moves the keyboard focus to its `<main>` at
// every page change but the first (`app.tsx`), and the outline read that focus as one the reader
// had placed: with a question waiting the page still went to it, and with none it opened with
// the focus on `<main>` and on no row, at 1440 by 900. The row above loads the page by its
// address, where the dashboard moves no focus, so it never met this.
// SLOW(>=2s): a whole seeded issue, then at two sizes a load from the Inbox and one from the
// parent's children's section, each waited on until the focus lands.
test("an issue opened by a link opens with the focus on its question, or on its first row where none waits", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const question = blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID);
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await page.goto("/");
      const link = page
        .locator(`[data-inbox-row] [data-dispatch-ref="dispatch://${seeded.key}"]`)
        .first();
      await expect(link, `the Inbox draws no link to ${seeded.key} at ${at}`).toBeVisible();
      await link.click();
      await expectIssuePageDrawn(page);
      await expectOpeningLanded(page);
      await expect
        .poll(() => focusedNodeId(page), {
          message: `the issue opened from the Inbox does not open with the focus on its open question at ${at}`,
        })
        .toBe(question);
    }

    // With the question answered none waits on the reader, so the focus is the first row's.
    // The Inbox lists the issue no more, and the parent's children's section links it.
    await answerAsk(seeded.openQuestion.askId, {
      expected_edited_at: null,
      selected: [],
      text: "1280 px.",
    });
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.parentKey}`);
      await expectIssuePageDrawn(page);
      const link = childEntry(page, seeded.key).locator(`a[href$="/issues/${seeded.key}"]`).first();
      await expect(
        link,
        `the parent's children's section draws no link to ${seeded.key} at ${at}`
      ).toBeVisible();
      await link.click();
      await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}$`));
      await expectIssuePageDrawn(page);
      await expectOpeningLanded(page);
      expect(
        await focusedNodeId(page),
        `the issue opened from its parent's children's section does not open with the focus on the outline's first row at ${at}`
      ).toBe((await drawnRows(page))[0]?.id ?? "");
    }
  });
});

// The tester's finding at #4205's e3136741, on editing in a node and on "after each move the
// focused row is on screen": Enter draws the statement's document - its one editor, and the
// whole of it - in the row, and the document scrolls the caret's block to the middle of the
// screen. The row holding the editor is still the focused row, and `useRowOnScreen`
// (NodeRow.tsx) places a focused row again on every change of its size: a row taller than the
// screen goes back to its top. So each new line the reader writes, a picture that loads above
// the caret, and another reader's edit take the page to the top of the document, and the caret
// off screen. On a real issue of some 160 rows at 1440 by 900 a picture that loaded during that
// scroll left the caret in its last statement 17,037 px below the top of the window. The seeds'
// statements sit near their document's top and c-keys' editing row writes no new line, so no row
// met it.
// SLOW(>=2s): a whole seeded issue and a long document of its own, a load waited on until the
// statement takes the focus, then Enter and each line written waited on until the caret settles.
test("the caret Enter puts in a statement deep in its document stays on screen while the reader writes", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page) => {
    const deep = await createIssue({
      force: true,
      project: PROJECT,
      spec: [
        "# A spec whose one statement is under eighty paragraphs",
        "",
        ...Array.from({ length: 80 }, (_, n) => [
          `Paragraph ${n + 1} of the words above the statement.`,
          "",
        ]).flat(),
        ":::requirement{#r-deep}",
        "The statement under eighty paragraphs.",
        ":::",
        "",
      ].join("\n"),
      title: "An issue whose one statement is deep in its spec",
    });
    const statement = blockNodeId(await primarySlug(deep.key), "r-deep");
    await gotoIssue(page, `/issues/${deep.key}`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await expect
      .poll(() => focusedNodeId(page), { message: "the page does not open on the statement" })
      .toBe(statement);
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the statement" })
      .toBe(true);
    // On screen is under the band that stays at the top and above the box at the foot: the
    // heights the page publishes for both (`IDENTITY_HEIGHT_PROPERTY`, `FOOT_HEIGHT_PROPERTY`) are
    // taken off the window, as `onScreen` and the open-question row's `inView` do.
    const caretOnScreen = (): Promise<boolean> =>
      page.evaluate(
        ({ band, foot }) => {
          const selection = window.getSelection();
          if (selection === null || selection.rangeCount === 0) return false;
          const style = getComputedStyle(document.documentElement);
          const top = Number.parseFloat(style.getPropertyValue(band)) || 0;
          const bottom =
            window.innerHeight - (Number.parseFloat(style.getPropertyValue(foot)) || 0);
          // A caret on an empty line has no rectangle of its own, so its line's box is read.
          const anchor = selection.anchorNode;
          const line = anchor instanceof Element ? anchor : anchor?.parentElement;
          const box = selection.getRangeAt(0).getClientRects()[0] ?? line?.getBoundingClientRect();
          return box !== undefined && box.top >= top - 1 && box.bottom <= bottom;
        },
        { band: IDENTITY_HEIGHT_PROPERTY, foot: FOOT_HEIGHT_PROPERTY }
      );
    await expect
      .poll(caretOnScreen, {
        message: "the caret Enter put in the statement is off screen at 1440x900",
      })
      .toBe(true);
    // A new line makes the row taller, and nothing else is pressed while it is drawn.
    await page.keyboard.press("End");
    await page.keyboard.press("Enter");
    await nextFrames(page);
    await nextFrames(page);
    expect(
      await caretOnScreen(),
      "a new line written in the statement took the caret off screen at 1440x900"
    ).toBe(true);
  });
});

// The tester's finding at #4205's e3136741, and the architect's ruling of 2026-11-16: while a
// row edits a block of a document the model holds whole, its editor opens at that block with
// the caret there, and the row is no taller than the window, with a scroll of its own, so the
// page around it stays where it was. At e3136741 Enter on the first requirement of a real issue
// of some 160 rows drew the whole spec in that row, 35,635 px high, and the rows under it
// went down by as much. At 861adff1 the editor's box is no taller than the room under it, but
// the row's body stands under the box, so a long statement the reader moved to and edited made a
// row of 971 px at 1440 by 900 on a spec of long statements like this one's, and of 927 px at
// 1440 by 900 and 745 px at 390 by 664 on that real issue, its last lines below the window.
// SLOW(>=2s): a whole seeded issue and two long specs of its own, a load of each waited on until
// the statement takes the focus, then Enter waited on until the editor opens and settles.
test("a row that edits a statement is no taller than the window, and the page around it stays put", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page) => {
    const deep = await createIssue({
      force: true,
      project: PROJECT,
      spec: [
        "# A spec whose one statement is under eighty paragraphs",
        "",
        ...Array.from({ length: 80 }, (_, n) => [
          `Paragraph ${n + 1} of the words above the statement.`,
          "",
        ]).flat(),
        ":::requirement{#r-deep}",
        "The statement under eighty paragraphs.",
        ":::",
        "",
      ].join("\n"),
      title: "An issue whose one statement is deep in its spec",
    });
    const statement = blockNodeId(await primarySlug(deep.key), "r-deep");
    await gotoIssue(page, `/issues/${deep.key}`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await expect
      .poll(() => focusedNodeId(page), { message: "the page does not open on the statement" })
      .toBe(statement);
    const row = nodeRow(page, statement).first();
    const before = await row.evaluate((element) => ({
      scroll: window.scrollY,
      top: element.getBoundingClientRect().top,
    }));
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the statement" })
      .toBe(true);
    await nextFrames(page);
    const after = await row.evaluate((element) => ({
      height: element.getBoundingClientRect().height,
      scroll: window.scrollY,
      top: element.getBoundingClientRect().top,
    }));
    expect(
      after.height,
      "the row that edits the statement is taller than the window at 1440x900"
    ).toBeLessThanOrEqual(DESKTOP.height);
    expect(
      Math.abs(after.scroll - before.scroll) + Math.abs(after.top - before.top),
      "the page around the row that edits the statement moved at 1440x900"
    ).toBeLessThanOrEqual(1);

    // A statement the reader moved to, on a spec of long statements an agent wrote: the row's
    // body - its facts and its controls - stands under the editor's box.
    const long = await createIssue(
      {
        force: true,
        project: PROJECT,
        spec: [
          "# Long statements an agent wrote",
          "",
          ...Array.from({ length: 12 }, (_, index) => [
            `:::requirement{#r-long-${index + 1}}`,
            `Statement ${index + 1} says ${Array.from({ length: 500 }, (_, word) => `word${word}`).join(" ")}.`,
            ":::",
            "",
          ]).flat(),
        ].join("\n"),
        title: "An issue of long statements an agent wrote",
      },
      asPlanner
    );
    const last = blockNodeId(await primarySlug(long.key), "r-long-12");
    await gotoIssue(page, `/issues/${long.key}`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await page.keyboard.press("End");
    await expect
      .poll(() => focusedRow(page), { message: "End did not reach the last statement" })
      .toBe(last);
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the statement" })
      .toBe(true);
    await expect
      .poll(
        () =>
          nodeRow(page, last)
            .first()
            .evaluate((element) => element.getBoundingClientRect().height),
        { message: "the row that edits a statement the reader moved to is taller than the window" }
      )
      .toBeLessThanOrEqual(DESKTOP.height);
  });
});

// The architect's ruling of 2026-11-18 on the width of a row's editor: its box never scrolls
// sideways. Prose wraps, a run with no space in it too, and a wide block scrolls sideways inside
// itself alone, as the document's reading view draws it. On a real spec at 390 by 664, Enter on a
// plain statement opened a box whose words were 414 px wide in a 320 px one. Read in this row's
// 350 px box before the fix, two things were wider than it. A reference the editor draws as its
// issue's title is one line of up to 24em, 384 px, that nothing held to its line, and in a bullet
// it starts 30 px in: 414 px. A note's title is an item of the note's header, which held this
// path, a run with no space in it, at its whole width, 862 px, since the editor's `break-word`
// gives the run no place to break while the header sizes it. No seed cites an issue in a bullet
// or titles a block with such a run, and no row read the editor at a phone's width, so no row met
// it.
// SLOW(>=2s): a seeded issue, an issue of another project and a spec citing it, a load waited on
// until the focus lands, then End and Enter, waited on until the editor draws the cited issue's
// name.
test("a row's editor on a phone does not scroll sideways, and its words wrap @phone @phone-only", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page) => {
    await page.setViewportSize({ height: 664, width: 390 });
    // The cited issue is another project's, so its name is drawn once the editor's read of it
    // answers, as in the row of the names a document cites.
    const cited = await seedCitedIssue();
    const spec = await createIssue({
      force: true,
      project: PROJECT,
      spec: [
        "# A plain statement under a cited issue and a path",
        "",
        "## Problem",
        "",
        `- dispatch://${cited}`,
        "",
        `:::callout{#n-path kind="note" title="${UNBROKEN_PATH}"}`,
        "The note's own words.",
        ":::",
        "",
        "## Requirements",
        "",
        ":::requirement{#r-plain}",
        "A plain statement.",
        ":::",
        "",
      ].join("\n"),
      title: "An issue whose plain statement stands under wide words",
    });
    const statement = blockNodeId(await primarySlug(spec.key), "r-plain");
    await enterNode(page, spec.key, statement, "the statement", { enter: true });
    const box = nodeRow(page, statement).first().locator("[data-editor-box]");
    await expect(
      box.locator("a.dispatch-reference"),
      "the row's editor draws no name for the cited issue"
    ).toBeVisible();
    await nextFrames(page);
    const drawn = await box.evaluate((element) => {
      const title = element.querySelector("[data-proof-block-title]");
      return {
        clientWidth: element.clientWidth,
        scrollWidth: element.scrollWidth,
        // How many lines the note's title is drawn on: its height over its own line's.
        titleLines:
          title === null
            ? 0
            : title.getBoundingClientRect().height /
              Number.parseFloat(getComputedStyle(title).lineHeight),
      };
    });
    expect(
      drawn.scrollWidth,
      "the editor's box of a plain statement scrolls sideways at 390x664"
    ).toBeLessThanOrEqual(drawn.clientWidth);
    expect(
      drawn.titleLines,
      "the note's title, a path with no space in it, is not wrapped at 390x664"
    ).toBeGreaterThan(1.5);
  });
});

// The same ruling's other half: a wide block the node itself holds is still whole by its own
// scroll, so a box held to its width cuts none of it. A note is the block that holds a table - a
// statement holds paragraphs alone (`pmdoc/schema/blocks.json`) - and this one names an issue too,
// whose title is what widened the box at f5d49c35, while the table already scrolled inside itself.
// A note of the spec is no kind Conversation's read holds, so the row edits it in Graph, which
// draws the whole tree read and keeps the editing every outline has.
// SLOW(>=2s): a seeded issue, an issue of another project and a spec citing it, a load waited on
// until the focus lands, then End and Enter, waited on until the editor draws the cited issue's
// name, a scroll of the editor's box waited on until it stops with the table's middle on screen,
// and a scroll of the table waited on until its last column shows.
test("a table a row's editor holds scrolls sideways inside itself, and the box around it does not @phone @phone-only", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page) => {
    await page.setViewportSize({ height: 664, width: 390 });
    const cited = await seedCitedIssue();
    const columns = Array.from({ length: 16 }, (_, index) => index + 1);
    const spec = await createIssue({
      force: true,
      project: PROJECT,
      spec: [
        "# A note that holds a table wider than a phone",
        "",
        // The spec's first callout is its summary, which the outline draws as no row of its own
        // (`spec-summary.ts`), so the note is the second.
        ':::callout{#summary kind="note"}',
        "What the issue is about, in a sentence.",
        ":::",
        "",
        ':::callout{#n-table kind="note"}',
        `The note names dispatch://${cited} and lists what it found in sixteen columns.`,
        "",
        `| ${columns.map((column) => `column ${column}`).join(" | ")} |`,
        `| ${columns.map(() => "---").join(" | ")} |`,
        `| ${columns.map((column) => `cell ${column}`).join(" | ")} |`,
        ":::",
        "",
      ].join("\n"),
      title: "An issue whose note holds a table wider than a phone",
    });
    const note = blockNodeId(await primarySlug(spec.key), "n-table");
    await enterNode(page, spec.key, note, "the note", {
      at: presetPath(spec.key, "graph"),
      enter: true,
    });
    const box = nodeRow(page, note).first().locator("[data-editor-box]");
    await expect(
      box.locator("a.dispatch-reference"),
      "the row's editor draws no name for the cited issue"
    ).toBeVisible();
    const table = box.locator("table");
    await expect(table, "the row's editor draws no table in the note").toHaveCount(1);
    await nextFrames(page);
    /** The box's and the table's widths, and whether the table's last column is inside what the
     *  table shows. */
    const widths = () =>
      box.evaluate((element) => {
        const drawn = element.querySelector("table");
        const shown = drawn?.getBoundingClientRect();
        const last = drawn?.querySelector("tr")?.lastElementChild?.getBoundingClientRect();
        return {
          box: {
            client: element.clientWidth,
            left: element.scrollLeft,
            scroll: element.scrollWidth,
          },
          lastShown:
            shown !== undefined &&
            last !== undefined &&
            last.left >= shown.left - 0.5 &&
            last.right <= shown.right + 0.5,
          table: { client: drawn?.clientWidth ?? 0, scroll: drawn?.scrollWidth ?? 0 },
        };
      });
    const before = await widths();
    expect(
      before.box.scroll,
      "the editor's box of a note that holds a table scrolls sideways at 390x664"
    ).toBeLessThanOrEqual(before.box.client);
    expect(
      before.table.scroll,
      "the table is no wider than what it shows, so it does not scroll inside itself"
    ).toBeGreaterThan(before.table.client);
    expect(before.lastShown, "the table shows its last column before it is scrolled").toBe(false);
    // A reader's sideways scroll over the table, as a trackpad makes it, with the table in the
    // middle of the editor's box: the box to write in stands over the screen's last lines, and a
    // wheel over it is the box's, not the table's. The reader brings the table there with the
    // wheel too, over the editor's box, which scrolls inside itself. A script's scroll does not do
    // it: once the editor's drawing settles, the row puts the node's block at the box's top again
    // (`refocusWhenSettled`, 200 ms after the cited issue's name is drawn), which a reader's wheel
    // stops and a script's scroll does not. On a slow machine that second placing undid the
    // script's scroll before the sideways wheel reached the table (CI at db240615).
    const held = await box.boundingBox();
    const placed = await table.boundingBox();
    expect(held, "the row's editor has no box on the page").not.toBeNull();
    expect(placed, "the table has no box on the page").not.toBeNull();
    const boxMiddle = {
      x: (held?.x ?? 0) + (held?.width ?? 0) / 2,
      y: (held?.y ?? 0) + (held?.height ?? 0) / 2,
    };
    expect(
      await page.evaluate(
        ({ x, y }) => document.elementFromPoint(x, y)?.closest("[data-editor-box]") != null,
        boxMiddle
      ),
      "the middle of the row's editor box is under something else the page draws"
    ).toBe(true);
    await page.mouse.move(boxMiddle.x, boxMiddle.y);
    await page.mouse.wheel(0, (placed?.y ?? 0) + (placed?.height ?? 0) / 2 - boxMiddle.y);
    /** The middle of the table where the page draws it, and whether the table is what the page
     *  draws there. */
    const tableMiddle = () =>
      table.evaluate((drawn) => {
        const at = drawn.getBoundingClientRect();
        const x = at.left + at.width / 2;
        const y = at.top + at.height / 2;
        return { onTop: document.elementFromPoint(x, y)?.closest("table") === drawn, x, y };
      });
    // The box scrolls smoothly, so the table's middle is read until two reads find it in the
    // same place, with the table drawn there.
    let last: { readonly x: number; readonly y: number } | undefined;
    await expect
      .poll(
        async () => {
          const now = await tableMiddle();
          const still = last !== undefined && now.x === last.x && now.y === last.y;
          last = now;
          return still && now.onTop;
        },
        { message: "the middle of the table is under something else the page draws" }
      )
      .toBe(true);
    const point = await tableMiddle();
    await page.mouse.move(point.x, point.y);
    await page.mouse.wheel(before.table.scroll, 0);
    await expect
      .poll(async () => (await widths()).lastShown, {
        message: "a sideways scroll over the table does not reach its last column",
      })
      .toBe(true);
    expect(
      (await widths()).box.left,
      "a sideways scroll over the table scrolled the editor's box"
    ).toBe(0);
  });
});

// The editor's own part of the same ruling: a collaborator's caret label hung from the caret
// toward the line's end, so with the caret near a line's end it passed the end of the box and the
// box scrolled sideways while the caret stood there. Measured at 390 by 664 with a caret drawn
// as the editor draws one, 1 px inside the box's end, the label ran 195 px past it and the box's
// scroll width went from 350 to 545 px. The editor now hangs such a label back from the caret
// (`collab-cursor-plugin.ts`). The row runs at 1280 px with a real second reader, because below
// 1280 px `ProofDocument.tsx` gives the editor no awareness and a phone draws no collaborator's
// caret at all. The statement is one run with no space in it, which the box breaks at its width,
// so its first line runs to within a character of the box's width, and bob's caret stands one
// character before that line's end.
// SLOW(>=2s): a seeded issue and a spec, two readers' pages each loaded and waited on until its
// caret is in the statement, then alice's waited on until it draws bob's caret.
test("a collaborator's caret label near a line's end hangs back inside the row's editor box", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page) => {
    await page.setViewportSize({ height: 900, width: 1280 });
    const spec = await createIssue({
      force: true,
      project: PROJECT,
      spec: [
        "# A statement two readers edit at once",
        "",
        "## Requirements",
        "",
        ":::requirement{#r-plain}",
        "Tworeaderseditthisstatementwhoselinerunstotheendofthebox".repeat(6),
        ":::",
        "",
      ].join("\n"),
      title: "An issue whose statement two readers edit",
    });
    const statement = blockNodeId(await primarySlug(spec.key), "r-plain");
    await enterNode(page, spec.key, statement, "the statement on alice's page", { enter: true });
    const bob = await asUser(browser, "bob");
    try {
      await withIssuePage(bob, "bob");
      const bobPage = await bob.newPage();
      await bobPage.setViewportSize({ height: 900, width: 1280 });
      await enterNode(bobPage, spec.key, statement, "the statement on bob's page", { enter: true });
      // End takes bob's caret to the end of the statement's first line, where the line breaks,
      // and the step back keeps it on that line.
      await bobPage.keyboard.press("End");
      await bobPage.keyboard.press("ArrowLeft");
      const label = nodeRow(page, statement)
        .first()
        .locator("[data-editor-box]")
        .locator(cursorLabel(page, "bob"));
      await expect(label, "alice's row draws no caret of bob's").toBeVisible();
      await nextFrames(page);
      const drawn = await label.evaluate((element) => {
        const box = element.closest("[data-editor-box]");
        if (box === null) throw new Error("bob's caret label stands in no editor box");
        const edges = ({ bottom, left, right, top }: DOMRect) => ({ bottom, left, right, top });
        return {
          box: {
            ...edges(box.getBoundingClientRect()),
            clientWidth: box.clientWidth,
            scrollWidth: box.scrollWidth,
          },
          label: {
            ...edges(element.getBoundingClientRect()),
            clientWidth: element.clientWidth,
            scrollWidth: element.scrollWidth,
            text: element.textContent,
          },
        };
      });
      expect(
        drawn.box.scrollWidth,
        "alice's editor box scrolls sideways while bob's caret stands near a line's end"
      ).toBeLessThanOrEqual(drawn.box.clientWidth);
      expect(
        {
          bottom: drawn.label.bottom <= drawn.box.bottom,
          left: drawn.label.left >= drawn.box.left,
          right: drawn.label.right <= drawn.box.right,
          top: drawn.label.top >= drawn.box.top,
        },
        `bob's label (${JSON.stringify(drawn.label)}) passes an edge of alice's editor box (${JSON.stringify(drawn.box)})`
      ).toEqual({ bottom: true, left: true, right: true, top: true });
      expect(drawn.label.text, "bob's label does not read his name").toBe("bob");
      expect(drawn.label.scrollWidth, "bob's label cuts his name short").toBeLessThanOrEqual(
        drawn.label.clientWidth
      );
    } finally {
      await bob.close();
    }
  });
});

// The tester's finding at #4205's 861adff1, on "after each move the focused row is on screen":
// `.` zooms into a row of the outline's top level, which draws that row alone at the top of the
// page, and `,` draws the whole outline again with the focus still on that row, and nothing
// places it: the row keeps the focus it held, so `useRowOnScreen` (NodeRow.tsx) has no new focus
// to answer, and a zoom back to the whole issue places nothing of its own. At 1440 by 900, on a
// real issue of some 160 rows and on this seed with forty more comments, the row stood 2,000 to
// 40,000 px below the window until the next key. The key row presses `,` on a row the page then
// draws on the first screen, so no row met it.
// SLOW(>=2s): a whole seeded issue and forty comments, a load waited on until the focus lands,
// then End, the zoom in and the zoom out, each waited on until the row settles.
test("zooming back out of a top-level row keeps that row on screen", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // Forty rows of the outline's top level under what the seed draws, so the last of them is
    // far below the first screen once the whole outline is drawn again.
    for (let older = 1; older <= 40; older += 1) {
      await createComment(seeded.key, { body: `Older words, number ${older}.` });
    }
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await page.keyboard.press("End");
    await expectFocusedRowOnScreen(page, "End", "left the last row off screen");
    const last = await focusedRow(page);
    await page.keyboard.press(".");
    await expect
      .poll(async () => (await visibleIds(page))[0], {
        message: "the zoom key did not make the last row the tree's root",
      })
      .toBe(last);
    await page.keyboard.press(",");
    expect(await focusedRow(page), "the key back out moved the focus off the row").toBe(last);
    await expectFocusedRowOnScreen(
      page,
      "the key back out of a top-level row",
      "left that row off screen at 1440x900"
    );
  });
});

// The tester's finding at #4205's 861adff1, on editing in a node and "after each move the
// focused row is on screen": the row's editor holds the statement's whole document in a box of
// its own scroll, and the words above the statement in that document cite issues of another
// project, whose names the editor draws only once its reads of them answer - after the caret is
// in. The words above grow, the box keeps the caret's block in place by scrolling itself, and the
// window, which anchors its own scroll on what the box draws, scrolls by as much: the caret went
// under the band at the top, at -43 px at 1440 by 900 and at -97 px at 390 by 664, and on a real
// issue of some 160 rows the whole editing row stood above the window. The seeds' documents cite
// nothing outside the issue's own project above a statement, so no row met it.
// SLOW(>=2s): a whole seeded issue, sixty issues of another project and a long spec citing them,
// a load waited on until the focus lands, then End and Enter, waited on until the editor's reads
// of the names have answered.
test("the caret Enter puts in a statement stays on screen while the editor draws the names its document cites", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page) => {
    // The cited issues are another project's, so nothing the page reads before the editor opens
    // has their names in hand.
    await createProject({ key: "CITED", name: "Cited issues" });
    const cited: string[] = [];
    for (let number = 1; number <= 60; number += 1) {
      const issue = await createIssue({
        force: true,
        project: "CITED",
        title: `A cited issue of another project, number ${number}, whose title runs long`,
      });
      cited.push(issue.key);
    }
    const spec = await createIssue(
      {
        force: true,
        project: PROJECT,
        spec: [
          "# Long statements under words that cite another project",
          "",
          "## Problem",
          "",
          ...Array.from({ length: 6 }, (_, paragraph) => [
            `Paragraph ${paragraph + 1} cites ${cited
              .slice(paragraph * 10, paragraph * 10 + 10)
              .map((key) => `dispatch://${key}`)
              .join(", ")}.`,
            "",
          ]).flat(),
          "## Requirements",
          "",
          ...Array.from({ length: 12 }, (_, index) => [
            `:::requirement{#r-cited-${index + 1}}`,
            `Statement ${index + 1} says ${Array.from({ length: 500 }, (_, word) => `word${word}`).join(" ")}.`,
            ":::",
            "",
          ]).flat(),
        ].join("\n"),
        title: "An issue whose statements stand under words citing another project",
      },
      asPlanner
    );
    const last = blockNodeId(await primarySlug(spec.key), "r-cited-12");
    await gotoIssue(page, `/issues/${spec.key}`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await page.keyboard.press("End");
    await expect
      .poll(() => focusedRow(page), { message: "End did not reach the last statement" })
      .toBe(last);
    const reads = watchApiRequests(page);
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the statement" })
      .toBe(true);
    // Every read the editor makes of the names it draws has answered, so the words above the
    // caret have their last size.
    await expectLoadSettled(reads);
    await nextFrames(page);
    expect(
      await page.evaluate(
        ({ band, foot }) => {
          const selection = window.getSelection();
          if (selection === null || selection.rangeCount === 0) return false;
          const style = getComputedStyle(document.documentElement);
          const top = Number.parseFloat(style.getPropertyValue(band)) || 0;
          const bottom =
            window.innerHeight - (Number.parseFloat(style.getPropertyValue(foot)) || 0);
          // A caret on an empty line has no rectangle of its own, so its line's box is read.
          const anchor = selection.anchorNode;
          const line = anchor instanceof Element ? anchor : anchor?.parentElement;
          const box = selection.getRangeAt(0).getClientRects()[0] ?? line?.getBoundingClientRect();
          return box !== undefined && box.top >= top - 1 && box.bottom <= bottom;
        },
        { band: IDENTITY_HEIGHT_PROPERTY, foot: FOOT_HEIGHT_PROPERTY }
      ),
      "the caret went off screen while the editor drew the names its document cites at 1440x900"
    ).toBe(true);
  });
});

// The box a row's editor scrolls in anchors its own scroll on the words it holds, and the window
// anchors nothing inside it: `EditorBox` (StagedDocument.tsx) puts `overflow-anchor: none` on an
// element around the box, not on the box. The editor draws the names of the issues a document
// cites after the caret is in, so the words above the caret grow, and the box, anchored on the
// words the caret is in, scrolls by as much in the same frame, so those words stay where they
// stand. With the property on the box itself, the box anchored nothing: at 1440 by 900 the caret
// went from 111 px down the window to 136 px, 443 px and then 597 px, for some 600 ms, until the
// row's second look at the block (`holdEditorAtBlock`) put it back. The row above reads the caret
// once, after that second look, so it passed with the property on either element.
// SLOW(>=2s): a whole seeded issue, sixty issues of another project and a long spec citing them,
// a load waited on until the focus lands, then End and Enter, and the caret read on every frame
// for two seconds from the frame it is in the statement.
test("the words the caret Enter puts in a statement stand still while the editor draws the names above them", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page) => {
    // The cited issues are another project's, so nothing the page reads before the editor opens
    // has their names in hand, and the editor draws them after the caret is in.
    await createProject({ key: "CITED", name: "Cited issues" });
    const cited: string[] = [];
    for (let number = 1; number <= 60; number += 1) {
      const issue = await createIssue({
        force: true,
        project: "CITED",
        title: `A cited issue of another project, number ${number}, whose title runs long`,
      });
      cited.push(issue.key);
    }
    const spec = await createIssue(
      {
        force: true,
        project: PROJECT,
        spec: [
          "# Long statements under words that cite another project",
          "",
          "## Problem",
          "",
          ...Array.from({ length: 6 }, (_, paragraph) => [
            `Paragraph ${paragraph + 1} cites ${cited
              .slice(paragraph * 10, paragraph * 10 + 10)
              .map((key) => `dispatch://${key}`)
              .join(", ")}.`,
            "",
          ]).flat(),
          "## Requirements",
          "",
          ...Array.from({ length: 12 }, (_, index) => [
            `:::requirement{#r-cited-${index + 1}}`,
            `Statement ${index + 1} says ${Array.from({ length: 500 }, (_, word) => `word${word}`).join(" ")}.`,
            ":::",
            "",
          ]).flat(),
        ].join("\n"),
        title: "An issue whose statements stand under words citing another project",
      },
      asPlanner
    );
    const statement = "r-cited-12";
    const last = blockNodeId(await primarySlug(spec.key), statement);
    await enterNode(page, spec.key, last, "the last statement", { enter: false });
    /** Where the caret stood at one frame, `at` ms after the first frame it was in the
     *  statement: its top down the window, and the window's scroll, in px. */
    type CaretReading = { readonly at: number; readonly scroll: number; readonly top: number };
    // The claim is where the caret stands over time, so the page reads it on every frame, from
    // the first frame the caret is in the statement until two seconds later, with no key pressed
    // between: a reading at one instant passes before the names draw or after the row's second
    // look has put the block back. The reading is armed before Enter, so it misses no frame.
    const sampler = await page.evaluateHandle(
      ({ block, span }) => {
        const done = Promise.withResolvers<{
          readonly first: CaretReading;
          readonly lineHeight: number;
          readonly readings: readonly CaretReading[];
        }>();
        const readings: CaretReading[] = [];
        let start: number | undefined;
        let lineHeight = 0;
        const frame = (now: number) => {
          const selection = window.getSelection();
          const anchor = selection?.anchorNode ?? null;
          if (start === undefined) {
            const editable =
              document.activeElement?.closest('[data-node-row] [contenteditable="true"]') ?? null;
            const drawn = editable?.querySelector(`[data-block-id="${block}"]`) ?? null;
            if (editable === null || anchor === null || drawn?.contains(anchor) !== true) {
              requestAnimationFrame(frame);
              return;
            }
            start = now;
            // One line of the editor, as `holdEditorAtBlock` measures it: its computed line
            // height, or CSS's `normal` of about 1.2 times its font size where it keeps the word.
            const style = getComputedStyle(editable);
            const height = Number.parseFloat(style.lineHeight);
            lineHeight = Number.isFinite(height) ? height : 1.2 * Number.parseFloat(style.fontSize);
          }
          // A caret on an empty line has no rectangle of its own, so its line's box is read.
          const line = anchor instanceof Element ? anchor : anchor?.parentElement;
          const box =
            (selection !== null && selection.rangeCount > 0
              ? selection.getRangeAt(0).getClientRects()[0]
              : undefined) ?? line?.getBoundingClientRect();
          const reading = {
            at: Math.round(now - start),
            scroll: window.scrollY,
            top: box?.top ?? Number.NaN,
          };
          readings.push(reading);
          if (now - start < span) {
            requestAnimationFrame(frame);
            return;
          }
          done.resolve({ first: readings[0] ?? reading, lineHeight, readings });
        };
        requestAnimationFrame(frame);
        return { done: done.promise };
      },
      { block: statement, span: 2_000 }
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInRowBlock(page, statement), {
        message: "Enter did not put the caret in the statement",
      })
      .toBe(true);
    const { first, lineHeight, readings } = await sampler.evaluate((armed) => armed.done);
    // Each place the caret stood in turn, from the first: the path a failure names.
    const path = readings
      .filter(
        (reading, index) =>
          Math.round(reading.top) !== Math.round(readings[index - 1]?.top ?? Number.NaN)
      )
      .map((reading) => `${Math.round(reading.top)} px at ${reading.at} ms`);
    expect(
      Math.max(...readings.map((reading) => Math.abs(reading.top - first.top))),
      `the caret stood ${path.join(", ")} down the window after it was in the statement, more than one line of the editor (${lineHeight} px) from where it went in, while the editor drew the names above it at 1440x900`
    ).toBeLessThanOrEqual(lineHeight);
    const scrolled = Math.max(
      ...readings.map((reading) => Math.abs(reading.scroll - first.scroll))
    );
    expect(
      scrolled,
      `the window scrolled by ${scrolled} px while the editor drew the names above the caret at 1440x900`
    ).toBe(0);
  });
});

// c-children, with the children's section above the outline: Conversation, each filter and
// Graph draw the section once, above the outline; it lists each child with its key, a link to
// its own page, its title, its status, what blocks it and who holds it; the outline still draws
// each child as a row where the tree places it, which for every child today is under the issue,
// with its key, its title and its status, its tag opening the child in the section; and
// `/children` and `t h` move the focus to the section's control. The header's bar and counts
// are the header's own to read.
// SLOW(>=2s): two seeds and twenty-five cold loads - each view's address at two sizes, each
// waited out until its reads settle - and the children's keys pressed at two sizes.
test("the children are a section above the outline, and each child is a row of the outline where the tree places it @phone", async ({
  browser,
}) => {
  // Two seeds and twenty-five cold loads of the issue page.
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
        // A view's body draws rows of its own shape, so the page is waited for until it has
        // stopped reading rather than for a row.
        const traffic = watchApiRequests(page);
        await gotoIssue(page, presetPath(seeded.key, preset));
        await expectLoadSettled(traffic);
        await expect(
          childrenSection(page),
          `the ${PRESET_NAMES[preset]} view does not draw the children's section once at ${size.width} px`
        ).toHaveCount(1);
        expect(
          await page.evaluate(() => {
            const foot = document
              .querySelector("[data-children-section]")
              ?.getBoundingClientRect().bottom;
            return [...document.querySelectorAll("[data-issue-page] [data-node-row]")].filter(
              (row) => foot === undefined || row.getBoundingClientRect().top < foot
            ).length;
          }),
          `a row of the ${PRESET_NAMES[preset]} view stands above the children's section's foot at ${size.width} px`
        ).toBe(0);
        if (preset !== "conversation") continue;
        await expectIssuePageDrawn(page);

        // With no filter on, each child is also one row of the outline, under no other row: the
        // issue itself is no row.
        await expect(
          childRows(page),
          `the outline draws other than one row a child at ${size.width} px`
        ).toHaveCount(seeded.children.keys.length);
        const rows = await drawnRows(page);
        for (const key of seeded.children.keys) {
          expect(
            ancestorsIn(rows, key),
            `the child ${key} sits under another row at ${size.width} px`
          ).toEqual([]);
        }
      }
    }

    // What a child's row holds: its title as its words, how it stands, and its key as the tag,
    // which opens the child in the section. What the section lists of it: its key as a link to
    // the child's own page, its title, how it stands, what blocks it and who holds it.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    // no opening focus: this load reads the children's rows, and the next read of the focus comes
    // after the next load, below.
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
      const entry = childEntry(page, key);
      await expect(entry, `the section lists ${key} other than once`).toHaveCount(1);
      await expect(entry, `the section does not list the title of ${key}`).toContainText(
        child.title
      );
      expect(
        await entry.locator("[data-child-key]").getAttribute("href"),
        `the section's key of ${key} opens another page`
      ).toBe(`/issues/${key}`);
      // Every colour is the page's own palette: the old application's link blue is gone.
      for (const links of [row.locator("a"), entry.locator("a")]) {
        expect(
          await links.evaluateAll((all) => all.map((link) => getComputedStyle(link).color)),
          `a link drawn for ${key} is in the old application's blue`
        ).not.toContain("rgb(0, 105, 168)");
      }
    }
    expect(
      (await childEntry(page, blocked).innerText()).replace(/\s+/g, " "),
      "the section does not name the issue that blocks the blocked child"
    ).toContain(`Blocked by ${running}`);
    expect(
      (await childEntry(page, running).innerText()).replace(/\s+/g, " "),
      "the section does not say who holds the running child"
    ).toMatch(/Claimed by \S/);
    // A child's tag on its row opens the child in the section, with the focus on its key there.
    await childRow(page, blocked).first().locator("[data-child-key]").click();
    await expect(
      childEntry(page, blocked).locator("[data-child-key]"),
      "a child's tag on its row does not open the child in the section"
    ).toBeFocused();

    // The children's own address, and `t h`, put the focus on the section's control, on screen,
    // each time: again after a move, and from a zoom that holds no child.
    const [, secondChild = ""] = seeded.children.keys;
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width} px`;
      const toChildren = async (from: string) => {
        await page.keyboard.press("t");
        await page.keyboard.press("h");
        await expect(
          childrenToggle(page),
          `t h ${from} does not focus the children's section at ${at}`
        ).toBeFocused();
      };
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}/children`);
      await expect(
        childrenToggle(page),
        `/children does not focus the children's section at ${at}`
      ).toBeFocused();
      expect(await onScreen(childrenToggle(page)), `/children leaves it off screen at ${at}`).toBe(
        true
      );

      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectOpeningLanded(page);
      await expect(childRows(page)).toHaveCount(seeded.children.keys.length);
      await toChildren("from the issue's address");
      await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/children$`));
      await expect
        .poll(() => onScreen(childrenToggle(page)), {
          message: `t h leaves the children's section off screen at ${at}`,
        })
        .toBe(true);
      await childRow(page, secondChild).first().focus();
      await page.keyboard.press("j");
      await toChildren("a second time, after j");
      await childRow(page, secondChild).first().focus();
      await page.keyboard.press(".");
      await expect(
        childRows(page),
        `. on the second child still draws another child at ${at}`
      ).toHaveCount(1);
      await toChildren("from a zoom into the second child");
    }
  });

  await phase(browser, seedNoChildren, async (page, seeded) => {
    for (const preset of PRESET_ORDER) {
      await gotoIssue(page, presetPath(seeded.key, preset));
      if (preset === "conversation") await expectIssuePageDrawn(page);
      expect(
        await childrenSection(page).count(),
        `an issue with no children draws a children section with the ${PRESET_NAMES[preset]} view`
      ).toBe(0);
      expect(
        await childRows(page).count(),
        `an issue with no children draws a child's row with the ${PRESET_NAMES[preset]} view`
      ).toBe(0);
    }

    // With no child, `t h` has nowhere to go: the Keys list greys its line rather than hiding
    // it, and a press moves no focus and reports nothing at the row.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(outlineRows(page).first(), "the issue draws no row").toBeVisible();
    await expectOpeningLanded(page);
    await focusThePage(page);
    await filterRow(page).getByRole("button", { exact: true, name: CONTROL_LABELS.keys }).click();
    const keys = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(keys).toBeVisible();
    expect(
      await keysListLines(page, "t h"),
      "the Keys list does not draw t h as disabled on an issue with no children"
    ).toEqual(["false"]);
    await page.keyboard.press("Escape");
    await expect(keys).toBeHidden();
    await expect(tabStopRow(page)).toBeFocused();
    const before = await focusedNodeId(page);
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    await nextFrames(page);
    expect(await focusedNodeId(page), "t h moved the focus on an issue with no children").toBe(
      before
    );
    expect(
      await pageStatus(page).count(),
      "t h reported something on an issue with no children"
    ).toBe(0);
  });
});

// The stage's choice: a view is drawn in place of the outline, never under or beside it, and
// Conversation draws the outline again, with the focus on its rows. Each view's own body is its
// own slot's; until that slot is filled, its view draws nothing, and the outline is gone all
// the same.
// SLOW(>=2s): the whole issue seeded and loaded, then four views turned on and off, the outline
// waited for after each, two chords pressed and the list of keys opened.
test("a view replaces the outline, and the outline comes back", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);

    // Graph uses the outline's own rows, but adds the plan tasks the Conversation read leaves
    // out. The other views draw their own trees, with none of the outline's row marks.
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const graphTask = blockNodeId(planSlug, seeded.planTasks.at(-1) ?? "");
    const graphRow = outlineRows(page).and(nodeRow(page, graphTask));
    for (const view of PRESET_ORDER.filter((one) => one !== "conversation")) {
      await presetToggle(page, view).click();
      await expect(page).toHaveURL(new RegExp(`${presetPath(seeded.key, view)}$`));
      if (view === "graph") {
        await expect(graphRow, "Graph draws no outline row for the plan task").toHaveCount(1);
      } else {
        await expect(
          outlineRows(page),
          `the ${PRESET_NAMES[view]} view draws the outline`
        ).toHaveCount(0);
      }
      await presetToggle(page, "conversation").click();
      await expect
        .poll(() => outlineRows(page).count(), {
          message: `${PRESET_NAMES.conversation} does not draw the outline again after ${PRESET_NAMES[view]}`,
        })
        .toBeGreaterThan(0);
      await expect(graphRow, "Conversation draws the plan task").toHaveCount(0);
    }

    // The chords: `t s` turns Spec on, and `t c` turns every view off and leaves the focus on a
    // row of the outline.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(page).toHaveURL(new RegExp(`${presetPath(seeded.key, "spec")}$`));
    await expect(outlineRows(page), "t s left the outline drawn under the Spec view").toHaveCount(
      0
    );
    await page.keyboard.press("t");
    await page.keyboard.press("c");
    await expect
      .poll(() => outlineRows(page).count(), { message: "t c did not draw the outline again" })
      .toBeGreaterThan(0);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "t s then t c did not leave the focus on a row of the outline",
      })
      .not.toBeNull();

    // Keys puts the focus on the outline's tab-stop row before it opens the list of keys, so the
    // list names the outline's keys and closing it gives the reader that row.
    await focusThePage(page);
    await filterRow(page).getByRole("button", { exact: true, name: CONTROL_LABELS.keys }).click();
    const keys = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(keys).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(keys).toBeHidden();
    await expect(
      tabStopRow(page),
      "Keys did not leave the focus on the outline's tab-stop row"
    ).toBeFocused();
  });
});

// Reading a thread on the issue page marks its Inbox row read, as the panel's list of the
// conversation did on its own address: the row of the thread's newest reply, on screen for a
// second, marks the thread read through that reply (`outline-thread-read.tsx`). The outline
// opens with every node unfolded, the thread among them, so the reply's row is drawn as the page
// opens and the thread is read once it has been on screen.
// SLOW(>=2s): a seeded thread and a reply, the Inbox polled, a load settled, and the reply's row
// waited on for the dwell that marks it read.
test("a thread read on the outline is read in the Inbox", async ({ browser }) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    // An agent replies in alice's thread, which makes the thread unread in her Inbox.
    const answer = await createComment(
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

    // The page opens at 1440 by 900 with the thread unfolded, as every node opens. The load is
    // let settle first, so the tree read here holds every node the reads draw.
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectLoadSettled(settling);
    await expectOpeningLanded(page);
    await expect(nodeRow(page, seeded.head), "the thread's head draws no row").toHaveCount(1);
    expect(
      rowOf(await drawnRows(page), seeded.head)?.expanded,
      "the thread is folded as the page opens"
    ).toBe("true");

    // With its newest write's row on screen, the thread is read.
    const newest = nodeRow(page, answer.id).first();
    await newest.scrollIntoViewIfNeeded();
    await expect(newest, "the agent's reply draws no row as the page opens").toBeVisible();
    await expect.poll(unread, { message: "reading the thread did not mark it read" }).toBe(false);
  });
});

// c-reflow, on words the seeded issue does not carry. The rule binds the page a reader opens,
// not one seed's text: a statement whose words name a file path holds a run of 108 characters
// with no space in it, which is longer than the row that draws it at every width this reads.
// Found by the tester on 2026-11-09 at 39649c0f269d, where the Spec preset's requirement list
// drew that run in a span that neither wrapped it nor gave it its own line, and the whole page
// scrolled sideways: 884 px of page in a 390 px viewport. Read here on the outline, with no view
// on.
// SLOW(>=2s): a seeded issue loaded cold at three widths, each waited on until its words draw and
// measured for its fit.
test("a statement whose words hold an unbroken run keeps the outline inside its viewport @phone", async ({
  browser,
}) => {
  await phase(browser, seedUnbrokenWords, async (page, seeded) => {
    for (const width of [320, 390, 1440]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      // The words are on the page before it is measured, so a fit read before the rows draw
      // cannot pass for the fit of a page that holds them.
      await expect(
        page.getByText(UNBROKEN_PATH, { exact: false }).first(),
        `the outline draws none of the statement's words at ${width} px`
      ).toBeVisible();
      await assertPageFits(page);
    }
  });
});

// c-reflow again, on the other surface the same run reaches: a child's own title, which is the
// words of the child's row of the outline. Measured at 320 px with the same 108 character path
// on 2026-11-09 at 454f2a3107a6, in the children's section the page drew then: 807 px of text in
// a 288 px box and a page 823 px wide.
// SLOW(>=2s): a seeded issue and its child, loaded cold at three widths, each waited on until the
// child's row draws and measured for its fit.
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

/** One CSS selector for the element axe names by `target`: axe writes one part for each frame
 *  the element is inside, and a part that reaches into a shadow root as a list of its own. */
function axeSelector(target: readonly unknown[]): string {
  return target.map((part) => (Array.isArray(part) ? part.join(" ") : String(part))).join(" ");
}

/** What axe's `target-size` rule (WCAG 2.2's 2.5.8) says of the crumbs the page draws: a line for
 *  each crumb it fails or leaves for review, with the crumb's box and axe's own summary, and how
 *  many crumbs it passed. A crumb is a button the reader presses to zoom out, so it is a target
 *  whatever its tab index, but axe leaves an undersized target outside the tab order for review
 *  rather than failing it, and every crumb is outside it (`tabIndex={-1}`): at 1440 by 900 px,
 *  16 px crumbs with no padding of their own were no violation, each "incomplete", with axe's
 *  own words "Safe clickable space has a diameter of 16px instead of at least 24px. Is this a
 *  target?". So a crumb left for review is a fault here as one that fails is. */
async function crumbTargets(
  page: Page
): Promise<{ readonly faults: readonly string[]; readonly passed: number }> {
  const { incomplete, passes, violations } = await new AxeBuilder({ page })
    .withRules(["target-size"])
    .include("[data-crumb]")
    .analyze();
  const faults = await Promise.all(
    [...violations, ...incomplete]
      .flatMap((result) => result.nodes)
      .map(async (node) => {
        const selector = axeSelector(node.target);
        const box = await page.locator(selector).evaluate((crumb) => {
          const drawn = crumb.getBoundingClientRect();
          return `${Math.round(drawn.width)} by ${Math.round(drawn.height)} px at ${Math.round(drawn.left)}, ${Math.round(drawn.top)}`;
        });
        return `${selector}, ${box}: ${(node.failureSummary ?? "").replace(/\s+/gu, " ")}`;
      })
  );
  return { faults, passed: passes.flatMap((result) => result.nodes).length };
}

/** Every crumb the page draws passes axe's `target-size` rule, WCAG 2.2's 2.5.8, at `where`: it
 *  is at least 24 by 24 px, or so spaced that a 24 px circle on it meets no other target. axe
 *  fails none, leaves none for review and passes each one, so a line it read no crumb of fails
 *  too. */
async function expectCrumbTargets(page: Page, where: string): Promise<void> {
  let read: Awaited<ReturnType<typeof crumbTargets>> | undefined;
  await expect
    .poll(
      async () => {
        read = await crumbTargets(page);
        return read.faults;
      },
      { message: `at ${where} axe's target-size rule (WCAG 2.2's 2.5.8) fails a crumb` }
    )
    .toEqual([]);
  expect(
    read?.passed,
    `at ${where} axe's target-size rule passed fewer crumbs than the line draws`
  ).toBe(await page.locator("[data-crumb]").count());
}

// c-tree, the crumb (the architect's ruling on a node's one line): under the breadcrumb a zoom
// draws, a crumb is the node's first line by that rule, cut at a word, never cut again
// mid-word by its box, and a line that is a whole sentence carries no ellipsis after its own full
// stop. The issue's crumb is its key. The Contents entry's half is left to the Contents pane's
// own entries, which this tree does not hold yet.
// SLOW(>=2s): the whole issue seeded and loaded at a phone's size on a reply's address, waited on
// until the reply has the focus, then zoomed into and measured, axe's target-size rule run on it,
// and the same loaded, zoomed into and read by axe again at 1440 by 900 px, then zoomed out and
// the reply's thread folded.
test("a crumb draws its node's first line, cut at a word @phone @phone-only", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // Zoomed into a reply of a thread, the issue's crumb is its key and the thread's is its
    // stored first line, whole. The thread is the one the server says the reply answers.
    const comments = await listComments(seeded.key);
    const reply = comments.find((one) => one.id === seeded.comments.replies[0]);
    const thread = comments.find((one) => one.id === reply?.reply_to);
    expect(thread, "the server holds no thread the seeded reply answers").toBeDefined();
    const crumbs = tree(page).locator("xpath=..").locator("[data-crumb]");
    /** Loads the reply's address at `size`, waits until the reply has the focus, and zooms into
     *  it, waiting until the zoom draws more than one crumb. */
    const zoomIntoReply = async (
      size: { readonly height: number; readonly width: number },
      at: string
    ): Promise<void> => {
      await page.setViewportSize(size);
      await gotoIssue(page, `/issues/${seeded.key}/comments/${seeded.comments.replies[0] ?? ""}`);
      await expectOpeningLanded(page);
      await expect
        .poll(() => focusedNodeId(page), {
          message: `the reply's address does not focus its row${at}`,
        })
        .toBe(seeded.comments.replies[0] ?? "");
      await page.keyboard.press(".");
      await expect
        .poll(() => crumbs.count(), { message: `no crumb is drawn${at}` })
        .toBeGreaterThan(1);
    };
    await zoomIntoReply({ height: PHONE.height, width: PHONE.width }, "");
    // The poll above waited until the zoom drew more than one crumb, so this read holds the
    // thread's crumb and the loop below walks at least it.
    const texts = (await crumbs.allInnerTexts()).map((crumb) => crumb.trim());
    expect(texts[0], "the issue's crumb repeats its title").toBe(seeded.key);
    expect(texts[1], "the thread's crumb is not its stored first line").toBe(
      firstLineOf(thread?.body ?? "").line
    );
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
    // A crumb is a word in a line of text and not a control box (`IssueTree.tsx`): below 1280 px
    // the floor of `styles.css` made each crumb a box at least 44 by 44 px and the line 44 px
    // tall, around a line of words 16 px tall. Its padding keeps it no shorter than WCAG 2.2's
    // 24 px target (2.5.8). The two crumbs and the "/" after them fit on one line at 390 px, and
    // that line is as tall as one line of a crumb's words and its padding.
    const drawn = await crumbs.evaluateAll((all) => {
      const [first] = all;
      const line = first?.parentElement?.getBoundingClientRect();
      const style = first === undefined ? null : getComputedStyle(first);
      const crumbBox = first?.getBoundingClientRect();
      return {
        heights: all.map((crumb) => crumb.getBoundingClientRect().height),
        line: line?.height ?? Number.NaN,
        oneLine: [...(first?.parentElement?.children ?? [])].every((element) => {
          const box = element.getBoundingClientRect();
          return (
            crumbBox !== undefined &&
            box.top >= crumbBox.top - 1 &&
            box.bottom <= crumbBox.bottom + 1
          );
        }),
        oneLineTall:
          style === null
            ? Number.NaN
            : Number.parseFloat(style.lineHeight) +
              Number.parseFloat(style.paddingTop) +
              Number.parseFloat(style.paddingBottom),
      };
    });
    for (const [index, height] of drawn.heights.entries()) {
      expect(
        height,
        `crumb ${index + 1} is a 44 px control box, not a word in a line of text`
      ).toBeLessThan(44);
      expect(
        height,
        `crumb ${index + 1} is shorter than WCAG 2.2's 24 px target`
      ).toBeGreaterThanOrEqual(24);
    }
    expect(
      drawn.oneLine,
      'the crumbs and their "/" wrap at 390 px, where they fit on one line'
    ).toBe(true);
    expect(
      drawn.line,
      `the crumb line is taller than one line of a crumb's words and its padding, ${drawn.oneLineTall} px`
    ).toBeLessThanOrEqual(drawn.oneLineTall + 1);
    // Nothing the zoom draws makes the page scroll sideways.
    await assertPageFits(page);

    // WCAG 2.2's 2.5.8 as axe reads it (`target-size`), on this zoom two levels deep: at the
    // iPhone 13's 390 by 664 px, and at 1440 by 900 px, zoomed the same way from the reply's
    // address. At 1440 px, where `dispatch-inline-link` sets no padding, a crumb with no padding
    // of its own is its 16 px line alone, 34 by 16 and 125 by 16 px, which axe left for review.
    await page.setViewportSize({ height: 664, width: 390 });
    await expectCrumbTargets(page, "390 by 664 px");
    await zoomIntoReply(DESKTOP, " at 1440 px");
    await expectCrumbTargets(page, "1440 by 900 px");

    // The reply's address holds the path to it open to land the reader there. `,` backs out one
    // level at a time, to the thread and then to the whole issue; out of the zoom, Left on the
    // reply steps to its thread, and Left on the thread, a branch of that path, folds it at once:
    // held open, the fold drew nothing and Left said nothing.
    const replyId = seeded.comments.replies[0] ?? "";
    await page.keyboard.press(",");
    await expect
      .poll(() => crumbs.count(), { message: "`,` did not back out one level at 1440 px" })
      .toBeLessThan(texts.length);
    await page.keyboard.press(",");
    await expect(crumbs, "`,` did not zoom out at 1440 px").toHaveCount(0);
    await focusRow(page, replyId);
    await page.keyboard.press("ArrowLeft");
    await expect
      .poll(() => focusedNodeId(page), { message: "Left on the reply did not step to its thread" })
      .toBe(thread?.id ?? "");
    await page.keyboard.press("ArrowLeft");
    await expect(
      nodeRow(page, thread?.id ?? "").first(),
      "Left on the thread the address goes through drew it still open"
    ).toHaveAttribute("aria-expanded", "false");
    await expect(nodeRow(page, replyId), "the folded thread still draws the reply").toHaveCount(0);
    await expect(pageStatus(page), "Left on the thread said something").toHaveCount(0);
  });
});

/** Reads that the outline holds no row the focus left any more, `after` something: no row has the
 *  focused look (`pageFocusedRow`), which the row kept while it was held (the architect's ruling
 *  on a press, as amended). A row's face is drawn whatever the focus, its acts with it (R11), so
 *  the bodies stay, and the rows that draw their acts are `acts`, the rows that drew them with the
 *  focus on the page. */
async function expectRowLetGo(page: Page, after: string, acts: readonly string[]): Promise<void> {
  await expect
    .poll(
      () =>
        nodeRows(page)
          .locator("[data-node-card]")
          .evaluateAll(
            (cards, look) => cards.filter((card) => card.className.includes(look)).length,
            pageFocusedRow
          ),
      { message: `a row still looks focused after ${after}` }
    )
    .toBe(0);
  expect(await rowsWithActs(page), `the rows draw other acts after ${after}`).toEqual(acts);
}

/** The ids of the rows that draw their acts (`[data-row-acts]`), in order. */
function rowsWithActs(page: Page): Promise<string[]> {
  return nodeRows(page).evaluateAll((rows) =>
    rows.flatMap((row) =>
      row.querySelector("[data-row-acts]") === null ? [] : [row.getAttribute("data-node-id") ?? ""]
    )
  );
}

/** The ids of the rows that draw a body, in order. */
async function rowsWithBodies(page: Page): Promise<string[]> {
  return (await drawnRows(page)).filter((row) => row.body).map((row) => row.id ?? "");
}

// c-focus-opens, as every row draws on its face what its focused body drew.
// SLOW(>=2s): the whole issue seeded and loaded, every row and every link rested on past the
// delay a card opens after, the rows read three times and their shapes' colours once, three
// controls outside the outline pressed, one of them the header's and one a view's, which the
// outline is waited for after, and the Inbox loaded.
test("a row draws its face whatever the focus, and nothing opens on hover", async ({ browser }) => {
  // The row rests the pointer on every row and every reference the page draws, each for longer
  // than a card takes to open elsewhere: half a second of absence each, on top of its own walk.
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);

    // The bodies are the rows' faces, drawn whatever the focus: the same rows draw one with the
    // focus on the page and on a row. The page's opening puts the focus on a row first, and the
    // focus leaves for the page after it.
    await expectOpeningLanded(page);
    await expectLoadSettled(settling);
    await focusThePage(page);
    const faces = await rowsWithBodies(page);
    expect(faces.length, "no row draws a body with the focus on the page").toBeGreaterThan(0);
    const acts = await rowsWithActs(page);
    expect(acts.length, "no row draws its acts with the focus on the page").toBeGreaterThan(0);
    await page.keyboard.press("Tab");
    const focused = await focusedNodeId(page);
    expect(focused, "Tab put the focus on no row of the tree").not.toBeNull();
    await expectIssuePageDrawn(page);
    expect(await rowsWithBodies(page), "the focus changed which rows draw a body").toEqual(faces);
    await page.keyboard.press("j");
    expect(await rowsWithBodies(page), "a move changed which rows draw a body").toEqual(faces);
    // A press on a control of the page outside the outline fires with a row focused (the
    // architect's ruling on a press, as amended). The press focuses the control as it goes down,
    // and a row changing as the focus left the outline moved the page under the pointer: the
    // press ended on something else, and the control never fired. The row stays held while the
    // press is held and through its click, and is let go once the press has ended.
    //
    // First a control of the header: the issue's actions control, which opens the issue's
    // actions.
    await focusRow(page, focused ?? "");
    const actions = issuePage(page)
      .locator("[data-issue-header]")
      .getByRole("button", { exact: true, name: CONTROL_LABELS.issue_actions });
    await actions.click();
    await expect(
      actions,
      "a press on the header's actions control, with a row's body open, did not open the actions"
    ).toHaveAttribute("aria-expanded", "true");
    await expectRowLetGo(page, "the press on the header's actions control ended", acts);
    await page.keyboard.press("Escape");
    await expect(actions, "Escape did not close the issue's actions").toHaveAttribute(
      "aria-expanded",
      "false"
    );

    // Then a control under the focused row, which a body closing above it moves. Nothing of the
    // page's own stands under the outline at this head - Details is a drawer fixed at the right
    // of the screen, whose controls stand at its top and move with nothing the outline draws - so
    // a stand-in does (`standInControlBelow`), in the middle of the screen, under the outline's
    // last row, which End focuses.
    await focusRow(page, focused ?? "");
    await page.keyboard.press("End");
    expect(await focusedNodeId(page), "End did not move to the outline's last row").toBe(
      (await visibleIds(page)).at(-1) ?? ""
    );
    const last = tabStopRow(page);
    await expect(last, "End left no row of the outline as its tab stop").toHaveCount(1);
    const below = await standInControlBelow(page);
    await below.control.evaluate((control) => control.scrollIntoView({ block: "center" }));
    await below.control.click();
    expect(
      await below.presses(),
      "a press on a control under the focused row, with the row's body open, did not fire it"
    ).toBe(1);
    await expectRowLetGo(page, "the press on the control under the focused row ended", acts);
    await below.remove();

    // Tab is no press: the focus leaving the outline by it lets go of the row at once.
    await focusRow(page, focused ?? "");
    await page.keyboard.press("Tab");
    expect(await focusedNodeId(page), "Tab left the focus on a row of the outline").toBeNull();
    await expectRowLetGo(page, "Tab left the outline", acts);

    // Then a view control above the outline: Spec, pressed with a row's body open, draws the
    // Spec view in the outline's place, and Conversation draws the outline again.
    await focusRow(page, focused ?? "");
    await presetToggle(page, "spec").click();
    await expect(
      page,
      "a press on Spec with a row's body open did not open the Spec view"
    ).toHaveURL(new RegExp(`${presetPath(seeded.key, "spec")}$`));
    await expect(outlineRows(page), "the Spec view draws the outline").toHaveCount(0);
    await presetToggle(page, "conversation").click();
    await expect
      .poll(() => outlineRows(page).count(), {
        message: "Conversation does not draw the outline again after Spec",
      })
      .toBeGreaterThan(0);

    // With the keyboard focus on no control the rows draw the faces they drew as the page
    // opened, and the pointer opens nothing either.
    await focusThePage(page);
    expect(
      await rowsWithBodies(page),
      "the rows draw other bodies with the keyboard focus on no control"
    ).toEqual(faces);
    await expectLoadSettled(settling);
    const resting = REF_PREVIEW_OPEN_DELAY_MS + 200;
    const before = await treeText(page);
    const rows = nodeRows(page);
    // Every read the page made went quiet above, after Conversation drew the outline again, so
    // this count is every row the outline draws.
    const count = await rows.count();
    for (let index = 0; index < count; index += 1) {
      await rows.nth(index).hover();
      // The pointer rests on each row, as on each link below, past the delay a card opens after
      // on every other page, so a body that opens for a resting pointer a moment after it arrives
      // is drawn by the time the row reads its absence: read at once after each hover, it was not.
      await page.waitForTimeout(resting);
      expect(
        await rowsWithBodies(page),
        `the pointer resting on row ${index} opened a body with the focus elsewhere`
      ).toEqual(faces);
    }
    expect(await treeText(page), "the pointer crossing every row changed the tree's text").toBe(
      before
    );
    expect(
      await tooltipTexts(page),
      "the page carries tooltips, which a reader on a phone or on the keyboard never sees"
    ).toEqual([]);

    // No link opens a card on hover either, a link in a row's face included: the issue page is
    // worked from the keyboard. A decision's words name the requirements it shaped, each as a
    // chip in place and again as a chip at the end of its words, and every node's words are
    // drawn whole, so the pointer rests on every link the page draws - the children's, the
    // panel's and that row's - with the keyboard focus elsewhere. No link of the outline is a
    // preview's trigger at this head: the links in a node's words and its chips carry no
    // `data-dispatch-ref` or `data-dispatch-href` (`row-references.ts`, `NodeRow.tsx`'s `Chip`),
    // so a focused one opens no card either, and the card a focused link opens is not this row's
    // to read.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const decision = nodeRow(page, blockNodeId(specSlug, seeded.decisions[0] ?? "")).first();
    await expect
      .poll(() => rowWords(decision).locator("a[href]:visible").count(), {
        message: "the decision's words draw no link",
      })
      .toBeGreaterThan(0);
    const links = issuePage(page).locator(
      "a[href]:visible, [data-dispatch-ref]:visible, [data-dispatch-href]:visible"
    );
    // The page's reads went quiet before the walk over the rows, and the poll above saw the
    // decision draw the links in its words, so one count holds every link drawn.
    const linkCount = await links.count();
    for (let index = 0; index < linkCount; index += 1) {
      await links.nth(index).hover();
      // A pointer resting on the link past the delay a card opens after on every other page:
      // the absence is the claim, so the wait is the span the card would have taken to open.
      await page.waitForTimeout(resting);
      expect(
        await page.getByRole("tooltip").count(),
        `the pointer resting on link ${index} of the issue page opened a card`
      ).toBe(0);
    }
    await page.mouse.move(0, 0);

    // Nothing is carried by colour: each state and type has a word and a shape no other carries,
    // and the reading holds with the colours forced. A greyscale filter is not the test - today's
    // three muted states already differ in luminance and would pass one.
    const drawn = await drawnRows(page);
    expectRowVocabulary(drawn, await rowTags(page));
    const states = statesDrawn(drawn);
    expect(
      [...new Set(states.values())].length,
      `two states are drawn the same way: ${JSON.stringify([...states])}`
    ).toBe(states.size);
    // Each typed kind's tag carries its shape, which is the first shape its tag draws in the row's
    // margin: a row's face draws its author's shape in its words, before the margin in the
    // document, so the first shape of the row is no longer the tag's. A turn of the conversation
    // draws no tag, and its author's shape says whose words it is.
    const tagShapes = await nodeRows(page).evaluateAll((rows) =>
      rows.map((row) => ({
        shape:
          row
            .querySelector("[data-node-margin] [data-node-tag] [data-glyph]")
            ?.getAttribute("data-glyph") ?? "",
        type: row.getAttribute("data-node-type") ?? "",
      }))
    );
    const types = new Map(
      tagShapes
        .filter(({ type }) => !UNTYPED_KINDS.includes(type))
        .map(({ shape, type }): [string, string] => [type, shape])
    );
    expect(
      [...new Set(types.values())].length,
      `two types draw the same shape: ${JSON.stringify([...types])}`
    ).toBe(types.size);
    // Forced colours put the reader's own colours in place of the page's, which neither the
    // tokens nor the words above show. Read with them on: each shape a row draws still stands
    // out from what is behind it as a graphic must (`SHAPE_CONTRAST`), so none is drawn by a
    // colour they take away, such as a box's background.
    await page.emulateMedia({ forcedColors: "active" });
    const forced = await drawnRows(page);
    expectRowVocabulary(forced, await rowTags(page));
    // The rows were drawn and read just above with the colours forced, and the assertion below
    // holds this read to those rows before the loop walks them.
    const painted = await shapesPainted(page);
    expect(
      painted.map((row) => row.id),
      "the rows whose shapes were read are not the rows drawn"
    ).toEqual(forced.map((row) => row.id));
    for (const row of painted) {
      for (const shape of row.shapes) {
        expect(
          shape.contrast,
          `the ${shape.token} shape on row ${row.id} is not drawn visibly once the colours are forced`
        ).toBeGreaterThanOrEqual(SHAPE_CONTRAST);
      }
    }

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

// SLOW(>=2s): the whole issue seeded and loaded in a context of its own, then some twenty keys
// pressed with every row read again after most of them, and a caret put in a node.
test("the tree's keys move, fold, zoom and open the focused node", async ({ browser }) => {
  const { context, page, seeded } = await openTree(browser);
  try {
    const order = await visibleIds(page);
    expect(
      order.length,
      "the keys have no rows to move through: the tree draws none"
    ).toBeGreaterThan(3);
    await expectTreeDrawn(page);

    // j, k and the arrows move to the next and previous visible row.
    await page.keyboard.press("Tab");
    // The tree's one tab stop is the row the page opened on, its open question; the keys step
    // from the first row, in the order the tree read gives the rows, since the question may stand
    // anywhere in it.
    const start = order.indexOf(blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID));
    expect(start, "the open question is no visible row").toBeGreaterThanOrEqual(0);
    expect(await focusedRow(page), "Tab did not put the reader on the row the page opened on").toBe(
      order[start]
    );
    await page.keyboard.press("Home");
    expect(await focusedRow(page), "Home did not reach the first visible row").toBe(order[0]);
    for (const [index, step] of [
      [1, "j"],
      [2, "ArrowDown"],
      [1, "k"],
      [0, "ArrowUp"],
    ] as const) {
      await page.keyboard.press(step);
      expect(await focusedRow(page), `${step} did not move to row ${index}`).toBe(order[index]);
      await expectFocusedRowClear(page, step);
    }

    // Home and End reach the first and last visible row.
    await page.keyboard.press("End");
    expect(await focusedRow(page), "End did not reach the last visible row").toBe(
      order[order.length - 1]
    );
    await expectFocusedRowClear(page, "End");
    await page.keyboard.press("Home");
    expect(await focusedRow(page), "Home did not reach the first visible row").toBe(order[0]);

    // The right arrow unfolds the focused node and then moves into its first child; the left
    // arrow moves back to the parent and then folds it. Nothing opens folded (R11), so the row
    // folds a branch with the left arrow before the right arrow unfolds it.
    const open = (await drawnRows(page)).find((row) => row.expanded === "true");
    expect(open, "no branch is drawn, so the arrows have nothing to fold and unfold").toBeDefined();
    const branch = open?.id ?? "";
    await nodeRow(page, branch).first().focus();
    await page.keyboard.press("ArrowLeft");
    expect(
      (await drawnRows(page)).find((row) => row.id === branch)?.expanded,
      "the left arrow did not fold the focused node"
    ).toBe("false");
    await page.keyboard.press("ArrowRight");
    expect(
      (await drawnRows(page)).find((row) => row.id === branch)?.expanded,
      "the right arrow did not unfold the focused node"
    ).toBe("true");
    expect(
      await focusedRow(page),
      "the right arrow left the focused node before unfolding it"
    ).toBe(branch);
    // The branch's first child is the row after it one level deeper, which is where the second
    // right arrow has to land: "not the branch" would also hold for a focus that was lost or
    // that moved to the branch's own sibling.
    const unfolded = (await drawnRows(page)).filter((row) => row.visible);
    const at = unfolded.findIndex((row) => row.id === branch);
    const firstChild = unfolded[at + 1];
    expect(
      firstChild?.level,
      "the unfolded branch draws no row under it, so it has no first child to move into"
    ).toBe(String(Number(unfolded[at]?.level ?? "0") + 1));
    await page.keyboard.press("ArrowRight");
    expect(
      await focusedRow(page),
      "the second right arrow did not move into the branch's first child"
    ).toBe(firstChild?.id);
    await page.keyboard.press("ArrowLeft");
    expect(await focusedRow(page), "the left arrow did not move to the parent").toBe(branch);
    await page.keyboard.press("ArrowLeft");
    expect(
      (await drawnRows(page)).find((row) => row.id === branch)?.expanded,
      "the second left arrow did not fold the focused node"
    ).toBe("false");

    // o unfolds the focused node, and again folds it.
    const expandedOf = async (id: string) =>
      (await drawnRows(page)).find((row) => row.id === id)?.expanded;
    await page.keyboard.press("o");
    expect(await expandedOf(branch), "o did not unfold the focused node").toBe("true");
    await page.keyboard.press("o");
    expect(await expandedOf(branch), "o did not fold the focused node").toBe("false");

    // . zooms into the focused node and , comes back out one level.
    const whole = await visibleIds(page);
    await page.keyboard.press(".");
    const zoomed = await visibleIds(page);
    expect(
      zoomed.length,
      "the zoom key drew the same rows, so it zoomed into nothing"
    ).toBeLessThan(whole.length);
    expect(zoomed[0], "the zoom key did not make the focused node the tree's root").toBe(branch);
    await page.keyboard.press(",");
    expect(await visibleIds(page), "the key back out did not restore the level above").toEqual(
      whole
    );

    // q moves to the next open question, whatever its depth.
    await page.keyboard.press("q");
    const question = await focusedRow(page);
    expect(
      (await drawnRows(page)).find((row) => row.id === question)?.type,
      "the next-question key reached a row that is no open question"
    ).toBe("ask");

    // w moves into the decision behind the focused node.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    await nodeRow(page, blockNodeId(specSlug, requirementBlockId(1)))
      .first()
      .focus();
    await page.keyboard.press("w");
    expect(await focusedRow(page), "w did not move into the decision behind the node").toBe(
      blockNodeId(specSlug, seeded.decisions[0] ?? "")
    );

    // z unfolds every node the reader folded: the branch folded above, and a second one folded
    // now, so the rows are again those the page opened with, every node unfolded.
    const other = (await drawnRows(page)).find(
      (row) => row.visible && row.expanded === "true" && row.id !== branch
    );
    expect(other, "no second branch is drawn to fold").toBeDefined();
    await focusRow(page, other?.id ?? "");
    await page.keyboard.press("ArrowLeft");
    await expect(
      nodeRow(page, other?.id ?? "").first(),
      "the left arrow did not fold the second branch"
    ).toHaveAttribute("aria-expanded", "false");
    await page.keyboard.press("z");
    await expect
      .poll(() => visibleIds(page), { message: "z did not unfold every node the reader folded" })
      .toEqual(order);

    // Enter puts the caret in the focused node; Escape leaves the caret for the row, then the
    // tree. This extends the binding the page already has and adds none.
    const second = blockNodeId(specSlug, requirementBlockId(2));
    await nodeRow(page, second).first().focus();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the focused node" })
      .toBe(true);
    await page.keyboard.press("Escape");
    expect(await caretInNode(page), "Escape did not leave the caret").toBe(false);
    expect(await focusedRow(page), "Escape left the row as well as the caret").toBe(second);
    await page.keyboard.press("Escape");
    expect(await focusedRow(page), "the second Escape did not leave the tree").toBeNull();
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the whole issue seeded and loaded in a context of its own, then some sixty keys
// pressed at two sizes, each followed by a read of where the focus is, and the page loaded again
// for a zoom into a long comment's row, read again a second later. It took 27 to 36 s in the
// pod's runs that passed, and twice in a busy pod it ran out of the project's 60 s mid-move, so it
// takes the 120 s the file's other long rows take.
test("Tab enters and leaves the tree at one stop and every move keeps the row on screen", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const { context, page, seeded } = await openTree(browser);
  try {
    const order = await visibleIds(page);
    expect(order.length, "the tree needs two visible rows to test a move").toBeGreaterThan(1);

    // One stop in: Tab reaches the tree, and the next Tab leaves it rather than stepping through
    // its rows, as the WAI-ARIA tree pattern requires.
    await page.keyboard.press("Tab");
    expect(await focusInside(tree(page)), "Tab did not enter the tree").toBe(true);
    await expectTreeDrawn(page);
    // Tab enters on the open question. It can be the last row in the tree read's order, so
    // start the forward move from Home rather than assuming there is a row after it.
    const opened = blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID);
    expect(await focusedRow(page), "Tab did not reach the open question").toBe(opened);
    await page.keyboard.press("Home");
    expect(await focusedRow(page), "Home did not reach the first visible row").toBe(order[0]);
    await page.keyboard.press("j");
    const inside = await focusedRow(page);
    expect(inside, "j did not reach the second visible row").toBe(order[1]);
    await page.keyboard.press("Tab");
    expect(
      await focusInside(tree(page)),
      "Tab stepped to another row of the tree instead of leaving it"
    ).toBe(false);
    await page.keyboard.press("Shift+Tab");
    expect(await focusInside(tree(page)), "Shift+Tab did not come back into the tree").toBe(true);
    expect(await focusedRow(page), "Shift+Tab came back to another row than the one left").toBe(
      inside
    );

    // The focused row is inside the viewport and clear of the sticky header after every move,
    // including the ones that scroll the page.
    for (let step = 0; step < Math.min(order.length - 1, 12); step += 1) {
      await page.keyboard.press("j");
      await expectFocusedRowClear(page, `move ${step + 1} with j`);
    }
    await pressToOutlineEnd(page, "End");
    await expectFocusedRowClear(page, "End");
    // The cycle key moves no focus, and draws bodies and then whole branches above the focused
    // row: a key that moves no focus still keeps the focused row on screen, held there while the
    // rows above it grow. On a long issue at 1440 by 900, before the page placed it, the row's
    // top stood at 3,276, 3,991 and 694 px after the three presses.
    for (const press of [1, 2, 3]) {
      await page.keyboard.press("z");
      await expectFocusedRowOnScreen(
        page,
        `press ${press} of z on the last row`,
        "left the focused row off the screen"
      );
    }
    await pressToOutlineEnd(page, "Home");
    await expectFocusedRowClear(page, "Home");
    // Home on the first row while it already holds the focus, after the reader scrolled it off
    // the screen: the move moves no focus, and still brings the row back on screen, since a key
    // that leaves the reader on a row they cannot see has taken them nowhere. Before it the row
    // stayed where the scroll left it, thousands of pixels above.
    const first = (await focusedNodeId(page)) ?? "";
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await expect
      .poll(() => onScreen(nodeRow(page, first).first()), {
        message: "the scroll to the page's foot left the first row on screen",
      })
      .toBe(false);
    await pressToOutlineEnd(page, "Home");
    await expectFocusedRowClear(page, "Home on the row that holds the focus, scrolled off");

    // The box at the foot of the screen: the page draws one to write in and publishes its height
    // on the document element (`FOOT_HEIGHT_PROPERTY` of `identity-block.ts`), and the page
    // brings the focused row clear of it as it does of the band at the top. A 200 px stand-in
    // for that box: every move keeps the focused row whole above it. Measured at the outline's
    // own head with only the height published and no box: the moves from the seventh on lift the
    // row to 8 px above it, and End leaves the last row under it, with the page scrolled as far
    // as it goes.
    const takeFootAway = await standInFootBox(page, 200);
    for (let step = 0; step < Math.min(order.length - 1, 12); step += 1) {
      await page.keyboard.press("j");
      await expectFocusedRowOnScreen(
        page,
        `move ${step + 1} with j`,
        "left the focused row under the box at the foot"
      );
    }
    await pressToOutlineEnd(page, "End");
    await expectFocusedRowOnScreen(page, "End", "left the focused row under the box at the foot");
    await takeFootAway();

    // At 760 px and below the header scrolls away and the view row stays at the top, the band
    // the header publishes (`IDENTITY_HEIGHT_PROPERTY`): the moves up from the outline's last row
    // take the page up under it, and each keeps the focused row whole and clear below it. Home
    // first, so that End moves and places the last row at this size. Measured with the band read
    // as 0 at this size by all three that place a row below it - `bringOnScreen`,
    // `belowWhatStays` (`NodeRow.tsx`) and the page's `scroll-padding-top`, by which a key's
    // focus scrolls - moves with k left the row's top at 40, 13, 15 and 0 px under a band 51 px
    // tall; at the outline's own head, where nothing published the band, at 40, 13 and 0 px.
    await page.setViewportSize({ height: 664, width: 390 });
    await pressToOutlineEnd(page, "Home");
    await pressToOutlineEnd(page, "End");
    for (let step = 0; step < 25; step += 1) {
      await page.keyboard.press("k");
      await expectFocusedRowOnScreen(
        page,
        `move ${step + 1} with k from End at 390x664`,
        "left the focused row off the screen or under the band that stays at the top"
      );
    }
    // A zoom draws the crumb line above the rows it zooms to, and places that line below the
    // band, with the focused row still on screen. The moves up with k leave the row just under
    // the band, where on a long issue the line drawn above it stood at -5 to 39 px under a band
    // 51 px tall, and the reader read it only once they had scrolled.
    await page.keyboard.press(".");
    await expectZoomPlaced(page, ". at 390x664");
    await expectFocusedRowOnScreen(page, ". at 390x664", "left the focused row off the screen");
    // Right on the row the zoom is rooted at, which is drawn open over what it holds, moves to
    // its first child, and where nothing is under it says so at the row, as every key with
    // nowhere to go does. At 390 by 664 it zoomed into the row it was on: nothing moved and
    // nothing was said.
    const [zoomRoot, under] = await drawnRows(page);
    expect(zoomRoot?.id, ". left the focus on another row than the zoom's own").toBe(
      await focusedRow(page)
    );
    await page.keyboard.press("ArrowRight");
    if (under !== undefined && Number(under.level) > Number(zoomRoot?.level)) {
      await expect
        .poll(() => focusedRow(page), {
          message: "Right on the zoom's own row did not move to its first child",
        })
        .toBe(under.id);
    } else {
      await expect(
        nodeRow(page, zoomRoot?.id ?? "")
          .first()
          .locator("[data-row-report]"),
        "Right on the zoom's own row, with nothing under it, did not say so at the row"
      ).toHaveText(KEY_REPORTS.nothing_under);
      expect(await focusedRow(page), "Right with nothing under the row moved the focus").toBe(
        zoomRoot?.id
      );
    }

    // A zoom into a row taller than the window leaves the line below the band and the row's top
    // just under the line. Placed by the page's one rule, as it was zoomed into and again as it
    // grew, the row's top went to just under the band, which took the line above it under the
    // band: on a real issue at this size rows of 1,634 and 1,464 px left the line at 15 to 59 and
    // at -53 to 59 px under a band 51 px tall, still there 1.5 s later. No row of the seed is that
    // tall, so a comment of thirty paragraphs is.
    const long = await createComment(seeded.key, {
      body: Array.from(
        { length: 30 },
        (_, paragraph) => `Paragraph ${paragraph + 1} of words taller than any window.`
      ).join("\n\n"),
    });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectTreeDrawn(page);
    await expectOpeningLanded(page);
    // The page lands on the question that waits on the reader once its read answers, which
    // would take the focus back from a row focused before then.
    const question = blockNodeId(await primarySlug(seeded.key), OPEN_QUESTION_BLOCK_ID);
    await expect
      .poll(() => focusedRow(page), {
        message: "the page loaded again with the focus off its open question",
      })
      .toBe(question);
    const longRow = await focusRow(page, long.id);
    expect(
      await longRow.evaluate((element) => element.getBoundingClientRect().height),
      "the long comment's focused row is not taller than the window"
    ).toBeGreaterThan(664);
    await page.keyboard.press(".");
    await expectZoomPlaced(page, ". on a row taller than the window at 390x664");
  } finally {
    await context.close();
  }
});

/**
 * Where the keyboard focus is, read against node row `row`, in the words a failure reads: "its
 * code block" or "its table" where it is on that block of the row's words - the `pre` or the
 * `table` itself, or a box that holds it alone and scrolls it sideways - "the row" on the row
 * itself, and otherwise what holds it and whether that is inside the row, elsewhere in the tree,
 * or outside the tree.
 */
function focusAgainstRow(row: Locator): Promise<string> {
  return row.evaluate((element) => {
    const focused = document.activeElement;
    if (focused === null || focused === document.body) return "outside the tree, on the page";
    if (focused === element) return "the row";
    const what = focused.localName;
    if (element.contains(focused)) {
      const code = focused.querySelector("pre") !== null;
      const table = focused.querySelector("table") !== null;
      const scrolls = focused.scrollWidth > focused.clientWidth;
      if (what === "pre" || (code && !table && scrolls)) return "its code block";
      if (what === "table" || (table && !code && scrolls)) return "its table";
      return `a ${what} inside the row`;
    }
    if (element.closest('[role="tree"]')?.contains(focused) === true) {
      return `a ${what} elsewhere in the tree`;
    }
    return `outside the tree, on a ${what}`;
  });
}

// The outline is one tab stop, and a reader on the keyboard still scrolls a wide block of the
// focused row's words sideways (WCAG 2.1.1, which axe's `scrollable-region-focusable` rule
// reads): the code block and the table that scroll in the row that holds the focus are tab stops,
// each named for what it holds, and every other row's are out of the tab order. With every row's
// out, as the outline had them, no key reached a wide block, and no key scrolled it; with the
// focus on the code block the tree still took its keys, so Right scrolled nothing and End moved the
// focus to the tree's last row. At 1440 by 900, a comment whose words hold a code block and a
// table, each wider than its box, and a comment of words alone after it: Tab from the first stops
// on its code block, where Right scrolls it sideways with the focus still on it, and End, which the
// browser scrolls by only up and down, leaves the focus on it and moves and folds no row; Shift+Tab
// and Escape each give the row back; Tab from the row stops on the code block, then on its table,
// then leaves the tree; from the second, Tab leaves the tree at once, and so does Shift+Tab, which
// passes the first comment's blocks above it; and with the first focused, axe finds no scroller on
// the page that the keyboard cannot reach.
// SLOW(>=2s): an issue of two comments seeded and loaded at the first one's address, its code
// block and table waited for until they are laid out wider than their boxes, fourteen keys each
// waited on until the focus lands, the block scrolls or the rows read as before, and the page read
// by axe once the first row has the focus.
test("Tab from the focused row stops on its code block and its table, then leaves the tree, and a row with neither leaves at once", async ({
  browser,
}) => {
  await phase(browser, seedWideCode, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}/comments/${seeded.wide}`);
    await expectTreeDrawn(page);
    await expectOpeningLanded(page);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the comment's address did not put the focus on its row",
      })
      .toBe(seeded.wide);
    const row = nodeRow(page, seeded.wide).first();
    for (const [block, what] of [
      ["pre", "code block"],
      ["table", "table"],
    ] as const) {
      await expect
        .poll(
          () =>
            rowWords(row)
              .locator(block)
              .evaluateAll((found) =>
                found.some((element) => element.scrollWidth > element.clientWidth)
              ),
          { message: `the focused comment's row never drew a ${what} wider than its own box` }
        )
        .toBe(true);
    }

    // Tab from the focused row stops on its code block, then on its table, each named for what
    // it holds, and then leaves the tree.
    const focused = page.locator(":focus");
    await page.keyboard.press("Tab");
    await expect
      .poll(() => focusAgainstRow(row), {
        message: "Tab from the focused comment's row did not stop on its code block",
      })
      .toBe("its code block");
    await expect(
      focused,
      "the code block's tab stop is not named for what it holds"
    ).toHaveAccessibleName(CONTROL_LABELS.scrolls_code);

    // On the code block the keys that scroll a box are the browser's: Right scrolls it sideways
    // with the focus still on it, and End, which the browser scrolls by in the other direction
    // alone, leaves the focus on it and moves and folds no row. Shift+Tab gives the row back, as
    // the tab order has it, and so does Escape; Tab from the row stops on the code block again.
    const code = rowWords(row).locator("pre");
    await page.keyboard.press("ArrowRight");
    await expect
      .poll(() => code.evaluate((element) => element.scrollLeft), {
        message: "Right on the focused comment's code block did not scroll it sideways",
      })
      .toBeGreaterThan(0);
    await expect
      .poll(() => focusAgainstRow(row), {
        message: "Right on the focused comment's code block took the focus off it",
      })
      .toBe("its code block");
    /** Each row the tree draws, by its node, with its fold and whether it holds the tree's
     *  stop. */
    const folds = async () =>
      (await drawnRows(page)).map(({ expanded, foldedCount, id, tabIndex }) => ({
        expanded,
        foldedCount,
        id,
        tabIndex,
      }));
    const beforeEnd = await folds();
    await page.keyboard.press("End");
    await expect
      .poll(() => focusAgainstRow(row), {
        message: "End on the focused comment's code block took the focus off it",
      })
      .toBe("its code block");
    await expect
      .poll(folds, {
        message: "End on the focused comment's code block moved the tree's stop or a fold",
      })
      .toEqual(beforeEnd);
    for (const key of ["Shift+Tab", "Escape"]) {
      await page.keyboard.press(key);
      await expect
        .poll(() => focusAgainstRow(row), {
          message: `${key} on the focused comment's code block did not give the row back`,
        })
        .toBe("the row");
      await page.keyboard.press("Tab");
      await expect
        .poll(() => focusAgainstRow(row), {
          message: `Tab from the row, after ${key}, did not stop on its code block again`,
        })
        .toBe("its code block");
    }
    await page.keyboard.press("Tab");
    await expect
      .poll(() => focusAgainstRow(row), {
        message: "Tab from the focused comment's code block did not stop on its table",
      })
      .toBe("its table");
    await expect(
      focused,
      "the table's tab stop is not named for what it holds"
    ).toHaveAccessibleName(CONTROL_LABELS.scrolls_table);
    await page.keyboard.press("Tab");
    await expect
      .poll(() => focusAgainstRow(row), {
        message: "Tab from the focused comment's table did not leave the tree",
      })
      .toMatch(/^outside the tree/u);

    // Back on the row, j moves to the comment of words alone. From it Tab leaves the tree at
    // once, and Shift+Tab does too: the first comment's blocks above it are no tab stop while
    // its row holds no focus.
    await page.keyboard.press("Shift+Tab");
    await expect
      .poll(() => focusAgainstRow(row), {
        message: "Shift+Tab from past the tree did not come back to the comment's row",
      })
      .toBe("the row");
    await page.keyboard.press("j");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "j did not move from the comment's row to the comment of words alone",
      })
      .toBe(seeded.plain);
    const plain = nodeRow(page, seeded.plain).first();
    await expect(
      rowWords(plain),
      "the comment of words alone draws no words in its row"
    ).toContainText("A comment of words alone");
    await expect(
      rowWords(plain).locator("pre, table"),
      "the comment of words alone draws a code block or a table"
    ).toHaveCount(0);
    await page.keyboard.press("Tab");
    await expect
      .poll(() => focusAgainstRow(plain), {
        message:
          "Tab from a row whose words hold neither a code block nor a table did not leave the tree at once",
      })
      .toMatch(/^outside the tree/u);
    await page.keyboard.press("Shift+Tab");
    await expect
      .poll(() => focusAgainstRow(plain), {
        message: "Shift+Tab from past the tree did not come back to the comment of words alone",
      })
      .toBe("the row");
    await page.keyboard.press("Shift+Tab");
    await expect
      .poll(() => focusAgainstRow(row), {
        message:
          "Shift+Tab from the comment of words alone did not leave the tree at once, past the blocks of the row above it, which holds no focus",
      })
      .toMatch(/^outside the tree/u);

    // With the first comment's row focused, axe finds no scroller on the page that the keyboard
    // cannot reach.
    await focusRow(page, seeded.wide);
    await expect
      .poll(
        async () =>
          (
            await new AxeBuilder({ page }).withRules(["scrollable-region-focusable"]).analyze()
          ).violations.flatMap((violation) =>
            violation.nodes.map((node) => axeSelector(node.target))
          ),
        {
          message:
            "axe finds scrollers the keyboard cannot reach on the page with the comment's row focused",
        }
      )
      .toEqual([]);
  });
});

// SLOW(>=2s): the whole issue seeded and loaded in a context of its own, for one read of its rows.
test("the tree answers the WAI-ARIA tree pattern's roles and properties", async ({ browser }) => {
  const { context, page } = await openTree(browser);
  try {
    const rows = await drawnRows(page);
    expect(
      rows.length,
      'the tree pattern has nothing to answer for: no role="treeitem" row is drawn'
    ).toBeGreaterThan(0);
    await expectTreeDrawn(page);
    expect(
      rows.filter((row) => row.role !== "treeitem").map((row) => row.id),
      "these rows of the tree are no treeitem"
    ).toEqual([]);
    expect(
      rows.filter((row) => !/^[1-9]\d*$/.test(row.level ?? "")).map((row) => row.id),
      "these rows carry no aria-level"
    ).toEqual([]);

    // A row that holds children says whether it is open; a row that holds none says nothing.
    const levels = rows.map((row) => Number(row.level ?? "0"));
    const holdsChildren = rows.map(
      (row, index) => (levels[index + 1] ?? 0) > (levels[index] ?? 0) || row.foldedCount !== null
    );
    expect(
      rows
        .filter((row, index) => holdsChildren[index] === true && row.expanded === null)
        .map((row) => row.id),
      "these rows hold children and carry no aria-expanded"
    ).toEqual([]);
    expect(
      rows
        .filter((row, index) => holdsChildren[index] !== true && row.expanded !== null)
        .map((row) => row.id),
      "these rows hold no children and carry aria-expanded all the same"
    ).toEqual([]);

    // The tree is one tab stop: it roves with one tabbable row, or it names the row it is on.
    const tabbable = rows.filter((row) => row.tabIndex === 0);
    // One state is the claim, the one the rows above were read in with no key pressed since, and
    // the ternaries below only spell this answer for the assertion.
    const named = await treeActiveDescendant(page);
    expect(
      `${tabbable.length}/${named === null ? "none" : "named"}`,
      `the tree has ${tabbable.length} tabbable rows and ${named === null ? "no" : "an"} aria-activedescendant, so it is not one stop`
    ).toMatch(/^(1\/none|0\/named|1\/named)$/);
  } finally {
    await context.close();
  }
});

// c-chain.
// SLOW(>=2s): the whole issue seeded, loaded until its reads settle and unfolded press by press,
// then its rows read whole five times or more and seven keys pressed.
test("the chain is reachable and only gaps that tell a node apart are drawn", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const planSlug = (await getArtifact(seeded.planArtifactId)).slug;
    const informed = blockNodeId(specSlug, requirementBlockId(1));
    const decision = blockNodeId(specSlug, seeded.decisions[0] ?? "");
    const check = blockNodeId(verificationSlug, seeded.checks.passed);
    const results = (await getIssueResults(seeded.key)).checks ?? [];
    const latest = results.find((one) => one.block_id === seeded.checks.passed)?.results?.[0];
    expect(latest, "the seed recorded no result against the check that serves r1").toBeDefined();

    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectLoadSettled(settling);
    await expectOpeningLanded(page);
    await unfoldEveryBranch(page);

    // The gaps the page draws are those that tell a node apart and name work somebody owes: each
    // requirement no check serves, and the plan task that serves no requirement, which Graph
    // draws (below) - and no other node draws any. The check whose latest result records no
    // negative control is one of those others: the server asks a negative control of a
    // before-merge pass alone, and this check runs after the merge, so the page names nothing it
    // lacks.
    const coverage = await getIssueCoverage(seeded.key);
    // A statement's gaps are those R10 names, in the order the page draws them: no check where
    // the coverage read counts none, and, since the seed's plan holds tasks, no plan task where
    // it counts none serving it (`tree-read/tree-nodes.ts`' `draftRequirement`).
    const planned = seeded.planTasks.length > 0;
    const wanted = new Map<string, string[]>();
    for (const requirement of coverage.requirements ?? []) {
      const gaps = [
        ...(requirement.checks === 0 ? ["no_check"] : []),
        ...(planned && requirement.plan_tasks === 0 ? ["no_plan_task"] : []),
      ];
      if (gaps.length > 0) wanted.set(blockNodeId(specSlug, requirement.block_id), gaps);
    }
    const nobodyAsked = blockNodeId(planSlug, planTaskBlockId(seeded.planTasks.length));
    const uncontrolled = (await getIssueResults(seeded.key)).checks?.find(
      (one) => one.block_id === seeded.checks.uncontrolled
    );
    expect(
      uncontrolled?.results?.[0]?.negative_control,
      "the seed's uncontrolled check's latest result records a negative control"
    ).toBe("");
    await expect
      .poll(
        async () => {
          const drawn = new Map<string, string[]>();
          for (const row of await drawnRows(page)) {
            if (row.gapKinds.length > 0) drawn.set(row.id ?? "", [...row.gapKinds]);
          }
          return Object.fromEntries([...drawn].sort());
        },
        {
          message: "the page draws a gap on a node it does not tell apart, or none on one it does",
        }
      )
      .toEqual(Object.fromEntries([...wanted].sort()));
    await expect
      .poll(async () => rowsByKind(await drawnRows(page)), {
        message: "the unfolded tree does not draw a row for every node the seed wrote",
      })
      .toEqual(rowsOfTheWholeIssue(seeded));

    // From the requirement to its checks: each is drawn under it, and the arrow keys walk down to
    // them from the requirement's row. The decision's chip that names the requirement, and
    // following it, are left to the Spec view's own rows, which this tree does not hold yet.
    expect(await parentOfRow(page, check), "the check that serves r1 is not under it").toBe(
      informed
    );
    await nodeRow(page, informed).first().focus();
    const walked: (string | null)[] = [];
    for (let step = 0; step < 6; step += 1) {
      await page.keyboard.press("ArrowDown");
      walked.push(await focusedNodeId(page));
    }
    expect(walked, "the arrows from the requirement do not reach its check").toContain(check);

    // From the check, `w` reaches back to the decision that informed its requirement.
    await nodeRow(page, check).first().focus();
    await page.keyboard.press("w");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "w on the check does not move the focus to the decision behind its requirement",
      })
      .toBe(decision);

    // No gap that every node of a kind carries is drawn: no requirement and no decision says
    // anything about a source, a read-back or an author. The head's gap and the Spec view's
    // readings are left to the Spec view's own rows, which this tree does not hold yet.
    expect(
      (await drawnRows(page))
        .filter((row) => row.type === "requirement" || row.type === "decision")
        .flatMap((row) => row.gapKinds)
        // A missing check and a missing plan task tell a statement apart by what serves it, and
        // the poll above holds each statement to them, so neither is a gap of its whole kind.
        .filter((kind) => kind !== "no_check" && kind !== "no_plan_task"),
      "a requirement or a decision draws a gap its whole kind carries"
    ).toEqual([]);
    for (const words of ["No source given", "Not confirmed in Dispatch", "No author in Dispatch"]) {
      expect(await issuePage(page).getByText(words).count(), `the page draws "${words}"`).toBe(0);
    }

    // Graph, which draws the whole tree read: the check's latest result stands under the check,
    // and the arrows walk down to it; the plan task that serves no requirement draws its gap.
    const graphSettling = watchApiRequests(page);
    await gotoIssue(page, presetPath(seeded.key, "graph"));
    await expectIssuePageDrawn(page);
    await expectLoadSettled(graphSettling);
    await expect
      .poll(async () => rowOf(await drawnRows(page), nobodyAsked)?.gapKinds, {
        message: "the plan task that serves no requirement draws no gap in Graph",
      })
      .toEqual(["nobody_asked"]);
    expect(
      await parentOfRow(page, latest?.id ?? ""),
      "the check's latest result is not under the check in Graph"
    ).toBe(check);
    await nodeRow(page, check).first().focus();
    const down: (string | null)[] = [];
    for (let step = 0; step < 6; step += 1) {
      await page.keyboard.press("ArrowDown");
      down.push(await focusedNodeId(page));
    }
    expect(down, "the arrows from the check do not reach its result in Graph").toContain(
      latest?.id ?? ""
    );
  });
});

// The tester's finding: what a key says when it has nowhere to go - `w` on a row no decision
// stands behind - is drawn at the row it was pressed on, once, and the reader's next move lets
// it go. At 1440 px Home on the first row, End on the last and Right on a row with nothing under
// it moved nothing and said nothing, and at 390 px so did `.` on the row the tree is zoomed to:
// each says so at its row, and the next move lets it go. The tree draws every level at every
// width (R11), so the reader zooms into the thread with `.` from its head, Left on the zoom's own
// row has nowhere to go and says so as it does at 1440 px, `,` zooms back out, and Left on a
// child of the issue with the whole issue drawn folds it and then says so.
// SLOW(>=2s): a seeded thread loaded cold and one of its rows focused, for nine keys pressed,
// then loaded again at a phone's size on a reply's address, waited on until the reply has the
// focus, for nine keys more and a zoom out waited on.
test("a key with nowhere to go says so at its row, and the next move lets it go", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectOpeningLanded(page);
    await expectIssuePageDrawn(page);
    await focusRow(page, seeded.answeredAsk);
    await page.keyboard.press("w");
    await expect(
      nodeRow(page, seeded.answeredAsk).first().locator("[data-row-report]"),
      "the key's report is not drawn at the row it was pressed on"
    ).toHaveCount(1);
    expect(await pageStatus(page).count(), "the key's report is drawn at more than one place").toBe(
      1
    );
    expect(
      (await issuePage(page).getByRole("status").allInnerTexts()).filter(
        (words) => words.trim() !== ""
      ).length,
      "the key's report is drawn a second time away from the row"
    ).toBe(1);
    await page.keyboard.press("j");
    await expect(pageStatus(page), "the report stays after the reader moved on").toHaveCount(0);
    /** The report of the key pressed `after` is drawn at the focused row, in `words`, and
     *  nowhere else on the page. */
    const reportedAt = async (after: string, words: string): Promise<void> => {
      const at = (await focusedNodeId(page)) ?? "";
      await expect(
        nodeRow(page, at).first().locator("[data-row-report]"),
        `${after} says nothing at the row`
      ).toHaveText(words);
      await expect(
        pageStatus(page),
        `the report of ${after} is drawn at more than one place`
      ).toHaveCount(1);
    };
    // The first press of each end key moves to its end, and the second has nowhere to go.
    await pressToOutlineEnd(page, "Home");
    await pressToOutlineEnd(page, "Home");
    await reportedAt("Home on the first row", KEY_REPORTS.first_row);
    await page.keyboard.press("j");
    await expect(
      pageStatus(page),
      "the report of Home on the first row stays after the reader moved on"
    ).toHaveCount(0);
    await pressToOutlineEnd(page, "End");
    await pressToOutlineEnd(page, "End");
    await reportedAt("End on the last row", KEY_REPORTS.last_row);
    await page.keyboard.press("k");
    await expect(
      pageStatus(page),
      "the report of End on the last row stays after the reader moved on"
    ).toHaveCount(0);
    const leaf =
      (await drawnRows(page)).find((row) => row.visible && row.expanded === null)?.id ?? "";
    expect(leaf, "the outline draws no row with nothing under it").not.toBe("");
    await focusRow(page, leaf);
    await page.keyboard.press("ArrowRight");
    await reportedAt("Right on a row with nothing under it", KEY_REPORTS.nothing_under);
    expect(await focusedNodeId(page), "Right on a row with nothing under it moved the focus").toBe(
      leaf
    );

    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}/comments/${seeded.first}`);
    await expectOpeningLanded(page);
    await expect
      .poll(() => focusedNodeId(page), { message: "the reply's address does not focus its row" })
      .toBe(seeded.first);
    const crumbs = issuePage(page).locator("[data-crumb]");
    await expect(crumbs, "the reply's address zooms the tree at 390 px").toHaveCount(0);
    // Up to the thread's head, and `.` zooms the tree to it.
    await page.keyboard.press("ArrowUp");
    await expect.poll(() => focusedNodeId(page)).toBe(seeded.head);
    await page.keyboard.press(".");
    await expect(crumbs, "`.` on the thread's head does not zoom the tree to it").toHaveCount(1);
    // `.` on the zoom's own row asks for the zoom the tree holds already: it says so at the row,
    // and zooms nowhere.
    await page.keyboard.press(".");
    await reportedAt(". on the row the tree is zoomed to", KEY_REPORTS.already_zoomed);
    await expect(crumbs, ". on the row the tree is zoomed to zoomed elsewhere").toHaveCount(1);
    expect(await focusedNodeId(page), ". on the zoom's own row moved the focus").toBe(seeded.head);
    // The next move lets the report go, and the move back to the zoom's own row says nothing.
    await page.keyboard.press("j");
    await expect
      .poll(() => focusedNodeId(page), { message: "j on the zoom's own row did not move down" })
      .not.toBe(seeded.head);
    await expect(pageStatus(page), "the report of . stays after the reader moved on").toHaveCount(
      0
    );
    await page.keyboard.press("k");
    await expect
      .poll(() => focusedNodeId(page), { message: "k did not move back to the zoom's own row" })
      .toBe(seeded.head);
    await expect(
      pageStatus(page),
      "the move back to the zoom's own row said something"
    ).toHaveCount(0);
    // Left on the zoom's own row: the row stays open over what it holds and its parent is above
    // the zoom, which no row draws, so the key has nowhere to go and says so at the row.
    await page.keyboard.press("ArrowLeft");
    await reportedAt("Left on the row the tree is zoomed to", KEY_REPORTS.already_zoomed);
    await expect(crumbs, "Left on the zoom's own row zoomed elsewhere").toHaveCount(1);
    expect(await focusedNodeId(page), "Left on the zoom's own row moved the focus").toBe(
      seeded.head
    );
    // `,` zooms back out to the whole issue, with the focus kept on the head.
    await page.keyboard.press(",");
    await expect(crumbs, ", on the zoom's own row did not zoom out").toHaveCount(0);
    await expect
      .poll(() => focusedNodeId(page), { message: "zooming out took the focus off the head" })
      .toBe(seeded.head);
    // Left on the head, a child of the issue that holds the thread's replies, folds it; Left on
    // it folded has nowhere to go with the whole issue drawn, and says so.
    await page.keyboard.press("ArrowLeft");
    await expect(
      nodeRow(page, seeded.head).first(),
      "Left on the open head of the thread did not fold it"
    ).toHaveAttribute("aria-expanded", "false");
    await page.keyboard.press("ArrowLeft");
    await expect(
      nodeRow(page, seeded.head).first().locator("[data-row-report]"),
      "Left on a child of the issue, with the whole issue drawn, says nothing at the row"
    ).toHaveText(KEY_REPORTS.whole_tree);
    expect(await focusedNodeId(page), "Left on a child of the issue moved the focus").toBe(
      seeded.head
    );
  });
});

// The architect's rule for an address of a node of the issue: following a link to it opens the
// branches above the node and puts the focus on its row. At 1440 by 900, zoomed into one thread,
// a link in a reply of that thread to a comment outside it went to the comment's address, and the
// tree, still zoomed into the thread, drew no row for the comment: the landing gave up with
// nothing said, and the focus stayed on the link it was followed from. The following leaves the
// zoom, and the comment's row takes the focus on screen.
// SLOW(>=2s): a seeded thread and two comments more, loaded cold, a thread zoomed into and a link
// in one of its replies followed, waited on until the comment's row has the focus on screen.
test("a comment's address followed from a link while the reader is zoomed elsewhere lands on the comment's row", async ({
  browser,
}) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    const elsewhere = await createComment(seeded.key, {
      body: "A comment outside the thread the reader zooms into.",
    });
    const reference = `dispatch://${seeded.key}/comment/${elsewhere.id}`;
    const pointer = await createComment(seeded.key, {
      body: `The comment this answers is ${reference} for now.`,
      reply_to: seeded.head,
    });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectOpeningLanded(page);
    await expectIssuePageDrawn(page);
    await focusRow(page, seeded.head);
    await page.keyboard.press(".");
    await expect(
      issuePage(page).locator("[data-crumb]"),
      "the thread's head was not zoomed into"
    ).toHaveCount(1);
    await expect(
      nodeRow(page, elsewhere.id),
      "the zoom into the thread draws the comment outside it"
    ).toHaveCount(0);

    await focusRow(page, pointer.id);
    await rowReference(nodeRow(page, pointer.id).first(), reference).click();
    await expect(page, "the link did not go to the comment's address").toHaveURL(
      new RegExp(`/issues/${seeded.key}/comments/${elsewhere.id}$`, "u")
    );
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the comment's address, followed from a zoom into another thread, landed nowhere",
      })
      .toBe(elsewhere.id);
    await expect(
      issuePage(page).locator("[data-crumb]"),
      "the zoom into the other thread stood after the following"
    ).toHaveCount(0);
    await expectFocusedRowOnScreen(page, "the following", "left the comment's row off screen");
  });
});

// Editing in a node. Enter on a row whose node is a block of one of the issue's documents puts
// the caret at the start of its words, in that document's one room: the row takes the page's one
// editor of the document, so the room gets no second connection. A second reader who does the
// same on their own page is in that room too, and the first one's row draws the second one's
// caret. What is typed is stored, and keys pressed before the caret has reached the statement
// edit the statement or nothing. Escape gives the same row the focus, and a second Escape leaves
// the outline. A comment's row holds no document's text, and Enter puts the read-only caret there.
// SLOW(>=2s): the whole issue seeded and loaded, a second reader's context loading it too, both
// carets put in the spec's live room, two reloads or more reading what was stored, and Enter
// pressed on a page slowed four times, with Delete pressed until the caret is in the statement.
test("Enter puts the caret in a node, and Escape gives the row back", async ({ browser }) => {
  test.setTimeout(120_000);
  const { context, page, seeded } = await openTree(browser);
  try {
    await expectTreeDrawn(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = blockNodeId(specSlug, requirementBlockId(2));
    const typed = "Caretword ";
    const sockets = countDocumentSockets(page, seeded.specArtifactId);

    // Enter on the statement: the caret is in its words, in the spec's one room.
    await focusRow(page, statement);
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the row" })
      .toBe(true);
    await expect
      .poll(sockets, { message: "the spec's room took other than one connection" })
      .toBe(1);
    await page.keyboard.type(typed);

    // A second reader, bob, presses Enter on the same statement on his own page: alice's row
    // draws his caret, and neither page's room takes a second connection. A peer's caret on the
    // very spot of a focused editor's own is not drawn (`collab-cursor-plugin.ts`), so he comes
    // in after alice has typed, which moved hers past the start of the words his lands on. Both
    // pages are 1280 px wide, the narrowest at which an editor takes part in the room's carets
    // (`ProofDocument.tsx`).
    const bob = await asUser(browser, "bob");
    try {
      await withIssuePage(bob, "bob");
      const bobPage = await bob.newPage();
      await bobPage.setViewportSize({ height: 900, width: 1280 });
      const bobSockets = countDocumentSockets(bobPage, seeded.specArtifactId);
      await gotoIssue(bobPage, `/issues/${seeded.key}`);
      await expectOpeningLanded(bobPage);
      await expectTreeDrawn(bobPage);
      await focusRow(bobPage, statement);
      await bobPage.keyboard.press("Enter");
      await expect
        .poll(() => caretInNode(bobPage), { message: "Enter did not put bob's caret in the row" })
        .toBe(true);
      await expect(
        nodeRow(page, statement).first().locator(cursorLabel(page, "bob")),
        "alice's row draws no caret of bob's, who is editing the same statement"
      ).toBeVisible();
      await expect
        .poll(bobSockets, { message: "bob's room took other than one connection" })
        .toBe(1);
      expect(sockets(), "alice's room took a second connection when bob came in").toBe(1);
    } finally {
      await bob.close();
    }

    // Escape leaves the caret on that row, and a second Escape leaves the outline.
    await page.keyboard.press("Escape");
    expect(await caretInNode(page), "Escape did not take the caret out").toBe(false);
    expect(await focusedRow(page), "Escape gave the focus to another row").toBe(statement);
    await page.keyboard.press("Escape");
    expect(await focusedRow(page), "the second Escape did not leave the outline").toBeNull();

    // What was typed is stored: a reload reads it, and the same keys remove it.
    const storedWords = async () => {
      await page.reload();
      await expectOpeningLanded(page);
      await expectTreeDrawn(page);
      const row = await focusRow(page, statement);
      return (await rowWords(row).innerText()).replace(/\s+/g, " ");
    };
    expect(await storedWords(), "the word typed in the row was not stored").toContain(typed.trim());
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the row" })
      .toBe(true);
    for (let at = 0; at < typed.length; at += 1) await page.keyboard.press("Delete");
    await page.keyboard.press("Escape");
    await expect
      .poll(storedWords, { message: "the word typed in the row was not removed" })
      .not.toContain(typed.trim());

    // Keys pressed the moment Enter is pressed edit the statement or nothing. The editor took the
    // focus with its own selection still at the top of the document, and read the caret put in
    // the statement only a task later: on a busy page that was 0.27 to 0.84 s, and ten Deletes
    // pressed in it removed the spec's first heading. The page is slowed here as a busy page is,
    // the CPU throttled four times, and Delete pressed at once and again, without waiting on
    // anything, until the caret has stood in the statement for three presses.
    const before = await storedMarkdown(seeded.specArtifactId);
    const words = requirementText(2);
    const at = before.indexOf(words);
    expect(at, "the stored spec does not hold the statement's words").toBeGreaterThan(-1);
    const cpu = await context.newCDPSession(page);
    await cpu.send("Emulation.setCPUThrottlingRate", { rate: 4 });
    try {
      await page.keyboard.press("Enter");
      const until = Date.now() + 30_000;
      for (let inBlock = 0; inBlock < 3 && Date.now() < until; ) {
        await page.keyboard.press("Delete");
        // The instant is the claim: each Delete goes out without waiting, and this read counts
        // the presses that found the caret standing in the statement.
        if (await caretInRowBlock(page, requirementBlockId(2))) inBlock += 1;
      }
    } finally {
      await cpu.send("Emulation.setCPUThrottlingRate", { rate: 1 });
      await cpu.detach();
    }
    await expect
      .poll(() => storedMarkdown(seeded.specArtifactId), {
        message: "no Delete pressed after Enter reached the statement",
      })
      .not.toBe(before);
    const after = await storedMarkdown(seeded.specArtifactId);
    const above = before.slice(0, at);
    const below = before.slice(at + words.length);
    expect(
      after.slice(0, above.length),
      "a key pressed after Enter edited above the statement"
    ).toBe(above);
    expect(after.slice(after.length - below.length), "a key edited below the statement").toBe(
      below
    );
    const left = after.slice(above.length, after.length - below.length);
    expect(
      left.length < words.length && words.endsWith(left),
      `the statement reads "${left}", not its own words with their first ones deleted`
    ).toBe(true);
    await page.keyboard.press("Escape");

    // A comment's row holds no document's text, and is read-only in the room: Enter puts the
    // read-only caret at the start of its words (R6), and the row says nothing of having no text.
    const comment = seeded.comments.threads[0] ?? "";
    const commentWords = rowWords(nodeRow(page, comment).first());
    await focusRow(page, comment);
    await page.keyboard.press("Enter");
    await expect(
      commentWords,
      "Enter on a comment's row put no caret in its words"
    ).toHaveAttribute("contenteditable", "true");
    await expect(commentWords, "the comment's caret asks for a screen keyboard").toHaveAttribute(
      "inputmode",
      "none"
    );
    await expect(
      commentWords,
      "the comment's caret does not say its words are read-only"
    ).toHaveAttribute("aria-readonly", "true");
    await expect
      .poll(() => caretInNode(page), { message: "Enter on a comment put the focus in no caret" })
      .toBe(true);
    await expect
      .poll(
        () =>
          commentWords.evaluate((words) => {
            const selection = window.getSelection();
            const anchor = selection?.anchorNode ?? null;
            if (selection === null || anchor === null || !words.contains(anchor)) {
              return "a selection outside the comment's words";
            }
            if (!selection.isCollapsed) return `the selection "${selection.toString()}"`;
            const before = document.createRange();
            before.setStart(words, 0);
            before.setEnd(anchor, selection.anchorOffset);
            return before.toString();
          }),
        { message: "the comment's caret is not collapsed at the start of its words" }
      )
      .toBe("");
    await expect(
      issuePage(page).getByText(KEY_REPORTS.no_text),
      "Enter on a comment's row said it has no text to put the caret in"
    ).toHaveCount(0);
  } finally {
    await context.close();
  }
});

/** A box's background before anything is drawn behind it, as computed. */
const TRANSPARENT = "rgba(0, 0, 0, 0)";

/** The ids of the rows the outline marks as taking typing, `[data-editable]` (`useNodeEditing.ts`'s
 *  `nodeEditable`), in the order they are drawn. */
function editableRows(page: Page): Promise<string[]> {
  return issuePage(page)
    .locator("[data-node-row][data-editable]")
    .evaluateAll((rows) => rows.map((row) => row.getAttribute("data-node-id") ?? "<no id>"));
}

/** What the cue that a block takes typing draws on `row` (`styles.css`'s `[data-editable]`
 *  rules), as computed: the row's cursor, and the background of its card, which is where the tint
 *  is. */
function rowCue(row: Locator): Promise<{ readonly background: string; readonly cursor: string }> {
  return row.evaluate((element) => {
    const card = element.querySelector(":scope > [data-node-card]");
    return {
      background: card === null ? "<no card>" : getComputedStyle(card).backgroundColor,
      cursor: getComputedStyle(element).cursor,
    };
  });
}

/** The outline the cue draws on the box a row's editor scrolls in, as computed, and whether the
 *  focus is inside that box. */
function editorBoxRule(row: Locator): Promise<{
  readonly color: string;
  readonly focused: boolean;
  readonly offset: string;
  readonly style: string;
  readonly width: string;
}> {
  return row.locator("[data-editor-box]").evaluate((box) => {
    const style = getComputedStyle(box);
    return {
      color: style.outlineColor,
      focused: box.matches(":focus-within"),
      offset: style.outlineOffset,
      style: style.outlineStyle,
      width: style.outlineWidth,
    };
  });
}

/** The page's faint pane and its ink as the browser computes them from the palette (`styles.css`'s
 *  `@theme`), which the cue is drawn in. */
function cueColours(page: Page): Promise<{ readonly ink: string; readonly pane: string }> {
  return page.evaluate(() => {
    const probe = document.createElement("span");
    probe.style.color = "var(--color-page-ink)";
    probe.style.backgroundColor = "var(--color-page-pane)";
    document.body.append(probe);
    const style = getComputedStyle(probe);
    const colours = { ink: style.color, pane: style.backgroundColor };
    probe.remove();
    return colours;
  });
}

/** Opens the issue `key` and, once the page has put the focus on a row of its own, takes it off
 *  the outline and moves the pointer away, so no row is drawn focused or under the pointer. */
async function openWithNoRowFocused(page: Page, key: string): Promise<void> {
  await gotoIssue(page, `/issues/${key}`);
  await expectIssuePageDrawn(page);
  await expectOpeningLanded(page);
  await focusThePage(page);
  await page.mouse.move(0, 0);
}

// A block that takes typing says so. Under the pointer, a row whose node Enter puts the caret in
// - a block of one of the issue's documents, on an open issue - draws the text cursor and the
// page's faint pane behind its card; once Enter has put the caret in it, the box its editor
// scrolls in draws a 1 px outline in the page's ink just inside its edge, which takes no room. A
// comment's row takes no typing and draws neither, and neither does any block of a closed issue,
// whose documents are read-only, nor the box of its editor at focus. Each is read as computed
// style, the rows by the mark the outline gives a row that takes typing.
// SLOW(>=2s): the whole issue seeded and loaded, Enter waited on until the statement's editor
// opens, then the issue closed and loaded again and the statement's editor opened there.
test("an editable row shows it takes typing under the pointer and at focus, and a comment row and a closed issue's block show neither", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    const statement = blockNodeId(specSlug, requirementBlockId(2));
    const check = blockNodeId(verificationSlug, seeded.checks.noResult);
    const comment = seeded.comments.threads[0] ?? "";
    await openWithNoRowFocused(page, seeded.key);
    const colours = await cueColours(page);
    const statementRow = nodeRow(page, statement).first();
    const checkRow = nodeRow(page, check).first();
    const commentRow = nodeRow(page, comment).first();

    // The statement's row and the check's are marked as taking typing, and the comment's is not.
    // Each row is drawn as the read of its own document lands, so the marks are read until both
    // rows hold theirs, and the comment's once its row is drawn.
    await expect
      .poll(
        async () => {
          const marked = await editableRows(page);
          return { check: marked.includes(check), statement: marked.includes(statement) };
        },
        {
          message:
            "the statement's row and the check's are not both marked as taking typing ([data-editable])",
        }
      )
      .toEqual({ check: true, statement: true });
    await expect(commentRow, "the comment's row is not drawn").toBeAttached();
    expect(await editableRows(page), "the comment's row is marked as taking typing").not.toContain(
      comment
    );

    // Under the pointer the statement's row draws the text cursor and the pane behind its card,
    // and drew neither before.
    expect(
      await rowCue(statementRow),
      "the statement's row draws a cue with no pointer on it"
    ).toEqual({ background: TRANSPARENT, cursor: "auto" });
    await statementRow.hover();
    await expect
      .poll(() => rowCue(statementRow), {
        message: "the statement's row under the pointer draws no text cursor and no tint",
      })
      .toEqual({ background: colours.pane, cursor: "text" });

    // The comment's row under the pointer draws neither.
    await commentRow.hover();
    await nextFrames(page);
    expect(await rowCue(commentRow), "the comment's row under the pointer draws a cue").toEqual({
      background: TRANSPARENT,
      cursor: "auto",
    });

    // Once Enter has put the caret in the statement, its editor's box draws the outline.
    await focusRow(page, statement);
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the statement" })
      .toBe(true);
    await expect
      .poll(() => editorBoxRule(statementRow), {
        message: "the box of the statement's editor draws no 1 px rule in the ink at focus",
      })
      .toEqual({ color: colours.ink, focused: true, offset: "-1px", style: "solid", width: "1px" });
    await page.keyboard.press("Escape");
    await expect
      .poll(() => caretInNode(page), { message: "Escape did not take the caret out" })
      .toBe(false);

    // Closed, the issue's documents are read-only: no row is marked, and the statement's row
    // under the pointer draws neither. The marks are read once the statement's row and the
    // check's are drawn, as the reads of their documents land.
    await patchIssue(seeded.key, { status: "done" });
    await openWithNoRowFocused(page, seeded.key);
    await expect(statementRow, "the closed issue's statement row is not drawn").toBeAttached();
    await expect(checkRow, "the closed issue's check row is not drawn").toBeAttached();
    await expect
      .poll(() => editableRows(page), {
        message: "a row of the closed issue is marked as taking typing",
      })
      .toEqual([]);
    await statementRow.hover();
    await nextFrames(page);
    expect(
      await rowCue(statementRow),
      "the closed issue's statement under the pointer draws a cue"
    ).toEqual({ background: TRANSPARENT, cursor: "auto" });

    // Nor does the box of its editor draw the rule at focus. Enter opens the statement's editor
    // there, admitted read-only, and the focus stays on the row, since that editor takes no
    // caret; so the document inside the box is given the focus, as a control of the document's
    // would take it. The box itself is not: the browser draws its own ring on what holds the
    // focus, which is no rule of the cue's.
    await focusRow(page, statement);
    await page.keyboard.press("Enter");
    const article = statementRow.locator("[data-editor-box] .dispatch-doc");
    await expect(
      article,
      "Enter drew no read-only document in the closed issue's statement row"
    ).toHaveAttribute("data-read-only", "true");
    await article.evaluate((element) => {
      element.tabIndex = -1;
      element.focus();
    });
    expect(
      await editorBoxRule(statementRow),
      "the box of the closed issue's statement editor draws a rule at focus"
    ).toMatchObject({ focused: true, style: "none" });
  });
});

// The tester's finding at #4205's 2ff6850a, on "a node the room keeps read-only draws neither"
// of the architect's ruling on the cue that a block takes typing: the server admits a document
// read-only to a page whose schema it cannot trust with writes, as for a page whose build is out
// of date, and no block of that document takes typing then. The page here presents schema version
// 0 on every document socket, which no server admits writable (`admits` in
// `packages/envoy/internal/dispatch/docs/schema_admission.go`). At 2ff6850a a statement's row
// is marked as taking typing by Enter's rule alone, so under the pointer it draws the text cursor
// and the tint after the room has admitted that editor read-only; only the box of the editor
// follows the document's `data-read-only`.
// SLOW(>=2s): the whole issue seeded and loaded, then Enter waited on until the statement's editor
// opens read-only.
test("a statement of a document the server admits read-only shows no cue that it takes typing", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.addInitScript(() => {
      window.WebSocket = new Proxy(window.WebSocket, {
        construct(target, args) {
          const url = new URL(String(args[0]), window.location.href);
          if (url.searchParams.has("schema_version")) url.searchParams.set("schema_version", "0");
          return new target(url.toString(), args[1]);
        },
      });
    });
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = blockNodeId(specSlug, requirementBlockId(2));
    const other = blockNodeId(specSlug, requirementBlockId(3));
    await openWithNoRowFocused(page, seeded.key);
    const statementRow = nodeRow(page, statement).first();
    const otherRow = nodeRow(page, other).first();
    await expect(statementRow, "the statement's row is not drawn").toBeAttached();
    await expect(otherRow, "the second statement's row is not drawn").toBeAttached();

    // Enter opens the statement's editor, which the room admits read-only; then another statement
    // of the same document is under the pointer.
    await focusRow(page, statement);
    await page.keyboard.press("Enter");
    await expect(
      statementRow.locator("[data-editor-box] .dispatch-doc"),
      "Enter drew no read-only document in the statement's row"
    ).toHaveAttribute("data-read-only", "true");
    await otherRow.hover();
    await nextFrames(page);
    expect(
      await rowCue(otherRow),
      "a statement of a document the room admitted read-only draws a cue under the pointer"
    ).toEqual({ background: TRANSPARENT, cursor: "auto" });
  });
});

// When a read of the issue's comments no longer holds the comment whose row has the focus, that
// row leaves the tree and the focus falls to the page's body. The reader caused no loss - another
// session resolved a comment, which made the page read the comments again, and the row had the
// focus by a script's hand, no act of theirs - so the focus stays on the body, where the page's
// keys work, no row of the tree takes it, and the page does not scroll: the page, and not the
// reader, would have chosen what stands near the lost place. The comments read is answered
// without that comment as the server's answer reaches the page, and so is the tree read, which
// the outline's rows come from, without the comment's node and every tuple that names it; a real
// event of the issue - another comment resolved - makes the page read both again. The half the
// reader causes is the last row of `issue-page-l.e2e.ts`.
// SLOW(>=2s): the whole issue seeded and loaded, then waited on until the read after the event
// has drawn the outline without the comment.
test("a read that drops the focused row with no act of the reader's leaves the focus on the page's body and scrolls nothing", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const [gone = "", other = ""] = seeded.comments.threads;
    expect(other, "the seed holds fewer than two comment threads").not.toBe("");
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectOpeningLanded(page);
    await expectIssuePageDrawn(page);
    await focusRow(page, gone);

    await page.route(`**/api/v1/issues/${seeded.key}/comments*`, async (route) => {
      const answer = await route.fetch();
      const body = (await answer.json()) as unknown;
      const without = (list: readonly { readonly id?: string }[]) =>
        list.filter((comment) => comment.id !== gone);
      let json: unknown = body;
      if (Array.isArray(body)) json = without(body);
      else if (typeof body === "object" && body !== null) {
        const record = body as Record<string, unknown>;
        for (const field of ["comments", "items"]) {
          const list = record[field];
          if (Array.isArray(list)) json = { ...record, [field]: without(list) };
        }
      }
      await route.fulfill({ json, response: answer });
    });
    await page.route(treeReadRoute(seeded.key), async (route) => {
      const answer = await route.fetch();
      const body = (await answer.json()) as IssueTreeRead;
      // The comment's node, by the id the outline draws it by: its block id, else its own.
      const dropped = new Set(
        (body.nodes ?? []).filter((node) => blockIdOfNode(node) === gone).map(({ id }) => id)
      );
      await route.fulfill({
        json: {
          ...body,
          nodes: (body.nodes ?? []).filter((node) => !dropped.has(node.id)),
          tuples: (body.tuples ?? []).filter(
            (tuple) => !dropped.has(tuple.from.id) && !dropped.has(tuple.to.id)
          ),
        },
        response: answer,
      });
    });
    // Where the page stands once the row holds the focus, before the event.
    await nextFrames(page);
    const scrolled = await page.evaluate(() => window.scrollY);
    await resolveComment(other);
    await expect(
      nodeRow(page, gone),
      "the read after the event still draws the comment its answer no longer holds"
    ).toHaveCount(0);
    // The frames a move of the focus, and the scroll a row makes as it takes the focus, would
    // take to come.
    await nextFrames(page);
    expect(
      await page.evaluate(() => document.activeElement === document.body),
      "a read that dropped the focused row, with no act of the reader's, took the focus off the page's body"
    ).toBe(true);
    expect(
      await page.evaluate(() => window.scrollY),
      "a read that dropped the focused row, with no act of the reader's, scrolled the page"
    ).toBe(scrolled);
  });
});

/** Where `after` differs from `before`: the stretch of each that differs, with a few characters
 *  around it, for a failure to say where an edit landed. */
function changeIn(before: string, after: string): string {
  let start = 0;
  while (start < before.length && before[start] === after[start]) start += 1;
  let tail = 0;
  while (
    tail < before.length - start &&
    tail < after.length - start &&
    before[before.length - 1 - tail] === after[after.length - 1 - tail]
  ) {
    tail += 1;
  }
  const around = (text: string) =>
    JSON.stringify(text.slice(Math.max(0, start - 30), text.length - tail + 30).slice(0, 200));
  return `${around(before)} became ${around(after)}`;
}

// The row's editor keeps its caret, and every edit a key makes, inside the node's own block. The
// editor is the document's whole editor, and the keys it answers for a whole document reached past
// the block: measured on a preview, Ctrl+Home moved the caret before the spec's `## Summary` and a
// `Y` typed then made it `## YSummary`, and Up after Left put the caret before a heading above the
// statement. Each move is pressed once Enter has put the caret at the start of a node's block, and
// a `Y` typed after it. In a statement of the spec, Up from its first line and Down from its last
// leave the caret at its start - unguarded, each put it between the statement and the block above
// or below, and the `Y` became a paragraph of its own there - and select-all selects the
// statement's words alone, which the `Y` replaces - unguarded, the `Y` replaced the whole spec.
// In a decision - a `decision` block of the spec's `## Decisions` part, whose first block is its
// heading and whose last is its words - Ctrl+Home leaves the caret at the heading's start and
// Ctrl+End takes it to the end of the decision's words, the end of the node's own block -
// unguarded, on the decisions document a decision was a section of before it was a block of the
// spec, they took it to the start of the document's first heading and the end of its last
// paragraph; the spec opens and closes with typed blocks whose header and foot take no caret, and
// Chromium's Ctrl+Home and Ctrl+End leave the caret where it is there. Backspace pressed three
// times at the heading's start leaves the caret there too - unguarded, on that document, the
// editor took the heading a level down and then made it a paragraph (Milkdown's
// `DowngradeHeading`), and the third joined it to the paragraph above; on the spec's `decision`
// block, before the editor held the blocks inside the node's block too (`holdEditsToBlock`), the
// three made the decision's `### Keep...` a paragraph inside it. The spec's statements are typed
// blocks, which no join crosses. A paste of two lines at the heading's start, sent as a
// browser sends a paste, is refused - unguarded, on that document, the editor put a line break
// into the heading, and the stored `## Keep...` became a heading of two lines, `Two\`,
// `linesYKeep...` and `---` under them. Each time the stored document holds the `Y` in that block,
// where the move stopped, and every other block byte for byte as it was; the block is put back
// through the row after - past the `Y` and Backspace in a heading, since Delete at a heading's
// start takes it a level down - and the stored document read back as it was before.
// SLOW(>=2s): the whole issue seeded and loaded, then seven edits made through the row, each read
// back from the stored document, put back through the row and read back again.
test("the keys of a row's editor move its caret and edit inside the node's own block alone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const { context, page, seeded } = await openTree(browser);
  try {
    await expectTreeDrawn(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statementBlock = requirementBlockId(2);
    const statement = blockNodeId(specSlug, statementBlock);
    const words = requirementText(2);
    const heading = "Keep every address the old page answered";
    // The decision's last words, which end its block, the spec's `## Decisions` part's second.
    const closing = decisionClosingWords(seeded.key, 2, seeded.answeredAskId);
    const decision = seeded.decisions[1] ?? "";

    /** Enter on the focused row, the caret in its node's block `blockId`, and the editor's own
     *  write of its selection after the focus passed. */
    const enter = async (blockId: string) => {
      await page.keyboard.press("Enter");
      await expect
        .poll(() => caretInRowBlock(page, blockId), { message: "Enter put no caret in the block" })
        .toBe(true);
      // ProseMirror writes its own selection back over the page's 20 ms after it takes the focus
      // (prosemirror-view's focus handler), undoing a caret move it has not read yet: an arrow
      // pressed in that window is put back, and the key after it acts at the old place. A reader
      // is never that quick, so the row waits that window out before its first key. The page can
      // drop the editable's focus and give it back some 45 ms after the caret goes in, which
      // arms that write again, so the wait is a quiet spell: it ends once 50 ms have passed with
      // no `focusin` anywhere in the document, starting again at each one, or at 2 s whatever
      // moves. It runs on the page's own clock: a wait in this process lets the next key go out
      // while that timer is still queued, and a busy page runs a queued key before a timer
      // already due, while a timer the page sets after ProseMirror's runs after it, so the key
      // is sent only once the write is made.
      await page.evaluate(
        ({ boundMs, quietMs }) => {
          const waited = Promise.withResolvers<void>();
          let quiet = 0;
          const end = () => {
            window.clearTimeout(quiet);
            window.clearTimeout(bound);
            document.removeEventListener("focusin", restart, true);
            waited.resolve();
          };
          const restart = () => {
            window.clearTimeout(quiet);
            quiet = window.setTimeout(end, quietMs);
          };
          const bound = window.setTimeout(end, boundMs);
          document.addEventListener("focusin", restart, true);
          restart();
          return waited.promise;
        },
        { boundMs: 2_000, quietMs: 50 }
      );
    };
    /** One edit through the row `focus` focuses: Enter, `keys` pressed, `paste` pasted as a
     *  browser sends a paste, a `Y` typed and Escape. The document `artifactId` then stores
     *  `edited` in the place of `words`, its words before, and nothing else changed; `undo`,
     *  after Enter, puts the words back. */
    const editThroughRow = async (edit: {
      artifactId: string;
      blockId: string;
      edited: string;
      focus: () => Promise<void>;
      keys: readonly string[];
      name: string;
      paste?: string;
      undo: () => Promise<void>;
      words: string;
    }) => {
      const before = await storedMarkdown(edit.artifactId);
      const at = before.indexOf(edit.words);
      expect(
        at > -1 && before.lastIndexOf(edit.words) === at,
        `the stored document does not hold "${edit.words}" once`
      ).toBe(true);
      await edit.focus();
      await enter(edit.blockId);
      for (const key of edit.keys) await page.keyboard.press(key);
      if (edit.paste !== undefined) {
        await page.evaluate((text) => {
          const clipboardData = new DataTransfer();
          clipboardData.setData("text/plain", text);
          document.activeElement?.dispatchEvent(
            new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData })
          );
        }, edit.paste);
      }
      await page.keyboard.type("Y");
      await page.keyboard.press("Escape");
      await expect
        .poll(() => storedMarkdown(edit.artifactId), { message: `${edit.name}: no Y was stored` })
        .not.toBe(before);
      const after = await storedMarkdown(edit.artifactId);
      expect(
        after,
        `${edit.name} then Y edited other than the block's words: ${changeIn(before, after)}`
      ).toBe(before.slice(0, at) + edit.edited + before.slice(at + edit.words.length));
      await edit.focus();
      await enter(edit.blockId);
      await edit.undo();
      await page.keyboard.press("Escape");
      await expect
        .poll(() => storedMarkdown(edit.artifactId), {
          message: `${edit.name}: the block was not put back`,
        })
        .toBe(before);
    };
    const inStatement = {
      artifactId: seeded.specArtifactId,
      blockId: statementBlock,
      focus: async () => {
        await focusRow(page, statement);
      },
      undo: () => page.keyboard.press("Delete"),
      words,
    };

    await editThroughRow({
      ...inStatement,
      edited: `Y${words}`,
      keys: ["ArrowUp"],
      name: "Up from the statement's first line",
    });
    await editThroughRow({
      ...inStatement,
      edited: `Y${words}`,
      keys: ["ArrowDown"],
      name: "Down from the statement's last line",
    });
    await editThroughRow({
      ...inStatement,
      edited: "Y",
      keys: ["Control+a"],
      name: "select-all",
      undo: async () => {
        await page.keyboard.press("Control+a");
        await page.keyboard.type(words);
      },
    });

    // Edit the agent's decision under the answered question, not the question that shaped a statement.
    const inHeading = {
      artifactId: seeded.specArtifactId,
      blockId: decision,
      focus: async () => {
        await unfoldEveryBranch(page);
        await focusRow(page, blockNodeId(specSlug, decision));
      },
      undo: async () => {
        await page.keyboard.press("ArrowRight");
        await page.keyboard.press("Backspace");
      },
      words: heading,
    };
    await editThroughRow({
      ...inHeading,
      edited: `Y${heading}`,
      keys: ["Control+Home"],
      name: "Ctrl+Home",
    });
    await editThroughRow({
      ...inHeading,
      edited: `${closing}Y`,
      keys: ["Control+End"],
      name: "Ctrl+End",
      undo: async () => {
        await page.keyboard.press("Control+End");
        await page.keyboard.press("Backspace");
      },
      words: closing,
    });
    await editThroughRow({
      ...inHeading,
      edited: `Y${heading}`,
      keys: ["Backspace", "Backspace", "Backspace"],
      name: "Backspace pressed three times at the heading's start",
    });
    await editThroughRow({
      ...inHeading,
      edited: `Y${heading}`,
      keys: [],
      name: "a paste of two lines at the heading's start",
      paste: "Two\nlines",
    });
  } finally {
    await context.close();
  }
});

// The row's editor is the document's whole editor, and Tab, which Milkdown answers in a list item
// alone, was the browser's: from the caret in a statement it moved the focus on to the next
// control the editor draws - driving a preview, an ask another block draws - and the editor's box
// scrolled to it, away from the statement. Tab from the row's editor goes past every control the
// editor draws outside the node's block to the first tab stop after the editor's box, and
// Shift+Tab to the row, the outline's one tab stop; each ends the editing, as a Tab out of the
// outline and Escape do, and edits nothing. The seeded spec holds the open question's `ask` block
// after its statements, and the ask's card draws controls of its own inside the editor. The
// statement is the third, which no comment or ask is on, so the editor draws no references
// button of its own block (the next row reads one), and the second's, which the seed's anchored
// comment draws, is another block's, passed over too. The first tab stop after the outline is the
// field of the box at the foot, so Tab goes there.
// SLOW(>=2s): the whole issue seeded and loaded, then the row's editor opened twice, each waited
// on until it takes the caret and the ask's card is drawn in it, and until it closes again.
test("Tab from a row's editor passes over the controls of an ask another block draws, Shift+Tab goes to the row, and each ends the editing", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const { context, page, seeded } = await openTree(browser);
  try {
    await expectTreeDrawn(page);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statementBlock = requirementBlockId(3);
    const statement = blockNodeId(specSlug, statementBlock);
    const before = await storedMarkdown(seeded.specArtifactId);
    const row = nodeRow(page, statement).first();
    const editable = row.locator('[contenteditable="true"]');
    // The tab stops the ask's card draws in the row's editor.
    const askControls = row
      .locator(`[data-block-id="${OPEN_QUESTION_BLOCK_ID}"]`)
      .locator('a[href], button, input, select, textarea, [tabindex]:not([tabindex="-1"])');
    /** Where the focus is: in an editor's box, in the ask's block, and its element - its name and
     *  the block it stands in - for a failure to name. */
    const focusAt = () =>
      page.evaluate((ask) => {
        const active = document.activeElement;
        if (active === null || active === document.body) {
          return { inAsk: false, inBox: false, name: "the page itself" };
        }
        const words = active.getAttribute("aria-label") ?? active.textContent?.slice(0, 40);
        const block = active.closest("[data-block-id]")?.getAttribute("data-block-id");
        const where = typeof block === "string" ? ` of block ${block}` : "";
        return {
          inAsk: active.closest(`[data-block-id="${ask}"]`) !== null,
          inBox: active.closest("[data-editor-box]") !== null,
          name: `${active.tagName.toLowerCase()} "${words}"${where}`,
        };
      }, OPEN_QUESTION_BLOCK_ID);
    /** Enter on the statement's row, the caret in its block and the ask's card drawn in the
     *  editor. */
    const enter = async () => {
      await focusRow(page, statement);
      await page.keyboard.press("Enter");
      await expect
        .poll(() => caretInRowBlock(page, statementBlock), {
          message: "Enter put no caret in the statement",
        })
        .toBe(true);
      await expect
        .poll(() => askControls.count(), {
          message: "the ask's block draws no control in the row's editor",
        })
        .toBeGreaterThan(0);
    };

    await enter();
    await page.keyboard.press("Tab");
    const afterTab = await focusAt();
    expect(
      afterTab.inBox,
      `Tab from the row's editor left the focus in an editor's box, on ${afterTab.name}`
    ).toBe(false);
    expect(afterTab.inAsk, `Tab stopped on the ask's ${afterTab.name}`).toBe(false);
    await expect(
      composerField(page),
      `Tab from the row's editor went to ${afterTab.name}, not the first tab stop after its box`
    ).toBeFocused();
    await expect(editable, "Tab from the row's editor did not end the editing").toHaveCount(0);
    expect(await caretInNode(page), "a caret stayed in a row after Tab").toBe(false);

    await enter();
    await page.keyboard.press("Shift+Tab");
    await expect(row, "Shift+Tab from the row's editor did not go to the row").toBeFocused();
    await expect(editable, "Shift+Tab from the row's editor did not end the editing").toHaveCount(
      0
    );
    await expect(row, "the row lost the focus as the editing ended").toBeFocused();
    expect(
      await storedMarkdown(seeded.specArtifactId),
      "Tab or Shift+Tab from the row's editor changed the spec"
    ).toBe(before);
  } finally {
    await context.close();
  }
});

// A document draws a button for each block with comments or asks on it, which shows them and puts
// the reader in that block, in an aside of its own beside the editor, outside every block
// (`ProofDocument.tsx`'s "Block references"). The node's own block's button is the node's: Tab
// from the row's editor stops at it, as the browser's own Tab did, and the editing stays; Tab from
// it goes on past every other block's button to the first tab stop after the editor's box, and
// ends the editing. The seed anchors a comment on the second statement, and the row anchors one on
// the first and one on the third, so another block's button stands before the statement's and one
// after it. A comment's quote is anchored to the innermost block that holds it - the paragraph
// inside a statement's typed block, by the id the editor gave it - so each button names a block
// inside its statement's (`data-block-references`). The buttons are found by their names, and the
// one Tab reaches is read by the statement whose block holds the block it names.
// SLOW(>=2s): the whole issue seeded with two more anchored comments and loaded, then the row's
// editor opened and waited on until it takes the caret and draws the references buttons.
test("Tab from a row's editor stops at the references button of the node's own block, and at no other block's", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(
    browser,
    async () => {
      const seeded = await seedWholeIssue();
      for (const index of [1, 3]) {
        await createComment(seeded.key, {
          anchor: { artifact: "spec", quote: requirementText(index) },
          body: `A comment on requirement ${index}, so its block draws a references button.`,
        });
      }
      return seeded;
    },
    async (page, seeded) => {
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectOpeningLanded(page);
      await expectTreeDrawn(page);
      const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
      const statementBlock = requirementBlockId(2);
      const statement = blockNodeId(specSlug, statementBlock);
      const row = nodeRow(page, statement).first();
      const editable = row.locator('[contenteditable="true"]');
      const references = row
        .locator("[data-editor-box]")
        .getByRole("button", { name: /references on block$/ });

      await focusRow(page, statement);
      await page.keyboard.press("Enter");
      await expect
        .poll(() => caretInRowBlock(page, statementBlock), {
          message: "Enter put no caret in the statement",
        })
        .toBe(true);
      await expect
        .poll(() => references.count(), {
          message: "the row's editor draws no references button for each statement commented on",
        })
        .toBeGreaterThanOrEqual(3);
      await page.keyboard.press("Tab");
      const reached = await page.evaluate((block) => {
        const active = document.activeElement;
        const named = active?.getAttribute("data-block-references") ?? null;
        const editable = active
          ?.closest("[data-editor-box]")
          ?.querySelector('[contenteditable="true"]');
        const drawn =
          named === null ? null : editable?.querySelector(`[data-block-id="${CSS.escape(named)}"]`);
        return {
          inStatement: (drawn?.closest(`[data-block-id="${block}"]`) ?? null) !== null,
          name: active?.getAttribute("aria-label") ?? active?.textContent?.slice(0, 40) ?? "",
          named,
        };
      }, statementBlock);
      expect(
        reached.inStatement,
        `Tab from the row's editor went to "${reached.name}", naming block ${reached.named}, not the statement's references button`
      ).toBe(true);
      await expect(editable, "the editing ended at the statement's references button").toHaveCount(
        1
      );
      await page.keyboard.press("Tab");
      await expect(
        composerField(page),
        "Tab from the statement's references button did not go past every other block's to the first tab stop after the editor's box"
      ).toBeFocused();
      await expect(editable, "Tab out of the editor's box did not end the editing").toHaveCount(0);
    }
  );
});
