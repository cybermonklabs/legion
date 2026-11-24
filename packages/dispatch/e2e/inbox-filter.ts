import { expect, type Locator, type Page } from "@playwright/test";

import { expandedState } from "./settled";

/**
 * The Inbox's `?assignee=` control (`features/inbox/AssigneeFilter.tsx`), one of the five fields
 * the bar's **Filter** button opens (INFRA-450; before it, the control stood in the bar itself).
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

/** Opens the five filter fields of the bar `within` holds - the page's or the drawer's - unless
 *  they are open already; the bar's **Filter** button says which. */
async function showFilterFields(within: Page | Locator): Promise<void> {
  const filter = within.locator("[data-inbox-filter]");
  if ((await expandedState("the Inbox's Filter button", filter)) === "shut") await filter.click();
}

/** Opens the bar's five filter fields unless they are open already, and hands back the panel. */
export async function openFilterFields(page: Page): Promise<Locator> {
  const fields = page.locator("[data-inbox-filter-fields]");
  await showFilterFields(page);
  await expect(fields).toHaveCount(1);
  return fields;
}

/** How many rows the **Type** filter counts for one of its values, read off that option's own
 *  accessible name. `within` is the bar the panel belongs to - the page's or the drawer's - and
 *  the popover is shut again before this answers, so the next read of the list is not made
 *  through it. */
export async function typeFilterCount(page: Page, within: Locator, name: string): Promise<number> {
  await showFilterFields(within);
  const trigger = within.getByRole("button", { name: "Filter by type" });
  if ((await expandedState("the Type filter", trigger)) === "shut") await trigger.click();
  const options = page.getByRole("listbox", { name: "Type options" });
  const label =
    (await options
      .getByRole("option", { name: new RegExp(`^${name},`) })
      .getAttribute("aria-label")) ?? "";
  await page.keyboard.press("Escape");
  return Number(label.replace(`${name}, `, "").replace(" rows", ""));
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

/** Closes the popover the way a reader does, and waits until it is gone, so whatever it covered
 *  is reachable again. Escape is handled by the search box, which holds focus while the popover
 *  is open (a row's press never takes it). */
export async function closeAssigneeFilter(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(assigneeOptions(page)).toHaveCount(0);
}
