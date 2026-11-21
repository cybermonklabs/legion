import { expect, spyOn, test } from "bun:test";

import { copyText } from "./clipboard";

// INFRA-3135 round 16, the tester's, from the class search of review round 2's third finding.
// The legacy copy, which runs where the Clipboard API is missing or refuses, appends a text box
// to the end of the body and selects it, and Chromium's `select()` moves the keyboard focus into
// that box (measured in Chromium 156.0.8078.4: the focus went from the pressed button to the
// text box, and to the body when the box was removed). The box is in no region of the page's
// focus rescue - outside the main content and every dialog and panel - and no press was in it,
// so its removal is a loss the rescue answers in the document, away from the control the reader
// pressed. The copy leaves the focus where it was.
test("the legacy copy leaves the keyboard focus on the control that had it", async () => {
  const button = document.createElement("button");
  button.textContent = "Copy";
  document.body.append(button);
  button.focus();
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  Object.defineProperty(document, "execCommand", { configurable: true, value: () => true });
  // happy-dom's select() leaves the focus alone; Chromium's moves it into the box.
  const select = spyOn(HTMLTextAreaElement.prototype, "select").mockImplementation(function (
    this: HTMLTextAreaElement
  ) {
    this.focus();
  });
  try {
    expect(await copyText("dev:4.7")).toBe(true);
    expect(select).toHaveBeenCalledTimes(1);
    expect(button).toHaveFocus();
  } finally {
    select.mockRestore();
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    button.remove();
  }
});
