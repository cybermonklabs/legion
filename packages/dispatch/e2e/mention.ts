import type { Page } from "@playwright/test";

import { pickReference } from "./ref-picker";

/** Types `text` into an issue's comment composer and picks the session `option` names from the
 *  reference picker the trailing `@` opens, as a person mentions one after `/btw @` or
 *  `/aside @`. */
export async function selectMention(page: Page, text: string, option: string): Promise<void> {
  const field = page.getByRole("form", { name: "Comment composer" }).getByLabel("Comment");
  await pickReference(field, text, option);
}
