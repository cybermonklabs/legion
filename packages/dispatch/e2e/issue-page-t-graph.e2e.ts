/** the rows read Graph against the tree read the server answers for an issue a person and an
 *  agent wrote through the server, and count the tree reads its load sends */

import type { Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { CONTROL_LABELS, PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { createComment, getIssue } from "./api";
import { seedSpineCases, seedWholeIssue } from "./issue-layout-seed";
import { SUMMARY_BLOCK_ID } from "./issue-layout-seed-text";
import { filterRow, gotoIssue, phase, presetToggle, presetToggles } from "./issue-page";
import {
  blockNode,
  DESKTOP,
  expectOpeningLanded,
  focusThePage,
  nodeRow,
  nodeRows,
  PHONE,
} from "./issue-page-g";
import { drawnRowReadings, parentIndex, type RowReading } from "./issue-page-t";
import {
  anchoredComments,
  answerRowIds,
  awaitIndexedCoverage,
  firstRowOutOfPlace,
  foldedRowIds,
  idSet,
  nodesNoRowDraws,
  type OpenedOutline,
  openOutline,
  ROW_MS,
  rowsNotWhole,
  servingOutOfPlace,
} from "./issue-page-t-rows";
import { convertedGate, outlineReads, owedUnless, treeReadGate } from "./issue-page-t-tree";
import { expect, test } from "./test";

/**
 * Row 6 of the rows that draw the new issue page from the tree read: Graph, the last control of
 * the view row, draws Graph's read - the tree read with no view - whole and unfolded, and folds
 * and zooms by the reader's keys alone (spec R10, R11). The head of `e2e/issue-page-t-rows.ts`
 * says what the rows read and what their gates are.
 */

/** How deep the thread row 6 writes goes: three levels past the six a row's indent grows
 *  through. */
const THREAD_DEPTH = 9;

/** How many levels a row's indent grows through (spec R11, root R16). */
const INDENT_LEVELS = 6;

/** The most characters a folded node's first line holds where its first sentence is longer
 *  (spec R11). */
const FIRST_LINE_LIMIT = 100;

/** What row 6 reads of the whole issue beyond `seedWholeIssue`'s. */
interface GraphSeed {
  /** A check that serves `statement`, by its block id. */
  readonly check: string;
  /** A comment thread of the seed, which holds its replies. */
  readonly commentThread: string;
  readonly key: string;
  /** The planner's message of the seed, which holds its replies. */
  readonly message: string;
  readonly specSlug: string;
  /** A statement, by its block id, that holds a check and a plan task. */
  readonly statement: string;
  /** The person's thread of `THREAD_DEPTH` comments, each answering the one before it. */
  readonly thread: readonly string[];
  readonly verificationSlug: string;
}

/** The whole issue, with a thread nine levels deep that a person writes through the route a
 *  person writes through (`reply_to`, which the server keeps as the comment each one answers). */
async function seedGraph(): Promise<GraphSeed> {
  const seeded = await seedWholeIssue();
  const thread: string[] = [];
  for (let level = 1; level <= THREAD_DEPTH; level += 1) {
    const above = thread.at(-1);
    const comment = await createComment(seeded.key, {
      body: `Level ${level} of the deep thread answers the one above it.`,
      ...(above === undefined ? {} : { reply_to: above }),
    });
    thread.push(comment.id);
  }
  const artifacts = (await getIssue(seeded.key)).artifacts ?? [];
  const slugOf = (id: string) => artifacts.find((artifact) => artifact.id === id)?.slug ?? "";
  return {
    check: seeded.checks.passed,
    commentThread: seeded.comments.threads[0] ?? "",
    key: seeded.key,
    message: seeded.messages.root,
    specSlug: slugOf(seeded.specArtifactId),
    statement: seeded.requirements[0] ?? "",
    thread,
    verificationSlug: slugOf(seeded.verificationArtifactId),
  };
}

/** Graph opened on issue `key` at the width the page has: one tree read with no view and none
 *  with `kind`, no node folded, the rows the answer's nodes but the memo's callout and each
 *  comment on a node's text, each under the parent the answer names and each check and plan task
 *  also under each statement it serves, and each row whole. */
async function expectGraphOpens(page: Page, key: string, where: string): Promise<OpenedOutline> {
  const opened = await openOutline(page, key, "graph", {
    loaded: (load) => {
      expect(
        {
          withKind: outlineReads(load, key, "conversation"),
          withoutKind: outlineReads(load, key, "graph"),
        },
        `${where}, the page sends one tree read with no view and none with kind`
      ).toEqual({ withKind: 0, withoutKind: 1 });
    },
  });
  expect(await foldedRowIds(page), `${where}, no node opens folded`).toEqual([]);
  const anchored = await anchoredComments(key);
  expect(
    idSet(opened.rows.flatMap((row) => (row.id === null ? [] : [row.id]))),
    `${where}, the drawn rows are the answer's nodes but the memo's callout and each comment on a node's text`
  ).toEqual(answerRowIds(opened, nodesNoRowDraws(opened.answer, anchored, SUMMARY_BLOCK_ID)));
  expect(
    firstRowOutOfPlace(`${where}, in Graph`, opened.rows, opened),
    `${where}, each row stands under the parent the answer names, in its order`
  ).toBeUndefined();
  expect(
    servingOutOfPlace(opened.rows, opened),
    `${where}, each check and plan task stands under each statement it serves`
  ).toEqual([]);
  expect(await rowsNotWhole(page), `${where}, each row's text is whole`).toEqual([]);
  return opened;
}

/** How a folded node's first line breaks the rule of spec R11, or undefined where it keeps it: its
 *  text to its first sentence end (a full stop, question mark or exclamation mark followed by a
 *  space or the end) where that falls within `FIRST_LINE_LIMIT` characters, else the text cut at
 *  a word within them. `stored` is the node's text, its whitespace as one space each. */
function firstLineOffence(stored: string, line: string): string | undefined {
  const drawn = line.replace(/\s*…$/u, "").trimEnd();
  const end = /[.?!](?=\s|$)/u.exec(stored);
  if (end !== null && end.index < FIRST_LINE_LIMIT) {
    const sentence = stored.slice(0, end.index + 1);
    return drawn === sentence ? undefined : `"${line}", not its first sentence "${sentence}"`;
  }
  const cut =
    drawn !== "" &&
    drawn.length <= FIRST_LINE_LIMIT &&
    stored.startsWith(drawn) &&
    /\s/u.test(stored[drawn.length] ?? " ");
  return cut
    ? undefined
    : `"${line}", which is not "${stored}" cut at a word within ${FIRST_LINE_LIMIT} characters`;
}

/** How many rows stand under the first row of node `id` in `rows`, at every level below it. */
function rowsUnder(rows: readonly RowReading[], id: string): number {
  const at = rows.findIndex((row) => row.id === id);
  const level = rows[at]?.level ?? 0;
  const after = rows.slice(at + 1);
  const end = after.findIndex((row) => (row.level ?? 0) <= level);
  return end < 0 ? after.length : end;
}

/** The node ids of the crumbs the zoom draws above the tree, from the issue down. */
function crumbs(page: Page): Promise<string[]> {
  return page
    .locator("[data-issue-page] [data-crumb]")
    .evaluateAll((all) => all.map((crumb) => crumb.getAttribute("data-crumb") ?? ""));
}

/** The node the tree is rooted at: its first row's, where every other row stands below it, and
 *  null where the tree draws rows beside its first, as the tree with no zoom does. */
function zoomRoot(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const rows = [...document.querySelectorAll<HTMLElement>("[data-issue-page] [data-node-row]")];
    const [first, ...rest] = rows.map((row) => ({
      id: row.dataset.nodeId ?? "",
      level: Number(row.getAttribute("aria-level") ?? "0"),
    }));
    if (first === undefined) return null;
    return rest.every((row) => row.level > first.level) ? first.id : null;
  });
}

/** Fails unless the tree is zoomed to node `id`, under the crumbs `above`. */
async function expectZoomedTo(
  page: Page,
  id: string,
  above: readonly string[],
  what: string
): Promise<void> {
  await expect
    .poll(() => zoomRoot(page), { message: `${what}: the tree is not zoomed to ${id}` })
    .toBe(id);
  await expect
    .poll(() => crumbs(page), { message: `${what}: the zoom draws its breadcrumb` })
    .toEqual([...above]);
}

// c-graph-view.
// SLOW(>=2s): two seeded issues, Graph loaded on each at two widths with each load waited out,
// three nodes folded and unfolded by three keys and a control, five zooms and Graph's addresses
// loaded.
test("Graph draws the tree read whole, and folds and zooms @phone", async ({ browser }) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedSpineCases, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    await awaitIndexedCoverage(seeded.key);
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize(size);
      await expectGraphOpens(page, seeded.key, `${seeded.key} at ${size.width} px`);
    }
  });

  await phase(browser, seedGraph, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    await awaitIndexedCoverage(seeded.key);

    // At 390 px: Graph whole and unfolded, and the thread nine levels deep drawn at every level,
    // its indent growing through the sixth level and no further.
    await page.setViewportSize(PHONE);
    const narrow = await expectGraphOpens(page, seeded.key, "at 390 px");
    const levels = seeded.thread.map(
      (id) => narrow.rows.find((row) => row.id === id)?.level ?? null
    );
    const top = levels[0] ?? 0;
    expect(levels, "at 390 px, the thread nine levels deep draws all nine").toEqual(
      seeded.thread.map((_, depth) => top + depth)
    );
    const lefts = await page.evaluate(
      (ids) =>
        ids.map(
          (id) =>
            document
              .querySelector(
                `[data-issue-page] [data-node-row][data-node-id="${CSS.escape(id)}"] [data-node-card]`
              )
              ?.getBoundingClientRect().left ?? Number.NaN
        ),
      [...seeded.thread]
    );
    const growth = lefts.slice(1).map((left, depth) => Math.round(left - (lefts[depth] ?? 0)));
    expect(
      growth.map((step, depth) => (top + depth + 1 > INDENT_LEVELS ? step === 0 : step > 0)),
      `at 390 px, the indent grows to the sixth level and stops past it: ${JSON.stringify(lefts)}`
    ).toEqual(growth.map(() => true));

    // At 1440 px: Graph is the last control of the row, and its control, its address and t g
    // each open it; it draws no version control.
    await page.setViewportSize(DESKTOP);
    await expectGraphOpens(page, seeded.key, "at 1440 px");
    const names = await presetToggles(page).evaluateAll((buttons) =>
      buttons.map((button) =>
        (button.querySelector("[data-preset-name]")?.textContent ?? "").trim()
      )
    );
    expect(names.at(-1), "Graph is the last control the row draws").toBe(PRESET_NAMES.graph);
    await expect(presetToggle(page, "graph"), "/graph does not open Graph").toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(
      filterRow(page).getByRole("button", { name: /version/iu }),
      "Graph draws a version control"
    ).toHaveCount(0);

    // Three nodes folded: one by its control, one by the left arrow and one by `o`. Each draws
    // its first line, read against the words its row drew whole before the fold (a paragraph
    // break read as one space), and the count of the nodes under it.
    const statement = blockNode(seeded.specSlug, seeded.statement);
    const folds = [
      { by: "its control", id: statement },
      { by: "the left arrow", id: seeded.commentThread },
      { by: "o", id: seeded.message },
    ] as const;
    const before = await drawnRowReadings(page, { bodies: false });
    for (const { by, id } of folds) {
      const row = nodeRow(page, id).first();
      const whole = (await row.locator("[data-node-words]").innerText())
        .replace(/\s+/gu, " ")
        .trim();
      if (by === "its control") {
        await row.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
      } else {
        await row.focus();
        await page.keyboard.press(by === "o" ? "o" : "ArrowLeft");
      }
      await expect(row, `${by} does not fold ${id}`).toHaveAttribute("aria-expanded", "false");
      await expect(row, `${id} folded by ${by} does not count the nodes under it`).toHaveAttribute(
        "data-folded-count",
        String(rowsUnder(before, id))
      );
      const line = (await row.locator("[data-node-first-line]").textContent()) ?? "";
      expect(
        firstLineOffence(whole, line.replace(/\s+/gu, " ").trim()),
        `${id} folded by ${by} draws its first line, to its first sentence end or 100 characters at a word`
      ).toBeUndefined();
    }
    // The right arrow and `o` unfold a node; folded again, `z` unfolds all three.
    const [first, second] = folds;
    await nodeRow(page, first.id).first().focus();
    await page.keyboard.press("ArrowRight");
    await expect(
      nodeRow(page, first.id).first(),
      "the right arrow does not unfold the node"
    ).toHaveAttribute("aria-expanded", "true");
    await nodeRow(page, second.id).first().focus();
    await page.keyboard.press("o");
    await expect(nodeRow(page, second.id).first(), "o does not unfold the node").toHaveAttribute(
      "aria-expanded",
      "true"
    );
    for (const { id } of [first, second]) {
      await nodeRow(page, id).first().focus();
      await page.keyboard.press("ArrowLeft");
      await expect(nodeRow(page, id).first()).toHaveAttribute("aria-expanded", "false");
    }
    await page.keyboard.press("z");
    await expect
      .poll(() => foldedRowIds(page), { message: "z does not unfold all three nodes" })
      .toEqual([]);

    // With the focus on a statement, `.` zooms into it under a breadcrumb of its ancestors from
    // the issue down, and `,` zooms out one level.
    await nodeRow(page, statement).first().focus();
    await page.keyboard.press(".");
    await expectZoomedTo(page, statement, [seeded.key], ". on the statement");
    await page.keyboard.press(",");
    await expect
      .poll(() => crumbs(page), { message: ", does not zoom out to the issue" })
      .toEqual([]);
    // Two levels deep, each crumb zooms out to its node, and the issue's to the issue.
    const check = blockNode(seeded.verificationSlug, seeded.check);
    const placed = await drawnRowReadings(page, { bodies: false });
    const underAt = placed.findIndex(
      (row, at) => row.id === check && placed[parentIndex(placed, at) ?? -1]?.id === statement
    );
    expect(underAt, `the check ${check} is drawn under no row of ${statement}`).toBeGreaterThan(-1);
    await nodeRows(page).nth(underAt).focus();
    await page.keyboard.press(".");
    await expectZoomedTo(
      page,
      check,
      [seeded.key, statement],
      ". on the check under the statement"
    );
    await page.locator(`[data-issue-page] [data-crumb="${statement}"]`).click();
    await expectZoomedTo(page, statement, [seeded.key], "the statement's crumb");
    await page.locator(`[data-issue-page] [data-crumb="${seeded.key}"]`).click();
    await expect
      .poll(() => crumbs(page), { message: "the issue's crumb does not zoom out to the issue" })
      .toEqual([]);
    // `t g` opens Graph, and pressing Graph again returns to Conversation.
    await gotoIssue(page, presetPath(seeded.key, "conversation"));
    await expectOpeningLanded(page);
    await presetToggle(page, "graph").click();
    await expect(page, "Graph's control does not open Graph").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "graph")}$`)
    );
    await presetToggle(page, "graph").click();
    await expect(page, "pressing Graph again does not return to Conversation").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "conversation")}$`)
    );
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("g");
    await expect(page, "t g does not open Graph").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "graph")}$`)
    );
    // `/graph/<the statement's id>` opens Graph zoomed to it, under the same breadcrumb.
    await gotoIssue(page, buildIssuePath({ key: seeded.key, kind: "graph", node: statement }));
    await expectZoomedTo(page, statement, [seeded.key], "/graph/<the statement's id>");
  });
});
