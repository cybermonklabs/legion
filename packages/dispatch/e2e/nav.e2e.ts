import { expect, test } from "@playwright/test";

import { createAsk, createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * What the navigation shows in the two configurations the `iphone` project is part of. The row
 * below is read on a desktop and in the phone drawer, and a stubbed row runs in one
 * configuration only - `stub` or `stub-phone`, never both - so it stays here. The rest of this
 * family, the phone badge among it, is in `nav.stub.e2e.ts`.
 */

const session = { actor: { id: "e2e-nav", kind: "session" as const }, as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
});

test("sidebar shows Inbox, Pinned, one row per project with its open-ask badge, and Settings — never an issue row — on desktop and in the phone drawer @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const coreIssues = await Promise.all(
    Array.from({ length: 12 }, (_value, index) =>
      createIssue({ project: "CORE", title: `Core issue ${index + 1}` })
    )
  );
  const opsIssues = await Promise.all(
    Array.from({ length: 8 }, (_value, index) =>
      createIssue({ project: "OPS", title: `Operations issue ${index + 1}` })
    )
  );
  await Promise.all([
    ...coreIssues
      .slice(0, 2)
      .map((issue, index) => createAsk(issue.key, { question: `Core ask ${index + 1}` }, session)),
    ...opsIssues
      .slice(0, 1)
      .map((issue) => createAsk(issue.key, { question: "Operations ask" }, session)),
  ]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  // The sidebar must never fan out per issue. The one sanctioned per-issue request on the
  // Inbox is each ask card's `GET /issues/{key}/subscribers` (`AskRecipients`, for `Reaches N`),
  // which is the card's, not the navigation's, so the recorder leaves it out.
  const issueRequests: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "GET" && url.pathname === "/api/v1/issues") {
      issueRequests.push(url.search);
    }
    if (
      request.method() === "GET" &&
      url.pathname.startsWith("/api/v1/issues/") &&
      !url.pathname.endsWith("/subscribers")
    ) {
      issueRequests.push(url.pathname);
    }
  });

  try {
    await page.goto("/");
    if (testInfo.project.name === "iphone") {
      await page.getByRole("button", { name: "Open navigation" }).click();
    }
    const navigation = page.getByRole("navigation", { name: "Navigation" });
    await expect(navigation.getByRole("link", { name: "Inbox" })).toContainText("3");
    await expect(navigation.getByRole("heading", { name: "Pinned" })).toHaveCount(0);
    await expect(navigation.locator('a[href="/projects/CORE"]')).toContainText("2");
    await expect(navigation.locator('a[href="/projects/OPS"]')).toContainText("1");
    await expect(navigation.getByRole("link", { name: "Settings" })).toBeVisible();
    await expect(navigation.getByRole("link", { name: /^(CORE|OPS)-\d+/ })).toHaveCount(0);
    // The Inbox makes no per-issue read of its own either: the list is one `GET /inbox`, and
    // the reading pane (INFRA-575) draws the selected question from the row it already holds.
    // Only a question written as a decision block has its issue read, for the one field the
    // live document is handed - whether the issue is closed (`DecisionBody` in
    // `ReadingPane.tsx`). These three are ordinary questions, so the pane reads no issue and
    // the navigation's one read stands alone.
    await expect.poll(() => issueRequests).toEqual(["?pinned=true"]);
    await page.screenshot({ path: testInfo.outputPath("projects-sidebar.png"), fullPage: true });
  } finally {
    await context.close();
  }
});
