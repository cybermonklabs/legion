import { expect, test } from "bun:test";
import { renderHook } from "@testing-library/react";

import { usePanelDismiss } from "./usePanelDismiss";

// The Escape that ends an IME composition is the composition's, marked `isComposing`, or sent
// unmarked with keyCode 229 after the composition's end, as Safari sends it (WebKit bug 165004):
// the open panel stays open, the focus stays where it is, and the key goes on to the view behind.
// A plain Escape after the composition closes the panel and puts the focus back on its trigger.
test("an Escape that ends an IME composition, marked or sent with keyCode 229, leaves the open panel open", () => {
  const trigger = document.createElement("button");
  const panel = document.createElement("div");
  const field = document.createElement("input");
  panel.append(field);
  document.body.append(trigger, panel);
  const changes: boolean[] = [];
  const behind: KeyboardEvent[] = [];
  const hearBehind = (event: KeyboardEvent) => behind.push(event);
  window.addEventListener("keydown", hearBehind);
  const view = renderHook(() =>
    usePanelDismiss({
      onOpenChange: (open) => changes.push(open),
      open: true,
      panelRef: { current: panel },
      triggerRef: { current: trigger },
    })
  );
  try {
    field.focus();
    // user-event has no form for a key that ends a composition, so the event is made by hand.
    field.dispatchEvent(
      new KeyboardEvent("keydown", {
        bubbles: true,
        cancelable: true,
        isComposing: true,
        key: "Escape",
      })
    );
    expect(changes, "the Escape that ended a composition closed the panel").toEqual([]);
    expect(document.activeElement, "that Escape moved the focus").toBe(field);
    expect(behind.length, "that Escape stopped short of the view behind").toBe(1);

    field.dispatchEvent(
      new KeyboardEvent("keydown", {
        bubbles: true,
        cancelable: true,
        isComposing: false,
        key: "Escape",
        keyCode: 229,
      })
    );
    expect(changes, "Safari's Escape that ended a composition closed the panel").toEqual([]);
    expect(document.activeElement, "that Escape moved the focus").toBe(field);
    expect(behind.length, "that Escape stopped short of the view behind").toBe(2);

    field.dispatchEvent(
      new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "Escape" })
    );
    expect(changes).toEqual([false]);
    expect(document.activeElement).toBe(trigger);
    expect(behind.length, "the panel's Escape reached the view behind").toBe(2);
  } finally {
    view.unmount();
    window.removeEventListener("keydown", hearBehind);
    trigger.remove();
    panel.remove();
  }
});
