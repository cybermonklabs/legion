/**
 * What a browser engine does where happy-dom does otherwise: the focus a removal drops, a key
 * pressed with Meta as macOS sends it, and a pointer press a hand holds. The hand-off rows of the
 * project Board and List (`IssueBoard.test.tsx`, `IssuesTable.test.tsx`) act through them.
 */

import { spyOn } from "bun:test";
import { act } from "@testing-library/react";

/** Makes the test's document drop the focus from a control taken out of the page as Chromium and
 *  WebKit do, with a `blur` and a `focusout` on it as it goes. happy-dom, as Firefox does, sends
 *  neither. Answers what gives happy-dom its own removal back. */
export function blurOnRemoval(): () => void {
  const remove = Node.prototype.removeChild;
  const removal = spyOn(Node.prototype, "removeChild").mockImplementation(function <T extends Node>(
    this: Node,
    child: T
  ): T {
    const focused = document.activeElement;
    if (focused instanceof HTMLElement && child.contains(focused)) focused.blur();
    return remove.call(this, child) as T;
  });
  return () => removal.mockRestore();
}

/** A key pressed with Meta as macOS sends one, ⌘C say: Meta's keydown and the key's, on `target`,
 *  and no keyup, which Chromium and WebKit do not send for a key released while Meta is held. */
export function metaKeyDown(target: Element, key: string, code: string): void {
  act(() => {
    target.dispatchEvent(
      new KeyboardEvent("keydown", { bubbles: true, code: "MetaLeft", key: "Meta", metaKey: true })
    );
    target.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, code, key, metaKey: true }));
  });
}

/** A pointer press on `target` as an engine sends one a hand holds for 100 ms: its `pointerdown`
 *  and the focus it moves, onto `focus`, or off what holds it onto the body where `focus` is null;
 *  then its `pointerup` and its `click`, or its `pointercancel` and no click. */
export async function pressFor100ms(
  target: Element,
  focus: HTMLElement | null,
  end: "pointercancel" | "pointerup"
): Promise<void> {
  act(() => {
    target.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    const focused = document.activeElement;
    if (focus !== null) focus.focus();
    else if (focused instanceof HTMLElement) focused.blur();
  });
  await act(() => Bun.sleep(100));
  act(() => {
    target.dispatchEvent(new PointerEvent(end, { bubbles: true }));
    if (end === "pointerup" && target instanceof HTMLElement) target.click();
  });
}
