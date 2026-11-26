/** the rows read the pages of an outline of an issue of 230 comments a person wrote through the
 *  server, as the tree read answers them 200 at a time, and count each page the page reads */

import type { Locator, Page } from "@playwright/test";

import type { IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { createComment, createIssue, createProject } from "./api";
import { PROJECT } from "./issue-layout-seed-support";
import { contentsPane, gotoIssue, phase } from "./issue-page";
import { focusedNodeId } from "./issue-page-i";
import { drawnRowReadings, parentIndex, type RowReading } from "./issue-page-t";
import {
  drawEveryPage,
  foldedRowIds,
  openOutline,
  ROW_MS,
  rowsNotWhole,
} from "./issue-page-t-rows";
import {
  type OutlineKind,
  olderCommentReads,
  owedUnless,
  TREE_PATH,
  treeReadGate,
} from "./issue-page-t-tree";
import { expect, test } from "./test";
import { expectLoadSettled } from "./traffic";

/**
 * Row 10 of the rows that draw the new issue page from the tree read: an outline draws a node's
 * first 50 children and a control for the rest, and reads the older comments its read holds back
 * only when the reader asks for them (spec R15). The head of `e2e/issue-page-t-rows.ts` says what
 * the rows read and what their gates are.
 */

/** How many posts the seed writes first, before every other comment: the ones past the outline
 *  read's page of 200 newest comments. */
const OLDEST_POSTS = 30;

/** How many posts the seed writes after those. */
const NEWER_POSTS = 143;

/** How many replies the first of the newer posts holds, written after every post. */
const REPLIES = 57;

/** How many comments one page of an outline read holds (spec R15). */
const COMMENT_PAGE = 200;

/** How many of a node's children an outline draws before its control for the rest (spec R15). */
const CHILD_PAGE = 50;

/** Which reply `?comment=` names: the 55th, past the first page of 50. */
const NAMED_REPLY = 55;

/** What `seedComments` wrote. */
interface CommentsSeed {
  readonly key: string;
  /** The 30 posts written first, oldest first. */
  readonly oldest: readonly string[];
  /** The post that holds the replies. */
  readonly post: string;
  /** Its 57 replies, oldest first. */
  readonly replies: readonly string[];
}

/** An issue of 230 comments, each written by a person through the route a person writes through:
 *  173 posts, the 30 oldest first, then the rest, and then 57 replies to the first of the rest,
 *  so the 200 newest comments are every post but the 30 oldest, and the replies. */
async function seedComments(): Promise<CommentsSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const { key } = await createIssue({
    force: true,
    project: PROJECT,
    title: "An issue of two hundred and thirty comments",
  });
  const post = async (body: string, replyTo?: string) =>
    (await createComment(key, { body, ...(replyTo === undefined ? {} : { reply_to: replyTo }) }))
      .id;
  const oldest: string[] = [];
  for (let index = 1; index <= OLDEST_POSTS; index += 1) {
    oldest.push(await post(`Old post ${index} is one of the thirty oldest.`));
  }
  const newer: string[] = [];
  for (let index = 1; index <= NEWER_POSTS; index += 1) {
    newer.push(await post(`Post ${index} is one of the newer posts.`));
  }
  const held = newer[0] ?? "";
  const replies: string[] = [];
  for (let index = 1; index <= REPLIES; index += 1) {
    replies.push(await post(`Reply ${index} answers the post that holds every reply.`, held));
  }
  return { key, oldest, post: held, replies };
}

/** The rows that stand directly under the first row of node `id`, by their node ids. */
function rowsDirectlyUnder(rows: readonly RowReading[], id: string): string[] {
  const at = rows.findIndex((row) => row.id === id);
  return rows.flatMap((row, index) =>
    at >= 0 && parentIndex(rows, index) === at && row.id !== null ? [row.id] : []
  );
}

/** The control after the post's first 50 replies, which says how many are left; the issue's own
 *  posts, past their first 50, are paged by a control of their own, which says another count. */
function repliesControl(page: Page): Locator {
  return page
    .locator("[data-issue-page] [data-tree-page]")
    .filter({ hasText: `${REPLIES - CHILD_PAGE} ${CONTROL_LABELS.page_rows}` });
}

/** One outline's first load, read: the post draws its first 50 replies, and no older page was
 *  read; its answer holds the page of 200 newest comments and the position of the oldest; the 30
 *  oldest posts are no rows; the replies are whole and unfolded, with a control that says 7 are
 *  left, whose press draws the rest. */
async function expectFirstPage(
  page: Page,
  seeded: CommentsSeed,
  outline: OutlineKind
): Promise<void> {
  const where = `in ${outline}`;
  const opened = await openOutline(page, seeded.key, outline);
  expect(
    rowsDirectlyUnder(opened.rows, seeded.post),
    `${where}, a node draws its first 50 children`
  ).toEqual(seeded.replies.slice(0, CHILD_PAGE));
  await expectLoadSettled(opened.traffic);
  expect(
    olderCommentReads(opened.traffic.requests, seeded.key),
    `${where}, no older comment page is read before the control is pressed`
  ).toBe(0);
  expect(
    {
      before: typeof opened.answer.comments_before,
      comments: (opened.answer.nodes ?? []).filter((node) => node.kind === "comment").length,
    },
    `${where}, the first load's outline read answers 200 comments and a comments_before`
  ).toEqual({ before: "string", comments: COMMENT_PAGE });
  const drawn = new Set(opened.rows.map((row) => row.id));
  expect(
    seeded.oldest.filter((id) => drawn.has(id)),
    `${where}, the 30 oldest posts are rows before the control is pressed`
  ).toEqual([]);
  expect(await foldedRowIds(page), `${where}, a reply opens folded`).toEqual([]);
  expect(await rowsNotWhole(page), `${where}, a reply is not drawn whole`).toEqual([]);
  await expect(repliesControl(page), `${where}, no control says 7 replies are left`).toHaveCount(1);
  await repliesControl(page).locator("button").click();
  await expect
    .poll(
      async () => rowsDirectlyUnder(await drawnRowReadings(page, { bodies: false }), seeded.post),
      { message: `${where}, pressing the control does not draw the rest of the replies` }
    )
    .toEqual(seeded.replies);
}

// c-pages.
// SLOW(>=2s): 230 comments seeded one by one, Conversation and Graph each loaded and their pages
// of replies drawn, an address of the 55th reply loaded, and the older comments read with each
// read waited out.
test("an outline draws 50 children at a time and reads older comments when asked", async ({
  browser,
}) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedComments, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    await expectFirstPage(page, seeded, "graph");
    await expectFirstPage(page, seeded, "conversation");

    // The address of the 55th reply opens the outline with every reply up to it drawn and the
    // focus on it.
    const named = seeded.replies[NAMED_REPLY - 1] ?? "";
    await gotoIssue(page, `${presetPath(seeded.key, "conversation")}?comment=${named}`);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "?comment= of the 55th reply does not put the focus on it",
      })
      .toBe(named);
    expect(
      rowsDirectlyUnder(await drawnRowReadings(page, { bodies: false }), seeded.post).slice(
        0,
        NAMED_REPLY
      ),
      "?comment= of the 55th reply does not draw every reply up to it"
    ).toEqual(seeded.replies.slice(0, NAMED_REPLY));

    // The control at the top of the issue's children reads the older comments, once, and draws
    // them in the read's order; the Contents list's foot control holds no second read of them.
    const again = await openOutline(page, seeded.key, "conversation");
    const older = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return TREE_PATH.test(url.pathname) && url.searchParams.has("comments_before");
    });
    const pressed = again.traffic.requests.length;
    await page.locator("[data-issue-page] [data-older-comments] button").click();
    const olderPage = (await (await older).json()) as IssueTreeRead;
    await expectLoadSettled(again.traffic);
    expect(
      olderCommentReads(again.traffic.requests.slice(pressed), seeded.key),
      "the press at the top of the issue's children sends one older comment read"
    ).toBe(1);
    const oldest = new Set(seeded.oldest);
    const readOrder = (olderPage.nodes ?? []).flatMap((node) =>
      oldest.has(node.id) ? [node.id] : []
    );
    expect(readOrder.length, "the older page holds other than the 30 oldest posts").toBe(
      OLDEST_POSTS
    );
    // The issue's posts are paged 50 at a time as any node's children are, so every page is
    // drawn before the old posts' rows are read.
    const oldestDrawn = async () => {
      await drawEveryPage(page);
      return (await drawnRowReadings(page, { bodies: false })).flatMap((row) =>
        row.id !== null && oldest.has(row.id) ? [row.id] : []
      );
    };
    await expect
      .poll(oldestDrawn, { message: "the 30 oldest posts are not rows, in the read's order" })
      .toEqual(readOrder);
    await expect(
      contentsPane(page).locator("[data-contents-older]"),
      "the Contents list's foot control offers a second read of the page the outline read"
    ).toHaveCount(0);
    await expectLoadSettled(again.traffic);
    expect(
      olderCommentReads(again.traffic.requests.slice(pressed), seeded.key),
      "the Contents list sends a second older comment read"
    ).toBe(1);

    // The Contents list's foot control reads the same page: on a fresh load of Graph, its press
    // sends one older comment read, the 30 posts are rows, and the outline's own control is gone
    // with no second read.
    const graph = await openOutline(page, seeded.key, "graph");
    const footPressed = graph.traffic.requests.length;
    await contentsPane(page).locator("[data-contents-older]").click();
    await expectLoadSettled(graph.traffic);
    expect(
      olderCommentReads(graph.traffic.requests.slice(footPressed), seeded.key),
      "the Contents list's foot control sends one older comment read"
    ).toBe(1);
    await expect
      .poll(oldestDrawn, {
        message: "the Contents list's foot control does not draw the 30 oldest posts as rows",
      })
      .toEqual(readOrder);
    await expect(
      page.locator("[data-issue-page] [data-older-comments]"),
      "the outline's control for older comments is still drawn after the foot control read them"
    ).toHaveCount(0);
    await expectLoadSettled(graph.traffic);
    expect(
      olderCommentReads(graph.traffic.requests.slice(footPressed), seeded.key),
      "the foot control's page is read a second time"
    ).toBe(1);
  });
});
