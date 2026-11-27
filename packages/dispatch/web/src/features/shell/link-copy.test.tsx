import { afterEach, beforeEach, expect, test } from "bun:test";
import { act, render, within } from "@testing-library/react";
import { KeymapProvider } from "./KeymapProvider";
import { appKeymap } from "./keymap";

let copied: string[] = [];
const originalClipboard = navigator.clipboard;

beforeEach(() => {
  copied = [];
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: async (text: string) => void copied.push(text) },
  });
});

afterEach(() => {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
});

/** Presses `y` on `target`, which the provider's window listener hears, and answers whether a
 *  binding took it. */
function pressY(target: Element): boolean {
  const event = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "y" });
  act(() => {
    target.dispatchEvent(event);
  });
  return event.defaultPrevented;
}

test("y on a focused link copies its address before every scope's own y, and nowhere else", async () => {
  const view = render(
    <KeymapProvider>
      <a href="https://example.com/a">elsewhere</a>
      <a data-dispatch-ref="dispatch://CORE-1" href="/issues/CORE-1">
        a reference
      </a>
      <button type="button">a button</button>
      <div contentEditable suppressContentEditableWarning>
        <a href="https://example.com/b">in an edited document</a>
      </div>
    </KeymapProvider>
  );
  // A dialog's own `y`, as the Inbox drawer's copies the item's reference: a dialog's scope is
  // the only one read while it is open, and a focused link's `y` still comes first.
  const dialogCopies: string[] = [];
  const unregister = appKeymap.register("dialog", [
    { id: "copy-ref", keys: "y", label: "Copy reference", run: () => void dialogCopies.push("y") },
  ]);
  const popScope = appKeymap.pushScope("dialog");
  const link = (name: string) => within(view.container).getByRole("link", { name });

  try {
    expect(appKeymap.describe().filter((entry) => entry.id === "copy-link-address")).toMatchObject([
      { keys: ["y"], label: "Copy the focused link's address", scope: "global" },
    ]);

    link("elsewhere").focus();
    expect(pressY(link("elsewhere"))).toBe(true);
    link("a reference").focus();
    expect(pressY(link("a reference"))).toBe(true);
    await act(async () => {});
    expect(copied).toEqual(["https://example.com/a", "dispatch://CORE-1"]);
    expect(dialogCopies).toEqual([]);

    // Off a link, the scope's own `y` runs.
    const button = within(view.container).getByRole("button", { name: "a button" });
    button.focus();
    expect(pressY(button)).toBe(true);
    expect(dialogCopies).toEqual(["y"]);

    // A link inside a document being edited takes `y` as the letter typed.
    link("in an edited document").focus();
    pressY(link("in an edited document"));
    await act(async () => {});
    expect(copied).toEqual(["https://example.com/a", "dispatch://CORE-1"]);
  } finally {
    popScope();
    unregister();
    view.unmount();
  }
  expect(appKeymap.describe().some((entry) => entry.id === "copy-link-address")).toBe(false);
});
