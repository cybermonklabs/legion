import {
  createArtifactComment,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  listArtifactComments,
  listComments,
} from "./api";
import { connectedDot, expectMark, markSpan, openedThreadCard, setSheet } from "./editor";
import { setCreatedAt } from "./seed";
import { expect, rowKey, test } from "./test";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-margin", origin: { tmux: "dispatch:1.4" } },
  as: "agent" as const,
};
const initialMarkdown = "The quick brown fox";

// The server writes RFC 3339 and drops trailing fractional zeros, so two replies in one second come
// back as strings of different widths, and as text `…01.12Z` sorts after `…01.123456Z`, the later
// time. The margin lists the replies in the order they were written.
// This row carries no `@pr` tag: the pull-request lane is at its row budget (`laneBudget`,
// e2e/__tests__/pull-request-lane.test.ts). Two unit rows keep the order claim on every pull
// request: `web/src/features/margin/useMarginItems.test.ts`'s "replies written in the same second
// follow their times, not their timestamp strings", which lists a thread's two same-second replies
// in the order they were written, and `web/src/lib/timestamps.test.ts`'s "two times in one second
// order by time, though their strings sort the other way", which holds `compareTimestamps` to that
// order. The full lane runs this one.
test("the margin lists replies written in the same second in the order they were written", async ({
  browser,
}) => {
  const order = rowKey("ORDER");
  await createProject({ key: order, name: "Reply order" });
  const issue = await createIssue({
    project: order,
    spec: initialMarkdown,
    title: "Reply order",
  });
  const root = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "brown" }, body: "root of the thread" },
    session
  );
  if (root.anchor === null) {
    throw new Error("The thread root has no anchor.");
  }
  const second = await createComment(
    issue.key,
    { body: "written second", reply_to: root.id },
    session
  );
  const first = await createComment(
    issue.key,
    { body: "written first", reply_to: root.id },
    session
  );
  await setCreatedAt("comments", first.id, "2026-10-02T00:00:01.12Z");
  await setCreatedAt("comments", second.id, "2026-10-02T00:00:01.123456Z");
  // The fixture holds only if the server hands back the two widths the trap needs.
  const stamped = new Map(
    (await listComments(issue.key)).map((item) => [item.id, item.created_at])
  );
  expect([stamped.get(first.id), stamped.get(second.id)]).toEqual([
    "2026-10-02T00:00:01.12Z",
    "2026-10-02T00:00:01.123456Z",
  ]);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");
    await expectMark(page, root.anchor.mark_id, "brown");
    await markSpan(page, root.anchor.mark_id).click();
    const card = openedThreadCard(page, "chromium", root.id);
    await expect(card.getByRole("list", { name: "Replies" }).getByRole("listitem")).toHaveText([
      /written first/,
      /written second/,
    ]);
  } finally {
    await alice.close();
  }
});

// A standalone document's document-level threads have no mark, so none of them is placed, and the
// margin lists them newest first: by time, where two written in one second are the trap above.
test("the margin lists unplaced threads written in the same second newest first", async ({
  browser,
}) => {
  const unpl = rowKey("UNPL");
  await createProject({ key: unpl, name: "Unplaced order" });
  const document = await createProjectDocument(unpl, {
    content: "The astrolabe handbook is a project document.",
    name: "handbook.md",
  });
  // Posted in the reverse of the times they are given, so creation order cannot pass the check.
  const newer = await createArtifactComment(document.artifact.id, { body: "root written second" });
  const older = await createArtifactComment(document.artifact.id, { body: "root written first" });
  await setCreatedAt("comments", older.id, "2026-10-02T00:00:01.12Z");
  await setCreatedAt("comments", newer.id, "2026-10-02T00:00:01.123456Z");
  const stamped = new Map(
    (await listArtifactComments(document.artifact.id)).map((item) => [item.id, item])
  );
  expect(
    [older.id, newer.id].map((id) => [stamped.get(id)?.anchor, stamped.get(id)?.created_at])
  ).toEqual([
    [null, "2026-10-02T00:00:01.12Z"],
    [null, "2026-10-02T00:00:01.123456Z"],
  ]);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/projects/${unpl}/documents/${document.artifact.slug}`);
    await expect(connectedDot(page)).toHaveText("connected");
    await setSheet(page, "chromium", true);
    const cards = page.getByTestId("margin-sheet").locator('[data-testid^="margin-comment-"]');
    await expect(cards).toHaveText([/root written second/, /root written first/]);
    // The list's order is the order on screen: the newer card sits above the older one.
    const [upper, lower] = await Promise.all([
      cards.nth(0).boundingBox(),
      cards.nth(1).boundingBox(),
    ]);
    expect(upper?.y ?? Number.POSITIVE_INFINITY).toBeLessThan(lower?.y ?? Number.NEGATIVE_INFINITY);
  } finally {
    await alice.close();
  }
});
