// The sidebar's Agents list (INFRA-812, R1), as a browser check reads it: the rows are the
// person's pinned agents, oldest pin first, and below 1280 pixels the whole rail is a sheet
// behind the Menu control rather than a column of the page. Every row that reads the rail goes
// through here, so one place knows where the rail is at the width it is run at. The helpers that
// open the rail, close it or ask whether it is a sheet first settle on the layout the shell drew
// (`e2e/settled.ts`), because a page still reading the person's sign-in shows no rail at all.
import { expect, type Locator, type Page } from "@playwright/test";

import type { LiveViewState } from "../web/src/features/agent-view/live-views";
import { settledState } from "./settled";

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

/** The Menu control that opens the sheet. It stays visible under the open sheet. */
function railMenu(page: Page): Locator {
  return page.getByRole("button", { name: "Open navigation" });
}

/** The layout the shell drew the rail in, read once it has drawn one: the sheet open, the sheet
 *  closed behind the Menu control, or the page's own column. The open sheet is read first,
 *  since the Menu control still shows under it. The skeleton drawn while the person's sign-in is
 *  read shows none of the three, so it is waited out; a page that still shows none when the
 *  expect timeout ends, such as a wide page whose sidebar the person collapsed, fails here with a
 *  message naming the three. */
function railLayout(page: Page): Promise<"open sheet" | "closed sheet" | "column"> {
  return settledState("the rail", [
    ["open sheet", railSheet(page)],
    ["closed sheet", railMenu(page)],
    ["column", page.getByRole("complementary", { name: "Navigation" })],
  ]);
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

/** Opens the rail where it is a sheet closed behind the Menu control, and does nothing where it
 *  is an open sheet or the page's own column. It settles on the rail's layout first, so a page
 *  still loading is waited for rather than read as one with no Menu control. A rail link closes
 *  the sheet behind it, so a row that uses the rail twice at a phone width opens it again. */
export async function openRail(page: Page): Promise<void> {
  if ((await railLayout(page)) !== "closed sheet") return;
  await railMenu(page).click();
  await expect(railSheet(page)).toBeVisible();
}

/** Whether the rail is the phone's sheet, open or closed, rather than the page's column. It
 *  settles on the rail's layout first, so a page still loading is not read as a column. */
export async function railIsSheet(page: Page): Promise<boolean> {
  const layout = await railLayout(page);
  return layout === "open sheet" || layout === "closed sheet";
}

/** Closes the rail where it is an open sheet, so the page under it takes the next press; where
 *  the rail is a closed sheet or the page's own column there is nothing to close. It settles on
 *  the rail's layout first, so a sheet the page has not drawn yet is not read as closed. */
export async function closeRail(page: Page): Promise<void> {
  if ((await railLayout(page)) !== "open sheet") return;
  await page.keyboard.press("Escape");
  await expect(railSheet(page)).toBeHidden();
}

/** Opens `sessionId`'s live view the way the person does it: the rail, then the row's name. */
export async function openAgentFromRail(page: Page, sessionId: string): Promise<void> {
  await openRail(page);
  await railAgentRow(page, sessionId).getByRole("link").click();
}

/** Leaves every live view for the Inbox, from the rail, as a reader does. */
export async function openInboxFromRail(page: Page): Promise<void> {
  await openRail(page);
  await page
    .getByRole("navigation", { name: "Navigation" })
    .getByRole("link", { name: "Inbox" })
    .click();
  await expect(page).toHaveURL(/\/$/);
}
