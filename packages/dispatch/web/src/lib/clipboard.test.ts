import { expect, spyOn, test } from "bun:test";
import userEvent from "@testing-library/user-event";

import { copyText } from "./clipboard";

// Where the clipboard refuses, `copyText` copies through a temporary text box that takes the
// page's selection and, in a browser, the focus, and then leaves the page, which drops the focus
// to the body. The clipboard answers after a wait. A reader who acts in that wait - a press in a
// field, and words typed there - has put the focus and the caret where they want them, and a late
// fallback would take both and leave the rest of their words nowhere. Here the clipboard's
// refusal is held until the reader has typed, as a slow clipboard holds it.
test("a reader who moves to a field before the clipboard refuses keeps the focus, the caret and the words typed there", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  const refused = Promise.withResolvers<void>();
  const commands: string[] = [];
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: () => refused.promise },
  });
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (command: string) => {
      commands.push(command);
      return true;
    },
  });
  // A browser's `select()` focuses the text box; happy-dom's only selects its text.
  const select = spyOn(HTMLTextAreaElement.prototype, "select").mockImplementation(function (
    this: HTMLTextAreaElement
  ) {
    this.focus();
  });
  const field = document.createElement("input");
  field.setAttribute("aria-label", "Elsewhere");
  document.body.append(field);
  try {
    const copying = copyText("CORE-1");
    await user.click(field);
    await user.keyboard("ab");
    refused.reject(new Error("denied"));
    const copied = await copying;
    expect(document.activeElement, "the copy's fallback took the focus from the field").toBe(field);
    expect(field.value).toBe("ab");
    expect([field.selectionStart, field.selectionEnd]).toEqual([2, 2]);
    // Nothing was copied, and the copy says so, so its caller offers the words to copy by hand.
    expect(commands).toEqual([]);
    expect(copied).toBe(false);
  } finally {
    field.remove();
    select.mockRestore();
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
  }
});
