import type { IssueTreeAnswer } from "../web/src/api/client";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { chipWords } from "../web/src/features/issue-page/node-row-chip";
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
import { seedWholeIssue } from "./issue-layout-seed";
import { gotoIssue, issuePage, issueRead, openDetails, phase } from "./issue-page";
import {
  DESKTOP,
  focusRow,
  focusThePage,
  nodeRow,
  nodeRows,
  PHONE,
  parentOfRow,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  activePreset,
  expectActivePreset,
  onScreen,
  openEveryDeeperBranch,
  pickPreset,
  rowGaps,
  treeText,
} from "./issue-page-i";
import { planChart, planChartRows } from "./issue-page-k";
import { withIssuePage } from "./issue-page-option";
import { decisionsPartRows, decisionsRows } from "./issue-page-q";
import {
  convertedGate,
  type Gate,
  heldAnswer,
  owedUnless,
  type TreeAnswerWatch,
  type TreeView,
  treeAnswerOf,
  treeReadGate,
  treeReads,
  viewAnswerOf,
  viewCount,
} from "./issue-page-t";
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
  foldedCounts,
  heldUnder,
  lineFocused,
  lineLocator,
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
  seedAuthorOnly,
  seedGateIssue,
  seedNodeStates,
  settleNodeApi,
  settlesSent,
  type TreeItem,
  treeIdReads,
  treeItemsDrawn,
  treeReadAs,
} from "./issue-page-x";
import { type Browser, expect, type Locator, type Page, type Route, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";
import { asUser } from "./users";

/**
 * The rows of a node's life on the new issue page - a proposed node marked and settled, an
 * overtaken one folded away - and of a node reserved for its author, read on the real stack
 * (`web/src/features/issue-page/node-states/`, with the helpers of `e2e/issue-page-x.ts`).
 *
 * Each row turns the new page on for alice with `phase` (`e2e/issue-page.ts`) and checks its
 * gates first, on an issue of its own (`seedGateIssue`): the tree read is served (`treeReadGate`,
 * which reads its 404) and an issue is born converted (`convertedGate`), each with `owedUnless`
 * (`e2e/issue-page-t.ts`). Where a gate is not met the row skips with an `owed` annotation that
 * names it, having written nothing else, and never passes; once both are met it runs whole: it
 * seeds its issue with `seedWholeIssue` and the seeds of `e2e/issue-page-x.ts`, asserts that
 * issue's tree read answers `converted: true`, and reads it. The proposed and overtaken nodes are
 * written by the addresses the tree read answers alice (`seedNodeStates`); the reserved nodes'
 * writes answer their own (`seedAuthorOnly`).
 *
 * Every node a row names it names by the id the tree read gives it, and every row it reads by the
 * id the page draws it under for that read id (`drawnIdOf`, `treeIdOf`): no row builds an id. A
 * `@phone` row reads 1440 px on `chromium` and 390 px on `iphone` (`rowSize`). Every word a row
 * reads is the page's own word list's (`page-words.ts`), never typed here.
 *
 * Details draws the focused row's facts, and their "Referenced by" list, only where this tree
 * holds the file that draws them (`FOCUSED_FACTS_GATE`): until then a row notes those readings
 * as owed (`noteOwed`) and makes every other.
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

/** The four views, by the filter each reads its view of the tree for. */
const VIEWS = ["spec", "verification", "plan", "decisions"] as const satisfies readonly TreeView[];

/** The lines a risk draws under its words of its own, none of which is about a lens. */
const RISK_LINES: readonly string[] = [
  "early_warning",
  "prevention",
  "prevented_by",
  "prevented_by_nothing",
];

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
 *  focus on no row. No poll opens an overtaken line: neither opener presses one. */
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

/** The block id an address names, or undefined for one that names no block. */
function blockOf(address: string): string | undefined {
  const route = parseNodeReference(address);
  return route !== undefined && "block" in route ? route.block?.id : undefined;
}

/** Fails where an element of the page has a raw value of the tree read, or a word-list key, as
 *  its whole text: the page names a node's life and its reader in its own words alone. */
async function expectNoRawValue(page: Page, where: string): Promise<void> {
  expect(
    await rawValueTexts(page, WORD_BOOK),
    `${where}, a text reads a state or a visibility as the tree read writes it`
  ).toEqual([]);
}

/** The line that holds the row of `readId` in `items`: the item just above that row one level
 *  up, where it is an overtaken line; undefined where the row is not drawn or is no line's. */
function lineHolding(items: readonly TreeItem[], readId: string): TreeItem | undefined {
  const at = items.findIndex((item) => item.readId === readId);
  const level = items[at]?.level ?? 0;
  for (let above = at - 1; above >= 0; above -= 1) {
    const item = items[above];
    if (item === undefined || item.level >= level) continue;
    return item.level === level - 1 && item.kind === "line" ? item : undefined;
  }
  return undefined;
}

/** The node `readId` of `answer`'s words, as the read holds them. */
function wordsOf(answer: IssueTreeAnswer, readId: string): string {
  return answer.nodes.find((node) => node.id === readId)?.text ?? "";
}

// ---------------------------------------------------------------------------------------------
// Row 1: a proposed node is marked and settled, and an overtaken one folds away.
// ---------------------------------------------------------------------------------------------

/** What row 1 reads its page with: the issue, its tree read, the id the page draws each node of
 *  that read under, and the page's one watcher of its requests, made before its first load. */
interface ProposedPage {
  readonly answer: IssueTreeAnswer;
  readonly key: string;
  readonly plain: string;
  readonly seeded: NodeStateSeed;
  readonly traffic: ApiRequests;
  readonly treeId: (readId: string) => string;
  readonly wide: boolean;
}

/** How many statements the seed writes: twelve and the one that cites a document. */
function statementsOf(seeded: NodeStateSeed): number {
  return seeded.whole.requirements.length + 1;
}

/**
 * Step (1) in the outline: statement 9, live with no check, carries "No check" and no mark,
 * read before statement 7, which carries "Proposed" and its shape and no gap; check 2, the first
 * decision and plan task 1 carry "Proposed"; the chip naming statement 7 on the decision that
 * shapes it carries it in its compact form; each keeps its word and its shape with forced
 * colours; and no text is a raw value of the tree read.
 */
async function expectProposedMarks(page: Page, { answer, seeded }: ProposedPage): Promise<void> {
  const { ids } = seeded;
  const statement9 = await drawnRow(page, ids.statement9, "statement 9");
  expect(await ownMarks(statement9), "statement 9 carries no Proposed mark").toEqual([]);
  await expect(
    statement9.locator('[data-gap][data-gap-kind="no_check"]'),
    "statement 9, live with no check, does not carry No check"
  ).toContainText(GAP_WORDS.no_check);
  const statement7 = await drawnRow(page, ids.statement7, "statement 7");
  expect(await ownMarks(statement7), "statement 7 carries Proposed").toEqual([PROPOSED]);
  await expect(rowGaps(statement7), "statement 7, which is proposed, draws a gap").toHaveCount(0);
  const proposed = [
    { readId: ids.check2, what: "check 2" },
    { readId: ids.decision1, what: "the first decision" },
    { readId: ids.planTask1, what: "plan task 1" },
  ];
  const rows = [statement7];
  for (const { readId, what } of proposed) {
    const row = await drawnRow(page, readId, what);
    expect(await ownMarks(row), `${what} does not carry Proposed in the outline`).toEqual([
      PROPOSED,
    ]);
    rows.push(row);
  }
  const shaping = await drawnRow(
    page,
    ids.shapingDecision,
    "the agent's decision that shapes statement 7"
  );
  const chip = shaping.locator("[data-node-chip]").filter({
    has: page.locator("[data-chip-name]", {
      hasText: chipWords(wordsOf(answer, ids.statement7)),
    }),
  });
  await expect(chip, "the decision that shapes statement 7 draws no chip naming it").toHaveCount(1);
  expect(
    await compactMarks(chip),
    "the chip naming statement 7 does not carry Proposed in its compact form"
  ).toEqual([PROPOSED]);

  await page.emulateMedia({ forcedColors: "active" });
  try {
    for (const row of rows) {
      expect(
        await ownMarks(row),
        "a proposed row's mark loses its word or its shape with forced colours"
      ).toEqual([PROPOSED]);
    }
    expect(
      await compactMarks(chip),
      "the chip's compact mark loses its word or its shape with forced colours"
    ).toEqual([PROPOSED]);
  } finally {
    await page.emulateMedia({ forcedColors: "none" });
  }
  await expectNoRawValue(page, "in the outline");
}

/**
 * Details' facts of statement 7 carry "Proposed", read only where Details draws the focused
 * row's facts; noted as owed otherwise.
 */
async function expectProposedInDetails(page: Page, { seeded }: ProposedPage): Promise<void> {
  if (!noteOwed(FOCUSED_FACTS_GATE)) return;
  const statement7 = await drawnRowId(page, seeded.ids.statement7, "statement 7");
  await focusRow(page, statement7);
  await openDetails(page);
  await expect
    .poll(() => compactMarks(focusedNodeFacts(page)), {
      message: "Details' facts of statement 7 do not carry Proposed",
    })
    .toEqual([PROPOSED]);
}

/**
 * Step (2) in the outline: statement 8 is no row and no line is open on load; its parent draws
 * its overtaken children's line after its other children, which `j` reaches, Enter opens with
 * statement 8 under it ("Overtaken", no gap, folded with its lens) and closes again; ArrowRight
 * opens it and ArrowLeft closes it at 1440 px; a press opens it; and the words filter with
 * statement 8's words draws statement 8 under its open line. Plan task 2 is no row either.
 */
async function expectOvertakenFold(page: Page, read: ProposedPage): Promise<void> {
  const { answer, key, plain, seeded, treeId, wide } = read;
  const { ids } = seeded;
  await gotoIssue(page, plain);
  await unfoldEveryBranch(page);
  await openEveryDeeperBranch(page);
  let items = await treeItemsDrawn(page);
  expect(
    items.some((item) => item.readId === ids.statement8),
    "statement 8 is a row of the outline"
  ).toBe(false);
  expect(
    items.some((item) => item.readId === ids.planTask2),
    "plan task 2 is a row of the outline"
  ).toBe(false);
  expect(
    items.filter((item) => item.kind === "line" && item.expanded !== "false").length,
    "a path is open to an overtaken node on load"
  ).toBe(0);

  const parent = answer.nodes.find((node) => node.id === ids.statement8)?.parent ?? null;
  const parentId = parent === null ? key : treeId(parent);
  const parentRow = parentId === key ? null : items.find((item) => item.id === parentId);
  const held = answer.nodes.filter(
    (node) =>
      node.parent === parent &&
      node.state === "overtaken" &&
      (node.issue === undefined || node.issue === key)
  ).length;
  const siblings = childItems(items, parentRow);
  const line = siblings[siblings.length - 1];
  expect(line?.kind, "statement 8's parent draws no overtaken line after its other children").toBe(
    "line"
  );
  expect(line?.text, `statement 8's parent does not say it holds ${held} overtaken`).toBe(
    `${held} ${CONTROL_LABELS.overtaken_count}`
  );
  if (line === undefined) return;
  const lines = items.filter((item) => item.kind === "line");
  const lineAt = lines.indexOf(line);
  const lineElement = lineLocator(page, items, line);
  await expect(lineElement, "statement 8's line is drawn open").toHaveAttribute(
    "aria-expanded",
    "false"
  );

  // `j` from the item drawn just above the line reaches it.
  await issuePage(page)
    .locator("[data-node-row], [data-overtaken-line]")
    .nth(items.indexOf(line) - 1)
    .focus();
  await page.keyboard.press("j");
  await expect
    .poll(() => lineFocused(page), { message: "j does not reach statement 8's overtaken line" })
    .toBe(true);

  // Enter opens it: statement 8 stands under it, overtaken, with no gap, folded with its lens.
  await page.keyboard.press("Enter");
  await expect(lineElement, "Enter does not open statement 8's line").toHaveAttribute(
    "aria-expanded",
    "true"
  );
  items = await treeItemsDrawn(page);
  const open = items.filter((item) => item.kind === "line")[lineAt];
  expect(
    childItems(items, open).map((item) => item.readId),
    "Enter does not draw statement 8 under its line"
  ).toContain(ids.statement8);
  const statement8 = issuePage(page).locator(
    `[data-node-row][data-node-read-id="${ids.statement8}"]`
  );
  expect(await ownMarks(statement8), "statement 8 does not carry Overtaken").toEqual([OVERTAKEN]);
  await expect(rowGaps(statement8), "statement 8, which is overtaken, draws a gap").toHaveCount(0);
  await expect(statement8, "statement 8 is drawn open under its line").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  await expect(
    statement8,
    "statement 8's fold does not count its lens with the rest of its branch"
  ).toHaveAttribute("data-folded-count", String(heldUnder(answer, key, ids.statement8)));
  expect(
    items.some((item) => item.readId === ids.lens),
    "the lens on statement 8 is drawn while statement 8 is folded"
  ).toBe(false);

  // Enter closes it again.
  await page.keyboard.press("Enter");
  await expect(lineElement, "Enter does not close statement 8's line").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  expect(
    (await treeItemsDrawn(page)).some((item) => item.readId === ids.statement8),
    "statement 8 stays drawn once its line is closed"
  ).toBe(false);

  // ArrowRight opens it and ArrowLeft closes it, above 1024 px.
  if (wide) {
    await page.keyboard.press("ArrowRight");
    await expect(lineElement, "ArrowRight does not open statement 8's line").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await page.keyboard.press("ArrowLeft");
    await expect(lineElement, "ArrowLeft does not close statement 8's line").toHaveAttribute(
      "aria-expanded",
      "false"
    );
  }

  // A press opens it.
  await lineElement.click();
  await expect(lineElement, "a press does not open statement 8's line").toHaveAttribute(
    "aria-expanded",
    "true"
  );

  // The words filter with statement 8's words draws it under its open line.
  await gotoIssue(page, `${plain}?q=${encodeURIComponent(wordsOf(answer, ids.statement8))}`);
  await expect
    .poll(async () => lineHolding(await treeItemsDrawn(page), ids.statement8)?.expanded ?? null, {
      message: "the words filter with statement 8's words does not draw it under its open line",
      timeout: DRAWN_MS,
    })
    .toBe("true");
}

/**
 * Step (3): `?node=` and `#b-` of statement 8 open its line with it focused; with the Decisions
 * view on, the overtaken decision's address draws the outline under no filter, its line open and
 * the decision focused, on screen.
 */
async function expectOvertakenAddresses(page: Page, read: ProposedPage): Promise<void> {
  const { key, plain, seeded, treeId } = read;
  const { ids } = seeded;
  for (const address of [
    `${plain}?node=${encodeURIComponent(ids.statement8)}`,
    `${plain}#b-${encodeURIComponent(ids.statement8)}`,
  ]) {
    await gotoIssue(page, address);
    await expect
      .poll(() => focusedRowReadId(page), {
        message: `${address} does not focus statement 8`,
        timeout: DRAWN_MS,
      })
      .toBe(ids.statement8);
    expect(
      lineHolding(await treeItemsDrawn(page), ids.statement8)?.expanded,
      `${address} does not open statement 8's line`
    ).toBe("true");
  }
  await gotoIssue(
    page,
    `${presetPath(key, "decisions")}#b-${encodeURIComponent(ids.waitingDecision)}`
  );
  const decision = nodeRow(page, treeId(ids.waitingDecision)).first();
  await expect
    .poll(
      async () => ({
        focused: await focusedRowReadId(page),
        line: lineHolding(await treeItemsDrawn(page), ids.waitingDecision)?.expanded ?? null,
        onScreen: await onScreen(decision),
        path: new URL(page.url()).pathname,
        query: new URL(page.url()).searchParams.get("q"),
        view: await activePreset(page),
      }),
      { message: "the overtaken decision's address opens the outline", timeout: DRAWN_MS }
    )
    .toEqual({
      focused: ids.waitingDecision,
      line: "true",
      onScreen: true,
      path: plain,
      query: null,
      view: PRESET_NAMES.conversation,
    });
}

/**
 * Steps (1) and (2) in each view: check 2 carries "Proposed" under Verification, and so does the
 * line that names it on its result's row; no failing flag and no summary count counts a check or
 * a statement that is proposed or overtaken; the first decision carries "Proposed" under
 * Decisions, where the overtaken decision is in no part and the to-review flag counts the review
 * part alone; plan task 1 carries "Proposed" under Plan, which draws plan task 2 as its line
 * after every step; and the Spec control counts every statement. No view draws a raw value of
 * the tree read.
 */
async function expectProposedViews(page: Page, read: ProposedPage): Promise<void> {
  const { answer, key, seeded, treeId } = read;
  const { ids } = seeded;
  const statements = statementsOf(seeded);

  await gotoIssue(page, presetPath(key, "verification"));
  const check2 = verificationCheck(page, treeId(ids.check2));
  await expect(check2, "the Verification view draws no row for check 2").toHaveCount(1);
  expect(await ownMarks(check2), "check 2 does not carry Proposed under Verification").toEqual([
    PROPOSED,
  ]);
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
        "the Verification summary counts statement 7, which is proposed, or statement 8, which is overtaken",
    })
    .toBe(statements - 2);
  await expectNoRawValue(page, "under Verification");
  const result = answer.nodes.find(
    (node) => node.supertag === "result" && node.parent === ids.check2
  );
  expect(result?.text ?? "", "the tree read holds no words of check 2's result").not.toBe("");
  await gotoIssue(
    page,
    `${presetPath(key, "verification")}?q=${encodeURIComponent(result?.text ?? "")}`
  );
  const parentLine = issuePage(page)
    .locator(`[data-node-parent-line][data-parent-id="${treeId(ids.check2)}"]`)
    .first();
  await expect(
    parentLine,
    "under Verification, the row of check 2's result draws no line naming check 2"
  ).toBeVisible();
  expect(
    await compactMarks(parentLine),
    "the line naming check 2 does not carry Proposed in its compact form"
  ).toEqual([PROPOSED]);

  await gotoIssue(page, presetPath(key, "decisions"));
  const decision1 = nodeRow(page, treeId(ids.decision1)).first();
  await expect(decision1, "the Decisions view draws no row for the first decision").toHaveCount(1);
  expect(
    await ownMarks(decision1),
    "the first decision does not carry Proposed under Decisions"
  ).toEqual([PROPOSED]);
  expect(
    (await decisionsRows(page)).map((row) => row.id),
    "the Decisions view draws the overtaken decision"
  ).not.toContain(treeId(ids.waitingDecision));
  const review = (await decisionsPartRows(page, "review")).length;
  await expect
    .poll(() => flagCount(page, "to_review"), {
      message: "the to-review flag counts a decision the review part does not hold",
    })
    .toBe(review);
  await expectNoRawValue(page, "under Decisions");

  await gotoIssue(page, presetPath(key, "plan"));
  const planTask1 = planChart(page)
    .locator(`[data-node-id="${treeId(ids.planTask1)}"]`)
    .first();
  await expect(planTask1, "the Plan view draws no step for plan task 1").toBeVisible();
  expect(await ownMarks(planTask1), "plan task 1 does not carry Proposed under Plan").toEqual([
    PROPOSED,
  ]);
  expect(
    (await planChartRows(page)).map((row) => row.id),
    "plan task 2 is a step of the Plan view"
  ).not.toContain(treeId(ids.planTask2));
  const planLine = planChart(page).locator("[data-overtaken-line]");
  await expect(planLine, "the Plan view does not draw plan task 2 as its one line").toHaveText(
    `1 ${CONTROL_LABELS.overtaken_count}`
  );
  expect(
    await planChart(page).evaluate(
      (chart) =>
        [
          ...chart.querySelectorAll(
            "[data-plan-bar-row], [data-plan-cycle-row], [data-overtaken-line]"
          ),
        ]
          .at(-1)
          ?.hasAttribute("data-overtaken-line") ?? false
    ),
    "the Plan view's overtaken line does not stand after every step"
  ).toBe(true);
  await expectNoRawValue(page, "under Plan");

  await gotoIssue(page, presetPath(key, "spec"));
  const statement7 = nodeRow(page, treeId(ids.statement7)).first();
  await expect(statement7, "the Spec view draws no row for statement 7").toHaveCount(1);
  expect(await ownMarks(statement7), "statement 7 does not carry Proposed under Spec").toEqual([
    PROPOSED,
  ]);
  await expect
    .poll(() => filterCount(page, "spec"), {
      message: "the Spec control does not count every statement",
    })
    .toBe(statements);
  await expectNoRawValue(page, "under Spec");
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
  const statement7 = await drawnRowId(page, ids.statement7, "statement 7");
  const row = await focusRow(page, statement7);
  const settle = row.locator('[data-row-act="settle"]');
  await pressFTo(page, settle, "the focused row offers Settle");
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
  expect(blockOf(settles[0] ?? ""), "the settle does not name statement 7's address").toBe(
    ids.statement7
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
    .toBe(statementsOf(seeded) - 1);

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

// SLOW(>=2s): a whole seeded issue with nine node states and a lens written, every branch opened
// three times, the line pressed and keyed open and shut, eleven loads of the outline and the views,
// one settle and a reload.
test("a proposed node is marked and settled on the page, and an overtaken one folds away @phone", async ({
  browser,
}) => {
  test.setTimeout(480_000);
  const size = rowSize();
  await phase(browser, seedGateIssue, async (page, gateIssue) => {
    owedUnless(await treeReadGate(page, gateIssue));
    owedUnless(await convertedGate(page, gateIssue));
    const whole = await seedWholeIssue();
    const key = whole.key;
    const seeded = await seedNodeStates(whole);
    const plain = presetPath(key, "conversation");
    await page.setViewportSize(size);
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    await gotoSettled(page, traffic, plain);
    // (5) The load sends no request of the node state routes: a node's life is the tree read's.
    expect(
      nodeStateRequests(traffic.requests),
      "the load sends a request of the node state routes"
    ).toEqual([]);
    const answer = await heldTreeAnswer(tree, "the load", (held) =>
      held.nodes.some((node) => node.id === seeded.ids.statement7 && node.state === "proposed")
    );
    const read: ProposedPage = {
      answer,
      key,
      plain,
      seeded,
      traffic,
      treeId: treeIdOf(answer, await treeIdReads(key)),
      wide: size.width === DESKTOP.width,
    };
    await expectProposedMarks(page, read);
    await expectProposedInDetails(page, read);
    await expectOvertakenFold(page, read);
    await expectOvertakenAddresses(page, read);
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

// SLOW(>=2s): a whole seeded issue with nine node states and a lens written, the outline's load
// waited out, and three settles each refused and read.
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
      held.nodes.some((node) => node.id === seeded.ids.statement7 && node.state === "proposed")
    );
    await expectLoadSettled(traffic);
    const statement7 = await drawnRowId(page, seeded.ids.statement7, "statement 7");
    const row = await focusRow(page, statement7);
    const settle = row.locator('[data-row-act="settle"]');
    const answer = row.locator("[data-settle-answer]");
    await pressFTo(page, settle, "the focused row offers Settle");

    // Bob settles it first, through the server; alice's page still draws it proposed.
    await settleNodeApi(key, seeded.addresses.statement7, "bob");
    traffic.reset();
    await page.keyboard.press("Enter");
    await expect(answer, "a settle of a node no longer proposed does not say so").toHaveText(
      SETTLE_ANSWERS.not_proposed
    );
    await expect(answer, "the refusal's sentence is no status").toHaveAttribute("role", "status");
    expect(await ownMarks(row), "the refused settle takes statement 7's mark").toEqual([PROPOSED]);
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
        expect(
          await ownMarks(row),
          `the ${refusal.status} ${refusal.body.code} takes statement 7's mark`
        ).toEqual([PROPOSED]);
      } finally {
        await page.unroute(SETTLE_ROUTE, refuse);
      }
    }
  });
});

// ---------------------------------------------------------------------------------------------
// Row 3: a node reserved for its author.
// ---------------------------------------------------------------------------------------------

/** What one reader's page draws of the issue, and the reads it drew it from. */
interface ReaderReading {
  /** The reader's own tree read. */
  readonly answer: IssueTreeAnswer;
  /** The read id of every row drawn with every body and every branch open. */
  readonly drawn: ReadonlySet<string>;
  /** Each filter control's count, read with that filter on. */
  readonly filters: Readonly<Record<(typeof VIEWS)[number], number>>;
  /** Each folded row's count on load. */
  readonly folded: readonly FoldedCount[];
  /** The page's text with every body and every branch open, and each view's. */
  readonly texts: readonly { readonly text: string; readonly where: string }[];
}

/**
 * Reads issue `key` on `page` as its reader, with `traffic`, the page's one watcher: the load's
 * folded counts, each beside the count the reader's own tree read makes; the outline with `z` to
 * every body open and every deeper branch open; and each view, its control's count beside the
 * count its own view read makes where the page holds that read.
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
  await expect
    .poll(
      async () =>
        (await foldedCounts(page)).flatMap((row) =>
          row.readId === null || row.count === heldUnder(answer, key, row.readId) ? [] : [row.id]
        ),
      { message: `${who}'s folds do not count what ${who}'s tree read holds under them` }
    )
    .toEqual([]);
  const folded = await foldedCounts(page);
  await focusThePage(page);
  await page.keyboard.press("z");
  await page.keyboard.press("z");
  await openEveryDeeperBranch(page);
  const drawn = new Set(
    (await treeItemsDrawn(page)).flatMap((item) => (item.readId === null ? [] : [item.readId]))
  );
  const texts = [{ text: await issuePage(page).innerText(), where: "the outline" }];
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
          return Number.isNaN(count);
        },
        { message: `${who}'s ${name} control draws no count` }
      )
      .toBe(false);
    filters[view] = count;
    texts.push({ text: await issuePage(page).innerText(), where: `the ${name} view` });
  }
  await pickPreset(page, PRESET_NAMES.conversation);
  return { answer, drawn, filters, folded, texts };
}

/** The read id of the node of `answer` that holds `word`; throws, naming `what`, where none
 *  does. */
function readIdHolding(answer: IssueTreeAnswer, word: string, what: string): string {
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
 *  has gone quiet (`gotoSettled`), with every body and every deeper branch open. */
async function jWalk(page: Page, traffic: ApiRequests, plain: string): Promise<string[]> {
  await gotoSettled(page, traffic, plain);
  await focusThePage(page);
  await page.keyboard.press("z");
  await page.keyboard.press("z");
  await openEveryDeeperBranch(page);
  // The branches are all open once openEveryDeeperBranch returns, so every row is drawn by now.
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
 *  row's facts, beside the count `answer` makes; null where it is not read. */
async function statementTwoReferences(
  page: Page,
  key: string,
  statementTwo: string,
  answer: IssueTreeAnswer,
  who: string
): Promise<number | null> {
  if (!noteOwed(FOCUSED_FACTS_GATE)) return null;
  await gotoIssue(page, presetPath(key, "conversation"));
  await focusRow(page, await drawnRowId(page, statementTwo, "statement 2"));
  await openDetails(page);
  const count = await referencedByCount(page);
  expect(count, `${who}'s Referenced by of statement 2 is not what ${who}'s read holds`).toBe(
    referencesTo(answer, statementTwo)
  );
  return count;
}

/** Bob's readings of the issue alice reserved nodes on, on his page with `traffic`, its one
 *  watcher: none of them, nor any trace of them. */
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
  await gotoIssue(page, plain);
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

  // The project risk under her reserved pre-mortem stands under statement 3, after its children,
  // with no mark, no gap and no line about a pre-mortem.
  await focusThePage(page);
  const projectRisk = readIdHolding(his.answer, seeded.nodes.projectRisk.word, "project risk");
  const riskId = await drawnRowId(page, projectRisk, "the project risk");
  const statementThree = await drawnRowId(page, seeded.whole.requirements[2] ?? "", "statement 3");
  expect(
    await parentOfRow(page, riskId),
    "bob's project risk does not stand under statement 3"
  ).toBe(statementThree);
  const items = await treeItemsDrawn(page);
  const statementThreeItem = items.find((item) => item.id === statementThree);
  expect(
    childItems(items, statementThreeItem).at(-1)?.readId,
    "bob's project risk does not stand after statement 3's other children"
  ).toBe(projectRisk);
  const riskRow = nodeRow(page, riskId).first();
  expect(await ownMarks(riskRow), "bob's project risk carries a mark").toEqual([]);
  await expect(rowGaps(riskRow), "bob's project risk draws a gap").toHaveCount(0);
  expect(
    (await lensLines(riskRow))
      .map((line) => line.line)
      .filter((line) => !RISK_LINES.includes(line)),
    "bob's project risk draws a line about a pre-mortem"
  ).toEqual([]);

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
    const references = await statementTwoReferences(page, key, statementTwo, his.answer, "bob");
    await expectNothingReserved(page, traffic, seeded, reserved, his);
    return { his, references };
  } finally {
    await context.close();
  }
}

// SLOW(>=2s): a whole seeded issue with seven lens nodes written, two readers each reading the
// outline with every branch open and each of the four views, five words filters, a walk of every
// row and three loads by `?node=`.
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
    const hers = await treeReadAs(key, "alice");
    expect(hers.converted, "the seeded issue's tree read does not answer converted").toBe(true);
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
    await page.setViewportSize(DESKTOP);
    const traffic = watchApiRequests(page);
    const her = await readAsReader(page, traffic, key, "alice");
    for (const name of RESERVED_NODES) {
      const row = await drawnRow(page, reserved.get(name) ?? "", `her reserved ${name}`);
      expect(await ownMarks(row), `her reserved ${name} does not carry Only you`).toEqual([
        ONLY_YOU,
      ]);
    }
    const projectLens = readIdHolding(her.answer, seeded.nodes.projectLens.word, "project lens");
    const lensId = await drawnRowId(page, projectLens, "the project lens");
    const lensRow = await focusRow(page, lensId);
    const triggered = (await lensLines(lensRow)).find((line) => line.line === "triggered_by");
    expect(
      { label: triggered?.label, chips: triggered?.chips.length },
      "her project lens does not read Triggered by with one chip"
    ).toEqual({ chips: 1, label: LENS_WORDS.triggered_by });
    expect(
      await compactMarks(lensRow.locator('[data-lens-line="triggered_by"] [data-node-chip]')),
      "the chip of her reserved lens does not carry Only you in its compact form"
    ).toEqual([ONLY_YOU]);

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
          her.answer.tuples.filter(
            (tuple) => tuple.to === statementTwo && reservedIds.has(tuple.from)
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
