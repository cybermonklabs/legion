/** a write the server settles changes what the outline draws; another reader's page holds what
 *  the read answers it */

import type { IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { chipWords } from "../web/src/features/issue-page/node-row-chip";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  CONTROL_LABELS,
  GAP_WORDS,
  GLYPH_TOKENS,
  LENS_WORDS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  phrasesIn,
  ROW_MARK_WORDS,
  SETTLE_ANSWERS,
} from "../web/src/features/issue-page/page-words";
import { treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { parseNodeReference } from "../web/src/features/refs/routes";
import { wordsOf as readingWords } from "../web/src/features/refs/tree-fragment";
import { pageStateText } from "../web/src/theme/classes";
import { answerAsk, getIssue } from "./api";
import { seedWholeIssue } from "./issue-layout-seed";
import { readTree } from "./issue-layout-seed-support";
import { contentsPane, gotoIssue, issuePage, issueRead, openDetails, phase } from "./issue-page";
import {
  DESKTOP,
  expectOpeningLanded,
  focusRow,
  nodeRow,
  nodeRows,
  PHONE,
  READER,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  activePreset,
  expectActivePreset,
  focusedNodeId,
  onScreen,
  openEveryDeeperBranch,
  pickPreset,
  rowGaps,
  treeText,
} from "./issue-page-i";
import { contentsEntries } from "./issue-page-j";
import { planChart, planChartRows, planView } from "./issue-page-k";
import { withIssuePage } from "./issue-page-option";
import { waitingCount } from "./issue-page-p";
import { decisionsPartRows } from "./issue-page-q";
import {
  convertedGate,
  type Gate,
  heldAnswer,
  OWED_BY,
  owedUnless,
  type TreeAnswerWatch,
  type TreeView,
  treeAnswerOf,
  treeReadGate,
  treeReads,
  viewAnswerOf,
  viewCount,
} from "./issue-page-t-tree";
import { checkLegendCounts, verificationCheck } from "./issue-page-v";
import {
  drawnIdOf,
  focusedRowReadId,
  heldTreeAnswer,
  lensLines,
  lensShowChain,
} from "./issue-page-w";
import {
  type AuthorOnlySeed,
  childItems,
  compactMarks,
  coveredCount,
  type DrawnMark,
  FOCUSED_FACTS_GATE,
  type FoldedCount,
  filterCount,
  flagCount,
  focusedNodeFacts,
  graphGate,
  graphName,
  type HeldRow,
  heldUnder,
  marginComments,
  NOBODY_WROTE,
  type NodeStateSeed,
  nodeHolding,
  nodeStateRequests,
  noteOwed,
  ownMarks,
  RESERVED_NODES,
  rawValueTexts,
  referencedByCount,
  referencesTo,
  reservedUnder,
  STATEMENT_8_WORD,
  seedAuthorOnly,
  seedGateIssue,
  seedNodeStates,
  settleNodeApi,
  settlesSent,
  treeIdReads,
  treeItemsDrawn,
  UNDRAWN_KINDS,
} from "./issue-page-x";
import { type Browser, expect, type Locator, type Page, type Route, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";
import { asUser } from "./users";

/**
 * The rows of a node's life on the new issue page - a proposed node marked and settled, an
 * overtaken one drawn open and marked, an overtaken question that stays a Contents entry and is
 * counted in no count of what waits, each way to a node reaching an overtaken one, a question's
 * row marked by its ask's node - and of a node reserved for its author, read on the real stack
 * (`web/src/features/issue-page/node-states/`, with the helpers of `e2e/issue-page-x.ts`).
 *
 * Each row turns the new page on for alice with `phase` (`e2e/issue-page.ts`) and checks its
 * gates first, on an issue of its own (`seedGateIssue`): the tree read is served (`treeReadGate`,
 * which reads its 404) and an issue is born converted (`convertedGate`), each with `owedUnless`
 * (`e2e/issue-page-t-tree.ts`). Where a gate is not met the row skips with an `owed` annotation
 * that names it, having written nothing else, and never passes; once both are met it runs whole:
 * it seeds its issue with `seedWholeIssue` and the seeds of `e2e/issue-page-x.ts`, asserts that
 * issue's tree read answers `converted: true`, and reads it. The proposed and overtaken nodes are
 * written by the addresses the tree read answers alice (`seedNodeStates`), a question's by its
 * ask's; the reserved nodes' writes answer their own (`seedAuthorOnly`).
 *
 * Every node a row names it names by the id the tree read gives it, and every row it reads by the
 * id the page draws it under for that read id (`drawnIdOf`, `treeIdOf`): no row builds an id. A
 * `@phone` row reads 1440 px on `chromium` and 390 px on `iphone` (`rowSize`). Every word a row
 * reads is the page's own word list's (`page-words.ts`), never typed here.
 *
 * What a row reads of code that is not under this head it reads behind a gate, and notes as owed
 * where the gate is not met (`noteOwed`): Details' facts of the focused row
 * (`FOCUSED_FACTS_GATE`), each Graph reading (`graphGate`), and each reading of the criterion the
 * seed wrote under the overtaken question (`criterionGate`: part Y draws criteria, and until it
 * does the page draws and counts the statement under the question alone). The page opens with
 * every node drawn open (part T's R11), so what a row reads of a node drawn open on load - row
 * 1's readings of statement 8, row 4's of the overtaken question, row 3 whole - is read
 * unconditionally and never owed.
 */

/** The mark of a proposed node, an overtaken one and one reserved for its reader, each as the
 *  page draws it: its word and its shape, drawn. */
const PROPOSED: DrawnMark = {
  mark: "proposed",
  shape: GLYPH_TOKENS.marks.proposed,
  shown: true,
  word: ROW_MARK_WORDS.proposed,
};
const OVERTAKEN: DrawnMark = {
  mark: "overtaken",
  shape: GLYPH_TOKENS.marks.overtaken,
  shown: true,
  word: ROW_MARK_WORDS.overtaken,
};
const ONLY_YOU: DrawnMark = {
  mark: "only_you",
  shape: GLYPH_TOKENS.marks.only_you,
  shown: true,
  word: ROW_MARK_WORDS.only_you,
};

/** The settle route, whatever node and issue it names. */
const SETTLE_ROUTE = "**/api/v1/issues/*/nodes/*/settle";

/** The focused row's Settle act. */
const SETTLE_ACT = '[data-row-act="settle"]';

/** Keep and Change on a decision's row, by the mark `DecisionActions.tsx` puts on each
 *  (`data-row-action`). */
const KEEP_OR_CHANGE = 'button[data-row-action="keep"], button[data-row-action="change"]';

/** The four views, by the filter each reads its view of the tree for. */
const VIEWS = ["spec", "verification", "plan", "decisions"] as const satisfies readonly TreeView[];

/** The words the page's word list writes, as it writes them, which a text may be whole: a raw
 *  value of the tree read, or a word-list key, is none of them. */
const WORD_BOOK: readonly string[] = [
  ...new Set(
    [
      CONTROL_LABELS,
      GAP_WORDS,
      LENS_WORDS,
      NODE_STATE_WORDS,
      NODE_TYPE_NAMES,
      PRESET_NAMES,
      ROW_MARK_WORDS,
      SETTLE_ANSWERS,
    ].flatMap(phrasesIn)
  ),
];

/** What no row of the page reads: a count of overtaken nodes in the words of a line that folds
 *  them away, "1 overtaken", in the mark's word in lower case after a number. */
const OVERTAKEN_COUNT = new RegExp(`\\d+\\s+${ROW_MARK_WORDS.overtaken.toLowerCase()}`, "u");

/** The size a `@phone` row reads: 390 px on the `iphone` project, 1440 px on every other. */
function rowSize(): { readonly height: number; readonly width: number } {
  return test.info().project.name === "iphone" ? PHONE : DESKTOP;
}

/** How long a reading waits for the page to draw a row the read holds. */
const DRAWN_MS = 30_000;

/** Loads `address` on `page` and waits until the page has gone quiet. `traffic` is the page's one
 *  watcher (`watchApiRequests`), made before its first load, so a read the load still has in
 *  flight is one it waits for; once this returns, `traffic` holds this load's requests alone. */
async function gotoSettled(page: Page, traffic: ApiRequests, address: string): Promise<void> {
  traffic.reset();
  await gotoIssue(page, address);
  await expectLoadSettled(traffic);
}

/** The id the page draws the node `readId` names under, as the poll that found it drawn read it:
 *  each poll first opens every branch a fold or the drawn levels left shut, which ends with the
 *  focus on no row. */
async function drawnRowId(page: Page, readId: string, what: string): Promise<string> {
  const message = `the page draws no row for ${what}`;
  let id: string | null = null;
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        await openEveryDeeperBranch(page);
        id = await drawnIdOf(page, readId);
        return id;
      },
      { message, timeout: DRAWN_MS }
    )
    .not.toBeNull();
  if (id === null) throw new Error(message);
  return id;
}

/** The row the page draws for the node `readId` names, once it draws it (`drawnRowId`). */
async function drawnRow(page: Page, readId: string, what: string): Promise<Locator> {
  return nodeRow(page, await drawnRowId(page, readId, what)).first();
}

/** How many times `pressFTo` presses `f` before it gives up: past every stop a row draws. */
const STOPS_AT_MOST = 40;

/** Presses `f` until `target` holds the focus, as a reader steps through the focused row's links
 *  and controls, and fails with `message` where none does. */
async function pressFTo(page: Page, target: Locator, message: string): Promise<void> {
  for (let pressed = 0; pressed < STOPS_AT_MOST; pressed += 1) {
    // A stop is drawn once a press of f reaches its row, so each press reads it again.
    const drawn = (await target.count()) > 0;
    // One read per press: f moves the focus before the press returns, so this reads where it is.
    const reached = drawn && (await target.first().evaluate((el) => el === document.activeElement));
    if (reached) return;
    await page.keyboard.press("f");
  }
  await expect(target.first(), message).toBeFocused();
}

/** Focuses the row of the node `readId` names (`drawnRowId`, naming it `what`) and presses `f`
 *  until its Settle holds the focus, failing with `offerMessage` where it never does; answers the
 *  row and its Settle. */
async function reachSettle(
  page: Page,
  readId: string,
  what: string,
  offerMessage: string
): Promise<{ readonly row: Locator; readonly settle: Locator }> {
  const row = await focusRow(page, await drawnRowId(page, readId, what));
  const settle = row.locator(SETTLE_ACT);
  await pressFTo(page, settle, offerMessage);
  return { row, settle };
}

/** The block id an address names, or undefined for one that names no block. */
function blockOf(address: string): string | undefined {
  const route = parseNodeReference(address);
  return route !== undefined && "block" in route ? route.block?.id : undefined;
}

/** The block id `address` names; throws, naming `what`, where it names no block. */
function blockNamed(address: string, what: string): string {
  const block = blockOf(address);
  if (block === undefined) throw new Error(`${what}'s address names no block`);
  return block;
}

/** Fails where an element of the page has a raw value of the tree read, or a word-list key, as
 *  its whole text: the page names a node's life and its reader in its own words alone. */
async function expectNoRawValue(page: Page, where: string): Promise<void> {
  expect(
    await rawValueTexts(page, WORD_BOOK),
    `${where}, a text reads a state or a visibility as the tree read writes it`
  ).toEqual([]);
}

/** Fails where a row of the page reads a count of overtaken nodes: an overtaken node is a row at
 *  its place, so no line stands for a parent's overtaken children. */
async function expectNoOvertakenCount(page: Page, where: string): Promise<void> {
  await expect(issuePage(page), `${where}, a row reads a count of overtaken nodes`).not.toHaveText(
    OVERTAKEN_COUNT
  );
}

/** Runs `body` with forced colours on, and puts them back. */
async function withForcedColours(page: Page, body: () => Promise<void>): Promise<void> {
  await page.emulateMedia({ forcedColors: "active" });
  try {
    await body();
  } finally {
    await page.emulateMedia({ forcedColors: "none" });
  }
}

/** The node `readId` of `answer`'s words, as the page reads them: the words of its reading
 *  (`text_tree`) where it carries one, else its stored `text`. */
function wordsOf(answer: IssueTreeRead, readId: string): string {
  const node = answer.nodes?.find((held) => held.id === readId);
  if (node === undefined) return "";
  return node.text_tree === undefined ? node.text : readingWords(node.text_tree);
}

/** The chip inside `row` that names the node whose words are `words`. */
function chipNaming(page: Page, row: Locator, words: string): Locator {
  return row
    .locator("[data-node-chip]")
    .filter({ has: page.locator("[data-chip-name]", { hasText: chipWords(words) }) });
}

/**
 * Waits until the row of the node `readId` is drawn open, with a row under it for each of
 * `under`, as the page draws a branch it holds open: `aria-expanded` true, and each of those
 * nodes one level below it. Fails with `message`. Answers the read id of each row drawn right
 * under it, as the poll that found it open read them.
 */
async function expectDrawnOpen(
  page: Page,
  readId: string,
  under: readonly string[],
  message: string
): Promise<readonly (string | null)[]> {
  let found: readonly (string | null)[] = [];
  await expect
    .poll(
      async () => {
        const items = await treeItemsDrawn(page);
        const row = items.find((item) => item.readId === readId);
        const below = childItems(items, row).map((item) => item.readId);
        found = below;
        return {
          expanded: row?.expanded ?? null,
          held: under.every((id) => below.includes(id)),
        };
      },
      { message, timeout: DRAWN_MS }
    )
    .toEqual({ expanded: "true", held: true });
  return found;
}

/** In Graph, the row of the node `readId` names carries `mark`, read as `what`. */
async function expectMarkedInGraph(
  page: Page,
  readId: string,
  mark: DrawnMark,
  what: string
): Promise<void> {
  const row = await drawnRow(page, readId, `${what} in Graph`);
  await expect
    .poll(() => ownMarks(row), { message: `${what} does not carry ${mark.word} in Graph` })
    .toEqual([mark]);
}

/** With the Decisions view on, the question whose row the page draws under the id `question`
 *  stands with the open questions, carrying `mark`, read as `what`. The view's rows carry the
 *  tree's id and no read id (`DecisionsRow.tsx`), so a row is told by `data-node-id`. */
async function expectWithOpenQuestions(
  page: Page,
  question: string,
  mark: DrawnMark,
  what: string
): Promise<void> {
  await expect
    .poll(() => decisionsPartRows(page, "open_questions"), {
      message: `${what} stands with the open questions`,
      timeout: DRAWN_MS,
    })
    .toContain(question);
  await expect
    .poll(() => ownMarks(nodeRow(page, question).first()), {
      message: `${what} does not carry ${mark.word} under Decisions`,
    })
    .toEqual([mark]);
}

/** Whether the Decisions view's row `row` draws its state in the colour of a decision that waits
 *  on the reader: the decisions' violet, which the margin's state carries as its class
 *  (`NodeRow.tsx`' `RowMargin`, `pageStateText.decision`), as it does for a decision the reader
 *  kept or asked to change. Null where the row draws no state; a decision of the review part
 *  draws one, its standing with the reader. */
function drawnWaiting(row: Locator): Promise<boolean | null> {
  return row
    .first()
    .evaluate(
      (element, waiting) =>
        element
          .querySelector("[data-node-margin] [data-node-state]")
          ?.parentElement?.classList.contains(waiting) ?? null,
      pageStateText.decision
    );
}

/** What a row reads its page with: the issue, its tree read, the id the page draws each node of
 *  that read under, the comments the page draws in the margin beside a node's text, and the
 *  page's one watcher of its requests, made before its first load. */
interface LoadedPage {
  readonly answer: IssueTreeRead;
  readonly key: string;
  /** The comments the reader's comments read anchors, read once the load answered: a count
   *  counts none of them, nor their threads (`HeldRow.margin`). */
  readonly margin: ReadonlySet<string>;
  readonly plain: string;
  readonly seeded: NodeStateSeed;
  readonly traffic: ApiRequests;
  readonly treeId: (readId: string) => string;
}

/** A loaded page and the one reading it holds back until part Y draws criteria: a criterion row
 *  (`criteria`). */
interface SeededPage extends LoadedPage {
  readonly criteria: Gate;
}

/** Opens the seeded issue on `page` with the watcher made before its first load, and answers
 *  what a row reads it with once its tree read has answered what the seed wrote. */
async function loadedPage(page: Page, seeded: NodeStateSeed): Promise<LoadedPage> {
  const key = seeded.whole.key;
  const plain = presetPath(key, "conversation");
  const tree = treeAnswerOf(page);
  const traffic = watchApiRequests(page);
  await gotoSettled(page, traffic, plain);
  const answer = await heldTreeAnswer(tree, "the load", (held) =>
    (held.nodes ?? []).some(
      (node) => node.id === seeded.ids.statement7 && node.state === "proposed"
    )
  );
  const [reads, margin] = await Promise.all([treeIdReads(key), marginComments(key)]);
  return { answer, key, margin, plain, seeded, traffic, treeId: treeIdOf(answer, reads) };
}

/**
 * Whether the page draws a criterion as a row, read on the criterion the seed wrote under the
 * overtaken question: no step of `tree-read/tree-nodes.ts`'s `treeDraftSteps` drafts one at this
 * head, so the page draws and counts the statement under the question alone, and part Y
 * (INFRA-4066) draws criteria. The statement under the same question, which this part draws at
 * every head, is opened first (`drawnRowId`), so a criterion row the page draws is drawn by the
 * time it is looked for, and a page that draws neither fails that reading as itself and does not
 * turn the criterion owed. It opens every branch, so each reading that follows loads the page
 * again.
 */
async function criterionGate(page: Page, { seeded }: LoadedPage): Promise<Gate> {
  await drawnRowId(
    page,
    seeded.ids.questionStatement,
    "the statement under the overtaken question"
  );
  return {
    met: (await drawnIdOf(page, seeded.ids.questionCriterion)) !== null,
    owed:
      "owed until part Y (INFRA-4066) draws criteria: the page draws no criterion row here, so " +
      `the criterion under the overtaken question is neither read nor counted; ${OWED_BY}`,
  };
}

/** `loaded` with its criterion gate. */
async function seededPage(page: Page, loaded: LoadedPage): Promise<SeededPage> {
  return { ...loaded, criteria: await criterionGate(page, loaded) };
}

/**
 * What the header's count of what waits, the Decisions view's to-review flag and the header's
 * presses read of the overtaken question and the overtaken decision: neither is counted, and no
 * press reaches either. The count is the open questions whose turn is alice's, the proposed one
 * among them and the overtaken one left out, and the decisions that wait for review, the
 * overtaken one left out. Each press of the count puts the focus on the next thing that waits,
 * round to the first, so one more press than there are things reaches each once. The Decisions
 * view's rows carry the tree's id and no read id (`DecisionsRow.tsx`), so a row is told by
 * `data-node-id`. Answers the id of each row the presses reached.
 */
async function expectWaitsLeaveOut(page: Page, read: LoadedPage): Promise<readonly string[]> {
  const { key, seeded, treeId } = read;
  const { ids } = seeded;
  const overtaken = [ids.overtakenQuestion, ids.waitingDecision].map(treeId);
  await gotoIssue(page, presetPath(key, "decisions"));
  const asking = ((await getIssue(key)).open_asks ?? []).filter(
    (ask) =>
      ask.state === "open" &&
      ask.kind !== "approval" &&
      ask.waiting_on === "human" &&
      ask.waiting_on_person === READER &&
      ask.id !== seeded.overtakenAskId
  ).length;
  await expect
    .poll(async () => (await decisionsPartRows(page, "review")).length, {
      message: "the Decisions view draws no decision that waits for review",
      timeout: DRAWN_MS,
    })
    .toBeGreaterThan(0);
  // The poll above waited for the review part to draw, so one read of its rows is its whole list.
  const reviewing = (await decisionsPartRows(page, "review")).filter(
    (id) => id !== treeId(ids.waitingDecision)
  ).length;
  await expect
    .poll(() => flagCount(page, "to_review"), {
      message: "the to-review flag counts the overtaken decision",
    })
    .toBe(reviewing);
  const waits = asking + reviewing;
  await expect(
    waitingCount(page),
    "the header's count of what waits is not the questions that wait on alice and the decisions that wait for review, the overtaken ones left out"
  ).toHaveText(`${PROPERTY_LABELS.waiting_on_you} (${waits})`);

  const reached: string[] = [];
  let last: string | null = null;
  for (let pressed = 0; pressed <= waits; pressed += 1) {
    await waitingCount(page).press("Enter");
    const before = last;
    await expect
      .poll(
        async () => {
          const now = await focusedNodeId(page);
          return now !== null && now !== before;
        },
        {
          message:
            "a press of the header's count does not put the focus on the next thing that waits",
          timeout: DRAWN_MS,
        }
      )
      .toBe(true);
    last = await focusedNodeId(page);
    reached.push(last ?? "");
  }
  expect(
    reached.filter((id) => overtaken.includes(id)),
    "a press of the header's count reaches an overtaken node"
  ).toEqual([]);
  expect(
    new Set(reached).size,
    "the header's presses do not reach each thing that waits once"
  ).toBe(waits);
  return reached;
}

// ---------------------------------------------------------------------------------------------
// Row 1: a proposed node is marked and settled, and an overtaken one is drawn open and marked.
// ---------------------------------------------------------------------------------------------

/** What row 1 reads its page with, and the reading it holds back until the code it needs is under
 *  the head: a Graph control (`graph`), read on the first load, before any press. */
interface ProposedPage extends LoadedPage {
  readonly graph: Gate;
}

/** How many statements the seed writes: twelve, the one that cites a document, and the one under
 *  the overtaken question. */
function statementsOf(seeded: NodeStateSeed): number {
  return seeded.whole.requirements.length + 2;
}

/** How many statements no Verification count reads before statement 7 is settled: statement 7,
 *  which is proposed, statement 8, which is overtaken, and the one under the overtaken
 *  question. */
const UNCOUNTED_BEFORE_SETTLE = 3;

/**
 * The first reading of step (2), on the page as it first opened, before any press, unfold or
 * reload: statement 8 is drawn open with its lens, as every node is on load (part T's R11), at
 * 1440 px and at 390 px alike.
 */
async function expectOpenOnLoad(page: Page, read: ProposedPage): Promise<void> {
  const { ids } = read.seeded;
  await expectDrawnOpen(
    page,
    ids.statement8,
    [ids.lens],
    "statement 8 is drawn open with its lens"
  );
}

/**
 * The next readings of step (2) and the first of (3): the page draws statement 8 open, with its
 * lens under it, where its own fold rules do - at the address that names it, whose node and the
 * path to it the fold opens at every width, live or overtaken. `?node=` and `#b-` of statement 8
 * focus its row, on screen, and the row is open with its lens, at 1440 px and at 390 px. They
 * come before any press opens a branch, so what they read is the page's fold and not a reader's.
 */
async function expectOvertakenOpen(page: Page, read: ProposedPage): Promise<void> {
  const { plain, seeded, treeId } = read;
  const { ids } = seeded;
  // A `#b-` fragment names a block by its id in its document, which the tree read's own id for
  // the block (`<KEY>/<slug>#<block id>`) is not: it is read from the block's address.
  const block = blockNamed(seeded.addresses.statement8, "statement 8");
  for (const address of [
    `${plain}?node=${encodeURIComponent(ids.statement8)}`,
    `${plain}#b-${encodeURIComponent(block)}`,
  ]) {
    await gotoIssue(page, address);
    await expect
      .poll(() => focusedRowReadId(page), {
        message: `${address} does not focus statement 8`,
        timeout: DRAWN_MS,
      })
      .toBe(ids.statement8);
    await expectDrawnOpen(
      page,
      ids.statement8,
      [ids.lens],
      "statement 8 is drawn open with its lens"
    );
    await expect
      .poll(() => onScreen(nodeRow(page, treeId(ids.statement8)).first()), {
        message: `${address} does not bring statement 8's row on screen`,
        timeout: DRAWN_MS,
      })
      .toBe(true);
  }
}

/**
 * Step (1) in the outline: statement 9, live with no check, carries "No check" and no mark,
 * read before statement 7, which carries "Proposed" and its shape and no gap; check 2 and the
 * first decision carry "Proposed", which the outline draws and plan task 1 it does not; the chip
 * naming statement 7 on the decision that shapes it carries it in its compact form; each keeps
 * its word and its shape with forced colours; and no text is a raw value of the tree read.
 */
async function expectProposedMarks(page: Page, read: ProposedPage): Promise<void> {
  const { answer, plain, seeded, traffic } = read;
  const { ids } = seeded;
  // The page as it opens: this step presses each branch open as a reader does.
  await gotoSettled(page, traffic, plain);
  const statement9 = await drawnRow(page, ids.statement9, "statement 9");
  expect(await ownMarks(statement9), "statement 9 carries no Proposed mark").toEqual([]);
  await expect(
    statement9.locator('[data-gap][data-gap-kind="no_check"]'),
    "statement 9, live with no check, does not carry No check"
  ).toContainText(GAP_WORDS.no_check);
  const statement7 = await drawnRow(page, ids.statement7, "statement 7");
  await expect
    .poll(() => ownMarks(statement7), { message: "statement 7 carries Proposed" })
    .toEqual([PROPOSED]);
  await expect(rowGaps(statement7), "statement 7, which is proposed, draws a gap").toHaveCount(0);
  const proposed = [
    { readId: ids.check2, what: "check 2" },
    { readId: ids.decision1, what: "the first decision" },
  ];
  const rows = [statement7];
  for (const { readId, what } of proposed) {
    const row = await drawnRow(page, readId, what);
    await expect
      .poll(() => ownMarks(row), { message: `${what} does not carry Proposed in the outline` })
      .toEqual([PROPOSED]);
    rows.push(row);
  }
  const shaping = await drawnRow(
    page,
    ids.shapingDecision,
    "the agent's decision that shapes statement 7"
  );
  const chip = chipNaming(page, shaping, wordsOf(answer, ids.statement7));
  await expect(chip, "the decision that shapes statement 7 draws no chip naming it").toHaveCount(1);
  await expect
    .poll(() => compactMarks(chip), {
      message: "the chip naming statement 7 does not carry Proposed in its compact form",
    })
    .toEqual([PROPOSED]);

  await withForcedColours(page, async () => {
    for (const row of rows) {
      await expect
        .poll(() => ownMarks(row), {
          message: "a proposed row's mark loses its word or its shape with forced colours",
        })
        .toEqual([PROPOSED]);
    }
    await expect
      .poll(() => compactMarks(chip), {
        message: "the chip's compact mark loses its word or its shape with forced colours",
      })
      .toEqual([PROPOSED]);
  });
  await expectNoRawValue(page, "in the outline");
}

/**
 * Details' facts of statement 7 carry "Proposed" and those of statement 8 "Overtaken", read only
 * where Details draws the focused row's facts; noted as owed otherwise.
 */
async function expectDetails(page: Page, { seeded }: ProposedPage): Promise<void> {
  if (!noteOwed(FOCUSED_FACTS_GATE)) return;
  const { ids } = seeded;
  for (const [readId, what, mark] of [
    [ids.statement7, "statement 7", PROPOSED],
    [ids.statement8, "statement 8", OVERTAKEN],
  ] as const) {
    await focusRow(page, await drawnRowId(page, readId, what));
    await openDetails(page);
    await expect
      .poll(() => compactMarks(focusedNodeFacts(page)), {
        message: `Details' facts of ${what} do not carry ${mark.word}`,
      })
      .toEqual([mark]);
  }
}

/** Every branch the page draws open, folded one at a time (`foldEachBranch`), counts what the
 *  tree read holds under it that Conversation draws, an overtaken node and what stands under it
 *  counted as every node is, and opens again. A question indexed from a block stands for the ask
 *  it was indexed into as well, a comment on a node's text and its thread count in no branch
 *  (`margin`), and a criterion is counted only where the page draws one (`criteria`). The page
 *  draws every branch open on load (part T's R11), so no row draws a count until a fold draws
 *  it. */
async function expectFoldedCounts(
  page: Page,
  { answer, key, margin, seeded }: ProposedPage,
  criteria: Gate
): Promise<void> {
  const asks = new Map([
    [seeded.ids.openQuestion, [seeded.whole.openQuestion.askId]],
    [seeded.ids.overtakenQuestion, [seeded.overtakenAskId]],
    [seeded.ids.proposedQuestion, [seeded.proposedAskId]],
  ]);
  const undrawn = noteOwed(criteria) ? undefined : UNDRAWN_KINDS;
  await foldEachBranch(page, answer, key, READER, {
    held: (readId) => ({ alike: asks.get(readId), margin, undrawn }),
    including: "an overtaken node and what stands under it included",
  });
}

/**
 * Statement 8 stands among its parent's other statements in the order the tree read gives them,
 * which is the place the read puts it.
 */
async function expectAtItsPlace(page: Page, read: ProposedPage): Promise<void> {
  const { answer, key, seeded } = read;
  const { ids } = seeded;
  const parent = answer.nodes?.find((node) => node.id === ids.statement8)?.parent?.id ?? null;
  const statements = (answer.nodes ?? [])
    .filter(
      (node) =>
        (node.parent?.id ?? null) === parent &&
        node.supertag === "requirement" &&
        (node.issue === undefined || node.issue === key)
    )
    .map((node) => node.id);
  const drawn = (await treeItemsDrawn(page)).flatMap((item) =>
    item.readId !== null && statements.includes(item.readId) ? [item.readId] : []
  );
  expect(drawn, "statement 8 is no row among its parent's children").toContain(ids.statement8);
  expect(drawn, "statement 8 does not stand at its place among its parent's statements").toEqual(
    statements.filter((id) => drawn.includes(id))
  );
}

/** `j` and `k` reach statement 8's row and leave it as they do any row, whatever the page draws
 *  next to it - the lens and the plan task that serves it stand under it: from the row above it
 *  `j` reaches it and the next `j` goes to the row under it, and `k` comes back the same way. */
async function expectKeysPassStatement8(page: Page, { seeded }: ProposedPage): Promise<void> {
  const { ids } = seeded;
  const items = await treeItemsDrawn(page);
  const at = items.findIndex((item) => item.readId === ids.statement8);
  const above = items[at - 1];
  const under = items[at + 1];
  expect(above?.readId ?? null, "no row stands above statement 8").not.toBeNull();
  expect(
    under?.readId ?? null,
    "no row stands under statement 8, which is drawn open"
  ).not.toBeNull();
  const start = nodeRow(page, above?.id ?? "").first();
  await start.focus();
  await expect(start).toBeFocused();
  await page.keyboard.press("j");
  await expect
    .poll(() => focusedRowReadId(page), { message: "j does not reach statement 8's row" })
    .toBe(ids.statement8);
  await page.keyboard.press("j");
  await expect
    .poll(() => focusedRowReadId(page), { message: "j does not leave statement 8's row" })
    .toBe(under?.readId ?? null);
  await page.keyboard.press("k");
  await expect
    .poll(() => focusedRowReadId(page), { message: "k does not reach statement 8's row" })
    .toBe(ids.statement8);
  await page.keyboard.press("k");
  await expect
    .poll(() => focusedRowReadId(page), { message: "k does not leave statement 8's row" })
    .toBe(above?.readId ?? null);
}

/** Statement 8's fold control folds it with its lens, counting what it holds, and opens it again
 *  with its lens. */
async function expectStatement8Folds(page: Page, read: ProposedPage): Promise<void> {
  const { answer, key, margin, seeded, treeId } = read;
  const { ids } = seeded;
  const row = nodeRow(page, treeId(ids.statement8)).first();
  await row.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
  await expect(row, "statement 8's fold control does not fold it").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  await expect(
    row,
    "statement 8's fold does not count its lens with the rest of its branch"
  ).toHaveAttribute(
    "data-folded-count",
    String(heldUnder(answer, key, ids.statement8, { margin }))
  );
  expect(
    (await treeItemsDrawn(page)).some((item) => item.readId === ids.lens),
    "the lens on statement 8 is drawn while statement 8 is folded"
  ).toBe(false);
  await row.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
  await expectDrawnOpen(
    page,
    ids.statement8,
    [ids.lens],
    "statement 8's fold control does not open it again with its lens"
  );
}

/**
 * The overtaken decision is a row carrying "Overtaken" in the outline, and the chip naming
 * statement 8 on the decision that shapes it carries it in its compact form; each keeps its word
 * and its shape with forced colours.
 */
async function expectOvertakenMarks(page: Page, read: ProposedPage): Promise<void> {
  const { answer, seeded } = read;
  const { ids } = seeded;
  const statement8 = await drawnRow(page, ids.statement8, "statement 8");
  const decision = await drawnRow(page, ids.waitingDecision, "the overtaken decision");
  await expect
    .poll(() => ownMarks(decision), {
      message: "the overtaken decision does not carry Overtaken in the outline",
    })
    .toEqual([OVERTAKEN]);
  const shaping = await drawnRow(
    page,
    ids.shapingDecision8,
    "the agent's decision that shapes statement 8"
  );
  const chip = chipNaming(page, shaping, wordsOf(answer, ids.statement8));
  await expect(chip, "the decision that shapes statement 8 draws no chip naming it").toHaveCount(1);
  await expect
    .poll(() => compactMarks(chip), {
      message: "the chip naming statement 8 does not carry Overtaken in its compact form",
    })
    .toEqual([OVERTAKEN]);
  await withForcedColours(page, async () => {
    for (const row of [statement8, decision]) {
      await expect
        .poll(() => ownMarks(row), {
          message: "an overtaken row's mark loses its word or its shape with forced colours",
        })
        .toEqual([OVERTAKEN]);
    }
    await expect
      .poll(() => compactMarks(chip), {
        message: "the chip's compact Overtaken loses its word or its shape with forced colours",
      })
      .toEqual([OVERTAKEN]);
  });
}

/**
 * Step (2) in the outline: statement 8 is a row at its place among its parent's children,
 * carrying "Overtaken" and no gap, open with its lens under it; every folded branch's count
 * counts it; no row reads "1 overtaken"; `j` and `k` reach and leave its row; its fold control
 * folds it with its lens and opens it again; the overtaken decision and the chip naming statement
 * 8 carry the mark; and statement 8 stays open with its lens after a reload.
 */
async function expectOvertakenRows(page: Page, read: ProposedPage): Promise<void> {
  const { plain, seeded, traffic } = read;
  const { ids } = seeded;
  // The load draws the outline once its tree read answers, after the page itself: wait for the
  // load to settle, as each other step does, and read the rows only once statement 8's is drawn,
  // which opens every branch on the way (`drawnRowId`). Read before the tree read answers, the
  // outline holds no rows; opened before the load settles, a row takes back the focus the opener
  // leaves for the page.
  await gotoSettled(page, traffic, plain);
  // The criterion gate opens every branch, and the counts are read on the page as it loads: after
  // it the page is loaded again. It stands here and not with the gates of the first load, which
  // is read before any press (`expectOpenOnLoad`).
  const criteria = await criterionGate(page, read);
  await gotoSettled(page, traffic, plain);
  await expectFoldedCounts(page, read, criteria);
  const statement8 = await drawnRow(page, ids.statement8, "statement 8");
  await expect
    .poll(() => ownMarks(statement8), { message: "statement 8 does not carry Overtaken" })
    .toEqual([OVERTAKEN]);
  await expect(rowGaps(statement8), "statement 8, which is overtaken, draws a gap").toHaveCount(0);
  await expectDrawnOpen(
    page,
    ids.statement8,
    [ids.lens],
    "statement 8 is drawn open with its lens"
  );
  await expectAtItsPlace(page, read);
  await expectNoOvertakenCount(page, "in the outline");
  await expectKeysPassStatement8(page, read);
  await expectStatement8Folds(page, read);
  await expectOvertakenMarks(page, read);
  await expectNoRawValue(page, "in the outline");

  // Statement 8 stays open after a reload, as every node is drawn open on load.
  await page.reload();
  await issueRead(page);
  await expectDrawnOpen(
    page,
    ids.statement8,
    [ids.lens],
    "statement 8 does not stay open with its lens after a reload"
  );
}

/** Each Graph reading of row 1, made only where the page draws a Graph control: check 2, the first
 *  decision and plan task 1 carry "Proposed"; statement 8, plan task 2 and the overtaken decision
 *  carry "Overtaken"; statement 8 draws no gap and is open with its lens. */
async function expectProposedInGraph(page: Page, read: ProposedPage): Promise<void> {
  if (!noteOwed(read.graph)) return;
  const { ids } = read.seeded;
  const name = graphName();
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  for (const [readId, what, mark] of [
    [ids.check2, "check 2", PROPOSED],
    [ids.decision1, "the first decision", PROPOSED],
    [ids.planTask1, "plan task 1", PROPOSED],
    [ids.statement8, "statement 8", OVERTAKEN],
    [ids.planTask2, "plan task 2", OVERTAKEN],
    [ids.waitingDecision, "the overtaken decision", OVERTAKEN],
  ] as const) {
    await expectMarkedInGraph(page, readId, mark, what);
  }
  const statement8 = await drawnRow(page, ids.statement8, "statement 8 in Graph");
  await expect(rowGaps(statement8), "statement 8 draws a gap in Graph").toHaveCount(0);
  await expectDrawnOpen(
    page,
    ids.statement8,
    [ids.lens],
    "statement 8 is drawn open with its lens in Graph"
  );
  await pickPreset(page, PRESET_NAMES.conversation);
}

/**
 * Step (3), with the Decisions view on: the overtaken decision stands with the decisions that
 * wait for review, read first, so a page that leaves it out of the view fails on that and not on
 * the address; loading its address keeps that view on, with the focus on the decision's row,
 * which carries "Overtaken". Nothing waits on the reader at or under an overtaken node, so the row
 * draws no Keep and no Change and not the waiting colour, as the outline's row of the same node
 * draws neither; they are read once the row is drawn whole, focused and marked, so a page that
 * draws them draws them by then. The to-review flag and the header's count of what waits do not
 * count it, and the header's presses never reach it (`expectWaitsLeaveOut`).
 */
async function expectOvertakenDecisionAddress(page: Page, read: ProposedPage): Promise<void> {
  const { key, seeded, treeId } = read;
  const { ids } = seeded;
  const decisions = presetPath(key, "decisions");
  const decisionBlock = blockNamed(seeded.addresses.waitingDecision, "the overtaken decision");
  await gotoIssue(page, `${decisions}#b-${encodeURIComponent(decisionBlock)}`);
  await expect
    .poll(() => decisionsPartRows(page, "review"), {
      message: "the overtaken decision stands with the decisions that wait for review",
      timeout: DRAWN_MS,
    })
    .toContain(treeId(ids.waitingDecision));
  const decision = nodeRow(page, treeId(ids.waitingDecision)).first();
  await expect
    .poll(
      async () => ({
        focused: await focusedNodeId(page),
        onScreen: await onScreen(decision),
        path: new URL(page.url()).pathname,
        query: new URL(page.url()).searchParams.get("q"),
        view: await activePreset(page),
      }),
      {
        message:
          "the overtaken decision's address does not keep the Decisions view on, with the focus on its row",
        timeout: DRAWN_MS,
      }
    )
    .toEqual({
      focused: treeId(ids.waitingDecision),
      onScreen: true,
      path: decisions,
      query: null,
      view: PRESET_NAMES.decisions,
    });
  await expect
    .poll(() => ownMarks(decision), {
      message: "the overtaken decision does not carry Overtaken under Decisions",
    })
    .toEqual([OVERTAKEN]);
  await expect(
    decision.locator(KEEP_OR_CHANGE),
    "the overtaken decision draws no Keep or Change"
  ).toHaveCount(0);
  await expect
    .poll(() => drawnWaiting(decision), {
      message: "the overtaken decision draws its state in the waiting colour under Decisions",
    })
    .toBe(false);
  await expectWaitsLeaveOut(page, read);
}

/**
 * Steps (1) and (2) in each view: check 2 carries "Proposed" under Verification, with no query
 * and with its result's words; no failing flag and no summary count counts a check or a statement
 * that is proposed or overtaken; the first decision carries "Proposed" under Decisions, where the
 * overtaken decision stands with the decisions that wait for review and the to-review flag leaves
 * it out; plan task 1 carries "Proposed" under Plan, which draws plan task 2 as a row after every
 * step, and the steps draw the line naming statement 7 on plan task 7's row in its compact form;
 * statement 7 carries "Proposed" and statement 8 "Overtaken", with no gap, under Spec, whose
 * control counts every statement. No view draws a raw value of the tree read.
 */
async function expectProposedViews(page: Page, read: ProposedPage): Promise<void> {
  const { answer, key, seeded, treeId } = read;
  const { ids } = seeded;
  const statements = statementsOf(seeded);

  await gotoIssue(page, presetPath(key, "verification"));
  const check2 = verificationCheck(page, treeId(ids.check2));
  await expect(check2, "the Verification view draws no row for check 2").toHaveCount(1);
  await expect
    .poll(() => ownMarks(check2), { message: "check 2 does not carry Proposed under Verification" })
    .toEqual([PROPOSED]);
  await expect
    .poll(() => flagCount(page, "failing"), {
      message: "the failing flag counts check 4, which is overtaken, or check 2, which is proposed",
    })
    .toBe(0);
  expect(
    (await checkLegendCounts(page)).failed,
    "the summary counts check 4, which is overtaken, or check 2, which is proposed, as failed"
  ).toBe(0);
  await expect
    .poll(async () => (await coveredCount(page))?.of, {
      message:
        "the Verification summary counts statement 7, which is proposed, statement 8, which is overtaken, or the statement under the overtaken question",
    })
    .toBe(statements - UNCOUNTED_BEFORE_SETTLE);
  await expectNoRawValue(page, "under Verification");
  await expectNoOvertakenCount(page, "under Verification");
  const result = answer.nodes?.find(
    (node) => node.supertag === "result" && node.parent?.id === ids.check2
  );
  expect(result?.text ?? "", "the tree read holds no words of check 2's result").not.toBe("");
  // With a query, the view draws a result it keeps in its check's own row, not under a line
  // naming the check (decision 391), so check 2's row carries Proposed there too.
  await gotoIssue(
    page,
    `${presetPath(key, "verification")}?q=${encodeURIComponent(result?.text ?? "")}`
  );
  const queried = verificationCheck(page, treeId(ids.check2));
  await expect(
    queried,
    "the Verification view with check 2's result words draws no check 2"
  ).toHaveCount(1);
  await expect
    .poll(() => ownMarks(queried), {
      message: "check 2 does not carry Proposed under Verification with its result's words",
    })
    .toEqual([PROPOSED]);

  await gotoIssue(page, presetPath(key, "decisions"));
  const decision1 = nodeRow(page, treeId(ids.decision1)).first();
  await expect(decision1, "the Decisions view draws no row for the first decision").toHaveCount(1);
  await expect
    .poll(() => ownMarks(decision1), {
      message: "the first decision does not carry Proposed under Decisions",
    })
    .toEqual([PROPOSED]);
  const overtaken = treeId(ids.waitingDecision);
  await expect
    .poll(() => decisionsPartRows(page, "review"), {
      message: "the overtaken decision stands with the decisions that wait for review",
      timeout: DRAWN_MS,
    })
    .toContain(overtaken);
  await expect
    .poll(() => ownMarks(nodeRow(page, overtaken).first()), {
      message: "the overtaken decision does not carry Overtaken under Decisions",
    })
    .toEqual([OVERTAKEN]);
  // The poll above waited for the review part to draw, so one read of its rows is its whole list.
  const review = (await decisionsPartRows(page, "review")).filter((id) => id !== overtaken).length;
  await expect
    .poll(() => flagCount(page, "to_review"), {
      message: "the to-review flag counts the overtaken decision",
    })
    .toBe(review);
  await expectNoRawValue(page, "under Decisions");
  await expectNoOvertakenCount(page, "under Decisions");

  await gotoIssue(page, presetPath(key, "plan"));
  const planTask1 = planChart(page)
    .locator(`[data-node-id="${treeId(ids.planTask1)}"]`)
    .first();
  await expect(planTask1, "the Plan view draws no step for plan task 1").toBeVisible();
  await expect
    .poll(() => ownMarks(planTask1), { message: "plan task 1 does not carry Proposed under Plan" })
    .toEqual([PROPOSED]);
  expect(
    (await planChartRows(page)).map((row) => row.id),
    "plan task 2 is a step of the Plan view"
  ).not.toContain(treeId(ids.planTask2));
  const planTask2 = planChart(page).locator(
    `[data-plan-overtaken-row][data-node-id="${treeId(ids.planTask2)}"]`
  );
  await expect(planTask2, "the Plan view draws no row for plan task 2").toHaveCount(1);
  await expect
    .poll(() => ownMarks(planTask2), {
      message: "plan task 2 does not carry Overtaken under Plan",
    })
    .toEqual([OVERTAKEN]);
  expect(
    await planChart(page).evaluate(
      (chart) =>
        [
          ...chart.querySelectorAll(
            "[data-plan-bar-row], [data-plan-cycle-row], [data-plan-overtaken-row]"
          ),
        ]
          .at(-1)
          ?.getAttribute("data-node-id") ?? null
    ),
    "the Plan view's row for plan task 2 does not stand after every step"
  ).toBe(treeId(ids.planTask2));
  // The steps draw plan task 7's row with the line naming statement 7, which it serves and
  // which is proposed (decision 391).
  await planView(page, "steps").click();
  await expect(
    planView(page, "steps"),
    "the Plan view does not switch to its steps"
  ).toHaveAttribute("aria-pressed", "true");
  const statement7Line = issuePage(page)
    .locator(`[data-node-parent-line][data-parent-id="${treeId(ids.statement7)}"]`)
    .first();
  await expect(
    statement7Line,
    "under Plan's steps, plan task 7 draws no line naming statement 7"
  ).toBeVisible();
  await expect
    .poll(() => compactMarks(statement7Line), {
      message: "the line naming statement 7 does not carry Proposed in its compact form",
    })
    .toEqual([PROPOSED]);
  await expectNoRawValue(page, "under Plan");
  await expectNoOvertakenCount(page, "under Plan");

  await gotoIssue(page, presetPath(key, "spec"));
  const statement7 = nodeRow(page, treeId(ids.statement7)).first();
  await expect(statement7, "the Spec view draws no row for statement 7").toHaveCount(1);
  await expect
    .poll(() => ownMarks(statement7), { message: "statement 7 does not carry Proposed under Spec" })
    .toEqual([PROPOSED]);
  const statement8 = nodeRow(page, treeId(ids.statement8)).first();
  await expect(statement8, "the Spec view draws no row for statement 8").toHaveCount(1);
  await expect
    .poll(() => ownMarks(statement8), {
      message: "statement 8 does not carry Overtaken under Spec",
    })
    .toEqual([OVERTAKEN]);
  await expect(
    rowGaps(statement8),
    "statement 8, which is overtaken, draws a gap under Spec"
  ).toHaveCount(0);
  await expect
    .poll(() => filterCount(page, "spec"), {
      message:
        "the Spec control does not count every statement, statement 8 and the one under the overtaken question included",
    })
    .toBe(statements);
  await expectNoRawValue(page, "under Spec");
  await expectNoOvertakenCount(page, "under Spec");
}

/**
 * Steps (4) and (5): no request of the node state routes before the press; on statement 7, `f`
 * reaches Settle and Enter sends one settle naming its address, which says "Settling…" while it
 * is out, and then statement 7 carries no mark and "No check", and the Verification summary
 * counts it; under Spec its mark is gone, with no reload and after one.
 */
async function expectSettled(page: Page, read: ProposedPage): Promise<void> {
  const { plain, seeded, traffic, treeId } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, plain);
  const { row, settle } = await reachSettle(
    page,
    ids.statement7,
    "statement 7",
    "the focused row offers Settle"
  );
  await expect(settle, "the focused row's Settle is not named Settle").toHaveText(
    CONTROL_LABELS.settle
  );
  expect(
    nodeStateRequests(traffic.requests),
    "the page sends a request of the node state routes before the press"
  ).toEqual([]);
  traffic.reset();

  const sent = Promise.withResolvers<void>();
  const hold = async (route: Route) => {
    await sent.promise;
    await route.continue();
  };
  await page.route(SETTLE_ROUTE, hold);
  try {
    await page.keyboard.press("Enter");
    await expect(settle, "Settle does not say it is settling while the write is out").toHaveText(
      SETTLE_ANSWERS.settling
    );
  } finally {
    sent.resolve();
  }
  await expect
    .poll(() => ownMarks(row), { message: "statement 7 keeps its mark once it is settled" })
    .toEqual([]);
  await page.unroute(SETTLE_ROUTE, hold);
  const settles = settlesSent(traffic.requests);
  expect(settles.length, "Enter on Settle does not send one settle").toBe(1);
  const statement7Block = blockNamed(seeded.addresses.statement7, "statement 7");
  expect(blockOf(settles[0] ?? ""), "the settle does not name statement 7's address").toBe(
    statement7Block
  );
  await expect(
    row.locator('[data-gap][data-gap-kind="no_check"]'),
    "statement 7, settled with no check, does not carry No check"
  ).toContainText(GAP_WORDS.no_check);

  await pickPreset(page, PRESET_NAMES.verification);
  await expectActivePreset(page, PRESET_NAMES.verification);
  await expect
    .poll(async () => (await coveredCount(page))?.of, {
      message: "the Verification summary does not count statement 7 once it is settled",
    })
    .toBe(statementsOf(seeded) - (UNCOUNTED_BEFORE_SETTLE - 1));

  await pickPreset(page, PRESET_NAMES.spec);
  await expectActivePreset(page, PRESET_NAMES.spec);
  const specRow = nodeRow(page, treeId(ids.statement7)).first();
  await expect(specRow, "the Spec view draws no row for statement 7").toHaveCount(1);
  await expect
    .poll(() => ownMarks(specRow), {
      message: "under Spec, statement 7 keeps its mark with no reload",
    })
    .toEqual([]);
  await page.reload();
  await issueRead(page);
  await expect(specRow, "the Spec view draws no row for statement 7 after a reload").toHaveCount(1);
  expect(await ownMarks(specRow), "under Spec, statement 7 keeps its mark after a reload").toEqual(
    []
  );
}

// SLOW(>=2s): a whole seeded issue with ten node states, two questions with the two nodes under one
// and a lens written, the outline's branches opened a dozen times, twelve loads of the outline and
// the views, the header's count pressed past each thing that waits, one settle and two reloads.
test("a proposed node is marked and settled on the page, and an overtaken one is drawn open and marked @phone", async ({
  browser,
}) => {
  test.setTimeout(480_000);
  const size = rowSize();
  await phase(browser, seedGateIssue, async (page, gateIssue) => {
    owedUnless(await treeReadGate(page, gateIssue));
    owedUnless(await convertedGate(page, gateIssue));
    const whole = await seedWholeIssue();
    const seeded = await seedNodeStates(whole);
    await page.setViewportSize(size);
    const loaded = await loadedPage(page, seeded);
    // (5) The load sends no request of the node state routes: a node's life is the tree read's.
    expect(
      nodeStateRequests(loaded.traffic.requests),
      "the load sends a request of the node state routes"
    ).toEqual([]);
    // The gate that reads the page as it opened, and the reading of statement 8 on it, before any
    // press.
    const graph = await graphGate(page);
    const read: ProposedPage = { ...loaded, graph };
    await expectOpenOnLoad(page, read);
    await expectOvertakenOpen(page, read);
    await expectProposedMarks(page, read);
    await expectDetails(page, read);
    await expectOvertakenRows(page, read);
    await expectProposedInGraph(page, read);
    await expectOvertakenDecisionAddress(page, read);
    await expectProposedViews(page, read);
    await expectSettled(page, read);
  });
});

// ---------------------------------------------------------------------------------------------
// Row 2: a refused settle.
// ---------------------------------------------------------------------------------------------

/** The refusals the page's routes answer the later presses with, and what each says: one the
 *  page names, and a 4xx it names nothing for, which the write does not try again. */
const REFUSALS = [
  {
    body: { code: "SETTLE_NOT_PERMITTED", error: "this credential may not settle this node" },
    status: 403,
    words: SETTLE_ANSWERS.not_permitted,
  },
  {
    body: { code: "INVALID_JSON", error: "the body is not JSON" },
    status: 400,
    words: SETTLE_ANSWERS.failed,
  },
] as const;

// SLOW(>=2s): a whole seeded issue with ten node states, two questions with the two nodes under one
// and a lens written, the outline's load waited out, and three settles each refused and read.
test("a refused settle says why and leaves the row as it was", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(browser, seedGateIssue, async (page, gateIssue) => {
    owedUnless(await treeReadGate(page, gateIssue));
    owedUnless(await convertedGate(page, gateIssue));
    const whole = await seedWholeIssue();
    const key = whole.key;
    const seeded = await seedNodeStates(whole);
    await page.setViewportSize(DESKTOP);
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(key, "conversation"));
    await heldTreeAnswer(tree, "the load", (held) =>
      (held.nodes ?? []).some(
        (node) => node.id === seeded.ids.statement7 && node.state === "proposed"
      )
    );
    await expectLoadSettled(traffic);
    const { row, settle } = await reachSettle(
      page,
      seeded.ids.statement7,
      "statement 7",
      "the focused row offers Settle"
    );
    const answer = row.locator("[data-settle-answer]");

    // Bob settles it first, through the server; alice's page still draws it proposed.
    await settleNodeApi(key, seeded.addresses.statement7, "bob");
    traffic.reset();
    await page.keyboard.press("Enter");
    await expect(answer, "a settle of a node no longer proposed does not say so").toHaveText(
      SETTLE_ANSWERS.not_proposed
    );
    await expect(answer, "the refusal's sentence is no status").toHaveAttribute("role", "status");
    await expect
      .poll(() => ownMarks(row), { message: "the refused settle takes statement 7's mark" })
      .toEqual([PROPOSED]);
    await expectLoadSettled(traffic);
    expect(
      treeReads(traffic.requests, key, null) +
        traffic.requests.filter(
          (request) => request.method === "GET" && request.path.includes("/coverage")
        ).length,
      "the refused settle reads the tree or the coverage again"
    ).toBe(0);

    for (const refusal of REFUSALS) {
      const refuse = (route: Route) =>
        route.fulfill({ json: refusal.body, status: refusal.status });
      await page.route(SETTLE_ROUTE, refuse);
      try {
        await pressFTo(page, settle, "the focused row offers Settle again");
        await page.keyboard.press("Enter");
        await expect(
          answer,
          `a ${refusal.status} ${refusal.body.code} does not say ${refusal.words}`
        ).toHaveText(refusal.words);
        await expect
          .poll(() => ownMarks(row), {
            message: `the ${refusal.status} ${refusal.body.code} takes statement 7's mark`,
          })
          .toEqual([PROPOSED]);
      } finally {
        await page.unroute(SETTLE_ROUTE, refuse);
      }
    }
  });
});

// ---------------------------------------------------------------------------------------------
// Row 4: an overtaken question stays a Contents entry and counts in no waiting count.
// ---------------------------------------------------------------------------------------------

/**
 * The nodes the page draws under the overtaken question, and how a reading names them: its
 * statement, and its criterion where the page draws one (`criteria`), which is noted as owed
 * where it draws none.
 */
function drawnUnderQuestion({ criteria, seeded }: SeededPage): {
  readonly named: string;
  readonly under: readonly string[];
} {
  const { ids } = seeded;
  return noteOwed(criteria)
    ? {
        named: "its criterion and its statement",
        under: [ids.questionCriterion, ids.questionStatement],
      }
    : { named: "its statement", under: [ids.questionStatement] };
}

/**
 * Row 4's first reading, on the page's first plain load, before any press, unfold or reload: the
 * overtaken question is drawn open with its statement under it, as its live self is and as every
 * node is on load (part T's R11), at every width, and the reading is never owed. Answers the read
 * id of each row drawn right under the question on this load, among which its criterion is read
 * once the criterion gate, which opens every branch, finds the page drawing one.
 */
async function expectQuestionOpenOnLoad(
  page: Page,
  { seeded }: LoadedPage
): Promise<readonly (string | null)[]> {
  const { ids } = seeded;
  return expectDrawnOpen(
    page,
    ids.overtakenQuestion,
    [ids.questionStatement],
    "the overtaken question is drawn open with its statement on the first load"
  );
}

/**
 * The overtaken question carries "Overtaken" in the outline, drawn open with its statement under
 * it, and its criterion where the page draws one, and the statement draws no gap.
 */
async function expectOvertakenQuestionRows(page: Page, read: SeededPage): Promise<void> {
  const { plain, seeded, traffic, treeId } = read;
  const { ids } = seeded;
  const { named, under } = drawnUnderQuestion(read);
  await gotoSettled(page, traffic, plain);
  const question = await drawnRow(page, ids.overtakenQuestion, "the overtaken question");
  await expect
    .poll(() => ownMarks(question), { message: "the overtaken question carries Overtaken" })
    .toEqual([OVERTAKEN]);
  await expectDrawnOpen(
    page,
    ids.overtakenQuestion,
    under,
    `the overtaken question is drawn open with ${named}`
  );
  const statement = nodeRow(page, treeId(ids.questionStatement)).first();
  await expect(statement, "the statement under the overtaken question is no row").toHaveCount(1);
  await expect(
    rowGaps(statement),
    "the statement under the overtaken question draws a gap"
  ).toHaveCount(0);
  await expectNoRawValue(page, "in the outline");
}

/**
 * The overtaken question is an entry of the Contents list, at its place in the tree's order, and
 * its own entry carries no count. It stands under the issue, so no entry stands above it to count
 * the statement under it.
 */
async function expectQuestionInContents(page: Page, read: SeededPage): Promise<void> {
  const { plain, seeded, traffic, treeId } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, plain);
  const pane = contentsPane(page);
  const entry = pane.locator(`[data-contents-entry="${treeId(ids.overtakenQuestion)}"]`);
  const open = pane.locator(`[data-contents-entry="${treeId(ids.openQuestion)}"]`);
  await expect(entry, "the overtaken question is a Contents entry").toHaveCount(1);
  await expect(open, "the open question is a Contents entry").toHaveCount(1);
  await expect(
    entry.locator("[data-contents-mark]"),
    "the overtaken question's own Contents entry carries a count"
  ).toHaveCount(0);
  await expect(
    entry,
    "an entry stands above the overtaken question, which stands under the issue"
  ).toHaveAttribute("aria-level", "1");
  const both = [treeId(ids.openQuestion), treeId(ids.overtakenQuestion)];
  await drawnRowId(page, ids.overtakenQuestion, "the overtaken question");
  const outline = (await treeItemsDrawn(page)).map((item) => item.id);
  const listed = await contentsEntries(pane).evaluateAll((rows) =>
    rows.map((row) => row.getAttribute("data-contents-entry") ?? "")
  );
  expect(
    listed.filter((id) => both.includes(id)),
    "the Contents list does not hold the questions in the tree's order"
  ).toEqual(outline.filter((id) => both.includes(id)));
}

/** With the Decisions view on, the overtaken question stands with the open questions, marked
 *  "Overtaken". */
async function expectQuestionInDecisions(page: Page, read: SeededPage): Promise<void> {
  const { key, seeded, treeId } = read;
  await gotoIssue(page, presetPath(key, "decisions"));
  await expectWithOpenQuestions(
    page,
    treeId(seeded.ids.overtakenQuestion),
    OVERTAKEN,
    "the overtaken question"
  );
}

/**
 * Answers the spec's own open question, which stands right before the overtaken one: the
 * overtaken question is then the first in the tree's order that would wait on alice were it live,
 * so a page that let it keep its wait would open on it (`expectOpensOnLiveQuestion`). The readings
 * after this write hold with the spec's own question answered: the header's count reads what
 * waits from the issue read as it stands (`expectWaitsLeaveOut`), and the Graph readings read the
 * overtaken question alone.
 */
async function answerSpecQuestion(read: SeededPage): Promise<void> {
  await answerAsk(read.seeded.whole.openQuestion.askId, {
    expected_edited_at: null,
    selected: [],
    text: "1280 px.",
  });
}

/**
 * Conversation opens with the focus on the first question in the tree's order that waits on
 * alice, and not on the overtaken question nor on a row under it. With the spec's own open
 * question answered (`answerSpecQuestion`), the overtaken question is the first in the tree's
 * order that would wait on alice were it live. The proposed question, which stands right after it
 * and still waits on alice (`seedNodeStates`), is where the page opens.
 */
async function expectOpensOnLiveQuestion(page: Page, read: SeededPage): Promise<void> {
  const { plain, seeded, traffic } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, plain);
  await expectOpeningLanded(page);
  await expect
    .poll(() => focusedRowReadId(page), {
      message:
        "Conversation opens with the focus on a row other than the open question that waits on alice",
    })
    .toBe(ids.proposedQuestion);
}

/** The overtaken question in Graph, with its statement open under it, and its criterion where the
 *  page draws one, and no gap on the statement. */
async function expectQuestionInGraph(page: Page, read: SeededPage): Promise<void> {
  if (!noteOwed(await graphGate(page))) return;
  const { ids } = read.seeded;
  const { named, under } = drawnUnderQuestion(read);
  const name = graphName();
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  await expectMarkedInGraph(page, ids.overtakenQuestion, OVERTAKEN, "the overtaken question");
  await expectDrawnOpen(
    page,
    ids.overtakenQuestion,
    under,
    `the overtaken question is drawn open with ${named} in Graph`
  );
  const statement = await drawnRow(page, ids.questionStatement, "the statement under the question");
  await expect(
    rowGaps(statement),
    "the statement under the question draws a gap in Graph"
  ).toHaveCount(0);
  await pickPreset(page, PRESET_NAMES.conversation);
}

// SLOW(>=2s): a whole seeded issue with ten node states, two questions with the two nodes under one
// and a lens written, five loads of the outline, its Contents list and the Decisions view, the
// spec's own question answered, and the header's count pressed past each thing that waits.
test("an overtaken question stays a Contents entry and counts in no waiting count", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedGateIssue, async (page, gateIssue) => {
    owedUnless(await treeReadGate(page, gateIssue));
    owedUnless(await convertedGate(page, gateIssue));
    const whole = await seedWholeIssue();
    const seeded = await seedNodeStates(whole);
    await page.setViewportSize(DESKTOP);
    const loaded = await loadedPage(page, seeded);
    // The first reading is made on the first load, before the criterion gate opens every branch,
    // and its criterion is read among the rows that load drew once the gate is known.
    const underOnLoad = await expectQuestionOpenOnLoad(page, loaded);
    const read = await seededPage(page, loaded);
    if (noteOwed(read.criteria)) {
      expect(
        underOnLoad,
        "the overtaken question is drawn open with its criterion on the first load"
      ).toContain(seeded.ids.questionCriterion);
    }
    await expectOvertakenQuestionRows(page, read);
    await expectQuestionInContents(page, read);
    await expectQuestionInDecisions(page, read);
    await answerSpecQuestion(read);
    // The first focus is read before the header's count: a closed question that kept its wait
    // would be reached by the count's presses too, and the first focus is the reading of that wait.
    await expectOpensOnLiveQuestion(page, read);
    await expectWaitsLeaveOut(page, read);
    await expectQuestionInGraph(page, read);
  });
});

// ---------------------------------------------------------------------------------------------
// Row 5: each way to a node reaches an overtaken one.
// ---------------------------------------------------------------------------------------------

/** The words filter with the one word only statement 8 holds draws statement 8 marked
 *  "Overtaken", as it draws every match. */
async function expectWordsFilterDraws(page: Page, read: SeededPage): Promise<void> {
  const { plain, seeded, traffic, treeId } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, `${plain}?q=${encodeURIComponent(STATEMENT_8_WORD)}`);
  const row = nodeRow(page, treeId(ids.statement8)).first();
  await expect(row, "the words filter draws statement 8 marked Overtaken").toHaveCount(1);
  await expect
    .poll(() => ownMarks(row), { message: "the words filter draws statement 8 marked Overtaken" })
    .toEqual([OVERTAKEN]);
}

/** Waits until the focus is on the row of the node `readId` names, and that row is on screen. */
async function expectFocusedOnScreen(
  page: Page,
  read: SeededPage,
  readId: string,
  what: string
): Promise<void> {
  await expect
    .poll(() => focusedRowReadId(page), {
      message: `${what} does not focus its row`,
      timeout: DRAWN_MS,
    })
    .toBe(readId);
  await expect
    .poll(() => onScreen(nodeRow(page, read.treeId(readId)).first()), {
      message: `${what} does not bring its row on screen`,
      timeout: DRAWN_MS,
    })
    .toBe(true);
}

/**
 * A press on the chip naming statement 8, on the decision that shapes it, follows the chip to
 * the statement's own address, the Spec view at its block (`/spec#b-…`), and the page puts the
 * focus on the statement's row there, on screen, as it does for a live statement a chip names
 * (`e2e/issue-page-j.e2e.ts` reads that). The Spec view's rows carry the tree's id and no read
 * id (`SpecFilterBody.tsx`), so the row is told by `data-node-id`.
 */
async function expectChipReaches(page: Page, read: SeededPage): Promise<void> {
  const { answer, key, plain, seeded, traffic, treeId } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, plain);
  const shaping = await drawnRow(
    page,
    ids.shapingDecision8,
    "the agent's decision that shapes statement 8"
  );
  const chip = chipNaming(page, shaping, wordsOf(answer, ids.statement8));
  await expect(chip, "the decision that shapes statement 8 draws no chip naming it").toHaveCount(1);
  await chip.locator("[data-chip-name]").click();
  await expect(
    page,
    "a press on the chip naming statement 8 does not open the Spec view at its block"
  ).toHaveURL(new RegExp(`${presetPath(key, "spec")}(#|$)`));
  await expectActivePreset(page, PRESET_NAMES.spec);
  const statement8 = treeId(ids.statement8);
  await expect
    .poll(() => focusedNodeId(page), {
      message: "a press on the chip naming statement 8 does not focus its row",
      timeout: DRAWN_MS,
    })
    .toBe(statement8);
  await expect
    .poll(() => onScreen(nodeRow(page, statement8).first()), {
      message: "a press on the chip naming statement 8 does not bring its row on screen",
      timeout: DRAWN_MS,
    })
    .toBe(true);
}

/** A press on the overtaken question's Contents entry, and its `/asks/<id>` address, each focus the
 *  question's row, on screen. */
async function expectQuestionReached(page: Page, read: SeededPage): Promise<void> {
  const { key, plain, seeded, traffic, treeId } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, plain);
  const entry = contentsPane(page).locator(
    `[data-contents-entry="${treeId(ids.overtakenQuestion)}"]`
  );
  await expect(entry, "the overtaken question is a Contents entry").toHaveCount(1);
  await entry.click();
  await expectFocusedOnScreen(
    page,
    read,
    ids.overtakenQuestion,
    "a press on the overtaken question's Contents entry"
  );
  await gotoIssue(page, `/issues/${key}/asks/${seeded.overtakenAskId}`);
  await expectFocusedOnScreen(
    page,
    read,
    ids.overtakenQuestion,
    "the overtaken question's /asks/<id> address"
  );
}

/** `q` from the open question that stands before the overtaken one focuses it, as it focuses any
 *  open question: from a question `q` goes to the next one in the tree's order, and from any other
 *  row to the first, so the press is made on the question before the overtaken one, whatever rows
 *  the page draws between them. */
async function expectQReaches(page: Page, read: SeededPage): Promise<void> {
  const { plain, seeded, traffic, treeId } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, plain);
  await drawnRowId(page, ids.overtakenQuestion, "the overtaken question");
  const start = nodeRow(page, treeId(ids.openQuestion)).first();
  await expect(start, "the open question is no row of the outline").toHaveCount(1);
  await start.focus();
  await expect(start).toBeFocused();
  await page.keyboard.press("q");
  await expectFocusedOnScreen(
    page,
    read,
    ids.overtakenQuestion,
    "q from the question before the overtaken one"
  );
}

/** `w` on a statement's row whose decision behind it is the overtaken decision focuses that
 *  decision's row: statement 10 is shaped by that decision and by no other. */
async function expectWReaches(page: Page, read: SeededPage): Promise<void> {
  const { plain, seeded, traffic } = read;
  const { ids } = seeded;
  await gotoSettled(page, traffic, plain);
  await focusRow(page, await drawnRowId(page, ids.statement10, "statement 10"));
  await page.keyboard.press("w");
  await expectFocusedOnScreen(
    page,
    read,
    ids.waitingDecision,
    "w on the row of the statement the overtaken decision shaped"
  );
}

// SLOW(>=2s): a whole seeded issue with ten node states, two questions with the two nodes under one
// and a lens written, and seven loads of the outline, each followed by a press or a key.
test("each way to a node reaches an overtaken one", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(browser, seedGateIssue, async (page, gateIssue) => {
    owedUnless(await treeReadGate(page, gateIssue));
    owedUnless(await convertedGate(page, gateIssue));
    const whole = await seedWholeIssue();
    const seeded = await seedNodeStates(whole);
    await page.setViewportSize(DESKTOP);
    const read = await seededPage(page, await loadedPage(page, seeded));
    await expectWordsFilterDraws(page, read);
    await expectChipReaches(page, read);
    await expectQuestionReached(page, read);
    await expectQReaches(page, read);
    await expectWReaches(page, read);
  });
});

// ---------------------------------------------------------------------------------------------
// Row 6: the ask node of a question marks its row.
// ---------------------------------------------------------------------------------------------

/** One question row 6 reads: the read id of the block the page draws its one row from, the id of
 *  the ask's node, whose state the row draws, the mark that state draws, and the words a reading
 *  names it by. */
interface StatedQuestion {
  readonly askId: string;
  readonly mark: DrawnMark;
  readonly readId: string;
  readonly what: string;
}

/** The question overtaken by its ask's address, read first, and the one proposed by its ask's
 *  address. */
function statedQuestions({ seeded }: LoadedPage): readonly StatedQuestion[] {
  return [
    {
      askId: seeded.overtakenAskId,
      mark: OVERTAKEN,
      readId: seeded.ids.overtakenQuestion,
      what: "the question overtaken by its ask's address",
    },
    {
      askId: seeded.proposedAskId,
      mark: PROPOSED,
      readId: seeded.ids.proposedQuestion,
      what: "the question proposed by its ask's address",
    },
  ];
}

/**
 * In the outline, each question carries its mark and its shape on its one row: the page draws the
 * block it is written as and the ask's node, whose state the seed wrote, as one row, and no row of
 * the ask's node apart. The block's own node stays live, so a page that reads it draws no mark.
 */
async function expectQuestionMarks(page: Page, read: LoadedPage): Promise<void> {
  const questions = statedQuestions(read);
  for (const { mark, readId, what } of questions) {
    const row = await drawnRow(page, readId, what);
    await expect
      .poll(() => ownMarks(row), { message: `${what} carries ${mark.word}` })
      .toEqual([mark]);
  }
  // Each reading above opened every branch and found its question's row drawn, so one read of
  // the rows holds every row drawn for either question.
  const items = await treeItemsDrawn(page);
  for (const { askId, readId, what } of questions) {
    const id = read.treeId(readId);
    const rows = items.filter((item) => item.id === id || item.readId === askId);
    expect(rows.length, `${what} is not one row of the outline`).toBe(1);
  }
}

/** Each question carries its mark in Graph, read only where the page draws a Graph control. */
async function expectQuestionMarksInGraph(page: Page, read: LoadedPage): Promise<void> {
  if (!noteOwed(await graphGate(page))) return;
  const name = graphName();
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  for (const { mark, readId, what } of statedQuestions(read)) {
    await expectMarkedInGraph(page, readId, mark, what);
  }
  await pickPreset(page, PRESET_NAMES.conversation);
}

/** With the Decisions view turned on from the outline, with no reload, each question stands with
 *  the open questions, carrying its mark (`expectWithOpenQuestions`). */
async function expectQuestionMarksInDecisions(page: Page, read: LoadedPage): Promise<void> {
  await pickPreset(page, PRESET_NAMES.decisions);
  await expectActivePreset(page, PRESET_NAMES.decisions);
  for (const { mark, readId, what } of statedQuestions(read)) {
    await expectWithOpenQuestions(page, read.treeId(readId), mark, what);
  }
}

/** The header's count of what waits counts the proposed question and not the overtaken one, and
 *  its presses reach the proposed one and never the overtaken one (`expectWaitsLeaveOut`). */
async function expectWaitCountsProposed(page: Page, read: LoadedPage): Promise<void> {
  const reached = await expectWaitsLeaveOut(page, read);
  expect(
    reached,
    "the header's presses do not reach the question proposed by its ask's address"
  ).toContain(read.treeId(read.seeded.ids.proposedQuestion));
}

/** Settle on the proposed question's row sends one `POST` to the settle route, naming its ask's
 *  address, and the mark goes. */
async function expectQuestionSettled(page: Page, read: LoadedPage): Promise<void> {
  const { plain, seeded, traffic } = read;
  await gotoSettled(page, traffic, plain);
  const { row } = await reachSettle(
    page,
    seeded.ids.proposedQuestion,
    "the question proposed by its ask's address",
    "the proposed question's row offers Settle"
  );
  traffic.reset();
  await page.keyboard.press("Enter");
  await expect
    .poll(() => ownMarks(row), {
      message: "the question proposed by its ask's address keeps its mark once it is settled",
    })
    .toEqual([]);
  expect(
    settlesSent(traffic.requests),
    "Settle on the proposed question's row does not send one settle naming its ask's address"
  ).toEqual([seeded.addresses.proposedQuestion]);
}

// SLOW(>=2s): a whole seeded issue with ten node states, two questions with the two nodes under one
// and a lens written, the outline, Graph and the Decisions view read on one load, the header's
// count pressed past each thing that waits, and one settle.
test("the ask node of a question marks its row", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(browser, seedGateIssue, async (page, gateIssue) => {
    owedUnless(await treeReadGate(page, gateIssue));
    owedUnless(await convertedGate(page, gateIssue));
    const whole = await seedWholeIssue();
    const seeded = await seedNodeStates(whole);
    await page.setViewportSize(DESKTOP);
    const read = await loadedPage(page, seeded);
    // The outline, Graph and the Decisions view are read on the page's first load, with no reload.
    await expectQuestionMarks(page, read);
    await expectQuestionMarksInGraph(page, read);
    await expectQuestionMarksInDecisions(page, read);
    await expectWaitCountsProposed(page, read);
    await expectQuestionSettled(page, read);
  });
});

// ---------------------------------------------------------------------------------------------
// Row 3: a node reserved for its author.
// ---------------------------------------------------------------------------------------------

/** What one reader's page draws of the issue, and the reads it drew it from. */
interface ReaderReading {
  /** The reader's own tree read. */
  readonly answer: IssueTreeRead;
  /** The read id of every row the page draws as it opens, every node drawn whole and open. */
  readonly drawn: ReadonlySet<string>;
  /** Each filter control's count, read with that filter on. */
  readonly filters: Readonly<Record<(typeof VIEWS)[number], number>>;
  /** Each branch's count, read once its row is folded. */
  readonly folded: readonly FoldedCount[];
  /** The page's text as it opens, and each view's. */
  readonly texts: readonly { readonly text: string; readonly where: string }[];
}

/** What `foldEachBranch` reads each branch's count against besides the reader's read. */
interface BranchCounts {
  /** What the count of the row drawn for `readId` reads besides that node (`HeldRow`); nothing
   *  besides it where this is not given. */
  readonly held?: (readId: string) => HeldRow;
  /** What a failure names the count as including, after what the reader's read holds under the
   *  branch; nothing more where this is not given. */
  readonly including?: string;
}

/**
 * Folds each branch the page draws open, one at a time, reads the count its row draws once it is
 * folded beside what `answer` holds under it (`heldUnder`, with `held`'s `HeldRow` for its read
 * id), and opens it again: each branch's count, as the reader's own read makes it.
 */
async function foldEachBranch(
  page: Page,
  answer: IssueTreeRead,
  key: string,
  who: string,
  { held = () => ({}), including }: BranchCounts = {}
): Promise<FoldedCount[]> {
  // The page opens with every node drawn, so one read of its rows holds every branch.
  const branches = (await treeItemsDrawn(page)).filter(
    (item) => item.expanded === "true" && item.readId !== null
  );
  const besides = including === undefined ? "" : `, ${including}`;
  const counts: FoldedCount[] = [];
  for (const branch of branches) {
    const readId = branch.readId ?? "";
    const row = nodeRow(page, branch.id).first();
    await row.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
    await expect(row, `${who}'s fold of ${branch.id} does not fold it`).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    const count = Number(await row.getAttribute("data-folded-count"));
    expect(
      count,
      `${who}'s fold of ${branch.id} does not count what ${who}'s tree read holds under it${besides}`
    ).toBe(heldUnder(answer, key, readId, held(readId)));
    counts.push({ count, id: branch.id, readId: branch.readId });
    await row.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
    await expect(row, `${who}'s fold of ${branch.id} does not open again`).toHaveAttribute(
      "aria-expanded",
      "true"
    );
  }
  return counts;
}

/**
 * Reads issue `key` on `page` as its reader, with `traffic`, the page's one watcher: the page as
 * it opens, every node drawn whole and unfolded, its text and the nodes it draws; each branch's
 * count once its row folds it, beside the count the reader's own tree read and comments read
 * make; and each view, its control's count beside the count its own view read makes where the
 * page holds that read.
 */
async function readAsReader(
  page: Page,
  traffic: ApiRequests,
  key: string,
  who: string
): Promise<ReaderReading> {
  const tree = treeAnswerOf(page);
  const views: Record<(typeof VIEWS)[number], TreeAnswerWatch> = {
    decisions: viewAnswerOf(page, "decisions"),
    plan: viewAnswerOf(page, "plan"),
    spec: viewAnswerOf(page, "spec"),
    verification: viewAnswerOf(page, "verification"),
  };
  const plain = presetPath(key, "conversation");
  await gotoSettled(page, traffic, plain);
  const answer = await heldAnswer(tree, `${who}'s tree read`);
  const margin = await marginComments(key, who);
  const drawn = new Set(
    (await treeItemsDrawn(page)).flatMap((item) => (item.readId === null ? [] : [item.readId]))
  );
  const texts = [{ text: await issuePage(page).innerText(), where: "the outline" }];
  const folded = await foldEachBranch(page, answer, key, who, { held: () => ({ margin }) });
  const filters = { decisions: 0, plan: 0, spec: 0, verification: 0 };
  for (const view of VIEWS) {
    const name = PRESET_NAMES[view];
    await pickPreset(page, name);
    await expectActivePreset(page, name);
    await expectLoadSettled(traffic);
    const viewed: Gate = {
      met: view !== "decisions" || treeReads(traffic.requests, key, "decisions") > 0,
      owed:
        "owed until the page reads its decisions as a view of the tree read, so the Decisions " +
        "count is compared with the other reader's alone",
    };
    if (noteOwed(viewed)) {
      const held = await heldAnswer(views[view], `${who}'s ${name} view read`);
      await expect
        .poll(() => filterCount(page, view), {
          message: `${who}'s ${name} control does not count what ${who}'s own view read holds`,
        })
        .toBe(viewCount(held, view));
    }
    let count = Number.NaN;
    await expect
      .poll(
        async () => {
          count = await filterCount(page, view);
          return count;
        },
        { message: `${who}'s ${name} control draws no count` }
      )
      .not.toBeNaN();
    filters[view] = count;
    texts.push({ text: await issuePage(page).innerText(), where: `the ${name} view` });
  }
  await pickPreset(page, PRESET_NAMES.conversation);
  return { answer, drawn, filters, folded, texts };
}

/** The read id of the node of `answer` that holds `word`; throws, naming `what`, where none
 *  does. */
function readIdHolding(answer: IssueTreeRead, word: string, what: string): string {
  const node = nodeHolding(answer, word);
  if (node === undefined) throw new Error(`the tree read holds no ${what} (${word})`);
  return node.id;
}

/** What `?node=` of `node` draws once the page has gone quiet (`gotoSettled`): the row it
 *  focuses and the tree's text. */
async function nodeAddressDraws(
  page: Page,
  traffic: ApiRequests,
  plain: string,
  node: string
): Promise<{ readonly focused: string | null; readonly text: string }> {
  await gotoSettled(page, traffic, `${plain}?node=${encodeURIComponent(node)}`);
  return { focused: await focusedRowReadId(page), text: await treeText(page) };
}

/** What the words filter with `word` draws once the page has gone quiet (`gotoSettled`): the
 *  tree's text. */
async function wordsFilterDraws(
  page: Page,
  traffic: ApiRequests,
  plain: string,
  word: string
): Promise<string> {
  await gotoSettled(page, traffic, `${plain}?q=${encodeURIComponent(word)}`);
  return treeText(page);
}

/** How many times the `j` walk presses `j` past the rows the tree draws before it gives up. */
const WALK_SLACK = 4;

/** The read id of every row `j` reaches from the outline's first row to its last, once the page
 *  has gone quiet (`gotoSettled`), as the page opens with every node drawn. */
async function jWalk(page: Page, traffic: ApiRequests, plain: string): Promise<string[]> {
  await gotoSettled(page, traffic, plain);
  // The load has gone quiet and the page draws every node it holds, so one read of its rows is
  // every row the walk can reach.
  const items = await treeItemsDrawn(page);
  await nodeRows(page).first().focus();
  const reached: string[] = [];
  for (let pressed = 0; pressed < items.length + WALK_SLACK; pressed += 1) {
    // One read per press: j moves the focus before the press returns, so this reads where it is.
    const focused = await focusedRowReadId(page);
    if (focused !== null) reached.push(focused);
    await page.keyboard.press("j");
  }
  return reached;
}

/** Details' "Referenced by" count of statement 2, read only where Details draws the focused
 *  row's facts, beside the count `answer` makes; null where it is not read. `traffic` is the
 *  page's one watcher, so the load settles before its rows are opened, as each step's does. */
async function statementTwoReferences(
  page: Page,
  traffic: ApiRequests,
  key: string,
  statementTwo: string,
  answer: IssueTreeRead,
  who: string
): Promise<number | null> {
  if (!noteOwed(FOCUSED_FACTS_GATE)) return null;
  await gotoSettled(page, traffic, presetPath(key, "conversation"));
  await focusRow(page, await drawnRowId(page, statementTwo, "statement 2"));
  await openDetails(page);
  const count = await referencedByCount(page);
  expect(count, `${who}'s Referenced by of statement 2 is not what ${who}'s read holds`).toBe(
    referencesTo(answer, statementTwo)
  );
  return count;
}

/** Bob's readings of the issue alice reserved nodes on, on his page with `traffic`, its one
 *  watcher: none of them, nor any trace of them, the risk she wrote under her reserved pre-mortem
 *  with no visibility of its own among them. */
async function expectNothingReserved(
  page: Page,
  traffic: ApiRequests,
  seeded: AuthorOnlySeed,
  reserved: ReadonlyMap<string, string>,
  his: ReaderReading
): Promise<void> {
  const key = seeded.whole.key;
  const plain = presetPath(key, "conversation");
  const words = RESERVED_NODES.map((name) => seeded.nodes[name].word);
  for (const { text, where } of his.texts) {
    expect(
      words.filter((word) => text.includes(word)),
      `bob's page draws a reserved node's words in ${where}`
    ).toEqual([]);
  }
  expect(
    [...reserved.values()].filter((id) => his.drawn.has(id)),
    "bob's page draws a reserved node"
  ).toEqual([]);

  // The words filter with each reserved word draws what a word nobody wrote draws.
  const nobody = await wordsFilterDraws(page, traffic, plain, NOBODY_WROTE);
  for (const word of words) {
    expect(
      await wordsFilterDraws(page, traffic, plain, word),
      `bob's words filter with ${word} draws what a word nobody wrote does not`
    ).toBe(nobody);
  }

  // `j` from the first row to the last never reaches one.
  expect(
    (await jWalk(page, traffic, plain)).filter((id) => [...reserved.values()].includes(id)),
    "j reaches a reserved node on bob's page"
  ).toEqual([]);

  // The project lens names no lens he cannot read, and offers no chain.
  // Opened before the load settles, a row takes back the focus the opener leaves for the page.
  await gotoSettled(page, traffic, plain);
  const projectLens = readIdHolding(his.answer, seeded.nodes.projectLens.word, "project lens");
  const lensRow = await focusRow(page, await drawnRowId(page, projectLens, "the project lens"));
  expect(
    (await lensLines(lensRow)).map((line) => line.line),
    "bob's project lens reads Triggered by"
  ).not.toContain("triggered_by");
  await expect(
    lensShowChain(lensRow),
    "bob's project lens offers to show the whole chain"
  ).toHaveCount(0);

  // `?node=` of the reserved lens and of the constraint draw what an unknown uuid draws.
  const unknown = await nodeAddressDraws(page, traffic, plain, crypto.randomUUID());
  for (const name of ["lens", "constraint"] as const) {
    expect(
      await nodeAddressDraws(page, traffic, plain, reserved.get(name) ?? ""),
      `bob's ?node= of the reserved ${name} does not draw what an unknown uuid draws`
    ).toEqual(unknown);
  }
}

/** Bob's reading of the issue, in a context of his own with the new page turned on for him,
 *  closed once read: what his page draws and counts, and that it draws nothing alice reserved. */
async function bobsReading(
  browser: Browser,
  seeded: AuthorOnlySeed,
  reserved: ReadonlyMap<string, string>
): Promise<{ readonly his: ReaderReading; readonly references: number | null }> {
  const key = seeded.whole.key;
  const context = await asUser(browser, "bob");
  try {
    await withIssuePage(context, "bob");
    const page = await context.newPage();
    const traffic = watchApiRequests(page);
    await page.setViewportSize(DESKTOP);
    const his = await readAsReader(page, traffic, key, "bob");
    const statementTwo = seeded.whole.requirements[1] ?? "";
    const references = await statementTwoReferences(
      page,
      traffic,
      key,
      statementTwo,
      his.answer,
      "bob"
    );
    await expectNothingReserved(page, traffic, seeded, reserved, his);
    return { his, references };
  } finally {
    await context.close();
  }
}

// SLOW(>=2s): a whole seeded issue with seven lens nodes written, two readers each reading the
// outline with each branch folded and opened and each of the four views, seven words filters, a
// walk of every row and three loads by `?node=`.
test("a node reserved for its author is on its author's page and on nobody else's", async ({
  browser,
}) => {
  test.setTimeout(480_000);
  await phase(browser, seedGateIssue, async (page, gateIssue) => {
    owedUnless(await treeReadGate(page, gateIssue));
    owedUnless(await convertedGate(page, gateIssue));
    const seeded = await seedAuthorOnly();
    const key = seeded.whole.key;
    const plain = presetPath(key, "conversation");
    const hers = await readTree(key, null);
    expect(hers.converted, "the seeded issue's tree read does not answer converted").toBe(true);
    await page.setViewportSize(DESKTOP);
    const traffic = watchApiRequests(page);
    const reserved = new Map(
      RESERVED_NODES.map((name) => [
        name,
        readIdHolding(hers, seeded.nodes[name].word, `reserved ${name}`),
      ])
    );
    const reservedIds = new Set(reserved.values());
    const statementTwo = seeded.whole.requirements[1] ?? "";

    // First as bob, whose page holds what his own read answers him.
    const { his, references: hisReferences } = await bobsReading(browser, seeded, reserved);

    // Then as alice, who reads each node she reserved, marked as hers alone.
    const her = await readAsReader(page, traffic, key, "alice");
    for (const name of RESERVED_NODES) {
      const row = await drawnRow(page, reserved.get(name) ?? "", `her reserved ${name}`);
      await expect
        .poll(() => ownMarks(row), { message: `her reserved ${name} does not carry Only you` })
        .toEqual([ONLY_YOU]);
    }
    const projectLens = readIdHolding(her.answer, seeded.nodes.projectLens.word, "project lens");
    const lensId = await drawnRowId(page, projectLens, "the project lens");
    const lensRow = await focusRow(page, lensId);
    const triggered = (await lensLines(lensRow)).find((line) => line.line === "triggered_by");
    expect(
      { label: triggered?.label, chips: triggered?.chips.length },
      "her project lens does not read Triggered by with one chip"
    ).toEqual({ chips: 1, label: LENS_WORDS.triggered_by });
    await expect
      .poll(
        () => compactMarks(lensRow.locator('[data-lens-line="triggered_by"] [data-node-chip]')),
        { message: "the chip of her reserved lens does not carry Only you in its compact form" }
      )
      .toEqual([ONLY_YOU]);

    // Each of her counts is bob's and the reserved nodes in that branch.
    const bobsFolds = new Map(his.folded.map((row) => [row.readId, row.count]));
    for (const row of her.folded) {
      const bobs = bobsFolds.get(row.readId);
      if (row.readId === null || bobs === undefined) continue;
      expect(row.count, `her fold of ${row.id} is not bob's and the reserved nodes under it`).toBe(
        bobs + reservedUnder(her.answer, key, row.readId, reservedIds)
      );
    }
    expect(her.filters, "her filter counts are not bob's: no filter draws a reserved node").toEqual(
      his.filters
    );
    const herReferences = await statementTwoReferences(
      page,
      traffic,
      key,
      statementTwo,
      her.answer,
      "alice"
    );
    if (herReferences !== null && hisReferences !== null) {
      expect(
        herReferences,
        "her Referenced by of statement 2 is not bob's and the reserved nodes'"
      ).toBe(
        hisReferences +
          (her.answer.tuples ?? []).filter(
            (tuple) => tuple.to.id === statementTwo && reservedIds.has(tuple.from.id)
          ).length
      );
    }

    // Every node of the project is drawn for both, and the reserved ones for her alone.
    expect(
      [...her.drawn].filter((id) => !reservedIds.has(id)).sort(),
      "the project's nodes are not drawn alike for both readers"
    ).toEqual([...his.drawn].sort());
    expect(
      [...reservedIds].filter((id) => !her.drawn.has(id)),
      "her page does not draw every node she reserved"
    ).toEqual([]);
    await gotoIssue(page, plain);
  });
});
