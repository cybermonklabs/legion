/** the row reads how a post's row in Conversation draws, at 1440 px and at 390 px, a reference to
 *  a comment no comment has and one to an issue that does not exist, both of which the server's
 *  batched read of reference targets answers gone, and one to a comment its author deleted after
 *  the post cited it, which that read answers with the delete, and the page's width beside the
 *  viewport's while that read is held back and once it has answered */

import type { Locator, Page, Route } from "@playwright/test";

import { createComment, createIssue, createProject, deleteComment } from "./api";
import { gotoIssue, phase } from "./issue-page";
import {
  DESKTOP,
  expectIssuePageDrawn,
  expectOpeningLanded,
  nodeRow,
  PHONE,
  rowWords,
} from "./issue-page-g";
import { ROW_MS } from "./issue-page-t-rows";
import { convertedGate, owedUnless, seedEventsLanded, treeReadGate } from "./issue-page-t-tree";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertPageFits } from "./unclipped";

/**
 * Row 15 of the rows that draw the new issue page from the tree read: a reference the page cannot
 * name - one nothing answers to, a node that is gone, an issue that does not exist or that the
 * reader may not see - is drawn as the words its author wrote, as a link of any body reads
 * (`row-references.ts`' `rowReferenceWords`), and whatever a row draws a reference with breaks
 * inside the row, so a written address of some 60 characters leaves the page no wider than the
 * viewport at 390 px, before the names read answers and after. A comment its author deleted reads
 * as written until that read answers, and as the page's deleted line once it has. The row sets
 * each viewport itself. The head of `e2e/issue-page-t-rows.ts` says what the rows read and what
 * their gates are.
 */

/** The project of the seeded issue. */
const PROJECT = "GONE";

/** The page's one batched read of what the tree's references point at. */
const NAMES_READ = /\/api\/v1\/reference-targets(?:\?|$)/u;

/** The seeded issue, its one post, and the three references the post's words hold. */
interface UnnamedSeed {
  /** The whole address of a comment of the issue that no comment has: the read answers it gone,
   *  as it answers a node that is gone or one the reader may not see. */
  readonly comment: string;
  /** The whole address of a comment of the issue its author deleted after the post cited it: the
   *  read answers it with the delete, and the page names it by the deleted line. */
  readonly deleted: string;
  /** The address of an issue of the project that does not exist: the read answers it gone. */
  readonly issue: string;
  readonly key: string;
  /** The post that cites the three, by its id. */
  readonly post: string;
}

/** An issue whose one post cites, each by its whole address, a comment of the issue that no
 *  comment has, a comment of the issue that its author deletes once the post is written, and an
 *  issue of its project that does not exist. */
async function seedUnnamed(): Promise<UnnamedSeed> {
  await createProject({ key: PROJECT, name: "References nothing answers to" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    title: "A post cites a comment and an issue that are not there",
  });
  const cited = await createComment(issue.key, { body: "The run's log is on the shared drive." });
  const comment = `dispatch://${issue.key}/comment/${crypto.randomUUID()}`;
  const deleted = `dispatch://${issue.key}/comment/${cited.id}`;
  const missing = `dispatch://${PROJECT}-404`;
  const post = await createComment(issue.key, {
    body: `The answer stood in ${comment}, and in ${deleted} before it, which ${missing} quoted.`,
  });
  await deleteComment(cited.id);
  return { comment, deleted, issue: missing, key: issue.key, post: post.id };
}

/** The post's row's own link to `reference`. */
function referenceIn(page: Page, seeded: UnnamedSeed, reference: string): Locator {
  return rowWords(nodeRow(page, seeded.post).first()).locator(
    `a[data-row-reference="${reference}"]`
  );
}

/** Fails unless the post's row draws each reference that names nothing as its author wrote it,
 *  the whole address, and the page is no wider than the viewport. */
async function expectWrittenAndFitting(
  page: Page,
  seeded: UnnamedSeed,
  where: string
): Promise<void> {
  for (const reference of [seeded.comment, seeded.issue]) {
    await expect(
      referenceIn(page, seeded, reference),
      `${where}, the post's row does not draw ${reference} as its author wrote it`
    ).toHaveText(reference);
  }
  await assertPageFits(page);
}

/** Opens Conversation at `size` with the names read held back, reads the post's row and the page's
 *  width while that read has not answered, lets it answer, and reads them again. */
async function expectUnnamedAt(
  page: Page,
  seeded: UnnamedSeed,
  size: { readonly height: number; readonly width: number }
): Promise<void> {
  await page.setViewportSize(size);
  const { promise: released, resolve: release } = Promise.withResolvers<void>();
  // Each read the route holds, which goes on once `release` is called.
  const held: Promise<void>[] = [];
  const hold = (route: Route): Promise<void> => {
    const going = released.then(() => route.continue());
    held.push(going);
    return going;
  };
  await page.route(NAMES_READ, hold);
  try {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(() => held.length, {
        message: `at ${size.width} px the page never asked the names read`,
      })
      .toBeGreaterThan(0);
    await expectWrittenAndFitting(page, seeded, `at ${size.width} px while the names read is held`);
    await expect(
      referenceIn(page, seeded, seeded.deleted),
      `at ${size.width} px while the names read is held, the deleted comment is not drawn as its author wrote it`
    ).toHaveText(seeded.deleted);
    const answered = page.waitForResponse(NAMES_READ);
    const settling = watchApiRequests(page);
    release();
    await answered;
    await expectLoadSettled(settling);
    // The outline's opening waits on every read its rows come from, so once it has landed the
    // page has drawn the rows from the names the read answered.
    await expectOpeningLanded(page);
    await expectWrittenAndFitting(page, seeded, `at ${size.width} px once the names read answered`);
    // A post its author deleted is named by the page's deleted line, which the row marks.
    const deleted = referenceIn(page, seeded, seeded.deleted);
    await expect(
      deleted,
      `at ${size.width} px once the names read answered, the deleted comment carries no deleted line's mark`
    ).toHaveAttribute("data-post-deleted", "");
    await expect(
      deleted,
      `at ${size.width} px once the names read answered, the deleted comment is still drawn as its address`
    ).not.toHaveText(seeded.deleted);
  } finally {
    release();
    // A held read goes on before its route is removed: removed first, the held read's own
    // `continue` fails on a route Playwright reads as handled already.
    await Promise.allSettled(held);
    await page.unroute(NAMES_READ, hold);
  }
}

// SLOW(>=2s): a seeded issue loaded at two widths, each with the names read held back until the
// row and the page's width are read, then let through and the load waited out.
test("a reference the page cannot name is drawn as written and breaks inside its row", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedUnnamed, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    owedUnless(await convertedGate(page, seeded.key));
    await seedEventsLanded(page);
    for (const size of [DESKTOP, PHONE]) await expectUnnamedAt(page, seeded, size);
  });
});
