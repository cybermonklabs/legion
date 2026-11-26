/** The server decides each move, delete, dry run and refusal of Graph's node menu, the order its
 *  bands stand in, the drain of a write into the spec and Verification queries, the version that
 *  last held a deleted node (`last_held`) and when the block index places a new block, so these
 *  rows read Graph's editing against the real stack (decision 427). */

import type { IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { CONTROL_LABELS, LENS_WORDS } from "../web/src/features/issue-page/page-words";
import { createComment, editArtifact, getComment } from "./api";
import { holdFirstRequest } from "./editor";
import { gotoIssue, issuePage, phase } from "./issue-page";
import { DESKTOP, PHONE, rowWords } from "./issue-page-g";
import {
  approvalWords,
  approveDocuments,
  asArchitect,
  cancelDelete,
  caretInARow,
  caretRow,
  caretRowReadId,
  childIds,
  closeRowMenu,
  confirmDelete,
  confirmDialog,
  confirmWords,
  countOf,
  criterionEditorGate,
  deleteNode,
  deleteRouteGate,
  deleteState,
  deleteWords,
  documentVersion,
  dryRuns,
  EDIT_LABELS,
  EDIT_LINES,
  emptyParagraphs,
  expectApprovalChangedSince,
  expectApprovalWordsChanged,
  expectApproved,
  expectGrey,
  expectMenusFit,
  expectOffered,
  focusOn,
  fragmentTree,
  gateDesignSteps,
  graphEditIds,
  holdsTuple,
  insertInto,
  landOn,
  MENU_ENTRIES,
  MOVE_ENTRIES,
  markDocument,
  member,
  menuControl,
  menuEntry,
  menuWords,
  moveNode,
  type NodeRequest,
  nextTag,
  nodeOf,
  nodeWhere,
  nodeWrites,
  nthWrite,
  openRowMenu,
  parentReadIdOf,
  pickByKey,
  proseRowGate,
  rankedProse,
  readRow,
  refusalLine,
  roomWriteGate,
  rowMenu,
  rowSubmenu,
  rowUnder,
  SEED_WORDS,
  seedGraphEdit,
  submenuEntry,
  submenuWords,
  treeAfterReload,
  treeOf,
  watchNodeRequests,
  writtenByAlice,
} from "./issue-page-r";
import { editNoticesSent, type LiveFrames, watchLiveFrames } from "./issue-page-t-rows";
import { owedUnless, seedEventsLanded, treeReadRoute, treeReads } from "./issue-page-t-tree";
import { focusedRowReadId, lensAct, lensCatalogue, requestedTreeRead } from "./issue-page-w";
import { pauseClock } from "./page-clock";
import { settledValue } from "./settled";
import { expect, type Page, type Route, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * The browser rows of part R of the new issue page (INFRA-5705): Graph's node menu, its moves by
 * the menu and by key, its delete with the confirm that names the count, its new rows and Retype,
 * the greys of every act a node does not take, the refusals the server gives, the link of a
 * reference to a node a delete took, and the requests the editing adds - one row for each title
 * the Verification document's browser checks name, each run by its check's `-g`.
 *
 * Each row makes its own seed (`seedGraphEdit`, `e2e/issue-page-r.ts`) through the server's own
 * routes, turns the new page on for alice with `phase` (`e2e/issue-page.ts`), and names each node
 * by the id the server's tree read gives it (`graphEditIds`). It reads what the server keeps with
 * the tree read after a reload (`treeAfterReload`), what the page sent to the node routes with
 * `watchNodeRequests` and every other request with `watchApiRequests` (`e2e/traffic.ts`), each
 * count read once the reads earlier events caused have settled (`seedEventsLanded`,
 * `expectLoadSettled`, and `settledValue` of `e2e/settled.ts`, decision 274). A row reaches each
 * node by its own address (`landOn`, `?node=`), which lands on its row at 1440 px and at 390 px
 * alike. A `@phone` row reads 1440 px on `chromium` and 390 px on `iphone` (`rowSize`); every
 * other row reads 1440 by 900, the size `phase` opens at.
 *
 * Rows check prerequisites through `owedUnless` (`e2e/issue-page-t-tree.ts`). When one is absent
 * the row skips with an `owed` annotation, not a pass. The gates are `e2e/issue-page-r.ts`'s:
 * `deleteRouteGate` for every row that
 * drives the `DELETE` or its dry run, `roomWriteGate` for every row whose new row, retype or key
 * writes through the issue's one room (part Y's editor, stage B), and `proseRowGate` for every row
 * that reads the seeded prose block. One gate stands later in its rows: `criterionEditorGate`
 * (part Y's criterion row and its block editor) stands in c-move's first row and c-grey's first
 * row right before the first assertion that needs part Y, so every assertion above it runs. The
 * words a row reads are `page-words-r.ts`'s and the page's shared tables', and no module of
 * `graph-edit/` is imported, so this file loads before that code.
 *
 * Each step a check's negative control names is an `expect` message of its row, word for word, so
 * a control fails its row on that step. Two readings depart from the checks' words, each where it
 * stands: the fragment's text is read from `GET /api/v1/artifacts/{tree}/text` (its
 * `markdown_tree`), since the artifact's own read carries no text, and c-menu-keys' clause on the
 * page's key list naming `m` waits on cybermonklabs/legion#4304, which no gate of these rows reads.
 */

/** The size a `@phone` row reads: 390 px on the `iphone` project, 1440 px on every other. */
function rowSize(): { readonly height: number; readonly width: number } {
  return test.info().project.name === "iphone" ? PHONE : DESKTOP;
}

/** The words each new row is typed with, each unique in its store. */
const TYPED = {
  belowCriterion: "The first new row, below the criterion.",
  belowProse: "The second new row, below the prose block.",
  belowStatement: "The fourth new row, below the first statement.",
  insideStatement: "A second paragraph of the first statement.",
  proseUnderIssue: "A paragraph the row writes under the issue.",
  retypedCriterion: "A paragraph that becomes a criterion.",
  retypedStatement: "A paragraph that becomes a statement.",
  underProse: "The third new row, under the prose block.",
} as const;

/** Waits until the caret is in a new row: in a row's text, where the row holds no read id yet. */
async function expectCaretInNewRow(page: Page, message: string): Promise<void> {
  await expect
    .poll(async () => ({ inRow: await caretInARow(page), readId: await caretRowReadId(page) }), {
      message,
    })
    .toEqual({ inRow: true, readId: null });
}

/** How long a new block takes to be indexed and placed: the block index reads a room write on
 *  the tree artifact's version, and the page's `PATCH` follows the read that holds its rank. */
const PLACED_MS = 90_000;

/**
 * Types `words` in the new row that holds the caret, leaves the caret for the row with Escape,
 * and waits until the page's `PATCH` that places the new block, the write after the `from`
 * writes `sent` held as the row was made, has been taken. Answers the new node's read id, the
 * `{id}` that `PATCH` names.
 */
async function typeNewRow(
  page: Page,
  sent: readonly NodeRequest[],
  from: number,
  words: string,
  what: string
): Promise<string> {
  await page.keyboard.type(words);
  await page.keyboard.press("Escape");
  const placed = await nthWrite(
    sent,
    from + 1,
    `the page sends no PATCH that places ${what}`,
    PLACED_MS
  );
  expect((await placed.answer)?.status, `the PATCH that places ${what} is refused`).toBe(200);
  return placed.id;
}

/** Makes a new row under the focused node from its menu, typed with `words` and placed, and
 *  answers its read id. */
async function newRowUnder(
  page: Page,
  sent: readonly NodeRequest[],
  words: string,
  what: string
): Promise<string> {
  const from = nodeWrites(sent).length;
  await openRowMenu(page, "press", what);
  await menuEntry(page, EDIT_LABELS.edit_new_under).click();
  await expectCaretInNewRow(page, `New row under on ${what} puts the caret in no new row`);
  return typeNewRow(page, sent, from, words, `the new row under ${what}`);
}

/** The `parent` a move's body names, or undefined where it names none. */
function parentOf(write: NodeRequest): unknown {
  return member(write.body, "parent");
}

/** A move's body as R4 builds it, for one comparison: its `parent` and its `after`, `null` where
 *  the body names `after: null` and undefined where it names none. */
function placeOf(write: NodeRequest): { readonly after: unknown; readonly parent: unknown } {
  return { after: member(write.body, "after"), parent: parentOf(write) };
}

// ---------------------------------------------------------------------------------------------
// c-new-node
// ---------------------------------------------------------------------------------------------

/** One new row, and what the server is to keep of it after the reload: R3's place. */
interface NewRow {
  readonly after: string | null;
  readonly id: string;
  /** The message its kind is read under. */
  readonly kind: string;
  readonly parent: string;
  /** The `parent` its one placing `PATCH` names: `parent` in the node route's form, a block by
   *  its read id and any other node by `<kind>/<read id>`, as `ask/<id>` for the question. */
  readonly patchParent: string;
  readonly what: string;
  readonly words: string;
}

/** Holds `row` to the place R3 names and to its author: alice wrote the words typed, under the
 *  parent R3 names and right after the sibling it names, placed by one `PATCH` of the node. */
function expectPlaced(answer: IssueTreeRead, sent: readonly NodeRequest[], row: NewRow): void {
  const node = nodeOf(answer, row.id);
  expect(node?.text, `${row.what} holds the words typed`).toContain(row.words);
  expect(writtenByAlice(node), `${row.what} names alice, the person signed in, as its author`).toBe(
    true
  );
  expect(node?.parent?.id, `${row.what} stands under the node R3 names`).toBe(row.parent);
  const siblings = childIds(answer, row.parent);
  expect(
    siblings[siblings.indexOf(row.id) - 1] ?? null,
    `${row.what} stands right after the sibling R3 names`
  ).toBe(row.after);
  expect(
    nodeWrites(sent)
      .filter((write) => write.id === row.id)
      .map((write) => ({ method: write.method, parent: parentOf(write), rest: write.rest })),
    `${row.what} is placed by one PATCH of the node`
  ).toEqual([{ method: "PATCH", parent: row.patchParent, rest: "" }]);
}

// c-new-node: Enter at the end of the criterion's text, New row below and New row under on the
// prose block, New row below on the first statement, each typed in and placed by one PATCH; the
// greys of New row below, Enter at the end of a check's text, Escape before the first character,
// Shift+Enter inside a statement, and a new row's greys until it is placed.
// SLOW(>=2s): eleven landings, four new rows each typed and placed through the block index's read
// of the room, a held tree read, and a reload.
test("a new node below and under is a prose block the reader types @phone", async ({ browser }) => {
  test.setTimeout(600_000);
  const size = rowSize();
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await roomWriteGate(page, seed.key));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const [statement1, , statement3] = ids.statements;
    await page.setViewportSize(size);
    const sent = watchNodeRequests(page, key);

    // Enter with the caret at the end of the criterion's text makes a new row below it.
    await landOn(page, key, ids.criterion, "the criterion");
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(page), { message: "Enter puts no caret in the criterion's text" })
      .toBe(ids.criterion);
    await page.keyboard.press("End");
    let from = nodeWrites(sent).length;
    await page.keyboard.press("Enter");
    await expectCaretInNewRow(page, "Enter at the end of the criterion's text draws no new row");
    const belowCriterion = await typeNewRow(
      page,
      sent,
      from,
      TYPED.belowCriterion,
      "the new row below the criterion"
    );

    // New row below on the prose block, a block of the issue's own fragment: right after it.
    await landOn(page, key, ids.prose, "the prose block");
    from = nodeWrites(sent).length;
    await openRowMenu(page, "press", "the prose block");
    await menuEntry(page, EDIT_LABELS.edit_new_below).click();
    await expectCaretInNewRow(page, "New row below puts the caret in no new row");
    const belowProse = await typeNewRow(
      page,
      sent,
      from,
      TYPED.belowProse,
      "the new row below the prose block"
    );

    // New row under on the prose block, with the tree read held: until a read places it, the new
    // row's moves and its New row below are grey and say that the block is not placed yet, and
    // the page sends nothing for them.
    await landOn(page, key, ids.prose, "the prose block");
    from = nodeWrites(sent).length;
    const held = Promise.withResolvers<void>();
    const hold = async (route: Route) => {
      await held.promise;
      await route.continue();
    };
    // The hold takes the tree reads that name no view, Graph's among them: the editor of the tree
    // artifact opens its room only once the issue's view read answers (`?view=spec`,
    // `issueViewOfQuery`, which `ProofDocument.tsx`'s admission waits for), so a hold on that read
    // put no caret in the new row. The hold stays routed once released, and passes each later
    // read through: unrouting it here raced the read it released, whose `route.continue` then
    // failed as already handled.
    const treeRead = treeReadRoute(key);
    await page.route((url) => treeRead(url) && !url.searchParams.has("view"), hold);
    try {
      await openRowMenu(page, "press", "the prose block");
      await menuEntry(page, EDIT_LABELS.edit_new_under).click();
      await expectCaretInNewRow(page, "New row under puts the caret in no new row");
      await page.keyboard.type(TYPED.underProse.slice(0, 1));
      await page.keyboard.press("Escape");
      await openRowMenu(page, "m", "the new row under the prose block");
      for (const word of [...MOVE_ENTRIES, EDIT_LABELS.edit_new_below]) {
        await expectGrey(
          page,
          word,
          EDIT_LINES.grey_not_placed,
          `${word} is grey on a new row not placed yet, and says it is not placed yet`
        );
      }
      await pickByKey(page, EDIT_LABELS.edit_indent);
      await expect(
        rowMenu(page),
        "a pick of a grey entry picks nothing and keeps the menu"
      ).toBeVisible();
      expect(nodeWrites(sent).length, "a grey move of a new row sends a request").toBe(from);
      await closeRowMenu(page, "the new row under the prose block");
      await page.keyboard.press("Enter");
      // The letters typed next go into the block once the caret is back in it: until then the
      // focus is the row's, whose keys move through the outline.
      await expectCaretInNewRow(
        page,
        "Enter on the new row under the prose block puts no caret in it"
      );
      await page.keyboard.press("End");
    } finally {
      held.resolve();
    }
    const underProse = await typeNewRow(
      page,
      sent,
      from,
      TYPED.underProse.slice(1),
      "the new row under the prose block"
    );

    // New row below on the first statement, a spec block with two spec blocks after it: the new
    // row stands after the third statement, and says that it stands after the spec's blocks.
    await landOn(page, key, statement1, "the first statement");
    from = nodeWrites(sent).length;
    await openRowMenu(page, "press", "the first statement");
    await menuEntry(page, EDIT_LABELS.edit_new_below).click();
    await expectCaretInNewRow(page, "New row below on the first statement draws no new row");
    await expect(
      caretRow(page),
      "the new row says it stands after the spec's blocks"
    ).toContainText(EDIT_LINES.spec_band);
    const belowStatement = await typeNewRow(
      page,
      sent,
      from,
      TYPED.belowStatement,
      "the new row below the first statement"
    );

    // New row below is grey on the check and on the plan task, with its reason.
    for (const [id, what] of [
      [ids.checkShared, "the shared check"],
      [ids.planTask, "the plan task"],
    ] as const) {
      await landOn(page, key, id, what);
      await openRowMenu(page, "press", what);
      await expectGrey(
        page,
        EDIT_LABELS.edit_new_below,
        EDIT_LINES.grey_below_document,
        `New row below is grey on ${what}, with its reason`
      );
      await closeRowMenu(page, what);
    }

    // Enter at the end of the check's text adds no row; Escape on a new row before its first
    // character leaves nothing; Shift+Enter at the end of the first statement's text adds a
    // paragraph inside it. None of the three adds a node.
    const writes = nodeWrites(sent).length;
    const nodes = (await requestedTreeRead(page, key)).nodes?.length ?? 0;
    await landOn(page, key, ids.checkShared, "the shared check");
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(page), { message: "Enter puts no caret in the check's text" })
      .toBe(ids.checkShared);
    await page.keyboard.press("End");
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(page), {
        message: "Enter at the end of the check's text adds a row",
      })
      .toBe(ids.checkShared);
    await page.keyboard.press("Escape");
    await landOn(page, key, ids.prose, "the prose block");
    await openRowMenu(page, "press", "the prose block");
    await menuEntry(page, EDIT_LABELS.edit_new_below).click();
    await expectCaretInNewRow(page, "New row below puts the caret in no new row");
    await page.keyboard.press("Escape");
    await landOn(page, key, statement1, "the first statement");
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(page), { message: "Enter puts no caret in the statement's text" })
      .toBe(statement1);
    await page.keyboard.press("End");
    await page.keyboard.press("Shift+Enter");
    await page.keyboard.type(TYPED.insideStatement);
    await page.keyboard.press("Escape");
    await expect
      .poll(async () => nodeOf(await requestedTreeRead(page, key), statement1)?.text ?? "", {
        message: "Shift+Enter at the end of the first statement adds no paragraph inside it",
        timeout: PLACED_MS,
      })
      .toContain(TYPED.insideStatement);

    const answer = await treeAfterReload(page, key);
    expect(
      answer.nodes?.length,
      "Enter at the end of the check's text, Escape on a new row or Shift+Enter adds a node"
    ).toBe(nodes);
    expect(nodeWrites(sent).length, "Enter at the end of the check's text sends a write").toBe(
      writes
    );
    const rows: readonly NewRow[] = [
      {
        after: ids.criterion,
        id: belowCriterion,
        kind: "the new node below the criterion is a prose block",
        parent: ids.question,
        patchParent: `ask/${ids.question}`,
        what: "the new row below the criterion",
        words: TYPED.belowCriterion,
      },
      {
        after: ids.prose,
        id: belowProse,
        kind: "the new node below the prose block is a prose block",
        parent: ids.criterion,
        patchParent: ids.criterion,
        what: "the new row below the prose block",
        words: TYPED.belowProse,
      },
      {
        after: null,
        id: underProse,
        kind: "the new node under the prose block is a prose block",
        parent: ids.prose,
        patchParent: ids.prose,
        what: "the new row under the prose block",
        words: TYPED.underProse,
      },
      {
        after: statement3,
        id: belowStatement,
        kind: "the new node below the first statement is a prose block",
        parent: ids.criterion,
        patchParent: ids.criterion,
        what: "the new row below the first statement",
        words: TYPED.belowStatement,
      },
    ];
    for (const row of rows) expect(nodeOf(answer, row.id)?.supertag, row.kind).toBe("block");
    for (const row of rows) {
      await gotoIssue(page, `${presetPath(key, "graph")}?node=${encodeURIComponent(row.id)}`);
      await expect
        .poll(() => focusedRowReadId(page), {
          message: "the new block is drawn after the reload",
          timeout: 30_000,
        })
        .toBe(row.id);
    }
    for (const row of rows) expectPlaced(answer, sent, row);
    expect(
      emptyParagraphs(await fragmentTree(page, seed.tree)),
      "the fragment holds no empty paragraph"
    ).toBe(0);
  });
});

// ---------------------------------------------------------------------------------------------
// c-new-node-wait
// ---------------------------------------------------------------------------------------------

/** How many of the page's 30 s periods fall inside its 2 minutes of reads: the read at the 2
 *  minutes themselves is the bound's, which the row reads neither way. And how many periods the
 *  row drives past the 2 minutes. */
const PERIODS_INSIDE_TWO_MINUTES = 3;
const PERIODS_PAST = 2;

/** One of the page's periods, the block index's reconcile tick, on the page's own clock. */
const PERIOD_MS = 30_000;

/** How long `stepPeriod` waits for a frame of the live socket: two of the server's 15 s
 *  heartbeats. */
const FRAME_WAIT_MS = 30_000;

/**
 * Moves the page's paused clock on by one 30 s period, running every timer due in it, once the
 * live socket has carried a frame since the last move, then waits for the page to go quiet, so a
 * count read after it holds what that period sent. The page replaces a socket that carried no
 * frame for 45 s of its clock (`LIVE_SOCKET_SILENT_MS`), and a frame is stamped with the paused
 * clock's time, so with a frame since the last move none is ever more than 30 s old to the page.
 */
async function stepPeriod(page: Page, traffic: ApiRequests, live: LiveFrames): Promise<void> {
  await expect
    .poll(() => live.at !== undefined, {
      message: `the page's live socket carried no frame within ${FRAME_WAIT_MS / 1000} s`,
      timeout: FRAME_WAIT_MS,
    })
    .toBe(true);
  live.at = undefined;
  await page.clock.runFor(PERIOD_MS);
  await expectLoadSettled(traffic);
}

// c-new-node-wait: a new row whose block the index owes, read once on its version event and then
// once each 30 s of the page's clock for 2 minutes, its moves grey until then, and the line past
// the 2 minutes; after the reload it stands where the server keeps it, and no PATCH is sent.
// SLOW(>=2s): a landing, a new row typed with every tree read answered as owed, six 30 s periods of
// the page's clock each held until the live socket carries a frame, and a reload.
test("a new node the index owes is read again every 30 seconds for 2 minutes", async ({
  browser,
}) => {
  test.setTimeout(480_000);
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await roomWriteGate(page, seed.key));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const sent = watchNodeRequests(page, key);
    const traffic = watchApiRequests(page);
    const live = watchLiveFrames(page);
    const before = new Set((await requestedTreeRead(page, key)).nodes?.map((node) => node.id));
    const version = await documentVersion(page, seed.tree);

    // Each tree read is answered with the tree artifact owed and no new node, as the server
    // answers while the block index has not read the room's write yet.
    const owed = async (route: Route) => {
      const response = await route.fetch();
      const answer = (await response.json()) as IssueTreeRead;
      await route.fulfill({
        json: {
          ...answer,
          nodes: (answer.nodes ?? []).filter((node) => before.has(node.id)),
          owed_documents: [
            ...(answer.owed_documents ?? []),
            { artifact_id: seed.tree, version: version + 1 },
          ],
        },
        response,
      });
    };
    await page.clock.install();
    await landOn(page, key, ids.prose, "the prose block");
    await seedEventsLanded(page);
    await expectLoadSettled(traffic);
    // One matcher for the route and its unroute (`treeReadRoute`), so the reload below reads the
    // server's own answer.
    const treeRead = treeReadRoute(key);
    await page.route(treeRead, owed);
    await openRowMenu(page, "press", "the prose block");
    await menuEntry(page, EDIT_LABELS.edit_new_below).click();
    await expectCaretInNewRow(page, "New row below puts the caret in no new row");
    traffic.reset();
    // The clock runs until the room has written the first character and its version event has
    // read the tree, so the write the editor sends on a timer of its own goes out.
    await page.keyboard.type("Q");
    await page.keyboard.press("Escape");
    await expect
      .poll(() => treeReads(traffic.requests, key, null), {
        message: "the tree artifact's version event reads the tree once",
        timeout: 30_000,
      })
      .toBeGreaterThanOrEqual(1);
    await pauseClock(page);
    await expectLoadSettled(traffic);
    expect(
      treeReads(traffic.requests, key, null),
      "the tree artifact's version event reads the tree once"
    ).toBe(1);

    for (let period = 1; period <= PERIODS_INSIDE_TWO_MINUTES; period += 1) {
      await stepPeriod(page, traffic, live);
      expect(treeReads(traffic.requests, key, null), "one read each 30 seconds").toBe(1 + period);
    }
    await openRowMenu(page, "m", "the new row");
    for (const word of MOVE_ENTRIES) {
      await expectGrey(
        page,
        word,
        EDIT_LINES.grey_not_placed,
        `${word} is grey on the new row the index owes, and says it is not placed yet`
      );
    }
    await closeRowMenu(page, "the new row");
    await stepPeriod(page, traffic, live);
    const atTwoMinutes = treeReads(traffic.requests, key, null);
    for (let period = 1; period <= PERIODS_PAST; period += 1) {
      await stepPeriod(page, traffic, live);
      expect(treeReads(traffic.requests, key, null), "no read after 2 minutes").toBe(atTwoMinutes);
    }
    await expect(
      issuePage(page).locator("[data-node-row]:focus"),
      "the row says the block is saved and not yet placed"
    ).toContainText(EDIT_LINES.saved_not_placed);
    expect(nodeWrites(sent), "the page sends a PATCH for a block the index owes").toEqual([]);

    await page.unroute(treeRead, owed);
    // The clock the periods were driven on is the context's, and a reload keeps it: held still,
    // it would hold every timer of the reloaded page, the query cache's own notice that a read
    // answered among them (`notifyManager` tells on a `setTimeout`), so the page would read
    // `whoami` and draw nothing more. The clock runs on from here; every period is counted above.
    await page.clock.resume();
    const answer = await treeAfterReload(page, key);
    const made = (answer.nodes ?? []).filter((node) => !before.has(node.id));
    expect(
      made.map((node) => ({ parent: node.parent?.id, text: node.text })),
      "the server keeps one new block, under the issue"
    ).toEqual([{ parent: ids.issue, text: "Q" }]);
    const ownBlocks = (answer.nodes ?? []).filter(
      (node) =>
        node.parent?.id === ids.issue && node.kind === "block" && (node.document ?? null) === null
    );
    expect(
      ownBlocks.at(-1)?.id,
      "the new block stands at the end of the issue's own blocks under the issue"
    ).toBe(made[0]?.id);
    // The node is drawn where the server keeps it: Graph at its own address names no node and no
    // zoom, so no address the reload kept roots the outline elsewhere, and the new block's row is
    // drawn among the rows under the issue.
    await gotoIssue(page, presetPath(key, "graph"));
    await expect(
      readRow(page, made[0]?.id ?? "").first(),
      "the new block is drawn after the reload"
    ).toBeVisible();
    expect(
      await parentReadIdOf(page, made[0]?.id ?? ""),
      "the new block is drawn under a row, not under the issue"
    ).toBeNull();
    await expectLoadSettled(traffic);
    expect(nodeWrites(sent), "the page sends a PATCH for the new block after the reload").toEqual(
      []
    );
  });
});

// ---------------------------------------------------------------------------------------------
// c-move
// ---------------------------------------------------------------------------------------------

/** One move a row picked, as the page is to send it and the server to keep it. */
interface Move {
  readonly after: string | null;
  readonly id: string;
  /** The message its status, and its tag where `tagged` is that message too, are read under. */
  readonly kept: string;
  readonly parent: string;
  /** The quoted `If-Match` it sends: the placement the page held, or the answer before it. */
  readonly tag: string;
  readonly tagged: string;
  readonly what: string;
}

/**
 * Holds the `count`th write the page sent to the move the row picked: one `PATCH` of the node's
 * own route, `If-Match` the quoted tag the move names, the body R4 names, and the move kept.
 * Answers the write.
 */
async function expectMove(
  sent: readonly NodeRequest[],
  count: number,
  move: Move
): Promise<NodeRequest> {
  const write = await nthWrite(sent, count, "each move is one PATCH of the node");
  expect(
    { id: write.id, method: write.method, rest: write.rest },
    "each move is one PATCH of the node"
  ).toEqual({ id: move.id, method: "PATCH", rest: "" });
  expect(write.ifMatch, move.tagged).toBe(move.tag);
  expect((await write.answer)?.status, move.kept).toBe(200);
  expect(placeOf(write), `${move.what} names R4's parent and after`).toEqual({
    after: move.after,
    parent: move.parent,
  });
  return write;
}

// c-move, first row: the menu's Indent and Outdent with no pause, then Move up and Move down, on
// the third statement; once the moves' drain reads have settled, Indent on the prose block, which
// one outline read follows; the agent's statement moved; and the grey edges, each with its reason.
// SLOW(>=2s): nine landings, six moves each answered by the server, the drain of five statement
// moves into the spec query waited out, and two reloads.
test("a block indents, outdents and moves from the menu @phone", async ({ browser }) => {
  test.setTimeout(480_000);
  const size = rowSize();
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await proseRowGate(page, seed.key));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const [statement1, statement2, statement3] = ids.statements;
    await approveDocuments(seed);
    await page.setViewportSize(size);

    // The grey edges.
    await landOn(page, key, statement1, "the first statement");
    await openRowMenu(page, "press", "the first statement");
    await expectGrey(
      page,
      EDIT_LABELS.edit_indent,
      EDIT_LINES.grey_no_sibling_above,
      "Indent is grey on the first statement"
    );
    await expectGrey(
      page,
      EDIT_LABELS.edit_move_up,
      EDIT_LINES.grey_first,
      "Move up is grey on the first statement"
    );
    await closeRowMenu(page, "the first statement");
    await landOn(page, key, statement3, "the third statement");
    await openRowMenu(page, "press", "the third statement");
    await expectGrey(
      page,
      EDIT_LABELS.edit_move_down,
      EDIT_LINES.grey_last,
      "Move down is grey on the last statement"
    );
    await closeRowMenu(page, "the third statement");
    // Keep the grey-edge assertions above this gate. The zoom needs part Y's criterion row.
    owedUnless(await criterionEditorGate(page, key));
    await landOn(page, key, ids.criterion, "the criterion");
    await page.keyboard.press(".");
    await focusOn(readRow(page, statement1).first(), "the first statement");
    await openRowMenu(page, "press", "the first statement, zoomed to the criterion");
    await expectGrey(
      page,
      EDIT_LABELS.edit_outdent,
      EDIT_LINES.grey_zoom_root,
      "Outdent is grey on the first statement while Graph is zoomed to the criterion"
    );
    await closeRowMenu(page, "the first statement");
    await landOn(page, key, statement2, "the second statement");
    await focusOn(
      await rowUnder(page, statement2, ids.checkShared, "the shared check"),
      "the shared check's second row"
    );
    await openRowMenu(page, "press", "the shared check's second row");
    for (const word of MOVE_ENTRIES) {
      await expectGrey(
        page,
        word,
        EDIT_LINES.grey_own_row,
        `${word} is grey on the shared check's second row, under the second statement`
      );
    }
    await closeRowMenu(page, "the shared check's second row");

    // Indent and then Outdent with no pause between them: both kept, the outdent sent with the
    // placement_seq the indent's answer gave. Then Move up and Move down.
    const held = await requestedTreeRead(page, key);
    const sent = watchNodeRequests(page, key);
    const traffic = watchApiRequests(page);
    await landOn(page, key, statement3, "the third statement");
    await markDocument(page);
    // The hold takes the first PATCH of the third statement on its own route or on one under it,
    // so a move sent another way is held and named by the route assertion below, before any
    // answer, rather than waited for until the row's guard.
    const indentWrite = await holdFirstRequest(
      page,
      new RegExp(`/api/v1/issues/${key}/nodes/${encodeURIComponent(statement3)}(?:/.*)?$`, "u"),
      { method: "PATCH" }
    );
    await openRowMenu(page, "press", "the third statement");
    await menuEntry(page, EDIT_LABELS.edit_indent).click();
    const sentIndent = await nthWrite(sent, 1, "each move is one PATCH of the node");
    expect(
      { id: sentIndent.id, method: sentIndent.method, rest: sentIndent.rest },
      "each move is one PATCH of the node"
    ).toEqual({ id: statement3, method: "PATCH", rest: "" });
    const pendingIndent = await indentWrite.held;
    try {
      await openRowMenu(page, "press", "the third statement");
      await menuEntry(page, EDIT_LABELS.edit_outdent).click();
    } finally {
      await pendingIndent.continue();
    }
    const indent = await expectMove(sent, 1, {
      after: null,
      id: statement3,
      kept: "each move is kept",
      parent: statement2,
      tag: `"${nodeOf(held, statement3)?.placement_seq}"`,
      tagged: "each move is kept",
      what: "the indent",
    });
    const outdent = await expectMove(sent, 2, {
      after: statement2,
      id: statement3,
      kept: "both moves are kept",
      parent: ids.criterion,
      tag: nextTag(await indent.answer),
      tagged: "both moves are kept",
      what: "the outdent",
    });
    await expectApprovalChangedSince(
      page,
      seed.spec,
      "after the first move the identity block reads the spec's approval as changed since"
    );
    await openRowMenu(page, "press", "the third statement");
    await menuEntry(page, EDIT_LABELS.edit_move_up).click();
    const up = await expectMove(sent, 3, {
      after: statement1,
      id: statement3,
      kept: "each move is kept",
      parent: ids.criterion,
      tag: nextTag(await outdent.answer),
      tagged: "each move is kept",
      what: "Move up",
    });
    await openRowMenu(page, "press", "the third statement");
    await menuEntry(page, EDIT_LABELS.edit_move_down).click();
    await expectMove(sent, 4, {
      after: statement2,
      id: statement3,
      kept: "each move is kept",
      parent: ids.criterion,
      tag: nextTag(await up.answer),
      tagged: "each move is kept",
      what: "Move down",
    });

    // The prose block's indent changes no query and moves no text, so it brings no event: once
    // the reads the statement moves' drain events caused have settled, one outline read follows
    // it, the page's own.
    await seedEventsLanded(page);
    await expectLoadSettled(traffic);
    await settledValue("the tree reads the statement moves' drain events caused", async () =>
      treeReads(traffic.requests, key, null)
    );
    const prose = nodeOf(await requestedTreeRead(page, key), ids.prose);
    await landOn(page, key, ids.prose, "the prose block");
    await expectLoadSettled(traffic);
    traffic.reset();
    await openRowMenu(page, "press", "the prose block");
    await menuEntry(page, EDIT_LABELS.edit_indent).click();
    await expectMove(sent, 5, {
      after: null,
      id: ids.prose,
      kept: "each move is kept",
      parent: statement3,
      tag: `"${prose?.placement_seq}"`,
      tagged: "each move is kept",
      what: "the prose block's indent",
    });
    await expectLoadSettled(traffic);
    expect(
      treeReads(traffic.requests, key, null),
      "one outline read follows the prose block's move"
    ).toBe(1);

    // After a reload the tree read holds each place as the row moved it.
    const answer = await treeAfterReload(page, key);
    expect(
      childIds(answer, ids.criterion).filter((id) => ids.statements.includes(id)),
      "the statements stand under the criterion in the order the moves left them"
    ).toEqual([statement1, statement2, statement3]);
    expect(
      nodeOf(answer, ids.prose)?.parent?.id,
      "the prose block stands under the third statement after the reload"
    ).toBe(statement3);

    // The person moves the agent's statement, and the move is kept.
    await landOn(page, key, statement2, "the second statement");
    await openRowMenu(page, "press", "the second statement");
    await expectOffered(page, EDIT_LABELS.edit_move_up, "the person moves the agent's statement");
    await menuEntry(page, EDIT_LABELS.edit_move_up).click();
    const agents = await nthWrite(sent, 6, "the person moves the agent's statement");
    expect((await agents.answer)?.status, "the person moves the agent's statement").toBe(200);
    expect(
      childIds(await treeAfterReload(page, key), ids.criterion)[0],
      "the person moves the agent's statement"
    ).toBe(statement2);
    expect(
      nodeWrites(sent).map((write) => `${write.method} ${write.rest}`),
      "each move is one PATCH of the node"
    ).toEqual(nodeWrites(sent).map(() => "PATCH "));
  });
});

// c-move, second row: Tab, Shift+Tab, Alt+Shift+Up and Alt+Shift+Down with the caret in the third
// statement, the caret kept in its text after each and the next character landing in it; Tab at
// a grey edge; and Tab in Conversation.
// SLOW(>=2s): three landings, four moves by key each answered, a character typed and stored after
// each, and Conversation's load.
test("a block indents, outdents and moves by key while its text is edited @phone", async ({
  browser,
}) => {
  test.setTimeout(360_000);
  const size = rowSize();
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await roomWriteGate(page, seed.key));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const [statement1, statement2, statement3] = ids.statements;
    await page.setViewportSize(size);
    const sent = watchNodeRequests(page, key);
    const traffic = watchApiRequests(page);

    await landOn(page, key, statement3, "the third statement");
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(page), { message: "Enter puts no caret in the third statement" })
      .toBe(statement3);
    await page.keyboard.press("End");
    let typed = "";
    for (const [index, move] of [
      { chord: "Tab", parent: statement2, what: "Tab indents the third statement" },
      { chord: "Shift+Tab", parent: ids.criterion, what: "Shift+Tab outdents it" },
      { chord: "Alt+Shift+ArrowUp", parent: ids.criterion, what: "Alt+Shift+Up moves it up" },
      { chord: "Alt+Shift+ArrowDown", parent: ids.criterion, what: "Alt+Shift+Down moves it down" },
    ].entries()) {
      await page.keyboard.press(move.chord);
      const write = await nthWrite(sent, index + 1, `${move.what}: the key sends no PATCH`);
      expect((await write.answer)?.status, `${move.what}: the move is refused`).toBe(200);
      await expect
        .poll(async () => nodeOf(await requestedTreeRead(page, key), statement3)?.parent?.id, {
          message: `${move.what} under the node the key names`,
        })
        .toBe(move.parent);
      await expect
        .poll(() => caretRowReadId(page), { message: "the caret stays in the node" })
        .toBe(statement3);
      const letter = ` ${"abcd".charAt(index)}`;
      typed += letter;
      await page.keyboard.type(letter);
      await expect
        .poll(async () => nodeOf(await requestedTreeRead(page, key), statement3)?.text ?? "", {
          message: `after ${move.what}, the next character typed lands in the statement's text`,
          timeout: 60_000,
        })
        .toContain(`${SEED_WORDS.statement3}${typed}`);
    }
    await page.keyboard.press("Escape");

    // Tab with the caret in the first statement writes nothing, keeps the caret, and shows
    // Indent's reason.
    await landOn(page, key, statement1, "the first statement");
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretRowReadId(page), { message: "Enter puts no caret in the first statement" })
      .toBe(statement1);
    const writes = nodeWrites(sent).length;
    await page.keyboard.press("Tab");
    await expect
      .poll(() => caretRowReadId(page), { message: "Tab on the first statement keeps the caret" })
      .toBe(statement1);
    await expect(
      readRow(page, statement1).first(),
      "Tab on the first statement shows Indent's reason"
    ).toContainText(EDIT_LINES.grey_no_sibling_above);
    await page.keyboard.press("Escape");

    // In Conversation, Tab with the caret in a statement keeps today's behaviour.
    await landOn(page, key, statement1, "the first statement", presetPath(key, "conversation"));
    await page.keyboard.press("Enter");
    await page.keyboard.press("Tab");
    await page.keyboard.press("Escape");
    await expectLoadSettled(traffic);
    expect(nodeWrites(sent).length, "Tab at the grey edge or in Conversation writes a node").toBe(
      writes
    );
  });
});

// ---------------------------------------------------------------------------------------------
// c-retype
// ---------------------------------------------------------------------------------------------

/** Opens the Retype submenu of the open menu, by a press on Retype, and waits until it is drawn. */
async function openRetype(page: Page, what: string): Promise<void> {
  await menuEntry(page, EDIT_LABELS.edit_retype).click();
  await expect(rowSubmenu(page), `Retype opens no submenu on ${what}`).toBeVisible();
}

// c-retype: a new prose block under the criterion retyped to a statement, and one under the
// question retyped to a criterion, each from the Retype submenu with no caret in the block; the
// greys of Retype and of its kinds; and the submenus of the view and of its ancestor.
// SLOW(>=2s): eleven landings, three new rows typed and placed, two of them in the converted
// issue's room and one in the child's, two retypes written through the room, a paragraph written
// under the issue, a comment written on the child, and the child's Graph loaded twice.
test("Retype gives a prose block another kind", async ({ browser }) => {
  test.setTimeout(480_000);
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await roomWriteGate(page, seed.key));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    await approveDocuments(seed);
    const sent = watchNodeRequests(page, key);

    // A new prose block under the criterion becomes a statement.
    await landOn(page, key, ids.criterion, "the criterion");
    const toStatement = await newRowUnder(page, sent, TYPED.retypedStatement, "the criterion");
    await landOn(page, key, toStatement, "the new prose block under the criterion");
    await markDocument(page);
    await openRowMenu(page, "m", "the new prose block under the criterion");
    await expectOffered(
      page,
      EDIT_LABELS.edit_retype,
      "Retype is offered on a prose block of one paragraph"
    );
    await openRetype(page, "the new prose block under the criterion");
    await expect(
      submenuEntry(page, EDIT_LABELS.retype_statement),
      "on a prose block of the first issue statement is offered"
    ).toBeVisible();
    await submenuEntry(page, EDIT_LABELS.retype_statement).click();
    await expect
      .poll(async () => nodeOf(await requestedTreeRead(page, key), toStatement)?.supertag, {
        message: "the retype keeps no node of the same id as a statement",
        timeout: 60_000,
      })
      .toBe("requirement");
    const retyped = await requestedTreeRead(page, key);
    expect(
      nodeOf(retyped, toStatement)?.parent?.id,
      "the statement stays under the criterion"
    ).toBe(ids.criterion);
    expect(
      holdsTuple(retyped, "serves", toStatement, ids.criterion),
      "the statement's serves names the criterion"
    ).toBe(true);
    await expect(
      readRow(page, toStatement).first(),
      "the row draws the new kind with the focus on it"
    ).toHaveAttribute("data-node-type", "requirement");
    await expect(
      readRow(page, toStatement).first(),
      "the retyped row keeps the focus"
    ).toBeFocused();
    await expectApprovalChangedSince(
      page,
      seed.spec,
      "after the retype the identity block reads the spec's approval as changed since"
    );

    // A new prose block under the question becomes a criterion, which stands under the question:
    // its `parent` names the question's ask, and it keeps the place its PATCH gave it under the
    // question's row, the ask's block of the spec, which a stored placement holds whatever the
    // attributes name.
    await landOn(page, key, ids.questionRow, "the question");
    const toCriterion = await newRowUnder(page, sent, TYPED.retypedCriterion, "the question");
    await landOn(page, key, toCriterion, "the new prose block under the question");
    await openRowMenu(page, "m", "the new prose block under the question");
    await openRetype(page, "the new prose block under the question");
    await submenuEntry(page, EDIT_LABELS.retype_criterion).click();
    await expect
      .poll(async () => nodeOf(await requestedTreeRead(page, key), toCriterion)?.supertag, {
        message: "the retype keeps no node of the same id as a criterion",
        timeout: 60_000,
      })
      .toBe("criterion");
    const criterionRead = await requestedTreeRead(page, key);
    expect(
      holdsTuple(criterionRead, "parent", toCriterion, ids.question),
      "the criterion stands under the question"
    ).toBe(true);
    expect(
      nodeOf(criterionRead, toCriterion)?.parent?.id,
      "the criterion keeps its place under the question's row"
    ).toBe(ids.questionRow);

    await landOn(page, key, ids.statements[0], "the first statement");
    await openRowMenu(page, "press", "the first statement");
    await expectGrey(
      page,
      EDIT_LABELS.edit_retype,
      EDIT_LINES.grey_not_paragraph,
      "Retype is grey on the statement"
    );
    await closeRowMenu(page, "the first statement");

    // A prose block under the issue: Retype's criterion is grey and names the parent it needs.
    await insertInto(seed.tree, "end", `${TYPED.proseUnderIssue}\n`);
    const underIssue = await rankedProse(key, TYPED.proseUnderIssue);
    await landOn(page, key, underIssue.id, "the prose block under the issue");
    await openRowMenu(page, "press", "the prose block under the issue");
    await openRetype(page, "the prose block under the issue");
    await expect(
      submenuEntry(page, EDIT_LABELS.retype_criterion),
      "on a prose block under the issue Retype's criterion is grey"
    ).toHaveAttribute("aria-disabled", "true");
    await expect(rowSubmenu(page), "Retype's criterion names the parent it needs").toContainText(
      EDIT_LINES.grey_criterion_parent
    );
    await closeRowMenu(page, "the prose block under the issue");

    // The child, a view of the spec (decision 359). The child's own fragment holds a prose block
    // only after a reader types one there: its spec's paragraph is prose of a role document, which
    // the tree read leaves out (INFRA-1253 R2), and the seed writes none. So the row writes a
    // person's comment on the child, as the seed writes the converted issue's, and types a new
    // row under it in the child's own Graph: a prose block under a comment, where criterion is
    // grey for the view alone and a statement would be offered by any schema but the view's.
    const childSent = watchNodeRequests(page, seed.child);
    const childComment = await createComment(seed.child, { body: SEED_WORDS.childComment });
    const commentRead = nodeWhere(
      await requestedTreeRead(page, seed.child),
      "person's comment on the child",
      (node) => node.kind === "comment" && node.id === childComment.id
    );
    await landOn(page, seed.child, commentRead.id, "the child's comment");
    await newRowUnder(page, childSent, SEED_WORDS.childParagraph, "the child's comment");
    const { childParagraph } = await graphEditIds(page, seed);
    expect(childParagraph, "the child's own fragment holds a prose block to retype").toBeDefined();
    await landOn(page, seed.child, childParagraph ?? "", "the child's paragraph");
    await openRowMenu(page, "press", "the child's paragraph");
    await openRetype(page, "the child's paragraph");
    await expect(
      submenuEntry(page, EDIT_LABELS.retype_criterion),
      "the view greys criterion"
    ).toHaveAttribute("aria-disabled", "true");
    expect(
      (await submenuWords(page)).some((words) => words.startsWith(EDIT_LABELS.retype_statement)),
      "the view offers no statement"
    ).toBe(false);
  });
});

// ---------------------------------------------------------------------------------------------
// c-delete
// ---------------------------------------------------------------------------------------------

/** Opens the menu of the focused row and waits until its Delete has settled on a count, failing
 *  with `message` where it is grey. */
async function openCountedDelete(page: Page, what: string, message: string): Promise<void> {
  await openRowMenu(page, "press", what);
  expect(await deleteState(page, what), message).toBe("counted");
}

/** The count the newest dry run of `id` among `sent` answered, once one has. */
async function dryRunCount(
  sent: readonly NodeRequest[],
  id: string,
  what: string
): Promise<number> {
  await expect
    .poll(() => dryRuns(sent).filter((request) => request.id === id).length, {
      message: `the menu on ${what} sends no dry run`,
    })
    .toBeGreaterThan(0);
  const runs = dryRuns(sent).filter((request) => request.id === id);
  return countOf(await runs[runs.length - 1]?.answer, `the dry run of ${what}`);
}

/** Waits until the focus is on a row other than the one of `gone`: the nearest row a drop gives
 *  it to (part I's `moveToRow`). */
async function expectFocusMoved(page: Page, gone: string, message: string): Promise<void> {
  await expect
    .poll(
      async () => {
        const focused = await focusedRowReadId(page);
        return focused !== null && focused !== gone;
      },
      { message }
    )
    .toBe(true);
}

// c-delete: Delete grey on the first statement and the shared check, which a result stands under;
// the third statement's count, its confirm, its cancel and its confirm after an agent's write
// moved the count; what the server keeps after; and the agent's check deleted by the person.
// SLOW(>=2s): five landings, four dry runs each answered, an agent's room write and move while
// the confirm is open, two deletes each drained into the spec and Verification queries, a reload.
test("a delete takes the block and its subtree after a confirm that names the count @phone", async ({
  browser,
}) => {
  test.setTimeout(480_000);
  const size = rowSize();
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await deleteRouteGate(page));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const [statement1, statement2, statement3] = ids.statements;
    await approveDocuments(seed);
    // The header draws the Verification document's approval only as a step of a gated root, so
    // the seed's project gates the design steps before the page first loads the issue.
    await gateDesignSteps();
    await page.setViewportSize(size);
    const sent = watchNodeRequests(page, key);

    await landOn(page, key, statement1, "the first statement");
    await expectApproved(
      page,
      seed.verification,
      "the header draws the Verification document's approval, approved, before any delete"
    );
    await openRowMenu(page, "press", "the first statement");
    expect(
      await deleteState(page, "the first statement"),
      "Delete is grey on the statement that holds a result"
    ).toBe("grey");
    await expectGrey(
      page,
      EDIT_LABELS.edit_delete,
      EDIT_LINES.refused_node_holds_result,
      "Delete is grey on the statement that holds a result"
    );
    await closeRowMenu(page, "the first statement");
    await landOn(page, key, ids.checkShared, "the shared check");
    await openRowMenu(page, "press", "the shared check");
    await expectGrey(
      page,
      EDIT_LABELS.edit_delete,
      EDIT_LINES.refused_node_holds_result,
      "Delete is grey on the check that holds a result, and says a result is never deleted"
    );
    await closeRowMenu(page, "the shared check");

    // The third statement: Delete names the dry run's count, the confirm names it, and its
    // cancel writes nothing.
    const held = nodeOf(await requestedTreeRead(page, key), statement3);
    await landOn(page, key, statement3, "the third statement");
    await markDocument(page);
    await openCountedDelete(page, "the third statement", "Delete on the third statement is grey");
    const counted = await dryRunCount(sent, statement3, "the third statement");
    await expect(
      menuEntry(page, EDIT_LABELS.edit_delete),
      "Delete names the dry run's count"
    ).toContainText(deleteWords(counted));
    await menuEntry(page, EDIT_LABELS.edit_delete).click();
    await expect(confirmDialog(page), "a confirm names the count").toBeVisible();
    await expect(confirmDialog(page), "the confirm names the dry run's count").toContainText(
      confirmWords(counted)
    );
    await cancelDelete(page).click();
    await expect(confirmDialog(page), "Cancel leaves the confirm open").toHaveCount(0);
    expect(nodeWrites(sent), "the confirm's cancel writes nothing").toEqual([]);

    // While the confirm is open the architect's credential writes a prose block under the third
    // statement, and the confirm asks again with the new count.
    await openCountedDelete(page, "the third statement", "Delete on the third statement is grey");
    const again = await dryRunCount(sent, statement3, "the third statement");
    await menuEntry(page, EDIT_LABELS.edit_delete).click();
    await expect(confirmDialog(page), "a confirm names the count").toContainText(
      confirmWords(again)
    );
    await editArtifact(
      seed.tree,
      { ops: [{ after: "end", markdown: `${SEED_WORDS.agentProse}\n`, op: "insert" }] },
      asArchitect
    );
    const agentProse = await rankedProse(key, SEED_WORDS.agentProse);
    const moved = await moveNode(
      key,
      agentProse.id,
      agentProse.placement_seq ?? 0,
      { parent: statement3 },
      "architect"
    );
    expect(
      moved.status,
      `the architect's move of its prose block under the third statement: ${JSON.stringify(moved.body)}`
    ).toBe(200);
    await confirmDelete(page).click();
    const first = await nthWrite(sent, 1, "a confirm names the count");
    expect(
      { expected: first.expected, ifMatch: first.ifMatch, method: first.method },
      "the confirm sends one DELETE with If-Match and expected_count"
    ).toEqual({ expected: String(again), ifMatch: `"${held?.placement_seq}"`, method: "DELETE" });
    const recount = countOf(await first.answer, "the DELETE whose count moved");
    await expect(confirmDialog(page), "the confirm asks again with the new count").toContainText(
      confirmWords(recount)
    );
    await confirmDelete(page).click();
    const second = await nthWrite(sent, 2, "the confirm asks again with the new count");
    expect(second.expected, "the confirm asks again with the new count").toBe(String(recount));
    expect((await second.answer)?.status, "the delete of the third statement is refused").toBe(200);
    await expectFocusMoved(page, statement3, "the focus goes to the nearest row after the delete");
    await expectApprovalChangedSince(
      page,
      seed.spec,
      "after the delete the identity block reads the spec's approval as changed since"
    );

    const answer = await treeAfterReload(page, key);
    expect(
      [statement3, agentProse.id].filter((id) => nodeOf(answer, id) !== undefined),
      "the tree read holds a node the delete took"
    ).toEqual([]);
    expect(
      [statement1, statement2].map((id) => holdsTuple(answer, "serves", ids.checkShared, id)),
      "the shared check keeps its serves to the other two statements"
    ).toEqual([true, true]);
    expect(
      (await getComment(seed.comment)).comment.anchor?.quote,
      "the person's comment on the deleted statement's text is still answered with its quote"
    ).toBe(SEED_WORDS.statement3Quote);

    // The second check, which the architect wrote and which holds no result.
    await landOn(page, key, ids.checkAgent, "the agent's check");
    await markDocument(page);
    const verificationBefore = await approvalWords(
      page,
      seed.verification,
      "the header draws no approval of the Verification document before its check is deleted"
    );
    await openCountedDelete(page, "the agent's check", "the person deletes the agent's check");
    await expectOffered(page, EDIT_LABELS.edit_delete, "the person deletes the agent's check");
    await menuEntry(page, EDIT_LABELS.edit_delete).click();
    await confirmDelete(page).click();
    const agents = await nthWrite(sent, 3, "the person deletes the agent's check");
    expect((await agents.answer)?.status, "the person deletes the agent's check").toBe(200);
    await expectApprovalWordsChanged(
      page,
      seed.verification,
      verificationBefore,
      "after the delete of the agent's check the Verification document's approval reads as before"
    );
    await expectApprovalChangedSince(
      page,
      seed.verification,
      "after the delete of the agent's check the Verification document's approval reads changed since"
    );
  });
});

// ---------------------------------------------------------------------------------------------
// c-grey
// ---------------------------------------------------------------------------------------------

/** The editing entries a node R2 does not admit holds grey. */
const EDITING_ENTRIES = [
  EDIT_LABELS.edit_retype,
  ...MOVE_ENTRIES,
  EDIT_LABELS.edit_delete,
] as const;

// c-grey, first row: every editing entry grey with its reason on the question, the person's
// comment on the issue (the comment on the third statement's text is drawn in the margin, not as a
// row), the message, the result, the agent's decision and the child's borrowed check, New row
// under offered where R3 offers it, no write sent, and the menu and both submenus fitting the
// screen.
// SLOW(>=2s): eight landings, a menu opened on each, and both submenus opened at the row's width.
test("the menu greys each act a node does not take @phone", async ({ browser }) => {
  test.setTimeout(360_000);
  const size = rowSize();
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await proseRowGate(page, seed.key));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    await page.setViewportSize(size);
    const sent = watchNodeRequests(page, key);

    for (const { id, reason, under, via, what } of [
      { id: ids.questionRow, reason: EDIT_LINES.grey_kind, under: true, what: "the question" },
      {
        id: ids.issueComment,
        reason: EDIT_LINES.grey_kind,
        under: true,
        what: "the person's comment on the issue",
      },
      { id: ids.message, reason: EDIT_LINES.grey_kind, under: true, what: "the person's message" },
      { id: ids.result, reason: EDIT_LINES.grey_kind, under: false, what: "the result" },
      {
        id: ids.decision,
        reason: EDIT_LINES.grey_kind,
        under: false,
        what: "the agent's decision",
      },
      {
        id: ids.childCheck,
        reason: EDIT_LINES.grey_own_issue,
        under: false,
        // A node of another issue is read here as this Graph draws it, under the first statement
        // it serves; the address of another issue's node is that issue's, not this row's.
        via: ids.statements[0],
        what: "the child's borrowed check",
      },
    ]) {
      if (via === undefined) {
        await landOn(page, key, id, what);
      } else {
        await landOn(page, key, via, "the first statement");
        await focusOn(await rowUnder(page, via, id, what), what);
      }
      await openRowMenu(page, "press", what);
      for (const word of EDITING_ENTRIES) {
        await expectGrey(
          page,
          word,
          reason,
          id === ids.issueComment && word === EDIT_LABELS.edit_indent
            ? "the comment's Indent is grey"
            : `${word} is grey on ${what}, with its reason`
        );
      }
      if (under) {
        // New row under writes through part Y's room: owed from the question's offer, every grey
        // above asserted, until the base draws Y's criterion row and its block editor (gate G3).
        if (id === ids.questionRow) owedUnless(await criterionEditorGate(page, key));
        await expectOffered(
          page,
          EDIT_LABELS.edit_new_under,
          `New row under is offered on ${what}`
        );
      } else {
        await expect(
          menuEntry(page, EDIT_LABELS.edit_new_under),
          `New row under is grey on ${what}`
        ).toHaveAttribute("aria-disabled", "true");
      }
      await expectMenusFit(page, `the menu on ${what} does not fit the screen`);
      await closeRowMenu(page, what);
    }

    // Both submenus fit the screen too: Lens's on the question, and Retype's on the prose block,
    // the one node of the seed Retype is offered on.
    await landOn(page, key, ids.questionRow, "the question");
    await openRowMenu(page, "press", "the question");
    await menuEntry(page, LENS_WORDS.act).click();
    await expect(rowSubmenu(page), "Lens opens no submenu on the question").toBeVisible();
    await expectMenusFit(page, "the menu and its Lens submenu do not fit the screen");
    await closeRowMenu(page, "the question");
    await landOn(page, key, ids.prose, "the prose block");
    await openRowMenu(page, "press", "the prose block");
    await menuEntry(page, EDIT_LABELS.edit_retype).click();
    await expect(rowSubmenu(page), "Retype opens no submenu on the prose block").toBeVisible();
    await expectMenusFit(page, "the menu and its Retype submenu do not fit the screen");
    await closeRowMenu(page, "the prose block");

    expect(nodeWrites(sent), "the menu sends a write on a node it greys").toEqual([]);
    expect(
      dryRuns(sent).filter((request) => request.id !== ids.prose),
      "the menu sends a dry run on a node it greys"
    ).toEqual([]);
  });
});

// c-grey, second row: on the issue the conversion has not reached, the menu holds Lens alone and
// fits the screen, and no key writes a node.
// SLOW(>=2s): two landings, the menu and its submenu opened, and every key of R4 pressed twice.
test("an unconverted issue offers no editing entry @phone", async ({ browser }) => {
  test.setTimeout(240_000);
  const size = rowSize();
  await phase(browser, seedGraphEdit, async (page, seed) => {
    const ids = await graphEditIds(page, seed);
    await page.setViewportSize(size);
    const sent = watchNodeRequests(page, seed.unconverted);
    const traffic = watchApiRequests(page);

    const row = await landOn(
      page,
      seed.unconverted,
      ids.unconvertedStatement,
      "the unconverted statement"
    );
    await openRowMenu(page, "press", "the unconverted statement");
    await expect
      .poll(() => menuWords(page), { message: "the menu holds Lens alone" })
      .toEqual([LENS_WORDS.act]);
    await expectMenusFit(page, "the menu on the unconverted issue does not fit the screen");
    await menuEntry(page, LENS_WORDS.act).click();
    await expect(rowSubmenu(page), "Lens opens no submenu on the unconverted issue").toBeVisible();
    await expectMenusFit(page, "the Lens submenu on the unconverted issue does not fit the screen");
    await closeRowMenu(page, "the unconverted statement");

    const keys = ["m", "Tab", "Shift+Tab", "Alt+Shift+ArrowUp", "Alt+Shift+ArrowDown"];
    for (const chord of keys) {
      await focusOn(row, "the unconverted statement");
      await page.keyboard.press(chord);
      await page.keyboard.press("Escape");
    }
    await focusOn(row, "the unconverted statement");
    await page.keyboard.press("Enter");
    for (const chord of keys.slice(1)) await page.keyboard.press(chord);
    await page.keyboard.press("Escape");
    await expectLoadSettled(traffic);
    expect(sent, "a key writes a node on the unconverted issue").toEqual([]);
  });
});

// ---------------------------------------------------------------------------------------------
// c-menu-keys
// ---------------------------------------------------------------------------------------------

// c-menu-keys, first row: with the keyboard alone, `m` opens the menu, the arrows, Home, End,
// Right and Left move through it and Lens's submenu, Escape closes it with the focus back on the
// row, and Enter picks a lens kind, which draws the lens under the node.
// SLOW(>=2s): a landing, the menu opened twice, a lens invoked and its row read from the tree.
test("the node menu works by keyboard alone", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(browser, seedGraphEdit, async (page, seed) => {
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const statement3 = ids.statements[2];
    const kinds = await lensCatalogue();

    const row = await landOn(page, key, statement3, "the third statement");
    await expect(lensAct(row), "Graph's row has no Lens act").toHaveCount(0);
    await openRowMenu(page, "m", "the third statement", "m opens the node menu");
    await expect
      .poll(
        async () =>
          (await menuWords(page)).map((words, at) => words.startsWith(MENU_ENTRIES[at] ?? "?")),
        { message: "the menu holds R1's entries in their order, Lens last" }
      )
      .toEqual(MENU_ENTRIES.map(() => true));
    const entries = rowMenu(page).getByRole("menuitem");
    await page.keyboard.press("ArrowDown");
    await expect(entries.nth(1), "Down moves to the next entry").toBeFocused();
    await page.keyboard.press("ArrowDown");
    await expect(entries.nth(2), "Down reaches Retype, grey on a statement").toBeFocused();
    await page.keyboard.press("ArrowUp");
    await expect(entries.nth(1), "Up moves to the entry before").toBeFocused();
    await page.keyboard.press("End");
    await expect(menuEntry(page, LENS_WORDS.act), "End goes to the last entry, Lens").toBeFocused();
    await page.keyboard.press("Home");
    await expect(entries.nth(0), "Home goes to the first entry").toBeFocused();
    await page.keyboard.press("End");
    await page.keyboard.press("ArrowRight");
    await expect(rowSubmenu(page), "Right on Lens opens its submenu").toBeVisible();
    await expect(
      rowSubmenu(page).getByRole("menuitem").first(),
      "Right puts the focus on the submenu's first kind"
    ).toBeFocused();
    await expect
      .poll(() => submenuWords(page), { message: "Lens's submenu holds part W's kinds, in order" })
      .toEqual(kinds.map((kind) => expect.stringContaining(kind.words)));
    await page.keyboard.press("ArrowLeft");
    await expect(rowSubmenu(page), "Left closes the submenu").toHaveCount(0);
    await expect(
      menuEntry(page, LENS_WORDS.act),
      "Left gives the focus back to Lens"
    ).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(rowMenu(page), "Escape closes the menu").toHaveCount(0);
    await expect(
      readRow(page, statement3).first(),
      "Escape gives the focus back to the row"
    ).toBeFocused();

    // Enter picks: on Lens it opens the kinds, and on a kind it invokes that lens, drawn under
    // the node.
    const before = new Set((await requestedTreeRead(page, key)).nodes?.map((node) => node.id));
    const newLenses = async () =>
      ((await requestedTreeRead(page, key)).nodes ?? []).filter(
        (node) => !before.has(node.id) && node.kind === "lens"
      );
    await openRowMenu(page, "m", "the third statement", "m opens the node menu");
    await page.keyboard.press("End");
    await page.keyboard.press("Enter");
    await expect(rowSubmenu(page), "Enter on Lens opens its kinds").toBeVisible();
    await page.keyboard.press("Enter");
    await expect
      .poll(async () => (await newLenses()).map((node) => node.parent?.id), {
        message: "Enter on a kind invokes no lens on the third statement",
        timeout: 30_000,
      })
      .toEqual([statement3]);
    const lens = (await newLenses())[0]?.id ?? "";
    await expect
      .poll(() => parentReadIdOf(page, lens), {
        message: "a pick draws the lens under the node",
        timeout: 30_000,
      })
      .toBe(statement3);
  });
});

// c-menu-keys, second row: in Graph the Lens act is the menu's last entry and not the row's; in
// Conversation it stays on the focused row, which draws no node menu.
// SLOW(>=2s): a landing in Graph and one in Conversation, the menu opened in Graph.
test("Graph puts Lens in the node menu and Conversation keeps it on the row", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedGraphEdit, async (page, seed) => {
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const statement3 = ids.statements[2];

    const graphRow = await landOn(page, key, statement3, "the third statement");
    await expect(lensAct(graphRow), "Graph's row has no Lens act").toHaveCount(0);
    await openRowMenu(page, "press", "the third statement");
    await expect
      .poll(async () => (await menuWords(page)).at(-1), {
        message: "in Graph the Lens act is the node menu's last entry",
      })
      .toBe(LENS_WORDS.act);
    await closeRowMenu(page, "the third statement");

    const row = await landOn(
      page,
      key,
      statement3,
      "the third statement",
      presetPath(key, "conversation")
    );
    await expect(lensAct(row), "in Conversation the Lens act stays on the focused row").toHaveCount(
      1
    );
    await expect(menuControl(page), "Conversation draws a node menu control").toHaveCount(0);
    await page.keyboard.press("m");
    await expect(rowMenu(page), "m opens a node menu in Conversation").toHaveCount(0);
  });
});

// ---------------------------------------------------------------------------------------------
// c-refusals
// ---------------------------------------------------------------------------------------------

// c-refusals: a move of the third statement after the architect moved it under the second, with a
// second move queued behind it (412), and a delete of it after the architect deleted it while the
// confirm was open (409 NODE_GONE or 404).
// SLOW(>=2s): a landing, two writes of the architect, a held move and a held tree read, two dry
// runs and a refused delete, each waited out.
test("a refused write keeps the tree as the server holds it", async ({ browser }) => {
  test.setTimeout(300_000);
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await deleteRouteGate(page));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const [, statement2, statement3] = ids.statements;
    const sent = watchNodeRequests(page, key);
    const traffic = watchApiRequests(page);

    // The page keeps the tree read it loaded while the architect moves the third statement under
    // the second behind it.
    const placed = nodeOf(await requestedTreeRead(page, key), statement3);
    await landOn(page, key, statement3, "the third statement");
    const reads = Promise.withResolvers<void>();
    const holdReads = async (route: Route) => {
      await reads.promise;
      await route.continue();
    };
    await page.route(treeReadRoute(key), holdReads);
    const architects = await moveNode(
      key,
      statement3,
      placed?.placement_seq ?? 0,
      { parent: statement2 },
      "architect"
    );
    expect(architects.status, `the architect's move: ${JSON.stringify(architects.body)}`).toBe(200);

    // The reader's first move is held at the network until a second is queued behind it.
    const queued = Promise.withResolvers<void>();
    const holdNodes = async (route: Route) => {
      await queued.promise;
      await route.continue();
    };
    const nodeRoutes = (url: URL) => url.pathname.includes("/nodes/");
    await page.route(nodeRoutes, holdNodes);
    try {
      await openRowMenu(page, "press", "the third statement");
      await menuEntry(page, EDIT_LABELS.edit_move_up).click();
      await nthWrite(sent, 1, "Move up sends no PATCH");
      await openRowMenu(page, "press", "the third statement");
      await menuEntry(page, EDIT_LABELS.edit_move_up).click();
    } finally {
      queued.resolve();
    }
    await page.unroute(nodeRoutes, holdNodes);
    const refused = await nthWrite(sent, 1, "Move up sends no PATCH");
    expect((await refused.answer)?.status, "the move behind the architect's answers 412").toBe(412);
    // The read hold stays routed once released, and passes each later read through, as the new
    // row's hold does.
    reads.resolve();
    await expectLoadSettled(traffic);
    expect(nodeWrites(sent), "no request writes after the refusal").toHaveLength(1);
    await expect
      .poll(() => parentReadIdOf(page, statement3), {
        message: "the row stays where the server holds it",
        timeout: 30_000,
      })
      .toBe(statement2);
    await expect
      .poll(() => focusedRowReadId(page), { message: "the focus stays on the refused row" })
      .toBe(statement3);
    const row = readRow(page, statement3).first();
    await expect(refusalLine(row), "one line on the row says that the node moved").toContainText(
      EDIT_LINES.refused_precondition_failed
    );
    await expect(
      refusalLine(row),
      "the line on the row says that one queued move was dropped"
    ).toContainText(`1 ${EDIT_LINES.dropped_move}`);

    // The architect deletes the third statement while the person's confirm is open.
    await openCountedDelete(page, "the third statement", "Delete on the third statement is grey");
    await menuEntry(page, EDIT_LABELS.edit_delete).click();
    await expect(confirmDialog(page), "a pick of Delete opens no confirm").toBeVisible();
    const now = nodeOf(await treeOf(key), statement3);
    const counted = countOf(
      await deleteNode(key, statement3, "architect"),
      "the architect's dry run"
    );
    const gone = await deleteNode(key, statement3, "architect", {
      expected: counted,
      placementSeq: now?.placement_seq ?? 0,
    });
    expect(gone.status, `the architect's delete: ${JSON.stringify(gone.body)}`).toBe(200);
    await confirmDelete(page).click();
    const late = await nthWrite(sent, 2, "the confirm sends no DELETE");
    expect([404, 409], "the delete of a node gone answers 409 NODE_GONE or 404").toContain(
      (await late.answer)?.status
    );
    await expect(readRow(page, statement3), "the row is dropped").toHaveCount(0);
    await expectFocusMoved(page, statement3, "the focus goes to the nearest row after the drop");
  });
});

// ---------------------------------------------------------------------------------------------
// c-citation
// ---------------------------------------------------------------------------------------------

/** The words of each line the page draws as a status or an alert, read once two reads agree. */
function pageLines(page: Page): Promise<string[]> {
  return settledValue("the page's lines", () =>
    issuePage(page)
      .locator('[role="status"], [role="alert"]')
      .evaluateAll((lines) =>
        lines.flatMap((line) => {
          const words = (line.textContent ?? "").replace(/\s+/gu, " ").trim();
          return words === "" ? [] : [words];
        })
      )
  );
}

// c-citation: the third statement deleted in Graph; a reference to it in a second comment reads
// Deleted as a link, in the comment's words and on a chip, to the spec at the version before the
// delete; its spec address opens the same version; and its node address opens Graph.
// SLOW(>=2s): two landings, a delete drained into the spec query, and four loads of the page.
test("a citation of a node deleted in Graph opens the version that last held it @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const size = rowSize();
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await deleteRouteGate(page));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const statement3 = ids.statements[2];
    await page.setViewportSize(size);
    const reference = `dispatch://${key}/spec#b-s3`;
    const second = await createComment(key, {
      body: `The delete in Graph takes ${reference} with it.`,
    });
    // A comment's node is read by the comment's own id, as the fixture's tree read names it.
    const citing = nodeWhere(
      await requestedTreeRead(page, key),
      "second comment of the person",
      (node) => node.kind === "comment" && node.id === second.id
    );
    const sent = watchNodeRequests(page, key);
    const traffic = watchApiRequests(page);

    await landOn(page, key, statement3, "the third statement");
    const nodeId = await settledValue("the third statement's row id", () =>
      readRow(page, statement3).first().getAttribute("data-node-id")
    );
    const version = await documentVersion(page, seed.spec);
    await openCountedDelete(page, "the third statement", "Delete on the third statement is grey");
    await menuEntry(page, EDIT_LABELS.edit_delete).click();
    await confirmDelete(page).click();
    const taken = await nthWrite(sent, 1, "the confirm sends no DELETE");
    expect((await taken.answer)?.status, "the delete of the third statement is refused").toBe(200);

    const address = `/issues/${key}/spec?v=${version}#b-s3`;
    const atVersion = (url: URL) =>
      url.pathname === `/issues/${key}/spec` &&
      url.searchParams.get("v") === String(version) &&
      url.hash === "#b-s3";
    const row = await landOn(page, key, citing.id, "the second comment");
    // Row edges follow the text in the same container. Select the inline citation itself.
    const link = rowWords(row)
      .getByRole("link", { exact: true, name: CONTROL_LABELS.target_gone })
      .and(row.locator("a[data-row-reference]"));
    await expect(link, "the reference links to the version that last held it").toHaveAttribute(
      "href",
      address
    );
    await expect(
      row
        .locator("[data-node-chip]")
        .getByRole("link", { exact: true, name: CONTROL_LABELS.target_gone }),
      "the reference's chip links to the version that last held it"
    ).toHaveAttribute("href", address);
    await link.click();
    await expect(page, "the press opens the Spec filter at the version").toHaveURL(atVersion);

    // The deleted statement's spec address sends one reference-targets read and opens the same.
    traffic.reset();
    await page.goto(`/issues/${key}/spec#b-s3`);
    await expect(page, "the deleted statement's address opens the version").toHaveURL(atVersion);
    await expectLoadSettled(traffic);
    expect(
      traffic.requests.filter((request) => request.route === "/api/v1/reference-targets"),
      "the deleted statement's address sends one reference-targets read"
    ).toHaveLength(1);

    // The statement's node address opens as Graph, with no line.
    await gotoIssue(page, presetPath(key, "graph"));
    const lines = await pageLines(page);
    await gotoIssue(page, `${presetPath(key, "graph")}/${encodeURIComponent(nodeId ?? "")}`);
    await expect(page, "the deleted node's address opens as /graph").toHaveURL(
      (url) => url.pathname === presetPath(key, "graph")
    );
    expect(await pageLines(page), "the deleted node's address opens Graph with no line").toEqual(
      lines
    );
  });
});

// ---------------------------------------------------------------------------------------------
// c-requests
// ---------------------------------------------------------------------------------------------

/** The reads the shell sends on a timer of its own, whatever the page does: the credential
 *  requests waiting on the reader, read every 15 s by the shell's Inbox count, and the agents,
 *  read every 15 s while a row draws an agent's name, as the architect's rows do. A window that
 *  spans either says nothing of a write. */
const SHELL_TIMED_READS: Readonly<Record<string, true>> = {
  "/api/v1/agents": true,
  "/api/v1/credential-requests": true,
};

/** The routes of the reads among `traffic`'s requests from `from` on, the shell's timed reads
 *  left out. */
function readRoutes(traffic: ApiRequests, from: number): string[] {
  return [
    ...new Set(
      traffic.requests
        .slice(from)
        .filter((request) => request.method === "GET" && SHELL_TIMED_READS[request.route] !== true)
        .map((request) => request.route)
    ),
  ];
}

/** The reads of the reader's own state that a read mark of the page brings: the page marks a
 *  thread, a document or the issue read as the focus reaches a row (`PUT /api/v1/me/…`, the issue's
 *  by `IssueReadMark.tsx`), and the `user_state.updated` and `user_thread_state.updated` events
 *  each mark sends read these again (`api/event-refresh.ts`). No write of Graph editing makes a
 *  mark. */
const READ_MARK_READS: Readonly<Record<string, true>> = {
  "/api/v1/me/replies": true,
  "/api/v1/me/state": true,
};

/** The reads the editor makes of the document it opens (`ProofDocument.tsx`: its live text and
 *  its blocks), which a pick from the menu that opens an editor brings: R12's "the one read and
 *  the one socket of part Y's first Enter". */
const EDITOR_OPEN_READS: readonly string[] = ["text", "blocks"];

/** `readRoutes` of the window a write of Graph editing is counted in, but for a read of the
 *  reader's own state that a read mark's `PUT /api/v1/me/…` before it in that window brings (the
 *  focus the row moves to the act starts those marks), and, where the act's pick opens an editor
 *  of the document `opened`, that editor's reads of it (`EDITOR_OPEN_READS`). Such a read with no
 *  mark before it, a read of another document, and every other read, stay counted. */
function readRoutesOfWrite(traffic: ApiRequests, from: number, opened?: string): string[] {
  const window = traffic.requests.slice(from);
  const marked = window.findIndex(
    (request) => request.method === "PUT" && request.route.startsWith("/api/v1/me/")
  );
  const editorReads = new Set(
    opened === undefined
      ? []
      : EDITOR_OPEN_READS.map((read) => `/api/v1/artifacts/${encodeURIComponent(opened)}/${read}`)
  );
  return [
    ...new Set(
      window
        .filter(
          (request, at) =>
            request.method === "GET" &&
            SHELL_TIMED_READS[request.route] !== true &&
            !(marked !== -1 && at > marked && READ_MARK_READS[request.route] === true) &&
            !editorReads.has(new URL(request.path, "http://page.test").pathname)
        )
        .map((request) => request.route)
    ),
  ];
}

/** Each HTTP write among `traffic`'s requests from `from` on to a route of issue `key` or of a
 *  document, as its method and path: a write of a block would be one. A menu's dry run of a delete
 *  (`?dry_run=true`, R6) writes nothing, so it is none. */
function blockWrites(traffic: ApiRequests, from: number, key: string): string[] {
  return traffic.requests
    .slice(from)
    .filter(
      (request) =>
        request.method !== "GET" &&
        !/[?&]dry_run=true(?:&|$)/u.test(request.path) &&
        (request.path.startsWith("/api/v1/artifacts/") ||
          request.path.startsWith(`/api/v1/issues/${encodeURIComponent(key)}`))
    )
    .map((request) => `${request.method} ${request.path}`);
}

// c-requests: Graph's first load sends no request of its editing; a menu sends one dry run on a
// statement and none on a comment; a move one PATCH, a delete one DELETE, a new node one PATCH and
// no write of its block, a retype no HTTP write; and no read follows a write but the outline's,
// the approval's and those today's version events cause.
// SLOW(>=2s): the seed's edit notices waited out, Graph's load, two menus, a move, a delete, a new
// row typed and placed and a retype, each waited out to a 1.5 s quiet window.
test("Graph sends no request of its editing until the reader acts", async ({ browser }) => {
  test.setTimeout(480_000);
  await phase(browser, seedGraphEdit, async (page, seed) => {
    owedUnless(await deleteRouteGate(page));
    owedUnless(await roomWriteGate(page, seed.key));
    const ids = await graphEditIds(page, seed);
    const key = seed.key;
    const statement3 = ids.statements[2];
    const sent = watchNodeRequests(page, key);
    const traffic = watchApiRequests(page);

    await seedEventsLanded(page);
    await editNoticesSent(key, traffic);
    traffic.reset();
    await gotoIssue(page, presetPath(key, "graph"));
    await expectLoadSettled(traffic);
    expect(dryRuns(sent), "the load sends no dry run").toEqual([]);
    expect(sent, "Graph's first load sends a request to the issue's node routes").toEqual([]);
    const blocks = new Set(
      ((await requestedTreeRead(page, key)).nodes ?? []).flatMap((node) => node.block_id ?? [])
    );
    expect(
      traffic.requests.filter((request) => {
        const block = /#b-([^&]+)/u.exec(decodeURIComponent(request.path))?.[1];
        return (
          request.route === "/api/v1/reference-targets" && block !== undefined && !blocks.has(block)
        );
      }),
      "Graph's first load sends a reference-targets read for a gone node"
    ).toEqual([]);

    // The reads today's version events cause, read off one edit of the spec's memo.
    let from = traffic.requests.length;
    await editArtifact(seed.spec, {
      ops: [{ find: SEED_WORDS.memo, op: "replace", with: `${SEED_WORDS.memo} Kept.` }],
    });
    await expect
      .poll(() => treeReads(traffic.requests.slice(from), key, null), {
        message: "a version of the spec reads no tree",
        timeout: 30_000,
      })
      .toBeGreaterThan(0);
    await expectLoadSettled(traffic);
    const ownReads: Readonly<Record<string, true>> = Object.fromEntries(
      [
        ...readRoutes(traffic, from),
        "/api/v1/issues/:id/tree",
        "/api/v1/issues/:id",
        "/api/v1/artifacts/:id",
      ].map((route) => [route, true] as const)
    );

    // A menu on a statement sends one dry run, and on a comment none.
    await focusOn(readRow(page, statement3).first(), "the third statement");
    await openCountedDelete(page, "the third statement", "Delete on the third statement is grey");
    await closeRowMenu(page, "the third statement");
    await expectLoadSettled(traffic);
    expect(
      sent.map((request) => ({ dryRun: request.dryRun, id: request.id, method: request.method })),
      "a menu on a statement sends one dry run"
    ).toEqual([{ dryRun: true, id: statement3, method: "DELETE" }]);
    await focusOn(readRow(page, ids.issueComment).first(), "the person's comment on the issue");
    await openRowMenu(page, "press", "the person's comment on the issue");
    await closeRowMenu(page, "the person's comment on the issue");
    await expectLoadSettled(traffic);
    expect(sent, "a menu on a comment sends a dry run").toHaveLength(1);

    // A move sends one PATCH, and a delete one DELETE; neither is followed by a read beside the
    // outline's, the approval's and those a version event causes. The focus on the row starts the
    // page's read marks, whose reads of the reader's own state are no write's (`readRoutesOfWrite`).
    for (const { entry, id, method, what } of [
      { entry: EDIT_LABELS.edit_move_up, id: statement3, method: "PATCH", what: "a move" },
      { entry: EDIT_LABELS.edit_delete, id: ids.checkAgent, method: "DELETE", what: "a delete" },
    ]) {
      from = traffic.requests.length;
      const count = sent.length;
      await focusOn(readRow(page, id).first(), what);
      await openCountedDelete(page, what, `Delete is grey where ${what} is made`);
      await menuEntry(page, entry).click();
      if (method === "DELETE") await confirmDelete(page).click();
      await nthWrite(sent, nodeWrites(sent.slice(0, count)).length + 1, `${what} sends no write`);
      await seedEventsLanded(page);
      await expectLoadSettled(traffic);
      expect(
        nodeWrites(sent.slice(count)).map((write) => write.method),
        `${what} sends one ${method}`
      ).toEqual([method]);
      expect(
        readRoutesOfWrite(traffic, from).filter((route) => ownReads[route] !== true),
        `${what} is followed by a read beside the outline's and the approval's`
      ).toEqual([]);
    }

    // A new node sends one PATCH and no HTTP write of its block; a retype sends no HTTP write.
    await landOn(page, key, ids.criterion, "the criterion");
    from = traffic.requests.length;
    let count = sent.length;
    const made = await newRowUnder(page, sent, TYPED.retypedStatement, "the criterion");
    await seedEventsLanded(page);
    await expectLoadSettled(traffic);
    expect(
      nodeWrites(sent.slice(count)).map((write) => ({ id: write.id, method: write.method })),
      "a new node sends one PATCH"
    ).toEqual([{ id: made, method: "PATCH" }]);
    expect(
      blockWrites(traffic, from, key).filter((write) => !write.startsWith("PATCH ")),
      "a new node sends an HTTP write of its block"
    ).toEqual([]);
    // The pick opens the editor of the issue's tree artifact, whose own reads of it R12 names.
    expect(
      readRoutesOfWrite(traffic, from, seed.tree).filter((route) => ownReads[route] !== true),
      "a new node is followed by a read beside the outline's and the approval's"
    ).toEqual([]);
    from = traffic.requests.length;
    count = sent.length;
    await openRowMenu(page, "m", "the new row under the criterion");
    await openRetype(page, "the new row under the criterion");
    await submenuEntry(page, EDIT_LABELS.retype_statement).click();
    await expect
      .poll(async () => nodeOf(await requestedTreeRead(page, key), made)?.supertag, {
        message: "the retype is never stored",
        timeout: 60_000,
      })
      .toBe("requirement");
    await seedEventsLanded(page);
    await expectLoadSettled(traffic);
    expect(nodeWrites(sent.slice(count)), "a retype sends a write to the node routes").toEqual([]);
    expect(blockWrites(traffic, from, key), "a retype sends an HTTP write").toEqual([]);
    expect(
      readRoutesOfWrite(traffic, from, seed.tree).filter((route) => ownReads[route] !== true),
      "a retype is followed by a read beside the outline's and the approval's"
    ).toEqual([]);
  });
});
