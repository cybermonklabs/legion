import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL, type ControlName } from "../web/src/components/control-names";
import { expandedState, settledState } from "./settled";

/**
 * One of the shared controls, found by the `data-control` value it writes
 * (`web/src/components/control-names.ts`): the one way a row names a control that carries no
 * accessible name of its own, so no row spells an attribute of its own. `within` is whatever
 * holds the control - the page, a dialog, or the Inbox drawer's own row.
 */
export function control(within: Locator | Page, name: ControlName): Locator {
  return within.locator(`[data-control="${name}"]`);
}

/** The one row a surface's search, filtering and display controls sit in. */
export function controlRow(within: Locator | Page): Locator {
  return control(within, CONTROL.controlRow);
}

/**
 * Opens the row's one filter panel unless it is open already, and hands back the panel.
 * `AnchoredPanel` portals it to the body, so the panel is read off the page however deep the
 * row that opened it sits; `within` is the surface whose row holds the trigger - the page, or
 * the Inbox drawer.
 */
export async function openFilterPanel(page: Page, within: Locator | Page = page): Promise<Locator> {
  const trigger = control(controlRow(within), CONTROL.panelTrigger);
  if ((await expandedState("the row's filter trigger", trigger)) === "shut") await trigger.click();
  // `Filter` on the Inbox, the Agents page, the Delivery page and the search bar; `Filters` on
  // the project List, the Board and a priority's own page.
  const panel = page.getByRole("dialog", { name: /^Filters?$/ });
  await expect(panel).toHaveCount(1);
  return panel;
}

/** `button` where the control bar `bar` draws it in its line, else `item` among **More**'s items,
 *  which this opens. Waits until the button or **More** is drawn, so the bar has read its own
 *  width. */
async function buttonOrMoreItem(bar: Locator, button: Locator, item: Locator): Promise<Locator> {
  const more = control(bar, CONTROL.moreMenu);
  await expect(button.or(more).first()).toBeVisible();
  // The wait above saw the button or More, and the bar takes its layout from its own width
  // before it first paints, so whether the button is drawn is settled by the time either shows.
  if ((await button.count()) > 0) return button;
  if ((await expandedState("the bar's More", more)) === "shut") await more.click();
  return item;
}

/**
 * The control a page offers for the action `label`: on a page whose control bar holds it, the
 * bar's own button where the bar is 640 px wide or wider, else its item of **More**, which this
 * opens (R8, `components/ControlRow.tsx`); on a page that draws the action as a button of its
 * own, that button.
 */
export async function barAction(page: Page, label: string): Promise<Locator> {
  return buttonOrMoreItem(
    controlRow(page),
    page.getByRole("button", { exact: true, name: label }),
    page.getByRole("menuitem", { exact: true, name: label })
  );
}

/**
 * The trigger of the display menu `label` inside `within` - the page, or the Inbox drawer: the
 * bar's display menu where it stands in the bar's line, else its item of **More**, which this
 * opens. A bar that sets the narrow display rule, the Inbox's and its drawer's, holds the display
 * menu in **More** below 640 px of its width (R8, `components/ControlRow.tsx`).
 */
export async function displayTrigger(
  page: Page,
  label = "View",
  within: Locator | Page = page
): Promise<Locator> {
  const bar = controlRow(within);
  return buttonOrMoreItem(
    bar,
    control(bar, CONTROL.displayMenu).and(page.getByRole("button", { exact: true, name: label })),
    page.getByRole("menu", { name: "More" }).getByRole("menuitem", { exact: true, name: label })
  );
}

/** Shuts the bar's **More** inside `within` where it stands open, as a reader does by pressing
 *  it again: a display menu opened from it leaves it open under that menu. Whether it stands open
 *  is read, not whether it is drawn: a wide bar draws **More** only while it holds an action, as
 *  the Inbox's holds **Mark all read** while a row is unread, so it can come or go as the list
 *  loads, while only a press opens it. */
export async function shutMore(within: Locator | Page): Promise<void> {
  const bar = controlRow(within);
  const more = control(bar, CONTROL.moreMenu);
  const page = bar.page();
  const expanded = page.locator('[aria-expanded="true"]');
  const shown = await settledState("the bar's More", [
    ["open", more.and(expanded)],
    // `hasNot` reads its locator inside each bar, so the open **More** is named from the page.
    ["not open", bar.filter({ hasNot: control(page, CONTROL.moreMenu).and(expanded) })],
  ]);
  if (shown === "not open") return;
  await more.click();
  await expect(more).toHaveAttribute("aria-expanded", "false");
}

/**
 * The display menu of the bar inside `within` - the page, or the Inbox drawer - opened unless it
 * is open already: the panel is the `dialog` named after the trigger, and `label` is what that
 * trigger reads (`View` on every page that draws one today), in the bar's line or among
 * **More**'s items (`displayTrigger`).
 */
export async function openDisplayMenu(
  page: Page,
  label = "View",
  within: Locator | Page = page
): Promise<Locator> {
  const menu = page.getByRole("dialog", { name: label });
  const trigger = await displayTrigger(page, label, within);
  if ((await expandedState(`the ${label} menu`, trigger)) === "shut") await trigger.click();
  await expect(menu).toHaveCount(1);
  return menu;
}

/** Shuts the **View** menu of the bar inside `within` - the page, or the Inbox drawer - by
 *  pressing its trigger again, in the bar's line or among **More**'s items, and then **More**,
 *  which stays open under a menu opened from it. */
export async function shutDisplayMenu(page: Page, within: Locator | Page = page): Promise<void> {
  const menu = page.getByRole("dialog", { name: "View" });
  const trigger = control(controlRow(within), CONTROL.displayMenu).or(
    page.getByRole("menu", { name: "More" }).getByRole("menuitem", { exact: true, name: "View" })
  );
  // The menu stands open on its trigger, which is pressed again to shut it, unless the trigger
  // has left the page and taken the menu with it: so the menu's state is read once the page
  // settles on one of the two, rather than the trigger's count at one instant.
  const after = await settledState("the View menu", [
    ["open", trigger.and(page.locator('[aria-expanded="true"]'))],
    ["shut", page.locator("body").filter({ hasNot: menu })],
  ]);
  if (after === "open") await trigger.click();
  await expect(menu).toHaveCount(0);
  await shutMore(within);
}

/** One choice of one display-menu section, as the radio that carries it: the sections share
 *  value words, so each is read through its own fieldset. */
export function displayChoice(menu: Locator, section: string, value: string): Locator {
  return menu
    .getByRole("group", { name: section })
    .locator(`input[type="radio"][value="${value}"]`);
}

/** Takes one choice of one display-menu section. The menu stays open, as it does for a reader
 *  who sets the order and then the banding. */
export async function chooseDisplay(
  page: Page,
  section: string,
  value: string,
  label = "View"
): Promise<void> {
  await displayChoice(await openDisplayMenu(page, label), section, value).click();
}

/** Picks one choice of the Inbox's **View** menu's **Show** section from the trigger inside
 *  `within` - the page, or the Inbox drawer - and shuts the menu again, and **More** where the
 *  menu stood among its items. */
export async function chooseShow(
  page: Page,
  label: string,
  within: Locator | Page = page
): Promise<void> {
  const menu = await openDisplayMenu(page, "View", within);
  await menu.getByRole("group", { name: "Show" }).getByRole("radio", { name: label }).click();
  // The pick leaves the menu open on its trigger, which is pressed again to shut it.
  await shutDisplayMenu(page, within);
}

/** A Board strip picker or the List table's header filter, by the name it carries. */
export function filterPicker(page: Page, picker: "Claimed by" | "Labels" | "Status"): Locator {
  return page
    .getByRole("button", {
      name: new RegExp(`^(?:${picker}( · \\d+)?|Filter ${picker}(?:: \\d+ picked)?)$`),
    })
    .and(page.locator('[aria-haspopup="listbox"]'));
}

/** Toggles one option in an expanded strip's `Labels` or `Status` multi-select, then closes the
 *  popover with Escape so it never covers the control the test drives next. The trigger says
 *  whether the popover is open, and is read once the page has drawn it. */
export async function pickFilterOption(
  page: Page,
  picker: "Claimed by" | "Labels" | "Status",
  option: string
): Promise<void> {
  const trigger = filterPicker(page, picker);
  if ((await expandedState(`the ${picker} picker`, trigger)) === "shut") await trigger.click();
  await page
    .getByRole("option", {
      // "Done, not loaded" while closed issues are still out: its count would be a lie.
      name: new RegExp(
        `^${option.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?:, (?:\\d+ issues?|not loaded))?$`
      ),
    })
    .click();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
}

/** The control labelled `label` that opens a list: the list's own button. A native select, which
 *  is what several of these lists were before each held a text box, is found by the same label, so
 *  a page that still draws one fails on that list by name rather than on a trigger not found. A
 *  label finds a trigger named by it alone or by it and then what the trigger shows, as a single
 *  pick's is (`MultiSelect`); a pattern finds a trigger whose name it matches. */
export function listTrigger(scope: Locator | Page, label: string | RegExp): Locator {
  const name =
    typeof label === "string"
      ? new RegExp(`^${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?: |$)`)
      : label;
  return scope.getByRole("button", { name }).or(scope.getByRole("combobox", { name }));
}

/** The panel a list's trigger names in `aria-controls` while the list stands open: the list is
 *  portalled out of its host, so it is found from its own trigger and from nothing page-wide, as
 *  `issueListOf` (`e2e/agents.ts`) finds the composer's. */
export function listOf(trigger: Locator): Locator {
  return trigger.locator("xpath=id(./@aria-controls)");
}

/** Picks the option named `option` from the list `trigger` opens, as a reader does: a press of
 *  the trigger, then of the option in the list's own panel (`listOf`). */
export async function pickFromList(trigger: Locator, option: string | RegExp): Promise<void> {
  await trigger.click();
  await listOf(trigger).getByRole("option", { exact: true, name: option }).click();
}
