/**
 * A press that starts a write, and the checks that a control the write holds
 * (`components/held-press.ts`) is held as it should be: by `aria-disabled`, never `disabled`. A
 * disabled control drops the focus to the page's body in a browser, and does here under
 * `emulateChromiumFocusFixup`, which is on for the press.
 */

import { expect } from "bun:test";
import { act, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

import { emulateChromiumFocusFixup } from "../features/issue-page/chromium-focus-fixup";

/** Focuses `start`, `pressed` unless it is given, and presses it by `press`, Enter by default.
 *  Once the write is out - `sent` counts one - `pressed`, focused if `start` was another control,
 *  holds the focus and is `aria-disabled`. A second press of it by `press`, and by the pointer
 *  where it takes no typed text, sends nothing more, and the focus stays on it. `name` names
 *  `pressed` in what a failed check prints. The write is left out, for the case to answer. */
export async function pressHeld(
  user: UserEvent,
  pressed: HTMLElement,
  {
    name,
    press = "{Enter}",
    sent,
    start = pressed,
  }: { name: string; press?: string; sent: () => number; start?: HTMLElement }
): Promise<void> {
  // What holds the focus, as a string a failed check prints whole.
  const focused = () =>
    document.activeElement === pressed ? name : (document.activeElement?.tagName ?? "nothing");
  const takesText =
    pressed.matches("textarea, input") && !pressed.matches('[type="checkbox"], [type="radio"]');
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    act(() => start.focus());
    await user.keyboard(press);
    await waitFor(() => expect(sent()).toBe(1));
    if (start !== pressed) act(() => pressed.focus());
    expect(focused()).toBe(name);
    expect(pressed.getAttribute("aria-disabled")).toBe("true");

    await user.keyboard(press);
    if (!takesText) await user.click(pressed);
    expect(sent()).toBe(1);
    expect(focused()).toBe(name);
  } finally {
    stopFocusFixup();
  }
}

/** Whether `control` is held: `aria-disabled`. It throws where `control` is `disabled`, which
 *  takes the focus off the control the reader pressed; `name` names it in that error. */
export function heldNotDisabled(
  control: Element & { readonly disabled: boolean },
  name: string
): boolean {
  if (control.disabled) throw new Error(`${name} is disabled, not held`);
  return control.getAttribute("aria-disabled") === "true";
}
