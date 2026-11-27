/** the row loads the Decisions view of an issue whose decisions and one of their statements stand
 *  past the first page of Conversation's read, and reads every decision row's box while that page
 *  is not read */

import type { Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  baseUrl,
  createComment,
  createIssue,
  createProject,
  editArtifact,
  patchIssue,
  userHeaders,
} from "./api";
import { asPlanner, PROJECT, pause } from "./issue-layout-seed-support";
import { requirementBlockId, specBlockRef } from "./issue-layout-seed-text";
import { gotoIssue, phase } from "./issue-page";
import { DESKTOP, PHONE } from "./issue-page-g";
import { ROW_MS } from "./issue-page-t-rows";
import { inBursts } from "./issue-page-t-scale";
import { owedUnless, treeReadGate } from "./issue-page-t-tree";
import { expect, test } from "./test";

/**
 * Decisions 579 and 583 (2), amended, on part U's path ("586, amended again"): the Decisions view
 * draws each decision from the decisions view read, which the server sends whole, and names a
 * statement the tree does not hold from its coverage row, so a decision whose statement stands on
 * a page of Conversation's read the page has not read is drawn in the review list from its first
 * drawing, unmarked, at its final height: one "informs" line for each statement it is tied to,
 * each named. No page has to land for that, so the page asks for no next page of Conversation for
 * the decisions, and no row moves while that page is not read.
 *
 * The seed: a spec of the statement `r1`, then `FILLERS` long statements, then the statement
 * `r2`; two agent's decisions waiting for the reader's review, one tied to `r2`, and one tied to
 * both `r1` and `r2`; then `COMMENTS` comments. Conversation's read walks the comments first and
 * then the spec's blocks in order, and a page holds at most 2,000,000 bytes of JSON
 * (`issueTreePageBytes`): the comments and `r1` fit on the first page, and the long statements
 * push `r2` and the decisions past its `cursor`. The row checks that shape on the page's own first
 * read before it trusts it.
 */

/** How many comments, and how long each: fewer comments than one comment page (200), so no older
 *  comment page is left once the next page lands and no decision stays marked. */
const COMMENTS = 60;
const COMMENT_CHARS = 2_500;

/** How many long statements stand between `r1` and `r2`, and how long each is: one paragraph,
 *  inside the 32,000 characters a block holds. With the comments they put more than a page's
 *  bytes before `r2` (each weighs about 61 KB in the read, as task 77 measured one), and the spec
 *  stays under the 1 MiB a document and a request hold. */
const FILLERS = 28;
const FILLER_CHARS = 30_000;

/** The two statements the decisions are tied to: `r1` on the first page, `r2` past it. */
const STATEMENTS = [1, 2] as const;

/** The two decisions, each the planner's, tied to `statements` and waiting for review. */
const DECISIONS = [
  {
    heading: "Keep a placed row at its height",
    id: "d-placed-one",
    statements: [2],
    words:
      "A decision whose statement is on a page not read yet keeps its height as that page lands.",
  },
  {
    heading: "Draw a line for each statement from the first drawing",
    id: "d-placed-two",
    statements: [1, 2],
    words: "Each decision row draws one informs line for each statement, before its names come.",
  },
] as const;

const WORDS = [
  "the",
  "placed",
  "row",
  "keeps",
  "its",
  "place",
  "while",
  "statement's",
  "page",
  "is",
  "out",
  "and",
  "the",
  "view",
  "draws",
  "every",
  "line",
  "once",
] as const;

/** One paragraph of at least `chars` characters, opening with `label`, the `n`th of its kind. */
function paragraph(label: string, n: number, chars: number): string {
  const words: string[] = [label];
  let length = label.length;
  for (let at = 0; length < chars; at += 1) {
    const word = WORDS[(at * 7 + n) % WORDS.length] ?? "word";
    words.push(at % 11 === 10 ? `${word}.` : word);
    length += word.length + 1;
  }
  return words.join(" ");
}

/** Writes the issue the head of this file describes, and waits until its coverage read holds the
 *  two statements, which the view places the decisions by before their page is read. */
async function seedPlacedRows(): Promise<{ key: string }> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const statement = (n: number) =>
    `:::requirement{#${requirementBlockId(n)}}\nStatement ${n}: the view keeps every row where it first drew it.\n:::`;
  const fillers = Array.from(
    { length: FILLERS },
    (_, at) =>
      `:::requirement{#f${at + 1}}\n${paragraph(`Statement f${at + 1}:`, at, FILLER_CHARS)}\n:::`
  );
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: `${[statement(1), ...fillers, statement(2)].join("\n\n")}\n`,
    title: "Agents' decisions wait for review past the first page of the conversation",
  });
  const key = issue.key;
  await patchIssue(key, { assignee: "alice" });
  const decisions = DECISIONS.map(
    (decision) =>
      `:::decision{#${decision.id} shaped="${decision.statements
        .map((n) => specBlockRef(key, requirementBlockId(n)))
        .join(" ")}"}\n### ${decision.heading}\n\n${decision.words}\n:::`
  ).join("\n\n");
  await editArtifact(
    issue.primary_artifact_id ?? "",
    { ops: [{ after: "end", markdown: `## Decisions\n\n${decisions}\n`, op: "insert" }] },
    asPlanner
  );
  await inBursts(
    Array.from({ length: COMMENTS }, (_, at) => at + 1),
    (n) =>
      createComment(
        key,
        { body: paragraph(`Comment ${n}:`, n, COMMENT_CHARS) },
        { login: n % 2 === 0 ? "alice" : "bob" }
      )
  );
  const cookie = await userHeaders("alice");
  const wanted = STATEMENTS.map((n) => requirementBlockId(n));
  const started = Date.now();
  for (;;) {
    const answer = (await (
      await fetch(new URL(`/api/v1/issues/${key}/coverage`, baseUrl), { headers: cookie })
    ).json()) as { requirements?: { block_id?: string }[] };
    const held = new Set((answer.requirements ?? []).map((one) => one.block_id));
    if (wanted.every((id) => held.has(id))) return { key };
    if (Date.now() - started > 120_000) {
      throw new Error(`the coverage read never held ${wanted.join(" and ")}`);
    }
    await pause(500);
  }
}

/** Each row of the Decisions view: its node, its height, the top of its box on the page, and how
 *  many informs lines it draws. */
async function rowBoxes(
  page: Page
): Promise<{ height: number; id: string; lines: number; top: number }[]> {
  return await page.locator("[data-decisions-body] [data-node-row]").evaluateAll((rows) =>
    rows.map((row) => {
      const box = row.getBoundingClientRect();
      return {
        height: Math.round(box.height * 10) / 10,
        id: row.getAttribute("data-node-id") ?? "",
        lines: row.querySelectorAll("[data-decision-line]").length,
        top: Math.round((box.top + window.scrollY) * 10) / 10,
      };
    })
  );
}

/** Whether `url` is the next page of Conversation's read: a tree read of the conversation's kinds
 *  that names a cursor. */
function conversationCursor(url: URL): boolean {
  return (
    url.pathname.endsWith("/tree") &&
    url.searchParams.has("kind") &&
    !url.searchParams.has("view") &&
    url.searchParams.has("cursor")
  );
}

// c-placed-rows. `@phone` because the claim is a box's height, which the phone's width reflows:
// the row reads it at 1440 px and at 390 px.
// SLOW(>=2s): an issue of a long spec and 60 long comments seeded and its block index waited out,
// then the Decisions view loaded at two widths, each read over a quiet second.
test("a decision placed by its statement's coverage row is drawn at its final height, and no row moves, while the statement's page is not read @phone", async ({
  browser,
}) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedPlacedRows, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await page.setViewportSize(size);
      // Each next page of Conversation the page asks for while the Decisions view is drawn.
      const asked: string[] = [];
      const watchCursor = (request: { url(): string }) => {
        if (conversationCursor(new URL(request.url()))) asked.push(request.url());
      };
      page.on("request", watchCursor);
      // The page's own first read of Conversation, whose shape the row checks.
      const first = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          url.pathname.endsWith("/tree") &&
          url.searchParams.has("kind") &&
          !url.searchParams.has("view") &&
          !url.searchParams.has("cursor") &&
          !url.searchParams.has("comments_before")
        );
      });
      await gotoIssue(page, presetPath(seeded.key, "decisions"));
      const shape = (await (await first).json()) as {
        cursor?: string | null;
        nodes?: { block_id?: string }[];
      };
      expect(
        typeof shape.cursor === "string" && shape.cursor !== "",
        `${where}, Conversation's first page names no next page: the seed's shape does not hold`
      ).toBe(true);
      const onFirst = (shape.nodes ?? []).flatMap((node) =>
        [...DECISIONS.map((one) => one.id), "r1", "r2"].includes(node.block_id ?? "")
          ? [node.block_id]
          : []
      );
      expect(
        onFirst,
        `${where}, Conversation's first page should hold r1 and neither r2 nor a decision`
      ).toEqual(["r1"]);
      const review = page.locator(
        '[data-decisions-body] [data-decisions-part="review"] [data-node-row][data-node-type="decision"]'
      );
      await expect(review, `${where}, the review list does not draw both decisions`).toHaveCount(2);
      // Each line is named from its statement's own words, at the first drawing: r1 by its node
      // on Conversation's first page, r2 by its coverage row ("586 (F), corrected").
      await expect(
        page.locator("[data-decisions-body] [data-decision-statement] a"),
        `${where}, the informs lines are not each named from their statement's own words`
      ).toHaveCount(3);
      await expect(
        page.locator("[data-decisions-body] [data-change-unread]"),
        `${where}, a decision is marked, though the decisions view read holds it whole`
      ).toHaveCount(0);
      // Every read has answered, the names among them, and the view is still: a quiet second.
      await pause(1_000);
      const before = await rowBoxes(page);
      await pause(1_000);
      const after = await rowBoxes(page);
      page.off("request", watchCursor);
      expect(
        asked,
        `${where}, the page asked for a next page of Conversation for the Decisions view`
      ).toEqual([]);
      expect(
        after,
        `${where}, a row's height, its informs lines or the top of a row below changed over a quiet second`
      ).toEqual(before);
      // One informs line for each statement a decision is tied to, from the first drawing.
      expect(
        Object.fromEntries(before.map(({ id, lines }) => [id.slice(id.indexOf("#b-") + 3), lines])),
        `${where}, a decision does not draw one informs line for each statement it is tied to`
      ).toEqual(
        Object.fromEntries(DECISIONS.map((decision) => [decision.id, decision.statements.length]))
      );
    }
  });
});
