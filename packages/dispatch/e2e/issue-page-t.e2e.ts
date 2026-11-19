import type { IssueTreeAnswer, IssueTreeNode } from "../web/src/api/client";
import { decisionNodeChanges } from "../web/src/features/issue-page/decision-review";
import { shortDay } from "../web/src/features/issue-page/node-time";
import { NODE_STATE_WORDS, PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import { type TreeIdReads, treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { wordsOf } from "../web/src/features/refs/tree-fragment";
import { getIssue, listIssueAsks } from "./api";
import { seedReviewList, seedSpineCases, seedWholeIssue } from "./issue-layout-seed";
import {
  closeDetails,
  gotoIssue,
  openDetails,
  phase,
  presetCount,
  presetToggle,
} from "./issue-page";
import {
  blockNode,
  expectIssuePageDrawn,
  focusRow,
  nodeRow,
  rowAct,
  unfoldEveryBranch,
} from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import {
  besideViewRead,
  blocksReads,
  convertedGate,
  type DecisionsRead,
  type DrawnNode,
  decisionNodesGate,
  decisionsAreNodes,
  decisionsReadOf,
  decisionsViewGate,
  decisionTimes,
  drawnNodes,
  drawnRowReadings,
  fixtureSeed,
  footLineGate,
  headerStatus,
  heldAnswer,
  type KeyedRow,
  keyedRows,
  type NamedSeed,
  owedUnless,
  parentIndex,
  partUGate,
  R2_COUNTED_KINDS,
  R2_ROW_KINDS,
  type RowReading,
  SEEDS,
  type SeededIssue,
  type SeedFixture,
  seedEventsLanded,
  seedKeys,
  sortedFields,
  steadily,
  type TreeAnswerWatch,
  type TreeView,
  treeAnswerOf,
  treeReadGate,
  treeReads,
  viewAnswerOf,
  viewCount,
  viewNodeIds,
} from "./issue-page-t";
import { expect, type Page, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * The rows of the new issue page that draw it from the tree read: the outline drawn from the
 * issue's tree read (`GET /api/v1/issues/{key}/tree`) and each filter from that read's view
 * (`?view=`), read on the real stack. Each row turns the new page on for alice with `phase`
 * (1440 by 900), seeds one of the issues of `e2e/issue-layout-seed.ts`, opens it with
 * `gotoIssue`, and reads it through `unfoldEveryBranch`, `pickPreset`, `watchApiRequests` and
 * `expectLoadSettled` (`e2e/traffic.ts`) and the helpers of `e2e/issue-page-t.ts`.
 *
 * The rows are the four of the Verification document's browser checks, by their titles:
 *
 * 1. `c-same-rows`: "the outline drawn from the tree read holds the rows the reads drew", one row
 *    per seed, so the check's `-g` runs all 14 and each ends inside CI's nine minutes. Each row
 *    stands under the node the answer names as its parent, or its nearest drawn ancestor, in the
 *    answer's order; that is read from the answer and never from the fixture, so the check's
 *    control fails on the parent first. Every other reading is the fixture's
 *    (`e2e/fixtures/issue-page-t-rows.json`), by key, as the page opens and with every branch
 *    open, and each time is its source's in this run.
 * 2. `c-tree-requests`: "the first load reads the tree once and a filter its view once", and Plan,
 *    pressed 31 s after the load, past the app's 30 s `staleTime`, sends no read but its view's:
 *    the page's reads are refreshed by events, never by age.
 * 3. `c-views`: "each filter draws its view read's rows", on `seedWholeIssue` and
 *    `seedSpineCases`.
 * 4. `c-fresh`: "a write is drawn without a reload": a status picked in the page's header sends
 *    one or two tree reads (its own refresh, and its event's), and a reply sent from the row's
 *    Reply on the open question becomes a row under it and sends one or two.
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
 * (`decisionsViewGate` and `partUGate`).
 */

/** The seeds whose rows the fixture records a decision row of: row 1's decision clauses read
 *  them (`decisionNodesGate`). */
const DECISION_SEEDS = new Set(
  SEEDS.flatMap(([name]) =>
    fixtureSeed(name).rows.some((row) => row.kind.type === "decision") ? [name] : []
  )
);

/** The filters that read a view of the tree today: Decisions reads one once the decisions view
 *  is on the base (`decisionsViewGate`). */
const VIEW_FILTERS = ["spec", "verification", "plan"] as const satisfies readonly TreeView[];

/** How long one seed's row 1 may take: every row of the outline read with its body, inside CI's
 *  nine minutes. */
const SAME_ROWS_MS = 480_000;

/** How long each other row may take. */
const ROW_MS = 240_000;

/** The words row 4 replies with. */
const REPLY_WORDS = "The tree read draws this reply with no reload.";

/** A decision row's state word once alice kept it. */
const KEPT_BY_ALICE = new RegExp(`^${NODE_STATE_WORDS.decision.kept}\\b.*\\balice\\b`, "i");

/** The reads `treeIdOf` names a node by: the issue read and the asks read, as alice reads them. */
async function idReadsOf(key: string): Promise<TreeIdReads> {
  const [issue, asks] = await Promise.all([getIssue(key), listIssueAsks(key)]);
  return { asks, issue };
}

/** The ids, each once and sorted, so two lists of drawn ids compare as sets. */
function idSet(ids: readonly string[]): string[] {
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

/** What row 1 reads of one seed's page. */
interface OutlineRead {
  readonly answer: IssueTreeAnswer;
  readonly decisions: DecisionsRead | undefined;
  readonly keys: ReadonlyMap<string, string>;
  readonly opened: readonly RowReading[];
  readonly reads: TreeIdReads;
  readonly rows: readonly RowReading[];
  readonly slugs: readonly string[];
}

/** Opens the seeded issue and reads it as the recorder did: the rows as the page opens, once two
 *  readings agree, then every row once every branch is open, with its body where `bodies`; and
 *  what the page received of the tree read and the decisions read. */
async function readOutline(
  page: Page,
  seeded: SeededIssue,
  { bodies = true }: { readonly bodies?: boolean } = {}
): Promise<OutlineRead> {
  const tree = treeAnswerOf(page);
  const decisions = decisionsReadOf(page, seeded.key);
  const traffic = watchApiRequests(page);
  await gotoIssue(page, `/issues/${seeded.key}`);
  await expectIssuePageDrawn(page);
  const opened = await steadily(page, traffic, `${seeded.key} as the page opens`, () =>
    drawnRowReadings(page, { bodies: false })
  );
  await unfoldEveryBranch(page);
  const unfolded = await steadily(page, traffic, `${seeded.key} with every branch open`, () =>
    drawnRowReadings(page, { bodies: false })
  );
  const rows = bodies ? await drawnRowReadings(page) : unfolded;
  const answer = await heldAnswer(tree, `${seeded.key}'s tree read`);
  const reads = await idReadsOf(seeded.key);
  const slugs = (reads.issue.artifacts ?? []).map((artifact) => artifact.slug);
  return {
    answer,
    decisions: decisions.read(),
    keys: seedKeys(seeded, slugs),
    opened,
    reads,
    rows,
    slugs,
  };
}

/** A node of the answer by the id its row carries: of the nodes the tree draws by one id, the
 *  first the answer holds, with its place in the answer's order. */
interface HeldNode {
  readonly node: IssueTreeNode;
  readonly order: number;
}

function heldNodes(
  answer: IssueTreeAnswer,
  idOf: (readId: string) => string
): Map<string, HeldNode> {
  const held = new Map<string, HeldNode>();
  for (const [order, node] of answer.nodes.entries()) {
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
  outline: OutlineRead,
  idOf: (readId: string) => string,
  drawn: ReadonlySet<string>
): ReadonlySet<string | null> {
  const key = outline.reads.issue.key;
  const nodes = new Map(outline.answer.nodes.map((one) => [one.id, one]));
  const foreign = (one: IssueTreeNode): boolean => one.issue !== undefined && one.issue !== key;
  let parent: string | null = null;
  const passed = new Set<string>();
  for (
    let above = node.parent;
    above !== null && !passed.has(above);
    above = nodes.get(above)?.parent ?? null
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
  const served = outline.answer.tuples.flatMap((tuple) => {
    const target =
      tuple.kind === "serves" && tuple.from === node.id ? nodes.get(tuple.to) : undefined;
    return target?.supertag === "requirement" && !foreign(target) ? [idOf(target.id)] : [];
  });
  return new Set(parent === null && served.length > 0 ? served : [parent, ...served]);
}

/** The first row of `readings` the answer holds that does not stand under the node the answer
 *  places it under, at the level that gives, or stands before a sibling the answer orders before
 *  it; undefined where none. */
function firstRowOutOfPlace(
  where: string,
  readings: readonly RowReading[],
  outline: OutlineRead
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
 * source in this run. Once the seeds' decisions are nodes (`nodes`, `decisionNodesGate`), the two
 * differences the check names are allowed: the section that restates a person's answer is no
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
      const theirs = match[field];
      const mine =
        field === "chips" && nodes && row.kind.type === "ask" && row.state?.token === "answered"
          ? row.chips.filter((chip) =>
              match.chips.some((kept) => canonical(kept) === canonical(chip))
            )
          : row[field];
      if (canonical(mine) !== canonical(theirs)) {
        found.push(
          `${where}, ${rowName(row)} draws its ${field} as ${canonical(mine)}, and the fixture ` +
            `as ${canonical(theirs)}`
        );
      }
    }
  }
  for (const rows of unmatched.values()) {
    for (const row of rows) {
      if (nodes && restatesAnAnswer(row, recorded)) continue;
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
    if (source === undefined) {
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
  if (node.replaced === true) return "replaced";
  if (changed) return "change_requested";
  if (node.waiting_review === true) return "waiting_review";
  return node.kept == null ? null : "kept";
}

/** Row 1's decision clauses, once the seeds' decisions are nodes (`decisionNodesGate`): each
 *  decision row's review state is the answer's, each Change stands under its decision's row, the
 *  section that restates a person's answer is no row, and the answered question's row carries a
 *  chip for each node its `shaped` ties name. */
function expectDecisionsFromNodes(outline: OutlineRead, recorded: SeedFixture): void {
  const { answer, rows } = outline;
  const idOf = treeIdOf(answer, outline.reads);
  const held = heldNodes(answer, idOf);
  const nodes = new Map(answer.nodes.map((node) => [node.id, node]));
  const changes = decisionNodeChanges(answer);
  const under = rows.map((_, index) => parentIdOf(rows, index));

  const states = rows.flatMap((row) => {
    const node = row.id === null ? undefined : held.get(row.id)?.node;
    if (row.kind.type !== "decision" || node?.supertag !== "decision") return [];
    const want = answeredReviewState(node, changes.has(node.id));
    const token = row.state?.token ?? null;
    return token === want ? [] : [`${row.id} draws ${token}, and the tree read answers ${want}`];
  });
  expect(states, "each decision row's review state is the tree read's").toEqual([]);

  const strays = answer.nodes.flatMap((node) => {
    const decision = node.parent === null ? undefined : nodes.get(node.parent);
    if (
      node.supertag !== "comment" ||
      node.author[0]?.kind !== "user" ||
      decision?.supertag !== "decision"
    ) {
      return [];
    }
    const id = idOf(node.id);
    const home = idOf(decision.id);
    const at = rows.findIndex((row) => row.id === id);
    if (at < 0) return [`the Change ${id} is no row`];
    return under[at] === home ? [] : [`the Change ${id} stands under ${under[at]}, not ${home}`];
  });
  expect(strays, "each Change stands under its decision's row").toEqual([]);

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
    const shaped = answer.tuples.flatMap((tuple) =>
      tuple.kind === "shaped" && idOf(tuple.from) === row.id ? [tuple.to] : []
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

for (const [name, seed] of SEEDS) {
  const title = `the outline drawn from the tree read holds the rows the reads drew: ${name}`;
  // c-same-rows on one seed.
  // SLOW(>=2s): a whole seeded issue, read as it opens and again with every branch opened and
  // every row's body read in turn, each reading taken twice until it holds still.
  test(title, async ({ browser }) => {
    test.setTimeout(SAME_ROWS_MS);
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      owedUnless(await convertedGate(page, seeded.key));
      const outline = await readOutline(page, seeded);
      expect(
        firstRowOutOfPlace("as the page opens", outline.opened, outline) ??
          firstRowOutOfPlace("with every branch open", outline.rows, outline),
        "each row stands under the parent the tree read answers"
      ).toBeUndefined();
      const recorded = fixtureSeed(name);
      const nodes = decisionsAreNodes(outline.answer, outline.decisions);
      expect(
        [
          ...differences("as the page opens", outline.opened, recorded.opened, outline, nodes),
          ...differences("with every branch open", outline.rows, recorded.rows, outline, nodes),
        ],
        "each row reads as the reads drew it"
      ).toEqual([]);
    });
  });

  if (DECISION_SEEDS.has(name)) {
    // c-same-rows' decision clauses on one seed, owed on `decisionNodesGate`.
    // SLOW(>=2s): a whole seeded issue, read as it opens and again with every branch opened,
    // each reading taken twice until it holds still.
    test(`${title}, its decisions as nodes of the tree read @owed`, async ({ browser }) => {
      test.setTimeout(SAME_ROWS_MS);
      await phase(browser, seed, async (page, seeded) => {
        owedUnless(await treeReadGate(page, seeded.key));
        owedUnless(await convertedGate(page, seeded.key));
        const outline = await readOutline(page, seeded, { bodies: false });
        owedUnless(decisionNodesGate(outline.answer, outline.decisions));
        expectDecisionsFromNodes(outline, fixtureSeed(name));
      });
    });
  }
}

// ---------------------------------------------------------------------------------------------
// Row 2: what the first load and each filter send.
// ---------------------------------------------------------------------------------------------

/** The requests of the first load: every request until the page is quiet and draws the same rows
 *  twice, as the fixture's `loadRequests` counted them. */
async function firstLoad(page: Page, traffic: ApiRequests, key: string) {
  await seedEventsLanded(page);
  await gotoIssue(page, `/issues/${key}`);
  await expectIssuePageDrawn(page);
  const { total } = await steadily(page, traffic, `${key} as the page opens`, async () => ({
    rows: await drawnRowReadings(page, { bodies: false }),
    total: traffic.total(),
  }));
  return traffic.requests.slice(0, total);
}

/** Turns `filter` on and waits for its rows to hold still. */
async function pressFilter(page: Page, traffic: ApiRequests, filter: TreeView) {
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
async function releaseFilter(page: Page, filter: TreeView): Promise<void> {
  await presetToggle(page, filter).click();
  await expectActivePreset(page, PRESET_NAMES.conversation);
}

/** Moves the page's clock 31 s on, past the app's 30 s `staleTime` (`web/src/main.tsx`), and
 *  waits out whatever a timer due in that time sent, so what follows counts what a press sends. */
async function pastStaleTime(page: Page, traffic: ApiRequests): Promise<void> {
  await page.clock.fastForward("00:31");
  await expectLoadSettled(traffic);
}

// c-tree-requests, with the clause that the page's reads are refreshed by events, never by age:
// a slot the stage mounts again past the app's 30 s `staleTime` sends no read of its own, shown
// on Plan, whose slot reads only through `useIssueTree`.
// SLOW(>=2s): the whole seeded issue, its load waited out to a 1.5 s quiet window, Details
// opened, and three filters each pressed on, off and on again with each press waited out.
test("the first load reads the tree once and a filter its view once", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const views = {
      plan: viewAnswerOf(page, "plan"),
      spec: viewAnswerOf(page, "spec"),
      verification: viewAnswerOf(page, "verification"),
    } satisfies Record<(typeof VIEW_FILTERS)[number], TreeAnswerWatch>;
    const traffic = watchApiRequests(page);
    await page.clock.install();
    const load = await firstLoad(page, traffic, seeded.key);
    await expect(headerStatus(page), "the page draws no header").toBeVisible();
    expect(
      treeReads(load, seeded.key, null),
      "the first load sends one tree read with no view"
    ).toBe(1);
    expect(blocksReads(load), "the first load sends no blocks read").toEqual([]);
    expect(
      load.length,
      "the first load sends no more requests under /api/ than the reads did"
    ).toBeLessThanOrEqual(fixtureSeed("seedWholeIssue").loadRequests);
    await openDetails(page);
    await expectLoadSettled(traffic);
    expect(
      blocksReads(traffic.requests),
      "the first load, with the outline, Details and the header drawn, sends no blocks read"
    ).toEqual([]);
    await closeDetails(page);

    for (const filter of VIEW_FILTERS) {
      const name = PRESET_NAMES[filter];
      const late = filter === "plan";
      if (late) await pastStaleTime(page, traffic);
      const first = traffic.requests.length;
      await pressFilter(page, traffic, filter);
      await heldAnswer(views[filter], `the ${name} view read`);
      await expectLoadSettled(traffic);
      const pressed = traffic.requests.slice(first);
      expect(treeReads(pressed, seeded.key, filter), `${name} sends one view read`).toBe(1);
      expect(blocksReads(pressed), `${name} sends no blocks read`).toEqual([]);
      if (late) {
        expect(
          besideViewRead(pressed, seeded.key, filter),
          "a view pressed after 31 s sends no read"
        ).toEqual([]);
      }
      await releaseFilter(page, filter);
      if (late) await pastStaleTime(page, traffic);
      const again = traffic.requests.length;
      await pressFilter(page, traffic, filter);
      if (late) await expectLoadSettled(traffic);
      const repressed = traffic.requests.slice(again);
      expect(
        treeReads(repressed, seeded.key, filter),
        "a filter turned on again sends no view read"
      ).toBe(0);
      expect(blocksReads(repressed), `${name} turned on again sends no blocks read`).toEqual([]);
      if (late) expect(repressed, "a view pressed after 31 s sends no read").toEqual([]);
      await releaseFilter(page, filter);
    }
  });
});

// c-tree-requests' Decisions clause, owed on the model's decisions view of the tree read
// (`decisionsViewGate`): the decisions read moves onto the tree read's `view=decisions`, which the
// load reads once whoever asks, and the Decisions filter reads no blocks at all, since decisions
// are nodes of the spec.
// SLOW(>=2s): the whole seeded issue, its load waited out to a 1.5 s quiet window, and Decisions
// pressed on and off twice with each press waited out.
test("the first load reads the tree once and a filter its view once, and the load reads the Decisions view once @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const traffic = watchApiRequests(page);
    const load = await firstLoad(page, traffic, seeded.key);
    owedUnless(decisionsViewGate(load, seeded.key));
    expect(
      treeReads(load, seeded.key, "decisions"),
      "the load reads the Decisions view once, whoever asks"
    ).toBe(1);
    for (let press = 0; press < 2; press += 1) {
      const from = traffic.requests.length;
      await pressFilter(page, traffic, "decisions");
      const pressed = traffic.requests.slice(from);
      expect(
        treeReads(pressed, seeded.key, "decisions"),
        "Decisions turned on sends no view read beside the load's"
      ).toBe(0);
      const blocks = blocksReads(pressed);
      expect(blocks, "Decisions reads no blocks").toEqual([]);
      await releaseFilter(page, "decisions");
    }
  });
});

// ---------------------------------------------------------------------------------------------
// Row 3: each filter's rows are its view read's.
// ---------------------------------------------------------------------------------------------

/** The seeds row 3 reads. */
const VIEW_SEEDS: readonly NamedSeed[] = [
  ["seedWholeIssue", seedWholeIssue],
  ["seedSpineCases", seedSpineCases],
];

/** The ids of the drawn nodes of `type`. */
function idsOfType(nodes: readonly DrawnNode[], type: string): string[] {
  return nodes.flatMap((node) => (node.type === type ? [node.id] : []));
}

/** Each check of the view that is drawn under no row of a statement its `serves` names. */
function checksOutOfPlace(
  rows: readonly RowReading[],
  answer: IssueTreeAnswer,
  reads: TreeIdReads
): string[] {
  const idOf = treeIdOf(answer, reads);
  const key = reads.issue.key;
  const nodes = new Map(answer.nodes.map((node) => [node.id, node]));
  const own = (node: IssueTreeNode | undefined): node is IssueTreeNode =>
    node !== undefined && (node.issue === undefined || node.issue === key);
  const placements = new Set(
    rows.flatMap((row, index) => {
      const parent = parentIndex(rows, index);
      return row.id === null ? [] : [`${row.id} ${parent === undefined ? "" : rows[parent]?.id}`];
    })
  );
  return answer.tuples.flatMap((tuple) => {
    const check = nodes.get(tuple.from);
    const statement = nodes.get(tuple.to);
    if (
      tuple.kind !== "serves" ||
      !own(check) ||
      !own(statement) ||
      check.supertag !== "check" ||
      check.mark !== "query" ||
      statement.supertag !== "requirement"
    ) {
      return [];
    }
    const [checkId, statementId] = [idOf(check.id), idOf(statement.id)];
    return placements.has(`${checkId} ${statementId}`)
      ? []
      : [`${checkId} is drawn under no row of ${statementId}, which it serves`];
  });
}

/** Each key the filter draws that the fixture's rows of it do not hold, and each the other way,
 *  that is of a kind root R2 draws under the filter: a difference R2 does not explain. */
function unexplainedDifferences(
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

// c-views.
// SLOW(>=2s): two seeded issues, each load waited out, and three filters each pressed, read
// until it holds still, and released.
test("each filter draws its view read's rows", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  for (const [name, seed] of VIEW_SEEDS) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      const views = {
        plan: viewAnswerOf(page, "plan"),
        spec: viewAnswerOf(page, "spec"),
        verification: viewAnswerOf(page, "verification"),
      } satisfies Record<(typeof VIEW_FILTERS)[number], TreeAnswerWatch>;
      const traffic = watchApiRequests(page);
      await firstLoad(page, traffic, seeded.key);
      const reads = await idReadsOf(seeded.key);
      const slugs = (reads.issue.artifacts ?? []).map((artifact) => artifact.slug);
      const keys = seedKeys(seeded, slugs);
      const recorded = fixtureSeed(name);
      for (const filter of VIEW_FILTERS) {
        const view = PRESET_NAMES[filter];
        const where = `${name} under ${view}`;
        const drawn = await pressFilter(page, traffic, filter);
        const answer = await heldAnswer(views[filter], `the ${view} view read`);
        if (filter === "spec") {
          expect(idsOfType(drawn.nodes, "decision"), "no decision row under Spec").toEqual([]);
          for (const kind of ["constraint", "ask", "comment", "message", "check", "plan_task"]) {
            expect(idsOfType(drawn.nodes, kind), `no ${kind} row under Spec`).toEqual([]);
          }
          expect(idsOfType(drawn.nodes, "callout"), "no memo row under Spec").toEqual([]);
        }
        const expected = viewNodeIds(answer, filter, reads);
        expect(
          idSet(drawn.nodes.map((node) => node.id)),
          `${where}, the rows are the view read's nodes of root R2's row kinds`
        ).toEqual(idSet(expected.map((node) => node.id)));
        expect(
          idSet(drawn.nodes.flatMap((node) => (node.context ? [node.id] : []))),
          `${where}, the rows drawn as context are the view read's context`
        ).toEqual(idSet(expected.flatMap((node) => (node.mark === "context" ? [node.id] : []))));
        if (filter === "verification") {
          expect(
            checksOutOfPlace(drawn.rows, answer, reads),
            `${where}, each check stands under each statement it serves`
          ).toEqual([]);
        }
        expect(
          drawn.count?.trim(),
          `${where}, the control counts the view read's own ${R2_COUNTED_KINDS[filter].join(", ")}`
        ).toBe(String(viewCount(answer, filter)));
        expect(
          unexplainedDifferences(filter, drawn.rows, keys, recorded),
          `${where}, each difference from the fixture's rows is a kind root R2 leaves out`
        ).toEqual([]);
        await releaseFilter(page, filter);
      }
    });
  }
});

// c-views' decision lines, owed until the page draws the decisions from the tree read's nodes,
// once decisions are nodes of the tree read on this branch's base (`partUGate`): each decision
// whose `shaped` tie names a statement under Spec is a line on that statement's row, never a row
// of its own.
// SLOW(>=2s): two seeded issues, each load waited out, and Spec pressed and read until it holds
// still.
test("each filter draws its view read's rows, and each decision that shaped a statement is a line on its row @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  for (const [, seed] of VIEW_SEEDS) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      const tree = treeAnswerOf(page);
      const decisions = decisionsReadOf(page, seeded.key);
      const spec = viewAnswerOf(page, "spec");
      const traffic = watchApiRequests(page);
      await firstLoad(page, traffic, seeded.key);
      owedUnless(partUGate(await heldAnswer(tree, "the tree read"), decisions.read()));
      const reads = await idReadsOf(seeded.key);
      const drawn = await pressFilter(page, traffic, "spec");
      const answer = await heldAnswer(spec, "the Spec view read");
      const idOf = treeIdOf(answer, reads);
      const nodes = new Map(answer.nodes.map((node) => [node.id, node]));
      const statements = new Set(drawn.rows.flatMap((row) => (row.id === null ? [] : [row.id])));
      const lines = answer.tuples.flatMap((tuple) => {
        const decision = nodes.get(tuple.from);
        const statement = idOf(tuple.to);
        if (tuple.kind !== "shaped" || decision?.supertag !== "decision") return [];
        if (!statements.has(statement)) return [];
        const words = (
          decision.text_tree === undefined ? decision.text : wordsOf(decision.text_tree)
        )
          .replace(/\s+/g, " ")
          .trim()
          .slice(0, 40)
          .trim();
        return words === "" ? [] : [{ statement, words }];
      });
      expect(
        lines.length,
        "the Spec view read holds a decision that shaped a statement the Spec filter draws"
      ).toBeGreaterThan(0);
      const missing: string[] = [];
      for (const { statement, words } of lines) {
        // The Spec rows held still across two readings in `pressFilter` above, with the page
        // quiet, so one reading of a drawn row's text is the line that row settled on.
        const text = ((await nodeRow(page, statement).first().textContent()) ?? "")
          .replace(/\s+/g, " ")
          .trim();
        if (!text.includes(words)) missing.push(`${statement} draws no line "${words}"`);
      }
      expect(
        missing,
        "each decision that shaped a statement is a line on that statement's row"
      ).toEqual([]);
      await releaseFilter(page, "spec");
    });
  }
});

// c-views for Decisions, owed on the model's decisions view of the tree read
// (`decisionsViewGate`): once the load reads the decisions as the tree read's `view=decisions`,
// the Decisions filter draws that view read's rows, as Spec, Verification and Plan draw theirs,
// which is how the root issue settled row 3's gap for Decisions.
// SLOW(>=2s): two seeded issues, each load waited out, and Decisions pressed, read until it holds
// still, and released.
test("each filter draws its view read's rows, and the Decisions filter draws its view read's rows @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  for (const [name, seed] of VIEW_SEEDS) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      const decisions = viewAnswerOf(page, "decisions");
      const traffic = watchApiRequests(page);
      const load = await firstLoad(page, traffic, seeded.key);
      owedUnless(decisionsViewGate(load, seeded.key));
      const reads = await idReadsOf(seeded.key);
      const view = PRESET_NAMES.decisions;
      const where = `${name} under ${view}`;
      const drawn = await pressFilter(page, traffic, "decisions");
      const answer = await heldAnswer(decisions, `the ${view} view read`);
      const expected = viewNodeIds(answer, "decisions", reads);
      expect(
        idSet(drawn.nodes.map((node) => node.id)),
        `${where}, the rows are the view read's nodes of root R2's row kinds`
      ).toEqual(idSet(expected.map((node) => node.id)));
      expect(
        idSet(drawn.nodes.flatMap((node) => (node.context ? [node.id] : []))),
        `${where}, the rows drawn as context are the view read's context`
      ).toEqual(idSet(expected.flatMap((node) => (node.mark === "context" ? [node.id] : []))));
      expect(
        drawn.count?.trim(),
        `${where}, the control counts the view read's own ${R2_COUNTED_KINDS.decisions.join(", ")}`
      ).toBe(String(viewCount(answer, "decisions")));
      await releaseFilter(page, "decisions");
    });
  }
});

// ---------------------------------------------------------------------------------------------
// Row 4: a write is drawn from the tree read again, with no reload.
// ---------------------------------------------------------------------------------------------

/** The id of the row the row whose words hold `words` stands under (null at the top), or
 *  undefined where no row holds them. */
function parentOfWords(readings: readonly RowReading[], words: string): string | null | undefined {
  const index = readings.findIndex((reading) => reading.words?.includes(words) === true);
  if (index < 0) return undefined;
  return parentIdOf(readings, index);
}

/** The state word the row of node `id` draws, or null where no row of it draws one. */
async function stateWordOf(page: Page, id: string): Promise<string | null> {
  const rows = await drawnRowReadings(page, { bodies: false });
  return rows.find((row) => row.id === id)?.state?.word ?? null;
}

/** Asserts what a write sends of the tree read: the read its event asks for - with the live feed
 *  down, its settle's - and one more where that read is still out as the other ask comes
 *  (`subscribeTreeFreshness`), never more. */
function expectOneOrTwoReads(reads: number, message: string): void {
  expect(reads, message).toBeGreaterThanOrEqual(1);
  expect(reads, message).toBeLessThanOrEqual(2);
}

// c-fresh, with its status clause: a status write sends one or two tree reads - its event's, and
// one more where the event lands while the settle's read is out - and needs no line at the foot of
// the outline.
// SLOW(>=2s): the whole seeded issue, its load waited out, a status written and waited out, and a
// reply sent and waited for until its row is drawn.
test("a write is drawn without a reload", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const traffic = watchApiRequests(page);
    await firstLoad(page, traffic, seeded.key);
    let loads = 0;
    page.on("load", () => {
      loads += 1;
    });

    const status = headerStatus(page);
    // The header draws only from the answered issue read, and the load above was waited out, so
    // the Status control's value once attached is the status the row starts from, before a pick.
    const was = await status.inputValue();
    const next = was === "testing" ? "todo" : "testing";
    const picked = traffic.requests.length;
    await status.selectOption(next);
    await expect
      .poll(async () => (await getIssue(seeded.key)).status, {
        message: "the status picked was never written",
      })
      .toBe(next);
    await expectLoadSettled(traffic);
    const sincePick = traffic.requests.slice(picked);
    const write = sincePick.findIndex(
      (request) => request.method === "PATCH" && request.path === `/api/v1/issues/${seeded.key}`
    );
    expect(write, "the status pick sent no write of the issue").toBeGreaterThanOrEqual(0);
    expectOneOrTwoReads(
      treeReads(sincePick.slice(write + 1), seeded.key, null),
      "the status write sends one or two tree reads"
    );
    await expect(status, "the issue's row reads the new status").toHaveValue(next);

    const reads = await idReadsOf(seeded.key);
    const specSlug = (reads.issue.artifacts ?? []).find(
      (artifact) => artifact.id === seeded.specArtifactId
    )?.slug;
    expect(specSlug, "the issue holds no spec document").toBeDefined();
    const question = blockNode(specSlug ?? "", seeded.openQuestion.blockId);
    await unfoldEveryBranch(page);
    await rowAct(await focusRow(page, question), "reply").click();
    owedUnless(await footLineGate(page));
    const sent = traffic.requests.length;
    await page.keyboard.type(REPLY_WORDS);
    // The line sends as the page's other writing boxes send (`submitOnModifiedEnter`).
    await page.keyboard.press("ControlOrMeta+Enter");
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          return parentOfWords(await drawnRowReadings(page, { bodies: false }), REPLY_WORDS);
        },
        { message: "the new comment is a row" }
      )
      .toBe(question);
    expect(loads, "the page reloaded to draw the new comment").toBe(0);
    await expectLoadSettled(traffic);
    expectOneOrTwoReads(
      treeReads(traffic.requests.slice(sent), seeded.key, null),
      "the write sends one or two tree reads"
    );
  });
});

// c-fresh's Keep clause, owed on the model's decisions view of the tree read
// (`decisionsViewGate`) for the Decisions view read, and on the page drawing the decisions from
// the tree read's nodes (`partUGate`), which records it once decisions are nodes on this
// branch's base.
// SLOW(>=2s): a seeded issue, its load waited out, a Keep in the outline waited for, Decisions
// pressed and a second Keep there waited for.
test("a write is drawn without a reload, and Keep is drawn kept in the outline and in the Decisions view @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedReviewList, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const tree = treeAnswerOf(page);
    const decisions = decisionsReadOf(page, seeded.key);
    const traffic = watchApiRequests(page);
    const load = await firstLoad(page, traffic, seeded.key);
    owedUnless(decisionsViewGate(load, seeded.key));
    owedUnless(partUGate(await heldAnswer(tree, "the tree read"), decisions.read()));
    await unfoldEveryBranch(page);
    const waiting = new Set(
      (await drawnRowReadings(page, { bodies: false })).flatMap((row) =>
        row.kind.type === "decision" && row.state?.token === "waiting_review" && row.id !== null
          ? [row.id]
          : []
      )
    );
    const [first, second] = [...waiting];
    if (first === undefined || second === undefined) {
      throw new Error(
        `${seeded.key} draws ${waiting.size} decisions that wait, and Keep needs two`
      );
    }

    const row = await focusRow(page, first);
    await row.locator('button[data-row-action="keep"]').click();
    await expect
      .poll(() => stateWordOf(page, first), { message: 'Keep makes its row read "Kept by" alice' })
      .toMatch(KEPT_BY_ALICE);

    await pressFilter(page, traffic, "decisions");
    const target = nodeRow(page, second).first();
    await target.focus();
    const kept = traffic.requests.length;
    await target.locator('button[data-row-action="keep"]').click();
    await expect
      .poll(() => stateWordOf(page, second), {
        message: "the decision reads kept in the Decisions view",
      })
      .toMatch(KEPT_BY_ALICE);
    await expectLoadSettled(traffic);
    expect(
      treeReads(traffic.requests.slice(kept), seeded.key, "decisions"),
      "Keep under Decisions sends the Decisions view read again"
    ).toBeGreaterThanOrEqual(1);
  });
});
