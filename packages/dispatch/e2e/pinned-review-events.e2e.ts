import { createIssue, createNamedVersion, createProject, reviewArtifact } from "./api";
import { expect, rowKey, test } from "./test";
import { asUser } from "./users";

// The review margin's Pinned tab draws a pinned event by its own line (`event-description.ts`,
// through `EventBody`), and a review event's line once read `Approved v1` with no document, so
// a reader could not tell which document was reviewed. This row pins an approval of the spec's
// version 1 and a changes request on its version 2, and reads each line whole: the document and
// the version.
//
// The title carries no `@pr` tag: the pull-request lane's real-stack share is at its cap
// (`realStackBudget`, e2e/__tests__/pull-request-lane.test.ts), so the full lane runs it.
//
// SLOW(>=2s): 3.5 s - a project, an issue with its spec, an approval, a named version and a
// changes request written first; then one load of the Activity tab, two pin writes and the
// margin's Pinned tab, which draws each pinned event's own line.
test("the Pinned tab names the document and the version of each review event", async ({
  browser,
}) => {
  const review = rowKey("REVIEW");
  await createProject({ key: review, name: "Review" });
  const issue = await createIssue({ project: review, spec: "The plan.", title: "Pin reviews" });
  const id = issue.primary_artifact_id;
  expect((await reviewArtifact(id, { state: "approved" }, { login: "alice" })).version).toBe(1);
  expect((await createNamedVersion(id, "Second draft", { login: "alice" })).number).toBe(2);
  expect(
    (
      await reviewArtifact(
        id,
        { reason: "Name the rollback path.", state: "changes_requested" },
        { login: "alice" }
      )
    ).version
  ).toBe(2);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/activity`);
    const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
    for (const line of ["approved spec.md v1", "requested changes on spec.md v2"]) {
      const row = rows.filter({ hasText: line });
      await expect(row).toHaveCount(1);
      await row.getByRole("button", { name: "Pin" }).click();
      await expect(row.getByRole("button", { name: "Unpin" })).toHaveAttribute(
        "aria-pressed",
        "true"
      );
    }

    const margin = page.getByTestId("margin-sheet");
    await margin.getByRole("tab", { name: "Pinned" }).click();
    const cards = margin.getByRole("article");
    await expect(cards).toHaveCount(2);
    await expect(cards.getByText("Approved spec.md v1", { exact: true })).toHaveCount(1);
    await expect(cards.getByText("Changes requested on spec.md v2", { exact: true })).toHaveCount(
      1
    );
  } finally {
    await alice.close();
  }
});
