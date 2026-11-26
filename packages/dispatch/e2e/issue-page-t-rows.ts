import { type Browser, expect, type Page } from "@playwright/test";

import type {
  Actor,
  DispatchEvent,
  IssueDetails,
  IssueTreeNode,
  IssueTreeRead,
} from "../web/src/api/types";
import { decisionNodeChanges } from "../web/src/features/issue-page/decision-review";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { shortDay } from "../web/src/features/issue-page/node-time";
import {
  CONTROL_LABELS,
  coverageNotes,
  NODE_STATE_WORDS,
  PRESET_NAMES,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { type TreeIdReads, treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { getIssue, getIssueCoverage, getIssueEvents, listComments, listIssueAsks } from "./api";
import { seedSpineCases, seedWholeIssue } from "./issue-layout-seed";
import { gotoIssue, phase, presetCount, presetToggle, presetToggles } from "./issue-page";
import { expectIssuePageDrawn } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import {
  drawnRowFaces,
  drawnRowReadings,
  fixtureSeed,
  type KeyedRow,
  keyedRows,
  type NamedSeed,
  parentIndex,
  type RowReading,
  SEEDS,
  type SeededIssue,
  type SeedFixture,
  seedKeys,
  sortedFields,
  steadily,
} from "./issue-page-t";
import {
  convertedGate,
  type DecisionsRead,
  type DrawnNode,
  decisionNodesGate,
  decisionsAreNodes,
  decisionsReadOf,
  decisionTimes,
  drawnNodes,
  heldAnswer,
  type OutlineKind,
  outlineAnswerOf,
  owedUnless,
  R2_ROW_KINDS,
  seedEventsLanded,
  type TreeView,
  treeAnswerOf,
  treeReadGate,
} from "./issue-page-t-tree";
import { isLiveSocket } from "./live-socket-watch";
import { type ApiRequest, type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * What the rows of the new issue page that draw it from the tree read share: the outline drawn
 * from the issue's tree read (`GET /api/v1/issues/{key}/tree`) and each filter from that read's
 * view (`?view=`), read on the real stack. Each row turns the new page on for alice with `phase`
 * (1440 by 900), seeds one of the issues of `e2e/issue-layout-seed.ts`, opens it with
 * `gotoIssue`, and reads it through `pickPreset`, `watchApiRequests` and `expectLoadSettled`
 * (`e2e/traffic.ts`), the helpers of `e2e/issue-page-t.ts` and `e2e/issue-page-t-tree.ts`, and
 * the steps and comparisons of this file.
 *
 * The rows are the four of the Verification document's browser checks, by their titles. They
 * stand in four spec files that hold only their rows, each about a minute of one browser shard
 * of CI, since one file of them all took most of a shard's limit:
 *
 * 1. `c-same-rows`: "the outline drawn from the tree read holds the rows the reads drew", one row
 *    per seed, so the check's `-g` runs all 14 and each ends inside CI's nine minutes:
 *    `e2e/issue-page-t-same-rows-a.e2e.ts` holds the first nine of `SEEDS` in their order
 *    (`SAME_ROWS_A_SEEDS`) and `e2e/issue-page-t-same-rows-b.e2e.ts` the last five
 *    (`SAME_ROWS_B_SEEDS`), a split by time (`SAME_ROWS_A`). It reads Graph (`/graph`) as it
 *    opens, every node unfolded and each row's face drawn (R11), with no key pressed. Each row
 *    stands under the node the answer names as its parent, or its nearest drawn ancestor, in the
 *    answer's order; that is read from the answer and never from the fixture, so the check's
 *    control fails on the parent first. Every other reading is the fixture's
 *    (`e2e/fixtures/issue-page-t-rows.json`) with every branch open, by key, and each time is its
 *    source's in this run.
 * 2. `c-tree-requests`: "the first load reads the tree once and a filter its view once", and Plan,
 *    pressed 31 s after the load, past the app's 30 s `staleTime`, sends no read but its view's:
 *    the page's reads are refreshed by events, never by age. It is in
 *    `e2e/issue-page-t-reads.e2e.ts`.
 * 3. `c-views`: "each filter draws its view read's rows", on `seedWholeIssue` and
 *    `seedSpineCases`, in `e2e/issue-page-t-reads.e2e.ts` too.
 * 4. `c-fresh`: "a write is drawn without a reload": a status picked in the page's header sends
 *    one or two tree reads (its own refresh, and its event's), and a reply sent from the row's
 *    Reply on the open question becomes a row under it and sends one or two. It is in
 *    `e2e/issue-page-t-write.e2e.ts`.
 *
 * Rows 5 to 14 are the root spec's checks of the new page, each in a file of its topic:
 * `c-conversation-read` (row 5, `e2e/issue-page-t-conversation.e2e.ts`), `c-graph-view` (row 6,
 * `e2e/issue-page-t-graph.e2e.ts`), `c-row-controls` (row 7) and `c-dashboard-reads` (row 11, in
 * `e2e/issue-page-t-views.e2e.ts`), `c-contents-beside` (row 8,
 * `e2e/issue-page-t-contents.e2e.ts`), `c-children-section` (row 9,
 * `e2e/issue-page-t-children.e2e.ts`), `c-pages` (row 10, `e2e/issue-page-t-pages.e2e.ts`), and
 * `c-row-link` (row 12), `c-ask-body` (row 13) and `c-row-face` (row 14, in
 * `e2e/issue-page-t-face.e2e.ts`). Each reads an outline as it opens (`openOutline`) against the
 * answer it was drawn from: its rows are the answer's nodes but those no row draws
 * (`nodesNoRowDraws`, `answerRowIds`), each in its place (`firstRowOutOfPlace`,
 * `servingOutOfPlace`), none folded (`foldedRowIds`) and each whole (`rowsNotWhole`). Rows 11 and
 * 13 wait on Dashboard's part (`dashboardPartGate`).
 *
 * Each row checks its gates first, each with one `owedUnless` call, and skips where a gate is not
 * on this branch's base, with an `owed` annotation naming the gate; it never passes then, since a
 * row cannot pass on code the base does not hold yet. Rows 1 to 4 wait on the model's tree read,
 * which `treeReadGate` reads as the route's 404; row 1 on issues born converted too
 * (`convertedGate`), the answer's `converted: true`; row 4's reply waits on the line at the foot
 * of the outline, which is not on this branch's base (`footLineGate`). These rows carry no
 * `@owed`: each runs for real once its gates are on the base. Each clause a later gate holds is a
 * row of its own, titled its row's title and the clause and tagged `@owed`, which checks that gate
 * the same way and asserts the clause once it is met: row 1's decision clauses, for each seed that
 * holds a decision (`decisionNodesGate`); row 2's Decisions (`decisionsViewGate`); row 3's
 * Decisions (`decisionsViewGate`) and its decision lines (`partUGate`); row 4's Keep
 * (`decisionsViewGate` and `partUGate`). Each such row stands in its row's file.
 *
 * These helpers name the tree read's modules and its answer's types, so they are not in
 * `e2e/issue-page-t.ts`, which the recorder loads on a base without the tree read. They are not
 * in `e2e/issue-page-t-tree.ts` either: this file reads the fixture as it loads
 * (`DECISION_SEEDS`), which the other specs that import that file for `refuseTreeRead` need not.
 */

/** The seeds whose rows the fixture records a decision row of: row 1's decision clauses read
 *  them (`decisionNodesGate`). */
export const DECISION_SEEDS = new Set(
  SEEDS.flatMap(([name]) =>
    fixtureSeed(name).rows.some((row) => row.kind.type === "decision") ? [name] : []
  )
);

/**
 * How many of `SEEDS`, in their order, row 1 reads in `e2e/issue-page-t-same-rows-a.e2e.ts`; the
 * rest, from `seedTwiceTheNodes` on, are in `e2e/issue-page-t-same-rows-b.e2e.ts`. The split is
 * by time rather than by count, since the later seeds are the larger issues: in one chromium run
 * with one worker, the first nine seeds took 72 s and the last five 71 s, their decision clauses
 * included, where the first seven took 53 s and the last seven 90 s.
 */
const SAME_ROWS_A = 9;

/** The seeds of row 1 in `e2e/issue-page-t-same-rows-a.e2e.ts`. */
export const SAME_ROWS_A_SEEDS: readonly NamedSeed[] = SEEDS.slice(0, SAME_ROWS_A);

/** The seeds of row 1 in `e2e/issue-page-t-same-rows-b.e2e.ts`. */
export const SAME_ROWS_B_SEEDS: readonly NamedSeed[] = SEEDS.slice(SAME_ROWS_A);

/** The filters that read a view of the tree today: Decisions reads one once the decisions view
 *  is on the base (`decisionsViewGate`). */
export const VIEW_FILTERS = ["spec", "verification", "plan"] as const satisfies readonly TreeView[];

/** The four filters, in the view row's order: each draws its count, and each reads a view of
 *  the tree once that view is on the base (`VIEW_FILTERS`). */
export const FILTERS = [
  "spec",
  "verification",
  "plan",
  "decisions",
] as const satisfies readonly TreeView[];

/** The views the new page draws from the tree read, in the view row's order: Conversation, the
 *  four filters and Graph. The row holds a control for each, whatever view of another part it
 *  holds beside them (spec R12). */
export const PAGE_VIEWS = [
  "conversation",
  ...FILTERS,
  "graph",
] as const satisfies readonly Preset[];

/** Each control of the view row, in the order the row draws them: the name it draws
 *  (`[data-preset-name]`) and whether it is pressed. */
export function presetControls(
  page: Page
): Promise<{ readonly name: string; readonly pressed: boolean }[]> {
  return presetToggles(page).evaluateAll((buttons) =>
    buttons.map((button) => ({
      name: (button.querySelector("[data-preset-name]")?.textContent ?? "").trim(),
      pressed: button.matches('[aria-pressed="true"]'),
    }))
  );
}

/** The address `view` has on issue `key`, at the end of the page's URL. */
export function addressOf(key: string, view: Preset): RegExp {
  return new RegExp(`${presetPath(key, view)}$`);
}

/** How long one seed's row 1 may take: every row of the outline read with its body, inside CI's
 *  nine minutes. */
export const SAME_ROWS_MS = 480_000;

/** How long each other row may take. */
export const ROW_MS = 240_000;

/** The words row 4 replies with. */
export const REPLY_WORDS = "The tree read draws this reply with no reload.";

/** A decision row's state word once alice kept it. */
export const KEPT_BY_ALICE = new RegExp(`^${NODE_STATE_WORDS.decision.kept}\\b.*\\balice\\b`, "i");

/** The reads `treeIdOf` names a node by: the issue read and the asks read, as alice reads them. */
export async function idReadsOf(key: string): Promise<TreeIdReads> {
  const [issue, asks] = await Promise.all([getIssue(key), listIssueAsks(key)]);
  return { asks, issue };
}

/** The slug of each document of issue `key`, by the document's id, as the issue read answers it:
 *  `""` for an id the issue holds no document of. */
export async function artifactSlugs(key: string): Promise<(id: string) => string> {
  const artifacts = (await getIssue(key)).artifacts ?? [];
  return (id) => artifacts.find((artifact) => artifact.id === id)?.slug ?? "";
}

/** The ids, each once and sorted, so two lists of drawn ids compare as sets. */
export function idSet(ids: readonly string[]): string[] {
  return [...new Set(ids)].sort();
}

/** The id of the row the row `index` of `rows` stands under (`parentIndex`), null at the top. */
function parentIdOf(rows: readonly RowReading[], index: number): string | null {
  const parent = parentIndex(rows, index);
  return parent === undefined ? null : (rows[parent]?.id ?? null);
}

/** `value` as JSON with every object's fields sorted (`sortedFields`), so two readings compare by
 *  what they hold and not by the order their fields were written in. */
function canonical(value: unknown): string {
  return JSON.stringify(sortedFields(value)) ?? "undefined";
}

// ---------------------------------------------------------------------------------------------
// Row 1: the outline from the tree read.
// ---------------------------------------------------------------------------------------------

/** What a row's place is read against: the answer the page drew its rows from, and the reads
 *  `treeIdOf` names the answer's nodes by. */
export interface DrawnFrom {
  readonly answer: IssueTreeRead;
  readonly reads: TreeIdReads;
}

/** What row 1 reads of one seed's page. */
interface OutlineRead extends DrawnFrom {
  /** The ids of the comments the comments read anchors to a node's text. */
  readonly anchored: ReadonlySet<string>;
  readonly decisions: DecisionsRead | undefined;
  readonly keys: ReadonlyMap<string, string>;
  readonly rows: readonly RowReading[];
  readonly slugs: readonly string[];
}

/** How many presses `drawEveryPage` makes at most: far more pages of children than any seed
 *  holds, so a control that a press never takes away fails the row here. */
const PAGES_DRAWN_AT_MOST = 40;

/**
 * Presses each control that draws the next page of a node's children (`data-tree-page`), until
 * none is left. An outline draws a node's first 50 children and, after them, a control that says
 * how many are left and draws the next 50 on the reader's press (R15); a page is no fold, so the
 * rows it holds back are rows Graph holds, and row 1 reads them with every page drawn.
 */
export async function drawEveryPage(page: Page): Promise<void> {
  const control = page.locator("[data-issue-page] [data-tree-page] button").first();
  const rows = page.locator("[data-issue-page] [data-node-row]");
  for (let pressed = 0; pressed < PAGES_DRAWN_AT_MOST; pressed += 1) {
    // The outline is settled on entry, and each press below waits until its rows are drawn.
    if ((await control.count()) === 0) return;
    const before = await rows.count();
    await control.click();
    await expect
      .poll(() => rows.count(), { message: "a press of a page's control drew no more rows" })
      .toBeGreaterThan(before);
  }
  await expect(control, "a page of children is still held back after every press").toHaveCount(0);
}

/** Opens the seeded issue on Graph (`/graph`, R10) and reads every row as Graph opens, with no
 *  key pressed and every page of children drawn (`drawEveryPage`), once two readings agree, each
 *  with the body its face draws (`drawnRowFaces`): Graph opens with every node unfolded and each
 *  row's face drawn whatever the focus (R11). And what the page received of the tree read,
 *  Graph's read, and of the decisions read, and which comments the comments read anchors to
 *  text. */
async function readOutline(page: Page, seeded: SeededIssue): Promise<OutlineRead> {
  const tree = treeAnswerOf(page);
  const decisions = decisionsReadOf(page, seeded.key);
  const traffic = watchApiRequests(page);
  await gotoIssue(page, presetPath(seeded.key, "graph"));
  await expectIssuePageDrawn(page);
  const rows = await steadily(page, traffic, `${seeded.key} as Graph opens`, async () => {
    await drawEveryPage(page);
    return drawnRowFaces(page);
  });
  const answer = await heldAnswer(tree, `${seeded.key}'s tree read`);
  const [reads, anchored] = await Promise.all([
    idReadsOf(seeded.key),
    anchoredComments(seeded.key),
  ]);
  const slugs = (reads.issue.artifacts ?? []).map((artifact) => artifact.slug);
  return {
    anchored,
    answer,
    decisions: decisions.read(),
    keys: seedKeys(seeded, slugs),
    reads,
    rows,
    slugs,
  };
}

/** The ids of the comments the comments read anchors to a node's text, of issue `key`. */
export async function anchoredComments(key: string): Promise<ReadonlySet<string>> {
  const comments = await listComments(key);
  return new Set(comments.flatMap((comment) => (comment.anchor === null ? [] : [comment.id])));
}

/**
 * The read ids of the answer's comments on text, which R1 (INFRA-5706, decision 474) draws as no
 * row of any outline, since the margin beside each comment's node draws it (root R41): every
 * comment anchored in a block or on a node (`anchored`, or carrying a tree-read `anchor`), whether
 * the answer hangs it under a drawn node or under the issue, and each comment the answer hangs
 * under such a comment, a reply in its thread.
 */
function textComments(answer: IssueTreeRead, anchored: ReadonlySet<string>): ReadonlySet<string> {
  const nodes = nodesById(answer);
  const onText = (node: IssueTreeNode | undefined, passed: Set<string>): boolean => {
    if (node?.supertag !== "comment" || passed.has(node.id)) return false;
    passed.add(node.id);
    if (anchored.has(node.id) || node.anchor !== undefined) return true;
    if (node.parent === null) return false;
    return node.parent.supertag === "comment" && onText(nodes.get(node.parent.id), passed);
  };
  return new Set(
    (answer.nodes ?? []).flatMap((node) => (onText(node, new Set()) ? [node.id] : []))
  );
}

/** The keys of the fixture's rows that are comments on a node's text (`textComments`). */
function textCommentKeys(outline: OutlineRead): ReadonlySet<string> {
  const onText = textComments(outline.answer, outline.anchored);
  const keys = new Set<string>();
  for (const [id, key] of outline.keys) {
    if (onText.has(id)) keys.add(key);
  }
  return keys;
}

/** A node of the answer by the id its row carries: of the nodes the tree draws by one id, the
 *  first the answer holds, with its place in the answer's order. */
interface HeldNode {
  readonly node: IssueTreeNode;
  readonly order: number;
}

/** The answer's nodes, by their read ids. */
export function nodesById(answer: IssueTreeRead): Map<string, IssueTreeNode> {
  return new Map((answer.nodes ?? []).map((node) => [node.id, node]));
}

function heldNodes(answer: IssueTreeRead, idOf: (readId: string) => string): Map<string, HeldNode> {
  const held = new Map<string, HeldNode>();
  for (const [order, node] of (answer.nodes ?? []).entries()) {
    const id = idOf(node.id);
    if (!held.has(id)) held.set(id, { node, order });
  }
  return held;
}

/** The rows that take their place from the fixture rather than from the tree read: a row the
 *  answer holds no node for, which is a decision of today's decisions read (until the page draws
 *  decisions from the tree read's nodes), and a row drawn under such a decision, a Change, which
 *  the read places under its decision only once the decision is a node (`decisionNodesGate`). */
function placedByFixture(
  readings: readonly RowReading[],
  held: ReadonlyMap<string, HeldNode>
): Set<number> {
  const placed = new Set<number>();
  for (const [index, reading] of readings.entries()) {
    if (reading.id === null || !held.has(reading.id)) {
      placed.add(index);
      continue;
    }
    const parent = parentIndex(readings, index);
    if (parent !== undefined && placed.has(parent) && readings[parent]?.kind.type === "decision") {
      placed.add(index);
    }
  }
  return placed;
}

/** Where the tree read places `node`'s rows, by the id of the row each stands under (null for
 *  the top, under the issue): its `parent`, walked up the answer to the nearest node `drawn` as a
 *  row; for a check or a plan task of this issue also each statement its `serves` tuples name,
 *  where the outline stands it again (root R4), and there alone where the walk reaches the
 *  issue. */
function homesOf(
  node: IssueTreeNode,
  outline: DrawnFrom,
  idOf: (readId: string) => string,
  drawn: ReadonlySet<string>
): ReadonlySet<string | null> {
  const key = outline.reads.issue.key;
  const nodes = new Map((outline.answer.nodes ?? []).map((one) => [one.id, one]));
  const foreign = (one: IssueTreeNode): boolean => one.issue !== undefined && one.issue !== key;
  let parent: string | null = null;
  const passed = new Set<string>();
  for (
    let above = node.parent?.id ?? null;
    above !== null && !passed.has(above);
    above = nodes.get(above)?.parent?.id ?? null
  ) {
    passed.add(above);
    const id = idOf(above);
    if (id === key) break;
    if (drawn.has(id)) {
      parent = id;
      break;
    }
  }
  if ((node.supertag !== "check" && node.supertag !== "plan_task") || foreign(node)) {
    return new Set([parent]);
  }
  const served = (outline.answer.tuples ?? []).flatMap((tuple) => {
    const target =
      tuple.kind === "serves" && tuple.from.id === node.id ? nodes.get(tuple.to.id) : undefined;
    return target?.supertag === "requirement" && !foreign(target) ? [idOf(target.id)] : [];
  });
  return new Set(parent === null && served.length > 0 ? served : [parent, ...served]);
}

/** The first row of `readings` the answer holds that does not stand under the node the answer
 *  places it under, at the level that gives, or stands before a sibling the answer orders before
 *  it; undefined where none. A row the answer holds no node for is read by row 1 against the
 *  fixture, and by every other row against the ids the answer holds (`answerRowIds`). */
export function firstRowOutOfPlace(
  where: string,
  readings: readonly RowReading[],
  outline: DrawnFrom
): string | undefined {
  const idOf = treeIdOf(outline.answer, outline.reads);
  const held = heldNodes(outline.answer, idOf);
  const placed = placedByFixture(readings, held);
  const drawn = new Set(readings.flatMap((reading) => (reading.id === null ? [] : [reading.id])));
  const last = new Map<number, { readonly id: string; readonly order: number }>();
  for (const [index, reading] of readings.entries()) {
    const own = reading.id === null ? undefined : held.get(reading.id);
    if (own === undefined || reading.id === null || placed.has(index)) continue;
    const parent = parentIndex(readings, index);
    const under = parentIdOf(readings, index);
    const homes = homesOf(own.node, outline, idOf, drawn);
    const named = `the ${reading.kind.type ?? "untyped"} row ${reading.id}`;
    if (!homes.has(under) || (parent === undefined && reading.level !== 1)) {
      const answered = [...homes].map((home) => home ?? "the issue").join(" or ");
      return (
        `${where}, ${named} stands at level ${reading.level ?? "none"} under ` +
        `${under ?? "the issue"}, and the tree read places it under ${answered}`
      );
    }
    const before = last.get(parent ?? -1);
    if (before !== undefined && before.order > own.order) {
      return (
        `${where}, ${named} is drawn under ${under ?? "the issue"} after ${before.id}, which ` +
        "the tree read orders after it"
      );
    }
    last.set(parent ?? -1, { id: reading.id, order: own.order });
  }
  return undefined;
}

/** What row 1 compares of a row with the fixture: every reading but its place. */
const READINGS = [
  "words",
  "kind",
  "author",
  "time",
  "state",
  "chips",
  "gaps",
  "facts",
  "counts",
  "alsoUnder",
  "body",
] as const satisfies readonly (keyof KeyedRow)[];

/** What the fixture compares of a row that takes its place from it (`placedByFixture`). */
const PLACE = ["parentKey", "level"] as const satisfies readonly (keyof KeyedRow)[];

/**
 * What row 1 compares of `row`'s `field`, for a row of the page and of the fixture alike. The
 * fixture holds the page that built its tree from the old reads, which drew a node's text whole,
 * with its author line, its facts, its gap sentences and its counts, in the focused row's body
 * alone, its line's words cut; the page draws every node's text whole on its row and the rest on
 * its face, whatever the focus (R11), and its reading takes each row's body from that face
 * (`drawnRowFaces`). So a row's words are the words its body drew wherever the reading holds a
 * body, and its body is what that body drew beside them: its author line, its other lines, and an
 * ask's options and answer. An answer's white space is the page's to lay out: the old page drew
 * the stored text as it stands and the page draws each paragraph as a block of its own, so its
 * words are compared with each run of white space read as one space. A child issue's row is its
 * key, its title and its status (R14), so its face draws no author line where the old page's body
 * drew "Author unknown": a child's body is compared without it.
 */
function compared(row: KeyedRow, field: keyof KeyedRow): unknown {
  if (field === "words") return row.body?.words ?? row.words;
  if (field !== "body" || row.body === null) return row[field];
  const { author, lines, options } = row.body;
  const answer =
    row.body.answer === null
      ? null
      : { ...row.body.answer, text: row.body.answer.text?.replace(/\s+/g, " ").trim() ?? null };
  return row.kind.type === "child_issue"
    ? { answer, lines, options }
    : { answer, author, lines, options };
}

/** Whether `row` of the fixture is a section that restates a person's answer: a decision the
 *  fixture draws under an answered question. */
function restatesAnAnswer(row: KeyedRow, recorded: readonly KeyedRow[]): boolean {
  return (
    row.kind.type === "decision" &&
    recorded.some(
      (other) =>
        other.key === row.parentKey &&
        other.kind.type === "ask" &&
        other.state?.token === "answered"
    )
  );
}

function rowName(row: KeyedRow): string {
  return `${row.key} under ${row.parentKey ?? "the issue"}`;
}

/**
 * How `readings` differ from the fixture's rows `recorded`, each row matched by its key (and, for
 * a node drawn under two parents, by its parent's key first): every reading but its place, and
 * the place too for a row the fixture places (`placedByFixture`); then each time against its
 * source in this run. A comment on a node's text, which R2 lets Graph draw as no row since the
 * margin beside that node draws it, is no row the fixture holds the page must draw
 * (`textCommentKeys`). Once the seeds' decisions are nodes (`nodes`, `decisionNodesGate`), the two
 * differences the check names are allowed too: the section that restates a person's answer is no
 * row, and the answered question's row carries its `shaped` ties as chips beside the fixture's.
 */
function differences(
  where: string,
  readings: readonly RowReading[],
  recorded: readonly KeyedRow[],
  outline: OutlineRead,
  nodes: boolean
): string[] {
  const idOf = treeIdOf(outline.answer, outline.reads);
  const held = heldNodes(outline.answer, idOf);
  const placed = placedByFixture(readings, held);
  const drawn = keyedRows(readings, outline.keys);
  const unmatched = new Map<string, KeyedRow[]>();
  for (const row of recorded) unmatched.set(row.key, [...(unmatched.get(row.key) ?? []), row]);
  const found: string[] = [];
  for (const [index, row] of drawn.entries()) {
    const candidates = unmatched.get(row.key) ?? [];
    const at = Math.max(
      0,
      candidates.findIndex((candidate) => candidate.parentKey === row.parentKey)
    );
    const match = candidates[at];
    if (match === undefined) {
      found.push(`${where}, ${rowName(row)} is a row the fixture does not hold`);
      continue;
    }
    candidates.splice(at, 1);
    const fields: readonly (keyof KeyedRow)[] = placed.has(index)
      ? [...READINGS, ...PLACE]
      : READINGS;
    for (const field of fields) {
      const theirs = compared(match, field);
      const mine =
        field === "chips" && nodes && row.kind.type === "ask" && row.state?.token === "answered"
          ? row.chips.filter((chip) =>
              match.chips.some((kept) => canonical(kept) === canonical(chip))
            )
          : compared(row, field);
      if (canonical(mine) !== canonical(theirs)) {
        found.push(
          `${where}, ${rowName(row)} draws its ${field} as ${canonical(mine)}, and the fixture ` +
            `as ${canonical(theirs)}`
        );
      }
    }
  }
  const onText = textCommentKeys(outline);
  for (const rows of unmatched.values()) {
    for (const row of rows) {
      if (onText.has(row.key) || (nodes && restatesAnAnswer(row, recorded))) continue;
      found.push(`${where}, ${rowName(row)} is a row of the fixture the page does not draw`);
    }
  }
  return [...found, ...timeDifferences(where, readings, drawn, outline, held)];
}

/** Each drawn time that is not its source's in this run, as the page's short day: a node's
 *  `made_at` in the tree read the page received, a decision's `changed_at` in the decisions read
 *  the page received. */
function timeDifferences(
  where: string,
  readings: readonly RowReading[],
  drawn: readonly KeyedRow[],
  outline: OutlineRead,
  held: ReadonlyMap<string, HeldNode>
): string[] {
  const decided = decisionTimes(outline.decisions, outline.slugs);
  return readings.flatMap((reading, index) => {
    const row = drawn[index];
    if (reading.time === null || row === undefined) return [];
    const decision = reading.kind.type === "decision";
    const id = reading.id ?? "";
    const source = decision ? decided.get(id) : held.get(id)?.node.made_at;
    if (source === undefined || source === null) {
      const read = decision ? "decisions read" : "tree read";
      return [`${where}, ${rowName(row)} draws a time, and the ${read} holds none for it`];
    }
    const want = shortDay(source);
    return reading.time === want
      ? []
      : [`${where}, ${rowName(row)} draws its time as ${reading.time}, and its source as ${want}`];
  });
}

/** The review state the tree read answers on decision `node`, as the page's token for it. */
function answeredReviewState(node: IssueTreeNode, changed: boolean): string | null {
  if (node.decision?.replaced === true) return "replaced";
  if (changed) return "change_requested";
  if (node.decision?.waiting_review === true) return "waiting_review";
  return node.decision?.kept == null ? null : "kept";
}

/** Row 1's decision clauses, once the seeds' decisions are nodes (`decisionNodesGate`): each
 *  decision row's review state is the answer's, each Change is a margin thread and no row, the
 *  section that restates a person's answer is no row, and the answered question's row carries a
 *  chip for each node its `shaped` ties name. */
async function expectDecisionsFromNodes(
  page: Page,
  outline: OutlineRead,
  recorded: SeedFixture
): Promise<void> {
  const { answer, rows } = outline;
  const idOf = treeIdOf(answer, outline.reads);
  const held = heldNodes(answer, idOf);
  const nodes = new Map((answer.nodes ?? []).map((node) => [node.id, node]));
  const changes = decisionNodeChanges(answer);

  const states = rows.flatMap((row) => {
    const node = row.id === null ? undefined : held.get(row.id)?.node;
    if (row.kind.type !== "decision" || node?.supertag !== "decision") return [];
    const want = answeredReviewState(node, changes.has(node.id));
    const token = row.state?.token ?? null;
    return token === want ? [] : [`${row.id} draws ${token}, and the tree read answers ${want}`];
  });
  expect(states, "each decision row's review state is the tree read's").toEqual([]);

  const onText = textComments(answer, outline.anchored);
  for (const comment of answer.nodes ?? []) {
    const decision = comment.parent === null ? undefined : nodes.get(comment.parent.id);
    if (
      comment.supertag !== "comment" ||
      comment.author?.[0]?.kind !== "user" ||
      decision?.supertag !== "decision"
    ) {
      continue;
    }
    const id = idOf(comment.id);
    const home = idOf(decision.id);
    const at = rows.findIndex((row) => row.id === id);
    if (!onText.has(comment.id)) {
      expect(at, `the unanchored comment ${id} is no row`).toBeGreaterThanOrEqual(0);
      expect(parentIdOf(rows, at), `the unanchored comment ${id}'s parent`).toBe(home);
      continue;
    }
    expect(at, `the Change ${id} is an outline row`).toBe(-1);
    if (rows.some((row) => row.id === home)) {
      await expect(
        page.locator(`[data-text-thread="${id}"]`),
        `the Change ${id} stands in the margin beside ${home}`
      ).toHaveAttribute("data-text-thread-node", home);
    }
  }

  const keyed = keyedRows(rows, outline.keys);
  const restating = recorded.rows.filter((row) => restatesAnAnswer(row, recorded.rows));
  expect(
    restating.flatMap((row) => (keyed.some((one) => one.key === row.key) ? [row.key] : [])),
    "the section that restates a person's answer is no row"
  ).toEqual([]);

  const asks = [
    ...new Set(restating.flatMap((row) => (row.parentKey === null ? [] : [row.parentKey]))),
  ];
  const unnamed = asks.flatMap((askKey) => {
    const row = rows[keyed.findIndex((one) => one.key === askKey)];
    if (row === undefined || row.id === null) return [`the answered question ${askKey} is no row`];
    // A chip's address names a block by its id in its document (`block_id`).
    const shaped = (answer.tuples ?? []).flatMap((tuple) =>
      tuple.kind === "shaped" && idOf(tuple.from.id) === row.id
        ? [nodes.get(tuple.to.id)?.block_id ?? tuple.to.id]
        : []
    );
    if (shaped.length === 0) return [`the answered question ${askKey} shaped no node`];
    return shaped.flatMap((target) =>
      row.chips.some((chip) => chip.address?.includes(target) === true)
        ? []
        : [`the answered question ${askKey} draws no chip naming ${target}`]
    );
  });
  expect(unnamed, "the answered question's row carries a chip for each node it shaped").toEqual([]);
}

/** Wait for the coverage read to stop naming documents that owe indexing. The worker normally
 *  runs every 30 seconds; this allows a second cycle before failing with the pending lines. */
export async function awaitIndexedCoverage(key: string): Promise<void> {
  await expect
    .poll(async () => coverageNotes(await getIssueCoverage(key)), {
      intervals: [250, 500, 1_000, 2_000],
      message: `${key}'s coverage still names documents awaiting indexing`,
      timeout: 60_000,
    })
    .toEqual([]);
}

/** Row 1 on the seed `name`, which `seed` seeds: as Graph opens, each row stands under the parent
 *  the tree read answers, and reads as the reads drew it. The fixture's `opened` rows are the old
 *  page as it opened, folded to the frontier, and Graph opens with every node unfolded (R11), so
 *  the rows as Graph opens are read against the fixture's `rows`, the old page with every branch
 *  open. */
export async function expectSameRows(
  browser: Browser,
  name: string,
  seed: NamedSeed[1]
): Promise<void> {
  await phase(browser, seed, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    await awaitIndexedCoverage(seeded.key);
    const outline = await readOutline(page, seeded);
    expect(
      firstRowOutOfPlace("as Graph opens", outline.rows, outline),
      "each row stands under the parent the tree read answers"
    ).toBeUndefined();
    const recorded = fixtureSeed(name);
    const nodes = decisionsAreNodes(outline.answer, outline.decisions);
    expect(
      differences("as Graph opens", outline.rows, recorded.rows, outline, nodes),
      "each row reads as the reads drew it"
    ).toEqual([]);
  });
}

/** Row 1's decision clauses on the seed `name`, which `seed` seeds, owed on `decisionNodesGate`
 *  and asserted once it is met (`expectDecisionsFromNodes`). */
export async function expectSameRowsDecisions(
  browser: Browser,
  name: string,
  seed: NamedSeed[1]
): Promise<void> {
  await phase(browser, seed, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    const outline = await readOutline(page, seeded);
    owedUnless(decisionNodesGate(outline.answer, outline.decisions));
    await expectDecisionsFromNodes(page, outline, fixtureSeed(name));
  });
}

// ---------------------------------------------------------------------------------------------
// Row 2: what the first load and each filter send.
// ---------------------------------------------------------------------------------------------

/** The requests of the first load: every request until the page is quiet and draws the same rows
 *  twice, as the fixture's `loadRequests` counted them. */
export async function firstLoad(page: Page, traffic: ApiRequests, key: string) {
  await seedEventsLanded(page);
  await gotoIssue(page, `/issues/${key}`);
  await expectIssuePageDrawn(page);
  const { total } = await steadily(page, traffic, `${key} as the page opens`, async () => ({
    rows: await drawnRowReadings(page, { bodies: false }),
    total: traffic.total(),
  }));
  return traffic.requests.slice(0, total);
}

/** The most requests under `/api/` a first load sends (root R21). */
export const LOAD_BOUND = 40;

/**
 * The view reads R4 sends with a load of Conversation of `issue`, whose Conversation's read
 * answered `answer`: a filter's view read goes with the load only where Conversation's read does
 * not hold that filter's nodes - Plan's always, Spec's where the issue is a child, and
 * Verification's where the issue is a parent and `answer` holds no check of an issue under it
 * (spec R4 and R5).
 */
export function loadViews(issue: IssueDetails, answer: IssueTreeRead): TreeView[] {
  const lacksChildChecks = !(answer.nodes ?? []).some(
    (node) => node.supertag === "check" && node.issue !== undefined && node.issue !== issue.key
  );
  return [
    "plan",
    ...(issue.parent === null ? [] : ["spec" as const]),
    ...((issue.children ?? []).length > 0 && lacksChildChecks ? ["verification" as const] : []),
  ];
}

/** The kind of a version's author the server counts as a person (`recordEditNotice`,
 *  `packages/envoy/internal/dispatch/docs/edit_notices.go`): a version that lists no author of
 *  that kind opens no edit notice. */
const PERSON = "user" satisfies Actor["kind"];

/** The most events one page of an issue's event log holds, and what a page holds when its read
 *  names no limit (`maxEventPage`, `packages/envoy/internal/dispatch/api/events.go`). */
const EVENT_PAGE = 200;

/** How long `editNoticesSent` waits: the 30 s a document stays quiet after a person's last
 *  version before its notice is due (`defaultEditNoticeQuiet`), up to 5 s more for the server's
 *  next sweep (`editNoticeSweepSchedule`), and a margin. */
const EDIT_NOTICES_WAIT_MS = 60_000;

/** Every event of issue `key`, oldest first: its event log read a page at a time, each page after
 *  the last event's `seq`, until a page comes back short. */
async function issueEventLog(key: string): Promise<DispatchEvent[]> {
  const log: DispatchEvent[] = [];
  let batch: DispatchEvent[];
  do {
    batch = await getIssueEvents(key, { after: log.at(-1)?.seq ?? 0, limit: EVENT_PAGE });
    log.push(...batch);
  } while (batch.length === EVENT_PAGE);
  return log;
}

/**
 * Each document of `log` the server still owes an edit notice, named with the version it owes it
 * for. The server opens a document's notice window at a version whose authors list a person, and
 * moves the window on at each later one; a document's creation is an upload, which opens none
 * (`recordEditNotice`). The window's `artifact.human_edited` event names the last of those
 * versions in `to_version`, so a document is owed its notice until such an event reaches the
 * last version a person wrote of it.
 */
function owedEditNotices(log: readonly DispatchEvent[]): string[] {
  const written = new Map<string, { readonly name: string; readonly version: number }>();
  const noticed = new Map<string, number>();
  for (const event of log) {
    if (event.type === "artifact.version") {
      const { artifact_id, name, version } = event.payload;
      if (version.authors.some((author) => author.kind === PERSON)) {
        written.set(artifact_id, { name, version: version.number });
      }
    } else if (event.type === "artifact.human_edited") {
      const { artifact_id, to_version } = event.payload;
      noticed.set(artifact_id, Math.max(noticed.get(artifact_id) ?? 0, to_version));
    }
  }
  return [...written].flatMap(([id, last]) =>
    (noticed.get(id) ?? 0) >= last.version
      ? []
      : [`${last.name} (${id}) at version ${last.version}`]
  );
}

/**
 * Waits until the server has sent every edit notice issue `key` is owed (`owedEditNotices`), and
 * then for the page to go quiet. The server sends a document's notice, one
 * `artifact.human_edited` event, about 30 s after a person's last version of it, in a sweep every
 * 5 s (`defaultEditNoticeQuiet` and `editNoticeSweepSchedule`,
 * `packages/envoy/internal/dispatch/docs/edit_notices.go`), and the page reads the issue's detail
 * again for each notice it hears. A seed whose documents a person wrote is still owed those
 * notices half a minute on, so a row that counts what a press sends waits them out first,
 * whatever its own timing is.
 */
export async function editNoticesSent(key: string, traffic: ApiRequests): Promise<void> {
  await expect
    .poll(async () => owedEditNotices(await issueEventLog(key)), {
      intervals: [1_000],
      message:
        `the server sent no edit notice within ${EDIT_NOTICES_WAIT_MS / 1000} s for each ` +
        "document listed, whose last version by a person no artifact.human_edited event reaches",
      timeout: EDIT_NOTICES_WAIT_MS,
    })
    .toEqual([]);
  await expectLoadSettled(traffic);
}

/** Turns `filter` on and waits for its rows to hold still. */
export async function pressFilter(page: Page, traffic: ApiRequests, filter: TreeView) {
  const name = PRESET_NAMES[filter];
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  return steadily(page, traffic, `the ${name} filter`, async () => ({
    count: await presetCount(page, filter).textContent(),
    nodes: await drawnNodes(page),
    rows: await drawnRowReadings(page, { bodies: false }),
  }));
}

/** Turns `filter` off, which puts the page back on Conversation. */
export async function releaseFilter(page: Page, filter: TreeView): Promise<void> {
  await presetToggle(page, filter).click();
  await expectActivePreset(page, PRESET_NAMES.conversation);
}

/** When the page's live socket last received a frame, by this process's clock, which
 *  `page.clock` does not move: undefined before its first frame, and again after each jump of
 *  `pastStaleTime`, since a frame from before a jump is 31 s older to the page than to this
 *  process. */
export interface LiveFrames {
  at: number | undefined;
}

/** Records when the page's live socket receives each frame. Called before the page loads, it
 *  watches every live socket the page opens. */
export function watchLiveFrames(page: Page): LiveFrames {
  const live: LiveFrames = { at: undefined };
  page.on("websocket", (socket) => {
    if (!isLiveSocket(socket)) return;
    socket.on("framereceived", () => {
      live.at = Date.now();
    });
  });
  return live;
}

/** The youngest the live socket's last frame may be as `pastStaleTime` jumps. */
const JUMP_FRAME_MIN_AGE_MS = 2_000;

/** The oldest the live socket's last frame may be as `pastStaleTime` jumps. */
const JUMP_FRAME_MAX_AGE_MS = 10_000;

/** How long `pastStaleTime` waits for a frame of that age: two of the server's heartbeats. */
const JUMP_FRAME_WAIT_MS = 30_000;

/**
 * Moves the page's clock 31 s on, past the app's 30 s `staleTime` (`web/src/main.tsx`), and
 * waits out whatever a timer due in that time sent, so what follows counts what a press sends.
 *
 * It jumps only once the live socket's last frame is 2 to 10 s old, by this process's clock. The
 * page replaces a socket that has carried no frame for 45 s (`LIVE_SOCKET_SILENT_MS`,
 * `web/src/api/live-socket.ts`), and its check for that, every 15 s, falls due inside the jump,
 * so it runs once at the jump's end and finds the last frame 31 s older. The server sends a
 * heartbeat frame every 15 s (`defaultStreamHeartbeat`), so a jump made more than 14 s after the
 * last frame replaces the socket, and the new socket's feed reads the issue, the tree and the
 * open view again inside the press that counts them. A frame under 10 s old is at most 41 s old
 * to that check. The next check comes 15 s after the jump and replaces the socket unless a frame
 * came since. A last frame at least 2 s old means the last heartbeat came at least 2 s before
 * the jump, so the next one comes inside those 15 s.
 */
export async function pastStaleTime(
  page: Page,
  traffic: ApiRequests,
  live: LiveFrames
): Promise<void> {
  await expect
    .poll(
      () => {
        if (live.at === undefined) return false;
        const age = Date.now() - live.at;
        return age >= JUMP_FRAME_MIN_AGE_MS && age < JUMP_FRAME_MAX_AGE_MS;
      },
      {
        message:
          `the page's live socket carried no frame ${JUMP_FRAME_MIN_AGE_MS / 1000} to ` +
          `${JUMP_FRAME_MAX_AGE_MS / 1000} s old within ${JUMP_FRAME_WAIT_MS / 1000} s, so the ` +
          "clock cannot jump 31 s without the page replacing the socket",
        timeout: JUMP_FRAME_WAIT_MS,
      }
    )
    .toBe(true);
  await page.clock.fastForward("00:31");
  // A frame from before the jump is 31 s older to the page than to this process, so the next
  // jump waits for one after it.
  live.at = undefined;
  await expectLoadSettled(traffic);
}

// ---------------------------------------------------------------------------------------------
// Row 3: each filter's rows are its view read's.
// ---------------------------------------------------------------------------------------------

/** The seeds row 3 reads. */
export const VIEW_SEEDS: readonly NamedSeed[] = [
  ["seedWholeIssue", seedWholeIssue],
  ["seedSpineCases", seedSpineCases],
];

/** The ids of the drawn nodes of `type`. */
export function idsOfType(nodes: readonly DrawnNode[], type: string): string[] {
  return nodes.flatMap((node) => (node.type === type ? [node.id] : []));
}

/** Each check of the view that is drawn under no row of a statement its `serves` names. */
export function checksOutOfPlace(
  rows: readonly RowReading[],
  answer: IssueTreeRead,
  reads: TreeIdReads
): string[] {
  return servesOutOfPlace(
    rows,
    { answer, reads },
    (node) => node.supertag === "check" && node.role === "query",
    false
  );
}

/** Each node of this issue that `serving` takes and that is drawn under no row of a statement of
 *  this issue its `serves` tuples name, both named by the ids `treeIdOf` gives them; where
 *  `drawnOnly` holds, only where both are drawn. */
function servesOutOfPlace(
  rows: readonly RowReading[],
  from: DrawnFrom,
  serving: (node: IssueTreeNode) => boolean,
  drawnOnly: boolean
): string[] {
  const idOf = treeIdOf(from.answer, from.reads);
  const key = from.reads.issue.key;
  const nodes = nodesById(from.answer);
  const own = (node: IssueTreeNode | undefined): node is IssueTreeNode =>
    node !== undefined && (node.issue === undefined || node.issue === key);
  const drawn = new Set(rows.flatMap((row) => (row.id === null ? [] : [row.id])));
  const placements = new Set(rows.map((row, index) => `${row.id} ${parentIdOf(rows, index)}`));
  return (from.answer.tuples ?? []).flatMap((tuple) => {
    const server = nodes.get(tuple.from.id);
    const statement = nodes.get(tuple.to.id);
    if (
      tuple.kind !== "serves" ||
      !own(server) ||
      !own(statement) ||
      !serving(server) ||
      statement.supertag !== "requirement"
    ) {
      return [];
    }
    const [servingId, statementId] = [idOf(server.id), idOf(statement.id)];
    if (drawnOnly && (!drawn.has(servingId) || !drawn.has(statementId))) return [];
    return placements.has(`${servingId} ${statementId}`)
      ? []
      : [`${servingId} is drawn under no row of ${statementId}, which it serves`];
  });
}

/** Each key the filter draws that the fixture's rows of it do not hold, and each the other way,
 *  that is of a kind root R2 draws under the filter: a difference R2 does not explain. */
export function unexplainedDifferences(
  filter: TreeView,
  rows: readonly RowReading[],
  keys: ReadonlyMap<string, string>,
  recorded: SeedFixture
): string[] {
  const drawn = keyedRows(rows, keys);
  const drawnKeys = new Set(drawn.map((row) => row.key));
  const recordedKeys = new Set(recorded.filters[filter]?.rows ?? []);
  const kinds: readonly string[] = [...R2_ROW_KINDS[filter].query, ...R2_ROW_KINDS[filter].context];
  const kindOf = (key: string): string | null =>
    [...drawn, ...recorded.rows, ...recorded.opened].find((row) => row.key === key)?.kind.type ??
    null;
  return [
    ...[...drawnKeys].filter((key) => !recordedKeys.has(key)),
    ...[...recordedKeys].filter((key) => !drawnKeys.has(key)),
  ].flatMap((key) => {
    const kind = kindOf(key);
    if (kind !== null && !kinds.includes(kind)) return [];
    const side = drawnKeys.has(key) ? "is drawn and the fixture holds no row of it" : "is no row";
    return [`${key} (${kind ?? "a row of no kind"}) ${side}`];
  });
}

// ---------------------------------------------------------------------------------------------
// Row 4: a write is drawn from the tree read again, with no reload.
// ---------------------------------------------------------------------------------------------

/** The id of the row the row whose words hold `words` stands under (null at the top), or
 *  undefined where no row holds them. */
export function parentOfWords(
  readings: readonly RowReading[],
  words: string
): string | null | undefined {
  const index = readings.findIndex((reading) => reading.words?.includes(words) === true);
  if (index < 0) return undefined;
  return parentIdOf(readings, index);
}

/** The state word the row of node `id` draws, or null where no row of it draws one. */
export async function stateWordOf(page: Page, id: string): Promise<string | null> {
  const rows = await drawnRowReadings(page, { bodies: false });
  return rows.find((row) => row.id === id)?.state?.word ?? null;
}

/** Asserts what a write sends of the tree read: the read its event asks for - with the live feed
 *  down, its settle's - and one more where that read is still out as the other ask comes
 *  (`subscribeTreeFreshness`), never more. */
export function expectOneOrTwoReads(reads: number, message: string): void {
  expect(reads, message).toBeGreaterThanOrEqual(1);
  expect(reads, message).toBeLessThanOrEqual(2);
}

// ---------------------------------------------------------------------------------------------
// Rows 5 to 14: each outline as it opens, read against the answer it was drawn from.
// ---------------------------------------------------------------------------------------------

/** An outline as it opened: the answer of its read, the reads its ids are named by, every row
 *  with the face it draws once two readings agree, and the requests the page sends from the
 *  navigation on. */
export interface OpenedOutline extends DrawnFrom {
  readonly rows: readonly RowReading[];
  readonly traffic: ApiRequests;
}

/** Opens issue `key` at the address of `outline`, once the seed's events have landed, and reads
 *  it as it opens: every row with its face (`drawnRowFaces`) once two readings agree, the load's
 *  requests, which `loaded` reads before anything waits on the outline's read (a row asserts
 *  there what its load sent), and the answer of that outline's read (`outlineAnswerOf`). */
export async function openOutline(
  page: Page,
  key: string,
  outline: OutlineKind,
  { loaded }: { readonly loaded?: (load: readonly ApiRequest[]) => void } = {}
): Promise<OpenedOutline> {
  const watch = outlineAnswerOf(page, outline);
  const traffic = watchApiRequests(page);
  await seedEventsLanded(page);
  await gotoIssue(page, presetPath(key, outline));
  await expectIssuePageDrawn(page);
  const { rows, total } = await steadily(page, traffic, `${key} as ${outline} opens`, async () => ({
    rows: await drawnRowFaces(page),
    total: traffic.total(),
  }));
  const load = traffic.requests.slice(0, total);
  loaded?.(load);
  const answer = await heldAnswer(watch, `${key}'s ${outline} read`);
  return { answer, reads: await idReadsOf(key), rows, traffic };
}

/** The kinds of an item's own blocks: a block of an ask, a comment or a message is part of its
 *  item and no row of its own (spec R10). */
const ITEM_BLOCK_KINDS: readonly string[] = ["ask_block", "comment_block", "message_block"];

/** The kinds of node that are who wrote or decided a node, a person and a session, which the
 *  answer holds as context of the nodes they name and the page draws no row of (spec R7): a row
 *  names them in its author and its state. */
const ACTOR_KINDS: readonly string[] = ["person", "session"];

/** The read ids of the answer's nodes an outline draws no row of on any seed of these rows: the
 *  memo's callout, which the three sentences draw (`memoBlock`, the block id the seed wrote it
 *  under), each comment on a node's text (`textComments`), each block of an item, and each
 *  person and session. */
export function nodesNoRowDraws(
  answer: IssueTreeRead,
  anchored: ReadonlySet<string>,
  memoBlock: string | undefined
): Set<string> {
  const onText = textComments(answer, anchored);
  return new Set(
    (answer.nodes ?? []).flatMap((node) =>
      onText.has(node.id) ||
      ITEM_BLOCK_KINDS.includes(node.kind) ||
      ACTOR_KINDS.includes(node.kind) ||
      ACTOR_KINDS.includes(node.supertag) ||
      (memoBlock !== undefined && node.supertag === "callout" && node.block_id === memoBlock)
        ? [node.id]
        : []
    )
  );
}

/** The tree ids of the decisions of `from` that wait on the reader's review, as the review list
 *  reads them (c-review-list): a decision the answer marks `waiting_review`, no Change asked of
 *  it, whose `shaped` tuples name a statement of this issue. */
export function decisionsThatWait(from: DrawnFrom): Set<string> {
  const idOf = treeIdOf(from.answer, from.reads);
  const key = from.reads.issue.key;
  const nodes = nodesById(from.answer);
  const changed = decisionNodeChanges(from.answer);
  const shapesStatement = (id: string) =>
    (from.answer.tuples ?? []).some((tuple) => {
      const target =
        tuple.kind === "shaped" && tuple.from.id === id ? nodes.get(tuple.to.id) : undefined;
      return (
        target?.supertag === "requirement" && (target.issue === undefined || target.issue === key)
      );
    });
  return new Set(
    (from.answer.nodes ?? []).flatMap((node) =>
      node.supertag === "decision" &&
      node.decision?.waiting_review === true &&
      !changed.has(node.id) &&
      shapesStatement(node.id)
        ? [idOf(node.id)]
        : []
    )
  );
}

/** The ids an outline drawn from `from` draws a row of, each once and sorted (`idSet`): the tree
 *  id of every node of the answer but the issue itself and each node `leftOut` names by its read
 *  id. */
export function answerRowIds(from: DrawnFrom, leftOut: ReadonlySet<string>): string[] {
  const idOf = treeIdOf(from.answer, from.reads);
  return idSet(
    (from.answer.nodes ?? []).flatMap((node) => {
      const id = idOf(node.id);
      return id === from.reads.issue.key || leftOut.has(node.id) ? [] : [id];
    })
  );
}

/** Each check and plan task of this issue that is drawn under no row of a statement its `serves`
 *  names, where both are drawn: the outline draws such a node again under each statement it
 *  serves, the one node (root R4, spec R9, R10). */
export function servingOutOfPlace(rows: readonly RowReading[], from: DrawnFrom): string[] {
  return servesOutOfPlace(
    rows,
    from,
    (node) => node.supertag === "check" || node.supertag === "plan_task",
    true
  );
}

/** Every row's node id that opens folded: a row that draws its one line (`aria-expanded`
 *  false), with the node's first line in place of its words. */
export function foldedRowIds(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLElement>(
        '[data-issue-page] [data-node-row][aria-expanded="false"]'
      ),
      (row) => row.dataset.nodeId ?? ""
    )
  );
}

/** Every row whose words the page does not draw whole: a row whose words box hides some of them
 *  (its `scrollHeight` past its `clientHeight`, a pixel for rounding), or one that draws a
 *  control for more words (`[data-node-more]`), each by its node id. */
export function rowsNotWhole(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll<HTMLElement>("[data-issue-page] [data-node-row]"))
      .filter((row) => {
        const words = row.querySelector("[data-node-words]");
        const hidden = words !== null && words.scrollHeight > words.clientHeight + 1;
        return hidden || row.querySelector("[data-node-more]") !== null;
      })
      .map((row) => row.dataset.nodeId ?? "")
  );
}

/** The serving-checks line the face of a statement draws from the coverage read's two counts
 *  (`NodeRow.tsx`'s `NodeFace`), as `drawnRowFaces` reads it into the row's `counts`. */
export function servingCountsLine(checks: number, planTasks: number): string {
  return (
    `${CONTROL_LABELS.serving_checks}: ${checks}, ${CONTROL_LABELS.serving_plan_tasks}: ` +
    `${planTasks}`
  );
}
