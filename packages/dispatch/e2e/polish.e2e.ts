import { expect, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createAsk, createIssue, createProject, patchIssue } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The two rows of this family that need the real stack. The collapsed project filters and the
 * margin ask's compact composer are a stubbed row, in `polish.stub.e2e.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

// The narrow-screen half of the stubbed row. The board lays its columns out side by side, so it
// is the page a phone's width can push sideways; the stubbed row walks the same board at
// 1280 px, where there is room for them. It carries `@phone`, which is why it is not a stubbed
// row of its own: the `iphone` project runs none.
test("the project board's columns stay inside the screen @phone", async ({ browser }, testInfo) => {
  test.skip(testInfo.project.name !== "iphone", "only a phone screen is narrower than the columns");
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Polish the queue" });
  await patchIssue(issue.key, { status: "todo" });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    const boardCard = page.getByRole("article", { name: `${issue.key} Polish the queue` });
    await boardCard.scrollIntoViewIfNeeded();
    await expect(boardCard).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= document.documentElement.clientWidth
      )
    ).toBe(true);
    await page.screenshot({
      path: testInfo.outputPath("polish-project-board.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

// It is no stubbed row: the Agents page folds a session unseen for ten minutes under `Inactive`
// (`partitionAgents`, `web/src/features/agents/AgentsPage.tsx`), read from `Date.now()` against
// the `last_seen` the registry reported. A recording freezes that timestamp, so this session
// would be folded away in every replay taken more than ten minutes after the recording and the
// heading below would not be on the page at all.
test("an untitled session reads the same on the Agents page, the ask card, its followers, and the Conversation", async ({
  browser,
}) => {
  const sessionId = "e2e-label-session";
  const label = `session:${sessionId.slice(0, 8)}…`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "One label" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }], question: "Same name everywhere?" },
    { actor: { id: sessionId, kind: "session" }, as: "agent" }
  );
  await setLiveSessions([{ session_id: sessionId, title: "" }]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/agents");
    await expect(page.getByRole("heading", { level: 2, name: label })).toBeVisible();

    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 1");
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    // The author line and the follower chip both carry the label — the same text twice.
    await expect(card.getByText(label, { exact: true })).toHaveCount(2);

    await page.goto(`/issues/${issue.key}/conversation`);
    const conversation = page.getByRole("region", { name: "Conversation" });
    await expect(conversation.getByTestId(`ask-${ask.id}`)).toContainText(label);
  } finally {
    await context.close();
  }
});
