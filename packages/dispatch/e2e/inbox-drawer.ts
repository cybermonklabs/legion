/** The drawer the Inbox opens as, and the question it is read over. Shared by
 *  `inbox-drawer.e2e.ts` and the rows of that family which run against a recording
 *  (`inbox-drawer.stub.e2e.ts`). */
import type { Locator, Page, TestInfo } from "@playwright/test";

import { createAsk, createIssue, createProject } from "./api";
import { COMPACT_VIEWPORT_WIDTH } from "./inbox-threads";

export const session = {
  actor: { id: "e2e-inbox-drawer", kind: "session" as const },
  as: "agent" as const,
};

export async function seedAsk(question: string): Promise<{ askId: string; issueKey: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Drawer issue" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question },
    session
  );
  return { askId: ask.id, issueKey: issue.key };
}

export function drawer(page: Page) {
  return page.getByRole("dialog", { name: "Inbox" });
}

/**
 * The button that opens the drawer. The shell draws it at one of two widths and both carry one
 * name on this surface: the navigation rail's Peek beside its Inbox entry from
 * `COMPACT_VIEWPORT_WIDTH` up, and below that the compact top bar's own Peek beside the bar's
 * Inbox count (`web/src/app.tsx`, `features/sidebar/Sidebar.tsx`). The rail is the only
 * `Navigation` landmark, so above the fold the name is read inside it; the top bar is no
 * landmark at all, so the phone reads it in the header.
 */
export function peekTrigger(page: Page): Locator {
  const name = "Peek at the inbox without leaving this page";
  if ((page.viewportSize()?.width ?? 0) >= COMPACT_VIEWPORT_WIDTH) {
    return page.getByRole("navigation", { name: "Navigation" }).getByRole("button", { name });
  }
  return page.locator("header").first().getByRole("button", { name });
}

export async function attachShot(page: Page, testInfo: TestInfo, name: string) {
  const path = testInfo.outputPath(`${name}-${testInfo.project.name}.png`);
  await page.screenshot({ path });
  await testInfo.attach(`${name} (${testInfo.project.name})`, { contentType: "image/png", path });
}
