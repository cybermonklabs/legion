/** the rows read what each row of an outline draws on its face from the tree read the server
 *  answers, the coverage, results and asks reads it keeps beside it, and a comment another
 *  person writes through the server */

import type { Page } from "@playwright/test";

import type { IssueTreeRead } from "../web/src/api/types";
import {
  coverageGapKind,
  GAP_LINES,
  type GapKind,
  nodeFactLabel,
} from "../web/src/features/issue-page/page-words";
import { treeIdOf } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { createComment, editArtifact, editComment, getIssueCoverage } from "./api";
import { seedReviewList, seedWholeIssue } from "./issue-layout-seed";
import { asPlanner } from "./issue-layout-seed-support";
import { requirementBlockId } from "./issue-layout-seed-text";
import { phase } from "./issue-page";
import {
  blockNode,
  DESKTOP,
  expectOpeningLanded,
  focusThePage,
  nodeRow,
  nodeRows,
  PHONE,
  rowAct,
} from "./issue-page-g";
import {
  drawnRowFaces,
  drawnRowReadings,
  parentIndex,
  type RowReading,
  type SeededIssue,
} from "./issue-page-t";
import {
  artifactSlugs,
  awaitIndexedCoverage,
  decisionsThatWait,
  firstLoad,
  nodesById,
  openOutline,
  parentOfWords,
  ROW_MS,
  rowsNotWhole,
  servingCountsLine,
} from "./issue-page-t-rows";
import {
  convertedGate,
  dashboardPartGate,
  type OutlineKind,
  outlineReads,
  owedUnless,
  treeReadGate,
} from "./issue-page-t-tree";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Rows 12, 13 and 14 of the rows that draw the new issue page from the tree read: a link the
 * reader is on keeps the focus through a re-read (spec R18), the outline's asks and decisions as
 * Dashboard's part draws them (spec R17), and each row's face, drawn whole with what its focused
 * body drew, whatever the focus (spec R11, R18). The head of `e2e/issue-page-t-rows.ts` says
 * what the rows read and what their gates are.
 */

// ---------------------------------------------------------------------------------------------
// Row 12: a focused link keeps the focus through a re-read.
// ---------------------------------------------------------------------------------------------

/** Where the link of row 12's post goes: a page outside Dispatch, so the link is the post's own
 *  and no reference of the store. */
const LINK_HREF = "https://example.com/outline-notes";

/** Row 12's post, whose words hold that link. */
const LINKED_POST = `The measurements behind the outline are in [the notes](${LINK_HREF}), read once.`;

/** The words the post's author rewrites it to, the link kept. */
const EDITED_WORDS = "read twice";

/** The post as its author rewrites it, while alice's focus is on the link. */
const EDITED_POST = `The measurements behind the outline are in [the notes](${LINK_HREF}), ${EDITED_WORDS}.`;

/** The words of the comment bob writes while alice's focus is on the link. */
const BOBS_WORDS = "Bob's comment reaches the page while the link keeps the focus.";

/** Whether the focus is on the link to `href` inside the row of node `id`. */
function onLink(page: Page, id: string, href: string): Promise<boolean> {
  return page.evaluate(
    ({ href, id }) =>
      document.activeElement?.matches(`a[href="${CSS.escape(href)}"]`) === true &&
      document.activeElement.closest(`[data-node-row][data-node-id="${CSS.escape(id)}"]`) !== null,
    { href, id }
  );
}

// c-row-link.
// SLOW(>=2s): the whole seeded issue, its load waited out, and another person's comment waited
// for until it is drawn and the re-read it starts has settled.
test("a focused link in a row keeps the focus through a re-read", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const post = await createComment(seeded.key, { body: LINKED_POST });
    const traffic = watchApiRequests(page);
    await firstLoad(page, traffic, seeded.key);
    await expectOpeningLanded(page);
    const row = nodeRow(page, post.id).first();
    const link = row.locator(`[data-node-words] a[href="${LINK_HREF}"]`);
    await expect(link, "the post's words draw no link").toHaveCount(1);
    await link.focus();
    expect(await onLink(page, post.id, LINK_HREF), "the link did not take the focus").toBe(true);

    const before = traffic.requests.length;
    await createComment(seeded.key, { body: BOBS_WORDS }, { login: "bob" });
    await expect
      .poll(
        async () =>
          parentOfWords(await drawnRowReadings(page, { bodies: false }), BOBS_WORDS) !== undefined,
        { message: "bob's comment is never drawn as a row with no reload" }
      )
      .toBe(true);
    await expectLoadSettled(traffic);
    expect(
      outlineReads(traffic.requests.slice(before), seeded.key, "conversation"),
      "bob's comment re-reads the tree"
    ).toBeGreaterThanOrEqual(1);
    expect(await onLink(page, post.id, LINK_HREF), "the focus stays on the link").toBe(true);
    await expect(row.locator("[data-row-acts]"), "the row's acts stay drawn").toBeVisible();
    await expect(rowAct(row, "reply"), "the row's Reply stays drawn").toHaveCount(1);

    // Its author rewrites the post, keeping the link: the re-read paints other words, and the
    // focus goes to the new link of the same address.
    await editComment(post.id, { body: EDITED_POST });
    await expect(
      row.locator("[data-node-words]"),
      "the post's rewritten words are never drawn with no reload"
    ).toContainText(EDITED_WORDS);
    await expectLoadSettled(traffic);
    expect(await onLink(page, post.id, LINK_HREF), "the focus stays on the link").toBe(true);
    await expect(row.locator("[data-row-acts]"), "the row's acts stay drawn").toBeVisible();
  });
});

// ---------------------------------------------------------------------------------------------
// Row 13: the asks and decisions of the outline, as Dashboard's part draws them.
// ---------------------------------------------------------------------------------------------

// What row 13 can only read once Dashboard's part draws the ask body and the decision card: each
// is a clause of c-ask-body that reads marks only that part draws, so it can be built only once
// those marks are on this branch's base. Each is false here, and each fails the row past its gate
// until it is built.
/** Each ask's row is the ask body, with its options in their radio or checkbox roles, the answer
 *  box in place and "Answer". */
const ASK_BODY_BUILT = false;
/** Each agent decision is the decision card, with its author line and its level word. */
const DECISION_CARD_BUILT = false;

/** Fails unless the first row of decision `id` draws Keep and Change, once each; `what` names the
 *  decision in each failure's words. */
async function expectKeepAndChange(page: Page, id: string, what: string): Promise<void> {
  const row = nodeRow(page, id).first();
  const keep = row.locator('button[data-row-action="keep"]');
  const change = row.locator('button[data-row-action="change"]');
  await expect(keep, `${what} draws Keep`).toHaveCount(1);
  await expect(change, `${what} draws Change`).toHaveCount(1);
}

/** Each of `rows` that stands under no row of `parent`, the ask the answer hangs it under, by
 *  the ids `idOf` gives the answer's nodes: an ask's replies are rows under it. */
function repliesOutOfPlace(
  rows: readonly RowReading[],
  answer: IssueTreeRead,
  idOf: (id: string) => string
): string[] {
  const nodes = nodesById(answer);
  return (answer.nodes ?? []).flatMap((node) => {
    const ask = node.parent === null ? undefined : nodes.get(node.parent.id);
    if (node.supertag !== "comment" || ask?.supertag !== "ask") return [];
    const [reply, home] = [idOf(node.id), idOf(ask.id)];
    const at = rows.findIndex((row) => row.id === reply);
    const under = at < 0 ? undefined : rows[parentIndex(rows, at) ?? -1]?.id;
    return under === home
      ? []
      : [`the reply ${reply} stands under ${under ?? "no row"}, not ${home}`];
  });
}

// c-ask-body, owed on Dashboard's part (`dashboardPartGate`), which brings the ask body and the
// decision card. Past the gate the row reads what the page draws without that part's marks, in
// Conversation and in Graph, and fails on each clause that needs those marks until it is built.
// SLOW(>=2s): two seeded issues, each loaded in two outlines with each load waited out.
test("the outline draws each ask with the ask body and each decision with the card", async ({
  browser,
}) => {
  owedUnless(dashboardPartGate);
  test.setTimeout(ROW_MS);
  const seeds: readonly (() => Promise<SeededIssue>)[] = [seedWholeIssue, seedReviewList];
  for (const seed of seeds) {
    await phase(browser, seed, async (page, seeded) => {
      owedUnless(await treeReadGate(page, seeded.key));
      for (const outline of ["conversation", "graph"] as const satisfies readonly OutlineKind[]) {
        const where = `${seeded.key} in ${outline}`;
        const opened = await openOutline(page, seeded.key, outline);
        await expect(
          nodeRows(page).locator("[data-node-options], [data-node-answer]"),
          `${where}, no row draws NodeRow.tsx's options or AskAnswer`
        ).toHaveCount(0);
        expect(
          repliesOutOfPlace(opened.rows, opened.answer, treeIdOf(opened.answer, opened.reads)),
          `${where}, each ask's replies are rows under it`
        ).toEqual([]);
        const waits = decisionsThatWait(opened);
        const waiting = opened.rows.flatMap((row) =>
          row.id !== null && waits.has(row.id) ? [row.id] : []
        );
        for (const id of waiting) {
          await expectKeepAndChange(page, id, `${where}, the waiting decision ${id}`);
        }
        const asks = opened.rows.flatMap((row) =>
          row.kind.type === "ask" && row.id !== null ? [row.id] : []
        );
        for (const id of asks) {
          await expect(
            nodeRow(page, id).first().locator("[data-row-acts]"),
            `${where}, the ask ${id} keeps its row's acts`
          ).toHaveCount(1);
        }
      }
    });
  }
  expect
    .soft(ASK_BODY_BUILT, "each ask is the ask body: not built: needs the Dashboard part's marks")
    .toBe(true);
  expect
    .soft(
      DECISION_CARD_BUILT,
      "each agent decision is the decision card: not built: needs the Dashboard part's marks"
    )
    .toBe(true);
});

// ---------------------------------------------------------------------------------------------
// Row 14: every row draws on its face what its body drew.
// ---------------------------------------------------------------------------------------------

/** The block id of the planner's statement of twenty lines. */
const LONG_STATEMENT = "r-twenty-lines";

/** How many lines the planner's long statement holds, each a paragraph of its own. */
const LONG_STATEMENT_LINES = 20;

/** The height of one line of a row's words (`NodeRow.tsx`'s `leading-6`). */
const WORDS_LINE_PX = 24;

/** The kinds of fact a check's face draws of its own attributes (`NodeFacts`) that every check
 *  the seed writes carries (`verificationDocument`): when it runs, its surface and its run. */
const CHECK_FACTS = ["when", "surface", "run"] as const;

/** A run of 64 hex digits, a sha256 digest as an image's address names it, which no break
 *  opportunity splits: only the row's break rule (`RowText.tsx`'s `overflow-wrap: anywhere`)
 *  keeps it inside its column at 390 px. The store address beside it breaks after each `/` and
 *  at its hyphens with no break rule at all. */
const UNBROKEN_RUN = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08";

/** What row 14 reads of the whole issue beyond what `seedWholeIssue` writes. */
interface FacesSeed {
  /** The first statement, by its block id, which a check serves and so holds nodes. */
  readonly branch: string;
  readonly key: string;
  /** The planner's statement of twenty lines, by its block id. */
  readonly longStatement: string;
  /** The post whose words hold a long store address and an unbroken run, by its id. */
  readonly longAddress: string;
  readonly specSlug: string;
}

/** The whole issue, with a statement of twenty lines the planner writes through the route an
 *  agent writes through, and alice's post whose words hold a long `dispatch://` store address,
 *  which the row's words draw as written: a store address names no node a reference chip names,
 *  and nothing links one that names no node of the tree (`refs/store-address.ts`); and, after
 *  it, an unbroken run (`UNBROKEN_RUN`). */
async function seedFaces(): Promise<FacesSeed> {
  const seeded = await seedWholeIssue();
  const lines = Array.from(
    { length: LONG_STATEMENT_LINES },
    (_, index) => `Line ${index + 1} of the planner's long statement keeps its words.`
  );
  await editArtifact(
    seeded.specArtifactId,
    {
      ops: [
        {
          after: `block:${requirementBlockId(12)}`,
          markdown: `:::requirement{#${LONG_STATEMENT}}\n${lines.join("\n\n")}\n:::`,
          op: "insert",
        },
      ],
    },
    asPlanner
  );
  const address = `dispatch://interaction/${crypto.randomUUID()}`;
  const post = await createComment(seeded.key, {
    body: `The planner keeps the thread's store at ${address} until the run ends, from image sha256:${UNBROKEN_RUN}.`,
  });
  const slug = (await artifactSlugs(seeded.key))(seeded.specArtifactId);
  return {
    branch: requirementBlockId(1),
    key: seeded.key,
    longAddress: post.id,
    longStatement: LONG_STATEMENT,
    specSlug: slug,
  };
}

/** What row 14 reads of one seed: the issue and its spec's slug, and the long statement and the
 *  long address where the seed wrote them. */
interface FacesOf {
  /** A statement of the spec, by its block id, that holds a check: the node `o` folds. */
  readonly branch?: string;
  readonly key: string;
  readonly longAddress?: string;
  readonly longStatement?: string;
  readonly specSlug: string;
}

/** Each row's whole text and its height, rounded to a pixel, in drawing order. */
function rowLooks(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLElement>("[data-issue-page] [data-node-row]"),
      (row) =>
        `${Math.round(row.getBoundingClientRect().height)} ${(row.textContent ?? "").replace(/\s+/g, " ").trim()}`
    )
  );
}

/** Each check row of `rows` whose face draws not every fact the seed gives each check
 *  (`CHECK_FACTS`), with what it lacks; and, where no check row draws its negative control,
 *  which every seeded check but one carries, that. */
function checksWithoutFacts(rows: readonly RowReading[]): string[] {
  const checks = rows.filter((row) => row.kind.type === "check");
  const lacking = checks.flatMap((row) => {
    const drawn = new Set(row.facts.map((fact) => fact.name));
    const missing = CHECK_FACTS.filter((fact) => !drawn.has(nodeFactLabel(fact)));
    return missing.length === 0 ? [] : [`${row.id} draws no ${missing.join(", ")}`];
  });
  const control = nodeFactLabel("negative_control");
  const controlled = checks.some((row) => row.facts.some((fact) => fact.name === control));
  return checks.length === 0 || controlled
    ? lacking
    : [...lacking, `no check draws its ${control}`];
}

/** The gap kind a statement draws where no check serves it. */
const NO_CHECK: GapKind = "no_check";

/** Row 14 on one seed, one outline and one width: each row's face as drawn with the focus outside
 *  the outline, the long statement whole, a walk of the focus down the outline that changes no
 *  row, `o` folding with no pin, and at 390 px the page no wider than the screen. */
async function expectFaces(
  page: Page,
  seeded: FacesOf,
  outline: OutlineKind,
  size: { readonly height: number; readonly width: number }
): Promise<void> {
  const where = `${seeded.key} in ${outline} at ${size.width} px`;
  await page.setViewportSize(size);
  const opened = await openOutline(page, seeded.key, outline);
  await expectOpeningLanded(page);
  await focusThePage(page);
  const rows = await drawnRowFaces(page);

  expect(checksWithoutFacts(rows), `${where}, a check row draws its facts with no focus`).toEqual(
    []
  );
  if (seeded.branch !== undefined) {
    expect(
      rows.filter((row) => row.kind.type === "check").length,
      `${where}, the whole issue draws no check row`
    ).toBeGreaterThan(0);
  }
  if (outline === "graph") {
    const verdict = nodeFactLabel("verdict");
    expect(
      rows.flatMap((row) =>
        row.kind.type === "result" && !row.facts.some((fact) => fact.name === verdict)
          ? [row.id]
          : []
      ),
      `${where}, each result row draws its facts with no focus`
    ).toEqual([]);
  }

  const coverage = await getIssueCoverage(seeded.key);
  const statements = (coverage.requirements ?? []).flatMap((requirement) => {
    const row = rows.find((one) => one.id === blockNode(seeded.specSlug, requirement.block_id));
    return row === undefined ? [] : [{ requirement, row }];
  });
  expect(statements.length, `${where}, no statement of the coverage read is drawn`).toBeGreaterThan(
    0
  );
  // The statements no check serves, as the page reads the coverage read's lines
  // (`coverageGapKind`): a proposed or an overtaken statement owes no work and draws no gap.
  const unchecked = statements.filter(
    ({ requirement }) =>
      requirement.state !== "proposed" &&
      requirement.state !== "overtaken" &&
      (requirement.gaps ?? []).some((line) => coverageGapKind(line) === NO_CHECK)
  );
  expect(
    unchecked.length,
    `${where}, the coverage read names no drawn statement that no check serves`
  ).toBeGreaterThan(0);
  expect(
    unchecked.flatMap(({ row }) =>
      row.gaps.some((gap) => gap.kind === NO_CHECK && gap.line === GAP_LINES[NO_CHECK])
        ? []
        : [row.id]
    ),
    `${where}, each requirement no check serves draws its gap in full`
  ).toEqual([]);
  expect(
    statements.flatMap(({ requirement, row }) =>
      row.counts.includes(servingCountsLine(requirement.checks, requirement.plan_tasks))
        ? []
        : [`${row.id} draws ${JSON.stringify(row.counts)}`]
    ),
    `${where}, each statement draws the counts the coverage read gives it`
  ).toEqual([]);
  // The decisions that wait on the reader's review, as the review list reads them, each drawn.
  const waits = decisionsThatWait(opened);
  const waiting = rows.flatMap((row) => (row.id !== null && waits.has(row.id) ? [row.id] : []));
  expect(
    [...waits].filter((id) => !rows.some((row) => row.id === id)),
    `${where}, every waiting decision has a row`
  ).toEqual([]);
  for (const id of waiting) {
    await expectKeepAndChange(page, id, `${where}, the decision ${id} that waits`);
  }

  if (seeded.longStatement !== undefined) {
    const words = nodeRow(page, blockNode(seeded.specSlug, seeded.longStatement))
      .first()
      .locator("[data-node-words]");
    await expect(words, `${where}, the long statement draws no row`).toHaveCount(1);
    expect(
      await words.evaluate((element) => element.getBoundingClientRect().height),
      `${where}, an agent's statement is drawn whole`
    ).toBeGreaterThanOrEqual(LONG_STATEMENT_LINES * WORDS_LINE_PX);
  }
  await expect(
    nodeRows(page).locator("[data-node-more]"),
    `${where}, no row draws "more · N words"`
  ).toHaveCount(0);
  expect(await rowsNotWhole(page), `${where}, each row's words are drawn whole`).toEqual([]);

  // The focus moves down the outline one row at a time, and no row changes its text or its
  // height. The walk stops at the last row, where the next key would report that it has nowhere
  // to go, which a row draws as words of its own. The rows were read twice alike as the outline
  // opened and nothing has been pressed since, so one reading counts the rows the walk takes.
  const looks = await rowLooks(page);
  await nodeRows(page).first().focus();
  for (let at = 1; at < looks.length; at += 1) {
    await page.keyboard.press("j");
    await expect(nodeRows(page).nth(at), `${where}, j did not move to row ${at}`).toBeFocused();
    expect(
      await rowLooks(page),
      `${where}, moving the focus down the outline to row ${at} changes no row's text and no row's height`
    ).toEqual(looks);
  }

  // `o` folds the focused node, and pins nothing: a node the seed wrote with nodes under it.
  if (seeded.branch !== undefined) {
    const branch = nodeRow(page, blockNode(seeded.specSlug, seeded.branch)).first();
    await branch.focus();
    await page.keyboard.press("o");
    await expect(branch, `${where}, o does not fold the focused node`).toHaveAttribute(
      "aria-expanded",
      "false"
    );
    await expect(
      nodeRows(page).locator("[data-pinned]"),
      `${where}, o pins the focused node`
    ).toHaveCount(0);
    await page.keyboard.press("o");
    await expect(branch, `${where}, o does not unfold the node again`).toHaveAttribute(
      "aria-expanded",
      "true"
    );
  }

  if (size.width === PHONE.width) {
    if (seeded.longAddress !== undefined) {
      const words = nodeRow(page, seeded.longAddress).first().locator("[data-node-words]");
      await expect(
        words,
        `${where}, the long address is not drawn whole in the row's words`
      ).toContainText(/dispatch:\/\/interaction\/[0-9a-f-]{36}/u);
      await expect(
        words.locator("a[data-row-chip]"),
        `${where}, the long address is drawn as a reference chip, not as the row's words`
      ).toHaveCount(0);
    }
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth),
      `${where}, the page is no wider than the screen: document.documentElement.scrollWidth is at most innerWidth`
    ).toBeLessThanOrEqual(0);
  }
}

// c-row-face.
// SLOW(>=2s): two seeded issues, each loaded in two outlines at two widths with each load waited
// out, and the focus walked down every row of each with every row read again after each move.
test("every row draws on its face what its body drew @phone", async ({ browser }) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedFaces, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    await awaitIndexedCoverage(seeded.key);
    for (const outline of ["conversation", "graph"] as const) {
      for (const size of [DESKTOP, PHONE]) await expectFaces(page, seeded, outline, size);
    }
  });
  await phase(browser, seedReviewList, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    await awaitIndexedCoverage(seeded.key);
    for (const outline of ["conversation", "graph"] as const) {
      for (const size of [DESKTOP, PHONE]) {
        await expectFaces(page, { key: seeded.key, specSlug: seeded.specSlug }, outline, size);
      }
    }
  });
});
