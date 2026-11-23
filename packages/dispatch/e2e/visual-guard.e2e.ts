/** The visual review's write guard (`e2e/visual-guard.ts`), on a real page of a real stack: the
 *  proof that it can fire. The visual rows run on the full lane, inside the pinned Playwright
 *  image, against a corpus they must not change, and every one of them passes only while the
 *  guard stops nothing; a guard that could no longer stop a write would pass them all the same.
 *  So this row plants the two kinds of write the guard exists for on a page it guards - a POST to
 *  the API and a Yjs update typed into the spec - and holds that each is stopped, named, and never
 *  reaches the server, while the Spec tab's read mark, which the visual seed writes before any
 *  visual row and so lets through, passes. It runs on the full lane, the lane of the visual rows
 *  it vouches for: in every merge group, and on a pull request under `full-ci`. It carries no
 *  `@pr` tag because the pull-request lane's real-stack rows are at their budget of 150
 *  (e2e/__tests__/pull-request-lane.test.ts), and a guard that guards no row there has nothing
 *  to answer for on that lane. */
import { createIssue, createProject } from "./api";
import { documentEditor, typeAtEnd } from "./editor";
import { expect, test } from "./test";
import { asUser } from "./users";
import { guardSharedCorpus } from "./visual-guard";

const SEEDED_LINE = "The corpus stays as it was seeded.";
const PLANTED_WORDS = "planted words the guard must stop";

// SLOW(>=2s): a sign-in, a document that connects and syncs, a typed edit, and a second page that reads the document back.
test("the visual review's guard lets the Spec tab's read mark through and stops a planted POST and a typed Yjs update, naming each", async ({
  browser,
}) => {
  await createProject({ key: "GUARD", name: "Guard" });
  const issue = await createIssue({
    project: "GUARD",
    spec: `## Guarded\n\n${SEEDED_LINE}\n`,
    title: "Guarded spec",
  });
  const readMark = `/api/v1/me/documents/${issue.primary_artifact_id}/read`;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const writes = await guardSharedCorpus(page);
    const markSent = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" && new URL(response.url()).pathname === readMark
    );
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText(SEEDED_LINE);
    // The passes: the Spec tab records its read of the version it shows, the one write the visual
    // seed makes first for its own spec, and the server takes it; and the editor stamps the
    // heading's id as it loads the spec, which the guard drops as derived rather than naming.
    expect((await markSent).status()).toBeLessThan(300);
    await expect.poll(() => writes.derived).toBeGreaterThan(0);
    expect(writes.stopped).toEqual([]);

    // A planted POST, as a page that wrote as it drew would send it: stopped before it leaves the
    // browser, and named.
    const planted = await page.evaluate(async (key) => {
      try {
        const response = await fetch(`/api/v1/issues/${key}/comments`, {
          body: JSON.stringify({ body: "planted" }),
          headers: { "Content-Type": "application/json" },
          method: "POST",
        });
        return `answered ${response.status}`;
      } catch {
        return "stopped";
      }
    }, issue.key);
    expect(planted).toBe("stopped");
    expect(writes.stopped).toEqual([
      expect.stringMatching(new RegExp(`^POST /api/v1/issues/${issue.key}/comments `, "u")),
    ]);

    // A typed edit: the editor's Yjs update is stopped on the document socket, and named.
    await typeAtEnd(page, PLANTED_WORDS);
    await expect(documentEditor(page)).toContainText(PLANTED_WORDS);
    await expect
      .poll(() => writes.stopped.filter((write) => write.startsWith("a Yjs update to document")))
      .not.toEqual([]);
  } finally {
    await alice.close();
  }

  // Neither write reached the server: a page with no guard reads the spec as it was seeded.
  const bob = await asUser(browser, "bob");
  try {
    const page = await bob.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(page)).toContainText(SEEDED_LINE);
    await expect(documentEditor(page)).not.toContainText(PLANTED_WORDS);
  } finally {
    await bob.close();
  }
});
