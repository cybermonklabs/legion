import { screen, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

/** The text boxes in the page now: a list's own box is one that appears when the list opens. */
export function textBoxes(): Element[] {
  return [
    ...document.querySelectorAll('input[type="search"], input[type="text"], input:not([type])'),
  ];
}

/** The element `control` names in `aria-controls`, else a throw saying `missing`. */
function controlledBy(control: Element, missing: string): HTMLElement {
  const id = control.getAttribute("aria-controls");
  const element = id === null ? null : document.getElementById(id);
  if (element === null) throw new Error(missing);
  return element;
}

/** The list a text box narrows: the element it names in `aria-controls`. Only that list's
 *  options count, never a control of the page behind it or of another list. */
export function listOf(box: Element): HTMLElement {
  return controlledBy(box, "the text box names no list");
}

/** The panel of the list `trigger` opened: the list is portalled to the body, outside the
 *  host's own container, so it is found through the panel its trigger names (`aria-controls`). */
export function panelOf(trigger: Element): HTMLElement {
  return controlledBy(trigger, "the trigger names no open list");
}

/** The name of the single pick labelled `label`: the label alone where its trigger shows the
 *  label, else the label and then what the trigger shows (`MultiSelect`). */
export function pickNamed(label: string): RegExp {
  return new RegExp(`^${label.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(?: |$)`);
}

/** Opens the single pick labelled `label` from its trigger and hands back its listbox, `<label>
 *  options`. The list is portalled to the body, outside the control's own container. */
export async function openList(user: UserEvent, label: string): Promise<HTMLElement> {
  await user.click(screen.getByRole("button", { name: pickNamed(label) }));
  return screen.findByRole("listbox", { name: `${label} options` });
}

/** The open list's rows, each as its whole text: the tick of the picked one, then its label. */
export function rows(list: HTMLElement): string[] {
  return within(list)
    .getAllByRole("option")
    .map((option) => option.textContent ?? "");
}

/** The open list's rows, each by the name it is read out as: its `aria-label` where it has one,
 *  else its whole text, as a row with no detail beside it is named by its label alone (a picked
 *  row's tick is in that text too). */
export function optionNames(list: HTMLElement): string[] {
  return within(list)
    .getAllByRole("option")
    .map((option) => option.getAttribute("aria-label") ?? option.textContent ?? "");
}

/** Opens the single pick labelled `label` and picks the row named `name` once it is listed. */
export async function pick(user: UserEvent, label: string, name: string): Promise<void> {
  const list = await openList(user, label);
  await user.click(await within(list).findByRole("option", { name }));
}

/** Picks the row named `name` from the list whose trigger is labelled `label`. The trigger is
 *  awaited, since the form that holds it may not be drawn yet (a host may draw it only once its
 *  own reads answer), and the row is found in the panel the trigger names, since the list is
 *  portalled out of the host's container. */
export async function pickFromLabelled(
  user: UserEvent,
  label: string,
  name: string
): Promise<void> {
  const trigger = await screen.findByLabelText(label);
  await user.click(trigger);
  await user.click(within(panelOf(trigger)).getByRole("option", { name }));
}
