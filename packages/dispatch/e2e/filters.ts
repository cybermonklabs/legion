import { expect, type Locator, type Page } from "@playwright/test";

import { expandedState } from "./settled";

/** A Board strip picker or the List table's header filter. */
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
