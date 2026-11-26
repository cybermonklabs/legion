/** the row reads whether the Spec view at 390 px, on a phone with touch, draws a statement whose
 *  words cite thirty comments of its issue, each named by a read of its own, without React
 *  stopping the page */

import type { Page } from "@playwright/test";

import { createComment, createIssue, createProject, editArtifact } from "./api";
import { withIssuePage } from "./issue-page-option";
import { ROW_MS } from "./issue-page-t-rows";
import { expect, test } from "./test";
import { asUser } from "./users";

/**
 * Below 1024 px the Spec view draws every statement of the spec, and a statement's words name
 * each post they cite by its own text, read by a read of its own (`refs/plain-text.tsx`'s
 * `OwnNameRead`). Those reads answer one after another. A name each answer brings must land in
 * the commits of that answer: an update of the statement left waiting on a later task makes React
 * count every commit after it, answer after answer, as one nested update, and past fifty of them
 * React stops the page ("Maximum update depth exceeded", minified as error #185) and the page
 * draws "Something went wrong in this page." On a preview at 390 px, one statement citing 30
 * comments, and eight citing 5 each, did that in every load; four citing 5 each, or 3 each, did
 * not.
 */

/** The project of the seeded issue. */
const PROJECT = "CITE";

/** How many comments the statement cites. */
const CITED = 30;

/** What comment `at` (from 1) says: its name, wherever a reference to it is drawn. */
function caseWords(at: number): string {
  return `Case ${at} is written up here.`;
}

/** The page a phone draws: 390 px wide, a phone's screen, and touch. */
const PHONE_CONTEXT = { hasTouch: true, isMobile: true, viewport: { height: 844, width: 390 } };

/** What React's stop prints, from a development build and from a production one. */
const STOPPED = /Maximum update depth exceeded|Minified React error #185/u;

/** What the page draws in place of a view React stopped. */
const FAILED_PAGE = "Something went wrong in this page.";

/** An issue whose spec holds one statement whose words cite `CITED` comments of the issue, each by
 *  its whole address. */
async function seedCitations(): Promise<{ readonly key: string }> {
  await createProject({ key: PROJECT, name: "Many posts cited" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: "# Cases\n",
    title: "A statement cites many comments",
  });
  const cited: string[] = [];
  for (let at = 1; at <= CITED; at += 1) {
    const comment = await createComment(issue.key, { body: caseWords(at) });
    cited.push(`dispatch://${issue.key}/comment/${comment.id}`);
  }
  await editArtifact(issue.primary_artifact_id, {
    ops: [
      {
        after: "end",
        markdown: `:::requirement{#r1}\nThe cases stand on ${cited.join(", ")}.\n:::\n`,
        op: "insert",
      },
    ],
  });
  return { key: issue.key };
}

/** Every error the page prints or throws that says React stopped it, from now on. */
function watchStops(page: Page): readonly string[] {
  const stops: string[] = [];
  page.on("pageerror", (error) => {
    if (STOPPED.test(String(error))) stops.push(String(error).slice(0, 300));
  });
  page.on("console", (message) => {
    if (message.type() === "error" && STOPPED.test(message.text())) {
      stops.push(message.text().slice(0, 300));
    }
  });
  return stops;
}

// SLOW(>=2s): thirty comments seeded, and the Spec view loaded at 390 px until every one of them
// is named in the statement's words, or the page has failed.
test("the Spec view at 390 px names thirty comments a statement cites, each read on its own, and React never stops it @phone @phone-only", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  const seeded = await seedCitations();
  const context = await asUser(browser, "alice", PHONE_CONTEXT);
  try {
    await withIssuePage(context, "alice");
    const page = await context.newPage();
    const stops = watchStops(page);
    // React can stop the page before the issue's column is drawn, so the load waits on nothing
    // of the page's own: every name lands, or the page fails, and the reads answer in any order.
    await page.goto(`/issues/${seeded.key}/spec`);
    const failed = page.getByText(FAILED_PAGE);
    const statement = page.locator("[data-spec-statement]").first();
    const named = async (): Promise<boolean> => {
      const words = (await statement.allTextContents()).join(" ");
      return Array.from({ length: CITED }, (_, at) => caseWords(at + 1)).every((one) =>
        words.includes(one)
      );
    };
    await expect
      .poll(async () => (await failed.count()) > 0 || (await named()), {
        message: "the statement never named every comment it cites, and the page never failed",
        timeout: 30_000,
      })
      .toBe(true);
    await expect(failed, `the page failed: ${stops.join(" | ")}`).toHaveCount(0);
    expect(stops, "React stopped the page").toEqual([]);
  } finally {
    await context.close();
  }
});
