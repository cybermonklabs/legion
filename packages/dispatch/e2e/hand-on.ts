import { expect, type Locator } from "@playwright/test";

/**
 * Hands the ask the control belongs to over to `login`, as a reader does: the people it offers
 * are read only once the reader reaches for it, so the control is focused first and the option
 * waited for. Naming the assignee of the ask's own issue is how a reader hands it back.
 */
export async function handTo(control: Locator, login: string): Promise<void> {
  await control.focus();
  await expect(control.locator(`option[value="${login}"]`)).toHaveCount(1);
  await control.selectOption(login);
}
