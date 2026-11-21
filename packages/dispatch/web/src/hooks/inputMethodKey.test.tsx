import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which is what these rows read
import { fireEvent, render } from "@testing-library/react";

import { inputMethodKey } from "./inputMethodKey";

// An input method's key is one pressed in a composition, marked `isComposing`, or the one that
// ends it, which Safari sends after its `compositionend` unmarked with keyCode 229 (WebKit bug
// 165004). Any other key is the page's, a plain Enter among them, so the predicate can answer
// either way.
test("an input method's key is one marked composing or sent with keyCode 229, and no other", () => {
  const native = (init: KeyboardEventInit) =>
    inputMethodKey(new KeyboardEvent("keydown", { key: "Enter", ...init }));
  expect(native({ isComposing: true })).toBe(true);
  expect(native({ isComposing: false, keyCode: 229 })).toBe(true);
  expect(native({})).toBe(false);
  expect(native({ isComposing: false, keyCode: 13 })).toBe(false);
});

// A React `onKeyDown` hands the predicate React's event, whose composition mark is on the native
// event under it: the predicate reads the same three keys the same way through it.
test("React's key event is read through its native event, the same three ways", () => {
  const read: boolean[] = [];
  const view = render(
    <input aria-label="Words" onKeyDown={(event) => read.push(inputMethodKey(event))} />
  );
  try {
    const field = view.getByLabelText("Words");
    // user-event cannot mark a key as an input method's, so all three keys are fired the same way.
    fireEvent.keyDown(field, { isComposing: true, key: "Enter" });
    fireEvent.keyDown(field, { isComposing: false, key: "Enter", keyCode: 229 });
    fireEvent.keyDown(field, { key: "Enter" });
    expect(read).toEqual([true, true, false]);
  } finally {
    view.unmount();
  }
});
