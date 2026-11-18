// The sidebar's Agents list (INFRA-812, R1), as a browser check reads it: the rows are the
// person's pinned agents, oldest pin first, and below 1280 pixels the whole rail is a sheet
// behind the Menu control rather than a column of the page. Every row that reads the rail goes
// through here, so one place knows where the rail is at the width it is run at.
import { expect, type Locator, type Page } from "@playwright/test";

import type { LiveViewState } from "../web/src/features/agent-view/live-views";
import { COMPACT_VIEWPORT_WIDTH } from "./inbox-threads";

/** What the Agents heading's chevron folds (`PINNED_AGENTS_ID`, `features/sidebar/Sidebar.tsx`):
 *  the list of pinned agents, or the one line that stands in for it while the person has pinned
 *  nothing. Folded away, it is in no part of the page. */
export const RAIL_AGENTS_ID = "rail-pinned-agents";

/** The three words a rail row can say its live view is doing, so a row that holds one of them
 *  holds the other two to absent. */
const VIEW_STATES: readonly LiveViewState[] = ["live", "live 30s", "asleep"];

/** The sheet the rail is below 1280 pixels: the same navigation, in a dialog over the page. */
function railSheet(page: Page): Locator {
  return page.getByRole("dialog", { name: "Navigation" });
}

/** Whatever the Agents heading folds right now: the `ul` of pinned agents, or the `p` saying
 *  where the pin is. */
export function railAgents(page: Page): Locator {
  return page.locator(`#${RAIL_AGENTS_ID}`);
}

/** The person's pinned agents in the rail, in the order the rail draws them. */
export function railAgentRows(page: Page): Locator {
  return page.locator(`#${RAIL_AGENTS_ID} > li`);
}

/** One session's rail row, found by the agent it names rather than by where it sits. */
export function railAgentRow(page: Page, sessionId: string): Locator {
  return railAgentRows(page).filter({ has: page.locator(`[data-agent-name="${sessionId}"]`) });
}

/** The chevron beside the Agents heading, which folds the rows under it; the heading itself
 *  stays the link to the Agents page. */
export function railAgentsFold(page: Page): Locator {
  return page.getByRole("button", { name: /^(Hide|Show) pinned agents$/ });
}

/** The agents the rail names, top to bottom: what the pin order is read from. */
export function railAgentIds(page: Page): Promise<string[]> {
  return railAgentRows(page)
    .locator("[data-agent-name]")
    .evaluateAll((names) => names.map((name) => name.getAttribute("data-agent-name") ?? ""));
}

/** Holds the rail to exactly these agents, in this order, waiting for the list to settle as any
 *  assertion does: a pin reaches the rail from the person's agent state, which a page that has
 *  just loaded, or another browser's write, is still reading. */
export async function expectRailAgents(page: Page, sessionIds: readonly string[]): Promise<void> {
  await expect(railAgentRows(page)).toHaveCount(sessionIds.length);
  await expect.poll(() => railAgentIds(page)).toEqual([...sessionIds]);
}

/** Holds `sessionId`'s rail row to the one word it says its live view is doing, and to that word
 *  alone: a row read for `live` with `toContainText` would pass on the `live 30s` of a view left
 *  lingering, so each of the three words is read exactly and the other two must be absent. */
export async function expectRailViewState(
  page: Page,
  sessionId: string,
  state: LiveViewState
): Promise<void> {
  const row = railAgentRow(page, sessionId);
  for (const word of VIEW_STATES) {
    await expect(row.getByText(word, { exact: true })).toHaveCount(word === state ? 1 : 0);
  }
}

/** Whether the rail is a sheet at the width this row runs at: below `COMPACT_VIEWPORT_WIDTH`
 *  (the shell's `COMPACT_VIEWPORT_QUERY`, `(max-width: 1279px)`, which `app.tsx` reads as
 *  `isCompactViewport`) the shell draws the Menu control and puts the rail in a sheet behind it,
 *  and from that width up the rail is the page's own column and there is no Menu control. It is
 *  read from the viewport the row set, never from the page: a page still painting its first
 *  frame shows neither the Menu control nor the sheet, so a read of the page could take the
 *  column's branch at a phone width. */
export function railIsSheet(page: Page): boolean {
  const width = page.viewportSize()?.width;
  if (width === undefined) {
    throw new Error("the rail helpers read the row's own viewport, and this page has none");
  }
  return width < COMPACT_VIEWPORT_WIDTH;
}

/** Opens the rail where it is a sheet behind the Menu control, and does nothing where it is the
 *  page's own column, once the page shows the control that width draws or the column it does
 *  not. A rail link closes the sheet behind it, so a row that uses the rail twice at a phone
 *  width opens it again. */
export async function openRail(page: Page): Promise<void> {
  const menu = page.getByRole("button", { name: "Open navigation" });
  if (!railIsSheet(page)) {
    await expect(menu).toBeHidden();
    return;
  }
  await expect(menu).toBeVisible();
  const sheet = railSheet(page);
  // Whether the sheet is open already is a read of a state the row's last rail step waited for:
  // this helper waits for the sheet to show, `closeRail` for it to go, and a rail link for it to
  // go behind the page it opens, so the read never meets the sheet half open or half closed.
  if (await sheet.isVisible()) return;
  await menu.click();
  await expect(sheet).toBeVisible();
}

/** Closes the rail where it is a sheet, so the page under it takes the next press; where the
 *  rail is the page's own column there is nothing to close. */
export async function closeRail(page: Page): Promise<void> {
  if (!railIsSheet(page)) return;
  const sheet = railSheet(page);
  // A sheet closed already takes no Escape, which would go to the page under it instead. The
  // read can be trusted for the reason `openRail`'s can.
  if (!(await sheet.isVisible())) return;
  await page.keyboard.press("Escape");
  await expect(sheet).toBeHidden();
}

/** Follows a link in the rail, and at a phone width waits for the sheet to go behind the page
 *  the link opens, so the row's next rail step starts from a closed sheet. */
async function followRailLink(page: Page, link: Locator): Promise<void> {
  await link.click();
  if (railIsSheet(page)) await expect(railSheet(page)).toBeHidden();
}

/** Opens `sessionId`'s live view the way the person does it: the rail, then the row's name. */
export async function openAgentFromRail(page: Page, sessionId: string): Promise<void> {
  await openRail(page);
  await followRailLink(page, railAgentRow(page, sessionId).getByRole("link"));
}

/** Leaves every live view for the Inbox, from the rail, as a reader does. */
export async function openInboxFromRail(page: Page): Promise<void> {
  await openRail(page);
  await followRailLink(
    page,
    page.getByRole("navigation", { name: "Navigation" }).getByRole("link", { name: "Inbox" })
  );
  await expect(page).toHaveURL(/\/$/);
}
