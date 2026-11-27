import { expect, spyOn, test } from "bun:test";

import { FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";
import { EDITOR_BOX_ATTRIBUTE, holdBlockEditorBox, measureBlockEditorRow } from "./node-caret";

test("a ready block editor caps its box without reading style after binding", () => {
  const root = document.documentElement;
  const row = document.createElement("div");
  const words = document.createElement("div");
  const box = document.createElement("div");
  words.style.lineHeight = "20px";
  box.setAttribute(EDITOR_BOX_ATTRIBUTE, "");
  row.append(words);
  document.body.append(row);
  const getStyle = window.getComputedStyle.bind(window);
  const style = spyOn(window, "getComputedStyle");
  const measureRow = spyOn(row, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, window.innerHeight - 204, 300, 100)
  );
  const measureWords = spyOn(words, "getBoundingClientRect").mockReturnValue(
    new DOMRect(20, window.innerHeight - 184, 280, 60)
  );
  const oldFoot = root.style.getPropertyValue(FOOT_HEIGHT_PROPERTY);
  const oldBand = root.style.getPropertyValue(IDENTITY_HEIGHT_PROPERTY);
  const frame = spyOn(window, "requestAnimationFrame").mockReturnValue(1);
  try {
    root.style.setProperty(FOOT_HEIGHT_PROPERTY, "32px");
    root.style.setProperty(IDENTITY_HEIGHT_PROPERTY, "64px");
    const measured = measureBlockEditorRow(row, words);
    const reads = style.mock.calls.length;
    row.replaceChildren(box);
    // Binding replaces the words with the kept editor and dirties layout. The cap still uses
    // the header and foot that were on screen when the row was measured.
    style.mockImplementation((target) => {
      if (target === root) throw new Error("the cap read document style after binding");
      return getStyle(target);
    });
    holdBlockEditorBox(row, measured);
    expect(style.mock.calls).toHaveLength(reads);
    expect(box.style.maxHeight).toBe(`${Math.floor((window.innerHeight - 136) / 2)}px`);
  } finally {
    root.style.setProperty(FOOT_HEIGHT_PROPERTY, oldFoot);
    root.style.setProperty(IDENTITY_HEIGHT_PROPERTY, oldBand);
    frame.mockRestore();
    measureWords.mockRestore();
    measureRow.mockRestore();
    style.mockRestore();
    row.remove();
  }
});
