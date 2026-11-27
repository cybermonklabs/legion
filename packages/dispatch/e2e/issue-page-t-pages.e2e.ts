/** the rows read the pages of an outline of an issue of more than 200 comments a person wrote
 *  through the server, as the tree read answers them 200 at a time, and count each page the page
 *  reads */

import type { Locator, Page } from "@playwright/test";

import type { IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import {
  createComment,
  createIssue,
  createProject,
  editArtifact,
  getIssue,
  patchIssue,
} from "./api";
import { asPlanner, PROJECT } from "./issue-layout-seed-support";
import { requirementBlockId, specBlockRef, specDocument } from "./issue-layout-seed-text";
import { contentsPane, gotoIssue, phase } from "./issue-page";
import { focusedNodeId, openPreset } from "./issue-page-i";
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

declare global {
  interface Window {
    /** For each older comment page the page asked for since it loaded, whether it had drawn a
     *  row of the issue then (`recordOlderPages`). */
    __dispatchOlderPages?: boolean[];
  }
}

/**
 * Row 10 of the rows that draw the new issue page from the tree read: an outline draws a node's
 * first 50 children and a control for the rest, and reads the older comments its read holds back
 * only when the reader asks for them (spec R15), or, once the page has drawn, where a decision's
 * Change or an address's comment may stand on them (decision 563 (4)). The head of
 * `e2e/issue-page-t-rows.ts` says what the rows read and what their gates are.
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

/** The decision whose Change the seed writes before every other comment but one, and the other
 *  decision, of which no Change is written: block ids of the spec. */
const CHANGED_DECISION = "d-changed";
const WAITING_DECISION = "d-waiting";

/** The words of the decision a Change is written on, which the Change quotes. */
const CHANGED_WORDS = "The outline is drawn at one width on every screen.";

/** What `seedOlderChange` wrote. */
interface OlderChangeSeed {
  /** The person's Change of the decision `CHANGED_DECISION`, the second oldest comment. */
  readonly change: string;
  readonly key: string;
  /** The oldest comment, a post. */
  readonly oldest: string;
  /** The spec's slug, which a decision's row is named by. */
  readonly specSlug: string;
}

/** An issue whose spec holds two statements and two decisions an agent wrote, each tied to one of
 *  them and waiting for review, and 202 comments a person wrote after the decisions: first a post,
 *  then a Change of the first decision, quoting its words, then 200 posts. So the outline read's
 *  page of 200 newest comments holds neither the oldest post nor the Change, and names the older
 *  page that holds both; the issue is the reader's own, so a decision that waits for review waits
 *  on her. */
async function seedOlderChange(): Promise<OlderChangeSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specDocument({ openQuestion: false, requirements: 2, summary: false }),
    title: "A decision whose Change is older than the 200 newest comments",
  });
  const spec = issue.primary_artifact_id ?? "";
  await patchIssue(issue.key, { assignee: "alice" });
  const decision = (id: string, statement: number, heading: string, words: string) =>
    `:::decision{#${id} shaped="${specBlockRef(issue.key, requirementBlockId(statement))}"}\n` +
    `### ${heading}\n\n${words}\n:::`;
  const decisions = [
    decision(CHANGED_DECISION, 1, "Draw the outline at one width", CHANGED_WORDS),
    decision(WAITING_DECISION, 2, "Keep the rail", "The rail keeps its place beside the outline."),
  ];
  await editArtifact(
    spec,
    {
      ops: [
        { after: "end", markdown: `## Decisions\n\n${decisions.join("\n\n")}\n`, op: "insert" },
      ],
    },
    asPlanner
  );
  const post = async (body: string) => (await createComment(issue.key, { body })).id;
  const oldest = await post("The oldest post, older than the 200 newest.");
  const change = (
    await createComment(issue.key, {
      anchor: { artifact: "spec", quote: CHANGED_WORDS },
      body: "Draw it at two widths instead.",
    })
  ).id;
  for (let index = 1; index <= COMMENT_PAGE; index += 1) {
    await post(`Post ${index} is one of the 200 newest.`);
  }
  const specSlug =
    ((await getIssue(issue.key)).artifacts ?? []).find((artifact) => artifact.id === spec)?.slug ??
    "";
  return { change, key: issue.key, oldest, specSlug };
}

/** Records, for each older comment page the page asks for from each load on (`?comments_before=`
 *  of a tree read), whether it had drawn a row of the issue then, and answers how to read the
 *  record of the page's load: the page's first drawing of the answer comes before every such page
 *  (decision 563 (4)). */
async function recordOlderPages(page: Page): Promise<() => Promise<boolean[]>> {
  await page.addInitScript(() => {
    const older: boolean[] = [];
    window.__dispatchOlderPages = older;
    const sent = window.fetch;
    const recording = (
      input: Parameters<typeof window.fetch>[0],
      init?: Parameters<typeof window.fetch>[1]
    ) => {
      const asked = input instanceof window.Request ? input.url : String(input);
      const url = new URL(asked, window.location.href);
      if (url.pathname.endsWith("/tree") && url.searchParams.has("comments_before")) {
        older.push(document.querySelector("[data-issue-page] [data-node-row]") !== null);
      }
      return sent(input, init);
    };
    window.fetch = recording as typeof window.fetch;
  });
  return () => page.evaluate(() => window.__dispatchOlderPages ?? []);
}

/** One decision's row of the Decisions view: the part it stands in, its state's token, and the
 *  address of the Change it draws, null where it draws none. */
interface DecisionRowRead {
  readonly change: string | null;
  readonly id: string;
  readonly part: string | null;
  readonly state: string | null;
}

/** The rows of the Decisions view of each node `ids` names, in the order the view draws them. */
function decisionRowsRead(page: Page, ids: readonly string[]): Promise<DecisionRowRead[]> {
  return page.evaluate(
    (wanted) =>
      [
        ...document.querySelectorAll("[data-issue-page] [data-decisions-body] [data-node-row]"),
      ].flatMap((row) => {
        const id = row.getAttribute("data-node-id") ?? "";
        return wanted.includes(id)
          ? [
              {
                change: row.querySelector("a[data-change-link]")?.getAttribute("href") ?? null,
                id,
                part:
                  row.closest("[data-decisions-part]")?.getAttribute("data-decisions-part") ?? null,
                state: row.getAttribute("data-node-state"),
              },
            ]
          : [];
      }),
    ids
  );
}

// c-pages (decision 563 (4)).
// SLOW(>=2s): 202 comments seeded one by one, and the Decisions view, Conversation and an address
// each loaded and waited out.
test("once the page has drawn it reads the older comment pages that hold a decision's Change or an address's comment", async ({
  browser,
}) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedOlderChange, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const olderPagesAsked = await recordOlderPages(page);
    const changed = blockNodeId(seeded.specSlug, CHANGED_DECISION);
    const waiting = blockNodeId(seeded.specSlug, WAITING_DECISION);

    // Once its older page is read, the Decisions view draws the other decision in the review
    // list, whose count and the header's count it alone is in, and the changed one with its
    // Change, among the reader's own.
    await openPreset(page, seeded.key, "decisions");
    await expect
      .poll(() => decisionRowsRead(page, [changed, waiting]), {
        message: "the Decisions view does not draw the decision with the Change it was asked",
      })
      .toEqual([
        { change: null, id: waiting, part: "review", state: "waiting_review" },
        {
          change: `/issues/${seeded.key}/comments/${seeded.change}`,
          id: changed,
          part: "yours",
          state: "change_requested",
        },
      ]);
    await expect(
      page.locator('[data-issue-page] [data-decisions-part="review"] [data-decisions-count]')
    ).toHaveText("1");
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");

    // A load sends no older comment page before the page has drawn: Conversation's first page
    // holds the 200 newest comments and names the older page, which the page then reads, once.
    const opened = await openOutline(page, seeded.key, "conversation");
    expect(
      {
        before: typeof opened.answer.comments_before,
        comments: (opened.answer.nodes ?? []).filter((node) => node.kind === "comment").length,
      },
      "the first load's outline read answers 200 comments and a comments_before"
    ).toEqual({ before: "string", comments: COMMENT_PAGE });
    expect(
      olderCommentReads(opened.traffic.requests, seeded.key),
      "Conversation's load reads the older comment page other than once"
    ).toBe(1);
    expect(
      await olderPagesAsked(),
      "an older comment page was asked for before the page drew a row"
    ).toEqual([true]);
    await expect(page.getByTestId("issue-whose-turn")).toHaveText("Waiting on you (1)");

    // An address naming the oldest comment lands on its row, with the focus on it.
    await gotoIssue(page, `${presetPath(seeded.key, "conversation")}?comment=${seeded.oldest}`);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "?comment= of the oldest comment does not put the focus on its row",
      })
      .toBe(seeded.oldest);
    expect(
      await olderPagesAsked(),
      "the address's older comment page was asked for before the page drew a row"
    ).toEqual([true]);
  });
});
