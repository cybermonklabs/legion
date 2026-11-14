import { expect, type Locator, type Page } from "@playwright/test";

import { CONTROL, type ControlName } from "../web/src/components/control-names";

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
  if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
  // `Filter` on the Inbox, the Agents page, the Delivery page and the search bar; `Filters` on
  // the project List and Board, where the trigger also counts what is on.
  const panel = page.getByRole("dialog", { name: /^Filters?$/ });
  await expect(panel).toHaveCount(1);
  return panel;
}

/**
 * The page's display menu, opened unless it is open already: the panel is the `dialog` named
 * after the trigger, and `label` is what that trigger reads (`View` on every page that draws
 * one today).
 */
export async function openDisplayMenu(page: Page, label = "View"): Promise<Locator> {
  const menu = page.getByRole("dialog", { name: label });
  if ((await menu.count()) === 0)
    await page.getByRole("button", { exact: true, name: label }).click();
  await expect(menu).toHaveCount(1);
  return menu;
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

/** A Board strip picker or the List table's header filter, by the name it carries. */
export function filterPicker(page: Page, picker: "Claimed by" | "Labels" | "Status"): Locator {
  return page
    .getByRole("button", {
      name: new RegExp(`^(?:${picker}( · \\d+)?|Filter ${picker}(?:: \\d+ picked)?)$`),
    })
    .and(page.locator('[aria-haspopup="listbox"]'));
}

/** Toggles one option in an expanded strip's `Labels` or `Status` multi-select, then closes the
 *  popover with Escape so it never covers the control the test drives next. */
export async function pickFilterOption(
  page: Page,
  picker: "Claimed by" | "Labels" | "Status",
  option: string
): Promise<void> {
  const trigger = filterPicker(page, picker);
  if ((await trigger.getAttribute("aria-expanded")) === "false") {
    await trigger.click();
  }
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
