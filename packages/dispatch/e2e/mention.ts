import type { Page } from "@playwright/test";

/** Types `text` into an issue's comment composer and picks the session `option` names from the
 *  mention list the trailing `@` opens, as a person mentions one after `/btw @` or `/aside @`. */
export async function selectMention(page: Page, text: string, option: string): Promise<void> {
  const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
  await field.fill(text);
  await page
    .getByRole("listbox", { name: "Mention suggestions" })
    .getByRole("option", { name: option })
    .click();
}
