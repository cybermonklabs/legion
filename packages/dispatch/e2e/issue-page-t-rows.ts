import { type Browser, expect, type Page } from "@playwright/test";

import type { Actor, DispatchEvent, IssueTreeNode, IssueTreeRead } from "../web/src/api/types";
import { decisionNodeChanges } from "../web/src/features/issue-page/decision-review";
import { shortDay } from "../web/src/features/issue-page/node-time";
import { NODE_STATE_WORDS, PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import { type TreeIdReads, treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { getIssue, getIssueEvents, listIssueAsks } from "./api";
import { seedSpineCases, seedWholeIssue } from "./issue-layout-seed";
import { gotoIssue, phase, presetCount, presetToggle } from "./issue-page";
import { expectIssuePageDrawn, unfoldEveryBranch } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import {
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
  owedUnless,
  R2_ROW_KINDS,
  seedEventsLanded,
  type TreeView,
  treeAnswerOf,
  treeReadGate,
} from "./issue-page-t-tree";
import { isLiveSocket } from "./live-socket-watch";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * What the rows of the new issue page that draw it from the tree read share: the outline drawn
 * from the issue's tree read (`GET /api/v1/issues/{key}/tree`) and each filter from that read's
 * view (`?view=`), read on the real stack. Each row turns the new page on for alice with `phase`
 * (1440 by 900), seeds one of the issues of `e2e/issue-layout-seed.ts`, opens it with
 * `gotoIssue`, and reads it through `unfoldEveryBranch`, `pickPreset`, `watchApiRequests` and
 * `expectLoadSettled` (`e2e/traffic.ts`), the helpers of `e2e/issue-page-t.ts` and
 * `e2e/issue-page-t-tree.ts`, and the steps and comparisons of this file.
 *
 * The rows are the four of the Verification document's browser checks, by their titles. They
 * stand in four spec files that hold only their rows, each about a minute of one browser shard
 * of CI, since one file of them all took most of a shard's limit:
 *
 * 1. `c-same-rows`: "the outline drawn from the tree read holds the rows the reads drew", one row
 *    per seed, so the check's `-g` runs all 14 and each ends inside CI's nine minutes:
 *    `e2e/issue-page-t-same-rows-a.e2e.ts` holds the first nine of `SEEDS` in their order
 *    (`SAME_ROWS_A_SEEDS`) and `e2e/issue-page-t-same-rows-b.e2e.ts` the last five
 *    (`SAME_ROWS_B_SEEDS`), a split by time (`SAME_ROWS_A`). Each row stands under the node the
 *    answer names as its parent, or its nearest drawn ancestor, in the answer's order; that is
 *    read from the answer and never from the fixture, so the check's control fails on the parent
 *    first. Every other reading is the fixture's (`e2e/fixtures/issue-page-t-rows.json`), by
 *    key, as the page opens and with every branch open, and each time is its source's in this
 *    run.
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

/** What row 1 reads of one seed's page. */
interface OutlineRead {
  readonly answer: IssueTreeRead;
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
  outline: OutlineRead,
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
 *  decision row's review state is the answer's, each Change stands under its decision's row, the
 *  section that restates a person's answer is no row, and the answered question's row carries a
 *  chip for each node its `shaped` ties name. */
function expectDecisionsFromNodes(outline: OutlineRead, recorded: SeedFixture): void {
  const { answer, rows } = outline;
  const idOf = treeIdOf(answer, outline.reads);
  const held = heldNodes(answer, idOf);
  const nodes = new Map((answer.nodes ?? []).map((node) => [node.id, node]));
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

  const strays = (answer.nodes ?? []).flatMap((node) => {
    const decision = node.parent === null ? undefined : nodes.get(node.parent.id);
    if (
      node.supertag !== "comment" ||
      node.author?.[0]?.kind !== "user" ||
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

/** Row 1 on the seed `name`, which `seed` seeds: each row stands under the parent the tree read
 *  answers, as the page opens and with every branch open, and reads as the reads drew it. */
export async function expectSameRows(
  browser: Browser,
  name: string,
  seed: NamedSeed[1]
): Promise<void> {
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
    const outline = await readOutline(page, seeded, { bodies: false });
    owedUnless(decisionNodesGate(outline.answer, outline.decisions));
    expectDecisionsFromNodes(outline, fixtureSeed(name));
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
  const idOf = treeIdOf(answer, reads);
  const key = reads.issue.key;
  const nodes = new Map((answer.nodes ?? []).map((node) => [node.id, node]));
  const own = (node: IssueTreeNode | undefined): node is IssueTreeNode =>
    node !== undefined && (node.issue === undefined || node.issue === key);
  const placements = new Set(
    rows.flatMap((row, index) => {
      const parent = parentIndex(rows, index);
      return row.id === null ? [] : [`${row.id} ${parent === undefined ? "" : rows[parent]?.id}`];
    })
  );
  return (answer.tuples ?? []).flatMap((tuple) => {
    const check = nodes.get(tuple.from.id);
    const statement = nodes.get(tuple.to.id);
    if (
      tuple.kind !== "serves" ||
      !own(check) ||
      !own(statement) ||
      check.supertag !== "check" ||
      check.role !== "query" ||
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
