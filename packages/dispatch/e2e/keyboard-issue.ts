/** Where the issue and project rows start from: the page open, the focus on the page itself.
 *  Shared by `keyboard-issue.e2e.ts` and the rows of that family which run against a recording
 *  (`keyboard-issue.stub.e2e.ts`). */
import { expect, type Page } from "@playwright/test";

/** The keymap is bound only once sign-in resolves (`AuthGate` renders a skeleton until the
 *  `["whoami"]` query answers, whose cold read is `GET /api/v1/landing`), so a key pressed before
 *  the page renders reaches no handler. */
export async function openIssue(page: Page, key: string, title: string): Promise<void> {
  await page.goto(`/issues/${key}`);
  await expect(
    page.getByTestId("issue-header").getByRole("heading", { level: 1, name: title })
  ).toBeVisible();
  await page.locator("body").focus();
}

/** Whether the page itself holds focus, which is where every row below starts from. */
export function onBody(page: Page): Promise<boolean> {
  return page.evaluate(() => document.activeElement === document.body);
}
