import { expect, type Locator, type Page } from "@playwright/test";

/** A Board strip picker or the List table's header filter. */
export function filterPicker(page: Page, picker: "Labels" | "Status"): Locator {
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
  picker: "Labels" | "Status",
  option: string
): Promise<void> {
  const trigger = filterPicker(page, picker);
  if ((await trigger.getAttribute("aria-expanded")) === "false") {
    await trigger.click();
  }
  await page
    .getByRole("option", {
      name: new RegExp(`^${option.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?:, \\d+ issues?)?$`),
    })
    .click();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("listbox")).toHaveCount(0);
}
