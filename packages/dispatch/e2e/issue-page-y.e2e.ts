import type { Locator, Page } from "@playwright/test";

import { agentRoleWord } from "../web/src/features/issue-page/agent-role";
import { KEY_REPORTS } from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_LINES,
  APPROVAL_STATE_WORDS,
  type CheckState,
  CONTROL_LABELS,
  GAP_WORDS,
  type GapKind,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { parseDispatchReference, shortForm } from "../web/src/features/refs/routes";
import { getArtifact } from "./api";
import { countDocumentSockets } from "./editor";
import { seedWholeIssue } from "./issue-layout-seed";
import { PLANNER_TITLE } from "./issue-layout-seed-support";
import { gotoIssue, issuePage, issueRead, phase, presetChooser, presetCount } from "./issue-page";
import {
  DESKTOP,
  type DrawnRow,
  drawnRows,
  expectIssuePageDrawn,
  focusRow,
  nodeRow,
  PHONE,
  parentOfRow,
  READER,
  rowAct,
  rowBody,
  rowLayouts,
  rowOf,
  rowWords,
  unfoldEveryBranch,
} from "./issue-page-g";
import { caretInNode, focusedNodeId, onScreen, pickPreset, rowReference } from "./issue-page-i";
import { composerField, composerTarget } from "./issue-page-l";
import {
  artifactRoomOf,
  criterionAct,
  criterionLine,
  criterionRefusal,
  criterionRows,
  seedModelRows,
  TREE_READ_OWED,
  treeAnswerOf,
  treeReadServed,
  writeCriterionApi,
} from "./issue-page-y";
import { settledState } from "./settled";
import { expect, test } from "./test";
import { type ApiRequest, expectLoadSettled, type RouteCount, watchApiRequests } from "./traffic";

/**
 * Criteria, readings, a child issue's checks under the statements they serve, and editing a
 * converted issue in its one room, on the new issue page. Each row is selected by its title, its
 * `-g` string, from one of the issue's checks: row 1 by c-write, row 2 by c-readings, with a row
 * of its own, owed and tagged `@owed`, that c-readings' run leaves out, and row 3 by c-editing.
 *
 * The rows are on the real stack, each through `phase` of `e2e/issue-page.ts` (a reset database,
 * alice signed in, the new page turned on with `withIssuePage`) on `seedModelRows` of
 * `e2e/issue-page-y.ts`, an issue born converted, and each first reads that the issue's tree read
 * answers `converted: true`. They are owed until the server serves the tree read
 * (`GET /api/v1/issues/{key}/tree`), the issue room and writes to the tree artifact: until then
 * each row skips on `treeReadServed`.
 */

/** What a failure says where the issue's tree read does not answer `converted: true`. */
const NOT_CONVERTED = "the issue's tree read does not answer converted: true";

/** The criterion's attribute that names the node it was written under: the model's word, which
 *  no element of the page draws as its whole text. */
const PARENT_ATTRIBUTE = "parent";

/** At most how many presses of `f` cross a row's stops: its links, its chips and the acts no key
 *  presses (`useTreeKeymap.ts`'s `rowStops`). */
const ROW_STOPS_AT_MOST = 12;

/** One word each text the rows write holds and no other node holds. */
const WRITTEN = {
  counted: "Kingsmere",
  edited: "Lanternby",
  escaped: "Ivyhollow",
  first: "Heronsedge",
  refused: "Jasperfell",
} as const;

/** One word each row 3 types at the start of a node's words. */
const TYPED = {
  check: "Nettlecombe",
  criterion: "Oakhurst",
  statement: "Marrowind",
} as const;

/** The page opened on issue `key` with every branch unfolded, so a node's row is drawn wherever
 *  the tree places it. */
async function openOutline(page: Page, key: string): Promise<void> {
  await gotoIssue(page, `/issues/${key}`);
  await expectIssuePageDrawn(page);
  await unfoldEveryBranch(page);
}

/** The first load of issue `key`, by route, read once the page has gone quiet. */
async function firstLoad(page: Page, key: string): Promise<readonly RouteCount[]> {
  const traffic = watchApiRequests(page);
  await gotoIssue(page, `/issues/${key}`);
  await expectLoadSettled(traffic);
  return traffic.byRoute();
}

/** Each route `loaded` sends more often than `bare` sent it, a route `bare` never sent included. */
function overBudget(loaded: readonly RouteCount[], bare: readonly RouteCount[]): string[] {
  const allowed = new Map(bare.map(({ count, route }) => [route, count]));
  return loaded
    .filter(({ count, route }) => count > (allowed.get(route) ?? 0))
    .map(
      ({ count, route }) =>
        `${route}: ${count}, where the bare issue's sent ${allowed.get(route) ?? 0}`
    );
}

/** Whether `request` writes into the tree artifact `tree`, as a criterion's send does. */
function writesTree(request: ApiRequest, tree: string): boolean {
  return request.method === "POST" && request.path === `/api/v1/artifacts/${tree}/edits`;
}

/** Whether `request` is the tree read of issue `key` with no view. */
function readsTree(request: ApiRequest, key: string): boolean {
  const url = new URL(request.path, "http://path.invalid");
  return (
    request.method === "GET" &&
    url.pathname === `/api/v1/issues/${key}/tree` &&
    !url.searchParams.has("view")
  );
}

/** The count of view `preset` the view row draws: beside its toggle from 1280 px up, and below
 *  that inside the one control's menu, which this opens and closes again. */
async function presetCountOf(page: Page, preset: Preset): Promise<number> {
  const count = presetCount(page, preset);
  const where = await settledState(`the count of ${preset}`, [
    ["beside", count.first()],
    ["chooser", presetChooser(page)],
  ]);
  if (where === "chooser") await presetChooser(page).click();
  const drawn = Number.parseInt((await count.first().innerText()).trim(), 10);
  if (where === "chooser") await page.keyboard.press("Escape");
  return drawn;
}

/** Presses `f` until it leaves the focus on `target`, a stop of the focused row. */
async function stepTo(page: Page, target: Locator, what: string): Promise<void> {
  for (let pressed = 0; pressed < ROW_STOPS_AT_MOST; pressed += 1) {
    // `f` moves the focus inside its own keydown handler, and the row was focused before the
    // first read, so each read sees where the last press left the focus, and nothing moves later.
    if (await target.evaluate((element) => element === document.activeElement)) break;
    await page.keyboard.press("f");
  }
  await expect(target, what).toBeFocused();
}

/** Aims the box at the node of row `id` with its row's Write a criterion. */
async function aimAt(page: Page, id: string): Promise<void> {
  await criterionAct(await focusRow(page, id)).click();
  await expect(
    criterionLine(page),
    `Write a criterion does not aim the box at ${id}`
  ).toHaveAttribute("data-criterion-under", id);
}

/** Whitespace as the page lays it out: runs of it one space. */
function laidOut(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/**
 * Fails unless the row of criterion `id` draws `typed` as typed, its `reference` drawn as a link
 * whose words hold its short form, the tag "criterion" and alice as its author (c-write (2)).
 */
async function expectCriterionDrawn(
  page: Page,
  id: string,
  typed: string,
  reference: string,
  when: string
): Promise<void> {
  const row = nodeRow(page, id).first();
  const route = parseDispatchReference(reference);
  if (route === undefined) throw new Error(`${reference} is no reference`);
  const link = rowReference(row, reference);
  await expect(link, `the criterion's reference is not drawn as a link ${when}`).toHaveCount(1);
  const linked = laidOut(await link.innerText());
  expect(linked, `the criterion's reference is not drawn as its short form ${when}`).toContain(
    shortForm(route)
  );
  expect(
    laidOut(await rowWords(row).innerText()),
    `the criterion's words are not the text as typed ${when}`
  ).toContain(laidOut(typed.replace(reference, linked)));
  await expect(
    row.locator("[data-node-margin] [data-node-tag]"),
    `the criterion does not carry its kind's tag ${when}`
  ).toHaveText(NODE_TYPE_NAMES.criterion);
  await expect(
    row.locator("[data-node-margin] [data-node-author]"),
    `the criterion is not alice's ${when}`
  ).toHaveText(READER);
}

/** The ids of the rows drawn under the first row of `id`: the run after it of deeper levels. */
function idsUnder(rows: readonly DrawnRow[], id: string): string[] {
  const at = rows.findIndex((row) => row.id === id);
  if (at === -1) return [];
  const level = Number(rows[at]?.level ?? "");
  const end = rows.findIndex((row, index) => index > at && Number(row.level ?? "") <= level);
  return rows.slice(at + 1, end === -1 ? undefined : end).map((row) => row.id ?? "<no id>");
}

/** The words the gap "No check" stands for on a row (`data-gap-kind`). */
const NO_CHECK: GapKind = "no_check";

/** Fails where statement 7, of row `row`, draws "No check", by its gap and by its word. */
async function expectStatementSevenChecked(page: Page, row: string, where: string): Promise<void> {
  const rows = await drawnRows(page);
  expect(rowOf(rows, row), `statement 7 draws no row ${where}`).toBeDefined();
  expect(rowOf(rows, row)?.gapKinds, `statement 7 draws "No check" ${where}`).not.toContain(
    NO_CHECK
  );
  await expect(
    nodeRow(page, row).first(),
    `statement 7 says "No check" ${where}`
  ).not.toContainText(GAP_WORDS.no_check);
}

/**
 * What the page may not draw, as c-readings (4) reads it: each element on screen but a row's
 * kind tag whose whole text is `criterion`, `reading` or `parent`, in any case, and each uuid the
 * page's text holds.
 */
function plainWordsOffences(page: Page): Promise<string[]> {
  return issuePage(page).evaluate(
    (root, words) => {
      const offences: string[] = [];
      for (const element of root.querySelectorAll("*")) {
        if (element.getClientRects().length === 0) continue;
        const text = (element.textContent ?? "").trim().toLowerCase();
        if (!words.includes(text)) continue;
        if (element.closest("[data-node-tag]") !== null) continue;
        if (element.querySelector("[data-node-tag]") !== null) continue;
        offences.push(`<${element.tagName.toLowerCase()}> reads "${text}"`);
      }
      const uuid = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi;
      for (const found of (root as HTMLElement).innerText.match(uuid) ?? []) {
        offences.push(`the text holds the uuid ${found}`);
      }
      return offences;
    },
    [NODE_TYPE_NAMES.criterion, NODE_TYPE_NAMES.reading, PARENT_ATTRIBUTE].map((word) =>
      word.toLowerCase()
    )
  );
}

/** Where `caretInto` puts the caret: the row's id, the artifact whose socket its editor opens,
 *  what a failure names the node by, and the message of the first assertion. */
interface CaretTarget {
  readonly artifact: string;
  readonly id: string;
  readonly message: string;
  readonly what: string;
}

/** Puts the caret in the node of row `target.id` with Enter, failing with `target.message` where
 *  it does not go in, and fails unless the Enter opens its editor's socket on `target.artifact`
 *  and at most one socket in all. */
async function caretInto(page: Page, target: CaretTarget): Promise<void> {
  await focusRow(page, target.id);
  // Counted from just before the Enter, so a socket an earlier step opened counts for neither.
  const own = countDocumentSockets(page, target.artifact);
  const all = countDocumentSockets(page);
  await page.keyboard.press("Enter");
  await expect.poll(() => caretInNode(page), { message: target.message }).toBe(true);
  await expect
    .poll(own, { message: `the editor of ${target.what} does not open its artifact's socket` })
    .toBeGreaterThan(0);
  expect(all(), `Enter on ${target.what} opened more than one socket`).toBeLessThanOrEqual(1);
}

/** Fails unless Enter on row `id` puts no caret and the row says `report`. */
async function expectNoCaret(page: Page, id: string, report: string, what: string): Promise<void> {
  await focusRow(page, id);
  await page.keyboard.press("Enter");
  await expect(
    nodeRow(page, id).first().locator("[data-row-report]"),
    `Enter on ${what} does not say why it takes no caret`
  ).toHaveText(report);
  expect(await caretInNode(page), `Enter on ${what} put a caret somewhere`).toBe(false);
}

// Each row reads the issue's tree read, so each skips, owed, where the server does not serve it.
test.beforeEach(async () => {
  test.skip(!(await treeReadServed()), TREE_READ_OWED);
});

// c-write. On the seed's first comment thread, `f` reaches Write a criterion and Enter aims the
// box at the comment; what alice types and sends is a criterion under that comment, as typed,
// drawn as hers; it stays after a reload, Spec counts it, and the header's chip reads the approved
// version against the later one the criterion made. Escape before a send writes nothing; the
// child, a view of the seed, and a check's row offer no Write a criterion; a refused send keeps
// its text and says why; and the send costs one write and one or two tree reads, on a first load
// that sends by route no more than a bare `seedWholeIssue` issue's. At 1440 px, and on `iphone` at
// 390 px, where the box's placeholder takes its phone form.
//
// Before (1) it loads a bare `seedWholeIssue` issue, a second seed of the row, and counts its first
// load by route, then counts the seeded issue's the same way: (8) compares the two, so the first
// assertion that fails on a page without the criterion line is (1)'s.
// SLOW(>=2s): two whole issues seeded and each loaded cold until its reads settle, the second with
// a decision, two readings, an approval and a child written over it and waited on in the tree
// read; then four sends from the box, two of them refused, a reload and a load of the child.
test("a person writes a criterion under the words they are reading @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  const phone = testInfo.project.name === "iphone";
  const size = phone ? PHONE : DESKTOP;

  let bareLoad: readonly RouteCount[] = [];
  await phase(browser, seedWholeIssue, async (page, bare) => {
    await page.setViewportSize(size);
    bareLoad = await firstLoad(page, bare.key);
  });

  await phase(browser, seedModelRows, async (page, seeded) => {
    await page.setViewportSize(size);
    const seededLoad = await firstLoad(page, seeded.key);
    expect((await treeAnswerOf(seeded.key)).converted, NOT_CONVERTED).toBe(true);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    const specBefore = await presetCountOf(page, "spec");
    const { thread } = seeded;

    // (1) `f` reaches the act on the focused comment, and Enter aims the box at the comment.
    const comment = await focusRow(page, thread.row);
    const act = criterionAct(comment);
    await expect(act, "the focused comment offers Write a criterion").toBeVisible();
    await stepTo(page, act, "f does not reach Write a criterion on the focused comment");
    await page.keyboard.press("Enter");
    await expect(
      criterionLine(page),
      "Enter on Write a criterion does not aim the box at the comment"
    ).toHaveAttribute("data-criterion-under", thread.row);
    await expect(
      composerTarget(page),
      "the box does not say what the criterion goes under"
    ).toContainText(CONTROL_LABELS.criterion_target);
    await expect(
      composerTarget(page),
      "the box does not name the comment's first line"
    ).toContainText(thread.firstLine);
    await expect(
      composerField(page),
      `the box does not say what to write at ${size.width} px`
    ).toHaveAttribute(
      "placeholder",
      phone ? CONTROL_LABELS.criterion_placeholder_phone : CONTROL_LABELS.criterion_placeholder
    );

    // (2) Two lines with a reference, sent: a row under the comment, as typed, alice's, focused
    // and on screen.
    const reference = `dispatch://${seeded.parentKey}`;
    const typed = `${WRITTEN.first}: a reader finds every statement this issue asks for.\nIt starts from ${reference} and its spec.`;
    await composerField(page).fill(typed);
    await composerField(page).press("Enter");
    const written = criterionRows(page).filter({ hasText: WRITTEN.first });
    await expect(written, "the sent criterion is no row of the outline").toHaveCount(1);
    const criterion = (await written.getAttribute("data-node-id")) ?? "";
    await expect
      .poll(() => parentOfRow(page, criterion), {
        message: "the criterion stands under the comment",
      })
      .toBe(thread.row);
    await expectCriterionDrawn(page, criterion, typed, reference, "after the send");
    await expect
      .poll(() => focusedNodeId(page), { message: "the sent criterion does not take the focus" })
      .toBe(criterion);
    await expect
      .poll(() => onScreen(nodeRow(page, criterion)), {
        message: "the sent criterion is not on screen",
      })
      .toBe(true);

    // (3) The same after a reload, and Spec draws it as a row, its count one higher.
    await page.reload();
    await issueRead(page);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    await expect
      .poll(() => parentOfRow(page, criterion), {
        message: "after a reload the criterion does not stand under the comment",
      })
      .toBe(thread.row);
    await expectCriterionDrawn(page, criterion, typed, reference, "after a reload");
    await pickPreset(page, PRESET_NAMES.spec);
    await expect(nodeRow(page, criterion), "Spec draws no row of the criterion").not.toHaveCount(0);
    expect(await presetCountOf(page, "spec"), "Spec does not count the criterion").toBe(
      specBefore + 1
    );
    await pickPreset(page, PRESET_NAMES.conversation);
    await expectIssuePageDrawn(page);

    // (4) The header's chip reads the version alice approved against the later one.
    const latest = (await getArtifact(seeded.specArtifactId)).approval?.latest_version ?? 0;
    expect(latest, "the criterion made no later version of the spec").toBeGreaterThan(
      seeded.approvedVersion
    );
    await expect(
      issuePage(page).locator(`[data-approval-artifact="${seeded.specArtifactId}"]`).first(),
      "the header's chip does not read the approved version against a later one"
    ).toHaveText(
      `${APPROVAL_STATE_WORDS.approved} v${seeded.approvedVersion} · ${CONTROL_LABELS.approval_latest} v${latest}`
    );

    // (5) Escape before a send ends the aim and writes nothing.
    await unfoldEveryBranch(page);
    const escapedSends = watchApiRequests(page);
    await aimAt(page, thread.row);
    await composerField(page).fill(`${WRITTEN.escaped} is typed and never sent.`);
    await page.keyboard.press("Escape");
    await expect(criterionLine(page), "Escape before a send does not end the aim").toHaveCount(0);
    await expect
      .poll(() => focusedNodeId(page), { message: "Escape does not give the comment's row back" })
      .toBe(thread.row);
    expect(
      escapedSends.requests.filter((request) => writesTree(request, seeded.tree)),
      "Escape before a send wrote into the tree"
    ).toEqual([]);
    expect(
      (await treeAnswerOf(seeded.key)).nodes.filter((node) => node.text.includes(WRITTEN.escaped)),
      "the tree read holds what was typed before Escape"
    ).toEqual([]);

    // (6) No act on the child, whose tree read answers `view_of` the seed, nor on a check's row.
    expect(
      (await treeAnswerOf(seeded.child.key)).view_of,
      "the child's tree read does not answer view_of the seed"
    ).toBe(seeded.key);
    await openOutline(page, seeded.child.key);
    const childComment = await focusRow(page, seeded.child.comment);
    await expect(rowAct(childComment, "reply"), "the child's comment draws no acts").toBeVisible();
    await expect(
      criterionAct(childComment),
      "the child, a view of the seed, offers Write a criterion"
    ).toHaveCount(0);
    await openOutline(page, seeded.key);
    const check = await focusRow(page, seeded.checkOne.row);
    await expect(rowBody(check), "check c1's focused row draws no body").toBeVisible();
    await expect(criterionAct(check), "a check's row offers Write a criterion").toHaveCount(0);

    // (7) A refused send keeps the text and says why: 400 `INVALID_JSON`, then 404.
    const refused = `${WRITTEN.refused} is refused and kept in the box.`;
    const edits = `**/api/v1/artifacts/${seeded.tree}/edits`;
    await aimAt(page, thread.row);
    await composerField(page).fill(refused);
    await page.route(edits, (route) =>
      route.fulfill({ json: { code: "INVALID_JSON", error: "the body is no JSON" }, status: 400 })
    );
    await composerField(page).press("Enter");
    await expect(
      criterionRefusal(page),
      "a send refused 400 INVALID_JSON does not say the criterion was not written"
    ).toHaveText(ANSWER_LINES.criterion_not_written);
    await expect(composerField(page), "a send refused 400 lets its text go").toHaveValue(refused);
    await page.unroute(edits);
    await page.route(edits, (route) =>
      route.fulfill({ json: { code: "NOT_FOUND", error: "no such node" }, status: 404 })
    );
    await composerField(page).press("Enter");
    await expect(
      criterionRefusal(page),
      "a send refused 404 does not say the node is no longer here"
    ).toHaveText(ANSWER_LINES.criterion_gone);
    await expect(composerField(page), "a send refused 404 lets its text go").toHaveValue(refused);
    await page.unroute(edits);
    await page.keyboard.press("Escape");
    await expect(criterionLine(page), "Escape after a refusal does not end the aim").toHaveCount(0);

    // (8) The reader goes back to the comment's row before Send, and Send is pressed from there:
    // the press a tap makes when the reader has gone back to a row, which the line must still
    // send. From that press to the landing, one write into the tree and one or two tree reads -
    // the send's own, and one more the page's freshness may queue for the write's own event; and
    // the first load sends by route no more than the bare issue's.
    await aimAt(page, thread.row);
    await composerField(page).fill(`${WRITTEN.counted} is the send whose requests are counted.`);
    await focusRow(page, thread.row);
    const sends = watchApiRequests(page);
    const send = criterionLine(page).getByRole("button", {
      exact: true,
      name: CONTROL_LABELS.send,
    });
    if (phone) await send.tap();
    else await send.click();
    const landed = criterionRows(page).filter({ hasText: WRITTEN.counted });
    await expect(landed, "the counted criterion is no row of the outline").toHaveCount(1);
    const landedId = (await landed.getAttribute("data-node-id")) ?? "";
    await expect
      .poll(() => focusedNodeId(page), { message: "the counted criterion does not take the focus" })
      .toBe(landedId);
    const sent = [...sends.requests];
    expect(
      sent.filter((request) => writesTree(request, seeded.tree)).length,
      "from the press of Send to the landing the page wrote into the tree other than once"
    ).toBe(1);
    const treeReads = sent.filter((request) => readsTree(request, seeded.key)).length;
    const readsOften =
      "from the press of Send to the landing the page read the tree other than once or twice";
    expect(treeReads, readsOften).toBeGreaterThanOrEqual(1);
    expect(treeReads, readsOften).toBeLessThanOrEqual(2);
    expect(
      overBudget(seededLoad, bareLoad),
      "the first load sends a route more often than a bare seedWholeIssue issue's"
    ).toEqual([]);
  });
});

// c-readings. An agent's reading is a row under the decision or the answered question it reads,
// tagged "reading", in the agents' grey and named by its agent's role; the child's check, with its
// result, and its plan task are rows under the statement they serve, each marked with the child's
// key as a link, and none under the child's own row; that statement draws no "No check" on the
// outline or under Verification. Under Decisions the decision's reading is a row under it and the
// question's is not drawn; and no element but a kind tag reads a model's word whole, nor any text
// a uuid. At 1440 px. How the child's check stands, which its result gives, is the next row's,
// owed until the tree read answers it.
//
// The steps are asserted in the order (1), (3), (2), (4). On a page without the reading rows, no
// step drafts a reading, so the row fails there on (1)'s first assertion.
// SLOW(>=2s): the whole issue seeded with a decision, two readings, an approval and a child written
// over it and waited on in the tree read, then loaded and unfolded, and three views turned on.
test("a reading stands under what it reads and a child's check under the statement it serves", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedModelRows, async (page, seeded) => {
    await openOutline(page, seeded.key);
    expect((await treeAnswerOf(seeded.key)).converted, NOT_CONVERTED).toBe(true);
    const { child, decision, decisionReading, question, questionReading, statementSeven } = seeded;

    // (1) The agent decision's reading under it, as the agent's; the question's under it.
    await expect
      .poll(() => parentOfRow(page, decisionReading.row), {
        message: "the agent decision's reading stands under it",
      })
      .toBe(decision.row);
    await expect(
      nodeRow(page, decisionReading.row).first().locator("[data-node-margin] [data-node-tag]"),
      "the decision's reading does not carry its kind's tag"
    ).toHaveText(NODE_TYPE_NAMES.reading);
    const rows = await drawnRows(page);
    expect(
      rowOf(rows, decisionReading.row)?.author,
      "the decision's reading is not named by its agent's role"
    ).toBe(agentRoleWord(PLANNER_TITLE));
    expect(
      rowOf(rows, decisionReading.row)?.authorShape,
      "the decision's reading is not drawn as an agent's"
    ).toBe(GLYPH_TOKENS.authors.agent);
    const layouts = await rowLayouts(page, seeded.specSlug);
    const readingLayout = layouts.find((layout) => layout.id === decisionReading.row);
    const agentTurn = layouts.find((layout) => layout.id === seeded.messages.root);
    expect(
      readingLayout?.bullet.fill ?? "no row of the reading",
      "the decision's reading is not drawn in the agents' grey, as the planner's message is"
    ).toBe(agentTurn?.bullet.fill ?? "no row of the planner's message");
    expect(
      readingLayout?.author?.color ?? "no author",
      "the decision's reading does not name its agent in grey"
    ).toBe(readingLayout?.quiet);
    await expect
      .poll(() => parentOfRow(page, questionReading.row), {
        message: "the answered question's reading is not a row under it",
      })
      .toBe(question.row);

    // (3) The child's check, its result and its plan task under statement 7, marked with the
    // child's key, and none of them under the child's own row.
    await expect
      .poll(() => parentOfRow(page, child.check.row), {
        message: "the child's check stands under statement 7",
      })
      .toBe(statementSeven.row);
    await expect
      .poll(() => parentOfRow(page, child.result), {
        message: "the child's check draws its result under it",
      })
      .toBe(child.check.row);
    await expect
      .poll(() => parentOfRow(page, child.task.row), {
        message: "the child's plan task does not stand under statement 7",
      })
      .toBe(statementSeven.row);
    for (const [node, what] of [
      [child.check, "check"],
      [child.task, "plan task"],
    ] as const) {
      const mark = nodeRow(page, node.row).first().locator("[data-node-issue]");
      await expect(mark, `the child's ${what} is not marked with the child's key`).toHaveText(
        child.key
      );
      await expect(mark, `the child's ${what} does not link to the child`).toHaveAttribute(
        "href",
        `/issues/${child.key}`
      );
    }
    const unfolded = await drawnRows(page);
    expect(rowOf(unfolded, child.key), "the child issue draws no row of its own").toBeDefined();
    expect(
      idsUnder(unfolded, child.key).filter((id) =>
        [child.check.row, child.task.row, child.result].includes(id)
      ),
      "a row of the child's check, result or plan task stands under the child's own row"
    ).toEqual([]);
    await expectStatementSevenChecked(page, statementSeven.row, "on the outline");
    await pickPreset(page, PRESET_NAMES.verification);
    await expect
      .poll(() => parentOfRow(page, child.check.row), {
        message: "under Verification the child's check is not a row under statement 7",
      })
      .toBe(statementSeven.row);
    await expectStatementSevenChecked(page, statementSeven.row, "under Verification");

    // (2) Under Decisions the decision's reading under it, and the question's nowhere.
    await pickPreset(page, PRESET_NAMES.decisions);
    await expect
      .poll(() => parentOfRow(page, decisionReading.row), {
        message: "under Decisions the decision's reading is not a row under it",
      })
      .toBe(decision.row);
    await expect(
      nodeRow(page, questionReading.row),
      "Decisions draws the answered question's reading"
    ).toHaveCount(0);

    // (4) No model's word drawn whole but on a kind tag, and no uuid, under Decisions and on the
    // outline.
    expect(
      await plainWordsOffences(page),
      "under Decisions an element but a kind tag reads a model's word, or the text holds a uuid"
    ).toEqual([]);
    await pickPreset(page, PRESET_NAMES.conversation);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    expect(
      await plainWordsOffences(page),
      "on the outline an element but a kind tag reads a model's word, or the text holds a uuid"
    ).toEqual([]);
  });
});

// A reading c-readings owes, in a row of its own beside row 2: the child's check under statement
// 7 stands as its passing result gives it, "passed". The page reads that standing from the tree
// read: a result node's `verdict`, `retracted` and `check_sha256`, and the check node's
// `text_sha256`, which the page's tree read asks of the server; until the server answers them the
// check draws no standing word, and the row is skipped. Its `@owed` tag is what c-readings' run
// leaves out (`--grep-invert @owed`), since a pass counts no skipped case; the change that makes
// the server answer them takes the tag off, and c-readings' `-g` then selects the row.
// SLOW(>=2s): the whole issue seeded with a decision, two readings, an approval and a child written
// over it and waited on in the tree read, then loaded and unfolded.
test("a reading stands under what it reads, and a child's check draws the standing its results give @owed", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedModelRows, async (page, seeded) => {
    const answer = await treeAnswerOf(seeded.key);
    expect(answer.converted, NOT_CONVERTED).toBe(true);
    const { child } = seeded;
    const result = answer.nodes.find((node) => node.id === child.result);
    const check = answer.nodes.find((node) => node.id === child.check.id);
    test.skip(
      result?.verdict == null || result.check_sha256 == null || check?.text_sha256 == null,
      "owed: the child's check's standing needs a result node's verdict, retracted and check_sha256 and a check node's text_sha256 in the tree read"
    );

    await openOutline(page, seeded.key);
    await expect(
      nodeRow(page, child.check.row).first(),
      "the child's check does not stand as its passing result says"
    ).toHaveAttribute("data-node-state", "passed" satisfies CheckState);
  });
});

// c-editing. On a converted issue, Enter on a statement, on a check of `verification.md` and on a
// criterion written into the issue puts the caret at the start of each one's words, each through
// its own fragment of the issue's one room: the spec's and `verification.md`'s artifact reads
// answer the tree artifact as their room, the criterion's editor opens the tree artifact after one
// artifact read of it, each Enter opens at most one socket, and what is typed in all three is in
// the tree read after a reload. A comment and an ask take no caret, and a check of the child issue
// says it is edited on its own issue. At 1440 px.
// c-editing's (4), an open editor that stays writable when the block schema changes, is not in
// this row: its helpers (`applySupertagMigration`, `restoreStoreSchema`) are not among the e2e
// helpers yet; the step and the title's words come with them.
// SLOW(>=2s): the whole issue seeded with a decision, two readings, an approval and a child written
// over it and waited on in the tree read, a criterion written and waited on too, then three
// editors opened and typed in, a reload, and the tree read polled for the stored words.
test("a node is edited where it lives", async ({ browser }) => {
  test.setTimeout(240_000);
  await phase(browser, seedModelRows, async (page, seeded) => {
    const criterion = await writeCriterionApi(
      seeded.key,
      seeded.tree,
      seeded.thread.address,
      `${WRITTEN.edited} is a criterion written through the page's own request.`
    );
    const traffic = watchApiRequests(page);
    await openOutline(page, seeded.key);
    expect((await treeAnswerOf(seeded.key)).converted, NOT_CONVERTED).toBe(true);

    // (1) Statement 1, then check c1: the caret at the start of each, in its document's socket.
    for (const [node, artifact, word, what] of [
      [seeded.statementOne, seeded.specArtifactId, TYPED.statement, "statement 1"],
      [seeded.checkOne, seeded.verificationArtifactId, TYPED.check, "check c1"],
    ] as const) {
      await caretInto(page, {
        artifact,
        id: node.row,
        message: `Enter on ${what} does not put the caret in it`,
        what,
      });
      await page.keyboard.type(`${word} `);
      await page.keyboard.press("Escape");
      await expect
        .poll(() => caretInNode(page), { message: `Escape does not take the caret out of ${what}` })
        .toBe(false);
    }

    // (2) The criterion: the caret in it, through the tree artifact's own socket, after one
    // artifact read of it; the two documents' rooms are the tree artifact.
    traffic.reset();
    await caretInto(page, {
      artifact: seeded.tree,
      id: criterion,
      message: "the criterion is edited in the issue's room",
      what: "the criterion",
    });
    expect(
      traffic.requests.filter(
        (request) =>
          request.method === "GET" &&
          request.path.split("?")[0] === `/api/v1/artifacts/${seeded.tree}`
      ).length,
      "the first Enter on the criterion read the tree artifact other than once"
    ).toBe(1);
    expect(
      await artifactRoomOf(seeded.specArtifactId),
      "the spec's artifact read does not answer the issue's tree artifact as its room"
    ).toBe(seeded.tree);
    expect(
      await artifactRoomOf(seeded.verificationArtifactId),
      "verification.md's artifact read does not answer the issue's tree artifact as its room"
    ).toBe(seeded.tree);
    await page.keyboard.type(`${TYPED.criterion} `);
    await page.keyboard.press("Escape");
    await expect
      .poll(() => caretInNode(page), {
        message: "Escape does not take the caret out of the criterion",
      })
      .toBe(false);

    await page.reload();
    await issueRead(page);
    await expectIssuePageDrawn(page);
    await expect
      .poll(
        async () => {
          const { nodes } = await treeAnswerOf(seeded.key);
          const startsWith = (id: string, word: string) =>
            nodes
              .find((node) => node.id === id)
              ?.text.trimStart()
              .startsWith(word) ?? false;
          return {
            check: startsWith(seeded.checkOne.id, TYPED.check),
            criterion: startsWith(criterion, TYPED.criterion),
            statement: startsWith(seeded.statementOne.id, TYPED.statement),
          };
        },
        {
          message:
            "after a reload the tree read does not hold the words typed at each node's start",
          timeout: 20_000,
        }
      )
      .toEqual({ check: true, criterion: true, statement: true });

    // (3) A comment and the answered question take no caret; the child's check says it is edited
    // on its own issue.
    await unfoldEveryBranch(page);
    await expectNoCaret(page, seeded.thread.row, KEY_REPORTS.no_text, "a comment");
    await expectNoCaret(page, seeded.question.row, KEY_REPORTS.no_text, "the answered question");
    await expectNoCaret(
      page,
      seeded.child.check.row,
      KEY_REPORTS.issue_above,
      "the child's check under statement 7"
    );
  });
});
