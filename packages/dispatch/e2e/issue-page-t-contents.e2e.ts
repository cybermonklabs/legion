/** the rows read the Contents pane and sheet beside each view of an issue the server seeds and
 *  serves, and count the tree reads each load and press sends the server */

import type { Locator, Page } from "@playwright/test";
import type { IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { PRESET_NAMES, type Preset } from "../web/src/features/issue-page/page-words";
import { nextTreeCursor } from "../web/src/features/issue-page/tree-read/tree-nodes";
import { createComment, createIssue, createIssueArtifact, createProject, getIssue } from "./api";
import { seedWholeIssue } from "./issue-layout-seed";
import { PROJECT } from "./issue-layout-seed-support";
import { contentsPane, gotoIssue, outlineControl, outlineSheet, phase } from "./issue-page";
import { DESKTOP, nodeRows, PHONE } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import { drawnContentsEntries } from "./issue-page-j";
import { FILTERS, loadViews, PAGE_VIEWS, ROW_MS } from "./issue-page-t-rows";
import {
  decisionsViewGate,
  heldAnswer,
  outlineAnswerOf,
  outlineReads,
  owedUnless,
  seedEventsLanded,
  treePath,
  treeReadGate,
  treeReads,
} from "./issue-page-t-tree";
import { expect, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Row 8 of the rows that draw the new issue page from the tree read: the Contents pane stands
 * beside Conversation, each filter and Graph from 1024 px up, the Outline control opens its list
 * as a sheet below, and the three sentences are drawn once in each view (spec R13). Beside Graph
 * the list reads Graph's read, so it holds the entries of the pages of that read the page has
 * loaded, and grows as each next page loads (R13, R15): the second row reads that on an issue
 * whose Graph read takes three pages. The head of `e2e/issue-page-t-rows.ts` says what the rows
 * read and what their gates are.
 */

/** The issue's three sentences, wherever a view draws them (`IssueThreeThings.tsx`). */
function threeThings(scope: Page | Locator): Locator {
  return scope.locator('[data-testid="issue-three-things"]');
}

/** The ids of the entries a Contents list draws, sorted: two lists hold the same entries where
 *  they hold the same ids, whichever outline read each takes its order from. */
async function entriesOf(scope: Locator): Promise<string[]> {
  const ids = await drawnContentsEntries(scope).evaluateAll((all) =>
    all.map((entry) => entry.getAttribute("data-contents-entry") ?? "")
  );
  return ids.sort();
}

/** Opens `view` of issue `key` cold and waits until its load has gone quiet. */
async function openView(page: Page, key: string, view: Preset): Promise<ApiRequests> {
  const traffic = watchApiRequests(page);
  await gotoIssue(page, presetPath(key, view));
  await expectLoadSettled(traffic);
  return traffic;
}

// c-contents-beside.
// SLOW(>=2s): the whole seeded issue, each of six views loaded cold at two widths with each load
// waited out, the Outline sheet opened in each at 390 px, and a filter pressed after a load.
test("the Contents pane stands beside each filter and Graph @phone", async ({ browser }) => {
  test.setTimeout(2 * ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const issue = await getIssue(seeded.key);

    // From 1024 px up: the pane, with the three sentences and the list, beside each view, the
    // list holding the same entries in each, and the sentences drawn once, in the pane.
    await page.setViewportSize(DESKTOP);
    let entries: string[] = [];
    for (const view of PAGE_VIEWS) {
      const name = PRESET_NAMES[view];
      // Conversation's read, watched from before its load, which only that view's load reads.
      const conversation =
        view === "conversation" ? outlineAnswerOf(page, "conversation") : undefined;
      const traffic = await openView(page, seeded.key, view);
      await expect(contentsPane(page), `the Contents pane stands beside ${name}`).toHaveCount(1);
      await expect(
        threeThings(contentsPane(page)),
        `at 1440 px, the pane beside ${name} draws no three sentences`
      ).toHaveCount(1);
      await expect(
        threeThings(page),
        `at 1440 px, ${name} draws the three sentences other than once, in the pane`
      ).toHaveCount(1);
      if (conversation !== undefined) {
        await expect
          .poll(async () => (await entriesOf(contentsPane(page))).length, {
            message: "the Contents list beside Conversation holds no entry",
          })
          .toBeGreaterThan(0);
        entries = await entriesOf(contentsPane(page));
        // The two rows under the three sentences take their counts from reads the page already
        // makes: the load sends no read of the tree but Conversation's, R4's view reads and, where
        // the decisions read is the tree read's `view=decisions`, that one (`decisionsViewGate`).
        const views = loadViews(issue, await heldAnswer(conversation, "Conversation's read"));
        const sent = traffic.requests;
        const decisions = decisionsViewGate(sent, seeded.key).met ? ["decisions"] : [];
        expect(
          {
            graph: outlineReads(sent, seeded.key, "graph"),
            kind: outlineReads(sent, seeded.key, "conversation"),
            views: FILTERS.flatMap((one) =>
              treeReads(sent, seeded.key, one) > 0 ? [one] : []
            ).sort(),
          },
          "Conversation's load sends no tree read but its kind= read, R4's view reads and the " +
            "Decisions view's read where the decisions read is one"
        ).toEqual({ graph: 0, kind: 1, views: [...views, ...decisions].sort() });
        // Spec turned on after Conversation has loaded reads Conversation's read from the cache.
        const pressed = traffic.requests.length;
        await pickPreset(page, PRESET_NAMES.spec);
        await expectActivePreset(page, PRESET_NAMES.spec);
        await expectLoadSettled(traffic);
        expect(
          outlineReads(traffic.requests.slice(pressed), seeded.key, "conversation"),
          "turning Spec on after Conversation has loaded sends no second kind= read"
        ).toBe(0);
      } else {
        await expect
          .poll(() => entriesOf(contentsPane(page)), {
            message: `the Contents list beside ${name} holds other entries than beside Conversation`,
          })
          .toEqual(entries);
      }
      if (view === "spec") {
        expect(
          {
            kind: outlineReads(traffic.requests, seeded.key, "conversation"),
            spec: treeReads(traffic.requests, seeded.key, "spec"),
          },
          "a cold load of /spec at 1440 px sends one kind= read beside the view=spec read"
        ).toEqual({ kind: 1, spec: 1 });
      }
      if (view === "graph") {
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /graph sends no kind= read"
        ).toBe(0);
      }
    }

    // Below 1024 px: the three sentences above the view's first row, once, and the Outline
    // control's sheet holding the same list and not the sentences.
    await page.setViewportSize(PHONE);
    for (const view of PAGE_VIEWS) {
      const name = PRESET_NAMES[view];
      const traffic = await openView(page, seeded.key, view);
      // The one control the view row is at 390 px names the view the address opened, Conversation
      // and Graph by their word alone, since neither draws a signal.
      await expectActivePreset(page, name);
      await expect(contentsPane(page), `at 390 px, ${name} draws the Contents pane`).toHaveCount(0);
      await expect(
        threeThings(page),
        `at 390 px, the three sentences stand in ${name} other than once`
      ).toHaveCount(1);
      const [sentences, firstRow] = await Promise.all([
        threeThings(page).boundingBox(),
        nodeRows(page).first().boundingBox(),
      ]);
      expect(
        (sentences?.y ?? Number.POSITIVE_INFINITY) + (sentences?.height ?? 0),
        `at 390 px, the three sentences stand above ${name}'s first row`
      ).toBeLessThanOrEqual(firstRow?.y ?? Number.NEGATIVE_INFINITY);
      if (view === "spec") {
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /spec at 390 px sends no kind= read before the sheet opens"
        ).toBe(0);
      }
      if (view === "verification") {
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /verification at 390 px sends the kind= read with the load"
        ).toBe(1);
      }
      await outlineControl(page).click();
      await expect(
        outlineSheet(page),
        `at 390 px, the Outline control opens no sheet in ${name}`
      ).toHaveCount(1);
      await expect
        .poll(() => entriesOf(outlineSheet(page)), {
          message: `at 390 px, the Outline sheet draws the Contents list in ${name}, with the entries beside Conversation`,
        })
        .toEqual(entries);
      if ((FILTERS as readonly string[]).includes(view)) {
        await expect(
          threeThings(outlineSheet(page)),
          `at 390 px, under ${name} the Outline sheet draws the three sentences`
        ).toHaveCount(0);
      }
      if (view === "spec") {
        await expectLoadSettled(traffic);
        expect(
          outlineReads(traffic.requests, seeded.key, "conversation"),
          "a cold load of /spec at 390 px sends the kind= read when the sheet opens"
        ).toBe(1);
      }
      await page.keyboard.press("Escape");
      await expect(
        outlineSheet(page),
        `at 390 px, Escape does not close the sheet in ${name}`
      ).toHaveCount(0);
    }
  });
});

/** The posts alice writes on the issue whose Graph read takes three pages, in the order she
 *  writes them: each is the whole of its words, which is the line its Contents entry draws. */
const PAGED_POSTS = ["First post", "Second post", "Third post", "Fourth post"] as const;

/** How many of the plan's tasks stand under each post, by the post's place in `PAGED_POSTS`. A
 *  task is no entry, and Conversation's read names no `kind=plan_task`, so it holds none of them
 *  (`planTasksOut`); Graph's read holds each under its post, after the post's own block and before
 *  the next post, since the read walks a node, then its children, then its next sibling. */
const TASKS_UNDER = [1, 2, 2, 0] as const;

/** How many times each task's words write `& `. */
const TASK_AMPERSANDS = 59_000;

/** Which posts each page of Graph's read holds, by their place in `PAGED_POSTS`. A page ends at
 *  2,000,000 bytes, after the last whole node of the walk that fits (spec R15, decision 443). A
 *  task is a node of about 828 kB of the read's JSON and a post with its block about 1.2 kB, so a
 *  page holds two tasks and the posts beside them and never three, for any task between 0.67 and
 *  1 MB: the first page ends after the task under the second post, the second after the first
 *  task under the third post. Measured at eec04dd1f4: pages of 1,657,673, 1,655,927 and 828,714
 *  bytes. */
const POSTS_BY_PAGE: readonly (readonly number[])[] = [[0, 1], [2], [3]];

/** The words of each task: `&` again and again, which the read's JSON writes as `\u0026`, six
 *  bytes, in the task's text and again in its display tree, so 118 kB of words are a node of
 *  about 828 kB. Plain text, which cites nothing the page would name. */
const TASK_WORDS = "& ".repeat(TASK_AMPERSANDS).trim();

/** How many pages `graphPages` reads at most: far more than the seed's read takes, so a read
 *  whose cursor never ends fails the row here. */
const PAGES_READ_AT_MOST = 10;

/** What `seedGraphPages` wrote. */
interface GraphPagesSeed {
  readonly key: string;
  /** Each post's id, in the order of `PAGED_POSTS`. */
  readonly posts: readonly string[];
}

/** An issue of four posts alice wrote, the reader's own threads and so the Contents list's
 *  entries, written one after another, so each was made after the one before; then its plan, whose
 *  tasks each name a post as their `parent` (`TASKS_UNDER`), which places them under it, and whose
 *  bytes put the posts on three pages of Graph's read (`POSTS_BY_PAGE`). */
async function seedGraphPages(): Promise<GraphPagesSeed> {
  await createProject({ key: PROJECT, name: "Issue layout" });
  const { key } = await createIssue({
    force: true,
    project: PROJECT,
    title: "An issue whose Graph read takes three pages",
  });
  const posts: string[] = [];
  for (const words of PAGED_POSTS) posts.push((await createComment(key, { body: words })).id);
  const tasks = TASKS_UNDER.flatMap((count, at) =>
    Array.from({ length: count }, () => `dispatch://${key}/comment/${posts[at] ?? ""}`)
  );
  await createIssueArtifact(key, {
    content: `${tasks
      .map((parent, at) => `:::plan_task{#t${at + 1} parent="${parent}"}\n${TASK_WORDS}\n:::`)
      .join("\n\n")}\n`,
    name: "plan.md",
  });
  return { key, posts };
}

/** Every page of Graph's read of issue `key`, as the page receives it: the tree read with no view
 *  and no kind, each next page by the cursor the page before it names, with the display trees
 *  the page asks for (`Dispatch-Trees`), since a page ends at its bound of bytes and those trees
 *  are half of them. Read through the signed-in page's own request context, and not through the
 *  page, so it reads the same pages whatever the page reads. */
async function graphPages(page: Page, key: string): Promise<IssueTreeRead[]> {
  const path = treePath(key);
  const pages: IssueTreeRead[] = [];
  let cursor: string | undefined;
  do {
    const response = await page.request.get(path, {
      headers: { "Dispatch-Trees": "1" },
      ...(cursor === undefined ? {} : { params: { cursor } }),
    });
    if (!response.ok()) throw new Error(`GET ${path} answers ${response.status()}`);
    const answer = (await response.json()) as IssueTreeRead;
    pages.push(answer);
    cursor = nextTreeCursor(answer);
  } while (cursor !== undefined && pages.length < PAGES_READ_AT_MOST);
  return pages;
}

/** The words of each of the seed's posts each of `pages` holds, page by page. */
function postsByPage(pages: readonly IssueTreeRead[], seeded: GraphPagesSeed): string[][] {
  return pages.map((one) => {
    const ids = new Set((one.nodes ?? []).map((node) => node.id));
    return PAGED_POSTS.filter((_, at) => ids.has(seeded.posts[at] ?? ""));
  });
}

/** The line each entry of the Contents list in `scope` draws, in the list's order. */
async function entryNames(scope: Locator): Promise<string[]> {
  const lines = await drawnContentsEntries(scope).locator("[data-contents-line]").allTextContents();
  return lines.map((line) => line.trim());
}

/** How long the list may take to draw what a load or a press brought: a page of Graph's read is
 *  two million bytes, read, merged and drawn. */
const PAGE_DRAWN_MS = 30_000;

// c-contents-beside.
// SLOW(>=2s): a plan of five tasks seeded and indexed, each a node of about 828 kB of Graph's read,
// Graph loaded cold and each of its next two pages read with each read waited out, and
// Conversation loaded cold.
test("beside Graph the Contents list holds the entries of the pages Graph has loaded @phone", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedGraphPages, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    // The seed's posts as Graph's read holds them, page by page, read from the server and not
    // from the page, once the block index has placed the plan's tasks: the bytes between the
    // posts put them there, so a server that ends a page elsewhere fails here, before the page is
    // read at all. Then the index's last events have landed, so none of them reads the tree again
    // under the row.
    const byPage = POSTS_BY_PAGE.map((posts) => posts.map((at) => PAGED_POSTS[at] ?? ""));
    await expect
      .poll(async () => postsByPage(await graphPages(page, seeded.key), seeded), {
        message: "Graph's read holds the seed's posts on other pages than the seed's",
        timeout: 60_000,
      })
      .toEqual(byPage);
    await seedEventsLanded(page);

    // At 1440 px the pane stands beside Graph, and its list holds the entries of the pages of
    // Graph's read the page has loaded: the first on the load, then each next one the control
    // after the outline's last row reads on a press (R15), and at the end every post.
    const traffic = await openView(page, seeded.key, "graph");
    await expect(contentsPane(page), "the Contents pane stands beside Graph").toHaveCount(1);
    const next = page.locator("[data-issue-page] [data-tree-more] button");
    const loaded: string[] = [];
    for (const [at, posts] of byPage.entries()) {
      if (at > 0) {
        await expect(
          next,
          `with ${at} of Graph's ${byPage.length} pages loaded, no control reads the next`
        ).toHaveCount(1);
        await next.click();
        await expectLoadSettled(traffic);
      }
      loaded.push(...posts);
      await expect
        .poll(() => entryNames(contentsPane(page)), {
          message:
            at === 0
              ? "beside Graph with its first page loaded, the Contents list holds other entries than that page's posts, in their order"
              : `beside Graph with ${at + 1} of its ${byPage.length} pages loaded, the Contents list holds other entries than those pages' posts, in their order`,
          timeout: PAGE_DRAWN_MS,
        })
        .toEqual(loaded);
    }
    await expect(
      next,
      "with every page of Graph's read loaded, a control reads one more"
    ).toHaveCount(0);

    // Beside Conversation the list reads Conversation's read, which holds no plan task and so
    // every post on its one page.
    const conversation = outlineAnswerOf(page, "conversation");
    await openView(page, seeded.key, "conversation");
    expect(
      postsByPage([await heldAnswer(conversation, "Conversation's read")], seeded),
      "Conversation's read holds other posts than every post"
    ).toEqual([[...PAGED_POSTS]]);
    await expect
      .poll(() => entryNames(contentsPane(page)), {
        message:
          "beside Conversation, the Contents list holds other entries than Conversation's read's posts, in their order",
        timeout: PAGE_DRAWN_MS,
      })
      .toEqual([...PAGED_POSTS]);
  });
});
