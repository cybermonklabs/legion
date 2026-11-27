/** the server's decision nodes, their review state and the comment under each, read through the
 *  tree and its views */

import { MIN_REFETCH_INTERVAL_MS } from "../web/src/api/query-refresh";
import type { IssueDetails, IssueTreeNode, IssueTreeRead } from "../web/src/api/types";
import { chipName } from "../web/src/features/issue-page/first-line";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  CONTROL_LABELS,
  DECISIONS_WORDS,
  NODE_STATE_WORDS,
  SPEC_WORDS,
} from "../web/src/features/issue-page/page-words";
import {
  nextTreeCursor,
  type TreeIdReads,
  treeIdOf,
} from "../web/src/features/issue-page/tree-read/tree-nodes";
import { parseNodeReference } from "../web/src/features/refs/routes";
import { listComments } from "./api";
import { seedDecisionsFilter, seedReviewList } from "./issue-layout-seed";
import {
  blockNodeIn,
  decisionNodesOf,
  decisionSum,
  decisionSumOf,
  readTree,
} from "./issue-layout-seed-support";
import { requirementBlockId, requirementText, specBlockRef } from "./issue-layout-seed-text";
import { gotoIssue, issueRead, phase, presetCount } from "./issue-page";
import {
  drawnRows,
  expectIssuePageDrawn,
  focusRow,
  nodeRow,
  rowOf,
  rowWords,
  unfoldEveryBranch,
} from "./issue-page-g";
import { rowReference } from "./issue-page-i";
import { withIssuePage } from "./issue-page-option";
import { waitingCount } from "./issue-page-p";
import {
  type DrawnPart,
  decisionsPartRows,
  decisionsPartsDrawn,
  decisionsRows,
  reviewFlag,
} from "./issue-page-q";
import { drawnRowReadings, fixtureSeed, keyedRows, seedKeys, steadily } from "./issue-page-t";
import { restatesAnAnswer } from "./issue-page-t-rows";
import { conversationFirstPage } from "./issue-page-t-scale";
import {
  blocksReads,
  convertedGate,
  decisionNodesGate,
  decisionsReadOf,
  heldAnswer,
  kindsSent,
  owedUnless,
  type TreeView,
  treeAnswerOf,
  treeReadGate,
  treeReads,
} from "./issue-page-t-tree";
import {
  actsBesideWords,
  askOfNode,
  askRowId,
  blockRowId,
  conversationRead,
  decisionRowId,
  decisionsPages,
  decisionsPartHeading,
  decisionsViewServedGate,
  type FilterReading,
  heldDecisionsRead,
  holdTreeReads,
  idReadsOf,
  nextCount,
  nextEntries,
  type PagedPart,
  readFilter,
  releaseFilter,
  reviewWordOf,
  rewordSecondDecision,
  rewordThirdDecision,
  SECOND_DECISION,
  type SentWrite,
  type SpecColumnRow,
  seedPagedDecisions,
  specCells,
  specColumn,
  statementsBeside,
  THIRD_DECISION,
  viewNodesOf,
  watchWrites,
  writeFootDecisions,
  writeThirdDecision,
} from "./issue-page-u";
import { type Browser, expect, type Locator, type Page, test } from "./test";
import { type ApiRequest, type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";
import { asUser } from "./users";

/**
 * The decision rows of the new issue page:
 * every decision the page draws is a node of the issue's tree read (`GET /api/v1/issues/{key}/tree`)
 * and its views (`?view=`), with the review state the model answers on it, and Keep and Change
 * write against that node, read on the real stack. Each row turns the new page on for alice with
 * `phase` (1440 by 900), seeds the issues of `e2e/issue-layout-seed.ts`, or row 6's of its own
 * (`seedPagedDecisions`, `e2e/issue-page-u.ts`), opens them with `gotoIssue` (`e2e/issue-page.ts`
 * holds both), and reads them through `nodeRow`, `rowOf`, `drawnRows` and `unfoldEveryBranch`
 * (`e2e/issue-page-g.ts`), `pickPreset` (`e2e/issue-page-i.ts`), `waitingCount`
 * (`e2e/issue-page-p.ts`), the readers of the Decisions view and of its control's flag
 * (`e2e/issue-page-q.ts`),
 * `watchApiRequests` (`e2e/traffic.ts`), the helpers of `e2e/issue-page-t.ts`, part T's
 * `conversationFirstPage` (`e2e/issue-page-t-scale.ts`), the reads of the tree read the rows share
 * with the seeds (`e2e/issue-layout-seed-support.ts`) and the rows' own (`e2e/issue-page-u.ts`).
 * Row 5 opens a second reader beside alice: bob signs in to a context of his own with `asUser`
 * (`e2e/users.ts`), and `withIssuePage` (`e2e/issue-page-option.ts`) turns the page on for him.
 *
 * The rows are six of the Verification document's browser checks, by their titles:
 *
 * 1. `c-decisions`: "every decision the page draws is a node with the review state the model
 *    holds", on `seedDecisionsFilter` and `seedReviewList`. Its expected state words and
 *    header count are read from the server's raw reads and from what the seed wrote
 *    (`restating`), never through the page's own classifiers (`restatesAnswer`,
 *    `decisionNodeChanges`).
 * 2. `c-keep-change`: "Keep and Change act on the decision node", on `seedReviewList`.
 * 3. `c-one-component`: "the Decisions view draws the one card and the one ask body", on
 *    `seedReviewList` and `seedDecisionsFilter`. Dashboard's part (dispatch://INFRA-5702), which
 *    makes root R42's decision card and ask body, is not on this branch's base, so the row reads
 *    the check's (3) and (4) alone and says so in an annotation (`ROW_THREE_CASE`).
 * 4. `c-pages`: "each part of the Decisions view pages 20 entries at a time", on
 *    `seedReviewList` with 25 decisions more at the foot (`writeFootDecisions`).
 * 5. Decision 502's row, which the Verification document names from its v43: "a decision another
 *    person made stands under Decided by others", on `seedDecisionsFilter`, read by bob and then
 *    by alice.
 * 6. The row INFRA-1196's architect asked over a paged tree read: "every decision of a paged
 *    issue is in its part, the header's count and the Spec column", on `seedPagedDecisions`,
 *    whose Conversation read pages before its answered question, the decision under it and a
 *    decision that waits; the decision that settles the spec's open question stands on that
 *    page. Its control is U's code at `776d9d85`, which draws the parts from Conversation's
 *    root. Its step (2) asserts that, before Decisions is pressed, the header's count, the
 *    Decisions filter's count and its to-review flag each read a number and "+", the flag
 *    against a control of its own (`IssuePresets.tsx` with only the flag's "+" dropped). Every
 *    value it expects is read from the server's raw reads or is what the seed wrote, never
 *    from the page's own classifiers (`restatesAnswer`, `decisionNodeChanges`). Its step (4)
 *    asserts that the decision that waits, past
 *    Conversation's first page, draws Keep and Change in the review list, that alice's Change
 *    on it anchors to the decision node's own document and block at `occurrence` 0, and that
 *    once the page has read again it reads "Changed by" alice among her own decisions, out of
 *    the review list, with the header's count one less. That step's two controls are
 *    `DecisionActions.tsx` as at `cc5b97fe`, which finds the decision in the outline read
 *    alone, and the Changes read from Conversation's read alone (`changesOf` and
 *    `waitingOnReader` as at `cc5b97fe`).
 *
 * Each row checks its gates first, each with `owedUnless`, and skips where one is not on the
 * base, with an `owed` annotation naming the gate and the decisions that make it owed; it never
 * passes then. The gates are the server's tree read (`treeReadGate`), its Decisions view
 * (`decisionsViewServedGate`: the model's tree route serves `view=decisions` since its
 * `2afc1550`, which this branch holds through part T, as `issue_tree_read.go`'s `issueTreeViews`
 * lists `QueryDecisions`, so the gate holds a row owed only on a server that lacks the view), an
 * issue whose documents are nodes of its tree (`convertedGate`), and the seeds written as
 * decision nodes of the tree read (`decisionNodesGate`, over the decisions read
 * `decisionsReadOf` keeps). The first three are read before anything else once the page is open,
 * the fourth once the page has received its decisions read, which the page sends once Decisions
 * is pressed, or as it opens on the Decisions view, as rows 3, 4 and 5 open it. Every row is
 * tagged `@owed`, and each skips on the first of its gates the server on the base does not meet.
 */

/** How long row 1 may take: two seeded issues, each read through all four filters. */
const ROW_ONE_MS = 480_000;

/** How long row 2 may take. */
const ROW_TWO_MS = 240_000;

/** How long row 3 may take: two seeded issues, each with a Change and a Keep sent, and every row
 *  of its Decisions view focused. */
const ROW_THREE_MS = 360_000;

/** How long row 4 may take: one seeded issue with 25 decisions more, its Decisions view loaded
 *  twice, and its foot's pages drawn once by the pointer and once by the keyboard. */
const ROW_FOUR_MS = 240_000;

/** How long row 5 may take: one seeded issue, its Decisions view loaded by two readers. */
const ROW_FIVE_MS = 240_000;

/** How long row 6 may take: one seeded issue whose spec holds twenty open questions of some
 *  10,000 characters each, Conversation's read of it read page by page, its Decisions and Spec
 *  views each pressed and read until they hold still, and a Change waited on until the page's
 *  reads redraw the Decisions view and the header's count. */
const ROW_SIX_MS = 300_000;

/** The four parts of the Decisions view row 1 reads, in the order it draws them
 *  (`data-decisions-part`): alice made every decision of row 1's seeds, so the view draws no
 *  part for what others decided (`others`, row 5). */
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
const KEPT_BY_ALICE = new RegExp(`^${NODE_STATE_WORDS.decision.kept}\\b.*\\balice\\b`, "i");

/** A decision row's state word once alice asked to change it: "Changed by" and her name, where
 *  the state token stays `change_requested` (R2, R3). */
const CHANGED_BY_ALICE = new RegExp(`^${DECISIONS_WORDS.changed_by}\\b.*\\balice\\b`, "i");

/** The words each row's Change sends. */
const CHANGE_WORDS = "Tie it to the second statement instead.";

/** How long the count of tree reads row 2 (3) holds must stay unchanged before the row takes it
 *  as every read the reword's event sent: the scheduler starts the reads of one event within
 *  `MIN_REFETCH_INTERVAL_MS` of each other, and none of a query whose read is out
 *  (`web/src/api/query-refresh.ts`), and 500 ms more lets a request the page sent reach
 *  Playwright's route. */
const EVENT_READS_STILL_MS = MIN_REFETCH_INTERVAL_MS + 500;

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

/** The words the Decisions control's to-review flag draws, its number and a "+" where the read
 *  it is counted from names a next page, and null where the page draws no flag. Each caller
 *  polls it, never once. */
async function reviewFlagWords(page: Page): Promise<string | null> {
  // Every caller polls this read, which asks again until the flag settles: one answer, one try.
  const [words] = await reviewFlag(page).allTextContents();
  return words === undefined ? null : words.replace(/\s+/g, " ").trim();
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

/** How row 1 names the rows of one seed: a decision and a statement by the id the row of their
 *  block carries (`blockRowId`, `<slug>#b-<block id>`), an ask by its row (`askRowId`), and the
 *  open questions of the asks read. A block the tree read holds no node of fails the row. */
interface RowNames {
  readonly ask: (askId: string) => string;
  readonly decision: (block: string) => string;
  readonly openAsks: readonly string[];
  readonly requirement: (block: string) => string;
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
  /** The decision alice kept, which stands among her own decisions and reads "Kept by" alice;
   *  null on a seed that keeps none. */
  readonly kept: string | null;
  /** The rows each part holds, by the ids they carry. */
  readonly parts: Readonly<Record<DecisionsPart, readonly string[]>>;
  /** The decision nodes the seed wrote that only restate a person's answer, by the ids their
   *  rows would carry (INFRA-1196's decisions 513 and 521): the decision it wrote with `from_ask`
   *  that shapes nothing the answered question did not shape. The page draws no row for each. */
  readonly restating: readonly string[];
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
 *  decision node that was not replaced, as the model's decision it carries (`decision`) answers,
 *  since a replaced one stands beside no statement in the Spec column, or from an ask the asks
 *  read says is answered, which is a person's decision. Each end of a tie names its node by the
 *  node's store id (`IssueTreeEnd.id`). */
function tiesToStatements(
  answer: IssueTreeRead,
  reads: TreeIdReads
): { readonly from: IssueTreeNode; readonly to: IssueTreeNode }[] {
  const nodes = new Map((answer.nodes ?? []).map((node) => [node.id, node]));
  return (answer.tuples ?? []).flatMap((tuple) => {
    const from = nodes.get(tuple.from.id);
    const to = nodes.get(tuple.to.id);
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
      (from.supertag === "decision" && from.decision?.replaced !== true) ||
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

/** Whether the read `answer` hangs a comment under decision node `node`, by the comment node's
 *  `parent`: the Change the row sent on it. The rows' seeds write no other comment under a
 *  decision (`seedWholeIssue`'s one anchored comment is on a statement's words), so a comment the
 *  read hangs under a decision is a Change, and the expectation reads it off the raw read and
 *  never through the page's own rule (`decisionNodeChanges`). */
function hasCommentUnder(answer: IssueTreeRead, node: IssueTreeNode): boolean {
  return (answer.nodes ?? []).some(
    (one) => one.supertag === "comment" && one.parent?.id === node.id
  );
}

/** Whether a `decided_by` tuple of `answer` names a person as who decided `node`: that person's
 *  decision, as the page reads a `person` end (`tree-drafts.ts`' `deciderOf`), which waits for no
 *  review (INFRA-1196's decision 516). */
function decidedByAPerson(answer: IssueTreeRead, node: IssueTreeNode): boolean {
  return (answer.tuples ?? []).some(
    (tuple) =>
      tuple.kind === "decided_by" && tuple.from.id === node.id && tuple.to.kind === "person"
  );
}

/** The decisions of `answer` that wait for alice's review, as the header counts them and the
 *  review list draws them (R35), read from the server's own members: each decision node of this
 *  issue that the read answers as its own rather than as context, as waiting and not replaced, in
 *  the model's decision it carries (`decision`), that no person decided (`decidedByAPerson`),
 *  that the read hangs no comment under (`hasCommentUnder`: the Change a row sends), and that a
 *  `shaped` tie binds to a statement of this issue: one of `ties` (`tiesToStatements`), read from
 *  `answer` itself where it holds the statements, as the tree read does, and else from the Spec
 *  view's read, as for the Decisions view's read, which holds none. */
function decisionsWaitingOnAlice(
  answer: IssueTreeRead,
  ties: readonly { readonly from: IssueTreeNode }[]
): IssueTreeNode[] {
  const tied = new Set(ties.map(({ from }) => from.id));
  return (answer.nodes ?? []).filter(
    (node) =>
      node.supertag === "decision" &&
      node.issue === undefined &&
      node.role !== "context" &&
      node.decision?.waiting_review === true &&
      node.decision.replaced !== true &&
      !hasCommentUnder(answer, node) &&
      !decidedByAPerson(answer, node) &&
      tied.has(node.id)
  );
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
  owedUnless(await decisionsViewServedGate(page, key));
  owedUnless(await convertedGate(page, key));
  const tree = treeAnswerOf(page);
  const decisions = decisionsReadOf(page, key);
  const traffic = watchApiRequests(page);
  // Conversation at its own address: the bare address is Dashboard's once Dashboard's part is on
  // the base, and its own `view=decisions` read is not this part's (root R39).
  await gotoIssue(page, `/issues/${key}/conversation`);
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

  // (1) The decisions are the tree read's: the load of Conversation sends no read of the
  // decisions route, no document's blocks and no Decisions view, and Decisions pressed sends its
  // view read once and no read of the decisions route or of any document's blocks. A second load,
  // of the Decisions view's own address, is read after (4).
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
  // its node in the Decisions view's read, and the decision alice kept "Kept by" alice among her
  // own. The foot is drawn unfolded as the view opens (R2), so nothing is pressed: its decisions
  // are read as the view drew them.
  const reads = await idReadsOf(key);
  const idOf = treeIdOf(answer, reads);
  const wanted = wantedOf({
    ask: (askId) => askRowId(reads, askId),
    decision: (block) => blockRowId(key, answer, "decision", block),
    openAsks: (reads.asks ?? []).flatMap((ask) =>
      ask.state === "open" && ask.kind !== "approval" ? [askRowId(reads, ask.id)] : []
    ),
    requirement: (block) => blockRowId(key, answer, "requirement", block),
  });
  // Each decision node of the Decisions view's read, but one that restates a person's answer
  // (INFRA-1196's decision 513), which the view draws no row for: the seed names it
  // (`RowOneWanted.restating`), the decision it wrote with `from_ask` that shapes nothing the
  // answered question did not shape. Each state word is the server's own members of the node
  // (`reviewWordOf`), never the page's rule for what restates an answer.
  const view = await readTree(key, "decisions");
  const restating = new Set(wanted.restating);
  const states = Object.fromEntries(
    (view.nodes ?? []).flatMap((node) =>
      node.supertag === "decision" &&
      node.issue === undefined &&
      node.role !== "context" &&
      !restating.has(decisionRowId(node))
        ? [[decisionRowId(node), reviewWordOf(node)]]
        : []
    )
  );
  const partsAndStates = async () => {
    const drawn = await decisionsPartsDrawn(page);
    const rows = await drawnRowReadings(page, { bodies: false });
    const rowsOf = (part: string) => drawn.find((one) => one.part === part)?.rows ?? [];
    return {
      declined:
        wanted.declined === null
          ? null
          : (rows.find((row) => row.id === wanted.declined)?.state?.word ?? null),
      kept:
        wanted.kept === null
          ? null
          : {
              own: rowsOf("yours").includes(wanted.kept),
              word: rows.find((row) => row.id === wanted.kept)?.state?.word ?? null,
            },
      parts: Object.fromEntries(
        DECISIONS_PARTS.map((part) => [part, partOrder(part, rowsOf(part))])
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
      kept:
        wanted.kept === null
          ? null
          : { own: true, word: `${NODE_STATE_WORDS.decision.kept} alice` },
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
  const asks = asksWaitingOnAlice(reads.issue);
  await expect
    .poll(() => waitingOnYou(page), {
      message: "the header's count of what waits counts the decisions that wait for alice's review",
    })
    .toBe(waitingWords(asks + decisionsWaitingOnAlice(answer, ties).length));

  // (1), a second load: the Decisions view opened at its own address sends no request to the
  // decisions route either.
  const second = traffic.requests.length;
  await gotoIssue(page, presetPath(key, "decisions"));
  await expectIssuePageDrawn(page);
  await steadily(page, traffic, `${key}'s Decisions view as it loads`, async () => ({
    rows: await drawnRowReadings(page, { bodies: false }),
    total: traffic.total(),
  }));
  expect(
    decisionsSent(traffic.requests.slice(second), key).decisions,
    "the page sends no decisions read, on a second load, of the Decisions view"
  ).toEqual([]);

  // (5) Each filter's row ids and count are the rows fixture's for the seed, the four parts and
  // the Spec column are the values row 1 expects of the seed, and the header's count is alice's
  // open questions and the review part's entries. The fixture recorded the Decisions view with
  // its foot folded, so that filter's rows are compared with the foot's left out, which (2)
  // reads; its count counts the foot's either way. The fixture is part T's record of the page
  // before INFRA-1196's decision 513, which drew a section that restates a person's answer - a
  // decision the fixture draws under an answered question (T's `restatesAnAnswer`) - as a row,
  // where this page draws none: each such row of the fixture is left out of the row ids
  // compared, and out of the count of each filter that counts it, Decisions alone, which counts
  // the asks and the decisions (`presets.ts`' `COUNTED_KINDS`).
  const slugs = (reads.issue.artifacts ?? []).map((artifact) => artifact.slug);
  const foot = new Set(parts.find((part) => part.part === "agents")?.rows ?? []);
  const recorded = fixtureSeed(name);
  const restated = new Set(
    recorded.rows.flatMap((row) => (restatesAnAnswer(row, recorded.rows) ? [row.key] : []))
  );
  const recordedNow = Object.fromEntries(
    Object.entries(recorded.filters).map(([filter, held]) => [
      filter,
      held === undefined
        ? held
        : {
            count: held.count - (filter === "decisions" ? restated.size : 0),
            rows: held.rows.filter((key) => !restated.has(key)),
          },
    ])
  );
  expect(
    recordedFilters(
      {
        decisions: {
          ...decisionsFilter,
          rows: decisionsFilter.rows.filter((row) => row.id === null || !foot.has(row.id)),
        },
        plan: planFilter,
        spec: specFilter,
        verification: verificationFilter,
      },
      seedKeys(seeded, slugs)
    ),
    "each filter's row ids and count equal the rows fixture for the seed, the Decisions view's foot and each section that restates a person's answer left out"
  ).toEqual(recordedNow);
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

/** Row 1's two seeds. On `seedDecisionsFilter` the review list and alice's own part are the ones
 *  the Decisions view's first row asserts (review `[D(5), D(0)]`, yours `[D(4), declined,
 *  optioned, answered]`), the foot is `[D(2), D(3), D(6)]`, and the Spec column is the one the
 *  Spec column's row asserts on the same decisions, less the second decision's entry. That
 *  decision, `D(1)`, settles the answered ask (`from_ask`), which the tree places it under: it is
 *  the old decisions document's section that restates the person's answer, and the person's
 *  decision is the answered ask itself (INFRA-1196's decision 513), so the page draws no row for
 *  it, and it stands in no part, in no count and in no entry of the Spec column; the row names
 *  it from what the seed wrote (`restating`), not from the page's rule. On `seedReviewList`,
 *  which writes no `from_ask`, `restating` is empty and the parts are the ones the Decisions
 *  view's review-list row asserts. */
const ROW_ONE_SEEDS = [
  rowOneSeed("seedDecisionsFilter", seedDecisionsFilter, (seeded, names) => {
    const block = (index: number) => seeded.decisions[index] ?? "";
    const D = (index: number) => names.decision(block(index));
    const r = (index: number) => names.requirement(seeded.requirements[index - 1] ?? "");
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
      kept: names.decision(seeded.keptBlock),
      parts: {
        agents: [D(2), D(3), D(6)],
        open_questions: names.openAsks,
        review: [D(5), D(0)],
        yours: [D(4), declined, names.ask(seeded.optionedAskId), answered],
      },
      restating: [D(1)],
    };
  }),
  rowOneSeed("seedReviewList", seedReviewList, (seeded, names) => ({
    declined: null,
    departure: null,
    kept: null,
    parts: {
      agents: [names.decision(seeded.tiedToNote), names.decision(seeded.untied)],
      open_questions: names.openAsks,
      review: [names.decision(seeded.tiedNewer), names.decision(seeded.tiedOlder)],
      yours: [],
    },
    restating: [],
  })),
];

// c-decisions, owed: the row skips on the gate of the server's tree read (`treeReadGate`) and of
// its Decisions view (`decisionsViewServedGate`), which the model's tree route serves since its
// `2afc1550` (`issue_tree_read.go`'s `issueTreeViews`), so that gate holds the row owed only on a
// server that lacks the view. It waits too on an issue whose documents are nodes of its tree
// (`convertedGate`), and on the seeds written as decision nodes of the tree read
// (`decisionNodesGate`, read once Decisions is pressed). The check holds it owed until the server
// on the base meets all four gates.
// SLOW(>=2s): two seeded issues, each load of Conversation waited out, the four filters each
// pressed, read until it holds still, and released, and the Decisions view loaded again.
test("every decision the page draws is a node with the review state the model holds @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_ONE_MS);
  for (const run of ROW_ONE_SEEDS) await run(browser);
});

// ---------------------------------------------------------------------------------------------
// Row 2: Keep and Change act on the decision node.
// ---------------------------------------------------------------------------------------------

/** The path of a Keep of the decision node `node`: its own document and its block
 *  (`block_id`). */
function keepPath(node: IssueTreeNode): string {
  const document = encodeURIComponent(node.document?.artifact_id ?? "");
  const block = encodeURIComponent(node.block_id ?? node.id);
  return `/api/v1/artifacts/${document}/blocks/${block}/keep`;
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

/** What one Change's checks say, so each row that sends one keeps its own words (`openChange`,
 *  `sendChange`): `change` names the Change - "the Change", or "the Change on" its decision -
 *  `said`, where given, leads each message but the wait for its comment, and `anchored` follows
 *  the Change's name in the check of its anchor. */
interface ChangeWords {
  readonly anchored: string;
  readonly change: string;
  readonly said?: string;
}

/** The Change's name as `words` leads a message with it (`ChangeWords.said`). */
function saidChange({ change, said }: ChangeWords): string {
  return said === undefined ? change : `${said}: ${change}`;
}

/** What a row's Change is sent and read through: issue `key`, the page's API requests
 *  (`watchApiRequests`) and the writes the page sent (`watchWrites`). */
interface ChangeSend {
  readonly key: string;
  readonly traffic: ApiRequests;
  readonly writes: readonly SentWrite[];
}

/** Opens decision row `row`'s Change and checks it draws its field. */
async function openChange(row: Locator, words: ChangeWords): Promise<void> {
  await row.locator(CHANGE).click();
  await expect(
    row.getByLabel(CONTROL_LABELS.your_change),
    `${saidChange(words)} draws its "${CONTROL_LABELS.your_change}" field`
  ).toBeVisible();
}

/**
 * Types `CHANGE_WORDS` into decision row `row`'s open Change (`openChange`) and sends it; checks
 * that once the page is quiet it sent one comment of issue `key`, anchored to `node`'s own
 * document and block at the first match of its quote there (`occurrence` 0), and answers that
 * comment's anchor.
 */
async function sendChange(
  row: Locator,
  node: IssueTreeNode,
  words: ChangeWords,
  { key, traffic, writes }: ChangeSend
): Promise<unknown> {
  await row.getByLabel(CONTROL_LABELS.your_change).fill(CHANGE_WORDS);
  const send = row.getByRole("button", { exact: true, name: CONTROL_LABELS.send_change });
  await expect(
    send,
    `${saidChange(words)} draws its "${CONTROL_LABELS.send_change}" control`
  ).toBeEnabled();
  const from = writes.length;
  await send.click();
  await expect
    .poll(() => commentsOf(writes.slice(from), key).length, {
      message: `${words.change} sends no comment`,
    })
    .toBeGreaterThan(0);
  await expectLoadSettled(traffic);
  const sent = commentsOf(writes.slice(from), key);
  const anchor = fieldOf(sent[0]?.body, "anchor");
  expect(
    {
      artifact: fieldOf(anchor, "artifact"),
      block_id: fieldOf(anchor, "block_id"),
      comments: sent.length,
      occurrence: fieldOf(anchor, "occurrence"),
    },
    `${saidChange(words)} ${words.anchored}`
  ).toEqual({
    artifact: node.document?.artifact_id,
    block_id: node.block_id ?? node.id,
    comments: 1,
    occurrence: 0,
  });
  return anchor;
}

/** Where decision row `id` stands in the Decisions view, from one reading of its parts
 *  (`decisionsPartsDrawn`, as row 1's `partsAndStates` reads them): whether it is in the review
 *  list and among alice's own decisions, and the state word its row draws. */
async function changedStanding(page: Page, id: string) {
  const drawn = await decisionsPartsDrawn(page);
  const rowsOf = (part: string) => drawn.find((one) => one.part === part)?.rows ?? [];
  return {
    inReview: rowsOf("review").includes(id),
    own: rowsOf("yours").includes(id),
    state: await stateWordOf(page, id),
  };
}

/** The read id of the node the tree read `answer` hangs comment `commentId` under: the `parent`
 *  of its comment node, the node whose block id is the comment's id (`blockIdOfNode`), as its row
 *  would carry it. Undefined while no answer is held, and where it holds no such comment. */
function commentParentOf(answer: IssueTreeRead | undefined, commentId: string): string | undefined {
  return (answer?.nodes ?? []).find(
    (node) => node.supertag === "comment" && (node.block_id ?? node.id) === commentId
  )?.parent?.id;
}

// c-keep-change, owed: the row skips on the gate of the server's tree read (`treeReadGate`) and of
// its Decisions view (`decisionsViewServedGate`), which the model's tree route serves since its
// `2afc1550` (`issue_tree_read.go`'s `issueTreeViews`), so that gate holds the row owed only on a
// server that lacks the view. It waits too on an issue whose documents are nodes of its tree
// (`convertedGate`), and on the seeds written as decision nodes of the tree read
// (`decisionNodesGate`, read once Decisions is pressed): the check holds it owed until the server
// on the base meets those four gates. Its Change clause asserts `anchor.block_id` and
// `anchor.occurrence` 0 on the captured comment request, which the page sends
// (`DecisionActions.tsx`' `ChangeComposer`); the server finds the quote inside that block alone
// and takes its first match there, and refuses with 400 a block that does not hold it
// (`api/anchors.go`' `resolveAnchor`).
// SLOW(>=2s): a seeded issue with a third decision written, a Keep and a Change each waited on
// until the server's reads redraw the list, a reload, a refused Keep under held tree reads,
// after the reword's event reads have held still for `EVENT_READS_STILL_MS`, and a reword
// waited on until the list draws it.
test("Keep and Change act on the decision node @owed", async ({ browser }) => {
  test.setTimeout(ROW_TWO_MS);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const { key } = seeded;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await decisionsViewServedGate(page, key));
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
    // to the node's own document and block, at the first match of its quote there
    // (`occurrence` 0), read from the request before anything drawn. Its quote is the decision's
    // first paragraph alone, "Shapes" and the address of `r1`, its heading passed over.
    const olderRow = nodeRow(page, ids.older).first();
    const change = olderRow.locator(CHANGE);
    await expect(change, "the second decision, tiedOlder, draws its Change control").toBeVisible();
    const changeWords = { anchored: "anchors to its decision's block", change: "the Change" };
    await openChange(olderRow, changeWords);
    // Read before Send: the form's quote is #3125's plain-text projection of the decision's first
    // paragraph (`DecisionActions.tsx`' `usePlainHeadline`, the reading list's
    // `decision-change-quote`), its heading passed over, in which a reference reads as its
    // target's current name in plain text, with no link: "Shapes", then `r1` by the name the page
    // published for it, which is the name a link to `r1` reads (`refs/reference-names.tsx`), then
    // a full stop. The projection cuts none of that name (`refs/plain-text.tsx`' `max`, which the
    // form leaves whole). The row's own words draw `r1` as a chip by the same words, cut to a
    // chip's room (`RowText.tsx`, `row-references.ts`' `rowReferenceWords`, by `issue-tree.ts`'
    // `chipName`). So the quote's name, cut as a chip is cut, is the chip the row draws for `r1`;
    // it is `r1`'s own words from their start, whole or cut where the projection's ellipsis
    // stands; and it is longer than that chip, since `r1` holds more than a chip's room. No text
    // of the page reads a raw `dispatch://` address (decision 448, root R22).
    const statement = specBlockRef(key, requirementBlockId(1));
    const statementRoute = parseNodeReference(statement);
    const statementWords = requirementText(1).replace(/\s+/g, " ").trim();
    const statementChip = chipName(statementRoute, statementWords);
    const quote = olderRow.locator('[data-testid="decision-quote"]');
    const chip = rowReference(olderRow, statement);
    await expect
      .poll(
        async () => {
          const quoted = (await quote.innerText()).replace(/\s+/g, " ").trim();
          const name = /^Shapes (.+)\.$/u.exec(quoted)?.[1] ?? "";
          const opening = name.endsWith("…") ? name.slice(0, -1) : name;
          return {
            chip: await chip.allInnerTexts(),
            heading: quoted.includes(SECOND_DECISION.heading),
            name: {
              chipped: chipName(statementRoute, name),
              longer: name.length > statementChip.length,
              words:
                opening === statementWords ||
                (opening !== name && statementWords.startsWith(opening)),
            },
            quote: quoted,
            raw: (await page.locator("body").innerText()).includes("dispatch://"),
          };
        },
        { message: "the Change form draws no raw address" }
      )
      .toEqual({
        chip: [statementChip],
        heading: false,
        name: { chipped: statementChip, longer: true, words: true },
        quote: expect.stringMatching(/^Shapes .+\.$/u),
        raw: false,
      });
    const anchor = await sendChange(olderRow, older, changeWords, { key, traffic, writes });
    const quoted = fieldOf(anchor, "quote");
    expect(
      {
        directive: typeof quoted === "string" && /:::|shaped=/.test(quoted),
        heading: typeof quoted === "string" && quoted.includes(SECOND_DECISION.heading),
        quote: quoted,
      },
      "the Change quotes its decision's first paragraph alone"
    ).toEqual({ directive: false, heading: false, quote: `Shapes ${statement}.` });
    // The changed decision reads "Changed by" and alice's name, leaves the review list and stands
    // among her own decisions, in the Decisions view as the server's reads redraw it, and again
    // after a reload (below).
    const changedByAlice = {
      inReview: false,
      own: true,
      state: expect.stringMatching(CHANGED_BY_ALICE),
    };
    await expect
      .poll(() => changedStanding(page, ids.older), {
        message:
          "the changed decision reads Changed by alice, leaves the review list and stands among her own decisions",
      })
      .toEqual(changedByAlice);
    const comment = (await listComments(key)).find((one) => one.body === CHANGE_WORDS);
    expect(comment, "the server holds no comment for the Change").toBeDefined();
    const commentId = comment?.id ?? "";
    // In Conversation the changed decision's row reads "Changed by" alice too, and the tree read
    // answers the Change under its decision, before a reload and after one. A comment anchored on
    // a node's text is drawn in the margin beside that node and no row of the outline (part T's
    // `onNodeText`, root R41), so the Change's place is read from the tree read the page
    // received: the comment node's `parent` is the decision node.
    await releaseFilter(page, "decisions");
    const stateInConversation = async () => {
      await unfoldEveryBranch(page);
      return stateWordOf(page, ids.older);
    };
    const changedByAliceWord = expect.stringMatching(CHANGED_BY_ALICE);
    await expect
      .poll(stateInConversation, {
        message: "the changed decision reads Changed by alice in Conversation",
      })
      .toEqual(changedByAliceWord);
    await expect
      .poll(() => commentParentOf(tree.answer(), commentId), {
        message: "the tree read answers the Change under its decision",
      })
      .toBe(older.id);
    // The reads of the tree the page sends from the reload on, so the answer read below is the
    // reloaded page's own.
    const reloadedTree = treeAnswerOf(page);
    await page.reload();
    await issueRead(page);
    await expectIssuePageDrawn(page);
    await expect
      .poll(stateInConversation, {
        message: "the changed decision reads Changed by alice in Conversation, after a reload",
      })
      .toEqual(changedByAliceWord);
    await expect
      .poll(() => commentParentOf(reloadedTree.answer(), commentId), {
        message: "the tree read answers the Change under its decision, after a reload",
      })
      .toBe(older.id);
    await readFilter(page, traffic, "decisions");
    await expect
      .poll(() => changedStanding(page, ids.older), {
        message:
          "the changed decision reads Changed by alice, out of the review list and among her own decisions, after a reload",
      })
      .toEqual(changedByAlice);

    // (3) With the page's next tree reads held, the third decision's words change through the
    // API and its Keep sends the sum of the words the page drew: Dispatch refuses it, the row
    // still waits with its alert, the refused Keep sends a tree read of its own, and once the
    // held reads go on the row draws the new words. The Decisions view is on since (2) read it
    // after the reload.
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
    // The reword's own event reads the tree too, and those reads are held with the rest, so the
    // Keep's read is counted past them. The count is taken once it is above zero and has not
    // moved for `EVENT_READS_STILL_MS`: the scheduler starts no read of a query whose read is
    // out, so from then on the count rises only for a read the page starts outside it, as the
    // refused Keep's settle does (`readDecisionsAgain`, `DecisionActions.tsx`).
    let eventReads = 0;
    let movedAt = Date.now();
    await expect
      .poll(
        () => {
          const count = held.count();
          if (count !== eventReads) {
            eventReads = count;
            movedAt = Date.now();
          }
          return eventReads > 0 && Date.now() - movedAt >= EVENT_READS_STILL_MS;
        },
        {
          intervals: [100],
          message: "the reword's event reads the tree, and the count of held reads holds still",
        }
      )
      .toBe(true);
    const refusedFrom = writes.length;
    const answered = page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        new URL(response.url()).pathname === keepPath(third)
    );
    const beforeKeep = held.count();
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
      .poll(() => held.count(), {
        message: "the refused Keep reads the tree again, past the reads the reword's event left",
      })
      .toBeGreaterThan(beforeKeep);
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

    // (4) The second decision's words then change through the API, after its Change: the
    // Change asks for nothing any more, so the decision waits for alice's review again and is
    // back in the review list (root R35).
    const changedSum = await decisionSum(key, seeded.tiedOlder);
    await rewordSecondDecision(spec);
    expect(
      await decisionSum(key, seeded.tiedOlder),
      "the edit leaves the second decision's sum as it was"
    ).not.toBe(changedSum);
    await expect
      .poll(
        async () => ({
          inReview: (await decisionsPartRows(page, "review")).includes(ids.older),
          state: await stateWordOf(page, ids.older),
        }),
        {
          message:
            'the second decision, its words changed after its Change, reads "Waiting for review" again and is back in the review list',
        }
      )
      .toEqual({ inReview: true, state: NODE_STATE_WORDS.decision.waiting_review });
  });
});

// ---------------------------------------------------------------------------------------------
// Row 3: the Decisions view draws the one card and the one ask body.
// ---------------------------------------------------------------------------------------------

/** What row 3 says it read, in an annotation (c-one-component): the case of a base that does not
 *  hold Dashboard's part, which makes root R42's decision card and ask body. */
const ROW_THREE_CASE =
  "dispatch://INFRA-5702 (Dashboard's part: root R42's decision card and ask body) is not on " +
  "this branch's base, so the row reads (3) and (4), each row's acts beside the row's words, " +
  "and passes on them; (1) `each agent decision is the one card` and (2) `each ask is the one " +
  "ask body` are owed to INFRA-5702's check of root R42 under the pair rule (decision 424)";

/** Row 3 on one seed, read from the page `phase` opened, at its Decisions view. */
async function readRowThree(page: Page, seeded: SeededIssue): Promise<void> {
  const { key } = seeded;
  owedUnless(await treeReadGate(page, key));
  owedUnless(await decisionsViewServedGate(page, key));
  owedUnless(await convertedGate(page, key));
  const decisions = decisionsReadOf(page, key);
  const traffic = watchApiRequests(page);
  const writes = watchWrites(page);
  await gotoIssue(page, presetPath(key, "decisions"));
  await expectIssuePageDrawn(page);
  const opened = await steadily(page, traffic, `${key}'s Decisions view as it opens`, () =>
    decisionsRows(page)
  );
  const answer = await readTree(key, null);
  owedUnless(decisionNodesGate(answer, await heldDecisionsRead(key, decisions)));
  expect(answer.converted, "the tree read answers converted: true").toBe(true);

  // (3) Keep and Change stand on the decisions that wait for alice's review alone, in their own
  // words, and write R4's Keep and R3's Change as row 2 reads them: the Change on the first of
  // them, its words typed under "Your change" and sent with "Send change", anchored to its
  // decision's block at `occurrence` 0, after which that decision reads "Changed by" alice and
  // draws neither control; then the Keep on the second, by the node's own document, block and
  // sum.
  const said = "the row draws Changed by and the controls' words";
  const review = opened.flatMap((row) => (row.part === "review" ? [row.id] : []));
  expect(
    review.length,
    `${said}: the seed holds two decisions that wait for alice's review`
  ).toBeGreaterThanOrEqual(2);
  expect(
    opened.flatMap(({ actions, id }) => (actions.length === 0 ? [] : [{ actions, id }])),
    `${said}: Keep and Change stand on the decisions that wait alone`
  ).toEqual(review.map((id) => ({ actions: ["keep", "change"], id })));
  for (const id of review) {
    await expect(
      nodeRow(page, id).first().locator("button[data-row-action]"),
      `${said}: ${id} draws Keep and Change in their words`
    ).toHaveText([CONTROL_LABELS.keep_decision, CONTROL_LABELS.change_decision]);
  }
  const nodes = new Map<string, IssueTreeNode>();
  for (const node of await viewNodesOf(key, "decisions")) {
    if (node.supertag === "decision" && node.issue === undefined) {
      nodes.set(decisionRowId(node), node);
    }
  }
  const nodeOf = (id: string): IssueTreeNode => {
    const node = nodes.get(id);
    if (node === undefined) throw new Error(`${key}'s Decisions view read holds no node of ${id}`);
    return node;
  };
  const [changedId = "", keptId = ""] = review;
  const changedNode = nodeOf(changedId);
  const keptNode = nodeOf(keptId);

  const changedRow = nodeRow(page, changedId).first();
  const changeWords = {
    anchored: "sends R3's comment, anchored to its decision's block at occurrence 0",
    change: "the Change",
    said,
  };
  await openChange(changedRow, changeWords);
  await sendChange(changedRow, changedNode, changeWords, { key, traffic, writes });
  await expect
    .poll(
      async () => ({
        acting: (await decisionsRows(page)).flatMap((row) =>
          row.actions.length === 0 ? [] : [row.id]
        ),
        state: await stateWordOf(page, changedId),
      }),
      { message: said }
    )
    .toEqual({ acting: review.slice(1), state: expect.stringMatching(CHANGED_BY_ALICE) });

  const keptFrom = writes.length;
  await nodeRow(page, keptId).first().locator(KEEP).click();
  await expect
    .poll(() => keepsOf(writes.slice(keptFrom)).length, {
      message: "Keep on the second decision that waits sends no request",
    })
    .toBeGreaterThan(0);
  await expectLoadSettled(traffic);
  expect(
    keepsOf(writes.slice(keptFrom)),
    `${said}: Keep sends R4's request, with the node's own document, block id and text_sha256`
  ).toEqual([{ path: keepPath(keptNode), text_sha256: decisionSumOf(key, keptNode) }]);
  await expect
    .poll(() => stateWordOf(page, keptId), {
      message: `${said}: the kept decision reads "Kept by" alice`,
    })
    .toMatch(KEPT_BY_ALICE);

  // (4) Each row's acts (`RowActs`), which a row draws while it holds the focus, stand beside its
  // words and outside them: with no card and no ask body on this branch's base, the row's words
  // are what they stand beside.
  const rows = await steadily(page, traffic, `${key}'s Decisions view after the Keep`, () =>
    decisionsRows(page)
  );
  for (const { id } of rows) {
    const row = await focusRow(page, id);
    await expect
      .poll(() => actsBesideWords(row), {
        message: `the row's acts stand beside the card: ${id}'s, beside its words`,
      })
      .toEqual([]);
  }
}

// c-one-component, owed as rows 1 and 2 are: on the gate of the server's tree read
// (`treeReadGate`), of its Decisions view (`decisionsViewServedGate`), of an issue whose
// documents are nodes of its tree (`convertedGate`), and of the seeds written as decision nodes of
// the tree read (`decisionNodesGate`, read once the Decisions view has opened). Dashboard's part
// (dispatch://INFRA-5702), which makes root R42's decision card and ask body, is not on this
// branch's base, and no mark of the card is known before it lands, so the row reads the check's
// (3) and (4) alone and records the case it read (`ROW_THREE_CASE`); where that part lands after
// this one, it moves these rows onto the card and the ask body (the pair rule).
// SLOW(>=2s): two seeded issues, each opened at its Decisions view and read until it holds still,
// a Change and a Keep each waited on until the server's reads redraw the list, and every row of
// the view focused and read.
test("the Decisions view draws the one card and the one ask body @owed", async ({ browser }) => {
  test.setTimeout(ROW_THREE_MS);
  test.info().annotations.push({ description: ROW_THREE_CASE, type: "case" });
  await phase(browser, seedReviewList, readRowThree);
  await phase(browser, seedDecisionsFilter, readRowThree);
});

// ---------------------------------------------------------------------------------------------
// Row 4: each part of the Decisions view pages 20 entries at a time.
// ---------------------------------------------------------------------------------------------

/** How many entries a part of the Decisions view draws as it opens, and how many a press of the
 *  control under it draws at most (`DecisionsFilterBody.tsx`' `PAGE`, root R40). */
const ENTRY_PAGE = 20;

/** The most lines an entry may draw and still be within the six-line cut the view no longer
 *  makes: each decision row 4 writes draws more. */
const SIX_LINES = 6;

/** The parts `seedReviewList` draws in the Decisions view, in the order it draws them: it holds
 *  none of alice's own decisions. */
const PAGED_PARTS = ["open_questions", "review", "agents"] as const;
type PagedName = (typeof PAGED_PARTS)[number];

/** What row 4 reads of each part `parts` (`decisionsPages`): its count, how many entries it draws,
 *  which of them are cut or folded, which of the decisions the row wrote (`written`, by row id)
 *  draw six lines or fewer, what each control under it says (`nextCount`), and what the foot's
 *  control says of it. */
function pagesRead(parts: readonly PagedPart[], written: ReadonlySet<string>) {
  return parts.map((part) => ({
    count: part.count,
    cut: part.entries.flatMap((entry) => (entry.cut || entry.folded ? [entry.id] : [])),
    drawn: part.entries.length,
    next: part.next.map(nextCount),
    part: part.part,
    short: part.entries.flatMap((entry) =>
      written.has(entry.id) && entry.lines <= SIX_LINES ? [entry.id] : []
    ),
    unfolded: part.unfolded,
  }));
}

/** What `pagesRead` reads where each part holds `totals` entries: each headed part draws all of
 *  them and no control; the foot, unfolded, draws its first `footPages` pages of `ENTRY_PAGE`,
 *  and under them, while it draws fewer than it holds, one control that says how many are left
 *  and draws a page more at most; no entry is cut or folded, and each decision the row wrote
 *  draws more than six lines. */
function pagesWanted(totals: Readonly<Record<PagedName, number>>, footPages: number) {
  return PAGED_PARTS.map((part) => {
    const total = totals[part];
    const drawn = part === "agents" ? Math.min(total, ENTRY_PAGE * footPages) : total;
    const left = total - drawn;
    return {
      count: String(total),
      cut: [],
      drawn,
      next: left === 0 ? [] : [{ left, shows: Math.min(ENTRY_PAGE, left) }],
      part,
      short: [],
      unfolded: part === "agents" ? "true" : null,
    };
  });
}

/** The ids of the entries the foot of `parts` draws, in the order it draws them. */
function footOf(parts: readonly PagedPart[]): string[] {
  return parts.find((part) => part.part === "agents")?.entries.map((entry) => entry.id) ?? [];
}

// c-pages, owed as row 1 is: on `treeReadGate`, `decisionsViewServedGate`, `convertedGate` and
// `decisionNodesGate`. Before the page opens the row writes 25 decisions that no statement rests
// on, each with words past six lines at 1440 px, so the foot holds more than a page and the
// six-line cut would reach each of them. The foot is the one part of the seed past a page:
// c-unit's `each part draws 20 entries at a time, with the count left` covers every part.
// SLOW(>=2s): a seeded issue with 25 long decisions written, its Decisions view loaded twice and
// read until it holds still as it opens and after each press of the foot's control, by the
// pointer and then by the keyboard.
test("each part of the Decisions view pages 20 entries at a time @owed", async ({ browser }) => {
  test.setTimeout(ROW_FOUR_MS);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const { key } = seeded;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await decisionsViewServedGate(page, key));
    owedUnless(await convertedGate(page, key));
    // Before the page opens: the decisions the planner writes in the spec with no `shaped`
    // pointer, so each stands at the foot.
    const blocks = await writeFootDecisions(key);
    const decisions = decisionsReadOf(page, key);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(key, "decisions"));
    await expectIssuePageDrawn(page);
    const answer = await readTree(key, null);
    owedUnless(decisionNodesGate(answer, await heldDecisionsRead(key, decisions)));
    expect(answer.converted, "the tree read answers converted: true").toBe(true);
    const written = new Set((await decisionNodesOf(key, blocks)).map(decisionRowId));
    const reads = await idReadsOf(key);
    const totals = {
      agents: written.size + [seeded.tiedToNote, seeded.untied].length,
      open_questions: (reads.asks ?? []).filter(
        (ask) => ask.state === "open" && ask.kind !== "approval"
      ).length,
      review: [seeded.tiedNewer, seeded.tiedOlder].length,
    };
    const every = String(totals.agents + totals.open_questions + totals.review);
    const readPages = (what: string) =>
      steadily(page, traffic, what, async () => ({
        control: ((await presetCount(page, "decisions").textContent()) ?? "").trim(),
        parts: await decisionsPages(page),
      }));

    // (1) As the view opens the foot draws its first 20 entries, each whole, and under them one
    // control that says how many are left, the foot's entries less 20; each other part draws all
    // of its entries and no control. (3) The Decisions control counts every entry.
    const opening = await readPages(`${key}'s Decisions view as it opens`);
    expect(pagesRead(opening.parts, written), "a part draws its first 20 entries").toEqual(
      pagesWanted(totals, 1)
    );
    expect(
      opening.control,
      "the Decisions control's count counts every entry, before a press"
    ).toBe(every);

    // (2) Each press of the control draws the foot's next entries, at most 20, after the ones it
    // drew, none cut and none folded, until none is left and the control goes. (3) The Decisions
    // control's count stays every entry's.
    const pressThrough = async (
      by: string,
      press: (control: Locator) => Promise<void>,
      start: readonly string[]
    ): Promise<void> => {
      let before = start;
      for (let footPages = 2; ENTRY_PAGE * (footPages - 1) < totals.agents; footPages += 1) {
        const pressed = `press ${footPages - 1} by ${by}`;
        const control = nextEntries(page, "agents");
        await expect(control, `the foot draws its control before ${pressed}`).toBeVisible();
        await press(control);
        const reading = await readPages(`${key}'s Decisions view after ${pressed}`);
        expect(
          {
            before: footOf(reading.parts).slice(0, before.length),
            pages: pagesRead(reading.parts, written),
          },
          `the control draws the next entries, ${pressed}`
        ).toEqual({ before, pages: pagesWanted(totals, footPages) });
        await expect(
          nodeRow(page, footOf(reading.parts)[before.length] ?? "").first(),
          `the focus is on the first entry ${pressed} drew`
        ).toBeFocused();
        expect(
          reading.control,
          `the Decisions control's count counts every entry, after ${pressed}`
        ).toBe(every);
        before = footOf(reading.parts);
      }
    };
    await pressThrough("the pointer", (control) => control.click(), footOf(opening.parts));

    // The same by the keyboard, from a new load. The control stands out of the tab order as a row
    // of the tree (decision 366), so the foot's last drawn row, the row before it, takes the focus,
    // and `j` there presses it.
    await page.reload();
    await issueRead(page);
    await expectIssuePageDrawn(page);
    const reloaded = await readPages(`${key}'s Decisions view after a reload`);
    expect(
      pagesRead(reloaded.parts, written),
      "a part draws its first 20 entries, after a reload"
    ).toEqual(pagesWanted(totals, 1));
    await pressThrough(
      "the keyboard",
      async () => {
        const last = footOf(await decisionsPages(page)).at(-1);
        expect(last, "the foot draws no row before its control").toBeDefined();
        await focusRow(page, last ?? "");
        await page.keyboard.press("j");
      },
      footOf(reloaded.parts)
    );
  });
});

// ---------------------------------------------------------------------------------------------
// Row 5: a decision another person made stands under "Decided by others".
// ---------------------------------------------------------------------------------------------

// Decision 502, owed as rows 1 to 4 are: on `treeReadGate`, `decisionsViewServedGate`,
// `convertedGate` and `decisionNodesGate`, each read from alice's page as it opens on the
// Decisions view. On `seedDecisionsFilter`, whose asks alice answered or declined and whose fifth
// decision she kept, bob opens the same view at 1440 px in a context of his own: none of alice's
// four decisions stands under Your decisions, and all four stand under "Decided by others", after
// the review list and before the foot, each answer naming alice. alice's own view still draws
// them under "Your decisions", and no part for others. c-unit's `another person's answer stands
// under Decided by others with their name` covers the part's count, its place after the reader's
// own and a reader who made every decision.
// SLOW(>=2s): two readers' contexts each load the seeded issue.
test("a decision another person made stands under Decided by others @owed", async ({ browser }) => {
  test.setTimeout(ROW_FIVE_MS);
  await phase(browser, seedDecisionsFilter, async (page, seeded) => {
    const { key } = seeded;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await decisionsViewServedGate(page, key));
    owedUnless(await convertedGate(page, key));
    const decisions = decisionsReadOf(page, key);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(key, "decisions"));
    await expectIssuePageDrawn(page);
    const answer = await readTree(key, null);
    owedUnless(decisionNodesGate(answer, await heldDecisionsRead(key, decisions)));
    expect(answer.converted, "the tree read answers converted: true").toBe(true);

    // alice's four decisions the seed made, by the ids their rows carry: the decision she kept,
    // the question she declined, and the two she answered, whose answers name her.
    const reads = await idReadsOf(key);
    const answered = [askRowId(reads, seeded.optionedAskId), askRowId(reads, seeded.answeredAskId)];
    const hers = [
      blockRowId(key, answer, "decision", seeded.keptBlock),
      askRowId(reads, seeded.declinedAskId),
      ...answered,
    ];

    const bob = await asUser(browser, "bob");
    try {
      await withIssuePage(bob, "bob");
      const bobPage = await bob.newPage();
      await bobPage.setViewportSize({ height: 900, width: 1440 });
      const bobTraffic = watchApiRequests(bobPage);
      await gotoIssue(bobPage, presetPath(key, "decisions"));
      await expectIssuePageDrawn(bobPage);
      // What bob's Decisions view draws, as it holds still: the parts it draws in order, the rows
      // of his own part and of what others decided, that part's heading, and who each of alice's
      // answers names.
      const bobs = await steadily(
        bobPage,
        bobTraffic,
        `${key}'s Decisions view as bob reads it`,
        async () => {
          const parts = await decisionsPartsDrawn(bobPage);
          const rows = await drawnRows(bobPage);
          return {
            authors: answered.map((id) => rowOf(rows, id)?.answerAuthor ?? null),
            heading: await decisionsPartHeading(bobPage, "others"),
            others: parts.find((one) => one.part === "others")?.rows ?? [],
            parts: parts.map((one) => one.part),
            yours: parts.find((one) => one.part === "yours")?.rows ?? [],
          };
        }
      );

      // (1) bob made none of the seed's decisions: his view draws no part of his own, or one
      // that holds none of alice's.
      expect(
        bobs.yours.filter((id) => hers.includes(id)),
        "none of alice's four decisions stands under Your decisions"
      ).toEqual([]);
      // (2) All four stand under "Decided by others", a part the view draws after the review
      // list and before the foot, and each of her answers names her.
      expect(
        {
          authors: bobs.authors,
          heading: bobs.heading,
          missing: hers.filter((id) => !bobs.others.includes(id)),
          order: bobs.parts.filter(
            (part) => part === "review" || part === "others" || part === "agents"
          ),
        },
        "all four stand under Decided by others, after the review list and before the foot"
      ).toEqual({
        authors: answered.map(() => "alice"),
        heading: DECISIONS_WORDS.parts.others,
        missing: [],
        order: ["review", "others", "agents"],
      });
    } finally {
      await bob.close();
    }

    // (3) alice's own view of the same issue still draws her decisions under "Your decisions",
    // and draws no part for what others decided.
    const alices = await steadily(page, traffic, `${key}'s Decisions view as alice reads it`, () =>
      decisionsPartsDrawn(page)
    );
    const yours = alices.find((one) => one.part === "yours")?.rows ?? [];
    expect(
      {
        missing: hers.filter((id) => !yours.includes(id)),
        others: alices.some((one) => one.part === "others"),
      },
      "alice's own view draws her decisions under Your decisions, and no part for others"
    ).toEqual({ missing: [], others: false });
  });
});

// ---------------------------------------------------------------------------------------------
// Row 6: every decision of a paged issue is in its part, the header's count and the Spec column.
// ---------------------------------------------------------------------------------------------

/** The header's count of what waits for alice while the read its decisions are counted from
 *  names a next page: a number and "+" (part T's R15, by INFRA-1196's architect at 13:20Z). */
const WAITING_SO_FAR = new RegExp(`^${PROPERTY_LABELS.waiting_on_you} \\(\\d+\\+\\)$`);

/** A filter's count, and the Decisions control's to-review flag, while the read it is counted
 *  from names a next page: a number and "+". */
const COUNTED_SO_FAR = /^\d+\+$/;

/** One entry of the Decisions view: its node in the view's read, the id its row carries, and the
 *  part it stands in. */
interface ViewEntry {
  readonly id: string;
  readonly node: IssueTreeNode;
  readonly part: string;
}

/**
 * Each decision and each answered question of the Decisions view's read `view`, with the part it
 * stands in by R2's rules: a decision that waits for alice's review in the review list
 * (`decisionsWaitingOnAlice`, its ties read from the Spec view's read `spec`, which holds each
 * decision whose tie names one of its statements, as `view` holds no statement); an agent's
 * decision no tie binds to a statement, a replaced one among them, at the foot; and a question a
 * person answered, which is that person's decision, under Your decisions where alice answered it
 * and under Decided by others where another person did. A decision in none of those parts - one
 * a person kept, changed or decided - is one this row reads no part for, and it fails the row,
 * named.
 */
function viewEntries(view: IssueTreeRead, spec: IssueTreeRead, reads: TreeIdReads): ViewEntry[] {
  const ties = tiesToStatements(spec, reads);
  const waiting = new Set(decisionsWaitingOnAlice(view, ties).map((node) => node.id));
  const tied = new Set(ties.map(({ from }) => from.id));
  return (view.nodes ?? []).flatMap((node): ViewEntry[] => {
    if (node.issue !== undefined || node.role === "context") return [];
    if (node.supertag === "ask") {
      const ask = askOfNode(node, reads);
      if (ask?.state !== "answered") return [];
      const part = ask.answer?.user === "alice" ? "yours" : "others";
      return [{ id: askRowId(reads, ask.id), node, part }];
    }
    if (node.supertag !== "decision") return [];
    const id = decisionRowId(node);
    if (waiting.has(node.id)) return [{ id, node, part: "review" }];
    if (!tied.has(node.id) && !decidedByAPerson(view, node)) return [{ id, node, part: "agents" }];
    throw new Error(`the Decisions view's read holds ${id}, a decision this row reads no part for`);
  });
}

// The row INFRA-1196's architect asked for at 11:42Z over a paged tree read, owed as rows 1 to 5
// are: on `treeReadGate`, `decisionsViewServedGate`, `convertedGate` and `decisionNodesGate`, the
// last read once Decisions is pressed. Its first step is no owed gate: it fails the row where the
// seed did not page - where Conversation's first page names no cursor, or holds one of the three
// nodes the seed writes past it - and the `PAGED-ROW` line it logs before that says where those
// nodes landed, since the seed's paging is measured only where a browser runs the row. The read
// answers the issue's conversation first, in the order it was written, so the open questions the
// seed writes before the answered one fill the first page, and every block of the spec, each
// statement and each decision but the one under the open question (`from_ask`, which stands it
// right after that question), stands past it. So the header's count before Decisions counts the
// open questions, which wait on alice, and that decision, which waits for her review, and reads
// "+" for the pages not read yet; so does the Decisions filter's count, and so does its
// "to review" flag, which the page draws from that decision alone while only Conversation's
// first page is read. The Decisions view's read holds the questions on one page, so once it has
// answered the header counts them with every decision that waits, with no "+". Every value the
// row expects is read from the server's raw reads - a node's `decision` members, its `decided_by`
// tuples and the comments the read hangs under it in the Decisions view's read, the `shaped`
// ties of the Spec view's, the issue read and the asks read; it calls none of the page's own
// classifiers (`restatesAnswer`, `decisionNodeChanges`, `inReviewList`), so a bug in one moves
// the page and not the row's expectation. Its control is U's code at 776d9d85, which draws the
// parts from Conversation's root. Step (2)'s poll before Decisions is pressed has a control of
// its own: `IssuePresets.tsx`' `Signal` with only the flag's "+" dropped, which leaves the
// header's count and the filter's count with their "+", so the poll fails on the flag alone. Its
// step (4), on the decision past the first page that waits for review, has two controls of its
// own: `DecisionActions.tsx` as at cc5b97fe, which finds its decision in the outline read alone,
// which holds no node of that decision, so the row draws neither Keep nor Change and the step's
// first assertion fails; and the Changes read from Conversation's read alone
// (`decisions-entries.ts`' `changesOf` and `waiting.ts`' `waitingOnReader` as at cc5b97fe), which
// holds no Change on that decision, so it stays in the review list and the step's "Changed by"
// assertion fails.
// SLOW(>=2s): a seeded issue whose spec holds twenty open questions of some 10,000 characters
// each, Conversation's read of it read page by page, its Decisions and Spec views each pressed and
// read until they hold still, and a Change waited on until the page's reads redraw the Decisions
// view and the header's count.
test("every decision of a paged issue is in its part, the header's count and the Spec column @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_SIX_MS);
  await phase(browser, seedPagedDecisions, async (page, seeded) => {
    const { key } = seeded;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await decisionsViewServedGate(page, key));
    owedUnless(await convertedGate(page, key));
    const decisions = decisionsReadOf(page, key);
    const traffic = watchApiRequests(page);
    const writes = watchWrites(page);
    const firstPage = conversationFirstPage(page);
    await gotoIssue(page, `/issues/${key}/conversation`);
    await expectIssuePageDrawn(page);
    const first = await firstPage;
    const reads = await idReadsOf(key);

    // (0) The seed pages: Conversation's first page, the page's own request with no cursor,
    // names a cursor and holds none of the three nodes the seed writes past it, which the whole
    // read holds. The rest of the read is read as the page's own next requests would read it.
    const kinds = kindsSent(traffic.requests, key)[0] ?? [];
    expect(kinds, "the page sends Conversation's read with its kind= values").not.toEqual([]);
    const conversation = await conversationRead(page, key, kinds, first);
    const paged = [
      {
        name: `the decision ${seeded.waitingBlock}, which waits for review`,
        of: (node: IssueTreeNode) =>
          node.supertag === "decision" && (node.block_id ?? node.id) === seeded.waitingBlock,
      },
      {
        name: `alice's answered question ${seeded.answeredAskId}`,
        of: (node: IssueTreeNode) => node.id === seeded.answeredAskId,
      },
      {
        name: `the decision ${seeded.underAskBlock} under it`,
        of: (node: IssueTreeNode) =>
          node.supertag === "decision" && (node.block_id ?? node.id) === seeded.underAskBlock,
      },
    ];
    const firstNodes = first.nodes ?? [];
    const wholeNodes = conversation.whole.nodes ?? [];
    const pageQuestions = firstNodes.filter((node) => seeded.pageQuestions.includes(node.id));
    const isFirstDecision = (node: IssueTreeNode) =>
      node.supertag === "decision" && (node.block_id ?? node.id) === seeded.firstBlock;
    const firstDecision = wholeNodes.findIndex(isFirstDecision);
    console.log(
      `PAGED-ROW ${key}: Conversation's first page holds ${firstNodes.length} nodes, ` +
        `${pageQuestions.length} of the ${seeded.pageQuestions.length} long open questions ` +
        `among them, remaining ${String(first.remaining ?? null)}, its cursor ` +
        `${nextTreeCursor(first) === undefined ? "none" : "named"}; the whole answer holds ` +
        `${wholeNodes.length} nodes in ${conversation.pages} pages, with the decision ` +
        `${seeded.firstBlock}, tied to the first statement, at index ${firstDecision}, ` +
        paged.map(({ name, of }) => `${name} at index ${wholeNodes.findIndex(of)}`).join(", ")
    );
    expect(
      {
        cursor: nextTreeCursor(first) !== undefined,
        missing: paged.flatMap(({ name, of }) => (wholeNodes.some(of) ? [] : [name])),
        onTheFirstPage: paged.flatMap(({ name, of }) => (firstNodes.some(of) ? [name] : [])),
      },
      "the seed did not page: Conversation's first page names a cursor and holds none of the three nodes the read holds past it"
    ).toEqual({ cursor: true, missing: [], onTheFirstPage: [] });
    expect(
      firstNodes.some(isFirstDecision),
      "the seed's first decision, which settles the open question and waits for review, is on Conversation's first page, so the to-review flag is drawn before Decisions is pressed"
    ).toBe(true);

    // (2), before Decisions is pressed: the header's count of what waits for alice, the Decisions
    // filter's own count and its to-review flag are counted from Conversation's read, which names
    // a next page, so each reads a number and "+" (part T's R15). The flag is drawn because the
    // decision the seed stood under the open question (`seeded.firstBlock`) is on that page and
    // waits for review.
    await expect
      .poll(
        async () => ({
          decisions: ((await presetCount(page, "decisions").textContent()) ?? "").trim(),
          flag: await reviewFlagWords(page),
          header: await waitingOnYou(page),
        }),
        {
          message:
            "while Conversation's read names a next page, the header's count, the Decisions filter's count and its to-review flag each read a number and +",
        }
      )
      .toEqual({
        decisions: expect.stringMatching(COUNTED_SO_FAR),
        flag: expect.stringMatching(COUNTED_SO_FAR),
        header: expect.stringMatching(WAITING_SO_FAR),
      });

    // (1) The Decisions view: each decision and answered question of its read stands in its part,
    // read from the parts the page draws - the decisions that wait in the review list, the
    // question alice answered under Your decisions, and the decision under it in the review list.
    await readFilter(page, traffic, "decisions");
    const [whole, held, view, spec] = await Promise.all([
      readTree(key, null),
      heldDecisionsRead(key, decisions),
      readTree(key, "decisions"),
      readTree(key, "spec"),
    ]);
    owedUnless(decisionNodesGate(whole, held));
    const specTies = tiesToStatements(spec, reads);
    /** What waits for alice as the header counts it over `answer`, a read of the Decisions view:
     *  each question that waits on her and each decision of `answer` that waits for her review. */
    const waitingOnAlice = (answer: IssueTreeRead) =>
      asksWaitingOnAlice(reads.issue) + decisionsWaitingOnAlice(answer, specTies).length;
    const entries = viewEntries(view, spec, reads);
    expect(
      paged.flatMap(({ name, of }) => (entries.some(({ node }) => of(node)) ? [] : [name])),
      "each of the three nodes past Conversation's first page is an entry of the Decisions view's read"
    ).toEqual([]);
    await expect
      .poll(
        async () => {
          const drawn = await decisionsPartsDrawn(page);
          return entries.map(({ id }) => ({
            id,
            parts: drawn.flatMap((part) => (part.rows.includes(id) ? [part.part] : [])),
          }));
        },
        {
          message:
            "each decision and answered question of the Decisions view's read stands in its part",
        }
      )
      .toEqual(entries.map(({ id, part }) => ({ id, parts: expect.arrayContaining([part]) })));

    // (2), once Decisions is on and its view read has answered: the header's count is counted
    // from that read, which names no next page, so it reads the whole number with no "+" - each
    // decision of the read that waits for alice's review and each question that waits on her.
    await expect
      .poll(() => waitingOnYou(page), {
        message:
          "once the Decisions view's read has answered, the header counts every decision of it that waits for alice and every question that waits on her, with no +",
      })
      .toBe(waitingWords(waitingOnAlice(view)));
    await releaseFilter(page, "decisions");

    // (3) The Spec view: each statement a decision shaped draws that decision's line - its cell
    // spans the statement, or the statement's line links to it - and alice's answered question is
    // drawn as hers: the cell beside each statement it shaped says she said it ("you") over her
    // answer's words, and never "Because an agent decided". The cell quotes the first sentence
    // of an answer, and the seed's answer is one sentence, so the cell holds its words whole.
    await readFilter(page, traffic, "spec");
    const column = await steadily(page, traffic, "the Spec view's column", () => specColumn(page));
    const cells = await steadily(page, traffic, "the Spec view's cells", () => specCells(page));
    const idOf = treeIdOf(spec, reads);
    const ties = tiesToStatements(spec, reads);
    expect(
      paged.flatMap(({ name, of }) => (ties.some(({ from }) => of(from)) ? [] : [name])),
      "each of the three nodes past Conversation's first page has a tie to a statement the Spec view's read holds"
    ).toEqual([]);
    expect(
      ties.flatMap(({ from, to }) =>
        statementsBeside(column, decisionNames(from, reads, idOf)).includes(idOf(to.id))
          ? []
          : [`${idOf(from.id)} stands beside no row of ${idOf(to.id)}, which its tie names`]
      ),
      "under Spec, each statement a decision shaped draws that decision's line"
    ).toEqual([]);
    const persons = ties.filter(({ from }) => from.supertag === "ask");
    expect(
      persons.map(({ from, to }) => {
        const names = decisionNames(from, reads, idOf);
        const cell = cells.find((one) => one.statements.includes(idOf(to.id)));
        return {
          heading: cell?.heading ?? null,
          hers: cell?.names.some((name) => names.includes(name)) ?? false,
          quote: cell?.quote ?? null,
          said: cell?.said ?? null,
          statement: idOf(to.id),
        };
      }),
      "under Spec, the cell beside each statement a person's answer shaped draws it as that person's, never as an agent's"
    ).toEqual(
      persons.map(({ from, to }) => {
        const answer = askOfNode(from, reads)?.answer;
        const alices = answer?.user === "alice";
        return {
          heading: alices ? SPEC_WORDS.said_you : SPEC_WORDS.said_person,
          hers: true,
          quote: expect.stringContaining((answer?.text ?? "").trim()),
          said: alices ? "you" : "person",
          statement: idOf(to.id),
        };
      })
    );
    await releaseFilter(page, "spec");

    // (4) The decision past Conversation's first page that waits for review: under Decisions its
    // row in the review list draws Keep and Change in their words, which find their decision in
    // the Decisions view read the row is drawn from (`DecisionActions.tsx`); alice's Change on it,
    // sent through the page as row 2 sends one, anchors to the decision node's own document and
    // block at the first match of its quote there (`occurrence` 0); and once the page has read
    // again the decision reads "Changed by" alice, stands among her own decisions and out of the
    // review list, and the header counts one thing less that waits for her than before the
    // Change, each count read from the page and from the Decisions view's read, as (2) reads them:
    // before the Change from (1)'s read of it, which no write has moved since.
    await readFilter(page, traffic, "decisions");
    const past = blockNodeIn(
      view.nodes ?? [],
      "decision",
      seeded.waitingBlock,
      `${key}'s Decisions view read`
    );
    const pastId = decisionRowId(past);
    const pastRow = nodeRow(page, pastId).first();
    const pastName = `the decision ${seeded.waitingBlock} past Conversation's first page`;
    await expect
      .poll(
        async () => ({
          controls: (await pastRow.locator("button[data-row-action]").allTextContents()).map(
            (text) => text.replace(/\s+/g, " ").trim()
          ),
          inReview: (await decisionsPartRows(page, "review")).includes(pastId),
        }),
        {
          message: `${pastName}, which waits for review, stands in the review list and draws Keep and Change in their words`,
        }
      )
      .toEqual({
        controls: [CONTROL_LABELS.keep_decision, CONTROL_LABELS.change_decision],
        inReview: true,
      });
    const waitingBefore = waitingOnAlice(view);
    await expect
      .poll(() => waitingOnYou(page), {
        message: `before the Change on ${pastName}, the header counts every decision of the Decisions view's read that waits for alice and every question that waits on her`,
      })
      .toBe(waitingWords(waitingBefore));
    const changeWords = {
      anchored: "anchors to the decision node's own document and block, at occurrence 0",
      change: `the Change on ${pastName}`,
    };
    await openChange(pastRow, changeWords);
    await sendChange(pastRow, past, changeWords, { key, traffic, writes });
    await expect
      .poll(() => changedStanding(page, pastId), {
        message: `once the page has read again, ${pastName} reads Changed by alice, stands among her own decisions and is not in the review list`,
      })
      .toEqual({ inReview: false, own: true, state: expect.stringMatching(CHANGED_BY_ALICE) });
    await expect
      .poll(
        async () => ({
          header: await waitingOnYou(page),
          view: waitingOnAlice(await readTree(key, "decisions")),
        }),
        {
          message: `once the page has read again, the header's count and the Decisions view's read each count one thing less that waits for alice than before the Change on ${pastName}`,
        }
      )
      .toEqual({ header: waitingWords(waitingBefore - 1), view: waitingBefore - 1 });
  });
});
