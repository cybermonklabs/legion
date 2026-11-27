/** The issue header's status and the priority as the rows reach them, and the one list a key or a
 *  press holds open on the page. Snooze, the priority and the status are each the shared single
 *  pick (`MultiSelect`) with dispatch://INFRA-2160's rules for a pick in a menu's place (its D5):
 *  a trigger, which is a button named for its control ("Priority of <KEY>", "Status", "Snooze
 *  <item>") and then for what it shows ("Priority of <KEY> P1", "Status In progress"), unless what
 *  it shows is the first words of that name already (an unset "Priority"), so it is found by the
 *  start of its name (`triggerNamed`). Its list is portalled to the body, so it is found by the id
 *  its trigger's `aria-controls` names while it is open (`listOf` in `filters.ts`), never under the
 *  trigger: a text box of role `combobox` over a `listbox` of `option`s. Each option is named by
 *  its words alone: the digit a choice shows is hidden from assistive tech, which reads it as the
 *  option's `aria-keyshortcuts`. The choice the list highlights is the one the text box names in
 *  `aria-activedescendant`. Nothing here reads the app's own modules. */
import { expect, type Locator, type Page } from "@playwright/test";

import { listOf } from "./filters";

/** The name of the trigger of the control named `label`: `label` alone, or `label` and then what
 *  the trigger shows, as `listTrigger` finds a single pick's trigger. */
function triggerNamed(label: string): RegExp {
  return new RegExp(`^${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?: |$)`);
}

/** The issue header's Status: the trigger of the status's list, which shows the status it
 *  holds. */
export function statusTrigger(page: Page): Locator {
  return page.getByTestId("issue-header").getByRole("button", { name: triggerNamed("Status") });
}

/** The trigger of the priority pick of the issue `key`, inside `scope`: the button of the shared
 *  single pick (`MultiSelect`, `aria-haspopup="listbox"`), named `Priority of <KEY>` and then the
 *  priority it shows. A press opens its list (`listOf` in `filters.ts`). */
export function priorityTrigger(scope: Locator | Page, key: string): Locator {
  return scope.getByRole("button", { name: triggerNamed(`Priority of ${key}`) });
}

/** The trigger whose list stands open on the page, whichever key or press opened it: the list
 *  button (`aria-haspopup="listbox"`) that is `aria-expanded`. A page holds one list open at a
 *  time, so a row that opens a list with a key finds it here. */
export function openTrigger(page: Page): Locator {
  return page.locator('[aria-haspopup="listbox"][aria-expanded="true"]');
}

/** The panel of the list open on the page (`openTrigger`'s, as `listOf` finds it): its text box,
 *  its options and, for a pick in a menu's place, its foot line of keys. It matches nothing while
 *  no list is open. */
export function openList(page: Page): Locator {
  return listOf(openTrigger(page));
}

/** Fails by `message` unless the open list `list` highlights its option `name`: the list's text
 *  box names that option in `aria-activedescendant`. The box names it whether or not it holds the
 *  focus, so this reads a list a tap opened on a phone, with no box focused, as well. */
export async function expectHighlight(list: Locator, name: string, message: string): Promise<void> {
  const choice = list.getByRole("option", { exact: true, name });
  await expect(choice, `${message}: the list holds ${name}`).toHaveCount(1);
  // The wait above found the option, whose id is its value's and stays while the list is open.
  const id = await choice.getAttribute("id");
  await expect(list.getByRole("combobox"), message).toHaveAttribute(
    "aria-activedescendant",
    id ?? ""
  );
}

/** Fails by `message` unless `trigger` holds its list open on the option `name`, highlighted,
 *  with the focus where a key or a press leaves it as a pick in a menu's place opens: in the
 *  list's text box on a fine pointer, and on a coarse pointer (a phone) in the list itself, so
 *  that no on-screen keyboard covers the choices. Such a list opens on its first choice (a
 *  snooze) or on the value it holds (a priority, a status). */
export async function expectOpenOn(trigger: Locator, name: string, message: string): Promise<void> {
  const list = listOf(trigger);
  // The pointer kind is the project's fixed device (`iphone` is coarse, the desktop projects are
  // fine), so one read gives the answer for the whole row and nothing on the page can change it.
  const coarse = await trigger
    .page()
    .evaluate(() => window.matchMedia("(pointer: coarse)").matches);
  await expect(
    coarse ? list.getByRole("listbox") : list.getByRole("combobox"),
    `${message}: the focus is in the list${coarse ? "" : "'s text box"}`
  ).toBeFocused();
  await expectHighlight(list, name, `${message}: the list highlights ${name}`);
}
