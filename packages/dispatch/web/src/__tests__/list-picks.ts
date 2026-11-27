import { screen, within } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";
import type { KeyboardEvent as ReactKeyboardEvent } from "react";

/** The text boxes in the page now: a list's own box is one that appears when the list opens. */
export function textBoxes(): Element[] {
  return [
    ...document.querySelectorAll('input[type="search"], input[type="text"], input:not([type])'),
  ];
}

/** The element `control` names in `aria-controls`, else a throw saying `missing`. */
export function controlledBy(control: Element, missing: string): HTMLElement {
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

/** An option's text, less the digit a choice of a single pick in a menu's place draws beside its
 *  words for the eye (`kbd`, `MultiSelect`'s `menu`): the tick of the picked one, then its label. */
function optionText(option: Element): string {
  const copy = option.cloneNode(true) as Element;
  for (const digit of copy.querySelectorAll("kbd")) digit.remove();
  return copy.textContent ?? "";
}

/** The row a list's text box `box` highlights (`aria-activedescendant`), as its text without the
 *  tick of the picked one; undefined while the box highlights none, or there is no box. */
export function highlightedRow(box: Element | null): string | undefined {
  const id = box?.getAttribute("aria-activedescendant");
  const row = document.getElementById(id ?? "");
  return row === null ? undefined : optionText(row).replace("✓", "");
}

/** The panel of the list `trigger` opened: the list is portalled to the body, outside the
 *  host's own container, so it is found through the panel its trigger names (`aria-controls`). */
export function panelOf(trigger: Element): HTMLElement {
  return controlledBy(trigger, "the trigger names no open list");
}

/** The option of the open list `trigger` opened that `name` names: only that list's options
 *  count, never a control of the page behind it or of another list. */
export function optionOf(trigger: HTMLElement, name: string): HTMLElement {
  return within(panelOf(trigger)).getByRole("option", { name });
}

/** The names of the options `trigger`'s list offers, in order - each option's words, without the
 *  mark of the picked one or the digit drawn beside it - read with the list opened by a press on
 *  the trigger and closed again by Escape. */
export async function optionsOffered(user: UserEvent, trigger: HTMLElement): Promise<string[]> {
  await user.click(trigger);
  const names = [...panelOf(trigger).querySelectorAll('[role="option"]')].map(
    (option) => option.querySelector(":scope > span:not([aria-hidden])")?.textContent ?? ""
  );
  await user.keyboard("{Escape}");
  return names;
}

/** Picks `name` from the list `trigger` opens, as a reader's two presses do: one on the trigger,
 *  one on the option. */
export async function pickFromList(
  user: UserEvent,
  trigger: HTMLElement,
  name: string
): Promise<void> {
  await user.click(trigger);
  await user.click(optionOf(trigger, name));
}

/** Whether a list is open anywhere on the page. */
export function listOpen(): boolean {
  return screen.queryByRole("listbox") !== null;
}

/** The name of the single pick or the menu labelled `label`: the label alone where its trigger
 *  shows the label or the label's first words, else the label and then what the trigger shows
 *  (`MultiSelect`, `ChoiceMenu`). */
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
  return within(list).getAllByRole("option").map(optionText);
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
  await pickFromList(user, await screen.findByLabelText(label), name);
}

/** A row's `key`, which presses the list's trigger `selector` finds as the page's binding does: a
 *  click with no pointer, with the focus still on the row. */
export function pressesTriggerOn(
  key: string,
  selector: string
): (event: ReactKeyboardEvent<HTMLElement>) => void {
  return (event) => {
    if (event.key !== key) return;
    event.preventDefault();
    document.querySelector<HTMLElement>(selector)?.click();
  };
}

/** Makes the reader's primary pointer a coarse one, a finger on a phone's screen, where a list
 *  opens with no text box focused, until what it answers is called. */
export function onCoarsePointer(): () => void {
  const original = window.matchMedia;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query === "(pointer: coarse)",
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  return () => {
    window.matchMedia = original;
  };
}
