import { expect, type Locator, type Page } from "@playwright/test";

import { navigateInApp } from "./sends";

/**
 * Moving between the issue page's presets, however the row draws itself at the width a test
 * runs at (dispatch://INFRA-1196 R3): a row of five tabs from 1280 px, and one control opening
 * a menu of five at 1024 px and below.
 *
 * `e2e/issue-layout.e2e.ts` reads the row through `e2e/issue-page.ts`, which is the contract
 * the page is built to; this is for the specs that only need to get to a preset on their way
 * to something else, and is the one place they spell how.
 */
export async function openPreset(page: Page, name: string): Promise<void> {
  const row = page.getByTestId("issue-presets");
  // The row has to be on the page before its shape can be read: a count taken while the page
  // is still loading answers zero for both shapes and would send every caller to the control
  // that is not there.
  await row.waitFor();
  const tab = row.getByRole("tab", { exact: true, name });
  if ((await tab.count()) > 0) {
    await tab.click();
    return;
  }
  await row.getByRole("button").click();
  await page.getByRole("menu").getByRole("menuitem", { exact: true, name }).click();
}

/** Opens one of the addresses the old tabs answered, from wherever the page is on that issue:
 *  every one of them still resolves (dispatch://INFRA-1196 R19), and a spec that only needs to
 *  be there goes straight to it rather than pressing a control that no longer exists. */
export async function openIssueAddress(page: Page, tab: string): Promise<void> {
  const key = new URL(page.url()).pathname.match(/^\/issues\/([A-Z][A-Z0-9]*-\d+)/)?.[1] ?? "";
  // Inside the page, as the tab it replaces was: a reload would drop a staged upload, a held
  // transport and everything else the page is holding when a test moves between addresses.
  await navigateInApp(page, `/issues/${key}/${tab}`);
}

/** The identity block's one actions control, open. Close, reopen and the control that gives the
 *  issue a parent are behind it now (dispatch://INFRA-1196 R11), where the eight-tab page drew
 *  them as bare buttons. The control toggles, so this opens it only while it is shut: a spec
 *  that asks twice leaves the actions on screen rather than hiding them again. */
export async function openIssueActions(page: Page): Promise<void> {
  const control = page.getByRole("button", { name: "Issue actions" });
  if ((await control.getAttribute("aria-expanded")) !== "true") {
    await control.click();
  }
}

/** Presses one of the identity block's actions by the name it carries - `Close issue`,
 *  `Reopen issue` - opening the control that holds it first. */
export async function runIssueAction(page: Page, name: string): Promise<void> {
  await openIssueActions(page);
  await page.getByRole("button", { exact: true, name }).click();
}

/** Fails until the page has taken in that its issue is closed. The identity block's own line is
 *  what says so (R11): Reopen is behind the actions control, so a closed issue no longer draws
 *  a bare `Reopen` button for a spec to wait on. */
export async function expectIssueClosed(page: Page): Promise<void> {
  await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
}

/** Fails unless the page is on `name`, whichever shape the row has at this width. */
export async function expectActivePreset(page: Page, name: string): Promise<void> {
  const row = page.getByTestId("issue-presets");
  await row.waitFor();
  const tab = row.getByRole("tab", { exact: true, name });
  await expect((await tab.count()) > 0 ? tab : row.getByRole("button")).toHaveAccessibleName(name);
}

/** One of the conversation panel's four state controls, which is where a tab's `aria-selected`
 *  moved to (R12). */
export function conversationState(page: Page, state: string): Locator {
  return page.getByTestId("issue-margin").locator(`[data-conversation-state="${state}"]`);
}
