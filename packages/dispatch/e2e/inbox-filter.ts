import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL } from "../web/src/components/control-names";
import { control, controlRow, openFilterPanel } from "./filters";
import { expandedState } from "./settled";

/**
 * The Inbox's `?assignee=` control (the Assignee field `features/inbox/Inbox.tsx` draws), one of
 * the five fields the bar's **Filter** button opens (INFRA-450; before it, the control stood in the
 * bar itself).
 * Driving it, rather than opening a filtered address, is what a test needs when the Inbox must
 * stay mounted across the change: a pick is a client-side `replace`, so the marks, drafts and
 * held responses the page is carrying survive it, where `page.goto` would lose them.
 */

/** The filter's trigger, whatever its selection reads (`Assignee · you`, `· everyone`, `· 2`);
 *  absent until the bar's **Filter** panel is open. */
export function assigneeFilter(page: Page): Locator {
  return page.getByRole("button", { name: "Filter by assignee" });
}

/** The popover's rows; absent while it is shut. */
export function assigneeOptions(page: Page): Locator {
  return page.getByRole("listbox", { name: "Assignee options" });
}

/** Opens the row's five filter fields unless they are open already, and hands back the panel
 *  the row's one **Filter** trigger opens. */
export async function openFilterFields(page: Page): Promise<Locator> {
  return openFilterPanel(page);
}

/** How many rows the **Type** filter counts for each of `names`, in their order, read off each
 *  option's own accessible name, with the panel and its Type list opened once for all of them.
 *  `within` is the surface whose row opens the panel - the page itself, or the drawer. The fields
 *  themselves are portalled to the body, so they are read off the page rather than through
 *  `within`. Before this answers, the popover and then the panel are shut again, one Escape each,
 *  as `closeAssigneeFilter` shuts them. The panel can hang over the list's rows, so the next press
 *  or read of the list is not made through it. */
export async function typeFilterCounts(
  page: Page,
  names: readonly string[],
  within: Locator | Page = page
): Promise<number[]> {
  const panel = await openFilterPanel(page, within);
  const trigger = page.getByRole("button", { name: "Filter by type" });
  if ((await expandedState("the Type filter", trigger)) === "shut") await trigger.click();
  const options = page.getByRole("listbox", { name: "Type options" });
  const counts: number[] = [];
  for (const name of names) {
    const label =
      (await options
        .getByRole("option", { name: new RegExp(`^${name},`) })
        .getAttribute("aria-label")) ?? "";
    counts.push(Number(label.replace(`${name}, `, "").replace(" rows", "")));
  }
  await page.keyboard.press("Escape");
  await expect(options).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(panel).toHaveCount(0);
  return counts;
}

/** Opens the popover unless it is open already; the trigger says which it is. */
export async function openAssigneeFilter(page: Page): Promise<void> {
  await openFilterFields(page);
  const trigger = assigneeFilter(page);
  if ((await expandedState("the assignee filter", trigger)) === "shut") {
    await trigger.click();
  }
  await expect(assigneeOptions(page)).toHaveCount(1);
}

/** Ticks one of the popover's rows - a login, or `Unassigned` - and leaves the popover open, as
 *  the control does. A row's accessible name carries its detail (`alice, you`), so `option` is
 *  matched as a substring of it. */
export async function pickAssignee(page: Page, option: string): Promise<void> {
  await openAssigneeFilter(page);
  await assigneeOptions(page).getByRole("option", { name: option }).click();
}

/** The popover's own `Clear assignee`: the cleared filter, every question. */
export async function clearAssigneeFilter(page: Page): Promise<void> {
  await openAssigneeFilter(page);
  await page.getByRole("button", { name: "Clear assignee" }).click();
}

/** The Inbox's own search box, beside the assignee filter in the header: the `?q=` the list
 *  narrows itself by (`features/inbox/inbox-search.ts`), which `f` also puts the focus in. */
export function inboxSearchBox(page: Page): Locator {
  return page.getByRole("searchbox", { name: "Search asks" });
}

/** Types `text` into that box, as a reader does: the whole field, replacing whatever it held. */
export async function searchInbox(page: Page, text: string): Promise<void> {
  await inboxSearchBox(page).fill(text);
}

/** Closes the popover the way a reader does, and the panel it is drawn in with it, waiting
 *  until both are gone so whatever they covered is reachable again. Escape is handled by the
 *  innermost layer the press comes from: the popover first, then the panel. */
export async function closeAssigneeFilter(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(assigneeOptions(page)).toHaveCount(0);
  const panel = page.getByRole("dialog", { name: "Filter" });
  const panelTrigger = control(controlRow(page), CONTROL.panelTrigger);
  if ((await expandedState("the row's filter trigger", panelTrigger)) === "open") {
    await page.keyboard.press("Escape");
    await expect(panel).toHaveCount(0);
  }
}
