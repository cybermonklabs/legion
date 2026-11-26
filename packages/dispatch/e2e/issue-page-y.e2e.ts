/** A criterion the server writes into the issue's tree, a child's checks the parent's tree read
 *  answers, and the issue room the server admits each editor to. */
import type { Locator, Page } from "@playwright/test";

import { agentRoleWord } from "../web/src/features/issue-page/agent-role";
import { presetPath } from "../web/src/features/issue-page/issue-address";
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
  ROW_MARK_WORDS,
  VERIFICATION_WORDS,
} from "../web/src/features/issue-page/page-words";
import {
  buildDispatchReference,
  parseDispatchReference,
  shortForm,
} from "../web/src/features/refs/routes";
import {
  editArtifact,
  getArtifact,
  getArtifactText,
  getIssueCoverage,
  quiesceDocuments,
} from "./api";
import {
  applySupertagMigration,
  builtSchemaVersion,
  installedVersion,
  restoreStoreSchema,
  servesAddedType,
} from "./block-schema";
import { countDocumentSockets } from "./editor";
import { nextFrames } from "./frames";
import { seedWholeIssue } from "./issue-layout-seed";
import { asPlanner, PLANNER_TITLE, readTree } from "./issue-layout-seed-support";
import { gotoIssue, issuePage, issueRead, phase, presetChooser, presetCount } from "./issue-page";
import {
  blockNode,
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
  rowMore,
  rowOf,
  rowWords,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  caretInNode,
  expectActivePreset,
  focusedNodeId,
  onScreen,
  openPreset,
  pickPreset,
  rowGaps,
  rowReference,
} from "./issue-page-i";
import { planChart, planView } from "./issue-page-k";
import { composerField, composerTarget } from "./issue-page-l";
import { decisionsFoot } from "./issue-page-q";
import { outlineAnswerOf } from "./issue-page-t-tree";
import { verificationCard, verificationCheck } from "./issue-page-v";
import { heldTreeAnswer } from "./issue-page-w";
import {
  childItems,
  type DrawnMark,
  filterCount,
  heldUnder,
  marginComments,
  type NodeStateSeed,
  ownMarks,
  questionReadId,
  seedNodeStates,
  treeItemsDrawn,
  writeNodeState,
} from "./issue-page-x";
import {
  artifactRoomOf,
  criterionAct,
  criterionLine,
  criterionRefusal,
  criterionRows,
  seedLongWords,
  seedModelRows,
  TREE_READ_OWED,
  treeAnswerOf,
  treeReadServed,
  treeWordsOf,
  type WrittenCriterion,
  writeCriterionApi,
} from "./issue-page-y";
import { settledState } from "./settled";
import { expect, test } from "./test";
import {
  type ApiRequest,
  type ApiRequests,
  expectLoadSettled,
  type RouteCount,
  watchApiRequests,
} from "./traffic";

/**
 * Six browser rows read the new issue page. The first writes a criterion under a comment;
 * the second checks where readings and a child's check are drawn, with a row of its own beside
 * it for the child's check's standing. The third edits nodes on the outline and the fourth
 * edits in place in the filters, both through the issue's one room. The fifth checks that
 * Verification draws a long statement and a long check whole on desktop and phone. The sixth
 * reads part X's marks, open branches and fold counts on criteria.
 *
 * `phase` opens the real stack as alice with the new issue page on. The first four rows and
 * the standing row use `seedModelRows`, and the sixth part X's `seedNodeStates` with criteria
 * written over it (`seedCriterionStates`); each skips when the server does not serve the issue's
 * tree read. The fifth uses `seedLongWords`, which waits on coverage and results instead.
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
  liveCriterion: "Quillmoor",
  overtakenCriterion: "Rookhaven",
  proposedCriterion: "Penhallow",
  refused: "Jasperfell",
  underOvertaken: "Thornwick",
} as const;

/** One word each rows 3 and 4 type into a node's words. */
const TYPED = {
  afterSchema: "Briarford",
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

/** The first load of issue `key`, by route, read once the page has gone quiet. The server first
 *  settles every document the seed wrote (`quiesceDocuments`), while no page holds one open, so
 *  no settlement of the seed's lands during the load or after it: one that lands later can write
 *  a version of the spec, and move alice's approval with it. */
async function firstLoad(page: Page, key: string): Promise<readonly RouteCount[]> {
  await quiesceDocuments();
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

/** Fails unless the row of `row`, a node of the child `child`, is marked with the child's key as
 *  a link to the child, read `where`. */
async function expectChildMark(
  page: Page,
  row: string,
  child: string,
  what: string,
  where: string
): Promise<void> {
  const mark = nodeRow(page, row).first().locator("[data-node-issue]");
  await expect(mark, `the child's ${what} is not marked with the child's key ${where}`).toHaveText(
    child
  );
  await expect(mark, `the child's ${what} does not link to the child ${where}`).toHaveAttribute(
    "href",
    `/issues/${child}`
  );
}

/** The first word of node `id`'s words in `words` (`treeWordsOf`), or null where it has none: the
 *  word typed where Enter puts the caret, at the start of the node's words. */
function firstWord(words: ReadonlyMap<string, string>, id: string): string | null {
  return words.get(id)?.split(/\s+/, 1)[0] ?? null;
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

/** Where `caretBy` and `caretInto` put the caret: the id of the row that holds the focus once the
 *  caret is in, the artifact whose socket its editor opens, what a failure names the node by, and
 *  the message of the first assertion. */
interface CaretTarget {
  readonly artifact: string;
  readonly id: string;
  readonly message: string;
  readonly what: string;
}

/** Puts the focus on a filter's row and answers it, as `focusRow` does on the outline, without
 *  waiting for the outline's one body: a Verification statement has none of its own, and a check
 *  can draw both its facts and its results' facts. */
async function focusFilterRow(row: Locator): Promise<Locator> {
  await row.focus();
  await expect(row).toBeFocused();
  return row;
}

/** Puts the caret with Enter in the row the caller has focused, with `focusRow` on the outline
 *  and `focusFilterRow` in a filter, so the sockets are counted after the focus and before the
 *  press. */
async function caretInto(page: Page, target: CaretTarget): Promise<void> {
  await caretBy(page, target, "Enter", () => page.keyboard.press("Enter"));
}

/** Fails unless the act puts the caret in the named row and opens its artifact's socket,
 * with at most one new document socket in all. Count from just before the act so sockets
 * opened by earlier steps do not count. */
async function caretBy(
  page: Page,
  target: CaretTarget,
  how: string,
  act: () => Promise<void>
): Promise<void> {
  const own = countDocumentSockets(page, target.artifact);
  const all = countDocumentSockets(page);
  await act();
  await expect
    .poll(async () => (await caretInNode(page)) && (await focusedNodeId(page)) === target.id, {
      message: target.message,
    })
    .toBe(true);
  await expect
    .poll(own, { message: `the editor of ${target.what} does not open its artifact's socket` })
    .toBeGreaterThan(0);
  expect(all(), `${how} on ${target.what} opened more than one socket`).toBeLessThanOrEqual(1);
}

/** Fails unless Enter on `row`, which the caller has focused, puts no caret, opens no document
 *  socket, and the row says `report`. */
async function expectNoCaret(
  page: Page,
  row: Locator,
  report: string,
  what: string
): Promise<void> {
  const sockets = countDocumentSockets(page);
  await page.keyboard.press("Enter");
  await expect(
    row.locator("[data-row-report]"),
    `Enter on ${what} does not say why it takes no caret`
  ).toHaveText(report);
  expect(await caretInNode(page), `Enter on ${what} put a caret somewhere`).toBe(false);
  expect(sockets(), `Enter on ${what} opened a document socket`).toBe(0);
}

/** Escape leaves the editor and returns the focus to the row that opened it. */
async function escapeToRow(page: Page, row: Locator, what: string): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(row, `Escape does not give back ${what}'s row`).toBeFocused();
  expect(await caretInNode(page), `Escape left the caret in ${what}`).toBe(false);
}

/** The check and its Facts control report the same state, and the check's own facts match it. */
async function expectFacts(check: Locator, open: boolean): Promise<void> {
  await expect(check).toHaveAttribute("aria-expanded", String(open));
  await expect(check.locator("[data-verification-facts]")).toHaveAttribute(
    "aria-expanded",
    String(open)
  );
  if (open) await expect(rowBody(check).first()).toBeVisible();
  else await expect(rowBody(check)).toHaveCount(0);
}

/** The row of check `check` on the card of statement `statement`: the Verification view draws a
 *  check once under each statement it serves, so its id alone can name more than one row. */
function checkUnder(page: Page, statement: string, check: string): Locator {
  return verificationCheck(page, check).and(
    verificationCard(page, statement).locator("[data-verification-check]")
  );
}

/**
 * Brings `row` into view as a reader's scroll does: through the band just below the view, where
 * the page measures words it may cut at six lines. A view that drew such a cut would measure it
 * (`NodeRow.tsx`'s `useClamp`) only as a row mounts in view, or while the row stands within one
 * screen of the view but wholly outside it; a row already in view keeps its length. A long row
 * that mounts screens down, as on a phone, and that Playwright's scroll jumps straight into view
 * is never measured, so it would be drawn whole even with the cut put back. So the row first
 * stands half a screen below the view's bottom for two frames, where the measurer reads it, and
 * only then comes into view.
 */
async function scrollInAsReader(page: Page, row: Locator): Promise<void> {
  await row.evaluate((element) => {
    const top = element.getBoundingClientRect().top + window.scrollY;
    window.scrollTo(0, Math.max(0, top - 1.5 * window.innerHeight));
  });
  await nextFrames(page);
  await row.scrollIntoViewIfNeeded();
  await nextFrames(page);
}

// c-write. On the seed's first comment thread, `f` reaches Write a criterion and Enter aims the
// box at the comment; what alice types and sends is a criterion under that comment, as typed,
// drawn as hers; it stays after a reload, Spec counts it, and the header's chip reads the version
// alice's approval stood at just before the send, as the API read it then, against the later one
// the criterion made. Escape before a send writes nothing; the child, a view of the seed, and a
// check's row offer no Write a criterion; a refused send keeps its text and says why; and the
// send costs one write and one or two tree reads, on a first load that sends by route no more
// than a bare `seedWholeIssue` issue's. At 1440 px, and on `iphone` at 390 px, where the box's
// placeholder takes its phone form.
//
// Before (1) it loads a bare `seedWholeIssue` issue, a second seed of the row, and counts its first
// load by route, then counts the seeded issue's the same way, each load once the server has
// settled every document its seed wrote (`firstLoad`): (8) compares the two, so the first
// assertion that fails on a page without the criterion line is (1)'s.
// SLOW(>=2s): two whole issues seeded and each loaded cold until its reads settle, the second with
// a decision, two readings, an approval and a child written over it and waited on in the tree
// read; then four sends from the box, two of them refused, a reload and a load of the child.
test("a person writes a criterion under the words they are reading @phone", async ({
  browser,
}, testInfo) => {
  test.skip(!(await treeReadServed()), TREE_READ_OWED);
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

    // (1) `f` reaches the act on the focused comment, and Enter aims the box at the comment, whose
    // target line names the comment by its author and its kind and draws none of its words.
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
      "the box does not name the comment's author as the page names alice"
    ).toContainText(READER);
    await expect(
      composerTarget(page),
      "the box does not name the comment by the page's word for its kind"
    ).toContainText(NODE_TYPE_NAMES.comment);
    await expect(
      composerTarget(page),
      "the box draws the comment's words on its target line"
    ).not.toContainText(thread.firstLine);
    await expect(
      composerField(page),
      `the box does not say what to write at ${size.width} px`
    ).toHaveAttribute(
      "placeholder",
      phone ? CONTROL_LABELS.criterion_placeholder_phone : CONTROL_LABELS.criterion_placeholder
    );

    // (2) Two lines with a reference, sent: a row under the comment, as typed, alice's, focused
    // and on screen. Just before the send the spec's approval, read through the API and not by
    // the page, stands at the spec's version then current: (4) reads it against the criterion's.
    const reference = `dispatch://${seeded.parentKey}`;
    const typed = `${WRITTEN.first}: a reader finds every statement this issue asks for.\nIt starts from ${reference} and its spec.`;
    await composerField(page).fill(typed);
    const standing = (await getArtifact(seeded.specArtifactId)).approval;
    expect(
      { state: standing?.state, version: standing?.version },
      "just before the send the spec's approval does not stand at its current version"
    ).toEqual({ state: "approved", version: expect.any(Number) });
    const approvedVersion = standing?.version ?? 0;
    const versionBeforeSend = standing?.latest_version ?? 0;
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

    // (4) The header's chip reads the version alice's approval stood at just before the send
    // against the later one the criterion made.
    const latest = (await getArtifact(seeded.specArtifactId)).approval?.latest_version ?? 0;
    expect(latest, "the criterion made no later version of the spec").toBeGreaterThan(
      versionBeforeSend
    );
    await expect(
      issuePage(page).locator(`[data-approval-artifact="${seeded.specArtifactId}"]`).first(),
      "the header's chip does not read the approved version against a later one"
    ).toHaveText(
      `${APPROVAL_STATE_WORDS.approved} v${approvedVersion} · ${CONTROL_LABELS.approval_latest} v${latest}`
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
// tagged "reading", in the agents' grey and named by its agent's role. The child's check is a row
// under the statement it serves, marked with the child's key as a link, in Conversation and in
// Graph; Conversation's read names no result and no plan task (`CONVERSATION_KINDS`), so there
// the check draws neither, while Graph, which reads the whole tree, draws the check's result under
// it and the child's plan task under the statement, and none of them under the child's own row.
// That statement draws no "No check" in either, and under Verification it is covered: the child's
// check is a row under it with its result. Under Decisions the decision's reading is a row under
// it and the question's is not drawn; and no element but a kind tag reads a model's word whole,
// nor any text a uuid. At 1440 px. How the child's check stands, which its result gives, is the
// next row's.
//
// The steps are asserted in the order (1), (3), (2), (4). On a page without the reading rows, no
// step drafts a reading, so the row fails there on (1)'s first assertion.
// SLOW(>=2s): the whole issue seeded with a decision, two readings, an approval and a child written
// over it and waited on in the tree read, then loaded and unfolded, and four views turned on.
test("a reading stands under what it reads and a child's check under the statement it serves", async ({
  browser,
}) => {
  test.skip(!(await treeReadServed()), TREE_READ_OWED);
  test.setTimeout(240_000);
  await phase(browser, seedModelRows, async (page, seeded) => {
    const traffic = watchApiRequests(page);
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
    // The agents' grey is the colour `rowLayouts` resolves from the theme's own token for a row of
    // an agent's words that no kind colours (`--color-page-bullet`), not any other row's.
    const readingLayout = (await rowLayouts(page, seeded.specSlug)).find(
      (layout) => layout.id === decisionReading.row
    );
    expect(
      readingLayout?.bullet.fill ?? "no row of the reading",
      "the decision's reading is not drawn in the agents' grey"
    ).toBe(readingLayout?.bullet.kindFill);
    expect(
      readingLayout?.author?.color ?? "no author",
      "the decision's reading does not name its agent in grey"
    ).toBe(readingLayout?.quiet);
    await expect
      .poll(() => parentOfRow(page, questionReading.row), {
        message: "the answered question's reading is not a row under it",
      })
      .toBe(question.row);

    // (3) In Conversation the child's check under statement 7, marked with the child's key, and no
    // row of its result or of the child's plan task, which Conversation's read does not name.
    await expect
      .poll(() => parentOfRow(page, child.check.row), {
        message: "the child's check stands under statement 7",
      })
      .toBe(statementSeven.row);
    await expectChildMark(page, child.check.row, child.key, "check", "in Conversation");
    await expect(
      nodeRow(page, child.result),
      "Conversation draws a row of the child's result, a kind its read does not name"
    ).toHaveCount(0);
    await expect(
      nodeRow(page, child.task.row),
      "Conversation draws a row of the child's plan task, a kind its read does not name"
    ).toHaveCount(0);
    await expectStatementSevenChecked(page, statementSeven.row, "in Conversation");

    // (3) In Graph the child's check with its result, and its plan task, under statement 7, each
    // marked with the child's key, and none of them under the child's own row.
    await pickSettled(page, traffic, PRESET_NAMES.graph);
    await unfoldEveryBranch(page);
    await expect
      .poll(() => parentOfRow(page, child.check.row), {
        message: "in Graph the child's check does not stand under statement 7",
      })
      .toBe(statementSeven.row);
    await expect
      .poll(() => parentOfRow(page, child.result), {
        message: "in Graph the child's check does not draw its result under it",
      })
      .toBe(child.check.row);
    await expect
      .poll(() => parentOfRow(page, child.task.row), {
        message: "in Graph the child's plan task does not stand under statement 7",
      })
      .toBe(statementSeven.row);
    await expectChildMark(page, child.check.row, child.key, "check", "in Graph");
    await expectChildMark(page, child.task.row, child.key, "plan task", "in Graph");
    const unfolded = await drawnRows(page);
    expect(
      rowOf(unfolded, child.key),
      "in Graph the child issue draws no row of its own"
    ).toBeDefined();
    expect(
      idsUnder(unfolded, child.key).filter((id) =>
        [child.check.row, child.task.row, child.result].includes(id)
      ),
      "in Graph a row of the child's check, result or plan task stands under the child's own row"
    ).toEqual([]);
    await expectStatementSevenChecked(page, statementSeven.row, "in Graph");

    // (3) Under Verification statement 7 is covered: the child's check is a row under it, with
    // its result, and it draws no "No check".
    await pickPreset(page, PRESET_NAMES.verification);
    await expect
      .poll(() => parentOfRow(page, child.check.row), {
        message: "under Verification the child's check is not a row under statement 7",
      })
      .toBe(statementSeven.row);
    const verifiedCheck = checkUnder(page, statementSeven.row, child.check.row);
    await expect(
      verifiedCheck.locator(`[data-verification-result][data-node-id="${child.result}"]`),
      "under Verification the child's check under statement 7 draws no row of its result"
    ).toHaveCount(1);
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

// c-readings, in a row of its own beside row 2: the child's check under statement 7 stands as its
// passing result gives it, "passed". The page reads that standing from the tree read alone: the
// check node's `check.text_sha256` and its result node's `result` (`verdict`, `retracted`,
// `check_sha256`), read by F's one reading of a check (`model-rows/model-drafts.ts`'
// `borrowedStanding`). It is read in Graph, whose read is the whole tree and so holds the result
// under the check: Conversation's read names no result (`CONVERSATION_KINDS`). The row first reads
// that Graph's own read holds the one pass over the check's words as they stand, so the standing
// drawn is the one that read gives. c-readings' `-g` selects it with row 2.
// SLOW(>=2s): the whole issue seeded with a decision, two readings, an approval and a child written
// over it and waited on in the tree read, then loaded in Graph and unfolded.
test("a reading stands under what it reads, and a child's check draws the standing its results give", async ({
  browser,
}) => {
  test.skip(!(await treeReadServed()), TREE_READ_OWED);
  test.setTimeout(240_000);
  await phase(browser, seedModelRows, async (page, seeded) => {
    expect((await treeAnswerOf(seeded.key)).converted, NOT_CONVERTED).toBe(true);
    const { child } = seeded;
    const traffic = watchApiRequests(page);
    const graph = outlineAnswerOf(page, "graph");
    await openPreset(page, seeded.key, "graph");
    await expectActivePreset(page, PRESET_NAMES.graph);
    await expectLoadSettled(traffic);
    // The seed waited until the tree read held the result under the check, so the Graph read that
    // holds the check holds its result too.
    const answer = await heldTreeAnswer(graph, "Graph's load", (held) =>
      (held.nodes ?? []).some((node) => node.id === child.check.id)
    );
    const nodes = answer.nodes ?? [];
    const words = nodes.find((node) => node.id === child.check.id)?.check?.text_sha256;
    expect(words, "Graph's read answers no sum of the child's check's words").toBeDefined();
    const results = nodes.filter(
      (node) => node.supertag === "result" && node.parent?.id === child.check.id
    );
    expect(
      results.map(({ result }) => ({
        retracted: result?.retracted ?? null,
        verdict: result?.verdict,
        words: result?.check_sha256,
      })),
      "Graph's read does not hold one pass nobody withdrew over the child's check's words as they stand"
    ).toEqual([{ retracted: null, verdict: "pass", words }]);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
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
// says it is edited on its own issue; and an open editor survives a supertag migration of the
// block schema, stays writable, stores further words with no reload notice, and restores the store
// schema in finally. At 1440 px.
// SLOW(>=2s): the whole issue seeded with a decision, two readings, an approval and a child written
// over it and waited on in the tree read, a criterion written and waited on too, then three
// editors opened and typed in, a reload, the tree read polled for the stored words, and a live
// supertag migration across an open editor.
test("a node is edited where it lives and an open editor survives a schema change", async ({
  browser,
}) => {
  test.skip(!(await treeReadServed()), TREE_READ_OWED);
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
      await focusRow(page, node.row);
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
    await focusRow(page, criterion.row);
    await caretInto(page, {
      artifact: seeded.tree,
      id: criterion.row,
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
    // After a reload each node's words, as the page reads them (`treeWordsOf`), start with the
    // word typed after Enter, which puts the caret at their start. A typed block's stored `text`
    // opens with its directive's fence, so its words are read from its display tree.
    await expect
      .poll(
        async () => {
          const words = await treeWordsOf(seeded.key);
          return {
            check: firstWord(words, seeded.checkOne.id),
            criterion: firstWord(words, criterion.id),
            statement: firstWord(words, seeded.statementOne.id),
          };
        },
        {
          message:
            "after a reload the tree read does not hold the words typed at each node's start",
          timeout: 20_000,
        }
      )
      .toEqual({ check: TYPED.check, criterion: TYPED.criterion, statement: TYPED.statement });

    // (3) A comment and the answered question take no caret; the child's check says it is edited
    // on its own issue.
    await unfoldEveryBranch(page);
    await expectNoCaret(
      page,
      await focusRow(page, seeded.thread.row),
      KEY_REPORTS.no_text,
      "a comment"
    );
    await expectNoCaret(
      page,
      await focusRow(page, seeded.question.row),
      KEY_REPORTS.no_text,
      "the answered question"
    );
    await expectNoCaret(
      page,
      await focusRow(page, seeded.child.check.row),
      KEY_REPORTS.issue_above,
      "the child's check under statement 7"
    );

    // (4) With the caret in statement 1, a supertag migration moves the block schema under the
    // running server: the open editor reconnects writable, takes more words that are stored, and
    // shows no reload notice; the store schema is restored in `finally`.
    await restoreStoreSchema();
    await expect.poll(() => installedVersion()).toBe(builtSchemaVersion);
    expect(await servesAddedType()).toBe(false);
    const statementRow = await focusRow(page, seeded.statementOne.row);
    await caretInto(page, {
      artifact: seeded.specArtifactId,
      id: seeded.statementOne.row,
      message: "Enter on statement 1 before the schema change does not put the caret in it",
      what: "statement 1 before the schema change",
    });
    const reconnects = countDocumentSockets(page, seeded.specArtifactId);
    try {
      await applySupertagMigration();
      await expect
        .poll(() => servesAddedType(), {
          message: "the server did not install the migrated block schema",
          timeout: 15_000,
        })
        .toBe(true);
      await expect
        .poll(reconnects, {
          message:
            "the open editor did not open a fresh document socket after the block schema switched",
          timeout: 15_000,
        })
        .toBeGreaterThanOrEqual(1);
      await expect(
        statementRow.getByRole("article", { exact: true, name: "Document" }),
        "the open editor became read-only after the schema change"
      ).toHaveAttribute("data-read-only", "false");
      await expect
        .poll(
          async () =>
            (await caretInNode(page)) && (await focusedNodeId(page)) === seeded.statementOne.row,
          {
            message: "the caret did not stay in statement 1 after the schema change",
            timeout: 15_000,
          }
        )
        .toBe(true);
      await expect(
        page.getByText("Reload to edit."),
        "the page asked to reload after the schema change"
      ).toHaveCount(0);
      await page.keyboard.type(`${TYPED.afterSchema} `);
      await expect(
        page.getByText("Reload to edit."),
        "the page asked to reload while typing after the schema change"
      ).toHaveCount(0);
      await expect
        .poll(async () => (await getArtifactText(seeded.specArtifactId)).markdown, {
          message: "the words typed after the schema change were not stored in the spec",
          timeout: 30_000,
        })
        .toContain(TYPED.afterSchema);
      await expect
        .poll(
          async () =>
            (await treeAnswerOf(seeded.key)).nodes.find(
              (node) => node.id === seeded.statementOne.id
            )?.text ?? "",
          {
            message: "the tree read does not hold the words typed after the schema change",
            timeout: 20_000,
          }
        )
        .toContain(TYPED.afterSchema);
    } finally {
      await restoreStoreSchema();
      await expect.poll(() => servesAddedType(), { timeout: 15_000 }).toBe(false);
    }
  });
});

// In Spec a statement and a criterion written into the issue are edited in place, each through
// its own document's socket. In Verification a statement and its check are edited in place in
// their issue's room, but a borrowed check is not. The check's Facts control and `o`, not a press
// on its words, keep facts drawn. Decisions edits a decision and its reading, but not an answered
// question. Plan edits a task under Steps, while Enter on its chart goes to the task in
// Conversation.
// SLOW(>=2s): seeds the whole converted issue and a criterion, opens seven editors, reloads to
// read the edits, and switches among four filters.
test("a node is edited in place in each filter", async ({ browser }) => {
  test.skip(!(await treeReadServed()), TREE_READ_OWED);
  test.setTimeout(240_000);
  await phase(browser, seedModelRows, async (page, seeded) => {
    const criterion = await writeCriterionApi(
      seeded.key,
      seeded.tree,
      seeded.thread.address,
      `${WRITTEN.edited} is a criterion written through the page's own request.`
    );
    await page.setViewportSize(DESKTOP);
    expect((await treeAnswerOf(seeded.key)).converted, NOT_CONVERTED).toBe(true);
    await gotoIssue(page, `/issues/${seeded.key}`);
    const { statementOne, checkOne, statementSeven, child } = seeded;

    // In Spec the words typed after Enter go before statement 7's text, through the spec's
    // socket, and a press on the criterion's words puts the caret in it, through the socket of
    // the tree artifact it is written in.
    await pickPreset(page, PRESET_NAMES.spec);
    const specStatement = nodeRow(page, statementSeven.row).first();
    const specCriterion = nodeRow(page, criterion.row).first();
    await focusFilterRow(specStatement);
    await caretInto(page, {
      artifact: seeded.specArtifactId,
      id: statementSeven.row,
      message: "Enter on statement 7 in Spec does not put the caret in its text",
      what: "statement 7",
    });
    await page.keyboard.type(`${TYPED.statement} `);
    await escapeToRow(page, specStatement, "statement 7");
    await caretBy(
      page,
      {
        artifact: seeded.tree,
        id: criterion.row,
        message: "a press on the criterion's words in Spec does not put the caret in it",
        what: "the criterion",
      },
      "a press",
      () => rowWords(specCriterion).click()
    );
    await page.keyboard.type(`${TYPED.criterion} `);
    await escapeToRow(page, specCriterion, "the criterion");

    await pickPreset(page, PRESET_NAMES.verification);

    // The words typed after Enter go before statement 1's text. The check's words take a caret
    // from a press, and its facts are drawn while that caret holds the focus inside the row.
    const statement = verificationCard(page, statementOne.row);
    const check = checkUnder(page, statementOne.row, checkOne.row);
    await focusFilterRow(statement);
    await caretInto(page, {
      artifact: seeded.specArtifactId,
      id: statementOne.row,
      message: "Enter on statement 1 does not put the caret in its text",
      what: "statement 1",
    });
    await page.keyboard.type(`${TYPED.statement} `);
    await escapeToRow(page, statement, "statement 1");
    await caretBy(
      page,
      {
        artifact: seeded.verificationArtifactId,
        id: checkOne.row,
        message: "a press on the check's words puts the caret in it",
        what: "check c1",
      },
      "a press",
      () => rowWords(check).click()
    );
    const facts = check.locator("[data-verification-facts]");
    await expectFacts(check, true);
    await page.keyboard.type(`${TYPED.check} `);
    await escapeToRow(page, check, "check c1");
    await page.keyboard.press("j");
    await expect(check, "the press kept no facts open").toHaveAttribute("aria-expanded", "false");

    // A Facts press from outside the row opens its facts, and another shuts them. `o` keeps
    // them open even after `j` takes the focus out.
    await expect(facts).toHaveText(VERIFICATION_WORDS.facts);
    await facts.click();
    await expectFacts(check, true);
    await facts.click();
    await expectFacts(check, false);
    await expect(facts).toBeFocused();
    await page.keyboard.press("o");
    await page.keyboard.press("j");
    await expect(facts).not.toBeFocused();
    await expect(check).not.toBeFocused();
    await expectFacts(check, true);

    await page.reload();
    await issueRead(page);
    // The words typed in Spec and in Verification, as the page reads them (`treeWordsOf`), each
    // where its caret went: at the start of the words of each node Enter put the caret in,
    // statement 7 in Spec and statement 1 in Verification, and wherever the press put it in the
    // words of the criterion in Spec and of check c1 in Verification.
    await expect
      .poll(
        async () => {
          const words = await treeWordsOf(seeded.key);
          return {
            check: words.get(checkOne.id)?.includes(TYPED.check) ?? false,
            criterion: words.get(criterion.id)?.includes(TYPED.criterion) ?? false,
            starts: [statementOne.id, statementSeven.id].map((id) => firstWord(words, id)),
          };
        },
        {
          message:
            "after a reload the tree read does not hold the words typed in Spec and Verification",
          timeout: 20_000,
        }
      )
      .toEqual({ check: true, criterion: true, starts: [TYPED.statement, TYPED.statement] });
    for (const artifact of [seeded.specArtifactId, seeded.verificationArtifactId]) {
      expect(await artifactRoomOf(artifact), "the document is not edited in the issue's room").toBe(
        seeded.tree
      );
    }

    // The child's check under statement 7 can be read here but edited only on its own issue.
    const childCheck = checkUnder(page, statementSeven.row, child.check.row);
    await expectNoCaret(
      page,
      await focusFilterRow(childCheck),
      KEY_REPORTS.issue_above,
      "the child's check"
    );
    const borrowedSockets = countDocumentSockets(page);
    await rowWords(childCheck).click();
    expect(await caretInNode(page), "a press on the child's check puts a caret in it").toBe(false);
    await expect.poll(() => focusedNodeId(page)).toBe(child.check.row);
    expect(borrowedSockets(), "a press on the child's check opened a document socket").toBe(0);

    await pickPreset(page, PRESET_NAMES.decisions);
    const { decision, decisionReading, question } = seeded;
    const answered = nodeRow(page, question.row).first();
    await expect(answered).toBeVisible();
    // The planner's decision shapes no statement, so the view draws it, its reading right after
    // it, in its foot. The foot is drawn unfolded as the view opens (part U's R2), and a press of
    // its control folds an unfolded foot, so the control is pressed only where the foot stands
    // folded.
    const foot = decisionsFoot(page);
    if ((await foot.getAttribute("aria-expanded")) !== "true") await foot.click();
    await expect(foot, "Decisions does not unfold its foot").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    const decisionRow = nodeRow(page, decision.row).first();
    const reading = nodeRow(page, decisionReading.row).first();
    await expect(
      decisionRow,
      "the foot of Decisions does not draw the agent decision"
    ).toBeVisible();
    await expect(
      reading,
      "the foot of Decisions does not draw the decision's reading"
    ).toBeVisible();
    await focusFilterRow(reading);
    await caretInto(page, {
      artifact: seeded.specArtifactId,
      id: decisionReading.row,
      message: "Enter on the decision's reading does not put the caret in it",
      what: "the decision's reading",
    });
    await escapeToRow(page, reading, "the decision's reading");
    await caretBy(
      page,
      {
        artifact: seeded.specArtifactId,
        id: decision.row,
        message: "a press on the agent decision's words does not put the caret in them",
        what: "the agent decision",
      },
      "a press",
      () => rowWords(decisionRow).click()
    );
    await escapeToRow(page, decisionRow, "the agent decision");
    await expectNoCaret(
      page,
      await focusFilterRow(answered),
      KEY_REPORTS.no_text,
      "the answered question"
    );

    // The ordered steps edit the task's text; the chart opens the same task in Conversation.
    await pickPreset(page, PRESET_NAMES.plan);
    const block = seeded.planTasks[0];
    if (block === undefined) throw new Error("the seed has no plan task");
    const { slug: planSlug } = await getArtifact(seeded.planArtifactId);
    const taskId = blockNode(planSlug, block);
    const task = nodeRow(page, taskId).first();
    await planView(page, "steps").click();
    await expect(planView(page, "steps")).toHaveAttribute("aria-pressed", "true");
    await focusRow(page, taskId);
    await caretInto(page, {
      artifact: seeded.planArtifactId,
      id: taskId,
      message: "Enter on the plan task under Steps does not put the caret in it",
      what: "the plan task",
    });
    await escapeToRow(page, task, "the plan task");
    await planView(page, "chart").click();
    await focusFilterRow(task.and(planChart(page).locator("[data-plan-bar-row]")));
    const chartSockets = countDocumentSockets(page);
    await page.keyboard.press("Enter");
    await expect(page, "Enter on the chart does not go to the task in Conversation").toHaveURL(
      new RegExp(`/issues/${seeded.key}#b-${block}$`)
    );
    expect(chartSockets(), "Enter on the chart opened a document socket").toBe(0);
  });
});

// The statement and the check that serves it each draw all 200 words at the project's width,
// without needing a control to open their text: each is as tall as its words, draws no
// `CONTROL_LABELS.read_more`, and draws every word written. The height is read first: `innerText`
// reads every word of words cut short, so only the height tells a cut from words drawn whole.
// Each row comes into view as a reader scrolls (`scrollInAsReader`), so a cut put back is measured.
// SLOW(>=2s): seeds the whole issue, appends two blocks and waits for them to be indexed before
// opening Verification.
test("the Verification view draws a long statement and a long check whole @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(180_000);
  await phase(browser, seedLongWords, async (page, { key, longStatement, longCheck }) => {
    await page.setViewportSize(testInfo.project.name === "iphone" ? PHONE : DESKTOP);
    await openPreset(page, key, "verification");
    const statement = verificationCard(page, longStatement.row);
    const check = checkUnder(page, longStatement.row, longCheck.row);
    for (const [row, seeded, what] of [
      [statement, longStatement, "the statement"],
      [check, longCheck, "the check"],
    ] as const) {
      const words = rowWords(row);
      // The words are drawn before the height is read: `toContainText` reads every word of a
      // row's words, cut or not, so it waits for the drawing alone.
      await expect(words, `${what} draws none of its words`).toContainText(
        seeded.words.split(" ").at(-1) ?? seeded.words
      );
      await scrollInAsReader(page, row);
      expect(
        await words.evaluate(
          (element) => element.scrollHeight - element.getBoundingClientRect().height
        ),
        `${what} is drawn whole`
      ).toBeLessThanOrEqual(1);
      await expect(rowMore(row), `${what} draws a ${CONTROL_LABELS.read_more} control`).toHaveCount(
        0
      );
      await expect(words, `${what} does not draw every word written`).toHaveText(seeded.words, {
        useInnerText: true,
      });
    }
  });
});

/** A proposed node's mark and an overtaken one's, as part X's rows read them (`NodeMarks.tsx`):
 *  its token, the token of its shape, the shape drawn, and its word. */
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

/** How long row 6 waits for the page to draw what the read holds, and for a write of its seed to
 *  stand in a read: part X's own bounds. */
const DRAWN_MS = 30_000;
const SEED_READ_MS = 20_000;

/** The block id of the statement row 6 writes into the spec under its overtaken criterion. */
const UNDER_CRITERION_BLOCK = "r-under-criterion";

/** The kinds the Spec control counts (`presets.ts`' `COUNTED_KINDS`): statements and criteria. */
const SPEC_COUNTED: readonly string[] = ["requirement", "criterion"];

/** What row 6 reads: part X's seed, the issue's first comment thread (a comment's read id is its
 *  own id), the three criteria written under it (each by read id and page row id), and the
 *  statement written under the overtaken one. */
interface CriterionStates {
  readonly comment: string;
  readonly criteria: Readonly<Record<"live" | "overtaken" | "proposed", WrittenCriterion>>;
  readonly states: NodeStateSeed;
  readonly statement: string;
}

/**
 * Part X's `seedNodeStates` on a `seedWholeIssue` issue - whose criterion and statement stand
 * under the open question made overtaken by its ask's address - and over it, as alice, three
 * criteria written under the issue's first comment thread with the page's own write
 * (`writeCriterionApi`), in this order: one made proposed, one made overtaken, and one left live
 * whose words hold the word "proposed"; and a statement written into the spec under the
 * overtaken one, as the planner, as X writes the statement under its question. The statement is
 * waited on in the tree read under that criterion, and in the coverage read with no check serving
 * it, before the states are written, so live it would draw "No check". Each criterion is written
 * into the issue's tree artifact and carries no `document`, so its address is built from that
 * artifact's slug and the block id the read answers. The seed asserts the read answers each
 * criterion's state and its place under the comment, and waits, as X's seed does, until no
 * document of the issue owes its block index, so the coverage read holds every gap.
 */
async function seedCriterionStates(): Promise<CriterionStates> {
  const whole = await seedWholeIssue();
  const states = await seedNodeStates(whole);
  const { key } = whole;
  const tree = states.answer.tree;
  if (tree === null) throw new Error(`the tree read of ${key} names no tree artifact`);
  const comment = whole.comments.threads[0];
  if (comment === undefined) throw new Error(`${key} holds no comment thread`);
  const under = `dispatch://${key}/comment/${comment}`;
  const proposed = await writeCriterionApi(
    key,
    tree,
    under,
    `${WRITTEN.proposedCriterion}: a reader tells a criterion put forward from one that stands.`
  );
  const overtaken = await writeCriterionApi(
    key,
    tree,
    under,
    `${WRITTEN.overtakenCriterion}: a reader finds each criterion a later one replaced.`
  );
  const live = await writeCriterionApi(
    key,
    tree,
    under,
    `${WRITTEN.liveCriterion}: this criterion is proposed in its words alone, and its state is live.`
  );
  const { slug } = await getArtifact(tree);
  const written = (await readTree(key, null)).nodes ?? [];
  const addressOf = (id: string): string => {
    const block = written.find((node) => node.id === id)?.block_id;
    if (block === undefined) throw new Error(`the tree read of ${key} names no block id for ${id}`);
    return buildDispatchReference({ block: { id: block }, key, kind: "artifact", slug });
  };

  const statementWritten = await editArtifact(
    whole.specArtifactId,
    {
      ops: [
        {
          after: "end",
          markdown: `:::requirement{#${UNDER_CRITERION_BLOCK} parent="${addressOf(overtaken.id)}"}\n${WRITTEN.underOvertaken}: the list keeps each criterion a later one replaced.\n:::`,
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  expect(
    statementWritten.applied,
    "the spec does not take the statement under the overtaken criterion"
  ).toBe(1);
  let statement = "";
  await expect
    .poll(
      async () => {
        const node = ((await readTree(key, null)).nodes ?? []).find(
          (one) =>
            one.supertag === "requirement" &&
            one.block_id === UNDER_CRITERION_BLOCK &&
            (one.issue === undefined || one.issue === key)
        );
        statement = node?.id ?? "";
        return node?.parent?.id ?? null;
      },
      {
        message: "the tree read never stands the statement under the overtaken criterion",
        timeout: SEED_READ_MS,
      }
    )
    .toBe(overtaken.id);
  await expect
    .poll(
      async () =>
        (await getIssueCoverage(key)).requirements?.find(
          (requirement) => requirement.block_id === UNDER_CRITERION_BLOCK
        )?.checks ?? null,
      {
        message:
          "the coverage read never holds the statement under the overtaken criterion with no check serving it",
        timeout: SEED_READ_MS,
      }
    )
    .toBe(0);

  await writeNodeState(key, addressOf(proposed.id), "proposed");
  await writeNodeState(key, addressOf(overtaken.id), "overtaken");
  const answer = await readTree(key, null);
  for (const [id, state, what] of [
    [proposed.id, "proposed", "the proposed criterion"],
    [overtaken.id, "overtaken", "the overtaken criterion"],
    [live.id, "live", "the live criterion"],
  ] as const) {
    const node = answer.nodes?.find((one) => one.id === id);
    expect(node?.state, `the tree read does not answer ${what} ${state}`).toBe(state);
    expect(node?.parent?.id, `the tree read does not stand ${what} under the comment`).toBe(
      comment
    );
  }
  await expect
    .poll(async () => (await getIssueCoverage(key)).owed_documents ?? [], {
      message: "a document of the seeded issue still owes its block index",
      timeout: 90_000,
    })
    .toEqual([]);
  return { comment, criteria: { live, overtaken, proposed }, states, statement };
}

/** The first row the page draws for the node `readId` names (`data-node-read-id`). */
function rowOfRead(page: Page, readId: string): Locator {
  return issuePage(page).locator(`[data-node-row][data-node-read-id="${readId}"]`).first();
}

/** Loads `path` with `traffic`, the page's one watcher, and waits until the page has gone
 *  quiet. */
async function loadSettled(page: Page, traffic: ApiRequests, path: string): Promise<void> {
  traffic.reset();
  await gotoIssue(page, path);
  await expectLoadSettled(traffic);
}

/** Turns the view `name` on and waits until the page has gone quiet: a view sends its own read,
 *  and the outline lands its opening focus once that read settles. */
async function pickSettled(page: Page, traffic: ApiRequests, name: string): Promise<void> {
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  await expectLoadSettled(traffic);
}

/** Waits until the row of the node `readId` is drawn open with a row right under it for each of
 *  `under`, as part X reads a branch the page holds open, and fails with `message`. */
async function expectOpenWith(
  page: Page,
  readId: string,
  under: readonly string[],
  message: string
): Promise<void> {
  await expect
    .poll(
      async () => {
        const items = await treeItemsDrawn(page);
        const row = items.find((item) => item.readId === readId);
        const below = childItems(items, row).map((item) => item.readId);
        return { expanded: row?.expanded ?? null, held: under.every((id) => below.includes(id)) };
      },
      { message, timeout: DRAWN_MS }
    )
    .toEqual({ expanded: "true", held: true });
}

/** Folds `row` with its fold control, answers the count it draws folded (`data-folded-count`),
 *  and opens it again, naming it `what` where a press does not. */
async function foldedCount(row: Locator, what: string): Promise<number> {
  await row.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
  await expect(row, `the fold of ${what} does not fold it`).toHaveAttribute(
    "aria-expanded",
    "false"
  );
  const count = Number(await row.getAttribute("data-folded-count"));
  await row.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
  await expect(row, `the fold of ${what} does not open it again`).toHaveAttribute(
    "aria-expanded",
    "true"
  );
  return count;
}

/** Where `row` draws its marks (`[data-node-mark]`): how many stand in its margin's state line
 *  (`[data-node-margin]`) and how many in the column its words open (`[data-node-content]`), and
 *  whether that column draws its words first, so nothing stands between the bullet and them. */
function marksPlace(row: Locator): Promise<{
  readonly inContent: number;
  readonly inMargin: number;
  readonly wordsFirst: boolean;
}> {
  return row.evaluate((element) => {
    const content = element.querySelector("[data-node-content]");
    return {
      inContent: content?.querySelectorAll("[data-node-mark]").length ?? 0,
      inMargin: element.querySelectorAll("[data-node-margin] [data-node-mark]").length,
      wordsFirst: content?.firstElementChild?.matches("[data-node-words]") === true,
    };
  });
}

/** The proposed criterion carries "Proposed" and its shape in its row's state line, never between
 *  its bullet and its words, and draws no gap, read `where`. */
async function expectProposedCriterion(page: Page, id: string, where: string): Promise<void> {
  const row = nodeRow(page, id).first();
  await expect
    .poll(() => ownMarks(row), {
      message: `the proposed criterion does not carry Proposed ${where}`,
    })
    .toEqual([PROPOSED]);
  expect(
    await marksPlace(row),
    `the proposed criterion draws its mark other than in its row's state line ${where}, or between its bullet and its words`
  ).toEqual({ inContent: 0, inMargin: 1, wordsFirst: true });
  await expect(rowGaps(row), `the proposed criterion draws a gap ${where}`).toHaveCount(0);
}

// c-write, row 6: part X's R2, R3 and R5 (a proposed node marked, every node counted, an
// overtaken node drawn open at its place) read on criteria, which X draws no row for (decision
// 510 (1)). (1) On the first load, with no address, the question overtaken by its ask's address is
// drawn open with its criterion and its statement under it; (2) after the load it carries
// "Overtaken" and its shape, open with both, and its criterion draws no gap; (3) the same in
// Graph. (4) Each branch the page draws open, folded one at a time, counts what the tree read
// holds under it that Conversation draws, the overtaken question's criterion included. (5) The
// proposed criterion carries "Proposed" and its shape in its row's state line, in the outline and
// in Graph, with no gap, and under Spec it is a row carrying "Proposed", in the Spec control's
// count; the live criterion whose words hold "proposed" carries no mark. (6) The overtaken
// criterion is a row at its place among the comment's children, carrying "Overtaken" and its
// shape, open with the statement under it, which draws no gap; its own fold control folds it and
// opens it again, and the comment, folded, counts it and its statement. No criterion is reserved
// for its author: a criterion is a block, and `refs/visibility.go`'s `nodeRegistry` gives a block
// no author rule. At 1440 px.
//
// The criteria stand under the issue's first comment thread, a post the outline draws as a row,
// and not under the comment `seedWholeIssue` anchors to statement 2: that one is on a node's text,
// drawn in the margin and as no row (`tree-read/tree-nodes.ts`' `onNodeText`), so no row of it
// could hold the criteria or count them. (5)'s reading of the live criterion is made right after
// (1), before any other mark is read, so a page that reads a node's life from a word of its text
// fails there first; (1) reads the page as it first opened, before any press.
// SLOW(>=2s): a whole seeded issue with part X's ten node states, two questions with the two nodes
// under one and a lens written, three criteria and a statement written and waited on in the tree
// and coverage reads, then every branch of the outline folded and opened again, Graph and Spec
// turned on, and three loads.
test("a criterion is marked, drawn open and counted as part X draws every node", async ({
  browser,
}) => {
  test.skip(!(await treeReadServed()), TREE_READ_OWED);
  test.setTimeout(480_000);
  await phase(browser, seedCriterionStates, async (page, seeded) => {
    const { comment, criteria, states, statement } = seeded;
    const { ids } = states;
    const key = states.whole.key;
    expect((await treeAnswerOf(key)).converted, NOT_CONVERTED).toBe(true);
    const plain = presetPath(key, "conversation");
    const underQuestion = [ids.questionCriterion, ids.questionStatement];
    const traffic = watchApiRequests(page);
    const conversation = outlineAnswerOf(page, "conversation");
    const graph = outlineAnswerOf(page, "graph");

    // (1) The page as it first opened, before any press: the overtaken question open with its
    // criterion and its statement. Conversation's read names its kinds (`CONVERSATION_KINDS`) and
    // holds each question by its ask's node rather than its block (`questionReadId`).
    await gotoIssue(page, plain);
    const firstAnswer = await heldTreeAnswer(conversation, "the first load", (held) =>
      (held.nodes ?? []).some(
        (node) => node.id === criteria.overtaken.id && node.state === "overtaken"
      )
    );
    const conversationQuestion = questionReadId(firstAnswer, states, "overtakenQuestion");
    await expectOpenWith(
      page,
      conversationQuestion,
      underQuestion,
      "the overtaken question is drawn open with its criterion on the first load"
    );
    // (5)'s live criterion, the first mark read: its words hold "proposed", its state is live.
    const live = nodeRow(page, criteria.live.row).first();
    await expect(live, "the live criterion is no row of the outline").toBeVisible();
    expect(await ownMarks(live), "the live criterion carries no Proposed mark").toEqual([]);

    // (2) Once the load settled: the question's mark, open with both, and no gap on its criterion.
    await expectLoadSettled(traffic);
    await expect
      .poll(() => ownMarks(rowOfRead(page, conversationQuestion)), {
        message: "after the load the overtaken question does not carry Overtaken",
      })
      .toEqual([OVERTAKEN]);
    await expectOpenWith(
      page,
      conversationQuestion,
      underQuestion,
      "after the load the overtaken question is not drawn open with its criterion and its statement"
    );
    await expect(
      rowGaps(rowOfRead(page, ids.questionCriterion)),
      "the criterion under the overtaken question draws a gap"
    ).toHaveCount(0);

    // (3) The same in Graph, whose read holds no `kind=` filter and so holds the question's block
    // (`questionReadId`).
    await pickSettled(page, traffic, PRESET_NAMES.graph);
    const graphAnswer = await heldTreeAnswer(graph, "Graph's load", (held) =>
      (held.nodes ?? []).some(
        (node) => node.id === criteria.overtaken.id && node.state === "overtaken"
      )
    );
    const graphQuestion = questionReadId(graphAnswer, states, "overtakenQuestion");
    await expectOpenWith(
      page,
      graphQuestion,
      underQuestion,
      "the overtaken question is drawn open with its criterion and its statement in Graph"
    );
    await expect
      .poll(() => ownMarks(rowOfRead(page, graphQuestion)), {
        message: "the overtaken question does not carry Overtaken in Graph",
      })
      .toEqual([OVERTAKEN]);
    await expect(
      rowGaps(rowOfRead(page, ids.questionCriterion)),
      "the criterion under the overtaken question draws a gap in Graph"
    ).toHaveCount(0);

    // (4) Each branch folded once on a fresh load, the overtaken question first, beside what
    // Conversation's own tree read holds under it: each question is one node of that read, its
    // ask's (`questionReadId`), and a comment on a node's text and its thread count in no branch
    // (`HeldRow.margin`).
    const foldedWatch = outlineAnswerOf(page, "conversation");
    await loadSettled(page, traffic, plain);
    const answer = await heldTreeAnswer(foldedWatch, "the fold load", (held) =>
      (held.nodes ?? []).some(
        (node) => node.id === criteria.overtaken.id && node.state === "overtaken"
      )
    );
    const foldedQuestion = questionReadId(answer, states, "overtakenQuestion");
    const margin = await marginComments(key);
    expect(
      await foldedCount(rowOfRead(page, foldedQuestion), "the overtaken question"),
      "the folded overtaken question counts its criterion"
    ).toBe(heldUnder(answer, key, foldedQuestion, { margin }));
    const branches = (await treeItemsDrawn(page)).filter(
      (item) => item.expanded === "true" && item.readId !== null && item.readId !== foldedQuestion
    );
    for (const branch of branches) {
      const readId = branch.readId ?? "";
      expect(
        await foldedCount(nodeRow(page, branch.id).first(), branch.id),
        `the fold of ${branch.id} does not count what the tree read holds under it, the criteria and what stands under them included`
      ).toBe(heldUnder(answer, key, readId, { margin }));
    }

    // (5) The proposed criterion in the outline, in Graph, and under Spec, in its count.
    await expectProposedCriterion(page, criteria.proposed.row, "in the outline");
    await pickSettled(page, traffic, PRESET_NAMES.graph);
    await expectProposedCriterion(page, criteria.proposed.row, "in Graph");
    await pickSettled(page, traffic, PRESET_NAMES.spec);
    await expect
      .poll(() => ownMarks(nodeRow(page, criteria.proposed.row).first()), {
        message: "the proposed criterion does not carry Proposed under Spec",
      })
      .toEqual([PROPOSED]);
    const specCounted = new Set(
      ((await readTree(key, "spec")).nodes ?? []).flatMap((node) =>
        node.role === "query" && SPEC_COUNTED.includes(node.supertag) ? [node.id] : []
      )
    );
    expect(
      specCounted.has(criteria.proposed.id),
      "the Spec view read does not take the proposed criterion"
    ).toBe(true);
    await expect
      .poll(() => filterCount(page, "spec"), {
        message: "the Spec control's count is not every statement and criterion of its view",
      })
      .toBe(specCounted.size);

    // (6) The overtaken criterion among the comment's children, in the read's order, marked and
    // open with the statement under it, which draws no gap.
    await loadSettled(page, traffic, plain);
    const siblings = (answer.nodes ?? [])
      .filter((node) => node.parent?.id === comment)
      .map((node) => node.id);
    let drawn: readonly (string | null)[] = [];
    await expect
      .poll(
        async () => {
          const items = await treeItemsDrawn(page);
          drawn = childItems(
            items,
            items.find((item) => item.readId === comment)
          ).map((item) => item.readId);
          return drawn.includes(criteria.overtaken.id);
        },
        {
          message: "the overtaken criterion is no row among the comment's children",
          timeout: DRAWN_MS,
        }
      )
      .toBe(true);
    expect(
      drawn.filter((id) => id !== null && siblings.includes(id)),
      "the overtaken criterion does not stand at its place among the comment's children"
    ).toEqual(siblings.filter((id) => drawn.includes(id)));
    const overtaken = nodeRow(page, criteria.overtaken.row).first();
    await expect
      .poll(() => ownMarks(overtaken), {
        message: "the overtaken criterion does not carry Overtaken",
      })
      .toEqual([OVERTAKEN]);
    await expectOpenWith(
      page,
      criteria.overtaken.id,
      [statement],
      "the overtaken criterion is not drawn open with the statement under it"
    );
    await expect(
      rowGaps(rowOfRead(page, statement)),
      "the statement under the overtaken criterion draws no gap"
    ).toHaveCount(0);

    // Its own fold control folds it and opens it again, and the comment, folded, counts it and
    // the statement under it.
    await overtaken.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
    await expect(
      overtaken,
      "the overtaken criterion's fold control does not fold it"
    ).toHaveAttribute("aria-expanded", "false");
    expect(
      (await treeItemsDrawn(page)).some((item) => item.readId === statement),
      "the statement under the overtaken criterion is drawn while the criterion is folded"
    ).toBe(false);
    await overtaken.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
    await expectOpenWith(
      page,
      criteria.overtaken.id,
      [statement],
      "the overtaken criterion's fold control does not open it again with the statement under it"
    );
    expect(
      await foldedCount(rowOfRead(page, comment), "the comment"),
      "the folded comment does not count the overtaken criterion and its statement"
    ).toBe(heldUnder(answer, key, comment, { margin }));
  });
});
