import {
  createComment,
  createIssue,
  createProject,
  getIssueEvents,
  listComments,
  putIssueState,
} from "./api";
import { barAction, selectEditorText } from "./editor";
import { expect, rowKey, rowPerson, test } from "./test";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-writes", origin: { tmux: "dispatch:1.5" } },
  as: "agent" as const,
};

// @pr: A write the server refuses: a failed pin offers a Retry, tells the sidebar nothing untrue, and recovers on that Retry.
test("a failed pin shows a retryable error, never lies to the sidebar, and recovers on retry @pr", async ({
  browser,
}, testInfo) => {
  const wr = rowKey("WR");
  await createProject({ key: wr, name: "Writes" });
  const issue = await createIssue({ project: wr, title: "Pin me" });

  // The sidebar's Pinned group is the reader's own pins: a person of the row's own
  // (`rowPerson`), who has pinned nothing until this row's pin lands.
  const context = await asUser(browser, rowPerson("alice"));
  const page = await context.newPage();
  let putAttempts = 0;
  let retryRequests = 0;
  await page.route("**/api/v1/me/issues/*/state", async (route) => {
    if (route.request().method() !== "PUT") {
      await route.continue();
      return;
    }
    putAttempts += 1;
    if (putAttempts === 1) {
      // Dispatch's own verdict: a dropped request, a 5xx or a 429 is tried again under the
      // write's key rather than handed back as a refusal, so only this ends the write.
      await route.fulfill({
        body: JSON.stringify({ code: "STATE_INPUT", error: "Dispatch refused the pin" }),
        contentType: "application/json",
        status: 400,
      });
      return;
    }
    retryRequests += 1;
    await route.continue();
  });

  await page.goto(`/issues/${issue.key}`);
  const header = page.locator("header");
  await expect(page.getByRole("heading", { name: "Pinned" })).toHaveCount(0);

  await header.getByRole("button", { name: "Pin issue" }).click();
  await expect(header.getByRole("alert")).toContainText("Could not save pin status.");
  const retry = header.getByRole("button", { name: "Retry" });
  await expect(retry).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("pin-error-retry.png"), fullPage: true });
  // The failed write must never be reflected anywhere: still "Pin issue", still no Pinned group.
  await expect(header.getByRole("button", { name: "Pin issue" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Pinned" })).toHaveCount(0);

  // Two synthetic clicks in the same task exercise the retry guard before React can render
  // the button disabled.
  const retryResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "PUT" &&
      response.url().includes(`/api/v1/me/issues/${issue.key}/state`) &&
      response.ok()
  );
  await retry.evaluate((button: HTMLButtonElement) => {
    button.click();
    button.click();
  });
  await retryResponse;
  await expect.poll(() => retryRequests).toBe(1);
  await expect(header.getByRole("alert")).toHaveCount(0);
  await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();

  await page.goto("/");
  await page.goto(`/issues/${issue.key}`);
  await expect(page.getByRole("heading", { name: "Pinned" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();

  await context.close();
});

// This row carries no `@pr` tag: the pull-request lane is at its row budget (`laneBudget`,
// e2e/__tests__/pull-request-lane.test.ts), and the idempotent-repeat rows of
// `keyed-writes.e2e.ts`, `keyed-send.e2e.ts` and `broadcast.e2e.ts` keep that claim on the lane.
// The full lane runs this one.
test("double-clicking Submit posts exactly one comment", async ({ browser }) => {
  const wr = rowKey("WR");
  await createProject({ key: wr, name: "Writes" });
  const issue = await createIssue({
    project: wr,
    spec: "The quick brown fox",
    title: "Double submit",
  });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();
  await selectEditorText(page, "brown");
  await barAction(page, "Comment");
  const composer = page.getByRole("form", { name: "Comment composer" });
  await composer.getByLabel("Comment").fill("dup check");
  const submit = composer.getByRole("button", { exact: true, name: "Send" });

  // Two synthetic clicks in the same task reproduce the sub-millisecond double click the
  // audit observed; Playwright's own click() serializes actionability checks and cannot.
  await submit.evaluate((button: HTMLButtonElement) => {
    button.click();
    button.click();
  });

  await expect.poll(() => listComments(issue.key, issue.primary_artifact_id)).toHaveLength(1);
  await expect(composer).toHaveCount(0);

  await context.close();
});

test("a pre-fold comment lifecycle pin remains removable from Pinned", async ({ browser }) => {
  const wr = rowKey("WR");
  await createProject({ key: wr, name: "Writes" });
  const issue = await createIssue({
    project: wr,
    spec: "The quick brown fox",
    title: "Resolve pending",
  });
  const comment = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "brown" }, body: "note" },
    session
  );

  // The pin is the reader's own, so the reader is a person of the row's own (`rowPerson`), whose
  // page draws one Unpin: the Pinned tab's, for this pin. alice's sidebar draws the sessions the
  // rows before this one pinned for her, each with an Unpin of its own.
  const me = rowPerson("alice");
  const context = await asUser(browser, me);
  const page = await context.newPage();
  await page.goto(`/issues/${issue.key}/conversation`);
  await page
    .locator(`[data-turn="comment:${comment.id}"]`)
    .getByRole("button", { name: "Expand thread" })
    .click();
  const thread = page
    .locator(`[data-turn="comment:${comment.id}"]`)
    .getByTestId(`margin-comment-${comment.id}`);
  await thread.getByRole("button", { name: "Resolve" }).click();
  await expect
    .poll(async () =>
      (await getIssueEvents(issue.key)).some(
        (event) => event.type === "comment.resolved" && event.payload.id === comment.id
      )
    )
    .toBe(true);
  const lifecycle = (await getIssueEvents(issue.key)).find(
    (event) => event.type === "comment.resolved" && event.payload.id === comment.id
  );
  if (lifecycle === undefined) {
    throw new Error("The resolved comment lifecycle event was not recorded.");
  }
  await putIssueState(
    issue.key,
    { dismissed: [`pinned_items:event:${lifecycle.id}`] },
    { login: me }
  );

  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Pinned" }).click();
  const pinnedEvent = page.getByText("Comment resolved");
  await expect(pinnedEvent).toBeVisible();
  await page.getByRole("button", { name: "Unpin" }).click();
  await expect(pinnedEvent).toHaveCount(0);
  await expect(page.getByText("No pinned items.")).toBeVisible();
  await expect
    .poll(() => getIssueEvents(issue.key))
    .toContainEqual(expect.objectContaining({ id: lifecycle.id, type: "comment.resolved" }));

  await context.close();
});
