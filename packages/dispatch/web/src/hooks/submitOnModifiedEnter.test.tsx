import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which the row below reads
import { fireEvent, render } from "@testing-library/react";

import { submitOnModifiedEnter } from "./submitOnModifiedEnter";

// A reader typing Chinese, Japanese or Korean confirms a word with Enter: that Enter is the input
// method's, marked `isComposing`, or sent unmarked with keyCode 229 after the composition's end, as
// Safari sends it (WebKit bug 165004), and with Ctrl or Cmd held it still submits nothing. A plain
// Ctrl+Enter after it submits the box's form.
test("the Ctrl+Enter that confirms an input method's word submits nothing, and a plain Ctrl+Enter submits", () => {
  let submitted = 0;
  const view = render(
    <form
      onSubmit={(event) => {
        event.preventDefault();
        submitted += 1;
      }}
    >
      <textarea aria-label="Words" onKeyDown={(event) => submitOnModifiedEnter(event)} />
    </form>
  );
  try {
    const field = view.getByLabelText("Words");
    // user-event cannot mark a key as an input method's, so the two such keys are fired.
    expect(fireEvent.keyDown(field, { ctrlKey: true, isComposing: true, key: "Enter" })).toBe(true);
    expect(
      fireEvent.keyDown(field, { ctrlKey: true, isComposing: false, key: "Enter", keyCode: 229 })
    ).toBe(true);
    expect(submitted).toBe(0);

    // The plain Ctrl+Enter goes the same way as the two above, so only the composition mark
    // differs.
    expect(fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" })).toBe(false);
    expect(submitted).toBe(1);
  } finally {
    view.unmount();
  }
});
