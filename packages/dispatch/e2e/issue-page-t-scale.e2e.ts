/** the row reads Conversation's first screen on an issue a person and agents wrote through the
 *  server at the largest issue's shape: where the outline the tree read with kind answers draws
 *  the discussion, and the row of an ask the server holds waiting on the reader */

import type { Page, TestInfo } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { gotoIssue, phase } from "./issue-page";
import { DESKTOP, expectIssuePageDrawn, expectOpeningLanded } from "./issue-page-g";
import { steadily } from "./issue-page-t";
import { ROW_MS } from "./issue-page-t-rows";
import {
  conversationFirstPage,
  describeFirstScreen,
  discussionOrderGate,
  type FirstScreen,
  PRODUCED_KINDS,
  readFirstScreen,
  seedLargestShape,
  seedLargestShapeWaiting,
  WAITING_QUESTION,
} from "./issue-page-t-scale";
import { owedUnless, seedEventsLanded, treeReadGate } from "./issue-page-t-tree";
import { expect, test } from "./test";
import { watchApiRequests } from "./traffic";

/**
 * The first screen of Conversation on an issue at the shape the largest issue in production had
 * when its load was measured (`seedLargestShape`), with no key pressed and no scroll, once the
 * reads have settled. The body of the page is the conversation (root R1): at 1440 by 900 the
 * first screen holds the first line of the first post or question, below the header, the three
 * sentences and the children's section; on the phone, where the expanded children's section is
 * taller than the screen, no requirement, check or decision stands between that section and the
 * first post or question. The order is the tree read's to give, so the first row is owed until
 * the read answers the discussion first (`discussionOrderGate`). With one more ask, which waits
 * on the reader, the page opens on that ask's row (root R7), whatever the read's order. The head
 * of `e2e/issue-page-t-rows.ts` says what the rows of the tree read share and what their gates
 * are.
 */

/** The size a row reads the page at: the phone project's own screen, and the desktop's in every
 *  other project. */
function screenOf(testInfo: TestInfo): { height: number; width: number } {
  const { isMobile, viewport } = testInfo.project.use;
  return isMobile === true && viewport !== undefined && viewport !== null ? viewport : DESKTOP;
}

/** Opens issue `key`'s own address, once the seed's events have landed, and reads its first
 *  screen once two readings agree after the page has gone quiet (`steadily`), with no key pressed
 *  and no scroll. */
async function openConversation(
  page: Page,
  key: string,
  posts: readonly string[]
): Promise<FirstScreen> {
  const traffic = watchApiRequests(page);
  await seedEventsLanded(page);
  await gotoIssue(page, presetPath(key, "conversation"));
  await expectIssuePageDrawn(page);
  return await steadily(page, traffic, `${key}'s Conversation as it opens`, () =>
    readFirstScreen(page, posts)
  );
}

// SLOW(>=2s): an issue of 188 comments, 37 children and three documents seeded through the
// server, and Conversation loaded with its load waited out.
test("at the largest issue's shape, Conversation's first screen holds the discussion @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedLargestShape, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const size = screenOf(testInfo);
    await page.setViewportSize(size);
    const where = `at ${size.width} x ${size.height}`;
    const firstPage = conversationFirstPage(page);
    const screen = await openConversation(page, seeded.key, seeded.posts);
    owedUnless(discussionOrderGate(await firstPage));
    if (testInfo.project.use.isMobile === true) {
      expect(
        screen.before.filter((kind) => PRODUCED_KINDS.includes(kind)),
        `${where}, a requirement, check or decision stands between the children's section and the first post or question: ${describeFirstScreen(screen)}`
      ).toEqual([]);
    } else {
      expect(
        screen.discussionInView,
        `${where}, no post or question has the first line of its row on the first screen below the children's section: ${describeFirstScreen(screen)}`
      ).toBeGreaterThan(0);
    }
  });
});

// SLOW(>=2s): an issue of 188 comments, 37 children, three documents and an ask that waits on
// the reader seeded through the server, and Conversation loaded with its load waited out.
test("with a question waiting on the reader, the page opens on it at the largest issue's shape @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedLargestShapeWaiting, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const size = screenOf(testInfo);
    await page.setViewportSize(size);
    const where = `at ${size.width} x ${size.height}`;
    await openConversation(page, seeded.key, seeded.posts);
    await expectOpeningLanded(page);
    // Read for the failure's words alone, once the opening has landed.
    const screen = await readFirstScreen(page, seeded.posts);
    const waiting = page
      .locator("[data-issue-page] [data-node-row][data-node-type='ask']")
      .filter({ hasText: WAITING_QUESTION });
    await expect(
      waiting,
      `${where}, the ask that waits on the reader is no row: ${describeFirstScreen(screen)}`
    ).toHaveCount(1);
    await expect(
      waiting,
      `${where}, the page does not open with the ask that waits on the reader in view: ${describeFirstScreen(screen)}`
    ).toBeInViewport();
  });
});
