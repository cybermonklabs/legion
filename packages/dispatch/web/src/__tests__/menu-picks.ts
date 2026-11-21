import { screen, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { controlledBy } from "./list-picks";

/** The open menu `trigger` names in `aria-controls`: only its choices count, never a control of
 *  the page behind it or of another menu. */
function menuOf(trigger: HTMLElement): HTMLElement {
  return controlledBy(trigger, "the trigger names no open menu");
}

/** The choice of `trigger`'s open menu that `name` names: an action (`menuitem`) or a value
 *  (`menuitemradio`). */
export function choiceOf(trigger: HTMLElement, name: string): HTMLElement {
  const menu = within(menuOf(trigger));
  return menu.queryByRole("menuitemradio", { name }) ?? menu.getByRole("menuitem", { name });
}

/** The names of the choices `trigger`'s menu offers, in order, read with the menu opened by a
 *  press on the trigger and closed again by Escape. */
export async function choicesOf(user: UserEvent, trigger: HTMLElement): Promise<string[]> {
  await user.click(trigger);
  const names = [
    ...menuOf(trigger).querySelectorAll('[role="menuitem"], [role="menuitemradio"]'),
  ].map((choice) => choice.querySelector(":scope > span:not([aria-hidden])")?.textContent ?? "");
  await user.keyboard("{Escape}");
  return names;
}

/** Picks `name` from the menu `trigger` opens, as a reader's two presses do: one on the
 *  trigger, one on the choice (`ChoiceMenu`: the snooze, the priority, the status). */
export async function pickFromMenu(
  user: UserEvent,
  trigger: HTMLElement,
  name: string
): Promise<void> {
  await user.click(trigger);
  await user.click(choiceOf(trigger, name));
}

/** Whether a menu is open anywhere on the page. */
export function menuOpen(): boolean {
  return screen.queryByRole("menu") !== null;
}
