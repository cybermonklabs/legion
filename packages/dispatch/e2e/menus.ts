/** The menus a menu button opens - the Snooze, the priority and the status (dispatch://INFRA-2160)
 *  - as the rows reach them. The trigger is a button named for its menu ("Snooze <name>",
 *  "Snooze selected asks", "Priority of <KEY>", "Status") and then for what it shows ("Priority
 *  of <KEY> P1", "Status In progress"), unless what it shows is the first words of that name
 *  already ("Snooze", an unset "Priority"), so it is found by the start of its name
 *  (`triggerNamed`). The menu is portalled to the body, so it is found by the id its trigger's
 *  `aria-controls` names while it is open, never under the trigger. A Snooze choice is an action
 *  (`menuitem`); a priority or a status is a value (`menuitemradio`, the current one checked).
 *  Nothing here reads the app's own modules. */
import { expect, type Locator, type Page } from "@playwright/test";

/** The name of the trigger of the menu named `label`: `label` alone, or `label` and then what
 *  the trigger shows, as a single pick's trigger is found (`listTrigger`). */
function triggerNamed(label: string): RegExp {
  return new RegExp(`^${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?: |$)`);
}

/** The bar of selected rows' Snooze: the trigger of the menu that snoozes every ticked ask. */
export function bulkSnoozeTrigger(page: Page): Locator {
  return page
    .getByRole("toolbar", { name: "Selected rows" })
    .getByRole("button", { exact: true, name: "Snooze selected asks" });
}

/** The bulk Snooze's menu, open: what that trigger, or `b` or `h` with rows ticked, opens. */
export function bulkSnoozeMenu(page: Page): Locator {
  return page.getByRole("menu", { name: "Snooze selected asks" });
}

/** The issue header's Status: the trigger of the status menu, which shows the status it holds. */
export function statusTrigger(page: Page): Locator {
  return page.getByTestId("issue-header").getByRole("button", { name: triggerNamed("Status") });
}

/** The trigger of the priority menu of the issue `key`, inside `scope`. */
export function priorityTrigger(scope: Locator | Page, key: string): Locator {
  return scope.getByRole("button", { name: triggerNamed(`Priority of ${key}`) });
}

/** The menu `trigger` holds open: the element its `aria-controls` names. */
export async function menuOf(trigger: Locator): Promise<Locator> {
  await expect(trigger, "the menu button holds its menu open").toHaveAttribute(
    "aria-expanded",
    "true"
  );
  // The wait above settled the open menu, so the id its trigger names now is the menu's own.
  const id = await trigger.getAttribute("aria-controls");
  if (id === null) throw new Error("the open menu button names no menu in aria-controls");
  return trigger.page().locator(`[id="${id}"]`);
}

/** The choice of `menu` named `name`, an action or a value. */
export function choiceOf(menu: Locator, name: string): Locator {
  return menu
    .getByRole("menuitem", { exact: true, name })
    .or(menu.getByRole("menuitemradio", { exact: true, name }));
}

/** A pointer's pick: a click on `trigger` opens its menu, and a click on the choice `name`
 *  writes it. */
export async function pickFromMenu(trigger: Locator, name: string): Promise<void> {
  await trigger.click();
  await choiceOf(await menuOf(trigger), name).click();
}
