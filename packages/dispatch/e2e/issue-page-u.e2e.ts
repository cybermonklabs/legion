import type { IssueTreeAnswer, IssueTreeNode } from "../web/src/api/client";
import type { IssueDetails } from "../web/src/api/types";
import { decisionNodeChanges } from "../web/src/features/issue-page/decision-review";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import { CONTROL_LABELS, NODE_STATE_WORDS } from "../web/src/features/issue-page/page-words";
import { type TreeIdReads, treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { getIssue, listComments } from "./api";
import { seedDecisionsFilter, seedReviewList } from "./issue-layout-seed";
import { gotoIssue, issueRead, phase } from "./issue-page";
import {
  drawnRows,
  expectIssuePageDrawn,
  nodeRow,
  parentOfRow,
  rowOf,
  rowWords,
  unfoldEveryBranch,
} from "./issue-page-g";
import { waitingCount } from "./issue-page-p";
import {
  type DrawnPart,
  decisionsFoot,
  decisionsPartRows,
  decisionsPartsDrawn,
} from "./issue-page-q";
import {
  blocksReads,
  convertedGate,
  decisionNodesGate,
  decisionsReadOf,
  drawnRowReadings,
  fixtureSeed,
  heldAnswer,
  keyedRows,
  owedUnless,
  seedKeys,
  steadily,
  type TreeView,
  treeAnswerOf,
  treeReadGate,
  treeReads,
} from "./issue-page-t";
import {
  askOfNode,
  askRowId,
  decisionNodesOf,
  decisionRowId,
  decisionSum,
  decisionSumOf,
  type FilterReading,
  heldDecisionsRead,
  holdTreeReads,
  idReadsOf,
  readFilter,
  releaseFilter,
  reviewWordOf,
  rewordThirdDecision,
  type SentWrite,
  type SpecColumnRow,
  specColumn,
  statementsBeside,
  THIRD_DECISION,
  viewNodesOf,
  watchWrites,
  writeThirdDecision,
} from "./issue-page-u";
import { type Browser, expect, type Page, test } from "./test";
import { type ApiRequest, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * The decision rows of the new issue page:
 * every decision the page draws is a node of the issue's tree read (`GET /api/v1/issues/{key}/tree`)
 * and its views (`?view=`), with the review state the model answers on it, and Keep and Change
 * write against that node, read on the real stack. Each row turns the new page on for alice with
 * `phase` (1440 by 900), seeds the issues of `e2e/issue-layout-seed.ts`, opens them with
 * `gotoIssue` (`e2e/issue-page.ts` holds both), and reads them through `nodeRow`, `rowOf`,
 * `drawnRows`, `parentOfRow` and `unfoldEveryBranch` (`e2e/issue-page-g.ts`), `pickPreset`
 * (`e2e/issue-page-i.ts`), `waitingCount` (`e2e/issue-page-p.ts`), the readers of the Decisions
 * view (`e2e/issue-page-q.ts`), `watchApiRequests` (`e2e/traffic.ts`), the helpers of
 * `e2e/issue-page-t.ts` and the rows' own
 * (`e2e/issue-page-u.ts`).
 *
 * The rows are the two of the Verification document's browser checks, by their titles:
 *
 * 1. `c-decisions`: "every decision the page draws is a node with the review state the model
 *    holds", on `seedDecisionsFilter` and `seedReviewList`.
 * 2. `c-keep-change`: "Keep and Change act on the decision node", on `seedReviewList`.
 *
 * Each row checks its gates first, each with `owedUnless`, and skips where one is not on the
 * base, with an `owed` annotation naming the gate and the decisions that make it owed; it never
 * passes then. The gates are the server's tree read (`treeReadGate`), an issue whose documents
 * are nodes of its tree (`convertedGate`), and the seeds written as decision nodes of the tree
 * read (`decisionNodesGate`, over the decisions read `decisionsReadOf` keeps). The first two are
 * read before anything else once the page is open, the third once the page has received its
 * decisions read, which the page sends only once Decisions is pressed. On this branch's base the
 * server serves no tree read, so both rows are tagged `@owed` and skip on the first gate.
 */

/** How long row 1 may take: two seeded issues, each read through all four filters. */
const ROW_ONE_MS = 480_000;

/** How long row 2 may take. */
const ROW_TWO_MS = 240_000;

/** The four parts of the Decisions view, in the order it draws them (`data-decisions-part`). */
const DECISIONS_PARTS = ["open_questions", "review", "yours", "agents"] as const;
type DecisionsPart = (typeof DECISIONS_PARTS)[number];

/** The parts whose order no rule of the view decides, compared as sets. */
const UNORDERED_PARTS: Readonly<Partial<Record<string, true>>> = {
  agents: true,
  open_questions: true,
};

/** The four filters, in the order the view row draws them. */
const FILTERS = [
  "spec",
  "verification",
  "plan",
  "decisions",
] as const satisfies readonly TreeView[];

/** Keep and Change on a decision's row (`DecisionActions.tsx`'s `data-row-action`). */
const KEEP = 'button[data-row-action="keep"]';
const CHANGE = 'button[data-row-action="change"]';

/** A decision row's state word once alice kept it. */
const KEPT_BY_ALICE = /^Kept by\b.*\balice\b/i;

/** The words row 2's Change sends. */
const CHANGE_WORDS = "Tie it to the second statement instead.";

/** What every seed returns: the key of the issue the page opens. */
interface SeededIssue {
  readonly key: string;
}

/** `rows` of the part `part`, in the order the part draws them where its order is the view's, and
 *  sorted where it is not (`UNORDERED_PARTS`). */
function partOrder(part: string, rows: readonly string[]): readonly string[] {
  return UNORDERED_PARTS[part] === true ? [...rows].sort() : rows;
}

/** The text `waitingCount` draws where something waits on alice, and null where it draws the
 *  agents' turn or nothing. Each caller reads it inside `expect.poll`, never once. */
async function waitingOnYou(page: Page): Promise<string | null> {
  const count = waitingCount(page);
  // Every caller polls this read, which asks again until the count settles: one answer, one try.
  if ((await count.count()) === 0) return null;
  // The same poll reads these words again until they settle, so one answer here is one try too.
  const text = ((await count.textContent()) ?? "").replace(/\s+/g, " ").trim();
  return text.startsWith(PROPERTY_LABELS.waiting_on_you) ? text : null;
}

/** What the header's count draws for `waiting` things that wait on alice. */
function waitingWords(waiting: number): string | null {
  return waiting === 0 ? null : `${PROPERTY_LABELS.waiting_on_you} (${waiting})`;
}

/** The open questions of the issue that wait on alice, as the header counts them (`waiting.ts`):
 *  open, a person's turn, alice's, and no approval request. The header's rows carry no snooze. */
function asksWaitingOnAlice(issue: IssueDetails): number {
  return (issue.open_asks ?? []).filter(
    (ask) =>
      ask.state === "open" &&
      ask.kind !== "approval" &&
      ask.waiting_on === "human" &&
      ask.waiting_on_person === "alice"
  ).length;
}

/** The row of node `id`'s state word, as `drawnRows` reads it. */
async function stateWordOf(page: Page, id: string): Promise<string | null> {
  return rowOf(await drawnRows(page), id)?.stateWord ?? null;
}

// ---------------------------------------------------------------------------------------------
// Row 1: every decision the page draws is a node, with the review state the model holds.
// ---------------------------------------------------------------------------------------------

/** How row 1 names the rows of one seed: by the id the tree draws a node of the tree read by
 *  (`treeIdOf`), an ask by its row (`askRowId`), and the open questions of the asks read. */
interface RowNames {
  readonly ask: (askId: string) => string;
  readonly node: (readId: string) => string;
  readonly openAsks: readonly string[];
}

/** One decision of the Spec column as row 1 expects it: the names its cell and its links go by,
 *  and the statements it stands beside. */
interface ColumnEntry {
  readonly decision: string;
  readonly names: readonly string[];
  readonly statements: readonly string[];
}

/** What row 1 holds one seed to. */
interface RowOneWanted {
  /** The declined ask's row, which reads "Declined"; null on a seed that declines none. */
  readonly declined: string | null;
  /** What the seed's four parts count and its Spec column holds, as the Decisions view's row and
   *  the Spec column's row assert them on the same decisions (`issue-page-q.e2e.ts`,
   *  `issue-page-j.e2e.ts`); null on a seed no such row asserts them for. */
  readonly departure: {
    readonly column: readonly ColumnEntry[];
    readonly counts: Readonly<Record<DecisionsPart, string>>;
  } | null;
  /** The rows each part holds, by the ids they carry. */
  readonly parts: Readonly<Record<DecisionsPart, readonly string[]>>;
}

/** What the page sent of the decisions: reads of the decisions route, of a document's blocks, and
 *  of the tree's Decisions view. */
function decisionsSent(requests: readonly ApiRequest[], key: string) {
  const path = `/api/v1/issues/${encodeURIComponent(key)}/decisions`;
  return {
    blocks: blocksReads(requests),
    decisions: requests.flatMap((request) =>
      new URL(request.path, "http://requests.invalid").pathname === path
        ? [`${request.method} ${request.path}`]
        : []
    ),
    views: treeReads(requests, key, "decisions"),
  };
}

/** Every `shaped` tie of the tree read from a decision of this issue to a statement of it: from a
 *  decision node that was not replaced, since a replaced one stands beside no statement in the
 *  Spec column, or from an ask the asks read says is answered, which is a person's decision. */
function tiesToStatements(
  answer: IssueTreeAnswer,
  reads: TreeIdReads
): { readonly from: IssueTreeNode; readonly to: IssueTreeNode }[] {
  const nodes = new Map(answer.nodes.map((node) => [node.id, node]));
  return answer.tuples.flatMap((tuple) => {
    const from = nodes.get(tuple.from);
    const to = nodes.get(tuple.to);
    if (
      tuple.kind !== "shaped" ||
      from === undefined ||
      to === undefined ||
      from.issue !== undefined ||
      to.issue !== undefined ||
      to.supertag !== "requirement"
    ) {
      return [];
    }
    const decides =
      (from.supertag === "decision" && from.replaced !== true) ||
      (from.supertag === "ask" && askOfNode(from, reads)?.state === "answered");
    return decides ? [{ from, to }] : [];
  });
}

/** The names a decision's cell and links go by: its row id, its own id, and for a person's
 *  decision the id of the ask they answered. */
function decisionNames(
  node: IssueTreeNode,
  reads: TreeIdReads,
  idOf: (readId: string) => string
): string[] {
  const ask = node.supertag === "ask" ? askOfNode(node, reads) : undefined;
  return [idOf(node.id), node.id, ...(ask === undefined ? [] : [ask.id])];
}

/** The decisions of the tree read that wait for alice's review, as the header counts them: a
 *  decision node of this issue the read answers as waiting, not replaced, that a `shaped` tie
 *  binds to a statement of this issue, and that no Change hangs under. */
function decisionsWaitingOnAlice(answer: IssueTreeAnswer): number {
  const nodes = new Map(answer.nodes.map((node) => [node.id, node]));
  const changes = decisionNodeChanges(answer);
  return answer.nodes.filter(
    (node) =>
      node.supertag === "decision" &&
      node.issue === undefined &&
      node.waiting_review === true &&
      node.replaced !== true &&
      !changes.has(node.id) &&
      answer.tuples.some((tuple) => {
        const to = nodes.get(tuple.to);
        return (
          tuple.kind === "shaped" &&
          tuple.from === node.id &&
          to?.supertag === "requirement" &&
          to.issue === undefined
        );
      })
  ).length;
}

/** Each filter as the rows fixture (`fixtureSeed`) records it: the keys of its rows (`seedKeys`,
 *  `keyedRows`) and its control's count. */
function recordedFilters(
  filters: Readonly<Record<TreeView, FilterReading>>,
  keys: ReadonlyMap<string, string>
) {
  return Object.fromEntries(
    FILTERS.map((filter) => [
      filter,
      {
        count: Number((filters[filter].count ?? "").trim()),
        rows: keyedRows(filters[filter].rows, keys).map((row) => row.key),
      },
    ])
  );
}

/** Row 1 on one seed, read from the page `phase` opened. */
async function readRowOne(
  page: Page,
  name: string,
  seeded: SeededIssue,
  wantedOf: (names: RowNames) => RowOneWanted
): Promise<void> {
  const { key } = seeded;
  owedUnless(await treeReadGate(page, key));
  owedUnless(await convertedGate(page, key));
  const tree = treeAnswerOf(page);
  const decisions = decisionsReadOf(page, key);
  const traffic = watchApiRequests(page);
  await gotoIssue(page, `/issues/${key}`);
  await expectIssuePageDrawn(page);
  await steadily(page, traffic, `${key} as the page opens`, async () => ({
    rows: await drawnRowReadings(page, { bodies: false }),
    total: traffic.total(),
  }));
  const load = [...traffic.requests];
  const decisionsFilter = await readFilter(page, traffic, "decisions");
  const pressed = traffic.requests.slice(load.length);
  const answer = await heldAnswer(tree, `${key}'s tree read`);
  owedUnless(decisionNodesGate(answer, await heldDecisionsRead(key, decisions)));
  expect(answer.converted, "the tree read answers converted: true").toBe(true);

  // (1) The decisions are the tree read's: the load sends no read of the decisions route, no
  // document's blocks and no Decisions view, and Decisions pressed sends its view read once and no
  // read of the decisions route or of any document's blocks.
  const loadSent = decisionsSent(load, key);
  const pressSent = decisionsSent(pressed, key);
  expect(
    {
      load: loadSent,
      pressed: { blocks: pressSent.blocks, decisions: pressSent.decisions, views: pressSent.views },
    },
    "the page sends no decisions read"
  ).toEqual({
    load: { blocks: [], decisions: [], views: 0 },
    pressed: { blocks: [], decisions: [], views: 1 },
  });

  // (2) The four parts hold the seed's decisions in the view's order, each reading its state from
  // its node in the Decisions view's read, with the foot unfolded so every decision is drawn.
  const reads = await idReadsOf(key);
  const idOf = treeIdOf(answer, reads);
  const wanted = wantedOf({
    ask: (askId) => askRowId(reads, askId),
    node: idOf,
    openAsks: (reads.asks ?? []).flatMap((ask) =>
      ask.state === "open" && ask.kind !== "approval" ? [askRowId(reads, ask.id)] : []
    ),
  });
  const foot = decisionsFoot(page);
  // `readFilter` above read the view once its rows and count held still with the network quiet,
  // so its foot is drawn by now or not at all, and one read says which.
  if ((await foot.count()) > 0) {
    await expect(foot, "the Decisions view's foot draws its control").toBeVisible();
    await foot.click();
  }
  const states = Object.fromEntries(
    (await viewNodesOf(key, "decisions")).flatMap((node) =>
      node.supertag === "decision" && node.issue === undefined && node.mark !== "context"
        ? [[decisionRowId(node), reviewWordOf(node)]]
        : []
    )
  );
  const partsAndStates = async () => {
    const drawn = await decisionsPartsDrawn(page);
    const rows = await drawnRowReadings(page, { bodies: false });
    return {
      declined:
        wanted.declined === null
          ? null
          : (rows.find((row) => row.id === wanted.declined)?.state?.word ?? null),
      parts: Object.fromEntries(
        DECISIONS_PARTS.map((part) => [
          part,
          partOrder(part, drawn.find((one) => one.part === part)?.rows ?? []),
        ])
      ),
      states: Object.fromEntries(
        rows.flatMap((row) =>
          row.kind.type === "decision" && row.id !== null ? [[row.id, row.state?.word ?? null]] : []
        )
      ),
    };
  };
  await expect
    .poll(partsAndStates, { message: "each part holds its decisions with their states" })
    .toEqual({
      declined: wanted.declined === null ? null : NODE_STATE_WORDS.ask.declined,
      parts: Object.fromEntries(
        DECISIONS_PARTS.map((part) => [part, partOrder(part, wanted.parts[part])])
      ),
      states,
    });
  const parts: readonly DrawnPart[] = await decisionsPartsDrawn(page);
  await releaseFilter(page, "decisions");

  // (3) Under Spec, each decision, the answered ask with `shaped` ties among them, stands beside
  // each statement its tie names: its cell spans the statement, or the statement's line links it.
  const specFilter = await readFilter(page, traffic, "spec");
  const column: readonly SpecColumnRow[] = await steadily(
    page,
    traffic,
    "the Spec view's column",
    () => specColumn(page)
  );
  const ties = tiesToStatements(answer, reads);
  expect(ties.length, "the tree read holds a decision whose tie names a statement").toBeGreaterThan(
    0
  );
  expect(
    ties.flatMap(({ from, to }) =>
      statementsBeside(column, decisionNames(from, reads, idOf)).includes(idOf(to.id))
        ? []
        : [`${idOf(from.id)} stands beside no row of ${idOf(to.id)}, which its tie names`]
    ),
    "under Spec, each decision stands beside each statement its tie names"
  ).toEqual([]);
  await releaseFilter(page, "spec");

  const verificationFilter = await readFilter(page, traffic, "verification");
  await releaseFilter(page, "verification");
  const planFilter = await readFilter(page, traffic, "plan");
  await releaseFilter(page, "plan");

  // (4) The header's count of what waits: alice's open questions and the decisions that wait
  // for her review, as the tree read answers them.
  const issue = await getIssue(key);
  const asks = asksWaitingOnAlice(issue);
  await expect
    .poll(() => waitingOnYou(page), {
      message: "the header's count of what waits counts the decisions that wait for alice's review",
    })
    .toBe(waitingWords(asks + decisionsWaitingOnAlice(answer)));

  // (5) Each filter's row ids and count are the rows fixture's for the seed, the four parts and
  // the Spec column are the values row 1 expects of the seed, and the header's count is alice's
  // open questions and the review part's entries.
  const slugs = (reads.issue.artifacts ?? []).map((artifact) => artifact.slug);
  expect(
    recordedFilters(
      {
        decisions: decisionsFilter,
        plan: planFilter,
        spec: specFilter,
        verification: verificationFilter,
      },
      seedKeys(seeded, slugs)
    ),
    "each filter's row ids and count equal the rows fixture for the seed"
  ).toEqual(fixtureSeed(name).filters);
  const { departure } = wanted;
  if (departure === null) return;
  expect(
    parts.map(({ count, part, rows }) => ({
      count,
      part,
      rows: partOrder(part, rows),
    })),
    "the four parts equal the values row 1 expects"
  ).toEqual(
    DECISIONS_PARTS.map((part) => ({
      count: departure.counts[part],
      part,
      rows: partOrder(part, wanted.parts[part]),
    }))
  );
  expect(
    departure.column.map(({ decision, names }) => ({
      decision,
      statements: statementsBeside(column, names),
    })),
    "the Spec column equals the values row 1 expects"
  ).toEqual(departure.column.map(({ decision, statements }) => ({ decision, statements })));
  const review = parts.find((part) => part.part === "review")?.rows.length ?? 0;
  await expect
    .poll(() => waitingOnYou(page), {
      message:
        "the header's count equals the open questions that wait on alice and the review part's entries",
    })
    .toBe(waitingWords(asks + review));
}

/** Row 1 on one seed, by the name the rows fixture records it under. */
function rowOneSeed<S extends SeededIssue>(
  name: string,
  seed: () => Promise<S>,
  wanted: (seeded: S, names: RowNames) => RowOneWanted
): (browser: Browser) => Promise<void> {
  return (browser) =>
    phase(browser, seed, (page, seeded) =>
      readRowOne(page, name, seeded, (names) => wanted(seeded, names))
    );
}

/** Row 1's two seeds. On `seedDecisionsFilter` the parts are the ones the Decisions view's first
 *  row asserts (review `[D(5), D(0)]`, yours `[D(4), declined, optioned, answered]`), and the
 *  Spec column is the one the Spec column's row asserts on the same decisions; on `seedReviewList`
 *  the parts are the ones the Decisions view's review-list row asserts. */
const ROW_ONE_SEEDS = [
  rowOneSeed("seedDecisionsFilter", seedDecisionsFilter, (seeded, names) => {
    const block = (index: number) => seeded.decisions[index] ?? "";
    const D = (index: number) => names.node(block(index));
    const r = (index: number) => names.node(seeded.requirements[index - 1] ?? "");
    const decision = (index: number) => [D(index), block(index)];
    const declined = names.ask(seeded.declinedAskId);
    const answered = names.ask(seeded.answeredAskId);
    return {
      declined,
      departure: {
        column: [
          {
            decision: "the person's decision",
            names: [answered, seeded.answeredAskId],
            statements: [r(3), r(4), r(5)],
          },
          { decision: "decision 0", names: decision(0), statements: [r(1), r(2)] },
          { decision: "decision 4", names: decision(4), statements: [r(4)] },
          { decision: "decision 5", names: decision(5), statements: [r(5)] },
          { decision: "the replaced decision", names: decision(6), statements: [] },
        ],
        counts: { agents: "3", open_questions: "1", review: "2", yours: "4" },
      },
      parts: {
        agents: [D(2), D(3), D(6)],
        open_questions: names.openAsks,
        review: [D(5), D(0)],
        yours: [D(4), declined, names.ask(seeded.optionedAskId), answered],
      },
    };
  }),
  rowOneSeed("seedReviewList", seedReviewList, (seeded, names) => ({
    declined: null,
    departure: null,
    parts: {
      agents: [names.node(seeded.tiedToNote), names.node(seeded.untied)],
      open_questions: names.openAsks,
      review: [names.node(seeded.tiedNewer), names.node(seeded.tiedOlder)],
      yours: [],
    },
  })),
];

// c-decisions, owed: on this branch's base the server serves no tree read, so the row skips on
// the gate of the server's tree read (`treeReadGate`). It waits too on an issue whose documents
// are nodes of its tree (`convertedGate`), and on the seeds written as decision nodes of the tree
// read (`decisionNodesGate`, read once Decisions is pressed). The check holds it owed until the
// server on the base meets all three gates.
// SLOW(>=2s): two seeded issues, each load waited out, and the four filters each pressed, read
// until it holds still, and released.
test("every decision the page draws is a node with the review state the model holds @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_ONE_MS);
  for (const run of ROW_ONE_SEEDS) await run(browser);
});

// ---------------------------------------------------------------------------------------------
// Row 2: Keep and Change act on the decision node.
// ---------------------------------------------------------------------------------------------

/** The path of a Keep of the decision node `node`: its own document and its block. */
function keepPath(node: IssueTreeNode): string {
  const document = encodeURIComponent(node.document?.artifact_id ?? "");
  return `/api/v1/artifacts/${document}/blocks/${encodeURIComponent(node.id)}/keep`;
}

/** A field of a request body, where the body is an object that holds it. */
function fieldOf(body: unknown, field: string): unknown {
  return typeof body === "object" && body !== null
    ? (body as Record<string, unknown>)[field]
    : undefined;
}

/** Each Keep `writes` holds: its path, and the `text_sha256` it sent. */
function keepsOf(writes: readonly SentWrite[]) {
  return writes.flatMap((write) =>
    write.method === "POST" && /^\/api\/v1\/artifacts\/[^/]+\/blocks\/[^/]+\/keep$/.test(write.path)
      ? [{ path: write.path, text_sha256: fieldOf(write.body, "text_sha256") }]
      : []
  );
}

/** Each comment of issue `key` `writes` holds (`POST /api/v1/issues/{key}/comments`). */
function commentsOf(writes: readonly SentWrite[], key: string): SentWrite[] {
  const path = `/api/v1/issues/${encodeURIComponent(key)}/comments`;
  return writes.filter((write) => write.method === "POST" && write.path === path);
}

// c-keep-change, owed: on this branch's base the server serves no tree read, so the row skips on
// the gate of the server's tree read (`treeReadGate`). It waits too on an issue whose documents
// are nodes of its tree (`convertedGate`), and on the seeds written as decision nodes of the tree
// read (`decisionNodesGate`, read once Decisions is pressed): the check holds it owed until the
// server on the base meets those three gates. Its Change clause asserts `anchor.block_id` on the
// captured comment request, which the page sends (`DecisionActions.tsx`' `ChangeAnchor`); no
// gate checks that the server reads it.
// SLOW(>=2s): a seeded issue with a third decision written, a Keep and a Change each waited on
// until the server's reads redraw the list, a reload, and a refused Keep under held tree reads.
test("Keep and Change act on the decision node @owed", async ({ browser }) => {
  test.setTimeout(ROW_TWO_MS);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const { key } = seeded;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    // Before the page opens: a third decision the planner writes in the spec, tied to `r1`, so it
    // waits too and, newest of the three, heads the review list.
    const spec = await writeThirdDecision(key);
    const tree = treeAnswerOf(page);
    const decisions = decisionsReadOf(page, key);
    const traffic = watchApiRequests(page);
    const writes = watchWrites(page);
    await gotoIssue(page, `/issues/${key}`);
    await expectIssuePageDrawn(page);
    let loads = 0;
    page.on("load", () => {
      loads += 1;
    });
    await readFilter(page, traffic, "decisions");
    const answer = await heldAnswer(tree, `${key}'s tree read`);
    owedUnless(decisionNodesGate(answer, await heldDecisionsRead(key, decisions)));
    expect(answer.converted, "the tree read answers converted: true").toBe(true);

    const [newer, older, third] = await decisionNodesOf(key, [
      seeded.tiedNewer,
      seeded.tiedOlder,
      THIRD_DECISION.block,
    ]);
    const ids = {
      newer: decisionRowId(newer),
      older: decisionRowId(older),
      third: decisionRowId(third),
    };
    await expect
      .poll(() => decisionsPartRows(page, "review"), {
        message: "the three decisions that wait are the review list, the third written heading it",
      })
      .toEqual([ids.third, ids.newer, ids.older]);

    // (1) Keep on the first, `tiedNewer`: one POST against the node's own document and block,
    // with the sum the tree read answers on it; the row reads kept by alice and leaves the list.
    const sum = decisionSumOf(key, newer);
    const keep = nodeRow(page, ids.newer).first().locator(KEEP);
    await expect(keep, "the first decision, tiedNewer, draws its Keep control").toBeVisible();
    const kept = writes.length;
    await keep.click();
    await expect
      .poll(() => keepsOf(writes.slice(kept)).length, {
        message: "Keep on the first decision sends no request",
      })
      .toBeGreaterThan(0);
    await expectLoadSettled(traffic);
    expect(
      keepsOf(writes.slice(kept)),
      "Keep sends one POST with the node's own document, block id and text_sha256"
    ).toEqual([{ path: keepPath(newer), text_sha256: sum }]);
    await expect
      .poll(
        async () => ({
          inReview: (await decisionsPartRows(page, "review")).includes(ids.newer),
          state: await stateWordOf(page, ids.newer),
        }),
        { message: 'the kept decision reads "Kept by" alice and leaves the review list' }
      )
      .toEqual({ inReview: false, state: expect.stringMatching(KEPT_BY_ALICE) });
    expect(loads, "the page reloaded to draw the Keep").toBe(0);

    // (2) Change on the second, `tiedOlder`, with words typed: the comment it sends is anchored
    // to the node's own document and block, read from the request before anything drawn.
    const olderRow = nodeRow(page, ids.older).first();
    const change = olderRow.locator(CHANGE);
    await expect(change, "the second decision, tiedOlder, draws its Change control").toBeVisible();
    await change.click();
    const field = olderRow.getByLabel(CONTROL_LABELS.your_change);
    await expect(field, `the Change draws its "${CONTROL_LABELS.your_change}" field`).toBeVisible();
    await field.fill(CHANGE_WORDS);
    const send = olderRow.getByRole("button", { exact: true, name: CONTROL_LABELS.send_change });
    await expect(
      send,
      `the Change draws its "${CONTROL_LABELS.send_change}" control`
    ).toBeEnabled();
    const changed = writes.length;
    await send.click();
    await expect
      .poll(() => commentsOf(writes.slice(changed), key).length, {
        message: "the Change sends no comment",
      })
      .toBeGreaterThan(0);
    await expectLoadSettled(traffic);
    const sent = commentsOf(writes.slice(changed), key);
    const anchor = fieldOf(sent[0]?.body, "anchor");
    expect(
      {
        artifact: fieldOf(anchor, "artifact"),
        block_id: fieldOf(anchor, "block_id"),
        comments: sent.length,
      },
      "the Change anchors to its decision's block"
    ).toEqual({ artifact: older.document?.artifact_id, block_id: older.id, comments: 1 });
    await expect
      .poll(
        async () => ({
          inReview: (await decisionsPartRows(page, "review")).includes(ids.older),
          state: await stateWordOf(page, ids.older),
        }),
        { message: 'the changed decision reads "Change requested" and leaves the review list' }
      )
      .toEqual({ inReview: false, state: NODE_STATE_WORDS.decision.change_requested });
    const comment = (await listComments(key)).find((one) => one.body === CHANGE_WORDS);
    expect(comment, "the server holds no comment for the Change").toBeDefined();
    const commentId = comment?.id ?? "";
    await releaseFilter(page, "decisions");
    const parentOfChange = async () => {
      await unfoldEveryBranch(page);
      return parentOfRow(page, commentId);
    };
    await expect
      .poll(parentOfChange, { message: "the Change is a row under its decision" })
      .toBe(ids.older);
    await page.reload();
    await issueRead(page);
    await expectIssuePageDrawn(page);
    await expect
      .poll(parentOfChange, { message: "the Change is a row under its decision, after a reload" })
      .toBe(ids.older);

    // (3) With the page's next tree reads held, the third decision's words change through the
    // API and its Keep sends the sum of the words the page drew: Dispatch refuses it, the row
    // still waits with its alert, and once the held reads go on the row draws the new words.
    await readFilter(page, traffic, "decisions");
    const thirdRow = nodeRow(page, ids.third).first();
    const thirdKeep = thirdRow.locator(KEEP);
    await expect(thirdKeep, "the third decision draws its Keep control").toBeVisible();
    const drawnSum = await decisionSum(key, THIRD_DECISION.block);
    const held = await holdTreeReads(page, key);
    await rewordThirdDecision(spec);
    expect(
      await decisionSum(key, THIRD_DECISION.block),
      "the edit leaves the third decision's sum as it was"
    ).not.toBe(drawnSum);
    const refusedFrom = writes.length;
    const answered = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === keepPath(third)
    );
    await thirdKeep.click();
    const refused = await answered;
    expect(
      { keeps: keepsOf(writes.slice(refusedFrom)), status: refused.status() },
      "the Keep sends the sum of the words the page drew, and Dispatch refuses it"
    ).toEqual({ keeps: [{ path: keepPath(third), text_sha256: drawnSum }], status: 409 });
    const refusal = fieldOf(await refused.json(), "error");
    await expect(
      thirdRow.getByRole("alert"),
      "the refused Keep draws its alert, in Dispatch's words"
    ).toHaveText(String(refusal));
    expect(
      await stateWordOf(page, ids.third),
      'the refused decision keeps "Waiting for review"'
    ).toBe(NODE_STATE_WORDS.decision.waiting_review);
    await expect
      .poll(() => held.count(), { message: "the refused Keep reads the tree again" })
      .toBeGreaterThan(0);
    await held.release();
    await expect
      .poll(
        async () =>
          ((await rowWords(nodeRow(page, ids.third).first()).textContent()) ?? "").replace(
            /\s+/g,
            " "
          ),
        { message: "once the held reads go on, the row draws the decision's new words" }
      )
      .toContain(THIRD_DECISION.reworded);
  });
});
