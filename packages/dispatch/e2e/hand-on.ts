import { expect, type Locator } from "@playwright/test";

/**
 * Hands the ask the control belongs to over to `login`, as a reader does: opens the list and
 * presses that person, whose option the list holds once the people are read - they are read
 * only once the reader reaches for the control. Naming the assignee of the ask's own issue is
 * how a reader hands it back, and that option carries their role after the login. A pick of
 * the person the ask already waits on writes nothing, so the option is pressed only once the
 * list no longer marks that person as picked: a card still to read the last hand-on is waited
 * for rather than pressed.
 */
export async function handTo(control: Locator, login: string): Promise<void> {
  await control.click();
  const person = control
    .page()
    .getByRole("listbox", { name: "Hand this question to options" })
    .getByRole("option", { name: new RegExp(`^${login}(?: \\(the issue's assignee\\))?$`) });
  await expect(person).toHaveAttribute("aria-selected", "false");
  await person.click();
}
